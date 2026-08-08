package model

import (
	"testing"
	"time"
)

func testLabelIdentity() RunnerIdentity {
	return RunnerIdentity{
		ScaleSetID: 42,
		RunnerID:   1001,
		RunnerName: "scale-set-abcdef123456",
	}
}

// TestBuildLabels_SixLabelsRoundTrip covers the managed-label round trip.
func TestBuildLabels_SixLabelsRoundTrip(t *testing.T) {
	identity := testLabelIdentity()
	createdAt := time.Date(2026, time.March, 1, 2, 3, 4, 5678, time.FixedZone("JST", 9*60*60))
	labels := BuildLabels(identity, "controller-instance-1", createdAt)

	if got, want := len(labels), 6; got != want {
		t.Fatalf("label count is incorrect: want=%d got=%d labels=%+v", want, got, labels)
	}
	for _, key := range RequiredLabelKeys() {
		if _, ok := labels[key]; !ok {
			t.Fatalf("required label %q is missing: labels=%+v", key, labels)
		}
	}
	if err := ValidateLabels(labels, identity); err != nil {
		t.Fatalf("generated labels failed validation: %v", err)
	}
	if !LabelsMatchIdentity(labels, identity) {
		t.Fatal("generated labels do not match the identity")
	}
	if got, want := labels[CreatedAtLabelKey], "2026-02-28T17:03:04.000005678Z"; got != want {
		t.Fatalf("created-at has an invalid UTC RFC3339Nano representation: want=%q got=%q", want, got)
	}
}

// TestValidateLabels_InvalidEachRequiredLabel covers each required-label guard.
func TestValidateLabels_InvalidEachRequiredLabel(t *testing.T) {
	identity := testLabelIdentity()
	createdAt := time.Date(2026, time.March, 1, 2, 3, 4, 0, time.UTC)

	tests := []struct {
		name   string
		mutate func(map[string]string)
	}{
		{name: "managed label", mutate: func(labels map[string]string) { labels[ManagedLabelKey] = "false" }},
		{name: "scale set id label", mutate: func(labels map[string]string) { labels[ScaleSetIDLabelKey] = "43" }},
		{name: "runner id label", mutate: func(labels map[string]string) { labels[RunnerIDLabelKey] = "1002" }},
		{name: "runner name label", mutate: func(labels map[string]string) { labels[RunnerNameLabelKey] = "other-runner" }},
		{name: "controller instance label", mutate: func(labels map[string]string) { labels[ControllerInstanceLabelKey] = "" }},
		{name: "created at label", mutate: func(labels map[string]string) { labels[CreatedAtLabelKey] = "not-a-time" }},
		{name: "missing required label", mutate: func(labels map[string]string) { delete(labels, RunnerNameLabelKey) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels := BuildLabels(identity, "controller-instance-1", createdAt)
			tt.mutate(labels)
			if err := ValidateLabels(labels, identity); err == nil {
				t.Fatalf("invalid labels were accepted: %+v", labels)
			}
			if LabelsMatchIdentity(labels, identity) {
				t.Fatalf("invalid labels matched the identity: %+v", labels)
			}
		})
	}

	if err := ValidateLabels(nil, identity); err == nil {
		t.Fatal("nil label map was accepted")
	}
}

// TestValidateLabels_IdentityAndTimestampValidation covers identity and time guards.
func TestValidateLabels_IdentityAndTimestampValidation(t *testing.T) {
	createdAt := time.Date(2026, time.March, 1, 2, 3, 4, 0, time.UTC)
	tests := []struct {
		name     string
		identity RunnerIdentity
	}{
		{name: "zero scale set id", identity: RunnerIdentity{RunnerID: 1001, RunnerName: "scale-set-abcdef123456"}},
		{name: "zero runner id", identity: RunnerIdentity{ScaleSetID: 42, RunnerName: "scale-set-abcdef123456"}},
		{name: "empty runner name", identity: RunnerIdentity{ScaleSetID: 42, RunnerID: 1001}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels := BuildLabels(testLabelIdentity(), "controller-instance-1", createdAt)
			if err := ValidateLabels(labels, tt.identity); err == nil {
				t.Fatalf("labels were accepted for an invalid identity: %+v", tt.identity)
			}
		})
	}
}
