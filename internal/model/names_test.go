package model

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSanitizeName_AllowedCharactersAndExplicitInputs covers Docker name normalization.
func TestSanitizeName_AllowedCharactersAndExplicitInputs(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "lowercase", input: "Runner-01", want: "runner-01"},
		{name: "allowed punctuation", input: "Scale_Set.v2", want: "scale_set.v2"},
		{name: "invalid run becomes one separator", input: "a / 日本語 / b", want: "a-b"},
		{name: "leading and trailing separators are removed", input: "--._Name_.--", want: "name"},
		{name: "only invalid characters becomes empty", input: "日本語 / !?", want: ""},
		{name: "empty input", input: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeName(tt.input); got != tt.want {
				t.Fatalf("sanitize result is incorrect: input=%q want=%q got=%q", tt.input, tt.want, got)
			}
		})
	}
}

// TestSanitizeName_ExplicitRandomInputRemainsSafe covers deterministic sanitization.
func TestSanitizeName_ExplicitRandomInputRemainsSafe(t *testing.T) {
	const alphabet = "aZ09_-./ :日本語!"
	random := rand.New(rand.NewSource(20260301))

	for caseIndex := 0; caseIndex < 128; caseIndex++ {
		var input strings.Builder
		for characterIndex := 0; characterIndex < 32; characterIndex++ {
			input.WriteByte(alphabet[random.Intn(len(alphabet))])
		}
		value := input.String()
		got := SanitizeName(value)
		for _, character := range got {
			allowed := (character >= 'a' && character <= 'z') ||
				(character >= '0' && character <= '9') ||
				character == '_' || character == '-' || character == '.'
			if !allowed {
				t.Fatalf("random input %d produced disallowed character %q: input=%q output=%q", caseIndex, character, value, got)
			}
		}
		if got != SanitizeName(value) {
			t.Fatalf("sanitize is not deterministic: input=%q output=%q", value, got)
		}
	}
}

// TestContainerName_AtMost63BytesAfterSanitization covers the Docker name limit.
func TestContainerName_AtMost63BytesAfterSanitization(t *testing.T) {
	name := ContainerName(strings.Repeat("日本語/VeryLong.Scale_Set-", 20), 123456789, "ABCDEF12-extra")
	if len(name) > containerNameLimit {
		t.Fatalf("container name exceeds 63 bytes: bytes=%d name=%q", len(name), name)
	}
	if !utf8.ValidString(name) {
		t.Fatalf("container name is not valid UTF-8: %q", name)
	}
	if !strings.HasPrefix(name, containerNamePrefix) {
		t.Fatalf("container name has an invalid prefix: %q", name)
	}
	if !strings.HasSuffix(name, "-abcdef12") {
		t.Fatalf("suffix is not normalized: %q", name)
	}

	for _, character := range name {
		allowed := (character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.'
		if !allowed {
			t.Fatalf("container name contains disallowed character %q: %q", character, name)
		}
	}
}

// TestRunnerName_SanitizedScaleSetAndExplicitHexSuffix covers canonical names.
func TestRunnerName_SanitizedScaleSetAndExplicitHexSuffix(t *testing.T) {
	if got, want := RunnerName("Scale Set/Prod", "ABC-def-123456789"), "scale-set-prod-abcdef123456"; got != want {
		t.Fatalf("runner name is invalid: want=%q got=%q", want, got)
	}
	if got, want := RunnerName("日本語", ""), "scale-set-000000000000"; got != want {
		t.Fatalf("runner name with an empty suffix is invalid: want=%q got=%q", want, got)
	}
}

// TestValidRunnerName_CanonicalFormAcceptance covers generated names.
func TestValidRunnerName_CanonicalFormAcceptance(t *testing.T) {
	tests := []struct {
		name     string
		scaleSet string
		suffix   string
	}{
		{name: "simple", scaleSet: "Scale Set/Prod", suffix: "ABC-def-123456789"},
		{name: "empty scale set falls back", scaleSet: "日本語", suffix: ""},
		{name: "long scale set", scaleSet: strings.Repeat("Scale_Set.v2-", 10), suffix: "0123456789abcdef"},
		{name: "separators inside prefix", scaleSet: "a_b.c-d", suffix: "ABCDEF-12345678"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if name := RunnerName(tt.scaleSet, tt.suffix); !ValidRunnerName(name) {
				t.Fatalf("RunnerName output was rejected: %q", name)
			}
		})
	}
}

// TestValidRunnerName_ExplicitRandomOutputsAlwaysAccepted covers fixed-seed output.
func TestValidRunnerName_ExplicitRandomOutputsAlwaysAccepted(t *testing.T) {
	const alphabet = "aZ09_-./ :日本語!"
	random := rand.New(rand.NewSource(20260302))

	for caseIndex := 0; caseIndex < 64; caseIndex++ {
		var scaleSet strings.Builder
		for i := 0; i < 24; i++ {
			scaleSet.WriteByte(alphabet[random.Intn(len(alphabet))])
		}
		var suffix strings.Builder
		for i := 0; i < 20; i++ {
			suffix.WriteByte(alphabet[random.Intn(len(alphabet))])
		}
		name := RunnerName(scaleSet.String(), suffix.String())
		if !ValidRunnerName(name) {
			t.Fatalf("RunnerName output generated from random input was rejected: scaleSet=%q suffix=%q output=%q", scaleSet.String(), suffix.String(), name)
		}
	}
}

// TestValidRunnerName_MalformedRejectionTable covers canonical-form rejection.
func TestValidRunnerName_MalformedRejectionTable(t *testing.T) {
	valid := RunnerName("Scale Set/Prod", "ABC-def-123456789")
	tests := []struct {
		name  string
		input string
	}{
		{name: "empty", input: ""},
		{name: "separator only", input: "-"},
		{name: "missing separator", input: "scale-set-prodabcdef123456"},
		{name: "empty prefix", input: "-abcdef123456"},
		{name: "short suffix", input: "scale-set-prod-abcdef12345"},
		{name: "long suffix", input: "scale-set-prod-abcdef1234567"},
		{name: "uppercase hex suffix", input: "scale-set-prod-ABCDEF123456"},
		{name: "non hex suffix", input: "scale-set-prod-abcdef12345g"},
		{name: "leading separator in prefix", input: "-scale-set-prod-abcdef123456"},
		{name: "trailing separator in prefix", input: "scale-set-prod.-abcdef123456"},
		{name: "uppercase in prefix", input: "Scale-set-prod-abcdef123456"},
		{name: "invalid character in prefix", input: "scale set-prod-abcdef123456"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if ValidRunnerName(tt.input) {
				t.Fatalf("malformed runner name was accepted: %q", tt.input)
			}
		})
	}
	if !ValidRunnerName(valid) {
		t.Fatalf("canonical runner name was rejected: %q", valid)
	}
}
