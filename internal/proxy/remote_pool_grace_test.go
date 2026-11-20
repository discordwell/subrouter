package proxy

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/session"
)

// PATCHES §67: a peer is DOWN only after it has answered nothing, on any
// connection, for the grace while this machine was online. These pin the
// grace on the breaker itself, on the fixed clock of
// remote_pool_attribution_test.go, and its log lines on the forward paths.

const testPeerDownGrace = 15 * time.Minute

type graceStrike struct {
	failedAt         time.Time
	opened, peerDown bool
	why              string
}

// strikeEvery sends a counted strike every step from first through last, each
// failing a dial timeout after it left, with a local turn answering on a new
// connection just before each: a laptop that can open connections and whose
// own upstream keeps answering, and a box that answers nothing. It stops at
// the first peer_down.
func strikeEvery(b *remotePoolBreaker, egress *localEgress, first, last time.Time, step, grace time.Duration, err error) []graceStrike {
	var out []graceStrike
	for started := first; !started.After(last); started = started.Add(step) {
		egress.noteAnswerOnANewConnection(started.Add(-time.Second))
		failedAt := started.Add(remotePoolDialTimeout)
		opened, peerDown, why := b.failure(started, failedAt, err, egress, grace)
		out = append(out, graceStrike{failedAt: failedAt, opened: opened, peerDown: peerDown, why: why})
		if peerDown {
			break
		}
	}
	return out
}

// peerDownAt is when the strike that found the peer down failed, and zero
// when none did. Every opening before it must have kept the holds for the
// grace or for a §54 reason in allowed.
func peerDownAt(t *testing.T, strikes []graceStrike, allowed ...string) time.Time {
	t.Helper()
	for _, s := range strikes {
		if s.peerDown {
			return s.failedAt
		}
		if !s.opened {
			continue
		}
		ok := s.why == keptWithinGrace
		for _, why := range allowed {
			ok = ok || s.why == why
		}
		if !ok {
			t.Fatalf("an opening at %v kept the holds why=%q, want %s", s.failedAt.Sub(attributionClock), s.why, keptWithinGrace)
		}
	}
	return time.Time{}
}

func sinceClock(at time.Time) time.Duration {
	if at.IsZero() {
		return -1
	}
	return at.Sub(attributionClock)
}

// U1: an opening the §54 rule attributes to the peer, inside the grace, still
// opens the breaker (the cost stays bounded) but keeps every hold, why=
// within_grace, one strike from re-opening. It says nothing about this
// machine's own network, so the egress is left as it was.
func TestAnAttributableOpeningInsideTheGraceKeepsTheHolds(t *testing.T) {
	for _, grace := range []time.Duration{0, testPeerDownGrace} {
		var b remotePoolBreaker
		var egress localEgress
		egress.noteAnswerOnANewConnection(at(-time.Minute))
		err := realDialTimeout(t)
		b.failure(at(0), at(2*time.Second), err, &egress, grace)
		egress.noteAnswerOnANewConnection(at(3 * time.Second))
		opened, peerDown, why := b.failure(at(5*time.Second), at(7*time.Second), err, &egress, grace)
		if grace == 0 {
			if !opened || !peerDown {
				t.Fatalf("calibration: with no grace, opened=%v peerDown=%v; the opening must be attributable to the peer", opened, peerDown)
			}
			continue
		}
		if !opened || peerDown || why != keptWithinGrace {
			t.Fatalf("opened=%v peerDown=%v why=%q; a peer silent for 7 s of a %v grace must keep the holds why=%s", opened, peerDown, why, grace, keptWithinGrace)
		}
		if b.allow(at(10 * time.Second)) {
			t.Fatal("an opening inside the grace let the next request through inside the cooldown")
		}
		if b.failures != remotePoolFailuresToOpen-1 || !b.unattributed {
			t.Fatalf("failures=%d unattributed=%v; want one strike from re-opening, as a kept opening leaves it", b.failures, b.unattributed)
		}
		if view := egress.view(); !view.lastFailure.IsZero() || !view.upSince.Equal(at(-time.Minute)) {
			t.Fatalf("egress %+v; a grace opening is not this machine offline and must not note a local failure", view)
		}
	}
}

