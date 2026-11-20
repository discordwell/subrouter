package proxy

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/session"
)

// A stranded chat's chain of holds gives up an hour after its last answered
// turn (PATCHES §67; owner, 2026-11-19: "claude code should give up just
// after an hour, because cache", then "I'd rather they go unlinked at 60"):
// the clock that unlinks it. Before SUBROUTER_STRANDED_GIVE_UP, the warm
// window by default, each hold answers 502, which Claude Code retries; at it
// the chat gets a 503 with Retry-After: 3600, which Claude Code 2.1.329 shows
// at once, and is unlinked: its next turn is placed as a new session.

// gaveUp fails unless got is the give-up answer.
func gaveUp(t *testing.T, rig *holdRig, got clientAnswer, what string) {
	t.Helper()
	if got.err != nil || got.status != http.StatusServiceUnavailable || got.retryAfter != "3600" || got.reason != "stranded_gave_up" {
		t.Fatalf("%s: status = %d Retry-After %q reason %q body %q err %v, want 503, 3600, stranded_gave_up; logs:\n%s",
			what, got.status, got.retryAfter, got.reason, got.body, got.err, rig.logs.String())
	}
}

// newSwitchedPeerRig is the warm peer rig with box A answering while up is
// set, and refusing every dial otherwise.
func newSwitchedPeerRig(t *testing.T, policy *HoldPolicy) (*warmPeerRig, *atomic.Bool) {
	t.Helper()
	up := new(atomic.Bool)
	up.Store(true)
	rig := newWarmPeerRig(t, policy, false, func(int64) error {
		if up.Load() {
			return nil
		}
		return refusedFailure()
	})
	return rig, up
}

