// managed_spec_test.go consolidates the pure unit tests of BuildManagedSpec
// into one file. The standard /dind-runner delta is expressed as table
// rows, and the common security fields (privileged=false, host
// namespace/socket/device/mount prohibitions, cap drop/add, resources,
// network, stop timeout) are checked once in a common check. Only the
// seccomp/AppArmor file reads use real files; no Docker daemon connection is
// made. No mock/stub is used.
package docker

import (
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"

	"github.com/nukanoto/gha-docker-controller/internal/config"
	"github.com/nukanoto/gha-docker-controller/internal/model"
)

// testImage is the official runner image digest for the standard profile.
// This test does not check that the image exists, so the pinned digest is
// used as-is.
const testImage = "ghcr.io/actions/actions-runner@sha256:0cfdcc701ce933c6d243c6b0b2da767366dc9f2e99961d4c3754b0b78084cdda"

// testConfig builds a validated-equivalent config as builder input. It does
// not load YAML; it sets the profile-required values directly.
func testConfig(t *testing.T, profile, runtime string) *config.Config {
	t.Helper()
	cfg := &config.Config{
		ScaleSet: config.ScaleSetConfig{Name: "my-scale-set", RunnerGroup: "default", MinRunners: 0, MaxRunners: 4},
		Docker: config.DockerConfig{
			Host:       "unix:///var/run/docker.sock",
			Runtime:    runtime,
			Network:    "bridge",
			PullPolicy: config.PullPolicyIfNotPresent,
		},
		Runner: config.RunnerConfig{
			Image:               testImage,
			Profile:             profile,
			CPU:                 config.NanoCPUs(2e9),
			Memory:              config.Memory(4 * 1024 * 1024 * 1024),
			MemorySwap:          config.Memory(6 * 1024 * 1024 * 1024),
			PidsLimit:           512,
			ProvisioningTimeout: config.Duration(5 * time.Minute),
			StopTimeout:         config.Duration(30 * time.Second),
			Ulimit:              []config.Ulimit{{Name: "nofile", Soft: 1024, Hard: 2048}},
			Tmpfs:               []string{"/tmp:size=64m,ro"},
			ReadOnlyRootfs:      false,
			CapDrop:             []string{"ALL"},
			NoNewPrivileges:     true,
			Network:             "bridge",
			DNS:                 []string{"1.1.1.1"},
			ExtraHosts:          []string{"db:127.0.0.1"},
		},
		DindRunner: config.DindRunnerConfig{
			StorageSize: config.DefaultDindStorageSize,
		},
	}
	if profile == config.ProfileDindRunner {
		// dind-runner carries the full allowed capability set by default.
		cfg.Runner.CapAdd = config.DindCapabilities()
	}
	return cfg
}

// testInput builds the minimal builder input matching testConfig. The
// runner name follows the canonical form.
func testInput(cfg *config.Config) ManagedSpecInput {
	return ManagedSpecInput{
		Config: cfg,
		Identity: model.RunnerIdentity{
			ScaleSetID: 42,
			RunnerID:   7,
			RunnerName: "my-scale-set-0123456789ab",
		},
		JITConfig:          "encoded-jit-config",
		ControllerInstance: "test-controller-instance",
		CreatedAt:          time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		ContainerName:      "ghadc-my-scale-set-r7-01234567",
		UserAgentVersion:   "9.9.9",
	}
}

// mustBuild is a helper that requires BuildManagedSpec to succeed.
func mustBuild(t *testing.T, cfg *config.Config) ManagedSpec {
	t.Helper()
	spec, err := BuildManagedSpec(testInput(cfg))
	if err != nil {
		t.Fatalf("BuildManagedSpec failed: %v", err)
	}
	return spec
}

