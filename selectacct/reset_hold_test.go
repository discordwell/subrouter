package selectacct

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

// PATCHES §56: an account holding an unclaimed saved reset (cedar_ember) ranks
// at ResetHoldPreferenceRatio, and below ResetBurndownWeeklyRemaining of its
// MEASURED week it joins the drain tier days before its roll, so the owner can
// claim the reset for a fresh week before the grant lapses.

const (
	reasonEarlyBurndown       = "early burndown: holds an unclaimed saved reset and under 25% of its week is left"
	reasonResetHold2x         = "higher weekly pace after the reset-hold 2x and the live-session discount"
	reasonSteadyEarlyBurndown = "the other is in early burndown on a saved reset; not a place to park a session"
)

// setResetHoldPriorityForTest sets §56's process-wide switch for the rest of
// the test and restores it in t.Cleanup. Only for tests that are not
// t.Parallel.
func setResetHoldPriorityForTest(t *testing.T, on bool) {
	t.Helper()
	prev := ResetHoldPriority()
	SetResetHoldPriority(on)
	t.Cleanup(func() { SetResetHoldPriority(prev) })
}

// resetScore is a local Claude account with exact weekly numbers, a roomy 5h
// window two hours from its reset, and a hold until heldUntil (zero: none).
func resetScore(id string, weeklyHeadroom, weeklyRemaining float64, roll, heldUntil time.Time) Score {
	return Score{
		AccountID: id, Provider: account.ProviderClaude, Fresh: true,
		Headroom: math.Min(weeklyRemaining, 0.9), ShortHeadroom: 0.9,
		ShortWindowSeconds: 5 * 3600, ShortResetAfterSeconds: 2 * 3600,
		WeeklyHeadroom: weeklyHeadroom, WeeklyRemaining: weeklyRemaining,
		WeeklyResetAt: roll, ResetHeldUntil: heldUntil,
	}
}

// resetScoreFromWindows is a local Claude account as a usage reading leaves
// it: generalUsed% of its week spent with the roll untilRoll away, the Fable
// meter half spent, 10% of the 5h window used with two hours to its reset.
// WeeklyHeadroom carries the Fable reservation only while §57's switch is on
// (setFableReservationForTest); off, the default, it is the measured week.
func resetScoreFromWindows(now time.Time, id string, generalUsed float64, untilRoll time.Duration, heldUntil time.Time) Score {
	score := ScoreFromLimitWindowsAt(id, 0, claudePoolWindows(generalUsed, 50, 10, int64(untilRoll/time.Second), 2*3600), now)
	score.Provider = account.ProviderClaude
	score.Fresh = true
	score.ResetHeldUntil = heldUntil
	return score
}

func rankedIDs(t *testing.T, scheduler Scheduler, now time.Time, allowEndgame bool, accts ...account.Account) []string {
	t.Helper()
	sorted, err := scheduler.rankAt(accts, allowEndgame, now)
	if err != nil {
		t.Fatalf("rank: %v", err)
	}
	ids := make([]string, 0, len(sorted))
	for _, acct := range sorted {
		ids = append(ids, acct.ID)
	}
	return ids
}

// Two accounts at the same pace: the holder wins on the 2x. Named so the
// alphabetical tie-break hands the pair to the non-holder when the 2x is gone.
func TestResetHolderOutranksAnEqualPaceNonHolder(t *testing.T) {
	now := time.Now()
	roll := now.Add(72 * time.Hour)
	plain := resetScore("alpha", 0.30, 0.60, roll, time.Time{})
	holder := resetScore("bravo", 0.30, 0.60, roll, now.Add(5*24*time.Hour))
	accts := []account.Account{claudeAccount("alpha"), claudeAccount("bravo")}

	stripped := holder
	stripped.ResetHeldUntil = time.Time{}
	if got := rankedIDs(t, NewScheduler([]Score{plain, stripped}), now, true, accts...); got[0] != "alpha" {
		t.Fatalf("calibration: without the hold the tie must fall to alpha, got %v", got)
	}

	scheduler := NewScheduler([]Score{plain, holder})
	if got := rankedIDs(t, scheduler, now, true, accts...); got[0] != "bravo" {
		t.Fatalf("ranked %v, want the holder bravo first: equal pace, and it holds a reset that lapses unclaimed", got)
	}
	if _, reason := scheduler.compare(accts[1], accts[0], now, true); reason != reasonResetHold2x {
		t.Fatalf("reason = %q, want %q", reason, reasonResetHold2x)
	}
}

