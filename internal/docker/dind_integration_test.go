//go:build integration

// DinD integration uses real Docker/runsc and production container paths. The
// inner daemon is verified through logs and wait, never exec; invalid JIT keeps
// the test free of credentials and GitHub I/O.
package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/system"
	mobyclient "github.com/moby/moby/client"

	cerrdefs "github.com/containerd/errdefs"

	"github.com/nukanoto/gha-docker-controller/internal/config"
	"github.com/nukanoto/gha-docker-controller/internal/model"
)

const (
	dindImageEnv   = "GHDC_TEST_DIND_IMAGE"
	dindContextEnv = "GHDC_TEST_DIND_CONTEXT"
	dindTimeoutEnv = "GHDC_TEST_DIND_TIMEOUT"
)

// buildDindImage builds the test image and drains the build response.
func buildDindImage(t *testing.T, c *Client, contextDir, tag string) {
	t.Helper()
	buildContext, err := tarDindContext(contextDir)
	if err != nil {
		t.Fatalf("dind image context を tar にできませんでした: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()
	res, err := c.c.ImageBuild(ctx, buildContext, mobyclient.ImageBuildOptions{
		Tags:        []string{tag},
		Dockerfile:  "Dockerfile",
		PullParent:  true,
		Remove:      true,
		ForceRemove: true,
	})
	if err != nil {
		t.Fatalf("ImageBuild が失敗しました: %v", err)
	}
	defer res.Body.Close()
	dec := json.NewDecoder(res.Body)
	for {
		var msg struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("build stream の読み込みに失敗しました: %v", err)
		}
		if msg.Error != "" {
			t.Fatalf("image build が失敗しました: %s", msg.Error)
		}
	}
}

// tarDindContext creates the minimal build context.
func tarDindContext(contextDir string) (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range []string{"Dockerfile", "entrypoint.sh"} {
		data, err := os.ReadFile(filepath.Join(contextDir, name))
		if err != nil {
			return nil, err
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), ModTime: time.Unix(0, 0).UTC()}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	return &buf, tw.Close()
}

// prepareDindImage pulls the configured image or builds the local context.
func prepareDindImage(t *testing.T, c *Client) (imageRef string) {
	t.Helper()
	if ref := os.Getenv(dindImageEnv); ref != "" {
		pullCtx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
		defer cancel()
		if err := c.EnsureImage(pullCtx, ref, config.PullPolicyIfNotPresent); err != nil {
			t.Fatalf("pinned dind image %s を用意できませんでした: %v", ref, err)
		}
		return ref
	}
	contextDir := os.Getenv(dindContextEnv)
	if contextDir == "" {
		contextDir = "../../images/dind-runner"
	}
	tag := fmt.Sprintf("ghadc-test/dind-integration:%012x", rand.Uint64())
	buildDindImage(t, c, contextDir, tag)
	t.Cleanup(func() { removeTestImage(t, c, tag) })
	return tag
}

// removeTestImage removes a locally built image when possible.
func removeTestImage(t *testing.T, c *Client, ref string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := c.c.ImageRemove(ctx, ref, mobyclient.ImageRemoveOptions{Force: false, PruneChildren: true}); err != nil && !cerrdefs.IsNotFound(err) {
		t.Logf("test image %s の削除に失敗しました (best-effort): %v", ref, err)
	}
}

// forceRemoveTestContainer is the test-only fallback after a fresh label check.
func forceRemoveTestContainer(t *testing.T, c *Client, containerID string, identity model.RunnerIdentity) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	inspect, err := c.ContainerInspect(ctx, containerID, mobyclient.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return
		}
		t.Fatalf("cleanup 前の fresh inspect が失敗しました: %v", err)
	}
	if err := verifyManagedLabels(containerID, containerLabels(inspect.Container), identity); err != nil {
		t.Fatalf("cleanup 前の managed label の exact 確認に失敗しました (削除しません): %v", err)
	}
	if _, err := c.c.ContainerRemove(ctx, containerID, mobyclient.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !cerrdefs.IsNotFound(err) {
		t.Fatalf("test container の強制 cleanup に失敗しました: %v", err)
	}
}

