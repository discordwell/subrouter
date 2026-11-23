package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// PATCHES §102.1: from the moment the provider says a claim used a saved
// reset, the account's row and ranking say what the daemon knows from the
// claim itself, before any reading: the block with the reset counted as
// used, no hold from it, and no figure measured before the claim for a meter
// it gave back. Every provider here is a fake that answers the reads after a
// claim as the usage endpoint answered on the live pool; no test sends a
// request to a real provider.

// claimRow is one usage-status row as a reader decodes it.
type claimRow struct {
	ID                 string                           `json:"id"`
	Windows            []accounts.UsageWindow           `json:"windows"`
	ComplimentaryReset *accounts.ComplimentaryResetInfo `json:"complimentary_reset"`
	Resets             []accounts.HeldReset             `json:"resets"`
	ResetsFromClaim    bool                             `json:"resets_from_claim"`
	NotReadSinceClaim  []string                         `json:"not_read_since_claim"`
	Normalized         struct {
		HeldWeek *float64 `json:"seconds_to_held_week_reset_expiry"`
	} `json:"normalized"`
}

func (r claimRow) window(name string) (accounts.UsageWindow, bool) {
	for _, window := range r.Windows {
		if window.Name == name && window.ExtraUsage == nil {
			return window, true
		}
	}
	return accounts.UsageWindow{}, false
}

