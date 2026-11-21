package proxy

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/session"
)

// PATCHES §54, end to end on the e2eLaptop/e2eBox harness: the laptop restarts
// with box-held sessions on disk while its own network is still down. Nothing
// here names an API §54 added, so the file runs unchanged against the code
// before it, where the replay fails at its second step.

// e2eOfflineTransport fails every round trip as a dial timeout while offline is
// set: this machine with no network. It wraps both the laptop's peer client and
// its local upstream transport.
type e2eOfflineTransport struct {
	base    http.RoundTripper
	offline *atomic.Bool
}

func (o e2eOfflineTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if o.offline.Load() {
		return nil, &net.OpError{Op: "dial", Net: "tcp4", Err: os.ErrDeadlineExceeded}
	}
	return o.base.RoundTrip(req)
}

// e2eColdLinkTransport fails the next model turn sent through it as a dial
// timeout, once, while cold is set: the first packet on an idle tailnet link,
// or a pooled keep-alive socket the box no longer has. The admin reads (GET)
// pass, so the one failure lands on the forward.
type e2eColdLinkTransport struct {
	base http.RoundTripper
	cold *atomic.Bool
}

func (c e2eColdLinkTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPost && c.cold.CompareAndSwap(true, false) {
		return nil, &net.OpError{Op: "dial", Net: "tcp4", Err: os.ErrDeadlineExceeded}
	}
	return c.base.RoundTrip(req)
}

