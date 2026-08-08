package config

import (
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
)

func validConfig() *Config {
	return &Config{
		GitHub:   GitHubConfig{URL: DefaultGitHubURL, Scope: ScopeOrganization, Owner: "org"},
		ScaleSet: ScaleSetConfig{Name: "set", RunnerGroup: "default", MaxRunners: 1},
		Docker:   DockerConfig{Host: DefaultDockerHost, PullPolicy: DefaultPullPolicy},
		Runner:   RunnerConfig{Image: "ubuntu", ProvisioningTimeout: Duration(DefaultProvisioningTimeout), StopTimeout: Duration(DefaultStopTimeout)},
		Health:   HealthConfig{Listen: DefaultHealthListen},
		Shutdown: ShutdownConfig{BusyPolicy: DefaultShutdownPolicy, Grace: Duration(DefaultShutdownGrace)},
		Log:      LogConfig{Format: DefaultLogFormat, Level: DefaultLogLevel},
	}
}

func runValidate(t *testing.T, name string, mutate func(*Config), wantErr string) {
	t.Helper()
	c := validConfig()
	mutate(c)
	errs := c.validate()
	if wantErr == "" {
		if len(errs) != 0 {
			t.Fatalf("%s: 予期しない validation error です: %v", name, errs)
		}
		return
	}
	for _, err := range errs {
		if strings.Contains(err.Error(), wantErr) {
			return
		}
	}
	t.Fatalf("%s: validation error %q がありません: %v", name, wantErr, errs)
}

func TestValidate_ImageReferences(t *testing.T) {
	tests := []struct {
		name  string
		image string
		err   string
	}{
		{"tagless is allowed", "ubuntu", ""},
		{"latest is allowed", "ubuntu:latest", ""},
		{"digest is allowed", "ubuntu@sha256:" + strings.Repeat("a", 64), ""},
		{"empty is rejected", "", "runner.image: required"},
		{"invalid syntax is rejected", "ubuntu:bad!", "runner.image: invalid image name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runValidate(t, tt.name, func(c *Config) { c.Runner.Image = tt.image }, tt.err)
		})
	}
}

func TestValidate_HostConfigHasNoControllerPolicy(t *testing.T) {
	runValidate(t, "HostConfig is user controlled", func(c *Config) {
		c.Runner.HostConfig = &container.HostConfig{
			Privileged:  true,
			Binds:       []string{"/:/host"},
			Runtime:     "custom-runtime",
			NetworkMode: "host",
		}
	}, "")
}

func TestValidate_RemovedDockerFieldsAreNotNeeded(t *testing.T) {
	runValidate(t, "Docker defaults", func(c *Config) {
		c.Docker = DockerConfig{Host: DefaultDockerHost, PullPolicy: PullPolicyNever}
	}, "")
	runValidate(t, "invalid pull policy", func(c *Config) {
		c.Docker.PullPolicy = "sometimes"
	}, "docker.pullPolicy")
	runValidate(t, "invalid Docker host", func(c *Config) {
		c.Docker.Host = "tcp://127.0.0.1:2375"
	}, "docker.host")
}

func TestValidate_RequiredGitHubAndScaleSetFields(t *testing.T) {
	runValidate(t, "missing owner", func(c *Config) { c.GitHub.Owner = "" }, "github.owner")
	runValidate(t, "missing scale set", func(c *Config) { c.ScaleSet.Name = "" }, "scaleSet.name")
	runValidate(t, "invalid health", func(c *Config) { c.Health.Listen = "localhost" }, "health.listen")
}
