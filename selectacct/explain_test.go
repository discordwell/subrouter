package selectacct

import (
	"math/rand"
	"strconv"
	"testing"
	"time"

	"github.com/discordwell/subrouter/account"
)

// The point of Explain is that it is not a second ranking rule. These tests pin
// that: if someone reintroduces a private comparator in either function, the
// randomised agreement test fails.

func explainAccount(id string, provider account.Provider) account.Account {
	return account.Account{ID: id, Provider: provider, AuthMode: account.AuthModeOAuth}
}

func TestExplainWinnerIsPickWinner(t *testing.T) {
	rng := rand.New(rand.NewSource(20260812))
	now := time.Now()
	// How often a §56 clause decided a step, so the invariant is shown to run
	// through them rather than assumed to.
	earlyBurndowns, holdWins, steadyDemotions := 0, 0, 0
	for trial := 0; trial < 400; trial++ {
		count := 2 + rng.Intn(7)
		candidates := make([]account.Account, 0, count)
		scores := make([]Score, 0, count)
		for i := 0; i < count; i++ {
			id := "acct-" + strconv.Itoa(i)
			provider := account.ProviderClaude
			if rng.Intn(2) == 0 {
				provider = account.ProviderCodex
			}
			candidates = append(candidates, explainAccount(id, provider))
			score := Score{
				AccountID:              id,
				Provider:               provider,
				Headroom:               rng.Float64(),
				ShortHeadroom:          rng.Float64(),
				ShortResetAfterSeconds: int64(rng.Intn(18000)),
				ShortWindowSeconds:     18000,
				WeeklyHeadroom:         rng.Float64(),
				LiveSessions:           rng.Intn(4),
				Fresh:                  true,
			}
			if rng.Intn(3) == 0 {
				score.WeeklyOnly = true
				score.ShortWindowSeconds = 0
				score.ShortResetAfterSeconds = 0
			}
			if rng.Intn(2) == 0 {
				// Spread deadlines across and outside the endgame window so the
				// drain clauses are exercised, not just the pace ones.
				score.WeeklyResetAt = now.Add(time.Duration(rng.Intn(7*24*3600)) * time.Second)
			}
			// PATCHES §56: a measured week under a quarter about half the time,
			// and a saved reset on a third of the accounts -- half of those
			// already expired -- so early burndown and the 2x are exercised.
			score.WeeklyRemaining = rng.Float64() * 0.5
			if rng.Intn(3) == 0 {
				score.ResetHeldUntil = now.Add(time.Duration(rng.Intn(14*24*3600)-7*24*3600) * time.Second)
			}
			scores = append(scores, score)
		}
		scheduler := NewScheduler(scores)

		picked, pickErr := scheduler.Pick(candidates)
		explained, explainErr := scheduler.Explain(candidates)
		if pickErr != nil || explainErr != nil {
			t.Fatalf("trial %d: pick err %v, explain err %v", trial, pickErr, explainErr)
		}
		if len(explained) != len(candidates) {
			t.Fatalf("trial %d: explain returned %d rows for %d candidates", trial, len(explained), len(candidates))
		}
		if explained[0].Account.ID != picked.ID {
			t.Fatalf("trial %d: Explain ranked %q first but Pick chose %q",
				trial, explained[0].Account.ID, picked.ID)
		}
		for i, row := range explained {
			if row.Reason == "" {
				t.Fatalf("trial %d: rank %d (%s) carries no reason", trial, i, row.Account.ID)
			}
			switch row.Reason {
			case reasonEarlyBurndown:
				earlyBurndowns++
			case reasonResetHold2x:
				holdWins++
			}
		}
		if explained[len(explained)-1].Reason != "last" {
			t.Fatalf("trial %d: bottom row reason = %q, want \"last\"", trial, explained[len(explained)-1].Reason)
		}

		// Same invariant for the steady ordering, which disables endgame drain
		// and would diverge if ExplainSteady forked PickSteady.
		steadyPicked, err := scheduler.PickSteady(candidates)
		if err != nil {
			t.Fatalf("trial %d: PickSteady: %v", trial, err)
		}
		steadyExplained, err := scheduler.ExplainSteady(candidates)
		if err != nil {
			t.Fatalf("trial %d: ExplainSteady: %v", trial, err)
		}
		if steadyExplained[0].Account.ID != steadyPicked.ID {
			t.Fatalf("trial %d: ExplainSteady ranked %q first but PickSteady chose %q",
				trial, steadyExplained[0].Account.ID, steadyPicked.ID)
		}
		for _, row := range steadyExplained {
			if row.Reason == reasonSteadyEarlyBurndown {
				steadyDemotions++
			}
		}
	}
	if earlyBurndowns == 0 || holdWins == 0 || steadyDemotions == 0 {
		t.Fatalf("the randomiser never reached a §56 clause: early burndown %d, reset-hold 2x %d, steady demotion %d",
			earlyBurndowns, holdWins, steadyDemotions)
	}
	t.Logf("§56 clauses decided: early burndown %d, reset-hold 2x %d, steady demotion %d", earlyBurndowns, holdWins, steadyDemotions)
}

// TestExplainReportsAlphabeticalTie is the finding that makes the column worth
// having: a pool of untouched Codex accounts is ordered by nothing at all, and
// the board should say so rather than implying the router had an opinion.
func TestExplainReportsAlphabeticalTie(t *testing.T) {
	var candidates []account.Account
	var scores []Score
	for _, id := range []string{"seat-l7", "seat-l12", "seat-l13"} {
		candidates = append(candidates, explainAccount(id, account.ProviderCodex))
		scores = append(scores, Score{
			AccountID:     id,
			Provider:      account.ProviderCodex,
			Headroom:      1,
			ShortHeadroom: 1,
			WeeklyOnly:    true,
			Fresh:         true,
		})
	}
	explained, err := NewScheduler(scores).Explain(candidates)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if explained[0].Account.ID != "seat-l12" {
		t.Fatalf("expected alphabetical winner, got %q", explained[0].Account.ID)
	}
	if explained[0].Reason != "tied; ordered by account id" {
		t.Fatalf("reason = %q, want the honest tie reason", explained[0].Reason)
	}
}

// TestExplainSurfacesEndgameReason pins that a real clause, not the tie
// fallback, is reported when one actually decides.
func TestExplainSurfacesEndgameReason(t *testing.T) {
	now := time.Now()
	candidates := []account.Account{
		explainAccount("steady", account.ProviderClaude),
		explainAccount("rolling", account.ProviderClaude),
	}
	scores := []Score{
		{AccountID: "steady", Provider: account.ProviderClaude, Headroom: 0.9, ShortHeadroom: 0.9,
			ShortWindowSeconds: 18000, ShortResetAfterSeconds: 9000,
			WeeklyHeadroom: 0.9, WeeklyResetAt: now.Add(6 * 24 * time.Hour), Fresh: true},
		{AccountID: "rolling", Provider: account.ProviderClaude, Headroom: 0.3, ShortHeadroom: 0.9,
			ShortWindowSeconds: 18000, ShortResetAfterSeconds: 9000,
			WeeklyHeadroom: 0.3, WeeklyResetAt: now.Add(3 * time.Hour), Fresh: true},
	}
	explained, err := NewScheduler(scores).Explain(candidates)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if explained[0].Account.ID != "rolling" {
		t.Fatalf("endgame account should rank first, got %q", explained[0].Account.ID)
	}
	want := "inside its weekly roll window; that budget expires either way"
	if explained[0].Reason != want {
		t.Fatalf("reason = %q, want %q", explained[0].Reason, want)
	}
}
