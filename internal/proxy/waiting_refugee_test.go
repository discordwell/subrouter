package proxy

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
	"github.com/discordwell/subrouter/session"
)

// A chat that gave up waiting for its box keeps a waiting record with its
// assignment, naming its home box and account (PATCHES §67). Whenever it is
// restarted, its next turn goes home if the home box serves it, found down
// or not; otherwise it is placed by the spread, under the same rules and
// caps; when the spread places nothing it gets the give-up answer at once,
// with no hold. While the record lasts it is not placed as a cold session.

// newGaveUpRig is the spread rig after hold-session gave up: box A found
// down, box B's acct-b and the laptop each holding refugeeSlots refugees, so
// its turn waited on its home hold for an hour and got the give-up.
func newGaveUpRig(t *testing.T) *spreadRig {
	t.Helper()
	rig := newSpreadRig(t, 0, nil, eligibleBoxAccount("acct-b"))
	refugeeHoldsOn(rig.boxB.pool, "acct-b", refugeeSlots, time.Minute)
	localRefugeesOn(t, rig.holdRig, "one@example.com", refugeeSlots)
	rig.gaveUpAtTheHour(t)
	return rig
}

// gaveUpAtTheHour has Claude Code resend hold-session's turn, held with
// every account full, until its give-up.
func (rig *spreadRig) gaveUpAtTheHour(t *testing.T) {
	t.Helper()
	giveUpAt := warmWaitStart.Add(DefaultStrandedGiveUp)
	at := resentUntil(t, rig.holdRig, rig.policy, rig.clock, warmWaitStart, giveUpAt)
	tries := attempts{count: func() int64 { return int64(rig.countLog(refugeeNotPlacedMessage)) }, kick: func() {}}
	givesUpAt(t, rig.holdRig, rig.policy, rig.clock, tries, at, giveUpAt, gaveUpOnItsCache)
	rig.settle(t, 1)
	if account, held := holdAccount(rig.boxA.pool, "hold-session"); held {
		t.Fatalf("calibration: still held on box A's %q after the give-up", account)
	}
}

// restarted is the chat's next turn, two hours after its give-up: it must be
// answered at once, never held.
func (rig *spreadRig) restarted(t *testing.T, what string) clientAnswer {
	t.Helper()
	rig.clock.set(warmWaitStart.Add(DefaultStrandedGiveUp + 2*time.Hour))
	got := notHeld(t, rig.holdRig, rig.policy, rig.sendAsync(t, holdTurnBody), what)
	rig.settle(t, 1)
	return got
}

// stoppedAgain fails unless the restarted turn got the give-up answer at
// once, saying no account can take it, and was placed nowhere.
func (rig *spreadRig) stoppedAgain(t *testing.T, got clientAnswer, what string) {
	t.Helper()
	gaveUpSaying(t, rig.holdRig, got, what, "no account can take it")
	if n := rig.local.Load(); n != 0 {
		t.Fatalf("%s: the laptop served %d turns", what, n)
	}
	if assignment, ok := rig.store.Get("claude", "hold-session"); ok && assignment.Refugee != nil {
		t.Fatalf("%s: placed on the laptop: %+v", what, assignment)
	}
	if account, held := holdAccount(rig.boxB.pool, "hold-session"); held {
		t.Fatalf("%s: placed on box B's %q", what, account)
	}
}

