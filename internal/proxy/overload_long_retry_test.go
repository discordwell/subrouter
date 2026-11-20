package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// The Codex same-account capacity wait is long by default (4m), capped in
// wall-clock time, unbounded with max-wait 0, and visible: a gauge of held
// requests. Clocks and sleeps are injected; nothing here really waits.
// Upstream manaflow-ai/subrouter #389 (overload_long_retry_test.go as at
// c9303d3), Codex tests only (PATCHES section 27).

// fakeOverloadClock is an injected clock that the fake sleep advances.
type fakeOverloadClock struct {
	now time.Time
}

func newFakeOverloadClock() *fakeOverloadClock {
	return &fakeOverloadClock{now: time.Unix(1_800_000_000, 0)}
}

func (c *fakeOverloadClock) Now() time.Time { return c.now }

// codexStayTransport is the capacity layer with the failover off, an
// injected clock and a sleep that advances it.
func codexStayTransport(config *CodexOverloadFailoverConfig, clock *fakeOverloadClock, onSleep func(context.Context, time.Duration) bool) (codexOverloadFailoverTransport, *Server, *int) {
	calls := 0
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return codexOverloadedResponse(), nil
	})
	server := &Server{CodexOverloadFailover: config, overloadHeld: newOverloadHeldGauge()}
	transport := codexOverloadFailoverTransport{
		base: base, server: server, account: "codex-account-0", poolModel: "gpt-6-astra",
		now: clock.Now,
		sleep: func(ctx context.Context, d time.Duration) bool {
			clock.now = clock.now.Add(d)
			if onSleep != nil {
				return onSleep(ctx, d)
			}
			return ctx.Err() == nil
		},
	}
	return transport, server, &calls
}

func codexStayRequest(ctx context.Context) *http.Request {
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://chatgpt.example/responses", bytes.NewReader([]byte(`{}`)))
	return request
}

// Default Codex ladder: 0.5s doubling to 8s, then a steady 8-10s, until the
// next wait would end past 4m of wall-clock time. No count bound.
func TestCodexCapacityDefaultStayLadderRunsFourMinutes(t *testing.T) {
	clock := newFakeOverloadClock()
	started := clock.now
	var gaps []time.Duration
	var heldDuring []int64
	var server *Server
	transport, server, calls := codexStayTransport(&CodexOverloadFailoverConfig{}, clock, func(ctx context.Context, d time.Duration) bool {
		gaps = append(gaps, d)
		heldDuring = append(heldDuring, server.overloadHeld.codex.Load())
		return true
	})
	response, err := transport.RoundTrip(codexStayRequest(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	elapsed := clock.now.Sub(started)
	if elapsed > codexCapacityDefaultStayMaxWait || elapsed < codexCapacityDefaultStayMaxWait-10*time.Second {
		t.Fatalf("ladder waited %v, want just inside the 4m cap", elapsed)
	}
	if len(gaps) < 25 || *calls != 1+len(gaps) {
		t.Fatalf("gaps = %d, calls = %d, want about 27 retries with no count bound", len(gaps), *calls)
	}
	for i, gap := range gaps {
		if i >= 5 && (gap < 8*time.Second || gap > 10*time.Second) {
			t.Fatalf("gap %d = %v (all %v), want a steady 8-10s after the ramp", i, gap, gaps)
		}
		if heldDuring[i] != 1 {
			t.Fatalf("gauge during gap %d = %d, want 1", i, heldDuring[i])
		}
	}
	if got := server.overloadHeld.codex.Load(); got != 0 {
		t.Fatalf("gauge after the ladder = %d, want 0", got)
	}
}

// StayMaxWait sets the Codex cap; StayUnbounded runs until the client
// cancels, and the cancel returns promptly and releases the gauge.
func TestCodexCapacityStayMaxWaitAndUnbounded(t *testing.T) {
	clock := newFakeOverloadClock()
	started := clock.now
	transport, _, _ := codexStayTransport(&CodexOverloadFailoverConfig{StayMaxWait: 30 * time.Second}, clock, nil)
	response, err := transport.RoundTrip(codexStayRequest(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if elapsed := clock.now.Sub(started); elapsed > 30*time.Second || elapsed < 15*time.Second {
		t.Fatalf("ladder waited %v, want it capped at 30s", elapsed)
	}

	clock = newFakeOverloadClock()
	started = clock.now
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	retries := 0
	var server *Server
	transport, server, _ = codexStayTransport(&CodexOverloadFailoverConfig{StayUnbounded: true}, clock, func(ctx context.Context, _ time.Duration) bool {
		retries++
		if retries == 500 { // well over an hour of simulated waiting
			cancel()
		}
		return ctx.Err() == nil
	})
	response, _ = transport.RoundTrip(codexStayRequest(ctx))
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if retries != 500 || clock.now.Sub(started) < time.Hour {
		t.Fatalf("retries = %d over %v, want 500 retries past an hour until the cancel", retries, clock.now.Sub(started))
	}
	if got := server.overloadHeld.codex.Load(); got != 0 {
		t.Fatalf("gauge after cancel = %d, want 0", got)
	}
}

// /_subrouter/health reports the held-in-overload count. Upstream's test,
// Codex only: the fork's gauge does not count Claude, whose ladder is not
// ported.
func TestOverloadHeldGaugeInHealth(t *testing.T) {
	server, _ := claudeFailoverServer(t)
	handler := server.Handler()
	// Handler() installs the gauge; with nothing held the count is 0.
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/_subrouter/health", nil))
	var health struct {
		Held map[string]int64 `json:"overload_retry_held"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	codex, okCodex := health.Held[string(accounts.ProviderCodex)]
	if !okCodex || codex != 0 || len(health.Held) != 1 {
		t.Fatalf("health overload_retry_held = %v, want codex at 0 and nothing else", health.Held)
	}
	gauge := newOverloadHeldGauge()
	releaseA := gauge.enter(accounts.ProviderCodex)
	releaseB := gauge.enter(accounts.ProviderCodex)
	releaseC := gauge.enter(accounts.ProviderClaude) // not counted
	if got := gauge.snapshot(); got["codex"] != 2 {
		t.Fatalf("snapshot = %v, want codex 2", got)
	}
	releaseA()
	releaseA() // idempotent
	releaseB()
	releaseC()
	if got := gauge.snapshot(); got["codex"] != 0 {
		t.Fatalf("snapshot after release = %v, want zero", got)
	}
}
