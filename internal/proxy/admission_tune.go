package proxy

import (
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// Self-tuning (PATCHES §70). Anthropic publishes no concurrency limit; past
// one it answers a 429 with no quota headers ("Server is temporarily
// limiting requests"). So a Claude bucket's limit climbs while requests wait
// at it and no such 429 comes, and falls by a quarter when one does. The
// pause and the 1, 2, 4 ramp after it already absorb the 429 a too-high
// limit costs, so a climb is cheap to undo.

// admissionTune bounds how far tuning moves a Claude bucket's limit.
type admissionTune struct {
	On  bool `json:"on"`
	Min int  `json:"min"`
	Max int  `json:"max"`
}

const (
	// admissionTuneStep is how often a bucket may rise by one.
	admissionTuneStep = 30 * time.Second
	// admissionTuneQuiet is how long after a transient 429 a bucket does
	// not rise.
	admissionTuneQuiet = 2 * time.Minute
	// admissionTuneHold is the least time between two backoffs, so one
	// burst of 429s from requests already in flight lowers the limit once.
	admissionTuneHold = 10 * time.Second
)

// tunes reports whether key's bucket tunes: a Claude bucket, tuning on.
func (l admissionLimits) tunes(key admissionKey) bool {
	return l.Tune.On && key.provider == accounts.ProviderClaude
}

// tuneBounds is how far key's bucket may move: the tuning band, widened to
// take in its configured limit, so a start above max is its ceiling.
func (l admissionLimits) tuneBounds(key admissionKey) (low, high int) {
	start := l.limitFor(key)
	return min(l.Tune.Min, start), max(l.Tune.Max, start)
}

// retuned is key's limit once l replaces previous, for a bucket now at
// current: its new configured limit where that changed or tuning is off,
// otherwise current inside the new bounds.
func (l admissionLimits) retuned(previous admissionLimits, key admissionKey, current int) int {
	start := l.limitFor(key)
	if !l.tunes(key) || start != previous.limitFor(key) {
		return start
	}
	low, high := l.tuneBounds(key)
	return min(max(current, low), high)
}

type admissionLimitChange struct {
	key      admissionKey
	from, to int
}

func (c admissionLimitChange) attrs() []any {
	return []any{"provider", c.key.provider, "account", c.key.account, "model", c.key.model, "from", c.from, "to", c.to}
}

// tune runs one step at now: each tuning bucket that had a request waiting at
// its full limit rises by one, unless a transient 429 came within
// admissionTuneQuiet. A queue behind a pause or its recovery ramp is the
// ramp's, not the limit's.
func (a *admissionControl) tune(now time.Time) {
	if a == nil {
		return
	}
	var raised []admissionLimitChange
	a.mu.Lock()
	for key, bucket := range a.buckets {
		bound := bucket.bound
		bucket.bound = false
		if !a.limits.tunes(key) || now.Sub(bucket.throttledAt) < admissionTuneQuiet {
			continue
		}
		effective, _ := a.admissionLimitLocked(bucket, now)
		if effective < bucket.limit {
			continue
		}
		if !bound && (len(bucket.waiters) == 0 || bucket.active < effective) {
			continue
		}
		if _, high := a.limits.tuneBounds(key); bucket.limit >= high {
			continue
		}
		raised = append(raised, admissionLimitChange{key: key, from: bucket.limit, to: bucket.limit + 1})
		bucket.limit++
		a.wakeLocked(key, bucket, now)
	}
	a.mu.Unlock()
	for _, change := range raised {
		a.log().Info("admission limit raised", change.attrs()...)
	}
}

// backoff lowers key's limit by a quarter, at least one, after a transient
// 429 at now: at most once per admissionTuneHold, never under its floor.
func (a *admissionControl) backoff(key admissionKey, now time.Time) {
	a.mu.Lock()
	if !a.limits.tunes(key) {
		a.mu.Unlock()
		return
	}
	bucket := a.bucketLocked(key)
	if now.After(bucket.throttledAt) {
		bucket.throttledAt = now
	}
	low, _ := a.limits.tuneBounds(key)
	if now.Sub(bucket.backedOffAt) < admissionTuneHold || bucket.limit <= low {
		a.mu.Unlock()
		return
	}
	change := admissionLimitChange{key: key, from: bucket.limit, to: max(low, bucket.limit-max(1, bucket.limit/4))}
	bucket.limit, bucket.backedOffAt = change.to, now
	a.mu.Unlock()
	a.log().Info("admission limit lowered", change.attrs()...)
}
