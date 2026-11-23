package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/session"
)

// claudeSubstituteOpusModel is the model a Fable request falls to once the
// Fable pool is spent: the newest Opus, whichever Fable version was asked for.
// The families do not release in lockstep (Fable 5.1 and 5.2 have no
// same-numbered Opus), so matching the version number lands on an older Opus
// at best and on an id Anthropic 404s at worst, and this stage is the last
// rescue the session has.
const claudeSubstituteOpusModel = "claude-opus-5-5"

// claudeOpusModel maps a Fable request model to claudeSubstituteOpusModel,
// keeping the client's bracketed context-window suffix verbatim: dropping
// "[1m]" silently downgrades a 1M-context session to the 200k default. ok is
// false when the model names no Fable family.
func claudeOpusModel(model string) (string, bool) {
	if !claudeFableModel(model) {
		return "", false
	}
	suffix := ""
	if i := strings.IndexByte(model, '['); i >= 0 {
		suffix = model[i:]
	}
	return claudeSubstituteOpusModel + suffix, true
}

// claudeOpusBody rewrites the top-level "model" field of a Claude Messages
// request body to the substitute Opus model. Everything else is preserved
// byte-for-byte (values stay json.RawMessage), including context_management,
// which the OAuth pool accepts and must not be stripped the way the direct
// API-key path strips it.
//
// Unlike stripClaudeUnsupportedFields this reports failure instead of silently
// returning the original: a body that is not a JSON object, has no top-level
// "model", holds a non-string model, or is compressed must NOT be substituted -
// sending a claude-fable-5 body to an Opus-selected account would be a silent
// mis-route. Every caller treats ok==false as "do not substitute".
func claudeOpusBody(body []byte) ([]byte, string, string, bool) {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return nil, "", "", false
	}
	raw, present := payload["model"]
	if !present {
		return nil, "", "", false
	}
	var from string
	if json.Unmarshal(raw, &from) != nil {
		return nil, "", "", false
	}
	to, ok := claudeOpusModel(from)
	if !ok {
		return nil, "", "", false
	}
	encoded, err := json.Marshal(to)
	if err != nil {
		return nil, "", "", false
	}
	payload["model"] = encoded
	rebuilt, err := json.Marshal(payload)
	if err != nil {
		return nil, "", "", false
	}
	return rebuilt, from, to, true
}

// rewriteRequestToClaudeOpus retargets an in-flight handler request at the Opus
// model so account selection re-runs against the Opus quota pool. Body,
// ContentLength and GetBody are restored together (the triple
// serveClaudeFableBedrockPrimary uses): makeRequestBodyReplayable short-circuits
// on a non-nil GetBody, so replacing only Body would send the old bytes on the
// first attempt and the new bytes on every retry.
//
// undo puts back everything the rewrite changed -- the body bytes,
// ContentLength, GetBody and X-Subrouter-Model -- and is for the caller whose
// Opus selection then fails: the request goes on to the peer chain, which ranks
// the peers on the FABLE pool and must forward what the client sent, not the
// Opus rewrite (PATCHES section 50). It is nil when ok is false; the request is
// already as it was.
func (s Server) rewriteRequestToClaudeOpus(r *http.Request) (from, to string, undo func(), ok bool) {
	if r == nil || r.Body == nil {
		return "", "", nil, false
	}
	priorContentLength, priorGetBody := r.ContentLength, r.GetBody
	priorModelHeader, hadModelHeader := r.Header["X-Subrouter-Model"]
	priorModelHeader = append([]string(nil), priorModelHeader...)
	body, err := io.ReadAll(io.LimitReader(r.Body, replayablePostMaxBodyBytes))
	if err != nil {
		return "", "", nil, false
	}
	restore := func(b []byte) {
		r.Body = io.NopCloser(bytes.NewReader(b))
		r.ContentLength = int64(len(b))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
	}
	rebuilt, from, to, ok := claudeOpusBody(body)
	if !ok {
		restore(body)
		return "", "", nil, false
	}
	restore(rebuilt)
	// ExtractModel reads headers BEFORE the body, so rewriting only the JSON
	// would leave every downstream ExtractModel - including the claudePoolModel
	// inside accountForSessionProviderWithOptions - still reporting Fable
	// whenever the client sets X-Subrouter-Model or X-Model. StripSubrouterHeaders
	// deletes both before the request is forwarded, so this never reaches
	// Anthropic.
	r.Header.Set("X-Subrouter-Model", to)
	undo = func() {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength, r.GetBody = priorContentLength, priorGetBody
		if hadModelHeader {
			r.Header["X-Subrouter-Model"] = priorModelHeader
		} else {
			r.Header.Del("X-Subrouter-Model")
		}
	}
	return from, to, undo, true
}

