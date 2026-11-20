package proxy

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/session"
)

// PATCHES §54: a breaker opening ends the holds on a peer only when this
// machine can attribute the outage to the peer. These pin the rule on the
// breaker itself (whyNotThePeers and failure, fed by localEgress) and the
// observer that feeds it. The end-to-end replay of the 2026-10-09 reboot is in
// crosspool_reboot_test.go.

// localUpstreamAnswered records one local upstream answer on a new
// connection, now, on each pool: what localEgressObserver does when a local
// turn opens a connection and gets its headers. Tests of a really dead peer
// call it between the strikes.
func localUpstreamAnswered(pools ...*RemotePool) {
	now := time.Now()
	for _, p := range pools {
		p.egress.noteAnswerOnANewConnection(now)
	}
}

// noteAnswerOnANewConnection is a local turn that opened a new connection and
// got its headers at at. The tests of the answer rule feed this: an answer on
// a connection opened before the outage is the gateway of 2026-11-19
// (gateway_attribution_test.go).
func (e *localEgress) noteAnswerOnANewConnection(at time.Time) {
	e.noteConnected(at)
	e.noteAnswer(at)
}

// attributionClock is a fixed wall-clock origin: the breaker compares times,
// never reads the clock, so no test here waits on anything.
var attributionClock = time.Unix(1_800_000_000, 0)

func at(offset time.Duration) time.Time { return attributionClock.Add(offset) }

// strikeAt fails one peer request sent at started, seen to fail 2 s later (a
// dial timeout), with this machine's evidence as egress holds it now.
func strikeAt(b *remotePoolBreaker, egress *localEgress, started time.Time, err error) (opened, peerDown bool) {
	opened, peerDown, _ = strikeWhyAt(b, egress, started, err)
	return opened, peerDown
}

// strikeWhyAt is strikeAt with the reason an opening kept the holds.
func strikeWhyAt(b *remotePoolBreaker, egress *localEgress, started time.Time, err error) (opened, peerDown bool, why string) {
	return b.failure(started, started.Add(remotePoolDialTimeout), err, egress, 0)
}

// T1: a fresh process whose network is not up yet. Nothing local has answered,
// so two strikes open the breaker (the cost stays bounded) without blaming the
// peer, and the breaker stays one strike from re-opening.
func TestBreakerOpenWithNothingAnsweredSinceStartIsNotThePeers(t *testing.T) {
	var b remotePoolBreaker
	var egress localEgress
	if opened, _ := strikeAt(&b, &egress, at(0), errPeerTimeout); opened {
		t.Fatal("calibration: one failure opened the breaker")
	}
	opened, peerDown, why := strikeWhyAt(&b, &egress, at(5*time.Second), errPeerTimeout)
	if !opened {
		t.Fatal("two failures did not open the breaker: an unreachable peer must still cost a bounded wait")
	}
	if peerDown || why != keptNoAnswer {
		t.Fatalf("peerDown=%v why=%q; with nothing local answering since the process started, want the holds kept why=no_answer", peerDown, why)
	}
	if b.allow(at(10 * time.Second)) {
		t.Fatal("an unattributed opening let the next request through inside the cooldown")
	}
	if b.failures != remotePoolFailuresToOpen-1 {
		t.Fatalf("failures after an unattributed opening = %d, want %d (one strike from re-opening)", b.failures, remotePoolFailuresToOpen-1)
	}
}

// T2: a really dead peer. This machine's own upstream answered after the
// outage began, so the opening is the peer's: every hold on it ends, as before.
func TestBreakerOpenAfterALocalAnswerDuringTheOutageIsThePeers(t *testing.T) {
	var b remotePoolBreaker
	var egress localEgress
	strikeAt(&b, &egress, at(0), errPeerTimeout)
	egress.noteAnswerOnANewConnection(at(3 * time.Second))
	opened, peerDown := strikeAt(&b, &egress, at(5*time.Second), errPeerTimeout)
	if !opened || !peerDown {
		t.Fatalf("opened=%v peerDown=%v; a peer that fails while this machine's upstream answers is down", opened, peerDown)
	}
	if b.failures != 0 {
		t.Fatalf("failures after the peer's opening = %d, want 0 (as before §54)", b.failures)
	}
}

// T2, a busy laptop: local turns answer before the outage, between the strikes
// and while the opening strike is still in flight, with no local failure among
// them. The egress has been up since the FIRST of those answers; a later one
// landing mid-flight is not the network coming back (the race guard), so a
// dead peer is still blamed. Were upSince the latest answer, a laptop whose
// turns keep answering would never fail over from a dead box.
func TestABusyLaptopWhoseTurnsKeepAnsweringBlamesADeadPeer(t *testing.T) {
	var b remotePoolBreaker
	var egress localEgress
	egress.noteAnswerOnANewConnection(at(-60 * time.Second))
	strikeAt(&b, &egress, at(0), errPeerTimeout)
	egress.noteAnswerOnANewConnection(at(1 * time.Second))
	secondSent := at(3 * time.Second)
	egress.noteAnswerOnANewConnection(at(4 * time.Second))
	opened, peerDown, _ := b.failure(secondSent, secondSent.Add(remotePoolResponseHeaderTimeout), errPeerTimeout, &egress, 0)
	if !opened || !peerDown {
		t.Fatalf("opened=%v peerDown=%v; a busy laptop whose local turns keep answering must blame a dead peer", opened, peerDown)
	}
	if up := egress.view().upSince; !up.Equal(at(-60 * time.Second)) {
		t.Fatalf("upSince = %v, want the first answer since the last failure (%v)", up, at(-60*time.Second))
	}
}

// T3: an idle laptop whose network drops. Its last local answer came before the
// outage began, which says nothing about the network now.
func TestALocalAnswerFromBeforeTheOutageDoesNotBlameThePeer(t *testing.T) {
	var b remotePoolBreaker
	var egress localEgress
	egress.noteAnswerOnANewConnection(at(-10 * time.Second))
	strikeAt(&b, &egress, at(0), errPeerTimeout)
	if opened, peerDown, why := strikeWhyAt(&b, &egress, at(3*time.Second), errPeerTimeout); !opened || peerDown || why != keptNoAnswer {
		t.Fatalf("opened=%v peerDown=%v why=%q; an answer from before the outage must not blame the peer (why=no_answer)", opened, peerDown, why)
	}
}

