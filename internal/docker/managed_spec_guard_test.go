// managed_spec_guard_test.go verifies the validateManagedSpec re-validation
// that CreateManaged runs on every call. The re-validation completes before
// the Docker SDK call, so no real daemon is needed (no unix socket exists).
// The table covers zero values, tampered specs, privileged, host
// namespace/network, socket/device/bind mounts, runtime/profile mismatches,
// missing resources and seccomp/AppArmor unconfined. No mock/stub is used.
package docker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"

	"github.com/nukanoto/gha-docker-controller/internal/config"
	"github.com/nukanoto/gha-docker-controller/internal/model"
)

// TestValidateManagedSpec_ValidSpecs verifies that builder output always
// passes its own re-validation. The three configurations standard/runsc,
// standard/runc and dind/runsc are checked.
func TestValidateManagedSpec_ValidSpecs(t *testing.T) {
	cases := []struct{ profile, runtime string }{
		{config.ProfileStandard, "runsc"},
		{config.ProfileStandard, "runc"},
		{config.ProfileDindRunner, "runsc"},
	}
	for _, tc := range cases {
		t.Run(tc.profile+"-"+tc.runtime, func(t *testing.T) {
			spec := mustBuild(t, testConfig(t, tc.profile, tc.runtime))
			if err := validateManagedSpec(spec); err != nil {
				t.Fatalf("builder の出力が再検証を通りません: %v", err)
			}
		})
	}
}

