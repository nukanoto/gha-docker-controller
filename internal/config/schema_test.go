package config

// Verifies YAML schema behavior: unknown fields, old-style keys and the
// normalization of the tmpfs map form.

import (
	"reflect"
	"strings"
	"testing"
)

// TestLoad_OldSchemaKeysRejected verifies that every old-style key is
// rejected as an unknown field or a type mismatch. Old keys must not be
// silently ignored; a strict YAML error is part of the contract.
func TestLoad_OldSchemaKeysRejected(t *testing.T) {
	base := baseWithKey(t)

	tests := []struct {
		name    string
		doc     string
		wantErr string
	}{
		{name: "old top-level scaleset", doc: strings.Replace(base, "scaleSet:\n", "scaleset:\n", 1), wantErr: "not found"},
		{name: "old github.app.appId", doc: strings.Replace(base, "    id: 1\n", "    appId: 1\n", 1), wantErr: "not found"},
		{name: "unused runner.idleTimeout", doc: base + "  idleTimeout: 15m\n", wantErr: "not found"},
		{name: "unused reconcile section", doc: base + "reconcile:\n  interval: 15s\n  orphanGracePeriod: 2m\n", wantErr: "not found"},
		{name: "old reconcile.orphanGrace", doc: base + "reconcile:\n  orphanGrace: 2m\n", wantErr: "not found"},
		{name: "old shutdown.busyPolicy", doc: base + "shutdown:\n  busyPolicy: leave\n", wantErr: "not found"},
		{name: "old shutdown.grace", doc: base + "shutdown:\n  grace: 2m\n", wantErr: "not found"},
		{name: "old runner.tmpfs sequence", doc: strings.Replace(base, "runner:\n", "runner:\n  tmpfs:\n    - /run\n", 1), wantErr: "cannot unmarshal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Load(writeConfig(t, tt.doc))
			checkErr(t, tt.name, tt.wantErr, err)
		})
	}
}

// TestLoad_UnknownAndForbiddenFieldsRejected verifies that unknown fields and
// forbidden mounts/devices/namespaces (privileged, devices, binds,
// volumesFrom, host PID, userns, networkMode) are rejected by KnownFields.
// github.token is forbidden because secrets must not be placed in the YAML
// body, so the body never appears in the error.
func TestLoad_UnknownAndForbiddenFieldsRejected(t *testing.T) {
	base := baseWithKey(t)

	tests := []struct {
		name   string
		extra  string
		secret string
	}{
		{name: "top level unknown section", extra: "plugin:\n  name: x\n"},
		{name: "forbidden yaml token", extra: "  token: pat-inline-secret-value\n", secret: "pat-inline-secret-value"},
		{name: "forbidden privileged", extra: "  privileged: true\n"},
		{name: "forbidden devices", extra: "  devices:\n    - /dev/sda:/dev/sda\n"},
		{name: "forbidden binds", extra: "  binds:\n    - /:/host\n"},
		{name: "forbidden volumesFrom", extra: "  volumesFrom:\n    - other\n"},
		{name: "forbidden host pid namespace", extra: "  pid: host\n"},
		{name: "forbidden userns mode", extra: "  usernsMode: host\n"},
		{name: "forbidden networkMode", extra: "  networkMode: host\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var doc string
			switch {
			case strings.HasPrefix(tt.extra, "plugin:"):
				// A top-level unknown section is inserted at the head of the
				// document.
				doc = tt.extra + base
			case strings.HasPrefix(tt.extra, "  token:"):
				// An unknown field of the github section is inserted right
				// after "github:".
				doc = strings.Replace(base, "github:\n", "github:\n"+tt.extra, 1)
			default:
				// Forbidden fields of the runner section are inserted right
				// after "runner:".
				doc = strings.Replace(base, "runner:\n", "runner:\n"+tt.extra, 1)
			}
			_, _, err := Load(writeConfig(t, doc))
			checkErr(t, tt.name, "not found", err)
			if tt.secret != "" && strings.Contains(err.Error(), tt.secret) {
				t.Fatalf("error に秘密の本文 %q が含まれています: %v", tt.secret, err)
			}
		})
	}
}

// TestLoad_TmpfsMapNormalization verifies that a map[path]options tmpfs is
// normalized into the internal form (path or path:options) in ascending path
// order. The result does not depend on the YAML order, and empty options
// become the bare path.
func TestLoad_TmpfsMapNormalization(t *testing.T) {
	base := baseWithKey(t)

	// The written order is /tmp, /run, /var/run, but the normalized order is
	// ascending by path.
	doc := strings.Replace(base, "  stopTimeout: 30s\n",
		"  stopTimeout: 30s\n  tmpfs:\n    /tmp: \"\"\n    /run: ro,size=64MiB\n    /var/run: \"\"\n", 1)
	c, _ := loadDoc(t, doc)
	want := []string{"/run:ro,size=64MiB", "/tmp", "/var/run"}
	if !reflect.DeepEqual(c.Runner.Tmpfs, want) {
		t.Fatalf("tmpfs の正規化が不正です: 期待値 %v、実測値 %v", want, c.Runner.Tmpfs)
	}

	// Surrounding whitespace of the value is removed by normalization.
	doc = strings.Replace(base, "  stopTimeout: 30s\n",
		"  stopTimeout: 30s\n  tmpfs:\n    /run: \" ro \"\n", 1)
	if c, _ := loadDoc(t, doc); !reflect.DeepEqual(c.Runner.Tmpfs, []string{"/run:ro"}) {
		t.Fatalf("空白 trim 後の正規化が不正です: %v", c.Runner.Tmpfs)
	}
}

// TestLoad_TmpfsMapInvalid verifies that invalid options, invalid
// destinations, duplicate YAML keys and the old sequence form are rejected
// for the map-form tmpfs. Each entry is validated by the Docker CLI
// compatible validateTmpfs.
func TestLoad_TmpfsMapInvalid(t *testing.T) {
	base := baseWithKey(t)

	tests := []struct {
		name    string
		entries string
		wantErr string
	}{
		{name: "unlimited size option", entries: "    /run: \"size=0\"\n", wantErr: "runner.tmpfs[0]: invalid tmpfs size option"},
		{name: "unlimited size inside comma options", entries: "    /run: \"ro,size=0\"\n", wantErr: "invalid tmpfs size option"},
		{name: "relative destination key", entries: "    run: \"ro\"\n", wantErr: "runner.tmpfs[0]: tmpfs destination must be an absolute path"},
		{name: "empty destination key", entries: "    \"\": \"ro\"\n", wantErr: "runner.tmpfs[0]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := strings.Replace(base, "  stopTimeout: 30s\n", "  stopTimeout: 30s\n  tmpfs:\n"+tt.entries, 1)
			_, _, err := Load(writeConfig(t, doc))
			checkErr(t, tt.name, tt.wantErr, err)
		})
	}

	// Duplicate keys for the same path are rejected by YAML duplicate
	// detection.
	doc := strings.Replace(base, "  stopTimeout: 30s\n",
		"  stopTimeout: 30s\n  tmpfs:\n    /run: \"ro\"\n    /run: \"rw\"\n", 1)
	_, _, err := Load(writeConfig(t, doc))
	checkErr(t, "duplicate tmpfs path", "already defined", err)
}
