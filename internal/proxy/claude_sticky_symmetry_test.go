package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// A local Claude session is exactly as hard to move to the peer as a delegated
// one is to move back: while its prompt cache is warm it stays on its account
// until that account is exhausted or a turn fails. Measured 2026-09-10..24, the
// below-floor verdict -- mostly the 5h burn projection -- moved 437 warm local
// sessions to the peer from accounts with most of their week left, each one a
// full cold-cache rewrite, while nothing on the peer side could do the same.

// hotAccountFixture holds one Claude account with 76% of its week left whose
// 5h window is burning too fast: 80% of the window spent with four hours still
// to run. That fails usableForNewSession on the burn RATE without being
// exhausted, which is the exact state the live pool was in when it moved warm
// sessions.
func hotAccountFixture(t *testing.T) (Server, *ActiveSessions) {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "s1", "hot", ""); err != nil {
		t.Fatal(err)
	}
	active := NewActiveSessions()
	server := Server{
		Accounts: []accounts.Account{
			{ID: "hot", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "hot-token"},
		},
		Sessions:       store,
		ActiveSessions: active,
		Scheduler: selectacct.NewScheduler([]selectacct.Score{{
			AccountID: "hot", Provider: accounts.ProviderClaude, Fresh: true,
			Headroom: 0.76, ShortHeadroom: 0.20,
			ShortWindowSeconds: 5 * 60 * 60, ShortResetAfterSeconds: 4 * 60 * 60,
			WeeklyHeadroom: 0.76, WeeklyResetAt: time.Now().Add(80 * time.Hour),
		}}),
		MaxBodyBytes: 1024,
	}
	return server, active
}

// healthyPeer is a peer the comparator would pick over any below-floor local
// account, so a test that keeps the session local is not passing because the
// peer looked worse.
func healthyPeer() selectacct.Score {
	return selectacct.Score{
		Headroom: 1, ShortHeadroom: 1, Fresh: true, WeeklyHeadroom: 1,
		WeeklyResetAt: time.Now().Add(30 * time.Hour),
	}
}

// offeredToHealthyPeer runs selection exactly as the handler does for a turn
// that may go to a peer -- the crossPool option set, so every ranking
// selection runs is the one cross-pool ranking -- against one peer publishing
// a single eligible account scored healthyPeer. It reports whether that
// ranking put the peer account first. A session selection keeps without
// ranking (the warm rule) is never offered, which is the point: the rule is
// read off the real path, not off a restated decision.
func offeredToHealthyPeer(t *testing.T, server Server, provider accounts.Provider, agent, sessionID string, r *http.Request) (accounts.Account, routingCapacity, bool) {
	t.Helper()
	score := healthyPeer()
	verdict := selectacct.NewScheduler([]selectacct.Score{func() selectacct.Score {
		s := score
		s.AccountID, s.Provider = "peer-best", provider
		return s
	}()}).NewSessionVerdict(provider, "peer-best", time.Now())
	if !verdict.Eligible {
		t.Fatalf("calibration: the healthy peer account is not eligible by its own verdict: %+v", verdict)
	}
	row := map[string]any{
		"id": "peer-best", "provider": string(provider), "auth_mode": "oauth",
		"auth_checked": true, "auth_valid": true,
		"normalized": map[string]any{
			"headroom": score.Headroom, "short_headroom": score.ShortHeadroom, "fresh": true,
			"weekly_headroom": score.WeeklyHeadroom, "seconds_to_weekly_reset": time.Until(score.WeeklyResetAt).Seconds(),
			"new_session": verdict,
		},
	}
	body, err := json.Marshal([]any{row})
	if err != nil {
		t.Fatal(err)
	}
	ranking := server.newCrossPoolRanking(r, provider, "", newRemotePoolTurn(r.Context()))
	ranking.fetched, ranking.considered = true, 1
	ranking.snapshots = []crossPoolPeerSnapshot{{
		peer:     NewRemotePool("http://peer.invalid", "", ""),
		snapshot: AdminSnapshot{Body: body, At: time.Now()},
	}}
	account, _, _, capacity, err := server.accountForSessionProviderWithOptions(provider, agent, sessionID, r, accountSelectionOptions{crossPool: ranking})
	if err != nil {
		t.Fatal(err)
	}
	return account, capacity, ranking.ranked && ranking.placement.Peer != nil
}

func claudeMessagesRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
}

func TestHotAccountFixtureIsBelowFloorButNotExhausted(t *testing.T) {
	// Calibration: without this the two tests below could pass vacuously on
	// an account the scheduler reads as healthy.
	server, _ := hotAccountFixture(t)
	scheduler := server.scheduler()
	if scheduler.UsableForNewSession(accounts.ProviderClaude, "hot") {
		t.Fatal("fixture account is usable for a new session; the tests below would prove nothing")
	}
	if scheduler.Exhausted(accounts.ProviderClaude, "hot") {
		t.Fatal("fixture account is exhausted; it must be below floor WITHOUT being spent")
	}
}

func TestWarmLocalClaudeSessionIsNotOfferedToThePeer(t *testing.T) {
	server, active := hotAccountFixture(t)
	markIdle(active, "s1", 5*time.Minute)

	account, _, offered := offeredToHealthyPeer(t, server, accounts.ProviderClaude, "claude", "s1", claudeMessagesRequest())
	if account.ID != "hot" {
		t.Fatalf("account = %q, want the session's own account", account.ID)
	}
	if offered {
		t.Fatal("a warm local session was moved to the peer")
	}
}

