package selectacct

import (
	"testing"
	"time"

	"github.com/discordwell/subrouter/account"
)

// The owner's holdout is a hard limit: nothing may START a session inside it.
// usableForNewSession already refused it, but when no account is routable Pick
// falls back to "any non-exhausted account" -- and a holdout account is not
// exhausted. Claude ranking already prices a holdout account's spendable week
// at ~0, so the fallback bites on Codex: weekly-only accounts are ordered on
// time to refresh, and a holdout account rolling soonest ranked FIRST.
func TestPickFallbackNeverLandsInTheHoldout(t *testing.T) {
	codex := func(id string, weekly float64, rollIn time.Duration) Score {
		return Score{
			AccountID: id, Provider: account.ProviderCodex, Fresh: true, WeeklyOnly: true,
			Headroom: weekly, ShortHeadroom: weekly,
			WeeklyHeadroom: weekly, WeeklyRemaining: weekly, WeeklyResetAt: time.Now().Add(rollIn),
		}
	}
	scheduler := NewScheduler([]Score{
		// In its holdout and refreshing first.
		codex("held", 0.04, 30*time.Hour),
		// Under the 5% exhaustion floor, so not routable either -- which is
		// what sends Pick to its fallback tier -- but not in any holdout.
		codex("other", 0.03, 100*time.Hour),
	}).WithReserves(map[string]float64{"held": 0.05})
	candidates := []account.Account{
		{ID: "held", Provider: account.ProviderCodex, AuthMode: account.AuthModeOAuth},
		{ID: "other", Provider: account.ProviderCodex, AuthMode: account.AuthModeOAuth},
	}

	for _, id := range []string{"held", "other"} {
		if scheduler.UsableForNewSession(account.ProviderCodex, id) {
			t.Fatalf("calibration: %s is routable, so the fallback tier is never reached", id)
		}
	}
	if !scheduler.InHoldout(account.ProviderCodex, "held") || !scheduler.Evicted(account.ProviderCodex, "held") {
		t.Fatal("an account at its holdout must read as in-holdout and evicted")
	}
	if scheduler.Exhausted(account.ProviderCodex, "held") {
		t.Fatal("a holdout is not a spent window; Exhausted must stay false")
	}
	picked, err := scheduler.Pick(candidates)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "other" {
		t.Fatalf("Pick fell back to %q, inside the owner's holdout", picked.ID)
	}
}

func TestEvictedCoversSpentWindowsAndOnlyThose(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "spent5h", Provider: account.ProviderClaude, Fresh: true, Headroom: 0.5, ShortHeadroom: 0, WeeklyHeadroom: 0.5},
		{AccountID: "spentweek", Provider: account.ProviderClaude, Fresh: true, Headroom: 0, ShortHeadroom: 0.9, WeeklyHeadroom: 0},
		{AccountID: "healthy", Provider: account.ProviderClaude, Fresh: true, Headroom: 0.5, ShortHeadroom: 0.9, WeeklyHeadroom: 0.5, WeeklyRemaining: 0.5},
		// Reserve with no measured week fails OPEN, as belowWeeklyReserve does.
		{AccountID: "unmeasured", Provider: account.ProviderClaude, Fresh: true, Headroom: 0.5, ShortHeadroom: 0.9},
	}).WithReserves(map[string]float64{"healthy": 0.05, "unmeasured": 0.05})
	want := map[string]bool{"spent5h": true, "spentweek": true, "healthy": false, "unmeasured": false}
	for id, evicted := range want {
		if got := scheduler.Evicted(account.ProviderClaude, id); got != evicted {
			t.Errorf("Evicted(%s) = %v, want %v", id, got, evicted)
		}
	}
}

