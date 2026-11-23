package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/fsutil"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// PATCHES §93.2: in real mode the daemon claims the reset §93.1 decided on,
// through the same claim functions the manual routes call (claudeReset,
// redeemAccountChecked), while the turns for the account wait on it in the
// usage-limit retry transport; and it never claims twice.

const (
	// autoResetClaimFloor: never two automatic claims on one account within
	// it, whatever the readings say.
	autoResetClaimFloor = 10 * time.Minute
	// autoResetClaudePatience is how long an automatic Claude claim may spend
	// before it sends: on the account's claim lock, its reads, and Anthropic's
	// 429s on the reset routes (claudeReset's caller context). The claim and
	// the re-read after it (claudeResetClaimReserve) then still fit inside the
	// turns' wait.
	autoResetClaudePatience = 2*time.Minute + 15*time.Second
	// autoResetCodexClaimBudget bounds one automatic Codex redeem: two usage
	// reads, the credit list and the consume.
	autoResetCodexClaimBudget = 2 * time.Minute
	// autoResetBackUsed is how used a meter may read and count as back after
	// a claim: half. A reset gives the meter back whole (the week or Fable
	// meter reads 0%), so a reading near the limit after a claim says it did
	// not land, and settles nothing.
	autoResetBackUsed = 50.0
	// autoResetTurnWaits is the turn of a decision whose turn waits for the
	// claim on the same account.
	autoResetTurnWaits = "waits on the same account for the claim"

	autoResetClaimingMessage = "auto reset: claiming the next saved reset"
	autoResetFinishedMessage = "auto reset: claim finished"
)

// How long a turn waits on the same account for a claim, counted from when
// the claim started. Past it the turn gets today's answer and the claim goes
// on. Claude Code waits 600 s for an answer's headers (§58); Codex's redeem
// takes seconds. Vars only so tests do not sit through them.
var (
	autoResetClaudeWait = autoResetClaudePatience + claudeResetClaimReserve
	autoResetCodexWait  = time.Minute
)

func autoResetWaitFor(provider accounts.Provider) time.Duration {
	if provider == accounts.ProviderClaude {
		return autoResetClaudeWait
	}
	return autoResetCodexWait
}

// autoResetClaim is one automatic claim in flight. Turns that wait on it read
// result once done is closed.
type autoResetClaim struct {
	// deadline is when the turns stop waiting.
	deadline time.Time
	done     chan struct{}
	result   autoResetClaimResult
}

// autoResetClaimResult is what a claim came to.
type autoResetClaimResult struct {
	// landed: the provider says the reset was used.
	landed bool
	// back: the reading after the claim shows every meter it gives back
	// back (autoResetBackUsed), so the waiting turns go to the same account.
	back bool
	// sent: a claim request may have reached the provider.
	sent bool
	// unsure: the provider's answer leaves it unknown whether the reset was
	// used.
	unsure bool
	reason string
}

// waitFor waits for c's answer while ctx lasts, until its deadline, and only
// while the daemon is not draining. ok is false when the wait ended without
// the answer: the turn then gets today's answer.
func (c *autoResetClaim) waitFor(ctx context.Context, lifecycle *Lifecycle) (autoResetClaimResult, bool) {
	if c == nil {
		return autoResetClaimResult{}, false
	}
	select {
	case <-c.done:
		return c.result, true
	default:
	}
	timer := time.NewTimer(time.Until(c.deadline))
	defer timer.Stop()
	poll := time.NewTicker(lifecycleWaitPoll)
	defer poll.Stop()
	for !lifecycle.Draining() {
		select {
		case <-c.done:
			return c.result, true
		case <-ctx.Done():
			return autoResetClaimResult{}, false
		case <-timer.C:
			return autoResetClaimResult{}, false
		case <-poll.C:
		}
	}
	return autoResetClaimResult{}, false
}

