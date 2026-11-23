package main

import (
	"reflect"
	"testing"
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
