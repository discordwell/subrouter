package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/session"
)

// A stranded chat waits. A warm session whose account cannot be reached
// waits for it under the hold, across the client's retries, until the account
// answers, its box is found down, or it gives up (PATCHES §67).

// movableClock is the hold's clock (HoldPolicy.now). It stands still unless
// the test moves it. reads counts the hold reading it, which every attempt
// does.
type movableClock struct{ at, reads atomic.Int64 }

func newMovableClock(at time.Time) *movableClock {
	c := &movableClock{}
	c.set(at)
	return c
}

func (c *movableClock) set(at time.Time) { c.at.Store(at.UnixNano()) }
func (c *movableClock) now() time.Time {
	c.reads.Add(1)
	return time.Unix(0, c.at.Load())
}

var warmWaitStart = time.Date(2026, 11, 19, 12, 46, 42, 0, time.UTC)

// promptly is a client that gives up after 10 s, so a turn held by mistake
// fails its test instead of hanging it.
func promptly(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// strandedPolicy is holdTestPolicy with the stranded give-up at its default,
// on clock when given.
func strandedPolicy(clock *movableClock) *HoldPolicy {
	policy := holdTestPolicy(DefaultHoldDeadline)
	policy.GiveUp = DefaultStrandedGiveUp
	if clock != nil {
		policy.now = clock.now
	}
	return policy
}

// seenSessions is a tracker that has run for hours and saw hold-session's
// last turn ago (never, for a negative ago).
func seenSessions(ago time.Duration) *ActiveSessions {
	active := NewActiveSessions()
	active.startedAt = time.Now().Add(-3 * time.Hour)
	if ago >= 0 {
		active.lastSeen[session.ScopedSessionKey("claude", "hold-session")] = time.Now().Add(-ago)
	}
	return active
}

// warmPeerRig is the laptop of 11/19: hold-session is held on box A's
// acct-a, box B has acct-b, which ranks above acct-a, and the local pool has
// its own account. local counts the turns the local upstream served.
type warmPeerRig struct {
	*holdRig
	boxA, boxB *contractBox
	dialsA     *atomic.Int64
	local      *atomic.Int64
}

func newWarmPeerRig(t *testing.T, policy *HoldPolicy, localSpent bool, failA func(n int64) error) *warmPeerRig {
	t.Helper()
	return newWarmPeerRigGated(t, policy, localSpent, failA, nil)
}

func newWarmPeerRigGated(t *testing.T, policy *HoldPolicy, localSpent bool, failA func(n int64) error, gate *admissionControl) *warmPeerRig {
	t.Helper()
	behind := eligibleBoxAccount("acct-a")
	behind.Score.Headroom, behind.Score.WeeklyHeadroom, behind.Score.WeeklyRemaining = 0.5, 0.5, 0.5
	boxA := newContractBox(t, behind)
	boxB := newContractBox(t, fourRefugeeBoxAccount("acct-b"))
	dialsA := cutPeer(t, boxA.pool, failA)
	cutPeer(t, boxB.pool, func(int64) error { return nil })
	local := new(atomic.Int64)
	rig := newHoldRig(t, holdRigOptions{
		hold: policy,
		upstream: func(w http.ResponseWriter, r *http.Request) {
			local.Add(1)
			answerOK(w, r)
		},
		exhausted: localSpent,
		peers:     []*RemotePool{boxA.pool, boxB.pool},
		admission: gate,
	})
	holdOn(t, boxA.pool, "hold-session", "acct-a")
	return &warmPeerRig{holdRig: rig, boxA: boxA, boxB: boxB, dialsA: dialsA, local: local}
}

func fourRefugeeBoxAccount(id string) boxAccount {
	account := eligibleBoxAccount(id)
	account.Score.Headroom, account.Score.WeeklyHeadroom, account.Score.WeeklyRemaining = 0.45, 0.45, 0.45
	return account
}

// answeredByAcctA checks the client got acct-a's answer from box A, and that
// neither the local pool nor box B was asked.
func (rig *warmPeerRig) answeredByAcctA(t *testing.T, resp *http.Response, body string) {
	t.Helper()
	logs := rig.logs.String()
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"served_by":"acct-a"`) {
		t.Fatalf("status = %d body = %q, want acct-a's answer once box A was back; logs:\n%s", resp.StatusCode, body, logs)
	}
	if turns := rig.boxA.seen(); len(turns) != 1 || turns[0].Target != "acct-a" {
		t.Fatalf("box A saw %+v, want the one turn, targeted at acct-a", turns)
	}
	if turns := rig.boxB.seen(); len(turns) != 0 {
		t.Fatalf("box B saw %+v, want nothing: the session's cache is on acct-a", turns)
	}
	if n := rig.local.Load(); n != 0 {
		t.Fatalf("the local upstream served %d turns, want none: the session's cache is on acct-a", n)
	}
	if account, held := holdAccount(rig.boxA.pool, "hold-session"); !held || account != "acct-a" {
		t.Fatalf("hold = %q, %v; want the session still held on acct-a", account, held)
	}
}

// A warm session's box cannot be dialed for a moment. The turn waits under
// the hold and gets acct-a's answer once a dial gets through, whether the
// local pool could have served it (the move to a local account) or is
// spent (the move to another box account).
func TestWarmPeerSessionWaitsForItsAccount(t *testing.T) {
	failures := []struct {
		cause string
		err   func(*testing.T) error
	}{
		{"dial_timeout", realDialTimeout},
		{"connection_refused", func(*testing.T) error { return refusedFailure() }},
		{"dns", func(*testing.T) error { return dnsFailure("box.example") }},
	}
	for _, local := range []struct {
		name  string
		spent bool
	}{{"local can serve", false}, {"local is spent", true}} {
		for _, failure := range failures {
			t.Run(local.name+"/"+failure.cause, func(t *testing.T) {
				rig := newWarmPeerRig(t, strandedPolicy(nil), local.spent, peerDownFor(1, failure.err(t)))
				resp, body, err := rig.turn(t, promptly(t), nil)
				if err != nil {
					t.Fatal(err)
				}
				rig.settle(t, 1)
				rig.answeredByAcctA(t, resp, body)
				if rig.dialsA.Load() != 2 {
					t.Fatalf("box A was dialed %d times, want 2: the failed dial and the one that got through", rig.dialsA.Load())
				}
				logs := rig.logs.String()
				if rig.countLog("proxy hold started") != 1 || !strings.Contains(logs, "reason=warm_wait:peer_unreachable") ||
					rig.countLog("outcome=answered") != 1 {
					t.Fatalf("want one hold naming the warm wait, answered; logs:\n%s", logs)
				}
			})
		}
	}
}

// After two strikes the breaker is open on an outage this machine cannot
// blame on the box, and the holds are kept. A warm session's turn is not
// dialed while the breaker is open; it waits, and goes to acct-a once the
// breaker lets it.
func TestWarmPeerSessionWaitsOutItsPeersOpenBreaker(t *testing.T) {
	for _, local := range []struct {
		name  string
		spent bool
	}{{"local can serve", false}, {"local is spent", true}} {
		t.Run(local.name, func(t *testing.T) {
			rig := newWarmPeerRig(t, strandedPolicy(nil), local.spent, func(int64) error { return nil })
			rig.boxA.pool.breaker.mu.Lock()
			rig.boxA.pool.breaker.openUntil = time.Now().Add(300 * time.Millisecond)
			rig.boxA.pool.breaker.mu.Unlock()
			resp, body, err := rig.turn(t, promptly(t), nil)
			if err != nil {
				t.Fatal(err)
			}
			rig.settle(t, 1)
			rig.answeredByAcctA(t, resp, body)
			if rig.dialsA.Load() != 1 {
				t.Fatalf("box A was dialed %d times, want 1: nothing is dialed while its breaker is open", rig.dialsA.Load())
			}
			logs := rig.logs.String()
			if rig.countLog("proxy hold started") != 1 || !strings.Contains(logs, "reason=warm_wait:peer_breaker_open") ||
				rig.countLog("outcome=answered") != 1 {
				t.Fatalf("want one hold naming the open breaker, answered; logs:\n%s", logs)
			}
		})
	}
}

// A session with nothing warm on the box moves at once, as today: a new
// session (no hold), one whose hold lapsed after the warm rule's hour,
// and one whose only hold is another request's pending claim (the box has
// served it nothing yet).
func TestColdPeerSessionMovesAtOnce(t *testing.T) {
	noHold := func(pool *RemotePool) {
		pool.delegations.clear(session.ScopedSessionKey("claude", "hold-session"))
	}
	lapsed := func(pool *RemotePool) {
		pool.delegations.set(session.ScopedSessionKey("claude", "hold-session"), time.Now().Add(-warmSessionIdleTTL-time.Minute),
			remotePoolDelegationTTLFor("claude"), "acct-a", pool.name())
	}
	claimed := func(pool *RemotePool) {
		key := session.ScopedSessionKey("claude", "hold-session")
		pool.delegations.clear(key)
		pool.delegations.claimTarget(key, time.Now(), remotePoolDelegationTTLFor("claude"), pool.name(), "acct-a")
	}
	cases := []struct {
		name  string
		spent bool
		hold  func(pool *RemotePool)
	}{
		{"new session", false, noHold},
		{"new session, local spent", true, noHold},
		{"hold lapsed", false, lapsed},
		{"hold lapsed, local spent", true, lapsed},
		{"pending claim", false, claimed},
		{"pending claim, local spent", true, claimed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newWarmPeerRig(t, strandedPolicy(nil), tc.spent, func(int64) error { return realDialTimeout(t) })
			tc.hold(rig.boxA.pool)
			started := time.Now()
			resp, body, err := rig.turn(t, promptly(t), nil)
			if err != nil {
				t.Fatalf("not answered at once (%v); logs:\n%s", err, rig.logs.String())
			}
			rig.settle(t, 1)
			if resp.StatusCode != http.StatusOK || strings.Contains(body, "acct-a") || time.Since(started) > 5*time.Second {
				t.Fatalf("status = %d body = %q after %v, want another account's answer at once", resp.StatusCode, body, time.Since(started))
			}
			if rig.countLog("proxy hold started") != 0 {
				t.Fatalf("a cold session was held; logs:\n%s", rig.logs.String())
			}
		})
	}
}

// A breaker opening this machine blames on the box (its own upstream answered
// during the outage) means the box is down: the warm session whose turn found
// it is a refugee and is placed at once, on box B before the local account
// (PATCHES §67, D3), with no hold.
func TestPeerJudgedDownPlacesItsWarmSessionAtOnce(t *testing.T) {
	rig := newWarmPeerRig(t, strandedPolicy(nil), false, func(int64) error { return realDialTimeout(t) })
	pool := rig.boxA.pool
	struck := time.Now().Add(-2 * time.Second)
	if opened, _, _ := pool.breaker.failure(struck, struck, realDialTimeout(t), &pool.egress, 0); opened {
		t.Fatal("calibration: one strike opened the breaker")
	}
	pool.egress.noteAnswerOnANewConnection(time.Now().Add(-time.Second))
	started := time.Now()
	resp, body, err := rig.turn(t, promptly(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	rig.settle(t, 1)
	logs := rig.logs.String()
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"served_by":"acct-b"`) || time.Since(started) > 5*time.Second {
		t.Fatalf("status = %d body = %q after %v, want acct-b's answer at once", resp.StatusCode, body, time.Since(started))
	}
	if !strings.Contains(logs, "peer_down=true") || rig.countLog("proxy hold started") != 0 {
		t.Fatalf("want box A judged down and no hold; logs:\n%s", logs)
	}
	refugeeHold(t, rig, rig.boxB.pool, "acct-b")
}