// onceOpusSubstitute bounds an Opus substitution to a single attempt per client
// request. replayablePostRetryTransport wraps usageLimitRetryTransport on the
// OUTSIDE, so a network error replays the whole RoundTrip on the same transport
// value; without this one client request could burn several Opus attempts.
func onceOpusSubstitute(substitute func(reason string) (*http.Response, bool)) func(string) (*http.Response, bool) {
	var once sync.Once
	return func(reason string) (*http.Response, bool) {
		first := false
		once.Do(func() { first = true })
		if !first {
			return nil, false
		}
		return substitute(reason)
	}
}

// claudeOpusSubstituteResponse is the last stage of the Fable fallback chain: it
// re-serves a Fable request from the Opus quota pool on the same subscription
// once the Fable pool has given up and the Bedrock/API-key stages declined or
// are unconfigured. Anthropic meters Fable in its own weekly bucket, so a
// subscription whose Fable quota is spent routinely still has 5h and 7d headroom
// that only Opus can reach.
//
// Capacity is not predicted: oauthRetryAccount under the Opus feature pool is
// the codebase's existing hard gate (Headroom and ShortHeadroom both above zero,
// i.e. weekly AND 5h quota remaining), and a decline there is the answer "no
// capacity". The Opus attempt then only replaces the pool's response on a clean
// 2xx, so every other outcome leaves the caller's original error intact.
//
// It round-trips on the base outbound transport, never on
// usageLimitRetryTransport, which makes re-entry structurally impossible.
//
// The pick is provisional, like every failover pick: the session moves to the
// Opus account only once that account has answered (commitSuccessfulHTTPResponse,
// on the stream's terminal event), by compare-and-swap from the pin the turn
// started from. It used to be written before the request went out, so a decline
// -- or a peer serving the turn instead -- left the session pinned to an account
// that never answered (upstream a3745cc; PATCHES section 44).
func (s Server) claudeOpusSubstituteResponse(r *http.Request, agentType, sessionID, userEmail, fableAccountID, reason string) (*http.Response, bool) {
	return s.claudeOpusSubstituteResponseAcross(r, agentType, sessionID, userEmail, fableAccountID, fableAccountID, reason, nil)
}

