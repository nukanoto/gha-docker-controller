package config

import (
	// go-digest Algorithm.Available() decides by whether the hash
	// implementation is linked in, so sha256/sha512 are imported explicitly
	// to keep them always available.
	_ "crypto/sha256"
	_ "crypto/sha512"
	"fmt"
	"math"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
)

// validate returns errors with a field path only for invalid values that can
// be decided statically. Dynamic checks such as Docker connectivity, image
// existence and runtime registration are the responsibility of the check
// command.
func (c *Config) validate() []error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	// GitHub
	errs = append(errs, validateGitHub(c)...)

	// Scale Set
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

	// Docker
	if err := validateDockerHost(c.Docker.Host); err != nil {
		add("docker.host: %v", err)
	}
	if !validName(c.Docker.Runtime) {
		add("docker.runtime: required, allowed characters are [A-Za-z0-9_.-]")
	}
	// standard allows the configured runtime as-is, but nested-docker is
	// fixed to runsc. The nested contract does not hold with any runtime
	// other than runsc sandboxing the inner dockerd.
	if c.Runner.Profile == ProfileNestedDocker && c.Docker.Runtime != DefaultRuntime {
		add("docker.runtime: nested-docker profile requires runtime %q", DefaultRuntime)
	}
	if err := validateNetwork(c.Docker.Network); err != nil {
		add("docker.network: %v", err)
	}
	switch c.Docker.PullPolicy {
	case PullPolicyAlways, PullPolicyIfNotPresent, PullPolicyNever:
	default:
		add("docker.pullPolicy: must be one of always, if-not-present, never")
	}

	// Runner image / profile
	errs = append(errs, validateRunnerProfile(c)...)
	// Resources are mandatory
	if c.Runner.CPU <= 0 {
		add("runner.cpu: required, must be > 0")
	}
	if c.Runner.Memory <= 0 {
		add("runner.memory: required, must be > 0")
	}
	if c.Runner.MemorySwap <= 0 {
		add("runner.memorySwap: required, must be > 0")
	}
	if c.Runner.MemorySwap < c.Runner.Memory {
		add("runner.memorySwap: must be >= runner.memory")
	}
	if c.Runner.PidsLimit <= 0 {
		add("runner.pidsLimit: required, must be > 0")
	}
	errs = append(errs, validateRunnerSecurity(c)...)
	errs = append(errs, validateRunnerCapabilities(c)...)
	errs = append(errs, validateRunnerIsolation(c)...)

	// network must match docker.network
	if err := validateNetwork(c.Runner.Network); err != nil {
		add("runner.network: %v", err)
	}
	if c.Runner.Network != c.Docker.Network {
		add("runner.network: must match docker.network when both are set")
	}
	for i, ip := range c.Runner.DNS {
		if net.ParseIP(ip) == nil {
			add("runner.dns[%d]: invalid IP address %q", i, ip)
		}
	}
	for i, h := range c.Runner.ExtraHosts {
		if err := validateExtraHost(h); err != nil {
			add("runner.extraHosts[%d]: %v", i, err)
		}
	}
	for i, spec := range c.Runner.Tmpfs {
		if err := validateTmpfs(spec); err != nil {
			add("runner.tmpfs[%d]: %v", i, err)
		}
	}

	// NestedDocker
	errs = append(errs, validateNestedDocker(c)...)

	// Health / Shutdown / Log
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

// validateGitHub validates the GitHub endpoint, scope, owner, repository and
// required App fields. It rejects invalid scope enum values, missing or
// mutually exclusive owner/repository, and App IDs that are not positive
// integers or missing private keys, with a field path in each error.
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
	// repository must be empty for the organization scope and mandatory for
	// the repository scope.
	if c.GitHub.Scope == ScopeRepository {
		if !validName(c.GitHub.Repository) {
			add("github.repository: required for repository scope, allowed characters are [A-Za-z0-9_.-]")
		}
	} else if c.GitHub.Repository != "" {
		add("github.repository: must be empty for organization scope")
	}
	// Required App fields
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

// validateRunnerProfile validates the combination of runner image and
// profile. The image reference form follows validateImage rules (digest or
// version tag; digest only for nested-docker), and the profile must be one of
// standard and nested-docker.
func validateRunnerProfile(c *Config) []error {
	var errs []error
	// Runner image
	if err := validateImage(c.Runner.Image, c.Runner.Profile); err != nil {
		errs = append(errs, fmt.Errorf("runner.image: %v", err))
	}
	switch c.Runner.Profile {
	case ProfileStandard, ProfileNestedDocker:
	default:
		errs = append(errs, fmt.Errorf("runner.profile: must be one of standard, nested-docker"))
	}
	return errs
}

