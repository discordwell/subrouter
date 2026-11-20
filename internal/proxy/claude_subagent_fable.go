package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// A subagent's Fable turn pins to Opus. Owner, 2026-11-20: "Ideally, it
// doesn't switch. It just pins to Opus. The only thing that forces you to
// move to a Fable box is if you're switching the main model from Opus [to]
// Fable."
//
// A subagent shares its session with the main thread, so moving the session
// for a subagent's Fable turn moves the main thread too, onto a cold cache.
// Measured 2026-11-20 (session 5b3cd8a6): the session was held on the box
// account sixteen (Fable 100 % spent, week 73 %); a subagent's Fable turn was
// refused there, the hold ended, the turn was re-ranked onto another box
// account with Fable left, and the main thread's next turn followed it there
// and resent its whole conversation.
//
// So a subagent's Fable turn in a session pinned to an account whose Fable
// meter is spent, but which still serves Opus, is served as Opus on that same
// account, and the pin stays. On a peer-held session (serveHeldAttempt) that
// is decided before sending, from the peer's cached per-pool verdicts, or
// after the peer refuses the Fable turn as exhausted. On a local pin
// (pinSubagentFableToOpus) the request is rewritten before selection, which
// then keeps the session on its account on the Opus pool. A main-thread Fable
// turn, and every other model's turn, routes as before.
//
// The rewrite is claudeOpusBody: only the top-level model changes, so the
// conversation's earlier Fable thinking blocks travel unchanged, as on every
// §11 substitution. Anthropic documents that Opus does not read a Fable
// thinking block and drops it from the prompt, unbilled, instead of failing
// the request, so the Opus turn runs without that reasoning. No test here
// exercises that; it is the API's behaviour, not this code's.

// claudeSubagentRequest reports a request Claude Code sent for a subagent. It
// sends X-Claude-Code-Agent-Id (with X-Claude-Code-Agent-Type, for example
// workflow-subagent) on every subagent request and neither on the main
// thread's (measured in captured traffic, 2026-11-20).
func claudeSubagentRequest(r *http.Request) bool {
	return r != nil && claudeSubagentID(r) != ""
}

func claudeSubagentID(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-Claude-Code-Agent-Id"))
}

const claudeSubagentFableAsOpusMessage = "claude subagent fable served as opus on pinned account"

// pinSubagentFableToOpus rewrites a subagent's Fable turn to Opus in place
// (rewriteRequestToClaudeOpus) when its session is pinned, warm, to a local
// account whose Fable pool is spent and whose Opus pool is not evicted. It
// reports whether it rewrote; the caller then routes the turn on the Opus
// pool. A session held on a peer is left to serveHeldTurn: its local pin is
// only a placeholder. A cold session has no cache to keep, and selection
// re-ranks it on any turn (P9), so its Fable turn goes where Fable is left.
func (s Server) pinSubagentFableToOpus(r *http.Request, agentType, sessionID string) bool {
	if s.Sessions == nil || !s.sessionWarm(agentType, sessionID) {
		return false
	}
	if _, _, held := s.peerHold(session.ScopedSessionKey(agentType, sessionID)); held {
		return false
	}
	assignment, ok := s.Sessions.Get(agentType, sessionID)
	if !ok {
		return false
	}
	account, ok := findAccount(filterAccountsForProvider(s.accountListContext(r.Context()), accounts.ProviderClaude), assignment.AccountID)
	if !ok || account.AuthMode != accounts.AuthModeOAuth {
		return false
	}
	scoreProvider := schedulerAccountProvider(account.Provider)
	if !s.selectionScheduler(agentclaude.FableFeature, "").Exhausted(scoreProvider, account.ID) ||
		s.selectionScheduler(agentclaude.OpusFeature, "").Evicted(scoreProvider, account.ID) {
		return false
	}
	from, to, _, ok := s.rewriteRequestToClaudeOpus(r)
	if !ok {
		return false
	}
	if s.Logger != nil {
		s.Logger.Info(claudeSubagentFableAsOpusMessage,
			"agent", agentType, "session", sessionID, "agent_id", claudeSubagentID(r),
			"account", account.ID, "path", "local_pin", "from_model", from, "to_model", to)
	}
	return true
}

