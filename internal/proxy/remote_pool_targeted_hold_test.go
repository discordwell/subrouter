package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discordwell/subrouter/session"
)

// PATCHES §54, Fix D's write side: a hold records whether the peer served the
// account it was targeted at (Targeted), and keeps the record across a
// restart. TestARestartedHeldTurnWithNoUsageBodyStillTargetsItsAccount is the
// read side.

func TestTheTargetedBitFollowsWhatThePeerServed(t *testing.T) {
	var named atomic.Value
	named.Store("acct-a")
	_, server := newTestRemotePool(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(subrouterAccountHeader, named.Load().(string))
	})
	pool := server.RemotePool
	forward := func(sessionID, target string) {
		t.Helper()
		req := forwardRequest(t, http.MethodPost, "/v1/messages", []byte(`{}`))
		out := server.forwardToPeer(pool, req, "claude", sessionID, "/v1/messages", "", []byte(`{}`), remotePoolHoldSet, target)
		if !out.Served {
			t.Fatalf("calibration: the peer did not serve %s: %+v", sessionID, out.Switch)
		}
		out.Resp.Body.Close()
	}
	targeted := func(sessionID string) bool {
		hold, ok := pool.delegations.lookup(session.ScopedSessionKey("claude", sessionID), time.Now())
		if !ok {
			t.Fatalf("calibration: %s is not held", sessionID)
		}
		return hold.Targeted
	}

	forward("sent-for-a", "acct-a")
	if !targeted("sent-for-a") {
		t.Fatal("a turn the peer served on the account it was targeted at did not mark the hold targeted")
	}
	forward("untargeted", "")
	if targeted("untargeted") {
		t.Fatal("an untargeted turn marked the hold targeted")
	}
	// An untargeted re-serve on the same account (a restart's first turn,
	// before §54) is no evidence against the peer honouring targets.
	forward("sent-for-a", "")
	if !targeted("sent-for-a") {
		t.Fatal("an untargeted re-serve on the same account dropped the bit")
	}
	// The peer placed it elsewhere by itself: the hold now names an account it
	// was never sent for.
	named.Store("acct-b")
	forward("sent-for-a", "")
	if targeted("sent-for-a") {
		t.Fatal("the bit survived the peer moving the session to an account it was not sent for")
	}

	path := filepath.Join(t.TempDir(), "peer-holds.json")
	named.Store("acct-a")
	forward("restarted", "acct-a")
	pool.delegations.flush(path, time.Now())
	after := NewRemotePool(pool.URL.String(), "", "")
	after.delegations.loadFor(path, time.Now(), pool.name())
	for sessionID, want := range map[string]bool{"restarted": true, "untargeted": false} {
		hold, ok := after.delegations.lookup(session.ScopedSessionKey("claude", sessionID), time.Now())
		if !ok || hold.Targeted != want {
			t.Fatalf("after a restart %s held=%v targeted=%v, want held targeted=%v", sessionID, ok, hold.Targeted, want)
		}
	}
}

// The websocket hold (Codex turns) records the same bit.
func TestAWebSocketTheTargetTookMarksTheHoldTargeted(t *testing.T) {
	peer := accountStampingPeer(t, nil)
	server := Server{RemotePool: NewRemotePool(peer.URL, "", "")}
	for _, target := range []string{"peer-acct", ""} {
		front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			server.serveWebSocketToPeer(w, r, server.RemotePool, "codex", wsHoldSession, remotePoolHoldSet, target)
		}))
		conn := dialFront(t, front)
		// The hold is set before the relay starts, so an echo proves it is there.
		echoOnce(t, conn, "turn")
		hold, ok := server.RemotePool.delegations.lookup(wsHoldKey(), time.Now())
		_ = conn.Close()
		front.Close()
		if !ok || hold.Targeted != (target != "") {
			t.Fatalf("target %q: held=%v targeted=%v, want targeted=%v", target, ok, hold.Targeted, target != "")
		}
		server.RemotePool.delegations.clear(wsHoldKey())
	}
}
