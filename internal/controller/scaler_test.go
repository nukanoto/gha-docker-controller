// scaler_test.go verifies only the pure parts of DockerScaler with table
// tests: the desired formula, scale-down selection (oldest first), and state
// map transitions. No Docker/GitHub I/O happens. No mocks, stubs, or fakes.
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

	"github.com/nukanoto/gha-docker-controller/internal/config"
	"github.com/nukanoto/gha-docker-controller/internal/docker"
	"github.com/nukanoto/gha-docker-controller/internal/model"
)

// ref builds a test runnerRef. The container ID is derived from the name and
// the runner ID is a deterministic stand-in (the name length). Tests never
// assert on the runner ID value.
func ref(name string) runnerRef {
	return runnerRef{containerID: "c" + name, runnerID: int64(len(name)), runnerName: name}
}

// stateWith builds a test state from idle/busy/protected name lists. idle
// keeps the given order; busy/protected are maps. It returns a pointer to
// avoid copying the mutex.
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

// stateNames returns the name lists of the state for assertions. idle keeps
// its order; busy/protected are sorted to be comparable.
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

// TestDesiredRunnerCount verifies the desired formula
// clamp(max(min, jobs), 0, max) with table tests. The scaler uses this
// formula as its single source of demand.
func TestDesiredRunnerCount(t *testing.T) {
	tests := []struct {
		name string
		min  int
		max  int
		jobs int
		want int
	}{
		{name: "job 数が min 未満なら min", min: 2, max: 10, jobs: 0, want: 2},
		{name: "job 数が min と等しいなら min", min: 2, max: 10, jobs: 2, want: 2},
		{name: "job 数が min と max の間なら job 数", min: 2, max: 10, jobs: 5, want: 5},
		{name: "job 数が max と等しいなら max", min: 2, max: 10, jobs: 10, want: 10},
		{name: "job 数が max を超えたら max", min: 2, max: 10, jobs: 15, want: 10},
		{name: "負の job 数は min 側へ clamp", min: 2, max: 10, jobs: -3, want: 2},
		{name: "min と job 数が負なら 0", min: -2, max: 10, jobs: -1, want: 0},
		{name: "min が 0 で負の job 数なら 0", min: 0, max: 10, jobs: -1, want: 0},
		{name: "min が max を超える場合は max", min: 10, max: 5, jobs: 0, want: 5},
		{name: "max が 0 なら 0", min: 0, max: 0, jobs: 5, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := desiredRunnerCount(tt.min, tt.max, tt.jobs); got != tt.want {
				t.Fatalf("desiredRunnerCount の結果が不正です: min=%d max=%d jobs=%d、実測値=%d、期待値=%d", tt.min, tt.max, tt.jobs, got, tt.want)
			}
		})
	}
}

// TestRunnerStateCount verifies that count is the total of idle, busy, and
// protected, with table tests.
func TestRunnerStateCount(t *testing.T) {
	tests := []struct {
		name      string
		idle      []string
		busy      []string
		protected []string
		want      int
	}{
		{name: "空状態は 0", want: 0},
		{name: "idle だけを数える", idle: []string{"a", "b"}, want: 2},
		{name: "busy だけを数える", busy: []string{"a"}, want: 1},
		{name: "protected だけを数える", protected: []string{"a", "b", "c"}, want: 3},
		{name: "3 状態の合計を数える", idle: []string{"a"}, busy: []string{"b"}, protected: []string{"c"}, want: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := stateWith(tt.idle, tt.busy, tt.protected)
			if got := st.count(); got != tt.want {
				t.Fatalf("count の結果が不正です: 実測値=%d、期待値=%d", got, tt.want)
			}
		})
	}
}

