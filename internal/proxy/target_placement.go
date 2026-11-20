package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// Targeted placement: the half of PATCHES §31 that ANSWERS a peer (owner rules
// P1, P3, P6, P7).
//
// A deciding daemon ranks its own accounts and every peer's in one ranking and
// names the peer account it chose (remotePoolTargetAccountHeader). This daemon
// then serves exactly that account or refuses before anything reaches
// upstream. It never substitutes: on 2026-09-27 a laptop sent ~23 sessions "to
// the box" because the box's best account (chip-5, in burndown) looked roomy,
// and the box -- which knew chip-5 was at its taper cap -- spread them over
// accounts whose 5h windows were spent (23,195 "routing optimistically" lines
// in a day), accounts the laptop never compared against its own roomy ones.
// The decider cannot rank what the answerer silently swaps in, so a refusal
// that makes it recompute is the only honest answer.
//
// Two things make that possible, and both are computed here by THIS daemon's
// own selection code rather than re-derived by the peer:
//
//   - the published per-account verdict (withNewSessionVerdicts), so the
//     decider never ranks an account this daemon would refuse; and
//   - the request-time judgement (selectTargetedAccount), which re-asks the
//     same question on the scheduler selection is about to use, because a
//     published verdict is up to a TTL old and live sessions move.
//
// A targeted request is folded into forcedAccountSelection in the handler,
// which disarms every stage that could substitute or re-forward (the peer
// pool, Fable/Bedrock, Opus substitution, Azure, usage-limit and Codex-overload
// failover). A request that arrived from a peer is therefore served on its
// target or refused; it is never forwarded again (P7).

// targetRefusedError is selection's answer to a targeted turn it will not
// serve. The handler writes it as the 409 refusal (writeTargetRefusal) before
// upstream is contacted and before any session-store write. It is never turned
// into a substitute account.
type targetRefusedError struct {
	Account string
	Reason  string
	// Continuing: the target was judged as the session's existing warm
	// placement (evicted-only), not as a new placement (the verdict).
	Continuing bool
	// Verdict is the target's new-session verdict at refusal time; nil when
	// there is no account to judge (unknown_account) or the refusal came from
	// the credential (auth_invalid).
	Verdict *selectacct.NewSessionVerdict
	cause   error
}

func (e *targetRefusedError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("targeted account %q refused: %s: %v", e.Account, e.Reason, e.cause)
	}
	return fmt.Sprintf("targeted account %q refused: %s", e.Account, e.Reason)
}

func (e *targetRefusedError) Unwrap() error { return e.cause }

// targetAccountFromRequest reads the peer's target off the request as this
// daemon received it. session.StripSubrouterHeaders removes the header from
// everything this daemon sends onward, so it never reaches upstream and never
// crosses a second hop.
func targetAccountFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	return session.NormalizeAccountID(r.Header.Get(remotePoolTargetAccountHeader))
}

// withNewSessionVerdicts attaches this daemon's own NewSessionVerdict to every
// measured OAuth row of usage-status, and to each of the row's quota pools
// (PATCHES §31, D1).
//
// Computed on the scheduler SELECTION builds (selectionSchedulerFrom: live
// debits, parked and live session counts, the owner's reserves), because those
// are exactly the facts a peer cannot see. `normalized.usable` is NOT that: it
// is UsableForNewSession on the bare scheduler, with no reserve, no live count
// and no endgame drain -- a peer reading it saw chip-5 as a burndown account
// with room, when it already carried its whole taper allowance.
//
// No machine term: the verdict answers for "a peer", and the enforcing machine
// policy is judged per caller at request time (machine_policy). A row with no
// score gets no verdict, and neither does an API-key row: absence reads as "no
// verdict" to a peer, never as eligible or ineligible.
//
// The input slice may be owned by AccountRef, so rows and their normalized
// blocks are copied before the verdict is attached.
func (s Server) withNewSessionVerdicts(statuses []AccountUsageStatus) []AccountUsageStatus {
	base := s.scheduler()
	now := time.Now()
	// One selection scheduler per quota pool for the whole response: they are
	// the same object selection builds, and building each counts every stored
	// session.
	schedulers := map[string]selectacct.Scheduler{}
	schedulerFor := func(poolKey string) selectacct.Scheduler {
		if scheduler, ok := schedulers[poolKey]; ok {
			return scheduler
		}
		scheduler := s.selectionSchedulerFrom(base, poolKey, "")
		schedulers[poolKey] = scheduler
		return scheduler
	}
	out := append([]AccountUsageStatus(nil), statuses...)
	for i := range out {
		row := out[i]
		if row.Normalized == nil || row.AuthMode != accounts.AuthModeOAuth {
			continue
		}
		provider := schedulerAccountProvider(row.Provider)
		normalized := *row.Normalized
		normalized.NewSession = newSessionVerdictForRow(schedulerFor(""), provider, row, now)
		if len(row.Normalized.Pools) > 0 {
			normalized.Pools = make(map[string]*NormalizedUsage, len(row.Normalized.Pools))
			for key, pool := range row.Normalized.Pools {
				if pool == nil {
					continue
				}
				entry := *pool
				entry.NewSession = newSessionVerdictForRow(schedulerFor(key), provider, row, now)
				normalized.Pools[key] = &entry
			}
		}
		out[i].Normalized = &normalized
	}
	return out
}

