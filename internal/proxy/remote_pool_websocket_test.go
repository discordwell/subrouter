package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// Codex opens every model turn as a websocket upgrade to /v1/responses. The
// remote-pool path used to buffer-and-replay it like any other request, and the
// header copy drops Connection/Upgrade as hop-by-hop -- so the peer received a
// bare GET on a POST-only route, answered 405, and the client burned five
// reconnects before falling back to HTTPS. Invisible while a daemon held local
// Codex accounts; constant once every Codex account lived on the peer.
//
// These tests pin the upgrade surviving the hop, and pin the things that make a
// forwarded upgrade safe rather than merely working.

// peerWebSocketServer stands up a fake PEER that speaks websocket, records the
// upgrade request it saw, and echoes frames back.
func peerWebSocketServer(t *testing.T, seen *http.Request, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		if seen != nil {
			*seen = *r.Clone(r.Context())
		}
		if !websocket.IsWebSocketUpgrade(r) {
			// Exactly what the real box did: /v1/responses is POST-only, so a
			// stripped upgrade arrives as a bare GET and is refused.
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(messageType, append([]byte("peer:"), payload...)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// dialThroughSubrouter wraps a Server's handler in an httptest server and opens
// a websocket through it, the way a Codex client would.
func dialThroughSubrouter(t *testing.T, s Server, path string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	return dialThroughSubrouterWithHeader(t, s, path, nil)
}

// dialThroughSubrouterWithHeader is dialThroughSubrouter carrying the client's
// own headers (agent, window id), so the handler keys the socket to a session.
func dialThroughSubrouterWithHeader(t *testing.T, s Server, path string, header http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	front := httptest.NewServer(s.Handler())
	t.Cleanup(front.Close)
	return websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http")+path, header)
}

// The regression. Before the fix the peer saw a bare GET and answered 405.
func TestRemotePoolForwardsWebSocketUpgradeToPeer(t *testing.T) {
	var seen http.Request
	peer := peerWebSocketServer(t, &seen, nil)
	pool := NewRemotePool(peer.URL, "peer-token", "")
	if pool == nil {
		t.Fatal("NewRemotePool returned nil")
	}
	server := Server{RemotePool: pool, Sessions: newSessionStore(t)}

	conn, resp, err := dialThroughSubrouter(t, server, "/v1/responses")
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("websocket dial through subrouter failed (status %d): %v", status, err)
	}
	defer conn.Close()

	// Prove it is a live relay, not just a successful handshake.
	if err := conn.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(payload) != "peer:hello" {
		t.Fatalf("relayed payload = %q, want %q", payload, "peer:hello")
	}

	// The peer must have seen a real upgrade, not the stripped GET.
	if !websocket.IsWebSocketUpgrade(&seen) {
		t.Fatalf("peer did not receive a websocket upgrade; Connection=%q Upgrade=%q",
			seen.Header.Get("Connection"), seen.Header.Get("Upgrade"))
	}
}

// The loop guard and the peer's own credential have to ride along, or two
// daemons pointed at each other recurse and the peer rejects us outright.
func TestRemotePoolWebSocketCarriesHopGuardAndPeerToken(t *testing.T) {
	var seen http.Request
	peer := peerWebSocketServer(t, &seen, nil)
	pool := NewRemotePool(peer.URL, "peer-token", "")
	server := Server{RemotePool: pool, Sessions: newSessionStore(t)}

	conn, _, err := dialThroughSubrouter(t, server, "/v1/responses?session_id=ws-session")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if got := seen.Header.Get(remotePoolHopHeader); got != "1" {
		t.Errorf("%s = %q, want \"1\" (without it, A->B->A recurses)", remotePoolHopHeader, got)
	}
	if got := seen.Header.Get("Authorization"); got != "Bearer peer-token" {
		t.Errorf("Authorization = %q, want the PEER bearer", got)
	}
	if got := seen.Header.Get("X-Subrouter-Agent"); got == "" {
		t.Error("X-Subrouter-Agent missing; the peer defaults to codex and a Claude turn asks the wrong pool")
	}
	// Our own credential must never travel to a machine that picks its own.
	if got := seen.Header.Get("X-Api-Key"); got != "" {
		t.Errorf("X-Api-Key leaked to the peer: %q", got)
	}
}

// The websocket hop builds its headers separately from the HTTP one, so it
// gets its own check that no local account selector reaches the peer.
func TestRemotePoolWebSocketNeverForwardsAccountSelectors(t *testing.T) {
	selectors := []string{"X-Subrouter-Account", "X-Subrouter-Account-ID", "X-Subrouter-Preferred-Account-ID"}
	var seen http.Request
	peer := peerWebSocketServer(t, &seen, nil)
	server := Server{RemotePool: NewRemotePool(peer.URL, "peer-token", ""), Sessions: newSessionStore(t)}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !server.serveRemotePoolWebSocket(w, r, "codex", "ws-selectors", remotePoolHoldSet) {
			http.Error(w, "peer declined", http.StatusBadGateway)
		}
	}))
	t.Cleanup(front.Close)

	header := http.Header{}
	for _, name := range selectors {
		header.Set(name, "local-account")
	}
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http")+"/v1/responses", header)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial through the peer hop failed (status %d): %v", status, err)
	}
	defer conn.Close()

	if got := seen.Header.Get(remotePoolHopHeader); got != "1" {
		t.Fatalf("%s = %q; the peer did not see this hop's upgrade", remotePoolHopHeader, got)
	}
	for _, name := range selectors {
		if got := seen.Header.Get(name); got != "" {
			t.Errorf("%s = %q reached the peer; a local account selector must not cross the hop", name, got)
		}
	}
}

