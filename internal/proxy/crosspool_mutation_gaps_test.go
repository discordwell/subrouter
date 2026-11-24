package proxy

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
)

// PATCHES §31, R3 fix 8: tests for five mutations the round-2 review found no
// test would notice. Each was watched failing with its mutation applied (the
// mutation is named in the test's comment) and passes without it.

// N3 -- mutation: drop the refusal bound on the forced-switch entry into
// transportStageAcrossPools. A forced switch inside local failover (warm's
// window spent mid-turn) ranks three box accounts above the local candidate
// spare, and all three refuse: after remotePoolMaxTargetRefusals failed peer
// attempts local failover continues on spare; the third is never asked.
func TestForcedSwitchRefusalsAreBoundedThenLocalFailoverContinues(t *testing.T) {
	box := newContractBox(t,
		boxAccount{Score: crossPoolClaudeScore("x1", 1, 30*time.Hour)},
		boxAccount{Score: crossPoolClaudeScore("x2", 0.95, 30*time.Hour)},
		boxAccount{Score: crossPoolClaudeScore("x3", 0.9, 30*time.Hour)})
	for _, id := range []string{"x1", "x2", "x3"} {
		box.refuse[id] = selectacct.VerdictAtCap
	}
	laptop := newCrossPoolLaptop(t, []selectacct.Score{
		crossPoolClaudeScore("warm", 0.3, 150*time.Hour), crossPoolClaudeScore("spare", 0.2, 150*time.Hour),
	}, box.pool)
	laptop.upstream.answer = rejectExhausted("warm")
	if _, err := laptop.store.Put("claude", "s1", "warm", ""); err != nil {
		t.Fatal(err)
	}
	markIdle(laptop.active, "s1", 2*time.Minute)

	status, servedBy := laptop.turn(t, "s1")
	if status != http.StatusOK || servedBy != "spare" {
		t.Fatalf("status %d served by %q; after the bound local failover serves spare", status, servedBy)
	}
	if got := targetsOf(box.seen()); len(got) != remotePoolMaxTargetRefusals {
		t.Fatalf("box targets %v; want exactly %d attempts (the bound)", got, remotePoolMaxTargetRefusals)
	}
}

// M25 -- mutation: an unreachable peer excludes only the account it was
// asked for, not the peer. A peer whose usage body answers but whose turns
// never connect holds two eligible accounts that outrank local: after the
// first attempt fails at the network, the second account on the same peer is
// not tried; the turn is served locally.
func TestAnUnreachablePeerIsExcludedAsAWhole(t *testing.T) {
	var turns atomic.Int32
	body := newVerdictPeer(t, accounts.ProviderClaude,
		verdictPeerAccount{Score: crossPoolClaudeScore("u1", 1, 30*time.Hour)},
		verdictPeerAccount{Score: crossPoolClaudeScore("u2", 0.95, 30*time.Hour)}).usageBody()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == remotePoolUsagePath {
			_, _ = w.Write(body)
			return
		}
		turns.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(peer.Close)
	laptop := newCrossPoolLaptop(t, []selectacct.Score{crossPoolClaudeScore("loc", 0.2, 150*time.Hour)}, NewRemotePool(peer.URL, "", ""))
	status, servedBy := laptop.turn(t, "s1")
	if status != http.StatusOK || servedBy != "loc" {
		t.Fatalf("status %d served by %q; want local once the peer proved unreachable", status, servedBy)
	}
	if got := turns.Load(); got != 1 {
		t.Fatalf("the unreachable peer was asked %d times; one failed connection excludes it for the turn", got)
	}
}

// M18 -- mutation: forget a refusal after the turn (refusedFor always false).
// A refused account stays out of every ranking until the peer's next usage
// body: the next new session is not sent to it on the same body, and once a
// newer body arrives it competes again.
func TestARefusalIsRememberedUntilTheNextBody(t *testing.T) {
	box := newContractBox(t, boxAccount{Score: crossPoolClaudeScore("y", 1, 30*time.Hour)})
	box.refuse["y"] = selectacct.VerdictAtCap
	laptop := newCrossPoolLaptop(t, []selectacct.Score{crossPoolClaudeScore("loc", 0.2, 150*time.Hour)}, box.pool)

	for _, sessionID := range []string{"s1", "s2"} {
		if status, servedBy := laptop.turn(t, sessionID); status != http.StatusOK || servedBy != "loc" {
			t.Fatalf("%s: status %d served by %q; want loc", sessionID, status, servedBy)
		}
	}
	if got := targetsOf(box.seen()); len(got) != 1 || got[0] != "y" {
		t.Fatalf("box targets %v; y refused s1 and must not be asked again on the same body", got)
	}
	e2eRefetch(t, box.pool)
	laptop.turn(t, "s3")
	if got := targetsOf(box.seen()); len(got) != 2 || got[1] != "y" {
		t.Fatalf("box targets %v; a newer body lifts the refusal and y competes again", got)
	}
}

// M22 -- mutation: rank a peer account on its top-level verdict whatever the
// request's quota pool. p's account-wide verdict is eligible but its Fable
// pool is spent: a Fable session is not sent there (the local Fable pool is
// thinner than p's account-wide numbers, so read at the top level p would
// win); read per pool, p is not a candidate.
func TestTheDeciderReadsThePoolVerdict(t *testing.T) {
	peer := opusPeer(t)
	server, _ := opusCrossPoolLaptop(t, peer.pool(), 0.5, false)
	status, body := postFableTurnBody(t, server.Handler(), "s1")
	if served := servedByOf(body); status != http.StatusOK || (served != "one" && served != "two") {
		t.Fatalf("status %d served by %q; p's Fable pool is spent by its own verdict", status, served)
	}
	if turns := peer.seen(); len(turns) != 0 {
		t.Fatalf("peer turns %+v; a Fable session must not be targeted at a spent Fable pool", turns)
	}
}
