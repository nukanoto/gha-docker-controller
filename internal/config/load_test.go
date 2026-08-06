package config

// End-to-end tests for Load using a temporary directory and real files
// (mocks/stubs are forbidden). Covered: defaults, required fields, auth
// exclusivity, normalization and profiles. Individual rules are covered by
// validate_test.go, schema_test.go, secret_test.go and parse_format_test.go.

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// keyPEM is a dummy PEM standing in for the secret file body. It is not a
// real key; a unique string is used to verify that secrets are not leaked.
const keyPEM = "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA-dummy-key-for-test\n-----END RSA PRIVATE KEY-----"

// sha256Digest is the multi-platform index digest of the official runner
// 2.336.0.
const sha256Digest = "sha256:0cfdcc701ce933c6d243c6b0b2da767366dc9f2e99961d4c3754b0b78084cdda"

// minimalConfigYAML is the smallest config with only the mandatory fields.
// Used to verify defaults.
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
  image: ghcr.io/actions/actions-runner:2.336.0
  cpu: "1"
  memory: 1GiB
  memorySwap: 1GiB
  pidsLimit: 128
`

// baseConfigYAML is the shared base for Load table tests, including the
// docker section and timeouts.
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
  runtime: runsc
  network: bridge
  pullPolicy: if-not-present
runner:
  image: ghcr.io/actions/actions-runner:2.336.0
  cpu: "1"
  memory: 1GiB
  memorySwap: 1GiB
  pidsLimit: 128
  provisioningTimeout: 5m
  stopTimeout: 30s
`

// patRepoYAML is a repository-scope config using PAT (GITHUB_TOKEN env)
// authentication. The config.example.yaml schema is validated against the
// real repository file by TestLoad_ConfigExampleFile.
const patRepoYAML = `github:
  url: https://github.com/
  scope: repository
  owner: my-org
  repository: my-repo
scaleSet:
  name: prod
  maxRunners: 2
runner:
  image: ghcr.io/actions/actions-runner:2.336.0
  cpu: "1"
  memory: 1GiB
  memorySwap: 1GiB
  pidsLimit: 128
`

// mustWriteSecret writes a real file into a temporary directory and returns
// its path. The permission is forced with chmod after creation so umask
// cannot interfere.
func mustWriteSecret(t *testing.T, name string, content string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("secret file の作成に失敗しました: %v", err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatalf("secret file の permission 設定に失敗しました: %v", err)
	}
	return path
}

// writeConfig writes YAML into a temporary directory and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("config file の作成に失敗しました: %v", err)
	}
	return path
}

// baseWithKey returns baseConfigYAML with __KEY__ replaced by the real path
// of a temporary key file.
func baseWithKey(t *testing.T) string {
	t.Helper()
	return strings.ReplaceAll(baseConfigYAML, "__KEY__", mustWriteSecret(t, "key.pem", keyPEM, 0600))
}

// loadDoc writes doc to a temporary file, loads it, requires success and
// returns the Config and Warnings.
func loadDoc(t *testing.T, doc string) (*Config, []Warning) {
	t.Helper()
	c, warnings, err := Load(writeConfig(t, doc))
	if err != nil {
		t.Fatalf("Load が失敗しました: %v", err)
	}
	return c, warnings
}

