//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/internal/agents/claude"
	"github.com/discordwell/subrouter/internal/proxy"
	"github.com/discordwell/subrouter/selectacct"
	"github.com/discordwell/subrouter/session"
)

// PATCHES section 29. These drive serve's own pieces -- newClaudeSocket, start,
// the private http.Server's ConnContext and proxy.Server's real handler -- and
// dial the socket the way Claude Code does. In Docker (Linux) an unwrapped Unix
// peer's RemoteAddr is "@", so the loopback-peer assertions exercise the real
// non-IP path, not a lucky "" .

// claudeSocketTestDir is the one directory every socket test binds in: short,
// canonical and private.
//
// Short, because a Unix socket path is at most maxUnixSocketPath bytes (103 on
// darwin) and t.TempDir under macOS's /var/folders/<..>/T/ plus the test's name
// runs past it (bind/connect: invalid argument). Canonical, because the opener
// refuses a parent that is not its own canonical path (/tmp on darwin is a
// symlink to /private/tmp). Private, because MkdirTemp creates it 0700.
func claudeSocketTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "srsock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// openPrivateClaudeSocketListener is the socket as start serves it, opened
// once and without a keeper: the opener's own rules, and peers that report
// loopback.
func openPrivateClaudeSocketListener(socket string) (net.Listener, error) {
	owned, err := openLeasedClaudeSocket(socket)
	if err != nil {
		return nil, err
	}
	return loopbackTCPListener{Listener: owned, addr: &net.UnixAddr{Name: socket, Net: "unix"}}, nil
}

func claudeSocketEnvFor(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func unixClient(socket string) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", socket)
			},
		},
	}
}

type upstreamSeen struct {
	mu    sync.Mutex
	calls []string // "<path> <Authorization>"
}

func (u *upstreamSeen) add(r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = append(u.calls, r.URL.Path+" "+r.Header.Get("Authorization"))
}

func (u *upstreamSeen) snapshot() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.calls...)
}

// The whole feature, end to end: Claude Code in first-party mode writes plain
// HTTP into the socket with Host api.anthropic.com and a placeholder bearer, and
// its policy_limits GET carries Host [::1] and no Authorization at all. On a
// daemon that ENFORCES a proxy token with the loopback exemption on (the
// laptop's plist sets SUBROUTER_ALLOW_LOOPBACK_PROXY=1), both must be served,
// and upstream must see the pool account's credential, never the placeholder.
//
// Watched red: with loopbackTCPListener removed from the opener, the POST is a
// 401 (peer "@" is not loopback, and "ssh-placeholder" is not the token).
func TestClaudeSocketServesFirstPartyClaudeCode(t *testing.T) {
	seen := &upstreamSeen{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.add(r)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/messages":
			_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(claudeSocketTestDir(t), "claude.sock")
	sock := newClaudeSocket("", claudeSocketEnvFor(map[string]string{claudeSocketEnv: socketPath}))
	handler := proxy.Server{
		ClaudeUpstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID: "claude-pool", Provider: accounts.ProviderClaude,
			AuthMode: accounts.AuthModeOAuth, Token: "pool-account-token",
		}},
		Sessions:                       sessions,
		Scheduler:                      selectacct.NewScheduler(nil),
		MaxBodyBytes:                   1 << 20,
		LocalProxyToken:                "local-proxy-token",
		AllowLoopbackProxyWithoutToken: true,
		ClaudeSocketStatus:             sock.statusFunc(),
	}.Handler()
	server := &http.Server{Handler: handler, ConnContext: proxy.LocalDataConnContext, ReadHeaderTimeout: 10 * time.Second}
	sock.start(server.Serve, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = server.Close() })
	if got := sock.Status(); got != claudeSocketServing {
		t.Fatalf("status = %q, want %q", got, claudeSocketServing)
	}
	client := unixClient(socketPath)

	post, _ := http.NewRequest(http.MethodPost, "http://api.anthropic.com/v1/messages?beta=true",
		strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	post.Header.Set("Authorization", "Bearer ssh-placeholder")
	post.Header.Set("Content-Type", "application/json")
	post.Header.Set("anthropic-version", "2023-06-01")
	post.Header.Set("anthropic-beta", "oauth-2025-04-20,interleaved-thinking-2025-05-14")
	post.Header.Set("x-app", "cli")
	post.Header.Set("User-Agent", "claude-cli/2.1.284 (external, sdk-cli)")
	response, err := client.Do(post)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/messages over the socket = %d %s, want 200", response.StatusCode, body)
	}

	get, _ := http.NewRequest(http.MethodGet, "http://[::1]/api/claude_code/policy_limits", nil)
	get.Header.Set("User-Agent", "claude-code/2.1.284")
	response, err = client.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET policy_limits over the socket = %d %s, want 200", response.StatusCode, body)
	}

	got := seen.snapshot()
	for _, want := range []string{
		"/v1/messages Bearer pool-account-token",
		"/api/claude_code/policy_limits Bearer pool-account-token",
	} {
		found := false
		for _, call := range got {
			found = found || call == want
		}
		if !found {
			t.Fatalf("upstream never saw %q; it saw %q", want, got)
		}
	}
	for _, call := range got {
		if strings.Contains(call, "ssh-placeholder") {
			t.Fatalf("the client's placeholder bearer reached upstream: %q", call)
		}
	}
}

