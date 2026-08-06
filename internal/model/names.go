package model

import (
	"strconv"
	"strings"
	"unicode"
)

const (
	containerNamePrefix = "ghadc-"
	containerNameLimit  = 63
	// runnerNameSuffixLength is the number of lowercase hex digits at the end
	// of a JIT runner name.
	runnerNameSuffixLength = 12
)

// SanitizeName normalizes a component used in Docker names into a safe ASCII
// string. Characters outside the allowed set are replaced with a single
// hyphen, and leading and trailing separators are removed. The source of
// truth for identity is labels and the GitHub runner ID, not the name.
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

// SanitizeScaleSetName normalizes a Scale Set name for use in container
// names.
func SanitizeScaleSetName(scaleSetName string) string {
	name := SanitizeName(scaleSetName)
	if name == "" {
		return "scale-set"
	}
	return name
}

// RunnerIDBase36 converts a runner ID into a short base36 component for
// container names.
func RunnerIDBase36(runnerID int64) string {
	if runnerID < 0 {
		return "0"
	}
	return strconv.FormatInt(runnerID, 36)
}

// ContainerName builds the fixed container name format. The result is always
// 63 bytes or less; the shortened name never represents identity.
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

// RunnerName builds the JIT runner name format. suffix is treated as the
// first 12 hex characters of a UUID.
func RunnerName(scaleSetName, suffix string) string {
	return SanitizeScaleSetName(scaleSetName) + "-" + hexSuffix(suffix, runnerNameSuffixLength)
}

// ValidRunnerName checks whether a name has the canonical runner name form:
// a non-empty canonical sanitized prefix + '-' + 12 lowercase hex characters
// at the end. RunnerName output is always accepted; malformed, empty,
// non-canonical, and short-suffix names are rejected. It is used for input
// validation before official I/O such as JIT generation.
func ValidRunnerName(name string) bool {
	// The minimum length is 14 characters: 1 prefix character + 1 separator
	// + 12 suffix characters.
	if len(name) < runnerNameSuffixLength+2 {
		return false
	}
	// The last '-' is treated as the suffix separator. The prefix itself may
	// contain '-' internally.
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
	// Only canonical prefixes whose round-trip through SanitizeName holds are
	// accepted. This rejects names with disallowed characters, uppercase
	// letters, or leading and trailing separators.
	return SanitizeName(prefix) == prefix
}

// isLowerHex reports whether r is a lowercase hex character.
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
