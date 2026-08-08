// These tests cover scaler demand and state transitions without external I/O.
package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nukanoto/arc-docker/internal/config"
	"github.com/nukanoto/arc-docker/internal/docker"
	"github.com/nukanoto/arc-docker/internal/model"
)

// ref uses deterministic IDs because these tests exercise state only.
func ref(name string) runnerRef {
	return runnerRef{containerID: "c" + name, runnerID: int64(len(name)), runnerName: name}
}

// stateWith preserves idle order and returns a pointer to avoid copying a mutex.
func stateWith(idle, busy, protected []string) *runnerState {
	st := newRunnerState()
	for _, n := range idle {
		st.addIdle(ref(n))
	}
	for _, n := range busy {
		st.busy[n] = ref(n)
	}
	for _, n := range protected {
		st.addProtected(ref(n))
	}
	return &st
}

// stateNames preserves idle order and sorts map-backed states for comparison.
func stateNames(st *runnerState) (idle, busy, protected []string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, r := range st.idle {
		idle = append(idle, r.runnerName)
	}
	for n := range st.busy {
		busy = append(busy, n)
	}
	for _, r := range st.protected {
		protected = append(protected, r.runnerName)
	}
	sort.Strings(busy)
	sort.Strings(protected)
	return idle, busy, protected
}

// TestDesiredRunnerCount covers demand clamping.
func TestDesiredRunnerCount(t *testing.T) {
	tests := []struct {
		name string
		min  int
		max  int
		jobs int
		want int
	}{
		{name: "jobs below min use min", min: 2, max: 10, jobs: 0, want: 2},
		{name: "jobs equal to min use min", min: 2, max: 10, jobs: 2, want: 2},
		{name: "jobs between min and max use jobs", min: 2, max: 10, jobs: 5, want: 5},
		{name: "jobs equal to max use max", min: 2, max: 10, jobs: 10, want: 10},
		{name: "jobs above max use max", min: 2, max: 10, jobs: 15, want: 10},
		{name: "negative jobs clamp to min", min: 2, max: 10, jobs: -3, want: 2},
		{name: "negative min and jobs clamp to zero", min: -2, max: 10, jobs: -1, want: 0},
		{name: "zero min and negative jobs clamp to zero", min: 0, max: 10, jobs: -1, want: 0},
		{name: "min above max uses max", min: 10, max: 5, jobs: 0, want: 5},
		{name: "zero max produces zero", min: 0, max: 0, jobs: 5, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := desiredRunnerCount(tt.min, tt.max, tt.jobs); got != tt.want {
				t.Fatalf("desiredRunnerCount is invalid: min=%d max=%d jobs=%d got=%d want=%d", tt.min, tt.max, tt.jobs, got, tt.want)
			}
		})
	}
}

// TestRunnerStateCount covers all runner state categories.
func TestRunnerStateCount(t *testing.T) {
	tests := []struct {
		name      string
		idle      []string
		busy      []string
		protected []string
		want      int
	}{
		{name: "empty state has zero runners", want: 0},
		{name: "counts idle runners", idle: []string{"a", "b"}, want: 2},
		{name: "counts busy runners", busy: []string{"a"}, want: 1},
		{name: "counts protected runners", protected: []string{"a", "b", "c"}, want: 3},
		{name: "counts all three states", idle: []string{"a"}, busy: []string{"b"}, protected: []string{"c"}, want: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := stateWith(tt.idle, tt.busy, tt.protected)
			if got := st.count(); got != tt.want {
				t.Fatalf("count is invalid: got=%d want=%d", got, tt.want)
			}
		})
	}
}

