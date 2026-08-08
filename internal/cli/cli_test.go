// CLI tests use real buffers and files to cover parsing, output, and secrets.
package cli

import (
	"bytes"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nukanoto/arc-docker/internal/buildinfo"
)

// runCLI returns Run's exit code and captured streams.
func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code = Run(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

// assertStream checks expected output fragments.
func assertStream(t *testing.T, name, stream string, want []string) {
	t.Helper()
	if want == nil {
		if stream != "" {
			t.Fatalf("%s should be empty, but contains output: %q", name, stream)
		}
		return
	}
	for _, w := range want {
		if !strings.Contains(stream, w) {
			t.Fatalf("%s does not contain %q:\n%s", name, w, stream)
		}
	}
}

// TestRun covers command parsing and output routing.
func TestRun(t *testing.T) {
	// Restore package-shared build variables after each subtest.
	injectBuildVars := func(t *testing.T, version, commit, date string) {
		oldVersion, oldCommit, oldDate := buildinfo.Version, buildinfo.Commit, buildinfo.Date
		buildinfo.Version, buildinfo.Commit, buildinfo.Date = version, commit, date
		t.Cleanup(func() {
			buildinfo.Version, buildinfo.Commit, buildinfo.Date = oldVersion, oldCommit, oldDate
		})
	}

	missingConfig := filepath.Join(t.TempDir(), "no-such-config.yaml")
	tests := []struct {
		name     string
		args     []string
		setup    func(*testing.T)
		wantCode int
		wantOut  []string
		wantErr  []string
	}{
		{
			name:     "version prints ldflags values to stdout",
			args:     []string{"version"},
			setup:    func(t *testing.T) { injectBuildVars(t, "v9.9.9-test", "deadbeefcafe", "2026-01-02T03:04:05Z") },
			wantCode: ExitOK,
			wantOut: []string{
				"arc-docker v9.9.9-test",
				"commit: deadbeefcafe",
				"build date: 2026-01-02T03:04:05Z",
				"go version: " + runtime.Version(),
			},
		},
		{
			name:     "version prints default values to stdout",
			args:     []string{"version"},
			wantCode: ExitOK,
			wantOut: []string{
				"arc-docker dev",
				"commit: unknown",
				"build date: unknown",
				"go version: " + runtime.Version(),
			},
		},
		{
			name:     "command-level -h prints usage to stdout",
			args:     []string{"-h"},
			wantCode: ExitOK,
			wantOut:  []string{"Usage: arc-docker"},
		},
		{
			name:     "command-level --help prints usage to stdout",
			args:     []string{"--help"},
			wantCode: ExitOK,
			wantOut:  []string{"Usage: arc-docker"},
		},
		{
			name:     "serve -h prints command usage to stderr",
			args:     []string{"serve", "-h"},
			wantCode: ExitOK,
			wantErr:  []string{"Usage: arc-docker serve"},
		},
		{
			name:     "check -h prints command usage to stderr",
			args:     []string{"check", "-h"},
			wantCode: ExitOK,
			wantErr:  []string{"Usage: arc-docker check"},
		},
		{
			name:     "version -h prints command usage to stderr",
			args:     []string{"version", "-h"},
			wantCode: ExitOK,
			wantErr:  []string{"Usage: arc-docker version"},
		},
		{
			name:     "missing command prints usage and returns ExitUsage",
			wantCode: ExitUsage,
			wantErr:  []string{"Usage: arc-docker"},
		},
		{
			name:     "unknown command prints error and usage and returns ExitUsage",
			args:     []string{"frobnicate"},
			wantCode: ExitUsage,
			wantErr:  []string{`unknown command "frobnicate"`, "Usage: arc-docker"},
		},
		{
			name:     "serve rejects an extra argument",
			args:     []string{"serve", "extra"},
			wantCode: ExitUsage,
			wantErr:  []string{`serve: unexpected argument "extra"`},
		},
		{
			name:     "check rejects an extra argument",
			args:     []string{"check", "extra"},
			wantCode: ExitUsage,
			wantErr:  []string{`check: unexpected argument "extra"`},
		},
		{
			name:     "version rejects an extra argument",
			args:     []string{"version", "extra"},
			wantCode: ExitUsage,
			wantErr:  []string{`version: unexpected argument "extra"`},
		},
		{
			name:     "serve rejects an unknown flag",
			args:     []string{"serve", "--frobnicate"},
			wantCode: ExitUsage,
			wantErr:  []string{"flag provided but not defined"},
		},
		{
			name:     "check rejects an unknown flag",
			args:     []string{"check", "--frobnicate"},
			wantCode: ExitUsage,
			wantErr:  []string{"flag provided but not defined"},
		},
		{
			name:     "version rejects an unknown flag",
			args:     []string{"version", "--frobnicate"},
			wantCode: ExitUsage,
			wantErr:  []string{"flag provided but not defined"},
		},
		{
			name:     "serve returns ExitError for a missing config",
			args:     []string{"serve", "--config", missingConfig},
			wantCode: ExitError,
			wantErr:  []string{"load config"},
		},
		{
			name:     "check returns ExitError for a missing config",
			args:     []string{"check", "--config", missingConfig},
			wantCode: ExitError,
			wantErr:  []string{"load config"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup(t)
			}
			code, stdout, stderr := runCLI(t, tt.args...)
			if code != tt.wantCode {
				t.Fatalf("exit code differs from expectation: got %d, want %d", code, tt.wantCode)
			}
			assertStream(t, "stdout", stdout, tt.wantOut)
			assertStream(t, "stderr", stderr, tt.wantErr)
		})
	}
}

// TestRun_InvalidConfigDoesNotLeakSecret keeps loaded secret data out of CLI output.
func TestRun_InvalidConfigDoesNotLeakSecret(t *testing.T) {
	// Keep this test on the public GitHub validation path.
	t.Setenv("GITHUB_ACTIONS_FORCE_GHES", "")
	secretPath := writeTempFile(t, "private-key.pem",
		"-----BEGIN RSA PRIVATE KEY-----\n"+secretMarker+"\n-----END RSA PRIVATE KEY-----")
	cfgPath := writeTempFile(t, "config.yaml", invalidAuthConfigYAML(secretPath))

	for _, cmd := range []string{"serve", "check"} {
		t.Run(cmd, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, cmd, "--config", cfgPath)
			if code != ExitError {
				t.Fatalf("%s invalid config returned %d instead of ExitError", cmd, code)
			}
			if !strings.Contains(stderr, "invalid config") {
				t.Fatalf("%s validation error is missing from stderr", cmd)
			}
			if stdout != "" {
				t.Fatalf("%s should not write to stdout", cmd)
			}
			if strings.Contains(stderr, secretMarker) || strings.Contains(stdout, secretMarker) {
				t.Fatalf("%s output contains the secret marker", cmd)
			}
		})
	}
}