// T3, with a strike left over (review round 2). A one-off blip on the box's
// path half an hour ago left one strike, its turn was served locally, and no
// forward has reached the box since: its sessions sat idle. Then this
// machine's own network drops (an i/o timeout, not no-route), and an idle
// box-held session resumes before anything local has failed. Its strike is
// the second, so the breaker opens as before, but the outage it judges starts
// at that strike, not at the leftover one: the answer from just before the
// drop is not "during" it, and every hold is kept.
//
// A box that really is down is still found: that opening was unattributed, so
// its series is kept, and the tripping turn's own local answer is then
// evidence for the strikes after it. Since review fix round 1 it takes two of
// them, not one: the opening kept the holds for no_answer, so this machine
// came back only at that local answer, and the opening's own strike, sent
// before it, is not one of the two strikes since recovery (one cold link
// after the drop keeps every hold).
func TestALeftoverStrikeDoesNotDateTheNextOutage(t *testing.T) {
	var b remotePoolBreaker
	var egress localEgress
	egress.noteAnswerOnANewConnection(at(-2 * time.Hour))
	strikeAt(&b, &egress, at(-30*time.Minute), errPeerTimeout)
	egress.noteAnswerOnANewConnection(at(-30*time.Minute + 3*time.Second)) // the blip's turn, served locally
	egress.noteAnswerOnANewConnection(at(-10 * time.Second))               // the last local answer before the drop
	opened, peerDown := strikeAt(&b, &egress, at(0), errPeerTimeout)
	if !opened {
		t.Fatal("a leftover strike and a new one did not open the breaker: the cost bound must stay two strikes")
	}
	if peerDown {
		t.Fatalf("opened=%v peerDown=%v: an answer from before this machine's network drop blamed the peer (series start %v)", opened, peerDown, b.seriesStarted)
	}
	if !b.seriesStarted.Equal(at(0)) {
		t.Fatalf("seriesStarted = %v, want the new strike's %v: a strike more than a cooldown old does not date this outage", b.seriesStarted, at(0))
	}

	egress.noteAnswerOnANewConnection(at(5 * time.Second)) // the tripping turn, served locally
	afterCooldown := at(remotePoolDialTimeout + remotePoolCooldown + time.Second)
	if opened, peerDown, why := strikeWhyAt(&b, &egress, afterCooldown, errPeerTimeout); !opened || peerDown || why != keptOneStrikeSinceBack {
		t.Fatalf("opened=%v peerDown=%v why=%q after the cooldown; the first strike since the tripping turn's local answer must keep the holds (why=one_strike_since_back)", opened, peerDown, why)
	}
	down := afterCooldown.Add(remotePoolDialTimeout + remotePoolCooldown + time.Second)
	if opened, peerDown := strikeAt(&b, &egress, down, errPeerTimeout); !opened || !peerDown {
		t.Fatalf("opened=%v peerDown=%v after two cooldowns; a box still failing while this machine answers is down", opened, peerDown)
	}

	// That peer_down opening ends the kept series too: a strike left over
	// after it does not date a later drop either.
	later := down.Add(time.Hour)
	strikeAt(&b, &egress, later, errPeerTimeout)
	egress.noteAnswerOnANewConnection(later.Add(30*time.Minute - 10*time.Second))
	if opened, peerDown := strikeAt(&b, &egress, later.Add(30*time.Minute), errPeerTimeout); !opened || peerDown {
		t.Fatalf("opened=%v peerDown=%v; a strike left over after a peer_down opening dated a later network drop", opened, peerDown)
	}
}

// T4: an answer during the outage that a local transport failure followed: the
// network went away again, so it no longer counts.
func TestALocalFailureAfterTheLastAnswerUnblamesThePeer(t *testing.T) {
	var b remotePoolBreaker
	var egress localEgress
	strikeAt(&b, &egress, at(0), errPeerTimeout)
	egress.noteAnswerOnANewConnection(at(3 * time.Second))
	egress.noteFailure(at(4 * time.Second))
	if opened, peerDown := strikeAt(&b, &egress, at(5*time.Second), errPeerTimeout); !opened || peerDown {
		t.Fatalf("opened=%v peerDown=%v; a local failure after the last answer must not blame the peer", opened, peerDown)
	}

	// The race the view guards: an answer noted after a later failure reset
	// upSince (two round trips ending together) is not the egress coming up.
	var raced localEgress
	raced.noteFailure(at(10 * time.Second))
	raced.noteAnswerOnANewConnection(at(9 * time.Second))
	if up := raced.view().upSince; !up.IsZero() {
		t.Fatalf("upSince = %v from an answer older than the last failure", up)
	}
	raced.noteAnswerOnANewConnection(at(11 * time.Second))
	if up := raced.view().upSince; !up.Equal(at(11 * time.Second)) {
		t.Fatalf("upSince = %v, want the first answer after the failure (%v)", up, at(11*time.Second))
	}
}

// T5: the network comes back while the opening strike is in flight. That
// request left with no route, so its failure is this machine's, not the
// peer's; the next failure decides.
func TestNetworkBackMidStrikeIsNotThePeers(t *testing.T) {
	var b remotePoolBreaker
	var egress localEgress
	strikeAt(&b, &egress, at(0), errPeerTimeout)
	secondSent := at(3 * time.Second)
	egress.noteAnswerOnANewConnection(at(4 * time.Second))
	// A header timeout: it fails 30 s after it left, long after the answer.
	opened, peerDown, why := b.failure(secondSent, secondSent.Add(remotePoolResponseHeaderTimeout), errPeerTimeout, &egress, 0)
	if !opened || peerDown || why != keptNetworkBackMidStrike {
		t.Fatalf("opened=%v peerDown=%v why=%q; a request sent before the egress came back must not blame the peer (why=network_back_mid_strike)", opened, peerDown, why)
	}
}

// T6: the kernel's own no-route verdict is never the peer's, however the
// error is wrapped and whatever else answered (7 of the laptop's 14 openings).
func TestNoRouteErrorIsNeverThePeers(t *testing.T) {
	wrap := func(errno syscall.Errno) error {
		return &url.Error{Op: "Post", URL: "http://100.64.0.9:31416/v1/messages",
			Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", errno)}}
	}
	for _, tc := range []struct {
		name     string
		err      error
		peerDown bool
		why      string
	}{
		{"ENETUNREACH", wrap(syscall.ENETUNREACH), false, keptNoRoute},
		{"EADDRNOTAVAIL", wrap(syscall.EADDRNOTAVAIL), false, keptNoRoute},
		{"ENETDOWN", wrap(syscall.ENETDOWN), false, keptNoRoute},
		// Calibration: the peer's own refusal, the same answering egress.
		{"ECONNREFUSED", wrap(syscall.ECONNREFUSED), true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b remotePoolBreaker
			var egress localEgress
			strikeAt(&b, &egress, at(0), tc.err)
			egress.noteAnswerOnANewConnection(at(time.Second))
			opened, peerDown, why := strikeWhyAt(&b, &egress, at(5*time.Second), tc.err)
			if !opened || peerDown != tc.peerDown || why != tc.why {
				t.Fatalf("opened=%v peerDown=%v why=%q, want opened, peerDown=%v why=%q", opened, peerDown, why, tc.peerDown, tc.why)
			}
		})
	}
}

