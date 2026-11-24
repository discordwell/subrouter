package proxy

// Review fix-up (2026-09-28): a FORCED switch inside local failover, and a
// transient 5xx on a held targeted turn. Real laptop against real box
// (crosspool_e2e_test.go helpers), PATCHES §31.

import (
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
)

// A forced switch inside local failover (fourteen's window is spent) is ranked
// over the local failover candidate AND every peer (P8). When a peer account
// wins and then refuses (at its cap at request time), the recompute is the
// SAME ranking with the local failover candidate still in it (P6: "any nope,
// recompute overall"): nothing else on the box is eligible, so local fifteen
// serves. The box is never sent an untargeted forward -- the last resort is
// only for a turn local cannot serve (Deviation X2) -- and the session is not
// held on the box's refused account.
func TestCrossPoolForcedFailoverRefusalRecomputesOverLocalToo(t *testing.T) {
	now := time.Now()
	box := newE2EBox(t, map[string]string{
		"a-burn": e2eUsage(now, 10, 150*time.Minute, 96, 16*time.Hour),
		"chip-1": e2eUsage(now, 100, 2*time.Hour, 50, 100*time.Hour), // 5h spent: never eligible
		"chip-3": e2eUsage(now, 80, 4*time.Hour, 24, 120*time.Hour),  // burn projection: ineligible
	})
	laptop := newE2ELaptop(t, map[string]string{
		"fourteen": e2eUsage(now, 0, 4*time.Hour, 0, 120*time.Hour),
		"fifteen":  e2eUsage(now, 0, 4*time.Hour, 50, 150*time.Hour),
	}, box.newPool())
	pool := laptop.server.RemotePool

	// Turn 1 lands local: a-burn is at its cap when the laptop looks.
	endBusy := box.live(t, "a-burn")
	e2ePrime(t, pool)
	box.calibrate(t, map[string]string{"a-burn": selectacct.VerdictAtCap})
	if got := laptop.turn(t, "s1"); got.ServedBy != "fourteen" {
		t.Fatalf("turn 1 served by %q; want fourteen", got.ServedBy)
	}
	// The laptop's cached body now says a-burn is burndown-eligible...
	endBusy()
	e2eRefetch(t, pool)
	box.calibrate(t, map[string]string{"a-burn": selectacct.VerdictBurndown, "chip-3": selectacct.VerdictBurnProjection})
	// ...but a box session starts there before the laptop's turn arrives.
	defer box.live(t, "a-burn")()
	box.calibrate(t, map[string]string{"a-burn": selectacct.VerdictAtCap})

	laptop.upstream.setAnswer(func(account string, w http.ResponseWriter) bool {
		if account == "fourteen" {
			e2eRejected(w)
			return true
		}
		return false
	})
	got := laptop.turn(t, "s1")
	turns := box.seen()
	if len(turns) != 1 || turns[0].Target != "a-burn" || turns[0].Refused != "at_cap" {
		t.Fatalf("box turns %+v; want exactly one: a-burn targeted and refused at_cap", turns)
	}
	for _, turn := range turns {
		if turn.Target == "" {
			t.Errorf("box was sent an UNTARGETED forward (served on %q) although local fifteen could serve", turn.Account)
		}
	}
	if got.Status != http.StatusOK || got.ServedBy != "fifteen" {
		t.Fatalf("turn 2: status %d served by %q; after a-burn refused, the recompute over local fifteen + the box must land on fifteen", got.Status, got.ServedBy)
	}
	if account, held := e2eHold(pool, "s1"); held {
		t.Fatalf("s1 is held on the box's %q; it was served locally", account)
	}
	if lines := e2eLogLines(laptop.logs, "remote pool last resort; forwarding untargeted"); len(lines) != 0 {
		t.Fatalf("laptop took the untargeted last resort %d times with local able to serve: %v", len(lines), lines)
	}
	// Turn 3 stays on fifteen, where the forced switch landed and stuck (P5).
	laptop.upstream.setAnswer(nil)
	if third := laptop.turn(t, "s1"); third.ServedBy != "fifteen" {
		t.Fatalf("turn 3 served by %q; the forced switch landed on fifteen and sticks", third.ServedBy)
	}
	if n := len(box.seen()); n != 1 {
		t.Fatalf("box saw %d turns; want 1 (the refusal only)", n)
	}
}