// newSessionVerdictForRow is the shared verdict plus the one clause only the
// proxy knows: a credential this daemon checked and found dead cannot serve a
// new session or a continuing one. A cut sweep's auth_valid:false is not that
// verdict (the same tells the peer reader uses: sweep_cut, a context error).
func newSessionVerdictForRow(scheduler selectacct.Scheduler, provider accounts.Provider, row AccountUsageStatus, now time.Time) *selectacct.NewSessionVerdict {
	// LookupScore, not the verdict alone: an unscored account would be judged
	// on the scheduler's optimistic default and published as eligible.
	if _, ok := scheduler.LookupScore(provider, row.ID); !ok {
		return nil
	}
	verdict := scheduler.NewSessionVerdict(provider, row.ID, now)
	if row.AuthChecked && !row.AuthValid && !row.SweepCut && !isContextErrorMessage(row.Error) {
		verdict.Eligible, verdict.Reason, verdict.Evicted = false, targetRefusedAuthInvalid, true
	}
	return &verdict
}

// selectTargetedAccount places a targeted turn on exactly the named account or
// refuses it with a *targetRefusedError. It is selection's answer for a peer's
// targeted forward, called from accountForSessionProviderWithOptions with the
// scheduler selection built (ForModel-narrowed, live counts, reserves, the
// machine term), after the usage refresh and after the control-plane and
// OAuth-only filters.
//
// The question depends on what the session already is here:
//
//   - CONTINUING -- pinned to the target and warm (the same warm test
//     reuseStickyAssignment applies), or a side request riding its pin: the
//     warm-session rule, PATCHES §20. Refused only when the account is
//     evicted (a spent window, the owner's holdout). A warm session below the
//     new-session floor, or on an account at its endgame cap, keeps its
//     account here exactly as it would locally.
//   - otherwise a NEW placement, a cold pin included: the new-session verdict
//     (selectacct.NewSessionVerdict), the same answer usage-status publishes,
//     re-asked now. An account at its endgame cap is refused, not demoted
//     (Deviation X3: locally a capped account only ranks last, but a peer
//     piling sessions past the cap is the incident).
//
// A placement pins immediately (the forced selector's pattern), never through
// the provisional-move block: the decider chose this account, and a pin that
// waits for a 2xx would let the next turn's sticky logic move it elsewhere.
// That also keeps §31 independent of the §25 sticky-commit fix.
func (s Server) selectTargetedAccount(provider accounts.Provider, agentType, sessionID, userEmail, model, targetAccountID string,
	r *http.Request, availableAccounts []accounts.Account, scheduler selectacct.Scheduler, machineID string,
	stored session.Assignment, hasAssignment bool, options accountSelectionOptions) (accounts.Account, string, string, routingCapacity, error) {
	refuse := func(reason string, continuing bool, verdict *selectacct.NewSessionVerdict) (accounts.Account, string, string, routingCapacity, error) {
		return accounts.Account{}, sessionID, userEmail, routingCapacity{}, &targetRefusedError{
			Account: targetAccountID, Reason: reason, Continuing: continuing, Verdict: verdict,
		}
	}
	if hasAssignment && userEmail == "" {
		userEmail = stored.UserEmail
	}
	target, ok := findAccount(availableAccounts, targetAccountID)
	if !ok {
		// availableAccounts is already narrowed to OAuth wherever the request
		// needs OAuth (the ChatGPT backend, the model catalog, the Fable
		// pool). An API-key account that exists but was filtered out is a
		// different answer from one this daemon does not have.
		reason := targetRefusedUnknownAccount
		if r != nil {
			if listed, found := findAccount(filterAccountsForProvider(s.accountListContext(r.Context()), provider), targetAccountID); found &&
				listed.AuthMode != accounts.AuthModeOAuth {
				reason = targetRefusedNotOAuth
			}
		}
		return refuse(reason, false, nil)
	}
	scoreProvider := schedulerAccountProvider(target.Provider)
	pinned := hasAssignment && accountMatches(target, stored.AccountID)
	warm := false
	if pinned {
		// reuseStickyAssignment's own warm test: the shared keyless Codex
		// bucket is not a conversation, so only a turn in flight keeps it.
		if sharedCodexFallbackBucket(agentType, sessionID) {
			warm = s.activeSession(agentType, sessionID)
		} else {
			warm = s.sessionWarm(agentType, sessionID)
		}
	}
	continuing := pinned && (options.sideChannel || warm)
	verdict := scheduler.NewSessionVerdict(scoreProvider, target.ID, time.Now())
	if continuing {
		if scheduler.Evicted(scoreProvider, target.ID) {
			reason := selectacct.VerdictExhausted
			if !scheduler.Exhausted(scoreProvider, target.ID) {
				reason = selectacct.VerdictHoldout
			}
			return refuse(reason, true, &verdict)
		}
	} else if !verdict.Eligible {
		return refuse(verdict.Reason, false, &verdict)
	}
	// The machine-sharing policy, exactly as the selection tail applies it:
	// logged always, refusing only when enforcing.
	if reason, refused := scheduler.MachineRefusal(target); refused {
		if s.Logger != nil {
			s.Logger.Info("machine sharing decision",
				"mode", string(s.MachinePolicy.Mode),
				"enforced", s.MachinePolicy.Enforcing(),
				"machine", machineID,
				"account", target.ID,
				"provider", string(target.Provider),
				"reason", reason,
			)
		}
		if s.MachinePolicy.Enforcing() {
			return refuse(targetRefusedMachinePolicy, continuing, &verdict)
		}
	}
	if options.sideChannel {
		// Served on the target and placed nowhere: no pin, no binding.
		return target, sessionID, userEmail, routingCapacity{}, nil
	}
	if s.MachinePolicy.Active() && machineID != "" {
		if isNew := s.MachinePolicy.Note(target.Provider, target.ID, machineID); isNew && s.Logger != nil {
			s.Logger.Info("machine bound to account",
				"machine", machineID,
				"account", target.ID,
				"provider", string(target.Provider),
				"bindings", len(s.MachinePolicy.Bindings(target.Provider, target.ID)),
				"cap", selectacct.MaxMachinesPerAccount,
			)
		}
	}
	if pinned {
		// Already here: keep UpdatedAt tracking use, as sticky reuse does,
		// rather than rewriting the store on every turn.
		if userEmail != "" && userEmail != stored.UserEmail {
			assignment, err := s.Sessions.Put(agentType, sessionID, target.ID, userEmail)
			if err != nil {
				return accounts.Account{}, sessionID, userEmail, routingCapacity{}, err
			}
			userEmail = assignment.UserEmail
		} else {
			s.touchSessionBestEffort(agentType, sessionID)
		}
	} else {
		previousAccountID := ""
		if hasAssignment {
			previousAccountID = stored.AccountID
		}
		// The decider ranked this account across every pool (§31): a
		// placement, committed now (upstream #532's reason and deferred flag).
		s.logAccountMove(agentType, sessionID, model, previousAccountID, target.ID, provider, accountMovePlacement, &scheduler, false)
		assignment, err := s.Sessions.Put(agentType, sessionID, target.ID, userEmail)
		if err != nil {
			return accounts.Account{}, sessionID, userEmail, routingCapacity{}, err
		}
		userEmail = assignment.UserEmail
	}
	if s.Logger != nil {
		s.Logger.Info("targeted placement",
			"agent", agentType,
			"session", sessionID,
			"account", target.ID,
			"continuing", continuing,
			"reason", verdict.Reason,
			"live_sessions", verdict.LiveSessions,
			"allowed_sessions", verdict.AllowedSessions,
			"remote_addr", clientRemoteIPOrEmpty(r),
		)
	}
	// Zero capacity, as for a forced account: nothing downstream compares a
	// targeted turn against a peer (peerAllowed is false for it).
	return target, sessionID, userEmail, routingCapacity{}, nil
}

