package proxy

// Claiming a Claude account's saved usage-limit reset (PATCHES §46, §46.7).
//
// The daemon claims, never the CLI, because the daemon is the account's only
// refresher: a claim needs a live access token, and a second process
// refreshing a Claude login rotates the refresh token out from under the
// daemon and kills the account. So the claim takes its token from the same
// store path the usage sweep uses, and never refreshes on its own -- a 401
// is reported, not retried.
//
// A request that does not say dry_run:false is a preview, and a claim must
// repeat the account (confirm_account); since §46.7 `sr claude reset <name>`
// fills that in and claims in one call, with no preview first (the owner's
// decision, 2026-09-29). The claim picks Anthropic's next_grant_id from the
// sweep's cached block when that is fresh, and reads live only otherwise:
// Anthropic's usage endpoint rate-limits per login, and the old preview's
// read plus the claim's own re-read was enough to draw a 429. Any 429 -- a
// status read, the profile read, the claim POST -- is waited out (its
// Retry-After, else 30 s, 60 s, 90 s) inside one bounded budget, and a claim
// POST that drew a 429 is resent with the SAME request_id. Claims on one
// account are serialized, and the request_id is logged before every POST.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// ClaudeResetPath is the claim route. Admin-gated; on the public listener only
// with --public-admin and a named admin-role token (PATCHES §46.9).
const ClaudeResetPath = "/_subrouter/claude-reset"

const (
	// claudeResetPendingTTL is how long an unconfirmed claim's request_id is
	// reused for the same grant, counted from when it was minted; Claude Code
	// keeps its own for 10 minutes the same way. It lives in this daemon's
	// memory only: a restart forgets it.
	claudeResetPendingTTL = 10 * time.Minute
	// ClaudeResetBudget bounds a whole call on the daemon: a refresh, the
	// reads, up to claudeResetRateLimitBound spent waiting out 429s, the
	// claim (25 s a try) and the re-read after it. The CLI waits longer than
	// this, so it always gets the daemon's answer.
	ClaudeResetBudget = 6 * time.Minute
	// claudeResetRateLimitBound is the most one call spends waiting out
	// Anthropic's 429s, over every read and claim try together.
	claudeResetRateLimitBound = 4 * time.Minute
	// claudeResetClaimReserve is kept back from the budget for the claim POST
	// and the re-read after it: a wait that would eat into it is not taken.
	claudeResetClaimReserve = 45 * time.Second
	// claudeResetCacheFresh is how old a cached saved-reset block (the
	// sweep's usage read, or Claude Code's own read) may be for a claim to
	// take its next_grant_id without reading live. Only a claimable answer is
	// taken from the cache; a refusal is read again live before it is given.
	claudeResetCacheFresh = 30 * time.Minute
	// claudeResetCacheFallback is how old a cached block may be to stand in
	// for a live read that stayed unreadable (a 429 past the bound, a 5xx).
	claudeResetCacheFallback = 24 * time.Hour
)

// claudeResetBackoff is the wait after the n-th 429 when Anthropic sends no
// Retry-After; past the end it repeats the last.
var claudeResetBackoff = []time.Duration{30 * time.Second, 60 * time.Second, 90 * time.Second}

