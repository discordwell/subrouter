package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// identityUpstream stands in for Anthropic. It records every call as
// "METHOD path token" and answers with answer(path, token), or 200 {}.
type identityUpstream struct {
	mu     sync.Mutex
	calls  []string
	answer func(path, token string) (int, string)
}

func (u *identityUpstream) seen() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.calls...)
}

func (u *identityUpstream) setAnswer(answer func(path, token string) (int, string)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.answer = answer
}

func newIdentityUpstream(t *testing.T) (*identityUpstream, *url.URL) {
	t.Helper()
	u := &identityUpstream{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		u.mu.Lock()
		u.calls = append(u.calls, r.Method+" "+r.URL.Path+" "+token)
		answer := u.answer
		u.mu.Unlock()
		status, body := http.StatusOK, `{}`
		if answer != nil {
			status, body = answer(r.URL.Path, token)
		}
		w.Header().Set("Content-Type", "application/json")
		if status == http.StatusTooManyRequests {
			// A spent window, as Anthropic says it: the header that marks.
			w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u, parsed
}

// identityServer is a three-account Claude pool behind upstream. alpha sorts
// first and has the most headroom, so it is what both the old fallback and the
// scheduler would choose.
func identityServer(t *testing.T, upstream *url.URL, pin string, socket, identityOnly bool) Server {
	t.Helper()
	dir := t.TempDir()
	store, err := session.NewStore(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	return Server{
		ClaudeUpstream: upstream,
		Accounts: []accounts.Account{
			{ID: "alpha@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-alpha"},
			{ID: "mid@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-mid"},
			{ID: "zeta@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-zeta"},
		},
		Sessions: store,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "alpha@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1, Fresh: true},
			{AccountID: "mid@example.com", Provider: accounts.ProviderClaude, Headroom: 0.5, ShortHeadroom: 0.5, Fresh: true},
			{AccountID: "zeta@example.com", Provider: accounts.ProviderClaude, Headroom: 0.5, ShortHeadroom: 0.5, Fresh: true},
		})),
		MaxBodyBytes:        1 << 20,
		IdentityAccountID:   pin,
		IdentityPinRequired: socket,
		IdentityOnly:        identityOnly,
		claudeCosts:         newClaudeCostStore(filepath.Join(dir, "claude-cost-state.json")),
	}
}

type identityReply struct {
	status int
	reason string
	body   string
}

// policyLimits sends Claude Code's socket-mode control-plane call: no
// credential and no session header.
func policyLimits(t *testing.T, baseURL string) identityReply {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+"/api/claude_code/policy_limits", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "claude-code/2.1.328")
	return identityDo(t, req)
}

func identityMessages(t *testing.T, baseURL, sessionID string, header map[string]string) identityReply {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", sessionID)
	for key, value := range header {
		req.Header.Set(key, value)
	}
	return identityDo(t, req)
}

func identityDo(t *testing.T, req *http.Request) identityReply {
	t.Helper()
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return identityReply{status: response.StatusCode, reason: response.Header.Get(subrouterReasonHeader), body: string(body)}
}