// TestBuildManagedSpec verifies all fields of both profiles in a table.
// Each row carries only the profile delta (user, entrypoint, cap add,
// dind tmpfs mount, runtime value); the common security fields are
// checked once in a common check. The configured runtime (runsc/runc),
// User, Cmd run.sh, CapDrop ALL, non-privileged, non-host, resources,
// security options, six labels, JIT env, DNS/extraHosts and
// no-new-privileges are included.
func TestBuildManagedSpec(t *testing.T) {
	cases := []struct {
		name       string
		profile    string
		runtime    string
		user       string
		entrypoint []string
		capAdd     []string
		dindTmp    bool
		// timeoutEnv is the exact entrypoint timeout env that only
		// dind-runner has. standard keeps it empty.
		timeoutEnv []string
	}{
		{name: "standard/runsc", profile: config.ProfileStandard, runtime: "runsc", user: "runner"},
		{name: "standard/runc", profile: config.ProfileStandard, runtime: "runc", user: "runner"},
		{name: "dind/runsc", profile: config.ProfileDindRunner, runtime: "runsc", user: "0:0",
			entrypoint: []string{"/usr/local/bin/gha-dind-entrypoint"}, capAdd: config.DindCapabilities(), dindTmp: true,
			timeoutEnv: []string{"PROVISIONING_TIMEOUT_SECONDS=300", "STOP_TIMEOUT_SECONDS=30"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t, tc.profile, tc.runtime)
			spec := mustBuild(t, cfg)
			input := testInput(cfg)

			if spec.profile != tc.profile || spec.runtime != tc.runtime {
				t.Fatalf("profile/runtime is invalid: profile=%q runtime=%q", spec.profile, spec.runtime)
			}
			if spec.create.Name != input.ContainerName {
				t.Fatalf("container name is invalid: %q", spec.create.Name)
			}

			// Common container.Config contract
			cc := spec.create.Config
			if cc.Image != cfg.Runner.Image {
				t.Fatalf("image is invalid: %q", cc.Image)
			}
			if cc.User != tc.user {
				t.Fatalf("user is invalid: %q", cc.User)
			}
			if cc.WorkingDir != "/home/runner" {
				t.Fatalf("working dir is invalid: %q", cc.WorkingDir)
			}
			if len(cc.Cmd) != 1 || cc.Cmd[0] != "/home/runner/run.sh" {
				t.Fatalf("command is invalid: %v", cc.Cmd)
			}
			if !slices.Equal(cc.Entrypoint, tc.entrypoint) {
				t.Fatalf("entrypoint is invalid: %v", cc.Entrypoint)
			}
			if cc.StopSignal != "SIGTERM" {
				t.Fatalf("stop signal is invalid: %q", cc.StopSignal)
			}
			// StopTimeout is the second ceiling of stopTimeoutSeconds, the
			// same as cleanup; the default 30s stays 30. The boundary values
			// are checked in TestBuildManagedSpec_StopTimeout.
			if cc.StopTimeout == nil || *cc.StopTimeout != 30 {
				t.Fatalf("stop timeout is invalid: %v", cc.StopTimeout)
			}
			if cc.Tty || cc.OpenStdin || cc.StdinOnce {
				t.Fatal("tty/stdin must be disabled")
			}

			// The three JIT env values (dind-runner appends the
			// profile-specific timeout env exactly at the end; the ceiling
			// seconds of the testConfig defaults 5m/30s go in as-is)
			wantEnv := []string{
				"ACTIONS_RUNNER_INPUT_JITCONFIG=" + input.JITConfig,
				"ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE=1",
				"GITHUB_ACTIONS_RUNNER_EXTRA_USER_AGENT=gha-docker-controller/" + input.UserAgentVersion,
			}
			wantEnv = append(wantEnv, tc.timeoutEnv...)
			if !slices.Equal(cc.Env, wantEnv) {
				t.Fatalf("JIT env is invalid: %v", cc.Env)
			}

			// Six labels
			if err := model.ValidateLabels(spec.labels, input.Identity); err != nil {
				t.Fatalf("label does not satisfy the managed contract: %v", err)
			}
			if len(cc.Labels) != 6 {
				t.Fatalf("container label count is not 6: %v", cc.Labels)
			}
			for _, k := range model.RequiredLabelKeys() {
				if cc.Labels[k] == "" {
					t.Fatalf("required label %q is missing: %v", k, cc.Labels)
				}
			}

			// Common HostConfig security fields
			hc := spec.create.HostConfig
			if hc.Runtime != tc.runtime {
				t.Fatalf("runtime is invalid: %q", hc.Runtime)
			}
			if hc.Privileged {
				t.Fatal("privileged must be false")
			}
			if hc.ReadonlyRootfs {
				t.Fatal("read-only rootfs must be false")
			}
			if len(hc.CapDrop) != 1 || hc.CapDrop[0] != "ALL" {
				t.Fatalf("cap drop is invalid: %v", hc.CapDrop)
			}
			if !slices.Equal(hc.CapAdd, tc.capAdd) {
				t.Fatalf("cap add is invalid: %v", hc.CapAdd)
			}
			if hc.NetworkMode != "bridge" {
				t.Fatalf("network mode is invalid: %q", hc.NetworkMode)
			}
			if hc.IpcMode != container.IPCModePrivate || hc.CgroupnsMode != container.CgroupnsModePrivate {
				t.Fatalf("namespace isolation is invalid: ipc=%q cgroupns=%q", hc.IpcMode, hc.CgroupnsMode)
			}
			if hc.PidMode != "" || hc.UTSMode != "" || hc.UsernsMode != "" {
				t.Fatalf("host namespace is configured: pid=%q uts=%q userns=%q", hc.PidMode, hc.UTSMode, hc.UsernsMode)
			}
			if hc.AutoRemove {
				t.Fatal("auto remove must be false")
			}
			if hc.RestartPolicy.Name != container.RestartPolicyDisabled {
				t.Fatalf("restart policy is invalid: %q", hc.RestartPolicy.Name)
			}
			if hc.Init == nil || !*hc.Init {
				t.Fatal("init must be enabled")
			}
			if len(hc.Binds) != 0 || len(hc.VolumesFrom) != 0 || len(hc.Devices) != 0 || len(hc.DeviceRequests) != 0 {
				t.Fatalf("prohibited mount/device is configured: binds=%v devices=%v", hc.Binds, hc.Devices)
			}
			if !slices.Contains(hc.SecurityOpt, "no-new-privileges") {
				t.Fatalf("no-new-privileges is missing from security options: %v", hc.SecurityOpt)
			}

			// Resources
			res := &hc.Resources
			if res.NanoCPUs != int64(cfg.Runner.CPU) {
				t.Fatalf("NanoCPUs is invalid: %d", res.NanoCPUs)
			}
			if res.Memory != int64(cfg.Runner.Memory) || res.MemorySwap != int64(cfg.Runner.MemorySwap) {
				t.Fatalf("memory is invalid: memory=%d swap=%d", res.Memory, res.MemorySwap)
			}
			if res.PidsLimit == nil || *res.PidsLimit != cfg.Runner.PidsLimit {
				t.Fatalf("pids limit is invalid: %v", res.PidsLimit)
			}
			if len(res.Ulimits) != 1 || res.Ulimits[0].Name != "nofile" || res.Ulimits[0].Soft != 1024 || res.Ulimits[0].Hard != 2048 {
				t.Fatalf("ulimit is invalid: %+v", res.Ulimits)
			}

			// tmpfs / DNS / extraHosts
			if got := hc.Tmpfs["/tmp"]; got != "size=64m,ro" {
				t.Fatalf("tmpfs is invalid: %v", hc.Tmpfs)
			}
			if len(hc.DNS) != 1 || hc.DNS[0] != netip.MustParseAddr("1.1.1.1") {
				t.Fatalf("DNS is invalid: %v", hc.DNS)
			}
			if len(hc.ExtraHosts) != 1 || hc.ExtraHosts[0] != "db:127.0.0.1" {
				t.Fatalf("extraHosts is invalid: %v", hc.ExtraHosts)
			}

			// Profile delta: the fixed dind /var/lib/docker tmpfs (mode
			// 0700, storageSize). standard has no mounts.
			if tc.dindTmp {
				if len(hc.Mounts) != 1 {
					t.Fatalf("dind mount count must be 1: %v", hc.Mounts)
				}
				m := hc.Mounts[0]
				if m.Type != mount.TypeTmpfs || m.Target != "/var/lib/docker" || m.Source != "" {
					t.Fatalf("dind tmpfs mount is invalid: %+v", m)
				}
				if m.TmpfsOptions == nil || m.TmpfsOptions.SizeBytes != int64(cfg.DindRunner.StorageSize) {
					t.Fatalf("dind tmpfs size is invalid: %+v", m.TmpfsOptions)
				}
				if m.TmpfsOptions.Mode != 0o700 {
					t.Fatalf("dind tmpfs mode must be 0700: %o", m.TmpfsOptions.Mode)
				}
			} else if len(hc.Mounts) != 0 {
				t.Fatalf("standard has mounts: %v", hc.Mounts)
			}
		})
	}
}

