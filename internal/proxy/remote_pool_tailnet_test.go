package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const tailnetTestToken = "tok-s3cr3t-do-not-log"

type countingListener struct {
	net.Listener
	accepted atomic.Int32
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return conn, err
}

func servePeer(t *testing.T, label string, cert tls.Certificate) (addr string, stop func()) {
	t.Helper()
	addr, stop, _ = servePeerCounting(t, label, cert)
	return addr, stop
}

func servePeerCounting(t *testing.T, label string, cert tls.Certificate) (addr string, stop func(), accepted *atomic.Int32) {
	t.Helper()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &countingListener{Listener: tcp}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			alpn := http.Header{"X-Alpn": {r.TLS.NegotiatedProtocol}}
			if websocket.IsWebSocketUpgrade(r) {
				conn, err := upgrader.Upgrade(w, r, alpn)
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.WriteMessage(websocket.TextMessage, []byte(label))
				return
			}
			w.Header().Set("X-Alpn", alpn.Get("X-Alpn"))
			body, _ := io.ReadAll(r.Body)
			_, _ = io.WriteString(w, label+"|"+r.Host+"|"+string(body))
		}),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
	}
	go func() { _ = server.ServeTLS(listener, "", "") }()
	// Close misses a listener ServeTLS has not taken yet, which would then
	// accept a dial and reset it.
	stop = func() { _ = server.Close(); _ = listener.Close() }
	t.Cleanup(stop)
	return listener.Addr().String(), stop, &listener.accepted
}

func boxIdentity(t *testing.T) BoxIdentity {
	t.Helper()
	identity, err := LoadOrCreateBoxIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func tailnetPeer(t *testing.T, onTailnet func() bool) (pool *RemotePool, stopTailnet func()) {
	t.Helper()
	identity := boxIdentity(t)
	public, _ := servePeer(t, "public", identity.Certificate)
	tailnet, stopTailnet := servePeer(t, "tailnet", identity.Certificate)
	pool = NewRemotePool("https://"+public, tailnetTestToken, "").WithFingerprint(identity.Fingerprint).WithName("box")
	pool.dialTailnetFirst(netip.MustParseAddrPort(tailnet), onTailnet)
	return pool, stopTailnet
}

func onTailnet(on bool) *atomic.Bool {
	var up atomic.Bool
	up.Store(on)
	return &up
}

type fakeHost struct {
	*hostTailnet
	on, unreadable atomic.Bool
	reads          atomic.Int64
	clock          atomic.Int64
}

func newFakeHost(on bool) *fakeHost {
	h := &fakeHost{}
	h.on.Store(on)
	h.clock.Store(time.Unix(1_800_000_000, 0).UnixNano())
	h.hostTailnet = &hostTailnet{
		interfaceAddrs: func() ([]net.Addr, error) {
			h.reads.Add(1)
			if h.unreadable.Load() {
				return nil, errors.New("route socket busy")
			}
			ip := net.ParseIP("203.0.113.20")
			if h.on.Load() {
				ip = net.ParseIP("fd7a:115c:a1e0::1")
			}
			return []net.Addr{&net.IPNet{IP: ip}}, nil
		},
		now: func() time.Time { return time.Unix(0, h.clock.Load()) },
	}
	return h
}

func (h *fakeHost) waitOutTheCache() { h.clock.Add(int64(hostTailnetTTL)) }

func answer(listener string, pool *RemotePool) string {
	return listener + "|" + pool.URL.Host + "|turn"
}

func ask(t *testing.T, pool *RemotePool) string {
	t.Helper()
	got, _ := askReused(t, pool)
	return got
}

func askReused(t *testing.T, pool *RemotePool) (got string, reused bool) {
	t.Helper()
	return post(t, pool, []byte("turn"))
}

func post(t *testing.T, pool *RemotePool, body []byte) (got string, reused bool) {
	t.Helper()
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace),
		http.MethodPost, pool.URL.String()+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := pool.client.Do(req)
	if err != nil {
		t.Fatalf("request to the peer failed: %v", err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(answer), reused
}

func TestPeerRequestGoesOverTheTailnetWhileThisMachineIsOnIt(t *testing.T) {
	pool, _ := tailnetPeer(t, onTailnet(true).Load)
	if got, want := ask(t, pool), answer("tailnet", pool); got != want {
		t.Fatalf("answer %q; want %q (the tailnet listener, the URL's Host, the body)", got, want)
	}
}

func TestPeerRequestGoesToThePublicAddressOffTheTailnet(t *testing.T) {
	pool, _ := tailnetPeer(t, onTailnet(false).Load)
	if got, want := ask(t, pool), answer("public", pool); got != want {
		t.Fatalf("answer %q; want %q", got, want)
	}
}

func TestPeerRequestFallsBackToThePublicAddressWhenTheTailnetDialFails(t *testing.T) {
	pool, stopTailnet := tailnetPeer(t, onTailnet(true).Load)
	stopTailnet()
	if got, want := ask(t, pool), answer("public", pool); got != want {
		t.Fatalf("answer %q; want %q", got, want)
	}
}

func TestPeerRequestAfterLeavingTheTailnetDoesNotGoOutOnATailnetConnection(t *testing.T) {
	host := newFakeHost(true)
	pool, _ := tailnetPeer(t, host.onTailnet)
	if got, want := ask(t, pool), answer("tailnet", pool); got != want {
		t.Fatalf("answer %q; want %q", got, want)
	}
	host.on.Store(false)
	host.waitOutTheCache()
	if got, want := ask(t, pool), answer("public", pool); got != want {
		t.Fatalf("after leaving the tailnet, answer %q; want %q on a public connection", got, want)
	}
}

func TestPeerUploadOverTheTailnetReadsTheInterfacesOnce(t *testing.T) {
	host := newFakeHost(true)
	pool, _ := tailnetPeer(t, host.onTailnet)
	body := bytes.Repeat([]byte("x"), 4<<20)
	if got, _ := post(t, pool, body); !strings.HasPrefix(got, "tailnet|") || len(got) < len(body) {
		t.Fatalf("a 4 MiB upload was answered with %.40q; want the tailnet listener's echo", got)
	}
	if got := host.reads.Load(); got != 1 {
		t.Fatalf("the interfaces were read %d times during one upload; want once", got)
	}
}

func TestLeavingTheTailnetMidTurnKeepsThePeerHolds(t *testing.T) {
	server, key := heldSessionServer(t, "https://203.0.113.10:31416")
	server.RemotePool.client.Transport = proxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errLeftTailnet
	})
	logs := &syncBuffer{}
	server.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	for i := 0; i < remotePoolFailuresToOpen; i++ {
		localUpstreamAnswered(server.RemotePool)
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
		server.forwardToPeer(server.RemotePool, req, "claude", "s1", "/v1/messages", "", []byte(`{}`), remotePoolHoldSet, "peer-acct")
	}
	if !server.RemotePool.delegations.active(key, time.Now()) {
		t.Fatal("leaving the tailnet mid-turn ended the session's hold on the peer")
	}
	failed := e2eLogLines(logs, "remote pool forward failed")
	if len(failed) != remotePoolFailuresToOpen || failed[1]["peer_down"] != false {
		t.Fatalf("forward-failed lines %v; want the second with peer_down=false", failed)
	}
	if kept := e2eLogLines(logs, holdsKeptMessage); len(kept) != 1 || kept[0]["why"] != keptNoRoute {
		t.Fatalf("holds-kept lines %v; want one with why=no_route", kept)
	}
}

