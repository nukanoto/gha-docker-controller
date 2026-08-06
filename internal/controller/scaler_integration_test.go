//go:build integration

// scaler_integration_test.go is an integration test of DockerScaler.Recover
// against a real Docker daemon. Four fixtures verify the Recover boundary:
// a running/exited managed container of the target Scale Set, a managed
// container of another Scale Set, and an unmanaged container. Running is
// protected and included in the current count, exited is cleaned up, and the
// others are unchanged. Finally the running fixture is stopped, and removal by
// the production wait watch -> CleanupManaged path plus the count decrease are
// verified by polling.
//
// The GitHub API is never called; the concrete scaleset.Client for the
// constructor is built with a dummy PAT only (the constructor does no I/O).
// Fixtures use test-unique labels and are always removed at the end. A missing
// daemon fails the test; no mocks/stubs/fakes. Existing Docker integration
// helpers are in another package, so they are not shared.
package controller

import (
	"context"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"

	cerrdefs "github.com/containerd/errdefs"

	"github.com/nukanoto/gha-docker-controller/internal/config"
	"github.com/nukanoto/gha-docker-controller/internal/docker"
	"github.com/nukanoto/gha-docker-controller/internal/model"
	"github.com/nukanoto/gha-docker-controller/internal/scaleset"
)

const (
	// integrationHost is the host of the real Docker daemon (unix:// absolute path contract).
	integrationHost = "unix:///var/run/docker.sock"
	// integrationDefaultImage is the default pinned image when GHDC_TEST_IMAGE
	// is unset. It exits immediately on an invalid JIT config, so the natural
	// exit of fixtures is observed deterministically.
	integrationDefaultImage = "ghcr.io/actions/actions-runner@sha256:0cfdcc701ce933c6d243c6b0b2da767366dc9f2e99961d4c3754b0b78084cdda"
	// integrationScaleSetName is the test-only name used for container/runner names.
	integrationScaleSetName = "scaler-integration-test"
)

