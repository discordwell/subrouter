package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
	"github.com/discordwell/subrouter/session"
)

// Codex capacity marks: the proxy half of selectacct/capacity.go (PATCHES
// section 27). Ported as written from upstream manaflow-ai/subrouter 862a5a5
// (#382; the same code at c9303d3), with the failover's quick same-account
// retry (codex_capacity_retry.go) and the size cap (#454/#457,
// codex_overload_size.go) since fix-up 1, and #373's detection (the peeked
// failure payload read below) and #389's same-account ladder since the
// 389/373 port, minus what rides on machinery the fork does not have --
// persist mode and the shedding tracker. The pieces here are plumbing; the
// hooks are one-liners at the detection points, the selection scheduler, the
// retry picker and the sticky branch.
//
// Marks are written only where a capacity failure is already detected with
// the overload failover on: the HTTP failover transport, the websocket 1012
// reroute and a websocket failure after visible output. That is upstream's
// gate too -- a mark steers placement and, repeated, evicts sticky sessions,
// which is an account switch, and switching is the failover's. Upstream makes
// it opt-in; the fork ships it on (SUBROUTER_CODEX_OVERLOAD_FAILOVER=0 turns
// it off, cmd/subrouter codexOverloadFailoverEnabled). With the failover off
// no capacity mark is written. The rate-limit mark below is not one: it only
// ranks and never evicts, so the usage-limit failover writes it on a Codex
// burst 429 whatever this switch says (PATCHES §35.3).

const (
	codexCapacityMarkMinTTL = 30 * time.Second
	codexCapacityMarkMaxTTL = 5 * time.Minute
	// codexCapacityMarkJitter spreads the moment a crowd of marked accounts
	// re-enters routing, so they do not all hit the pool again together.
	codexCapacityMarkJitter = 0.2
)

// codexCapacityRetryHint returns how long the upstream asked callers to stay
// away, or zero: Retry-After-Ms or Retry-After headers first, then the
// failure payload the classifier kept (retry_after_ms, retry_after,
// resets_in_seconds at any level). It never reads the body. The payload is
// #373's codexPeekedBody: the stream peek keeps the failure event, and
// codexCapacityBody keeps a non-stream error body (including a 5xx's, peeked
// only for this hint).
func codexCapacityRetryHint(response *http.Response) time.Duration {
	if response == nil {
		return 0
	}
	if raw := strings.TrimSpace(response.Header.Get("Retry-After-Ms")); raw != "" {
		if ms, err := strconv.ParseFloat(raw, 64); err == nil && ms > 0 {
			return time.Duration(ms * float64(time.Millisecond))
		}
	}
	now := time.Now()
	if until := parseRetryAfter(strings.TrimSpace(response.Header.Get("Retry-After")), now); !until.IsZero() {
		return until.Sub(now)
	}
	if peeked, ok := response.Body.(*codexPeekedBody); ok {
		return codexRetryHintJSON(peeked.payload)
	}
	return 0
}

// codexRetryHintJSON digs a retry hint out of a failure payload: top level,
// error, response or response.error.
func codexRetryHintJSON(payload []byte) time.Duration {
	if len(payload) == 0 {
		return 0
	}
	var event map[string]any
	if json.Unmarshal(payload, &event) != nil {
		return 0
	}
	return codexRetryHintMap(event, 0)
}

func codexRetryHintMap(event map[string]any, depth int) time.Duration {
	if event == nil || depth > 3 {
		return 0
	}
	if ms, ok := numberField(event, "retry_after_ms"); ok && ms > 0 {
		return time.Duration(ms * float64(time.Millisecond))
	}
	for _, key := range []string{"retry_after", "resets_in_seconds"} {
		if seconds, ok := numberField(event, key); ok && seconds > 0 {
			return time.Duration(seconds * float64(time.Second))
		}
		if raw, ok := event[key].(string); ok {
			if seconds, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(raw), "s"), 64); err == nil && seconds > 0 {
				return time.Duration(seconds * float64(time.Second))
			}
		}
	}
	for _, key := range []string{"error", "response"} {
		if nested, ok := event[key].(map[string]any); ok {
			if hint := codexRetryHintMap(nested, depth+1); hint > 0 {
				return hint
			}
		}
	}
	return 0
}

