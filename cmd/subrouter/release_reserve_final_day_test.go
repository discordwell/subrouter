package main

import (
	"strings"
	"testing"

	"github.com/discordwell/subrouter/selectacct"
)

// PATCHES §60's kill switch reads exactly like §55's and §56's: unset is on,
// only an explicit 0/false/no/off turns it off, and an unrecognized value fails
// loudly instead of silently picking a side.
func TestReleaseReserveFinalDayFromEnvironment(t *testing.T) {
	const name = "SUBROUTER_RELEASE_RESERVE_FINAL_DAY"
	for _, c := range []struct {
		raw  []string
		want bool
	}{
		{[]string{"", "  ", "1", "true", "YES", "on"}, true},
		{[]string{"0", "false", "No", "OFF"}, false},
	} {
		for _, raw := range c.raw {
			t.Setenv(name, raw)
			if got, err := releaseReserveFinalDayEnabled(); err != nil || got != c.want {
				t.Fatalf("%s=%q: got %v err %v, want %v", name, raw, got, err, c.want)
			}
		}
	}
	for _, raw := range []string{"2", "maybe", "disabled"} {
		t.Setenv(name, raw)
		if _, err := releaseReserveFinalDayEnabled(); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s=%q accepted (err %v)", name, raw, err)
		}
	}
}

// serve sets the switch for the process before any scheduler exists, and
// refuses to start on a value it cannot read (fixture: serveMustReturn, as in
// TestServeSetsTheResetHoldPriority).
func TestServeSetsTheFinalDayReserveRelease(t *testing.T) {
	prev := selectacct.ReleaseReserveFinalDay()
	t.Cleanup(func() { selectacct.SetReleaseReserveFinalDay(prev) })
	clearControlSecretEnv(t)
	dir := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", dir)
	t.Setenv("SUBROUTER_CLOUD_CONFIG", dir)
	args := []string{"--addr", "127.0.0.1:0", "--fetch-usage=false", "--sr-switch-interval=0"}

	for _, env := range []string{"", "0"} {
		t.Setenv("SUBROUTER_RELEASE_RESERVE_FINAL_DAY", env)
		want := env == ""
		// The opposite of what serve should set, so a serve that never sets
		// the switch is red here.
		selectacct.SetReleaseReserveFinalDay(!want)
		if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), "cmux.com config") {
			t.Fatalf("env %q: serve returned %v, want the deliberate cloud-config failure", env, err)
		}
		if got := selectacct.ReleaseReserveFinalDay(); got != want {
			t.Fatalf("env %q: serve left the final-day release %v, want %v", env, got, want)
		}
	}

	t.Setenv("SUBROUTER_RELEASE_RESERVE_FINAL_DAY", "disabled")
	if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), "SUBROUTER_RELEASE_RESERVE_FINAL_DAY") {
		t.Fatalf("serve with SUBROUTER_RELEASE_RESERVE_FINAL_DAY=disabled returned %v, want a refusal naming it", err)
	}
}