// A warm LOCAL session whose upstream cannot be dialed for a moment waits on
// its own account instead of being spilled to the box for one turn.
func TestWarmLocalSessionWaitsForItsUpstreamInsteadOfSpilling(t *testing.T) {
	for _, tc := range []struct {
		cause string
		fail  func(t *testing.T) error
	}{
		{"dns", func(*testing.T) error { return dnsFailure("api.anthropic.com") }},
		{"connection_refused", func(*testing.T) error { return refusedFailure() }},
		{"dial_timeout", realDialTimeout},
	} {
		t.Run(tc.cause, func(t *testing.T) {
			failure := tc.fail(t)
			rig := newHoldRig(t, holdRigOptions{
				hold:      strandedPolicy(nil),
				upstream:  answerOK,
				pin:       "one@example.com",
				active:    seenSessions(time.Minute),
				failDials: peerDownFor(2, failure),
				peer:      peerAnswer,
				spill:     spillTestPolicy(DefaultOutageSpillRate, DefaultOutageSpillBurst, standingClock()),
			})
			resp, body, err := rig.turn(t, promptly(t), nil)
			if err != nil {
				t.Fatal(err)
			}
			rig.settle(t, 1)
			logs := rig.logs.String()
			if resp.StatusCode != http.StatusOK || !strings.Contains(body, "msg_held") || rig.peerTurns.Load() != 0 {
				t.Fatalf("status = %d body = %q peer turns = %d, want the local account's answer and nothing spilled; logs:\n%s",
					resp.StatusCode, body, rig.peerTurns.Load(), logs)
			}
			if rig.countLog("outage spill") != 0 || rig.countLog("proxy hold started") != 1 ||
				!strings.Contains(logs, "reason=warm_wait:upstream_unreachable:"+tc.cause) || rig.countLog("outcome=answered") != 1 {
				t.Fatalf("want one warm-wait hold, answered, and no spill; logs:\n%s", logs)
			}
		})
	}
}