// verifyDindInspectFields checks the user-provided HostConfig and env.
func verifyDindInspectFields(t *testing.T, in container.InspectResponse, cfg *config.Config, input ManagedSpecInput) {
	t.Helper()
	cc := in.Config
	hc := in.HostConfig

	// Docker does not guarantee environment ordering.
	for _, want := range []string{
		"ACTIONS_RUNNER_INPUT_JITCONFIG=" + input.JITConfig,
		"ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE=1",
		"GITHUB_ACTIONS_RUNNER_EXTRA_USER_AGENT=gha-docker-controller/" + input.UserAgentVersion,
	} {
		if !slices.Contains(cc.Env, want) {
			t.Fatalf("daemon 上の JIT env が契約と一致しません: %v", cc.Env)
		}
	}

	expected := cfg.Runner.HostConfig
	if expected == nil {
		t.Fatal("テスト設定の HostConfig が nil です")
	}
	if hc.Runtime != expected.Runtime || hc.NetworkMode != expected.NetworkMode || hc.Privileged != expected.Privileged {
		t.Fatalf("daemon 上の HostConfig が設定値と一致しません: runtime=%q network=%q privileged=%v", hc.Runtime, hc.NetworkMode, hc.Privileged)
	}
	if !slices.Equal(hc.CapDrop, expected.CapDrop) || !slices.Equal(hc.CapAdd, expected.CapAdd) {
		t.Fatalf("daemon 上の capability が設定値と一致しません: drop=%v add=%v", hc.CapDrop, hc.CapAdd)
	}
	if len(hc.Mounts) != len(expected.Mounts) || len(hc.Mounts) != 1 {
		t.Fatalf("daemon 上の mount が設定値と一致しません: got=%+v want=%+v", hc.Mounts, expected.Mounts)
	}
	m := hc.Mounts[0]
	wantMount := expected.Mounts[0]
	if m.Type != mount.TypeTmpfs || m.Target != wantMount.Target || m.TmpfsOptions == nil || wantMount.TmpfsOptions == nil ||
		m.TmpfsOptions.SizeBytes != wantMount.TmpfsOptions.SizeBytes || m.TmpfsOptions.Mode != wantMount.TmpfsOptions.Mode {
		t.Fatalf("daemon 上の tmpfs が設定値と一致しません: got=%+v want=%+v", m, wantMount)
	}
}

// fetchLogs returns bounded container logs.
func fetchLogs(t *testing.T, c *Client, containerID string, limit int) LogResult {
	t.Helper()
	logs, err := c.FetchLogs(t.Context(), containerID, LogOptions{
		MaxStdoutBytes: limit,
		MaxStderrBytes: limit,
		Tail:           "all",
	})
	if err != nil {
		t.Fatalf("FetchLogs が失敗しました: %v", err)
	}
	return logs
}