// capacityMarkTTL is how long a capacity-failed account stays marked: the
// upstream's hint clamped to [30s, 5m], else the configured TTL, either way
// jittered by +-20%.
func (c *CodexOverloadFailoverConfig) capacityMarkTTL(hint time.Duration) time.Duration {
	if hint <= 0 {
		return codexJitter(c.markTTL(), codexCapacityMarkJitter)
	}
	ttl := codexJitter(hint, codexCapacityMarkJitter)
	return min(max(ttl, codexCapacityMarkMinTTL), codexCapacityMarkMaxTTL)
}

func codexJitter(value time.Duration, fraction float64) time.Duration {
	return time.Duration(float64(value) * (1 - fraction + 2*fraction*rand.Float64()))
}

// clearAccountCapacity drops the account's capacity marks for the model
// after it served the model successfully.
func (s *Server) clearAccountCapacity(accountID, model string) {
	if s == nil || s.SchedulerRef == nil || accountID == "" {
		return
	}
	if s.SchedulerRef.ClearCapacity(accounts.ProviderCodex, accountID, model) && s.Logger != nil {
		s.Logger.Info("codex account served the model again; capacity mark cleared", "account", accountID, "model", model)
	}
}

// markCodexAccountRateLimited records a Codex 429 that names no usage limit
// (a burst) as a rate-limit mark: the account ranks after unmarked ones for
// new placements and failover replacements until the mark expires, and no
// session moves for it (selectacct/rate_limit_marks.go, PATCHES §35.3). The
// mark lasts the upstream's retry hint when it gives one, clamped like a
// capacity mark's to [30s, 5m], else the 30 s floor.
func (s Server) markCodexAccountRateLimited(accountID string, response *http.Response) {
	if s.SchedulerRef == nil || accountID == "" {
		return
	}
	hint := codexCapacityRetryHint(response)
	if hint <= 0 {
		hint = codexRetryHintJSON(peekResponseBodyPrefix(response))
	}
	ttl := min(max(hint, codexCapacityMarkMinTTL), codexCapacityMarkMaxTTL)
	until := time.Now().Add(ttl)
	s.SchedulerRef.MarkRateLimitedUntil(accounts.ProviderCodex, accountID, until)
	if s.Logger != nil {
		s.Logger.Info("codex 429 names no usage limit; account ranked last for new placements until the mark expires",
			"account", accountID, "until", until.UTC().Format(time.RFC3339), "ttl", ttl.String())
	}
}

// withCapacityMarks attaches the Codex capacity marks for the request's
// (model, tier) pool to a scheduler, with the rate-limit marks folded in
// (SchedulerRef.RankMarks). tier may be selectacct.CapacityAnyTier.
// Any other provider gets the scheduler back unchanged: Claude overload keeps
// its same-account wait (claude_overload.go) and is never marked.
func (s Server) withCapacityMarks(scheduler selectacct.Scheduler, provider accounts.Provider, model, tier string) selectacct.Scheduler {
	if s.SchedulerRef == nil || accountProviderFor(provider) != accounts.ProviderCodex {
		return scheduler
	}
	marks := s.SchedulerRef.RankMarks(accounts.ProviderCodex, model, tier)
	if len(marks) == 0 {
		return scheduler
	}
	return scheduler.WithCapacityMarks(marks)
}

// codexCapacitySelectionTier is the capacity pool tier for placing a
// request: the body's service_tier, or every tier for a websocket upgrade,
// whose turns name their tier only after the connection is up.
func codexCapacitySelectionTier(r *http.Request, maxBodyBytes int64) string {
	if r == nil || websocket.IsWebSocketUpgrade(r) {
		return selectacct.CapacityAnyTier
	}
	return session.ExtractServiceTier(r, maxBodyBytes)
}

type codexServiceTierContextKey struct{}

// withCodexServiceTier carries the request's service tier to account
// pickers that only receive a context (oauthRetryCandidate).
func withCodexServiceTier(ctx context.Context, tier string) context.Context {
	return context.WithValue(ctx, codexServiceTierContextKey{}, tier)
}

