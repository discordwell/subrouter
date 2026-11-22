package main

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// The daemon's own log (PATCHES §39). serve, supervise and front write
// structured lines through the process logger to stderr, which launchd points
// at ~/Library/Logs/subrouter.err.log and systemd at the journal. Nothing
// capped the launchd file: it reached 930 MB, never rotated, on a disk 98%
// full. newsyslog would need root for a user agent, so the daemon caps the file
// itself (stderrLogCap), and SUBROUTER_LOG_LEVEL sets how much it writes.
const (
	defaultDaemonLogMaxMB = 64
	defaultDaemonLogKeep  = 3
	maxDaemonLogKeep      = 20
	// How often the cap looks at the file. At the laptop's ~100 MB a day the
	// file overshoots the cap by well under 100 KB between looks.
	daemonLogCapInterval = time.Minute
)

// isDaemonLogCommand reports whether args run a long-lived daemon command
// whose stderr is the daemon log. They are the commands that keep the process
// logger (shouldKeepProcessLogger), less the one-shot listener-transfer.
func isDaemonLogCommand(args []string) bool {
	return len(args) > 0 && (args[0] == "serve" || args[0] == "supervise" || args[0] == "standby" || args[0] == "front")
}

// configureDaemonLog applies SUBROUTER_LOG_LEVEL and starts the stderr cap for
// a daemon command. main calls it once, before the command runs; it changes
// process-wide state (the default logger's level, a goroutine on fd 2), so
// nothing else may.
//
// Start anyway, and complain: a malformed value falls back to its default and
// says so. Refusing to start under launchd's KeepAlive would turn a typo into a
// restart loop.
func configureDaemonLog(args []string) {
	if !isDaemonLogCommand(args) {
		return
	}
	level, problem := daemonLogLevel(os.Getenv("SUBROUTER_LOG_LEVEL"))
	// serve never replaces the default logger, so this sets the level the
	// default handler writes at: every slog call in the daemon, unchanged
	// format.
	slog.SetLogLoggerLevel(level)
	if problem != "" {
		slog.Warn("SUBROUTER_LOG_LEVEL is malformed; logging at info", "problem", problem)
	}
	maxBytes, keep, problems := daemonLogCapConfig(os.Getenv("SUBROUTER_LOG_MAX_MB"), os.Getenv("SUBROUTER_LOG_KEEP"))
	for _, problem := range problems {
		slog.Warn("daemon log cap setting is malformed; using its default", "problem", problem)
	}
	if maxBytes > 0 {
		startStderrLogCap(maxBytes, keep, daemonLogCapInterval, slog.Default())
	}
}

// daemonLogLevel parses SUBROUTER_LOG_LEVEL: debug, info (the default), warn
// or error, any case, with slog's +N/-N offsets. Empty is info.
func daemonLogLevel(raw string) (slog.Level, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return slog.LevelInfo, ""
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(raw)); err != nil {
		return slog.LevelInfo, fmt.Sprintf("%q: want debug, info, warn or error", raw)
	}
	return level, ""
}

// daemonLogCapConfig parses SUBROUTER_LOG_MAX_MB (the size, in MiB, past which
// the file is rotated; 0 turns the cap off) and SUBROUTER_LOG_KEEP (how many
// rotated files are kept beside it, 1 to maxDaemonLogKeep).
func daemonLogCapConfig(rawMaxMB, rawKeep string) (int64, int, []string) {
	var problems []string
	maxMB := int64(defaultDaemonLogMaxMB)
	if raw := strings.TrimSpace(rawMaxMB); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 || parsed > 1<<20 {
			problems = append(problems, fmt.Sprintf("SUBROUTER_LOG_MAX_MB=%q: want a whole number of MiB, 0 to turn the cap off; using %d", raw, defaultDaemonLogMaxMB))
		} else {
			maxMB = parsed
		}
	}
	keep := defaultDaemonLogKeep
	if raw := strings.TrimSpace(rawKeep); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxDaemonLogKeep {
			problems = append(problems, fmt.Sprintf("SUBROUTER_LOG_KEEP=%q: want 1 to %d; using %d", raw, maxDaemonLogKeep, defaultDaemonLogKeep))
		} else {
			keep = parsed
		}
	}
	return maxMB << 20, keep, problems
}

// rotatedDaemonLogName is the n-th rotated copy of path: path.1 is the newest.
func rotatedDaemonLogName(path string, n int) string {
	return path + "." + strconv.Itoa(n)
}
