package proxy

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// PATCHES §31, R3 fix 5 (F4): the decision line says which comparator clause
// decided and who came second, and a forced switch's line reports the local
// failover candidate as measured.

// A forced switch inside local failover (warm's window spent mid-turn): the
// line names spare, the local candidate, with the numbers the ranking scored
// it on -- not a zero value that reads local_known=false -- and says which
// clause put acct-a first and that spare was the runner-up.
func TestForcedSwitchDecisionLineNamesTheClauseTheRunnerUpAndKnowsLocal(t *testing.T) {
	peer := crossPoolClaudeScore("acct-a", 1, 30*time.Hour)
	box := newContractBox(t, boxAccount{Score: peer})
	laptop := newCrossPoolLaptop(t, []selectacct.Score{
		crossPoolClaudeScore("warm", 0.3, 150*time.Hour), crossPoolClaudeScore("spare", 0.2, 150*time.Hour),
	}, box.pool)
	laptop.upstream.answer = rejectExhausted("warm")
	if _, err := laptop.store.Put("claude", "s1", "warm", ""); err != nil {
		t.Fatal(err)
	}
	markIdle(laptop.active, "s1", 2*time.Minute)
	if status, servedBy := laptop.turn(t, "s1"); status != http.StatusOK || servedBy != "acct-a" {
		t.Fatalf("calibration: status %d served by %q; want acct-a", status, servedBy)
	}
	var line map[string]any
	for _, d := range laptop.logLines("remote pool decision") {
		if d["session_state"] == "forced" && d["target_account"] == "acct-a" {
			line = d
		}
	}
	if line == nil {
		t.Fatalf("no forced decision line targeting acct-a: %v", laptop.logLines("remote pool decision"))
	}
	if line["account"] != "spare" || line["local_known"] != true || line["local_headroom"] == float64(0) {
		t.Fatalf("forced line reports local %v known=%v headroom=%v; want spare, measured", line["account"], line["local_known"], line["local_headroom"])
	}
	clause, _ := line["clause"].(string)
	if clause == "" || line["runner_up"] != "spare" {
		t.Fatalf("forced line clause %q runner_up %v; want the deciding clause and spare", clause, line["runner_up"])
	}
	if reason, _ := line["reason"].(string); !strings.Contains(reason, "("+clause+"; runner-up spare)") {
		t.Fatalf("reason %q does not name the clause and the runner-up", reason)
	}
}

// Local wins: the runner-up is the peer's account, named with its peer.
func TestLocalWinDecisionLineNamesThePeerRunnerUp(t *testing.T) {
	box := newContractBox(t, boxAccount{Score: crossPoolClaudeScore("acct-a", 0.5, 140*time.Hour)})
	laptop := newCrossPoolLaptop(t, []selectacct.Score{crossPoolClaudeScore("solo", 0.9, 100*time.Hour)}, box.pool)
	if status, servedBy := laptop.turn(t, "s1"); status != http.StatusOK || servedBy != "solo" {
		t.Fatalf("calibration: status %d served by %q; want solo", status, servedBy)
	}
	decisions := laptop.logLines("remote pool decision")
	if len(decisions) != 1 {
		t.Fatalf("decisions %v; want one", decisions)
	}
	want := box.pool.name() + "/acct-a"
	clause, _ := decisions[0]["clause"].(string)
	if decisions[0]["runner_up"] != want || clause == "" {
		t.Fatalf("runner_up %v clause %q; want %s and the deciding clause", decisions[0]["runner_up"], clause, want)
	}
	if reason, _ := decisions[0]["reason"].(string); !strings.HasPrefix(reason, "local still outranks every eligible peer account (") || !strings.Contains(reason, want) {
		t.Fatalf("reason %q; want the old wording, then the clause and %s", reason, want)
	}
}

// The pool board's spill row runs the one ranking over every peer: it names
// the account a new session would be targeted at, reads eligibility off the
// peer's verdicts (the incident box's chip-5 at its cap is not offered), and
// reports the same clause and runner-up the decision line would.
func TestPoolBoardSpillIsTheOneRanking(t *testing.T) {
	t.Run("an eligible peer account outranks local", func(t *testing.T) {
		box := incidentBox(t, 0)
		laptop := newCrossPoolLaptop(t, incidentLocal(), box.pool)
		board := laptop.server.poolBoard(context.Background(), "", "")
		if len(board.Spill) != 1 {
			t.Fatalf("spill rows %+v; want one (claude)", board.Spill)
		}
		spill := board.Spill[0]
		if !spill.PreferPeer || spill.TargetAccount != "chip-5" || spill.TargetPeer != box.pool.name() {
			t.Fatalf("spill %+v; want chip-5 on %s targeted (burndown outranks the roomy local pool)", spill, box.pool.name())
		}
		if !strings.HasPrefix(spill.Reason, "peer outranks local") || spill.Clause == "" || spill.RunnerUp != spill.LocalBest || !spill.LocalKnown {
			t.Fatalf("spill %+v; want the peer-wins reason, the clause, the best local account as runner-up, measured", spill)
		}
	})
	t.Run("an account at its cap is not offered", func(t *testing.T) {
		box := incidentBox(t, 1)
		laptop := newCrossPoolLaptop(t, incidentLocal(), box.pool)
		board := laptop.server.poolBoard(context.Background(), "", "")
		if len(board.Spill) != 1 {
			t.Fatalf("spill rows %+v; want one (claude)", board.Spill)
		}
		if spill := board.Spill[0]; spill.PreferPeer || spill.TargetAccount != "" || spill.LocalBest == "" {
			t.Fatalf("spill %+v; nothing on the box is eligible by its own verdicts", spill)
		}
	})
}