// The hold takes precedence over where the account lives: a peer's holder
// ranks at twice a LOCAL non-holder, the same as a local holder does, so a box
// holder draws laptop sessions away from the laptop's own non-holders. Between
// two holders a pace tie still goes to the local one.
func TestAPeerResetHolderRanksAtTwiceALocalNonHolder(t *testing.T) {
	now := time.Now()
	roll := now.Add(72 * time.Hour)
	hold := now.Add(5 * 24 * time.Hour)
	accts := []account.Account{claudeAccount("alpha"), claudeAccount("bravo")}
	for _, tc := range []struct {
		name        string
		localWeekly float64
		localHold   time.Time
		want        string
		reason      string
	}{
		{"at the same pace the peer holder wins on its 2x", 0.30, time.Time{}, "bravo", reasonResetHold2x},
		{"a local non-holder at 1.9x the pace still loses", 0.57, time.Time{}, "bravo", reasonResetHold2x},
		{"a local non-holder more than twice as fast wins", 0.66, time.Time{}, "alpha", "higher weekly pace after the live-session discount"},
		{"against a local holder at the same pace, local is preferred", 0.30, hold, "alpha", "local pool preferred; the rate signals tied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := resetScore("alpha", tc.localWeekly, 0.70, roll, tc.localHold)
			peer := resetScore("bravo", 0.30, 0.60, roll, hold)
			peer.Remote = true
			peer.PeerVerdict = &NewSessionVerdict{Eligible: true, Reason: VerdictEligible}
			scheduler := NewScheduler([]Score{local, peer})
			sorted, err := scheduler.rankAt(accts, true, now)
			if err != nil {
				t.Fatalf("rank: %v", err)
			}
			if sorted[0].ID != tc.want {
				t.Fatalf("ranked %q first, want %q", sorted[0].ID, tc.want)
			}
			if _, reason := scheduler.compare(sorted[0], sorted[1], now, true); reason != tc.reason {
				t.Fatalf("reason = %q, want %q", reason, tc.reason)
			}
		})
	}
}

// A factor, not a tier: a non-holder spending more than twice as fast still
// wins. Here it is 2.5x.
func TestNonHolderMoreThanTwiceAsFastOutranksAHolder(t *testing.T) {
	now := time.Now()
	roll := now.Add(72 * time.Hour)
	holder := resetScore("alpha", 0.10, 0.50, roll, now.Add(5*24*time.Hour))
	fast := resetScore("bravo", 0.25, 0.50, roll, time.Time{})
	if holder.inResetBurndown(now) {
		t.Fatal("fixture drift: the holder must not be in early burndown")
	}
	scheduler := NewScheduler([]Score{holder, fast})
	accts := []account.Account{claudeAccount("alpha"), claudeAccount("bravo")}
	if got := rankedIDs(t, scheduler, now, true, accts...); got[0] != "bravo" {
		t.Fatalf("ranked %v, want bravo first: it spends 2.5x the holder's pace, more than the 2x", got)
	}
	want := "higher weekly pace after the live-session discount"
	if _, reason := scheduler.compare(accts[1], accts[0], now, true); reason != want {
		t.Fatalf("reason = %q, want %q", reason, want)
	}
}

// No live hold, no 2x: none read, one already expired (the seed carried it
// forward), one expiring this instant, and the switch off.
func TestResetHoldBoostNeedsALiveHold(t *testing.T) {
	now := time.Now()
	roll := now.Add(72 * time.Hour)
	accts := []account.Account{claudeAccount("alpha"), claudeAccount("bravo")}
	for _, tc := range []struct {
		name string
		held time.Time
		on   bool
	}{
		{"no hold", time.Time{}, true},
		{"expired a minute ago", now.Add(-time.Minute), true},
		{"expiring this instant", now, true},
		{"switch off", now.Add(5 * 24 * time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setResetHoldPriorityForTest(t, tc.on)
			plain := resetScore("alpha", 0.30, 0.60, roll, time.Time{})
			holder := resetScore("bravo", 0.30, 0.60, roll, tc.held)
			if holder.HoldsReset(now) {
				t.Fatalf("HoldsReset is true for %s; that account must rank at 1x", tc.name)
			}
			if got := rankedIDs(t, NewScheduler([]Score{plain, holder}), now, true, accts...); got[0] != "alpha" {
				t.Fatalf("ranked %v, want alpha first on the ID tie: no live hold means no 2x", got)
			}
		})
	}
}

