package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// claudeSocketEnv names an OPT-IN Unix socket that serves Claude Code the same
// private handler the TCP listener serves. PATCHES section 29.
//
// Why a socket at all: Claude Code turns on its first-party-only behaviour (tool
// search / deferred tools, and the rest of what it gates on the API host) only
// when `new URL(ANTHROPIC_BASE_URL).host === "api.anthropic.com"`. A client
// pointed at http://127.0.0.1:31415 never qualifies. With
// ANTHROPIC_BASE_URL=http://api.anthropic.com plus ANTHROPIC_UNIX_SOCKET=<this
// path> in the client's PROCESS environment, it qualifies and every API request
// is written, as plain HTTP, into this socket instead of onto the network.
//
// Why an environment variable and not a `serve` flag: the LaunchAgent plist is
// shared by every binary that ever runs under it, including the backup a failed
// deploy rolls back to. An old binary given an unknown flag exits in flag.Parse
// and launchd's KeepAlive respawns it forever -- every local session down. An
// unknown environment variable is ignored by an old binary.
const claudeSocketEnv = "SUBROUTER_CLAUDE_SOCKET"

// Health vocabulary for the socket, reported as `claude_socket` in
// /_subrouter/health. Fixed words, never the path or an error string: health is
// also served on --public-addr, and a syscall error carries the path (and so the
// user name).
//
// waiting and displaced are the keeper's (socketKeeper, PATCHES section 77.1).
// waiting: this process does not hold the socket's lock and keeps asking for
// it. displaced: it holds the lock, and a socket it does not own answers at the
// path. unavailable is a path refused at start, which is never tried again, or
// a path the keeper could not bind again, which it keeps trying.
//
// Fixed since 2026-11-21: the deploy scripts and a process that stands in on
// the socket path match these words, and the supervisor-status fields
// claude_socket_waits_for_lock and claude_socket_rebinds, by name.
const (
	claudeSocketServing     = "serving"
	claudeSocketUnavailable = "unavailable"
	claudeSocketStopped     = "stopped"
	claudeSocketSupervised  = "owned_by_supervisor"
	claudeSocketWaiting     = "waiting"
	claudeSocketDisplaced   = "displaced"
)

// claudeSocket owns the opt-in listener and its health word.
//
// Its failures are SOFT, unlike --local-data-socket's: an open or serve failure
// is logged and reported, and TCP keeps serving. The local-data socket is fatal
// because the credential-bearing CLI cannot work without it; this socket is an
// optional second door onto a handler that TCP still serves, and a fatal policy
// would turn a bad path or a stale lock into a launchd respawn loop that takes
// every TCP client down with it.
type claudeSocket struct {
	path       string
	supervised bool
	state      atomic.Value // string
	// open binds the path under its lock; the keeper calls it until it works.
	open     func(string) (leasedListener, error)
	keeper   *socketKeeper
	listener net.Listener
	served   chan struct{}
	// guard keeps every 401 from the socket's clients, which Claude Code
	// would answer by sending the human's own stored login, and counts the
	// clients that already send one (proxy.ClaudeSocketGuard). startGuarded
	// puts it in front of the server, so serve never serves the socket
	// without it.
	guard *proxy.ClaudeSocketGuard
}

// newClaudeSocket reads the opt-in: the --unix-socket flag when given, else the
// environment. nil means not configured, and then nothing about serve changes.
// The flag exists for hand runs and tests; a LaunchAgent must use the variable
// (see claudeSocketEnv).
//
// A supervised worker (one handed SUBROUTER_LISTEN_FD) never opens it. The
// supervisor passes its whole environment to every worker generation, and the
// socket's lifetime lock would refuse the replacement worker while the retiring
// one drains -- and the retiring one's Close would then unlink the path with
// nobody left to re-bind it.
func newClaudeSocket(flagValue string, getenv func(string) string) *claudeSocket {
	path := strings.TrimSpace(flagValue)
	if path == "" {
		path = strings.TrimSpace(getenv(claudeSocketEnv))
	}
	if path == "" {
		return nil
	}
	// The guard logs through slog.Default, which serve has set by the time a
	// request arrives.
	socket := &claudeSocket{
		path:       filepath.Clean(path),
		supervised: strings.TrimSpace(getenv(inheritedListenerFDEnv)) != "",
		open:       openLeasedClaudeSocket,
		guard:      proxy.NewClaudeSocketGuard(nil),
	}
	socket.state.Store(claudeSocketUnavailable)
	if socket.supervised {
		socket.state.Store(claudeSocketSupervised)
	}
	return socket
}