// A cold or new local session is spilled at once, as today.
func TestColdOrNewLocalSessionSpillsAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pin    string
		active *ActiveSessions
	}{
		{"new session", "", seenSessions(-1)},
		{"cold session", "one@example.com", seenSessions(2 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newHoldRig(t, holdRigOptions{
				hold:      strandedPolicy(nil),
				upstream:  answerOK,
				pin:       tc.pin,
				active:    tc.active,
				failDials: func(int64) error { return refusedFailure() },
				peer:      peerAnswer,
				spill:     spillTestPolicy(DefaultOutageSpillRate, DefaultOutageSpillBurst, standingClock()),
			})
			resp, body, err := rig.turn(t, promptly(t), nil)
			if err != nil {
				t.Fatal(err)
			}
			rig.settle(t, 1)
			if resp.StatusCode != http.StatusOK || !strings.Contains(body, "msg_peer") || rig.countLog("proxy hold started") != 0 {
				t.Fatalf("status = %d body = %q, want the box's answer at once and no hold; logs:\n%s", resp.StatusCode, body, rig.logs.String())
			}
		})
	}
}

// A new session's first attempt pins it and stamps it as seen, so its second
// attempt reads as warm. Warmth is judged when the session's failures begin:
// a new session that could not spill on its first attempt (no token) spills
// on the next one, and never waits.
func TestNewLocalSessionNeverWarmWaitsOnItsRetry(t *testing.T) {
	var refilled atomic.Bool
	bucketClock := func() time.Time {
		if refilled.Load() {
			return warmWaitStart.Add(time.Second)
		}
		return warmWaitStart
	}
	spill := spillTestPolicy(DefaultOutageSpillRate, 1, bucketClock)
	if !spill.take() {
		t.Fatal("calibration: the bucket starts empty")
	}
	rig := newHoldRig(t, holdRigOptions{
		hold:     strandedPolicy(nil),
		upstream: answerOK,
		failDials: func(n int64) error {
			if n == 2 {
				refilled.Store(true)
			}
			return refusedFailure()
		},
		peer:  peerAnswer,
		spill: spill,
	})
	resp, body, err := rig.turn(t, promptly(t), nil)
	if err != nil {
		t.Fatalf("the new session's turn was not answered (%v); logs:\n%s", err, rig.logs.String())
	}
	rig.settle(t, 1)
	logs := rig.logs.String()
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "msg_peer") {
		t.Fatalf("status = %d body = %q, want the box's answer; logs:\n%s", resp.StatusCode, body, logs)
	}
	if rig.countLog("outcome=capped") != 1 || strings.Contains(logs, "warm_wait") {
		t.Fatalf("want one capped spill, then the spill, and no warm wait; logs:\n%s", logs)
	}
}

