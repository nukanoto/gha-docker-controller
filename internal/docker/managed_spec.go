// managed_spec.go implements the immutable managed-create spec type, the
// input contract, and the builder. A raw mobyclient.ContainerCreateOptions
// could assemble arbitrary container.Config / HostConfig values, so
// privileged mode, host namespaces, socket/device/bind mounts and
// runtime/profile mismatches could be built from the outside; the type
// cannot enforce the prohibitions. Instead, ManagedSpec has only unexported
// fields, BuildManagedSpec in this package produces it, and only
// Client.CreateManaged can accept it. The create options, labels, env, maps
// and slices are never returned to outside packages, and the internally
// stored values are defensive copies. Every security field is checked both
// by this builder and by the CreateManaged re-validation; it does not rely
// on the static config validation in internal/config.
package docker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/docker/go-units"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	mobyclient "github.com/moby/moby/client"

	"github.com/nukanoto/gha-docker-controller/internal/config"
	"github.com/nukanoto/gha-docker-controller/internal/model"
)

// Fixed values for the standard profile.
const (
	// standardUser is the user the standard profile runs as.
	standardUser = "runner"
	// runnerWorkDir is the runner working directory.
	runnerWorkDir = "/home/runner"
	// runnerCommand is the effective runner command for both profiles.
	runnerCommand = "/home/runner/run.sh"
)

// Fixed values for the dind-runner profile.
const (
	// dindUser is the user the outer container runs as. The entrypoint
	// starts dockerd as root and then drops privileges to runner with
	// setpriv.
	dindUser = "0:0"
	// dindEntrypoint is the supervisor of the project dind image.
	dindEntrypoint = "/usr/local/bin/gha-dind-entrypoint"
	// dindRunnerDataDir is the inner dockerd data directory, mounted as a
	// tmpfs that is never reused between runners.
	dindRunnerDataDir = "/var/lib/docker"
	// dindRuntime is the literal runtime name fixed for dind-runner.
	dindRuntime = "runsc"
)

// JIT env contract. The official runner requires exactly these three env
// values. The JIT config value is an opaque secret and must never appear in
// logs or errors.
const (
	jitEnvKey          = "ACTIONS_RUNNER_INPUT_JITCONFIG"
	returnEnvKey       = "ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE"
	userAgentEnvPrefix = "GITHUB_ACTIONS_RUNNER_EXTRA_USER_AGENT"
)

// managedProfile carries only the create fields that differ per profile
// between the builders. The common security fields are assembled in one
// place by BuildManagedSpec, so standard and dind-runner do not duplicate
// them.
type managedProfile struct {
	user       string
	entrypoint []string
	runtime    string
	mounts     []mount.Mount
	// env is the profile-specific extra env. Only dind-runner sets the
	// timeout seconds for the entrypoint; standard keeps it nil.
	env []string
}

// buildProfile selects the fixed builder for the given profile.
func buildProfile(cfg *config.Config) managedProfile {
	if cfg.Runner.Profile == config.ProfileDindRunner {
		return buildDindProfile(cfg)
	}
	return buildStandardProfile(cfg)
}

// buildStandardProfile builds the standard-profile delta fields. standard
// sets the configured docker.runtime value into HostConfig.Runtime;
// dind-runner sets the literal "runsc".
func buildStandardProfile(cfg *config.Config) managedProfile {
	return managedProfile{
		user:    standardUser,
		runtime: cfg.Docker.Runtime,
	}
}

