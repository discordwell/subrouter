package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestLoopbackAdminAcceptsConfiguredPublicHost is upstream #374's guard for
// the Host check's two escape hatches (PATCHES §29). It is kept apart from
// admin_origin_test.go only because Server.PublicURL does not exist on the
// pre-port tree, and a file that does not compile there would have hidden the
// red run of every other test in the package. It is a guard, not a regression
// test: on that tree loopback was trusted unconditionally, so it passed there
// too. Its failing case is a mutation of the fix (drop the PublicURL clause,
// or the token waiver, from the Host check), not the old tree.
func TestLoopbackAdminAcceptsConfiguredPublicHost(t *testing.T) {
	server := newAdminOriginTestServer(t)
	server.PublicURL = "https://Subrouter.Example.test/"
	handler := server.Handler()
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, adminOriginRequest(http.MethodGet, "/_subrouter/drain-status", "subrouter.example.test", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("public host status = %d, want 200", resp.Code)
	}

	resp = httptest.NewRecorder()
	req := adminOriginRequest(http.MethodGet, "/_subrouter/drain-status", "other.example.test", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("token-bearing loopback request with other host status = %d, want 200", resp.Code)
	}

	// Neither hatch is a wildcard: another name with no token is refused.
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, adminOriginRequest(http.MethodGet, "/_subrouter/drain-status", "other.example.test", nil))
	if resp.Code != http.StatusForbidden {
		t.Fatalf("other host without token status = %d, want 403", resp.Code)
	}
}