// This machine's own kernel saying it has no route (7 of the laptop's 14
// openings) is never the peer's, even with local turns answering throughout.
// The forward path keeps every hold, and both lines say why: peer_down=false
// on the opening forward, no_route=true on the holds-kept line.
func TestANoRouteOpeningKeepsTheHoldsAndSaysWhy(t *testing.T) {
	server, key := heldSessionServer(t, "http://100.64.0.9:31416")
	server.RemotePool.client.Transport = proxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ENETUNREACH)}
	})
	logs := &syncBuffer{}
	server.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	for i := 0; i < remotePoolFailuresToOpen; i++ {
		localUpstreamAnswered(server.RemotePool)
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
		server.forwardToPeer(server.RemotePool, req, "claude", "s1", "/v1/messages", "", []byte(`{}`), remotePoolHoldSet, "peer-acct")
	}
	if server.RemotePool.breaker.allow(time.Now()) {
		t.Fatal("calibration: the breaker did not open")
	}
	if !server.RemotePool.delegations.active(key, time.Now()) {
		t.Fatal("an opening on this machine's own network-unreachable ended the hold")
	}
	failed := e2eLogLines(logs, "remote pool forward failed")
	if len(failed) != remotePoolFailuresToOpen || failed[1]["breaker_opened"] != true || failed[1]["peer_down"] != false {
		t.Fatalf("forward-failed lines %v; want the second opening the breaker with peer_down=false", failed)
	}
	kept := e2eLogLines(logs, holdsKeptMessage)
	if len(kept) != 1 || kept[0]["no_route"] != true || kept[0]["why"] != keptNoRoute || kept[0]["held"] != float64(1) {
		t.Fatalf("holds-kept lines %v; want one with why=no_route, no_route=true and held=1", kept)
	}
	if age, ok := kept[0]["local_answer_age_ms"].(float64); !ok || age < 0 {
		t.Fatalf("holds-kept line %v; local turns answered, so local_answer_age_ms must be an age, not -1", kept[0])
	}
}

// T2 through the forward path: the egress is read when the opening strike's
// failure is seen, not when its request left. A laptop that is online but idle
// has been answering since before the outage, and its only local answer since
// the series began lands while the opening strike waits on the box. That is
// an answer during the outage, so the dead box is blamed: every hold ends and
// the line says peer_down=true. Read at send, it would keep the holds for
// another cooldown (review round 2).
func TestAnAnswerDuringTheOpeningForwardsFlightBlamesTheDeadPeer(t *testing.T) {
	server, key := heldSessionServer(t, "http://100.64.0.9:31416")
	pool := server.RemotePool
	pool.egress.noteAnswerOnANewConnection(time.Now().Add(-time.Minute))
	var sent atomic.Int32
	pool.client.Transport = proxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		if sent.Add(1) == remotePoolFailuresToOpen {
			localUpstreamAnswered(pool) // a local turn gets its headers meanwhile
		}
		return nil, errPeerTimeout
	})
	logs := &syncBuffer{}
	server.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	for i := 0; i < remotePoolFailuresToOpen; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
		server.forwardToPeer(pool, req, "claude", "s1", "/v1/messages", "", []byte(`{}`), remotePoolHoldSet, "peer-acct")
	}
	failed := e2eLogLines(logs, "remote pool forward failed")
	if len(failed) != remotePoolFailuresToOpen || !e2eLineSays(failed[1], "breaker_opened", true) || !e2eLineSays(failed[1], "peer_down", true) {
		t.Fatalf("forward-failed lines %v; an answer during the opening strike's flight, egress up all along, must blame the peer", failed)
	}
	if !pool.breaker.foundDown() || !pool.delegations.active(key, time.Now()) {
		t.Fatal("want the box found down with the hold kept until its chat is placed (PATCHES §67)")
	}
}

// The same on the websocket dial.
func TestAnAnswerDuringTheOpeningDialsFlightBlamesTheDeadPeer(t *testing.T) {
	server := heldCodexWebSocketServer(t, "http://100.64.0.9:31416")
	pool := server.RemotePool
	pool.egress.noteAnswerOnANewConnection(time.Now().Add(-time.Minute))
	var sent atomic.Int32
	pool.wsDialer.Proxy = nil
	pool.wsDialer.NetDialContext = func(context.Context, string, string) (net.Conn, error) {
		if sent.Add(1) == remotePoolFailuresToOpen {
			localUpstreamAnswered(pool)
		}
		return nil, errPeerTimeout
	}
	logs := &syncBuffer{}
	server.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	for i := 0; i < remotePoolFailuresToOpen; i++ {
		forwardHeldWebSocket(t, server, context.Background())
	}
	failed := e2eLogLines(logs, "remote pool websocket dial failed")
	if len(failed) != remotePoolFailuresToOpen || !e2eLineSays(failed[1], "breaker_opened", true) || !e2eLineSays(failed[1], "peer_down", true) {
		t.Fatalf("dial-failed lines %v; an answer during the opening dial's flight, egress up all along, must blame the peer", failed)
	}
	if !pool.breaker.foundDown() || !pool.delegations.active(wsHoldKey(), time.Now()) {
		t.Fatal("want the box found down with the hold kept until its chat is placed (PATCHES §67)")
	}
}

// The owner's decision through the forward path (PATCHES §54): an opening
// kept the holds, this machine came back, and the first held forward after the
// cooldown meets a cold link. The breaker re-opens and the turn is served
// locally; every hold stays, in memory and on disk, and the lines say so. A
// second failed forward sent since recovery is the box down: every hold ends.
func TestAColdLinkAfterRecoveryKeepsEveryHold(t *testing.T) {
	server, first := heldSessionServer(t, "http://100.64.0.9:31416")
	pool := server.RemotePool
	second := session.ScopedSessionKey("claude", "s2")
	pool.delegations.set(second, time.Now(), remotePoolDelegationTTLFor("claude"), "peer-acct", "box")
	pool.client.Transport = proxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errPeerTimeout
	})
	logs := &syncBuffer{}
	server.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	path := filepath.Join(t.TempDir(), "peer-holds.json")
	forward := func() map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
		server.forwardToPeer(pool, req, "claude", "s1", "/v1/messages", "", []byte(`{}`), remotePoolHoldSet, "peer-acct")
		failed := e2eLogLines(logs, "remote pool forward failed")
		return failed[len(failed)-1]
	}
	for i := 0; i < remotePoolFailuresToOpen; i++ {
		forward() // nothing local has answered: the opening keeps the holds
	}
	if kept := e2eLogLines(logs, holdsKeptMessage); len(kept) != 1 || kept[0]["why"] != keptNoAnswer {
		t.Fatalf("calibration: holds-kept lines %v, want one for the offline opening, why=no_answer", kept)
	}

	localUpstreamAnswered(pool) // this machine is back
	e2eRewindCooldown(pool)
	cold := forward()
	if !e2eLineSays(cold, "breaker_opened", true) || !e2eLineSays(cold, "peer_down", false) {
		t.Fatalf("forward-failed line %v; one failed forward since this machine came back must re-open the breaker with peer_down=false", cold)
	}
	if kept := e2eLogLines(logs, holdsKeptMessage); len(kept) != 2 || kept[1]["held"] != float64(2) || kept[1]["why"] != keptOneStrikeSinceBack {
		t.Fatalf("holds-kept lines %v, want a second one naming both holds for the re-opening, why=one_strike_since_back", kept)
	}
	for _, key := range []string{first, second} {
		if !pool.delegations.active(key, time.Now()) {
			t.Fatalf("%s lost its hold to one failed forward since this machine came back", key)
		}
	}
	pool.delegations.flush(path, time.Now())
	if onDisk := heldOnDisk(t, path); len(onDisk) != 2 {
		t.Fatalf("peer-holds.json lists %v after the re-opening, want both holds", onDisk)
	}

	e2eRewindCooldown(pool)
	if down := forward(); !e2eLineSays(down, "breaker_opened", true) || !e2eLineSays(down, "peer_down", true) {
		t.Fatalf("forward-failed line %v; the second failed forward since recovery must find the box down", down)
	}
	if live := pool.delegations.live(time.Now()); len(live) != 2 || !pool.breaker.foundDown() {
		t.Fatalf("holds %v after the box was found down; want both kept until their chats are placed (PATCHES §67)", live)
	}
}

