//go:build integration

// Recover integration tests use a real Docker daemon and cover managed,
// cross-Scale-Set, and unmanaged container boundaries. The GitHub API is not
// called; unique fixtures are removed after every test and no mocks are used.
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
	// Keep the integration target explicit; DOCKER_HOST is not used.
	integrationHost = "unix:///var/run/docker.sock"
	// The invalid JIT makes the official runner image exit deterministically.
	integrationDefaultImage = "ghcr.io/actions/actions-runner@sha256:0cfdcc701ce933c6d243c6b0b2da767366dc9f2e99961d4c3754b0b78084cdda"
	// This name is used only to construct fixture names.
	integrationScaleSetName = "scaler-integration-test"
)

// TestScalerRecover_ManagedBoundary covers Recover on a real daemon.
func TestScalerRecover_ManagedBoundary(t *testing.T) {
	c, err := docker.New(integrationHost, 2*time.Minute)
	if err != nil {
		t.Fatalf("Docker client を作成できませんでした (daemon 不在とみなして fail): %v", err)
	}
	defer c.Close()
	if _, err := c.Ping(t.Context()); err != nil {
		t.Fatalf("Docker daemon に接続できませんでした (daemon 不在とみなして fail): %v", err)
	}

	// Prefer runsc so this test also covers the production runtime path.
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

	// Allow local images while keeping the default reproducible.
	imageRef := os.Getenv("GHDC_TEST_IMAGE")
	if imageRef == "" {
		imageRef = integrationDefaultImage
	}
	pullCtx, pullCancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer pullCancel()
	if err := c.EnsureImage(pullCtx, imageRef, config.PullPolicyIfNotPresent); err != nil {
		t.Fatalf("image %s を用意できませんでした (GHDC_TEST_IMAGE で local の pinned image を指定できます): %v", imageRef, err)
	}

	// Create the unmanaged control with the raw SDK because CreateManaged adds labels.
	raw, err := mobyclient.New(mobyclient.WithHost(integrationHost), mobyclient.WithTimeout(2*time.Minute))
	if err != nil {
		t.Fatalf("raw Moby client を作成できませんでした: %v", err)
	}
	defer raw.Close()

	// Unique identities avoid collisions with parallel or stale fixtures.
	targetScaleSetID := rand.Int64N(1<<62) + 1
	runningRunnerID := rand.Int64N(1<<62) + 1
	exitedRunnerID := rand.Int64N(1<<62) + 1
	otherScaleSetID := rand.Int64N(1<<62) + 1
	otherRunnerID := rand.Int64N(1<<62) + 1
	runningIdentity, runningName := fixtureIdentity(t, targetScaleSetID, runningRunnerID)
	exitedIdentity, exitedName := fixtureIdentity(t, targetScaleSetID, exitedRunnerID)
	otherIdentity, otherName := fixtureIdentity(t, otherScaleSetID, otherRunnerID)

	// The constructor does not perform GitHub I/O.
	cfg := scalerTestConfig(runtime, imageRef)
	scalesetClient, err := scaleset.New(cfg, "9.9.9-test", "test-commit")
	if err != nil {
		t.Fatalf("scaleset client を dummy PAT で構築できませんでした: %v", err)
	}
	s, err := NewDockerScaler(context.Background(), c, scalesetClient, cfg, int(targetScaleSetID), "9.9.9-test", nil)
	if err != nil {
		t.Fatalf("NewDockerScaler が失敗しました: %v", err)
	}

	// Confirm the random identity does not collide with an existing fixture.
	items, err := c.ListManaged(t.Context(), targetScaleSetID)
	if err != nil {
		t.Fatalf("ListManaged が失敗しました: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("一意な scale-set-id %d に既存 container があります: %+v", targetScaleSetID, items)
	}
	// Let the exited fixture use Docker defaults; the running fixture overrides them.
	exitedCfg := *cfg
	exitedCfg.Runner = cfg.Runner
	exitedCfg.Runner.HostConfig = nil

	// Join watches before removing fixtures so cleanup cannot race with them.
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

	// The exited fixture must be present when Recover enumerates the daemon.
	fx.exited = createManagedFixture(t, c, &exitedCfg, exitedIdentity, exitedName)
	if _, err := c.StartManaged(t.Context(), fx.exited, exitedIdentity); err != nil {
		t.Logf("exited fixture の StartManaged が error を返しました: %v", err)
	}
	waitExited(t, c, fx.exited, exitedIdentity)

	// Control fixtures stay created and must remain untouched.
	fx.other = createManagedFixture(t, c, cfg, otherIdentity, otherName)
	otherState := containerState(t, c, fx.other)
	fx.unmanaged = createUnmanagedFixture(t, raw, imageRef)
	unmanagedState := containerState(t, c, fx.unmanaged)

	// Start this fixture last so Recover observes it as running.
	fx.running = createManagedFixture(t, c, cfg, runningIdentity, runningName)
	if _, err := c.StartManaged(t.Context(), fx.running, runningIdentity); err != nil {
		t.Fatalf("running fixture を start できませんでした: %v", err)
	}
	if err := s.Recover(t.Context()); err != nil {
		t.Fatalf("Recover が失敗しました: %v", err)
	}

	// Running fixtures are protected and counted.
	s.state.mu.Lock()
	_, runningProtected := s.state.protected[fx.running]
	protectedCount := len(s.state.protected)
	s.state.mu.Unlock()
	if !runningProtected || protectedCount != 1 || s.state.count() != 1 {
		t.Fatalf("Recover 後の state が不正です: protected 数=%d running 含む=%v count=%d (want 1/true/1)",
			protectedCount, runningProtected, s.state.count())
	}

	// Recover removes exited fixtures.
	if _, err := c.ContainerInspect(t.Context(), fx.exited, mobyclient.ContainerInspectOptions{}); !cerrdefs.IsNotFound(err) {
		t.Fatalf("exited fixture が除去されていません (inspect error=%v)", err)
	}

	// A different Scale Set is outside the list boundary.
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

	// Unmanaged containers are outside the controller boundary.
	if got := containerState(t, c, fx.unmanaged); got != unmanagedState {
		t.Fatalf("unmanaged fixture の状態が変化しました: %q → %q", unmanagedState, got)
	}

	// Stop the protected fixture through the production path.
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

	// The wait watch removes the stopped fixture asynchronously.
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

// scalerTestConfig builds an integration config with explicit host settings.
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
			PullPolicy: config.PullPolicyIfNotPresent,
		},
		Runner: config.RunnerConfig{
			Image: imageRef,
			HostConfig: &container.HostConfig{
				Runtime:       runtime,
				NetworkMode:   "bridge",
				RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyAlways},
			},
			ProvisioningTimeout: config.Duration(5 * time.Minute),
			StopTimeout:         config.Duration(30 * time.Second),
		},
		Shutdown: config.ShutdownConfig{BusyPolicy: config.ShutdownPolicyLeave},
	}
}

