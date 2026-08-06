// jit.go implements a concrete wrapper for JIT runner config generation.
// The encoded JIT config is an opaque secret: it is never decoded, signed,
// re-encoded, or reused, and never appears in errors, logs, or stringers.
// The runner layer builds the delivery method
// (ACTIONS_RUNNER_INPUT_JITCONFIG environment variable). The runner name is
// `<sanitized-scale-set>-<12 hex UUID>`; callers generate it with
// model.RunnerName and pass it in. This wrapper uses exact validation to
// guarantee the GitHub response matches the requested values.
package scaleset

import (
	"context"
	"fmt"

	scalesetapi "github.com/actions/scaleset"

	"github.com/nukanoto/gha-docker-controller/internal/model"
)

// jitWorkFolder is the work folder of the runner.
const jitWorkFolder = "/home/runner/_work"

// JitConfig is a typed DTO that carries the result of
// GenerateJitRunnerConfig. Encoded is an opaque secret, so String always
// redacts it. Returning the official RunnerScaleSetJitRunnerConfig directly
// would expose the encoded value via %v, so this DTO is used instead.
type JitConfig struct {
	// RunnerID is the runner ID issued by GitHub.
	RunnerID int64
	// RunnerName is the GitHub-side name, matching the requested runner name.
	RunnerName string
	// ScaleSetID is the ID of the Scale Set the runner belongs to.
	ScaleSetID int64
	// Encoded is the opaque encoded JIT config. It is never decoded.
	Encoded string
}

// String returns the redacted representation of JitConfig. The encoded JIT
// value must never appear in logs, errors, or health responses. The runner
// name is derived from the container name and is not a secret.
func (j JitConfig) String() string {
	return fmt.Sprintf("JitConfig{RunnerID:%d RunnerName:%q ScaleSetID:%d Encoded:<redacted>}",
		j.RunnerID, j.RunnerName, j.ScaleSetID)
}

// GenerateJitRunnerConfig generates a JIT runner config for the given Scale
// Set. Pass a canonical runner name. Before official I/O, validateJitInput
// checks the requested values and fails as protocol-fatal on invalid input
// without I/O. The official response is converted to JitConfig only after
// validateJitConfig passes exact validation. The encoded value is returned
// as-is and is never included in errors.
func (c *Client) GenerateJitRunnerConfig(ctx context.Context, runnerName string, scaleSetID int) (*JitConfig, error) {
	// Require a positive Scale Set ID and a canonical runner name before official I/O.
	if err := validateJitInput(runnerName, scaleSetID); err != nil {
		return nil, err
	}
	raw, err := c.official.GenerateJitRunnerConfig(ctx, &scalesetapi.RunnerScaleSetJitRunnerSetting{
		Name:       runnerName,
		WorkFolder: jitWorkFolder,
	}, scaleSetID)
	if err != nil {
		// The official error has no path that leaks JIT values, but keep
		// only the runner name in the error so secrets are not amplified.
		return nil, fmt.Errorf("generate JIT runner config for runner %q: %w", runnerName, err)
	}
	if err := validateJitConfig(raw, runnerName, scaleSetID); err != nil {
		return nil, err
	}
	return &JitConfig{
		RunnerID:   int64(raw.Runner.ID),
		RunnerName: raw.Runner.Name,
		ScaleSetID: int64(raw.Runner.RunnerScaleSetID),
		Encoded:    raw.EncodedJITConfig,
	}, nil
}

// validateJitInput is a pure validator that checks the requested values
// before JIT generation I/O. scaleSetID must be positive and runnerName must
// be canonical (model.ValidRunnerName). A violation fails as protocol-fatal
// without calling official I/O.
func validateJitInput(runnerName string, scaleSetID int) error {
	if scaleSetID <= 0 {
		return protocolErrorf("validate JIT input", "scale set ID %d is not positive", scaleSetID)
	}
	if !model.ValidRunnerName(runnerName) {
		return protocolErrorf("validate JIT input", "runner name %q is not a valid canonical runner name", runnerName)
	}
	return nil
}

// validateJitConfig performs exact validation purely. A nil Runner, a
// non-positive Runner.ID, a Runner.Name mismatch with the request, a
// Runner.RunnerScaleSetID mismatch with the target Scale Set ID, and an empty
// EncodedJITConfig are all protocol-fatal errors. The encoded value is never
// included in errors.
func validateJitConfig(raw *scalesetapi.RunnerScaleSetJitRunnerConfig, wantName string, wantScaleSetID int) error {
	if raw == nil {
		return protocolErrorf("validate JIT config", "response is nil")
	}
	if raw.Runner == nil {
		return protocolErrorf("validate JIT config", "runner is nil for %q", wantName)
	}
	if raw.Runner.ID <= 0 {
		return protocolErrorf("validate JIT config", "runner ID %d is not positive", raw.Runner.ID)
	}
	if raw.Runner.Name != wantName {
		return protocolErrorf("validate JIT config", "runner name mismatch: got %q, want %q", raw.Runner.Name, wantName)
	}
	if raw.Runner.RunnerScaleSetID != wantScaleSetID {
		return protocolErrorf("validate JIT config", "scale set mismatch: got %d, want %d", raw.Runner.RunnerScaleSetID, wantScaleSetID)
	}
	if raw.EncodedJITConfig == "" {
		return protocolErrorf("validate JIT config", "encoded JIT config is empty for %q", wantName)
	}
	return nil
}
