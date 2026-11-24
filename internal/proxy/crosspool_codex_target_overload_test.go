package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
)

// P8 on the box half, for Codex: a TARGETED Codex turn (PATCHES §31) is a hard
// placement, so every stage that could substitute an account is disarmed --
// but a provider-side 5xx or capacity answer is not the account's, and must
// neither end the decider's hold nor reach the decider as the box's "my pool
// is spent". Before this, the Codex capacity layer (§27's stay ladder) was not
// installed on a targeted turn at all and the stamp named overload for Claude
// only, so ONE upstream 503 reached the decider bare, classifyAccountSwitch
// read it as a legacy peer's peer_declined (forced), and the hold ended, the
// peer went down for the turn, and the session was re-ranked.

// codexTargetBox is a Codex box as production builds it (§27: the failover
// and its capacity marks ON) with fast capacity gaps, and a scheduler that
// accepts both accounts as a target.
func codexTargetBox(t *testing.T, answer func(token string, index int, w http.ResponseWriter) bool) (Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		index := len(seen)
		seen = append(seen, token)
		mu.Unlock()
		if answer != nil && answer(token, index, w) {
			return
		}
		codexEgressWriteCompleted(w, token)
	}))
	t.Cleanup(pool.Close)
	poolURL, _ := url.Parse(pool.URL)
	server := codexOverloadServer(t, poolURL, 2, true)
	fastCapacityGaps(server.CodexOverloadFailover, 5*time.Millisecond)
	now := time.Now()
	scores := []selectacct.Score{}
	for _, id := range []string{"codex-account-0", "codex-account-1"} {
		scores = append(scores, selectacct.Score{
			AccountID: id, Provider: accounts.ProviderCodex, Fresh: true,
			Headroom: 0.6, ShortHeadroom: 0.9, ShortWindowSeconds: 18000, ShortResetAfterSeconds: 9000,
			WeeklyHeadroom: 0.6, WeeklyRemaining: 0.6, WeeklyResetAt: now.Add(100 * time.Hour),
		})
	}
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler(scores))
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func codexTargetedPost(t *testing.T, server Server, target string) *http.Response {
	t.Helper()
	proxy := httptest.NewServer(server.Handler())
	t.Cleanup(proxy.Close)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, proxy.URL+"/responses",
		strings.NewReader(`{"model":"gpt-6-astra","session_id":"s1","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(remotePoolTargetAccountHeader, target)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

func writeCodex503(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, `{"error":{"type":"server_error","message":"upstream unavailable"}}`)
}

// One upstream 503 on a targeted Codex turn is absorbed on the SAME account,
// as main's box absorbed it: the retry's success is what the decider gets,
// and no other account is ever tried (the target is hard).
func TestATargetedCodexTurnRetriesAProvider503OnTheTarget(t *testing.T) {
	server, seen := codexTargetBox(t, func(_ string, index int, w http.ResponseWriter) bool {
		if index == 0 {
			writeCodex503(w)
			return true
		}
		return false
	})
	response := codexTargetedPost(t, server, "codex-account-0")
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "served-from-oauth-token-0") {
		t.Fatalf("status %d body %s, pool saw %v; a provider 503 is retried on the target", response.StatusCode, body, seen())
	}
	if got := seen(); len(got) != 2 || got[0] != "oauth-token-0" || got[1] != "oauth-token-0" {
		t.Fatalf("pool saw %v; want the 503 and its retry, both on the target", got)
	}
	if refused := response.Header.Get(remotePoolTargetRefusedHeader); refused != "" {
		t.Fatalf("refused %q on a served turn", refused)
	}
}

// A 503 that outlasts the same-account ladder passes through STAMPED
// overload, which the decider reads as no switch at all: the hold is kept,
// nothing is refused, the peer is not marked down.
func TestAPersistentProvider503OnATargetedCodexTurnIsStampedOverload(t *testing.T) {
	server, seen := codexTargetBox(t, func(_ string, _ int, w http.ResponseWriter) bool {
		writeCodex503(w)
		return true
	})
	response := codexTargetedPost(t, server, "codex-account-0")
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d; a persistent provider 503 passes through", response.StatusCode)
	}
	got := seen()
	if len(got) < 2 {
		t.Fatalf("pool saw %v; the same-account ladder never ran", got)
	}
	for _, token := range got {
		if token != "oauth-token-0" {
			t.Fatalf("pool saw %v; a targeted turn never leaves its target", got)
		}
	}
	if stamp := response.Header.Get(remotePoolTransientHeader); stamp != remotePoolTransientOverload {
		t.Fatalf("transient header %q, want %q", stamp, remotePoolTransientOverload)
	}
	sw := classifyAccountSwitch(accountSwitchEvidence{PeerStatus: response.StatusCode, PeerHeader: response.Header})
	if sw.Kind != switchNone {
		t.Fatalf("the decider reads it as %s/%q; a provider 503 keeps the hold (P8)", sw.Kind, sw.Reason)
	}
	// Marks steer a switch; a targeted turn cannot switch, and its account
	// is the decider's to move.
	if marks := server.SchedulerRef.CapacityMarks(accounts.ProviderCodex, "gpt-6-astra", selectacct.CapacityAnyTier); len(marks) != 0 {
		t.Fatalf("capacity marks %v on a targeted turn", marks)
	}
}

// The stamp itself, on the answer a capacity ladder hands on: a 5xx and a
// non-5xx body naming capacity (#373) are overload; a usage_limit 429 is
// still a refusal, and the body survives the inspection.
func TestStampTargetedCodexCapacityAnswerIsOverload(t *testing.T) {
	codex := accounts.Account{ID: "cx", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth}
	server := Server{SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "cx", Provider: accounts.ProviderCodex, Fresh: true, Headroom: 0.6, ShortHeadroom: 0.6},
	}))}
	const capacity429 = `{"error":{"code":"server_is_overloaded","message":"Selected model is at capacity"}}`
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{name: "bare 503", status: http.StatusServiceUnavailable, body: `{"error":"unavailable"}`},
		{name: "502", status: http.StatusBadGateway, body: `bad gateway`},
		{name: "capacity-named 429", status: http.StatusTooManyRequests, body: capacity429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(tc.body))}
			server.stampTargetedResponse(response, codex, "gpt-6-astra")
			if got := response.Header.Get(remotePoolTransientHeader); got != remotePoolTransientOverload {
				t.Fatalf("transient = %q, want overload (refused %q)", got, response.Header.Get(remotePoolTargetRefusedHeader))
			}
			if body, _ := io.ReadAll(response.Body); string(body) != tc.body {
				t.Fatalf("body after the stamp = %q, want %q", body, tc.body)
			}
		})
	}
}

// The targeted ladder ends before the decider's time-to-first-byte timeout,
// whatever the operator's stay budget (4m by default). A ladder that ran on
// would be a peer that never answered: the decider would book it
// unreachable, and two of those open its breaker, which evicts every hold on
// the peer -- the forced switch P8 forbids for a provider blip. Driven on a
// fake clock with the production default config (no count bound).
func TestATargetedCodexCapacityLadderEndsInsideTheDecidersHeaderTimeout(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	var mu sync.Mutex
	clock := start
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	sleep := func(_ context.Context, d time.Duration) bool {
		mu.Lock()
		clock = clock.Add(d)
		mu.Unlock()
		return true
	}
	attempts := 0
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"error":"unavailable"}`))}, nil
	})
	server := &Server{CodexOverloadFailover: &CodexOverloadFailoverConfig{Enabled: true}}
	transport := codexOverloadFailoverTransport{base: base, server: server, account: "codex-account-0",
		poolModel: "gpt-6-astra", budget: newAttemptBudget(100), targeted: true, now: now, sleep: sleep}
	body := `{"model":"gpt-6-astra","input":[]}`
	req, _ := http.NewRequest(http.MethodPost, "http://pool/responses", strings.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }
	response, err := transport.RoundTrip(req)
	if err != nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("response %v err %v; want the 503 handed on", response, err)
	}
	if attempts < 2 {
		t.Fatalf("%d attempts; the same-account ladder never ran", attempts)
	}
	if elapsed := now().Sub(start); elapsed >= remotePoolResponseHeaderTimeout {
		t.Fatalf("the targeted ladder ran %v on the fake clock (%d attempts); it must end inside the decider's %v header timeout",
			elapsed, attempts, remotePoolResponseHeaderTimeout)
	}
}
