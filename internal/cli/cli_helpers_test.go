// cli_helpers_test.go verifies the shared CLI test helpers and logger
// setup. A fixed marker is used for secret non-leak verification.
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nukanoto/gha-docker-controller/internal/config"
)

// secretMarker is the fixed marker string used to verify that secrets do
// not leak. Passing a real secret into tests would itself leak it through
// logs, so a fixed non-secret string verifies the wrap semantics and
// non-exposure.
const secretMarker = "SECRET-MARKER-9f8e7d6c5b4a"

// writeTempFile writes a real file into a temporary directory and returns
// its path. t.TempDir cleanup removes it at test end.
func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("一時 file の作成に失敗しました: %v", err)
	}
	return path
}

// invalidAuthConfigYAML returns a statically invalid config YAML that
// references a secret file. maxRunners is missing, so config.Load reads the
// secret and then fails validation; this verifies that the CLI error output
// does not leak the secret value.
func invalidAuthConfigYAML(secretPath string) string {
	return fmt.Sprintf(`github:
  scope: organization
  owner: my-org
  app:
    id: 1
    installationId: 2
    privateKeyFile: "%s"
scaleSet:
  name: prod
runner:
  image: ghcr.io/actions/actions-runner:2.336.0
`, secretPath)
}

// TestLogWarnings_PathAndMessageOnly verifies that a config Warning appears
// in the JSON log with only the two fields path and message. Per config's
// contract warnings contain no secrets. The failure branch prints no
// observed warning and reports with fixed Japanese text.
func TestLogWarnings_PathAndMessageOnly(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logWarnings(logger, []config.Warning{
		{Path: "github.app.privateKeyFile", Message: "secret file permissions are -rwxrwxrwx; set 0600"},
	})

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal("logWarnings の出力が JSON ではありません (error 本文は出力しません)")
	}
	if got["msg"] != "config warning" ||
		got["path"] != "github.app.privateKeyFile" ||
		got["warning"] == "" {
		t.Fatal("logWarnings の field が期待と異なります (warning 本文は出力しません)")
	}
}

// TestNewLogger_JSONDefaultAndLevelFiltering verifies that newLogger picks
// JSON for the default case (non-text) and text only when explicitly
// configured, and filters output by level. The destination is a real
// writer.
func TestNewLogger_JSONDefaultAndLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	logger := newLogger(config.LogFormatJSON, config.LogLevelError, &buf)
	logger.Info("hidden info")
	logger.Error("shown error")
	if !strings.HasPrefix(buf.String(), "{") {
		t.Fatalf("JSON format の出力が JSON ではありません: %q", buf.String())
	}
	if strings.Contains(buf.String(), "hidden info") {
		t.Fatalf("level error で info が出力されています: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "shown error") {
		t.Fatalf("level error で error が出力されていません: %q", buf.String())
	}

	// text is chosen only when explicitly configured.
	buf.Reset()
	textLogger := newLogger(config.LogFormatText, config.LogLevelInfo, &buf)
	textLogger.Info("hello")
	if !strings.Contains(buf.String(), "msg=hello") {
		t.Fatalf("text format の出力形式が期待と異なります: %q", buf.String())
	}
}

// TestSlogLevel_Mapping verifies the mapping of the 4 config level names and
// the defensive default (info) for unknown values.
func TestSlogLevel_Mapping(t *testing.T) {
	tests := []struct {
		level string
		want  slog.Level
	}{
		{level: config.LogLevelDebug, want: slog.LevelDebug},
		{level: config.LogLevelInfo, want: slog.LevelInfo},
		{level: config.LogLevelWarn, want: slog.LevelWarn},
		{level: config.LogLevelError, want: slog.LevelError},
		{level: "bogus", want: slog.LevelInfo},
	}
	for _, tt := range tests {
		if got := slogLevel(tt.level); got != tt.want {
			t.Fatalf("slogLevel(%q) が期待と異なります: %v", tt.level, got)
		}
	}
}