// The websocket twin: the same breaker, through the Codex dial.
func TestAWebSocketColdLinkAfterRecoveryKeepsEveryHold(t *testing.T) {
	server := heldCodexWebSocketServer(t, "http://100.64.0.9:31416")
	pool := server.RemotePool
	other := session.ScopedSessionKey("codex", "another-warm-session")
	pool.delegations.set(other, time.Now(), remotePoolDelegationTTLFor("codex"), "peer-acct", pool.name())
	pool.wsDialer.Proxy = nil
	pool.wsDialer.NetDialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, errPeerTimeout
	}
	logs := &syncBuffer{}
	server.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	path := filepath.Join(t.TempDir(), "peer-holds.json")
	dial := func() map[string]any {
		t.Helper()
		forwardHeldWebSocket(t, server, context.Background())
		failed := e2eLogLines(logs, "remote pool websocket dial failed")
		return failed[len(failed)-1]
	}
	for i := 0; i < remotePoolFailuresToOpen; i++ {
		dial()
	}
	if kept := e2eLogLines(logs, holdsKeptMessage); len(kept) != 1 || kept[0]["why"] != keptNoAnswer {
		t.Fatalf("calibration: holds-kept lines %v, want one for the offline opening, why=no_answer", kept)
	}

	localUpstreamAnswered(pool)
	e2eRewindCooldown(pool)
	cold := dial()
	if !e2eLineSays(cold, "breaker_opened", true) || !e2eLineSays(cold, "peer_down", false) {
		t.Fatalf("dial-failed line %v; one failed dial since this machine came back must re-open the breaker with peer_down=false", cold)
	}
	if kept := e2eLogLines(logs, holdsKeptMessage); len(kept) != 2 || kept[1]["held"] != float64(2) || kept[1]["why"] != keptOneStrikeSinceBack {
		t.Fatalf("holds-kept lines %v, want a second one naming both holds for the re-opening, why=one_strike_since_back", kept)
	}
	for _, key := range []string{wsHoldKey(), other} {
		if !pool.delegations.active(key, time.Now()) {
			t.Fatalf("%s lost its hold to one failed dial since this machine came back", key)
		}
	}
	pool.delegations.flush(path, time.Now())
	if onDisk := heldOnDisk(t, path); len(onDisk) != 2 {
		t.Fatalf("peer-holds.json lists %v after the re-opening, want both holds", onDisk)
	}

	e2eRewindCooldown(pool)
	if down := dial(); !e2eLineSays(down, "breaker_opened", true) || !e2eLineSays(down, "peer_down", true) {
		t.Fatalf("dial-failed line %v; the second failed dial since recovery must find the box down", down)
	}
	if live := pool.delegations.live(time.Now()); len(live) != 2 || !pool.breaker.foundDown() {
		t.Fatalf("holds %v after the box was found down; want both kept until their chats are placed (PATCHES §67)", live)
	}
}

// T7: a peer that is really down, after an opening that kept the holds (the
// owner's decision, PATCHES §54). The next failure re-opens the breaker at
// once, so turns stay local for a cooldown and the cost bound is unchanged.
// But only a SECOND counted strike sent after this machine came back ends the
// holds: one failure right after recovery is what a cold tailnet link or a
// dead pooled keep-alive looks like, and it keeps them. Without the answer
// every re-opening keeps them.
func TestUnattributedOpenNeedsTwoStrikesSentSinceRecovery(t *testing.T) {
	for _, answered := range []bool{true, false} {
		var b remotePoolBreaker
		var egress localEgress
		strikeAt(&b, &egress, at(0), errPeerTimeout)
		if opened, peerDown := strikeAt(&b, &egress, at(3*time.Second), errPeerTimeout); !opened || peerDown {
			t.Fatalf("calibration: opened=%v peerDown=%v, want an unattributed opening", opened, peerDown)
		}
		if answered {
			egress.noteAnswerOnANewConnection(at(20 * time.Second))
		}
		first := at(3*time.Second + remotePoolDialTimeout + remotePoolCooldown + time.Second)
		if !b.allow(first) {
			t.Fatal("calibration: the breaker did not reclose after its cooldown")
		}
		wantWhy := keptNoAnswer
		if answered {
			wantWhy = keptOneStrikeSinceBack
		}
		opened, peerDown, why := strikeWhyAt(&b, &egress, first, errPeerTimeout)
		if !opened {
			t.Fatalf("answered=%v: the first failure after an unattributed opening did not re-open the breaker", answered)
		}
		if peerDown || why != wantWhy {
			t.Fatalf("answered=%v: opened=%v peerDown=%v why=%q; one failure since this machine came back ended every hold (want why=%s)", answered, opened, peerDown, why, wantWhy)
		}
		if b.failures != remotePoolFailuresToOpen-1 {
			t.Fatalf("answered=%v: failures after the re-opening = %d, want %d: the next failure must re-open at once", answered, b.failures, remotePoolFailuresToOpen-1)
		}
		// A request sent with it, before its failure was seen, is the same
		// blip: it counts once (§21), so it is not the second strike.
		if opened, peerDown, _ := b.failure(first.Add(time.Second), first.Add(3*time.Second), errPeerTimeout, &egress, 0); opened || peerDown {
			t.Fatalf("answered=%v: opened=%v peerDown=%v from a request in flight with the first strike since recovery: one blip counts once", answered, opened, peerDown)
		}
		second := first.Add(remotePoolDialTimeout + remotePoolCooldown + time.Second)
		if !b.allow(second) {
			t.Fatal("calibration: the breaker did not reclose after the re-opening's cooldown")
		}
		opened, peerDown = strikeAt(&b, &egress, second, errPeerTimeout)
		if !opened || peerDown != answered {
			t.Fatalf("answered=%v: opened=%v peerDown=%v on the second strike since recovery, want opened, peerDown=%v", answered, opened, peerDown, answered)
		}
	}
}