func TestPeerTailnetConnectionOutlastsAnUnreadableInterfaceList(t *testing.T) {
	host := newFakeHost(true)
	pool, _ := tailnetPeer(t, host.onTailnet)
	if got, want := ask(t, pool), answer("tailnet", pool); got != want {
		t.Fatalf("answer %q; want %q", got, want)
	}
	host.unreadable.Store(true)
	host.waitOutTheCache()
	got, reused := askReused(t, pool)
	if want := answer("tailnet", pool); got != want || !reused {
		t.Fatalf("with the interfaces unreadable, answer %q (reused %v); want %q on the open tailnet connection", got, reused, want)
	}
}

func TestPeerPublicConnectionIsStillReusedAfterLeavingTheTailnet(t *testing.T) {
	up := onTailnet(true)
	pool, stopTailnet := tailnetPeer(t, up.Load)
	stopTailnet()
	if got, want := ask(t, pool), answer("public", pool); got != want {
		t.Fatalf("answer %q; want %q", got, want)
	}
	up.Store(false)
	got, reused := askReused(t, pool)
	if want := answer("public", pool); got != want || !reused {
		t.Fatalf("after leaving the tailnet, answer %q (reused %v); want %q on the open public connection", got, reused, want)
	}
}

func webSocketAnswer(t *testing.T, pool *RemotePool) string {
	t.Helper()
	conn, _, err := pool.webSocketDialer().Dial("wss://"+pool.URL.Host+"/v1/responses", nil)
	if err != nil {
		t.Fatalf("websocket dial failed: %v", err)
	}
	defer conn.Close()
	_, message, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	return string(message)
}

func TestPeerOverTheTailnetStillChecksThePin(t *testing.T) {
	identity := boxIdentity(t)
	public, stopPublic := servePeer(t, "public", identity.Certificate)
	stopPublic()
	impostor, _ := servePeer(t, "impostor", boxIdentity(t).Certificate)
	pool := NewRemotePool("https://"+public, "", "").WithFingerprint(identity.Fingerprint)
	pool.dialTailnetFirst(netip.MustParseAddrPort(impostor), onTailnet(true).Load)

	resp, err := pool.client.Get(pool.URL.String() + "/_subrouter/health")
	if err == nil {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("a listener presenting another certificate answered %q; want the pin to refuse it", body)
	}
}

func TestPeerWebSocketOverTheTailnetStillChecksThePin(t *testing.T) {
	identity := boxIdentity(t)
	public, stopPublic := servePeer(t, "public", identity.Certificate)
	stopPublic()
	impostor, _ := servePeer(t, "impostor", boxIdentity(t).Certificate)
	pool := NewRemotePool("https://"+public, "", "").WithFingerprint(identity.Fingerprint)
	pool.dialTailnetFirst(netip.MustParseAddrPort(impostor), onTailnet(true).Load)

	conn, _, err := pool.webSocketDialer().Dial("wss://"+pool.URL.Host+"/v1/responses", nil)
	if err == nil {
		_, message, _ := conn.ReadMessage()
		conn.Close()
		t.Fatalf("a listener presenting another certificate answered %q; want the pin to refuse it", message)
	}
}

