package proxy

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// PATCHES §92.1: a score built from a cached usage reading rolls its week at
// the reading's own reset time. The daemon scores every 30 s from a window
// cache that lives 2 minutes, and a cached window's seconds to reset count
// from when it was read. Read against the clock of the score instead, the
// roll moved by the cache's age, and past a minute §72's tracker took that for
// a new week and threw away the account's measured burn.
func TestACachedReadingKeepsTheRollTimeAndTheMeasuredBurn(t *testing.T) {
	server, _ := resetDaemon(t, &anthropicFake{t: t})
	ctx := context.Background()
	ref := server.AccountRef
	var row AccountUsageStatus
	for _, status := range ref.UsageStatuses(ctx) {
		if status.Provider == accounts.ProviderClaude {
			row = status
		}
	}
	if row.ID == "" {
		t.Fatal("the daemon listed no Claude account")
	}
	provider := schedulerAccountProvider(accounts.ProviderClaude)
	roll := time.Now().Add(3 * 24 * time.Hour).Truncate(time.Second)

	// A fresh reading 40 minutes ago: 70 % of the week used, rolling at roll.
	server.SchedulerRef.Set(selectacct.NewScheduler([]selectacct.Score{{
		AccountID: row.ID, Provider: provider, Headroom: 0.3, Fresh: true,
		MeasuredAt: time.Now().Add(-40 * time.Minute), WeeklyRemaining: 0.3, WeeklyHeadroom: 0.3, WeeklyResetAt: roll,
	}}))
	// The window cache holds a reading taken 90 seconds ago: 78 % used, the
	// same roll, its seconds to reset counted from then.
	read := time.Now().Add(-90 * time.Second)
	ref.usageWindowsMu.Lock()
	ref.usageWindows[row.ID+"\x00"+string(row.Provider)] = usageWindowsEntry{
		windows: []accounts.UsageWindow{{
			Name: "7d", UsedPercent: 78, LimitWindowSeconds: 7 * 24 * 3600,
			ResetAt: roll, ResetAfterSeconds: int64(roll.Sub(read) / time.Second),
		}},
		at: read, supplementalFresh: true,
	}
	ref.usageWindowsMu.Unlock()
	ref.InvalidateUsageStatusCache()
	server.updateSchedulerFromUsageStatusesContext(ctx, ref.UsageStatuses(ctx))

	scheduler := server.SchedulerRef.Get()
	score := scheduler.ScoreFor(provider, row.ID)
	if !score.Fresh || !score.MeasuredAt.Equal(read) {
		t.Fatalf("the score did not come from the cached reading: fresh %v measured %v, want %v", score.Fresh, score.MeasuredAt, read)
	}
	want := 0.08 / read.Sub(time.Now().Add(-40*time.Minute)).Hours()
	if got := scheduler.PaceAllowance(provider, row.ID, time.Now()).Pace.BurnNow; math.Abs(got-want) > 0.01 {
		t.Errorf("§72's measured burn after the cached reading = %.4f a week per hour, want %.4f", got, want)
	}
	if drift := score.WeeklyResetAt.Sub(roll); drift > 2*time.Second || drift < -2*time.Second {
		t.Errorf("the cached reading rolls the week at %v, %v from the reading's own %v", score.WeeklyResetAt, drift, roll)
	}
}
