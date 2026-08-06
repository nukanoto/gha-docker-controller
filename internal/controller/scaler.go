// Package controller provides a thin Docker scaler that implements the
// official listener.Scaler. In-process state is only the minimal set of
// idle/busy and protected.
package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	scalesetapi "github.com/actions/scaleset"
	listenerapi "github.com/actions/scaleset/listener"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"

	"github.com/nukanoto/gha-docker-controller/internal/config"
	"github.com/nukanoto/gha-docker-controller/internal/docker"
	"github.com/nukanoto/gha-docker-controller/internal/model"
	"github.com/nukanoto/gha-docker-controller/internal/scaleset"
)

// DockerScaler implements the official listener.Scaler. Desired is the single
// formula clamp(max(minRunners, TotalAssignedJobs), 0, maxRunners). Shortage is
// provisioned one by one; surplus is scaled down oldest idle first.
type DockerScaler struct {
	// runCtx is the run context of serve. It stops new provisioning when cancelled.
	runCtx              context.Context
	dockerClient        *docker.Client
	scalesetClient      *scaleset.Client
	config              *config.Config
	scaleSetID          int
	scaleSetName        string
	minRunners          int
	maxRunners          int
	stopTimeout         time.Duration
	provisioningTimeout time.Duration
	cleanupTimeout      time.Duration
	busyPolicy          string
	version             string
	controllerInstance  string
	logger              *slog.Logger
	errCh               chan error
	// watchCtx is the parent context of wait goroutines. Shutdown cancels and joins it.
	watchCtx    context.Context
	watchCancel context.CancelFunc
	wg          sync.WaitGroup
	// watchMu and watchStopped prevent a race between startWatch's wg.Add and
	// Shutdown's wg.Wait. Later startWatch calls do not call wg.Add.
	watchMu      sync.Mutex
	watchStopped bool
	state        runnerState
}

var _ listenerapi.Scaler = (*DockerScaler)(nil)

// NewDockerScaler builds a DockerScaler. Nil args, a non-positive scaleSetID,
// and an empty version are errors.
func NewDockerScaler(runCtx context.Context, dockerClient *docker.Client, scalesetClient *scaleset.Client,
	cfg *config.Config, scaleSetID int, version string, logger *slog.Logger) (*DockerScaler, error) {
	if runCtx == nil {
		return nil, errors.New("controller: nil run context")
	}
	if dockerClient == nil {
		return nil, errors.New("controller: nil docker client")
	}
	if scalesetClient == nil {
		return nil, errors.New("controller: nil scaleset client")
	}
	if cfg == nil {
		return nil, errors.New("controller: nil config")
	}
	if scaleSetID <= 0 {
		return nil, fmt.Errorf("controller: scale set ID must be positive, got %d", scaleSetID)
	}
	if version == "" {
		return nil, errors.New("controller: version is required")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	instance, err := newRandomHex(16)
	if err != nil {
		return nil, fmt.Errorf("controller: generate controller instance: %w", err)
	}
	cleanupTimeout := time.Duration(cfg.Shutdown.Grace)
	if cleanupTimeout <= 0 {
		cleanupTimeout = time.Duration(config.DefaultShutdownGrace)
	}
	watchCtx, watchCancel := context.WithCancel(context.Background())
	return &DockerScaler{
		runCtx:              runCtx,
		dockerClient:        dockerClient,
		scalesetClient:      scalesetClient,
		config:              cfg,
		scaleSetID:          scaleSetID,
		scaleSetName:        cfg.ScaleSet.Name,
		minRunners:          cfg.ScaleSet.MinRunners,
		maxRunners:          cfg.ScaleSet.MaxRunners,
		stopTimeout:         time.Duration(cfg.Runner.StopTimeout),
		provisioningTimeout: time.Duration(cfg.Runner.ProvisioningTimeout),
		cleanupTimeout:      cleanupTimeout,
		busyPolicy:          cfg.Shutdown.BusyPolicy,
		version:             version,
		controllerInstance:  instance,
		logger:              logger,
		errCh:               make(chan error, 1),
		watchCtx:            watchCtx,
		watchCancel:         watchCancel,
		state:               newRunnerState(),
	}, nil
}

// HandleDesiredRunnerCount scales idle runners toward the requested count.
func (s *DockerScaler) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	desired := desiredRunnerCount(s.minRunners, s.maxRunners, count)
	current := s.state.count()

	switch {
	case desired == current:
		return current, nil
	case desired > current:
		if s.runCtx.Err() != nil {
			return current, nil
		}
		s.logger.Info("Scaling up runners",
			slog.Int("currentCount", current), slog.Int("desiredCount", desired), slog.Int("scaleUp", desired-current))
		for range desired - current {
			if err := s.provision(ctx); err != nil {
				// Return the current count on failure. Provision only grows
				// state on success, so the count is the source of truth.
				return s.state.count(), fmt.Errorf("failed to start runner: %w", err)
			}
		}
		return s.state.count(), nil
	default:
		s.logger.Info("Scaling down runners",
			slog.Int("currentCount", current), slog.Int("desiredCount", desired), slog.Int("scaleDown", current-desired))
		// The listener ctx uses WithoutCancel, so use one fresh context shared by all scale-downs.
		cctx, ccancel := s.cleanupContext()
		defer ccancel()
		for _, ref := range s.state.scaleDownIdle(current - desired) {
			if err := s.removeRunner(cctx, ref); err != nil {
				// Return the current count, which excludes refs already removed by scaleDownIdle.
				return s.state.count(), fmt.Errorf("failed to remove runner: %w", err)
			}
		}
		return s.state.count(), nil
	}
}