// TestScalerRecover_ManagedBoundary verifies the protected/cleanup boundary
// of Recover on a real daemon. The identity uses test-unique random values,
// so parallel runs and leftover containers from past runs cannot collide. The
// default image exits naturally ~0.5 s after start, so the running fixture is
// started last and Recover is called immediately after.
func TestScalerRecover_ManagedBoundary(t *testing.T) {
	c, err := docker.New(integrationHost, 2*time.Minute)
	if err != nil {
		t.Fatalf("Docker client を作成できませんでした (daemon 不在とみなして fail): %v", err)
	}
	defer c.Close()
	if _, err := c.Ping(t.Context()); err != nil {
		t.Fatalf("Docker daemon に接続できませんでした (daemon 不在とみなして fail): %v", err)
	}

	// Pick a registered runtime, preferring runsc. Fail if neither exists.
	info, err := c.Info(t.Context())
	if err != nil {
		t.Fatalf("Docker Info を取得できませんでした: %v", err)
	}
	runtime := "runc"
	if _, ok := info.Info.Runtimes["runsc"]; ok {
		runtime = "runsc"
	}
	if _, ok := info.Info.Runtimes[runtime]; !ok {
		t.Fatalf("Docker daemon に runsc も runc も登録されていません: %v", info.Info.Runtimes)
	}

	// The pinned image can be overridden by env; otherwise use the official
	// runner digest pin.
	imageRef := os.Getenv("GHDC_TEST_IMAGE")
	if imageRef == "" {
		imageRef = integrationDefaultImage
	}
	pullCtx, pullCancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer pullCancel()
	if err := c.EnsureImage(pullCtx, imageRef, config.PullPolicyIfNotPresent); err != nil {
		t.Fatalf("image %s を用意できませんでした (GHDC_TEST_IMAGE で local の pinned image を指定できます): %v", imageRef, err)
	}

	// The unmanaged fixture cannot be made via CreateManaged (it forces managed
	// labels), so create it with a test-only raw client of the official SDK
	// (real Moby SDK).
	raw, err := mobyclient.New(mobyclient.WithHost(integrationHost), mobyclient.WithTimeout(2*time.Minute))
	if err != nil {
		t.Fatalf("raw Moby client を作成できませんでした: %v", err)
	}
	defer raw.Close()

	// Generate unique labels.
	targetScaleSetID := rand.Int64N(1<<62) + 1
	runningRunnerID := rand.Int64N(1<<62) + 1
	exitedRunnerID := rand.Int64N(1<<62) + 1
	otherScaleSetID := rand.Int64N(1<<62) + 1
	otherRunnerID := rand.Int64N(1<<62) + 1
	runningIdentity, runningName := fixtureIdentity(t, targetScaleSetID, runningRunnerID)
	exitedIdentity, exitedName := fixtureIdentity(t, targetScaleSetID, exitedRunnerID)
	otherIdentity, otherName := fixtureIdentity(t, otherScaleSetID, otherRunnerID)

	// Build the concrete scaleset.Client with a dummy PAT (GitHub API is never called).
	cfg := scalerTestConfig(runtime, imageRef)
	scalesetClient, err := scaleset.New(cfg, "9.9.9-test", "test-commit")
	if err != nil {
		t.Fatalf("scaleset client を dummy PAT で構築できませんでした: %v", err)
	}
	s, err := NewDockerScaler(context.Background(), c, scalesetClient, cfg, int(targetScaleSetID), "9.9.9-test", nil)
	if err != nil {
		t.Fatalf("NewDockerScaler が失敗しました: %v", err)
	}

	// The unique target scale set must have no existing managed containers
	// (confirms label uniqueness).
	items, err := c.ListManaged(t.Context(), targetScaleSetID)
	if err != nil {
		t.Fatalf("ListManaged が失敗しました: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("一意な scale-set-id %d に既存 container があります: %+v", targetScaleSetID, items)
	}

	// Never leave anything behind: cleanup uses individual registrations (LIFO)
	// so one failure does not interrupt the others. First Shutdown joins the
	// wait goroutines (removes concurrent cleanup races), then fixtures are
	// removed, and finally zero managed leftovers are confirmed.
	var fx struct {
		running, exited, other, unmanaged string
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		items, err := c.ListManaged(ctx, targetScaleSetID)
		if err != nil {
			t.Fatalf("cleanup 後の ListManaged が失敗しました: %v", err)
		}
		if len(items) != 0 {
			t.Fatalf("cleanup 後に managed container が残っています: %+v", items)
		}
	})
	t.Cleanup(func() { removeUnmanagedFixture(t, raw, fx.unmanaged) })
	t.Cleanup(func() { cleanupManagedFixture(t, c, fx.other, otherIdentity) })
	t.Cleanup(func() { cleanupManagedFixture(t, c, fx.exited, exitedIdentity) })
	t.Cleanup(func() { cleanupManagedFixture(t, c, fx.running, runningIdentity) })
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown が error を返しました: %v", err)
		}
	})

	// exited fixture: create and start it, then wait for exit (the default
	// image exits immediately; a non-exiting override image is moved to exited
	// via managed stop).
	fx.exited = createManagedFixture(t, c, cfg, exitedIdentity, exitedName)
	if _, err := c.StartManaged(t.Context(), fx.exited, exitedIdentity); err != nil {
		// A start error still moves to exited/created, both Recover cleanup targets.
		t.Logf("exited fixture の StartManaged が error を返しました: %v", err)
	}
	waitExited(t, c, fx.exited, exitedIdentity)

	// The other-scale managed and unmanaged fixtures stay created, not started.
	fx.other = createManagedFixture(t, c, cfg, otherIdentity, otherName)
	otherState := containerState(t, c, fx.other)
	fx.unmanaged = createUnmanagedFixture(t, raw, imageRef)
	unmanagedState := containerState(t, c, fx.unmanaged)

	// The running fixture is started last, and Recover is called immediately
	// after (deterministic running observation before natural exit).
	fx.running = createManagedFixture(t, c, cfg, runningIdentity, runningName)
	if _, err := c.StartManaged(t.Context(), fx.running, runningIdentity); err != nil {
		t.Fatalf("running fixture を start できませんでした: %v", err)
	}
	if err := s.Recover(t.Context()); err != nil {
		t.Fatalf("Recover が失敗しました: %v", err)
	}

	// 1. running is protected and included in the current count (immediate check).
	s.state.mu.Lock()
	_, runningProtected := s.state.protected[fx.running]
	protectedCount := len(s.state.protected)
	s.state.mu.Unlock()
	if !runningProtected || protectedCount != 1 || s.state.count() != 1 {
		t.Fatalf("Recover 後の state が不正です: protected 数=%d running 含む=%v count=%d (want 1/true/1)",
			protectedCount, runningProtected, s.state.count())
	}

	// 2. The exited fixture has been removed.
	if _, err := c.ContainerInspect(t.Context(), fx.exited, mobyclient.ContainerInspectOptions{}); !cerrdefs.IsNotFound(err) {
		t.Fatalf("exited fixture が除去されていません (inspect error=%v)", err)
	}

	// 3. The other-scale managed container keeps its managed labels and its
	// state is unchanged (ListManaged's filter boundary never touches it).
	otherInspect, err := c.ContainerInspect(t.Context(), fx.other, mobyclient.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("別 Scale Set fixture の inspect が失敗しました: %v", err)
	}
	labels := otherInspect.Container.Config.Labels
	if labels[model.ManagedLabelKey] != model.ManagedLabelValue ||
		labels[model.ScaleSetIDLabelKey] != strconv.FormatInt(otherScaleSetID, 10) {
		t.Fatalf("別 Scale Set fixture の managed label が不正です: %v", labels)
	}
	if got := containerState(t, c, fx.other); got != otherState {
		t.Fatalf("別 Scale Set fixture の状態が変化しました: %q → %q", otherState, got)
	}

	// 4. The unmanaged container's ID/state are unchanged.
	if got := containerState(t, c, fx.unmanaged); got != unmanagedState {
		t.Fatalf("unmanaged fixture の状態が変化しました: %q → %q", unmanagedState, got)
	}

	// 5. Stop the running fixture with the production managed stop (if already
	// removed, VerifyManaged returns 404, which counts as success).
	runningMC, err := c.VerifyManaged(t.Context(), fx.running, runningIdentity)
	if err != nil {
		if !cerrdefs.IsNotFound(err) {
			t.Fatalf("running fixture の VerifyManaged が失敗しました: %v", err)
		}
	} else {
		timeout := 5
		if _, err := c.StopManaged(t.Context(), runningMC, runningIdentity, mobyclient.ContainerStopOptions{Timeout: &timeout}); err != nil && !cerrdefs.IsNotFound(err) {
			t.Fatalf("running fixture の StopManaged が失敗しました: %v", err)
		}
	}

	// 6. Poll until the wait watch observes the exit, removes via CleanupManaged,
	// the count decreases, and zero managed containers remain (removal runs in
	// a goroutine, so also wait via ListManaged).
	deadline := time.Now().Add(2 * time.Minute)
	for {
		items, err := c.ListManaged(t.Context(), targetScaleSetID)
		if err != nil {
			t.Fatalf("ListManaged が失敗しました: %v", err)
		}
		if s.state.count() == 0 && len(items) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("wait 監視による除去を待てませんでした (count=%d managed=%d)", s.state.count(), len(items))
		}
		time.Sleep(100 * time.Millisecond)
	}
	select {
	case waitErr := <-s.ErrCh():
		t.Fatalf("wait 監視が error を通知しました: %v", waitErr)
	default:
	}
}

