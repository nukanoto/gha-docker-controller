package docker

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/nukanoto/arc-docker/internal/model"
)

// These unit tests cover managed guards and pure lifecycle helpers without I/O.

// testIdentity returns a valid managed identity.
func testIdentity() model.RunnerIdentity {
	return model.RunnerIdentity{ScaleSetID: 10, RunnerID: 100, RunnerName: "ghadc-test-r100"}
}

// TestVerifyManagedLabels_ExactMatch covers the valid managed identity.
func TestVerifyManagedLabels_ExactMatch(t *testing.T) {
	identity := testIdentity()
	labels := model.BuildLabels(identity, "instance-1", time.Now())
	if err := verifyManagedLabels("c1", labels, identity); err != nil {
		t.Fatalf("正しい label の組み合わせが guard を通過しません: %v", err)
	}
}

// TestVerifyManagedLabels_Mismatch covers every required-label guard.
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
		// Controller instance is audited but not tied to the test identity.
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

// TestVerifyManagedLabels_InvalidIdentityAndNil covers invalid guard inputs.
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

// TestManagedContainer_LabelsDefensiveCopy protects observed label state.
func TestManagedContainer_LabelsDefensiveCopy(t *testing.T) {
	identity := testIdentity()
	source := model.BuildLabels(identity, "instance-1", time.Now())

	summary := managedFromSummary(container.Summary{
		ID:     "c1",
		Names:  []string{"/ghadc-name"},
		Labels: source,
		State:  container.StateRunning,
	})
	inspect := managedFromInspect(container.InspectResponse{
		ID:     "c2",
		Name:   "/ghadc-name2",
		Config: &container.Config{Labels: source},
		State:  &container.State{Status: container.StateExited, ExitCode: 3},
	})

	for _, mc := range []ManagedContainer{summary, inspect} {
		source[model.RunnerNameLabelKey] = "tampered"
		if got := mc.Labels()[model.RunnerNameLabelKey]; got != identity.RunnerName {
			t.Fatalf("入力元の書き換えが内部 label に反映されています: %q", got)
		}
		got := mc.Labels()
		got[model.ManagedLabelKey] = "false"
		if internal := mc.Labels()[model.ManagedLabelKey]; internal != model.ManagedLabelValue {
			t.Fatalf("取得側の書き換えが内部 label に反映されています: %q", internal)
		}
		first := mc.Labels()
		second := mc.Labels()
		first[model.ControllerInstanceLabelKey] = "changed"
		if second[model.ControllerInstanceLabelKey] != "instance-1" {
			t.Fatal("Labels() が同じ map を再利用しています")
		}
	}
}

// TestManagedContainer_LabelsNilSafety covers malformed daemon observations.
func TestManagedContainer_LabelsNilSafety(t *testing.T) {
	summary := managedFromSummary(container.Summary{ID: "c1", Labels: nil})
	if labels := summary.Labels(); labels != nil {
		t.Fatalf("nil label 入力が非 nil map を返しました: %v", labels)
	}
	inspect := managedFromInspect(container.InspectResponse{ID: "c2", Config: nil})
	if labels := inspect.Labels(); labels != nil {
		t.Fatalf("nil Config 入力が非 nil map を返しました: %v", labels)
	}
}

// TestManagedFromInspect_ExitCode covers terminal-state exit codes.
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

// TestNeedsStop covers stoppable container states.
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

// TestStopTimeoutSeconds protects the SDK's nonzero grace period.
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

// TestIdentityFromObserved_Valid covers identity restoration for cleanup.
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

// TestIdentityFromObserved_Malformed prevents malformed observations from authorizing cleanup.
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

// TestIdentityFromObserved_NilLabels covers a zero-value observation.
func TestIdentityFromObserved_NilLabels(t *testing.T) {
	_, err := identityFromObserved("c1", nil)
	var guardErr *ManagedGuardError
	if !errors.As(err, &guardErr) {
		t.Fatalf("nil label が ManagedGuardError を返しません: %v", err)
	}
}

// TestBoundedWriter protects bounded retention while draining the stream.
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
