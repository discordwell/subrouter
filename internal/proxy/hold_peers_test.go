package proxy

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// switchablePeer refuses every dial to pool while down is set. With readFirst
// it reads the peer's usage before the outage, so the ranking has verdicts;
// without, it has none, as after a daemon restart during an outage.
func switchablePeer(t *testing.T, pool *RemotePool, down *atomic.Bool, readFirst bool) {
	t.Helper()
	if readFirst {
		if snapshot := pool.usageForDecision(time.Now().Add(5 * time.Second)); !snapshot.Reachable {
			t.Fatalf("the peer's usage read failed before the outage: %s", snapshot.Err)
		}
	}
	transport := pool.client.Transport.(*http.Transport)
	transport.CloseIdleConnections()
	transport.DisableKeepAlives = true
	dial := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if down.Load() {
			return nil, refusedFailure()
		}
		return dial(ctx, network, addr)
	}
}

func ageUsage(pool *RemotePool, by time.Duration) {
	pool.admin.mu.Lock()
	defer pool.admin.mu.Unlock()
	e := pool.admin.entries[remotePoolUsagePath]
	e.at = e.at.Add(-by)
	e.tried = e.at
	pool.admin.entries[remotePoolUsagePath] = e
}

type heldTurn struct {
	status int
	body   string
}

// bringBack ends the outage: the peers answer again and their breakers'
// cooldowns have run out, as they do within 30 s of the network returning.
func bringBack(down *atomic.Bool, pools []*RemotePool) {
	down.Store(false)
	for _, p := range pools {
		p.breaker.mu.Lock()
		p.breaker.openUntil = time.Now().Add(-time.Millisecond)
		p.breaker.mu.Unlock()
	}
}

// turnUntilPeersReturn sends one turn while every peer is down, waits for the
// hold, keeps the peers down for downFor, then brings them back.
func turnUntilPeersReturn(t *testing.T, rig *holdRig, policy *HoldPolicy, down *atomic.Bool, downFor time.Duration, pools []*RemotePool) heldTurn {
	t.Helper()
	done := make(chan heldTurn, 1)
	go func() {
		resp, body, err := rig.turn(t, context.Background(), nil)
		if err != nil {
			t.Error(err)
			done <- heldTurn{}
			return
		}
		done <- heldTurn{resp.StatusCode, body}
	}()
	waitHeld(t, rig, policy, done)
	select {
	case got := <-done:
		t.Fatalf("answered %d %q while every peer was still down; logs:\n%s", got.status, got.body, rig.logs.String())
	case <-time.After(downFor):
	}
	bringBack(down, pools)
	select {
	case got := <-done:
		rig.settle(t, 1)
		return got
	case <-time.After(10 * time.Second):
		t.Fatalf("no answer 10s after the peers came back; logs:\n%s", rig.logs.String())
	}
	return heldTurn{}
}

// waitHeld fails at once, with the answer, when the turn is answered instead of
// held.
func waitHeld(t *testing.T, rig *holdRig, policy *HoldPolicy, done chan heldTurn) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for policy.Active() != 1 {
		select {
		case got := <-done:
			t.Fatalf("answered %d %q instead of holding the turn; logs:\n%s", got.status, got.body, rig.logs.String())
		case <-time.After(5 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("not held and not answered after 10s; logs:\n%s", rig.logs.String())
		}
	}
}

func assertHeldThenServedOnce(t *testing.T, rig *holdRig, got heldTurn, boxes ...*contractBox) {
	t.Helper()
	logs := rig.logs.String()
	if got.status != http.StatusOK || !strings.Contains(got.body, "msg_box") {
		t.Fatalf("status = %d body = %q, want a peer's 200 once the peers came back; logs:\n%s", got.status, got.body, logs)
	}
	if rig.countLog("proxy hold started") != 1 || !strings.Contains(logs, "reason=peers_unreachable:pool_exhausted") {
		t.Fatalf("want one hold start naming the unreachable peers; logs:\n%s", logs)
	}
	if rig.countLog("proxy hold ended") != 1 || !strings.Contains(logs, "outcome=answered") {
		t.Fatalf("want one hold end, answered; logs:\n%s", logs)
	}
	served := 0
	for _, b := range boxes {
		served += len(b.seen())
	}
	if served != 1 {
		t.Fatalf("the peers saw %d turns, want 1", served)
	}
}

// Two peers and no verdict to rank them on (a restart during the outage): the
// turn goes untargeted, and it is held while neither peer can be dialed.
func TestHoldRidesOutTwoUnreachablePeersWithNoVerdicts(t *testing.T) {
	boxA := newContractBox(t, eligibleBoxAccount("acct-a"))
	boxB := newContractBox(t, eligibleBoxAccount("acct-b"))
	var down atomic.Bool
	down.Store(true)
	switchablePeer(t, boxA.pool, &down, false)
	switchablePeer(t, boxB.pool, &down, false)
	policy := holdTestPolicy(20 * time.Second)
	rig := newHoldRig(t, holdRigOptions{hold: policy, upstream: noLocalUpstream(t), exhausted: true,
		peers: []*RemotePool{boxA.pool, boxB.pool}})
	got := turnUntilPeersReturn(t, rig, policy, &down, 300*time.Millisecond, []*RemotePool{boxA.pool, boxB.pool})
	assertHeldThenServedOnce(t, rig, got, boxA, boxB)
}