// serveHeldAttempt is serveHeldTurn's one attempt on the held account. A
// subagent's Fable turn goes there as Opus when the peer's cached verdicts say
// the account's Fable pool is spent and its Opus pool serves ("pre_send"), or
// is sent once more as Opus when the peer refuses the Fable turn as exhausted
// ("after_refusal"). forwardToPeer ends the hold on a refusal; Opus on the
// same account is not a switch, so the hold that refusal ended is put back
// first. A refusal that ended no hold -- another turn of the session moved it
// while this one was in flight -- is not resent: the session lives elsewhere
// now. Any other refusal, and an Opus attempt that is refused too, is
// returned for serveHeldTurn to act on as before.
func (s Server) serveHeldAttempt(w http.ResponseWriter, r *http.Request, p *RemotePool, agentType, sessionID string, body []byte, mode remotePoolHoldMode, target string, snapshot AdminSnapshot, turn *remotePoolTurn) peerForwardOutcome {
	if !turn.subagentFable || target == "" || mode != remotePoolHoldSet {
		return s.serveOnePeerAttempt(w, r, p, agentType, sessionID, body, mode, target)
	}
	opusBody, from, to, ok := claudeOpusBody(body)
	if !ok {
		return s.serveOnePeerAttempt(w, r, p, agentType, sessionID, body, mode, target)
	}
	path := "pre_send"
	var out peerForwardOutcome
	if p.cachedFableSpentOpusServes(snapshot, target) {
		out = s.serveOnePeerAttempt(w, r, p, agentType, sessionID, opusBody, mode, target)
	} else {
		out = s.serveOnePeerAttempt(w, r, p, agentType, sessionID, body, mode, target)
		if out.Served || out.Refusal.Reason != selectacct.VerdictExhausted || out.endedHold == nil {
			return out
		}
		p.delegations.restore(session.ScopedSessionKey(agentType, sessionID), *out.endedHold)
		path = "after_refusal"
		out = s.serveOnePeerAttempt(w, r, p, agentType, sessionID, opusBody, mode, target)
	}
	if s.Logger != nil && !out.clientGone() {
		message := claudeSubagentFableAsOpusMessage
		fields := []any{"agent", agentType, "session", sessionID, "agent_id", claudeSubagentID(r),
			"remote", p.name(), "account", target, "path", path,
			"from_model", from, "to_model", to, "status", out.Status}
		if !out.Served {
			// serveHeldTurn now acts on this outcome as on any other.
			reason := out.Switch.Reason
			if out.Refusal.Reason != "" {
				reason = out.Refusal.Reason
			}
			message, fields = "claude subagent fable as opus not served on pinned account", append(fields, "reason", reason)
		}
		s.Logger.Info(message, fields...)
	}
	return out
}

// cachedFableSpentOpusServes reads a held account's two quota pools off the
// peer's cached usage body, trusted for as long as cachedHoldVerdict trusts
// it: true when the account's claudefable verdict is exhausted and its Opus
// pool (its claudeopus entry, else the account's own verdict, as
// decisionCandidates reads it) is not evicted. A body without a claudefable
// verdict, a stale body or an unmeasured row says nothing, and the Fable turn
// is sent as it is.
func (p *RemotePool) cachedFableSpentOpusServes(snapshot AdminSnapshot, account string) bool {
	if len(snapshot.Body) == 0 || snapshot.At.IsZero() || time.Since(snapshot.At) > remotePoolHoldVerdictMaxAge {
		return false
	}
	var rows []remotePoolAccountUsage
	if json.Unmarshal(snapshot.Body, &rows) != nil {
		return false
	}
	for _, row := range rows {
		if row.Provider != string(accounts.ProviderClaude) || row.ID != account {
			continue
		}
		if row.unmeasured() || row.knownInvalid() {
			return false
		}
		fable, ok := row.Normalized.Pools[selectacct.ModelKey(agentclaude.FableFeature)]
		if !ok || fable.NewSession == nil || fable.NewSession.Reason != selectacct.VerdictExhausted {
			return false
		}
		opus := row.Normalized
		if pool, ok := row.Normalized.Pools[selectacct.ModelKey(agentclaude.OpusFeature)]; ok {
			opus = inheritPeerAccountFacts(pool, row.Normalized)
		}
		return opus.NewSession != nil && !opus.NewSession.Evicted
	}
	return false
}