// buildDindProfile builds the dind-runner profile delta fields. The
// runtime is fixed to the literal runsc, and the inner dockerd data
// directory is mounted as a tmpfs (mode 0700, storageSize). Volumes and
// state are never reused. The container env gets the provisioning/stop
// timeouts read by the entrypoint as the ceiling seconds of the positive
// config durations (not set for standard).
func buildDindProfile(cfg *config.Config) managedProfile {
	return managedProfile{
		user:       dindUser,
		entrypoint: []string{dindEntrypoint},
		runtime:    dindRuntime,
		mounts: []mount.Mount{{
			Type:   mount.TypeTmpfs,
			Target: dindRunnerDataDir,
			TmpfsOptions: &mount.TmpfsOptions{
				SizeBytes: int64(cfg.DindRunner.StorageSize),
				Mode:      0o700,
			},
		}},
		env: []string{
			"PROVISIONING_TIMEOUT_SECONDS=" + strconv.Itoa(stopTimeoutSeconds(time.Duration(cfg.Runner.ProvisioningTimeout))),
			"STOP_TIMEOUT_SECONDS=" + strconv.Itoa(stopTimeoutSeconds(time.Duration(cfg.Runner.StopTimeout))),
		},
	}
}

// ManagedSpec is the immutable managed container creation instruction. All
// fields are unexported, so an outside package can only receive the output
// of BuildManagedSpec or pass a zero value; it is structurally impossible
// to assemble raw create options and pass them. CreateManaged re-validates
// with validateManagedSpec on every call, so zero, tampered and incomplete
// specs are rejected. It is a value type and shares no input maps or
// slices.
type ManagedSpec struct {
	// create is the Docker SDK create options. It is unexported, so outside
	// packages can neither read nor change it.
	create mobyclient.ContainerCreateOptions
	// profile is the runner.profile value. It backs the runtime/profile
	// mismatch re-validation in CreateManaged.
	profile string
	// runtime is the value set into HostConfig.Runtime. It backs the
	// runtime/profile contract re-validation.
	runtime string
	// identity is the runner identity used for label validation.
	identity model.RunnerIdentity
	// labels is a defensive copy of the six labels.
	labels map[string]string
}

// ManagedSpecInput is the minimal typed input for BuildManagedSpec. It is
// limited to validated runtime config, the GitHub runner identity, the
// opaque JIT config and label auxiliary values; raw Docker SDK types are not
// accepted.
type ManagedSpecInput struct {
	// Config is the validated runtime config. nil is rejected.
	Config *config.Config
	// Identity is the GitHub runner identity. ScaleSetID and RunnerID must be
	// positive, and RunnerName must be canonical.
	Identity model.RunnerIdentity
	// JITConfig is the opaque encoded JIT config. It must never appear in
	// logs or errors. Empty is rejected.
	JITConfig string
	// ControllerInstance is the value of the controller-instance label.
	ControllerInstance string
	// CreatedAt is the value of the created-at label.
	CreatedAt time.Time
	// ContainerName is the container name. It is auxiliary information, not
	// the identity source of truth; the caller generates it with
	// model.ContainerName.
	ContainerName string
	// UserAgentVersion is the controller build version embedded in
	// GITHUB_ACTIONS_RUNNER_EXTRA_USER_AGENT. buildinfo supplies it.
	UserAgentVersion string
}