// A strike sent before this machine came back and failing after it is not
// one of the two: it left with no network (T5's race guard, one level up).
// Only the two sent after the answer end the holds.
func TestAStrikeSentBeforeRecoveryDoesNotCountTowardTheTwo(t *testing.T) {
	var b remotePoolBreaker
	var egress localEgress
	strikeAt(&b, &egress, at(0), errPeerTimeout)
	if opened, peerDown := strikeAt(&b, &egress, at(3*time.Second), errPeerTimeout); !opened || peerDown {
		t.Fatalf("calibration: opened=%v peerDown=%v, want an unattributed opening", opened, peerDown)
	}
	straddling := at(3*time.Second + remotePoolDialTimeout + remotePoolCooldown + time.Second)
	egress.noteAnswerOnANewConnection(straddling.Add(time.Second)) // the network comes back while it is in flight
	if opened, peerDown, why := strikeWhyAt(&b, &egress, straddling, errPeerTimeout); !opened || peerDown || why != keptNetworkBackMidStrike {
		t.Fatalf("calibration: opened=%v peerDown=%v why=%q, want the straddling strike to re-open and keep the holds, why=network_back_mid_strike", opened, peerDown, why)
	}
	// A request sent after the answer but before the straddling strike's
	// failure was seen is that same blip: not counted, so not a strike since
	// recovery either.
	if opened, _, _ := b.failure(straddling.Add(1500*time.Millisecond), straddling.Add(3500*time.Millisecond), errPeerTimeout, &egress, 0); opened {
		t.Fatal("calibration: a request in flight with the straddling strike was counted")
	}
	first := straddling.Add(remotePoolDialTimeout + remotePoolCooldown + time.Second)
	if opened, peerDown, why := strikeWhyAt(&b, &egress, first, errPeerTimeout); !opened || peerDown || why != keptOneStrikeSinceBack {
		t.Fatalf("opened=%v peerDown=%v why=%q; the first strike sent since recovery ended every hold, counting one that left before it", opened, peerDown, why)
	}
	second := first.Add(remotePoolDialTimeout + remotePoolCooldown + time.Second)
	if opened, peerDown := strikeAt(&b, &egress, second, errPeerTimeout); !opened || !peerDown {
		t.Fatalf("opened=%v peerDown=%v; two strikes sent since recovery must find the box down", opened, peerDown)
	}
}

// A no-route opening: the tailnet route is gone while local turns answer.
// Those strikes were this machine's own: each notes a local failure (since
// review fix round 2 every counted one does, opening or not), so this machine
// came back at the next local answer, after the opening strike. Once the
// route is back the first forward that meets a cold link is the first strike
// since then, and keeps the holds; the second ends them.
func TestANoRouteStrikeDoesNotCountTowardTheTwo(t *testing.T) {
	var b remotePoolBreaker
	var egress localEgress
	unreachable := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ENETUNREACH)}
	egress.noteAnswerOnANewConnection(at(-time.Minute))
	strikeAt(&b, &egress, at(0), unreachable)
	egress.noteAnswerOnANewConnection(at(time.Second))
	if opened, peerDown := strikeAt(&b, &egress, at(3*time.Second), unreachable); !opened || peerDown {
		t.Fatalf("calibration: opened=%v peerDown=%v, want a no-route opening that keeps the holds", opened, peerDown)
	}
	egress.noteAnswerOnANewConnection(at(10 * time.Second))
	first := at(3*time.Second + remotePoolDialTimeout + remotePoolCooldown + time.Second)
	if opened, peerDown, why := strikeWhyAt(&b, &egress, first, errPeerTimeout); !opened || peerDown || why != keptOneStrikeSinceBack {
		t.Fatalf("opened=%v peerDown=%v why=%q; one timeout once the route was back ended every hold, counting a no-route strike", opened, peerDown, why)
	}
	second := first.Add(remotePoolDialTimeout + remotePoolCooldown + time.Second)
	if opened, peerDown := strikeAt(&b, &egress, second, errPeerTimeout); !opened || !peerDown {
		t.Fatalf("opened=%v peerDown=%v; two timeouts once the route was back must find the box down", opened, peerDown)
	}
}

// The kept series' residual, as far as two strikes since recovery close it.
// An opening kept the holds, this machine came back and kept answering while
// the box's sessions sat idle, and then its own network drops with nothing
// local noticing. The first held forward after the drop is judged against the
// old series, so the answer from before the drop reads as "during the
// outage" -- but it is only one strike since recovery, so it keeps the holds.
// Its turn's local fallback then fails at the transport, which resets upSince,
// so the next strike cannot be attributed either.
func TestASecondDropAfterRecoveryKeepsTheHoldsWhenTheFallbackFails(t *testing.T) {
	var b remotePoolBreaker
	var egress localEgress
	strikeAt(&b, &egress, at(0), errPeerTimeout)
	if opened, peerDown := strikeAt(&b, &egress, at(3*time.Second), errPeerTimeout); !opened || peerDown {
		t.Fatalf("calibration: opened=%v peerDown=%v, want an unattributed opening", opened, peerDown)
	}
	egress.noteAnswerOnANewConnection(at(time.Minute))      // back
	egress.noteAnswerOnANewConnection(at(20 * time.Minute)) // the last local answer before the drop
	dropped := at(20*time.Minute + 30*time.Second)
	if opened, peerDown, why := strikeWhyAt(&b, &egress, dropped, errPeerTimeout); !opened || peerDown || why != keptOneStrikeSinceBack {
		t.Fatalf("opened=%v peerDown=%v why=%q; the first forward after a second drop ended every hold on an answer from before it", opened, peerDown, why)
	}
	egress.noteFailure(dropped.Add(remotePoolDialTimeout + time.Second)) // its turn's local fallback fails
	next := dropped.Add(remotePoolDialTimeout + remotePoolCooldown + time.Second)
	if opened, peerDown, why := strikeWhyAt(&b, &egress, next, errPeerTimeout); !opened || peerDown || why != keptNoAnswer {
		t.Fatalf("opened=%v peerDown=%v why=%q; still offline after the fallback failed, the holds must stay (why=no_answer)", opened, peerDown, why)
	}
}

// A drop nothing local saw fail (review fix round 1). The egress has been up
// since long before it, with no local transport failure since, so upSince is
// old. The opening kept the holds because nothing local answered during the
// outage, so this machine may have been offline when that opening's strike
// left: it is not a strike sent since this machine came back. The first local
// answer after it is the recovery, so one cold-link failure after that keeps
// every hold, and the second ends them.
func TestAQuietDropsOpeningIsNotAStrikeSinceRecovery(t *testing.T) {
	var b remotePoolBreaker
	var egress localEgress
	egress.noteAnswerOnANewConnection(at(-10 * time.Minute)) // up since then: nothing local has failed
	egress.noteAnswerOnANewConnection(at(-5 * time.Minute))  // the last local answer before the drop
	strikeAt(&b, &egress, at(0), errPeerTimeout)
	if opened, peerDown, why := strikeWhyAt(&b, &egress, at(10*time.Second), errPeerTimeout); !opened || peerDown || why != keptNoAnswer {
		t.Fatalf("calibration: opened=%v peerDown=%v why=%q, want the drop's opening to keep the holds, why=no_answer", opened, peerDown, why)
	}
	egress.noteAnswerOnANewConnection(at(70 * time.Second)) // the network is back
	first := at(80 * time.Second)
	if opened, peerDown, why := strikeWhyAt(&b, &egress, first, errPeerTimeout); !opened || peerDown || why != keptOneStrikeSinceBack {
		t.Fatalf("opened=%v peerDown=%v why=%q; one cold-link failure after the network came back ended every hold, counting the drop's own strike as sent since recovery", opened, peerDown, why)
	}
	second := first.Add(remotePoolDialTimeout + remotePoolCooldown + time.Second)
	if opened, peerDown := strikeAt(&b, &egress, second, errPeerTimeout); !opened || !peerDown {
		t.Fatalf("opened=%v peerDown=%v; two strikes sent since the network came back must find the box down", opened, peerDown)
	}
}

