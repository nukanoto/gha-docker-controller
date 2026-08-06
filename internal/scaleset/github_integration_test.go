//go:build integration

// The GitHub integration test verifies the official actions/scaleset API
// against real GitHub.com.
//
// Startup conditions (explicit opt-in):
//   - GHA_CONTROLLER_E2E=1
//   - GHA_CONTROLLER_E2E_CONFIG=<path to a dedicated config file>
//
// Without both, the GitHub parts are skipped with a reason. This differs from
// the Docker integration, which fails when the daemon is missing: GitHub is
// never accessed externally without an explicit credential opt-in. The config
// is loaded with the same config.Load as production, and the PAT comes from
// the GITHUB_TOKEN environment variable. The PAT never appears in test args,
// YAML, or failure output.
//
// The coverage is the minimal set aligned with the official listener failure
// model:
//   - read-only check (CheckScaleSet): GETs existing objects only.
//   - get-or-create exact contract (EnsureScaleSet): runs only with the
//     separate env GHA_CONTROLLER_E2E_MUTATING=1. The config's scaleset.name
//     must have the prefix "ghadc-e2e-", and running against a production
//     name is rejected as a failure. Like the "no delete on shutdown"
//     contract, the created Scale Set is left as a uniquely named dedicated
//     set (a rerun reuses it via get-or-create).
//   - listener session creation: a temporary server-side object that creates
//     no Scale Set or runner (deleted by Close); with an existing set it can
//     be verified without mutating.
//   - session creation failure path non-exposure: operating on a Scale Set ID
//     that cannot exist creates nothing server-side and fails, so it always
//     runs.
//
// No mutating JIT generation tests. JIT config generation can create
// server-side objects that cannot be cleaned up, so JIT response validation
// is limited to the pure validateJitConfig (jit_test.go). No mocks/fakes/
// stubs; only official GitHub.com is targeted.
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

	"github.com/nukanoto/gha-docker-controller/internal/config"
)

const (
	// e2eTestTimeout is the deadline of the whole GitHub E2E.
	e2eTestTimeout = 3 * time.Minute

	// e2eSetNamePrefix is the required prefix of test-only Scale Set names.
	// A config without this prefix is treated as a production name and rejected.
	e2eSetNamePrefix = "ghadc-e2e-"

	// e2eNonexistentScaleSetID is a Scale Set ID that cannot exist. IDs are
	// small sequential numbers, so this is only used for the failure path
	// (credential non-exposure).
	e2eNonexistentScaleSetID = 1 << 30
)

// e2eState is the runtime state shared inside TestGitHubIntegration.
type e2eState struct {
	cfg      *config.Config
	client   *Client
	ctx      context.Context
	token    string
	mutating bool
}

// TestGitHubIntegration is the entry point of the integration test against
// real GitHub.com. Without the opt-in environment variables, the GitHub parts
// are skipped with a reason (a different design from the Docker integration,
// which fails on a missing daemon).
func TestGitHubIntegration(t *testing.T) {
	// Never access external services without an explicit credential opt-in.
	if os.Getenv("GHA_CONTROLLER_E2E") != "1" {
		t.Skip("GHA_CONTROLLER_E2E=1 が設定されていないため GitHub integration を実行しません (明示 credential の opt-in が必要です)")
	}
	cfgPath := os.Getenv("GHA_CONTROLLER_E2E_CONFIG")
	if cfgPath == "" {
		t.Skip("GHA_CONTROLLER_E2E_CONFIG が設定されていないため GitHub integration を実行しません (専用 config file の path が必要です)")
	}
	// An unreadable explicit config is an environment setup error: fail, not skip.
	cfg, warnings, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("GHA_CONTROLLER_E2E_CONFIG の config を読み込めませんでした: %v", err)
	}
	for _, w := range warnings {
		t.Logf("config warning を確認しました: %s: %s", w.Path, w.Message)
	}
	// Reject running against a production name; require the test-only prefix.
	if !strings.HasPrefix(cfg.ScaleSet.Name, e2eSetNamePrefix) {
		t.Fatalf("config の scaleset.name %q は test 専用 prefix %q を持ちません (production 名への誤実行を拒否します)", cfg.ScaleSet.Name, e2eSetNamePrefix)
	}

	state := &e2eState{
		cfg:      cfg,
		token:    cfg.GitHub.Token,
		mutating: os.Getenv("GHA_CONTROLLER_E2E_MUTATING") == "1",
	}
	state.client, err = New(cfg, "integration-test", "integration-test")
	if err != nil {
		t.Fatalf("scaleset client を作成できませんでした: %v", err)
	}
	// Keep all I/O within this deadline.
	ctx, cancel := context.WithTimeout(t.Context(), e2eTestTimeout)
	defer cancel()
	state.ctx = ctx

	// 1. Read-only check. Only GETs existing objects; creates nothing.
	check := testReadOnlyCheck(t, state)

	// 2. Listener session creation. When mutating, first get-or-create the
	// dedicated Scale Set and verify the session on it. With an existing set,
	// the session can be verified without mutating (a session is a temporary
	// object deleted by Close).
	if state.mutating {
		ss := testDedicatedScaleSet(t, state)
		testListenerSession(t, state, ss.ID)
	} else if check != nil && check.ScaleSet != nil {
		testListenerSession(t, state, check.ScaleSet.ID)
	} else {
		t.Log("既存の Scale Set が無く GHA_CONTROLLER_E2E_MUTATING=1 も無いため listener session の検証は skip します (read-only check のみ)")
	}

	// 3. Session creation failure path non-exposure. Operating on a Scale Set
	// ID that cannot exist creates nothing server-side, so it runs regardless
	// of the mutating opt-in.
	testSessionFailureNonExposure(t, state)
}