func TestAChatThatGaveUpOnItsBoxResumesWhereThereIsRoom(t *testing.T) {
	t.Run("its box is down and box B has room", func(t *testing.T) {
		rig := newGaveUpRig(t)
		rig.boxB.pool.delegations.clear(session.ScopedSessionKey("claude", "refugee-acct-b-0"))
		dialsA := rig.dialsA.Load()
		got := rig.restarted(t, "the restart with a slot on box B")
		if place := placeOf(got.status, got.body); place != "acct-b" {
			t.Fatalf("placed on %s, want box B's free acct-b; logs:\n%s", place, rig.logs.String())
		}
		refugeeHoldOf(t, rig, "hold-session", "acct-b")
		if n := rig.dialsA.Load() - dialsA; n != 0 {
			t.Fatalf("box A, found down with its breaker open, was dialed %d times; want it not tried", n)
		}
	})
	t.Run("every account is full", func(t *testing.T) {
		rig := newGaveUpRig(t)
		holds := rig.countLog("proxy hold started")
		rig.stoppedAgain(t, rig.restarted(t, "the restart with every account full"), "the restart with every account full")
		if n := rig.countLog("proxy hold started"); n != holds {
			t.Fatalf("the restart started a hold; logs:\n%s", rig.logs.String())
		}
	})
	t.Run("a slot frees on the laptop", func(t *testing.T) {
		rig := newGaveUpRig(t)
		rig.stoppedAgain(t, rig.restarted(t, "the first restart"), "the first restart")
		if _, err := rig.store.Delete("claude", "local-refugee-0"); err != nil {
			t.Fatal(err)
		}
		got := rig.restarted(t, "the restart once a laptop slot freed")
		if place := placeOf(got.status, got.body); place != "local" {
			t.Fatalf("placed on %s, want the laptop's freed slot with box B full; logs:\n%s", place, rig.logs.String())
		}
		if assignment, ok := rig.store.Get("claude", "hold-session"); !ok || assignment.Refugee == nil ||
			assignment.Refugee.Origin != rig.boxA.pool.name()+"/acct-a" {
			t.Fatalf("assignment = %+v (%v), want a refugee from box A's acct-a", assignment, ok)
		}
	})
	t.Run("its box is back", func(t *testing.T) {
		rig := newGaveUpRig(t)
		rig.boxA.mu.Lock()
		rig.boxA.substitute = "acct-x"
		rig.boxA.mu.Unlock()
		rig.boxAAnswers(t)
		got := rig.restarted(t, "the restart with box A back")
		if place := placeOf(got.status, got.body); place != "acct-a" {
			t.Fatalf("served on %s, want its home acct-a; logs:\n%s", place, rig.logs.String())
		}
		hold, held := rig.boxA.pool.delegations.lookup(session.ScopedSessionKey("claude", "hold-session"), time.Now())
		if !held || hold.Account != "acct-a" || hold.Refugee != nil || rig.countLog(refugeePlacedMessage) != 0 {
			t.Fatalf("hold on box A = %+v (held %v), want acct-a, not a refugee placement; logs:\n%s", hold, held, rig.logs.String())
		}
		if refugees := rig.boxB.pool.delegations.refugeesOn("", "acct-b", time.Now()); refugees != refugeeSlots {
			t.Fatalf("box B's acct-b holds %d refugees, want its %d: going home takes no slot", refugees, refugeeSlots)
		}
	})
	t.Run("once answered it is a normal session", func(t *testing.T) {
		rig := newGaveUpRig(t)
		rig.boxB.pool.delegations.clear(session.ScopedSessionKey("claude", "refugee-acct-b-0"))
		if got := rig.restarted(t, "the restart with a slot on box B"); placeOf(got.status, got.body) != "acct-b" {
			t.Fatalf("calibration: the restart was answered %d %q, want it placed on acct-b", got.status, got.body)
		}
		rig.boxAAnswers(t)
		got := rig.restarted(t, "its next turn with box A back")
		if place := placeOf(got.status, got.body); place != "acct-b" {
			t.Fatalf("its next turn was served on %s, want acct-b, where it was placed: an answered turn ends the waiting record; logs:\n%s", place, rig.logs.String())
		}
	})
}

