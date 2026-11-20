package proxy

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// PATCHES §67 on a session held on a box whose turn cannot reach it. A turn
// the box may be running is never sent to it again (PATCHES §58), and a side
// request never waits for the box. Each test drives the whole handler
// through the hold rig, with the box a contractBox that publishes verdicts
// and honours targets.

// peerWaitRig is a laptop with one local account and box S, holding
// hold-session on S's s-acct with every dial to S going through fail, and
// opts' hold. local counts the turns the local upstream served.
func peerWaitRig(t *testing.T, opts holdRigOptions, fail func(n int64) error) (rig *holdRig, box *contractBox, dials, local *atomic.Int64) {
	t.Helper()
	box = newContractBox(t, eligibleBoxAccount("s-acct"))
	dials = cutPeer(t, box.pool, fail)
	local = new(atomic.Int64)
	opts.upstream = func(w http.ResponseWriter, r *http.Request) {
		local.Add(1)
		answerOK(w, r)
	}
	opts.peers = []*RemotePool{box.pool}
	opts.peerDownGrace = DefaultPeerDownGrace
	rig = newHoldRig(t, opts)
	holdOn(t, box.pool, "hold-session", "s-acct")
	return rig, box, dials, local
}

func openBreaker(pool *RemotePool) {
	pool.breaker.mu.Lock()
	pool.breaker.openUntil = time.Now().Add(remotePoolCooldown)
	pool.breaker.failures = remotePoolFailuresToOpen - 1
	pool.breaker.unattributed = true
	pool.breaker.mu.Unlock()
}

// Across the hold's attempts. S took the turn and sent no headers before the
// timeout, so it may be running it. No later attempt sends the turn to S
// again, whether that timeout opened S's breaker or not: the turn waits for
// S's account (PATCHES §67), and once S is found down it is placed as a
// refugee, here on the local account.
func TestAHeldTurnItsBoxMayBeRunningIsNotSentToItOnALaterAttempt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		opens bool
	}{
		{"the timeout opens the breaker", true},
		{"the breaker stays closed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var local, peerSeen atomic.Int64
			clock := newMovableClock(warmWaitStart)
			policy := strandedPolicy(clock)
			rig := newHoldRig(t, holdRigOptions{
				hold: policy,
				upstream: func(w http.ResponseWriter, r *http.Request) {
					local.Add(1)
					answerOK(w, r)
				},
				peer: func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					if peerSeen.Add(1) == 1 {
						<-r.Context().Done()
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"id":"msg_box","type":"message"}`))
				},
				peerDownGrace: DefaultPeerDownGrace,
			})
			cutPeer(t, rig.pool, func(int64) error { return nil })
			rig.pool.client.Transport.(*http.Transport).ResponseHeaderTimeout = 50 * time.Millisecond
			holdOn(t, rig.pool, "hold-session", "peer-acct")
			if tc.opens {
				rig.pool.breaker.mu.Lock()
				rig.pool.breaker.failures = remotePoolFailuresToOpen - 1
				rig.pool.breaker.unattributed = true
				rig.pool.breaker.lastFailureAt = time.Now().Add(-time.Minute)
				rig.pool.breaker.lastStrikeStarted = time.Now().Add(-time.Minute)
				rig.pool.breaker.mu.Unlock()
			}
			answer := rig.sendAsync(t, holdTurnBody)
			awaitHeld(t, rig, policy, answer, "a turn the box may be running")
			heldFor(t, rig, answer, attempts{count: clock.reads.Load, kick: func() { e2eRewindCooldown(rig.pool) }}, 50, "while S is not found down")
			logs := rig.logs.String()
			if !strings.Contains(logs, "breaker_opened="+strconv.FormatBool(tc.opens)) || rig.countLog("proxy hold started") != 1 {
				t.Fatalf("calibration: want S's timeout to leave breaker_opened=%v and one hold; logs:\n%s", tc.opens, logs)
			}
			if n := peerSeen.Load(); n != 1 {
				t.Fatalf("the box saw the turn %d times, want once: the first attempt sent it and S may be running it; logs:\n%s", n, logs)
			}
			rig.pool.breaker.mu.Lock()
			rig.pool.breaker.down = true
			rig.pool.breaker.mu.Unlock()
			got := <-answer
			rig.settle(t, 1)
			if got.status != http.StatusOK || !strings.Contains(got.body, "msg_held") || local.Load() != 1 || peerSeen.Load() != 1 {
				t.Fatalf("status = %d body = %q local turns %d box %d, want the local upstream's 200 once S was found down; logs:\n%s",
					got.status, got.body, local.Load(), peerSeen.Load(), rig.logs.String())
			}
		})
	}
}

// U13 inside one attempt, when local cannot serve. The box takes the held turn
// and sends no headers before the timeout, which leaves its breaker closed,
// and every local account is spent. The box is never sent the turn again: it
// sees it once while the turn waits for its account (PATCHES §67), and the
// hold's deadline answers a 502 Claude Code retries, never the spent pool's
// 503.
func TestAHeldTurnItsBoxMayBeRunningIsNotSentToItWhenLocalCannotServe(t *testing.T) {
	var peerSeen atomic.Int64
	clock := newMovableClock(warmWaitStart)
	policy := strandedPolicy(clock)
	rig := newHoldRig(t, holdRigOptions{
		hold:      policy,
		upstream:  noLocalUpstream(t),
		exhausted: true,
		peer: func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			if peerSeen.Add(1) == 1 {
				<-r.Context().Done()
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_box","type":"message"}`))
		},
	})
	cutPeer(t, rig.pool, func(int64) error { return nil })
	rig.pool.client.Transport.(*http.Transport).ResponseHeaderTimeout = 50 * time.Millisecond
	holdOn(t, rig.pool, "hold-session", "peer-acct")
	answer := rig.sendAsync(t, holdTurnBody)
	awaitHeld(t, rig, policy, answer, "a turn the box may be running")
	heldFor(t, rig, answer, attempts{count: clock.reads.Load, kick: func() {}}, 50, "with local spent")
	logs := rig.logs.String()
	if !strings.Contains(logs, "breaker_opened=false") {
		t.Fatalf("calibration: want the box's timeout to leave its breaker closed; logs:\n%s", logs)
	}
	if n := peerSeen.Load(); n != 1 {
		t.Fatalf("the box saw the turn %d times, want once: it may be running it; logs:\n%s", n, logs)
	}
	clock.set(warmWaitStart.Add(DefaultHoldDeadline + time.Second))
	got := <-answer
	rig.settle(t, 1)
	retryable(t, got, http.StatusBadGateway)
	if account, held := holdAccount(rig.pool, "hold-session"); !held || account != "peer-acct" {
		t.Fatalf("hold = %q/%v, want peer-acct kept", account, held)
	}
}

