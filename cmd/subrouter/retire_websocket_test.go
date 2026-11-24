package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/internal/proxy"
	"github.com/discordwell/subrouter/selectacct"
	"github.com/discordwell/subrouter/session"
	"github.com/gorilla/websocket"
)

func TestRetireSignalClosesAnIdleCodexWebSocket1012(t *testing.T) {
	if retireSignal == nil {
		t.Skip("worker retirement signal is unavailable")
	}
	upgrader := websocket.Upgrader{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := proxy.NewLifecycle()
	handler := proxy.Server{
		Upstream:     upstreamURL,
		Accounts:     []accounts.Account{{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth, Token: "token"}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		Lifecycle:    lifecycle,
		MaxBodyBytes: 1024,
	}.Handler()
	private, privateAddr, privateErr := startDrainTestServer(t, handler)
	sigCh := make(chan os.Signal, 1)
	served := serveUntilSignalForTest(private, nil, lifecycle, 10*time.Second, privateErr, sigCh)

	conn, response, err := websocket.DefaultDialer.Dial("ws://"+privateAddr+"/v1/responses", http.Header{"X-Subrouter-Session": []string{"idle-session"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	defer conn.Close()

	// The dial returns at the 101, before the handler watches for the retire
	// signal, and a socket first watched after the signal owes its first turn
	// and gets no 1012. The relay answers a ping only once the watch is set,
	// as the proxy package's readSocketOnceRelayed relies on.
	pong := make(chan struct{}, 1)
	conn.SetPongHandler(func(string) error {
		select {
		case pong <- struct{}{}:
		default:
		}
		return nil
	})
	closed := make(chan error, 1)
	go func() {
		_, _, err := conn.ReadMessage()
		closed <- err
	}()
	if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pong:
	case <-time.After(10 * time.Second):
		t.Fatal("no pong within 10s: the daemon is not relaying the socket")
	}

	sigCh <- retireSignal
	select {
	case err := <-closed:
		if !websocket.IsCloseError(err, websocket.CloseServiceRestart) || !strings.Contains(err.Error(), "reconnect") {
			t.Fatalf("after the retire signal the idle socket got err=%v; want a 1012 close", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("after the retire signal the idle socket got nothing within 10s; want a 1012 close")
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