// testReadOnlyCheck verifies the read-only contract of CheckScaleSet. It only
// GETs the runner group and the existing Scale Set; it never creates
// anything. A missing Scale Set returns a warning and that alone is not a
// failure. It also verifies that no credential leaks into errors.
func testReadOnlyCheck(t *testing.T, state *e2eState) *CheckResult {
	t.Helper()
	result, err := state.client.CheckScaleSet(state.ctx, state.cfg.ScaleSet.RunnerGroup, state.cfg.ScaleSet.Name)
	if err != nil {
		assertNoSecrets(t, err, state.token)
		t.Fatalf("CheckScaleSet が失敗しました: %v", err)
	}
	if result.Group == nil {
		t.Fatalf("CheckScaleSet の runner group が nil です (protocol fatal)")
	}
	if result.Group.ID <= 0 {
		t.Fatalf("runner group %q の ID が正ではありません: %d", state.cfg.ScaleSet.RunnerGroup, result.Group.ID)
	}
	if result.Group.Name != state.cfg.ScaleSet.RunnerGroup {
		t.Fatalf("runner group 名が要求と一致しません: got %q, want %q", result.Group.Name, state.cfg.ScaleSet.RunnerGroup)
	}
	if result.ScaleSet != nil {
		if result.ScaleSet.ID <= 0 {
			t.Fatalf("Scale Set %q の ID が正ではありません: %d", state.cfg.ScaleSet.Name, result.ScaleSet.ID)
		}
		if result.ScaleSet.Name != state.cfg.ScaleSet.Name {
			t.Fatalf("Scale Set 名が要求と一致しません: got %q, want %q", result.ScaleSet.Name, state.cfg.ScaleSet.Name)
		}
		if result.ScaleSet.RunnerGroupID != result.Group.ID {
			t.Fatalf("Scale Set の group ID が GET した group と一致しません: set=%d group=%d", result.ScaleSet.RunnerGroupID, result.Group.ID)
		}
		if result.Warning != "" {
			t.Fatalf("Scale Set が存在するのに warning が返りました: %s", result.Warning)
		}
		t.Logf("既存 Scale Set %q (ID=%d) を read-only で取得しました", result.ScaleSet.Name, result.ScaleSet.ID)
	} else {
		// A missing set returns a warning that creation permission cannot be
		// proven read-only.
		if result.Warning == "" {
			t.Fatalf("Scale Set が存在しないのに warning が空です")
		}
		t.Logf("Scale Set %q は存在しません (read-only では作成権限を証明できない旨の warning): %s", state.cfg.ScaleSet.Name, result.Warning)
	}
	return result
}

