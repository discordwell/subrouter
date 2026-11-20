package proxy

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// Sticky DNS (PATCHES §59). The 10/20 outage was mostly the resolver: 4,178
// `lookup api.anthropic.com: no such host` while a dial to an IP literal still
// worked. These tests run a fake network under the real outbound transport:
// names resolve through a table until DNS is turned off, and an address the
// table routes connects to a local TLS listener while reporting the public
// address it stands for, as a real dial would.

type stickyNet struct {
	mu      sync.Mutex
	dnsDown bool
	names   map[string]string // host -> public IP
	routes  map[string]string // public ip:port -> local listener
	dialed  []string          // every address dialed, after its lookup
}

func (n *stickyNet) setDNS(up bool) {
	n.mu.Lock()
	n.dnsDown = !up
	n.mu.Unlock()
}

func (n *stickyNet) dialedAddrs() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.dialed...)
}

func (n *stickyNet) dial(ctx context.Context, _, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	if !isIPLiteral(host) {
		ip, ok := n.names[host]
		if n.dnsDown || !ok {
			n.mu.Unlock()
			return nil, dnsFailure(host)
		}
		addr = net.JoinHostPort(ip, port)
	}
	n.dialed = append(n.dialed, addr)
	target, ok := n.routes[addr]
	n.mu.Unlock()
	if !ok {
		return nil, refusedFailure()
	}
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", target)
	if err != nil {
		return nil, err
	}
	return remoteAddrConn{Conn: conn, remote: net.TCPAddrFromAddrPort(netip.MustParseAddrPort(addr))}, nil
}

// remoteAddrConn reports the public address the fake network routed.
type remoteAddrConn struct {
	net.Conn
	remote net.Addr
}

func (c remoteAddrConn) RemoteAddr() net.Addr { return c.remote }

const stickyTestIP = "160.79.104.10" // api.anthropic.com's address on 10/20

// stickyUpstream is a TLS server whose certificate is valid for example.com
// (httptest's), recording the SNI of each request.
func stickyUpstream(t *testing.T) (*httptest.Server, *atomic.Value) {
	t.Helper()
	var sni atomic.Value
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sni.Store(r.TLS.ServerName)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{"id":"msg_sticky"}`)
	}))
	upstream.StartTLS()
	t.Cleanup(upstream.Close)
	return upstream, &sni
}

// stickyTransport is serve's outbound transport over the fake network, with
// mem around the dial exactly as NewOutboundTransport puts the process memory,
// trusting the upstream's certificate and dialing fresh for every request.
func stickyTransport(t *testing.T, mem *lastGoodAddrs, fake *stickyNet, upstream *httptest.Server) *http.Transport {
	t.Helper()
	transport := NewOutboundTransport()
	transport.DialContext = mem.wrap(fake.dial)
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	transport.TLSClientConfig.RootCAs = roots
	transport.DisableKeepAlives = true
	t.Cleanup(transport.CloseIdleConnections)
	return transport
}

func stickyMemory(maxAge time.Duration) (*lastGoodAddrs, *syncBuffer) {
	logs := &syncBuffer{}
	mem := newLastGoodAddrs(maxAge)
	mem.logger = slog.New(slog.NewTextHandler(logs, nil))
	return mem, logs
}

func stickyGet(transport http.RoundTripper, rawURL string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp, nil
}

func TestStickyDNSDialsTheLastGoodAddressWhenTheLookupFails(t *testing.T) {
	upstream, sni := stickyUpstream(t)
	fake := &stickyNet{
		names:  map[string]string{"example.com": stickyTestIP},
		routes: map[string]string{stickyTestIP + ":443": upstream.Listener.Addr().String()},
	}
	mem, logs := stickyMemory(time.Hour)
	transport := stickyTransport(t, mem, fake, upstream)

	if resp, err := stickyGet(transport, "https://example.com/v1/messages"); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("with DNS up: %v %v", resp, err)
	}
	fake.setDNS(false)
	sni.Store("")
	resp, err := stickyGet(transport, "https://example.com/v1/messages")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("with DNS down and %s remembered: response %v, error %v; want 200", stickyTestIP, resp, err)
	}
	if got := sni.Load(); got != "example.com" {
		t.Fatalf("the upstream saw SNI %q, want the real hostname example.com", got)
	}
	if dialed := fake.dialedAddrs(); dialed[len(dialed)-1] != stickyTestIP+":443" {
		t.Fatalf("dialed %v, want the last dial to the remembered %s:443", dialed, stickyTestIP)
	}
	line := logs.String()
	for _, want := range []string{"upstream lookup failed; dialing its last good address", "host=example.com",
		"address=" + stickyTestIP + ":443", "outcome=connected", `lookup_error="lookup example.com: no such host"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("log %q lacks %q", line, want)
		}
	}
}

