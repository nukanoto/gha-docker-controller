// Package app provides orchestration for serve and check. It takes a loaded
// config.Config and runs the host lock, startup order, shutdown order, and
// the read-only check. serve uses the official actions/scaleset listener
// directly; message polling, ack, and desired computation are delegated to
// the official listener and a thin DockerScaler. This package is only
// responsible for component construction order and lifecycle. All errors
// contain no secrets (per the callees' contract).
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	listenerapi "github.com/actions/scaleset/listener"

	"github.com/nukanoto/gha-docker-controller/internal/config"
	"github.com/nukanoto/gha-docker-controller/internal/controller"
	"github.com/nukanoto/gha-docker-controller/internal/docker"
	"github.com/nukanoto/gha-docker-controller/internal/health"
	"github.com/nukanoto/gha-docker-controller/internal/scaleset"
)

// dockerHTTPTimeout is the safety-net timeout for the HTTP client passed to
// docker.New. Moby client's WithTimeout also applies to the whole response
// body (until an image pull finishes), so a value covering the longest pull
// is fixed (docker.New's contract). 30 minutes covers multi-GB runner images
// even on slow links; shorter waits are controlled via ctx deadlines.
const dockerHTTPTimeout = 30 * time.Minute

// app holds the serve components and the state shutdown needs. shutdown is
// also called from error paths during startup, so components may be nil and
// shutdown guards against them.
type app struct {
	cfg    *config.Config
	logger *slog.Logger

	lock    *lockFile
	docker  *docker.Client
	session *scaleset.ListenerClient
	scaler  *controller.DockerScaler
	health  *health.Server
	// store is the source of truth for readiness. Right after shutdown
	// starts, session/listener are flipped to false so readyz stays 503 until
	// the health server stops.
	store *health.Store

	// runCtx is the context shared by the listener and the scaler; it is
	// cancelled in the first shutdown phase.
	runCtx context.Context
	// cancel cancels runCtx.
	cancel context.CancelFunc
	// wg waits for the listener.Run goroutine to finish.
	wg sync.WaitGroup
	// errCh receives the listener.Run exit error; it is a buffered-1
	// channel. The only writer is the listener goroutine, and the buffer
	// absorbs every send, so a missing reader never blocks it (no goroutine
	// leak).
	errCh chan error
}

