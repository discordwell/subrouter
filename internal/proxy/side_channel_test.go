package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// Side requests read a conversation's placement and never write it. Codex
// posts telemetry on the thread's own key (49,724 analytics POSTs against
// 39,407 model turns on the box, 2026-09-17..24) and Claude Code counts tokens
// on it; both ran full selection, so a cold burn projection re-pinned the
// conversation between turns (2,029 of 4,893 box moves) and an unpinned one
// could hand the whole thread to the peer with a hold. Only a model turn
// decides where a conversation lives.

const sideThread = "019a4d2e-7c1b-7f00-9c3e-5b2a1d0e8f41"

// sideRecorder is the local upstream: it records which credential served each
// request.
type sideRecorder struct {
	mu    sync.Mutex
	auths []string
}

func (r *sideRecorder) served() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.auths...)
}

func newSideUpstream(t *testing.T) (*httptest.Server, *sideRecorder) {
	t.Helper()
	rec := &sideRecorder{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.auths = append(rec.auths, r.Header.Get("Authorization"))
		rec.mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(upstream.Close)
	return upstream, rec
}

// sidePeer is a measured, healthy peer that counts the requests forwarded to
// it and answers them with status (200 when zero).
type sidePeer struct {
	mu     sync.Mutex
	hits   int
	status int
}

func (p *sidePeer) forwarded() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits
}

func newSidePeer(t *testing.T, status int) (*RemotePool, *sidePeer) {
	t.Helper()
	peer := &sidePeer{status: status}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == remotePoolUsagePath {
			_, _ = io.WriteString(w, measuredPeer)
			return
		}
		peer.mu.Lock()
		peer.hits++
		status := peer.status
		peer.mu.Unlock()
		if status != 0 {
			http.Error(w, "peer says no", status)
			return
		}
		w.Header().Set(subrouterAccountHeader, "peer-acct")
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	return NewRemotePool(srv.URL, "", "box"), peer
}

// coldCodexSideFixture is hotCodexFixture served through the handler, with
// sideThread pinned to the burn-refused "hot" account and the tracker up for
// a full idle bound: the session is COLD, so the sticky rule would move it off
// "hot" on its next model turn. A side request must not be that turn.
func coldCodexSideFixture(t *testing.T, pool *RemotePool) (Server, *session.Store, *sideRecorder, *syncBuffer) {
	t.Helper()
	server, active, store := hotCodexFixture(t, burnRefusedCodexScore())
	active.startedAt = time.Now().Add(-(71 * time.Minute))
	if _, err := store.Put("codex", sideThread, "hot", ""); err != nil {
		t.Fatal(err)
	}
	upstream, rec := newSideUpstream(t)
	server.Upstream = mustURL(t, upstream.URL)
	server.RemotePool = pool
	logs := &syncBuffer{}
	server.Logger = slog.New(slog.NewTextHandler(logs, nil))
	return server, store, rec, logs
}

