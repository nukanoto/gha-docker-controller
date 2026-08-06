package docker

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"

	"github.com/nukanoto/gha-docker-controller/internal/model"
)

// ManagedContainer is the observation of a container that passed the managed
// label checks. ID and name are unexported, so only ListManaged and
// VerifyManaged in this package can produce this type. Destructive APIs
// (StopManaged) take this type as an argument, so an unmanaged container
// cannot structurally be passed to a destructive operation. The observation
// is only a snapshot from enumeration time; right before a destructive
// operation the labels are re-checked with a fresh inspect. The final remove
// can only run from the single SDK ContainerRemove inside CleanupManaged in
// lifecycle.go; nothing outside this package can call it. The internally
// stored labels and the Labels() return value are defensive copies; writes
// by the caller do not change the internal state.
type ManagedContainer struct {
	id          string
	name        string
	labels      map[string]string
	state       container.ContainerState
	status      string
	created     time.Time
	exitCode    int
	hasExitCode bool
}

// ID returns the container ID.
func (m ManagedContainer) ID() string {
	return m.id
}

// Name returns the container name. The leading "/" added by the daemon is
// stripped. The name is auxiliary information, not the identity source of
// truth.
func (m ManagedContainer) Name() string {
	return m.name
}

// Labels returns a defensive copy of all container labels. Writes by the
// caller do not change the internal state. A nil input stays nil; callers
// must treat the result as read-only (no write path exists).
func (m ManagedContainer) Labels() map[string]string {
	return maps.Clone(m.labels)
}

// State returns the container state (created/running/paused/restarting/exited/dead).
func (m ManagedContainer) State() container.ContainerState {
	return m.state
}

// Status returns the human-readable status string from the daemon.
func (m ManagedContainer) Status() string {
	return m.status
}

// CreatedAt returns the container creation time.
func (m ManagedContainer) CreatedAt() time.Time {
	return m.created
}

// ExitCode returns the exit code of an exited/dead container. It returns
// ok=false for any other state.
func (m ManagedContainer) ExitCode() (code int, ok bool) {
	return m.exitCode, m.hasExitCode
}

// ManagedGuardError is the error that reports a managed label mismatch
// detected by the fresh inspect right before a destructive API runs. It is
// a protection violation and must not be retried. The daemon state is
// unchanged, so recovery relies on a controller systemd restart and the
// official client retry contract.
type ManagedGuardError struct {
	// ContainerID is the ID of the container that failed the check.
	ContainerID string
	// Reason describes the mismatch.
	Reason string
}

// Error returns the managed guard failure without secret data.
func (e *ManagedGuardError) Error() string {
	return fmt.Sprintf("container %s is not a managed runner of this scale set: %s", e.ContainerID, e.Reason)
}

// ListManaged enumerates candidate containers with both labels
// managed=true and scale-set-id=<id>. The result is sorted by created
// ascending (by ID for equal timestamps) to keep the order deterministic.
// The daemon label filter is an exact match, so an item that does not
// satisfy the condition is a daemon contract violation; it is not silently
// skipped but returned as an error. Destructive operations do not trust
// this result blindly; StopManaged and CleanupManaged re-check with a fresh
// inspect.
func (c *Client) ListManaged(ctx context.Context, scaleSetID int64) ([]ManagedContainer, error) {
	if scaleSetID <= 0 {
		return nil, fmt.Errorf("scale set id must be positive, got %d", scaleSetID)
	}
	wantScaleSetID := strconv.FormatInt(scaleSetID, 10)
	filters := mobyclient.Filters{}.
		Add("label", model.ManagedLabelKey+"="+model.ManagedLabelValue).
		Add("label", model.ScaleSetIDLabelKey+"="+wantScaleSetID)
	res, err := c.c.ContainerList(ctx, mobyclient.ContainerListOptions{
		All:     true,
		Filters: filters,
	})
	if err != nil {
		return nil, err
	}
	items := make([]ManagedContainer, 0, len(res.Items))
	for _, s := range res.Items {
		if s.Labels[model.ManagedLabelKey] != model.ManagedLabelValue ||
			s.Labels[model.ScaleSetIDLabelKey] != wantScaleSetID {
			return nil, fmt.Errorf("docker daemon returned container %s that does not match the managed label filter (managed=%q scale-set-id=%q); refusing to enumerate it as managed", s.ID, s.Labels[model.ManagedLabelKey], s.Labels[model.ScaleSetIDLabelKey])
		}
		items = append(items, managedFromSummary(s))
	}
	// Scale-down targets the oldest idle runner, so oldest-first ordering
	// keeps the caller's decision deterministic.
	slices.SortFunc(items, func(a, b ManagedContainer) int {
		if c := a.created.Compare(b.created); c != 0 {
			return c
		}
		return cmp.Compare(a.id, b.id)
	})
	return items, nil
}

// containerLabels returns the label map of an inspect result nil-safely.
// Config is a *container.Config pointer; a nil inspect result violates the
// daemon contract, but the guard fails with "no labels" instead of a
// panic.
func containerLabels(in container.InspectResponse) map[string]string {
	if in.Config == nil {
		return nil
	}
	return in.Config.Labels
}