// claimRows is usage-status, a sweep included, as a peer or `sr status`
// reads it.
func claimRows(t *testing.T, server Server) map[string]claimRow {
	t.Helper()
	rec := httptest.NewRecorder()
	server.handleUsageStatus(rec, loopbackRequest(http.MethodGet, "/_subrouter/usage-status", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("usage-status = %d: %s", rec.Code, rec.Body.String())
	}
	var rows []claimRow
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	out := map[string]claimRow{}
	for _, row := range rows {
		out[row.ID] = row
	}
	return out
}

// wantBlockFromClaim checks a row that carries the block a week claim left:
// one reset fewer, the Fable reset alone, no week hold, marked as from the
// claim, and the resets list built from it with the same mark.
func wantBlockFromClaim(t *testing.T, what string, row claimRow) {
	t.Helper()
	reset := row.ComplimentaryReset
	if reset == nil || !reset.Known || reset.Remaining == nil || *reset.Remaining != 1 ||
		!maps.Equal(reset.RemainingByKind, map[string]int{"fable": 1}) || reset.WeekHeldUntil != "" || !row.ResetsFromClaim {
		t.Fatalf("%s: the row carries %+v (from the claim: %v); want one reset left, the Fable one, no week hold, marked as from the claim",
			what, reset, row.ResetsFromClaim)
	}
	if len(row.Resets) != 1 || row.Resets[0].Kind != accounts.ResetKind("fable") || row.Resets[0].Count != 1 {
		t.Fatalf("%s: the row lists %+v, want the Fable reset alone", what, row.Resets)
	}
}

// 1. Claude, a week reset claimed at the wall in real mode, the read after
// it refused: at once the row carries the block with the week reset used, and
// its resets list the Fable reset, both marked as from the claim.
func TestAClaimedWeekResetLeavesItsBlockOnTheRow(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	f := newClaudeClaimFake(t, heldWeek())
	d := f.daemon(t, "")
	answerReadsAfterClaim(d, f, "held", readAfterRefused)
	if before := claimRows(t, d.server)["held"]; before.ComplimentaryReset == nil || *before.ComplimentaryReset.Remaining != 2 || before.ResetsFromClaim {
		t.Fatalf("calibration: before the claim held's row carries %+v", before.ComplimentaryReset)
	}
	d.pinSession("conversation", "held")
	f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
	if answer := d.turn("conversation", opusModel); !strings.Contains(answer.body, "msg_held") {
		t.Fatalf("calibration: the turn got %+v, want it served on held after one claim", answer)
	}
	wantBlockFromClaim(t, "after the claim", claimRows(t, d.server)["held"])
	if spare := claimRows(t, d.server)["spare"]; spare.ResetsFromClaim || spare.NotReadSinceClaim != nil {
		t.Fatalf("spare made no claim, yet its row says %+v", spare)
	}
}

// 2. The same for a claim by hand through the daemon's claim route, and for
// a Codex redeem by hand: the count one lower, the credit redeemed gone from
// the list, marked as from the claim.
func TestAClaimByHandAndACodexRedeemLeaveTheirBlockOnTheRow(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		setAutoResetForTest(t, AutoResetShadow, true)
		f := newClaudeClaimFake(t, heldWeek())
		d := f.daemon(t, "")
		answerReadsAfterClaim(d, f, "held", readAfterRefused)
		f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
		code, response := postClaudeReset(t, d.server.Handler(), `{"account":"held","dry_run":false,"confirm_account":"held"}`)
		if code != http.StatusOK || response.Result != "reset" {
			t.Fatalf("calibration: the claim by hand answered %d %+v", code, response)
		}
		wantBlockFromClaim(t, "after the claim by hand", claimRows(t, d.server)["held"])
	})
	t.Run("codex", func(t *testing.T) {
		setAutoResetForTest(t, AutoResetShadow, true)
		credits := heldCodexCredits("credit-soon", "credit-late")
		late := credits[1].ExpiresAt
		f := newCodexClaimFake(t, map[string]*codexClaimAccount{
			heldCodex:  {used: 100, credits: credits},
			spareCodex: {used: 30},
		})
		memory := f.server.AccountRef.sharedHeldResets()
		f.server.readCodexResetExpiries(context.Background(), memory, time.Now())
		memory.reads.Wait()
		if before := claimRows(t, f.server)[heldCodex]; len(before.Resets) != 2 || before.Resets[0].Expiry != accounts.ResetExpiryRead {
			t.Fatalf("calibration: before the redeem held lists %+v, want both credits with their dates", before.Resets)
		}
		f.server.AccountRef.client = &http.Client{Transport: codexReadAfter{f: f, account: heldCodex}}
		recorder := httptest.NewRecorder()
		f.server.Handler().ServeHTTP(recorder, loopbackAdminRequest(http.MethodPost, "/_subrouter/rate-limit-reset?email="+heldCodex+"&anytime=true", nil))
		if consumes, _ := f.counts(); recorder.Code != http.StatusOK || consumes != 1 {
			t.Fatalf("calibration: the redeem by hand answered %d %s with %d consumes", recorder.Code, recorder.Body.String(), consumes)
		}
		row := claimRows(t, f.server)[heldCodex]
		if row.ComplimentaryReset == nil || row.ComplimentaryReset.Remaining == nil || *row.ComplimentaryReset.Remaining != 1 || !row.ResetsFromClaim {
			t.Fatalf("after the redeem held's row carries %+v (from the claim: %v), want one credit left, marked", row.ComplimentaryReset, row.ResetsFromClaim)
		}
		if len(row.Resets) != 1 || row.Resets[0].Count != 1 || row.Resets[0].ExpiresAt != late {
			t.Fatalf("after the redeem held lists %+v, want credit-late alone (expires %s)", row.Resets, late)
		}
	})
}

