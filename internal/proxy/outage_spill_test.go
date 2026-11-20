package proxy

import (
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// The outage spill (PATCHES §59). On 10/20 the laptop could not reach
// api.anthropic.com while the peer, dialed by IP, answered. These tests drive
// the whole handler (the hold rig) with a peer daemon beside it.

func peerAnswer(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set(subrouterAccountHeader, "peer-acct")
	_, _ = io.WriteString(w, `{"id":"msg_peer"}`)
}

// spillTestPolicy spills at rate with burst on a clock that stands still
// unless the test moves it, so a test decides when tokens come back.
func spillTestPolicy(rate float64, burst int, now func() time.Time) *OutageSpillPolicy {
	policy := NewOutageSpillPolicy(rate, burst)
	policy.bucket.now = now
	return policy
}

// settle waits for n handler runs to end, so the lines logged after the
// response went out (the spill's outcome, the hold's end) are in the log.
func (rig *holdRig) settle(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case <-rig.handled:
		case <-time.After(10 * time.Second):
			t.Fatal("a handler run did not end")
		}
	}
}

func standingClock() func() time.Time {
	at := time.Date(2026, 10, 20, 21, 49, 0, 0, time.UTC)
	return func() time.Time { return at }
}

func TestOutageSpillServesAnUnreachableTurnOnThePeer(t *testing.T) {
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
				upstream:  answerOK,
				pin:       "one@example.com",
				failDials: func(int64) error { return failure },
				peer:      peerAnswer,
				spill:     spillTestPolicy(DefaultOutageSpillRate, DefaultOutageSpillBurst, standingClock()),
			})
			resp, body, err := rig.turn(t, context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			rig.settle(t, 1)
			if resp.StatusCode != http.StatusOK || !strings.Contains(body, "msg_peer") || resp.Header.Get(subrouterPoolHeader) != "remote" {
				t.Fatalf("status %d pool %q body %q: want the peer's 200", resp.StatusCode, resp.Header.Get(subrouterPoolHeader), body)
			}
			if rig.dials.Load() != 1 || rig.peerTurns.Load() != 1 {
				t.Fatalf("local dials %d, peer turns %d: want one of each", rig.dials.Load(), rig.peerTurns.Load())
			}
			// One turn, not a move: the session keeps its local account and
			// the peer gets no hold on it.
			if got, _ := rig.store.Get("claude", "hold-session"); got.AccountID != "one@example.com" {
				t.Fatalf("the session moved to %q", got.AccountID)
			}
			if rig.pool.delegations.active(session.ScopedSessionKey("claude", "hold-session"), time.Now()) {
				t.Fatal("a spilled turn gave the peer a hold on the session")
			}
			for _, want := range []string{"msg=\"outage spill\"", "outcome=served", "reason=upstream_unreachable:" + tc.cause} {
				if rig.countLog(want) != 1 {
					t.Fatalf("log lacks one %q:\n%s", want, rig.logs.String())
				}
			}
		})
	}
}

// With the bucket empty the turn is held (§58), and comes back local when the
// network does. The second turn is a new session's: the first session, warm
// on its local account by then, would wait for it without spilling (PATCHES
// §67).
func TestOutageSpillAtItsCapHoldsTheTurn(t *testing.T) {
	rig := newHoldRig(t, holdRigOptions{
		hold:     holdTestPolicy(5 * time.Second),
		upstream: answerOK,
		failDials: func(n int64) error {
			if n <= 4 {
				return refusedFailure()
			}
			return nil
		},
		peer:  peerAnswer,
		spill: spillTestPolicy(1, 1, standingClock()),
	})
	first, body, err := rig.turn(t, context.Background(), nil)
	if err != nil || first.StatusCode != http.StatusOK || !strings.Contains(body, "msg_peer") {
		t.Fatalf("first turn: %v %q %v; want the peer's 200 on the one token", first, body, err)
	}
	second, body := rig.turnOf(t, "second-session")
	if second.StatusCode != http.StatusOK || strings.Contains(body, "msg_peer") || second.Header.Get(subrouterPoolHeader) != "local" {
		t.Fatalf("second turn: %v %q; want local's 200 after the hold", second, body)
	}
	rig.settle(t, 2)
	if rig.peerTurns.Load() != 1 || rig.dials.Load() != 5 {
		t.Fatalf("peer turns %d, local dials %d: want 1 and 5", rig.peerTurns.Load(), rig.dials.Load())
	}
	if rig.countLog("outcome=capped") != 3 || rig.countLog("proxy hold started") != 1 || rig.countLog("outcome=answered") != 1 {
		t.Fatalf("want 3 capped spills and one hold that was answered:\n%s", rig.logs.String())
	}
}

