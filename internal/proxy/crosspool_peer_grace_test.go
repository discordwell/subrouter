package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/session"
)

// PATCHES §67 end to end on two boxes.

// The box really is down (§54, no grace configured here), and finding it
// down ends no hold (PATCHES §67). The turn that finds it is a refugee, and
// so is the next turn of every other chat it held: each is placed on a local
// account (the box is the laptop's only peer) and marked there. A refugee's
// following turn is an ordinary one on that account.
func TestEverySessionADownBoxHeldIsPlacedAsARefugeeOnItsNextTurn(t *testing.T) {
	now := time.Now()
	box := newE2EBox(t, map[string]string{
		"acct-p": e2eUsage(now, 10, 3*time.Hour, 30, 100*time.Hour),
		"chip-a": e2eUsage(now, 10, 3*time.Hour, 30, 100*time.Hour),
	})
	e2eMeasure(t, box.server)
	holdsPath := filepath.Join(t.TempDir(), "peer-holds.json")
	e2eWriteHolds(t, holdsPath, box.newPool().name(), e2eRebootHolds(), true)
	laptop, pool := e2eRestartedLaptop(t, box, map[string]string{
		"nine-alpha": e2eUsage(now, 10, 3*time.Hour, 20, 120*time.Hour),
		"main":       e2eUsage(now, 10, 3*time.Hour, 20, 120*time.Hour),
		"fifteen":    e2eUsage(now, 10, 3*time.Hour, 20, 120*time.Hour),
	}, holdsPath, map[string]string{rebootSixE: "nine-alpha", rebootFdcd: "main", rebootD6c2: "fifteen"})
	var boxDown atomic.Bool
	boxDown.Store(true)
	pool.client.Transport = e2eOfflineTransport{base: pool.client.Transport, offline: &boxDown}
	laptop.turn(t, rebootFdcd)
	if got := laptop.turn(t, rebootSixE); got.Status != http.StatusOK || got.Pool == "remote" {
		t.Fatalf("%s: status %d pool %q; the turn that finds the box down is placed locally", rebootSixE, got.Status, got.Pool)
	}
	if failed := e2eLogLines(laptop.logs, "remote pool forward failed"); len(failed) != 2 || !e2eLineSays(failed[1], "peer_down", true) {
		t.Fatalf("calibration: forward-failed lines %v, want 46ee's strike finding the box down", failed)
	}
	for _, sessionID := range []string{rebootD6c2, rebootFdcd} {
		if _, held := e2eHold(pool, sessionID); !held {
			t.Fatalf("%s: hold ended when the box was found down; it ends only when the chat is placed", sessionID)
		}
	}
	for _, sessionID := range []string{rebootD6c2, rebootFdcd} {
		if got := laptop.turn(t, sessionID); got.Status != http.StatusOK || got.Pool == "remote" {
			t.Fatalf("%s: status %d pool %q; with the box down the turn is placed locally", sessionID, got.Status, got.Pool)
		}
		if assignment, ok := laptop.store.Get("claude", sessionID); !ok || assignment.Refugee == nil {
			t.Errorf("%s: assignment %+v, want it marked a refugee", sessionID, assignment)
		}
		if _, held := e2eHold(pool, sessionID); held {
			t.Errorf("%s: still held on the box found down after it was placed", sessionID)
		}
	}
	placed := len(awaitLogLines(t, laptop.logs, refugeePlacedMessage, 3))
	if placed != 3 {
		t.Fatalf("%d placement lines, want one for each chat the box held", placed)
	}
	for _, sessionID := range []string{rebootSixE, rebootD6c2, rebootFdcd} {
		if got := laptop.turn(t, sessionID); got.Status != http.StatusOK || got.Pool == "remote" {
			t.Errorf("%s: status %d pool %q on its following turn", sessionID, got.Status, got.Pool)
		}
	}
	if n := len(e2eLogLines(laptop.logs, refugeePlacedMessage)); n != placed {
		t.Errorf("%d placement lines after the following turns, want %d: a refugee is placed once", n, placed)
	}
}

// heldOnSWithItsBreakerOpen has the laptop's local accounts given: box S
// holds s1 on s-acct with its breaker open and its holds kept, and box B's
// b2d ranks above every local account.
func heldOnSWithItsBreakerOpen(t *testing.T, local map[string]string) (laptop *e2eLaptop, b *e2eBox, sPool, bPool *RemotePool) {
	t.Helper()
	now := time.Now()
	s := newE2EBox(t, map[string]string{"s-acct": e2eUsage(now, 10, 3*time.Hour, 30, 100*time.Hour)})
	b = newE2EBox(t, map[string]string{"b2d": e2eUsage(now, 0, 4*time.Hour, 0, 48*time.Hour)})
	laptop = newE2ELaptop(t, local, nil)
	sPool, bPool = e2eTwoPeers(t, laptop, s, b, true)
	sPool.delegations.setServed(session.ScopedSessionKey("claude", "s1"), now, remotePoolDelegationTTLFor("claude"), "s-acct", sPool.name(), true)
	sPool.breaker.mu.Lock()
	sPool.breaker.openUntil = time.Now().Add(remotePoolCooldown)
	sPool.breaker.failures = remotePoolFailuresToOpen - 1
	sPool.breaker.unattributed = true
	sPool.breaker.mu.Unlock()
	return laptop, b, sPool, bPool
}