// fifteen-beta's shape: under a quarter of its week measured, the roll
// three days out, a saved reset held. It drains now, above accounts with far
// more pace; at 26%, or without a hold, or unmeasured, or on a reading from
// before its roll, it does not, and at its 5h wall it publishes no allowance.
func TestResetHolderUnderAQuarterOfItsWeekBurnsDownEarly(t *testing.T) {
	// With the Fable reservation on (SUBROUTER_FABLE_RESERVATION=1, PATCHES
	// §57) WeeklyHeadroom is about half the measured week, which is what
	// makes the 26% case below prove the trigger reads the measured week.
	// TestEarlyBurndownTakesTheFullTaperWithTheReservationOff is the default.
	setFableReservationForTest(t, true)
	now := time.Now()
	held := now.Add(5 * 24 * time.Hour)
	steadyA := resetScoreFromWindows(now, "steady-a", 30, 48*time.Hour, time.Time{})
	steadyB := resetScoreFromWindows(now, "steady-b", 50, 96*time.Hour, time.Time{})
	accts := []account.Account{claudeAccount("steady-a"), claudeAccount("steady-b"), claudeAccount("early")}

	t.Run("24% measured with a hold drains first", func(t *testing.T) {
		early := resetScoreFromWindows(now, "early", 76, 72*time.Hour, held)
		if early.WeeklyRemaining >= ResetBurndownWeeklyRemaining || early.WeeklyHeadroom >= early.WeeklyRemaining {
			t.Fatalf("fixture drift: want 24%% measured with the Fable reservation off WeeklyHeadroom, got remaining %.3f headroom %.3f",
				early.WeeklyRemaining, early.WeeklyHeadroom)
		}
		if early.inEndgameWindow(now) {
			t.Fatal("fixture drift: the roll must be days away")
		}
		if !early.endgameDrainEligible(now) || early.BurndownCause(now) != BurndownCauseReset {
			t.Fatalf("drain-eligible=%v cause=%q, want true and %q",
				early.endgameDrainEligible(now), early.BurndownCause(now), BurndownCauseReset)
		}
		scheduler := NewScheduler([]Score{steadyA, steadyB, early})
		sorted, err := scheduler.rankAt(accts, true, now)
		if err != nil {
			t.Fatalf("rank: %v", err)
		}
		if sorted[0].ID != "early" {
			t.Fatalf("ranked %q first, want early: it is in the drain tier", sorted[0].ID)
		}
		if _, reason := scheduler.compare(sorted[0], sorted[1], now, true); reason != reasonEarlyBurndown {
			t.Fatalf("reason = %q, want %q", reason, reasonEarlyBurndown)
		}
	})

	unmeasured := resetScoreFromWindows(now, "early", 76, 72*time.Hour, held)
	unmeasured.WeeklyRemaining = 0
	// A seed measured two hours ago, an hour before its roll, and carried
	// across it: its 10% is last week's, and the roll has refilled the week.
	rolled := resetScoreFromWindows(now.Add(-2*time.Hour), "early", 90, time.Hour, held)
	if !rolled.WeeklyResetAt.Before(now) {
		t.Fatalf("fixture drift: the carried seed's roll %v must be past", rolled.WeeklyResetAt)
	}
	for _, tc := range []struct {
		name  string
		score Score
	}{
		{"26% measured with a hold", resetScoreFromWindows(now, "early", 74, 72*time.Hour, held)},
		{"10% measured without a hold", resetScoreFromWindows(now, "early", 90, 72*time.Hour, time.Time{})},
		{"an unmeasured week with a hold", unmeasured},
		{"10% measured with a hold, on a seed carried across its roll", rolled},
	} {
		t.Run(tc.name+" does not", func(t *testing.T) {
			if tc.score.inResetBurndown(now) || tc.score.endgameDrainEligible(now) || tc.score.BurndownCause(now) != "" {
				t.Fatalf("early burndown=%v drain-eligible=%v cause=%q, want none",
					tc.score.inResetBurndown(now), tc.score.endgameDrainEligible(now), tc.score.BurndownCause(now))
			}
			if got := rankedIDs(t, NewScheduler([]Score{steadyA, steadyB, tc.score}), now, true, accts...); got[0] != "steady-a" {
				t.Fatalf("ranked %v, want steady-a first on pace", got)
			}
		})
	}

	// At its 5h wall a holder with a fifth of its week left is exhausted, not
	// burning down: its verdict names no cause and allows no session.
	t.Run("20% measured with a hold, at its 5h wall, publishes no allowance", func(t *testing.T) {
		walled := ScoreFromLimitWindowsAt("early", 0, claudePoolWindows(80, 50, 100, 72*3600, 2*3600), now)
		walled.Provider, walled.Fresh, walled.ResetHeldUntil = account.ProviderClaude, true, held
		if walled.ShortHeadroom != 0 || !walled.exhausted() ||
			walled.WeeklyRemaining <= 0 || walled.WeeklyRemaining >= ResetBurndownWeeklyRemaining {
			t.Fatalf("fixture drift: want an exhausted 5h and a fifth of the week, got short %.3f remaining %.3f",
				walled.ShortHeadroom, walled.WeeklyRemaining)
		}
		verdict := NewScheduler([]Score{walled}).NewSessionVerdict(account.ProviderClaude, "early", now)
		if verdict.BurndownCause != "" || verdict.AllowedSessions != 0 || verdict.Burndown || verdict.Reason != VerdictExhausted {
			t.Fatalf("verdict %+v, want exhausted with no burndown cause and no allowance", verdict)
		}
	})
}