// validateRunnerSecurity validates the runner rootfs, privileges and
// capability drops. Based on the official runner's operating requirements and
// the least-privilege security contract, it requires read-only rootfs to be
// off, no-new-privileges to be on and CapDrop to be exactly ALL.
func validateRunnerSecurity(c *Config) []error {
	var errs []error
	// The official runner writes config and helpers to /home/runner, so a
	// read-only rootfs cannot be used.
	if c.Runner.ReadOnlyRootfs {
		errs = append(errs, fmt.Errorf("runner.readOnlyRootfs: must be false (the official runner writes to /home/runner)"))
	}
	// no-new-privileges is always enabled.
	if !c.Runner.NoNewPrivileges {
		errs = append(errs, fmt.Errorf("runner.noNewPrivileges: must be true"))
	}
	// Only CapDrop ALL is allowed.
	if len(c.Runner.CapDrop) != 1 || c.Runner.CapDrop[0] != "ALL" {
		errs = append(errs, fmt.Errorf("runner.capDrop: only [\"ALL\"] is allowed"))
	}
	return errs
}

// validateRunnerCapabilities validates the CapAdd constraints per profile.
// standard allows no additional capabilities at all; nested-docker allows
// only a subset of the 17 nestedCapAdd capabilities.
func validateRunnerCapabilities(c *Config) []error {
	var errs []error
	// Apply the per-profile CapAdd constraints.
	switch c.Runner.Profile {
	case ProfileStandard:
		if len(c.Runner.CapAdd) > 0 {
			errs = append(errs, fmt.Errorf("runner.capAdd: must be empty for standard profile"))
		}
	case ProfileNestedDocker:
		for _, cap := range c.Runner.CapAdd {
			if !slices.Contains(nestedCapAdd, cap) {
				errs = append(errs, fmt.Errorf("runner.capAdd: %q is not in the nested-docker allowed set", cap))
			}
		}
	}
	return errs
}

// validateRunnerIsolation validates the runner seccomp and AppArmor
// settings. "unconfined" disables the sandbox and is rejected for every
// profile.
func validateRunnerIsolation(c *Config) []error {
	var errs []error
	// unconfined is rejected for both profiles.
	if c.Runner.Seccomp == "unconfined" {
		errs = append(errs, fmt.Errorf("runner.seccomp: \"unconfined\" is not allowed"))
	}
	if c.Runner.AppArmor == "unconfined" {
		errs = append(errs, fmt.Errorf("runner.apparmor: \"unconfined\" is not allowed"))
	}
	return errs
}

// validateNestedDocker validates the nested-docker profile storage. The inner
// dockerd storage is fixed to tmpfs. The host daemon runsc runtimeArgs cannot
// be verified through the Docker API, so that is left to the check warning
// and a manual operator check (see the README procedure).
func validateNestedDocker(c *Config) []error {
	var errs []error
	// Only tmpfs storage is allowed. resolve already applied the tmpfs
	// default, so any other value is a misconfiguration.
	if c.NestedDocker.Storage != DefaultNestedStorage {
		errs = append(errs, fmt.Errorf("nestedDocker.storage: only %q is supported", DefaultNestedStorage))
	}
	if c.NestedDocker.StorageSize <= 0 {
		errs = append(errs, fmt.Errorf("nestedDocker.storageSize: must be positive"))
	}
	return errs
}

// validateGitHubURL verifies that the normalized github.url is exactly
// "https://github.com". port, query, fragment, path and userinfo are
// rejected, which excludes GHES, ghe.com and github.localhost.
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

// validateDockerHost allows only an absolute-path unix://. tcp:// and ssh://
// are rejected, and the DOCKER_HOST environment variable override is not used
// when building the client. Connecting to a remote daemon could bypass host
// settings and carry secrets, so it is not allowed.
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

// validateNetwork rejects host network, none and container mode. Only an
// existing bridge or user-defined network is allowed. container:<id-or-name>
// shares another container's network namespace, which breaks isolation like
// host mode and is rejected by the security contract that forbids sharing the
// runner's host namespace. The nested-docker inner network is operated within
// the same constraint.
func validateNetwork(network string) error {
	if network == "" {
		return fmt.Errorf("required")
	}
	switch network {
	case "host":
		return fmt.Errorf("host network is not allowed")
	case "none":
		return fmt.Errorf("network mode %q is not allowed", network)
	}
	// container mode rejects both bare "container" and "container:<id-or-name>".
	if network == "container" || strings.HasPrefix(network, "container:") {
		return fmt.Errorf("network mode %q is not allowed", network)
	}
	return nil
}

