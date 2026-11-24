package main

import (
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/discordwell/subrouter/internal/proxy"
)

// The hold (PATCHES §58) is configured by these variables, or by the matching
// --hold-* flags, which win when given. Put any override in the LaunchAgent
// plist or /etc/default/subrouter, never in the unit's flags: a rolled-back
// binary that predates the flags exits on them and launchd or systemd
// respawns it forever, while it ignores a variable it does not know. The
// stranded give-up and the Retry-After cap have only their variables, for
// that reason.
const (
	holdDeadlineEnv        = "SUBROUTER_HOLD_DEADLINE"
	holdMaxRequestsEnv     = "SUBROUTER_HOLD_MAX_REQUESTS"
	holdMaxBytesEnv        = "SUBROUTER_HOLD_MAX_BYTES"
	strandedGiveUpEnv      = "SUBROUTER_STRANDED_GIVE_UP"
	clientRetryAfterMaxEnv = "SUBROUTER_CLIENT_RETRY_AFTER_MAX"
)

// holdSettings is serve's hold configuration before it becomes a policy.
type holdSettings struct {
	deadline    time.Duration
	maxRequests int
	maxBytes    int64
	giveUp      time.Duration
}

// resolveHoldSettings applies flag > variable > default to each setting. A
// variable that does not parse is reported and ignored rather than refused:
// a typo in the plist must not stop the daemon that every Claude Code
// session on the machine talks to. A flag that does not parse never gets
// here (flag.Parse refuses it), except --hold-max-bytes, which is a string.
func resolveHoldSettings(flagDeadline time.Duration, deadlineSet bool,
	flagMaxRequests int, maxRequestsSet bool,
	flagMaxBytes string, maxBytesSet bool,
	getenv func(string) string, logger *slog.Logger,
) (holdSettings, error) {
	settings := holdSettings{
		deadline:    proxy.DefaultHoldDeadline,
		maxRequests: proxy.DefaultHoldMaxRequests,
		maxBytes:    proxy.DefaultHoldMaxBytes,
		giveUp:      proxy.DefaultStrandedGiveUp,
	}
	ignored := func(name, value string, err error) {
		if logger != nil {
			logger.Warn("ignoring an unreadable hold setting; using the default",
				"env", name, "value", value, "error", err)
		}
	}
	if deadlineSet {
		settings.deadline = flagDeadline
	} else if raw := strings.TrimSpace(getenv(holdDeadlineEnv)); raw != "" {
		if raw == "0" {
			settings.deadline = 0
		} else if parsed, err := time.ParseDuration(raw); err != nil {
			ignored(holdDeadlineEnv, raw, err)
		} else {
			settings.deadline = parsed
		}
	}
	if maxRequestsSet {
		settings.maxRequests = flagMaxRequests
	} else if raw := strings.TrimSpace(getenv(holdMaxRequestsEnv)); raw != "" {
		if parsed, err := strconv.Atoi(raw); err != nil {
			ignored(holdMaxRequestsEnv, raw, err)
		} else {
			settings.maxRequests = parsed
		}
	}
	if maxBytesSet {
		parsed, err := parseByteSize(flagMaxBytes)
		if err != nil {
			return holdSettings{}, err
		}
		settings.maxBytes = parsed
	} else if raw := strings.TrimSpace(getenv(holdMaxBytesEnv)); raw != "" {
		if parsed, err := parseByteSize(raw); err != nil {
			ignored(holdMaxBytesEnv, raw, err)
		} else {
			settings.maxBytes = parsed
		}
	}
	if raw := strings.TrimSpace(getenv(strandedGiveUpEnv)); raw != "" {
		if raw == "0" {
			settings.giveUp = 0
		} else if parsed, err := time.ParseDuration(raw); err != nil {
			ignored(strandedGiveUpEnv, raw, err)
		} else {
			settings.giveUp = parsed
		}
	}
	return settings, nil
}

// policy is the HoldPolicy serve runs with, and the startup line that says
// what it is. A non-positive deadline is off.
func (h holdSettings) policy(logger *slog.Logger) *proxy.HoldPolicy {
	policy := proxy.NewHoldPolicy(h.deadline, h.maxRequests, h.maxBytes, h.giveUp)
	if logger != nil {
		if policy == nil {
			logger.Info("proxy hold is off: a transient upstream failure is answered at once",
				"env", holdDeadlineEnv)
		} else {
			logger.Info("proxy hold is on: a transient upstream failure is retried until the deadline",
				"deadline", policy.Deadline.String(), "max_requests", policy.MaxRequests,
				"max_bytes", policy.MaxBytes, "stranded_give_up", policy.GiveUp.String())
			if policy.GiveUp <= 0 {
				logger.Info("stranded give-up is off: a chat whose account cannot be reached waits for it across retries with no limit; 0 turns off only the limit",
					"env", strandedGiveUpEnv)
			} else {
				logger.Info("stranded give-up is on: a chat whose account cannot be reached waits for it across retries until this long after its last answered turn, then gets a 503 (stranded_gave_up); a chat a box held then waits for room, and any other is unlinked",
					"stranded_give_up", policy.GiveUp.String(), "env", strandedGiveUpEnv)
				if policy.GiveUp > proxy.PeerHoldIdleTTL {
					logger.Warn("stranded give-up is longer than the warm window: a stranded chat goes unlinked first and its turn is placed cold",
						"stranded_give_up", policy.GiveUp.String(), "warm_idle", proxy.PeerHoldIdleTTL.String(), "env", strandedGiveUpEnv)
				}
			}
		}
	}
	return policy
}

// resolveClientRetryAfterMax is serve's Server.ClientRetryAfterMax (PATCHES
// §78): SUBROUTER_CLIENT_RETRY_AFTER_MAX as a Go duration, or
// DefaultClientRetryAfterMax; 0 turns the cap off. An unreadable value is
// reported and the default kept, as for the hold's variables.
func resolveClientRetryAfterMax(getenv func(string) string, logger *slog.Logger) time.Duration {
	limit := proxy.DefaultClientRetryAfterMax
	if raw := strings.TrimSpace(getenv(clientRetryAfterMaxEnv)); raw != "" {
		if raw == "0" {
			limit = 0
		} else if parsed, err := time.ParseDuration(raw); err != nil {
			if logger != nil {
				logger.Warn("ignoring an unreadable Retry-After cap; using the default",
					"env", clientRetryAfterMaxEnv, "value", raw, "error", err)
			}
		} else {
			limit = parsed
		}
	}
	return limit
}

// logClientRetryAfterMax is serve's start line for the cap. serve passes the
// field of the Server it built, so the line reports the cap the daemon runs
// with, not the one it read.
func logClientRetryAfterMax(limit time.Duration, logger *slog.Logger) {
	if limit <= 0 {
		logger.Info("Retry-After cap is off: an end client is told the whole wait of a spent pool's 503 and of the stranded give-up",
			"env", clientRetryAfterMaxEnv)
		return
	}
	logger.Info("Retry-After cap is on: an end client is told to wait at most this long in a spent pool's 503 and in the stranded give-up; the body keeps the real wait, and a peer daemon gets the whole wait",
		"retry_after_max", limit.String(), "env", clientRetryAfterMaxEnv)
}