// The session is held on S, S's breaker is open with its holds kept, and the
// laptop cannot serve the turn: its one account is spent (the no-account
// chain), or its upstream answers that account spent (local failover runs
// out). Until S is found down the turn waits for S's s-acct and box B is
// never asked. Once S is found down the session moves to box B for good: B
// serves the turn and holds the session as a refugee, and S holds it no
// more. It is never served on B for one turn and sent back (owner,
// 2026-11-19, D4).
func TestAHeldTurnLocalCannotServeWaitsForItsBoxThenMovesForGood(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name  string
		local map[string]string
		setup func(laptop *e2eLaptop)
	}{
		{name: "the local account is spent", local: map[string]string{"spent": e2eUsage(now, 100, 2*time.Hour, 100, 100*time.Hour)}},
		{name: "the local upstream answers the account spent", local: e2eFiveDays(), setup: func(laptop *e2eLaptop) {
			laptop.upstream.setAnswer(func(_ string, w http.ResponseWriter) bool {
				e2eRejected(w)
				return true
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			laptop, b, sPool, bPool := heldOnSWithItsBreakerOpen(t, tc.local)
			if tc.setup != nil {
				tc.setup(laptop)
			}
			policy := strandedPolicy(nil)
			laptop.server.Hold = policy
			front := httptest.NewServer(laptop.server.Handler())
			t.Cleanup(front.Close)
			ctx := promptly(t)
			done := make(chan e2eResult, 1)
			go func() {
				got, err := e2eDoBody(ctx, front.URL, "s1", e2eModelTurn, nil)
				if err != nil && ctx.Err() == nil {
					t.Error(err)
				}
				done <- got
			}()
			deadline := time.Now().Add(10 * time.Second)
			for policy.Active() != 1 {
				select {
				case got := <-done:
					t.Fatalf("answered %d by %q instead of waiting for s-acct", got.Status, got.ServedBy)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("the turn was never held")
				}
				time.Sleep(time.Millisecond)
			}
			if turns := b.seen(); len(turns) != 0 {
				t.Fatalf("box B turns %+v before S was found down; want none", turns)
			}
			if account, held := e2eHold(sPool, "s1"); !held || account != "s-acct" {
				t.Fatalf("hold on S = %q/%v while it waits; want s-acct kept", account, held)
			}
			sPool.breaker.mu.Lock()
			sPool.breaker.down = true
			sPool.breaker.mu.Unlock()
			var got e2eResult
			select {
			case got = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("still held 10 s after S was found down")
			}
			if got.Status != http.StatusOK || got.ServedBy != "b2d" {
				t.Fatalf("status %d served by %q; want box B's b2d once S was found down", got.Status, got.ServedBy)
			}
			if account, held := e2eHold(sPool, "s1"); held {
				t.Errorf("S still holds the session on %q; it moved to box B for good", account)
			}
			if hold, held := bPool.delegations.lookup(session.ScopedSessionKey("claude", "s1"), time.Now()); !held || hold.Account != "b2d" || hold.Refugee == nil {
				t.Errorf("hold on B = %+v/%v; want b2d, marked a refugee", hold, held)
			}
		})
	}
}

// The may-be-running rule (PATCHES §58) when local cannot serve, for a box
// that publishes verdicts. Box S takes the held turn on s-acct and sends no
// headers before the timeout, which leaves its breaker closed, and the
// laptop's one account is spent, so the no-account chain ranks the peers'
// accounts. It does not rank s-acct again: S sees the turn once, and the
// client gets the spent pool's 503.
func TestAHeldTurnItsBoxMayBeRunningIsNotRankedOntoItWhenLocalCannotServe(t *testing.T) {
	now := time.Now()
	s := newE2EBox(t, map[string]string{"s-acct": e2eUsage(now, 0, 4*time.Hour, 0, 48*time.Hour)})
	unblock := make(chan struct{})
	t.Cleanup(func() { close(unblock) })
	var hits atomic.Int64
	s.upstream.setAnswer(func(_ string, w http.ResponseWriter) bool {
		if hits.Add(1) == 1 {
			<-unblock
			return true
		}
		return false
	})
	laptop := newE2ELaptop(t, map[string]string{"spent": e2eUsage(now, 100, 2*time.Hour, 100, 100*time.Hour)}, nil)
	sPool := s.newPool().WithName("box-s")
	laptop.server.RemotePeers = []*RemotePool{sPool}
	e2ePrime(t, sPool)
	sPool.client.Transport.(*http.Transport).ResponseHeaderTimeout = 50 * time.Millisecond
	sPool.delegations.setServed(session.ScopedSessionKey("claude", "s1"), now, remotePoolDelegationTTLFor("claude"), "s-acct", sPool.name(), true)

	got := laptop.turn(t, "s1")
	if failed := e2eLogLines(laptop.logs, "remote pool forward failed"); len(failed) != 1 || !e2eLineSays(failed[0], "breaker_opened", false) {
		t.Fatalf("calibration: forward-failed lines %v; want S's one timeout, its breaker left closed", failed)
	}
	if turns := s.seen(); len(turns) != 1 {
		t.Errorf("box S saw the turn %d times (%+v), want once: it may be running it", len(turns), turns)
	}
	if got.Status != http.StatusServiceUnavailable {
		t.Errorf("status %d served by %q; want the spent pool's 503", got.Status, got.ServedBy)
	}
	if account, held := e2eHold(sPool, "s1"); !held || account != "s-acct" {
		t.Errorf("hold on S = %q/%v; want s-acct kept", account, held)
	}
}
