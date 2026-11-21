package proxy

import (
	"sync"
	"sync/atomic"
	"testing"
)

// A refugee placement in flight (PATCHES §67). While one turn of a chat
// places it, the chat's home hold is cleared and its claim on the account it
// goes to is pending until that account answers. Another turn of the chat
// that arrives meanwhile waits for the placement and then follows the chat
// where it went; it is never served on the laptop unmarked.

// The first turn's forward to box B's acct-b is in flight, and box B refuses
// every other dial until it answers. The second turn is held, not served on
// the laptop, and once the first is served on acct-b it is served there too.
func TestATurnOfAChatBeingPlacedWaitsForThePlacement(t *testing.T) {
	rig := newSpreadRig(t, 0, nil, eligibleBoxAccount("acct-b"))
	var armed atomic.Bool
	forwarding, answer := make(chan struct{}), make(chan struct{})
	var answerOnce sync.Once
	boxBAnswers := func() { answerOnce.Do(func() { close(answer) }) }
	defer boxBAnswers()
	cutPeer(t, rig.boxB.pool, func(int64) error {
		if armed.CompareAndSwap(true, false) {
			close(forwarding)
			<-answer
			return nil
		}
		select {
		case <-answer:
			return nil
		default:
			return refusedFailure()
		}
	})
	armed.Store(true)
	first := rig.sendAsync(t, holdTurnBody)
	select {
	case <-forwarding:
	case got := <-first:
		t.Fatalf("calibration: the first turn answered %d %q before its forward to box B; logs:\n%s", got.status, got.body, rig.logs.String())
	}
	second := rig.sendAsync(t, holdTurnBody)
	awaitHeld(t, rig.holdRig, rig.policy, second, "the second turn, while the first is being placed on acct-b")
	boxBAnswers()
	for i, turn := range []<-chan clientAnswer{first, second} {
		got := <-turn
		if place := placeOf(got.status, got.body); place != "acct-b" {
			t.Fatalf("turn %d was served on %s, want acct-b, where the chat was placed; logs:\n%s", i+1, place, rig.logs.String())
		}
	}
	rig.settle(t, 2)
	if n := rig.local.Load(); n != 0 {
		t.Fatalf("the laptop served %d turns of a chat placed on box B; logs:\n%s", n, rig.logs.String())
	}
	refugeeHoldOf(t, rig, "hold-session", "acct-b")
}

// The first turn found box B's acct-b refusing it and is reading the
// laptop's room, which is none, when the second turn arrives: the chat has
// no hold at that moment. The second turn is held with the first, and the
// laptop serves neither.
func TestATurnOfAChatBeingPlacedWaitsWhileThePlacementReadsTheLaptop(t *testing.T) {
	var armed atomic.Bool
	reading, read := make(chan struct{}), make(chan struct{})
	var readOnce sync.Once
	laptopRead := func() { readOnce.Do(func() { close(read) }) }
	defer laptopRead()
	slots := func(peer, _, _ string) int {
		if peer != "" {
			return refugeeSlots
		}
		if armed.CompareAndSwap(true, false) {
			close(reading)
			<-read
		}
		return 0
	}
	rig := newSpreadRig(t, 0, slots, eligibleBoxAccount("acct-b"))
	rig.boxB.mu.Lock()
	rig.boxB.refuse["acct-b"] = "at_cap"
	rig.boxB.mu.Unlock()
	armed.Store(true)
	first := rig.sendAsync(t, holdTurnBody)
	select {
	case <-reading:
	case got := <-first:
		t.Fatalf("calibration: the first turn answered %d %q before it read the laptop's room; logs:\n%s", got.status, got.body, rig.logs.String())
	}
	for _, box := range []*contractBox{rig.boxA, rig.boxB} {
		if account, held := holdAccount(box.pool, "hold-session"); held {
			t.Fatalf("calibration: the chat is held on %s's %q while the first turn reads the laptop", box.pool.name(), account)
		}
	}
	second := rig.sendAsync(t, holdTurnBody)
	awaitHeld(t, rig.holdRig, rig.policy, second, "the second turn, while the first reads the laptop's room")
	laptopRead()
	tries := attempts{count: func() int64 { return int64(rig.countLog(refugeeNotPlacedMessage)) }, kick: func() {}}
	heldFor(t, rig.holdRig, second, tries, 3, "the second turn, with no account to take the chat")
	select {
	case got := <-first:
		t.Fatalf("the first turn answered %d %q, want it held with no account to take the chat; logs:\n%s", got.status, got.body, rig.logs.String())
	default:
	}
	if n := rig.local.Load(); n != 0 {
		t.Fatalf("the laptop, with no room, served %d turns of the chat; logs:\n%s", n, rig.logs.String())
	}
}

// The first turn is reading box B's room, before its claim, when the second
// turn arrives and finds the chat still held on box A, found down. The
// second turn does not place the chat again: it is held, and once the first
// is served on acct-b it is served there too. The chat is placed once.
func TestATurnOfAChatBeingPlacedDoesNotPlaceItAgain(t *testing.T) {
	var armed atomic.Bool
	reading, read := make(chan struct{}), make(chan struct{})
	var readOnce sync.Once
	boxBRead := func() { readOnce.Do(func() { close(read) }) }
	defer boxBRead()
	slots := func(peer, _, _ string) int {
		if peer != "" && armed.CompareAndSwap(true, false) {
			close(reading)
			<-read
		}
		return refugeeSlots
	}
	rig := newSpreadRig(t, 0, slots, eligibleBoxAccount("acct-b"))
	armed.Store(true)
	first := rig.sendAsync(t, holdTurnBody)
	select {
	case <-reading:
	case got := <-first:
		t.Fatalf("calibration: the first turn answered %d %q before it read box B's room; logs:\n%s", got.status, got.body, rig.logs.String())
	}
	if account, held := holdAccount(rig.boxA.pool, "hold-session"); !held || account != "acct-a" {
		t.Fatalf("calibration: the chat's hold on box A is %q (%v) while the first turn reads box B", account, held)
	}
	second := rig.sendAsync(t, holdTurnBody)
	awaitHeld(t, rig.holdRig, rig.policy, second, "the second turn, while the first reads box B's room")
	boxBRead()
	for i, turn := range []<-chan clientAnswer{first, second} {
		got := <-turn
		if place := placeOf(got.status, got.body); place != "acct-b" {
			t.Fatalf("turn %d was served on %s, want acct-b, where the chat was placed; logs:\n%s", i+1, place, rig.logs.String())
		}
	}
	rig.settle(t, 2)
	if lines := rig.logRecords(refugeePlacedMessage); len(lines) != 1 {
		t.Fatalf("placed %d times, want once; logs:\n%s", len(lines), rig.logs.String())
	}
	refugeeHoldOf(t, rig, "hold-session", "acct-b")
}
