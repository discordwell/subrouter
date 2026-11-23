package proxy

import (
	"log/slog"
	"math"
	"sync"
	"time"

	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

// fableMeterWarnEvery bounds the disagreement log to one line per account an
// hour: the usage sweep reads every account each few minutes.
const fableMeterWarnEvery = time.Hour

// fableMeterWarner logs a usage answer whose limits[] Fable row and
// seven_day_oauth_apps differ by more than a point. The Fable window is built
// from the row; the log is how a deployed daemon tells us whether the two
// were ever the same bucket.
type fableMeterWarner struct {
	// now nil is time.Now; logger nil is slog.Default(), which serve
	// configures. Tests set both.
	now    func() time.Time
	logger *slog.Logger

	mu     sync.Mutex
	logged map[string]time.Time
}

var fableMeterDisagreement = &fableMeterWarner{}

func (w *fableMeterWarner) check(accountID string, usage *agentclaude.UsageResponse) {
	row := usage.FableLimitRow()
	if row == nil || usage.SevenDayOAuthApps == nil || usage.SevenDayOAuthApps.Utilization == nil {
		return
	}
	apps := usage.SevenDayOAuthApps
	if math.Abs(*row.Utilization-*apps.Utilization) <= 1 {
		return
	}
	now := time.Now()
	if w.now != nil {
		now = w.now()
	}
	w.mu.Lock()
	if last, ok := w.logged[accountID]; ok && now.Sub(last) < fableMeterWarnEvery {
		w.mu.Unlock()
		return
	}
	if w.logged == nil {
		w.logged = map[string]time.Time{}
	}
	w.logged[accountID] = now
	w.mu.Unlock()
	logger := w.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("claude fable meter: the limits[] Fable row and seven_day_oauth_apps disagree; the Fable window uses the row",
		"account", accountID,
		"limits_row_percent", *row.Utilization, "limits_row_resets_at", row.ResetsAt,
		"seven_day_oauth_apps_percent", *apps.Utilization, "seven_day_oauth_apps_resets_at", apps.ResetsAt)
}
