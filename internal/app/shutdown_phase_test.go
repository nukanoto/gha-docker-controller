// shutdown_phase_test.go verifies shutdown's phase contexts, listener join,
// and the minimal coordinator with real objects. It fixes that earlier-phase
// contexts are never reused and that each phase's timeout and join
// completion conditions hold.
package app

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nukanoto/gha-docker-controller/internal/config"
	"github.com/nukanoto/gha-docker-controller/internal/health"
)

// TestShutdown_WaitListenerTimesOutAtDeadline verifies with real wg/ctx that
// waitListener returns false at ctx's deadline before wg completes and that
// the internal goroutine also finishes after the deadline (context-aware
// wait). joined=false on timeout is the input for shutdown deciding to give
// up the join and proceed to the remaining phases.
func TestShutdown_WaitListenerTimesOutAtDeadline(t *testing.T) {
	a := newShutdownTestApp(&config.Config{})
	release := make(chan struct{})
	addWaitGate(a, release)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	if a.waitListener(ctx) {
		t.Fatalf("deadline 前に waitListener が true を返しました")
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("waitListener が deadline より早く戻りました: %v", elapsed)
	}
	// Confirm the internal goroutine finishes after release (leak
	// prevention).
	close(release)
	wgDone := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(wgDone)
	}()
	select {
	case <-wgDone:
	case <-time.After(time.Second):
		t.Fatalf("waitListener の内部 goroutine が終了しませんでした")
	}
}

// TestShutdown_WaitListenerReturnsTrueWhenDrained verifies with real wg/ctx
// that waitListener returns true after wg completes.
func TestShutdown_WaitListenerReturnsTrueWhenDrained(t *testing.T) {
	a := newShutdownTestApp(&config.Config{})
	release := make(chan struct{})
	addWaitGate(a, release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	start := time.Now()
	close(release)
	if !a.waitListener(ctx) {
		t.Fatalf("wg 完了後に waitListener が false を返しました")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("waitListener の完了に時間がかかりすぎました: %v", elapsed)
	}
}

// TestShutdown_ListenerJoinTimeout verifies that the listener join wait
// bound is max(provisioning timeout, cleanup timeout). The cleanup timeout
// comes from the shutdown grace like the scaler (default when non-positive),
// and the official listener calls handlers with context.WithoutCancel, so
// the running handler execution bound becomes the join wait bound.
func TestShutdown_ListenerJoinTimeout(t *testing.T) {
	cases := []struct {
		name      string
		provision config.Duration
		grace     config.Duration
		want      time.Duration
	}{
		{name: "provisioning が長ければ provisioning", provision: config.Duration(90 * time.Second), grace: config.Duration(30 * time.Second), want: 90 * time.Second},
		{name: "cleanup が長ければ cleanup", provision: config.Duration(30 * time.Second), grace: config.Duration(90 * time.Second), want: 90 * time.Second},
		{name: "等しければその値", provision: config.Duration(60 * time.Second), grace: config.Duration(60 * time.Second), want: 60 * time.Second},
		{name: "provisioning 非正は既定 5 分と比較", provision: 0, grace: config.Duration(10 * time.Second), want: time.Duration(config.DefaultProvisioningTimeout)},
		{name: "grace 非正は既定 2 分と比較", provision: config.Duration(10 * time.Second), grace: 0, want: time.Duration(config.DefaultShutdownGrace)},
		{name: "両方非正なら既定の大きい方", provision: -1, grace: -1, want: time.Duration(config.DefaultProvisioningTimeout)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newShutdownTestApp(&config.Config{
				Runner:   config.RunnerConfig{ProvisioningTimeout: tc.provision},
				Shutdown: config.ShutdownConfig{Grace: tc.grace},
			})
			if got := a.listenerJoinTimeout(); got != tc.want {
				t.Fatalf("listenerJoinTimeout が期待と異なります: got %v want %v", got, tc.want)
			}
		})
	}
}

// TestShutdown_JoinTimeoutWarningIsFixed confirms that the join timeout
// warnings are fixed English messages without dynamic information or
// errors. Log output is not swapped; only the warning values are verified.
func TestShutdown_JoinTimeoutWarningIsFixed(t *testing.T) {
	for _, want := range []string{listenerJoinTimeoutWarning, scalerJoinTimeoutWarning} {
		if want == "" {
			t.Fatalf("join timeout warning が空です")
		}
		if strings.ContainsAny(want, "{}") {
			t.Fatalf("join timeout warning に動的な値が含まれています: %q", want)
		}
	}
}

