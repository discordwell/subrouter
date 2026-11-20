package proxy

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// controlPlanePath reports whether a path asks Anthropic a question about the
// CALLER rather than for inference: who am I, what may I use, what is my org.
// Everything under /api/ is account-scoped identity and entitlement -- the CLI's
// startup bootstrap (/api/claude_cli/bootstrap), /api/oauth/profile, skills,
// notification preferences. Codex's own backend lives under /backend-api/, so
// this prefix is Claude's control plane and nothing else.
//
// These must not be scheduled like inference. A pooled daemon answers each call
// from whichever account the scheduler picks, so consecutive calls describe
// DIFFERENT organizations, and the client caches whichever one replied as its
// own identity. Observed live: a Claude Code bootstrap answered by another
// account cached that account's organization settings, which hid a model from
// the picker for a user whose own accounts allowed it.
func controlPlanePath(path string) bool {
	return path == "/api" || strings.HasPrefix(path, "/api/")
}

// claudeControlPlaneRequest reports a Claude control-plane call: the calls the
// hardened identity below answers, and whose answers are not inference
// evidence.
func claudeControlPlaneRequest(provider accounts.Provider, r *http.Request) bool {
	return r != nil && provider == accounts.ProviderClaude && controlPlanePath(r.URL.Path)
}

// identityAccount picks the single account that answers control-plane questions
// for a provider other than Claude (claudeControlPlaneIdentity is Claude's).
//
// Stability is the property that matters here, more than which account wins: a
// client caches the first answer and keeps it, so any fixed account is correct
// and any rotation is not. IdentityAccountID names one explicitly; without it
// the lowest account ID wins, which is arbitrary but survives restarts,
// scheduler state, exhaustion marks and account-list order.
//
// OAuth only: an API-key account has no subscription identity to report, so
// letting one answer would describe an organization the user does not have.
func (s Server) identityAccount(available []accounts.Account) (accounts.Account, bool) {
	candidates := oauthAccounts(available)
	if len(candidates) == 0 {
		return accounts.Account{}, false
	}
	if want := strings.TrimSpace(s.IdentityAccountID); want != "" {
		if account, ok := findAccount(candidates, want); ok {
			return account, true
		}
		// A named account that is gone must NOT silently fall through to the
		// rotating scheduler -- that is the bug this function exists to prevent.
		// The deterministic pick below is still stable, so the identity stays
		// consistent; it is just no longer the one that was asked for.
	}
	sorted := make([]accounts.Account, len(candidates))
	copy(sorted, candidates)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	return sorted[0], true
}

// The Claude control plane's identity is explicit, never dead, and loud.
//
// Claude Code in socket mode (SUBROUTER_CLAUDE_SOCKET) sends one control-plane
// call into the daemon, GET /api/claude_code/policy_limits, with no credential
// of its own, and every process needs a live 200 for it: without one it
// withholds WebFetch, DesignSync and channels and denies every policy key whose
// cache-miss rule is "deny". The account answering decides whose organization's
// policy the session gets. On 2026-11-19 a Mac's daemon had no pin, so the
// lowest account ID answered -- a setup token Anthropic had revoked -- and every
// socket session lost those tools, with nothing in health or the log saying
// why.
//
//   - With a socket configured (IdentityPinRequired), the pin answers or nothing
//     does: no pin, a pin naming no Claude OAuth account here, and a pin whose
//     current credential is recorded dead are each a 503 on /api/*, never
//     another account. Without a socket the deterministic fallback stays, minus
//     dead credentials, and health and the startup warning say the pin lost.
//   - A credential recorded dead is never picked: the revoked overlay
//     (claude_credential_health.go), set by Anthropic's 401 "has been revoked" or
//     403 "OAuth authentication is currently not allowed for this organization",
//     keyed to that credential, so a re-login clears it.
//   - A refresh verdict against the identity never fails over to another account
//     (refreshSelectedAccount): the call is a 503.
//   - A control-plane answer is not inference evidence: its 401, 403 or 429 sets
//     no scheduler mark (captureResponseBodyForRequest).
//   - Health reports control_plane_identity, and serve warns at startup.
const (
	// identityPinned: SUBROUTER_IDENTITY_ACCOUNT names a Claude OAuth account
	// here whose current credential is not recorded dead. It answers.
	identityPinned = "pinned"
	// identityDefault: no pin and no socket. The lowest live account ID answers.
	identityDefault = "default"
	// identityUnpinned: a socket is configured and nothing is pinned.
	identityUnpinned = "unpinned"
	// identityMissing: the pin names no Claude OAuth account here.
	identityMissing = "missing"
	// identityDead: the pinned account's current credential is recorded dead.
	identityDead = "dead"
	// identityNone: no pin, no socket, and no live Claude OAuth account.
	identityNone = "none"
	// identityRefreshFailed is a refusal, not a health state: the identity's
	// token refresh reached a verdict that it cannot serve.
	identityRefreshFailed = "refresh_failed"
)

