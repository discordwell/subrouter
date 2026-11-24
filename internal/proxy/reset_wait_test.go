package proxy

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// PATCHES §103: a claim §94's rule would make waits when the same reset would
// be worth clearly more at the account's next wall, and §95's choice follows
// the same rule. The rule's own tests decide on facts; every provider in the
// daemon's tests is a fake.

func setAutoResetWaitForBetterForTest(t *testing.T, on bool) {
	t.Helper()
	prev := AutoResetWaitForBetter()
	SetAutoResetWaitForBetter(on)
	t.Cleanup(func() { SetAutoResetWaitForBetter(prev) })
}

// liveFable is the automatic claim of 2026-11-24 as facts: a Fable reset
// expiring in 15 weeks, the Fable meter spent, 6h23m to the roll, the
// account's own 24-hour rate 7.07, and the pool needing its Fable resets.
var liveFable = worthFixture{kind: autoResetKindFable, left: 0, toRoll: 6*time.Hour + 23*time.Minute, expires: 15 * 7 * burnDay, pool: 1.2, own: 7.07}

// liveFableWall is that account's next wall: a fresh week at 18:23 spent at
// 7.07 is spent again 168/7.07 hours later, 23h45m45s after the roll.
const liveFableWall = "2026-11-24T18:08:45Z"

// waiting is the fixture's facts with §103's rule on: the account holds held
// resets of the kind through the reset's expiry, and the last decision on
// the reset let a claim through the rule when stood.
func waiting(w worthFixture, held int, stood bool) autoResetFacts {
	f := w.facts(true)
	f.worth.waitForBetter, f.worth.held, f.worth.stood = true, held, stood
	return f
}

func withRoll(w worthFixture, toRoll time.Duration) worthFixture { w.toRoll = toRoll; return w }
func withOwn(w worthFixture, own float64) worthFixture           { w.own = own; return w }

// withWindowAt opens the reset's expiry window at opens from now.
func withWindowAt(f autoResetFacts, opens time.Duration) autoResetFacts {
	f.worth.window = &accounts.ResetExpiryWindow{OpensAt: worthNow.Add(opens), LeadSeconds: int64((f.worth.expires.Sub(worthNow) - opens) / time.Second)}
	return f
}

func wantDecision(t *testing.T, what string, got AutoResetDecision, verdict AutoResetVerdict, by AutoResetRuleStep, words ...string) {
	t.Helper()
	if got.Verdict != verdict || got.Rule == nil || got.Rule.DecidedBy != by {
		t.Fatalf("%s: %s decided by %+v, want %s decided by %s: %s", what, got.Verdict, got.Rule, verdict, by, got.Reason)
	}
	wantWords(t, what, got.Reason, words...)
}

func laterOf(t *testing.T, what string, d AutoResetDecision) (float64, string) {
	t.Helper()
	if d.Rule == nil || d.Rule.LaterValue == nil || d.Rule.LaterWallAt == nil {
		t.Fatalf("%s: the rule carries no later wall: %+v", what, d.Rule)
	}
	return *d.Rule.LaterValue, d.Rule.LaterWallAt.UTC().Format(time.RFC3339)
}

