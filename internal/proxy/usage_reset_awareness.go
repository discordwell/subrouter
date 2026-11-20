package proxy

import (
	"context"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// knownAccountWideQuotaReset returns a provider-measured recovery deadline
// only when an account-wide quota score is exhausted and every binding
// exhausted window has a known reset. Model-specific exhaustion is not enough
// to skip an account's usage fetch: another model family may still be usable.
func knownAccountWideQuotaReset(score selectacct.Score, now time.Time) (time.Time, bool) {
	until, known := score.ExhaustionClearsAt()
	return until, known && !until.IsZero() && until.After(now)
}

// scoreWeeklyCooked is upstream's Score.WeeklyCooked (#577: the account's
// weekly window is spent) on the fork's Score, which has no
// WeeklyHeadroomKnown: its WeeklyHeadroom and WeeklyRemaining read zero both
// when unmeasured and when spent. A live WeeklyResetAt says a binding weekly
// window was measured (selectacct.scoreFromLimitWindows sets it only from
// one, and it reads as no weekly information once elapsed), and
// WeeklyRemaining is that reading without the Fable reservation or a live
// debit.
func scoreWeeklyCooked(score selectacct.Score, now time.Time) bool {
	return score.WeeklyResetAt.After(now) && score.WeeklyRemaining <= 0
}

// mergeClaudeResponseQuotaScore publishes account-wide reset headers learned
// from a successful model response. This is a partial observation, so merge it
// into the current scheduler instead of replacing other providers' scores.
func (s Server) mergeClaudeResponseQuotaScore(account accounts.Account, windows []accounts.UsageWindow, generation uint64, credentialIdentity string, orderAt time.Time) {
	if s.AccountRef == nil || s.SchedulerRef == nil || account.ID == "" {
		return
	}
	current := s.AccountRef.credentialSnapshot(accounts.ProviderClaude, account.ID)
	if current.CredentialIdentity() != credentialIdentity || current.CredentialIdentity() != account.CredentialIdentity() || s.AccountRef.Generation() != generation {
		return
	}
	accountWindows := make([]accounts.UsageWindow, 0, len(windows))
	for _, window := range windows {
		if window.Feature == "" {
			accountWindows = append(accountWindows, window)
		}
	}
	// The response may carry only one account-wide bucket. Reuse the cache
	// merged by observeClaudeQuotaHeaders so a partial response cannot drop a
	// still-valid 5h or 7d window from the replacement scheduler score.
	cacheKey := account.ID + "\x00" + string(account.Provider)
	s.AccountRef.usageWindowsMu.Lock()
	if entry, ok := s.AccountRef.usageWindows[cacheKey]; ok &&
		(entry.credentialKey == "" || entry.credentialKey == usageWindowsCredentialKey(account)) {
		if !entry.orderAt.IsZero() && entry.orderAt.After(orderAt) {
			// A newer response won the cache while this handler was waiting to
			// publish. Use its complete account view and discard this older one.
			accountWindows = append([]accounts.UsageWindow(nil), entry.windows...)
		} else {
			accountWindows = mergeUsageWindows(entry.windows, accountWindows)
		}
	}
	s.AccountRef.usageWindowsMu.Unlock()
	if len(accountWindows) == 0 {
		return
	}
	provider := schedulerAccountProvider(account.Provider)
	score := scoreFromUsageWindows(provider, account.ID, accountWindows)
	score.Fresh = true
	// The headers were measured when Anthropic admitted the request, which
	// its start bounds (PATCHES §29.3b): a 429 marked after that start --
	// while this request was in flight -- is newer evidence and stays. With
	// no MeasuredAt the merge ordered by its own revision, read inside the
	// publish, and so dropped any mark set before the headers arrived.
	score.MeasuredAt = orderAt
	// The merged score replaces the account's whole score; the headers say
	// nothing about the saved reset, so the §56 hold carries over as every
	// other score builder carries it (resetHeldUntil with no reading).
	if seed, known := s.SchedulerRef.Get().LookupScore(provider, account.ID); known {
		score.ResetHeldUntil = resetHeldUntil(nil, seed, time.Now())
	}
	if _, ok := s.SchedulerRef.MergeScoresForAccountGeneration([]selectacct.Score{score}, generation); !ok {
		return
	}
	// The status rows follow, without persisting: this runs in ModifyResponse,
	// before the reply's headers reach the client.
	s.AccountRef.refreshUsageStatusRowsFromWindows(false, score)
}

// allOAuthAccountsWaitingForReset is true only when every locally managed
// OAuth account is measured exhausted, with a future authoritative reset.
// A missing/unknown score or any healthy account keeps the normal background
// cadence; do not guess a global recovery deadline from partial telemetry.
func (s Server) allOAuthAccountsWaitingForReset(now time.Time) (time.Time, bool) {
	if s.AccountRef == nil || s.SchedulerRef == nil {
		// Account-import/re-login advances the account generation and
		// invalidates the measured score timestamp. Do not apply an old
		// account's deadline to its newly installed credential.
		return time.Time{}, false
	}
	// Refresh the local account view before deciding that an old quota clock
	// still applies. This only reads the published account-generation marker;
	// it never contacts a provider. A replacement login advances the
	// generation and clears UpdatedAt, so the next score pass can verify it.
	allAccounts, _ := s.accountListSnapshotContext(context.Background())
	if s.SchedulerRef.UpdatedAt().IsZero() {
		return time.Time{}, false
	}
	// accountListSnapshotContext includes the server's startup list for
	// compatibility and appends the AccountRef's reloaded list. Prefer the
	// latter when a generation changed so a repaired credential does not get
	// shadowed by its pre-login copy.
	loadedByKey := make(map[string]accounts.Account, len(allAccounts))
	for _, account := range allAccounts {
		if account.AuthMode != accounts.AuthModeOAuth {
			continue
		}
		key := string(accountProviderOrCodex(account)) + "\x00" + account.ID
		loadedByKey[key] = account
	}
	loaded := make([]accounts.Account, 0, len(loadedByKey))
	for _, account := range loadedByKey {
		loaded = append(loaded, account)
	}
	if len(loaded) == 0 {
		return time.Time{}, false
	}
	current := s.SchedulerRef.RefreshSeed()
	var earliest time.Time
	for _, account := range loaded {
		if account.AuthMode != accounts.AuthModeOAuth {
			continue
		}
		score := current.ScoreFor(schedulerAccountProvider(account.Provider), account.ID)
		if account.Provider == accounts.ProviderClaude && scoreWeeklyCooked(score, now) &&
			s.AccountRef.claudeUsageThrottleActive(account, now) {
			// A weekly-cooked score paired with a throttled telemetry endpoint is
			// stale evidence. Let the request path run one cheap Claude Code-shaped
			// header probe before treating the whole pool as asleep until reset.
			return time.Time{}, false
		}
		until, known := knownAccountWideQuotaReset(score, now)
		if !known {
			return time.Time{}, false
		}
		if earliest.IsZero() || until.Before(earliest) {
			earliest = until
		}
	}
	return earliest, !earliest.IsZero()
}