func TestPeerTailnetListenerFailingThePinFallsBackToThePublicAddress(t *testing.T) {
	identity := boxIdentity(t)
	public, _ := servePeer(t, "public", identity.Certificate)
	impostor, _, impostorAccepted := servePeerCounting(t, "impostor", boxIdentity(t).Certificate)
	pool := NewRemotePool("https://"+public, tailnetTestToken, "").WithFingerprint(identity.Fingerprint).WithName("box")
	pool.dialTailnetFirst(netip.MustParseAddrPort(impostor), onTailnet(true).Load)

	if got, want := ask(t, pool), answer("public", pool); got != want {
		t.Fatalf("answer %q; want %q", got, want)
	}
	pool.client.CloseIdleConnections()
	if got, want := ask(t, pool), answer("public", pool); got != want {
		t.Fatalf("on a new connection, answer %q; want %q", got, want)
	}
	if got := impostorAccepted.Load(); got != 1 {
		t.Fatalf("the tailnet listener was dialed %d times; want once, then the cooldown", got)
	}
}

func TestPeerWebSocketTailnetListenerFailingThePinFallsBackToThePublicAddress(t *testing.T) {
	identity := boxIdentity(t)
	public, _ := servePeer(t, "public", identity.Certificate)
	impostor, _, impostorAccepted := servePeerCounting(t, "impostor", boxIdentity(t).Certificate)
	pool := NewRemotePool("https://"+public, tailnetTestToken, "").WithFingerprint(identity.Fingerprint).WithName("box")
	pool.dialTailnetFirst(netip.MustParseAddrPort(impostor), onTailnet(true).Load)

	for range 2 {
		if got := webSocketAnswer(t, pool); got != "public" {
			t.Fatalf("websocket answered by the %s listener; want public", got)
		}
	}
	if got := impostorAccepted.Load(); got != 1 {
		t.Fatalf("the tailnet listener was dialed %d times; want once, then the cooldown", got)
	}
}

func TestPeerPinSetAfterTheTailnetAddressStillApplies(t *testing.T) {
	identity := boxIdentity(t)
	public, _ := servePeer(t, "public", identity.Certificate)
	tailnet, _ := servePeer(t, "tailnet", identity.Certificate)
	pool := NewRemotePool("https://"+public, tailnetTestToken, "")
	pool.dialTailnetFirst(netip.MustParseAddrPort(tailnet), onTailnet(true).Load)
	pool.WithFingerprint(identity.Fingerprint)

	if got, want := ask(t, pool), answer("tailnet", pool); got != want {
		t.Fatalf("answer %q; want %q", got, want)
	}
	if got := webSocketAnswer(t, pool); got != "tailnet" {
		t.Fatalf("websocket answered by the %s listener; want tailnet", got)
	}
}

func TestPeerKeepsHTTP1AndTheWebSocketALPNOnBothRoutes(t *testing.T) {
	for _, tc := range []struct {
		on    bool
		label string
	}{{true, "tailnet"}, {false, "public"}} {
		t.Run(tc.label, func(t *testing.T) {
			pool, _ := tailnetPeer(t, onTailnet(tc.on).Load)
			resp, err := pool.client.Get(pool.URL.String() + "/_subrouter/health")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if !strings.HasPrefix(string(body), tc.label+"|") || resp.Proto != "HTTP/1.1" {
				t.Fatalf("answered %q over %s; want the %s listener over HTTP/1.1", body, resp.Proto, tc.label)
			}
			conn, upgrade, err := pool.webSocketDialer().Dial("wss://"+pool.URL.Host+"/v1/responses", nil)
			if err != nil {
				t.Fatal(err)
			}
			conn.Close()
			if got := upgrade.Header.Get("X-Alpn"); got != "http/1.1" {
				t.Fatalf("the websocket negotiated ALPN %q; want http/1.1", got)
			}
		})
	}
}

func TestPeerHandshakeThatNeverFinishesFailsAsAHandshakeTimeout(t *testing.T) {
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { silent.Close() })
	go func() {
		for {
			conn, err := silent.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { conn.Close() })
		}
	}()
	pool := NewRemotePool("https://"+silent.Addr().String(), "", "").WithFingerprint(boxIdentity(t).Fingerprint)
	pool.dialTailnetFirst(netip.MustParseAddrPort("100.64.0.7:"+pool.URL.Port()), onTailnet(false).Load)

	ctx, cancel := context.WithTimeout(context.Background(), 4*remotePoolDialTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, pool.URL.String()+"/_subrouter/health", nil)
	resp, err := pool.client.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("a peer that never answers the handshake answered the request")
	}
	if cause, neverSent, ok := transientNetworkCause(err); cause != "tls_handshake_timeout" || !neverSent || !ok {
		t.Fatalf("error %v reads as (%q, neverSent=%v, %v); want a TLS handshake timeout that never sent the request", err, cause, neverSent, ok)
	}
}

func TestPeerWebSocketThroughAProxyStillUsesTLSToThePeer(t *testing.T) {
	pool, _ := tailnetPeer(t, onTailnet(true).Load)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		upstream, err := net.Dial("tcp", r.Host)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		client, buffered, err := http.NewResponseController(w).Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		t.Cleanup(func() { client.Close(); upstream.Close() })
		_, _ = io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n")
		go func() { _, _ = io.Copy(upstream, buffered) }()
		_, _ = io.Copy(client, upstream)
	}))
	defer proxy.Close()
	pool.wsDialer.Proxy = http.ProxyURL(mustParseURL(t, proxy.URL))

	if got := webSocketAnswer(t, pool); got != "public" {
		t.Fatalf("websocket through the proxy answered by the %s listener; want public", got)
	}
}