// TestRunnerStateMarkBusy covers the idle-to-busy transition.
func TestRunnerStateMarkBusy(t *testing.T) {
	tests := []struct {
		name     string
		idle     []string
		busy     []string
		target   string
		wantOK   bool
		wantIdle []string
		wantBusy []string
	}{
		{name: "first idle runner becomes busy", idle: []string{"a", "b"}, target: "a", wantOK: true, wantIdle: []string{"b"}, wantBusy: []string{"a"}},
		{name: "last idle runner becomes busy", idle: []string{"a", "b"}, target: "b", wantOK: true, wantIdle: []string{"a"}, wantBusy: []string{"b"}},
		{name: "empty idle state returns false", idle: nil, target: "a", wantOK: false, wantIdle: nil, wantBusy: nil},
		{name: "unknown runner returns false without changes", idle: []string{"a"}, target: "x", wantOK: false, wantIdle: []string{"a"}, wantBusy: nil},
		{name: "already busy runner returns false without changes", idle: []string{"a"}, busy: []string{"b"}, target: "b", wantOK: false, wantIdle: []string{"a"}, wantBusy: []string{"b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := stateWith(tt.idle, tt.busy, nil)
			if got := st.markBusy(tt.target); got != tt.wantOK {
				t.Fatalf("markBusy is invalid: target=%q got=%v want=%v", tt.target, got, tt.wantOK)
			}
			idle, busy, _ := stateNames(st)
			if !reflect.DeepEqual(idle, tt.wantIdle) || !reflect.DeepEqual(busy, tt.wantBusy) {
				t.Fatalf("state after markBusy(%q) differs: idle=%v busy=%v want idle=%v busy=%v",
					tt.target, idle, busy, tt.wantIdle, tt.wantBusy)
			}
		})
	}
}

// TestRunnerStateTakeOwnership covers cleanup ownership acquisition.
func TestRunnerStateTakeOwnership(t *testing.T) {
	tests := []struct {
		name     string
		idle     []string
		busy     []string
		target   string
		wantOK   bool
		wantIdle []string
		wantBusy []string
	}{
		{name: "removes a busy runner", idle: []string{"a"}, busy: []string{"b"}, target: "b", wantOK: true, wantIdle: []string{"a"}, wantBusy: nil},
		{name: "removes an idle runner", idle: []string{"a", "b"}, target: "a", wantOK: true, wantIdle: []string{"b"}, wantBusy: nil},
		{name: "busy state takes priority for duplicate names", idle: []string{"a"}, busy: []string{"a"}, target: "a", wantOK: true, wantIdle: []string{"a"}, wantBusy: nil},
		{name: "unknown runner returns false without changes", idle: []string{"a"}, busy: []string{"b"}, target: "x", wantOK: false, wantIdle: []string{"a"}, wantBusy: []string{"b"}},
		{name: "empty state returns false", target: "a", wantOK: false, wantIdle: nil, wantBusy: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := stateWith(tt.idle, tt.busy, nil)
			got, ok := st.takeOwnership(tt.target)
			if ok != tt.wantOK {
				t.Fatalf("takeOwnership is invalid: target=%q got=%v want=%v", tt.target, ok, tt.wantOK)
			}
			if tt.wantOK && got.runnerName != tt.target {
				t.Fatalf("takeOwnership(%q) returned runner name %q, want %q", tt.target, got.runnerName, tt.target)
			}
			idle, busy, _ := stateNames(st)
			if !reflect.DeepEqual(idle, tt.wantIdle) || !reflect.DeepEqual(busy, tt.wantBusy) {
				t.Fatalf("state after takeOwnership(%q) differs: idle=%v busy=%v want idle=%v busy=%v",
					tt.target, idle, busy, tt.wantIdle, tt.wantBusy)
			}
		})
	}
}

