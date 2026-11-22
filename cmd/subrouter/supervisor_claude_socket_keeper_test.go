//go:build !windows

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/front"
)

// PATCHES section 77.1. These tests drive a real supervisor with a real worker
// and touch the socket path the way the outside world can: delete it, hold its
// lock, put another listener there. The keeper looks every 250 ms without the
// lock and every 2 s with it. Each wait below needs a few looks, and allows
// far more because a loaded machine is slow.
const keeperTestPatience = 20 * time.Second

func waitForClaudeSocketWord(t *testing.T, control *http.Client, want string) supervisorControlStatus {
	t.Helper()
	deadline := time.Now().Add(keeperTestPatience)
	last := "no answer"
	for time.Now().Before(deadline) {
		status, err := fetchSupervisorStatus(control)
		if err == nil {
			if status.ClaudeSocket == want {
				return status
			}
			last = status.ClaudeSocket
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("supervisor-status claude_socket = %q after %s, want %q", last, keeperTestPatience, want)
	return supervisorControlStatus{}
}

// answerThroughClaudeSocket is one GET / through the path on a fresh
// connection: the body and, from the supervisor's worker, its pid.
func answerThroughClaudeSocket(socket string) (body string, pid string, err error) {
	client := unixClient(socket)
	defer client.CloseIdleConnections()
	response, err := client.Get("http://api.anthropic.com/")
	if err != nil {
		return "", "", err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	return string(raw), response.Header.Get("X-Fake-Worker-Pid"), err
}

func waitForWorkerThroughClaudeSocket(t *testing.T, socket string, control *http.Client) {
	t.Helper()
	deadline := time.Now().Add(keeperTestPatience)
	var last error
	for time.Now().Before(deadline) {
		_, pid, err := answerThroughClaudeSocket(socket)
		if err == nil && pid != "" {
			if want := fmt.Sprint(getSupervisorStatus(t, control).Worker.PID); pid != want {
				t.Fatalf("the Claude socket was answered by pid %s, want the active worker %s", pid, want)
			}
			return
		}
		last = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the supervisor's worker never answered through the Claude socket in %s: %v", keeperTestPatience, last)
}

// foreignSocket is another process's listener: bound beside the served path,
// private, answering "foreign" to every request. Closing it leaves its socket
// file behind, as a crash does.
type foreignSocket struct {
	path   string
	server *http.Server
}

func startForeignSocket(t *testing.T, path string) *foreignSocket {
	t.Helper()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "foreign")
	}), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	foreign := &foreignSocket{path: path, server: server}
	t.Cleanup(foreign.die)
	return foreign
}

func (f *foreignSocket) die() { _ = f.server.Close() }

func requireForeignAnswer(t *testing.T, socket, when string) {
	t.Helper()
	body, _, err := answerThroughClaudeSocket(socket)
	if err != nil || body != "foreign" {
		t.Fatalf("%s the path answered %q, %v; want the other process's listener to still have it", when, body, err)
	}
}

// watchClaudeSocketLock blocks on the socket's lock the way a process standing
// by for the path does: the kernel hands it the lock the moment the holder
// lets go. The returned func stops the watch and reports whether that
// happened.
func watchClaudeSocketLock(t *testing.T, socket string) (released func() bool) {
	t.Helper()
	fd, err := syscall.Open(socket+".lock", syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	var watching, got atomic.Bool
	watching.Store(true)
	go func() {
		defer syscall.Close(fd)
		for {
			err := syscall.Flock(fd, syscall.LOCK_EX)
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if err == nil && watching.Load() {
				got.Store(true)
			}
			return
		}
	}()
	return func() bool {
		watching.Store(false)
		return got.Load()
	}
}

func requireClaudeSocketLockFree(t *testing.T, socket, when string) {
	t.Helper()
	lease, err := acquireLocalDataSocketLease(socket)
	if err != nil {
		t.Fatalf("the Claude socket's lock is still held %s: %v", when, err)
	}
	_ = lease.Close()
}

// Before the keeper, a deleted socket path stayed deleted until the supervisor
// restarted: every new connection got ENOENT while the supervisor reported
// "serving".
func TestSupervisorRecreatesDeletedClaudeSocket(t *testing.T) {
	dir := claudeSocketTestDir(t)
	socket := filepath.Join(dir, "claude.sock")
	t.Setenv(claudeSocketEnv, socket)
	config := testSupervisorConfig(t, dir)
	startTestSupervisor(t, config)
	control := unixHTTPClient(config.ControlSocket)
	waitForWorkerThroughClaudeSocket(t, socket, control)

	gate := filepath.Join(dir, "gate")
	streams := streamingUnixClient(socket)
	defer streams.CloseIdleConnections()
	stream, err := streams.Get("http://api.anthropic.com/stream?gate=" + url.QueryEscape(gate))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	streamReader := bufio.NewReader(stream.Body)
	if line, err := streamReader.ReadString('\n'); err != nil || line != "first\n" {
		t.Fatalf("stream opened with %q, %v", line, err)
	}

	lockReleased := watchClaudeSocketLock(t, socket)
	if err := os.Remove(socket); err != nil {
		t.Fatal(err)
	}
	waitForWorkerThroughClaudeSocket(t, socket, control)
	if lockReleased() {
		t.Fatal("the supervisor let go of the socket's lock while it re-created the path: a process standing by took it")
	}
	if status := getSupervisorStatus(t, control); status.ClaudeSocket != claudeSocketServing || status.ClaudeSocketRebinds != 1 {
		t.Fatalf("after one re-created path: claude_socket = %q, claude_socket_rebinds = %d; want serving, 1",
			status.ClaudeSocket, status.ClaudeSocketRebinds)
	}
	if info, err := os.Lstat(socket); err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("re-created path = %v, %v; want a mode-0600 socket", info, err)
	}

	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if rest, err := io.ReadAll(streamReader); err != nil || string(rest) != "second\n" {
		t.Fatalf("the stream accepted before the path was deleted ended with %q, %v; want it to finish", rest, err)
	}
}

// A crash can leave a dead socket file at the path instead of nothing.
func TestSupervisorReplacesADeadSocketAtTheClaudeSocketPath(t *testing.T) {
	dir := claudeSocketTestDir(t)
	socket := filepath.Join(dir, "claude.sock")
	t.Setenv(claudeSocketEnv, socket)
	config := testSupervisorConfig(t, dir)
	startTestSupervisor(t, config)
	control := unixHTTPClient(config.ControlSocket)
	waitForWorkerThroughClaudeSocket(t, socket, control)

	dead := startForeignSocket(t, filepath.Join(dir, "dead.sock"))
	dead.die()
	if err := os.Rename(dead.path, socket); err != nil {
		t.Fatal(err)
	}
	waitForWorkerThroughClaudeSocket(t, socket, control)
	if got := getSupervisorStatus(t, control).ClaudeSocketRebinds; got != 1 {
		t.Fatalf("claude_socket_rebinds = %d, want 1", got)
	}
}

// Before the keeper, a supervisor that found the lock held when it started
// reported "unavailable" and never tried again, so whoever held the lock at
// that moment kept the daemon off its own socket until the next restart.
func TestSupervisorTakesClaudeSocketWhenLeaseFrees(t *testing.T) {
	dir := claudeSocketTestDir(t)
	socket := filepath.Join(dir, "claude.sock")
	t.Setenv(claudeSocketEnv, socket)
	lease, err := acquireLocalDataSocketLease(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	config := testSupervisorConfig(t, dir)
	startTestSupervisor(t, config)
	control := unixHTTPClient(config.ControlSocket)

	if got := getSupervisorStatus(t, control).ClaudeSocket; got != "waiting" {
		t.Fatalf("supervisor-status claude_socket = %q with the lock held elsewhere, want %q", got, "waiting")
	}
	tcp := &http.Client{Timeout: 10 * time.Second}
	defer tcp.CloseIdleConnections()
	response, err := tcp.Get("http://" + config.Addr + "/")
	if err != nil {
		t.Fatalf("TCP stopped serving while the supervisor waits for the socket: %v", err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the supervisor bound the path without the lock: %v", err)
	}

	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	waitForWorkerThroughClaudeSocket(t, socket, control)
	if status := getSupervisorStatus(t, control); status.ClaudeSocket != claudeSocketServing || status.ClaudeSocketRebinds != 0 {
		t.Fatalf("after the lock freed: claude_socket = %q, claude_socket_rebinds = %d; want serving, 0",
			status.ClaudeSocket, status.ClaudeSocketRebinds)
	}
}

// The lock free and a live listener at the path: the supervisor waits for the
// listener too, and takes the path only once nothing answers there.
func TestSupervisorWaitsWhileAnotherProcessListensAtTheClaudeSocket(t *testing.T) {
	dir := claudeSocketTestDir(t)
	socket := filepath.Join(dir, "claude.sock")
	t.Setenv(claudeSocketEnv, socket)
	foreign := startForeignSocket(t, socket)
	config := testSupervisorConfig(t, dir)
	startTestSupervisor(t, config)
	control := unixHTTPClient(config.ControlSocket)

	if got := getSupervisorStatus(t, control).ClaudeSocket; got != "waiting" {
		t.Fatalf("supervisor-status claude_socket = %q with another listener at the path, want %q", got, "waiting")
	}
	time.Sleep(4 * claudeSocketLockPoll)
	requireForeignAnswer(t, socket, "after four more looks")

	foreign.die()
	waitForWorkerThroughClaudeSocket(t, socket, control)
}

func TestSupervisorDoesNotStealALiveForeignSocket(t *testing.T) {
	dir := claudeSocketTestDir(t)
	socket := filepath.Join(dir, "claude.sock")
	t.Setenv(claudeSocketEnv, socket)
	config := testSupervisorConfig(t, dir)
	startTestSupervisor(t, config)
	control := unixHTTPClient(config.ControlSocket)
	waitForWorkerThroughClaudeSocket(t, socket, control)

	// Renamed over the supervisor's socket, so the path is never empty and the
	// supervisor has no moment to re-create it first.
	foreign := startForeignSocket(t, filepath.Join(dir, "foreign.sock"))
	if err := os.Rename(foreign.path, socket); err != nil {
		t.Fatal(err)
	}
	theirs, err := os.Lstat(socket)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(2*claudeSocketPathCheck + claudeSocketPathCheck/2)
	if now, err := os.Lstat(socket); err != nil || !os.SameFile(theirs, now) {
		t.Fatalf("the other process's socket was replaced at the path: %v", err)
	}
	requireForeignAnswer(t, socket, "two path checks later")
	if got := getSupervisorStatus(t, control).ClaudeSocket; got != "displaced" {
		t.Fatalf("supervisor-status claude_socket = %q while another process answers at the path, want %q", got, "displaced")
	}

	foreign.die()
	waitForWorkerThroughClaudeSocket(t, socket, control)
	if status := getSupervisorStatus(t, control); status.ClaudeSocket != claudeSocketServing || status.ClaudeSocketRebinds != 1 {
		t.Fatalf("after the other listener died: claude_socket = %q, claude_socket_rebinds = %d; want serving, 1",
			status.ClaudeSocket, status.ClaudeSocketRebinds)
	}
}

// What the opener refuses to remove at start, the keeper refuses to remove
// later: only a private socket of this user is ever unlinked. The supervisor
// keeps trying, so removing the entry is enough to get the socket back.
func TestSupervisorLeavesAnUnsafeEntryAtTheClaudeSocketPathAlone(t *testing.T) {
	dir := claudeSocketTestDir(t)
	socket := filepath.Join(dir, "claude.sock")
	t.Setenv(claudeSocketEnv, socket)
	config := testSupervisorConfig(t, dir)
	startTestSupervisor(t, config)
	control := unixHTTPClient(config.ControlSocket)
	waitForWorkerThroughClaudeSocket(t, socket, control)

	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(file, socket); err != nil {
		t.Fatal(err)
	}
	waitForClaudeSocketWord(t, control, claudeSocketUnavailable)
	time.Sleep(claudeSocketPathCheck + claudeSocketPathCheck/2)
	if body, err := os.ReadFile(socket); err != nil || string(body) != "keep" {
		t.Fatalf("the regular file at the path was touched: %q, %v", body, err)
	}

	if err := os.Remove(socket); err != nil {
		t.Fatal(err)
	}
	waitForWorkerThroughClaudeSocket(t, socket, control)
}

// A lock is a file in the socket's folder. Once that folder is moved away and
// a new one made at its path, the lock the supervisor holds guards a socket no
// client can dial, and another process can take the new folder's lock.
func TestSupervisorTakesTheClaudeSocketAgainWhenItsFolderIsReplaced(t *testing.T) {
	dir := claudeSocketTestDir(t)
	folder := filepath.Join(dir, "state")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(folder, "claude.sock")
	t.Setenv(claudeSocketEnv, socket)
	config := testSupervisorConfig(t, dir)
	startTestSupervisor(t, config)
	control := unixHTTPClient(config.ControlSocket)
	waitForWorkerThroughClaudeSocket(t, socket, control)

	moved := filepath.Join(dir, "moved")
	if err := os.Rename(folder, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	waitForWorkerThroughClaudeSocket(t, socket, control)
	if _, err := os.Lstat(filepath.Join(moved, "claude.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the socket in the folder that was moved away was left behind: %v", err)
	}
	requireClaudeSocketLockFree(t, filepath.Join(moved, "claude.sock"), "in the folder that was moved away")
}

// A process that stands in on the socket path must know the supervisor will
// take the path back, and reads these two fields to find out. A supervisor
// with no Claude socket reports none of them.
func TestSupervisorStatusSaysItWaitsForTheClaudeSocketLock(t *testing.T) {
	rawStatus := func(t *testing.T, config supervisorConfig) map[string]any {
		t.Helper()
		response, err := unixHTTPClient(config.ControlSocket).Get("http://supervisor/_subrouter/supervisor-status")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var status map[string]any
		if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
			t.Fatal(err)
		}
		return status
	}
	t.Run("with a Claude socket", func(t *testing.T) {
		dir := claudeSocketTestDir(t)
		t.Setenv(claudeSocketEnv, filepath.Join(dir, "claude.sock"))
		config := testSupervisorConfig(t, dir)
		startTestSupervisor(t, config)
		status := rawStatus(t, config)
		if got := status["claude_socket_waits_for_lock"]; got != true {
			t.Fatalf("claude_socket_waits_for_lock = %v, want true", got)
		}
		if got := status["claude_socket_rebinds"]; got != float64(0) {
			t.Fatalf("claude_socket_rebinds = %v, want 0", got)
		}
	})
	t.Run("without one", func(t *testing.T) {
		dir := claudeSocketTestDir(t)
		t.Setenv(claudeSocketEnv, "")
		config := testSupervisorConfig(t, dir)
		startTestSupervisor(t, config)
		status := rawStatus(t, config)
		for _, key := range []string{"claude_socket", "claude_socket_waits_for_lock", "claude_socket_rebinds"} {
			if got, ok := status[key]; ok {
				t.Fatalf("a supervisor with no Claude socket reports %s = %v", key, got)
			}
		}
	})
}

func TestSupervisorShutsDownCleanlyWhileWaitingForTheClaudeSocket(t *testing.T) {
	dir := claudeSocketTestDir(t)
	socket := filepath.Join(dir, "claude.sock")
	t.Setenv(claudeSocketEnv, socket)
	lease, err := acquireLocalDataSocketLease(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	config := testSupervisorConfig(t, dir)
	ts := startTestSupervisor(t, config)
	waitForClaudeSocketWord(t, unixHTTPClient(config.ControlSocket), "waiting")

	ts.signals <- syscall.SIGTERM
	if err := ts.waitExit(t, keeperTestPatience, "after SIGTERM while it waited for the socket's lock"); err != nil {
		t.Fatalf("run = %v", err)
	}
	// Nobody is left asking: the freed lock stays free and the path stays empty.
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * claudeSocketLockPoll)
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("something bound the path after the supervisor exited: %v", err)
	}
	requireClaudeSocketLockFree(t, socket, "after the supervisor exited")
}

func TestSupervisorShutsDownCleanlyWhileDisplacedFromTheClaudeSocket(t *testing.T) {
	dir := claudeSocketTestDir(t)
	socket := filepath.Join(dir, "claude.sock")
	t.Setenv(claudeSocketEnv, socket)
	config := testSupervisorConfig(t, dir)
	ts := startTestSupervisor(t, config)
	control := unixHTTPClient(config.ControlSocket)
	waitForWorkerThroughClaudeSocket(t, socket, control)
	foreign := startForeignSocket(t, filepath.Join(dir, "foreign.sock"))
	if err := os.Rename(foreign.path, socket); err != nil {
		t.Fatal(err)
	}
	waitForClaudeSocketWord(t, control, "displaced")

	ts.signals <- syscall.SIGTERM
	if err := ts.waitExit(t, keeperTestPatience, "after SIGTERM while another process had its path"); err != nil {
		t.Fatalf("run = %v", err)
	}
	requireForeignAnswer(t, socket, "after the supervisor exited")
	requireClaudeSocketLockFree(t, socket, "after the supervisor exited")
	// Nobody is left checking: the dead socket the other process leaves stays.
	theirs, err := os.Lstat(socket)
	if err != nil {
		t.Fatal(err)
	}
	foreign.die()
	time.Sleep(claudeSocketPathCheck + claudeSocketPathCheck/2)
	if now, err := os.Lstat(socket); err != nil || !os.SameFile(theirs, now) {
		t.Fatalf("something replaced the socket at the path after the supervisor exited: %v", err)
	}
}

// failingLeasedListener is the real socket with an accept(2) that fails first:
// each of failures in turn, then real connections.
type failingLeasedListener struct {
	leasedListener
	mu       sync.Mutex
	failures []error
}

func (l *failingLeasedListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if len(l.failures) > 0 {
		err := l.failures[0]
		l.failures = l.failures[1:]
		l.mu.Unlock()
		return nil, err
	}
	l.mu.Unlock()
	return l.leasedListener.Accept()
}

// startRoutedClaudeSocket serves the socket as the supervisor does, through a
// router to one worker that answers "worker", with accept(2) failing first.
func startRoutedClaudeSocket(t *testing.T, socketPath string, failures ...syscall.Errno) *claudeSocket {
	t.Helper()
	worker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	workerServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "worker")
	}), ReadHeaderTimeout: 10 * time.Second}
	t.Cleanup(func() { _ = workerServer.Close() })
	go func() { _ = workerServer.Serve(front.NewProxyProtocolListener(worker)) }()
	router, err := front.NewRouter(front.Backend{ID: "worker", Address: worker.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}

	sock := newClaudeSocket("", claudeSocketEnvFor(map[string]string{claudeSocketEnv: socketPath}))
	sock.open = func(path string) (leasedListener, error) {
		real, err := openLeasedClaudeSocket(path)
		if err != nil {
			return nil, err
		}
		failing := &failingLeasedListener{leasedListener: real}
		for _, errno := range failures {
			failing.failures = append(failing.failures, &net.OpError{Op: "accept", Net: "unix", Err: os.NewSyscallError("accept", errno)})
		}
		return failing, nil
	}
	sock.start(router.Serve, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(sock.close)
	return sock
}

// Before this the supervisor's accept loop ended on the first EMFILE. The
// socket then read "stopped" while its path stayed in place, queueing
// connections nobody would ever accept.
func TestClaudeSocketKeepsAcceptingAfterTemporaryError(t *testing.T) {
	socketPath := filepath.Join(claudeSocketTestDir(t), "claude.sock")
	sock := startRoutedClaudeSocket(t, socketPath, syscall.EMFILE, syscall.ENFILE)

	body, _, err := answerThroughClaudeSocket(socketPath)
	if err != nil || body != "worker" {
		t.Fatalf("after two temporary accept errors the socket answered %q, %v (status %q); want the worker's answer",
			body, err, sock.Status())
	}
	if got := sock.Status(); got != claudeSocketServing {
		t.Fatalf("status = %q after two temporary accept errors, want %q", got, claudeSocketServing)
	}
}

// An accept loop that ends for good used to keep the path and the lock under
// the supervisor: clients queued on a socket nobody accepted, and no other
// process could take the path over.
func TestClaudeSocketGivesUpThePathWhenItStopsServing(t *testing.T) {
	socketPath := filepath.Join(claudeSocketTestDir(t), "claude.sock")
	sock := startRoutedClaudeSocket(t, socketPath, syscall.EBADF)

	deadline := time.Now().Add(keeperTestPatience)
	for sock.Status() != claudeSocketStopped {
		if time.Now().After(deadline) {
			t.Fatalf("status = %q after an accept error that is not temporary, want %q", sock.Status(), claudeSocketStopped)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a socket nobody accepts on was left at the path: %v", err)
	}
	requireClaudeSocketLockFree(t, socketPath, "after the socket stopped serving")
}
