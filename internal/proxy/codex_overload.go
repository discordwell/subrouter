package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// CodexOverloadFailoverConfig shapes the Codex capacity retry. Measured
// 2026-09-09/10 across five regions and four hours: whether gpt-6-astra
// "fast" returns server_is_overloaded is a stable property of the ACCOUNT
// (some accounts never fail, most fail about half the time under load), not
// of the region or the hour. So the retry most likely to clear the error is
// the same request on a different account. But the session's prompt cache
// lives on its account, and a switch re-bills the whole conversation, so the
// switch is gated (Enabled, and the size cap below).
//
// A nil or disabled config still retries capacity failures on the session's
// own account (the stay ladder, codex_capacity_retry.go; upstream #389);
// Enabled adds the account failover, the capacity marks and the websocket
// reroute. Before the 389/373 port a disabled config meant nothing here ran.
type CodexOverloadFailoverConfig struct {
	// Enabled turns on switching accounts on capacity failures
	// (SUBROUTER_CODEX_OVERLOAD_FAILOVER).
	Enabled bool
	// MaxAccounts bounds how many further accounts one request may try.
	MaxAccounts int
	// MarkTTL is how long a capacity mark lasts on the (model, tier) pool
	// that failed when the upstream sent no retry hint (jittered +-20%). The
	// mark ranks the account last for that pool and, repeated, releases its
	// sticky sessions (selectacct/capacity.go). Short: overload is a
	// probability, not a state.
	MarkTTL time.Duration
	// RetryBudget overrides the default capacity retry policy's wall-time
	// budget, same-account or failover ladder alike. Zero means StayMaxWait
	// for the same-account ladder, 10s with the failover.
	RetryBudget time.Duration
	// StayInterval is the same-account ladder's steady gap after its ramp,
	// which is capped at it (SUBROUTER_CODEX_CAPACITY_RETRY_INTERVAL). Zero
	// means ~9s.
	StayInterval time.Duration
	// StayMaxWait caps the same-account ladder in wall-clock time from the
	// first attempt (SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT). Zero means 4m;
	// see StayUnbounded.
	StayMaxWait time.Duration
	// StayUnbounded removes that cap
	// (SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT=0): the request waits until
	// the client disconnects.
	StayUnbounded bool
	// FailoverMaxInput caps, in estimated input tokens, the conversation the
	// failover may move (SUBROUTER_CODEX_OVERLOAD_FAILOVER_MAX_INPUT). A
	// larger one keeps its account and takes the same-account ladder, as if
	// the failover were off: no switch, no capacity mark, no websocket
	// reroute, and marks other sessions left on its account do not evict it.
	// A switch would re-bill its whole cached prefix. Zero means 32k; see
	// FailoverMaxInputUnlimited. Upstream c9303d3 (#454/#457),
	// codex_overload_size.go.
	FailoverMaxInput int64
	// FailoverMaxInputUnlimited removes that cap
	// (SUBROUTER_CODEX_OVERLOAD_FAILOVER_MAX_INPUT=0): every conversation may
	// switch, whatever its size.
	FailoverMaxInputUnlimited bool

	// Test seams for the jittered gaps (codex_capacity_retry.go); nil uses
	// the real jitter. sameAccountGap also replaces every stay-ladder gap.
	sameAccountGap func() time.Duration
	switchGap      func() time.Duration
	// stayRetryLimit, when positive, bounds the same-account ladder by count
	// so tests can assert exact attempt counts; production has no count
	// bound, only the wall-clock cap and cancellation.
	stayRetryLimit int
}

const (
	codexOverloadDefaultMaxAccounts = 3
	codexOverloadDefaultMarkTTL     = 2 * time.Minute
	// codexCapacityTargetedRetryBudget bounds a targeted turn's same-account
	// ladder: a third of remotePoolResponseHeaderTimeout, the failover
	// ladder's own ~10s, so the answer (served or stamped overload) reaches
	// the decider well before it gives up on the peer.
	codexCapacityTargetedRetryBudget = remotePoolResponseHeaderTimeout / 3
	// codexOverloadMaxWebSocketReroutes bounds 1012 reconnect storms for one
	// session; past it the websocket path falls through to the egress and
	// Azure diverts.
	codexOverloadMaxWebSocketReroutes = 3
	codexOverloadRerouteWindow        = 10 * time.Minute
)

func (c *CodexOverloadFailoverConfig) enabled() bool {
	return c != nil && c.Enabled
}