// 3. A reading of the block replaces the block a claim left: the mark goes
// and the row is the reading's. A reading that still lists the reset the
// claim used wins, and one Warn says so.
func TestAReadingOfTheBlockReplacesTheBlockAClaimLeft(t *testing.T) {
	const disagree = "saved resets: the first reading after a claim still lists the reset the claim used; the reading stands"
	for _, c := range []struct {
		name string
		// keep: Anthropic answers reset and keeps listing the grant.
		keep bool
		want map[string]int
	}{
		{"the reading agrees", false, map[string]int{"fable": 1}},
		{"the reading still lists the reset", true, map[string]int{"week": 1, "fable": 1}},
	} {
		t.Run(c.name, func(t *testing.T) {
			setAutoResetForTest(t, AutoResetShadow, true)
			f := newClaudeClaimFake(t, heldWeek())
			if c.keep {
				f.answer = func() (int, string) {
					return http.StatusOK, `{"result":"reset","resets_left":0,"cleared":["seven_day","seven_day_overage_included"]}`
				}
			}
			d := f.daemon(t, "")
			reads := answerReadsAfterClaim(d, f, "held", readAfterRefused)
			f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
			if code, response := postClaudeReset(t, d.server.Handler(), `{"account":"held","dry_run":false,"confirm_account":"held"}`); code != http.StatusOK || response.Result != "reset" {
				t.Fatalf("calibration: the claim answered %d %+v", code, response)
			}
			wantBlockFromClaim(t, "before a reading", claimRows(t, d.server)["held"])
			reads.set(readAfterServed)
			pastRetryDeadline()
			d.measure()
			row := claimRows(t, d.server)["held"]
			if row.ResetsFromClaim || row.ComplimentaryReset == nil || !maps.Equal(row.ComplimentaryReset.RemainingByKind, c.want) {
				t.Fatalf("after a reading the row carries %+v (from the claim: %v), want the reading's %v unmarked",
					row.ComplimentaryReset, row.ResetsFromClaim, c.want)
			}
			d.measure()
			claimRows(t, d.server)
			lines := jsonLogLines(d.logs, disagree)
			if !c.keep {
				if len(lines) != 0 {
					t.Fatalf("a reading that agrees logged %v", lines)
				}
				return
			}
			if len(lines) != 1 || lines[0]["level"] != "WARN" || lines[0]["kind"] != "week" || lines[0]["left_read"] != 1.0 || lines[0]["left_after_claim"] != 0.0 {
				t.Fatalf("lines %v, want one Warn naming the week reset still listed", lines)
			}
		})
	}
}

// 4. Nothing claims on a block that is not a reading: with the Fable reset
// left in the block a week claim left, a Fable wall decides no claim, says
// the saved resets were last known from the claim, and sends nothing. After
// a reading of the block the same wall decides as §94 rules: here it claims
// the Fable reset.
func TestNoClaimGoesByTheBlockAClaimLeft(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	f := newClaudeClaimFake(t, heldWeek())
	d := f.daemon(t, "")
	reads := answerReadsAfterClaim(d, f, "held", readAfterRefused)
	d.pinSession("week", "held")
	f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
	if answer := d.turn("week", opusModel); !strings.Contains(answer.body, "msg_held") {
		t.Fatalf("calibration: the week wall's turn got %+v", answer)
	}
	fableWall := func(session string) claudeAnswer {
		f.set("held", func(p *claudeClaimProfile) { p.fable = 100 })
		d.clearMarks("held")
		d.pinSession(session, "held")
		return d.turn(session, "claude-fable-5-2")
	}
	fableWall("fable-before")
	decision := d.decision("held")
	if claims, _ := f.counts(); claims != 1 || decision.Verdict != AutoResetNoClaim ||
		!strings.Contains(decision.Reason, "its saved resets were last known from the claim at ") ||
		!strings.Contains(decision.Reason, ", not from a reading") {
		t.Fatalf("a Fable wall on the block from the claim: %d claims, decision %+v; want none, saying the resets are known from the claim", claims, decision)
	}
	reads.set(readAfterServed)
	pastRetryDeadline()
	d.measure()
	setAutoResetClock(d.server, later(11*time.Minute))
	if answer := fableWall("fable-after"); !strings.Contains(answer.body, "msg_held") {
		t.Fatalf("after a reading the Fable wall's turn got %+v, want a claim and the turn served on held", answer)
	}
	if claims, _ := f.counts(); claims != 2 || d.decision("held").Verdict != AutoResetClaimed || d.decision("held").Kind != "fable" {
		t.Fatalf("after a reading: %d claims, decision %+v; want the Fable reset claimed", claims, d.decision("held"))
	}
}

