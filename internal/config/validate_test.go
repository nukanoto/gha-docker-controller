package config

import (
	"math"
	"strings"
	"testing"
	"time"
)

// This file verifies Config.validate with value inputs. Covered: scope,
// min/max, enum values, non-positive resources, incomplete App, security
// (capability, read-only rootfs, no-new-privileges, unconfined),
// dindRunner storage and network. Validation through YAML parsing is
// covered by load_test.go and schema_test.go; the format pure function
// details are covered by parse_format_test.go.

// validConfig returns the smallest valid Config for which validate returns no
// errors.
func validConfig() *Config {
	return &Config{
		GitHub: GitHubConfig{
			URL:   "https://github.com",
			Scope: ScopeOrganization,
			Owner: "my-org",
			App:   &GitHubAppConfig{AppID: 123, InstallationID: 456, PrivateKeyFile: "/tmp/gha-app.pem"},
			Token: "",
		},
		ScaleSet: ScaleSetConfig{Name: "prod", RunnerGroup: "default", MinRunners: 0, MaxRunners: 4},
		Docker: DockerConfig{
			Host:       "unix:///var/run/docker.sock",
			Runtime:    "runsc",
			Network:    "bridge",
			PullPolicy: PullPolicyIfNotPresent,
		},
		Runner: RunnerConfig{
			// A digest reference is used so profile-change tests to
			// dind-runner also pass.
			Image:               "ghcr.io/actions/actions-runner@sha256:0cfdcc701ce933c6d243c6b0b2da767366dc9f2e99961d4c3754b0b78084cdda",
			Profile:             ProfileStandard,
			CPU:                 NanoCPUs(2000000000),
			Memory:              Memory(4 << 30),
			MemorySwap:          Memory(4 << 30),
			PidsLimit:           512,
			ProvisioningTimeout: Duration(5 * time.Minute),
			StopTimeout:         Duration(30 * time.Second),
			CapDrop:             []string{"ALL"},
			Network:             "bridge",
			NoNewPrivileges:     true,
		},
		DindRunner: DindRunnerConfig{Storage: DefaultDindStorage, StorageSize: DefaultDindStorageSize},
		Health:     HealthConfig{Listen: "127.0.0.1:8080"},
		Shutdown:   ShutdownConfig{BusyPolicy: ShutdownPolicyLeave, Grace: Duration(DefaultShutdownGrace)},
		Log:        LogConfig{Format: LogFormatJSON, Level: LogLevelInfo},
	}
}

// runValidate searches the validate errors after mutation for one containing
// wantErr. When wantErr is empty it requires that no errors are returned.
func runValidate(t *testing.T, name string, mutate func(*Config), wantErr string) {
	t.Helper()
	c := validConfig()
	mutate(c)
	errs := c.validate()
	if wantErr == "" {
		if len(errs) != 0 {
			t.Fatalf("%s: 期待しない error が返りました: %v", name, errs)
		}
		return
	}
	for _, e := range errs {
		if strings.Contains(e.Error(), wantErr) {
			return
		}
	}
	t.Fatalf("%s: 期待 error %q がありません: %v", name, wantErr, errs)
}

// TestValidate_ScopeOwnerRepository verifies the scope enum values and the
// owner/repository requiredness and exclusivity.
func TestValidate_ScopeOwnerRepository(t *testing.T) {
	tests := []struct {
		name    string
		scope   string
		owner   string
		repo    string
		wantErr string
	}{
		{name: "organization scope is allowed", scope: ScopeOrganization, owner: "my-org"},
		{name: "repository scope is allowed", scope: ScopeRepository, owner: "my-org", repo: "my-repo"},
		{name: "unknown scope is rejected", scope: "enterprise", owner: "my-org", wantErr: "github.scope"},
		{name: "empty owner is rejected", scope: ScopeOrganization, wantErr: "github.owner"},
		{name: "owner with invalid characters is rejected", scope: ScopeOrganization, owner: "my org!", wantErr: "github.owner"},
		{name: "repository scope without repository is rejected", scope: ScopeRepository, owner: "my-org", wantErr: "github.repository: required"},
		{name: "repository with invalid characters is rejected", scope: ScopeRepository, owner: "my-org", repo: "my repo", wantErr: "github.repository"},
		{name: "organization scope with repository is rejected", scope: ScopeOrganization, owner: "my-org", repo: "my-repo", wantErr: "must be empty for organization scope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runValidate(t, tt.name, func(c *Config) {
				c.GitHub.Scope = tt.scope
				c.GitHub.Owner = tt.owner
				c.GitHub.Repository = tt.repo
			}, tt.wantErr)
		})
	}
}