// PATCHES §34: a warm turn whose account's credential read got no answer is
// served by the peer for that one turn (errRefreshTransient), and the session
// keeps its account. The decision line used to carry an empty local --
// account "", local_known=false -- and no cause: on the laptop, 2026-09-28,
// warm turns on accounts measured at 0.51 and 0.56 went to chip-5 that way,
// and nothing in the log said a keychain read had run out its clock. The line
// now names the account whose refresh reached no verdict, as measured, and
// the refresh error; a request-path line says the same.
func TestATransientRefreshNamesTheAccountAndTheCauseOnTheDecisionLine(t *testing.T) {
	box := newContractBox(t, boxAccount{Score: crossPoolClaudeScore("acct-a", 0.6, 140*time.Hour)})
	laptop := newCrossPoolLaptop(t, []selectacct.Score{crossPoolClaudeScore("warm", 0.51, 100*time.Hour)}, box.pool)
	laptop.server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		return account, fmt.Errorf("%w: read Claude credential from keychain: security find-generic-password did not answer within 5s (signal: killed)",
			agentclaude.ErrCredentialUnchecked)
	}
	if _, err := laptop.store.Put("claude", "s1", "warm", ""); err != nil {
		t.Fatal(err)
	}
	markIdle(laptop.active, "s1", 2*time.Minute)
	if status, servedBy := laptop.turn(t, "s1"); status != http.StatusOK || servedBy != "acct-a" {
		t.Fatalf("calibration: status %d served by %q; want the peer's acct-a for this one turn", status, servedBy)
	}
	if pin, _ := laptop.store.Get("claude", "s1"); pin.AccountID != "warm" {
		t.Fatalf("pin %q; a refresh with no verdict keeps the session's account", pin.AccountID)
	}
	var line map[string]any
	for _, d := range laptop.logLines("remote pool decision") {
		if reason, _ := d["reason"].(string); strings.HasPrefix(reason, "local cannot serve") {
			line = d
		}
	}
	if line == nil {
		t.Fatalf("no local-cannot-serve decision line: %v", laptop.logLines("remote pool decision"))
	}
	cause, _ := line["local_cause"].(string)
	if line["account"] != "warm" || line["local_known"] != true || line["local_headroom"] == float64(0) ||
		!strings.Contains(cause, "did not answer") {
		t.Fatalf("decision line account=%v local_known=%v local_headroom=%v local_cause=%q; want warm, measured, and the refresh error",
			line["account"], line["local_known"], line["local_headroom"], cause)
	}
	warned := false
	for _, w := range laptop.logLines("selected account refresh reached no verdict; this turn is not served on it") {
		if errText, _ := w["error"].(string); w["account"] == "warm" && w["session"] == "s1" && strings.Contains(errText, "did not answer") {
			warned = true
		}
	}
	if !warned {
		t.Fatal("the request path did not name the account and the refresh error")
	}
}

// The same turn when the session's own account was found dead first: its
// refresh answered invalid_grant, so it was marked and the refresh failed over
// to spare, whose own read then got no answer. spare is what the turn could
// not be served on, and the lines name it -- but they used to name it alone,
// as "the account the session keeps", while the session is still pinned to
// the dead warm, which its next turn moves off. warm is named beside it.
func TestATransientRefreshAfterAFailoverNamesTheDeadAccountToo(t *testing.T) {
	box := newContractBox(t, boxAccount{Score: crossPoolClaudeScore("acct-a", 0.6, 140*time.Hour)})
	laptop := newCrossPoolLaptop(t, []selectacct.Score{
		crossPoolClaudeScore("warm", 0.51, 100*time.Hour), crossPoolClaudeScore("spare", 0.4, 100*time.Hour),
	}, box.pool)
	laptop.server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		if account.ID == "warm" {
			return account, fmt.Errorf("Claude OAuth refresh failed: 400 Bad Request: {\"error\":\"invalid_grant\"}")
		}
		return account, fmt.Errorf("%w: read Claude credential from keychain: security find-generic-password did not answer within 5s (signal: killed)",
			agentclaude.ErrCredentialUnchecked)
	}
	if _, err := laptop.store.Put("claude", "s1", "warm", ""); err != nil {
		t.Fatal(err)
	}
	markIdle(laptop.active, "s1", 2*time.Minute)
	if status, servedBy := laptop.turn(t, "s1"); status != http.StatusOK || servedBy != "acct-a" {
		t.Fatalf("calibration: status %d served by %q; want the peer's acct-a for this one turn", status, servedBy)
	}
	var line map[string]any
	for _, d := range laptop.logLines("remote pool decision") {
		if reason, _ := d["reason"].(string); strings.HasPrefix(reason, "local cannot serve") {
			line = d
		}
	}
	if line == nil {
		t.Fatalf("no local-cannot-serve decision line: %v", laptop.logLines("remote pool decision"))
	}
	if line["account"] != "spare" || line["local_failed_over_from"] != "warm" {
		t.Fatalf("decision line account=%v local_failed_over_from=%v; want spare, failed over from warm",
			line["account"], line["local_failed_over_from"])
	}
	warned := false
	for _, w := range laptop.logLines("selected account refresh reached no verdict; this turn is not served on it") {
		if w["account"] == "spare" && w["failed_over_from"] == "warm" && w["session_pinned"] == true {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("the request path did not name spare, the dead warm, and the pin: %v",
			laptop.logLines("selected account refresh reached no verdict; this turn is not served on it"))
	}
}
