package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
)

// Claude cost tiers, for status output (upstream #569/#570, adapted):
//
//  1. plan: Claude Max/Pro subscription quota (OAuth and setup-token
//     profiles). Already paid for.
//  2. api-credits: Anthropic API keys. Each is expected to belong to a
//     Console organization funded by free monthly credits, so it costs no
//     money until its balance is spent.
//
// With both spent the pool answers its own 503. Upstream's third tier, a
// subscription's paid extra usage, is never selected or retried onto here
// (PATCHES §23: nothing spends money), and its credit promotion (an API key
// whose grant would lapse unspent ranked ahead of plans) is not taken
// either: routing ranks on the fork's comparator (§10). What this file keeps
// is the metering and display of an API key's credit, the hold that takes a
// key with a spent balance out of routing until its re-probe or period end,
// and the revoked-credential overlay (#576, #625).
const (
	claudeCostTierPlan       = "plan"
	claudeCostTierAPICredits = "api-credits"
)

// ClaudeCostOrder is the human form of the order above, for status output.
const ClaudeCostOrder = "plan > API credits > 503 (paid extra usage is never used)"

// claudeCreditExhaustionHold is how long a key whose credit balance is spent
// stays out of routing before one request probes it again. Anthropic does not
// expose a key's balance or its grant's reset date, so the hold is a re-probe
// interval rather than a reset time: a probe costs one rejected request
// (Anthropic answers it before any tokens are billed), and a monthly grant or
// a manual top-up is picked up within the hour.
const claudeCreditExhaustionHold = time.Hour

// claudeCreditExhaustedMessage matches Anthropic's billing rejection, e.g.
// "Your credit balance is too low to access the Anthropic API. Please go to
// Plans & Billing to upgrade or purchase credits."
func claudeCreditExhaustedMessage(body []byte) bool {
	message := normalizedProviderErrorMessage(body)
	if strings.Contains(message, "credit balance") &&
		(strings.Contains(message, "too low") || strings.Contains(message, "insufficient") || strings.Contains(message, "not enough")) {
		return true
	}
	if strings.Contains(message, "insufficient credits") || strings.Contains(message, "credits exhausted") {
		return true
	}
	return claudeBillingErrorType(body)
}

func claudeBillingErrorType(body []byte) bool {
	var value any
	if json.Unmarshal(body, &value) != nil {
		return false
	}
	var visit func(any) bool
	visit = func(current any) bool {
		switch node := current.(type) {
		case map[string]any:
			for key, child := range node {
				if key == "type" || key == "code" || key == "error_code" {
					if text, ok := child.(string); ok {
						normalized := strings.ToLower(strings.TrimSpace(text))
						if normalized == "billing_error" || normalized == "insufficient_credits" || normalized == "credit_balance_exhausted" {
							return true
						}
					}
				}
				if visit(child) {
					return true
				}
			}
		case []any:
			for _, child := range node {
				if visit(child) {
					return true
				}
			}
		}
		return false
	}
	return visit(value)
}

func claudeCreditExhaustedStatus(code int) bool {
	return code == http.StatusBadRequest || code == http.StatusPaymentRequired || code == http.StatusForbidden
}

// responseClaudeCreditExhausted reports whether an Anthropic error response
// says the organization's prepaid credit balance is spent. It reads at most
// usageLimitInspectMaxBytes and always leaves the body readable for the
// caller. Successful responses (including streams) are never read.
func responseClaudeCreditExhausted(response *http.Response) (bool, error) {
	if response == nil || response.Body == nil || !claudeCreditExhaustedStatus(response.StatusCode) {
		return false, nil
	}
	body := response.Body
	prefix, err := io.ReadAll(io.LimitReader(body, usageLimitInspectMaxBytes+1))
	if err != nil || int64(len(prefix)) > usageLimitInspectMaxBytes {
		response.Body = prefixReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), body), Closer: body}
		return false, err
	}
	closeErr := body.Close()
	response.Body = io.NopCloser(bytes.NewReader(prefix))
	if closeErr != nil {
		return false, closeErr
	}
	return claudeCreditExhaustedMessage(prefix), nil
}