func (c *CodexOverloadFailoverConfig) maxAccounts() int {
	if c == nil || c.MaxAccounts <= 0 {
		return codexOverloadDefaultMaxAccounts
	}
	return c.MaxAccounts
}

func (c *CodexOverloadFailoverConfig) markTTL() time.Duration {
	if c == nil || c.MarkTTL <= 0 {
		return codexOverloadDefaultMarkTTL
	}
	return c.MarkTTL
}

// codexOverloadFailure classifies a pool response as a capacity failure the
// account failover should act on: 408/5xx status, a 2xx SSE stream that opens
// with a server-class response.failed, or a body on any other status that
// names model capacity explicitly (codexCapacityBody). The returned response
// carries any peeked bytes stitched back in place. Quota, auth and client
// errors are not capacity failures and are returned untouched for the layers
// that own them.
//
// Upstream #373 (e2cdd06, unchanged at c9303d3), ported as written (PATCHES
// section 27). Before it the fork caught only 408/5xx and a server-class
// event in a stream's first four events: a 400/429 or 2xx JSON body naming
// capacity went to the client, which Codex renders as the same "Selected
// model is at capacity", and a failure after a reasoning output_item.added
// slipped past the peek. The egress fallback and the Azure fallback's
// default branch read the same predicate.
func codexOverloadFailure(response *http.Response) (bool, string, *http.Response) {
	if response == nil {
		return false, "", response
	}
	if codexEgressPoolFailed(response.StatusCode) {
		if !codexEventStream(response) {
			// The status decides; the peek only keeps the body's retry
			// hints (retry_after_ms, resets_in_seconds) for the mark TTL.
			_, response = codexCapacityBody(response)
		}
		return true, fmt.Sprintf("pool_status_%d", response.StatusCode), response
	}
	if codexSuccessStatus(response.StatusCode) && codexEventStream(response) {
		class, replaced := azureCodexStreamFailure(response)
		if class == codexFailureServer {
			return true, "pool_stream_failed", replaced
		}
		return false, "", replaced
	}
	capacity, replaced := codexCapacityBody(response)
	if capacity {
		return true, "pool_capacity_body", replaced
	}
	return false, "", replaced
}

func codexSuccessStatus(status int) bool {
	return status >= http.StatusOK && status < http.StatusMultipleChoices
}

// codexCapacityBodyPeekBytes bounds how much of a non-stream body the
// capacity check reads. Capacity errors are a few hundred bytes; a larger
// body is a real response and is left alone.
const codexCapacityBodyPeekBytes = 64 * 1024

// codexCapacityBody reports whether a response that is not a 2xx SSE stream
// carries a body naming model capacity: a 4xx JSON error, a non-2xx SSE error
// event, or a 2xx JSON error body. Codex renders all of them as "Selected
// model is at capacity", but the status alone (400, 429, 200) says nothing.
// Only an explicit capacity code or message counts; an unknown code on a
// non-5xx status is the request's business, not the pool's. 2xx SSE streams
// are the stream sniff's job and report false here. The body is stitched back
// either way.
func codexCapacityBody(response *http.Response) (bool, *http.Response) {
	if response == nil || response.Body == nil || response.Body == http.NoBody {
		return false, response
	}
	success := codexSuccessStatus(response.StatusCode)
	if codexEventStream(response) {
		if success {
			return false, response
		}
		class, capacity, replaced := codexStreamPeek(response)
		return class == codexFailureServer && capacity, replaced
	}
	switch {
	case response.StatusCode >= http.StatusBadRequest:
	case success && strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "json"):
	default:
		return false, response
	}
	rest := response.Body
	peeked, err := io.ReadAll(io.LimitReader(rest, codexCapacityBodyPeekBytes+1))
	var tail io.Reader = rest
	if err != nil {
		tail = errorReader{err: err}
	}
	if err != nil || len(peeked) > codexCapacityBodyPeekBytes {
		response.Body = readCloser{Reader: io.MultiReader(bytes.NewReader(peeked), tail), Closer: rest}
		return false, response
	}
	payload := bytes.TrimSpace(peeked)
	class, capacity := codexTurnFailure(payload)
	// Some Codex gateways flatten the error into text/plain while retaining
	// the same user-facing capacity message.  Keep the status/content-type
	// guards above so ordinary successful text responses are never rerouted.
	// Upstream #479 (0c46d4b).
	if !capacity && !json.Valid(payload) {
		lower := strings.ToLower(string(payload))
		capacity = strings.Contains(lower, "selected model is at capacity") ||
			strings.Contains(lower, "model is at capacity")
		if capacity {
			class = codexFailureServer
		}
	}
	response.Body = &codexPeekedBody{
		Reader:   io.MultiReader(bytes.NewReader(peeked), tail),
		Closer:   rest,
		class:    class,
		capacity: capacity,
		payload:  payload,
	}
	return class == codexFailureServer && capacity, response
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