// TestValidate_GitHubAppRequiredIds verifies that App id and installationId
// are positive integers and privateKeyFile is mandatory.
func TestValidate_GitHubAppRequiredIds(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*GitHubAppConfig)
		wantErr string
	}{
		{name: "zero appId is rejected", mutate: func(a *GitHubAppConfig) { a.AppID = 0 }, wantErr: "github.app.id"},
		{name: "negative installationId is rejected", mutate: func(a *GitHubAppConfig) { a.InstallationID = -1 }, wantErr: "github.app.installationId"},
		{name: "empty privateKeyFile is rejected", mutate: func(a *GitHubAppConfig) { a.PrivateKeyFile = "" }, wantErr: "github.app.privateKeyFile"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runValidate(t, tt.name, func(c *Config) { tt.mutate(c.GitHub.App) }, tt.wantErr)
		})
	}
}

// TestValidate_ScalesetMinMax verifies min>max, out-of-range max and name
// rejection, and accepts boundary values.
func TestValidate_ScalesetMinMax(t *testing.T) {
	tests := []struct {
		name    string
		min     int
		max     int
		setName string
		wantErr string
	}{
		{name: "zero min with positive max is allowed", min: 0, max: 4},
		{name: "min greater than max is rejected", min: 5, max: 4, wantErr: "scaleSet.minRunners: must be <= scaleSet.maxRunners"},
		{name: "negative min is rejected", min: -1, max: 4, wantErr: "scaleSet.minRunners: must be >= 0"},
		{name: "zero max is rejected", min: 0, max: 0, wantErr: "scaleSet.maxRunners: required, must be in range 1..2147483647"},
		{name: "negative max is rejected", min: 0, max: -2, wantErr: "scaleSet.maxRunners"},
		{name: "minimal max is allowed", min: 0, max: 1},
		{name: "max boundary is allowed", min: 0, max: math.MaxInt32},
		{name: "max above boundary is rejected", min: 0, max: math.MaxInt32 + 1, wantErr: "scaleSet.maxRunners: required, must be in range"},
		{name: "empty name is rejected", min: 0, max: 4, setName: "", wantErr: "scaleSet.name"},
		{name: "invalid name characters are rejected", min: 0, max: 4, setName: "bad name!", wantErr: "scaleSet.name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runValidate(t, tt.name, func(c *Config) {
				c.ScaleSet.MinRunners = tt.min
				c.ScaleSet.MaxRunners = tt.max
				if tt.setName != "" || strings.Contains(tt.wantErr, "scaleSet.name") {
					c.ScaleSet.Name = tt.setName
				}
			}, tt.wantErr)
		})
	}
}

// TestValidate_FieldPathWiring verifies with representative examples that
// format pure function errors are integrated with a field path. Individual
// rule details are covered by parse_format_test.go.
func TestValidate_FieldPathWiring(t *testing.T) {
	runValidate(t, "ghe url is rejected", func(c *Config) { c.GitHub.URL = "https://ghe.com" }, "github.url: host must be exactly github.com")
	runValidate(t, "tcp host is rejected", func(c *Config) { c.Docker.Host = "tcp://127.0.0.1:2375" }, "docker.host: only unix://")
}

