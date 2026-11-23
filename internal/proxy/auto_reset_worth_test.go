package proxy

import (
	"context"
	"maps"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// PATCHES §94.2: an automatic claim is made only when it is worth making.
// The rule's own tests decide on facts; the daemon's tests run its usage
// history and the fakes of §93, which fail on a claim or spend as the
// providers do. No test sends a request to a real provider.

func setAutoResetWorthForTest(t *testing.T, on bool) {
	t.Helper()
	prev := AutoResetWorth()
	SetAutoResetWorth(on)
	t.Cleanup(func() { SetAutoResetWorth(prev) })
}

// worthNow is the clock of the rule's own tests.
var worthNow = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

// worthFixture is a reading moment on an account whose next reset is of
// kind: its meter read a minute ago with left on it and rolling toRoll from
// now, and the reset expiring expires from now.
type worthFixture struct {
	kind            autoResetKind
	left            float64
	toRoll, expires time.Duration
	// pool is the pool's 24-hour pressure on the meter, 0 for none known;
	// own is the account's own 24-hour burn, 0 for none.
	pool, own float64
	// window opens the kind's expiry window an hour ago.
	window bool
	// restarts, when set, stands for the kinds table's restarts_clock.
	restarts accounts.ClockRestart
}

// facts are the fixture's facts; with rule, §94.2's too.
func (w worthFixture) facts(rule bool) autoResetFacts {
	meter := w.kind.meter()
	f := autoResetFacts{moment: autoResetOnAReading, readingMeter: meter, demand: true, nearEmpty: true, now: worthNow,
		next: autoResetNext{kind: w.kind, id: "the_reset", expiresAt: worthNow.Add(w.expires).Format(time.RFC3339)},
		readings: map[autoResetMeter]autoResetReading{
			meter: {left: w.left, at: worthNow.Add(-time.Minute), roll: worthNow.Add(w.toRoll)},
		}}
	if !rule {
		return f
	}
	provider := accounts.ProviderClaude
	if w.kind == autoResetKindCodexCredit {
		provider = accounts.ProviderCodex
	}
	spec, _ := accounts.ResetKindNamed(provider, w.kind.tableKind())
	pressure := map[string]Pressure{}
	if w.pool > 0 {
		pressure["24h"] = measured(w.pool)
	}
	worth := autoResetWorth{label: usageMeterLabel(string(provider), string(spec.Meter)), need: needOf(pressure, nil),
		restarts: spec.RestartsClock, roll: worthNow.Add(w.toRoll), expires: worthNow.Add(w.expires)}
	if w.restarts != "" {
		worth.restarts = w.restarts
	}
	if w.own > 0 {
		worth.own = UsageMeterBurn{"24h": {Burn: figure(w.own), Covered: 1}}
	}
	if w.window {
		worth.window = &accounts.ResetExpiryWindow{OpensAt: worthNow.Add(-time.Hour), LeadSeconds: int64((w.expires + time.Hour) / time.Second)}
	}
	f.worth = &worth
	return f
}

// The fixtures of the expected behaviour, a week reset at 0.4% left unless
// named otherwise.
var (
	weekAt07      = worthFixture{kind: autoResetKindWeek, left: 0.004, toRoll: 60 * time.Hour, expires: 17 * burnDay, pool: 0.7}
	weekAt12      = worthFixture{kind: autoResetKindWeek, left: 0.004, toRoll: 60 * time.Hour, expires: 17 * burnDay, pool: 1.2}
	weekNearRoll  = worthFixture{kind: autoResetKindWeek, left: 0.004, toRoll: 10 * time.Hour, expires: 17 * burnDay, pool: 1.2, own: 1.2}
	weekFarRoll   = worthFixture{kind: autoResetKindWeek, left: 0.004, toRoll: 40 * time.Hour, expires: 17 * burnDay, pool: 1.2, own: 1.2}
	weekNoFigure  = worthFixture{kind: autoResetKindWeek, left: 0.004, toRoll: 60 * time.Hour, expires: 17 * burnDay}
	creditFarRoll = worthFixture{kind: autoResetKindCodexCredit, left: 0.004, toRoll: 50 * time.Hour, expires: 20 * burnDay, pool: 1.3}
	creditNear    = worthFixture{kind: autoResetKindCodexCredit, left: 0.004, toRoll: 30 * time.Hour, expires: 20 * burnDay, pool: 1.3}
)

func expiring(w worthFixture, in time.Duration) worthFixture { w.expires = in; return w }
func windowOpen(w worthFixture) worthFixture                 { w.window = true; return w }
func withLeft(w worthFixture, left float64) worthFixture     { w.left = left; return w }

type worthCase struct {
	name    string
	fixture worthFixture
	verdict AutoResetVerdict
	reason  []string
}

func checkWorth(t *testing.T, cases []worthCase) {
	t.Helper()
	for _, c := range cases {
		got := decideAutoReset(c.fixture.facts(true))
		if got.Verdict != c.verdict {
			t.Errorf("%s: %s, want %s: %s", c.name, got.Verdict, c.verdict, got.Reason)
			continue
		}
		for _, words := range c.reason {
			if !strings.Contains(got.Reason, words) {
				t.Errorf("%s: the reason does not say %q: %s", c.name, words, got.Reason)
			}
		}
	}
}

// 1, 2 and 4, on facts. A pool at pressure 0.7 does not need the reset: held
// back with the pressure. At 1.2 with 60 hours to the roll a claim adds 0.43
// of a week: claimed. At 1.2 with the account's own burn 1.2 and 10 hours to
// the roll a claim adds 0.07: held back with the hours and the share; with 40
// hours, 0.29: claimed. No figure at all: no claim, the burn not known yet.
func TestTheRuleClaimsOnlyWhenThePoolNeedsItAndItAddsEnough(t *testing.T) {
	checkWorth(t, []worthCase{
		{"pressure 0.7", weekAt07, AutoResetHeldBack, []string{"held back", "the 24-hour pressure is 0.70, under 1.0",
			"the rest of the pool can carry the work"}},
		{"pressure 1.2, 60 hours to the roll", weekAt12, AutoResetWouldClaim, []string{"the 24-hour pressure is 1.20, at or above 1.0",
			"a claim adds 0.43 of a week"}},
		{"10 hours to the roll", weekNearRoll, AutoResetHeldBack, []string{"held back: the roll is 10h away",
			"a claim would add 0.07 of a week, under the floor of 0.25"}},
		{"40 hours to the roll", weekFarRoll, AutoResetWouldClaim, []string{"a claim adds 0.29 of a week"}},
		{"no figure", weekNoFigure, AutoResetNoClaim, []string{"the pool's burn on the Claude week is not known yet"}},
	})
}

// 3. Last chance and the expiry window each override the pool's need and the
// value floor, and the reason says which.
func TestLastChanceAndTheExpiryWindowOverrideTheRule(t *testing.T) {
	var cases []worthCase
	for name, held := range map[string]worthFixture{"pressure 0.7": weekAt07, "10 hours to the roll": weekNearRoll} {
		cases = append(cases,
			worthCase{name + ", expiring before the roll", expiring(held, held.toRoll/2), AutoResetWouldClaim, []string{"its last chance"}},
			worthCase{name + ", inside the expiry window", windowOpen(held), AutoResetWouldClaim, []string{"inside its expiry window",
				"any demand at the limit is enough"}},
		)
	}
	// A kind whose clock nobody knows is never claimed, not even inside its
	// window or in its last chance.
	unknownClock := windowOpen(expiring(weekAt12, 5*time.Hour))
	unknownClock.restarts = accounts.ClockRestartUnknown
	cases = append(cases, worthCase{"a kind whose clock is not known", unknownClock, AutoResetNoClaim,
		[]string{"such a reset is never claimed automatically"}})
	// Last chance is the reset expiring before the account could meet its
	// next wall: the roll plus a week at the rate floored at 1.
	cases = append(cases,
		worthCase{"pressure 0.7, expiring 6 days after the roll", expiring(weekAt07, 60*time.Hour+6*burnDay), AutoResetWouldClaim, []string{"its last chance"}},
		worthCase{"pressure 0.7, expiring 8 days after the roll", expiring(weekAt07, 60*time.Hour+8*burnDay), AutoResetHeldBack, nil})
	checkWorth(t, cases)
}

// 5. A Codex credit restarts the seven days: a claim is worth the wait it
// skips, whatever the rate.
func TestACodexCreditIsWorthTheWaitItSkips(t *testing.T) {
	checkWorth(t, []worthCase{
		{"50 hours to the roll at 1.3", creditFarRoll, AutoResetWouldClaim, []string{"the 24-hour pressure is 1.30",
			"a claim adds 0.30 of a week (a claim restarts the seven days and skips the 50h to the roll"}},
		{"30 hours to the roll", creditNear, AutoResetHeldBack, []string{"held back: the roll is 30h away", "0.18 of a week"}},
		{"30 hours, expiring before the roll", expiring(creditNear, 20*time.Hour), AutoResetWouldClaim, []string{"its last chance"}},
	})
}

// 6. The owner's condition comes first: at 2% left every fixture is refused
// by it, whatever the pressure, the value, the window or the expiry.
func TestTheOwnersConditionStillComesFirst(t *testing.T) {
	var cases []worthCase
	for _, w := range []worthFixture{weekAt07, weekAt12, weekNearRoll, weekFarRoll, weekNoFigure, creditFarRoll, creditNear,
		windowOpen(weekAt07), expiring(weekNearRoll, 5*time.Hour), expiring(creditNear, 20*time.Hour)} {
		cases = append(cases, worthCase{string(w.kind) + " at 2% left", withLeft(w, 0.02), AutoResetRefusedByCondition,
			[]string{"refused by the under-1-percent condition", "has 2.0% left"}})
	}
	checkWorth(t, cases)
}

// 7. With the rule off every fixture decides exactly as §93.1 decided it.
func TestWithTheRuleOffEveryFixtureDecidesAsSection93(t *testing.T) {
	for _, w := range []worthFixture{weekAt07, weekAt12, weekNearRoll, weekFarRoll, weekNoFigure, creditFarRoll, creditNear,
		windowOpen(weekAt07), expiring(weekNearRoll, 5*time.Hour), withLeft(weekAt12, 0.02), withLeft(creditNear, 0.02)} {
		got := decideAutoReset(w.facts(false))
		want := AutoResetDecision{Moment: "reading", Verdict: AutoResetWouldClaim, Kind: string(w.kind), Reset: "the_reset",
			ResetExpiresAt: worthNow.Add(w.expires).Format(time.RFC3339), Left: "0.4%"}
		meter := "the week"
		want.Reason = "a reading says the week is 99.6% used; the next reset is " + w.kind.words() + ", which gives back " + meter
		if w.left >= 0.01 {
			want.Verdict, want.Left = AutoResetRefusedByCondition, "2.0%"
			want.Reason = "a reading says the week is 98.0% used; the next reset is " + w.kind.words() + ", which gives back " + meter +
				"; refused by the under-1-percent condition: the week has 2.0% left"
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%+v with the rule off: %+v, want §93's %+v", w, got, want)
		}
	}
}

// A window whose lead is capped at the time left is open from now, whatever
// fraction of a second the clock reads: a rounded lead must not put it
// ahead of the decision that reads it.
func TestAWindowCappedAtTheTimeLeftIsOpenNow(t *testing.T) {
	expires := worthNow.Add(300 * time.Hour)
	demand := resetDemand{label: "Claude week", pressure: measured(0.5), window: "24h", accounts: 1}
	for _, fraction := range []time.Duration{100 * time.Millisecond, 400 * time.Millisecond, 600 * time.Millisecond} {
		now := worthNow.Add(-fraction)
		window := expiryWindow(expires, now, 2, 0.65, demand)
		if now.Before(window.OpensAt) || !strings.Contains(window.Why, "so the window is open") {
			t.Fatalf("now %s: the window opens at %s, after now: %+v", now.Format(time.RFC3339Nano), window.OpensAt.Format(time.RFC3339Nano), window)
		}
	}
}

// worthClaude is a daemon of profiles in mode whose usage history holds
// each account's week rising at its rate in burns over 49 hours: with spare
// alone, the pool's 24-hour pressure on the Claude week is spare's rate. The
// history's clock stands 100 hours before the live readings, which add
// nothing to its windows. With no burns the daemon keeps no history.
func worthClaude(t *testing.T, mode AutoResetMode, burns map[string]float64, profiles map[string]*autoResetProfile) *autoResetClaude {
	t.Helper()
	setAutoResetForTest(t, mode, true)
	d := newAutoResetClaude(t, profiles)
	if len(burns) > 0 {
		now := time.Now().Add(-100 * time.Hour).Truncate(time.Second)
		h := burnHistory(t, now)
		for id, rate := range burns {
			seedSeries(h, accounts.Account{ID: id, Provider: accounts.ProviderClaude}, now, 49*time.Hour, 0,
				func(ago time.Duration, _ time.Time) []accounts.UsageWindow {
					return claudeWeek(100*line(0.10, rate, 49*time.Hour)(ago), now.Add(3*burnDay))
				})
		}
		d.server.AccountRef.SetUsageHistory(h)
	}
	return d
}

// heldAtTheWall is held, read at 95% of a week that rolls in roll (60 hours
// when 0), holding a week reset that expires at ends, at the week's wall; and
// spare.
func heldAtTheWall(ends time.Time, spareGrant bool, roll time.Duration) map[string]*autoResetProfile {
	if roll == 0 {
		roll = 60 * time.Hour
	}
	grant := autoResetWeekGrant("week_reset", 1, false)
	grant["ends_at"] = ends.UTC().Format(time.RFC3339)
	spare := &autoResetProfile{week: 40, fable: 10, roll: roll}
	if spareGrant {
		other := autoResetWeekGrant("spare_reset", 1, false)
		other["ends_at"] = grant["ends_at"]
		spare.block = cedarBlock(other)
	}
	return map[string]*autoResetProfile{
		"held":  {week: 95, fable: 20, block: cedarBlock(grant), turn: claudeWall("7d"), roll: roll},
		"spare": spare,
	}
}

// 1, 3, 4 and 7 through the daemon, in shadow mode at the week's wall: the
// rule reads the pool's pressure from the usage history, the account's roll
// from its reading, and the reset's expiry and window from its block. Each
// turn gets exactly what it gets with automatic resets off, and only a
// would-claim logs a line.
func TestTheDaemonDecidesByThePoolsPressureAtTheWall(t *testing.T) {
	ends := time.Now().Add(17 * burnDay)
	off := worthClaude(t, AutoResetOff, nil, heldAtTheWall(ends, false, 0))
	off.pinSession("conversation", "held")
	today := off.turn("conversation", opusModel, "")
	if today.status != 200 || !strings.Contains(today.body, "msg_spare") {
		t.Fatalf("calibration: with automatic resets off the turn got %+v, want spare's answer", today)
	}
	for _, c := range []struct {
		name  string
		worth bool
		burns map[string]float64
		ends  time.Time
		roll  time.Duration
		// spareGrant gives spare a week reset of the same date, so the
		// expiry window counts two holders.
		spareGrant bool
		verdict    AutoResetVerdict
		reason     []string
	}{
		{name: "pressure 0.7", worth: true, burns: map[string]float64{"spare": 0.7}, ends: ends, verdict: AutoResetHeldBack,
			reason: []string{"held back", "the 24-hour pressure is 0.70, under 1.0", "the rest of the pool can carry the work"}},
		{name: "pressure 1.2, 60 hours to the roll", worth: true, burns: map[string]float64{"spare": 1.2}, ends: ends, verdict: AutoResetWouldClaim,
			reason: []string{"the 24-hour pressure is 1.20, at or above 1.0", "a claim adds 0.43 of a week"}},
		// Held's own 3.0 in a pool at 2.1 adds 0.27 of a week in 15 hours;
		// at the pool's pressure it would add 0.19.
		{name: "its own burn 3.0, 15 hours to the roll", worth: true, burns: map[string]float64{"spare": 1.2, "held": 3.0}, ends: ends,
			roll: 15 * time.Hour, verdict: AutoResetWouldClaim, reason: []string{"the 24-hour pressure is 2.10", "a claim adds 0.27 of a week",
				"its own 24-hour rate"}},
		{name: "no history", worth: true, ends: ends, verdict: AutoResetNoClaim,
			reason: []string{"the pool's burn on the Claude week is not known yet"}},
		{name: "pressure 0.7, expiring before the roll", worth: true, burns: map[string]float64{"spare": 0.7}, ends: time.Now().Add(50 * time.Hour),
			verdict: AutoResetWouldClaim, reason: []string{"its last chance"}},
		// Two holders with 0.65 of a week left between them, at 0.5 a week,
		// need 1.3 weeks: 1.5 times that is more than the 300 hours left.
		{name: "pressure 0.5, inside the expiry window", worth: true, burns: map[string]float64{"spare": 0.5}, ends: time.Now().Add(300 * time.Hour),
			spareGrant: true, verdict: AutoResetWouldClaim, reason: []string{"inside its expiry window"}},
		{name: "pressure 0.7, the rule off", burns: map[string]float64{"spare": 0.7}, ends: ends, verdict: AutoResetWouldClaim},
	} {
		t.Run(c.name, func(t *testing.T) {
			setAutoResetWorthForTest(t, c.worth)
			d := worthClaude(t, AutoResetShadow, c.burns, heldAtTheWall(c.ends, c.spareGrant, c.roll))
			d.pinSession("conversation", "held")
			answer := d.turn("conversation", opusModel, "")
			if !reflect.DeepEqual(answer, today) {
				t.Fatalf("the turn got %+v; with automatic resets off it got %+v", answer, today)
			}
			decision, _ := d.decision("held")
			if decision.Verdict != c.verdict || decision.Kind != "week" || decision.Moment != "wall" {
				t.Fatalf("decision %+v, want %s on the week reset at the wall", decision, c.verdict)
			}
			for _, words := range c.reason {
				if !strings.Contains(decision.Reason, words) {
					t.Fatalf("the reason does not say %q: %s", words, decision.Reason)
				}
			}
			if !c.worth && decision.Reason != "a request met the limit of the week; the next reset is a week reset, which gives back the week" {
				t.Fatalf("with the rule off the reason is %q, want §93's", decision.Reason)
			}
			if lines := wouldClaimLines(d.logs); len(lines) != map[bool]int{true: 1}[c.verdict == AutoResetWouldClaim] {
				t.Fatalf("%d would-claim lines for %s:\n%s", len(lines), c.verdict, d.logs.String())
			}
		})
	}
}

// switchedHistory is a usage history whose clock the test moves: spare's
// week rising at 0.7 a week for the 49 hours to t1, and at 2.0 for the 13
// hours after. At t1 the pool's 24-hour pressure is 0.70; at t1 plus 13
// hours, 1.40. Its clock stands 100 hours before the live readings.
func switchedHistory(t *testing.T) (*UsageHistory, func(later bool)) {
	t.Helper()
	t1 := time.Now().Add(-100 * time.Hour).Truncate(time.Second)
	var at atomic.Int64
	at.Store(t1.UnixNano())
	h, _, _ := testUsageHistory(t, withUsageHistoryClock(func() time.Time { return time.Unix(0, at.Load()) }))
	start := t1.Add(-49 * time.Hour)
	roll := t1.Add(3 * burnDay)
	for when := start; !when.After(t1.Add(13 * time.Hour)); when = when.Add(10 * time.Minute) {
		used := 0.10 + 0.7*weekShare(when.Sub(start))
		if when.After(t1) {
			used = 0.10 + 0.7*weekShare(t1.Sub(start)) + 2.0*weekShare(when.Sub(t1))
		}
		h.observe(accounts.Account{ID: "spare", Provider: accounts.ProviderClaude}, claudeWeek(100*used, roll), when)
	}
	return h, func(later bool) {
		if later {
			at.Store(t1.Add(13 * time.Hour).UnixNano())
			return
		}
		at.Store(t1.UnixNano())
	}
}

// 8. Real mode. At the wall a held-back decision claims nothing and holds no
// turn, which is moved as with automatic resets off; a claim decision claims
// in place as §93.2 does. On readings a held-back account is decided again
// at its next reading: when the pool's pressure rises past 1.0 between two
// readings, the second claims.
func TestRealModeHoldsNothingForAHeldBackDecisionAndDecidesAgainAtTheNextReading(t *testing.T) {
	setAutoResetWorthForTest(t, true)
	setAutoResetForTest(t, AutoResetReal, true)
	for _, c := range []struct {
		later  bool
		claims int
		served []string
		on     string
	}{
		{false, 0, []string{"held 429", "spare 200"}, "spare"},
		{true, 1, []string{"held 429", "held 200"}, "held"},
	} {
		f := newClaudeClaimFake(t, heldWeek())
		d := f.daemon(t, "")
		h, move := switchedHistory(t)
		move(c.later)
		d.server.AccountRef.SetUsageHistory(h)
		d.pinSession("conversation", "held")
		f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
		answer := d.turn("conversation", opusModel)
		claims, served := f.counts()
		if claims != c.claims || !reflect.DeepEqual(served, c.served) || d.sessionAccount("conversation") != c.on ||
			!strings.Contains(answer.body, "msg_"+c.on) {
			t.Fatalf("pressure risen %v: %d claims, served %v, session on %q, answer %+v; want %d claims, %v and the session on %s",
				c.later, claims, served, d.sessionAccount("conversation"), answer, c.claims, c.served, c.on)
		}
		want := map[bool]AutoResetVerdict{false: AutoResetHeldBack, true: AutoResetClaimed}[c.later]
		if decision := d.decision("held"); decision.Verdict != want {
			t.Fatalf("pressure risen %v: decision %+v, want %s", c.later, decision, want)
		}
	}

	// Readings: held at 99.6% with a grant usable now, a session on it.
	f := newClaudeClaimFake(t, map[string]*claudeClaimProfile{
		"held":  {week: 99.6, fable: 20, grants: []bool{false}, usableEarly: true},
		"spare": {week: 40, fable: 10},
	})
	d := f.daemon(t, "")
	h, move := switchedHistory(t)
	d.server.AccountRef.SetUsageHistory(h)
	d.pinSession("conversation", "held")
	d.server.autoResetReadings(context.Background())
	settle(t, d.server)
	if claims, _ := f.counts(); claims != 0 || d.decision("held").Verdict != AutoResetHeldBack {
		t.Fatalf("the first reading at pressure 0.70: %d claims, decision %+v; want held back", claims, d.decision("held"))
	}
	// The same reading again decides nothing new.
	d.server.autoResetReadings(context.Background())
	settle(t, d.server)
	move(true)
	d.measure()
	d.server.autoResetReadings(context.Background())
	settle(t, d.server)
	if claims, _ := f.counts(); claims != 1 || d.decision("held").Verdict != AutoResetClaimed {
		t.Fatalf("the next reading at pressure 1.40: %d claims, decision %+v; want the week reset claimed", claims, d.decision("held"))
	}
}

// 9. Real mode, Claude, a reading moment: the account is read live before the
// claim, and a week that rolled since the reading is not claimed on. At the
// wall nothing more is read: the claim goes by the cached block.
func TestARealClaudeClaimAtAReadingReadsTheMeterLiveFirst(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	for _, rolled := range []bool{true, false} {
		f := newClaudeClaimFake(t, map[string]*claudeClaimProfile{"held": {week: 99.6, fable: 20, grants: []bool{false}, usableEarly: true}})
		d := f.daemon(t, "")
		d.pinSession("conversation", "held")
		if rolled {
			// The week rolls after the daemon's reading.
			f.set("held", func(p *claudeClaimProfile) { p.week = 0 })
		}
		before := f.usageReads()
		d.server.autoResetReadings(context.Background())
		settle(t, d.server)
		claims, _ := f.counts()
		decision := d.decision("held")
		if rolled {
			if claims != 0 || decision.Verdict != AutoResetFailed ||
				!strings.Contains(decision.Reason, "nothing was sent: the live reading before the claim no longer says the week is at least 99.5% used") {
				t.Fatalf("a week rolled since the reading: %d claims, decision %+v; want none sent", claims, decision)
			}
			continue
		}
		if claims != 1 || decision.Verdict != AutoResetClaimed {
			t.Fatalf("a week still at 99.6%%: %d claims, decision %+v; want the claim", claims, decision)
		}
		// One live read before the claim and the re-read after it.
		if reads := f.usageReads() - before; reads != 2 {
			t.Fatalf("the claim at a reading read usage %d times, want 2", reads)
		}
	}

	f := newClaudeClaimFake(t, map[string]*claudeClaimProfile{
		"held":  {week: 95, fable: 20, grants: []bool{false}, usableEarly: true},
		"spare": {week: 40, fable: 10},
	})
	d := f.daemon(t, "")
	d.pinSession("conversation", "held")
	f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
	before := f.usageReads()
	d.turn("conversation", opusModel)
	claims, served := f.counts()
	reads := f.usageReads() - before
	if claims != 1 || !reflect.DeepEqual(served, []string{"held 429", "held 200"}) || reads != 1 {
		t.Fatalf("at the wall: %d claims, served %v, %d usage reads; want one claim, the turn served in place and only the re-read after the claim",
			claims, served, reads)
	}
}

func (f *claudeClaimFake) usageReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// 10. A landed claim's record is compared with its kind's entry in the kinds
// table. A Codex redeem whose roll did not move, a Claude week claim whose
// roll moved, and a Codex redeem whose week did not fall each log one Warn
// naming the kind, what the table says and what the claim showed. A record
// that agrees logs nothing, and the table is not changed.
func TestAClaimRecordThatContradictsTheTableWarns(t *testing.T) {
	at := time.Now().Truncate(time.Second)
	roll := at.Add(3 * burnDay)
	codexWeek := func(used float64, roll time.Time, read time.Time) []accounts.UsageWindow {
		return codexWeekRead(used, int64(roll.Sub(read)/time.Second), read)
	}
	after := at.Add(time.Minute)
	for _, c := range []struct {
		name          string
		provider      accounts.Provider
		kind          string
		before, after []accounts.UsageWindow
		table, claim  string
	}{
		{"a Codex redeem whose roll did not move", accounts.ProviderCodex, "codex_rate_limits",
			codexWeek(100, roll, at), codexWeek(0, roll, after), "restarts_clock assumed_yes", "the week roll stayed at"},
		{"a Claude week claim whose roll moved", accounts.ProviderClaude, "week",
			claudeWeek(100, roll), claudeWeek(3, roll.Add(4*burnDay)), "restarts_clock measured_no", "the week roll moved from"},
		{"a Codex redeem whose week did not fall", accounts.ProviderCodex, "codex_rate_limits",
			codexWeek(100, roll, at), codexWeek(100, after.Add(7*burnDay-time.Hour), after), "gives back week (weekly)", "the week did not fall"},
		{"a Codex redeem that restarted the week", accounts.ProviderCodex, "codex_rate_limits",
			codexWeek(100, roll, at), codexWeek(0, after.Add(7*burnDay-time.Hour), after), "", ""},
		{"a Claude week claim that kept the roll", accounts.ProviderClaude, "week",
			claudeWeek(100, roll), claudeWeek(3, roll), "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, logs, _ := testUsageHistory(t)
			account := accounts.Account{ID: "acct", Provider: c.provider}
			h.observe(account, c.before, at)
			draft := h.startClaim(account, c.kind, at.Add(30*time.Second))
			h.finishClaim(draft, "reset", true)
			h.observe(account, c.after, after)
			lines := jsonLogLines(logs, claimContradictsTableMessage)
			if c.table == "" {
				if len(lines) != 0 || len(historyClaims(h)) != 1 {
					t.Fatalf("an agreeing record logged %v (claims %d), want nothing", lines, len(historyClaims(h)))
				}
				return
			}
			if len(lines) != 1 {
				t.Fatalf("%d contradiction lines, want 1:\n%s", len(lines), logs.String())
			}
			line := lines[0]
			table, _ := line["table"].(string)
			claim, _ := line["claim"].(string)
			if line["level"] != "WARN" || line["kind"] != c.kind || !strings.Contains(table, c.table) || !strings.Contains(claim, c.claim) {
				t.Fatalf("line %v, want a Warn naming %s, %q and %q", line, c.kind, c.table, c.claim)
			}
		})
	}
	for provider, want := range map[accounts.Provider]map[accounts.ResetKind]accounts.ClockRestart{
		accounts.ProviderCodex:  {accounts.ResetKindCodexRateLimits: accounts.ClockRestartAssumedYes},
		accounts.ProviderClaude: {accounts.ResetKindClaudeWeek: accounts.ClockRestartMeasuredNo},
	} {
		for kind, clock := range want {
			if spec, _ := accounts.ResetKindNamed(provider, kind); spec.RestartsClock != clock {
				t.Fatalf("the table's %s %s reads %s after the claims, want %s unchanged", provider, kind, spec.RestartsClock, clock)
			}
		}
	}
}

