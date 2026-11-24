package proxy

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// PATCHES §103: no automatic claim while the meter a reset gives back rolls
// in under 24 hours, except in the reset's last chance, and a Fable reset only
// on an account with at least 25% of its week left. The decision is judged on
// facts, §95's choice on rows, and a real claim's last check through the
// fakes of §93.2. No test sends a request to a real provider.

// gateCase is one decision on facts: its verdict, the rule's step, and words
// its reason must carry.
type gateCase struct {
	name    string
	facts   autoResetFacts
	verdict AutoResetVerdict
	by      AutoResetRuleStep
	reason  []string
}

func checkGate(t *testing.T, cases []gateCase) {
	t.Helper()
	for _, c := range cases {
		got := decideAutoReset(c.facts)
		if got.Verdict != c.verdict || got.Rule == nil || got.Rule.DecidedBy != c.by {
			t.Errorf("%s: %s by %+v, want %s by %s: %s", c.name, got.Verdict, got.Rule, c.verdict, c.by, got.Reason)
			continue
		}
		for _, words := range c.reason {
			if !strings.Contains(got.Reason, words) {
				t.Errorf("%s: the reason does not say %q: %s", c.name, words, got.Reason)
			}
		}
	}
}

// fableAt0837 is the claim of 2026-11-24 08:37, in invented numbers where
// they do not matter: a reading at 100% of an account's Fable meter, its next
// reset a Fable reset that expires in 105 days, the roll 6h23m away, the
// account's own 24-hour rate 7.07 in a pool at 1.05, and its week 97% used.
// §94 alone claimed it: 7.07 x 6h23m / 168h is 0.27 of a week, over the floor
// of 0.25.
var fableAt0837 = worthFixture{kind: autoResetKindFable, left: 0, toRoll: 6*time.Hour + 23*time.Minute, expires: 105 * burnDay,
	pool: 1.05, own: 7.07, week: 0.03}

// The 08:37 claim is held: its meter rolls in 6h23m. The rule still shows
// what a claim would have added and the account's week.
func TestTheFableClaimSixHoursBeforeTheRollIsHeld(t *testing.T) {
	got := decideAutoReset(fableAt0837.facts(true))
	if got.Verdict != AutoResetHeldBack || got.Rule == nil || got.Rule.DecidedBy != AutoResetByRollGate {
		t.Fatalf("the 08:37 claim: %s by %+v, want held back by the 24-hour gate: %s", got.Verdict, got.Rule, got.Reason)
	}
	for _, words := range []string{"a reading says the Fable meter is 100.0% used", "held back: the Fable meter rolls in 6h23m, under 24h",
		"only a reset's last chance is claimed"} {
		if !strings.Contains(got.Reason, words) {
			t.Fatalf("the reason does not say %q: %s", words, got.Reason)
		}
	}
	rule := got.Rule
	if rule.HoursToRoll == nil || *rule.HoursToRoll != 6.4 || rule.Value == nil || math.Abs(*rule.Value-0.269) > 0.001 ||
		rule.WeekLeft == nil || *rule.WeekLeft != 0.03 {
		t.Fatalf("the rule %+v, want 6.4 hours to the roll, a value of 0.269 and 0.03 of the week left", rule)
	}
}

// A week reset claimed 18 to 23 hours before its roll, as two were on
// 2026-11-23, is held whatever it would add; from 24 hours on the rest of the
// rule decides. A Codex credit is held by the gate too.
func TestNoResetIsClaimedInsideADayOfItsRoll(t *testing.T) {
	week := func(toRoll time.Duration) worthFixture {
		return worthFixture{kind: autoResetKindWeek, left: 0.004, toRoll: toRoll, expires: 17 * burnDay, pool: 1.2, own: 3.0}
	}
	checkGate(t, []gateCase{
		{"a week reset 18 hours before the roll", week(18 * time.Hour).facts(true), AutoResetHeldBack, AutoResetByRollGate,
			[]string{"held back: the week rolls in 18h, under 24h"}},
		{"a week reset 23h59m before the roll", week(24*time.Hour - time.Minute).facts(true), AutoResetHeldBack, AutoResetByRollGate,
			[]string{"the week rolls in 23h59m, under 24h"}},
		{"a week reset 24 hours before the roll", week(24 * time.Hour).facts(true), AutoResetWouldClaim, AutoResetByValue,
			[]string{"a claim adds 0.43 of a week"}},
		{"a Codex credit 20 hours before the roll", worthFixture{kind: autoResetKindCodexCredit, left: 0.004, toRoll: 20 * time.Hour,
			expires: 20 * burnDay, pool: 1.3}.facts(true), AutoResetHeldBack, AutoResetByRollGate, []string{"the week rolls in 20h, under 24h"}},
	})
}