// autoResetLatch is "never twice" for one account: its last automatic claim,
// from when it started until a reading measured after that shows the meters
// it gives back are back. It is kept in the daemon's state directory too, so
// the next worker generation does not claim again without a reading.
type autoResetLatch struct {
	Provider  accounts.Provider `json:"provider"`
	Account   string            `json:"account"`
	Kind      string            `json:"kind"`
	Reset     string            `json:"reset,omitempty"`
	Meters    []autoResetMeter  `json:"meters"`
	StartedAt time.Time         `json:"started_at"`
	// Finished is false while the claim runs, and stays false in the record
	// of a worker that left in the middle of one: such a claim counts as sent.
	Finished bool `json:"finished"`
	Sent     bool `json:"sent"`
	// BackAt is when a reading measured after StartedAt showed every meter in
	// Meters back (autoResetBackUsed).
	BackAt  time.Time `json:"back_at,omitzero"`
	Outcome string    `json:"outcome,omitempty"`
}

// blocks says why l forbids another automatic claim at now, or "".
func (l autoResetLatch) blocks(now time.Time) string {
	switch {
	case l.StartedAt.IsZero():
		return ""
	case now.Sub(l.StartedAt) < autoResetClaimFloor:
		return fmt.Sprintf("never twice: an automatic claim on this account started at %s, and no other follows within %s",
			l.StartedAt.UTC().Format(time.RFC3339), autoResetClaimFloor)
	case (l.Sent || !l.Finished) && l.BackAt.IsZero():
		return fmt.Sprintf("never twice: the automatic claim sent at %s is not settled, and only a reading that shows %s back settles it",
			l.StartedAt.UTC().Format(time.RFC3339), autoResetMetersWords(l.Meters))
	}
	return ""
}

// settle notes BackAt when readings, measured after the claim started, show
// every meter in Meters back. It reports a change.
func (l *autoResetLatch) settle(readings map[autoResetMeter]autoResetReading) bool {
	if l.StartedAt.IsZero() || !l.BackAt.IsZero() {
		return false
	}
	back, ok := autoResetMetersBack(l.Meters, readings, l.StartedAt)
	if ok {
		l.BackAt = back
	}
	return ok
}

// autoResetMetersBack reports whether every meter reads back (at most
// autoResetBackUsed) on a reading measured after since, and when the newest
// of those was measured.
func autoResetMetersBack(meters []autoResetMeter, readings map[autoResetMeter]autoResetReading, since time.Time) (time.Time, bool) {
	var newest time.Time
	for _, meter := range meters {
		reading, ok := readings[meter]
		if !ok || !reading.at.After(since) || (1-reading.left)*100 > autoResetBackUsed {
			return time.Time{}, false
		}
		if reading.at.After(newest) {
			newest = reading.at
		}
	}
	return newest, len(meters) > 0
}

func autoResetMetersWords(meters []autoResetMeter) string {
	words := make([]string, 0, len(meters))
	for _, meter := range meters {
		words = append(words, meter.words())
	}
	return strings.Join(words, " and ")
}

// autoResetClaimMeters are the meters a claim must give back for the turns to
// go to the same account: the one its kind gives back, and the one the moment
// found spent.
func autoResetClaimMeters(kind autoResetKind, f autoResetFacts) []autoResetMeter {
	meters := []autoResetMeter{kind.meter()}
	moment := f.wallMeter
	if f.moment == autoResetOnAReading {
		moment = f.readingMeter
	}
	if moment != "" && moment != kind.meter() {
		meters = append(meters, moment)
	}
	return meters
}

func autoResetKey(account accounts.Account) string {
	return selectacct.ScoreKey(schedulerAccountProvider(accountProviderOrCodex(account)), account.ID)
}

// autoResetRunningClaim is the automatic claim in flight on account, or nil.
func (s Server) autoResetRunningClaim(account accounts.Account) *autoResetClaim {
	if s.AccountRef == nil {
		return nil
	}
	memory := s.AccountRef.sharedAutoResets()
	memory.mu.Lock()
	defer memory.mu.Unlock()
	return memory.running[autoResetKey(account)]
}