// A box found down reads so until something reaches it. A restarted chat
// tries its home box once that box's breaker lets a request through, as a
// held turn does, so it is served there on its own account when the box is
// back though nothing else has read it, and placed by the spread when the
// box is still down. Box A serves an untargeted turn on acct-x.
func TestARestartedChatTriesItsHomeBoxThoughItWasFoundDown(t *testing.T) {
	freeLaptopSlot := func(t *testing.T, rig *spreadRig) {
		if _, err := rig.store.Delete("claude", "local-refugee-0"); err != nil {
			t.Fatal(err)
		}
	}
	freeBoxBSlot := func(_ *testing.T, rig *spreadRig) {
		rig.boxB.pool.delegations.clear(session.ScopedSessionKey("claude", "refugee-acct-b-0"))
	}
	for _, tc := range []struct {
		name string
		free func(*testing.T, *spreadRig)
		back bool
		want string
	}{
		{"back, with every other account full", nil, true, "acct-a"},
		{"back, with a slot free on the laptop", freeLaptopSlot, true, "acct-a"},
		{"still down, with a slot free on box B", freeBoxBSlot, false, "acct-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newGaveUpRig(t)
			rig.boxA.mu.Lock()
			rig.boxA.substitute = "acct-x"
			rig.boxA.mu.Unlock()
			if tc.free != nil {
				tc.free(t, rig)
			}
			rig.aDown.Store(!tc.back)
			e2eRewindCooldown(rig.boxA.pool)
			if !rig.boxA.pool.breaker.foundDown() {
				t.Fatal("calibration: box A is not found down")
			}
			got := rig.restarted(t, "the restart with box A's breaker past its cooldown")
			if place := placeOf(got.status, got.body); place != tc.want {
				t.Fatalf("served on %s, want %s; logs:\n%s", place, tc.want, rig.logs.String())
			}
			if n := rig.local.Load(); n != 0 {
				t.Fatalf("the laptop served %d turns", n)
			}
		})
	}
}

// A restarted chat whose home box is not found down, but cannot be asked
// right now, is a box the laptop waits for, as any other box is: with box B
// full, the restart gets the give-up at once and the laptop takes nothing,
// though it has a slot.
func TestARestartedChatStaysOffTheLaptopWhileItsHomeBoxCannotBeAsked(t *testing.T) {
	for _, tc := range []struct {
		name string
		cut  func(*spreadRig)
	}{
		{"its breaker is open", func(rig *spreadRig) { setBreakerOpenUntil(rig.boxA.pool, time.Now().Add(time.Hour)) }},
		{"its dial fails", func(rig *spreadRig) { rig.aDown.Store(true) }},
		{"it answers transiently", func(rig *spreadRig) {
			rig.boxA.mu.Lock()
			rig.boxA.turnStatus, rig.boxA.turnTransient = http.StatusTooManyRequests, true
			rig.boxA.mu.Unlock()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newGaveUpRig(t)
			rig.boxAAnswers(t)
			if _, err := rig.store.Delete("claude", "local-refugee-0"); err != nil {
				t.Fatal(err)
			}
			tc.cut(rig)
			got := rig.restarted(t, "the restart while box A cannot be asked")
			gaveUpSaying(t, rig.holdRig, got, "the restart while box A cannot be asked", "no account can take it")
			if rig.boxA.pool.breaker.foundDown() {
				t.Fatal("calibration: box A was found down")
			}
			if n := rig.local.Load(); n != 0 {
				t.Fatalf("the laptop served %d turns while box A could not be asked; logs:\n%s", n, rig.logs.String())
			}
			if assignment, ok := rig.store.Get("claude", "hold-session"); ok && assignment.Refugee != nil {
				t.Fatalf("placed on the laptop while box A could not be asked: %+v", assignment)
			}
			lines := rig.logRecords(waitingRefugeeNotPlacedMessage)
			if len(lines) != 1 || lines[0]["why"] != "box_unreachable:"+rig.boxA.pool.name() {
				t.Fatalf("not-placed lines %v, want one with why=box_unreachable:%s", lines, rig.boxA.pool.name())
			}
		})
	}
}