// TestValidate_EnumValues verifies invalid enum values for pull policy,
// profile, busy policy and log format/level are rejected.
func TestValidate_EnumValues(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "unknown pull policy", mutate: func(c *Config) { c.Docker.PullPolicy = "sometimes" }, wantErr: "docker.pullPolicy: must be one of always, if-not-present, never"},
		{name: "empty pull policy", mutate: func(c *Config) { c.Docker.PullPolicy = "" }, wantErr: "docker.pullPolicy"},
		{name: "unknown profile", mutate: func(c *Config) { c.Runner.Profile = "custom" }, wantErr: "runner.profile: must be one of standard, dind-runner"},
		{name: "old nested profile is rejected", mutate: func(c *Config) { c.Runner.Profile = "nested-docker" }, wantErr: "runner.profile: must be one of standard, dind-runner"},
		{name: "unknown busy policy", mutate: func(c *Config) { c.Shutdown.BusyPolicy = "kill" }, wantErr: "shutdown.busyRunnerPolicy"},
		{name: "unknown log format", mutate: func(c *Config) { c.Log.Format = "yaml" }, wantErr: "log.format"},
		{name: "unknown log level", mutate: func(c *Config) { c.Log.Level = "verbose" }, wantErr: "log.level"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runValidate(t, tt.name, tt.mutate, tt.wantErr)
		})
	}
}

// TestValidate_NonPositiveResources requires positive CPU/memory/memorySwap/
// PIDs and swap>=memory.
func TestValidate_NonPositiveResources(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "zero cpu is rejected", mutate: func(c *Config) { c.Runner.CPU = 0 }, wantErr: "runner.cpu: required"},
		{name: "zero memory is rejected", mutate: func(c *Config) { c.Runner.Memory = 0 }, wantErr: "runner.memory: required"},
		{name: "zero memorySwap is rejected", mutate: func(c *Config) { c.Runner.MemorySwap = 0 }, wantErr: "runner.memorySwap: required"},
		{name: "swap below memory is rejected", mutate: func(c *Config) { c.Runner.MemorySwap = Memory(1 << 30) }, wantErr: "runner.memorySwap: must be >= runner.memory"},
		{name: "zero pidsLimit is rejected", mutate: func(c *Config) { c.Runner.PidsLimit = 0 }, wantErr: "runner.pidsLimit: required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runValidate(t, tt.name, tt.mutate, tt.wantErr)
		})
	}
}

// TestValidate_SecurityBasics verifies rejection of read-only rootfs,
// no-new-privileges false, CapDrop other than ALL, standard CapAdd and
// unconfined.
func TestValidate_SecurityBasics(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "readOnlyRootfs true is rejected", mutate: func(c *Config) { c.Runner.ReadOnlyRootfs = true }, wantErr: "runner.readOnlyRootfs: must be false"},
		{name: "noNewPrivileges false is rejected", mutate: func(c *Config) { c.Runner.NoNewPrivileges = false }, wantErr: "runner.noNewPrivileges: must be true"},
		{name: "single capability drop is rejected", mutate: func(c *Config) { c.Runner.CapDrop = []string{"CHOWN"} }, wantErr: "runner.capDrop: only [\"ALL\"] is allowed"},
		{name: "extra capability drop is rejected", mutate: func(c *Config) { c.Runner.CapDrop = []string{"ALL", "CHOWN"} }, wantErr: "runner.capDrop"},
		{name: "empty capDrop is rejected", mutate: func(c *Config) { c.Runner.CapDrop = []string{} }, wantErr: "runner.capDrop"},
		{name: "standard capAdd is rejected", mutate: func(c *Config) { c.Runner.CapAdd = []string{"CHOWN"} }, wantErr: "runner.capAdd: must be empty for standard profile"},
		{name: "standard full dind capAdd is rejected", mutate: func(c *Config) { c.Runner.CapAdd = DindCapabilities() }, wantErr: "runner.capAdd: must be empty for standard profile"},
		{name: "seccomp unconfined is rejected", mutate: func(c *Config) { c.Runner.Seccomp = "unconfined" }, wantErr: "runner.seccomp: \"unconfined\" is not allowed"},
		{name: "apparmor unconfined is rejected", mutate: func(c *Config) { c.Runner.AppArmor = "unconfined" }, wantErr: "runner.apparmor: \"unconfined\" is not allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runValidate(t, tt.name, tt.mutate, tt.wantErr)
		})
	}
}

