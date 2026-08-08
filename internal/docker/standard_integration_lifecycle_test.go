//go:build integration

// standard_integration_lifecycle_test.go verifies the configured HostConfig
// lifecycle and managed boundary against a real Docker daemon. It covers
// only the public production paths (docker.New/Ping/Info, EnsureImage
// (if-not-present), BuildManagedSpec + CreateManaged,
// StartManaged/ContainerInspect/WaitContainer/FetchLogs,
// ListManaged/VerifyManaged/CleanupManaged); CleanupManaged is the only
// removal path of the simple lifecycle that depends only on Docker.
//
// The test fixture cleanup principle is the fresh managed guard path
// (VerifyManaged + CleanupManaged; internally it always does a fresh
// inspect and managed label re-match); only in the abnormal case does it
// exact-check the test labels with a fresh inspect and force-remove with
// the official Moby SDK ContainerRemove (forceRemoveTestContainer, shared
// with the dind integration helpers). After the test, ListManaged verifies
// that no managed container remains, and the ID/state/label invariants of
// the unmanaged sentinel container are checked at cleanup.
//
// GHDC_TEST_IMAGE can pin the image (default: the official runner image
// digest pin). No mock/fake/stub is used, and a missing daemon is an explicit
// fail, not t.Skip. The runtime is picked from runsc and runc in the
// registration list; a subtest runs for every registered one.
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
	// integrationHost is the real Docker daemon host the integration tests
	// connect to. It follows the absolute unix:// path contract; env
	// overrides such as DOCKER_HOST are not used.
	integrationHost = "unix:///var/run/docker.sock"

	// integrationDefaultImage is the default pinned image when
	// GHDC_TEST_IMAGE is unset (official runner image digest pin). It has
	// User=runner and /home/runner/run.sh, and exits immediately on an
	// invalid JIT config, so wait/log can be verified deterministically.
	integrationDefaultImage = "ghcr.io/actions/actions-runner@sha256:0cfdcc701ce933c6d243c6b0b2da767366dc9f2e99961d4c3754b0b78084cdda"

	// integrationScaleSetName is the test-only Scale Set name used to
	// generate container/runner names. The name is not the identity source
	// of truth; the labels and the runner ID are.
	integrationScaleSetName = "standard-integration-test"
)

