// Package controller implements the official listener.Scaler with Docker.
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

	"github.com/nukanoto/arc-docker/internal/config"
	"github.com/nukanoto/arc-docker/internal/docker"
	"github.com/nukanoto/arc-docker/internal/model"
	"github.com/nukanoto/arc-docker/internal/scaleset"
)

// DockerScaler implements the official listener.Scaler with oldest-first
// scale-down.
type DockerScaler struct {
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
	watchCtx            context.Context
	watchCancel         context.CancelFunc
	wg                  sync.WaitGroup
	// watchMu serializes watch registration with Shutdown's Wait.
	watchMu      sync.Mutex
	watchStopped bool
	state        runnerState
}

var _ listenerapi.Scaler = (*DockerScaler)(nil)

// NewDockerScaler builds a validated DockerScaler.
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

// HandleDesiredRunnerCount moves the runner count toward demand.
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
				return s.state.count(), fmt.Errorf("failed to start runner: %w", err)
			}
		}
		return s.state.count(), nil
	default:
		s.logger.Info("Scaling down runners",
			slog.Int("currentCount", current), slog.Int("desiredCount", desired), slog.Int("scaleDown", current-desired))
		// Handlers may outlive listener cancellation; use a fresh cleanup context.
		cctx, ccancel := s.cleanupContext()
		defer ccancel()
		for _, ref := range s.state.scaleDownIdle(current - desired) {
			if err := s.removeRunner(cctx, ref); err != nil {
				return s.state.count(), fmt.Errorf("failed to remove runner: %w", err)
			}
		}
		return s.state.count(), nil
	}
}

// HandleJobStarted marks the assigned runner busy.
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

// HandleJobCompleted takes cleanup ownership and removes the runner.
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

// provision creates, starts, registers, and watches one runner.
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
		// Recovery is centralized in the next startup after uncertain creates.
		return fmt.Errorf("create runner container: %w", err)
	}
	if _, err := s.dockerClient.StartManaged(ctx, created.ID, identity); err != nil {
		s.cleanupAfterProvisionFailure(created.ID, identity)
		return fmt.Errorf("start runner container: %w", err)
	}
	ref := runnerRef{containerID: created.ID, runnerID: jit.RunnerID, runnerName: jit.RunnerName}
	s.state.addIdle(ref)
	s.startWatch(ref, false)
	return nil
}

// cleanupAfterProvisionFailure removes a container after a start failure.
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

// removeRunner verifies and removes one managed container.
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

// Recover adopts or removes managed containers left by an earlier process.
func (s *DockerScaler) Recover(ctx context.Context) error {
	containers, err := s.dockerClient.ListManaged(ctx, int64(s.scaleSetID))
	if err != nil {
		return err
	}
	// Cleanup must outlive the startup/listing context.
	cctx, ccancel := s.cleanupContext()
	defer ccancel()
	for _, mc := range containers {
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

// startWatch removes a runner when its container exits.
func (s *DockerScaler) startWatch(ref runnerRef, protected bool) {
	s.watchMu.Lock()
	if s.watchStopped {
		// The same lock prevents concurrent wg.Add and wg.Wait.
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
				s.releaseWatchOwnership(ref, protected)
				return
			}
			s.notifyError(fmt.Errorf("wait container %s: %w", ref.containerID, err))
			return
		}
		if !s.releaseWatchOwnership(ref, protected) {
			return
		}
		cctx, ccancel := s.cleanupContext()
		err = s.removeRunner(cctx, ref)
		ccancel()
		if err != nil {
			s.notifyError(fmt.Errorf("cleanup container %s: %w", ref.containerID, err))
		}
	}()
}

// releaseWatchOwnership removes a watched runner from in-process state.
func (s *DockerScaler) releaseWatchOwnership(ref runnerRef, protected bool) bool {
	if protected {
		_, ok := s.state.takeProtected(ref.containerID)
		return ok
	}
	_, ok := s.state.takeOwnership(ref.runnerName)
	return ok
}

// cleanupContext returns a fresh deadline independent of handler and watch contexts.
func (s *DockerScaler) cleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), s.cleanupTimeout)
}

// notifyError reports the first asynchronous error without blocking.
func (s *DockerScaler) notifyError(err error) {
	select {
	case s.errCh <- err:
	default:
	}
}

// ErrCh returns asynchronous scaler errors.
func (s *DockerScaler) ErrCh() <-chan error {
	return s.errCh
}

// ErrShutdownJoinTimeout means watches outlived the shutdown deadline.
var ErrShutdownJoinTimeout = errors.New("controller: scaler join grace expired; runner watches still running; aborting shutdown; process exit will release resources; leftover containers will be recovered at next startup")

// Shutdown stops provisioning, joins watches, and cleans up owned runners.
// Protected runners remain available for the next process.
func (s *DockerScaler) Shutdown(ctx context.Context) error {
	s.watchMu.Lock()
	s.watchStopped = true
	s.watchMu.Unlock()
	s.watchCancel()
	// Return on deadline instead of waiting for late watches.
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

// cleanupRefs collects cleanup failures for errors.Join.
func (s *DockerScaler) cleanupRefs(ctx context.Context, refs []runnerRef) []error {
	var errs []error
	for _, ref := range refs {
		if err := s.removeRunner(ctx, ref); err != nil {
			errs = append(errs, fmt.Errorf("shutdown cleanup container %s: %w", ref.containerID, err))
		}
	}
	return errs
}

// desiredRunnerCount clamps demand to the configured bounds.
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
