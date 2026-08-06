package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/docker/go-units"
	"gopkg.in/yaml.v3"
)

// Duration holds a YAML duration string (for example "5m", "30s"). Bare
// numbers without a unit are rejected as misconfiguration.
type Duration time.Duration

// UnmarshalYAML accepts only strings with a unit such as "5m".
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a string with a unit (e.g. 5m, 30s)")
	}
	if node.Tag == "!!int" || node.Tag == "!!float" {
		return fmt.Errorf("duration must be a string with a unit (e.g. 5m, 30s), not a bare number")
	}
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	if v <= 0 {
		return fmt.Errorf("duration must be positive: %q", s)
	}
	*d = Duration(v)
	return nil
}

// String returns the duration in time.Duration form.
func (d Duration) String() string {
	return time.Duration(d).String()
}

// Memory holds a YAML memory amount (for example "512MiB", "2GiB") in bytes.
type Memory int64

// UnmarshalYAML accepts a number or a string with a Docker CLI compatible
// unit.
func (m *Memory) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("memory must be a number or a string with a unit")
	}
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	v, err := parseMemory(s)
	if err != nil {
		return err
	}
	*m = Memory(v)
	return nil
}

// String returns the byte count as a decimal string.
func (m Memory) String() string {
	return strconv.FormatInt(int64(m), 10)
}

// parseMemory converts memory notation to bytes with the same rules as
// Docker's units.RAMInBytes. k/kb/kib, m/mb/mib, g/gb/gib and t/tb/tib are all
// powers of 1024. 0, negative values, -1, unlimited, unset and none are
// rejected as unlimited values.
func parseMemory(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("memory must not be empty")
	}
	switch s {
	case "0", "-1", "unlimited", "unset", "none":
		return 0, fmt.Errorf("unlimited memory value %q is not allowed", s)
	}
	// RAMInBytes truncates decimals without a unit, so bare bytes are limited
	// to integers.
	if !strings.ContainsAny(s, "abcdefghijklmnopqrstuvwxyz") && strings.Contains(s, ".") {
		return 0, fmt.Errorf("memory without a unit must be an integer number of bytes: %q", s)
	}
	v, err := units.RAMInBytes(s)
	if err != nil {
		return 0, fmt.Errorf("invalid memory value %q", s)
	}
	// Out-of-range float64->int64 conversion wraps to a negative value on
	// amd64 and saturates to MaxInt64 on arm64, so both are rejected as
	// overflow.
	if v < 0 || v == math.MaxInt64 {
		return 0, fmt.Errorf("memory value is too large: %q", s)
	}
	return v, nil
}

// NanoCPUs holds a YAML CPU count (for example "2", "2.5") in NanoCPUs.
type NanoCPUs int64

// UnmarshalYAML accepts only CPU counts greater than 0.
func (n *NanoCPUs) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("cpu must be a number or a decimal string")
	}
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	s = strings.TrimSpace(s)
	if s == "" || s == "unlimited" || s == "unset" {
		return fmt.Errorf("invalid cpu value %q", s)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("invalid cpu value %q", s)
	}
	if f <= 0 {
		return fmt.Errorf("cpu must be positive: %q", s)
	}
	if f > 1e6 {
		return fmt.Errorf("cpu value is too large: %q", s)
	}
	*n = NanoCPUs(math.Round(f * 1e9))
	return nil
}

// Ulimit holds a Docker ulimit specification (for example "nofile=1024:2048").
// Soft and Hard are positive integers; when Hard is unset it equals Soft.
type Ulimit struct {
	// Name is the ulimit resource name (nofile, nproc and so on).
	Name string
	// Soft is the soft limit.
	Soft int64
	// Hard is the hard limit.
	Hard int64
}

// UnmarshalYAML accepts only strings in "name=soft[:hard]" form. Format,
// resource name and hard>=soft validation are delegated to units.ParseUlimit.
func (u *Ulimit) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("ulimit must be a string in name=soft[:hard] form")
	}
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	parsed, err := units.ParseUlimit(s)
	// ParseUlimit accepts -1 (unlimited) and 0, so the project policy of
	// positive values only is applied here.
	if err != nil || parsed.Soft <= 0 || parsed.Hard <= 0 {
		return fmt.Errorf("invalid ulimit %q", s)
	}
	*u = Ulimit{Name: parsed.Name, Soft: parsed.Soft, Hard: parsed.Hard}
	return nil
}
