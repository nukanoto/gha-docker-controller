package scaleset

import (
	"strings"
	"testing"

	"github.com/nukanoto/gha-docker-controller/internal/config"
)

// newTestConfig returns a minimal config for constructor tests. An empty
// Owner makes GitHubConfigURL invalid as an organization, so the official
// client's URL parse returns an error without I/O.
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

// TestNew_SecretNotExposedInError verifies that the PAT and the App private
// key never leak into constructor errors. Both the official client's URL
// parse error and the App validation error are covered here.
func TestNew_SecretNotExposedInError(t *testing.T) {
	t.Run("PAT", func(t *testing.T) {
		const marker = "ghp_SECRET_MARKER_XYZ"
		// An empty Owner makes the URL parse return an error without I/O.
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
		// A missing App field is a validation error that never includes the
		// key body.
		if _, err := New(appCfg(""), "v1", "abc"); err == nil ||
			!strings.Contains(err.Error(), "github app auth: app private key is required") {
			t.Fatalf("App 検証が error になりません")
		}
		// The URL parse error also excludes the key body.
		if _, err := New(appCfg(marker), "v1", "abc"); err == nil {
			t.Fatalf("不正な config が error になりません")
		} else if strings.Contains(err.Error(), marker) {
			t.Fatalf("private key が error に露出しました")
		}
	})
}

// TestNew_AuthNotConfiguredIsRejected verifies that a config with neither PAT
// nor App is rejected before I/O. App/PAT exclusivity is already guaranteed by
// config validation; this is the constructor's defensive input check.
func TestNew_AuthNotConfiguredIsRejected(t *testing.T) {
	if _, err := New(newTestConfig("octo", ""), "v1", "abc"); err == nil {
		t.Fatalf("認証未設定の config が error になりません")
	} else if !strings.Contains(err.Error(), "github auth is not configured") {
		t.Fatalf("認証未設定の error 文言が不正です: %v", err)
	}
}

// TestNew_ValidConfigConstructsWithoutIO verifies that a valid config passes
// the constructor without I/O. Creating the official client only parses the
// URL and builds retryablehttp; no network is involved.
func TestNew_ValidConfigConstructsWithoutIO(t *testing.T) {
	client, err := New(newTestConfig("octo", "ghp_validtoken"), "v1", "abc")
	if err != nil {
		t.Fatalf("妥当な config が error になりました: %v", err)
	}
	if client == nil {
		t.Fatalf("Client が nil です")
	}
}
