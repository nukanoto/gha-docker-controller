// check.go implements the read-only check.
// check never creates or deletes containers, runners, Scale Sets, or
// networks; only Docker image store changes from image pulls are allowed. It
// verifies authentication for querying an existing Scale Set, but it cannot
// prove create permission when the Scale Set is absent. It checks Docker
// ping/version, an explicitly configured runtime, and the image pull policy.
// All errors contain no secrets.
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

	"github.com/nukanoto/gha-docker-controller/internal/config"
	"github.com/nukanoto/gha-docker-controller/internal/docker"
	"github.com/nukanoto/gha-docker-controller/internal/model"
	"github.com/nukanoto/gha-docker-controller/internal/scaleset"
)

// Check verifies read-only that the connection targets of a validated config
// satisfy the contracts serve needs. cfg is the runtime config produced by
// config.Load. version and commit are build info provided by cli/buildinfo.
// A nil logger discards logs. On failure it returns a nonzero error that the
// caller (CLI) converts to an exit code.
//
// The checks run in this order:
//
//  1. Read-only query of credential/auth and the runner group / Scale Set.
//     If the Scale Set is missing, a warning is logged that create permission
//     cannot be proven read-only; that alone is not a failure.
//  2. Docker ping/version (API >= 1.42, Engine >= 28.0) and an explicitly
//     configured runtime. An omitted runtime is left to Docker.
//  3. Apply the image pull policy (pull is the allowed mutation).
//  4. Validate the controller-owned runner spec. No container is created.
//
// Containers, runners, Scale Sets, and networks are never created or
// deleted. Image pulls change the Docker image store, so the README and CLI
// help state that check is not fully host read-only.
func Check(cfg *config.Config, version, commit string, logger *slog.Logger) error {
	if cfg == nil {
		return errors.New("check: nil config")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	// check may also have long pulls, so allow interruption via signals.
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 1. Read-only query of credential/auth and the runner group / Scale
	// Set.
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
		// If the Scale Set is missing, warn that create permission cannot be
		// proven read-only; that alone is not a failure.
		logger.Warn("github scale set warning", "warning", result.Warning)
	}

	// 2. Docker client and ping/version/runtime verification.
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

	// Apply the image pull policy.
	if err := dc.EnsureImage(signalCtx, cfg.Runner.Image, cfg.Docker.PullPolicy); err != nil {
		return fmt.Errorf("check: %w", err)
	}
	logger.Info("runner image verified", "image", cfg.Runner.Image, "policy", cfg.Docker.PullPolicy)

	// Validate the controller-owned spec. BuildManagedSpec is a read-only
	// pure builder; no container is created. A dummy JIT config is used and
	// never logged or included in errors.
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

// checkSpecInput builds the dummy input used for the read-only
// BuildManagedSpec validation. The JIT config is an opaque secret, so a
// dummy value is passed and neither decoded nor logged. The runner ID and
// Scale Set ID are dummies satisfying "positive value"; no GitHub
// communication happens. The container name and runner name follow the
// managed container naming/identity conventions.
func checkSpecInput(cfg *config.Config, version string) docker.ManagedSpecInput {
	// Dummy values are fixed and for validation only; no real runner or
	// container is created. The suffix is 12 lowercase hex digits.
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