// BuildManagedSpec deterministically assembles the container.Config and
// HostConfig for standard /dind-runner and returns an immutable
// ManagedSpec. standard sets the configured docker.runtime value into
// HostConfig.Runtime; dind-runner sets the literal "runsc". When seccomp
// is configured, the JSON file is read, compacted and passed as
// "seccomp=<compact JSON>" in SecurityOpt. No input map or slice is shared;
// everything is defensive-copied into the spec.
func BuildManagedSpec(input ManagedSpecInput) (ManagedSpec, error) {
	if err := validateSpecInput(input.Config, input); err != nil {
		return ManagedSpec{}, fmt.Errorf("build managed spec: %w", err)
	}
	cfg := input.Config

	// The six managed labels. BuildLabels returns a fresh map each call, so
	// nothing is shared.
	labels := model.BuildLabels(input.Identity, input.ControllerInstance, input.CreatedAt)

	// Profile-specific values are delegated to the builders.
	profile := buildProfile(cfg)

	// The three JIT env values. The JIT config value is a secret, so it
	// never goes into errors or logs. dind-runner appends the
	// entrypoint timeout env added by the profile at the end.
	env := []string{
		jitEnvKey + "=" + input.JITConfig,
		returnEnvKey + "=1",
		userAgentEnvPrefix + "=gha-docker-controller/" + input.UserAgentVersion,
	}
	env = append(env, profile.env...)

	// SecurityOpt always includes no-new-privileges; seccomp/AppArmor are
	// added only when configured. unconfined was already rejected by
	// validateSpecInput.
	securityOpt := []string{"no-new-privileges"}
	if cfg.Runner.Seccomp != "" {
		compact, err := readSeccompProfile(cfg.Runner.Seccomp)
		if err != nil {
			return ManagedSpec{}, fmt.Errorf("build managed spec: %w", err)
		}
		securityOpt = append(securityOpt, "seccomp="+compact)
	}
	if cfg.Runner.AppArmor != "" {
		securityOpt = append(securityOpt, "apparmor="+cfg.Runner.AppArmor)
	}

	tmpfs, err := buildTmpfs(cfg.Runner.Tmpfs)
	if err != nil {
		return ManagedSpec{}, fmt.Errorf("build managed spec: %w", err)
	}
	dns, err := buildDNS(cfg.Runner.DNS)
	if err != nil {
		return ManagedSpec{}, fmt.Errorf("build managed spec: %w", err)
	}

	mounts := profile.mounts

	// Config.StopTimeout uses the same second-ceiling stopTimeoutSeconds as
	// the cleanup Docker stop API. Sub-second values never round down to 0;
	// a positive setting is always at least 1 second.
	stopTimeout := stopTimeoutSeconds(time.Duration(cfg.Runner.StopTimeout))
	var pidsLimit *int64 = &cfg.Runner.PidsLimit
	if *pidsLimit <= 0 {
		pidsLimit = nil
	}
	initEnabled := true

	spec := ManagedSpec{
		profile:  cfg.Runner.Profile,
		runtime:  profile.runtime,
		identity: input.Identity,
		labels:   maps.Clone(labels),
		create: mobyclient.ContainerCreateOptions{
			Name: input.ContainerName,
			Config: &container.Config{
				Image:       cfg.Runner.Image,
				User:        profile.user,
				WorkingDir:  runnerWorkDir,
				Cmd:         []string{runnerCommand},
				Entrypoint:  profile.entrypoint,
				Env:         env,
				Labels:      labels,
				StopSignal:  "SIGTERM",
				StopTimeout: &stopTimeout,
			},
			HostConfig: &container.HostConfig{
				NetworkMode:    container.NetworkMode(cfg.Runner.Network),
				RestartPolicy:  container.RestartPolicy{Name: container.RestartPolicyDisabled},
				AutoRemove:     false,
				CapAdd:         slices.Clone(cfg.Runner.CapAdd),
				CapDrop:        slices.Clone(cfg.Runner.CapDrop),
				CgroupnsMode:   container.CgroupnsModePrivate,
				DNS:            dns,
				ExtraHosts:     slices.Clone(cfg.Runner.ExtraHosts),
				IpcMode:        container.IPCModePrivate,
				Privileged:     false,
				ReadonlyRootfs: false,
				Runtime:        profile.runtime,
				SecurityOpt:    securityOpt,
				Tmpfs:          tmpfs,
				// Docker interprets zero CPU, memory and swap as unlimited. A nil
				// PidsLimit leaves the process count unlimited.
				Resources: container.Resources{
					NanoCPUs:   int64(cfg.Runner.CPU),
					Memory:     int64(cfg.Runner.Memory),
					MemorySwap: int64(cfg.Runner.MemorySwap),
					PidsLimit:  pidsLimit,
					Ulimits:    buildUlimits(cfg.Runner.Ulimit),
				},
				Mounts: mounts,
				Init:   &initEnabled,
			},
		},
	}
	return spec, nil
}

