package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

const (
	// Retry-After bounds for a full queue. The queue drains in seconds, so a
	// client told to wait longer idles needlessly, and one told 0 burns its
	// retry attempts against the same full queue.
	minAdmissionRetryAfter = 1
	maxAdmissionRetryAfter = 5
)

var errAdmissionQueueFull = errors.New("upstream admission queue is full")

// errAdmissionPaused rejects a new attempt on an account whose bucket is
// paused for longer than admissionPauseWaitMax. A Claude 429 pauses the
// bucket until the quota reset, which can be hours away: queueing behind that
// held requests until the client gave up and then answered 503 for every
// request once the queue filled (2026-10-14: 1,393 rejections in 19 minutes
// on one account while other accounts had room). Rejecting fast lets the
// caller fail over to another account instead.
var errAdmissionPaused = errors.New("upstream account is paused after a rate limit")

// admissionPauseWaitMax is the longest pause a new attempt still waits out in
// the queue. A bare 429 without a reset pauses for a second; that is cheaper
// to wait than to move a session and lose its prompt cache.
const admissionPauseWaitMax = 5 * time.Second

// admissionQuotaPauseMax bounds the pause a Claude quota rejection sets.
// Upstream #589 paused the bucket until the quota reset the 429 named (days,
// for a 7d rejection), and only time ended it. In this fork the quota hold is
// the scheduler's exhaustion mark, set from the same 429: a reading taken
// after the 429 ends it early (PATCHES §29.3b) and a reset claim clears it
// (§46.4), and neither reached this pause, so a reset account was refused
// locally on every attempt until the old reset. The pause stays long enough
// to turn the burst away (past admissionPauseWaitMax, queued and new attempts
// fail over at once); holding the account out longer is the scheduler's job.
// A transient throttle's Retry-After is still honoured as given (#645): no
// scheduler mark stands behind it.
const admissionQuotaPauseMax = time.Minute

// A Claude 429 without quota evidence or a Retry-After hint is a short burst
// throttle, not a reason to apply the ten-minute quota fallback.
const claudeTransientThrottleFallback = time.Second

// Reopen a transiently throttled bucket gradually, rather than sending the
// entire queued burst as soon as its cooldown expires: 1, 2, 4, then the
// normal limit, one second apart. Time, rather than a successful response,
// bounds recovery so a cancelled request or a long stream cannot wedge it.
const admissionRecoveryStep = time.Second

// admissionRejected reports a limiter rejection that another account could
// serve: the queue is full or the account is paused.
func admissionRejected(err error) bool {
	return errors.Is(err, errAdmissionQueueFull) || errors.Is(err, errAdmissionPaused)
}

// subrouterReasonHeader names why subrouter itself answered a request.
const subrouterReasonHeader = "X-Subrouter-Reason"

// admissionReasonQueueFull is the X-Subrouter-Reason for a full admission
// queue, so a client or operator can tell it from an upstream 503.
const admissionReasonQueueFull = "admission_queue_full"

// admissionQueueFullError is errAdmissionQueueFull with the wait the queue
// implies, so the 503 can carry a Retry-After.
type admissionQueueFullError struct {
	retryAfterSeconds int
}

func (e admissionQueueFullError) Error() string { return errAdmissionQueueFull.Error() }

func (e admissionQueueFullError) Is(target error) bool { return target == errAdmissionQueueFull }

// admissionRetryAfterSeconds is the Retry-After for err, or 0 when err is not
// a full admission queue.
func admissionRetryAfterSeconds(err error) int {
	var full admissionQueueFullError
	if errors.As(err, &full) {
		return full.retryAfterSeconds
	}
	if errors.Is(err, errAdmissionQueueFull) {
		return minAdmissionRetryAfter
	}
	return 0
}

// admissionReasonPaused is the X-Subrouter-Reason for an account paused by a
// rate limit when no other account could take the request.
const admissionReasonPaused = "admission_paused"