// HandleJobStarted implements the official listener.Scaler. It moves a known
// idle runner to busy; a nil event is a fixed error.
func (s *DockerScaler) HandleJobStarted(ctx context.Context, jobInfo *scalesetapi.JobStarted) error {
	if jobInfo == nil {
		return errors.New("controller: nil job started event")
	}
	s.state.markBusy(jobInfo.RunnerName)
	s.logger.Info("Job started",
		slog.Int64("runnerRequestId", jobInfo.RunnerRequestID), slog.String("jobId", jobInfo.JobID),
		slog.String("runnerName", jobInfo.RunnerName))
	return nil
}

// HandleJobCompleted implements the official listener.Scaler. It takes
// cleanup ownership atomically, removes the container, and treats a nil event
// as a fixed error.
func (s *DockerScaler) HandleJobCompleted(ctx context.Context, jobInfo *scalesetapi.JobCompleted) error {
	if jobInfo == nil {
		return errors.New("controller: nil job completed event")
	}
	s.logger.Info("Job completed",
		slog.Int64("runnerRequestId", jobInfo.RunnerRequestID), slog.String("jobId", jobInfo.JobID),
		slog.String("runnerName", jobInfo.RunnerName))
	ref, ok := s.state.takeOwnership(jobInfo.RunnerName)
	if !ok {
		return nil
	}
	cctx, ccancel := s.cleanupContext()
	defer ccancel()
	if err := s.removeRunner(cctx, ref); err != nil {
		return fmt.Errorf("failed to remove runner container: %w", err)
	}
	return nil
}

// provision creates one runner. It is a one-way flow: JIT generation, spec
// build, create, start, idle registration, and wait watch. provisioningTimeout
// bounds completion.
func (s *DockerScaler) provision(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.provisioningTimeout)
	defer cancel()

	suffix, err := newRandomHex(16)
	if err != nil {
		return fmt.Errorf("generate runner name suffix: %w", err)
	}
	name := model.RunnerName(s.scaleSetName, suffix)

	jit, err := s.scalesetClient.GenerateJitRunnerConfig(ctx, name, s.scaleSetID)
	if err != nil {
		return fmt.Errorf("generate JIT config: %w", err)
	}
	identity := model.RunnerIdentity{
		ScaleSetID: int64(s.scaleSetID),
		RunnerID:   jit.RunnerID,
		RunnerName: jit.RunnerName,
	}
	spec, err := docker.BuildManagedSpec(docker.ManagedSpecInput{
		Config:             s.config,
		Identity:           identity,
		JITConfig:          jit.Encoded,
		ControllerInstance: s.controllerInstance,
		CreatedAt:          time.Now().UTC(),
		ContainerName:      model.ContainerName(s.scaleSetName, jit.RunnerID, suffix),
		UserAgentVersion:   s.version,
	})
	if err != nil {
		return fmt.Errorf("build managed spec: %w", err)
	}
	created, err := s.dockerClient.CreateManaged(ctx, spec)
	if err != nil {
		// Even if the create response is an error or timeout, the daemon may
		// have created it; do not re-list to recover it.
		return fmt.Errorf("create runner container: %w", err)
	}
	// start runs only after a fresh inspect passes full validation of the 6 required labels.
	if _, err := s.dockerClient.StartManaged(ctx, created.ID, identity); err != nil {
		s.cleanupAfterProvisionFailure(created.ID, identity)
		return fmt.Errorf("start runner container: %w", err)
	}
	ref := runnerRef{containerID: created.ID, runnerID: jit.RunnerID, runnerName: jit.RunnerName}
	s.state.addIdle(ref)
	s.startWatch(ref, false)
	return nil
}