// answeredOnAcctA is one of hold-session's turns that box A's acct-a answers.
func answeredOnAcctA(t *testing.T, rig *warmPeerRig) {
	t.Helper()
	e2eRewindCooldown(rig.boxA.pool)
	resp, body, err := rig.turn(t, promptly(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	rig.settle(t, 1)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"served_by":"acct-a"`) {
		t.Fatalf("calibration: status = %d body = %q, want acct-a's answer; logs:\n%s", resp.StatusCode, body, rig.logs.String())
	}
}

// strandedFrom holds a turn of hold-session from at, with box A refusing.
func strandedFrom(t *testing.T, rig *warmPeerRig, policy *HoldPolicy, clock *movableClock, at time.Time, what string) <-chan clientAnswer {
	t.Helper()
	clock.set(at)
	answer := rig.sendAsync(t, holdTurnBody)
	awaitHeld(t, rig.holdRig, policy, answer, what)
	return answer
}

func TestAStrandedChatGivesUpAnHourAfterItsLastAnsweredTurn(t *testing.T) {
	clock := newMovableClock(warmWaitStart)
	policy := strandedPolicy(clock)
	rig, up := newSwitchedPeerRig(t, policy)
	answeredOnAcctA(t, rig)
	up.Store(false)

	first := strandedFrom(t, rig, policy, clock, warmWaitStart.Add(5*time.Minute), "the first stranded turn")
	clock.set(warmWaitStart.Add(5*time.Minute + DefaultHoldDeadline + time.Second))
	retryable(t, <-first, http.StatusBadGateway)
	rig.settle(t, 1)

	resent := strandedFrom(t, rig, policy, clock, warmWaitStart.Add(55*time.Minute), "the client's resend")
	clock.set(warmWaitStart.Add(warmSessionIdleTTL - time.Second))
	heldFor(t, rig.holdRig, resent, boxAttempts(rig), 3, "a second before the hour")
	clock.set(warmWaitStart.Add(warmSessionIdleTTL))
	gaveUp(t, rig.holdRig, answeredWithin(t, rig.holdRig, resent, boxAttempts(rig), 20, "an hour after the answered turn"), "an hour after the answered turn")
}

// The chat is unlinked at the give-up (PATCHES §67, R8): its next turn is
// placed as a new session and served, with no 503. On a box, that placement
// is not a refugee's and takes no slot, so it is made with every slot full.
// On the local account, the chat's pin goes too: a new session whose dial
// fails is not waited for, and its turn is spilled to the box.
func TestTheTurnAfterTheGiveUpIsPlacedAsANewSession(t *testing.T) {
	t.Run("a chat held on a box", func(t *testing.T) {
		clock := newMovableClock(warmWaitStart)
		policy := strandedPolicy(clock)
		rig, up := newSwitchedPeerRig(t, policy)
		answeredOnAcctA(t, rig)
		up.Store(false)
		resent := strandedFrom(t, rig, policy, clock, warmWaitStart.Add(59*time.Minute), "a turn a minute before the hour")
		clock.set(warmWaitStart.Add(warmSessionIdleTTL))
		gaveUp(t, rig.holdRig, answeredWithin(t, rig.holdRig, resent, boxAttempts(rig), 20, "at the hour"), "at the hour")
		rig.settle(t, 1)

		refugeeHoldsOn(rig.boxB.pool, "acct-b", refugeeSlots, time.Minute)
		localRefugeesOn(t, rig.holdRig, "one@example.com", refugeeSlots)
		got := notHeld(t, rig.holdRig, policy, rig.sendAsync(t, holdTurnBody), "the turn after the give-up")
		rig.settle(t, 1)
		if got.status != http.StatusOK || rig.countLog(refugeePlacedMessage) != 0 {
			t.Fatalf("status = %d body = %q, want it placed as a new session and served, not as a refugee; logs:\n%s", got.status, got.body, rig.logs.String())
		}
		if account, held := holdAccount(rig.boxA.pool, "hold-session"); held {
			t.Fatalf("still held on box A's %q after the give-up", account)
		}
	})
	t.Run("a chat on the local account", func(t *testing.T) {
		clock := newMovableClock(warmWaitStart)
		policy := strandedPolicy(clock)
		var refusing atomic.Bool
		rig := newHoldRig(t, holdRigOptions{
			hold:     policy,
			upstream: answerOK,
			pin:      "one@example.com",
			active:   seenSessions(time.Minute),
			failDials: func(int64) error {
				if refusing.Load() {
					return refusedFailure()
				}
				return nil
			},
			peer:  peerAnswer,
			spill: spillTestPolicy(DefaultOutageSpillRate, DefaultOutageSpillBurst, standingClock()),
		})
		if got := <-rig.sendAsync(t, holdTurnBody); got.status != http.StatusOK {
			t.Fatalf("calibration: status = %d body = %q, want the local answer", got.status, got.body)
		}
		rig.settle(t, 1)
		refusing.Store(true)
		clock.set(warmWaitStart.Add(59 * time.Minute))
		stranded := rig.sendAsync(t, holdTurnBody)
		awaitHeld(t, rig, policy, stranded, "a turn a minute before the hour")
		clock.set(warmWaitStart.Add(warmSessionIdleTTL))
		gaveUp(t, rig, answeredWithin(t, rig, stranded, localAttempts(rig), 20, "at the hour"), "at the hour")
		rig.settle(t, 1)

		got := notHeld(t, rig, policy, rig.sendAsync(t, holdTurnBody), "the turn after the give-up")
		rig.settle(t, 1)
		if got.status != http.StatusOK || !strings.Contains(got.body, "msg_peer") {
			t.Fatalf("status = %d body = %q, want it placed as a new session and spilled to the box; logs:\n%s", got.status, got.body, rig.logs.String())
		}
	})
}

// lapseHold ends a session's hold on pool as an hour idle does.
func lapseHold(pool *RemotePool, sessionID string) {
	key := session.ScopedSessionKey("claude", sessionID)
	pool.delegations.mu.Lock()
	defer pool.delegations.mu.Unlock()
	if hold, ok := pool.delegations.entries[key]; ok {
		hold.Until = time.Now().Add(-time.Second)
		pool.delegations.entries[key] = hold
	}
}

// The give-up and the unlink fall at the same hour, and the give-up wins
// (PATCHES §67, R8): a turn held across the hour is answered 503 there, and is
// not run once more to find its hold lapsed and be placed cold, uncapped. The
// hold's backoff is an hour, so its one wait ends at the hour, and the hold
// lapses during that wait.
func TestTheGiveUpWinsOverTheUnlinkAtTheHour(t *testing.T) {
	clock := newMovableClock(warmWaitStart)
	policy := strandedPolicy(clock)
	policy.baseBackoff, policy.maxBackoff = time.Hour, time.Hour
	hour := warmWaitStart.Add(warmSessionIdleTTL)
	rig, up := newSwitchedPeerRig(t, policy)
	answeredOnAcctA(t, rig)
	up.Store(false)
	resent := strandedFrom(t, rig, policy, clock, hour.Add(-200*time.Millisecond), "a turn just before the hour")
	lapseHold(rig.boxA.pool, "hold-session")
	clock.set(hour)
	gaveUp(t, rig.holdRig, <-resent, "at the hour its hold lapses")
	rig.settle(t, 1)
	if turns := rig.boxB.seen(); len(turns) != 0 || rig.local.Load() != 0 {
		t.Fatalf("box B saw %+v and the local upstream served %d; want the turn placed nowhere", turns, rig.local.Load())
	}
}

// A 5xx the hold asks again is not one of the chat's turns answered (PATCHES
// §67, R8): neither the local upstream's nor one a box relays starts the hour
// over, so a chat stranded later gives up an hour after its last 2xx.
func TestA5xxHeldAndAskedAgainDoesNotStartTheHourOver(t *testing.T) {
	t.Run("the local upstream's", func(t *testing.T) {
		clock := newMovableClock(warmWaitStart)
		policy := strandedPolicy(clock)
		var failing, refusing atomic.Bool
		rig := newHoldRig(t, holdRigOptions{
			hold: policy,
			upstream: func(w http.ResponseWriter, r *http.Request) {
				if failing.Load() {
					_, _ = io.Copy(io.Discard, r.Body)
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"upstream connect error"}}`))
					return
				}
				answerOK(w, r)
			},
			pin:    "one@example.com",
			active: seenSessions(time.Minute),
			failDials: func(int64) error {
				if refusing.Load() {
					return refusedFailure()
				}
				return nil
			},
			peer:  peerAnswer,
			spill: spillTestPolicy(DefaultOutageSpillRate, DefaultOutageSpillBurst, standingClock()),
		})
		if got := <-rig.sendAsync(t, holdTurnBody); got.status != http.StatusOK {
			t.Fatalf("calibration: status = %d body = %q, want the local answer", got.status, got.body)
		}
		rig.settle(t, 1)

		failing.Store(true)
		clock.set(warmWaitStart.Add(30 * time.Minute))
		failed := rig.sendAsync(t, holdTurnBody)
		awaitHeld(t, rig, policy, failed, "a turn the upstream answers 500")
		clock.set(warmWaitStart.Add(30*time.Minute + DefaultHoldDeadline + time.Second))
		if got := <-failed; got.status != http.StatusInternalServerError {
			t.Fatalf("calibration: status = %d body = %q, want the 500 the hold kept", got.status, got.body)
		}
		rig.settle(t, 1)

		failing.Store(false)
		refusing.Store(true)
		clock.set(warmWaitStart.Add(59 * time.Minute))
		stranded := rig.sendAsync(t, holdTurnBody)
		awaitHeld(t, rig, policy, stranded, "a turn whose upstream cannot be dialed")
		clock.set(warmWaitStart.Add(warmSessionIdleTTL))
		gaveUp(t, rig, answeredWithin(t, rig, stranded, localAttempts(rig), 20, "an hour after the last 2xx"), "an hour after the last 2xx")
	})
	t.Run("a box's, relayed", func(t *testing.T) {
		clock := newMovableClock(warmWaitStart)
		policy := strandedPolicy(clock)
		rig, up := newSwitchedPeerRig(t, policy)
		answeredOnAcctA(t, rig)

		rig.boxA.mu.Lock()
		rig.boxA.turnStatus = http.StatusBadGateway
		rig.boxA.mu.Unlock()
		failed := strandedFrom(t, rig, policy, clock, warmWaitStart.Add(30*time.Minute), "a turn box A answers 502")
		clock.set(warmWaitStart.Add(30*time.Minute + DefaultHoldDeadline + time.Second))
		retryable(t, <-failed, http.StatusBadGateway)
		rig.settle(t, 1)
		if !strings.Contains(rig.logs.String(), "reason=peer_status:502") {
			t.Fatalf("calibration: want the relayed 502 held; logs:\n%s", rig.logs.String())
		}

		rig.boxA.mu.Lock()
		rig.boxA.turnStatus = 0
		rig.boxA.mu.Unlock()
		up.Store(false)
		stranded := strandedFrom(t, rig, policy, clock, warmWaitStart.Add(59*time.Minute), "a turn box A cannot be dialed for")
		clock.set(warmWaitStart.Add(warmSessionIdleTTL))
		gaveUp(t, rig.holdRig, answeredWithin(t, rig.holdRig, stranded, boxAttempts(rig), 20, "an hour after the last 2xx"), "an hour after the last 2xx")
	})
}

