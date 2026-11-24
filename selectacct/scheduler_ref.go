package selectacct

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/discordwell/subrouter/account"
)

type SchedulerRef struct {
	mu                 sync.RWMutex
	scheduler          Scheduler
	updatedAt          time.Time
	refreshing         bool
	refreshInvalidated bool
	accountGeneration  uint64
	refreshGeneration  uint64
	scoreRevision      uint64
	// legacyFinishInvalidated preserves the historical tokenless FinishRefresh
	// API for callers that publish without BeginRefreshIfStale. New concurrent
	// code uses the generation-aware methods below.
	legacyFinishInvalidated bool
	// scored says the ref holds scores worth routing on while a refresh runs
	// (Scored). It is set by construction, by every publish and by every
	// finished refresh, and cleared only by SetUpdatedAt(time.Time{}). An
	// account-generation advance zeroes updatedAt but leaves it alone: that
	// makes the scores stale, not absent.
	scored bool
	// exhaustedUntil expires request-time exhaustion marks. A mark set from a
	// rejected upstream response is only true until the account's rate-limit
	// window resets; without an expiry a recovered account stayed zero-scored
	// until the next SUCCESSFUL usage refresh, which under load can fail for
	// hours, leaving real quota unroutable while clients got 429s.
	exhaustedUntil map[string]time.Time
	// A score publish may only supersede an exhaustion mark with evidence its
	// measurement could have seen (upstream #398, adapted; PATCHES.md §29): a
	// 429 that lands after the usage reading behind a Fresh score is newer
	// than that score, so the score's headroom is not recovery. Two orders
	// decide "newer", in this priority:
	//
	//   - exhaustedMarkedAt is when each mark was set. A score that carries
	//     Score.MeasuredAt -- every score this daemon builds from a usage
	//     reading -- supersedes only marks set before that instant. This is
	//     the order that matters here: our scores are routinely published
	//     well after they were measured (the 2-minute window cache, the 30 s
	//     sweep cache every usage-status read republishes, PATCHES.md §26),
	//     so when a publish BEGAN says little about when its headroom was
	//     true.
	//   - markSeq numbers marks as they are set (exhaustedMarkSeq holds each
	//     mark's number), upstream's rule, kept for scores that carry no
	//     measurement time: refreshMarkSeq is markSeq when the in-flight full
	//     refresh began, revisionMarkSeq is markSeq when the current
	//     scoreRevision was first read, the start of any Set/Merge computed
	//     at that revision. Only marks at or below that number can be
	//     superseded.
	//
	// Upstream's weekly-mark half is not ported: this fork skipped T3 and
	// has no weekly-cooked mark to reconcile.
	markSeq            uint64
	exhaustedMarkSeq   map[string]uint64
	exhaustedMarkedAt  map[string]time.Time
	refreshMarkSeq     uint64
	revisionMarkSeq    uint64
	revisionMarkSeqSet bool
	// credentialExhaustedUntil is separate from quota/model evidence and is
	// scoped to the account snapshot generation that observed the bad token.
	// Replacing credentials advances the generation, immediately discarding the
	// old token's exclusion while rejecting late reports from in-flight work.
	credentialExhaustedUntil map[string]time.Time
	credentialFingerprints   map[string]string
	credentialRevision       uint64
	// accountAliases is AccountAliases() of the account list last published
	// with the credential fingerprints, attached to every Get so reserves
	// configured by email or label resolve for accounts whose ID is an opaque
	// "codex-owner-<hash>". Replaced wholesale, never mutated in place, so
	// schedulers already handed out keep a consistent table.
	accountAliases map[string][]string
	// accountUnavailableUntil records account-state exclusions, such as an
	// organization disabling OAuth. Neither healthy quota evidence nor token
	// rotation proves that account state recovered, so those events must not
	// clear this overlay.
	accountUnavailableUntil map[string]time.Time
	// credentialUnavailable records persistent authorization or entitlement
	// denials for the exact credential that observed them (upstream #585). A
	// credential reload changes the fingerprint, which makes the old denial
	// inactive immediately.
	credentialUnavailable map[string]struct{}
	// incompatibleUntil records account/model exclusions learned from upstream
	// entitlement errors. Usage refreshes cannot supersede these marks because
	// quota headroom says nothing about whether an account supports a model.
	incompatibleUntil map[string]time.Time
	// recoveryProbeReady remembers that an authoritative exclusion expired while
	// the measured snapshot still read zero. Routing may attempt exactly one
	// optimistic probe; RunIfAccountNotBlocked consumes the allowance atomically.
	recoveryProbeReady map[string]struct{}
	// routedSinceRefresh counts requests the proxy routed per account (by
	// ScoreKey) since the last successful usage refresh. Pick debits headroom
	// by LiveDebitPerRequest per routed request so concurrent traffic spreads
	// instead of herding onto the snapshot's best account until it cooks.
	routedSinceRefresh map[string]int
	// capacityUntil holds capacity (load-shedding) marks per account and
	// (model, service tier). They are deliberately not an exhaustion overlay:
	// see capacity.go. Living beside the snapshot, they survive Set() and
	// usage refreshes, and they are never persisted across restarts.
	capacityUntil map[capacityMarkKey]capacityMark
	// rateLimitedUntil holds rate-limit marks per account (ScoreKey): a
	// request-rate refusal that ranks the account last for new placements
	// and never evicts (rate_limit_marks.go). Never persisted either.
	rateLimitedUntil map[string]time.Time
	// startSentUntil holds start marks per account (ScoreKey): a new session
	// was given to an account whose week had not started, so the priority of
	// PATCHES §86 is off it until the mark expires (unstarted_week.go); the
	// daemon's own start request sets the same mark (ClaimStartMark, §90).
	// Never cleared early and never persisted.
	startSentUntil map[string]time.Time
	// placementReservations counts picks that have been made but have not yet
	// crossed the caller's upstream-success boundary (PickAndReserve, upstream
	// #642). They steer concurrent placements immediately, then become
	// routedSinceRefresh on Commit or are discarded on Release.
	placementReservations map[string]int
	// inflight counts physical upstream attempts whose response is still open,
	// keyed by ScoreKey (BeginInflight, upstream #449). Unlike
	// routedSinceRefresh it is instantaneous and is never cleared by a usage
	// refresh. Diagnostics only: ranking does not read it (InflightCounts).
	inflight map[string]int
	// lastDemand is when a request for each provider was last routed or
	// turned away for lack of a usable account (upstream #392). Unlike
	// routedSinceRefresh it survives refreshes, so it answers "is anyone
	// asking right now?".
	lastDemand map[account.Provider]time.Time
	// placement counts where work actually went since process start
	// (upstream #388); it has its own lock (see placement_stats.go).
	placement placementStats
	pace      paceTracker
}

func NewSchedulerRef(scheduler Scheduler) *SchedulerRef {
	now := time.Now()
	return &SchedulerRef{
		scheduler: scheduler,
		updatedAt: now,
		scored:    true,
		placement: placementStats{since: now},
	}
}

func (r *SchedulerRef) Get() Scheduler {
	now := time.Now()
	r.pruneExpired(now)
	r.mu.RLock()
	defer r.mu.RUnlock()
	scheduler := applyExhaustionMarks(r.scheduler, r.exhaustedUntil, now)
	scheduler = applyExhaustionMarks(scheduler, r.activeCredentialExhaustionLocked(), now)
	scheduler = applyExhaustionMarks(scheduler, r.accountUnavailableUntil, now)
	scheduler = applyPersistentCredentialMarks(scheduler, r.activeCredentialUnavailableLocked())
	scheduler = applyExhaustionMarks(scheduler, r.incompatibleUntil, now)
	// Aliases ride on every read rather than on the stored snapshot: a usage
	// refresh publishes a freshly built Scheduler, which must not be able to
	// drop the table that lets an email-keyed holdout find a codex-owner id.
	if r.accountAliases != nil {
		scheduler = scheduler.withAccountAliasMap(r.accountAliases)
	}
	if paces := r.pace.snapshot.Load(); paces != nil {
		scheduler = scheduler.withPaces(*paces)
	}
	// Like the aliases, the start marks ride on every read: they live beside
	// the snapshot, so a refresh cannot drop them.
	if marks := r.startSentLocked(now); marks != nil {
		scheduler = scheduler.withStartSent(marks)
	}
	return scheduler
}

