package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/internal/tailnet"
	"github.com/discordwell/subrouter/selectacct"
	"github.com/discordwell/subrouter/session"
	"github.com/gorilla/websocket"
)

// PATCHES §38. A shutdown waits for the websocket TURNS in flight and for
// nothing else: a Codex socket stays open, idle, between turns, so waiting for
// open sockets would hold every restart to its full deadline. Once stopping, a
// turn that has not started is refused 1012 and never reaches upstream, so the
// client replays it on the next process instead of extending this one.
func TestStoppingWaitsForAWebSocketTurnInFlightAndNotForAnIdleSocket(t *testing.T) {
	var creates atomic.Int64
	release := make(chan struct{})
	upstreamURL := startTurnHoldingCodexUpstream(t, &creates, release)
	lifecycle := NewLifecycle()
	subrouter := newProxyTestServer(t, shutdownDrainTestServer(t, upstreamURL, lifecycle).Handler())
	defer subrouter.Close()

	busy := dialShutdownDrainSocket(t, subrouter.URL, "busy-session")
	defer busy.Close()
	idle := dialShutdownDrainSocket(t, subrouter.URL, "idle-session")
	defer idle.Close()

	if err := busy.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.6-sol"}`)); err != nil {
		t.Fatal(err)
	}
	_ = busy.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, body, err := busy.ReadMessage(); err != nil || !strings.Contains(string(body), "response.created") {
		t.Fatalf("turn did not start: body=%q err=%v", body, err)
	}
	if got := lifecycle.ActiveWebSocketTurns(); got != 1 {
		t.Fatalf("turns in flight = %d, want 1 (the busy socket's; the idle socket has none)", got)
	}

	lifecycle.Stop()
	waited := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		waited <- lifecycle.WaitForWebSocketTurns(ctx)
	}()
	select {
	case err := <-waited:
		t.Fatalf("shutdown stopped waiting (err=%v) with a turn still streaming; the process would exit and cut it", err)
	case <-time.After(300 * time.Millisecond):
	}

	// The idle socket's next turn is refused, never forwarded.
	if err := idle.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.6-sol"}`)); err != nil {
		t.Fatal(err)
	}
	_ = idle.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, body, err := idle.ReadMessage()
	if !websocket.IsCloseError(err, websocket.CloseServiceRestart) {
		t.Fatalf("turn started after Stop: body=%q err=%v; want a 1012 close so the client replays it on the next process", body, err)
	}

	close(release)
	if _, body, err := busy.ReadMessage(); err != nil || !strings.Contains(string(body), "response.completed") {
		t.Fatalf("the turn in flight at Stop did not finish: body=%q err=%v", body, err)
	}
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("wait ended with %v after the last turn finished", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("shutdown still waiting with %d turns counted after the last one finished", lifecycle.ActiveWebSocketTurns())
	}
	if got := creates.Load(); got != 1 {
		t.Fatalf("upstream saw %d response.create, want 1: the refused turn reached it", got)
	}
}

// A socket that dies mid-turn releases its turn. Otherwise one dropped client
// holds every later shutdown open until its deadline.
func TestAWebSocketThatDiesMidTurnReleasesItsTurn(t *testing.T) {
	var creates atomic.Int64
	release := make(chan struct{})
	defer close(release)
	upstreamURL := startTurnHoldingCodexUpstream(t, &creates, release)
	lifecycle := NewLifecycle()
	subrouter := newProxyTestServer(t, shutdownDrainTestServer(t, upstreamURL, lifecycle).Handler())
	defer subrouter.Close()

	conn := dialShutdownDrainSocket(t, subrouter.URL, "dropped-session")
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.6-sol"}`)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	if got := lifecycle.ActiveWebSocketTurns(); got != 1 {
		t.Fatalf("turns in flight = %d, want 1", got)
	}
	_ = conn.UnderlyingConn().Close() // the client vanishes mid-turn

	deadline := time.Now().Add(10 * time.Second)
	for lifecycle.ActiveWebSocketTurns() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("turns in flight = %d after the socket died, want 0", lifecycle.ActiveWebSocketTurns())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A message too large for the global inspect budget reaches the relay with no
