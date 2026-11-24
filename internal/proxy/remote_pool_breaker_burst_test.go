package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
	"github.com/discordwell/subrouter/session"
)

// blipPeer is a peer whose connection drops before any headers, on every
// request. The first request waits at the peer until the second one arrives,
// so the two are in flight together and fail in one blip. firstArrived closes
// when the first request reaches the peer.
func blipPeer(t *testing.T) (url string, hits *atomic.Int32, firstArrived <-chan struct{}) {
	t.Helper()
	hits = new(atomic.Int32)
	arrived := make(chan struct{})
	second := make(chan struct{})
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == remotePoolUsagePath {
			_, _ = io.WriteString(w, strongCodexPeer)
			return
		}
		switch hits.Add(1) {
		case 1:
			close(arrived)
			select {
			case <-second:
			case <-time.After(3 * time.Second):
			}
		case 2:
			close(second)
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(peer.Close)
	return peer.URL, hits, arrived
}

func waitArrived(t *testing.T, arrived <-chan struct{}) {
	t.Helper()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("calibration: the first request never reached the peer")
	}
}

// Two forwards in flight together that both fail are ONE blip, not two strikes.
// With the pre-claim a spilling turn and the requests that follow its pending
// claim hit the peer together, so counting each failure opened the breaker on
// one blip, which found the peer down. A forward sent AFTER that failure that
// fails too is a second strike: the peer is down, and the breaker opens as
// before.
func TestForwardsThatFailInOneBlipCountAsOneFailure(t *testing.T) {
	peerURL, hits, arrived := blipPeer(t)
	server, key := heldSessionServer(t, peerURL)
	other := session.ScopedSessionKey("claude", "another-warm-session")
	server.RemotePool.delegations.set(other, time.Now(), remotePoolDelegationTTLFor("claude"), "peer-acct", "box")
	forward := func() bool {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
		_, ok := server.remotePoolForward(req, "claude", "s1", "/v1/messages", "", []byte(`{}`), true)
		return ok
	}

	results := make(chan bool, 1)
	go func() { results <- forward() }()
	waitArrived(t, arrived)
	if forward() {
		t.Fatal("a forward to a blipping peer succeeded")
	}
	if <-results {
		t.Fatal("a forward to a blipping peer succeeded")
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("calibration: peer saw %d requests, want 2", got)
	}
	if !server.RemotePool.breaker.allow(time.Now()) {
		t.Fatal("two forwards that failed in one blip opened the breaker")
	}
	if !server.RemotePool.delegations.active(other, time.Now()) {
		t.Fatal("one blip evicted another session held on the peer")
	}
	if !server.RemotePool.delegations.active(key, time.Now()) {
		t.Fatal("one blip ended the forwarding session's own hold")
	}

	// Sent after the blip, and it fails too while this machine's own upstream
	// answers: that is the peer being down (PATCHES §54).
	localUpstreamAnswered(server.RemotePool)
	if forward() {
		t.Fatal("a forward to a blipping peer succeeded")
	}
	if server.RemotePool.breaker.allow(time.Now()) {
		t.Fatal("a failure sent after the last counted failure did not open the breaker")
	}
	if !server.RemotePool.breaker.foundDown() || !server.RemotePool.delegations.active(other, time.Now()) {
		t.Fatal("want the opening to find the peer down and keep its holds until each chat is placed (PATCHES §67)")
	}
}

// The websocket dial counts toward the same breaker, so it follows the same
// rule: dials in flight together that fail together are one failure.
func TestWebSocketDialsThatFailInOneBlipCountAsOneFailure(t *testing.T) {
	peerURL, hits, arrived := blipPeer(t)
	server := heldCodexWebSocketServer(t, peerURL)
	other := session.ScopedSessionKey("codex", "another-warm-session")
	server.RemotePool.delegations.set(other, time.Now(), remotePoolDelegationTTLFor("codex"), "peer-acct", server.RemotePool.name())
	dial := func() bool {
		recorder := httptest.NewRecorder()
		return server.serveRemotePool(recorder, webSocketUpgradeRequest(context.Background()), "codex", wsHoldSession, remotePoolHoldSet)
	}

	results := make(chan bool, 1)
	go func() { results <- dial() }()
	waitArrived(t, arrived)
	if dial() {
		t.Fatal("a dial to a blipping peer reported the socket as served")
	}
	if <-results {
		t.Fatal("a dial to a blipping peer reported the socket as served")
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("calibration: peer saw %d dials, want 2", got)
	}
	if !server.RemotePool.breaker.allow(time.Now()) {
		t.Fatal("two dials that failed in one blip opened the breaker")
	}
	if !server.RemotePool.delegations.active(other, time.Now()) {
		t.Fatal("one blip evicted another session held on the peer")
	}

	localUpstreamAnswered(server.RemotePool) // this machine is online (PATCHES §54)
	if dial() {
		t.Fatal("a dial to a blipping peer reported the socket as served")
	}
	if server.RemotePool.breaker.allow(time.Now()) {
		t.Fatal("a dial failure sent after the last counted failure did not open the breaker")
	}
	if !server.RemotePool.breaker.foundDown() || !server.RemotePool.delegations.active(other, time.Now()) {
		t.Fatal("want the opening to find the peer down and keep its holds until each chat is placed (PATCHES §67)")
	}
}

// The review's reproduction, end to end: one network blip while a NEW Codex
// session spills. Its first turn and a concurrent turn on the same thread
// (following the pending claim) both hit the peer's broken connection. Another
// session already held on the peer must survive a single blip.
func TestOneBlipOnASpillDoesNotEvictOtherHolds(t *testing.T) {
	var local atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		local.Add(1)
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(upstream.Close)

	peerURL, hits, arrived := blipPeer(t)
	pool := NewRemotePool(peerURL, "", "")
	const other = "codex:another-warm-session"
	pool.delegations.set(other, time.Now(), remotePoolDelegationTTLFor("codex"), "peer-acct", pool.name())

	server := Server{
		Accounts: []accounts.Account{
			{ID: "loc", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "loc-token"},
		},
		Sessions:       newSessionStore(t),
		ActiveSessions: NewActiveSessions(),
		Scheduler: selectacct.NewScheduler([]selectacct.Score{{
			AccountID: "loc", Provider: accounts.ProviderCodex, Fresh: true,
			Headroom: 0.35, ShortHeadroom: 0.5, WeeklyHeadroom: 0.35, WeeklyRemaining: 0.35,
			WeeklyResetAt: time.Now().Add(100 * time.Hour),
		}}),
		Upstream:     mustURL(t, upstream.URL),
		RemotePool:   pool,
		MaxBodyBytes: 1024,
	}
	front := httptest.NewServer(server.Handler())
	t.Cleanup(front.Close)

	done := make(chan struct{})
	go func() { _, _ = postCodexTurn(front.URL); close(done) }()
	waitArrived(t, arrived)
	_, _ = postCodexTurn(front.URL)
	<-done

	if got := hits.Load(); got != 2 {
		t.Fatalf("calibration: peer saw %d turns, want 2 (both turns must reach the peer)", got)
	}
	if !pool.breaker.allow(time.Now()) {
		t.Fatal("one blip during one session's spill opened the breaker")
	}
	if !pool.delegations.active(other, time.Now()) {
		t.Fatal("one blip during one session's spill evicted every other session held on the peer")
	}
}