// TestValidate_DindCapAddExactSet verifies that dind CapAdd allows only
// subsets of the 17-capability allowed set, rejects anything outside, and
// that mutating the returned slice does not affect the fixed set.
func TestValidate_DindCapAddExactSet(t *testing.T) {
	// Fix the allowed set size at 17 with no duplicates.
	dind := DindCapabilities()
	if len(dind) != 17 {
		t.Fatalf("DindCapabilities の個数が不正です: 期待値 %d、実測値 %d", 17, len(dind))
	}
	seen := make(map[string]bool, len(dind))
	for _, cap := range dind {
		if seen[cap] {
			t.Fatalf("DindCapabilities に重複があります: %q", cap)
		}
		seen[cap] = true
	}

	// Mutating the returned slice does not change the fixed set (defensive
	// copy).
	dind[0] = "CORRUPTED"
	if got := DindCapabilities()[0]; got != "AUDIT_WRITE" {
		t.Fatalf("DindCapabilities の固定集合が変更されました: %q", got)
	}

	// Each single-element subset and the full set are accepted.
	for _, cap := range DindCapabilities() {
		runValidate(t, "allowed_"+cap, func(c *Config) {
			c.Runner.Profile = ProfileDindRunner
			c.Runner.CapAdd = []string{cap}
		}, "")
	}
	runValidate(t, "full dind set is allowed", func(c *Config) {
		c.Runner.Profile = ProfileDindRunner
		c.Runner.CapAdd = DindCapabilities()
	}, "")

	// Capabilities outside the set are rejected.
	tests := []struct {
		name string
		cap  string
	}{
		{name: "SYS_TIME is rejected", cap: "SYS_TIME"},
		{name: "ALL is rejected", cap: "ALL"},
		{name: "NET_BROADCAST is rejected", cap: "NET_BROADCAST"},
		{name: "empty capability is rejected", cap: ""},
	}
	for _, tt := range tests {
		runValidate(t, tt.name, func(c *Config) {
			c.Runner.Profile = ProfileDindRunner
			c.Runner.CapAdd = []string{tt.cap}
		}, "runner.capAdd: \""+tt.cap+"\" is not in the dind-runner allowed set")
	}
}

// TestValidate_RuntimeProfileCombination verifies that standard allows any
// registered runtime whiledind-runner is fixed to runsc, rejected with an
// error on the docker.runtime path.
func TestValidate_RuntimeProfileCombination(t *testing.T) {
	tests := []struct {
		name    string
		profile string
		runtime string
		wantErr string
	}{
		{name: "standard with runsc is allowed", profile: ProfileStandard, runtime: "runsc"},
		{name: "standard with runc is allowed", profile: ProfileStandard, runtime: "runc"},
		{name: "standard with custom runtime is allowed", profile: ProfileStandard, runtime: "kata-runtime"},
		{name: "dind with runsc is allowed", profile: ProfileDindRunner, runtime: "runsc"},
		{name: "dind with runc is rejected", profile: ProfileDindRunner, runtime: "runc", wantErr: "docker.runtime: dind-runner profile requires"},
		{name: "dind with empty runtime is rejected", profile: ProfileDindRunner, runtime: "", wantErr: "docker.runtime"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runValidate(t, tt.name, func(c *Config) {
				c.Runner.Profile = tt.profile
				c.Docker.Runtime = tt.runtime
			}, tt.wantErr)
		})
	}
}

