package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// A Codex websocket takes no admission slot. Upstream #589 held one of the
// account's slots for the socket's whole life, but a Codex socket stays open,
// idle, between turns (PATCHES §33, §38), and this fork's placement neither
// reads free slots nor spills Codex: past the limit, the next session pinned
// to the account hung on its upgrade until some other socket closed. Here the
// gate admits one attempt per account and a second socket still connects
// while the first sits idle.
func TestIdleCodexWebSocketHoldsNoAdmissionSlot(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
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
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	server := codexEgressServer(t, upstreamURL, nil, 1)
	server.admission = testAdmissionControl(1, 1)
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()
	wsURL := "ws" + strings.TrimPrefix(proxy.URL, "http") + "/backend-api/codex/responses"

	dial := func(session string) (*websocket.Conn, error) {
		dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
		conn, response, err := dialer.Dial(wsURL, http.Header{"Session-Id": []string{session}})
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		return conn, err
	}
	first, err := dial("ws-idle-1")
	if err != nil {
		t.Fatalf("first socket: %v", err)
	}
	defer first.Close()
	second, err := dial("ws-idle-2")
	if err != nil {
		t.Fatalf("second socket on the same account while the first sits idle: %v", err)
	}
	defer second.Close()
	if snapshot := server.admission.snapshot(); len(snapshot.Accounts) != 0 {
		t.Fatalf("admission buckets = %+v; open sockets must hold no slot", snapshot.Accounts)
	}
}