// An upgrade that already came from another daemon must be served locally, not
// bounced onward. remotePoolEligible enforces this; assert it holds for the
// websocket branch too, which bypasses the body path entirely.
func TestRemotePoolDoesNotReForwardAnInboundWebSocketHop(t *testing.T) {
	var hits atomic.Int32
	peer := peerWebSocketServer(t, nil, &hits)
	pool := NewRemotePool(peer.URL, "", "")
	server := Server{RemotePool: pool, Sessions: newSessionStore(t)}

	front := httptest.NewServer(server.Handler())
	t.Cleanup(front.Close)
	header := http.Header{}
	header.Set(remotePoolHopHeader, "1")
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http")+"/v1/responses", header)
	if conn != nil {
		defer conn.Close()
	}
	_ = err // may fail for want of a local account; the point is where it did NOT go

	if got := hits.Load(); got != 0 {
		t.Fatalf("peer was contacted %d times for an already-forwarded upgrade; want 0", got)
	}
}

// A peer that cannot take the upgrade must leave the response untouched so the
// caller can still serve locally. Writing to w here would strand the client.
func TestRemotePoolWebSocketDeclineLeavesResponseWritable(t *testing.T) {
	// A peer that refuses every upgrade.
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	t.Cleanup(peer.Close)
	pool := NewRemotePool(peer.URL, "", "")
	server := Server{RemotePool: pool, Sessions: newSessionStore(t)}

	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	recorder := httptest.NewRecorder()

	if served := server.serveRemotePoolFallthrough(recorder, req, "codex", "ws-session"); served {
		t.Fatal("declined peer reported as served; the caller would never try locally")
	}
	if recorder.Code != http.StatusOK || recorder.Body.Len() != 0 {
		t.Fatalf("decline wrote to the response (code=%d body=%q); it must stay untouched",
			recorder.Code, recorder.Body.String())
	}
}

// peerWebSocketDialer must not inherit outboundWebSocketDialer's tcp4 pin. That
// pin exists for chatgpt.com's edge; applied to a peer it would make an
// IPv6-only tailnet peer unreachable over websockets while plain HTTP kept
// working -- a failure that looks like the pool is broken.
func TestPeerWebSocketDialerIsNotPinnedToIPv4(t *testing.T) {
	pool := NewRemotePool("http://peer.example:31415", "", "")
	peerWebSocketDialer := pool.webSocketDialer
	if peerWebSocketDialer() == outboundWebSocketDialer() {
		t.Fatal("peer dialer is the chatgpt-tuned outbound dialer; tcp4 pin would break IPv6 peers")
	}
	if peerWebSocketDialer().HandshakeTimeout != websocketHandshakeTimeout {
		t.Error("peer dialer lost the handshake timeout; a silent peer would hang the upgrade forever")
	}
	// The ALPN pin is protocol-level and MUST be kept: an upgrade cannot run over h2.
	tlsConfig := peerWebSocketDialer().TLSClientConfig
	if tlsConfig == nil || len(tlsConfig.NextProtos) == 0 || tlsConfig.NextProtos[0] != "http/1.1" {
		t.Error("peer dialer must pin http/1.1 ALPN; websocket cannot upgrade over h2")
	}
}