// body, so its finishing event used to go unrecognised: the turn stayed
// counted while the socket idled, and every later shutdown waited out its
// deadline. The event is now told from the message's head.
func TestAFinishingEventTooLargeToInspectStillEndsItsTurn(t *testing.T) {
	var creates atomic.Int64
	release := make(chan struct{})
	upstreamURL := startTurnHoldingCodexUpstream(t, &creates, release)
	lifecycle := NewLifecycle()
	subrouter := newProxyTestServer(t, shutdownDrainTestServer(t, upstreamURL, lifecycle).Handler())
	defer subrouter.Close()

	conn := dialShutdownDrainSocket(t, subrouter.URL, "budget-session")
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.6-sol"}`)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	if got := lifecycle.ActiveWebSocketTurns(); got != 1 {
		t.Fatalf("turns in flight = %d, want 1", got)
	}

	exhaustWebSocketInspectBudget(t)
	close(release)
	if _, body, err := conn.ReadMessage(); err != nil || !strings.Contains(string(body), "response.completed") {
		t.Fatalf("the finishing event was not delivered: body=%q err=%v", body, err)
	}
	waitForWebSocketTurns(t, lifecycle, 0, "after an uninspected response.completed was delivered; the idle socket would hold a shutdown to its deadline")
}

// The other half: a response.create too large to inspect -- the image-heavy
// kind that spends the budget -- is still a turn. It is counted, and once the
// process is stopping it is refused like any other.
func TestAResponseCreateTooLargeToInspectIsStillCountedOrRefused(t *testing.T) {
	var creates atomic.Int64
	release := make(chan struct{})
	upstreamURL := startTurnHoldingCodexUpstream(t, &creates, release)
	lifecycle := NewLifecycle()
	subrouter := newProxyTestServer(t, shutdownDrainTestServer(t, upstreamURL, lifecycle).Handler())
	defer subrouter.Close()
	busy := dialShutdownDrainSocket(t, subrouter.URL, "busy-session")
	defer busy.Close()
	idle := dialShutdownDrainSocket(t, subrouter.URL, "idle-session")
	defer idle.Close()

	exhaustWebSocketInspectBudget(t)
	if err := busy.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.6-sol"}`)); err != nil {
		t.Fatal(err)
	}
	_ = busy.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, body, err := busy.ReadMessage(); err != nil || !strings.Contains(string(body), "response.created") {
		t.Fatalf("turn did not start: body=%q err=%v", body, err)
	}
	if got := lifecycle.ActiveWebSocketTurns(); got != 1 {
		t.Fatalf("turns in flight = %d, want 1: an uninspected response.create went uncounted, so a shutdown would not wait for it", got)
	}

	lifecycle.Stop()
	if err := idle.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.6-sol"}`)); err != nil {
		t.Fatal(err)
	}
	_ = idle.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, body, err := idle.ReadMessage(); !websocket.IsCloseError(err, websocket.CloseServiceRestart) {
		t.Fatalf("an uninspected turn started after Stop: body=%q err=%v; want a 1012 close", body, err)
	}

	close(release)
	if _, body, err := busy.ReadMessage(); err != nil || !strings.Contains(string(body), "response.completed") {
		t.Fatalf("the turn in flight at Stop did not finish: body=%q err=%v", body, err)
	}
	waitForWebSocketTurns(t, lifecycle, 0, "after the last turn finished")
	if got := creates.Load(); got != 1 {
		t.Fatalf("upstream saw %d response.create, want 1: the refused turn reached it", got)
	}
}

func TestWebSocketEventTypeReadsACutShortHead(t *testing.T) {
	for _, test := range []struct{ head, want string }{
		{`{"type":"response.completed","response":{"output":[{"text":"cut sh`, "response.completed"},
		{`{"sequence_number":7,"type":"response.done","response":{"id":"r`, "response.done"},
		{`{"response":{"output":[{"text":"cut before the ty`, ""},
		{`{"type":"response.comp`, ""},
		{`{"type":7}`, ""},
		{`["type","error"]`, ""},
		{``, ""},
	} {
		if got := webSocketEventType([]byte(test.head)); got != test.want {
			t.Errorf("webSocketEventType(%q) = %q, want %q", test.head, got, test.want)
		}
	}
}

// exhaustWebSocketInspectBudget takes every free byte of the global inspect
// budget until the test ends, so the messages relayed meanwhile reach the
// relay uninspected: the state several image-heavy sockets at once leave it in.
func exhaustWebSocketInspectBudget(t *testing.T) {
	t.Helper()
	budget := webSocketInspectBudget
	budget.mu.Lock()
	taken := budget.limit - budget.used
	budget.used = budget.limit
	budget.mu.Unlock()
	t.Cleanup(func() { budget.release(taken) })
}

