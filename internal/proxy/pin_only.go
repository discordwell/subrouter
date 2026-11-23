package proxy

import (
	"strings"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// Pin-only accounts (PATCHES §98, SUBROUTER_PIN_ONLY_ACCOUNTS).
//
// 2026-11-23 owner: an account listed here takes pinned sessions only (the meter test).
//
// The daemon that holds a listed account serves it only for a request that
// names it with the caller's own selector (X-Subrouter-Account-ID, what
// claude-route's pinned launch sends), on the forced path of
// accountForSessionProviderWithOptions, which is unchanged: a spent or
// refused pin gets the provider's answer or §89's, and is never moved. The
// pool's own work never lands there:
//   - selection's scheduler carries the list (Server.scheduler), so the
//     account is not routable for a new session, its verdict is pin_only for
//     every peer, and it takes no refugee and no reset loading;
//   - every path that places or moves work refuses it: a new placement and a
//     session leaving its account (selectionRefusesAccount), a session already
//     on it without a pin, a preference, an idle re-rank, a failover
//     (withoutPinOnly), an admission spill, the recovery probe and the cooked
//     Codex pool's redeem;
//   - §93 decides nothing for it at either moment, §95 never chooses it, and
//     the pool figures §94 to §96 read leave it out (its own burn stays on its
//     row, and §92's history keeps recording it);
//   - the daemon sends it nothing of its own: no paid Claude Messages probe and
//     no §90 start of a Codex week.
// A manual `sr claude reset` or `sr codex reset` is unchanged.

// SetPinOnlyAccounts records the IDs of this daemon's accounts that take
// pinned sessions only. serve calls it before any sweep starts; empty IDs are
// dropped, and none leaves every account as it was.
func (r *AccountRef) SetPinOnlyAccounts(ids []string) {
	if r == nil {
		return
	}
	set := map[string]bool{}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			set[id] = true
		}
	}
	if len(set) == 0 {
		set = nil
	}
	r.pinOnly.Store(&set)
}

// pinOnlyAccounts is the set SetPinOnlyAccounts recorded, nil for none. It
// must not be mutated.
func (r *AccountRef) pinOnlyAccounts() map[string]bool {
	if r == nil {
		return nil
	}
	if set := r.pinOnly.Load(); set != nil {
		return *set
	}
	return nil
}

// pinOnlyAccount reports whether the account with this ID takes pinned
// sessions only.
func (r *AccountRef) pinOnlyAccount(id string) bool {
	return r.pinOnlyAccounts()[id]
}

func (s Server) pinOnlyAccount(id string) bool {
	return s.AccountRef.pinOnlyAccount(id)
}

// withoutPinOnly drops the pin-only accounts from a failover's candidates.
func withoutPinOnly(scheduler selectacct.Scheduler, candidates []accounts.Account) []accounts.Account {
	var out []accounts.Account
	for _, account := range candidates {
		if scheduler.PinOnly(schedulerAccountProvider(account.Provider), account.ID) {
			continue
		}
		out = append(out, account)
	}
	return out
}
