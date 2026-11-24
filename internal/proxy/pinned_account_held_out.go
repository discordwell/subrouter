package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
)

// A pin on an account inside its owner's holdout (PATCHES section 89).
//
// Selection refuses it (PATCHES section 20): no session starts or continues
// inside a holdout, however the account was asked for, and a pin is never
// moved to another account. That refusal used to leave as the handler's
// no-account 503, which a client retries. Claude Code gave up after ten
// tries, well inside a minute, and that was the whole of "a pinned launch on
// a spent account fails fast". A client told to wait hours for a daemon that
// is gone (CLAUDE_CODE_RETRY_WATCHDOG) asks again for as long: measured on
// 2.1.330, 13 identical refusals in 4 minutes and no end.
//
// The holdout ends when the account's week rolls, or in its last day (PATCHES
// section 60), never in the seconds between two tries. So the answer is
// final, as for a pin whose credential is dead (PATCHES section 28): one
// response with X-Should-Retry: false, in the provider's error shape, that
// names the pin and says what to do. For Claude it is a 429 rate_limit_error:
// measured on 2.1.330 with the long wait on, the client stops at once and
// prints the message, where the same 429 without the header is retried
// without end. Every other provider gets section 28's 403.
//
// The gate is the one section 28 uses, and for its reason: the caller's own
// selector (forcedAccountID), never a session lease or a placement another
// daemon made, which recover by being asked again.

// pinnedAccountHeldOutError is selection's refusal of a requested account
// that is inside its owner's holdout.
type pinnedAccountHeldOutError struct {
	accountID string
}

func (e pinnedAccountHeldOutError) Error() string {
	return fmt.Sprintf("requested account %q is inside its owner's holdout", e.accountID)
}

// writePinnedAccountHeldOut answers a pinned request whose account is inside
// its owner's holdout, finally. weekRolls is the zero time when it is not
// known.
func writePinnedAccountHeldOut(w http.ResponseWriter, provider accounts.Provider, accountID string, weekRolls time.Time) {
	_, repin := pinnedAccountHints(provider, accountID, nil, "")
	when := ""
	if !weekRolls.IsZero() {
		when = " Its week rolls at " + weekRolls.UTC().Format("2006-01-02 15:04 UTC") + "."
	}
	message := fmt.Sprintf("subrouter: pinned account %q is inside its owner's holdout: the rest of its week is kept back, and a pinned session is never moved to another account.%s %s",
		accountID, when, repin)
	status := http.StatusForbidden
	var body []byte
	if provider == accounts.ProviderClaude {
		status = http.StatusTooManyRequests
		body, _ = json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]string{"type": "rate_limit_error", "message": message},
		})
	} else {
		body, _ = json.Marshal(map[string]any{
			"error": map[string]string{"type": "invalid_request_error", "code": "account_held_out", "message": message},
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Should-Retry", "false")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
