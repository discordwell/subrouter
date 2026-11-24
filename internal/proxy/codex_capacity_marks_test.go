package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
)

// Capacity marks (PATCHES section 27). The first eight tests are upstream
// manaflow-ai/subrouter's (internal/proxy/codex_capacity_marks_test.go,
// codex_capacity_retry_test.go and codex_overload_test.go, 862a5a5 /
// c9303d3), with the failover's quick same-account retry ported too (fix-up
// 1), so they run as upstream wrote them bar the fork's test-helper
// signatures. The rest are the fork's and pin how the rule slots into the
// hour-long warm rule.

// codexCapacityAttempt is one request the fake pool saw.
type codexCapacityAttempt struct {
	token  string
	marker string
	at     time.Time
}

// fastCapacityGaps shrinks every retry gap so the policy's shape can be
// tested without waiting out real jitter. It also bounds the same-account
// ladder at codexTestStayRetries retries, so tests can count attempts;
// production bounds it only by time. Upstream's helper (#389) also shrinks
// its persist gap, which the fork does not have.
func fastCapacityGaps(config *CodexOverloadFailoverConfig, gap time.Duration) {
	fixed := func() time.Duration { return gap }
	config.sameAccountGap = fixed
	config.switchGap = fixed
	config.stayRetryLimit = codexTestStayRetries
}

// codexTestStayRetries is the same-account retry count tests with fast gaps
// allow.
const codexTestStayRetries = 8

// codexCapacityPool answers capacity failures while fail returns true and
// completes otherwise. fail sees the account token, the request's marker and
// how many attempts the pool has seen before this one (0-based). Ported from
// upstream's codex_capacity_retry_test.go with the marker passed to fail.
func codexCapacityPool(t *testing.T, fail func(token, marker string, index int) bool) (*url.URL, func() []codexCapacityAttempt) {
	t.Helper()
	var mu sync.Mutex
	var seen []codexCapacityAttempt
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		marker := ""
		if _, after, ok := strings.Cut(string(body), `"marker":"`); ok {
			marker, _, _ = strings.Cut(after, `"`)
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		index := len(seen)
		seen = append(seen, codexCapacityAttempt{token: token, marker: marker, at: time.Now()})
		mu.Unlock()
		if fail(token, marker, index) {
			codexEgressWriteOverloaded(w)
			return
		}
		codexEgressWriteCompleted(w, token)
	}))
	t.Cleanup(server.Close)
	poolURL, _ := url.Parse(server.URL)
	return poolURL, func() []codexCapacityAttempt {
		mu.Lock()
		defer mu.Unlock()
		return append([]codexCapacityAttempt(nil), seen...)
	}
}

func codexCapacityPost(t *testing.T, proxyURL, sessionID, marker string) (int, string) {
	t.Helper()
	return codexCapacityPostBody(t, proxyURL, `{"model":"gpt-6-astra","session_id":"`+sessionID+`","marker":"`+marker+`","input":[]}`)
}

func codexCapacityPostBody(t *testing.T, proxyURL, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, proxyURL+"/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	out, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(out)
}

func tokens(attempts []codexCapacityAttempt) []string {
	out := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		out = append(out, attempt.token)
	}
	return out
}

func lastToken(t *testing.T, seen func() []codexCapacityAttempt) string {
	t.Helper()
	got := tokens(seen())
	if len(got) == 0 {
		t.Fatal("the pool saw no request")
	}
	return got[len(got)-1]
}

// A capacity failure is not quota: it leaves no exhaustion mark, and the
// account's first success on the model clears its capacity mark. Upstream's
// test, unchanged: the success is the failover's quick same-account retry.
func TestCodexCapacityMarkIsNotQuotaAndClearsOnSuccess(t *testing.T) {
	poolURL, _ := codexCapacityPool(t, func(_, _ string, index int) bool { return index == 0 })
	server := codexOverloadServer(t, poolURL, 2, true)
	server.SchedulerRef = selectacct.NewSchedulerRef(server.Scheduler)
	fastCapacityGaps(server.CodexOverloadFailover, 5*time.Millisecond)
	if _, err := server.Sessions.Put("codex", "session-clear", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body := codexCapacityPost(t, proxy.URL, "session-clear", "a")
	if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-0") {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if _, ok := server.SchedulerRef.ExhaustedUntilFor(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra"); ok {
		t.Fatal("capacity failure left a quota exhaustion mark")
	}
	if _, _, ok := server.SchedulerRef.CapacityMarkFor(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", ""); ok {
		t.Fatal("capacity mark survived the account's success on the same model")
	}
}

