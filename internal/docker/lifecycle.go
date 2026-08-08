package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"

	"github.com/nukanoto/arc-docker/internal/model"
)

// ManagedCleanupOptions controls managed-container cleanup.
type ManagedCleanupOptions struct {
	// StopTimeout is the grace period for a running container.
	StopTimeout time.Duration
}

// ManagedCleanupResult contains the observations collected during cleanup.
type ManagedCleanupResult struct {
	// ContainerID is the cleaned-up container ID.
	ContainerID string
	// ExitCode is valid only when HasExitCode is true.
	ExitCode    int
	HasExitCode bool
	// Stdout is bounded container output.
	Stdout string
	// Stderr is bounded container error output.
	Stderr string
}

// RefreshManaged re-inspects a managed container and re-checks its labels.
// The original observed labels remain the identity source for this check.
func (c *Client) RefreshManaged(ctx context.Context, mc ManagedContainer) (ManagedContainer, error) {
	identity, err := identityFromObserved(mc.id, mc.labels)
	if err != nil {
		return ManagedContainer{}, err
	}
	inspect, err := c.ContainerInspect(ctx, mc.id, mobyclient.ContainerInspectOptions{})
	if err != nil {
		return ManagedContainer{}, err
	}
	if err := verifyManagedLabels(mc.id, containerLabels(inspect.Container), identity); err != nil {
		return ManagedContainer{}, err
	}
	return managedFromInspect(inspect.Container), nil
}

// identityFromObserved rebuilds the identity used by the destructive-operation
// guard. Keeping it derived from the original observation prevents a later
// inspect from changing which container the guard authorizes.
func identityFromObserved(id string, labels map[string]string) (model.RunnerIdentity, error) {
	scaleSetID, err := strconv.ParseInt(labels[model.ScaleSetIDLabelKey], 10, 64)
	if err != nil {
		return model.RunnerIdentity{}, &ManagedGuardError{ContainerID: id, Reason: fmt.Sprintf("observed scale-set-id label is not a base-10 integer: %q", labels[model.ScaleSetIDLabelKey])}
	}
	runnerID, err := strconv.ParseInt(labels[model.RunnerIDLabelKey], 10, 64)
	if err != nil {
		return model.RunnerIdentity{}, &ManagedGuardError{ContainerID: id, Reason: fmt.Sprintf("observed runner-id label is not a base-10 integer: %q", labels[model.RunnerIDLabelKey])}
	}
	identity := model.RunnerIdentity{
		ScaleSetID: scaleSetID,
		RunnerID:   runnerID,
		RunnerName: labels[model.RunnerNameLabelKey],
	}
	if err := model.ValidateLabels(labels, identity); err != nil {
		return model.RunnerIdentity{}, &ManagedGuardError{ContainerID: id, Reason: fmt.Sprintf("observed managed labels are malformed: %v", err)}
	}
	return identity, nil
}

// needsStop reports whether cleanup must stop the container first.
func needsStop(state container.ContainerState) bool {
	switch state {
	case container.StateRunning, container.StatePaused, container.StateRestarting:
		return true
	default:
		return false
	}
}

// stopTimeoutSeconds rounds up while preserving the SDK's minimum one-second
// grace period. A zero timeout means an immediate kill to the Docker SDK.
func stopTimeoutSeconds(d time.Duration) int {
	return max(int((d+time.Second-1)/time.Second), 1)
}

