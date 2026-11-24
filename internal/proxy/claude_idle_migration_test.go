package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
	"github.com/discordwell/subrouter/session"
)

func claudePost(t *testing.T, path string) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodPost, path, nil)
}

// Claude turns were invisible to the in-flight tracker: activeSessionRequest
// short-circuited on anything that was not Codex. That kept LiveSessions at
// zero for every Claude account, which is the input the endgame taper runs on.
func TestActiveSessionRequestCountsClaudeTurns(t *testing.T) {
	cases := []struct {
		name  string
		agent string
		req   *http.Request
		want  bool
	}{
		{"claude messages", "claude", claudePost(t, "/v1/messages"), true},
		{"claude unversioned", "claude", claudePost(t, "/messages"), true},
		{"claude count_tokens", "claude", claudePost(t, "/v1/messages/count_tokens"), false},
		{"claude get", "claude", httptest.NewRequest(http.MethodGet, "/v1/messages", nil), false},
		{"codex responses", "codex", claudePost(t, "/v1/responses"), true},
		{"codex messages", "codex", claudePost(t, "/v1/messages"), false},
		{"unknown agent", "gemini", claudePost(t, "/v1/messages"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := activeSessionRequest(tc.agent, tc.req); got != tc.want {
				t.Fatalf("activeSessionRequest(%q, %s) = %v, want %v", tc.agent, tc.req.URL.Path, got, tc.want)
			}
		})
	}
}

func TestActiveSessionCountsByAccountSeesClaudeSessions(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "s1", "acct-a", ""); err != nil {
		t.Fatal(err)
	}
	active := NewActiveSessions()
	server := Server{Sessions: store, ActiveSessions: active}

	if got := server.activeSessionCountsByAccount()["acct-a"]; got != 0 {
		t.Fatalf("idle account live count = %d, want 0", got)
	}
	end := active.Begin("claude", "s1")
	if got := server.activeSessionCountsByAccount()["acct-a"]; got != 1 {
		t.Fatalf("in-flight claude session live count = %d, want 1", got)
	}
	end()
	if got := server.activeSessionCountsByAccount()["acct-a"]; got != 0 {
		t.Fatalf("released claude session live count = %d, want 0", got)
	}
}

func TestIdleSinceUnknownUntilFirstTurn(t *testing.T) {
	active := NewActiveSessions()
	if _, seen := active.IdleSince("claude", "s1"); seen {
		t.Fatal("a session with no observed turn must report unknown, not zero idle")
	}
	active.Begin("claude", "s1")()
	idle, seen := active.IdleSince("claude", "s1")
	if !seen {
		t.Fatal("expected a last-seen stamp after a turn")
	}
	if idle > time.Minute {
		t.Fatalf("idle = %s, want ~0 immediately after a turn", idle)
	}

	var nilSessions *ActiveSessions
	if _, seen := nilSessions.IdleSince("claude", "s1"); seen {
		t.Fatal("nil tracker must report unknown rather than panic")
	}
}

func TestSweepLastSeenBoundsTheMap(t *testing.T) {
	active := NewActiveSessions()
	stale := time.Now().Add(-2 * lastSeenRetention)
	for i := 0; i < lastSeenSweepThreshold+10; i++ {
		active.lastSeen[session.ScopedSessionKey("claude", "stale-"+string(rune('a'+i%26))+time.Duration(i).String())] = stale
	}
	before := len(active.lastSeen)
	active.Begin("claude", "live")()
	if len(active.lastSeen) >= before {
		t.Fatalf("lastSeen did not shrink: %d -> %d", before, len(active.lastSeen))
	}
	if _, ok := active.lastSeen[session.ScopedSessionKey("claude", "live")]; !ok {
		t.Fatal("sweep must not drop the session that triggered it")
	}
}

// endgameScores builds a two-account pool where `draining` is inside its weekly
// roll window with budget left and `steady` is days away from rolling.
func endgameScores(drainingID, steadyID string) []selectacct.Score {
	return []selectacct.Score{
		{
			AccountID: drainingID, Provider: accounts.ProviderClaude,
			Headroom: 1, ShortHeadroom: 1,
			WeeklyHeadroom: 0.5, WeeklyResetAt: time.Now().Add(3 * time.Hour),
		},
		{
			AccountID: steadyID, Provider: accounts.ProviderClaude,
			Headroom: 1, ShortHeadroom: 1,
			WeeklyHeadroom: 0.5, WeeklyResetAt: time.Now().Add(120 * time.Hour),
		},
	}
}