// markClaudeCreditExhausted holds a Claude API key whose balance Anthropic
// reports spent. With credit state the hold lasts until the grant period
// rolls over, with one re-probe a day; without it, one hour. The key itself
// is never logged, only its account ID.
func (s Server) markClaudeCreditExhausted(accountID string) {
	if accountID == "" {
		return
	}
	until := time.Now().Add(claudeCreditExhaustionHold)
	if s.claudeCosts != nil {
		until = s.claudeCosts.markExhausted(accountID)
	}
	if s.SchedulerRef != nil {
		// Upstream's MarkCreditExhaustedUntil (#569) is not in this fork's
		// selectacct; the account-wide unavailable mark holds the key out the
		// same way and reads back through ExhaustedUntilFor.
		s.SchedulerRef.MarkAccountUnavailableUntil(schedulerAccountProvider(accounts.ProviderClaude), accountID, until)
	}
	if s.Logger != nil {
		s.Logger.Warn("claude API key credit balance spent; holding it out until re-probe or period end",
			"account", accountID, "until", until.UTC().Format(time.RFC3339))
	}
}

// withClaudeCostTiers labels every Claude status row with the cost tier the
// router would spend it in, an API key's credit state, and a revoked OAuth
// credential.
func (s Server) withClaudeCostTiers(statuses []AccountUsageStatus) []AccountUsageStatus {
	out := append([]AccountUsageStatus(nil), statuses...)
	s.ensureClaudeCreditKeys(s.accountListContext(context.Background()))
	now := time.Now()
	var views map[string]claudeCreditView
	costFile := newClaudeCostStateFile()
	if s.claudeCosts != nil {
		views = s.claudeCosts.views(now)
		costFile = s.claudeCosts.snapshot()
	}
	for i := range out {
		if accountProviderFor(out[i].Provider) != accounts.ProviderClaude {
			continue
		}
		switch out[i].AuthMode {
		case accounts.AuthModeAPIKey:
			out[i].CostTier = claudeCostTierAPICredits
			out[i].CostTierRank = 2
			if view, ok := views[out[i].ID]; ok {
				out[i].Credit = claudeCreditStatusFromView(view)
				if !view.HeldUntil.IsZero() {
					out[i].CreditsExhaustedUntil = view.HeldUntil.UTC()
				}
			} else if s.SchedulerRef != nil {
				if until, held := s.SchedulerRef.ExhaustedUntilFor(schedulerAccountProvider(accounts.ProviderClaude), out[i].ID, ""); held && until.After(now) {
					out[i].CreditsExhaustedUntil = until.UTC()
				}
			}
		case accounts.AuthModeOAuth:
			out[i].CostTier = claudeCostTierPlan
			out[i].CostTierRank = 1
			if since, revoked := claudeRevokedSince(costFile, out[i].ID, s.claudeCredentialIdentity(out[i].ID)); revoked {
				out[i].RevokedSince = since.UTC()
				out[i].RevokedKind = costFile.Revoked[out[i].ID].kind()
			}
		}
	}
	return out
}

// ClaudeCreditStatus is one API key's credit state in usage-status.
type ClaudeCreditStatus struct {
	GrantUSD     float64   `json:"grant_usd"`
	SpentUSD     float64   `json:"spent_usd"`
	RemainingUSD float64   `json:"remaining_usd"`
	BurnPerHour  float64   `json:"burn_usd_per_hour"`
	PeriodStart  time.Time `json:"period_start"`
	ExpiresAt    time.Time `json:"expires_at"`
	Defaulted    bool      `json:"defaulted,omitempty"`
	Pace         string    `json:"pace"`
	// Promoted is upstream's credit promotion (#570). This fork's routing
	// never promotes a key (see the tier comment above), so it never sets
	// it; the field stays for the wire.
	Promoted bool `json:"promoted,omitempty"`
}

func claudeCreditStatusFromView(view claudeCreditView) *ClaudeCreditStatus {
	round := func(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }
	return &ClaudeCreditStatus{
		GrantUSD:     round(view.AmountUSD),
		SpentUSD:     round(view.SpentUSD),
		RemainingUSD: round(view.RemainingUSD),
		BurnPerHour:  round(view.BurnPerHour),
		PeriodStart:  view.PeriodStart.UTC(),
		ExpiresAt:    view.ExpiresAt.UTC(),
		Defaulted:    view.Defaulted,
		Pace:         view.Pace,
	}
}

