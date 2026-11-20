package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// The owner's rule: a warm session takes priority over everything except a
// forced eviction -- its account hit a 5h or weekly limit or reached the
// holdout, or its side genuinely cannot serve. These tests pin the places that
// used to move a warm session for something softer.

// --- a transient token refresh serves one turn elsewhere and moves nothing ---

func transientRefreshServer(t *testing.T, peer *RemotePool) (Server, *session.Store) {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "s1", "a", ""); err != nil {
		t.Fatal(err)
	}
	roll := time.Now().Add(100 * time.Hour)
	unreachable := &url.Error{Op: "Post", URL: "https://platform.claude.com/v1/oauth/token",
		Err: &net.OpError{Op: "dial", Net: "tcp4", Err: errors.New("lookup platform.claude.com: no such host")}}
	server := Server{
		ClaudeUpstream: mustURL(t, "http://127.0.0.1:1"), // never reached: the refresh fails first
		Accounts: []accounts.Account{
			{ID: "a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-a"},
			{ID: "b", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-b"},
		},
		Sessions:       store,
		ActiveSessions: NewActiveSessions(),
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "a", Provider: accounts.ProviderClaude, Fresh: true, Headroom: 0.6, ShortHeadroom: 0.9, WeeklyHeadroom: 0.6, WeeklyRemaining: 0.6, WeeklyResetAt: roll},
			{AccountID: "b", Provider: accounts.ProviderClaude, Fresh: true, Headroom: 0.6, ShortHeadroom: 0.9, WeeklyHeadroom: 0.6, WeeklyRemaining: 0.6, WeeklyResetAt: roll},
		})),
		RefreshAccountFn: func(_ context.Context, account accounts.Account) (accounts.Account, error) {
			if account.ID == "a" {
				return account, unreachable
			}
			return account, nil
		},
		RemotePool:   peer,
		MaxBodyBytes: 1 << 20,
	}
	return server, store
}

func postClaudeTurn(t *testing.T, handler http.Handler, sessionID string) *http.Response {
	t.Helper()
	subrouter := httptest.NewServer(handler)
	t.Cleanup(subrouter.Close)
	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", sessionID)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

func TestTransientRefreshServesOneTurnOnThePeerWithoutMovingTheSession(t *testing.T) {
	peerHits := 0
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == remotePoolUsagePath {
			// Not a turn: since PATCHES §31 the one-turn serve ranks the
			// peer's accounts first, which reads its usage body. This peer
			// publishes none, so it is served untargeted, as before.
			return
		}
		peerHits++
		w.Header().Set(subrouterAccountHeader, "peer-acct")
		_, _ = io.WriteString(w, `{"id":"msg"}`)
	}))
	defer peer.Close()
	pool := NewRemotePool(peer.URL, "", "")
	server, store := transientRefreshServer(t, pool)

	response := postClaudeTurn(t, server.Handler(), "s1")
	if response.StatusCode != http.StatusOK || peerHits != 1 {
		t.Fatalf("status %d, peer hits %d: the turn should have been served once by the peer", response.StatusCode, peerHits)
	}
	if pool.delegations.active(session.ScopedSessionKey("claude", "s1"), time.Now()) {
		t.Fatal("a refresh blip gave the peer a hold on the session")
	}
	if got, _ := store.Get("claude", "s1"); got.AccountID != "a" {
		t.Fatalf("session rebound to %q", got.AccountID)
	}
	if server.scheduler().Exhausted(accounts.ProviderClaude, "a") {
		t.Fatal("a refresh blip marked the account exhausted")
	}
}

func TestTransientRefreshWithNoPeerFailsTheTurnRetryably(t *testing.T) {
	server, store := transientRefreshServer(t, nil)

	response := postClaudeTurn(t, server.Handler(), "s1")
	if response.StatusCode != http.StatusBadGateway || response.Header.Get(remotePoolTransientHeader) == "" {
		t.Fatalf("status %d transient=%q: want a marked 502 so a forwarding daemon keeps its hold",
			response.StatusCode, response.Header.Get(remotePoolTransientHeader))
	}
	if got, _ := store.Get("claude", "s1"); got.AccountID != "a" {
		t.Fatalf("session rebound to %q", got.AccountID)
	}
}

func TestPeerTransientFailureKeepsTheHold(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(remotePoolTransientHeader, "1")
		http.Error(w, "refresh blip", http.StatusBadGateway)
	}))
	defer peer.Close()
	server, key := heldSessionServer(t, peer.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	if _, ok := server.remotePoolForward(req, "claude", "s1", "/v1/messages", "", []byte(`{}`), true); ok {
		t.Fatal("a transient peer failure was handed to the client instead of served locally")
	}
	if !server.RemotePool.delegations.active(key, time.Now()) {
		t.Fatal("a transient failure on the peer ended the session's hold")
	}
}

