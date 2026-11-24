package selectacct

import (
	"testing"
	"time"

	"github.com/discordwell/subrouter/account"
)

// PATCHES §60: an owner's holdout is lifted in the last day before the
// account's weekly roll, which would otherwise wipe it unspent.

// keepReserveThroughRollForTest turns §60's release off for the rest of the
// test and restores it in t.Cleanup. Only for tests that are not t.Parallel.
func keepReserveThroughRollForTest(t *testing.T) {
	t.Helper()
	prev := ReleaseReserveFinalDay()
	SetReleaseReserveFinalDay(false)
	t.Cleanup(func() { SetReleaseReserveFinalDay(prev) })
}

// atItsHoldout is main as the owner found it on 2026-10-29: 4% of its week
// left against a 5% holdout, with every other account spent.
func atItsHoldout(untilRoll time.Duration) Scheduler {
	return NewScheduler([]Score{
		{AccountID: "spent", Provider: account.ProviderClaude, Fresh: true,
			Headroom: 0, ShortHeadroom: 0, WeeklyHeadroom: 0, WeeklyResetAt: time.Now().Add(100 * time.Hour)},
		{AccountID: "main", Provider: account.ProviderClaude, Fresh: true,
			Headroom: 0.04, ShortHeadroom: 0.9, WeeklyHeadroom: 0.04, WeeklyRemaining: 0.04,
			WeeklyResetAt: time.Now().Add(untilRoll)},
	}).WithReserves(map[string]float64{"main": 0.05})
}

// 5h out is where the owner found main; 23h pins the window's far edge, so a
// window narrowed below a day goes red here and one widened past it goes red
// in TestHoldoutKeptBeforeTheFinalDay.
func TestHoldoutLiftedInTheFinalDay(t *testing.T) {
	for _, untilRoll := range []time.Duration{5 * time.Hour, 23 * time.Hour} {
		now := time.Now()
		sched := atItsHoldout(untilRoll)
		if sched.InHoldout(account.ProviderClaude, "main") || sched.Evicted(account.ProviderClaude, "main") {
			t.Fatalf("an account %v from its roll was held in its holdout", untilRoll)
		}
		if v := sched.NewSessionVerdict(account.ProviderClaude, "main", now); !v.Eligible || v.Reason != VerdictBurndown {
			t.Fatalf("verdict %v from the roll = %+v, want eligible to burn down", untilRoll, v)
		}
		if got := pickID(t, sched, "spent", "main"); got != "main" {
			t.Fatalf("pick %v from the roll = %q, want main spending its holdout", untilRoll, got)
		}
	}
}

// A day and an hour out, the holdout holds exactly as before §60.
func TestHoldoutKeptBeforeTheFinalDay(t *testing.T) {
	now := time.Now()
	sched := atItsHoldout(25 * time.Hour)
	if !sched.InHoldout(account.ProviderClaude, "main") || !sched.Evicted(account.ProviderClaude, "main") {
		t.Fatal("an account 25h from its roll was not held in its holdout")
	}
	if v := sched.NewSessionVerdict(account.ProviderClaude, "main", now); v.Eligible || v.Reason != VerdictHoldout {
		t.Fatalf("verdict 25h from the roll = %+v, want refused for the holdout", v)
	}
	if sched.UsableForNewSession(account.ProviderClaude, "main") {
		t.Fatal("the pool offered main's holdout a day before its roll")
	}
}

// The kill switch restores the holdout through the roll.
func TestHoldoutKeptThroughTheRollWithTheReleaseOff(t *testing.T) {
	keepReserveThroughRollForTest(t)
	now := time.Now()
	sched := atItsHoldout(5 * time.Hour)
	if !sched.InHoldout(account.ProviderClaude, "main") {
		t.Fatal("release off: an account 5h from its roll was not held in its holdout")
	}
	if v := sched.NewSessionVerdict(account.ProviderClaude, "main", now); v.Eligible {
		t.Fatalf("release off: verdict 5h from the roll = %+v, want refused", v)
	}
}