// checkErr verifies that err is nil when wantErr is empty, and that err's
// message contains wantErr otherwise.
func checkErr(t *testing.T, name, wantErr string, err error) {
	t.Helper()
	if wantErr == "" {
		if err != nil {
			t.Fatalf("%s: 期待しない error が返りました: %v", name, err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("%s: 期待 error %q がありません: err=%v", name, wantErr, err)
	}
}

// TestLoad_ConfigExampleFile loads the repository config.example.yaml with
// the secret file path replaced, and verifies there are zero warnings and the
// normalization results (the schema itself is not modified).
func TestLoad_ConfigExampleFile(t *testing.T) {
	data, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatalf("config.example.yaml の読み込みに失敗しました: %v", err)
	}
	keyPath := mustWriteSecret(t, "key.pem", keyPEM, 0600)
	replaced := strings.ReplaceAll(string(data), "/etc/gha-docker-controller/github-app.pem", keyPath)
	c, warnings, err := Load(writeConfig(t, replaced))
	if err != nil {
		t.Fatalf("config.example.yaml の Load が失敗しました: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("example に warning が返りました: %+v", warnings)
	}
	// Verify the normalization of the fully explicit config with a few
	// representative fields.
	if c.GitHub.URL != "https://github.com" || c.GitHub.Owner != "your-organization" || c.GitHub.App.AppID != 123456 || !bytes.Equal(c.GitHub.App.PrivateKey, []byte(keyPEM)) {
		t.Fatalf("github の正規化結果が不正です: %+v", c.GitHub)
	}
	if c.ScaleSet != (ScaleSetConfig{Name: "production", RunnerGroup: "default", MinRunners: 0, MaxRunners: 4}) {
		t.Fatalf("scaleSet の正規化結果が不正です: %+v", c.ScaleSet)
	}
	if c.Docker != (DockerConfig{Host: "unix:///var/run/docker.sock", Runtime: "runsc", Network: "bridge", PullPolicy: PullPolicyIfNotPresent}) {
		t.Fatalf("docker の正規化結果が不正です: %+v", c.Docker)
	}
	if c.Runner.Image != "ghcr.io/actions/actions-runner@"+sha256Digest || c.Runner.Profile != ProfileStandard || c.Runner.CPU != NanoCPUs(2000000000) {
		t.Fatalf("runner の正規化結果が不正です: image=%q profile=%q cpu=%d", c.Runner.Image, c.Runner.Profile, c.Runner.CPU)
	}
	if !reflect.DeepEqual(c.Runner.CapDrop, []string{"ALL"}) || !c.Runner.NoNewPrivileges {
		t.Fatalf("runner の security 正規化結果が不正です: capDrop=%v nnp=%v", c.Runner.CapDrop, c.Runner.NoNewPrivileges)
	}
	if c.Shutdown.BusyPolicy != ShutdownPolicyLeave || c.Log.Format != LogFormatJSON || c.Health.Listen != "127.0.0.1:8080" {
		t.Fatalf("health/shutdown/log の正規化結果が不正です: %+v %+v %+v", c.Health, c.Shutdown, c.Log)
	}
	// The trailing "/" of the url is normalized and the base URL is composed
	// of the owner only.
	if got := c.GitHubConfigURL(); got != "https://github.com/your-organization" {
		t.Fatalf("GitHubConfigURL が不正です: 期待値 %q、実測値 %q", "https://github.com/your-organization", got)
	}
}

// TestLoad_DefaultsApplied verifies that defaults are applied to unset
// fields.
func TestLoad_DefaultsApplied(t *testing.T) {
	keyPath := mustWriteSecret(t, "key.pem", keyPEM, 0600)
	c, warnings := loadDoc(t, strings.ReplaceAll(minimalConfigYAML, "__KEY__", keyPath))
	if len(warnings) != 0 {
		t.Fatalf("期待しない warning が返りました: %+v", warnings)
	}
	if c.GitHub.URL != DefaultGitHubURL || c.ScaleSet.RunnerGroup != DefaultRunnerGroup || c.ScaleSet.MinRunners != 0 {
		t.Fatalf("既定値が適用されていません: url=%q group=%q min=%d", c.GitHub.URL, c.ScaleSet.RunnerGroup, c.ScaleSet.MinRunners)
	}
	if c.Runner.Profile != DefaultProfile || c.Runner.ProvisioningTimeout != Duration(DefaultProvisioningTimeout) || c.Runner.StopTimeout != Duration(DefaultStopTimeout) || c.Runner.Network != DefaultNetwork {
		t.Fatalf("runner の既定値が適用されていません: profile=%q provisioning=%v stop=%v network=%q", c.Runner.Profile, c.Runner.ProvisioningTimeout, c.Runner.StopTimeout, c.Runner.Network)
	}
	// Struct-level defaults are verified in a table.
	tests := []struct {
		name string
		got  any
		want any
	}{
		{"docker", c.Docker, DockerConfig{Host: DefaultDockerHost, Runtime: DefaultRuntime, Network: DefaultNetwork, PullPolicy: DefaultPullPolicy}},
		{"runner security", c.Runner.CapDrop, []string{"ALL"}},
		{"runner privilege", c.Runner.NoNewPrivileges, true},
		{"nestedDocker", c.NestedDocker, NestedDockerConfig{Storage: DefaultNestedStorage, StorageSize: DefaultNestedStorageSize}},
		{"health", c.Health, HealthConfig{Listen: DefaultHealthListen}},
		{"shutdown", c.Shutdown, ShutdownConfig{BusyPolicy: DefaultShutdownPolicy, Grace: Duration(DefaultShutdownGrace)}},
		{"log", c.Log, LogConfig{Format: DefaultLogFormat, Level: DefaultLogLevel}},
	}
	for _, tt := range tests {
		if !reflect.DeepEqual(tt.got, tt.want) {
			t.Fatalf("%s の既定値が不正です: got=%+v want=%+v", tt.name, tt.got, tt.want)
		}
	}
}

// TestLoad_RepoScopeWithPAT verifies the combination of repository scope with
// PAT (GITHUB_TOKEN env) authentication and url normalization. The PAT body
// never appears in logs or errors.
func TestLoad_RepoScopeWithPAT(t *testing.T) {
	token := "ghp_secret-token-value-12345"
	t.Setenv("GITHUB_TOKEN", token)
	c, warnings := loadDoc(t, patRepoYAML)
	if len(warnings) != 0 {
		t.Fatalf("期待しない warning が返りました: %+v", warnings)
	}
	if c.GitHub.Token != token {
		// Never print the PAT value on failure (avoids re-exposing the
		// secret).
		t.Fatal("PAT の読み込み結果が不正です (値は秘密のため出力しません)")
	}
	if c.GitHub.App != nil {
		t.Fatal("PAT 設定時に App が構築されました")
	}
	if c.GitHub.URL != "https://github.com" {
		t.Fatalf("url の正規化が不正です: 期待値 %q、実測値 %q", "https://github.com", c.GitHub.URL)
	}
	if got := c.GitHubConfigURL(); got != "https://github.com/my-org/my-repo" {
		t.Fatalf("GitHubConfigURL が不正です: 期待値 %q、実測値 %q", "https://github.com/my-org/my-repo", got)
	}
}

// TestLoad_RunnerNetworkInheritanceAndMismatch verifies runner.network
// inheritance, mismatch rejection and host rejection.
func TestLoad_RunnerNetworkInheritanceAndMismatch(t *testing.T) {
	base := baseWithKey(t)

	// Inheritance: keep runner.network unset and change docker.network to
	// user-net.
	doc := strings.Replace(base, "  network: bridge\n", "  network: user-net\n", 1)
	if c, _ := loadDoc(t, doc); c.Runner.Network != "user-net" || c.Docker.Network != "user-net" {
		t.Fatalf("runner.network の継承が不正です")
	}

	// Explicit match passes.
	doc = strings.Replace(base, "  cpu: \"1\"\n", "  network: user-net\n  cpu: \"1\"\n", 1)
	doc = strings.Replace(doc, "  network: bridge\n", "  network: user-net\n", 1)
	if c, _ := loadDoc(t, doc); c.Runner.Network != "user-net" {
		t.Fatalf("明示した runner.network が反映されていません: %q", c.Runner.Network)
	}

	// Mismatch and host are rejected (the detailed rules live in
	// validate_test.go).
	doc = strings.Replace(base, "  cpu: \"1\"\n", "  network: user-net\n  cpu: \"1\"\n", 1)
	_, _, err := Load(writeConfig(t, doc))
	checkErr(t, "mismatch is rejected", "runner.network: must match docker.network", err)
	doc = strings.Replace(base, "  cpu: \"1\"\n", "  network: host\n  cpu: \"1\"\n", 1)
	_, _, err = Load(writeConfig(t, doc))
	checkErr(t, "runner host network is rejected", "host network is not allowed", err)
}

// TestLoad_NestedProfileDefaults verifies that nested-docker requires a
// digest image and that the default CapAdd is the set of 17 capabilities.
func TestLoad_NestedProfileDefaults(t *testing.T) {
	base := baseWithKey(t)
	imageLine := "  image: ghcr.io/actions/actions-runner:2.336.0\n"
	nestedImage := "  image: ghcr.io/actions/actions-runner@" + sha256Digest + "\n"

	// A complete nested config (digest image) gets the default of 17 CapAdd
	// capabilities.
	doc := strings.Replace(base, imageLine, nestedImage+"  profile: nested-docker\n", 1)
	c, _ := loadDoc(t, doc)
	if c.Runner.Profile != ProfileNestedDocker {
		t.Fatalf("nested の profile が反映されていません: %+v", c.NestedDocker)
	}
	if !reflect.DeepEqual(c.Runner.CapAdd, NestedCapabilities()) {
		t.Fatalf("nested の CapAdd 既定が 17 個の集合と一致しません: %v", c.Runner.CapAdd)
	}

	// A subset CapAdd is accepted and reflected as specified.
	doc = strings.Replace(base, imageLine, nestedImage+"  profile: nested-docker\n  capAdd: [\"CHOWN\"]\n", 1)
	if c, _ := loadDoc(t, doc); !reflect.DeepEqual(c.Runner.CapAdd, []string{"CHOWN"}) {
		t.Fatalf("CapAdd の部分集合が反映されていません: %v", c.Runner.CapAdd)
	}

	// Rejections: tag image and runtime mismatch.
	tests := []struct {
		name    string
		with    string
		wantErr string
	}{
		{name: "nested tag image is rejected", with: imageLine + "  profile: nested-docker\n", wantErr: "requires a digest reference"},
		{name: "nested runc is rejected", with: nestedImage + "  profile: nested-docker\n", wantErr: "docker.runtime: nested-docker profile requires"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := strings.Replace(base, imageLine, tt.with, 1)
			if tt.name == "nested runc is rejected" {
				doc = strings.Replace(doc, "  runtime: runsc\n", "  runtime: runc\n", 1)
			}
			_, _, err := Load(writeConfig(t, doc))
			checkErr(t, tt.name, tt.wantErr, err)
		})
	}

	// standard can freely set any registered runtime.
	doc = strings.Replace(base, "  runtime: runsc\n", "  runtime: runc\n", 1)
	if c, _ := loadDoc(t, doc); c.Docker.Runtime != "runc" {
		t.Fatalf("standard の runtime が反映されていません: %q", c.Docker.Runtime)
	}
}

// TestLoad_RequiredFieldsMissing rejects missing mandatory fields with an
// error carrying the field path.
func TestLoad_RequiredFieldsMissing(t *testing.T) {
	base := baseWithKey(t)

	tests := []struct {
		name    string
		remove  string
		wantErr string
	}{
		{name: "missing owner", remove: "  owner: my-org\n", wantErr: "github.owner"},
		{name: "missing app", wantErr: "github.app or GITHUB_TOKEN is required"},
		{name: "missing scaleset name", remove: "  name: prod\n", wantErr: "scaleSet.name"},
		{name: "missing maxRunners", remove: "  maxRunners: 2\n", wantErr: "scaleSet.maxRunners"},
		{name: "missing image", remove: "  image: ghcr.io/actions/actions-runner:2.336.0\n", wantErr: "runner.image"},
		{name: "missing cpu", remove: "  cpu: \"1\"\n", wantErr: "runner.cpu"},
		{name: "missing memory", remove: "  memory: 1GiB\n", wantErr: "runner.memory"},
		{name: "missing memorySwap", remove: "  memorySwap: 1GiB\n", wantErr: "runner.memorySwap"},
		{name: "missing pidsLimit", remove: "  pidsLimit: 128\n", wantErr: "runner.pidsLimit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := base
			if tt.remove != "" {
				doc = strings.ReplaceAll(base, tt.remove, "")
				if doc == base {
					t.Fatalf("test の remove 文字列 %q が base に一致しません", tt.remove)
				}
			} else {
				// Remove the app block (from "  app:\n" up to just before
				// "scaleSet:"). Clear the env so an environment PAT cannot
				// accidentally conflict.
				t.Setenv("GITHUB_TOKEN", "")
				i := strings.Index(base, "  app:\n")
				j := strings.Index(base[i:], "\nscaleSet:")
				doc = base[:i] + base[i+j:]
			}
			_, _, err := Load(writeConfig(t, doc))
			checkErr(t, tt.name, tt.wantErr, err)
		})
	}
}