func TestPeerDeclineStillEndsTheHold(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no non-exhausted claude accounts", http.StatusServiceUnavailable)
	}))
	defer peer.Close()
	server, key := heldSessionServer(t, peer.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	server.remotePoolForward(req, "claude", "s1", "/v1/messages", "", []byte(`{}`), true)
	if server.RemotePool.delegations.active(key, time.Now()) {
		t.Fatal("a peer whose pool is spent kept the session")
	}
}

// --- the token endpoint answering 429/5xx is transient, not a dead account ---

func TestTokenEndpointOverloadIsTransient(t *testing.T) {
	live := context.Background()
	cases := []struct {
		err  error
		want bool
	}{
		{errors.New("Claude OAuth refresh failed: 429 Too Many Requests: slow down"), true},
		{errors.New("Claude OAuth refresh failed: 503 Service Unavailable: "), true},
		{errors.New("Claude OAuth refresh failed: 500 Internal Server Error: oops"), true},
		{errors.New("Claude OAuth refresh failed: 400 Bad Request: invalid_grant"), false},
		{errors.New("Claude OAuth refresh failed: 401 Unauthorized: "), false},
		{&accounts.CodexAuthRefreshError{StatusCode: http.StatusBadGateway}, true},
		{&accounts.CodexAuthRefreshError{StatusCode: http.StatusUnauthorized, ProviderCode: "invalid_grant"}, false},
	}
	for _, tc := range cases {
		if got := refreshFailureIsTransient(live, tc.err); got != tc.want {
			t.Errorf("%v: transient = %v, want %v", tc.err, got, tc.want)
		}
	}

	p := newRefreshProbe(t, func(_ context.Context, account accounts.Account) error {
		if account.ID == "a" {
			return errors.New("Claude OAuth refresh failed: 429 Too Many Requests: rate limited")
		}
		return nil
	})
	if _, err := p.refresh(t, live); err == nil {
		t.Fatal("a rate-limited refresh reported success")
	}
	p.assertUntouched(t)
}

// --- a down peer is found down for every session held on it ---

// The peer is down while this machine's own upstream answers between the
// strikes: an outage attributable to the peer (PATCHES §54). The same opening
// with nothing local answering keeps every hold
// (TestBreakerOpeningWhileOfflineKeepsEveryHoldOnDisk).
func TestBreakerOpeningFindsThePeerDownForEverySessionHeldOnIt(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	server, first := heldSessionServer(t, deadURL)
	second := session.ScopedSessionKey("claude", "s2")
	server.RemotePool.delegations.set(second, time.Now(), remotePoolDelegationTTLFor("claude"), "peer-acct", "box")

	for i := 0; i < remotePoolFailuresToOpen; i++ {
		if i > 0 {
			localUpstreamAnswered(server.RemotePool)
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
		server.remotePoolForward(req, "claude", "s1", "/v1/messages", "", []byte(`{}`), true)
	}
	if server.RemotePool.breaker.allow(time.Now()) {
		t.Fatal("calibration: the breaker did not open")
	}
	if !server.RemotePool.breaker.foundDown() || !server.RemotePool.delegations.active(first, time.Now()) || !server.RemotePool.delegations.active(second, time.Now()) {
		t.Fatal("want the peer found down with every hold kept until its chat is placed (PATCHES §67)")
	}
}

// --- peer holds survive a restart, as local assignments already do ---

func TestPeerHoldsSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peer-holds.json")
	now := time.Now()
	before := NewRemotePool("http://box.example:31416", "", "")
	live := session.ScopedSessionKey("claude", "live")
	lapsed := session.ScopedSessionKey("claude", "lapsed")
	before.delegations.set(live, now, warmSessionIdleTTL, "peer-acct", "box")
	before.delegations.set(lapsed, now.Add(-2*warmSessionIdleTTL), warmSessionIdleTTL, "peer-acct", "box")
	before.delegations.flush(path, now)

	after := NewRemotePool("http://box.example:31416", "", "")
	after.delegations.load(path, now)
	got, ok := after.delegations.lookup(live, now)
	if !ok || got.Account != "peer-acct" {
		t.Fatalf("a live hold did not survive the restart: %+v ok=%v", got, ok)
	}
	if after.delegations.active(lapsed, now) {
		t.Fatal("an expired hold was revived by the restart")
	}
	var none *RemotePool
	if none.PersistHolds(t.Context(), path) != nil {
		t.Fatal("PersistHolds on a machine with no peer must stay nil")
	}
}

// --- a failover response is attributed to the account that answered it ---