// targetRefusedModelUnsupported stamps a targeted Codex answer whose account
// does not serve the model. Kept out of remote_pool_target.go, the wire file
// both halves share byte for byte: a decider reads any refusal header as a
// forced refusal whatever its reason, so an older decider needs no new word.
const targetRefusedModelUnsupported = "model_unsupported"

// stampTargetedResponse marks a targeted turn's non-2xx (or Claude-rejected)
// upstream answer so the decider can tell "this account can no longer serve
// the session" from "this turn was limited". Every failover stage is disarmed
// for a targeted turn, so the answer passes through unchanged; the stamp is the
// only thing added.
//
//   - refused (X-Subrouter-Target-Refused): a 401, or the target is now
//     evicted -- the passive capture has just marked it -- or Anthropic said
//     `rejected`. A rejected 200 in particular MUST read as a refusal: Claude
//     Code hard-blocks on that header, and with failover disarmed this daemon
//     can no longer hide it by replaying elsewhere.
//   - transient (X-Subrouter-Transient): a 429 on an account that is limited,
//     not spent. The decider keeps the session's hold and serves only this turn
//     elsewhere.
//   - overload (X-Subrouter-Transient: overload): a Claude 529 or other 5xx
//     that survived targetedOverloadRetryTransport's same-account retries, or
//     a Codex capacity failure (codexOverloadFailure) that survived the
//     targeted same-account ladder. The
//     decider passes it through and keeps the hold; unstamped, a 503 read as a
//     legacy peer's "my pool is spent" and ended it.
//
// Judged on the scheduler narrowed to the request's quota pool, the one the
// target was placed on, so a spent Fable pool on an account with Opus left
// still refuses a Fable turn.
func (s Server) stampTargetedResponse(response *http.Response, account accounts.Account, poolModel string) {
	if response == nil || account.ID == "" {
		return
	}
	claude := accountProviderOrCodex(account) == accounts.ProviderClaude
	rejected := claude && claudeResponseRejected(response.Header)
	status := response.StatusCode
	if status >= 200 && status < 300 && !rejected {
		return
	}
	if status == http.StatusUnauthorized {
		response.Header.Set(remotePoolTargetRefusedHeader, targetRefusedAuthInvalid)
		return
	}
	// A Codex account whose plan does not serve the model: the decider ranks
	// peer accounts on a forced model_unsupported switch too, and cannot see
	// a peer account's model support, so the answer must be a refusal it
	// recomputes on (its local replacement is still in the ranking), not a
	// 400 handed to the client. responseCodexChatGPTModelUnsupported restores
	// what it reads.
	if !claude {
		if unsupported, _ := responseCodexChatGPTModelUnsupported(response); unsupported {
			response.Header.Set(remotePoolTargetRefusedHeader, targetRefusedModelUnsupported)
			return
		}
	}
	if claude && !rejected && claudeOverloadStatus(status) {
		response.Header.Set(remotePoolTransientHeader, remotePoolTransientOverload)
		return
	}
	// The Codex twin: a 408/5xx, or any status whose body names model
	// capacity (#373), that outlasted the targeted same-account ladder
	// (codexOverloadFailoverTransport.targeted). Checked before the 429
	// rows, so a capacity-named 429 is not read as the account's limit; a
	// usage_limit_reached body names no capacity and still falls through to
	// the refusal. Unstamped, a bare 503 read as a legacy peer's
	// peer_declined -- forced -- and ended the hold on a provider blip (P8).
	// codexOverloadFailure restores what it peeks into response.Body.
	if !claude {
		if overloaded, _, _ := codexOverloadFailure(response); overloaded {
			response.Header.Set(remotePoolTransientHeader, remotePoolTransientOverload)
			return
		}
	}
	limited := status == http.StatusTooManyRequests
	if claude {
		limited = claudeAccountUnusableStatus(status) || rejected
	}
	if !limited {
		return
	}
	scheduler := s.selectionSchedulerFrom(s.scheduler(), poolModel, "")
	scoreProvider := schedulerAccountProvider(account.Provider)
	switch {
	case scheduler.Exhausted(scoreProvider, account.ID):
		response.Header.Set(remotePoolTargetRefusedHeader, selectacct.VerdictExhausted)
	case scheduler.InHoldout(scoreProvider, account.ID):
		response.Header.Set(remotePoolTargetRefusedHeader, selectacct.VerdictHoldout)
	case rejected:
		response.Header.Set(remotePoolTargetRefusedHeader, selectacct.VerdictExhausted)
	case !claude && usageLimitJSON(peekResponseBodyPrefix(response)):
		// A Codex usage_limit_reached is marked by the passive capture only
		// once the body is read, which is after this runs; the body says it
		// now. peekResponseBodyPrefix restores what it reads.
		response.Header.Set(remotePoolTargetRefusedHeader, selectacct.VerdictExhausted)
	default:
		response.Header.Set(remotePoolTransientHeader, "1")
	}
}