// codexOverloadFailoverTransport replays a capacity-failed Codex request: on
// the same account by default (the stay ladder), and on the next best account
// when the failover is enabled. It sits above the pool retry stack and below
// the regional egress and Azure fallbacks, which only run once it gives up.
// Upstream #389 made it the default capacity layer (installed with the
// failover off too); before the 389/373 port the fork installed it only with
// the failover on.
type codexOverloadFailoverTransport struct {
	base      http.RoundTripper
	server    *Server
	agent     string
	session   string
	userEmail string
	account   string
	poolModel string
	// budget and attempt: see replayablePostRetryTransport. attempt is the
	// request's shared state (upstream #405), set by upstreamLayers.build;
	// nil when the layer is constructed on its own, and RoundTrip then makes
	// a private one from budget.
	budget  *attemptBudget
	attempt *upstreamAttempt
	// serviceTier is the request's service_tier; with poolModel it names
	// the capacity pool a failure is marked in (codex_capacity_marks.go).
	serviceTier string
	// inputTokens estimates the conversation's input tokens from its decoded
	// body (a Codex HTTP turn carries the whole conversation); zero when
	// unknown. Above the failover's size cap the request keeps its account
	// (codex_overload_size.go).
	inputTokens int64
	// targeted is a peer's targeted turn (PATCHES §31): the decider ranked
	// exactly this account, so the layer runs the same-account ladder only --
	// no switch, and no capacity mark, which exists to steer a switch and
	// whose sticky eviction is the decider's call, not this daemon's -- and
	// bounds it under the decider's time-to-first-byte timeout
	// (remotePoolResponseHeaderTimeout). A ladder that outlasts it is a peer
	// that did not answer; two of those open the decider's breaker, and a
	// breaker opening evicts every hold on the peer, the opposite of P8. What
	// outlasts the ladder is stamped overload (stampTargetedResponse) and
	// passed through with the hold kept.
	targeted bool
	// pendingCommit: selection or the refresh already moved this session off
	// a spent account (pendingSessionCommit), and the usage-limit layer below
	// commits the account that serves, as its commitFirstSuccess. Such a move
	// is not a leave P8 keeps the pin through (transientFailoverKeepsPin).
	pendingCommit bool
	// Test seams: the clock and the gap sleep. Nil uses the real ones.
	now   func() time.Time
	sleep func(context.Context, time.Duration) bool
}

// codexCapacityAttemptPlan is where and when the next capacity retry goes.
type codexCapacityAttemptPlan struct {
	// next is the account to switch to; nil retries on the current one.
	next  *accounts.Account
	gap   time.Duration
	phase string
}