// TestBuildManagedSpec_UnlimitedResources maps zero resource values to Docker
// unlimited values, including a nil process limit.
func TestBuildManagedSpec_UnlimitedResources(t *testing.T) {
	cfg := testConfig(t, config.ProfileStandard, "runsc")
	cfg.Runner.CPU = 0
	cfg.Runner.Memory = 0
	cfg.Runner.MemorySwap = 0
	cfg.Runner.PidsLimit = 0
	spec := mustBuild(t, cfg)
	res := spec.create.HostConfig.Resources
	if res.NanoCPUs != 0 || res.Memory != 0 || res.MemorySwap != 0 || res.PidsLimit != nil {
		t.Fatalf("unlimited resource mapping is incorrect: cpu=%d memory=%d swap=%d pids=%v", res.NanoCPUs, res.Memory, res.MemorySwap, res.PidsLimit)
	}
}

// TestBuildManagedSpec_Prohibited is the table test of the prohibited
// settings the builder must not produce. It covers the dind/runc mismatch
// and every shared prohibition.
func TestBuildManagedSpec_Prohibited(t *testing.T) {
	cases := []struct {
		name        string
		profile     string
		runtime     string
		mutate      func(*config.Config)
		inputMutate func(*ManagedSpecInput)
		wantErr     string
	}{
		{name: "dind は runc を拒否", profile: config.ProfileDindRunner, runtime: "runc", wantErr: "not allowed for dind-runner profile"},
		{name: "dind の CapAdd に 17 個外", profile: config.ProfileDindRunner, mutate: func(c *config.Config) { c.Runner.CapAdd = []string{"SYS_TIME"} }, wantErr: "not in the dind-runner allowed set"},
		{name: "standard の CapAdd を拒否", profile: config.ProfileStandard, mutate: func(c *config.Config) { c.Runner.CapAdd = []string{"CHOWN"} }, wantErr: "must be empty for standard profile"},
		{name: "CapDrop は ALL 以外を拒否", mutate: func(c *config.Config) { c.Runner.CapDrop = []string{"SYS_ADMIN"} }, wantErr: "capDrop must be exactly"},
		{name: "read-only rootfs を拒否", mutate: func(c *config.Config) { c.Runner.ReadOnlyRootfs = true }, wantErr: "readOnlyRootfs must be false"},
		{name: "no-new-privileges false を拒否", mutate: func(c *config.Config) { c.Runner.NoNewPrivileges = false }, wantErr: "noNewPrivileges must be true"},
		{name: "negative cpu を拒否", mutate: func(c *config.Config) { c.Runner.CPU = -1 }, wantErr: "runner.cpu must be non-negative"},
		{name: "negative memory を拒否", mutate: func(c *config.Config) { c.Runner.Memory = -1 }, wantErr: "runner.memory must be non-negative"},
		{name: "negative pidsLimit を拒否", mutate: func(c *config.Config) { c.Runner.PidsLimit = -1 }, wantErr: "runner.pidsLimit must be non-negative"},
		{name: "memorySwap < memory を拒否", mutate: func(c *config.Config) { c.Runner.MemorySwap = c.Runner.Memory - 1 }, wantErr: "memorySwap must be >= runner.memory"},
		{name: "host network を拒否", mutate: func(c *config.Config) { c.Runner.Network = "host" }, wantErr: `network mode "host"`},
		{name: "none network を拒否", mutate: func(c *config.Config) { c.Runner.Network = "none" }, wantErr: `network mode "none"`},
		{name: "container network を拒否", mutate: func(c *config.Config) { c.Runner.Network = "container" }, wantErr: `network mode "container"`},
		{name: "container:id network を拒否", mutate: func(c *config.Config) { c.Runner.Network = "container:other" }, wantErr: "not allowed"},
		{name: "seccomp unconfined を拒否", mutate: func(c *config.Config) { c.Runner.Seccomp = "unconfined" }, wantErr: "unconfined"},
		{name: "apparmor unconfined を拒否", mutate: func(c *config.Config) { c.Runner.AppArmor = "unconfined" }, wantErr: "unconfined"},
		{name: "invalid runtime 名を拒否", runtime: "run sc", wantErr: "not a valid runtime name"},
		{name: "dind の /var/lib/docker tmpfs 重複を拒否", profile: config.ProfileDindRunner, mutate: func(c *config.Config) { c.Runner.Tmpfs = []string{"/var/lib/docker:1g"} }, wantErr: "reserved for the dind-runner profile"},
		{name: "JIT config 空を拒否", inputMutate: func(in *ManagedSpecInput) { in.JITConfig = "" }, wantErr: "JIT config is empty"},
		{name: "runner ID 非正を拒否", inputMutate: func(in *ManagedSpecInput) { in.Identity.RunnerID = 0 }, wantErr: "positive scale set id and runner id"},
		{name: "runner name 非 canonical を拒否", inputMutate: func(in *ManagedSpecInput) { in.Identity.RunnerName = "evil" }, wantErr: "not a valid canonical runner name"},
		{name: "controller instance 空を拒否", inputMutate: func(in *ManagedSpecInput) { in.ControllerInstance = "" }, wantErr: "controller instance is empty"},
		{name: "created at zero を拒否", inputMutate: func(in *ManagedSpecInput) { in.CreatedAt = time.Time{} }, wantErr: "created at is zero"},
		{name: "container name 空を拒否", inputMutate: func(in *ManagedSpecInput) { in.ContainerName = "" }, wantErr: "container name is empty"},
		{name: "user agent version 空を拒否", inputMutate: func(in *ManagedSpecInput) { in.UserAgentVersion = "" }, wantErr: "user agent version is empty"},
		{name: "config nil を拒否", inputMutate: func(in *ManagedSpecInput) { in.Config = nil }, wantErr: "config is nil"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			profile := tc.profile
			if profile == "" {
				profile = config.ProfileStandard
			}
			runtime := tc.runtime
			if runtime == "" {
				runtime = "runsc"
			}
			cfg := testConfig(t, profile, runtime)
			if tc.mutate != nil {
				tc.mutate(cfg)
			}
			input := testInput(cfg)
			if tc.inputMutate != nil {
				tc.inputMutate(&input)
			}
			_, err := BuildManagedSpec(input)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("BuildManagedSpec did not reject: err=%v", err)
			}
		})
	}
}