// Under a day from the roll only last chance claims. The expiry window alone
// does not pass the gate, and inside it a roll not read is no claim.
func TestOnlyLastChanceClaimsInsideADayOfTheRoll(t *testing.T) {
	sixHours := worthFixture{kind: autoResetKindWeek, left: 0.004, toRoll: 6 * time.Hour, expires: 17 * burnDay, pool: 1.2, own: 1.2}
	unread := windowOpen(sixHours).facts(true)
	unread.worth.roll = time.Time{}
	checkGate(t, []gateCase{
		{"last chance, 6 hours before the roll", expiring(sixHours, 5*time.Hour).facts(true), AutoResetWouldClaim, AutoResetByLastChance,
			[]string{"its last chance"}},
		{"the expiry window open, 6 hours before the roll, not its last chance", windowOpen(sixHours).facts(true), AutoResetHeldBack,
			AutoResetByRollGate, []string{"held back: the week rolls in 6h, under 24h"}},
		{"the expiry window open and its last chance", windowOpen(expiring(sixHours, 5*time.Hour)).facts(true), AutoResetWouldClaim,
			AutoResetByExpiryWindow, []string{"inside its expiry window"}},
		{"the expiry window open, the roll not read", unread, AutoResetNoClaim, AutoResetByRollUnread,
			[]string{"the roll of the week is not read", "so nothing is claimed"}},
	})
}

// A Fable reset is claimed only on an account whose week has at least 25%
// left, read at most five minutes ago. A week reset has no such bar.
func TestAFableResetIsClaimedOnlyWithRoomOnTheWeek(t *testing.T) {
	fable := func(weekLeft float64) worthFixture {
		return worthFixture{kind: autoResetKindFable, left: 0, toRoll: 48 * time.Hour, expires: 100 * burnDay, pool: 1.2, own: 1.2, week: weekLeft}
	}
	unread := fable(0.5)
	unread.weekUnread = true
	stale := fable(0.5).facts(true)
	reading := stale.readings[autoResetWeek]
	reading.at = worthNow.Add(-7 * time.Minute)
	stale.readings[autoResetWeek] = reading
	checkGate(t, []gateCase{
		{"its week 97% used", fable(0.03).facts(true), AutoResetHeldBack, AutoResetByWeekRoom,
			[]string{"held back: the week has 3.0% left, under 25%", "out of Fable that still has week left"}},
		{"its week 50% used", fable(0.5).facts(true), AutoResetWouldClaim, AutoResetByValue, []string{"a claim adds 0.34 of a week"}},
		{"its week at 25% left", fable(0.25).facts(true), AutoResetWouldClaim, AutoResetByValue, nil},
		{"its week unread", unread.facts(true), AutoResetNoClaim, AutoResetByWeekRoom,
			[]string{"the week is unread, and a Fable reset is claimed only on an account whose week is read", "so nothing is claimed"}},
		{"its week read 7 minutes ago", stale, AutoResetNoClaim, AutoResetByWeekRoom, []string{"the week was last read 7m0s ago"}},
		{"a week reset on a week 99.6% used", worthFixture{kind: autoResetKindWeek, left: 0.004, toRoll: 48 * time.Hour,
			expires: 17 * burnDay, pool: 1.2, own: 1.2}.facts(true), AutoResetWouldClaim, AutoResetByValue, nil},
	})
	if rule := decideAutoReset(fable(0.03).facts(true)).Rule; rule == nil || rule.WeekLeft == nil || *rule.WeekLeft != 0.03 {
		t.Fatalf("the rule %+v, want week_left 0.03", rule)
	}
	if rule := decideAutoReset(unread.facts(true)).Rule; rule == nil || rule.WeekLeft != nil {
		t.Fatalf("the rule %+v, want no week_left for a week not read", rule)
	}
}