// autoResetWaitBeforeSend is where a turn addressed to an account waits for
// the claim running on it before anything is sent, so it is neither sent to
// the limit nor moved off the account by the marks or the paused admission
// gate the limit left (§93.2).
func (s Server) autoResetWaitBeforeSend(ctx context.Context, provider accounts.Provider, id string) {
	claim := s.autoResetRunningClaim(accounts.Account{ID: id, Provider: provider})
	claim.waitFor(ctx, s.Lifecycle)
}

// startAutoResetClaim starts the claim d decided on, unless the account's
// last automatic claim forbids it (never twice) or the daemon is draining;
// then it returns nil and why. A claim already running on the account is
// returned as it is.
func (s Server) startAutoResetClaim(account accounts.Account, f autoResetFacts, d AutoResetDecision) (*autoResetClaim, string) {
	if s.Lifecycle.Draining() {
		return nil, "the daemon is draining or shutting down, and starts no claim"
	}
	memory := s.AccountRef.sharedAutoResets()
	key := autoResetKey(account)
	stored, _ := s.loadAutoResetLatch(key)
	memory.mu.Lock()
	if claim := memory.running[key]; claim != nil {
		memory.mu.Unlock()
		return claim, ""
	}
	latch := newerAutoResetLatch(memory.latches[key], stored)
	settled := latch.settle(f.readings)
	memory.latches[key] = latch
	if why := latch.blocks(f.now); why != "" {
		memory.mu.Unlock()
		if settled {
			s.saveAutoResetLatch(key, latch)
		}
		return nil, why
	}
	kind := autoResetKind(d.Kind)
	claim := &autoResetClaim{deadline: time.Now().Add(autoResetWaitFor(d.Provider)), done: make(chan struct{})}
	latch = autoResetLatch{Provider: d.Provider, Account: account.ID, Kind: d.Kind, Reset: d.Reset,
		Meters: autoResetClaimMeters(kind, f), StartedAt: f.now}
	memory.running[key] = claim
	memory.latches[key] = latch
	d.Reason += "; claiming it now"
	memory.last[key] = d
	memory.mu.Unlock()
	// On disk before anything is sent: a worker that takes over in the middle
	// of the claim reads it as sent.
	s.saveAutoResetLatch(key, latch)
	if !s.Lifecycle.StartBackground(func() { s.runAutoResetClaim(account, key, claim, f, d) }) {
		s.finishAutoResetClaim(key, claim, d, autoResetClaimResult{reason: "nothing was sent: the daemon began draining"})
		return nil, "the daemon is draining or shutting down, and starts no claim"
	}
	if s.Logger != nil {
		s.Logger.Warn(autoResetClaimingMessage, autoResetLineFields(d)...)
	}
	return claim, ""
}

// newerAutoResetLatch is the latch of the later claim, mine from this
// process or stored by another worker, with what either learned of it.
func newerAutoResetLatch(mine, stored autoResetLatch) autoResetLatch {
	switch {
	case stored.StartedAt.After(mine.StartedAt):
		return stored
	case stored.StartedAt.Equal(mine.StartedAt) && mine.BackAt.IsZero():
		mine.BackAt = stored.BackAt
	}
	return mine
}

// runAutoResetClaim sends the claim and reads the account again.
func (s Server) runAutoResetClaim(account accounts.Account, key string, claim *autoResetClaim, f autoResetFacts, d AutoResetDecision) {
	var result autoResetClaimResult
	var after []accounts.UsageWindow
	switch d.Provider {
	case accounts.ProviderClaude:
		result, after = s.autoResetClaimClaude(account, d.Reset)
	case accounts.ProviderCodex:
		result, after = s.autoResetClaimCodex(account, f)
	default:
		result.reason = "nothing was sent: no claim exists for provider " + string(d.Provider)
	}
	if result.landed {
		memory := s.AccountRef.sharedAutoResets()
		memory.mu.Lock()
		meters := memory.latches[key].Meters
		memory.mu.Unlock()
		// The claim functions read the account again once the reset is used.
		_, result.back = autoResetMetersBack(meters, autoResetReadingsIn(d.Provider, after, time.Now()), time.Time{})
		if result.back {
			result.reason += "; the reading after it shows " + autoResetMetersWords(meters) + " back"
		} else {
			result.reason += "; the reading after it does not show " + autoResetMetersWords(meters) + " back"
		}
	}
	s.finishAutoResetClaim(key, claim, d, result)
}

