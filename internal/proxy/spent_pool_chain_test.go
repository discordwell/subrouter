package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// A spent pool's chat waits for a box (PATCHES §67), and its chain of held
// turns gives up an hour after its first held turn. The chain is the run of
// held turns that Claude Code's resends of the hold's 502 carry. It ends when
// a model turn is answered, at the give-up, when a hold of it ends with an
// answer Claude Code does not resend (the box's own 503, a client that left),
// or when no held attempt came for a hold's deadline and 2 minutes. The
// session store keeps it beside the last answered turn, so a supervisor's
// next worker carries it on.

// spentPoolChainGap is how long a chain lasts with no held attempt: the hold
// deadline and 2 minutes. Claude Code resends a 502 within seconds.
const spentPoolChainGap = DefaultHoldDeadline + 2*time.Minute

// resendDelay is how long Claude Code waits before it resends a 502. Its
// first retries wait 0.5 s to 2.1 s.
const resendDelay = 2 * time.Second

// resentUntil is Claude Code resending a spent pool's turn that no box can be
// reached for. From at, each resend is held and answered 502 at its deadline,
// and the next goes out resendDelay later, while the next hold's deadline is
// not after until. It returns when the next resend goes out.
func resentUntil(t *testing.T, rig *holdRig, policy *HoldPolicy, clock *movableClock, at, until time.Time) time.Time {
	t.Helper()
	for !at.Add(policy.Deadline).After(until) {
		clock.set(at)
		answer := rig.sendAsync(t, holdTurnBody)
		awaitHeld(t, rig, policy, answer, "a resend at "+at.Sub(warmWaitStart).String())
		clock.set(at.Add(policy.Deadline))
		retryable(t, <-answer, http.StatusBadGateway)
		rig.settle(t, 1)
		at = at.Add(policy.Deadline + resendDelay)
	}
	return at
}

// givesUpAt sends the turn at at, and fails unless it is held a second
// before giveUpAt and gets the give-up that says says at giveUpAt.
func givesUpAt(t *testing.T, rig *holdRig, policy *HoldPolicy, clock *movableClock, tries attempts, at, giveUpAt time.Time, says string) {
	t.Helper()
	clock.set(at)
	answer := rig.sendAsync(t, holdTurnBody)
	awaitHeld(t, rig, policy, answer, "the last resend")
	clock.set(giveUpAt.Add(-time.Second))
	heldFor(t, rig, answer, tries, 3, "a second before the hour from the first held turn")
	clock.set(giveUpAt)
	gaveUpSaying(t, rig, answeredWithin(t, rig, answer, tries, 20, "an hour after the first held turn"),
		"an hour after the first held turn", says)
}

// spentPoolChat is hold-session on a spent local pool, with box A refusing
// every dial while out is set and refusing the next blips dials after that.
// Pinned, the session has an assignment, and the session store keeps its
// chain; unpinned, only this process does.
type spentPoolChat struct {
	*holdRig
	policy *HoldPolicy
	clock  *movableClock
	box    *contractBox
	out    *atomic.Bool
	blips  *atomic.Int64
	dials  *atomic.Int64
}

func newSpentPoolChat(t *testing.T, giveUp time.Duration, pinned bool, local http.HandlerFunc) *spentPoolChat {
	t.Helper()
	chat := &spentPoolChat{clock: newMovableClock(warmWaitStart), out: new(atomic.Bool), blips: new(atomic.Int64)}
	chat.out.Store(true)
	chat.policy = strandedPolicy(chat.clock)
	chat.policy.GiveUp = giveUp
	chat.box = newContractBox(t, eligibleBoxAccount("acct-a"))
	chat.dials = cutPeer(t, chat.box.pool, func(int64) error {
		if chat.out.Load() {
			return refusedFailure()
		}
		if chat.blips.Load() > 0 {
			chat.blips.Add(-1)
			return refusedFailure()
		}
		return nil
	})
	if local == nil {
		local = noLocalUpstream(t)
	}
	opts := holdRigOptions{hold: chat.policy, upstream: local, exhausted: true, peers: []*RemotePool{chat.box.pool}}
	if pinned {
		opts.pin = "one@example.com"
	}
	chat.holdRig = newHoldRig(t, opts)
	return chat
}

