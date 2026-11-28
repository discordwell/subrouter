package selectacct

import (
	"math"
	"testing"
	"time"

	"github.com/discordwell/subrouter/account"
)

// PATCHES §110: an account projected to waste budget joins the drain when
// that projection appears, not at 24 hours.

const reasonPaceBurndown = "early burndown: at its present burn its week would roll with budget unspent"

// The incident, 2026-11-27. idle-full read 100% of its week left at every
// reading, 51.46 hours from its roll, with no live session and no saved reset,
// while an account chosen for its week reset with 84% left took the new
// sessions and the pool's other accounts sat days from their rolls. The pace
// tracker measured its own burn at 0 over the last hour; the pool's burn per
// live session-hour was measured in one case (the 51 h log line read
// burn_source=pool, 0.055) and absent in the other (the 36.6 h line read
// burn_per_live=0). In both it must take the new session ahead of the chosen
// account, and publish the burndown with its cause.
func TestAnIdleFullAccountDaysFromItsRollTakesTheNewSessionFirst(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	for _, tc := range []struct {
		name     string
		poolBurn bool
	}{
		{"no pool burn per live session", false},
		{"the pool's burn per live session measured", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			t0 := now.Add(-time.Hour)
			ref, _ := paceRef(now)
			idleRoll := now.Add(time.Duration(51.4588 * float64(time.Hour)))
			chosenRoll := now.Add(100 * time.Hour)
			steadyRoll := now.Add(138 * time.Hour)
			busyRoll := now.Add(120 * time.Hour)
			busy := ScoreKey(account.ProviderClaude, "busy")
			readings := func(at time.Time, busyLeft float64) Scheduler {
				return NewScheduler([]Score{
					reading("idle-full", at, idleRoll, 1),
					reading("chosen", at, chosenRoll, 0.84),
					reading("steady", at, steadyRoll, 1),
					reading("busy", at, busyRoll, busyLeft),
				})
			}
			if tc.poolBurn {
				ref.ObserveLiveSessions(map[string]int{busy: 1}, t0)
			}
			ref.Set(readings(t0, 0.50))
			if tc.poolBurn {
				ref.ObserveLiveSessions(map[string]int{busy: 1}, now)
			}
			ref.Set(readings(now, 0.445))

			pace := refPace(ref, "idle-full")
			if pace.BurnNow != 0 || (pace.BurnPerLive > 0) != tc.poolBurn {
				t.Fatalf("fixture drift: idle-full's pace %+v, want its own burn 0 and a pool burn only when measured", pace)
			}
			scheduler := ref.Get().WithResetChoices(chooseFor(ResetChoiceWeek, 1, account.ProviderClaude, "chosen"))
			accts := []account.Account{claudeAccount("chosen"), claudeAccount("steady"), claudeAccount("idle-full")}

			allowance := scheduler.PaceAllowance(account.ProviderClaude, "idle-full", now)
			if math.Abs(allowance.ProjectedWaste-1) > 1e-9 || !allowance.Danger {
				t.Fatalf("allowance %+v, want projected waste 1 and danger: at a burn of 0 the whole week expires", allowance)
			}
			wantFirst(t, "the incident", scheduler, "idle-full", reasonPaceBurndown, accts...)
			verdict := scheduler.NewSessionVerdict(account.ProviderClaude, "idle-full", now)
			if !verdict.Eligible || verdict.Reason != VerdictBurndown || !verdict.Burndown || verdict.AtCap ||
				verdict.BurndownCause != "pace" || verdict.AllowedSessions < 1 {
				t.Fatalf("verdict %+v, want eligible, reason burndown, cause pace, with an allowance", verdict)
			}
			if got := scheduler.NewSessionVerdict(account.ProviderClaude, "chosen", now); got.Reason != VerdictChosen || got.BurndownCause != "" {
				t.Fatalf("the chosen account's verdict %+v, want chosen with no burndown cause", got)
			}
		})
	}
}

// setEarlyDrainForTest sets §110's process-wide switch for the rest of the
// test and restores it in t.Cleanup. Only for tests that are not t.Parallel.
func setEarlyDrainForTest(t *testing.T, on bool) {
	t.Helper()
	prev := EarlyDrain()
	SetEarlyDrain(on)
	t.Cleanup(func() { SetEarlyDrain(prev) })
}

