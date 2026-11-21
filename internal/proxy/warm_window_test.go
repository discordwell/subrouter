package proxy

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A stranded chat waits (PATCHES §67). A chat whose account cannot be reached
// waits for it under the hold, across the client's retries, until the account
// answers or the chat gives up an hour after its last answered turn. A chat
// held on a box becomes a refugee only once the box is found down; a chat on
// the local account stays there until it gives up. Whatever a hold answers at
// its deadline or its caps is a status Claude Code retries: it shows a 503
// whose Retry-After is over 60 s to the user and stops.

// holdTurnBody is the body rig.turn sends: no cache_control, so a 5 minute
// prompt cache.
const holdTurnBody = `{"model":"claude-opus-5-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

// oneHourCacheTurn is a main thread's turn: its system prompt is cached for
// an hour.
const oneHourCacheTurn = `{"model":"claude-opus-5-5","max_tokens":16,"system":[{"type":"text","text":"a main thread","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":"hi"}]}`

type clientAnswer struct {
	status     int
	body       string
	retryAfter string
	reason     string
	err        error
}

// sendAsync is one model turn of hold-session on its own goroutine.
func (rig *holdRig) sendAsync(t *testing.T, body string) <-chan clientAnswer {
	t.Helper()
	ctx := promptly(t)
	out := make(chan clientAnswer, 1)
	go func() {
		resp, text, err := rig.send(t, ctx, "/v1/messages", body, nil)
		if err != nil {
			out <- clientAnswer{err: err}
			return
		}
		out <- clientAnswer{status: resp.StatusCode, body: text, retryAfter: resp.Header.Get("Retry-After"), reason: resp.Header.Get(subrouterReasonHeader)}
	}()
	return out
}

// attempts counts something every attempt of the held turn does once: a
// dial refused. kick runs between polls; a peer rig uses it to let the box's
// breaker through, so every attempt dials it.
type attempts struct {
	count func() int64
	kick  func()
}

func boxAttempts(rig *warmPeerRig) attempts {
	return attempts{count: rig.dialsA.Load, kick: func() { e2eRewindCooldown(rig.boxA.pool) }}
}

func localAttempts(rig *holdRig) attempts {
	return attempts{count: rig.dials.Load, kick: func() {}}
}

// heldFor fails unless the turn is still unanswered after n more attempts.
func heldFor(t *testing.T, rig *holdRig, answer <-chan clientAnswer, a attempts, n int64, what string) {
	t.Helper()
	from := a.count()
	deadline := time.Now().Add(10 * time.Second)
	for a.count() < from+n {
		select {
		case got := <-answer:
			t.Fatalf("%s: answered %d %q (%v); want it still waiting for its account; logs:\n%s", what, got.status, got.body, got.err, rig.logs.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d attempts in 10 s, want %d; logs:\n%s", what, a.count()-from, n, rig.logs.String())
		}
		a.kick()
		time.Sleep(time.Millisecond)
	}
}

// answeredWithin returns the turn's answer, and fails if it is still waiting
// after n more attempts.
func answeredWithin(t *testing.T, rig *holdRig, answer <-chan clientAnswer, a attempts, n int64, what string) clientAnswer {
	t.Helper()
	from := a.count()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case got := <-answer:
			if got.err != nil {
				t.Fatalf("%s: %v; logs:\n%s", what, got.err, rig.logs.String())
			}
			return got
		default:
		}
		if a.count() >= from+n || time.Now().After(deadline) {
			t.Fatalf("%s: still waiting %d attempts later; logs:\n%s", what, a.count()-from, rig.logs.String())
		}
		a.kick()
		time.Sleep(time.Millisecond)
	}
}

// notHeld returns the turn's answer, and fails as soon as the hold has it.
func notHeld(t *testing.T, rig *holdRig, policy *HoldPolicy, answer <-chan clientAnswer, what string) clientAnswer {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case got := <-answer:
			if got.err != nil {
				t.Fatalf("%s: %v; logs:\n%s", what, got.err, rig.logs.String())
			}
			return got
		default:
		}
		if policy.Active() != 0 || time.Now().After(deadline) {
			t.Fatalf("%s: held, want an answer at once; logs:\n%s", what, rig.logs.String())
		}
		time.Sleep(time.Millisecond)
	}
}

// awaitHeld waits until the hold has the turn, and fails as soon as it is
// answered instead.
func awaitHeld(t *testing.T, rig *holdRig, policy *HoldPolicy, answer <-chan clientAnswer, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for policy.Active() != 1 {
		select {
		case got := <-answer:
			t.Fatalf("%s: answered %d %q (%v) instead of held; logs:\n%s", what, got.status, got.body, got.err, rig.logs.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: never held; logs:\n%s", what, rig.logs.String())
		}
		time.Sleep(time.Millisecond)
	}
}

// A subagent served by acct-a at 12:46:42 has a 5 minute cache there. Its box
// stops answering and its next turn waits for acct-a past the cache's end and
// past seven minutes, because the box is not found down: the end of a window
// no longer moves a chat (PATCHES §67). acct-a serves it once the box
// answers.
func TestAChatHeldOnABoxWaitsPastItsPromptCacheUntilTheBoxAnswers(t *testing.T) {
	clock := newMovableClock(warmWaitStart)
	policy := strandedPolicy(clock)
	var up atomic.Bool
	up.Store(true)
	rig := newWarmPeerRig(t, policy, false, func(int64) error {
		if up.Load() {
			return nil
		}
		return refusedFailure()
	})
	resp, body, err := rig.turn(t, promptly(t), nil)
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(body, `"served_by":"acct-a"`) {
		t.Fatalf("calibration: first turn %v %v %q, want acct-a's answer", err, resp, body)
	}
	rig.settle(t, 1)
	up.Store(false)

	clock.set(warmWaitStart.Add(3 * time.Minute))
	answer := rig.sendAsync(t, holdTurnBody)
	awaitHeld(t, rig.holdRig, policy, answer, "three minutes after acct-a's last answer")
	clock.set(warmWaitStart.Add(5*time.Minute + time.Second))
	heldFor(t, rig.holdRig, answer, boxAttempts(rig), 3, "a second after the cache expired")
	clock.set(warmWaitStart.Add(10*time.Minute + time.Second))
	heldFor(t, rig.holdRig, answer, boxAttempts(rig), 3, "seven minutes into the wait")
	up.Store(true)
	got := answeredWithin(t, rig.holdRig, answer, boxAttempts(rig), 20, "once box A lets a dial through")
	rig.settle(t, 1)
	if got.status != http.StatusOK || !strings.Contains(got.body, `"served_by":"acct-a"`) || rig.local.Load() != 0 || len(rig.boxB.seen()) != 0 {
		t.Fatalf("status = %d body = %q local %d box B %d, want acct-a's answer and nothing moved; logs:\n%s",
			got.status, got.body, rig.local.Load(), len(rig.boxB.seen()), rig.logs.String())
	}
}

// The same for a chat on the laptop's own account: its upstream cannot be
// dialed, and it waits past its cache and past seven minutes, and is never
// spilled to the box, which needs the same network (PATCHES §67).
func TestAChatOnTheLocalAccountWaitsPastItsPromptCacheAndIsNeverSpilled(t *testing.T) {
	clock := newMovableClock(warmWaitStart)
	policy := strandedPolicy(clock)
	var up atomic.Bool
	up.Store(true)
	rig := newHoldRig(t, holdRigOptions{
		hold:     policy,
		upstream: answerOK,
		pin:      "one@example.com",
		active:   seenSessions(time.Minute),
		failDials: func(int64) error {
			if up.Load() {
				return nil
			}
			return refusedFailure()
		},
		peer:  peerAnswer,
		spill: spillTestPolicy(DefaultOutageSpillRate, DefaultOutageSpillBurst, standingClock()),
	})
	resp, body, err := rig.turn(t, promptly(t), nil)
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(body, "msg_held") {
		t.Fatalf("calibration: first turn %v %v %q, want the local answer", err, resp, body)
	}
	rig.settle(t, 1)
	up.Store(false)

	clock.set(warmWaitStart.Add(3 * time.Minute))
	answer := rig.sendAsync(t, holdTurnBody)
	awaitHeld(t, rig, policy, answer, "three minutes after the local account's last answer")
	clock.set(warmWaitStart.Add(5*time.Minute + time.Second))
	heldFor(t, rig, answer, localAttempts(rig), 3, "a second after the cache expired")
	clock.set(warmWaitStart.Add(10*time.Minute + time.Second))
	heldFor(t, rig, answer, localAttempts(rig), 3, "seven minutes into the wait")
	up.Store(true)
	got := answeredWithin(t, rig, answer, localAttempts(rig), 20, "once the upstream lets a dial through")
	rig.settle(t, 1)
	if got.status != http.StatusOK || !strings.Contains(got.body, "msg_held") || rig.peerTurns.Load() != 0 || rig.countLog("outage spill") != 0 {
		t.Fatalf("status = %d body = %q peer turns = %d, want the local answer and no spill; logs:\n%s", got.status, got.body, rig.peerTurns.Load(), rig.logs.String())
	}
}

// findDown has box A's breaker find it down, the way two strikes do once
// this machine has opened a new connection during the outage, and leaves the
// hold on it in place.
func findDown(t *testing.T, pool *RemotePool) {
	t.Helper()
	struck := time.Now().Add(-2 * time.Second)
	if opened, _, _ := pool.breaker.failure(struck, struck, realDialTimeout(t), &pool.egress, 0); opened {
		t.Fatal("calibration: one strike opened the breaker")
	}
	pool.egress.noteAnswerOnANewConnection(time.Now().Add(-time.Second))
	now := time.Now()
	if opened, down, why := pool.breaker.failure(now, now, realDialTimeout(t), &pool.egress, 0); !opened || !down {
		t.Fatalf("calibration: the second strike opened=%v down=%v why=%q, want box A found down", opened, down, why)
	}
}

// A chat whose box was found down is a refugee: its turn is placed at once,
// on box B, with the breaker open and after its cooldown, when the next dial
// is refused too (PATCHES §67). A box that answers again after it was
// found down is waited for again on the next blip.
func TestAChatOnABoxFoundDownIsPlacedAsARefugeeAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before func(rig *warmPeerRig)
	}{
		{"the breaker is open", func(*warmPeerRig) {}},
		{"the breaker is past its cooldown", func(rig *warmPeerRig) { e2eRewindCooldown(rig.boxA.pool) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := strandedPolicy(nil)
			rig := newWarmPeerRig(t, policy, false, func(int64) error { return refusedFailure() })
			findDown(t, rig.boxA.pool)
			tc.before(rig)
			got := notHeld(t, rig.holdRig, policy, rig.sendAsync(t, holdTurnBody), "a turn of a chat on a box found down")
			rig.settle(t, 1)
			if got.status != http.StatusOK || !strings.Contains(got.body, `"served_by":"acct-b"`) || rig.countLog("proxy hold started") != 0 {
				t.Fatalf("status = %d body = %q, want acct-b's answer at once and no hold; logs:\n%s", got.status, got.body, rig.logs.String())
			}
			refugeeHold(t, rig, rig.boxB.pool, "acct-b")
		})
	}
	t.Run("the box answered since", func(t *testing.T) {
		policy := strandedPolicy(nil)
		var down atomic.Bool
		down.Store(true)
		rig := newWarmPeerRig(t, policy, false, func(int64) error {
			if down.Load() {
				return refusedFailure()
			}
			return nil
		})
		findDown(t, rig.boxA.pool)
		rig.boxA.pool.breaker.success(time.Now())
		answer := rig.sendAsync(t, holdTurnBody)
		awaitHeld(t, rig.holdRig, policy, answer, "a turn of a chat whose box answered after it was found down")
		down.Store(false)
		got := answeredWithin(t, rig.holdRig, answer, boxAttempts(rig), 20, "once box A lets the dial through")
		rig.settle(t, 1)
		if got.status != http.StatusOK || !strings.Contains(got.body, `"served_by":"acct-a"`) {
			t.Fatalf("status = %d body = %q, want acct-a's answer after the wait", got.status, got.body)
		}
	})
}

// retryable fails unless Claude Code retries the answer: a 502, or a 503 with
// a Retry-After of 60 s or less. A 502 with no Retry-After is retried with
// Claude Code's own backoff.
func retryable(t *testing.T, got clientAnswer, wantStatus int) {
	t.Helper()
	if got.err != nil {
		t.Fatalf("no answer: %v", got.err)
	}
	if got.status != wantStatus {
		t.Fatalf("status = %d Retry-After %q body = %q, want %d", got.status, got.retryAfter, got.body, wantStatus)
	}
	if got.retryAfter == "" {
		if wantStatus == http.StatusServiceUnavailable {
			t.Fatalf("a 503 with no Retry-After; body %q", got.body)
		}
		return
	}
	seconds, err := strconv.Atoi(got.retryAfter)
	if err != nil || seconds < 0 || seconds > 60 {
		t.Fatalf("Retry-After %q on a %d, want at most 60 s: Claude Code shows a longer one to the user and stops", got.retryAfter, got.status)
	}
}

// strandedOnBoxA is a chat warm on box A's acct-a, the laptop's only peer,
// with every dial to box A refused and every local account spent: nothing
// can serve it, and the pool-exhausted 503 carries the spent pool's reset,
// 90 minutes away.
func strandedOnBoxA(t *testing.T, policy *HoldPolicy) (*holdRig, *RemotePool) {
	t.Helper()
	boxA := newContractBox(t, eligibleBoxAccount("acct-a"))
	cutPeer(t, boxA.pool, func(int64) error { return refusedFailure() })
	rig := newHoldRig(t, holdRigOptions{hold: policy, upstream: noLocalUpstream(t), exhausted: true, peers: []*RemotePool{boxA.pool}})
	holdOn(t, boxA.pool, "hold-session", "acct-a")
	return rig, boxA.pool
}

// Every hold §67 adds or changes answers its deadline, or a hold with no
// room, with a status Claude Code retries: a stranded chat's wait on a box
// and on the local account, a refugee's wait for a slot, and 5a's full
// admission queue. A stranded wait never runs the turn once more at its end,
// where a spent pool would answer 503 with Retry-After: 5400 and a reachable
// box would take a local chat for one turn.
func TestEveryHoldAnswersAtItsDeadlineWithAStatusClaudeCodeRetries(t *testing.T) {
	t.Run("a stranded wait on a box, at the deadline", func(t *testing.T) {
		clock := newMovableClock(warmWaitStart)
		policy := strandedPolicy(clock)
		policy.Deadline = time.Minute
		rig, _ := strandedOnBoxA(t, policy)
		answer := rig.sendAsync(t, holdTurnBody)
		awaitHeld(t, rig, policy, answer, "the stranded wait")
		clock.set(warmWaitStart.Add(time.Minute + time.Second))
		got := <-answer
		rig.settle(t, 1)
		retryable(t, got, http.StatusBadGateway)
	})
	t.Run("a stranded wait on a box the hold has no room for", func(t *testing.T) {
		policy := strandedPolicy(nil)
		policy.MaxBytes = 16
		rig, _ := strandedOnBoxA(t, policy)
		got := <-rig.sendAsync(t, holdTurnBody)
		rig.settle(t, 1)
		retryable(t, got, http.StatusBadGateway)
	})
	t.Run("a stranded wait on the local account, at the deadline", func(t *testing.T) {
		clock := newMovableClock(warmWaitStart)
		policy := strandedPolicy(clock)
		policy.Deadline = time.Minute
		rig := newHoldRig(t, holdRigOptions{
			hold:      policy,
			upstream:  answerOK,
			pin:       "one@example.com",
			active:    seenSessions(time.Minute),
			failDials: func(int64) error { return refusedFailure() },
			peer:      peerAnswer,
			spill:     spillTestPolicy(DefaultOutageSpillRate, DefaultOutageSpillBurst, standingClock()),
		})
		answer := rig.sendAsync(t, holdTurnBody)
		awaitHeld(t, rig, policy, answer, "the stranded wait")
		clock.set(warmWaitStart.Add(time.Minute + time.Second))
		got := <-answer
		rig.settle(t, 1)
		retryable(t, got, http.StatusBadGateway)
		if n := rig.peerTurns.Load(); n != 0 {
			t.Fatalf("the box served %d turns, want none: a spill would send the chat's next turn back (D4)", n)
		}
	})
	t.Run("a refugee's wait for a slot, at the deadline", func(t *testing.T) {
		clock := newMovableClock(warmWaitStart)
		policy := strandedPolicy(clock)
		policy.Deadline = time.Minute
		rig, boxA := strandedOnBoxA(t, policy)
		findDown(t, boxA)
		answer := rig.sendAsync(t, holdTurnBody)
		awaitHeld(t, rig, policy, answer, "the refugee's wait")
		clock.set(warmWaitStart.Add(time.Minute + time.Second))
		got := <-answer
		rig.settle(t, 1)
		retryable(t, got, http.StatusBadGateway)
		if !strings.Contains(rig.logs.String(), "reason=refugee_overflow") {
			t.Fatalf("want the hold to be the refugee's; logs:\n%s", rig.logs.String())
		}
	})
	t.Run("a full admission queue, at the deadline", func(t *testing.T) {
		clock := newMovableClock(warmWaitStart)
		policy := holdTestPolicy(time.Minute)
		policy.MaxRequests = DefaultHoldMaxRequests
		policy.now = clock.now
		gate := newAdmissionControl()
		release := make(chan struct{})
		rig := newDrainedHoldRig(t, holdRigOptions{hold: policy, admission: gate, upstream: func(w http.ResponseWriter, r *http.Request) {
			<-release
			answerOK(w, r)
		}})
		var once sync.Once
		free := func() { once.Do(func() { close(release) }) }
		t.Cleanup(free)
		for range defaultAdmissionLimit + defaultAdmissionQueue {
			rig.sendAsync(t, holdTurnBody)
		}
		waitFor(t, "six turns in flight and 64 queued", func() bool {
			accounts := gate.snapshot().Accounts
			return len(accounts) == 1 && accounts[0].Active == defaultAdmissionLimit && accounts[0].Queued == defaultAdmissionQueue
		})
		answer := rig.sendAsync(t, holdTurnBody)
		awaitHeld(t, rig, policy, answer, "the turn the full queue turned away")
		clock.set(warmWaitStart.Add(time.Minute + time.Second))
		got := <-answer
		free()
		retryable(t, got, http.StatusServiceUnavailable)
	})
}

// A manual drain (POST /_subrouter/drain) ends a stranded hold with the 502
// the hold kept (PATCHES §67). The chat has no local pin, as a chat a box
// holds may not, and the local pool is spent: the drain's 503 to a new
// request and the spent pool's 503 with Retry-After: 5400 are both answers
// Claude Code would not retry the same way, and the held turn gets neither.
func TestADrainEndsAStrandedHoldWithA502(t *testing.T) {
	for _, tc := range []struct {
		name    string
		heldOnA bool
	}{
		{"a chat held on a box", true},
		{"a spent pool's turn no box could be asked for", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lifecycle := NewLifecycle()
			policy := strandedPolicy(nil)
			boxA := newContractBox(t, eligibleBoxAccount("acct-a"))
			cutPeer(t, boxA.pool, func(int64) error { return refusedFailure() })
			rig := newHoldRig(t, holdRigOptions{hold: policy, upstream: noLocalUpstream(t), exhausted: true,
				peers: []*RemotePool{boxA.pool}, lifecycle: lifecycle})
			if tc.heldOnA {
				holdOn(t, boxA.pool, "hold-session", "acct-a")
			}
			answer := rig.sendAsync(t, holdTurnBody)
			awaitHeld(t, rig, policy, answer, "the stranded wait")
			resp, err := http.Post(rig.daemon.URL+"/_subrouter/drain", "application/json", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || !lifecycle.Draining() {
				t.Fatalf("calibration: drain status = %d draining = %v", resp.StatusCode, lifecycle.Draining())
			}
			got := <-answer
			rig.settle(t, 2)
			retryable(t, got, http.StatusBadGateway)
			if !strings.Contains(rig.logs.String(), "outcome=draining") {
				t.Fatalf("want the hold ended by the drain; logs:\n%s", rig.logs.String())
			}
		})
	}
}

// A quiesce (POST /_subrouter/quiesce, a deploy's loopback stop) also ends a
// stranded hold with the 502 it kept, and runs no attempt after it: a quiesce
// admits nothing new, a hold's retry included. The hold's backoff is a second
// or more, so the quiesce comes while it waits.
func TestAQuiesceEndsAStrandedHoldWithA502AndRunsNoAttemptAfterIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		heldOnA bool
	}{
		{"a chat held on a box", true},
		{"a spent pool's turn no box could be asked for", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lifecycle := NewLifecycle()
			policy := strandedPolicy(nil)
			policy.baseBackoff, policy.maxBackoff = 2*time.Second, 2*time.Second
			boxA := newContractBox(t, eligibleBoxAccount("acct-a"))
			dials := cutPeer(t, boxA.pool, func(int64) error { return refusedFailure() })
			rig := newHoldRig(t, holdRigOptions{hold: policy, upstream: noLocalUpstream(t), exhausted: true,
				peers: []*RemotePool{boxA.pool}, lifecycle: lifecycle})
			if tc.heldOnA {
				holdOn(t, boxA.pool, "hold-session", "acct-a")
			}
			answer := rig.sendAsync(t, holdTurnBody)
			awaitHeld(t, rig, policy, answer, "the stranded wait")
			e2eRewindCooldown(boxA.pool)
			before := dials.Load()
			resp, err := http.Post(rig.daemon.URL+"/_subrouter/quiesce", "application/json", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || !lifecycle.Quiesced() {
				t.Fatalf("calibration: quiesce status = %d quiesced = %v", resp.StatusCode, lifecycle.Quiesced())
			}
			got := <-answer
			rig.settle(t, 2)
			retryable(t, got, http.StatusBadGateway)
			if after := dials.Load() - before; after != 0 {
				t.Fatalf("box A was dialed %d times after the quiesce, want no attempt after it; logs:\n%s", after, rig.logs.String())
			}
			if active := lifecycle.ActiveProxyRequests(); active != 0 {
				t.Fatalf("%d proxy requests still counted active after the hold ended", active)
			}
			if !strings.Contains(rig.logs.String(), "outcome=quiesced") {
				t.Fatalf("want the hold ended by the quiesce; logs:\n%s", rig.logs.String())
			}
		})
	}
}

// A quiesce that comes as a hold's retry starts does not turn that retry
// away: the hold admitted it first, so it runs, and the hold ends with the
// 502 it kept. The proxy's fault hook, which runs before the quiesce gate,
// quiesces at that moment.
func TestAHoldsRetryAdmittedBeforeAQuiesceRunsAndTheHoldEndsWithA502(t *testing.T) {
	lifecycle := NewLifecycle()
	var activeAtRetry atomic.Int64
	injectedProxyFault = func(r *http.Request) bool {
		if holdRetry(r) && !lifecycle.Quiesced() {
			activeAtRetry.Store(lifecycle.ActiveProxyRequests())
			lifecycle.Quiesce()
		}
		return false
	}
	t.Cleanup(func() { injectedProxyFault = nil })
	policy := strandedPolicy(nil)
	boxA := newContractBox(t, eligibleBoxAccount("acct-a"))
	cutPeer(t, boxA.pool, func(int64) error { return refusedFailure() })
	rig := newHoldRig(t, holdRigOptions{hold: policy, upstream: noLocalUpstream(t), exhausted: true,
		peers: []*RemotePool{boxA.pool}, lifecycle: lifecycle})
	holdOn(t, boxA.pool, "hold-session", "acct-a")
	got := <-rig.sendAsync(t, holdTurnBody)
	rig.settle(t, 1)
	retryable(t, got, http.StatusBadGateway)
	if !lifecycle.Quiesced() || !strings.Contains(rig.logs.String(), "outcome=draining") {
		t.Fatalf("calibration: quiesced = %v; want the retry run and the hold ended after it; logs:\n%s", lifecycle.Quiesced(), rig.logs.String())
	}
	if n := activeAtRetry.Load(); n != 1 {
		t.Fatalf("%d proxy requests counted active as the retry started, want the retry itself: a quiesce waits for what it counts", n)
	}
	if active := lifecycle.ActiveProxyRequests(); active != 0 {
		t.Fatalf("%d proxy requests still counted active after the hold ended", active)
	}
}