// e2eWriteHolds writes peer-holds.json as a laptop left it: each session held on
// the box's account, for the rest of an hour-long idle bound. targeted adds the
// bit §54 persists ("the box served this account when targeted at it"); a file
// from an older build has no such field.
func e2eWriteHolds(t *testing.T, path, peer string, holds map[string]string, targeted bool) {
	t.Helper()
	stored := map[string]map[string]any{}
	for sessionID, account := range holds {
		entry := map[string]any{"Until": time.Now().Add(warmSessionIdleTTL - 5*time.Minute), "Account": account, "Peer": peer}
		if targeted {
			entry["Targeted"] = true
		}
		stored[session.ScopedSessionKey("claude", sessionID)] = entry
	}
	body, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// e2eRestartedLaptop is a laptop process that has just started: the holds come
// off disk exactly as PersistHolds loads them (without its 15 s flush ticker,
// which the tests run by hand), it has no peer usage body yet, and every
// session it has not seen reads warm on its local pin (uptime under an hour).
func e2eRestartedLaptop(t *testing.T, box *e2eBox, local map[string]string, holdsPath string, pins map[string]string) (*e2eLaptop, *RemotePool) {
	t.Helper()
	pool := box.newPool()
	pool.delegations.loadFor(holdsPath, time.Now(), pool.name())
	laptop := newE2ELaptop(t, local, pool)
	laptop.active.startedAt = time.Now()
	pool.admin.mu.Lock()
	delete(pool.admin.entries, remotePoolUsagePath)
	pool.admin.mu.Unlock()
	for sessionID, account := range pins {
		if _, err := laptop.store.Put("claude", sessionID, account, ""); err != nil {
			t.Fatal(err)
		}
	}
	return laptop, pool
}

// e2eRewindCooldown ends the peer breaker's cooldown now, under its lock, as if
// its 30 s had passed (no waiting; PATCHES §45).
func e2eRewindCooldown(pool *RemotePool) {
	pool.breaker.mu.Lock()
	pool.breaker.openUntil = time.Now().Add(-time.Millisecond)
	pool.breaker.mu.Unlock()
}

func e2eLineSays(line map[string]any, field string, want any) bool {
	got, ok := line[field]
	return ok && got == want
}

// The 2026-10-09 22:44-22:48 replay. The laptop restarts at 22:44:20 with
// fdcd3806 and 46eeabf7 held on the box's acct-p and d6c27c01 on chip-a;
// its sessions.json pins are the local placeholders (46ee nine-alpha, fdcd
// main, d6c2 fifteen). Its network is down for the first minutes: every peer
// dial and every local upstream dial times out.
const (
	rebootFdcd = "fdcd3806"
	rebootSixE = "46eeabf7"
	rebootD6c2 = "d6c27c01"
)

func e2eRebootHolds() map[string]string {
	return map[string]string{rebootFdcd: "acct-p", rebootSixE: "acct-p", rebootD6c2: "chip-a"}
}

// e2eRebootOffline is the replay's starting point: the box, the holds file, the
// restarted laptop with its placeholder pins, and the laptop's peer client and
// local upstream both offline. It returns the two offline switches too.
func e2eRebootOffline(t *testing.T) (laptop *e2eLaptop, pool *RemotePool, holdsPath string, box *e2eBox, localOffline, peerOffline *atomic.Bool) {
	t.Helper()
	now := time.Now()
	box = newE2EBox(t, map[string]string{
		"acct-p": e2eUsage(now, 10, 3*time.Hour, 30, 100*time.Hour),
		"chip-a": e2eUsage(now, 10, 3*time.Hour, 30, 100*time.Hour),
	})
	e2eMeasure(t, box.server) // the box itself never went down
	holdsPath = filepath.Join(t.TempDir(), "peer-holds.json")
	e2eWriteHolds(t, holdsPath, box.newPool().name(), e2eRebootHolds(), true)
	laptop, pool = e2eRestartedLaptop(t, box, map[string]string{
		"nine-alpha": e2eUsage(now, 10, 3*time.Hour, 20, 120*time.Hour),
		"main":       e2eUsage(now, 10, 3*time.Hour, 20, 120*time.Hour),
		"fifteen":    e2eUsage(now, 10, 3*time.Hour, 20, 120*time.Hour),
	}, holdsPath, map[string]string{rebootSixE: "nine-alpha", rebootFdcd: "main", rebootD6c2: "fifteen"})

	localOffline, peerOffline = &atomic.Bool{}, &atomic.Bool{}
	localOffline.Store(true)
	peerOffline.Store(true)
	pool.client.Transport = e2eOfflineTransport{base: pool.client.Transport, offline: peerOffline}
	laptop.server.Transport = e2eOfflineTransport{base: http.DefaultTransport, offline: localOffline}
	for sessionID := range e2eRebootHolds() {
		if _, held := e2eHold(pool, sessionID); !held {
			t.Fatalf("calibration: %s's hold did not load from disk", sessionID)
		}
	}
	return laptop, pool, holdsPath, box, localOffline, peerOffline
}

// e2eRebootUntilTheNetworkReturns runs steps 1-4 of the replay and returns the
// laptop, its peer, the holds file and the box. peerStaysDown keeps the peer
// blackholed after the laptop's own network returns (the calibration branch).
func e2eRebootUntilTheNetworkReturns(t *testing.T, peerStaysDown bool) (*e2eLaptop, *RemotePool, string, *e2eBox) {
	t.Helper()
	laptop, pool, holdsPath, box, localOffline, peerOffline := e2eRebootOffline(t)

	// Step 1 (22:46:04): fdcd's held turn is strike 1, transient; its local
	// attempt fails too.
	laptop.turn(t, rebootFdcd)
	if failed := e2eLogLines(laptop.logs, "remote pool forward failed"); len(failed) != 1 || !e2eLineSays(failed[0], "breaker_opened", false) {
		t.Fatalf("step 1: forward-failed lines %v, want one strike that did not open the breaker", failed)
	}

	// Step 2 (22:46:10): 46ee's held turn is strike 2 and the breaker opens.
	// Nothing local has answered since the restart, so this is this machine
	// offline, not the box down: every hold stays, in memory and on disk.
	laptop.turn(t, rebootSixE)
	failed := e2eLogLines(laptop.logs, "remote pool forward failed")
	if len(failed) != 2 || !e2eLineSays(failed[1], "breaker_opened", true) {
		t.Fatalf("step 2: forward-failed lines %v, want strike 2 opening the breaker", failed)
	}
	if ended := e2eLogLines(laptop.logs, "remote pool hold ended"); len(ended) != 0 {
		t.Fatalf("step 2: the opening ended holds while nothing on this machine answered: %v", ended)
	}
	if !e2eLineSays(failed[1], "peer_down", false) {
		t.Fatalf("step 2: forward-failed line %v, want peer_down=false", failed[1])
	}
	for sessionID, account := range e2eRebootHolds() {
		if got, held := e2eHold(pool, sessionID); !held || got != account {
			t.Fatalf("step 2: %s held=%v on %q, want still held on %s", sessionID, held, got, account)
		}
	}
	pool.delegations.flush(holdsPath, time.Now())
	if onDisk := heldOnDisk(t, holdsPath); len(onDisk) != 3 {
		t.Fatalf("step 2: peer-holds.json lists %v after the opening, want all 3: a second restart would find none", onDisk)
	}
	for _, decision := range laptop.decisions() {
		if decision["session_state"] == "forced" {
			t.Fatalf("step 2: a forced decision while offline: %v", decision)
		}
	}
	if kept := e2eLogLines(laptop.logs, holdsKeptMessage); len(kept) != 1 || kept[0]["held"] != float64(3) || kept[0]["why"] != "no_answer" {
		t.Fatalf("step 2: holds-kept lines %v, want one naming the 3 holds, why=no_answer", kept)
	}
	if got := laptop.pinnedTo(rebootSixE); got != "nine-alpha" {
		t.Fatalf("step 2: 46ee's pin moved to %q", got)
	}

	// Step 3: the cooldown passes, still offline. d6c2's held turn re-opens the
	// breaker at once, and every hold is still kept.
	e2eRewindCooldown(pool)
	laptop.turn(t, rebootD6c2)
	failed = e2eLogLines(laptop.logs, "remote pool forward failed")
	if len(failed) != 3 || failed[2]["session"] != rebootD6c2 || !e2eLineSays(failed[2], "breaker_opened", true) || !e2eLineSays(failed[2], "peer_down", false) {
		t.Fatalf("step 3: forward-failed lines %v, want d6c2's strike re-opening the breaker with peer_down=false", failed)
	}
	if ended := e2eLogLines(laptop.logs, "remote pool hold ended"); len(ended) != 0 {
		t.Fatalf("step 3: holds ended while still offline: %v", ended)
	}
	if live := len(pool.delegations.live(time.Now())); live != 3 {
		t.Fatalf("step 3: %d holds live, want 3", live)
	}

	// Step 4 (~22:48:13): the laptop's network returns and one local turn
	// answers. The box's breaker is still open, so it is served locally.
	localOffline.Store(false)
	peerOffline.Store(peerStaysDown)
	if got := laptop.turn(t, "c435d05a"); got.Status != http.StatusOK || got.Pool == "remote" {
		t.Fatalf("step 4: status %d pool %q; want a local answer", got.Status, got.Pool)
	}
	return laptop, pool, holdsPath, box
}

// T17: after the network returns, every box session's next turn is served by
// the box on the account it was held on, targeted there, and no local pin ever
// moved. Red before §54 at step 2 (the opening ended every hold).
func TestE2EARebootWithTheNetworkDownKeepsEveryBoxSessionOnItsAccount(t *testing.T) {
	laptop, pool, holdsPath, box := e2eRebootUntilTheNetworkReturns(t, false)

	// Step 5 (the owner's decision): the cooldown passes and the first held
	// forward since the network came back meets a cold link. The breaker
	// re-opens and the turn is served locally, but one strike since recovery
	// is not the box down: every hold is kept, in memory and on disk.
	e2eRewindCooldown(pool)
	var cold atomic.Bool
	cold.Store(true)
	pool.client.Transport = e2eColdLinkTransport{base: pool.client.Transport, cold: &cold}
	keptBefore := len(e2eLogLines(laptop.logs, holdsKeptMessage))
	if got := laptop.turn(t, rebootFdcd); got.Status != http.StatusOK || got.Pool == "remote" {
		t.Fatalf("step 5: status %d pool %q; with the link cold the turn is served locally", got.Status, got.Pool)
	}
	if cold.Load() {
		t.Fatal("calibration: step 5's forward never met the cold link")
	}
	failed := e2eLogLines(laptop.logs, "remote pool forward failed")
	if last := failed[len(failed)-1]; last["session"] != rebootFdcd || !e2eLineSays(last, "breaker_opened", true) || !e2eLineSays(last, "peer_down", false) {
		t.Fatalf("step 5: forward-failed line %v, want fdcd's strike re-opening the breaker with peer_down=false", last)
	}
	if ended := e2eLogLines(laptop.logs, "remote pool hold ended"); len(ended) != 0 {
		t.Fatalf("step 5: one failed forward since the network came back ended holds: %v", ended)
	}
	if kept := e2eLogLines(laptop.logs, holdsKeptMessage); len(kept) != keptBefore+1 || kept[len(kept)-1]["held"] != float64(3) || kept[len(kept)-1]["why"] != "one_strike_since_back" {
		t.Fatalf("step 5: holds-kept lines %v, want one more naming the 3 holds, why=one_strike_since_back", kept)
	}
	for sessionID, account := range e2eRebootHolds() {
		if got, held := e2eHold(pool, sessionID); !held || got != account {
			t.Fatalf("step 5: %s held=%v on %q, want still held on %s", sessionID, held, got, account)
		}
	}
	pool.delegations.flush(holdsPath, time.Now())
	if onDisk := heldOnDisk(t, holdsPath); len(onDisk) != 3 {
		t.Fatalf("step 5: peer-holds.json lists %v after the cold link, want all 3", onDisk)
	}

	// Step 6: the cooldown passes; each session's next turn goes to the box.
	e2eEveryBoxSessionBackOnItsAccount(t, "step 6", laptop, pool, holdsPath, box)
}

// e2eEveryBoxSessionBackOnItsAccount passes the cooldown and sends each held
// session's next turn: the box serves it, targeted at and served on the
// account it was held on, no local pin moves, and the file lists all three.
func e2eEveryBoxSessionBackOnItsAccount(t *testing.T, step string, laptop *e2eLaptop, pool *RemotePool, holdsPath string, box *e2eBox) {
	t.Helper()
	e2eRewindCooldown(pool)
	before := len(box.seen())
	for _, sessionID := range []string{rebootFdcd, rebootSixE, rebootD6c2} {
		want := e2eRebootHolds()[sessionID]
		got := laptop.turn(t, sessionID)
		if got.Status != http.StatusOK || got.Pool != "remote" || got.ServedBy != want {
			t.Fatalf("%s: %s status %d pool %q served by %q; want the box on %s", step, sessionID, got.Status, got.Pool, got.ServedBy, want)
		}
	}
	turns := box.seen()[before:]
	if len(turns) != 3 {
		t.Fatalf("%s: the box saw %d turns, want 3: %+v", step, len(turns), turns)
	}
	for _, turn := range turns {
		want := e2eRebootHolds()[turn.Session]
		if turn.Target != want || turn.Account != want {
			t.Fatalf("%s: box turn %+v; want %s targeted at and served on %s", step, turn, turn.Session, want)
		}
	}
	for sessionID, pin := range map[string]string{rebootSixE: "nine-alpha", rebootFdcd: "main", rebootD6c2: "fifteen"} {
		if got := laptop.pinnedTo(sessionID); got != pin {
			t.Fatalf("%s: %s's local pin moved to %q (was %s)", step, sessionID, got, pin)
		}
	}
	pool.delegations.flush(holdsPath, time.Now())
	if onDisk := heldOnDisk(t, holdsPath); len(onDisk) != 3 {
		t.Fatalf("%s: peer-holds.json lists %v, want all 3", step, onDisk)
	}
	if ended := e2eLogLines(laptop.logs, "remote pool hold ended"); len(ended) != 0 {
		t.Fatalf("%s: holds ended: %v", step, ended)
	}
}

// T17 with ONE offline strike (review fix round 2): the reboot where only one
// held turn tries the box before the network is up. fdcd's turn is strike 1 and
// its local fallback fails too. The network returns and a session pinned
// locally answers. Then, inside the 30 s cooldown of strike 1, 46ee's held
// forward meets a cold link: strike 2 opens the breaker and its turn is served
// locally, but strike 1 left while this machine was offline, so this is one
// strike since recovery and every hold is kept. After the cooldown every box
// session's next turn is served by the box on the account it was held on.
func TestE2EARebootWithOneOfflineStrikeKeepsEveryHoldThroughAColdLink(t *testing.T) {
	laptop, pool, holdsPath, box, localOffline, peerOffline := e2eRebootOffline(t)
	const localSession = "c435d05a"
	if _, err := laptop.store.Put("claude", localSession, "main", ""); err != nil {
		t.Fatal(err)
	}

	laptop.turn(t, rebootFdcd)
	if failed := e2eLogLines(laptop.logs, "remote pool forward failed"); len(failed) != 1 || !e2eLineSays(failed[0], "breaker_opened", false) {
		t.Fatalf("strike 1: forward-failed lines %v, want one strike that did not open the breaker", failed)
	}

	localOffline.Store(false)
	peerOffline.Store(false)
	if got := laptop.turn(t, localSession); got.Status != http.StatusOK || got.Pool == "remote" {
		t.Fatalf("network back: status %d pool %q; want %s's pinned turn answered locally", got.Status, got.Pool, localSession)
	}

	var cold atomic.Bool
	cold.Store(true)
	pool.client.Transport = e2eColdLinkTransport{base: pool.client.Transport, cold: &cold}
	if got := laptop.turn(t, rebootSixE); got.Status != http.StatusOK || got.Pool == "remote" {
		t.Fatalf("cold link: status %d pool %q; with the breaker opened the turn is served locally", got.Status, got.Pool)
	}
	if cold.Load() {
		t.Fatal("calibration: 46ee's forward never met the cold link")
	}
	failed := e2eLogLines(laptop.logs, "remote pool forward failed")
	if len(failed) != 2 || failed[1]["session"] != rebootSixE || !e2eLineSays(failed[1], "breaker_opened", true) || !e2eLineSays(failed[1], "peer_down", false) {
		t.Fatalf("cold link: forward-failed lines %v, want 46ee's strike opening the breaker with peer_down=false: strike 1 left while this machine was offline", failed)
	}
	if ended := e2eLogLines(laptop.logs, "remote pool hold ended"); len(ended) != 0 {
		t.Fatalf("cold link: one failed forward since the network came back ended holds: %v", ended)
	}
	if kept := e2eLogLines(laptop.logs, holdsKeptMessage); len(kept) != 1 || kept[0]["held"] != float64(3) || kept[0]["why"] != "one_strike_since_back" {
		t.Fatalf("cold link: holds-kept lines %v, want one naming the 3 holds, why=one_strike_since_back", kept)
	}
	for sessionID, account := range e2eRebootHolds() {
		if got, held := e2eHold(pool, sessionID); !held || got != account {
			t.Fatalf("cold link: %s held=%v on %q, want still held on %s", sessionID, held, got, account)
		}
	}
	pool.delegations.flush(holdsPath, time.Now())
	if onDisk := heldOnDisk(t, holdsPath); len(onDisk) != 3 {
		t.Fatalf("cold link: peer-holds.json lists %v, want all 3", onDisk)
	}

	e2eEveryBoxSessionBackOnItsAccount(t, "after the cooldown", laptop, pool, holdsPath, box)
}

// T17's calibration branch: the laptop's network returns but the box stays
// down. The first held forward since then fails, and one strike since
// recovery could be a cold link, so it keeps the holds (the owner's decision,
// PATCHES §54). Its turn is served locally, so this machine has answered
// during the outage; the next held forward, a cooldown later, is the second
// strike sent since recovery: the breaker opens as peer_down. That chat is
// placed as a refugee, and every other hold stays until its own chat is
// placed (PATCHES §67).
func TestE2EAfterTheRebootABoxThatStaysDownIsStillFoundDown(t *testing.T) {
	laptop, pool, holdsPath, _ := e2eRebootUntilTheNetworkReturns(t, true)
	keptBefore := len(e2eLogLines(laptop.logs, holdsKeptMessage))

	e2eRewindCooldown(pool)
	if got := laptop.turn(t, rebootFdcd); got.Status != http.StatusOK || got.Pool == "remote" {
		t.Fatalf("status %d pool %q; with the box down the turn is served locally", got.Status, got.Pool)
	}
	failed := e2eLogLines(laptop.logs, "remote pool forward failed")
	if last := failed[len(failed)-1]; last["session"] != rebootFdcd || !e2eLineSays(last, "breaker_opened", true) || !e2eLineSays(last, "peer_down", false) {
		t.Fatalf("forward-failed line %v, want fdcd's strike, the first since recovery, re-opening the breaker with peer_down=false", last)
	}
	if live := len(pool.delegations.live(time.Now())); live != 3 {
		t.Fatalf("%d holds live after one strike since recovery, want 3", live)
	}
	if kept := e2eLogLines(laptop.logs, holdsKeptMessage); len(kept) != keptBefore+1 || kept[keptBefore]["why"] != "one_strike_since_back" {
		t.Fatalf("holds-kept lines %v, want one more for fdcd's re-opening, why=one_strike_since_back", kept[keptBefore:])
	}
	keptBefore++

	e2eRewindCooldown(pool)
	if got := laptop.turn(t, rebootSixE); got.Status != http.StatusOK || got.Pool == "remote" {
		t.Fatalf("status %d pool %q; with the box down the turn is served locally", got.Status, got.Pool)
	}
	failed = e2eLogLines(laptop.logs, "remote pool forward failed")
	last := failed[len(failed)-1]
	if last["session"] != rebootSixE || !e2eLineSays(last, "breaker_opened", true) || !e2eLineSays(last, "peer_down", true) {
		t.Fatalf("forward-failed line %v, want 46ee's strike, the second since recovery, opening the breaker with peer_down=true", last)
	}
	if kept := e2eLogLines(laptop.logs, holdsKeptMessage); len(kept) != keptBefore {
		t.Fatalf("holds-kept lines %v after the box was found down; an attributed opening keeps nothing and must not say it did", kept[keptBefore:])
	}
	// The box found down ends no hold (PATCHES §67): the tripping session is
	// placed as a refugee, and the other two keep their holds until their
	// own next turns place them.
	if ended := e2eLogLines(laptop.logs, "remote pool hold ended"); len(ended) != 0 {
		t.Fatalf("hold-ended lines %v; a box found down ends no hold", ended)
	}
	if live := pool.delegations.live(time.Now()); len(live) != len(e2eRebootHolds())-1 {
		t.Fatalf("holds %v after the box was found down; want every hold but the placed chat's", live)
	}
	pool.delegations.flush(holdsPath, time.Now())
	if onDisk := heldOnDisk(t, holdsPath); len(onDisk) != len(e2eRebootHolds())-1 {
		t.Fatalf("peer-holds.json lists %v after the box was found down; want the holds it kept", onDisk)
	}
	placed := awaitLogLines(t, laptop.logs, refugeePlacedMessage, 1)
	if len(placed) != 1 || placed[0]["session"] != rebootSixE {
		t.Fatalf("placement lines %v, want 46ee's turn placed as a refugee", placed)
	}
}

// The design's idle-laptop row (review round 2): the box really is down, the
// laptop is online, and its only traffic is its box-held sessions. Nothing
// local has answered since the restart and no test seeds an answer. Strike 1's
// turn is served locally through Server.transport(), and its headers are this
// machine answering during the outage, so strike 2 is the box's: the box is
// found down at the second strike, and only the chat that found it is placed
// (PATCHES §67).
func TestE2EADeadBoxOnALaptopIdleApartFromItsHeldSessionsIsFoundDown(t *testing.T) {
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

	if got := laptop.turn(t, rebootFdcd); got.Status != http.StatusOK || got.Pool == "remote" {
		t.Fatalf("strike 1: status %d pool %q; want fdcd's turn served locally", got.Status, got.Pool)
	}
	if failed := e2eLogLines(laptop.logs, "remote pool forward failed"); len(failed) != 1 || !e2eLineSays(failed[0], "breaker_opened", false) {
		t.Fatalf("strike 1: forward-failed lines %v, want one strike that did not open the breaker", failed)
	}
	if got := laptop.turn(t, rebootSixE); got.Status != http.StatusOK || got.Pool == "remote" {
		t.Fatalf("strike 2: status %d pool %q; want 46ee's turn served locally", got.Status, got.Pool)
	}
	failed := e2eLogLines(laptop.logs, "remote pool forward failed")
	if len(failed) != 2 || failed[1]["session"] != rebootSixE || !e2eLineSays(failed[1], "breaker_opened", true) || !e2eLineSays(failed[1], "peer_down", true) {
		t.Fatalf("strike 2: forward-failed lines %v, want 46ee's strike opening the breaker with peer_down=true: fdcd's local answer is the evidence", failed)
	}
	if live := pool.delegations.live(time.Now()); len(live) != len(e2eRebootHolds())-1 {
		t.Fatalf("holds %v after the box was found down; want every hold but the placed chat's (PATCHES §67)", live)
	}
	if kept := e2eLogLines(laptop.logs, holdsKeptMessage); len(kept) != 0 {
		t.Fatalf("holds-kept lines %v after the box was found down", kept)
	}
}

// T16: a restart loses the box's usage body. When the first fetch misses the
// 250 ms decision budget (the box is slow) or fails, a hold the box served
// targeted keeps its target; a hold from an older file stays untargeted, as
// before; and a box that then serves another account clears the hold and is
// marked as ignoring targets.
func TestARestartedHeldTurnWithNoUsageBodyStillTargetsItsAccount(t *testing.T) {
	now := time.Now()
	local := map[string]string{"main": e2eUsage(now, 10, 3*time.Hour, 20, 120*time.Hour)}
	for _, tc := range []struct {
		name     string
		targeted bool
		// ignore: the box keeps publishing verdicts but places the turn itself.
		ignore bool
		// legacy: an old box whose usage body arrives, with no verdicts in it.
		legacy     bool
		wantTarget string
	}{
		{name: "a targeted hold stays targeted", targeted: true, wantTarget: "acct-p"},
		{name: "a hold from an older file stays untargeted", targeted: false, wantTarget: ""},
		{name: "a box that substitutes ends the hold", targeted: true, ignore: true, wantTarget: "acct-p"},
		{name: "a body without verdicts still blanks the target", targeted: true, legacy: true, wantTarget: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := newE2EBox(t, map[string]string{
				// acct-p is the busier of the two, so a box that places the
				// turn itself puts it on chip-a.
				"acct-p": e2eUsage(now, 40, 3*time.Hour, 60, 100*time.Hour),
				"chip-a": e2eUsage(now, 5, 3*time.Hour, 5, 100*time.Hour),
			})
			e2eMeasure(t, box.server)
			box.mu.Lock()
			// Blackholed: the laptop's admin read gives up after 3 s and no
			// body ever arrives, so every held turn here decides without one.
			// The legacy box answers instead, with a body that has no verdicts.
			if !tc.legacy {
				box.usageDelay = time.Minute
			}
			box.legacy = tc.legacy
			box.ignoreTarget = tc.ignore
			box.mu.Unlock()
			holdsPath := filepath.Join(t.TempDir(), "peer-holds.json")
			e2eWriteHolds(t, holdsPath, box.newPool().name(), map[string]string{"s1": "acct-p", "s2": "acct-p"}, tc.targeted)
			laptop, pool := e2eRestartedLaptop(t, box, local, holdsPath, map[string]string{"s1": "main", "s2": "main"})
			if tc.legacy {
				// The body is in before the held turn decides: fetched here,
				// waiting for it, not raced against the 250 ms budget.
				if body := pool.usageForDecision(time.Now().Add(10 * time.Second)).Body; len(body) == 0 {
					t.Fatal("calibration: the legacy box's usage body did not arrive")
				}
			}

			got := laptop.turn(t, "s1")
			turns := box.seen()
			if len(turns) != 1 {
				t.Fatalf("the box saw %d turns, want the held turn: %+v", len(turns), turns)
			}
			if arrived := len(pool.cachedUsage().Body) != 0; arrived != tc.legacy {
				t.Fatalf("calibration: the box's usage body arrived=%v, want %v", arrived, tc.legacy)
			}
			if turns[0].Target != tc.wantTarget {
				t.Fatalf("the held turn reached the box targeted at %q, want %q", turns[0].Target, tc.wantTarget)
			}
			switch {
			case tc.ignore:
				if got.ServedBy != "chip-a" {
					t.Fatalf("calibration: the box placed the turn on %q, want chip-a", got.ServedBy)
				}
				if _, held := e2eHold(pool, "s1"); held {
					t.Fatal("the box served another account than the target and the session stayed held")
				}
				if len(e2eLogLines(laptop.logs, "remote pool peer substituted the target")) != 1 {
					t.Fatal("no substitution line for a box that served another account")
				}
				if !pool.ignoresTargets(pool.cachedUsage().At) {
					t.Fatal("the box was not marked as ignoring targets")
				}
				// s2's hold is targeted too, but the box has just been seen
				// ignoring a target: its held turn goes untargeted (X1).
				laptop.turn(t, "s2")
				if turns := box.seen(); len(turns) != 2 || turns[1].Session != "s2" || turns[1].Target != "" {
					t.Fatalf("box turns %+v; s2's held turn must reach the box untargeted after the substitution", turns)
				}
			case tc.legacy:
				// Untargeted, the legacy box placed the turn by its own rules,
				// as it always has.
			case tc.targeted:
				if got.ServedBy != "acct-p" {
					t.Fatalf("served by %q, want acct-p", got.ServedBy)
				}
				if account, held := e2eHold(pool, "s1"); !held || account != "acct-p" {
					t.Fatalf("hold after the turn = %q/%v, want acct-p", account, held)
				}
			}
		})
	}
}
