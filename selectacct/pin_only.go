package selectacct

import "github.com/discordwell/subrouter/account"

// Pin-only accounts (PATCHES §98, SUBROUTER_PIN_ONLY_ACCOUNTS).
//
// 2026-11-23 owner: an account listed here takes pinned sessions only (the meter test).
//
// A pin-only account is this daemon's own, and it is never a place for the
// pool's work: it is not routable for a new session, ranks with the evicted
// and closed accounts when nothing else is left (selectionTier), takes no
// refugee, and its published verdict says pin_only, so a peer places nothing
// on it either. A reset choice cannot load it: routableForNewSession refuses
// it before loadEligible is read. A pin by name never asks the
// scheduler; the proxy serves it. A peer's score never carries the mark: its
// holder publishes the verdict.

// WithPinOnlyAccounts attaches the IDs of this daemon's own accounts that take
// pinned sessions only. Keyed by bare account ID. The map must not be mutated
// afterwards; nil or empty marks none.
func (s Scheduler) WithPinOnlyAccounts(ids map[string]bool) Scheduler {
	next := s
	next.pinOnly = ids
	return next
}

// PinOnly reports whether the account takes pinned sessions only.
func (s Scheduler) PinOnly(provider account.Provider, accountID string) bool {
	return s.score(provider, accountID).pinOnly
}
