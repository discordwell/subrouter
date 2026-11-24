package main

import (
	"strings"
	"testing"

	"github.com/discordwell/subrouter/selectacct"
)

// PATCHES §55's kill switch: unset is on, only an explicit 0/false/no/off
// turns it off, and an unrecognized value fails loudly instead of silently
// picking a side (SUBROUTER_CODEX_OVERLOAD_FAILOVER's shape).
func TestEndgameMeasuredInputsFromEnvironment(t *testing.T) {
	const name = "SUBROUTER_ENDGAME_MEASURED_INPUTS"
	for _, c := range []struct {
		raw  []string
		want bool
	}{
		{[]string{"", "  ", "1", "true", "YES", "on"}, true},
		{[]string{"0", "false", "No", "OFF"}, false},
	} {
		for _, raw := range c.raw {
			t.Setenv(name, raw)
			if got, err := endgameMeasuredInputsEnabled(); err != nil || got != c.want {
				t.Fatalf("%s=%q: got %v err %v, want %v", name, raw, got, err, c.want)
			}
		}
	}
	for _, raw := range []string{"2", "maybe", "disabled"} {
		t.Setenv(name, raw)
		if _, err := endgameMeasuredInputsEnabled(); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s=%q accepted (err %v)", name, raw, err)
		}
	}
}

// serve sets the switch for the process before any scheduler exists, and
// refuses to start on a value it cannot read. The fixture is the admin-gate
// tests' (serveMustReturn): a directory as the cloud config makes serve fail
// a step after the switch is set, without binding a socket.
func TestServeSetsTheEndgameMeasuredInputs(t *testing.T) {
	prev := selectacct.EndgameMeasuredInputs()
	t.Cleanup(func() { selectacct.SetEndgameMeasuredInputs(prev) })
	clearControlSecretEnv(t)
	dir := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", dir)
	t.Setenv("SUBROUTER_CLOUD_CONFIG", dir)
	args := []string{"--addr", "127.0.0.1:0", "--fetch-usage=false", "--sr-switch-interval=0"}

	for _, env := range []string{"", "0"} {
		t.Setenv("SUBROUTER_ENDGAME_MEASURED_INPUTS", env)
		want := env == ""
		// The opposite of what serve should set, so a serve that never sets
		// the switch is red here.
		selectacct.SetEndgameMeasuredInputs(!want)
		if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), "cmux.com config") {
			t.Fatalf("env %q: serve returned %v, want the deliberate cloud-config failure", env, err)
		}
		if got := selectacct.EndgameMeasuredInputs(); got != want {
			t.Fatalf("env %q: serve left measured inputs %v, want %v", env, got, want)
		}
	}

	t.Setenv("SUBROUTER_ENDGAME_MEASURED_INPUTS", "disabled")
	if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), "SUBROUTER_ENDGAME_MEASURED_INPUTS") {
		t.Fatalf("serve with SUBROUTER_ENDGAME_MEASURED_INPUTS=disabled returned %v, want a refusal naming it", err)
	}
}