// finishAutoResetClaim records what claim came to and lets the turns waiting
// on it go.
func (s Server) finishAutoResetClaim(key string, claim *autoResetClaim, d AutoResetDecision, result autoResetClaimResult) {
	memory := s.AccountRef.sharedAutoResets()
	memory.mu.Lock()
	now := memory.now()
	latch := memory.latches[key]
	latch.Finished, latch.Sent, latch.Outcome = true, result.sent, result.reason
	if result.back {
		latch.BackAt = now
	}
	memory.latches[key] = latch
	delete(memory.running, key)
	claim.result = result
	d.At, d.Verdict, d.Reason = now, AutoResetFailed, d.Reason+"; "+result.reason
	switch {
	case result.landed:
		d.Verdict = AutoResetClaimed
	case result.unsure:
		d.Verdict = AutoResetMayHaveLanded
	}
	memory.last[key] = d
	memory.mu.Unlock()
	close(claim.done)
	s.saveAutoResetLatch(key, latch)
	if s.Logger != nil {
		s.Logger.Warn(autoResetFinishedMessage, append(autoResetLineFields(d),
			"verdict", string(d.Verdict), "landed", result.landed, "back", result.back, "sent", result.sent)...)
	}
}

// autoResetClaimClaude claims grant on account through the manual route's
// claudeReset, which takes the account's claim lock, reads the block live
// unless the cached one is claimable, refuses any grant but Anthropic's next
// (so a grant the decision did not judge is never claimed), reuses a
// request_id that may have landed, and reads the account again after a
// reset. It returns what came of it and that reading.
func (s Server) autoResetClaimClaude(account accounts.Account, grant string) (autoResetClaimResult, []accounts.UsageWindow) {
	work, cancelWork := context.WithTimeout(context.Background(), ClaudeResetBudget)
	defer cancelWork()
	caller, cancelCaller := context.WithTimeout(work, autoResetClaudePatience)
	defer cancelCaller()
	claim := false
	response, code := s.claudeReset(work, caller, ClaudeResetRequest{
		Account: account.ID, ConfirmAccount: account.ID, DryRun: &claim, GrantID: grant,
	})
	answered := response.Result
	if response.Reason != "" {
		answered += " (" + response.Reason + ")"
	}
	switch {
	case code != http.StatusOK || !response.Claimed:
		why := response.Refusal
		if why == "" {
			why = response.Error
		}
		return autoResetClaimResult{reason: "nothing was sent: " + why}, nil
	case response.Result == agentclaude.ResetResultReset:
		return autoResetClaimResult{landed: true, sent: true, reason: "Anthropic used the reset (result reset)"}, response.WindowsAfter
	case response.Unconfirmed:
		return autoResetClaimResult{sent: true, unsure: true, reason: "Anthropic's answer, " + answered + " " + response.Error +
			", leaves it unknown whether the reset was used: it may have landed"}, nil
	}
	return autoResetClaimResult{sent: true, reason: "Anthropic answered " + answered + " " + response.Error}, nil
}