// TestBuildManagedSpec_SecurityOpt verifies the SecurityOpt generation for
// seccomp/AppArmor with real files. The seccomp JSON file is compacted and
// passed as "seccomp=<compact JSON>"; invalid JSON and a missing file are
// rejected. AppArmor appends "apparmor=<name>" when configured and never
// loses the required no-new-privileges. Both together coexist.
func TestBuildManagedSpec_SecurityOpt(t *testing.T) {
	dir := t.TempDir()
	seccomp := filepath.Join(dir, "seccomp.json")
	if err := os.WriteFile(seccomp, []byte("{\n  \"defaultAction\": \"SCMP_ACT_ERRNO\"\n}\n"), 0o600); err != nil {
		t.Fatalf("seccomp failed to create file: %v", err)
	}

	t.Run("seccomp は compact 化して渡す", func(t *testing.T) {
		cfg := testConfig(t, config.ProfileStandard, "runsc")
		cfg.Runner.Seccomp = seccomp
		spec := mustBuild(t, cfg)
		want := `seccomp={"defaultAction":"SCMP_ACT_ERRNO"}`
		if !slices.Contains(spec.create.HostConfig.SecurityOpt, want) {
			t.Fatalf("seccomp option %q is missing: %v", want, spec.create.HostConfig.SecurityOpt)
		}
	})
	t.Run("seccomp invalid JSON を拒否", func(t *testing.T) {
		bad := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
			t.Fatalf("seccomp failed to create file: %v", err)
		}
		cfg := testConfig(t, config.ProfileStandard, "runsc")
		cfg.Runner.Seccomp = bad
		if _, err := BuildManagedSpec(testInput(cfg)); err == nil || !strings.Contains(err.Error(), "not valid JSON") {
			t.Fatalf("invalid JSON was not rejected: err=%v", err)
		}
	})
	t.Run("seccomp file 不存在を拒否", func(t *testing.T) {
		cfg := testConfig(t, config.ProfileStandard, "runsc")
		cfg.Runner.Seccomp = filepath.Join(dir, "missing.json")
		if _, err := BuildManagedSpec(testInput(cfg)); err == nil || !strings.Contains(err.Error(), "read seccomp profile") {
			t.Fatalf("missing file was not rejected: err=%v", err)
		}
	})
	t.Run("apparmor 指定で必須値を維持", func(t *testing.T) {
		cfg := testConfig(t, config.ProfileStandard, "runsc")
		cfg.Runner.AppArmor = "ghadc-runner"
		spec := mustBuild(t, cfg)
		opt := spec.create.HostConfig.SecurityOpt
		if !slices.Contains(opt, "no-new-privileges") {
			t.Fatalf("no-new-privileges is missing: %v", opt)
		}
		if !slices.Contains(opt, "apparmor=ghadc-runner") {
			t.Fatalf("apparmor option is missing from SecurityOpt: %v", opt)
		}
	})
	t.Run("seccomp と apparmor の同時指定", func(t *testing.T) {
		cfg := testConfig(t, config.ProfileStandard, "runsc")
		cfg.Runner.Seccomp = seccomp
		cfg.Runner.AppArmor = "ghadc-runner"
		spec := mustBuild(t, cfg)
		opt := spec.create.HostConfig.SecurityOpt
		for _, want := range []string{
			"no-new-privileges",
			`seccomp={"defaultAction":"SCMP_ACT_ERRNO"}`,
			"apparmor=ghadc-runner",
		} {
			if !slices.Contains(opt, want) {
				t.Fatalf("security option %q is missing: %v", want, opt)
			}
		}
	})
}