// The give-up's clock is the session store's (PATCHES §67, R8): a chat
// answered by one daemon and stranded on another that shares its
// sessions.json, as a supervisor's next worker does, gives up an hour after
// that answer, not an hour after the second daemon first held it.
func TestTheGiveUpClockIsKeptInTheSessionStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	first := newHoldRig(t, holdRigOptions{
		hold:         strandedPolicy(newMovableClock(warmWaitStart)),
		upstream:     answerOK,
		pin:          "one@example.com",
		active:       seenSessions(time.Minute),
		sessionsPath: path,
	})
	if got := <-first.sendAsync(t, holdTurnBody); got.status != http.StatusOK {
		t.Fatalf("calibration: status = %d body = %q, want the first daemon's answer", got.status, got.body)
	}
	first.settle(t, 1)

	clock := newMovableClock(warmWaitStart.Add(55 * time.Minute))
	policy := strandedPolicy(clock)
	next := newHoldRig(t, holdRigOptions{
		hold:         policy,
		upstream:     answerOK,
		active:       seenSessions(time.Minute),
		failDials:    func(int64) error { return refusedFailure() },
		peer:         peerAnswer,
		spill:        spillTestPolicy(DefaultOutageSpillRate, DefaultOutageSpillBurst, standingClock()),
		sessionsPath: path,
	})
	stranded := next.sendAsync(t, holdTurnBody)
	awaitHeld(t, next, policy, stranded, "the next daemon's stranded turn")
	clock.set(warmWaitStart.Add(warmSessionIdleTTL - time.Second))
	heldFor(t, next, stranded, localAttempts(next), 3, "a second before the hour")
	clock.set(warmWaitStart.Add(warmSessionIdleTTL))
	gaveUp(t, next, answeredWithin(t, next, stranded, localAttempts(next), 20, "an hour after the first daemon's answer"), "an hour after the first daemon's answer")
}