// Every accepted connection reports 127.0.0.1, whatever the kernel says about
// a Unix peer, so every loopback gate sees the same machine a TCP client on
// this box is.
func TestClaudeSocketPeerIsLoopback(t *testing.T) {
	socketPath := filepath.Join(claudeSocketTestDir(t), "claude.sock")
	sock := newClaudeSocket("", claudeSocketEnvFor(map[string]string{claudeSocketEnv: socketPath}))
	remote := make(chan string, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote <- r.RemoteAddr
	}), ReadHeaderTimeout: 10 * time.Second}
	sock.start(server.Serve, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = server.Close() })
	response, err := unixClient(socketPath).Get("http://api.anthropic.com/")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	got := <-remote
	host, _, err := net.SplitHostPort(got)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		t.Fatalf("RemoteAddr = %q, want a loopback IP:port", got)
	}
}

// M3: a crash between bind and chmod must never leave a start-blocking socket.
// The socket is bound under <name>.bind and renamed into place only at 0600, so
// the most a crash leaves is a stale <name>.bind of any mode -- simulated here
// at 0755, bind(2)'s mode under umask 022 -- which the next start removes.
//
// Watched red: with removeStaleBind skipped, the bind fails EADDRINUSE; with the
// chmod at 0666, the mode assertion fails.
func TestClaudeSocketCrashLeftoverNeverBlocksStart(t *testing.T) {
	dir := claudeSocketTestDir(t)
	socketPath := filepath.Join(dir, "claude.sock")
	leftover, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath + ".bind", Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	leftover.SetUnlinkOnClose(false)
	if err := os.Chmod(socketPath+".bind", 0o755); err != nil {
		t.Fatal(err)
	}
	_ = leftover.Close()

	listener, err := openPrivateClaudeSocketListener(socketPath)
	if err != nil {
		t.Fatalf("start after a crash mid-bind = %v, want it to recover", err)
	}
	defer listener.Close()
	info, err := os.Lstat(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, want a mode-0600 socket", info.Mode())
	}
	if _, err := os.Lstat(socketPath + ".bind"); !os.IsNotExist(err) {
		t.Fatalf("temporary bind name still present: %v", err)
	}
	if listener.Addr().String() != socketPath {
		t.Fatalf("listener address = %q, want the served path %q", listener.Addr(), socketPath)
	}
	// The rename kept the listening inode: a client dialing the served path
	// reaches this listener.
	accepted := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.Close()
		}
		accepted <- err
	}()
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial the renamed socket: %v", err)
	}
	_ = conn.Close()
	if err := <-accepted; err != nil {
		t.Fatalf("accept on the renamed socket: %v", err)
	}
}

