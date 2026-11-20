package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const testResponseCreate = `{"type":"response.create","model":"gpt-5.6-sol"}`

type socketEvent struct {
	body string
	err  error
}

// readSocket relays every message conn receives, then the error that ended
// it, so a test can assert that nothing arrived without breaking the conn
// with a read deadline.
func readSocket(conn *websocket.Conn) <-chan socketEvent {
	events := make(chan socketEvent, 16)
	go func() {
		defer close(events)
		for {
			_, body, err := conn.ReadMessage()
			events <- socketEvent{body: string(body), err: err}
			if err != nil {
				return
			}
		}
	}()
	return events
}

func nextSocketEvent(t *testing.T, events <-chan socketEvent, what string) socketEvent {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(10 * time.Second):
		t.Fatalf("no %s within 10s", what)
		return socketEvent{}
	}
}

func expectServiceRestart(t *testing.T, events <-chan socketEvent, when string) {
	t.Helper()
	event := nextSocketEvent(t, events, "1012 close "+when)
	if !websocket.IsCloseError(event.err, websocket.CloseServiceRestart) {
		t.Fatalf("%s the client got body=%q err=%v; want a 1012 close so Codex reconnects onto the active generation", when, event.body, event.err)
	}
}

func expectNothing(t *testing.T, events <-chan socketEvent, d time.Duration, why string) {
	t.Helper()
	select {
	case event := <-events:
		t.Fatalf("%s, but the client got body=%q err=%v", why, event.body, event.err)
	case <-time.After(d):
	}
}

func runTurn(t *testing.T, conn *websocket.Conn, events <-chan socketEvent) {
	t.Helper()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(testResponseCreate)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"response.created", "response.completed"} {
		if event := nextSocketEvent(t, events, want); event.err != nil || !strings.Contains(event.body, want) {
			t.Fatalf("turn: got body=%q err=%v, want %s", event.body, event.err, want)
		}
	}
}

func TestRetiringClosesAnIdleCodexWebSocketWith1012(t *testing.T) {
	var creates atomic.Int64
	release := make(chan struct{})
	close(release)
	upstreamURL := startTurnHoldingCodexUpstream(t, &creates, release)
	lifecycle := NewLifecycle()
	subrouter := newProxyTestServer(t, shutdownDrainTestServer(t, upstreamURL, lifecycle).Handler())
	defer subrouter.Close()

	fresh := dialShutdownDrainSocket(t, subrouter.URL, "fresh-session")
	defer fresh.Close()
	freshEvents := readSocket(fresh)
	between := dialShutdownDrainSocket(t, subrouter.URL, "between-turns-session")
	defer between.Close()
	betweenEvents := readSocket(between)
	runTurn(t, between, betweenEvents)

	lifecycle.Retire()
	expectServiceRestart(t, freshEvents, "on an idle socket that never ran a turn, after Retire,")
	expectServiceRestart(t, betweenEvents, "on an idle socket between turns, after Retire,")
	waitForWebSocketTurns(t, lifecycle, 0, "after both idle sockets closed")
}

func TestRetiringLetsTheTurnInFlightFinishThenCloses1012(t *testing.T) {
	var creates atomic.Int64
	release := make(chan struct{})
	upstreamURL := startTurnHoldingCodexUpstream(t, &creates, release)
	lifecycle := NewLifecycle()
	subrouter := newProxyTestServer(t, shutdownDrainTestServer(t, upstreamURL, lifecycle).Handler())
	defer subrouter.Close()

	busy := dialShutdownDrainSocket(t, subrouter.URL, "busy-session")
	defer busy.Close()
	events := readSocket(busy)
	if err := busy.WriteMessage(websocket.TextMessage, []byte(testResponseCreate)); err != nil {
		t.Fatal(err)
	}
	if event := nextSocketEvent(t, events, "response.created"); event.err != nil || !strings.Contains(event.body, "response.created") {
		t.Fatalf("turn did not start: body=%q err=%v", event.body, event.err)
	}

	lifecycle.Retire()
	expectNothing(t, events, 300*time.Millisecond, "a turn in flight at Retire must run to its end")

	close(release)
	if event := nextSocketEvent(t, events, "response.completed"); event.err != nil || !strings.Contains(event.body, "response.completed") {
		t.Fatalf("the turn in flight at Retire did not finish: body=%q err=%v", event.body, event.err)
	}
	expectServiceRestart(t, events, "right after the turn in flight finished")
	waitForWebSocketTurns(t, lifecycle, 0, "after the retired socket's last turn")
	if got := creates.Load(); got != 1 {
		t.Fatalf("upstream saw %d response.create, want 1", got)
	}
}

