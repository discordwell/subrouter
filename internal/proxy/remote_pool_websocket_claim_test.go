package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// The peer took the socket (101) but the client's own upgrade failed: no turn
// reached the peer, so the claim this call made before dialing goes, and the
// failure is reported as handled (Upgrade already wrote its own response).
func TestAFailedClientUpgradeReleasesThePeerClaim(t *testing.T) {
	var upgrades atomic.Int32
	peer := accountStampingPeer(t, func(*http.Request) { upgrades.Add(1) })
	server := Server{RemotePool: NewRemotePool(peer.URL, "", "")}

	// httptest.ResponseRecorder cannot be hijacked, so Upgrade fails after
	// the peer has answered 101.
	served := server.serveRemotePool(httptest.NewRecorder(), webSocketUpgradeRequest(context.Background()), "codex", wsHoldSession, remotePoolHoldSet)
	if !served {
		t.Fatal("calibration: a failed client upgrade must report the request as handled")
	}
	if upgrades.Load() != 1 {
		t.Fatalf("calibration: the peer saw %d upgrades, want 1", upgrades.Load())
	}
	if server.RemotePool.delegations.active(wsHoldKey(), time.Now()) {
		t.Fatal("a failed client upgrade left the pre-claim holding the session on the peer")
	}
}

// Traffic on an open socket touches the hold at most once per
// remotePoolWebSocketTouchInterval. The observer runs before each frame is
// flushed to the other side, so once echoOnce returns both of its frames have
// been observed: the checks below need no sleeps.
func TestAnOpenWebSocketTouchesTheHoldAtMostOncePerInterval(t *testing.T) {
	peer := accountStampingPeer(t, nil)
	server := Server{RemotePool: NewRemotePool(peer.URL, "", "")}
	front, _ := poolFront(t, server, remotePoolHoldSet)
	pool := server.RemotePool

	conn := dialFront(t, front)
	waitForNamedHold(t, pool)

	// Calibration: the first frame touches. Shorten the hold, send one turn,
	// and the observer brings it back to a full idle bound.
	pool.delegations.set(wsHoldKey(), time.Now(), 20*time.Second, "peer-acct", pool.name())
	sent := time.Now()
	echoOnce(t, conn, "turn1")
	ttl := remotePoolDelegationTTLFor("codex")
	entry, ok := pool.delegations.lookup(wsHoldKey(), time.Now())
	if !ok || entry.Until.Before(sent.Add(ttl-time.Second)) {
		t.Fatalf("calibration: the first frame did not touch the hold (%+v, ok=%v)", entry, ok)
	}

	// Shorten it again: every further frame inside the interval leaves it be.
	pool.delegations.set(wsHoldKey(), time.Now(), 20*time.Second, "peer-acct", pool.name())
	shortened, _ := pool.delegations.lookup(wsHoldKey(), time.Now())
	for _, message := range []string{"turn2", "turn3", "turn4", "turn5"} {
		echoOnce(t, conn, message)
		entry, ok := pool.delegations.lookup(wsHoldKey(), time.Now())
		if !ok {
			t.Fatalf("hold gone after %s", message)
		}
		if !entry.Until.Equal(shortened.Until) {
			t.Fatalf("%s inside the touch interval moved the hold: Until %v -> %v", message, shortened.Until, entry.Until)
		}
	}
}
