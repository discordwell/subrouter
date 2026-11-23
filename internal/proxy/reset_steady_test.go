package proxy

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// PATCHES §96: need holds its answer near its bar, a choice keeps its place
// through a short gap in the pool's need, and an account whose automatic
// claim has not settled is not chosen. Every provider here is a fake.

// weekNeed is the Claude week's need read through needs at now, with its
// 24-hour pressure day and its 6-hour pressure six rising by trend (nil for
// none yet).
func weekNeed(needs *resetNeeds, now time.Time, day, six float64, trend *float64) MeterNeed {
	pressure := map[string]Pressure{"1h": measured(six), "6h": measured(six), "24h": measured(day)}
	return readNeed(needs, now, needOf(pressure, trend))
}

func readNeed(needs *resetNeeds, now time.Time, raw MeterNeed) MeterNeed {
	meters := []UsagePoolMeter{{Provider: string(accounts.ProviderClaude), Meter: usageMeterWeek, Need: &raw}}
	needs.hold(meters, now)
	return *meters[0].Need
}

func wantNeed(t *testing.T, what string, got MeterNeed, answer ResetNeed, held bool, words ...string) {
	t.Helper()
	if got.Answer != answer || got.Held != held {
		t.Fatalf("%s: need %s (held %v), want %s (held %v): %s", what, got.Answer, got.Held, answer, held, got.Why)
	}
	wantWords(t, what, got.Why, words...)
}

// 1. Need holds near its bar. From yes, 0.98 and 0.95 stay yes and say so,
// with where it turns no; 0.89 is no. From no, 0.98 is no and 1.0 is yes.
// The rising case turns it yes and holds it the same way.
func TestNeedHoldsItsYesUntilThePressureFallsUnderTheHoldBar(t *testing.T) {
	needs := &resetNeeds{}
	at := func(minutes int) time.Time { return choiceNow.Add(time.Duration(minutes) * time.Minute) }
	wantNeed(t, "1.02", weekNeed(needs, at(0), 1.02, 1.02, nil), ResetNeedYes, false, "the 24-hour pressure is 1.02, at or above 1.0")
	for i, pressure := range []float64{0.98, 0.95} {
		if raw := needOf(map[string]Pressure{"24h": measured(pressure), "6h": measured(pressure)}, nil); raw.Answer != ResetNeedNo {
			t.Fatalf("calibration: the figures alone at %.2f answer %s", pressure, raw.Answer)
		}
		got := weekNeed(needs, at(1+i), pressure, pressure, nil)
		wantNeed(t, "after a yes", got, ResetNeedYes, true, fmt.Sprintf("still yes: the 24-hour pressure is %.2f, and it turns no under 0.9", pressure))
		if got.Window != "24h" {
			t.Fatalf("a held yes reads the window %q, want 24h", got.Window)
		}
	}
	wantNeed(t, "0.89", weekNeed(needs, at(3), 0.89, 0.89, nil), ResetNeedNo, false, "the 24-hour pressure is 0.89, under 1.0")
	wantNeed(t, "0.98 from no", weekNeed(needs, at(4), 0.98, 0.98, nil), ResetNeedNo, false, "the 24-hour pressure is 0.98, under 1.0")
	wantNeed(t, "1.0 from no", weekNeed(needs, at(5), 1.0, 1.0, nil), ResetNeedYes, false, "at or above 1.0")

	rising := &resetNeeds{}
	wantNeed(t, "rising to 1.15", weekNeed(rising, at(0), 0.8, 0.95, figure(0.2)), ResetNeedYes, false, "is 1.15, at or above 1.1")
	held := weekNeed(rising, at(1), 0.8, 0.9, figure(0.05))
	wantNeed(t, "rising 0.95", held, ResetNeedYes, true,
		"still yes: the 24-hour pressure is 0.80, and the 6-hour pressure 0.90 plus its trend +0.05 is 0.95, and it turns no under 0.9")
	if held.Window != "6h" {
		t.Fatalf("a yes the rising case holds reads the window %q, want 6h", held.Window)
	}
	wantNeed(t, "rising 0.85", weekNeed(rising, at(2), 0.8, 0.85, figure(0)), ResetNeedNo, false, "is 0.85, under 1.1")
}