// A warm local session's count_tokens can be the first of its requests to
// hit the blip. It spills as today and leaves the session's warm wait alone:
// the model turn after it still waits for the local account.
func TestSideRequestLeavesItsSessionsWarmWaitAlone(t *testing.T) {
	rig := newHoldRig(t, holdRigOptions{
		hold:      strandedPolicy(nil),
		upstream:  answerOK,
		pin:       "one@example.com",
		active:    seenSessions(time.Minute),
		failDials: peerDownFor(3, refusedFailure()),
		peer:      peerAnswer,
		spill:     spillTestPolicy(DefaultOutageSpillRate, DefaultOutageSpillBurst, standingClock()),
	})
	resp, body, err := rig.post(t, promptly(t), "/v1/messages/count_tokens", nil)
	if err != nil {
		t.Fatal(err)
	}
	rig.settle(t, 1)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "msg_peer") {
		t.Fatalf("count_tokens: status = %d body = %q, want the box's answer at once; logs:\n%s", resp.StatusCode, body, rig.logs.String())
	}
	resp, body, err = rig.turn(t, promptly(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	rig.settle(t, 1)
	logs := rig.logs.String()
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "msg_held") || rig.peerTurns.Load() != 1 {
		t.Fatalf("model turn: status = %d body = %q peer turns = %d, want the local account's answer after a warm wait; logs:\n%s",
			resp.StatusCode, body, rig.peerTurns.Load(), logs)
	}
	if !strings.Contains(logs, "reason=warm_wait:upstream_unreachable:connection_refused") {
		t.Fatalf("want the model turn's warm wait; logs:\n%s", logs)
	}
}

