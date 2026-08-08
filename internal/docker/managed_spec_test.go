package docker

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"

	"github.com/nukanoto/gha-docker-controller/internal/config"
	"github.com/nukanoto/gha-docker-controller/internal/model"
)

func testConfig(t *testing.T, runtime ...string) *config.Config {
	t.Helper()
	cfg := &config.Config{
		Docker: config.DockerConfig{Host: config.DefaultDockerHost, PullPolicy: config.DefaultPullPolicy},
		Runner: config.RunnerConfig{
			Image:               "ubuntu",
			ProvisioningTimeout: config.Duration(5 * time.Minute),
			StopTimeout:         config.Duration(30 * time.Second),
		},
	}
	if len(runtime) == 1 {
		// Keep lifecycle fixtures observable after invalid JIT makes the runner exit.
		cfg.Runner.HostConfig = &container.HostConfig{
			Runtime:       runtime[0],
			NetworkMode:   "bridge",
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyAlways},
		}
	}
	return cfg
}

func testInput(cfg *config.Config) ManagedSpecInput {
	return ManagedSpecInput{
		Config:             cfg,
		Identity:           model.RunnerIdentity{ScaleSetID: 1, RunnerID: 2, RunnerName: model.RunnerName("set", "0123456789ab")},
		JITConfig:          "encoded-jit-config",
		ControllerInstance: "controller-1",
		CreatedAt:          time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC),
		ContainerName:      model.ContainerName("set", 2, "0123456789ab"),
		UserAgentVersion:   "test-version",
	}
}

func mustBuild(t *testing.T, cfg *config.Config) ManagedSpec {
	t.Helper()
	spec, err := BuildManagedSpec(testInput(cfg))
	if err != nil {
		t.Fatalf("managed spec を構築できませんでした: %v", err)
	}
	return spec
}

func TestBuildManagedSpec_UsesUserHostConfig(t *testing.T) {
	cfg := testConfig(t)
	cfg.Runner.HostConfig = &container.HostConfig{
		Privileged:  true,
		Binds:       []string{"/:/host"},
		Runtime:     "custom-runtime",
		NetworkMode: "host",
		CapAdd:      []string{"SYS_ADMIN"},
		CapDrop:     []string{"ALL"},
		Tmpfs:       map[string]string{"/tmp/cache": "rw"},
		Mounts:      []mount.Mount{{Type: mount.TypeBind, Source: "/source", Target: "/target"}},
		Init:        boolPointer(false),
		Resources:   container.Resources{CPUQuota: 100000, Memory: 4096},
	}
	spec := mustBuild(t, cfg)
	if spec.create.HostConfig != cfg.Runner.HostConfig {
		t.Fatal("HostConfig が同じ object として spec に渡されていません")
	}
	if !spec.create.HostConfig.Privileged || len(spec.create.HostConfig.Binds) != 1 ||
		spec.create.HostConfig.Runtime != "custom-runtime" || spec.create.HostConfig.CPUQuota != 100000 {
		t.Fatalf("HostConfig の値が失われています: %+v", spec.create.HostConfig)
	}
}

func TestBuildManagedSpec_LeavesHostConfigNil(t *testing.T) {
	spec := mustBuild(t, testConfig(t))
	if spec.create.HostConfig != nil {
		t.Fatalf("HostConfig 未指定時に nil ではありません: %+v", spec.create.HostConfig)
	}
}

func TestBuildManagedSpec_ControllerFields(t *testing.T) {
	spec := mustBuild(t, testConfig(t))
	if spec.create.Config.Image != "ubuntu" || !reflect.DeepEqual(spec.create.Config.Cmd, []string{runnerCommand}) || len(spec.create.Config.Env) != 3 {
		t.Fatalf("controller-owned config が不正です: %+v", spec.create.Config)
	}
	if !reflect.DeepEqual(spec.create.Config.Labels, spec.labels) {
		t.Fatalf("label の保持値が異なります: got=%v want=%v", spec.create.Config.Labels, spec.labels)
	}
	if err := validateManagedSpec(spec); err != nil {
		t.Fatalf("managed spec の validation に失敗しました: %v", err)
	}
}

func TestBuildManagedSpec_InputValidation(t *testing.T) {
	tests := []struct {
		name string
		edit func(*ManagedSpecInput)
		want string
	}{
		{"nil config", func(in *ManagedSpecInput) { in.Config = nil }, "config is nil"},
		{"empty JIT", func(in *ManagedSpecInput) { in.JITConfig = "" }, "JIT config is empty"},
		{"invalid identity", func(in *ManagedSpecInput) { in.Identity.RunnerID = 0 }, "positive scale set id"},
		{"empty name", func(in *ManagedSpecInput) { in.ContainerName = "" }, "container name is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig(t)
			input := testInput(cfg)
			tt.edit(&input)
			_, err := BuildManagedSpec(input)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("入力エラーが期待どおりではありません: %v", err)
			}
		})
	}
}

func boolPointer(value bool) *bool {
	return &value
}