// The temporary name is only ever removed when it is a current-user socket: a
// regular file (or anything else) there is refused and left alone.
func TestClaudeSocketRefusesNonSocketAtBindName(t *testing.T) {
	socketPath := filepath.Join(claudeSocketTestDir(t), "claude.sock")
	if err := os.WriteFile(socketPath+".bind", []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openPrivateClaudeSocketListener(socketPath); err == nil || !strings.Contains(err.Error(), "refuse unsafe stale bind name") {
		t.Fatalf("open = %v, want a refusal", err)
	}
	if body, err := os.ReadFile(socketPath + ".bind"); err != nil || string(body) != "keep" {
		t.Fatalf("regular file at the bind name was touched: %q %v", body, err)
	}
}

// A configured path too long for a Unix socket is refused with an error that
// names the limit, in the log line an operator reads, instead of bind(2)'s bare
// "invalid argument" about a <path>.bind name nobody configured.
//
// maxUnixSocketPath is pinned to the OS from both sides: net.Listen binds a
// path exactly that long and refuses one byte more with EINVAL, so a constant
// set too high or too strict fails here. A socket path exactly at the derived
// limit is then served, and one byte over is refused before anything on disk
// is touched.
//
// Watched red: with the length check removed, the one-byte-over path logs
// "listen unix <path>.bind: bind: invalid argument"; with maxUnixSocketPath set
// 2 bytes too strict, the OS subtest fails; with the check left in
// listenPrivate, after the liveness dial and the stale-socket removal, the live
// socket in the last subtest is unlinked and a .lock is left behind.
func TestClaudeSocketTooLongPathNamesTheLimit(t *testing.T) {
	limit := maxUnixSocketPath - len(".bind")
	pathOfLength := func(t *testing.T, n int) string {
		t.Helper()
		dir := claudeSocketTestDir(t)
		path := filepath.Join(dir, strings.Repeat("s", n-len(dir)-1))
		if len(path) != n {
			t.Fatalf("built a %d-byte path, want %d", len(path), n)
		}
		return path
	}
	t.Run("the OS binds exactly maxUnixSocketPath bytes and no more", func(t *testing.T) {
		listener, err := net.Listen("unix", pathOfLength(t, maxUnixSocketPath))
		if err != nil {
			t.Fatalf("listen on a %d-byte path = %v, want it bound: maxUnixSocketPath is too high", maxUnixSocketPath, err)
		}
		_ = listener.Close()
		listener, err = net.Listen("unix", pathOfLength(t, maxUnixSocketPath+1))
		if err == nil {
			_ = listener.Close()
		}
		if !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("listen on a %d-byte path = %v, want EINVAL: maxUnixSocketPath is too strict", maxUnixSocketPath+1, err)
		}
	})
	t.Run("at the limit is served", func(t *testing.T) {
		listener, err := openPrivateClaudeSocketListener(pathOfLength(t, limit))
		if err != nil {
			t.Fatalf("open a %d-byte path = %v, want it served", limit, err)
		}
		_ = listener.Close()
	})
	t.Run("one byte over is refused and says why", func(t *testing.T) {
		socketPath := pathOfLength(t, limit+1)
		var logged bytes.Buffer
		sock := newClaudeSocket("", claudeSocketEnvFor(map[string]string{claudeSocketEnv: socketPath}))
		server := &http.Server{Handler: http.NotFoundHandler(), ReadHeaderTimeout: 10 * time.Second}
		t.Cleanup(func() { _ = server.Close() })
		sock.start(server.Serve, "", slog.New(slog.NewTextHandler(&logged, nil)))
		if got := sock.Status(); got != claudeSocketUnavailable {
			t.Fatalf("status = %q, want %q", got, claudeSocketUnavailable)
		}
		want := fmt.Sprintf("socket path is %d bytes, over the %d-byte limit", limit+1, limit)
		if !strings.Contains(logged.String(), want) {
			t.Fatalf("log = %q, want it to name the limit: %q", logged.String(), want)
		}
		for _, path := range []string{socketPath, socketPath + ".bind", socketPath + ".lock"} {
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("a refused path left %s behind: %v", path, err)
			}
		}
	})
	t.Run("a refused path never removes a live socket there", func(t *testing.T) {
		// A live 0600 socket at a path too long to dial by its absolute name,
		// bound relative to the working directory as any process can. The
		// liveness dial would get EINVAL, read it as "nobody listens", and the
		// stale-socket removal would unlink a socket someone is serving on.
		socketPath := pathOfLength(t, maxUnixSocketPath+1)
		t.Chdir(filepath.Dir(socketPath))
		live, err := net.Listen("unix", filepath.Base(socketPath))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = live.Close() })
		if err := os.Chmod(socketPath, 0o600); err != nil {
			t.Fatal(err)
		}
		if listener, err := openPrivateClaudeSocketListener(socketPath); err == nil {
			_ = listener.Close()
			t.Fatalf("opened a %d-byte path, want it refused", len(socketPath))
		} else if !strings.Contains(err.Error(), fmt.Sprintf("over the %d-byte limit", limit)) {
			t.Fatalf("open error = %v, want it to name the %d-byte limit", err, limit)
		}
		conn, err := net.Dial("unix", filepath.Base(socketPath))
		if err != nil {
			t.Fatalf("the live socket stopped answering after a refused open: %v", err)
		}
		_ = conn.Close()
		if _, err := os.Lstat(socketPath + ".lock"); !os.IsNotExist(err) {
			t.Fatalf("a refused path left its .lock behind: %v", err)
		}
	})
}

