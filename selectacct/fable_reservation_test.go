package selectacct

import (
	"fmt"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

// PATCHES §57: the Fable reservation is off unless SUBROUTER_FABLE_RESERVATION
// turns it on. The owner, 2026-10-15: "Fable allocation isn't important
// anymore! ... for now it's all about the opus".

// setFableReservationForTest sets §57's process-wide switch for the rest of
// the test and restores it in t.Cleanup. Only for tests that are not
// t.Parallel. Scores are computed when they are built, so call it first.
func setFableReservationForTest(t *testing.T, on bool) {
	t.Helper()
	prev := FableReservation()
	SetFableReservation(on)
	t.Cleanup(func() { SetFableReservation(prev) })
}

// legacyFableSpendable is a non-Fable Claude pool's WeeklyHeadroom as it stood
// before §57, from the general and Fable used percentages and the seconds to
// the roll. Do not edit: it is the baseline the switch, turned on, must
// reproduce.
func legacyFableSpendable(generalUsed, fableUsed float64, resetSeconds int64) float64 {
	general := 1 - clampPercent(generalUsed)/100
	fable := 1 - clampPercent(fableUsed)/100
	if general < fable {
		fable = general
	}
	decay := 1.0
	switch {
	case resetSeconds <= 0:
		decay = 0
	case resetSeconds < 24*60*60:
		decay = float64(resetSeconds) / float64(24*60*60)
	}
	spendable := general - 0.5*fable*decay
	if spendable < 0 {
		spendable = 0
	}
	return spendable
}

// The grid every switch test below walks: general and Fable meters from
// untouched to past full, and rolls from minutes to days away.
var (
	fableGridGeneralUsed = []float64{0, 10, 50, 61, 76, 90, 100}
	fableGridFableUsed   = []float64{0, 25, 50, 90, 100, 104}
	fableGridRolls       = []int64{60, 3600, 12 * 3600, 23 * 3600, 24 * 3600, 4 * 24 * 3600}
)

// Off is the default: with the switch never touched, every non-Fable pool's
// WeeklyHeadroom is the measured week, whatever the Fable meter says. That is
// fifteen-beta on 10/15 reading 0.39 rather than 0.195, and the rest of
// the grid.
func TestFableReservationIsOffByDefault(t *testing.T) {
	if FableReservation() {
		t.Fatal("the Fable reservation is on in a process that never set it; §57's default is off")
	}
	now := time.Now()
	fifteenUsed := 61.0
	fifteen := ScoreFromLimitWindowsAt("fifteen-beta", 0, claudePoolWindows(fifteenUsed, 0, 10, fourDays, twoHours), now)
	for _, key := range []string{"", "claudeopus", "claudesonnet", "claudefable"} {
		score := fifteen
		if key != "" {
			score = fifteen.ModelScores[key]
		}
		if score.WeeklyHeadroom != 1-fifteenUsed/100 || score.WeeklyRemaining != 1-fifteenUsed/100 {
			t.Fatalf("pool %q: WeeklyHeadroom %.3f, measured %.3f; want the measured 0.39 with the Fable meter unspent",
				key, score.WeeklyHeadroom, score.WeeklyRemaining)
		}
	}
	for _, general := range fableGridGeneralUsed {
		for _, fable := range fableGridFableUsed {
			for _, roll := range fableGridRolls {
				score := ScoreFromLimitWindowsAt("acct", 0, claudePoolWindows(general, fable, 10, roll, twoHours), now)
				for _, key := range []string{"", "claudeopus", "claudesonnet"} {
					pool := score
					if key != "" {
						pool = score.ModelScores[key]
					}
					if want := 1 - general/100; pool.WeeklyHeadroom != want {
						t.Fatalf("general %v%% fable %v%% roll %ds pool %q: WeeklyHeadroom %v, want the measured %v",
							general, fable, roll, key, pool.WeeklyHeadroom, want)
					}
				}
			}
		}
	}
}

// On, the reservation is today's exactly: every non-Fable pool on the grid
// matches the frozen pre-§57 formula, and the Fable pool's own score is the
// same as with the switch off.
func TestFableReservationOnIsTodaysReservation(t *testing.T) {
	now := time.Now()
	type key struct {
		general, fable float64
		roll           int64
	}
	fableOff := map[key]float64{}
	for _, general := range fableGridGeneralUsed {
		for _, fable := range fableGridFableUsed {
			for _, roll := range fableGridRolls {
				score := ScoreFromLimitWindowsAt("acct", 0, claudePoolWindows(general, fable, 10, roll, twoHours), now)
				fableOff[key{general, fable, roll}] = score.ModelScores["claudefable"].WeeklyHeadroom
			}
		}
	}

	setFableReservationForTest(t, true)
	fifteen := ScoreFromLimitWindowsAt("fifteen-beta", 0, claudePoolWindows(61, 0, 10, fourDays, twoHours), now)
	if got := fifteen.ModelScores["claudeopus"].WeeklyHeadroom; got < 0.195-1e-9 || got > 0.195+1e-9 {
		t.Fatalf("fifteen-beta's opus WeeklyHeadroom %.4f, want 0.195 as measured live on 10/15", got)
	}
	reserved := 0
	for _, general := range fableGridGeneralUsed {
		for _, fable := range fableGridFableUsed {
			for _, roll := range fableGridRolls {
				score := ScoreFromLimitWindowsAt("acct", 0, claudePoolWindows(general, fable, 10, roll, twoHours), now)
				want := legacyFableSpendable(general, fable, roll)
				for _, poolKey := range []string{"", "claudeopus", "claudesonnet"} {
					pool := score
					if poolKey != "" {
						pool = score.ModelScores[poolKey]
					}
					if pool.WeeklyHeadroom != want {
						t.Fatalf("general %v%% fable %v%% roll %ds pool %q: WeeklyHeadroom %v, want today's %v",
							general, fable, roll, poolKey, pool.WeeklyHeadroom, want)
					}
					if pool.WeeklyRemaining != 1-general/100 {
						t.Fatalf("general %v%% pool %q: WeeklyRemaining %v carries the reservation", general, poolKey, pool.WeeklyRemaining)
					}
				}
				if want < 1-general/100 {
					reserved++
				}
				if got, off := score.ModelScores["claudefable"].WeeklyHeadroom, fableOff[key{general, fable, roll}]; got != off {
					t.Fatalf("general %v%% fable %v%% roll %ds: the Fable pool reads %v with the switch on, %v off; it never reserves against itself",
						general, fable, roll, got, off)
				}
			}
		}
	}
	if reserved == 0 {
		t.Fatal("calibration: no grid point carried a reservation; the comparison above proves nothing")
	}
}

// The drain taper reads WeeklyHeadroom, so with the reservation off an
// early-burndown holder with its Fable meter unspent gets the full taper,
// 4/3/2/1 at 24/18/12/6% of its week measured; on, it gets today's 2/2/1/1.
// The Fable pool reads its own meter and is the same either way.
func TestEarlyBurndownTakesTheFullTaperWithTheReservationOff(t *testing.T) {
	setResetHoldPriorityForTest(t, true)
	now := time.Now()
	held := now.Add(5 * 24 * time.Hour)
	for _, on := range []bool{false, true} {
		for _, tc := range []struct {
			generalUsed float64
			off, onAt   int
		}{
			{76, 4, 2},
			{82, 3, 2},
			{88, 2, 1},
			{94, 1, 1},
		} {
			want := tc.off
			if on {
				want = tc.onAt
			}
			t.Run(fmt.Sprintf("reservation on=%v, %.0f%% measured", on, 100-tc.generalUsed), func(t *testing.T) {
				setFableReservationForTest(t, on)
				holder := ScoreFromLimitWindowsAt("holder", 0, claudePoolWindows(tc.generalUsed, 0, 10, 72*3600, twoHours), now)
				holder.Provider, holder.Fresh, holder.ResetHeldUntil = account.ProviderClaude, true, held
				for _, model := range []string{"", "claude-opus", "claude-fable"} {
					scheduler := NewScheduler([]Score{holder})
					if model != "" {
						scheduler = scheduler.ForModel(model)
					}
					poolWant := want
					if model == "claude-fable" {
						poolWant = tc.off
					}
					verdict := scheduler.NewSessionVerdict(account.ProviderClaude, "holder", now)
					if verdict.Reason != VerdictBurndown || verdict.BurndownCause != BurndownCauseReset || verdict.AllowedSessions != poolWant {
						t.Fatalf("model %q: verdict %+v, want early burndown (cause %q) allowing %d",
							model, verdict, BurndownCauseReset, poolWant)
					}
				}
			})
		}
	}
}