func (t codexOverloadFailoverTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if t.attempt == nil {
		t.attempt = standaloneUpstreamAttempt(req, t.server, accounts.Account{ID: t.account, Provider: accounts.ProviderCodex}, t.budget)
		t.attempt.provider, t.attempt.path, t.attempt.poolModel = accounts.ProviderCodex, req.URL.Path, t.poolModel
	}
	a := t.attempt
	config := t.server.CodexOverloadFailover
	// failover is whether this request may switch accounts: the opt-in,
	// unless the conversation is too large to move without re-billing its
	// cache. A kept one runs exactly the failover-off path: the same-account
	// ladder, no capacity marks (whose sticky eviction would move it on the
	// next turn). Upstream #454 as at c9303d3. This replaces fix-up 2's one
	// budgeted retry for a kept conversation: upstream's ladder
	// never hands a kept conversation on after one or two sheds, and never
	// draws on the attempt budget, which is sized for account failover.
	sizeKept := config.failoverKeepsAccount(t.inputTokens)
	keepAccount := sizeKept || t.targeted
	failover := config.enabled() && !keepAccount
	ctx := req.Context()
	// The replacement pick ranks capacity marks of this request's tier only.
	pickCtx := withCodexServiceTier(ctx, t.serviceTier)
	// Both ladders count from the client request's first attempt, also when a
	// layer wrapped around this one sends the request through it again: the
	// start lives in the request's shared attempt state (upstream #486's
	// capacityRetryStart; its persist-after-fallback re-entry is not ported).
	started := a.capacityRetryStart(t.clock())
	// The failover ladder's ~10s budget (#382), from the first attempt.
	deadline := started.Add(t.server.codexDefaultRetryBudget(t.poolModel, t.serviceTier))
	// The same-account ladder (failover off, or kept) has its own budget.
	stayPolicy := config.stayPolicy()
	stayInterval := stayPolicy.intervalOr(codexCapacityDefaultStayInterval)
	stayBudget, stayUnbounded := t.server.codexStayBudget(stayPolicy)
	if t.targeted {
		// Under the decider's header timeout, whatever the operator's stay
		// budget (4m by default, or unbounded): see the field.
		stayBudget, stayUnbounded = min(stayBudget, codexCapacityTargetedRetryBudget), false
		if stayBudget <= 0 {
			stayBudget = codexCapacityTargetedRetryBudget
		}
	}
	stayDeadline := started.Add(stayBudget)
	var stayLog overloadRetryLog
	// releaseHeld ends this request's count in the held-in-overload gauge;
	// set on its first same-account retry.
	var releaseHeld func()
	defer func() {
		if releaseHeld != nil {
			releaseHeld()
		}
	}()
	attemptReq := req
	// addressed is the account attemptReq carries credentials for. accountID
	// is the account that answered last, which a lower layer's failover can
	// make different (upstream #405; the fork's targetID before it).
	addressed := a.current()
	accountID := t.account
	tried := map[string]struct{}{}
	if accountID != "" {
		tried[accountID] = struct{}{}
	}
	maxAccounts := config.maxAccounts()
	switched := 0
	sameAccountLeft := codexCapacitySameAccountRetries
	stayRetries := 0
	// firstLeave is why this turn first left the session's account
	// (t.account): this layer's own switch, or the usage-limit layer's
	// failover below, which tags its answer with the reason
	// (failoverLeave). Only a FORCED leave moves the pin (owner rule P8,
	// PATCHES §35); see the commit below.
	var firstLeave accountSwitch
	// sessionAccountFailures counts this request's capacity failures on
	// t.account: in a row, since nothing returns to it once it is left.
	sessionAccountFailures := 0
	for attempt := 1; ; attempt++ {
		response, err := a.send(base, attemptReq, addressed)
		if err != nil || !a.replayable() || ctx.Err() != nil {
			return response, err
		}
		routed, tagged := routedResponseAccount(response)
		if tagged && routed.ID == "" {
			// A fallback stage answered, not a local account: the usage-limit
			// layer tags the fork's remote pool (the peer that takes a turn
			// once every local account is spent) with an EMPTY account, like
			// every fallback (see ModifyResponse). Nothing here is this
			// layer's business: marking or clearing the local account for the
			// peer's capacity, committing the session to it, or replaying the
			// turn, which only finds the local account still spent and the
			// once-only peer stage used, and hands the client the local 429
			// instead of the peer's answer. Pass it through untouched, exactly
			// as the build before this port did with the failover unset (no layer at
			// all).
			// Upstream has no remote pool, so its #371 crediting below never
			// meets an empty tag on Codex. Fork, fix-up 389-1.
			return response, nil
		}
		// The usage-limit layer below may have failed over again; credit the
		// account that actually answered (upstream #371).
		if tagged {
			accountID = routed.ID
			tried[accountID] = struct{}{}
		}
		if firstLeave.Kind == switchNone {
			firstLeave = failoverLeave(response)
		}
		// Every decision below -- #373's stream check, the stay ladder, the
		// marks and their clear, #389's commit on the terminal event -- tells
		// a stream from a body by its Content-Type, which chatgpt.com never
		// sends. The response already carries one: proxyHandler labels Codex
		// at the base transport, under every layer (upstream #450;
		// fix-up 389-2, which replaced fix-up 389-1's per-attempt labelling
		// here, because the usage-limit layer below this one needs the
		// label too).
		failed, reason, response := codexOverloadFailure(response)
		if !failed {
			if codexSuccessStatus(response.StatusCode) {
				// The account served the model: its capacity marks for the
				// model (every tier) end here, so one success resets the
				// consecutive count that capacity eviction reads. A kept
				// (large) conversation clears too, as upstream's does.
				t.server.clearAccountCapacity(accountID, t.poolModel)
			}
			if t.account != "" && accountID != t.account &&
				codexSuccessStatus(response.StatusCode) && t.server.Sessions != nil {
				if transientFailoverKeepsPin(accounts.ProviderCodex, t.pendingCommit, firstLeave) {
					// Owner rule P8 (PATCHES §35): the session left its
					// account for a TRANSIENT reason -- the layer below
					// failed over on a 429 that is not a spent window -- so
					// the replacement serves this one turn and the session
					// keeps its account. Upstream #389 commits every move;
					// before §35 this commit undid the usage-limit layer's
					// decision to keep the pin. When the layer below made
					// that decision on this very answer, it has logged it.
					if failoverLeave(response).Kind == switchNone {
						logTransientFailoverKept(t.server.Logger, t.agent, t.session, t.account, accountID, firstLeave)
					}
					return response, nil
				}
				// Pin the conversation to the account that served it,
				// whether this layer's failover or the layer below (quota or
				// model failover) moved it there, so the next turn does not
				// start on the one that failed. Only once the turn completes
				// (terminal success event or clean EOF), like the
				// usage-limit layer: a stream that fails after its 2xx
				// headers must not move the session. CompareAndPut from the
				// request's original account, so it is a no-op when the
				// layer below already committed the same move. Upstream #389;
				// the fork committed at the 2xx headers before.
				if err := t.server.commitSuccessfulHTTPResponse(response, t.agent, t.session, t.account, accountID, t.userEmail); err != nil && t.server.Logger != nil {
					t.server.Logger.Warn("codex overload failover could not commit session reassignment", "session", t.session, "account", accountID, "error", err)
				}
			}
			return response, nil
		}
		// Every failure classified here is pre-output: the stream peek
		// holds the response until the first visible output, so nothing has
		// reached the client yet and a replay cannot duplicate anything.
		if sizeKept && attempt == 1 {
			t.server.logFailoverKeptAccount("http", t.agent, t.session, accountID, t.inputTokens)
		}
		if accountID == t.account {
			sessionAccountFailures++
		}
		if failover {
			// Marks steer later placement away from this account, and after
			// repeated failures evict its sticky sessions. That is a switch,
			// so it belongs to the opt-in failover only. The mark carries the
			// classifier's reason for the capacity views (upstream #620).
			hint := codexCapacityRetryHint(response)
			t.server.markAccountOverloadedReason(accountID, t.poolModel, t.serviceTier, reason, config.capacityMarkTTL(hint))
		}

		var plan codexCapacityAttemptPlan
		var planned bool
		if failover {
			plan, planned = t.planDefaultRetry(pickCtx, accountID, reason, tried, &sameAccountLeft, &switched, maxAccounts, deadline)
			if planned && plan.next != nil && accountID == t.account && firstLeave.Kind == switchNone {
				// This layer is leaving the session's account, and only
				// after its quick retry there failed too: two failures in a
				// row, the §27 capacity-eviction threshold that moves even a
				// warm session -- FORCED (classifyAccountSwitch's
				// capacity_evicted row), so the pin follows the turn, as
				// upstream #389 commits it. Counted from this request's own
				// attempts, not the scheduler's marks: it holds with no
				// scheduler, and whatever another request's success did to
				// the marks in between. Short of the threshold it would be
				// an overload, transient (P8).
				firstLeave = classifyAccountSwitch(accountSwitchEvidence{
					Overload:        true,
					CapacityEvicted: sessionAccountFailures >= selectacct.CapacityStickyEvictFailures,
				})
			}
		} else {
			plan, planned = t.planStayRetry(accountID, reason, &stayRetries, stayInterval, stayDeadline, stayUnbounded)
			if planned && releaseHeld == nil {
				releaseHeld = t.server.enterOverloadHold(accounts.ProviderCodex)
			}
		}
		if !planned {
			// The failure goes back up as it is (to the egress and Azure
			// fallbacks when configured, else the client), labelled for the
			// capacity views: X-Subrouter-Reason, a Retry-After from the
			// pool's marks, the traffic error class and the counters
			// (upstream #490, #620). Status and body are unchanged.
			if t.server.recoveryCounters != nil {
				t.server.recoveryCounters.add(accounts.ProviderCodex, "exhausted", t.clock())
			}
			t.server.labelCodexCapacityExhausted(ctx, response, t.poolModel, t.serviceTier)
			return response, nil
		}
		// A switch is the plan's choice; the retarget below for an account
		// the layer below already moved to is still the same account.
		switching := plan.next != nil
		if plan.next == nil && accountID != addressed.ID {
			// "Same account" is the account that answered, not the one this
			// layer addressed: replaying the old request would send it back
			// through the account the layer below already left.
			current, ok := t.server.codexAccountByID(ctx, accountID)
			if !ok {
				return response, nil
			}
			plan.next = &current
		}
		if !t.sleepContext(ctx, plan.gap) {
			return response, nil
		}
		// Same account: attemptReq again, keeping its auth headers and the
		// account it is addressed to. Otherwise the client request retargeted
		// to plan.next: its upstream URL and auth headers (upstream #405).
		var nextReq *http.Request
		var replayErr error
		var moved accounts.Account
		if plan.next == nil {
			nextReq, replayErr = a.replay(attemptReq, nil)
		} else {
			// The layers below start from the account the request is
			// addressed to, so a quota/auth failure from next is charged to
			// next, not to the account that was merely overloaded (upstream
			// #371). The shared attempt carries it (a.send, addressed below);
			// the request context carries it too (withAttemptAccount, the
			// fork's #371 port and what §35's usage-limit layer reads to
			// leave the pin to this layer), so a layer that reads either one
			// starts from next, also when the layers were built standalone
			// and do not share an attempt.
			moved = accounts.Account{
				ID: plan.next.ID, Provider: accounts.ProviderCodex, CredentialVersion: plan.next.CredentialIdentity(),
			}
			nextReq, replayErr = a.replay(req.WithContext(withAttemptAccount(ctx, moved)), plan.next)
		}
		if replayErr != nil {
			return response, nil
		}
		if response.Body != nil {
			_ = response.Body.Close()
		}
		previous := accountID
		attemptReq = nextReq
		if switching {
			t.server.codexCapacityCounters.add(codexCapacityCounterAccountSwitch)
		} else {
			t.server.codexCapacityCounters.add(codexCapacityCounterSameAccountRetry)
		}
		if plan.next != nil {
			if switching && t.server.SchedulerRef != nil {
				// Placement stats (upstream #388): a capacity failover off
				// the account that shed. Not for the retarget above, which
				// retries the account the layer below already moved to.
				t.server.SchedulerRef.NoteFailover(accounts.ProviderCodex, accountID, selectacct.FailoverCapacity)
			}
			accountID = plan.next.ID
			tried[accountID] = struct{}{}
			if t.server.SchedulerRef != nil {
				t.server.SchedulerRef.NoteRouted(accounts.ProviderCodex, accountID)
			}
			addressed = moved
		}
		// The same-account wait can run for minutes: log its first retry,
		// then about once a minute. Account switches log every time.
		now := t.clock()
		if t.server.Logger != nil && (plan.phase != "same_account" || failover || stayLog.due(now)) {
			t.server.Logger.Warn("retrying codex request after capacity failure",
				"agent", t.agent, "session", t.session, "reason", reason, "phase", plan.phase,
				"previous_account", previous, "account", accountID, "attempt", attempt+1,
				"gap", plan.gap.String(), "elapsed", now.Sub(started).Round(time.Millisecond).String(),
				"kept_for_size", sizeKept, "targeted", t.targeted)
		}
	}
}