// claudeResetSleep waits d, or until ctx ends. A var only so tests do not
// sit through real backoff; production never changes it.
var claudeResetSleep = func(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ClaudeResetRequest is the route's body. DryRun is a pointer so that a body
// that does not say dry_run:false is a preview: nothing is ever spent by
// omission.
type ClaudeResetRequest struct {
	Account   string `json:"account"`
	DryRun    *bool  `json:"dry_run,omitempty"`
	GrantID   string `json:"grant_id,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	// ConfirmAccount must repeat Account for a claim (dry_run:false): a
	// bare dry_run:false is refused with 400, so no process spends a reset
	// with one careless request. `sr claude reset` fills it in.
	ConfirmAccount string `json:"confirm_account,omitempty"`
	// ExpectedOrganization, when given, is the organization the caller
	// expects the claim to go to; the daemon refuses when the account's live
	// profile names another. Optional since §46.7 (the one-call CLI sends
	// none).
	ExpectedOrganization string `json:"expected_organization,omitempty"`
	// Early is accepted and ignored since §46.7: a claim no longer needs it
	// when none of the limits the grant clears is exhausted (the answer
	// says so after the fact, EarlyUse).
	Early bool `json:"early,omitempty"`
}

// ClaudeResetResponse is what one call found and, for a claim, what came
// back. Result and Reason are Anthropic's, verbatim.
type ClaudeResetResponse struct {
	Account string `json:"account"`
	DryRun  bool   `json:"dry_run"`
	// Status is the cedar_ember block this call went by: a live read, or the
	// daemon's cached one (StatusSource).
	Status *agentclaude.ResetStatus `json:"status,omitempty"`
	// StatusSource is "live" (read by this call) or "cache" (the daemon's
	// last read, StatusAgeSeconds old).
	StatusSource     string `json:"status_source,omitempty"`
	StatusAgeSeconds int64  `json:"status_age_seconds,omitempty"`
	// Windows are the meters from the same read (before any claim).
	Windows []accounts.UsageWindow `json:"windows,omitempty"`
	// Grant is the grant claimed, or that would be claimed, or (when the
	// call refuses) the one the refusal is about.
	Grant *agentclaude.ResetGrant `json:"grant,omitempty"`
	// Organization and AccountEmail are the live profile this call read:
	// who a claim is addressed to.
	Organization string `json:"organization,omitempty"`
	AccountEmail string `json:"account_email,omitempty"`
	// EarlyUse is set when none of the limits the grant clears was exhausted
	// in the status this call went by: a claim then spends the reset before
	// it is needed. Reported, never a refusal (§46.7).
	EarlyUse  bool `json:"early_use,omitempty"`
	Claimable bool `json:"claimable"`
	// Refusal is why the daemon will not send a claim. Nothing was sent.
	Refusal string `json:"refusal,omitempty"`
	// Claimed is true when a claim request was sent upstream.
	Claimed          bool                   `json:"claimed,omitempty"`
	RequestID        string                 `json:"request_id,omitempty"`
	Result           string                 `json:"result,omitempty"`
	AnsweredResult   string                 `json:"answered_result,omitempty"`
	Reason           string                 `json:"reason,omitempty"`
	ResetsLeftBefore *int                   `json:"resets_left_before,omitempty"`
	ResetsLeftAfter  *int                   `json:"resets_left_after,omitempty"`
	Cleared          []string               `json:"cleared,omitempty"`
	WeeklyResetsAt   string                 `json:"weekly_resets_at,omitempty"`
	CooldownUntil    string                 `json:"cooldown_until,omitempty"`
	HTTPStatus       int                    `json:"http_status,omitempty"`
	WindowsAfter     []accounts.UsageWindow `json:"windows_after,omitempty"`
	// ClaimAttempts counts the claim POSTs this call sent, all with
	// RequestID: more than one only after a 429.
	ClaimAttempts int `json:"claim_attempts,omitempty"`
	// RateLimitWaitSeconds is how long this call waited out Anthropic's 429s.
	RateLimitWaitSeconds int64 `json:"rate_limit_wait_seconds,omitempty"`
	// Unconfirmed is set when this claim, or an earlier one on the same
	// grant, may have landed without a definite answer: the daemon keeps
	// RequestID and a retry within 10 minutes resends it.
	Unconfirmed bool   `json:"unconfirmed,omitempty"`
	Error       string `json:"error,omitempty"`
}

type pendingClaudeResetClaim struct {
	requestID string
	at        time.Time
}

func (s Server) handleClaudeReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.AccountRef == nil {
		http.Error(w, "account store not configured", http.StatusInternalServerError)
		return
	}
	var request ClaudeResetRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&request); err != nil {
		http.Error(w, "invalid claude-reset request", http.StatusBadRequest)
		return
	}
	request.Account = strings.TrimSpace(request.Account)
	request.GrantID = strings.TrimSpace(request.GrantID)
	request.RequestID = strings.TrimSpace(request.RequestID)
	request.ExpectedOrganization = strings.TrimSpace(request.ExpectedOrganization)
	if request.Account == "" {
		http.Error(w, "account is required", http.StatusBadRequest)
		return
	}
	if request.GrantID != "" && !agentclaude.ResetGrantIDPattern.MatchString(request.GrantID) {
		http.Error(w, "grant_id is malformed", http.StatusBadRequest)
		return
	}
	if request.RequestID != "" && !agentclaude.ResetRequestIDPattern.MatchString(request.RequestID) {
		http.Error(w, "request_id is malformed", http.StatusBadRequest)
		return
	}
	if request.DryRun != nil && !*request.DryRun && strings.TrimSpace(request.ConfirmAccount) != request.Account {
		// A claim spends a one-time reset: it must name the account twice,
		// or nothing is read at all.
		http.Error(w, "a claim (dry_run:false) must repeat the account in confirm_account; nothing was claimed", http.StatusBadRequest)
		return
	}
	// One deadline, two contexts. caller ends when the caller hangs up: it
	// bounds the waits (for the account's claim lock and out 429s), and no
	// claim is sent once it has ended. work outlives the caller: once a POST
	// is on the wire its answer must be read and logged, whatever the CLI
	// does. The CLI's own timeout is longer than ClaudeResetBudget.
	deadline := time.Now().Add(ClaudeResetBudget)
	caller, cancelCaller := context.WithDeadline(r.Context(), deadline)
	defer cancelCaller()
	work, cancelWork := context.WithDeadline(context.WithoutCancel(r.Context()), deadline)
	defer cancelWork()
	response, status := s.claudeReset(work, caller, request)
	w.Header().Set("Cache-Control", "no-store")
	if status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(response)
		return
	}
	writeJSON(w, response)
}

// claudeResetPacer spends one call's patience with Anthropic's 429s: every
// wait, for every read and claim try, comes out of one bound.
type claudeResetPacer struct {
	// patient is false for a preview: a preview never waits.
	patient bool
	bound   time.Duration
	waited  time.Duration
	tries   int
}

// wait sleeps before the next try after a 429: retryAfter when Anthropic sent
// one, else the next step of claudeResetBackoff. It reports false, without
// waiting, when the wait would pass the bound or leave too little of ctx's
// deadline for the claim, and false when ctx ends during it.
func (p *claudeResetPacer) wait(ctx context.Context, retryAfter time.Duration) bool {
	if p == nil || !p.patient {
		return false
	}
	d := retryAfter
	if d <= 0 {
		d = claudeResetBackoff[min(p.tries, len(claudeResetBackoff)-1)]
	}
	if d < time.Second {
		d = time.Second
	}
	if p.waited+d > p.bound {
		return false
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < d+claudeResetClaimReserve {
		return false
	}
	p.tries++
	if err := claudeResetSleep(ctx, d); err != nil {
		return false
	}
	p.waited += d
	return true
}

func (p *claudeResetPacer) waitedSeconds() int64 {
	if p == nil {
		return 0
	}
	return int64(p.waited / time.Second)
}

// lockClaudeResetClaim takes account's claim lock -- one claim per account at
// a time, start to finish, or two concurrent claims would each read the grant
// as unused and send two requests -- waiting no longer than ctx.
func (r *AccountRef) lockClaudeResetClaim(ctx context.Context, account string) (func(), error) {
	r.claudeResetMu.Lock()
	if r.claudeResetClaimLocks == nil {
		r.claudeResetClaimLocks = map[string]chan struct{}{}
	}
	lock, ok := r.claudeResetClaimLocks[account]
	if !ok {
		lock = make(chan struct{}, 1)
		r.claudeResetClaimLocks[account] = lock
	}
	r.claudeResetMu.Unlock()
	unlock := func() { <-lock }
	// A free lock is taken at once, whatever ctx says; only a wait for a
	// held one ends with ctx.
	select {
	case lock <- struct{}{}:
		return unlock, nil
	default:
	}
	select {
	case lock <- struct{}{}:
		return unlock, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// claudeResetState is the saved-reset block a call goes by, and where it
// came from.
type claudeResetState struct {
	status  *agentclaude.ResetStatus
	windows []accounts.UsageWindow
	source  string
	at      time.Time
}

// readClaudeResetStatus reads account's block live with Claude Code's own
// query, waiting out 429s through pacer. A 401/403 comes back at once as
// *agentclaude.ResetHTTPError, never refreshed or retried.
func (s Server) readClaudeResetStatus(work, caller context.Context, account accounts.Account, pacer *claudeResetPacer) (claudeResetState, error) {
	ref := s.AccountRef
	for {
		// A block is as old as the read that asked for it: stamped when it
		// was sent, so a read begun before a claim never passes for one
		// after it.
		started := time.Now()
		usage, status, err := agentclaude.FetchResetStatus(work, ref.client, account.Token)
		if err == nil {
			windows := displayClaudeWindows(claudeUsageWindows(usage))
			ref.noteClaudeResetStatus(account.ID, account.Token, status, windows, started)
			return claudeResetState{status: status, windows: windows, source: "live", at: started}, nil
		}
		var httpErr *agentclaude.ResetHTTPError
		if errors.As(err, &httpErr) && httpErr.RateLimited() {
			if s.Logger != nil {
				s.Logger.Warn("claude reset status read rate limited", "account", account.ID,
					"retry_after", httpErr.RetryAfter.String(), "waited", pacer.waited.String())
			}
			if pacer.wait(caller, httpErr.RetryAfter) {
				continue
			}
		}
		state := claudeResetState{}
		if usage != nil {
			state.windows = displayClaudeWindows(claudeUsageWindows(usage))
		}
		return state, err
	}
}

// readClaudeResetProfile reads account's profile, waiting out 429s through
// pacer. Read on every call, never cached: the claim goes to the
// organization of the login the account holds NOW.
func (s Server) readClaudeResetProfile(work, caller context.Context, account accounts.Account, pacer *claudeResetPacer) (agentclaude.ResetProfile, error) {
	for {
		profile, err := agentclaude.FetchResetProfile(work, s.AccountRef.client, account.Token)
		var httpErr *agentclaude.ResetHTTPError
		if err != nil && errors.As(err, &httpErr) && httpErr.RateLimited() && pacer.wait(caller, httpErr.RetryAfter) {
			continue
		}
		return profile, err
	}
}

// claudeReset is one call of the route. work carries the budget and outlives
// the caller; caller ends when the caller hangs up (see handleClaudeReset).
func (s Server) claudeReset(work, caller context.Context, request ClaudeResetRequest) (response ClaudeResetResponse, code int) {
	ref := s.AccountRef
	dryRun := request.DryRun == nil || *request.DryRun
	response = ClaudeResetResponse{Account: request.Account, DryRun: dryRun}
	profile, found, err := ref.findClaudeResetProfile(request.Account)
	if err != nil {
		response.Error = err.Error()
		return response, http.StatusConflict
	}
	if !found {
		response.Error = fmt.Sprintf("this daemon holds no Claude account named %q", request.Account)
		return response, http.StatusNotFound
	}
	response.Account = profile.Name
	if !dryRun {
		// The wait for another claim on this account ends in time to leave
		// this one its claim and re-read (claudeResetClaimReserve).
		lockCtx := caller
		if deadline, ok := work.Deadline(); ok {
			var cancelLock context.CancelFunc
			lockCtx, cancelLock = context.WithDeadline(caller, deadline.Add(-claudeResetClaimReserve))
			defer cancelLock()
		}
		unlock, err := ref.lockClaudeResetClaim(lockCtx, profile.Name)
		if err != nil {
			response.Error = "another claim on this account was still running when this call gave up waiting for it; nothing was sent"
			return response, http.StatusConflict
		}
		defer unlock()
	}
	// The sweep's own path to a live token: refreshed only if it has expired,
	// under the store's single-refresher lock. Nothing below refreshes again.
	account, credential, _, err := ref.claudeStore.RefreshCredentialDetailsIfExpired(work, ref.client, profile)
	if err != nil {
		response.Error = "could not get a live token for this account: " + err.Error()
		return response, http.StatusOK
	}
	ref.replace(account)
	if agentclaude.CredentialCannotReadResets(credential) {
		response.Refusal = "this account holds a setup token (scope user:inference), which can neither read nor claim " +
			"a saved reset; only a browser login (sr claude addr) can"
		return response, http.StatusOK
	}
	pacer := &claudeResetPacer{patient: !dryRun, bound: claudeResetRateLimitBound}
	defer func() { response.RateLimitWaitSeconds = pacer.waitedSeconds() }()

	// The block to go by: the daemon's own recent read when it offers a
	// claimable grant, else a live read. A refusal is never taken from the
	// cache: it may predate the limit the account is at now. The cache holds
	// only blocks this login read after the account's last claim POST.
	now := time.Now()
	cached := ref.cachedClaudeResetState(account.ID, account.Token)
	var state claudeResetState
	var grant *agentclaude.ResetGrant
	refusal := ""
	if cached.status != nil && now.Sub(cached.at) < claudeResetCacheFresh {
		if g, why := chooseClaudeResetGrant(cached.status, request.GrantID, now); why == "" {
			state, grant = cached, g
		}
	}
	if state.status == nil {
		live, err := s.readClaudeResetStatus(work, caller, account, pacer)
		var httpErr *agentclaude.ResetHTTPError
		switch {
		case err != nil && errors.As(err, &httpErr) && httpErr.Unauthorized():
			response.Result = agentclaude.ResetResultAuthError
			response.HTTPStatus = httpErr.StatusCode
			response.Error = fmt.Sprintf("Anthropic answered %s to the reset status read; the daemon did not refresh or retry", httpErr.Status)
			return response, http.StatusOK
		case err != nil && cached.status != nil && time.Since(cached.at) < claudeResetCacheFallback:
			// The live read stayed unreadable (a 429 past the bound, a 5xx):
			// the daemon's last read is the best there is, and the claim's
			// answer is Anthropic's word either way.
			if s.Logger != nil {
				s.Logger.Warn("claude reset status read failed; using the cached block", "account", profile.Name,
					"error", err, "cached_age", now.Sub(cached.at).Round(time.Second).String())
			}
			state = cached
		case err != nil:
			response.Error = err.Error()
			response.Windows = live.windows
			return response, http.StatusOK
		default:
			state = live
		}
	}
	// The reads may have waited out 429s: judge the grant now.
	now = time.Now()
	response.Status = state.status
	response.StatusSource = state.source
	if state.source == "cache" {
		response.StatusAgeSeconds = int64(time.Since(state.at) / time.Second)
	}
	response.Windows = state.windows
	if state.status == nil {
		response.Refusal = "Anthropic returned no saved-reset (cedar_ember) block for this account"
		return response, http.StatusOK
	}
	if grant == nil {
		grant, refusal = chooseClaudeResetGrant(state.status, request.GrantID, now)
	}
	response.Grant = grant
	if grant != nil {
		left := grant.ResetsLeft
		response.ResetsLeftBefore = &left
	}
	if refusal != "" {
		response.Refusal = refusal
		return response, http.StatusOK
	}
	resetProfile, err := s.readClaudeResetProfile(work, caller, account, pacer)
	if err != nil {
		response.Refusal = "could not read this account's organization, which the claim is addressed to: " + err.Error()
		return response, http.StatusOK
	}
	response.Organization = resetProfile.OrganizationUUID
	response.AccountEmail = resetProfile.Email
	// Early use is the owner's call (§46.7): reported, never refused.
	response.EarlyUse = !grant.ClearsExhausted(state.status)
	if request.ExpectedOrganization != "" && request.ExpectedOrganization != resetProfile.OrganizationUUID {
		response.Refusal = fmt.Sprintf("the account's login changed: its organization is %s, the caller expected %s; nothing was claimed",
			resetProfile.OrganizationUUID, request.ExpectedOrganization)
		return response, http.StatusOK
	}
	response.Claimable = true
	if dryRun {
		return response, http.StatusOK
	}
	if caller.Err() != nil {
		// The caller left before anything was spent -- a Ctrl-C is a no.
		response.Claimable = false
		response.Refusal = "the caller went away before the claim was sent; nothing was claimed"
		return response, http.StatusOK
	}
	if deadline, ok := work.Deadline(); ok && time.Until(deadline) < claudeResetClaimReserve {
		// The reads and waits spent the call's time: a claim sent now could
		// be cut off before its answer, and "may have landed" is worse than
		// "try again".
		response.Claimable = false
		response.Refusal = "too little of this call's time was left to send the claim and read its answer; nothing was sent, try again"
		return response, http.StatusOK
	}

	// Stamped now, after the profile read and any wait, so a new id's 10
	// minutes start when the claim goes out.
	requestID, reused, err := ref.claudeResetRequestID(profile.Name, grant.ID, request.RequestID, time.Now())
	if err != nil {
		response.Claimable = false
		response.Error = err.Error()
		return response, http.StatusOK
	}
	response.RequestID = requestID
	var claim agentclaude.ResetClaim
	var claimErr error
	for {
		response.ClaimAttempts++
		if s.Logger != nil {
			// Before every POST, at WARN so it survives SUBROUTER_LOG_LEVEL=warn:
			// a claim whose answer is lost can be matched to its retry. Never
			// the token.
			s.Logger.Warn("claude reset claim sending",
				"account", profile.Name, "grant", grant.ID, "grant_label", grant.Label,
				"organization", resetProfile.OrganizationUUID, "early", response.EarlyUse,
				"status_source", state.source, "resets_left_before", grant.ResetsLeft,
				"request_id", requestID, "request_id_reused", reused || response.ClaimAttempts > 1,
				"attempt", response.ClaimAttempts)
		}
		response.Claimed = true
		ref.noteClaudeResetClaimSent(account.ID, time.Now())
		claim, claimErr = agentclaude.ClaimReset(work, ref.client, account.Token, resetProfile.OrganizationUUID, grant.ID, requestID)
		// A 429 touched nothing: wait it out and resend the SAME request_id,
		// inside the call's one bound. The wait ends if the caller leaves.
		if claim.Result != agentclaude.ResetResultRateLimited || !pacer.wait(caller, claim.RetryAfter) {
			break
		}
	}
	response.Result = claim.Result
	response.AnsweredResult = claim.AnsweredResult
	response.Reason = claim.Reason
	response.ResetsLeftAfter = claim.ResetsLeft
	response.Cleared = claim.Cleared
	response.WeeklyResetsAt = claim.WeeklyResetsAt
	response.CooldownUntil = claim.CooldownUntil
	response.HTTPStatus = claim.HTTPStatus
	if claimErr != nil {
		response.Error = claimErr.Error()
	}
	// The request_id's life follows Claude Code's: a settled answer lets it
	// go; a claim that may have landed keeps it; auth_error, rate_limited
	// and cooldown keep an earlier unconfirmed claim's id but record none of
	// their own.
	switch {
	case claim.Settled():
		ref.forgetClaudeResetRequest(profile.Name, grant.ID)
	case claim.MayHaveLanded():
		response.Unconfirmed = true
	case reused:
		response.Unconfirmed = true
	default:
		ref.forgetClaudeResetRequest(profile.Name, grant.ID)
	}
	// A reset -- or an unconfirmed answer that says what it cleared, which
	// Claude Code applies too -- drops the marks and reads the account again.
	// Whatever the answer, no block read before this POST is gone by again
	// (noteClaudeResetClaimSent): the next call reads live.
	if claim.Result == agentclaude.ResetResultReset || (claim.MayHaveLanded() && len(claim.Cleared) > 0) {
		response.WindowsAfter = s.afterClaudeReset(work, account)
	}
	if s.Logger != nil {
		after := ""
		if claim.ResetsLeft != nil {
			after = fmt.Sprint(*claim.ResetsLeft)
		}
		s.Logger.Warn("claude reset claim",
			"account", profile.Name, "grant", grant.ID, "grant_label", grant.Label,
			"result", claim.Result, "answered_result", claim.AnsweredResult, "reason", claim.Reason,
			"http_status", claim.HTTPStatus, "unconfirmed", response.Unconfirmed, "early", response.EarlyUse,
			"resets_left_before", grant.ResetsLeft, "resets_left_after", after,
			"request_id", requestID, "attempts", response.ClaimAttempts,
			"rate_limit_waited", pacer.waited.String(), "error", response.Error)
	}
	return response, http.StatusOK
}

// afterClaudeReset makes routing see the reset at once: the exhaustion marks
// and admission pauses the limit left behind are dropped, the cached usage
// read and usage-status snapshot are thrown away, and the account is read
// again.
func (s Server) afterClaudeReset(ctx context.Context, account accounts.Account) []accounts.UsageWindow {
	ref := s.AccountRef
	if s.SchedulerRef != nil {
		s.SchedulerRef.ClearExhausted(schedulerAccountProvider(accounts.ProviderClaude), account.ID)
		s.SchedulerRef.SetUpdatedAt(time.Time{})
	}
	// The 429 that set the marks also paused the account's admission gate.
	s.admissionGate().clearAccount(schedulerAccountProvider(accounts.ProviderClaude), account.ID)
	ref.forgetUsageWindows(account)
	ref.forgetClaudeResetStatus(account.ID)
	ref.InvalidateUsageStatusCache()
	windows, _, _, _, err := ref.fetchUsageWindowsCachedReason(ctx, ref.client, account)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Warn("claude reset re-read failed", "account", account.ID, "error", err)
		}
		return nil
	}
	ref.InvalidateUsageStatusCache()
	return displayClaudeWindows(windows)
}

// displayClaudeWindows keeps the meters a person reads (5h, 7d, the model
// weeklies), without the paid-usage reporting window.
func displayClaudeWindows(windows []accounts.UsageWindow) []accounts.UsageWindow {
	out := make([]accounts.UsageWindow, 0, len(windows))
	for _, window := range windows {
		if window.ExtraUsage != nil {
			continue
		}
		out = append(out, window)
	}
	return out
}

// findClaudeResetProfile resolves a profile by name, or by its account email
// when exactly one profile carries it.
func (r *AccountRef) findClaudeResetProfile(name string) (agentclaude.Profile, bool, error) {
	if profile, ok := r.claudeStore.FindProfile(name); ok {
		return profile, true, nil
	}
	if !strings.Contains(name, "@") {
		return agentclaude.Profile{}, false, nil
	}
	want := strings.ToLower(strings.TrimSpace(name))
	var matches []agentclaude.Profile
	for _, profile := range r.claudeStore.ListProfiles() {
		if claudeProfileEmail(r.claudeStore, profile.Name) == want {
			matches = append(matches, profile)
		}
	}
	switch len(matches) {
	case 0:
		return agentclaude.Profile{}, false, nil
	case 1:
		return matches[0], true, nil
	default:
		names := make([]string, 0, len(matches))
		for _, profile := range matches {
			names = append(names, profile.Name)
		}
		return agentclaude.Profile{}, false, fmt.Errorf("%d Claude profiles sign in as %s (%s); name one", len(matches), name, strings.Join(names, ", "))
	}
}

// chooseClaudeResetGrant picks the grant a claim would spend, or says why
// there is none. Like Claude Code it claims only Anthropic's next_grant_id,
// and only when that grant is usable now, not paused and not expired; a
// caller that names a grant must name that one. It never picks on its own
// among several.
func chooseClaudeResetGrant(status *agentclaude.ResetStatus, grantID string, now time.Time) (*agentclaude.ResetGrant, string) {
	grantByID := func(id string) *agentclaude.ResetGrant {
		for i := range status.Grants {
			if status.Grants[i].ID == id {
				return &status.Grants[i]
			}
		}
		return nil
	}
	next := grantByID(status.NextGrantID)
	// shown is the grant a refusal is about.
	shown := next
	if shown == nil {
		for i := range status.Grants {
			if status.Grants[i].ResetsLeft > 0 && !status.Grants[i].Expired(now) {
				shown = &status.Grants[i]
				break
			}
		}
	}
	if shown == nil && len(status.Grants) > 0 {
		shown = &status.Grants[0]
	}
	if grantID != "" {
		named := grantByID(grantID)
		if named == nil {
			return nil, fmt.Sprintf("this account has no grant %q", grantID)
		}
		shown = named
	}
	if agentclaude.ResetStatusUnread(status) {
		return shown, "Anthropic could not read this account's saved reset right now (ineligible_reason: unavailable); try again in a moment"
	}
	if !status.Eligible {
		reason := status.IneligibleReason
		if reason == "" {
			reason = "none given"
		}
		return shown, "Anthropic says this account is not eligible (ineligible_reason: " + reason + ")"
	}
	if until, ok := agentclaude.ParseResetTime(status.CooldownUntil); ok && now.Before(until) {
		return shown, "Anthropic says resets are cooling down until " + until.UTC().Format(time.RFC3339)
	}
	if next == nil {
		if shown != nil {
			if why := claudeResetGrantUnusable(*shown, status, now); why != "" {
				return shown, why
			}
		}
		if len(status.Grants) == 0 {
			return nil, "this account has no saved reset"
		}
		return shown, "Anthropic offers no reset to claim now (next_grant_id is null)"
	}
	if grantID != "" && grantID != next.ID {
		return shown, fmt.Sprintf("Anthropic offers grant %q next, not %q; only the next grant can be claimed", next.ID, grantID)
	}
	if why := claudeResetGrantUnusable(*next, status, now); why != "" {
		return next, why
	}
	return next, ""
}

// claudeResetGrantUnusable says why a grant cannot be claimed now, or "".
func claudeResetGrantUnusable(grant agentclaude.ResetGrant, status *agentclaude.ResetStatus, now time.Time) string {
	switch {
	case grant.ResetsLeft <= 0:
		return "this account's saved reset is already used"
	case grant.Expired(now):
		return "the saved reset expired " + grant.EndsAt
	case grant.Paused:
		return "Anthropic has paused this saved reset"
	case !grant.UsableNow && grant.UseRequiresLimit && !status.AtLimit:
		return "not usable now: this reset only applies at a usage limit, and the account is not at one"
	case !grant.UsableNow:
		why := "Anthropic says this saved reset is not usable right now"
		if len(grant.Blocking) > 0 {
			why += " (blocking: " + strings.Join(grant.Blocking, ", ") + ")"
		}
		return why
	}
	return ""
}

// claudeResetRequestID is the request_id for a claim on (account, grant): the
// caller's, else the one an unconfirmed claim on the same grant minted in the
// last 10 minutes, else a new one. It is recorded before it is returned, so
// it is on record before the POST goes out; its time is stamped only when the
// id changes, so reuse never slides the 10-minute window. reused reports an
// id that an earlier claim already sent.
func (r *AccountRef) claudeResetRequestID(account, grantID, requested string, now time.Time) (requestID string, reused bool, err error) {
	key := account + "\x00" + grantID
	r.claudeResetMu.Lock()
	defer r.claudeResetMu.Unlock()
	if r.claudeResetPending == nil {
		r.claudeResetPending = map[string]pendingClaudeResetClaim{}
	}
	pending, havePending := r.claudeResetPending[key]
	if havePending && now.Sub(pending.at) >= claudeResetPendingTTL {
		havePending = false
	}
	requestID = requested
	if requestID == "" && havePending {
		requestID = pending.requestID
	}
	if requestID == "" {
		minted, err := agentclaude.NewResetRequestID()
		if err != nil {
			return "", false, err
		}
		requestID = minted
	}
	reused = havePending && pending.requestID == requestID
	if !reused {
		r.claudeResetPending[key] = pendingClaudeResetClaim{requestID: requestID, at: now}
	}
	return requestID, reused, nil
}

func (r *AccountRef) forgetClaudeResetRequest(account, grantID string) {
	r.claudeResetMu.Lock()
	defer r.claudeResetMu.Unlock()
	delete(r.claudeResetPending, account+"\x00"+grantID)
}

// claudeResetStatusEntry is one account's saved reset as Claude Code's own
// read (?cedar_ember=1&skip_spend=1) last saw it: for the row (info) and,
// decoded whole, for a claim (status).
type claudeResetStatusEntry struct {
	info   *accounts.ComplimentaryResetInfo
	status *agentclaude.ResetStatus
	// windows are the meters the same read carried.
	windows []accounts.UsageWindow
	// login marks the token that read status (claudeResetLoginMark).
	login string
	// at is when the read was sent.
	at       time.Time
	inflight bool
}

// claudeResetLoginMark stands for the token a block was read with, so a claim
// goes by a cached block only for the login it holds now: a profile logged in
// again (or refreshed) reads live. A hash, never the token.
func claudeResetLoginMark(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:8])
}

// noteClaudeResetClaimSent records that a claim POST for accountID goes out
// at: every block read before it is outdated (cachedClaudeResetState).
func (r *AccountRef) noteClaudeResetClaimSent(accountID string, at time.Time) {
	r.claudeResetMu.Lock()
	defer r.claudeResetMu.Unlock()
	if r.claudeResetClaimSentAt == nil {
		r.claudeResetClaimSentAt = map[string]time.Time{}
	}
	r.claudeResetClaimSentAt[accountID] = at
}

// cachedClaudeResetState is accountID's saved-reset block as this daemon last
// read it -- the sweep's usage read, or Claude Code's own read (the fallback,
// or an earlier claim) -- whichever is newer, with the windows of the sweep's
// read when it has one. Only a block read with token (the login the account
// holds now) and sent after the account's last claim POST counts: a claim
// that may have landed outdates every block read before it. A block Anthropic
// could not read itself (ineligible_reason "unavailable") is no block. Zero
// when there is none.
func (r *AccountRef) cachedClaudeResetState(accountID, token string) claudeResetState {
	var out claudeResetState
	login := claudeResetLoginMark(token)
	r.claudeResetMu.Lock()
	claimSent := r.claudeResetClaimSentAt[accountID]
	r.claudeResetMu.Unlock()
	usable := func(status *agentclaude.ResetStatus, readWith string, at time.Time) bool {
		return status != nil && !agentclaude.ResetStatusUnread(status) && readWith == login && at.After(claimSent)
	}
	r.usageWindowsMu.Lock()
	entry, ok := r.usageWindows[accountID+"\x00"+string(accounts.ProviderClaude)]
	r.usageWindowsMu.Unlock()
	if ok {
		out.windows = displayClaudeWindows(entry.windows)
		// entry.resetAt, not entry.at: upstream's response evidence (#544,
		// claude_response_evidence.go) moves entry.at forward on every Claude
		// reply, so a reset block read before a claim would look newer than
		// claimSent (§46.7). resetAt is when the block itself was read.
		if usable(entry.resetStatus, entry.resetLogin, entry.resetAt) {
			out.status, out.at = entry.resetStatus, entry.resetAt
		}
	}
	r.claudeResetMu.Lock()
	own, ok := r.claudeResetStatuses[accountID]
	r.claudeResetMu.Unlock()
	if ok && usable(own.status, own.login, own.at) && own.at.After(out.at) {
		out.status, out.at = own.status, own.at
		if len(own.windows) > 0 {
			out.windows = own.windows
		}
	}
	if out.status == nil {
		return claudeResetState{}
	}
	out.source = "cache"
	return out
}

// claudeResetFallbackTimeout bounds one fallback read.
const claudeResetFallbackTimeout = 15 * time.Second

// EnableClaudeResetFallback turns on the sweep's fallback read of the saved
// reset (PATCHES §46): when a Claude account's usage read (?cedar_ember=1,
// without skip_spend, which Claude Code itself never sends) comes back with
// no block, the daemon reads the block with Claude Code's own query at most
// once per every for that account, in the background, and publishes it. When
// the sweep's read does carry the block, nothing extra is ever sent.
func (r *AccountRef) EnableClaudeResetFallback(every time.Duration) {
	if r == nil {
		return
	}
	r.claudeResetMu.Lock()
	r.claudeResetFallbackEvery = every
	r.claudeResetMu.Unlock()
}

// noteClaudeResetStatus records a saved reset read with Claude Code's own
// query (the claim path) with token, sent at started, so the fallback serves
// it without reading again, and the next claim can go by it (with the meters
// the read carried).
func (r *AccountRef) noteClaudeResetStatus(accountID, token string, status *agentclaude.ResetStatus, windows []accounts.UsageWindow, started time.Time) {
	r.claudeResetMu.Lock()
	defer r.claudeResetMu.Unlock()
	if r.claudeResetStatuses == nil {
		r.claudeResetStatuses = map[string]claudeResetStatusEntry{}
	}
	entry := r.claudeResetStatuses[accountID]
	entry.info, entry.status, entry.windows = agentclaude.ResetInfo(status, started), status, windows
	entry.login, entry.at = claudeResetLoginMark(token), started
	r.claudeResetStatuses[accountID] = entry
}

func (r *AccountRef) forgetClaudeResetStatus(accountID string) {
	r.claudeResetMu.Lock()
	defer r.claudeResetMu.Unlock()
	if entry, ok := r.claudeResetStatuses[accountID]; ok && !entry.inflight {
		delete(r.claudeResetStatuses, accountID)
	} else if ok {
		entry.info, entry.status, entry.windows, entry.at = nil, nil, nil, time.Time{}
		r.claudeResetStatuses[accountID] = entry
	}
}

// finishClaudeResetFallback stores one fallback read of id's block, sent at
// started with token. No invalidation: the next sweep publishes the answer,
// and a sweep in flight keeps its snapshot.
func (r *AccountRef) finishClaudeResetFallback(id, token string, started time.Time, usage *agentclaude.UsageResponse, status *agentclaude.ResetStatus, err error) {
	r.claudeResetMu.Lock()
	defer r.claudeResetMu.Unlock()
	if r.claudeResetStatuses == nil {
		r.claudeResetStatuses = map[string]claudeResetStatusEntry{}
	}
	current := r.claudeResetStatuses[id]
	current.inflight = false
	switch {
	case !started.After(r.claudeResetClaimSentAt[id]):
		// Sent before a claim on this account that came after it: what it
		// says may already be spent. Nothing is published, and the next
		// sweep reads again.
		current.info, current.status, current.windows, current.login, current.at = nil, nil, nil, "", time.Time{}
	case err == nil:
		current.info, current.status = agentclaude.ResetInfo(status, started), status
		current.windows = displayClaudeWindows(claudeUsageWindows(usage))
		current.login, current.at = claudeResetLoginMark(token), started
	default:
		// A failed read is not "none": nothing is published, and the next
		// try waits the interval like a good one.
		current.info, current.status, current.windows, current.login = nil, nil, nil, ""
		current.at = started
	}
	r.claudeResetStatuses[id] = current
}

// claudeResetFromFallback is account's saved reset from the fallback read,
// or nil. With start, it starts a background read when the last one is older
// than the fallback interval (or there is none), and serves the previous
// answer up to twice the interval old meanwhile. Without start it only serves
// that answer: nothing is sent, marked in flight or stamped. Off unless
// EnableClaudeResetFallback.
func (r *AccountRef) claudeResetFromFallback(account accounts.Account, start bool) *accounts.ComplimentaryResetInfo {
	if r == nil || account.Provider != accounts.ProviderClaude || strings.TrimSpace(account.Token) == "" {
		return nil
	}
	now := time.Now()
	r.claudeResetMu.Lock()
	defer r.claudeResetMu.Unlock()
	every := r.claudeResetFallbackEvery
	if every <= 0 {
		return nil
	}
	if r.claudeResetStatuses == nil {
		r.claudeResetStatuses = map[string]claudeResetStatusEntry{}
	}
	entry, ok := r.claudeResetStatuses[account.ID]
	if start && (!ok || now.Sub(entry.at) >= every) && !entry.inflight {
		entry.inflight = true
		r.claudeResetStatuses[account.ID] = entry
		client, token, id := r.client, account.Token, account.ID
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), claudeResetFallbackTimeout)
			defer cancel()
			started := time.Now()
			usage, status, err := agentclaude.FetchResetStatus(ctx, client, token)
			r.finishClaudeResetFallback(id, token, started, usage, status, err)
		}()
	}
	if entry.info == nil || entry.at.IsZero() || now.Sub(entry.at) >= 2*every {
		return nil
	}
	return cloneResetInfo(entry.info)
}

// usageReadReset is the saved reset a fresh usage read of account stands for:
// the block that read carried, else the fallback read's, else nil. The sweep
// publishes it on the row, and the scheduled score refresh holds the account
// on it (resetHeldUntil), so both read the reset in the same order.
//
// Only the sweep starts a fallback read (startFallback). The refresh takes
// the answer the sweep's last fallback read stored, and never sends one: the
// sweep knows a setup token cannot read the reset and starts none for it, and
// the refresh, which does not, would send one an hour per setup token, every
// one refused (PATCHES §56).
func (r *AccountRef) usageReadReset(account accounts.Account, startFallback bool) *accounts.ComplimentaryResetInfo {
	if info := r.cachedUsageReset(account); info != nil {
		return info
	}
	return r.claudeResetFromFallback(account, startFallback)
}

// resetHeldUntil is the hold carry rule of PATCHES §56: the ResetHeldUntil of
// a score built from a fresh usage read, given the saved reset that read
// stands for (info) and the score it replaces (seed).
//
// A Known cedar_ember reading decides. An Available reading holds the account
// until its WeekHeldUntil, the soonest expiry among the live grants that
// clear the general week, when that parses and lies ahead; anything else it
// says -- consumed, expired, ineligible, an expiry that does not parse, or
// only grants that clear the Fable limit -- is no hold. A Fable claim zeroes
// the Fable meter and gives back the week Fable usage spent; it does not
// refill the week, so nothing left in the week is lost at the claim and
// there is nothing to spend down first.
//
// A reading that knows nothing carries the seed's hold: nil (no block, the
// fallback not back yet or failed) and Known false (Anthropic's
// "unavailable", a setup token). Zeroing there took a holder out of early
// burndown on every unread answer and put it back on the next, and the
// fallback can stay empty for an hour. The carried hold is an absolute
// expiry, so it ends on its own, and the forced re-read after a claim
// (afterClaudeReset) is a Known reading that clears it. Any other Source --
// Codex's reset credits fill the same struct -- is not this program and holds
// nothing.
func resetHeldUntil(info *accounts.ComplimentaryResetInfo, seed selectacct.Score, now time.Time) time.Time {
	if info == nil {
		return seed.ResetHeldUntil
	}
	if info.Source != agentclaude.ResetProgram {
		return time.Time{}
	}
	if !info.Known {
		return seed.ResetHeldUntil
	}
	if !info.Available {
		return time.Time{}
	}
	expires, ok := agentclaude.ParseResetTime(info.WeekHeldUntil)
	if !ok || !expires.After(now) {
		return time.Time{}
	}
	return expires
}

// heldWeekResetSeconds is the hold a peer's usage-status body states for an
// account, from its two keys (NormalizedUsage). A peer since PATCHES §64
// publishes both for a week hold and neither otherwise, so the week key
// decides. The legacy key alone is a peer from before §64: it held an account
// on any grant, a Fable-only one too, and its body ranks as it did then.
func heldWeekResetSeconds(week, legacy *float64) *float64 {
	if week != nil {
		return week
	}
	return legacy
}
