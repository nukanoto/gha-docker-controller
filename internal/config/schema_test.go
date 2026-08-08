package config

import (
	"strings"
	"testing"
)

func TestLoad_RemovedSettingsRejected(t *testing.T) {
	base := baseWithKey(t)
	tests := []struct {
		name string
		doc  string
	}{
		{"docker runtime", strings.Replace(base, "  pullPolicy: if-not-present\n", "  pullPolicy: if-not-present\n  runtime: runsc\n", 1)},
		{"docker network", strings.Replace(base, "  pullPolicy: if-not-present\n", "  pullPolicy: if-not-present\n  network: bridge\n", 1)},
		{"runner profile", strings.Replace(base, "  image: ghcr.io/actions/actions-runner:2.336.0\n", "  image: ghcr.io/actions/actions-runner:2.336.0\n  profile: standard\n", 1)},
		{"runner cpu", strings.Replace(base, "  image: ghcr.io/actions/actions-runner:2.336.0\n", "  image: ghcr.io/actions/actions-runner:2.336.0\n  cpu: 1\n", 1)},
		{"dind runner section", "dindRunner:\n  storage: tmpfs\n" + base},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Load(writeConfig(t, tt.doc))
			checkErr(t, tt.name, "not found", err)
		})
	}
}

func TestLoad_UnknownTopLevelAndRunnerFieldsRejected(t *testing.T) {
	base := baseWithKey(t)
	tests := []string{
		"plugin:\n  name: x\n" + base,
		strings.Replace(base, "runner:\n", "runner:\n  unknown: true\n", 1),
	}
	for _, doc := range tests {
		_, _, err := Load(writeConfig(t, doc))
		checkErr(t, "unknown field", "not found", err)
	}
}
