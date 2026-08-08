// Shutdown runs the daemon phases in order. Each phase gets a fresh timeout
// context so an expired phase cannot cancel later cleanup.
package app

import (
	"context"
	"errors"
	"time"

	"github.com/nukanoto/arc-docker/internal/config"
	"github.com/nukanoto/arc-docker/internal/controller"
)

const (
	healthShutdownTimeout      = 5 * time.Second
	sessionCloseTimeout        = 30 * time.Second
	listenerJoinTimeoutWarning = "listener join grace expired; listener still running; aborting shutdown; process exit will release resources; leftover containers will be recovered at next startup"
	scalerJoinTimeoutWarning   = "scaler join grace expired; runner watches still running; aborting shutdown; process exit will release resources; leftover containers will be recovered at next startup"
)

// errListenerJoinTimeout makes a listener timeout fail the process restart.
var errListenerJoinTimeout = errors.New(listenerJoinTimeoutWarning)

// newShutdownPhaseContext creates an independent phase timeout.
func newShutdownPhaseContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}

// shutdown stops the daemon in the fixed phase order. It is safe to call
// during partial startup.
func (a *app) shutdown() error {
	a.logger.Info("shutting down", "busy_policy", a.cfg.Shutdown.BusyPolicy)
	grace := time.Duration(a.cfg.Shutdown.Grace)
	if grace <= 0 {
		// Keep a defensive lower bound for callers outside config.Load.
		grace = time.Second
	}

	// Stop new work and make readiness fail immediately.
	a.cancel()
	if a.store != nil {
		a.store.SetSessionRunning(false)
		a.store.SetListenerRunning(false)
	}

	// Do not close dependencies while a handler is still running.
	joinCtx, joinCancel := newShutdownPhaseContext(a.listenerJoinTimeout())
	joined := a.waitListener(joinCtx)
	joinCancel()
	if !joined {
		a.logger.Warn(listenerJoinTimeoutWarning)
		return errListenerJoinTimeout
	}

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
		}
	}

	if a.health != nil {
		hctx, hcancel := newShutdownPhaseContext(healthShutdownTimeout)
		err := a.health.Shutdown(hctx)
		hcancel()
		if err != nil {
			a.logger.Warn("health shutdown failed", "error", err)
		}
	}

	if a.session != nil {
		sctx, scancel := newShutdownPhaseContext(sessionCloseTimeout)
		err := a.session.Close(sctx)
		scancel()
		if err != nil {
			a.logger.Warn("message session close failed", "error", err)
		}
	}

	if a.docker != nil {
		if err := a.docker.Close(); err != nil {
			a.logger.Warn("docker close failed", "error", err)
		}
	}
	a.logger.Info("shutdown complete")
	return scalerErr
}

// listenerJoinTimeout bounds handlers that outlive listener cancellation.
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

// waitListener waits for the listener goroutine until ctx expires.
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