// tries counts the held turn's attempts, each a dial to box A.
func (c *spentPoolChat) tries() attempts {
	return attempts{count: c.dials.Load, kick: func() { e2eRewindCooldown(c.box.pool) }}
}

// heldAt sends a turn at at and waits until the hold has it.
func (c *spentPoolChat) heldAt(t *testing.T, at time.Time, what string) <-chan clientAnswer {
	t.Helper()
	c.clock.set(at)
	answer := c.sendAsync(t, holdTurnBody)
	awaitHeld(t, c.holdRig, c.policy, answer, what)
	return answer
}

// endsAtItsDeadline holds a turn at at and answers it 502 at its deadline,
// the chain's last held attempt.
func (c *spentPoolChat) endsAtItsDeadline(t *testing.T, at time.Time) time.Time {
	t.Helper()
	answer := c.heldAt(t, at, "the first held turn")
	c.clock.set(at.Add(c.policy.Deadline))
	retryable(t, <-answer, http.StatusBadGateway)
	c.settle(t, 1)
	return at.Add(c.policy.Deadline)
}

// stillHeldAt fails unless the held turn is still held at at.
func (c *spentPoolChat) stillHeldAt(t *testing.T, answer <-chan clientAnswer, at time.Time, what string) {
	t.Helper()
	c.clock.set(at)
	heldFor(t, c.holdRig, answer, c.tries(), 3, what)
}

var chainHomes = []struct {
	name   string
	pinned bool
}{{"in the session store", true}, {"in memory", false}}

// A spent pool's chat whose earlier hold ended at its deadline, with no
// answer and no give-up, comes back two hours later during another gateway
// blip. Its new turn is held for its box, and served when the box comes
// back, as a chat with no earlier hold is. It is not given up on its first
// attempt.
func TestASpentPoolChatBackAfterAnEarlierHoldIsHeldNotGivenUpAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name         string
		earlierHold  bool
		boxComesBack bool
	}{
		{"calibration: no earlier hold, its box comes back", false, true},
		{"after the earlier hold, its box stays out", true, false},
		{"after the earlier hold, its box comes back", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chat := newSpentPoolChat(t, DefaultStrandedGiveUp, true, nil)
			if tc.earlierHold {
				chat.endsAtItsDeadline(t, warmWaitStart)
			}
			later := warmWaitStart.Add(2 * time.Hour)
			if !tc.boxComesBack {
				chat.heldAt(t, later, "the turn two hours after the earlier hold")
				return
			}
			chat.out.Store(false)
			chat.blips.Store(1)
			chat.clock.set(later)
			got := answeredWithin(t, chat.holdRig, chat.sendAsync(t, holdTurnBody), chat.tries(), 20, "the turn two hours later")
			if got.status != http.StatusOK || !strings.Contains(got.body, `"served_by":"acct-a"`) {
				t.Fatalf("status = %d Retry-After %q reason %q body %q, want box A's 200; logs:\n%s",
					got.status, got.retryAfter, got.reason, got.body, chat.logs.String())
			}
		})
	}
}

