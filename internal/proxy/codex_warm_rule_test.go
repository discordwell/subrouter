package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
	"github.com/discordwell/subrouter/session"
)

// The owner's warm rule holds for Codex exactly as it does for Claude: a warm
// session -- a turn in flight, its last turn inside the idle bound, or unseen
// since a restart that recent -- keeps its account until that account is
// forced off (a spent 5h or weekly window, the holdout). Measured 2026-09-17..24
// on the box, one Codex Desktop user moved accounts 3,700 times in 39,404 model
// calls, a median 15 s after the previous call and 1,806 times straight back:
// the 5h burn projection re-picked warm Codex sessions on every idle turn.

// hotCodexFixture holds a Codex account whose 5h window is burning too fast
// (80% spent with four hours to run: burn-refused, not exhausted) and a
// healthy second local Codex account the old rule moved warm sessions to.
func hotCodexFixture(t *testing.T, hot selectacct.Score) (Server, *ActiveSessions, *session.Store) {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "s1", "hot", ""); err != nil {
		t.Fatal(err)
	}
	active := NewActiveSessions()
	hot.AccountID, hot.Provider, hot.Fresh = "hot", accounts.ProviderCodex, true
	server := Server{
		Accounts: []accounts.Account{
			{ID: "hot", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "hot-token"},
			{ID: "cool", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "cool-token"},
		},
		Sessions:       store,
		ActiveSessions: active,
		Scheduler: selectacct.NewScheduler([]selectacct.Score{hot, {
			AccountID: "cool", Provider: accounts.ProviderCodex, Fresh: true,
			Headroom: 0.9, ShortHeadroom: 0.9, WeeklyHeadroom: 0.9, WeeklyRemaining: 0.9,
			WeeklyResetAt: time.Now().Add(100 * time.Hour),
		}}),
		MaxBodyBytes: 1024,
	}
	return server, active, store
}

// burnRefusedCodexScore is 76% of the week left with the 5h window burning
// too fast: usableForNewSession refuses it on the RATE while nothing is spent.
func burnRefusedCodexScore() selectacct.Score {
	return selectacct.Score{
		Headroom: 0.76, ShortHeadroom: 0.20,
		ShortWindowSeconds: 5 * 60 * 60, ShortResetAfterSeconds: 4 * 60 * 60,
		WeeklyHeadroom: 0.76, WeeklyRemaining: 0.76, WeeklyResetAt: time.Now().Add(80 * time.Hour),
	}
}

func codexResponsesRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
}

func markCodexIdle(active *ActiveSessions, sessionID string, idle time.Duration) {
	active.lastSeen[session.ScopedSessionKey("codex", sessionID)] = time.Now().Add(-idle)
}

// assertCodexSessionStays checks the account, the pin and the peer decision:
// staying on "hot" locally but handing the comparator a below-floor score
// would still move the conversation, just to the peer instead of "cool".
func assertCodexSessionStays(t *testing.T, server Server, store *session.Store, why string) {
	t.Helper()
	account, _, offered := offeredToHealthyPeer(t, server, accounts.ProviderCodex, "codex", "s1", codexResponsesRequest())
	if account.ID != "hot" {
		t.Fatalf("%s: account = %q, want the session's own account hot", why, account.ID)
	}
	if got, _ := store.Get("codex", "s1"); got.AccountID != "hot" {
		t.Fatalf("%s: pin = %q, want it unchanged on hot", why, got.AccountID)
	}
	if offered {
		t.Fatalf("%s: the session was offered to the peer", why)
	}
}

func assertCodexSessionMovesToCool(t *testing.T, server Server, store *session.Store, why string) {
	t.Helper()
	account, _, _, _, err := server.accountForSessionProvider(accounts.ProviderCodex, "codex", "s1", codexResponsesRequest())
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != "cool" {
		t.Fatalf("%s: account = %q, want cool", why, account.ID)
	}
	if got, _ := store.Get("codex", "s1"); got.AccountID != "cool" {
		t.Fatalf("%s: pin = %q, want cool", why, got.AccountID)
	}
}