// Status is the health word. It is only handed to the proxy when the socket is
// configured, so an unconfigured daemon's health body is unchanged.
func (c *claudeSocket) Status() string {
	return c.state.Load().(string)
}

func (c *claudeSocket) statusFunc() func() string {
	if c == nil {
		return nil
	}
	return c.Status
}

// socketGuard is the socket's proxy.ClaudeSocketGuard, for the proxy's health.
// nil when the socket is not configured, and then serve is unchanged.
func (c *claudeSocket) socketGuard() *proxy.ClaudeSocketGuard {
	if c == nil {
		return nil
	}
	return c.guard
}

func guardServer(server *http.Server, guard *proxy.ClaudeSocketGuard, supervised bool) {
	server.Handler = guard.Handler(server.Handler)
	inner := server.ConnContext
	server.ConnContext = func(ctx context.Context, conn net.Conn) context.Context {
		if inner != nil {
			ctx = inner(ctx, conn)
		}
		if claudeSocketConn(conn, supervised) {
			ctx = proxy.ClaudeSocketConnContext(ctx)
		}
		return ctx
	}
}

func claudeSocketConn(conn net.Conn, supervised bool) bool {
	if !supervised {
		_, ok := conn.(loopbackTCPConn)
		return ok
	}
	peer, ok := conn.RemoteAddr().(*net.TCPAddr)
	socket := claudeSocketAddr()
	return ok && peer.Port == socket.Port && peer.IP.Equal(socket.IP)
}

func (c *claudeSocket) startGuarded(server *http.Server, localDataSocket string, logger *slog.Logger) {
	if c == nil {
		return
	}
	guardServer(server, c.guard, c.supervised)
	c.start(server.Serve, localDataSocket, logger)
}

func (c *claudeSocket) start(serve func(net.Listener) error, localDataSocket string, logger *slog.Logger) {
	if c == nil {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	if c.supervised {
		logger.Info("leaving "+claudeSocketEnv+" to the supervisor", "path", c.path)
		return
	}
	if local := filepath.Clean(strings.TrimSpace(localDataSocket)); local != "." && local == c.path {
		logger.Error("claude socket not served: it names the --local-data-socket path", "path", c.path)
		return
	}
	// A path the opener refuses before it takes the lock is a fault in the
	// configuration. It is not kept, and the word stays unavailable.
	if _, _, err := servedSocketParent(c.path); err != nil {
		logger.Error("claude socket not served; TCP is unaffected", "path", c.path, "error", err)
		return
	}
	c.keeper = keepSocket(c.path, c.open, logger, func(word string) { c.state.Store(word) })
	listener := loopbackTCPListener{Listener: c.keeper, addr: c.keeper.Addr()}
	c.listener = listener
	c.served = make(chan struct{})
	go func() {
		defer close(c.served)
		err := serve(listener)
		// Whatever ended serve, nobody accepts now: give up the path and
		// the lock, and only then say so, after the keeper's last word.
		_ = listener.Close()
		c.state.Store(claudeSocketStopped)
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			// Never fatal (see claudeSocket). Health now says "stopped", which is
			// what deploy-laptop.sh and anyone polling health will see.
			logger.Error("claude socket stopped serving; TCP is unaffected", "path", c.path, "error", err)
		}
	}()
}

func (c *claudeSocket) close() {
	if c != nil && c.listener != nil {
		_ = c.listener.Close()
		<-c.served
	}
}

// rebinds counts the times the keeper bound the path after the first.
func (c *claudeSocket) rebinds() int {
	if c.keeper == nil {
		return 0
	}
	return c.keeper.rebinds()
}

