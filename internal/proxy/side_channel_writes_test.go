package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
	"github.com/discordwell/subrouter/session"
)

// The paths below are the ones a side request can take besides "served on
// the pin" and "no pin": an evicted pin, a forced account, a changed user
// email, a machine policy, and the transient-refresh one-turn peer forward.
// Every one of them used to write placement (a pin, a binding or a peer hold)
// on a model turn's behalf; each test here goes red if its guard is dropped.

func sendSideWithHeaders(t *testing.T, handler http.Handler, path string, headers map[string]string, body string) *http.Response {
	t.Helper()
	front := httptest.NewServer(handler)
	t.Cleanup(front.Close)
	req, err := http.NewRequest(http.MethodPost, front.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	return response
}

func codexAnalyticsHeaders(windowID string, extra map[string]string) map[string]string {
	headers := map[string]string{"X-Subrouter-Agent": "codex", "X-Codex-Window-ID": windowID}
	for key, value := range extra {
		headers[key] = value
	}
	return headers
}

// evictedScore has a spent 5h window: the account is evicted for every
// provider, so a side request may not be served there.
func evictedScore(id string, provider accounts.Provider) selectacct.Score {
	return selectacct.Score{
		AccountID: id, Provider: provider, Fresh: true,
		Headroom: 0.5, ShortHeadroom: 0, WeeklyHeadroom: 0.5, WeeklyRemaining: 0.5,
		ShortWindowSeconds: 5 * 60 * 60, ShortResetAfterSeconds: 60 * 60,
		WeeklyResetAt: time.Now().Add(80 * time.Hour),
	}
}

func healthyScore(id string, provider accounts.Provider) selectacct.Score {
	return selectacct.Score{
		AccountID: id, Provider: provider, Fresh: true,
		Headroom: 0.9, ShortHeadroom: 0.9, WeeklyHeadroom: 0.9, WeeklyRemaining: 0.9,
		WeeklyResetAt: time.Now().Add(100 * time.Hour),
	}
}

// An evicted pin is the one placement a side request may not follow. It is
// served on another account, and the pin -- account and UpdatedAt -- stays
// exactly where the last model turn left it: moving the conversation is that
// turn's decision, not telemetry's.
func TestSideRequestOnAnEvictedPinIsServedElsewhereWithoutMovingIt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider accounts.Provider
		agent    string
		fixture  func(*testing.T, *RemotePool) (Server, *session.Store, *sideRecorder, *syncBuffer)
		send     func(*testing.T, http.Handler) *http.Response
	}{
		{
			name: "codex analytics", provider: accounts.ProviderCodex, agent: "codex",
			fixture: coldCodexSideFixture,
			send: func(t *testing.T, handler http.Handler) *http.Response {
				return sendSide(t, handler, http.MethodPost, codexAnalyticsPath, "codex", sideThread+":0", `{"events":[]}`)
			},
		},
		{
			name: "claude count_tokens", provider: accounts.ProviderClaude, agent: "claude",
			fixture: coldClaudeSideFixture,
			send: func(t *testing.T, handler http.Handler) *http.Response {
				return sendCountTokens(t, handler, sideThread)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, store, rec, _ := tc.fixture(t, nil)
			server.Scheduler = selectacct.NewScheduler([]selectacct.Score{
				evictedScore("hot", tc.provider), healthyScore("cool", tc.provider),
			})
			if !server.scheduler().WithReserves(server.AccountReserves).Evicted(tc.provider, "hot") {
				t.Fatal("calibration: hot must be evicted")
			}
			before, ok := store.Get(tc.agent, sideThread)
			if !ok || before.AccountID != "hot" {
				t.Fatalf("calibration: the thread must be pinned to hot, got %+v", before)
			}

			response := tc.send(t, server.Handler())
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status %d, want 200", response.StatusCode)
			}
			if got := rec.served(); len(got) != 1 || !strings.Contains(got[0], "cool-token") {
				t.Fatalf("served with %q, want once on cool (the pinned hot is evicted)", got)
			}
			after, ok := store.Get(tc.agent, sideThread)
			if !ok || after.AccountID != "hot" || !after.UpdatedAt.Equal(before.UpdatedAt) {
				t.Fatalf("pin %+v -> %+v, want it untouched on hot", before, after)
			}
		})
	}
}

