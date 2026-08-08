package docker

import (
	"context"
	"fmt"

	cerrdefs "github.com/containerd/errdefs"
	mobyclient "github.com/moby/moby/client"

	"github.com/nukanoto/arc-docker/internal/config"
)

// ImageInspect inspects an image in the local store.
func (c *Client) ImageInspect(ctx context.Context, ref string) (mobyclient.ImageInspectResult, error) {
	return c.c.ImageInspect(ctx, ref)
}

// PullImage pulls an image and waits for the daemon's response stream.
func (c *Client) PullImage(ctx context.Context, ref string) error {
	resp, err := c.c.ImagePull(ctx, ref, mobyclient.ImagePullOptions{})
	if err != nil {
		return err
	}
	// Wait normally closes the stream; the defer also covers early errors.
	defer resp.Close()
	return resp.Wait(ctx)
}

// EnsureImage applies the configured pull policy to the local image store.
func (c *Client) EnsureImage(ctx context.Context, ref, policy string) error {
	switch policy {
	case config.PullPolicyAlways:
		return c.PullImage(ctx, ref)
	case config.PullPolicyIfNotPresent:
		if _, err := c.ImageInspect(ctx, ref); err == nil {
			return nil
		} else if !cerrdefs.IsNotFound(err) {
			return err
		}
		return c.PullImage(ctx, ref)
	case config.PullPolicyNever:
		if _, err := c.ImageInspect(ctx, ref); err == nil {
			return nil
		} else if !cerrdefs.IsNotFound(err) {
			return err
		}
		return fmt.Errorf("image %s is not present locally and pull policy is %s", ref, policy)
	default:
		return fmt.Errorf("unknown pull policy %q", policy)
	}
}
