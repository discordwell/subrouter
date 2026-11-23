package proxy

import (
	"context"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// Quota failover with nowhere to go is what this file exists for.
//
// The websocket and HTTP failover paths both end at oauthRetryAccount, which
// answers "no untried non-exhausted account" when every account in the pool is
// cooked. That answer is correct and useless: the turn dies with the upstream's
// own quota error, which is exactly the interruption failover was built to
// prevent. Observed live with a two-account Codex pool -- one on a workspace
// spend cap, one at 100% of its weekly window -- every turn died on the first
// message and the failover logged nothing at all, because the "no alternate"
// branch returned silently.
//
// But a cooked ChatGPT Pro account is not necessarily out of capacity: it may
// still hold complimentary rate-limit-reset credits, the same ones `sr reset`
// and /_subrouter/rate-limit-reset redeem by hand. Redeeming one restores the
// account's window immediately, which turns "no alternate exists" into "an
// alternate exists once we spend a credit".
//
// Spending it automatically is gated, and stays off by default, for the reason
// that makes the credit worth anything: it is FINITE and does not come back.
// A daemon that burns one per interrupted turn empties the reserve in an
// afternoon, so the gate below is three separate bounds -- an explicit opt-in,
// one redemption at a time daemon-wide, and a floor on the interval between
// them -- rather than one flag anyone could flip and forget.

// autoRedeemMinInterval is the floor between two automatic redemptions. Bursts
// are the hazard: a single interrupted turn produces several failing requests
// (Codex retries, and every concurrent session sees the same cooked pool), and
// without a floor each one would independently decide to spend a credit. One
// redemption restores a window for hours, so anything finer buys nothing and
// costs the reserve.
const autoRedeemMinInterval = 30 * time.Minute

// quotaResetGate serializes automatic redemptions and enforces the interval
// floor. Server is copied by value on every request, so the gate is a pointer
// field filled in by normalized() -- one gate per daemon, shared by all
// handlers.
type quotaResetGate struct {
	mu     sync.Mutex
	lastAt time.Time
}

// claim takes the redemption slot, returning false if the caller must not
// spend. It returns HOLDING the lock on success -- release frees it.
//
// Holding it across the whole redemption (two upstream fetches and a consume)
// is deliberate: a burst of turns all discovering the same cooked pool must
// serialize, not race. The second caller therefore blocks here rather than
// returning immediately, and by the time it wins the lock the first caller has
// recorded lastAt, so the interval floor turns it away. It is the failure path
// of a turn that is already dying, so the wait costs nothing that was working.
func (g *quotaResetGate) claim() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	if !g.lastAt.IsZero() && time.Since(g.lastAt) < autoRedeemMinInterval {
		g.mu.Unlock()
		return false
	}
	return true
}

// release records the outcome and frees the slot. It arms the interval floor
// only when a credit actually left the account: a redemption that failed
// upstream changed nothing, and must not lock out the next attempt.
func (g *quotaResetGate) release(spent bool) {
	if g == nil {
		return
	}
	if spent {
		g.lastAt = time.Now()
	}
	g.mu.Unlock()
}

// failoverRedeems reports the failover redeem on: SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET
// set, and automatic resets not in real mode, where the one decision spends
// Codex credits instead (PATCHES §94.2).
func (s Server) failoverRedeems() bool {
	return s.AutoRedeemRateLimitReset && !autoResetFoldsOlderSpenders()
}

// redeemForFailover is the last stage of Codex quota failover: every account is
// cooked, so find one that still holds a rate-limit-reset credit, spend it, and
// hand back the revived account for the caller to route to.
//
// It returns ok=false whenever it changes nothing -- disabled, rate-limited by
// the interval floor, or no eligible account -- and every one of those branches
// logs, because a failover that silently gives up is the bug this replaces.
func (s Server) redeemForFailover(ctx context.Context, provider accounts.Provider, agentType, sessionID, userEmail string, tried map[string]struct{}) (accounts.Account, bool) {
	if !s.failoverRedeems() || provider != accounts.ProviderCodex || s.AccountRef == nil {
		return accounts.Account{}, false
	}
	if !s.quotaResetGate.claim() {
		if s.Logger != nil {
			s.Logger.Warn("codex quota failover has no alternate and will not redeem",
				"reason", "another redemption happened within the interval floor",
				"session", sessionID,
				"min_interval", autoRedeemMinInterval.String())
		}
		return accounts.Account{}, false
	}
	spent := false
	defer func() { s.quotaResetGate.release(spent) }()

	candidates := filterAccountsForProvider(s.accountListContext(ctx), provider)
	for _, candidate := range candidates {
		if candidate.AuthMode != accounts.AuthModeOAuth || candidate.Token == "" {
			continue
		}
		if _, ok := tried[candidate.ID]; ok {
			continue
		}
		// redeemAccountIfEligible re-reads live usage and refuses an account
		// that is not cooked or holds no credit, so eligibility is decided
		// against the upstream rather than against our own cached scores --
		// the scores are what just told us the pool was empty.
		result := s.redeemAccountIfEligible(ctx, candidate, false)
		if !result.Reset {
			if s.Logger != nil && result.Eligible {
				s.Logger.Warn("codex rate-limit reset redemption failed",
					"account", candidate.ID, "error", result.Error)
			}
			continue
		}
		spent = true
		// The account is revived upstream, but routing still holds the marks
		// and the zero score that made it unroutable. Clear both, or the
		// credit is spent on an account the scheduler keeps skipping.
		if s.SchedulerRef != nil {
			s.SchedulerRef.ClearExhausted(provider, candidate.ID)
			// Force the next selection to re-score against live usage rather
			// than serve the snapshot that predates the reset.
			s.SchedulerRef.SetUpdatedAt(time.Time{})
		}
		if s.Sessions != nil {
			if _, err := s.Sessions.Put(agentType, sessionID, candidate.ID, userEmail); err != nil && s.Logger != nil {
				s.Logger.Warn("codex quota failover could not pin session to reset account",
					"session", sessionID, "account", candidate.ID, "error", err)
			}
		}
		if s.Logger != nil {
			creditID := ""
			if result.Credit != nil {
				creditID = result.Credit.ID
			}
			s.Logger.Warn("codex quota failover redeemed a rate-limit reset credit",
				"session", sessionID,
				"account", candidate.ID,
				"credit", creditID)
		}
		return candidate, true
	}
	if s.Logger != nil {
		s.Logger.Warn("codex quota failover has no alternate and no redeemable credit",
			"session", sessionID,
			"provider", provider,
			"candidates", len(candidates))
	}
	return accounts.Account{}, false
}
