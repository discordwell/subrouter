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
	"runtime"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/storepath"
)

// The drain socket (PATCHES section 38.1): drain, resume and drain-status, and
// nothing else, on a mode-0600 Unix socket in the daemon's state directory. It
// lets an operator logged in to the machine drain the daemon with no token --
// `sudo subrouter drain` on the box -- and lets the box's deploy script
// drain the build it is replacing and watch its shutdown.
//
// Authority is the filesystem: the socket is 0600 and owned by the daemon's
// user, inside that user's private state directory (/var/lib/subrouter on the
// box, 0750; ~/.subrouter, 0700, on a laptop). Only root and that user can
// connect, and neither gains anything by it. Root can already stop the
// service and read the admin token from /etc/default/subrouter (root:root,
// 0600). The service user is the daemon's own uid: it can already signal the
// daemon and read its environment, the admin token included, from
// /proc/<pid>/environ; and an SSH login reaches that uid only through sudo.
// Where the kernel reports the peer's uid (SO_PEERCRED on Linux, getpeereid's
// LOCAL_PEERCRED on darwin) the listener also refuses any uid but root's and
// its own, so a socket whose mode was loosened by hand still does not open to
// other users.
//
// Least privilege:
//   - It serves DrainSocketHandler, never the private mux: no accounts, no
//     usage, no proxy, no quiesce. A process that can reach it gains a drain,
//     which a restart or a resume undoes.
//   - It is never bound on TCP. The box's tailnet admin surface is unchanged:
//     drain there still takes the admin token or an admitted tailnet identity
//     (PATCHES section 38).
//   - Resume lifts a drain and never a quiesce.
//
// On by default, at <state dir>/drain.sock: the box needs nothing configured,
// and a unit file, a plist or a rolled-back binary that knows nothing of it is
// unaffected (an old binary ignores the variable and serves no socket).
// SUBROUTER_DRAIN_SOCKET=<absolute path> moves it; =off turns it off.
//
// Like the Claude socket it is soft: a socket that cannot open is logged and
// serve goes on without it. Unlike the Claude socket it outlives the
// listeners. It closes when serve returns, so during a SIGTERM drain it still
// answers drain-status (shutting_down: true, and the turns in flight).
const (
	drainSocketEnv  = "SUBROUTER_DRAIN_SOCKET"
	drainSocketName = "drain.sock"
)

// drainSocketDisabled reports a SUBROUTER_DRAIN_SOCKET value that turns the
// socket off.
func drainSocketDisabled(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "off", "0", "false", "no", "none":
		return true
	}
	return false
}

// drainSocketPath is where serve serves the drain socket, or "" for none.
//
// The default is <state dir>/drain.sock with the state directory resolved
// through symlinks, because the opener requires a canonical parent. Windows
// has no default.
func drainSocketPath(getenv func(string) string) (string, error) {
	value := strings.TrimSpace(getenv(drainSocketEnv))
	if drainSocketDisabled(value) {
		return "", nil
	}
	if value != "" {
		if !filepath.IsAbs(value) {
			return "", fmt.Errorf("%s must be an absolute path, got %q", drainSocketEnv, value)
		}
		return filepath.Clean(value), nil
	}
	if runtime.GOOS == "windows" {
		return "", nil
	}
	return filepath.Join(canonicalDir(storepath.StateDir()), drainSocketName), nil
}

// canonicalDir is dir made absolute and resolved through symlinks where it
// exists, else as given.
func canonicalDir(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	return dir
}

// drainSocket is the running drain socket; nil when there is none.
type drainSocket struct {
	path   string
	server *http.Server
}

// startDrainSocket resolves the drain socket's path and serves handler on it.
// Every failure is soft and logged; nil means no socket.
//
// A supervised worker (one handed SUBROUTER_LISTEN_FD) never opens it, for the
// Claude socket's reason: the supervisor passes its environment to every
// generation, the replacement would meet the retiring worker's lease, and the
// retiring worker's close would then unlink the path.
func startDrainSocket(getenv func(string) string, handler http.Handler, logger *slog.Logger) *drainSocket {
	if logger == nil {
		logger = slog.Default()
	}
	path, err := drainSocketPath(getenv)
	if err != nil {
		logger.Error("drain socket not served", "error", err)
		return nil
	}
	if path == "" {
		return nil
	}
	if strings.TrimSpace(getenv(inheritedListenerFDEnv)) != "" {
		logger.Info("no drain socket in a supervised worker; the supervisor owns stable sockets", "path", path)
		return nil
	}
	return serveDrainSocket(path, handler, logger)
}

