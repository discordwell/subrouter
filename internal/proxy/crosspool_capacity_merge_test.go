package proxy

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
)

// The merge of origin/main (PATCHES §27, Codex capacity marks) into the
// cross-pool branch (§31) joined two changes to the SAME sticky-session line:
// main released a warm Codex session from an account that shed its model twice
// in a row, and kept it there when no local account is better
// (keepCapacityEvictedSession); the branch routed every leave through the one
// cross-pool ranking and named WHY the session was ranked in the decision
// line's session_state. Taking either side of that conflict compiles.
//
// This pins the union on a daemon with a peer configured, so the ranking is
// live on the turn:
//
//   - the capacity-evicted session still moves, to the local account that is
//     not shedding (main's behaviour, through the branch's ranking);
//   - the decision line says capacity_evicted, not cold -- the session was not
//     idle, it was released, and an operator reading "cold" on a session that
//     turned seconds ago would chase the warm rule instead of the marks.
//
// The peer publishes nothing measured (its usage path 404s), so it cannot win
// and the turn is served locally; its only role is to make the turn one that
// may go to a peer.
func TestCapacityEvictedSessionIsRankedAndNamedAcrossPools(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == remotePoolUsagePath {
			http.NotFound(w, r)
			return
		}
		t.Errorf("peer was sent a turn (%s); an unmeasured peer must not win", r.URL.Path)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer peer.Close()
	poolURL, seen := codexCapacityPool(t, func(string, string, int) bool { return false })
	server := codexOverloadServer(t, poolURL, 2, true)
	server.SchedulerRef = selectacct.NewSchedulerRef(server.Scheduler)
	server.RemotePool = NewRemotePool(peer.URL, "", "")
	logs := &syncBuffer{}
	server.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	until := time.Now().Add(time.Minute)
	// Two consecutive capacity failures on account 0 for this pool: it is
	// capacity-evicting (CapacityStickyEvictFailures). Account 1 is clean.
	server.SchedulerRef.MarkCapacityUntil(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "", until)
	server.SchedulerRef.MarkCapacityUntil(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "", until)
	if _, err := server.Sessions.Put("codex", "released", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	if status, body := codexCapacityPost(t, proxy.URL, "released", "a"); status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-1") {
		t.Fatalf("status=%d body=%.160q, want the released session served by the account that is not shedding", status, body)
	}
	if got := lastToken(t, seen); got != "oauth-token-1" {
		t.Fatalf("pool saw %v, want the turn on account 1", tokens(seen()))
	}
	if assignment, _ := server.Sessions.Get("codex", "released"); assignment.AccountID != "codex-account-1" {
		t.Fatalf("session pinned to %q, want it moved to codex-account-1", assignment.AccountID)
	}
	decisions := e2eLogLines(logs, "remote pool decision")
	if len(decisions) != 1 {
		t.Fatalf("got %d decision lines, want 1 (the ranking must be live on this turn): %s", len(decisions), logs.String())
	}
	if state := decisions[0]["session_state"]; state != "capacity_evicted" {
		t.Fatalf("session_state = %v, want capacity_evicted: %v", state, decisions[0])
	}
}