// writeTargetedAdmissionRejected answers a TARGETED turn the admission gate
// turned away (a long pause after a 429, or a full queue). A targeted turn has
// no failover, so the rejection reaches the decider as the answer; a bare 503
// there reads as a legacy peer's peer_declined -- forced -- and ended the hold
// on what is a throttle or a busy account, not a spent one (P8, PATCHES §31).
// So the answer is stamped like stampTargetedResponse's 429 rows: a refusal
// when the scheduler holds the target spent or in the owner's holdout, else a
// 429 marked transient, which keeps the hold and serves only this turn
// elsewhere (classifyAccountSwitch's rate_limited row).
func (s Server) writeTargetedAdmissionRejected(w http.ResponseWriter, err error, account accounts.Account, poolModel string) {
	reason := admissionReasonQueueFull
	retryAfter := admissionRetryAfterSeconds(err)
	if errors.Is(err, errAdmissionPaused) {
		reason = admissionReasonPaused
		retryAfter = minAdmissionRetryAfter
	}
	if retryAfter < minAdmissionRetryAfter {
		retryAfter = minAdmissionRetryAfter
	}
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	w.Header().Set(subrouterReasonHeader, reason)
	scheduler := s.selectionSchedulerFrom(s.scheduler(), poolModel, "")
	scoreProvider := schedulerAccountProvider(account.Provider)
	switch {
	case account.ID != "" && scheduler.Exhausted(scoreProvider, account.ID):
		w.Header().Set(remotePoolTargetRefusedHeader, selectacct.VerdictExhausted)
	case account.ID != "" && scheduler.InHoldout(scoreProvider, account.ID):
		w.Header().Set(remotePoolTargetRefusedHeader, selectacct.VerdictHoldout)
	default:
		w.Header().Set(remotePoolTransientHeader, "1")
	}
	http.Error(w, err.Error(), http.StatusTooManyRequests)
}