// codexServiceTierFromContext is the tier withCodexServiceTier stored, or
// CapacityAnyTier when the caller did not know it.
func codexServiceTierFromContext(ctx context.Context) string {
	if ctx != nil {
		if tier, ok := ctx.Value(codexServiceTierContextKey{}).(string); ok {
			return tier
		}
	}
	return selectacct.CapacityAnyTier
}

// keepCapacityEvictedSession is the capacity half of upstream's
// keepConstrainedStickyAssignment (862a5a5), the only half ported. It runs
// when reuseStickyAssignment released a Codex OAuth session because its
// account kept shedding the request's pool (CapacityEvicting), and reports
// whether the session should stay anyway: leaving costs the prompt cache, so
// it is worth it only for an account that is not shedding this pool too and
// can itself hold the session. With nothing like that in the pool the
// session keeps its account, cache and all, and the overload failover deals
// with the turn.
//
// Two deliberate differences from upstream, both in the direction of the
// owner's rules:
//
//   - The guard is !Evicted, not !Exhausted. An account inside its owner's
//     holdout is Evicted but not Exhausted; upstream's guard would keep a
//     capacity-evicted session there, and no session may stay in a holdout
//     (PATCHES section 20).
//   - "Can hold the session" is UsableForStickySession, which in the fork is
//     the new-session eligibility predicate on the measured score, not
//     upstream's 5% retention floor. The retention floor and upstream's +10pp
//     headroom-gain gate (the rest of keepConstrainedStickyAssignment) are not
//     ported: the hour-long warm rule already decides retention.
//
// It picks for itself so the call site stays a one-liner beside
// reuseStickyAssignment; Pick is deterministic, so the move path that runs
// when this returns false names the same candidate.
func (s Server) keepCapacityEvictedSession(agentType, sessionID string, current accounts.Account, scheduler selectacct.Scheduler, availableAccounts []accounts.Account) bool {
	if accountProviderOrCodex(current) != accounts.ProviderCodex || current.AuthMode != accounts.AuthModeOAuth {
		return false
	}
	currentProvider := schedulerAccountProvider(current.Provider)
	if !scheduler.CapacityEvicting(currentProvider, current.ID) || scheduler.Evicted(currentProvider, current.ID) {
		return false
	}
	picked, err := pickRoutingAccount(scheduler, availableAccounts)
	if err != nil {
		return false
	}
	pickedProvider := schedulerAccountProvider(picked.Provider)
	keep := picked.ID == current.ID ||
		scheduler.CapacityFailures(pickedProvider, picked.ID) > 0 ||
		!scheduler.UsableForStickySession(pickedProvider, picked.ID)
	if keep && s.Logger != nil {
		s.Logger.Info("keeping sticky session on constrained account; no materially better account",
			"agent", agentType,
			"session", sessionID,
			"account", current.ID,
			"candidate", picked.ID,
			"active", s.activeSession(agentType, sessionID),
			"exhausted", scheduler.Exhausted(currentProvider, current.ID),
			"capacity_failures", scheduler.CapacityFailures(currentProvider, current.ID),
			"candidate_capacity_failures", scheduler.CapacityFailures(pickedProvider, picked.ID),
		)
	}
	return keep
}

// codexWebSocketRequestServiceTier reads service_tier from a response.create
// event, top level or under response.
func codexWebSocketRequestServiceTier(body []byte) string {
	var event map[string]any
	if err := json.Unmarshal(body, &event); err != nil {
		return ""
	}
	if tier := stringField(event, "service_tier"); tier != "" {
		return strings.ToLower(strings.TrimSpace(tier))
	}
	response, _ := event["response"].(map[string]any)
	return strings.ToLower(strings.TrimSpace(stringField(response, "service_tier")))
}

// webSocketTurnModel is the model of the turn in flight, for capacity
// marks: the response.create's model, else the connection's.
func webSocketTurnModel(state *webSocketModelState, fallback string) string {
	if model := state.current(); model != "" {
		return model
	}
	return fallback
}

// codexWebSocketResponseCompleted reports a successfully finished turn.
func codexWebSocketResponseCompleted(body []byte) bool {
	if !bytes.Contains(body, []byte("response.completed")) {
		return false
	}
	var event map[string]any
	if err := json.Unmarshal(body, &event); err != nil {
		return false
	}
	return strings.EqualFold(stringField(event, "type"), "response.completed")
}
