package selectacct

import (
	"math/rand"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

// A single shared deadline, so the three paces differ only by the headroom
// numerator. Stamping each score with its own time.Now() would inject
// microseconds of clock noise into the very comparison under test.
var weeklyResetAt = time.Now().Add(7 * 24 * time.Hour)

func fixedWeeklyReset() time.Time { return weeklyResetAt }

// The real production Codex window shape, as published by every account on
// both daemons on 2026-08-11. OpenAI removed the 5-hour bucket around
// 2026-07-12 (it was returned as `primary` before then, with the week as
// `secondary`); since then a Codex plan publishes ONE account-wide meter --
// `primary` at 604800s -- plus a per-model Spark pool, and no short window at
// all. Fixtures that model `primary` as 5h describe a shape upstream stopped
// sending, so every assertion built on them passes vacuously.
func codexWindows(usedPercent float64, resetAfterSeconds int64) []LimitWindow {
	return []LimitWindow{
		{Name: "primary", UsedPercent: usedPercent, LimitWindowSeconds: 604800, ResetAfterSeconds: resetAfterSeconds},
		{Name: "GPT-5.3-Codex-Spark/primary", Feature: "GPT-5.3-Codex-Spark", UsedPercent: 0, LimitWindowSeconds: 604800, ResetAfterSeconds: 604800},
	}
}

// The shape contract itself. If upstream ever restores a 5h bucket this test
// is where the change surfaces, instead of silently re-arming the burn-rate
// gate underneath a scheduler tuned for a weekly-only meter.
func TestCodexPublishesNoShortWindow(t *testing.T) {
	for _, window := range codexWindows(10, 500000) {
		if isShortWindow(window) {
			t.Fatalf("window %q (%ds) classified short; codex has published weekly-only since 2026-07-12",
				window.Name, window.LimitWindowSeconds)
		}
	}
	score := ScoreFromLimitWindows("codex", 0, codexWindows(10, 500000))
	if score.ShortWindowSeconds != 0 {
		t.Fatalf("ShortWindowSeconds = %d, want 0 (no short window exists)", score.ShortWindowSeconds)
	}
	if score.WeeklyHeadroom <= 0 {
		t.Fatal("weekly headroom unset; the weekly meter is the only one codex has")
	}
}

// The forfeit bug. A Codex weekly window is a tumbling window: it starts on
// first use and then counts down to zero at 1s/s whether or not anyone spends
// (measured on seat-e8@example.com: used=0 throughout, reset 577390 ->
// 574981 over 37 minutes). So budget left on an OPENED account does not keep
// -- it expires. Refusing new sessions at 60% used forfeits the last 40% of
// every opened week, which is exactly the stranding usableForNewSession's own
// doc comment claims to have removed.
func TestCodexWeeklyGateIsAThinExhaustionFloor(t *testing.T) {
	for _, tc := range []struct {
		name       string
		used       float64
		wantUsable bool
	}{
		{"untouched week", 0, true},
		{"half spent", 50, true},
		// The old 0.40 cliff sat here. Both sides must now be usable: this
		// budget expires when the window rolls, so refusing it is a forfeit.
		{"just above the old floor", 60, true},
		{"just below the old floor", 61, true},
		{"deep into the week", 90, true},
		// The thin floor: a sliver left cannot sustain a new session.
		{"at the exhaustion floor", 96, false},
		{"fully spent", 100, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			score := ScoreFromLimitWindows("codex", 0, codexWindows(tc.used, 300000))
			if got := score.usableForNewSession(); got != tc.wantUsable {
				t.Fatalf("usableForNewSession(%.0f%% used) = %v, want %v (headroom %.2f)",
					tc.used, got, tc.wantUsable, score.ShortHeadroom)
			}
		})
	}
}

// "First to refresh, we exhaust first." Among accounts holding equal budget,
// the one whose window rolls soonest must win, because its budget is the only
// one with a deadline. A pristine account (reset == length == 604800) has not
// started its clock and loses nothing by waiting, so it ranks last.
func TestCodexPrefersTheSoonestRefresh(t *testing.T) {
	scheduler := NewScheduler([]Score{
		ScoreFromLimitWindows("pristine", 0, codexWindows(0, 604800)),
		ScoreFromLimitWindows("closing-soon", 0, codexWindows(0, 200000)),
		ScoreFromLimitWindows("mid-week", 0, codexWindows(0, 400000)),
	})
	got, err := scheduler.Pick([]account.Account{
		{ID: "pristine", AuthMode: account.AuthModeOAuth},
		{ID: "mid-week", AuthMode: account.AuthModeOAuth},
		{ID: "closing-soon", AuthMode: account.AuthModeOAuth},
	})
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if got.ID != "closing-soon" {
		t.Fatalf("picked %q, want closing-soon (its budget is the only one with a deadline)", got.ID)
	}
}

// spreadAdjusted must ride on live concurrency, not on the lifetime session
// ledger. ~/.subrouter/sessions.json is never pruned -- proxy.go:244-250 says
// so in as many words -- and measured counts today span 1..45, which turns a
// "±13% nudge" into a 6.7x multiplier swing (0.870 vs 0.129) driven by
// eight-day-old history rather than by anything the account is doing now.
func TestSpreadAdjustedRidesOnLiveConcurrencyNotLifetimeSessions(t *testing.T) {
	const value = 1.0

	busyLifetime := Score{Sessions: 45}
	freshLifetime := Score{Sessions: 1}
	if busyLifetime.spreadAdjusted(value) != freshLifetime.spreadAdjusted(value) {
		t.Fatalf("lifetime sessions changed the ranking value (45 -> %.4f, 1 -> %.4f); "+
			"the ledger is never pruned, so this penalises history rather than load",
			busyLifetime.spreadAdjusted(value), freshLifetime.spreadAdjusted(value))
	}

	// Live concurrency still influences, and still only influences: at the
	// realistic ceiling it must not be able to override a 2x capacity gap.
	idle := Score{LiveSessions: 0}
	loaded := Score{LiveSessions: 3}
	if loaded.spreadAdjusted(value) >= idle.spreadAdjusted(value) {
		t.Fatal("live concurrency did not discount the ranking value")
	}
	if got := idle.spreadAdjusted(value) / loaded.spreadAdjusted(value); got > 2 {
		t.Fatalf("live-session discount spans %.2fx at 3 concurrent sessions; "+
			"sessions must influence the pick, not decide it", got)
	}
}

