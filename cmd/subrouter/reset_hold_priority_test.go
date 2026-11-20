package main

import (
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/selectacct"
)

// PATCHES §56's kill switch reads exactly like §55's: unset is on, only an
// explicit 0/false/no/off turns it off, and an unrecognized value fails
// loudly instead of silently picking a side.
func TestResetHoldPriorityFromEnvironment(t *testing.T) {
	const name = "SUBROUTER_RESET_HOLD_PRIORITY"
	for _, c := range []struct {
		raw  []string
		want bool
	}{
		{[]string{"", "  ", "1", "true", "YES", "on"}, true},
		{[]string{"0", "false", "No", "OFF"}, false},
	} {
		for _, raw := range c.raw {
			t.Setenv(name, raw)
			if got, err := resetHoldPriorityEnabled(); err != nil || got != c.want {
				t.Fatalf("%s=%q: got %v err %v, want %v", name, raw, got, err, c.want)
			}
		}
	}
	for _, raw := range []string{"2", "maybe", "disabled"} {
		t.Setenv(name, raw)
		if _, err := resetHoldPriorityEnabled(); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s=%q accepted (err %v)", name, raw, err)
		}
	}
}

// serve sets the switch for the process before any scheduler exists, and
// refuses to start on a value it cannot read. The fixture is §55's
// (serveMustReturn): a directory as the cloud config makes serve fail a step
// after the switch is set, without binding a socket.
func TestServeSetsTheResetHoldPriority(t *testing.T) {
	prev := selectacct.ResetHoldPriority()
	t.Cleanup(func() { selectacct.SetResetHoldPriority(prev) })
	clearControlSecretEnv(t)
	dir := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", dir)
	t.Setenv("SUBROUTER_CLOUD_CONFIG", dir)
	args := []string{"--addr", "127.0.0.1:0", "--fetch-usage=false", "--sr-switch-interval=0"}

	for _, env := range []string{"", "0"} {
		t.Setenv("SUBROUTER_RESET_HOLD_PRIORITY", env)
		want := env == ""
		// The opposite of what serve should set, so a serve that never sets
		// the switch is red here.
		selectacct.SetResetHoldPriority(!want)
		if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), "cmux.com config") {
			t.Fatalf("env %q: serve returned %v, want the deliberate cloud-config failure", env, err)
		}
		if got := selectacct.ResetHoldPriority(); got != want {
			t.Fatalf("env %q: serve left the reset hold priority %v, want %v", env, got, want)
		}
	}

	t.Setenv("SUBROUTER_RESET_HOLD_PRIORITY", "disabled")
	if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), "SUBROUTER_RESET_HOLD_PRIORITY") {
		t.Fatalf("serve with SUBROUTER_RESET_HOLD_PRIORITY=disabled returned %v, want a refusal naming it", err)
	}
}
