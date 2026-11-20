package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/session"
)

// A session held on the peer re-ranks on the same idle bound that holds one on
// a local account, for both providers, so neither side is stickier than the
// other.
func TestPeerHoldMatchesTheLocalIdleBound(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		if got := remotePoolDelegationTTLFor(agent); got != warmSessionIdleTTL {
			t.Fatalf("%s peer hold = %s, want the local idle bound %s", agent, got, warmSessionIdleTTL)
		}
	}

	var d remotePoolDelegations
	now := time.Now()
	d.set("k", now, remotePoolDelegationTTLFor("claude"), "", "")
	if !d.active("k", now.Add(warmSessionIdleTTL-time.Minute)) {
		t.Fatal("a claude session idle inside the cache TTL must stay on the peer")
	}
	if d.active("k", now.Add(warmSessionIdleTTL+time.Minute)) {
		t.Fatal("a claude session idle past the cache TTL must be free to re-rank")
	}
}

func TestPeerHoldIdleClockRestartsWhenATurnEnds(t *testing.T) {
	var d remotePoolDelegations
	ttl := warmSessionIdleTTL
	start := time.Now()
	d.set("k", start, ttl, "acct", "box")

	// A turn that ends an hour after it was forwarded restarts the clock from
	// its end, exactly as ActiveSessions stamps a local turn on release.
	end := start.Add(time.Hour)
	d.touch("k", end, ttl)
	if got, _ := d.lookup("k", end); got.Account != "acct" || got.Peer != "box" {
		t.Fatalf("touch dropped the serving account: %+v", got)
	}
	if !d.active("k", end.Add(ttl-time.Minute)) {
		t.Fatal("the idle clock ran from the forward, not from the end of the turn")
	}
	// Last: lookup deletes an entry it finds expired.
	if d.active("k", end.Add(ttl+time.Minute)) {
		t.Fatal("a touched hold must still expire one idle bound after the turn ends")
	}
}

func TestTouchNeverResurrectsAPeerHold(t *testing.T) {
	var d remotePoolDelegations
	ttl := warmSessionIdleTTL
	now := time.Now()

	d.set("cleared", now, ttl, "", "")
	d.clear("cleared")
	d.touch("cleared", now, ttl)
	if d.active("cleared", now) {
		t.Fatal("a turn ending after its forward failed brought the hold back")
	}

	d.set("expired", now, ttl, "", "")
	d.touch("expired", now.Add(ttl+time.Minute), ttl)
	if d.active("expired", now.Add(ttl+2*time.Minute)) {
		t.Fatal("touching an expired hold revived it")
	}

	d.touch("never-set", now, ttl)
	if d.active("never-set", now) {
		t.Fatal("touch created a hold that was never set")
	}
}

// End to end through remotePoolForward: the hold's deadline moves when the
// forwarded body is closed, not when the request was sent.
func TestForwardedTurnRestartsThePeerHoldWhenItsBodyCloses(t *testing.T) {
	const streamFor = 150 * time.Millisecond
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(subrouterAccountHeader, "peer-acct")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(streamFor)
		_, _ = io.WriteString(w, "done")
	}))
	defer peer.Close()

	server := Server{RemotePool: NewRemotePool(peer.URL, "", "")}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	resp, ok := server.remotePoolForward(req, "claude", "s1", "/v1/messages", "", []byte(`{}`), true)
	if !ok {
		t.Fatal("forward to a healthy peer declined")
	}
	key := session.ScopedSessionKey("claude", "s1")
	atHeaders, ok := server.RemotePool.delegations.lookup(key, time.Now())
	if !ok {
		t.Fatal("a successful forward did not hold the session")
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	atClose, ok := server.RemotePool.delegations.lookup(key, time.Now())
	if !ok {
		t.Fatal("the hold vanished when the turn ended")
	}
	if moved := atClose.Until.Sub(atHeaders.Until); moved < streamFor/2 {
		t.Fatalf("closing the body moved the deadline by %s; want about the %s the turn streamed", moved, streamFor)
	}
	if atClose.Account != "peer-acct" {
		t.Fatalf("serving account after close = %q, want peer-acct", atClose.Account)
	}
}

// Only a peer that cannot serve ends a warm session's hold: it declines (its
// pool is spent) or it is down (the breaker opens). A client cancel and a
// single failed round trip are neither.

func heldSessionServer(t *testing.T, peerURL string) (Server, string) {
	t.Helper()
	server := Server{RemotePool: NewRemotePool(peerURL, "", "")}
	key := session.ScopedSessionKey("claude", "s1")
	server.RemotePool.delegations.set(key, time.Now(), remotePoolDelegationTTLFor("claude"), "peer-acct", "box")
	return server, key
}

func TestAClientCancelKeepsThePeerHold(t *testing.T) {
	release := make(chan struct{})
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer peer.Close()
	defer close(release)
	server, key := heldSessionServer(t, peer.URL)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`)).WithContext(ctx)
	time.AfterFunc(50*time.Millisecond, cancel)
	if _, ok := server.remotePoolForward(req, "claude", "s1", "/v1/messages", "", []byte(`{}`), true); ok {
		t.Fatal("a cancelled forward reported success")
	}
	if !server.RemotePool.delegations.active(key, time.Now()) {
		t.Fatal("pressing Esc ended the session's hold on the peer")
	}
	if !server.RemotePool.breaker.allow(time.Now()) {
		t.Fatal("a client cancel counted against the peer")
	}
}

func TestOneFailedForwardKeepsTheHoldAndADownPeerIsFoundDown(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // connection refused from here on
	server, key := heldSessionServer(t, deadURL)
	forward := func() bool {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
		_, ok := server.remotePoolForward(req, "claude", "s1", "/v1/messages", "", []byte(`{}`), true)
		return ok
	}

	if forward() {
		t.Fatal("a forward to a dead peer succeeded")
	}
	if !server.RemotePool.delegations.active(key, time.Now()) {
		t.Fatal("one failed round trip ended the hold; that is a blip, not an eviction")
	}
	// This machine's own upstream answers meanwhile: the peer, not this
	// machine, is down (PATCHES §54).
	localUpstreamAnswered(server.RemotePool)
	if forward() {
		t.Fatal("a forward to a dead peer succeeded")
	}
	if server.RemotePool.breaker.allow(time.Now()) {
		t.Fatalf("calibration: the breaker did not open after %d failures", remotePoolFailuresToOpen)
	}
	if !server.RemotePool.breaker.foundDown() || !server.RemotePool.delegations.active(key, time.Now()) {
		t.Fatal("want the peer found down with the hold kept: the session's next turn places it elsewhere (PATCHES §67)")
	}
}
