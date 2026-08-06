package config

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Warning is a notice found while loading config that contains no secret.
type Warning struct {
	// Path is the path of the affected config field.
	Path string
	// Message is the body of the notice. It never contains secret values.
	Message string
}

// Load reads the config file with strict YAML and runs, in order, default
// application, secret file loading, normalization and static validation, and
// returns the runtime config. The returned Warnings contain no secrets.
// Dynamic connectivity checks against Docker/GitHub are not performed.
func Load(path string) (*Config, []Warning, error) {
	// GITHUB_ACTIONS_FORCE_GHES points the runner at GHES. This daemon
	// supports GitHub.com only, so the variable is rejected as a likely
	// misconfiguration.
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

// decodeYAML enforces a strict decode and a single document. Unknown fields,
// duplicated sections and any document after the first are all rejected.
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

// resolve applies defaults and normalization. It only fixes up the syntax;
// semantically invalid values become errors with a field path in validate.
func resolve(raw *rawConfig) (*Config, error) {
	if raw == nil {
		return nil, fmt.Errorf("empty config")
	}
	c := &Config{}

	// GitHub
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

	// Scale Set
	c.ScaleSet.Name = strings.TrimSpace(raw.ScaleSet.Name)
	c.ScaleSet.RunnerGroup = strings.TrimSpace(optionalString(raw.ScaleSet.RunnerGroup, DefaultRunnerGroup))
	c.ScaleSet.MinRunners = optionalInt(raw.ScaleSet.MinRunners, 0)
	c.ScaleSet.MaxRunners = optionalInt(raw.ScaleSet.MaxRunners, 0)

	// Docker
	c.Docker.Host = normalizeDockerHost(optionalString(raw.Docker.Host, DefaultDockerHost))
	c.Docker.Runtime = strings.TrimSpace(optionalString(raw.Docker.Runtime, DefaultRuntime))
	dockerNetwork := strings.TrimSpace(optionalString(raw.Docker.Network, DefaultNetwork))
	c.Docker.Network = dockerNetwork
	c.Docker.PullPolicy = strings.TrimSpace(optionalString(raw.Docker.PullPolicy, DefaultPullPolicy))

	// Runner
	c.Runner.Image = strings.TrimSpace(raw.Runner.Image)
	c.Runner.Profile = strings.TrimSpace(optionalString(raw.Runner.Profile, DefaultProfile))
	if raw.Runner.CPU != nil {
		c.Runner.CPU = *raw.Runner.CPU
	}
	if raw.Runner.Memory != nil {
		c.Runner.Memory = *raw.Runner.Memory
	}
	if raw.Runner.MemorySwap != nil {
		c.Runner.MemorySwap = *raw.Runner.MemorySwap
	}
	if raw.Runner.PidsLimit != nil {
		c.Runner.PidsLimit = *raw.Runner.PidsLimit
	}
	c.Runner.ProvisioningTimeout = optionalDuration(raw.Runner.ProvisioningTimeout, Duration(DefaultProvisioningTimeout))
	c.Runner.StopTimeout = optionalDuration(raw.Runner.StopTimeout, Duration(DefaultStopTimeout))
	c.Runner.Ulimit = raw.Runner.Ulimit
	// tmpfs normalizes the YAML map[path]options into an internal list sorted
	// by path (either "path" or "path:options").
	c.Runner.Tmpfs = normalizeTmpfs(raw.Runner.Tmpfs)
	c.Runner.ReadOnlyRootfs = optionalBool(raw.Runner.ReadOnlyRootfs, false)
	c.Runner.NoNewPrivileges = optionalBool(raw.Runner.NoNewPrivileges, true)
	// CapDrop defaults to ALL. HostConfig CapDrop=["ALL"] is fixed for the
	// container.
	if raw.Runner.CapDrop != nil {
		c.Runner.CapDrop = *raw.Runner.CapDrop
	} else {
		c.Runner.CapDrop = []string{"ALL"}
	}
	if raw.Runner.CapAdd != nil {
		c.Runner.CapAdd = *raw.Runner.CapAdd
	} else if c.Runner.Profile == ProfileNestedDocker {
		// nested-docker applies the 17 nestedCapAdd capabilities by default.
		c.Runner.CapAdd = NestedCapabilities()
	}
	if raw.Runner.Seccomp != nil {
		c.Runner.Seccomp = *raw.Runner.Seccomp
	}
	if raw.Runner.AppArmor != nil {
		c.Runner.AppArmor = *raw.Runner.AppArmor
	}
	if raw.Runner.Network != nil {
		c.Runner.Network = strings.TrimSpace(*raw.Runner.Network)
	} else {
		// When runner.network is unset it inherits docker.network.
		c.Runner.Network = dockerNetwork
	}
	c.Runner.DNS = raw.Runner.DNS
	c.Runner.ExtraHosts = raw.Runner.ExtraHosts

	// NestedDocker
	c.NestedDocker.Storage = strings.TrimSpace(optionalString(raw.NestedDocker.Storage, DefaultNestedStorage))
	if raw.NestedDocker.StorageSize != nil {
		c.NestedDocker.StorageSize = *raw.NestedDocker.StorageSize
	} else {
		c.NestedDocker.StorageSize = DefaultNestedStorageSize
	}

	// Health / Shutdown / Log
	c.Health.Listen = strings.TrimSpace(optionalString(raw.Health.Listen, DefaultHealthListen))
	c.Shutdown.BusyPolicy = strings.TrimSpace(optionalString(raw.Shutdown.BusyPolicy, DefaultShutdownPolicy))
	c.Shutdown.Grace = optionalDuration(raw.Shutdown.Grace, Duration(DefaultShutdownGrace))
	c.Log.Format = strings.TrimSpace(optionalString(raw.Log.Format, DefaultLogFormat))
	c.Log.Level = strings.TrimSpace(optionalString(raw.Log.Level, DefaultLogLevel))

	return c, nil
}

// loadSecrets reads the App private key file and takes the PAT from the
// GITHUB_TOKEN environment variable. It handles the mutual exclusion of App
// and GITHUB_TOKEN, rejects symlinks and non-regular private key files, and
// warns about loose permissions. Secret bodies never appear in errors or
// warnings.
func loadSecrets(c *Config) ([]Warning, error) {
	// The PAT is read from the environment at Load time with surrounding
	// whitespace trimmed. Its value never appears in errors or warnings.
	token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	switch {
	case c.GitHub.App == nil && token == "":
		return nil, fmt.Errorf("invalid config: github.app or GITHUB_TOKEN is required")
	case c.GitHub.App != nil && token != "":
		return nil, fmt.Errorf("invalid config: github.app and GITHUB_TOKEN are mutually exclusive")
	}
	var warnings []Warning
	if c.GitHub.App != nil {
		// A missing file becomes an error with its path before validation.
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
		// Any group/other permission bit (0077) triggers a warning. Not only
		// read but also write/execute can expose or tamper with the secret.
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

// normalizeDockerHost trims trailing "/" from the unix socket path.
// Format validation is done by validate.
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

func optionalBool(v *bool, def bool) bool {
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

// normalizeTmpfs normalizes the YAML map[path]options into the internal
// []string form. Each element is the bare path when options are empty and
// "path:options" otherwise. The order is fixed to lexicographic (byte) order
// of paths; duplicate paths are already rejected by YAML duplicate key
// detection. The old sequence ([]string) form is rejected as a schema type
// mismatch.
func normalizeTmpfs(raw map[string]string) []string {
	if len(raw) == 0 {
		return nil
	}
	paths := slices.Collect(maps.Keys(raw))
	slices.Sort(paths)
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		spec := strings.TrimSpace(p)
		if opts := strings.TrimSpace(raw[p]); opts != "" {
			spec += ":" + opts
		}
		out = append(out, spec)
	}
	return out
}
