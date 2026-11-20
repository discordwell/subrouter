package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// PATCHES §32: /_subrouter/peers/usage-status hands `sr status` every peer's
// usage body as this daemon's peer link last read it.

// usagePeer is a peer daemon answering usage-status with body.
func usagePeer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != poolPeerUsagePath {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer peer-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(peer.Close)
	return peer
}

func peerViewServer(t *testing.T, peers ...*RemotePool) Server {
	t.Helper()
	server := testServer(t)
	server.RemotePeers = peers
	return server
}

// readPeerView GETs the route through the private handler as the local CLI
// does: loopback, naming this machine, no credential.
func readPeerView(t *testing.T, server Server) PeersUsageStatus {
	t.Helper()
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, loopbackRequest(http.MethodGet, PeersUsageStatusPath, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var view PeersUsageStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return view
}

func sameJSON(t *testing.T, got, want []byte) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("decode %s: %v", got, err)
	}
	if err := json.Unmarshal(want, &b); err != nil {
		t.Fatalf("decode %s: %v", want, err)
	}
	return reflect.DeepEqual(a, b)
}

// Every configured peer (P7), each with its body passed through untouched.
func TestPeersUsageStatusReturnsEveryPeerVerbatim(t *testing.T) {
	bodyA := `[{"id":"box-codex","provider":"codex","auth_mode":"oauth","auth_valid":true,
	  "windows":[{"Name":"primary","UsedPercent":10,"LimitWindowSeconds":604800,"ResetAfterSeconds":500000}],
	  "normalized":{"headroom":0.9,"short_headroom":0.9,"fresh":true,"new_session":{"eligible":true,"reason":"eligible"}}}]`
	bodyB := `[{"id":"two-claude","provider":"claude","auth_mode":"oauth","auth_valid":true,"label":"two"}]`
	a, b := usagePeer(t, bodyA), usagePeer(t, bodyB)
	server := peerViewServer(t,
		NewRemotePool(a.URL, "peer-token", "").WithName("boxa"),
		NewRemotePool(b.URL, "peer-token", "").WithName("boxb"),
	)

	before := time.Now().Add(-time.Second)
	view := readPeerView(t, server)
	if len(view.Peers) != 2 {
		t.Fatalf("peers = %+v, want both configured peers", view.Peers)
	}
	for i, want := range []struct {
		name, url, body string
	}{
		{"boxa", a.URL, bodyA},
		{"boxb", b.URL, bodyB},
	} {
		got := view.Peers[i]
		if got.Name != want.name || got.URL != want.url {
			t.Errorf("peer %d = %s at %s, want %s at %s", i, got.Name, got.URL, want.name, want.url)
		}
		if !got.Reachable || got.Error != "" {
			t.Errorf("peer %s: reachable=%v error=%q, want a live read", got.Name, got.Reachable, got.Error)
		}
		if at, err := time.Parse(time.RFC3339, got.AsOf); err != nil || at.Before(before.Truncate(time.Second)) {
			t.Errorf("peer %s: as_of = %q, want when it was just read", got.Name, got.AsOf)
		}
		if !sameJSON(t, got.Accounts, []byte(want.body)) {
			t.Errorf("peer %s: accounts = %s, want the peer's body %s", got.Name, got.Accounts, want.body)
		}
	}
}

// The single-peer environment configuration names its peer by URL host, and
// the URL carries no credential, query or path.
func TestPeersUsageStatusNamesAnEnvironmentPeerByHost(t *testing.T) {
	peer := usagePeer(t, `[]`)
	parsed, err := url.Parse(peer.URL)
	if err != nil {
		t.Fatal(err)
	}
	server := testServer(t)
	server.RemotePool = NewRemotePool("http://user:secret@"+parsed.Host+"/t/tenant-key?x=1", "peer-token", "")

	view := readPeerView(t, server)
	if len(view.Peers) != 1 {
		t.Fatalf("peers = %+v", view.Peers)
	}
	got := view.Peers[0]
	if got.Name != parsed.Host || got.URL != "http://"+parsed.Host {
		t.Fatalf("peer = %s at %s, want %s at http://%s", got.Name, got.URL, parsed.Host, parsed.Host)
	}
}