// The chain ends when no held attempt of the session came for a hold's
// deadline and 2 minutes. With a 15 minute give-up, a turn a second inside
// that gap after the last held attempt carries the chain on and gets the
// give-up at once; a turn at the gap starts a new chain and is held.
func TestASpentPoolsChainEndsAfterAGapWithNoHeldAttempt(t *testing.T) {
	for _, home := range chainHomes {
		for _, tc := range []struct {
			name   string
			after  time.Duration
			gaveUp bool
		}{
			{"a second inside the gap", spentPoolChainGap - time.Second, true},
			{"at the gap", spentPoolChainGap, false},
		} {
			t.Run(home.name+"/"+tc.name, func(t *testing.T) {
				chat := newSpentPoolChat(t, 15*time.Minute, home.pinned, nil)
				lastHeld := chat.endsAtItsDeadline(t, warmWaitStart)
				if !tc.gaveUp {
					chat.heldAt(t, lastHeld.Add(tc.after), "a turn after the chain ended")
					return
				}
				chat.clock.set(lastHeld.Add(tc.after))
				gaveUpSaying(t, chat.holdRig, <-chat.sendAsync(t, holdTurnBody), "a resend inside the gap", gaveUpOnABox)
			})
		}
	}
}

// localPool is the rig's one local account, spent until a reset or not.
func localPool(spent bool) selectacct.Scheduler {
	score := selectacct.Score{AccountID: "one@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1}
	if spent {
		reset := time.Now().Add(90 * time.Minute)
		score = selectacct.Score{AccountID: "one@example.com", Provider: accounts.ProviderClaude, Fresh: true,
			WeeklyResetAt: reset, ExhaustedResetAt: reset}
	}
	return selectacct.NewScheduler([]selectacct.Score{score})
}

// An answered model turn ends the chain. The pool frees up for one turn,
// which its local account answers, and is spent again a minute later. The
// next held turn starts a new chain: it is still held when the first chain's
// 15 minute give-up comes.
func TestASpentPoolsChainEndsWhenATurnIsAnswered(t *testing.T) {
	chat := newSpentPoolChat(t, 15*time.Minute, true, answerOK)
	lastHeld := chat.endsAtItsDeadline(t, warmWaitStart)
	chat.sched.Set(localPool(false))
	chat.clock.set(lastHeld.Add(resendDelay))
	if got := <-chat.sendAsync(t, holdTurnBody); got.status != http.StatusOK || !strings.Contains(got.body, "msg_held") {
		t.Fatalf("calibration: status = %d body %q, want the local account's answer; logs:\n%s", got.status, got.body, chat.logs.String())
	}
	chat.settle(t, 1)
	chat.sched.Set(localPool(true))
	again := chat.heldAt(t, lastHeld.Add(time.Minute), "the next turn on the spent pool")
	chat.stillHeldAt(t, again, warmWaitStart.Add(15*time.Minute), "at the first chain's give-up")
}

// The give-up ends the chain. A turn the next second starts a new chain
// and is held, not given up at once.
func TestASpentPoolsChainEndsAtItsGiveUp(t *testing.T) {
	for _, home := range chainHomes {
		t.Run(home.name, func(t *testing.T) {
			chat := newSpentPoolChat(t, 5*time.Minute, home.pinned, nil)
			first := chat.heldAt(t, warmWaitStart, "the first held turn")
			chat.clock.set(warmWaitStart.Add(5 * time.Minute))
			gaveUpSaying(t, chat.holdRig, answeredWithin(t, chat.holdRig, first, chat.tries(), 20, "at the give-up"),
				"at the give-up", gaveUpOnABox)
			chat.settle(t, 1)
			chat.heldAt(t, warmWaitStart.Add(5*time.Minute+time.Second), "the turn after the give-up")
		})
	}
}

// A hold of the chain that ends with an answer Claude Code does not resend
// ends the chain: the box's own 503 that its pool is out, or a client
// that left. The next held turn, a minute later, starts a new chain and is
// still held when the first chain's 9 minute give-up comes.
func TestASpentPoolsChainEndsWithAnAnswerClaudeCodeDoesNotResend(t *testing.T) {
	ends := []struct {
		name string
		end  func(t *testing.T, chat *spentPoolChat)
	}{
		{"the box says its pool is out", func(t *testing.T, chat *spentPoolChat) {
			first := chat.heldAt(t, warmWaitStart, "the first held turn")
			chat.box.mu.Lock()
			chat.box.turnStatus = http.StatusServiceUnavailable
			chat.box.mu.Unlock()
			chat.clock.set(warmWaitStart.Add(time.Minute))
			chat.out.Store(false)
			got := answeredWithin(t, chat.holdRig, first, chat.tries(), 20, "the box's answer")
			if got.status != http.StatusServiceUnavailable || got.reason != "pool_exhausted" {
				t.Fatalf("calibration: status = %d reason %q body %q, want the spent pool's 503 once the box answered; logs:\n%s",
					got.status, got.reason, got.body, chat.logs.String())
			}
			chat.settle(t, 1)
			chat.out.Store(true)
		}},
		{"the client leaves", func(t *testing.T, chat *spentPoolChat) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			chat.clock.set(warmWaitStart)
			left := make(chan struct{})
			go func() {
				defer close(left)
				resp, _, err := chat.send(t, ctx, "/v1/messages", holdTurnBody, nil)
				if err == nil {
					t.Errorf("the client that left got %d", resp.StatusCode)
				}
			}()
			awaitHeld(t, chat.holdRig, chat.policy, nil, "the first held turn")
			chat.clock.set(warmWaitStart.Add(time.Minute))
			cancel()
			<-left
			chat.settle(t, 1)
			if chat.countLog("outcome=client_canceled") != 1 {
				t.Fatalf("calibration: want the hold ended by the client; logs:\n%s", chat.logs.String())
			}
		}},
	}
	for _, home := range chainHomes {
		for _, end := range ends {
			t.Run(home.name+"/"+end.name, func(t *testing.T) {
				chat := newSpentPoolChat(t, 9*time.Minute, home.pinned, nil)
				end.end(t, chat)
				again := chat.heldAt(t, warmWaitStart.Add(2*time.Minute), "the next held turn")
				chat.stillHeldAt(t, again, warmWaitStart.Add(9*time.Minute), "at the first chain's give-up")
			})
		}
	}
}

