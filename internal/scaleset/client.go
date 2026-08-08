// Package scaleset wraps authentication and the official Scale Set client.
package scaleset

import (
	"fmt"
	"log/slog"
	"strconv"

	scalesetapi "github.com/actions/scaleset"

	"github.com/nukanoto/gha-docker-controller/internal/config"
)

// systemName identifies this implementation to GitHub.
const systemName = "gha-docker-controller"

// Client holds the official client and build info.
type Client struct {
	official *scalesetapi.Client
	version  string
	commit   string
}

// New creates a Client from validated config and build info.
func New(cfg *config.Config, version, commit string) (*Client, error) {
	official, err := newOfficialClient(cfg, version, commit)
	if err != nil {
		return nil, err
	}
	return &Client{official: official, version: version, commit: commit}, nil
}

// newOfficialClient configures the official client without exposing secrets
// through its logger.
func newOfficialClient(cfg *config.Config, version, commit string) (*scalesetapi.Client, error) {
	systemInfo := scalesetapi.SystemInfo{
		System:    systemName,
		Version:   version,
		CommitSHA: commit,
		Subsystem: "controller",
	}
	// Keep secret-bearing upstream logs disabled even if its default changes.
	options := []scalesetapi.HTTPOption{
		scalesetapi.WithLogger(slog.New(slog.DiscardHandler)),
	}
	if cfg.GitHub.App != nil {
		app := scalesetapi.GitHubAppAuth{
			ClientID:       strconv.FormatInt(cfg.GitHub.App.AppID, 10),
			InstallationID: cfg.GitHub.App.InstallationID,
			PrivateKey:     string(cfg.GitHub.App.PrivateKey),
		}
		if err := app.Validate(); err != nil {
			return nil, fmt.Errorf("github app auth: %w", err)
		}
		return scalesetapi.NewClientWithGitHubApp(scalesetapi.ClientWithGitHubAppConfig{
			GitHubConfigURL: cfg.GitHubConfigURL(),
			GitHubAppAuth:   app,
			SystemInfo:      systemInfo,
		}, options...)
	}
	if cfg.GitHub.Token == "" {
		return nil, fmt.Errorf("github auth is not configured: github.app or GITHUB_TOKEN is required")
	}
	return scalesetapi.NewClientWithPersonalAccessToken(scalesetapi.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL:     cfg.GitHubConfigURL(),
		PersonalAccessToken: cfg.GitHub.Token,
		SystemInfo:          systemInfo,
	}, options...)
}

// protocolErrorf creates an error for an invalid upstream response or contract.
func protocolErrorf(op, format string, args ...any) error {
	return fmt.Errorf("%s: protocol error: %s", op, fmt.Sprintf(format, args...))
}