// U2: a box that answers nothing while local turns keep answering, one strike
// every 32 s. With the grace, the first strike that fails at least the grace
// after the first one was sent finds it down, and every opening before keeps
// the holds. With no grace it is down at the second strike, as §54 has it.
func TestThePeerIsDownAtTheFirstStrikePastTheGrace(t *testing.T) {
	err := realDialTimeout(t)
	for _, tc := range []struct {
		grace time.Duration
		want  time.Duration
	}{
		{0, 32*time.Second + remotePoolDialTimeout},
		// 29 strikes after the first: 29*32 s + 2 s = 930 s is the first to
		// fail 15 min or more after 0.
		{testPeerDownGrace, 29*32*time.Second + remotePoolDialTimeout},
	} {
		var b remotePoolBreaker
		var egress localEgress
		egress.noteAnswerOnANewConnection(at(-time.Minute))
		strikes := strikeEvery(&b, &egress, at(0), at(20*time.Minute), 32*time.Second, tc.grace, err)
		if got := sinceClock(peerDownAt(t, strikes)); got != tc.want {
			t.Fatalf("grace %v: peer down at %v, want %v", tc.grace, got, tc.want)
		}
	}
}

// A box is found down only when both rules blame it (PATCHES §67, owner,
// 2026-11-19): gateway attribution, which needs a new connection this
// machine completed since the outage began, and the grace, which needs the
// box silent for it. One strike every 32 s for 20 minutes, with a local turn
// answering before each on a new connection, or only on pooled ones as
// behind the 11/19 gateway.
func TestABoxIsFoundDownOnlyWhenGatewayAttributionAndTheGraceBothBlameIt(t *testing.T) {
	err := realDialTimeout(t)
	for _, tc := range []struct {
		name          string
		newConnection bool
		grace         time.Duration
		down          bool
		why           string
	}{
		{"new connections, silent past the grace", true, testPeerDownGrace, true, ""},
		{"new connections, silent inside the grace", true, time.Hour, false, keptWithinGrace},
		{"no new connection, silent past the grace", false, testPeerDownGrace, false, keptNoNewConnection},
		{"no new connection, no grace", false, 0, false, keptNoNewConnection},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b remotePoolBreaker
			var egress localEgress
			egress.noteAnswerOnANewConnection(at(-time.Minute))
			down, lastWhy, openings := false, "", 0
			for started := at(0); !started.After(at(20 * time.Minute)); started = started.Add(32 * time.Second) {
				if tc.newConnection {
					egress.noteAnswerOnANewConnection(started.Add(-time.Second))
				} else {
					egress.noteAnswer(started.Add(-time.Second))
				}
				opened, peerDown, why := b.failure(started, started.Add(remotePoolDialTimeout), err, &egress, tc.grace)
				if peerDown {
					down = true
					break
				}
				if opened {
					lastWhy, openings = why, openings+1
				}
			}
			if down != tc.down {
				t.Fatalf("found down = %v after 20 minutes of strikes, want %v (last kept why=%q)", down, tc.down, lastWhy)
			}
			if !tc.down && (openings == 0 || lastWhy != tc.why) {
				t.Fatalf("%d openings, the last kept why=%q; want them kept why=%s", openings, lastWhy, tc.why)
			}
		})
	}
}

// The grace's edge: a box silent for the grace or longer is down. A strike
// that fails exactly the grace after the box went quiet finds it down, and one
// that fails a nanosecond sooner keeps the holds.
func TestABoxSilentForExactlyTheGraceIsDown(t *testing.T) {
	err := realDialTimeout(t)
	for _, tc := range []struct {
		failedAt time.Duration
		peerDown bool
		why      string
	}{
		{testPeerDownGrace - time.Nanosecond, false, keptWithinGrace},
		{testPeerDownGrace, true, ""},
	} {
		var b remotePoolBreaker
		var egress localEgress
		egress.noteAnswerOnANewConnection(at(-time.Minute))
		strikes := strikeEvery(&b, &egress, at(0), at(28*32*time.Second), 32*time.Second, testPeerDownGrace, err)
		if down := peerDownAt(t, strikes); !down.IsZero() {
			t.Fatalf("calibration: peer down at %v, inside the grace", sinceClock(down))
		}
		started := at(28*32*time.Second + remotePoolDialTimeout)
		egress.noteAnswerOnANewConnection(started.Add(-time.Second))
		opened, peerDown, why := b.failure(started, at(tc.failedAt), err, &egress, testPeerDownGrace)
		if !opened || peerDown != tc.peerDown || why != tc.why {
			t.Fatalf("a strike failing %v after the box went quiet: opened=%v peerDown=%v why=%q; want opened, peerDown=%v why=%q (grace %v)",
				tc.failedAt, opened, peerDown, why, tc.peerDown, tc.why, testPeerDownGrace)
		}
	}
}

