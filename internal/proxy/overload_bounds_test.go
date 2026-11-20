package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// Bounds on the Codex stay-on-account capacity ladder: the fallback cap,
// wall-clock budgets and prompt cancellation. Upstream manaflow-ai/subrouter
// #389 (overload_bounds_test.go as at c9303d3), Codex tests only; the persist
// and header opt-in tests are not ported with those features (PATCHES
// section 27).

// An egress or Azure fallback is the operator's deliberate next step for a
// shedding pool, so with the failover off the same-account ladder gives way
// to it after ~10s instead of ~30s.
func TestCodexCapacityStayBudgetCappedWhenFallbackConfigured(t *testing.T) {
	proxyURL := mustParseURL(t, "http://egress.example:3128")
	azure := &AzureCodexConfig{Endpoints: []AzureCodexEndpoint{{BaseURL: mustParseURL(t, "https://azure.example/openai/v1"), APIKey: "key"}}}
	cases := []struct {
		name   string
		server Server
		want   time.Duration
	}{
		{"no fallback", Server{}, codexCapacityDefaultStayMaxWait},
		{"egress", Server{CodexEgress: &CodexEgressConfig{Proxies: []*url.URL{proxyURL}}}, codexCapacityFallbackStayRetryBudget},
		{"azure", Server{AzureCodex: azure}, codexCapacityFallbackStayRetryBudget},
		{"azure without a usable endpoint", Server{AzureCodex: &AzureCodexConfig{}}, codexCapacityDefaultStayMaxWait},
		{"failover with egress", Server{
			CodexOverloadFailover: &CodexOverloadFailoverConfig{Enabled: true},
			CodexEgress:           &CodexEgressConfig{Proxies: []*url.URL{proxyURL}},
		}, codexCapacityDefaultRetryBudget},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := test.server.codexCapacityRetryBudget(); got != test.want {
				t.Fatalf("budget = %v, want %v", got, test.want)
			}
			if got := test.server.codexDefaultRetryBudget("gpt-6-astra", ""); got != test.want {
				t.Fatalf("request budget = %v, want %v", got, test.want)
			}
		})
	}
	if codexCapacityFallbackStayRetryBudget != 10*time.Second {
		t.Fatalf("fallback stay budget = %v, want 10s", codexCapacityFallbackStayRetryBudget)
	}
}

func codexOverloadedResponse() *http.Response {
	recorder := httptest.NewRecorder()
	codexEgressWriteOverloaded(recorder)
	return recorder.Result()
}

// The Codex same-account budget is wall clock from the first attempt, so
// slow upstream attempts count against it, not only the gaps between them.
func TestCodexCapacityStayBudgetIsWallClock(t *testing.T) {
	var calls atomic.Int32
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		time.Sleep(30 * time.Millisecond)
		return codexOverloadedResponse(), nil
	})
	config := &CodexOverloadFailoverConfig{RetryBudget: 100 * time.Millisecond}
	fastCapacityGaps(config, time.Millisecond)
	transport := codexOverloadFailoverTransport{base: base, server: &Server{CodexOverloadFailover: config}, account: "codex-account-0", poolModel: "gpt-6-astra"}
	request, _ := http.NewRequest(http.MethodPost, "https://chatgpt.example/responses", bytes.NewReader([]byte(`{}`)))
	started := time.Now()
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	// ~30ms per attempt inside a 100ms budget: about four attempts, well
	// below the count bound the 1ms gaps alone would allow.
	if got := calls.Load(); got < 2 || got > 5 {
		t.Fatalf("upstream attempts = %d, want 2-5 inside a 100ms wall-clock budget", got)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("ladder took %v, want it bounded by the 100ms budget plus one attempt", elapsed)
	}
}

// A client cancel during a same-account gap ends the Codex ladder at once.
func TestCodexCapacityStaySleepHonorsCancel(t *testing.T) {
	var calls atomic.Int32
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return codexOverloadedResponse(), nil
	})
	config := &CodexOverloadFailoverConfig{sameAccountGap: func() time.Duration { return 5 * time.Second }}
	transport := codexOverloadFailoverTransport{base: base, server: &Server{CodexOverloadFailover: config}, account: "codex-account-0", poolModel: "gpt-6-astra"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://chatgpt.example/responses", bytes.NewReader([]byte(`{}`)))
	time.AfterFunc(50*time.Millisecond, cancel)
	started := time.Now()
	response, _ := transport.RoundTrip(request)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("RoundTrip returned after %v, want it to stop promptly on cancel instead of sleeping the 5s gap", elapsed)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream attempts = %d, want no retry after the cancel", got)
	}
}

// The fork's: the stay ladder's own budget (codexStayBudget), which upstream
// pins only inside its X-Subrouter-Retry header test (not ported). A
// configured fallback caps the operator's wait at ~10s, an unbounded one
// included; RetryBudget overrides everything; without a fallback the
// operator's wait and its unboundedness stand. The fork has no Azure or egress
// configured today, so in production this is the first row.
func TestCodexStayBudgetCappedOnlyByAFallback(t *testing.T) {
	egress := &CodexEgressConfig{Proxies: []*url.URL{mustParseURL(t, "http://egress.example:3128")}}
	azure := &AzureCodexConfig{Endpoints: []AzureCodexEndpoint{{BaseURL: mustParseURL(t, "https://azure.example/openai/v1"), APIKey: "key"}}}
	cases := []struct {
		name          string
		server        Server
		want          time.Duration
		wantUnbounded bool
	}{
		{"no fallback", Server{}, codexCapacityDefaultStayMaxWait, false},
		{"no fallback, operator wait", Server{CodexOverloadFailover: &CodexOverloadFailoverConfig{StayMaxWait: 90 * time.Second}}, 90 * time.Second, false},
		{"no fallback, unbounded", Server{CodexOverloadFailover: &CodexOverloadFailoverConfig{StayUnbounded: true}}, codexCapacityDefaultStayMaxWait, true},
		{"egress", Server{CodexEgress: egress}, codexCapacityFallbackStayRetryBudget, false},
		{"azure", Server{AzureCodex: azure}, codexCapacityFallbackStayRetryBudget, false},
		{"azure, unbounded", Server{AzureCodex: azure, CodexOverloadFailover: &CodexOverloadFailoverConfig{StayUnbounded: true}}, codexCapacityFallbackStayRetryBudget, false},
		{"egress, shorter operator wait", Server{CodexEgress: egress, CodexOverloadFailover: &CodexOverloadFailoverConfig{StayMaxWait: 5 * time.Second}}, 5 * time.Second, false},
		{"failover on, kept conversation, egress", Server{CodexEgress: egress, CodexOverloadFailover: &CodexOverloadFailoverConfig{Enabled: true}}, codexCapacityFallbackStayRetryBudget, false},
		{"retry budget wins", Server{CodexEgress: egress, CodexOverloadFailover: &CodexOverloadFailoverConfig{RetryBudget: time.Minute, StayUnbounded: true}}, time.Minute, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			budget, unbounded := test.server.codexStayBudget(test.server.CodexOverloadFailover.stayPolicy())
			if budget != test.want || unbounded != test.wantUnbounded {
				t.Fatalf("stay budget = %v unbounded=%t, want %v unbounded=%t", budget, unbounded, test.want, test.wantUnbounded)
			}
		})
	}
}