// ownBurns attaches a measured own burn per account, as the pace tracker
// publishes it for an account with a span and no live session-hours.
func ownBurns(scheduler Scheduler, burns map[string]float64) Scheduler {
	paces := map[string]Pace{}
	for id, burn := range burns {
		paces[ScoreKey(account.ProviderClaude, id)] = Pace{BurnNow: burn, Measured: true}
	}
	return scheduler.withPaces(paces)
}

// paceCase is one candidate against a steady account with a full week
// rolling in 138 hours, which is never in any burndown.
func paceCase(t *testing.T, candidate Score, pace *Pace, configure func(Scheduler) Scheduler) (Scheduler, []account.Account) {
	t.Helper()
	now := time.Now()
	steady := resetScore("steady", 1, 1, now.Add(138*time.Hour), time.Time{})
	scheduler := NewScheduler([]Score{candidate, steady})
	if pace != nil {
		scheduler = scheduler.withPaces(map[string]Pace{ScoreKey(account.ProviderClaude, candidate.AccountID): *pace})
	}
	if configure != nil {
		scheduler = configure(scheduler)
	}
	return scheduler, []account.Account{claudeAccount("steady"), claudeAccount(candidate.AccountID)}
}

// Where the pace burndown does not act. Each candidate would rank first on
// its pace anyway, so the steady account leads only in steady mode; what is
// asserted is the published verdict and, where the drain would have named
// it, the reason.
func TestThePaceBurndownActsOnlyOnAMeasuredLocalAccountInsideItsHorizon(t *testing.T) {
	now := time.Now()
	idle := func(untilRoll time.Duration) Score { return resetScore("idle", 1, 1, now.Add(untilRoll), time.Time{}) }
	measured := func(burn float64) *Pace { return &Pace{BurnNow: burn, Measured: true} }
	notFresh := idle(51 * time.Hour)
	notFresh.Fresh = false
	walled := idle(51 * time.Hour)
	walled.ShortHeadroom = 0
	remote := idle(51 * time.Hour)
	remote.Remote = true
	// A peer's score carries no pace table entry; one set on it by hand is
	// still not read.
	remote.Pace = *measured(0)
	for _, tc := range []struct {
		name      string
		score     Score
		pace      *Pace
		configure func(Scheduler) Scheduler
		reason    string
		cause     string
	}{
		{"73 hours from its roll, past the horizon", idle(73 * time.Hour), measured(0), nil, VerdictEligible, ""},
		{"exactly 24 hours from its roll: the roll", idle(24 * time.Hour), measured(0), nil, VerdictBurndown, BurndownCauseRoll},
		{"23 hours from its roll: the roll", idle(23 * time.Hour), measured(0), nil, VerdictBurndown, BurndownCauseRoll},
		{"a burn that spends the week by the roll", idle(51 * time.Hour), measured(0.019), nil, VerdictEligible, ""},
		{"its own burn unmeasured", idle(51 * time.Hour), nil, nil, VerdictEligible, ""},
		{"a reading that is not fresh", notFresh, measured(0), nil, VerdictEligible, ""},
		{"at its 5h wall", walled, measured(0), nil, VerdictExhausted, ""},
		{"a peer's account with no verdict", remote, nil, nil, VerdictEligible, ""},
		{"chosen for its week reset", idle(51 * time.Hour), measured(0), func(s Scheduler) Scheduler {
			return s.WithResetChoices(chooseFor(ResetChoiceWeek, 1, account.ProviderClaude, "idle"))
		}, VerdictChosen, ""},
		{"with the switch off", idle(51 * time.Hour), measured(0), func(s Scheduler) Scheduler {
			setEarlyDrainForTest(t, false)
			return s
		}, VerdictEligible, ""},
		{"horizon set to 48 hours, 51 from its roll", idle(51 * time.Hour), measured(0), func(s Scheduler) Scheduler {
			prev := EarlyDrainHours()
			SetEarlyDrainHours(48)
			t.Cleanup(func() { SetEarlyDrainHours(prev) })
			return s
		}, VerdictEligible, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheduler, accts := paceCase(t, tc.score, tc.pace, tc.configure)
			verdict := scheduler.NewSessionVerdict(account.ProviderClaude, "idle", now)
			if verdict.Reason != tc.reason || verdict.BurndownCause != tc.cause {
				t.Fatalf("verdict %+v, want reason %q cause %q", verdict, tc.reason, tc.cause)
			}
			if _, reason := scheduler.compare(accts[1], accts[0], now, true); reason == reasonPaceBurndown {
				t.Fatalf("ranked on %q", reason)
			}
		})
	}
}

