package proxy

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// PATCHES §102.1: when the provider says a claim used a saved reset, the
// daemon knows that without any reading. From that moment the account's
// block is the last block read with the claimed reset counted as used, until
// a reading of the block replaces it; the hold that reset gave the account
// ends; and no figure measured before the claim stands for a meter the claim
// gave back.

// claimedReset is what one landed claim told the daemon about its account.
type claimedReset struct {
	// at is when the provider's answer said it used the reset. A read sent
	// after it is a reading after the claim.
	at       time.Time
	provider accounts.Provider
	id       string
	// kind is the kind of reset used: Claude's "week" or "fable", or a Codex
	// credit's reset_type.
	kind   string
	label  string
	logger *slog.Logger
	// block is the last block read before the claim with the claimed reset
	// counted as used; nil once a reading of the block replaced it.
	block *accounts.ComplimentaryResetInfo
	// read is the reading that replaced block.
	read *accounts.ComplimentaryResetInfo
	// meters are the meters the claim gave back.
	meters []claimedMeter
	// followUp reads the account again when the claim path's own read did
	// not read the block (§102.2); nil otherwise and once it ends.
	followUp *claimFollowUp
}

// claimedMeter is one meter a claim gave back, as the usage windows name it.
type claimedMeter struct{ name, feature string }

func (m claimedMeter) is(window accounts.UsageWindow) bool {
	return window.ExtraUsage == nil && window.Name == m.name && window.Feature == m.feature
}

func (c *claimedReset) gaveBack(window accounts.UsageWindow) bool {
	return slices.ContainsFunc(c.meters, func(m claimedMeter) bool { return m.is(window) })
}

// withoutMeters is windows without the meters c gave back.
func (c *claimedReset) withoutMeters(windows []accounts.UsageWindow) []accounts.UsageWindow {
	out := make([]accounts.UsageWindow, 0, len(windows))
	for _, window := range windows {
		if !c.gaveBack(window) {
			out = append(out, window)
		}
	}
	return out
}

// claudeClearedMeters maps what a Claude grant clears to the window that
// meters it.
var claudeClearedMeters = map[string]claimedMeter{
	"five_hour":                  {"5h", ""},
	"seven_day":                  {"7d", ""},
	"seven_day_overage_included": {agentclaude.FableWindowName, agentclaude.FableFeature},
}

// claudeGivenBack is the meters a Claude claim gave back: what Anthropic
// says it cleared, else what the grant clears, else (October's shape, which
// names nothing) the week.
func claudeGivenBack(cleared, clears []string) []claimedMeter {
	names := cleared
	if len(names) == 0 {
		names = clears
	}
	if len(names) == 0 {
		names = []string{"seven_day"}
	}
	var meters []claimedMeter
	for _, name := range names {
		if meter, ok := claudeClearedMeters[name]; ok && !slices.Contains(meters, meter) {
			meters = append(meters, meter)
		}
	}
	return meters
}

// codexGivenBack is the meters a Codex redeem gives back: the account's
// week, as the reading before it named it.
func codexGivenBack(before []accounts.UsageWindow) []claimedMeter {
	var meters []claimedMeter
	for _, window := range before {
		if _, week := autoResetReadingsIn(accounts.ProviderCodex, []accounts.UsageWindow{window}, time.Time{})[autoResetWeek]; week {
			meter := claimedMeter{window.Name, window.Feature}
			if !slices.Contains(meters, meter) {
				meters = append(meters, meter)
			}
		}
	}
	return meters
}

// claudeBlockAfterClaim is status with one reset of grantID counted as used.
func claudeBlockAfterClaim(status *agentclaude.ResetStatus, grantID string, now time.Time) *accounts.ComplimentaryResetInfo {
	if status == nil {
		return nil
	}
	after := *status
	after.Grants = slices.Clone(status.Grants)
	for i := range after.Grants {
		if after.Grants[i].ID == grantID && after.Grants[i].ResetsLeft > 0 {
			after.Grants[i].ResetsLeft--
			break
		}
	}
	return agentclaude.ResetInfo(&after, now)
}