// The recompute after a refusal inside local failover keeps the local failover
// candidate in the ranking, so a LOWER peer account never beats it: with a-burn
// refused, local fifteen (50% of the week over 6.25 days, x2 = 112 on the
// owner's scale) outranks the box's b7d (100% over ~7 days = 100).
func TestCrossPoolForcedFailoverRefusalDoesNotPreferALowerPeerOverLocal(t *testing.T) {
	now := time.Now()
	box := newE2EBox(t, map[string]string{
		"a-burn": e2eUsage(now, 10, 150*time.Minute, 96, 16*time.Hour),
		"b7d":    e2eUsage(now, 0, 4*time.Hour, 0, 167*time.Hour),
	})
	laptop := newE2ELaptop(t, map[string]string{
		"fourteen": e2eUsage(now, 0, 4*time.Hour, 0, 120*time.Hour),
		"fifteen":  e2eUsage(now, 0, 4*time.Hour, 50, 150*time.Hour),
	}, box.newPool())
	pool := laptop.server.RemotePool

	endBusy := box.live(t, "a-burn")
	e2ePrime(t, pool)
	if got := laptop.turn(t, "s1"); got.ServedBy != "fourteen" {
		t.Fatalf("turn 1 served by %q; want fourteen", got.ServedBy)
	}
	endBusy()
	e2eRefetch(t, pool)
	box.calibrate(t, map[string]string{"a-burn": selectacct.VerdictBurndown, "b7d": selectacct.VerdictEligible})
	defer box.live(t, "a-burn")()

	// Calibration, on the one ranking: with a-burn out, fifteen outranks b7d.
	request, _ := http.NewRequest(http.MethodPost, "http://laptop/v1/messages", nil)
	ranking := laptop.server.newCrossPoolRanking(request, accounts.ProviderClaude, "", newRemotePoolTurn(request.Context()))
	ranking.turn.failed[remotePoolTargetKey{pool.name(), "a-burn"}] = "at_cap"
	fifteen, ok := findAccount(filterAccountsForProvider(laptop.server.accountListContext(t.Context()), "claude"), "fifteen")
	if !ok {
		t.Fatal("no fifteen")
	}
	placement, err := ranking.rank(laptop.server.selectionScheduler("", ""), []accounts.Account{fifteen}, "")
	if err != nil || !placement.Local || placement.Account.ID != "fifteen" || placement.BestPeer == nil || placement.BestPeer.Account != "b7d" {
		t.Fatalf("calibration: placement %+v err %v; want local fifteen over b7d", placement, err)
	}

	laptop.upstream.setAnswer(func(account string, w http.ResponseWriter) bool {
		if account == "fourteen" {
			e2eRejected(w)
			return true
		}
		return false
	})
	got := laptop.turn(t, "s1")
	turns := box.seen()
	if got.Status != http.StatusOK || got.ServedBy != "fifteen" {
		t.Fatalf("turn 2: status %d served by %q, box turns %+v; after a-burn refused, local fifteen outranks b7d and must serve", got.Status, got.ServedBy, turns)
	}
	if len(turns) != 1 || turns[0].Target != "a-burn" {
		t.Fatalf("box turns %+v; want only the refused a-burn attempt", turns)
	}
}

// A forced switch inside local failover where the local failover account
// outranks every eligible peer account stays on local failover: the box is
// never asked (P2/P8; the ranking includes next, not the peers alone).
func TestCrossPoolForcedSwitchStaysLocalWhenTheLocalFailoverOutranksEveryPeer(t *testing.T) {
	now := time.Now()
	box := newE2EBox(t, map[string]string{"b5d": e2eUsage(now, 0, 4*time.Hour, 0, 110*time.Hour)})
	laptop := newE2ELaptop(t, map[string]string{
		"fourteen": e2eUsage(now, 0, 4*time.Hour, 0, 120*time.Hour),
		"fifteen":  e2eUsage(now, 0, 4*time.Hour, 0, 125*time.Hour),
	}, box.newPool())
	e2ePrime(t, laptop.server.RemotePool)
	first := laptop.turn(t, "s1").ServedBy
	if first != "fourteen" && first != "fifteen" {
		t.Fatalf("calibration: turn 1 served by %q; want a local account", first)
	}
	other := map[string]string{"fourteen": "fifteen", "fifteen": "fourteen"}[first]
	laptop.upstream.setAnswer(func(account string, w http.ResponseWriter) bool {
		if account == first {
			e2eRejected(w)
			return true
		}
		return false
	})
	got := laptop.turn(t, "s1")
	if got.Status != http.StatusOK || got.ServedBy != other {
		t.Fatalf("turn 2: status %d served by %q; forced off %s, the local %s (x2) outranks the box's b5d", got.Status, got.ServedBy, first, other)
	}
	if turns := box.seen(); len(turns) != 0 {
		t.Fatalf("box turns %+v; the box's account ranks below the local failover", turns)
	}
}