// TestShutdown_ReadinessDropsImmediately verifies with a real loopback
// server that right after shutdown phase 0 (run cancel) the
// session/listener states flip to false and /readyz returns 503 while the
// health server is still running. The listener join (phase 1) is held with a
// wg gate to deterministically observe the state before phase 3 (health
// shutdown).
func TestShutdown_ReadinessDropsImmediately(t *testing.T) {
	a := newShutdownTestApp(&config.Config{
		Shutdown: config.ShutdownConfig{
			BusyPolicy: config.ShutdownPolicyLeave,
			Grace:      config.Duration(time.Second),
		},
		Runner: config.RunnerConfig{ProvisioningTimeout: config.Duration(time.Second)},
	})
	store := health.NewStore()
	a.store = store
	hs, err := health.New("127.0.0.1:0", store, a.logger)
	if err != nil {
		t.Fatalf("health.New が error を返した: %v", err)
	}
	if err := hs.Start(); err != nil {
		t.Fatalf("health.Start が error を返した: %v", err)
	}
	a.health = hs
	store.SetSessionRunning(true)
	store.SetListenerRunning(true)

	// Hold the listener join (phase 1) to observe the state after phase 0.
	release := make(chan struct{})
	addWaitGate(a, release)

	done := make(chan error)
	go func() {
		done <- a.shutdown()
	}()

	// Wait until the store flips to false right after the phase 0 cancel.
	deadline := time.Now().Add(3 * time.Second)
	for store.Ready() {
		if time.Now().After(deadline) {
			t.Fatalf("shutdown の cancel 後も readiness が false になりませんでした")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The health server is still running, so /readyz returns 503.
	resp, err := http.Get("http://" + hs.Addr().String() + "/readyz")
	if err != nil {
		t.Fatalf("/readyz への GET が error を返した: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("shutdown 中の /readyz が %d を返しました (want 503)", resp.StatusCode)
	}

	// Release phase 1 and confirm shutdown completes.
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown が error を返しました: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("shutdown が完了しませんでした")
	}
}

// TestShutdown_ListenerJoinTimeoutSkipsRemainingPhases verifies with real
// objects that on a listener join timeout shutdown returns immediately
// without running later phases (health shutdown, lock release). This is the
// contract that structurally prevents races from closing components in use
// by running handlers; the health server keeps answering and the lock is not
// released, confirmed with real objects.
func TestShutdown_ListenerJoinTimeoutSkipsRemainingPhases(t *testing.T) {
	joinTimeout := 100 * time.Millisecond
	a := newShutdownTestApp(&config.Config{
		Shutdown: config.ShutdownConfig{
			BusyPolicy: config.ShutdownPolicyLeave,
			Grace:      config.Duration(joinTimeout),
		},
		Runner: config.RunnerConfig{ProvisioningTimeout: config.Duration(joinTimeout)},
	})
	store := health.NewStore()
	a.store = store
	hs, err := health.New("127.0.0.1:0", store, a.logger)
	if err != nil {
		t.Fatalf("health.New が error を返した: %v", err)
	}
	if err := hs.Start(); err != nil {
		t.Fatalf("health.Start が error を返した: %v", err)
	}
	a.health = hs
	// Also observe that the lock is not released, so pass a lockFile holding
	// a real file.
	lock, err := os.CreateTemp(t.TempDir(), "lock")
	if err != nil {
		t.Fatalf("CreateTemp が error を返した: %v", err)
	}
	a.lock = &lockFile{f: lock}

	// Hold the listener until the join timeout.
	release := make(chan struct{})
	addWaitGate(a, release)

	done := make(chan error)
	go func() {
		done <- a.shutdown()
	}()
	select {
	case err := <-done:
		// The join timeout is returned as a fixed error, which Serve joins
		// with the main error to make the exit code nonzero.
		if err == nil || !errors.Is(err, errListenerJoinTimeout) {
			t.Fatalf("join timeout の shutdown が errListenerJoinTimeout を返しません: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("shutdown が join timeout 後も return しませんでした")
	}

	// Later phases do not run. The health server keeps answering and the
	// lock stays held.
	resp, err := http.Get("http://" + hs.Addr().String() + "/readyz")
	if err != nil {
		t.Fatalf("join timeout 後に health server が応答しませんでした: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("join timeout 後の /readyz が %d を返しました (want 503)", resp.StatusCode)
	}
	if a.lock.f == nil {
		t.Fatalf("join timeout 後に lock が解放されました")
	}

	// Cleanup: release the gate to end the listener-side goroutine, and
	// close the health server and lock the same way the normal shutdown path
	// does.
	close(release)
	wgDone := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(wgDone)
	}()
	select {
	case <-wgDone:
	case <-time.After(time.Second):
		t.Fatalf("gate 解放後も listener goroutine が終了しませんでした")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := hs.Shutdown(ctx); err != nil {
		t.Fatalf("health.Shutdown が error を返した: %v", err)
	}
	a.lock.release()
}

// TestStartup_ReadinessLifecycle fixes the startup readiness transition
// order with the store alone. session true is set before listener true;
// listener true lasts only from right before Run to right after; both are
// true only while the listener is actually running.
func TestStartup_ReadinessLifecycle(t *testing.T) {
	store := health.NewStore()
	if store.Ready() {
		t.Fatalf("起動直後の store が ready です")
	}
	store.SetSessionRunning(true) // after session creation
	if store.Ready() {
		t.Fatalf("listener 未稼働で ready になりました")
	}
	store.SetListenerRunning(true) // right before listener.Run
	if !store.Ready() {
		t.Fatalf("session と listener の両方稼働で ready になりません")
	}
	store.SetListenerRunning(false) // right after listener.Run
	if store.Ready() {
		t.Fatalf("listener 停止後も ready のままです")
	}
}

// TestShutdown_CompletesWithoutComponents verifies that a shutdown with no
// components completes immediately with real objects. This is the minimal
// form of the contract that shutdown is called from error paths during
// startup, and it returns no error.
func TestShutdown_CompletesWithoutComponents(t *testing.T) {
	a := newShutdownTestApp(&config.Config{
		Shutdown: config.ShutdownConfig{Grace: config.Duration(time.Second)},
		Runner:   config.RunnerConfig{ProvisioningTimeout: config.Duration(time.Second)},
	})
	done := make(chan error)
	go func() {
		done <- a.shutdown()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown が error を返しました: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("shutdown が完了しませんでした")
	}
}

// TestShutdown_WaitsForListenerWithoutResidualGrace verifies that shutdown
// waits until the listener goroutine finishes and completes without waiting
// out the remaining grace afterwards (phase 1), with real wg/channel.
func TestShutdown_WaitsForListenerWithoutResidualGrace(t *testing.T) {
	grace := time.Second
	a := newShutdownTestApp(&config.Config{
		Shutdown: config.ShutdownConfig{
			BusyPolicy: config.ShutdownPolicyLeave,
			Grace:      config.Duration(grace),
		},
		Runner: config.RunnerConfig{ProvisioningTimeout: config.Duration(2 * time.Second)},
	})
	release := make(chan struct{})
	started := addWaitGate(a, release)
	<-started

	start := time.Now()
	done := make(chan error)
	go func() {
		done <- a.shutdown()
	}()
	// Delay wg completion past the start of shutdown's join so the test
	// measures that shutdown waits for goroutine exit and then completes
	// without the remaining grace.
	time.Sleep(50 * time.Millisecond)
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown が error を返しました: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("shutdown が完了しませんでした")
	}
	elapsed := time.Since(start)
	if elapsed >= grace/2 {
		t.Fatalf("shutdown が残余 grace を待ちました: %v (grace %v)", elapsed, grace)
	}
	if elapsed < 40*time.Millisecond {
		t.Fatalf("shutdown が listener の終了を待ちませんでした: %v", elapsed)
	}
}

// TestShutdown_PhaseContextsAreFresh verifies that every shutdown phase
// creates its timeout context fresh from context.Background() and is not
// affected by an earlier phase's expiry/cancel. It reproduces the same
// creation order and timeouts as shutdown itself (join provisioning
// timeout, scaler grace, health 5s, session 30s).
func TestShutdown_PhaseContextsAreFresh(t *testing.T) {
	grace := time.Second
	provisioning := 2 * time.Second
	joinCtx, joinCancel := newShutdownPhaseContext(provisioning)
	scalerCtx, scalerCancel := newShutdownPhaseContext(grace)
	healthCtx, healthCancel := newShutdownPhaseContext(healthShutdownTimeout)
	sessionCtx, sessionCancel := newShutdownPhaseContext(sessionCloseTimeout)
	defer func() {
		joinCancel()
		scalerCancel()
		healthCancel()
		sessionCancel()
	}()

	// Each phase deadline matches shutdown's timeout.
	checkDeadline := func(name string, ctx context.Context, want time.Duration) {
		t.Helper()
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatalf("%s phase の context に deadline がありません", name)
		}
		if d := time.Until(deadline); d < want-100*time.Millisecond || d > want+100*time.Millisecond {
			t.Fatalf("%s phase の deadline が期待と異なります: %v (want %v)", name, d, want)
		}
	}
	checkDeadline("join", joinCtx, provisioning)
	checkDeadline("scaler", scalerCtx, grace)
	checkDeadline("health", healthCtx, healthShutdownTimeout)
	checkDeadline("session", sessionCtx, sessionCloseTimeout)

	// Cancelling the earlier phase (join) leaves later phase contexts alive
	// independently (no structural reuse of expired contexts).
	joinCancel()
	if joinCtx.Err() == nil {
		t.Fatalf("join phase の cancel が反映されていません")
	}
	for _, phase := range []struct {
		name string
		ctx  context.Context
	}{
		{name: "scaler", ctx: scalerCtx},
		{name: "health", ctx: healthCtx},
		{name: "session", ctx: sessionCtx},
	} {
		if err := phase.ctx.Err(); err != nil {
			t.Fatalf("%s phase の context が join の cancel の影響を受けました: %v", phase.name, err)
		}
		if _, ok := phase.ctx.Deadline(); !ok {
			t.Fatalf("%s phase の context が fresh ではありません", phase.name)
		}
	}
}