// Garbage from a peer is reported as a failed read, never passed through as
// accounts: the CLI would render it as an empty box.
func TestPeersUsageStatusRefusesABodyThatIsNotAnArray(t *testing.T) {
	for name, body := range map[string]string{
		"an object":  `{"error":"maintenance"}`,
		"not json":   `<html>502</html>`,
		"a string":   `"accounts"`,
		"truncation": `[{"id":"box-codex"`,
	} {
		t.Run(name, func(t *testing.T) {
			server := peerViewServer(t, NewRemotePool(usagePeer(t, body).URL, "peer-token", "").WithName("box"))
			got := readPeerView(t, server).Peers[0]
			if got.Reachable || got.Error == "" {
				t.Fatalf("reachable=%v error=%q, want an unreachable peer with a reason", got.Reachable, got.Error)
			}
			if len(got.Accounts) != 0 && string(got.Accounts) != "null" {
				t.Fatalf("accounts = %s, want none", got.Accounts)
			}
		})
	}
}

// A peer that has never answered has no accounts at all (null), which is not
// the same thing as a box reporting none ([]).
func TestPeersUsageStatusSeparatesNoReadingFromAnEmptyBox(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	server := peerViewServer(t,
		NewRemotePool(deadURL, "peer-token", "").WithName("dead"),
		NewRemotePool(usagePeer(t, `null`).URL, "peer-token", "").WithName("empty"),
	)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, loopbackRequest(http.MethodGet, PeersUsageStatusPath, ""))
	var raw struct {
		Peers []map[string]json.RawMessage `json:"peers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil || len(raw.Peers) != 2 {
		t.Fatalf("body = %s (%v)", rec.Body.String(), err)
	}
	if got := string(raw.Peers[0]["accounts"]); got != "null" {
		t.Fatalf("a peer never read has accounts %s, want null", got)
	}
	if got := string(raw.Peers[0]["reachable"]); got != "false" || string(raw.Peers[0]["error"]) == `""` {
		t.Fatalf("a peer never read: reachable=%s error=%s", got, raw.Peers[0]["error"])
	}
	if got := string(raw.Peers[1]["accounts"]); got != "[]" {
		t.Fatalf("a peer answering null has accounts %s, want []", got)
	}
}

// No peers is an empty list, not null and not an error.
func TestPeersUsageStatusWithNoPeersIsAnEmptyList(t *testing.T) {
	rec := httptest.NewRecorder()
	testServer(t).Handler().ServeHTTP(rec, loopbackRequest(http.MethodGet, PeersUsageStatusPath, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if !sameJSON(t, rec.Body.Bytes(), []byte(`{"peers":[]}`)) {
		t.Fatalf("body = %s, want {\"peers\":[]}", rec.Body.String())
	}
}

// The route relays another daemon's roster, so it answers loopback only: not
// the tailnet with this daemon's admin token, and nobody on the public
// listener.
func TestPeersUsageStatusIsLoopbackOnlyAndPrivate(t *testing.T) {
	server := peerViewServer(t, NewRemotePool(usagePeer(t, `[]`).URL, "peer-token", "").WithName("box"))

	req := httptest.NewRequest(http.MethodGet, PeersUsageStatusPath, nil)
	req.RemoteAddr = "100.64.10.6:41000"
	req.Header.Set("Authorization", "Bearer admin-token")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a tailnet caller with the admin token got %d, want 403", rec.Code)
	}

	for _, remote := range []string{"127.0.0.1:5555", "203.0.113.9:5555"} {
		req := httptest.NewRequest(http.MethodGet, PeersUsageStatusPath, nil)
		req.RemoteAddr = remote
		req.Header.Set("Authorization", "Bearer admin-token")
		rec := httptest.NewRecorder()
		server.PublicHandler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("the public listener answered %s with %d, want 404", remote, rec.Code)
		}
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, loopbackRequest(http.MethodPost, PeersUsageStatusPath, ""))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d, want 405", rec.Code)
	}
}

// `sr status` stops waiting after 5 s. Its hang-up must not cut the peer read
// short, or the cache would record a failure the routing path then trusts.
func TestPeersUsageStatusReadOutlivesTheCaller(t *testing.T) {
	pool := NewRemotePool(usagePeer(t, `[{"id":"box-codex","provider":"codex"}]`).URL, "peer-token", "").WithName("box")
	server := peerViewServer(t, pool)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := loopbackRequest(http.MethodGet, PeersUsageStatusPath, "").WithContext(ctx)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	pool.admin.mu.Lock()
	entry := pool.admin.entries[poolPeerUsagePath]
	pool.admin.mu.Unlock()
	if entry.err != "" || len(entry.body) == 0 {
		t.Fatalf("a caller that hung up left the peer cache holding err=%q body=%q", entry.err, entry.body)
	}
}

// A failing peer is dialled once per remotePoolAdminTTL, not once per `sr`:
// in between, the route answers from the cache with the last good body and
// the failure, as the routing path does. Reading it through AdminGet instead
// re-dialled on every call, and a black-holed peer made each `sr` wait out
// the read timeout.
func TestPeersUsageStatusReadsAFailingPeerOncePerTTL(t *testing.T) {
	const body = `[{"id":"box-codex","provider":"codex"}]`
	var hits atomic.Int32
	var failing atomic.Bool
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if failing.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(peer.Close)
	pool := NewRemotePool(peer.URL, "peer-token", "").WithName("box")
	server := peerViewServer(t, pool)

	first := readPeerView(t, server).Peers[0]
	if !first.Reachable || !sameJSON(t, first.Accounts, []byte(body)) {
		t.Fatalf("first read = %+v, want the live body", first)
	}
	// The body ages past the TTL, and the peer goes down.
	pool.admin.mu.Lock()
	entry := pool.admin.entries[poolPeerUsagePath]
	entry.at = entry.at.Add(-2 * remotePoolAdminTTL)
	entry.tried = entry.at
	pool.admin.entries[poolPeerUsagePath] = entry
	pool.admin.mu.Unlock()
	failing.Store(true)
	measured := entry.at.UTC().Format(time.RFC3339)

	for i := 0; i < 3; i++ {
		got := readPeerView(t, server).Peers[0]
		if got.Reachable || got.Error != "peer returned 502 Bad Gateway" {
			t.Fatalf("read %d: reachable=%v error=%q, want the failure", i, got.Reachable, got.Error)
		}
		if !sameJSON(t, got.Accounts, []byte(body)) || got.AsOf != measured {
			t.Fatalf("read %d: accounts=%s as_of=%s, want the last good body as of %s", i, got.Accounts, got.AsOf, measured)
		}
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("the peer was read %d times, want 2: once live, once failing, then not until the TTL passes", got)
	}
}

// A failed read's error is relayed without the peer's URL: net/http puts the
// whole request URL in it, userinfo, tenant path and query included.
func TestPeersUsageStatusErrorCarriesNoPeerURL(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	host := strings.TrimPrefix(dead.URL, "http://")
	dead.Close()
	server := testServer(t)
	server.RemotePool = NewRemotePool("http://opsuser:pw-secret@"+host+"/t/srt_tenantsecret?q=querysecret", "peer-token", "")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, loopbackRequest(http.MethodGet, PeersUsageStatusPath, ""))
	for _, secret := range []string{"opsuser", "pw-secret", "srt_tenantsecret", "querysecret", "/_subrouter/usage-status"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("the reply carries %q: %s", secret, rec.Body.String())
		}
	}
	var view PeersUsageStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil || len(view.Peers) != 1 {
		t.Fatalf("body = %s (%v)", rec.Body.String(), err)
	}
	if got := view.Peers[0].Error; !strings.Contains(got, "refused") {
		t.Fatalf("error = %q, want the cause kept", got)
	}
}

func TestPeerViewErrorDropsTheRequestURL(t *testing.T) {
	for in, want := range map[string]string{
		`Get "http://u:***@10.0.0.1:1/t/srt_x/_subrouter/usage-status?q=1": dial tcp 10.0.0.1:1: connect: connection refused`: "dial tcp 10.0.0.1:1: connect: connection refused",
		`Get "https://203.0.113.7:31416/_subrouter/usage-status": context deadline exceeded`:                                  "context deadline exceeded",
		"peer returned 502 Bad Gateway":             "peer returned 502 Bad Gateway",
		"read tcp: reset on /t/srt_left/_subrouter": "read tcp: reset on /t/[redacted]/_subrouter",
		"": "",
	} {
		if got := peerViewError(in); got != want {
			t.Errorf("peerViewError(%q) = %q, want %q", in, got, want)
		}
	}
}