// A session held on box A keeps its cache on acct-a; its local pin is a
// placeholder. Once box A is found down, with the local upstream down too, it
// is placed on box B as a refugee. It does not wait for a local account that
// holds none of its cache.
func TestSessionWaitsOneWindowForTheAccountItsCacheIsOn(t *testing.T) {
	policy := strandedPolicy(nil)
	behind := eligibleBoxAccount("acct-a")
	behind.Score.Headroom, behind.Score.WeeklyHeadroom, behind.Score.WeeklyRemaining = 0.5, 0.5, 0.5
	boxA := newContractBox(t, behind)
	boxB := newContractBox(t, eligibleBoxAccount("acct-b"))
	cutPeer(t, boxA.pool, func(int64) error { return refusedFailure() })
	cutPeer(t, boxB.pool, func(int64) error { return nil })
	rig := newHoldRig(t, holdRigOptions{
		hold:      policy,
		upstream:  answerOK,
		pin:       "one@example.com",
		active:    seenSessions(time.Minute),
		failDials: func(int64) error { return refusedFailure() },
		peers:     []*RemotePool{boxA.pool, boxB.pool},
		spill:     spillTestPolicy(DefaultOutageSpillRate, DefaultOutageSpillBurst, standingClock()),
	})
	holdOn(t, boxA.pool, "hold-session", "acct-a")
	findDown(t, boxA.pool)
	got := notHeld(t, rig, policy, rig.sendAsync(t, holdTurnBody), "a turn of a chat on a box found down")
	rig.settle(t, 1)
	if got.status != http.StatusOK || !strings.Contains(got.body, `"served_by":"acct-b"`) {
		t.Fatalf("status = %d body = %q, want box B's answer; logs:\n%s", got.status, got.body, rig.logs.String())
	}
	if logs := rig.logs.String(); strings.Contains(logs, "warm_wait:upstream_unreachable") {
		t.Fatalf("the session waited for its local placeholder; logs:\n%s", logs)
	}
}

