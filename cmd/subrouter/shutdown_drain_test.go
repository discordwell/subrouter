package main

import (
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/proxy"
)

// startDrainTestServer serves handler on a fresh loopback listener and returns
// the server, its address, and the channel serve's own goroutine reports on.
func startDrainTestServer(t *testing.T, handler http.Handler) (*http.Server, string, chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()
	t.Cleanup(func() { _ = server.Close() })
	return server, listener.Addr().String(), errCh
}

// serveUntilSignalForTest runs the serving loop the way serve does, including
// serve's deferred Close of the public server once the loop returns: that
// Close was the whole public shutdown before PATCHES §38.
func serveUntilSignalForTest(private, public *http.Server, lifecycle *proxy.Lifecycle, timeout time.Duration, errCh chan error, sigCh chan os.Signal) chan error {
	served := make(chan error, 1)
	go func() {
		err := serveUntilSignal(private, public, lifecycle, timeout, nil, errCh, nil, sigCh)
		if public != nil {
			_ = public.Close()
		}
		served <- err
	}()
	return served
}

// PATCHES §38. On 09-28 the box exited 9 ms after SIGTERM: the private
// listener, idle, finished its graceful Shutdown at once, and serve's deferred
// Close then cut every stream in flight on the public listener -- the laptop's
// peer forwards. The public listener now drains with the private one: it stops
// taking connections, and serve does not return until its stream is done.
func TestSIGTERMDrainsAStreamInFlightOnThePublicListener(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	public, publicAddr, _ := startDrainTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "head;")
		w.(http.Flusher).Flush()
		close(started)
		<-release
		_, _ = io.WriteString(w, "tail")
	}))
	private, _, privateErr := startDrainTestServer(t, http.NotFoundHandler())

	type result struct {
		body string
		err  error
	}
	streamed := make(chan result, 1)
	go func() {
		response, err := (&http.Client{Timeout: 30 * time.Second}).Get("http://" + publicAddr)
		if err != nil {
			streamed <- result{err: err}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		streamed <- result{body: string(body), err: err}
	}()
	<-started

	sigCh := make(chan os.Signal, 1)
	served := serveUntilSignalForTest(private, public, proxy.NewLifecycle(), 30*time.Second, privateErr, sigCh)
	sigCh <- syscall.SIGTERM

	// The drain begins by closing the listener: a new public client is
	// refused rather than served by a process on its way out.
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", publicAddr, time.Second)
		if err != nil {
			break
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("the public listener still accepts connections after SIGTERM")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-served:
		t.Fatalf("serve returned (err=%v) with a public stream in flight; its deferred Close cuts that stream", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(release)
	select {
	case got := <-streamed:
		if got.err != nil || got.body != "head;tail" {
			t.Fatalf("public stream = %q, %v; want %q, whole", got.body, got.err, "head;tail")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the public stream never finished")
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve returned %v after a clean drain", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not return after the public stream finished")
	}
}

// Refusing turns is only for the wait AFTER the listeners have drained. While
// an HTTP stream still holds the process up, an established websocket keeps
// starting turns, as it always did: refused then, its client would reconnect
// to a listener already closed -- the laptop has no socket activation -- and
// lose the turn, where before §38 the old process simply served it.
func TestSIGTERMKeepsServingTurnsOnEstablishedSocketsWhileTheListenersDrain(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	private, privateAddr, privateErr := startDrainTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "head;")
		w.(http.Flusher).Flush()
		close(started)
		<-release
		_, _ = io.WriteString(w, "tail")
	}))
	streamed := make(chan error, 1)
	go func() {
		response, err := (&http.Client{Timeout: 30 * time.Second}).Get("http://" + privateAddr)
		if err == nil {
			_, err = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		streamed <- err
	}()
	<-started

	lifecycle := proxy.NewLifecycle()
	sigCh := make(chan os.Signal, 1)
	served := serveUntilSignalForTest(private, nil, lifecycle, 30*time.Second, privateErr, sigCh)
	sigCh <- syscall.SIGTERM
	deadline := time.Now().Add(10 * time.Second)
	for !lifecycle.Draining() {
		if time.Now().After(deadline) {
			t.Fatal("SIGTERM never drained the lifecycle")
		}
		time.Sleep(10 * time.Millisecond)
	}
	endTurn, admitted := lifecycle.TryBeginWebSocketTurn()
	if !admitted {
		t.Fatal("an established socket's turn was refused while an HTTP stream still held the process up; its client reconnects to a closed listener")
	}

	close(release)
	if err := <-streamed; err != nil {
		t.Fatalf("the HTTP stream in flight at SIGTERM was cut: %v", err)
	}
	// Once the listeners have drained, no new turn starts, and the one begun
	// during the drain is waited for.
	waitUntilWebSocketTurnsRefused(t, lifecycle)
	select {
	case err := <-served:
		t.Fatalf("serve returned (err=%v) with a websocket turn in flight; the exit would cut it", err)
	case <-time.After(300 * time.Millisecond):
	}
	endTurn()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not return after the last turn finished")
	}
}

