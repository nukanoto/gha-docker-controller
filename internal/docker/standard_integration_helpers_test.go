//go:build integration

// standard_integration_helpers_test.go defines the common helpers of the
// standard integration tests. The unmanaged sentinel
// (createUnmanagedSentinel and verifyAndRemoveUnmanagedSentinel) is shared
// with the dind integration test. The sentinel approach does not compare
// whole-daemon snapshots; it verifies the ID/state/label invariants of the
// single unique sentinel the test creates (no contention with fixtures of
// parallel packages). cleanupManagedFixture follows the test fixture
// cleanup principle (via the fresh managed guard); only in the abnormal
// case does it fall back to the forceRemoveTestContainer official SDK
// forced removal (forceRemoveTestContainer is defined in
// dind_integration_test.go).
package docker

import (
	"context"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"

	cerrdefs "github.com/containerd/errdefs"

	"github.com/nukanoto/gha-docker-controller/internal/config"
	"github.com/nukanoto/gha-docker-controller/internal/model"
)

// cleanupManagedFixture removes a test-created container through the
// production removal path (VerifyManaged + CleanupManaged). CleanupManaged
// always does a fresh inspect and managed label re-match (fresh managed
// guard) internally, so this is the cleanup principle. Only when the guard
// detects a label mismatch or the managed path returns an error does it
// force-remove with the official SDK ContainerRemove
// (forceRemoveTestContainer). A 404 is a state observation meaning "already
// gone" and counts as success. It is called from t.Cleanup, so it uses an
// independent bounded context instead of t.Context() (already cancelled
// right before cleanup).
func cleanupManagedFixture(t *testing.T, c *Client, containerID string, identity model.RunnerIdentity) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	mc, err := c.VerifyManaged(ctx, containerID, identity)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			// Already gone (for example, already removed by the CleanupManaged
			// of the test body).
			return
		}
		// Abnormal case: fresh inspect + exact check of the test labels,
		// then force-remove with the official SDK.
		t.Logf("managed 経路の VerifyManaged が失敗したため、test-only の強制削除へ倒します: %v", err)
		forceRemoveTestContainer(t, c, containerID, identity)
		return
	}
	if _, err := c.CleanupManaged(ctx, mc, ManagedCleanupOptions{StopTimeout: 30 * time.Second}); err != nil {
		// Abnormal case: same as above.
		t.Logf("CleanupManaged が失敗したため、test-only の強制削除へ倒します: %v", err)
		forceRemoveTestContainer(t, c, containerID, identity)
	}
}

// unmanagedSentinelLabel is the test-only label key that identifies an
// unmanaged sentinel container.
const unmanagedSentinelLabel = "ghadc.test.sentinel"

// createUnmanagedSentinel creates one unmanaged sentinel container without
// managed labels and registers its cleanup (verify and remove). It is never
// started; it stays in the created state.
func createUnmanagedSentinel(t *testing.T, c *Client, imageRef string) string {
	t.Helper()
	created, err := c.c.ContainerCreate(t.Context(), mobyclient.ContainerCreateOptions{Config: &container.Config{
		Image: imageRef, Cmd: []string{"/bin/sleep", "600"}, Labels: map[string]string{unmanagedSentinelLabel: "1"},
	}})
	if err != nil {
		t.Fatalf("unmanaged sentinel の作成に失敗しました: %v", err)
	}
	// Never leave it behind: register the cleanup here (LIFO, so the managed
	// fixture cleanup registered later runs first).
	t.Cleanup(func() {
		verifyAndRemoveUnmanagedSentinel(t, c, created.ID)
	})
	return created.ID
}

// verifyAndRemoveUnmanagedSentinel fresh-inspects that the sentinel
// ID/state/label are unchanged, then removes it with the official SDK (a
// 404 counts as success). The production removal path requires managed
// labels, so the official SDK is used as the cleanup of the test-only
// container.
func verifyAndRemoveUnmanagedSentinel(t *testing.T, c *Client, containerID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	inspect, err := c.ContainerInspect(ctx, containerID, mobyclient.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return
		}
		t.Fatalf("unmanaged sentinel の inspect が失敗しました: %v", err)
	}
	if got := string(inspect.Container.State.Status); got != string(container.StateCreated) {
		t.Fatalf("unmanaged sentinel の状態が変化しました: %q → %q", container.StateCreated, got)
	}
	if got := inspect.Container.Config.Labels[unmanagedSentinelLabel]; got != "1" {
		t.Fatalf("unmanaged sentinel の test label が変化しました: %v", inspect.Container.Config.Labels)
	}
	if _, err := c.c.ContainerRemove(ctx, containerID, mobyclient.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !cerrdefs.IsNotFound(err) {
		t.Fatalf("unmanaged sentinel の削除に失敗しました: %v", err)
	}
}

