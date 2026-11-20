package accounts

import (
	"testing"
	"time"
)

func TestWeeklyCookedWindow(t *testing.T) {
	const week = 604800
	const fiveHours = 18000
	cases := []struct {
		name    string
		windows []UsageWindow
		want    bool
	}{
		{"weekly primary full", []UsageWindow{{Name: "primary", UsedPercent: 100, LimitWindowSeconds: week}}, true},
		{"weekly secondary full", []UsageWindow{
			{Name: "primary", UsedPercent: 10, LimitWindowSeconds: fiveHours},
			{Name: "secondary", UsedPercent: 100, LimitWindowSeconds: week},
		}, true},
		{"legacy secondary without length", []UsageWindow{{Name: "secondary", UsedPercent: 100}}, true},
		{"claude 7d by name", []UsageWindow{{Name: "7d", UsedPercent: 100}}, true},
		{"model-scoped weekly full is ignored", []UsageWindow{
			{Name: "primary", UsedPercent: 20, LimitWindowSeconds: week},
			{Name: "spark/primary", Feature: "spark", UsedPercent: 100, LimitWindowSeconds: week},
		}, false},
		{"reached with no weekly window reported", []UsageWindow{{Name: "reached", UsedPercent: 100}}, true},
		{"reached from the 5h window while weekly has room", []UsageWindow{
			{Name: "primary", UsedPercent: 100, LimitWindowSeconds: fiveHours},
			{Name: "secondary", UsedPercent: 40, LimitWindowSeconds: week},
			{Name: "reached", UsedPercent: 100},
		}, false},
		{"reached explained by a full 5h window, no weekly reported", []UsageWindow{
			{Name: "primary", UsedPercent: 100, LimitWindowSeconds: fiveHours},
			{Name: "reached", UsedPercent: 100},
		}, false},
		{"only 5h full", []UsageWindow{{Name: "primary", UsedPercent: 100, LimitWindowSeconds: fiveHours}}, false},
		// A usage-based seat (PATCHES.md §42): its spend cap is its only quota.
		{"spend window full", []UsageWindow{{Name: CodexSpendWindowName, UsedPercent: 100, LimitWindowSeconds: 30 * 24 * 3600}}, true},
		{"spend window with room", []UsageWindow{{Name: CodexSpendWindowName, UsedPercent: 60, LimitWindowSeconds: 30 * 24 * 3600}}, false},
		{"spend/reached alone (no readable amounts)", []UsageWindow{{Name: CodexSpendReachedWindowName, UsedPercent: 100}}, true},
		{"spend/reached below a full percentage", []UsageWindow{
			{Name: CodexSpendWindowName, UsedPercent: 95, LimitWindowSeconds: 30 * 24 * 3600},
			{Name: CodexSpendReachedWindowName, UsedPercent: 100},
		}, true},
		{"weekly reset has passed", []UsageWindow{{Name: "secondary", UsedPercent: 100, LimitWindowSeconds: week, ResetAt: time.Now().Add(-time.Minute)}}, false},
		// Codex sends limit_reached with the full weekly window, so a stale
		// reading carries the synthetic "reached" too; it has no reset of
		// its own and must not bring the cook back once the weekly reset
		// has passed.
		{"weekly reset has passed, reached from the same reading", []UsageWindow{
			{Name: "secondary", UsedPercent: 100, LimitWindowSeconds: week, ResetAt: time.Now().Add(-time.Minute)},
			{Name: "reached", UsedPercent: 100},
		}, false},
		{"5h reset has passed, reached from the same reading", []UsageWindow{
			{Name: "primary", UsedPercent: 100, LimitWindowSeconds: fiveHours, ResetAt: time.Now().Add(-time.Minute)},
			{Name: "reached", UsedPercent: 100},
		}, false},
		{"none", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := WeeklyCookedWindow(tc.windows); got != tc.want {
				t.Fatalf("WeeklyCookedWindow = %v, want %v", got, tc.want)
			}
		})
	}
}

// Upstream limit_reached with no weekly window reported stands in as the
// cooked window but carries no reset. The wait is unknown (0): a short
// window's reset must not pose as the weekly wait.
func TestWeeklyResetWaitIsUnknownWithoutAWeeklyReset(t *testing.T) {
	windows := []UsageWindow{
		{Name: "primary", UsedPercent: 60, LimitWindowSeconds: 5 * 3600, ResetAfterSeconds: 9000},
		{Name: "reached", UsedPercent: 100},
	}
	if got := WeeklyResetWait(windows); got != 0 {
		t.Fatalf("WeeklyResetWait = %d, want 0 (unknown), not the 5h reset", got)
	}
	full := []UsageWindow{{Name: "primary", UsedPercent: 100, LimitWindowSeconds: 7 * 24 * 3600, ResetAfterSeconds: 400000}}
	if got := WeeklyResetWait(full); got != 400000 {
		t.Fatalf("WeeklyResetWait = %d, want the cooked window's own reset", got)
	}
	if got := WeeklyResetWait([]UsageWindow{{Name: "primary", UsedPercent: 20, LimitWindowSeconds: 7 * 24 * 3600, ResetAfterSeconds: 400000}}); got != 0 {
		t.Fatalf("not cooked: WeeklyResetWait = %d, want 0", got)
	}
}