// testDedicatedScaleSet verifies the get-or-create contract and runs only
// with GHA_CONTROLLER_E2E_MUTATING=1. It get-or-creates a uniquely named
// dedicated Scale Set and confirms an existing one (left over from a previous
// run) passes the match validation. Like the "no delete on shutdown"
// contract, the created Scale Set is left in place.
func testDedicatedScaleSet(t *testing.T, state *e2eState) *scalesetapi.RunnerScaleSet {
	t.Helper()
	// A unique name per run. It never collides with sets left from past runs.
	setName := e2eSetNamePrefix + "set-" + fmt.Sprintf("%x", rand.Uint64())
	t.Logf("専用 Scale Set 名: %s", setName)

	// get-or-create. An existing set (left from a previous run) must pass the
	// match validation.
	ss, err := state.client.EnsureScaleSet(state.ctx, state.cfg.ScaleSet.RunnerGroup, setName)
	if err != nil {
		assertNoSecrets(t, err, state.token)
		t.Fatalf("EnsureScaleSet が失敗しました: %v", err)
	}
	if ss == nil {
		t.Fatalf("EnsureScaleSet が nil を返しました (protocol fatal)")
	}

	// Confirm the read-only check recognizes the created set and the warning is gone.
	result, err := state.client.CheckScaleSet(state.ctx, state.cfg.ScaleSet.RunnerGroup, setName)
	if err != nil {
		assertNoSecrets(t, err, state.token)
		t.Fatalf("作成後の CheckScaleSet が失敗しました: %v", err)
	}
	if result.ScaleSet == nil {
		t.Fatalf("作成後の CheckScaleSet が set を認識しません")
	}
	if result.Warning != "" {
		t.Fatalf("作成後の CheckScaleSet に warning が残っています: %s", result.Warning)
	}
	// get-or-create contract: ID, name, group, a single System label, DisableUpdate=true.
	if err := validateScaleSet(ss, result.Group.ID, setName); err != nil {
		t.Fatalf("作成した Scale Set が契約に一致しません: %v", err)
	}
	t.Logf("専用 Scale Set (ID=%d) を取得/作成しました", ss.ID)
	return ss
}

// testListenerSession verifies creating the transparent session adapter for
// the official listener. NewListenerClient starts it; the initial Statistics
// must be non-nil (protocol-fatal condition); Close deletes the session. It
// also verifies that start/Close errors expose no credential and no URL with
// a session token.
func testListenerSession(t *testing.T, state *e2eState, scaleSetID int) {
	t.Helper()
	lc, err := state.client.NewListenerClient(state.ctx, scaleSetID, state.cfg.GitHub.Owner)
	if err != nil {
		assertNoSecrets(t, err, state.token)
		assertNoURL(t, err)
		t.Fatalf("ListenerClient を開始できませんでした: %v", err)
	}
	// On delete failure, rely on the server-side session expiry and only verify
	// credential non-exposure in the error.
	defer func() {
		if err := lc.Close(state.ctx); err != nil {
			assertNoSecrets(t, err, state.token)
			assertNoURL(t, err)
			t.Errorf("ListenerClient.Close が失敗しました (session は server 側の失効に任せます): %v", err)
		}
	}()

	stats := lc.Session().Statistics
	if stats == nil {
		t.Fatalf("session 開始直後の Statistics が nil です (protocol fatal)")
	}
	t.Logf("初期 statistics: available=%d acquired=%d assigned=%d running=%d registered=%d busy=%d idle=%d",
		stats.TotalAvailableJobs, stats.TotalAcquiredJobs, stats.TotalAssignedJobs, stats.TotalRunningJobs,
		stats.TotalRegisteredRunners, stats.TotalBusyRunners, stats.TotalIdleRunners)
}

// testSessionFailureNonExposure verifies that the real API failure path
// exposes neither the credential nor a URL with the session token in errors.
// Creating a session for a nonexistent Scale Set ID always fails and creates
// nothing server-side. JIT generation failures are not verified (JIT is a
// mutating operation that cannot be cleaned up, so integration never calls
// it).
func testSessionFailureNonExposure(t *testing.T, state *e2eState) {
	t.Helper()
	_, err := state.client.NewListenerClient(state.ctx, e2eNonexistentScaleSetID, state.cfg.GitHub.Owner)
	if err == nil {
		t.Fatalf("存在しない Scale Set (ID=%d) への session 作成が成功しました", e2eNonexistentScaleSetID)
	}
	assertNoSecrets(t, err, state.token)
	assertNoURL(t, err)
	t.Log("failure 経路の error に credential / session token の露出がないことを確認しました")
}

// assertNoSecrets verifies that the error string contains none of the secrets
// (PAT, JIT encoded value, and so on). On failure the error body is not
// printed (prevents re-exposing the secret).
func assertNoSecrets(t *testing.T, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		return
	}
	msg := err.Error()
	for _, s := range secrets {
		if s != "" && strings.Contains(msg, s) {
			t.Fatalf("error の文字列に秘密が含まれています (秘密非露出契約違反)。error 本文は秘密露出のため出力しません")
		}
	}
}

// assertNoURL verifies that the error string contains no URL. redactSessionError
// removes the URL from message session errors, so a URL is treated as a sign
// of session token exposure and fails the test.
func assertNoURL(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	msg := err.Error()
	if strings.Contains(msg, "http://") || strings.Contains(msg, "https://") {
		t.Fatalf("error の文字列に URL が含まれています (session token 露出の可能性)。error 本文は出力しません")
	}
}
