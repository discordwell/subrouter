package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// PATCHES §78. A Claude Code session with CLAUDE_CODE_RETRY_WATCHDOG sleeps
// the whole Retry-After of a 503 before it tries again, so the daemon tells
// an end client to wait at most ClientRetryAfterMax in its own 503s: the
// spent pool's answer and the stranded give-up. The body keeps the real wait
// ("(retry after Ns)", which cmux parses), and a peer daemon's forward
// (X-Pool-Hop) gets the header as before.

// retryAfterBodySeconds is the N of the body's "(retry after Ns)".
func retryAfterBodySeconds(t *testing.T, body string) int64 {
	t.Helper()
	match := retryAfterToken.FindStringSubmatch(body)
	if match == nil {
		t.Fatalf("body has no %q token: %q", retryAfterToken, body)
	}
	seconds, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return seconds
}

// near fails unless got is within two seconds of want.
func near(t *testing.T, what string, got int64, want time.Duration) {
	t.Helper()
	if diff := got - int64(want.Seconds()); diff < -2 || diff > 2 {
		t.Fatalf("%s = %ds, want about %ds", what, got, int64(want.Seconds()))
	}
}

func TestASpentPoolTellsAnEndClientToWaitAtMostTheCap(t *testing.T) {
	spentFor := func(wait time.Duration) []exhaustedPoolAccount {
		now := time.Now()
		return []exhaustedPoolAccount{{id: "a@example.com", windows: []accounts.UsageWindow{
			fiveHour(100, now.Add(wait)), sevenDay(40, now.Add(72*time.Hour)),
		}}}
	}
	for _, tc := range []struct {
		name       string
		cap        time.Duration
		hop        string
		wait       time.Duration
		wantHeader func(bodySeconds int64) string
	}{
		{"a wait longer than the cap", time.Minute, "", 47 * time.Minute, func(int64) string { return "60" }},
		{"a wait shorter than the cap is sent as it is", time.Minute, "", 45 * time.Second, func(n int64) string { return strconv.FormatInt(n, 10) }},
		{"a cap of 90 s", 90 * time.Second, "", 47 * time.Minute, func(int64) string { return "90" }},
		{"the cap off", 0, "", 47 * time.Minute, func(n int64) string { return strconv.FormatInt(n, 10) }},
		{"a peer daemon's forward", time.Minute, "1", 47 * time.Minute, func(n int64) string { return strconv.FormatInt(n, 10) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var extra http.Header
			if tc.hop != "" {
				extra = http.Header{remotePoolHopHeader: {tc.hop}}
			}
			response := serveExhaustedClaudePoolTo(t, spentFor(tc.wait), tc.cap, extra)
			body := response.Body.String()
			bodySeconds := retryAfterBodySeconds(t, body)
			near(t, "the body's wait", bodySeconds, tc.wait)
			if got, want := response.Header().Get("Retry-After"), tc.wantHeader(bodySeconds); got != want {
				t.Fatalf("Retry-After = %q, want %q; body=%q", got, want, body)
			}
			if got := response.Header().Get(subrouterReasonHeader); got != "pool_exhausted" {
				t.Fatalf("%s = %q, want pool_exhausted", subrouterReasonHeader, got)
			}
		})
	}
}