// One upstream 503 on a HELD, targeted turn is transient (P8): an
// Anthropic-side server failure is not the account running out. The box
// retries it on the SAME account (never another: the target is hard), the hold
// on chip-5 survives, and nothing is excluded or moved.
func TestCrossPoolUpstream503OnHeldTurnKeepsTheHold(t *testing.T) {
	box := e2eIncidentBox(t)
	laptop := newE2ELaptop(t, e2eIncidentLocal(), box.newPool())
	e2ePrime(t, laptop.server.RemotePool)
	if got := laptop.turn(t, "s1"); got.ServedBy != "chip-5" {
		t.Fatalf("turn 1 served by %q; want chip-5", got.ServedBy)
	}
	var once sync.Once
	box.upstream.setAnswer(func(account string, w http.ResponseWriter) bool {
		answered := false
		once.Do(func() {
			answered = true
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"unavailable"}}`)
		})
		return answered
	})
	got := laptop.turn(t, "s1")
	account, held := e2eHold(laptop.server.RemotePool, "s1")
	if got.Status != http.StatusOK || got.ServedBy != "chip-5" {
		t.Fatalf("turn 2: status %d served by %q, box turns %+v; the box retries a 503 on the target", got.Status, got.ServedBy, box.seen())
	}
	if !held || account != "chip-5" {
		t.Fatalf("one upstream 503 ended the hold on chip-5 (now %q/%v): a transient blip was classified forced", account, held)
	}
	if hits := box.upstream.accounts(); len(hits) != 3 {
		t.Fatalf("box upstream hits %v; want chip-5 x3 (turn 1, the 503, its same-account retry)", hits)
	}
	for _, hit := range box.upstream.accounts() {
		if hit != "chip-5" {
			t.Fatalf("box upstream hits %v; a targeted retry never leaves the target", box.upstream.accounts())
		}
	}
	if third := laptop.turn(t, "s1"); third.ServedBy != "chip-5" {
		t.Fatalf("turn 3 served by %q; the hold survives a transient 503", third.ServedBy)
	}
}

// An upstream 503 that outlasts the box's same-account retries is stamped
// overload and passed through to the client -- never served elsewhere, never
// read as the box's pool being spent -- and the hold on chip-5 survives.
func TestCrossPoolPersistentUpstream503OnHeldTurnPassesThroughAndKeepsTheHold(t *testing.T) {
	box := e2eIncidentBox(t)
	laptop := newE2ELaptop(t, e2eIncidentLocal(), box.newPool())
	e2ePrime(t, laptop.server.RemotePool)
	if got := laptop.turn(t, "s1"); got.ServedBy != "chip-5" {
		t.Fatalf("turn 1 served by %q; want chip-5", got.ServedBy)
	}
	box.upstream.setAnswer(func(account string, w http.ResponseWriter) bool {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"unavailable"}}`)
		return true
	})
	got := laptop.turn(t, "s1")
	if got.Status != http.StatusServiceUnavailable {
		t.Fatalf("turn 2: status %d served by %q; a persistent provider 503 reaches the client", got.Status, got.ServedBy)
	}
	if hits := laptop.upstream.accounts(); len(hits) != 0 {
		t.Fatalf("laptop upstream hits %v; an overload is never served elsewhere", hits)
	}
	hits := box.upstream.accounts()
	if len(hits) != 1+1+providerOverloadMaxRetries {
		t.Fatalf("box upstream hits %v; want turn 1 plus the 503 and its %d same-account retries", hits, providerOverloadMaxRetries)
	}
	for _, hit := range hits {
		if hit != "chip-5" {
			t.Fatalf("box upstream hits %v; a targeted retry never leaves the target", hits)
		}
	}
	if account, held := e2eHold(laptop.server.RemotePool, "s1"); !held || account != "chip-5" {
		t.Fatalf("hold after a persistent 503 = %q/%v; want chip-5 kept", account, held)
	}
	box.upstream.setAnswer(nil)
	if third := laptop.turn(t, "s1"); third.ServedBy != "chip-5" {
		t.Fatalf("turn 3 served by %q; want chip-5", third.ServedBy)
	}
}

