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
			t.Fatalf("%s は空であるべきですが出力があります: %q", name, stream)
		}
		return
	}
	for _, w := range want {
		if !strings.Contains(stream, w) {
			t.Fatalf("%s に %q がありません:\n%s", name, w, stream)
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
			name:     "version は ldflags 注入値を固定 key で stdout へ出す",
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
			name:     "version は ldflags 未設定の既定値を stdout へ出す",
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
			name:     "command 位置の -h は usage を stdout へ出して成功する",
			args:     []string{"-h"},
			wantCode: ExitOK,
			wantOut:  []string{"Usage: arc-docker"},
		},
		{
			name:     "command 位置の --help は usage を stdout へ出して成功する",
			args:     []string{"--help"},
			wantCode: ExitOK,
			wantOut:  []string{"Usage: arc-docker"},
		},
		{
			name:     "serve の -h は専用 usage を stderr へ出して成功する",
			args:     []string{"serve", "-h"},
			wantCode: ExitOK,
			wantErr:  []string{"Usage: arc-docker serve"},
		},
		{
			name:     "check の -h は専用 usage を stderr へ出して成功する",
			args:     []string{"check", "-h"},
			wantCode: ExitOK,
			wantErr:  []string{"Usage: arc-docker check"},
		},
		{
			name:     "version の -h は専用 usage を stderr へ出して成功する",
			args:     []string{"version", "-h"},
			wantCode: ExitOK,
			wantErr:  []string{"Usage: arc-docker version"},
		},
		{
			name:     "command 欠落は usage を stderr へ出して ExitUsage で終了する",
			wantCode: ExitUsage,
			wantErr:  []string{"Usage: arc-docker"},
		},
		{
			name:     "未知 command は error と usage を stderr へ出して ExitUsage で終了する",
			args:     []string{"frobnicate"},
			wantCode: ExitUsage,
			wantErr:  []string{`unknown command "frobnicate"`, "Usage: arc-docker"},
		},
		{
			name:     "serve の余分な引数は ExitUsage で終了する",
			args:     []string{"serve", "extra"},
			wantCode: ExitUsage,
			wantErr:  []string{`serve: unexpected argument "extra"`},
		},
		{
			name:     "check の余分な引数は ExitUsage で終了する",
			args:     []string{"check", "extra"},
			wantCode: ExitUsage,
			wantErr:  []string{`check: unexpected argument "extra"`},
		},
		{
			name:     "version の余分な引数は ExitUsage で終了する",
			args:     []string{"version", "extra"},
			wantCode: ExitUsage,
			wantErr:  []string{`version: unexpected argument "extra"`},
		},
		{
			name:     "serve の未知 flag は ExitUsage で終了する",
			args:     []string{"serve", "--frobnicate"},
			wantCode: ExitUsage,
			wantErr:  []string{"flag provided but not defined"},
		},
		{
			name:     "check の未知 flag は ExitUsage で終了する",
			args:     []string{"check", "--frobnicate"},
			wantCode: ExitUsage,
			wantErr:  []string{"flag provided but not defined"},
		},
		{
			name:     "version の未知 flag は ExitUsage で終了する",
			args:     []string{"version", "--frobnicate"},
			wantCode: ExitUsage,
			wantErr:  []string{"flag provided but not defined"},
		},
		{
			name:     "serve の config 欠落は error を stderr へ出して ExitError で終了する",
			args:     []string{"serve", "--config", missingConfig},
			wantCode: ExitError,
			wantErr:  []string{"load config"},
		},
		{
			name:     "check の config 欠落は error を stderr へ出して ExitError で終了する",
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
				t.Fatalf("終了 code が期待と異なります: got %d, want %d", code, tt.wantCode)
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
				t.Fatalf("%s の不正 config の終了 code が ExitError ではありません: %d", cmd, code)
			}
			if !strings.Contains(stderr, "invalid config") {
				t.Fatalf("%s の validation error が stderr にありません", cmd)
			}
			if stdout != "" {
				t.Fatalf("%s は stdout へ何も出力しません", cmd)
			}
			if strings.Contains(stderr, secretMarker) || strings.Contains(stdout, secretMarker) {
				t.Fatalf("%s の出力へ秘密値 (marker) が漏れています", cmd)
			}
		})
	}
}