// A forced account (X-Subrouter-Account) serves the side request and pins
// nothing: forcing a model turn pins, forcing telemetry must not.
func TestForcedAccountSideRequestWritesNoPin(t *testing.T) {
	server, store, rec, _ := coldCodexSideFixture(t, nil)
	before, _ := store.Get("codex", sideThread)
	response := sendSideWithHeaders(t, server.Handler(), codexAnalyticsPath,
		codexAnalyticsHeaders(sideThread+":0", map[string]string{"X-Subrouter-Account": "cool"}), `{"events":[]}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", response.StatusCode)
	}
	if got := rec.served(); len(got) != 1 || !strings.Contains(got[0], "cool-token") {
		t.Fatalf("served with %q, want once on the forced account cool", got)
	}
	after, _ := store.Get("codex", sideThread)
	if after.AccountID != "hot" || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("pin %+v -> %+v, want it untouched on hot", before, after)
	}
}

// A side request carrying a different user email reads the pin and rewrites
// nothing, not even the email.
func TestEmailChangingSideRequestWritesNoPin(t *testing.T) {
	server, store, rec, _ := coldCodexSideFixture(t, nil)
	before, _ := store.Get("codex", sideThread)
	response := sendSideWithHeaders(t, server.Handler(), codexAnalyticsPath,
		codexAnalyticsHeaders(sideThread+":0", map[string]string{"X-Subrouter-User-Email": "someone-else@example.com"}), `{"events":[]}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", response.StatusCode)
	}
	if got := rec.served(); len(got) != 1 || !strings.Contains(got[0], "hot-token") {
		t.Fatalf("served with %q, want once on the pinned account hot", got)
	}
	after, _ := store.Get("codex", sideThread)
	if after.AccountID != before.AccountID || after.UserEmail != before.UserEmail || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("pin %+v -> %+v, want it untouched", before, after)
	}
}

// Machine bindings record real usage. A side request on a conversation no
// model turn has placed yet is served on a pick and binds no machine; a model
// turn in the same fixture does bind (the calibration that the check can see
// a binding at all).
func TestSideRequestBindsNoMachine(t *testing.T) {
	bindings := func(server Server) int {
		return len(server.MachinePolicy.Bindings(accounts.ProviderCodex, "hot")) +
			len(server.MachinePolicy.Bindings(accounts.ProviderCodex, "cool"))
	}

	calibration, _, _, _ := coldCodexSideFixture(t, nil)
	calibration.MachinePolicy = NewMachinePolicy(t.TempDir(), MachinePolicyObserve, true)
	const modelThread = "019a4d2e-0000-7000-8000-00000000000a"
	sendSideWithHeaders(t, calibration.Handler(), "/v1/responses", codexAnalyticsHeaders(modelThread+":0", nil), `{}`)
	if bindings(calibration) == 0 {
		t.Fatal("calibration: a model turn bound no machine; this test cannot see a binding")
	}

	server, store, rec, _ := coldCodexSideFixture(t, nil)
	server.MachinePolicy = NewMachinePolicy(t.TempDir(), MachinePolicyObserve, true)
	const sideOnlyThread = "019a4d2e-0000-7000-8000-000000000009"
	response := sendSideWithHeaders(t, server.Handler(), codexAnalyticsPath, codexAnalyticsHeaders(sideOnlyThread+":0", nil), `{"events":[]}`)
	if response.StatusCode != http.StatusOK || len(rec.served()) != 1 {
		t.Fatalf("status %d, local %d: want the side request served once", response.StatusCode, len(rec.served()))
	}
	if n := bindings(server); n != 0 {
		t.Fatalf("a side request bound %d machine(s)", n)
	}
	if _, ok := store.Get("codex", sideOnlyThread); ok {
		t.Fatal("a side request pinned a conversation no model turn has placed")
	}
}

// claudeSidePeer answers every forwarded request with status (200 naming its
// account when zero) and records whether the session was already held when
// each request arrived.
type claudeSidePeer struct {
	hits          atomic.Int32
	heldAtArrival atomic.Bool
}

func newClaudeSidePeer(t *testing.T, status int) (*RemotePool, *claudeSidePeer) {
	t.Helper()
	peer := &claudeSidePeer{}
	var pool *RemotePool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer.hits.Add(1)
		if pool.delegations.active(session.ScopedSessionKey("claude", "s1"), time.Now()) {
			peer.heldAtArrival.Store(true)
		}
		if status != 0 {
			http.Error(w, "peer says no", status)
			return
		}
		w.Header().Set(subrouterAccountHeader, "peer-acct")
		_, _ = io.WriteString(w, `{"input_tokens":1}`)
	}))
	t.Cleanup(srv.Close)
	pool = NewRemotePool(srv.URL, "", "")
	return pool, peer
}

