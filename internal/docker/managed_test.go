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
		t.Fatalf("valid label set was rejected by the guard: %v", err)
	}
}

// TestVerifyManagedLabels_Mismatch covers every required-label guard.
func TestVerifyManagedLabels_Mismatch(t *testing.T) {
	identity := testIdentity()
	cases := []struct {
		name   string
		mutate func(map[string]string)
	}{
		{name: "missing managed label", mutate: func(m map[string]string) { delete(m, model.ManagedLabelKey) }},
		{name: "tampered managed label", mutate: func(m map[string]string) { m[model.ManagedLabelKey] = "false" }},
		{name: "scale-set-id mismatch", mutate: func(m map[string]string) { m[model.ScaleSetIDLabelKey] = "11" }},
		{name: "runner-id mismatch", mutate: func(m map[string]string) { m[model.RunnerIDLabelKey] = "101" }},
		{name: "runner-name mismatch", mutate: func(m map[string]string) { m[model.RunnerNameLabelKey] = "tampered-runner" }},
		{name: "missing runner-name", mutate: func(m map[string]string) { delete(m, model.RunnerNameLabelKey) }},
		// Controller instance is audited but not tied to the test identity.
		{name: "missing controller-instance", mutate: func(m map[string]string) { delete(m, model.ControllerInstanceLabelKey) }},
		{name: "tampered created-at (non-UTC)", mutate: func(m map[string]string) { m[model.CreatedAtLabelKey] = "2000-01-01T00:00:00+09:00" }},
		{name: "missing created-at", mutate: func(m map[string]string) { delete(m, model.CreatedAtLabelKey) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			labels := model.BuildLabels(identity, "instance-1", time.Now())
			tc.mutate(labels)
			err := verifyManagedLabels("c1", labels, identity)
			var guardErr *ManagedGuardError
			if !errors.As(err, &guardErr) {
				t.Fatalf("mismatched labels did not return ManagedGuardError: %v", err)
			}
			if guardErr.ContainerID != "c1" {
				t.Fatalf("ContainerID differs: %q", guardErr.ContainerID)
			}
		})
	}
}

// TestVerifyManagedLabels_InvalidIdentityAndNil covers invalid guard inputs.
func TestVerifyManagedLabels_InvalidIdentityAndNil(t *testing.T) {
	identity := testIdentity()
	if err := verifyManagedLabels("c1", nil, identity); err == nil {
		t.Fatal("nil labels passed the guard")
	}
	labels := model.BuildLabels(identity, "instance-1", time.Now())
	err := verifyManagedLabels("c1", labels, model.RunnerIdentity{})
	var guardErr *ManagedGuardError
	if errors.As(err, &guardErr) {
		t.Fatalf("non-positive identity returned ManagedGuardError: %v", err)
	}
	if err == nil {
		t.Fatal("non-positive identity passed the guard")
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
			t.Fatalf("mutating the source labels changed internal labels: %q", got)
		}
		got := mc.Labels()
		got[model.ManagedLabelKey] = "false"
		if internal := mc.Labels()[model.ManagedLabelKey]; internal != model.ManagedLabelValue {
			t.Fatalf("mutating returned labels changed internal labels: %q", internal)
		}
		first := mc.Labels()
		second := mc.Labels()
		first[model.ControllerInstanceLabelKey] = "changed"
		if second[model.ControllerInstanceLabelKey] != "instance-1" {
			t.Fatal("Labels() reuses the same map")
		}
	}
}

// TestManagedContainer_LabelsNilSafety covers malformed daemon observations.
func TestManagedContainer_LabelsNilSafety(t *testing.T) {
	summary := managedFromSummary(container.Summary{ID: "c1", Labels: nil})
	if labels := summary.Labels(); labels != nil {
		t.Fatalf("nil labels returned a non-nil map: %v", labels)
	}
	inspect := managedFromInspect(container.InspectResponse{ID: "c2", Config: nil})
	if labels := inspect.Labels(); labels != nil {
		t.Fatalf("nil Config returned a non-nil map: %v", labels)
	}
}

// TestManagedFromInspect_ExitCode covers terminal-state exit codes.
func TestManagedFromInspect_ExitCode(t *testing.T) {
	exited := managedFromInspect(container.InspectResponse{
		State: &container.State{Status: container.StateExited, ExitCode: 7},
	})
	if code, ok := exited.ExitCode(); !ok || code != 7 {
		t.Fatalf("failed to get exited container exit code: code=%d ok=%v", code, ok)
	}
	dead := managedFromInspect(container.InspectResponse{
		State: &container.State{Status: container.StateDead, ExitCode: 137},
	})
	if code, ok := dead.ExitCode(); !ok || code != 137 {
		t.Fatalf("failed to get dead container exit code: code=%d ok=%v", code, ok)
	}
	running := managedFromInspect(container.InspectResponse{
		State: &container.State{Status: container.StateRunning},
	})
	if _, ok := running.ExitCode(); ok {
		t.Fatal("running container has an exit code")
	}
}

