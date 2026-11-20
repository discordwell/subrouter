package main

import (
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/selectacct"
)

// PATCHES §65's kill switch reads exactly like §56's: unset is on, only an
// explicit 0/false/no/off turns it off, and an unrecognized value fails
// loudly instead of silently picking a side.
func TestFableSpentPreferenceFromEnvironment(t *testing.T) {
	const name = "SUBROUTER_FABLE_SPENT_PREFERENCE"
	for _, c := range []struct {
		raw  []string
		want bool
	}{
		{[]string{"", "  ", "1", "true", "YES", "on"}, true},
		{[]string{"0", "false", "No", "OFF"}, false},
	} {
		for _, raw := range c.raw {
			t.Setenv(name, raw)
			if got, err := fableSpentPreferenceEnabled(); err != nil || got != c.want {
				t.Fatalf("%s=%q: got %v err %v, want %v", name, raw, got, err, c.want)
			}
		}
	}
	for _, raw := range []string{"2", "maybe", "disabled"} {
		t.Setenv(name, raw)
		if _, err := fableSpentPreferenceEnabled(); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s=%q accepted (err %v)", name, raw, err)
		}
	}
}

// serve sets the switch for the process before any scheduler exists, says so
// once at startup when it is off and not at all when it is on, and refuses to
// start on a value it cannot read. The fixture is §55's (serveMustReturn): a
// directory as the cloud config makes serve fail a step after the switch is
// set, without binding a socket.
func TestServeSetsTheFableSpentPreference(t *testing.T) {
	prev := selectacct.FableSpentPreference()
	t.Cleanup(func() { selectacct.SetFableSpentPreference(prev) })
	clearControlSecretEnv(t)
	dir := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", dir)
	t.Setenv("SUBROUTER_CLOUD_CONFIG", dir)
	args := []string{"--addr", "127.0.0.1:0", "--fetch-usage=false", "--sr-switch-interval=0"}
	const logLine = "fable spent preference disabled by SUBROUTER_FABLE_SPENT_PREFERENCE"

	for _, env := range []string{"", "0"} {
		t.Setenv("SUBROUTER_FABLE_SPENT_PREFERENCE", env)
		want := env == ""
		// The opposite of what serve should set, so a serve that never sets
		// the switch is red here.
		selectacct.SetFableSpentPreference(!want)
		log := captureDefaultLog(t)
		if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), "cmux.com config") {
			t.Fatalf("env %q: serve returned %v, want the deliberate cloud-config failure", env, err)
		}
		if got := selectacct.FableSpentPreference(); got != want {
			t.Fatalf("env %q: serve left the Fable-spent preference %v, want %v", env, got, want)
		}
		wantLines := 0
		if !want {
			wantLines = 1
		}
		if got := strings.Count(log.String(), logLine); got != wantLines ||
			(!want && !strings.Contains(log.String(), "unset SUBROUTER_FABLE_SPENT_PREFERENCE and restart")) {
			t.Fatalf("env %q: startup logged %q %d times, want %d with the re-enable hint:\n%s", env, logLine, got, wantLines, log)
		}
	}

	t.Setenv("SUBROUTER_FABLE_SPENT_PREFERENCE", "disabled")
	if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), "SUBROUTER_FABLE_SPENT_PREFERENCE") {
		t.Fatalf("serve with SUBROUTER_FABLE_SPENT_PREFERENCE=disabled returned %v, want a refusal naming it", err)
	}
}