// TestBuildManagedSpec_StopTimeout verifies that Config.StopTimeout is the
// second ceiling of stopTimeoutSeconds. Sub-second values never round down
// to 0; a positive setting is always at least 1 second.
func TestBuildManagedSpec_StopTimeout(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want int
	}{
		{name: "1ns は 1 秒へ切り上げ", in: time.Nanosecond, want: 1},
		{name: "1ms は 1 秒へ切り上げ", in: time.Millisecond, want: 1},
		{name: "999ms は 1 秒へ切り上げ", in: 999 * time.Millisecond, want: 1},
		{name: "500ms は 1 秒へ切り上げ", in: 500 * time.Millisecond, want: 1},
		{name: "1s は 1 秒", in: time.Second, want: 1},
		{name: "1500ms は 2 秒へ切り上げ", in: 1500 * time.Millisecond, want: 2},
		{name: "30s は 30 秒", in: 30 * time.Second, want: 30},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t, config.ProfileStandard, "runsc")
			cfg.Runner.StopTimeout = config.Duration(tc.in)
			spec := mustBuild(t, cfg)
			cc := spec.create.Config
			if cc.StopTimeout == nil || *cc.StopTimeout != tc.want {
				t.Fatalf("StopTimeout conversion is invalid: input=%s, actual=%v, expected=%d", tc.in, cc.StopTimeout, tc.want)
			}
		})
	}
}

