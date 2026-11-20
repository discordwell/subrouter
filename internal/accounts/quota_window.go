package accounts

import (
	"strings"
	"time"
)

// longQuotaWindowMinSeconds is the shortest window length treated as an
// account-wide weekly limit.
const longQuotaWindowMinSeconds = 6 * 24 * 60 * 60

// IsModelScopedWindow reports a window that belongs to an additional
// per-model limit rather than the account-wide quota.
func IsModelScopedWindow(window UsageWindow) bool {
	return strings.TrimSpace(window.Feature) != ""
}

// IsSpendWindow reports a window that carries a usage-based Codex seat's
// workspace spend cap (spend_control) rather than a rate limit. A rate-limit
// reset credit resets rate limits, so it never lifts one of these.
func IsSpendWindow(window UsageWindow) bool {
	return window.Name == CodexSpendWindowName || window.Name == CodexSpendReachedWindowName
}

// IsLongQuotaWindow reports whether a window is the weekly quota. A reported
// length decides; without one, the name does. Codex once reported the weekly
// window only as "secondary" and now sometimes only as "primary" with a
// length, so the slot name alone is never trusted when a length is present.
func IsLongQuotaWindow(window UsageWindow) bool {
	if window.LimitWindowSeconds > 0 {
		return window.LimitWindowSeconds >= longQuotaWindowMinSeconds
	}
	name := strings.ToLower(window.Name)
	return name == "secondary" || strings.Contains(name, "7d") || strings.Contains(name, "weekly")
}

// WeeklyCookedWindow returns the first account-wide weekly window that is
// fully consumed. When no account-wide weekly window is reported at all, the
// synthetic "reached" window (upstream limit_reached) stands in for it,
// unless a full shorter window already explains the limit.
// A usage-based seat's "spend/reached" is cooked on its own: it is the
// upstream's hard stop on the seat's only quota, which no shorter window
// explains and no reset credit lifts (PATCHES.md §42).
// This is the single definition of "cooked" shared by sr status, sr switch,
// sr reset, and the server's reset handler.
func WeeklyCookedWindow(windows []UsageWindow) (UsageWindow, bool) {
	var reached *UsageWindow
	sawLong := false
	shortFull := false
	for i, window := range windows {
		if IsModelScopedWindow(window) {
			continue
		}
		// Cached status can outlive the provider's quota window when the
		// usage endpoint is throttled. An absolute reset is authoritative:
		// once it has passed, the old 100% reading no longer means cooked.
		// The window was still reported, though: the same reading's
		// synthetic "reached" (limit_reached, no reset of its own) must not
		// stand in for a weekly window whose reset passed, nor for a full
		// short window that explained it, or the stale cook the skip exists
		// to clear came back through "reached".
		if !window.ResetAt.IsZero() && !window.ResetAt.After(time.Now()) {
			if IsLongQuotaWindow(window) {
				sawLong = true
			} else if window.LimitWindowSeconds > 0 && window.UsedPercent >= 100 {
				shortFull = true
			}
			continue
		}
		if window.Name == CodexSpendReachedWindowName {
			return window, true
		}
		if window.Name == "reached" {
			reached = &windows[i]
			continue
		}
		if !IsLongQuotaWindow(window) {
			if window.LimitWindowSeconds > 0 && window.UsedPercent >= 100 {
				shortFull = true
			}
			continue
		}
		sawLong = true
		if window.UsedPercent >= 100 {
			return window, true
		}
	}
	if reached != nil && !sawLong && !shortFull {
		return *reached, true
	}
	return UsageWindow{}, false
}

// WeeklyResetWait is how long a weekly-cooked account waits for its quota to
// come back on its own: the cooked window's reset. It is 0 when unknown or
// not cooked. The synthetic "reached" window (upstream limit_reached with no
// weekly window reported) carries no reset, and no other window can stand in
// for it: only short windows exist then, and their reset says nothing about
// the weekly wait.
func WeeklyResetWait(windows []UsageWindow) int64 {
	window, cooked := WeeklyCookedWindow(windows)
	if !cooked {
		return 0
	}
	return window.ResetAfterSeconds
}