func migrationFixture(t *testing.T, scores []selectacct.Score) (Server, *ActiveSessions, []accounts.Account) {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	active := NewActiveSessions()
	available := []accounts.Account{
		{ID: "draining", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
		{ID: "steady", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
	}
	_ = scores
	return Server{Sessions: store, ActiveSessions: active}, active, available
}

func markIdle(active *ActiveSessions, sessionID string, idle time.Duration) {
	active.lastSeen[session.ScopedSessionKey("claude", sessionID)] = time.Now().Add(-idle)
}

func TestIdleClaudeSessionMigratesToDrainingAccount(t *testing.T) {
	scores := endgameScores("draining", "steady")
	server, active, available := migrationFixture(t, scores)
	scheduler := selectacct.NewScheduler(scores)
	steady := available[1]

	markIdle(active, "s1", warmSessionIdleTTL+time.Minute)
	if server.reuseStickyAssignment("claude", "s1", steady, scheduler, available) {
		t.Fatal("an idle session on a steady account should release its binding when an account is draining")
	}
}

func TestWarmClaudeSessionKeepsItsAccount(t *testing.T) {
	scores := endgameScores("draining", "steady")
	server, active, available := migrationFixture(t, scores)
	scheduler := selectacct.NewScheduler(scores)
	steady := available[1]

	// One minute under the threshold: the upstream prompt cache is plausibly
	// still warm, and moving would force a full re-write on another org.
	markIdle(active, "s1", warmSessionIdleTTL-time.Minute)
	if !server.reuseStickyAssignment("claude", "s1", steady, scheduler, available) {
		t.Fatal("a session inside the cache TTL must keep its account")
	}
}

func TestUnseenClaudeSessionKeepsItsAccount(t *testing.T) {
	scores := endgameScores("draining", "steady")
	server, _, available := migrationFixture(t, scores)
	scheduler := selectacct.NewScheduler(scores)
	steady := available[1]

	// No turn observed: most likely a daemon restart under a live upstream
	// cache. Unknown must read as warm, never as infinitely idle.
	if !server.reuseStickyAssignment("claude", "s1", steady, scheduler, available) {
		t.Fatal("a session with no last-seen stamp must keep its account")
	}
}

// Migration used to be one-directional: an idle session moved only toward an
// account inside its weekly roll window, and otherwise stayed put however badly
// it was placed. Past the cache TTL there is no warmth left to trade away, so a
// dormant session now re-ranks against the whole pool like any unbound one --
// both accounts here are days from rolling and it still moves to the better of
// the two.
func TestIdleClaudeSessionReRanksFreelyPastTheCacheTTL(t *testing.T) {
	scores := []selectacct.Score{
		{AccountID: "sooner", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1,
			WeeklyHeadroom: 0.5, WeeklyResetAt: time.Now().Add(100 * time.Hour)},
		{AccountID: "later", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1,
			WeeklyHeadroom: 0.5, WeeklyResetAt: time.Now().Add(120 * time.Hour)},
	}
	available := []accounts.Account{
		{ID: "sooner", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
		{ID: "later", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
	}
	server, active, _ := migrationFixture(t, scores)
	scheduler := selectacct.NewScheduler(scores)

	// Equal budgets, unequal deadlines: "sooner" carries the higher weekly pace
	// and neither account is anywhere near its endgame window.
	markIdle(active, "s1", 48*time.Hour)
	if server.reuseStickyAssignment("claude", "s1", available[1], scheduler, available) {
		t.Fatal("an idle session on the slower-pace account should re-rank onto the faster one")
	}
	if !server.reuseStickyAssignment("claude", "s1", available[0], scheduler, available) {
		t.Fatal("an idle session already on the winner has nowhere better to go and must not churn")
	}
}

// The case the old direction rule got backwards. The comparator demotes an
// account at its endgame concurrency cap BELOW every routable account, so the
// better destination for a session stranded on one is a steady account by
// construction -- and "the destination must itself be draining" is exactly the
// rule that refused that move. The taper could shed load onto new sessions and
// never off the sessions already there.
func TestIdleSessionLeavesADrainingAccountThatIsAtItsCap(t *testing.T) {
	scores := []selectacct.Score{
		{AccountID: "capped", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1,
			WeeklyHeadroom: 0.05, WeeklyResetAt: time.Now().Add(3 * time.Hour), LiveSessions: 4},
		{AccountID: "steady", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1,
			WeeklyHeadroom: 0.9, WeeklyResetAt: time.Now().Add(120 * time.Hour)},
	}
	available := []accounts.Account{
		{ID: "capped", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
		{ID: "steady", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
	}
	server, active, _ := migrationFixture(t, scores)
	scheduler := selectacct.NewScheduler(scores)

	markIdle(active, "s1", 48*time.Hour)
	if server.reuseStickyAssignment("claude", "s1", available[0], scheduler, available) {
		t.Fatal("an idle session must be able to leave a drain account that is already at its concurrency cap")
	}
}

func TestSessionAlreadyOnDrainingAccountIsNotMoved(t *testing.T) {
	scores := endgameScores("draining", "steady")
	server, active, available := migrationFixture(t, scores)
	scheduler := selectacct.NewScheduler(scores)
	draining := available[0]

	markIdle(active, "s1", 48*time.Hour)
	if !server.reuseStickyAssignment("claude", "s1", draining, scheduler, available) {
		t.Fatal("a session already on the draining account has nowhere better to go")
	}
}

func TestInFlightClaudeSessionIsNeverMigrated(t *testing.T) {
	scores := endgameScores("draining", "steady")
	server, active, available := migrationFixture(t, scores)
	scheduler := selectacct.NewScheduler(scores)
	steady := available[1]

	markIdle(active, "s1", 48*time.Hour)
	end := active.Begin("claude", "s1")
	defer end()
	if !server.reuseStickyAssignment("claude", "s1", steady, scheduler, available) {
		t.Fatal("a session with work in flight must never be re-pointed mid-turn")
	}
}

func TestMigrationNeverSelectsAnExhaustedAccount(t *testing.T) {
	scores := []selectacct.Score{
		{AccountID: "draining", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0,
			WeeklyHeadroom: 0, WeeklyResetAt: time.Now().Add(3 * time.Hour)},
		{AccountID: "steady", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1,
			WeeklyHeadroom: 0.5, WeeklyResetAt: time.Now().Add(120 * time.Hour)},
	}
	server, active, available := migrationFixture(t, scores)
	scheduler := selectacct.NewScheduler(scores)
	steady := available[1]

	markIdle(active, "s1", 48*time.Hour)
	if !server.reuseStickyAssignment("claude", "s1", steady, scheduler, available) {
		t.Fatal("an exhausted account is not a drain target, however close its roll")
	}
}

// lastSeen is in-memory and a daemon restart wipes it. The assignment's
// UpdatedAt is NOT a substitute: it is when the session was last ASSIGNED, not
// last used, so a session placed 48h ago and busy every minute read as 48h
// idle after every restart and was moved off a warm cache. What a restarted
// daemon does know is its own uptime: any turn since start would have been
// seen, so an unseen session is possibly-warm for one idle bound after the
// restart and genuinely idle after that.
func restartedStore(t *testing.T, assignedAgo time.Duration) *session.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sessions.json")
	store, err := session.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "s1", "steady", ""); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var entries map[string]session.Assignment
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-assignedAgo)
	for key, assignment := range entries {
		assignment.CreatedAt, assignment.UpdatedAt = stamp, stamp
		entries[key] = assignment
	}
	out, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded, err := session.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	return reloaded
}

func restartFixture() ([]accounts.Account, selectacct.Scheduler) {
	available := []accounts.Account{
		{ID: "draining", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
		{ID: "steady", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
	}
	return available, selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "draining", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1,
			WeeklyHeadroom: 0.5, WeeklyResetAt: time.Now().Add(6 * time.Hour)},
		{AccountID: "steady", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1,
			WeeklyHeadroom: 0.5, WeeklyResetAt: time.Now().Add(120 * time.Hour)},
	})
}

func TestSessionUnseenSinceARecentRestartKeepsItsAccount(t *testing.T) {
	available, scheduler := restartFixture()
	// Assigned 48h ago, and this process -- just restarted -- has seen no turn.
	// It may have taken one a second before the restart.
	server := Server{Sessions: restartedStore(t, 48*time.Hour), ActiveSessions: NewActiveSessions()}
	if !server.reuseStickyAssignment("claude", "s1", available[1], scheduler, available) {
		t.Fatal("a possibly-warm session was moved because it was ASSIGNED 48h ago; " +
			"assignment time says nothing about its last turn")
	}
}

func TestSessionUnseenForAFullIdleBoundAfterRestartReRanks(t *testing.T) {
	available, scheduler := restartFixture()
	active := NewActiveSessions()
	active.startedAt = time.Now().Add(-(warmSessionIdleTTL + time.Minute))
	server := Server{Sessions: restartedStore(t, 48*time.Hour), ActiveSessions: active}
	if server.reuseStickyAssignment("claude", "s1", available[1], scheduler, available) {
		t.Fatal("a session with no turn for a full idle bound of uptime was still held")
	}
}
