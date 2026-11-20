//go:build !windows

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// drainSocketTestServer is the proxy.Server serve builds, as far as the drain
// socket sees it: a lifecycle, and every admin credential configured, so a
// pass cannot be explained by a tokenless fallback.
func drainSocketTestServer(logs io.Writer) proxy.Server {
	if logs == nil {
		logs = io.Discard
	}
	return proxy.Server{
		Lifecycle:          proxy.NewLifecycle(),
		AdminToken:         "admin-secret",
		AccountImportToken: "import-secret",
		LocalProxyToken:    "proxy-secret",
		Logger:             slog.New(slog.NewTextHandler(logs, nil)),
	}
}

// runDrainCommand runs `subrouter <verb> --socket <socket>` the way main does.
func runDrainCommand(t *testing.T, verb, socket string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	args := []string{}
	if socket != "" {
		args = append(args, "--socket", socket)
	}
	err := drainCommand(verb, args, &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

// PATCHES §38.1. An operator logged in to the machine drains, reads and
// resumes the daemon with the CLI over its drain socket, holding no token --
// on a daemon that has every admin credential configured. The socket is a
// mode-0600 socket, and the lifecycle log names the caller by uid and pid.
//
// Watched red: with serve's handler (DrainSocketHandler) swapped for one that
// requires the admin token, drain answers 401; with the socket bound without
// the 0600 chmod, the mode check fails.
func TestDrainSocketDrainsResumesAndReportsWithNoToken(t *testing.T) {
	var logs bytes.Buffer
	server := drainSocketTestServer(&logs)
	socket := filepath.Join(claudeSocketTestDir(t), drainSocketName)
	sock := serveDrainSocket(socket, server.DrainSocketHandler(drainSocketCaller), slog.New(slog.NewTextHandler(&logs, nil)))
	if sock == nil {
		t.Fatalf("drain socket not served:\n%s", logs.String())
	}
	t.Cleanup(sock.close)
	if info, err := os.Lstat(socket); err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("drain socket on disk = %v %v, want a mode-0600 socket", info, err)
	}

	stdout, stderr, err := runDrainCommand(t, "drain", socket)
	if err != nil || !server.Lifecycle.Draining() {
		t.Fatalf("drain = %v (draining=%v)\nstdout: %s\nstderr: %s", err, server.Lifecycle.Draining(), stdout, stderr)
	}
	if !strings.Contains(stderr, "new sessions get 503") {
		t.Fatalf("drain did not say what it does: %q", stderr)
	}
	want := "uid=" + strconv.Itoa(os.Getuid())
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		want += " pid=" + strconv.Itoa(os.Getpid())
	}
	if !strings.Contains(logs.String(), "subrouter lifecycle control") || !strings.Contains(logs.String(), want) {
		t.Fatalf("the drain was not logged with its caller (%s):\n%s", want, logs.String())
	}

	stdout, _, err = runDrainCommand(t, "drain-status", socket)
	var status map[string]any
	if err != nil || json.Unmarshal([]byte(stdout), &status) != nil || status["draining"] != true {
		t.Fatalf("drain-status = %v %q, want draining", err, stdout)
	}
	if _, ok := status["active_websocket_turns"]; !ok {
		t.Fatalf("drain-status %v lacks active_websocket_turns", status)
	}

	if _, _, err := runDrainCommand(t, "resume", socket); err != nil || server.Lifecycle.Draining() {
		t.Fatalf("resume = %v (draining=%v), want it lifted", err, server.Lifecycle.Draining())
	}

	// The verbs are dispatched under both program names.
	for _, program := range []string{"subrouter", "sr"} {
		if err := runForProgram(program, []string{"drain-status", "--socket", socket}); err != nil {
			t.Fatalf("%s drain-status: %v", program, err)
		}
	}

	sock.close()
	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		t.Fatalf("drain socket still on disk after close: %v", err)
	}
}

