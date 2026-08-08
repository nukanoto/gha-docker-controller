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

	"github.com/nukanoto/arc-docker/internal/model"
)

// ManagedContainer is an observed container that passed the managed-label
// guard. Destructive operations re-check the labels before making changes.
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

// ID returns the Docker container ID.
func (m ManagedContainer) ID() string {
	return m.id
}

// Name returns the container name without Docker's leading slash.
func (m ManagedContainer) Name() string {
	return m.name
}

// Labels returns a defensive copy of the container labels.
func (m ManagedContainer) Labels() map[string]string {
	return maps.Clone(m.labels)
}

// State returns the Docker container state.
func (m ManagedContainer) State() container.ContainerState {
	return m.state
}

// Status returns the daemon's human-readable status.
func (m ManagedContainer) Status() string {
	return m.status
}

// CreatedAt returns the container creation time.
func (m ManagedContainer) CreatedAt() time.Time {
	return m.created
}

// ExitCode returns the observed exit code, when available.
func (m ManagedContainer) ExitCode() (code int, ok bool) {
	return m.exitCode, m.hasExitCode
}

// ManagedGuardError reports that a fresh label check rejected an operation.
type ManagedGuardError struct {
	// ContainerID identifies the rejected container.
	ContainerID string
	// Reason describes the guard failure.
	Reason string
}

// Error returns the managed guard failure without secret data.
func (e *ManagedGuardError) Error() string {
	return fmt.Sprintf("container %s is not a managed runner of this scale set: %s", e.ContainerID, e.Reason)
}

// ListManaged enumerates managed containers for a Scale Set in creation order.
// A daemon result that violates the label filter is an error, not a skipped
// item.
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

// containerLabels returns labels without panicking on a malformed inspect.
func containerLabels(in container.InspectResponse) map[string]string {
	if in.Config == nil {
		return nil
	}
	return in.Config.Labels
}

// VerifyManaged fresh-inspects a container and checks its managed labels.
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

// StartManaged re-checks the labels and starts a managed container.
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

// StopManaged re-checks the labels and stops a managed container.
func (c *Client) StopManaged(ctx context.Context, mc ManagedContainer, identity model.RunnerIdentity, options mobyclient.ContainerStopOptions) (mobyclient.ContainerStopResult, error) {
	// Labels may change after enumeration, so do not trust the snapshot.
	inspect, err := c.ContainerInspect(ctx, mc.id, mobyclient.ContainerInspectOptions{})
	if err != nil {
		return mobyclient.ContainerStopResult{}, err
	}
	if err := verifyManagedLabels(mc.id, containerLabels(inspect.Container), identity); err != nil {
		return mobyclient.ContainerStopResult{}, err
	}
	return c.containerStop(ctx, mc.id, options)
}

// verifyManagedLabels checks the complete managed-container identity.
func verifyManagedLabels(id string, labels map[string]string, identity model.RunnerIdentity) error {
	if identity.ScaleSetID <= 0 || identity.RunnerID <= 0 {
		return fmt.Errorf("identity must have positive scale set id and runner id, got scale-set-id=%d runner-id=%d", identity.ScaleSetID, identity.RunnerID)
	}
	if err := model.ValidateLabels(labels, identity); err != nil {
		return &ManagedGuardError{ContainerID: id, Reason: fmt.Sprintf("managed labels are malformed or do not match the identity: %v", err)}
	}
	return nil
}

// managedFromSummary converts a list summary to a ManagedContainer.
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

// managedFromInspect converts an inspect result to a ManagedContainer.
func managedFromInspect(in container.InspectResponse) ManagedContainer {
	m := ManagedContainer{
		id:     in.ID,
		name:   strings.TrimPrefix(in.Name, "/"),
		labels: maps.Clone(containerLabels(in)),
	}
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