// 1. The live case. §94 alone claims, adding 0.27 of a week. With §103 the
// claim waits: 0.27 now against about a week at the account's next wall,
// about a day after its roll, and the reset lasts 15 weeks.
func TestTheLiveFableClaimWaitsForTheAccountsNextWall(t *testing.T) {
	alone := decideAutoReset(liveFable.facts(true))
	wantDecision(t, "§94 alone", alone, AutoResetWouldClaim, AutoResetByValue, "a claim adds 0.27 of a week",
		"7.07 (its own 24-hour rate)")
	if alone.Rule.LaterValue != nil || strings.Contains(alone.Reason, "next wall") {
		t.Fatalf("§94 alone compared with a later wall: %+v: %s", alone.Rule, alone.Reason)
	}

	got := decideAutoReset(waiting(liveFable, 1, false))
	wantDecision(t, "with §103", got, AutoResetHeldBack, AutoResetByLaterWall,
		"held back: a claim adds 0.27 of a week now, and the same reset is expected to add 1.00 at the account's next wall at "+liveFableWall,
		"23h46m after its roll at 2026-11-23T18:23:00Z", "the reset lasts until 2027-03-08T12:00:00Z",
		"the account can expect 15 such walls before then", "so it waits for that wall")
	if value, at := laterOf(t, "with §103", got); value != 1 || at != liveFableWall {
		t.Fatalf("the rule carries the later wall %.3f at %s, want 1.000 at %s", value, at, liveFableWall)
	}
	if got.Rule.Value == nil || math.Abs(*got.Rule.Value-0.269) > 0.001 {
		t.Fatalf("the rule carries the value now %v, want 0.269", got.Rule.Value)
	}
}

// 2. The later value. A reset that keeps the roll is worth min(1, rate-1) at
// the next wall, one that restarts the seven days 1-1/rate: at 7, 1.0 and
// 0.86; at 1.5, 0.5 and 0.33. At a rate of 1 or under, or unknown, there is
// no later wall.
func TestTheValueOfTheSameResetAtTheNextWall(t *testing.T) {
	roll := worthNow.Add(10 * time.Hour)
	for _, tc := range []struct {
		name     string
		restarts accounts.ClockRestart
		rate     claimRate
		want     float64
		after    time.Duration
	}{
		{"keeps the roll, rate 7", accounts.ClockRestartMeasuredNo, claimRate{value: 7}, 1, 24 * time.Hour},
		{"restarts the seven days, rate 7", accounts.ClockRestartAssumedYes, claimRate{value: 7}, 0.857, 24 * time.Hour},
		{"keeps the roll, rate 1.5", accounts.ClockRestartMeasuredNo, claimRate{value: 1.5}, 0.5, 112 * time.Hour},
		{"restarts the seven days, rate 1.5", accounts.ClockRestartAssumedYes, claimRate{value: 1.5}, 0.333, 112 * time.Hour},
		{"a pool all at its limit", accounts.ClockRestartAssumedYes, claimRate{unbounded: true}, 1, 0},
	} {
		later, ok := laterWallOf(tc.restarts, roll, tc.rate)
		if !ok || math.Abs(later.value-tc.want) > 0.0005 || !later.at.Equal(roll.Add(tc.after)) {
			t.Errorf("%s: later wall %+v (%v), want %.3f at the roll plus %s", tc.name, later, ok, tc.want, tc.after)
		}
	}
	for _, rate := range []claimRate{{value: 1}, {value: 0.8}, claimRateOf(nil, Pressure{})} {
		if later, ok := laterWallOf(accounts.ClockRestartMeasuredNo, roll, rate); ok {
			t.Errorf("rate %s: a later wall %+v, want none", rate, later)
		}
	}

	// Through the decision: what it publishes, and at a rate of 1 nothing
	// but §94's decision.
	for _, tc := range []struct {
		name    string
		fixture worthFixture
		verdict AutoResetVerdict
		later   float64
	}{
		{"a Fable reset at its own rate 7", withOwn(liveFable, 7), AutoResetHeldBack, 1},
		{"a Codex credit at its own rate 7", withOwn(creditFarRoll, 7), AutoResetHeldBack, 0.857},
		{"a Fable reset at its own rate 1.5, 60 hours to the roll", withRoll(withOwn(liveFable, 1.5), 60*time.Hour), AutoResetWouldClaim, 0.5},
		{"a Codex credit at its own rate 1.5", withOwn(creditFarRoll, 1.5), AutoResetWouldClaim, 0.333},
	} {
		got := decideAutoReset(waiting(tc.fixture, 1, false))
		if value, _ := laterOf(t, tc.name, got); got.Verdict != tc.verdict || math.Abs(value-tc.later) > 0.0005 {
			t.Errorf("%s: %s with a later value %.3f, want %s with %.3f: %s", tc.name, got.Verdict, value, tc.verdict, tc.later, got.Reason)
		}
	}
	atOne := withOwn(weekAt12, 0)
	atOne.pool = 1.0
	if on, off := decideAutoReset(waiting(atOne, 1, false)), decideAutoReset(atOne.facts(true)); !reflect.DeepEqual(on, off) || on.Verdict != AutoResetWouldClaim {
		t.Fatalf("at a rate of 1.0 the rule decided %+v, want §94's %+v", on, off)
	}
}

