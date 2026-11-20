package selectacct

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

// The endgame drain must not outrank the exhaustion floor.
//
// MinShortHeadroomFloor's own doc comment calls itself "an absolute backstop
// under the rate rule: however close the reset is, an account with almost
// nothing left should not be handed a new session". routableForNewSession is
// `usableForNewSession() || endgameDrainEligible()`, and endgameDrainEligible
// never consulted that floor -- so the one case the floor names in as many
// words (a close reset) was the exact case that bypassed it. A Codex account
// with 1% of its week left, inside the 24h roll window, was promoted to
// selection tier 0 and then ranked ABOVE every healthy account by compare()'s
// endgame clause: picked BECAUSE it was nearly empty.
func TestEndgameDrainDoesNotBypassTheExhaustionFloor(t *testing.T) {
	now := time.Now()
	// 99% spent, rolling in 12h: inside the endgame window, under the floor.
	drained := ScoreFromLimitWindowsAt("drained", 0, codexWindows(99, 12*60*60), now)
	// 40% spent, rolling in 6 days: plainly healthy, plainly not in endgame.
	healthy := ScoreFromLimitWindowsAt("healthy", 0, codexWindows(40, 6*24*60*60), now)

	if drained.ShortHeadroom >= MinShortHeadroomFloor {
		t.Fatalf("fixture drift: drained headroom %.3f is not under the %.2f floor",
			drained.ShortHeadroom, MinShortHeadroomFloor)
	}
	if !drained.inEndgameWindow(now) {
		t.Fatal("fixture drift: drained account is not inside the endgame window")
	}

	if drained.endgameDrainEligible(now) {
		t.Fatalf("a %.0f%%-spent account is endgame-eligible; the floor is meant to be absolute",
			100*(1-drained.ShortHeadroom))
	}
	if drained.routableForNewSession(now, true) {
		t.Fatal("drained account is routable for a new session with endgame allowed")
	}

	scheduler := NewScheduler([]Score{drained, healthy})
	sorted, err := scheduler.rankAt([]account.Account{
		{ID: "drained", AuthMode: account.AuthModeOAuth},
		{ID: "healthy", AuthMode: account.AuthModeOAuth},
	}, true, now)
	if err != nil {
		t.Fatalf("rank: %v", err)
	}
	if sorted[0].ID != "healthy" {
		t.Fatalf("ranked %q first, want healthy -- the drained account won on the endgame clause", sorted[0].ID)
	}
}

// The drain itself must survive: an account inside the roll window that still
// holds real budget is exactly what endgame exists to spend, and it must still
// outrank a healthy account whose budget is not expiring.
func TestEndgameDrainStillPrefersARollingAccountThatHasBudget(t *testing.T) {
	now := time.Now()
	rolling := ScoreFromLimitWindowsAt("rolling", 0, codexWindows(50, 12*60*60), now)
	healthy := ScoreFromLimitWindowsAt("healthy", 0, codexWindows(40, 6*24*60*60), now)

	if !rolling.endgameDrainEligible(now) {
		t.Fatal("a half-spent account inside the roll window lost its drain eligibility")
	}

	scheduler := NewScheduler([]Score{rolling, healthy})
	sorted, err := scheduler.rankAt([]account.Account{
		{ID: "healthy", AuthMode: account.AuthModeOAuth},
		{ID: "rolling", AuthMode: account.AuthModeOAuth},
	}, true, now)
	if err != nil {
		t.Fatalf("rank: %v", err)
	}
	if sorted[0].ID != "rolling" {
		t.Fatalf("ranked %q first, want rolling -- its budget expires at the roll either way", sorted[0].ID)
	}
}
