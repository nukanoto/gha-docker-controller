//go:build integration

// Shared integration helpers use a single unmanaged sentinel and the fresh
// managed guard; this avoids whole-daemon snapshots and parallel contention.
package docker

import (
	"context"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"

	cerrdefs "github.com/containerd/errdefs"

	"github.com/nukanoto/arc-docker/internal/config"
	"github.com/nukanoto/arc-docker/internal/model"
)

// cleanupManagedFixture removes a fixture through the production guard and
// uses the test-only force path only after that path fails.
func cleanupManagedFixture(t *testing.T, c *Client, containerID string, identity model.RunnerIdentity) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	mc, err := c.VerifyManaged(ctx, containerID, identity)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return
		}
		t.Logf("managed 経路の VerifyManaged が失敗したため、test-only の強制削除へ倒します: %v", err)
		forceRemoveTestContainer(t, c, containerID, identity)
		return
	}
	if _, err := c.CleanupManaged(ctx, mc, ManagedCleanupOptions{StopTimeout: 30 * time.Second}); err != nil {
		t.Logf("CleanupManaged が失敗したため、test-only の強制削除へ倒します: %v", err)
		forceRemoveTestContainer(t, c, containerID, identity)
	}
}

// unmanagedSentinelLabel identifies the test-only unmanaged sentinel.
const unmanagedSentinelLabel = "ghadc.test.sentinel"

// createUnmanagedSentinel creates an unstarted unmanaged control fixture.
func createUnmanagedSentinel(t *testing.T, c *Client, imageRef string) string {
	t.Helper()
	created, err := c.c.ContainerCreate(t.Context(), mobyclient.ContainerCreateOptions{Config: &container.Config{
		Image: imageRef, Cmd: []string{"/bin/sleep", "600"}, Labels: map[string]string{unmanagedSentinelLabel: "1"},
	}})
	if err != nil {
		t.Fatalf("unmanaged sentinel の作成に失敗しました: %v", err)
	}
	// Register cleanup here so later fixture cleanup runs first (LIFO).
	t.Cleanup(func() {
		verifyAndRemoveUnmanagedSentinel(t, c, created.ID)
	})
	return created.ID
}

// verifyAndRemoveUnmanagedSentinel checks the control fixture before removal.
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

// verifyInspectHostConfig checks user-provided HostConfig fields.
func verifyInspectHostConfig(t *testing.T, in container.InspectResponse, cfg *config.Config, input ManagedSpecInput) {
	t.Helper()
	cc := in.Config
	hc := in.HostConfig

	// Docker does not guarantee environment ordering.
	wantEnv := map[string]bool{
		"ACTIONS_RUNNER_INPUT_JITCONFIG=" + input.JITConfig:                                      true,
		"ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE=1":                                   true,
		"GITHUB_ACTIONS_RUNNER_EXTRA_USER_AGENT=arc-docker/" + input.UserAgentVersion: true,
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

	expected := cfg.Runner.HostConfig
	if expected == nil {
		t.Fatal("テスト設定の HostConfig が nil です")
	}
	if hc.Runtime != expected.Runtime || hc.NetworkMode != expected.NetworkMode {
		t.Fatalf("daemon 上の HostConfig が設定値と一致しません: runtime=%q network=%q", hc.Runtime, hc.NetworkMode)
	}
}