// TestRunnerStateMarkBusy verifies the JobStarted transition with table
// tests. Only a known idle runner moves to busy; unknown names return false
// without changes.
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
		{name: "先頭の idle が busy へ移る", idle: []string{"a", "b"}, target: "a", wantOK: true, wantIdle: []string{"b"}, wantBusy: []string{"a"}},
		{name: "末尾の idle が busy へ移る", idle: []string{"a", "b"}, target: "b", wantOK: true, wantIdle: []string{"a"}, wantBusy: []string{"b"}},
		{name: "idle が空なら false", idle: nil, target: "a", wantOK: false, wantIdle: nil, wantBusy: nil},
		{name: "unknown は無変更で false", idle: []string{"a"}, target: "x", wantOK: false, wantIdle: []string{"a"}, wantBusy: nil},
		{name: "busy 済みの name は無変更で false", idle: []string{"a"}, busy: []string{"b"}, target: "b", wantOK: false, wantIdle: []string{"a"}, wantBusy: []string{"b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := stateWith(tt.idle, tt.busy, nil)
			if got := st.markBusy(tt.target); got != tt.wantOK {
				t.Fatalf("markBusy の結果が不正です: target=%q、実測値=%v、期待値=%v", tt.target, got, tt.wantOK)
			}
			idle, busy, _ := stateNames(st)
			if !reflect.DeepEqual(idle, tt.wantIdle) || !reflect.DeepEqual(busy, tt.wantBusy) {
				t.Fatalf("markBusy(%q) 後の state が不一致: idle=%v busy=%v, want idle=%v busy=%v",
					tt.target, idle, busy, tt.wantIdle, tt.wantBusy)
			}
		})
	}
}

// TestRunnerStateTakeOwnership verifies ownership acquisition by JobCompleted
// and wait exit with table tests. Removal is busy-first; unknown names return
// false without changes.
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
		{name: "busy の runner を除去する", idle: []string{"a"}, busy: []string{"b"}, target: "b", wantOK: true, wantIdle: []string{"a"}, wantBusy: nil},
		{name: "idle の runner を除去する", idle: []string{"a", "b"}, target: "a", wantOK: true, wantIdle: []string{"b"}, wantBusy: nil},
		{name: "同名は busy 優先で除去する", idle: []string{"a"}, busy: []string{"a"}, target: "a", wantOK: true, wantIdle: []string{"a"}, wantBusy: nil},
		{name: "unknown は無変更で false", idle: []string{"a"}, busy: []string{"b"}, target: "x", wantOK: false, wantIdle: []string{"a"}, wantBusy: []string{"b"}},
		{name: "空状態は false", target: "a", wantOK: false, wantIdle: nil, wantBusy: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := stateWith(tt.idle, tt.busy, nil)
			got, ok := st.takeOwnership(tt.target)
			if ok != tt.wantOK {
				t.Fatalf("takeOwnership の結果が不正です: target=%q、実測値=%v、期待値=%v", tt.target, ok, tt.wantOK)
			}
			if tt.wantOK && got.runnerName != tt.target {
				t.Fatalf("takeOwnership(%q) が返した ref の name = %q, want %q", tt.target, got.runnerName, tt.target)
			}
			idle, busy, _ := stateNames(st)
			if !reflect.DeepEqual(idle, tt.wantIdle) || !reflect.DeepEqual(busy, tt.wantBusy) {
				t.Fatalf("takeOwnership(%q) 後の state が不一致: idle=%v busy=%v, want idle=%v busy=%v",
					tt.target, idle, busy, tt.wantIdle, tt.wantBusy)
			}
		})
	}
}

