package scaleset

import (
	"testing"

	scalesetapi "github.com/actions/scaleset"
)

// TestValidateRunnerGroup covers malformed and matching API responses.
func TestValidateRunnerGroup(t *testing.T) {
	tests := []struct {
		name  string
		group *scalesetapi.RunnerGroup
		want  string
		ok    bool
	}{
		{name: "nil response", group: nil, want: "default"},
		{name: "non-positive ID", group: &scalesetapi.RunnerGroup{ID: 0, Name: "default"}, want: "default"},
		{name: "name mismatch", group: &scalesetapi.RunnerGroup{ID: 1, Name: "other"}, want: "default"},
		{name: "empty requested name", group: &scalesetapi.RunnerGroup{ID: 1, Name: "default"}, want: ""},
		{name: "matching response", group: &scalesetapi.RunnerGroup{ID: 5, Name: "self-hosted"}, want: "self-hosted", ok: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRunnerGroup(tt.group, tt.want)
			if tt.ok && err != nil {
				t.Fatalf("rejected matching group: %v", err)
			}
			if !tt.ok && err == nil {
				t.Fatalf("invalid response did not return an error: %v", err)
			}
		})
	}
}

// TestValidateScaleSet covers the exact Scale Set contract.
func TestValidateScaleSet(t *testing.T) {
	valid := &scalesetapi.RunnerScaleSet{
		ID:            42,
		Name:          "scale-set",
		RunnerGroupID: 5,
		Labels:        []scalesetapi.Label{{Type: "system", Name: "scale-set"}},
		RunnerSetting: scalesetapi.RunnerSetting{DisableUpdate: true},
	}
	tests := []struct {
		name string
		ss   *scalesetapi.RunnerScaleSet
		ok   bool
	}{
		{name: "nil response", ss: nil},
		{name: "non-positive ID", ss: &scalesetapi.RunnerScaleSet{
			ID: 0, Name: "scale-set", RunnerGroupID: 5,
			Labels:        []scalesetapi.Label{{Type: "system", Name: "scale-set"}},
			RunnerSetting: scalesetapi.RunnerSetting{DisableUpdate: true},
		}},
		{name: "name mismatch", ss: &scalesetapi.RunnerScaleSet{
			ID: 42, Name: "other", RunnerGroupID: 5,
			Labels:        []scalesetapi.Label{{Type: "system", Name: "scale-set"}},
			RunnerSetting: scalesetapi.RunnerSetting{DisableUpdate: true},
		}},
		{name: "group ID mismatch", ss: &scalesetapi.RunnerScaleSet{
			ID: 42, Name: "scale-set", RunnerGroupID: 9,
			Labels:        []scalesetapi.Label{{Type: "system", Name: "scale-set"}},
			RunnerSetting: scalesetapi.RunnerSetting{DisableUpdate: true},
		}},
		{name: "no labels", ss: &scalesetapi.RunnerScaleSet{
			ID: 42, Name: "scale-set", RunnerGroupID: 5,
			RunnerSetting: scalesetapi.RunnerSetting{DisableUpdate: true},
		}},
		{name: "two labels", ss: &scalesetapi.RunnerScaleSet{
			ID: 42, Name: "scale-set", RunnerGroupID: 5,
			Labels: []scalesetapi.Label{
				{Type: "system", Name: "scale-set"},
				{Type: "system", Name: "extra"},
			},
			RunnerSetting: scalesetapi.RunnerSetting{DisableUpdate: true},
		}},
		{name: "label type is not System", ss: &scalesetapi.RunnerScaleSet{
			ID: 42, Name: "scale-set", RunnerGroupID: 5,
			Labels:        []scalesetapi.Label{{Type: "Custom", Name: "scale-set"}},
			RunnerSetting: scalesetapi.RunnerSetting{DisableUpdate: true},
		}},
		{name: "label type is uppercase System", ss: &scalesetapi.RunnerScaleSet{
			ID: 42, Name: "scale-set", RunnerGroupID: 5,
			Labels:        []scalesetapi.Label{{Type: "System", Name: "scale-set"}},
			RunnerSetting: scalesetapi.RunnerSetting{DisableUpdate: true},
		}},
		{name: "DisableUpdate=false", ss: &scalesetapi.RunnerScaleSet{
			ID: 42, Name: "scale-set", RunnerGroupID: 5,
			Labels:        []scalesetapi.Label{{Type: "system", Name: "scale-set"}},
			RunnerSetting: scalesetapi.RunnerSetting{DisableUpdate: false},
		}},
		{name: "matching response", ss: valid, ok: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateScaleSet(tt.ss, 5, "scale-set")
			if tt.ok && err != nil {
				t.Fatalf("rejected matching response: %v", err)
			}
			if !tt.ok && err == nil {
				t.Fatalf("invalid response did not return an error: %v", err)
			}
		})
	}
}

// TestCheckScaleSetResult covers missing, matching, and mismatched sets.
func TestCheckScaleSetResult(t *testing.T) {
	group := &scalesetapi.RunnerGroup{ID: 5, Name: "default"}
	valid := &scalesetapi.RunnerScaleSet{
		ID:            42,
		Name:          "scale-set",
		RunnerGroupID: 5,
		Labels:        []scalesetapi.Label{{Type: "system", Name: "scale-set"}},
		RunnerSetting: scalesetapi.RunnerSetting{DisableUpdate: true},
	}
	t.Run("missing Scale Set returns a warning without failing", func(t *testing.T) {
		result, err := checkScaleSetResult(group, nil, "scale-set")
		if err != nil {
			t.Fatalf("missing Scale Set should not fail: %v", err)
		}
		if result.Group != group {
			t.Fatalf("group was not preserved in the result")
		}
		if result.ScaleSet != nil {
			t.Fatalf("ScaleSet is non-nil when missing")
		}
		if result.Warning == "" {
			t.Fatalf("warning is not set when missing")
		}
	})
	t.Run("matching existing Scale Set is accepted without a warning", func(t *testing.T) {
		result, err := checkScaleSetResult(group, valid, "scale-set")
		if err != nil {
			t.Fatalf("rejected matching existing Scale Set: %v", err)
		}
		if result.ScaleSet != valid {
			t.Fatalf("ScaleSet was not preserved in the result")
		}
		if result.Warning != "" {
			t.Fatalf("warning was set for a matching Scale Set: %q", result.Warning)
		}
	})
	t.Run("mismatch returns an error", func(t *testing.T) {
		mismatched := &scalesetapi.RunnerScaleSet{
			ID: 42, Name: "other", RunnerGroupID: 5,
			Labels:        []scalesetapi.Label{{Type: "system", Name: "scale-set"}},
			RunnerSetting: scalesetapi.RunnerSetting{DisableUpdate: true},
		}
		if _, err := checkScaleSetResult(group, mismatched, "scale-set"); err == nil {
			t.Fatalf("mismatch did not return an error: %v", err)
		}
	})
}
