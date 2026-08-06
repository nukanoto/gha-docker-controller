package scaleset

import (
	"testing"

	scalesetapi "github.com/actions/scaleset"
)

// TestValidateRunnerGroup verifies the GetRunnerGroupByName response
// validation. A nil response, a non-positive ID, a name mismatch, and an
// empty requested name become errors without panicking; only a group with a
// matching ID and name is accepted. Validating before dereferencing prevents
// nil panics.
func TestValidateRunnerGroup(t *testing.T) {
	tests := []struct {
		name  string
		group *scalesetapi.RunnerGroup
		want  string
		ok    bool
	}{
		{name: "nil 応答", group: nil, want: "default"},
		{name: "非正の ID", group: &scalesetapi.RunnerGroup{ID: 0, Name: "default"}, want: "default"},
		{name: "name の不一致", group: &scalesetapi.RunnerGroup{ID: 1, Name: "other"}, want: "default"},
		{name: "空の要求名", group: &scalesetapi.RunnerGroup{ID: 1, Name: "default"}, want: ""},
		{name: "一致する応答", group: &scalesetapi.RunnerGroup{ID: 5, Name: "self-hosted"}, want: "self-hosted", ok: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRunnerGroup(tt.group, tt.want)
			if tt.ok && err != nil {
				t.Fatalf("一致する group を拒否しました: %v", err)
			}
			if !tt.ok && err == nil {
				t.Fatalf("不正な応答が error になりません: %v", err)
			}
		})
	}
}

// TestValidateScaleSet verifies the match requirements (ID, name, group ID,
// a single System label, DisableUpdate=true). Every mismatch including a nil
// response is an error without auto-update. Both the create-success response
// and the post-exists reget flow through this validator, so nil is rejected
// in one place.
func TestValidateScaleSet(t *testing.T) {
	valid := &scalesetapi.RunnerScaleSet{
		ID:            42,
		Name:          "scale-set",
		RunnerGroupID: 5,
		Labels:        []scalesetapi.Label{{Type: "System", Name: "scale-set"}},
		RunnerSetting: scalesetapi.RunnerSetting{DisableUpdate: true},
	}
	tests := []struct {
		name string
		ss   *scalesetapi.RunnerScaleSet
		ok   bool
	}{
		{name: "nil 応答", ss: nil},
		{name: "非正の ID", ss: &scalesetapi.RunnerScaleSet{
			ID: 0, Name: "scale-set", RunnerGroupID: 5,
			Labels:        []scalesetapi.Label{{Type: "System", Name: "scale-set"}},
			RunnerSetting: scalesetapi.RunnerSetting{DisableUpdate: true},
		}},
		{name: "name の不一致", ss: &scalesetapi.RunnerScaleSet{
			ID: 42, Name: "other", RunnerGroupID: 5,
			Labels:        []scalesetapi.Label{{Type: "System", Name: "scale-set"}},
			RunnerSetting: scalesetapi.RunnerSetting{DisableUpdate: true},
		}},
		{name: "group ID の不一致", ss: &scalesetapi.RunnerScaleSet{
			ID: 42, Name: "scale-set", RunnerGroupID: 9,
			Labels:        []scalesetapi.Label{{Type: "System", Name: "scale-set"}},
			RunnerSetting: scalesetapi.RunnerSetting{DisableUpdate: true},
		}},
		{name: "label なし", ss: &scalesetapi.RunnerScaleSet{
			ID: 42, Name: "scale-set", RunnerGroupID: 5,
			RunnerSetting: scalesetapi.RunnerSetting{DisableUpdate: true},
		}},
		{name: "label が 2 個", ss: &scalesetapi.RunnerScaleSet{
			ID: 42, Name: "scale-set", RunnerGroupID: 5,
			Labels: []scalesetapi.Label{
				{Type: "System", Name: "scale-set"},
				{Type: "System", Name: "extra"},
			},
			RunnerSetting: scalesetapi.RunnerSetting{DisableUpdate: true},
		}},
		{name: "label type が System 以外", ss: &scalesetapi.RunnerScaleSet{
			ID: 42, Name: "scale-set", RunnerGroupID: 5,
			Labels:        []scalesetapi.Label{{Type: "Custom", Name: "scale-set"}},
			RunnerSetting: scalesetapi.RunnerSetting{DisableUpdate: true},
		}},
		{name: "DisableUpdate=false", ss: &scalesetapi.RunnerScaleSet{
			ID: 42, Name: "scale-set", RunnerGroupID: 5,
			Labels:        []scalesetapi.Label{{Type: "System", Name: "scale-set"}},
			RunnerSetting: scalesetapi.RunnerSetting{DisableUpdate: false},
		}},
		{name: "一致する応答", ss: valid, ok: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateScaleSet(tt.ss, 5, "scale-set")
			if tt.ok && err != nil {
				t.Fatalf("一致する応答を拒否しました: %v", err)
			}
			if !tt.ok && err == nil {
				t.Fatalf("不正な応答が error になりません: %v", err)
			}
		})
	}
}

// TestCheckScaleSetResult verifies the pure result assembly of CheckScaleSet.
// A nil ss returns a warning that creation permission cannot be proven
// read-only, without failing. An existing Scale Set is verified against the
// exact contract by validateScaleSet: a mismatch is an error, a match is
// accepted without a warning. check uses the same validators as serve's
// EnsureScaleSet, so check and serve always agree.
func TestCheckScaleSetResult(t *testing.T) {
	group := &scalesetapi.RunnerGroup{ID: 5, Name: "default"}
	valid := &scalesetapi.RunnerScaleSet{
		ID:            42,
		Name:          "scale-set",
		RunnerGroupID: 5,
		Labels:        []scalesetapi.Label{{Type: "System", Name: "scale-set"}},
		RunnerSetting: scalesetapi.RunnerSetting{DisableUpdate: true},
	}
	t.Run("不存在は warning で失敗しない", func(t *testing.T) {
		result, err := checkScaleSetResult(group, nil, "scale-set")
		if err != nil {
			t.Fatalf("不存在は失敗にしないはずです: %v", err)
		}
		if result.Group != group {
			t.Fatalf("group が結果に引き継がれていません")
		}
		if result.ScaleSet != nil {
			t.Fatalf("不存在時に ScaleSet が nil ではありません")
		}
		if result.Warning == "" {
			t.Fatalf("不存在時に warning が設定されていません")
		}
	})
	t.Run("一致する既存 Scale Set は warning なしで受理", func(t *testing.T) {
		result, err := checkScaleSetResult(group, valid, "scale-set")
		if err != nil {
			t.Fatalf("一致する既存 Scale Set を拒否しました: %v", err)
		}
		if result.ScaleSet != valid {
			t.Fatalf("ScaleSet が結果に引き継がれていません")
		}
		if result.Warning != "" {
			t.Fatalf("一致時に warning が設定されました: %q", result.Warning)
		}
	})
	t.Run("不一致は error", func(t *testing.T) {
		mismatched := &scalesetapi.RunnerScaleSet{
			ID: 42, Name: "other", RunnerGroupID: 5,
			Labels:        []scalesetapi.Label{{Type: "System", Name: "scale-set"}},
			RunnerSetting: scalesetapi.RunnerSetting{DisableUpdate: true},
		}
		if _, err := checkScaleSetResult(group, mismatched, "scale-set"); err == nil {
			t.Fatalf("不一致が error になりません: %v", err)
		}
	})
}