// A peer's early burndown is its owner's call: only the owner measures the
// week. The numbers a peer score carries never re-derive it; the verdict does,
// and this daemon's switch does not override it (§56 decision 5).
func TestPeerResetHolderDrainsEarlyOnlyOnItsOwnersVerdict(t *testing.T) {
	now := time.Now()
	steady := resetScoreFromWindows(now, "steady-a", 30, 48*time.Hour, time.Time{})
	peer := resetScore("there", 0.05, 0.10, now.Add(72*time.Hour), now.Add(5*24*time.Hour))
	peer.Remote = true
	accts := []account.Account{claudeAccount("steady-a"), claudeAccount("there")}

	t.Run("without a verdict it is not re-derived", func(t *testing.T) {
		if peer.inResetBurndown(now) || peer.endgameDrainEligible(now) || peer.BurndownCause(now) != "" {
			t.Fatalf("early burndown=%v drain-eligible=%v cause=%q from a peer's numbers",
				peer.inResetBurndown(now), peer.endgameDrainEligible(now), peer.BurndownCause(now))
		}
		if got := rankedIDs(t, NewScheduler([]Score{steady, peer}), now, true, accts...); got[0] != "steady-a" {
			t.Fatalf("ranked %v, want the steady local account first", got)
		}
	})

	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprintf("its owner's verdict drains it (switch on=%v)", on), func(t *testing.T) {
			setResetHoldPriorityForTest(t, on)
			withVerdict := peer
			withVerdict.PeerVerdict = &NewSessionVerdict{
				Eligible: true, Reason: VerdictBurndown, Burndown: true, AllowedSessions: 1, BurndownCause: BurndownCauseReset,
			}
			if !withVerdict.endgameDrainEligible(now) || withVerdict.BurndownCause(now) != BurndownCauseReset {
				t.Fatalf("drain-eligible=%v cause=%q, want the verdict's", withVerdict.endgameDrainEligible(now), withVerdict.BurndownCause(now))
			}
			scheduler := NewScheduler([]Score{steady, withVerdict})
			sorted, err := scheduler.rankAt(accts, true, now)
			if err != nil {
				t.Fatalf("rank: %v", err)
			}
			if sorted[0].ID != "there" {
				t.Fatalf("ranked %q first, want the peer its owner is burning down", sorted[0].ID)
			}
			if _, reason := scheduler.compare(sorted[0], sorted[1], now, true); reason != reasonEarlyBurndown {
				t.Fatalf("reason = %q, want %q", reason, reasonEarlyBurndown)
			}
		})
	}
}

