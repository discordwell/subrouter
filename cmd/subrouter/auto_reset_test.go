package main

import (
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// 1. PATCHES §93.1's two switches. SUBROUTER_AUTO_RESET unset or shadow is
// shadow, 0 is off, 1 is real; SUBROUTER_AUTO_RESET_NEAR_EMPTY unset or 1 is
// on, 0 is off. Any other value of either is refused with a message naming
// the setting.
func TestAutoResetFromEnvironment(t *testing.T) {
	const mode, nearEmpty = "SUBROUTER_AUTO_RESET", "SUBROUTER_AUTO_RESET_NEAR_EMPTY"
	for raw, want := range map[string]proxy.AutoResetMode{
		"": proxy.AutoResetShadow, " ": proxy.AutoResetShadow, "shadow": proxy.AutoResetShadow,
		"0": proxy.AutoResetOff, "1": proxy.AutoResetReal,
	} {
		t.Setenv(mode, raw)
		if got, err := autoResetMode(); err != nil || got != want {
			t.Fatalf("%s=%q: got %v err %v, want %v", mode, raw, got, err, want)
		}
	}
	for _, raw := range []string{"2", "true", "on", "off", "Shadow", "real"} {
		t.Setenv(mode, raw)
		if _, err := autoResetMode(); err == nil || !strings.Contains(err.Error(), mode) {
			t.Fatalf("%s=%q accepted (err %v)", mode, raw, err)
		}
	}
	for raw, want := range map[string]bool{"": true, " ": true, "1": true, "0": false} {
		t.Setenv(nearEmpty, raw)
		if got, err := autoResetNearEmptyEnabled(); err != nil || got != want {
			t.Fatalf("%s=%q: got %v err %v, want %v", nearEmpty, raw, got, err, want)
		}
	}
	for _, raw := range []string{"2", "true", "off", "no"} {
		t.Setenv(nearEmpty, raw)
		if _, err := autoResetNearEmptyEnabled(); err == nil || !strings.Contains(err.Error(), nearEmpty) {
			t.Fatalf("%s=%q accepted (err %v)", nearEmpty, raw, err)
		}
	}
}

// serve sets both switches for the process, says at startup what is not the
// default, and refuses to start on a value it cannot read.
func TestServeSetsTheAutoReset(t *testing.T) {
	prevMode, prevNearEmpty := proxy.AutoReset()
	t.Cleanup(func() { proxy.SetAutoReset(prevMode, prevNearEmpty) })
	clearControlSecretEnv(t)
	dir := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", dir)
	t.Setenv("SUBROUTER_CLOUD_CONFIG", dir)
	args := []string{"--addr", "127.0.0.1:0", "--fetch-usage=false", "--sr-switch-interval=0"}
	const offLine, realLine, conditionLine = "automatic resets disabled by SUBROUTER_AUTO_RESET=0",
		"SUBROUTER_AUTO_RESET=1: this daemon claims its accounts' saved resets by itself",
		"the under-1-percent condition on automatic claims is off"

	for _, c := range []struct {
		mode, nearEmpty string
		want            proxy.AutoResetMode
		wantNearEmpty   bool
		lines           []string
	}{
		{"", "", proxy.AutoResetShadow, true, nil},
		{"0", "", proxy.AutoResetOff, true, []string{offLine}},
		{"1", "", proxy.AutoResetReal, true, []string{realLine}},
		{"shadow", "0", proxy.AutoResetShadow, false, []string{conditionLine}},
	} {
		t.Setenv("SUBROUTER_AUTO_RESET", c.mode)
		t.Setenv("SUBROUTER_AUTO_RESET_NEAR_EMPTY", c.nearEmpty)
		// Not what serve should set, so a serve that never sets them is red.
		proxy.SetAutoReset(proxy.AutoResetMode(-1), !c.wantNearEmpty)
		log := captureDefaultLog(t)
		if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), "cmux.com config") {
			t.Fatalf("env %q/%q: serve returned %v, want the deliberate cloud-config failure", c.mode, c.nearEmpty, err)
		}
		if mode, nearEmpty := proxy.AutoReset(); mode != c.want || nearEmpty != c.wantNearEmpty {
			t.Fatalf("env %q/%q: serve set %v and the condition %v, want %v and %v", c.mode, c.nearEmpty, mode, nearEmpty, c.want, c.wantNearEmpty)
		}
		for _, line := range []string{offLine, realLine, conditionLine} {
			want := 0
			for _, expected := range c.lines {
				if expected == line {
					want = 1
				}
			}
			if got := strings.Count(log.String(), line); got != want {
				t.Fatalf("env %q/%q: startup logged %q %d times, want %d:\n%s", c.mode, c.nearEmpty, line, got, want, log)
			}
		}
	}

	for name, value := range map[string]string{"SUBROUTER_AUTO_RESET": "yes", "SUBROUTER_AUTO_RESET_NEAR_EMPTY": "off"} {
		t.Setenv("SUBROUTER_AUTO_RESET", "")
		t.Setenv("SUBROUTER_AUTO_RESET_NEAR_EMPTY", "")
		t.Setenv(name, value)
		if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("serve with %s=%s returned %v, want a refusal naming it", name, value, err)
		}
	}
}