// 2. Without a figure a yes holds for 30 minutes, then need is unknown and
// the yes is gone. A daemon that has just started answers from the figures
// alone.
func TestAYesHoldsThirtyMinutesWithoutAFigure(t *testing.T) {
	none := needOf(map[string]Pressure{}, nil)
	needs := &resetNeeds{}
	wantNeed(t, "1.02", weekNeed(needs, choiceNow, 1.02, 1.02, nil), ResetNeedYes, false)
	for _, after := range []time.Duration{10 * time.Minute, 30 * time.Minute} {
		wantNeed(t, "no figure "+after.String()+" on", readNeed(needs, choiceNow.Add(after), none), ResetNeedYes, true,
			"still yes: no window has a pressure now, and the last one said yes "+shortDuration(after)+" ago", "a yes holds for 30m")
	}
	wantNeed(t, "no figure 31 minutes on", readNeed(needs, choiceNow.Add(31*time.Minute), none), ResetNeedUnknown, false,
		"no window has enough history")
	wantNeed(t, "0.95 once the yes is gone", weekNeed(needs, choiceNow.Add(32*time.Minute), 0.95, 0.95, nil), ResetNeedNo, false)

	started := &resetNeeds{}
	wantNeed(t, "a new daemon at 0.95", weekNeed(started, choiceNow, 0.95, 0.95, nil), ResetNeedNo, false)
	wantNeed(t, "a new daemon with no figure", readNeed(started, choiceNow, none), ResetNeedUnknown, false)
}

// claudeWeekHistory is a usage history as worthClaude seeds it: each
// account's Claude week rising at its rate for 49 hours, its clock 100 hours
// before the live readings.
func claudeWeekHistory(t *testing.T, burns map[string]float64) *UsageHistory {
	t.Helper()
	now := time.Now().Add(-100 * time.Hour).Truncate(time.Second)
	h := burnHistory(t, now)
	for id, rate := range burns {
		seedSeries(h, accounts.Account{ID: id, Provider: accounts.ProviderClaude}, now, 49*time.Hour, 0,
			func(ago time.Duration, _ time.Time) []accounts.UsageWindow {
				return claudeWeek(100*line(0.10, rate, 49*time.Hour)(ago), now.Add(3*burnDay))
			})
	}
	return h
}

// wallDecision sends a turn on held that meets the week's wall, and returns
// the decision made there.
func (d *autoResetClaude) wallDecision(session string) AutoResetDecision {
	d.t.Helper()
	d.clearMarks("held")
	d.pinSession(session, "held")
	d.turn(session, opusModel, "")
	decision, _ := d.decision("held")
	if decision.Moment != "wall" {
		d.t.Fatalf("calibration: no decision at the wall: %+v", decision)
	}
	return decision
}

func weekNeedOf(t *testing.T, server Server) MeterNeed {
	t.Helper()
	need := poolMeter(t, burnReport(t, server), string(accounts.ProviderClaude), usageMeterWeek).Need
	if need == nil {
		t.Fatal("burn.json publishes no need on the Claude week")
	}
	return *need
}

