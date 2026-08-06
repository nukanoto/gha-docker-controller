// shutdown.go implements shutdown. Order is: stop new work (run cancel),
// listener join, scaler shutdown, health shutdown, session close, Docker
// close. Each phase uses an independent timeout context created
// fresh from context.Background(); expired contexts are never reused in later
// phases. If the listener join or the scaler watch join times out, shutdown
// returns immediately without closing later components and leaves it to
// process exit (structurally preventing races where components in use by
// running handlers get closed). After a scaler cleanup failure the watches
// are joined, so later phases still run and the error is joined with the
// main error by Serve to make the exit code nonzero (systemd
// Restart=on-failure triggers even for SIGTERM cleanup failures). The Scale
// Set is never deleted. Recovered protected runners are kept by the scaler.
// Anything left after a phase timeout is recovered by the next startup's
// Recover.
package app

import (
	"context"
	"errors"
	"time"

	"github.com/nukanoto/gha-docker-controller/internal/config"
	"github.com/nukanoto/gha-docker-controller/internal/controller"
)

// Timeout constants for later phases. With defaults the maximum stop time is
// listener join (max(provisioning 5 min, cleanup 2 min) = 5 min) + scaler
// cleanup (shutdown grace 2 min) + 5s (health) + 30s (session) = 455s, which
// the deploy systemd unit covers with TimeoutStopSec=480.
const (
	// healthShutdownTimeout is the independent timeout for the health
	// server's graceful shutdown.
	healthShutdownTimeout = 5 * time.Second
	// sessionCloseTimeout is the independent timeout for closing the message
	// session.
	sessionCloseTimeout = 30 * time.Second
	// listenerJoinTimeoutWarning is the fixed warning logged on a listener
	// join timeout. It contains no leftover goroutine details or dynamic
	// errors.
	listenerJoinTimeoutWarning = "listener join grace expired; listener still running; aborting shutdown; process exit will release resources; leftover containers will be recovered at next startup"
	// scalerJoinTimeoutWarning is the fixed warning logged when the scaler
	// watch join times out. It contains no dynamic information.
	scalerJoinTimeoutWarning = "scaler join grace expired; runner watches still running; aborting shutdown; process exit will release resources; leftover containers will be recovered at next startup"
)

// errListenerJoinTimeout is the fixed error representing a listener join
// deadline exceeded. Serve joins it with the main error to make the exit
// code nonzero and trigger systemd Restart=on-failure.
var errListenerJoinTimeout = errors.New(listenerJoinTimeoutWarning)

// newShutdownPhaseContext creates the timeout context each phase uses, fresh
// from context.Background(). It takes no parent, so an expired earlier-phase
// context can never be reused by a later phase.
func newShutdownPhaseContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}

// shutdown stops the daemon in the fixed phase order. It is also called from
// error paths during startup, so unbuilt components are skipped when nil.
// The return value is the listener join timeout and the scaler shutdown
// failure (watch join timeout, cleanup failure) errors; Serve joins them
// with the main error to make the process exit code nonzero.
// Health/session/Docker close failures do not block the daemon stop, so they
// are logged as warnings and not included in the error.
func (a *app) shutdown() error {
	a.logger.Info("shutting down", "busy_policy", a.cfg.Shutdown.BusyPolicy)
	grace := time.Duration(a.cfg.Shutdown.Grace)
	if grace <= 0 {
		// Config validation guarantees a positive value, but defensively
		// keep at least 1 second.
		grace = time.Second
	}

	// phase 0: cancel the run context. The DockerScaler stops new
	// provisioning and the official listener ends polling. Running handlers
	// are not interrupted (context.WithoutCancel); the provisioning timeout
	// bounds their execution.
	// Right after cancel, session/listener are flipped to false so readyz
	// returns 503. The health server itself keeps running until phase 3.
	a.cancel()
	if a.store != nil {
		a.store.SetSessionRunning(false)
		a.store.SetListenerRunning(false)
	}

	// phase 1: listener join. Wait for listener.Run with an independent
	// context bounded by the larger handler timeout: provisioning or cleanup.
	// On timeout, warn and return immediately
	// without running the remaining phases, leaving it to process exit. Not
	// closing the scaler/session/Docker in use by running handlers
	// structurally prevents close races. Leftovers are recovered by the next
	// startup's Recover.
	joinCtx, joinCancel := newShutdownPhaseContext(a.listenerJoinTimeout())
	joined := a.waitListener(joinCtx)
	joinCancel()
	if !joined {
		a.logger.Warn(listenerJoinTimeoutWarning)
		return errListenerJoinTimeout
	}

	// phase 2: scaler shutdown. Cancel and join the watches, clean up this
	// process's idle runners, and also clean up busy ones only under the
	// stop policy. Protected runners are always kept. If the join times out
	// (ErrShutdownJoinTimeout), return immediately without running later
	// phases; on cleanup failure the watches are joined, so later phases run
	// and then the error is returned.
	var scalerErr error
	if a.scaler != nil {
		sctx, scancel := newShutdownPhaseContext(grace)
		scalerErr = a.scaler.Shutdown(sctx)
		scancel()
		if scalerErr != nil {
			if errors.Is(scalerErr, controller.ErrShutdownJoinTimeout) {
				a.logger.Warn(scalerJoinTimeoutWarning)
				return scalerErr
			}
			a.logger.Warn("scaler shutdown cleanup failed", "error", scalerErr)
			// A cleanup failure does not block later phases. The error is
			// passed to the caller by the return below.
		}
	}

	// phase 3: health server graceful shutdown (fresh 5 seconds).
	if a.health != nil {
		hctx, hcancel := newShutdownPhaseContext(healthShutdownTimeout)
		err := a.health.Shutdown(hctx)
		hcancel()
		if err != nil {
			a.logger.Warn("health shutdown failed", "error", err)
		}
	}

	// phase 4: close the message session (fresh 30 seconds). Runs only after
	// listener.Run has finished.
	if a.session != nil {
		sctx, scancel := newShutdownPhaseContext(sessionCloseTimeout)
		err := a.session.Close(sctx)
		scancel()
		if err != nil {
			a.logger.Warn("message session close failed", "error", err)
		}
	}

	// phase 5: close the Docker connection. Synchronous work that needs no
	// context.
	if a.docker != nil {
		if err := a.docker.Close(); err != nil {
			a.logger.Warn("docker close failed", "error", err)
		}
	}
	a.logger.Info("shutdown complete")
	return scalerErr
}

// listenerJoinTimeout returns the wait bound for the listener join. The
// official listener calls handlers with context.WithoutCancel, so the max of
// the provisioning timeout and the cleanup timeout bounds running handlers;
// the join waits with the same bound. The cleanup timeout comes from the
// shutdown grace, like the scaler.
func (a *app) listenerJoinTimeout() time.Duration {
	provisioning := time.Duration(a.cfg.Runner.ProvisioningTimeout)
	if provisioning <= 0 {
		provisioning = time.Duration(config.DefaultProvisioningTimeout)
	}
	cleanup := time.Duration(a.cfg.Shutdown.Grace)
	if cleanup <= 0 {
		cleanup = time.Duration(config.DefaultShutdownGrace)
	}
	return max(provisioning, cleanup)
}

// waitListener waits until the listener.Run goroutine (a.wg) finishes or
// ctx's deadline. On deadline it returns false without waiting further.
// Internal goroutines live only until wg completes; the listener always
// exits via the handler timeout (no leak). The return value is "did the
// listener finish before the deadline".
func (a *app) waitListener(ctx context.Context) bool {
	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}