// §95 loads nothing toward a claim the gate would hold: an account whose
// roll is under a day past the wall it is heading for is not chosen, unless
// the reset is in its last chance, and not inside the expiry window either; a
// Fable holder with under 25% of its week left is not chosen.
func TestTheChoiceLoadsNothingTowardAClaimTheGateWouldHold(t *testing.T) {
	// At 2.5 over 4 accounts, 30% of a week reaches its limit in 5h2m: a
	// roll 25 hours out is 19h58m past it, where a claim would add 0.30.
	nearRoll := func(toRoll, expires time.Duration) []choiceAccount {
		return append([]choiceAccount{{id: "held", kind: accounts.ResetKindClaudeWeek, week: 0.3, fable: 0.8, toRoll: toRoll, expires: expires,
			weekBurn: 2.5}}, spares(2.5, 3)...)
	}
	got := choose(choiceNow, resetCodexHold{}, nearRoll(25*time.Hour, 17*burnDay)...)
	wantChosen(t, "the roll 19h58m past its wall", got, selectacct.ResetChoiceWeek)
	wantWords(t, "the roll 19h58m past its wall", got.why["held"], "it reaches its limit in 5h2m",
		"a claim of its week reset at its limit would be held: the week rolls in 19h58m, under 24h")
	wantChosen(t, "calibration: the roll 24h58m past its wall", choose(choiceNow, resetCodexHold{}, nearRoll(30*time.Hour, 17*burnDay)...),
		selectacct.ResetChoiceWeek, "held")
	last := choose(choiceNow, resetCodexHold{}, nearRoll(25*time.Hour, 30*time.Hour)...)
	wantChosen(t, "its last chance", last, selectacct.ResetChoiceWeek, "held")
	wantWords(t, "its last chance", last.chosen["held"].Why, "last chance")

	// Four holders of one week reset expiring in 250 hours, at 0.5 a week:
	// they need 1.65 weeks for the 3.3 weeks they have left, so the window is
	// open. Held reaches its limit in 25 hours, 15 before its roll; the reset
	// is not in its last chance, since it expires after the roll plus a week.
	window := []choiceAccount{{id: "held", kind: accounts.ResetKindClaudeWeek, week: 0.3, fable: 0.5, toRoll: 40 * time.Hour,
		expires: 250 * time.Hour, weekBurn: 0.5}}
	for _, id := range []string{"w1", "w2", "w3"} {
		window = append(window, choiceAccount{id: id, kind: accounts.ResetKindClaudeWeek, week: 1, fable: 0.5, toRoll: 150 * time.Hour,
			expires: 250 * time.Hour, weekBurn: 0.5})
	}
	got = choose(choiceNow, resetCodexHold{}, window...)
	wantChosen(t, "inside the expiry window", got, selectacct.ResetChoiceWeek, "w1", "w2", "w3")
	wantWords(t, "inside the expiry window", got.why["held"], "is inside its expiry window", ", but at the pool's whole demand",
		"would be held: the week rolls in 14h48m, under 24h")

	fabled := func(weekLeft float64) []choiceAccount {
		return []choiceAccount{
			{id: "fabled", kind: accounts.ResetKindClaudeFable, week: weekLeft, fable: 0.2, toRoll: 100 * time.Hour, expires: 100 * burnDay,
				weekBurn: 0.5, fableBurn: 1.5},
			{id: "other", week: 0.6, fable: 0.5, toRoll: 100 * time.Hour, weekBurn: 0.5, fableBurn: 1.5},
		}
	}
	wantChosen(t, "calibration: a Fable holder with 60% of its week", choose(choiceNow, resetCodexHold{}, fabled(0.6)...),
		selectacct.ResetChoiceFable, "fabled")
	got = choose(choiceNow, resetCodexHold{}, fabled(0.1)...)
	wantChosen(t, "a Fable holder with 10% of its week", got, selectacct.ResetChoiceFable)
	wantWords(t, "a Fable holder with 10% of its week", got.why["fabled"], "would be held: the week has 10.0% left, under 25%")
}