// TestNeedsStop covers stoppable container states.
func TestNeedsStop(t *testing.T) {
	cases := []struct {
		name  string
		state container.ContainerState
		want  bool
	}{
		{name: "running/paused/restarting are stoppable", state: container.StateRunning, want: true},
		{name: "paused is stoppable", state: container.StatePaused, want: true},
		{name: "restarting is stoppable", state: container.StateRestarting, want: true},
		{name: "created is not stoppable", state: container.StateCreated, want: false},
		{name: "exited is not stoppable", state: container.StateExited, want: false},
		{name: "dead is not stoppable", state: container.StateDead, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsStop(tc.state); got != tc.want {
				t.Fatalf("needsStop is invalid: state=%q got=%v want=%v", tc.state, got, tc.want)
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
			t.Fatalf("stopTimeoutSeconds is invalid: input=%s got=%d want=%d", tc.in, got, tc.want)
		}
	}
}

// TestIdentityFromObserved_Valid covers identity restoration for cleanup.
func TestIdentityFromObserved_Valid(t *testing.T) {
	identity := testIdentity()
	labels := model.BuildLabels(identity, "instance-1", time.Now())
	got, err := identityFromObserved("c1", labels)
	if err != nil {
		t.Fatalf("failed to restore identity from valid observed labels: %v", err)
	}
	if got != identity {
		t.Fatalf("restored identity differs: got=%+v want=%+v", got, identity)
	}
}

// TestIdentityFromObserved_Malformed prevents malformed observations from authorizing cleanup.
func TestIdentityFromObserved_Malformed(t *testing.T) {
	identity := testIdentity()
	cases := []struct {
		name   string
		mutate func(map[string]string)
	}{
		{name: "missing scale-set-id", mutate: func(m map[string]string) { delete(m, model.ScaleSetIDLabelKey) }},
		{name: "non-integer scale-set-id", mutate: func(m map[string]string) { m[model.ScaleSetIDLabelKey] = "ten" }},
		{name: "zero scale-set-id", mutate: func(m map[string]string) { m[model.ScaleSetIDLabelKey] = "0" }},
		{name: "missing runner-id", mutate: func(m map[string]string) { delete(m, model.RunnerIDLabelKey) }},
		{name: "non-integer runner-id", mutate: func(m map[string]string) { m[model.RunnerIDLabelKey] = "one" }},
		{name: "zero runner-id", mutate: func(m map[string]string) { m[model.RunnerIDLabelKey] = "0" }},
		{name: "negative runner-id", mutate: func(m map[string]string) { m[model.RunnerIDLabelKey] = "-7" }},
		{name: "missing runner-name", mutate: func(m map[string]string) { delete(m, model.RunnerNameLabelKey) }},
		{name: "missing controller-instance", mutate: func(m map[string]string) { delete(m, model.ControllerInstanceLabelKey) }},
		{name: "missing created-at", mutate: func(m map[string]string) { delete(m, model.CreatedAtLabelKey) }},
		{name: "invalid created-at", mutate: func(m map[string]string) { m[model.CreatedAtLabelKey] = "not-a-timestamp" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			labels := model.BuildLabels(identity, "instance-1", time.Now())
			tc.mutate(labels)
			_, err := identityFromObserved("c1", labels)
			var guardErr *ManagedGuardError
			if !errors.As(err, &guardErr) {
				t.Fatalf("malformed observed labels did not return ManagedGuardError: %v", err)
			}
			if guardErr.ContainerID != "c1" {
				t.Fatalf("ContainerID differs: %q", guardErr.ContainerID)
			}
		})
	}
}

// TestIdentityFromObserved_NilLabels covers a zero-value observation.
func TestIdentityFromObserved_NilLabels(t *testing.T) {
	_, err := identityFromObserved("c1", nil)
	var guardErr *ManagedGuardError
	if !errors.As(err, &guardErr) {
		t.Fatalf("nil labels did not return ManagedGuardError: %v", err)
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
		{name: "exact limit is retained", max: 5, in: "abcde", want: "abcde"},
		{name: "excess is discarded", max: 3, in: "abcdef", want: "abc"},
		{name: "empty input", max: 3, in: "", want: ""},
		{name: "zero limit retains nothing", max: 0, in: "abc", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := &boundedWriter{max: tc.max, buf: &buf}
			n, err := w.Write([]byte(tc.in))
			if err != nil {
				t.Fatalf("Write returned an error: %v", err)
			}
			if n != len(tc.in) {
				t.Fatalf("Write returned the wrong length: got %d want %d", n, len(tc.in))
			}
			if got := buf.String(); got != tc.want {
				t.Fatalf("retained content differs: got %q want %q", got, tc.want)
			}
		})
	}
}