// The early burndown in its two parts: the switch on, and the horizon widened
// to 96 hours reaching an account 80 hours out.
func TestThePaceBurndownDrainsAnIdleAccountInsideItsHorizon(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name      string
		untilRoll time.Duration
		hours     int
	}{
		{"51 hours out, the default horizon", 51 * time.Hour, 0},
		{"just past the 24h window", 24*time.Hour + time.Minute, 0},
		{"72 hours out, the default horizon's edge", 72 * time.Hour, 0},
		{"80 hours out, the horizon at 96", 80 * time.Hour, 96},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.hours > 0 {
				prev := EarlyDrainHours()
				SetEarlyDrainHours(tc.hours)
				t.Cleanup(func() { SetEarlyDrainHours(prev) })
			}
			idle := resetScore("idle", 1, 1, now.Add(tc.untilRoll), time.Time{})
			chosen := resetScore("chosen", 0.84, 0.84, now.Add(100*time.Hour), time.Time{})
			scheduler := ownBurns(NewScheduler([]Score{idle, chosen}), map[string]float64{"idle": 0, "chosen": 0}).
				WithResetChoices(chooseFor(ResetChoiceWeek, 1, account.ProviderClaude, "chosen"))
			verdict := scheduler.NewSessionVerdict(account.ProviderClaude, "idle", now)
			if !verdict.Eligible || verdict.Reason != VerdictBurndown || verdict.BurndownCause != BurndownCausePace || verdict.AllowedSessions != 4 {
				t.Fatalf("verdict %+v, want eligible burndown on its pace with the full taper of 4", verdict)
			}
			sorted, err := scheduler.rankAt([]account.Account{claudeAccount("chosen"), claudeAccount("idle")}, true, now)
			if err != nil {
				t.Fatal(err)
			}
			if sorted[0].ID != "idle" {
				t.Fatalf("ranked %q first, want the idle account ahead of the chosen one", sorted[0].ID)
			}
			if _, reason := scheduler.compare(sorted[0], sorted[1], now, true); reason != reasonPaceBurndown {
				t.Fatalf("reason %q, want %q", reason, reasonPaceBurndown)
			}
		})
	}
}

// Only the budget above the owner's reserve can be wasted. With 30% of its
// week measured and a 20% reserve, 10 points are left to spend and it drains;
// with a 25% reserve 5 points are, which is not above the 5-point threshold.
func TestThePaceBurndownCountsOnlyTheUnreservedWeek(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		reserve float64
		cause   string
	}{
		{0.20, BurndownCausePace},
		{0.25, ""},
	} {
		score := resetScore("idle", 0.30, 0.30, now.Add(51*time.Hour), time.Time{})
		scheduler := ownBurns(NewScheduler([]Score{score}), map[string]float64{"idle": 0}).
			WithReserves(map[string]float64{"idle": tc.reserve})
		allowance := scheduler.PaceAllowance(account.ProviderClaude, "idle", now)
		if math.Abs(allowance.ProjectedWaste-(0.30-tc.reserve)) > 1e-9 {
			t.Fatalf("reserve %.2f: projected waste %.4f, want %.2f", tc.reserve, allowance.ProjectedWaste, 0.30-tc.reserve)
		}
		if got := scheduler.NewSessionVerdict(account.ProviderClaude, "idle", now).BurndownCause; got != tc.cause {
			t.Fatalf("reserve %.2f: cause %q, want %q", tc.reserve, got, tc.cause)
		}
	}
}