// 3. The comparison: 0.5 now against 1.0 at the next wall waits; 0.85
// against 1.0 claims; 0.3 against 0.33 claims.
func TestAClaimWaitsWhenItAddsUnderFourFifthsOfTheNextWall(t *testing.T) {
	at7 := withOwn(liveFable, 7)
	creditAt15 := withRoll(withOwn(creditFarRoll, 1.5), 50*time.Hour+24*time.Minute)
	for _, tc := range []struct {
		name    string
		fixture worthFixture
		verdict AutoResetVerdict
		by      AutoResetRuleStep
		words   []string
	}{
		{"0.5 against 1.0", withRoll(at7, 12*time.Hour), AutoResetHeldBack, AutoResetByLaterWall,
			[]string{"a claim adds 0.50 of a week now, and the same reset is expected to add 1.00"}},
		{"0.85 against 1.0", withRoll(at7, 20*time.Hour+24*time.Minute), AutoResetWouldClaim, AutoResetByValue,
			[]string{"a claim adds 0.85 of a week", "the same reset is expected to add 1.00, and 0.85 now is not under 0.80 of that"}},
		{"0.3 against 0.33", creditAt15, AutoResetWouldClaim, AutoResetByValue,
			[]string{"a claim adds 0.30 of a week", "the same reset is expected to add 0.33, and 0.30 now is not under 0.80 of that"}},
	} {
		wantDecision(t, tc.name, decideAutoReset(waiting(tc.fixture, 1, false)), tc.verdict, tc.by, tc.words...)
	}
}

// 4. The reset must still be there at the next wall. Expiring 12 hours after
// the roll, before the next wall, it is claimed now (its last chance). With
// the next wall inside the reset's expiry window, it is claimed now, as §94
// decides. Last chance and an open window still claim at any value.
func TestALaterWallCountsOnlyWhileTheResetIsStillThere(t *testing.T) {
	soon := expiring(liveFable, liveFable.toRoll+12*time.Hour)
	wantDecision(t, "expiring 12 hours after the roll", decideAutoReset(waiting(soon, 1, false)), AutoResetWouldClaim, AutoResetByLastChance,
		"its last chance")

	inWindow := decideAutoReset(withWindowAt(waiting(liveFable, 1, false), liveFable.toRoll+20*time.Hour))
	wantDecision(t, "the next wall inside the expiry window", inWindow, AutoResetWouldClaim, AutoResetByValue, "a claim adds 0.27 of a week")
	if inWindow.Rule.LaterValue != nil {
		t.Fatalf("a wall inside the window counted: %+v", inWindow.Rule)
	}
	outside := decideAutoReset(withWindowAt(waiting(liveFable, 1, false), liveFable.toRoll+30*time.Hour))
	wantDecision(t, "calibration: the window opening after the next wall", outside, AutoResetHeldBack, AutoResetByLaterWall,
		"the account can expect 1 such wall before its expiry window opens at 2026-11-25T00:23:00Z")

	open := withRoll(liveFable, 2*time.Hour)
	open.window = true
	wantDecision(t, "inside the window, worth 0.08", decideAutoReset(waiting(open, 1, false)), AutoResetWouldClaim, AutoResetByExpiryWindow,
		"inside its expiry window")
	last := expiring(withRoll(liveFable, 2*time.Hour), 3*time.Hour)
	wantDecision(t, "in its last chance, worth 0.08", decideAutoReset(waiting(last, 1, false)), AutoResetWouldClaim, AutoResetByLastChance,
		"its last chance")
}

