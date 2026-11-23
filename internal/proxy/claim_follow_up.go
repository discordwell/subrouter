package proxy

import (
	"context"
	"errors"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

// PATCHES §102.2: when the claim path's own read after a landed claim did not
// read the account's saved resets, the daemon reads the account again at the
// first moment its own rules and the provider's deadline allow. Never while
// the provider holds the account's usage reads off, since a read then can
// only extend the hold. Through the read the sweep makes, so a sweep reading
// the account at the same moment shares it. Until a reading of the block
// arrives by any path, the daemon drains, or two hours pass.

const (
	// claimFollowUpRetry is the wait after a read that failed with no
	// deadline from the provider (a network error). It doubles with each
	// such failure in a row, up to claimFollowUpMaxWait.
	claimFollowUpRetry   = 20 * time.Second
	claimFollowUpMaxWait = 10 * time.Minute
	// claimFollowUpGiveUp is how long after the claim the reads go on.
	claimFollowUpGiveUp = 2 * time.Hour
	// claimFollowUpTimeout bounds one follow-up read.
	claimFollowUpTimeout = 30 * time.Second
	// claudeResetReadHold is the hold a refused reset read sets when Anthropic
	// names no Retry-After, as a refused usage read's does
	// (agentclaude.usageThrottleFallbackWait).
	claudeResetReadHold = 5 * time.Minute
)

const (
	firstReadingAfterClaimMessage = "saved resets: the first reading after a claim was taken"
	noReadingAfterClaimMessage    = "saved resets: no reading of the account's saved resets in the two hours since the claim; the daemon stops reading it for the claim"
)

var (
	errClaudeResetReadInFlight = errors.New("a read of the saved resets is already in flight")
	// errNoBlockInAnswer is a read answered without a block the daemon can
	// take: Anthropic's "unavailable", or a usage answer with none while the
	// daemon's own reset read (§46.1) is off. The next read backs off.
	errNoBlockInAnswer = errors.New("the answer carried no saved-reset block to take")
)

// blockReadPath names the read that took the first reading after a claim:
// the Info line's read_by.
type blockReadPath string

const (
	// readByClaim is the claim path's own read after the claim.
	readByClaim blockReadPath = "claim"
	// readByFollowUp is the follow-up of §102.2.
	readByFollowUp blockReadPath = "follow_up"
	// readBySweep is a usage read of the sweeps or the score refresh.
	readBySweep blockReadPath = "sweep"
	// readByFallback is the sweep's hourly read of the block with Claude
	// Code's own query (§46.1).
	readByFallback blockReadPath = "fallback"
	// readByClaimRoute is the claim route's status read: a later claim, or a
	// dry run.
	readByClaimRoute blockReadPath = "claim_route"
)

type blockReadPathKey struct{}

func withBlockReadPath(ctx context.Context, path blockReadPath) context.Context {
	return context.WithValue(ctx, blockReadPathKey{}, path)
}

// blockReadPathOf is the read ctx belongs to. A read with no mark is a
// sweep's.
func blockReadPathOf(ctx context.Context) blockReadPath {
	if path, ok := ctx.Value(blockReadPathKey{}).(blockReadPath); ok {
		return path
	}
	if after, _ := ctx.Value(readAfterClaimKey{}).(bool); after {
		return readByClaim
	}
	return readBySweep
}

// claimFollowUp is the reading that follows a claim whose own read did not
// read the account's saved resets.
type claimFollowUp struct {
	// due is the earliest the next read may be sent.
	due time.Time
	// failures counts reads in a row that failed with no deadline from the
	// provider.
	failures int
	// blockless is a usage read after the claim that came back without the
	// block: the next read is Claude Code's own reset read, as the sweep's
	// fallback is (§46.1).
	blockless bool
	running   bool
	// tries counts the reads tried, and err is the last one's error.
	tries int
	err   error
}

// next sets when the read after one that ended with err may go: at the
// provider's deadline while it holds the account off (hold); at the daemon's
// own deadline after a refusal that recorded none (usageWindowsThrottleUntil);
// after the backoff when the read failed otherwise; else at once.
func (f *claimFollowUp) next(err error, hold, now time.Time) {
	f.err = err
	switch {
	case hold.After(now):
		f.due, f.failures = hold, 0
	case usageWindowsIsThrottle(err):
		f.due, f.failures = usageWindowsThrottleUntil(err, now), 0
	case err != nil:
		wait := claimFollowUpRetry
		for i := 0; i < f.failures && wait < claimFollowUpMaxWait; i++ {
			wait *= 2
		}
		f.due, f.failures = now.Add(min(wait, claimFollowUpMaxWait)), f.failures+1
	default:
		f.due, f.failures = now, 0
	}
}

// usageHoldUntil is when the provider's hold-off on account's usage reads
// ends, as a refused read recorded it; zero with none.
func (r *AccountRef) usageHoldUntil(account accounts.Account) time.Time {
	key := usageWindowsFailureKey(account.ID + "\x00" + string(account.Provider))
	r.usageWindowsMu.Lock()
	defer r.usageWindowsMu.Unlock()
	return r.usageWindowsFailures[key].retryAt
}

// holdUsageReads records a refusal that another read of account's usage
// endpoint met, so no usage read of the account goes before its deadline.
func (r *AccountRef) holdUsageReads(account accounts.Account, refusal *agentclaude.UsageThrottleError) {
	cacheKey := account.ID + "\x00" + string(account.Provider)
	failureKey := usageWindowsFailureKey(cacheKey)
	r.usageWindowsMu.Lock()
	if current, ok := r.usageWindowsFailures[failureKey]; ok && !current.retryAt.Before(refusal.RetryAt) {
		r.usageWindowsMu.Unlock()
		return
	}
	if r.usageWindowsFailures == nil {
		r.usageWindowsFailures = map[string]usageWindowsFailure{}
	}
	failure := usageWindowsFailure{err: refusal, at: time.Now(), retryAt: refusal.RetryAt, credentialKey: usageWindowsCredentialKey(account)}
	r.usageWindowsFailures[failureKey] = failure
	r.usageWindowsMu.Unlock()
	r.persistUsageThrottleState(cacheKey, failure)
}

// armClaimFollowUp starts the follow-up of account's last landed claim when
// the claim path's own read after it (read) left the block from the claim.
func (s Server) armClaimFollowUp(account accounts.Account, read resetReadAfter) {
	ref := s.AccountRef
	if ref == nil {
		return
	}
	provider := accountProviderOrCodex(account)
	hold := ref.usageHoldUntil(account)
	// A Claude read that was taken and still left the block unread carried
	// none: Claude Code's own query reads it, as the sweep's fallback does.
	taken := read.err == nil && read.servedOver == nil && !read.measuredAt.Before(read.asked)
	blockless, err := ref.noBlockInAnswer(provider == accounts.ProviderClaude && taken, read.err)
	memory := ref.sharedClaimedResets()
	now := memory.clock()
	memory.mu.Lock()
	defer memory.mu.Unlock()
	claim, ok := memory.byKey[claimedResetKey(provider, account.ID)]
	if !memory.runs || !ok || claim.block == nil || claim.followUp != nil {
		return
	}
	follow := &claimFollowUp{blockless: blockless}
	follow.next(err, hold, now)
	claim.followUp = follow
}

// noBlockInAnswer is what a usage read that came back without the block
// leaves the follow-up: Claude Code's own reset read next (blockless), or,
// with the daemon's own reset read off, a failed read.
func (r *AccountRef) noBlockInAnswer(blockless bool, err error) (bool, error) {
	if !blockless || r.claudeResetFallbackOn() {
		return blockless, err
	}
	return false, errNoBlockInAnswer
}

// claudeResetFallbackOn reports the daemon's own reset read (§46.1) on.
func (r *AccountRef) claudeResetFallbackOn() bool {
	r.claudeResetMu.Lock()
	defer r.claudeResetMu.Unlock()
	return r.claudeResetFallbackEvery > 0
}

func (c *claimedResets) setRuns(runs bool) {
	c.mu.Lock()
	c.runs = runs
	c.mu.Unlock()
}

// claimFollowUpArmed reports an account whose follow-up reads its block.
func (r *AccountRef) claimFollowUpArmed(provider accounts.Provider, id string) bool {
	memory := r.sharedClaimedResets()
	memory.mu.Lock()
	defer memory.mu.Unlock()
	claim, ok := memory.byKey[claimedResetKey(provider, id)]
	return ok && claim.followUp != nil && claim.block != nil
}

// RunClaimFollowUps runs the follow-up pass every interval until ctx ends.
func (s Server) RunClaimFollowUps(ctx context.Context, every time.Duration) {
	if s.AccountRef == nil || every <= 0 {
		return
	}
	s.AccountRef.sharedClaimedResets().setRuns(true)
	defer s.AccountRef.sharedClaimedResets().setRuns(false)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		s.claimFollowUpPass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// claimFollowUpPass starts the read of each account whose follow-up is due,
// unless the provider holds its usage reads off. It ends a follow-up whose
// block a reading replaced, every follow-up once the daemon drains, and one
// two hours after its claim, with one Warn.
func (s Server) claimFollowUpPass(ctx context.Context) {
	ref := s.AccountRef
	if ref == nil || s.CredentialBroker != nil || ctx.Err() != nil {
		return
	}
	memory := ref.sharedClaimedResets()
	now := memory.clock()
	draining := s.Lifecycle.Draining()
	var due []*claimedReset
	var gaveUp []claimedReset
	memory.mu.Lock()
	for _, claim := range memory.byKey {
		follow := claim.followUp
		switch {
		case follow == nil || follow.running:
		case claim.block == nil || draining:
			claim.followUp = nil
		case now.Sub(claim.at) >= claimFollowUpGiveUp:
			gaveUp = append(gaveUp, *claim)
			claim.followUp = nil
		case !now.Before(follow.due):
			follow.running = true
			due = append(due, claim)
		}
	}
	memory.mu.Unlock()
	for _, claim := range gaveUp {
		if claim.logger == nil {
			continue
		}
		fields := []any{"account", claim.label, "provider", string(claim.provider), "kind", claim.kind,
			"claimed_at", claim.at.UTC().Format(time.RFC3339), "tries", claim.followUp.tries}
		if claim.followUp.err != nil {
			fields = append(fields, "last_error", claim.followUp.err.Error())
		}
		claim.logger.Warn(noReadingAfterClaimMessage, fields...)
	}
	for _, claim := range due {
		account, own := s.autoResetOwnAccount(claim.provider, claim.id)
		hold := ref.usageHoldUntil(account)
		started := own && !hold.After(now) && s.Lifecycle.StartBackground(func() { s.followUpClaim(ctx, account, claim) })
		if started {
			continue
		}
		memory.mu.Lock()
		if follow := claim.followUp; follow != nil {
			follow.running = false
			if own && hold.After(now) {
				follow.due = hold
			} else {
				claim.followUp = nil
			}
		}
		memory.mu.Unlock()
	}
}

// followUpClaim is one follow-up read of account for claim: the usage read
// the sweep makes, shared with a sweep's read of the account at the same
// moment, and, once the usage endpoint answered without the block, Claude
// Code's own reset read.
func (s Server) followUpClaim(ctx context.Context, account accounts.Account, claim *claimedReset) {
	ref := s.AccountRef
	memory := ref.sharedClaimedResets()
	unread := func() bool {
		memory.mu.Lock()
		defer memory.mu.Unlock()
		return claim.block != nil
	}
	memory.mu.Lock()
	follow := claim.followUp
	memory.mu.Unlock()
	if follow == nil {
		return
	}
	readCtx, cancel := context.WithTimeout(withBlockReadPath(context.WithoutCancel(ctx), readByFollowUp), claimFollowUpTimeout)
	defer cancel()
	held := func() bool { return ref.usageHoldUntil(account).After(time.Now()) }
	blockless, tries := follow.blockless, 0
	var err error
	// A sweep may have read the block since the pass.
	if !blockless && unread() {
		tries++
		var fetchedAt time.Time
		_, _, fetchedAt, err = ref.fetchUsageWindowsShared(readCtx, ref.client, account, account.ID+"\x00"+string(account.Provider))
		blockless, err = ref.noBlockInAnswer(err == nil && account.Provider == accounts.ProviderClaude && fetchedAt.After(claim.at) && !held() && unread(), err)
	}
	if blockless && unread() && !held() {
		tries++
		err = ref.readClaudeResetNow(readCtx, account)
	}
	hold := ref.usageHoldUntil(account)
	now := memory.clock()
	memory.mu.Lock()
	follow.running = false
	follow.tries += tries
	follow.blockless = blockless
	taken := claim.block == nil
	read := claim.read
	if taken {
		if claim.followUp == follow {
			claim.followUp = nil
		}
	} else {
		if err == nil && tries > 0 {
			err = errNoBlockInAnswer
		}
		follow.next(err, hold, now)
	}
	memory.mu.Unlock()
	if !taken {
		return
	}
	// The next usage-status read sweeps, and shows the reading.
	ref.InvalidateUsageStatusCache()
	if accountProviderOrCodex(account) == accounts.ProviderCodex {
		s.readCodexCreditsNow(ctx, account, read)
	}
}

// readClaudeResetNow reads account's block with Claude Code's own query now,
// unless such a read is in flight. A refusal holds the account's usage reads
// off until Anthropic's Retry-After, as a refused usage read does: both read
// the same endpoint for the same login.
func (r *AccountRef) readClaudeResetNow(ctx context.Context, account accounts.Account) error {
	r.claudeResetMu.Lock()
	if r.claudeResetStatuses == nil {
		r.claudeResetStatuses = map[string]claudeResetStatusEntry{}
	}
	entry := r.claudeResetStatuses[account.ID]
	if entry.inflight {
		r.claudeResetMu.Unlock()
		return errClaudeResetReadInFlight
	}
	entry.inflight = true
	r.claudeResetStatuses[account.ID] = entry
	r.claudeResetMu.Unlock()
	err := r.fetchClaudeResetFallback(ctx, account)
	var refused *agentclaude.ResetHTTPError
	if !errors.As(err, &refused) || !refused.RateLimited() {
		return err
	}
	wait := refused.RetryAfter
	if wait <= 0 {
		wait = claudeResetReadHold
	}
	throttle := &agentclaude.UsageThrottleError{Status: refused.Status, RetryAt: time.Now().Add(wait)}
	r.holdUsageReads(account, throttle)
	return throttle
}

// readCodexCreditsNow reads account's credit list at once after a reading of
// its count, with §91.1's reader, not at its hourly cadence.
func (s Server) readCodexCreditsNow(ctx context.Context, account accounts.Account, read *accounts.ComplimentaryResetInfo) {
	if !ReadCodexResetExpiry() || read == nil || read.Remaining == nil || *read.Remaining <= 0 || ctx.Err() != nil {
		return
	}
	memory := s.AccountRef.sharedHeldResets()
	memory.mu.Lock()
	defer memory.mu.Unlock()
	entry := memory.codex[account.ID]
	if entry != nil && entry.inflight {
		return
	}
	if entry == nil {
		entry = &codexResetExpiry{}
		memory.codex[account.ID] = entry
	}
	entry.triedAt, entry.count, entry.inflight = memory.clock(), *read.Remaining, true
	s.startCodexResetExpiryRead(ctx, memory, entry, account)
}
