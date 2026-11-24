package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
)

// The Codex same-account capacity ladder (upstream manaflow-ai/subrouter
// #389, overload_stay_on_account_test.go as at c9303d3; PATCHES section 27).
// Prompt caches are per account, so by default an overloaded request stays
// on its account: switching turns the turn into a full cache miss. Upstream's
// Codex tests, bar the fork's test-helper signatures (codexCapacityPool's
// marker argument, codexCapacityPost without a context). Not ported: the
// Claude ladder tests and TestCodexCapacityPersistWithoutFailoverStaysOnAccount
// (no Claude ladder, no persist mode here).

// codexStayServer is a Codex pool with the account failover off but test
// gaps installed, which is the default policy with fast timing.
func codexStayServer(t *testing.T, fail func(token, marker string, index int) bool, accountCount int) (Server, func() []codexCapacityAttempt) {
	t.Helper()
	poolURL, seen := codexCapacityPool(t, fail)
	server := codexOverloadServer(t, poolURL, accountCount, false)
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	server.CodexOverloadFailover = &CodexOverloadFailoverConfig{}
	fastCapacityGaps(server.CodexOverloadFailover, 5*time.Millisecond)
	return server, seen
}

func assertOnlyToken(t *testing.T, attempts []codexCapacityAttempt, token string) {
	t.Helper()
	for i, attempt := range attempts {
		if attempt.token != token {
			t.Fatalf("attempt %d went to %s (all: %v), want every attempt on %s", i, attempt.token, tokens(attempts), token)
		}
	}
}

// With no configuration at all, a capacity failure is retried on the same
// account and the retry's success is served.
func TestCodexCapacityRetryOnByDefault(t *testing.T) {
	poolURL, seen := codexCapacityPool(t, func(_, _ string, index int) bool { return index == 0 })
	server := codexOverloadServer(t, poolURL, 2, false)
	// The unconfigured default, exactly as serve builds it without env.
	server.CodexOverloadFailover = nil
	if _, err := server.Sessions.Put("codex", "session-default", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body := codexCapacityPost(t, proxy.URL, "session-default", "a")
	if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-0") {
		t.Fatalf("status=%d body=%s, want the same account to serve the retry", status, body)
	}
	got := seen()
	if len(got) != 2 {
		t.Fatalf("pool saw %v, want the failure and one retry", tokens(got))
	}
	assertOnlyToken(t, got, "oauth-token-0")
	if assignment, ok := server.Sessions.Get("codex", "session-default"); !ok || assignment.AccountID != "codex-account-0" {
		t.Fatalf("session moved to %+v, want it to stay on account 0", assignment)
	}
}

// Failover off: a sustained capacity failure is retried on the session's
// account only, bounded, then passed through. The account is not marked, so
// the next turn does not get evicted from its prompt cache either.
func TestCodexCapacityDefaultStaysOnAccountThenPassesThrough(t *testing.T) {
	server, seen := codexStayServer(t, func(string, string, int) bool { return true }, 3)
	if _, err := server.Sessions.Put("codex", "session-stay", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body := codexCapacityPost(t, proxy.URL, "session-stay", "a")
	if status != http.StatusOK || !strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("status=%d body=%s, want the capacity failure passed through", status, body)
	}
	got := seen()
	if len(got) != 1+codexTestStayRetries {
		t.Fatalf("pool saw %d attempts %v, want %d (first try plus the same-account ladder)", len(got), tokens(got), 1+codexTestStayRetries)
	}
	assertOnlyToken(t, got, "oauth-token-0")
	if marks := server.SchedulerRef.CapacityMarks(accounts.ProviderCodex, "gpt-6-astra", selectacct.CapacityAnyTier); len(marks) != 0 {
		t.Fatalf("capacity marks = %v, want none while failover is off", marks)
	}
	if assignment, ok := server.Sessions.Get("codex", "session-stay"); !ok || assignment.AccountID != "codex-account-0" {
		t.Fatalf("session moved to %+v, want it to stay on account 0", assignment)
	}
}

