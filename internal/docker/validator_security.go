package docker

import (
	"fmt"
	"slices"

	"github.com/nukanoto/gha-docker-controller/internal/config"
)

// validateSpecInputSecurity validates the security contract of the builder
// input.
func validateSpecInputSecurity(cfg *config.Config) error {
	// Security fields.
	if cfg.Runner.ReadOnlyRootfs {
		return fmt.Errorf("runner.readOnlyRootfs must be false (the official runner writes to /home/runner)")
	}
	if !cfg.Runner.NoNewPrivileges {
		return fmt.Errorf("runner.noNewPrivileges must be true")
	}
	if len(cfg.Runner.CapDrop) != 1 || cfg.Runner.CapDrop[0] != "ALL" {
		return fmt.Errorf("runner.capDrop must be exactly [\"ALL\"]")
	}
	if cfg.Runner.Seccomp == "unconfined" || cfg.Runner.AppArmor == "unconfined" {
		return fmt.Errorf("unconfined seccomp/apparmor is not allowed")
	}
	switch cfg.Runner.Profile {
	case config.ProfileStandard:
		if len(cfg.Runner.CapAdd) > 0 {
			return fmt.Errorf("runner.capAdd must be empty for standard profile")
		}
	case config.ProfileDindRunner:
		allowed := config.DindCapabilities()
		for _, cap := range cfg.Runner.CapAdd {
			if !slices.Contains(allowed, cap) {
				return fmt.Errorf("runner.capAdd %q is not in the dind-runner allowed set", cap)
			}
		}
	}

	// The builder manages /var/lib/docker for dind-runner as a fixed
	// tmpfs, so also declaring it in runner.tmpfs could conflict on the
	// daemon side. Misconfiguration is rejected statically.
	if cfg.Runner.Profile == config.ProfileDindRunner {
		for _, spec := range cfg.Runner.Tmpfs {
			dest, _, err := parseTmpfsSpec(spec)
			if err != nil {
				return err
			}
			if dest == dindRunnerDataDir {
				return fmt.Errorf("runner.tmpfs destination %q is reserved for the dind-runner profile", dest)
			}
		}
	}
	return nil
}