// Inside the drain tier the roll still orders: a 24h-roll account's budget
// expires first, so it leads an early-burndown holder days from its roll --
// even on expiry day, with the hold lapsing before that roll (§56 decision 2:
// the urgency deadline stays the roll).
func TestARollingDrainAccountOutranksAnEarlyBurndownOne(t *testing.T) {
	now := time.Now()
	rolling := resetScore("bravo", 0.30, 0.30, now.Add(16*time.Hour), time.Time{})
	early := resetScore("alpha", 0.10, 0.20, now.Add(72*time.Hour), now.Add(10*time.Hour))
	if rolling.BurndownCause(now) != BurndownCauseRoll || !rolling.endgameDrainEligible(now) {
		t.Fatal("calibration: the rolling account must drain on its roll")
	}
	if early.BurndownCause(now) != BurndownCauseReset || !early.endgameDrainEligible(now) {
		t.Fatal("calibration: the holder must be in early burndown")
	}
	scheduler := NewScheduler([]Score{rolling, early})
	sorted, err := scheduler.rankAt([]account.Account{claudeAccount("alpha"), claudeAccount("bravo")}, true, now)
	if err != nil {
		t.Fatalf("rank: %v", err)
	}
	if sorted[0].ID != "bravo" {
		t.Fatalf("ranked %q first, want the account rolling in 16h", sorted[0].ID)
	}
	want := "both draining; this one rolls sooner for the load it is carrying"
	if _, reason := scheduler.compare(sorted[0], sorted[1], now, true); reason != want {
		t.Fatalf("reason = %q, want %q", reason, want)
	}
}

// The proxy sets the hold on the account score only, and every Fable request
// ranks the Fable pool's score: the pool inherits the hold and the account's
// measured week, so a holder burns down early on its model pools too.
func TestModelPoolsInheritTheResetHold(t *testing.T) {
	now := time.Now()
	acct := resetScoreFromWindows(now, "holder", 78, 72*time.Hour, now.Add(5*24*time.Hour))
	if !acct.ModelScores["claudefable"].ResetHeldUntil.IsZero() {
		t.Fatal("fixture drift: the pool entry must not carry the hold itself")
	}
	for _, model := range []string{"claude-fable", "claude-opus"} {
		t.Run(model, func(t *testing.T) {
			pool := NewScheduler([]Score{acct}).ForModel(model)
			score := pool.score(account.ProviderClaude, "holder")
			if !score.ResetHeldUntil.Equal(acct.ResetHeldUntil) || score.WeeklyRemaining != acct.WeeklyRemaining {
				t.Fatalf("pool hold %v remaining %.3f, want the account's %v and %.3f",
					score.ResetHeldUntil, score.WeeklyRemaining, acct.ResetHeldUntil, acct.WeeklyRemaining)
			}
			verdict := pool.NewSessionVerdict(account.ProviderClaude, "holder", now)
			if !verdict.Burndown || verdict.BurndownCause != BurndownCauseReset || verdict.AllowedSessions < 1 {
				t.Fatalf("pool verdict %+v, want early burndown with an allowance", verdict)
			}
		})
	}
}

// The published verdict: Burndown and an allowance in early burndown, and the
// cause, roll winning when a holder is also inside its 24h window. A peer
// sizes the account by AllowedSessions, so a reset-burndown verdict with 0
// would read as a bare burndown with no taper.
func TestNewSessionVerdictNamesTheBurndownCause(t *testing.T) {
	now := time.Now()
	held := now.Add(5 * 24 * time.Hour)
	for _, tc := range []struct {
		name  string
		score Score
		live  int
		want  NewSessionVerdict
	}{
		{"early burndown", resetScore("a", 0.12, 0.24, now.Add(72*time.Hour), held), 0,
			NewSessionVerdict{Eligible: true, Reason: VerdictBurndown, Burndown: true, AllowedSessions: 2, BurndownCause: BurndownCauseReset, RefugeeCap: refugeeCapOf(1)}},
		{"early burndown at its cap", resetScore("a", 0.12, 0.24, now.Add(72*time.Hour), held), 2,
			NewSessionVerdict{Reason: VerdictAtCap, AtCap: true, LiveSessions: 2, AllowedSessions: 2, BurndownCause: BurndownCauseReset, RefugeeCap: refugeeCapOf(1)}},
		{"the 24h roll", resetScore("a", 0.30, 0.30, now.Add(16*time.Hour), time.Time{}), 0,
			NewSessionVerdict{Eligible: true, Reason: VerdictBurndown, Burndown: true, AllowedSessions: 4, BurndownCause: BurndownCauseRoll, RefugeeCap: refugeeCapOf(9)}},
		{"a holder inside its 24h roll", resetScore("a", 0.12, 0.20, now.Add(16*time.Hour), held), 0,
			NewSessionVerdict{Eligible: true, Reason: VerdictBurndown, Burndown: true, AllowedSessions: 2, BurndownCause: BurndownCauseRoll, RefugeeCap: refugeeCapOf(3)}},
		{"a holder with most of its week", resetScore("a", 0.30, 0.60, now.Add(72*time.Hour), held), 0,
			NewSessionVerdict{Eligible: true, Reason: VerdictEligible, RefugeeCap: refugeeCapOf(3)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := NewScheduler([]Score{tc.score}).
				WithLiveSessionCounts(map[string]int{ScoreKey(account.ProviderClaude, "a"): tc.live}).
				NewSessionVerdict(account.ProviderClaude, "a", now)
			if got != tc.want {
				t.Fatalf("verdict %+v, want %+v", got, tc.want)
			}
			body, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if published := strings.Contains(string(body), `"burndown_cause"`); published != (tc.want.BurndownCause != "") {
				t.Fatalf("burndown_cause published=%v in %s", published, body)
			}
		})
	}
}