// The classifier row and the real forward: a 5xx stamped overload is no switch
// at all (served through, the hold set), while an unstamped 503 is still a
// legacy peer's decline.
func TestPeerOverloadStampIsNotASwitch(t *testing.T) {
	overload := http.Header{}
	overload.Set(remotePoolTransientHeader, remotePoolTransientOverload)
	for _, status := range []int{http.StatusServiceUnavailable, 529, http.StatusBadGateway, http.StatusInternalServerError} {
		if got := classifyAccountSwitch(accountSwitchEvidence{PeerStatus: status, PeerHeader: overload}); got.Kind != switchNone || got.Reason != "overload" {
			t.Fatalf("status %d stamped overload = %s/%q; want none/overload", status, got.Kind, got.Reason)
		}
	}
	_, server := newTestRemotePool(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(remotePoolTransientHeader, remotePoolTransientOverload)
		w.Header().Set(subrouterAccountHeader, "acct-a")
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	req := forwardRequest(t, http.MethodPost, "/v1/messages", []byte(`{}`))
	out := server.forwardToPeer(server.RemotePool, req, "claude", "s1", "/v1/messages", "", []byte(`{}`), remotePoolHoldSet, "acct-a")
	if !out.Served || out.Status != http.StatusServiceUnavailable || out.Switch.Kind != switchNone {
		t.Fatalf("outcome served=%v status %d %s/%q; want the 503 served through", out.Served, out.Status, out.Switch.Kind, out.Switch.Reason)
	}
	out.Resp.Body.Close()
	if account, held := e2eHold(server.RemotePool, "s1"); !held || account != "acct-a" {
		t.Fatalf("hold = %q/%v; want acct-a", account, held)
	}
}

// A forced switch that a refusal hands back to local failover leaves the
// give-up stage armed: when the local failover account is spent too, the turn
// is still offered to the box's best remaining eligible account (targeted),
// rather than the client getting the last local 429. The forced switch used to
// spend the give-up stage's once-per-turn guard.
func TestCrossPoolForcedSwitchBackToLocalLeavesTheGiveUpStageArmed(t *testing.T) {
	now := time.Now()
	box := newE2EBox(t, map[string]string{
		"a-burn": e2eUsage(now, 10, 150*time.Minute, 96, 16*time.Hour),
		"b7d":    e2eUsage(now, 0, 4*time.Hour, 0, 167*time.Hour),
	})
	laptop := newE2ELaptop(t, map[string]string{
		"fourteen": e2eUsage(now, 0, 4*time.Hour, 0, 120*time.Hour),
		"fifteen":  e2eUsage(now, 0, 4*time.Hour, 50, 150*time.Hour),
	}, box.newPool())
	pool := laptop.server.RemotePool

	endBusy := box.live(t, "a-burn")
	e2ePrime(t, pool)
	if got := laptop.turn(t, "s1"); got.ServedBy != "fourteen" {
		t.Fatalf("turn 1 served by %q; want fourteen", got.ServedBy)
	}
	endBusy()
	e2eRefetch(t, pool)
	box.calibrate(t, map[string]string{"a-burn": selectacct.VerdictBurndown, "b7d": selectacct.VerdictEligible})
	defer box.live(t, "a-burn")()

	// Both local accounts are spent: fourteen forces the switch, a-burn wins
	// and refuses, fifteen (x2) outranks b7d and is tried, and is spent too.
	laptop.upstream.setAnswer(func(account string, w http.ResponseWriter) bool {
		e2eRejected(w)
		return true
	})
	got := laptop.turn(t, "s1")
	turns := box.seen()
	if got.Status != http.StatusOK || got.ServedBy != "b7d" {
		t.Fatalf("turn 2: status %d served by %q, box turns %+v; local ran out, so the box's b7d serves", got.Status, got.ServedBy, turns)
	}
	if len(turns) != 2 || turns[0].Target != "a-burn" || turns[1].Target != "b7d" {
		t.Fatalf("box turns %+v; want a-burn refused, then b7d targeted", turns)
	}
	if hits := laptop.upstream.accounts(); len(hits) < 3 || hits[len(hits)-1] != "fifteen" {
		t.Fatalf("laptop upstream hits %v; fifteen is tried before the box's lower b7d", hits)
	}
}
