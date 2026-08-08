// These unit tests cover app guards and pure input construction without I/O.
package app

import (
	"testing"
	"time"

	"github.com/nukanoto/arc-docker/internal/config"
	"github.com/nukanoto/arc-docker/internal/model"
)

// TestServe_NilConfigGuard covers the pre-start nil check.
func TestServe_NilConfigGuard(t *testing.T) {
	err := Serve(nil, "dev", "unknown", nil)
	if err == nil || err.Error() != "serve: nil config" {
		t.Fatalf("nil config error differs from expectation: %v", err)
	}
}

// TestCheck_NilConfigGuard covers the pre-check nil guard.
func TestCheck_NilConfigGuard(t *testing.T) {
	err := Check(nil, "dev", "unknown", nil)
	if err == nil || err.Error() != "check: nil config" {
		t.Fatalf("nil config error differs from expectation: %v", err)
	}
}

// TestCheck_CheckSpecInputIsPureDummy keeps opaque inputs out of logs/errors.
func TestCheck_CheckSpecInputIsPureDummy(t *testing.T) {
	cfg := &config.Config{ScaleSet: config.ScaleSetConfig{Name: "prod"}}
	in := checkSpecInput(cfg, "v9.9.9")

	if in.JITConfig != "check" {
		t.Fatalf("dummy JIT config differs from expectation: %q", in.JITConfig)
	}
	if in.ControllerInstance != "check" {
		t.Fatalf("dummy controller instance differs from expectation: %q", in.ControllerInstance)
	}
	if in.Identity.ScaleSetID != 1 || in.Identity.RunnerID != 1 {
		t.Fatalf("dummy identity is not the expected positive fixed value: %+v", in.Identity)
	}
	if want := model.RunnerName("prod", "000000000000"); in.Identity.RunnerName != want {
		t.Fatalf("dummy runner name is not canonical: got %q want %q", in.Identity.RunnerName, want)
	}
	if want := model.ContainerName("prod", 1, "000000000000"); in.ContainerName != want {
		t.Fatalf("dummy container name violates the naming contract: got %q want %q", in.ContainerName, want)
	}
	if in.UserAgentVersion != "v9.9.9" {
		t.Fatalf("UserAgentVersion differs from the supplied version: %q", in.UserAgentVersion)
	}
	// CreatedAt is part of the managed-label contract and must be current UTC.
	if in.CreatedAt.IsZero() || in.CreatedAt.Location() != time.UTC {
		t.Fatalf("CreatedAt is not UTC: %v", in.CreatedAt)
	}
	if d := time.Since(in.CreatedAt); d < 0 || d > time.Minute {
		t.Fatalf("CreatedAt is too far from the current time: %v", in.CreatedAt)
	}
}