// CleanupManaged stops, observes, and removes one verified managed container.
// Each destructive operation is preceded by a fresh label check; missing
// containers are treated as an idempotent success.
func (c *Client) CleanupManaged(ctx context.Context, mc ManagedContainer, options ManagedCleanupOptions) (ManagedCleanupResult, error) {
	if options.StopTimeout <= 0 {
		return ManagedCleanupResult{}, fmt.Errorf("cleanup managed container %s: stop timeout must be positive, got %s", mc.id, options.StopTimeout)
	}

	refreshed, err := c.RefreshManaged(ctx, mc)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			// Cleaning up a container that no longer exists is an idempotent success.
			return ManagedCleanupResult{ContainerID: mc.id}, nil
		}
		return ManagedCleanupResult{}, err
	}
	identity, err := identityFromObserved(refreshed.id, refreshed.labels)
	if err != nil {
		return ManagedCleanupResult{}, err
	}
	gone := false
	result := ManagedCleanupResult{ContainerID: refreshed.id}

	if needsStop(refreshed.State()) {
		timeout := stopTimeoutSeconds(options.StopTimeout)
		if _, err := c.StopManaged(ctx, refreshed, identity, mobyclient.ContainerStopOptions{Timeout: &timeout}); err != nil {
			if cerrdefs.IsNotFound(err) {
				gone = true
			} else {
				return ManagedCleanupResult{}, err
			}
		} else if _, err := c.WaitContainer(ctx, refreshed.id, mobyclient.ContainerWaitOptions{Condition: container.WaitConditionNotRunning}); err != nil {
			if cerrdefs.IsNotFound(err) {
				gone = true
			} else {
				return ManagedCleanupResult{}, err
			}
		}
	}

	if !gone {
		inspect, err := c.ContainerInspect(ctx, refreshed.id, mobyclient.ContainerInspectOptions{})
		if err != nil {
			if cerrdefs.IsNotFound(err) {
				gone = true
			} else {
				return ManagedCleanupResult{}, err
			}
		} else {
			if code, ok := managedFromInspect(inspect.Container).ExitCode(); ok {
				result.ExitCode = code
				result.HasExitCode = true
			}
			logs, err := c.FetchLogs(ctx, refreshed.id, LogOptions{
				MaxStdoutBytes: 64 * 1024,
				MaxStderrBytes: 64 * 1024,
				Tail:           "200",
			})
			if err == nil {
				result.Stdout = logs.Stdout
				result.Stderr = logs.Stderr
			} else if cerrdefs.IsNotFound(err) {
				gone = true
			}
			// Removing a container that may hold JIT credentials takes priority
			// over collecting optional diagnostics.
		}
	}

	if !gone {
		latest, err := c.RefreshManaged(ctx, refreshed)
		if err != nil {
			if cerrdefs.IsNotFound(err) {
				gone = true
			} else {
				return ManagedCleanupResult{}, err
			}
		} else {
			if _, err := c.containerRemove(ctx, latest.id, mobyclient.ContainerRemoveOptions{Force: false, RemoveVolumes: true}); err != nil {
				if cerrdefs.IsNotFound(err) {
					gone = true
				} else {
					return ManagedCleanupResult{}, err
				}
			}
		}
	}
	return result, nil
}

// DefaultLogBytesPerStream is used when a log limit is not positive.
const DefaultLogBytesPerStream = 64 * 1024

// ContainerLogs opens a container log stream.
func (c *Client) ContainerLogs(ctx context.Context, id string, options mobyclient.ContainerLogsOptions) (mobyclient.ContainerLogsResult, error) {
	return c.c.ContainerLogs(ctx, id, options)
}

// LogOptions controls the log range and per-stream limits.
type LogOptions struct {
	MaxStdoutBytes int
	MaxStderrBytes int
	Tail           string
}

// LogResult contains bounded container logs.
type LogResult struct {
	Stdout string
	Stderr string
}

// FetchLogs reads bounded stdout/stderr without exposing container environment
// variables such as the JIT configuration.
func (c *Client) FetchLogs(ctx context.Context, id string, options LogOptions) (LogResult, error) {
	maxOut, maxErr := options.MaxStdoutBytes, options.MaxStderrBytes
	if maxOut <= 0 {
		maxOut = DefaultLogBytesPerStream
	}
	if maxErr <= 0 {
		maxErr = DefaultLogBytesPerStream
	}
	stream, err := c.ContainerLogs(ctx, id, mobyclient.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       options.Tail,
	})
	if err != nil {
		return LogResult{}, err
	}
	// Close the stream on every return path.
	defer stream.Close()
	var out, errBuf bytes.Buffer
	if _, err := stdcopy.StdCopy(
		&boundedWriter{max: maxOut, buf: &out},
		&boundedWriter{max: maxErr, buf: &errBuf},
		stream,
	); err != nil {
		return LogResult{}, fmt.Errorf("read container logs: %w", err)
	}
	return LogResult{Stdout: out.String(), Stderr: errBuf.String()}, nil
}

// boundedWriter retains a prefix while allowing the source stream to drain.
type boundedWriter struct {
	max int
	buf *bytes.Buffer
}

// Write retains at most max bytes and reports the full input as consumed.
func (w *boundedWriter) Write(p []byte) (int, error) {
	remaining := w.max - w.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			// Keep consuming the stream so the daemon-side connection closes.
			w.buf.Write(p[:remaining])
		} else {
			w.buf.Write(p)
		}
	}
	return len(p), nil
}

var _ io.Writer = (*boundedWriter)(nil)
