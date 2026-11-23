package main

import (
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// PATCHES §91.1's switch reads like §90's: unset is on, only an explicit
// 0/false/no/off turns it off, and any other value fails loudly.
func TestReadCodexResetExpiryFromEnvironment(t *testing.T) {
	const name = "SUBROUTER_READ_CODEX_RESET_EXPIRY"
	for _, c := range []struct {
		raw  []string
		want bool
	}{
		{[]string{"", "  ", "1", "true", "YES", "on"}, true},
		{[]string{"0", "false", "No", "OFF"}, false},
	} {
		for _, raw := range c.raw {
			t.Setenv(name, raw)
			if got, err := readCodexResetExpiryEnabled(); err != nil || got != c.want {
				t.Fatalf("%s=%q: got %v err %v, want %v", name, raw, got, err, c.want)
			}
		}
	}
	for _, raw := range []string{"2", "maybe", "disabled"} {
		t.Setenv(name, raw)
		if _, err := readCodexResetExpiryEnabled(); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s=%q accepted (err %v)", name, raw, err)
		}
	}
}

// serve sets the reader's switch for the process, says once at startup when
// it is off, and refuses to start on a value it cannot read.
func TestServeSetsTheCodexResetExpiryReader(t *testing.T) {
	prev := proxy.ReadCodexResetExpiry()
	t.Cleanup(func() { proxy.SetReadCodexResetExpiry(prev) })
	clearControlSecretEnv(t)
	dir := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", dir)
	t.Setenv("SUBROUTER_CLOUD_CONFIG", dir)
	args := []string{"--addr", "127.0.0.1:0", "--fetch-usage=false", "--sr-switch-interval=0"}
	const logLine = "codex reset credit expiry reads disabled by SUBROUTER_READ_CODEX_RESET_EXPIRY"

	for _, env := range []string{"", "0"} {
		t.Setenv("SUBROUTER_READ_CODEX_RESET_EXPIRY", env)
		want := env == ""
		// The opposite of what serve should set, so a serve that never sets
		// it is red here.
		proxy.SetReadCodexResetExpiry(!want)
		log := captureDefaultLog(t)
		if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), "cmux.com config") {
			t.Fatalf("env %q: serve returned %v, want the deliberate cloud-config failure", env, err)
		}
		if got := proxy.ReadCodexResetExpiry(); got != want {
			t.Fatalf("env %q: serve left the reader %v, want %v", env, got, want)
		}
		wantLines := 0
		if !want {
			wantLines = 1
		}
		if got := strings.Count(log.String(), logLine); got != wantLines ||
			(!want && !strings.Contains(log.String(), "unset SUBROUTER_READ_CODEX_RESET_EXPIRY and restart")) {
			t.Fatalf("env %q: startup logged %q %d times, want %d with the re-enable hint:\n%s", env, logLine, got, wantLines, log)
		}
	}

	t.Setenv("SUBROUTER_READ_CODEX_RESET_EXPIRY", "disabled")
	if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), "SUBROUTER_READ_CODEX_RESET_EXPIRY") {
		t.Fatalf("serve with SUBROUTER_READ_CODEX_RESET_EXPIRY=disabled returned %v, want a refusal naming it", err)
	}
}
