//go:build !darwin && !linux

package main

import (
	"log/slog"
	"time"
)

// startStderrLogCap is the cap on the file behind a daemon's stderr; here there
// is no launchd-style log file to cap (see daemon_log_cap_unix.go).
func startStderrLogCap(int64, int, time.Duration, *slog.Logger) {}