// Guards the shape of the forwarded URL: scheme becomes ws, and the path is the
// CLIENT-facing one so the peer runs its own pathForUpstream.
func TestRemotePoolWebSocketForwardsClientFacingPath(t *testing.T) {
	var seen http.Request
	peer := peerWebSocketServer(t, &seen, nil)
	pool := NewRemotePool(peer.URL, "", "")
	server := Server{RemotePool: pool, Sessions: newSessionStore(t)}

	conn, _, err := dialThroughSubrouter(t, server, "/backend-api/codex/responses")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if seen.URL.Path != "/backend-api/codex/responses" {
		t.Fatalf("peer saw path %q, want the client-facing /backend-api/codex/responses", seen.URL.Path)
	}
	if _, err := url.Parse(peer.URL); err != nil {
		t.Fatalf("bad peer url: %v", err)
	}
}

// Websocket turns set and keep the peer hold (PATCHES §21). Codex opens every
// model turn as a websocket, and the forward never set a hold: the next turn
// re-ran local selection and a peer-served conversation could snap back to a
// local account mid-thread. A dial error also cleared the hold before looking
// at why, so one blip or a client cancel ended a warm session the HTTP path
// (§20) keeps.

const wsHoldSession = "s1"

func wsHoldKey() string { return session.ScopedSessionKey("codex", wsHoldSession) }

// accountStampingPeer is an echo peer whose 101 names the account that took
// the upgrade, as the local proxyWebSocket now does. onUpgrade runs before the
// peer answers.
func accountStampingPeer(t *testing.T, onUpgrade func(*http.Request)) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if onUpgrade != nil {
			onUpgrade(r)
		}
		conn, err := upgrader.Upgrade(w, r, http.Header{subrouterAccountHeader: []string{"peer-acct"}})
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(messageType, append([]byte("peer:"), payload...)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// poolFront serves every request through serveRemotePool with one hold mode
// and reports on done each time serveRemotePool has returned (for a socket:
// once the relay has ended).
func poolFront(t *testing.T, server Server, mode remotePoolHoldMode) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	done := make(chan struct{}, 8)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !server.serveRemotePool(w, r, "codex", wsHoldSession, mode) {
			http.Error(w, "declined", http.StatusBadGateway)
		}
		done <- struct{}{}
	}))
	t.Cleanup(front.Close)
	return front, done
}

func dialFront(t *testing.T, front *httptest.Server) *websocket.Conn {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http")+"/v1/responses", nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("websocket dial failed (status %d): %v", status, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func echoOnce(t *testing.T, conn *websocket.Conn, message string) {
	t.Helper()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(message)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(payload) != "peer:"+message {
		t.Fatalf("relayed payload = %q, want %q", payload, "peer:"+message)
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForSocketDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the forwarded socket never finished")
	}
}

// waitForNamedHold waits for the hold the 101 sets: it names the peer's
// account, which nothing set before the peer answered can know.
func waitForNamedHold(t *testing.T, pool *RemotePool) {
	t.Helper()
	waitUntil(t, "the peer's 101 to set a hold naming its account", func() bool {
		entry, ok := pool.delegations.lookup(wsHoldKey(), time.Now())
		return ok && entry.Account == "peer-acct"
	})
}