func healthIdentity(t *testing.T, baseURL string) (ControlPlaneIdentity, bool) {
	t.Helper()
	response, err := http.Get(baseURL + "/_subrouter/health")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var payload struct {
		Identity *ControlPlaneIdentity `json:"control_plane_identity"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Identity == nil {
		return ControlPlaneIdentity{}, false
	}
	return *payload.Identity, true
}

func wantIdentityRefused(t *testing.T, reply identityReply, state string) {
	t.Helper()
	if reply.status != http.StatusServiceUnavailable || reply.reason != controlPlaneIdentityReason || !strings.Contains(reply.body, "("+state+")") {
		t.Fatalf("reply = %+v, want a 503 %s refusal naming %q", reply, controlPlaneIdentityReason, state)
	}
}

func markCredentialDead(t *testing.T, server Server, id, token string) {
	t.Helper()
	server.claudeCosts.markDead(id, token, claudeRevokedState{Kind: agentclaude.CredentialDeathRevoked, Status: http.StatusUnauthorized})
}

// The 2026-11-19 laptop: the socket configured and nothing pinned, so the
// lowest account ID -- whatever its state -- answered Claude Code's policy call,
// and nothing said so. With a socket the pin is required: no pin is a loud 503
// and health says "unpinned", and no account is asked.
func TestClaudeControlPlaneWithSocketRequiresAPin(t *testing.T) {
	upstream, upstreamURL := newIdentityUpstream(t)
	server := identityServer(t, upstreamURL, "", true, false)
	subrouter := httptest.NewServer(server.Handler())
	defer subrouter.Close()

	wantIdentityRefused(t, policyLimits(t, subrouter.URL), identityUnpinned)
	if calls := upstream.seen(); len(calls) != 0 {
		t.Fatalf("upstream calls = %v, want none: no fallback account may answer", calls)
	}
	identity, ok := healthIdentity(t, subrouter.URL)
	if !ok || identity.ID != "" || identity.State != identityUnpinned {
		t.Fatalf("health control_plane_identity = %+v (present %v), want unpinned with no id", identity, ok)
	}
	// Inference is not the control plane: it still serves.
	if reply := identityMessages(t, subrouter.URL, "turn-1", nil); reply.status != http.StatusOK {
		t.Fatalf("inference without a pin = %+v, want 200", reply)
	}

	// Without a socket nothing changes: the lowest ID answers, and an
	// unconfigured daemon's health has no identity field.
	plain := identityServer(t, upstreamURL, "", false, false)
	plainRouter := httptest.NewServer(plain.Handler())
	defer plainRouter.Close()
	if reply := policyLimits(t, plainRouter.URL); reply.status != http.StatusOK {
		t.Fatalf("no socket, no pin: %+v, want the deterministic fallback's 200", reply)
	}
	if calls := upstream.seen(); calls[len(calls)-1] != "GET /api/claude_code/policy_limits tok-alpha" {
		t.Fatalf("upstream calls = %v, want the policy call on the lowest ID", calls)
	}
	if _, ok := healthIdentity(t, plainRouter.URL); ok {
		t.Fatal("an unconfigured daemon's health grew a control_plane_identity field")
	}
}

// A pin naming no account here used to fall back to the lowest ID silently.
// With a socket it is a 503; without one the fallback still answers, but
// health names the pin that lost and the startup warning fires.
func TestClaudeControlPlanePinNamingNoAccountIsLoud(t *testing.T) {
	upstream, upstreamURL := newIdentityUpstream(t)
	server := identityServer(t, upstreamURL, "ghost@example.com", true, false)
	var logMu sync.Mutex
	var logs bytes.Buffer
	server.Logger = slog.New(slog.NewTextHandler(&lockedWriter{mu: &logMu, w: &logs}, nil))
	subrouter := httptest.NewServer(server.Handler())
	defer subrouter.Close()

	wantIdentityRefused(t, policyLimits(t, subrouter.URL), identityMissing)
	if calls := upstream.seen(); len(calls) != 0 {
		t.Fatalf("upstream calls = %v, want none", calls)
	}
	identity, _ := healthIdentity(t, subrouter.URL)
	if identity.ID != "" || identity.State != identityMissing || identity.Pin != "ghost@example.com" {
		t.Fatalf("health = %+v, want missing, naming the pin", identity)
	}
	server.WarnControlPlaneIdentity(context.Background())
	logMu.Lock()
	logged := logs.String()
	logMu.Unlock()
	for _, want := range []string{"level=WARN", "state=missing", "pin=ghost@example.com", "/api/* answers 503"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("startup warning missing %q; logs:\n%s", want, logged)
		}
	}

	plain := identityServer(t, upstreamURL, "ghost@example.com", false, false)
	plainRouter := httptest.NewServer(plain.Handler())
	defer plainRouter.Close()
	if reply := policyLimits(t, plainRouter.URL); reply.status != http.StatusOK {
		t.Fatalf("no socket: %+v, want the fallback's 200", reply)
	}
	identity, _ = healthIdentity(t, plainRouter.URL)
	if identity.ID != "alpha@example.com" || identity.State != identityMissing || identity.Pin != "ghost@example.com" {
		t.Fatalf("health without a socket = %+v, want alpha answering for a missing pin", identity)
	}
}

// A credential recorded dead never answers. Unpinned, the deterministic pick
// skips it; pinned with a socket, the call is a 503 and nothing is sent on
// the dead credential; a new sign-in the refresh finds answers at once.
func TestClaudeControlPlaneNeverAnswersFromADeadCredential(t *testing.T) {
	upstream, upstreamURL := newIdentityUpstream(t)

	unpinned := identityServer(t, upstreamURL, "", false, false)
	markCredentialDead(t, unpinned, "alpha@example.com", "tok-alpha")
	account, _, _, _, err := unpinned.accountForSessionProvider(accounts.ProviderClaude, "claude", "s", controlPlaneRequest(t, "/api/oauth/profile"))
	if err != nil || account.ID != "mid@example.com" {
		t.Fatalf("unpinned pick = %q, %v; want mid@example.com, the lowest live ID", account.ID, err)
	}

	server := identityServer(t, upstreamURL, "alpha@example.com", true, false)
	markCredentialDead(t, server, "alpha@example.com", "tok-alpha")
	subrouter := httptest.NewServer(server.Handler())
	defer subrouter.Close()
	wantIdentityRefused(t, policyLimits(t, subrouter.URL), identityDead)
	if calls := upstream.seen(); len(calls) != 0 {
		t.Fatalf("upstream calls = %v, want none on a dead credential", calls)
	}
	identity, _ := healthIdentity(t, subrouter.URL)
	if identity.ID != "" || identity.State != identityDead || identity.Pin != "alpha@example.com" {
		t.Fatalf("health = %+v, want dead, naming the pin", identity)
	}

	// The owner signs in again; the refresh reads the new credential from the
	// store. It answers, and the dead record (keyed to the old one) is moot.
	server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		if account.ID == "alpha@example.com" {
			account.Token = "tok-alpha-2"
		}
		return account, nil
	}
	recovered := httptest.NewServer(server.Handler())
	defer recovered.Close()
	if reply := policyLimits(t, recovered.URL); reply.status != http.StatusOK {
		t.Fatalf("after a new sign-in: %+v, want 200", reply)
	}
	if calls := upstream.seen(); len(calls) != 1 || calls[0] != "GET /api/claude_code/policy_limits tok-alpha-2" {
		t.Fatalf("upstream calls = %v, want one policy call on the new credential", calls)
	}
}

// A refresh verdict against the identity used to fail over to a scheduler
// pick for that one call: the client was answered as another organization,
// and the next call went back to the dead identity. Now it is a 503, socket or
// not, and no other account is refreshed or asked.
func TestClaudeControlPlaneRefreshVerdictDoesNotRotateTheIdentity(t *testing.T) {
	for _, socket := range []bool{true, false} {
		upstream, upstreamURL := newIdentityUpstream(t)
		server := identityServer(t, upstreamURL, "alpha@example.com", socket, false)
		var refreshed []string
		server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
			refreshed = append(refreshed, account.ID)
			if account.ID == "alpha@example.com" {
				return account, errors.New(`Claude OAuth refresh failed: 400 Bad Request: {"error": "invalid_grant"}`)
			}
			return account, nil
		}
		subrouter := httptest.NewServer(server.Handler())
		wantIdentityRefused(t, policyLimits(t, subrouter.URL), identityRefreshFailed)
		subrouter.Close()
		if calls := upstream.seen(); len(calls) != 0 {
			t.Fatalf("socket=%v: upstream calls = %v, want none", socket, calls)
		}
		if len(refreshed) != 1 || refreshed[0] != "alpha@example.com" {
			t.Fatalf("socket=%v: refreshed %v, want only the identity", socket, refreshed)
		}
	}
}

// A control-plane answer is not inference evidence. A 401 the endpoint gives
// the identity, a 403 scope refusal and a rejected 429 each pass through to
// the client and set no scheduler mark; the same 401 on inference does.
func TestClaudeControlPlaneAnswersSetNoInferenceMarks(t *testing.T) {
	upstream, upstreamURL := newIdentityUpstream(t)
	server := identityServer(t, upstreamURL, "alpha@example.com", true, false)
	subrouter := httptest.NewServer(server.Handler())
	defer subrouter.Close()
	blocked := func() bool {
		_, explicit := server.SchedulerRef.ExplicitBlockedUntilFor(accounts.ProviderClaude, "alpha@example.com", "", time.Now())
		return explicit || server.scheduler().Evicted(accounts.ProviderClaude, "alpha@example.com")
	}

	answers := []struct {
		status int
		body   string
	}{
		{http.StatusUnauthorized, `{"type":"error","error":{"type":"authentication_error","message":"Invalid bearer token"}}`},
		{http.StatusForbidden, `{"type":"error","error":{"type":"permission_error","message":"OAuth token does not meet scope requirement user:profile"}}`},
		{http.StatusTooManyRequests, `{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`},
	}
	for _, answer := range answers {
		upstream.setAnswer(func(path, token string) (int, string) { return answer.status, answer.body })
		if reply := policyLimits(t, subrouter.URL); reply.status != answer.status {
			t.Fatalf("policy reply = %+v, want the upstream's %d passed through", reply, answer.status)
		}
		if blocked() {
			t.Fatalf("a %d on /api/* marked the identity out of inference", answer.status)
		}
		if server.claudeCredentialDead()(server.Accounts[0]) {
			t.Fatalf("a %d with no death message recorded the credential dead", answer.status)
		}
	}

	// Calibration: the same 401 on inference marks the account.
	upstream.setAnswer(func(path, token string) (int, string) {
		if token == "tok-alpha" {
			return http.StatusUnauthorized, answers[0].body
		}
		return http.StatusOK, `{}`
	})
	if reply := identityMessages(t, subrouter.URL, "turn-1", map[string]string{"X-Subrouter-Account-ID": "alpha@example.com"}); reply.status != http.StatusUnauthorized {
		t.Fatalf("pinned inference = %+v, want the 401", reply)
	}
	if !blocked() {
		t.Fatal("calibration: a 401 on inference set no mark, so the checks above prove nothing")
	}
}

// Anthropic's "has been revoked" is a fact about the credential wherever it is
// said: the first policy call passes the 401 through and records the death,
// with no scheduler mark, and the next one is refused without being sent.
func TestClaudeControlPlaneRevokedAnswerMakesTheIdentityDead(t *testing.T) {
	upstream, upstreamURL := newIdentityUpstream(t)
	upstream.setAnswer(func(path, token string) (int, string) {
		return http.StatusUnauthorized, anthropicRevokedBodyWithID
	})
	server := identityServer(t, upstreamURL, "alpha@example.com", true, false)
	subrouter := httptest.NewServer(server.Handler())
	defer subrouter.Close()

	if reply := policyLimits(t, subrouter.URL); reply.status != http.StatusUnauthorized {
		t.Fatalf("first call = %+v, want Anthropic's 401", reply)
	}
	wantIdentityRefused(t, policyLimits(t, subrouter.URL), identityDead)
	if calls := upstream.seen(); len(calls) != 1 {
		t.Fatalf("upstream calls = %v, want only the first", calls)
	}
	if _, explicit := server.SchedulerRef.ExplicitBlockedUntilFor(accounts.ProviderClaude, "alpha@example.com", "", time.Now()); explicit {
		t.Fatal("the control-plane 401 set a scheduler mark")
	}
	if identity, _ := healthIdentity(t, subrouter.URL); identity.State != identityDead {
		t.Fatalf("health = %+v, want dead", identity)
	}
}

// The public listener's health gives the state but names no account.
func TestControlPlaneIdentityHealthNamesNoAccountPublicly(t *testing.T) {
	_, upstreamURL := newIdentityUpstream(t)
	server := identityServer(t, upstreamURL, "alpha@example.com", true, true)
	private := httptest.NewServer(server.Handler())
	defer private.Close()
	public := httptest.NewServer(server.PublicHandler())
	defer public.Close()

	identity, _ := healthIdentity(t, private.URL)
	if identity.ID != "alpha@example.com" || identity.State != identityPinned || !identity.IdentityOnly {
		t.Fatalf("private health = %+v, want alpha pinned and identity-only", identity)
	}
	identity, ok := healthIdentity(t, public.URL)
	if !ok || identity.ID != "" || identity.Pin != "" || identity.State != identityPinned {
		t.Fatalf("public health = %+v (present %v), want the state only", identity, ok)
	}
}

// Identity-only: the pinned identity answers the control plane and serves no
// inference however it is reached -- a new session, a sticky session already
// on it, a forced pin, a peer's target -- even with the most headroom.
func TestIdentityOnlyAccountServesNoInference(t *testing.T) {
	upstream, upstreamURL := newIdentityUpstream(t)
	server := identityServer(t, upstreamURL, "alpha@example.com", true, true)
	if _, err := server.Sessions.Put("claude", "sticky", "alpha@example.com", ""); err != nil {
		t.Fatal(err)
	}
	subrouter := httptest.NewServer(server.Handler())
	defer subrouter.Close()

	for i := 0; i < 12; i++ {
		if reply := identityMessages(t, subrouter.URL, "new-"+strings.Repeat("x", i), nil); reply.status != http.StatusOK {
			t.Fatalf("new session %d = %+v, want 200", i, reply)
		}
	}
	if reply := identityMessages(t, subrouter.URL, "sticky", nil); reply.status != http.StatusOK {
		t.Fatalf("sticky session = %+v, want 200 on another account", reply)
	}
	if reply := identityMessages(t, subrouter.URL, "forced", map[string]string{"X-Subrouter-Account-ID": "alpha@example.com"}); reply.status != http.StatusServiceUnavailable || !strings.Contains(reply.body, "answers only the control plane") {
		t.Fatalf("forced pin on the identity = %+v, want a refusal", reply)
	}
	if reply := policyLimits(t, subrouter.URL); reply.status != http.StatusOK {
		t.Fatalf("policy call = %+v, want 200 from the identity", reply)
	}
	for _, call := range upstream.seen() {
		if strings.Contains(call, "/v1/messages") && strings.HasSuffix(call, " tok-alpha") {
			t.Fatalf("inference reached the identity-only account: %s (all calls %v)", call, upstream.seen())
		}
	}
	if calls := upstream.seen(); calls[len(calls)-1] != "GET /api/claude_code/policy_limits tok-alpha" {
		t.Fatalf("last call = %q, want the policy call on the identity", calls[len(calls)-1])
	}
	if assignment, _ := server.Sessions.Get("claude", "sticky"); assignment.AccountID == "alpha@example.com" {
		t.Fatal("the sticky session stayed on the identity-only account")
	}

	// A peer's target on it is refused before upstream.
	req := httptest.NewRequest(http.MethodPost, "https://subrouter.test/v1/messages", strings.NewReader(`{}`))
	req.Header.Set(remotePoolTargetAccountHeader, "alpha@example.com")
	if _, _, _, _, err := server.accountForSessionProvider(accounts.ProviderClaude, "claude", "peer-turn", req); err == nil {
		t.Fatal("a peer's target on the identity-only account was served")
	}

	// Calibration: off, the forced pin is served.
	off := identityServer(t, upstreamURL, "alpha@example.com", true, false)
	offRouter := httptest.NewServer(off.Handler())
	defer offRouter.Close()
	if reply := identityMessages(t, offRouter.URL, "forced", map[string]string{"X-Subrouter-Account-ID": "alpha@example.com"}); reply.status != http.StatusOK {
		t.Fatalf("identity-only off: forced pin = %+v, want 200", reply)
	}
}

// Identity-only: peers read the account as taking no session.
func TestIdentityOnlyAccountIsPublishedIneligible(t *testing.T) {
	_, upstreamURL := newIdentityUpstream(t)
	server := identityServer(t, upstreamURL, "alpha@example.com", true, true)
	rows := server.withNewSessionVerdicts(server.withNormalizedUsage([]AccountUsageStatus{
		{AccountStatus: AccountStatus{ID: "alpha@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}},
		{AccountStatus: AccountStatus{ID: "mid@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}},
	}))
	verdict := func(i int) *selectacct.NewSessionVerdict {
		if rows[i].Normalized == nil || rows[i].Normalized.NewSession == nil {
			t.Fatalf("row %d carries no verdict", i)
		}
		return rows[i].Normalized.NewSession
	}
	if v := verdict(0); v.Eligible || !v.Evicted {
		t.Fatalf("identity-only verdict = %+v, want ineligible and evicted", *v)
	}
	if v := verdict(1); !v.Eligible {
		t.Fatalf("pool account verdict = %+v, want eligible", *v)
	}
}

// Identity-only: a refresh failover and a usage failover never land on it.
func TestIdentityOnlyAccountIsNoFailoverTarget(t *testing.T) {
	dead := errors.New(`Claude OAuth refresh failed: 400 Bad Request: {"error": "invalid_grant"}`)
	refreshFailover := func(identityOnly bool) (accounts.Account, error) {
		server, _ := claudeFailoverServer(t)
		server.IdentityAccountID, server.IdentityOnly = "fresh@example.com", identityOnly
		server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
			if account.ID == "cooked@example.com" {
				return account, dead
			}
			return account, nil
		}
		got, _, err := server.refreshSelectedAccount(context.Background(), accounts.ProviderClaude, "claude", "session-1", "", claudeRefreshRequest(t), server.Accounts[0])
		return got, err
	}
	if got, err := refreshFailover(false); err != nil || got.ID != "fresh@example.com" {
		t.Fatalf("calibration: failover = %q, %v; want fresh@example.com", got.ID, err)
	}
	if got, err := refreshFailover(true); err == nil || got.ID == "fresh@example.com" {
		t.Fatalf("identity-only: refresh failover = %q, %v; want no account", got.ID, err)
	}

	// The usage failover's ordinary pick, then its lapsed-mark probe.
	for _, score := range []selectacct.Score{{Headroom: 1, ShortHeadroom: 1}, {Headroom: 0, ShortHeadroom: 0}} {
		f := newRecoveryProbeFixture(t, score, claudeOK)
		f.server.SchedulerRef.MarkExhaustedUntil(accounts.ProviderClaude, "fresh@example.com", "", time.Now().Add(-time.Second))
		f.server.IdentityAccountID, f.server.IdentityOnly = "fresh@example.com", true
		if status := f.roundTrip(t); status != http.StatusTooManyRequests || f.freshCalls != 0 {
			t.Fatalf("headroom %v: status %d, identity-only calls %d; want the pool's 429 and none", score.Headroom, status, f.freshCalls)
		}
	}
}

// Identity-only: the sweep sends the account no Messages probe, which would
// spend its quota; other accounts are probed as before.
func TestIdentityOnlyAccountGetsNoPaidProbe(t *testing.T) {
	transport := &claudeProbeTransport{}
	// Neither alive nor dead: the probe is only counted, and nothing is
	// written behind the test's back.
	transport.set(http.StatusInternalServerError, `{}`)
	ref := cacheTestAccountRef(t, transport)
	ref.SetClaudeIdentityOnly("claude@example.com")
	identity := accounts.Account{ID: "claude@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok"}
	other := accounts.Account{ID: "other@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-other"}

	if windows, err := ref.claudeFableProbe(context.Background(), ref.client, identity); err != nil || windows != nil {
		t.Fatalf("identity-only Fable probe = %v, %v; want nothing", windows, err)
	}
	ref.probeClaudeLiveness(context.Background(), ref.client, identity)
	server := Server{
		AccountRef:   ref,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{futureExhaustedQuota(identity, time.Now().Add(3*time.Hour))})),
	}
	for pass := 0; pass < 3; pass++ {
		server.scoreAccounts(context.Background(), []accounts.Account{identity})
	}
	if got := transport.calls("tok"); got != 0 {
		t.Fatalf("Messages probes on the identity-only account = %d, want 0", got)
	}
	if _, err := ref.claudeFableProbe(context.Background(), ref.client, other); err != nil {
		t.Fatal(err)
	}
	if got := transport.calls("tok-other"); got != 1 {
		t.Fatalf("calibration: probes on a pool account = %d, want 1", got)
	}
}