// 5. More resets than walls. Expiring 300 hours after the roll at 7.07, the
// account can expect two walls first: holding three, it claims now; holding
// two or one, it waits. Holding one with 15 walls before its expiry, it
// waits.
func TestMoreResetsThanWallsClaimNow(t *testing.T) {
	twoWalls := expiring(liveFable, liveFable.toRoll+300*time.Hour)
	three := decideAutoReset(waiting(twoWalls, 3, false))
	wantDecision(t, "three resets, two walls", three, AutoResetWouldClaim, AutoResetByValue, "a claim adds 0.27 of a week")
	if three.Rule.LaterValue != nil {
		t.Fatalf("three resets with two walls compared with a later wall: %+v", three.Rule)
	}
	for _, held := range []int{2, 1} {
		wantDecision(t, "two walls", decideAutoReset(waiting(twoWalls, held, false)), AutoResetHeldBack, AutoResetByLaterWall,
			"the account can expect 2 such walls before then")
	}
	wantDecision(t, "one reset, 15 walls", decideAutoReset(waiting(liveFable, 1, false)), AutoResetHeldBack, AutoResetByLaterWall,
		"the account can expect 15 such walls before then")
}

// 6. The owner's condition comes first: at 2% left the decision is refused
// by it, whatever §103 would say. A pool that does not need the reset, and a
// claim under the value floor, are held back with their own reasons before
// §103 is asked.
func TestTheChecksOfSection94ComeBeforeTheLaterWall(t *testing.T) {
	refused := decideAutoReset(waiting(withLeft(liveFable, 0.02), 1, false))
	if refused.Verdict != AutoResetRefusedByCondition || refused.Rule != nil || !strings.Contains(refused.Reason, "the Fable meter has 2.0% left") {
		t.Fatalf("at 2%% left: %+v", refused)
	}
	notNeeded := liveFable
	notNeeded.pool = 0.7
	wantDecision(t, "need no", decideAutoReset(waiting(notNeeded, 1, false)), AutoResetHeldBack, AutoResetByNeed,
		"held back: the pool does not need its saved resets on the Claude Fable")
	wantDecision(t, "under the floor", decideAutoReset(waiting(withRoll(liveFable, 2*time.Hour), 1, false)), AutoResetHeldBack, AutoResetByValue,
		"held back: the roll is 2h away, and a claim would add 0.08 of a week, under the floor of 0.25")
}

// fableHolder is an account whose next reset is a Fable reset expiring in 15
// weeks, spending its Fable meter at 7.07 alone in the pool, with 2% of its
// Fable meter left: at the pool's whole demand it reaches its limit in 28.5
// minutes.
func fableHolder(toRoll time.Duration) choiceAccount {
	return choiceAccount{id: "held", kind: accounts.ResetKindClaudeFable, week: 0.5, fable: 0.02, toRoll: toRoll, expires: 15 * 7 * burnDay,
		fableBurn: 7.07}
}