// A chat with no answered turn on record counts from its first held turn,
// and the chain belongs to the chat, not to a request: a resend half a
// minute before the hour gives up at the hour, not an hour after the resend.
func TestTheStrandedGiveUpCountsFromTheChatsFirstHeldTurnAcrossResends(t *testing.T) {
	clock := newMovableClock(warmWaitStart)
	policy := strandedPolicy(clock)
	rig := newWarmPeerRig(t, policy, false, func(int64) error { return refusedFailure() })
	first, abandon := context.WithCancel(context.Background())
	defer abandon()
	firstDone := make(chan error, 1)
	go func() {
		_, _, err := rig.turn(t, first, nil)
		firstDone <- err
	}()
	waitFor(t, "the first request's hold", func() bool { return policy.Active() == 1 })
	abandon()
	if err := <-firstDone; err == nil {
		t.Fatal("the client abandoned the first request; Do should report it")
	}
	rig.settle(t, 1)

	clock.set(warmWaitStart.Add(DefaultStrandedGiveUp - 30*time.Second))
	resent := rig.sendAsync(t, holdTurnBody)
	awaitHeld(t, rig.holdRig, policy, resent, "the resend")
	clock.set(warmWaitStart.Add(DefaultStrandedGiveUp))
	gaveUp(t, rig.holdRig, <-resent, "the resend at the hour")
}