func sendClaudeCountTokensOnS1(t *testing.T, handler http.Handler) *http.Response {
	t.Helper()
	return sendSideWithHeaders(t, handler, "/v1/messages/count_tokens",
		map[string]string{"X-Subrouter-Agent": "claude", "X-Subrouter-Session": "s1"},
		`{"model":"claude-opus-4-8","messages":[]}`)
}

// A side request whose pinned account's token refresh reaches no verdict is
// served by the peer for that one request, and the peer gets no hold for it:
// no claim before the request leaves, no set after it answers, and the pin
// stays put.
func TestTransientRefreshSideRequestGivesThePeerNoHold(t *testing.T) {
	pool, peer := newClaudeSidePeer(t, 0)
	server, store := transientRefreshServer(t, pool)
	key := session.ScopedSessionKey("claude", "s1")

	response := sendClaudeCountTokensOnS1(t, server.Handler())
	if response.StatusCode != http.StatusOK || peer.hits.Load() != 1 {
		t.Fatalf("status %d, peer hits %d: want the side request served once by the peer", response.StatusCode, peer.hits.Load())
	}
	if peer.heldAtArrival.Load() {
		t.Fatal("a side request claimed the session for the peer before it answered")
	}
	if pool.delegations.active(key, time.Now()) {
		t.Fatal("a side request on a transient refresh gave the peer a hold")
	}
	if got, _ := store.Get("claude", "s1"); got.AccountID != "a" {
		t.Fatalf("pin moved to %q, want it on a", got.AccountID)
	}
}

// The same path on a conversation the peer already holds: the peer's 429 is
// the side endpoint's answer. The hold survives both forwards (the delegated
// follow and the transient one-turn), with its idle clock untouched.
func TestTransientRefreshSideRequestKeepsAPeerHoldThroughA429(t *testing.T) {
	pool, peer := newClaudeSidePeer(t, http.StatusTooManyRequests)
	server, _ := transientRefreshServer(t, pool)
	key := session.ScopedSessionKey("claude", "s1")
	pool.delegations.set(key, time.Now().Add(-10*time.Minute), remotePoolDelegationTTLFor("claude"), "peer-acct", pool.name())
	held, ok := pool.delegations.lookup(key, time.Now())
	if !ok {
		t.Fatal("calibration: the hold must be live")
	}

	sendClaudeCountTokensOnS1(t, server.Handler())
	if peer.hits.Load() != 2 {
		t.Fatalf("calibration: peer hits %d, want 2 (the held follow, then the transient one-turn forward)", peer.hits.Load())
	}
	now, ok := pool.delegations.lookup(key, time.Now())
	if !ok {
		t.Fatal("a side request's 429 on the transient path ended the conversation's peer hold")
	}
	if !now.Until.Equal(held.Until) || now.Account != held.Account {
		t.Fatalf("a side request changed the hold: %+v -> %+v", held, now)
	}
}

// A peer declining a side request says nothing about the conversation and
// repeats on every telemetry POST, so it is logged at Info; a model turn's
// decline ends the hold and stays a Warn.
func TestRemotePoolSideDeclineLogsAtInfo(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mode  remotePoolHoldMode
		level string
	}{
		{name: "side request", mode: remotePoolHoldReadOnly, level: "INFO"},
		{name: "model turn", mode: remotePoolHoldSet, level: "WARN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, peer := newSidePeer(t, http.StatusTooManyRequests)
			logs := &syncBuffer{}
			server := Server{RemotePool: pool, Logger: slog.New(slog.NewTextHandler(logs, nil))}
			body := []byte(`{"events":[]}`)
			req := httptest.NewRequest(http.MethodPost, codexAnalyticsPath, strings.NewReader(string(body)))
			if _, served := server.remotePoolForwardMode(req, "codex", sideThread, codexAnalyticsPath, "", body, tc.mode); served {
				t.Fatal("a declined forward reported as served")
			}
			if peer.forwarded() != 1 {
				t.Fatalf("calibration: peer received %d requests, want 1", peer.forwarded())
			}
			var declined string
			for _, line := range strings.Split(logs.String(), "\n") {
				if strings.Contains(line, `msg="remote pool declined"`) {
					declined = line
				}
			}
			if declined == "" {
				t.Fatalf("no decline logged:\n%s", logs.String())
			}
			if !strings.Contains(declined, "level="+tc.level+" ") {
				t.Fatalf("decline logged as %q, want level=%s", declined, tc.level)
			}
		})
	}
}