// Failures are marked in the request's (model, service tier) pool: the
// first failure and the quick retry's, before the switch. Upstream's test.
func TestCodexCapacityMarkCarriesServiceTier(t *testing.T) {
	poolURL, _ := codexCapacityPool(t, func(token, _ string, _ int) bool { return token == "oauth-token-0" })
	server := codexOverloadServer(t, poolURL, 2, true)
	server.SchedulerRef = selectacct.NewSchedulerRef(server.Scheduler)
	fastCapacityGaps(server.CodexOverloadFailover, 5*time.Millisecond)
	if _, err := server.Sessions.Put("codex", "session-tier", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body := codexCapacityPostBody(t, proxy.URL, `{"model":"gpt-6-astra","service_tier":"priority","session_id":"session-tier","input":[]}`)
	if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-1") {
		t.Fatalf("status=%d body=%s", status, body)
	}
	_, failures, ok := server.SchedulerRef.CapacityMarkFor(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "priority")
	if !ok || failures < 2 {
		t.Fatalf("priority-tier mark failures=%d ok=%v, want the repeated failures recorded", failures, ok)
	}
	if _, _, ok := server.SchedulerRef.CapacityMarkFor(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", ""); ok {
		t.Fatal("priority-tier failure marked the default tier")
	}
}

// "Selected model is at capacity" is load shedding: the same request on the
// same account usually gets through a moment later. The first retry stays on
// the session's account (its prompt cache lives there) after a short
// jittered gap. Upstream's test (codex_capacity_retry_test.go), unchanged.
func TestCodexCapacityRetriesSameAccountFirst(t *testing.T) {
	poolURL, seen := codexCapacityPool(t, func(_, _ string, index int) bool { return index == 0 })
	server := codexOverloadServer(t, poolURL, 2, true)
	if _, err := server.Sessions.Put("codex", "session-same", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body := codexCapacityPost(t, proxy.URL, "session-same", "a")
	if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-0") {
		t.Fatalf("status=%d body=%s, want the same account to serve the retry", status, body)
	}
	got := seen()
	if strings.Join(tokens(got), ",") != "oauth-token-0,oauth-token-0" {
		t.Fatalf("pool saw %v, want account 0 twice", tokens(got))
	}
	if gap := got[1].at.Sub(got[0].at); gap < 250*time.Millisecond || gap > 1500*time.Millisecond {
		t.Fatalf("same-account retry gap = %v, want a 250-750ms jittered pause", gap)
	}
	if assignment, ok := server.Sessions.Get("codex", "session-same"); !ok || assignment.AccountID != "codex-account-0" {
		t.Fatalf("session moved to %+v, want it to stay on account 0", assignment)
	}
}

// When the same account fails again, the failover moves on to the other
// accounts. Upstream's test, unchanged.
func TestCodexCapacityDefaultMovesToOtherAccountsAfterSameAccountRetry(t *testing.T) {
	poolURL, seen := codexCapacityPool(t, func(token, _ string, _ int) bool { return token == "oauth-token-0" })
	server := codexOverloadServer(t, poolURL, 2, true)
	fastCapacityGaps(server.CodexOverloadFailover, 10*time.Millisecond)
	if _, err := server.Sessions.Put("codex", "session-move", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body := codexCapacityPost(t, proxy.URL, "session-move", "a")
	if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-1") {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if got := strings.Join(tokens(seen()), ","); got != "oauth-token-0,oauth-token-0,oauth-token-1" {
		t.Fatalf("pool saw %s, want account 0 twice then account 1", got)
	}
}