// readSeccompProfile reads a seccomp JSON file and returns its compacted
// JSON. It fixes the contract: "the controller reads the JSON file and
// passes seccomp=<compact JSON>". json.Compact validates the JSON grammar,
// so an invalid file fails.
func readSeccompProfile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read seccomp profile %q: %w", path, err)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return "", fmt.Errorf("seccomp profile %q is not valid JSON: %w", path, err)
	}
	return buf.String(), nil
}

// buildDNS converts a DNS server list to Moby v29 []netip.Addr. The config
// is assumed to be validated, but an unconvertible value fails with an
// error.
func buildDNS(ips []string) ([]netip.Addr, error) {
	if len(ips) == 0 {
		return nil, nil
	}
	addrs := make([]netip.Addr, 0, len(ips))
	for _, ip := range ips {
		a, err := netip.ParseAddr(ip)
		if err != nil {
			return nil, fmt.Errorf("invalid DNS IP %q", ip)
		}
		addrs = append(addrs, a)
	}
	return addrs, nil
}

// buildTmpfs converts a Docker CLI compatible tmpfs list
// (dest[:size[:options]]) into HostConfig.Tmpfs (dest -> options). The size
// is normalized to "size=<value>". A duplicate destination is rejected as a
// config error.
func buildTmpfs(specs []string) (map[string]string, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	m := make(map[string]string, len(specs))
	for _, spec := range specs {
		dest, options, err := parseTmpfsSpec(spec)
		if err != nil {
			return nil, err
		}
		if _, dup := m[dest]; dup {
			return nil, fmt.Errorf("duplicate tmpfs destination %q", dest)
		}
		m[dest] = options
	}
	return m, nil
}

// parseTmpfsSpec splits "dest[:size[:options]]" into dest and options. It
// decides size vs options with the same rule as config validateTmpfs: when
// the second element is a size value (a positive memory value without a
// comma) it is normalized to size=, otherwise it is treated as options.
func parseTmpfsSpec(spec string) (dest, options string, err error) {
	parts := strings.Split(spec, ":")
	if len(parts) > 3 {
		return "", "", fmt.Errorf("invalid tmpfs spec %q", spec)
	}
	dest = parts[0]
	if dest == "" || !strings.HasPrefix(dest, "/") {
		return "", "", fmt.Errorf("tmpfs destination must be an absolute path: %q", dest)
	}
	if len(parts) >= 2 && parts[1] != "" {
		if size, ok := memorySizeValue(parts[1]); ok && !strings.Contains(parts[1], ",") {
			// "dest:size[:options]" — the size is normalized to size=, which
			// the daemon understands.
			options = "size=" + size
			if len(parts) == 3 && parts[2] != "" {
				options += "," + parts[2]
			}
		} else if len(parts) == 2 {
			options = parts[1]
		} else {
			return "", "", fmt.Errorf("invalid tmpfs spec %q: size must come before options", spec)
		}
	}
	return dest, options, nil
}

// memorySizeValue decides whether a string is a memory value using the same
// units.RAMInBytes acceptance rule as config.parseMemory, and returns the
// normalized lowercase value. Unlimited values such as 0 are not accepted
// as a size. It is a local helper kept consistent with config
// validateTmpfs; if the decisions diverge, the size/options interpretation
// would differ from config.
func memorySizeValue(s string) (string, bool) {
	lower := strings.TrimSpace(strings.ToLower(s))
	v, err := units.RAMInBytes(lower)
	if err != nil || v <= 0 {
		return "", false
	}
	return lower, true
}

// buildUlimits converts the config ulimit list into value-copied
// []*container.Ulimit. The pointers are not shared with the input slice.
func buildUlimits(ul []config.Ulimit) []*container.Ulimit {
	if len(ul) == 0 {
		return nil
	}
	out := make([]*container.Ulimit, 0, len(ul))
	for _, u := range ul {
		v := container.Ulimit{Name: u.Name, Soft: u.Soft, Hard: u.Hard}
		out = append(out, &v)
	}
	return out
}