func TestHotCodexFixtureIsBurnRefusedButNotEvicted(t *testing.T) {
	// Calibration: without this the warm tests could pass vacuously on an
	// account the scheduler reads as healthy, and the cold one on a pool
	// where hot was never a candidate to leave.
	server, _, _ := hotCodexFixture(t, burnRefusedCodexScore())
	scheduler := server.scheduler().WithReserves(server.AccountReserves)
	if scheduler.UsableForNewSession(accounts.ProviderCodex, "hot") {
		t.Fatal("hot is usable for a new session; the warm tests would prove nothing")
	}
	if scheduler.Evicted(accounts.ProviderCodex, "hot") {
		t.Fatal("hot is evicted; it must be burn-refused WITHOUT being forced off")
	}
	if !scheduler.UsableForNewSession(accounts.ProviderCodex, "cool") {
		t.Fatal("cool is not usable; a move would have nowhere to go")
	}
}

func TestWarmCodexSessionKeepsItsBurnRefusedAccount(t *testing.T) {
	server, active, store := hotCodexFixture(t, burnRefusedCodexScore())
	markCodexIdle(active, "s1", 5*time.Minute)
	assertCodexSessionStays(t, server, store, "last turn 5m ago")
}

func TestInFlightCodexSessionKeepsItsBurnRefusedAccount(t *testing.T) {
	server, active, store := hotCodexFixture(t, burnRefusedCodexScore())
	// Begin stamps lastSeen, so the 3h idle mark must come AFTER it: only the
	// in-flight count keeps this session warm.
	end := active.Begin("codex", "s1")
	defer end()
	markCodexIdle(active, "s1", 3*time.Hour)
	assertCodexSessionStays(t, server, store, "turn in flight")
}

func TestCodexSessionUnseenSinceARecentRestartKeepsItsAccount(t *testing.T) {
	// A fresh tracker has seen no turn: the session may have taken one a
	// second before the restart.
	server, _, store := hotCodexFixture(t, burnRefusedCodexScore())
	assertCodexSessionStays(t, server, store, "unseen since a recent restart")
}

func TestColdCodexSessionOnABurnRefusedAccountMoves(t *testing.T) {
	server, active, store := hotCodexFixture(t, burnRefusedCodexScore())
	markCodexIdle(active, "s1", warmSessionIdleTTL+time.Minute)
	assertCodexSessionMovesToCool(t, server, store, "last turn past the idle bound")
}

func TestCodexSessionUnseenForAFullIdleBoundAfterRestartMoves(t *testing.T) {
	server, active, store := hotCodexFixture(t, burnRefusedCodexScore())
	active.startedAt = time.Now().Add(-(warmSessionIdleTTL + time.Minute))
	assertCodexSessionMovesToCool(t, server, store, "unseen for a full idle bound of uptime")
}

func TestWarmCodexSessionOnAnExhaustedAccountMoves(t *testing.T) {
	exhausted := burnRefusedCodexScore()
	exhausted.ShortHeadroom = 0
	server, active, store := hotCodexFixture(t, exhausted)
	if !server.scheduler().Evicted(accounts.ProviderCodex, "hot") {
		t.Fatal("calibration: hot must read as exhausted")
	}
	markCodexIdle(active, "s1", 5*time.Minute)
	assertCodexSessionMovesToCool(t, server, store, "warm on a spent 5h window")
}

func TestWarmCodexSessionInItsHoldoutMoves(t *testing.T) {
	held := selectacct.Score{
		Headroom: 0.04, ShortHeadroom: 0.9,
		WeeklyHeadroom: 0.04, WeeklyRemaining: 0.04, WeeklyResetAt: time.Now().Add(100 * time.Hour),
	}
	server, active, store := hotCodexFixture(t, held)
	server.AccountReserves = map[string]float64{"hot": 0.05}
	scheduler := server.scheduler().WithReserves(server.AccountReserves)
	if !scheduler.InHoldout(accounts.ProviderCodex, "hot") || scheduler.Exhausted(accounts.ProviderCodex, "hot") {
		t.Fatal("calibration: hot must be inside its holdout without being exhausted")
	}
	end := active.Begin("codex", "s1")
	defer end()
	assertCodexSessionMovesToCool(t, server, store, "turn in flight inside the holdout")
}
