package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The limiter guards a cost-amplification path: without it, a public endpoint
// lets anyone who reaches it spend pooled subscription accounts as fast as they
// can open connections.
//
// It is also the change in this phase most able to cause an outage by itself. A
// fan-out session was measured at ~1000 requests in 20 minutes and every one is
// real work, so the default is observe and the tests below pin BOTH that
// enforcement works when asked for and that it is not on by default.

func limiterFor(t *testing.T, mode RateLimitMode) (*rateLimiter, *time.Time) {
	t.Helper()
	clock := time.Now()
	limiter := newRateLimiter(mode, nil)
	limiter.now = func() time.Time { return clock }
	return limiter, &clock
}

func TestRateLimiterRefusesOnlyPastTheBurst(t *testing.T) {
	limiter, _ := limiterFor(t, RateLimitEnforce)

	for i := 0; i < int(publicRateBurst); i++ {
		if !limiter.allow("t:abc") {
			t.Fatalf("refused request %d, inside the burst of %.0f", i+1, publicRateBurst)
		}
	}
	if limiter.allow("t:abc") {
		t.Fatal("the bucket never empties; the limiter cannot refuse anything")
	}
}

// Tokens come back over time, or a limiter is just a hard cap.
func TestRateLimiterRefills(t *testing.T) {
	limiter, clock := limiterFor(t, RateLimitEnforce)
	for i := 0; i < int(publicRateBurst); i++ {
		limiter.allow("t:abc")
	}
	if limiter.allow("t:abc") {
		t.Fatal("fixture broken: bucket should be empty")
	}

	*clock = clock.Add(time.Second)
	allowed := 0
	for i := 0; i < int(publicRatePerSecond); i++ {
		if limiter.allow("t:abc") {
			allowed++
		}
	}
	if allowed == 0 {
		t.Fatal("no tokens returned after a second; the bucket never refills and a principal " +
			"is locked out permanently after one burst")
	}
}

// Principals must not share a bucket, or one busy caller starves everyone.
func TestRateLimiterSeparatesPrincipals(t *testing.T) {
	limiter, _ := limiterFor(t, RateLimitEnforce)
	for i := 0; i < int(publicRateBurst)+10; i++ {
		limiter.allow("t:busy")
	}
	if !limiter.allow("t:quiet") {
		t.Fatal("a second principal was refused because the first exhausted its bucket")
	}
}

// The token is the principal; the address is only a fallback. Keying primarily
// on IP would let one laptop exhaust a bucket shared by everyone behind a NAT.
func TestRatePrincipalPrefersTheTokenOverTheAddress(t *testing.T) {
	withToken := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	withToken.RemoteAddr = "203.0.113.9:5555"
	withToken.Header.Set("Authorization", "Bearer secret-value")

	key, kind := ratePrincipal(withToken)
	if kind != "token" {
		t.Fatalf("principal kind = %q, want token", kind)
	}
	if strings.Contains(key, "secret-value") {
		t.Fatalf("the raw bearer is in the bucket key (%q); a heap dump or a log line would "+
			"leak a live credential", key)
	}

	sameTokenDifferentHost := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	sameTokenDifferentHost.RemoteAddr = "198.51.100.4:5555"
	sameTokenDifferentHost.Header.Set("Authorization", "Bearer secret-value")
	if otherKey, _ := ratePrincipal(sameTokenDifferentHost); otherKey != key {
		t.Error("one token from two addresses got two buckets; the limit is per-connection " +
			"rather than per-principal")
	}

	anonymous := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	anonymous.RemoteAddr = "203.0.113.9:5555"
	if _, kind := ratePrincipal(anonymous); kind != "addr" {
		t.Errorf("credential-free request keyed as %q, want the address fallback", kind)
	}
}

// Observe is the default and must refuse NOTHING.
func TestObserveModeRefusesNothing(t *testing.T) {
	limiter, _ := limiterFor(t, RateLimitObserve)
	handler := limiter.middleware(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))

	for i := 0; i < int(publicRateBurst)+50; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		req.RemoteAddr = "203.0.113.9:5555"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("observe mode returned 429 on request %d; the default mode is throttling "+
				"real traffic", i+1)
		}
	}
}

// An unset or misspelled mode must not silently arm or disable enforcement.
func TestUnknownModeFallsBackToObserve(t *testing.T) {
	for _, mode := range []RateLimitMode{"", "enforcing", "ENFORCE", "yes"} {
		if got := newRateLimiter(mode, nil).mode; got != RateLimitObserve {
			t.Errorf("mode %q resolved to %q, want observe", mode, got)
		}
	}
	if got := newRateLimiter(RateLimitEnforce, nil).mode; got != RateLimitEnforce {
		t.Error("the exact enforce value stopped arming enforcement")
	}
}

// Enforcement, end to end through the middleware.
func TestEnforceModeReturns429(t *testing.T) {
	limiter, _ := limiterFor(t, RateLimitEnforce)
	handler := limiter.middleware(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))

	var last int
	for i := 0; i < int(publicRateBurst)+5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		req.RemoteAddr = "203.0.113.9:5555"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		last = rec.Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("last response = %d, want 429 once the burst is spent", last)
	}
}

// The PRIVATE listener must never be rate limited: it is reached over the
// tailnet or loopback by our own fleet, where the limiter could only throttle
// real work.
func TestPrivateHandlerIsNotRateLimited(t *testing.T) {
	server := Server{
		Sessions:        newSessionStore(t),
		PublicRateLimit: RateLimitEnforce,
	}
	handler := server.Handler()

	for i := 0; i < int(publicRateBurst)+50; i++ {
		req := httptest.NewRequest(http.MethodGet, "/_subrouter/health", nil)
		req.RemoteAddr = "127.0.0.1:5555"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("the private handler rate limited request %d; enforcing on the public "+
				"listener leaked onto the tailnet surface", i+1)
		}
	}
}

// Buckets must not accumulate forever on a public endpoint, where the set of
// source addresses is unbounded.
func TestIdleBucketsAreEvicted(t *testing.T) {
	limiter, clock := limiterFor(t, RateLimitEnforce)
	for i := 0; i < 100; i++ {
		limiter.allow(string(rune('a'+i%26)) + string(rune('0'+i/26)))
	}
	before := len(limiter.buckets)

	*clock = clock.Add(publicRateIdleEviction + time.Minute)
	limiter.allow("t:fresh")

	if len(limiter.buckets) >= before {
		t.Fatalf("buckets grew from %d to %d after the idle window; the map is unbounded on a "+
			"public endpoint", before, len(limiter.buckets))
	}
}
