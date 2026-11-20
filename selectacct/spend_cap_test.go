package selectacct

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

// spendCapWindows is a usage-based Codex seat as the proxy scores it
// (PATCHES.md §42): rate_limit is null, and the one meter is the workspace
// spend cap, published as an account-wide window with a nominal 30-day length.
func spendCapWindows(usedPercent float64, resetAfterSeconds int64) []LimitWindow {
	return []LimitWindow{{Name: "spend", UsedPercent: usedPercent, LimitWindowSeconds: 30 * 24 * 60 * 60, ResetAfterSeconds: resetAfterSeconds, SpendCap: true}}
}

// A spend cap is long, but it is billed: what is left of it at reset_at is
// money not spent, not budget forfeited at a roll. So it gates the seat on its
// real headroom -- the floor and exhaustion still apply -- but it never earns
// a weekly pace or the endgame drain. With a pace, a seat 12h from its monthly
// reset was drained first and outranked a subscription seat whose prepaid week
// still had 60% to spend: the router chose to spend real money while prepaid
// budget expired.
func TestSpendCapGatesButNeverPacesOrDrains(t *testing.T) {
	now := time.Now()
	billed := ScoreFromLimitWindowsAt("billed", 0, spendCapWindows(50, 12*60*60), now)

	if billed.Headroom != 0.5 || billed.ShortHeadroom != 0.5 {
		t.Fatalf("Headroom/ShortHeadroom = %.2f/%.2f, want 0.50/0.50 from the cap", billed.Headroom, billed.ShortHeadroom)
	}
	if !billed.WeeklyOnly {
		t.Fatal("a seat whose only meter is its spend cap has no short window; it must read WeeklyOnly")
	}
	if _, ok := billed.weeklyPace(now); ok {
		t.Fatalf("spend cap earned a weekly pace (WeeklyHeadroom %.2f, reset %v)", billed.WeeklyHeadroom, billed.WeeklyResetAt)
	}
	if billed.inEndgameWindow(now) || billed.endgameDrainEligible(now) {
		t.Fatal("a spend cap 12h from reset_at is in endgame drain; that spends billed usage first")
	}
	if !billed.usableForNewSession() {
		t.Fatal("a half-spent cap must still take new sessions")
	}
	if verdict := NewScheduler([]Score{billed}).NewSessionVerdict("", "billed", now); verdict.Burndown || !verdict.Eligible {
		t.Fatalf("published verdict = %+v, want eligible and not burndown (a peer reads this)", verdict)
	}

	// Gated on real headroom: the floor and exhaustion still hold.
	if nearly := ScoreFromLimitWindowsAt("nearly", 0, spendCapWindows(99, 12*60*60), now); nearly.usableForNewSession() {
		t.Fatalf("a cap at 99%% is usable for a new session: %+v", nearly)
	}
	if spent := ScoreFromLimitWindowsAt("spent", 0, spendCapWindows(100, 12*60*60), now); !spent.exhausted() {
		t.Fatalf("a cap at 100%% is not exhausted: %+v", spent)
	}

	for _, tc := range []struct {
		name         string
		billed       Score
		subscription Score
	}{
		// Month end: half the cap left, 12h to reset_at, against a Pro seat
		// with 60% of its week left and 5 days to go.
		{"month end", billed, ScoreFromLimitWindowsAt("subscription", 0, codexWindows(40, 5*24*60*60), now)},
		// Mid-month: 80% of the cap left with 20 days to go, against a Pro
		// seat with 10% of its week left and 3 days to go. On pace the billed
		// seat won this too.
		{"mid-month", ScoreFromLimitWindowsAt("billed", 0, spendCapWindows(20, 20*24*60*60), now), ScoreFromLimitWindowsAt("subscription", 0, codexWindows(90, 3*24*60*60), now)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheduler := NewScheduler([]Score{tc.billed, tc.subscription})
			for _, allowEndgame := range []bool{true, false} {
				sorted, err := scheduler.rankAt([]account.Account{
					// Listed first and alphabetically first, so a comparator that
					// falls through to the id tiebreak picks it.
					{ID: "billed", AuthMode: account.AuthModeOAuth},
					{ID: "subscription", AuthMode: account.AuthModeOAuth},
				}, allowEndgame, now)
				if err != nil {
					t.Fatalf("rank: %v", err)
				}
				if sorted[0].ID != "subscription" {
					_, why := scheduler.compare(sorted[0], sorted[1], now, allowEndgame)
					t.Fatalf("allowEndgame=%v: ranked %q first (%s), want the subscription seat's prepaid week spent before billed usage",
						allowEndgame, sorted[0].ID, why)
				}
			}
		})
	}
}

// Without a paced seat to prefer, the billed seat still serves: a spend cap is
// capacity, just not the capacity spent first.
func TestSpendCapSeatServesWhenNoPacedSeatCan(t *testing.T) {
	now := time.Now()
	scheduler := NewScheduler([]Score{
		ScoreFromLimitWindowsAt("billed", 0, spendCapWindows(50, 12*60*60), now),
		// Under the new-session floor, so not usable.
		ScoreFromLimitWindowsAt("subscription", 0, codexWindows(97, 5*24*60*60), now),
	})
	sorted, err := scheduler.rankAt([]account.Account{
		{ID: "subscription", AuthMode: account.AuthModeOAuth},
		{ID: "billed", AuthMode: account.AuthModeOAuth},
	}, true, now)
	if err != nil {
		t.Fatalf("rank: %v", err)
	}
	if sorted[0].ID != "billed" {
		t.Fatalf("ranked %q first, want billed -- the only seat above the floor", sorted[0].ID)
	}
}