// TestRunnerStateScaleDownIdle covers oldest-first idle selection.
func TestRunnerStateScaleDownIdle(t *testing.T) {
	tests := []struct {
		name      string
		idle      []string
		busy      []string
		protected []string
		limit     int
		wantGone  []string
		wantIdle  []string
	}{
		{name: "removes only the oldest limit runners", idle: []string{"oldest", "middle", "newest"}, limit: 2, wantGone: []string{"oldest", "middle"}, wantIdle: []string{"newest"}},
		{name: "removes all idle runners when limit exceeds the count", idle: []string{"a", "b"}, limit: 5, wantGone: []string{"a", "b"}, wantIdle: nil},
		{name: "removes all idle runners when limit equals the count", idle: []string{"a", "b"}, limit: 2, wantGone: []string{"a", "b"}, wantIdle: nil},
		{name: "zero limit makes no changes", idle: []string{"a"}, limit: 0, wantGone: nil, wantIdle: []string{"a"}},
		{name: "negative limit makes no changes", idle: []string{"a"}, limit: -1, wantGone: nil, wantIdle: []string{"a"}},
		{name: "empty idle state returns no runners", limit: 3, wantGone: nil, wantIdle: nil},
		{name: "busy and protected runners are not removed", idle: []string{"a"}, busy: []string{"b"}, protected: []string{"p"}, limit: 5, wantGone: []string{"a"}, wantIdle: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := stateWith(tt.idle, tt.busy, tt.protected)
			got := st.scaleDownIdle(tt.limit)
			gotNames := make([]string, 0, len(got))
			for _, r := range got {
				gotNames = append(gotNames, r.runnerName)
			}
			if len(gotNames) == 0 {
				gotNames = nil
			}
			if !reflect.DeepEqual(gotNames, tt.wantGone) {
				t.Fatalf("scaleDownIdle(%d) removed idle=%v want=%v", tt.limit, gotNames, tt.wantGone)
			}
			idle, busy, protected := stateNames(st)
			if !reflect.DeepEqual(idle, tt.wantIdle) {
				t.Fatalf("idle state after scaleDownIdle(%d)=%v want=%v", tt.limit, idle, tt.wantIdle)
			}
			wantBusy := append([]string(nil), tt.busy...)
			sort.Strings(wantBusy)
			wantProtected := append([]string(nil), tt.protected...)
			sort.Strings(wantProtected)
			if !reflect.DeepEqual(busy, wantBusy) || !reflect.DeepEqual(protected, wantProtected) {
				t.Fatalf("scaleDownIdle(%d) changed busy/protected: busy=%v protected=%v want busy=%v protected=%v",
					tt.limit, busy, protected, wantBusy, wantProtected)
			}
		})
	}
}

// TestRunnerStateProtected covers restart-adopted runners.
func TestRunnerStateProtected(t *testing.T) {
	tests := []struct {
		name       string
		protected  []string
		target     string
		wantTaken  bool
		wantRemain []string
	}{
		{name: "known runner is removed by container ID", protected: []string{"a", "b"}, target: "a", wantTaken: true, wantRemain: []string{"b"}},
		{name: "unknown runner returns false without changes", protected: []string{"a"}, target: "x", wantTaken: false, wantRemain: []string{"a"}},
		{name: "empty state returns false", target: "a", wantTaken: false, wantRemain: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := stateWith(nil, nil, tt.protected)
			got, ok := st.takeProtected("c" + tt.target)
			if ok != tt.wantTaken {
				t.Fatalf("takeProtected is invalid: target=%q got=%v want=%v", tt.target, ok, tt.wantTaken)
			}
			if tt.wantTaken && got.containerID != "c"+tt.target {
				t.Fatalf("takeProtected(%q) returned container ID %q, want %q", tt.target, got.containerID, "c"+tt.target)
			}
			_, _, protected := stateNames(st)
			if !reflect.DeepEqual(protected, tt.wantRemain) {
				t.Fatalf("protected state after takeProtected(%q)=%v want=%v", tt.target, protected, tt.wantRemain)
			}
		})
	}
}

// TestRunnerStateTakeAll covers shutdown ownership transfer.
func TestRunnerStateTakeAll(t *testing.T) {
	st := stateWith([]string{"a", "b"}, []string{"c"}, []string{"p"})
	allIdle := st.takeAllIdle()
	allBusy := st.takeAllBusy()
	if len(allIdle) != 2 || len(allBusy) != 1 {
		t.Fatalf("takeAllIdle() returned %d and takeAllBusy() returned %d; want 2 and 1", len(allIdle), len(allBusy))
	}
	if st.count() != 1 {
		t.Fatalf("count() after removing idle and busy runners=%d, want 1 protected runner", st.count())
	}
	_, _, protected := stateNames(st)
	if !reflect.DeepEqual(protected, []string{"p"}) {
		t.Fatalf("protected runners after removal=%v, want [p]", protected)
	}
}

