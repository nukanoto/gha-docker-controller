//go:build integration

// Standard lifecycle integration uses a real Docker daemon and production
// managed-container paths. It covers registered runsc/runc runtimes, uses no
// mocks, and cleans up through a fresh managed-label guard.
package docker

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"

	"github.com/nukanoto/gha-docker-controller/internal/config"
	"github.com/nukanoto/gha-docker-controller/internal/model"
)

const (
	// Keep the integration target explicit; DOCKER_HOST is not used.
	integrationHost = "unix:///var/run/docker.sock"

	// Invalid JIT makes the official runner image exit deterministically.
	integrationDefaultImage = "ghcr.io/actions/actions-runner@sha256:0cfdcc701ce933c6d243c6b0b2da767366dc9f2e99961d4c3754b0b78084cdda"

	// This name is used only to construct fixture names.
	integrationScaleSetName = "standard-integration-test"
)

// TestStandardLifecycle_ManagedBoundary covers each registered runtime.
func TestStandardLifecycle_ManagedBoundary(t *testing.T) {
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

	// Prefer runsc, then cover the standard runc registration when present.
	runtimes := make([]string, 0, 2)
	if _, ok := info.Info.Runtimes["runsc"]; ok {
		runtimes = append(runtimes, "runsc")
	}
	if _, ok := info.Info.Runtimes["runc"]; ok {
		runtimes = append(runtimes, "runc")
	}
	if len(runtimes) == 0 {
		t.Fatalf("Docker daemon に runsc も runc も登録されていません: %v", info.Info.Runtimes)
	}

	for _, runtime := range runtimes {
		runtime := runtime
		t.Run("standard-"+runtime, func(t *testing.T) {
			testStandardLifecycle(t, c, runtime)
		})
	}
}

