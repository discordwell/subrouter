package proxy

import (
	"context"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

// Codex capacity retry policy and the websocket reconnect's capacity model
// (PATCHES section 27).
//
// OpenAI's "Selected model is at capacity" is probabilistic load shedding:
// pressing retry a few times usually gets the same request through, often on
// the same account. So the proxy retries capacity failures transparently, but
// only before any output reached the client (#373's stream peek guarantees a
// failure it classifies is pre-output). Upstream manaflow-ai/subrouter #389
// (fb575d1) and #382 (862a5a5), as at c9303d3, ported as written bar the
// pieces named below.
//
// The session's prompt cache lives on its account, and moving a turn to
// another account re-bills the whole conversation as uncached input. So the
// capacity layer is installed on every replayable Codex POST, and:
//
//   - the stay ladder (failover off, or a conversation above the failover's
//     size cap with it on): wait it out on the same account with jittered
//     gaps of about 0.5s, 1s, 2s, 4s, 8s, then a steady ~9s (8-10s;
//     CodexOverloadFailoverConfig.StayInterval, the ramp capped at it), for
//     up to 4m of wall-clock time from the first attempt (StayMaxWait;
//     unbounded with SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT=0), inside Codex
//     CLI's 5-minute stream idle timeout. With an egress or Azure fallback
//     configured the operator default is capped at ~10s so the fallback takes
//     over sooner (the fork configures neither today). The account is not
//     marked and the attempt budget is not drawn on: the ladder is bounded by
//     time and cancellation only. Then the failure goes back to the client
//     (or on to the egress and Azure fallbacks when configured).
//   - the failover ladder (SUBROUTER_CODEX_OVERLOAD_FAILOVER, a conversation
//     within the size cap): one same-account retry after a 250-750ms jittered
//     gap, then the overload failover to other accounts (100-400ms gaps), all
//     inside a ~10s budget and the request's attempt budget. Failed accounts
//     are marked at capacity for the model pool.
//
// The quick same-account retry is also what makes the capacity rule hold for
// the session that hit the error: it leaves only after its account shed it
// twice in a row -- the same CapacityStickyEvictFailures that releases every
// other sticky session there.
//
// Not ported: persist mode (SUBROUTER_CODEX_CAPACITY_RETRY=persist and the
// X-Subrouter-Capacity-Retry header), the X-Subrouter-Retry header
// (overload_retry.go says why), and the shedding tracker that shortens the
// failover budget to ~3s while a pool sheds. Client cancellation ends either
// ladder immediately.
const (
	// codexCapacityDefaultRetryBudget bounds the failover ladder.
	codexCapacityDefaultRetryBudget = 10 * time.Second
	// codexCapacityFallbackStayRetryBudget caps the same-account ladder
	// (codexCapacityDefaultStayMaxWait) when a regional egress or Azure
	// fallback is configured: the operator set those up to take over from a
	// shedding pool, so they get their turn sooner.
	codexCapacityFallbackStayRetryBudget = 10 * time.Second
	// codexCapacitySameAccountRetries is how many times the failover ladder
	// retries on the account that just shed the request before moving on.
	codexCapacitySameAccountRetries = 1
	// codexCapacityStayFirstGap and codexCapacityStayMaxGap shape the ramp
	// of the same-account ladder: the gap doubles from the first to the max
	// (or the interval, when smaller), then holds at the interval.
	codexCapacityStayFirstGap = 500 * time.Millisecond
	codexCapacityStayMaxGap   = 8 * time.Second
)

// stayPolicy is the same-account ladder's shape: the operator's StayInterval,
// StayMaxWait and StayUnbounded. Upstream layers persist mode and the
// client's X-Subrouter-Retry on top; the fork has neither, so its stay wait
// is never "explicit" and a configured fallback always caps it
// (codexStayBudget).
func (c *CodexOverloadFailoverConfig) stayPolicy() overloadRetryPolicy {
	if c == nil {
		return overloadRetryPolicy{}
	}
	return overloadRetryPolicy{interval: c.StayInterval, maxWait: c.StayMaxWait, unbounded: c.StayUnbounded}
}

// retryBudget is the default ladder's time budget: the configured one, else
// ~10s for the failover ladder and the operator's same-account cap (default
// 4m) without it. Zero means unbounded (StayUnbounded).
func (c *CodexOverloadFailoverConfig) retryBudget() time.Duration {
	if c != nil && c.RetryBudget > 0 {
		return c.RetryBudget
	}
	if c.enabled() {
		return codexCapacityDefaultRetryBudget
	}
	if c != nil && c.StayUnbounded {
		return 0
	}
	if c != nil && c.StayMaxWait > 0 {
		return c.StayMaxWait
	}
	return codexCapacityDefaultStayMaxWait
}

// stayDelay is the gap before same-account retry number retry (0-based) on
// the stay ladder: 0.5s doubling to 8s (the ramp capped at interval),
// jittered by +-20%, then interval jittered by +-10% (8-10s for the default
// 9s), so a burst of shed requests does not come back in lockstep.
func (c *CodexOverloadFailoverConfig) stayDelay(retry int, interval time.Duration) time.Duration {
	if c != nil && c.sameAccountGap != nil {
		return c.sameAccountGap()
	}
	if retry < 5 {
		return codexJitter(min(codexCapacityStayFirstGap<<retry, codexCapacityStayMaxGap, interval), 0.2)
	}
	return codexJitter(interval, 0.1)
}

func (c *CodexOverloadFailoverConfig) sameAccountDelay() time.Duration {
	if c != nil && c.sameAccountGap != nil {
		return c.sameAccountGap()
	}
	return 250*time.Millisecond + rand.N(500*time.Millisecond)
}

// codexOverloadSwitchDelay is the pause before retrying on the next account:
// 100-400ms, so a burst of failed requests does not stampede the accounts that
// are still healthy in the same instant the pool shed load. Upstream #373.
func codexOverloadSwitchDelay() time.Duration {
	return 100*time.Millisecond + rand.N(300*time.Millisecond)
}

func (c *CodexOverloadFailoverConfig) switchDelay() time.Duration {
	if c != nil && c.switchGap != nil {
		return c.switchGap()
	}
	return codexOverloadSwitchDelay()
}

// codexSleepContext waits d, returning false if the request ended first.
func codexSleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// codexFallbackConfigured reports whether an egress or Azure fallback runs
// after the capacity retry gives up.
func (s *Server) codexFallbackConfigured() bool {
	return s.CodexEgress.configured() || s.AzureCodex.configured()
}

// codexCapacityRetryBudget is the operator's default budget: the configured
// one, with the same-account ladder capped at ~10s when an egress or Azure
// fallback is configured to take over after it. Zero means unbounded.
func (s *Server) codexCapacityRetryBudget() time.Duration {
	budget := s.CodexOverloadFailover.retryBudget()
	if !s.CodexOverloadFailover.enabled() && s.codexFallbackConfigured() {
		if budget <= 0 {
			return codexCapacityFallbackStayRetryBudget
		}
		budget = min(budget, codexCapacityFallbackStayRetryBudget)
	}
	return budget
}

// codexDefaultRetryBudget is the failover ladder's budget for a request of
// this (model, tier) pool. Upstream shrinks it to ~3s while the pool is
// shedding (codexSheddingTracker); the fork has no tracker, so it is
// codexCapacityRetryBudget for every pool.
func (s *Server) codexDefaultRetryBudget(_, _ string) time.Duration {
	return s.codexCapacityRetryBudget()
}

// codexStayBudget is the same-account ladder's wall-clock budget for a
// request's stay policy, or unbounded. A configured fallback caps the
// operator default at ~10s (upstream exempts an explicit persist or
// X-Subrouter-Retry wait, neither of which the fork has).
func (s *Server) codexStayBudget(policy overloadRetryPolicy) (time.Duration, bool) {
	if c := s.CodexOverloadFailover; c != nil && c.RetryBudget > 0 {
		return c.RetryBudget, false
	}
	budget := policy.maxWaitOr(codexCapacityDefaultStayMaxWait)
	if s.codexFallbackConfigured() {
		if policy.unbounded {
			return codexCapacityFallbackStayRetryBudget, false
		}
		return min(budget, codexCapacityFallbackStayRetryBudget), false
	}
	return budget, policy.unbounded
}

// Reconnect capacity model (fork plumbing, not upstream's):
// capacity marks are per (model, tier) pool, and a websocket turn's model
// arrives in its response.create, after the upgrade was placed. Codex's
// upgrade carries no model (ExtractModel reads only X-Subrouter-Model /
// X-Model, and a GET has no body), so the reconnect after a 1012 capacity
// reroute read the marks of model "" -- none -- and the rule's second exit
// never fired for it: the session reconnected onto the shedding account
// every time, and after the 3-reroute budget its capacity failures reached
// the client. Upstream c9303d3 has the same gap. The reroute now records the
// model it marked against the session, and the reconnect's placement reads
// that model's marks. Only the marks: the quota pool model is untouched, and
// the rule's semantics (per-pool marks, two in a row, first success clears)
// are upstream's as written; this only lets the rule see the session.

// noteCapacityModel records the model pool a session's websocket reroute
// just marked.
func (r *codexOverloadReroutes) noteCapacityModel(key, model string) {
	if r == nil || key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, ok := r.entries[key]; ok {
		entry.model = model
		r.entries[key] = entry
	}
}

// capacityModel is the model the session's last websocket reroute marked,
// "" when it has none inside the reroute window.
func (r *codexOverloadReroutes) capacityModel(key string) string {
	if r == nil || key == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[key]
	if !ok || !entry.expiresAt.After(time.Now()) {
		return ""
	}
	return entry.model
}

// codexCapacityMarksModel is the model whose capacity marks a Codex
// placement reads: the request's own, or, for a websocket upgrade that names
// none, the model the session's last capacity reroute marked.
func (s Server) codexCapacityMarksModel(model, agentType, sessionID string, r *http.Request) string {
	if model != "" || r == nil || !websocket.IsWebSocketUpgrade(r) {
		return model
	}
	return s.codexOverloadRerouteCounts.capacityModel(azureCodexSessionKeyFor(agentType, sessionID))
}