// Serve starts the daemon with a validated config and runs until a signal or
// a fatal error. cfg is the runtime config produced by config.Load and is
// not modified afterwards. version and commit are build info provided by
// cli/buildinfo. A nil logger discards logs.
//
// Startup order is fixed: host lock, Docker host verification, Scale Set
// get-or-create, DockerScaler construction and Recover, official message
// session, official listener, health server, listener goroutine. If the host
// lock cannot be acquired, another process is running on the same host and a
// fatal error is returned. shutdown always runs in the fixed order and never
// deletes the Scale Set. The returned error joins the main error with the
// shutdown join timeout / cleanup failure and contains no secrets.
func Serve(cfg *config.Config, version, commit string, logger *slog.Logger) error {
	if cfg == nil {
		return errors.New("serve: nil config")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	// signalCtx is cancelled by SIGINT/SIGTERM. Signals are the shutdown
	// trigger and also cancel I/O during startup.
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runCtx, cancel := context.WithCancel(signalCtx)

	a := &app{
		cfg:    cfg,
		logger: logger,
		runCtx: runCtx,
		cancel: cancel,
		errCh:  make(chan error, 1),
	}

	var err error
	if err = a.startup(signalCtx, version, commit); err == nil {
		err = a.wait(signalCtx)
	}
	// shutdown always runs even on error paths during startup, reliably
	// releasing the lock and the Docker connection. Even on a normal SIGTERM
	// shutdown, listener/scaler join timeouts and shutdown cleanup failures
	// are returned as errors so the process exits nonzero and triggers
	// systemd Restart=on-failure.
	return errors.Join(err, a.shutdown())
}

// startup builds the components in the fixed startup order and starts the
// health server and the listener goroutine. Restart adoption (Recover) runs
// before the listener/session start. Errors from each step contain no
// secrets.
func (a *app) startup(signalCtx context.Context, version, commit string) error {
	cfg := a.cfg

	// 1. Host lock, held on the fixed path for the whole process lifetime.
	lock, err := acquireLock()
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	a.lock = lock
	a.logger.Info("host lock acquired", "path", lockPath)

	// 2. Docker client and host verification.
	dc, err := docker.New(cfg.Docker.Host, dockerHTTPTimeout)
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	a.docker = dc
	// Verify ping/version (API >= 1.42, Engine >= 28.0).
	if _, _, err := dc.ValidateVersions(signalCtx); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	a.logger.Info("docker engine verified", "host", dc.Host())
	// Check the runtime is registered before creating containers; an
	// unregistered runtime is fatal. No trial container is created.
	rt, err := dc.CheckRuntime(signalCtx, cfg.Docker.Runtime)
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	a.logger.Info("docker runtime verified", "runtime", cfg.Docker.Runtime, "is_default", rt.IsDefault)
	// Only inspect the existing network object; never create one.
	if _, err := dc.InspectNetwork(signalCtx, cfg.Runner.Network); err != nil {
		return fmt.Errorf("serve: docker network %q: %w", cfg.Runner.Network, err)
	}
	a.logger.Info("docker network verified", "network", cfg.Runner.Network)
	// Prepare the image per the pull policy and verify the profile's OCI
	// label contract.
	if err := dc.EnsureImage(signalCtx, cfg.Runner.Image, cfg.Docker.PullPolicy); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	if err := dc.ValidateImageContract(signalCtx, cfg.Runner.Image, cfg.Runner.Profile); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	a.logger.Info("runner image ready", "image", cfg.Runner.Image, "policy", cfg.Docker.PullPolicy)

	// 3. GitHub client and Scale Set get-or-create. Never delete it at
	// shutdown.
	gh, err := scaleset.New(cfg, version, commit)
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	ss, err := gh.EnsureScaleSet(signalCtx, cfg.ScaleSet.RunnerGroup, cfg.ScaleSet.Name)
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	a.logger.Info("scale set ready", "scale_set_id", ss.ID, "runner_group", cfg.ScaleSet.RunnerGroup)

	// 4. DockerScaler construction and restart adoption. Recover runs before
	// the listener and protects or cleans up existing managed containers.
	scaler, err := controller.NewDockerScaler(a.runCtx, dc, gh, cfg, ss.ID, version, a.logger)
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	// Store the scaler before Recover so shutdown can still join watches and
	// close Docker even if Recover fails midway.
	a.scaler = scaler
	if err := scaler.Recover(signalCtx); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	a.logger.Info("runner recovery complete")

	// 5. Message session for the official listener. Pass the github.owner of
	// the organization/repository as owner.
	session, err := gh.NewListenerClient(signalCtx, ss.ID, cfg.GitHub.Owner)
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	a.session = session

	// 6. Official listener. MaxRunners is used for the poll maxCapacity and
	// the listener capacity.
	listener, err := listenerapi.New(session, listenerapi.Config{
		ScaleSetID: ss.ID,
		MaxRunners: cfg.ScaleSet.MaxRunners,
		Logger:     a.logger.WithGroup("listener"),
	})
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}

	// 7. Start the health server. Readiness requires only the session and
	// the listener to be running. The session is already created, so it is
	// set true here; the listener state is set right before/after Run inside
	// the goroutine. Both are never true at startup. A bind failure returns
	// a synchronous error without starting the goroutine.
	store := health.NewStore()
	hs, err := health.New(cfg.Health.Listen, store, a.logger)
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	if err := hs.Start(); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	a.health = hs
	// Keep the store so shutdown can drop readyz to 503 immediately.
	a.store = store
	store.SetSessionRunning(true)
	a.logger.Info("health server listening", "addr", hs.Addr().String())

	// 8. Start the listener goroutine. Its exit error goes to errCh, received
	// by wait in a single select. context.Canceled is a normal shutdown
	// exit. The readiness listener state is set true right before Run and
	// false right after.
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.store.SetListenerRunning(true)
		err := listener.Run(a.runCtx, scaler)
		a.store.SetListenerRunning(false)
		a.errCh <- err
	}()
	a.logger.Info("daemon started", "scale_set_id", ss.ID)
	return nil
}

// wait watches signal, listener, scaler, and health termination in a single
// select. The listener's context.Canceled is a normal shutdown exit and is
// ignored. Any other error is fatal.
func (a *app) wait(signalCtx context.Context) error {
	for {
		select {
		case <-signalCtx.Done():
			// Signal: proceed to graceful shutdown.
			return nil
		case err := <-a.errCh:
			if err == nil || errors.Is(err, context.Canceled) {
				continue
			}
			return err
		case err := <-a.scaler.ErrCh():
			if err != nil {
				return err
			}
		case err := <-a.health.ErrCh():
			if err != nil {
				return err
			}
		}
	}
}
