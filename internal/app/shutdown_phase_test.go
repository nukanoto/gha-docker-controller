// These tests cover shutdown phase isolation and listener joining.
package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nukanoto/arc-docker/internal/config"
	"github.com/nukanoto/arc-docker/internal/health"
)

// TestShutdown_WaitListenerTimesOutAtDeadline covers the bounded join path.
func TestShutdown_WaitListenerTimesOutAtDeadline(t *testing.T) {
	a := newShutdownTestApp(&config.Config{})
	release := make(chan struct{})
	addWaitGate(a, release)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	if a.waitListener(ctx) {
		t.Fatalf("waitListener returned true before the deadline")
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("waitListener returned before the deadline: %v", elapsed)
	}
	// The helper goroutine must still be joined after the timeout.
	close(release)
	wgDone := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(wgDone)
	}()
	select {
	case <-wgDone:
	case <-time.After(time.Second):
		t.Fatalf("waitListener helper goroutine did not finish")
	}
}

// TestShutdown_WaitListenerReturnsTrueWhenDrained covers a completed join.
func TestShutdown_WaitListenerReturnsTrueWhenDrained(t *testing.T) {
	a := newShutdownTestApp(&config.Config{})
	release := make(chan struct{})
	addWaitGate(a, release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	start := time.Now()
	close(release)
	if !a.waitListener(ctx) {
		t.Fatalf("waitListener returned false after the WaitGroup drained")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("waitListener took too long to complete: %v", elapsed)
	}
}

// TestShutdown_ListenerJoinTimeout covers the maximum handler lifetime bound.
func TestShutdown_ListenerJoinTimeout(t *testing.T) {
	cases := []struct {
		name      string
		provision config.Duration
		grace     config.Duration
		want      time.Duration
	}{
		{name: "longer provisioning timeout wins", provision: config.Duration(90 * time.Second), grace: config.Duration(30 * time.Second), want: 90 * time.Second},
		{name: "longer cleanup timeout wins", provision: config.Duration(30 * time.Second), grace: config.Duration(90 * time.Second), want: 90 * time.Second},
		{name: "equal timeouts keep their value", provision: config.Duration(60 * time.Second), grace: config.Duration(60 * time.Second), want: 60 * time.Second},
		{name: "non-positive provisioning uses the five-minute default", provision: 0, grace: config.Duration(10 * time.Second), want: time.Duration(config.DefaultProvisioningTimeout)},
		{name: "non-positive grace uses the two-minute default", provision: config.Duration(10 * time.Second), grace: 0, want: time.Duration(config.DefaultShutdownGrace)},
		{name: "both non-positive values use the larger default", provision: -1, grace: -1, want: time.Duration(config.DefaultProvisioningTimeout)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newShutdownTestApp(&config.Config{
				Runner:   config.RunnerConfig{ProvisioningTimeout: tc.provision},
				Shutdown: config.ShutdownConfig{Grace: tc.grace},
			})
			if got := a.listenerJoinTimeout(); got != tc.want {
				t.Fatalf("listenerJoinTimeout differs from expectation: got %v want %v", got, tc.want)
			}
		})
	}
}

// TestShutdown_JoinTimeoutWarningIsFixed keeps warnings free of dynamic data.
func TestShutdown_JoinTimeoutWarningIsFixed(t *testing.T) {
	for _, want := range []string{listenerJoinTimeoutWarning, scalerJoinTimeoutWarning} {
		if want == "" {
			t.Fatalf("join timeout warning is empty")
		}
		if strings.ContainsAny(want, "{}") {
			t.Fatalf("join timeout warning contains dynamic data: %q", want)
		}
	}
}

// TestShutdown_ReadinessDropsImmediately checks readiness before health stops.
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
		t.Fatalf("health.New returned an error: %v", err)
	}
	if err := hs.Start(); err != nil {
		t.Fatalf("health.Start returned an error: %v", err)
	}
	a.health = hs
	store.SetSessionRunning(true)
	store.SetListenerRunning(true)

	// Hold phase 1 so readiness can be observed while health still serves.
	release := make(chan struct{})
	addWaitGate(a, release)

	done := make(chan error)
	go func() {
		done <- a.shutdown()
	}()

	deadline := time.Now().Add(3 * time.Second)
	for store.Ready() {
		if time.Now().After(deadline) {
			t.Fatalf("readiness did not become false after shutdown cancellation")
		}
		time.Sleep(5 * time.Millisecond)
	}
	resp, err := http.Get("http://" + hs.Addr().String() + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz returned an error: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/readyz returned %d during shutdown (want 503)", resp.StatusCode)
	}

	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown returned an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("shutdown did not complete")
	}
}

