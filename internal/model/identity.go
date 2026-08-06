// Package model defines side-effect-free domain types for the runner
// controller. It keeps only runner identity, managed label constants, and
// their generation and validation; there is no state machine, registry, or
// reservation. The controller package owns the in-process state.
package model

import (
	"fmt"
	"strconv"
	"time"
)

// RunnerIdentity represents identifying information for a GitHub runner.
// RunnerID is the positive GitHub runner ID and ScaleSetID is the target
// Scale Set ID. Labels and the container name are auxiliary; they are not the
// source of truth for identity.
type RunnerIdentity struct {
	// ScaleSetID is the ID of the Scale Set the runner belongs to.
	ScaleSetID int64
	// RunnerID is the runner ID issued by GitHub.
	RunnerID int64
	// RunnerName is the runner name registered with GitHub.
	RunnerName string
}

const (
	// ManagedLabelKey is the label key that marks a container as managed by
	// the controller.
	ManagedLabelKey = "managed"
	// ScaleSetIDLabelKey is the label key for the Scale Set ID.
	ScaleSetIDLabelKey = "scale-set-id"
	// RunnerIDLabelKey is the label key for the GitHub runner ID.
	RunnerIDLabelKey = "runner-id"
	// RunnerNameLabelKey is the label key for the GitHub runner name.
	RunnerNameLabelKey = "runner-name"
	// ControllerInstanceLabelKey is the audit label key for the controller
	// process.
	ControllerInstanceLabelKey = "controller-instance"
	// CreatedAtLabelKey is the label key for the creation time.
	CreatedAtLabelKey = "created-at"
	// ManagedLabelValue is the fixed value of the managed label.
	ManagedLabelValue = "true"
)

// RequiredLabelKeys returns the six required label keys.
func RequiredLabelKeys() []string {
	return []string{
		ManagedLabelKey,
		ScaleSetIDLabelKey,
		RunnerIDLabelKey,
		RunnerNameLabelKey,
		ControllerInstanceLabelKey,
		CreatedAtLabelKey,
	}
}

// BuildLabels builds the fixed six labels. createdAt is normalized to UTC
// RFC3339Nano; controllerInstance is used only as audit information.
func BuildLabels(identity RunnerIdentity, controllerInstance string, createdAt time.Time) map[string]string {
	return map[string]string{
		ManagedLabelKey:            ManagedLabelValue,
		ScaleSetIDLabelKey:         strconv.FormatInt(identity.ScaleSetID, 10),
		RunnerIDLabelKey:           strconv.FormatInt(identity.RunnerID, 10),
		RunnerNameLabelKey:         identity.RunnerName,
		ControllerInstanceLabelKey: controllerInstance,
		CreatedAtLabelKey:          createdAt.UTC().Format(time.RFC3339Nano),
	}
}

// ValidateLabels checks that the labels satisfy the invariants for identity
// and the required labels. controller-instance is not an authorization
// condition for destructive operations; it is validated as a non-empty audit
// value.
func ValidateLabels(labels map[string]string, identity RunnerIdentity) error {
	if labels == nil {
		return fmt.Errorf("labels are missing")
	}
	if labels[ManagedLabelKey] != ManagedLabelValue {
		return fmt.Errorf("managed label is invalid")
	}
	if identity.ScaleSetID <= 0 || labels[ScaleSetIDLabelKey] != strconv.FormatInt(identity.ScaleSetID, 10) {
		return fmt.Errorf("scale-set-id label is invalid")
	}
	if identity.RunnerID <= 0 || labels[RunnerIDLabelKey] != strconv.FormatInt(identity.RunnerID, 10) {
		return fmt.Errorf("runner-id label is invalid")
	}
	if identity.RunnerName == "" || labels[RunnerNameLabelKey] != identity.RunnerName {
		return fmt.Errorf("runner-name label is invalid")
	}
	if labels[ControllerInstanceLabelKey] == "" {
		return fmt.Errorf("controller-instance label is missing")
	}
	createdAt := labels[CreatedAtLabelKey]
	parsed, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil || createdAt != parsed.UTC().Format(time.RFC3339Nano) {
		return fmt.Errorf("created-at label is invalid")
	}
	return nil
}

// LabelsMatchIdentity reports whether the required labels match the identity.
func LabelsMatchIdentity(labels map[string]string, identity RunnerIdentity) bool {
	return ValidateLabels(labels, identity) == nil
}