// The chain survives a move to a new worker. A spent pool's chat is held on
// one worker, whose resends carry it for 48 minutes; then a deploy hands
// Claude Code's next resend to the next worker, which shares the session
// store. It gives up an hour after the first worker's first held turn, not
// an hour after its own.
func TestASpentPoolsChainCarriesOnOnTheNextWorker(t *testing.T) {
	sessionsPath := filepath.Join(t.TempDir(), "sessions.json")
	box := newContractBox(t, eligibleBoxAccount("acct-a"))
	cutPeer(t, box.pool, func(int64) error { return refusedFailure() })
	firstClock := newMovableClock(warmWaitStart)
	firstPolicy := strandedPolicy(firstClock)
	first := newHoldRig(t, holdRigOptions{hold: firstPolicy, upstream: noLocalUpstream(t), exhausted: true,
		peers: []*RemotePool{box.pool}, pin: "one@example.com", sessionsPath: sessionsPath})
	at := resentUntil(t, first, firstPolicy, firstClock, warmWaitStart, warmWaitStart.Add(50*time.Minute))

	nextA := NewRemotePool(box.srv.URL, "", "")
	dials := cutPeer(t, nextA, func(int64) error { return refusedFailure() })
	clock := newMovableClock(at)
	policy := strandedPolicy(clock)
	next := newHoldRig(t, holdRigOptions{hold: policy, upstream: noLocalUpstream(t), exhausted: true,
		peers: []*RemotePool{nextA}, sessionsPath: sessionsPath})
	giveUpAt := warmWaitStart.Add(DefaultStrandedGiveUp)
	at = resentUntil(t, next, policy, clock, at, giveUpAt)
	givesUpAt(t, next, policy, clock, attempts{count: dials.Load, kick: func() { e2eRewindCooldown(nextA) }}, at, giveUpAt, gaveUpOnABox)
}

