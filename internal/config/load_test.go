package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
)

const keyPEM = "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA-dummy-key-for-test\n-----END RSA PRIVATE KEY-----"

const minimalConfigYAML = `github:
  scope: organization
  owner: my-org
  app:
    id: 1
    installationId: 2
    privateKeyFile: __KEY__
scaleSet:
  name: prod
  maxRunners: 2
runner:
  image: ubuntu
`

const baseConfigYAML = `github:
  scope: organization
  owner: my-org
  app:
    id: 1
    installationId: 2
    privateKeyFile: __KEY__
scaleSet:
  name: prod
  maxRunners: 2
docker:
  host: unix:///var/run/docker.sock
  pullPolicy: if-not-present
runner:
  image: ghcr.io/actions/actions-runner:2.336.0
  provisioningTimeout: 5m
  stopTimeout: 30s
`

const patRepoYAML = `github:
  url: https://github.com/
  scope: repository
  owner: my-org
  repository: my-repo
scaleSet:
  name: prod
  maxRunners: 2
runner:
  image: ubuntu
`

func mustWriteSecret(t *testing.T, name, content string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("秘密ファイルを作成できませんでした: %v", err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatalf("秘密ファイルの権限を設定できませんでした: %v", err)
	}
	return path
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("設定ファイルを作成できませんでした: %v", err)
	}
	return path
}

func baseWithKey(t *testing.T) string {
	t.Helper()
	return strings.ReplaceAll(baseConfigYAML, "__KEY__", mustWriteSecret(t, "key.pem", keyPEM, 0600))
}

func loadDoc(t *testing.T, doc string) (*Config, []Warning) {
	t.Helper()
	c, warnings, err := Load(writeConfig(t, doc))
	if err != nil {
		t.Fatalf("設定を読み込めませんでした: %v", err)
	}
	return c, warnings
}