// VerifyManaged fresh-inspects the target container, checks the destructive
// operation condition (exact match of the required six labels against the
// identity) and returns a ManagedContainer. It backs the first label
// re-check of cleanup and the exit code fetch of exited/dead containers.
// A 404 is returned as a state observation meaning "the target does not
// exist" and can be tested with cerrdefs.IsNotFound. A label mismatch is
// returned as a ManagedGuardError.
func (c *Client) VerifyManaged(ctx context.Context, id string, identity model.RunnerIdentity) (ManagedContainer, error) {
	inspect, err := c.ContainerInspect(ctx, id, mobyclient.ContainerInspectOptions{})
	if err != nil {
		return ManagedContainer{}, err
	}
	if err := verifyManagedLabels(id, containerLabels(inspect.Container), identity); err != nil {
		return ManagedContainer{}, err
	}
	return managedFromInspect(inspect.Container), nil
}

// StartManaged starts a managed container. Right before starting, a fresh
// inspect re-checks the required six labels exactly against the identity.
// This is the only path through which controller provisioning starts a
// container; no raw start without a fresh check is callable from outside
// this package. A 404 on the fresh inspect or the start is returned as a
// state observation meaning "the target does not exist" and can be tested
// with cerrdefs.IsNotFound. A label mismatch is returned as a
// ManagedGuardError and the start is not executed.
func (c *Client) StartManaged(ctx context.Context, id string, identity model.RunnerIdentity) (mobyclient.ContainerStartResult, error) {
	inspect, err := c.ContainerInspect(ctx, id, mobyclient.ContainerInspectOptions{})
	if err != nil {
		return mobyclient.ContainerStartResult{}, err
	}
	if err := verifyManagedLabels(id, containerLabels(inspect.Container), identity); err != nil {
		return mobyclient.ContainerStartResult{}, err
	}
	return c.containerStart(ctx, id, mobyclient.ContainerStartOptions{})
}

// StopManaged stops a managed container. Right before stopping, a fresh
// inspect re-checks the required six labels exactly against the identity.
// A 404 on the fresh inspect or the stop is returned as a state observation
// meaning "the target does not exist" and can be tested with
// cerrdefs.IsNotFound. A 304 for an already-stopped container counts as
// success. A label mismatch is returned as a ManagedGuardError. The final
// remove (containerRemove) is not done here; only CleanupManaged in
// lifecycle.go performs it.
func (c *Client) StopManaged(ctx context.Context, mc ManagedContainer, identity model.RunnerIdentity, options mobyclient.ContainerStopOptions) (mobyclient.ContainerStopResult, error) {
	// Labels may have been rewritten after ListManaged, so always re-check
	// with a fresh inspect instead of trusting the passed observation.
	inspect, err := c.ContainerInspect(ctx, mc.id, mobyclient.ContainerInspectOptions{})
	if err != nil {
		return mobyclient.ContainerStopResult{}, err
	}
	if err := verifyManagedLabels(mc.id, containerLabels(inspect.Container), identity); err != nil {
		return mobyclient.ContainerStopResult{}, err
	}
	return c.containerStop(ctx, mc.id, options)
}

// verifyManagedLabels checks whether the fresh-inspect labels satisfy the
// destructive operation condition. model.ValidateLabels fully validates the
// required six labels (managed, scale-set-id, runner-id, runner-name,
// controller-instance, created-at) for an exact match against the identity;
// no weaker per-label checks are done. Tampering with runner-name,
// controller-instance or created-at is also rejected as a
// ManagedGuardError. Every destructive path (StartManaged, StopManaged,
// VerifyManaged, RefreshManaged) passes through this check, so a container
// with malformed labels is never modified.
func verifyManagedLabels(id string, labels map[string]string, identity model.RunnerIdentity) error {
	if identity.ScaleSetID <= 0 || identity.RunnerID <= 0 {
		return fmt.Errorf("identity must have positive scale set id and runner id, got scale-set-id=%d runner-id=%d", identity.ScaleSetID, identity.RunnerID)
	}
	if err := model.ValidateLabels(labels, identity); err != nil {
		return &ManagedGuardError{ContainerID: id, Reason: fmt.Sprintf("managed labels are malformed or do not match the identity: %v", err)}
	}
	return nil
}

// managedFromSummary builds a ManagedContainer from a /containers/json
// Summary. The exit code is not in a Summary, so hasExitCode is false.
func managedFromSummary(s container.Summary) ManagedContainer {
	name := ""
	if len(s.Names) > 0 {
		name = strings.TrimPrefix(s.Names[0], "/")
	}
	return ManagedContainer{
		id:      s.ID,
		name:    name,
		labels:  maps.Clone(s.Labels),
		state:   s.State,
		status:  s.Status,
		created: time.Unix(s.Created, 0).UTC(),
	}
}

// managedFromInspect builds a ManagedContainer from an inspect result.
// Only exited/dead containers keep an exit code.
func managedFromInspect(in container.InspectResponse) ManagedContainer {
	m := ManagedContainer{
		id:     in.ID,
		name:   strings.TrimPrefix(in.Name, "/"),
		labels: maps.Clone(containerLabels(in)),
	}
	// created comes as RFC3339 (nanoseconds optional). On a parse failure it
	// falls back to the zero time; that is a daemon contract violation, so
	// the caller's checks surface it.
	if t, err := time.Parse(time.RFC3339Nano, in.Created); err == nil {
		m.created = t.UTC()
	}
	if in.State != nil {
		m.state = in.State.Status
		m.status = string(in.State.Status)
		if in.State.Status == container.StateExited || in.State.Status == container.StateDead {
			m.exitCode = in.State.ExitCode
			m.hasExitCode = true
		}
	}
	return m
}