// A side request (count_tokens) of a session held on a box, warm there, is
// served locally at once when it cannot reach the box, because the box's
// breaker is open or the dial fails. It never waits for the box (design C11).
func TestASideRequestOfAHeldSessionNeverWaitsForItsBox(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fail    func(t *testing.T) func(int64) error
		breaker bool
	}{
		{name: "the breaker is open", fail: func(*testing.T) func(int64) error { return func(int64) error { return nil } }, breaker: true},
		{name: "the dial fails", fail: func(t *testing.T) func(int64) error { return func(int64) error { return realDialTimeout(t) } }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig, box, _, local := peerWaitRig(t, holdRigOptions{hold: strandedPolicy(nil)}, tc.fail(t))
			if tc.breaker {
				openBreaker(box.pool)
			}
			resp, body, err := rig.post(t, context.Background(), "/v1/messages/count_tokens", nil)
			if err != nil {
				t.Fatal(err)
			}
			rig.settle(t, 1)
			logs := rig.logs.String()
			if strings.Contains(logs, "warm_wait") || rig.countLog("proxy hold started") != 0 {
				t.Fatalf("a side request waited for its box; logs:\n%s", logs)
			}
			if resp.StatusCode != http.StatusOK || !strings.Contains(body, "msg_held") || local.Load() != 1 {
				t.Fatalf("status = %d body = %q local %d, want the local upstream's 200; logs:\n%s", resp.StatusCode, body, local.Load(), logs)
			}
			if account, held := holdAccount(box.pool, "hold-session"); !held || account != "s-acct" {
				t.Fatalf("hold on S = %q/%v, want s-acct kept", account, held)
			}
		})
	}
}
