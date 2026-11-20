package proxy

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// The certificate pin is the ENTIRE trust decision for a public peer. Boxes are
// self-signed by design, so there is no CA to fall back on: if the pin does not
// hold, any TLS server on the internet can present itself as the peer and
// receive forwarded traffic — bearer token included.
//
// InsecureSkipVerify is set alongside it, which is correct (the cert is
// self-signed and carries no SANs) and is also exactly how this check becomes
// decorative. Every test here is calibrated against the decorative form.

// tlsPeer stands up a TLS server with its own box identity and returns its
// address and fingerprint.
func tlsPeer(t *testing.T, handler http.Handler) (addr, fingerprint string) {
	t.Helper()
	identity, err := LoadOrCreateBoxIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{
		Handler:   handler,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{identity.Certificate}},
	}
	go func() { _ = server.ServeTLS(listener, "", "") }()
	t.Cleanup(func() { _ = server.Close() })
	return listener.Addr().String(), identity.Fingerprint
}

const otherPin = "1111111111111111111111111111111111111111111111111111111111111111"

// HTTP path: the right pin connects.
func TestPeerPinAcceptsTheMatchingCertificate(t *testing.T) {
	addr, pin := tlsPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	pool := NewRemotePool("https://"+addr, "", "").WithFingerprint(pin)

	resp, err := pool.client.Get("https://" + addr + "/_subrouter/health")
	if err != nil {
		t.Fatalf("the CORRECT pin was rejected: %v", err)
	}
	defer resp.Body.Close()
}

// HTTP path: the wrong pin must not.
func TestPeerPinRejectsWrongCertificate(t *testing.T) {
	addr, pin := tlsPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	if pin == otherPin {
		t.Fatal("fixture collision")
	}
	pool := NewRemotePool("https://"+addr, "", "").WithFingerprint(otherPin)

	resp, err := pool.client.Get("https://" + addr + "/_subrouter/health")
	if err == nil {
		resp.Body.Close()
		t.Fatal("a peer presenting a DIFFERENT certificate was accepted; the pin verifies " +
			"nothing and any TLS server can impersonate this peer")
	}
	if !strings.Contains(err.Error(), "does not match the pin") {
		t.Errorf("connection failed for the wrong reason (%v); the pin may not be what "+
			"refused it", err)
	}
}

// WEBSOCKET path. Codex opens every model turn as an upgrade, so a pin enforced
// only on the HTTP client would leave the majority of peer traffic unverified
// while every HTTP-path test above still passed.
func TestPeerWebSocketPinIsEnforced(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	addr, pin := tlsPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(websocket.TextMessage, []byte("ok"))
	}))

	good := NewRemotePool("https://"+addr, "", "").WithFingerprint(pin)
	conn, _, err := good.webSocketDialer().Dial("wss://"+addr+"/v1/responses", nil)
	if err != nil {
		t.Fatalf("the CORRECT pin was rejected on the websocket path: %v", err)
	}
	conn.Close()

	bad := NewRemotePool("https://"+addr, "", "").WithFingerprint(otherPin)
	badConn, _, err := bad.webSocketDialer().Dial("wss://"+addr+"/v1/responses", nil)
	if err == nil {
		badConn.Close()
		t.Fatal("the websocket dialer accepted a peer with a DIFFERENT certificate; every " +
			"Codex model turn would be unverified")
	}
}

// Pins are PER PEER. A shared dialer would carry one peer's pin to all of them.
func TestEachPeerCarriesItsOwnPin(t *testing.T) {
	first := NewRemotePool("https://a.example:31415", "", "").WithFingerprint(otherPin)
	second := NewRemotePool("https://b.example:31415", "", "").
		WithFingerprint("2222222222222222222222222222222222222222222222222222222222222222")

	if first.webSocketDialer() == second.webSocketDialer() {
		t.Fatal("two peers share one websocket dialer; they cannot both be pinned correctly")
	}
	firstConfig := first.webSocketDialer().TLSClientConfig
	secondConfig := second.webSocketDialer().TLSClientConfig
	if firstConfig == secondConfig {
		t.Fatal("two peers share one TLS config; one peer's pin governs the other")
	}
	// The ALPN pin is protocol-level and must survive pinning: an upgrade
	// cannot run over h2.
	for name, config := range map[string]*tls.Config{"first": firstConfig, "second": secondConfig} {
		if len(config.NextProtos) == 0 || config.NextProtos[0] != "http/1.1" {
			t.Errorf("%s peer lost the http/1.1 ALPN pin; websocket cannot upgrade over h2", name)
		}
	}
}

// No pin must NOT mean "trust anything". Without a fingerprint the dialers keep
// ordinary CA validation, so a self-signed peer — which is what every box is —
// is refused rather than accepted unverified.
func TestMissingPinRefusesSelfSignedRatherThanTrusting(t *testing.T) {
	addr, _ := tlsPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	pool := NewRemotePool("https://"+addr, "", "") // deliberately no WithFingerprint

	resp, err := pool.client.Get("https://" + addr + "/_subrouter/health")
	if err == nil {
		resp.Body.Close()
		t.Fatal("a peer with NO configured pin accepted a self-signed certificate; " +
			"forgetting the pin silently disables verification instead of failing")
	}
}

// The plain-HTTP tailnet peer keeps working untouched: pinning must not become
// a requirement for the deployment that exists today.
func TestPlainHTTPPeerIsUnaffectedByPinning(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer peer.Close()

	pool := NewRemotePool(peer.URL, "", "")
	resp, err := pool.client.Get(peer.URL + "/_subrouter/health")
	if err != nil {
		t.Fatalf("plain-HTTP peer broke: %v", err)
	}
	defer resp.Body.Close()
}
