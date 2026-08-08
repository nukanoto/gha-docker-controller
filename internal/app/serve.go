// Package app coordinates daemon startup, serving, checking, and shutdown.
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

// dockerHTTPTimeout must cover a complete runner-image pull.
const dockerHTTPTimeout = 30 * time.Minute

// app holds serving components and shutdown state.
type app struct {
	cfg    *config.Config
	logger *slog.Logger

	docker  *docker.Client
	session *scaleset.ListenerClient
	scaler  *controller.DockerScaler
	health  *health.Server
	// store is flipped to not-ready before dependencies are closed.
	store *health.Store

	runCtx context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// The buffer lets the listener finish even when shutdown stops reading.
	errCh chan error
}

// Serve starts the daemon and runs until a signal or fatal component error.
// Shutdown runs even when startup fails.
func Serve(cfg *config.Config, version, commit string, logger *slog.Logger) error {
	if cfg == nil {
		return errors.New("serve: nil config")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
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
	return errors.Join(err, a.shutdown())
}

// startup builds components in dependency order and starts the listener.
func (a *app) startup(signalCtx context.Context, version, commit string) error {
	cfg := a.cfg

	dc, err := docker.New(cfg.Docker.Host, dockerHTTPTimeout)
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	a.docker = dc
	if _, _, err := dc.ValidateVersions(signalCtx); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	a.logger.Info("docker engine verified", "host", dc.Host())
	if runtime := configuredRuntime(cfg); runtime != "" {
		rt, err := dc.CheckRuntime(signalCtx, runtime)
		if err != nil {
			return fmt.Errorf("serve: %w", err)
		}
		a.logger.Info("docker runtime verified", "runtime", runtime, "is_default", rt.IsDefault)
	}
	if err := dc.EnsureImage(signalCtx, cfg.Runner.Image, cfg.Docker.PullPolicy); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	a.logger.Info("runner image ready", "image", cfg.Runner.Image, "policy", cfg.Docker.PullPolicy)

	gh, err := scaleset.New(cfg, version, commit)
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	ss, err := gh.EnsureScaleSet(signalCtx, cfg.ScaleSet.RunnerGroup, cfg.ScaleSet.Name)
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	a.logger.Info("scale set ready", "scale_set_id", ss.ID, "runner_group", cfg.ScaleSet.RunnerGroup)

	scaler, err := controller.NewDockerScaler(a.runCtx, dc, gh, cfg, ss.ID, version, a.logger)
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	// Register before recovery so startup errors still have shutdown ownership.
	a.scaler = scaler
	if err := scaler.Recover(signalCtx); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	a.logger.Info("runner recovery complete")

	session, err := gh.NewListenerClient(signalCtx, ss.ID, cfg.GitHub.Owner)
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	a.session = session

	listener, err := listenerapi.New(session, listenerapi.Config{
		ScaleSetID: ss.ID,
		MaxRunners: cfg.ScaleSet.MaxRunners,
		Logger:     a.logger.WithGroup("listener"),
	})
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}

	store := health.NewStore()
	hs, err := health.New(cfg.Health.Listen, store, a.logger)
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	if err := hs.Start(); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	a.health = hs
	a.store = store
	store.SetSessionRunning(true)
	a.logger.Info("health server listening", "addr", hs.Addr().String())

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

// wait returns on a signal or component failure.
func (a *app) wait(signalCtx context.Context) error {
	for {
		select {
		case <-signalCtx.Done():
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
