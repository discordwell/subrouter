package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	tailscaleIPv4 = netip.MustParsePrefix("100.64.0.0/10")
	tailscaleIPv6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

const (
	remotePoolTailnetCooldown = time.Minute
	hostTailnetTTL            = time.Second
)

type peerRoute struct {
	route, reason string
	failure       dialFailure
}

type dialFailure string

const (
	failedConnect          dialFailure = "connect"
	failedHandshakeTimeout dialFailure = "handshake_timeout"
	failedPin              dialFailure = "pin_mismatch"
	failedHandshake        dialFailure = "handshake"
)

type tailnetCooldown struct {
	from    int64
	failure dialFailure
}

type peerTailnet struct {
	public    string
	addr      string
	onTailnet func() bool
	now       func() time.Time
	cooldown  atomic.Pointer[tailnetCooldown]
	probing   atomic.Bool
	route     atomic.Pointer[peerRoute]
}

type hostTailnet struct {
	interfaceAddrs func() ([]net.Addr, error)
	now            func() time.Time
	mu             sync.Mutex
	readAt         atomic.Int64
	on             atomic.Bool
}

var thisHost = &hostTailnet{interfaceAddrs: net.InterfaceAddrs, now: time.Now}

func (h *hostTailnet) onTailnet() bool {
	now := h.now().UnixNano()
	if h.fresh(now) {
		return h.on.Load()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.fresh(now) {
		if addrs, err := h.interfaceAddrs(); err == nil {
			h.on.Store(holdsTailscaleIPv6(addrs))
		}
		h.readAt.Store(now)
	}
	return h.on.Load()
}

func (h *hostTailnet) fresh(now int64) bool {
	at := h.readAt.Load()
	return at != 0 && within(now, at, hostTailnetTTL)
}

func within(now, at int64, window time.Duration) bool {
	age := time.Duration(now - at)
	return 0 <= age && age < window
}

// Only Tailscale assigns fd7a:115c:a1e0::/48. Other VPNs hand out
// 100.64.0.0/10 too, since it is carrier-grade NAT space.
func holdsTailscaleIPv6(addrs []net.Addr) bool {
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if addr, ok := netip.AddrFromSlice(ipnet.IP); ok && tailscaleIPv6.Contains(addr) {
			return true
		}
	}
	return false
}

func peerPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

func DialPeersOverTailnet(peers []*RemotePool, raw string) error {
	addrs, err := parsePeerTailnet(peers, raw)
	if err != nil {
		return err
	}
	for _, p := range peers {
		if addr, ok := addrs[p]; ok {
			p.dialTailnetFirst(addr, thisHost.onTailnet)
		}
	}
	return nil
}

func parsePeerTailnet(peers []*RemotePool, raw string) (map[*RemotePool]netip.AddrPort, error) {
	byName := map[string]*RemotePool{}
	var names []string
	for _, p := range peers {
		if p != nil {
			byName[strings.ToLower(p.name())] = p
			names = append(names, p.name())
		}
	}
	addrs := map[*RemotePool]netip.AddrPort{}
	owners := map[netip.AddrPort]string{}
	for i, entry := range strings.Split(raw, ",") {
		if strings.TrimSpace(entry) == "" {
			continue
		}
		n := i + 1
		name, ip, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("entry %d is not name=IP", n)
		}
		p := byName[strings.ToLower(strings.TrimSpace(name))]
		if p == nil {
			return nil, fmt.Errorf("entry %d names no configured peer; the peers are %q", n, names)
		}
		if _, dup := addrs[p]; dup {
			return nil, fmt.Errorf("entry %d (%s) names a peer an earlier entry named", n, p.name())
		}
		if _, ok := p.client.Transport.(*http.Transport); !ok {
			return nil, fmt.Errorf("entry %d (%s): the peer's HTTP client cannot be rerouted", n, p.name())
		}
		addr, err := netip.ParseAddr(strings.TrimSpace(ip))
		if err != nil || !(tailscaleIPv4.Contains(addr) || tailscaleIPv6.Contains(addr)) {
			return nil, fmt.Errorf("entry %d (%s) is not a Tailscale IP (100.64.0.0/10 or fd7a:115c:a1e0::/48)", n, p.name())
		}
		dial, err := netip.ParseAddrPort(net.JoinHostPort(addr.String(), peerPort(p.URL)))
		if err != nil {
			return nil, fmt.Errorf("entry %d (%s): the peer's URL has no port to dial", n, p.name())
		}
		if owner, dup := owners[dial]; dup {
			return nil, fmt.Errorf("entry %d (%s) gives the IP and port already given to %s", n, p.name(), owner)
		}
		owners[dial] = p.name()
		addrs[p] = dial
	}
	return addrs, nil
}

func (p *RemotePool) dialTailnetFirst(addr netip.AddrPort, onTailnet func() bool) {
	transport := p.client.Transport.(*http.Transport)
	p.tailnet = &peerTailnet{
		public:    net.JoinHostPort(p.URL.Hostname(), peerPort(p.URL)),
		addr:      addr.String(),
		onTailnet: onTailnet,
		now:       time.Now,
	}
	if p.URL.Scheme == "https" {
		transport.DialTLSContext = p.dialPeer(transport.DialContext, p.handshake(nil))
		p.wsDialer.NetDialTLSContext = p.dialPeer(p.wsDialer.NetDialContext, p.handshake([]string{"http/1.1"}))
		return
	}
	transport.DialContext = p.dialPeer(transport.DialContext, nil)
	p.wsDialer.NetDialContext = p.dialPeer(p.wsDialer.NetDialContext, nil)
}

