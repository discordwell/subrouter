package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/front"
	"github.com/manaflow-ai/subrouter/internal/proxy"
)

func TestSupervisedWorkerSeesClaudeSocketClientsAsLoopback(t *testing.T) {
	dir := claudeSocketTestDir(t)
	socket := filepath.Join(dir, "claude.sock")
	t.Setenv(claudeSocketEnv, socket)
	config := testSupervisorConfig(t, dir)
	startTestSupervisor(t, config)

	if got := getSupervisorStatus(t, unixHTTPClient(config.ControlSocket)).ClaudeSocket; got != claudeSocketServing {
		t.Fatalf("supervisor-status claude_socket = %q, want %q", got, claudeSocketServing)
	}
	_, body := getThroughClaudeSocket(t, socket, "/_subrouter/test-peer")
	host, _, err := net.SplitHostPort(body)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		t.Fatalf("worker saw the socket client as %q, want a loopback peer", body)
	}
}

func TestSupervisedWorkerGuardsClaudeSocketRefusals(t *testing.T) {
	dir := claudeSocketTestDir(t)
	socket := filepath.Join(dir, "claude.sock")
	t.Setenv(claudeSocketEnv, socket)
	config := testSupervisorConfig(t, dir)
	startTestSupervisor(t, config)

	refused := func(client *http.Client, base string) *http.Response {
		t.Helper()
		defer client.CloseIdleConnections()
		request, _ := http.NewRequest(http.MethodPost, base+"/refuse", nil)
		request.Header.Set("Authorization", "Bearer sk-ant-oat01-adopted")
		request.Header.Set("X-Claude-Code-Session-Id", "sess-supervised")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		return response
	}
	tcp := &http.Client{Timeout: 5 * time.Second}
	if got := refused(tcp, "http://"+config.Addr); got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("TCP = %d, want the worker's 401", got.StatusCode)
	}
	got := refused(unixHTTPClient(socket), "http://api.anthropic.com")
	if got.StatusCode != http.StatusServiceUnavailable || got.Header.Get("X-Subrouter-Reason") != "claude_socket_refused" {
		t.Fatalf("socket = %d (X-Subrouter-Reason %q), want the guard's 503", got.StatusCode, got.Header.Get("X-Subrouter-Reason"))
	}

	response, err := tcp.Get("http://" + config.Addr + "/_subrouter/test-claude-socket-guard")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var status proxy.ClaudeSocketGuardStatus
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	want := proxy.ClaudeSocketGuardStatus{AdoptionCanaryRequests: 1, AdoptionCanarySessions: 1, RefusalsAnswered503: 1}
	if status != want {
		t.Fatalf("guard status = %+v, want %+v (the socket's request only)", status, want)
	}
}

// A supervisor outlives the workers it upgrades to, so every later worker must
// still know a socket connection by the header this release's supervisor
// writes for one.
func TestSupervisedWorkerGuardsTheSocketHeaderThisReleaseWrites(t *testing.T) {
	dir := claudeSocketTestDir(t)
	sock := newClaudeSocket("", claudeSocketEnvFor(map[string]string{
		claudeSocketEnv:        filepath.Join(dir, "claude.sock"),
		inheritedListenerFDEnv: "3",
	}))
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "refused", http.StatusUnauthorized)
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	sock.startGuarded(server, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	private := filepath.Join(dir, "worker.sock")
	listener, err := net.Listen("unix", private)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.Serve(front.NewProxyProtocolListener(listener)) }()

	for header, want := range map[string]int{
		"PROXY TCP4 127.0.0.1 127.0.0.1 0 0\r\n":         http.StatusServiceUnavailable,
		"PROXY TCP4 127.0.0.1 127.0.0.1 54321 31415\r\n": http.StatusUnauthorized,
	} {
		conn, err := net.Dial("unix", private)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(conn, header+"GET /v1/models HTTP/1.1\r\nHost: api.anthropic.com\r\nConnection: close\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		response, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("after %q: %v", strings.TrimSpace(header), err)
		}
		_ = response.Body.Close()
		_ = conn.Close()
		if response.StatusCode != want {
			t.Errorf("after %q the worker answered %d, want %d", strings.TrimSpace(header), response.StatusCode, want)
		}
	}
}

