package config

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a YAML duration that requires a unit.
type Duration time.Duration

// UnmarshalYAML parses a unit-bearing duration.
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

// String returns the duration text.
func (d Duration) String() string {
	return time.Duration(d).String()
}
