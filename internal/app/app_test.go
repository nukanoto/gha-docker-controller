// app_test.go verifies only the pure parts of the app package. Serve/Check
// external I/O (Docker, GitHub) is not faked; paths without I/O such as the
// nil config guard and the check dummy input generation (checkSpecInput) are
// verified with real objects. Tests that start the daemon are integration
// tag targets.
package app

import (
	"testing"
	"time"

	"github.com/nukanoto/gha-docker-controller/internal/config"
	"github.com/nukanoto/gha-docker-controller/internal/model"
)

// TestServe_NilConfigGuard verifies that Serve rejects a nil config
// immediately without I/O and returns "serve: nil config". This guard runs
// before the logger and signal registration.
func TestServe_NilConfigGuard(t *testing.T) {
	err := Serve(nil, "dev", "unknown", nil)
	if err == nil || err.Error() != "serve: nil config" {
		t.Fatalf("nil config の error が期待と異なります: %v", err)
	}
}

// TestCheck_NilConfigGuard verifies that Check rejects a nil config
// immediately without I/O and returns "check: nil config".
func TestCheck_NilConfigGuard(t *testing.T) {
	err := Check(nil, "dev", "unknown", nil)
	if err == nil || err.Error() != "check: nil config" {
		t.Fatalf("nil config の error が期待と異なります: %v", err)
	}
}

// TestCheck_CheckSpecInputIsPureDummy verifies that checkSpecInput produces
// fixed dummy values without external I/O. The JIT config and controller
// instance are an opaque secret / audit value, so they are fixed dummies
// ("check") and no real value ever reaches logs or errors. The identity
// follows positive dummy IDs and the naming conventions.
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
	// CreatedAt is the current UTC time, i.e., right before the call.
	if in.CreatedAt.IsZero() || in.CreatedAt.Location() != time.UTC {
		t.Fatalf("CreatedAt が UTC ではありません: %v", in.CreatedAt)
	}
	if d := time.Since(in.CreatedAt); d < 0 || d > time.Minute {
		t.Fatalf("CreatedAt が現在時刻とかけ離れています: %v", in.CreatedAt)
	}
}