// 3. One answer per daemon. Held is read at 95% of a week rolling in 60
// hours, with a week reset, and spare's week rises at 1.02 and then at 0.95.
// burn.json, the decision at held's wall and the choice all say yes at 1.02,
// and all say still yes at 0.95, where a daemon that has just started says no
// in all three. A yes burn.json alone read at 1.02 is the one the decision and
// the choice read at 0.95.
func TestBurnJSONTheDecisionAndTheChoiceReadOneNeed(t *testing.T) {
	setAutoResetWorthForTest(t, true)
	setResetLoadingForTest(t, true)
	ends := time.Now().Add(17 * burnDay)
	daemon := func(rate float64) *autoResetClaude {
		return worthClaude(t, AutoResetShadow, map[string]float64{"spare": rate}, heldAtTheWall(ends, false, 0))
	}
	choice := func(d *autoResetClaude) *selectacct.ResetChoice {
		d.pass()
		would, _ := published(t, usageRows(t, d.server)["held"])
		return would
	}

	d := daemon(1.02)
	wantNeed(t, "burn.json at 1.02", weekNeedOf(t, d.server), ResetNeedYes, false, "the 24-hour pressure is 1.02")
	if decision := d.wallDecision("first"); decision.Verdict != AutoResetWouldClaim || decision.Rule == nil || decision.Rule.Need != ResetNeedYes {
		t.Fatalf("the decision at 1.02: %+v", decision)
	}
	if would := choice(d); would == nil || !strings.Contains(would.Why, "the 24-hour pressure is 1.02") {
		t.Fatalf("the choice at 1.02: %+v", would)
	}
	d.server.AccountRef.SetUsageHistory(claudeWeekHistory(t, map[string]float64{"spare": 0.95}))
	const stillYes = "still yes: the 24-hour pressure is 0.95, and it turns no under 0.9"
	decision := d.wallDecision("second")
	if decision.Verdict != AutoResetWouldClaim || decision.Rule == nil || decision.Rule.Need != ResetNeedYes || !strings.Contains(decision.Reason, stillYes) {
		t.Fatalf("the decision at 0.95: %+v", decision)
	}
	if would := choice(d); would == nil || !strings.Contains(would.Why, stillYes) {
		t.Fatalf("the choice at 0.95: %+v", would)
	}
	wantNeed(t, "burn.json at 0.95", weekNeedOf(t, d.server), ResetNeedYes, true, stillYes)

	fresh := daemon(0.95)
	wantNeed(t, "calibration: burn.json of a new daemon at 0.95", weekNeedOf(t, fresh.server), ResetNeedNo, false, "the 24-hour pressure is 0.95, under 1.0")
	if decision := fresh.wallDecision("first"); decision.Verdict != AutoResetHeldBack {
		t.Fatalf("calibration: the decision of a new daemon at 0.95: %+v", decision)
	}
	if would := choice(fresh); would != nil {
		t.Fatalf("calibration: a new daemon at 0.95 chooses %+v", would)
	}

	shared := daemon(1.02)
	weekNeedOf(t, shared.server)
	shared.server.AccountRef.SetUsageHistory(claudeWeekHistory(t, map[string]float64{"spare": 0.95}))
	if decision := shared.wallDecision("first"); decision.Verdict != AutoResetWouldClaim || !strings.Contains(decision.Reason, stillYes) {
		t.Fatalf("the decision after burn.json alone read the yes: %+v", decision)
	}
	if would := choice(shared); would == nil || !strings.Contains(would.Why, stillYes) {
		t.Fatalf("the choice after burn.json alone read the yes: %+v", would)
	}
}

// gapPasses chooses as the daemon's passes do, each from what the one before
// left: its choices, the gaps still open and the Codex hold. outs gives an
// account a reason it cannot be chosen whatever the pool says, as a dead
// credential or an owner's reserve does.
type gapPasses struct {
	before map[string]resetChosen
	gaps   map[string]resetGap
	hold   resetCodexHold
}

func (p *gapPasses) pass(now time.Time, outs map[string]string, list ...choiceAccount) choiceResult {
	ids := map[string]string{}
	var weighed []resetWeighing
	var rows []AccountUsageStatus
	for _, a := range list {
		rows = append(rows, a.row(now))
	}
	pool := Server{}.resetPoolOf(rows, now)
	for _, row := range rows {
		c := resetCandidate{key: autoResetKey(accounts.Account{ID: row.ID, Provider: row.Provider}), provider: string(row.Provider),
			id: row.ID, label: row.Label, readings: autoResetReadingsOf(&row), burn: row.Burn}
		switch {
		case outs[row.ID] != "":
			c.out = outs[row.ID]
		case len(row.Resets) == 0:
			c.out = "it holds no saved reset"
		default:
			c.next = row.Resets[0]
		}
		ids[c.key] = row.ID
		weighed = append(weighed, pool.weigh(c))
	}
	choosing, gaps := chooseAcrossGaps(weighed, p.before, p.gaps, p.hold, now)
	p.before, p.gaps, p.hold = map[string]resetChosen{}, gaps, choosing.codex
	out := choiceResult{chosen: map[string]selectacct.ResetChoice{}, why: map[string]string{}, hold: choosing.codex}
	for key, choice := range choosing.choices {
		p.before[key] = resetChosen{choice: choice, label: ids[key]}
		out.chosen[ids[key]] = choice
	}
	for key, why := range choosing.notChosen {
		out.why[ids[key]] = why
	}
	return out
}

func minutes(n int) time.Duration { return time.Duration(n) * time.Minute }

