package selectacct

import (
	"testing"

	"github.com/manaflow-ai/subrouter/account"
)

// Production publishes per-model weekly windows with NO reset time. Every live
// Claude account on the box read `opus-weekly … reset_seconds=0` while its
// account-wide `7d` window carried a real one.
//
// claudePoolWindows does not build that shape -- it stamps the account's
// weeklyResetSeconds onto every feature window -- so the existing endgame
// tests all run against model scores that already have weekly timing, and the
// case that actually shipped was never exercised. These fixtures reproduce the
// published shape instead of the convenient one.
//
// Why it matters: WeeklyResetAt zero means "no weekly pacing" by construction,
// so a model pool whose windows all lack a reset time deletes weeklyPace AND
// inEndgameWindow for EVERY candidate at once. The primary signal does not
// merely tie, it disappears, and ordering falls through to the 5h window --
// which cannot see a weekly roll. Measured on the live pool: an account holding
// 72% of its week and rolling in 13.6h ranked below one holding 47% with 155h
// to go, and forfeited the difference.

// modelWindowsWithoutWeeklyReset mirrors what the provider actually returns:
// account-wide 5h/7d windows with real timing, and feature weeklies with
// ResetAfterSeconds ZERO.
func modelWindowsWithoutWeeklyReset(weeklyUsedPct, shortUsedPct float64, weeklyResetSeconds, shortResetSeconds int64) []LimitWindow {
	const (
		fiveHour = int64(5 * 60 * 60)
		sevenDay = int64(7 * 24 * 60 * 60)
	)
	windows := []LimitWindow{
		{Name: "5h", UsedPercent: shortUsedPct, LimitWindowSeconds: fiveHour, ResetAfterSeconds: shortResetSeconds},
		{Name: "7d", UsedPercent: weeklyUsedPct, LimitWindowSeconds: sevenDay, ResetAfterSeconds: weeklyResetSeconds},
	}
	for _, f := range []struct{ name, feature string }{
		{"opus-weekly", "claude-opus"},
		{"sonnet-weekly", "claude-sonnet"},
	} {
		// The feature weekly itself: reported, unused, and WITHOUT a reset.
		windows = append(windows, LimitWindow{
			Name: f.name, UsedPercent: 0, LimitWindowSeconds: sevenDay,
			ResetAfterSeconds: 0, Feature: f.feature,
		})
		// The 5h copy inside the pool keeps its real timing, exactly as the
		// live payload does -- which is what made the 5h window the only
		// surviving signal.
		windows = append(windows, LimitWindow{
			Name: "5h", UsedPercent: shortUsedPct, LimitWindowSeconds: fiveHour,
			ResetAfterSeconds: shortResetSeconds, Feature: f.feature,
		})
	}
	return windows
}

// The regression, with the live numbers. "rolling" holds more of its week and
// rolls tonight; "steady" holds less and has six days. Under the model pool the
// 5h signal favours steady, so only a surviving weekly signal picks rolling.
func TestModelPoolStillSeesTheWeeklyRoll(t *testing.T) {
	// acct-p: weekly 71.7% left, rolls in 13.6h, 5h untouched, resets in 2.4h
	rolling := claudeScore("rolling", modelWindowsWithoutWeeklyReset(28.3, 0, 48973, 8772))
	// claw-2: weekly 47.5% left, rolls in 154.6h, 5h 27% used, resets in 1.4h
	steady := claudeScore("steady", modelWindowsWithoutWeeklyReset(52.5, 27, 556560, 5173))

	scheduler := NewScheduler([]Score{steady, rolling}).ForModel("claude-opus")

	if got := pickID(t, scheduler, "steady", "rolling"); got != "rolling" {
		t.Fatalf("model-pool pick = %q, want \"rolling\": the account rolling in 13.6h with 72%% "+
			"of its week left must outrank one with 47%% and 155h to go. Getting %q means the "+
			"weekly signal did not survive model scoping and the 5h window decided alone.", got, got)
	}
}

// The mechanism, pinned directly: scoping must not turn a paced account into an
// unpaced one. Asserting the pick alone would let this pass again for some
// unrelated ordering reason.
func TestModelScopingKeepsWeeklyPacing(t *testing.T) {
	score := claudeScore("acct", modelWindowsWithoutWeeklyReset(28.3, 0, 48973, 8772))
	scheduler := NewScheduler([]Score{score}).ForModel("claude-opus")

	scoped, ok := scheduler.LookupScore(account.ProviderClaude, "acct")
	if !ok {
		t.Fatal("scoped score missing entirely")
	}
	if scoped.WeeklyResetAt.IsZero() {
		t.Fatal("model-scoped score has a ZERO WeeklyResetAt: weeklyPace and inEndgameWindow " +
			"are both dead for this account, and the comparator will fall through to the 5h window")
	}
	if scoped.WeeklyHeadroom <= 0 {
		t.Fatalf("model-scoped WeeklyHeadroom = %v, want the account's weekly budget carried through",
			scoped.WeeklyHeadroom)
	}
}

// Inheriting account facts must NOT resurrect an account the model pool has
// exhausted -- Headroom/ShortHeadroom stay model-scoped and exhausted() reads
// those. Without this, "carry the weekly window through" could route traffic to
// an account that 429s on the requested model.
func TestModelScopingStillExcludesModelExhausted(t *testing.T) {
	healthy := claudeScore("healthy", modelWindowsWithoutWeeklyReset(52.5, 27, 556560, 5173))

	spent := claudeScore("spent", modelWindowsWithoutWeeklyReset(28.3, 0, 48973, 8772))
	// Exhaust ONLY the model pool, leaving the account's weekly budget rich --
	// the precise combination the inheritance could otherwise paper over.
	opus := spent.ModelScores[ModelKey("claude-opus")]
	opus.Headroom = 0
	opus.ShortHeadroom = 0
	spent.ModelScores[ModelKey("claude-opus")] = opus

	scheduler := NewScheduler([]Score{spent, healthy}).ForModel("claude-opus")
	if got := pickID(t, scheduler, "spent", "healthy"); got != "healthy" {
		t.Fatalf("pick = %q, want \"healthy\": a model-exhausted account must stay excluded "+
			"however rich its account-wide week looks", got)
	}
}

// Remote is an account-level fact. A model sub-score that drops it silently
// relocates a peer into the local pool and hands it the local preference.
func TestModelScopingKeepsRemoteFlag(t *testing.T) {
	score := claudeScore("peer", modelWindowsWithoutWeeklyReset(28.3, 0, 48973, 8772))
	score.Remote = true
	scheduler := NewScheduler([]Score{score}).ForModel("claude-opus")

	scoped, ok := scheduler.LookupScore(account.ProviderClaude, "peer")
	if !ok {
		t.Fatal("scoped score missing")
	}
	if !scoped.Remote {
		t.Fatal("model scoping cleared Remote: a peer account now ranks as local and collects " +
			"the LocalPreferenceRatio advantage it must not have")
	}
}