// Nothing remembered: the dial fails exactly as it did before §59, with the
// lookup's own error, which the hold classifies as dns.
func TestStickyDNSWithNothingRememberedFailsTheLookupAsBefore(t *testing.T) {
	upstream, _ := stickyUpstream(t)
	fake := &stickyNet{dnsDown: true, routes: map[string]string{stickyTestIP + ":443": upstream.Listener.Addr().String()}}
	mem, logs := stickyMemory(time.Hour)

	_, withMemory := stickyGet(stickyTransport(t, mem, fake, upstream), "https://example.com/v1/messages")
	bare := NewOutboundTransport()
	bare.DialContext = fake.dial
	_, without := stickyGet(bare, "https://example.com/v1/messages")
	if withMemory == nil || without == nil || withMemory.Error() != without.Error() {
		t.Fatalf("error with the memory %v, without %v: want the same failed lookup", withMemory, without)
	}
	if cause, neverSent, ok := transientNetworkCause(withMemory); cause != "dns" || !neverSent || !ok {
		t.Fatalf("transientNetworkCause = (%q, %v, %v), want a held dns failure", cause, neverSent, ok)
	}
	if strings.Contains(logs.String(), "last good address") || len(fake.dialedAddrs()) != 0 {
		t.Fatalf("dialed %v, log %q: nothing remembered must dial nothing", fake.dialedAddrs(), logs.String())
	}
}

// The address the fallback dials is the one remembered; the handshake still
// names the URL's host, so an address that does not serve that host fails
// TLS -- with the certificate error, not a connection.
func TestStickyDNSHandshakeVerifiesTheRealHostname(t *testing.T) {
	upstream, sni := stickyUpstream(t)
	fake := &stickyNet{
		// httptest's certificate is for example.com, not api.anthropic.com.
		names:  map[string]string{"api.anthropic.com": stickyTestIP},
		routes: map[string]string{stickyTestIP + ":443": upstream.Listener.Addr().String()},
	}
	mem, logs := stickyMemory(time.Hour)
	transport := stickyTransport(t, mem, fake, upstream)
	if _, err := stickyGet(transport, "https://api.anthropic.com/v1/messages"); err == nil {
		t.Fatal("a certificate for example.com was accepted for api.anthropic.com with DNS up")
	}
	fake.setDNS(false)
	_, err := stickyGet(transport, "https://api.anthropic.com/v1/messages")
	var hostErr x509.HostnameError
	if !errors.As(err, &hostErr) || hostErr.Host != "api.anthropic.com" {
		t.Fatalf("error %v (%T), want an x509.HostnameError for api.anthropic.com", err, err)
	}
	if !strings.Contains(logs.String(), "outcome=connected") {
		t.Fatalf("log %q: the fallback should have connected to the remembered address", logs.String())
	}
	if got := sni.Load(); got != nil && got != "" {
		t.Fatalf("a request was served (SNI %q) by a certificate for another host", got)
	}
}

func TestStickyDNSForgetsAnAddressPastItsMaxAge(t *testing.T) {
	upstream, _ := stickyUpstream(t)
	fake := &stickyNet{
		names:  map[string]string{"example.com": stickyTestIP},
		routes: map[string]string{stickyTestIP + ":443": upstream.Listener.Addr().String()},
	}
	mem, _ := stickyMemory(time.Hour)
	now := time.Date(2026, 10, 20, 21, 0, 0, 0, time.UTC)
	mem.now = func() time.Time { return now }
	transport := stickyTransport(t, mem, fake, upstream)
	if _, err := stickyGet(transport, "https://example.com/"); err != nil {
		t.Fatal(err)
	}
	fake.setDNS(false)

	now = now.Add(59 * time.Minute)
	if _, err := stickyGet(transport, "https://example.com/"); err != nil {
		t.Fatalf("59 minutes old under a 1-hour maximum: %v", err)
	}
	// The fallback at 59 minutes did not make the address younger: only a
	// lookup that gives it does.
	now = now.Add(2 * time.Minute)
	_, err := stickyGet(transport, "https://example.com/")
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		t.Fatalf("61 minutes old under a 1-hour maximum: error %v, want the failed lookup", err)
	}
	// A maximum age of 0 is off, however young the address.
	mem.maxAge.Store(0)
	now = now.Add(-2 * time.Minute)
	if _, err := stickyGet(transport, "https://example.com/"); !errors.As(err, &dnsErr) {
		t.Fatalf("a maximum age of 0 (off): error %v, want the failed lookup", err)
	}
}