// 4, on facts. Two week holders, a first and b second, in a pool at 1.5.
// The pool's need turns no (0.7) for a pass, and during that gap b spends
// most of its week, so the usual order would put b first. Back 20 minutes
// into the gap, both are chosen in their old order, and keep it until the
// gap's 30 minutes are up; then the usual order. Back after 31 minutes, the
// usual order.
func TestAChosenWeekHolderKeepsItsPlaceThroughAGapInNeed(t *testing.T) {
	pool := func(rate, bLeft float64) []choiceAccount {
		return append([]choiceAccount{
			{id: "a", kind: accounts.ResetKindClaudeWeek, week: 0.1, fable: 0.5, toRoll: 100 * time.Hour, expires: 17 * burnDay, weekBurn: rate},
			{id: "b", kind: accounts.ResetKindClaudeWeek, week: bLeft, fable: 0.5, toRoll: 110 * time.Hour, expires: 17 * burnDay, weekBurn: rate},
		}, spares(rate, 2)...)
	}
	p := &gapPasses{}
	wantChosen(t, "before the gap", p.pass(choiceNow, nil, pool(1.5, 0.9)...), selectacct.ResetChoiceWeek, "a", "b")
	gap := p.pass(choiceNow.Add(minutes(5)), nil, pool(0.7, 0.9)...)
	wantChosen(t, "during the gap", gap, selectacct.ResetChoiceWeek)
	until := rfc3339(choiceNow.Add(minutes(35)))
	for _, id := range []string{"a", "b"} {
		wantWords(t, "during the gap", gap.why[id], "the pool does not need its saved resets on the Claude week", "it keeps its place until "+until)
	}
	wantChosen(t, "calibration: the usual order once b has spent its week",
		(&gapPasses{}).pass(choiceNow.Add(minutes(25)), nil, pool(1.5, 0.05)...), selectacct.ResetChoiceWeek, "b", "a")

	back := *p
	got := back.pass(choiceNow.Add(minutes(25)), nil, pool(1.5, 0.05)...)
	wantChosen(t, "back 20 minutes into the gap", got, selectacct.ResetChoiceWeek, "a", "b")
	wantWords(t, "back in its place", got.chosen["a"].Why, "it keeps the place it had before the pool's need paused, until "+until)
	wantChosen(t, "29 minutes into the gap", back.pass(choiceNow.Add(minutes(34)), nil, pool(1.5, 0.05)...), selectacct.ResetChoiceWeek, "a", "b")
	wantChosen(t, "31 minutes after the gap began", back.pass(choiceNow.Add(minutes(36)), nil, pool(1.5, 0.05)...), selectacct.ResetChoiceWeek, "b", "a")

	late := *p
	got = late.pass(choiceNow.Add(minutes(36)), nil, pool(1.5, 0.05)...)
	wantChosen(t, "back after 31 minutes", got, selectacct.ResetChoiceWeek, "b", "a")
	if strings.Contains(got.chosen["a"].Why, "before the pool's need paused") {
		t.Fatalf("after 31 minutes a still names its old place: %s", got.chosen["a"].Why)
	}
}

// 4, through the daemon in real mode. Held is chosen and takes a new
// session. While the pool's need is no it is neither published nor ranked
// on, so a new session goes to spare, and the stop line says it keeps its
// place. Need back 20 minutes later, it is chosen again and takes the next
// new session.
func TestAChoiceIsNeitherPublishedNorRankedDuringAGap(t *testing.T) {
	profiles := choiceProfiles("")
	profiles["spare"].week = 10
	d := choiceClaude(t, AutoResetReal, true, nil, profiles)
	if got := d.servedBy("early"); got != "spare" {
		t.Fatalf("calibration: before any choice a new session goes to %s, want spare", got)
	}
	d.pass()
	if _, inside := published(t, usageRows(t, d.server)["held"]); inside == nil || d.servedBy("chosen") != "held" {
		t.Fatalf("calibration: held is not chosen and loaded: %+v", inside)
	}
	d.server.AccountRef.SetUsageHistory(claudeWeekHistory(t, map[string]float64{"held": 0.5, "spare": 0.5}))
	d.pass()
	if would, inside := published(t, usageRows(t, d.server)["held"]); would != nil || inside != nil {
		t.Fatalf("during the gap held publishes %+v and %+v", would, inside)
	}
	if got := d.servedBy("during"); got != "spare" {
		t.Fatalf("during the gap a new session goes to %s, want spare", got)
	}
	stops := jsonLogLines(d.logs, resetUnchooseMessage)
	if len(stops) != 1 || !strings.Contains(stops[0]["reason"].(string), "it keeps its place until") {
		t.Fatalf("stop lines %v, want one saying held keeps its place", stops)
	}
	d.server.AccountRef.SetUsageHistory(claudeWeekHistory(t, map[string]float64{"held": 1.2, "spare": 1.2}))
	setAutoResetClock(d.server, later(minutes(20)))
	d.pass()
	if _, inside := published(t, usageRows(t, d.server)["held"]); inside == nil || inside.Order != 1 {
		t.Fatalf("need back 20 minutes later: held publishes %+v, want order 1", inside)
	}
	if got := d.servedBy("after"); got != "held" {
		t.Fatalf("need back 20 minutes later a new session goes to %s, want held", got)
	}
}