func TestATurnQueuedBehindOneInFlightAtRetireIsServed(t *testing.T) {
	var creates atomic.Int64
	release := make(chan struct{})
	upstreamURL := startTurnHoldingCodexUpstream(t, &creates, release)
	lifecycle := NewLifecycle()
	subrouter := newProxyTestServer(t, shutdownDrainTestServer(t, upstreamURL, lifecycle).Handler())
	defer subrouter.Close()

	busy := dialShutdownDrainSocket(t, subrouter.URL, "busy-session")
	defer busy.Close()
	events := readSocket(busy)
	if err := busy.WriteMessage(websocket.TextMessage, []byte(testResponseCreate)); err != nil {
		t.Fatal(err)
	}
	if event := nextSocketEvent(t, events, "response.created"); event.err != nil {
		t.Fatalf("turn did not start: %v", event.err)
	}
	lifecycle.Retire()
	if err := busy.WriteMessage(websocket.TextMessage, []byte(testResponseCreate)); err != nil {
		t.Fatal(err)
	}
	waitForWebSocketTurns(t, lifecycle, 2, "with a second turn sent behind the first after Retire")

	close(release)
	for _, want := range []string{"response.completed", "response.created", "response.completed"} {
		if event := nextSocketEvent(t, events, want); event.err != nil || !strings.Contains(event.body, want) {
			t.Fatalf("got body=%q err=%v, want %s", event.body, event.err, want)
		}
	}
	expectServiceRestart(t, events, "after the queued turn finished")
	if got := creates.Load(); got != 2 {
		t.Fatalf("upstream saw %d response.create, want 2", got)
	}
}

func TestATurnRacingRetireIsServedWholeOrNeverForwarded(t *testing.T) {
	var creates atomic.Int64
	release := make(chan struct{})
	close(release)
	upstreamURL := startTurnHoldingCodexUpstream(t, &creates, release)
	lifecycle := NewLifecycle()
	subrouter := newProxyTestServer(t, shutdownDrainTestServer(t, upstreamURL, lifecycle).Handler())
	defer subrouter.Close()

	const sockets = 24
	conns := make([]*websocket.Conn, sockets)
	streams := make([]<-chan socketEvent, sockets)
	for i := range conns {
		conns[i] = dialShutdownDrainSocket(t, subrouter.URL, fmt.Sprintf("racing-session-%d", i))
		defer conns[i].Close()
		streams[i] = readSocket(conns[i])
	}
	var sent sync.WaitGroup
	for _, conn := range conns {
		sent.Add(1)
		go func() {
			defer sent.Done()
			_ = conn.WriteMessage(websocket.TextMessage, []byte(testResponseCreate))
		}()
	}
	lifecycle.Retire()
	sent.Wait()

	served, redirected := 0, 0
	for i, events := range streams {
		var bodies []string
		for {
			event := nextSocketEvent(t, events, "the end of a racing socket")
			if event.err != nil {
				if !websocket.IsCloseError(event.err, websocket.CloseServiceRestart) {
					t.Fatalf("socket %d ended with %v after %q, want a 1012 close", i, event.err, bodies)
				}
				break
			}
			bodies = append(bodies, event.body)
		}
		switch {
		case len(bodies) == 0:
			redirected++
		case len(bodies) == 2 && strings.Contains(bodies[0], "response.created") && strings.Contains(bodies[1], "response.completed"):
			served++
		default:
			t.Fatalf("socket %d got %q before its 1012: a turn was cut", i, bodies)
		}
	}
	if got := creates.Load(); got != int64(served) {
		t.Fatalf("upstream saw %d response.create but %d turns reached their client whole (%d redirected)", got, served, redirected)
	}
	waitForWebSocketTurns(t, lifecycle, 0, "after every racing socket closed")
	t.Logf("%d turns served whole, %d redirected before reaching upstream", served, redirected)
}