// openPrivateServedSocket opens a socket serve serves beside TCP: the Claude
// socket and the drain socket (drain_socket.go). It is
// openPrivateLocalDataListener's contract -- absolute path, canonical
// current-user-owned parent that is not group/world writable, a lifetime flock
// lease on <path>.lock, stale removal only under that lease, unlink-on-close of
// only our own inode -- with one difference: the socket is bound under a
// temporary name, chmodded 0600 and only then renamed into place, so the served
// path never holds a socket of any other mode. bind(2) creates it at
// 0777&^umask (0755 here), and a crash between bind and chmod used to leave
// exactly the kind of stale socket removeStaleSocket refuses, forever: every
// later start would fail.
//
// The returned listener's Addr is the temporary bind name; callers wrap it to
// report the served path. `what` names the socket in errors.
//
// The local-data socket keeps its own opener unchanged: it is the supervisor's
// fatal path on the box, and this change has no reason to touch it.
func openPrivateServedSocket(socket, what string) (net.Listener, error) {
	parent, parentInfo, err := servedSocketParent(socket)
	if err != nil {
		return nil, err
	}
	lease, err := acquireLocalDataSocketLease(socket)
	if err != nil {
		return nil, err
	}
	releaseLease := true
	defer func() {
		if releaseLease {
			_ = lease.Close()
		}
	}()
	if !lease.parentMatches(parentInfo) {
		return nil, errors.New(what + " parent changed during lease acquisition")
	}
	// The lease says no other subrouter owns the path, not that nothing
	// listens there: a foreign process (or a hand-run `nc -lU`) could. A stale
	// socket is removed only when a dial finds nobody behind it, so a live
	// listener is never silently stolen from.
	if claudeSocketAnswers(socket) {
		return nil, errors.New("refuse to replace a socket another process is listening on")
	}
	if err := lease.removeStaleSocket(); err != nil {
		return nil, err
	}
	listener, err := lease.listenPrivate(socket, 0o600, func() bool {
		current, statErr := os.Stat(parent)
		return statErr == nil && lease.parentMatches(current)
	})
	if err != nil {
		return nil, err
	}
	owned, err := wrapOwnedLocalDataListener(listener, socket, lease)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	releaseLease = false
	return owned, nil
}

// servedSocketParent is what openPrivateServedSocket checks before it touches
// anything: the path's shape and its folder. A path it refuses can be served
// by no process, whoever holds the lock.
func servedSocketParent(socket string) (string, os.FileInfo, error) {
	if !filepath.IsAbs(socket) || socket == string(filepath.Separator) {
		return "", nil, errors.New("path must be absolute")
	}
	// First, before the lease, the liveness dial or the stale-socket removal
	// touch the path (see checkPrivateSocketPathLength).
	if err := checkPrivateSocketPathLength(socket); err != nil {
		return "", nil, err
	}
	parent, err := validatePrivateLocalServingStorePath(filepath.Dir(socket), true)
	if err != nil || parent != filepath.Dir(socket) {
		return "", nil, errors.New("parent must be canonical, current-user-owned, and not group/world writable")
	}
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return "", nil, err
	}
	return parent, parentInfo, nil
}

// claudeSocketAnswers reports whether something accepts connections at socket.
// A missing path and a dead socket (ECONNREFUSED) both answer false.
func claudeSocketAnswers(socket string) bool {
	conn, err := net.DialTimeout("unix", socket, 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Every "is this caller local" decision in the proxy is
// net.ParseIP(host).IsLoopback() on r.RemoteAddr: the proxy-token loopback
// exemption (SUBROUTER_ALLOW_LOOPBACK_PROXY), the loopback-only admin verbs,
// machine-sharing policy and the remote-pool allow list. A Unix peer's
// RemoteAddr is "" on darwin and "@" on Linux, so without this every one of
// them would treat a same-user socket client as REMOTE -- a token-enforcing
// daemon would 401 Claude Code's placeholder bearer, and machine policy would
// count the socket as a second machine. 127.0.0.1 is the truth, and a narrower
// one than TCP: the socket is mode 0600, so only this user can reach it at all.
type loopbackTCPListener struct {
	net.Listener
	addr net.Addr
}

func (l loopbackTCPListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return loopbackTCPConn{Conn: conn}, nil
}

// Addr is the served path, not the temporary name the socket was bound under.
func (l loopbackTCPListener) Addr() net.Addr {
	return l.addr
}

type loopbackTCPConn struct {
	net.Conn
}

func (loopbackTCPConn) RemoteAddr() net.Addr {
	return claudeSocketAddr()
}

func (loopbackTCPConn) LocalAddr() net.Addr {
	return claudeSocketAddr()
}

func claudeSocketAddr() *net.TCPAddr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}
}

// CloseWrite keeps net/http's graceful half-close on the wrapped *UnixConn,
// which embedding the net.Conn interface would otherwise hide.
func (c loopbackTCPConn) CloseWrite() error {
	if closer, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return fmt.Errorf("claude socket connection cannot half-close")
}
