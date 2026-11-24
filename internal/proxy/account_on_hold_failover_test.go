package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
)

// An UNPINNED request whose account refreshes to Anthropic's account_on_hold
// (a 403 from the token endpoint) fails over locally and marks the account
// (refreshFailureCannotServe, PATCHES §35). Before, the refusal was neither
// transient nor on the failover gate: refreshSelectedAccount handed it back
// unmarked, the turn ended in the handler's no-account 503, and the next pick
// could land on the same account (the gap §29.2 recorded). A caller's own pin
// on it still gets §29.2's final 403 (TestPinnedClaudeAccountWithDeadCredentialFailsFast).
func TestUnpinnedAccountOnHoldFailsOverAndIsMarked(t *testing.T) {
	var mu sync.Mutex
	var served []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		served = append(served, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","content":[]}`)
	}))
	defer upstream.Close()
	servedSoFar := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), served...)
	}

	server, store := claudeFailoverServer(t)
	server.ClaudeUpstream = mustParseURL(t, upstream.URL)
	server.MaxBodyBytes = 1 << 20
	// Both accounts measure healthy, the held one best: nothing but the
	// refresh's refusal can take it out of routing.
	server.ScoreAccounts = func(_ context.Context, available []accounts.Account) ([]selectacct.Score, int) {
		scores := make([]selectacct.Score, 0, len(available))
		for _, account := range available {
			headroom := 0.6
			if account.ID == "cooked@example.com" {
				headroom = 0.9
			}
			scores = append(scores, selectacct.Score{AccountID: account.ID, Provider: accounts.ProviderClaude,
				Headroom: headroom, ShortHeadroom: headroom, WeeklyRemaining: headroom})
		}
		return scores, len(scores)
	}
	var refreshed []string
	server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		refreshed = append(refreshed, account.ID)
		if account.ID == "cooked@example.com" {
			return account, errors.New(`Claude OAuth refresh failed: 403 Forbidden: {"error": {"type": "account_on_hold"}}`)
		}
		return account, nil
	}
	// A warm session whose account Anthropic has just put on hold.
	if _, err := store.Put("claude", "pinned-session", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, pinnedClaudeRequest(""))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want the turn served by another account; body = %s", response.Code, response.Body.String())
	}
	if got := servedSoFar(); len(got) != 1 || got[0] != "tok-fresh" {
		t.Fatalf("upstream saw %v, want exactly one request, on fresh@example.com", got)
	}
	if len(refreshed) != 2 || refreshed[0] != "cooked@example.com" || refreshed[1] != "fresh@example.com" {
		t.Fatalf("refreshes = %v, want the held account, then the replacement", refreshed)
	}
	if !server.SchedulerRef.Get().Exhausted(accounts.ProviderClaude, "cooked@example.com") {
		t.Fatal("the account on hold is still routable after the failover")
	}
	// A spent credential is a forced switch: the session sticks to the
	// account that served it (P5).
	if assignment, ok := store.Get("claude", "pinned-session"); !ok || assignment.AccountID != "fresh@example.com" {
		t.Fatalf("session assignment = %+v, want it moved to fresh@example.com", assignment)
	}

	// The next new session is not placed on the held account: no refresh of
	// it, and the turn goes straight to the replacement.
	refreshed = nil
	next := pinnedClaudeRequest("")
	next.Header.Set("X-Claude-Code-Session-Id", "another-session")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, next)
	if response.Code != http.StatusOK {
		t.Fatalf("new session status = %d; body = %s", response.Code, response.Body.String())
	}
	for _, id := range refreshed {
		if id == "cooked@example.com" {
			t.Fatalf("refreshes = %v; a new session was placed on the account on hold", refreshed)
		}
	}
	if got := servedSoFar(); len(got) != 2 || got[1] != "tok-fresh" {
		t.Fatalf("upstream saw %v, want the new session on fresh@example.com", got)
	}
}
