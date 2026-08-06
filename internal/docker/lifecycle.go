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

	"github.com/nukanoto/gha-docker-controller/internal/model"
)

// ManagedCleanupOptions is the execution condition for CleanupManaged.
type ManagedCleanupOptions struct {
	// StopTimeout is the grace period for stopping running/paused/restarting
	// containers. It must be positive; 0 or less is rejected before any I/O.
	StopTimeout time.Duration
}

// ManagedCleanupResult is the set of observations CleanupManaged collected.
// The stdout/stderr content is returned here and never written to the
// application log (no path may leak secrets such as the JIT config into a
// log).
type ManagedCleanupResult struct {
	// ContainerID is the ID of the container that was cleaned up.
	ContainerID string
	// ExitCode is the exit code observed by inspect after the stop. When it
	// could not be observed, HasExitCode is false.
	ExitCode int
	// HasExitCode reports whether ExitCode is valid.
	HasExitCode bool
	// Stdout is the bounded stdout content (tail 200 lines, at most 64 KiB).
	Stdout string
	// Stderr is the bounded stderr content (tail 200 lines, at most 64 KiB).
	Stderr string
}

// RefreshManaged refreshes an observed ManagedContainer with a fresh
// inspect. It also re-checks that the required six labels exactly match the
// identity restored from the observed labels; a mismatch is rejected as a
// ManagedGuardError. The return value carries the newest state
// (running/paused/exited/dead, ...) and, for exited/dead, the exit code. A
// 404 is returned as an error that means "the target no longer exists" and
// can be tested with cerrdefs.IsNotFound. The observed labels at enumeration
// time are the only source of truth for the re-match; no external identity
// is used.
func (c *Client) RefreshManaged(ctx context.Context, mc ManagedContainer) (ManagedContainer, error) {
	// Reject malformed observed labels before any I/O.
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

// identityFromObserved restores the runner identity from the labels observed
// at enumeration time. The destructive-operation guard keeps no external
// state, so the observed labels themselves are the source of truth for the
// re-match. It parses scale-set-id and runner-id as base-10 integers, builds
// the RunnerIdentity, then validates the required six labels (including
// runner-name, controller-instance and created-at) with
// model.ValidateLabels. Non-positive IDs and missing or invalid labels are
// malformed and rejected as ManagedGuardError. This way CleanupManaged and
// Recover never change a container whose observation is malformed.
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

// CleanupManaged destroys and removes a managed container. The order is:
//
//  1. RefreshManaged does a fresh inspect and re-matches the required six
//     labels against the identity restored from the observed values. A 404
//     is a state observation meaning "already gone", so the remaining steps
//     are skipped and success is returned (idempotent).
//  2. Only running/paused/restarting containers are stopped with a stop
//     timeout and waited to not-running. Already-stopped containers are not
//     stopped (a 304 for stop-on-stopped is not an error for the Moby
//     client).
//  3. The exit code is observed by inspect, and bounded logs (64 KiB, tail
//     200 lines per stream) are fetched best-effort. Any fetch failure other
//     than 404 does not stop the cleanup; stdout/stderr stay empty and step 4
//     runs.
//  4. After a fresh inspect and label re-check, containerRemove runs with
//     Force=false and RemoveVolumes=true. A 404 is an idempotent success.
//
// This API never touches GitHub runner deletion or a state machine; it only
// stops, observes and removes. The argument must be a ManagedContainer with
// unexported fields, so there is no way to pass an ID string or an
// unmanaged container.

// needsStop is a pure function that decides whether a container state needs
// a stop (running/paused/restarting). created/exited/dead need no stop.
// It backs the stop decision in CleanupManaged.
func needsStop(state container.ContainerState) bool {
	switch state {
	case container.StateRunning, container.StatePaused, container.StateRestarting:
		return true
	default:
		return false
	}
}

// stopTimeoutSeconds converts a duration to a positive ceiling in seconds.
// The SDK interprets a Timeout of 0 as "kill immediately with no grace", so
// a positive setting must never round down to 0; the result is always at
// least 1 second. It is used for the CleanupManaged stop, the spec
// StopTimeout conversion and the dind entrypoint timeout env.
func stopTimeoutSeconds(d time.Duration) int {
	return max(int((d+time.Second-1)/time.Second), 1)
}

// CleanupManaged stops, inspects, and removes one verified managed container.
func (c *Client) CleanupManaged(ctx context.Context, mc ManagedContainer, options ManagedCleanupOptions) (ManagedCleanupResult, error) {
	if options.StopTimeout <= 0 {
		return ManagedCleanupResult{}, fmt.Errorf("cleanup managed container %s: stop timeout must be positive, got %s", mc.id, options.StopTimeout)
	}

	// 1. Fresh inspect re-matches the labels against the observed values and
	// refreshes the state.
	refreshed, err := c.RefreshManaged(ctx, mc)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			// Cleaning up a container that no longer exists is an idempotent success.
			return ManagedCleanupResult{ContainerID: mc.id}, nil
		}
		return ManagedCleanupResult{}, err
	}
	// Only an identity that has been re-matched against the observed values
	// authorizes destructive operations.
	identity, err := identityFromObserved(refreshed.id, refreshed.labels)
	if err != nil {
		return ManagedCleanupResult{}, err
	}
	// gone means "the container no longer exists". The remaining steps are
	// skipped and only the observations already collected are returned.
	gone := false
	result := ManagedCleanupResult{ContainerID: refreshed.id}

	// 2. Only running/paused/restarting containers are stopped, then waited
	// to not-running.
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

	// 3. Collect the exit code and the bounded logs.
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
			// Other log fetch failures are treated as missing diagnostics;
			// removing the container that holds env secrets wins.
		}
	}

	// 4. After a fresh inspect and label re-check, remove with Force=false and
	// RemoveVolumes=true. This is the only containerRemove call in this
	// lifecycle, so it is the single removal path.
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