// codexAt is codexHolders in a pool at rate; with passed, c100's roll is 200
// hours away, so a claim at its wall adds more than c150's.
func codexAt(rate float64, passed bool) []choiceAccount {
	return codexHolders(func(a *choiceAccount) {
		a.weekBurn = rate
		if passed && a.id == "c100" {
			a.toRoll = 200 * time.Hour
		}
	})
}

// 5. The Codex account being loaded, c150, keeps its place through a gap in
// the pool's need: when need returns 20 minutes later it is loaded again from
// the same moment, and c100, whose claim now adds more, is not chosen in its
// place. After a gap of 31 minutes the usual rule chooses c100.
func TestTheCodexAccountBeingLoadedKeepsItsPlaceThroughAGap(t *testing.T) {
	p := &gapPasses{}
	first := p.pass(choiceNow, nil, codexAt(1.3, false)...)
	wantChosen(t, "before the gap", first, selectacct.ResetChoiceCodexWeek, "c150")
	gap := p.pass(choiceNow.Add(minutes(5)), nil, codexAt(0.7, false)...)
	wantChosen(t, "during the gap", gap, selectacct.ResetChoiceCodexWeek)
	wantWords(t, "during the gap", gap.why["c150"], "the pool does not need its saved resets on the Codex week", "it keeps its place until")
	if gap.hold != first.hold {
		t.Fatalf("during the gap the hold is %+v, want %+v", gap.hold, first.hold)
	}

	back := *p
	got := back.pass(choiceNow.Add(minutes(25)), nil, codexAt(1.3, true)...)
	wantChosen(t, "back 20 minutes into the gap", got, selectacct.ResetChoiceCodexWeek, "c150")
	wantWords(t, "the one worth more", got.why["c100"], "Codex accounts are loaded one at a time, and another one is loaded now")
	if got.hold != first.hold {
		t.Fatalf("back 20 minutes into the gap the hold is %+v, want %+v", got.hold, first.hold)
	}

	late := *p
	wantChosen(t, "back after 31 minutes", late.pass(choiceNow.Add(minutes(36)), nil, codexAt(1.3, true)...), selectacct.ResetChoiceCodexWeek, "c100")
}

// droppedAtOnce runs c150 chosen, then a pass where the pool's need turns no
// and, in the case, change and outs say something else of c150 too; then
// need back 20 minutes into the gap, with c100 worth more and c150 able to be
// chosen again. The calibration runs the same without the other reason, and
// the gap keeps c150's place; in the case c150 is dropped at once.
func droppedAtOnce(t *testing.T, why string, change func(*choiceAccount), outs map[string]string) {
	t.Helper()
	gapPool := func(other bool) []choiceAccount {
		list := codexAt(0.7, false)
		for i := range list {
			if other && list[i].id == "c150" && change != nil {
				change(&list[i])
			}
		}
		return list
	}
	for _, other := range []bool{false, true} {
		p := &gapPasses{}
		first := p.pass(choiceNow, nil, codexAt(1.3, false)...)
		var gapOuts map[string]string
		if other {
			gapOuts = outs
		}
		gap := p.pass(choiceNow.Add(minutes(5)), gapOuts, gapPool(other)...)
		back := p.pass(choiceNow.Add(minutes(25)), nil, codexAt(1.3, true)...)
		if !other {
			wantChosen(t, "calibration: the gap alone", back, selectacct.ResetChoiceCodexWeek, "c150")
			continue
		}
		wantWords(t, "the gap and "+why, gap.why["c150"], why)
		if strings.Contains(gap.why["c150"], "keeps its place") || gap.hold == first.hold {
			t.Fatalf("the gap and %s: c150 kept its place (%s; hold %+v)", why, gap.why["c150"], gap.hold)
		}
		wantChosen(t, "need back after "+why, back, selectacct.ResetChoiceCodexWeek, "c100")
	}
}

