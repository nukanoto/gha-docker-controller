package docker

import (
	"fmt"

	"github.com/nukanoto/gha-docker-controller/internal/config"
	"github.com/nukanoto/gha-docker-controller/internal/model"
)

// validateSpecInput checks the values required to create a managed runner.
func validateSpecInput(cfg *config.Config, input ManagedSpecInput) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	if cfg.Runner.Image == "" {
		return fmt.Errorf("runner image is empty")
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
	return nil
}