func TestInFlightLocalClaudeSessionIsNotOfferedToThePeer(t *testing.T) {
	server, active := hotAccountFixture(t)
	// Begin stamps lastSeen, so the 3h idle mark must come AFTER it: only then
	// does the in-flight count alone keep the session warm, which is the
	// branch this test exists to pin.
	end := active.Begin("claude", "s1")
	defer end()
	markIdle(active, "s1", 3*time.Hour)

	if _, _, offered := offeredToHealthyPeer(t, server, accounts.ProviderClaude, "claude", "s1", claudeMessagesRequest()); offered {
		t.Fatal("a session with a turn in flight was moved to the peer")
	}
}

// Past the cache TTL there is nothing to protect, so a cold session on a
// below-floor account is still offered to the peer. This is the half of the
// old behavior that was right, and the reason the fix is scoped to warm
// sessions rather than deleting the spill.
func TestColdLocalClaudeSessionOnABelowFloorAccountIsOfferedToThePeer(t *testing.T) {
	server, active := hotAccountFixture(t)
	markIdle(active, "s1", warmSessionIdleTTL+time.Minute)

	_, capacity, offered := offeredToHealthyPeer(t, server, accounts.ProviderClaude, "claude", "s1", claudeMessagesRequest())
	if !capacity.BelowFloor {
		t.Fatal("a cold session on a below-floor account must report the floor to the peer decision")
	}
	if !offered {
		t.Fatal("a cold session on a below-floor account stayed local against a healthy peer")
	}
}

// The owner's holdout is a hard limit for a session already on the account,
// exactly like a spent 5h or weekly window: the warm session is evicted rather
// than spending the holdout down to the real weekly limit.
func holdoutFixture(t *testing.T, heldWeekly float64) (Server, *ActiveSessions, *session.Store) {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "s1", "held", ""); err != nil {
		t.Fatal(err)
	}
	active := NewActiveSessions()
	roll := time.Now().Add(100 * time.Hour)
	server := Server{
		Accounts: []accounts.Account{
			{ID: "held", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "held-token"},
			{ID: "free", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "free-token"},
		},
		Sessions:       store,
		ActiveSessions: active,
		Scheduler: selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "held", Provider: accounts.ProviderClaude, Fresh: true,
				Headroom: heldWeekly, ShortHeadroom: 0.9, WeeklyHeadroom: heldWeekly, WeeklyRemaining: heldWeekly, WeeklyResetAt: roll},
			{AccountID: "free", Provider: accounts.ProviderClaude, Fresh: true,
				Headroom: 0.6, ShortHeadroom: 0.9, WeeklyHeadroom: 0.6, WeeklyRemaining: 0.6, WeeklyResetAt: roll},
		}),
		AccountReserves: map[string]float64{"held": 0.05},
		MaxBodyBytes:    1024,
	}
	return server, active, store
}

func TestWarmSessionIsEvictedWhenItsAccountReachesTheHoldout(t *testing.T) {
	server, active, store := holdoutFixture(t, 0.04)
	end := active.Begin("claude", "s1")
	defer end()

	account, _, _, _, err := server.accountForSessionProvider(accounts.ProviderClaude, "claude", "s1", claudeMessagesRequest())
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != "free" {
		t.Fatalf("a session with a turn in flight kept spending %q inside its holdout", account.ID)
	}
	if got, _ := store.Get("claude", "s1"); got.AccountID != "free" {
		t.Fatalf("assignment = %q after eviction, want free", got.AccountID)
	}
}

func TestWarmSessionAboveTheHoldoutKeepsItsAccount(t *testing.T) {
	// Control for the test above: same fixture, week just above the holdout.
	server, active, _ := holdoutFixture(t, 0.06)
	end := active.Begin("claude", "s1")
	defer end()

	account, _, _, _, err := server.accountForSessionProvider(accounts.ProviderClaude, "claude", "s1", claudeMessagesRequest())
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != "held" {
		t.Fatalf("a warm session above its holdout moved to %q", account.ID)
	}
}

func TestNoSessionStartsInTheHoldout(t *testing.T) {
	server, _, store := holdoutFixture(t, 0.04)
	// Only the holdout account is left: the other one has spent its week.
	server.Scheduler = selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "held", Provider: accounts.ProviderClaude, Fresh: true,
			Headroom: 0.04, ShortHeadroom: 0.9, WeeklyHeadroom: 0.04, WeeklyRemaining: 0.04, WeeklyResetAt: time.Now().Add(100 * time.Hour)},
		{AccountID: "free", Provider: accounts.ProviderClaude, Fresh: true,
			Headroom: 0, ShortHeadroom: 0.9, WeeklyHeadroom: 0, WeeklyResetAt: time.Now().Add(100 * time.Hour)},
	})

	account, _, _, _, err := server.accountForSessionProvider(accounts.ProviderClaude, "claude", "fresh-session", claudeMessagesRequest())
	if err == nil {
		t.Fatalf("a new session started on %q with only the holdout left", account.ID)
	}
	if _, ok := store.Get("claude", "fresh-session"); ok {
		t.Fatal("a refused session was still bound to an account")
	}
}