// M2: the socket fails soft. A socket that cannot open, and one that stops
// serving later, both leave TCP serving -- and health, on TCP, says which.
// Contrast TestPrivateLocalDataListenerFailureIsFatal: reusing that path for
// this socket would have exited the process, and launchd's KeepAlive would have
// respawned it into the same failure with every TCP client down.
func TestClaudeSocketFailureKeepsTCPServingAndShowsInHealth(t *testing.T) {
	health := func(t *testing.T, address string) map[string]any {
		t.Helper()
		response, err := http.Get("http://" + address + "/_subrouter/health")
		if err != nil {
			t.Fatalf("TCP health: %v", err)
		}
		defer response.Body.Close()
		var payload map[string]any
		if err := json.NewDecoder(response.Body).Decode(&payload); err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("TCP health = %d %v", response.StatusCode, err)
		}
		return payload
	}
	serveTCP := func(t *testing.T, sock *claudeSocket) (*http.Server, string) {
		t.Helper()
		server := &http.Server{Handler: proxy.Server{ClaudeSocketStatus: sock.statusFunc()}.Handler(), ReadHeaderTimeout: 10 * time.Second}
		tcp, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = server.Close() })
		// As serve does: the socket starts (and puts its guard on the
		// server) before the server serves TCP.
		sock.startGuarded(server, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
		go func() { _ = server.Serve(tcp) }()
		return server, tcp.Addr().String()
	}

	t.Run("cannot open", func(t *testing.T) {
		sock := newClaudeSocket("", claudeSocketEnvFor(map[string]string{claudeSocketEnv: "relative/claude.sock"}))
		_, address := serveTCP(t, sock)
		if got := health(t, address)["claude_socket"]; got != claudeSocketUnavailable {
			t.Fatalf("health claude_socket = %v, want %q", got, claudeSocketUnavailable)
		}
	})

	t.Run("stops serving", func(t *testing.T) {
		socketPath := filepath.Join(claudeSocketTestDir(t), "claude.sock")
		sock := newClaudeSocket("", claudeSocketEnvFor(map[string]string{claudeSocketEnv: socketPath}))
		_, address := serveTCP(t, sock)
		if got := health(t, address)["claude_socket"]; got != claudeSocketServing {
			t.Fatalf("health claude_socket = %v, want %q", got, claudeSocketServing)
		}
		// A runtime failure: the listener dies under Serve.
		_ = sock.listener.Close()
		deadline := time.Now().Add(5 * time.Second)
		for sock.Status() != claudeSocketStopped {
			if time.Now().After(deadline) {
				t.Fatalf("status = %q after the listener closed, want %q", sock.Status(), claudeSocketStopped)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if got := health(t, address)["claude_socket"]; got != claudeSocketStopped {
			t.Fatalf("health claude_socket = %v, want %q", got, claudeSocketStopped)
		}
	})

	t.Run("not configured", func(t *testing.T) {
		sock := newClaudeSocket("", claudeSocketEnvFor(nil))
		if sock != nil {
			t.Fatal("an unset env var configured a socket")
		}
		_, address := serveTCP(t, sock)
		if got, ok := health(t, address)["claude_socket"]; ok {
			t.Fatalf("unconfigured daemon's health gained claude_socket = %v", got)
		}
	})
}

// M4: a supervised worker never binds the socket. The supervisor hands every
// worker generation its own environment; were the worker to bind, the
// replacement generation would meet the retiring one's lease, and the retiring
// one would later unlink the path with nobody left to re-bind it.
//
// Watched red: with the SUBROUTER_LISTEN_FD check removed, the path is bound.
func TestClaudeSocketLeftToTheSupervisorInSupervisedWorker(t *testing.T) {
	socketPath := filepath.Join(claudeSocketTestDir(t), "claude.sock")
	sock := newClaudeSocket("", claudeSocketEnvFor(map[string]string{
		claudeSocketEnv:        socketPath,
		inheritedListenerFDEnv: "3",
	}))
	server := &http.Server{Handler: http.NotFoundHandler(), ReadHeaderTimeout: 10 * time.Second}
	t.Cleanup(func() { _ = server.Close() })
	sock.start(server.Serve, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := sock.Status(); got != "owned_by_supervisor" {
		t.Fatalf("status = %q, want owned_by_supervisor", got)
	}
	for _, path := range []string{socketPath, socketPath + ".lock", socketPath + ".bind"} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("supervised worker touched %s: %v", path, err)
		}
	}
}

