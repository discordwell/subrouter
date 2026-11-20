package selectacct

import (
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

// Rate-limit marks (fork only, PATCHES section 35.3).
//
// A Codex 429 that names no usage limit is a burst: a request rate refused
// for seconds, not a spent quota. Owner rule P8 keeps a session on its
// account through one, so a mark for it must never move a session. That
// rules out both marks capacity.go and the exhaustion overlays already have:
// an exhaustion mark makes the account Evicted, which moves every warm
// session on it at once, and a capacity mark releases them at its second
// failure in a row (CapacityStickyEvictFailures) -- which a burst, many
// requests refused together, reaches at once. What is left to do is the
// placement: a new session, or a failover's replacement, is better off on an
// account that is not refusing requests right now.
//
// A rate-limit mark does exactly that. RankMarks folds it into a pool's
// capacity counts as ONE for an account with no capacity mark of its own, so
// the account ranks after unmarked ones of its selection tier, as after a
// single capacity failure; it never adds to a capacity count, so it never
// makes an account CapacityEvicting, however many 429s arrive. It is per
// account (a request rate is not one model's), lasts until its own short
// expiry (a success does not end it: a burst's refusals and successes
// interleave), and, like the capacity marks, is never persisted.

// MarkRateLimitedUntil marks the account rate-limited until the given time,
// or extends a live mark to it. A time already past records nothing.
func (r *SchedulerRef) MarkRateLimitedUntil(provider account.Provider, accountID string, until time.Time) {
	now := time.Now()
	if r == nil || accountID == "" || !until.After(now) {
		return
	}
	key := ScoreKey(provider, accountID)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rateLimitedUntil == nil {
		r.rateLimitedUntil = make(map[string]time.Time)
	}
	for k, expiry := range r.rateLimitedUntil {
		if !expiry.After(now) {
			delete(r.rateLimitedUntil, k)
		}
	}
	if until.After(r.rateLimitedUntil[key]) {
		r.rateLimitedUntil[key] = until
	}
}

// RateLimitedUntilFor reports a live rate-limit mark's expiry.
func (r *SchedulerRef) RateLimitedUntilFor(provider account.Provider, accountID string) (time.Time, bool) {
	if r == nil {
		return time.Time{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	until, ok := r.rateLimitedUntil[ScoreKey(provider, accountID)]
	if !ok || !until.After(time.Now()) {
		return time.Time{}, false
	}
	return until, true
}

// RankMarks is CapacityMarks for one (model, tier) pool with the provider's
// live rate-limit marks folded in: one for a rate-limited account with no
// capacity mark, and a capacity count left exactly as it is. Pass the result
// to Scheduler.WithCapacityMarks.
func (r *SchedulerRef) RankMarks(provider account.Provider, model, tier string) map[string]int {
	out := r.CapacityMarks(provider, model, tier)
	if r == nil {
		return out
	}
	now := time.Now()
	prefix := ScoreKey(provider, "")
	r.mu.RLock()
	defer r.mu.RUnlock()
	for key, until := range r.rateLimitedUntil {
		if !until.After(now) || !strings.HasPrefix(key, prefix) || out[key] > 0 {
			continue
		}
		if out == nil {
			out = make(map[string]int)
		}
		out[key] = 1
	}
	return out
}

// HasRankMarks reports whether any capacity or rate-limit mark is live, so
// callers can skip attaching marks when none is.
func (r *SchedulerRef) HasRankMarks() bool {
	if r == nil {
		return false
	}
	if r.HasCapacityMarks() {
		return true
	}
	now := time.Now()
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, until := range r.rateLimitedUntil {
		if until.After(now) {
			return true
		}
	}
	return false
}
