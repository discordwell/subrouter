package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	agentclaude "github.com/discordwell/subrouter/internal/agents/claude"
	"github.com/discordwell/subrouter/selectacct"
	"github.com/discordwell/subrouter/session"
	"github.com/gorilla/websocket"
)

// PATCHES §31, the box half: a peer's targeted forward is served on exactly
// the named account or refused before upstream, and usage-status publishes the
// box's own new-session verdict for every account. These tests drive the real
// handler (Server.Handler behind httptest) against a fake Anthropic upstream
// and a real scheduler; none of them re-implements a rule.
//
// Every test here names only symbols that exist on origin/main plus the shared
// §31 files, so each one can be (and was) run against the unmodified tree to
// watch it fail for the reason it guards.

// targetFixtureScores is a box pool with one account in every state the
// verdict distinguishes. `chip-5` is the 2026-09-27 account: ~4% of its week
// left, rolling in 16h, a healthy 5h window.
func targetFixtureScores(now time.Time) []selectacct.Score {
	claude := accounts.ProviderClaude
	return []selectacct.Score{
		{AccountID: "roomy", Provider: claude, Fresh: true, Headroom: 0.8, ShortHeadroom: 0.9,
			ShortWindowSeconds: 18000, ShortResetAfterSeconds: 9000,
			WeeklyHeadroom: 0.8, WeeklyRemaining: 0.8, WeeklyResetAt: now.Add(100 * time.Hour)},
		{AccountID: "steady", Provider: claude, Fresh: true, Headroom: 0.5, ShortHeadroom: 0.9,
			ShortWindowSeconds: 18000, ShortResetAfterSeconds: 9000,
			WeeklyHeadroom: 0.5, WeeklyRemaining: 0.5, WeeklyResetAt: now.Add(120 * time.Hour)},
		{AccountID: "chip-5", Provider: claude, Fresh: true, Headroom: 0.04, ShortHeadroom: 0.9,
			ShortWindowSeconds: 18000, ShortResetAfterSeconds: 9000,
			WeeklyHeadroom: 0.04, WeeklyRemaining: 0.04, WeeklyResetAt: now.Add(16 * time.Hour)},
		// 80% of the 5h window gone with four hours still to run: refused on
		// the burn RATE with most of the week left (hotAccountFixture's state).
		{AccountID: "hot", Provider: claude, Fresh: true, Headroom: 0.76, ShortHeadroom: 0.20,
			ShortWindowSeconds: 18000, ShortResetAfterSeconds: 14400,
			WeeklyHeadroom: 0.76, WeeklyRemaining: 0.76, WeeklyResetAt: now.Add(80 * time.Hour)},
		{AccountID: "floor", Provider: claude, Fresh: true, Headroom: 0.7, ShortHeadroom: 0.03,
			ShortWindowSeconds: 18000, ShortResetAfterSeconds: 3600,
			WeeklyHeadroom: 0.7, WeeklyRemaining: 0.7, WeeklyResetAt: now.Add(90 * time.Hour)},
		// Reserve 0.3 (targetFixtureReserves): measured remainder at it.
		{AccountID: "held", Provider: claude, Fresh: true, Headroom: 0.25, ShortHeadroom: 0.9,
			ShortWindowSeconds: 18000, ShortResetAfterSeconds: 9000,
			WeeklyHeadroom: 0.25, WeeklyRemaining: 0.25, WeeklyResetAt: now.Add(100 * time.Hour)},
		{AccountID: "spent", Provider: claude, Fresh: true, Headroom: 0, ShortHeadroom: 0,
			WeeklyResetAt: now.Add(100 * time.Hour)},
	}
}

var targetFixtureReserves = map[string]float64{"held": 0.3}

// targetUpstream is a fake Anthropic that names the account each hit spent
// (by bearer token) and records what it received.
type targetUpstream struct {
	mu      sync.Mutex
	hits    []string
	headers []http.Header
	// respond answers one hit; nil is a plain allowed 200.
	respond func(account string, w http.ResponseWriter)
}

func (u *targetUpstream) handler(w http.ResponseWriter, r *http.Request) {
	account := strings.TrimPrefix(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "tok-")
	u.mu.Lock()
	u.hits = append(u.hits, account)
	u.headers = append(u.headers, r.Header.Clone())
	respond := u.respond
	u.mu.Unlock()
	if respond != nil {
		respond(account, w)
		return
	}
	w.Header().Set("anthropic-ratelimit-unified-status", "allowed")
	_, _ = io.WriteString(w, `{"id":"msg"}`)
}

func (u *targetUpstream) accounts() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.hits...)
}

type targetBox struct {
	server   Server
	store    *session.Store
	active   *ActiveSessions
	upstream *targetUpstream
	logs     *syncBuffer
}

func newTargetBox(t *testing.T) *targetBox {
	t.Helper()
	now := time.Now()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	upstream := &targetUpstream{}
	fake := httptest.NewServer(http.HandlerFunc(upstream.handler))
	t.Cleanup(fake.Close)
	var list []accounts.Account
	for _, score := range targetFixtureScores(now) {
		list = append(list, accounts.Account{
			ID: score.AccountID, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth,
			Token: "tok-" + score.AccountID,
		})
	}
	logs := &syncBuffer{}
	box := &targetBox{
		server: Server{
			ClaudeUpstream:  mustURL(t, fake.URL),
			Accounts:        list,
			Sessions:        store,
			ActiveSessions:  NewActiveSessions(),
			SchedulerRef:    selectacct.NewSchedulerRef(selectacct.NewScheduler(targetFixtureScores(now))),
			AccountReserves: targetFixtureReserves,
			Logger:          slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
			MaxBodyBytes:    1 << 20,
		},
		store:    store,
		upstream: upstream,
		logs:     logs,
	}
	box.active = box.server.ActiveSessions
	return box
}