// TestValidate_NetworkRules verifies container mode, host, none and the
// docker.network/runner.network mismatch are rejected on both paths.
func TestValidate_NetworkRules(t *testing.T) {
	tests := []struct {
		name    string
		docker  string
		runner  string
		wantErr string
	}{
		{name: "same network is allowed", docker: "container-net", runner: "container-net"},
		{name: "mismatch is rejected", docker: "bridge", runner: "my-net", wantErr: "runner.network: must match docker.network when both are set"},
		{name: "runner host network is rejected", docker: "bridge", runner: "host", wantErr: "runner.network: host network is not allowed"},
		{name: "empty runner network is rejected", docker: "bridge", runner: "", wantErr: "runner.network: required"},
		{name: "docker container mode is rejected", docker: "container", runner: "container", wantErr: "docker.network: network mode \"container\" is not allowed"},
		{name: "docker container:id mode is rejected", docker: "container:db", runner: "container:db", wantErr: "docker.network: network mode \"container:db\" is not allowed"},
		{name: "runner container mode is rejected", docker: "bridge", runner: "container", wantErr: "runner.network: network mode \"container\" is not allowed"},
		{name: "runner container:name mode is rejected", docker: "bridge", runner: "container:db", wantErr: "runner.network: network mode \"container:db\" is not allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runValidate(t, tt.name, func(c *Config) {
				c.Docker.Network = tt.docker
				c.Runner.Network = tt.runner
			}, tt.wantErr)
		})
	}
}

// TestValidate_DindRunnerStorage verifies positive storageSize and the
// tmpfs-only storage. The host daemon runsc runtimeArgs are out of scope for
// config; they are left to the check warning and a manual operator check.
func TestValidate_DindRunnerStorage(t *testing.T) {
	tests := []struct {
		name    string
		storage Memory
		kind    string
		wantErr string
	}{
		{name: "default storage size is allowed", storage: DefaultDindStorageSize},
		{name: "zero storage size is rejected", storage: 0, wantErr: "dindRunner.storageSize: must be positive"},
		{name: "non tmpfs storage is rejected", storage: DefaultDindStorageSize, kind: "volume", wantErr: "dindRunner.storage: only \"tmpfs\" is supported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runValidate(t, tt.name, func(c *Config) {
				c.DindRunner.StorageSize = tt.storage
				if tt.kind != "" {
					c.DindRunner.Storage = tt.kind
				}
			}, tt.wantErr)
		})
	}
}

// TestValidate_ImageWiring verifies that invalid runner.image values are
// rejected with a field path.
func TestValidate_ImageWiring(t *testing.T) {
	runValidate(t, "latest tag is rejected", func(c *Config) { c.Runner.Image = "ghcr.io/actions/actions-runner:latest" }, "runner.image: tag \"latest\" is not allowed")
	runValidate(t, "tagless is rejected", func(c *Config) { c.Runner.Image = "ghcr.io/actions/actions-runner" }, "runner.image: tag or digest is required")
	runValidate(t, "empty image is rejected", func(c *Config) { c.Runner.Image = "" }, "runner.image: required")
}

// TestValidate_DNSAndExtraHostsAndTmpfs verifies that invalid DNS,
// extraHosts and tmpfs values are rejected with an element index.
func TestValidate_DNSAndExtraHostsAndTmpfs(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "valid ipv4 dns is allowed", mutate: func(c *Config) { c.Runner.DNS = []string{"1.1.1.1", "8.8.8.8"} }},
		{name: "valid ipv6 dns is allowed", mutate: func(c *Config) { c.Runner.DNS = []string{"2001:db8::1"} }},
		{name: "invalid dns is rejected", mutate: func(c *Config) { c.Runner.DNS = []string{"not-an-ip"} }, wantErr: "runner.dns[0]: invalid IP address"},
		{name: "valid extraHosts are allowed", mutate: func(c *Config) { c.Runner.ExtraHosts = []string{"host.internal:192.168.0.10"} }},
		{name: "host-gateway extraHost is allowed", mutate: func(c *Config) { c.Runner.ExtraHosts = []string{"host:host-gateway"} }},
		{name: "invalid extraHost is rejected", mutate: func(c *Config) { c.Runner.ExtraHosts = []string{"broken"} }, wantErr: "runner.extraHosts[0]"},
		{name: "invalid tmpfs is rejected", mutate: func(c *Config) { c.Runner.Tmpfs = []string{"run"} }, wantErr: "runner.tmpfs[0]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runValidate(t, tt.name, tt.mutate, tt.wantErr)
		})
	}
}