// TestShutdown_ListenerJoinTimeoutSkipsRemainingPhases protects live handlers
// from later component closure.
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
		t.Fatalf("health.New returned an error: %v", err)
	}
	if err := hs.Start(); err != nil {
		t.Fatalf("health.Start returned an error: %v", err)
	}
	a.health = hs
	// Keep a handler-like goroutine alive past the join deadline.
	release := make(chan struct{})
	addWaitGate(a, release)

	done := make(chan error)
	go func() {
		done <- a.shutdown()
	}()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, errListenerJoinTimeout) {
			t.Fatalf("shutdown did not return errListenerJoinTimeout after the join timeout: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("shutdown did not return after the join timeout")
	}

	resp, err := http.Get("http://" + hs.Addr().String() + "/readyz")
	if err != nil {
		t.Fatalf("health server did not respond after the join timeout: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/readyz returned %d after the join timeout (want 503)", resp.StatusCode)
	}
	close(release)
	wgDone := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(wgDone)
	}()
	select {
	case <-wgDone:
	case <-time.After(time.Second):
		t.Fatalf("listener goroutine did not finish after releasing the gate")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := hs.Shutdown(ctx); err != nil {
		t.Fatalf("health.Shutdown returned an error: %v", err)
	}
}

// TestStartup_ReadinessLifecycle covers startup readiness ordering.
func TestStartup_ReadinessLifecycle(t *testing.T) {
	store := health.NewStore()
	if store.Ready() {
		t.Fatalf("store is ready immediately after startup")
	}
	store.SetSessionRunning(true)
	if store.Ready() {
		t.Fatalf("store became ready while the listener was stopped")
	}
	store.SetListenerRunning(true)
	if !store.Ready() {
		t.Fatalf("store did not become ready while session and listener were running")
	}
	store.SetListenerRunning(false)
	if store.Ready() {
		t.Fatalf("store remained ready after the listener stopped")
	}
}

// TestShutdown_CompletesWithoutComponents covers partial startup cleanup.
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
			t.Fatalf("shutdown returned an error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("shutdown did not complete")
	}
}

// TestShutdown_WaitsForListenerWithoutResidualGrace covers prompt post-join cleanup.
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
	// Ensure shutdown has entered its join before releasing the goroutine.
	time.Sleep(50 * time.Millisecond)
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown returned an error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("shutdown did not complete")
	}
	elapsed := time.Since(start)
	if elapsed >= grace/2 {
		t.Fatalf("shutdown waited through the remaining grace period: %v (grace %v)", elapsed, grace)
	}
	if elapsed < 40*time.Millisecond {
		t.Fatalf("shutdown did not wait for the listener to finish: %v", elapsed)
	}
}

// TestShutdown_PhaseContextsAreFresh prevents one phase from cancelling later cleanup.
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

	checkDeadline := func(name string, ctx context.Context, want time.Duration) {
		t.Helper()
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatalf("%s phase context has no deadline", name)
		}
		if d := time.Until(deadline); d < want-100*time.Millisecond || d > want+100*time.Millisecond {
			t.Fatalf("%s phase deadline differs from expectation: %v (want %v)", name, d, want)
		}
	}
	checkDeadline("join", joinCtx, provisioning)
	checkDeadline("scaler", scalerCtx, grace)
	checkDeadline("health", healthCtx, healthShutdownTimeout)
	checkDeadline("session", sessionCtx, sessionCloseTimeout)

	// Cancelling one phase must not cancel later phases.
	joinCancel()
	if joinCtx.Err() == nil {
		t.Fatalf("join phase cancellation was not applied")
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
			t.Fatalf("%s phase context was affected by join cancellation: %v", phase.name, err)
		}
		if _, ok := phase.ctx.Deadline(); !ok {
			t.Fatalf("%s phase context is not independent", phase.name)
		}
	}
}