func (t codexOverloadFailoverTransport) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

func (t codexOverloadFailoverTransport) sleepContext(ctx context.Context, d time.Duration) bool {
	if t.sleep != nil {
		return t.sleep(ctx, d)
	}
	return codexSleepContext(ctx, d)
}

// planStayRetry is the default ladder while the failover is off (or the
// conversation is size-kept): retries on the same account, ramping up to the
// steady interval, until the next wait would end past the wall-clock deadline
// (never, when unbounded; the client's cancellation still ends it). It does
// not draw from the request's shared attempt budget, which is sized for
// account failover inside the pool stack below and would otherwise cut the
// ladder to a few quick tries.
func (t codexOverloadFailoverTransport) planStayRetry(accountID, reason string, retries *int, interval time.Duration, deadline time.Time, unbounded bool) (codexCapacityAttemptPlan, bool) {
	config := t.server.CodexOverloadFailover
	if config != nil && config.stayRetryLimit > 0 && *retries >= config.stayRetryLimit {
		t.logOverload("codex capacity retry exhausted", accountID, reason, 0, "max_retries")
		return codexCapacityAttemptPlan{}, false
	}
	gap := config.stayDelay(*retries, interval)
	if !unbounded && t.clock().Add(gap).After(deadline) {
		t.logOverload("codex capacity retry exhausted", accountID, reason, 0, "time_budget")
		return codexCapacityAttemptPlan{}, false
	}
	*retries++
	return codexCapacityAttemptPlan{gap: gap, phase: "same_account"}, true
}