// PATCHES §94.2: SUBROUTER_AUTO_RESET_WORTH unset or 1 is on, 0 goes back
// to §93.1's rule, and any other value is refused with a message naming it.
// serve sets it for the process and says at startup when it is off.
func TestServeSetsTheAutoResetWorthRule(t *testing.T) {
	const name = "SUBROUTER_AUTO_RESET_WORTH"
	for raw, want := range map[string]bool{"": true, " ": true, "1": true, "0": false} {
		t.Setenv(name, raw)
		if got, err := autoResetWorthEnabled(); err != nil || got != want {
			t.Fatalf("%s=%q: got %v err %v, want %v", name, raw, got, err, want)
		}
	}
	for _, raw := range []string{"2", "true", "on", "off", "no"} {
		t.Setenv(name, raw)
		if _, err := autoResetWorthEnabled(); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s=%q accepted (err %v)", name, raw, err)
		}
	}

	prevMode, prevNearEmpty := proxy.AutoReset()
	prevWorth := proxy.AutoResetWorth()
	t.Cleanup(func() {
		proxy.SetAutoReset(prevMode, prevNearEmpty)
		proxy.SetAutoResetWorth(prevWorth)
	})
	clearControlSecretEnv(t)
	dir := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", dir)
	t.Setenv("SUBROUTER_CLOUD_CONFIG", dir)
	t.Setenv("SUBROUTER_AUTO_RESET", "")
	t.Setenv("SUBROUTER_AUTO_RESET_NEAR_EMPTY", "")
	args := []string{"--addr", "127.0.0.1:0", "--fetch-usage=false", "--sr-switch-interval=0"}
	const offLine = "SUBROUTER_AUTO_RESET_WORTH=0: an automatic claim follows §93's rule alone"
	for raw, want := range map[string]bool{"": true, "1": true, "0": false} {
		t.Setenv(name, raw)
		// Not what serve should set, so a serve that never sets it is red.
		proxy.SetAutoResetWorth(!want)
		log := captureDefaultLog(t)
		if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), "cmux.com config") {
			t.Fatalf("%s=%q: serve returned %v, want the deliberate cloud-config failure", name, raw, err)
		}
		if got := proxy.AutoResetWorth(); got != want {
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

// 11. PATCHES §94.2: SUBROUTER_AUTO_RESET=1 no longer refuses to start beside
// the failover redeem or the background spender in its spending mode. serve
// starts and logs one Warn for each, naming its setting and saying that
// automatic resets now decide; the proxy turns both off in real mode. In
// shadow or off both older paths start as they did, with no Warn.
func TestRealModeFoldsTheOlderCodexSpenders(t *testing.T) {
	prevMode, prevNearEmpty := proxy.AutoReset()
	t.Cleanup(func() { proxy.SetAutoReset(prevMode, prevNearEmpty) })
	clearControlSecretEnv(t)
	dir := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", dir)
	t.Setenv("SUBROUTER_CLOUD_CONFIG", dir)
	t.Setenv("SUBROUTER_AUTO_RESET_NEAR_EMPTY", "")
	const failoverLine, spenderLine = "SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET is set and SUBROUTER_AUTO_RESET=1: the failover redeem is off, automatic resets now decide",
		"--reset-credit-autospend=spend and SUBROUTER_AUTO_RESET=1: the background spender redeems nothing and only advises"
	for _, c := range []struct {
		mode, failoverRedeem, spender string
		lines                         []string
	}{
		{"1", "1", "warn", []string{failoverLine}},
		{"1", "", "spend", []string{spenderLine}},
		{"1", "1", "spend", []string{failoverLine, spenderLine}},
		{"1", "", "warn", nil},
		{"shadow", "1", "spend", nil},
		{"0", "1", "spend", nil},
	} {
		t.Setenv("SUBROUTER_AUTO_RESET", c.mode)
		t.Setenv("SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET", c.failoverRedeem)
		log := captureDefaultLog(t)
		err := serveMustReturn(t, []string{"--addr", "127.0.0.1:0", "--fetch-usage=false", "--sr-switch-interval=0",
			"--reset-credit-autospend=" + c.spender})
		if err == nil || !strings.Contains(err.Error(), "cmux.com config") {
			t.Fatalf("%+v: serve returned %v, want it to start (and meet the deliberate cloud-config failure)", c, err)
		}
		for _, line := range []string{failoverLine, spenderLine} {
			want := 0
			for _, expected := range c.lines {
				if expected == line {
					want = 1
				}
			}
			if got := strings.Count(log.String(), line); got != want {
				t.Fatalf("%+v: startup logged %q %d times, want %d:\n%s", c, line, got, want, log)
			}
		}
		if want := len(c.lines); strings.Count(log.String(), "automatic resets now decide every Codex claim") != want ||
			(want > 0 && !strings.Contains(log.String(), "level=WARN")) {
			t.Fatalf("%+v: want %d Warns that say automatic resets now decide:\n%s", c, want, log)
		}
	}
}