// autoResetClaimCodex redeems a credit on account through the manual
// routes' redeemAccountChecked, which reads usage live under the daemon-wide
// redeem lock and consumes the available credit that expires soonest. At a
// reading moment the account is not yet at its limit, so the cooked rule is
// lifted there (anytime). The last check sees the exact credit and the live
// reading: a credit of a kind this daemon does not know is never redeemed,
// and while the owner's condition is on the live week must have under 1%
// left.
func (s Server) autoResetClaimCodex(account accounts.Account, f autoResetFacts) (autoResetClaimResult, []accounts.UsageWindow) {
	ctx, cancel := context.WithTimeout(context.Background(), autoResetCodexClaimBudget)
	defer cancel()
	sent := false
	approve := func(live rateLimitResetCandidate, now time.Time) error {
		if resetType := strings.TrimSpace(live.credit.ResetType); resetType != "" && resetType != codexRateLimitsResetType {
			return fmt.Errorf("the credit it would spend is of a kind this daemon does not know (reset_type %s)", resetType)
		}
		if f.nearEmpty {
			week, ok := autoResetReadingsIn(accounts.ProviderCodex, live.before, now)[autoResetWeek]
			if !ok || week.left >= autoResetNearEmptyLeft {
				return errors.New("refused by the under-1-percent condition on the live reading before the consume")
			}
		}
		sent = true
		return nil
	}
	result := s.redeemAccountChecked(ctx, account, false, f.moment == autoResetOnAReading, approve)
	switch {
	case result.Reset:
		credit := ""
		if result.Credit != nil {
			credit = result.Credit.ID
		}
		// As the failover redeem does: routing still holds the marks and the
		// score that made the account unroutable.
		if s.SchedulerRef != nil {
			s.SchedulerRef.ClearExhausted(schedulerAccountProvider(accounts.ProviderCodex), account.ID)
			s.SchedulerRef.SetUpdatedAt(time.Time{})
		}
		s.AccountRef.InvalidateUsageStatusCache()
		return autoResetClaimResult{landed: true, sent: true, reason: "the credit " + credit + " was redeemed"}, result.WindowsAfter
	case sent:
		return autoResetClaimResult{sent: true, unsure: true, reason: "the consume did not come back as a redeem (" + result.Error + "); it may have landed"}, nil
	}
	return autoResetClaimResult{reason: "nothing was sent: " + result.Error}, nil
}

// settleAutoResetLatch settles the account's last automatic claim with a
// reading, in memory and on disk.
func (s Server) settleAutoResetLatch(memory *autoResets, key string, readings map[autoResetMeter]autoResetReading) {
	memory.mu.Lock()
	latch, ok := memory.latches[key]
	changed := ok && latch.settle(readings)
	if changed {
		memory.latches[key] = latch
	}
	memory.mu.Unlock()
	if changed {
		s.saveAutoResetLatch(key, latch)
	}
}

// autoResetLatchPath is where the latch of key is kept, or "" with no state
// directory.
func (s Server) autoResetLatchPath(key string) string {
	if strings.TrimSpace(s.AutoResetStateDir) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(s.AutoResetStateDir, "auto-reset", hex.EncodeToString(sum[:8])+".json")
}

func (s Server) saveAutoResetLatch(key string, latch autoResetLatch) {
	path := s.autoResetLatchPath(key)
	if path == "" {
		return
	}
	body, err := json.Marshal(latch)
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
			err = fsutil.WriteFileAtomic(path, body, 0o600)
		}
	}
	if err != nil && s.Logger != nil {
		s.Logger.Warn("auto reset: could not keep the claim's record for the next worker", "path", path, "error", err)
	}
}

func (s Server) loadAutoResetLatch(key string) (autoResetLatch, bool) {
	path := s.autoResetLatchPath(key)
	if path == "" {
		return autoResetLatch{}, false
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return autoResetLatch{}, false
	}
	var latch autoResetLatch
	if json.Unmarshal(body, &latch) != nil {
		return autoResetLatch{}, false
	}
	return latch, true
}

// autoResetWaitsInPlace reports a turn that may wait for an automatic claim
// on its account: real mode, on a daemon that holds its credentials itself
// and is not draining, for a client still waiting, and not a turn a peer
// forwarded. Nothing of it has reached the client: the retry transport
// answers only once it is done.
func (t usageLimitRetryTransport) autoResetWaitsInPlace(req *http.Request) bool {
	if mode, _ := AutoReset(); mode != AutoResetReal || t.server == nil || t.peerForwarded {
		return false
	}
	return t.server.AccountRef != nil && t.server.CredentialBroker == nil && !t.server.Lifecycle.Draining() && req.Context().Err() == nil
}
