package proxy

// Where a LEGACY peer (no verdicts, or seen ignoring a target) may win the one
// ranking (crossPoolRanking.targetedOnly, PATCHES §31), and a spent window
// that arrives as a targeted 5xx. Real laptop against real box
// (crosspool_e2e_test.go helpers). The first two were written as review
// probes: green on origin/main, red on the branch before targetedOnly.

import (
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// New laptop + OLD box. A warm local session is FORCED off its account
// mid-turn (fourteen's window is spent) and local failover has fifteen.
// origin/main never asked the peer here: local failover served the turn on
// fifteen. An old box can only be sent the turn untargeted, where it places
// the session by its own pre-§31 rules -- onto chip-5 past the one session its
// taper allows, the 9/27 path -- so it is not a candidate on this turn.
func TestCrossPoolForcedSwitchNeverSpillsUntargetedToALegacyPeer(t *testing.T) {
	box := e2eIncidentBox(t)
	box.legacy = true
	laptop := newE2ELaptop(t, e2eIncidentLocal(), box.newPool())
	e2ePrime(t, laptop.server.RemotePool)
	if _, err := laptop.store.Put("claude", "s1", "fourteen", ""); err != nil {
		t.Fatal(err)
	}
	markIdle(laptop.active, "s1", time.Minute) // warm on fourteen
	defer box.live(t, "chip-5")()              // chip-5 carries the one session its taper allows
	laptop.upstream.setAnswer(func(account string, w http.ResponseWriter) bool {
		if account == "fourteen" {
			e2eRejected(w)
			return true
		}
		return false
	})
	got := laptop.turn(t, "s1")
	if turns := box.seen(); len(turns) != 0 {
		t.Fatalf("a forced switch with local fifteen available was forwarded to the old box (%+v); origin/main served it on fifteen", turns)
	}
	if got.Status != http.StatusOK || got.ServedBy != "fifteen" {
		t.Fatalf("status %d served by %q; want local failover on fifteen", got.Status, got.ServedBy)
	}
	if account, held := e2eHold(laptop.server.RemotePool, "s1"); held {
		t.Fatalf("s1 held on the old box (%q)", account)
	}
	if lines := box.logLines("selected OAuth account below new-session headroom; routing optimistically"); len(lines) != 0 {
		t.Fatalf("the old box routed %d turns optimistically", len(lines))
	}
}

// Same class, P9 site: a COLD session whose own account is still the best
// LOCAL one, with an OLD box. origin/main never compared the peer for this
// turn (the P9 gap); a peer with verdicts now may take it
// (TestE2EColdSessionIsRerankedAcrossEveryPoolAndAWarmOneIsNot), an old box
// may not, and the decision line says why.
func TestCrossPoolColdRetainedSessionIsNotSentUntargetedToALegacyPeer(t *testing.T) {
	box := e2eIncidentBox(t)
	box.legacy = true
	laptop := newE2ELaptop(t, e2eIncidentLocal(), box.newPool())
	e2ePrime(t, laptop.server.RemotePool)
	if _, err := laptop.store.Put("claude", "s1", "fifteen", ""); err != nil {
		t.Fatal(err)
	}
	markIdle(laptop.active, "s1", warmSessionIdleTTL+time.Minute)
	defer box.live(t, "chip-5")()
	got := laptop.turn(t, "s1")
	if turns := box.seen(); len(turns) != 0 {
		t.Fatalf("a cold session retained on its best local account was forwarded untargeted to the old box (%+v)", turns)
	}
	if got.Status != http.StatusOK || got.ServedBy != "fifteen" {
		t.Fatalf("status %d served by %q; want fifteen", got.Status, got.ServedBy)
	}
	if account, held := e2eHold(laptop.server.RemotePool, "s1"); held {
		t.Fatalf("s1 held on the old box (%q)", account)
	}
	decisions := laptop.decisions()
	if len(decisions) != 1 || decisions[0]["session_state"] != "cold" || decisions[0]["reason"] != crossPoolReasonLegacyNotAsked {
		t.Fatalf("decisions %v; want one cold decision saying the old box is not asked", decisions)
	}
	if moves := e2eLogLines(laptop.logs, "re-ranking idle session"); len(moves) != 0 {
		t.Fatalf("logged a move for a session that stayed on its account: %v", moves)
	}
}

// CHARACTERIZATION (passes on origin/main + the e2e harness, and on the
// branch before targetedOnly):
// a cold session its local ranking MOVES (fourteen -> fifteen) reached
// origin/main's peer comparison with a real score, so an old box competed on
// its best score and won untargeted. That is today's behaviour for a legacy
// peer (D1) and targetedOnly must not take it away.
func TestCrossPoolColdSessionMovedLocallyStillConsultsALegacyPeer(t *testing.T) {
	box := e2eIncidentBox(t)
	box.legacy = true
	laptop := newE2ELaptop(t, e2eIncidentLocal(), box.newPool())
	e2ePrime(t, laptop.server.RemotePool)
	if _, err := laptop.store.Put("claude", "s1", "fourteen", ""); err != nil {
		t.Fatal(err)
	}
	markIdle(laptop.active, "s1", warmSessionIdleTTL+time.Minute)
	got := laptop.turn(t, "s1")
	turns := box.seen()
	if got.Status != http.StatusOK || e2eLocalAccount(got.ServedBy) || len(turns) != 1 || turns[0].Target != "" {
		t.Fatalf("status %d served by %q, box turns %+v; want today's call: one untargeted forward to the old box", got.Status, got.ServedBy, turns)
	}
}

// A 5xx that carries anthropic-ratelimit-unified-status: rejected is the
// target's window being spent, not provider overload
// (targetedOverloadRetryTransport, stampTargetedResponse). The box does not
// retry it and refuses it (forced), so the laptop ends the hold on chip-5 and
// recomputes: nothing else on the incident box is eligible, so a local account
// serves this very turn. Kills the fix-up-1 mutants N7 (stamped overload: the
// client got the 529, the session stayed held on the spent chip-5) and N6
// (retried the spent window three times with backoff).
func TestCrossPoolRejected5xxOnATargetIsASpentWindowNotOverload(t *testing.T) {
	box := e2eIncidentBox(t)
	laptop := newE2ELaptop(t, e2eIncidentLocal(), box.newPool())
	e2ePrime(t, laptop.server.RemotePool)
	if got := laptop.turn(t, "s1"); got.ServedBy != "chip-5" {
		t.Fatalf("turn 1 served by %q; want chip-5", got.ServedBy)
	}
	box.upstream.setAnswer(func(account string, w http.ResponseWriter) bool {
		if account != "chip-5" {
			return false
		}
		w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
		w.Header().Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10))
		w.WriteHeader(529)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`)
		return true
	})
	got := laptop.turn(t, "s1")
	if got.Status != http.StatusOK || !e2eLocalAccount(got.ServedBy) {
		t.Fatalf("turn 2: status %d served by %q, box turns %+v; a rejected 5xx is a spent window: refuse, recompute, serve locally", got.Status, got.ServedBy, box.seen())
	}
	if account, held := e2eHold(laptop.server.RemotePool, "s1"); held {
		t.Fatalf("s1 still held on the box's %q after its window was spent", account)
	}
	if hits := box.upstream.accounts(); len(hits) != 2 {
		t.Fatalf("box upstream hits %v; want 2 (turn 1, the rejected 5xx): a spent window is not retried as overload", hits)
	}
}