// TestLoad_FileLevelRejections rejects an empty file and multiple documents
// (syntax rules on the same level as unknown fields).
func TestLoad_FileLevelRejections(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{name: "empty file", content: "", wantErr: "config file is empty"},
		{name: "multiple documents", content: "github: {}\n---\nscaleSet: {}\n", wantErr: "multiple YAML documents are not allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Load(writeConfig(t, tt.content))
			checkErr(t, tt.name, tt.wantErr, err)
		})
	}
}

// TestLoad_AuthConflictAndIncompleteApp rejects App and GITHUB_TOKEN
// coexistence and missing mandatory App fields.
func TestLoad_AuthConflictAndIncompleteApp(t *testing.T) {
	keyPath := mustWriteSecret(t, "key.pem", keyPEM, 0600)
	base := strings.ReplaceAll(baseConfigYAML, "__KEY__", keyPath)

	tests := []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{name: "app and GITHUB_TOKEN conflict", mutate: func(doc string) string {
			// An environment PAT together with App yields a fixed error.
			t.Setenv("GITHUB_TOKEN", "ghp_xxx")
			return doc
		}, wantErr: "mutually exclusive"},
		{name: "neither app nor GITHUB_TOKEN", mutate: func(doc string) string {
			// Clear the env so an environment PAT cannot accidentally
			// conflict.
			t.Setenv("GITHUB_TOKEN", "")
			// Remove the app block of the github section up to just before
			// "scaleSet:".
			i := strings.Index(doc, "  app:\n")
			j := strings.Index(doc[i:], "\nscaleSet:")
			return doc[:i] + doc[i+j:]
		}, wantErr: "github.app or GITHUB_TOKEN is required"},
		{name: "zero appId", mutate: func(doc string) string {
			return strings.Replace(doc, "    id: 1\n", "    id: 0\n", 1)
		}, wantErr: "github.app.id: positive integer required"},
		{name: "zero installationId", mutate: func(doc string) string {
			return strings.Replace(doc, "    installationId: 2\n", "    installationId: 0\n", 1)
		}, wantErr: "github.app.installationId: positive integer required"},
		{name: "missing privateKeyFile", mutate: func(doc string) string {
			return strings.Replace(doc, "    privateKeyFile: "+keyPath+"\n", "    privateKeyFile: \"\"\n", 1)
		}, wantErr: "github.app.privateKeyFile: required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Load(writeConfig(t, tt.mutate(base)))
			checkErr(t, tt.name, tt.wantErr, err)
		})
	}
}

// TestLoad_ForcesGHESRejected rejects the GITHUB_ACTIONS_FORCE_GHES
// environment variable.
func TestLoad_ForcesGHESRejected(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS_FORCE_GHES", "1")
	_, _, err := Load("this-file-does-not-matter.yaml")
	checkErr(t, "ghes env is rejected", "GITHUB_ACTIONS_FORCE_GHES", err)
}