// A websocket is a hijacked connection, which http.Server.Shutdown neither
// waits for nor closes, so SIGTERM waits on the lifecycle's count of turns in
// flight -- up to the same deadline, and no further.
func TestSIGTERMWaitsForAWebSocketTurnUpToTheDeadline(t *testing.T) {
	t.Run("the turn finishes", func(t *testing.T) {
		lifecycle := proxy.NewLifecycle()
		endTurn, admitted := lifecycle.TryBeginWebSocketTurn()
		if !admitted {
			t.Fatal("turn refused before any shutdown")
		}
		private, _, privateErr := startDrainTestServer(t, http.NotFoundHandler())
		sigCh := make(chan os.Signal, 1)
		served := serveUntilSignalForTest(private, nil, lifecycle, 30*time.Second, privateErr, sigCh)
		sigCh <- syscall.SIGTERM
		select {
		case err := <-served:
			t.Fatalf("serve returned (err=%v) with a websocket turn in flight; the exit would cut it", err)
		case <-time.After(300 * time.Millisecond):
		}
		// The idle listener drains at once, and from then on no turn starts.
		waitUntilWebSocketTurnsRefused(t, lifecycle)
		endTurn()
		select {
		case err := <-served:
			if err != nil {
				t.Fatalf("serve returned %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("serve did not return after the last turn finished")
		}
	})
	t.Run("the deadline passes first", func(t *testing.T) {
		lifecycle := proxy.NewLifecycle()
		if _, admitted := lifecycle.TryBeginWebSocketTurn(); !admitted {
			t.Fatal("turn refused before any shutdown")
		}
		private, _, privateErr := startDrainTestServer(t, http.NotFoundHandler())
		sigCh := make(chan os.Signal, 1)
		served := serveUntilSignalForTest(private, nil, lifecycle, 200*time.Millisecond, privateErr, sigCh)
		sigCh <- syscall.SIGTERM
		select {
		case <-served:
		case <-time.After(15 * time.Second):
			t.Fatal("serve waited past --shutdown-timeout for a turn that never finished")
		}
	})
}

// waitUntilWebSocketTurnsRefused probes until the lifecycle refuses a new
// turn, which it does from Lifecycle.Stop on. An admitted probe ends at once.
func waitUntilWebSocketTurnsRefused(t *testing.T, lifecycle *proxy.Lifecycle) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		endProbe, admitted := lifecycle.TryBeginWebSocketTurn()
		endProbe()
		if !admitted {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("new websocket turns were still admitted after the listeners drained, so a busy socket could hold the exit open")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Retirement (the supervisor's SIGUSR1) asks every client to reconnect onto
// the new generation. It covered only the private listener, so a peer holding
// a keep-alive connection to the public one stayed on the retired worker.
func TestRetireStopsConnectionReuseOnThePublicListenerToo(t *testing.T) {
	if retireSignal == nil {
		t.Skip("no retire signal on this platform")
	}
	public, publicAddr, _ := startDrainTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	private, _, privateErr := startDrainTestServer(t, http.NotFoundHandler())
	client := &http.Client{Timeout: 10 * time.Second}
	get := func() *http.Response {
		t.Helper()
		response, err := client.Get("http://" + publicAddr)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		return response
	}
	if get().Close {
		t.Fatal("the public listener asked the client to close before retirement")
	}

	sigCh := make(chan os.Signal, 1)
	served := serveUntilSignalForTest(private, public, nil, 10*time.Second, privateErr, sigCh)
	sigCh <- retireSignal
	deadline := time.Now().Add(10 * time.Second)
	for !get().Close {
		if time.Now().After(deadline) {
			t.Fatal("the retired worker's public listener kept connections alive, so its clients stay pinned to it")
		}
		time.Sleep(10 * time.Millisecond)
	}

	sigCh <- syscall.SIGTERM
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not return after SIGTERM")
	}
}