func sendSide(t *testing.T, handler http.Handler, method, path, agent, windowID, body string) *http.Response {
	t.Helper()
	subrouter := httptest.NewServer(handler)
	t.Cleanup(subrouter.Close)
	req, err := http.NewRequest(method, subrouter.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", agent)
	if windowID != "" {
		req.Header.Set("X-Codex-Window-ID", windowID)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	return response
}

const codexAnalyticsPath = "/backend-api/codex/analytics-events/events"

func assertNoStickyDecision(t *testing.T, logs *syncBuffer, why string) {
	t.Helper()
	for _, line := range []string{"rerouting cold sticky session", "re-ranking idle session", "remote pool decision"} {
		if strings.Contains(logs.String(), line) {
			t.Fatalf("%s: a side request ran the model turn's placement logic (%q logged)", why, line)
		}
	}
}

// (a) A pinned conversation's telemetry is served on its account, and the
// pin -- account and UpdatedAt -- is exactly what the last model turn left.
func TestCodexSideRequestServedOnThePinWithoutRewritingIt(t *testing.T) {
	server, store, rec, logs := coldCodexSideFixture(t, nil)
	scheduler := server.scheduler().WithReserves(server.AccountReserves)
	if scheduler.UsableForNewSession(accounts.ProviderCodex, "hot") || scheduler.Evicted(accounts.ProviderCodex, "hot") {
		t.Fatal("calibration: hot must be burn-refused and NOT evicted")
	}
	before, ok := store.Get("codex", sideThread)
	if !ok {
		t.Fatal("calibration: the thread must start pinned")
	}

	response := sendSide(t, server.Handler(), http.MethodPost, codexAnalyticsPath, "codex", sideThread+":0", `{"events":[]}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", response.StatusCode)
	}
	if got := rec.served(); len(got) != 1 || !strings.Contains(got[0], "hot-token") {
		t.Fatalf("served with %q, want once on the pinned account hot", got)
	}
	after, ok := store.Get("codex", sideThread)
	if !ok || after.AccountID != "hot" {
		t.Fatalf("pin = %+v, want it left on hot", after)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("pin rewritten: UpdatedAt %v -> %v", before.UpdatedAt, after.UpdatedAt)
	}
	assertNoStickyDecision(t, logs, "pinned analytics POST")
}

// (b) A conversation with no pin yet: the side request is served, creates no
// pin, never asks the comparator and never hands the thread to the peer. On
// the box an unpinned analytics POST did exactly that and the 2h hold pulled
// the next websocket turn after it.
func TestCodexSideRequestWithNoPinCreatesNothing(t *testing.T) {
	pool, peer := newSidePeer(t, 0)
	server, store, rec, logs := coldCodexSideFixture(t, pool)
	const fresh = "019a4d2e-0000-7000-8000-000000000001"

	response := sendSide(t, server.Handler(), http.MethodPost, codexAnalyticsPath, "codex", fresh+":0", `{"events":[]}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", response.StatusCode)
	}
	if got := rec.served(); len(got) != 1 {
		t.Fatalf("local upstream served %d requests, want 1", len(got))
	}
	if got := peer.forwarded(); got != 0 {
		t.Fatalf("peer received %d side requests, want 0", got)
	}
	if got, ok := store.Get("codex", fresh); ok {
		t.Fatalf("a side request pinned the conversation to %q", got.AccountID)
	}
	if pool.delegations.active(session.ScopedSessionKey("codex", fresh), time.Now()) {
		t.Fatal("a side request gave the peer a hold on the conversation")
	}
	assertNoStickyDecision(t, logs, "unpinned analytics POST")
}

// (c) A conversation held on the peer: its side requests follow the hold but
// never refresh it (no set, no touch), and a 429 from the side endpoint is that
// endpoint's answer -- it does not end the conversation's hold.
func TestCodexSideRequestFollowsAPeerHoldWithoutWritingIt(t *testing.T) {
	for _, tc := range []struct {
		name       string
		peerStatus int
		wantPeer   bool
	}{
		{name: "peer serves", peerStatus: 0, wantPeer: true},
		{name: "peer answers 429", peerStatus: http.StatusTooManyRequests},
		{name: "peer answers 503", peerStatus: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, peer := newSidePeer(t, tc.peerStatus)
			server, store, rec, logs := coldCodexSideFixture(t, pool)
			key := session.ScopedSessionKey("codex", sideThread)
			pool.delegations.set(key, time.Now().Add(-10*time.Minute), remotePoolDelegationTTLFor("codex"), "peer-acct", "box")
			held, ok := pool.delegations.lookup(key, time.Now())
			if !ok {
				t.Fatal("calibration: the hold must be live")
			}
			pinned, _ := store.Get("codex", sideThread)

			response := sendSide(t, server.Handler(), http.MethodPost, codexAnalyticsPath, "codex", sideThread+":0", `{"events":[]}`)
			if tc.wantPeer {
				if response.StatusCode != http.StatusOK || peer.forwarded() != 1 || len(rec.served()) != 0 {
					t.Fatalf("status %d, peer %d, local %d: want the side request served by the peer that holds the thread",
						response.StatusCode, peer.forwarded(), len(rec.served()))
				}
			} else if peer.forwarded() != 1 {
				t.Fatalf("peer received %d requests, want 1 (the hold is followed first)", peer.forwarded())
			}
			now, ok := pool.delegations.lookup(key, time.Now())
			if !ok {
				t.Fatal("a side request ended the conversation's peer hold")
			}
			if !now.Until.Equal(held.Until) {
				t.Fatalf("a side request refreshed the hold: Until %v -> %v", held.Until, now.Until)
			}
			after, _ := store.Get("codex", sideThread)
			if after.AccountID != pinned.AccountID || !after.UpdatedAt.Equal(pinned.UpdatedAt) {
				t.Fatalf("local pin rewritten: %+v -> %+v", pinned, after)
			}
			assertNoStickyDecision(t, logs, "held analytics POST")
		})
	}
}

// coldClaudeSideFixture: sideThread pinned to a burn-refused Claude account,
// cold, with a healthy second account the idle re-rank would move it to.
func coldClaudeSideFixture(t *testing.T, pool *RemotePool) (Server, *session.Store, *sideRecorder, *syncBuffer) {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", sideThread, "hot", ""); err != nil {
		t.Fatal(err)
	}
	active := NewActiveSessions()
	active.startedAt = time.Now().Add(-(71 * time.Minute))
	hot := burnRefusedCodexScore()
	hot.AccountID, hot.Provider, hot.Fresh = "hot", accounts.ProviderClaude, true
	upstream, rec := newSideUpstream(t)
	logs := &syncBuffer{}
	server := Server{
		Upstream: mustURL(t, upstream.URL),
		Accounts: []accounts.Account{
			{ID: "hot", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "hot-token"},
			{ID: "cool", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "cool-token"},
		},
		Sessions:       store,
		ActiveSessions: active,
		Scheduler: selectacct.NewScheduler([]selectacct.Score{hot, {
			AccountID: "cool", Provider: accounts.ProviderClaude, Fresh: true,
			Headroom: 0.9, ShortHeadroom: 0.9, WeeklyHeadroom: 0.9, WeeklyRemaining: 0.9,
			WeeklyResetAt: time.Now().Add(100 * time.Hour),
		}}),
		RemotePool:   pool,
		Logger:       slog.New(slog.NewTextHandler(logs, nil)),
		MaxBodyBytes: 1 << 20,
	}
	return server, store, rec, logs
}

