package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/session"
)

// Refugee placement (PATCHES §67; owner, 2026-11-19). A chat held on a box
// found down is a refugee: it goes to the other box first and to a local
// account last (D3), to an account with fewer than 4 warm refugees (D1), and
// never back (D4). With no slot anywhere it keeps its hold and waits, and
// each held attempt tries again (D2).

// refugeeHoldsOn puts n refugee holds from box A on pool's account, each
// placed and last served idle ago.
func refugeeHoldsOn(pool *RemotePool, account string, n int, idle time.Duration) {
	for i := range n {
		key := session.ScopedSessionKey("claude", fmt.Sprintf("refugee-%s-%d", account, i))
		pool.delegations.mu.Lock()
		if pool.delegations.entries == nil {
			pool.delegations.entries = map[string]remotePoolDelegation{}
		}
		at := time.Now().Add(-idle)
		pool.delegations.entries[key] = remotePoolDelegation{Until: holdUntil(at, remotePoolDelegationTTLFor("claude")), Account: account,
			Peer: pool.name(), Targeted: true, Refugee: &session.Refugee{Origin: "box-a/acct-a", PlacedAt: at}}
		pool.delegations.mu.Unlock()
	}
}

// localRefugeesOn pins n sessions to the local account as refugees, warm.
func localRefugeesOn(t *testing.T, rig *holdRig, account string, n int) {
	t.Helper()
	for i := range n {
		if _, placed, err := rig.store.PutRefugee("claude", fmt.Sprintf("local-refugee-%d", i), account,
			session.Refugee{Origin: "box-a/acct-a", PlacedAt: time.Now()}, 99, func(session.Assignment) bool { return true }); err != nil || !placed {
			t.Fatalf("calibration: local refugee %d placed=%v err=%v", i, placed, err)
		}
	}
}

// refugeeHold returns hold-session's hold on pool and fails unless it is a
// refugee from box A's acct-a on account.
func refugeeHold(t *testing.T, rig *warmPeerRig, pool *RemotePool, account string) {
	t.Helper()
	hold, held := pool.delegations.lookup(session.ScopedSessionKey("claude", "hold-session"), time.Now())
	if !held || hold.Account != account || hold.Refugee == nil || hold.Refugee.Origin != rig.boxA.pool.name()+"/acct-a" {
		t.Fatalf("hold on %s = %+v (held %v), want a refugee from %s/acct-a on %s; logs:\n%s", pool.name(), hold, held, rig.boxA.pool.name(), account, rig.logs.String())
	}
	if _, held := holdAccount(rig.boxA.pool, "hold-session"); held {
		t.Fatalf("still held on box A after it was placed; logs:\n%s", rig.logs.String())
	}
}

// A refugee goes to the other box first, even with a local account that could
// serve it (D3: "we see if we can have them go to the other box first"), and
// to the local account when no account on the other box has a slot.
func TestARefugeeGoesToTheOtherBoxFirstAndToALocalAccountLast(t *testing.T) {
	t.Run("the other box has a slot", func(t *testing.T) {
		policy := strandedPolicy(nil)
		rig := newWarmPeerRig(t, policy, false, func(int64) error { return refusedFailure() })
		findDown(t, rig.boxA.pool)
		got := notHeld(t, rig.holdRig, policy, rig.sendAsync(t, holdTurnBody), "a refugee")
		rig.settle(t, 1)
		if got.status != http.StatusOK || !strings.Contains(got.body, `"served_by":"acct-b"`) || rig.local.Load() != 0 {
			t.Fatalf("status = %d body = %q local turns = %d, want acct-b's answer; logs:\n%s", got.status, got.body, rig.local.Load(), rig.logs.String())
		}
		refugeeHold(t, rig, rig.boxB.pool, "acct-b")
		if rig.countLog("refugee placed") != 1 || rig.countLog("to_account=acct-b refugees=1") != 1 {
			t.Fatalf("want one placement line with acct-b's count; logs:\n%s", rig.logs.String())
		}
	})
	t.Run("the other box is full", func(t *testing.T) {
		policy := strandedPolicy(nil)
		rig := newWarmPeerRig(t, policy, false, func(int64) error { return refusedFailure() })
		refugeeHoldsOn(rig.boxB.pool, "acct-b", refugeeSlots, time.Minute)
		findDown(t, rig.boxA.pool)
		got := notHeld(t, rig.holdRig, policy, rig.sendAsync(t, holdTurnBody), "a refugee")
		rig.settle(t, 1)
		if got.status != http.StatusOK || !strings.Contains(got.body, "msg_held") || rig.local.Load() != 1 || len(rig.boxB.seen()) != 0 {
			t.Fatalf("status = %d body = %q local turns = %d box B turns %d, want the local account's answer; logs:\n%s",
				got.status, got.body, rig.local.Load(), len(rig.boxB.seen()), rig.logs.String())
		}
		assignment, ok := rig.store.Get("claude", "hold-session")
		if !ok || assignment.AccountID != "one@example.com" || assignment.Refugee == nil || assignment.Refugee.Origin != rig.boxA.pool.name()+"/acct-a" {
			t.Fatalf("assignment = %+v (%v), want a refugee from box A on one@example.com", assignment, ok)
		}
		for _, pool := range []*RemotePool{rig.boxA.pool, rig.boxB.pool} {
			if _, held := holdAccount(pool, "hold-session"); held {
				t.Fatalf("a refugee placed locally is still held on %s", pool.name())
			}
		}
	})
}