// cleanupAfterProvisionFailure removes the created container on start failure
// as far as possible. The provision ctx may have reached its deadline, so
// cleanup uses a fresh context derived from Background.
func (s *DockerScaler) cleanupAfterProvisionFailure(containerID string, identity model.RunnerIdentity) {
	cctx, ccancel := s.cleanupContext()
	defer ccancel()
	mc, err := s.dockerClient.VerifyManaged(cctx, containerID, identity)
	if err != nil {
		return
	}
	if _, err := s.dockerClient.CleanupManaged(cctx, mc, docker.ManagedCleanupOptions{StopTimeout: s.stopTimeout}); err != nil {
		return
	}
}

// removeRunner verifies the managed labels freshly, then removes the
// container. A 404 counts as success because the state is already gone.
func (s *DockerScaler) removeRunner(ctx context.Context, ref runnerRef) error {
	mc, err := s.dockerClient.VerifyManaged(ctx, ref.containerID, ref.identity(s.scaleSetID))
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil
		}
		return err
	}
	if _, err := s.dockerClient.CleanupManaged(ctx, mc, docker.ManagedCleanupOptions{StopTimeout: s.stopTimeout}); err != nil {
		return err
	}
	return nil
}

// Recover lists the managed containers of the target Scale Set at serve
// startup and protects or cleans them up without guessing state. Call it
// before listener.Run. running/paused/restarting/unknown states are protected;
// created/exited/dead are cleaned up.
func (s *DockerScaler) Recover(ctx context.Context) error {
	containers, err := s.dockerClient.ListManaged(ctx, int64(s.scaleSetID))
	if err != nil {
		return err
	}
	// Cleanup uses one fresh context derived from Background, shared by all
	// targets, instead of the startup ctx of listing and inspect. This keeps
	// startup cancellation from interrupting cleanup.
	cctx, ccancel := s.cleanupContext()
	defer ccancel()
	for _, mc := range containers {
		// Re-check the value observed at listing time with a fresh inspect.
		// Malformed containers return an error without changing the container.
		refreshed, err := s.dockerClient.RefreshManaged(ctx, mc)
		if err != nil {
			if cerrdefs.IsNotFound(err) {
				continue
			}
			return err
		}
		ref, err := runnerRefFromManaged(refreshed)
		if err != nil {
			return err
		}
		switch refreshed.State() {
		case container.StateCreated, container.StateExited, container.StateDead:
			if _, err := s.dockerClient.CleanupManaged(cctx, refreshed, docker.ManagedCleanupOptions{StopTimeout: s.stopTimeout}); err != nil {
				return err
			}
		default:
			s.state.addProtected(ref)
			s.startWatch(ref, true)
		}
	}
	return nil
}

// runnerRefFromManaged restores a runnerRef from the labels of a managed
// container. The runner-id label must be a positive base-10 integer.
func runnerRefFromManaged(mc docker.ManagedContainer) (runnerRef, error) {
	return runnerRefFromLabels(mc.ID(), mc.Labels())
}

func runnerRefFromLabels(id string, labels map[string]string) (runnerRef, error) {
	runnerID, err := strconv.ParseInt(labels[model.RunnerIDLabelKey], 10, 64)
	if err != nil {
		return runnerRef{}, fmt.Errorf("recover: runner-id label of container %s is not a base-10 integer: %w", id, err)
	}
	if runnerID <= 0 {
		return runnerRef{}, fmt.Errorf("recover: runner-id label of container %s must be positive, got %d", id, runnerID)
	}
	return runnerRef{
		containerID: id,
		runnerID:    runnerID,
		runnerName:  labels[model.RunnerNameLabelKey],
	}, nil
}