func sendCountTokens(t *testing.T, handler http.Handler, sessionID string) *http.Response {
	t.Helper()
	subrouter := httptest.NewServer(handler)
	t.Cleanup(subrouter.Close)
	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/messages/count_tokens",
		strings.NewReader(`{"model":"claude-sonnet-4-5","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Claude-Code-Session-Id", sessionID)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	return response
}

// (d) Claude count_tokens, pinned: served on the pin, which the idle re-rank
// would otherwise have moved, and nothing about the pin changes.
func TestClaudeCountTokensServedOnThePinWithoutRewritingIt(t *testing.T) {
	server, store, rec, logs := coldClaudeSideFixture(t, nil)
	scheduler := server.scheduler().WithReserves(server.AccountReserves)
	if scheduler.Evicted(accounts.ProviderClaude, "hot") {
		t.Fatal("calibration: hot must not be evicted")
	}
	if best, err := scheduler.Pick(server.Accounts); err != nil || best.ID != "cool" {
		t.Fatalf("calibration: the idle re-rank must prefer cool, got %q (%v)", best.ID, err)
	}
	before, _ := store.Get("claude", sideThread)

	response := sendCountTokens(t, server.Handler(), sideThread)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", response.StatusCode)
	}
	if got := rec.served(); len(got) != 1 || !strings.Contains(got[0], "hot-token") {
		t.Fatalf("served with %q, want once on the pinned account hot", got)
	}
	after, ok := store.Get("claude", sideThread)
	if !ok || after.AccountID != "hot" || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("pin %+v -> %+v, want it untouched on hot", before, after)
	}
	assertNoStickyDecision(t, logs, "pinned count_tokens")
}

// (d) Claude count_tokens with no pin: served, nothing pinned, no comparator,
// no peer hold.
func TestClaudeCountTokensWithNoPinCreatesNothing(t *testing.T) {
	pool, peer := newSidePeer(t, 0)
	server, store, rec, logs := coldClaudeSideFixture(t, pool)
	const fresh = "5f1c2d3e-4b5a-4c6d-8e7f-000000000002"

	response := sendCountTokens(t, server.Handler(), fresh)
	if response.StatusCode != http.StatusOK || len(rec.served()) != 1 || peer.forwarded() != 0 {
		t.Fatalf("status %d, local %d, peer %d: want served once locally", response.StatusCode, len(rec.served()), peer.forwarded())
	}
	if got, ok := store.Get("claude", fresh); ok {
		t.Fatalf("count_tokens pinned the conversation to %q", got.AccountID)
	}
	if pool.delegations.active(session.ScopedSessionKey("claude", fresh), time.Now()) {
		t.Fatal("count_tokens gave the peer a hold on the conversation")
	}
	assertNoStickyDecision(t, logs, "unpinned count_tokens")
}

// (e) Keyless Codex side traffic is not a conversation and keeps today's one
// shared codex:fallback pin: it is what keeps the account the client's
// identity calls see stable.
func TestFallbackKeyedCodexRequestsStillPin(t *testing.T) {
	server, store, rec, _ := coldCodexSideFixture(t, nil)
	response := sendSide(t, server.Handler(), http.MethodGet, "/v1/models", "codex", "", "")
	if response.StatusCode != http.StatusOK || len(rec.served()) != 1 {
		t.Fatalf("status %d, local %d: want /v1/models served once", response.StatusCode, len(rec.served()))
	}
	if _, ok := store.Get("codex", "fallback"); !ok {
		t.Fatal("a fallback-keyed request no longer pins codex:fallback")
	}
}

// (f) Compact is a model turn, not a side request: a cold conversation's
// compact still runs the sticky rule and moves off the burn-refused account.
func TestCodexCompactIsAModelTurn(t *testing.T) {
	server, store, rec, _ := coldCodexSideFixture(t, nil)
	response := sendSide(t, server.Handler(), http.MethodPost, "/v1/responses/compact", "codex", sideThread+":0", `{}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", response.StatusCode)
	}
	if got := rec.served(); len(got) != 1 || !strings.Contains(got[0], "cool-token") {
		t.Fatalf("served with %q, want the cold compact re-picked onto cool", got)
	}
	if got, _ := store.Get("codex", sideThread); got.AccountID != "cool" {
		t.Fatalf("pin = %q, want cool", got.AccountID)
	}
}