// controlPlaneIdentityReason is X-Subrouter-Reason, and the traffic error
// class, of a control-plane call no identity may answer.
const controlPlaneIdentityReason = "control_plane_identity"

// claudeIdentity is who answers the Claude control plane, and why.
type claudeIdentity struct {
	// Account answers. Zero when nothing may.
	Account accounts.Account
	// Pinned is the account the pin names, whether or not its credential is
	// live. Zero when the pin is unset or names no account here.
	Pinned accounts.Account
	// State is one of the identity* health words.
	State string
}

// claudeControlPlaneIdentity resolves the Claude control plane's identity over
// the Claude accounts available now, reading credentials as they are in
// memory (the request path re-reads a dead pin's store: see
// accountForSessionProviderWithOptions).
func (s Server) claudeControlPlaneIdentity(available []accounts.Account) claudeIdentity {
	candidates := oauthAccounts(available)
	dead := s.claudeCredentialDead()
	if pin := strings.TrimSpace(s.IdentityAccountID); pin != "" {
		identity := claudeIdentity{State: identityMissing}
		if account, ok := findAccount(candidates, pin); ok {
			if !dead(account) {
				return claudeIdentity{Account: account, Pinned: account, State: identityPinned}
			}
			identity = claudeIdentity{Pinned: account, State: identityDead}
		}
		if !s.IdentityPinRequired {
			identity.Account, _ = lowestLiveAccount(candidates, dead)
		}
		return identity
	}
	if s.IdentityPinRequired {
		return claudeIdentity{State: identityUnpinned}
	}
	if account, ok := lowestLiveAccount(candidates, dead); ok {
		return claudeIdentity{Account: account, State: identityDefault}
	}
	return claudeIdentity{State: identityNone}
}

// claudeCredentialDead reads the revoked overlay once and reports whether an
// account's current credential is recorded dead in it.
func (s Server) claudeCredentialDead() func(accounts.Account) bool {
	costs := s.claudeCostStore()
	if costs == nil {
		return func(accounts.Account) bool { return false }
	}
	file := costs.snapshot()
	return func(account accounts.Account) bool {
		_, dead := claudeRevokedSince(file, account.ID, account.CredentialIdentity())
		return dead
	}
}

// lowestLiveAccount is the deterministic pick: the lowest account ID whose
// credential is not recorded dead.
func lowestLiveAccount(candidates []accounts.Account, dead func(accounts.Account) bool) (accounts.Account, bool) {
	var best accounts.Account
	found := false
	for _, account := range candidates {
		if dead(account) {
			continue
		}
		if !found || account.ID < best.ID {
			best, found = account, true
		}
	}
	return best, found
}

// controlPlaneIdentityError refuses a Claude control-plane call that no
// account may answer. The handler answers it 503
// (writeControlPlaneIdentityUnavailable); no other account is tried.
type controlPlaneIdentityError struct {
	State string
	// Account is the identity account the refusal is about, "" when there is
	// none.
	Account string
	cause   error
}

func (e *controlPlaneIdentityError) Error() string {
	message := "claude control-plane identity unavailable: " + e.State
	if e.cause != nil {
		message += ": " + e.cause.Error()
	}
	return message
}

func (e *controlPlaneIdentityError) Unwrap() error { return e.cause }

// controlPlaneIdentityFix is the operator's next step for a state.
func controlPlaneIdentityFix(state string) string {
	switch state {
	case identityUnpinned:
		return "set SUBROUTER_IDENTITY_ACCOUNT to a working Claude account in your own organization"
	case identityMissing:
		return "SUBROUTER_IDENTITY_ACCOUNT names no Claude OAuth account here; fix the name or add the account"
	case identityDead, identityRefreshFailed:
		return "sign the identity account in again ('sr claude addr <name>' for a browser login, 'sr claude add <name>' for a setup token), or pin another"
	case identityNone:
		return "add a Claude OAuth account"
	}
	return ""
}