// The drain's taper is sized on the whole week once the holdout is lifted:
// 15 points above a 5% holdout allowed 3 sessions, and 20% unreserved allows 4.
// TestReservedDrainingAccountSpillsBeforeItsHoldout is the same pick with the
// release off.
func TestFinalDayDrainSizesOnTheWholeWeek(t *testing.T) {
	now := time.Now()
	owner := Score{
		AccountID: "owner", Provider: account.ProviderClaude,
		Headroom: 1, ShortHeadroom: 1, Fresh: true,
		WeeklyHeadroom: 0.20, WeeklyRemaining: 0.20, WeeklyResetAt: now.Add(10 * time.Hour),
	}
	healthy := claudeScore("healthy", claudePoolWindows(20, 20, 5, fourDays, twoHours))
	base := NewScheduler([]Score{owner, healthy}).WithReserves(map[string]float64{"owner": 0.05})
	if got := pickID(t, base.WithLiveSessionCounts(map[string]int{"owner": 3}), "owner", "healthy"); got != "owner" {
		t.Fatalf("pick with the owner carrying 3 = %q, want the owner still draining on its whole week", got)
	}
	if got := pickID(t, base.WithLiveSessionCounts(map[string]int{"owner": 4}), "owner", "healthy"); got != "healthy" {
		t.Fatalf("pick with the owner carrying 4 = %q, want the spill at the ladder's ceiling", got)
	}
}

// A model pool can carry a meter with its own reset (Fable). The holdout is a
// fraction of the ACCOUNT's week, so the release follows the account's roll in
// that pool's view too, and the pool view agrees with the account view.
func TestFinalDayReleaseFollowsTheAccountsRollInAModelPool(t *testing.T) {
	fable := ModelKey("claude-fable")
	scheduler := func(accountRoll, poolRoll time.Duration) Scheduler {
		now := time.Now()
		return NewScheduler([]Score{{
			AccountID: "main", Provider: account.ProviderClaude, Fresh: true,
			Headroom: 0.04, ShortHeadroom: 0.9, WeeklyHeadroom: 0.04, WeeklyRemaining: 0.04,
			WeeklyResetAt: now.Add(accountRoll),
			ModelScores: map[string]Score{fable: {
				AccountID: "main", Provider: account.ProviderClaude, Fresh: true,
				Headroom: 0.02, ShortHeadroom: 0.9, WeeklyHeadroom: 0.02, WeeklyResetAt: now.Add(poolRoll),
			}},
		}}).WithReserves(map[string]float64{"main": 0.05})
	}
	for _, tc := range []struct {
		name                  string
		accountRoll, poolRoll time.Duration
		wantHeld              bool
	}{
		{"the pool rolls tonight, the account's week in 3 days", 72 * time.Hour, 10 * time.Hour, true},
		{"the account's week rolls tonight, the pool in 3 days", 10 * time.Hour, 72 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := scheduler(tc.accountRoll, tc.poolRoll)
			if got := base.InHoldout(account.ProviderClaude, "main"); got != tc.wantHeld {
				t.Fatalf("account view: InHoldout = %v, want %v", got, tc.wantHeld)
			}
			if got := base.ForModel("claude-fable").InHoldout(account.ProviderClaude, "main"); got != tc.wantHeld {
				t.Fatalf("Fable view: InHoldout = %v, want %v (the account view's answer)", got, tc.wantHeld)
			}
		})
	}
}

// A reset holder's early burndown (PATCHES §56) can start days before the
// roll. Outside the last day the holdout stands there: no burndown into it.
func TestEarlyBurndownKeepsTheHoldout(t *testing.T) {
	now := time.Now()
	sched := NewScheduler([]Score{{
		AccountID: "main", Provider: account.ProviderClaude, Fresh: true,
		Headroom: 0.04, ShortHeadroom: 0.9, WeeklyHeadroom: 0.04, WeeklyRemaining: 0.04,
		WeeklyResetAt: now.Add(72 * time.Hour), ResetHeldUntil: now.Add(48 * time.Hour),
	}}).WithReserves(map[string]float64{"main": 0.05})
	if !sched.ScoreFor(account.ProviderClaude, "main").HoldsReset(now) {
		t.Fatal("calibration: the fixture holds no reset, so early burndown never runs")
	}
	if !sched.InHoldout(account.ProviderClaude, "main") {
		t.Fatal("a reset holder 72h from its roll was not held in its holdout")
	}
	if v := sched.NewSessionVerdict(account.ProviderClaude, "main", now); v.Eligible || v.Burndown || v.Reason != VerdictHoldout {
		t.Fatalf("verdict = %+v, want refused for the holdout, not burning down", v)
	}
}
