package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// A pinned account whose credential is dead (PATCHES section 28, from upstream
// #429 / 19bb33b).
//
// The caller's own X-Subrouter-Account-ID pin disarms every failover stage --
// peer, Fable/Bedrock, Opus substitution, Azure, usage and overload failover --
// and refreshSelectedAccount hands the pinned account's refresh error straight
// back instead of failing over. So a pin on an account whose refresh token is
// dead used to end in the handler's no-account 503. Claude Code reads a 503 as
// retryable and retried it ten times, each retry re-running the same doomed
// refresh against the token endpoint, and the user got a raw refresh error at
// the end with nothing saying the pin was the problem. The pin cannot be served
// until someone re-adds the account (or Anthropic lifts a hold), so the honest
// answer is final: 403 with x-should-retry: false, which the Anthropic and
// OpenAI SDKs both honour, in the provider's own error shape so the client
// prints the message.
//
// Three things differ from upstream, each load-bearing here:
//
//  1. pinnedAccountCannotRecover asks refreshFailureIsTransient FIRST, the order
//     refreshSelectedAccount keeps (PATCHES section 20). The pinned early return
//     there comes before its transient test, so a refresh that reached no
//     verdict -- the token endpoint's 429/5xx, a network failure, the client's
//     own cancel, a credential store that did not answer -- arrives here raw.
//     That must stay the retryable 503 it was: the next attempt may well
//     succeed. Upstream tests only the terminal list, so a 5xx whose body
//     echoes a terminal marker would be answered as final.
//  2. The gate is the caller's own selector (forcedAccountID), never the header's
//     mere presence nor forcedAccountSelection: a session lease installs
//     X-Subrouter-Account-ID itself and recovers from a dead account by
//     re-leasing, and a placement the router makes for a peer (a targeted
//     forward) must stay a 503 the deciding side reads as a decline and
//     recomputes around. Neither may be told "never retry". forcedAccountSelection
//     may grow to cover such placements; forcedAccountID is only ever the
//     caller's X-Subrouter-Account-ID.
//  3. The hint names commands this CLI has. There is no `sr codex proxy` here:
//     a Codex pin comes from SUBROUTER_CODEX_ACCOUNT_ID.

// accountOnHold reports Anthropic's account_on_hold refusal: the account is
// restricted by Anthropic, and no re-login fixes it. It is deliberately NOT on
// isTerminalCredentialError's list, so the credential failure cache and the
// long credential hold are unchanged by this port. An unpinned request fails
// over on it (refreshFailureCannotServe, PATCHES §35) with the short mark.
func accountOnHold(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "account_on_hold")
}

// claudeSetupTokenExpired reports the store's refusal of an expired one-year
// setup token (claude.longLivedCredentialError): the one Claude credential
// failure whose repair is a new setup token rather than a browser sign-in.
func claudeSetupTokenExpired(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "setup token expired")
}

// pinnedAccountCannotRecover reports whether a pinned account's failure is a
// verdict no retry can change: a dead credential or an Anthropic hold. A
// failure that reached no verdict is never one, whatever its text says.
func pinnedAccountCannotRecover(ctx context.Context, err error) bool {
	if err == nil || refreshFailureIsTransient(ctx, err) {
		return false
	}
	return accountOnHold(err) || isTerminalCredentialError(err)
}

// pinnedAccountRepairHost says where a pin's repair must run when the caller is
// on another machine, and is "" when the pin arrived over loopback. A pin only
// ever names an account of the daemon answering (never a peer's; point 2
// above), and `sr claude proxy --account` with a server selected launches
// straight at that server, so the 403 is read on the laptop while the account
// is a profile on the box. There `sr claude addr <name>` would sign in a NEW
// local profile -- an OAuth login never uploads -- and leave the box's copy,
// and the pin, dead. The socket peer is the evidence (clientRemoteIP's
// reasoning: nothing sits in front of the daemon), and the host name is the
// daemon's own, the one name for that machine it can vouch for.
func pinnedAccountRepairHost(r *http.Request) string {
	if r == nil || isLoopbackRemote(r.RemoteAddr) {
		return ""
	}
	if host, err := os.Hostname(); err == nil && strings.TrimSpace(host) != "" {
		return "the server that holds it (host " + strings.TrimSpace(host) + ")"
	}
	return "the server that holds it"
}

// pinnedAccountHints returns how to revive the account and how to pin another
// one, in this fork's CLI. Providers without a single re-add or pin command get
// the plain instruction rather than a command that does not exist.
//
// A Claude pin names the account, so the command carries that name, and
// repairHost (pinnedAccountRepairHost) the machine it must run on when that is
// not the caller's. Which command depends on the login (PATCHES §36): a dead
// OAuth refresh token is signed in again in place with `sr claude addr
// <name>`, and only an expired one-year setup token is re-minted with `sr
// claude add <name>`. This said `sr add claude` for both -- the setup-token
// verb, which turns an OAuth account into a setup token and refuses without a
// name.
func pinnedAccountHints(provider accounts.Provider, accountID string, cause error, repairHost string) (readd, repin string) {
	switch provider {
	case accounts.ProviderClaude:
		repin = "Pick another account (sr claude proxy --account) or launch pooled (sr claude proxy)."
		where := ""
		if repairHost != "" {
			where = " on " + repairHost
		}
		if claudeSetupTokenExpired(cause) {
			return fmt.Sprintf("re-add it%s with 'sr claude add %s'", where, accountID), repin
		}
		return fmt.Sprintf("re-auth it%s with 'sr claude addr %s'", where, accountID), repin
	case accounts.ProviderCodex:
		return "re-add it with 'sr add codex'", "Set SUBROUTER_CODEX_ACCOUNT_ID to another account, or unset it to launch pooled."
	default:
		return "re-add it", "Pin another account or launch pooled."
	}
}

// writePinnedAccountUnusable answers a pinned request whose account cannot
// serve with a non-retryable 403 in the provider's error shape.
func writePinnedAccountUnusable(w http.ResponseWriter, r *http.Request, provider accounts.Provider, accountID string, cause error) {
	readd, repin := pinnedAccountHints(provider, accountID, cause, pinnedAccountRepairHost(r))
	reason := "its credential needs re-login (refresh token invalid or expired); " + readd
	if provider == accounts.ProviderClaude && claudeSetupTokenExpired(cause) {
		reason = "its one-year setup token has expired, and a setup token does not renew; " + readd
	}
	if accountOnHold(cause) {
		reason = "it is restricted by Anthropic (account_on_hold); re-login does not lift that"
	}
	message := fmt.Sprintf("subrouter: pinned account %q is unusable: %s. %s", accountID, reason, repin)
	var body []byte
	if provider == accounts.ProviderClaude {
		body, _ = json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]string{"type": "permission_error", "message": message},
		})
	} else {
		body, _ = json.Marshal(map[string]any{
			"error": map[string]string{"type": "invalid_request_error", "code": "account_unusable", "message": message},
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Should-Retry", "false")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write(body)
}
