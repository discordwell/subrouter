package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The gateway of 2026-11-19. The building's gateway refused new outbound
// connections for seconds to minutes at a time while the connections already
// open kept working. The laptop's local turns kept answering over their open
// connections, so the breaker read this machine's network as up and blamed
// the box for its failed dials: 14 openings with peer_down=true between 07:08
// and 11:44, each a `dial tcp <box>:31416: i/o timeout` against a box that
// kept serving, and each ended every hold on that box. A blamed box also ends
// the warm wait of every session held on it. These drive the real handler
// over real listeners.

// attributionRig is the laptop behind the gateway. hold-session is warm on box
// A's acct-a, b-session on box B's acct-b, and local-session on the local
// account, which the daemon reaches over a connection it keeps open. While
// gateway is on, every new connection fails at the dial (the upstream, box A,
// box B), and connections already open keep working. While boxADown is on,
// only box A's dials fail. Each failed dial to box A first runs onBoxADial,
// so the evidence it makes lands during the strike.
type attributionRig struct {
	*holdRig
	boxA, boxB *contractBox
	gateway    atomic.Bool
	boxADown   atomic.Bool
	onBoxADial func()
	// heldLocally counts hold-session turns the local upstream served;
	// localTurns the local-session turns sent, localAnswers those the local
	// account answered.
	heldLocally, localTurns, localAnswers atomic.Int64
}

func newAttributionRig(t *testing.T, keepAlive bool) *attributionRig {
	t.Helper()
	timeout := realDialTimeout(t)
	rig := &attributionRig{}
	behind := eligibleBoxAccount("acct-a")
	behind.Score.Headroom, behind.Score.WeeklyHeadroom, behind.Score.WeeklyRemaining = 0.5, 0.5, 0.5
	rig.boxA = newContractBox(t, behind)
	rig.boxB = newContractBox(t, eligibleBoxAccount("acct-b"))
	cutPeer(t, rig.boxA.pool, func(int64) error {
		if !rig.gateway.Load() && !rig.boxADown.Load() {
			return nil
		}
		if rig.onBoxADial != nil {
			rig.onBoxADial()
		}
		return timeout
	})
	cutPeer(t, rig.boxB.pool, func(int64) error {
		if rig.gateway.Load() {
			return timeout
		}
		return nil
	})
	rig.holdRig = newHoldRig(t, holdRigOptions{
		hold: strandedPolicy(nil),
		upstream: func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), `"hold-session"`) {
				rig.heldLocally.Add(1)
			}
			answerOK(w, r)
		},
		failDials: func(int64) error {
			if rig.gateway.Load() {
				return timeout
			}
			return nil
		},
		keepAlive: keepAlive,
		peers:     []*RemotePool{rig.boxA.pool, rig.boxB.pool},
	})
	if _, err := rig.store.Put("claude", "local-session", "one@example.com", ""); err != nil {
		t.Fatal(err)
	}
	holdOn(t, rig.boxA.pool, "hold-session", "acct-a")
	holdOn(t, rig.boxB.pool, "b-session", "acct-b")
	return rig
}

type gatewayAnswer struct {
	status int
	body   string
	err    error
}

// turnAs is one model turn of sessionID, its body naming the session.
func (rig *attributionRig) turnAs(ctx context.Context, sessionID string) gatewayAnswer {
	body := fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":16,"messages":[{"role":"user","content":%q}]}`, sessionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rig.daemon.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		return gatewayAnswer{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+holdTestProxyToken)
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", sessionID)
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		return gatewayAnswer{err: err}
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	return gatewayAnswer{resp.StatusCode, string(got), err}
}

// localTurn runs one local-session turn.
func (rig *attributionRig) localTurn() {
	rig.localTurns.Add(1)
	if got := rig.turnAs(context.Background(), "local-session"); got.err == nil && got.status == http.StatusOK && strings.Contains(got.body, "msg_held") {
		rig.localAnswers.Add(1)
	}
}

// heldTurn starts hold-session's turn; its answer arrives on the channel.
func (rig *attributionRig) heldTurn(t *testing.T) <-chan gatewayAnswer {
	done := make(chan gatewayAnswer, 1)
	ctx := promptly(t)
	go func() { done <- rig.turnAs(ctx, "hold-session") }()
	return done
}