// TestCreateManaged_RejectsTamperedSpec verifies that the CreateManaged
// re-validation rejects a tampered spec before any I/O. No unix socket
// exists, but the re-validation completes before the I/O, so CreateManaged
// never connects to a daemon.
func TestCreateManaged_RejectsTamperedSpec(t *testing.T) {
	c, err := New("unix:///tmp/ghadc-test-no-daemon.sock", time.Second)
	if err != nil {
		t.Fatalf("client の作成に失敗しました: %v", err)
	}
	cases := []struct {
		name    string
		profile string
		tamper  func(*ManagedSpec)
		wantErr string
	}{
		{name: "zero value spec", tamper: func(s *ManagedSpec) { *s = ManagedSpec{} }, wantErr: "zero or missing container config"},
		{name: "nil config pointer", tamper: func(s *ManagedSpec) { s.create.Config = nil }, wantErr: "zero or missing container config"},
		{name: "profile 欠落", tamper: func(s *ManagedSpec) { s.profile = "" }, wantErr: "missing profile/runtime"},
		{name: "runtime 欠落", tamper: func(s *ManagedSpec) { s.runtime = "" }, wantErr: "missing profile/runtime"},
		{name: "host config runtime 不一致", tamper: func(s *ManagedSpec) { s.create.HostConfig.Runtime = "runc" }, wantErr: "does not match spec runtime"},
		{name: "identity 欠落", tamper: func(s *ManagedSpec) { s.identity.RunnerID = 0 }, wantErr: "no valid runner identity"},
		{name: "runner name 非 canonical", tamper: func(s *ManagedSpec) { s.identity.RunnerName = "evil" }, wantErr: "not a valid canonical runner name"},
		{name: "name 欠落", tamper: func(s *ManagedSpec) { s.create.Name = "" }, wantErr: "no container name"},
		{name: "image 欠落", tamper: func(s *ManagedSpec) { s.create.Config.Image = "" }, wantErr: "no image"},
		{name: "privileged", tamper: func(s *ManagedSpec) { s.create.HostConfig.Privileged = true }, wantErr: "privileged mode is not allowed"},
		{name: "host PID namespace", tamper: func(s *ManagedSpec) { s.create.HostConfig.PidMode = "host" }, wantErr: "PID namespace"},
		{name: "host UTS namespace", tamper: func(s *ManagedSpec) { s.create.HostConfig.UTSMode = "host" }, wantErr: "UTS namespace"},
		{name: "host user namespace", tamper: func(s *ManagedSpec) { s.create.HostConfig.UsernsMode = "host" }, wantErr: "user namespace"},
		{name: "IPC host", tamper: func(s *ManagedSpec) { s.create.HostConfig.IpcMode = "host" }, wantErr: "IPC mode must be private"},
		{name: "cgroupns host", tamper: func(s *ManagedSpec) { s.create.HostConfig.CgroupnsMode = "host" }, wantErr: "cgroup namespace mode must be private"},
		{name: "host network", tamper: func(s *ManagedSpec) { s.create.HostConfig.NetworkMode = "host" }, wantErr: `network mode "host"`},
		{name: "none network", tamper: func(s *ManagedSpec) { s.create.HostConfig.NetworkMode = "none" }, wantErr: `network mode "none"`},
		{name: "container network", tamper: func(s *ManagedSpec) { s.create.HostConfig.NetworkMode = "container" }, wantErr: `network mode "container"`},
		{name: "container:id network", tamper: func(s *ManagedSpec) { s.create.HostConfig.NetworkMode = "container:other" }, wantErr: "network mode"},
		{name: "bind mount", tamper: func(s *ManagedSpec) { s.create.HostConfig.Binds = []string{"/host:/host"} }, wantErr: "bind mounts are not allowed"},
		{name: "device mount", tamper: func(s *ManagedSpec) {
			s.create.HostConfig.Resources.Devices = []container.DeviceMapping{{PathOnHost: "/dev/kvm", PathInContainer: "/dev/kvm"}}
		}, wantErr: "device mounts are not allowed"},
		{name: "device request", tamper: func(s *ManagedSpec) {
			s.create.HostConfig.Resources.DeviceRequests = []container.DeviceRequest{{Driver: "nvidia"}}
		}, wantErr: "device requests are not allowed"},
		{name: "volumes-from", tamper: func(s *ManagedSpec) { s.create.HostConfig.VolumesFrom = []string{"other"} }, wantErr: "volumes-from is not allowed"},
		{name: "docker socket tmpfs", tamper: func(s *ManagedSpec) {
			s.create.HostConfig.Mounts = []mount.Mount{{Type: mount.TypeTmpfs, Target: "/var/run/docker.sock"}}
		}, wantErr: "docker socket mounts are not allowed"},
		{name: "standard の mount", tamper: func(s *ManagedSpec) {
			s.create.HostConfig.Mounts = []mount.Mount{{Type: mount.TypeTmpfs, Target: "/tmp/x"}}
		}, wantErr: "standard profile must not have mounts"},
		{name: "dind runtime 不一致", profile: config.ProfileDindRunner, tamper: func(s *ManagedSpec) { s.runtime = "runc" }, wantErr: "dind-runner profile requires runtime"},
		{name: "standard runtime 不正", tamper: func(s *ManagedSpec) { s.runtime = "run sc" }, wantErr: "requires a valid runtime name"},
		{name: "unknown profile", tamper: func(s *ManagedSpec) { s.profile = "evil" }, wantErr: "unknown profile"},
		{name: "NanoCPUs 欠落", tamper: func(s *ManagedSpec) { s.create.HostConfig.Resources.NanoCPUs = 0 }, wantErr: "NanoCPUs must be positive"},
		{name: "memory 欠落", tamper: func(s *ManagedSpec) { s.create.HostConfig.Resources.Memory = 0 }, wantErr: "memory must be positive"},
		{name: "pidsLimit 欠落", tamper: func(s *ManagedSpec) { s.create.HostConfig.Resources.PidsLimit = nil }, wantErr: "pids limit must be positive"},
		{name: "memorySwap < memory", tamper: func(s *ManagedSpec) {
			s.create.HostConfig.Resources.MemorySwap = s.create.HostConfig.Resources.Memory - 1
		}, wantErr: "memory swap must be >= memory"},
		{name: "CapDrop 変更", tamper: func(s *ManagedSpec) { s.create.HostConfig.CapDrop = []string{"SYS_ADMIN"} }, wantErr: "cap drop must be exactly"},
		{name: "standard の CapAdd", tamper: func(s *ManagedSpec) { s.create.HostConfig.CapAdd = []string{"CHOWN"} }, wantErr: "must not add capabilities"},
		{name: "dind の 17 個外 CapAdd", profile: config.ProfileDindRunner, tamper: func(s *ManagedSpec) { s.create.HostConfig.CapAdd = append(s.create.HostConfig.CapAdd, "SYS_TIME") }, wantErr: "not in the dind-runner allowed set"},
		{name: "JIT env 欠落", tamper: func(s *ManagedSpec) { s.create.Config.Env = nil }, wantErr: "JIT env contract"},
		{name: "JIT env 空値", tamper: func(s *ManagedSpec) { s.create.Config.Env[0] = "ACTIONS_RUNNER_INPUT_JITCONFIG=" }, wantErr: "JIT config env value must not be empty"},
		{name: "no-new-privileges 欠落", tamper: func(s *ManagedSpec) { s.create.HostConfig.SecurityOpt = nil }, wantErr: "no-new-privileges security option is required"},
		{name: "seccomp unconfined", tamper: func(s *ManagedSpec) {
			s.create.HostConfig.SecurityOpt = []string{"no-new-privileges", "seccomp=unconfined"}
		}, wantErr: "unconfined security option"},
		{name: "apparmor unconfined", tamper: func(s *ManagedSpec) {
			s.create.HostConfig.SecurityOpt = []string{"no-new-privileges", "apparmor=unconfined"}
		}, wantErr: "unconfined security option"},
		{name: "standard user 変更", tamper: func(s *ManagedSpec) { s.create.Config.User = "root" }, wantErr: "standard profile user must be"},
		{name: "dind user 変更", profile: config.ProfileDindRunner, tamper: func(s *ManagedSpec) { s.create.Config.User = "runner" }, wantErr: "dind-runner profile user must be"},
		{name: "standard entrypoint 追加", tamper: func(s *ManagedSpec) { s.create.Config.Entrypoint = []string{"/bin/sh"} }, wantErr: "must not have an entrypoint"},
		{name: "dind entrypoint 変更", profile: config.ProfileDindRunner, tamper: func(s *ManagedSpec) { s.create.Config.Entrypoint = []string{"/bin/sh"} }, wantErr: "entrypoint must be"},
		{name: "command 変更", tamper: func(s *ManagedSpec) { s.create.Config.Cmd = []string{"/bin/sh"} }, wantErr: "command must be"},
		{name: "working dir 変更", tamper: func(s *ManagedSpec) { s.create.Config.WorkingDir = "/" }, wantErr: "runner contract"},
		{name: "stop signal 変更", tamper: func(s *ManagedSpec) { s.create.Config.StopSignal = "SIGKILL" }, wantErr: "stop signal must be SIGTERM"},
		{name: "tty 有効", tamper: func(s *ManagedSpec) { s.create.Config.Tty = true }, wantErr: "tty/stdin must be disabled"},
		{name: "auto remove 有効", tamper: func(s *ManagedSpec) { s.create.HostConfig.AutoRemove = true }, wantErr: "auto remove must be disabled"},
		{name: "read-only rootfs", tamper: func(s *ManagedSpec) { s.create.HostConfig.ReadonlyRootfs = true }, wantErr: "read-only rootfs is not allowed"},
		{name: "restart policy 変更", tamper: func(s *ManagedSpec) {
			s.create.HostConfig.RestartPolicy = container.RestartPolicy{Name: container.RestartPolicyAlways}
		}, wantErr: "restart policy must be"},
		{name: "init 無効", tamper: func(s *ManagedSpec) { init := false; s.create.HostConfig.Init = &init }, wantErr: "init must be enabled"},
		{name: "label 改変", tamper: func(s *ManagedSpec) { s.labels[model.ManagedLabelKey] = "false" }, wantErr: "managed label is invalid"},
		{name: "create label nil", tamper: func(s *ManagedSpec) { s.create.Config.Labels = nil }, wantErr: "labels are missing"},
		{name: "create label managed 改変", tamper: func(s *ManagedSpec) { s.create.Config.Labels[model.ManagedLabelKey] = "false" }, wantErr: "managed label is invalid"},
		{name: "create label runner-id 改変", tamper: func(s *ManagedSpec) { s.create.Config.Labels[model.RunnerIDLabelKey] = "1" }, wantErr: "runner-id label is invalid"},
		{name: "dind tmpfs 欠落", profile: config.ProfileDindRunner, tamper: func(s *ManagedSpec) { s.create.HostConfig.Mounts = nil }, wantErr: "requires exactly one tmpfs mount"},
		{name: "dind tmpfs size 0", profile: config.ProfileDindRunner, tamper: func(s *ManagedSpec) { s.create.HostConfig.Mounts[0].TmpfsOptions.SizeBytes = 0 }, wantErr: "positive size"},
		{name: "dind tmpfs mode 変更", profile: config.ProfileDindRunner, tamper: func(s *ManagedSpec) { s.create.HostConfig.Mounts[0].TmpfsOptions.Mode = 0o755 }, wantErr: "mode must be 0700"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			profile := tc.profile
			if profile == "" {
				profile = config.ProfileStandard
			}
			spec := mustBuild(t, testConfig(t, profile, "runsc"))
			tc.tamper(&spec)
			_, err := c.CreateManaged(context.Background(), spec)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("CreateManaged が改変済み spec を拒否しませんでした: err=%v", err)
			}
		})
	}
}