func waitForWebSocketTurns(t *testing.T, lifecycle *Lifecycle, want int64, when string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for lifecycle.ActiveWebSocketTurns() != want {
		if time.Now().After(deadline) {
			t.Fatalf("turns in flight = %d %s, want %d", lifecycle.ActiveWebSocketTurns(), when, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startTurnHoldingCodexUpstream is a Codex websocket upstream that opens every
// turn with response.created and finishes it with response.completed only
// once release is closed.
func startTurnHoldingCodexUpstream(t *testing.T, creates *atomic.Int64, release <-chan struct{}) *url.URL {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if !strings.Contains(string(body), "response.create") {
				continue
			}
			creates.Add(1)
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created"}`)); err != nil {
				return
			}
			<-release
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}`)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(upstream.Close)
	parsed, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func shutdownDrainTestServer(t *testing.T, upstream *url.URL, lifecycle *Lifecycle) Server {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	return Server{
		Upstream: upstream,
		Accounts: []accounts.Account{{
			ID:       "a@example.com",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		Lifecycle:    lifecycle,
		MaxBodyBytes: 1024,
	}
}

func dialShutdownDrainSocket(t *testing.T, serverURL, sessionID string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(serverURL, "http") + "/v1/responses"
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"X-Subrouter-Session": []string{sessionID}})
	if err != nil {
		t.Fatalf("dial %s: %v", sessionID, err)
	}
	_ = response.Body.Close()
	return conn
}

// PATCHES §38. The box binds only its tailnet address, so nothing reaching it
// is a loopback peer and the loopback-only drain answered 403 there. Drain and
// its undo now take a CONFIGURED admin credential from the tailnet; nothing
// weaker, and never on the public listener.
func TestDrainAndResumeTakeTheAdminTokenFromTheTailnet(t *testing.T) {
	lifecycle := NewLifecycle()
	server := Server{
		Lifecycle:          lifecycle,
		AdminToken:         "admin-secret",
		AccountImportToken: "import-secret",
		LocalProxyToken:    "proxy-secret",
	}
	handler := server.Handler()
	tailnetPost := func(path, bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.RemoteAddr = "100.64.10.3:51234"
		req.Host = "100.64.10.1:31415"
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		return resp
	}

	for _, bearer := range []string{"", "proxy-secret", "import-secret"} {
		if resp := tailnetPost("/_subrouter/drain", bearer); resp.Code == http.StatusOK || lifecycle.Draining() {
			t.Fatalf("drain with bearer %q = %d (draining=%v), want a refusal", bearer, resp.Code, lifecycle.Draining())
		}
	}

	resp := tailnetPost("/_subrouter/drain", "admin-secret")
	if resp.Code != http.StatusOK || !lifecycle.Draining() {
		t.Fatalf("drain with the admin token from the tailnet = %d %s (draining=%v), want 200 and draining", resp.Code, resp.Body.String(), lifecycle.Draining())
	}
	var status map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if _, ok := status["active_websocket_turns"]; !ok {
		t.Fatalf("drain status %v lacks active_websocket_turns, the count a restart would wait for", status)
	}

	// Quiesce is not widened: it refuses sessions already running.
	if resp := tailnetPost("/_subrouter/quiesce", "admin-secret"); resp.Code != http.StatusForbidden || lifecycle.Quiesced() {
		t.Fatalf("quiesce from the tailnet = %d (quiesced=%v), want 403", resp.Code, lifecycle.Quiesced())
	}
	if resp := tailnetPost("/_subrouter/resume", "proxy-secret"); resp.Code == http.StatusOK || !lifecycle.Draining() {
		t.Fatalf("resume with the proxy token = %d (draining=%v), want a refusal", resp.Code, lifecycle.Draining())
	}
	if resp := tailnetPost("/_subrouter/resume", "admin-secret"); resp.Code != http.StatusOK || lifecycle.Draining() {
		t.Fatalf("resume with the admin token from the tailnet = %d %s (draining=%v), want 200 and not draining", resp.Code, resp.Body.String(), lifecycle.Draining())
	}

	// The public listener has no drain at all, whatever the caller holds.
	public := server.PublicHandler()
	req := httptest.NewRequest(http.MethodPost, "/_subrouter/drain", nil)
	req.RemoteAddr = "203.0.113.9:40000"
	req.Header.Set("Authorization", "Bearer admin-secret")
	publicResp := httptest.NewRecorder()
	public.ServeHTTP(publicResp, req)
	if publicResp.Code != http.StatusNotFound || lifecycle.Draining() {
		t.Fatalf("public drain = %d (draining=%v), want 404", publicResp.Code, lifecycle.Draining())
	}
}

// The tailnet identity --tailscale-auth admits is the other configured admin
// credential. The tokenless daemon is not: authorizeAdmin's legacy fallback
// lets every caller past requireAdmin there, and drain stays loopback-only.
func TestDrainTakesAnAdmittedTailnetIdentityButNotTheTokenlessFallback(t *testing.T) {
	post := func(server Server) (*httptest.ResponseRecorder, bool) {
		req := httptest.NewRequest(http.MethodPost, "/_subrouter/drain", nil)
		req.RemoteAddr = "100.82.214.112:52344"
		req.Host = "100.64.10.1:31415"
		resp := httptest.NewRecorder()
		server.Handler().ServeHTTP(resp, req)
		return resp, server.Lifecycle.Draining()
	}

	admitted := Server{Lifecycle: NewLifecycle(), TailnetAuth: &stubTailnetAuth{identity: tailnet.Identity{LoginName: "robert@example.com"}, allow: true}}
	if resp, draining := post(admitted); resp.Code != http.StatusOK || !draining {
		t.Fatalf("drain by an admitted tailnet identity = %d %s (draining=%v), want 200", resp.Code, resp.Body.String(), draining)
	}
	notAdmitted := Server{Lifecycle: NewLifecycle(), TailnetAuth: &stubTailnetAuth{allow: false}}
	if resp, draining := post(notAdmitted); resp.Code == http.StatusOK || draining {
		t.Fatalf("drain by a tailnet peer the authorizer refused = %d (draining=%v), want a refusal", resp.Code, draining)
	}
	tokenless := Server{Lifecycle: NewLifecycle()}
	if resp, draining := post(tokenless); resp.Code != http.StatusForbidden || draining {
		t.Fatalf("drain from the tailnet on a tokenless daemon = %d (draining=%v), want 403", resp.Code, draining)
	}
}

// Resume takes the admin credential from the tailnet so a remote drain can be
// undone remotely. Lifecycle.Resume also lifts a quiesce, which only loopback
// can set, so from anywhere else resume lifts the drain and refuses to touch a
// quiesce.
func TestResumeFromTheTailnetUndoesADrainButNotAQuiesce(t *testing.T) {
	lifecycle := NewLifecycle()
	handler := Server{Lifecycle: lifecycle, AdminToken: "admin-secret"}.Handler()
	post := func(path, remoteAddr, host string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.RemoteAddr = remoteAddr
		req.Host = host
		req.Header.Set("Authorization", "Bearer admin-secret")
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		return resp
	}
	loopback := func(path string) *httptest.ResponseRecorder {
		return post(path, "127.0.0.1:52000", "127.0.0.1:31415")
	}
	tailnet := func(path string) *httptest.ResponseRecorder {
		return post(path, "100.64.10.3:51234", "100.64.10.1:31415")
	}

	if resp := loopback("/_subrouter/quiesce"); resp.Code != http.StatusOK || !lifecycle.Quiesced() {
		t.Fatalf("quiesce from loopback = %d %s, want 200 and quiesced", resp.Code, resp.Body.String())
	}
	if resp := tailnet("/_subrouter/resume"); resp.Code != http.StatusForbidden || !lifecycle.Quiesced() || !lifecycle.Draining() {
		t.Fatalf("resume from the tailnet of a loopback quiesce = %d (quiesced=%v draining=%v), want 403 and both still set",
			resp.Code, lifecycle.Quiesced(), lifecycle.Draining())
	}
	if resp := loopback("/_subrouter/resume"); resp.Code != http.StatusOK || lifecycle.Quiesced() || lifecycle.Draining() {
		t.Fatalf("resume from loopback = %d (quiesced=%v draining=%v), want 200 and neither", resp.Code, lifecycle.Quiesced(), lifecycle.Draining())
	}
	if resp := tailnet("/_subrouter/drain"); resp.Code != http.StatusOK || !lifecycle.Draining() {
		t.Fatalf("drain from the tailnet = %d, want 200 and draining", resp.Code)
	}
	if resp := tailnet("/_subrouter/resume"); resp.Code != http.StatusOK || lifecycle.Draining() {
		t.Fatalf("resume of a plain drain from the tailnet = %d (draining=%v), want 200 and not draining", resp.Code, lifecycle.Draining())
	}
}