// scalerTestConfig builds a standard profile config (GitHub is dummy PAT only).
func scalerTestConfig(runtime, imageRef string) *config.Config {
	return &config.Config{
		GitHub: config.GitHubConfig{
			URL:   "https://github.com",
			Scope: config.ScopeOrganization,
			Owner: "ghadc-integration-test",
			Token: "dummy-pat-for-constructor-only",
		},
		ScaleSet: config.ScaleSetConfig{Name: integrationScaleSetName, RunnerGroup: "default", MinRunners: 0, MaxRunners: 4},
		Docker: config.DockerConfig{
			Host:       integrationHost,
			Runtime:    runtime,
			Network:    "bridge",
			PullPolicy: config.PullPolicyIfNotPresent,
		},
		Runner: config.RunnerConfig{
			Image:               imageRef,
			Profile:             config.ProfileStandard,
			CPU:                 config.NanoCPUs(2e9),
			Memory:              config.Memory(4 * 1024 * 1024 * 1024),
			MemorySwap:          config.Memory(6 * 1024 * 1024 * 1024),
			PidsLimit:           512,
			ProvisioningTimeout: config.Duration(5 * time.Minute),
			StopTimeout:         config.Duration(30 * time.Second),
			CapDrop:             []string{"ALL"},
			NoNewPrivileges:     true,
			Network:             "bridge",
		},
		DindRunner: config.DindRunnerConfig{StorageSize: config.DefaultDindStorageSize},
		Shutdown:   config.ShutdownConfig{BusyPolicy: config.ShutdownPolicyLeave},
	}
}

