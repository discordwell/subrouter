package selectacct

import (
	"math/rand"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

// The verdict must be the Pick predicates, not a second copy of them: for any
// score, Eligible is tier-0 routability minus the endgame cap and minus the
// eviction selection refuses after Pick, and every flag is the predicate of the
// same name.
func TestNewSessionVerdictAgreesWithPickPredicates(t *testing.T) {
	rng := rand.New(rand.NewSource(25))
	now := time.Now()
	resetBurndowns := 0
	for i := 0; i < 5000; i++ {
		score := Score{
			AccountID:              "a",
			Provider:               account.ProviderClaude,
			Headroom:               float64(rng.Intn(5)) / 4,
			ShortHeadroom:          float64(rng.Intn(21)) / 20,
			ShortWindowSeconds:     int64(rng.Intn(2)) * 18000,
			ShortResetAfterSeconds: int64(rng.Intn(18001)),
			WeeklyHeadroom:         float64(rng.Intn(21)) / 20,
			WeeklyRemaining:        float64(rng.Intn(21)) / 20,
			WeeklyOnly:             rng.Intn(5) == 0,
			Fresh:                  true,
		}
		if rng.Intn(3) > 0 {
			score.WeeklyResetAt = now.Add(time.Duration(rng.Intn(7*86400)) * time.Second)
		}
		// PATCHES §56: no hold, an expired one, a live one. WeeklyRemaining
		// above already reaches under a quarter, so early burndown occurs.
		switch rng.Intn(3) {
		case 1:
			score.ResetHeldUntil = now.Add(-time.Duration(1+rng.Intn(3*86400)) * time.Second)
		case 2:
			score.ResetHeldUntil = now.Add(time.Duration(1+rng.Intn(7*86400)) * time.Second)
		}
		reserves := map[string]float64{}
		if rng.Intn(3) == 0 {
			reserves["a"] = float64(rng.Intn(5)) / 10
		}
		sched := NewScheduler([]Score{score}).
			WithReserves(reserves).
			WithLiveSessionCounts(map[string]int{ScoreKey(account.ProviderClaude, "a"): rng.Intn(6)}).
			WithClosedAccounts(map[string]bool{ScoreKey(account.ProviderClaude, "a"): rng.Intn(4) == 0})
		got := sched.NewSessionVerdict(account.ProviderClaude, "a", now)
		s := sched.score(account.ProviderClaude, "a")
		if got.Burndown != s.endgameDrainEligible(now) || got.AtCap != s.endgameAtCap(now) || got.Evicted != s.evicted() {
			t.Fatalf("flags diverge from the predicates: %+v for %+v", got, s)
		}
		if want := s.routableForNewSession(now, true) && !s.endgameAtCap(now) && !s.evicted(); got.Eligible != want {
			t.Fatalf("Eligible=%v, Pick tier-0 minus cap and eviction says %v: %+v for %+v", got.Eligible, want, got, s)
		}
		if got.Eligible != (got.Reason == VerdictEligible || got.Reason == VerdictBurndown) {
			t.Fatalf("reason %q disagrees with Eligible=%v", got.Reason, got.Eligible)
		}
		if got.LiveSessions != s.LiveSessions {
			t.Fatalf("live sessions %d, scheduler holds %d", got.LiveSessions, s.LiveSessions)
		}
		if got.BurndownCause != s.BurndownCause(now) || (got.AllowedSessions > 0) != (got.BurndownCause != "") {
			t.Fatalf("cause %q with allowance %d, the predicates say %q: %+v", got.BurndownCause, got.AllowedSessions, s.BurndownCause(now), s)
		}
		if got.Burndown && got.BurndownCause == BurndownCauseReset {
			resetBurndowns++
		}
	}
	if resetBurndowns == 0 {
		t.Fatal("the randomiser never produced an early-burndown verdict; the §56 half of this invariant is untested")
	}
	t.Logf("early-burndown verdicts: %d", resetBurndowns)
}

// 2026-09-27: chip-5 had ~4% of its week left and rolled in ~16h. Its taper
// allows one live session; at one it is capped and a peer may not place there.
func TestNewSessionVerdictCapsAnEndgameAccountOnLiveSessions(t *testing.T) {
	now := time.Now()
	chip5 := Score{
		AccountID: "chip-5", Provider: account.ProviderClaude,
		Headroom: 0.04, ShortHeadroom: 0.23, ShortWindowSeconds: 18000, ShortResetAfterSeconds: 9000,
		WeeklyHeadroom: 0.04, WeeklyRemaining: 0.04, WeeklyResetAt: now.Add(16 * time.Hour), Fresh: true,
	}
	key := ScoreKey(account.ProviderClaude, "chip-5")
	idle := NewScheduler([]Score{chip5}).WithLiveSessionCounts(map[string]int{key: 0}).
		NewSessionVerdict(account.ProviderClaude, "chip-5", now)
	if !idle.Eligible || !idle.Burndown || idle.Reason != VerdictBurndown || idle.AllowedSessions != 1 {
		t.Fatalf("idle chip-5 should be burndown-eligible with an allowance of 1: %+v", idle)
	}
	busy := NewScheduler([]Score{chip5}).WithLiveSessionCounts(map[string]int{key: 1}).
		NewSessionVerdict(account.ProviderClaude, "chip-5", now)
	if busy.Eligible || !busy.AtCap || busy.Reason != VerdictAtCap || busy.Evicted {
		t.Fatalf("chip-5 carrying its one allowed session must refuse a new one without evicting: %+v", busy)
	}
}
