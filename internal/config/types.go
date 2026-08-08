// Package config provides the YAML config schema and static validation.
package config

import (
	"strings"

	"github.com/moby/moby/api/types/container"
)

const (
	ScopeOrganization = "organization"
	ScopeRepository   = "repository"

	PullPolicyAlways       = "always"
	PullPolicyIfNotPresent = "if-not-present"
	PullPolicyNever        = "never"

	ShutdownPolicyLeave = "leave"
	ShutdownPolicyStop  = "stop"

	LogFormatJSON = "json"
	LogFormatText = "text"

	LogLevelDebug = "debug"
	LogLevelInfo  = "info"
	LogLevelWarn  = "warn"
	LogLevelError = "error"
)

// Config is the validated runtime config.
type Config struct {
	GitHub   GitHubConfig
	ScaleSet ScaleSetConfig
	Docker   DockerConfig
	Runner   RunnerConfig
	Health   HealthConfig
	Shutdown ShutdownConfig
	Log      LogConfig
}

// GitHubConfig is the GitHub.com connection and authentication config.
type GitHubConfig struct {
	URL        string
	Scope      string
	Owner      string
	Repository string
	App        *GitHubAppConfig
	Token      string
}

// GitHubAppConfig is the GitHub App authentication config.
type GitHubAppConfig struct {
	AppID          int64
	InstallationID int64
	PrivateKeyFile string
	PrivateKey     []byte
}

// ScaleSetConfig is the Scale Set config.
type ScaleSetConfig struct {
	Name        string
	RunnerGroup string
	MinRunners  int
	MaxRunners  int
}

// DockerConfig is the Docker daemon connection and image pull policy.
type DockerConfig struct {
	Host       string
	PullPolicy string
}

// RunnerConfig is the runner container config.
type RunnerConfig struct {
	Image string
	// HostConfig is passed through without controller-owned defaults.
	HostConfig          *container.HostConfig
	ProvisioningTimeout Duration
	StopTimeout         Duration
}

// HealthConfig is the health endpoint config.
type HealthConfig struct {
	Listen string
}

// ShutdownConfig controls Busy runner handling at shutdown.
type ShutdownConfig struct {
	BusyPolicy string
	Grace      Duration
}

// LogConfig is the structured log config.
type LogConfig struct {
	Format string
	Level  string
}

// GitHubConfigURL builds the GitHub API base URL.
func (c *Config) GitHubConfigURL() string {
	base := strings.TrimSuffix(c.GitHub.URL, "/")
	if c.GitHub.Scope == ScopeRepository {
		return base + "/" + c.GitHub.Owner + "/" + c.GitHub.Repository
	}
	return base + "/" + c.GitHub.Owner
}

// raw* types are the strict YAML schema layer. Optional fields are pointers
// so resolve can distinguish omitted values from explicit zero values.
type rawConfig struct {
	GitHub   rawGitHub   `yaml:"github"`
	ScaleSet rawScaleSet `yaml:"scaleSet"`
	Docker   rawDocker   `yaml:"docker"`
	Runner   rawRunner   `yaml:"runner"`
	Health   rawHealth   `yaml:"health"`
	Shutdown rawShutdown `yaml:"shutdown"`
	Log      rawLog      `yaml:"log"`
}

type rawGitHub struct {
	URL        *string       `yaml:"url"`
	Scope      string        `yaml:"scope"`
	Owner      string        `yaml:"owner"`
	Repository string        `yaml:"repository"`
	App        *rawGitHubApp `yaml:"app"`
}

type rawGitHubApp struct {
	AppID          int64  `yaml:"id"`
	InstallationID int64  `yaml:"installationId"`
	PrivateKeyFile string `yaml:"privateKeyFile"`
}

type rawScaleSet struct {
	Name        string  `yaml:"name"`
	RunnerGroup *string `yaml:"runnerGroup"`
	MinRunners  *int    `yaml:"minRunners"`
	MaxRunners  *int    `yaml:"maxRunners"`
}

type rawDocker struct {
	Host       *string `yaml:"host"`
	PullPolicy *string `yaml:"pullPolicy"`
}

type rawRunner struct {
	Image               string          `yaml:"image"`
	HostConfig          *hostConfigYAML `yaml:"hostConfig"`
	ProvisioningTimeout *Duration       `yaml:"provisioningTimeout"`
	StopTimeout         *Duration       `yaml:"stopTimeout"`
}

type rawHealth struct {
	Listen *string `yaml:"listen"`
}

type rawShutdown struct {
	BusyPolicy *string   `yaml:"busyRunnerPolicy"`
	Grace      *Duration `yaml:"gracePeriod"`
}

type rawLog struct {
	Format *string `yaml:"format"`
	Level  *string `yaml:"level"`
}
