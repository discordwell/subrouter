package proxy

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	agentclaude "github.com/discordwell/subrouter/internal/agents/claude"
)

// Every saved reset an account holds is shown, with its kind and expiry
// (PATCHES §91.1). A Claude row lists the grants its cedar_ember block
// carries. A Codex usage reading carries only a count, so the daemon that
// holds the account reads the dates from the credit list Codex.app reads
// (GET wham/rate-limit-reset-credits): in the background, at most once an
// hour per account and at once when the count changes, never for a count of
// 0. It only reads. Nothing here ranks, routes or claims.

const (
	// codexResetExpiryEvery is the most often the list is read for an account
	// whose count has not changed.
	codexResetExpiryEvery = time.Hour
	// codexResetExpiryTimeout bounds one read of the list.
	codexResetExpiryTimeout = 20 * time.Second
	// unknownResetWarnEvery bounds the unknown-kind line to one per account
	// and kind an hour: every usage-status read lists the resets.
	unknownResetWarnEvery = time.Hour
)

const (
	codexResetExpiryFailedMessage = "codex reset credit list read failed; the last good expiry dates stand"
	unknownResetMessage           = "saved reset of a kind this daemon does not know; shown, never claimed automatically"
)

// readCodexResetExpiryOff is the switch of the Codex list reader
// (SUBROUTER_READ_CODEX_RESET_EXPIRY), on by default.
var readCodexResetExpiryOff atomic.Bool

// SetReadCodexResetExpiry turns the Codex credit list reader on or off for
// this process. Off, nothing is read and every Codex count says its expiry
// was not read.
func SetReadCodexResetExpiry(on bool) {
	readCodexResetExpiryOff.Store(!on)
}

// ReadCodexResetExpiry reports whether the Codex credit list reader is on.
func ReadCodexResetExpiry() bool {
	return !readCodexResetExpiryOff.Load()
}

// heldResets is the daemon's memory for the resets list: the Codex credit
// lists as last read, and when each unknown kind was last logged.
type heldResets struct {
	mu      sync.Mutex
	codex   map[string]*codexResetExpiry
	warned  map[string]time.Time
	timeout time.Duration
	// now nil is time.Now: the clock the list is published by.
	now   func() time.Time
	reads sync.WaitGroup
}

// codexResetExpiry is one account's credit list reads.
type codexResetExpiry struct {
	// triedAt is when the last read was sent, and count the usage reading's
	// count it was sent for.
	triedAt  time.Time
	count    int
	inflight bool
	// failed is the last read's outcome; the dates stay those of goodAt.
	failed bool
	// goodAt is when the last good read was sent, and credits its list.
	goodAt  time.Time
	credits []accounts.RateLimitResetCredit
}

func newHeldResets() *heldResets {
	return &heldResets{codex: map[string]*codexResetExpiry{}, warned: map[string]time.Time{}, timeout: codexResetExpiryTimeout}
}

// sharedHeldResets returns the daemon's memory, created on first use.
// Handlers and the reader run on separate copies of Server, so it lives on
// the ref they share.
func (r *AccountRef) sharedHeldResets() *heldResets {
	if r == nil {
		return newHeldResets()
	}
	if memory := r.heldResetMemory.Load(); memory != nil {
		return memory
	}
	r.heldResetMemory.CompareAndSwap(nil, newHeldResets())
	return r.heldResetMemory.Load()
}

