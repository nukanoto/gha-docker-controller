// check.go implements the read-only check.
// check never creates or deletes containers, runners, Scale Sets, or
// networks; only Docker image store changes from image pulls are allowed. It
// verifies authentication for querying an existing Scale Set, but it cannot
// prove create permission when the Scale Set is absent. It also checks Docker
// ping/version/runtime/network, the image pull policy/OCI contract, and
// resource/profile validation. The
// nested-docker runtimeArgs cannot be verified through the official Docker
// API, so a warning and the operator's manual verification are the
// responsibility boundary. All errors contain no secrets.
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
//  2. Docker ping/version (API >= 1.42, Engine >= 28.0) and runtime
//     registration. For nested-docker the runtime name is defensively
//     re-checked to be runsc, and runtimeArgs always produce a warning.
//  3. Inspect the existing network (never create or delete).
//  4. Apply the image pull policy (pull is the allowed mutation) and the
//     profile's OCI label contract.
//  5. Validate the resource/profile spec (including seccomp file, tmpfs,
//     DNS, and ulimit). No container is created.
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
	rt, err := dc.CheckRuntime(signalCtx, cfg.Docker.Runtime)
	if err != nil {
		return fmt.Errorf("check: %w", err)
	}
	logger.Info("docker runtime registered", "runtime", cfg.Docker.Runtime, "is_default", rt.IsDefault)
	if cfg.Runner.Profile == config.ProfileNestedDocker {
		// Defensively re-check that the nested-docker runtime name is runsc
		// (also guaranteed by static validation).
		if cfg.Docker.Runtime != config.DefaultRuntime {
			return fmt.Errorf("check: nested-docker profile requires docker.runtime %q, got %q",
				config.DefaultRuntime, cfg.Docker.Runtime)
		}
		// runtimeArgs (--net-raw, --allow-packet-socket-write) cannot be
		// introspected through the official Docker API, so always warn. The
		// operator checks the host side manually in daemon.json (see README).
		logger.Warn("nested-docker runtime args (--net-raw --allow-packet-socket-write) cannot be verified via the Docker API; verify them manually in the host daemon.json")
	} else if len(rt.Args) > 0 {
		// Even for standard, runtimeArgs introspection is unreliable, so
		// observed args only produce a warning.
		logger.Warn("runtime args are observed but not verified", "runtime", cfg.Docker.Runtime)
	}

	// 3. Inspect the existing network. Never create or delete.
	if _, err := dc.InspectNetwork(signalCtx, cfg.Runner.Network); err != nil {
		return fmt.Errorf("check: docker network %q: %w", cfg.Runner.Network, err)
	}
	logger.Info("docker network verified", "network", cfg.Runner.Network)

	// 4. Apply the image pull policy and check the OCI label contract.
	if err := dc.EnsureImage(signalCtx, cfg.Runner.Image, cfg.Docker.PullPolicy); err != nil {
		return fmt.Errorf("check: %w", err)
	}
	logger.Info("runner image verified", "image", cfg.Runner.Image, "policy", cfg.Docker.PullPolicy)
	if err := dc.ValidateImageContract(signalCtx, cfg.Runner.Image, cfg.Runner.Profile); err != nil {
		return fmt.Errorf("check: %w", err)
	}

	// 5. Validate the resource/profile spec. BuildManagedSpec is a read-only
	// pure builder; no container is created. A dummy JIT config is used and
	// never logged or included in errors.
	if _, err := docker.BuildManagedSpec(checkSpecInput(cfg, version)); err != nil {
		return fmt.Errorf("check: runner spec: %w", err)
	}
	logger.Info("runner spec verified", "profile", cfg.Runner.Profile)
	return nil
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
