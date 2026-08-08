package scaleset

import (
	"fmt"
	"strings"
	"testing"

	scalesetapi "github.com/actions/scaleset"

	"github.com/nukanoto/arc-docker/internal/model"
)

// validJitRunnerName returns a canonical runner name.
func validJitRunnerName(t *testing.T) string {
	t.Helper()
	return model.RunnerName("scale-set", "0123456789ab")
}

// TestJitConfig_StringRedactsSecret protects opaque JIT data in Stringer output.
func TestJitConfig_StringRedactsSecret(t *testing.T) {
	secret := "opaque-encoded-jit-secret-value"
	jit := JitConfig{RunnerID: 11, RunnerName: validJitRunnerName(t), ScaleSetID: 42, Encoded: secret}
	for _, verb := range []string{"%s", "%v"} {
		out := fmt.Sprintf(verb, jit)
		if strings.Contains(out, secret) {
			t.Fatalf("%s output exposes the encoded secret value: %s", verb, out)
		}
		if !strings.Contains(out, "<redacted>") {
			t.Fatalf("%s output has no redaction marker: %s", verb, out)
		}
	}
}

// TestValidateJitInput covers validation before official I/O.
func TestValidateJitInput(t *testing.T) {
	t.Run("Scale Set ID", func(t *testing.T) {
		name := validJitRunnerName(t)
		for _, id := range []int{0, -1, -100} {
			if err := validateJitInput(name, id); err == nil {
				t.Fatalf("scale set ID %d did not return an error", id)
			}
		}
		if err := validateJitInput(name, 42); err != nil {
			t.Fatalf("rejected positive scale set ID")
		}
	})
	t.Run("runner name", func(t *testing.T) {
		tests := []struct {
			name  string
			input string
			want  bool
		}{
			{name: "empty string", want: false},
			{name: "no suffix", input: "scale-set", want: false},
			{name: "11-digit suffix", input: "scale-set-0123456789a", want: false},
			{name: "13-digit suffix", input: "scale-set-0123456789abc", want: false},
			{name: "uppercase hex suffix", input: "scale-set-0123456789AB", want: false},
			{name: "non-hex suffix", input: "scale-set-0123456789zz", want: false},
			{name: "non-canonical uppercase prefix", input: "Scale-Set-0123456789ab", want: false},
			{name: "non-canonical leading separator", input: "-scale-set-0123456789ab", want: false},
			{name: "canonical runner name", input: validJitRunnerName(t), want: true},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				err := validateJitInput(tt.input, 42)
				if tt.want && err != nil {
					t.Fatalf("rejected canonical runner name")
				}
				if !tt.want && err == nil {
					t.Fatalf("invalid runner name %q did not return an error", tt.input)
				}
			})
		}
	})
}

// TestValidateJitConfig covers malformed and matching JIT responses.
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
		{name: "nil response"},
		{name: "nil Runner", raw: &scalesetapi.RunnerScaleSetJitRunnerConfig{}},
		{name: "non-positive runner ID", raw: &scalesetapi.RunnerScaleSetJitRunnerConfig{
			Runner:           &scalesetapi.RunnerReference{ID: 0, Name: name, RunnerScaleSetID: 42},
			EncodedJITConfig: "opaque-secret-value",
		}},
		{name: "runner name mismatch", raw: &scalesetapi.RunnerScaleSetJitRunnerConfig{
			Runner:           &scalesetapi.RunnerReference{ID: 11, Name: "other-name", RunnerScaleSetID: 42},
			EncodedJITConfig: "opaque-secret-value",
		}},
		{name: "scale set ID mismatch", raw: &scalesetapi.RunnerScaleSetJitRunnerConfig{
			Runner:           &scalesetapi.RunnerReference{ID: 11, Name: name, RunnerScaleSetID: 1},
			EncodedJITConfig: "opaque-secret-value",
		}},
		{name: "empty encoded JIT config", raw: &scalesetapi.RunnerScaleSetJitRunnerConfig{
			Runner: &scalesetapi.RunnerReference{ID: 11, Name: name, RunnerScaleSetID: 42},
		}},
		{name: "matching response", raw: valid, ok: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateJitConfig(tt.raw, name, 42)
			if tt.ok && err != nil {
				t.Fatalf("rejected matching response")
			}
			if !tt.ok && err == nil {
				t.Fatalf("invalid response did not return an error")
			}
		})
	}
}