// U3: the box answering restarts the grace. Silence is counted from the first
// strike sent after the answer, so a box that answered at 14 min is not down
// before 29 min. The replay is 07:45 on 2026-11-19, when the box answered at
// :02 and :04 and peer_down came at :07: inside the grace, the holds stay.
func TestAPeerAnswerRestartsTheGrace(t *testing.T) {
	err := realDialTimeout(t)
	t.Run("an answer at 14 min", func(t *testing.T) {
		var b remotePoolBreaker
		var egress localEgress
		egress.noteAnswerOnANewConnection(at(-time.Minute))
		before := strikeEvery(&b, &egress, at(0), at(26*32*time.Second), 32*time.Second, testPeerDownGrace, err)
		if down := peerDownAt(t, before); !down.IsZero() {
			t.Fatalf("calibration: peer down at %v, before the answer", sinceClock(down))
		}
		b.success(at(14 * time.Minute))
		after := strikeEvery(&b, &egress, at(27*32*time.Second), at(40*time.Minute), 32*time.Second, testPeerDownGrace, err)
		// The first strike after the answer left at 27*32 s = 864 s; the first
		// to fail 15 min after that is 56*32 s + 2 s = 1794 s.
		if got, want := sinceClock(peerDownAt(t, after)), 56*32*time.Second+remotePoolDialTimeout; got != want {
			t.Fatalf("peer down at %v, want %v: silence starts at the first strike sent after the answer", got, want)
		}
	})
	t.Run("07:45 replay", func(t *testing.T) {
		for _, tc := range []struct {
			grace    time.Duration
			peerDown bool
			why      string
		}{
			{0, true, ""},
			{testPeerDownGrace, false, keptWithinGrace},
		} {
			var b remotePoolBreaker
			var egress localEgress
			egress.noteAnswerOnANewConnection(at(-time.Minute))
			b.success(at(2 * time.Second))
			b.success(at(4 * time.Second))
			egress.noteAnswerOnANewConnection(at(4500 * time.Millisecond))
			b.failure(at(3*time.Second), at(5*time.Second), err, &egress, tc.grace)
			b.failure(at(4*time.Second), at(6*time.Second), err, &egress, tc.grace)
			opened, peerDown, why := b.failure(at(5*time.Second), at(7*time.Second), err, &egress, tc.grace)
			if !opened || peerDown != tc.peerDown || why != tc.why {
				t.Fatalf("grace %v: opened=%v peerDown=%v why=%q at :07, want opened, peerDown=%v why=%q", tc.grace, opened, peerDown, why, tc.peerDown, tc.why)
			}
		}
	})
}

// U4: a usage body from the box is an answer too. It restarts the grace and
// leaves the breaker as it was: still open, still one strike from re-opening.
func TestAUsageBodyRestartsTheGraceOnly(t *testing.T) {
	err := realDialTimeout(t)
	var b remotePoolBreaker
	var egress localEgress
	egress.noteAnswerOnANewConnection(at(-time.Minute))
	strikeEvery(&b, &egress, at(0), at(26*32*time.Second), 32*time.Second, testPeerDownGrace, err)
	b.noteAnswer(at(14 * time.Minute))
	if b.allow(at(850 * time.Second)) {
		t.Fatal("a usage body closed the breaker inside its cooldown")
	}
	after := strikeEvery(&b, &egress, at(27*32*time.Second), at(40*time.Minute), 32*time.Second, testPeerDownGrace, err)
	if !after[0].opened {
		t.Fatal("a usage body reset the strike count: the next failure must re-open the breaker at once")
	}
	if got, want := sinceClock(peerDownAt(t, after)), 56*32*time.Second+remotePoolDialTimeout; got != want {
		t.Fatalf("peer down at %v, want %v: the usage body at 14 min restarts the grace", got, want)
	}
}