// The credit balance's 503 (upstream #598) is the spent pool's answer made
// in the transport, and is capped the same way.
func TestASpentCreditPoolTellsAnEndClientToWaitAtMostTheCap(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cap    time.Duration
		hop    string
		capped bool
	}{
		{"an end client", time.Minute, "", true},
		{"the cap off", 0, "", false},
		{"a peer daemon's forward", time.Minute, "1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			oauth := "spent@example.com"
			server := Server{
				Accounts: []accounts.Account{
					{ID: "claude:only", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey, Token: "tok-only"},
					{ID: oauth, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-oauth"},
				},
				SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
					scoreFromUsageWindows(accounts.ProviderClaude, oauth, []accounts.UsageWindow{
						fiveHour(100, now.Add(2*time.Hour)), sevenDay(40, now.Add(72*time.Hour)),
					}),
				})),
				ClientRetryAfterMax: tc.cap,
			}
			stub := &stubRoundTripper{responses: func(*http.Request) *http.Response {
				return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{},
					Body: io.NopCloser(strings.NewReader(anthropicCreditBalanceBody))}
			}}
			transport := usageLimitRetryTransport{
				base: stub, server: &server, provider: accounts.ProviderClaude,
				agent: "claude", session: "session-credit-cap", account: "claude:only",
				method: http.MethodPost, path: "/v1/messages", maxAttempts: 1,
				budget: newAttemptBudget(0),
			}
			req := claudeCostTierRequest(t, "tok-only")
			if tc.hop != "" {
				req.Header.Set(remotePoolHopHeader, tc.hop)
			}
			response, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			raw, _ := io.ReadAll(response.Body)
			body := string(raw)
			if response.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("status = %d body %q, want the pool-exhausted 503", response.StatusCode, body)
			}
			bodySeconds := retryAfterBodySeconds(t, body)
			near(t, "the body's wait", bodySeconds, 2*time.Hour)
			want := strconv.FormatInt(bodySeconds, 10)
			if tc.capped {
				want = "60"
			}
			if got := response.Header.Get("Retry-After"); got != want {
				t.Fatalf("Retry-After = %q, want %q; body=%q", got, want, body)
			}
		})
	}
}

// boxAnswer is what a box daemon answered one forwarded turn.
type boxAnswer struct {
	hop, retryAfter, reason, body string
	status                        int
}

// newSpentBox is a box daemon whose one Claude account is spent for wait,
// with the cap on. It records each turn's answer as the laptop got it.
func newSpentBox(t *testing.T, wait time.Duration) (http.HandlerFunc, func() []boxAnswer) {
	t.Helper()
	now := time.Now()
	id := "box@example.com"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the box's spent pool reached its upstream")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "box-sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	box := Server{
		Sessions:       store,
		ClaudeUpstream: mustParseURL(t, upstream.URL),
		Accounts:       []accounts.Account{{ID: id, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-box"}},
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			scoreFromUsageWindows(accounts.ProviderClaude, id, []accounts.UsageWindow{
				fiveHour(100, now.Add(wait)), sevenDay(40, now.Add(72*time.Hour)),
			}),
		})),
		MaxBodyBytes:        1 << 20,
		ClientRetryAfterMax: DefaultClientRetryAfterMax,
	}.Handler()
	var mu sync.Mutex
	var answers []boxAnswer
	handler := func(w http.ResponseWriter, r *http.Request) {
		recorder := httptest.NewRecorder()
		box.ServeHTTP(recorder, r)
		mu.Lock()
		answers = append(answers, boxAnswer{
			hop: r.Header.Get(remotePoolHopHeader), status: recorder.Code, body: recorder.Body.String(),
			retryAfter: recorder.Header().Get("Retry-After"), reason: recorder.Header().Get(subrouterReasonHeader),
		})
		mu.Unlock()
		for key, values := range recorder.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
	}
	return handler, func() []boxAnswer {
		mu.Lock()
		defer mu.Unlock()
		return append([]boxAnswer(nil), answers...)
	}
}

