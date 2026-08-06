package docker

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/nukanoto/gha-docker-controller/internal/model"
)

// This file holds only pure unit tests for managed/lifecycle. Real Docker
// I/O (fresh inspect, stop/wait, logs, ContainerRemove) goes to the
// integration tests against a real daemon with no mock/stub, so here the
// fresh label guard (verifyManagedLabels), unmanaged rejection, identity
// restoration from observed labels (identityFromObserved), label defensive
// copies, exit code observation, stop target decision (needsStop), stop
// timeout ceiling (stopTimeoutSeconds) and bounded log retention
// (boundedWriter) are verified without mocks.

// testIdentity is the positive identity used for the managed guard checks.
func testIdentity() model.RunnerIdentity {
	return model.RunnerIdentity{ScaleSetID: 10, RunnerID: 100, RunnerName: "ghadc-test-r100"}
}

// TestVerifyManagedLabels_ExactMatch verifies that the managed guard
// accepts the required six labels (managed=true, scale-set-id, runner-id,
// runner-name, controller-instance, created-at) with an exact match against
// the identity.
func TestVerifyManagedLabels_ExactMatch(t *testing.T) {
	identity := testIdentity()
	labels := model.BuildLabels(identity, "instance-1", time.Now())
	if err := verifyManagedLabels("c1", labels, identity); err != nil {
		t.Fatalf("正しい label の組み合わせが guard を通過しません: %v", err)
	}
}

// TestVerifyManagedLabels_Mismatch verifies that missing or mismatched
// required labels are rejected as ManagedGuardError. Besides
// managed/scale-set-id/runner-id, tampering with runner-name,
// controller-instance and created-at is also rejected (full validation via
// model.ValidateLabels). Destructive operations always abort on a fresh
// inspect label mismatch.
func TestVerifyManagedLabels_Mismatch(t *testing.T) {
	identity := testIdentity()
	cases := []struct {
		name   string
		mutate func(map[string]string)
	}{
		{name: "managed label の欠落", mutate: func(m map[string]string) { delete(m, model.ManagedLabelKey) }},
		{name: "managed label の改変", mutate: func(m map[string]string) { m[model.ManagedLabelKey] = "false" }},
		{name: "scale-set-id の不一致", mutate: func(m map[string]string) { m[model.ScaleSetIDLabelKey] = "11" }},
		{name: "runner-id の不一致", mutate: func(m map[string]string) { m[model.RunnerIDLabelKey] = "101" }},
		{name: "runner-name の不一致", mutate: func(m map[string]string) { m[model.RunnerNameLabelKey] = "tampered-runner" }},
		{name: "runner-name の欠落", mutate: func(m map[string]string) { delete(m, model.RunnerNameLabelKey) }},
		// controller-instance is for auditing; any non-empty value is allowed.
		{name: "controller-instance の欠落", mutate: func(m map[string]string) { delete(m, model.ControllerInstanceLabelKey) }},
		{name: "created-at の改変 (非 UTC)", mutate: func(m map[string]string) { m[model.CreatedAtLabelKey] = "2000-01-01T00:00:00+09:00" }},
		{name: "created-at の欠落", mutate: func(m map[string]string) { delete(m, model.CreatedAtLabelKey) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			labels := model.BuildLabels(identity, "instance-1", time.Now())
			tc.mutate(labels)
			err := verifyManagedLabels("c1", labels, identity)
			var guardErr *ManagedGuardError
			if !errors.As(err, &guardErr) {
				t.Fatalf("不一致 label が ManagedGuardError を返しません: %v", err)
			}
			if guardErr.ContainerID != "c1" {
				t.Fatalf("ContainerID が一致しません: %q", guardErr.ContainerID)
			}
		})
	}
}

// TestVerifyManagedLabels_InvalidIdentityAndNil verifies that the guard
// rejects a non-positive identity and nil labels. A non-positive identity is
// distinguished as an input error, not a guard error.
func TestVerifyManagedLabels_InvalidIdentityAndNil(t *testing.T) {
	identity := testIdentity()
	if err := verifyManagedLabels("c1", nil, identity); err == nil {
		t.Fatal("nil label が guard を通過してしまいました")
	}
	labels := model.BuildLabels(identity, "instance-1", time.Now())
	err := verifyManagedLabels("c1", labels, model.RunnerIdentity{})
	var guardErr *ManagedGuardError
	if errors.As(err, &guardErr) {
		t.Fatalf("非正 identity が ManagedGuardError として返りました: %v", err)
	}
	if err == nil {
		t.Fatal("非正 identity が guard を通過してしまいました")
	}
}