// TestRunnerStateScaleDownIdle verifies scale-down selection with table
// tests. Only up to limit idle runners are removed, oldest first; busy and
// protected are never targets.
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
		{name: "古い順に limit 個だけ除去する", idle: []string{"oldest", "middle", "newest"}, limit: 2, wantGone: []string{"oldest", "middle"}, wantIdle: []string{"newest"}},
		{name: "limit が idle 数を超えたら全部除去する", idle: []string{"a", "b"}, limit: 5, wantGone: []string{"a", "b"}, wantIdle: nil},
		{name: "limit と idle 数が等しいら全部除去する", idle: []string{"a", "b"}, limit: 2, wantGone: []string{"a", "b"}, wantIdle: nil},
		{name: "limit 0 は何もしない", idle: []string{"a"}, limit: 0, wantGone: nil, wantIdle: []string{"a"}},
		{name: "負の limit は何もしない", idle: []string{"a"}, limit: -1, wantGone: nil, wantIdle: []string{"a"}},
		{name: "idle が空なら空を返す", limit: 3, wantGone: nil, wantIdle: nil},
		{name: "busy と protected は除去しない", idle: []string{"a"}, busy: []string{"b"}, protected: []string{"p"}, limit: 5, wantGone: []string{"a"}, wantIdle: nil},
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
				t.Fatalf("scaleDownIdle(%d) が除去した idle = %v, want %v", tt.limit, gotNames, tt.wantGone)
			}
			idle, busy, protected := stateNames(st)
			if !reflect.DeepEqual(idle, tt.wantIdle) {
				t.Fatalf("scaleDownIdle(%d) 後の idle = %v, want %v", tt.limit, idle, tt.wantIdle)
			}
			wantBusy := append([]string(nil), tt.busy...)
			sort.Strings(wantBusy)
			wantProtected := append([]string(nil), tt.protected...)
			sort.Strings(wantProtected)
			if !reflect.DeepEqual(busy, wantBusy) || !reflect.DeepEqual(protected, wantProtected) {
				t.Fatalf("scaleDownIdle(%d) が busy/protected を変更した: busy=%v protected=%v, want busy=%v protected=%v",
					tt.limit, busy, protected, wantBusy, wantProtected)
			}
		})
	}
}

// TestRunnerStateProtected verifies registration and removal of protected
// runners after restart, with table tests. addProtected includes them in
// count; takeProtected removes by container ID.
func TestRunnerStateProtected(t *testing.T) {
	tests := []struct {
		name       string
		protected  []string
		target     string
		wantTaken  bool
		wantRemain []string
	}{
		{name: "known は container ID で除去される", protected: []string{"a", "b"}, target: "a", wantTaken: true, wantRemain: []string{"b"}},
		{name: "unknown は無変更で false", protected: []string{"a"}, target: "x", wantTaken: false, wantRemain: []string{"a"}},
		{name: "空状態は false", target: "a", wantTaken: false, wantRemain: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := stateWith(nil, nil, tt.protected)
			got, ok := st.takeProtected("c" + tt.target)
			if ok != tt.wantTaken {
				t.Fatalf("takeProtected の結果が不正です: target=%q、実測値=%v、期待値=%v", tt.target, ok, tt.wantTaken)
			}
			if tt.wantTaken && got.containerID != "c"+tt.target {
				t.Fatalf("takeProtected(%q) が返した container ID = %q, want %q", tt.target, got.containerID, "c"+tt.target)
			}
			_, _, protected := stateNames(st)
			if !reflect.DeepEqual(protected, tt.wantRemain) {
				t.Fatalf("takeProtected(%q) 後の protected = %v, want %v", tt.target, protected, tt.wantRemain)
			}
		})
	}
}

// TestRunnerStateTakeAll verifies the shutdown bulk removal with table tests.
// takeAllIdle and takeAllBusy empty the state and the change is reflected in
// count.
func TestRunnerStateTakeAll(t *testing.T) {
	st := stateWith([]string{"a", "b"}, []string{"c"}, []string{"p"})
	allIdle := st.takeAllIdle()
	allBusy := st.takeAllBusy()
	if len(allIdle) != 2 || len(allBusy) != 1 {
		t.Fatalf("takeAllIdle() が %d 個、takeAllBusy() が %d 個, want 2 個と 1 個", len(allIdle), len(allBusy))
	}
	// protected stays; idle/busy disappear from count.
	if st.count() != 1 {
		t.Fatalf("全量除去後の count() = %d, want 1 (protected のみ)", st.count())
	}
	_, _, protected := stateNames(st)
	if !reflect.DeepEqual(protected, []string{"p"}) {
		t.Fatalf("全量除去後の protected = %v, want [p]", protected)
	}
}

