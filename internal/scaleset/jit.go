// This file wraps JIT configuration generation. The encoded value is opaque
// and must not appear in logs or errors.
package scaleset

import (
	"context"
	"fmt"

	scalesetapi "github.com/actions/scaleset"

	"github.com/nukanoto/gha-docker-controller/internal/model"
)

const jitWorkFolder = "/home/runner/_work"

// JitConfig carries the validated JIT response while redacting Encoded in String.
type JitConfig struct {
	RunnerID   int64
	RunnerName string
	ScaleSetID int64
	Encoded    string
}

// String returns a representation without the encoded JIT value.
func (j JitConfig) String() string {
	return fmt.Sprintf("JitConfig{RunnerID:%d RunnerName:%q ScaleSetID:%d Encoded:<redacted>}",
		j.RunnerID, j.RunnerName, j.ScaleSetID)
}

// GenerateJitRunnerConfig validates the request and official response before
// returning the opaque encoded value.
func (c *Client) GenerateJitRunnerConfig(ctx context.Context, runnerName string, scaleSetID int) (*JitConfig, error) {
	if err := validateJitInput(runnerName, scaleSetID); err != nil {
		return nil, err
	}
	raw, err := c.official.GenerateJitRunnerConfig(ctx, &scalesetapi.RunnerScaleSetJitRunnerSetting{
		Name:       runnerName,
		WorkFolder: jitWorkFolder,
	}, scaleSetID)
	if err != nil {
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

// validateJitInput checks the request before official I/O.
func validateJitInput(runnerName string, scaleSetID int) error {
	if scaleSetID <= 0 {
		return protocolErrorf("validate JIT input", "scale set ID %d is not positive", scaleSetID)
	}
	if !model.ValidRunnerName(runnerName) {
		return protocolErrorf("validate JIT input", "runner name %q is not a valid canonical runner name", runnerName)
	}
	return nil
}

// validateJitConfig checks the exact fields returned by GitHub.
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