// codexBlockAfterRedeem is a Codex reading's credits with the one redeemed
// counted as used.
func codexBlockAfterRedeem(before *accounts.ComplimentaryResetInfo) *accounts.ComplimentaryResetInfo {
	if before == nil {
		return nil
	}
	after := cloneResetInfo(before)
	if after.Remaining != nil {
		left := max(*after.Remaining-1, 0)
		after.Remaining = &left
	}
	if after.Used != nil {
		used := *after.Used + 1
		after.Used = &used
	}
	after.Available = after.Remaining != nil && *after.Remaining > 0
	after.Consumed = !after.Available
	return after
}

type claimedResets struct {
	mu    sync.Mutex
	byKey map[string]*claimedReset
	// now nil is time.Now: the clock the follow-up reads are timed by.
	now func() time.Time
	// runs is set while RunClaimFollowUps runs: a daemon that runs no
	// follow-up pass arms none, and its sweep keeps its own reset read.
	runs bool
}

func (c *claimedResets) clock() time.Time {
	c.mu.Lock()
	now := c.now
	c.mu.Unlock()
	if now != nil {
		return now()
	}
	return time.Now()
}

func (r *AccountRef) sharedClaimedResets() *claimedResets {
	if r == nil {
		return &claimedResets{byKey: map[string]*claimedReset{}}
	}
	if memory := r.claimedResetMemory.Load(); memory != nil {
		return memory
	}
	r.claimedResetMemory.CompareAndSwap(nil, &claimedResets{byKey: map[string]*claimedReset{}})
	return r.claimedResetMemory.Load()
}

func claimedResetKey(provider accounts.Provider, id string) string {
	return selectacct.ScoreKey(schedulerAccountProvider(provider), id)
}

// claimedResetFor is a copy of the last landed claim on the account that a
// reading has not wholly replaced yet.
func (r *AccountRef) claimedResetFor(provider accounts.Provider, id string) (claimedReset, bool) {
	if r == nil {
		return claimedReset{}, false
	}
	memory := r.sharedClaimedResets()
	memory.mu.Lock()
	defer memory.mu.Unlock()
	claim, ok := memory.byKey[claimedResetKey(provider, id)]
	if !ok {
		return claimedReset{}, false
	}
	return *claim, true
}

// resetClaimed is one landed claim, as the claim functions saw it.
type resetClaimed struct {
	kind string
	// block is the block the claim went by, with the reset counted as used.
	block *accounts.ComplimentaryResetInfo
	// before are the meters the last reading before the claim gave, read at
	// beforeAt.
	before   []accounts.UsageWindow
	beforeAt time.Time
	meters   []claimedMeter
}

// noteResetClaimed records that the provider used a reset on account, and
// makes the daemon say only what it knows from then on: no figure measured
// before the claim stands for a meter the claim gave back, in the usage
// cache, in the Fable cache or in the account's score, and the account's hold
// follows the claim's block. It adds no usage read: the next read of the
// account is the one it would make anyway.
func (s Server) noteResetClaimed(account accounts.Account, used resetClaimed) {
	ref := s.AccountRef
	if ref == nil {
		return
	}
	provider := accountProviderOrCodex(account)
	claim := &claimedReset{at: time.Now(), provider: provider, id: account.ID, kind: used.kind, label: account.ID,
		logger: s.Logger, block: used.block, meters: used.meters}
	if strings.TrimSpace(account.Label) != "" {
		claim.label = account.Label
	}
	memory := ref.sharedClaimedResets()
	memory.mu.Lock()
	memory.byKey[claimedResetKey(provider, account.ID)] = claim
	memory.mu.Unlock()

	ref.forgetUsageWindows(account)
	ref.forgetFableBeforeClaim(account, claim)
	if s.SchedulerRef == nil {
		return
	}
	score := scoreFromUsageWindows(schedulerAccountProvider(provider), account.ID, claim.withoutMeters(used.before))
	score.MeasuredAt = used.beforeAt
	score.ResetHeldUntil = resetHeldUntil(used.block, selectacct.Score{}, claim.at)
	s.SchedulerRef.MergeScoresForAccountGeneration([]selectacct.Score{score}, ref.Generation())
}