// U5: a gap of more than 5 min between strikes is idle time or a sleeping Mac,
// not observed silence. The strike after it starts the grace again.
func TestAGapBetweenStrikesRestartsTheGrace(t *testing.T) {
	err := realDialTimeout(t)
	var b remotePoolBreaker
	var egress localEgress
	egress.noteAnswerOnANewConnection(at(-time.Minute))
	strikeEvery(&b, &egress, at(0), at(7*32*time.Second), 32*time.Second, testPeerDownGrace, err)
	resumed := at(7*32*time.Second + 6*time.Minute) // 584 s
	after := strikeEvery(&b, &egress, resumed, at(40*time.Minute), 32*time.Second, testPeerDownGrace, err)
	// 584 s + 29*32 s + 2 s = 1514 s is the first to fail 15 min after 584 s.
	if got, want := sinceClock(peerDownAt(t, after)), 584*time.Second+29*32*time.Second+remotePoolDialTimeout; got != want {
		t.Fatalf("peer down at %v, want %v: the 6-min gap restarts the grace", got, want)
	}
}

// U6: time this machine spent offline never counts. A local failure at 10 min
// and a local answer a second later: the grace runs from that answer, so the
// box is not down before 25 min.
func TestTheGraceRunsFromWhenThisMachineCameBack(t *testing.T) {
	err := realDialTimeout(t)
	var b remotePoolBreaker
	var egress localEgress
	egress.noteAnswerOnANewConnection(at(-time.Minute))
	strikeEvery(&b, &egress, at(0), at(18*32*time.Second), 32*time.Second, testPeerDownGrace, err)
	egress.noteFailure(at(10 * time.Minute))
	egress.noteAnswerOnANewConnection(at(10*time.Minute + time.Second))
	after := strikeEvery(&b, &egress, at(19*32*time.Second), at(40*time.Minute), 32*time.Second, testPeerDownGrace, err)
	// 47*32 s + 2 s = 1506 s is the first to fail 15 min after 601 s. The
	// strike that straddles the drop keeps the holds as §54 does.
	if got, want := sinceClock(peerDownAt(t, after, keptOneStrikeSinceBack)), 47*32*time.Second+remotePoolDialTimeout; got != want {
		t.Fatalf("peer down at %v, want %v: the grace must run from this machine's first answer after its failure", got, want)
	}
}

// U7: a strike sent just before the box's last answer, and failing just after
// it, does not start the grace early: silence starts at the answer.
func TestAStrikeSentBeforeTheLastAnswerDoesNotStartTheGraceEarly(t *testing.T) {
	err := realDialTimeout(t)
	var b remotePoolBreaker
	var egress localEgress
	egress.noteAnswerOnANewConnection(at(-time.Minute))
	b.success(at(5 * time.Minute))
	b.failure(at(5*time.Minute-time.Second), at(5*time.Minute+time.Second), err, &egress, testPeerDownGrace)
	// The 28th strike fails at 19:59.5, half a second inside a grace that
	// runs from the answer at 5:00 and half a second past one that ran from
	// the strike sent at 4:59.
	first := at(5*time.Minute + 1500*time.Millisecond)
	strikes := strikeEvery(&b, &egress, first, at(40*time.Minute), 32*time.Second, testPeerDownGrace, err)
	if got, want := sinceClock(peerDownAt(t, strikes)), 5*time.Minute+3500*time.Millisecond+29*32*time.Second; got != want {
		t.Fatalf("peer down at %v, want %v: the grace must run from the last answer", got, want)
	}
}

