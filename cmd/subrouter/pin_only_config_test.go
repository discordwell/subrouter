package main

import (
	"context"
	"reflect"
	"testing"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
)

// PATCHES §98: SUBROUTER_PIN_ONLY_ACCOUNTS is a comma-separated list of
// account IDs; unset, empty or blank lists none.
func TestPinOnlyAccountsFromTheEnvironment(t *testing.T) {
	for _, raw := range []string{"", "   ", ",,", " , "} {
		if got := pinOnlyAccounts(raw); got != nil {
			t.Fatalf("pinOnlyAccounts(%q) = %q, want none", raw, got)
		}
	}
	if got := pinOnlyAccounts(" test-account, other ,test-account,,"); !reflect.DeepEqual(got, []string{"test-account", "other"}) {
		t.Fatalf("pinOnlyAccounts = %q, want test-account and other once each", got)
	}
}

// PATCHES §98: the daemon's sr auto-switch scores a pin-only account like any
// other but never picks it, for either provider, even with the most headroom.
// As the active Claude profile, a bare `sr claude` launch would spend it with
// no pin.
func TestAutoSwitchNeverRotatesOntoAPinOnlyAccount(t *testing.T) {
	rotate := func(listed bool) (claude, codex []string) {
		t.Helper()
		var scored []string
		_, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
			Accounts: []accounts.Account{
				{ID: "test-account", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
				{ID: "spare", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
				{ID: "codex-test-account", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
				{ID: "codex-spare", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
			},
			FetchScores: func(_ context.Context, candidates []accounts.Account) ([]selectacct.Score, int) {
				for _, candidate := range candidates {
					scored = append(scored, candidate.ID)
				}
				return []selectacct.Score{
					{AccountID: "test-account", Provider: accounts.ProviderClaude, Headroom: 0.95, ShortHeadroom: 0.95, Fresh: true},
					{AccountID: "spare", Provider: accounts.ProviderClaude, Headroom: 0.4, ShortHeadroom: 0.4, Fresh: true},
					{AccountID: "codex-test-account", Provider: accounts.ProviderCodex, Headroom: 0.95, ShortHeadroom: 0.95, Fresh: true},
					{AccountID: "codex-spare", Provider: accounts.ProviderCodex, Headroom: 0.4, ShortHeadroom: 0.4, Fresh: true},
				}, 4
			},
			SwitchActiveClaude: func(_ context.Context, accountID string) error {
				claude = append(claude, accountID)
				return nil
			},
			SwitchActive: func(_ context.Context, accountID string) error {
				codex = append(codex, accountID)
				return nil
			},
			PinOnly: func(accountID string) bool {
				return listed && (accountID == "test-account" || accountID == "codex-test-account")
			},
		})
		if err != nil {
			t.Fatalf("listed=%v: %v", listed, err)
		}
		if len(scored) != 4 {
			t.Fatalf("listed=%v: scored %v, want every account", listed, scored)
		}
		return claude, codex
	}
	if claude, codex := rotate(false); !reflect.DeepEqual(claude, []string{"test-account"}) || !reflect.DeepEqual(codex, []string{"codex-test-account"}) {
		t.Fatalf("calibration: rotation claude %v codex %v, want the accounts with the most headroom", claude, codex)
	}
	if claude, codex := rotate(true); !reflect.DeepEqual(claude, []string{"spare"}) || !reflect.DeepEqual(codex, []string{"codex-spare"}) {
		t.Fatalf("listed: rotation claude %v codex %v, want [spare] and [codex-spare]", claude, codex)
	}
}