// ---- bounded logs ----

// DefaultLogBytesPerStream is the safety net for FetchLogs when a stream
// limit is 0 or less. The caller's LogOptions values are expected to be
// positive; this constant only guards against a missing configuration.
const DefaultLogBytesPerStream = 64 * 1024

// ContainerLogs opens the container log stream with the v29 signature.
// The caller must always Close the returned stream. Use FetchLogs to get
// bounded content and stream close in one call.
func (c *Client) ContainerLogs(ctx context.Context, id string, options mobyclient.ContainerLogsOptions) (mobyclient.ContainerLogsResult, error) {
	return c.c.ContainerLogs(ctx, id, options)
}

// LogOptions is the fetch range and limits for FetchLogs.
type LogOptions struct {
	// MaxStdoutBytes is the maximum number of stdout bytes to fetch. 0 or
	// less falls back to DefaultLogBytesPerStream.
	MaxStdoutBytes int
	// MaxStderrBytes is the maximum number of stderr bytes to fetch. 0 or
	// less falls back to DefaultLogBytesPerStream.
	MaxStderrBytes int
	// Tail is the number of lines to fetch from the end. An empty string or
	// "all" fetches all lines up to the per-stream byte limit.
	Tail string
}

// LogResult is the result of FetchLogs. stdout/stderr are each truncated at
// the LogOptions byte limit.
type LogResult struct {
	// Stdout is the stdout content.
	Stdout string
	// Stderr is the stderr content.
	Stderr string
}

// FetchLogs fetches bounded stdout/stderr from a container. Each stream is
// truncated at MaxStdoutBytes / MaxStderrBytes, but the stream itself is
// always read to the end and then Closed (the daemon side is not cut off
// mid-send and the connection ends cleanly). This API returns only log
// streams; it never fetches or returns Config.Env (secrets including the JIT
// config). A container with TTY=false has a multiplexed stream, which
// stdcopy splits into stdout/stderr.
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
	// The stream must always be closed; this covers every return path of
	// FetchLogs.
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

// boundedWriter is an io.Writer that keeps only the first max bytes in buf
// and discards the rest. Write always returns the full input length, so
// stdcopy keeps consuming the stream to the end while only the retained log
// is truncated.
type boundedWriter struct {
	max int
	buf *bytes.Buffer
}

// Write implements io.Writer. It always returns len(p) (the excess is
// silently discarded).
func (w *boundedWriter) Write(p []byte) (int, error) {
	remaining := w.max - w.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			// The limit is reached; the rest is discarded (same cutoff as
			// io.LimitReader).
			w.buf.Write(p[:remaining])
		} else {
			w.buf.Write(p)
		}
	}
	return len(p), nil
}

// _ asserts at compile time that boundedWriter satisfies io.Writer.
var _ io.Writer = (*boundedWriter)(nil)