// writeControlPlaneIdentityUnavailable answers a refused control-plane call.
// No body names an account: the log line does.
func (s Server) writeControlPlaneIdentityUnavailable(w http.ResponseWriter, r *http.Request, agentType, sessionID string, refusal *controlPlaneIdentityError) {
	markTrafficErrorClass(r.Context(), controlPlaneIdentityReason)
	if s.Logger != nil {
		fields := []any{
			"agent", agentType,
			"session", sessionID,
			"path", r.URL.Path,
			"state", refusal.State,
			"pin", strings.TrimSpace(s.IdentityAccountID),
			"account", refusal.Account,
			"fix", controlPlaneIdentityFix(refusal.State),
		}
		if refusal.cause != nil {
			fields = append(fields, "error", refusal.cause.Error())
		}
		s.Logger.Warn("claude control-plane call refused: no identity account may answer it", fields...)
	}
	w.Header().Set(subrouterReasonHeader, controlPlaneIdentityReason)
	http.Error(w, "subrouter: no Claude control-plane identity may answer this call ("+refusal.State+
		"); see control_plane_identity in /_subrouter/health", http.StatusServiceUnavailable)
}

// ControlPlaneIdentity is health's control_plane_identity.
type ControlPlaneIdentity struct {
	// ID is the account that answers Claude control-plane calls, "" when none
	// may.
	ID    string `json:"id"`
	State string `json:"state"`
	// Pin is SUBROUTER_IDENTITY_ACCOUNT when it is set and does not answer.
	Pin string `json:"pin,omitempty"`
	// IdentityOnly reports SUBROUTER_IDENTITY_ONLY.
	IdentityOnly bool `json:"identity_only,omitempty"`
}

// controlPlaneIdentityConfigured: health reports the identity only where it was
// configured, so an unconfigured daemon's health body is unchanged.
func (s Server) controlPlaneIdentityConfigured() bool {
	return s.IdentityPinRequired || s.IdentityOnly || strings.TrimSpace(s.IdentityAccountID) != ""
}

// ControlPlaneIdentityStatus reports who answers the Claude control plane now.
func (s Server) ControlPlaneIdentityStatus(ctx context.Context) ControlPlaneIdentity {
	identity := s.claudeControlPlaneIdentity(filterAccountsForProvider(s.accountListContext(ctx), accounts.ProviderClaude))
	status := ControlPlaneIdentity{ID: identity.Account.ID, State: identity.State, IdentityOnly: s.IdentityOnly}
	if identity.State == identityMissing || identity.State == identityDead {
		status.Pin = strings.TrimSpace(s.IdentityAccountID)
	}
	return status
}

// WarnControlPlaneIdentity logs, once at startup, a Claude control-plane
// identity that is not what the configuration asks for.
func (s Server) WarnControlPlaneIdentity(ctx context.Context) {
	if s.Logger == nil {
		return
	}
	status := s.ControlPlaneIdentityStatus(ctx)
	switch status.State {
	case identityPinned:
		s.Logger.Info("claude control-plane identity", "account", status.ID, "identity_only", status.IdentityOnly)
	case identityDefault:
		if s.IdentityOnly {
			s.Logger.Warn("SUBROUTER_IDENTITY_ONLY is set without SUBROUTER_IDENTITY_ACCOUNT; no account is kept out of inference",
				"answering", status.ID, "fix", "set SUBROUTER_IDENTITY_ACCOUNT")
		}
	default:
		answering := status.ID
		if answering == "" {
			answering = "nobody: /api/* answers 503"
		}
		s.Logger.Warn("claude control-plane identity is not the configured one",
			"state", status.State, "pin", strings.TrimSpace(s.IdentityAccountID), "answering", answering,
			"socket", s.IdentityPinRequired, "fix", controlPlaneIdentityFix(status.State))
	}
}