// claudeAuthMode resolves an account's auth mode. A response tagged by the
// failover layer carries only the account ID and credential version, so the
// mode comes from the loaded account list.
func (s Server) claudeAuthMode(account accounts.Account) accounts.AuthMode {
	if account.AuthMode != "" {
		return account.AuthMode
	}
	if s.AccountRef != nil {
		s.AccountRef.mu.RLock()
		defer s.AccountRef.mu.RUnlock()
		for _, candidate := range s.AccountRef.accounts {
			if sameCredentialProvider(candidate.Provider, accounts.ProviderClaude) && candidate.ID == account.ID {
				return candidate.AuthMode
			}
		}
		return ""
	}
	for _, candidate := range s.Accounts {
		if accountProviderOrCodex(candidate) == accounts.ProviderClaude && candidate.ID == account.ID {
			return candidate.AuthMode
		}
	}
	return ""
}

// claudeCredentialIdentity is the current credential identity of a Claude
// account, or "" when the account is unknown.
func (s Server) claudeCredentialIdentity(accountID string) string {
	if s.AccountRef != nil {
		return s.AccountRef.credentialSnapshot(accounts.ProviderClaude, accountID).CredentialIdentity()
	}
	for _, account := range s.Accounts {
		if accountProviderOrCodex(account) == accounts.ProviderClaude && account.ID == accountID {
			return account.CredentialIdentity()
		}
	}
	return ""
}

// ensureClaudeCreditKeys gives every Claude API key a credit record, so a
// newly added key is paced from its add date with the default grant.
func (s Server) ensureClaudeCreditKeys(all []accounts.Account) {
	if s.claudeCosts == nil {
		return
	}
	keys := map[string]time.Time{}
	for _, account := range all {
		if accountProviderOrCodex(account) == accounts.ProviderClaude && account.AuthMode == accounts.AuthModeAPIKey {
			keys[account.ID] = account.AddedAt
		}
	}
	s.claudeCosts.ensureKeys(keys)
}

// withClaudeCostOverlay applies credit pacing and revocations to a routing
// view of the scheduler. It is never written back to SchedulerRef.
func (s Server) withClaudeCostOverlay(base selectacct.Scheduler) selectacct.Scheduler {
	if s.claudeCosts == nil {
		return base
	}
	now := time.Now()
	file := s.claudeCosts.snapshot()
	if len(file.Keys) == 0 && len(file.Revoked) == 0 {
		return base
	}
	scores := make([]selectacct.Score, 0, len(file.Keys)+len(file.Revoked))
	for id, state := range file.Keys {
		view := evaluateClaudeCredit(id, *state, now)
		if view.HeldUntil.IsZero() {
			// Upstream also overlaid a funded key's credit expiry and
			// promotion here (#570); routing does not promote (see above).
			continue
		}
		score := base.ScoreFor(accounts.ProviderClaude, id)
		score.AccountID, score.Provider = id, accounts.ProviderClaude
		score.MissingModelSupport = selectacct.ModelSupportUnknown
		score.Headroom, score.ShortHeadroom, score.ModelScores = 0, 0, nil
		score.ExhaustedResetAt, score.ExhaustedResetUnknown = view.HeldUntil, false
		scores = append(scores, score)
	}
	for id := range file.Revoked {
		if _, revoked := claudeRevokedSince(file, id, s.claudeCredentialIdentity(id)); !revoked {
			continue
		}
		score := base.ScoreFor(accounts.ProviderClaude, id)
		score.AccountID, score.Provider = id, accounts.ProviderClaude
		score.Headroom, score.ShortHeadroom, score.ModelScores = 0, 0, nil
		// A revoked credential only heals by re-login, which changes the
		// credential and drops this overlay. The far reset keeps failover
		// from treating it as a last-resort candidate.
		score.ExhaustedResetAt, score.ExhaustedResetUnknown = now.Add(365*24*time.Hour), false
		scores = append(scores, score)
	}
	// One copy-then-modify WithScore per overlaid account: the fork's
	// selectacct has no upstream WithScores, and WithScore keeps every other
	// field of the view (reserves, live counts, machine view).
	for _, score := range scores {
		base = base.WithScore(score)
	}
	return base
}

// claudeCostStore returns the server's Claude cost store, falling back to
// the account ref's shared one.
func (s Server) claudeCostStore() *claudeCostStore {
	if s.claudeCosts != nil {
		return s.claudeCosts
	}
	return s.AccountRef.claudeCostStore()
}

