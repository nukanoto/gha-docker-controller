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
			t.Fatalf("不正な config が error になりません")
		} else if strings.Contains(err.Error(), marker) {
			t.Fatalf("PAT が error に露出しました")
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
			t.Fatalf("App 検証が error になりません")
		}
		if _, err := New(appCfg(marker), "v1", "abc"); err == nil {
			t.Fatalf("不正な config が error になりません")
		} else if strings.Contains(err.Error(), marker) {
			t.Fatalf("private key が error に露出しました")
		}
	})
}

// TestNew_AuthNotConfiguredIsRejected covers the constructor guard.
func TestNew_AuthNotConfiguredIsRejected(t *testing.T) {
	if _, err := New(newTestConfig("octo", ""), "v1", "abc"); err == nil {
		t.Fatalf("認証未設定の config が error になりません")
	} else if !strings.Contains(err.Error(), "github auth is not configured") {
		t.Fatalf("認証未設定の error 文言が不正です: %v", err)
	}
}

// TestNew_ValidConfigConstructsWithoutIO covers local client construction.
func TestNew_ValidConfigConstructsWithoutIO(t *testing.T) {
	client, err := New(newTestConfig("octo", "ghp_validtoken"), "v1", "abc")
	if err != nil {
		t.Fatalf("妥当な config が error になりました: %v", err)
	}
	if client == nil {
		t.Fatalf("Client が nil です")
	}
}
