package docker

import (
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"

	"github.com/nukanoto/gha-docker-controller/internal/model"
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
		t.Fatalf("ユーザー HostConfig を拒否しました: %v", err)
	}

	spec = mustBuild(t, testConfig(t))
	if err := validateManagedSpec(spec); err != nil {
		t.Fatalf("nil HostConfig を拒否しました: %v", err)
	}
}

func TestValidateManagedSpec_PreservesLabelGuard(t *testing.T) {
	spec := mustBuild(t, testConfig(t))
	spec.create.Config.Labels[model.ManagedLabelKey] = "false"
	if err := validateManagedSpec(spec); err == nil || !strings.Contains(err.Error(), "managed label") {
		t.Fatalf("改変された managed label を拒否しませんでした: %v", err)
	}
}

func TestValidateManagedSpec_RequiresRunnerCommand(t *testing.T) {
	spec := mustBuild(t, testConfig(t))
	spec.create.Config.Cmd = nil
	if err := validateManagedSpec(spec); err == nil || !strings.Contains(err.Error(), "runner command") {
		t.Fatalf("runner command の改変を拒否しませんでした: %v", err)
	}
}

func TestValidateManagedSpec_RejectsZeroValue(t *testing.T) {
	if err := validateManagedSpec(ManagedSpec{}); err == nil {
		t.Fatal("zero value の ManagedSpec を拒否しませんでした")
	}
}