// An overflow chat gives up at the hour even when box B, with a slot, answers
// each of its placements with a transient 502 (its token refresh reached no
// verdict): an answer that serves nothing is not one of the chat's turns
// answered, so it does not start the clock over (R7).
func TestAnOverflowChatGivesUpAtTheHourThoughBoxBAnswersEachPlacementWithoutServingIt(t *testing.T) {
	clock := newMovableClock(warmWaitStart)
	policy := strandedPolicy(clock)
	rig := newWarmPeerRig(t, policy, false, func(int64) error { return refusedFailure() })
	localRefugeesOn(t, rig.holdRig, "one@example.com", refugeeSlots)
	rig.boxB.mu.Lock()
	rig.boxB.turnStatus, rig.boxB.turnTransient = http.StatusBadGateway, true
	rig.boxB.mu.Unlock()
	findDown(t, rig.boxA.pool)
	first := rig.sendAsync(t, holdTurnBody)
	awaitHeld(t, rig.holdRig, policy, first, "the overflow chat's first turn")
	placements := attempts{count: func() int64 { return int64(len(rig.boxB.seen())) }, kick: func() {}}
	heldFor(t, rig.holdRig, first, placements, 3, "box B answering each placement with a 502")
	clock.set(warmWaitStart.Add(DefaultHoldDeadline + time.Second))
	retryable(t, answeredWithin(t, rig.holdRig, first, placements, 20, "at its deadline"), http.StatusBadGateway)
	rig.settle(t, 1)

	clock.set(warmWaitStart.Add(DefaultStrandedGiveUp + time.Minute))
	got := notHeld(t, rig.holdRig, policy, rig.sendAsync(t, holdTurnBody), "a retry past the hour")
	rig.settle(t, 1)
	gaveUp(t, rig.holdRig, got, "a retry past the hour")
}

// A hold sleeps no later than the give-up: with a backoff far longer than the
// time left, the turn is answered at the hour, not at its next attempt.
func TestAHeldTurnGivesUpAtTheHourNotAtItsNextBackoff(t *testing.T) {
	clock := newMovableClock(warmWaitStart)
	policy := strandedPolicy(clock)
	policy.GiveUp = time.Minute
	policy.baseBackoff, policy.maxBackoff = time.Hour, time.Hour
	rig, _ := strandedOnBoxA(t, policy)
	first, abandon := context.WithCancel(context.Background())
	defer abandon()
	firstDone := make(chan error, 1)
	go func() {
		_, _, err := rig.turn(t, first, nil)
		firstDone <- err
	}()
	waitFor(t, "the first request's hold", func() bool { return policy.Active() == 1 })
	abandon()
	<-firstDone
	rig.settle(t, 1)

	clock.set(warmWaitStart.Add(time.Minute - 10*time.Millisecond))
	resent := rig.sendAsync(t, holdTurnBody)
	awaitHeld(t, rig, policy, resent, "the resend just before the give-up")
	clock.set(warmWaitStart.Add(time.Minute))
	gaveUp(t, rig, <-resent, "the resend at the give-up")
}

// The give-up covers every hold of a stranded chat: on a box (above), on the
// local account and for a refugee slot. It does not cover a hold for a full
// admission queue, which is local load, not a stranded chat.
func TestTheStrandedGiveUpCoversTheLocalWaitAndTheRefugeesWaitButNotAFullQueue(t *testing.T) {
	short := func(clock *movableClock) *HoldPolicy {
		policy := strandedPolicy(clock)
		policy.GiveUp = time.Minute
		return policy
	}
	t.Run("the local account", func(t *testing.T) {
		clock := newMovableClock(warmWaitStart)
		policy := short(clock)
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
		awaitHeld(t, rig, policy, answer, "the local wait")
		clock.set(warmWaitStart.Add(time.Minute))
		gaveUp(t, rig, <-answer, "the local wait at its give-up")
	})
	t.Run("a refugee's wait for a slot", func(t *testing.T) {
		clock := newMovableClock(warmWaitStart)
		policy := short(clock)
		rig, boxA := strandedOnBoxA(t, policy)
		findDown(t, boxA)
		answer := rig.sendAsync(t, holdTurnBody)
		awaitHeld(t, rig, policy, answer, "the refugee's wait")
		clock.set(warmWaitStart.Add(time.Minute))
		gaveUp(t, rig, <-answer, "the refugee's wait at its give-up")
	})
	t.Run("a full admission queue", func(t *testing.T) {
		clock := newMovableClock(warmWaitStart)
		policy := short(clock)
		policy.Deadline = 5 * time.Minute
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
		clock.set(warmWaitStart.Add(2 * time.Minute))
		heldFor(t, rig, answer, attempts{count: clock.reads.Load, kick: func() {}}, 20, "past the stranded give-up")
		free()
		if got := <-answer; got.status != http.StatusOK || strings.Contains(rig.logs.String(), "stranded_gave_up") {
			t.Fatalf("status = %d reason %q, want the turn served once a slot freed, never given up; logs:\n%s", got.status, got.reason, rig.logs.String())
		}
	})
}