// writeAdmissionRejected answers a request the admission gate turned away
// after failover found no other account: a full queue, or a long pause.
func writeAdmissionRejected(w http.ResponseWriter, err error) {
	if errors.Is(err, errAdmissionPaused) {
		w.Header().Set("Retry-After", strconv.Itoa(minAdmissionRetryAfter))
		w.Header().Set(subrouterReasonHeader, admissionReasonPaused)
		http.Error(w, errAdmissionPaused.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Retry-After", strconv.Itoa(admissionRetryAfterSeconds(err)))
	w.Header().Set(subrouterReasonHeader, admissionReasonQueueFull)
	http.Error(w, errAdmissionQueueFull.Error(), http.StatusServiceUnavailable)
}

type admissionKey struct {
	provider accounts.Provider
	account  string
	model    string
}

type admissionWaiter struct {
	ready    chan struct{}
	queuedAt time.Time
	// rejected is set under the control mutex before ready is closed when a
	// long pause turns the waiter away instead of granting it a slot.
	rejected bool
	who      admissionSessionKey
}

// admissionSessionKey names the client session a physical attempt belongs
// to. A session fanning out (subagents, workflows) shares one id, so its
// attempts are the ones that pile onto one sticky account.
type admissionSessionKey struct {
	agent   string
	session string
}

// admissionSession is one session's share of the admission gates.
type admissionSession struct {
	active  int
	queued  int
	peak    int
	served  uint64
	blocked uint64
	spilled uint64
	// minutePeak is peak since the last minute line.
	minutePeak int
	account    string
	lastSeen   time.Time
	// overflowFrom and overflowStreak count consecutive queue-full overflows
	// off one sticky account; an admitted attempt there resets them.
	overflowFrom   string
	overflowStreak int
}

// admissionOverflowMoveAfter is how many queue-full overflows in a row move a
// session off its sticky account. One overflow keeps the session, and its
// prompt cache, where it is; a sticky account that stays full would instead
// make every turn re-write the cache on a random account.
const admissionOverflowMoveAfter = 3

const (
	// admissionSessionIdle is how long an idle session's counters are kept,
	// so a fan-out that just finished is still visible in the snapshot.
	admissionSessionIdle = 15 * time.Minute
	// admissionSessionMax bounds the per-session table. Past it, idle
	// entries are dropped first.
	admissionSessionMax = 1024
	// admissionSessionTop is how many sessions the snapshot lists.
	admissionSessionTop = 20
)

type admissionBucket struct {
	// limit is how many requests the bucket admits at once outside a pause:
	// its configured limit (admissionLimits.limitFor), moved by tuning.
	limit     int
	active    int
	waiters   []*admissionWaiter
	pausedTil time.Time
	// changed wakes waiters that queued before a pause was applied, so they
	// arm an expiry timer even if every active stream closes during the pause.
	changed chan struct{}
	// recoverSlowly applies only to non-quota Claude 429s. The ramp starts
	// at pausedTil, so another throttle restarts it without shortening a pause.
	recoverSlowly bool
	served        uint64
	queued        uint64
	blocked       uint64
	throttled     uint64
	// minute is the bucket's activity since its last minute line
	// (admission_minute.go).
	minute admissionMinute
	// Tuning state (admission_tune.go). bound: a request queued at the full
	// limit since the last step. throttledAt and backedOffAt: the last
	// transient 429, and the last one that lowered the limit.
	bound       bool
	throttledAt time.Time
	backedOffAt time.Time
}

// admissionControl is the controller-wide physical-attempt gate. It is
// intentionally process-local: a controller owns one credential pool and all
// clients enter through this process. The lease spans response-body lifetime,
// just like SchedulerRef's in-flight count.
type admissionControl struct {
	mu sync.Mutex
	// limits sizes every bucket (admission_limits.go). A bucket copies its
	// limit when it is created and whenever limits change.
	limits   admissionLimits
	buckets  map[admissionKey]*admissionBucket
	sessions map[admissionSessionKey]*admissionSession
	// logger takes limit changes; unset is slog.Default().
	logger atomic.Pointer[slog.Logger]
}

type admissionLease struct {
	control *admissionControl
	key     admissionKey
	who     admissionSessionKey
	once    sync.Once
}

type admissionKeySnapshot struct {
	Provider       accounts.Provider `json:"provider"`
	AccountHash    string            `json:"account_hash"`
	Model          string            `json:"model"`
	Limit          int               `json:"limit"`
	EffectiveLimit int               `json:"effective_limit"`
	Active         int               `json:"active"`
	Queued         int               `json:"queued"`
	Served         uint64            `json:"served"`
	Blocked        uint64            `json:"blocked"`
	Throttled      uint64            `json:"throttled"`
	PausedUntil    string            `json:"paused_until,omitempty"`
	// Closed: at least the closed queue waits (admissionControl.closed).
	Closed bool `json:"closed,omitempty"`
}

// admissionLimitLocked returns the concurrency allowed now and the next time
// that limit increases. Every admission path uses this under a.mu, including
// spill reservations; waiters also wake on each boundary without an upstream
// response having to complete first.
func (a *admissionControl) admissionLimitLocked(bucket *admissionBucket, now time.Time) (int, time.Time) {
	if bucket.pausedTil.After(now) {
		return 0, bucket.pausedTil
	}
	if !bucket.recoverSlowly {
		return bucket.limit, time.Time{}
	}
	limit := min(1, bucket.limit)
	elapsed := now.Sub(bucket.pausedTil)
	for elapsed >= admissionRecoveryStep && limit < bucket.limit {
		limit = min(bucket.limit, limit*2)
		elapsed -= admissionRecoveryStep
	}
	if limit >= bucket.limit {
		return bucket.limit, time.Time{}
	}
	return limit, now.Add(admissionRecoveryStep - elapsed)
}

// admissionSessionSnapshot is one session's fan-out: how many physical
// attempts it holds and queues across every account right now, its peak, and
// the account it last used. The traffic endpoint is unauthenticated, so it
// carries only a session id prefix: enough to find the session (and the
// client address) in the server log, not enough to send requests as it.
type admissionSessionSnapshot struct {
	Agent         string `json:"agent"`
	SessionPrefix string `json:"session_prefix"`
	Active        int    `json:"active"`
	Queued        int    `json:"queued"`
	Peak          int    `json:"peak"`
	Served        uint64 `json:"served"`
	Blocked       uint64 `json:"blocked"`
	Spilled       uint64 `json:"spilled"`
	AccountHash   string `json:"account_hash,omitempty"`
	LastSeen      string `json:"last_seen"`
}

type admissionSnapshot struct {
	Limit       int            `json:"limit"`
	Queue       int            `json:"queue"`
	SpillQueue  int            `json:"spill_queue"`
	ClosedQueue int            `json:"closed_queue"`
	PerModel    map[string]int `json:"per_model,omitempty"`
	// PerAccount is keyed by account hash, like Accounts.
	PerAccount map[string]int             `json:"per_account,omitempty"`
	Accounts   []admissionKeySnapshot     `json:"accounts,omitempty"`
	Sessions   []admissionSessionSnapshot `json:"sessions,omitempty"`
	Tune       admissionTune              `json:"tune"`
}

func newAdmissionControl() *admissionControl {
	limits := defaultAdmissionLimits()
	// Only runAdmission's clock raises a tuned limit, so a gate without it
	// (a tenant pool's) would only ever back off. StartAdmission turns
	// tuning on with the clock.
	limits.Tune.On = false
	return &admissionControl{
		limits:  limits,
		buckets: make(map[admissionKey]*admissionBucket),
	}
}

func (a *admissionControl) log() *slog.Logger {
	if logger := a.logger.Load(); logger != nil {
		return logger
	}
	return slog.Default()
}

func (a *admissionControl) setLogger(logger *slog.Logger) {
	a.logger.Store(logger)
}

func (a *admissionControl) bucketLocked(key admissionKey) *admissionBucket {
	bucket := a.buckets[key]
	if bucket == nil {
		bucket = &admissionBucket{limit: a.limits.limitFor(key)}
		a.buckets[key] = bucket
	}
	if bucket.changed == nil {
		bucket.changed = make(chan struct{})
	}
	return bucket
}

// sessionLocked returns who's counters, creating them, or nil for a request
// without a session.
func (a *admissionControl) sessionLocked(who admissionSessionKey, now time.Time) *admissionSession {
	if who.session == "" {
		return nil
	}
	if a.sessions == nil {
		a.sessions = make(map[admissionSessionKey]*admissionSession)
	}
	entry := a.sessions[who]
	if entry == nil {
		if len(a.sessions) >= admissionSessionMax {
			a.pruneSessionsLocked(now)
		}
		entry = &admissionSession{}
		a.sessions[who] = entry
	}
	entry.lastSeen = now
	return entry
}

// pruneSessionsLocked drops sessions idle past admissionSessionIdle, and if
// the table is still full, the longest-idle sessions holding nothing.
func (a *admissionControl) pruneSessionsLocked(now time.Time) {
	var oldest admissionSessionKey
	var oldestSeen time.Time
	for who, entry := range a.sessions {
		if entry.active > 0 || entry.queued > 0 {
			continue
		}
		if now.Sub(entry.lastSeen) > admissionSessionIdle {
			delete(a.sessions, who)
			continue
		}
		if oldestSeen.IsZero() || entry.lastSeen.Before(oldestSeen) {
			oldest, oldestSeen = who, entry.lastSeen
		}
	}
	if len(a.sessions) >= admissionSessionMax && !oldestSeen.IsZero() {
		delete(a.sessions, oldest)
	}
}

// grantLocked starts one attempt on key for who: the waiter it already
// dequeued, or a new arrival when waiter is nil.
func (a *admissionControl) grantLocked(key admissionKey, bucket *admissionBucket, who admissionSessionKey, waiter *admissionWaiter, now time.Time) {
	bucket.active++
	bucket.served++
	bucket.minute.served++
	if waiter != nil {
		wait := now.Sub(waiter.queuedAt)
		bucket.minute.waited++
		bucket.minute.waitTotal += wait
		bucket.minute.waitMax = max(bucket.minute.waitMax, wait)
	}
	bucket.noteLoad()
	a.grantSessionLocked(who, key.account, waiter != nil, now)
}

// noteLoad raises the minute's peaks to what the bucket holds now.
func (b *admissionBucket) noteLoad() {
	b.minute.peakActive = max(b.minute.peakActive, b.active)
	b.minute.peakQueued = max(b.minute.peakQueued, len(b.waiters))
	b.minute.peakDemand = max(b.minute.peakDemand, b.active+len(b.waiters))
}

// noteLoad raises the session's peaks to what it holds now.
func (s *admissionSession) noteLoad() {
	load := s.active + s.queued
	s.peak = max(s.peak, load)
	s.minutePeak = max(s.minutePeak, load)
}

// grantSessionLocked moves one of who's attempts onto account.
func (a *admissionControl) grantSessionLocked(who admissionSessionKey, account string, fromQueue bool, now time.Time) {
	entry := a.sessionLocked(who, now)
	if entry == nil {
		return
	}
	if fromQueue && entry.queued > 0 {
		entry.queued--
	}
	entry.active++
	entry.served++
	entry.account = account
	if account == entry.overflowFrom {
		entry.overflowFrom, entry.overflowStreak = "", 0
	}
	entry.noteLoad()
}

// noteQueueFullOverflow records one queue-full overflow off from and reports
// whether the session should now move: the streak reached
// admissionOverflowMoveAfter, which also resets it.
func (a *admissionControl) noteQueueFullOverflow(who admissionSessionKey, from string) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	entry := a.sessionLocked(who, time.Now())
	if entry == nil {
		return false
	}
	entry.spilled++
	if entry.overflowFrom != from {
		entry.overflowFrom, entry.overflowStreak = from, 0
	}
	entry.overflowStreak++
	if entry.overflowStreak < admissionOverflowMoveAfter {
		return false
	}
	entry.overflowFrom, entry.overflowStreak = "", 0
	return true
}

// noteSpill records one request who moved from a full bucket to another
// account's.
func (a *admissionControl) noteSpill(from, to admissionKey, who admissionSessionKey) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if entry := a.sessionLocked(who, time.Now()); entry != nil {
		entry.spilled++
	}
	if bucket := a.buckets[from]; bucket != nil {
		bucket.minute.spillsOut++
	}
	if bucket := a.buckets[to]; bucket != nil {
		bucket.minute.spillsIn++
	}
}