// 7. §95's choice follows the rule. Rolling 6h50m away, the holder reaches
// its limit 6h21m before its roll, where a claim adds 0.27 of a week and
// §103 would hold it back for the next wall: it is not chosen, and the
// reason says so. With the rule off it is chosen, as §95 chose it. Rolling
// 30 hours away a claim at its wall adds a week, the rule would claim, and
// it is chosen.
func TestTheChoiceFollowsTheLaterWall(t *testing.T) {
	setAutoResetWaitForBetterForTest(t, true)
	near := fableHolder(6*time.Hour + 50*time.Minute)
	got := choose(choiceNow, resetCodexHold{}, near)
	wantChosen(t, "a claim at its wall would wait", got, selectacct.ResetChoiceFable)
	wantWords(t, "a claim at its wall would wait", got.why["held"], "at the pool's whole demand it reaches its limit in 29m",
		"where a claim would wait for the account's next wall, under 0.80 of what it adds there",
		"a claim adds 0.27 of a week there, and the same reset is expected to add 1.00 at the account's next wall at")

	got = choose(choiceNow, resetCodexHold{}, fableHolder(30*time.Hour))
	wantChosen(t, "a claim at its wall adds a week", got, selectacct.ResetChoiceFable, "held")
	wantWords(t, "a claim at its wall adds a week", got.chosen["held"].Why, "a claim there adds 1.00 of a week",
		"the same reset is expected to add 1.00, and 1.00 there is not under 0.80 of that")

	SetAutoResetWaitForBetter(false)
	got = choose(choiceNow, resetCodexHold{}, near)
	wantChosen(t, "the rule off", got, selectacct.ResetChoiceFable, "held")
	wantWords(t, "the rule off", got.chosen["held"].Why, "a claim there adds 0.27 of a week")
}

// loggedPasses chooses as the daemon's passes do, each from what the one
// before left, and logs each change as the daemon logs it.
type loggedPasses struct {
	gapPasses
	logs *syncBuffer
}

func (p *loggedPasses) pass(t *testing.T, now time.Time, list ...choiceAccount) choiceResult {
	t.Helper()
	key := func(id string) string {
		return autoResetKey(accounts.Account{ID: id, Provider: accounts.ProviderClaude})
	}
	before := map[string]resetChosen{}
	for k, held := range p.before {
		before[k] = resetChosen{choice: held.choice, label: held.label, provider: "claude"}
	}
	got := p.gapPasses.pass(now, nil, list...)
	after, notChosen := map[string]resetChosen{}, map[string]string{}
	for id, choice := range got.chosen {
		after[key(id)] = resetChosen{choice: choice, label: id, provider: "claude"}
	}
	for id, why := range got.why {
		notChosen[key(id)] = why
	}
	Server{Logger: slogJSON(p.logs)}.logResetChoices(AutoResetReal, before, after, notChosen)
	return got
}

func (p *loggedPasses) lines(message string) int { return len(jsonLogLines(p.logs, message)) }

// weekHolderWorth is a week holder alone in the pool at 1.2 with a tenth of
// its week left, so it reaches its limit in 14 hours, rolling where a claim
// at that wall adds value: at 1.2 the same reset adds 0.2 at the next wall,
// so §103 never holds it back.
func weekHolderWorth(value float64) choiceAccount {
	toRoll := 14*time.Hour + time.Duration(value*168/1.2*float64(time.Hour))
	return choiceAccount{id: "held", kind: accounts.ResetKindClaudeWeek, week: 0.1, fable: 0.5, toRoll: toRoll, expires: 17 * burnDay, weekBurn: 1.2}
}