// claudeOpusSubstituteResponseAcross is claudeOpusSubstituteResponse on a turn
// that may go to a peer. The Fable window the session was on is spent -- a
// FORCED switch for the model (P8) -- so the Opus account is chosen by the
// one ranking (D7), on the Opus pool: crossPool ranks the local Opus pick
// against every peer account's Opus pool (forcedSwitchAcrossPools) and, when
// a peer account wins, serves the rewritten Opus body there, targeted. False
// means the local pick serves, as before. nil is a turn that may not leave.
//
// expectedAccountID is the session's pin as this turn found it: the account a
// successful local Opus answer moves the session from.
func (s Server) claudeOpusSubstituteResponseAcross(r *http.Request, agentType, sessionID, userEmail, fableAccountID, expectedAccountID, reason string,
	crossPool func(opusRequest *http.Request, next accounts.Account) (*http.Response, bool)) (*http.Response, bool) {
	if r == nil || r.GetBody == nil {
		return nil, false
	}
	rc, err := r.GetBody()
	if err != nil {
		return nil, false
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return nil, false
	}
	rebuilt, from, to, ok := claudeOpusBody(body)
	if !ok {
		if s.Logger != nil {
			s.Logger.Warn("claude opus substitution skipped: no rewritable model in request body",
				"agent", agentType, "session", sessionID, "reason", reason)
		}
		return nil, false
	}
	// A fresh tried set on purpose: the exhaustion marks the Fable failover just
	// wrote are scoped to the Fable pool, so every account is a legitimate Opus
	// candidate again. Nothing is written here; see the commit below.
	account, err := s.oauthRetryCandidate(
		r.Context(), accounts.ProviderClaude, agentType, sessionID, userEmail,
		agentclaude.OpusFeature, map[string]struct{}{}, true, false,
	)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Warn("claude fable pool exhausted and opus pool has no capacity; returning the pool's own error",
				"agent", agentType, "session", sessionID, "account", fableAccountID,
				"reason", reason, "error", err)
		}
		return nil, false
	}
	if crossPool != nil {
		opusRequest := r.Clone(r.Context())
		opusRequest.Body = io.NopCloser(bytes.NewReader(rebuilt))
		opusRequest.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(rebuilt)), nil }
		opusRequest.ContentLength = int64(len(rebuilt))
		if resp, ok := crossPool(opusRequest, account); ok {
			if s.Logger != nil {
				s.Logger.Warn("substituting claude opus for exhausted fable pool",
					"agent", agentType, "session", sessionID, "account", account.ID,
					"previous_account", fableAccountID,
					"from_model", from, "to_model", to,
					"stage", "transport", "reason", reason, "status", resp.StatusCode,
					"served_by", "peer")
			}
			return resp, true
		}
	}
	// Live debit: this stage sends a full completion to the picked Opus account,
	// so the scheduler must see that traffic draining the snapshot until the next
	// usage refresh. Debit keys are account-wide, so an unrecorded substitution
	// leaves the account looking unloaded to every other pool as well.
	s.SchedulerRef.NoteRouted(accounts.ProviderClaude, account.ID)
	upstream := s.upstreamForRequest(r.URL.Path, account)
	if upstream == nil {
		return nil, false
	}
	target := *upstream
	target.Path = strings.TrimRight(upstream.Path, "/") + r.URL.Path
	target.RawQuery = r.URL.RawQuery
	outReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target.String(), bytes.NewReader(rebuilt))
	if err != nil {
		return nil, false
	}
	for key, values := range r.Header {
		lower := strings.ToLower(key)
		if isHopByHopHeader(key) || lower == "host" || lower == "content-length" {
			continue
		}
		for _, value := range values {
			outReq.Header.Add(key, value)
		}
	}
	// Belt-and-braces: proxyRequest was already stripped in the handler, but this
	// keeps the routing hint from leaking should the helper ever be handed a
	// handler-side request. The delegated pair is then restored the same way the
	// handler restores it, so a Subrouter-to-Subrouter hop keeps its stickiness
	// instead of losing it only on substituted requests.
	session.StripSubrouterHeaders(outReq.Header)
	s.setDelegatedSessionHeaders(outReq.Header, agentType, sessionID)
	stripOutboundForwardingHeaders(outReq.Header)
	stripClientAcceptEncoding(outReq.Header)
	setAccountAuthHeaders(outReq.Header, account, to)
	outReq.Host = target.Host
	outReq.ContentLength = int64(len(rebuilt))
	outReq.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(rebuilt)), nil }

	resp, err := s.transport().RoundTrip(outReq)
	if err != nil {
		if s.Logger != nil && r.Context().Err() == nil {
			s.Logger.Warn("claude opus substitution request failed",
				"agent", agentType, "session", sessionID, "account", account.ID,
				"reason", reason, "error", err)
		}
		return nil, false
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || claudeResponseRejected(resp.Header) {
		// Anything other than a clean 2xx means the substitution did not help.
		// Declining here surfaces the pool's original error unchanged, degrades a
		// stale Opus model id to today's behavior instead of leaking a 404 to the
		// client, and guarantees the response flowing back into ModifyResponse can
		// never be marked unusable - so an Opus rejection cannot poison the FABLE
		// pool score that captureResponseBody is closed over.
		if s.Logger != nil {
			s.Logger.Warn("claude opus substitution declined by upstream; returning the pool's own error",
				"agent", agentType, "session", sessionID, "account", account.ID,
				"to_model", to, "reason", reason,
				"status", resp.StatusCode, "rejected", claudeResponseRejected(resp.Header))
		}
		_ = resp.Body.Close()
		return nil, false
	}
	// The Fable window is spent, a forced switch (P8): the session follows the
	// account that serves it (§25), but only once the answer completes.
	if err := s.commitSuccessfulHTTPResponse(resp, agentType, sessionID, expectedAccountID, account.ID, userEmail); err != nil {
		if s.Logger != nil {
			s.Logger.Warn("claude opus substitution could not record the session move; returning the pool's own error",
				"agent", agentType, "session", sessionID, "account", account.ID, "error", err)
		}
		_ = resp.Body.Close()
		return nil, false
	}
	if s.Logger != nil {
		s.Logger.Warn("substituting claude opus for exhausted fable pool",
			"agent", agentType, "session", sessionID, "account", account.ID,
			"previous_account", fableAccountID,
			"from_model", from, "to_model", to,
			"stage", "transport", "reason", reason, "status", resp.StatusCode)
	}
	return resp, true
}