// 4, Codex. Nothing claims on the block a redeem left either, and for Codex
// that rule alone stops a claim on a row that still lists a credit. After a
// redeem by hand whose read is refused, the daemon's snapshot holds the row its
// last sweep read before the redeem, with two credits, as a redeem that leaves
// the snapshot in place does (the failover's). A wall on the account decides
// no claim, says its saved resets were last known from the claim, and sends
// no second consume.
func TestNoCodexClaimGoesByTheBlockARedeemLeft(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	f := newCodexClaimFake(t, map[string]*codexClaimAccount{
		heldCodex:  {used: 90, credits: heldCodexCredits("credit-soon", "credit-late")},
		spareCodex: {used: 30},
	})
	readBefore, ok := f.server.AccountRef.UsageStatusSnapshot()
	if !ok {
		t.Fatal("calibration: no sweep before the redeem")
	}
	reads := &codexFollowUpFake{f: f, refuse: true}
	f.server.AccountRef.client = &http.Client{Transport: reads}
	recorder := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(recorder, loopbackAdminRequest(http.MethodPost, "/_subrouter/rate-limit-reset?email="+heldCodex+"&anytime=true", nil))
	if consumes, _ := f.counts(); recorder.Code != http.StatusOK || consumes != 1 {
		t.Fatalf("calibration: the redeem by hand answered %d %s with %d consumes", recorder.Code, recorder.Body.String(), consumes)
	}
	reads.mu.Lock()
	reads.refuse = false
	reads.mu.Unlock()
	ref := f.server.AccountRef
	ref.usageStatusMu.Lock()
	ref.usageStatusCache, ref.usageStatusAt = readBefore, time.Now()
	ref.usageStatusMu.Unlock()
	f.pinSession("conversation", heldCodex)
	f.set(heldCodex, func(a *codexClaimAccount) { a.used = 100 })
	f.post("conversation")
	decision := f.decision(heldCodex)
	if consumes, _ := f.counts(); consumes != 1 || decision.Verdict != AutoResetNoClaim ||
		!strings.Contains(decision.Reason, "its saved resets were last known from the claim at ") ||
		!strings.Contains(decision.Reason, ", not from a reading") {
		t.Fatalf("a wall on the block the redeem left: %d consumes, decision %+v; want no second consume, saying the credits are known from the claim",
			consumes, decision)
	}
}

