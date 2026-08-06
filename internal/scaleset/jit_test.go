package scaleset

import (
	"fmt"
	"strings"
	"testing"

	scalesetapi "github.com/actions/scaleset"

	"github.com/nukanoto/gha-docker-controller/internal/model"
)

// validJitRunnerName is a test helper that returns a canonical runner name.
// It uses no official I/O or mocks; the input is the pure output of
// model.RunnerName.
func validJitRunnerName(t *testing.T) string {
	t.Helper()
	return model.RunnerName("scale-set", "0123456789ab")
}

// TestJitConfig_StringRedactsSecret verifies that JitConfig never prints the
// real encoded secret value through fmt.Stringer output (%s and %v) and only
// prints the fixed redaction marker. The encoded value must never appear in
// logs, errors, or stringers.
func TestJitConfig_StringRedactsSecret(t *testing.T) {
	secret := "opaque-encoded-jit-secret-value"
	jit := JitConfig{RunnerID: 11, RunnerName: validJitRunnerName(t), ScaleSetID: 42, Encoded: secret}
	for _, verb := range []string{"%s", "%v"} {
		out := fmt.Sprintf(verb, jit)
		if strings.Contains(out, secret) {
			t.Fatalf("%s 出力に encoded の実 secret 値が露出しています: %s", verb, out)
		}
		if !strings.Contains(out, "<redacted>") {
			t.Fatalf("%s 出力に redact marker がありません: %s", verb, out)
		}
	}
}

// TestValidateJitInput verifies the requested values before JIT generation
// I/O. A non-positive Scale Set ID and a non-canonical runner name (empty, no
// suffix, wrong length/hex/uppercase, non-canonical prefix) become errors
// without official I/O; only a positive ID and model.RunnerName output are
// accepted.
func TestValidateJitInput(t *testing.T) {
	t.Run("Scale Set ID", func(t *testing.T) {
		name := validJitRunnerName(t)
		for _, id := range []int{0, -1, -100} {
			if err := validateJitInput(name, id); err == nil {
				t.Fatalf("scale set ID %d が error になりません", id)
			}
		}
		if err := validateJitInput(name, 42); err != nil {
			t.Fatalf("正の scale set ID を拒否しました")
		}
	})
	t.Run("runner name", func(t *testing.T) {
		tests := []struct {
			name  string
			input string
			want  bool
		}{
			{name: "空文字", want: false},
			{name: "suffix なし", input: "scale-set", want: false},
			{name: "suffix が 11 桁", input: "scale-set-0123456789a", want: false},
			{name: "suffix が 13 桁", input: "scale-set-0123456789abc", want: false},
			{name: "suffix が大文字 hex", input: "scale-set-0123456789AB", want: false},
			{name: "suffix が hex 以外", input: "scale-set-0123456789zz", want: false},
			{name: "prefix が非 canonical (大文字)", input: "Scale-Set-0123456789ab", want: false},
			{name: "prefix が非 canonical (先頭区切り)", input: "-scale-set-0123456789ab", want: false},
			{name: "canonical な runner name", input: validJitRunnerName(t), want: true},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				err := validateJitInput(tt.input, 42)
				if tt.want && err != nil {
					t.Fatalf("canonical な runner name を拒否しました")
				}
				if !tt.want && err == nil {
					t.Fatalf("不正な runner name %q が error になりません", tt.input)
				}
			})
		}
	})
}

// TestValidateJitConfig verifies the exact validation of a JIT response. A
// nil response, a nil Runner, a non-positive runner ID, a name mismatch, a
// Scale Set ID mismatch, and an empty encoded value become errors without
// panicking; only the matching response succeeds.
func TestValidateJitConfig(t *testing.T) {
	name := validJitRunnerName(t)
	valid := &scalesetapi.RunnerScaleSetJitRunnerConfig{
		Runner: &scalesetapi.RunnerReference{
			ID:               11,
			Name:             name,
			RunnerScaleSetID: 42,
		},
		EncodedJITConfig: "opaque-secret-value",
	}
	tests := []struct {
		name string
		raw  *scalesetapi.RunnerScaleSetJitRunnerConfig
		ok   bool
	}{
		{name: "nil 応答"},
		{name: "nil Runner", raw: &scalesetapi.RunnerScaleSetJitRunnerConfig{}},
		{name: "非正の runner ID", raw: &scalesetapi.RunnerScaleSetJitRunnerConfig{
			Runner:           &scalesetapi.RunnerReference{ID: 0, Name: name, RunnerScaleSetID: 42},
			EncodedJITConfig: "opaque-secret-value",
		}},
		{name: "runner name の不一致", raw: &scalesetapi.RunnerScaleSetJitRunnerConfig{
			Runner:           &scalesetapi.RunnerReference{ID: 11, Name: "other-name", RunnerScaleSetID: 42},
			EncodedJITConfig: "opaque-secret-value",
		}},
		{name: "scale set ID の不一致", raw: &scalesetapi.RunnerScaleSetJitRunnerConfig{
			Runner:           &scalesetapi.RunnerReference{ID: 11, Name: name, RunnerScaleSetID: 1},
			EncodedJITConfig: "opaque-secret-value",
		}},
		{name: "空の encoded JIT config", raw: &scalesetapi.RunnerScaleSetJitRunnerConfig{
			Runner: &scalesetapi.RunnerReference{ID: 11, Name: name, RunnerScaleSetID: 42},
		}},
		{name: "一致する応答", raw: valid, ok: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateJitConfig(tt.raw, name, 42)
			if tt.ok && err != nil {
				t.Fatalf("一致する応答を拒否しました")
			}
			if !tt.ok && err == nil {
				t.Fatalf("不正な応答が error になりません")
			}
		})
	}
}