func TestSupervisorServesClaudeSocketAcrossAnUpgrade(t *testing.T) {
	dir := claudeSocketTestDir(t)
	socket := filepath.Join(dir, "claude.sock")
	t.Setenv(claudeSocketEnv, socket)
	config := testSupervisorConfig(t, dir)
	ts := startTestSupervisor(t, config)
	control := unixHTTPClient(config.ControlSocket)

	before := getSupervisorStatus(t, control)
	if before.ClaudeSocket != claudeSocketServing {
		t.Fatalf("supervisor-status claude_socket = %q, want %q", before.ClaudeSocket, claudeSocketServing)
	}
	first, _ := getThroughClaudeSocket(t, socket, "/")
	if pid := workerPID(t, first); pid != before.Worker.PID {
		t.Fatalf("socket request served by pid %d, want the active worker %d", pid, before.Worker.PID)
	}

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

	next := upgradeSupervisor(t, control)
	after := getSupervisorStatus(t, control)
	if next == before.Active.ID || after.Active.ID != next || after.Worker.PID == before.Worker.PID {
		t.Fatalf("upgrade did not switch generations: before %s (pid %d), after %s (pid %d)",
			before.Active.ID, before.Worker.PID, after.Active.ID, after.Worker.PID)
	}
	fresh, _ := getThroughClaudeSocket(t, socket, "/")
	if pid := workerPID(t, fresh); pid != after.Worker.PID {
		t.Fatalf("new socket connection served by pid %d, want the new generation %d", pid, after.Worker.PID)
	}

	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(streamReader)
	if err != nil || string(rest) != "second\n" {
		t.Fatalf("stream in flight across the upgrade ended with %q, %v; want it to finish", rest, err)
	}
	if pid := workerPID(t, stream); pid != before.Worker.PID {
		t.Fatalf("stream served by pid %d, want the generation it started on (%d)", pid, before.Worker.PID)
	}
	_ = stream.Body.Close()
	streams.CloseIdleConnections()
	deadline := time.Now().Add(5 * time.Second)
	for supervisorBackendPresent(getSupervisorStatus(t, control).Backends, before.Active.ID) {
		if time.Now().After(deadline) {
			t.Fatal("old generation was never reaped after its last socket stream closed")
		}
		time.Sleep(10 * time.Millisecond)
	}

	ts.fatal <- errors.New("stop")
	if err := ts.waitExit(t, 5*time.Second, "after a fatal error"); err == nil || err.Error() != "stop" {
		t.Fatalf("run = %v, want the fatal error", err)
	}
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Claude socket left behind after the supervisor exited: %v", err)
	}
}

func TestSupervisorClaudeSocketFailureLeavesTCPServing(t *testing.T) {
	dir := claudeSocketTestDir(t)
	t.Setenv(claudeSocketEnv, "relative/claude.sock")
	config := testSupervisorConfig(t, dir)
	ts := startTestSupervisor(t, config)

	if got := getSupervisorStatus(t, unixHTTPClient(config.ControlSocket)).ClaudeSocket; got != claudeSocketUnavailable {
		t.Fatalf("supervisor-status claude_socket = %q, want %q", got, claudeSocketUnavailable)
	}
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + config.Addr + "/")
	if err != nil {
		t.Fatalf("TCP stopped serving: %v", err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	client.CloseIdleConnections()

	ts.signals <- syscall.SIGTERM
	if err := ts.waitExit(t, 5*time.Second, "after SIGTERM"); err != nil {
		t.Fatalf("run = %v", err)
	}
}
