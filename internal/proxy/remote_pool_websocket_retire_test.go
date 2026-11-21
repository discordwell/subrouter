package proxy

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestRetiringClosesACodexWebSocketForwardedToAPeer(t *testing.T) {
	var creates atomic.Int64
	release := make(chan struct{})
	peerURL := startTurnHoldingCodexUpstream(t, &creates, release)
	pool := NewRemotePool(peerURL.String(), "peer-token", "")
	if pool == nil {
		t.Fatal("NewRemotePool returned nil")
	}
	lifecycle := NewLifecycle()
	server := Server{RemotePool: pool, Sessions: newSessionStore(t), Lifecycle: lifecycle}

	idle, _, err := dialThroughSubrouter(t, server, "/v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	idleEvents := readSocketOnceRelayed(t, idle)
	busy, _, err := dialThroughSubrouter(t, server, "/v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	busyEvents := readSocket(busy)
	if err := busy.WriteMessage(websocket.TextMessage, []byte(testResponseCreate)); err != nil {
		t.Fatal(err)
	}
	if event := nextSocketEvent(t, busyEvents, "response.created"); event.err != nil || !strings.Contains(event.body, "response.created") {
		t.Fatalf("forwarded turn did not start: body=%q err=%v", event.body, event.err)
	}

	lifecycle.Retire()
	expectServiceRestart(t, idleEvents, "on an idle socket forwarded to a peer, after Retire,")
	expectNothing(t, busyEvents, 300*time.Millisecond, "a forwarded turn in flight at Retire must run to its end")
	close(release)
	if event := nextSocketEvent(t, busyEvents, "response.completed"); event.err != nil || !strings.Contains(event.body, "response.completed") {
		t.Fatalf("the forwarded turn in flight at Retire did not finish: body=%q err=%v", event.body, event.err)
	}
	expectServiceRestart(t, busyEvents, "right after the forwarded turn finished")
	waitForWebSocketTurns(t, lifecycle, 0, "after both forwarded sockets closed")
}

func TestStoppingWaitsForATurnForwardedToAPeer(t *testing.T) {
	var creates atomic.Int64
	release := make(chan struct{})
	peerURL := startTurnHoldingCodexUpstream(t, &creates, release)
	pool := NewRemotePool(peerURL.String(), "peer-token", "")
	if pool == nil {
		t.Fatal("NewRemotePool returned nil")
	}
	lifecycle := NewLifecycle()
	busy, _, err := dialThroughSubrouter(t, Server{RemotePool: pool, Sessions: newSessionStore(t), Lifecycle: lifecycle}, "/v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	events := readSocket(busy)
	if err := busy.WriteMessage(websocket.TextMessage, []byte(testResponseCreate)); err != nil {
		t.Fatal(err)
	}
	if event := nextSocketEvent(t, events, "response.created"); event.err != nil {
		t.Fatalf("forwarded turn did not start: %v", event.err)
	}

	lifecycle.Stop()
	if got := lifecycle.ActiveWebSocketTurns(); got != 1 {
		t.Fatalf("turns in flight = %d at Stop, want the forwarded one; the exit would cut it", got)
	}
	close(release)
	if event := nextSocketEvent(t, events, "response.completed"); event.err != nil || !strings.Contains(event.body, "response.completed") {
		t.Fatalf("the forwarded turn did not finish: body=%q err=%v", event.body, event.err)
	}
	waitForWebSocketTurns(t, lifecycle, 0, "after the forwarded turn finished")
}

func TestASocketForwardedToAPeerAfterRetireServesItsFirstTurnThenCloses1012(t *testing.T) {
	var creates atomic.Int64
	release := make(chan struct{})
	close(release)
	peerURL := startTurnHoldingCodexUpstream(t, &creates, release)
	pool := NewRemotePool(peerURL.String(), "peer-token", "")
	if pool == nil {
		t.Fatal("NewRemotePool returned nil")
	}
	lifecycle := NewLifecycle()
	lifecycle.Retire()
	conn, _, err := dialThroughSubrouter(t, Server{RemotePool: pool, Sessions: newSessionStore(t), Lifecycle: lifecycle}, "/v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	events := readSocket(conn)

	runTurn(t, conn, events)
	expectServiceRestart(t, events, "after the first turn of a socket forwarded to a peer after Retire")
	if got := creates.Load(); got != 1 {
		t.Fatalf("peer saw %d response.create, want the late socket's one turn", got)
	}
	waitForWebSocketTurns(t, lifecycle, 0, "after the late forwarded socket closed")
}

func TestRetiringAfterAnErrorThatDidNotEndATurnForwardedToAPeerLetsItFinish(t *testing.T) {
	sendRest := make(chan struct{})
	pool := NewRemotePool(startErrorMidTurnCodexUpstream(t, sendRest).String(), "peer-token", "")
	if pool == nil {
		t.Fatal("NewRemotePool returned nil")
	}
	lifecycle := NewLifecycle()
	conn, _, err := dialThroughSubrouter(t, Server{RemotePool: pool, Sessions: newSessionStore(t), Lifecycle: lifecycle}, "/v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	expectErrorMidTurnSurvivesRetire(t, conn, lifecycle, sendRest)
}
