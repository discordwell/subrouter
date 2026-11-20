package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Owner decision 8 (2026-09 upstream merge, round 2): authorizeAdmin takes
// upstream's tokenless fallback. With no AdminToken and no AccountImportToken
// configured, a caller of the PRIVATE handler is admin -- the explicit
// AllowUnauthenticatedAdmin opt-in this fork used to require is no longer
// consulted. What stays: a scoped import token is never the only barrier, a
// configured admin token is required once set, forwarded headers never claim
// loopback, and the public listener's copy is admin to nobody
// (TestPublicHandlerGrantsNothingUnderTokenlessAdmin).
func TestAuthorizeAdminTakesUpstreamTokenlessFallback(t *testing.T) {
	cases := []struct {
		name       string
		server     Server
		remoteAddr string
		host       string
		forwarded  string
		want       bool
	}{
		{
			name:       "tokenless remote is admin (upstream's unsecured legacy default)",
			server:     Server{},
			remoteAddr: "100.64.0.9:52011",
			want:       true,
		},
		{
			name:       "the old opt-in changes nothing",
			server:     Server{AllowUnauthenticatedAdmin: true},
			remoteAddr: "100.64.0.9:52011",
			want:       true,
		},
		{
			name:       "a scoped import token is never the only barrier",
			server:     Server{AccountImportToken: "import-only"},
			remoteAddr: "100.64.0.9:52011",
			want:       false,
		},
		{
			name:       "a configured admin token is required once set",
			server:     Server{AdminToken: "admin-token"},
			remoteAddr: "100.64.0.9:52011",
			want:       false,
		},
		{
			name:       "loopback naming this machine keeps admin without a token",
			server:     Server{AdminToken: "admin-token"},
			remoteAddr: "127.0.0.1:40000",
			host:       "127.0.0.1:31415",
			want:       true,
		},
		{
			// PATCHES §29: a page that DNS-rebinds its own name to 127.0.0.1
			// is a loopback peer too; the name it used is what gives it away.
			name:       "loopback naming a foreign host needs the token",
			server:     Server{AdminToken: "admin-token"},
			remoteAddr: "127.0.0.1:40000",
			host:       "rebind.evil.example:31415",
			want:       false,
		},
		{
			name:       "spoofed forwarded header cannot claim loopback",
			server:     Server{AdminToken: "admin-token"},
			remoteAddr: "100.64.0.9:52011",
			forwarded:  "127.0.0.1",
			want:       false,
		},
		{
			name:       "the public listener's copy is admin to nobody, even tokenless",
			server:     Server{}.publicSurfaceServer(),
			remoteAddr: "100.64.0.9:52011",
			want:       false,
		},
		{
			name:       "the public listener's copy refuses loopback too",
			server:     Server{AdminToken: "admin-token"}.publicSurfaceServer(),
			remoteAddr: "127.0.0.1:40000",
			want:       false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "http://example/_subrouter/accounts", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.host != "" {
				req.Host = tc.host
			}
			if tc.forwarded != "" {
				req.Header.Set("X-Forwarded-For", tc.forwarded)
				req.Header.Set("X-Real-IP", tc.forwarded)
			}
			if got := tc.server.authorizeAdmin(req); got != tc.want {
				t.Fatalf("authorizeAdmin = %v, want %v", got, tc.want)
			}
		})
	}
}

// loopbackAdminRequest builds a request whose peer is the daemon host itself,
// which is how the operator-facing admin endpoints are reached in the
// deployments these tests model. httptest.NewRequest's default RemoteAddr
// (192.0.2.1) is a REMOTE peer; with upstream's tokenless fallback that peer is
// admin only while no admin or import token is configured, so tests exercising
// admin endpoint logic still say which peer they mean.
//
// It also names the loopback Host the sr CLI sends. Loopback trust requires
// one since PATCHES §29 (a page that DNS-rebinds its own name to 127.0.0.1
// arrives from a loopback peer too), and httptest.NewRequest's default Host,
// example.com, is exactly that foreign name.
func loopbackAdminRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "127.0.0.1:31415"
	return req
}