// An account takes at most 4 warm refugees (D1). A refugee stays warm until
// the warm window, an hour, passes without a turn; a hold that has lapsed
// frees its slot.
func TestAnAccountTakesAtMostFourWarmRefugees(t *testing.T) {
	for _, tc := range []struct {
		name    string
		holds   int
		idle    time.Duration
		account string
	}{
		{"three warm refugees", refugeeSlots - 1, time.Minute, "acct-b"},
		{"four warm refugees", refugeeSlots, time.Minute, "one@example.com"},
		{"four refugees idle a minute short of the warm window", refugeeSlots, warmSessionIdleTTL - time.Minute, "one@example.com"},
		{"four refugees idle a minute past the warm window", refugeeSlots, warmSessionIdleTTL + time.Minute, "acct-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := strandedPolicy(nil)
			rig := newWarmPeerRig(t, policy, false, func(int64) error { return refusedFailure() })
			refugeeHoldsOn(rig.boxB.pool, "acct-b", tc.holds, tc.idle)
			findDown(t, rig.boxA.pool)
			got := notHeld(t, rig.holdRig, policy, rig.sendAsync(t, holdTurnBody), "a refugee")
			rig.settle(t, 1)
			if got.status != http.StatusOK || rig.countLog("to_account="+tc.account+" ") != 1 {
				t.Fatalf("status = %d body = %q, want it placed on %s; logs:\n%s", got.status, got.body, tc.account, rig.logs.String())
			}
		})
	}
}

// A refugee never goes back (D4: "No returning"): once box A answers again,
// the chat stays on the account it was placed on.
func TestARefugeeNeverGoesBack(t *testing.T) {
	policy := strandedPolicy(nil)
	var up atomic.Bool
	rig := newWarmPeerRig(t, policy, false, func(int64) error {
		if !up.Load() {
			return refusedFailure()
		}
		return nil
	})
	findDown(t, rig.boxA.pool)
	if got := notHeld(t, rig.holdRig, policy, rig.sendAsync(t, holdTurnBody), "a refugee"); !strings.Contains(got.body, `"served_by":"acct-b"`) {
		t.Fatalf("calibration: %d %q, want it placed on acct-b", got.status, got.body)
	}
	rig.settle(t, 1)
	up.Store(true)
	rig.boxA.pool.breaker.success(time.Now())
	got := notHeld(t, rig.holdRig, policy, rig.sendAsync(t, holdTurnBody), "the refugee's next turn")
	rig.settle(t, 1)
	if got.status != http.StatusOK || !strings.Contains(got.body, `"served_by":"acct-b"`) || len(rig.boxA.seen()) != 0 {
		t.Fatalf("status = %d body = %q box A turns = %d, want acct-b's answer and nothing sent home; logs:\n%s", got.status, got.body, len(rig.boxA.seen()), rig.logs.String())
	}
	refugeeHold(t, rig, rig.boxB.pool, "acct-b")
}