// 6. A spent reset drops a choice at once, even while the pool's need is no.
func TestASpentResetDropsAChoiceAtOnce(t *testing.T) {
	droppedAtOnce(t, "it holds no saved reset", func(a *choiceAccount) { a.kind = "" }, nil)
}

// 6. A dead credential drops a choice at once, even while the pool's need is
// no.
func TestADeadCredentialDropsAChoiceAtOnce(t *testing.T) {
	droppedAtOnce(t, "its credential is dead", nil, map[string]string{"c150": "its credential is dead: the refresh token was refused"})
}

// 6. An owner's reserve drops a choice at once, even while the pool's need is
// no.
func TestAnOwnersReserveDropsAChoiceAtOnce(t *testing.T) {
	droppedAtOnce(t, "it has an owner's reserve", nil,
		map[string]string{"c150": "it has an owner's reserve of 5% of its week, which loading would spend"})
}

// 7. In real mode held, chosen, meets its wall, and the redeem answers 502:
// it may have landed. Held is no longer chosen, the line that ends its choice
// says why, and second is chosen in its place. With second's credential dead
// held is still not chosen; once a reading shows held's week back it is.
func TestAnAccountWhoseClaimHasNotSettledIsNotChosen(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	setResetLoadingForTest(t, true)
	const second = "second@example.com"
	f := newCodexClaimFake(t, map[string]*codexClaimAccount{
		heldCodex:  {used: 50, credits: heldCodexCredits("credit-1")},
		second:     {used: 40, credits: heldCodexCredits("credit-2")},
		spareCodex: {used: 30},
	})
	now := time.Now().Add(-100 * time.Hour).Truncate(time.Second)
	h := burnHistory(t, now)
	for _, id := range []string{heldCodex, second, spareCodex} {
		seedSeries(h, accounts.Account{ID: id, Provider: accounts.ProviderCodex}, now, 49*time.Hour, 0,
			codexRising(line(0.10, 1.3, 49*time.Hour), fixedRoll(now.Add(3*burnDay))))
	}
	f.server.AccountRef.SetUsageHistory(h)
	pass := func() { f.server.autoResetPass(context.Background()) }
	chosen := func(id string) bool {
		_, inside := published(t, usageRows(t, f.server)[id])
		return inside != nil
	}
	pass()
	if !chosen(heldCodex) || chosen(second) {
		t.Fatal("calibration: held is not the one Codex account chosen")
	}
	f.consumeStatus = http.StatusBadGateway
	f.set(heldCodex, func(a *codexClaimAccount) { a.used = 99 })
	f.measure()
	f.set(heldCodex, func(a *codexClaimAccount) { a.used = 100 })
	f.clearMarks(heldCodex)
	f.pinSession("wall", heldCodex)
	f.post("wall")
	if decision := f.decision(heldCodex); decision.Verdict != AutoResetMayHaveLanded {
		t.Fatalf("calibration: the claim at the wall is %+v, want may_have_landed", decision)
	}
	f.consumeStatus = 0
	pass()
	if chosen(heldCodex) || !chosen(second) {
		t.Fatalf("after a claim that may have landed: held chosen %v, second chosen %v; want second alone", chosen(heldCodex), chosen(second))
	}
	stops := jsonLogLines(f.logs, resetUnchooseMessage)
	if len(stops) != 1 {
		t.Fatalf("stop lines %v, want one, held's", stops)
	}
	wantWords(t, "the line that ends held's choice", stops[0]["reason"].(string),
		"its automatic claim started at", "is not settled: no reading since shows the week back")

	var dead accounts.Account
	for _, account := range f.server.accountList() {
		if account.ID == second {
			dead = account
		}
	}
	f.server.AccountRef.noteCredResult(dead, &accounts.CodexStoredRefreshFailureError{})
	pass()
	if chosen(heldCodex) || chosen(second) {
		t.Fatal("with second's credential dead and held's claim not settled, an account is chosen")
	}
	f.set(heldCodex, func(a *codexClaimAccount) { a.used = 0 })
	f.measure()
	pass()
	if !chosen(heldCodex) {
		t.Fatal("once a reading shows held's week back, held is not chosen")
	}
	if consumes, _ := f.counts(); consumes != 1 {
		t.Fatalf("%d consumes, want the one claim", consumes)
	}
}