// Identity-only (SUBROUTER_IDENTITY_ONLY=1, off by default): the pinned
// identity account answers the Claude control plane and nothing else. Off, it
// is also an ordinary pool account, which is right for an owner who keeps it
// spendable.
//
// One mechanism does most of it: withIdentityOnlyOverlay scores the account
// evicted in every routing view (Server.scheduler), the way a revoked
// credential is. Everything that ranks or refuses by that view therefore
// leaves it alone: a new placement, a sticky or side request, a peer's target,
// the verdict peers read, a refresh or usage failover, an admission spill. The
// paths that do not read that view name it out themselves: a forced pin
// (accountForSessionProviderWithOptions), the usage failover's recovery probe
// (oauthRecoveryProbeCandidate), the auto-switch's active Claude profile
// (srAutoSwitchConfig.ClaudeIdentityOnly), and the sweep's Messages probes,
// which spend the account's quota (claudeFableProbe, probeClaudeLiveness).

// identityOnlyAccountIn resolves the identity-only pin over list the way the
// control plane resolves it: findAccount over the Claude OAuth accounts.
func identityOnlyAccountIn(list []accounts.Account, pin string) string {
	if pin = strings.TrimSpace(pin); pin == "" {
		return ""
	}
	if account, ok := findAccount(oauthAccounts(filterAccountsForProvider(list, accounts.ProviderClaude)), pin); ok {
		return account.ID
	}
	return ""
}

// IdentityOnlyAccountID is the ID of the Claude account identity-only keeps
// out of everything but the control plane, "" when the mode is off or the pin
// names no Claude OAuth account here.
func (s Server) IdentityOnlyAccountID() string {
	if !s.IdentityOnly {
		return ""
	}
	return identityOnlyAccountIn(append(append([]accounts.Account(nil), s.Accounts...), s.AccountRef.All()...), s.IdentityAccountID)
}

// identityOnlyAccount reports whether account is the identity-only account.
func (s Server) identityOnlyAccount(account accounts.Account) bool {
	id := s.IdentityOnlyAccountID()
	return id != "" && account.ID == id && accountProviderFor(account.Provider) == accounts.ProviderClaude
}

// withoutIdentityOnly drops the identity-only account from a candidate list
// that is not ranked through the overlay.
func (s Server) withoutIdentityOnly(list []accounts.Account) []accounts.Account {
	id := s.IdentityOnlyAccountID()
	if id == "" {
		return list
	}
	out := make([]accounts.Account, 0, len(list))
	for _, account := range list {
		if account.ID == id && accountProviderFor(account.Provider) == accounts.ProviderClaude {
			continue
		}
		out = append(out, account)
	}
	return out
}

// withIdentityOnlyOverlay scores the identity-only account out of every
// routing view, the way the revoked overlay scores a dead credential out: no
// headroom and a reset a year away, so it is evicted, never a last resort, and
// published to peers as taking no session. Never written back to SchedulerRef.
func (s Server) withIdentityOnlyOverlay(base selectacct.Scheduler) selectacct.Scheduler {
	id := s.IdentityOnlyAccountID()
	if id == "" {
		return base
	}
	score := base.ScoreFor(accounts.ProviderClaude, id)
	score.AccountID, score.Provider = id, accounts.ProviderClaude
	score.Headroom, score.ShortHeadroom, score.ModelScores = 0, 0, nil
	score.ExhaustedResetAt, score.ExhaustedResetUnknown = time.Now().Add(365*24*time.Hour), false
	return base.WithScore(score)
}

// SetClaudeIdentityOnly tells the ref which Claude account identity-only
// keeps out of the paid Messages probes (claudeFableProbe,
// probeClaudeLiveness). serve calls it before any sweep starts.
func (r *AccountRef) SetClaudeIdentityOnly(pin string) {
	if r == nil {
		return
	}
	pin = strings.TrimSpace(pin)
	r.claudeIdentityOnly.Store(&pin)
}

// claudeIdentityOnlyAccount reports whether account is the identity-only
// account. It matches the pin by ID or label without reading the account
// list, so a probe never waits on the ref's lock.
func (r *AccountRef) claudeIdentityOnlyAccount(account accounts.Account) bool {
	if r == nil || accountProviderOrCodex(account) != accounts.ProviderClaude {
		return false
	}
	pin := r.claudeIdentityOnly.Load()
	return pin != nil && *pin != "" && accountMatches(account, *pin)
}