// PickSteady parks an interactive session for hours: an account the router is
// running into its wall is the wrong place, whether its roll is tonight or it
// is a holder burning down early.
func TestPickSteadyRanksAnEarlyBurndownAccountLast(t *testing.T) {
	now := time.Now()
	holder := resetScore("alpha", 0.20, 0.20, now.Add(36*time.Hour), now.Add(5*24*time.Hour))
	healthy := resetScore("bravo", 0.50, 0.50, now.Add(144*time.Hour), time.Time{})
	holderPace, _ := holder.weeklyPace(now)
	healthyPace, _ := healthy.weeklyPace(now)
	if (Scheduler{}).rankingPace(holder, holderPace, now) <= (Scheduler{}).rankingPace(healthy, healthyPace, now) || !holder.inResetBurndown(now) {
		t.Fatal("calibration: the holder must be in early burndown and ahead on pace")
	}
	scheduler := NewScheduler([]Score{holder, healthy})
	accts := []account.Account{claudeAccount("alpha"), claudeAccount("bravo")}
	if got, err := scheduler.Pick(accts); err != nil || got.ID != "alpha" {
		t.Fatalf("Pick chose %q (%v), want the draining holder", got.ID, err)
	}
	if got, err := scheduler.PickSteady(accts); err != nil || got.ID != "bravo" {
		t.Fatalf("PickSteady chose %q (%v), want the healthy account", got.ID, err)
	}
	explained, err := scheduler.ExplainSteady(accts)
	if err != nil {
		t.Fatalf("ExplainSteady: %v", err)
	}
	if explained[0].Account.ID != "bravo" || explained[0].Reason != reasonSteadyEarlyBurndown {
		t.Fatalf("ExplainSteady led with %q (%q), want bravo (%q)", explained[0].Account.ID, explained[0].Reason, reasonSteadyEarlyBurndown)
	}
}

