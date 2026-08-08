package docker

import (
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"

	"github.com/nukanoto/arc-docker/internal/model"
)

func TestValidateManagedSpec_AllowsDockerHostConfig(t *testing.T) {
	cfg := testConfig(t)
	cfg.Runner.HostConfig = &container.HostConfig{
		Privileged:  true,
		Binds:       []string{"/:/host"},
		Runtime:     "registered-runtime",
		NetworkMode: "none",
	}
	spec := mustBuild(t, cfg)
	if err := validateManagedSpec(spec); err != nil {
		t.Fatalf("rejected user HostConfig: %v", err)
	}

	spec = mustBuild(t, testConfig(t))
	if err := validateManagedSpec(spec); err != nil {
		t.Fatalf("rejected nil HostConfig: %v", err)
	}
}

func TestValidateManagedSpec_PreservesLabelGuard(t *testing.T) {
	spec := mustBuild(t, testConfig(t))
	spec.create.Config.Labels[model.ManagedLabelKey] = "false"
	if err := validateManagedSpec(spec); err == nil || !strings.Contains(err.Error(), "managed label") {
		t.Fatalf("accepted tampered managed labels: %v", err)
	}
}

func TestValidateManagedSpec_RequiresRunnerCommand(t *testing.T) {
	spec := mustBuild(t, testConfig(t))
	spec.create.Config.Cmd = nil
	if err := validateManagedSpec(spec); err == nil || !strings.Contains(err.Error(), "runner command") {
		t.Fatalf("accepted tampered runner command: %v", err)
	}
}

func TestValidateManagedSpec_RejectsZeroValue(t *testing.T) {
	if err := validateManagedSpec(ManagedSpec{}); err == nil {
		t.Fatal("accepted zero-value ManagedSpec")
	}
}