// forgetFableBeforeClaim drops the meters claim gave back from the Fable
// cache, which a usage answer that leaves the Fable meter out is merged with
// (§99's independent check). The figures a throttled answer carried before
// the claim stay with the provider's hold-off: a read after the claim that
// is served them judges them as read before it (§99), and a row measured
// before the claim shows none of them (withResetsFromClaims).
func (r *AccountRef) forgetFableBeforeClaim(account accounts.Account, claim *claimedReset) {
	key := claudeSupplementalCacheKey(account)
	r.usageWindowsMu.Lock()
	defer r.usageWindowsMu.Unlock()
	cached, ok := r.claudeSupplemental[key]
	if !ok {
		return
	}
	if kept := claim.withoutMeters(cached.windows); len(kept) > 0 {
		cached.windows = kept
		r.claudeSupplemental[key] = cached
		return
	}
	delete(r.claudeSupplemental, key)
}

// noteResetBlockRead takes a block read at readAt by the read by for the
// account: a known block read after the account's last landed claim replaces
// the block the claim left, and one Info line says which read took it and how
// long after the claim (§102.2). A reading that still lists the reset the
// claim used wins, and one Warn says so.
func (r *AccountRef) noteResetBlockRead(provider accounts.Provider, id string, read *accounts.ComplimentaryResetInfo, readAt time.Time, by blockReadPath) {
	if r == nil || read == nil || !read.Known {
		return
	}
	memory := r.sharedClaimedResets()
	memory.mu.Lock()
	claim, ok := memory.byKey[claimedResetKey(provider, id)]
	if !ok || claim.block == nil || !readAt.After(claim.at) {
		memory.mu.Unlock()
		return
	}
	expected := claim.block
	claim.block, claim.read = nil, cloneResetInfo(read)
	if len(claim.meters) == 0 {
		delete(memory.byKey, claimedResetKey(provider, id))
	}
	memory.mu.Unlock()
	if claim.logger == nil {
		return
	}
	fields := []any{"account", claim.label, "provider", string(provider), "kind", claim.kind,
		"claimed_at", claim.at.UTC().Format(time.RFC3339), "read_at", readAt.UTC().Format(time.RFC3339)}
	claim.logger.Info(firstReadingAfterClaimMessage, append(fields,
		"after_seconds", int64(readAt.Sub(claim.at)/time.Second), "read_by", string(by))...)
	if claimedLeft(read, claim.kind) <= claimedLeft(expected, claim.kind) {
		return
	}
	claim.logger.Warn("saved resets: the first reading after a claim still lists the reset the claim used; the reading stands",
		append(fields, "left_after_claim", claimedLeft(expected, claim.kind), "left_read", claimedLeft(read, claim.kind))...)
}

// claimedLeft is how many resets of kind block holds: by kind where the
// block splits its count (Claude), else its count (Codex).
func claimedLeft(block *accounts.ComplimentaryResetInfo, kind string) int {
	if block.RemainingByKind != nil || block.Source == agentclaude.ResetProgram {
		return block.RemainingByKind[kind]
	}
	if block.Remaining == nil {
		return 0
	}
	return *block.Remaining
}