func TestAWebSocketTurnThePeerTakesSetsTheHold(t *testing.T) {
	peer := accountStampingPeer(t, nil)
	server := Server{RemotePool: NewRemotePool(peer.URL, "", "")}
	front, done := poolFront(t, server, remotePoolHoldSet)

	conn := dialFront(t, front)
	echoOnce(t, conn, "turn")
	waitForNamedHold(t, server.RemotePool)
	entry, _ := server.RemotePool.delegations.lookup(wsHoldKey(), time.Now())
	if min := time.Now().Add(remotePoolDelegationTTLFor("codex") - time.Minute); entry.Until.Before(min) {
		t.Fatalf("hold runs until %v; want a full idle bound (at least %v)", entry.Until, min)
	}

	_ = conn.Close()
	waitForSocketDone(t, done)
	if !server.RemotePool.delegations.active(wsHoldKey(), time.Now()) {
		t.Fatal("closing the socket ended the hold; the next turn would re-decide and move the conversation")
	}
}

// One socket carries many turns and can outlive the idle bound; a frame on it
// is the conversation being used, so it keeps the hold fresh.
func TestAnOpenWebSocketKeepsThePeerHoldFresh(t *testing.T) {
	peer := accountStampingPeer(t, nil)
	server := Server{RemotePool: NewRemotePool(peer.URL, "", "")}
	front, _ := poolFront(t, server, remotePoolHoldSet)

	conn := dialFront(t, front)
	waitForNamedHold(t, server.RemotePool)
	// Age the hold to a couple of seconds. Only a touch from the open socket
	// can bring it back to a full idle bound.
	server.RemotePool.delegations.set(wsHoldKey(), time.Now(), 2*time.Second, "peer-acct", server.RemotePool.name())
	sent := time.Now()
	echoOnce(t, conn, "turn")

	ttl := remotePoolDelegationTTLFor("codex")
	waitUntil(t, "a frame on the open socket to restart the hold's idle clock", func() bool {
		entry, ok := server.RemotePool.delegations.lookup(wsHoldKey(), time.Now())
		return ok && !entry.Until.Before(sent.Add(ttl-time.Second))
	})
}

// The idle clock starts when the conversation stops, which for a socket is
// when it closes -- the websocket analogue of delegationTouchingBody.
func TestClosingAWebSocketRestartsThePeerHoldIdleClock(t *testing.T) {
	peer := accountStampingPeer(t, nil)
	server := Server{RemotePool: NewRemotePool(peer.URL, "", "")}
	front, done := poolFront(t, server, remotePoolHoldSet)

	conn := dialFront(t, front)
	echoOnce(t, conn, "turn")
	waitForNamedHold(t, server.RemotePool)
	server.RemotePool.delegations.set(wsHoldKey(), time.Now(), 5*time.Second, "peer-acct", server.RemotePool.name())
	closed := time.Now()
	_ = conn.Close()
	waitForSocketDone(t, done)

	entry, ok := server.RemotePool.delegations.lookup(wsHoldKey(), time.Now())
	if !ok {
		t.Fatal("the hold is gone after the socket closed")
	}
	if min := closed.Add(remotePoolDelegationTTLFor("codex") - time.Second); entry.Until.Before(min) {
		t.Fatalf("hold runs until %v after close; want the idle clock restarted at close (at least %v)", entry.Until, min)
	}
}

// A one-turn forward (a transient local failure, not an eviction) serves the
// turn on the peer without moving the session there.
func TestAOneTurnWebSocketSetsNoPeerHold(t *testing.T) {
	var pool *RemotePool
	var heldAtArrival atomic.Bool
	peer := accountStampingPeer(t, func(*http.Request) {
		heldAtArrival.Store(pool.delegations.active(wsHoldKey(), time.Now()))
	})
	pool = NewRemotePool(peer.URL, "", "")
	server := Server{RemotePool: pool}
	front, done := poolFront(t, server, remotePoolHoldNone)

	conn := dialFront(t, front)
	echoOnce(t, conn, "turn")
	_ = conn.Close()
	waitForSocketDone(t, done)
	if heldAtArrival.Load() {
		t.Fatal("a one-turn forward claimed the session for the peer before it answered")
	}
	if pool.delegations.active(wsHoldKey(), time.Now()) {
		t.Fatal("a one-turn forward left the session held on the peer")
	}
}

func webSocketUpgradeRequest(ctx context.Context) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil).WithContext(ctx)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	return req
}

