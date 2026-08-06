package docker

import (
	"fmt"
	"slices"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"

	"github.com/nukanoto/gha-docker-controller/internal/config"
	"github.com/nukanoto/gha-docker-controller/internal/model"
)

// validateManagedSpec is the security invariant re-validation that
// CreateManaged runs on every call. It rejects zero, tampered and
// incomplete specs. The checks directly encode the following contract:
//   - zero value / incomplete spec (nil Config/HostConfig, identity, name,
//     image)
//   - profile and runtime consistency (dind-runner: literal runsc;
//     standard: a valid runtime name)
//   - positive resources (CPU, memory, swap>=memory, pids)
//   - host namespaces (PID/UTS/Userns) forbidden; IPC/cgroup private; host
//     and container networks forbidden
//   - privileged=false; no socket/device/bind mounts
//   - six labels and the JIT env
//   - exact per-profile contract for User/Entrypoint/Cmd/CapDrop/CapAdd
func validateManagedSpec(spec ManagedSpec) error {
	// Zero value / incomplete spec
	if spec.create.Config == nil || spec.create.HostConfig == nil {
		return fmt.Errorf("refusing to create: managed spec is zero or missing container config")
	}
	if spec.profile == "" || spec.runtime == "" {
		return fmt.Errorf("refusing to create: managed spec is zero or missing profile/runtime")
	}
	if spec.identity.ScaleSetID <= 0 || spec.identity.RunnerID <= 0 {
		return fmt.Errorf("refusing to create: managed spec has no valid runner identity")
	}
	if !model.ValidRunnerName(spec.identity.RunnerName) {
		return fmt.Errorf("refusing to create: runner name %q is not a valid canonical runner name", spec.identity.RunnerName)
	}
	if spec.create.Name == "" {
		return fmt.Errorf("refusing to create: managed spec has no container name")
	}
	if spec.create.Config.Image == "" {
		return fmt.Errorf("refusing to create: managed spec has no image")
	}

	// Profile and runtime consistency
	switch spec.profile {
	case config.ProfileStandard:
		if !validRuntimeName(spec.runtime) {
			return fmt.Errorf("refusing to create: standard profile requires a valid runtime name, got %q", spec.runtime)
		}
	case config.ProfileDindRunner:
		if spec.runtime != dindRuntime {
			return fmt.Errorf("refusing to create: dind-runner profile requires runtime %q, got %q", dindRuntime, spec.runtime)
		}
	default:
		return fmt.Errorf("refusing to create: unknown profile %q", spec.profile)
	}

	// Re-check that the real create values match the values kept in the spec.
	// Checking only the kept values would leave a path where a tampered spec
	// reaches the daemon.
	if spec.create.HostConfig.Runtime != spec.runtime {
		return fmt.Errorf("refusing to create: host config runtime %q does not match spec runtime %q", spec.create.HostConfig.Runtime, spec.runtime)
	}

	// Fixed container.Config values
	cfg := spec.create.Config
	if cfg.Image == "" || cfg.User == "" || cfg.WorkingDir != runnerWorkDir {
		return fmt.Errorf("refusing to create: container config violates the runner contract (image/user/working dir)")
	}
	if len(cfg.Cmd) == 0 || cfg.Cmd[0] != runnerCommand {
		return fmt.Errorf("refusing to create: command must be [%s]", runnerCommand)
	}
	if cfg.StopSignal != "SIGTERM" {
		return fmt.Errorf("refusing to create: stop signal must be SIGTERM")
	}
	if cfg.Tty || cfg.OpenStdin || cfg.StdinOnce {
		return fmt.Errorf("refusing to create: tty/stdin must be disabled")
	}
	switch spec.profile {
	case config.ProfileStandard:
		if cfg.User != standardUser {
			return fmt.Errorf("refusing to create: standard profile user must be %q, got %q", standardUser, cfg.User)
		}
		if len(cfg.Entrypoint) != 0 {
			return fmt.Errorf("refusing to create: standard profile must not have an entrypoint")
		}
	case config.ProfileDindRunner:
		if cfg.User != dindUser {
			return fmt.Errorf("refusing to create: dind-runner profile user must be %q, got %q", dindUser, cfg.User)
		}
		if len(cfg.Entrypoint) != 1 || cfg.Entrypoint[0] != dindEntrypoint {
			return fmt.Errorf("refusing to create: dind-runner profile entrypoint must be [%s]", dindEntrypoint)
		}
	}

	// Capabilities
	hostCfg := spec.create.HostConfig
	if len(hostCfg.CapDrop) != 1 || hostCfg.CapDrop[0] != "ALL" {
		return fmt.Errorf("refusing to create: cap drop must be exactly [ALL]")
	}
	switch spec.profile {
	case config.ProfileStandard:
		if len(hostCfg.CapAdd) != 0 {
			return fmt.Errorf("refusing to create: standard profile must not add capabilities")
		}
	case config.ProfileDindRunner:
		allowed := config.DindCapabilities()
		for _, cap := range hostCfg.CapAdd {
			if !slices.Contains(allowed, cap) {
				return fmt.Errorf("refusing to create: capability %q is not in the dind-runner allowed set", cap)
			}
		}
	}

	// Zero resources mean unlimited; negative values are invalid.
	res := &hostCfg.Resources
	if res.NanoCPUs < 0 {
		return fmt.Errorf("refusing to create: NanoCPUs must be non-negative")
	}
	if res.Memory < 0 {
		return fmt.Errorf("refusing to create: memory must be non-negative")
	}
	if res.Memory == 0 && res.MemorySwap > 0 {
		return fmt.Errorf("refusing to create: memory swap cannot be limited when memory is unlimited")
	}
	if res.MemorySwap < 0 {
		return fmt.Errorf("refusing to create: memory swap must be non-negative")
	}
	if res.Memory > 0 && res.MemorySwap > 0 && res.MemorySwap < res.Memory {
		return fmt.Errorf("refusing to create: memory swap must be >= memory")
	}
	if res.PidsLimit != nil && *res.PidsLimit < 0 {
		return fmt.Errorf("refusing to create: pids limit must be non-negative")
	}

	// Host namespaces are forbidden. PID/UTS/Userns must not be shared with
	// the host or other containers; IPC/cgroup are fixed to private.
	if string(hostCfg.PidMode) == "host" || strings.HasPrefix(string(hostCfg.PidMode), "container:") {
		return fmt.Errorf("refusing to create: host or shared PID namespace is not allowed")
	}
	if string(hostCfg.UTSMode) == "host" {
		return fmt.Errorf("refusing to create: host UTS namespace is not allowed")
	}
	if string(hostCfg.UsernsMode) == "host" {
		return fmt.Errorf("refusing to create: host user namespace is not allowed")
	}
	if hostCfg.IpcMode != container.IPCModePrivate {
		return fmt.Errorf("refusing to create: IPC mode must be private")
	}
	if hostCfg.CgroupnsMode != container.CgroupnsModePrivate {
		return fmt.Errorf("refusing to create: cgroup namespace mode must be private")
	}

	// Only an existing non-host network is allowed
	netMode := string(hostCfg.NetworkMode)
	if netMode == "" || netMode == "host" || netMode == "none" || netMode == "container" || strings.HasPrefix(netMode, "container:") {
		return fmt.Errorf("refusing to create: network mode %q is not allowed (existing non-host network required)", netMode)
	}

	// privileged, host sockets, devices and bind mounts are forbidden
	if hostCfg.Privileged {
		return fmt.Errorf("refusing to create: privileged mode is not allowed")
	}
	if len(hostCfg.Binds) > 0 {
		return fmt.Errorf("refusing to create: bind mounts are not allowed")
	}
	if len(hostCfg.VolumesFrom) > 0 {
		return fmt.Errorf("refusing to create: volumes-from is not allowed")
	}
	if len(res.Devices) > 0 {
		return fmt.Errorf("refusing to create: device mounts are not allowed")
	}
	if len(res.DeviceRequests) > 0 {
		return fmt.Errorf("refusing to create: device requests are not allowed")
	}
	for _, m := range hostCfg.Mounts {
		if m.Type == mount.TypeBind {
			return fmt.Errorf("refusing to create: bind mounts are not allowed")
		}
		if strings.Contains(m.Source, "docker.sock") || strings.Contains(m.Target, "docker.sock") {
			return fmt.Errorf("refusing to create: docker socket mounts are not allowed")
		}
	}
	switch spec.profile {
	case config.ProfileStandard:
		if len(hostCfg.Mounts) > 0 {
			return fmt.Errorf("refusing to create: standard profile must not have mounts")
		}
	case config.ProfileDindRunner:
		// Fixed dind-runner tmpfs: /var/lib/docker, mode 0700, positive size.
		if len(hostCfg.Mounts) != 1 ||
			hostCfg.Mounts[0].Type != mount.TypeTmpfs ||
			hostCfg.Mounts[0].Target != dindRunnerDataDir {
			return fmt.Errorf("refusing to create: dind-runner profile requires exactly one tmpfs mount at %s", dindRunnerDataDir)
		}
		opt := hostCfg.Mounts[0].TmpfsOptions
		if opt == nil || opt.SizeBytes <= 0 {
			return fmt.Errorf("refusing to create: dind-runner tmpfs %s must have a positive size", dindRunnerDataDir)
		}
		if opt.Mode != 0o700 {
			return fmt.Errorf("refusing to create: dind-runner tmpfs %s mode must be 0700", dindRunnerDataDir)
		}
	}

	// JIT env contract.
	const jitPrefix = jitEnvKey + "="
	const returnValue = returnEnvKey + "=1"
	const uaPrefix = userAgentEnvPrefix + "=gha-docker-controller/"
	haveJIT, haveReturn, haveUA := false, false, false
	for _, e := range cfg.Env {
		switch {
		case strings.HasPrefix(e, jitPrefix):
			if strings.TrimPrefix(e, jitPrefix) == "" {
				return fmt.Errorf("refusing to create: JIT config env value must not be empty")
			}
			haveJIT = true
		case e == returnValue:
			haveReturn = true
		case strings.HasPrefix(e, uaPrefix):
			if strings.TrimPrefix(e, uaPrefix) == "" {
				return fmt.Errorf("refusing to create: user agent env value must not be empty")
			}
			haveUA = true
		}
	}
	if !haveJIT || !haveReturn || !haveUA {
		return fmt.Errorf("refusing to create: JIT env contract is incomplete")
	}

	// SecurityOpt: no-new-privileges is required; unconfined is forbidden.
	haveNNP := false
	for _, s := range hostCfg.SecurityOpt {
		if s == "no-new-privileges" {
			haveNNP = true
		}
		if s == "unconfined" || strings.HasPrefix(s, "seccomp=unconfined") || strings.HasPrefix(s, "apparmor=unconfined") {
			return fmt.Errorf("refusing to create: unconfined security option is not allowed")
		}
	}
	if !haveNNP {
		return fmt.Errorf("refusing to create: no-new-privileges security option is required")
	}

	// Re-validate the six labels: both the spec-held value and the
	// create.Config.Labels actually sent to the daemon are exact-checked.
	if err := model.ValidateLabels(spec.labels, spec.identity); err != nil {
		return fmt.Errorf("refusing to create: %w", err)
	}
	if err := model.ValidateLabels(spec.create.Config.Labels, spec.identity); err != nil {
		return fmt.Errorf("refusing to create: %w", err)
	}

	// Remaining fixed fields.
	if hostCfg.AutoRemove {
		return fmt.Errorf("refusing to create: auto remove must be disabled")
	}
	if hostCfg.ReadonlyRootfs {
		return fmt.Errorf("refusing to create: read-only rootfs is not allowed")
	}
	if hostCfg.RestartPolicy.Name != container.RestartPolicyDisabled {
		return fmt.Errorf("refusing to create: restart policy must be %q", container.RestartPolicyDisabled)
	}
	if hostCfg.Init == nil || !*hostCfg.Init {
		return fmt.Errorf("refusing to create: init must be enabled")
	}
	return nil
}
