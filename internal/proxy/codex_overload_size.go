package proxy

import (
	"net/http"

	"github.com/discordwell/subrouter/selectacct"
	"github.com/discordwell/subrouter/session"
)

// The overload failover's size cap, ported from upstream manaflow-ai/subrouter
// c9303d3 (#454 body size, #457 the cap) as part of "the model at capacity
// rule as written" (PATCHES section 27). A capacity failure's cure is a switch
// to an account that is not shedding, but the session's prompt cache lives on
// its account: a switch re-bills the whole conversation as uncached input.
// Re-billing 32k tokens costs about one quick turn; past that the cache a
// switch throws away costs more than waiting the shedding out. So, with the
// failover on, a conversation estimated above FailoverMaxInput (default 32k
// input tokens) is treated as if the failover were off, at all three points
// where the failover would move it:
//
//   - the HTTP transport neither marks its account nor retries it elsewhere:
//     it waits on its account, the failover-off stay ladder (#389,
//     codexOverloadFailoverTransport.RoundTrip);
//   - the websocket neither marks nor closes 1012 on its capacity failure
//     (codexOverloadWebSocketReroute);
//   - at placement, marks OTHER sessions left on its account do not evict it
//     (codexStickyScheduler): reuse and the capacity keep check see a
//     mark-free scheduler, while any account picked for it is still ranked
//     with the marks.
//
// This is the same cache argument as the owner's hour-long warm rule, and it
// narrows the owner-approved capacity exit to conversations cheap enough to
// move. Estimates: an HTTP turn carries the whole conversation, so its decoded
// body length / 4; a websocket turn chained with previous_response_id sends
// only its new input, so the connection keeps the largest response.create and
// the input tokens its finished turns reported, and uses the larger.

const (
	// codexOverloadDefaultFailoverMaxInput is the default size, in input
	// tokens, above which a capacity failure stays on the session's account.
	codexOverloadDefaultFailoverMaxInput = 32_000
	// codexInputBytesPerToken turns a body length into a token estimate.
	// About 4 bytes per token for English text and JSON; code and base64
	// reasoning payloads run somewhat off that, which is close enough for a
	// threshold between sessions that differ by 10x or more.
	codexInputBytesPerToken = 4
)

// failoverMaxInput is the input-token cap on a failover switch, zero when
// unlimited.
func (c *CodexOverloadFailoverConfig) failoverMaxInput() int64 {
	if c == nil || c.FailoverMaxInputUnlimited {
		return 0
	}
	if c.FailoverMaxInput > 0 {
		return c.FailoverMaxInput
	}
	return codexOverloadDefaultFailoverMaxInput
}

// failoverKeepsAccount reports whether the failover is on but a
// conversation of about estimate input tokens is too large to move: its
// capacity failures then take the failover-off path, keeping its prompt
// cache. An unknown estimate (zero) may switch, as before the cap existed.
func (c *CodexOverloadFailoverConfig) failoverKeepsAccount(estimate int64) bool {
	limit := c.failoverMaxInput()
	return c.enabled() && limit > 0 && estimate > limit
}

// codexInputTokensFromBytes estimates input tokens from a decoded request
// body or response.create length.
func codexInputTokensFromBytes(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return (n + codexInputBytesPerToken - 1) / codexInputBytesPerToken
}

// logFailoverKeptAccount records one capacity failure (or, at placement, one
// capacity eviction) that did not move a conversation because of its size.
func (s *Server) logFailoverKeptAccount(transport, agent, sessionID, accountID string, estimate int64) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Warn("codex overload failover skipped for a large conversation; keeping its account",
		"transport", transport, "agent", agent, "session", sessionID, "account", accountID,
		"estimated_input_tokens", estimate, "max_input_tokens", s.CodexOverloadFailover.failoverMaxInput())
}

// codexStickyScheduler is the scheduler that decides whether a sticky Codex
// session leaves its account (reuseStickyAssignment and
// keepCapacityEvictedSession). Normally the marked selection scheduler; for a
// conversation too large for the failover to move, the same scheduler with
// no capacity marks, so marks left by smaller sessions cannot evict it. The
// estimate is returned when that happened (zero otherwise), for the log.
// Upstream c9303d3 proxy.go:7400-7415 (stickyScheduler).
func (s Server) codexStickyScheduler(r *http.Request, scheduler selectacct.Scheduler) (selectacct.Scheduler, int64) {
	if !s.CodexOverloadFailover.enabled() || r == nil {
		return scheduler, 0
	}
	estimate := codexInputTokensFromBytes(session.ExtractBodySize(r, s.MaxBodyBytes))
	if !s.CodexOverloadFailover.failoverKeepsAccount(estimate) {
		return scheduler, 0
	}
	return scheduler.WithCapacityMarks(nil), estimate
}

func (s *webSocketModelState) noteInputTokens(tokens int64) {
	if tokens <= 0 {
		return
	}
	s.mu.Lock()
	s.inputTokens = tokens
	s.mu.Unlock()
}

// inputTokenEstimate is the connection's conversation size in input tokens,
// zero when nothing is known yet.
func (s *webSocketModelState) inputTokenEstimate() int64 {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return max(s.inputTokens, codexInputTokensFromBytes(s.requestBytes))
}