// startWatch starts a goroutine that watches the container exit. On exit it
// takes cleanup ownership atomically, removes the container, and reports
// non-context errors to errCh.
func (s *DockerScaler) startWatch(ref runnerRef, protected bool) {
	s.watchMu.Lock()
	if s.watchStopped {
		// Do not add new watches after shutdown starts. The same critical
		// section on watchStopped prevents concurrent wg.Add and wg.Wait.
		s.watchMu.Unlock()
		return
	}
	s.wg.Add(1)
	s.watchMu.Unlock()
	go func() {
		defer s.wg.Done()
		_, err := s.dockerClient.WaitContainer(s.watchCtx, ref.containerID,
			mobyclient.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
		if err != nil {
			if s.watchCtx.Err() != nil {
				return
			}
			if cerrdefs.IsNotFound(err) {
				// The container was removed externally or by another cleanup.
				// Release in-process state so the missing container is not
				// counted in capacity.
				s.releaseWatchOwnership(ref, protected)
				return
			}
			s.notifyError(fmt.Errorf("wait container %s: %w", ref.containerID, err))
			return
		}
		// Exit: take ownership atomically, then clean up. If it cannot be
		// taken, another path already handled it.
		if !s.releaseWatchOwnership(ref, protected) {
			return
		}
		// watchCtx may be cancelled by shutdown, so cleanup uses a fresh context.
		cctx, ccancel := s.cleanupContext()
		err = s.removeRunner(cctx, ref)
		ccancel()
		if err != nil {
			s.notifyError(fmt.Errorf("cleanup container %s: %w", ref.containerID, err))
		}
	}()
}

// releaseWatchOwnership removes a watched runner from in-process state. The
// same path is used when the container no longer exists, preventing capacity
// from being over-counted.
func (s *DockerScaler) releaseWatchOwnership(ref runnerRef, protected bool) bool {
	if protected {
		_, ok := s.state.takeProtected(ref.containerID)
		return ok
	}
	_, ok := s.state.takeOwnership(ref.runnerName)
	return ok
}

// cleanupContext returns a fresh context for cleanup. It does not reuse the
// WithoutCancel handler ctx or watchCtx; it creates a new deadline from
// Background.
func (s *DockerScaler) cleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), s.cleanupTimeout)
}

// notifyError notifies errCh at most once. The non-blocking send on the
// buffer-1 channel never blocks.
func (s *DockerScaler) notifyError(err error) {
	select {
	case s.errCh <- err:
	default:
	}
}

// ErrCh returns fatal asynchronous scaler errors.
func (s *DockerScaler) ErrCh() <-chan error {
	return s.errCh
}

// ErrShutdownJoinTimeout is a fixed error meaning watch join exceeded the ctx
// deadline. It contains no dynamic information. The caller (app) skips
// subsequent component closes on this error and relies on process exit.
var ErrShutdownJoinTimeout = errors.New("controller: scaler join grace expired; runner watches still running; aborting shutdown; process exit will release resources; leftover containers will be recovered at next startup")

// Shutdown stops new creation, cancels and joins watches, and cleans up the
// idle runners of this process. Busy runners are cleaned up only when
// busyPolicy=stop; protected runners always stay. If watch join exceeds the ctx
// deadline, it returns ErrShutdownJoinTimeout without cleanup, and the caller
// closes no further components. Idle/busy cleanup failures are returned via
// errors.Join so the caller (app) makes the process exit code nonzero together
// with the main error (not just an ErrCh notification).
func (s *DockerScaler) Shutdown(ctx context.Context) error {
	s.watchMu.Lock()
	s.watchStopped = true
	s.watchMu.Unlock()
	s.watchCancel()
	// Wait for wg completion via a channel. On ctx deadline, return the timeout
	// error without waiting for the rest.
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return ErrShutdownJoinTimeout
	}
	var errs []error
	errs = append(errs, s.cleanupRefs(ctx, s.state.takeAllIdle())...)
	if s.busyPolicy == config.ShutdownPolicyStop {
		errs = append(errs, s.cleanupRefs(ctx, s.state.takeAllBusy())...)
	}
	return errors.Join(errs...)
}

// cleanupRefs returns each ref's cleanup failure as an error. Shutdown joins
// them with errors.Join for the caller, so nothing is sent to errCh.
func (s *DockerScaler) cleanupRefs(ctx context.Context, refs []runnerRef) []error {
	var errs []error
	for _, ref := range refs {
		if err := s.removeRunner(ctx, ref); err != nil {
			errs = append(errs, fmt.Errorf("shutdown cleanup container %s: %w", ref.containerID, err))
		}
	}
	return errs
}

// desiredRunnerCount computes the single demand formula
// clamp(max(minRunners, TotalAssignedJobs), 0, maxRunners).
func desiredRunnerCount(minRunners, maxRunners, totalAssignedJobs int) int {
	return min(max(0, minRunners, totalAssignedJobs), max(0, maxRunners))
}

func newRandomHex(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}