// The laptop's pool is spent and it forwards the turn to a box whose pool is
// spent too. The box answers the laptop as it always has: the header carries
// the real wait, which the laptop's peer_declined row never reads. The
// laptop then answers its own client with its own spent pool's 503, the
// header capped.
func TestAPeerGetsTheSpentPoolsRealWaitAndItsClientIsCapped(t *testing.T) {
	box, answers := newSpentBox(t, 2*time.Hour)
	rig := newHoldRig(t, holdRigOptions{
		upstream:      noLocalUpstream(t),
		exhausted:     true,
		peer:          box,
		retryAfterMax: DefaultClientRetryAfterMax,
	})
	resp, body, err := rig.turn(t, promptly(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	rig.settle(t, 1)

	seen := answers()
	if len(seen) != 1 {
		t.Fatalf("the box answered %d turns, want 1; logs:\n%s", len(seen), rig.logs.String())
	}
	peer := seen[0]
	if peer.hop != "1" || peer.status != http.StatusServiceUnavailable || peer.reason != "pool_exhausted" {
		t.Fatalf("the box saw hop %q and answered %d reason %q, want a forward answered with the spent pool's 503", peer.hop, peer.status, peer.reason)
	}
	peerSeconds := retryAfterBodySeconds(t, peer.body)
	near(t, "the box's wait", peerSeconds, 2*time.Hour)
	if peer.retryAfter != strconv.FormatInt(peerSeconds, 10) {
		t.Fatalf("the box told the laptop Retry-After %q with %ds in its body; a peer gets the real wait", peer.retryAfter, peerSeconds)
	}

	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get(subrouterReasonHeader) != "pool_exhausted" {
		t.Fatalf("the client got %d reason %q body %q, want the laptop's spent pool 503; logs:\n%s",
			resp.StatusCode, resp.Header.Get(subrouterReasonHeader), body, rig.logs.String())
	}
	near(t, "the laptop's wait in its body", retryAfterBodySeconds(t, body), 90*time.Minute)
	if got := resp.Header.Get("Retry-After"); got != "60" {
		t.Fatalf("the client was told Retry-After %q, want 60; body=%q", got, body)
	}
}

// The stranded give-up (PATCHES §67.5) was chosen to wait an hour, so that
// a client without the watchdog shows it at once. An end client is told at
// most the cap; with the cap off it gets 3600 as before.
func TestTheGiveUpTellsAnEndClientToWaitAtMostTheCap(t *testing.T) {
	for _, tc := range []struct {
		name string
		cap  time.Duration
		want string
	}{
		{"the default cap", DefaultClientRetryAfterMax, "60"},
		{"the cap off", 0, "3600"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chat := newSpentPoolChat(t, 5*time.Minute, false, nil, func(opts *holdRigOptions) { opts.retryAfterMax = tc.cap })
			first := chat.heldAt(t, warmWaitStart, "the first held turn")
			chat.clock.set(warmWaitStart.Add(5 * time.Minute))
			got := answeredWithin(t, chat.holdRig, first, chat.tries(), 20, "at the give-up")
			if got.err != nil || got.status != http.StatusServiceUnavailable || got.reason != strandedGaveUpReason || !strings.Contains(got.body, gaveUpOnABox) {
				t.Fatalf("at the give-up: status %d reason %q body %q err %v, want the give-up; logs:\n%s",
					got.status, got.reason, got.body, got.err, chat.logs.String())
			}
			if got.retryAfter != tc.want {
				t.Fatalf("the give-up said Retry-After %q, want %q", got.retryAfter, tc.want)
			}
			if lines := chat.logRecords("stranded chat gave up; answered 503"); len(lines) != 1 || lines[0]["retry_after"] != tc.want {
				t.Fatalf("give-up lines %v, want one with retry_after=%s", lines, tc.want)
			}
		})
	}
}

// A chat that gave up waiting for its box, restarted while nothing can take
// it, gets the give-up answer at once (PATCHES §67.5), capped the same way.
func TestAWaitingChatWithNowhereToGoIsToldToWaitAtMostTheCap(t *testing.T) {
	for _, tc := range []struct {
		name string
		cap  time.Duration
		want string
	}{
		{"the default cap", DefaultClientRetryAfterMax, "60"},
		{"the cap off", 0, "3600"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := newContractBox(t, eligibleBoxAccount("acct-a"))
			cutPeer(t, home.pool, func(int64) error { return refusedFailure() })
			rig := newHoldRig(t, holdRigOptions{
				upstream:      noLocalUpstream(t),
				exhausted:     true,
				peers:         []*RemotePool{home.pool},
				retryAfterMax: tc.cap,
			})
			if err := rig.store.Wait("claude", "hold-session", session.Waiting{Peer: home.pool.name(), Account: "acct-a", Since: time.Now()}); err != nil {
				t.Fatal(err)
			}
			resp, body, err := rig.turn(t, promptly(t), nil)
			if err != nil {
				t.Fatal(err)
			}
			rig.settle(t, 1)
			if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get(subrouterReasonHeader) != strandedGaveUpReason ||
				!strings.Contains(body, "no account can take it") {
				t.Fatalf("status %d reason %q body %q, want the give-up at once; logs:\n%s",
					resp.StatusCode, resp.Header.Get(subrouterReasonHeader), body, rig.logs.String())
			}
			if got := resp.Header.Get("Retry-After"); got != tc.want {
				t.Fatalf("Retry-After = %q, want %q", got, tc.want)
			}
		})
	}
}