// 8. No flicker at the floor. Chosen at 0.26, the holder stays chosen at
// 0.24 and 0.26, with one chose line and no stop line, and stops at 0.19.
// Not chosen, 0.24 does not choose it and 0.25 does. The same for §103's
// bar: chosen at 0.81 of the next wall's value, it stays chosen at 0.79,
// stops at 0.63, and from there 0.79 does not choose it and 0.80 does.
func TestAChoiceDoesNotFlickerAtItsBars(t *testing.T) {
	setAutoResetWaitForBetterForTest(t, true)
	p := &loggedPasses{logs: &syncBuffer{}}
	at := func(minutes int) time.Time { return choiceNow.Add(time.Duration(minutes) * time.Minute) }
	for i, value := range []float64{0.26, 0.24, 0.26} {
		got := p.pass(t, at(i), weekHolderWorth(value))
		wantChosen(t, "around the floor", got, selectacct.ResetChoiceWeek, "held")
		if value < resetValueFloor {
			wantWords(t, "kept at 0.24", got.chosen["held"].Why, "a claim there adds 0.24 of a week",
				"it is chosen already, and an account chosen stays chosen until a claim at its wall adds under 0.20 of a week or under 0.64 of what the same reset adds at its next wall")
		}
	}
	if chose, stopped := p.lines(resetChooseMessage), p.lines(resetUnchooseMessage); chose != 1 || stopped != 0 {
		t.Fatalf("around the floor: %d chose and %d stop lines, want 1 and 0:\n%s", chose, stopped, p.logs)
	}
	got := p.pass(t, at(3), weekHolderWorth(0.19))
	wantChosen(t, "0.19", got, selectacct.ResetChoiceWeek)
	wantWords(t, "0.19", got.why["held"], "a claim there adds 0.19 of a week, under 0.20, where a chosen account stops, 0.8 of the floor of 0.25")
	if stopped := jsonLogLines(p.logs, resetUnchooseMessage); len(stopped) != 1 || !strings.Contains(stopped[0]["reason"].(string), "under 0.20") {
		t.Fatalf("at 0.19: stop lines %v, want one naming 0.20", stopped)
	}
	got = p.pass(t, at(4), weekHolderWorth(0.24))
	wantChosen(t, "0.24, not chosen", got, selectacct.ResetChoiceWeek)
	wantWords(t, "0.24, not chosen", got.why["held"], "a claim there adds 0.24 of a week, under the floor of 0.25")
	got = p.pass(t, at(5), weekHolderWorth(0.25))
	wantChosen(t, "0.25, not chosen", got, selectacct.ResetChoiceWeek, "held")

	// §103's bar, at 7.07 alone in the pool, where the next wall is worth a
	// week: 2% of the Fable meter left is 29 minutes to the wall.
	p = &loggedPasses{logs: &syncBuffer{}}
	fable := func(value float64) choiceAccount {
		return fableHolder(29*time.Minute + time.Duration(value*168/7.07*float64(time.Hour)))
	}
	for i, value := range []float64{0.81, 0.79, 0.81, 0.79} {
		got := p.pass(t, at(i), fable(value))
		wantChosen(t, "around §103's bar", got, selectacct.ResetChoiceFable, "held")
	}
	if chose, stopped := p.lines(resetChooseMessage), p.lines(resetUnchooseMessage); chose != 1 || stopped != 0 {
		t.Fatalf("around §103's bar: %d chose and %d stop lines, want 1 and 0:\n%s", chose, stopped, p.logs)
	}
	got = p.pass(t, at(4), fable(0.63))
	wantChosen(t, "0.63", got, selectacct.ResetChoiceFable)
	wantWords(t, "0.63", got.why["held"], "under 0.64 of what it adds there")
	got = p.pass(t, at(5), fable(0.79))
	wantChosen(t, "0.79, not chosen", got, selectacct.ResetChoiceFable)
	got = p.pass(t, at(6), fable(0.80))
	wantChosen(t, "0.80, not chosen", got, selectacct.ResetChoiceFable, "held")
}

// 8, for the decision. A decision that waits at 0.79 of the next wall's value
// claims at 0.81, and the next decision on the same reset still claims at
// 0.79; at 0.63 it waits, and from there 0.79 waits too.
func TestADecisionDoesNotFlickerAtTheLaterWallsBar(t *testing.T) {
	at7 := withOwn(liveFable, 7)
	worth := func(value float64) worthFixture { return withRoll(at7, time.Duration(value*24*float64(time.Hour))) }
	for _, tc := range []struct {
		value   float64
		stood   bool
		verdict AutoResetVerdict
	}{
		{0.79, false, AutoResetHeldBack},
		{0.81, false, AutoResetWouldClaim},
		{0.79, true, AutoResetWouldClaim},
		{0.63, true, AutoResetHeldBack},
		{0.79, false, AutoResetHeldBack},
		{0.80, false, AutoResetWouldClaim},
	} {
		if got := decideAutoReset(waiting(worth(tc.value), 1, tc.stood)); got.Verdict != tc.verdict {
			t.Errorf("%.2f after a claim that passed %v: %s, want %s: %s", tc.value, tc.stood, got.Verdict, tc.verdict, got.Reason)
		}
	}
}