// A restarted chat placed on a box is no longer waiting, whatever that box
// answered: its next turn, with box A back, stays where it was placed (D4).
// Box B's first answer is a 400, which is no answered turn; or the next
// process runs with the give-up off, so no answered time is kept.
func TestARestartedChatPlacedOnABoxStaysThereWhenItsHomeComesBack(t *testing.T) {
	t.Run("its first answer there is a 400", func(t *testing.T) {
		rig := newGaveUpRig(t)
		rig.boxB.pool.delegations.clear(session.ScopedSessionKey("claude", "refugee-acct-b-0"))
		rig.boxB.mu.Lock()
		rig.boxB.turnStatus = http.StatusBadRequest
		rig.boxB.mu.Unlock()
		if got := rig.restarted(t, "the restart placed on box B"); got.status != http.StatusBadRequest {
			t.Fatalf("calibration: the restart was answered %d %q, want box B's 400", got.status, got.body)
		}
		refugeeHoldOf(t, rig, "hold-session", "acct-b")
		rig.boxB.mu.Lock()
		rig.boxB.turnStatus = 0
		rig.boxB.mu.Unlock()
		rig.boxAAnswers(t)
		got := rig.restarted(t, "its next turn with box A back")
		if place := placeOf(got.status, got.body); place != "acct-b" {
			t.Fatalf("its next turn was served on %s, want acct-b, where it was placed; logs:\n%s", place, rig.logs.String())
		}
	})
	t.Run("the give-up is off", func(t *testing.T) {
		rig := newGaveUpRig(t)
		nextA := NewRemotePool(rig.boxA.srv.URL, "", "")
		var aUp atomic.Bool
		cutPeer(t, nextA, func(int64) error {
			if aUp.Load() {
				return nil
			}
			return refusedFailure()
		})
		findDown(t, nextA)
		nextB := NewRemotePool(rig.boxB.srv.URL, "", "")
		cutPeer(t, nextB, func(int64) error { return nil })
		refugeeHoldsOn(nextB, "acct-b", refugeeSlots-1, time.Minute)
		policy := strandedPolicy(nil)
		policy.GiveUp = 0
		next := newHoldRig(t, holdRigOptions{hold: policy, upstream: answerOK, sessionsPath: rig.storePath,
			peers: []*RemotePool{nextA, nextB}, refugeeSlots: slotsOf(nil)})
		got := <-next.sendAsync(t, holdTurnBody)
		next.settle(t, 1)
		if place := placeOf(got.status, got.body); place != "acct-b" {
			t.Fatalf("calibration: the restart was served on %s, want acct-b; logs:\n%s", place, next.logs.String())
		}
		aUp.Store(true)
		ageDecisionBody(t, nextA, remotePoolAdminTTL+time.Second)
		if snapshot := nextA.usageForDecision(time.Now().Add(5 * time.Second)); !snapshot.Reachable {
			t.Fatalf("calibration: box A's usage read failed: %s", snapshot.Err)
		}
		e2eRewindCooldown(nextA)
		got = <-next.sendAsync(t, holdTurnBody)
		next.settle(t, 1)
		if place := placeOf(got.status, got.body); place != "acct-b" {
			t.Fatalf("its next turn was served on %s, want acct-b, where it was placed; logs:\n%s", place, next.logs.String())
		}
	})
}

// A restarted chat its home box serves is home and no longer waiting, though
// no answered turn ended the record (the next process runs with the give-up
// off): cut from its box again, its next turn waits for that box on its
// hold, as any chat a box holds does, rather than getting the give-up.
func TestARestartedChatServedAtHomeWaitsForItsBoxOnTheNextBlip(t *testing.T) {
	rig := newGaveUpRig(t)
	nextA := NewRemotePool(rig.boxA.srv.URL, "", "")
	var aUp atomic.Bool
	aUp.Store(true)
	cutPeer(t, nextA, func(int64) error {
		if aUp.Load() {
			return nil
		}
		return refusedFailure()
	})
	nextB := NewRemotePool(rig.boxB.srv.URL, "", "")
	cutPeer(t, nextB, func(int64) error { return nil })
	refugeeHoldsOn(nextB, "acct-b", refugeeSlots, time.Minute)
	policy := strandedPolicy(nil)
	policy.GiveUp = 0
	next := newHoldRig(t, holdRigOptions{hold: policy, upstream: answerOK, sessionsPath: rig.storePath,
		peers: []*RemotePool{nextA, nextB}, refugeeSlots: slotsOf(nil)})
	got := <-next.sendAsync(t, holdTurnBody)
	next.settle(t, 1)
	if place := placeOf(got.status, got.body); place != "acct-a" {
		t.Fatalf("calibration: the restart was served on %s, want its home acct-a; logs:\n%s", place, next.logs.String())
	}
	aUp.Store(false)
	answer := next.sendAsync(t, holdTurnBody)
	awaitHeld(t, next, policy, answer, "its next turn with box A cut")
}