// A real claim judges the gate again just before it sends: a roll that came
// under 24 hours since the decision, on the clock or on a newer reading, or a
// week that ran low, holds it; a reading older than the decision's is not
// read; with §94's rule off there is no gate.
func TestTheClaimsLastCheckJudgesTheGateAgain(t *testing.T) {
	week := worthFixture{kind: autoResetKindWeek, left: 0.004, toRoll: 24*time.Hour + time.Minute, expires: 17 * burnDay, pool: 1.2, own: 3.0}
	f := week.facts(true)
	if d := decideAutoReset(f); d.Verdict != AutoResetWouldClaim {
		t.Fatalf("calibration: the decision 24h1m before the roll is %s: %s", d.Verdict, d.Reason)
	}
	reading := func(at time.Time, left float64, roll time.Duration) autoResetReading {
		return autoResetReading{left: left, at: at, roll: worthNow.Add(roll)}
	}
	for _, c := range []struct {
		name  string
		facts autoResetFacts
		live  map[autoResetMeter]autoResetReading
		now   time.Time
		held  string
	}{
		{"at the decision's time", f, nil, worthNow, ""},
		{"two minutes on", f, nil, worthNow.Add(2 * time.Minute), "the week rolls in 23h59m, under 24h"},
		{"a newer reading of a roll 23 hours away", f,
			map[autoResetMeter]autoResetReading{autoResetWeek: reading(worthNow, 0, 23*time.Hour)}, worthNow, "the week rolls in 23h, under 24h"},
		{"an older reading of a roll 23 hours away", f,
			map[autoResetMeter]autoResetReading{autoResetWeek: reading(worthNow.Add(-2*time.Minute), 0, 23*time.Hour)}, worthNow, ""},
		{"a Fable reset whose week ran low", fableWithWeek(0.5),
			map[autoResetMeter]autoResetReading{autoResetWeek: reading(worthNow, 0.1, 48*time.Hour)}, worthNow, "the week has 10.0% left, under 25%"},
		{"§94's rule off", week.facts(false), nil, worthNow.Add(2 * time.Minute), ""},
	} {
		err := c.facts.holdBeforeSend(c.live, c.now)
		switch {
		case c.held == "" && err != nil:
			t.Errorf("%s: held: %v", c.name, err)
		case c.held != "" && (err == nil || !strings.Contains(err.Error(), "§103 holds it on the reading before the claim: "+c.held)):
			t.Errorf("%s: %v, want held: %s", c.name, err, c.held)
		}
	}
}

func fableWithWeek(left float64) autoResetFacts {
	return worthFixture{kind: autoResetKindFable, left: 0, toRoll: 48 * time.Hour, expires: 100 * burnDay, pool: 1.2, own: 1.2, week: left}.facts(true)
}

