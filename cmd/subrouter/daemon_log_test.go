package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// PATCHES §39: SUBROUTER_LOG_LEVEL and the daemon log cap's settings.

func TestDaemonLogLevel(t *testing.T) {
	for raw, want := range map[string]slog.Level{
		"":        slog.LevelInfo,
		"info":    slog.LevelInfo,
		" DEBUG ": slog.LevelDebug,
		"warn":    slog.LevelWarn,
		"Error":   slog.LevelError,
	} {
		if got, problem := daemonLogLevel(raw); got != want || problem != "" {
			t.Errorf("daemonLogLevel(%q) = %v, %q; want %v", raw, got, problem, want)
		}
	}
	// A typo starts the daemon at info and says so; it never refuses to start.
	for _, raw := range []string{"verbose", "debug2", "trace"} {
		if got, problem := daemonLogLevel(raw); got != slog.LevelInfo || problem == "" {
			t.Errorf("daemonLogLevel(%q) = %v, %q; want info and a problem", raw, got, problem)
		}
	}
}

func TestDaemonLogCapConfig(t *testing.T) {
	for _, tc := range []struct {
		maxMB, keep  string
		wantBytes    int64
		wantKeep     int
		wantProblems int
	}{
		{"", "", defaultDaemonLogMaxMB << 20, defaultDaemonLogKeep, 0},
		{"100", "5", 100 << 20, 5, 0},
		{"0", "", 0, defaultDaemonLogKeep, 0}, // 0 turns the cap off
		{"lots", "0", defaultDaemonLogMaxMB << 20, defaultDaemonLogKeep, 2},
		{"-1", "21", defaultDaemonLogMaxMB << 20, defaultDaemonLogKeep, 2},
	} {
		gotBytes, gotKeep, problems := daemonLogCapConfig(tc.maxMB, tc.keep)
		if gotBytes != tc.wantBytes || gotKeep != tc.wantKeep || len(problems) != tc.wantProblems {
			t.Errorf("daemonLogCapConfig(%q, %q) = %d, %d, %v; want %d, %d and %d problems",
				tc.maxMB, tc.keep, gotBytes, gotKeep, problems, tc.wantBytes, tc.wantKeep, tc.wantProblems)
		}
	}
}

// Only the long-lived daemon commands touch the process log.
func TestIsDaemonLogCommand(t *testing.T) {
	for _, args := range [][]string{{"serve"}, {"supervise", "--worker-bin", "x"}, {"front"}, {"standby"}} {
		if !isDaemonLogCommand(args) {
			t.Errorf("%v is a daemon command", args)
		}
	}
	for _, args := range [][]string{nil, {"status"}, {"listener-transfer"}, {"codex", "isolation-check"}, {"daemon", "logs"}} {
		if isDaemonLogCommand(args) {
			t.Errorf("%v is not a daemon command", args)
		}
	}
}

// runDaemonLogChild is the test binary standing in for `subrouter serve` under
// launchd (TestMain, SUBROUTER_TEST_DAEMON_LOG_CHILD): main's
// configureDaemonLog on the stderr its parent opened, then one line at each
// level. It waits until the file is under the cap and not empty: a file that
// started over the cap has then been truncated AND carries the cap's own
// "daemon log rotated" line, so this process cannot exit before writing it.
func runDaemonLogChild() {
	configureDaemonLog([]string{"serve"})
	deadline := time.Now().Add(20 * time.Second)
	for {
		info, err := os.Stderr.Stat()
		if err == nil && info.Size() > 0 && info.Size() < 1<<20 {
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(os.Stdout, "child: stderr never came under the cap")
			os.Exit(2)
		}
		time.Sleep(10 * time.Millisecond)
	}
	slog.Debug("child debug line")
	slog.Info("child info line")
	os.Exit(0)
}

func containsAll(s string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(s, part) {
			return false
		}
	}
	return true
}
