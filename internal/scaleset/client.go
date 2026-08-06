// Package scaleset provides authentication for the official actions/scaleset
// client, Scale Set get-or-create, JIT config, and message sessions.
// HTTP retry follows the official client's default behavior.
package scaleset

import (
	"fmt"
	"log/slog"
	"strconv"

	scalesetapi "github.com/actions/scaleset"

	"github.com/nukanoto/gha-docker-controller/internal/config"
)

// systemName is the implementation name passed to the official client's SystemInfo.
const systemName = "gha-docker-controller"

// Client holds the official actions/scaleset client and build info.
type Client struct {
	// official is the official actions/scaleset client.
	official *scalesetapi.Client
	// version and commit are build info used to update the official client's
	// SystemInfo. New receives them from cli/buildinfo.
	version string
	commit  string
}

// New creates a Client from the validated config and build info.
// cfg is the runtime config built by internal/config. Secrets (PAT, App
// private key) are only passed to the official client's config and are never
// included in logs or errors. version and commit come from cli/buildinfo.
func New(cfg *config.Config, version, commit string) (*Client, error) {
	official, err := newOfficialClient(cfg, version, commit)
	if err != nil {
		return nil, err
	}
	return &Client{official: official, version: version, commit: commit}, nil
}

// newOfficialClient creates the official actions/scaleset client from the
// validated config. App auth converts the positive YAML appId to a decimal
// string for ClientID; PAT auth passes the token to PersonalAccessToken. The
// GitHubConfigURL is built internally from the config. Retry is left to the
// official client's default, and the logger is discarded because the
// controller only uses fixed-field logs.
func newOfficialClient(cfg *config.Config, version, commit string) (*scalesetapi.Client, error) {
	systemInfo := scalesetapi.SystemInfo{
		System:    systemName,
		Version:   version,
		CommitSHA: commit,
		Subsystem: "controller",
	}
	// The official client's default logger is also discard, but we explicitly
	// discard ours so the contract of keeping secrets out of logs does not
	// depend on upstream default changes.
	options := []scalesetapi.HTTPOption{
		scalesetapi.WithLogger(slog.New(slog.DiscardHandler)),
	}
	if cfg.GitHub.App != nil {
		app := scalesetapi.GitHubAppAuth{
			// Convert the positive YAML appId to a decimal string.
			ClientID:       strconv.FormatInt(cfg.GitHub.App.AppID, 10),
			InstallationID: cfg.GitHub.App.InstallationID,
			PrivateKey:     string(cfg.GitHub.App.PrivateKey),
		}
		if err := app.Validate(); err != nil {
			// Do not include the secret value (private key).
			return nil, fmt.Errorf("github app auth: %w", err)
		}
		return scalesetapi.NewClientWithGitHubApp(scalesetapi.ClientWithGitHubAppConfig{
			GitHubConfigURL: cfg.GitHubConfigURL(),
			GitHubAppAuth:   app,
			SystemInfo:      systemInfo,
		}, options...)
	}
	if cfg.GitHub.Token == "" {
		// App and GITHUB_TOKEN exclusivity is already guaranteed by config
		// validation, but reject defensively. The secret value itself is not
		// included in the error.
		return nil, fmt.Errorf("github auth is not configured: github.app or GITHUB_TOKEN is required")
	}
	return scalesetapi.NewClientWithPersonalAccessToken(scalesetapi.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL:     cfg.GitHubConfigURL(),
		PersonalAccessToken: cfg.GitHub.Token,
		SystemInfo:          systemInfo,
	}, options...)
}

// protocolErrorf creates a protocol-fatal error. op is the operation name
// and format is the failure description. Never pass secrets such as PAT or
// private keys; they must not appear in error strings.
func protocolErrorf(op, format string, args ...any) error {
	return fmt.Errorf("%s: protocol error: %s", op, fmt.Sprintf(format, args...))
}