// planDefaultRetry is the failover ladder: one quick retry on the same
// account, then the overload failover to other accounts, all inside the
// time budget and the request's shared attempt budget.
func (t codexOverloadFailoverTransport) planDefaultRetry(ctx context.Context, accountID, reason string, tried map[string]struct{}, sameAccountLeft, switched *int, maxAccounts int, deadline time.Time) (codexCapacityAttemptPlan, bool) {
	config := t.server.CodexOverloadFailover
	if *sameAccountLeft > 0 {
		// One failure is usually shedding that a retry clears
		// (selectacct/capacity.go), and the session's prompt cache is on
		// this account: retry here first. A second failure has now marked
		// the account CapacityStickyEvictFailures times, so the switch below
		// is the same "two in a row" that evicts every other sticky session
		// on it.
		*sameAccountLeft--
		gap := config.sameAccountDelay()
		if time.Now().Add(gap).After(deadline) {
			t.logOverload("codex capacity retry exhausted", accountID, reason, *switched, "time_budget")
			return codexCapacityAttemptPlan{}, false
		}
		if !t.attempt.consume() {
			t.logOverload("codex capacity retry exhausted", accountID, reason, *switched, "retry_budget")
			return codexCapacityAttemptPlan{}, false
		}
		return codexCapacityAttemptPlan{gap: gap, phase: "same_account"}, true
	}
	// The same account shed the request again (its capacity mark now
	// ranks it last for this pool): move on.
	if *switched >= maxAccounts {
		t.logOverload("codex overload failover exhausted", accountID, reason, *switched, "max_accounts")
		return codexCapacityAttemptPlan{}, false
	}
	gap := config.switchDelay()
	if time.Now().Add(gap).After(deadline) {
		t.logOverload("codex overload failover exhausted", accountID, reason, *switched, "time_budget")
		return codexCapacityAttemptPlan{}, false
	}
	if !t.attempt.consume() {
		t.logOverload("codex overload failover exhausted", accountID, reason, *switched, "retry_budget")
		return codexCapacityAttemptPlan{}, false
	}
	next, pickErr := t.server.oauthRetryCandidate(ctx, accounts.ProviderCodex, t.agent, t.session, t.userEmail, t.poolModel, tried, false, false)
	if pickErr != nil {
		t.logOverload("codex overload failover has no alternate account", accountID, reason, *switched, pickErr.Error())
		return codexCapacityAttemptPlan{}, false
	}
	*switched++
	return codexCapacityAttemptPlan{next: &next, gap: gap, phase: "switch_account"}, true
}

