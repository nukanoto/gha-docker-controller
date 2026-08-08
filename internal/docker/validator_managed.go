package docker

import (
	"fmt"
	"strings"

	"github.com/nukanoto/gha-docker-controller/internal/model"
)

// validateManagedSpec re-checks only the controller-owned create contract.
// User-provided container settings are deliberately outside this validator.
func validateManagedSpec(spec ManagedSpec) error {
	if spec.create.Config == nil {
		return fmt.Errorf("refusing to create: managed spec is zero or missing container config")
	}
	if spec.identity.ScaleSetID <= 0 || spec.identity.RunnerID <= 0 {
		return fmt.Errorf("refusing to create: managed spec has no valid runner identity")
	}
	if !model.ValidRunnerName(spec.identity.RunnerName) {
		return fmt.Errorf("refusing to create: runner name %q is not a valid canonical runner name", spec.identity.RunnerName)
	}
	if spec.create.Name == "" {
		return fmt.Errorf("refusing to create: managed spec has no container name")
	}
	if spec.create.Config.Image == "" {
		return fmt.Errorf("refusing to create: managed spec has no image")
	}

	if err := model.ValidateLabels(spec.labels, spec.identity); err != nil {
		return fmt.Errorf("refusing to create: %w", err)
	}
	if err := model.ValidateLabels(spec.create.Config.Labels, spec.identity); err != nil {
		return fmt.Errorf("refusing to create: %w", err)
	}
	if !sameLabels(spec.labels, spec.create.Config.Labels) {
		return fmt.Errorf("refusing to create: managed labels differ from container labels")
	}

	haveJIT, haveReturn, haveUserAgent := false, false, false
	for _, value := range spec.create.Config.Env {
		switch {
		case strings.HasPrefix(value, jitEnvKey+"="):
			if strings.TrimPrefix(value, jitEnvKey+"=") == "" {
				return fmt.Errorf("refusing to create: JIT config env value must not be empty")
			}
			haveJIT = true
		case value == returnEnvKey+"=1":
			haveReturn = true
		case strings.HasPrefix(value, userAgentEnvPrefix+"=gha-docker-controller/"):
			if strings.TrimPrefix(value, userAgentEnvPrefix+"=gha-docker-controller/") == "" {
				return fmt.Errorf("refusing to create: user agent env value must not be empty")
			}
			haveUserAgent = true
		}
	}
	if !haveJIT || !haveReturn || !haveUserAgent {
		return fmt.Errorf("refusing to create: JIT env contract is incomplete")
	}
	return nil
}

func sameLabels(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}