// The comparator must be a strict weak ordering. A relative equality band is
// not transitive: with paces 1.000e-6 / 1.008e-6 / 1.016e-6 each neighbouring
// pair falls inside a 1% band while the outer pair does not, so A~B, B~C, C>A.
// sort.SliceStable then returns whichever element the candidate order happened
// to leave in front, and the "winner" changes with the shuffle.
func TestPickIsIndependentOfCandidateOrder(t *testing.T) {
	// Named so the alphabetical last-resort tiebreak DISAGREES with the
	// correct answer: alpha is the slowest pace but sorts first. Without that
	// the test passes for the wrong reason, since a comparator that gives up
	// and falls through to ID would still land on the right account.
	scores := []Score{
		{AccountID: "alpha", Headroom: 1, ShortHeadroom: 1, WeeklyHeadroom: 0.500, WeeklyResetAt: fixedWeeklyReset()},
		{AccountID: "bravo", Headroom: 1, ShortHeadroom: 1, WeeklyHeadroom: 0.504, WeeklyResetAt: fixedWeeklyReset()},
		{AccountID: "charlie", Headroom: 1, ShortHeadroom: 1, WeeklyHeadroom: 0.508, WeeklyResetAt: fixedWeeklyReset()},
	}
	scheduler := NewScheduler(scores)

	ids := []string{"alpha", "bravo", "charlie"}
	var winners []string
	for _, order := range [][]int{{0, 1, 2}, {2, 1, 0}, {1, 0, 2}, {0, 2, 1}, {2, 0, 1}, {1, 2, 0}} {
		candidates := make([]account.Account, 0, len(order))
		for _, i := range order {
			candidates = append(candidates, account.Account{ID: ids[i], AuthMode: account.AuthModeOAuth})
		}
		got, err := scheduler.Pick(candidates)
		if err != nil {
			t.Fatalf("pick: %v", err)
		}
		winners = append(winners, got.ID)
	}
	for _, winner := range winners {
		if winner != winners[0] {
			t.Fatalf("pick depends on candidate order: got %v across permutations; "+
				"the comparator is not a strict weak ordering", winners)
		}
	}
	if winners[0] != "charlie" {
		t.Fatalf("picked %q, want charlie (the genuinely fastest pace)", winners[0])
	}

	// The same property over random pools carrying PATCHES §56's inputs -- live
	// and expired reset holds, weeks under a quarter, peers with and without a
	// verdict: every permutation must produce the same whole ranking, in both
	// orderings. One clock for all of them, as pick uses one for its sort.
	rng := rand.New(rand.NewSource(5601))
	now := time.Now()
	// Steps the 2x and an early burndown decided. A peer's published "reset"
	// cause is not counted: it decides even with every hold ignored.
	holdWins, earlyBurndowns := 0, 0
	for trial := 0; trial < 300; trial++ {
		accts, scores := randomResetPool(rng, now, 3+rng.Intn(2), true)
		scheduler := NewScheduler(scores)
		for _, allowEndgame := range []bool{true, false} {
			want, err := scheduler.rankAt(accts, allowEndgame, now)
			if err != nil {
				t.Fatalf("rank: %v", err)
			}
			for i := 0; i+1 < len(want); i++ {
				_, reason := scheduler.compare(want[i], want[i+1], now, allowEndgame)
				if reason == reasonResetHold2x {
					holdWins++
				}
				if reason == reasonEarlyBurndown && !scheduler.score(want[i].Provider, want[i].ID).Remote {
					earlyBurndowns++
				}
			}
			for _, order := range permutations(len(accts)) {
				shuffled := make([]account.Account, 0, len(order))
				for _, i := range order {
					shuffled = append(shuffled, accts[i])
				}
				got, err := scheduler.rankAt(shuffled, allowEndgame, now)
				if err != nil {
					t.Fatalf("rank: %v", err)
				}
				for i := range got {
					if got[i].ID != want[i].ID {
						t.Fatalf("trial %d (endgame=%v): order %v ranked %v, the identity order ranked %v; "+
							"the comparator is not a strict weak ordering", trial, allowEndgame, order, accountIDs(got), accountIDs(want))
					}
				}
			}
		}
	}
	if holdWins == 0 || earlyBurndowns == 0 {
		t.Fatalf("the random pools never ran through a §56 clause: reset-hold 2x %d, local early burndown %d", holdWins, earlyBurndowns)
	}
	t.Logf("§56 clauses decided: reset-hold 2x %d, local early burndown %d", holdWins, earlyBurndowns)
}

// permutations lists every ordering of 0..n-1.
func permutations(n int) [][]int {
	if n == 0 {
		return [][]int{{}}
	}
	var out [][]int
	for _, rest := range permutations(n - 1) {
		for at := 0; at <= len(rest); at++ {
			order := append(append(append([]int{}, rest[:at]...), n-1), rest[at:]...)
			out = append(out, order)
		}
	}
	return out
}

func accountIDs(accts []account.Account) []string {
	ids := make([]string, 0, len(accts))
	for _, acct := range accts {
		ids = append(ids, acct.ID)
	}
	return ids
}