// heldCodexWebSocketServer is a server whose codex session wsHoldSession is
// already held on the peer at peerURL.
func heldCodexWebSocketServer(t *testing.T, peerURL string) Server {
	t.Helper()
	server := Server{RemotePool: NewRemotePool(peerURL, "", "")}
	server.RemotePool.delegations.set(wsHoldKey(), time.Now(), remotePoolDelegationTTLFor("codex"), "peer-acct", server.RemotePool.name())
	return server
}

func forwardHeldWebSocket(t *testing.T, server Server, ctx context.Context) {
	t.Helper()
	recorder := httptest.NewRecorder()
	if server.serveRemotePool(recorder, webSocketUpgradeRequest(ctx), "codex", wsHoldSession, remotePoolHoldSet) {
		t.Fatal("a failed dial reported the socket as served")
	}
	if recorder.Code != http.StatusOK || recorder.Body.Len() != 0 {
		t.Fatalf("a failed dial wrote to the response (code=%d)", recorder.Code)
	}
}

// Only a peer that cannot serve ends a held session: it declines (429/503) or
// it is down (the breaker opens). Anything else the peer answers is a blip for
// this one turn, exactly as on the HTTP path.
func TestAWebSocketDialErrorEndsTheHoldOnlyOnADecline(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		transient bool
		wantHeld  bool
	}{
		{name: "429 decline", status: http.StatusTooManyRequests, wantHeld: false},
		{name: "503 decline", status: http.StatusServiceUnavailable, wantHeld: false},
		{name: "transient 502", status: http.StatusBadGateway, transient: true, wantHeld: true},
		{name: "405 refusal", status: http.StatusMethodNotAllowed, wantHeld: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.transient {
					w.Header().Set(remotePoolTransientHeader, "1")
				}
				http.Error(w, "no", tc.status)
			}))
			t.Cleanup(peer.Close)
			server := heldCodexWebSocketServer(t, peer.URL)

			forwardHeldWebSocket(t, server, context.Background())
			if got := server.RemotePool.delegations.active(wsHoldKey(), time.Now()); got != tc.wantHeld {
				t.Fatalf("held after a %d dial answer = %v, want %v", tc.status, got, tc.wantHeld)
			}
			if !server.RemotePool.breaker.allow(time.Now()) {
				t.Fatal("a peer that answered counted against the breaker")
			}
		})
	}
}

func TestAWebSocketDialBlipKeepsTheHoldAndADownPeerIsFoundDown(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // connection refused from here on
	server := heldCodexWebSocketServer(t, deadURL)
	logs := &syncBuffer{}
	server.Logger = slog.New(slog.NewJSONHandler(logs, nil))

	forwardHeldWebSocket(t, server, context.Background())
	if !server.RemotePool.delegations.active(wsHoldKey(), time.Now()) {
		t.Fatal("one failed dial ended the hold; that is a blip, not an eviction")
	}
	localUpstreamAnswered(server.RemotePool) // this machine is online (PATCHES §54)
	forwardHeldWebSocket(t, server, context.Background())
	if server.RemotePool.breaker.allow(time.Now()) {
		t.Fatalf("calibration: the breaker did not open after %d failures", remotePoolFailuresToOpen)
	}
	if !server.RemotePool.breaker.foundDown() || !server.RemotePool.delegations.active(wsHoldKey(), time.Now()) {
		t.Fatal("want the peer found down with the hold kept: the session's next turn places it elsewhere (PATCHES §67)")
	}
	// The peer is down and nothing was kept, so nothing may say it was: the
	// after-deploy check in ops/PATCHES.md §54 reads this line (review round 2).
	if failed := e2eLogLines(logs, "remote pool websocket dial failed"); len(failed) != remotePoolFailuresToOpen || !e2eLineSays(failed[1], "peer_down", true) {
		t.Fatalf("dial-failed lines %v; want the opening dial's with peer_down=true", failed)
	}
	if kept := e2eLogLines(logs, holdsKeptMessage); len(kept) != 0 {
		t.Fatalf("holds-kept lines %v after a websocket peer_down; an attributed opening keeps nothing", kept)
	}
}