// codexAccountByID finds a Codex account of the current inventory by ID.
func (s *Server) codexAccountByID(ctx context.Context, id string) (accounts.Account, bool) {
	if s == nil || id == "" {
		return accounts.Account{}, false
	}
	for _, account := range s.accountListContext(ctx) {
		if account.ID == id && accountProviderOrCodex(account) == accounts.ProviderCodex {
			return account, true
		}
	}
	return accounts.Account{}, false
}

func (t codexOverloadFailoverTransport) logOverload(message, accountID, reason string, switched int, why string) {
	if t.server == nil || t.server.Logger == nil {
		return
	}
	t.server.Logger.Warn(message, "agent", t.agent, "session", t.session, "account", accountID,
		"reason", reason, "switched", switched, "why", why)
}

// codexCapacityReason names a capacity failure for its mark and the capacity
// event log: "server_is_overloaded" for the capacity code, otherwise the
// upstream's own code or the transport reason. Upstream #599/#620 keep it in
// codex_capacity_history.go, which this fork does not carry (PATCHES §27).
func codexCapacityReason(body []byte, fallback string) string {
	var event map[string]any
	if err := json.Unmarshal(body, &event); err == nil {
		code, message := codexFailureCodeAndMessage(event)
		if code != "" {
			return code
		}
		if strings.Contains(strings.ToLower(message), "at capacity") {
			return "server_is_overloaded"
		}
	}
	if fallback == "" {
		return "unknown"
	}
	return fallback
}

// markAccountOverloaded records a capacity mark for the account in the
// (model, service tier) pool for a short window and returns the account's
// consecutive failures there. It is not a quota mark: headroom and sticky
// sessions are untouched, the account only ranks below unmarked ones for
// this pool, and a session leaves it only once it keeps failing
// (selectacct.CapacityStickyEvictFailures).
//
// Until PATCHES section 27 this wrote MarkExhaustedUntil -- a pool exhaustion
// overlay, which made the account Evicted, so one capacity failure moved every
// warm session on it (and, for a websocket, whose pool model is empty, marked
// the whole account). Upstream 862a5a5 replaced exactly that with this mark.
func (s *Server) markAccountOverloaded(accountID, model, tier string, ttl time.Duration) int {
	return s.markAccountOverloadedReason(accountID, model, tier, "", ttl)
}