// The same on a machine whose observed local traffic is rare (its turns go the
// unobserved ways): one answer an hour ago and no observed failure since. It
// reads a dead box as itself offline until something observed answers. A
// strike sent before that answer and failing after it is not one of the two
// (the race guard); the two sent after it find the box down.
func TestAMachineThatSawNoFailureNeedsTwoStrikesAfterItsNextAnswer(t *testing.T) {
	var b remotePoolBreaker
	var egress localEgress
	egress.noteAnswerOnANewConnection(at(-time.Hour))
	strikeAt(&b, &egress, at(0), errPeerTimeout)
	if opened, peerDown, why := strikeWhyAt(&b, &egress, at(3*time.Second), errPeerTimeout); !opened || peerDown || why != keptNoAnswer {
		t.Fatalf("calibration: opened=%v peerDown=%v why=%q, want the opening to keep the holds, why=no_answer", opened, peerDown, why)
	}
	still := at(3*time.Second + remotePoolDialTimeout + remotePoolCooldown + time.Second)
	if opened, peerDown, why := strikeWhyAt(&b, &egress, still, errPeerTimeout); !opened || peerDown || why != keptNoAnswer {
		t.Fatalf("calibration: opened=%v peerDown=%v why=%q; with nothing observed answering, want the re-opening to keep the holds, why=no_answer", opened, peerDown, why)
	}
	straddling := still.Add(remotePoolDialTimeout + remotePoolCooldown + time.Second)
	egress.noteAnswerOnANewConnection(straddling.Add(time.Second)) // something observed answers while it is in flight
	if opened, peerDown, why := strikeWhyAt(&b, &egress, straddling, errPeerTimeout); !opened || peerDown || why != keptNetworkBackMidStrike {
		t.Fatalf("opened=%v peerDown=%v why=%q; a strike sent before the first observed answer since the outage counted as sent since recovery (want why=network_back_mid_strike)", opened, peerDown, why)
	}
	first := straddling.Add(remotePoolDialTimeout + remotePoolCooldown + time.Second)
	if opened, peerDown, why := strikeWhyAt(&b, &egress, first, errPeerTimeout); !opened || peerDown || why != keptOneStrikeSinceBack {
		t.Fatalf("opened=%v peerDown=%v why=%q; the first strike sent after the observed answer ended every hold", opened, peerDown, why)
	}
	second := first.Add(remotePoolDialTimeout + remotePoolCooldown + time.Second)
	if opened, peerDown := strikeAt(&b, &egress, second, errPeerTimeout); !opened || !peerDown {
		t.Fatalf("opened=%v peerDown=%v; two strikes sent after the observed answer must find the box down", opened, peerDown)
	}
}

// An outage that left only ONE counted strike (review fix round 2): a reboot or
// a wake where a single held turn tried the box while this machine was offline.
// Its network then comes back, a local turn answers, and within the cooldown
// the next held forward meets a cold link. That second strike opens the
// breaker, but the strike before it was sent before this machine came back,
// so it is one strike since recovery and keeps every hold. A machine is "seen
// offline" by an observed local transport failure (before the strike or after
// it) or by the strike's own no-route error. The control is a process that
// has seen no local failure since it started: it finds a dead box at the
// second strike, as before §54.
func TestAnOutageThatLeftOneStrikeKeepsTheHoldsAtTheFirstColdLinkAfterRecovery(t *testing.T) {
	unreachable := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ENETUNREACH)}
	for _, tc := range []struct {
		name    string
		offline func(*localEgress) // this machine's own evidence before the strike
		strike  error
		after   func(*localEgress) // and after it, before the network comes back
		kept    bool
	}{
		{"timeout, its local fallback failed", func(e *localEgress) { e.noteAnswerOnANewConnection(at(-time.Hour)) },
			errPeerTimeout, func(e *localEgress) { e.noteFailure(at(remotePoolDialTimeout + time.Second)) }, true},
		{"no route, nothing local observed", func(e *localEgress) { e.noteAnswerOnANewConnection(at(-time.Hour)) },
			unreachable, func(*localEgress) {}, true},
		{"timeout after a local failure, its fallback unobserved",
			func(e *localEgress) {
				e.noteAnswerOnANewConnection(at(-time.Hour))
				e.noteFailure(at(-5 * time.Second))
			},
			errPeerTimeout, func(*localEgress) {}, true},
		// Calibration: nothing local has failed since the process started.
		{"no local failure since the start", func(*localEgress) {}, errPeerTimeout, func(*localEgress) {}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b remotePoolBreaker
			var egress localEgress
			tc.offline(&egress)
			if opened, _ := strikeAt(&b, &egress, at(0), tc.strike); opened {
				t.Fatal("calibration: one failure opened the breaker")
			}
			tc.after(&egress)
			egress.noteAnswerOnANewConnection(at(10 * time.Second)) // the network is back
			cold := at(15 * time.Second)                            // inside the cooldown of the first strike: one series
			opened, peerDown, why := strikeWhyAt(&b, &egress, cold, errPeerTimeout)
			if !opened {
				t.Fatal("calibration: the second strike did not open the breaker")
			}
			if !tc.kept {
				if !peerDown || why != "" {
					t.Fatalf("opened=%v peerDown=%v why=%q; a process that saw no local failure must find a dead box at the second strike", opened, peerDown, why)
				}
				return
			}
			if peerDown || why != keptOneStrikeSinceBack {
				t.Fatalf("opened=%v peerDown=%v why=%q; one cold-link failure after the network came back ended every hold, counting a strike sent while offline (want why=one_strike_since_back)", opened, peerDown, why)
			}
			if b.failures != remotePoolFailuresToOpen-1 {
				t.Fatalf("failures after the opening = %d, want %d: the next failure must re-open at once", b.failures, remotePoolFailuresToOpen-1)
			}
			second := cold.Add(remotePoolDialTimeout + remotePoolCooldown + time.Second)
			if opened, peerDown := strikeAt(&b, &egress, second, errPeerTimeout); !opened || !peerDown {
				t.Fatalf("opened=%v peerDown=%v; two strikes sent since the network came back must find the box down", opened, peerDown)
			}
		})
	}
}

