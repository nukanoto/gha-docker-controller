// Check validates the configured GitHub and Docker dependencies without
// creating runners or containers. Image pulls are the only allowed mutation.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nukanoto/arc-docker/internal/config"
	"github.com/nukanoto/arc-docker/internal/docker"
	"github.com/nukanoto/arc-docker/internal/model"
	"github.com/nukanoto/arc-docker/internal/scaleset"
)

func Check(cfg *config.Config, version, commit string, logger *slog.Logger) error {
	if cfg == nil {
		return errors.New("check: nil config")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	gh, err := scaleset.New(cfg, version, commit)
	if err != nil {
		return fmt.Errorf("check: github credential: %w", err)
	}
	result, err := gh.CheckScaleSet(signalCtx, cfg.ScaleSet.RunnerGroup, cfg.ScaleSet.Name)
	if err != nil {
		return fmt.Errorf("check: github scale set: %w", err)
	}
	logger.Info("github scale set access verified", "runner_group", cfg.ScaleSet.RunnerGroup)
	if result.Warning != "" {
		logger.Warn("github scale set warning", "warning", result.Warning)
	}

	dc, err := docker.New(cfg.Docker.Host, dockerHTTPTimeout)
	if err != nil {
		return fmt.Errorf("check: %w", err)
	}
	defer dc.Close()
	negotiatedAPI, engineVersion, err := dc.ValidateVersions(signalCtx)
	if err != nil {
		return fmt.Errorf("check: %w", err)
	}
	logger.Info("docker engine verified", "api_version", negotiatedAPI, "engine_version", engineVersion)
	if runtime := configuredRuntime(cfg); runtime != "" {
		rt, err := dc.CheckRuntime(signalCtx, runtime)
		if err != nil {
			return fmt.Errorf("check: %w", err)
		}
		logger.Info("docker runtime registered", "runtime", runtime, "is_default", rt.IsDefault)
	}

	if err := dc.EnsureImage(signalCtx, cfg.Runner.Image, cfg.Docker.PullPolicy); err != nil {
		return fmt.Errorf("check: %w", err)
	}
	logger.Info("runner image verified", "image", cfg.Runner.Image, "policy", cfg.Docker.PullPolicy)

	// BuildManagedSpec is pure; the dummy JIT value never reaches Docker.
	if _, err := docker.BuildManagedSpec(checkSpecInput(cfg, version)); err != nil {
		return fmt.Errorf("check: runner spec: %w", err)
	}
	logger.Info("runner spec verified")
	return nil
}

func configuredRuntime(cfg *config.Config) string {
	if cfg.Runner.HostConfig == nil {
		return ""
	}
	return cfg.Runner.HostConfig.Runtime
}

// checkSpecInput supplies valid dummy identity values for pure spec validation.
func checkSpecInput(cfg *config.Config, version string) docker.ManagedSpecInput {
	const (
		dummyJIT      = "check"
		dummyInstance = "check"
		dummySuffix   = "000000000000"
	)
	return docker.ManagedSpecInput{
		Config: cfg,
		Identity: model.RunnerIdentity{
			ScaleSetID: 1,
			RunnerID:   1,
			RunnerName: model.RunnerName(cfg.ScaleSet.Name, dummySuffix),
		},
		JITConfig:          dummyJIT,
		ControllerInstance: dummyInstance,
		CreatedAt:          time.Now().UTC(),
		ContainerName:      model.ContainerName(cfg.ScaleSet.Name, 1, dummySuffix),
		UserAgentVersion:   version,
	}
}