// U4 through the real usage fetch: the box answers its usage endpoint on a
// connection that still works while every forward fails, the 2026-11-19
// shape. That answer restarts the grace, so the opening after it keeps the
// holds although the box was silent for longer than the grace before it.
// Without the answer the same opening finds it down.
func TestAUsageFetchTheBoxAnswersRestartsTheGrace(t *testing.T) {
	for _, fetched := range []bool{false, true} {
		server, key := heldSessionServer(t, "http://100.64.0.9:31416")
		server.PeerDownGrace = testPeerDownGrace
		pool := server.RemotePool
		err := realDialTimeout(t)
		pool.client.Transport = proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodGet && req.URL.Path == remotePoolUsagePath {
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`[]`)), Request: req}, nil
			}
			return nil, err
		})
		logs := &syncBuffer{}
		server.Logger = slog.New(slog.NewJSONHandler(logs, nil))
		pool.egress.noteAnswer(time.Now().Add(-testPeerDownGrace - time.Hour))
		forward := func() map[string]any {
			t.Helper()
			localUpstreamAnswered(pool)
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
			server.forwardToPeer(pool, req, "claude", "s1", "/v1/messages", "", []byte(`{}`), remotePoolHoldSet, "peer-acct")
			failed := e2eLogLines(logs, "remote pool forward failed")
			return failed[len(failed)-1]
		}

		forward()
		rewindQuiet(pool, testPeerDownGrace)
		if fetched {
			if snapshot := pool.AdminGet(context.Background(), remotePoolUsagePath); !snapshot.Reachable {
				t.Fatalf("calibration: the usage fetch did not reach the box: %s", snapshot.Err)
			}
		}
		opening := forward()
		if !e2eLineSays(opening, "breaker_opened", true) || !e2eLineSays(opening, "peer_down", !fetched) {
			t.Fatalf("usage answered=%v: forward-failed line %v, want the opening with peer_down=%v", fetched, opening, !fetched)
		}
		if down := pool.breaker.foundDown(); down == fetched || !pool.delegations.active(key, time.Now()) {
			t.Fatalf("usage answered=%v: found down=%v after the opening; want down=%v and the hold kept either way (PATCHES §67)", fetched, down, !fetched)
		}
	}
}

// failingPeerTransport fails every peer round trip as a real dial timeout.
func failingPeerTransport(t *testing.T) http.RoundTripper {
	err := realDialTimeout(t)
	return proxyRoundTripFunc(func(*http.Request) (*http.Response, error) { return nil, err })
}

// rewindQuiet moves the start of the peer's silence back by d, under the
// breaker's lock, as if that much silence had passed (no waiting; PATCHES §45).
func rewindQuiet(pool *RemotePool, d time.Duration) {
	pool.breaker.mu.Lock()
	pool.breaker.quietSince = pool.breaker.quietSince.Add(-d)
	pool.breaker.mu.Unlock()
}