// A single capacity failure lowers the account's rank for new sessions but
// keeps the sticky session (and its prompt cache) where it is; the same
// account failing again moves the session. Upstream's test, with #388's
// placement-stats assertions.
func TestCodexCapacityMarkKeepsStickySessionUntilRepeatedFailure(t *testing.T) {
	poolURL, seen := codexCapacityPool(t, func(string, string, int) bool { return false })
	server := codexOverloadServer(t, poolURL, 2, true)
	server.SchedulerRef = selectacct.NewSchedulerRef(server.Scheduler)
	if _, err := server.Sessions.Put("codex", "session-sticky", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()
	until := time.Now().Add(time.Minute)
	server.SchedulerRef.MarkCapacityUntil(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "", until)

	if status, body := codexCapacityPost(t, proxy.URL, "session-sticky", "a"); status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if got := lastToken(t, seen); got != "oauth-token-0" {
		t.Fatalf("sticky session left its account after one capacity failure: %v", tokens(seen()))
	}
	// That success cleared the mark; a brand-new session placed while the
	// account is marked avoids it.
	server.SchedulerRef.MarkCapacityUntil(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "", until)
	for i := range 8 {
		if status, body := codexCapacityPost(t, proxy.URL, "session-new-"+string(rune('a'+i)), "b"); status != http.StatusOK {
			t.Fatalf("status=%d body=%s", status, body)
		}
		if got := lastToken(t, seen); got != "oauth-token-1" {
			t.Fatalf("new session placed on the capacity-marked account: %v", tokens(seen()))
		}
	}
	server.SchedulerRef.MarkCapacityUntil(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "", until)
	if status, body := codexCapacityPost(t, proxy.URL, "session-sticky", "c"); status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if got := lastToken(t, seen); got != "oauth-token-1" {
		t.Fatalf("sticky session stayed on an account that kept failing: %v", tokens(seen()))
	}
	// The placement stats (upstream #388, 9b065b5) saw the same story: one
	// sticky eviction and three capacity marks on account 0, and every new
	// session placed on account 1.
	marked := placementCountersFor(server.SchedulerRef, accounts.ProviderCodex, "codex-account-0")
	if marked.Evictions != 1 || marked.Placements != 0 || marked.CapacityMarks != 3 {
		t.Fatalf("account 0 counters = %+v, want 1 eviction, 0 placements, 3 capacity marks", marked)
	}
	if got := placementCountersFor(server.SchedulerRef, accounts.ProviderCodex, "codex-account-1"); got.Placements != 8 {
		t.Fatalf("account 1 placements = %d, want the 8 new sessions", got.Placements)
	}
}

// Over the websocket transport the capacity mark uses the turn's model and
// tier from response.create, is not an account-wide quota mark, and the
// 1012 reconnect lands on the same account first. Upstream's test, unchanged.
func TestCodexWebSocketCapacityMarkKeepsSessionOnAccount(t *testing.T) {
	failed := `{"type":"response.failed","response":{"error":{"code":"server_is_overloaded","message":"Selected model is at capacity."}}}`
	var mu sync.Mutex
	var connections []string
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		connections = append(connections, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		first := len(connections) == 1
		mu.Unlock()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if first {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(failed))
		} else {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"r"}}`))
		}
		_, _, _ = conn.ReadMessage()
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	server := codexEgressServer(t, upstreamURL, nil, 2)
	server.CodexOverloadFailover = &CodexOverloadFailoverConfig{Enabled: true}
	server.SchedulerRef = selectacct.NewSchedulerRef(server.Scheduler)
	counters := &codexCapacityCounterStore{}
	server.codexCapacityCounters = counters
	if _, err := server.Sessions.Put("codex", "ws-capacity", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := newProxyTestServer(t, server.Handler())
	wsURL := "ws" + strings.TrimPrefix(proxy.URL, "http") + "/backend-api/codex/responses"
	create := `{"type":"response.create","model":"gpt-6-astra","service_tier":"priority"}`

	turn := func() (string, error) {
		conn, response, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Session-Id": []string{"ws-capacity"}})
		if err != nil {
			return "", err
		}
		defer response.Body.Close()
		defer conn.Close()
		if err := conn.WriteMessage(websocket.TextMessage, []byte(create)); err != nil {
			return "", err
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, body, err := conn.ReadMessage()
		return string(body), err
	}
	_, err := turn()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseServiceRestart {
		t.Fatalf("first turn err = %v, want a 1012 reroute", err)
	}
	if _, failures, ok := server.SchedulerRef.CapacityMarkFor(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "priority"); !ok || failures != 1 {
		t.Fatalf("websocket capacity mark failures=%d ok=%v, want one priority-tier mark", failures, ok)
	}
	if _, ok := server.SchedulerRef.ExhaustedUntilFor(accounts.ProviderCodex, "codex-account-0", ""); ok {
		t.Fatal("websocket capacity failure left an account-wide exhaustion mark")
	}
	body, err := turn()
	if err != nil || !strings.Contains(body, "response.completed") {
		t.Fatalf("reconnect body=%q err=%v", body, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(connections) != 2 || connections[1] != "oauth-token-0" {
		t.Fatalf("upstream connections %v, want the reconnect on the same account", connections)
	}
	if _, _, ok := server.SchedulerRef.CapacityMarkFor(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "priority"); ok {
		t.Fatal("websocket success did not clear the capacity mark")
	}
	// Fork: the reconnect stayed on the account, so the 1012 counts as a
	// same-account retry, not as upstream #620's account switch (§27).
	if got := counters.snapshot(); got.SameAccountRetries != 1 || got.AccountSwitches != 0 {
		t.Fatalf("capacity counters = %+v, want one same-account retry and no switch", got)
	}
}

// The mark's lifetime follows the upstream's retry hint (Retry-After,
// retry_after_ms, resets_in_seconds), clamped to [30s, 5m] and jittered
// +-20%, or the configured TTL without one. Upstream's table (#373, as at
// c9303d3): the body cases read the failure payload #373's peek keeps
// (codexPeekedBody); before the 389/373 port the fork read headers only and
// they fell to the default TTL. Plus the fork's Retry-After-Ms header case.
func TestCodexOverloadMarkTTLHonorsRetryHints(t *testing.T) {
	const overloaded = `{"error":{"code":"server_is_overloaded"}}`
	cases := []struct {
		name     string
		header   string
		ms       string
		body     string
		min, max time.Duration
	}{
		{"retry-after header", "200", "", overloaded, 160 * time.Second, 240 * time.Second},
		{"retry-after-ms header", "", "60000", overloaded, 48 * time.Second, 72 * time.Second},
		{"retry_after_ms body", "", "", `{"error":{"code":"server_is_overloaded","retry_after_ms":60000}}`, 48 * time.Second, 72 * time.Second},
		{"resets_in_seconds body", "", "", `{"error":{"code":"slow_down","resets_in_seconds":90}}`, 72 * time.Second, 108 * time.Second},
		{"short hint clamps up", "", "", `{"error":{"code":"server_is_overloaded","retry_after_ms":2000}}`, 30 * time.Second, 30 * time.Second},
		{"long hint clamps down", "3600", "", overloaded, 5 * time.Minute, 5 * time.Minute},
		{"no hint uses the default", "", "", overloaded, 96 * time.Second, 144 * time.Second},
	}
	for index, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") == "oauth-token-0" {
					w.Header().Set("Content-Type", "application/json")
					if test.header != "" {
						w.Header().Set("Retry-After", test.header)
					}
					if test.ms != "" {
						w.Header().Set("Retry-After-Ms", test.ms)
					}
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(w, test.body)
					return
				}
				codexEgressWriteCompleted(w, "ok")
			}))
			defer pool.Close()
			poolURL, _ := url.Parse(pool.URL)
			server := codexOverloadServer(t, poolURL, 2, true)
			server.SchedulerRef = selectacct.NewSchedulerRef(server.Scheduler)
			fastCapacityGaps(server.CodexOverloadFailover, 5*time.Millisecond)
			sessionID := fmt.Sprintf("session-ttl-%d", index)
			if _, err := server.Sessions.Put("codex", sessionID, "codex-account-0", ""); err != nil {
				t.Fatal(err)
			}
			proxy := httptest.NewServer(server.Handler())
			defer proxy.Close()
			started := time.Now()
			if status, body := codexEgressPost(t, proxy.URL, sessionID); status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, body)
			}
			until, _, ok := server.SchedulerRef.CapacityMarkFor(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "")
			if !ok {
				t.Fatal("overloaded account was not marked")
			}
			ttl := until.Sub(started)
			slack := 2 * time.Second
			if ttl < test.min-slack || ttl > test.max+slack {
				t.Fatalf("mark ttl = %v, want within [%v, %v]", ttl, test.min, test.max)
			}
		})
	}
}

func TestCodexRetryHintJSONShapes(t *testing.T) {
	cases := map[string]time.Duration{
		`{"type":"response.failed","response":{"error":{"code":"server_is_overloaded","retry_after_ms":1500}}}`: 1500 * time.Millisecond,
		`{"error":{"retry_after":"12s"}}`:           12 * time.Second,
		`{"error":{"resets_in_seconds":"45"}}`:      45 * time.Second,
		`{"type":"error","retry_after":7}`:          7 * time.Second,
		`{"error":{"code":"server_is_overloaded"}}`: 0,
		`not json`: 0,
	}
	for body, want := range cases {
		if got := codexRetryHintJSON([]byte(body)); got != want {
			t.Errorf("codexRetryHintJSON(%s) = %v, want %v", body, got, want)
		}
	}
}

// --- the fork's: how capacity eviction slots into the hour-long warm rule ---

// The warm rule, end to end on real traffic. Account 0 carries a warm session
// ("warm"); other sessions pinned to it hit capacity failures.
//
//   - One failure: hit-1's request is shed once and the failover's quick
//     same-account retry serves it on account 0. Nobody moves -- not warm, and
//     not hit-1 either, whose cache is on account 0 too. Before the port the
//     failure wrote a pool exhaustion mark, which reads as Evicted, and moved
//     every warm session on the account; before fix-up 1 the failing session
//     itself switched accounts on that first failure.
//   - Two consecutive failures: hit-2's request is shed, and shed again on the
//     quick retry, so account 0 is capacity-evicting. The failover serves
//     hit-2 on account 1 (and moves it), and warm's next turn moves to account
//     1, which is not shedding and can hold it. That is upstream's rule as
//     written, and the owner's one approved exception to the warm rule.
//
// Compiles against the pre-port tree and fails there on the first claim.
func TestWarmCodexSessionMovesOnlyAfterRepeatedCapacityFailures(t *testing.T) {
	var shedMu sync.Mutex
	shedOnce := map[string]bool{}
	poolURL, seen := codexCapacityPool(t, func(token, marker string, _ int) bool {
		if token != "oauth-token-0" {
			return false
		}
		switch marker {
		case "shed":
			return true
		case "shed-once":
			shedMu.Lock()
			defer shedMu.Unlock()
			if shedOnce[marker] {
				return false
			}
			shedOnce[marker] = true
			return true
		}
		return false
	})
	server := codexOverloadServer(t, poolURL, 2, true)
	server.SchedulerRef = selectacct.NewSchedulerRef(server.Scheduler)
	fastCapacityGaps(server.CodexOverloadFailover, 5*time.Millisecond)
	for _, id := range []string{"warm", "hit-1", "hit-2"} {
		if _, err := server.Sessions.Put("codex", id, "codex-account-0", ""); err != nil {
			t.Fatal(err)
		}
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	// One capacity failure on account 0; the quick retry serves hit-1 there.
	if status, body := codexCapacityPost(t, proxy.URL, "hit-1", "shed-once"); status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-0") {
		t.Fatalf("hit-1 status=%d body=%s, want its own account to serve the quick retry", status, body)
	}
	if assignment, _ := server.Sessions.Get("codex", "hit-1"); assignment.AccountID != "codex-account-0" {
		t.Fatalf("hit-1 pinned to %q after ONE capacity failure, want codex-account-0 (its cache)", assignment.AccountID)
	}
	if status, body := codexCapacityPost(t, proxy.URL, "warm", "ok"); status != http.StatusOK {
		t.Fatalf("warm status=%d body=%s", status, body)
	}
	if got := lastToken(t, seen); got != "oauth-token-0" {
		t.Fatalf("warm session moved after ONE capacity failure on its account: pool saw %v", tokens(seen()))
	}
	if assignment, _ := server.Sessions.Get("codex", "warm"); assignment.AccountID != "codex-account-0" {
		t.Fatalf("warm session pinned to %q after one failure, want codex-account-0", assignment.AccountID)
	}

	// Two in a row: hit-2 is shed, and shed again on the quick retry.
	if status, body := codexCapacityPost(t, proxy.URL, "hit-2", "shed"); status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-1") {
		t.Fatalf("hit-2 status=%d body=%s", status, body)
	}
	if got := strings.Join(tokens(seen()), ","); !strings.HasSuffix(got, "oauth-token-0,oauth-token-0,oauth-token-1") {
		t.Fatalf("pool saw %s, want hit-2 on account 0 twice, then account 1", got)
	}
	if assignment, _ := server.Sessions.Get("codex", "hit-2"); assignment.AccountID != "codex-account-1" {
		t.Fatalf("hit-2 pinned to %q, want it moved with the failover to codex-account-1", assignment.AccountID)
	}
	if status, body := codexCapacityPost(t, proxy.URL, "warm", "ok"); status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-1") {
		t.Fatalf("warm status=%d body=%s, want it moved to account 1", status, body)
	}
	if got := lastToken(t, seen); got != "oauth-token-1" {
		t.Fatalf("warm session stayed on an account with two consecutive capacity failures: pool saw %v", tokens(seen()))
	}
	if assignment, _ := server.Sessions.Get("codex", "warm"); assignment.AccountID != "codex-account-1" {
		t.Fatalf("warm session pinned to %q after the move, want codex-account-1", assignment.AccountID)
	}
}

// Released by capacity eviction, a session still moves only to an account
// that is not shedding this pool and can hold the session; with none, it
// keeps its account and its cache (keepCapacityEvictedSession, the capacity
// half of upstream's keepConstrainedStickyAssignment).
func TestCapacityEvictedCodexSessionStaysWhenNoAccountIsBetter(t *testing.T) {
	t.Run("every other account is shedding too", func(t *testing.T) {
		poolURL, seen := codexCapacityPool(t, func(string, string, int) bool { return false })
		server := codexOverloadServer(t, poolURL, 2, true)
		server.SchedulerRef = selectacct.NewSchedulerRef(server.Scheduler)
		until := time.Now().Add(time.Minute)
		server.SchedulerRef.MarkCapacityUntil(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "", until)
		server.SchedulerRef.MarkCapacityUntil(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "", until)
		server.SchedulerRef.MarkCapacityUntil(accounts.ProviderCodex, "codex-account-1", "gpt-6-astra", "", until)
		if _, err := server.Sessions.Put("codex", "stay", "codex-account-0", ""); err != nil {
			t.Fatal(err)
		}
		proxy := httptest.NewServer(server.Handler())
		defer proxy.Close()
		if status, body := codexCapacityPost(t, proxy.URL, "stay", "a"); status != http.StatusOK {
			t.Fatalf("status=%d body=%s", status, body)
		}
		if got := lastToken(t, seen); got != "oauth-token-0" {
			t.Fatalf("capacity-evicted session moved to an account that is shedding too: %v", tokens(seen()))
		}
	})
	t.Run("the unmarked account cannot hold the session", func(t *testing.T) {
		poolURL, seen := codexCapacityPool(t, func(string, string, int) bool { return false })
		server := codexOverloadServer(t, poolURL, 2, true)
		// Both below the new-session floor, neither spent: the unmarked one
		// outranks the marked one but could not keep a session either.
		server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "codex-account-0", Provider: accounts.ProviderCodex, Headroom: 0.5, ShortHeadroom: 0.03},
			{AccountID: "codex-account-1", Provider: accounts.ProviderCodex, Headroom: 0.5, ShortHeadroom: 0.03},
		}))
		until := time.Now().Add(time.Minute)
		server.SchedulerRef.MarkCapacityUntil(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "", until)
		server.SchedulerRef.MarkCapacityUntil(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "", until)
		if _, err := server.Sessions.Put("codex", "stay", "codex-account-0", ""); err != nil {
			t.Fatal(err)
		}
		proxy := httptest.NewServer(server.Handler())
		defer proxy.Close()
		if status, body := codexCapacityPost(t, proxy.URL, "stay", "a"); status != http.StatusOK {
			t.Fatalf("status=%d body=%s", status, body)
		}
		if got := lastToken(t, seen); got != "oauth-token-0" {
			t.Fatalf("capacity-evicted session moved to an account that cannot hold it: %v", tokens(seen()))
		}
	})
}

// The owner's holdout still wins. Upstream keeps a capacity-evicted session
// whenever its account is not Exhausted; an account inside its owner's
// holdout is Evicted but not Exhausted, and no session may stay there
// (PATCHES section 20). The fork's guard is !Evicted, so the session leaves
// even though the only other account is shedding too.
func TestCapacityEvictedCodexSessionInHoldoutIsNotKept(t *testing.T) {
	poolURL, seen := codexCapacityPool(t, func(string, string, int) bool { return false })
	server := codexOverloadServer(t, poolURL, 2, true)
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		// Past the final day, where the holdout is lifted (PATCHES §60).
		{AccountID: "codex-account-0", Provider: accounts.ProviderCodex, Fresh: true, Headroom: 0.9, ShortHeadroom: 0.9,
			WeeklyHeadroom: 0.04, WeeklyRemaining: 0.04, WeeklyResetAt: time.Now().Add(30 * time.Hour)},
		{AccountID: "codex-account-1", Provider: accounts.ProviderCodex, Fresh: true, Headroom: 0.9, ShortHeadroom: 0.9,
			WeeklyHeadroom: 0.3, WeeklyRemaining: 0.3, WeeklyResetAt: time.Now().Add(100 * time.Hour)},
	}))
	server.AccountReserves = map[string]float64{"codex-account-0": 0.05}
	until := time.Now().Add(time.Minute)
	server.SchedulerRef.MarkCapacityUntil(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "", until)
	server.SchedulerRef.MarkCapacityUntil(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "", until)
	server.SchedulerRef.MarkCapacityUntil(accounts.ProviderCodex, "codex-account-1", "gpt-6-astra", "", until)
	if _, err := server.Sessions.Put("codex", "held", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()
	if status, body := codexCapacityPost(t, proxy.URL, "held", "a"); status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if got := lastToken(t, seen); got != "oauth-token-1" {
		t.Fatalf("capacity-evicted session was kept inside its owner's holdout: %v", tokens(seen()))
	}
}

// Marks are Codex-only. A Claude scheduler never carries them, even for a
// Claude profile sharing its ID with a capacity-evicting Codex account, so
// Claude sessions see exactly the warm rule.
func TestCapacityMarksAttachOnlyToCodexSelection(t *testing.T) {
	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	until := time.Now().Add(time.Minute)
	ref.MarkCapacityUntil(accounts.ProviderCodex, "same@example.com", "gpt-6-astra", "", until)
	ref.MarkCapacityUntil(accounts.ProviderCodex, "same@example.com", "gpt-6-astra", "", until)
	server := Server{SchedulerRef: ref}
	claude := server.withCapacityMarks(ref.Get(), accounts.ProviderClaude, "gpt-6-astra", "")
	if claude.CapacityEvicting(accounts.ProviderClaude, "same@example.com") ||
		claude.CapacityFailures(accounts.ProviderCodex, "same@example.com") != 0 {
		t.Fatal("capacity marks attached to a Claude selection")
	}
	codex := server.withCapacityMarks(ref.Get(), accounts.ProviderCodex, "gpt-6-astra", "")
	if !codex.CapacityEvicting(accounts.ProviderCodex, "same@example.com") {
		t.Fatal("fixture drift: the Codex selection did not see the marks")
	}
}

// The failover's replacement pick ranks capacity marks too, scoped to the
// request's service tier (carried to oauthRetryCandidate on the context):
// account 1 shedding the priority tier is passed over for a priority request
// and still used for a default-tier one.
func TestCodexOverloadFailoverReplacementAvoidsCapacityMarkedAccount(t *testing.T) {
	for _, test := range []struct {
		name, tier, want string
	}{
		{"same tier: skip the shedding account", "priority", "oauth-token-2"},
		{"other tier: the mark does not apply", "", "oauth-token-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			poolURL, seen := codexCapacityPool(t, func(token, _ string, _ int) bool { return token == "oauth-token-0" })
			server := codexOverloadServer(t, poolURL, 3, true)
			server.SchedulerRef = selectacct.NewSchedulerRef(server.Scheduler)
			fastCapacityGaps(server.CodexOverloadFailover, 5*time.Millisecond)
			// One priority-tier mark on account 1. Unmarked, the pick among
			// the untried accounts is account 1 (ties order by id).
			until := time.Now().Add(time.Minute)
			server.SchedulerRef.MarkCapacityUntil(accounts.ProviderCodex, "codex-account-1", "gpt-6-astra", "priority", until)
			if _, err := server.Sessions.Put("codex", "replace", "codex-account-0", ""); err != nil {
				t.Fatal(err)
			}
			proxy := httptest.NewServer(server.Handler())
			defer proxy.Close()
			body := `{"model":"gpt-6-astra","session_id":"replace","input":[]}`
			if test.tier != "" {
				body = `{"model":"gpt-6-astra","service_tier":"` + test.tier + `","session_id":"replace","input":[]}`
			}
			if status, out := codexCapacityPostBody(t, proxy.URL, body); status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, out)
			}
			if got := tokens(seen()); len(got) != 3 || got[0] != "oauth-token-0" || got[1] != "oauth-token-0" || got[2] != test.want {
				t.Fatalf("pool saw %v, want account 0 twice (the quick retry), then %s", got, test.want)
			}
		})
	}
}

// A websocket session whose account keeps shedding the model leaves it after
// two consecutive failures, like an HTTP session -- even though Codex's
// upgrade names no model. The 1012 reroute records the model it marked
// against the session, and the reconnect reads that model's marks
// (codexCapacityMarksModel, fix-up 1). Without it the reconnect read the
// marks of model "" (none), came back to the shedding account every time,
// and once the 3-reroute budget was spent every later capacity failure
// reached the client. Before the port, the account-wide exhaustion mark moved
// the session on the FIRST failure instead.
func TestCodexWebSocketSessionLeavesPersistentlySheddingAccountWithoutModelHeader(t *testing.T) {
	failed := `{"type":"response.failed","response":{"error":{"code":"server_is_overloaded","message":"Selected model is at capacity."}}}`
	var mu sync.Mutex
	var connections []string
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		connections = append(connections, token)
		mu.Unlock()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if token == "oauth-token-0" {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(failed))
		} else {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"r"}}`))
		}
		_, _, _ = conn.ReadMessage()
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	server := codexEgressServer(t, upstreamURL, nil, 2)
	server.CodexOverloadFailover = &CodexOverloadFailoverConfig{Enabled: true}
	server.SchedulerRef = selectacct.NewSchedulerRef(server.Scheduler)
	if _, err := server.Sessions.Put("codex", "ws-shed", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := newProxyTestServer(t, server.Handler())
	wsURL := "ws" + strings.TrimPrefix(proxy.URL, "http") + "/backend-api/codex/responses"
	create := `{"type":"response.create","model":"gpt-6-astra"}`
	turn := func() string {
		// No X-Subrouter-Model / X-Model: Codex's upgrade carries none.
		conn, response, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Session-Id": []string{"ws-shed"}})
		if err != nil {
			return "dial-error: " + err.Error()
		}
		defer response.Body.Close()
		defer conn.Close()
		if err := conn.WriteMessage(websocket.TextMessage, []byte(create)); err != nil {
			return "write-error"
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, body, err := conn.ReadMessage()
		var closeErr *websocket.CloseError
		if errors.As(err, &closeErr) {
			return fmt.Sprintf("close-%d", closeErr.Code)
		}
		if err != nil {
			return "error: " + err.Error()
		}
		if strings.Contains(string(body), "response.completed") {
			return "completed"
		}
		if strings.Contains(string(body), "server_is_overloaded") {
			return "client-saw-capacity-failure"
		}
		return "other"
	}
	var outcomes []string
	for range 5 {
		outcomes = append(outcomes, turn())
	}
	if got, want := strings.Join(outcomes, ","), "close-1012,close-1012,completed,completed,completed"; got != want {
		t.Fatalf("turn outcomes %s, want %s (stay through one failure, leave after two)", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if got, want := strings.Join(connections, ","), "oauth-token-0,oauth-token-0,oauth-token-1,oauth-token-1,oauth-token-1"; got != want {
		t.Fatalf("upstream connections %s, want %s", got, want)
	}
	if assignment, _ := server.Sessions.Get("codex", "ws-shed"); assignment.AccountID != "codex-account-1" {
		t.Fatalf("session pinned to %q, want codex-account-1", assignment.AccountID)
	}
}
