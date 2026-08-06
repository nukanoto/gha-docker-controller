// scaleset.go implements Scale Set get-or-create and the read-only check.
// serve uses EnsureScaleSet; check uses CheckScaleSet. Both call only the
// official client's concrete APIs and create no custom abstraction.
package scaleset

import (
	"context"
	"errors"
	"fmt"

	scalesetapi "github.com/actions/scaleset"
)

// CheckResult is the result of the read-only check.
type CheckResult struct {
	// Group is the fetched runner group.
	Group *scalesetapi.RunnerGroup
	// ScaleSet is the existing Scale Set, or nil if it does not exist.
	ScaleSet *scalesetapi.RunnerScaleSet
	// Warning explains the note when the Scale Set does not exist, and is
	// empty otherwise. It states that creation permission cannot be proven
	// by read-only GETs alone.
	Warning string
}

// EnsureScaleSet fetches or creates the Scale Set at serve startup. The order
// is: get the runner group, look up the existing Scale Set, create it once if
// missing, and re-fetch once on RunnerExistsError. For an existing set it
// requires ID, name, group ID, a single System label, and DisableUpdate=true
// to match; a mismatch is a fatal error without auto-update. After fetching it
// updates the official client's SystemInfo with build info and the ScaleSetID.
// It never deletes the Scale Set on shutdown.
func (c *Client) EnsureScaleSet(ctx context.Context, groupName, scaleSetName string) (*scalesetapi.RunnerScaleSet, error) {
	// Do not hard-code default=1; always look up the group by name.
	group, err := c.official.GetRunnerGroupByName(ctx, groupName)
	if err != nil {
		return nil, fmt.Errorf("get runner group %q: %w", groupName, err)
	}
	// Validate nil, a positive ID, and the requested name before dereferencing.
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

// createRunnerScaleSet creates the Scale Set exactly once.
// RunnerExistsError means another process already created it, so re-fetch once
// and return that. If the re-fetch finds nothing, it is a contradiction and an
// error.
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
			// The official client returned a nil response without an error.
			return nil, protocolErrorf("create runner scale set", "nil response for %q", name)
		}
		return created, nil
	}
	if !errors.Is(err, scalesetapi.RunnerExistsError) {
		return nil, fmt.Errorf("create runner scale set %q: %w", name, err)
	}
	// Conflict: another process created it. Re-fetch exactly once.
	reget, rerr := c.official.GetRunnerScaleSet(ctx, groupID, name)
	if rerr != nil {
		return nil, fmt.Errorf("reget runner scale set %q after exists error: %w", name, rerr)
	}
	if reget == nil {
		// Reporting exists but not finding it on re-fetch is a contradiction
		// and protocol-fatal.
		return nil, protocolErrorf("reget runner scale set", "%q reports exists but was not found on reget", name)
	}
	return reget, nil
}

// validateRunnerGroup is a pure validator that checks the
// GetRunnerGroupByName response against the contract. A nil response, a
// non-positive ID, and a name mismatch with the request are rejected as
// protocol-fatal. Always call it before dereferencing.
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

// validateScaleSet is a pure validator that checks the Scale Set matches the
// expectation. A nil response is rejected as protocol-fatal. The ID must be
// positive, name and group ID must match the lookup values, there must be
// exactly one System label matching the name, and DisableUpdate must be true.
// A mismatch is an error without auto-update.
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
	// "System" is the fixed value the official client uses for the label Type.
	if len(ss.Labels) != 1 || ss.Labels[0].Type != "System" || ss.Labels[0].Name != name {
		return protocolErrorf("validate scale set", "runner scale set %q must have exactly one System label %q (got %v)", name, name, ss.Labels)
	}
	if !ss.RunnerSetting.DisableUpdate {
		return protocolErrorf("validate scale set", "runner scale set %q must have DisableUpdate=true", name)
	}
	return nil
}

// setSystemInfo updates the official client's SystemInfo with build info and
// the ScaleSetID. It appears in the User-Agent and contains no secrets.
func (c *Client) setSystemInfo(scaleSetID int) {
	c.official.SetSystemInfo(scalesetapi.SystemInfo{
		System:     systemName,
		Version:    c.version,
		CommitSHA:  c.commit,
		ScaleSetID: scaleSetID,
		Subsystem:  "controller",
	})
}

// CheckScaleSet is the read-only path of the check command. It only GETs the
// runner group and the existing Scale Set and never creates a Scale Set,
// runner, or container. An existing Scale Set is verified against the exact
// contract by validateScaleSet; a mismatch is protocol-fatal even read-only,
// without auto-update. Only when the set does not exist does it return a
// warning that creation permission cannot be proven read-only; that alone is
// not a failure.
func (c *Client) CheckScaleSet(ctx context.Context, groupName, scaleSetName string) (*CheckResult, error) {
	group, err := c.official.GetRunnerGroupByName(ctx, groupName)
	if err != nil {
		return nil, fmt.Errorf("get runner group %q: %w", groupName, err)
	}
	// A nil group is protocol-fatal even in the read-only check, without panicking.
	if err := validateRunnerGroup(group, groupName); err != nil {
		return nil, err
	}
	ss, err := c.official.GetRunnerScaleSet(ctx, group.ID, scaleSetName)
	if err != nil {
		return nil, fmt.Errorf("get runner scale set %q: %w", scaleSetName, err)
	}
	// Existing-Scale Set matching and the missing warning are done by a pure
	// function separated from I/O. It uses the same validators as serve's
	// EnsureScaleSet, so check and serve always agree.
	return checkScaleSetResult(group, ss, scaleSetName)
}

// checkScaleSetResult builds the pure result of CheckScaleSet. When ss is
// non-nil it verifies the exact contract with validateScaleSet and returns a
// protocol-fatal error on mismatch. When ss is nil it returns a warning that
// creation permission cannot be proven read-only, without failing.
func checkScaleSetResult(group *scalesetapi.RunnerGroup, ss *scalesetapi.RunnerScaleSet, scaleSetName string) (*CheckResult, error) {
	result := &CheckResult{Group: group, ScaleSet: ss}
	if ss == nil {
		// Creation permission cannot be proven without a write, so keep it as a warning.
		result.Warning = fmt.Sprintf("runner scale set %q does not exist; creation permission cannot be verified with read-only checks", scaleSetName)
		return result, nil
	}
	if err := validateScaleSet(ss, group.ID, scaleSetName); err != nil {
		return nil, err
	}
	return result, nil
}