// A session store that cannot be written (a full disk, a directory gone
// read-only) does not keep a spent pool's chat waiting past its hour: the
// chain the store could not save is kept by this process, which gives up an
// hour after the first held turn.
func TestASpentPoolChatWhoseStoreCannotBeWrittenGivesUpAtTheHour(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	box := newContractBox(t, eligibleBoxAccount("acct-a"))
	dials := cutPeer(t, box.pool, func(int64) error { return refusedFailure() })
	clock := newMovableClock(warmWaitStart)
	policy := strandedPolicy(clock)
	rig := newHoldRig(t, holdRigOptions{hold: policy, upstream: noLocalUpstream(t), exhausted: true,
		peers: []*RemotePool{box.pool}, pin: "one@example.com", sessionsPath: filepath.Join(dir, "sessions.json")})
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := rig.store.NoteAnswered("claude", "hold-session", time.Now()); err == nil {
		t.Skip("this user writes into a read-only directory (root), so the store cannot be made to fail")
	}
	giveUpAt := warmWaitStart.Add(DefaultStrandedGiveUp)
	at := resentUntil(t, rig, policy, clock, warmWaitStart, giveUpAt)
	givesUpAt(t, rig, policy, clock, attempts{count: dials.Load, kick: func() { e2eRewindCooldown(box.pool) }}, at, giveUpAt, gaveUpOnABox)
	if rig.countLog("this process keeps it") == 0 {
		t.Fatalf("want the warning to say this process keeps the chain; logs:\n%s", rig.logs.String())
	}
}

// A held turn does not give up on a chain another turn of its chat has
// ended. Its chain's give-up is due when the held turn starts its last
// backoff; during that backoff the pool frees and another turn of the chat
// is answered. The held turn wakes at the old give-up time, is tried again,
// and is served: the chat is not answered the give-up or unlinked.
func TestAHeldTurnDoesNotGiveUpOnAChainAnAnswerEndedDuringItsBackoff(t *testing.T) {
	clock := newMovableClock(warmWaitStart)
	policy := strandedPolicy(clock)
	policy.GiveUp = 10 * time.Minute
	giveUpAt := warmWaitStart.Add(policy.GiveUp)
	box := newContractBox(t, eligibleBoxAccount("acct-a"))
	cutPeer(t, box.pool, func(int64) error { return refusedFailure() })
	var holds atomic.Int64
	backoff, resume := make(chan struct{}), make(chan struct{})
	rig := newHoldRig(t, holdRigOptions{hold: policy, upstream: answerOK, exhausted: true,
		peers: []*RemotePool{box.pool}, pin: "one@example.com",
		onLog: func(record slog.Record) {
			if record.Message == "proxy hold started" && holds.Add(1) == 2 {
				backoff <- struct{}{}
				<-resume
			}
		}})

	first := rig.sendAsync(t, holdTurnBody)
	awaitHeld(t, rig, policy, first, "the chain's first held turn")
	clock.set(warmWaitStart.Add(policy.Deadline))
	retryable(t, <-first, http.StatusBadGateway)
	rig.settle(t, 1)

	clock.set(giveUpAt.Add(-3 * time.Second))
	held := rig.sendAsync(t, holdTurnBody)
	<-backoff
	rig.sched.Set(localPool(false))
	if got := <-rig.sendAsync(t, holdTurnBody); got.status != http.StatusOK || !strings.Contains(got.body, "msg_held") {
		t.Fatalf("calibration: the other turn = %d %q, want the local answer; logs:\n%s", got.status, got.body, rig.logs.String())
	}
	rig.settle(t, 1)
	clock.set(giveUpAt)
	close(resume)
	got := <-held
	if got.status != http.StatusOK || !strings.Contains(got.body, "msg_held") {
		t.Fatalf("the held turn = %d Retry-After %q reason %q, want it tried again and served; logs:\n%s",
			got.status, got.retryAfter, got.reason, rig.logs.String())
	}
	if _, linked := rig.store.Get("claude", "hold-session"); !linked {
		t.Fatal("the chat was unlinked after its turn was answered")
	}
}
