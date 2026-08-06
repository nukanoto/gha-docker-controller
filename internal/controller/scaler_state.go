// scaler_state.go implements the mutex-protected minimal state of DockerScaler.
// It holds only the idle/busy runners created by the current process and the
// protected runners adopted after restart. It never does I/O. It is pure state
// transition, so it is the target of table-driven tests and is separated from
// the I/O paths in scaler.go.
package controller

import (
	"slices"
	"sync"

	"github.com/nukanoto/gha-docker-controller/internal/model"
)

// runnerRef is the minimal identity of one runner. It holds the container ID
// and GitHub runner ID, with no dual index by name and container ID.
type runnerRef struct {
	containerID string
	runnerID    int64
	runnerName  string
}

// identity returns the runner identity used for managed label re-verification.
func (r runnerRef) identity(scaleSetID int) model.RunnerIdentity {
	return model.RunnerIdentity{
		ScaleSetID: int64(scaleSetID),
		RunnerID:   r.runnerID,
		RunnerName: r.runnerName,
	}
}

// runnerState is the mutex-protected minimal state. idle is a slice in
// creation order (oldest first); busy and protected are maps and are never
// removal targets. All methods do no I/O and only move ownership atomically.
// Create it via newRunnerState or NewDockerScaler; the zero value is not used.
type runnerState struct {
	mu        sync.Mutex
	idle      []runnerRef
	busy      map[string]runnerRef
	protected map[string]runnerRef
}

// newRunnerState creates an empty runnerState.
func newRunnerState() runnerState {
	return runnerState{
		idle:      make([]runnerRef, 0),
		busy:      make(map[string]runnerRef),
		protected: make(map[string]runnerRef),
	}
}

// count returns the total of idle, busy, and protected. It is the current
// count for scale-down.
func (r *runnerState) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.idle) + len(r.busy) + len(r.protected)
}

// addIdle appends a runner to the tail of idle right after provisioning.
// Removal starts from the head for oldest-first scale-down.
func (r *runnerState) addIdle(ref runnerRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.idle = append(r.idle, ref)
}

// markBusy moves only a known idle runner to busy. Unknown names return false
// without changes. It is used by JobStarted handling.
func (r *runnerState) markBusy(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, ref := range r.idle {
		if ref.runnerName == name {
			r.idle = append(r.idle[:i], r.idle[i+1:]...)
			r.busy[name] = ref
			return true
		}
	}
	return false
}

// takeOwnership removes the runner with the given name from state, busy
// first, and returns the cleanup ownership. It is the atomic ownership move
// that keeps JobCompleted and wait exit from cleaning up the same runner
// twice. Unknown names return false.
func (r *runnerState) takeOwnership(name string) (runnerRef, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ref, ok := r.busy[name]; ok {
		delete(r.busy, name)
		return ref, true
	}
	for i, ref := range r.idle {
		if ref.runnerName == name {
			r.idle = append(r.idle[:i], r.idle[i+1:]...)
			return ref, true
		}
	}
	return runnerRef{}, false
}

// scaleDownIdle removes and returns up to limit idle runners, oldest first.
// busy and protected are never targets. limit <= 0 does nothing.
func (r *runnerState) scaleDownIdle(limit int) []runnerRef {
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit <= 0 || len(r.idle) == 0 {
		return nil
	}
	if limit > len(r.idle) {
		limit = len(r.idle)
	}
	removed := slices.Clone(r.idle[:limit])
	r.idle = slices.Clone(r.idle[limit:])
	return removed
}

// takeAllIdle removes and returns all idle runners. It is used by shutdown cleanup.
func (r *runnerState) takeAllIdle() []runnerRef {
	r.mu.Lock()
	defer r.mu.Unlock()
	all := r.idle
	r.idle = make([]runnerRef, 0)
	return all
}

// takeAllBusy removes and returns all busy runners. It is used by shutdown
// with busyPolicy=stop.
func (r *runnerState) takeAllBusy() []runnerRef {
	r.mu.Lock()
	defer r.mu.Unlock()
	all := make([]runnerRef, 0, len(r.busy))
	for name, ref := range r.busy {
		all = append(all, ref)
		delete(r.busy, name)
	}
	return all
}

// addProtected registers a runner adopted after restart, keyed by container
// ID. protected is included in count but is not a target of events or
// scale-down.
func (r *runnerState) addProtected(ref runnerRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.protected[ref.containerID] = ref
}

// takeProtected removes and returns the protected runner with the given
// container ID. It is the cleanup ownership of wait exit. Unknown IDs return
// false.
func (r *runnerState) takeProtected(containerID string) (runnerRef, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ref, ok := r.protected[containerID]
	if ok {
		delete(r.protected, containerID)
	}
	return ref, ok
}
