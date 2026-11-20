package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// End-to-end checks on the public listener as an actual TLS server, rather than
// on PublicHandler in isolation. The handler tests prove the route split; these
// prove the wiring around it -- that it terminates TLS with the box's own
// pinnable identity, and that the admin surface is still absent once a real
// connection is involved.

// startPublicTLS serves PublicHandler over TLS on a loopback port using the
// box's persisted identity, mirroring how serve() constructs it.
func startPublicTLS(t *testing.T, server Server) (addr string, fingerprint string) {
	t.Helper()
	identity, err := LoadOrCreateBoxIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{
		Handler: server.PublicHandler(),
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{identity.Certificate},
			MinVersion:   tls.VersionTLS12,
		},
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = httpServer.ServeTLS(listener, "", "") }()
	t.Cleanup(func() { _ = httpServer.Close() })
	return listener.Addr().String(), identity.Fingerprint
}

// pinnedClient verifies by fingerprint, the way a peer will.
func pinnedClient(pin string) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // replaced by the pin check below
			VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
				if len(rawCerts) == 0 {
					return errNoPeerCertificate
				}
				if CertificateFingerprint(rawCerts[0]) != pin {
					return errPinMismatch
				}
				return nil
			},
		}},
	}
}

func TestPublicListenerServesProviderTrafficOverTLS(t *testing.T) {
	addr, pin := startPublicTLS(t, Server{Sessions: newSessionStore(t)})

	resp, err := pinnedClient(pin).Get("https://" + addr + "/_subrouter/health")
	if err != nil {
		t.Fatalf("pinned TLS request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health over the public listener = %d, want 200", resp.StatusCode)
	}
}

// Plaintext to a TLS listener must fail. Without this, a misconfiguration that
// silently serves the public mux in the clear would go unnoticed -- and every
// bearer token crossing it would be readable.
func TestPublicListenerRefusesPlaintext(t *testing.T) {
	addr, _ := startPublicTLS(t, Server{Sessions: newSessionStore(t)})

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + addr + "/_subrouter/health")
	if err != nil {
		return // handshake refused outright, which is the stronger outcome
	}
	defer resp.Body.Close()

	// Go's TLS server recognises a plaintext request and answers 400 "Client
	// sent an HTTP request to an HTTPS server" rather than erroring, so a bare
	// `err == nil` check would call that a success. What must be true is that
	// the request was not SERVED: no handler ran, so no 200.
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("plaintext HTTP was served by the TLS listener (%d): the public surface is "+
			"reachable in the clear and every bearer token crossing it is readable",
			resp.StatusCode)
	}
}

// The credential split, over the wire.
//
// Two powers, deliberately different: the PROXY token spends the pool and reads
// what the pool looks like; the ADMIN token enumerates, drains, imports and
// mints. Admin is a strict superset, so it is not accepted on the public
// listener at all -- it would buy a caller nothing the proxy token cannot do
// while putting the more powerful secret on the public wire.
func TestPublicListenerCredentialSplit(t *testing.T) {
	server := Server{
		Sessions:        newSessionStore(t),
		AdminToken:      "admin-token",
		LocalProxyToken: "proxy-token",
	}
	addr, pin := startPublicTLS(t, server)
	client := pinnedClient(pin)

	get := func(path, token string) int {
		req, err := http.NewRequest(http.MethodGet, "https://"+addr+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// Stats: readable with the SPEND credential. A peer must see capacity to be
	// rankable at all -- the cross-pool ranking reads usage-status, and without it a
	// publicly-reached peer is invisible to the scheduler.
	for _, path := range []string{"/_subrouter/accounts", "/_subrouter/usage-status"} {
		if code := get(path, "proxy-token"); code != http.StatusOK {
			t.Errorf("%s with the proxy token = %d, want 200: a peer cannot read capacity "+
				"and would never be chosen", path, code)
		}
		if code := get(path, ""); code != http.StatusUnauthorized {
			t.Errorf("%s with NO credential = %d, want 401", path, code)
		}
		// The whole point of the split.
		if code := get(path, "admin-token"); code != http.StatusUnauthorized {
			t.Errorf("%s accepted the ADMIN token publicly (%d); the more powerful secret is "+
				"now expected on the public wire for a capability the proxy token already has",
				path, code)
		}
	}

	// Everything that is not stats stays absent, admin token or not.
	for _, path := range []string{
		"/_subrouter/sessions",
		"/_subrouter/pool.json",
		"/_subrouter/drain-status",
		"/_subrouter/transcripts",
		"/internal/v1/session-leases",
	} {
		for _, token := range []string{"", "proxy-token", "admin-token"} {
			if code := get(path, token); code != http.StatusNotFound {
				t.Errorf("%s (token=%q) = %d, want 404: a non-stats admin route is "+
					"internet-reachable", path, token, code)
			}
		}
	}

	// And the private listener is untouched by any of it.
	privateReq := httptest.NewRequest(http.MethodGet, "/_subrouter/sessions", nil)
	privateReq.RemoteAddr = "127.0.0.1:5555"
	privateReq.Header.Set("Authorization", "Bearer admin-token")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, privateReq)
	if rec.Code == http.StatusNotFound {
		t.Error("/_subrouter/sessions 404s on the PRIVATE listener; the split broke the " +
			"existing surface")
	}
}

// A wrong pin must fail the handshake. This is the check the entire trust model
// rests on, so it is exercised against a real server rather than a fake.
func TestPublicListenerRejectsAWrongPin(t *testing.T) {
	addr, pin := startPublicTLS(t, Server{Sessions: newSessionStore(t)})

	wrong := "0000000000000000000000000000000000000000000000000000000000000000"
	if wrong == pin {
		t.Fatal("fixture collision")
	}
	resp, err := pinnedClient(wrong).Get("https://" + addr + "/_subrouter/health")
	if err == nil {
		resp.Body.Close()
		t.Fatal("a client pinning the WRONG fingerprint completed the handshake; the pin " +
			"is decorative and any TLS server could impersonate this box")
	}
}

var (
	errNoPeerCertificate = tlsPinError("peer presented no certificate")
	errPinMismatch       = tlsPinError("certificate fingerprint does not match the pin")
)

type tlsPinError string

func (e tlsPinError) Error() string { return string(e) }