// TestScaler_NilEventReturnsFixedError covers invalid listener events.
func TestScaler_NilEventReturnsFixedError(t *testing.T) {
	s := &DockerScaler{state: newRunnerState()}
	if err := s.HandleJobStarted(context.Background(), nil); err == nil || err.Error() != "controller: nil job started event" {
		t.Fatalf("nil JobStarted error differs from expectation: %v", err)
	}
	if err := s.HandleJobCompleted(context.Background(), nil); err == nil || err.Error() != "controller: nil job completed event" {
		t.Fatalf("nil JobCompleted error differs from expectation: %v", err)
	}
	if got := s.state.count(); got != 0 {
		t.Fatalf("nil event changed state: count=%d", got)
	}
}

// TestRunnerRefFromLabels covers malformed runner IDs.
func TestRunnerRefFromLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		wantOK bool
	}{
		{name: "positive integer is restored", labels: map[string]string{model.RunnerIDLabelKey: "42", model.RunnerNameLabelKey: "runner-42"}, wantOK: true},
		{name: "non-integer is malformed", labels: map[string]string{model.RunnerIDLabelKey: "abc"}, wantOK: false},
		{name: "empty value is malformed", labels: map[string]string{model.RunnerIDLabelKey: ""}, wantOK: false},
		{name: "zero is malformed", labels: map[string]string{model.RunnerIDLabelKey: "0"}, wantOK: false},
		{name: "negative value is malformed", labels: map[string]string{model.RunnerIDLabelKey: "-1"}, wantOK: false},
		{name: "missing label is malformed", labels: map[string]string{}, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, err := runnerRefFromLabels("c1", tt.labels)
			if tt.wantOK {
				if err != nil {
					t.Fatalf("runnerRefFromLabels returned an error: %v", err)
				}
				if ref.containerID != "c1" || ref.runnerID != 42 || ref.runnerName != "runner-42" {
					t.Fatalf("restored runnerRef differs from expectation: %+v", ref)
				}
				return
			}
			if err == nil {
				t.Fatalf("malformed label did not return an error: %+v", ref)
			}
		})
	}
}

// TestScaler_WatchStartShutdownRace protects the WaitGroup registration rule.
func TestScaler_WatchStartShutdownRace(t *testing.T) {
	dc, err := docker.New("unix:///tmp/ghadc-unit-test-nonexistent.sock", time.Second)
	if err != nil {
		t.Fatalf("docker.New failed: %v", err)
	}
	defer dc.Close()
	watchCtx, watchCancel := context.WithCancel(context.Background())
	s := &DockerScaler{
		dockerClient:   dc,
		cleanupTimeout: time.Second,
		errCh:          make(chan error, 1),
		watchCtx:       watchCtx,
		watchCancel:    watchCancel,
		state:          newRunnerState(),
	}
	// The unit test must not connect to a real Docker socket.
	s.watchCancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			s.startWatch(ref(fmt.Sprintf("r%d", i)), false)
		}
	}()
	go func() {
		defer wg.Done()
		if err := s.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown failed to join watches: %v", err)
		}
	}()
	wg.Wait()

	s.startWatch(ref("after-shutdown"), false)
	if !s.watchStopped {
		t.Fatalf("watchStopped is false after Shutdown")
	}
	if got := s.state.count(); got != 0 {
		t.Fatalf("startWatch changed state after Shutdown: count=%d", got)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown failed: %v", err)
	}
}

// TestScaler_ShutdownTimesOutWithoutCleanup covers the join timeout boundary.
func TestScaler_ShutdownTimesOutWithoutCleanup(t *testing.T) {
	watchCtx, watchCancel := context.WithCancel(context.Background())
	s := &DockerScaler{
		cleanupTimeout: time.Second,
		errCh:          make(chan error, 1),
		watchCtx:       watchCtx,
		watchCancel:    watchCancel,
		state:          newRunnerState(),
	}
	s.state.addIdle(ref("keep"))

	release := make(chan struct{})
	s.wg.Add(1)
	go func() {
		<-release
		s.wg.Done()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := s.Shutdown(ctx)
	if !errors.Is(err, ErrShutdownJoinTimeout) {
		t.Fatalf("Shutdown did not return ErrShutdownJoinTimeout while a watch was active: %v", err)
	}
	if got := s.state.count(); got != 1 {
		t.Fatalf("state changed on timeout: count=%d (want 1)", got)
	}
	if !s.watchStopped {
		t.Fatalf("watchStopped is false after timeout")
	}

	close(release)
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("watch goroutine did not finish after releasing the gate")
	}
}

