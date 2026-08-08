// Package docker provides the concrete client for the Docker daemon.
// It never uses the Docker CLI; it uses only the official Go SDK from the
// Moby v29 split modules (github.com/moby/moby/client, github.com/moby/moby/api).
//
// Errors from the Moby client are propagated as-is. A 404 maps to the
// containerd errdefs ErrNotFound sentinel, so callers can test it with
// cerrdefs.IsNotFound. This package does not retry; retry policy belongs to
// the caller (for example, systemd restart).
package docker

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/versions"
)

// DefaultTimeout is the HTTP client safety net used when New's timeout
// argument is 0 or less. http.Client.Timeout also covers the whole stream
// (until the image pull completes), so callers must pass a timeout that
// covers the longest pull. Shorter waits are controlled by ctx deadlines.
const DefaultTimeout = 2 * time.Minute

// MinAPIVersion is the minimum API version after negotiation.
const MinAPIVersion = "1.42"

// MinEngineVersion is the minimum host Docker Engine version.
const MinEngineVersion = "28.0"

// Client is the concrete client for the Docker daemon.
type Client struct {
	c    *mobyclient.Client
	host string
}

// New builds a concrete client for a unix socket Docker daemon.
// Only an absolute unix:// host is allowed. timeout is the HTTP client
// safety net; 0 or less falls back to DefaultTimeout. The API version is not
// pinned; the default negotiation on the first Ping is used. FromEnv,
// WithAPIVersion and the deprecated WithAPIVersionNegotiation are not used,
// and environment variables such as DOCKER_HOST are never read.
func New(host string, timeout time.Duration) (*Client, error) {
	if err := validateHost(host); err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	c, err := mobyclient.New(
		mobyclient.WithHost(host),
		mobyclient.WithTimeout(timeout),
	)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return &Client{c: c, host: host}, nil
}

// validateHost validates the Docker host contract.
// Only an absolute unix:// is allowed; tcp://, ssh://, npipe:// and relative
// paths are rejected. Environment overrides are excluded by construction
// because this package never reads them.
func validateHost(host string) error {
	const prefix = "unix://"
	if !strings.HasPrefix(host, prefix) {
		return fmt.Errorf("docker host must be an absolute unix socket URL (unix:///...), got %q; tcp://, ssh:// and environment overrides are rejected", host)
	}
	socketPath := strings.TrimPrefix(host, prefix)
	if socketPath == "" || !filepath.IsAbs(socketPath) {
		return fmt.Errorf("docker host %q must point to an absolute socket path", host)
	}
	return nil
}

// Host returns the host URL of the connection.
func (c *Client) Host() string {
	return c.host
}

// Close closes the idle connections held by the client. Call it at process
// exit. Later calls will fail to connect.
func (c *Client) Close() error {
	return c.c.Close()
}

// Ping runs /_ping and performs the API version negotiation.
// It returns an error when the negotiation fails.
func (c *Client) Ping(ctx context.Context) (mobyclient.PingResult, error) {
	return c.c.Ping(ctx, mobyclient.PingOptions{NegotiateAPIVersion: true})
}

// ClientVersion returns the negotiated API version. It returns an empty
// string before a successful Ping.
func (c *Client) ClientVersion() string {
	return c.c.ClientVersion()
}

// ServerVersion returns the result of /version.
func (c *Client) ServerVersion(ctx context.Context) (mobyclient.ServerVersionResult, error) {
	return c.c.ServerVersion(ctx, mobyclient.ServerVersionOptions{})
}

// Info returns the result of /info.
func (c *Client) Info(ctx context.Context) (mobyclient.SystemInfoResult, error) {
	return c.c.Info(ctx, mobyclient.InfoOptions{})
}

// ValidateVersions checks the minimum version requirements.
// It verifies that the negotiated API version is 1.42 or newer and the
// engine version is 28.0 or newer, and returns the values it used.
func (c *Client) ValidateVersions(ctx context.Context) (negotiatedAPI, engineVersion string, err error) {
	if _, err := c.Ping(ctx); err != nil {
		return "", "", fmt.Errorf("docker ping: %w", err)
	}
	negotiatedAPI = c.ClientVersion()
	if versions.LessThan(negotiatedAPI, MinAPIVersion) {
		return negotiatedAPI, "", fmt.Errorf("docker API version %s is below the minimum required version %s", negotiatedAPI, MinAPIVersion)
	}
	sv, err := c.ServerVersion(ctx)
	if err != nil {
		return negotiatedAPI, "", fmt.Errorf("docker server version: %w", err)
	}
	engineVersion = sv.Version
	if versions.LessThan(engineVersion, MinEngineVersion) {
		return negotiatedAPI, engineVersion, fmt.Errorf("docker engine version %s is below the minimum required version %s", engineVersion, MinEngineVersion)
	}
	return negotiatedAPI, engineVersion, nil
}

// RuntimeCheck is the result of a runtime check.
type RuntimeCheck struct {
	// Present reports whether the runtime is registered on the daemon.
	Present bool
	// IsDefault reports whether it is the daemon default runtime.
	IsDefault bool
}