// A held turn may go to the peer: the hold re-runs the handler, and an
// attempt that finds a token spills.
func TestOutageSpillTakesAHeldTurn(t *testing.T) {
	at := time.Date(2026, 10, 20, 21, 49, 0, 0, time.UTC)
	var reads atomic.Int64
	clock := func() time.Time {
		// The test's own drain is read 1 and the first attempt read 2; a
		// second later there is a token again.
		if reads.Add(1) <= 2 {
			return at
		}
		return at.Add(time.Second)
	}
	spill := spillTestPolicy(1, 1, clock)
	if !spill.take() {
		t.Fatal("a fresh bucket had no token")
	}
	rig := newHoldRig(t, holdRigOptions{
		hold:      holdTestPolicy(5 * time.Second),
		upstream:  answerOK,
		failDials: func(int64) error { return dnsFailure("api.anthropic.com") },
		peer:      peerAnswer,
		spill:     spill,
	})
	resp, body, err := rig.turn(t, context.Background(), nil)
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(body, "msg_peer") {
		t.Fatalf("%v %q %v: want the peer's 200 on the held attempt", resp, body, err)
	}
	rig.settle(t, 1)
	logs := rig.logs.String()
	if rig.countLog("proxy hold started") != 1 || rig.countLog("outcome=capped") != 1 || rig.countLog("outcome=served") != 1 ||
		!strings.Contains(logs, "outcome=answered status=200 attempts=2") {
		t.Fatalf("want a hold whose second attempt spilled:\n%s", logs)
	}
}

// Under load the bucket is the cap: 40 turns at once, a burst of 5 and no
// refill, and exactly 5 reach the peer.
func TestOutageSpillCapHoldsUnderLoad(t *testing.T) {
	rig := newHoldRig(t, holdRigOptions{
		upstream:  answerOK,
		failDials: func(int64) error { return refusedFailure() },
		peer:      peerAnswer,
		spill:     spillTestPolicy(DefaultOutageSpillRate, 5, standingClock()),
	})
	const turns = 40
	statuses := make(chan int, turns)
	var wg sync.WaitGroup
	for range turns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _, err := rig.turn(t, context.Background(), nil)
			if err != nil {
				statuses <- 0
				return
			}
			statuses <- resp.StatusCode
		}()
	}
	wg.Wait()
	rig.settle(t, turns)
	close(statuses)
	counts := map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	if counts[http.StatusOK] != 5 || counts[http.StatusBadGateway] != turns-5 || rig.peerTurns.Load() != 5 {
		t.Fatalf("statuses %v, peer turns %d: want 5 served by the peer and 35 answered 502", counts, rig.peerTurns.Load())
	}
	if rig.countLog("outcome=capped") != turns-5 {
		t.Fatalf("%d capped lines, want %d", rig.countLog("outcome=capped"), turns-5)
	}
}

