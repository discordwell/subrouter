package selectacct

import "testing"

// The short window is a burn rate, not a level. 20% left means "something is
// ripping through this account, do not add to it" four hours before the reset,
// and "harmless, it refills shortly" four minutes before -- and a flat
// threshold answers both the same way.
func TestShortWindowGateIsARate(t *testing.T) {
	const fiveHours = int64(5 * 60 * 60)
	for _, tc := range []struct {
		name        string
		shortLeft   float64
		resetInSecs int64
		wantUsable  bool
	}{
		// A monster session ripping through the window with most of it still
		// to run. Refused however good the weekly numbers are.
		{"80% spent, 4h still to run", 0.20, 4 * 60 * 60, false},
		// Same 20% left, but the window is about to refill.
		{"80% spent, 4m to reset", 0.20, 4 * 60, true},
		// A fresh window is always fine.
		{"untouched window", 1.00, fiveHours, true},
		// Comfortably ahead of linear burn halfway through.
		{"70% left at the halfway mark", 0.70, 150 * 60, true},
		// Behind linear burn early on: half the quota gone in the first hour.
		{"50% spent in the first hour", 0.50, 4 * 60 * 60, false},
		// The absolute backstop: a near reset does not justify handing a new
		// session to an account with nothing left.
		{"almost nothing left, reset imminent", 0.01, 30, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			score := Score{
				Headroom:               1,
				ShortHeadroom:          tc.shortLeft,
				ShortResetAfterSeconds: tc.resetInSecs,
				ShortWindowSeconds:     fiveHours,
			}
			if got := score.usableForNewSession(); got != tc.wantUsable {
				t.Fatalf("usableForNewSession = %v, want %v (%.0f%% left, %ds to reset)",
					got, tc.wantUsable, tc.shortLeft*100, tc.resetInSecs)
			}
		})
	}

	// A weekly window at 5% must not block a session when the 5h window is
	// fresh: weekly decides preference, not permission.
	spentWeekButFreshWindow := Score{
		Headroom:               0.05,
		ShortHeadroom:          1.0,
		ShortResetAfterSeconds: fiveHours,
		ShortWindowSeconds:     fiveHours,
	}
	if !spentWeekButFreshWindow.usableForNewSession() {
		t.Fatal("a fresh 5h window must be usable regardless of how little of the week is left")
	}
}