// pin assigns a session to an account in the box's store.
func (b *targetBox) pin(t *testing.T, sessionID, accountID string) {
	t.Helper()
	if _, err := b.store.Put("claude", sessionID, accountID, ""); err != nil {
		t.Fatal(err)
	}
}

// turn posts one Claude model turn to the box. target "" sends no target
// header (an old laptop, or a client).
func (b *targetBox) turn(t *testing.T, sessionID, target string, extra http.Header) *http.Response {
	t.Helper()
	return b.request(t, "/v1/messages", `{"model":"claude-opus-4-8","messages":[]}`, sessionID, target, extra)
}

func (b *targetBox) request(t *testing.T, path, body, sessionID, target string, extra http.Header) *http.Response {
	t.Helper()
	box := httptest.NewServer(b.server.Handler())
	t.Cleanup(box.Close)
	req, err := http.NewRequest(http.MethodPost, box.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", sessionID)
	if target != "" {
		req.Header.Set(remotePoolTargetAccountHeader, target)
	}
	for name, values := range extra {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

func (b *targetBox) pinnedTo(t *testing.T, sessionID string) string {
	t.Helper()
	assignment, ok := b.store.Get("claude", sessionID)
	if !ok {
		return ""
	}
	return assignment.AccountID
}

// readRefusal asserts the response is the §31 refusal and returns its body.
func readRefusal(t *testing.T, response *http.Response, wantReason string) remotePoolTargetRefusal {
	t.Helper()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != remotePoolTargetRefusedStatus {
		t.Fatalf("status %d, want %d refusal; body %s", response.StatusCode, remotePoolTargetRefusedStatus, body)
	}
	if got := response.Header.Get(remotePoolTargetRefusedHeader); got != wantReason {
		t.Fatalf("%s = %q, want %q; body %s", remotePoolTargetRefusedHeader, got, wantReason, body)
	}
	var refusal remotePoolTargetRefusal
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatalf("refusal body does not decode: %v: %s", err, body)
	}
	if refusal.Target.Reason != wantReason {
		t.Fatalf("body reason %q, want %q", refusal.Target.Reason, wantReason)
	}
	return refusal
}

func servedOn(t *testing.T, response *http.Response, want string) {
	t.Helper()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status %d, want 200 on %q; body %s", response.StatusCode, want, body)
	}
	if got := response.Header.Get(subrouterAccountHeader); got != want {
		t.Fatalf("served on %q, want exactly the target %q", got, want)
	}
}

// Calibration: the fixture's states are the ones the tests below name. Without
// this a refusal test could pass on an account the scheduler reads otherwise.
func TestTargetFixtureVerdicts(t *testing.T) {
	box := newTargetBox(t)
	scheduler := box.server.selectionScheduler("", "")
	now := time.Now()
	for id, want := range map[string]string{
		"roomy": selectacct.VerdictEligible, "steady": selectacct.VerdictEligible,
		"chip-5": selectacct.VerdictBurndown, "hot": selectacct.VerdictBurnProjection,
		"floor": selectacct.VerdictShortFloor, "held": selectacct.VerdictHoldout,
		"spent": selectacct.VerdictExhausted,
	} {
		if got := scheduler.NewSessionVerdict(accounts.ProviderClaude, id, now); got.Reason != want {
			t.Errorf("%s: verdict %+v, want reason %q", id, got, want)
		}
	}
	// And Pick, untargeted, would NOT choose steady: test 4 targets it.
	if picked, _ := scheduler.Pick(box.server.Accounts); picked.ID == "steady" {
		t.Fatal("Pick already chooses steady; targeting it would prove nothing")
	}
}

// 4. The target is served and pinned even though selection would pick another.
func TestTargetedNewPlacementServesExactlyTheTargetAndPinsIt(t *testing.T) {
	box := newTargetBox(t)
	response := box.turn(t, "fresh", "steady", nil)
	servedOn(t, response, "steady")
	if got := box.upstream.accounts(); len(got) != 1 || got[0] != "steady" {
		t.Fatalf("upstream hits %v, want exactly one on steady", got)
	}
	if got := box.pinnedTo(t, "fresh"); got != "steady" {
		t.Fatalf("session pinned to %q, want steady immediately (no provisional move)", got)
	}
	if !strings.Contains(box.logs.String(), `msg="targeted placement"`) {
		t.Fatalf("no targeted placement log line:\n%s", box.logs.String())
	}
}

// 5. The incident: chip-5 already carries its one allowed endgame session, so
// a peer's new session is refused -- before upstream, with no pin written.
func TestTargetedNewPlacementAtCapIsRefusedBeforeUpstream(t *testing.T) {
	box := newTargetBox(t)
	box.pin(t, "busy", "chip-5")
	end := box.active.Begin("claude", "busy")
	defer end()

	response := box.turn(t, "new-session", "chip-5", nil)
	refusal := readRefusal(t, response, selectacct.VerdictAtCap)
	if refusal.Target.Account != "chip-5" || refusal.Target.Continuing {
		t.Fatalf("refusal detail %+v, want chip-5 judged as a new placement", refusal.Target)
	}
	if v := refusal.Target.Verdict; v == nil || !v.AtCap || v.Eligible || v.LiveSessions != 1 || v.AllowedSessions != 1 {
		t.Fatalf("refusal verdict %+v, want at cap on 1 of 1 live sessions", refusal.Target.Verdict)
	}
	if got := box.upstream.accounts(); len(got) != 0 {
		t.Fatalf("upstream contacted %v; a refusal must precede upstream", got)
	}
	if got := box.pinnedTo(t, "new-session"); got != "" {
		t.Fatalf("a refused turn wrote a pin to %q", got)
	}
	logs := box.logs.String()
	if !strings.Contains(logs, `msg="targeted placement refused"`) || !strings.Contains(logs, "target_account=chip-5") ||
		!strings.Contains(logs, "reason=at_cap") {
		t.Fatalf("refusal not logged with account and reason:\n%s", logs)
	}
	if strings.Contains(logs, "routing optimistically") {
		t.Fatal("the incident's optimistic routing line appeared on a targeted turn")
	}
}

// 6. Every reason the box can refuse a new placement for.
func TestTargetedNewPlacementRefusalReasons(t *testing.T) {
	for _, tc := range []struct {
		name, target, want string
		setup              func(t *testing.T, box *targetBox)
		model              string
	}{
		{name: "burn projection", target: "hot", want: selectacct.VerdictBurnProjection},
		{name: "short floor", target: "floor", want: selectacct.VerdictShortFloor},
		{name: "holdout", target: "held", want: selectacct.VerdictHoldout},
		{name: "exhausted", target: "spent", want: selectacct.VerdictExhausted},
		{name: "unknown account", target: "nobody", want: targetRefusedUnknownAccount},
		{name: "not oauth", target: "key", want: targetRefusedNotOAuth, model: agentclaude.FableModel,
			setup: func(t *testing.T, box *targetBox) {
				// The Fable pool is OAuth-only once a Fable fallback exists.
				box.server.ClaudeFableAPIKey = "sk-fable"
				box.server.Accounts = append(box.server.Accounts, accounts.Account{
					ID: "key", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey, Token: "tok-key",
				})
			}},
		{name: "machine policy", target: "steady", want: targetRefusedMachinePolicy,
			setup: func(t *testing.T, box *targetBox) {
				policy := NewMachinePolicy(t.TempDir(), MachinePolicyEnforce, true)
				for i := 1; i <= selectacct.MaxMachinesPerAccount; i++ {
					policy.Note(accounts.ProviderClaude, "steady", "10.0.0."+strconv.Itoa(i))
				}
				box.server.MachinePolicy = policy
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := newTargetBox(t)
			if tc.setup != nil {
				tc.setup(t, box)
			}
			model := tc.model
			if model == "" {
				model = "claude-opus-4-8"
			}
			response := box.request(t, "/v1/messages", `{"model":"`+model+`","messages":[]}`, "s-"+tc.target, tc.target, nil)
			readRefusal(t, response, tc.want)
			if got := box.upstream.accounts(); len(got) != 0 {
				t.Fatalf("upstream contacted %v before the refusal", got)
			}
			if got := box.pinnedTo(t, "s-"+tc.target); got != "" {
				t.Fatalf("refused turn pinned the session to %q", got)
			}
		})
	}
}

// 7. PATCHES §20 on the box: a WARM session already on its account keeps it
// below the new-session floor. Refusing it would cold-cache a live
// conversation for a burn projection.
func TestTargetedWarmContinuingSessionBelowFloorIsServed(t *testing.T) {
	box := newTargetBox(t)
	box.pin(t, "warm", "hot")
	markIdle(box.active, "warm", 5*time.Minute)

	response := box.turn(t, "warm", "hot", nil)
	servedOn(t, response, "hot")
	if got := box.pinnedTo(t, "warm"); got != "hot" {
		t.Fatalf("warm session moved to %q", got)
	}
}

// 8. ...but not onto an evicted one: the owner's holdout ends even a warm
// session, and the box says so instead of moving it to another account.
func TestTargetedContinuingSessionOnAnEvictedAccountIsRefused(t *testing.T) {
	box := newTargetBox(t)
	box.pin(t, "warm", "held")
	markIdle(box.active, "warm", 5*time.Minute)

	response := box.turn(t, "warm", "held", nil)
	refusal := readRefusal(t, response, selectacct.VerdictHoldout)
	if !refusal.Target.Continuing {
		t.Fatal("a warm pinned session was judged as a new placement")
	}
	if got := box.upstream.accounts(); len(got) != 0 {
		t.Fatalf("upstream contacted %v", got)
	}
	if got := box.pinnedTo(t, "warm"); got != "held" {
		t.Fatalf("a refused turn moved the pin to %q", got)
	}
}

// 9. A COLD pin has no warmth to protect, so the target is judged as a new
// placement -- and the box does not quietly re-rank it somewhere else.
func TestTargetedColdPinIsJudgedAsANewPlacement(t *testing.T) {
	box := newTargetBox(t)
	box.pin(t, "cold", "hot")
	markIdle(box.active, "cold", warmSessionIdleTTL+time.Minute)

	response := box.turn(t, "cold", "hot", nil)
	refusal := readRefusal(t, response, selectacct.VerdictBurnProjection)
	if refusal.Target.Continuing {
		t.Fatal("a cold pin was judged as continuing")
	}
	if got := box.upstream.accounts(); len(got) != 0 {
		t.Fatalf("upstream contacted %v", got)
	}
}

// 10. With failover disarmed, an upstream 429 on the target passes through,
// names the target, and is stamped refused: the decider recomputes, the box
// does not substitute.
func TestTargetedTurnNeverSubstitutesOnUpstream429(t *testing.T) {
	box := newTargetBox(t)
	box.upstream.respond = func(account string, w http.ResponseWriter) {
		w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
		w.Header().Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"limit"}}`)
	}
	peerHits := 0
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peerHits++
		_, _ = io.WriteString(w, `{"id":"peer"}`)
	}))
	defer peer.Close()
	box.server.RemotePool = NewRemotePool(peer.URL, "", "")

	response := box.turn(t, "s", "roomy", nil)
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status %d, want the upstream 429 passed through", response.StatusCode)
	}
	if got := response.Header.Get(subrouterAccountHeader); got != "roomy" {
		t.Fatalf("answered by %q, want the target", got)
	}
	if got := response.Header.Get(remotePoolTargetRefusedHeader); got != selectacct.VerdictExhausted {
		t.Fatalf("refused stamp %q, want exhausted", got)
	}
	if got := box.upstream.accounts(); len(got) != 1 || got[0] != "roomy" {
		t.Fatalf("upstream hits %v, want exactly one, on the target", got)
	}
	if peerHits != 0 {
		t.Fatalf("a targeted turn was forwarded to this daemon's peer %d times", peerHits)
	}
}

// 11a. A 429 that leaves the account limited but not spent is transient.
func TestTargetedTransient429IsStampedTransient(t *testing.T) {
	box := newTargetBox(t)
	box.upstream.respond = func(account string, w http.ResponseWriter) {
		w.Header().Set("anthropic-ratelimit-unified-status", "allowed_warning")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	}
	response := box.turn(t, "s", "roomy", nil)
	if response.StatusCode != http.StatusTooManyRequests || response.Header.Get(remotePoolTransientHeader) != "1" {
		t.Fatalf("status %d transient=%q: want a 429 marked transient",
			response.StatusCode, response.Header.Get(remotePoolTransientHeader))
	}
	if got := response.Header.Get(remotePoolTargetRefusedHeader); got != "" {
		t.Fatalf("a limited-not-spent 429 was stamped refused (%q)", got)
	}
	if got := box.upstream.accounts(); len(got) != 1 {
		t.Fatalf("upstream hits %v, want one (no failover)", got)
	}
}

// 11a'. The next turn of the same held session, inside the pause that
// transient 429's Retry-After set on the account's admission gate, is turned
// away by the gate before any upstream call. A targeted turn has no failover,
// so that answer reaches the decider: a bare 503 read there as a legacy
// peer's peer_declined (forced) and ended the hold on a throttle (P8). It is
// stamped like the 429 that caused it: limited, not spent.
func TestTargetedTurnTurnedAwayByAdmissionIsStampedTransient(t *testing.T) {
	box := newTargetBox(t)
	// One gate for both turns: each request builds its own handler here.
	box.server.admission = newAdmissionControl()
	var mu sync.Mutex
	first := true
	box.upstream.respond = func(account string, w http.ResponseWriter) {
		mu.Lock()
		throttle := first
		first = false
		mu.Unlock()
		if throttle {
			w.Header().Set("anthropic-ratelimit-unified-status", "allowed_warning")
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
			return
		}
		w.Header().Set("anthropic-ratelimit-unified-status", "allowed")
		_, _ = io.WriteString(w, `{"id":"msg"}`)
	}
	if response := box.turn(t, "s", "roomy", nil); response.Header.Get(remotePoolTransientHeader) != "1" {
		t.Fatalf("first turn: status %d, want the upstream 429 marked transient", response.StatusCode)
	}

	response := box.turn(t, "s", "roomy", nil)
	if got := box.upstream.accounts(); len(got) != 1 {
		t.Fatalf("upstream hits %v, want the second turn held off by the paused gate", got)
	}
	if response.Header.Get(remotePoolTargetRefusedHeader) != "" || response.Header.Get(remotePoolTransientHeader) != "1" {
		t.Fatalf("second turn: status %d refused=%q transient=%q, want a transient stamp",
			response.StatusCode, response.Header.Get(remotePoolTargetRefusedHeader), response.Header.Get(remotePoolTransientHeader))
	}
	verdict := classifyAccountSwitch(accountSwitchEvidence{PeerStatus: response.StatusCode, PeerHeader: response.Header})
	if verdict.Kind != switchTransient {
		t.Fatalf("decider reads %+v, want a transient switch that keeps the hold", verdict)
	}
}

// 11b. Anthropic answered 200 through overage but said `rejected`: Claude Code
// hard-blocks on that header, so the decider must read it as a refusal.
func TestTargetedRejected200IsStampedRefused(t *testing.T) {
	box := newTargetBox(t)
	box.upstream.respond = func(account string, w http.ResponseWriter) {
		w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
		_, _ = io.WriteString(w, `{"id":"overage"}`)
	}
	response := box.turn(t, "s", "roomy", nil)
	if got := response.Header.Get(remotePoolTargetRefusedHeader); got != selectacct.VerdictExhausted {
		t.Fatalf("status %d refused=%q: a rejected 200 must be stamped refused", response.StatusCode, got)
	}
	if got := box.upstream.accounts(); len(got) != 1 || got[0] != "roomy" {
		t.Fatalf("upstream hits %v, want one on the target", got)
	}
}

// 12. A refresh verdict against the target refuses (no failover to another
// account); a refresh that reached no verdict keeps today's transient 502.
func TestTargetedRefreshFailureRefusesInsteadOfFailingOver(t *testing.T) {
	t.Run("terminal", func(t *testing.T) {
		box := newTargetBox(t)
		box.server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
			if account.ID == "roomy" {
				return account, errors.New(`Claude OAuth refresh failed: 400 Bad Request: {"error": "invalid_grant", "error_description": "Refresh token not found or invalid"}`)
			}
			return account, nil
		}
		response := box.turn(t, "s", "roomy", nil)
		readRefusal(t, response, targetRefusedAuthInvalid)
		if got := box.upstream.accounts(); len(got) != 0 {
			t.Fatalf("upstream hits %v; a dead target must not fail over", got)
		}
		// The pin selection wrote before the refresh stays. That is
		// harmless, because the mark below evicts any untargeted reuse
		// and a retarget re-judges. What must not happen is a substitute.
		if !box.server.scheduler().Exhausted(accounts.ProviderClaude, "roomy") {
			t.Fatal("a dead credential was not marked, so the published verdict keeps offering it")
		}
	})
	// Anthropic's account_on_hold is a verdict too (PATCHES §35.2): refused
	// like a dead login and marked, so the published verdict stops offering
	// the account at once, not only after the next usage sweep's 403.
	t.Run("on hold", func(t *testing.T) {
		box := newTargetBox(t)
		box.server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
			if account.ID == "roomy" {
				return account, errors.New(`Claude OAuth refresh failed: 403 Forbidden: {"error": {"type": "account_on_hold"}}`)
			}
			return account, nil
		}
		response := box.turn(t, "s", "roomy", nil)
		readRefusal(t, response, targetRefusedAuthInvalid)
		if got := box.upstream.accounts(); len(got) != 0 {
			t.Fatalf("upstream hits %v; a target on hold must not fail over", got)
		}
		if !box.server.scheduler().Exhausted(accounts.ProviderClaude, "roomy") {
			t.Fatal("a target on hold was refused unmarked, so the published verdict keeps offering it")
		}
	})
	t.Run("transient", func(t *testing.T) {
		box := newTargetBox(t)
		unreachable := &url.Error{Op: "Post", URL: "https://platform.claude.com/v1/oauth/token",
			Err: &net.OpError{Op: "dial", Net: "tcp4", Err: errors.New("lookup platform.claude.com: no such host")}}
		box.server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
			if account.ID == "roomy" {
				return account, unreachable
			}
			return account, nil
		}
		response := box.turn(t, "s", "roomy", nil)
		if response.StatusCode != http.StatusBadGateway || response.Header.Get(remotePoolTransientHeader) == "" {
			t.Fatalf("status %d transient=%q: a refresh blip must stay a transient 502",
				response.StatusCode, response.Header.Get(remotePoolTransientHeader))
		}
		if response.Header.Get(remotePoolTargetRefusedHeader) != "" {
			t.Fatal("a refresh blip was reported as a refusal")
		}
		if got := box.upstream.accounts(); len(got) != 0 {
			t.Fatalf("upstream hits %v", got)
		}
	})
}

// 13. P7: a targeted request is served or refused here; it never goes on to
// this daemon's own peer, whatever its hop count.
func TestTargetedRequestIsNeverForwardedToThisDaemonsPeer(t *testing.T) {
	peerHits := 0
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == remotePoolUsagePath {
			// Not a turn: the decider half ranks the peer's accounts before
			// an untargeted no-account forward, which reads its usage body.
			// This peer publishes none, so it is served untargeted, as
			// before. A targeted turn never gets this far (no fetch at all).
			return
		}
		peerHits++
		w.Header().Set(subrouterAccountHeader, "peer-acct")
		_, _ = io.WriteString(w, `{"id":"peer"}`)
	}))
	defer peer.Close()

	// Calibration: with every local account unusable for this model, an
	// UNTARGETED first-hop turn does go to the peer -- so the zero below is
	// the target's doing, not an inert peer.
	calibrate := newTargetBox(t)
	calibrate.server.RemotePool = NewRemotePool(peer.URL, "", "")
	calibrate.server.Accounts = []accounts.Account{calibrate.server.Accounts[6]} // spent only
	if response := calibrate.turn(t, "c", "", nil); response.StatusCode != http.StatusOK || peerHits != 1 {
		t.Fatalf("calibration: status %d, peer hits %d; the peer should take an untargeted turn", response.StatusCode, peerHits)
	}
	peerHits = 0

	for _, hop := range []string{"", "1"} {
		t.Run("hop="+hop, func(t *testing.T) {
			box := newTargetBox(t)
			box.server.RemotePool = NewRemotePool(peer.URL, "", "")
			extra := http.Header{}
			if hop != "" {
				extra.Set(remotePoolHopHeader, hop)
			}
			readRefusal(t, box.turn(t, "s-spent", "spent", extra), selectacct.VerdictExhausted)
			servedOn(t, box.turn(t, "s-roomy", "roomy", extra), "roomy")
			// A refresh blip on the target is where an untargeted turn is
			// handed to the peer for one turn; a targeted one answers the
			// transient 502 itself.
			box.server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
				return account, &url.Error{Op: "Post", URL: "https://platform.claude.com/v1/oauth/token",
					Err: &net.OpError{Op: "dial", Net: "tcp4", Err: errors.New("no such host")}}
			}
			if response := box.turn(t, "s-blip", "steady", extra); response.StatusCode != http.StatusBadGateway ||
				response.Header.Get(remotePoolTransientHeader) == "" {
				t.Fatalf("refresh blip on a target: status %d transient=%q, want the box's own transient 502",
					response.StatusCode, response.Header.Get(remotePoolTransientHeader))
			}
			if peerHits != 0 {
				t.Fatalf("a targeted turn reached this daemon's peer %d times", peerHits)
			}
		})
	}
}

// 14. The target names an account on THIS daemon; it must never reach the
// provider.
func TestTargetHeaderNeverReachesUpstream(t *testing.T) {
	box := newTargetBox(t)
	servedOn(t, box.turn(t, "s", "roomy", nil), "roomy")
	box.upstream.mu.Lock()
	defer box.upstream.mu.Unlock()
	if len(box.upstream.headers) != 1 {
		t.Fatalf("upstream hits %d, want 1", len(box.upstream.headers))
	}
	if got := box.upstream.headers[0].Get(remotePoolTargetAccountHeader); got != "" {
		t.Fatalf("upstream received %s: %q", remotePoolTargetAccountHeader, got)
	}
	headers := http.Header{}
	headers.Set(remotePoolTargetAccountHeader, "roomy")
	session.StripSubrouterHeaders(headers)
	if headers.Get(remotePoolTargetAccountHeader) != "" {
		t.Fatal("StripSubrouterHeaders keeps the target header, so a further hop would receive it")
	}
	// The Codex model catalog is answered from one identity account and is
	// never placed; a target must not pin it any more than a forced selector.
	catalog := httptest.NewRequest(http.MethodGet, "/backend-api/codex/models", nil)
	catalog.Header.Set(remotePoolTargetAccountHeader, "roomy")
	// Upstream #606 made it a Server method that also resolves the caller's
	// catalog account; with no accounts it takes the plain shared path.
	if routed, _ := (Server{}).codexModelCatalogRoutingRequest(catalog); routed.Header.Get(remotePoolTargetAccountHeader) != "" {
		got := routed.Header.Get(remotePoolTargetAccountHeader)
		t.Fatalf("the model-catalog routing request keeps the target %q", got)
	}
}

// 15. A side request (count_tokens) follows the target and writes nothing: no
// pin, no move of the conversation's existing pin.
func TestTargetedSideRequestFollowsTargetAndWritesNothing(t *testing.T) {
	box := newTargetBox(t)
	box.pin(t, "conv", "roomy")
	response := box.request(t, "/v1/messages/count_tokens", `{"model":"claude-opus-4-8","messages":[]}`, "conv", "steady", nil)
	servedOn(t, response, "steady")
	if got := box.pinnedTo(t, "conv"); got != "roomy" {
		t.Fatalf("a side request moved the pin to %q", got)
	}
}

// 16. A websocket turn is refused as an HTTP answer to the upgrade, so the
// decider can re-dial elsewhere before anything is spent.
func TestTargetedWebSocketIsRefusedBeforeUpgrade(t *testing.T) {
	box := newTargetBox(t)
	box.pin(t, "busy", "chip-5")
	end := box.active.Begin("claude", "busy")
	defer end()
	server := httptest.NewServer(box.server.Handler())
	defer server.Close()

	header := http.Header{}
	header.Set("X-Subrouter-Agent", "claude")
	header.Set("X-Subrouter-Session", "ws-session")
	header.Set(remotePoolTargetAccountHeader, "chip-5")
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/messages", header)
	if conn != nil {
		conn.Close()
		t.Fatal("the upgrade succeeded on a refused target")
	}
	if err == nil || response == nil {
		t.Fatalf("dial error %v, response %v: want an HTTP refusal", err, response)
	}
	defer response.Body.Close()
	readRefusal(t, response, selectacct.VerdictAtCap)
	if got := box.upstream.accounts(); len(got) != 0 {
		t.Fatalf("upstream contacted %v", got)
	}
}

// 17. CHARACTERIZATION (passes on origin/main by design): an old laptop's
// untargeted forward (hop 1, no target) gets exactly today's box behaviour --
// the warm session stays on its pin, and a 429 on it still fails over locally.
func TestUntargetedForwardIsUnchanged(t *testing.T) {
	box := newTargetBox(t)
	box.pin(t, "warm", "roomy")
	markIdle(box.active, "warm", 5*time.Minute)
	hop := http.Header{}
	hop.Set(remotePoolHopHeader, "1")
	servedOn(t, box.turn(t, "warm", "", hop), "roomy")

	box.upstream.respond = func(account string, w http.ResponseWriter) {
		if account == "roomy" {
			w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"limit"}}`)
			return
		}
		w.Header().Set("anthropic-ratelimit-unified-status", "allowed")
		_, _ = io.WriteString(w, `{"id":"msg"}`)
	}
	response := box.turn(t, "warm", "", hop)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: an untargeted turn must still fail over locally", response.StatusCode)
	}
	if got := response.Header.Get(subrouterAccountHeader); got == "roomy" || got == "" {
		t.Fatalf("served on %q, want a local failover away from the spent pin", got)
	}
	if response.Header.Get(remotePoolTargetRefusedHeader) != "" || response.Header.Get(remotePoolTransientHeader) != "" {
		t.Fatal("an untargeted turn carries a §31 stamp")
	}
}