// Two processes naming one path: the second waits and leaves the first
// serving, rather than failing either one, and serves the path once the first
// lets go of it. PATCHES section 77.1: it used to report unavailable and never
// try again.
func TestClaudeSocketSecondOwnerIsSoft(t *testing.T) {
	socketPath := filepath.Join(claudeSocketTestDir(t), "claude.sock")
	env := claudeSocketEnvFor(map[string]string{claudeSocketEnv: socketPath})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	first, second := newClaudeSocket("", env), newClaudeSocket("", env)
	answering := func(body string) *http.Server {
		return &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, body)
		}), ReadHeaderTimeout: 10 * time.Second}
	}
	firstServer, secondServer := answering("first"), answering("second")
	t.Cleanup(func() {
		_ = firstServer.Close()
		_ = secondServer.Close()
		first.close()
		second.close()
	})
	first.start(firstServer.Serve, "", logger)
	second.start(secondServer.Serve, "", logger)
	if first.Status() != claudeSocketServing || second.Status() != "waiting" {
		t.Fatalf("statuses = %q, %q; want serving, waiting", first.Status(), second.Status())
	}
	answer := func() (string, error) {
		client := unixClient(socketPath)
		defer client.CloseIdleConnections()
		response, err := client.Get("http://api.anthropic.com/")
		if err != nil {
			return "", err
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		return string(body), err
	}
	if body, err := answer(); err != nil || body != "first" {
		t.Fatalf("socket answered %q, %v; want the first owner", body, err)
	}

	_ = firstServer.Close()
	first.close()
	deadline := time.Now().Add(20 * time.Second)
	for {
		body, err := answer()
		if err == nil && body == "second" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the first owner let go the socket answered %q, %v; want the second owner (status %q)", body, err, second.Status())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := second.Status(); got != claudeSocketServing {
		t.Fatalf("second owner's status = %q once it serves, want %q", got, claudeSocketServing)
	}
}

// The same path for both sockets is refused for the Claude one, before either
// lease can decide it, so the fatal local-data socket keeps its path.
func TestClaudeSocketRefusesLocalDataSocketPath(t *testing.T) {
	socketPath := filepath.Join(claudeSocketTestDir(t), "local-data.sock")
	sock := newClaudeSocket("", claudeSocketEnvFor(map[string]string{claudeSocketEnv: socketPath}))
	server := &http.Server{Handler: http.NotFoundHandler(), ReadHeaderTimeout: 10 * time.Second}
	t.Cleanup(func() { _ = server.Close() })
	sock.start(server.Serve, socketPath, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := sock.Status(); got != claudeSocketUnavailable {
		t.Fatalf("status = %q, want %q", got, claudeSocketUnavailable)
	}
	if _, err := os.Lstat(socketPath + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("claude socket took the local-data socket's lease: %v", err)
	}
}

// M5: a Claude child launched by sr (for a remote server, a managed login, the
// Bedrock gateway or claude-direct) must not inherit a socket-mode shell's
// ANTHROPIC_UNIX_SOCKET. Claude Code 2.1.284 sends every API request to a
// process-env socket regardless of ANTHROPIC_BASE_URL, and an empty value in
// --settings does not override it (measured), so stripping it from the env is
// the only scrub that works.
//
// Watched red before the fix: the key is absent from the routing list, and both
// child environments below keep it.
func TestClaudeChildEnvironmentsStripUnixSocket(t *testing.T) {
	const socketVar = "ANTHROPIC_UNIX_SOCKET"
	inherited := []string{"PATH=/usr/bin", socketVar + "=/home/user/.subrouter/claude.sock", "ANTHROPIC_BASE_URL=http://api.anthropic.com"}
	hasSocket := func(env []string) bool {
		for _, entry := range env {
			if strings.HasPrefix(strings.ToUpper(entry), socketVar+"=") {
				return true
			}
		}
		return false
	}
	if env := claudeSettingsChildEnvironment(inherited, "https://remote.example:31415", ""); hasSocket(env) {
		t.Fatalf("sr claude child keeps %s: %q", socketVar, env)
	}
	if env := claudeAWSChildEnvironment(inherited, "https://remote.example:31415/bedrock", "us-east-1", "fable", ""); hasSocket(env) {
		t.Fatalf("sr claude-aws child keeps %s: %q", socketVar, env)
	}
	t.Setenv(socketVar, "/home/user/.subrouter/claude.sock")
	if env := claude.EnvForConfigDir(t.TempDir()); hasSocket(env) {
		t.Fatalf("managed login/auth-status child keeps %s", socketVar)
	}
}

// A stale socket at the served path from a previous run (crash, SIGKILL) is
// replaced -- but only when nothing is listening there. A live listener this
// daemon does not own (no lease) is refused and left serving.
//
// This test cannot see removeStaleSocket: the final renameat replaces a dead
// socket at the served path whether or not the cleanup ran. The cleanup's
// observable job is the refusal, which TestClaudeSocketRefusesUnsafeEntryAtServedPath
// pins.
//
// Watched red: with the claudeSocketAnswers check removed, the second case
// unlinks the live socket and binds over it.
func TestClaudeSocketStaleSocketRemovedOnlyWhenNobodyListens(t *testing.T) {
	bindAt := func(t *testing.T, path string) *net.UnixListener {
		t.Helper()
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		listener.SetUnlinkOnClose(false)
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		return listener
	}
	t.Run("dead socket is replaced", func(t *testing.T) {
		socketPath := filepath.Join(claudeSocketTestDir(t), "claude.sock")
		_ = bindAt(t, socketPath).Close() // a previous run's leftover: file, no listener
		listener, err := openPrivateClaudeSocketListener(socketPath)
		if err != nil {
			t.Fatalf("open over a dead socket = %v, want it replaced", err)
		}
		defer listener.Close()
		if info, err := os.Lstat(socketPath); err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
			t.Fatalf("served path after replace: %v %v", info, err)
		}
	})
	t.Run("live foreign listener is refused", func(t *testing.T) {
		socketPath := filepath.Join(claudeSocketTestDir(t), "claude.sock")
		foreign := bindAt(t, socketPath)
		defer foreign.Close()
		go func() {
			for {
				conn, err := foreign.Accept()
				if err != nil {
					return
				}
				_ = conn.Close()
			}
		}()
		if _, err := openPrivateClaudeSocketListener(socketPath); err == nil || !strings.Contains(err.Error(), "another process is listening") {
			t.Fatalf("open over a live socket = %v, want a refusal", err)
		}
		conn, err := net.Dial("unix", socketPath)
		if err != nil {
			t.Fatalf("the foreign listener lost its path: %v", err)
		}
		_ = conn.Close()
	})
}