// boxAAnswers brings box A back: its dials get through, its usage is read,
// which clears its found-down state, and its breaker's cooldown has run out.
func (rig *spreadRig) boxAAnswers(t *testing.T) {
	t.Helper()
	rig.aDown.Store(false)
	ageDecisionBody(t, rig.boxA.pool, remotePoolAdminTTL+time.Second)
	if snapshot := rig.boxA.pool.usageForDecision(time.Now().Add(5 * time.Second)); !snapshot.Reachable {
		t.Fatalf("calibration: box A's usage read failed: %s", snapshot.Err)
	}
	e2eRewindCooldown(rig.boxA.pool)
	if rig.boxA.pool.breaker.foundDown() {
		t.Fatal("calibration: box A answered and is still found down")
	}
}

// The waiting record is kept with the session's assignment, so it outlives
// the process that gave up: the next process gives the restarted chat the
// give-up answer at once with every account still full, not a cold
// placement.
func TestAWaitingChatSurvivesADaemonRestart(t *testing.T) {
	rig := newGaveUpRig(t)
	nextA := NewRemotePool(rig.boxA.srv.URL, "", "")
	cutPeer(t, nextA, func(int64) error { return refusedFailure() })
	findDown(t, nextA)
	nextB := NewRemotePool(rig.boxB.srv.URL, "", "")
	cutPeer(t, nextB, func(int64) error { return nil })
	refugeeHoldsOn(nextB, "acct-b", refugeeSlots, time.Minute)
	policy := strandedPolicy(nil)
	next := newHoldRig(t, holdRigOptions{hold: policy, upstream: answerOK, sessionsPath: rig.storePath,
		peers: []*RemotePool{nextA, nextB}, refugeeSlots: slotsOf(nil)})
	got := notHeld(t, next, policy, next.sendAsync(t, holdTurnBody), "the restart on the next process")
	next.settle(t, 1)
	gaveUpSaying(t, next, got, "the restart on the next process", "no account can take it")
	if strings.Contains(got.body, "msg_held") || len(rig.boxB.seen()) != 0 {
		t.Fatalf("placed somewhere on the next process: %q", got.body)
	}
}

// A chat that gave up while the session store could not be written (a full
// disk, a directory gone read-only) waits all the same: this process keeps
// its waiting record. With every account full its restart gets the give-up
// answer at once, not a placement as a new session. Once a slot frees it is
// placed there, on box B or, with the store writable again, on the laptop.
// The placement's answer is a 400, which is no answered turn, and its next
// turn stays where it was placed with box A back.
func TestAChatThatGaveUpWhileItsStoreCouldNotBeWrittenWaitsAllTheSame(t *testing.T) {
	t.Run("box B takes it", func(t *testing.T) {
		rig, _ := gaveUpUnsaved(t)
		rig.boxB.pool.delegations.clear(session.ScopedSessionKey("claude", "refugee-acct-b-0"))
		rig.boxB.mu.Lock()
		rig.boxB.turnStatus = http.StatusBadRequest
		rig.boxB.mu.Unlock()
		if got := rig.restarted(t, "the restart with a slot on box B"); got.status != http.StatusBadRequest {
			t.Fatalf("the restart with a slot on box B was answered %d %q, want box B's 400; logs:\n%s", got.status, got.body, rig.logs.String())
		}
		refugeeHoldOf(t, rig, "hold-session", "acct-b")
		rig.boxB.mu.Lock()
		rig.boxB.turnStatus = 0
		rig.boxB.mu.Unlock()
		rig.boxAAnswers(t)
		if got := rig.restarted(t, "its next turn with box A back"); placeOf(got.status, got.body) != "acct-b" {
			t.Fatalf("its next turn was answered %d %q, want acct-b, where it was placed; logs:\n%s", got.status, got.body, rig.logs.String())
		}
	})
	t.Run("the laptop takes it", func(t *testing.T) {
		rig, dir := gaveUpUnsaved(t)
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := rig.store.Delete("claude", "local-refugee-0"); err != nil {
			t.Fatal(err)
		}
		rig.localStatus.Store(http.StatusBadRequest)
		if got := rig.restarted(t, "the restart with a slot on the laptop"); got.status != http.StatusBadRequest || rig.local.Load() != 1 {
			t.Fatalf("the restart with a slot on the laptop was answered %d %q (the laptop served %d), want the laptop's 400; logs:\n%s", got.status, got.body, rig.local.Load(), rig.logs.String())
		}
		rig.localStatus.Store(0)
		rig.boxAAnswers(t)
		if got := rig.restarted(t, "its next turn with box A back"); placeOf(got.status, got.body) != "local" {
			t.Fatalf("its next turn was answered %d %q, want the laptop, where it was placed; logs:\n%s", got.status, got.body, rig.logs.String())
		}
	})
}

