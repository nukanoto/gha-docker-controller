package docker

import (
	"context"
	"fmt"

	cerrdefs "github.com/containerd/errdefs"
	mobyclient "github.com/moby/moby/client"

	"github.com/nukanoto/gha-docker-controller/internal/config"
)

// OCI label contract for the dind profile.
// The project-provided dind-runner derived image must set these two labels
// at build time. The operator pins the digest after build/push in the config,
// and check requires the digest pin plus both labels.
const (
	// DindProfileLabelKey is the label key for the dind-runner derived image profile.
	DindProfileLabelKey = "io.github.nukanoto.gha-docker-controller.profile"
	// DindProfileLabelValue is the fixed profile label value.
	DindProfileLabelValue = "dind-runner-v1"
	// DindInnerDockerLabelKey is the label key for the inner dockerd version.
	DindInnerDockerLabelKey = "io.github.nukanoto.gha-docker-controller.inner-docker"
	// DindInnerDockerLabelValue is the fixed inner dockerd version.
	DindInnerDockerLabelValue = "29.6.1"
)

// ImageInspect inspects an image in the local image store.
// A missing image returns a 404 error that can be tested with
// cerrdefs.IsNotFound.
func (c *Client) ImageInspect(ctx context.Context, ref string) (mobyclient.ImageInspectResult, error) {
	return c.c.ImageInspect(ctx, ref)
}

// PullImage pulls an image from a registry and waits for the stream to
// finish. The stream is always closed inside Wait, and Close is also called
// explicitly so an error path that never calls Wait cannot leak it. The
// duration is bounded by the ctx deadline. The real error rides on the JSON
// message in the stream body, which the Moby client maps to cerrdefs
// sentinels, so cerrdefs.IsNotFound and friends work. The stream body is
// never kept; only the error is carried, so no secret body is left anywhere.
func (c *Client) PullImage(ctx context.Context, ref string) error {
	resp, err := c.c.ImagePull(ctx, ref, mobyclient.ImagePullOptions{})
	if err != nil {
		return err
	}
	// stream cleanup: Wait consumes and closes the stream, and the defer
	// double-checks it (Close is idempotent).
	defer resp.Close()
	return resp.Wait(ctx)
}

// EnsureImage prepares an image in the local store according to the pull
// policy.
//   - always: pull on every call.
//   - if-not-present: pull only when ImageInspect reports NotFound.
//   - never: fail when the local inspect reports NotFound.
//
// Changing the image store by pulling is an allowed mutation; it does not
// create a runner or control-plane resource. An unknown policy is assumed to
// have been rejected by static config validation, but it still fails with an
// error to prevent misuse.
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

// ValidateImageContract checks the OCI label contract of the image for the
// profile. The dind-runner profile strictly requires the two labels of the
// dedicated derived image; standard has no label contract, so nothing is
// checked. The image must already be prepared by EnsureImage.
func (c *Client) ValidateImageContract(ctx context.Context, ref, profile string) error {
	if profile != config.ProfileDindRunner {
		return nil
	}
	img, err := c.ImageInspect(ctx, ref)
	if err != nil {
		return err
	}
	if img.Config == nil {
		return fmt.Errorf("image %s has no config; dind-runner profile requires labels %s=%s and %s=%s", ref, DindProfileLabelKey, DindProfileLabelValue, DindInnerDockerLabelKey, DindInnerDockerLabelValue)
	}
	labels := img.Config.Labels
	if labels[DindProfileLabelKey] != DindProfileLabelValue {
		return fmt.Errorf("image %s is not the project dind-runner image: label %s must be %q", ref, DindProfileLabelKey, DindProfileLabelValue)
	}
	if labels[DindInnerDockerLabelKey] != DindInnerDockerLabelValue {
		return fmt.Errorf("image %s has an unexpected inner docker version: label %s must be %q", ref, DindInnerDockerLabelKey, DindInnerDockerLabelValue)
	}
	return nil
}
