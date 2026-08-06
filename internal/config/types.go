// Package config provides the YAML config schema, default application,
// normalization and static validation. It performs no dynamic connectivity
// checks against Docker or GitHub; those are the responsibility of the check
// command (internal/app). Only the GitHub.com organization/repository scopes
// are supported, and the security contract assumes runner containers run on a
// sandbox runtime such as runsc.
package config

import "strings"

// Definitions of enum values such as scope and profile.
const (
	// ScopeOrganization is the organization scope.
	ScopeOrganization = "organization"
	// ScopeRepository is the repository scope.
	ScopeRepository = "repository"

	// ProfileStandard is the standard profile.
	ProfileStandard = "standard"
	// ProfileDindRunner is the dind-runner profile.
	ProfileDindRunner = "dind-runner"

	// PullPolicyAlways pulls the image every time.
	PullPolicyAlways = "always"
	// PullPolicyIfNotPresent pulls only when the image is not local.
	PullPolicyIfNotPresent = "if-not-present"
	// PullPolicyNever never pulls.
	PullPolicyNever = "never"

	// ShutdownPolicyLeave leaves Busy runners running at shutdown.
	ShutdownPolicyLeave = "leave"
	// ShutdownPolicyStop stops Busy runners after the shutdown grace period.
	ShutdownPolicyStop = "stop"

	// LogFormatJSON is the JSON log format.
	LogFormatJSON = "json"
	// LogFormatText is the text log format.
	LogFormatText = "text"

	// LogLevelDebug is the debug log level and above.
	LogLevelDebug = "debug"
	// LogLevelInfo is the info log level and above.
	LogLevelInfo = "info"
	// LogLevelWarn is the warn log level and above.
	LogLevelWarn = "warn"
	// LogLevelError is the error log level and above.
	LogLevelError = "error"
)

// Config is the validated runtime config. Load creates it and it is never
// mutated afterwards.
type Config struct {
	GitHub     GitHubConfig
	ScaleSet   ScaleSetConfig
	Docker     DockerConfig
	Runner     RunnerConfig
	DindRunner DindRunnerConfig
	Health     HealthConfig
	Shutdown   ShutdownConfig
	Log        LogConfig
}

// GitHubConfig is the GitHub.com connection and authentication config.
// The URL is always normalized to https://github.com, and the GitHub API base
// URL is composed from the scope and owner/repository.
type GitHubConfig struct {
	// URL is the normalized GitHub.com base URL. Always "https://github.com".
	URL string
	// Scope is ScopeOrganization or ScopeRepository.
	Scope string
	// Owner is the organization name or the repository owner name.
	Owner string
	// Repository is the repository name for the repository scope. Empty for
	// the organization scope.
	Repository string
	// App is the GitHub App authentication config. If nil, PAT auth via Token
	// is used.
	App *GitHubAppConfig
	// Token is the PAT read from the GITHUB_TOKEN environment variable. It
	// cannot be placed in the YAML body. It is secret and must never appear
	// in logs or errors.
	Token string
}

// GitHubAppConfig is the GitHub App authentication config.
type GitHubAppConfig struct {
	// AppID is the numeric GitHub App ID. It is converted to a decimal string
	// for the official client.
	AppID int64
	// InstallationID is the installation ID.
	InstallationID int64
	// PrivateKeyFile is the path of the App private key (PEM).
	PrivateKeyFile string
	// PrivateKey is the PEM body read from PrivateKeyFile. It is secret and
	// must never appear in logs or errors.
	PrivateKey []byte
}

// ScaleSetConfig is the Scale Set config.
type ScaleSetConfig struct {
	// Name is the Scale Set name.
	Name string
	// RunnerGroup is the runner group name.
	RunnerGroup string
	// MinRunners is the lower bound of runners kept at all times.
	MinRunners int
	// MaxRunners is the upper bound of runners. Mandatory.
	MaxRunners int
}

// DockerConfig is the Docker daemon connection and default network config.
type DockerConfig struct {
	// Host allows only an absolute unix:// path. tcp:// and ssh:// are
	// rejected as a security contract because they can carry credentials to a
	// remote daemon.
	Host string
	// Runtime is the OCI runtime name. Defaults to runsc.
	Runtime string
	// Network is the default network. host and none are rejected.
	Network string
	// PullPolicy is the image pull policy.
	PullPolicy string
}

// RunnerConfig is the runner container config. There are two profiles,
// standard and dind-runner, and the allowed values of security-related
// fields are fixed per profile (for example validateRunnerSecurity).
type RunnerConfig struct {
	// Image is an image reference with a digest or a version tag. latest and
	// references without tag/digest are rejected to prevent unreproducible
	// floating configurations. dind-runner allows only digest references
	// because the inner dockerd behavior is determined by the image.
	Image string
	// Profile is ProfileStandard or ProfileDindRunner.
	Profile string
	// CPU is the CPU limit in NanoCPUs. Zero means unlimited.
	CPU NanoCPUs
	// Memory is the memory limit in bytes. Zero means unlimited.
	Memory Memory
	// MemorySwap is the memory+swap limit in bytes. Zero means unlimited.
	MemorySwap Memory
	// PidsLimit is the process count limit. Zero means unlimited.
	PidsLimit int64
	// ProvisioningTimeout is the deadline for provisioning.
	ProvisioningTimeout Duration
	// StopTimeout is the grace period when stopping.
	StopTimeout Duration
	// Ulimit is the list of ulimit settings.
	Ulimit []Ulimit
	// Tmpfs is the normalized list of tmpfs mounts. YAML specifies a
	// map[path]options; each element is normalized to "path" (empty options)
	// or "path:options" in ascending path order (Docker CLI compatible).
	Tmpfs []string
	// ReadOnlyRootfs is always false. true is rejected because the official
	// runner writes to /home/runner.
	ReadOnlyRootfs bool
	// CapDrop is always ["ALL"]. Anything else is rejected.
	CapDrop []string
	// CapAdd is the additional capabilities per profile. Empty for standard;
	// only a subset of the 17 dindCapAdd capabilities is allowed for
	// dind-runner.
	CapAdd []string
	// Seccomp is the path of a seccomp profile JSON file. Empty means daemon
	// default. "unconfined" is rejected.
	Seccomp string
	// AppArmor is the AppArmor profile name. Empty means daemon default.
	// "unconfined" is rejected.
	AppArmor string
	// Network is the network the runner connects to. When unset it inherits
	// Docker.Network; when both are set they must match.
	Network string
	// DNS is the list of container DNS servers.
	DNS []string
	// ExtraHosts is the list of /etc/hosts entries (host:ip).
	ExtraHosts []string
	// NoNewPrivileges is always true. false is rejected.
	NoNewPrivileges bool
}