// fixtureIdentity generates the identity and container name of a unique
// managed fixture.
func fixtureIdentity(t *testing.T, scaleSetID, runnerID int64) (model.RunnerIdentity, string) {
	t.Helper()
	suffix := strconv.FormatInt(runnerID, 16)
	runnerName := model.RunnerName(integrationScaleSetName, suffix)
	containerName := model.ContainerName(integrationScaleSetName, runnerID, suffix)
	if !model.ValidRunnerName(runnerName) || len(containerName) > 63 {
		t.Fatalf("生成した runner/container name が契約外です: runner=%q container=%q", runnerName, containerName)
	}
	return model.RunnerIdentity{ScaleSetID: scaleSetID, RunnerID: runnerID, RunnerName: runnerName}, containerName
}

// createManagedFixture creates a managed fixture through the production path
// (BuildManagedSpec + CreateManaged).
func createManagedFixture(t *testing.T, c *docker.Client, cfg *config.Config, identity model.RunnerIdentity, containerName string) string {
	t.Helper()
	input := docker.ManagedSpecInput{
		Config:             cfg,
		Identity:           identity,
		JITConfig:          "integration-test-invalid-jit-config",
		ControllerInstance: "scaler-integration-test",
		CreatedAt:          time.Now().UTC(),
		ContainerName:      containerName,
		UserAgentVersion:   "9.9.9-test",
	}
	spec, err := docker.BuildManagedSpec(input)
	if err != nil {
		t.Fatalf("BuildManagedSpec が失敗しました: %v", err)
	}
	created, err := c.CreateManaged(t.Context(), spec)
	if err != nil {
		t.Fatalf("CreateManaged が失敗しました: %v", err)
	}
	if created.ID == "" {
		t.Fatalf("CreateManaged が空の container ID を返しました")
	}
	return created.ID
}