// CheckRuntime verifies that a runtime name is registered in Info.Runtimes.
// It returns an error when the runtime is not registered.
func (c *Client) CheckRuntime(ctx context.Context, name string) (RuntimeCheck, error) {
	info, err := c.Info(ctx)
	if err != nil {
		return RuntimeCheck{}, err
	}
	_, ok := info.Info.Runtimes[name]
	if !ok {
		return RuntimeCheck{Present: false}, fmt.Errorf("runtime %q is not registered on the docker daemon", name)
	}
	return RuntimeCheck{
		Present:   true,
		IsDefault: info.Info.DefaultRuntime == name,
	}, nil
}

// CreateManaged accepts only an immutable ManagedSpec and creates a
// container. Before any I/O it re-validates the spec with
// validateManagedSpec, rejecting zero, tampered and incomplete specs. This
// is the only place that calls the Docker SDK ContainerCreate. Even when
// the create response is an error or a timeout, the daemon may still have
// created the container, so the caller (lifecycle) re-enumerates by the
// runner-id label to recover it. The name given at create time is auxiliary
// information, not the identity source of truth; the labels and the GitHub
// runner ID are the source of truth.
func (c *Client) CreateManaged(ctx context.Context, spec ManagedSpec) (mobyclient.ContainerCreateResult, error) {
	if err := validateManagedSpec(spec); err != nil {
		return mobyclient.ContainerCreateResult{}, err
	}
	return c.c.ContainerCreate(ctx, spec.create)
}

// containerStart starts a container. This raw operation is unexported, so
// no path can start a container without a fresh managed label check
// (StartManaged in managed.go is the only start entry point). A 404 is a
// state observation meaning "the target does not exist" and can be tested
// with cerrdefs.IsNotFound.
func (c *Client) containerStart(ctx context.Context, id string, options mobyclient.ContainerStartOptions) (mobyclient.ContainerStartResult, error) {
	return c.c.ContainerStart(ctx, id, options)
}

// ContainerInspect inspects a container. A 404 is a state observation
// meaning "the target does not exist" and can be tested with
// cerrdefs.IsNotFound. Inspect is read-only; the guards for destructive
// operations live in VerifyManaged/StopManaged (managed.go) and
// CleanupManaged (lifecycle.go).
func (c *Client) ContainerInspect(ctx context.Context, id string, options mobyclient.ContainerInspectOptions) (mobyclient.ContainerInspectResult, error) {
	return c.c.ContainerInspect(ctx, id, options)
}

// ContainerList lists containers with arbitrary filters. Use ListManaged in
// managed.go for candidate enumeration with the managed=true and
// scale-set-id label filters.
func (c *Client) ContainerList(ctx context.Context, options mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error) {
	return c.c.ContainerList(ctx, options)
}

// containerStop stops a container. This raw operation is unexported;
// StopManaged in managed.go is the only path that stops managed containers,
// with a fresh label re-check. A 304 for an already-stopped container is not
// an error for the Moby client, so it counts as success; a 404 can be
// tested with cerrdefs.IsNotFound.
func (c *Client) containerStop(ctx context.Context, id string, options mobyclient.ContainerStopOptions) (mobyclient.ContainerStopResult, error) {
	return c.c.ContainerStop(ctx, id, options)
}

// containerRemove removes a container. This raw operation is unexported;
// CleanupManaged in lifecycle.go is the only path that removes managed
// containers. A 404 is an idempotent success and can be tested with
// cerrdefs.IsNotFound.
func (c *Client) containerRemove(ctx context.Context, id string, options mobyclient.ContainerRemoveOptions) (mobyclient.ContainerRemoveResult, error) {
	return c.c.ContainerRemove(ctx, id, options)
}

// WaitResult is the result of WaitContainer.
type WaitResult struct {
	// StatusCode is the container exit code.
	StatusCode int64
	// Error is the wait failure reason (OOM, ...), nil on normal exit.
	Error *container.WaitExitError
}

// WaitContainer waits for the container to exit and returns the exit code.
// The v29 signature ContainerWait returns two channels, Result (unbuffered)
// and Error (buffer 1); this method selects over them and combines the
// result. On context cancellation it leaves a goroutine behind that drains
// the channels, so a late send cannot block the Moby client goroutine
// forever and leak it.
func (c *Client) WaitContainer(ctx context.Context, id string, options mobyclient.ContainerWaitOptions) (WaitResult, error) {
	wait := c.c.ContainerWait(ctx, id, options)
	select {
	case <-ctx.Done():
		// The Moby client wait goroutine may send on Result or Error later.
		// The Error channel is buffered (1) but Result is unbuffered, so we
		// leave a draining goroutine behind to prevent a blocking sender
		// after cancellation. Cancelling ctx aborts the HTTP request, so
		// this normally ends on the Error path.
		go func() {
			select {
			case <-wait.Result:
			case <-wait.Error:
			}
		}()
		return WaitResult{}, ctx.Err()
	case err := <-wait.Error:
		return WaitResult{}, err
	case res := <-wait.Result:
		return WaitResult{StatusCode: res.StatusCode, Error: res.Error}, nil
	}
}