// markAccountOverloadedReason is markAccountOverloaded that labels the mark
// with the failure's reason (codexCapacityReason or the HTTP classifier's),
// shown in the traffic and usage-status capacity marks. The label is
// diagnostic only: ranking and eviction read the failure count and expiry.
// Upstream #620 (d8fd3d6).
func (s *Server) markAccountOverloadedReason(accountID, model, tier, reason string, ttl time.Duration) int {
	if s == nil || s.SchedulerRef == nil || accountID == "" {
		return 0
	}
	failures := s.SchedulerRef.MarkCapacityReasonUntil(accounts.ProviderCodex, accountID, model, tier, reason, time.Now().Add(ttl))
	s.codexCapacityCounters.add(codexCapacityCounterMark)
	if s.Logger != nil {
		s.Logger.Warn("codex account marked at capacity for this model pool", "account", accountID,
			"pool", selectacct.CapacityPoolKey(model, tier), "failures", failures, "reason", reason, "ttl", ttl.String())
	}
	return failures
}

// codexOverloadReroutes counts websocket 1012 reroutes per session so one
// session cannot bounce forever between overloaded accounts.
type codexOverloadReroutes struct {
	mu      sync.Mutex
	entries map[string]codexOverloadRerouteEntry
}

type codexOverloadRerouteEntry struct {
	count     int
	expiresAt time.Time
	// model is the model pool the session's last reroute marked, for its
	// reconnect's capacity marks (reconnectCapacityModel).
	model string
}

func newCodexOverloadReroutes() *codexOverloadReroutes {
	return &codexOverloadReroutes{entries: map[string]codexOverloadRerouteEntry{}}
}

// allow records one reroute and reports whether it is within budget.
func (r *codexOverloadReroutes) allow(key string, limit int) bool {
	if r == nil || key == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for k, entry := range r.entries {
		if !entry.expiresAt.After(now) {
			delete(r.entries, k)
		}
	}
	entry := r.entries[key]
	if entry.count >= limit {
		return false
	}
	entry.count++
	entry.expiresAt = now.Add(codexOverloadRerouteWindow)
	r.entries[key] = entry
	return true
}

// codexOverloadWebSocketReroute marks the account in the turn's (model,
// tier) pool and reports whether the websocket turn should be closed 1012 so
// the session reconnects. The reconnect goes through selection like any turn:
// after one failure the mark only ranks, so a warm session reconnects onto the
// SAME account (its cache is there); after CapacityStickyEvictFailures in a row
// it moves if the pool has an account that is not shedding. False once the
// session has used its reroute budget. False, unmarked, for a conversation of
// more than the failover's size cap (inputTokens): it stays on its account
// like with the failover off (upstream c9303d3, #457).
//
// It also records the marked model against the session (noteCapacityModel):
// the reconnect's upgrade usually names no model, and without it the
// reconnect would read no marks and never be capacity-evicted (PATCHES
// section 27, fork plumbing).
func (s Server) codexOverloadWebSocketReroute(agentType, sessionID, accountID, model, tier string, body []byte, inputTokens int64) bool {
	if !s.CodexOverloadFailover.enabled() {
		return false
	}
	if s.CodexOverloadFailover.failoverKeepsAccount(inputTokens) {
		s.logFailoverKeptAccount("websocket", agentType, sessionID, accountID, inputTokens)
		return false
	}
	key := azureCodexSessionKeyFor(agentType, sessionID)
	if !s.codexOverloadRerouteCounts.allow(key, codexOverloadMaxWebSocketReroutes) {
		return false
	}
	failures := s.markAccountOverloadedReason(accountID, model, tier, codexCapacityReason(body, "websocket"),
		s.CodexOverloadFailover.capacityMarkTTL(codexRetryHintJSON(body)))
	s.codexOverloadRerouteCounts.noteCapacityModel(key, model)
	// Upstream #620 counts every reroute as an account switch: with #599's
	// per-session ban its reconnect left the account. Here the mark only
	// ranks until CapacityStickyEvictFailures in a row (PATCHES §27), so a
	// first failure's reconnect is a retry on the same account, as the HTTP
	// ladder counts its stays.
	if failures >= selectacct.CapacityStickyEvictFailures {
		s.codexCapacityCounters.add(codexCapacityCounterAccountSwitch)
	} else {
		s.codexCapacityCounters.add(codexCapacityCounterSameAccountRetry)
	}
	if s.Logger != nil {
		s.Logger.Warn("codex websocket turn hit a capacity error; closing 1012 so the session reconnects",
			"agent", agentType, "session", sessionID, "account", accountID)
	}
	return true
}