// Only an address a public host can really be on is remembered: on 10/20 the
// resolver answered 172.16.0.1 for api.anthropic.com at times.
func TestStickyDNSRemembersOnlyPubliclyRoutableAddresses(t *testing.T) {
	for _, tc := range []struct {
		ip       string
		remember bool
	}{
		{stickyTestIP, true},
		{"104.18.32.47", true},
		{"2607:6bc0::10", true},
		{"172.16.0.1", false},  // the 10/20 resolver's private answer
		{"10.9.61.232", false}, // private
		{"192.168.1.1", false}, // private
		{"100.64.10.1", false},
		{"198.18.0.7", false}, // a fake-IP proxy's answer
		{"127.0.0.1", false},
		{"169.254.169.254", false},
		{"::ffff:172.16.0.1", false},
	} {
		mem := newLastGoodAddrs(time.Hour)
		mem.remember("api.anthropic.com", &net.TCPAddr{IP: net.ParseIP(tc.ip), Port: 443})
		if _, _, ok := mem.lookup("api.anthropic.com"); ok != tc.remember {
			t.Errorf("%s remembered = %v, want %v", tc.ip, ok, tc.remember)
		}
	}
}

// A lookup the client canceled is the client leaving: no fallback dial.
func TestStickyDNSLeavesACanceledLookupAlone(t *testing.T) {
	canceled := realCanceledLookup(t)
	var dials atomic.Int64
	mem, logs := stickyMemory(time.Hour)
	mem.note("api.anthropic.com", netip.MustParseAddr(stickyTestIP), time.Now())
	dial := mem.wrap(func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, canceled
	})
	if _, err := dial(context.Background(), "tcp", "api.anthropic.com:443"); err != canceled {
		t.Fatalf("error %v, want the canceled lookup unchanged", err)
	}
	if dials.Load() != 1 || logs.String() != "" {
		t.Fatalf("dials = %d, log %q: want one dial and no fallback", dials.Load(), logs.String())
	}
}

// NewOutboundTransport and the Codex websocket dialer both dial through the
// process memory, with the real resolver: a .invalid name never resolves.
func TestUpstreamDialersShareTheProcessMemory(t *testing.T) {
	const host = "sticky-dns-wiring.invalid"
	if _, err := net.DefaultResolver.LookupHost(context.Background(), host); err == nil {
		t.Skipf("this resolver answers %s", host)
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	// note, not remember: a loopback address is never remembered from a dial.
	upstreamLastGood.note(host, netip.MustParseAddr("127.0.0.1"), time.Now())

	for name, dial := range map[string]dialFunc{
		"outbound transport": NewOutboundTransport().DialContext,
		"websocket dialer":   outboundWebSocketDialer().NetDialContext,
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		conn, err := dial(ctx, "tcp", net.JoinHostPort(host, port))
		cancel()
		if err != nil {
			t.Fatalf("%s: %v, want the remembered 127.0.0.1", name, err)
		}
		conn.Close()
	}
}

// Through the daemon, as Claude Code sees it: the resolver fails, the turn is
// served on the remembered address at once, and nothing is held.
func TestStickyDNSServesATurnThroughTheDaemonWhileTheLookupFails(t *testing.T) {
	upstream, sni := stickyUpstream(t)
	fake := &stickyNet{
		names:  map[string]string{"example.com": stickyTestIP},
		routes: map[string]string{stickyTestIP + ":443": upstream.Listener.Addr().String()},
	}
	mem, _ := stickyMemory(time.Hour)
	logs := &syncBuffer{}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	account := accounts.Account{ID: "one@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-one"}
	server := Server{
		ClaudeUpstream:  &url.URL{Scheme: "https", Host: "example.com"},
		Accounts:        []accounts.Account{account},
		Sessions:        store,
		SchedulerRef:    selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{{AccountID: account.ID, Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1}})),
		MaxBodyBytes:    1 << 20,
		Transport:       stickyTransport(t, mem, fake, upstream),
		Logger:          slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		LocalProxyToken: holdTestProxyToken,
		Hold:            holdTestPolicy(5 * time.Second),
	}
	rig := &holdRig{logs: logs, daemon: httptest.NewServer(server.Handler())}
	t.Cleanup(rig.daemon.Close)

	if resp, _, err := rig.turn(t, context.Background(), nil); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("with DNS up: %v %v", resp, err)
	}
	fake.setDNS(false)
	resp, body, err := rig.turn(t, context.Background(), nil)
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(body, "msg_sticky") {
		t.Fatalf("with DNS down: response %v body %q error %v; want the upstream's 200", resp, body, err)
	}
	if sni.Load() != "example.com" {
		t.Fatalf("SNI %q, want example.com", sni.Load())
	}
	if n := rig.countLog("proxy hold started"); n != 0 {
		t.Fatalf("%d holds started; the remembered address should have served the turn at once", n)
	}
}