// TestManagedContainer_LabelsDefensiveCopy verifies that the internal label
// map of ManagedContainer and the Labels() return value are defensive
// copies. Neither mutating the input observation map nor the returned map
// affects the internal state.
func TestManagedContainer_LabelsDefensiveCopy(t *testing.T) {
	identity := testIdentity()
	source := model.BuildLabels(identity, "instance-1", time.Now())

	// Observation via Summary (ListManaged path).
	summary := managedFromSummary(container.Summary{
		ID:     "c1",
		Names:  []string{"/ghadc-name"},
		Labels: source,
		State:  container.StateRunning,
	})
	// Observation via Inspect (VerifyManaged path).
	inspect := managedFromInspect(container.InspectResponse{
		ID:     "c2",
		Name:   "/ghadc-name2",
		Config: &container.Config{Labels: source},
		State:  &container.State{Status: container.StateExited, ExitCode: 3},
	})

	for _, mc := range []ManagedContainer{summary, inspect} {
		// Mutating the source map later must not change the internal state.
		source[model.RunnerNameLabelKey] = "tampered"
		if got := mc.Labels()[model.RunnerNameLabelKey]; got != identity.RunnerName {
			t.Fatalf("入力元の書き換えが内部 label に反映されています: %q", got)
		}
		// Mutating the Labels() return value must not change the internal state.
		got := mc.Labels()
		got[model.ManagedLabelKey] = "false"
		if internal := mc.Labels()[model.ManagedLabelKey]; internal != model.ManagedLabelValue {
			t.Fatalf("取得側の書き換えが内部 label に反映されています: %q", internal)
		}
		// Labels() returns an independent map on every call.
		first := mc.Labels()
		second := mc.Labels()
		first[model.ControllerInstanceLabelKey] = "changed"
		if second[model.ControllerInstanceLabelKey] != "instance-1" {
			t.Fatal("Labels() が同じ map を再利用しています")
		}
	}
}

// TestManagedContainer_LabelsNilSafety verifies that observations with nil
// labels / nil Config return a nil map without panicking. maps.Clone keeps
// nil as nil; the Labels() return value is read-only, and nil is rejected by
// the "labels are missing" check in model.ValidateLabels, so it is safe.
func TestManagedContainer_LabelsNilSafety(t *testing.T) {
	// A Summary with nil Labels must not panic.
	summary := managedFromSummary(container.Summary{ID: "c1", Labels: nil})
	if labels := summary.Labels(); labels != nil {
		t.Fatalf("nil label 入力が非 nil map を返しました: %v", labels)
	}
	// An Inspect with nil Config (daemon contract violation) must not panic.
	inspect := managedFromInspect(container.InspectResponse{ID: "c2", Config: nil})
	if labels := inspect.Labels(); labels != nil {
		t.Fatalf("nil Config 入力が非 nil map を返しました: %v", labels)
	}
}

// TestManagedFromInspect_ExitCode verifies that only exited/dead containers
// keep an exit code. Cleanup step 4 puts this observation on the result.
func TestManagedFromInspect_ExitCode(t *testing.T) {
	exited := managedFromInspect(container.InspectResponse{
		State: &container.State{Status: container.StateExited, ExitCode: 7},
	})
	if code, ok := exited.ExitCode(); !ok || code != 7 {
		t.Fatalf("exited の終了 code が取得できません: code=%d ok=%v", code, ok)
	}
	dead := managedFromInspect(container.InspectResponse{
		State: &container.State{Status: container.StateDead, ExitCode: 137},
	})
	if code, ok := dead.ExitCode(); !ok || code != 137 {
		t.Fatalf("dead の終了 code が取得できません: code=%d ok=%v", code, ok)
	}
	running := managedFromInspect(container.InspectResponse{
		State: &container.State{Status: container.StateRunning},
	})
	if _, ok := running.ExitCode(); ok {
		t.Fatal("running の container が終了 code を持っています")
	}
}

// TestNeedsStop verifies the stop target decision (running/paused/restarting).
// created/exited/dead need no stop; the stop is applied only to targets.
func TestNeedsStop(t *testing.T) {
	cases := []struct {
		name  string
		state container.ContainerState
		want  bool
	}{
		{name: "running は停止対象", state: container.StateRunning, want: true},
		{name: "paused は停止対象", state: container.StatePaused, want: true},
		{name: "restarting は停止対象", state: container.StateRestarting, want: true},
		{name: "created は停止不要", state: container.StateCreated, want: false},
		{name: "exited は停止不要", state: container.StateExited, want: false},
		{name: "dead は停止不要", state: container.StateDead, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsStop(tc.state); got != tc.want {
				t.Fatalf("needsStop の結果が不正です: state=%q、実測値=%v、期待値=%v", tc.state, got, tc.want)
			}
		})
	}
}

// TestStopTimeoutSeconds verifies the ceiling conversion of the stop timeout
// to seconds. The SDK interprets a Timeout of 0 as "kill immediately with no
// grace", so a positive setting must never round down to 0 seconds.
func TestStopTimeoutSeconds(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want int
	}{
		{in: 30 * time.Second, want: 30},
		{in: time.Second, want: 1},
		{in: 1500 * time.Millisecond, want: 2},
		{in: 500 * time.Millisecond, want: 1},
	}
	for _, tc := range cases {
		if got := stopTimeoutSeconds(tc.in); got != tc.want {
			t.Fatalf("stopTimeoutSeconds の結果が不正です: 入力=%s、実測値=%d、期待値=%d", tc.in, got, tc.want)
		}
	}
}