func (h *heldResets) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// RunCodexResetExpiryReader reads this daemon's Codex credit lists every
// interval until ctx ends, and returns once its reads have ended with it.
func (s Server) RunCodexResetExpiryReader(ctx context.Context, every time.Duration) {
	if s.AccountRef == nil || every <= 0 {
		return
	}
	memory := s.AccountRef.sharedHeldResets()
	defer memory.reads.Wait()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		s.readCodexResetExpiries(ctx, memory, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// readCodexResetExpiries is one pass at now. It reads the counts the last
// usage sweep published, so it adds no usage read, and starts a list read for
// each account due. Only an account this daemon holds a credential for is
// read: a peer's account is never in its sweep, and in team mode the daemon
// holds no credential of its own.
func (s Server) readCodexResetExpiries(ctx context.Context, memory *heldResets, now time.Time) {
	if !ReadCodexResetExpiry() || ctx.Err() != nil || s.AccountRef == nil || s.CredentialBroker != nil || s.Lifecycle.Draining() {
		return
	}
	rows, ok := s.AccountRef.UsageStatusSnapshot()
	if !ok {
		return
	}
	held := map[string]accounts.Account{}
	for _, account := range oauthAccounts(filterAccountsForProvider(s.accountList(), accounts.ProviderCodex)) {
		if account.Token != "" {
			held[account.ID] = account
		}
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	for _, row := range rows {
		count, _, ok := codexResetCount(row)
		account, holds := held[row.ID]
		if !ok || !holds {
			continue
		}
		entry := memory.codex[row.ID]
		if !codexResetExpiryDue(entry, count, now) {
			continue
		}
		if entry == nil {
			entry = &codexResetExpiry{}
			memory.codex[row.ID] = entry
		}
		entry.triedAt, entry.count, entry.inflight = now, count, true
		s.startCodexResetExpiryRead(ctx, memory, entry, account)
	}
}

// codexResetExpiryDue reports whether an account whose usage reading says
// count credits is read now: never for 0, first for an account never read,
// at once when the count is not the one the last read was for, else an hour
// after the last read, whatever it answered.
func codexResetExpiryDue(entry *codexResetExpiry, count int, now time.Time) bool {
	switch {
	case count <= 0:
		return false
	case entry == nil:
		return true
	case entry.inflight:
		return false
	case entry.count != count:
		return true
	}
	return now.Sub(entry.triedAt) >= codexResetExpiryEvery
}

// startCodexResetExpiryRead reads account's list in the background. The
// caller holds memory.mu. A failed read keeps the last good list, and is
// only logged: it marks nothing and moves no session.
func (s Server) startCodexResetExpiryRead(ctx context.Context, memory *heldResets, entry *codexResetExpiry, account accounts.Account) {
	sentAt, timeout := entry.triedAt, memory.timeout
	client := s.AccountRef.client
	memory.reads.Add(1)
	started := s.Lifecycle.StartBackground(func() {
		defer memory.reads.Done()
		readCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		credits, err := accounts.ListRateLimitResetCredits(readCtx, client, account)
		memory.mu.Lock()
		entry.inflight = false
		entry.failed = err != nil
		if err == nil {
			entry.goodAt, entry.credits = sentAt, credits
		}
		goodAt := entry.goodAt
		memory.mu.Unlock()
		if err != nil {
			fields := []any{"account", account.ID, "error", err.Error()}
			if !goodAt.IsZero() {
				fields = append(fields, "dates_read_at", goodAt.UTC().Format(time.RFC3339))
			}
			s.heldResetLogger().Warn(codexResetExpiryFailedMessage, fields...)
		}
	})
	if !started {
		memory.reads.Done()
		entry.inflight = false
	}
}

func (s Server) heldResetLogger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// codexResetCount is the credit count a Codex OAuth row's usage reading
// carries, and the key it came from.
func codexResetCount(row AccountUsageStatus) (int, string, bool) {
	reset := row.ComplimentaryReset
	if row.Provider != accounts.ProviderCodex || row.AuthMode != accounts.AuthModeOAuth || reset == nil || !reset.Known || reset.Remaining == nil {
		return 0, "", false
	}
	return *reset.Remaining, reset.Source, true
}

// withHeldResets sets each row's resets: a Claude row's from the block it
// carries, a Codex row's from its count and the credit list as last read.
// A row whose account's last landed claim no reading has replaced carries
// the block that claim left first (§102.1), and its resets are built from it.
// It logs each unknown kind. The input may be the shared cached snapshot, so
// it is copied, not mutated.
func (s Server) withHeldResets(statuses []AccountUsageStatus) []AccountUsageStatus {
	memory := s.AccountRef.sharedHeldResets()
	now := memory.clock()
	out := append([]AccountUsageStatus(nil), statuses...)
	s.withResetsFromClaims(out)
	for i := range out {
		out[i].Resets = nil
		if reset := out[i].ComplimentaryReset; reset != nil && reset.Source == agentclaude.ResetProgram {
			// Nil on a block restored from the persisted snapshot, which
			// does not keep the list: not read, never "none".
			out[i].Resets = reset.Held
		} else if count, source, ok := codexResetCount(out[i]); ok {
			out[i].Resets = memory.codexHeld(out[i].ID, count, source, now)
		}
		memory.warnUnknown(s.heldResetLogger(), out[i], now)
	}
	return out
}

// codexHeld is a Codex row's resets: each credit the last good list read
// holds, then whatever the count has beyond them, whose expiry was not read.
// With the reader off every credit is that.
func (h *heldResets) codexHeld(id string, count int, source string, now time.Time) []accounts.HeldReset {
	if count <= 0 {
		return []accounts.HeldReset{}
	}
	var readings []accounts.HeldResetReading
	listed := 0
	h.mu.Lock()
	entry := h.codex[id]
	if entry != nil && !entry.goodAt.IsZero() && ReadCodexResetExpiry() {
		state := accounts.ResetExpiryRead
		if entry.failed {
			state = accounts.ResetExpiryLastGood
		}
		for _, credit := range entry.credits {
			if !accounts.CreditHeldAt(credit, now) {
				continue
			}
			expiry := state
			if strings.TrimSpace(credit.ExpiresAt) == "" {
				expiry = accounts.ResetExpiryNoneGiven
			}
			readings = append(readings, accounts.HeldResetReading{
				ResetReading: accounts.ResetReading{Provider: accounts.ProviderCodex, Type: credit.ResetType},
				Count:        1,
				ExpiresAt:    credit.ExpiresAt,
				Expiry:       expiry,
				ExpiryReadAt: entry.goodAt,
			})
			listed++
		}
	}
	h.mu.Unlock()
	if count > listed {
		readings = append(readings, accounts.HeldResetReading{
			ResetReading: accounts.ResetReading{Provider: accounts.ProviderCodex, Source: source},
			Count:        count - listed,
			Expiry:       accounts.ResetExpiryNotRead,
		})
	}
	return accounts.HeldResets(readings, now)
}

// forgetCredit drops a credit redeemed on account id from the list as last
// read: it is spent at the provider (§102.1).
func (h *heldResets) forgetCredit(id, creditID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if entry := h.codex[id]; entry != nil {
		entry.credits = slices.DeleteFunc(slices.Clone(entry.credits), func(credit accounts.RateLimitResetCredit) bool {
			return credit.ID == creditID
		})
	}
}

// warnUnknown logs each unknown kind on row, once an hour per account and
// kind.
func (h *heldResets) warnUnknown(logger *slog.Logger, row AccountUsageStatus, now time.Time) {
	for _, entry := range row.Resets {
		if entry.Kind != accounts.ResetKindUnknown {
			continue
		}
		key := strings.Join([]string{string(row.Provider), row.ID, entry.RawType, strings.Join(entry.GivesBack, ",")}, "\x00")
		h.mu.Lock()
		last, seen := h.warned[key]
		if seen && now.Sub(last) < unknownResetWarnEvery {
			h.mu.Unlock()
			continue
		}
		h.warned[key] = now
		h.mu.Unlock()
		logger.Warn(unknownResetMessage, "account", row.ID, "provider", string(row.Provider),
			"raw_type", entry.RawType, "clears", entry.GivesBack, "count", entry.Count, "expires_at", entry.ExpiresAt)
	}
}