func TestPeerWebSocketGoesOverTheTailnetWhileThisMachineIsOnIt(t *testing.T) {
	pool, _ := tailnetPeer(t, onTailnet(true).Load)
	conn, _, err := pool.webSocketDialer().Dial("wss://"+pool.URL.Host+"/v1/responses", nil)
	if err != nil {
		t.Fatalf("websocket dial failed: %v", err)
	}
	defer conn.Close()
	_, message, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if string(message) != "tailnet" {
		t.Fatalf("websocket answered by the %s listener; want tailnet", message)
	}
}

func TestPeerWebSocketThroughAProxyIsNotRerouted(t *testing.T) {
	pool, _ := tailnetPeer(t, onTailnet(true).Load)
	connects := make(chan string, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			connects <- r.Host
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	pool.wsDialer.Proxy = http.ProxyURL(mustParseURL(t, proxy.URL))

	if conn, _, err := pool.webSocketDialer().Dial("wss://"+pool.URL.Host+"/v1/responses", nil); err == nil {
		conn.Close()
		t.Fatal("the dial succeeded although the proxy refused the tunnel")
	}
	select {
	case target := <-connects:
		if target != pool.URL.Host {
			t.Fatalf("proxy was asked to tunnel to %s; want the peer's %s", target, pool.URL.Host)
		}
	default:
		t.Fatal("the dial never reached the proxy; it was rerouted to the tailnet address")
	}
}

func TestHostOnTailnetNeedsATailscaleIPv6Address(t *testing.T) {
	addrs := func(cidrs ...string) func() ([]net.Addr, error) {
		return func() ([]net.Addr, error) {
			out := make([]net.Addr, 0, len(cidrs))
			for _, cidr := range cidrs {
				ip, network, err := net.ParseCIDR(cidr)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, &net.IPNet{IP: ip, Mask: network.Mask})
			}
			return out, nil
		}
	}
	for _, tc := range []struct {
		name  string
		addrs func() ([]net.Addr, error)
		want  bool
	}{
		{"a Tailscale IPv6 address beside another one", addrs("203.0.113.20/24", "fd7a:115c:a1e0::9/128"), true},
		{"only a 100.64.0.0/10 address, as carrier-grade NAT or another VPN assigns", addrs("203.0.113.20/24", "100.64.0.7/32"), false},
		{"an fd7a address outside Tailscale's /48", addrs("fd7a:115c:a1e1::7/128"), false},
		{"no interface addresses readable", func() ([]net.Addr, error) { return nil, errors.New("no interfaces") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := &hostTailnet{interfaceAddrs: tc.addrs, now: time.Now}
			if got := host.onTailnet(); got != tc.want {
				t.Fatalf("on the tailnet = %v; want %v", got, tc.want)
			}
		})
	}
}

func TestHostOnTailnetIsReadAgainWhenTheClockStepsBack(t *testing.T) {
	host := newFakeHost(true)
	if !host.onTailnet() {
		t.Fatal("a host with a Tailscale address reads as off the tailnet")
	}
	host.on.Store(false)
	host.clock.Add(-int64(time.Hour))
	if host.onTailnet() {
		t.Fatal("after the clock stepped back an hour, the host still reads as on the tailnet it left")
	}
}

func fakeTailnetPeer(t *testing.T, up *atomic.Bool, refuse func(addr string) bool) (pool *RemotePool, dial dialFunc, dialed *[]string) {
	t.Helper()
	pool = NewRemotePool("https://203.0.113.10:31416", tailnetTestToken, "").WithName("box")
	pool.dialTailnetFirst(netip.MustParseAddrPort("100.64.0.7:31416"), up.Load)
	dialed = &[]string{}
	dial = pool.dialPeer(func(ctx context.Context, _, addr string) (net.Conn, error) {
		*dialed = append(*dialed, addr)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if refuse(addr) {
			return nil, errors.New("connection refused")
		}
		client, server := net.Pipe()
		t.Cleanup(func() { client.Close(); server.Close() })
		return client, nil
	}, nil)
	return pool, dial, dialed
}

func refuseTailnet(addr string) bool { return addr == "100.64.0.7:31416" }

func dialFakePeer(t *testing.T, dial dialFunc, dialed *[]string) []string {
	t.Helper()
	*dialed = (*dialed)[:0]
	if _, err := dial(context.Background(), "tcp", "203.0.113.10:31416"); err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	return append([]string(nil), *dialed...)
}

func TestPeerTailnetDialFailureSkipsTheTailnetForAMinute(t *testing.T) {
	pool, dial, dialed := fakeTailnetPeer(t, onTailnet(true), refuseTailnet)
	clock := time.Unix(1_800_000_000, 0)
	pool.tailnet.now = func() time.Time { return clock }

	both := []string{"100.64.0.7:31416", "203.0.113.10:31416"}
	publicOnly := []string{"203.0.113.10:31416"}
	for _, step := range []struct {
		after time.Duration
		want  []string
	}{
		{0, both},
		{time.Second, publicOnly},
		{58 * time.Second, publicOnly},
		{time.Second, both},
		{time.Second, publicOnly},
	} {
		clock = clock.Add(step.after)
		if got := dialFakePeer(t, dial, dialed); !slices.Equal(got, step.want) {
			t.Fatalf("after %s more, dialed %v; want %v", step.after, got, step.want)
		}
	}
}

