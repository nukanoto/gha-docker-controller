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

	"github.com/nukanoto/gha-docker-controller/internal/config"
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
		t.Skip("GHA_CONTROLLER_E2E=1 が設定されていないため GitHub integration を実行しません (明示 credential の opt-in が必要です)")
	}
	cfgPath := os.Getenv("GHA_CONTROLLER_E2E_CONFIG")
	if cfgPath == "" {
		t.Skip("GHA_CONTROLLER_E2E_CONFIG が設定されていないため GitHub integration を実行しません (専用 config file の path が必要です)")
	}
	cfg, warnings, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("GHA_CONTROLLER_E2E_CONFIG の config を読み込めませんでした: %v", err)
	}
	for _, w := range warnings {
		t.Logf("config warning を確認しました: %s: %s", w.Path, w.Message)
	}
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
		t.Log("既存の Scale Set が無く GHA_CONTROLLER_E2E_MUTATING=1 も無いため listener session の検証は skip します (read-only check のみ)")
	}

	testSessionFailureNonExposure(t, state)
}

// testReadOnlyCheck covers the non-mutating Scale Set lookup.
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
		if result.Warning == "" {
			t.Fatalf("Scale Set が存在しないのに warning が空です")
		}
		t.Logf("Scale Set %q は存在しません (read-only では作成権限を証明できない旨の warning): %s", state.cfg.ScaleSet.Name, result.Warning)
	}
	return result
}

// testDedicatedScaleSet covers the opt-in get-or-create contract.
func testDedicatedScaleSet(t *testing.T, state *e2eState) *scalesetapi.RunnerScaleSet {
	t.Helper()
	// Leave a uniquely named dedicated set for safe reruns.
	setName := e2eSetNamePrefix + "set-" + fmt.Sprintf("%x", rand.Uint64())
	t.Logf("専用 Scale Set 名: %s", setName)

	ss, err := state.client.EnsureScaleSet(state.ctx, state.cfg.ScaleSet.RunnerGroup, setName)
	if err != nil {
		assertNoSecrets(t, err, state.token)
		t.Fatalf("EnsureScaleSet が失敗しました: %v", err)
	}
	if ss == nil {
		t.Fatalf("EnsureScaleSet が nil を返しました (protocol fatal)")
	}

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
	if err := validateScaleSet(ss, result.Group.ID, setName); err != nil {
		t.Fatalf("作成した Scale Set が契約に一致しません: %v", err)
	}
	t.Logf("専用 Scale Set (ID=%d) を取得/作成しました", ss.ID)
	return ss
}

// testListenerSession covers session creation, statistics, and cleanup.
func testListenerSession(t *testing.T, state *e2eState, scaleSetID int) {
	t.Helper()
	lc, err := state.client.NewListenerClient(state.ctx, scaleSetID, state.cfg.GitHub.Owner)
	if err != nil {
		assertNoSecrets(t, err, state.token)
		assertNoURL(t, err)
		t.Fatalf("ListenerClient を開始できませんでした: %v", err)
	}
	// A failed delete is left to server-side expiry after redaction checks.
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

// testSessionFailureNonExposure covers credential and session-token redaction.
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

// assertNoSecrets checks redaction without printing the error body.
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

// assertNoURL rejects message-session URLs that can carry session tokens.
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