// Same with verdicts older than the ranking accepts, where the 11/19 outage
// was a quarter of an hour in.
func TestHoldRidesOutTwoUnreachablePeersWithStaleVerdicts(t *testing.T) {
	boxA := newContractBox(t, eligibleBoxAccount("acct-a"))
	boxB := newContractBox(t, eligibleBoxAccount("acct-b"))
	var down atomic.Bool
	down.Store(true)
	switchablePeer(t, boxA.pool, &down, true)
	switchablePeer(t, boxB.pool, &down, true)
	ageUsage(boxA.pool, remotePoolDecisionBodyMaxAge+time.Minute)
	ageUsage(boxB.pool, remotePoolDecisionBodyMaxAge+time.Minute)
	policy := holdTestPolicy(20 * time.Second)
	rig := newHoldRig(t, holdRigOptions{hold: policy, upstream: noLocalUpstream(t), exhausted: true,
		peers: []*RemotePool{boxA.pool, boxB.pool}})
	got := turnUntilPeersReturn(t, rig, policy, &down, 300*time.Millisecond, []*RemotePool{boxA.pool, boxB.pool})
	assertHeldThenServedOnce(t, rig, got, boxA, boxB)
}

// Three ranked peers: a failed dial sends nothing, so it must not use up the
// per-turn bound before the third peer is tried.
func TestHoldRidesOutThreeUnreachablePeers(t *testing.T) {
	boxes := []*contractBox{
		newContractBox(t, eligibleBoxAccount("acct-a")),
		newContractBox(t, eligibleBoxAccount("acct-b")),
		newContractBox(t, eligibleBoxAccount("acct-c")),
	}
	var down atomic.Bool
	down.Store(true)
	var pools []*RemotePool
	for _, b := range boxes {
		switchablePeer(t, b.pool, &down, true)
		pools = append(pools, b.pool)
	}
	policy := holdTestPolicy(20 * time.Second)
	rig := newHoldRig(t, holdRigOptions{hold: policy, upstream: noLocalUpstream(t), exhausted: true, peers: pools})
	got := turnUntilPeersReturn(t, rig, policy, &down, 300*time.Millisecond, pools)
	assertHeldThenServedOnce(t, rig, got, boxes...)
}

// A held turn stays held when the peers' breakers close again and the next
// attempt dials them all afresh: it is not let go with the pool-exhausted 503.
func TestHoldOutlastsThePeerBreakersClosingMidHold(t *testing.T) {
	boxA := newContractBox(t, eligibleBoxAccount("acct-a"))
	boxB := newContractBox(t, eligibleBoxAccount("acct-b"))
	var down atomic.Bool
	down.Store(true)
	switchablePeer(t, boxA.pool, &down, false)
	switchablePeer(t, boxB.pool, &down, false)
	policy := holdTestPolicy(20 * time.Second)
	rig := newHoldRig(t, holdRigOptions{hold: policy, upstream: noLocalUpstream(t), exhausted: true,
		peers: []*RemotePool{boxA.pool, boxB.pool}})
	done := make(chan heldTurn, 1)
	go func() {
		resp, body, err := rig.turn(t, context.Background(), nil)
		if err != nil {
			t.Error(err)
			done <- heldTurn{}
			return
		}
		done <- heldTurn{resp.StatusCode, body}
	}()
	waitHeld(t, rig, policy, done)
	open := func(p *RemotePool) bool { return !p.breaker.allow(time.Now()) }
	waitFor(t, "both breakers to open", func() bool { return open(boxA.pool) && open(boxB.pool) })
	for _, p := range []*RemotePool{boxA.pool, boxB.pool} {
		p.breaker.mu.Lock()
		p.breaker.openUntil = time.Now().Add(-time.Millisecond)
		p.breaker.mu.Unlock()
	}
	select {
	case got := <-done:
		t.Fatalf("let go with %d %q after the breakers closed, while no peer could be dialed; logs:\n%s", got.status, got.body, rig.logs.String())
	case <-time.After(500 * time.Millisecond):
	}
	bringBack(&down, []*RemotePool{boxA.pool, boxB.pool})
	select {
	case got := <-done:
		rig.settle(t, 1)
		assertHeldThenServedOnce(t, rig, got, boxA, boxB)
	case <-time.After(10 * time.Second):
		t.Fatalf("no answer 10s after the peers came back; logs:\n%s", rig.logs.String())
	}
}

// The last resort moves on to the next peer when the first cannot be dialed:
// nothing was sent, so the second peer serves the turn at once, unheld.
func TestLastResortMovesOnWhenAPeerCannotBeDialed(t *testing.T) {
	boxA := newContractBox(t, eligibleBoxAccount("acct-a"))
	boxB := newContractBox(t, eligibleBoxAccount("acct-b"))
	var downA, downB atomic.Bool
	downA.Store(true)
	switchablePeer(t, boxA.pool, &downA, false)
	switchablePeer(t, boxB.pool, &downB, false)
	rig := newHoldRig(t, holdRigOptions{hold: holdTestPolicy(20 * time.Second), upstream: noLocalUpstream(t), exhausted: true,
		peers: []*RemotePool{boxA.pool, boxB.pool}})
	resp, body, err := rig.turn(t, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rig.settle(t, 1)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "msg_box") {
		t.Fatalf("status = %d body = %q, want peer B's 200; logs:\n%s", resp.StatusCode, body, rig.logs.String())
	}
	if len(boxA.seen()) != 0 || len(boxB.seen()) != 1 {
		t.Fatalf("A saw %d turns and B %d, want 0 and 1", len(boxA.seen()), len(boxB.seen()))
	}
	if rig.countLog("proxy hold started") != 0 {
		t.Fatalf("held a turn a reachable peer could serve; logs:\n%s", rig.logs.String())
	}
}