func TestPeerTailnetCooldownEndsWhenTheClockStepsBack(t *testing.T) {
	pool, dial, dialed := fakeTailnetPeer(t, onTailnet(true), refuseTailnet)
	clock := time.Unix(1_800_000_000, 0)
	pool.tailnet.now = func() time.Time { return clock }
	dialFakePeer(t, dial, dialed)

	clock = clock.Add(-time.Hour)
	if got, want := dialFakePeer(t, dial, dialed), []string{"100.64.0.7:31416", "203.0.113.10:31416"}; !slices.Equal(got, want) {
		t.Fatalf("after the clock stepped back an hour, dialed %v; want %v, the tailnet tried again", got, want)
	}
}

func TestPeerTailnetDialCancelledByTheCallerStartsNoCooldown(t *testing.T) {
	_, dial, dialed := fakeTailnetPeer(t, onTailnet(true), func(string) bool { return false })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := dial(ctx, "tcp", "203.0.113.10:31416"); err == nil {
		t.Fatal("a cancelled dial succeeded")
	}
	if got, want := dialFakePeer(t, dial, dialed), []string{"100.64.0.7:31416"}; !slices.Equal(got, want) {
		t.Fatalf("the next dial went to %v; want %v, the tailnet tried again", got, want)
	}
}

func TestPeerTailnetDialFailureStartsNoCooldownWhileThePublicDialFailsToo(t *testing.T) {
	var tailnetDown atomic.Bool
	tailnetDown.Store(true)
	pool, dial, dialed := fakeTailnetPeer(t, onTailnet(true), func(addr string) bool {
		return addr != "100.64.0.7:31416" || tailnetDown.Load()
	})
	clock := time.Unix(1_800_000_000, 0)
	pool.tailnet.now = func() time.Time { return clock }

	if _, err := dial(context.Background(), "tcp", "203.0.113.10:31416"); err == nil {
		t.Fatal("the dial succeeded although both addresses refused it")
	}
	tailnetDown.Store(false)
	clock = clock.Add(time.Second)
	if got, want := dialFakePeer(t, dial, dialed), []string{"100.64.0.7:31416"}; !slices.Equal(got, want) {
		t.Fatalf("with the public address refusing too, the next dial went to %v; want %v", got, want)
	}
}

func TestPeerDialDuringTheCooldownTriesTheTailnetWhenThePublicDialFails(t *testing.T) {
	var publicDown atomic.Bool
	pool, dial, dialed := fakeTailnetPeer(t, onTailnet(true), func(addr string) bool {
		if addr == "100.64.0.7:31416" {
			return !publicDown.Load()
		}
		return publicDown.Load()
	})
	clock := time.Unix(1_800_000_000, 0)
	pool.tailnet.now = func() time.Time { return clock }

	both := []string{"100.64.0.7:31416", "203.0.113.10:31416"}
	if got := dialFakePeer(t, dial, dialed); !slices.Equal(got, both) {
		t.Fatalf("dialed %v; want %v", got, both)
	}
	publicDown.Store(true)
	clock = clock.Add(time.Second)
	if got, want := dialFakePeer(t, dial, dialed), []string{"203.0.113.10:31416", "100.64.0.7:31416"}; !slices.Equal(got, want) {
		t.Fatalf("during the cooldown, with the public address refusing, dialed %v; want %v", got, want)
	}
	clock = clock.Add(time.Second)
	if got, want := dialFakePeer(t, dial, dialed), []string{"100.64.0.7:31416"}; !slices.Equal(got, want) {
		t.Fatalf("after the tailnet answered, the next dial went to %v; want %v, the cooldown over", got, want)
	}
}

func TestPeerDialDuringTheCooldownReachesTheTailnetWhileThePublicDialHangs(t *testing.T) {
	pool := NewRemotePool("https://203.0.113.10:31416", tailnetTestToken, "").WithName("box")
	pool.dialTailnetFirst(netip.MustParseAddrPort("100.64.0.7:31416"), onTailnet(true).Load)
	var tailnetDown atomic.Bool
	tailnetDown.Store(true)
	dial := pool.dialPeer(func(ctx context.Context, _, addr string) (net.Conn, error) {
		if addr == "203.0.113.10:31416" && !tailnetDown.Load() {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		if addr == "100.64.0.7:31416" && tailnetDown.Load() {
			return nil, errors.New("connection refused")
		}
		client, server := net.Pipe()
		t.Cleanup(func() { client.Close(); server.Close() })
		return client, nil
	}, nil)
	if _, err := dial(context.Background(), "tcp", "203.0.113.10:31416"); err != nil {
		t.Fatal(err)
	}
	tailnetDown.Store(false)

	ctx, cancel := context.WithTimeout(context.Background(), 4*remotePoolDialTimeout)
	defer cancel()
	start := time.Now()
	conn, err := dial(ctx, "tcp", "203.0.113.10:31416")
	if err != nil {
		t.Fatalf("with the public address silent during the cooldown, the dial failed after %v: %v", time.Since(start), err)
	}
	if _, ok := conn.(tailnetConn); !ok {
		t.Fatalf("the dial returned %T; want a tailnet connection", conn)
	}
}

func slowConnectAndTLS(dial dialFunc, target string, delay time.Duration) dialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != target {
			return dial(ctx, network, addr)
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &slowFirstRead{Conn: conn, delay: delay}, nil
	}
}

type slowFirstRead struct {
	net.Conn
	delay time.Duration
	once  sync.Once
}