// randomResetPool is a random pool for the §56 invariants: Claude and Codex,
// local and peer, deadlines across and outside the 24h window, a measured
// week under a quarter about half the time (and sometimes unmeasured), and
// holds -- none, expired, live -- in even thirds. verdictPeers lets half the
// peers carry a verdict.
func randomResetPool(rng *rand.Rand, now time.Time, count int, verdictPeers bool) ([]account.Account, []Score) {
	accts := make([]account.Account, 0, count)
	scores := make([]Score, 0, count)
	for i := 0; i < count; i++ {
		id := "acct-" + strconv.Itoa(i)
		provider := account.ProviderClaude
		if rng.Intn(4) == 0 {
			provider = account.ProviderCodex
		}
		short := 0.02 + rng.Float64()*0.98
		remaining := rng.Float64() * 0.5
		score := Score{
			AccountID: id, Provider: provider, Fresh: true,
			Headroom: math.Max(0.01, math.Min(short, remaining)), ShortHeadroom: short,
			ShortWindowSeconds: 18000, ShortResetAfterSeconds: int64(rng.Intn(18001)),
			WeeklyHeadroom: remaining * (0.4 + 0.6*rng.Float64()), WeeklyRemaining: remaining,
			LiveSessions: rng.Intn(4),
		}
		if provider == account.ProviderCodex {
			score.WeeklyOnly, score.ShortWindowSeconds, score.ShortResetAfterSeconds = true, 0, 0
		}
		if rng.Intn(8) == 0 {
			score.WeeklyRemaining = 0
		}
		if rng.Intn(5) > 0 {
			score.WeeklyResetAt = now.Add(time.Duration(rng.Intn(7*24*3600)) * time.Second)
		}
		switch rng.Intn(3) {
		case 1:
			score.ResetHeldUntil = now.Add(-time.Duration(1+rng.Intn(3*24*3600)) * time.Second)
		case 2:
			score.ResetHeldUntil = now.Add(time.Duration(1+rng.Intn(7*24*3600)) * time.Second)
		}
		if rng.Intn(4) == 0 {
			score.Remote = true
			if verdictPeers && rng.Intn(2) == 0 {
				verdict := NewSessionVerdict{LiveSessions: score.LiveSessions}
				switch rng.Intn(4) {
				case 0:
					verdict.Eligible, verdict.Reason, verdict.Burndown = true, VerdictBurndown, true
					verdict.AllowedSessions = 1 + rng.Intn(4)
					verdict.BurndownCause = []string{BurndownCauseRoll, BurndownCauseReset, ""}[rng.Intn(3)]
				case 1:
					verdict.Reason, verdict.AtCap, verdict.AllowedSessions = VerdictAtCap, true, 1
					verdict.BurndownCause = BurndownCauseReset
				case 2:
					verdict.Eligible, verdict.Reason = true, VerdictEligible
				default:
					verdict.Reason = VerdictBurnProjection
				}
				score.PeerVerdict = &verdict
			}
		}
		accts = append(accts, account.Account{ID: id, Provider: provider, AuthMode: account.AuthModeOAuth})
		scores = append(scores, score)
	}
	return accts, scores
}

// rankingFingerprint is everything a pool's holds could move: both orderings
// with the clause behind each step, and every account's verdict.
func rankingFingerprint(t *testing.T, scores []Score, accts []account.Account, now time.Time) string {
	t.Helper()
	scheduler := NewScheduler(scores)
	var b strings.Builder
	for _, allowEndgame := range []bool{true, false} {
		sorted, err := scheduler.rankAt(accts, allowEndgame, now)
		if err != nil {
			t.Fatalf("rank: %v", err)
		}
		for i, acct := range sorted {
			b.WriteString(acct.ID)
			if i+1 < len(sorted) {
				_, reason := scheduler.compare(acct, sorted[i+1], now, allowEndgame)
				b.WriteString(" (" + reason + ") ")
			}
		}
		b.WriteString("\n")
	}
	for _, acct := range accts {
		fmt.Fprintf(&b, "%s %+v\n", acct.ID, scheduler.NewSessionVerdict(acct.Provider, acct.ID, now))
	}
	return b.String()
}

// SUBROUTER_RESET_HOLD_PRIORITY=0 must be today's router: over random pools of
// local accounts and verdict-less peers, ranking, reasons and verdicts with
// the holds equal those with the holds stripped. Peers carrying a verdict are
// out of scope by design -- their burndown is their owner's (§56 decision 5).
// Calibrated by the switch on: the same pools must rank differently somewhere,
// or this parity would hold for a feature that does nothing.
func TestResetHoldSwitchOffRanksAsIfNoAccountHeldAReset(t *testing.T) {
	setResetHoldPriorityForTest(t, false)
	rng := rand.New(rand.NewSource(56))
	now := time.Now()
	movedWithSwitchOn := 0
	for trial := 0; trial < 2000; trial++ {
		accts, held := randomResetPool(rng, now, 2+rng.Intn(5), false)
		stripped := make([]Score, len(held))
		for i, score := range held {
			score.ResetHeldUntil = time.Time{}
			stripped[i] = score
		}
		SetResetHoldPriority(false)
		if got, want := rankingFingerprint(t, held, accts, now), rankingFingerprint(t, stripped, accts, now); got != want {
			t.Fatalf("trial %d: switch off, the holds changed the ranking:\nwith holds:\n%s\nstripped:\n%s", trial, got, want)
		}
		SetResetHoldPriority(true)
		if rankingFingerprint(t, held, accts, now) != rankingFingerprint(t, stripped, accts, now) {
			movedWithSwitchOn++
		}
	}
	if movedWithSwitchOn == 0 {
		t.Fatal("calibration: with the switch on no random pool ranked differently; the parity above proves nothing")
	}
}