// 429 and 529 are answers, not an unreachable upstream: account switching
// runs exactly as with the spill off -- the same answer, the same upstream
// hits per account, and the same peer traffic (the existing give-up stage
// still forwards a turn every local account refused).
func TestOutageSpillLeaves429And529Alone(t *testing.T) {
	twoAccounts := []accounts.Account{
		{ID: "cooked@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-cooked"},
		{ID: "fresh@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-fresh"},
	}
	run := func(t *testing.T, spill bool, upstream func(*sync.Map) http.HandlerFunc) (string, *holdRig) {
		hits := &sync.Map{}
		opts := holdRigOptions{accounts: twoAccounts, pin: "cooked@example.com", upstream: upstream(hits), peer: peerAnswer}
		if spill {
			opts.spill = spillTestPolicy(DefaultOutageSpillRate, DefaultOutageSpillBurst, standingClock())
		}
		rig := newHoldRig(t, opts)
		resp, body, err := rig.turn(t, context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		rig.settle(t, 1)
		var counts []string
		for _, token := range []string{"tok-cooked", "tok-fresh"} {
			var n int64
			if counter, ok := hits.Load(token); ok {
				n = counter.(*atomic.Int64).Load()
			}
			counts = append(counts, fmt.Sprintf("%s=%d", token, n))
		}
		return fmt.Sprintf("%d %q [%s] peer=%d", resp.StatusCode, body, strings.Join(counts, " "), rig.peerTurns.Load()), rig
	}
	count := func(hits *sync.Map, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		n, _ := hits.LoadOrStore(token, new(atomic.Int64))
		n.(*atomic.Int64).Add(1)
	}
	rejected := func(status int, body string, only string) func(*sync.Map) http.HandlerFunc {
		return func(hits *sync.Map) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				count(hits, r)
				_, _ = io.Copy(io.Discard, r.Body)
				if only != "" && r.Header.Get("Authorization") != "Bearer "+only {
					_, _ = w.Write([]byte(`{"id":"msg_fresh"}`))
					return
				}
				if status == http.StatusTooManyRequests {
					w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(body))
			}
		}
	}
	limit := `{"type":"error","error":{"type":"rate_limit_error","message":"limit"}}`
	overloaded := `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`
	for name, upstream := range map[string]func(*sync.Map) http.HandlerFunc{
		"429 fails over":                    rejected(http.StatusTooManyRequests, limit, "tok-cooked"),
		"429 everywhere reaches the client": rejected(http.StatusTooManyRequests, limit, ""),
		"529 reaches the client":            rejected(529, overloaded, ""),
	} {
		t.Run(name, func(t *testing.T) {
			without, _ := run(t, false, upstream)
			with, rig := run(t, true, upstream)
			if with != without {
				t.Fatalf("with the spill: %s; without: %s", with, without)
			}
			if rig.countLog("outage spill") != 0 {
				t.Fatalf("a %s was taken for an unreachable upstream:\n%s", name, rig.logs.String())
			}
		})
	}
}

// A request that may have reached the upstream -- its connection closed after
// it was sent -- is never sent again elsewhere: the upstream may be running it.
func TestOutageSpillNeverResendsASentRequest(t *testing.T) {
	rig := newHoldRig(t, holdRigOptions{
		upstream: func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			conn, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				conn.Close()
			}
		},
		peer:  peerAnswer,
		spill: spillTestPolicy(DefaultOutageSpillRate, DefaultOutageSpillBurst, standingClock()),
	})
	resp, _, err := rig.turn(t, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rig.settle(t, 1)
	if resp.StatusCode != http.StatusBadGateway || rig.peerTurns.Load() != 0 || rig.countLog("outage spill") != 0 {
		t.Fatalf("status %d, peer turns %d: a sent request was spilled", resp.StatusCode, rig.peerTurns.Load())
	}
}