func (c *slowFirstRead) Read(b []byte) (int, error) {
	c.once.Do(func() { time.Sleep(c.delay) })
	return c.Conn.Read(b)
}

func TestPeerSlowPublicAddressStillAnswersDuringTheCooldown(t *testing.T) {
	overHalfTheDialBound := remotePoolDialTimeout/2 + 100*time.Millisecond
	for _, tc := range []struct {
		name string
		ask  func(*testing.T, *RemotePool) string
	}{
		{"http", func(t *testing.T, pool *RemotePool) string {
			listener, _, _ := strings.Cut(ask(t, pool), "|")
			return listener
		}},
		{"websocket", webSocketAnswer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			identity := boxIdentity(t)
			public, _ := servePeer(t, "public", identity.Certificate)
			tailnet, stopTailnet := servePeer(t, "tailnet", identity.Certificate)
			stopTailnet()
			pool := NewRemotePool("https://"+public, tailnetTestToken, "").WithFingerprint(identity.Fingerprint).WithName("box")
			transport := pool.client.Transport.(*http.Transport)
			transport.DialContext = slowConnectAndTLS(transport.DialContext, public, overHalfTheDialBound)
			pool.wsDialer.NetDialContext = slowConnectAndTLS(pool.wsDialer.NetDialContext, public, overHalfTheDialBound)
			pool.dialTailnetFirst(netip.MustParseAddrPort(tailnet), onTailnet(true).Load)

			for _, dial := range []string{"the dial that starts the cooldown", "a dial during the cooldown"} {
				pool.client.CloseIdleConnections()
				if got := tc.ask(t, pool); got != "public" {
					t.Fatalf("%s, with %v to connect and %v more for TLS, was answered by the %s listener; want public", dial, overHalfTheDialBound, overHalfTheDialBound, got)
				}
			}
		})
	}
}

func TestPeerTailnetSuccessEndsTheCooldownAFailureRacingItStarted(t *testing.T) {
	pool := NewRemotePool("https://203.0.113.10:31416", tailnetTestToken, "").WithName("box")
	pool.dialTailnetFirst(netip.MustParseAddrPort("100.64.0.7:31416"), onTailnet(true).Load)
	slowDialStarted, release := make(chan struct{}), make(chan struct{})
	var tailnetDials atomic.Int32
	var mu sync.Mutex
	var dialed []string
	dial := pool.dialPeer(func(_ context.Context, _, addr string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, addr)
		mu.Unlock()
		if addr == "100.64.0.7:31416" {
			switch tailnetDials.Add(1) {
			case 1:
				close(slowDialStarted)
				<-release
			case 2:
				return nil, errors.New("connection refused")
			}
		}
		client, server := net.Pipe()
		t.Cleanup(func() { client.Close(); server.Close() })
		return client, nil
	}, nil)

	slow := make(chan error)
	go func() {
		_, err := dial(context.Background(), "tcp", "203.0.113.10:31416")
		slow <- err
	}()
	<-slowDialStarted
	if _, err := dial(context.Background(), "tcp", "203.0.113.10:31416"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-slow; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	dialed = dialed[:0]
	mu.Unlock()
	if _, err := dial(context.Background(), "tcp", "203.0.113.10:31416"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"100.64.0.7:31416"}; !slices.Equal(dialed, want) {
		t.Fatalf("after the tailnet dial that finished last succeeded, the next dial went to %v; want %v", dialed, want)
	}
}

