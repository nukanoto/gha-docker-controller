// Package docker wraps the official Moby client and managed-container guards.
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

// DefaultTimeout is the HTTP safety timeout used when New receives no timeout.
// It covers image pulls; shorter operations use context deadlines.
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

// New creates a client for an absolute unix:// Docker socket.
// Environment overrides are intentionally ignored.
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

// validateHost accepts only an absolute unix:// socket URL.
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

// Host returns the configured Docker host.
func (c *Client) Host() string {
	return c.host
}

// Close closes the client's idle connections.
func (c *Client) Close() error {
	return c.c.Close()
}

// Ping checks connectivity and negotiates the API version.
func (c *Client) Ping(ctx context.Context) (mobyclient.PingResult, error) {
	return c.c.Ping(ctx, mobyclient.PingOptions{NegotiateAPIVersion: true})
}

// ClientVersion returns the negotiated API version.
func (c *Client) ClientVersion() string {
	return c.c.ClientVersion()
}

// ServerVersion returns the Docker server version.
func (c *Client) ServerVersion(ctx context.Context) (mobyclient.ServerVersionResult, error) {
	return c.c.ServerVersion(ctx, mobyclient.ServerVersionOptions{})
}

// Info returns Docker daemon information.
func (c *Client) Info(ctx context.Context) (mobyclient.SystemInfoResult, error) {
	return c.c.Info(ctx, mobyclient.InfoOptions{})
}

// ValidateVersions checks the minimum API and engine versions.
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

// RuntimeCheck reports whether a runtime is registered and selected by default.
type RuntimeCheck struct {
	Present   bool
	IsDefault bool
}

// CheckRuntime verifies that a named runtime is registered.
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

// CreateManaged validates and creates one immutable managed spec.
// Callers must recover by labels after a timeout because Docker may create the
// container before returning an error.
func (c *Client) CreateManaged(ctx context.Context, spec ManagedSpec) (mobyclient.ContainerCreateResult, error) {
	if err := validateManagedSpec(spec); err != nil {
		return mobyclient.ContainerCreateResult{}, err
	}
	return c.c.ContainerCreate(ctx, spec.create)
}

// containerStart is the unguarded SDK call used by StartManaged.
func (c *Client) containerStart(ctx context.Context, id string, options mobyclient.ContainerStartOptions) (mobyclient.ContainerStartResult, error) {
	return c.c.ContainerStart(ctx, id, options)
}

// ContainerInspect inspects a container without changing it.
func (c *Client) ContainerInspect(ctx context.Context, id string, options mobyclient.ContainerInspectOptions) (mobyclient.ContainerInspectResult, error) {
	return c.c.ContainerInspect(ctx, id, options)
}

// ContainerList lists containers with caller-provided filters.
func (c *Client) ContainerList(ctx context.Context, options mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error) {
	return c.c.ContainerList(ctx, options)
}

// containerStop is the unguarded SDK call used by StopManaged.
func (c *Client) containerStop(ctx context.Context, id string, options mobyclient.ContainerStopOptions) (mobyclient.ContainerStopResult, error) {
	return c.c.ContainerStop(ctx, id, options)
}

// containerRemove is the unguarded SDK call used by CleanupManaged.
func (c *Client) containerRemove(ctx context.Context, id string, options mobyclient.ContainerRemoveOptions) (mobyclient.ContainerRemoveResult, error) {
	return c.c.ContainerRemove(ctx, id, options)
}

// WaitResult contains the Docker wait result.
type WaitResult struct {
	StatusCode int64
	Error      *container.WaitExitError
}

// WaitContainer waits for exit and drains the SDK result after cancellation
// so the Moby wait goroutine cannot block forever.
func (c *Client) WaitContainer(ctx context.Context, id string, options mobyclient.ContainerWaitOptions) (WaitResult, error) {
	wait := c.c.ContainerWait(ctx, id, options)
	select {
	case <-ctx.Done():
		// Result is unbuffered, so drain whichever channel the SDK completes.
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
