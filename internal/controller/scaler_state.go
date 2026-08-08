// This file contains DockerScaler's mutex-protected in-memory state.
package controller

import (
	"slices"
	"sync"

	"github.com/nukanoto/gha-docker-controller/internal/model"
)

// runnerRef identifies a runner and its container.
type runnerRef struct {
	containerID string
	runnerID    int64
	runnerName  string
}

func (r runnerRef) identity(scaleSetID int) model.RunnerIdentity {
	return model.RunnerIdentity{
		ScaleSetID: int64(scaleSetID),
		RunnerID:   r.runnerID,
		RunnerName: r.runnerName,
	}
}

// runnerState tracks ownership without performing I/O. idle stays oldest first;
// protected runners are excluded from event handling and scale-down.
type runnerState struct {
	mu        sync.Mutex
	idle      []runnerRef
	busy      map[string]runnerRef
	protected map[string]runnerRef
}

func newRunnerState() runnerState {
	return runnerState{
		idle:      make([]runnerRef, 0),
		busy:      make(map[string]runnerRef),
		protected: make(map[string]runnerRef),
	}
}

func (r *runnerState) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.idle) + len(r.busy) + len(r.protected)
}

func (r *runnerState) addIdle(ref runnerRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.idle = append(r.idle, ref)
}

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

// takeOwnership moves one runner out of state. The atomic move prevents both
// JobCompleted and process exit from cleaning it up.
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

// scaleDownIdle removes idle runners from oldest to newest.
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

func (r *runnerState) takeAllIdle() []runnerRef {
	r.mu.Lock()
	defer r.mu.Unlock()
	all := r.idle
	r.idle = make([]runnerRef, 0)
	return all
}

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

// addProtected registers a runner adopted after restart.
func (r *runnerState) addProtected(ref runnerRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.protected[ref.containerID] = ref
}

func (r *runnerState) takeProtected(containerID string) (runnerRef, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ref, ok := r.protected[containerID]
	if ok {
		delete(r.protected, containerID)
	}
	return ref, ok
}
