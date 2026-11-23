package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// PATCHES §92.1's switch reads like §90's: unset is on, only an explicit
// 0/false/no/off turns it off, and any other value stops serve.
func TestUsageHistoryFromEnvironment(t *testing.T) {
	const name = "SUBROUTER_USAGE_HISTORY"
	for _, c := range []struct {
		raw  []string
		want bool
	}{
		{[]string{"", "  ", "1", "true", "YES", "on"}, true},
		{[]string{"0", "false", "No", "OFF"}, false},
	} {
		for _, raw := range c.raw {
			t.Setenv(name, raw)
			if got, err := usageHistoryEnabled(); err != nil || got != c.want {
				t.Fatalf("%s=%q: got %v err %v, want %v", name, raw, got, err, c.want)
			}
		}
	}
	for _, raw := range []string{"2", "maybe", "disabled"} {
		t.Setenv(name, raw)
		if _, err := usageHistoryEnabled(); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s=%q accepted (err %v)", name, raw, err)
		}
	}
}

// Off, serve keeps no history, so nothing is written to the state directory;
// on, the history is the state directory's usage-history.jsonl.
func TestServeUsageHistoryIsOffWithTheSwitch(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	if h := serveUsageHistory(false, dir, logger); h != nil {
		t.Fatal("the switch off still made a usage history")
	}
	if err := serveUsageHistory(false, dir, logger).Close(); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("the switch off wrote %v into the state directory", entries)
	}
	if h := serveUsageHistory(true, dir, logger); h == nil {
		t.Fatal("the switch on made no usage history")
	}
	if _, err := os.Stat(filepath.Join(dir, proxy.UsageHistoryFile)); !os.IsNotExist(err) {
		t.Fatalf("an empty history wrote its file at start: %v", err)
	}
}

// serve says once at startup when the switch is off, and refuses to start on
// a value it cannot read.
func TestServeReadsTheUsageHistorySwitch(t *testing.T) {
	clearControlSecretEnv(t)
	dir := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", dir)
	t.Setenv("SUBROUTER_CLOUD_CONFIG", dir)
	args := []string{"--addr", "127.0.0.1:0", "--fetch-usage=false", "--sr-switch-interval=0"}
	const logLine = "usage history disabled by SUBROUTER_USAGE_HISTORY"
	for _, env := range []string{"", "0"} {
		t.Setenv("SUBROUTER_USAGE_HISTORY", env)
		log := captureDefaultLog(t)
		if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), "cmux.com config") {
			t.Fatalf("env %q: serve returned %v, want the deliberate cloud-config failure", env, err)
		}
		want := 0
		if env == "0" {
			want = 1
		}
		if got := strings.Count(log.String(), logLine); got != want ||
			(want == 1 && !strings.Contains(log.String(), "unset SUBROUTER_USAGE_HISTORY and restart")) {
			t.Fatalf("env %q: startup logged %q %d times, want %d with the re-enable hint:\n%s", env, logLine, got, want, log)
		}
	}
	t.Setenv("SUBROUTER_USAGE_HISTORY", "disabled")
	if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), "SUBROUTER_USAGE_HISTORY") {
		t.Fatalf("serve with SUBROUTER_USAGE_HISTORY=disabled returned %v, want a refusal naming it", err)
	}
}