// waitForLogMarker polls logs until a marker appears.
func waitForLogMarker(t *testing.T, c *Client, containerID, marker string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		logs := fetchLogs(t, c, containerID, 256*1024)
		if strings.Contains(logs.Stderr, marker) || strings.Contains(logs.Stdout, marker) {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// snippet limits log data included in failures.
func snippet(s string) string {
	const limit = 2 * 1024
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

// runtimeNames returns runtime names in stable order.
func runtimeNames(runtimes map[string]system.RuntimeWithStatus) []string {
	names := make([]string, 0, len(runtimes))
	for name := range runtimes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestDindLifecycle_RunscDaemon covers natural and signal exits under runsc.
func TestDindLifecycle_RunscDaemon(t *testing.T) {
	c, err := New(integrationHost, 2*time.Minute)
	if err != nil {
		t.Fatalf("Docker client を作成できませんでした (daemon 不在とみなして fail): %v", err)
	}
	defer c.Close()
	if _, err := c.Ping(t.Context()); err != nil {
		t.Fatalf("Docker daemon に接続できませんでした (daemon 不在とみなして fail): %v", err)
	}
	info, err := c.Info(t.Context())
	if err != nil {
		t.Fatalf("Docker Info を取得できませんでした: %v", err)
	}

	if _, ok := info.Info.Runtimes["runsc"]; !ok {
		t.Skipf("Docker daemon に runsc runtime が登録されていません。登録済み runtime: %v", runtimeNames(info.Info.Runtimes))
	}
	imageRef := prepareDindImage(t, c)
	// The shared sentinel proves unmanaged containers are untouched.
	createUnmanagedSentinel(t, c, imageRef)

	t.Run("自然終了と dockerd _ping", func(t *testing.T) { runDindContainer(t, c, imageRef, dindModeNatural) })
	t.Run("signal 転送と cleanup", func(t *testing.T) { runDindContainer(t, c, imageRef, dindModeSignal) })
}

// dindRunMode selects the container exit path.
type dindRunMode string

const (
	// Invalid JIT makes the runner exit naturally.
	dindModeNatural dindRunMode = "natural"
	// SIGTERM exercises signal forwarding during startup.
	dindModeSignal dindRunMode = "signal"
)

// dindTestTimeout returns the positive integration timeout.
func dindTestTimeout(t *testing.T) time.Duration {
	t.Helper()
	raw := os.Getenv(dindTimeoutEnv)
	if raw == "" {
		return 7 * time.Minute
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		t.Fatalf("%s が正の duration ではありません: %q", dindTimeoutEnv, raw)
	}
	return d
}

// runDindContainer creates and verifies one uniquely labeled fixture.
func runDindContainer(t *testing.T, c *Client, imageRef string, mode dindRunMode) {
	scaleSetID := rand.Int64N(1<<62) + 1
	runnerID := rand.Int64N(1<<62) + 1
	suffix := strconv.FormatInt(scaleSetID, 16)
	runnerName := model.RunnerName("dind-integration-test", suffix)
	if !model.ValidRunnerName(runnerName) {
		t.Fatalf("runner name が canonical 形式になりません: %q", runnerName)
	}
	containerName := model.ContainerName("dind-integration-test", runnerID, suffix)
	if len(containerName) > 63 {
		t.Fatalf("container name が 63 byte を超えています: %q", containerName)
	}
	identity := model.RunnerIdentity{ScaleSetID: scaleSetID, RunnerID: runnerID, RunnerName: runnerName}

	cfg := testConfig(t, "runsc")
	cfg.Runner.Image = imageRef
	cfg.Runner.HostConfig = &container.HostConfig{
		Runtime:     "runsc",
		NetworkMode: "bridge",
		CapDrop:     []string{"ALL"},
		CapAdd: []string{
			"AUDIT_WRITE", "CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL",
			"MKNOD", "NET_BIND_SERVICE", "NET_ADMIN", "NET_RAW", "SETFCAP",
			"SETGID", "SETPCAP", "SETUID", "SYS_ADMIN", "SYS_CHROOT", "SYS_PTRACE",
		},
		SecurityOpt:   []string{"no-new-privileges"},
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
		Init:          boolPointer(true),
		Mounts: []mount.Mount{{
			Type:   mount.TypeTmpfs,
			Target: "/var/lib/docker",
			TmpfsOptions: &mount.TmpfsOptions{
				SizeBytes: 2 * 1024 * 1024 * 1024,
				Mode:      0o700,
			},
		}},
		Resources: container.Resources{
			NanoCPUs:   2e9,
			Memory:     4 * 1024 * 1024 * 1024,
			MemorySwap: 6 * 1024 * 1024 * 1024,
		},
	}
	input := testInput(cfg)
	input.Identity = identity
	input.ContainerName = containerName
	input.JITConfig = "dind-integration-invalid-jit-config"
	input.ControllerInstance = "dind-integration-test"
	input.CreatedAt = time.Now().UTC()

	spec, err := BuildManagedSpec(input)
	if err != nil {
		t.Fatalf("BuildManagedSpec が失敗しました: %v", err)
	}
	create, err := c.CreateManaged(t.Context(), spec)
	if err != nil {
		t.Fatalf("CreateManaged が失敗しました: %v", err)
	}
	containerID := create.ID
	if containerID == "" {
		t.Fatalf("CreateManaged が空の container ID を返しました")
	}

	t.Cleanup(func() { forceRemoveTestContainer(t, c, containerID, identity) })

	// Image-derived labels are allowed; the six managed labels must match.
	inspect, err := c.ContainerInspect(t.Context(), containerID, mobyclient.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("作成直後の inspect が失敗しました: %v", err)
	}
	labels := inspect.Container.Config.Labels
	if err := model.ValidateLabels(labels, identity); err != nil {
		t.Fatalf("daemon 上の label が managed の contract を満たしません: %v", err)
	}
	verifyDindInspectFields(t, inspect.Container, cfg, input)

	if _, err := c.containerStart(t.Context(), containerID, mobyclient.ContainerStartOptions{}); err != nil {
		t.Fatalf("containerStart が失敗しました (HostConfig と image の要件を確認してください): %v", err)
	}
	verifyDindRun(t, c, containerID, mode)
}

// verifyDindRun checks startup, privilege drop, and cleanup through logs/wait.
func verifyDindRun(t *testing.T, c *Client, containerID string, mode dindRunMode) {
	timeout := dindTestTimeout(t)
	if mode == dindModeSignal {
		// Send SIGTERM after startup begins to exercise PID 1 forwarding.
		if !waitForLogMarker(t, c, containerID, "Waiting for Docker daemon", 90*time.Second) {
			t.Fatalf("\"Waiting for Docker daemon\" が log に現れませんでした (container が早期に終了した可能性があります)")
		}
		if _, err := c.c.ContainerKill(t.Context(), containerID, mobyclient.ContainerKillOptions{Signal: "SIGTERM"}); err != nil {
			t.Fatalf("SIGTERM の送信に失敗しました: %v", err)
		}
	}
	status, logs := waitExit(t, c, containerID, timeout)
	switch mode {
	case dindModeNatural:
		if status != 0 {
			t.Fatalf("container の終了 code が 0 ではありません: %d。stderr 抜粋: %s", status, snippet(logs.Stderr))
		}
		if !strings.Contains(logs.Stderr, "Docker daemon is ready") {
			t.Fatalf("inner dockerd の _ping が確認できません。stderr 抜粋: %s", snippet(logs.Stderr))
		}
		if !strings.Contains(logs.Stderr, "Starting runner") {
			t.Fatalf("runner の起動が確認できません。stderr 抜粋: %s", snippet(logs.Stderr))
		}
		if strings.Contains(logs.Stderr, "Must not run interactively with sudo") || strings.Contains(logs.Stdout, "Must not run interactively with sudo") {
			t.Fatal("runner が root で起動されました (setpriv による privilege drop が機能していません)")
		}
		if !strings.Contains(logs.Stdout, "Runner listener exit with terminated error") {
			t.Fatalf("runner の JIT 終了が確認できません。stdout 抜粋: %s", snippet(logs.Stdout))
		}
	case dindModeSignal:
		// TERM/INT may be reported by either PID 1 or the runner.
		if status == 137 || (status != 143 && status != 130 && status != 0) {
			t.Fatalf("container の終了 code が graceful な範囲にありません: %d", status)
		}
	default:
		t.Fatalf("未知の mode: %s", mode)
	}
	// EXIT cleanup must stop the inner daemon.
	if !strings.Contains(logs.Stderr, "Stopping Docker daemon") {
		t.Fatalf("dockerd 停止 cleanup が確認できません。stderr 抜粋: %s", snippet(logs.Stderr))
	}
}

// waitExit returns the exit code and bounded logs.
func waitExit(t *testing.T, c *Client, containerID string, timeout time.Duration) (int64, LogResult) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(t.Context(), timeout)
	waitResult, err := c.WaitContainer(waitCtx, containerID, mobyclient.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	cancel()
	if err != nil {
		t.Fatalf("container が期限内に終了しませんでした: %v", err)
	}
	return waitResult.StatusCode, fetchLogs(t, c, containerID, 512*1024)
}
