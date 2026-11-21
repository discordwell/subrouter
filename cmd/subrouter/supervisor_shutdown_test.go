//go:build !windows

package main

import (
	"bufio"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func holdIdleKeepAliveOnEachDoor(t *testing.T, config supervisorConfig, socket string) {
	t.Helper()
	tcp := &http.Client{}
	unix := streamingUnixClient(socket)
	t.Cleanup(tcp.CloseIdleConnections)
	t.Cleanup(unix.CloseIdleConnections)
	for client, target := range map[*http.Client]string{
		tcp:  "http://" + config.Addr + "/",
		unix: "http://api.anthropic.com/",
	} {
		response, err := client.Get(target)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
}

func TestSupervisorSIGTERMDoesNotWaitOnIdleKeepAlives(t *testing.T) {
	if retireSignal == nil {
		t.Skip("worker retirement signal is unavailable")
	}
	dir := claudeSocketTestDir(t)
	socket := filepath.Join(dir, "claude.sock")
	t.Setenv(claudeSocketEnv, socket)
	config := testSupervisorConfig(t, dir)
	config.DrainTimeout = 30 * time.Second
	ts := startTestSupervisor(t, config)
	holdIdleKeepAliveOnEachDoor(t, config, socket)
	status := getSupervisorStatus(t, unixHTTPClient(config.ControlSocket))
	if connections := backendConnectionCount(status.Backends, status.Active.ID); connections != 2 {
		t.Fatalf("routed connections = %d, want the 2 idle keep-alives", connections)
	}

	ts.signals <- syscall.SIGTERM
	if err := ts.waitExit(t, 5*time.Second, "with only idle keep-alive connections (drain timeout 30s)"); err != nil {
		t.Fatalf("run = %v", err)
	}
}

func TestSupervisorSIGTERMLetsAnInFlightStreamFinish(t *testing.T) {
	if retireSignal == nil {
		t.Skip("worker retirement signal is unavailable")
	}
	dir := claudeSocketTestDir(t)
	socket := filepath.Join(dir, "claude.sock")
	t.Setenv(claudeSocketEnv, socket)
	config := testSupervisorConfig(t, dir)
	config.DrainTimeout = 30 * time.Second
	ts := startTestSupervisor(t, config)
	holdIdleKeepAliveOnEachDoor(t, config, socket)

	gate := filepath.Join(dir, "gate")
	streams := streamingUnixClient(socket)
	t.Cleanup(streams.CloseIdleConnections)
	stream, err := streams.Get("http://api.anthropic.com/stream?gate=" + url.QueryEscape(gate))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	streamReader := bufio.NewReader(stream.Body)
	if line, err := streamReader.ReadString('\n'); err != nil || line != "first\n" {
		t.Fatalf("stream opened with %q, %v", line, err)
	}

	ts.signals <- syscall.SIGTERM
	select {
	case <-ts.exited:
		t.Fatalf("supervisor exited with a stream in flight: %v", ts.err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(streamReader)
	if err != nil || string(rest) != "second\n" {
		t.Fatalf("stream in flight at SIGTERM ended with %q, %v; want it to finish", rest, err)
	}
	_ = stream.Body.Close()
	if err := ts.waitExit(t, 5*time.Second, "once its last stream finished"); err != nil {
		t.Fatalf("run = %v", err)
	}
}
