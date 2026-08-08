//go:build integration

// GitHub integration uses the official API only after explicit credential and
// dedicated-config opt-in. Read-only checks always run; mutating Scale Set
// creation requires GHA_CONTROLLER_E2E_MUTATING=1. JIT generation is excluded
// because its server-side runner cannot be cleaned up.
package scaleset

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"

	scalesetapi "github.com/actions/scaleset"

	"github.com/nukanoto/arc-docker/internal/config"
)

const (
	e2eTestTimeout = 3 * time.Minute

	// Reject configurations that could target a production Scale Set.
	e2eSetNamePrefix = "ghadc-e2e-"

	// This ID exercises failure redaction without creating a server object.
	e2eNonexistentScaleSetID = 1 << 30
)

// e2eState holds the explicitly opted-in integration state.
type e2eState struct {
	cfg      *config.Config
	client   *Client
	ctx      context.Context
	token    string
	mutating bool
}

// TestGitHubIntegration is the opt-in GitHub integration entry point.
func TestGitHubIntegration(t *testing.T) {
	if os.Getenv("GHA_CONTROLLER_E2E") != "1" {
		t.Skip("GHA_CONTROLLER_E2E=1 is not set; skipping GitHub integration (explicit credential opt-in is required)")
	}
	cfgPath := os.Getenv("GHA_CONTROLLER_E2E_CONFIG")
	if cfgPath == "" {
		t.Skip("GHA_CONTROLLER_E2E_CONFIG is not set; skipping GitHub integration (a dedicated config file path is required)")
	}
	cfg, warnings, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("failed to load config from GHA_CONTROLLER_E2E_CONFIG: %v", err)
	}
	for _, w := range warnings {
		t.Logf("config warning: %s: %s", w.Path, w.Message)
	}
	if !strings.HasPrefix(cfg.ScaleSet.Name, e2eSetNamePrefix) {
		t.Fatalf("config Scale Set name %q lacks test-only prefix %q; refusing to target a production name", cfg.ScaleSet.Name, e2eSetNamePrefix)
	}

	state := &e2eState{
		cfg:      cfg,
		token:    cfg.GitHub.Token,
		mutating: os.Getenv("GHA_CONTROLLER_E2E_MUTATING") == "1",
	}
	state.client, err = New(cfg, "integration-test", "integration-test")
	if err != nil {
		t.Fatalf("failed to create Scale Set client: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), e2eTestTimeout)
	defer cancel()
	state.ctx = ctx

	check := testReadOnlyCheck(t, state)

	if state.mutating {
		ss := testDedicatedScaleSet(t, state)
		testListenerSession(t, state, ss.ID)
	} else if check != nil && check.ScaleSet != nil {
		testListenerSession(t, state, check.ScaleSet.ID)
	} else {
		t.Log("an existing Scale Set is unavailable and GHA_CONTROLLER_E2E_MUTATING=1 is not set; skipping listener session validation (read-only check only)")
	}

	testSessionFailureNonExposure(t, state)
}

// testReadOnlyCheck covers the non-mutating Scale Set lookup.
func testReadOnlyCheck(t *testing.T, state *e2eState) *CheckResult {
	t.Helper()
	result, err := state.client.CheckScaleSet(state.ctx, state.cfg.ScaleSet.RunnerGroup, state.cfg.ScaleSet.Name)
	if err != nil {
		assertNoSecrets(t, err, state.token)
		t.Fatalf("CheckScaleSet failed: %v", err)
	}
	if result.Group == nil {
		t.Fatalf("CheckScaleSet returned a nil runner group (protocol failure)")
	}
	if result.Group.ID <= 0 {
		t.Fatalf("runner group %q has a non-positive ID: %d", state.cfg.ScaleSet.RunnerGroup, result.Group.ID)
	}
	if result.Group.Name != state.cfg.ScaleSet.RunnerGroup {
		t.Fatalf("runner group name differs from the request: got %q want %q", result.Group.Name, state.cfg.ScaleSet.RunnerGroup)
	}
	if result.ScaleSet != nil {
		if result.ScaleSet.ID <= 0 {
			t.Fatalf("Scale Set %q has a non-positive ID: %d", state.cfg.ScaleSet.Name, result.ScaleSet.ID)
		}
		if result.ScaleSet.Name != state.cfg.ScaleSet.Name {
			t.Fatalf("Scale Set name differs from the request: got %q want %q", result.ScaleSet.Name, state.cfg.ScaleSet.Name)
		}
		if result.ScaleSet.RunnerGroupID != result.Group.ID {
			t.Fatalf("Scale Set group ID differs from the fetched group: set=%d group=%d", result.ScaleSet.RunnerGroupID, result.Group.ID)
		}
		if result.Warning != "" {
			t.Fatalf("warning returned for an existing Scale Set: %s", result.Warning)
		}
		t.Logf("fetched existing Scale Set %q (ID=%d) in read-only mode", result.ScaleSet.Name, result.ScaleSet.ID)
	} else {
		if result.Warning == "" {
			t.Fatalf("warning is empty for a missing Scale Set")
		}
		t.Logf("Scale Set %q is missing (read-only mode cannot prove create permission): %s", state.cfg.ScaleSet.Name, result.Warning)
	}
	return result
}

// testDedicatedScaleSet covers the opt-in get-or-create contract.
func testDedicatedScaleSet(t *testing.T, state *e2eState) *scalesetapi.RunnerScaleSet {
	t.Helper()
	// Leave a uniquely named dedicated set for safe reruns.
	setName := e2eSetNamePrefix + "set-" + fmt.Sprintf("%x", rand.Uint64())
	t.Logf("dedicated Scale Set name: %s", setName)

	ss, err := state.client.EnsureScaleSet(state.ctx, state.cfg.ScaleSet.RunnerGroup, setName)
	if err != nil {
		assertNoSecrets(t, err, state.token)
		t.Fatalf("EnsureScaleSet failed: %v", err)
	}
	if ss == nil {
		t.Fatalf("EnsureScaleSet returned nil (protocol failure)")
	}

	result, err := state.client.CheckScaleSet(state.ctx, state.cfg.ScaleSet.RunnerGroup, setName)
	if err != nil {
		assertNoSecrets(t, err, state.token)
		t.Fatalf("CheckScaleSet failed after creation: %v", err)
	}
	if result.ScaleSet == nil {
		t.Fatalf("CheckScaleSet did not find the set after creation")
	}
	if result.Warning != "" {
		t.Fatalf("CheckScaleSet returned a warning after creation: %s", result.Warning)
	}
	if err := validateScaleSet(ss, result.Group.ID, setName); err != nil {
		t.Fatalf("created Scale Set violates the contract: %v", err)
	}
	t.Logf("fetched or created dedicated Scale Set (ID=%d)", ss.ID)
	return ss
}

// testListenerSession covers session creation, statistics, and cleanup.
func testListenerSession(t *testing.T, state *e2eState, scaleSetID int) {
	t.Helper()
	lc, err := state.client.NewListenerClient(state.ctx, scaleSetID, state.cfg.GitHub.Owner)
	if err != nil {
		assertNoSecrets(t, err, state.token)
		assertNoURL(t, err)
		t.Fatalf("failed to start ListenerClient: %v", err)
	}
	// A failed delete is left to server-side expiry after redaction checks.
	defer func() {
		if err := lc.Close(state.ctx); err != nil {
			assertNoSecrets(t, err, state.token)
			assertNoURL(t, err)
			t.Errorf("ListenerClient.Close failed (relying on server-side session expiry): %v", err)
		}
	}()

	stats := lc.Session().Statistics
	if stats == nil {
		t.Fatalf("Statistics is nil immediately after starting the session (protocol failure)")
	}
	t.Logf("initial statistics: available=%d acquired=%d assigned=%d running=%d registered=%d busy=%d idle=%d",
		stats.TotalAvailableJobs, stats.TotalAcquiredJobs, stats.TotalAssignedJobs, stats.TotalRunningJobs,
		stats.TotalRegisteredRunners, stats.TotalBusyRunners, stats.TotalIdleRunners)
}

// testSessionFailureNonExposure covers credential and session-token redaction.
func testSessionFailureNonExposure(t *testing.T, state *e2eState) {
	t.Helper()
	_, err := state.client.NewListenerClient(state.ctx, e2eNonexistentScaleSetID, state.cfg.GitHub.Owner)
	if err == nil {
		t.Fatalf("session creation succeeded for missing Scale Set (ID=%d)", e2eNonexistentScaleSetID)
	}
	assertNoSecrets(t, err, state.token)
	assertNoURL(t, err)
	t.Log("verified that the failure-path error does not expose credentials or the session token")
}

// assertNoSecrets checks redaction without printing the error body.
func assertNoSecrets(t *testing.T, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		return
	}
	msg := err.Error()
	for _, s := range secrets {
		if s != "" && strings.Contains(msg, s) {
			t.Fatalf("error string contains a secret (secret non-disclosure contract violation); error text is omitted to avoid exposing it")
		}
	}
}

// assertNoURL rejects message-session URLs that can carry session tokens.
func assertNoURL(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	msg := err.Error()
	if strings.Contains(msg, "http://") || strings.Contains(msg, "https://") {
		t.Fatalf("error string contains a URL (possible session token exposure); error text is omitted")
	}
}
