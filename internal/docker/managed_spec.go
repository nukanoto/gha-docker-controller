package docker

import (
	"fmt"
	"maps"
	"time"

	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"

	"github.com/nukanoto/gha-docker-controller/internal/config"
	"github.com/nukanoto/gha-docker-controller/internal/model"
)

const (
	jitEnvKey          = "ACTIONS_RUNNER_INPUT_JITCONFIG"
	returnEnvKey       = "ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE"
	userAgentEnvPrefix = "GITHUB_ACTIONS_RUNNER_EXTRA_USER_AGENT"
)

// ManagedSpec contains the create request and the controller-owned identity.
// HostConfig is intentionally not synthesized or restricted here.
type ManagedSpec struct {
	create   mobyclient.ContainerCreateOptions
	identity model.RunnerIdentity
	labels   map[string]string
}

// ManagedSpecInput is the typed input for BuildManagedSpec.
type ManagedSpecInput struct {
	Config             *config.Config
	Identity           model.RunnerIdentity
	JITConfig          string
	ControllerInstance string
	CreatedAt          time.Time
	ContainerName      string
	UserAgentVersion   string
}

// BuildManagedSpec assembles the controller-owned image, environment and
// labels. Docker container configuration and HostConfig are otherwise left to
// the image and the user's HostConfig value.
func BuildManagedSpec(input ManagedSpecInput) (ManagedSpec, error) {
	if err := validateSpecInput(input.Config, input); err != nil {
		return ManagedSpec{}, fmt.Errorf("build managed spec: %w", err)
	}
	cfg := input.Config
	labels := model.BuildLabels(input.Identity, input.ControllerInstance, input.CreatedAt)
	env := []string{
		jitEnvKey + "=" + input.JITConfig,
		returnEnvKey + "=1",
		userAgentEnvPrefix + "=gha-docker-controller/" + input.UserAgentVersion,
	}

	var stopTimeout *int
	if timeout := time.Duration(cfg.Runner.StopTimeout); timeout > 0 {
		seconds := stopTimeoutSeconds(timeout)
		stopTimeout = &seconds
	}

	return ManagedSpec{
		identity: input.Identity,
		labels:   maps.Clone(labels),
		create: mobyclient.ContainerCreateOptions{
			Name: input.ContainerName,
			Config: &container.Config{
				Image:       cfg.Runner.Image,
				Env:         env,
				Labels:      labels,
				StopTimeout: stopTimeout,
			},
			HostConfig: cfg.Runner.HostConfig,
		},
	}, nil
}