// TestScaler_ShutdownReturnsTrueWhenWatchDrains covers a drained shutdown.
func TestScaler_ShutdownReturnsTrueWhenWatchDrains(t *testing.T) {
	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()
	s := &DockerScaler{
		cleanupTimeout: time.Second,
		errCh:          make(chan error, 1),
		watchCtx:       watchCtx,
		watchCancel:    watchCancel,
		state:          newRunnerState(),
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown returned an error with no watches: %v", err)
	}
}

// TestScaler_ShutdownReturnsCleanupErrors covers joined cleanup failures.
func TestScaler_ShutdownReturnsCleanupErrors(t *testing.T) {
	dc, err := docker.New("unix:///tmp/ghadc-unit-test-nonexistent.sock", time.Second)
	if err != nil {
		t.Fatalf("docker.New failed: %v", err)
	}
	defer dc.Close()
	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()
	s := &DockerScaler{
		dockerClient:   dc,
		cleanupTimeout: time.Second,
		errCh:          make(chan error, 1),
		watchCtx:       watchCtx,
		watchCancel:    watchCancel,
		state:          newRunnerState(),
	}
	s.state.addIdle(ref("idle-1"))
	s.state.addIdle(ref("busy-1"))
	s.state.addIdle(ref("idle-2"))
	s.state.markBusy("busy-1")
	s.busyPolicy = config.ShutdownPolicyStop

	err = s.Shutdown(context.Background())
	if err == nil {
		t.Fatalf("Shutdown returned nil despite cleanup failures")
	}
	if errors.Is(err, ErrShutdownJoinTimeout) {
		t.Fatalf("cleanup failure was misclassified as a join timeout: %v", err)
	}
	if got := strings.Count(err.Error(), "shutdown cleanup container"); got != 3 {
		t.Fatalf("unexpected number of joined cleanup errors: %d (%v)", got, err)
	}
	select {
	case waitErr := <-s.ErrCh():
		t.Fatalf("Shutdown reported an error on errCh: %v", waitErr)
	default:
	}
}

// TestScaler_CleanupContextIsFresh covers independent cleanup deadlines.
func TestScaler_CleanupContextIsFresh(t *testing.T) {
	s := &DockerScaler{cleanupTimeout: 200 * time.Millisecond}
	cctx, ccancel := s.cleanupContext()
	defer ccancel()
	if err := cctx.Err(); err != nil {
		t.Fatalf("fresh context is already canceled: %v", err)
	}
	deadline, ok := cctx.Deadline()
	if !ok {
		t.Fatalf("cleanup context has no deadline")
	}
	if d := time.Until(deadline); d < 100*time.Millisecond || d > 200*time.Millisecond {
		t.Fatalf("cleanup context deadline differs from expectation: %v", d)
	}
}

// TestScaler_ReleaseWatchOwnership covers external removal bookkeeping.
func TestScaler_ReleaseWatchOwnership(t *testing.T) {
	idle := ref("idle-runner")
	protected := ref("protected-runner")
	s := &DockerScaler{state: newRunnerState()}
	s.state.addIdle(idle)
	s.state.addProtected(protected)

	if !s.releaseWatchOwnership(idle, false) {
		t.Fatalf("failed to release idle runner ownership")
	}
	if !s.releaseWatchOwnership(protected, true) {
		t.Fatalf("failed to release protected runner ownership")
	}
	if got := s.state.count(); got != 0 {
		t.Fatalf("runner remains in capacity after release: %d", got)
	}
	if s.releaseWatchOwnership(idle, false) || s.releaseWatchOwnership(protected, true) {
		t.Fatalf("the same runner ownership was released twice")
	}
}