// claudeCostStore returns the ref's shared Claude cost store, creating it
// under the store dir on first use so a sweep that starts before Handler
// still sees revoked credentials. Nil when the ref has no store dir.
func (r *AccountRef) claudeCostStore() *claudeCostStore {
	if r == nil {
		return nil
	}
	if costs := r.claudeCosts.Load(); costs != nil {
		return costs
	}
	if r.store.Dir == "" {
		return nil
	}
	r.claudeCosts.CompareAndSwap(nil, newClaudeCostStore(filepath.Join(r.store.Dir, "claude-cost-state.json")))
	return r.claudeCosts.Load()
}

// claudeCredentialRevokedSince reports whether account is a Claude OAuth
// account whose current credential Anthropic has revoked. The record is
// keyed by credential, so a re-login clears it.
func claudeCredentialRevokedSince(costs *claudeCostStore, account accounts.Account) (time.Time, bool) {
	if costs == nil || account.ID == "" || accountProviderOrCodex(account) != accounts.ProviderClaude ||
		account.AuthMode == accounts.AuthModeAPIKey {
		return time.Time{}, false
	}
	return claudeRevokedSince(costs.snapshot(), account.ID, account.CredentialIdentity())
}

// meterClaudeCredit returns the spend recorder for a Claude API key response,
// or nil when the account is not metered.
func (s Server) meterClaudeCredit(account accounts.Account) func(model string, usage tokenUsage) {
	if s.claudeCosts == nil || accountProviderOrCodex(account) != accounts.ProviderClaude ||
		account.ID == "" || s.claudeAuthMode(account) != accounts.AuthModeAPIKey {
		return nil
	}
	store, id := s.claudeCosts, account.ID
	return func(model string, usage tokenUsage) {
		store.recordSpend(id, claudeAPIListCostUSD(model, usage))
	}
}

// handleClaudeKeyCredit serves GET (list) and POST (set) for Claude API-key
// credit grants: POST {"account_id","amount_usd","expires_at","spent_usd"}.
func (s Server) handleClaudeKeyCredit(w http.ResponseWriter, r *http.Request) {
	if s.claudeCosts == nil {
		http.Error(w, "credit state unavailable", http.StatusServiceUnavailable)
		return
	}
	all := s.accountListContext(r.Context())
	s.ensureClaudeCreditKeys(all)
	switch r.Method {
	case http.MethodGet:
		out := map[string]*ClaudeCreditStatus{}
		for id, view := range s.claudeCosts.views(time.Now()) {
			out[id] = claudeCreditStatusFromView(view)
		}
		writeJSON(w, out)
	case http.MethodPost:
		var input struct {
			AccountID string    `json:"account_id"`
			AmountUSD float64   `json:"amount_usd"`
			ExpiresAt time.Time `json:"expires_at"`
			SpentUSD  *float64  `json:"spent_usd"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&input); err != nil {
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		id := strings.TrimSpace(input.AccountID)
		found := false
		for _, account := range all {
			if account.ID == id && accountProviderOrCodex(account) == accounts.ProviderClaude && account.AuthMode == accounts.AuthModeAPIKey {
				found = true
				break
			}
		}
		if !found {
			http.Error(w, "Claude API-key account not found", http.StatusNotFound)
			return
		}
		if input.AmountUSD < 0 || input.AmountUSD > 1_000_000 || (input.SpentUSD != nil && *input.SpentUSD < 0) {
			http.Error(w, "invalid amount", http.StatusBadRequest)
			return
		}
		if input.AmountUSD == 0 && input.ExpiresAt.IsZero() && input.SpentUSD == nil {
			http.Error(w, "nothing to set", http.StatusBadRequest)
			return
		}
		if err := s.claudeCosts.set(id, input.AmountUSD, input.ExpiresAt, input.SpentUSD); err != nil {
			http.Error(w, "credit state write failed", http.StatusInternalServerError)
			return
		}
		if s.SchedulerRef != nil {
			// An operator update ends any spent-balance hold.
			s.SchedulerRef.MarkAccountUnavailableUntil(schedulerAccountProvider(accounts.ProviderClaude), id, time.Now())
		}
		writeJSON(w, claudeCreditStatusFromView(s.claudeCosts.views(time.Now())[id]))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
