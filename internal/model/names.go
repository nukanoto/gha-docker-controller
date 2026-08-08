package model

import (
	"strconv"
	"strings"
	"unicode"
)

const (
	containerNamePrefix = "ghadc-"
	containerNameLimit  = 63
	// Runner names end with a fixed lowercase-hex suffix.
	runnerNameSuffixLength = 12
)

// SanitizeName normalizes a component for use in a Docker name.
// Labels and the GitHub runner ID remain the identity source of truth.
func SanitizeName(value string) string {
	var b strings.Builder
	separator := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			if separator && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(unicode.ToLower(r))
			separator = false
			continue
		}
		separator = true
	}
	return strings.Trim(b.String(), "-_.")
}

// SanitizeScaleSetName normalizes a Scale Set name for container names.
func SanitizeScaleSetName(scaleSetName string) string {
	name := SanitizeName(scaleSetName)
	if name == "" {
		return "scale-set"
	}
	return name
}

// RunnerIDBase36 converts a runner ID to a base36 name component.
func RunnerIDBase36(runnerID int64) string {
	if runnerID < 0 {
		return "0"
	}
	return strconv.FormatInt(runnerID, 36)
}

// ContainerName builds a Docker name within the 63-byte limit.
func ContainerName(scaleSetName string, runnerID int64, suffix string) string {
	suffix = hexSuffix(suffix, 8)
	fixed := containerNamePrefix + "r" + RunnerIDBase36(runnerID) + "-" + suffix
	scale := SanitizeScaleSetName(scaleSetName)
	available := containerNameLimit - len(fixed) - 1
	if available < 1 {
		available = 1
	}
	if len(scale) > available {
		scale = scale[:available]
		scale = strings.Trim(scale, "-_.")
		if scale == "" {
			scale = "s"
		}
	}
	return containerNamePrefix + scale + "-r" + RunnerIDBase36(runnerID) + "-" + suffix
}

// RunnerName builds the canonical JIT runner name.
func RunnerName(scaleSetName, suffix string) string {
	return SanitizeScaleSetName(scaleSetName) + "-" + hexSuffix(suffix, runnerNameSuffixLength)
}

// ValidRunnerName checks the canonical runner-name form.
func ValidRunnerName(name string) bool {
	if len(name) < runnerNameSuffixLength+2 {
		return false
	}
	separator := strings.LastIndexByte(name, '-')
	if separator < 1 {
		return false
	}
	prefix := name[:separator]
	suffix := name[separator+1:]
	if len(suffix) != runnerNameSuffixLength {
		return false
	}
	for _, r := range suffix {
		if !isLowerHex(r) {
			return false
		}
	}
	return SanitizeName(prefix) == prefix
}

func isLowerHex(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
}

func hexSuffix(value string, length int) string {
	var b strings.Builder
	for _, r := range strings.ToLower(value) {
		if isLowerHex(r) {
			b.WriteRune(r)
			if b.Len() == length {
				break
			}
		}
	}
	for b.Len() < length {
		b.WriteByte('0')
	}
	return b.String()
}