func (a *admissionControl) acquire(ctx context.Context, key admissionKey) (*admissionLease, error) {
	return a.acquireFor(ctx, key, admissionSessionKey{})
}

// acquireFor is acquire with the attempt attributed to who.
func (a *admissionControl) acquireFor(ctx context.Context, key admissionKey, who admissionSessionKey) (*admissionLease, error) {
	if a == nil || key.account == "" {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var waiter *admissionWaiter
	for {
		now := time.Now()
		a.mu.Lock()
		bucket := a.bucketLocked(key)
		limit, nextWake := a.admissionLimitLocked(bucket, now)
		if waiter == nil && len(bucket.waiters) == 0 && bucket.active < limit {
			a.grantLocked(key, bucket, who, nil, now)
			a.mu.Unlock()
			return &admissionLease{control: a, key: key, who: who}, nil
		}
		if waiter != nil && len(bucket.waiters) > 0 && bucket.waiters[0] == waiter &&
			bucket.active < limit {
			copy(bucket.waiters, bucket.waiters[1:])
			bucket.waiters = bucket.waiters[:len(bucket.waiters)-1]
			a.grantLocked(key, bucket, who, waiter, now)
			a.wakeLocked(key, bucket, now)
			a.mu.Unlock()
			return &admissionLease{control: a, key: key, who: who}, nil
		}
		if waiter == nil {
			if bucket.pausedTil.Sub(now) > admissionPauseWaitMax {
				bucket.blocked++
				a.mu.Unlock()
				return nil, errAdmissionPaused
			}
			if len(bucket.waiters) >= a.limits.Queue {
				bucket.blocked++
				bucket.minute.queueFull++
				bucket.bound = bucket.bound || limit >= bucket.limit
				if entry := a.sessionLocked(who, now); entry != nil {
					entry.blocked++
				}
				retryAfter := a.retryAfterLocked(bucket)
				a.mu.Unlock()
				return nil, admissionQueueFullError{retryAfterSeconds: retryAfter}
			}
			waiter = &admissionWaiter{ready: make(chan struct{}), queuedAt: now, who: who}
			bucket.waiters = append(bucket.waiters, waiter)
			bucket.queued++
			bucket.noteLoad()
			bucket.bound = bucket.bound || limit >= bucket.limit
			if entry := a.sessionLocked(who, now); entry != nil {
				entry.queued++
				entry.noteLoad()
			}
		}
		changed := bucket.changed
		a.mu.Unlock()

		var timer <-chan time.Time
		var stop func()
		if !nextWake.IsZero() {
			t := time.NewTimer(time.Until(nextWake))
			timer = t.C
			stop = func() { t.Stop() }
		}
		select {
		case <-waiter.ready:
			if stop != nil {
				stop()
			}
			if waiter.rejected {
				return nil, errAdmissionPaused
			}
			return &admissionLease{control: a, key: key, who: who}, nil
		case <-ctx.Done():
			if stop != nil {
				stop()
			}
			if !a.cancelWaiter(key, waiter) {
				// A release or a long pause already took this waiter. Return the
				// slot only if it was granted; a rejected waiter holds none.
				a.mu.Lock()
				rejected := waiter.rejected
				a.mu.Unlock()
				if !rejected {
					a.release(key, who)
				}
			}
			return nil, ctx.Err()
		case <-timer:
			// Re-check both the pause and recovery ramp in FIFO order.
		case <-changed:
			if stop != nil {
				stop()
			}
			// The deadline changed after this request queued. Arm the new
			// timer even when no response remains in flight to wake us.
		}
	}
}

// retryAfterLocked estimates how long a full queue takes to drain: one
// second per limit-sized batch ahead, clamped to a range clients honor.
func (a *admissionControl) retryAfterLocked(bucket *admissionBucket) int {
	limit := bucket.limit
	if limit < 1 {
		limit = 1
	}
	seconds := (len(bucket.waiters) + limit - 1) / limit
	if seconds < minAdmissionRetryAfter {
		seconds = minAdmissionRetryAfter
	}
	if seconds > maxAdmissionRetryAfter {
		seconds = maxAdmissionRetryAfter
	}
	return seconds
}

// shouldSpill reports whether a new request for key would wait behind at
// least the spill queue, so it is better placed on another account.
func (a *admissionControl) shouldSpill(key admissionKey) bool {
	if a == nil || key.account == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	bucket := a.buckets[key]
	return a.limits.SpillQueue > 0 && bucket != nil && len(bucket.waiters) >= a.limits.SpillQueue
}

// tryAcquire admits a request on key only if it can start now. A spill
// reserves its target this way, under the same lock that counts the slot,
// so concurrent spillers see each other and spread out instead of all
// picking the one account that looked free.
func (a *admissionControl) tryAcquire(key admissionKey, who admissionSessionKey) (*admissionLease, bool) {
	if a == nil || key.account == "" {
		return nil, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	bucket := a.bucketLocked(key)
	limit, _ := a.admissionLimitLocked(bucket, now)
	if len(bucket.waiters) > 0 || bucket.active >= limit {
		return nil, false
	}
	a.grantLocked(key, bucket, who, nil, now)
	return &admissionLease{control: a, key: key, who: who}, true
}

// freeLoad reports whether key would admit a request immediately, and how
// many requests it is already serving, for picking the least-loaded account.
func (a *admissionControl) freeLoad(key admissionKey) (free bool, load int) {
	if a == nil || key.account == "" {
		return false, 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	bucket := a.buckets[key]
	if bucket == nil {
		return true, 0
	}
	limit, _ := a.admissionLimitLocked(bucket, time.Now())
	free = len(bucket.waiters) == 0 && bucket.active < limit
	return free, bucket.active + len(bucket.waiters)
}

func (a *admissionControl) cancelWaiter(key admissionKey, waiter *admissionWaiter) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	bucket := a.buckets[key]
	if bucket == nil {
		return false
	}
	for i, candidate := range bucket.waiters {
		if candidate == waiter {
			copy(bucket.waiters[i:], bucket.waiters[i+1:])
			bucket.waiters = bucket.waiters[:len(bucket.waiters)-1]
			bucket.minute.abandoned++
			if entry := a.sessions[waiter.who]; entry != nil && entry.queued > 0 {
				entry.queued--
			}
			a.wakeLocked(key, bucket, time.Now())
			return true
		}
	}
	return false
}

func (a *admissionControl) release(key admissionKey, who admissionSessionKey) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if entry := a.sessions[who]; entry != nil && entry.active > 0 {
		entry.active--
		entry.lastSeen = time.Now()
	}
	bucket := a.buckets[key]
	if bucket == nil {
		return
	}
	if bucket.active > 0 {
		bucket.active--
	}
	a.wakeLocked(key, bucket, time.Now())
}

func (a *admissionControl) wakeLocked(key admissionKey, bucket *admissionBucket, now time.Time) {
	limit, _ := a.admissionLimitLocked(bucket, now)
	for bucket.active < limit && len(bucket.waiters) > 0 {
		waiter := bucket.waiters[0]
		copy(bucket.waiters, bucket.waiters[1:])
		bucket.waiters = bucket.waiters[:len(bucket.waiters)-1]
		a.grantLocked(key, bucket, waiter.who, waiter, now)
		close(waiter.ready)
	}
}

func (a *admissionControl) pause(key admissionKey, until time.Time) {
	a.pauseWithRecovery(key, until, false)
}

func (a *admissionControl) pauseWithRecovery(key admissionKey, until time.Time, recoverSlowly bool) {
	if a == nil || until.IsZero() {
		return
	}
	a.mu.Lock()
	bucket := a.bucketLocked(key)
	bucket.throttled++
	if now := time.Now(); !bucket.pausedTil.After(now) && until.After(now) {
		bucket.minute.pauses++
	}
	if until.After(bucket.pausedTil) {
		bucket.pausedTil = until
		bucket.recoverSlowly = recoverSlowly
	} else if !recoverSlowly {
		// A shorter transient hint cannot change an authoritative quota
		// pause into transient recovery. A quota response still wins when
		// its deadline is shorter than an existing transient pause.
		bucket.recoverSlowly = false
	}
	// Requests already queued would otherwise sit out the whole pause. Turn
	// them away like new attempts so their callers can fail over.
	if bucket.pausedTil.Sub(time.Now()) > admissionPauseWaitMax {
		for _, waiter := range bucket.waiters {
			waiter.rejected = true
			if entry := a.sessions[waiter.who]; entry != nil {
				if entry.queued > 0 {
					entry.queued--
				}
				entry.blocked++
			}
			close(waiter.ready)
		}
		bucket.blocked += uint64(len(bucket.waiters))
		bucket.waiters = nil
	}
	close(bucket.changed)
	bucket.changed = make(chan struct{})
	a.mu.Unlock()
}

func (a *admissionControl) observe(key admissionKey, status int, header http.Header, now time.Time) {
	if a == nil || status != http.StatusTooManyRequests {
		return
	}
	until := retryAfterExpiry(header, now)
	recoverSlowly := false
	if key.provider == accounts.ProviderClaude {
		if claudeResponseRejectedForPool(header, key.model) {
			until = claudeExhaustionExpiryForPool(header, now, key.model)
			if limit := now.Add(admissionQuotaPauseMax); until.After(limit) {
				until = limit
			}
		} else {
			recoverSlowly = true
			// A bare/allowed 429 is a short request throttle, not quota
			// exhaustion. Honor Retry-After when present, without inheriting
			// an unrelated healthy quota window's reset or the quota default.
			if until.IsZero() {
				until = now.Add(claudeTransientThrottleFallback)
			}
		}
	}
	if until.IsZero() {
		until = now.Add(time.Second)
	}
	a.pauseWithRecovery(key, until, recoverSlowly)
	if recoverSlowly {
		a.backoff(key, now)
	}
}

// admissionGate is the server's admission gate, or the one its account ref
// shares when this Server copy was never normalized; nil when neither exists.
func (s Server) admissionGate() *admissionControl {
	if s.admission != nil {
		return s.admission
	}
	if s.AccountRef != nil {
		return s.AccountRef.admission.Load()
	}
	return nil
}

// clearAccount ends every admission pause on accountID (each pool model of
// it), for an account whose limit was just reset: a Claude reset claim
// (afterClaudeReset) or a redeemed Codex rate-limit reset. The scheduler's
// marks are cleared there too; a pause left behind refused the reset account
// locally until it ran out (a Codex 429's Retry-After is not capped).
func (a *admissionControl) clearAccount(provider accounts.Provider, accountID string) {
	if a == nil || accountID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for key, bucket := range a.buckets {
		if key.provider != provider || key.account != accountID || !bucket.pausedTil.After(now) {
			continue
		}
		bucket.pausedTil = time.Time{}
		bucket.recoverSlowly = false
		close(bucket.changed)
		bucket.changed = make(chan struct{})
		a.wakeLocked(key, bucket, now)
	}
}

// paused reports whether an account/model is in an admission cooldown. It is
// deliberately separate from SchedulerRef's quota overlays: transient Claude
// throttles must not appear as exhausted in status or score snapshots.
func (a *admissionControl) paused(key admissionKey, now time.Time) bool {
	if a == nil || key.account == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	bucket := a.buckets[key]
	return bucket != nil && bucket.pausedTil.After(now)
}

func (l *admissionLease) release() {
	if l == nil || l.control == nil {
		return
	}
	l.once.Do(func() { l.control.release(l.key, l.who) })
}

func (a *admissionControl) snapshot() admissionSnapshot {
	if a == nil {
		return admissionSnapshot{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := admissionSnapshot{
		Limit:       a.limits.Limit,
		Queue:       a.limits.Queue,
		SpillQueue:  a.limits.SpillQueue,
		ClosedQueue: a.limits.ClosedQueue,
		PerModel:    maps.Clone(a.limits.PerModel),
		Tune:        a.limits.Tune,
	}
	for account, limit := range a.limits.PerAccount {
		if out.PerAccount == nil {
			out.PerAccount = make(map[string]int, len(a.limits.PerAccount))
		}
		out.PerAccount[admissionAccountHash(account)] = limit
	}
	now := time.Now()
	for key, bucket := range a.buckets {
		limit, _ := a.admissionLimitLocked(bucket, now)
		entry := admissionKeySnapshot{
			Provider:       key.provider,
			AccountHash:    admissionAccountHash(key.account),
			Model:          key.model,
			Limit:          bucket.limit,
			EffectiveLimit: limit,
			Active:         bucket.active,
			Queued:         len(bucket.waiters),
			Served:         bucket.served,
			Blocked:        bucket.blocked,
			Throttled:      bucket.throttled,
			Closed:         a.closedLocked(bucket),
		}
		if bucket.pausedTil.After(now) {
			entry.PausedUntil = bucket.pausedTil.UTC().Format(time.RFC3339)
		}
		out.Accounts = append(out.Accounts, entry)
	}
	a.pruneSessionsLocked(now)
	for who, entry := range a.sessions {
		session := admissionSessionSnapshot{
			Agent:         who.agent,
			SessionPrefix: admissionSessionPrefix(who.session),
			Active:        entry.active,
			Queued:        entry.queued,
			Peak:          entry.peak,
			Served:        entry.served,
			Blocked:       entry.blocked,
			Spilled:       entry.spilled,
			LastSeen:      entry.lastSeen.UTC().Format(time.RFC3339),
		}
		if entry.account != "" {
			session.AccountHash = admissionAccountHash(entry.account)
		}
		out.Sessions = append(out.Sessions, session)
	}
	// Busiest first: what a session holds now, then the most it ever held.
	sort.Slice(out.Sessions, func(i, j int) bool {
		left, right := out.Sessions[i], out.Sessions[j]
		if l, r := left.Active+left.Queued, right.Active+right.Queued; l != r {
			return l > r
		}
		if left.Peak != right.Peak {
			return left.Peak > right.Peak
		}
		return left.SessionPrefix < right.SessionPrefix
	})
	if len(out.Sessions) > admissionSessionTop {
		out.Sessions = out.Sessions[:admissionSessionTop]
	}
	return out
}

// admissionSessionPrefixLen is the length of the session id shown in the
// traffic snapshot.
const admissionSessionPrefixLen = 8

func admissionSessionPrefix(session string) string {
	if len(session) > admissionSessionPrefixLen {
		return session[:admissionSessionPrefixLen]
	}
	return session
}

func admissionAccountHash(account string) string {
	digest := sha256.Sum256([]byte(account))
	return "sha256:" + hex.EncodeToString(digest[:8])
}

func admissionKeyFor(provider accounts.Provider, accountID, model string) admissionKey {
	if model == "" {
		model = "default"
	}
	return admissionKey{provider: provider, account: accountID, model: model}
}
