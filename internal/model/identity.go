// Package model defines runner identity and managed-label invariants.
package model

import (
	"fmt"
	"strconv"
	"time"
)

// RunnerIdentity identifies a GitHub runner in a Scale Set.
type RunnerIdentity struct {
	ScaleSetID int64
	RunnerID   int64
	RunnerName string
}

const (
	// ManagedLabelKey marks a controller-managed container.
	ManagedLabelKey = "managed"
	// ScaleSetIDLabelKey stores the Scale Set ID.
	ScaleSetIDLabelKey = "scale-set-id"
	// RunnerIDLabelKey stores the GitHub runner ID.
	RunnerIDLabelKey = "runner-id"
	// RunnerNameLabelKey stores the GitHub runner name.
	RunnerNameLabelKey = "runner-name"
	// ControllerInstanceLabelKey stores the controller instance.
	ControllerInstanceLabelKey = "controller-instance"
	// CreatedAtLabelKey stores the container creation time.
	CreatedAtLabelKey = "created-at"
	// ManagedLabelValue is the fixed managed marker.
	ManagedLabelValue = "true"
)

// RequiredLabelKeys returns the required managed-label keys.
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

// BuildLabels builds the managed labels with a canonical UTC timestamp.
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

// ValidateLabels checks identity labels and the non-empty audit fields.
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

// LabelsMatchIdentity reports whether labels satisfy the identity contract.
func LabelsMatchIdentity(labels map[string]string, identity RunnerIdentity) bool {
	return ValidateLabels(labels, identity) == nil
}
