package proxy

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newProxyTestServer serves a subrouter handler for one test. Its cleanup
// closes the server, then waits up to 10 s more for hijacked handlers to
// return, before any cleanup registered earlier runs, t.TempDir()'s removal
// among them. httptest.Server.Close waits for requests, but a websocket is a
// hijacked connection, which it neither waits for nor closes, so a websocket's
// handler ran on after its test. What it
// wrote landed in a temp dir that cleanup was removing (every session-store
// call creates sessions.json.lock), and the test failed on "directory not
// empty" (CI, 2026-10-29, TestHandlerForcedCodexWebSocketServerErrorDoesNotDivertToAzure).
func newProxyTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	var running sync.WaitGroup
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		running.Add(1)
		defer running.Done()
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		server.Close()
		returned := make(chan struct{})
		go func() {
			running.Wait()
			close(returned)
		}()
		select {
		case <-returned:
		case <-time.After(10 * time.Second):
			t.Error("a handler was still running 10s after its test ended")
		}
	})
	return server
}

func TestProxyTestServerOutwaitsAHijackedHandler(t *testing.T) {
	var returned atomic.Bool
	t.Cleanup(func() {
		if !returned.Load() {
			t.Error("cleanup ran on while a hijacked handler was still running")
		}
	})
	hijacked := make(chan struct{})
	server := newProxyTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		close(hijacked)
		// Until the client hangs up, then a little past the test's end.
		_, _ = conn.Read(make([]byte, 1))
		time.Sleep(50 * time.Millisecond)
		returned.Store(true)
	}))
	client, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(client, "GET / HTTP/1.1\r\nHost: test\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-hijacked:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler never hijacked the connection")
	}
	_ = client.Close()
}