// Whatever sits at the served path is removed only when it is a
// current-user-owned mode-0600 socket (removeStaleSocket). Anything else -- a
// regular file, a symlink, a socket of another mode -- is refused and left
// exactly as found. Without that check the final renameat would silently
// clobber it: rename(2) replaces a file or symlink at the target name.
//
// Watched red: with lease.removeStaleSocket() removed from
// openPrivateClaudeSocketListener, every case opens successfully and the entry
// is replaced by our socket.
func TestClaudeSocketRefusesUnsafeEntryAtServedPath(t *testing.T) {
	refused := func(t *testing.T, socketPath string) {
		t.Helper()
		listener, err := openPrivateClaudeSocketListener(socketPath)
		if err == nil {
			_ = listener.Close()
			t.Fatal("open over an unsafe entry succeeded, want a refusal")
		}
		if !strings.Contains(err.Error(), "refuse unsafe stale socket") {
			t.Fatalf("open = %v, want the unsafe-stale-socket refusal", err)
		}
	}
	t.Run("regular file", func(t *testing.T) {
		socketPath := filepath.Join(claudeSocketTestDir(t), "claude.sock")
		if err := os.WriteFile(socketPath, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		refused(t, socketPath)
		if info, err := os.Lstat(socketPath); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("regular file at the served path was replaced: %v %v", info, err)
		}
		if body, err := os.ReadFile(socketPath); err != nil || string(body) != "keep" {
			t.Fatalf("regular file at the served path was touched: %q %v", body, err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		dir := claudeSocketTestDir(t)
		socketPath := filepath.Join(dir, "claude.sock")
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, socketPath); err != nil {
			t.Fatal(err)
		}
		refused(t, socketPath)
		if got, err := os.Readlink(socketPath); err != nil || got != target {
			t.Fatalf("symlink at the served path was replaced: %q %v", got, err)
		}
	})
	t.Run("dead socket of mode 0755", func(t *testing.T) {
		socketPath := filepath.Join(claudeSocketTestDir(t), "claude.sock")
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		listener.SetUnlinkOnClose(false)
		_ = listener.Close()
		if err := os.Chmod(socketPath, 0o755); err != nil {
			t.Fatal(err)
		}
		before, err := os.Lstat(socketPath)
		if err != nil {
			t.Fatal(err)
		}
		refused(t, socketPath)
		after, err := os.Lstat(socketPath)
		if err != nil || !os.SameFile(before, after) || after.Mode().Perm() != 0o755 {
			t.Fatalf("0755 socket at the served path was replaced: %v %v", after, err)
		}
	})
}

// Graceful shutdown (serve's Shutdown on SIGTERM) removes the socket file, so
// no stale socket is left for a client to hang on or the next start to judge.
func TestClaudeSocketRemovedOnGracefulShutdown(t *testing.T) {
	socketPath := filepath.Join(claudeSocketTestDir(t), "claude.sock")
	sock := newClaudeSocket("", claudeSocketEnvFor(map[string]string{claudeSocketEnv: socketPath}))
	server := &http.Server{Handler: http.NotFoundHandler(), ReadHeaderTimeout: 10 * time.Second}
	sock.start(server.Serve, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if sock.Status() != claudeSocketServing {
		t.Fatalf("status = %q", sock.Status())
	}
	if _, err := os.Lstat(socketPath); err != nil {
		t.Fatalf("socket not created: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	sock.close() // serve's deferred close: a no-op after Shutdown, and must not fail
	if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("socket file survives graceful shutdown: %v", err)
	}
}

// The --unix-socket flag names the socket for hand runs and wins over the
// environment; the environment alone still configures it.
func TestClaudeSocketFlagWinsOverEnvironment(t *testing.T) {
	env := claudeSocketEnvFor(map[string]string{claudeSocketEnv: "/env/claude.sock"})
	if got := newClaudeSocket(" /flag/claude.sock ", env); got == nil || got.path != "/flag/claude.sock" {
		t.Fatalf("flag + env = %+v, want the flag's path", got)
	}
	if got := newClaudeSocket("", env); got == nil || got.path != "/env/claude.sock" {
		t.Fatalf("env only = %+v, want the env's path", got)
	}
}

// supervise refuses --unix-socket in worker args, as it refuses
// --local-data-socket: every generation would bind the same path.
func TestSupervisorRefusesWorkerUnixSocket(t *testing.T) {
	for _, args := range [][]string{{"--unix-socket", "/x/claude.sock"}, {"--unix-socket=/x/claude.sock"}} {
		config := supervisorConfig{
			Addr: "127.0.0.1:31415", ControlSocket: "/var/run/subrouter-test.sock", WorkerBin: "/usr/local/bin/subrouter",
			ReadyTimeout: time.Second, DrainTimeout: time.Second, WorkerStopGrace: time.Second,
			TakeoverListenerFD: -1, WorkerArgs: args,
		}
		if err := validateSupervisorConfig(config); err == nil || !strings.Contains(err.Error(), "--unix-socket") {
			t.Fatalf("worker args %q: validate = %v, want a refusal", args, err)
		}
	}
}

// The exemption is exactly TCP loopback's, no broader: with
// SUBROUTER_ALLOW_LOOPBACK_PROXY off, a token-enforcing daemon refuses the
// placeholder over the socket just as it does over 127.0.0.1 TCP. TCP gets the
// 401; the socket gets the guard's 503, because Claude Code answers a 401 by
// sending the human's stored login (proxy.ClaudeSocketGuard).
func TestClaudeSocketPeerGetsNoMoreThanTCPLoopback(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := proxy.Server{
		ClaudeUpstream: upstreamURL,
		Sessions:       sessions,
		Accounts: []accounts.Account{{
			ID: "claude-pool", Provider: accounts.ProviderClaude,
			AuthMode: accounts.AuthModeOAuth, Token: "pool-account-token",
		}},
		Scheduler:       selectacct.NewScheduler(nil),
		MaxBodyBytes:    1 << 20,
		LocalProxyToken: "local-proxy-token",
	}.Handler()
	socketPath := filepath.Join(claudeSocketTestDir(t), "claude.sock")
	sock := newClaudeSocket("", claudeSocketEnvFor(map[string]string{claudeSocketEnv: socketPath}))
	server := &http.Server{Handler: handler, ConnContext: proxy.LocalDataConnContext, ReadHeaderTimeout: 10 * time.Second}
	sock.startGuarded(server, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = server.Close() })
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(tcp) }()

	status := func(client *http.Client, target string) int {
		t.Helper()
		request, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":1,"messages":[]}`))
		request.Header.Set("Authorization", "Bearer ssh-placeholder")
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return response.StatusCode
	}
	viaTCP := status(http.DefaultClient, "http://"+tcp.Addr().String()+"/v1/messages?beta=true")
	viaSocket := status(unixClient(socketPath), "http://api.anthropic.com/v1/messages?beta=true")
	if viaTCP != http.StatusUnauthorized || viaSocket != http.StatusServiceUnavailable {
		t.Fatalf("placeholder without the loopback exemption: TCP %d, socket %d; want 401 and the guard's 503", viaTCP, viaSocket)
	}
}

// The socket brings its guard: start puts it in front of the whole server and
// marks the socket's connections, so the same refusal is a 401 on TCP and a 503
// on the socket, and the adoption canary counts the socket's clients only. The
// server is built as serve builds it (proxy.LocalDataConnContext; the guard in
// the proxy's health).
//
// Watched red: with guardServer's ConnContext not marking loopbackTCPConn, or
// with startGuarded not calling guardServer, the socket's answer is the 401 and
// the canary reads zero.
func TestClaudeSocketAnswers503WhereTCPAnswers401(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if (*claudeSocket)(nil).socketGuard() != nil {
		t.Fatal("an unconfigured socket has a guard")
	}
	socketPath := filepath.Join(claudeSocketTestDir(t), "claude.sock")
	sock := newClaudeSocket("", claudeSocketEnvFor(map[string]string{claudeSocketEnv: socketPath}))
	guard := sock.socketGuard()
	if guard == nil {
		t.Fatal("a configured socket has no guard")
	}
	// No loopback exemption: the daemon refuses the placeholder, on either
	// listener, with a 401.
	handler := proxy.Server{
		ClaudeUpstream: upstreamURL,
		Sessions:       sessions,
		Accounts: []accounts.Account{{
			ID: "claude-pool", Provider: accounts.ProviderClaude,
			AuthMode: accounts.AuthModeOAuth, Token: "pool-account-token",
		}},
		Scheduler:          selectacct.NewScheduler(nil),
		MaxBodyBytes:       1 << 20,
		LocalProxyToken:    "local-proxy-token",
		ClaudeSocketStatus: sock.statusFunc(),
		ClaudeSocketGuard:  guard,
	}.Handler()
	server := &http.Server{Handler: handler, ConnContext: proxy.LocalDataConnContext, ReadHeaderTimeout: 10 * time.Second}
	sock.startGuarded(server, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = server.Close() })
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(tcp) }()

	turn := func(client *http.Client, target, bearer string) *http.Response {
		t.Helper()
		request, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":1,"messages":[]}`))
		request.Header.Set("Authorization", "Bearer "+bearer)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Claude-Code-Session-Id", "sess-wiring")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return response
	}
	tcpTarget := "http://" + tcp.Addr().String() + "/v1/messages?beta=true"
	socketTarget := "http://api.anthropic.com/v1/messages?beta=true"
	if got := turn(http.DefaultClient, tcpTarget, claudeSocketPlaceholderToken); got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("TCP = %d, want the 401", got.StatusCode)
	}
	got := turn(unixClient(socketPath), socketTarget, claudeSocketPlaceholderToken)
	if got.StatusCode != http.StatusServiceUnavailable || got.Header.Get("X-Subrouter-Reason") != "claude_socket_refused" {
		t.Fatalf("socket = %d (X-Subrouter-Reason %q), want the guard's 503", got.StatusCode, got.Header.Get("X-Subrouter-Reason"))
	}
	if status := guard.Status(); status != (proxy.ClaudeSocketGuardStatus{RefusalsAnswered503: 1}) {
		t.Fatalf("guard status after the placeholder = %+v", status)
	}

	turn(http.DefaultClient, tcpTarget, "sk-ant-oat01-adopted")
	turn(unixClient(socketPath), socketTarget, "sk-ant-oat01-adopted")
	want := proxy.ClaudeSocketGuardStatus{AdoptionCanaryRequests: 1, AdoptionCanarySessions: 1, RefusalsAnswered503: 2}
	if status := guard.Status(); status != want {
		t.Fatalf("guard status = %+v, want %+v (the socket's adopted bearer only)", status, want)
	}
	response, err := http.Get("http://" + tcp.Addr().String() + "/_subrouter/health")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var health struct {
		Guard proxy.ClaudeSocketGuardStatus `json:"claude_socket_guard"`
	}
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil || health.Guard != want {
		t.Fatalf("health claude_socket_guard = %+v (%v), want %+v", health.Guard, err, want)
	}
}
