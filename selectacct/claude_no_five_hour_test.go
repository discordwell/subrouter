package selectacct

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

// claudeWindowsWithoutFiveHour is a Claude Max window set as the proxy builds
// it when Anthropic's usage endpoint returns no `five_hour` bucket: the 7d
// window, the Fable meter, and the 7d window duplicated into the Fable pool
// (scoreFromUsageWindows).
func claudeWindowsWithoutFiveHour(weekUsedPct, fableUsedPct float64, weekResetSeconds int64) []LimitWindow {
	const sevenDay = int64(7 * 24 * 60 * 60)
	return []LimitWindow{
		{Name: "7d", UsedPercent: weekUsedPct, LimitWindowSeconds: sevenDay, ResetAfterSeconds: weekResetSeconds},
		{Name: "oauth-apps-weekly", UsedPercent: fableUsedPct, LimitWindowSeconds: sevenDay, ResetAfterSeconds: weekResetSeconds, Feature: "claude-fable"},
		{Name: "7d", UsedPercent: weekUsedPct, LimitWindowSeconds: sevenDay, ResetAfterSeconds: weekResetSeconds, Feature: "claude-fable"},
	}
}

// The box's pixel-gamma on 2026-11-03: 96% of its week used, Fable
// spent, no 5h reading, rolling in 1h38m. Its last 4% expires at the roll, so
// it is the account the drain exists for, and nothing about a 5h window says
// otherwise: there is no 5h reading to say anything. That holds down to the
// meter's last percent, so no floor read off the week, however low, may stand
// in for the 5h window. Days from its roll the same 4% still takes a new
// session, ranked on its weekly pace.
func TestClaudeWithNoFiveHourReadingSpendsTheLastOfItsWeek(t *testing.T) {
	now := time.Now()
	healthy := ScoreFromLimitWindowsAt("healthy", 0, claudePoolWindows(30, 0, 10, fourDays, twoHours), now)
	healthy.Provider = account.ProviderClaude
	for _, used := range []float64{96, 99} {
		pixel := ScoreFromLimitWindowsAt("pixel", 0, claudeWindowsWithoutFiveHour(used, 100, 98*60), now)
		pixel.Provider = account.ProviderClaude
		scheduler := NewScheduler([]Score{pixel, healthy})

		verdict := scheduler.NewSessionVerdict(account.ProviderClaude, "pixel", now)
		if !verdict.Eligible || !verdict.Burndown || verdict.BurndownCause != BurndownCauseRoll {
			t.Fatalf("%v%% used: verdict = %+v, want eligible, burning down on the roll", used, verdict)
		}
		if got := pickID(t, scheduler, "healthy", "pixel"); got != "pixel" {
			t.Fatalf("%v%% used: picked %q, want pixel: what it holds expires in 98 minutes", used, got)
		}
	}

	early := ScoreFromLimitWindowsAt("early", 0, claudeWindowsWithoutFiveHour(96, 100, 3*24*60*60), now)
	early.Provider = account.ProviderClaude
	if !early.UsableForNewSession() {
		t.Fatalf("a Claude account with 4%% of its week left and no 5h reading refuses new sessions: %+v", early)
	}
}

// Requests this daemon routes between usage readings debit an account's
// headroom so load spreads before the next reading lands. With no 5h reading
// there is no 5h window to debit: 50 requests on an account with most of its
// week left must not read as its 5h window run dry.
func TestClaudeWithNoFiveHourReadingTakesNewSessionsThroughLiveDebits(t *testing.T) {
	now := time.Now()
	busy := ScoreFromLimitWindowsAt("busy", 0, claudeWindowsWithoutFiveHour(5, 0, 3*24*60*60), now)
	busy.Provider = account.ProviderClaude
	scheduler := NewScheduler([]Score{busy}).
		WithLiveDebits(map[string]int{ScoreKey(account.ProviderClaude, "busy"): 50})

	if verdict := scheduler.NewSessionVerdict(account.ProviderClaude, "busy", now); !verdict.Eligible {
		t.Fatalf("after 50 routed requests: verdict = %+v, want eligible", verdict)
	}
}
