package config

import (
	"fmt"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/distribution/reference"
)

// validate checks values that do not require a live Docker or GitHub query.
func (c *Config) validate() []error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	errs = append(errs, validateGitHub(c)...)
	if !validName(c.ScaleSet.Name) {
		add("scaleSet.name: required, allowed characters are [A-Za-z0-9_.-]")
	}
	if !validName(c.ScaleSet.RunnerGroup) {
		add("scaleSet.runnerGroup: allowed characters are [A-Za-z0-9_.-]")
	}
	if c.ScaleSet.MinRunners < 0 {
		add("scaleSet.minRunners: must be >= 0")
	}
	if c.ScaleSet.MaxRunners < 1 || c.ScaleSet.MaxRunners > math.MaxInt32 {
		add("scaleSet.maxRunners: required, must be in range 1..%d", math.MaxInt32)
	}
	if c.ScaleSet.MinRunners > c.ScaleSet.MaxRunners {
		add("scaleSet.minRunners: must be <= scaleSet.maxRunners")
	}

	if err := validateDockerHost(c.Docker.Host); err != nil {
		add("docker.host: %v", err)
	}
	switch c.Docker.PullPolicy {
	case PullPolicyAlways, PullPolicyIfNotPresent, PullPolicyNever:
	default:
		add("docker.pullPolicy: must be one of always, if-not-present, never")
	}
	if err := validateImage(c.Runner.Image); err != nil {
		add("runner.image: %v", err)
	}

	if err := validateListen(c.Health.Listen); err != nil {
		add("health.listen: %v", err)
	}
	switch c.Shutdown.BusyPolicy {
	case ShutdownPolicyLeave, ShutdownPolicyStop:
	default:
		add("shutdown.busyRunnerPolicy: must be one of leave, stop")
	}
	switch c.Log.Format {
	case LogFormatJSON, LogFormatText:
	default:
		add("log.format: must be one of json, text")
	}
	switch c.Log.Level {
	case LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError:
	default:
		add("log.level: must be one of debug, info, warn, error")
	}
	return errs
}

func validateGitHub(c *Config) []error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}
	if err := validateGitHubURL(c.GitHub.URL); err != nil {
		add("github.url: %v", err)
	}
	switch c.GitHub.Scope {
	case ScopeOrganization, ScopeRepository:
	default:
		add("github.scope: must be %q or %q", ScopeOrganization, ScopeRepository)
	}
	if !validName(c.GitHub.Owner) {
		add("github.owner: required, allowed characters are [A-Za-z0-9_.-]")
	}
	if c.GitHub.Scope == ScopeRepository {
		if !validName(c.GitHub.Repository) {
			add("github.repository: required for repository scope, allowed characters are [A-Za-z0-9_.-]")
		}
	} else if c.GitHub.Repository != "" {
		add("github.repository: must be empty for organization scope")
	}
	if c.GitHub.App != nil {
		if c.GitHub.App.AppID <= 0 {
			add("github.app.id: positive integer required")
		}
		if c.GitHub.App.InstallationID <= 0 {
			add("github.app.installationId: positive integer required")
		}
		if c.GitHub.App.PrivateKeyFile == "" {
			add("github.app.privateKeyFile: required")
		}
	}
	return errs
}

// validateImage delegates syntax rules to Docker's reference parser. Tags and
// digests are both optional because Docker accepts floating image references.
func validateImage(image string) error {
	if image == "" {
		return fmt.Errorf("required")
	}
	ref, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return fmt.Errorf("invalid image name %q", image)
	}
	if ref.Name() != strings.ToLower(ref.Name()) {
		return fmt.Errorf("invalid image name %q", ref.Name())
	}
	return nil
}

// validateGitHubURL accepts only the public GitHub endpoint.
func validateGitHubURL(rawURL string) error {
	if rawURL == "" {
		return fmt.Errorf("required")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %v", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("scheme must be https")
	}
	if u.Hostname() != "github.com" {
		return fmt.Errorf("host must be exactly github.com (GHES and ghe.com, github.localhost are not supported)")
	}
	if u.Port() != "" {
		return fmt.Errorf("port is not allowed")
	}
	if u.User != nil {
		return fmt.Errorf("userinfo is not allowed")
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("path is not allowed")
	}
	if u.RawQuery != "" {
		return fmt.Errorf("query is not allowed")
	}
	if u.Fragment != "" {
		return fmt.Errorf("fragment is not allowed")
	}
	return nil
}

// validateDockerHost allows only an absolute unix:// socket URL.
func validateDockerHost(host string) error {
	if host == "" {
		return fmt.Errorf("required")
	}
	if !strings.HasPrefix(host, "unix://") {
		return fmt.Errorf("only unix:// with an absolute path is allowed (tcp:// and ssh:// are rejected)")
	}
	path := strings.TrimPrefix(host, "unix://")
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("unix socket path must be absolute: %q", path)
	}
	if path == "/" || strings.HasSuffix(path, "/") {
		return fmt.Errorf("unix socket path must not be or end with '/'")
	}
	return nil
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-') {
			return false
		}
	}
	return true
}

func validateListen(addr string) error {
	if addr == "" {
		return fmt.Errorf("required")
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("must be host:port: %v", err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("invalid port %q", port)
	}
	return nil
}

func joinErrors(errs []error) string {
	var b strings.Builder
	for i, err := range errs {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "- %v", err)
	}
	return b.String()
}
