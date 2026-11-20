package main

import (
	"testing"
)

func TestExplicitServerTargetPrecedence(t *testing.T) {
	for _, tc := range []struct {
		server, codex, want string
	}{
		{"", "", ""},
		{"", "legacy", "legacy"},
		{"neutral", "", "neutral"},
		{"neutral", "legacy", "neutral"},
		{"  ", " legacy ", "legacy"},
		{"local", "legacy", "local"},
	} {
		t.Setenv("SUBROUTER_SERVER", tc.server)
		t.Setenv("SUBROUTER_CODEX_SERVER", tc.codex)
		if got := explicitServerTarget(); got != tc.want {
			t.Fatalf("SUBROUTER_SERVER=%q SUBROUTER_CODEX_SERVER=%q: target = %q, want %q", tc.server, tc.codex, got, tc.want)
		}
	}
}

// Upstream's TestSRCommandHonorsProviderNeutralServerEnvOverTeamVault is not
// here: it pins run() routing on SUBROUTER_SERVER, which PATCHES §29.5 did
// not take (run() keeps SUBROUTER_CODEX_SERVER, the §22 spelling).