func TestSideChannelRequestClassifier(t *testing.T) {
	post := func(path string) *http.Request {
		return httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
	}
	upgrade := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	upgrade.Header.Set("Connection", "Upgrade")
	upgrade.Header.Set("Upgrade", "websocket")
	for _, tc := range []struct {
		name     string
		provider accounts.Provider
		agent    string
		session  string
		req      *http.Request
		want     bool
	}{
		{"codex analytics on the thread", accounts.ProviderCodex, "codex", sideThread, post(codexAnalyticsPath), true},
		{"codex mcp on the thread", accounts.ProviderCodex, "codex", sideThread, post("/backend-api/ps/mcp"), true},
		{"claude count_tokens", accounts.ProviderClaude, "claude", sideThread, post("/v1/messages/count_tokens"), true},
		{"codex model POST", accounts.ProviderCodex, "codex", sideThread, post("/v1/responses"), false},
		{"codex compact", accounts.ProviderCodex, "codex", sideThread, post("/v1/responses/compact"), false},
		{"codex backend responses", accounts.ProviderCodex, "codex", sideThread, post("/backend-api/codex/responses"), false},
		{"codex websocket turn", accounts.ProviderCodex, "codex", sideThread, upgrade, false},
		{"claude model POST", accounts.ProviderClaude, "claude", sideThread, post("/v1/messages"), false},
		{"file upload follows the model account", accounts.ProviderCodex, "codex", sideThread, post("/backend-api/files"), false},
		{"file sub-path", accounts.ProviderCodex, "codex", sideThread, post("/backend-api/files/abc/uploaded"), false},
		{"fallback key", accounts.ProviderCodex, "codex", "fallback:0123456789abcdef", post(codexAnalyticsPath), false},
		{"no key", accounts.ProviderCodex, "codex", "", post(codexAnalyticsPath), false},
		{"other provider", accounts.ProviderKimi, "kimi", sideThread, post("/v1/chat/completions"), false},
	} {
		if got := sideChannelRequest(tc.provider, tc.agent, tc.session, tc.req); got != tc.want {
			t.Errorf("%s: sideChannelRequest = %v, want %v", tc.name, got, tc.want)
		}
	}
}