// verifyInspectSecurityFields verifies that the security fields really
// exist in the inspect result. It verifies with inspect alone (the
// production read-only API) that the create-time spec was reflected on the
// daemon as-is.
func verifyInspectSecurityFields(t *testing.T, in container.InspectResponse, cfg *config.Config, runtime string, input ManagedSpecInput) {
	t.Helper()
	cc := in.Config
	hc := in.HostConfig

	// container.Config
	if cc.User != "runner" || cc.WorkingDir != "/home/runner" {
		t.Fatalf("daemon 上の Config の user/workdir が契約と一致しません: user=%q workdir=%q", cc.User, cc.WorkingDir)
	}
	if len(cc.Cmd) != 1 || cc.Cmd[0] != "/home/runner/run.sh" {
		t.Fatalf("daemon 上の command が契約と一致しません: %v", cc.Cmd)
	}
	if len(cc.Entrypoint) != 0 {
		t.Fatalf("standard の entrypoint は空でなければなりません: %v", cc.Entrypoint)
	}
	if cc.StopSignal != "SIGTERM" || cc.Tty || cc.OpenStdin || cc.StdinOnce {
		t.Fatalf("daemon 上の process 設定が契約と一致しません: signal=%q tty=%v open-stdin=%v", cc.StopSignal, cc.Tty, cc.OpenStdin)
	}
	// The three JIT env values really exist in the inspect. This exposure is
	// the documented README contract; compare as a set (the order depends on
	// the daemon).
	wantEnv := map[string]bool{
		"ACTIONS_RUNNER_INPUT_JITCONFIG=" + input.JITConfig:                                      true,
		"ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE=1":                                   true,
		"GITHUB_ACTIONS_RUNNER_EXTRA_USER_AGENT=gha-docker-controller/" + input.UserAgentVersion: true,
	}
	found := 0
	for _, e := range cc.Env {
		if wantEnv[e] {
			found++
		}
	}
	if found != len(wantEnv) {
		t.Fatalf("daemon 上の JIT env が契約と一致しません: %v", cc.Env)
	}

	// HostConfig
	if hc.Runtime != runtime {
		t.Fatalf("daemon 上の runtime が不正です: %q (期待 %q)", hc.Runtime, runtime)
	}
	if hc.Privileged || hc.ReadonlyRootfs || hc.AutoRemove {
		t.Fatalf("privileged/readonly/auto-remove が daemon 上で有効です: privileged=%v readonly=%v auto-remove=%v", hc.Privileged, hc.ReadonlyRootfs, hc.AutoRemove)
	}
	if len(hc.CapDrop) != 1 || hc.CapDrop[0] != "ALL" || len(hc.CapAdd) != 0 {
		t.Fatalf("daemon 上の capability 設定が契約と一致しません: drop=%v add=%v", hc.CapDrop, hc.CapAdd)
	}
	if hc.NetworkMode != "bridge" || hc.IpcMode != container.IPCModePrivate || hc.CgroupnsMode != container.CgroupnsModePrivate {
		t.Fatalf("daemon 上の namespace/network が契約と一致しません: net=%q ipc=%q cgroupns=%q", hc.NetworkMode, hc.IpcMode, hc.CgroupnsMode)
	}
	if hc.PidMode != "" || hc.UTSMode != "" || hc.UsernsMode != "" {
		t.Fatalf("daemon 上に host namespace が設定されています: pid=%q uts=%q userns=%q", hc.PidMode, hc.UTSMode, hc.UsernsMode)
	}
	if hc.RestartPolicy.Name != container.RestartPolicyDisabled {
		t.Fatalf("daemon 上の restart policy が不正です: %q", hc.RestartPolicy.Name)
	}
	if hc.Init == nil || !*hc.Init {
		t.Fatal("daemon 上で init が有効ではありません")
	}
	if !slices.Contains(hc.SecurityOpt, "no-new-privileges") {
		t.Fatalf("daemon 上に no-new-privileges がありません: %v", hc.SecurityOpt)
	}
	if len(hc.Binds) != 0 || len(hc.Devices) != 0 || len(hc.DeviceRequests) != 0 || len(hc.VolumesFrom) != 0 || len(hc.Mounts) != 0 {
		t.Fatalf("daemon 上に禁止 mount/device が存在します: binds=%v devices=%v mounts=%v", hc.Binds, hc.Devices, hc.Mounts)
	}
	if got := hc.Tmpfs["/tmp"]; got != "size=64m,ro" {
		t.Fatalf("daemon 上の tmpfs が不正です: %v", hc.Tmpfs)
	}

	// resources
	res := &hc.Resources
	if res.NanoCPUs != int64(cfg.Runner.CPU) || res.Memory != int64(cfg.Runner.Memory) || res.MemorySwap != int64(cfg.Runner.MemorySwap) {
		t.Fatalf("daemon 上の resource が契約と一致しません: cpu=%d memory=%d swap=%d", res.NanoCPUs, res.Memory, res.MemorySwap)
	}
	if res.PidsLimit == nil || *res.PidsLimit != cfg.Runner.PidsLimit {
		t.Fatalf("daemon 上の pids limit が不正です: %v", res.PidsLimit)
	}
	foundUlimit := false
	for _, u := range res.Ulimits {
		if u.Name == "nofile" && u.Soft == 1024 && u.Hard == 2048 {
			foundUlimit = true
		}
	}
	if !foundUlimit {
		t.Fatalf("daemon 上に nofile=1024:2048 の ulimit がありません: %+v", res.Ulimits)
	}
	if !slices.Equal(hc.DNS, []netip.Addr{netip.MustParseAddr("1.1.1.1")}) {
		t.Fatalf("daemon 上の DNS が不正です: %v", hc.DNS)
	}
	if len(hc.ExtraHosts) != 1 || hc.ExtraHosts[0] != "db:127.0.0.1" {
		t.Fatalf("daemon 上の extraHosts が不正です: %v", hc.ExtraHosts)
	}
}
