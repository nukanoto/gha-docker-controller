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
		t.Fatalf("nil config の error が期待と異なります: %v", err)
	}
}

// TestCheck_NilConfigGuard covers the pre-check nil guard.
func TestCheck_NilConfigGuard(t *testing.T) {
	err := Check(nil, "dev", "unknown", nil)
	if err == nil || err.Error() != "check: nil config" {
		t.Fatalf("nil config の error が期待と異なります: %v", err)
	}
}

// TestCheck_CheckSpecInputIsPureDummy keeps opaque inputs out of logs/errors.
func TestCheck_CheckSpecInputIsPureDummy(t *testing.T) {
	cfg := &config.Config{ScaleSet: config.ScaleSetConfig{Name: "prod"}}
	in := checkSpecInput(cfg, "v9.9.9")

	if in.JITConfig != "check" {
		t.Fatalf("dummy JIT config が期待と異なります: %q", in.JITConfig)
	}
	if in.ControllerInstance != "check" {
		t.Fatalf("dummy controller instance が期待と異なります: %q", in.ControllerInstance)
	}
	if in.Identity.ScaleSetID != 1 || in.Identity.RunnerID != 1 {
		t.Fatalf("dummy identity が正の固定値ではありません: %+v", in.Identity)
	}
	if want := model.RunnerName("prod", "000000000000"); in.Identity.RunnerName != want {
		t.Fatalf("dummy runner name が canonical 形式と異なります: %q want %q", in.Identity.RunnerName, want)
	}
	if want := model.ContainerName("prod", 1, "000000000000"); in.ContainerName != want {
		t.Fatalf("dummy container name が命名規約と異なります: %q want %q", in.ContainerName, want)
	}
	if in.UserAgentVersion != "v9.9.9" {
		t.Fatalf("UserAgentVersion が渡した version と異なります: %q", in.UserAgentVersion)
	}
	// CreatedAt is part of the managed-label contract and must be current UTC.
	if in.CreatedAt.IsZero() || in.CreatedAt.Location() != time.UTC {
		t.Fatalf("CreatedAt が UTC ではありません: %v", in.CreatedAt)
	}
	if d := time.Since(in.CreatedAt); d < 0 || d > time.Minute {
		t.Fatalf("CreatedAt が現在時刻とかけ離れています: %v", in.CreatedAt)
	}
}