func TestANonRetiredGenerationKeepsItsIdleWebSocket(t *testing.T) {
	var creates atomic.Int64
	release := make(chan struct{})
	close(release)
	upstreamURL := startTurnHoldingCodexUpstream(t, &creates, release)
	active := NewLifecycle()
	activeServer := newProxyTestServer(t, shutdownDrainTestServer(t, upstreamURL, active).Handler())
	defer activeServer.Close()
	retiring := NewLifecycle()
	retiringServer := newProxyTestServer(t, shutdownDrainTestServer(t, upstreamURL, retiring).Handler())
	defer retiringServer.Close()

	kept := dialShutdownDrainSocket(t, activeServer.URL, "active-session")
	defer kept.Close()
	keptEvents := readSocket(kept)
	moved := dialShutdownDrainSocket(t, retiringServer.URL, "retiring-session")
	defer moved.Close()
	movedEvents := readSocket(moved)

	retiring.Retire()
	expectServiceRestart(t, movedEvents, "on the retired generation's idle socket")
	expectNothing(t, keptEvents, 300*time.Millisecond, "the active generation's idle socket must stay open")
	runTurn(t, kept, keptEvents)
}

func TestASocketOpenedAfterRetireServesItsFirstTurnThenCloses1012(t *testing.T) {
	var creates atomic.Int64
	release := make(chan struct{})
	close(release)
	upstreamURL := startTurnHoldingCodexUpstream(t, &creates, release)
	lifecycle := NewLifecycle()
	subrouter := newProxyTestServer(t, shutdownDrainTestServer(t, upstreamURL, lifecycle).Handler())
	defer subrouter.Close()
	lifecycle.Retire()

	const sockets = 2
	for i := range sockets {
		conn := dialShutdownDrainSocket(t, subrouter.URL, fmt.Sprintf("late-session-%d", i))
		defer conn.Close()
		events := readSocket(conn)
		expectNothing(t, events, 200*time.Millisecond, "a socket opened after Retire must stay open for its first turn")
		runTurn(t, conn, events)
		expectServiceRestart(t, events, "after the first turn of a socket opened after Retire")
	}
	if got := creates.Load(); got != sockets {
		t.Fatalf("upstream saw %d response.create, want one per socket opened after Retire (%d)", got, sockets)
	}
	waitForWebSocketTurns(t, lifecycle, 0, "after every late socket closed")
}

func startErrorMidTurnCodexUpstream(t *testing.T, sendRest <-chan struct{}) *url.URL {
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
			for _, event := range []string{
				`{"type":"response.created"}`,
				`{"type":"error","error":{"type":"invalid_request_error","code":"side_message_rejected"}}`,
			} {
				if err := conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
					return
				}
			}
			<-sendRest
			for _, event := range []string{
				`{"type":"response.output_text.delta","delta":"hello"}`,
				`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}`,
			} {
				if err := conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
					return
				}
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

func expectErrorMidTurnSurvivesRetire(t *testing.T, conn *websocket.Conn, lifecycle *Lifecycle, sendRest chan<- struct{}) {
	t.Helper()
	events := readSocket(conn)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(testResponseCreate)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"response.created", `"type":"error"`} {
		if event := nextSocketEvent(t, events, want); event.err != nil || !strings.Contains(event.body, want) {
			t.Fatalf("turn: got body=%q err=%v, want %s", event.body, event.err, want)
		}
	}

	lifecycle.Retire()
	expectNothing(t, events, 300*time.Millisecond, "a turn still streaming after an error event must not be cut at Retire")
	close(sendRest)
	for _, want := range []string{"response.output_text.delta", "response.completed"} {
		if event := nextSocketEvent(t, events, want); event.err != nil || !strings.Contains(event.body, want) {
			t.Fatalf("the rest of the turn: got body=%q err=%v, want %s", event.body, event.err, want)
		}
	}
	expectServiceRestart(t, events, "once the retired socket's upstream went quiet")
}

func TestRetiringAfterAnErrorThatDidNotEndTheTurnLetsTheTurnFinish(t *testing.T) {
	sendRest := make(chan struct{})
	upstreamURL := startErrorMidTurnCodexUpstream(t, sendRest)
	lifecycle := NewLifecycle()
	subrouter := newProxyTestServer(t, shutdownDrainTestServer(t, upstreamURL, lifecycle).Handler())
	defer subrouter.Close()
	conn := dialShutdownDrainSocket(t, subrouter.URL, "error-mid-turn-session")
	defer conn.Close()

	expectErrorMidTurnSurvivesRetire(t, conn, lifecycle, sendRest)
}