// The 36.6 h log line: no live session-hours anywhere in the pool, so no burn
// per live session, and the account's own burn measured at 0. Its projected
// waste is the whole week, and it is in danger.
func TestAMeasuredIdleAccountIsProjectedToWasteItsWeek(t *testing.T) {
	now := time.Now()
	roll := now.Add(time.Duration(36.6 * float64(time.Hour)))
	ref, _ := paceRef(now)
	ref.Set(NewScheduler([]Score{reading("idle-full", now.Add(-time.Hour), roll, 1)}))
	ref.Set(NewScheduler([]Score{reading("idle-full", now, roll, 1)}))
	got := ref.Get().PaceAllowance(account.ProviderClaude, "idle-full", now)
	if got.Pace.Source != "default" || got.Pace.BurnNow != 0 || math.Abs(got.ProjectedWaste-1) > 1e-9 || !got.Danger {
		t.Fatalf("allowance %+v, want the default burn per live session, own burn 0, projected waste 1 and danger", got)
	}
	ref, _ = paceRef(now)
	ref.Set(NewScheduler([]Score{reading("idle-full", now.Add(-20*time.Minute), roll, 1)}))
	ref.Set(NewScheduler([]Score{reading("idle-full", now, roll, 1)}))
	if got := ref.Get().PaceAllowance(account.ProviderClaude, "idle-full", now); got.ProjectedWaste != 0 || got.Danger {
		t.Fatalf("a 20-minute series: %+v, want no projection", got)
	}
}

// Inside the drain the roll still orders: an account inside its 24h window
// leads one in early burndown on its pace, and two in early burndown go
// soonest roll first.
func TestThePaceBurndownDrainsBehindEarlierRolls(t *testing.T) {
	now := time.Now()
	rolling := resetScore("rolling", 0.30, 0.30, now.Add(16*time.Hour), time.Time{})
	sooner := resetScore("sooner", 1, 1, now.Add(40*time.Hour), time.Time{})
	later := resetScore("later", 1, 1, now.Add(60*time.Hour), time.Time{})
	scheduler := ownBurns(NewScheduler([]Score{rolling, sooner, later}), map[string]float64{"rolling": 0, "sooner": 0, "later": 0})
	ids, reasons := choiceRanking(t, scheduler.Explain, claudeAccount("later"), claudeAccount("sooner"), claudeAccount("rolling"))
	want := "both draining; this one rolls sooner for the load it is carrying"
	if ids[0] != "rolling" || ids[1] != "sooner" || ids[2] != "later" || reasons[0] != want || reasons[1] != want {
		t.Fatalf("ranked %v %q, want rolling, sooner, later, each for %q", ids, reasons, want)
	}
	for id, cause := range map[string]string{"rolling": BurndownCauseRoll, "sooner": BurndownCausePace, "later": BurndownCausePace} {
		if got := scheduler.NewSessionVerdict(account.ProviderClaude, id, now).BurndownCause; got != cause {
			t.Fatalf("%s: cause %q, want %q", id, got, cause)
		}
	}
}

// Leaving the early burndown and coming back changes only the account's rank
// for the next new session. Its sessions are never told to leave: at its cap
// in the window, out of the window under load, and back in.
func TestLeavingThePaceBurndownMovesNoSession(t *testing.T) {
	now := time.Now()
	score := resetScore("idle", 1, 1, now.Add(51*time.Hour), time.Time{})
	type exits struct{ verdictEvicted, evicted, holdout, capacity, sticky bool }
	for _, tc := range []struct {
		name    string
		burn    float64
		live    int
		inside  bool
		eligble bool
	}{
		{"idle, inside", 0, 0, true, true},
		{"at its taper of 4, inside and at its cap", 0, 4, true, false},
		{"burning enough to spend the week, out", 0.03, 6, false, true},
		{"slowed again, back inside and at its cap", 0.005, 6, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := ScoreKey(account.ProviderClaude, "idle")
			scheduler := ownBurns(NewScheduler([]Score{score}), map[string]float64{"idle": tc.burn}).
				WithLiveSessionCounts(map[string]int{key: tc.live})
			verdict := scheduler.NewSessionVerdict(account.ProviderClaude, "idle", now)
			if inside := verdict.BurndownCause == BurndownCausePace; inside != tc.inside || verdict.Eligible != tc.eligble {
				t.Fatalf("verdict %+v, want inside=%v eligible=%v", verdict, tc.inside, tc.eligble)
			}
			got := exits{
				verdictEvicted: verdict.Evicted,
				evicted:        scheduler.Evicted(account.ProviderClaude, "idle"),
				holdout:        scheduler.InHoldout(account.ProviderClaude, "idle"),
				capacity:       scheduler.CapacityEvicting(account.ProviderClaude, "idle"),
				sticky:         scheduler.UsableForStickySession(account.ProviderClaude, "idle"),
			}
			if got != (exits{sticky: true}) {
				t.Fatalf("exits %+v, want none and the sticky session kept", got)
			}
		})
	}
}