// blockOn is the block c sets on row, an account's usage row (nil when the
// account has none), and whether it is the block from the claim: that block
// until a reading replaces it, then, for Codex, the reading on a row that no
// read since the claim measured. A Codex row's count comes only from the
// sweep's own read, so such a row (the snapshot a failover redeem leaves, a
// refused sweep's) carries a count from before the claim, while the redeem's
// own read after the consume holds the count since (§102.2, part I). A Claude
// claim's read reaches the row through the usage cache the sweep serves. Nil
// when the row's own block stands.
func (c *claimedReset) blockOn(row *AccountUsageStatus) (*accounts.ComplimentaryResetInfo, bool) {
	if c.block != nil {
		return c.block, true
	}
	if c.read == nil || c.provider != accounts.ProviderCodex || (row != nil && row.UsageMeasuredAt.After(c.at)) {
		return nil, false
	}
	return c.read, false
}

// withResetsFromClaims puts on each row of an account whose last landed
// claim no reading has replaced yet the block that claim left, marked
// (resets_from_claim), and on a Codex row read before the claim the reading
// that replaced it, unmarked (blockOn). It names each meter the claim gave
// back that no figure measured after it shows yet (not_read_since_claim). A
// row read before the claim shows no figure for those meters. Once a reading
// has replaced the block and every meter has a figure again, the claim is
// forgotten. The input may be the shared snapshot: rows are changed in the
// copy the caller made.
func (s Server) withResetsFromClaims(rows []AccountUsageStatus) {
	memory := s.AccountRef.sharedClaimedResets()
	memory.mu.Lock()
	defer memory.mu.Unlock()
	if len(memory.byKey) == 0 {
		return
	}
	for i := range rows {
		row := &rows[i]
		key := claimedResetKey(accountProviderFor(row.Provider), row.ID)
		claim, ok := memory.byKey[key]
		if !ok || row.AuthMode != accounts.AuthModeOAuth {
			continue
		}
		if block, fromClaim := claim.blockOn(row); block != nil {
			row.ComplimentaryReset = cloneResetInfo(block)
			row.ComplimentaryReset.Held = slices.Clone(block.Held)
			row.ResetsFromClaim = fromClaim
		}
		if !row.UsageMeasuredAt.IsZero() && row.UsageMeasuredAt.Before(claim.at) {
			row.Windows = claim.withoutMeters(row.Windows)
		}
		row.NotReadSinceClaim = nil
		for _, meter := range claim.meters {
			if !slices.ContainsFunc(row.Windows, meter.is) {
				row.NotReadSinceClaim = append(row.NotReadSinceClaim, meter.name)
			}
		}
		if claim.block == nil && len(row.NotReadSinceClaim) == 0 {
			delete(memory.byKey, key)
		}
	}
}

// readAfterClaimKey marks the claim path's own usage read after a landed
// claim (afterClaudeReset).
type readAfterClaimKey struct{}

func withReadAfterClaim(ctx context.Context) context.Context {
	return context.WithValue(ctx, readAfterClaimKey{}, true)
}

// readAfterClaimMissing reports a meter the claim on account gave back that
// windows, the claim path's own read after it, carry no figure for. That
// read then adds nothing for it: no figure from before the claim, no Fable
// probe (a request the claim path never made), and no figure made up for a
// bucket the answer left out. The meter reads as not read since the claim.
func (r *AccountRef) readAfterClaimMissing(ctx context.Context, account accounts.Account, windows []accounts.UsageWindow) bool {
	if after, _ := ctx.Value(readAfterClaimKey{}).(bool); !after {
		return false
	}
	claim, ok := r.claimedResetFor(accountProviderOrCodex(account), account.ID)
	if !ok {
		return false
	}
	for _, meter := range claim.meters {
		if !slices.ContainsFunc(windows, meter.is) {
			return true
		}
	}
	return false
}

// claimedBefore reports a reply to a request sent before the account's last
// landed claim: its figures were measured before the claim, so they say
// nothing about the meters it gave back.
func (r *AccountRef) claimedBefore(account accounts.Account, requestStarted time.Time) bool {
	claim, ok := r.claimedResetFor(accountProviderOrCodex(account), account.ID)
	return ok && !requestStarted.After(claim.at)
}