// 11. In real mode the two older automatic Codex paths spend nothing: the
// failover redeem of SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET redeems no credit,
// and the background spender in its spending mode only advises, as in warn
// mode. In shadow mode both behave as before.
func TestRealModeTurnsTheOlderCodexSpendersOff(t *testing.T) {
	for _, mode := range []AutoResetMode{AutoResetShadow, AutoResetReal} {
		setAutoResetForTest(t, mode, true)
		spends := mode != AutoResetReal

		var consume int
		server, _ := cookedPoolServer(t, true, &consume)
		if _, ok := server.redeemForFailover(context.Background(), accounts.ProviderCodex, "codex", "s1", "", nil); ok != spends || (consume == 1) != spends {
			t.Fatalf("mode %s: the failover redeem revived %v with %d consumes, want a redeem %v", mode, ok, consume, spends)
		}

		spender, consumed := fakeResetUpstream(t, w7Fixtures())
		results := spender.spendResetCredits(t.Context(), time.Now(), ResetCreditAutospendSpend, nil)
		want := map[string]int{}
		if spends {
			want = map[string]int{"lapsing@example.com": 1, "early-cook@example.com": 1}
		}
		if got := consumed(); !maps.Equal(got, want) {
			t.Fatalf("mode %s: the spender in spend mode consumed %v, want %v", mode, got, want)
		}
		for _, res := range results {
			if res.Reset == !spends || res.DryRun == spends {
				t.Fatalf("mode %s: result %+v", mode, res)
			}
		}
		advice := spender.AccountRef.withResetAdvice([]AccountUsageStatus{
			{AccountStatus: AccountStatus{ID: "early-cook@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth}},
		})
		if (advice[0].ResetAdvice != nil) == spends {
			t.Fatalf("mode %s: advice %+v", mode, advice[0].ResetAdvice)
		}
	}
}