// fixtureIdentity generates a unique managed fixture identity.
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

// createManagedFixture uses the production spec and create path.
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

// waitExited waits for exit and stops images that do not exit naturally.
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

// createUnmanagedFixture creates a container outside the managed boundary.
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

// createRawManagedFixture creates malformed managed fixtures for recovery tests.
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

// TestScalerRecover_MalformedLabelShutdownJoinsWatch covers partial recovery.
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

	// Start a valid fixture before the malformed one to create a watch.
	running = createManagedFixture(t, c, cfg, runningIdentity, runningName)
	if _, err := c.StartManaged(t.Context(), running, runningIdentity); err != nil {
		t.Fatalf("running fixture を start できませんでした: %v", err)
	}
	// A non-integer runner ID must fail recovery without unsafe cleanup.
	malformed = createRawManagedFixture(t, raw, imageRef, map[string]string{
		model.ManagedLabelKey:    model.ManagedLabelValue,
		model.ScaleSetIDLabelKey: strconv.FormatInt(targetScaleSetID, 10),
		model.RunnerIDLabelKey:   "not-an-int",
		model.RunnerNameLabelKey: "malformed-runner",
	})

	if err := s.Recover(t.Context()); err == nil {
		t.Fatalf("malformed label があるのに Recover が error を返しませんでした")
	}

	// Partial recovery must still allow a bounded, joined shutdown.
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

// containerState returns a fixture state.
func containerState(t *testing.T, c *docker.Client, containerID string) string {
	t.Helper()
	inspect, err := c.ContainerInspect(t.Context(), containerID, mobyclient.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("container %s の inspect が失敗しました: %v", containerID, err)
	}
	return string(inspect.Container.State.Status)
}

// cleanupManagedFixture removes a managed fixture through the production path.
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

// removeUnmanagedFixture removes a fixture outside the managed path.
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