// gaveUpUnsaved is the gave-up rig with the session store unwritable from
// before the give-up, restarted once with every account full. It returns the
// store's directory.
func gaveUpUnsaved(t *testing.T) (*spreadRig, string) {
	t.Helper()
	rig := newSpreadRig(t, 0, nil, eligibleBoxAccount("acct-b"))
	refugeeHoldsOn(rig.boxB.pool, "acct-b", refugeeSlots, time.Minute)
	localRefugeesOn(t, rig.holdRig, "one@example.com", refugeeSlots)
	dir := filepath.Dir(rig.storePath)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := rig.store.Put("claude", "store-check", "one@example.com", ""); err == nil {
		t.Skip("this user writes into a read-only directory (root), so the store cannot be made to fail")
	}
	rig.gaveUpAtTheHour(t)
	rig.stoppedAgain(t, rig.restarted(t, "the restart with every account full"), "the restart with every account full")
	if rig.countLog("this process keeps it") == 0 {
		t.Fatalf("want the warning to say this process keeps the waiting record; logs:\n%s", rig.logs.String())
	}
	return rig, dir
}

// A refugee placed on a local account is served there, though it has been
// idle past the warm window, as a chat restarted hours after its give-up has:
// the placement is its turn, not a stale session's for selection to re-rank
// onto a better local account with no slot taken. one@example.com has the
// most free slots and two@example.com ranks first for a new session.
func TestARefugeePlacedOnALocalAccountIsServedThereThoughItIsIdle(t *testing.T) {
	boxA := newContractBox(t, eligibleBoxAccount("acct-a"))
	boxB := newContractBox(t, eligibleBoxAccount("acct-b"))
	cutPeer(t, boxA.pool, func(int64) error { return refusedFailure() })
	cutPeer(t, boxB.pool, func(int64) error { return nil })
	active := seenSessions(-1)
	policy := strandedPolicy(nil)
	var mu sync.Mutex
	var servedWith []string
	rig := newHoldRig(t, holdRigOptions{
		hold: policy,
		upstream: func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			servedWith = append(servedWith, r.Header.Get("Authorization"))
			mu.Unlock()
			answerOK(w, r)
		},
		accounts: []accounts.Account{
			{ID: "one@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-one"},
			{ID: "two@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-two"},
		},
		active:       active,
		peers:        []*RemotePool{boxA.pool, boxB.pool},
		refugeeSlots: slotsOf(nil),
	})
	rig.sched.Set(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "one@example.com", Provider: accounts.ProviderClaude, Headroom: 0.5, ShortHeadroom: 0.5},
		{AccountID: "two@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
	}))
	holdOn(t, boxA.pool, "hold-session", "acct-a")
	refugeeHoldsOn(boxB.pool, "acct-b", refugeeSlots, time.Minute)
	localRefugeesOn(t, rig, "two@example.com", refugeeSlots-1)
	for i := range refugeeSlots - 1 {
		active.Begin("claude", fmt.Sprintf("local-refugee-%d", i))()
	}
	findDown(t, boxA.pool)
	got := notHeld(t, rig, policy, rig.sendAsync(t, holdTurnBody), "a refugee placed on the laptop")
	rig.settle(t, 1)
	mu.Lock()
	defer mu.Unlock()
	if got.status != http.StatusOK || fmt.Sprint(servedWith) != "[Bearer tok-one]" {
		t.Fatalf("status = %d, upstream saw %v, want one turn on one@example.com; logs:\n%s", got.status, servedWith, rig.logs.String())
	}
	if assignment, ok := rig.store.Get("claude", "hold-session"); !ok || assignment.AccountID != "one@example.com" || assignment.Refugee == nil {
		t.Fatalf("assignment = %+v (%v), want a refugee on one@example.com", assignment, ok)
	}
}