// serveDrainSocket opens the socket at path and serves handler on it, on its
// own http.Server: the private server's Shutdown at SIGTERM must not close it.
func serveDrainSocket(path string, handler http.Handler, logger *slog.Logger) *drainSocket {
	listener, err := openPrivateDrainSocketListener(path, drainSocketPeerAllowed, logger)
	if err != nil {
		logger.Error("drain socket not served; drain with the admin token instead", "path", path, "error", err)
		return nil
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       time.Minute,
		ConnContext:       drainSocketConnContext,
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			logger.Error("drain socket stopped serving", "path", path, "error", err)
		}
	}()
	logger.Info("subrouter drain socket", "path", path)
	return &drainSocket{path: path, server: server}
}

// close closes the socket, unlinking it (its own inode only) and releasing the
// lease. serve defers it, so the socket answers through the SIGTERM drain.
func (d *drainSocket) close() {
	if d != nil {
		_ = d.server.Close()
	}
}

// openPrivateDrainSocketListener opens the drain socket under the Claude
// socket's rules (openPrivateServedSocket: the path-length check first, the
// lease, a stale socket removed only when nothing answers, bound 0600 under a
// temporary name) and admits only the peers allow accepts.
func openPrivateDrainSocketListener(socket string, allow func(unixPeer) bool, logger *slog.Logger) (net.Listener, error) {
	owned, err := openPrivateServedSocket(socket, "drain socket")
	if err != nil {
		return nil, err
	}
	return drainSocketListener{Listener: owned, addr: &net.UnixAddr{Name: socket, Net: "unix"}, allow: allow, logger: logger}, nil
}

// unixPeer is who is on the other end of a Unix socket, as the kernel reports
// it (unixPeerCredentials).
type unixPeer struct {
	UID int
	PID int
}

// errUnixPeerUnsupported: this OS reports no peer credentials here, and the
// socket's mode is the only gate.
var errUnixPeerUnsupported = errors.New("peer credentials are not available on this OS")

// drainSocketPeerAllowed admits root and the daemon's own user: the two the
// socket's mode admits.
func drainSocketPeerAllowed(peer unixPeer) bool {
	return peer.UID == 0 || peer.UID == os.Geteuid()
}

// drainSocketListener refuses, before any byte is read, a peer whose uid
// allow does not accept. It keeps accepting after a refusal: an error from
// Accept would stop http.Server.Serve.
type drainSocketListener struct {
	net.Listener
	addr   net.Addr
	allow  func(unixPeer) bool
	logger *slog.Logger
}

func (l drainSocketListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		peer, err := unixPeerCredentials(conn)
		switch {
		case errors.Is(err, errUnixPeerUnsupported):
			return conn, nil
		case err != nil:
			l.logger.Warn("drain socket refused a peer it could not identify", "error", err)
		case !l.allow(peer):
			l.logger.Warn("drain socket refused a peer", "uid", peer.UID, "pid", peer.PID)
		default:
			return conn, nil
		}
		_ = conn.Close()
	}
}

// Addr is the served path, not the temporary name the socket was bound under.
func (l drainSocketListener) Addr() net.Addr {
	return l.addr
}

type drainSocketPeerKey struct{}

// drainSocketConnContext records the peer for the lifecycle log line.
func drainSocketConnContext(ctx context.Context, conn net.Conn) context.Context {
	if peer, err := unixPeerCredentials(conn); err == nil {
		return context.WithValue(ctx, drainSocketPeerKey{}, peer)
	}
	return ctx
}

// drainSocketCaller names the caller in `subrouter lifecycle control`.
func drainSocketCaller(r *http.Request) string {
	if peer, ok := r.Context().Value(drainSocketPeerKey{}).(unixPeer); ok {
		return fmt.Sprintf("uid=%d pid=%d", peer.UID, peer.PID)
	}
	return "drain socket"
}
