package proxy

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
	"github.com/gorilla/websocket"
)

// Codex opens every model turn as a websocket upgrade. Both websocket paths --
// the local upstream dial and the peer hop -- logged ONLY on dial failure, so a
// pool that was working produced an empty log while a broken one was loud.
// Once this machine held no local Codex accounts, that silence covered every
// Codex turn: nothing recorded which account spent the quota.
//
// These pin the success lines, and pin that `transport` still distinguishes the
// websocket turn from the HTTP turn logged under the same message.

func logsForWebSocketTurn(t *testing.T, logs *syncBuffer) {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(websocket.TextMessage, []byte("ok"))
		// Hold the turn open until the client hangs up, so the handler cannot
		// race past its own log line.
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
	server := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID:       "a@example.com",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "selected-token",
		}},
		Sessions:     newSessionStore(t),
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
		Logger:       slog.New(slog.NewTextHandler(logs, nil)),
	}

	front := httptest.NewServer(server.Handler())
	t.Cleanup(front.Close)

	conn, response, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(front.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatalf("dial through subrouter: %v", err)
	}
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	defer conn.Close()

	// Read one frame so the relay is provably live before we inspect the log.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read relayed frame: %v", err)
	}
}

// The regression: a successful websocket turn must produce a "proxy request"
// line naming the account, exactly as the HTTP path does.
func TestWebSocketTurnLogsProxyRequest(t *testing.T) {
	var logs syncBuffer
	logsForWebSocketTurn(t, &logs)

	out := logs.String()
	if !strings.Contains(out, `msg="proxy request"`) {
		t.Fatalf("successful websocket turn logged no \"proxy request\" line.\nlog:\n%s", out)
	}
	if !strings.Contains(out, "transport=websocket") {
		t.Fatalf("proxy request line lacks transport=websocket.\nlog:\n%s", out)
	}
	// Attribution is the whole point -- a line that does not name the account
	// cannot answer "which account served this turn".
	if !strings.Contains(out, "account=a@example.com") {
		t.Fatalf("proxy request line does not name the account.\nlog:\n%s", out)
	}
}

// The discriminator has to stay meaningful: if the HTTP path ever loses its
// transport field, `transport=websocket` stops distinguishing anything.
func TestHTTPTurnLogsProxyRequestWithHTTPTransport(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	server := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID:       "a@example.com",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "selected-token",
		}},
		Sessions:     newSessionStore(t),
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
		Logger:       slog.New(slog.NewTextHandler(&logs, nil)),
	}

	front := httptest.NewServer(server.Handler())
	defer front.Close()

	resp, err := http.Post(front.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	out := logs.String()
	if !strings.Contains(out, `msg="proxy request"`) {
		t.Fatalf("http turn logged no \"proxy request\" line.\nlog:\n%s", out)
	}
	if !strings.Contains(out, "transport=http") {
		t.Fatalf("http proxy request line lacks transport=http.\nlog:\n%s", out)
	}
}

// REGRESSION GUARD, not a proof of the fix above. The peer hop already logged
// its success line -- reverting the proxy.go change leaves this test green,
// which is exactly how it was discovered that the matching remote_pool.go edit
// was a DUPLICATE of an existing log rather than a missing one. It is kept
// because that line was otherwise untested, and because the pair of transports
// only stays greppable if both halves are pinned.
func TestRemotePoolWebSocketLogsSuccessfulHop(t *testing.T) {
	peer := peerWebSocketServer(t, nil, nil)
	pool := NewRemotePool(peer.URL, "peer-token", "")
	if pool == nil {
		t.Fatal("NewRemotePool returned nil")
	}
	var logs syncBuffer
	server := Server{
		RemotePool: pool,
		Sessions:   newSessionStore(t),
		Logger:     slog.New(slog.NewTextHandler(&logs, nil)),
	}

	conn, resp, err := dialThroughSubrouter(t, server, "/v1/responses")
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial through subrouter failed (status %d): %v", status, err)
	}
	defer conn.Close()

	// Prove the relay is live before inspecting the log.
	if err := conn.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read: %v", err)
	}

	out := logs.String()
	if !strings.Contains(out, `msg="serving from remote pool"`) {
		t.Fatalf("successful peer hop logged nothing.\nlog:\n%s", out)
	}
	if !strings.Contains(out, "transport=websocket") {
		t.Fatalf("peer hop line lacks transport=websocket.\nlog:\n%s", out)
	}
}
