package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The limiter has to actually be reachable from the Server field, and it has to
// REFUSE when told to. Both halves have failed independently elsewhere in this
// phase: a flag that was never read, and a mode string that normalizes to
// observe when it is not recognised.
func TestPublicListenerEnforcesWhenAsked(t *testing.T) {
	server := Server{
		LocalProxyToken: "spend-token",
		PublicRateLimit: RateLimitEnforce,
	}
	handler := server.PublicHandler()

	// Liveness is unauthenticated, so it exercises the limiter without dragging
	// in the proxy path or a real upstream.
	var lastCode int
	sawRefusal := false
	for i := 0; i < int(publicRateBurst)+50; i++ {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/_subrouter/health", nil)
		request.RemoteAddr = "203.0.113.7:5555"
		handler.ServeHTTP(recorder, request)
		lastCode = recorder.Code
		if recorder.Code == http.StatusTooManyRequests {
			sawRefusal = true
			break
		}
	}
	if !sawRefusal {
		t.Fatalf("burst past the limit was never refused (last status %d); an internet-facing "+
			"listener would absorb an unauthenticated flood for free", lastCode)
	}
}

// Observe mode must refuse NOTHING, or "log before enforcing" is not available
// as a rollout step.
func TestPublicListenerObserveRefusesNothing(t *testing.T) {
	server := Server{
		LocalProxyToken: "spend-token",
		PublicRateLimit: RateLimitObserve,
	}
	handler := server.PublicHandler()

	for i := 0; i < int(publicRateBurst)+50; i++ {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/_subrouter/health", nil)
		request.RemoteAddr = "203.0.113.8:5555"
		handler.ServeHTTP(recorder, request)
		if recorder.Code == http.StatusTooManyRequests {
			t.Fatalf("observe mode returned 429 at request %d; it must only log", i)
		}
	}
}

// The PRIVATE listener is never rate limited, whatever the public mode says.
// Throttling loopback tooling or a tailnet peer would be an outage caused by a
// setting whose entire scope is supposed to be the public surface.
func TestPrivateListenerIsNeverRateLimited(t *testing.T) {
	server := Server{
		LocalProxyToken:                "spend-token",
		PublicRateLimit:                RateLimitEnforce,
		AllowLoopbackProxyWithoutToken: true,
	}
	handler := server.Handler()

	for i := 0; i < int(publicRateBurst)+50; i++ {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/_subrouter/health", nil)
		request.RemoteAddr = "127.0.0.1:5555"
		handler.ServeHTTP(recorder, request)
		if recorder.Code == http.StatusTooManyRequests {
			t.Fatalf("the private listener returned 429 at request %d; the limiter must be "+
				"confined to the public mux", i)
		}
	}
}