// The series restart measures its gap on the wall clock (review fix round 2).
// darwin's monotonic clock stops while the machine sleeps, so a strike seen
// just before the lid closed read as seconds before the first forward after
// waking: the series kept its pre-sleep start, the first local answer after
// waking read as "during the outage", and one cold-link failure ended every
// hold. Here nothing local fails, so only the series start decides.
func TestAStrikeFromBeforeASleepDoesNotDateTheOutageAfterIt(t *testing.T) {
	var b remotePoolBreaker
	var egress localEgress
	before := time.Now() // a reading with a monotonic clock, as forwardToPeer's are
	egress.noteAnswerOnANewConnection(before.Add(-time.Hour))
	strikeAt(&b, &egress, before, errPeerTimeout)                  // a blip on the box's path
	egress.noteAnswerOnANewConnection(before.Add(3 * time.Second)) // its turn, served locally
	woke := sleptThrough(t, before.Add(5*time.Second), 40*time.Minute)
	egress.noteAnswerOnANewConnection(woke.Add(10 * time.Second)) // the first local answer after waking
	sent := woke.Add(13 * time.Second)
	if lastSeen := before.Add(remotePoolDialTimeout); sent.Sub(lastSeen) > remotePoolCooldown || sent.Round(0).Sub(lastSeen.Round(0)) <= remotePoolCooldown {
		t.Fatalf("calibration: gap %v monotonic, %v wall; want inside the cooldown on the first and past it on the second",
			sent.Sub(lastSeen), sent.Round(0).Sub(lastSeen.Round(0)))
	}
	opened, peerDown, why := strikeWhyAt(&b, &egress, sent, errPeerTimeout)
	if !opened {
		t.Fatal("calibration: the leftover strike and the new one did not open the breaker (the count stands)")
	}
	if peerDown || why != keptNoAnswer {
		t.Fatalf("opened=%v peerDown=%v why=%q; a strike from before a 40-minute sleep dated the outage after it, so the first answer after waking read as during the outage", opened, peerDown, why)
	}
	if !b.seriesStarted.Equal(sent) {
		t.Fatalf("seriesStarted = %v, want the strike after waking (%v)", b.seriesStarted, sent)
	}
}

// held= on the holds-kept line counts pending pre-claims. On the HTTP forward
// that includes the tripping forward's own: its session is not held yet, so it
// claims before the request leaves and releases the claim only after the line.
// One hold already on the box, so held=2.
func TestTheHoldsKeptLineCountsTheTrippingForwardsOwnClaim(t *testing.T) {
	server := Server{RemotePool: NewRemotePool("http://100.64.0.9:31416", "", "")}
	pool := server.RemotePool
	held := session.ScopedSessionKey("claude", "held")
	pool.delegations.set(held, time.Now(), remotePoolDelegationTTLFor("claude"), "peer-acct", pool.name())
	tripping := session.ScopedSessionKey("claude", "new")
	var claimedInFlight atomic.Int32
	pool.client.Transport = proxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		if pool.delegations.active(tripping, time.Now()) {
			claimedInFlight.Add(1)
		}
		return nil, errPeerTimeout
	})
	logs := &syncBuffer{}
	server.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	for i := 0; i < remotePoolFailuresToOpen; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
		server.forwardToPeer(pool, req, "claude", "new", "/v1/messages", "", []byte(`{}`), remotePoolHoldSet, "peer-acct")
	}
	if claimedInFlight.Load() != remotePoolFailuresToOpen {
		t.Fatalf("calibration: the session was pre-claimed on %d of %d forwards, want every one", claimedInFlight.Load(), remotePoolFailuresToOpen)
	}
	if kept := e2eLogLines(logs, holdsKeptMessage); len(kept) != 1 || kept[0]["held"] != float64(2) {
		t.Fatalf("holds-kept lines %v; want one with held=2: the hold on the box and the tripping forward's own pre-claim", kept)
	}
	if live := pool.delegations.live(time.Now()); len(live) != 1 || !pool.delegations.active(held, time.Now()) {
		t.Fatalf("holds after the opening = %v, want only %s: the tripping forward's claim is released after the line", live, held)
	}
}

// The websocket twin: the dial releases its own pre-claim before it logs, so
// with one hold already on the box the line says held=1.
func TestTheWebSocketHoldsKeptLineLeavesOutTheDialsOwnClaim(t *testing.T) {
	server := Server{RemotePool: NewRemotePool("http://100.64.0.9:31416", "", "")}
	pool := server.RemotePool
	held := session.ScopedSessionKey("codex", "another-warm-session")
	pool.delegations.set(held, time.Now(), remotePoolDelegationTTLFor("codex"), "peer-acct", pool.name())
	var claimedInFlight atomic.Int32
	pool.wsDialer.Proxy = nil
	pool.wsDialer.NetDialContext = func(context.Context, string, string) (net.Conn, error) {
		if pool.delegations.active(wsHoldKey(), time.Now()) {
			claimedInFlight.Add(1)
		}
		return nil, errPeerTimeout
	}
	logs := &syncBuffer{}
	server.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	for i := 0; i < remotePoolFailuresToOpen; i++ {
		forwardHeldWebSocket(t, server, context.Background()) // wsHoldSession is not held: each dial claims it
	}
	if claimedInFlight.Load() != remotePoolFailuresToOpen {
		t.Fatalf("calibration: the session was pre-claimed on %d of %d dials, want every one", claimedInFlight.Load(), remotePoolFailuresToOpen)
	}
	if kept := e2eLogLines(logs, holdsKeptMessage); len(kept) != 1 || kept[0]["held"] != float64(1) {
		t.Fatalf("holds-kept lines %v; want one with held=1: the dial's own pre-claim is released before the line", kept)
	}
	if live := pool.delegations.live(time.Now()); len(live) != 1 || !pool.delegations.active(held, time.Now()) {
		t.Fatalf("holds after the opening = %v, want only %s", live, held)
	}
}

// T7, still offline: an idle laptop whose last answer predates the outage. The
// unattributed opening keeps the series start, so the strike after the
// cooldown is judged against the same outage and keeps the holds again. With
// the start dropped, that pre-outage answer would read as "answered during
// the outage" and the re-opening would evict every hold while still offline.
func TestAnUnattributedOpeningKeepsItsSeriesStart(t *testing.T) {
	var b remotePoolBreaker
	var egress localEgress
	egress.noteAnswerOnANewConnection(at(-10 * time.Second))
	strikeAt(&b, &egress, at(0), errPeerTimeout)
	if opened, peerDown := strikeAt(&b, &egress, at(3*time.Second), errPeerTimeout); !opened || peerDown {
		t.Fatalf("calibration: opened=%v peerDown=%v, want an unattributed opening", opened, peerDown)
	}
	if !b.seriesStarted.Equal(at(0)) {
		t.Fatalf("seriesStarted = %v after an unattributed opening, want the first strike's %v", b.seriesStarted, at(0))
	}
	afterCooldown := at(3*time.Second + remotePoolDialTimeout + remotePoolCooldown + time.Second)
	opened, peerDown, why := strikeWhyAt(&b, &egress, afterCooldown, errPeerTimeout)
	if !opened || peerDown || why != keptNoAnswer {
		t.Fatalf("opened=%v peerDown=%v why=%q; still offline with only a pre-outage answer, the re-opening must keep the holds (why=no_answer)", opened, peerDown, why)
	}
}

