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
	case config.ProfileNestedDocker:
		allowed := config.NestedCapabilities()
		for _, cap := range cfg.Runner.CapAdd {
			if !slices.Contains(allowed, cap) {
				return fmt.Errorf("runner.capAdd %q is not in the nested-docker allowed set", cap)
			}
		}
	}

	// The builder manages /var/lib/docker for nested-docker as a fixed
	// tmpfs, so also declaring it in runner.tmpfs could conflict on the
	// daemon side. Misconfiguration is rejected statically.
	if cfg.Runner.Profile == config.ProfileNestedDocker {
		for _, spec := range cfg.Runner.Tmpfs {
			dest, _, err := parseTmpfsSpec(spec)
			if err != nil {
				return err
			}
			if dest == nestedDockerDataDir {
				return fmt.Errorf("runner.tmpfs destination %q is reserved for the nested-docker profile", dest)
			}
		}
	}
	return nil
}