// TestParseTmpfsSpec verifies the conversion rules for Docker CLI compatible
// tmpfs specs. It uses the same decision as config validateTmpfs (comma
// presence and size value); the size is normalized to size=.
func TestParseTmpfsSpec(t *testing.T) {
	cases := []struct {
		name    string
		spec    string
		dest    string
		options string
		wantErr string
	}{
		{name: "dest only", spec: "/tmp", dest: "/tmp", options: ""},
		{name: "size", spec: "/tmp:1g", dest: "/tmp", options: "size=1g"},
		{name: "size and options", spec: "/tmp:1g:ro", dest: "/tmp", options: "size=1g,ro"},
		{name: "options with size=", spec: "/tmp:size=1g,ro", dest: "/tmp", options: "size=1g,ro"},
		{name: "options only", spec: "/tmp:ro", dest: "/tmp", options: "ro"},
		{name: "decimal size", spec: "/tmp:2.5g:noexec", dest: "/tmp", options: "size=2.5g,noexec"},
		{name: "byte size", spec: "/tmp:2048", dest: "/tmp", options: "size=2048"},
		{name: "MiB size lowercased", spec: "/tmp:512MiB", dest: "/tmp", options: "size=512mib"},
		{name: "comma options not size", spec: "/tmp:1g,ro", dest: "/tmp", options: "1g,ro"},
		{name: "relative dest", spec: "tmp:1g", wantErr: "absolute path"},
		{name: "empty dest", spec: ":1g", wantErr: "absolute path"},
		{name: "too many parts", spec: "/tmp:1g:ro:x", wantErr: "invalid tmpfs spec"},
		{name: "size after options", spec: "/tmp:ro:1g", wantErr: "size must come before options"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dest, options, err := parseTmpfsSpec(tc.spec)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseTmpfsSpec did not reject: err=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTmpfsSpec failed: %v", err)
			}
			if dest != tc.dest || options != tc.options {
				t.Fatalf("conversion result is invalid: dest=%q options=%q", dest, options)
			}
		})
	}
}