// A peer that does not answer either: the spill serves nothing, writes
// nothing, and the hold carries the turn until local comes back.
func TestOutageSpillWithThePeerDownHolds(t *testing.T) {
	dead, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadURL := "http://" + dead.Addr().String()
	dead.Close()
	rig := newHoldRig(t, holdRigOptions{
		hold:     holdTestPolicy(5 * time.Second),
		upstream: answerOK,
		failDials: func(n int64) error {
			if n <= 3 {
				return refusedFailure()
			}
			return nil
		},
		peer:  peerAnswer,
		spill: spillTestPolicy(DefaultOutageSpillRate, DefaultOutageSpillBurst, standingClock()),
	})
	rig.pool.URL = mustURL(t, deadURL)
	resp, _, err := rig.turn(t, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rig.settle(t, 1)
	if resp.StatusCode != http.StatusOK || resp.Header.Get(subrouterPoolHeader) != "local" {
		t.Fatalf("status %d pool %q: want local's 200 after the hold", resp.StatusCode, resp.Header.Get(subrouterPoolHeader))
	}
	if rig.countLog("outcome=not_served") == 0 || rig.countLog("outcome=served") != 0 || rig.countLog("proxy hold started") != 1 {
		t.Fatalf("want spills that served nothing and one hold:\n%s", rig.logs.String())
	}
}

// What never spills: a canary (one observable attempt), a request another
// daemon forwarded, and any turn when the spill is off.
func TestOutageSpillLeavesCanariesForwardsAndTheOffSwitchAlone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra http.Header
		spill *OutageSpillPolicy
	}{
		{"canary", http.Header{subrouterNoRetryHeader: {"1"}}, spillTestPolicy(DefaultOutageSpillRate, DefaultOutageSpillBurst, standingClock())},
		{"peer forward", http.Header{remotePoolHopHeader: {"1"}}, spillTestPolicy(DefaultOutageSpillRate, DefaultOutageSpillBurst, standingClock())},
		{"spill off", nil, NewOutageSpillPolicy(0, DefaultOutageSpillBurst)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newHoldRig(t, holdRigOptions{
				upstream:  answerOK,
				failDials: func(int64) error { return dnsFailure("api.anthropic.com") },
				peer:      peerAnswer,
				spill:     tc.spill,
			})
			resp, body, err := rig.turn(t, context.Background(), tc.extra)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusBadGateway || body != "upstream request failed\n" || rig.peerTurns.Load() != 0 {
				t.Fatalf("status %d body %q peer turns %d: want today's 502 and nothing on the peer", resp.StatusCode, body, rig.peerTurns.Load())
			}
		})
	}
}

func TestOutageSpillPolicyDefaults(t *testing.T) {
	for _, rate := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		if NewOutageSpillPolicy(rate, 10) != nil {
			t.Fatalf("rate %v gave a policy; a rate that is not a positive finite number must turn the spill off", rate)
		}
	}
	policy := NewOutageSpillPolicy(2, 0)
	if policy.Burst != 1 {
		t.Fatalf("burst %d, want a burst below one to be one", policy.Burst)
	}
	if DefaultOutageSpillRate >= publicRatePerSecond/2 {
		t.Fatalf("the default spill rate %v is not well under the box's %v a second", DefaultOutageSpillRate, publicRatePerSecond)
	}
}

// A held turn whose peer attempt failed transiently (serveHeldTurn sets
// stayLocal) may still be running on that peer: when local cannot be reached
// either, the turn is held, never sent to a peer a second time.
func TestOutageSpillNeverResendsAHeldTurnItsPeerJustFailed(t *testing.T) {
	a := crossPoolClaudeScore("acct-a", 1, 30*time.Hour)
	a.ShortHeadroom = 1
	box := newContractBox(t, boxAccount{Score: a})
	box.turnStatus, box.turnTransient = http.StatusBadGateway, true
	laptop := newCrossPoolLaptop(t, []selectacct.Score{crossPoolClaudeScore("loc", 0.2, 150*time.Hour)}, box.pool)
	laptop.server.Transport = &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, dnsFailure("api.anthropic.com")
	}}
	laptop.server.OutageSpill = spillTestPolicy(DefaultOutageSpillRate, DefaultOutageSpillBurst, standingClock())
	holdOn(t, box.pool, heldSession, "acct-a")
	box.pins[heldSession] = "acct-a"

	if status, _ := laptop.turn(t, heldSession); status != http.StatusBadGateway {
		t.Fatalf("status %d, want the local failure's 502", status)
	}
	if turns := box.seen(); len(turns) != 1 {
		t.Fatalf("box turns %+v; the held attempt must be the only one the peer sees", turns)
	}
	if lines := laptop.logLines("outage spill"); len(lines) != 0 {
		t.Fatalf("spill lines %v", lines)
	}
}