// RefreshSeed returns the measured scheduler snapshot, including live routing
// debits but excluding temporary exclusion overlays. A usage refresh carries
// stale scores forward when an account cannot be fetched; seeding that work
// from Get would bake an overlay's zero into the replacement snapshot if the
// overlay expired between scoring and FinishRefresh. Routing reads must use
// Get, while refresh construction must use this base-only view.
func (r *SchedulerRef) RefreshSeed() Scheduler {
	if r == nil {
		return Scheduler{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.scheduler.WithLiveDebits(r.liveDebitsLocked())
}

// PlacementReservation is the live-load reservation returned by
// PickAndReserve (upstream #642, #650). Commit turns the reservation into the
// same routed-request debit recorded by NoteRouted. Release removes it when
// account preparation fails before the request is routed. Either method is
// idempotent.
type PlacementReservation struct {
	ref        *SchedulerRef
	key        string
	provider   account.Provider
	accountID  string
	once       sync.Once
	cancelMu   sync.Mutex
	cancelStop func() bool
}

// bindContext arranges for a request cancellation to release an uncommitted
// reservation. The callback races safely with Commit and Release through the
// same once gate; it only removes the pending placement and never records a
// routed request.
func (p *PlacementReservation) bindContext(ctx context.Context) {
	if p == nil || p.ref == nil || ctx == nil {
		return
	}
	stop := context.AfterFunc(ctx, func() { p.releaseOnce() })
	p.cancelMu.Lock()
	p.cancelStop = stop
	p.cancelMu.Unlock()
}

func (p *PlacementReservation) finish(fn func()) {
	if p == nil || p.ref == nil {
		return
	}
	p.once.Do(func() {
		p.cancelMu.Lock()
		stop := p.cancelStop
		p.cancelStop = nil
		p.cancelMu.Unlock()
		if stop != nil {
			stop()
		}
		fn()
	})
}

func (p *PlacementReservation) releaseOnce() {
	p.finish(func() { p.ref.releasePlacementReservation(p.key) })
}

// Commit records the reserved request as routed. It is safe to call more than
// once; only the first call changes scheduler state or placement statistics.
func (p *PlacementReservation) Commit() {
	p.finish(func() { p.ref.commitPlacementReservation(p.key, p.provider, p.accountID) })
}

// Release drops a reservation without recording a routed request. It is safe
// to call more than once.
func (p *PlacementReservation) Release() {
	p.releaseOnce()
}

// PickAndReserve chooses an account and reserves its live placement load as
// one atomic operation. The scheduler passed by the caller supplies the
// immutable selection state (for example the model pool, session counts,
// reserves, the machine view and capacity marks); its live debits are
// replaced with the latest values held by this ref -- committed routes plus
// pending reservations -- while its lock is held.
//
// Upstream also attaches its in-flight counts here (WithInflightCounts, #449),
// a headroom penalty per open attempt. This fork does not: live concurrency
// enters ranking as Score.LiveSessions through the session-spread discount,
// and an open attempt has spent nothing (Score.spreadAdjusted, PATCHES §10).
//
// The caller must Commit or Release the returned reservation. A committed
// reservation replaces the later NoteRouted call for that request.
func (r *SchedulerRef) PickAndReserve(scheduler Scheduler, candidates []account.Account) (account.Account, *PlacementReservation, error) {
	return r.pickAndReserve(scheduler, candidates)
}

// PickAndReserveContext is PickAndReserve with request-lifetime cleanup. If
// ctx is canceled before the caller commits the upstream placement, the
// pending reservation is released automatically. This closes the cancellation
// gap between account selection and the upstream success boundary.
func (r *SchedulerRef) PickAndReserveContext(ctx context.Context, scheduler Scheduler, candidates []account.Account) (account.Account, *PlacementReservation, error) {
	picked, reservation, err := r.pickAndReserve(scheduler, candidates)
	if err == nil && reservation != nil {
		reservation.bindContext(ctx)
	}
	return picked, reservation, err
}

func (r *SchedulerRef) pickAndReserve(scheduler Scheduler, candidates []account.Account) (account.Account, *PlacementReservation, error) {
	if r == nil {
		picked, err := scheduler.Pick(candidates)
		return picked, nil, err
	}

	r.mu.Lock()
	scheduler = scheduler.WithLiveDebits(r.liveDebitsLocked())
	picked, err := scheduler.Pick(candidates)
	if err != nil {
		r.mu.Unlock()
		return account.Account{}, nil, err
	}
	if r.placementReservations == nil {
		r.placementReservations = make(map[string]int)
	}
	key := ScoreKey(picked.Provider, picked.ID)
	r.placementReservations[key]++
	r.mu.Unlock()
	return picked, &PlacementReservation{ref: r, key: key, provider: picked.Provider, accountID: picked.ID}, nil
}

// commitPlacementReservation converts one pending reservation into the
// existing routed-request debit and placement statistic.
func (r *SchedulerRef) commitPlacementReservation(key string, provider account.Provider, accountID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if count := r.placementReservations[key]; count <= 1 {
		delete(r.placementReservations, key)
	} else {
		r.placementReservations[key] = count - 1
	}
	if accountID == "" {
		r.mu.Unlock()
		return
	}
	if r.routedSinceRefresh == nil {
		r.routedSinceRefresh = make(map[string]int)
	}
	r.routedSinceRefresh[key]++
	r.noteDemandLocked(provider, time.Now())
	r.mu.Unlock()
	r.noteRoutedStat(provider, accountID)
}

// releasePlacementReservation removes one pending reservation without
// recording a routed request.
func (r *SchedulerRef) releasePlacementReservation(key string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if count := r.placementReservations[key]; count <= 1 {
		delete(r.placementReservations, key)
	} else {
		r.placementReservations[key] = count - 1
	}
}

// liveDebitsLocked returns committed and pending placement load for callers
// that already hold r.mu.
func (r *SchedulerRef) liveDebitsLocked() map[string]int {
	if len(r.routedSinceRefresh) == 0 && len(r.placementReservations) == 0 {
		return nil
	}
	debits := make(map[string]int, len(r.routedSinceRefresh)+len(r.placementReservations))
	for key, count := range r.routedSinceRefresh {
		debits[key] = count
	}
	for key, count := range r.placementReservations {
		debits[key] += count
	}
	return debits
}

// RunIfAccountNotBlocked executes publish while the current scheduler state
// remains read-locked. The caller may hold its own publication mutex, but
// publish must not call back into this SchedulerRef.
func (r *SchedulerRef) RunIfAccountNotBlocked(
	provider account.Provider,
	accountID string,
	model string,
	now time.Time,
	publish func(),
) (time.Time, bool) {
	if r == nil {
		publish()
		return time.Time{}, true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if until, blocked := r.blockedUntilLocked(provider, accountID, model, now); blocked {
		return until, false
	}
	r.clearExpiredRouteMarksLocked(provider, accountID, model, now)
	publish()
	return time.Time{}, true
}

// BlockedUntilFor reports the union of account-wide, credential, quota-pool,
// account-state, and model-incompatibility exclusions for one route.
func (r *SchedulerRef) BlockedUntilFor(
	provider account.Provider,
	accountID string,
	model string,
	now time.Time,
) (time.Time, bool) {
	if r == nil {
		return time.Time{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.blockedUntilLocked(provider, accountID, model, now)
}

// ExplicitBlockedUntilFor reports only time-bounded exclusion marks, without
// interpreting a missing model score as exhausted. API-key accounts do not
// participate in subscription model-pool scoring, but still honor explicit
// account, credential, and model-incompatibility state.
func (r *SchedulerRef) ExplicitBlockedUntilFor(
	provider account.Provider,
	accountID string,
	model string,
	now time.Time,
) (time.Time, bool) {
	if r == nil {
		return time.Time{}, false
	}
	r.pruneExpired(now)
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.persistentCredentialBlockedLocked(provider, accountID) {
		return time.Time{}, true
	}
	return r.explicitBlockedUntilLocked(provider, accountID, model, now)
}

func (r *SchedulerRef) explicitBlockedUntilLocked(
	provider account.Provider,
	accountID string,
	model string,
	now time.Time,
) (time.Time, bool) {
	marks := r.expiryMarksLocked()
	var until time.Time
	for _, key := range []string{
		poolScopedExhaustionKey(provider, accountID, ""),
		poolScopedExhaustionKey(provider, accountID, model),
	} {
		if candidate := marks[key]; candidate.After(until) {
			until = candidate
		}
	}
	return until, until.After(now)
}

// RunIfAccountNotExplicitlyBlocked is the API-key publication guard. It
// ignores absent subscription pool scores while retaining explicit routing
// exclusions and the same lock-order guarantee as RunIfAccountNotBlocked.
func (r *SchedulerRef) RunIfAccountNotExplicitlyBlocked(
	provider account.Provider,
	accountID string,
	model string,
	now time.Time,
	publish func(),
) (time.Time, bool) {
	if r == nil {
		publish()
		return time.Time{}, true
	}
	r.pruneExpired(now)
	r.mu.RLock()
	defer r.mu.RUnlock()
	if until, blocked := r.explicitBlockedUntilLocked(provider, accountID, model, now); blocked {
		return until, false
	}
	publish()
	return time.Time{}, true
}

func (r *SchedulerRef) blockedUntilLocked(
	provider account.Provider,
	accountID string,
	model string,
	now time.Time,
) (time.Time, bool) {
	scheduler := applyExhaustionMarks(r.scheduler, r.exhaustedUntil, now)
	scheduler = applyExhaustionMarks(scheduler, r.activeCredentialExhaustionLocked(), now)
	scheduler = applyExhaustionMarks(scheduler, r.accountUnavailableUntil, now)
	scheduler = applyPersistentCredentialMarks(scheduler, r.activeCredentialUnavailableLocked())
	scheduler = applyExhaustionMarks(scheduler, r.incompatibleUntil, now)
	if r.persistentCredentialBlockedLocked(provider, accountID) {
		return time.Time{}, true
	}
	if !scheduler.ForModel(model).Exhausted(provider, accountID) {
		return time.Time{}, false
	}
	var until time.Time
	marks := r.expiryMarksLocked()
	for _, key := range []string{
		poolScopedExhaustionKey(provider, accountID, ""),
		poolScopedExhaustionKey(provider, accountID, model),
	} {
		if candidate := marks[key]; candidate.After(until) {
			until = candidate
		}
	}
	if until.IsZero() {
		if r.recoveryProbeReadyLocked(provider, accountID, model) {
			return time.Time{}, false
		}
		until = now.Add(DefaultExhaustedTTL)
	} else if !until.After(now) {
		// An expired authoritative mark grants one optimistic probe even when
		// the measured snapshot still says zero. The publication guard consumes
		// that mark atomically so concurrent lease attempts cannot all probe.
		return time.Time{}, false
	}
	return until, true
}

func (r *SchedulerRef) clearExpiredRouteMarksLocked(
	provider account.Provider,
	accountID string,
	model string,
	now time.Time,
) {
	keys := []string{
		poolScopedExhaustionKey(provider, accountID, ""),
		poolScopedExhaustionKey(provider, accountID, model),
	}
	for _, key := range keys {
		delete(r.recoveryProbeReady, key)
		if until := r.exhaustedUntil[key]; !until.IsZero() && !until.After(now) {
			r.dropExhaustedMarkLocked(key)
		}
	}
	for _, marks := range []map[string]time.Time{
		r.exhaustedUntil,
		r.accountUnavailableUntil,
		r.incompatibleUntil,
	} {
		for _, key := range keys {
			if until := marks[key]; !until.IsZero() && !until.After(now) {
				delete(marks, key)
			}
		}
	}
	// Credential exclusions include the credential fingerprint after the score
	// key, so remove any expired entry for this route's account.
	prefix := ScoreKey(provider, accountID) + "\x00"
	for key, until := range r.credentialExhaustedUntil {
		if strings.HasPrefix(key, prefix) && !until.After(now) {
			delete(r.credentialExhaustedUntil, key)
		}
	}
}

func (r *SchedulerRef) recoveryProbeReadyLocked(provider account.Provider, accountID, model string) bool {
	for _, key := range []string{
		poolScopedExhaustionKey(provider, accountID, ""),
		poolScopedExhaustionKey(provider, accountID, model),
	} {
		if _, ok := r.recoveryProbeReady[key]; ok {
			return true
		}
	}
	return false
}

// pruneExpired drops exhaustion marks whose window has reset. The base snapshot
// is not mutated; once the overlay mark is gone, routing reads the snapshot (or
// optimistic default) normally.
//
// Deliberate tradeoff: a lapsed mark cannot distinguish "recovered" from "fresh
// usage re-confirmed exhausted" (refreshes seed zero scores forward, so the two
// are indistinguishable here). We choose optimistic retry-once: if the account
// is still cooked, the one probe request is rejected upstream and the account
// is immediately re-marked with the NEW authoritative reset time, so the cost
// is bounded at one attempt per account per expiry window. The opposite choice
// (trusting a zero score without an expiry) is the failure this fixes: a
// recovered account's real quota stayed unroutable for hours. This matches the
// scheduler-wide philosophy that scores are load-balancing hints and the
// upstream response is the source of truth.
func (r *SchedulerRef) pruneExpired(now time.Time) {
	r.mu.RLock()
	anyExpired := false
	for _, until := range r.exhaustedUntil {
		if !until.After(now) {
			anyExpired = true
			break
		}
	}
	if !anyExpired {
		for _, until := range r.credentialExhaustedUntil {
			if !until.After(now) {
				anyExpired = true
				break
			}
		}
	}
	if !anyExpired {
		for _, until := range r.accountUnavailableUntil {
			if !until.After(now) {
				anyExpired = true
				break
			}
		}
	}
	if !anyExpired {
		for _, until := range r.incompatibleUntil {
			if !until.After(now) {
				anyExpired = true
				break
			}
		}
	}
	r.mu.RUnlock()
	if !anyExpired {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, until := range r.exhaustedUntil {
		if until.After(now) {
			continue
		}
		probeKeys := r.recoveryProbeKeysForExpiredMarkLocked(key)
		if len(probeKeys) > 0 {
			if r.recoveryProbeReady == nil {
				r.recoveryProbeReady = make(map[string]struct{})
			}
			for _, probeKey := range probeKeys {
				r.recoveryProbeReady[probeKey] = struct{}{}
			}
		}
		r.dropExhaustedMarkLocked(key)
	}
	for key, until := range r.credentialExhaustedUntil {
		if !until.After(now) {
			delete(r.credentialExhaustedUntil, key)
		}
	}
	for key, until := range r.accountUnavailableUntil {
		if !until.After(now) {
			delete(r.accountUnavailableUntil, key)
		}
	}
	for key, until := range r.incompatibleUntil {
		if until.After(now) {
			continue
		}
		delete(r.incompatibleUntil, key)
	}
}

func (r *SchedulerRef) scoreForExhaustionKeyLocked(key string) (Score, bool) {
	scoreKey, _, poolKey, ok := exhaustionKeyParts(key)
	if !ok {
		return Score{}, false
	}
	score, ok := r.scheduler.scores[scoreKey]
	if !ok {
		return Score{}, false
	}
	if poolKey != "" {
		modelScore, exists := score.ModelScores[poolKey]
		if exists {
			return modelScore, true
		}
		if r.scheduler.hasModelScore(poolKey) && score.missingModelSupport() != ModelSupportUnknown {
			return Score{}, false
		}
	}
	return score, true
}

func (r *SchedulerRef) recoveryProbeKeysForExpiredMarkLocked(key string) []string {
	scoreKey, provider, poolKey, ok := exhaustionKeyParts(key)
	if !ok {
		return nil
	}
	score, ok := r.scheduler.scores[scoreKey]
	if !ok {
		return nil
	}
	if poolKey != "" {
		modelScore, exists := score.ModelScores[poolKey]
		if exists && modelScore.exhausted() {
			return []string{key}
		}
		if !exists && (!r.scheduler.hasModelScore(poolKey) || score.missingModelSupport() == ModelSupportUnknown) && score.exhausted() {
			return []string{key}
		}
		return nil
	}
	if score.exhausted() {
		return []string{key}
	}
	keys := make([]string, 0, len(score.ModelScores))
	for model, modelScore := range score.ModelScores {
		if modelScore.exhausted() {
			keys = append(keys, poolScopedExhaustionKey(provider, score.AccountID, model))
		}
	}
	return keys
}

func (r *SchedulerRef) Set(scheduler Scheduler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	inflightRefresh := r.refreshing
	r.setLocked(scheduler)
	if inflightRefresh {
		r.refreshInvalidated = true
		return
	}
	r.refreshing = false
	r.refreshInvalidated = false
}

func (r *SchedulerRef) setLocked(scheduler Scheduler) {
	r.setLockedForScoreKeys(scheduler, nil, true)
}

func (r *SchedulerRef) setLockedForScoreKeys(scheduler Scheduler, scoreKeys map[string]struct{}, touchUpdatedAt bool) {
	base := r.scheduler
	r.scheduler = scheduler
	r.retainExhaustedExpiriesForScoreKeysLocked(scoreKeys, r.revisionSinceSeqLocked())
	r.scheduler = stripCarriedForwardExhaustionOverlaysForScoreKeys(r.scheduler, base, r.expiryMarksLocked(), scoreKeys)
	now := time.Now()
	for key := range r.recoveryProbeReady {
		scoreKey, _, _, valid := exhaustionKeyParts(key)
		if scoreKeys != nil {
			if _, included := scoreKeys[scoreKey]; valid && !included {
				continue
			}
		}
		score, ok := r.scoreForExhaustionKeyLocked(key)
		if !ok || !score.exhausted() {
			delete(r.recoveryProbeReady, key)
			continue
		}
		if score.Fresh {
			delete(r.recoveryProbeReady, key)
			if r.exhaustedUntil == nil {
				r.exhaustedUntil = make(map[string]time.Time)
			}
			until := now.Add(DefaultExhaustedTTL)
			if score.ShortResetAfterSeconds > 0 {
				if fromReset := now.Add(time.Duration(score.ShortResetAfterSeconds) * time.Second); fromReset.After(until) {
					until = fromReset
				}
			}
			if cap := now.Add(8 * 24 * time.Hour); until.After(cap) {
				until = cap
			}
			r.exhaustedUntil[key] = until
		}
	}
	if touchUpdatedAt {
		r.updatedAt = time.Now()
	}
	r.scored = true
	r.bumpScoreRevisionLocked()
	r.pace.observeScores(r.scheduler)
}

// AdvanceAccountGeneration invalidates refresh work computed from an older
// account snapshot. Callers advance immediately after publishing a new
// AccountRef snapshot, before any potentially slow usage scoring begins.
func (r *SchedulerRef) AdvanceAccountGeneration(generation uint64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.advanceAccountGenerationLocked(generation)
}

// AdvanceAccountGenerationWithAccounts advances the snapshot and publishes
// the credential identity currently attached to each routing account. Existing
// exclusions remain effective for unchanged tokens; repaired tokens do not
// inherit them.
func (r *SchedulerRef) AdvanceAccountGenerationWithAccounts(generation, credentialRevision uint64, accounts []account.Account) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation < r.accountGeneration {
		return
	}
	if generation == r.accountGeneration && credentialRevision <= r.credentialRevision && r.credentialFingerprints != nil {
		return
	}
	r.advanceAccountGenerationLocked(generation)
	r.credentialRevision = credentialRevision
	r.pruneExhaustedMarksForAccountsLocked(accounts)
	r.credentialFingerprints = make(map[string]string, len(accounts))
	for _, candidate := range accounts {
		r.credentialFingerprints[ScoreKey(candidate.Provider, candidate.ID)] = credentialFingerprint(candidate.CredentialIdentity())
	}
	r.accountAliases = AccountAliases(accounts)
}

// pruneExhaustedMarksForAccountsLocked removes quota marks for accounts that
// disappeared from the latest account snapshot (upstream #561). Marks for
// accounts that remain present are intentionally left untouched so a stale or
// missing usage refresh cannot make a recently rejected account eligible
// again: a refresh that carries no score for an account keeps its mark
// (retainExhaustedExpiriesForScoreKeysLocked), and this is where a mark whose
// account is gone ends. A persistent credential denial for a removed account
// goes with it.
func (r *SchedulerRef) pruneExhaustedMarksForAccountsLocked(accounts []account.Account) {
	present := make(map[string]struct{}, len(accounts))
	for _, candidate := range accounts {
		present[ScoreKey(candidate.Provider, candidate.ID)] = struct{}{}
	}
	absent := func(key string) bool {
		scoreKey, _, _, ok := exhaustionKeyParts(key)
		if !ok {
			return true
		}
		_, exists := present[scoreKey]
		return !exists
	}
	for key := range r.exhaustedUntil {
		if absent(key) {
			r.dropExhaustedMarkLocked(key)
		}
	}
	// The ordering stamps normally go with their mark; an orphan whose mark
	// is already gone is swept here too.
	for key := range r.exhaustedMarkSeq {
		if absent(key) {
			delete(r.exhaustedMarkSeq, key)
		}
	}
	for key := range r.exhaustedMarkedAt {
		if absent(key) {
			delete(r.exhaustedMarkedAt, key)
		}
	}
	for key := range r.credentialUnavailable {
		parts := strings.SplitN(key, "\x00", 3)
		if len(parts) != 3 {
			delete(r.credentialUnavailable, key)
			continue
		}
		if _, exists := present[ScoreKey(account.Provider(parts[0]), parts[1])]; !exists {
			delete(r.credentialUnavailable, key)
		}
	}
}

// SyncAccountCredentials publishes token rotations that occur within an
// account generation (for example, a successful OAuth refresh). It does not
// invalidate score work; it only changes which credential-scoped exclusions
// apply to the current snapshot.
func (r *SchedulerRef) SyncAccountCredentials(generation, credentialRevision uint64, accounts []account.Account) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation != r.accountGeneration || credentialRevision < r.credentialRevision {
		return false
	}
	if credentialRevision == r.credentialRevision && r.credentialFingerprints != nil {
		return true
	}
	r.credentialRevision = credentialRevision
	r.credentialFingerprints = make(map[string]string, len(accounts))
	for _, candidate := range accounts {
		r.credentialFingerprints[ScoreKey(candidate.Provider, candidate.ID)] = credentialFingerprint(candidate.CredentialIdentity())
	}
	r.accountAliases = AccountAliases(accounts)
	return true
}

func (r *SchedulerRef) advanceAccountGenerationLocked(generation uint64) {
	if generation == r.accountGeneration {
		return
	}
	if r.refreshing {
		r.legacyFinishInvalidated = true
	}
	r.accountGeneration = generation
	r.refreshing = false
	r.refreshInvalidated = false
	r.updatedAt = time.Time{}
	r.bumpScoreRevisionLocked()
}

// SetForAccountGeneration publishes a scheduler only when it was computed
// from the current account snapshot. The comparison and write share one lock,
// so a concurrent account reload cannot slip between them.
func (r *SchedulerRef) SetForAccountGeneration(scheduler Scheduler, generation uint64) bool {
	return r.SetForAccountGenerationAtScoreRevision(scheduler, generation, r.ScoreRevision())
}

// ScoreRevision identifies the measured score snapshot independently of the
// account generation. Callers capture it before slow score work and present it
// when publishing so a newer same-generation measurement cannot be replaced.
//
// The first read at a revision also records where work computed at it began
// in mark order (revisionMarkSeq), for scores that carry no MeasuredAt.
func (r *SchedulerRef) ScoreRevision() uint64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.revisionMarkSeqSet {
		r.revisionMarkSeq = r.markSeq
		r.revisionMarkSeqSet = true
	}
	return r.scoreRevision
}

// bumpScoreRevisionLocked starts a new score revision; the next
// ScoreRevision read marks where work computed at it began.
func (r *SchedulerRef) bumpScoreRevisionLocked() {
	r.scoreRevision++
	r.revisionMarkSeqSet = false
}

// revisionSinceSeqLocked is the mark number a Set/Merge at the current
// revision measured after: marks numbered above it are newer than its scores.
// A publish whose revision was never read (a plain Set) began now.
func (r *SchedulerRef) revisionSinceSeqLocked() uint64 {
	if r.revisionMarkSeqSet {
		return r.revisionMarkSeq
	}
	return r.markSeq
}

func (r *SchedulerRef) SetForAccountGenerationAtScoreRevision(
	scheduler Scheduler,
	generation uint64,
	scoreRevision uint64,
) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation != r.accountGeneration || scoreRevision != r.scoreRevision {
		return false
	}
	inflightRefresh := r.refreshing
	r.setLocked(scheduler)
	if inflightRefresh {
		r.refreshInvalidated = true
	} else {
		r.refreshing = false
		r.refreshInvalidated = false
	}
	return true
}

// PublishWithoutRefreshForAccountGenerationAtScoreRevision publishes a
// scheduler rebuilt from the CACHED usage sweep -- what every usage-status and
// pool-board read does -- without counting as the scheduled usage refresh. It
// has the same generation and score-revision guard as
// SetForAccountGenerationAtScoreRevision, and differs in exactly the two ways
// that let a reader starve the refresh:
//
//   - It leaves the refresh clock (updatedAt) alone. The live debits are
//     cleared only by FinishRefresh, which runs only after BeginRefreshIfStale
//     finds updatedAt older than the usage-score TTL, and a republication does
//     NOT clear them. Stamping updatedAt here meant any reader faster than the
//     TTL -- a peer daemon's capacity probe re-reads usage-status every 10 s
//     (remotePoolAdminTTL) while it has traffic, the pool board every 5 s --
//     kept the scheduler looking fresh indefinitely: the refresh never fired,
//     and every account this daemon routed to sank a further
//     LiveDebitPerRequest per request until it failed the new-session gate on
//     our own optimism alone. Measured scores kept updating underneath (the
//     sweep cache expires on its own clock); only the debit on top grew.
//     Observed on the box 2026-09-28 while the laptop probed it: debits held
//     for minutes, not seconds, and a measured-healthy account read as below
//     the new-session gate (ops/PATCHES.md §26).
//   - It does not cancel a refresh already in flight. Set marks an in-flight
//     refresh invalidated so its older view cannot overwrite a newer
//     measurement, but this is a republication, drawn from the same
//     FetchUsageWindowsCached cache the refresh itself reads. Invalidating on
//     it discarded the refresh -- and with it the only debit clear -- whenever
//     a read landed inside the sweep's few seconds, which a 10 s reader does
//     often and a faster one does every time. The price of letting the
//     refresh land instead: an account whose sweep row was newer than what the
//     refresh saw (a window-cache entry that turned over mid-sweep, or the
//     refresh's carried-forward seed when it could not fetch the account)
//     reads the refresh's view until the next read republishes the sweep row.
//
// This is the rule MergeScoresForAccountGeneration already follows for the
// clock (a partial provider refresh does not make the snapshot look fresh):
// only the scheduled refresh, which also supersedes the debits, may postpone
// the next one. It does not clear the debits itself on purpose: it republishes
// the same cached measurement on every read, and clearing there would erase
// real, not-yet-measured traffic every time a peer looked.
func (r *SchedulerRef) PublishWithoutRefreshForAccountGenerationAtScoreRevision(
	scheduler Scheduler,
	generation uint64,
	scoreRevision uint64,
) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation != r.accountGeneration || scoreRevision != r.scoreRevision {
		return false
	}
	r.setLockedForScoreKeys(scheduler, nil, false)
	return true
}

// MergeScoresForAccountGeneration atomically overlays measured scores onto the
// current shared scheduler when they were computed from the current account
// snapshot. Unlike SetForAccountGeneration, a partial provider refresh cannot
// replace scores published concurrently for other providers, and it does not
// cancel a full refresh already in progress for the same generation.
func (r *SchedulerRef) MergeScoresForAccountGeneration(scores []Score, generation uint64) (Scheduler, bool) {
	return r.MergeScoresForAccountGenerationAtScoreRevision(scores, generation, r.ScoreRevision())
}

func (r *SchedulerRef) MergeScoresForAccountGenerationAtScoreRevision(
	scores []Score,
	generation uint64,
	scoreRevision uint64,
) (Scheduler, bool) {
	if r == nil {
		return Scheduler{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation != r.accountGeneration || scoreRevision != r.scoreRevision {
		return Scheduler{}, false
	}
	merged := r.scheduler
	scoreKeys := make(map[string]struct{}, len(scores))
	for _, score := range scores {
		merged = merged.WithScore(score)
		scoreKeys[ScoreKey(score.Provider, score.AccountID)] = struct{}{}
	}
	// This is only a partial provider refresh, so it must not make the shared
	// full-provider snapshot look fresh and suppress its next scheduled refresh.
	r.setLockedForScoreKeys(merged, scoreKeys, false)
	if r.refreshing {
		// A full refresh that seeded before this merge is now stale for at least
		// these scores. Keep its claim only until its finish is consumed and
		// rejected; admitting a replacement sooner would give both attempts the
		// same account-generation identity and let the older one publish first.
		r.refreshInvalidated = true
	}
	now := time.Now()
	published := applyExhaustionMarks(r.scheduler, r.exhaustedUntil, now)
	published = applyExhaustionMarks(published, r.activeCredentialExhaustionLocked(), now)
	published = applyExhaustionMarks(published, r.accountUnavailableUntil, now)
	published = applyExhaustionMarks(published, r.incompatibleUntil, now)
	return published.WithLiveDebits(r.liveDebitsLocked()), true
}

// retainExhaustedExpiriesLocked reconciles mark expiries with an incoming
// refresh, by evidence class:
//   - A pool-scoped mark has no matching refreshed pool score: keep the expiry;
//     the refresh has no evidence about that pool.
//   - A matching score shows FRESH headroom: the mark is superseded; drop the
//     expiry. No score at all is not evidence of recovery (upstream #560):
//     the mark is kept until it lapses, and a mark whose account left the
//     snapshot is pruned there (pruneExhaustedMarksForAccountsLocked,
//     upstream #561).
//   - Carried-forward zero (the account's own usage fetch failed, seed dragged
//     along, Fresh=false): keep the existing expiry. Clearing it would make the
//     request-time mark permanent again, recreating the stranded-recovered-
//     account failure this exists to fix.
//   - Fresh zero (a successful fetch re-confirmed exhaustion, Fresh=true):
//     re-anchor the expiry to this newest evidence — extend it to at least the
//     fresh window's own reset (floored at the default TTL) — so an OLDER
//     request-time expiry can never lapse a freshly-observed zero back to the
//     optimistic default. Expiries only extend here, never shorten, so an
//     authoritative long reset from a rejected response still holds.
//   - A mark set after the incoming score's usage reading (MeasuredAt, or for
//     a score without one, numbered above sinceSeq) is newer than that
//     score: the score supersedes nothing, however healthy it reads. See
//     markNewerThanEvidenceLocked.
func (r *SchedulerRef) retainExhaustedExpiriesLocked(sinceSeq uint64) {
	r.retainExhaustedExpiriesForScoreKeysLocked(nil, sinceSeq)
}

func (r *SchedulerRef) retainExhaustedExpiriesForScoreKeysLocked(scoreKeys map[string]struct{}, sinceSeq uint64) {
	now := time.Now()
	for key := range r.exhaustedUntil {
		scoreKey, _, poolKey, ok := exhaustionKeyParts(key)
		if !ok {
			r.dropExhaustedMarkLocked(key)
			continue
		}
		if scoreKeys != nil {
			if _, included := scoreKeys[scoreKey]; !included {
				continue
			}
		}
		score, ok := r.scheduler.scores[scoreKey]
		// A model pool's score comes from the same usage reading as its
		// account, so it inherits the account's measurement time unless it
		// carries its own.
		measuredAt := score.MeasuredAt
		if ok && poolKey != "" {
			modelScore, modelOK := score.ModelScores[poolKey]
			if !modelOK {
				continue
			}
			score = modelScore
			if !score.MeasuredAt.IsZero() {
				measuredAt = score.MeasuredAt
			}
		}
		if r.markNewerThanEvidenceLocked(key, measuredAt, sinceSeq) {
			continue
		}
		switch {
		case !ok:
			// A publish that carries no score for the account did not
			// measure its recovery (upstream #560): keep the upstream
			// rejection until it lapses, or until the account leaves the
			// snapshot (pruneExhaustedMarksForAccountsLocked, upstream #561).
			continue
		case !score.exhausted():
			// Only FRESH headroom supersedes a mark. A carried seed showing
			// headroom is not evidence the account recovered -- it is the
			// previous snapshot dragged forward past a failed usage fetch,
			// which is precisely the state an upstream 429 contradicts. This
			// is the mirror of the carried-forward-zero rule below: neither
			// direction may be decided by a score nobody measured. A mark
			// still lapses on its own expiry, so a genuine recovery costs at
			// most DefaultExhaustedTTL of caution rather than stranding.
			if score.Fresh {
				r.dropExhaustedMarkLocked(key)
			}
		case score.Fresh:
			until := now.Add(DefaultExhaustedTTL)
			if score.ShortResetAfterSeconds > 0 {
				if fromReset := now.Add(time.Duration(score.ShortResetAfterSeconds) * time.Second); fromReset.After(until) {
					until = fromReset
				}
			}
			if cap := now.Add(8 * 24 * time.Hour); until.After(cap) {
				until = cap
			}
			if until.After(r.exhaustedUntil[key]) {
				r.exhaustedUntil[key] = until
			}
		}
	}
}

// markNewerThanEvidenceLocked reports whether the exhaustion mark at key was
// set after the usage reading behind an incoming score, which then may not
// supersede it -- neither drop it on headroom nor re-anchor it.
//
// A score that says when it was measured is ordered on that alone. The
// order upstream #398 uses -- when the publish BEGAN (sinceSeq) -- is not
// enough here, because our publishes routinely carry older readings: the
// scheduled refresh reads the 2-minute window cache, and every usage-status
// read republishes the 30 s sweep cache (PATCHES.md §26). A refresh that
// began a second after a 429, or a peer's probe seven seconds after it,
// published headroom read before the rejection and deleted a 5 h mark;
// routing went straight back into the account upstream had just refused. The
// measurement instant is when the fetch STARTED (a cache entry records the
// moment before its fetch), so a reading in flight when the 429 lands counts
// as older than it. Equal instants keep the mark.
//
// A score with no measurement time (tests, peer-built or persisted scores
// that predate the field) falls back to upstream's numbering. A mark with no
// stamp -- one the recovery probe re-derived from a fresh zero score -- is as
// supersedable as the score it came from.
func (r *SchedulerRef) markNewerThanEvidenceLocked(key string, measuredAt time.Time, sinceSeq uint64) bool {
	if !measuredAt.IsZero() {
		markedAt, stamped := r.exhaustedMarkedAt[key]
		return stamped && !measuredAt.After(markedAt)
	}
	return r.exhaustedMarkSeq[key] > sinceSeq
}

// dropExhaustedMarkLocked removes an exhaustion mark together with the
// stamps that order it against incoming scores.
func (r *SchedulerRef) dropExhaustedMarkLocked(key string) {
	delete(r.exhaustedUntil, key)
	delete(r.exhaustedMarkSeq, key)
	delete(r.exhaustedMarkedAt, key)
}

// DefaultExhaustedTTL bounds an exhaustion mark when the upstream response gave
// no reset time. Short on purpose: re-marking a still-cooked account costs one
// failed attempt, while over-holding a recovered account starves routing of
// real quota.
const DefaultExhaustedTTL = 10 * time.Minute

func (r *SchedulerRef) MarkExhausted(provider account.Provider, accountID, poolKey string) {
	r.MarkExhaustedUntil(provider, accountID, poolKey, time.Now().Add(DefaultExhaustedTTL))
}

// MarkExhaustedUntil records an exhaustion overlay until the given time, after
// which routing reads the base snapshot again. An empty poolKey marks the whole
// account; a non-empty poolKey marks only that model pool. Callers pass the
// upstream's own reset time
// (anthropic-ratelimit-unified-reset / Retry-After) when available.
func (r *SchedulerRef) MarkExhaustedUntil(provider account.Provider, accountID, poolKey string, until time.Time) {
	if accountID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.markExhaustedUntilLocked(provider, accountID, poolKey, until)
}

func (r *SchedulerRef) markExhaustedUntilLocked(provider account.Provider, accountID, poolKey string, until time.Time) {
	if r.exhaustedUntil == nil {
		r.exhaustedUntil = make(map[string]time.Time)
	}
	key := poolScopedExhaustionKey(provider, accountID, poolKey)
	r.exhaustedUntil[key] = until
	now := time.Now()
	r.markSeq++
	if r.exhaustedMarkSeq == nil {
		r.exhaustedMarkSeq = make(map[string]uint64)
	}
	r.exhaustedMarkSeq[key] = r.markSeq
	if r.exhaustedMarkedAt == nil {
		r.exhaustedMarkedAt = make(map[string]time.Time)
	}
	r.exhaustedMarkedAt[key] = now
	delete(r.recoveryProbeReady, key)
	r.updatedAt = now
}

// MarkCredentialExhaustedUntil records a terminal credential failure only if
// it was observed against the current account snapshot. Unlike quota marks,
// credential exclusions are discarded when credentials are reloaded.
func (r *SchedulerRef) MarkCredentialExhaustedUntil(
	provider account.Provider,
	accountID string,
	credentialIdentity string,
	until time.Time,
	accountGeneration uint64,
) bool {
	if r == nil || accountID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	fingerprint := credentialFingerprint(credentialIdentity)
	scoreKey := ScoreKey(provider, accountID)
	if accountGeneration != r.accountGeneration {
		return false
	}
	if r.credentialFingerprints == nil {
		r.credentialFingerprints = make(map[string]string)
	}
	if current, tracked := r.credentialFingerprints[scoreKey]; tracked && current != fingerprint {
		return false
	}
	r.credentialFingerprints[scoreKey] = fingerprint
	if r.credentialExhaustedUntil == nil {
		r.credentialExhaustedUntil = make(map[string]time.Time)
	}
	r.credentialExhaustedUntil[scoreKey+"\x00"+fingerprint] = until
	r.updatedAt = time.Now()
	return true
}

// MarkCredentialExhaustedForSnapshot atomically publishes a coherent account
// snapshot and records a terminal failure against the credential in that same
// snapshot. This closes the reload window where AccountRef has advanced but a
// separate scheduler-generation sync has not arrived yet.
func (r *SchedulerRef) MarkCredentialExhaustedForSnapshot(
	provider account.Provider,
	accountID string,
	credentialIdentity string,
	until time.Time,
	accountGeneration uint64,
	credentialRevision uint64,
	accounts []account.Account,
) bool {
	if r == nil || accountID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if accountGeneration < r.accountGeneration ||
		(accountGeneration == r.accountGeneration && credentialRevision < r.credentialRevision) {
		return false
	}
	publishesSnapshot := accountGeneration > r.accountGeneration
	if accountGeneration > r.accountGeneration {
		r.advanceAccountGenerationLocked(accountGeneration)
	}
	if credentialRevision > r.credentialRevision || r.credentialFingerprints == nil {
		publishesSnapshot = true
		r.credentialRevision = credentialRevision
		r.credentialFingerprints = make(map[string]string, len(accounts))
		for _, candidate := range accounts {
			r.credentialFingerprints[ScoreKey(candidate.Provider, candidate.ID)] = credentialFingerprint(candidate.CredentialIdentity())
		}
		r.accountAliases = AccountAliases(accounts)
	}
	if publishesSnapshot {
		r.pruneExhaustedMarksForAccountsLocked(accounts)
	}

	fingerprint := credentialFingerprint(credentialIdentity)
	scoreKey := ScoreKey(provider, accountID)
	if current, tracked := r.credentialFingerprints[scoreKey]; !tracked || current != fingerprint {
		return false
	}
	if r.credentialExhaustedUntil == nil {
		r.credentialExhaustedUntil = make(map[string]time.Time)
	}
	r.credentialExhaustedUntil[scoreKey+"\x00"+fingerprint] = until
	r.updatedAt = time.Now()
	return true
}

// MarkCredentialUnavailableForSnapshot permanently excludes the exact
// credential that received a persistent authorization or entitlement denial
// (upstream #585). A later credential update changes the fingerprint and
// restores eligibility; usage refreshes cannot supersede the denial. Like
// MarkCredentialExhaustedForSnapshot it publishes the snapshot it was
// observed against, aliases included.
func (r *SchedulerRef) MarkCredentialUnavailableForSnapshot(
	provider account.Provider,
	accountID string,
	credentialIdentity string,
	accountGeneration uint64,
	credentialRevision uint64,
	accounts []account.Account,
) bool {
	if r == nil || accountID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if accountGeneration < r.accountGeneration ||
		(accountGeneration == r.accountGeneration && credentialRevision < r.credentialRevision) {
		return false
	}
	publishesSnapshot := accountGeneration > r.accountGeneration
	if accountGeneration > r.accountGeneration {
		r.advanceAccountGenerationLocked(accountGeneration)
	}
	if credentialRevision > r.credentialRevision || r.credentialFingerprints == nil {
		publishesSnapshot = true
		r.credentialRevision = credentialRevision
		r.credentialFingerprints = make(map[string]string, len(accounts))
		for _, candidate := range accounts {
			r.credentialFingerprints[ScoreKey(candidate.Provider, candidate.ID)] = credentialFingerprint(candidate.CredentialIdentity())
		}
		r.accountAliases = AccountAliases(accounts)
	}
	if publishesSnapshot {
		r.pruneExhaustedMarksForAccountsLocked(accounts)
	}
	fingerprint := credentialFingerprint(credentialIdentity)
	scoreKey := ScoreKey(provider, accountID)
	if current, tracked := r.credentialFingerprints[scoreKey]; !tracked || current != fingerprint {
		return false
	}
	if r.credentialUnavailable == nil {
		r.credentialUnavailable = make(map[string]struct{})
	}
	r.credentialUnavailable[scoreKey+"\x00"+fingerprint] = struct{}{}
	r.updatedAt = time.Now()
	return true
}

func credentialFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (r *SchedulerRef) activeCredentialExhaustionLocked() map[string]time.Time {
	active := make(map[string]time.Time)
	for key, until := range r.credentialExhaustedUntil {
		parts := strings.SplitN(key, "\x00", 3)
		if len(parts) != 3 {
			continue
		}
		scoreKey := ScoreKey(account.Provider(parts[0]), parts[1])
		if current, tracked := r.credentialFingerprints[scoreKey]; tracked && current != parts[2] {
			continue
		}
		active[poolScopedExhaustionKey(account.Provider(parts[0]), parts[1], "")] = until
	}
	return active
}

func (r *SchedulerRef) activeCredentialUnavailableLocked() map[string]struct{} {
	active := make(map[string]struct{})
	for key := range r.credentialUnavailable {
		parts := strings.SplitN(key, "\x00", 3)
		if len(parts) != 3 {
			continue
		}
		scoreKey := ScoreKey(account.Provider(parts[0]), parts[1])
		if current, tracked := r.credentialFingerprints[scoreKey]; tracked && current != parts[2] {
			continue
		}
		active[poolScopedExhaustionKey(account.Provider(parts[0]), parts[1], "")] = struct{}{}
	}
	return active
}

func (r *SchedulerRef) persistentCredentialBlockedLocked(provider account.Provider, accountID string) bool {
	_, ok := r.activeCredentialUnavailableLocked()[poolScopedExhaustionKey(provider, accountID, "")]
	return ok
}

// MarkAccountUnavailableUntil excludes an account because of state that is
// independent of both its quota and its current credential. Usage refreshes
// and token rotation cannot supersede this evidence; only expiry can.
func (r *SchedulerRef) MarkAccountUnavailableUntil(provider account.Provider, accountID string, until time.Time) {
	if accountID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.accountUnavailableUntil == nil {
		r.accountUnavailableUntil = make(map[string]time.Time)
	}
	r.accountUnavailableUntil[poolScopedExhaustionKey(provider, accountID, "")] = until
	r.updatedAt = time.Now()
}

// MarkModelIncompatibleUntil excludes one account from one model until the
// supplied expiry. Unlike quota exhaustion, usage-score refreshes cannot clear
// this mark because they do not carry entitlement evidence.
func (r *SchedulerRef) MarkModelIncompatibleUntil(provider account.Provider, accountID, model string, until time.Time) {
	if accountID == "" || model == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.incompatibleUntil == nil {
		r.incompatibleUntil = make(map[string]time.Time)
	}
	r.incompatibleUntil[poolScopedExhaustionKey(provider, accountID, model)] = until
	r.updatedAt = time.Now()
}

func (r *SchedulerRef) MarkModelIncompatible(provider account.Provider, accountID, model string) {
	r.MarkModelIncompatibleUntil(provider, accountID, model, time.Now().Add(DefaultExhaustedTTL))
}

// ExhaustedUntilFor reports the expiry recorded for an account's exhaustion
// mark, if any. Used by tests and diagnostics to verify TTL selection.
func (r *SchedulerRef) ExhaustedUntilFor(provider account.Provider, accountID, poolKey string) (time.Time, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	key := poolScopedExhaustionKey(provider, accountID, poolKey)
	if poolKey == "" && r.persistentCredentialBlockedLocked(provider, accountID) {
		return time.Time{}, true
	}
	until, ok := r.exhaustedUntil[key]
	if credentialUntil, credentialOK := r.activeCredentialExhaustionLocked()[key]; credentialOK && (!ok || credentialUntil.After(until)) {
		until, ok = credentialUntil, true
	}
	if accountUntil, accountOK := r.accountUnavailableUntil[key]; accountOK && (!ok || accountUntil.After(until)) {
		until, ok = accountUntil, true
	}
	return until, ok
}

func (r *SchedulerRef) ModelIncompatibleUntilFor(provider account.Provider, accountID, model string) (time.Time, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	until, ok := r.incompatibleUntil[poolScopedExhaustionKey(provider, accountID, model)]
	return until, ok
}

func (r *SchedulerRef) expiryMarksLocked() map[string]time.Time {
	marks := make(map[string]time.Time, len(r.exhaustedUntil)+len(r.accountUnavailableUntil)+len(r.incompatibleUntil))
	for key, until := range r.exhaustedUntil {
		marks[key] = until
	}
	for key, until := range r.incompatibleUntil {
		if until.After(marks[key]) {
			marks[key] = until
		}
	}
	for key, until := range r.accountUnavailableUntil {
		if until.After(marks[key]) {
			marks[key] = until
		}
	}
	for key, until := range r.activeCredentialExhaustionLocked() {
		if until.After(marks[key]) {
			marks[key] = until
		}
	}
	return marks
}

// applyPersistentCredentialMarks zeroes every account whose current credential
// carries a persistent denial (upstream #585). It has no expiry: the denial
// ends only when the credential changes (activeCredentialUnavailableLocked).
func applyPersistentCredentialMarks(base Scheduler, blocked map[string]struct{}) Scheduler {
	if len(blocked) == 0 {
		return base
	}
	// Copy-then-modify, like applyExhaustionMarks: an overlay replaces scores
	// and nothing else, so it must not drop the counts, reserves, aliases or
	// machine view it was handed. (Upstream's literal keeps only the counts,
	// the debits and the capacity marks.)
	next := base
	next.scores = make(map[string]Score, len(base.scores)+len(blocked))
	for key, score := range base.scores {
		next.scores[key] = copyScore(score)
	}
	for key := range blocked {
		scoreKey, provider, _, ok := exhaustionKeyParts(key)
		if !ok {
			continue
		}
		score := next.scores[scoreKey]
		if score.AccountID == "" {
			_, accountID, _ := strings.Cut(scoreKey, "\x00")
			// No WeeklyHeadroom default: in this fork 0 means "weekly
			// unmeasured" and fails open (PATCHES.md section 10).
			score = Score{AccountID: accountID, Provider: provider, Headroom: 1, ShortHeadroom: 1}
		}
		score.Provider = provider
		score.Headroom = 0
		score.ShortHeadroom = 0
		score.ModelScores = nil
		next.scores[scoreKey] = score
	}
	return next
}

func poolScopedExhaustionKey(provider account.Provider, accountID, poolKey string) string {
	return ScoreKey(provider, accountID) + "\x00" + ModelKey(poolKey)
}

func exhaustionKeyParts(key string) (scoreKey string, provider account.Provider, poolKey string, ok bool) {
	parts := strings.SplitN(key, "\x00", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}
	provider = account.Provider(parts[0])
	return ScoreKey(provider, parts[1]), provider, parts[2], true
}

func applyExhaustionMarks(base Scheduler, exhaustedUntil map[string]time.Time, now time.Time) Scheduler {
	if len(exhaustedUntil) == 0 {
		return base
	}
	// Copy-then-modify: an overlay replaces scores and nothing else, so it must
	// not drop the counts, reserves, aliases or machine view it was handed.
	next := base
	next.scores = make(map[string]Score, len(base.scores))
	for key, score := range base.scores {
		next.scores[key] = copyScore(score)
	}
	introducedPools := make(map[string]bool)
	for key, until := range exhaustedUntil {
		if !until.After(now) {
			continue
		}
		// A pool a mark introduces is copied onto the other accounts only
		// where absence would zero-fill them. A Claude or Antigravity account
		// without the pool already routes on its account-wide score
		// (absentPoolInheritsAccountScore), so a copy would only publish a
		// pool with no limit behind it; the marked account still gets its own
		// exhausted pool below.
		_, provider, poolKey, ok := exhaustionKeyParts(key)
		if ok && poolKey != "" && !base.hasModelScore(poolKey) && !absentPoolInheritsAccountScore(provider) {
			introducedPools[poolKey] = true
		}
	}
	for poolKey := range introducedPools {
		for scoreKey, score := range next.scores {
			if _, ok := score.ModelScores[poolKey]; ok {
				continue
			}
			score.ModelScores = copyModelScores(score.ModelScores)
			if score.ModelScores == nil {
				score.ModelScores = make(map[string]Score, 1)
			}
			poolScore := score
			poolScore.ModelScores = nil
			score.ModelScores[poolKey] = poolScore
			next.scores[scoreKey] = score
		}
	}
	accountWide := make(map[string]bool)
	for key, until := range exhaustedUntil {
		if !until.After(now) {
			continue
		}
		scoreKey, provider, poolKey, ok := exhaustionKeyParts(key)
		if !ok || poolKey != "" {
			continue
		}
		score := next.scores[scoreKey]
		if score.AccountID == "" {
			_, accountID, _ := strings.Cut(scoreKey, "\x00")
			// No WeeklyHeadroom default: in this fork 0 means "weekly
			// unmeasured" and fails open (PATCHES.md section 10).
			score = Score{AccountID: accountID, Provider: provider, Headroom: 1, ShortHeadroom: 1}
		}
		score.Provider = provider
		score.Headroom = 0
		score.ShortHeadroom = 0
		score.ModelScores = nil
		next.scores[scoreKey] = score
		accountWide[scoreKey] = true
	}
	for key, until := range exhaustedUntil {
		if !until.After(now) {
			continue
		}
		scoreKey, provider, poolKey, ok := exhaustionKeyParts(key)
		if !ok || poolKey == "" || accountWide[scoreKey] {
			continue
		}
		score := next.scores[scoreKey]
		if score.AccountID == "" {
			_, accountID, _ := strings.Cut(scoreKey, "\x00")
			score = Score{AccountID: accountID, Provider: provider, Headroom: 1, ShortHeadroom: 1}
		}
		score.Provider = provider
		score.ModelScores = copyModelScores(score.ModelScores)
		if score.ModelScores == nil {
			score.ModelScores = make(map[string]Score, 1)
		}
		poolScore, exists := score.ModelScores[poolKey]
		if !exists {
			poolScore = score
			poolScore.ModelScores = nil
		}
		poolScore.AccountID = score.AccountID
		poolScore.Provider = provider
		poolScore.Headroom = 0
		poolScore.ShortHeadroom = 0
		score.ModelScores[poolKey] = poolScore
		next.scores[scoreKey] = score
	}
	return next
}

func copyScore(score Score) Score {
	score.ModelScores = copyModelScores(score.ModelScores)
	return score
}

func copyModelScores(modelScores map[string]Score) map[string]Score {
	if len(modelScores) == 0 {
		return nil
	}
	out := make(map[string]Score, len(modelScores))
	for key, score := range modelScores {
		out[key] = score
	}
	return out
}

func stripCarriedForwardExhaustionOverlays(current, base Scheduler, exhaustedUntil map[string]time.Time) Scheduler {
	return stripCarriedForwardExhaustionOverlaysForScoreKeys(current, base, exhaustedUntil, nil)
}

func stripCarriedForwardExhaustionOverlaysForScoreKeys(current, base Scheduler, exhaustedUntil map[string]time.Time, scoreKeys map[string]struct{}) Scheduler {
	if len(exhaustedUntil) == 0 {
		return current
	}
	next := current
	next.scores = make(map[string]Score, len(current.scores))
	for key, score := range current.scores {
		next.scores[key] = copyScore(score)
	}
	for key := range exhaustedUntil {
		scoreKey, _, poolKey, ok := exhaustionKeyParts(key)
		if !ok {
			continue
		}
		if scoreKeys != nil {
			if _, included := scoreKeys[scoreKey]; !included {
				continue
			}
		}
		if poolKey != "" && !base.hasModelScore(poolKey) {
			for candidateKey, candidate := range next.scores {
				if scoreKeys != nil {
					if _, included := scoreKeys[candidateKey]; !included {
						continue
					}
				}
				modelScore, modelOK := candidate.ModelScores[poolKey]
				// A freshly measured score was rebuilt from its windows, so a
				// pool it carries is a bucket the provider reported, never an
				// overlay: model sub-scores are not stamped Fresh themselves,
				// and a Claude Opus/Sonnet bucket is the one kind of pool the
				// base snapshot may legitimately lack while a mark is live.
				if !modelOK || modelScore.Fresh || candidate.Fresh {
					continue
				}
				candidate.ModelScores = copyModelScores(candidate.ModelScores)
				delete(candidate.ModelScores, poolKey)
				next.scores[candidateKey] = candidate
			}
		}
		score, ok := next.scores[scoreKey]
		if !ok {
			continue
		}
		if poolKey == "" {
			if !score.exhausted() || score.Fresh {
				continue
			}
			if baseScore, baseOK := base.scores[scoreKey]; baseOK {
				next.scores[scoreKey] = copyScore(baseScore)
			} else {
				delete(next.scores, scoreKey)
			}
			continue
		}
		modelScore, modelOK := score.ModelScores[poolKey]
		if !modelOK || !modelScore.exhausted() || modelScore.Fresh {
			continue
		}
		score.ModelScores = copyModelScores(score.ModelScores)
		if baseScore, baseOK := base.scores[scoreKey]; baseOK {
			if baseModelScore, baseModelOK := baseScore.ModelScores[poolKey]; baseModelOK {
				score.ModelScores[poolKey] = baseModelScore
			} else {
				delete(score.ModelScores, poolKey)
			}
		} else {
			delete(score.ModelScores, poolKey)
		}
		next.scores[scoreKey] = score
	}
	return next
}

func (r *SchedulerRef) Stale(ttl time.Duration) bool {
	if ttl <= 0 {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.updatedAt.IsZero() || time.Since(r.updatedAt) >= ttl
}

// Scored reports whether the ref holds scores a request may route on while a
// stale-score refresh runs elsewhere (PATCHES section 44): true once it was
// built with NewSchedulerRef, published to, or a refresh finished on it, and
// false for a zero SchedulerRef or after SetUpdatedAt(time.Time{}), the
// explicit "re-score before routing" of a redeemed rate-limit reset.
//
// It is not "updatedAt is set". An account-generation advance zeroes updatedAt
// on every account reload -- after any `sr` command that changes the account
// files, and the startup coordinator's reload -- so the next request starts a
// sweep; the scores it keeps are still the best reading of every account that
// remains, and one the reload added reads optimistic until that sweep lands,
// as every account does at daemon start (fallbackScores).
func (r *SchedulerRef) Scored() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.scored
}

// UpdatedAt reports when the scores were last stamped by a refresh (successful
// or not), a Set or a mark (upstream #378). The zero time means a refresh is
// due before the next read is fresh: never scored, an account-generation
// advance, or SetUpdatedAt(time.Time{}). Whether the ref holds scores worth
// routing on is Scored's question, not this one's (PATCHES §44.3).
func (r *SchedulerRef) UpdatedAt() time.Time {
	if r == nil {
		return time.Time{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.updatedAt
}

func (r *SchedulerRef) BeginRefreshIfStale(ttl time.Duration) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	generation := r.accountGeneration
	r.mu.RUnlock()
	return r.BeginRefreshIfStaleForAccountGeneration(ttl, generation)
}

func (r *SchedulerRef) BeginRefreshIfStaleForAccountGeneration(ttl time.Duration, generation uint64) bool {
	if ttl <= 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation != r.accountGeneration || r.refreshing || (!r.updatedAt.IsZero() && time.Since(r.updatedAt) < ttl) {
		return false
	}
	r.refreshing = true
	r.refreshInvalidated = false
	r.refreshGeneration = generation
	r.refreshMarkSeq = r.markSeq
	return true
}

func (r *SchedulerRef) FinishRefresh(scheduler Scheduler, update bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.legacyFinishInvalidated {
		r.legacyFinishInvalidated = false
		return
	}
	if r.refreshing && r.refreshInvalidated {
		r.refreshInvalidated = false
		r.refreshing = false
		return
	}
	if r.refreshing && r.refreshGeneration != r.accountGeneration {
		return
	}
	if !r.refreshing {
		// The tokenless API publishes without a Begin (upstream dropped it;
		// this fork keeps it). Such a publish began now, so it orders after
		// every mark already set, not after whatever refreshMarkSeq an older
		// refresh left behind -- which would keep marks its scores have
		// genuinely overtaken.
		r.refreshMarkSeq = r.markSeq
	}
	r.finishRefreshLocked(scheduler, update)
}

func (r *SchedulerRef) FinishRefreshForAccountGeneration(scheduler Scheduler, update bool, generation uint64) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation != r.accountGeneration || !r.refreshing || r.refreshGeneration != generation {
		return false
	}
	if r.refreshInvalidated {
		r.refreshInvalidated = false
		r.refreshing = false
		return false
	}
	r.finishRefreshLocked(scheduler, update)
	return true
}

func (r *SchedulerRef) finishRefreshLocked(scheduler Scheduler, update bool) {
	if update {
		base := r.scheduler
		r.scheduler = scheduler
		r.retainExhaustedExpiriesLocked(r.refreshMarkSeq)
		r.scheduler = stripCarriedForwardExhaustionOverlays(r.scheduler, base, r.expiryMarksLocked())
		// Fresh scores supersede the live debits accumulated against the old
		// snapshot. A failed refresh (update=false) keeps them: the snapshot
		// is still the old one, so its debits still apply.
		r.routedSinceRefresh = nil
		r.bumpScoreRevisionLocked()
		r.pace.observeScores(r.scheduler)
	}
	r.updatedAt = time.Now()
	r.scored = true
	r.refreshing = false
	r.refreshInvalidated = false
}

// NoteRouted records that one request was routed to the account, debiting its
// live score until the next successful usage refresh. It also feeds the
// provider's demand clock (LastDemand) and the cumulative routed-request count
// in PlacementStats.
func (r *SchedulerRef) NoteRouted(provider account.Provider, accountID string) {
	if r == nil || accountID == "" {
		return
	}
	r.mu.Lock()
	if r.routedSinceRefresh == nil {
		r.routedSinceRefresh = make(map[string]int)
	}
	r.routedSinceRefresh[ScoreKey(provider, accountID)]++
	r.noteDemandLocked(provider, time.Now())
	r.mu.Unlock()
	r.noteRoutedStat(provider, accountID)
}

// NoteUnserved records a request for provider that no account could take.
// It counts as demand (LastDemand) without debiting any account.
func (r *SchedulerRef) NoteUnserved(provider account.Provider) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.noteDemandLocked(provider, time.Now())
}

func (r *SchedulerRef) noteDemandLocked(provider account.Provider, now time.Time) {
	if r.lastDemand == nil {
		r.lastDemand = make(map[account.Provider]time.Time)
	}
	r.lastDemand[provider] = now
}

// LastDemand is when a request for provider was last routed (NoteRouted) or
// turned away (NoteUnserved) by this process; zero if never.
func (r *SchedulerRef) LastDemand(provider account.Provider) time.Time {
	if r == nil {
		return time.Time{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lastDemand[provider]
}

// BeginInflight records one physical upstream attempt against an account and
// returns an idempotent release function. The count spans response-body
// lifetime, so streamed requests remain visible until EOF or Close.
func (r *SchedulerRef) BeginInflight(provider account.Provider, accountID string) func() {
	if r == nil || accountID == "" {
		return func() {}
	}
	key := ScoreKey(provider, accountID)
	r.mu.Lock()
	if r.inflight == nil {
		r.inflight = make(map[string]int)
	}
	r.inflight[key]++
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.inflight[key] <= 1 {
				delete(r.inflight, key)
				return
			}
			r.inflight[key]--
		})
	}
}

// InflightCounts returns a snapshot of live physical upstream attempts by
// ScoreKey, for diagnostics (pool state on recorded upstream errors). Ranking
// does not read it: upstream's Scheduler.WithInflightCounts headroom penalty
// (#449) is not ported, because live concurrency already enters ranking as
// Score.LiveSessions (WithLiveSessionCounts) and an open attempt has spent
// nothing (Score.spreadAdjusted, PATCHES §10).
func (r *SchedulerRef) InflightCounts() map[string]int {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.inflight) == 0 {
		return nil
	}
	out := make(map[string]int, len(r.inflight))
	for key, count := range r.inflight {
		out[key] = count
	}
	return out
}