// TestBuildManagedSpec_DefensiveCopy verifies the defensive copies. Changing
// the input config maps/slices after the build does not change the spec
// internals, and the maps inside the spec are not shared with each other.
// Together with the CreateManaged re-validation this is the basis of the
// immutable spec.
func TestBuildManagedSpec_DefensiveCopy(t *testing.T) {
	cfg := testConfig(t, config.ProfileStandard, "runsc")
	spec := mustBuild(t, cfg)

	// Mutate the input config maps/slices afterwards
	cfg.Runner.CapDrop[0] = "SYS_ADMIN"
	cfg.Runner.CapAdd = append(cfg.Runner.CapAdd, "CHOWN")
	cfg.Runner.DNS[0] = "8.8.8.8"
	cfg.Runner.ExtraHosts[0] = "evil:10.0.0.1"
	cfg.Runner.Ulimit[0].Soft = 1
	cfg.Runner.Tmpfs[0] = "/changed:1g"
	cfg.Runner.Image = "changed:tag"

	if got := spec.create.HostConfig.CapDrop[0]; got != "ALL" {
		t.Fatalf("CapDrop changed when the input changed: %q", got)
	}
	if len(spec.create.HostConfig.CapAdd) != 0 {
		t.Fatalf("CapAdd changed when the input changed: %v", spec.create.HostConfig.CapAdd)
	}
	if got := spec.create.HostConfig.DNS[0]; got != netip.MustParseAddr("1.1.1.1") {
		t.Fatalf("DNS changed when the input changed: %v", got)
	}
	if got := spec.create.HostConfig.ExtraHosts[0]; got != "db:127.0.0.1" {
		t.Fatalf("ExtraHosts changed when the input changed: %q", got)
	}
	if got := spec.create.HostConfig.Resources.Ulimits[0].Soft; got != 1024 {
		t.Fatalf("ulimit changed when the input changed: %d", got)
	}
	if got := spec.create.HostConfig.Tmpfs["/tmp"]; got != "size=64m,ro" {
		t.Fatalf("tmpfs changed when the input changed: %v", spec.create.HostConfig.Tmpfs)
	}
	if spec.create.Config.Image != testImage {
		t.Fatalf("image changed when the input changed: %q", spec.create.Config.Image)
	}

	// The label maps inside the spec are not shared with each other either
	spec.labels[model.ManagedLabelKey] = "tampered"
	if got := spec.create.Config.Labels[model.ManagedLabelKey]; got != model.ManagedLabelValue {
		t.Fatalf("container label shares spec.labels: %q", got)
	}
}
