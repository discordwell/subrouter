package proxy

import (
	"net/http/httptest"
	"testing"
)

// The loopback exemption exists for a client that CANNOT present the token --
// the ChatGPT/Codex desktop app honours only CODEX_API_BASE_URL and has no way
// to set a header, so every /backend-api request it makes is otherwise a 401.
//
// The boundary that matters is that it stays loopback-only and opt-in. The
// alternative once tried -- clearing LocalProxyToken outright -- is refused by
// startup validation and crash-looped the daemon, so this must not quietly
// become the same thing for remote callers.
func TestLoopbackProxyExemption(t *testing.T) {
	const token = "real-local-proxy-token"

	cases := []struct {
		name       string
		allow      bool
		remoteAddr string
		authHeader string
		want       bool
	}{
		{"off: loopback without a token is refused", false, "127.0.0.1:51000", "", false},
		{"on: loopback without a token is admitted", true, "127.0.0.1:51000", "", true},
		{"on: loopback IPv6 is admitted", true, "[::1]:51000", "", true},

		// The whole point of the flag being narrow.
		{"on: tailnet caller is still refused", true, "100.64.10.5:51000", "", false},
		{"on: LAN caller is still refused", true, "192.168.1.20:51000", "", false},
		{"off: tailnet caller is refused", false, "100.64.10.5:51000", "", false},

		// A forgeable header must not stand in for the socket peer.
		{"on: spoofed forwarded-for does not make a remote caller local",
			true, "100.64.10.5:51000", "", false},

		// The token itself keeps working regardless of the flag.
		{"off: correct bearer is admitted", false, "100.64.10.5:51000", "Bearer " + token, true},
		{"on: correct bearer is admitted", true, "100.64.10.5:51000", "Bearer " + token, true},
		{"off: wrong bearer is refused", false, "127.0.0.1:51000", "Bearer nope", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := Server{
				LocalProxyToken:                token,
				AllowLoopbackProxyWithoutToken: tc.allow,
			}
			r := httptest.NewRequest("POST", "/backend-api/codex/responses", nil)
			r.RemoteAddr = tc.remoteAddr
			if tc.authHeader != "" {
				r.Header.Set("Authorization", tc.authHeader)
			}
			// Set on every case: if the implementation ever consults it, the
			// remote-caller rows above fail rather than silently passing.
			r.Header.Set("X-Forwarded-For", "127.0.0.1")

			if got := server.localProxyAuthorized(r); got != tc.want {
				t.Fatalf("localProxyAuthorized(remote=%s, allow=%v) = %v, want %v",
					tc.remoteAddr, tc.allow, got, tc.want)
			}
		})
	}
}

// With no token configured at all the daemon serves unenforced, and the flag
// must not change that in either direction.
func TestNoTokenServesUnenforcedRegardlessOfFlag(t *testing.T) {
	for _, allow := range []bool{false, true} {
		server := Server{LocalProxyToken: "", AllowLoopbackProxyWithoutToken: allow}
		r := httptest.NewRequest("POST", "/backend-api/codex/responses", nil)
		r.RemoteAddr = "100.64.10.5:51000"
		if !server.localProxyAuthorized(r) {
			t.Fatalf("allow=%v: an unset token must serve unenforced", allow)
		}
	}
}