// clientRemoteIPOrEmpty is clientRemoteIP for a request that may be nil.
func clientRemoteIPOrEmpty(r *http.Request) string {
	if r == nil {
		return ""
	}
	return clientRemoteIP(r)
}

// targetedOverloadRetryTransport is the same-account overload retry of
// usageLimitRetryTransport for a TARGETED Claude turn, which is installed on
// no targeted turn: its account failover would substitute an account the
// decider did not rank. An Anthropic 529/5xx is provider-wide, not the
// account's (claudeOverloadStatus), so it is retried on the SAME request --
// same account, same credential, never another -- after the same bounded
// backoff (providerOverloadMaxRetries, providerOverloadBackoff), spending the
// request's retry budget. A blip is absorbed where the session's cache is, as
// it was before §31 made the turn targeted; one that outlasts the retries is
// stamped overload (stampTargetedResponse) and passed through.
type targetedOverloadRetryTransport struct {
	base    http.RoundTripper
	logger  *slog.Logger
	agent   string
	session string
	account string
	budget  *attemptBudget
	// sleep waits for the backoff; nil is a real timer (tests inject).
	sleep func(context.Context, time.Duration) error
}

func (t targetedOverloadRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	attemptReq := req
	for retries := 0; ; retries++ {
		response, err := t.base.RoundTrip(attemptReq)
		if err != nil || req.GetBody == nil || req.Context().Err() != nil {
			return response, err
		}
		// A 5xx that carries the rejected unified-status is the account out of
		// quota, not overload: it passes through and is stamped refused.
		if !claudeOverloadStatus(response.StatusCode) || claudeResponseRejected(response.Header) ||
			retries >= providerOverloadMaxRetries || !t.budget.consume() {
			return response, nil
		}
		wait := providerOverloadBackoff(response.Header, retries)
		if t.logger != nil {
			t.logger.Warn("retrying targeted turn after provider overload on the same account",
				"agent", t.agent, "session", t.session, "account", t.account,
				"status", response.StatusCode, "wait", wait.String(),
				"overload_retry", retries+1, "max_overload_retries", providerOverloadMaxRetries)
		}
		if response.Body != nil {
			_ = response.Body.Close()
		}
		if err := t.wait(req.Context(), wait); err != nil {
			return nil, err
		}
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		attemptReq = req.Clone(req.Context())
		attemptReq.Body = body
		attemptReq.GetBody = req.GetBody
		attemptReq.ContentLength = req.ContentLength
	}
}

func (t targetedOverloadRetryTransport) wait(ctx context.Context, d time.Duration) error {
	if t.sleep != nil {
		return t.sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