// TestScaler_NilEventReturnsFixedError verifies that a nil JobStarted /
// JobCompleted event returns a fixed error and does not change state. The
// official listener passes events as pointers, so nil is a protocol violation
// that becomes fatal through the listener.
func TestScaler_NilEventReturnsFixedError(t *testing.T) {
	s := &DockerScaler{state: newRunnerState()}
	if err := s.HandleJobStarted(context.Background(), nil); err == nil || err.Error() != "controller: nil job started event" {
		t.Fatalf("nil JobStarted の error が期待と異なります: %v", err)
	}
	if err := s.HandleJobCompleted(context.Background(), nil); err == nil || err.Error() != "controller: nil job completed event" {
		t.Fatalf("nil JobCompleted の error が期待と異なります: %v", err)
	}
	if got := s.state.count(); got != 0 {
		t.Fatalf("nil event が state を変更しました: count=%d", got)
	}
}

// TestRunnerRefFromLabels verifies runnerRef restoration from labels with
// table tests. The runner-id label must be a positive base-10 integer;
// non-integers, 0, and negatives are malformed and return an error (Recover
// becomes fatal without changing the container).
func TestRunnerRefFromLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		wantOK bool
	}{
		{name: "正の整数は復元できる", labels: map[string]string{model.RunnerIDLabelKey: "42", model.RunnerNameLabelKey: "runner-42"}, wantOK: true},
		{name: "非整数は malformed", labels: map[string]string{model.RunnerIDLabelKey: "abc"}, wantOK: false},
		{name: "空文字列は malformed", labels: map[string]string{model.RunnerIDLabelKey: ""}, wantOK: false},
		{name: "0 は malformed", labels: map[string]string{model.RunnerIDLabelKey: "0"}, wantOK: false},
		{name: "負数は malformed", labels: map[string]string{model.RunnerIDLabelKey: "-1"}, wantOK: false},
		{name: "label 不在は malformed", labels: map[string]string{}, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, err := runnerRefFromLabels("c1", tt.labels)
			if tt.wantOK {
				if err != nil {
					t.Fatalf("runnerRefFromLabels が error を返しました: %v", err)
				}
				if ref.containerID != "c1" || ref.runnerID != 42 || ref.runnerName != "runner-42" {
					t.Fatalf("復元した runnerRef が期待と異なります: %+v", ref)
				}
				return
			}
			if err == nil {
				t.Fatalf("malformed label なのに error を返しませんでした: %+v", ref)
			}
		})
	}
}

// TestScaler_WatchStartShutdownRace verifies under -race that startWatch's
// wg.Add and Shutdown's wg.Wait running concurrently do not misuse the
// WaitGroup, and that shutdown joins every watch. The test cancels watchCtx
// first so watch goroutines never connect to a real Docker socket (real
// socket connections are excluded from unit tests).
func TestScaler_WatchStartShutdownRace(t *testing.T) {
	dc, err := docker.New("unix:///tmp/ghadc-unit-test-nonexistent.sock", time.Second)
	if err != nil {
		t.Fatalf("docker.New が失敗しました: %v", err)
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
	// Cancel first so the watch goroutines never connect to the socket.
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
			t.Errorf("Shutdown が watch join に失敗しました: %v", err)
		}
	}()
	wg.Wait()

	// startWatch after shutdown is a no-op without wg.Add, and a second
	// Shutdown completes immediately (no deadlock on double call).
	s.startWatch(ref("after-shutdown"), false)
	if !s.watchStopped {
		t.Fatalf("Shutdown 後に watchStopped が false のままです")
	}
	if got := s.state.count(); got != 0 {
		t.Fatalf("startWatch が state を変更しました: count=%d", got)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("2 回目の Shutdown が失敗しました: %v", err)
	}
}

// TestScaler_ShutdownTimesOutWithoutCleanup verifies that Shutdown returns
// ErrShutdownJoinTimeout without cleanup when watch join exceeds the deadline,
// using a real wg and real ctx. On timeout, takeAllIdle is not called, so the
// idle refs stay in state (not cleaned up).
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

	// Hold watch completion behind a gate.
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
		t.Fatalf("watch 未完了なのに Shutdown が ErrShutdownJoinTimeout を返しませんでした: %v", err)
	}
	// On timeout, cleanup does not run and the idle refs stay in state.
	if got := s.state.count(); got != 1 {
		t.Fatalf("timeout 時に state が変更されました: count=%d (want 1)", got)
	}
	if !s.watchStopped {
		t.Fatalf("timeout 後も watchStopped が false のままです")
	}

	// Release the gate and confirm the watch goroutine exits (leak prevention).
	close(release)
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("gate 解放後も watch goroutine が終了しませんでした")
	}
}