func checkErr(t *testing.T, name, wantErr string, err error) {
	t.Helper()
	if wantErr == "" {
		if err != nil {
			t.Fatalf("%s: 予期しないエラーです: %v", name, err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("%s: エラー %q が返りませんでした: %v", name, wantErr, err)
	}
}

func TestLoad_ConfigExampleFile(t *testing.T) {
	data, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatalf("config.example.yaml を読めませんでした: %v", err)
	}
	keyPath := mustWriteSecret(t, "key.pem", keyPEM, 0600)
	replaced := strings.ReplaceAll(string(data), "/etc/gha-docker-controller/github-app.pem", keyPath)
	c, warnings, err := Load(writeConfig(t, replaced))
	if err != nil {
		t.Fatalf("config.example.yaml を読み込めませんでした: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("example に警告があります: %+v", warnings)
	}
	if c.GitHub.URL != "https://github.com" || c.GitHub.Owner != "your-organization" ||
		c.GitHub.App.AppID != 123456 || !bytes.Equal(c.GitHub.App.PrivateKey, []byte(keyPEM)) {
		t.Fatalf("GitHub 設定の正規化結果が不正です: %+v", c.GitHub)
	}
	if c.ScaleSet != (ScaleSetConfig{Name: "my-gha-docker-runner", RunnerGroup: "Default", MinRunners: 0, MaxRunners: 4}) {
		t.Fatalf("Scale Set 設定の正規化結果が不正です: %+v", c.ScaleSet)
	}
	if c.Docker != (DockerConfig{Host: DefaultDockerHost, PullPolicy: DefaultPullPolicy}) {
		t.Fatalf("Docker 設定の正規化結果が不正です: %+v", c.Docker)
	}
	if c.Runner.Image == "" || c.Runner.HostConfig == nil {
		t.Fatalf("runner 設定の正規化結果が不正です: %+v", c.Runner)
	}
	if c.Shutdown.BusyPolicy != ShutdownPolicyLeave || c.Log.Format != LogFormatJSON || c.Health.Listen != DefaultHealthListen {
		t.Fatalf("health、shutdown、log の既定値が不正です: %+v %+v %+v", c.Health, c.Shutdown, c.Log)
	}
}

func TestLoad_DefaultsApplied(t *testing.T) {
	keyPath := mustWriteSecret(t, "key.pem", keyPEM, 0600)
	c, warnings := loadDoc(t, strings.ReplaceAll(minimalConfigYAML, "__KEY__", keyPath))
	if len(warnings) != 0 {
		t.Fatalf("予期しない警告があります: %+v", warnings)
	}
	wants := []struct {
		name string
		got  any
		want any
	}{
		{"GitHub URL", c.GitHub.URL, DefaultGitHubURL},
		{"runner group", c.ScaleSet.RunnerGroup, DefaultRunnerGroup},
		{"Docker", c.Docker, DockerConfig{Host: DefaultDockerHost, PullPolicy: DefaultPullPolicy}},
		{"HostConfig", c.Runner.HostConfig, (*container.HostConfig)(nil)},
		{"provisioning timeout", c.Runner.ProvisioningTimeout, Duration(DefaultProvisioningTimeout)},
		{"stop timeout", c.Runner.StopTimeout, Duration(DefaultStopTimeout)},
		{"health", c.Health, HealthConfig{Listen: DefaultHealthListen}},
		{"shutdown", c.Shutdown, ShutdownConfig{BusyPolicy: DefaultShutdownPolicy, Grace: Duration(DefaultShutdownGrace)}},
		{"log", c.Log, LogConfig{Format: DefaultLogFormat, Level: DefaultLogLevel}},
	}
	for _, tt := range wants {
		if !reflect.DeepEqual(tt.got, tt.want) {
			t.Fatalf("%s の既定値が不正です: got=%+v want=%+v", tt.name, tt.got, tt.want)
		}
	}
}

func TestLoad_RepoScopeWithPAT(t *testing.T) {
	token := "ghp_secret-token-value-12345"
	t.Setenv("GITHUB_TOKEN", token)
	c, warnings := loadDoc(t, patRepoYAML)
	if len(warnings) != 0 || c.GitHub.Token != token || c.GitHub.App != nil {
		t.Fatal("PAT 認証の読み込み結果が不正です")
	}
	if got := c.GitHubConfigURL(); got != "https://github.com/my-org/my-repo" {
		t.Fatalf("GitHubConfigURL が不正です: %q", got)
	}
}

func TestLoad_RequiredFieldsMissing(t *testing.T) {
	base := baseWithKey(t)
	tests := []struct {
		name   string
		remove string
		want   string
	}{
		{"owner", "  owner: my-org\n", "github.owner"},
		{"scale set name", "  name: prod\n", "scaleSet.name"},
		{"max runners", "  maxRunners: 2\n", "scaleSet.maxRunners"},
		{"image", "  image: ghcr.io/actions/actions-runner:2.336.0\n", "runner.image"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Load(writeConfig(t, strings.Replace(base, tt.remove, "", 1)))
			checkErr(t, tt.name, tt.want, err)
		})
	}
}

func TestLoad_FileLevelRejections(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{"empty", "", "config file is empty"},
		{"multiple documents", "github: {}\n---\nscaleSet: {}\n", "multiple YAML documents are not allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Load(writeConfig(t, tt.doc))
			checkErr(t, tt.name, tt.want, err)
		})
	}
}

func TestLoad_AuthConflictAndIncompleteApp(t *testing.T) {
	keyPath := mustWriteSecret(t, "key.pem", keyPEM, 0600)
	base := strings.ReplaceAll(baseConfigYAML, "__KEY__", keyPath)
	withoutApp := strings.Replace(base, "  app:\n    id: 1\n    installationId: 2\n    privateKeyFile: "+keyPath+"\n", "", 1)
	tests := []struct {
		name  string
		doc   string
		token string
		want  string
	}{
		{"App and PAT", base, "ghp_xxx", "mutually exclusive"},
		{"missing App and PAT", withoutApp, "", "github.app or GITHUB_TOKEN is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GITHUB_TOKEN", tt.token)
			_, _, err := Load(writeConfig(t, tt.doc))
			checkErr(t, tt.name, tt.want, err)
		})
	}
}

func TestLoad_ForcesGHESRejected(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS_FORCE_GHES", "1")
	_, _, err := Load("this-file-does-not-matter.yaml")
	checkErr(t, "GHES environment", "GITHUB_ACTIONS_FORCE_GHES", err)
}
