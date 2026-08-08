// This file implements Scale Set lookup, creation, and validation.
package scaleset

import (
	"context"
	"errors"
	"fmt"

	scalesetapi "github.com/actions/scaleset"
)

// CheckResult is the result of a read-only Scale Set check.
type CheckResult struct {
	Group    *scalesetapi.RunnerGroup
	ScaleSet *scalesetapi.RunnerScaleSet
	Warning  string
}

// EnsureScaleSet looks up or creates the configured Scale Set and validates
// the returned contract. It never deletes the Scale Set.
func (c *Client) EnsureScaleSet(ctx context.Context, groupName, scaleSetName string) (*scalesetapi.RunnerScaleSet, error) {
	group, err := c.official.GetRunnerGroupByName(ctx, groupName)
	if err != nil {
		return nil, fmt.Errorf("get runner group %q: %w", groupName, err)
	}
	if err := validateRunnerGroup(group, groupName); err != nil {
		return nil, err
	}
	ss, err := c.official.GetRunnerScaleSet(ctx, group.ID, scaleSetName)
	if err != nil {
		return nil, fmt.Errorf("get runner scale set %q: %w", scaleSetName, err)
	}
	if ss == nil {
		created, err := c.createRunnerScaleSet(ctx, group.ID, scaleSetName)
		if err != nil {
			return nil, err
		}
		ss = created
	}
	if err := validateScaleSet(ss, group.ID, scaleSetName); err != nil {
		return nil, err
	}
	c.setSystemInfo(ss.ID)
	return ss, nil
}

// createRunnerScaleSet creates once and re-fetches after a create conflict.
func (c *Client) createRunnerScaleSet(ctx context.Context, groupID int, name string) (*scalesetapi.RunnerScaleSet, error) {
	ss := &scalesetapi.RunnerScaleSet{
		Name:          name,
		RunnerGroupID: groupID,
		Labels:        []scalesetapi.Label{{Type: "System", Name: name}},
		RunnerSetting: scalesetapi.RunnerSetting{DisableUpdate: true},
	}
	created, err := c.official.CreateRunnerScaleSet(ctx, ss)
	if err == nil {
		if created == nil {
			return nil, protocolErrorf("create runner scale set", "nil response for %q", name)
		}
		return created, nil
	}
	if !errors.Is(err, scalesetapi.RunnerExistsError) {
		return nil, fmt.Errorf("create runner scale set %q: %w", name, err)
	}
	reget, rerr := c.official.GetRunnerScaleSet(ctx, groupID, name)
	if rerr != nil {
		return nil, fmt.Errorf("reget runner scale set %q after exists error: %w", name, rerr)
	}
	if reget == nil {
		return nil, protocolErrorf("reget runner scale set", "%q reports exists but was not found on reget", name)
	}
	return reget, nil
}

// validateRunnerGroup checks the lookup response before it is dereferenced.
func validateRunnerGroup(group *scalesetapi.RunnerGroup, wantName string) error {
	if wantName == "" {
		return protocolErrorf("validate runner group", "requested group name is empty")
	}
	if group == nil {
		return protocolErrorf("validate runner group", "response for %q is nil", wantName)
	}
	if group.ID <= 0 {
		return protocolErrorf("validate runner group", "group %q has invalid ID %d", wantName, group.ID)
	}
	if group.Name != wantName {
		return protocolErrorf("validate runner group", "name mismatch: got %q, want %q", group.Name, wantName)
	}
	return nil
}

// validateScaleSet checks the exact Scale Set contract without auto-updating.
func validateScaleSet(ss *scalesetapi.RunnerScaleSet, groupID int, name string) error {
	if ss == nil {
		return protocolErrorf("validate scale set", "response for %q is nil", name)
	}
	if ss.ID <= 0 {
		return protocolErrorf("validate scale set", "runner scale set %q has invalid ID %d", name, ss.ID)
	}
	if ss.Name != name {
		return protocolErrorf("validate scale set", "runner scale set ID %d name mismatch: got %q, want %q", ss.ID, ss.Name, name)
	}
	if ss.RunnerGroupID != groupID {
		return protocolErrorf("validate scale set", "runner scale set %q group mismatch: got %d, want %d", name, ss.RunnerGroupID, groupID)
	}
	if len(ss.Labels) != 1 || ss.Labels[0].Type != "system" || ss.Labels[0].Name != name {
		return protocolErrorf("validate scale set", "runner scale set %q must have exactly one system label %q (got %v)", name, name, ss.Labels)
	}
	if !ss.RunnerSetting.DisableUpdate {
		return protocolErrorf("validate scale set", "runner scale set %q must have DisableUpdate=true", name)
	}
	return nil
}

// setSystemInfo adds build and Scale Set identity to official requests.
func (c *Client) setSystemInfo(scaleSetID int) {
	c.official.SetSystemInfo(scalesetapi.SystemInfo{
		System:     systemName,
		Version:    c.version,
		CommitSHA:  c.commit,
		ScaleSetID: scaleSetID,
		Subsystem:  "controller",
	})
}

// CheckScaleSet verifies the existing Scale Set without creating resources.
func (c *Client) CheckScaleSet(ctx context.Context, groupName, scaleSetName string) (*CheckResult, error) {
	group, err := c.official.GetRunnerGroupByName(ctx, groupName)
	if err != nil {
		return nil, fmt.Errorf("get runner group %q: %w", groupName, err)
	}
	if err := validateRunnerGroup(group, groupName); err != nil {
		return nil, err
	}
	ss, err := c.official.GetRunnerScaleSet(ctx, group.ID, scaleSetName)
	if err != nil {
		return nil, fmt.Errorf("get runner scale set %q: %w", scaleSetName, err)
	}
	return checkScaleSetResult(group, ss, scaleSetName)
}

// checkScaleSetResult validates an existing set or returns a missing-set warning.
func checkScaleSetResult(group *scalesetapi.RunnerGroup, ss *scalesetapi.RunnerScaleSet, scaleSetName string) (*CheckResult, error) {
	result := &CheckResult{Group: group, ScaleSet: ss}
	if ss == nil {
		result.Warning = fmt.Sprintf("runner scale set %q does not exist; creation permission cannot be verified with read-only checks", scaleSetName)
		return result, nil
	}
	if err := validateScaleSet(ss, group.ID, scaleSetName); err != nil {
		return nil, err
	}
	return result, nil
}
