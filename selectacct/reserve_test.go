package selectacct

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

// A reserved account stops taking new sessions once its weekly budget reaches
// the holdout, and keeps serving normally above it.
func TestWeeklyReserveGatesNewSessions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		weekly     float64
		reserve    float64
		wantUsable bool
	}{
		{"plenty left", 0.50, 0.05, true},
		{"well above the holdout", 0.10, 0.05, true},
		{"exactly at the holdout", 0.05, 0.05, false},
		{"inside the holdout", 0.03, 0.05, false},
		// No reserve configured is the ordinary pooled account, spendable to
		// the usual floor.
		{"unreserved account at the same level", 0.03, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			score := Score{
				Headroom:       1,
				ShortHeadroom:  1,
				WeeklyOnly:     true,
				WeeklyHeadroom: tc.weekly,
				Reserve:        tc.reserve,
			}
			if got := score.usableForNewSession(); got != tc.wantUsable {
				t.Fatalf("usableForNewSession(weekly %.2f, reserve %.2f) = %v, want %v",
					tc.weekly, tc.reserve, got, tc.wantUsable)
			}
		})
	}
}

// The drain predicates on a score that still carries a reserve inside a drain
// window -- the 24h window with PATCHES §60's release off here, or a reset
// holder's early burndown (TestEarlyBurndownKeepsTheHoldout) -- do not drain it
// into its holdout. Endgame drain will hand back an account at
// 1%, so a holdout enforced only in usableForNewSession would be eaten there.
func TestEndgameDrainDoesNotEatTheReserve(t *testing.T) {
	now := time.Now()
	inEndgame := now.Add(6 * time.Hour)

	reserved := Score{
		AccountID:      "main",
		Headroom:       1,
		ShortHeadroom:  1,
		WeeklyOnly:     true,
		WeeklyHeadroom: 0.04,
		Reserve:        0.05,
		WeeklyResetAt:  inEndgame,
	}
	if reserved.endgameDrainEligible(now) {
		t.Fatal("endgame drain claimed a reserved account inside its holdout")
	}
	if reserved.routableForNewSession(now, true) {
		t.Fatal("routableForNewSession admitted a reserved account via the endgame widening")
	}

	// Same account above its holdout still drains normally -- the reserve is a
	// floor, not an exemption from endgame.
	spendable := reserved
	spendable.WeeklyHeadroom = 0.40
	if !spendable.endgameDrainEligible(now) {
		t.Fatal("reserved account above its holdout was refused the endgame drain")
	}
}

// Fail open on missing telemetry: a zero WeeklyHeadroom on a non-exhausted
// score means the weekly window was never measured, not that the week is gone.
// Refusing there would turn a telemetry gap into an outage.
func TestReserveFailsOpenWhenWeeklyIsUnknown(t *testing.T) {
	score := Score{
		Headroom:       1,
		ShortHeadroom:  1,
		WeeklyOnly:     true,
		WeeklyHeadroom: 0,
		Reserve:        0.05,
	}
	if score.belowWeeklyReserve() {
		t.Fatal("unmeasured weekly budget read as inside the holdout")
	}
	if !score.usableForNewSession() {
		t.Fatal("account refused for lack of weekly telemetry; the reserve must fail open")
	}
}

// The reserve must survive the builder chain. Every With* builder used to
// construct a fresh Scheduler literal, which silently dropped any field it did
// not name -- machineID and machines were already being lost that way.
func TestReserveSurvivesTheBuilderChain(t *testing.T) {
	scheduler := NewScheduler([]Score{{
		AccountID:      "main",
		Headroom:       1,
		ShortHeadroom:  1,
		WeeklyOnly:     true,
		WeeklyHeadroom: 0.04,
	}}).
		WithReserves(map[string]float64{"main": 0.05}).
		WithSessionCounts(map[string]int{"main": 3}).
		WithLiveSessionCounts(map[string]int{"main": 1}).
		WithLiveDebits(map[string]int{})

	if got := scheduler.score("", "main").Reserve; got != 0.05 {
		t.Fatalf("Reserve = %v after the builder chain, want 0.05 (a builder dropped it)", got)
	}
	if scheduler.UsableForNewSession("", "main") {
		t.Fatal("reserved account inside its holdout was reported usable through the scheduler")
	}
}

// End to end: with every other account spent, the pool refuses rather than
// spending the holdout. That refusal IS the feature -- it is what leaves a way
// back in when the pool is dry.
func TestPoolRefusesRatherThanSpendingTheHoldout(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "spent", Headroom: 0, ShortHeadroom: 0, WeeklyOnly: true, WeeklyHeadroom: 0},
		{AccountID: "main", Headroom: 1, ShortHeadroom: 1, WeeklyOnly: true, WeeklyHeadroom: 0.04},
	}).WithReserves(map[string]float64{"main": 0.05})

	if scheduler.UsableForNewSession("", "main") {
		t.Fatal("the holdout was offered to the pool once everything else was spent")
	}

	// And above the holdout it is an ordinary pool member.
	open := NewScheduler([]Score{
		{AccountID: "main", Headroom: 1, ShortHeadroom: 1, WeeklyOnly: true, WeeklyHeadroom: 0.30},
	}).WithReserves(map[string]float64{"main": 0.05})
	picked, err := open.Pick([]account.Account{{ID: "main", AuthMode: account.AuthModeOAuth}})
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if picked.ID != "main" {
		t.Fatalf("picked %q, want main", picked.ID)
	}
}