// The holdout evicts warm sessions, so it is judged on the MEASURED weekly
// remainder. WeeklyHeadroom is the wrong input twice over: score() lowers it by
// a live debit per routed request between usage refreshes, and for non-Fable
// Claude pools it already has the Fable routing reservation subtracted. Either
// one evicted sessions from accounts well above their holdout.
func TestHoldoutIgnoresLiveDebits(t *testing.T) {
	scheduler := NewScheduler([]Score{{
		AccountID: "a", Provider: account.ProviderClaude, Fresh: true,
		Headroom: 0.5, ShortHeadroom: 0.9, WeeklyHeadroom: 0.20, WeeklyRemaining: 0.20,
		WeeklyResetAt: time.Now().Add(100 * time.Hour),
	}}).WithReserves(map[string]float64{"a": 0.10}).
		WithLiveDebits(map[string]int{ScoreKey(account.ProviderClaude, "a"): 5})

	// Calibration: the debits DO reach the soft new-session gate (which reads
	// the debited WeeklyHeadroom) -- without them this account is usable.
	undebited := NewScheduler([]Score{{
		AccountID: "a", Provider: account.ProviderClaude, Fresh: true,
		Headroom: 0.5, ShortHeadroom: 0.9, WeeklyHeadroom: 0.20, WeeklyRemaining: 0.20,
		WeeklyResetAt: time.Now().Add(100 * time.Hour),
	}}).WithReserves(map[string]float64{"a": 0.10})
	if !undebited.UsableForNewSession(account.ProviderClaude, "a") || scheduler.UsableForNewSession(account.ProviderClaude, "a") {
		t.Fatal("calibration: five live debits did not reach the holdout on the debited figure")
	}
	if scheduler.Evicted(account.ProviderClaude, "a") || scheduler.InHoldout(account.ProviderClaude, "a") {
		t.Fatal("an estimate of five routed requests evicted an account measured at twice its holdout")
	}
}

func TestHoldoutIgnoresTheFableReservation(t *testing.T) {
	scheduler := NewScheduler([]Score{
		// Spendable-after-Fable is under the holdout; the week itself is not.
		{AccountID: "a", Provider: account.ProviderClaude, Fresh: true,
			Headroom: 0.5, ShortHeadroom: 0.9, WeeklyHeadroom: 0.08, WeeklyRemaining: 0.20,
			WeeklyResetAt: time.Now().Add(100 * time.Hour)},
		{AccountID: "b", Provider: account.ProviderClaude, Fresh: true,
			Headroom: 0.5, ShortHeadroom: 0.9, WeeklyHeadroom: 0.02, WeeklyRemaining: 0.08,
			WeeklyResetAt: time.Now().Add(100 * time.Hour)},
	}).WithReserves(map[string]float64{"a": 0.10, "b": 0.10})

	if scheduler.Evicted(account.ProviderClaude, "a") {
		t.Fatal("the Fable reservation pushed an account with 20% of its week into its 10% holdout")
	}
	if !scheduler.Evicted(account.ProviderClaude, "b") {
		t.Fatal("an account whose measured week is inside its holdout was not evicted")
	}
}

// A score carried forward past its weekly roll describes a week that is gone;
// as a hard limit it would evict sessions from an account that just reset.
func TestHoldoutIgnoresAReadingFromBeforeTheWeekRolled(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "rolled", Provider: account.ProviderClaude, Headroom: 0.5, ShortHeadroom: 0.9,
			WeeklyHeadroom: 0.04, WeeklyRemaining: 0.04, WeeklyResetAt: time.Now().Add(-time.Hour)},
		// Past the final day, where the holdout is lifted (PATCHES §60).
		{AccountID: "current", Provider: account.ProviderClaude, Headroom: 0.5, ShortHeadroom: 0.9,
			WeeklyHeadroom: 0.04, WeeklyRemaining: 0.04, WeeklyResetAt: time.Now().Add(48 * time.Hour)},
	}).WithReserves(map[string]float64{"rolled": 0.05, "current": 0.05})
	if scheduler.Evicted(account.ProviderClaude, "rolled") {
		t.Fatal("a pre-roll reading held an account in its holdout after the week reset")
	}
	// The soft gate fails open on it too (PATCHES §60): the old week's
	// headroom says nothing about the new week's.
	if !scheduler.UsableForNewSession(account.ProviderClaude, "rolled") {
		t.Fatal("a pre-roll reading refused new sessions for a holdout of a week that is gone")
	}
	if !scheduler.Evicted(account.ProviderClaude, "current") {
		t.Fatal("control: the same reading inside the current week must still evict")
	}
}