// 5. The hold ends with the reset: after its week reset is used by hand,
// with the block unread, the account ranks without the holder's 2x and is not
// in early burndown, and a peer reading its row sees no hold. A claim that
// did not land keeps the hold.
func TestTheHoldEndsWithTheWeekResetItWasFor(t *testing.T) {
	setResetHoldPriorityForTest(t, true)
	for _, c := range []struct {
		name   string
		answer string
		landed bool
	}{
		{"the claim landed", "", true},
		{"the claim did not land", `{"result":"not_limited"}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			setAutoResetForTest(t, AutoResetShadow, true)
			f := newClaudeClaimFake(t, map[string]*claudeClaimProfile{
				"a-plain":  {week: 80, fable: 20},
				"b-holder": {week: 80, fable: 20, grants: []bool{false, true}, usableEarly: true},
			})
			if c.answer != "" {
				f.answer = func() (int, string) { return http.StatusOK, c.answer }
			}
			d := f.daemon(t, "")
			answerReadsAfterClaim(d, f, "b-holder", readAfterRefused)
			now := time.Now()
			holder := func() selectacct.Score {
				score, _ := d.server.SchedulerRef.Get().LookupScore(accounts.ProviderClaude, "b-holder")
				return score
			}
			if !holder().HoldsReset(now) || holder().BurndownCause(now) != selectacct.BurndownCauseReset ||
				!strings.HasPrefix(explainClaude(t, d.server.SchedulerRef.Get(), "", "a-plain", "b-holder")[0].Reason, "early burndown") ||
				claimRows(t, d.server)["b-holder"].Normalized.HeldWeek == nil {
				t.Fatalf("calibration: before the claim b-holder holds %v, burndown %q, and its row publishes the hold %v",
					holder().ResetHeldUntil, holder().BurndownCause(now), claimRows(t, d.server)["b-holder"].Normalized.HeldWeek)
			}
			// A sweep that read the account before the claim and publishes
			// after it, as one in flight at the claim does.
			readBefore, _ := d.server.AccountRef.UsageStatusSnapshot()
			postClaudeReset(t, d.server.Handler(), `{"account":"b-holder","dry_run":false,"confirm_account":"b-holder"}`)
			explained := explainClaude(t, d.server.SchedulerRef.Get(), "", "a-plain", "b-holder")
			peer := claimRows(t, d.server)["b-holder"].Normalized.HeldWeek
			if !c.landed {
				if !holder().HoldsReset(now) || peer == nil {
					t.Fatalf("a claim that did not land: b-holder holds %v and publishes %v, want the hold kept", holder().ResetHeldUntil, peer)
				}
				return
			}
			for _, e := range explained {
				if e.Account.ID == "b-holder" && (strings.Contains(e.Reason, "reset-hold 2x") || strings.Contains(e.Reason, "early burndown")) {
					t.Fatalf("after its week reset was used b-holder ranks %q", e.Reason)
				}
			}
			if holder().HoldsReset(now) || holder().BurndownCause(now) == selectacct.BurndownCauseReset || peer != nil {
				t.Fatalf("after its week reset was used b-holder holds %v, burndown %q, and publishes %v; want no hold",
					holder().ResetHeldUntil, holder().BurndownCause(now), peer)
			}
			d.server.updateSchedulerFromUsageStatusesContext(context.Background(), readBefore)
			if holder().HoldsReset(now) || !holder().WeeklyResetAt.IsZero() {
				t.Fatalf("rows read before the claim, published after it, gave b-holder back its hold %v and week %v",
					holder().ResetHeldUntil, holder().WeeklyResetAt)
			}
		})
	}
}

// 6. §95's choice follows the same block: once the week reset is used, the
// pass after the claim does not choose the account for it, and weighs it for
// the Fable reset it has left. The pool burns its week so fast that a week
// reset is worth claiming at the account's next wall even on the fresh week
// the claim gave back, so only the block keeps the week reset from the choice.
func TestTheChoiceIsNotMadeForTheResetAClaimUsed(t *testing.T) {
	setResetLoadingForTest(t, true)
	setAutoResetForTest(t, AutoResetReal, true)
	f := newClaudeClaimFake(t, map[string]*claudeClaimProfile{
		"held":  {week: 60, fable: 20, grants: []bool{false, true}, usableEarly: true},
		"spare": {week: 40, fable: 10},
	})
	d := f.daemon(t, "")
	d.server.AccountRef.SetUsageHistory(claudeWeekHistory(t, map[string]float64{"held": 3, "spare": 3}))
	d.server.autoResetPass(context.Background())
	chosen := func() (selectacct.ResetChoice, bool) {
		d.server.AccountRef.sharedAutoResets().mu.Lock()
		defer d.server.AccountRef.sharedAutoResets().mu.Unlock()
		held, ok := d.server.AccountRef.sharedAutoResets().choices[autoResetKey(accounts.Account{ID: "held", Provider: accounts.ProviderClaude})]
		return held.choice, ok
	}
	if choice, ok := chosen(); !ok || choice.Meter != selectacct.ResetChoiceWeek {
		t.Fatalf("calibration: before the claim held is chosen %+v (%v), want for its week reset", choice, ok)
	}
	// The reads after the claim are taken, and leave the saved-reset block
	// out: the row's meters are fresh, and its block is the claim's.
	d.server.AccountRef.client = &http.Client{Transport: &leftOut{f: f, account: "held", key: "cedar_ember"}}
	if code, response := postClaudeReset(t, d.server.Handler(), `{"account":"held","dry_run":false,"confirm_account":"held"}`); code != http.StatusOK || response.Result != "reset" {
		t.Fatalf("calibration: the claim answered %d %+v", code, response)
	}
	d.measure()
	d.server.autoResetPass(context.Background())
	if row := claimRows(t, d.server)["held"]; !row.ResetsFromClaim {
		t.Fatalf("calibration: after the claim held's row is %+v, want the block from the claim", row.ComplimentaryReset)
	}
	if choice, ok := chosen(); ok && choice.Meter == selectacct.ResetChoiceWeek {
		t.Fatalf("the pass after the claim chose held %+v for the week reset it used", choice)
	}
	// Weighed for the reset the block says is next, the Fable reset; the
	// week reset it used is gone from it.
	stops := jsonLogLines(d.logs, resetUnchooseMessage)
	if len(stops) != 1 || !strings.Contains(stops[0]["reason"].(string), "the Claude Fable") {
		t.Fatalf("stop lines %v, want held weighed for the Fable reset it has left", stops)
	}
}

// 7. The meters a claim gives back: after a landed claim whose read is
// refused, the week figure read before the claim (100% used) is neither on
// the row nor ranked on, the row names the week as not read since the
// claim, and a new session can be placed on the account. A reply's headers
// measured after the claim put the week back on the row.
func TestAFigureFromBeforeTheClaimIsNotShownOrRankedOn(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	f := newClaudeClaimFake(t, map[string]*claudeClaimProfile{
		"held":  {week: 95, fable: 20, grants: []bool{false}},
		"spare": {week: 100, fable: 100},
	})
	d := f.daemon(t, "")
	answerReadsAfterClaim(d, f, "held", readAfterRefused)
	f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
	d.measure()
	if week, ok := claimRows(t, d.server)["held"].window("7d"); !ok || week.UsedPercent != 100 {
		t.Fatalf("calibration: before the claim held's row reads the week %+v", week)
	}
	d.pinSession("warm", "held")
	d.server.autoResetReadings(context.Background())
	settle(t, d.server)
	if claims, _ := f.counts(); claims != 1 || d.decision("held").Verdict != AutoResetClaimed {
		t.Fatalf("calibration: %d claims, decision %+v; want one claim at the reading", claims, d.decision("held"))
	}
	row := claimRows(t, d.server)["held"]
	if week, ok := row.window("7d"); ok || !slices.Contains(row.NotReadSinceClaim, "7d") {
		t.Fatalf("after the claim held's row reads the week %+v (%v), not read since the claim: %v; want no figure and the week named",
			week, ok, row.NotReadSinceClaim)
	}
	score, _ := d.server.SchedulerRef.Get().LookupScore(accounts.ProviderClaude, "held")
	if !score.WeeklyResetAt.IsZero() || score.Headroom <= 0 || !score.ExhaustedResetAt.IsZero() {
		t.Fatalf("after the claim held ranks on %+v, want no week figure from before the claim", score)
	}
	if answer := d.turn("new-session", opusModel); answer.status != http.StatusOK || !strings.Contains(answer.body, "msg_held") ||
		d.sessionAccount("new-session") != "held" {
		t.Fatalf("a new session after the claim got %+v on %q, want it placed on held", answer, d.sessionAccount("new-session"))
	}

	replies := withReplyHeaders(t, d, f, http.Header{
		"Anthropic-Ratelimit-Unified-7d-Status":      {"allowed"},
		"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.05"},
		"Anthropic-Ratelimit-Unified-7d-Reset":       {strconv.FormatInt(time.Now().Add(72*time.Hour).Unix(), 10)},
	})
	if answer := replies.ask("new-session", opusModel, nil); answer.status != http.StatusOK {
		t.Fatalf("calibration: the reply with headers got %+v", answer)
	}
	row = claimRows(t, d.server)["held"]
	if week, ok := row.window("7d"); !ok || week.UsedPercent != 5 || slices.Contains(row.NotReadSinceClaim, "7d") {
		t.Fatalf("after a reply measured after the claim held's row reads the week %+v (%v), not read: %v; want 5%%",
			week, ok, row.NotReadSinceClaim)
	}
}

// replyHeaderDaemon is the daemon of d with every Messages answer carrying
// header, as Anthropic's replies carry their unified rate-limit figures.
type replyHeaderDaemon struct{ *claudeClaimDaemon }

func withReplyHeaders(t *testing.T, d *claudeClaimDaemon, f *claudeClaimFake, header http.Header) replyHeaderDaemon {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for name, values := range header {
			w.Header()[name] = values
		}
		f.turn(w, r)
	}))
	t.Cleanup(upstream.Close)
	server := d.server
	server.ClaudeUpstream = mustURL(t, upstream.URL)
	proxied := httptest.NewServer(server.Handler())
	t.Cleanup(proxied.Close)
	copy := *d
	copy.server, copy.proxy = server, proxied
	return replyHeaderDaemon{&copy}
}

// leftOut wraps the fake Anthropic: once it answered a claim, the held
// account's usage reads leave key out of their answer.
type leftOut struct {
	f       *claudeClaimFake
	account string
	key     string
	mu      sync.Mutex
	after   int
}

func (r *leftOut) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := r.f.anthropic(req)
	r.f.mu.Lock()
	claimed := r.f.claims > 0
	r.f.mu.Unlock()
	name := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer tok-")
	if err != nil || !claimed || name != r.account || req.URL.Path != "/api/oauth/usage" {
		return response, err
	}
	r.mu.Lock()
	r.after++
	r.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(response.Body).Decode(&body)
	delete(body, r.key)
	raw, _ := json.Marshal(body)
	response.Body = io.NopCloser(bytes.NewReader(raw))
	return response, nil
}

// 9. §99's independent check, the second finding: a Fable reset claimed at
// a Fable wall, the usage answer after it leaving the Fable bucket out, and a
// Fable figure of 100% cached from before the claim. Here the figure comes
// from a reply's headers, which fill the Fable cache and leave the account's
// score as the usage read made it, so the session's next Fable turn still
// goes to the account and meets the wall. No reading of the Fable meter was
// taken after the claim, so the turn is served in place, and the row does
// not show the old figure.
func TestAFableFigureFromBeforeTheClaimIsNoReadingAfterIt(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	f := newClaudeClaimFake(t, map[string]*claudeClaimProfile{
		"held":  {week: 20, fable: 95, grants: []bool{true}},
		"spare": {week: 40, fable: 10},
	})
	d := f.daemon(t, "")
	reads := &leftOut{f: f, account: "held", key: "seven_day_oauth_apps"}
	d.server.AccountRef.client = &http.Client{Transport: reads}
	d.pinSession("conversation", "held")
	replies := withReplyHeaders(t, d, f, http.Header{
		"Anthropic-Ratelimit-Unified-7d_oi-Status":      {"allowed"},
		"Anthropic-Ratelimit-Unified-7d_oi-Utilization": {"1.0"},
		"Anthropic-Ratelimit-Unified-7d_oi-Reset":       {strconv.FormatInt(time.Now().Add(72*time.Hour).Unix(), 10)},
	})
	if answer := replies.ask("conversation", opusModel, nil); answer.status != http.StatusOK {
		t.Fatalf("calibration: the reply with the Fable figure got %+v", answer)
	}
	if cached, ok := d.server.AccountRef.cachedClaudeSupplemental(d.server.AccountRef.credentialSnapshot(accounts.ProviderClaude, "held"), time.Now()); !ok ||
		!slices.ContainsFunc(cached, func(w accounts.UsageWindow) bool {
			return w.Name == agentclaude.FableWindowName && w.UsedPercent == 100
		}) {
		t.Fatalf("calibration: before the claim the Fable cache holds %+v (%v), want the Fable meter at 100%%", cached, ok)
	}
	f.set("held", func(p *claudeClaimProfile) { p.fable = 100 })
	answer := d.turn("conversation", "claude-fable-5-2")
	claims, served := f.counts()
	reads.mu.Lock()
	after := reads.after
	reads.mu.Unlock()
	if claims != 1 || after == 0 || !reflect.DeepEqual(served, []string{"held 200", "held 429", "held 200"}) || !strings.Contains(answer.body, "msg_held") ||
		d.sessionAccount("conversation") != "held" {
		t.Fatalf("%d claims, %d reads after, served %v, answer %+v, session on %q; want one claim and the turn served on held",
			claims, after, served, answer, d.sessionAccount("conversation"))
	}
	if decision := d.decision("held"); decision.Verdict != AutoResetClaimed ||
		!strings.Contains(decision.Reason, "the reading after it could not be taken (it carried no figure for the Fable meter)") {
		t.Fatalf("last decision %+v, want claimed, saying the reading after it carried no Fable figure", decision)
	}
	if fable, ok := claimRows(t, d.server)["held"].window(agentclaude.FableWindowName); ok && fable.UsedPercent >= 100 {
		t.Fatalf("after the claim held's row shows the Fable meter from before it: %+v", fable)
	}
}

// 7, a reply in flight across the claim: Anthropic measured its figures when
// it admitted the request, before the claim, so when its headers arrive after
// the claim the account is not ranked on them.
func TestARepliesFiguresFromBeforeTheClaimAreNotRankedOn(t *testing.T) {
	setAutoResetForTest(t, AutoResetShadow, true)
	f := newClaudeClaimFake(t, map[string]*claudeClaimProfile{
		"held":  {week: 95, fable: 20, grants: []bool{false}, usableEarly: true},
		"spare": {week: 40, fable: 10},
	})
	d := f.daemon(t, "")
	answerReadsAfterClaim(d, f, "held", readAfterRefused)
	arrived, release := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-release
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.95")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(time.Now().Add(72*time.Hour).Unix(), 10))
		f.turn(w, r)
	}))
	t.Cleanup(upstream.Close)
	server := d.server
	server.ClaudeUpstream = mustURL(t, upstream.URL)
	slow := *d
	slow.server, slow.proxy = server, httptest.NewServer(server.Handler())
	t.Cleanup(slow.proxy.Close)
	d.pinSession("slow", "held")
	answered := make(chan claudeAnswer, 1)
	go func() { answered <- slow.ask("slow", opusModel, nil) }()
	<-arrived
	if code, response := postClaudeReset(t, d.server.Handler(), `{"account":"held","dry_run":false,"confirm_account":"held"}`); code != http.StatusOK || response.Result != "reset" {
		t.Fatalf("calibration: the claim answered %d %+v", code, response)
	}
	close(release)
	if answer := <-answered; answer.status != http.StatusOK {
		t.Fatalf("calibration: the reply in flight got %+v", answer)
	}
	score, _ := d.server.SchedulerRef.Get().LookupScore(accounts.ProviderClaude, "held")
	if !score.WeeklyResetAt.IsZero() || score.WeeklyRemaining != 0 {
		t.Fatalf("after the claim held ranks on the week a reply measured before it: %+v", score)
	}
	if week, ok := claimRows(t, d.server)["held"].window("7d"); ok {
		t.Fatalf("after the claim held's row reads the week %+v from a reply measured before it", week)
	}
}

// 7, the provider's hold-off: the usage endpoint is holding the account off
// at the claim, so its reads serve the figures a throttled answer carried
// before the claim (the week at 99%) without asking, and the sweep serves
// its last good row (95%) over them. The row shows no week figure from before
// the claim, and names the week as not read since the claim.
func TestFiguresAThrottledAnswerCarriedBeforeTheClaimAreNotShown(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	f := newClaudeClaimFake(t, heldWeek())
	d := f.daemon(t, "")
	reads := answerReadsAfterClaim(d, f, "held", readAfterServed)
	reads.mu.Lock()
	reads.answer, reads.before, reads.retryAfter = readAfterFigures, true, "60"
	reads.mu.Unlock()
	f.set("held", func(p *claudeClaimProfile) { p.week = 99 })
	d.measure()
	reads.set(readAfterServed)
	if response, _ := d.server.claudeReset(context.Background(), context.Background(), ClaudeResetRequest{Account: "held"}); response.Status == nil {
		t.Fatalf("calibration: the dry run read no saved resets: %+v", response)
	}
	if week, ok := claimRows(t, d.server)["held"].window("7d"); !ok || week.UsedPercent < 95 {
		t.Fatalf("calibration: during the hold-off held's row reads the week %+v (%v), want a figure from before the claim", week, ok)
	}
	d.pinSession("conversation", "held")
	f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
	if answer := d.turn("conversation", opusModel); !strings.Contains(answer.body, "msg_held") {
		t.Fatalf("calibration: the turn got %+v, want it served on held after one claim", answer)
	}
	row := claimRows(t, d.server)["held"]
	if week, ok := row.window("7d"); ok || !slices.Contains(row.NotReadSinceClaim, "7d") {
		t.Fatalf("after the claim held's row reads the week %+v (%v), not read since the claim: %v; want no figure from before it",
			week, ok, row.NotReadSinceClaim)
	}
}