// With every account full the refugee is not moved (D2: "then they're just
// left, and they keep trying subrouter"): it keeps its hold on box A, its turn
// waits, and every held attempt tries placement again. Once a slot frees it
// is placed there.
func TestARefugeeWithNoSlotKeepsItsHoldAndIsPlacedWhenOneFrees(t *testing.T) {
	policy := strandedPolicy(nil)
	rig := newWarmPeerRig(t, policy, false, func(int64) error { return refusedFailure() })
	refugeeHoldsOn(rig.boxB.pool, "acct-b", refugeeSlots, time.Minute)
	localRefugeesOn(t, rig.holdRig, "one@example.com", refugeeSlots)
	findDown(t, rig.boxA.pool)
	answer := rig.sendAsync(t, holdTurnBody)
	awaitHeld(t, rig.holdRig, policy, answer, "a refugee with no slot")
	tries := attempts{count: func() int64 { return int64(rig.countLog("refugee not placed")) }, kick: func() {}}
	heldFor(t, rig.holdRig, answer, tries, 3, "with every account full")
	if account, held := holdAccount(rig.boxA.pool, "hold-session"); !held || account != "acct-a" {
		t.Fatalf("hold on box A = %q/%v, want acct-a kept while it waits", account, held)
	}
	if !strings.Contains(rig.logs.String(), "reason=refugee_overflow") {
		t.Fatalf("want the hold to name the overflow; logs:\n%s", rig.logs.String())
	}
	rig.boxB.pool.delegations.clear(session.ScopedSessionKey("claude", "refugee-acct-b-0"))
	got := answeredWithin(t, rig.holdRig, answer, tries, 20, "once acct-b has a slot")
	rig.settle(t, 1)
	if got.status != http.StatusOK || !strings.Contains(got.body, `"served_by":"acct-b"`) {
		t.Fatalf("status = %d body = %q, want acct-b's answer once its slot freed; logs:\n%s", got.status, got.body, rig.logs.String())
	}
	refugeeHold(t, rig, rig.boxB.pool, "acct-b")
}

// The refugee count survives a restart: the mark is persisted with the hold
// in peer-holds.json and with the assignment in the session store.
func TestRefugeeMarksSurviveARestart(t *testing.T) {
	now := time.Now()
	mark := session.Refugee{Origin: "box-a/acct-a", PlacedAt: now.UTC().Truncate(time.Second)}
	path := filepath.Join(t.TempDir(), "peer-holds.json")
	var before remotePoolDelegations
	for i := range refugeeSlots {
		key := session.ScopedSessionKey("claude", fmt.Sprintf("r%d", i))
		id, _ := before.claimRefugee(key, now, remotePoolDelegationTTLFor("claude"), "box-b", "acct-b", mark, refugeeSlots)
		if id == 0 {
			t.Fatalf("calibration: refugee %d not claimed", i)
		}
		before.setServed(key, now, remotePoolDelegationTTLFor("claude"), "acct-b", "box-b", true)
	}
	before.flush(path, now)
	var after remotePoolDelegations
	after.loadFor(path, now, "box-b")
	if id, held := after.claimRefugee(session.ScopedSessionKey("claude", "r-next"), now, remotePoolDelegationTTLFor("claude"), "box-b", "acct-b", mark, refugeeSlots); id != 0 || held != refugeeSlots {
		t.Fatalf("after a restart acct-b took a refugee (claim %d, held %d), want it full with %d", id, held, refugeeSlots)
	}

	storePath := filepath.Join(t.TempDir(), "sessions.json")
	store, err := session.NewStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	warm := func(session.Assignment) bool { return true }
	for i := range refugeeSlots {
		if _, placed, err := store.PutRefugee("claude", fmt.Sprintf("l%d", i), "owner", mark, refugeeSlots, warm); err != nil || !placed {
			t.Fatalf("calibration: local refugee %d placed=%v err=%v", i, placed, err)
		}
	}
	reopened, err := session.NewStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if held, placed, err := reopened.PutRefugee("claude", "l-next", "owner", mark, refugeeSlots, warm); err != nil || placed || held != refugeeSlots {
		t.Fatalf("after a restart owner took a refugee (placed %v, held %d, err %v), want it full with %d", placed, held, err, refugeeSlots)
	}
	if assignment, ok := reopened.Get("claude", "l0"); !ok || assignment.Refugee == nil || *assignment.Refugee != mark {
		t.Fatalf("reopened assignment %+v, want its refugee mark %+v", assignment, mark)
	}
}