// testStandardLifecycle covers one runtime through production paths.
func testStandardLifecycle(t *testing.T, c *Client, runtime string) {
	// Unique identities avoid collisions with parallel or stale fixtures.
	scaleSetID := rand.Int64N(1<<62) + 1
	runnerID := rand.Int64N(1<<62) + 1
	suffix := strconv.FormatInt(scaleSetID, 16)
	runnerName := model.RunnerName(integrationScaleSetName, suffix)
	if !model.ValidRunnerName(runnerName) {
		t.Fatalf("runner name が canonical 形式になりません: %q", runnerName)
	}
	containerName := model.ContainerName(integrationScaleSetName, runnerID, suffix)
	if len(containerName) > 63 {
		t.Fatalf("container name が 63 byte を超えています: %q", containerName)
	}
	identity := model.RunnerIdentity{ScaleSetID: scaleSetID, RunnerID: runnerID, RunnerName: runnerName}
	t.Logf("一意な identity を生成しました: scale-set-id=%d runner-id=%d runner-name=%q", scaleSetID, runnerID, runnerName)

	// Confirm the random identity does not collide with an existing fixture.
	items, err := c.ListManaged(t.Context(), scaleSetID)
	if err != nil {
		t.Fatalf("ListManaged が失敗しました: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("一意な scale-set-id %d に既存 container があります (label が一意でありません): %v", scaleSetID, items)
	}

	// Allow local images while keeping the default reproducible.
	imageRef := os.Getenv("GHDC_TEST_IMAGE")
	if imageRef == "" {
		imageRef = integrationDefaultImage
	}
	t.Logf("使用する image: %s", imageRef)
	pullCtx, pullCancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer pullCancel()
	if err := c.EnsureImage(pullCtx, imageRef, config.PullPolicyIfNotPresent); err != nil {
		t.Fatalf("image %s を用意できませんでした (GHDC_TEST_IMAGE で local にある pinned image を指定できます): %v", imageRef, err)
	}

	// A single sentinel avoids whole-daemon snapshots and parallel contention.
	createUnmanagedSentinel(t, c, imageRef)

	// Use standard settings with only identity and name made unique.
	cfg := testConfig(t, runtime)
	cfg.Runner.Image = imageRef
	input := testInput(cfg)
	input.Identity = identity
	input.ContainerName = containerName
	input.JITConfig = "integration-test-invalid-jit-config"
	input.ControllerInstance = "standard-integration-test"
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
	t.Logf("container を作成しました: id=%s name=%s", containerID, containerName)

	t.Cleanup(func() {
		cleanupManagedFixture(t, c, containerID, identity)
	})

	// Image-derived labels are allowed; the six managed labels must match.
	inspect, err := c.ContainerInspect(t.Context(), containerID, mobyclient.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("作成直後の inspect が失敗しました: %v", err)
	}
	labels := inspect.Container.Config.Labels
	if err := model.ValidateLabels(labels, identity); err != nil {
		t.Fatalf("daemon 上の label が managed の contract を満たしません: %v", err)
	}
	t.Logf("label 検証を完了しました: required=%d total=%d (image 由来の extra label は許容)",
		len(model.RequiredLabelKeys()), len(labels))
	if labels[model.ScaleSetIDLabelKey] != strconv.FormatInt(scaleSetID, 10) {
		t.Fatalf("scale-set-id label が一意値になりません: %q", labels[model.ScaleSetIDLabelKey])
	}

	// Confirm security-sensitive HostConfig fields reached the daemon.
	verifyInspectHostConfig(t, inspect.Container, cfg, input)

	// The fresh guard must reject zero and mismatched identities before start.
	if _, err := c.StartManaged(t.Context(), containerID, model.RunnerIdentity{}); err == nil {
		t.Fatalf("zero identity の StartManaged が error を返しませんでした")
	}
	_, err = c.StartManaged(t.Context(), containerID, model.RunnerIdentity{
		ScaleSetID: scaleSetID, RunnerID: runnerID + 1, RunnerName: runnerName,
	})
	var guardErr *ManagedGuardError
	if !errors.As(err, &guardErr) {
		t.Fatalf("identity 不一致の StartManaged が ManagedGuardError を返しません: %v", err)
	}
	stateInspect, err := c.ContainerInspect(t.Context(), containerID, mobyclient.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("guard 拒否後の inspect が失敗しました: %v", err)
	}
	if got := string(stateInspect.Container.State.Status); got != string(container.StateCreated) {
		t.Fatalf("guard 拒否後に container が start されています: %q", got)
	}

	if _, err := c.StartManaged(t.Context(), containerID, identity); err != nil {
		t.Fatalf("StartManaged が失敗しました: %v", err)
	}

	waitCtx, waitCancel := context.WithTimeout(t.Context(), 10*time.Second)
	waitResult, err := c.WaitContainer(waitCtx, containerID, mobyclient.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	waitCancel()
	if err != nil {
		t.Logf("WaitContainer が timeout/error しました (長時間実行 runner は valid なため、running のまま cleanup の stop 経路で検証します): %v", err)
	} else {
		t.Logf("container は exit しました: code=%d", waitResult.StatusCode)
	}

	// Image output is runtime-dependent; only the bounds are contractual.
	logs, err := c.FetchLogs(t.Context(), containerID, LogOptions{
		MaxStdoutBytes: 64 * 1024,
		MaxStderrBytes: 64 * 1024,
		Tail:           "200",
	})
	if err != nil {
		t.Fatalf("FetchLogs が失敗しました: %v", err)
	}
	if len(logs.Stdout) > 64*1024 || len(logs.Stderr) > 64*1024 {
		t.Fatalf("log が上限を超えています: stdout=%d stderr=%d", len(logs.Stdout), len(logs.Stderr))
	}
	t.Logf("log を取得しました: stdout=%d bytes stderr=%d bytes", len(logs.Stdout), len(logs.Stderr))

	// The managed list must contain only this fixture.
	items, err = c.ListManaged(t.Context(), scaleSetID)
	if err != nil {
		t.Fatalf("ListManaged が失敗しました: %v", err)
	}
	if len(items) != 1 || items[0].ID() != containerID {
		t.Fatalf("ListManaged が期待と一致しません: %+v", items)
	}
	if !model.LabelsMatchIdentity(items[0].Labels(), identity) {
		t.Fatalf("列挙された container の label が identity と一致しません")
	}

	verified, err := c.VerifyManaged(t.Context(), containerID, identity)
	if err != nil {
		t.Fatalf("VerifyManaged が失敗しました: %v", err)
	}
	if verified.ID() != containerID {
		t.Fatalf("VerifyManaged が別の container を返しました: %q", verified.ID())
	}

	cleanupResult, err := c.CleanupManaged(t.Context(), verified, ManagedCleanupOptions{
		StopTimeout: time.Duration(cfg.Runner.StopTimeout),
	})
	if err != nil {
		t.Fatalf("CleanupManaged が失敗しました: %v", err)
	}
	if cleanupResult.ContainerID != containerID {
		t.Fatalf("CleanupManaged が別の container を返しました: %q", cleanupResult.ContainerID)
	}
	if !cleanupResult.HasExitCode {
		t.Fatalf("CleanupManaged が終了 code を観測できませんでした: %+v", cleanupResult)
	}
	if len(cleanupResult.Stdout) > 64*1024 || len(cleanupResult.Stderr) > 64*1024 {
		t.Fatalf("cleanup 時の log が上限を超えています: stdout=%d stderr=%d", len(cleanupResult.Stdout), len(cleanupResult.Stderr))
	}
	t.Logf("CleanupManaged が完了しました: exit-code=%d stdout=%d bytes stderr=%d bytes",
		cleanupResult.ExitCode, len(cleanupResult.Stdout), len(cleanupResult.Stderr))

	items, err = c.ListManaged(t.Context(), scaleSetID)
	if err != nil {
		t.Fatalf("ListManaged が失敗しました: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("cleanup 後に managed container が残っています: %+v", items)
	}
}