// The same rule where the local path does run for a session held on box A:
// box A is closed to this caller, so the turn is served locally and the hold
// is left as it is. With the local upstream down too, the turn is spilled to
// box B for this one turn (PATCHES §59); it does not wait for a local account
// that holds none of its cache.
func TestASessionHeldOnABoxClosedToItsCallerDoesNotWaitForItsLocalPlaceholder(t *testing.T) {
	policy := strandedPolicy(nil)
	boxA := newContractBox(t, eligibleBoxAccount("acct-a"))
	boxB := newContractBox(t, eligibleBoxAccount("acct-b"))
	_, elsewhere, err := net.ParseCIDR("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	boxA.pool.AllowFrom = []*net.IPNet{elsewhere}
	cutPeer(t, boxA.pool, func(int64) error { return nil })
	cutPeer(t, boxB.pool, func(int64) error { return nil })
	rig := newHoldRig(t, holdRigOptions{
		hold:      policy,
		upstream:  answerOK,
		pin:       "one@example.com",
		active:    seenSessions(time.Minute),
		failDials: func(int64) error { return refusedFailure() },
		peers:     []*RemotePool{boxA.pool, boxB.pool},
		spill:     spillTestPolicy(DefaultOutageSpillRate, DefaultOutageSpillBurst, standingClock()),
	})
	holdOn(t, boxA.pool, "hold-session", "acct-a")
	got := notHeld(t, rig, policy, rig.sendAsync(t, holdTurnBody), "a turn of a session held on a box closed to its caller")
	rig.settle(t, 1)
	if got.status != http.StatusOK || !strings.Contains(got.body, `"served_by":"acct-b"`) || len(boxA.seen()) != 0 {
		t.Fatalf("status = %d body = %q box A turns = %d, want box B's answer and nothing sent to box A; logs:\n%s",
			got.status, got.body, len(boxA.seen()), rig.logs.String())
	}
	if logs := rig.logs.String(); strings.Contains(logs, "warm_wait:upstream_unreachable") {
		t.Fatalf("the session waited for its local placeholder; logs:\n%s", logs)
	}
}

// A request that left for the box and then went unanswered may be running
// there, and it is never sent to the box again (PATCHES §58). The turn still
// waits for its account, past seven minutes and past its prompt cache,
// because the client's resend is a new request the box can take. Once the
// box is found down it is placed as a refugee (PATCHES §67).
func TestWarmWaitHoldsATurnThePeerMayBeRunningWithoutSendingItAgain(t *testing.T) {
	clock := newMovableClock(warmWaitStart)
	policy := strandedPolicy(clock)
	rig := newHoldRig(t, holdRigOptions{
		hold:     policy,
		upstream: answerOK,
		peer: func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
		},
	})
	cutPeer(t, rig.pool, func(int64) error { return nil })
	rig.pool.client.Transport.(*http.Transport).ResponseHeaderTimeout = 50 * time.Millisecond
	holdOn(t, rig.pool, "hold-session", "peer-acct")
	answer := rig.sendAsync(t, holdTurnBody)
	awaitHeld(t, rig, policy, answer, "a turn the box may be running")
	heldFor(t, rig, answer, attempts{count: clock.reads.Load, kick: func() {}}, 50, "at first")
	clock.set(warmWaitStart.Add(7*time.Minute + time.Second))
	heldFor(t, rig, answer, attempts{count: clock.reads.Load, kick: func() {}}, 50, "past seven minutes and the prompt cache")
	rig.pool.breaker.mu.Lock()
	rig.pool.breaker.down = true
	rig.pool.breaker.mu.Unlock()
	got := <-answer
	rig.settle(t, 1)
	if got.err != nil || got.status != http.StatusOK || !strings.Contains(got.body, "msg_held") {
		t.Fatalf("status = %d body = %q err = %v, want the local answer once the box was found down; logs:\n%s", got.status, got.body, got.err, rig.logs.String())
	}
	if rig.peerTurns.Load() != 1 || !strings.Contains(rig.logs.String(), "reason=warm_wait:") {
		t.Fatalf("peer turns = %d, want 1 and a stranded wait; logs:\n%s", rig.peerTurns.Load(), rig.logs.String())
	}
}

// What a stranded chat's wait never changes, with the session warm on box A
// and box A refusing every dial: a side request (count_tokens), a canary
// (X-Subrouter-No-Retry) and a caller box A is not for get exactly the answer
// they get with holding off, with box A's breaker closed or open. A
// turn the hold has no room for gets a 502 (PATCHES §67;
// TestEveryHoldAnswersAtItsDeadlineWithAStatusClaudeCodeRetries).
func TestWarmWaitLeavesOtherTurnsAsTheyWere(t *testing.T) {
	for _, tc := range []struct {
		name        string
		path        string
		extra       http.Header
		maxRequests int
		breakerOpen bool
		allowFrom   string
	}{
		{"count_tokens", "/v1/messages/count_tokens", nil, 8, false, ""},
		{"count_tokens, breaker open", "/v1/messages/count_tokens", nil, 8, true, ""},
		{"canary", "/v1/messages", http.Header{subrouterNoRetryHeader: {"1"}}, 8, false, ""},
		{"box not for this caller, breaker open", "/v1/messages", nil, 8, true, "100.64.0.0/10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := func(holding bool) (int, string, *warmPeerRig) {
				var policy *HoldPolicy
				if holding {
					policy = strandedPolicy(nil)
					policy.MaxRequests = tc.maxRequests
				}
				rig := newWarmPeerRig(t, policy, false, func(int64) error { return refusedFailure() })
				if tc.breakerOpen {
					rig.boxA.pool.breaker.mu.Lock()
					rig.boxA.pool.breaker.openUntil = time.Now().Add(time.Minute)
					rig.boxA.pool.breaker.mu.Unlock()
				}
				if tc.allowFrom != "" {
					rig.boxA.pool.AllowFrom = parseAllowFrom(tc.allowFrom)
				}
				resp, body, err := rig.post(t, promptly(t), tc.path, tc.extra)
				if err != nil {
					t.Fatalf("not answered at once (%v); logs:\n%s", err, rig.logs.String())
				}
				rig.settle(t, 1)
				return resp.StatusCode, body, rig
			}
			offStatus, offBody, _ := run(false)
			status, body, rig := run(true)
			if status != offStatus || body != offBody || status != http.StatusOK {
				t.Fatalf("with the hold %d %q, without %d %q: want the same local answer", status, body, offStatus, offBody)
			}
			if rig.countLog("proxy hold started") != 0 {
				t.Fatalf("a hold started; logs:\n%s", rig.logs.String())
			}
		})
	}
}

