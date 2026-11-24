package proxy

import "github.com/discordwell/subrouter/internal/accounts"

// PATCHES §93.3: what the views read of automatic resets. Each usage-status
// row of an account this daemon holds carries the daemon's mode, the owner's
// condition, a claim in flight and the last decision; the health route names
// the mode and the condition. Since §94.3 both also say whether §94.2's rule
// decides, and the last decision carries what the rule used.

// AutoResetStatus is one account's automatic resets as its usage-status row
// publishes them. A peer's row carries the peer's own; nothing here ranks on
// it (remotePoolAccountUsage has no such field).
type AutoResetStatus struct {
	// Mode is SUBROUTER_AUTO_RESET as this daemon runs it: off, shadow or
	// real.
	Mode string `json:"mode"`
	// NearEmpty is the owner's under-1-percent condition
	// (SUBROUTER_AUTO_RESET_NEAR_EMPTY).
	NearEmpty bool `json:"near_empty"`
	// Worth is whether §94.2's rule decides (SUBROUTER_AUTO_RESET_WORTH).
	Worth bool `json:"worth"`
	// Claiming is an automatic claim in flight on the account (real mode).
	Claiming bool `json:"claiming,omitempty"`
	// Last is the last decision, absent before the first.
	Last *AutoResetDecision `json:"last,omitempty"`
}

// autoResetHealth is the mode, the condition and §94.2's rule, for the
// health route.
func autoResetHealth() map[string]any {
	mode, nearEmpty := AutoReset()
	return map[string]any{"mode": mode.String(), "near_empty": nearEmpty, "worth": AutoResetWorth()}
}

// withAutoResets fills AutoReset on each row of an account this daemon holds
// and could decide for: a Claude or Codex OAuth account, with no credential
// broker. The input may be the shared cached snapshot, so it is copied.
func (s Server) withAutoResets(statuses []AccountUsageStatus) []AccountUsageStatus {
	if s.AccountRef == nil || s.CredentialBroker != nil {
		return statuses
	}
	own := map[string]bool{}
	for _, account := range oauthAccounts(s.accountList()) {
		if provider := accountProviderOrCodex(account); provider == accounts.ProviderClaude || provider == accounts.ProviderCodex {
			own[autoResetKey(account)] = true
		}
	}
	mode, nearEmpty := AutoReset()
	worth := AutoResetWorth()
	memory := s.AccountRef.sharedAutoResets()
	memory.mu.Lock()
	defer memory.mu.Unlock()
	out := append([]AccountUsageStatus(nil), statuses...)
	for i := range out {
		key := autoResetKey(accounts.Account{ID: out[i].ID, Provider: accountProviderFor(out[i].Provider)})
		if out[i].AuthMode != accounts.AuthModeOAuth || !own[key] {
			continue
		}
		status := &AutoResetStatus{Mode: mode.String(), NearEmpty: nearEmpty, Worth: worth, Claiming: memory.running[key] != nil}
		if last, ok := memory.last[key]; ok {
			status.Last = &last
		}
		out[i].AutoReset = status
	}
	return out
}