// keptBeforeAnswered waits for the breaker to open on box A with every hold
// kept, and fails the test if hold-session is answered first.
func (rig *attributionRig) keptBeforeAnswered(t *testing.T, done <-chan gatewayAnswer) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for rig.countLog("holds kept") == 0 {
		select {
		case got := <-done:
			t.Fatalf("hold-session was answered (%d %q %v) before box A's breaker opened with its holds kept; logs:\n%s",
				got.status, got.body, got.err, rig.logs.String())
		case <-deadline:
			t.Fatalf("box A's breaker never opened with its holds kept; logs:\n%s", rig.logs.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// openings is every forward-failed line that opened box A's breaker.
func (rig *attributionRig) openings() []string {
	var out []string
	for _, line := range strings.Split(rig.logs.String(), "\n") {
		if strings.Contains(line, "remote pool forward failed") && strings.Contains(line, "remote="+rig.boxA.pool.name()) &&
			strings.Contains(line, "breaker_opened=true") {
			out = append(out, line)
		}
	}
	return out
}

// The gateway blip, end to end. A local turn opened the daemon's connection
// to the upstream before the blip. During it, every new connection fails,
// and the local turns keep answering over that open connection, one of them
// while each failed dial to box A is in flight. hold-session's turn waits
// through two failed dials and the breaker opening, which keeps every hold
// and names the evidence (no_new_connection). Once dials get through again,
// acct-a answers it. Nothing was asked of the local account or box B for it.
func TestAWarmBoxSessionRidesOutAGatewayBlipOnItsAccount(t *testing.T) {
	rig := newAttributionRig(t, true)
	rig.localTurn()
	if rig.localAnswers.Load() != 1 || rig.dials.Load() != 1 {
		t.Fatalf("calibration: local answers = %d over %d dials, want the local account's answer over one new connection; logs:\n%s",
			rig.localAnswers.Load(), rig.dials.Load(), rig.logs.String())
	}
	rig.onBoxADial = rig.localTurn
	rig.gateway.Store(true)

	done := rig.heldTurn(t)
	rig.keptBeforeAnswered(t, done)
	openings := rig.openings()
	if len(openings) != 1 || !strings.Contains(openings[0], "peer_down=false") {
		t.Fatalf("box A's openings %q, want one, with peer_down=false: this machine could not open a new connection either", openings)
	}
	if !strings.Contains(rig.logs.String(), "why=no_new_connection") {
		t.Fatalf("the holds-kept line does not name the evidence; logs:\n%s", rig.logs.String())
	}
	if rig.localAnswers.Load() < 3 || rig.dials.Load() != 1 {
		t.Fatalf("local answers = %d over %d local dials, want the warm-up and one during each failed dial to box A, all over the open connection",
			rig.localAnswers.Load(), rig.dials.Load())
	}
	if account, held := holdAccount(rig.boxA.pool, "hold-session"); !held || account != "acct-a" {
		t.Fatalf("hold = %q, %v during the blip; want hold-session still held on acct-a", account, held)
	}

	rig.gateway.Store(false)
	e2eRewindCooldown(rig.boxA.pool)
	got := <-done
	rig.settle(t, 1+int(rig.localTurns.Load()))
	if got.err != nil || got.status != http.StatusOK || !strings.Contains(got.body, `"served_by":"acct-a"`) {
		t.Fatalf("status = %d body = %q err = %v, want acct-a's answer once dials got through; logs:\n%s", got.status, got.body, got.err, rig.logs.String())
	}
	if turns := rig.boxA.seen(); len(turns) != 1 || turns[0].Target != "acct-a" {
		t.Fatalf("box A saw %+v, want the one turn, targeted at acct-a", turns)
	}
	if turns := rig.boxB.seen(); len(turns) != 0 {
		t.Fatalf("box B saw %+v, want nothing", turns)
	}
	if n := rig.heldLocally.Load(); n != 0 {
		t.Fatalf("the local upstream served hold-session %d times, want never: its cache is on acct-a", n)
	}
	if account, held := holdAccount(rig.boxA.pool, "hold-session"); !held || account != "acct-a" {
		t.Fatalf("hold = %q, %v; want hold-session still held on acct-a", account, held)
	}
}

// A box that is really down. Its dials time out while this machine opens new
// connections elsewhere, the upstream or box B, during each strike. The
// opening blames the box as before: every hold on it ends, and its warm
// session moves at once.
func TestADownBoxIsBlamedWhenThisMachineOpensNewConnections(t *testing.T) {
	for _, tc := range []struct {
		name      string
		keepAlive bool
		elsewhere func(rig *attributionRig)
	}{
		{"to the upstream", false, func(rig *attributionRig) {
			rig.localTurn()
		}},
		{"to box B", true, func(rig *attributionRig) {
			rig.localTurn()
			rig.turnAs(context.Background(), "b-session")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newAttributionRig(t, tc.keepAlive)
			rig.localTurn()
			rig.onBoxADial = func() { tc.elsewhere(rig) }
			rig.boxADown.Store(true)
			started := time.Now()
			got := <-rig.heldTurn(t)
			if got.err != nil || got.status != http.StatusOK || strings.Contains(got.body, "acct-a") || time.Since(started) > 5*time.Second {
				t.Fatalf("status = %d body = %q err = %v after %v, want another account's answer at once; logs:\n%s",
					got.status, got.body, got.err, time.Since(started), rig.logs.String())
			}
			if openings := rig.openings(); len(openings) != 1 || !strings.Contains(openings[0], "peer_down=true") {
				t.Fatalf("box A's openings %q, want one, with peer_down=true", openings)
			}
			if rig.countLog("holds kept") != 0 {
				t.Fatalf("a dead box kept its holds; logs:\n%s", rig.logs.String())
			}
			if _, held := holdAccount(rig.boxA.pool, "hold-session"); held {
				t.Fatal("box A was found down; its holds should be gone")
			}
		})
	}
}

// Nothing opened a new connection anywhere since box A's dials began to
// fail: the local turns answer over the connection opened before, and
// nothing else dials. This is the gateway as this machine sees it when
// nothing else needs a new connection during the strikes, so the holds are
// kept. A box that is really down is still found: once a new connection gets
// through anywhere, the next strike after the cooldown blames it, and
// hold-session moves.
func TestAnOpeningWithNoNewConnectionAnywhereKeepsTheHoldsUntilOneGetsThrough(t *testing.T) {
	rig := newAttributionRig(t, true)
	rig.localTurn()
	rig.onBoxADial = rig.localTurn
	rig.boxADown.Store(true)

	done := rig.heldTurn(t)
	rig.keptBeforeAnswered(t, done)
	if openings := rig.openings(); len(openings) != 1 || !strings.Contains(openings[0], "peer_down=false") ||
		!strings.Contains(rig.logs.String(), "why=no_new_connection") {
		t.Fatalf("box A's openings %q, want one, with peer_down=false and the holds kept why=no_new_connection; logs:\n%s", openings, rig.logs.String())
	}
	if rig.dials.Load() != 1 {
		t.Fatalf("calibration: %d local dials, want the warm-up's only", rig.dials.Load())
	}
	if account, held := holdAccount(rig.boxA.pool, "hold-session"); !held || account != "acct-a" {
		t.Fatalf("hold = %q, %v; want hold-session still held on acct-a", account, held)
	}

	if got := rig.turnAs(promptly(t), "b-session"); got.status != http.StatusOK || !strings.Contains(got.body, `"served_by":"acct-b"`) {
		t.Fatalf("calibration: b-session got %d %q, want acct-b's answer over a new connection to box B", got.status, got.body)
	}
	e2eRewindCooldown(rig.boxA.pool)
	got := <-done
	if got.err != nil || got.status != http.StatusOK || strings.Contains(got.body, "acct-a") {
		t.Fatalf("status = %d body = %q err = %v, want another account's answer once box A was found down; logs:\n%s",
			got.status, got.body, got.err, rig.logs.String())
	}
	if openings := rig.openings(); len(openings) != 2 || !strings.Contains(openings[1], "peer_down=true") {
		t.Fatalf("box A's openings %q, want a second, with peer_down=true", openings)
	}
	if _, held := holdAccount(rig.boxA.pool, "hold-session"); held {
		t.Fatal("box A was found down; its holds should be gone")
	}
}

// A strike that reached the box and went unanswered (a header timeout) did
// not fail to open a connection, so it is judged on the answers as before: a
// box that takes requests and never answers is blamed while local turns
// answer, with no new connection anywhere since. The same evidence keeps the
// holds when the strikes are failed dials.
func TestAStrikeThatReachedTheBoxIsJudgedOnAnswersAsBefore(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		peerDown bool
		why      string
	}{
		{"header timeout", realHeaderTimeout(t), true, ""},
		{"dial timeout", realDialTimeout(t), false, "no_new_connection"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b remotePoolBreaker
			var egress localEgress
			egress.noteAnswerOnANewConnection(at(-time.Hour))
			strikeAt(&b, &egress, at(0), tc.err)
			egress.noteAnswer(at(time.Second))
			opened, peerDown, why := strikeWhyAt(&b, &egress, at(3*time.Second), tc.err)
			if !opened || peerDown != tc.peerDown || why != tc.why {
				t.Fatalf("opened=%v peerDown=%v why=%q, want opened, peerDown=%v why=%q", opened, peerDown, why, tc.peerDown, tc.why)
			}
		})
	}
}
