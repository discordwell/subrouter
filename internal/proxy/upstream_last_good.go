package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// Sticky DNS (PATCHES §59).
//
// On 2026-10-20 the laptop's resolver stopped answering for about two hours
// while the network itself still carried traffic: 4,178 turns failed with
// `dial tcp4: lookup api.anthropic.com: no such host` and 237 token refreshes
// with `lookup platform.claude.com: no such host`, while a dial to an IP
// literal (the peer at 203.0.113.201) kept working.
//
// So every upstream dial that reaches a host by name remembers the address it
// connected to. When a later lookup for that host fails, the dial goes to the
// remembered address instead, if it is younger than the maximum age. The
// request keeps its URL, so TLS still sends the real hostname as SNI and
// verifies the certificate against it: an address that no longer serves that
// host fails the handshake, it cannot reach an impostor.
//
// Only a failed LOOKUP falls back. A lookup that answers is always used, even
// with an address that then refuses: that is the network down, not DNS, and
// the hold and the outage spill (§59, outage_spill.go) take it. Only an
// address that is publicly routable is remembered, so a captive portal's or a
// fake-IP proxy's private answer is never dialed later in place of a real one.

// DefaultStickyDNSMaxAge is how old a remembered address may be and still be
// dialed when a lookup fails: a laptop closed over a long weekend still has
// one. Anthropic's and OpenAI's front doors keep their addresses for far
// longer, and TLS refuses one that moved.
const DefaultStickyDNSMaxAge = 7 * 24 * time.Hour

// dialFunc is net.Dialer.DialContext's shape, which http.Transport and the
// websocket dialer both take.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// lastGoodAddrs is the address each upstream host last connected on.
type lastGoodAddrs struct {
	// maxAge, in nanoseconds; zero or less turns the fallback off. Remembering
	// goes on regardless, so turning it back on needs no warm-up.
	maxAge atomic.Int64
	// now is the age clock; nil is time.Now. Tests move it.
	now func() time.Time
	// logger nil is slog.Default(), which serve configures.
	logger *slog.Logger

	mu    sync.Mutex
	hosts map[string]lastGoodAddr
}

type lastGoodAddr struct {
	ip netip.Addr
	// at is the wall clock of the last connect: Round(0) drops the monotonic
	// reading, which on darwin stops while the machine sleeps, so an address
	// remembered before a weekend with the lid shut reads its real age.
	at time.Time
}

// upstreamLastGood is the one memory every upstream dialer of this process
// shares: the outbound transports (NewOutboundTransport) and the Codex
// websocket dialer (outboundWebSocketDialer).
var upstreamLastGood = newLastGoodAddrs(DefaultStickyDNSMaxAge)

func newLastGoodAddrs(maxAge time.Duration) *lastGoodAddrs {
	m := &lastGoodAddrs{hosts: map[string]lastGoodAddr{}}
	m.maxAge.Store(int64(maxAge))
	return m
}

// SetStickyDNSMaxAge sets how old a remembered upstream address may be and
// still be dialed when a lookup fails; zero or less turns that off. serve
// calls it once at start, from SUBROUTER_STICKY_DNS_MAX_AGE.
func SetStickyDNSMaxAge(maxAge time.Duration) {
	upstreamLastGood.maxAge.Store(int64(maxAge))
}

func (m *lastGoodAddrs) clock() time.Time {
	if m.now != nil {
		return m.now().Round(0)
	}
	return time.Now().Round(0)
}

func (m *lastGoodAddrs) log() *slog.Logger {
	if m.logger != nil {
		return m.logger
	}
	return slog.Default()
}

// wrap returns dial with the memory around it. A dial to an IP literal, or to
// an address that does not parse, is dial unchanged.
func (m *lastGoodAddrs) wrap(dial dialFunc) dialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		host, port, splitErr := net.SplitHostPort(addr)
		if splitErr != nil || isIPLiteral(host) {
			return conn, err
		}
		if err == nil {
			m.remember(host, conn.RemoteAddr())
			return conn, nil
		}
		// A canceled lookup is the client leaving, not DNS failing.
		var dnsErr *net.DNSError
		if !errors.As(err, &dnsErr) || errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return nil, err
		}
		last, age, ok := m.lookup(host)
		if !ok {
			return nil, err
		}
		address := net.JoinHostPort(last.String(), port)
		conn, dialErr := dial(ctx, network, address)
		fields := []any{"host", host, "address", address, "age", age.Round(time.Second).String(),
			"lookup_error", dnsErr.Error()}
		if dialErr != nil {
			m.log().Warn("upstream lookup failed; dialing its last good address",
				append(fields, "outcome", "failed", "dial_error", dialErr.Error())...)
			return nil, dialErr
		}
		// Not remembered again: the age counts from the last lookup that
		// gave this address, so an outage cannot keep it alive past the
		// maximum age.
		m.log().Warn("upstream lookup failed; dialing its last good address",
			append(fields, "outcome", "connected")...)
		return conn, nil
	}
}

// remember records the address a dial to host connected on, when it is one
// worth dialing later.
func (m *lastGoodAddrs) remember(host string, remote net.Addr) {
	if remote == nil {
		return
	}
	addrPort, err := netip.ParseAddrPort(remote.String())
	if err != nil {
		return
	}
	ip := addrPort.Addr().Unmap()
	if !publiclyRoutable(ip) {
		return
	}
	m.note(host, ip, m.clock())
}

func (m *lastGoodAddrs) note(host string, ip netip.Addr, at time.Time) {
	m.mu.Lock()
	m.hosts[host] = lastGoodAddr{ip: ip, at: at.Round(0)}
	m.mu.Unlock()
}

// lookup is host's remembered address and its age, when the fallback is on
// and the address is young enough.
func (m *lastGoodAddrs) lookup(host string) (netip.Addr, time.Duration, bool) {
	maxAge := time.Duration(m.maxAge.Load())
	if maxAge <= 0 {
		return netip.Addr{}, 0, false
	}
	m.mu.Lock()
	last, ok := m.hosts[host]
	m.mu.Unlock()
	if !ok {
		return netip.Addr{}, 0, false
	}
	age := m.clock().Sub(last.at)
	if age > maxAge {
		return netip.Addr{}, 0, false
	}
	return last.ip, age, true
}

func isIPLiteral(host string) bool {
	_, err := netip.ParseAddr(host)
	return err == nil
}

// Address ranges a public upstream host never answers on, beyond the ones
// netip classifies: carrier-grade NAT (and Tailscale's addresses), and the
// benchmarking range that fake-IP proxies hand out for every name.
var (
	sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")
	benchmarkingRange  = netip.MustParsePrefix("198.18.0.0/15")
)

// publiclyRoutable is an address a public upstream may really be on: not
// loopback, link-local, private, CGNAT or the benchmarking range. On 10/20 the
// resolver answered 172.16.0.1 for api.anthropic.com at times; remembered, that
// would be dialed in place of the real address on the next failed lookup.
func publiclyRoutable(ip netip.Addr) bool {
	return ip.IsValid() && ip.IsGlobalUnicast() && !ip.IsPrivate() &&
		!sharedAddressSpace.Contains(ip) && !benchmarkingRange.Contains(ip)
}