// waitExited waits for the managed fixture to exit (an override image that
// does not exit within 60 s is moved to exited via managed stop).
func waitExited(t *testing.T, c *docker.Client, containerID string, identity model.RunnerIdentity) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	_, err := c.WaitContainer(waitCtx, containerID, mobyclient.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	cancel()
	if err == nil {
		return
	}
	stopCtx, stopCancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer stopCancel()
	mc, err := c.VerifyManaged(stopCtx, containerID, identity)
	if err != nil {
		t.Fatalf("exited fixture の VerifyManaged が失敗しました: %v", err)
	}
	timeout := 5
	if _, err := c.StopManaged(stopCtx, mc, identity, mobyclient.ContainerStopOptions{Timeout: &timeout}); err != nil {
		t.Fatalf("exited fixture の StopManaged が失敗しました: %v", err)
	}
	if _, err := c.WaitContainer(stopCtx, containerID, mobyclient.ContainerWaitOptions{Condition: container.WaitConditionNotRunning}); err != nil {
		t.Fatalf("exited fixture を終了できませんでした: %v", err)
	}
}

// createUnmanagedFixture creates a container without managed labels via the
// raw client of the official SDK.
func createUnmanagedFixture(t *testing.T, raw *mobyclient.Client, imageRef string) string {
	t.Helper()
	created, err := raw.ContainerCreate(t.Context(), mobyclient.ContainerCreateOptions{
		Config: &container.Config{
			Image: imageRef,
			Cmd:   []string{"/bin/sleep", "600"},
		},
	})
	if err != nil {
		t.Fatalf("unmanaged fixture の ContainerCreate が失敗しました: %v", err)
	}
	return created.ID
}

// createRawManagedFixture creates a container with the given labels via the
// raw client of the official SDK. Production CreateManaged cannot produce
// invalid labels, so it is used to build malformed managed fixtures.
func createRawManagedFixture(t *testing.T, raw *mobyclient.Client, imageRef string, labels map[string]string) string {
	t.Helper()
	created, err := raw.ContainerCreate(t.Context(), mobyclient.ContainerCreateOptions{
		Config: &container.Config{
			Image:  imageRef,
			Cmd:    []string{"/bin/sleep", "600"},
			Labels: labels,
		},
	})
	if err != nil {
		t.Fatalf("raw ContainerCreate が失敗しました: %v", err)
	}
	return created.ID
}