// T8: the peer answering clears an unattributed opening entirely: a later lone
// blip is a blip again, however much local traffic answers.
func TestPeerSuccessClearsAnUnattributedOpen(t *testing.T) {
	var b remotePoolBreaker
	var egress localEgress
	strikeAt(&b, &egress, at(0), errPeerTimeout)
	strikeAt(&b, &egress, at(3*time.Second), errPeerTimeout)
	b.success(at(10 * time.Second))
	egress.noteAnswerOnANewConnection(at(40 * time.Second))
	if opened, _ := strikeAt(&b, &egress, at(50*time.Second), errPeerTimeout); opened {
		t.Fatal("one failure after the peer answered opened the breaker: the unattributed opening outlived the peer's answer")
	}
	if !b.seriesStarted.Equal(at(50 * time.Second)) {
		t.Fatalf("seriesStarted = %v; the success must end the old series, and this failure start the next", b.seriesStarted)
	}
	// Nor does the success leave the series kept: a strike left over from that
	// blip does not date a later drop of this machine's own network (T3).
	egress.noteAnswerOnANewConnection(at(time.Hour - 10*time.Second))
	if opened, peerDown := strikeAt(&b, &egress, at(time.Hour), errPeerTimeout); !opened || peerDown {
		t.Fatalf("opened=%v peerDown=%v; a strike left over since the peer's answer dated a later network drop", opened, peerDown)
	}
}

// T11: every local upstream round trip reaches every peer's evidence. Any
// answer counts (a 529 is the provider answering), a transport failure is a
// failure, a client cancel is nothing, and a machine with no peer gets no
// wrapper at all.
func TestLocalUpstreamAnswersAndFailuresReachEveryPeer(t *testing.T) {
	// The 529 is held until the test has read the clock, so its answer is
	// noted when its headers arrive, not when its request left.
	entered, release := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/overloaded":
			entered <- struct{}{}
			<-release
			w.WriteHeader(529)
		case "/slow":
			// Answers only once its client has gone (Esc mid-turn).
			entered <- struct{}{}
			<-r.Context().Done()
		}
	}))
	t.Cleanup(upstream.Close)
	refused := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	refusedURL := refused.URL
	refused.Close()

	a := NewRemotePool("http://box-a.example:31416", "", "")
	b := NewRemotePool("http://box-b.example:31416", "", "")
	server := Server{RemotePeers: []*RemotePool{a, b}}
	roundTrip := func(ctx context.Context, url string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp, err := server.transport().RoundTrip(req); err == nil {
			resp.Body.Close()
		}
	}
	both := func(check func(egressView) bool) bool { return check(a.egress.view()) && check(b.egress.view()) }

	before := time.Now()
	roundTrip(context.Background(), upstream.URL+"/ok")
	if !both(func(v egressView) bool { return !v.upSince.Before(before) && !v.lastAnswer.Before(before) }) {
		t.Fatalf("a 200 was not noted as an answer on both peers: a=%+v b=%+v", a.egress.view(), b.egress.view())
	}

	roundTrip(context.Background(), refusedURL)
	if !both(func(v egressView) bool { return v.upSince.IsZero() }) {
		t.Fatalf("a refused local dial was not noted as a failure on both peers: a=%+v b=%+v", a.egress.view(), b.egress.view())
	}

	overloaded, err := http.NewRequest(http.MethodGet, upstream.URL+"/overloaded", nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := server.transport().RoundTrip(overloaded); err == nil {
			resp.Body.Close()
		}
	}()
	<-entered
	headersBack := time.Now() // the request is at the upstream; its headers are not back yet
	close(release)
	<-done
	if !both(func(v egressView) bool { return !v.upSince.Before(headersBack) && !v.lastAnswer.Before(headersBack) }) {
		t.Fatalf("a 529 was not noted as an answer, when its headers came back (%v), on both peers: a=%+v b=%+v", headersBack, a.egress.view(), b.egress.view())
	}

	upSince, failureAt := a.egress.upSince.Load(), a.egress.failureAt.Load()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	roundTrip(cancelled, refusedURL)
	if a.egress.upSince.Load() != upSince || a.egress.failureAt.Load() != failureAt {
		t.Fatal("a client cancel was noted as evidence about this machine's network")
	}

	// The real Esc: the client goes while the request is at the upstream, so
	// whether it was cancelled is known only once the round trip returns
	// (review round 2).
	midFlight, cancelMidFlight := context.WithCancel(context.Background())
	slow, err := http.NewRequestWithContext(midFlight, http.MethodGet, upstream.URL+"/slow", nil)
	if err != nil {
		t.Fatal(err)
	}
	done = make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := server.transport().RoundTrip(slow); err == nil {
			resp.Body.Close()
		}
	}()
	<-entered
	cancelMidFlight()
	<-done
	if a.egress.upSince.Load() != upSince || a.egress.failureAt.Load() != failureAt {
		t.Fatal("a client cancel mid-flight was noted as evidence about this machine's network")
	}

	alone := Server{Transport: http.DefaultTransport}
	if _, wrapped := alone.transport().(localEgressObserver); wrapped {
		t.Fatal("a machine with no peer wrapped its transport")
	}
	if _, wrapped := server.transport().(localEgressObserver); !wrapped {
		t.Fatal("calibration: a machine with peers did not wrap its transport")
	}
}

// T11, the websocket half: Codex opens every model turn as a websocket, and
// that dial does not go through Server.transport(). A handshake that gets an
// answer -- a 101, or a refusal with a status -- is an answer and a new
// connection on every peer, and a dial that cannot connect is a failure. Unobserved, a machine whose
// local turns are Codex websockets never shows its egress up, and a really
// dead peer keeps its holds until the idle bound (PATCHES §54).
func TestALocalWebSocketDialIsEvidenceForEveryPeer(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upgrading := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if conn, err := upgrader.Upgrade(w, r, nil); err == nil {
			conn.Close()
		}
	}))
	t.Cleanup(upgrading.Close)
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "challenge", http.StatusForbidden)
	}))
	t.Cleanup(refusing.Close)
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close()

	a := NewRemotePool("http://box-a.example:31416", "", "")
	b := NewRemotePool("http://box-b.example:31416", "", "")
	server := Server{RemotePeers: []*RemotePool{a, b}}
	account := accounts.Account{ID: "local@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "tok"}
	dial := func(upstreamURL string) {
		t.Helper()
		// The recorder cannot be hijacked, so the client side of the upgrade
		// fails after the upstream dial: only the dial is under test.
		server.proxyWebSocket(httptest.NewRecorder(), webSocketUpgradeRequest(context.Background()), account, nil,
			"codex", "s1", "", "", "", mustURL(t, upstreamURL), nil, false, "", "")
	}
	both := func(check func(egressView) bool) bool { return check(a.egress.view()) && check(b.egress.view()) }

	before := time.Now()
	dial(upgrading.URL)
	if !both(func(v egressView) bool {
		return !v.upSince.Before(before) && !v.lastAnswer.Before(before) && !v.lastConnected.Before(before)
	}) {
		t.Fatalf("a local websocket upgrade was not noted as an answer on a new connection on both peers: a=%+v b=%+v", a.egress.view(), b.egress.view())
	}

	dial(closedURL)
	if !both(func(v egressView) bool { return v.upSince.IsZero() }) {
		t.Fatalf("a refused local websocket dial was not noted as a failure on both peers: a=%+v b=%+v", a.egress.view(), b.egress.view())
	}

	afterFailure := time.Now()
	dial(refusing.URL)
	if !both(func(v egressView) bool { return !v.upSince.Before(afterFailure) }) {
		t.Fatalf("a websocket handshake refused with a status was not noted as an answer on both peers: a=%+v b=%+v", a.egress.view(), b.egress.view())
	}
}