func TestPeerOneDialTriesTheTailnetWhenTheCooldownEnds(t *testing.T) {
	pool := NewRemotePool("https://203.0.113.10:31416", tailnetTestToken, "").WithName("box")
	pool.dialTailnetFirst(netip.MustParseAddrPort("100.64.0.7:31416"), onTailnet(true).Load)
	var clock atomic.Int64
	clock.Store(time.Unix(1_800_000_000, 0).UnixNano())
	pool.tailnet.now = func() time.Time { return time.Unix(0, clock.Load()) }
	var tailnetDials atomic.Int32
	release := make(chan struct{})
	dial := pool.dialPeer(func(ctx context.Context, _, addr string) (net.Conn, error) {
		if addr == "100.64.0.7:31416" {
			if tailnetDials.Add(1) > 1 {
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			return nil, errors.New("connection refused")
		}
		client, server := net.Pipe()
		t.Cleanup(func() { client.Close(); server.Close() })
		return client, nil
	}, nil)
	if _, err := dial(context.Background(), "tcp", "203.0.113.10:31416"); err != nil {
		t.Fatal(err)
	}
	clock.Add(int64(remotePoolTailnetCooldown))

	const dials = 6
	results := make(chan error, dials)
	for range dials {
		go func() {
			_, err := dial(context.Background(), "tcp", "203.0.113.10:31416")
			results <- err
		}()
	}
	for range dials - 1 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if got := tailnetDials.Load() - 1; got != 1 {
		t.Fatalf("%d of %d dials tried the tailnet as the cooldown ended; want 1", got, dials)
	}
	close(release)
	if err := <-results; err != nil {
		t.Fatal(err)
	}
}

type routeLine struct {
	Remote, Route, Reason, Failure string
}

func logRouteLines(t *testing.T) *syncBuffer {
	t.Helper()
	logs := &syncBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

func routeLines(logs *syncBuffer, remote string) []routeLine {
	var got []routeLine
	for _, raw := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var line struct {
			Msg string `json:"msg"`
			routeLine
		}
		if err := json.Unmarshal([]byte(raw), &line); err == nil && line.Msg == "remote pool peer route changed" && line.Remote == remote {
			got = append(got, line.routeLine)
		}
	}
	return got
}

func TestPeerRouteIsLoggedOncePerChangeOfRouteReasonAndFailure(t *testing.T) {
	logs := logRouteLines(t)
	up := onTailnet(true)
	pool := NewRemotePool("https://203.0.113.10:31416", tailnetTestToken, "").WithName("box")
	pool.dialTailnetFirst(netip.MustParseAddrPort("100.64.0.7:31416"), up.Load)
	clock := time.Unix(1_800_000_000, 0)
	pool.tailnet.now = func() time.Time { return clock }
	var tailnetFails string
	dialed := &[]string{}
	dial := pool.dialPeer(func(_ context.Context, _, addr string) (net.Conn, error) {
		*dialed = append(*dialed, addr)
		if refuseTailnet(addr) && tailnetFails == "connect" {
			return nil, errors.New("connection refused")
		}
		client, server := net.Pipe()
		t.Cleanup(func() { client.Close(); server.Close() })
		return client, nil
	}, func(_ context.Context, conn net.Conn, _ string) (net.Conn, error) {
		if _, overTailnet := conn.(tailnetConn); overTailnet && tailnetFails == "handshake_timeout" {
			return nil, peerHandshakeTimeout{}
		}
		return conn, nil
	})

	for _, step := range []struct {
		on           bool
		tailnetFails string
		after        time.Duration
	}{
		{on: true},
		{on: true},
		{on: false},
		{on: false},
		{on: true, tailnetFails: "connect"},
		{on: true, tailnetFails: "connect"},
		{on: true, after: remotePoolTailnetCooldown},
		{on: true, tailnetFails: "handshake_timeout"},
		{on: true, tailnetFails: "connect", after: remotePoolTailnetCooldown},
		{on: true, tailnetFails: "connect", after: remotePoolTailnetCooldown},
		{on: false},
		{on: true},
		{on: true, after: remotePoolTailnetCooldown},
	} {
		up.Store(step.on)
		tailnetFails = step.tailnetFails
		clock = clock.Add(step.after)
		dialFakePeer(t, dial, dialed)
	}

	want := []routeLine{
		{"box", "tailnet", "", ""},
		{"box", "public", "off_tailnet", ""},
		{"box", "public", "tailnet_unreachable", "connect"},
		{"box", "tailnet", "", ""},
		{"box", "public", "tailnet_unreachable", "handshake_timeout"},
		{"box", "public", "tailnet_unreachable", "connect"},
		{"box", "public", "off_tailnet", ""},
		{"box", "public", "tailnet_unreachable", "connect"},
		{"box", "tailnet", "", ""},
	}
	if got := routeLines(logs, "box"); !slices.Equal(got, want) {
		t.Fatalf("route lines %+v; want %+v", got, want)
	}
	if strings.Contains(logs.String(), tailnetTestToken) {
		t.Fatal("a route line carries the peer token")
	}
}

func TestPeerRouteLineSaysWhyTheTailnetDialFailed(t *testing.T) {
	logs := logRouteLines(t)
	identity := boxIdentity(t)
	public, _ := servePeer(t, "public", identity.Certificate)
	stopped, stop := servePeer(t, "stopped", identity.Certificate)
	stop()
	impostor, _ := servePeer(t, "impostor", boxIdentity(t).Certificate)
	plain := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(plain.Close)

	for _, tc := range []struct {
		failure, tailnet string
	}{
		{"connect", stopped},
		{"pin_mismatch", impostor},
		{"handshake_timeout", silentListener(t).Addr().String()},
		{"handshake", plain.Listener.Addr().String()},
	} {
		t.Run(tc.failure, func(t *testing.T) {
			t.Parallel()
			pool := NewRemotePool("https://"+public, tailnetTestToken, "").WithFingerprint(identity.Fingerprint).WithName(tc.failure)
			pool.dialTailnetFirst(netip.MustParseAddrPort(tc.tailnet), onTailnet(true).Load)
			for range 2 {
				pool.client.CloseIdleConnections()
				if got, want := ask(t, pool), answer("public", pool); got != want {
					t.Fatalf("answer %q; want %q", got, want)
				}
			}
			want := []routeLine{{tc.failure, "public", "tailnet_unreachable", tc.failure}}
			if got := routeLines(logs, tc.failure); !slices.Equal(got, want) {
				t.Fatalf("route lines over two dials %+v; want %+v", got, want)
			}
			if strings.Contains(logs.String(), tailnetTestToken) {
				t.Fatal("a route line carries the peer token")
			}
		})
	}
}

func TestPeerTailnetEnv(t *testing.T) {
	peers := func() []*RemotePool {
		return []*RemotePool{
			NewRemotePool("https://203.0.113.10:31416", tailnetTestToken, "").WithName("alpha"),
			NewRemotePool("https://203.0.113.11", "", "").WithName("Beta"),
			NewRemotePool("https://203.0.113.12:31416", "", "").WithName("gamma"),
		}
	}
	tailnetOf := func(p *RemotePool) string {
		if p.tailnet == nil {
			return ""
		}
		return p.tailnet.addr
	}

	t.Run("each named peer dials its tailnet IP on its URL's port", func(t *testing.T) {
		set := peers()
		if err := DialPeersOverTailnet(set, " alpha = 100.64.0.7 ,beta=fd7a:115c:a1e0::9,"); err != nil {
			t.Fatal(err)
		}
		if got := tailnetOf(set[0]); got != "100.64.0.7:31416" {
			t.Fatalf("alpha dials %q over the tailnet; want 100.64.0.7:31416", got)
		}
		if got := tailnetOf(set[1]); got != "[fd7a:115c:a1e0::9]:443" {
			t.Fatalf("beta dials %q over the tailnet; want [fd7a:115c:a1e0::9]:443", got)
		}
	})
	t.Run("the single-peer configuration is named by its URL's host", func(t *testing.T) {
		single := NewRemotePool("http://203.0.113.10:31416", "", "")
		if err := DialPeersOverTailnet([]*RemotePool{single}, "203.0.113.10:31416=100.64.0.9"); err != nil {
			t.Fatal(err)
		}
		if got := tailnetOf(single); got != "100.64.0.9:31416" {
			t.Fatalf("the peer dials %q over the tailnet; want 100.64.0.9:31416", got)
		}
	})
	t.Run("two daemons on one tailnet host share its IP on their own ports", func(t *testing.T) {
		set := peers()
		if err := DialPeersOverTailnet(set, "alpha=100.64.0.7,beta=100.64.0.7"); err != nil {
			t.Fatal(err)
		}
		if got := tailnetOf(set[0]) + " " + tailnetOf(set[1]); got != "100.64.0.7:31416 100.64.0.7:443" {
			t.Fatalf("alpha and beta dial %q over the tailnet; want 100.64.0.7:31416 100.64.0.7:443", got)
		}
	})
	t.Run("a peer the value leaves out is unchanged", func(t *testing.T) {
		set := peers()
		if err := DialPeersOverTailnet(set, "alpha=100.64.0.7"); err != nil {
			t.Fatal(err)
		}
		if got := tailnetOf(set[1]); got != "" {
			t.Fatalf("beta dials %q over the tailnet; it was never named", got)
		}
	})
	t.Run("unset changes no peer", func(t *testing.T) {
		set := peers()
		if err := DialPeersOverTailnet(set, ""); err != nil {
			t.Fatal(err)
		}
		for _, p := range set {
			if got := tailnetOf(p); got != "" {
				t.Fatalf("%s dials %q over the tailnet with the variable unset", p.name(), got)
			}
		}
	})

	t.Run("fails open on a peer whose client cannot be rerouted", func(t *testing.T) {
		set := peers()
		set[1].client.Transport = proxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("not dialed")
		})
		err := DialPeersOverTailnet(set, "alpha=100.64.0.7,beta=100.64.0.8")
		if err == nil || !strings.Contains(err.Error(), "entry 2") {
			t.Fatalf("error %v; want one naming entry 2", err)
		}
		for _, p := range set {
			if got := tailnetOf(p); got != "" {
				t.Fatalf("%s dials %q over the tailnet after an invalid value; want every peer on its URL", p.name(), got)
			}
		}
	})

	for _, tc := range []struct {
		name, raw, entry string
	}{
		{"not name=IP", "alpha=100.64.0.7,beta", "entry 2"},
		{"no IP", "alpha=", "entry 1"},
		{"no name", "=100.64.0.7", "entry 1"},
		{"a host name", "alpha=box.example", "entry 1"},
		{"an IP with a port", "alpha=100.64.0.7:31416", "entry 1"},
		{"an IP with a zone", "alpha=fd7a:115c:a1e0::9%utun4", "entry 1"},
		{"an address outside 100.64.0.0/10", "alpha=100.128.0.1", "entry 1"},
		{"an address outside fd7a:115c:a1e0::/48", "alpha=fd7a:115c:a1e1::7", "entry 1"},
		{"an address in neither range", "alpha=203.0.113.7", "entry 1"},
		{"an IPv4-mapped IPv6 address", "alpha=::ffff:100.64.0.7", "entry 1"},
		{"a token where the IP goes", "alpha=" + tailnetTestToken, "entry 1"},
		{"a name matching no peer", "alpha=100.64.0.7," + tailnetTestToken + "=100.64.0.8", "entry 2"},
		{"one IP and port on two peers", "alpha=100.64.0.7,gamma=100.64.0.7", "entry 2"},
		{"one peer named twice", "alpha=100.64.0.7,ALPHA=100.64.0.8", "entry 2"},
		{"a valid entry before an invalid one", "alpha=100.64.0.7,beta=203.0.113.7", "entry 2"},
	} {
		t.Run("fails open on "+tc.name, func(t *testing.T) {
			set := peers()
			err := DialPeersOverTailnet(set, tc.raw)
			if err == nil {
				t.Fatal("no error; want one naming the bad entry")
			}
			if !strings.Contains(err.Error(), tc.entry) {
				t.Fatalf("error %q does not name %s", err, tc.entry)
			}
			if strings.Contains(err.Error(), tailnetTestToken) {
				t.Fatalf("error %q repeats a token", err)
			}
			for _, p := range set {
				if got := tailnetOf(p); got != "" {
					t.Fatalf("%s dials %q over the tailnet after an invalid value; want every peer on its URL", p.name(), got)
				}
			}
			if set[0].URL.Host != "203.0.113.10:31416" || set[0].Token != tailnetTestToken || set[0].name() != "alpha" {
				t.Fatalf("alpha changed: %s %q", set[0].URL.Host, set[0].name())
			}
		})
	}
}