// A peer's early burndown is its owner's answer. Published with the pace
// cause, it drains here ahead of a local chosen account and is named for its
// cause; from a peer that publishes no cause, it still drains, on the roll's
// words.
func TestAPeersPaceBurndownDrainsOnItsOwnersVerdict(t *testing.T) {
	now := time.Now()
	chosen := resetScore("chosen", 0.84, 0.84, now.Add(100*time.Hour), time.Time{})
	for _, tc := range []struct {
		cause, reason string
	}{
		{BurndownCausePace, reasonPaceBurndown},
		{"", "inside its weekly roll window; that budget expires either way"},
	} {
		peer := resetScore("there", 1, 1, now.Add(51*time.Hour), time.Time{})
		peer.Remote = true
		peer.PeerVerdict = &NewSessionVerdict{
			Eligible: true, Reason: VerdictBurndown, Burndown: true, AllowedSessions: 4, BurndownCause: tc.cause,
		}
		scheduler := NewScheduler([]Score{chosen, peer}).
			WithResetChoices(chooseFor(ResetChoiceWeek, 1, account.ProviderClaude, "chosen"))
		wantFirst(t, "cause "+tc.cause, scheduler, "there", tc.reason, claudeAccount("chosen"), claudeAccount("there"))
	}
}

// PickSteady parks an interactive session for hours, and an account the
// router is loading on its pace is not the place.
func TestPickSteadyRanksAPaceBurndownAccountLast(t *testing.T) {
	now := time.Now()
	idle := resetScore("idle", 1, 1, now.Add(51*time.Hour), time.Time{})
	steady := resetScore("steady", 1, 1, now.Add(138*time.Hour), time.Time{})
	accts := []account.Account{claudeAccount("idle"), claudeAccount("steady")}
	plain := NewScheduler([]Score{idle, steady})
	if got, err := plain.PickSteady(accts); err != nil || got.ID != "idle" {
		t.Fatalf("calibration: unmeasured, PickSteady chose %q (%v), want idle on its pace", got.ID, err)
	}
	scheduler := ownBurns(plain, map[string]float64{"idle": 0})
	ids, reasons := choiceRanking(t, scheduler.ExplainSteady, accts...)
	want := "the other is in early burndown on its pace and the router is loading it; not a place to park a session"
	if ids[0] != "steady" || reasons[0] != want {
		t.Fatalf("ExplainSteady %v %q, want steady first for %q", ids, reasons, want)
	}
}

// An account chosen for its Fable meter is not drained on its pace on the
// Opus pool either: Opus work there would eat the week its Fable loading
// needs (PATCHES §95.2).
func TestAFableChoiceIsNotDrainedOnItsPace(t *testing.T) {
	now := time.Now()
	idle := resetScore("idle", 1, 1, now.Add(51*time.Hour), time.Time{})
	scheduler := ownBurns(NewScheduler([]Score{idle}), map[string]float64{"idle": 0}).
		WithResetChoices(chooseFor(ResetChoiceFable, 1, account.ProviderClaude, "idle")).
		ForModel("claude-opus")
	if got := scheduler.NewSessionVerdict(account.ProviderClaude, "idle", now); got.BurndownCause != "" || got.Reason != VerdictEligible {
		t.Fatalf("Opus pool verdict %+v, want eligible with no burndown", got)
	}
}