// TestScalerRecover_MalformedLabelShutdownJoinsWatch verifies on a real
// daemon that, even when Recover returns an error at a later malformed
// container, Shutdown joins and completes the watches started before that.
// A running fixture is created first to start protected + watch; then a
// malformed fixture (non-integer runner-id label) makes Recover fail. It
// confirms Shutdown returns within a bounded context (no watch-join
// deadlock) and that no shutdown-originated fatal reaches ErrCh.
func TestScalerRecover_MalformedLabelShutdownJoinsWatch(t *testing.T) {
	c, err := docker.New(integrationHost, 2*time.Minute)
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
	runtime := "runc"
	if _, ok := info.Info.Runtimes["runsc"]; ok {
		runtime = "runsc"
	}
	if _, ok := info.Info.Runtimes[runtime]; !ok {
		t.Fatalf("Docker daemon に runsc も runc も登録されていません: %v", info.Info.Runtimes)
	}

	imageRef := os.Getenv("GHDC_TEST_IMAGE")
	if imageRef == "" {
		imageRef = integrationDefaultImage
	}
	pullCtx, pullCancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer pullCancel()
	if err := c.EnsureImage(pullCtx, imageRef, config.PullPolicyIfNotPresent); err != nil {
		t.Fatalf("image %s を用意できませんでした (GHDC_TEST_IMAGE で local の pinned image を指定できます): %v", imageRef, err)
	}

	raw, err := mobyclient.New(mobyclient.WithHost(integrationHost), mobyclient.WithTimeout(2*time.Minute))
	if err != nil {
		t.Fatalf("raw Moby client を作成できませんでした: %v", err)
	}
	defer raw.Close()

	targetScaleSetID := rand.Int64N(1<<62) + 1
	runningRunnerID := rand.Int64N(1<<62) + 1
	runningIdentity, runningName := fixtureIdentity(t, targetScaleSetID, runningRunnerID)

	cfg := scalerTestConfig(runtime, imageRef)
	scalesetClient, err := scaleset.New(cfg, "9.9.9-test", "test-commit")
	if err != nil {
		t.Fatalf("scaleset client を dummy PAT で構築できませんでした: %v", err)
	}
	s, err := NewDockerScaler(context.Background(), c, scalesetClient, cfg, int(targetScaleSetID), "9.9.9-test", nil)
	if err != nil {
		t.Fatalf("NewDockerScaler が失敗しました: %v", err)
	}

	var running, malformed string
	t.Cleanup(func() {
		removeUnmanagedFixture(t, raw, malformed)
	})
	t.Cleanup(func() {
		cleanupManagedFixture(t, c, running, runningIdentity)
	})

	// Create and start the running fixture first, aiming for Recover to start
	// protected + watch. The default image exits immediately, so call Recover
	// right after start.
	running = createManagedFixture(t, c, cfg, runningIdentity, runningName)
	if _, err := c.StartManaged(t.Context(), running, runningIdentity); err != nil {
		t.Fatalf("running fixture を start できませんでした: %v", err)
	}
	// malformed fixture: the runner-id label is a non-integer, so
	// RefreshManaged's identityFromObserved returns ManagedGuardError.
	malformed = createRawManagedFixture(t, raw, imageRef, map[string]string{
		model.ManagedLabelKey:    model.ManagedLabelValue,
		model.ScaleSetIDLabelKey: strconv.FormatInt(targetScaleSetID, 10),
		model.RunnerIDLabelKey:   "not-an-int",
		model.RunnerNameLabelKey: "malformed-runner",
	})

	if err := s.Recover(t.Context()); err == nil {
		t.Fatalf("malformed label があるのに Recover が error を返しませんでした")
	}

	// Even after the mid-way failure, Shutdown joins the watches and completes
	// within a bounded context. Join timeout and cleanup failures return as
	// errors, and nothing is sent to ErrCh either.
	sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := s.Shutdown(sctx); err != nil {
		t.Fatalf("Shutdown が error を返しました: %v", err)
	}
	scancel()
	select {
	case waitErr := <-s.ErrCh():
		t.Fatalf("shutdown が error を通知しました: %v", waitErr)
	default:
	}
}

// containerState returns the current state of the container. Missing
// containers fail the test.
func containerState(t *testing.T, c *docker.Client, containerID string) string {
	t.Helper()
	inspect, err := c.ContainerInspect(t.Context(), containerID, mobyclient.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("container %s の inspect が失敗しました: %v", containerID, err)
	}
	return string(inspect.Container.State.Status)
}

// cleanupManagedFixture removes a test-created managed container via the
// production removal path (VerifyManaged + CleanupManaged, which always
// re-checks labels freshly). 404 counts as success. It uses its own bounded
// context because it runs from cleanup.
func cleanupManagedFixture(t *testing.T, c *docker.Client, containerID string, identity model.RunnerIdentity) {
	t.Helper()
	if containerID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	mc, err := c.VerifyManaged(ctx, containerID, identity)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return
		}
		t.Fatalf("managed fixture の VerifyManaged が失敗しました: %v", err)
	}
	if _, err := c.CleanupManaged(ctx, mc, docker.ManagedCleanupOptions{StopTimeout: 30 * time.Second}); err != nil && !cerrdefs.IsNotFound(err) {
		t.Fatalf("managed fixture の CleanupManaged が失敗しました: %v", err)
	}
}

// removeUnmanagedFixture removes a test-created unmanaged container via the
// official SDK.
func removeUnmanagedFixture(t *testing.T, raw *mobyclient.Client, containerID string) {
	t.Helper()
	if containerID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := raw.ContainerRemove(ctx, containerID, mobyclient.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !cerrdefs.IsNotFound(err) {
		t.Fatalf("unmanaged fixture の削除に失敗しました: %v", err)
	}
}
