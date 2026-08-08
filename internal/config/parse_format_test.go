package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func unmarshalInto(t *testing.T, name, doc string, value any, wantErr string) error {
	t.Helper()
	err := yaml.Unmarshal([]byte(doc), value)
	checkErr(t, name, wantErr, err)
	return err
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want time.Duration
		err  string
	}{
		{"minutes", "5m", 5 * time.Minute, ""},
		{"compound", "1h30m", 90 * time.Minute, ""},
		{"bare number", "300", 0, "not a bare number"},
		{"zero", "0s", 0, "duration must be positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got Duration
			if err := unmarshalInto(t, tt.name, tt.doc, &got, tt.err); err == nil && tt.err == "" && got != Duration(tt.want) {
				t.Fatalf("duration の変換結果が不正です: got=%v want=%v", got, tt.want)
			}
		})
	}
}

func TestNormalizeDockerHost(t *testing.T) {
	if got := normalizeDockerHost("unix:///tmp/docker.sock/"); got != "unix:///tmp/docker.sock" {
		t.Fatalf("Docker host の正規化結果が不正です: %q", got)
	}
}

func TestValidName(t *testing.T) {
	for _, test := range []struct {
		value string
		want  bool
	}{
		{"my-org_1.prod", true},
		{"", false},
		{"a/b", false},
		{"日本語", false},
	} {
		if got := validName(test.value); got != test.want {
			t.Fatalf("validName(%q) が不正です: got=%v want=%v", test.value, got, test.want)
		}
	}
}

func TestValidateGitHubURLAndDockerHost(t *testing.T) {
	checkErr(t, "GitHub URL", "", validateGitHubURL("https://github.com/"))
	checkErr(t, "GHES URL", "host must be exactly github.com", validateGitHubURL("https://ghe.example"))
	checkErr(t, "Docker socket", "", validateDockerHost("unix:///tmp/docker.sock"))
	checkErr(t, "Docker TCP", "only unix://", validateDockerHost("tcp://127.0.0.1:2375"))
}

func TestValidateImage_DockerSyntaxOnly(t *testing.T) {
	tests := []struct {
		name  string
		image string
		err   string
	}{
		{"tag", "ubuntu:latest", ""},
		{"tagless", "ubuntu", ""},
		{"digest", "ghcr.io/actions/actions-runner@sha256:" + strings.Repeat("a", 64), ""},
		{"empty", "", "required"},
		{"bad tag", "ubuntu:bad!", "invalid image name"},
		{"bad digest", "ubuntu@sha256:short", "invalid image name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkErr(t, tt.name, tt.err, validateImage(tt.image))
		})
	}
}

func TestValidateListen(t *testing.T) {
	checkErr(t, "IPv4", "", validateListen("127.0.0.1:8080"))
	checkErr(t, "IPv6", "", validateListen("[::1]:8080"))
	checkErr(t, "port zero", "invalid port", validateListen("127.0.0.1:0"))
	checkErr(t, "missing port", "must be host:port", validateListen("localhost"))
}