func TestFailoverResponseIsNotBlamedOnTheSessionsAccount(t *testing.T) {
	reset := strconv.FormatInt(time.Now().Add(72*time.Hour).Unix(), 10)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.Header.Get("Authorization"), "tok-warm"):
			// A bare 429: fails this attempt over, marks nothing.
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`)
		default:
			// The failover account's genuine limit, with a reset days away.
			w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
			w.Header().Set("Anthropic-Ratelimit-Unified-Reset", reset)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"weekly limit"}}`)
		}
	}))
	defer upstream.Close()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "s1", "warm", ""); err != nil {
		t.Fatal(err)
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "warm", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
		{AccountID: "spent", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
	}))
	handler := Server{
		ClaudeUpstream: mustURL(t, upstream.URL),
		Accounts: []accounts.Account{
			{ID: "warm", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-warm"},
			{ID: "spent", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-spent"},
		},
		Sessions:     store,
		SchedulerRef: schedulerRef,
		MaxBodyBytes: 1 << 20,
	}.Handler()

	response := postClaudeTurn(t, handler, "s1")
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the failover account's 429 passed through", response.StatusCode)
	}
	opus := schedulerRef.Get().ForModel(agentclaude.OpusFeature)
	if !opus.Exhausted(accounts.ProviderClaude, "spent") {
		t.Fatal("calibration: the account that hit its limit was not marked")
	}
	if opus.Exhausted(accounts.ProviderClaude, "warm") {
		t.Fatal("another account's weekly-limit 429 was blamed on the warm session's account")
	}
}

// --- pins cannot start a session inside the holdout ---

func TestForcedAccountInsideItsHoldoutIsRefused(t *testing.T) {
	server, _, _ := holdoutFixture(t, 0.04)
	req := claudeMessagesRequest()
	req.Header.Set("X-Subrouter-Account-ID", "held")
	if account, _, _, _, err := server.accountForSessionProvider(accounts.ProviderClaude, "claude", "pinned", req); err == nil {
		t.Fatalf("a forced request started a session on %q inside its holdout", account.ID)
	}
}

func TestPoolPinRefusesAHoldoutAccount(t *testing.T) {
	server := poolTestServer(t, []accounts.Account{
		{ID: "local-codex", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
	})
	server.Scheduler = selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "local-codex", Provider: accounts.ProviderCodex, Fresh: true, WeeklyOnly: true,
			Headroom: 0.04, ShortHeadroom: 0.04, WeeklyHeadroom: 0.04, WeeklyRemaining: 0.04, WeeklyResetAt: time.Now().Add(50 * time.Hour)},
	})
	server.AccountReserves = map[string]float64{"local-codex": 0.05}
	rec := httptest.NewRecorder()
	server.handlePoolPin(rec, loopbackRequest(http.MethodPost, "/_subrouter/pool/pin",
		`{"account_id":"local-codex","sessions":[{"agent_type":"codex","session_id":"abc"}]}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("pin into a holdout = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

// --- failover never walks a session into the holdout ---

// Both retry pickers built their scheduler from s.scheduler(), which carries no
// reserves, so a 429 failover ranked the holdout account like any other.
func TestFailoverPickSkipsAHoldoutAccount(t *testing.T) {
	roll := time.Now().Add(150 * time.Hour)
	server := Server{
		Accounts: []accounts.Account{
			{ID: "limited", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-limited"},
			{ID: "held", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-held"},
			{ID: "other", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-other"},
		},
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "limited", Provider: accounts.ProviderClaude, Fresh: true, Headroom: 0, ShortHeadroom: 0},
			// In its holdout, and rolling far sooner: on pace it outranks "other"
			// (0.04 over 30h against 0.15 over 150h). Past the final day, where
			// the holdout is lifted (PATCHES §60).
			{AccountID: "held", Provider: accounts.ProviderClaude, Fresh: true, Headroom: 0.9, ShortHeadroom: 0.9,
				WeeklyHeadroom: 0.04, WeeklyRemaining: 0.04, WeeklyResetAt: time.Now().Add(30 * time.Hour)},
			{AccountID: "other", Provider: accounts.ProviderClaude, Fresh: true, Headroom: 0.9, ShortHeadroom: 0.9,
				WeeklyHeadroom: 0.15, WeeklyRemaining: 0.15, WeeklyResetAt: roll},
		})),
		AccountReserves: map[string]float64{"held": 0.05},
	}
	for _, pick := range []struct {
		name string
		fn   func() (accounts.Account, error)
	}{
		{"oauthRetryAccount", func() (accounts.Account, error) {
			return server.oauthRetryAccount(context.Background(), accounts.ProviderClaude, "claude", "", "", "", map[string]struct{}{"limited": {}}, true)
		}},
		{"retryAccount", func() (accounts.Account, error) {
			return server.retryAccount(context.Background(), accounts.ProviderClaude, "claude", "", "", map[string]struct{}{"limited": {}}, false)
		}},
	} {
		got, err := pick.fn()
		if err != nil {
			t.Fatalf("%s: %v", pick.name, err)
		}
		if got.ID != "other" {
			t.Fatalf("%s failed over to %q, inside the owner's holdout", pick.name, got.ID)
		}
	}
}
