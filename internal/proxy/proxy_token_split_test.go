package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Splitting SUBROUTER_PROXY_TOKEN from SUBROUTER_ADMIN_TOKEN is what lets a
// spend-only credential exist -- someone who should use the pool but never
// enumerate its accounts, drain it, or import into it.
//
// It only works because admin is a SUPERSET of proxy. A peer daemon sends one
// bearer for both surfaces (it reads /_subrouter/usage-status, which is
// requireAdmin, and forwards through the proxy gate), so if the admin token did
// not open the proxy, splitting would break the peer in one direction or the
// other -- and the scheduler going blind to peer capacity is the silent half.

func proxyGateRequest(token string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.RemoteAddr = "100.64.0.9:54321" // NOT loopback: the exemption must not decide this
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func TestAdminTokenOpensTheProxyGate(t *testing.T) {
	server := Server{LocalProxyToken: "proxy-only-token", AdminToken: "admin-token-value"}

	for name, tc := range map[string]struct {
		token string
		want  bool
	}{
		"proxy token":          {"proxy-only-token", true},
		"admin token":          {"admin-token-value", true}, // the superset rule
		"unrelated token":      {"neither-of-them", false},
		"no token":             {"", false},
		"proxy token, 1 short": {"proxy-only-toke", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := server.localProxyAuthorized(proxyGateRequest(tc.token)); got != tc.want {
				t.Fatalf("localProxyAuthorized(%q) = %v, want %v", tc.token, got, tc.want)
			}
		})
	}
}

// The whole point of the split: the proxy token must NOT reach admin. If this
// ever passes, a shared spend credential silently carries drain and
// account-enumeration with it.
func TestProxyTokenDoesNotReachAdmin(t *testing.T) {
	server := Server{LocalProxyToken: "proxy-only-token", AdminToken: "admin-token-value"}

	req := httptest.NewRequest(http.MethodGet, "/_subrouter/accounts", nil)
	req.RemoteAddr = "100.64.0.9:54321"
	req.Header.Set("Authorization", "Bearer proxy-only-token")
	if server.authorizeAdmin(req) {
		t.Fatal("proxy token authorized ADMIN — the split is decorative and the credential is not spend-only")
	}

	req.Header.Set("Authorization", "Bearer admin-token-value")
	if !server.authorizeAdmin(req) {
		t.Fatal("admin token did not authorize admin")
	}
}

// An unset admin token must not become a wildcard on the proxy gate. Empty
// compared against empty is the classic way a guard turns into an open door.
func TestEmptyAdminTokenIsNotAProxyWildcard(t *testing.T) {
	server := Server{LocalProxyToken: "proxy-only-token"} // AdminToken deliberately empty

	if server.localProxyAuthorized(proxyGateRequest("")) {
		t.Fatal("empty bearer authorized against an empty AdminToken")
	}
	if server.localProxyAuthorized(proxyGateRequest("anything")) {
		t.Fatal("arbitrary bearer authorized against an empty AdminToken")
	}
	if !server.localProxyAuthorized(proxyGateRequest("proxy-only-token")) {
		t.Fatal("the real proxy token stopped working")
	}
}

// Equal tokens (today's deployed config) must keep working unchanged, so the
// code change is safe to ship BEFORE the tokens are actually split.
func TestEqualTokensStillAuthorizeBothSurfaces(t *testing.T) {
	const shared = "same-value-both-surfaces"
	server := Server{LocalProxyToken: shared, AdminToken: shared}

	if !server.localProxyAuthorized(proxyGateRequest(shared)) {
		t.Fatal("shared token stopped opening the proxy")
	}
	adminReq := httptest.NewRequest(http.MethodGet, "/_subrouter/accounts", nil)
	adminReq.RemoteAddr = "100.64.0.9:54321"
	adminReq.Header.Set("Authorization", "Bearer "+shared)
	if !server.authorizeAdmin(adminReq) {
		t.Fatal("shared token stopped opening admin")
	}
}