// DindRunnerConfig is the dind-runner profile config. The inner dockerd
// is sandboxed with runsc, and the host daemon runsc runtimeArgs cannot be
// verified through the Docker API, so verification is left to a manual check
// by the operator (see the README procedure).
type DindRunnerConfig struct {
	// Storage is the storage kind for /var/lib/docker. Only tmpfs is allowed
	// and it is also the default.
	Storage string
	// StorageSize is the tmpfs size for /var/lib/docker.
	StorageSize Memory
}

// HealthConfig is the listen address of the health endpoint.
type HealthConfig struct {
	// Listen is the listen address in host:port form.
	Listen string
}

// ShutdownConfig is how Busy runners are treated at shutdown. The default is
// leave (keep them running); stop also stops Busy runners after the grace
// period.
type ShutdownConfig struct {
	// BusyPolicy is ShutdownPolicyLeave or ShutdownPolicyStop.
	BusyPolicy string
	// Grace is how long to wait for Busy runners to stop.
	Grace Duration
}

// LogConfig is the structured log config. The production default is JSON
// format, emitted via helpers that never include secret values in fields.
type LogConfig struct {
	// Format is LogFormatJSON or LogFormatText.
	Format string
	// Level is one of debug, info, warn, error.
	Level string
}

// GitHubConfigURL builds the GitHub API base URL from github.url, scope,
// owner and repository. It is https://github.com/<owner> for organizations
// and https://github.com/<owner>/<repository> for repositories. Because it is
// embedded directly into official client queries, owner and repository are
// already validated to allow only [A-Za-z0-9_.-]+.
func (c *Config) GitHubConfigURL() string {
	base := strings.TrimSuffix(c.GitHub.URL, "/")
	if c.GitHub.Scope == ScopeRepository {
		return base + "/" + c.GitHub.Owner + "/" + c.GitHub.Repository
	}
	return base + "/" + c.GitHub.Owner
}

// raw* types are the YAML schema layer. Optional fields are pointers to
// distinguish unset from zero values. Unknown fields and old-style keys are
// rejected by yaml.v3 KnownFields(true).
type rawConfig struct {
	GitHub     rawGitHub     `yaml:"github"`
	ScaleSet   rawScaleSet   `yaml:"scaleSet"`
	Docker     rawDocker     `yaml:"docker"`
	Runner     rawRunner     `yaml:"runner"`
	DindRunner rawDindRunner `yaml:"dindRunner"`
	Health     rawHealth     `yaml:"health"`
	Shutdown   rawShutdown   `yaml:"shutdown"`
	Log        rawLog        `yaml:"log"`
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
	Runtime    *string `yaml:"runtime"`
	Network    *string `yaml:"network"`
	PullPolicy *string `yaml:"pullPolicy"`
}

type rawRunner struct {
	Image               string    `yaml:"image"`
	Profile             *string   `yaml:"profile"`
	CPU                 *NanoCPUs `yaml:"cpu"`
	Memory              *Memory   `yaml:"memory"`
	MemorySwap          *Memory   `yaml:"memorySwap"`
	PidsLimit           *int64    `yaml:"pidsLimit"`
	ProvisioningTimeout *Duration `yaml:"provisioningTimeout"`
	StopTimeout         *Duration `yaml:"stopTimeout"`
	Ulimit              []Ulimit  `yaml:"ulimit"`
	// Tmpfs is the tmpfs specification as map[path]options. An empty string
	// value means no options. The old []string (sequence) form is a type
	// mismatch and is rejected.
	Tmpfs           map[string]string `yaml:"tmpfs"`
	ReadOnlyRootfs  *bool             `yaml:"readOnlyRootfs"`
	CapDrop         *[]string         `yaml:"capDrop"`
	CapAdd          *[]string         `yaml:"capAdd"`
	Seccomp         *string           `yaml:"seccomp"`
	AppArmor        *string           `yaml:"apparmor"`
	Network         *string           `yaml:"network"`
	DNS             []string          `yaml:"dns"`
	ExtraHosts      []string          `yaml:"extraHosts"`
	NoNewPrivileges *bool             `yaml:"noNewPrivileges"`
}

type rawDindRunner struct {
	// Storage is the storage kind for /var/lib/docker. Currently only tmpfs
	// is allowed.
	Storage     *string `yaml:"storage"`
	StorageSize *Memory `yaml:"storageSize"`
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