// TestScaler_ShutdownReturnsTrueWhenWatchDrains verifies that Shutdown
// returns nil after every watch completes. With empty state, cleanup involves
// no I/O.
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
		t.Fatalf("watch が空なのに Shutdown が error を返しました: %v", err)
	}
}

// TestScaler_ShutdownReturnsCleanupErrors verifies that Shutdown returns
// idle/busy cleanup failures via errors.Join and does not notify errCh.
// Cleanup fails because the real client's inspect connects to a nonexistent
// socket (real Docker socket connections are excluded from unit tests).
func TestScaler_ShutdownReturnsCleanupErrors(t *testing.T) {
	dc, err := docker.New("unix:///tmp/ghadc-unit-test-nonexistent.sock", time.Second)
	if err != nil {
		t.Fatalf("docker.New が失敗しました: %v", err)
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
	// Lead 3 idle and busy refs into cleanup failure.
	s.state.addIdle(ref("idle-1"))
	s.state.addIdle(ref("busy-1"))
	s.state.addIdle(ref("idle-2"))
	s.state.markBusy("busy-1")
	s.busyPolicy = config.ShutdownPolicyStop

	err = s.Shutdown(context.Background())
	if err == nil {
		t.Fatalf("cleanup 失敗なのに Shutdown が nil を返しました")
	}
	if errors.Is(err, ErrShutdownJoinTimeout) {
		t.Fatalf("cleanup 失敗が join timeout と誤分類されました: %v", err)
	}
	// The 3 refs' cleanup errors are joined.
	if got := strings.Count(err.Error(), "shutdown cleanup container"); got != 3 {
		t.Fatalf("Join された cleanup error の数が期待と異なります: %d (%v)", got, err)
	}
	// errCh is not notified (returning to the caller is the only path).
	select {
	case waitErr := <-s.ErrCh():
		t.Fatalf("Shutdown が errCh へ error を通知しました: %v", waitErr)
	default:
	}
}

// TestScaler_CleanupContextIsFresh verifies that cleanupContext returns a
// fresh context derived from Background. It is a regression test that
// cancelling watchCtx or the handler ctx does not interrupt cleanup, and the
// deadline equals cleanupTimeout.
func TestScaler_CleanupContextIsFresh(t *testing.T) {
	s := &DockerScaler{cleanupTimeout: 200 * time.Millisecond}
	cctx, ccancel := s.cleanupContext()
	defer ccancel()
	if err := cctx.Err(); err != nil {
		t.Fatalf("fresh context が cancel 済みです: %v", err)
	}
	deadline, ok := cctx.Deadline()
	if !ok {
		t.Fatalf("cleanup context に deadline がありません")
	}
	if d := time.Until(deadline); d < 100*time.Millisecond || d > 200*time.Millisecond {
		t.Fatalf("cleanup context の deadline が期待と異なります: %v", d)
	}
}

// TestScaler_ReleaseWatchOwnership verifies that idle and protected ownership
// can be released so an externally removed container is not counted in
// capacity.
func TestScaler_ReleaseWatchOwnership(t *testing.T) {
	idle := ref("idle-runner")
	protected := ref("protected-runner")
	s := &DockerScaler{state: newRunnerState()}
	s.state.addIdle(idle)
	s.state.addProtected(protected)

	if !s.releaseWatchOwnership(idle, false) {
		t.Fatalf("idle runner の所有権を解放できませんでした")
	}
	if !s.releaseWatchOwnership(protected, true) {
		t.Fatalf("protected runner の所有権を解放できませんでした")
	}
	if got := s.state.count(); got != 0 {
		t.Fatalf("解放後も runner が capacity に残っています: %d", got)
	}
	if s.releaseWatchOwnership(idle, false) || s.releaseWatchOwnership(protected, true) {
		t.Fatalf("同じ runner の所有権を二重に解放できました")
	}
}