// Where serve puts the socket: <state dir>/drain.sock by default, anywhere
// absolute with SUBROUTER_DRAIN_SOCKET, nowhere with =off or in a supervised
// worker.
func TestDrainSocketPathDefaultsToTheStateDir(t *testing.T) {
	state := claudeSocketTestDir(t)
	t.Setenv("SUBROUTER_STATE_DIR", state)
	env := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}

	for name, tc := range map[string]struct {
		values map[string]string
		// Off and a supervised worker are choices, and quiet; a relative path
		// is a mistake, and says so.
		logged string
	}{
		"off":               {values: map[string]string{drainSocketEnv: "OFF"}},
		"supervised worker": {values: map[string]string{inheritedListenerFDEnv: "3"}},
		"relative path":     {values: map[string]string{drainSocketEnv: "drain.sock"}, logged: "must be an absolute path"},
	} {
		var logs bytes.Buffer
		if sock := startDrainSocket(env(tc.values), drainSocketTestServer(nil).DrainSocketHandler(nil), slog.New(slog.NewTextHandler(&logs, nil))); sock != nil {
			sock.close()
			t.Fatalf("%s: drain socket served at %s, want none", name, sock.path)
		}
		if tc.logged == "" && strings.Contains(logs.String(), "level=ERROR") {
			t.Fatalf("%s: logged an error:\n%s", name, logs.String())
		}
		if tc.logged != "" && !strings.Contains(logs.String(), tc.logged) {
			t.Fatalf("%s: log lacks %q:\n%s", name, tc.logged, logs.String())
		}
	}
	if entries, _ := os.ReadDir(state); len(entries) != 0 {
		t.Fatalf("state dir holds %v after the refused starts, want nothing", entries)
	}

	sock := startDrainSocket(env(nil), drainSocketTestServer(nil).DrainSocketHandler(nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if sock == nil || sock.path != filepath.Join(state, drainSocketName) {
		t.Fatalf("default drain socket = %+v, want %s", sock, filepath.Join(state, drainSocketName))
	}
	if _, _, err := runDrainCommand(t, "drain-status", ""); err != nil {
		t.Fatalf("drain-status with SUBROUTER_STATE_DIR and no --socket: %v", err)
	}
	sock.close()

	moved := filepath.Join(claudeSocketTestDir(t), "elsewhere.sock")
	sock = startDrainSocket(env(map[string]string{drainSocketEnv: moved}), drainSocketTestServer(nil).DrainSocketHandler(nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if sock == nil || sock.path != moved {
		t.Fatalf("SUBROUTER_DRAIN_SOCKET=%s served %+v", moved, sock)
	}
	sock.close()
}

// The kernel-reported peer is checked before a byte is read: a peer the check
// refuses gets its connection closed and changes nothing. The real check
// admits root and this user, and reads this process's own uid and pid.
//
// Watched red: with drainSocketListener.Accept returning every connection
// unchecked, the refused client drains the daemon.
func TestDrainSocketRefusesAPeerTheCredentialCheckRejects(t *testing.T) {
	// The listener's Accept runs on the server's goroutine.
	var seenMu sync.Mutex
	var seen []unixPeer
	server := drainSocketTestServer(nil)
	socket := filepath.Join(claudeSocketTestDir(t), drainSocketName)
	listener, err := openPrivateDrainSocketListener(socket, func(peer unixPeer) bool {
		seenMu.Lock()
		defer seenMu.Unlock()
		seen = append(seen, peer)
		return false
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: server.DrainSocketHandler(nil), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })

	if code, body, err := drainSocketRequest(socket, http.MethodPost, "/_subrouter/drain"); err == nil {
		t.Fatalf("a refused peer got HTTP %d %s, want the connection closed", code, body)
	}
	if _, _, err := runDrainCommand(t, "drain", socket); err == nil || !strings.Contains(err.Error(), "refused this user") {
		t.Fatalf("drain by a refused peer = %v, want it told it was refused", err)
	}
	if server.Lifecycle.Draining() {
		t.Fatal("a refused peer drained the daemon")
	}
	switch runtime.GOOS {
	case "linux", "darwin":
		seenMu.Lock()
		first := append([]unixPeer(nil), seen...)
		seenMu.Unlock()
		if len(first) == 0 || first[0].UID != os.Getuid() || first[0].PID != os.Getpid() {
			t.Fatalf("peer credentials = %+v, want uid %d pid %d", first, os.Getuid(), os.Getpid())
		}
	}

	if !drainSocketPeerAllowed(unixPeer{UID: 0}) || !drainSocketPeerAllowed(unixPeer{UID: os.Geteuid()}) {
		t.Fatal("root or the daemon's own user refused")
	}
	if os.Geteuid() != 12345 && drainSocketPeerAllowed(unixPeer{UID: 12345}) {
		t.Fatal("another user admitted")
	}
}

// The Claude socket's rules (PATCHES §30, and its later path-length fix) hold for
// the drain socket: a dead socket left by a SIGKILL is replaced; a live one is
// never unlinked, whether a foreign process or another daemon holds it; a path
// too long to bind is refused before anything on disk is touched.
func TestDrainSocketKeepsTheClaudeSocketsStaleAndLeaseRules(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := drainSocketTestServer(nil).DrainSocketHandler(nil)
	answers := func(socket string) bool {
		_, _, err := drainSocketRequest(socket, http.MethodGet, "/_subrouter/drain-status")
		return err == nil
	}

	t.Run("a dead socket is replaced", func(t *testing.T) {
		socket := filepath.Join(claudeSocketTestDir(t), drainSocketName)
		dead, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		dead.SetUnlinkOnClose(false)
		_ = os.Chmod(socket, 0o600)
		_ = dead.Close()
		sock := serveDrainSocket(socket, handler, logger)
		if sock == nil || !answers(socket) {
			t.Fatal("a dead socket at the path was not replaced")
		}
		sock.close()
	})
	t.Run("a live foreign listener is left serving", func(t *testing.T) {
		socket := filepath.Join(claudeSocketTestDir(t), drainSocketName)
		foreign, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(socket, 0o600)
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
		if sock := serveDrainSocket(socket, handler, logger); sock != nil {
			sock.close()
			t.Fatal("served over a live foreign socket")
		}
		conn, err := net.Dial("unix", socket)
		if err != nil {
			t.Fatalf("the foreign listener lost its path: %v", err)
		}
		_ = conn.Close()
	})
	t.Run("a second daemon does not take a live drain socket", func(t *testing.T) {
		socket := filepath.Join(claudeSocketTestDir(t), drainSocketName)
		first := serveDrainSocket(socket, handler, logger)
		if first == nil {
			t.Fatal("first drain socket not served")
		}
		defer first.close()
		if second := serveDrainSocket(socket, handler, logger); second != nil {
			second.close()
			t.Fatal("a second drain socket was served at a live one's path")
		}
		if !answers(socket) {
			t.Fatal("the first drain socket stopped answering when a second tried its path")
		}
	})
	t.Run("a path too long is refused before anything is touched", func(t *testing.T) {
		dir := claudeSocketTestDir(t)
		limit := maxUnixSocketPath - len(".bind")
		socket := filepath.Join(dir, strings.Repeat("d", limit+1-len(dir)-1))
		var logged bytes.Buffer
		if sock := serveDrainSocket(socket, handler, slog.New(slog.NewTextHandler(&logged, nil))); sock != nil {
			sock.close()
			t.Fatalf("served a %d-byte path, over the %d-byte limit", len(socket), limit)
		}
		if !strings.Contains(logged.String(), "byte limit") {
			t.Fatalf("the refusal does not name the limit:\n%s", logged.String())
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Fatalf("the refused path left %v on disk", entries)
		}
	})
}

// The drain socket outlives the listeners: during the SIGTERM drain it still
// answers drain-status, which says shutting_down, and refuses a resume that
// would make a leaving process look ready.
//
// Watched red: with shutdownOnSignal back to lifecycle.Drain(), drain-status
// never says shutting_down and the resume is accepted.
func TestDrainSocketAnswersThroughTheSIGTERMDrain(t *testing.T) {
	server := drainSocketTestServer(nil)
	socket := filepath.Join(claudeSocketTestDir(t), drainSocketName)
	sock := serveDrainSocket(socket, server.DrainSocketHandler(drainSocketCaller), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if sock == nil {
		t.Fatal("drain socket not served")
	}
	t.Cleanup(sock.close)

	started := make(chan struct{})
	release := make(chan struct{})
	private, privateAddr, privateErr := startDrainTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-release
	}))
	go func() {
		if response, err := (&http.Client{Timeout: 30 * time.Second}).Get("http://" + privateAddr); err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
	}()
	<-started
	sigCh := make(chan os.Signal, 1)
	served := serveUntilSignalForTest(private, nil, server.Lifecycle, 30*time.Second, privateErr, sigCh)
	sigCh <- syscall.SIGTERM

	var status map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for {
		stdout, _, err := runDrainCommand(t, "drain-status", socket)
		if err != nil {
			t.Fatalf("drain-status during the SIGTERM drain: %v", err)
		}
		status = nil
		_ = json.Unmarshal([]byte(stdout), &status)
		if status["shutting_down"] == true {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("drain-status during the SIGTERM drain = %v, want shutting_down", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status["draining"] != true {
		t.Fatalf("drain-status during the SIGTERM drain = %v, want draining", status)
	}
	if _, _, err := runDrainCommand(t, "resume", socket); err == nil || !strings.Contains(err.Error(), "409") || !server.Lifecycle.Draining() {
		t.Fatalf("resume during the SIGTERM drain = %v (draining=%v), want HTTP 409 and still draining", err, server.Lifecycle.Draining())
	}
	close(release)
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not return after the stream finished")
	}
}

// With no --socket and no environment, the CLI finds the systemd service's
// socket from /etc/default/subrouter -- `sudo subrouter drain` on the box,
// where root has no ~/.subrouter -- and says what to do when it cannot.
func TestDrainCommandFindsTheServiceSocketAndExplainsFailures(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUBROUTER_STATE_DIR", "")
	t.Setenv(drainSocketEnv, "")
	state := claudeSocketTestDir(t)
	etc := t.TempDir()
	previous := systemdEtcDir
	systemdEtcDir = etc
	t.Cleanup(func() { systemdEtcDir = previous })
	if err := os.MkdirAll(filepath.Join(etc, "default"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(etc, "default", "subrouter"), []byte("SUBROUTER_ADDR=100.64.0.1:31415\nSUBROUTER_STATE_DIR="+state+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	server := drainSocketTestServer(nil)
	sock := serveDrainSocket(filepath.Join(state, drainSocketName), server.DrainSocketHandler(nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if sock == nil {
		t.Fatal("drain socket not served")
	}
	// A ~/.subrouter this user cannot look into -- `sudo -u subrouter` with the
	// operator's HOME -- does not shadow the service's socket.
	if err := os.Mkdir(filepath.Join(home, ".subrouter"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(home, ".subrouter"), 0o700) })
	if _, _, err := runDrainCommand(t, "drain", ""); err != nil || !server.Lifecycle.Draining() {
		t.Fatalf("drain with no --socket = %v (draining=%v), want the service's socket found through the env file", err, server.Lifecycle.Draining())
	}
	if err := os.Chmod(filepath.Join(home, ".subrouter"), 0o700); err != nil {
		t.Fatal(err)
	}

	if os.Geteuid() != 0 {
		// The socket is the service user's: anyone else is told to sudo.
		if err := os.Chmod(filepath.Join(state, drainSocketName), 0); err != nil {
			t.Fatal(err)
		}
		if _, _, err := runDrainCommand(t, "drain-status", ""); err == nil || !strings.Contains(err.Error(), "sudo") {
			t.Fatalf("drain-status on a socket this user cannot open = %v, want a hint to use sudo", err)
		}
	}

	sock.close()
	if _, _, err := runDrainCommand(t, "drain-status", ""); err == nil || !strings.Contains(err.Error(), "no drain socket at") ||
		!strings.Contains(err.Error(), filepath.Join(state, drainSocketName)) {
		t.Fatalf("drain-status with no socket anywhere = %v, want every place it looked", err)
	}
	t.Setenv(drainSocketEnv, "off")
	if _, _, err := runDrainCommand(t, "drain", ""); err == nil || !strings.Contains(err.Error(), "serves no drain socket") {
		t.Fatalf("drain with %s=off = %v, want it named", drainSocketEnv, err)
	}
}

// The listener refuses a peer by closing the connection before reading a byte,
// and which error the client then sees is a race between that close and its
// own write: a reset or EOF when it reads, EPIPE or (darwin) ENOTCONN when it
// writes, net/http's "server closed idle connection" when the close lands
// first. The CLI named only a reset and EOF, so 2 runs in 300 of the
// refused-peer test got the generic error instead. Here the request always
// goes out after the close, 200 times: which error each one meets is still up
// to the transport, and every one of them must read as the refusal. A socket
// that accepts and then says nothing is not a refusal: that is a timeout, and
// says so.
//
// Watched red: with the errno match back (ECONNRESET, EOF), "drain after the
// close = drain socket ...: write: broken pipe" in every run of 20; about 1
// attempt in 25 meets a broken pipe rather than EOF.
func TestDrainSocketCallsAConnectionClosedUnansweredARefusal(t *testing.T) {
	socket := filepath.Join(claudeSocketTestDir(t), drainSocketName)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	closed := make(chan struct{}, 1)
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	var hangs atomic.Bool
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if hangs.Load() {
				go func() { <-hang; _ = conn.Close() }()
				continue
			}
			_ = conn.Close()
			closed <- struct{}{}
		}
	}()
	previous := drainSocketDialed
	drainSocketDialed = func() { <-closed }
	t.Cleanup(func() { drainSocketDialed = previous })
	for i := 0; i < 200; i++ {
		if _, _, err := runDrainCommand(t, "drain", socket); err == nil || !strings.Contains(err.Error(), "refused this user") {
			t.Fatalf("attempt %d: drain after the close = %v, want it told it was refused", i, err)
		}
	}

	drainSocketDialed = func() {}
	hangs.Store(true)
	previousTimeout := drainCommandTimeout
	drainCommandTimeout = 200 * time.Millisecond
	t.Cleanup(func() { drainCommandTimeout = previousTimeout })
	if _, _, err := runDrainCommand(t, "drain-status", socket); err == nil || strings.Contains(err.Error(), "refused") || !strings.Contains(err.Error(), "Timeout") {
		t.Fatalf("drain-status on a socket that never answers = %v, want a timeout, not a refusal", err)
	}
}

// When the service's env file sets SUBROUTER_DRAIN_SOCKET, the service's
// socket is what it says: none for off or a relative path (the daemon refuses
// one), and that path otherwise. The CLI used to add install-systemd's default
// path after it regardless, so a stale drain.sock left there by a SIGKILL was
// picked and reported as "the daemon that left it there is not running" while
// the daemon ran with its socket turned off.
//
// Watched red: before the change, off and the relative path each reported the
// stale default socket, and the moved socket's error named the default.
func TestDrainCommandTakesTheServicesSettingOverTheDefaultPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUBROUTER_STATE_DIR", "")
	t.Setenv(drainSocketEnv, "")
	etc := t.TempDir()
	previousEtc, previousHome := systemdEtcDir, systemdServiceHome
	systemdEtcDir = etc
	systemdServiceHome = claudeSocketTestDir(t)
	t.Cleanup(func() { systemdEtcDir, systemdServiceHome = previousEtc, previousHome })
	envFile := filepath.Join(etc, "default", "subrouter")
	if err := os.MkdirAll(filepath.Dir(envFile), 0o755); err != nil {
		t.Fatal(err)
	}
	// What a SIGKILLed daemon leaves at install-systemd's default path.
	stale := filepath.Join(systemdServiceHome, drainSocketName)
	dead, err := net.ListenUnix("unix", &net.UnixAddr{Name: stale, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	dead.SetUnlinkOnClose(false)
	_ = dead.Close()

	moved := filepath.Join(claudeSocketTestDir(t), "moved.sock")
	for value, want := range map[string]string{
		"off":        "sets SUBROUTER_DRAIN_SOCKET=off: the daemon serves no drain socket",
		"None":       "sets SUBROUTER_DRAIN_SOCKET=None: the daemon serves no drain socket",
		"drain.sock": "sets SUBROUTER_DRAIN_SOCKET=drain.sock, which is not an absolute path: the daemon serves no drain socket",
		moved:        moved + ": no daemon runs here",
	} {
		if err := os.WriteFile(envFile, []byte("SUBROUTER_ADDR=100.64.0.1:31415\nSUBROUTER_DRAIN_SOCKET="+value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := runDrainCommand(t, "drain-status", "")
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "not running") || strings.Contains(err.Error(), stale) {
			t.Errorf("drain-status with the env file's %s=%s = %v, want %q and no word of the stale %s", drainSocketEnv, value, err, want, stale)
		}
		if value != moved && (err == nil || !strings.Contains(err.Error(), envFile)) {
			t.Errorf("%s=%s: %v does not name %s", drainSocketEnv, value, err, envFile)
		}
	}
}

// `sudo subrouter drain-status` runs as root against a socket in a directory
// the service user owns, so what answers there may be the service user's own
// listener rather than the daemon. What it sends is never written raw to
// root's terminal: a JSON answer is re-encoded, anything else is quoted, and
// an error body is quoted. And the socket must be a socket, not a symlink the
// service user could point at another listener.
//
// Watched red: before the change the escape sequences reached stdout and the
// error unchanged, and the symlink was followed.
func TestDrainCommandNeverPrintsTheAnswerRaw(t *testing.T) {
	dir := claudeSocketTestDir(t)
	socket := filepath.Join(dir, drainSocketName)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	// OSC 52 writes the terminal's clipboard; CSI (0x9b) and BEL follow.
	const hostile = "\x1b]52;c;aGVsbG8=\x07\u009b2J"
	answer := map[string]struct {
		code int
		body string
	}{
		"/_subrouter/drain-status": {http.StatusOK, `{"draining": true, "note": "` + "\u009b" + `31m", "\u001b": 1}`},
		"/_subrouter/drain":        {http.StatusOK, "draining" + hostile},
		"/_subrouter/resume":       {http.StatusConflict, "no" + hostile},
	}
	server := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := answer[r.URL.Path]
		w.WriteHeader(a.code)
		_, _ = io.WriteString(w, a.body)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	raw := func(s string) bool { return strings.ContainsAny(s, "\x1b\x07\u009b") }
	stdout, _, err := runDrainCommand(t, "drain-status", socket)
	if err != nil || raw(stdout) || !strings.Contains(stdout, `"draining": true`) {
		t.Errorf("drain-status = %v %q, want the JSON re-encoded with no raw control character", err, stdout)
	}
	stdout, _, err = runDrainCommand(t, "drain", socket)
	if err == nil || raw(stdout) || raw(err.Error()) || !strings.Contains(err.Error(), "not the daemon's JSON") {
		t.Errorf("drain answered with non-JSON = %v, stdout %q: want an error quoting it", err, stdout)
	}
	stdout, _, err = runDrainCommand(t, "resume", socket)
	if err == nil || raw(stdout) || raw(err.Error()) || !strings.Contains(err.Error(), "409") {
		t.Errorf("resume answered 409 = %v, stdout %q: want the body quoted", err, stdout)
	}

	link := filepath.Join(dir, "link.sock")
	if err := os.Symlink(socket, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runDrainCommand(t, "drain-status", link); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("drain-status through a symlink = %v, want it refused as not a socket", err)
	}
}
