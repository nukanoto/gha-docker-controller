package scaleset

import (
	"strings"
	"testing"

	"github.com/nukanoto/arc-docker/internal/config"
)

// newTestConfig returns a minimal constructor config.
func newTestConfig(owner, token string) *config.Config {
	return &config.Config{
		GitHub: config.GitHubConfig{
			URL:   "https://github.com",
			Scope: config.ScopeOrganization,
			Owner: owner,
			Token: token,
		},
	}
}

// TestNew_SecretNotExposedInError covers constructor redaction.
func TestNew_SecretNotExposedInError(t *testing.T) {
	t.Run("PAT", func(t *testing.T) {
		const marker = "ghp_SECRET_MARKER_XYZ"
		if _, err := New(newTestConfig("", marker), "v1", "abc"); err == nil {
			t.Fatalf("invalid config did not return an error")
		} else if strings.Contains(err.Error(), marker) {
			t.Fatalf("PAT was exposed in the error")
		}
	})
	t.Run("App private key", func(t *testing.T) {
		const marker = "PRIVATE_KEY_MARKER_XYZ"
		appCfg := func(privateKey string) *config.Config {
			cfg := newTestConfig("", "")
			cfg.GitHub.App = &config.GitHubAppConfig{
				AppID:          1,
				InstallationID: 1,
				PrivateKey:     []byte(privateKey),
			}
			return cfg
		}
		if _, err := New(appCfg(""), "v1", "abc"); err == nil ||
			!strings.Contains(err.Error(), "github app auth: app private key is required") {
			t.Fatalf("App validation did not return an error")
		}
		if _, err := New(appCfg(marker), "v1", "abc"); err == nil {
			t.Fatalf("invalid config did not return an error")
		} else if strings.Contains(err.Error(), marker) {
			t.Fatalf("private key was exposed in the error")
		}
	})
}

// TestNew_AuthNotConfiguredIsRejected covers the constructor guard.
func TestNew_AuthNotConfiguredIsRejected(t *testing.T) {
	if _, err := New(newTestConfig("octo", ""), "v1", "abc"); err == nil {
		t.Fatalf("config without authentication did not return an error")
	} else if !strings.Contains(err.Error(), "github auth is not configured") {
		t.Fatalf("error for missing authentication is incorrect: %v", err)
	}
}

// TestNew_ValidConfigConstructsWithoutIO covers local client construction.
func TestNew_ValidConfigConstructsWithoutIO(t *testing.T) {
	client, err := New(newTestConfig("octo", "ghp_validtoken"), "v1", "abc")
	if err != nil {
		t.Fatalf("valid config returned an error: %v", err)
	}
	if client == nil {
		t.Fatalf("Client is nil")
	}
}