// RefundRouted takes back one NoteRouted for a request the upstream never
// served (a DNS or dial failure, a server error). The proxy's hold retries
// such a request many times (PATCHES §58), and an outage also stops the
// usage refresh that would clear the debits, so without the refund a held
// request alone would read its account as spent. Never below zero: a refresh
// in between has already cleared the debit.
func (r *SchedulerRef) RefundRouted(provider account.Provider, accountID string) {
	if r == nil || accountID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := ScoreKey(provider, accountID)
	switch count := r.routedSinceRefresh[key]; {
	case count > 1:
		r.routedSinceRefresh[key] = count - 1
	case count == 1:
		delete(r.routedSinceRefresh, key)
	}
}

// LiveDebits returns the per-account routed-request counts since the last
// successful refresh, plus placements reserved and not yet committed
// (PickAndReserve), for Scheduler.WithLiveDebits.
func (r *SchedulerRef) LiveDebits() map[string]int {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.liveDebitsLocked()
}

func (r *SchedulerRef) Touch() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updatedAt = time.Now()
	r.scored = true
}

// SetUpdatedAt dates the scores. The zero time also clears Scored, so the next
// request re-scores before it routes (see Scored).
func (r *SchedulerRef) SetUpdatedAt(updatedAt time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updatedAt = updatedAt
	r.scored = !updatedAt.IsZero()
}

// ClearExhausted drops every exhaustion mark held against one account, across
// all model pools. It is the inverse of MarkExhausted, and exists for the one
// event that makes a mark provably wrong before its expiry: the account's quota
// was restored out of band -- a redeemed rate-limit-reset credit. Letting the
// mark stand there would waste the credit, because routing would keep skipping
// the account it just paid to revive until the mark lapsed on its own schedule.
//
// It deliberately does NOT touch incompatibleUntil: a reset restores quota, and
// says nothing about whether the account is entitled to a model. Nor the
// credential or account-state exclusions (credentialExhaustedUntil,
// accountUnavailableUntil): a reset credit revives quota, not a dead token or
// an organization that disabled OAuth.
func (r *SchedulerRef) ClearExhausted(provider account.Provider, accountID string) {
	if r == nil || accountID == "" {
		return
	}
	prefix := ScoreKey(provider, accountID) + "\x00"
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.exhaustedUntil {
		if strings.HasPrefix(key, prefix) {
			r.dropExhaustedMarkLocked(key)
		}
	}
	r.updatedAt = time.Now()
}