// A box found down ends no hold in bulk (PATCHES §67): the chat whose turn
// found it is placed, and every other chat held there keeps its hold until
// its own next turn places it.
func TestABoxFoundDownEndsOnlyTheHoldsOfTheChatsItPlaces(t *testing.T) {
	policy := strandedPolicy(nil)
	rig := newWarmPeerRig(t, policy, false, func(int64) error { return realDialTimeout(t) })
	holdOn(t, rig.boxA.pool, "other-session", "acct-a")
	pool := rig.boxA.pool
	struck := time.Now().Add(-2 * time.Second)
	if opened, _, _ := pool.breaker.failure(struck, struck, realDialTimeout(t), &pool.egress, 0); opened {
		t.Fatal("calibration: one strike opened the breaker")
	}
	pool.egress.noteAnswerOnANewConnection(time.Now().Add(-time.Second))
	got := notHeld(t, rig.holdRig, policy, rig.sendAsync(t, holdTurnBody), "the turn that finds box A down")
	rig.settle(t, 1)
	if got.status != http.StatusOK || !strings.Contains(got.body, `"served_by":"acct-b"`) || !strings.Contains(rig.logs.String(), "peer_down=true") {
		t.Fatalf("status = %d body = %q, want box A found down and the turn placed on acct-b; logs:\n%s", got.status, got.body, rig.logs.String())
	}
	if account, held := holdAccount(pool, "other-session"); !held || account != "acct-a" {
		t.Fatalf("other-session's hold = %q/%v, want it kept on acct-a until its next turn", account, held)
	}
	if rig.countLog("remote pool hold ended") != 0 {
		t.Fatalf("a box found down ended holds; logs:\n%s", rig.logs.String())
	}
	resp, body := rig.turnOf(t, "other-session")
	rig.settle(t, 1)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"served_by":"acct-b"`) || rig.countLog("to_account=acct-b refugees=2") != 1 {
		t.Fatalf("other-session: status = %d body = %q, want it placed on acct-b as its second refugee; logs:\n%s", resp.StatusCode, body, rig.logs.String())
	}
}

// turnOf is one model turn of sessionID.
func (rig *holdRig) turnOf(t *testing.T, sessionID string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(promptly(t), http.MethodPost, rig.daemon.URL+"/v1/messages", strings.NewReader(holdTurnBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+holdTestProxyToken)
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", sessionID)
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		t.Fatalf("%s: %v", sessionID, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

// A local refugee takes its account's slot only while it is warm (the warm
// window, an hour) and not held on a box since (its local pin is then a
// placeholder). Of six marked on the local account, two are warm though idle
// a minute short of the window, two are cold (one idle a minute past it, one
// unseen in the tracker's three hours) and two are warm but held on box B:
// the account holds two, so a third is placed there.
func TestALocalRefugeeCountsOnlyWhileWarmAndNotHeldOnABox(t *testing.T) {
	policy := strandedPolicy(nil)
	boxA := newContractBox(t, eligibleBoxAccount("acct-a"))
	boxB := newContractBox(t, eligibleBoxAccount("acct-b"))
	cutPeer(t, boxA.pool, func(int64) error { return refusedFailure() })
	cutPeer(t, boxB.pool, func(int64) error { return nil })
	active := seenSessions(time.Minute)
	rig := newHoldRig(t, holdRigOptions{hold: policy, upstream: answerOK, pin: "one@example.com", active: active,
		peers: []*RemotePool{boxA.pool, boxB.pool}})
	holdOn(t, boxA.pool, "hold-session", "acct-a")
	refugeeHoldsOn(boxB.pool, "acct-b", refugeeSlots, time.Minute)
	localRefugeesOn(t, rig, "one@example.com", 6)
	for i, idle := range []time.Duration{warmSessionIdleTTL - time.Minute, warmSessionIdleTTL - time.Minute, 0, 0, warmSessionIdleTTL + time.Minute} {
		active.lastSeen[session.ScopedSessionKey("claude", fmt.Sprintf("local-refugee-%d", i))] = time.Now().Add(-idle)
	}
	holdOn(t, boxB.pool, "local-refugee-2", "acct-b")
	holdOn(t, boxB.pool, "local-refugee-3", "acct-b")
	findDown(t, boxA.pool)
	got := notHeld(t, rig, policy, rig.sendAsync(t, holdTurnBody), "a refugee")
	rig.settle(t, 1)
	if got.status != http.StatusOK || !strings.Contains(got.body, "msg_held") || rig.countLog("to_account=one@example.com refugees=3") != 1 {
		t.Fatalf("status = %d body = %q, want it placed locally as the account's third refugee; logs:\n%s", got.status, got.body, rig.logs.String())
	}
}

// A refugee placed on the local account stays there (D4, R4): when its
// upstream cannot be dialed the turn waits for it under the hold and is never
// spilled to box B for a turn. That holds whether its box was found down at
// its first turn or it waited for a slot first, until a local refugee went
// cold.
func TestALocalRefugeeWaitsForItsUpstreamAndIsNeverSpilledToABox(t *testing.T) {
	for _, tc := range []struct {
		name          string
		waitsForASlot bool
	}{
		{"its box was found down at its first turn", false},
		{"it waited for a slot first", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := strandedPolicy(nil)
			boxA := newContractBox(t, eligibleBoxAccount("acct-a"))
			boxB := newContractBox(t, eligibleBoxAccount("acct-b"))
			cutPeer(t, boxA.pool, func(int64) error { return refusedFailure() })
			cutPeer(t, boxB.pool, func(int64) error { return nil })
			var localDown atomic.Bool
			localDown.Store(true)
			active := seenSessions(time.Minute)
			rig := newHoldRig(t, holdRigOptions{
				hold:     policy,
				upstream: answerOK,
				pin:      "one@example.com",
				active:   active,
				peers:    []*RemotePool{boxA.pool, boxB.pool},
				spill:    spillTestPolicy(DefaultOutageSpillRate, DefaultOutageSpillBurst, standingClock()),
				failDials: func(int64) error {
					if localDown.Load() {
						return refusedFailure()
					}
					return nil
				},
			})
			holdOn(t, boxA.pool, "hold-session", "acct-a")
			refugeeHoldsOn(boxB.pool, "acct-b", refugeeSlots, time.Minute)
			if tc.waitsForASlot {
				localRefugeesOn(t, rig, "one@example.com", refugeeSlots)
				for i := range refugeeSlots {
					active.Begin("claude", fmt.Sprintf("local-refugee-%d", i))()
				}
			}
			findDown(t, boxA.pool)
			answer := rig.sendAsync(t, holdTurnBody)
			awaitHeld(t, rig, policy, answer, "a refugee")
			if tc.waitsForASlot {
				tries := attempts{count: func() int64 { return int64(rig.countLog("refugee not placed")) }, kick: func() {}}
				heldFor(t, rig, answer, tries, 3, "with every account full")
				active.mu.Lock()
				active.lastSeen[session.ScopedSessionKey("claude", "local-refugee-0")] = time.Now().Add(-warmSessionIdleTTL - time.Minute)
				active.mu.Unlock()
			}
			heldFor(t, rig, answer, localAttempts(rig), 3, "placed on the local account, its upstream refusing")
			if assignment, ok := rig.store.Get("claude", "hold-session"); !ok || assignment.AccountID != "one@example.com" || assignment.Refugee == nil {
				t.Fatalf("assignment = %+v (%v), want a refugee on one@example.com; logs:\n%s", assignment, ok, rig.logs.String())
			}
			localDown.Store(false)
			got := answeredWithin(t, rig, answer, localAttempts(rig), 20, "once the local upstream answers")
			rig.settle(t, 1)
			if got.status != http.StatusOK || !strings.Contains(got.body, "msg_held") || len(boxB.seen()) != 0 {
				t.Fatalf("status = %d body = %q box B turns = %d, want the local account's answer and nothing on box B; logs:\n%s",
					got.status, got.body, len(boxB.seen()), rig.logs.String())
			}
		})
	}
}

// A refugee box B takes but cannot serve (its dial fails), with no local
// slot, keeps its hold on box A: the forward to box B cleared it, and it is
// put back (D2).
func TestARefugeeWhosePlacementFailsKeepsItsHomeHold(t *testing.T) {
	policy := strandedPolicy(nil)
	boxA := newContractBox(t, eligibleBoxAccount("acct-a"))
	boxB := newContractBox(t, eligibleBoxAccount("acct-b"))
	cutPeer(t, boxA.pool, func(int64) error { return refusedFailure() })
	dialsB := cutPeer(t, boxB.pool, func(int64) error { return refusedFailure() })
	rig := newHoldRig(t, holdRigOptions{hold: policy, upstream: answerOK, pin: "one@example.com", peers: []*RemotePool{boxA.pool, boxB.pool}})
	holdOn(t, boxA.pool, "hold-session", "acct-a")
	localRefugeesOn(t, rig, "one@example.com", refugeeSlots)
	findDown(t, boxA.pool)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	answer := make(chan clientAnswer, 1)
	go func() {
		_, _, err := rig.turn(t, ctx, nil)
		answer <- clientAnswer{err: err}
	}()
	awaitHeld(t, rig, policy, answer, "a refugee box B could not take")
	heldFor(t, rig, answer, attempts{count: dialsB.Load, kick: func() { e2eRewindCooldown(boxB.pool) }}, 3, "box B refusing every dial")
	cancel()
	<-answer
	rig.settle(t, 1)
	if account, held := holdAccount(boxA.pool, "hold-session"); !held || account != "acct-a" {
		t.Fatalf("hold on box A = %q/%v, want acct-a put back; logs:\n%s", account, held, rig.logs.String())
	}
	if account, held := holdAccount(boxB.pool, "hold-session"); held {
		t.Fatalf("held on box B's %q, which never served it", account)
	}
}