type peerHandshake func(ctx context.Context, conn net.Conn, serverName string) (net.Conn, error)

func (p *RemotePool) handshake(nextProtos []string) peerHandshake {
	return func(ctx context.Context, conn net.Conn, serverName string) (net.Conn, error) {
		config := p.peerTLSConfig(nextProtos)
		config.ServerName = serverName
		tlsConn := tls.Client(conn, config)
		// net/http skips TLSHandshakeTimeout when DialTLSContext is set.
		ctx, cancel := context.WithTimeout(ctx, remotePoolDialTimeout)
		defer cancel()
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, peerHandshakeTimeout{}
			}
			return nil, err
		}
		return tlsConn, nil
	}
}

type peerHandshakeTimeout struct{}

func (peerHandshakeTimeout) Error() string   { return "peer TLS handshake timed out" }
func (peerHandshakeTimeout) Timeout() bool   { return true }
func (peerHandshakeTimeout) Temporary() bool { return true }

func handshakeFailure(err error) dialFailure {
	switch {
	case errors.Is(err, peerHandshakeTimeout{}):
		return failedHandshakeTimeout
	case errors.Is(err, errPeerPinMismatch):
		return failedPin
	default:
		return failedHandshake
	}
}

type peerConnect func(connectCtx context.Context, target string) (net.Conn, dialFailure, error)

func (p *RemotePool) dialPeer(dial dialFunc, handshake peerHandshake) dialFunc {
	t := p.tailnet
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		serverName, _, _ := net.SplitHostPort(addr)
		connect := func(connectCtx context.Context, target string) (net.Conn, dialFailure, error) {
			conn, err := dial(connectCtx, network, target)
			if err != nil {
				return nil, failedConnect, err
			}
			if target == t.addr {
				conn = tailnetConn{Conn: conn, onTailnet: t.onTailnet}
			}
			if handshake == nil {
				return conn, "", nil
			}
			tlsConn, err := handshake(ctx, conn, serverName)
			if err != nil {
				return nil, handshakeFailure(err), err
			}
			return tlsConn, "", nil
		}
		// The websocket dialer may be reaching a proxy from the environment.
		if addr != t.public {
			conn, _, err := connect(ctx, addr)
			return conn, err
		}
		if !t.onTailnet() {
			return p.dialPublic(ctx, connect, "off_tailnet", "")
		}
		cooldown := t.cooldown.Load()
		first, release := t.tailnetFirst(cooldown)
		if !first {
			return p.dialPublicFirst(ctx, connect, cooldown)
		}
		defer release()
		conn, failure := p.dialTailnet(ctx, connect)
		if failure == "" {
			return conn, nil
		}
		conn, err := p.dialPublic(ctx, connect, "tailnet_unreachable", failure)
		if err == nil {
			t.cooldown.Store(&tailnetCooldown{from: t.now().UnixNano(), failure: failure})
		}
		return conn, err
	}
}

func (t *peerTailnet) tailnetFirst(cooldown *tailnetCooldown) (first bool, release func()) {
	switch {
	case cooldown == nil:
		return true, func() {}
	case within(t.now().UnixNano(), cooldown.from, remotePoolTailnetCooldown):
		return false, nil
	case t.probing.CompareAndSwap(false, true):
		return true, func() { t.probing.Store(false) }
	default:
		return false, nil
	}
}

func (p *RemotePool) dialPublicFirst(ctx context.Context, connect peerConnect, cooldown *tailnetCooldown) (net.Conn, error) {
	connectCtx, cancel := context.WithTimeout(ctx, remotePoolDialTimeout)
	conn, err := p.dialPublic(connectCtx, connect, "tailnet_unreachable", cooldown.failure)
	cancel()
	if err == nil || ctx.Err() != nil {
		return conn, err
	}
	if conn, failure := p.dialTailnet(ctx, connect); failure == "" {
		return conn, nil
	}
	return nil, err
}

func (p *RemotePool) dialTailnet(ctx context.Context, connect peerConnect) (net.Conn, dialFailure) {
	connectCtx, cancel := context.WithTimeout(ctx, remotePoolDialTimeout)
	defer cancel()
	conn, failure, err := connect(connectCtx, p.tailnet.addr)
	if err != nil {
		return nil, failure
	}
	p.tailnet.cooldown.Store(nil)
	p.noteRoute(peerRoute{route: "tailnet"})
	return conn, ""
}

func (p *RemotePool) dialPublic(ctx context.Context, connect peerConnect, reason string, failure dialFailure) (net.Conn, error) {
	conn, _, err := connect(ctx, p.tailnet.public)
	if err == nil {
		p.noteRoute(peerRoute{route: "public", reason: reason, failure: failure})
	}
	return conn, err
}

func (p *RemotePool) noteRoute(route peerRoute) {
	if last := p.tailnet.route.Swap(&route); last != nil && *last == route {
		return
	}
	fields := []any{"remote", p.name(), "route", route.route}
	if route.reason != "" {
		fields = append(fields, "reason", route.reason)
	}
	if route.failure != "" {
		fields = append(fields, "failure", string(route.failure))
	}
	slog.Info("remote pool peer route changed", fields...)
}

// errLeftTailnet refuses a write on a connection dialed over the tailnet once
// this machine is off it. An idle one still looks open and would stall the
// next request until its header timeout. Refused before a byte is sent, the
// request goes out again on another connection.
var errLeftTailnet = errors.New("this machine is no longer on the tailnet")

type tailnetConn struct {
	net.Conn
	onTailnet func() bool
}

func (c tailnetConn) Write(b []byte) (int, error) {
	if !c.onTailnet() {
		return 0, errLeftTailnet
	}
	return c.Conn.Write(b)
}