// Through the daemon in real mode at a reading: the decision reads a roll 48
// hours out (Claude) or six days out (Codex) and claims; when the live read
// before the claim says the roll is 23 hours away, nothing is sent.
func TestARealClaimIsHeldWhenTheLiveReadingPutsTheRollUnderADay(t *testing.T) {
	setAutoResetWorthForTest(t, true)
	setAutoResetForTest(t, AutoResetReal, true)
	for _, moved := range []bool{false, true} {
		f := newClaudeClaimFake(t, map[string]*claudeClaimProfile{
			"held":  {week: 99.6, fable: 20, grants: []bool{false}, usableEarly: true, roll: 48 * time.Hour},
			"spare": {week: 40, fable: 10, roll: 100 * time.Hour},
		})
		d := f.daemon(t, "")
		d.server.AccountRef.SetUsageHistory(claudeWeekBurns(t, map[string]float64{"spare": 1.2, "held": 3.0}))
		d.pinSession("conversation", "held")
		if moved {
			f.set("held", func(p *claudeClaimProfile) { p.roll = 23 * time.Hour })
		}
		d.server.autoResetReadings(context.Background())
		settle(t, d.server)
		claims, _ := f.counts()
		decision := d.decision("held")
		if !moved {
			if claims != 1 || decision.Verdict != AutoResetClaimed {
				t.Fatalf("calibration, Claude: %d claims, decision %+v; want the week reset claimed", claims, decision)
			}
			continue
		}
		if claims != 0 || decision.Verdict != AutoResetFailed ||
			!strings.Contains(decision.Reason, "nothing was sent: §103 holds it on the reading before the claim: the week rolls in 23h, under 24h") {
			t.Fatalf("Claude, the live roll 23 hours away: %d claims, decision %+v; want nothing sent", claims, decision)
		}
	}

	for _, moved := range []bool{false, true} {
		f := newCodexClaimFake(t, map[string]*codexClaimAccount{
			heldCodex: {used: 99.6, credits: []accounts.RateLimitResetCredit{autoResetTestCredit("credit", 20*burnDay)}},
		})
		now := time.Now().Add(-100 * time.Hour).Truncate(time.Second)
		h := burnHistory(t, now)
		seedSeries(h, accounts.Account{ID: heldCodex, Provider: accounts.ProviderCodex}, now, 49*time.Hour, 0,
			codexRising(line(0.10, 1.3, 49*time.Hour), fixedRoll(now.Add(3*burnDay))))
		f.server.AccountRef.SetUsageHistory(h)
		f.pinSession("warm", heldCodex)
		if moved {
			f.set(heldCodex, func(a *codexClaimAccount) { a.resetAfter = 23 * 3600 })
		}
		f.server.autoResetReadings(context.Background())
		settle(t, f.server)
		consumes, _ := f.counts()
		decision := f.decision(heldCodex)
		if !moved {
			if consumes != 1 || decision.Verdict != AutoResetClaimed {
				t.Fatalf("calibration, Codex: %d consumes, decision %+v; want the credit redeemed", consumes, decision)
			}
			continue
		}
		if consumes != 0 || decision.Verdict != AutoResetFailed || !strings.Contains(decision.Reason, "nothing was sent") ||
			!strings.Contains(decision.Reason, "§103 holds it on the reading before the claim: the week rolls in 23h, under 24h") {
			t.Fatalf("Codex, the live roll 23 hours away: %d consumes, decision %+v; want nothing sent", consumes, decision)
		}
	}
}

// At the wall the Claude claim reads nothing extra when the daemon's cached
// block offers the grant; here it does not (the grant was not usable when
// the account was read at 95%), so the claim reads the account live, and that
// read puts the week's roll 23 hours away. The decision went by the row's
// roll and passed the gate; the claim's last check judges it again on the
// read it goes by and sends nothing, and the turn moves on. The calibration
// run, whose live read keeps the roll 48 hours away, claims in place.
func TestARealClaimAtTheWallIsHeldWhenItsReadPutsTheRollUnderADay(t *testing.T) {
	setAutoResetWorthForTest(t, true)
	setAutoResetForTest(t, AutoResetReal, true)
	for _, moved := range []bool{false, true} {
		f := newClaudeClaimFake(t, map[string]*claudeClaimProfile{
			"held":  {week: 95, fable: 20, grants: []bool{false}, roll: 48 * time.Hour},
			"spare": {week: 40, fable: 10, roll: 100 * time.Hour},
		})
		d := f.daemon(t, "")
		d.server.AccountRef.SetUsageHistory(claudeWeekBurns(t, map[string]float64{"spare": 1.2, "held": 3.0}))
		d.pinSession("conversation", "held")
		f.set("held", func(p *claudeClaimProfile) {
			p.week = 100
			if moved {
				p.roll = 23 * time.Hour
			}
		})
		answer := d.turn("conversation", opusModel)
		claims, served := f.counts()
		decision := d.decision("held")
		if !moved {
			if claims != 1 || decision.Verdict != AutoResetClaimed || !reflect.DeepEqual(served, []string{"held 429", "held 200"}) {
				t.Fatalf("calibration at the wall: %d claims, served %v, decision %+v; want the week reset claimed in place",
					claims, served, decision)
			}
			continue
		}
		if claims != 0 || decision.Verdict != AutoResetFailed ||
			!strings.Contains(decision.Reason, "nothing was sent: §103 holds it on the reading before the claim: the week rolls in 23h, under 24h") ||
			!reflect.DeepEqual(served, []string{"held 429", "spare 200"}) || !strings.Contains(answer.body, "msg_spare") {
			t.Fatalf("at the wall, the claim's read puts the roll 23 hours away: %d claims, served %v, answer %+v, decision %+v; "+
				"want nothing sent and the turn served on spare", claims, served, answer, decision)
		}
	}
}