// --- D1: the published verdict ---------------------------------------------

// usageStatusVerdictRow decodes only what these tests read, with the verdict
// as JSON: the same shape a peer decodes, and it compiles against a tree that
// has no such field (where it stays nil -- the red this guards).
type usageStatusVerdictRow struct {
	ID         string `json:"id"`
	Normalized *struct {
		Usable     bool                            `json:"usable"`
		NewSession *selectacct.NewSessionVerdict   `json:"new_session"`
		Pools      map[string]usageStatusPoolEntry `json:"pools"`
	} `json:"normalized"`
}

type usageStatusPoolEntry struct {
	Usable     bool                          `json:"usable"`
	NewSession *selectacct.NewSessionVerdict `json:"new_session"`
}

// claudeUsageBox is a box whose Claude profiles are measured through the real
// usage sweep: the fake usage endpoint answers per bearer token, the scheduler
// is refreshed from those rows exactly as in production, and usage-status is
// read through the admin handler.
func claudeUsageBox(t *testing.T, usage map[string]string, invalid map[string]bool) (Server, *session.Store, *ActiveSessions) {
	t.Helper()
	claudeStore := agentclaude.Store{Dir: t.TempDir()}
	for name := range usage {
		credential := agentclaude.CredentialInfo{
			AccessToken: "tok-" + name, RefreshToken: "refresh-" + name,
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		}
		if invalid[name] {
			// Expired, so the sweep refreshes it -- and the token endpoint
			// rejects the refresh token: a checked, dead credential.
			credential.ExpiresAt = time.Now().Add(-time.Hour).UnixMilli()
		}
		if err := claudeStore.ImportProfileCredential(name, credential); err != nil {
			t.Fatal(err)
		}
		dir := claudeStore.PreferredInstancePath(claudeStore.InstancePath(name))
		config := `{"oauthAccount":{"emailAddress":"` + name + `@example.com"}}`
		if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ref := &AccountRef{
		store:       accounts.CodexStore{Dir: t.TempDir()},
		claudeStore: claudeStore,
		client: &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if strings.Contains(req.URL.Path, "/oauth/token") {
				return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader(`{"error":"invalid_grant","error_description":"Refresh token not found or invalid"}`))}, nil
			}
			name := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer tok-")
			body, ok := usage[name]
			if !ok {
				return nil, errors.New("unexpected usage fetch for " + name)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	active := NewActiveSessions()
	return Server{
		AccountRef:     ref,
		SchedulerRef:   selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		Sessions:       store,
		ActiveSessions: active,
		AdminToken:     "admin",
		MaxBodyBytes:   1 << 20,
	}, store, active
}

func claudeUsageJSON(now time.Time, fiveHourUsed float64, fiveHourReset time.Duration, weeklyUsed float64, weeklyReset time.Duration, extra string) string {
	rate := func(used float64, reset time.Duration) string {
		return `{"utilization":` + strconv.FormatFloat(used, 'f', -1, 64) +
			`,"resets_at":"` + now.Add(reset).UTC().Format(time.RFC3339) + `"}`
	}
	body := `{"five_hour":` + rate(fiveHourUsed, fiveHourReset) + `,"seven_day":` + rate(weeklyUsed, weeklyReset)
	if extra != "" {
		body += `,` + extra
	}
	return body + `}`
}

func readUsageStatus(t *testing.T, server Server) map[string]usageStatusVerdictRow {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/_subrouter/usage-status", nil)
	req.Header.Set("Authorization", "Bearer admin")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("usage-status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var rows []usageStatusVerdictRow
	if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	byID := map[string]usageStatusVerdictRow{}
	for _, row := range rows {
		byID[row.ID] = row
	}
	return byID
}

// 1. The verdict a peer reads is the box's SELECTION scheduler's verdict: it
// counts the box's live sessions (chip-5 at its taper cap) and applies the
// owner's reserve (reserved in its holdout) -- the two things `usable` never
// saw, and the reason the laptop sent 23 sessions to a capped chip-5.
func TestUsageStatusPublishesNewSessionVerdictFromTheSelectionScheduler(t *testing.T) {
	now := time.Now()
	server, store, active := claudeUsageBox(t, map[string]string{
		"chip-5":   claudeUsageJSON(now, 10, 150*time.Minute, 96, 16*time.Hour, ""),
		"reserved": claudeUsageJSON(now, 10, 150*time.Minute, 75, 100*time.Hour, ""),
		"roomy":    claudeUsageJSON(now, 10, 150*time.Minute, 20, 100*time.Hour, ""),
	}, nil)
	server.AccountReserves = map[string]float64{"reserved": 0.3}
	if _, err := store.Put("claude", "busy", "chip-5", ""); err != nil {
		t.Fatal(err)
	}
	end := active.Begin("claude", "busy")
	defer end()

	rows := readUsageStatus(t, server)
	selection := server.selectionScheduler("", "")
	for id, want := range map[string]string{
		"chip-5": selectacct.VerdictAtCap, "reserved": selectacct.VerdictHoldout, "roomy": selectacct.VerdictEligible,
	} {
		row, ok := rows[id]
		if !ok || row.Normalized == nil {
			t.Fatalf("%s: no measured row in %v", id, rows)
		}
		got := row.Normalized.NewSession
		if got == nil {
			t.Fatalf("%s: usage-status publishes no new_session verdict", id)
		}
		if got.Reason != want {
			t.Errorf("%s: verdict %+v, want reason %q", id, *got, want)
		}
		// The published verdict IS the selection scheduler's, field for field.
		if direct := selection.NewSessionVerdict(accounts.ProviderClaude, id, time.Now()); *got != direct {
			t.Errorf("%s: published %+v, selection says %+v", id, *got, direct)
		}
	}
	if chip5 := rows["chip-5"].Normalized; !chip5.Usable || chip5.NewSession.Eligible ||
		!chip5.NewSession.AtCap || chip5.NewSession.LiveSessions != 1 || chip5.NewSession.AllowedSessions != 1 {
		t.Errorf("chip-5: usable=%v verdict=%+v; want `usable` (the old re-derivation) to say yes and the "+
			"verdict to say at cap on 1 of 1 live sessions", chip5.Usable, *chip5.NewSession)
	}
	if reserved := rows["reserved"].Normalized; !reserved.Usable || reserved.NewSession.Eligible || !reserved.NewSession.Evicted {
		t.Errorf("reserved: usable=%v verdict=%+v; want the reserve to evict what `usable` admits",
			reserved.Usable, *reserved.NewSession)
	}
}

// 3. Each quota pool carries its own verdict: an account whose Fable pool is
// spent is eligible for Opus and exhausted for Fable.
func TestUsageStatusPublishesPerPoolVerdicts(t *testing.T) {
	now := time.Now()
	fableSpent := `"seven_day_oauth_apps":{"utilization":100,"resets_at":"` + now.Add(100*time.Hour).UTC().Format(time.RFC3339) + `"}`
	server, _, _ := claudeUsageBox(t, map[string]string{
		"acct": claudeUsageJSON(now, 10, 150*time.Minute, 20, 100*time.Hour, fableSpent),
	}, nil)
	rows := readUsageStatus(t, server)
	row := rows["acct"]
	if row.Normalized == nil || row.Normalized.NewSession == nil || !row.Normalized.NewSession.Eligible {
		t.Fatalf("account-level verdict %+v, want eligible", row.Normalized)
	}
	fable, ok := row.Normalized.Pools["claudefable"]
	if !ok {
		t.Fatalf("no claudefable pool published: %+v", row.Normalized.Pools)
	}
	if fable.NewSession == nil {
		t.Fatal("the claudefable pool publishes no verdict")
	}
	if fable.NewSession.Eligible || fable.NewSession.Reason != selectacct.VerdictExhausted {
		t.Fatalf("fable pool verdict %+v, want exhausted", *fable.NewSession)
	}
	direct := server.selectionScheduler(agentclaude.FableFeature, "").NewSessionVerdict(accounts.ProviderClaude, "acct", time.Now())
	if *fable.NewSession != direct {
		t.Fatalf("fable pool published %+v, the pool's selection scheduler says %+v", *fable.NewSession, direct)
	}
}

// 18. The mixed-version guarantee: publishing verdicts is ADDITIVE. A peer
// running origin/main's reader knows no new_session key (encoding/json drops
// unknown keys), so what it reads is exactly the rows with new_session
// removed -- and those must be byte for byte the rows this daemon published
// before it published verdicts, or an old laptop's decision moves.
//
// Round 3 (C3): the earlier version compared the reader's winner on the body
// with and without new_session, using THIS tree's reader, which parses
// new_session itself -- and over a fixture where the verdict changed nothing.
// It could not fail. This one compares the rows withNewSessionVerdicts is
// handed with the rows it returns, stripped, over a fixture where the verdict
// DOES change the winner (b is in burndown and at its taper cap with one live
// session: the numbers put it first, its verdict refuses it), and shows the
// verdict-aware reader and the numbers-only reader disagree there.
func TestLegacyPeerReaderIgnoresNewSessionVerdict(t *testing.T) {
	now := time.Now()
	server, store, active := claudeUsageBox(t, map[string]string{
		"a": claudeUsageJSON(now, 10, 150*time.Minute, 20, 100*time.Hour, ""),
		"b": claudeUsageJSON(now, 10, 150*time.Minute, 96, 16*time.Hour, ""),
	}, nil)
	if _, err := store.Put("claude", "live-b", "b", ""); err != nil {
		t.Fatal(err)
	}
	defer active.Begin("claude", "live-b")()
	// The handler's own sweep fills the scheduler the verdicts are read from.
	req := httptest.NewRequest(http.MethodGet, "/_subrouter/usage-status", nil)
	req.Header.Set("Authorization", "Bearer admin")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("calibration: usage-status = %d", recorder.Code)
	}
	statuses := server.withNormalizedUsage(server.AccountRef.UsageStatuses(context.Background()))
	before, err := json.Marshal(statuses)
	if err != nil {
		t.Fatal(err)
	}
	published := server.withNewSessionVerdicts(statuses)
	if after, _ := json.Marshal(statuses); !bytes.Equal(before, after) {
		t.Fatalf("withNewSessionVerdicts wrote into the rows it was handed:\nbefore %s\nafter  %s", before, after)
	}
	withVerdicts, err := json.Marshal(published)
	if err != nil {
		t.Fatal(err)
	}
	var generic []map[string]any
	if err := json.Unmarshal(withVerdicts, &generic); err != nil {
		t.Fatal(err)
	}
	verdicts := 0
	for _, row := range generic {
		if normalized, ok := row["normalized"].(map[string]any); ok {
			if _, has := normalized["new_session"]; has {
				verdicts++
			}
			delete(normalized, "new_session")
			if pools, ok := normalized["pools"].(map[string]any); ok {
				for _, pool := range pools {
					if entry, ok := pool.(map[string]any); ok {
						delete(entry, "new_session")
					}
				}
			}
		}
	}
	if verdicts == 0 {
		t.Fatal("calibration: the body carries no new_session; nothing is being compared")
	}
	var old []map[string]any
	if err := json.Unmarshal(before, &old); err != nil {
		t.Fatal(err)
	}
	oldJSON, _ := json.Marshal(old)
	strippedJSON, _ := json.Marshal(generic)
	if !bytes.Equal(oldJSON, strippedJSON) {
		t.Fatalf("an old reader sees different rows once verdicts are published:\nbefore %s\nnow    %s", oldJSON, strippedJSON)
	}

	// Calibration: the fixture discriminates. Read by numbers alone -- the
	// old reader's view -- b wins (burndown); read with its verdict, b is at
	// its cap and only a competes.
	peer := NewRemotePool("http://peer.invalid", "", "")
	legacy, _, ok := peer.bestPeerScoreFrom(AdminSnapshot{Body: strippedJSON, At: now}, string(accounts.ProviderClaude))
	if !ok || legacy.AccountID != "b" {
		t.Fatalf("calibration: the numbers-only winner is %q (ok=%v), want b", legacy.AccountID, ok)
	}
	candidates, _, _ := peer.decisionCandidates(AdminSnapshot{Body: withVerdicts, At: now}, string(accounts.ProviderClaude), "", nil, now)
	if len(candidates) != 1 || candidates[0].Account != "a" {
		t.Fatalf("calibration: verdict-aware candidates %+v, want only a (b is at its cap)", candidates)
	}
}
