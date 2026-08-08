// CLI helper tests use real files, buffers, and loggers.
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

	"github.com/nukanoto/arc-docker/internal/config"
)

// A fixed non-secret marker avoids putting real credentials in test output.
const secretMarker = "SECRET-MARKER-9f8e7d6c5b4a"

// writeTempFile creates a real temporary file.
func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("failed to create temporary file: %v", err)
	}
	return path
}

// invalidAuthConfigYAML makes Load read a secret before static validation fails.
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

// TestLogWarnings_PathAndMessageOnly covers the non-secret warning fields.
func TestLogWarnings_PathAndMessageOnly(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logWarnings(logger, []config.Warning{
		{Path: "github.app.privateKeyFile", Message: "secret file permissions are -rwxrwxrwx; set 0600"},
	})

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal("logWarnings output is not JSON; error details are intentionally omitted")
	}
	if got["msg"] != "config warning" ||
		got["path"] != "github.app.privateKeyFile" ||
		got["warning"] == "" {
		t.Fatal("logWarnings fields differ from expectation; warning details are intentionally omitted")
	}
}

// TestNewLogger_JSONDefaultAndLevelFiltering covers format and level filtering.
func TestNewLogger_JSONDefaultAndLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	logger := newLogger(config.LogFormatJSON, config.LogLevelError, &buf)
	logger.Info("hidden info")
	logger.Error("shown error")
	if !strings.HasPrefix(buf.String(), "{") {
		t.Fatalf("JSON format did not produce JSON: %q", buf.String())
	}
	if strings.Contains(buf.String(), "hidden info") {
		t.Fatalf("info was emitted at error level: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "shown error") {
		t.Fatalf("error was not emitted at error level: %q", buf.String())
	}

	buf.Reset()
	textLogger := newLogger(config.LogFormatText, config.LogLevelInfo, &buf)
	textLogger.Info("hello")
	if !strings.Contains(buf.String(), "msg=hello") {
		t.Fatalf("text format differs from expectation: %q", buf.String())
	}
}

// TestSlogLevel_Mapping covers configured and defensive levels.
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
			t.Fatalf("slogLevel(%q) differs from expectation: %v", tt.level, got)
		}
	}
}