// The client closing the connection ends a warm wait, and nothing else is
// asked on its behalf. A client that leaves while an attempt is dialing the
// box (Claude Code abandoning a silent request) leaves the session held on
// acct-a, so its resend waits there too.
func TestWarmWaitEndsWhenTheClientGoesAway(t *testing.T) {
	for _, tc := range []struct {
		name    string
		midDial bool
		spent   bool
	}{{"between attempts", false, false}, {"during a dial", true, false}, {"during a dial, local spent", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			dialing := make(chan struct{})
			var once atomic.Bool
			policy := strandedPolicy(nil)
			rig := newWarmPeerRig(t, policy, tc.spent, func(n int64) error {
				if tc.midDial && n == 2 && once.CompareAndSwap(false, true) {
					close(dialing)
					time.Sleep(200 * time.Millisecond)
				}
				return refusedFailure()
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clientDone := make(chan error, 1)
			go func() {
				_, _, err := rig.turn(t, ctx, nil)
				clientDone <- err
			}()
			waitFor(t, "the warm wait", func() bool { return policy.Active() == 1 })
			if tc.midDial {
				select {
				case <-dialing:
				case <-time.After(10 * time.Second):
					t.Fatal("the second attempt never dialed box A")
				}
			}
			cancel()
			if err := <-clientDone; err == nil {
				t.Fatal("the client gave up; Do should report it")
			}
			rig.settle(t, 1)
			if policy.Active() != 0 || !strings.Contains(rig.logs.String(), "outcome=client_canceled") {
				t.Fatalf("active holds = %d, want 0 and a client_canceled end; logs:\n%s", policy.Active(), rig.logs.String())
			}
			if rig.local.Load() != 0 || len(rig.boxB.seen()) != 0 {
				t.Fatalf("local turns = %d, box B turns = %d: nothing else is asked for a client that left", rig.local.Load(), len(rig.boxB.seen()))
			}
			if account, held := holdAccount(rig.boxA.pool, "hold-session"); !held || account != "acct-a" {
				t.Fatalf("hold = %q, %v after the client left; want the session still held on acct-a; logs:\n%s", account, held, rig.logs.String())
			}
		})
	}
}

// A new session on a spent pool whose peers cannot be dialed is held as on
// the parent change, with the warm wait on: it has nothing warm anywhere.
func TestPeersUnreachableHoldIsUnchangedByTheWarmWait(t *testing.T) {
	boxA := newContractBox(t, eligibleBoxAccount("acct-a"))
	boxB := newContractBox(t, eligibleBoxAccount("acct-b"))
	cutPeer(t, boxA.pool, peerDownFor(1, realDialTimeout(t)))
	cutPeer(t, boxB.pool, peerDownFor(1, refusedFailure()))
	rig := newHoldRig(t, holdRigOptions{
		hold:      strandedPolicy(nil),
		upstream:  noLocalUpstream(t),
		exhausted: true,
		peers:     []*RemotePool{boxA.pool, boxB.pool},
	})
	resp, body, err := rig.turn(t, promptly(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	rig.settle(t, 1)
	logs := rig.logs.String()
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "msg_box") {
		t.Fatalf("status = %d body = %q, want a box's answer; logs:\n%s", resp.StatusCode, body, logs)
	}
	if rig.countLog("proxy hold started") != 1 || !strings.Contains(logs, "reason=peers_unreachable:pool_exhausted") || strings.Contains(logs, "warm_wait") {
		t.Fatalf("want the peers_unreachable hold and no warm wait; logs:\n%s", logs)
	}
}

// A turn answered starts the stranded chat's hour over (PATCHES §67). On
// the box, a chat answered at 12:46 and again at 13:36, then stranded at
// 14:31, waits until 14:36, an hour after the later answer. On the local
// upstream the later answer comes half a minute after the first, inside the
// minute the session store does not rewrite: the chat still waits a full
// hour from it.
func TestAnAnsweredTurnStartsTheHourOver(t *testing.T) {
	t.Run("box", func(t *testing.T) {
		later := warmWaitStart.Add(50 * time.Minute)
		clock := newMovableClock(warmWaitStart)
		policy := strandedPolicy(clock)
		rig, up := newSwitchedPeerRig(t, policy)
		answeredOnAcctA(t, rig)
		clock.set(later)
		answeredOnAcctA(t, rig)
		up.Store(false)
		stranded := strandedFrom(t, rig, policy, clock, later.Add(55*time.Minute), "a turn stranded 55 minutes after the later answer")
		clock.set(later.Add(warmSessionIdleTTL - time.Second))
		heldFor(t, rig.holdRig, stranded, boxAttempts(rig), 3, "a second before the hour from the later answer")
		clock.set(later.Add(warmSessionIdleTTL))
		gaveUp(t, rig.holdRig, answeredWithin(t, rig.holdRig, stranded, boxAttempts(rig), 20, "an hour after the later answer"), "an hour after the later answer")
	})
	t.Run("local upstream", func(t *testing.T) {
		later := warmWaitStart.Add(30 * time.Second)
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
		for _, at := range []time.Time{warmWaitStart, later} {
			clock.set(at)
			if got := <-rig.sendAsync(t, holdTurnBody); got.status != http.StatusOK {
				t.Fatalf("calibration: status = %d body = %q, want the local answer", got.status, got.body)
			}
			rig.settle(t, 1)
		}
		refusing.Store(true)
		clock.set(later.Add(55 * time.Minute))
		stranded := rig.sendAsync(t, holdTurnBody)
		awaitHeld(t, rig, policy, stranded, "a turn stranded 55 minutes after the later answer")
		clock.set(later.Add(warmSessionIdleTTL - time.Second))
		heldFor(t, rig, stranded, localAttempts(rig), 3, "a second before the hour from the later answer")
		clock.set(later.Add(warmSessionIdleTTL))
		gaveUp(t, rig, answeredWithin(t, rig, stranded, localAttempts(rig), 20, "an hour after the later answer"), "an hour after the later answer")
	})
}

// A run ends when one of the session's turns is answered or the chat gives
// up, so a session that never comes back would keep its run, its answered
// time and its chain of held turns for the life of the daemon. Runs and
// answered times are swept as ActiveSessions sweeps lastSeen, and a chain once
// it has had no held attempt for its gap.
func TestStrandedRunsAreBounded(t *testing.T) {
	policy := strandedPolicy(nil)
	policy.runs = map[string]strandedRun{}
	stale := time.Now().Add(-2 * lastSeenRetention)
	for i := 0; i < lastSeenSweepThreshold+10; i++ {
		policy.runs[fmt.Sprintf("claude:stale-%d", i)] = strandedRun{began: stale}
	}
	policy.localWarm("claude:live", false)
	if len(policy.runs) != 1 {
		t.Fatalf("runs = %d after the sweep, want only the live session's", len(policy.runs))
	}
	now := time.Now()
	policy.chains = map[string]session.HeldChain{}
	ended := now.Add(-policy.chainGap())
	for i := 0; i < lastSeenSweepThreshold+10; i++ {
		policy.chains[fmt.Sprintf("claude:stale-%d", i)] = session.HeldChain{Since: ended, Last: ended}
	}
	if chain := policy.heldAttempt("claude:live", now); !chain.Since.Equal(now) {
		t.Fatalf("a stranded session's first held attempt starts the chain %+v, want one from %v", chain, now)
	}
	if len(policy.chains) != 1 {
		t.Fatalf("chains = %d after the sweep, want only the live session's", len(policy.chains))
	}
	policy.answeredAt = map[string]time.Time{}
	for i := 0; i < lastSeenSweepThreshold+10; i++ {
		policy.answeredAt[fmt.Sprintf("claude:stale-%d", i)] = stale
	}
	policy.answered("claude:live", now)
	if len(policy.answeredAt) != 1 {
		t.Fatalf("answered times = %d after the sweep, want only the live session's", len(policy.answeredAt))
	}
}