// TestIdentityFromObserved_Valid verifies that the correct identity is
// restored from the labels observed at enumeration time. The destructive
// guard matches this identity against fresh-inspect labels, so a broken
// restoration would make managed containers impossible to destroy.
func TestIdentityFromObserved_Valid(t *testing.T) {
	identity := testIdentity()
	labels := model.BuildLabels(identity, "instance-1", time.Now())
	got, err := identityFromObserved("c1", labels)
	if err != nil {
		t.Fatalf("正しい観測 label からの復元が失敗しました: %v", err)
	}
	if got != identity {
		t.Fatalf("復元された identity が一致しません: got %+v, want %+v", got, identity)
	}
}

// TestIdentityFromObserved_Malformed verifies that malformed observed
// labels are rejected as ManagedGuardError. Besides missing/non-integer/
// non-positive scale-set-id and runner-id, missing or invalid values among
// the required six labels (runner-name, controller-instance, created-at)
// are also rejected. A container with malformed observed labels is rejected
// before any I/O, so CleanupManaged and Recover never change it.
func TestIdentityFromObserved_Malformed(t *testing.T) {
	identity := testIdentity()
	cases := []struct {
		name   string
		mutate func(map[string]string)
	}{
		{name: "scale-set-id の欠落", mutate: func(m map[string]string) { delete(m, model.ScaleSetIDLabelKey) }},
		{name: "scale-set-id の非整数", mutate: func(m map[string]string) { m[model.ScaleSetIDLabelKey] = "ten" }},
		{name: "scale-set-id が 0", mutate: func(m map[string]string) { m[model.ScaleSetIDLabelKey] = "0" }},
		{name: "runner-id の欠落", mutate: func(m map[string]string) { delete(m, model.RunnerIDLabelKey) }},
		{name: "runner-id の非整数", mutate: func(m map[string]string) { m[model.RunnerIDLabelKey] = "one" }},
		{name: "runner-id が 0", mutate: func(m map[string]string) { m[model.RunnerIDLabelKey] = "0" }},
		{name: "runner-id が負", mutate: func(m map[string]string) { m[model.RunnerIDLabelKey] = "-7" }},
		{name: "runner-name の欠落", mutate: func(m map[string]string) { delete(m, model.RunnerNameLabelKey) }},
		{name: "controller-instance の欠落", mutate: func(m map[string]string) { delete(m, model.ControllerInstanceLabelKey) }},
		{name: "created-at の欠落", mutate: func(m map[string]string) { delete(m, model.CreatedAtLabelKey) }},
		{name: "created-at の不正", mutate: func(m map[string]string) { m[model.CreatedAtLabelKey] = "not-a-timestamp" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			labels := model.BuildLabels(identity, "instance-1", time.Now())
			tc.mutate(labels)
			_, err := identityFromObserved("c1", labels)
			var guardErr *ManagedGuardError
			if !errors.As(err, &guardErr) {
				t.Fatalf("malformed な観測 label が ManagedGuardError を返しません: %v", err)
			}
			if guardErr.ContainerID != "c1" {
				t.Fatalf("ContainerID が一致しません: %q", guardErr.ContainerID)
			}
		})
	}
}

// TestIdentityFromObserved_NilLabels verifies that a nil label observation
// (for example, a zero-value ManagedContainer) is rejected as a
// ManagedGuardError.
func TestIdentityFromObserved_NilLabels(t *testing.T) {
	_, err := identityFromObserved("c1", nil)
	var guardErr *ManagedGuardError
	if !errors.As(err, &guardErr) {
		t.Fatalf("nil label が ManagedGuardError を返しません: %v", err)
	}
}

// TestBoundedWriter verifies that boundedWriter keeps only up to the limit
// bytes, discards the excess and always returns the full input length. This
// contract is what lets stdcopy keep consuming the stream to the end.
func TestBoundedWriter(t *testing.T) {
	cases := []struct {
		name string
		max  int
		in   string
		want string
	}{
		{name: "上限ちょうどは全保持", max: 5, in: "abcde", want: "abcde"},
		{name: "超過分は破棄", max: 3, in: "abcdef", want: "abc"},
		{name: "空入力", max: 3, in: "", want: ""},
		{name: "上限 0 は何も保持しない", max: 0, in: "abc", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := &boundedWriter{max: tc.max, buf: &buf}
			n, err := w.Write([]byte(tc.in))
			if err != nil {
				t.Fatalf("Write が error を返しました: %v", err)
			}
			if n != len(tc.in) {
				t.Fatalf("Write の戻り値が入力長と一致しません: got %d, want %d", n, len(tc.in))
			}
			if got := buf.String(); got != tc.want {
				t.Fatalf("保持内容が一致しません: got %q, want %q", got, tc.want)
			}
		})
	}
}