// 9, on facts. With the switch at 0 every fixture above decides exactly as
// §94 decided it, field by field, and §95 chooses as it chose.
func TestWithTheSwitchOffEveryFixtureDecidesAsSection94(t *testing.T) {
	at7 := withOwn(liveFable, 7)
	for _, w := range []worthFixture{liveFable, withOwn(liveFable, 1.5), withOwn(creditFarRoll, 7), withRoll(at7, 12*time.Hour),
		withRoll(at7, 20*time.Hour+24*time.Minute), expiring(liveFable, liveFable.toRoll+300*time.Hour),
		expiring(liveFable, liveFable.toRoll+12*time.Hour), withLeft(liveFable, 0.02)} {
		f := waiting(w, 1, true)
		f.worth.waitForBetter = false
		if got, want := decideAutoReset(f), decideAutoReset(w.facts(true)); !reflect.DeepEqual(got, want) || (got.Rule != nil && got.Rule.LaterValue != nil) {
			t.Errorf("%+v with the switch off: %+v, want §94's %+v", w, got, want)
		}
	}
	setAutoResetWaitForBetterForTest(t, false)
	for _, toRoll := range []time.Duration{6*time.Hour + 50*time.Minute, 30 * time.Hour} {
		wantChosen(t, "the switch off", choose(choiceNow, resetCodexHold{}, fableHolder(toRoll)), selectacct.ResetChoiceFable, "held")
	}
	p := &loggedPasses{logs: &syncBuffer{}}
	for i, value := range []float64{0.26, 0.24} {
		got := p.pass(t, choiceNow.Add(time.Duration(i)*time.Minute), weekHolderWorth(value))
		if _, chosen := got.chosen["held"]; chosen != (value >= resetValueFloor) {
			t.Fatalf("the switch off at %.2f: chosen %v, want §95's floor alone", value, chosen)
		}
	}
}

// 5, the count. The resets that need a wall before a reset's expiry are the
// ones of its kind that expire no later, itself among them, and any of the
// kind whose date is not read, which may expire as soon.
func TestTheResetsThatNeedAWallBeforeAnExpiryAreCounted(t *testing.T) {
	march, january := "2027-03-09T07:59:00Z", "2027-01-15T00:00:00Z"
	at := func(text string) time.Time { parsed, _ := time.Parse(time.RFC3339, text); return parsed }
	claude := []accounts.HeldReset{
		{Kind: accounts.ResetKindClaudeWeek, Count: 1, ExpiresAt: "2026-12-10T22:00:00Z"},
		{Kind: accounts.ResetKindClaudeFable, Count: 1, ExpiresAt: january},
		{Kind: accounts.ResetKindClaudeFable, Count: 2, ExpiresAt: march},
	}
	codex := []accounts.HeldReset{
		{Kind: accounts.ResetKindCodexRateLimits, Count: 1, ExpiresAt: "2026-12-14T00:00:00Z"},
		{Kind: accounts.ResetKindCodexRateLimits, Count: 1, ExpiresAt: "2026-12-20T00:00:00Z"},
		{Kind: accounts.ResetKindCodexRateLimits, Count: 2, Expiry: accounts.ResetExpiryNotRead},
	}
	for _, tc := range []struct {
		name    string
		list    []accounts.HeldReset
		kind    accounts.ResetKind
		expires time.Time
		want    int
	}{
		{"the Fable resets through March", claude, accounts.ResetKindClaudeFable, at(march), 3},
		{"the Fable resets through January", claude, accounts.ResetKindClaudeFable, at(january), 1},
		{"the Codex credits through 12/14, with two not read", codex, accounts.ResetKindCodexRateLimits, at("2026-12-14T00:00:00Z"), 3},
		{"no list", nil, accounts.ResetKindClaudeFable, at(march), 1},
	} {
		if got := heldThrough(tc.list, tc.kind, tc.expires); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
}
