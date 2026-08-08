package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Warning is a non-secret configuration warning.
type Warning struct {
	Path    string
	Message string
}

// Load parses, normalizes, and statically validates the configuration.
func Load(path string) (*Config, []Warning, error) {
	// This daemon supports GitHub.com only; reject the GHES override early.
	if v := os.Getenv("GITHUB_ACTIONS_FORCE_GHES"); v != "" {
		return nil, nil, fmt.Errorf("GITHUB_ACTIONS_FORCE_GHES environment variable must not be set; only GitHub.com is supported")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read config %s: %w", path, err)
	}
	raw, err := decodeYAML(data)
	if err != nil {
		return nil, nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	c, err := resolve(raw)
	if err != nil {
		return nil, nil, err
	}
	secretWarnings, err := loadSecrets(c)
	if err != nil {
		return nil, nil, err
	}
	if errs := c.validate(); len(errs) > 0 {
		return nil, secretWarnings, fmt.Errorf("invalid config %s:\n%s", path, joinErrors(errs))
	}
	return c, secretWarnings, nil
}

// decodeYAML rejects unknown fields and multiple YAML documents.
func decodeYAML(data []byte) (*rawConfig, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var raw rawConfig
	if err := dec.Decode(&raw); err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("config file is empty")
		}
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple YAML documents are not allowed")
		}
		return nil, err
	}
	return &raw, nil
}

// resolve applies defaults and trims user-provided strings.
func resolve(raw *rawConfig) (*Config, error) {
	if raw == nil {
		return nil, fmt.Errorf("empty config")
	}
	c := &Config{}

	c.GitHub.URL = strings.TrimRight(optionalString(raw.GitHub.URL, DefaultGitHubURL), "/")
	c.GitHub.Scope = raw.GitHub.Scope
	c.GitHub.Owner = strings.TrimSpace(raw.GitHub.Owner)
	c.GitHub.Repository = strings.TrimSpace(raw.GitHub.Repository)
	if raw.GitHub.App != nil {
		c.GitHub.App = &GitHubAppConfig{
			AppID:          raw.GitHub.App.AppID,
			InstallationID: raw.GitHub.App.InstallationID,
			PrivateKeyFile: strings.TrimSpace(raw.GitHub.App.PrivateKeyFile),
		}
	}

	c.ScaleSet.Name = strings.TrimSpace(raw.ScaleSet.Name)
	c.ScaleSet.RunnerGroup = strings.TrimSpace(optionalString(raw.ScaleSet.RunnerGroup, DefaultRunnerGroup))
	c.ScaleSet.MinRunners = optionalInt(raw.ScaleSet.MinRunners, 0)
	c.ScaleSet.MaxRunners = optionalInt(raw.ScaleSet.MaxRunners, 0)

	c.Docker.Host = normalizeDockerHost(optionalString(raw.Docker.Host, DefaultDockerHost))
	c.Docker.PullPolicy = strings.TrimSpace(optionalString(raw.Docker.PullPolicy, DefaultPullPolicy))

	c.Runner.Image = strings.TrimSpace(raw.Runner.Image)
	if raw.Runner.HostConfig != nil {
		c.Runner.HostConfig = &raw.Runner.HostConfig.HostConfig
	}
	c.Runner.ProvisioningTimeout = optionalDuration(raw.Runner.ProvisioningTimeout, Duration(DefaultProvisioningTimeout))
	c.Runner.StopTimeout = optionalDuration(raw.Runner.StopTimeout, Duration(DefaultStopTimeout))

	c.Health.Listen = strings.TrimSpace(optionalString(raw.Health.Listen, DefaultHealthListen))
	c.Shutdown.BusyPolicy = strings.TrimSpace(optionalString(raw.Shutdown.BusyPolicy, DefaultShutdownPolicy))
	c.Shutdown.Grace = optionalDuration(raw.Shutdown.Grace, Duration(DefaultShutdownGrace))
	c.Log.Format = strings.TrimSpace(optionalString(raw.Log.Format, DefaultLogFormat))
	c.Log.Level = strings.TrimSpace(optionalString(raw.Log.Level, DefaultLogLevel))

	return c, nil
}

// loadSecrets loads exactly one authentication method without exposing its
// value in errors or warnings.
func loadSecrets(c *Config) ([]Warning, error) {
	token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	switch {
	case c.GitHub.App == nil && token == "":
		return nil, fmt.Errorf("invalid config: github.app or GITHUB_TOKEN is required")
	case c.GitHub.App != nil && token != "":
		return nil, fmt.Errorf("invalid config: github.app and GITHUB_TOKEN are mutually exclusive")
	}
	var warnings []Warning
	if c.GitHub.App != nil {
		if c.GitHub.App.PrivateKeyFile == "" {
			return nil, fmt.Errorf("github.app.privateKeyFile: required")
		}
		content, mode, err := readSecretFile(c.GitHub.App.PrivateKeyFile)
		if err != nil {
			return nil, fmt.Errorf("github.app.privateKeyFile: %w", err)
		}
		trimmed := bytes.TrimSpace(content)
		if len(trimmed) == 0 {
			return nil, fmt.Errorf("github.app.privateKeyFile: secret file is empty")
		}
		c.GitHub.App.PrivateKey = trimmed
		// Group/other permissions can expose or tamper with the secret.
		if mode.Perm()&0o077 != 0 {
			warnings = append(warnings, Warning{
				Path:    "github.app.privateKeyFile",
				Message: fmt.Sprintf("secret file permissions are %s; group or others have permission bits set, set 0600", mode.Perm()),
			})
		}
		return warnings, nil
	}
	c.GitHub.Token = token
	return warnings, nil
}

// normalizeDockerHost removes trailing slashes from unix socket URLs.
func normalizeDockerHost(host string) string {
	if strings.HasPrefix(host, "unix://") {
		return "unix://" + strings.TrimRight(strings.TrimPrefix(host, "unix://"), "/")
	}
	return host
}

func optionalString(v *string, def string) string {
	if v == nil {
		return def
	}
	return *v
}

func optionalInt(v *int, def int) int {
	if v == nil {
		return def
	}
	return *v
}

func optionalDuration(v *Duration, def Duration) Duration {
	if v == nil {
		return def
	}
	return *v
}
