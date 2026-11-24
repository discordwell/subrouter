package main

import (
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// 9. PATCHES §103: SUBROUTER_AUTO_RESET_WAIT_FOR_BETTER unset or 1 is on, 0
// goes back to §94's decision and §95's choice, and any other value is
// refused with a message naming it. serve sets it for the process and says
// at startup when it is off.
func TestServeSetsTheWaitForABetterWall(t *testing.T) {
	const name = "SUBROUTER_AUTO_RESET_WAIT_FOR_BETTER"
	for raw, want := range map[string]bool{"": true, " ": true, "1": true, "0": false} {
		t.Setenv(name, raw)
		if got, err := autoResetWaitForBetterEnabled(); err != nil || got != want {
			t.Fatalf("%s=%q: got %v err %v, want %v", name, raw, got, err, want)
		}
	}
	for _, raw := range []string{"2", "true", "on", "off", "no"} {
		t.Setenv(name, raw)
		if _, err := autoResetWaitForBetterEnabled(); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s=%q accepted (err %v)", name, raw, err)
		}
	}

	prevMode, prevNearEmpty := proxy.AutoReset()
	prevWorth, prevWait := proxy.AutoResetWorth(), proxy.AutoResetWaitForBetter()
	t.Cleanup(func() {
		proxy.SetAutoReset(prevMode, prevNearEmpty)
		proxy.SetAutoResetWorth(prevWorth)
		proxy.SetAutoResetWaitForBetter(prevWait)
	})
	clearControlSecretEnv(t)
	dir := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", dir)
	t.Setenv("SUBROUTER_CLOUD_CONFIG", dir)
	t.Setenv("SUBROUTER_AUTO_RESET", "")
	t.Setenv("SUBROUTER_AUTO_RESET_NEAR_EMPTY", "")
	t.Setenv("SUBROUTER_AUTO_RESET_WORTH", "")
	args := []string{"--addr", "127.0.0.1:0", "--fetch-usage=false", "--sr-switch-interval=0"}
	const offLine = "SUBROUTER_AUTO_RESET_WAIT_FOR_BETTER=0: a claim §94's rule would make is made even when the same reset would be worth clearly more"
	for raw, want := range map[string]bool{"": true, "1": true, "0": false} {
		t.Setenv(name, raw)
		// Not what serve should set, so a serve that never sets it is red.
		proxy.SetAutoResetWaitForBetter(!want)
		log := captureDefaultLog(t)
		if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), "cmux.com config") {
			t.Fatalf("%s=%q: serve returned %v, want the deliberate cloud-config failure", name, raw, err)
		}
		if got := proxy.AutoResetWaitForBetter(); got != want {
			t.Fatalf("%s=%q: serve set the rule %v, want %v", name, raw, got, want)
		}
		if lines := strings.Count(log.String(), offLine); lines != map[bool]int{false: 1}[want] {
			t.Fatalf("%s=%q: startup logged the off line %d times:\n%s", name, raw, lines, log)
		}
	}
	t.Setenv(name, "yes")
	if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), name) {
		t.Fatalf("serve with %s=yes returned %v, want a refusal naming it", name, err)
	}
}