// U14 and C5 through the forward path: openings inside the grace keep every
// hold and their line says how long the box has been silent and what the
// grace is; the first opening past it finds the box down and keeps every
// hold, with one line naming how many (PATCHES §67): each chat's hold ends
// only when it is placed as a refugee.
func TestAPeerDownPastTheGraceKeepsEveryHoldWithOneLine(t *testing.T) {
	server, first := heldSessionServer(t, "http://100.64.0.9:31416")
	server.PeerDownGrace = testPeerDownGrace
	pool := server.RemotePool
	holds := map[string]string{first: "peer-acct"}
	for _, sessionID := range []string{"s2", "s3"} {
		key := session.ScopedSessionKey("claude", sessionID)
		pool.delegations.set(key, time.Now(), remotePoolDelegationTTLFor("claude"), "acct-"+sessionID, "box")
		holds[key] = "acct-" + sessionID
	}
	pool.client.Transport = failingPeerTransport(t)
	logs := &syncBuffer{}
	server.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	// This machine has been up since well before the grace began.
	pool.egress.noteAnswer(time.Now().Add(-testPeerDownGrace - time.Hour))
	forward := func() map[string]any {
		t.Helper()
		localUpstreamAnswered(pool)
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
		server.forwardToPeer(pool, req, "claude", "a-new-session", "/v1/messages", "", []byte(`{}`), remotePoolHoldSet, "")
		failed := e2eLogLines(logs, "remote pool forward failed")
		return failed[len(failed)-1]
	}

	forward()
	rewindQuiet(pool, 10*time.Second)
	if opening := forward(); !e2eLineSays(opening, "breaker_opened", true) || !e2eLineSays(opening, "peer_down", false) {
		t.Fatalf("forward-failed line %v; inside the grace the opening must keep the holds", opening)
	}
	kept := e2eLogLines(logs, holdsKeptMessage)
	if len(kept) != 1 || kept[0]["why"] != keptWithinGrace || kept[0]["grace_ms"] != float64(testPeerDownGrace.Milliseconds()) {
		t.Fatalf("holds-kept lines %v; want one, why=%s, grace_ms=%d", kept, keptWithinGrace, testPeerDownGrace.Milliseconds())
	}
	if quiet, ok := kept[0]["quiet_for_ms"].(float64); !ok || quiet < 10_000 || quiet >= 60_000 {
		t.Fatalf("holds-kept line %v; quiet_for_ms must be the 10 s and change the box has been silent", kept[0])
	}
	for key := range holds {
		if !pool.delegations.active(key, time.Now()) {
			t.Fatalf("%s lost its hold inside the grace", key)
		}
	}
	if ended := e2eLogLines(logs, "remote pool hold ended"); len(ended) != 0 {
		t.Fatalf("holds ended inside the grace: %v", ended)
	}

	e2eRewindCooldown(pool)
	rewindQuiet(pool, testPeerDownGrace)
	if down := forward(); !e2eLineSays(down, "breaker_opened", true) || !e2eLineSays(down, "peer_down", true) {
		t.Fatalf("forward-failed line %v; past the grace the box is down", down)
	}
	if ended := e2eLogLines(logs, "remote pool hold ended"); len(ended) != 0 {
		t.Fatalf("hold-ended lines %v; a box found down ends no hold (PATCHES §67)", ended)
	}
	if downs := e2eLogLines(logs, peerFoundDownMessage); len(downs) != 1 || downs[0]["held"] != float64(len(holds)) || downs[0]["remote"] != pool.name() {
		t.Fatalf("peer-down lines %v; want one naming the %d holds it keeps", downs, len(holds))
	}
	for key := range holds {
		if !pool.delegations.active(key, time.Now()) {
			t.Fatalf("%s lost its hold when the box was found down; it ends only when its chat is placed", key)
		}
	}
}

// The websocket twin, with no grace (§54): the second failed dial while this
// machine answers finds the box down, keeps both holds, and says so once.
func TestAWebSocketPeerDownKeepsEveryHoldWithOneLine(t *testing.T) {
	server := heldCodexWebSocketServer(t, "http://100.64.0.9:31416")
	pool := server.RemotePool
	other := session.ScopedSessionKey("codex", "another-warm-session")
	pool.delegations.set(other, time.Now(), remotePoolDelegationTTLFor("codex"), "other-acct", pool.name())
	err := realDialTimeout(t)
	pool.wsDialer.Proxy = nil
	pool.wsDialer.NetDialContext = func(context.Context, string, string) (net.Conn, error) { return nil, err }
	logs := &syncBuffer{}
	server.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	pool.egress.noteAnswer(time.Now().Add(-time.Minute))
	for i := 0; i < remotePoolFailuresToOpen; i++ {
		localUpstreamAnswered(pool)
		forwardHeldWebSocket(t, server, context.Background())
	}
	failed := e2eLogLines(logs, "remote pool websocket dial failed")
	if len(failed) != remotePoolFailuresToOpen || !e2eLineSays(failed[1], "peer_down", true) {
		t.Fatalf("calibration: dial-failed lines %v, want the second finding the box down", failed)
	}
	if ended := e2eLogLines(logs, "remote pool hold ended"); len(ended) != 0 {
		t.Fatalf("hold-ended lines %v; a box found down ends no hold (PATCHES §67)", ended)
	}
	if downs := e2eLogLines(logs, peerFoundDownMessage); len(downs) != 1 || downs[0]["held"] != float64(2) {
		t.Fatalf("peer-down lines %v; want one naming both holds it keeps", downs)
	}
	for _, key := range []string{other, wsHoldKey()} {
		if !pool.delegations.active(key, time.Now()) {
			t.Fatalf("%s lost its hold when the box was found down", key)
		}
	}
}