func TestACancelledWebSocketDialKeepsTheHold(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	server := heldCodexWebSocketServer(t, deadURL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	forwardHeldWebSocket(t, server, ctx)
	if !server.RemotePool.delegations.active(wsHoldKey(), time.Now()) {
		t.Fatal("a cancelled client ended the session's hold on the peer")
	}
	if !server.RemotePool.breaker.allow(time.Now()) {
		t.Fatal("a client cancel counted against the peer")
	}
}

// A peer's websocket 101 names the account that took it, as its HTTP responses
// do, so the forwarding side's hold can say where the session lives.
func TestALocalWebSocketUpgradeNamesItsAccount(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(upstream.Close)
	server := Server{
		Upstream: mustURL(t, upstream.URL),
		Accounts: []accounts.Account{{
			ID: "a@example.com", AuthMode: accounts.AuthModeOAuth, Token: "selected-token", AccountID: "acct-1",
		}},
		Sessions:     newSessionStore(t),
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}

	conn, response, err := dialThroughSubrouterWithHeader(t, server, "/v1/responses",
		http.Header{"X-Codex-Window-Id": []string{"window-1"}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if got := response.Header.Get(subrouterAccountHeader); got != "a@example.com" {
		t.Fatalf("101 %s = %q, want the account that took the upgrade", subrouterAccountHeader, got)
	}
}

// A Codex websocket turn of a chat held on a box that is not found down is
// answered 502 (PATCHES §67): Codex 0.170.0 retries a 502 upgrade for about
// 7 s and then falls back to HTTPS, whose turns the hold carries. The turn is
// never moved to the other box or served locally, where it would land with no
// refugee mark and no slot before its box is found down (R1, D1), or come
// back to the box on its next turn (D4).
func TestAStrandedCodexWebSocketTurnIsAnswered502UntilItsBoxIsFoundDown(t *testing.T) {
	for _, tc := range []struct {
		name        string
		breakerOpen bool
	}{{"the dial fails", false}, {"the breaker is open", true}} {
		t.Run(tc.name, func(t *testing.T) {
			dead := httptest.NewServer(http.NotFoundHandler())
			deadURL := dead.URL
			dead.Close()
			boxA := NewRemotePool(deadURL, "", "")
			boxB := newVerdictPeer(t, accounts.ProviderCodex, verdictPeerAccount{Score: codexVerdictScore("px", 0.9, 50*time.Hour)})
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			var local atomic.Int64
			server, _, logs := codexCrossPoolLaptop(t, nil, func(_ string, w http.ResponseWriter, r *http.Request) bool {
				local.Add(1)
				if conn, err := upgrader.Upgrade(w, r, nil); err == nil {
					defer conn.Close()
					_, _, _ = conn.ReadMessage()
				}
				return true
			})
			server.RemotePeers = []*RemotePool{boxA, boxB.pool()}
			server.Hold = strandedPolicy(nil)
			key := session.ScopedSessionKey("codex", "ws-stranded")
			boxA.delegations.setServed(key, time.Now(), remotePoolDelegationTTLFor("codex"), "pa", boxA.name(), true)
			if tc.breakerOpen {
				boxA.breaker.mu.Lock()
				boxA.breaker.openUntil = time.Now().Add(time.Hour)
				boxA.breaker.mu.Unlock()
			}
			front := newProxyTestServer(t, server.Handler())
			conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http")+"/backend-api/codex/responses",
				http.Header{"Session-Id": []string{"ws-stranded"}})
			if err == nil {
				conn.Close()
				t.Fatalf("the upgrade went through; want a 502 while box A is not found down. box B turns %+v, local %d; logs:\n%s",
					boxB.seen(), local.Load(), logs.String())
			}
			if resp == nil || resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("dial: %v (response %+v); want a 502", err, resp)
			}
			if turns := boxB.seen(); len(turns) != 0 || local.Load() != 0 {
				t.Fatalf("box B saw %+v and the local upstream %d; want the turn sent nowhere", turns, local.Load())
			}
			if hold, held := boxA.delegations.lookup(key, time.Now()); !held || hold.Account != "pa" {
				t.Fatalf("hold on box A = %+v (held %v); want it kept on pa", hold, held)
			}
		})
	}
}
