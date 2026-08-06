package docker

import (
	"fmt"
	"strings"

	"github.com/nukanoto/gha-docker-controller/internal/config"
	"github.com/nukanoto/gha-docker-controller/internal/model"
)

// validateSpecInput validates the BuildManagedSpec input (config + identity
// + JIT). It does not assume the config passed the static validation in
// internal/config; this builder re-checks the security contract itself.
func validateSpecInput(cfg *config.Config, input ManagedSpecInput) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	if input.JITConfig == "" {
		return fmt.Errorf("JIT config is empty")
	}
	if input.ControllerInstance == "" {
		return fmt.Errorf("controller instance is empty")
	}
	if input.CreatedAt.IsZero() {
		return fmt.Errorf("created at is zero")
	}
	if input.ContainerName == "" {
		return fmt.Errorf("container name is empty")
	}
	if input.UserAgentVersion == "" {
		return fmt.Errorf("user agent version is empty")
	}
	if input.Identity.ScaleSetID <= 0 || input.Identity.RunnerID <= 0 {
		return fmt.Errorf("identity must have positive scale set id and runner id, got scale-set-id=%d runner-id=%d", input.Identity.ScaleSetID, input.Identity.RunnerID)
	}
	if !model.ValidRunnerName(input.Identity.RunnerName) {
		return fmt.Errorf("runner name %q is not a valid canonical runner name", input.Identity.RunnerName)
	}

	// Profile and runtime consistency. dind-runner is fixed to runsc;
	// standard allows a registered runtime name matching [A-Za-z0-9_.-]+.
	switch cfg.Runner.Profile {
	case config.ProfileStandard:
		if !validRuntimeName(cfg.Docker.Runtime) {
			return fmt.Errorf("docker.runtime %q is not a valid runtime name for standard profile", cfg.Docker.Runtime)
		}
	case config.ProfileDindRunner:
		if cfg.Docker.Runtime != dindRuntime {
			return fmt.Errorf("docker.runtime %q is not allowed for dind-runner profile (requires %q)", cfg.Docker.Runtime, dindRuntime)
		}
	default:
		return fmt.Errorf("runner.profile %q is unknown", cfg.Runner.Profile)
	}

	// Only an existing non-host network is allowed.
	if err := validateSpecNetwork(cfg.Runner.Network); err != nil {
		return err
	}

	// Resources are explicitly required and positive.
	if cfg.Runner.CPU <= 0 {
		return fmt.Errorf("runner.cpu must be positive (missing resource)")
	}
	if cfg.Runner.Memory <= 0 {
		return fmt.Errorf("runner.memory must be positive (missing resource)")
	}
	if cfg.Runner.MemorySwap < cfg.Runner.Memory {
		return fmt.Errorf("runner.memorySwap must be >= runner.memory")
	}
	if cfg.Runner.PidsLimit <= 0 {
		return fmt.Errorf("runner.pidsLimit must be positive (missing resource)")
	}
	if cfg.DindRunner.StorageSize <= 0 {
		return fmt.Errorf("dindRunner.storageSize must be positive")
	}

	return validateSpecInputSecurity(cfg)
}

// validateSpecNetwork validates the network contract (existing non-host
// network). host, none and container:<id> share the host namespace, so they
// are rejected.
func validateSpecNetwork(network string) error {
	switch network {
	case "", "host", "none", "container":
		return fmt.Errorf("network mode %q is not allowed (existing non-host network required)", network)
	}
	if strings.HasPrefix(network, "container:") {
		return fmt.Errorf("network mode %q is not allowed", network)
	}
	return nil
}

// validRuntimeName validates the allowed runtime name character set
// [A-Za-z0-9_.-]+.
func validRuntimeName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}