// Failover off: success on a later same-account retry is what the client gets.
func TestCodexCapacityDefaultServesLaterSameAccountRetry(t *testing.T) {
	server, seen := codexStayServer(t, func(_, _ string, index int) bool { return index < 4 }, 3)
	if _, err := server.Sessions.Put("codex", "session-later", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body := codexCapacityPost(t, proxy.URL, "session-later", "a")
	if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-0") || strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("status=%d body=%s, want the fifth attempt's completion", status, body)
	}
	got := seen()
	if len(got) != 5 {
		t.Fatalf("pool saw %v, want four failures and the success", tokens(got))
	}
	assertOnlyToken(t, got, "oauth-token-0")
}

// The same-account ladder's shape: gaps double from ~0.5s to ~8s (±20%
// jitter), then hold at ~9s (8-10s), inside a 4m budget; the failover ladder
// keeps its ~10s.
func TestCodexCapacityStayLadderShape(t *testing.T) {
	var config *CodexOverloadFailoverConfig
	if got := config.retryBudget(); got != 4*time.Minute {
		t.Fatalf("unconfigured budget = %v, want 4m", got)
	}
	if got := (&CodexOverloadFailoverConfig{Enabled: true}).retryBudget(); got != codexCapacityDefaultRetryBudget {
		t.Fatalf("failover budget = %v, want %v", got, codexCapacityDefaultRetryBudget)
	}
	for retry, want := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second} {
		for range 20 {
			got := config.stayDelay(retry, codexCapacityDefaultStayInterval)
			low, high := time.Duration(float64(want)*0.8), time.Duration(float64(want)*1.2)
			if got < low || got > high {
				t.Fatalf("stayDelay(%d) = %v, want %v ±20%%", retry, got, want)
			}
		}
	}
	for retry := 5; retry < 40; retry++ {
		got := config.stayDelay(retry, codexCapacityDefaultStayInterval)
		if got < 8*time.Second || got > 10*time.Second {
			t.Fatalf("stayDelay(%d) = %v, want a steady 8-10s", retry, got)
		}
	}
	// A shorter interval caps the ramp: 0.5s, 1s, 2s, 2s, ...
	for retry, want := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second} {
		got := config.stayDelay(retry, 2*time.Second)
		low, high := time.Duration(float64(want)*0.8), time.Duration(float64(want)*1.2)
		if got < low || got > high {
			t.Fatalf("stayDelay(%d, 2s) = %v, want %v with jitter", retry, got, want)
		}
	}
}

// With the failover off and an egress configured, the same-account ladder's
// retries leave from the next region (upstream #665's region transport sits
// under every retry layer), so the first retry goes out through the egress
// and is served there: the ladder is never spent and the egress fallback
// layer never runs, so the session is not pinned to the egress.
//
// This test used to say the egress got its turn "once the same-account
// ladder is spent", and passed through the region rotation instead (it held
// with zero stay retries, or with the fallback layer removed). In production
// the region transport is installed whenever an egress is (both need
// CodexEgress.configured()), so "fallback only after the ladder" is not a
// reachable configuration; the rotation is what this pins.
func TestCodexCapacityDefaultLadderRetriesFromTheEgressRegion(t *testing.T) {
	var local atomic.Int32
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if region := r.Header.Get(codexEgressHeader); region != "" {
			codexEgressWriteCompleted(w, region)
			return
		}
		local.Add(1)
		codexEgressWriteOverloaded(w)
	}))
	defer pool.Close()
	poolURL, _ := url.Parse(pool.URL)
	var calls atomic.Int32
	fra := codexEgressTestProxy(t, "fra", &calls)
	server := codexEgressServer(t, poolURL, []*url.URL{fra}, 2)
	server.CodexOverloadFailover = &CodexOverloadFailoverConfig{}
	fastCapacityGaps(server.CodexOverloadFailover, time.Millisecond)
	server.codexEgressSessions = newAzureCodexSticky()
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body := codexEgressPost(t, proxy.URL, "session-stay-egress")
	if status != http.StatusOK || !strings.Contains(body, "served-from-fra") {
		t.Fatalf("status=%d body=%s, want the ladder's first retry served from the egress region", status, body)
	}
	if local.Load() != 1 || calls.Load() != 1 {
		t.Fatalf("local attempts=%d egress calls=%d, want the first attempt local and the first retry through the egress", local.Load(), calls.Load())
	}
	if _, pinned := server.codexEgressSessions.lookup(azureCodexSessionKeyFor("codex", "session-stay-egress")); pinned {
		t.Fatal("session pinned to the egress: the egress fallback layer ran, the region rotation did not serve it")
	}
}
