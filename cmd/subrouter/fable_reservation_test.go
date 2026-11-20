package main

import (
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/selectacct"
)

// PATCHES §57's switch reads like §55's and §56's except that its default is
// off: unset and 0/false/no/off are off, only an explicit 1/true/yes/on turns
// the Fable reservation on, and an unrecognized value fails loudly instead of
// silently picking a side.
func TestFableReservationFromEnvironment(t *testing.T) {
	const name = "SUBROUTER_FABLE_RESERVATION"
	for _, c := range []struct {
		raw  []string
		want bool
	}{
		{[]string{"", "  ", "0", "false", "No", "OFF"}, false},
		{[]string{"1", "true", "YES", "on", " 1 "}, true},
	} {
		for _, raw := range c.raw {
			t.Setenv(name, raw)
			if got, err := fableReservationEnabled(); err != nil || got != c.want {
				t.Fatalf("%s=%q: got %v err %v, want %v", name, raw, got, err, c.want)
			}
		}
	}
	for _, raw := range []string{"2", "maybe", "enabled"} {
		t.Setenv(name, raw)
		if _, err := fableReservationEnabled(); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s=%q accepted (err %v)", name, raw, err)
		}
	}
}

// serve sets the switch for the process before any scheduler exists, and
// refuses to start on a value it cannot read. The fixture is §55's
// (serveMustReturn): a directory as the cloud config makes serve fail a step
// after the switch is set, without binding a socket.
func TestServeSetsTheFableReservation(t *testing.T) {
	prev := selectacct.FableReservation()
	t.Cleanup(func() { selectacct.SetFableReservation(prev) })
	clearControlSecretEnv(t)
	dir := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", dir)
	t.Setenv("SUBROUTER_CLOUD_CONFIG", dir)
	args := []string{"--addr", "127.0.0.1:0", "--fetch-usage=false", "--sr-switch-interval=0"}

	for _, env := range []string{"", "1"} {
		t.Setenv("SUBROUTER_FABLE_RESERVATION", env)
		want := env == "1"
		// The opposite of what serve should set, so a serve that never sets
		// the switch is red here.
		selectacct.SetFableReservation(!want)
		if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), "cmux.com config") {
			t.Fatalf("env %q: serve returned %v, want the deliberate cloud-config failure", env, err)
		}
		if got := selectacct.FableReservation(); got != want {
			t.Fatalf("env %q: serve left the Fable reservation %v, want %v", env, got, want)
		}
	}

	t.Setenv("SUBROUTER_FABLE_RESERVATION", "enabled")
	if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), "SUBROUTER_FABLE_RESERVATION") {
		t.Fatalf("serve with SUBROUTER_FABLE_RESERVATION=enabled returned %v, want a refusal naming it", err)
	}
}
