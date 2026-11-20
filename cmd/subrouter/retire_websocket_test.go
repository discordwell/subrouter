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

	"github.com/gorilla/websocket"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
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

	sigCh <- retireSignal
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, body, err := conn.ReadMessage(); !websocket.IsCloseError(err, websocket.CloseServiceRestart) || !strings.Contains(err.Error(), "reconnect") {
		t.Fatalf("after the retire signal the idle socket got body=%q err=%v; want a 1012 close", body, err)
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