// validateImage rejects latest and references without tag/digest. standard
// allows a digest or a version tag; nested-docker allows only digest
// references. Floating references such as latest make configurations
// unreproducible and are rejected for every profile. Syntax validation is
// delegated to distribution/reference ParseNormalizedNamed, and only
// sha256/sha512 digests are explicitly allowed.
func validateImage(image, profile string) error {
	if image == "" {
		return fmt.Errorf("required")
	}
	// The reference digest syntax requires at least 32 hex characters but
	// does not restrict the algorithm (sha384 and so on also pass). digest
	// length and character set are strictly validated per algorithm by
	// digest.Parse (64 for sha256, 128 for sha512), so only the algorithm
	// allowlist is applied here first.
	if _, d, ok := strings.Cut(image, "@"); ok {
		dgst, err := digest.Parse(d)
		if err != nil || (dgst.Algorithm() != "sha256" && dgst.Algorithm() != "sha512") {
			return fmt.Errorf("invalid digest %q (sha256 or sha512 required)", d)
		}
	}
	ref, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		// The digest was already validated above, so a failure here is a bad
		// name.
		return fmt.Errorf("invalid image name %q", image)
	}
	// ParseNormalizedNamed turns "latest@digest" into
	// "docker.io/library/latest@digest", which looks like a tagless digest
	// reference. The case where the part before "@" is "latest" is therefore
	// explicitly rejected even with a digest, for consistency.
	if name, _, _ := strings.Cut(image, "@"); name == "latest" {
		return fmt.Errorf("tag %q is not allowed", "latest")
	}
	if tagged, ok := ref.(reference.Tagged); ok && tagged.Tag() == "latest" {
		return fmt.Errorf("tag %q is not allowed", "latest")
	}
	if _, ok := ref.(reference.Digested); !ok {
		if profile == ProfileNestedDocker {
			return fmt.Errorf("nested-docker profile requires a digest reference")
		}
		// A tag makes a digest unnecessary. Only references with neither tag
		// nor digest are rejected.
		if _, ok := ref.(reference.Tagged); !ok {
			return fmt.Errorf("tag or digest is required (latest is not allowed)")
		}
	}
	// ParseNormalizedNamed allows uppercase domains (for example GHCR.IO/...),
	// so lowercase for the whole repository is explicitly required.
	if ref.Name() != strings.ToLower(ref.Name()) {
		return fmt.Errorf("invalid image name %q", ref.Name())
	}
	return nil
}

// validName validates the character set [A-Za-z0-9_.-]+ of identifiers
// embedded directly into queries (owner, repository, Scale Set name, runner
// group). Empty strings return false. No other characters are allowed so the
// official client queries can be embedded safely.
func validName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}

// validateExtraHost validates "host:ip" or "host:host-gateway" form.
func validateExtraHost(spec string) error {
	host, ip, ok := strings.Cut(spec, ":")
	if !ok || host == "" {
		return fmt.Errorf("expected host:ip (or host:host-gateway)")
	}
	for _, r := range host {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '.' || r == '_'
		if !ok {
			return fmt.Errorf("invalid hostname %q", host)
		}
	}
	if ip != "host-gateway" && net.ParseIP(ip) == nil {
		return fmt.Errorf("invalid IP address %q", ip)
	}
	return nil
}

// validateTmpfs validates a Docker CLI compatible tmpfs specification
// (dest[:size[:options]]). The size appears as a standalone value or as a
// size= option inside options, and unlimited values are rejected.
func validateTmpfs(spec string) error {
	if spec == "" {
		return fmt.Errorf("must not be empty")
	}
	parts := strings.Split(spec, ":")
	if len(parts) > 3 {
		return fmt.Errorf("invalid tmpfs spec %q", spec)
	}
	dest := parts[0]
	if dest == "" || !strings.HasPrefix(dest, "/") {
		return fmt.Errorf("tmpfs destination must be an absolute path: %q", dest)
	}
	// Decide whether the second part is a size value or options with the
	// same rule as the Docker CLI.
	var opts string
	if len(parts) >= 2 && parts[1] != "" {
		if _, err := parseMemory(parts[1]); err == nil && !strings.Contains(parts[1], ",") {
			if len(parts) == 3 {
				opts = parts[2]
			}
		} else if len(parts) == 2 {
			opts = parts[1]
		} else {
			return fmt.Errorf("invalid tmpfs spec %q: size must come before options", spec)
		}
	}
	for _, opt := range strings.Split(opts, ",") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(opt), "size="); ok {
			if _, err := parseMemory(v); err != nil {
				return fmt.Errorf("invalid tmpfs size option %q", opt)
			}
		}
	}
	return nil
}

// validateListen validates host:port form and the port range.
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

// joinErrors formats validation errors as a newline-separated list.
func joinErrors(errs []error) string {
	var b strings.Builder
	for i, e := range errs {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "- %v", e)
	}
	return b.String()
}