// TestStandardLifecycle_ManagedBoundary verifies the configured HostConfig
// lifecycle and managed boundary against a real daemon. A missing daemon is
// a fail; every registered runtime becomes a subtest with runsc preferred,
// and each subtest uses unique managed labels for scale-set-id / runner-id.
func TestStandardLifecycle_ManagedBoundary(t *testing.T) {
	c, err := New(integrationHost, 2*time.Minute)
	if err != nil {
		// A missing daemon is an environment problem and an explicit fail.
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

	// Enumerate the registered runtimes with runsc preferred (runc is the
	// default in a standard setup).
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

// testStandardLifecycle verifies the lifecycle and managed boundary for one
// runtime. It creates exactly one container with unique managed labels and
// runs create/start/inspect/wait/log/cleanup through the production paths.
// The cleanup principle is the fresh managed guard path (CleanupManaged);
// only the abnormal case falls back to the test-only official SDK forced
// removal (cleanupManagedFixture).
func testStandardLifecycle(t *testing.T, c *Client, runtime string) {
	// Generate unique managed labels. Random values change every run, so
	// parallel runs and leftovers from the past cannot collide.
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

	// The unique scale-set-id must have no existing managed container (label
	// uniqueness check).
	items, err := c.ListManaged(t.Context(), scaleSetID)
	if err != nil {
		t.Fatalf("ListManaged が失敗しました: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("一意な scale-set-id %d に既存 container があります (label が一意でありません): %v", scaleSetID, items)
	}

	// The pinned image is given by env; nothing is built. if-not-present
	// pulls only when it is missing locally.
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

	// Create the unmanaged sentinel. Whole-daemon snapshot comparison would
	// contend with the fixtures of parallel packages, so it is not used.
	createUnmanagedSentinel(t, c, imageRef)

	// Build the immutable spec and create. Only the identity and name are
	// replaced with unique values; the rest uses the same standard settings
	// as the unit tests.
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

	// Never leave it behind: remove via the fresh managed guard path at test
	// end (cleanupManagedFixture). Only the abnormal case force-removes
	// after a fresh inspect of the test labels with the official SDK.
	t.Cleanup(func() {
		cleanupManagedFixture(t, c, containerID, identity)
	})

	// Verify right after create that the required 6 label keys really exist
	// on the daemon. model.ValidateLabels checks the presence and the exact
	// value match of the required keys. Config.Labels can merge
	// image-derived OCI labels, so exactly 6 labels total is not required
	// (extras are allowed); only the unique scale-set-id is additionally
	// checked.
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

	// Verify with inspect alone that the security fields really exist on the
	// daemon.
	verifyInspectHostConfig(t, inspect.Container, cfg, input)

	// The start uses the production path (StartManaged). Before that, verify
	// against the real daemon that a zero identity and an identity mismatch
	// (another runner-id) are rejected by the fresh guard and the container
	// is not started.
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
	// The rejected start was not executed; the state stays created.
	stateInspect, err := c.ContainerInspect(t.Context(), containerID, mobyclient.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("guard 拒否後の inspect が失敗しました: %v", err)
	}
	if got := string(stateInspect.Container.State.Status); got != string(container.StateCreated) {
		t.Fatalf("guard 拒否後に container が start されています: %q", got)
	}

	// StartManaged with the correct identity starts only after the fresh
	// inspect passes the full six-label validation. The production spec sets
	// the runner command explicitly, so the official image starts its runner.
	if _, err := c.StartManaged(t.Context(), containerID, identity); err != nil {
		t.Fatalf("StartManaged が失敗しました: %v", err)
	}

	// The explicit restart policy keeps this fixture running until cleanup.
	waitCtx, waitCancel := context.WithTimeout(t.Context(), 10*time.Second)
	waitResult, err := c.WaitContainer(waitCtx, containerID, mobyclient.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	waitCancel()
	if err != nil {
		t.Logf("WaitContainer が timeout/error しました (長時間実行 runner は valid なため、running のまま cleanup の stop 経路で検証します): %v", err)
	} else {
		t.Logf("container は exit しました: code=%d", waitResult.StatusCode)
	}

	// Fetch the bounded log. The content correctness depends on the
	// image/runtime, so it is not checked; only that the API completes
	// within the limits.
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

	// Managed boundary: the enumeration of the unique scale-set-id returns
	// only itself.
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

	// VerifyManaged passes the fresh inspect + label validation (the same
	// validation as the cleanup fresh managed guard).
	verified, err := c.VerifyManaged(t.Context(), containerID, identity)
	if err != nil {
		t.Fatalf("VerifyManaged が失敗しました: %v", err)
	}
	if verified.ID() != containerID {
		t.Fatalf("VerifyManaged が別の container を返しました: %q", verified.ID())
	}

	// CleanupManaged is the single production removal path (the simple
	// lifecycle that depends only on Docker). Internally it collects the
	// exit code / bounded logs after the fresh inspect + label re-match,
	// and stops first when still running.
	cleanupResult, err := c.CleanupManaged(t.Context(), verified, ManagedCleanupOptions{
		StopTimeout: time.Duration(cfg.Runner.StopTimeout),
	})
	if err != nil {
		t.Fatalf("CleanupManaged が失敗しました: %v", err)
	}
	if cleanupResult.ContainerID != containerID {
		t.Fatalf("CleanupManaged が別の container を返しました: %q", cleanupResult.ContainerID)
	}
	// An exited (or stopped) container must always have an observable exit
	// code.
	if !cleanupResult.HasExitCode {
		t.Fatalf("CleanupManaged が終了 code を観測できませんでした: %+v", cleanupResult)
	}
	if len(cleanupResult.Stdout) > 64*1024 || len(cleanupResult.Stderr) > 64*1024 {
		t.Fatalf("cleanup 時の log が上限を超えています: stdout=%d stderr=%d", len(cleanupResult.Stdout), len(cleanupResult.Stderr))
	}
	t.Logf("CleanupManaged が完了しました: exit-code=%d stdout=%d bytes stderr=%d bytes",
		cleanupResult.ExitCode, len(cleanupResult.Stdout), len(cleanupResult.Stderr))

	// No managed container may remain after the test. The
	// cleanupManagedFixture in t.Cleanup succeeds idempotently on a 404, so
	// it does not touch an already-removed container.
	items, err = c.ListManaged(t.Context(), scaleSetID)
	if err != nil {
		t.Fatalf("ListManaged が失敗しました: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("cleanup 後に managed container が残っています: %+v", items)
	}
}
