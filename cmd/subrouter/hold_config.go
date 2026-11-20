package main

import (
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// The hold (PATCHES §58) is configured by these variables, or by the matching
// --hold-* flags, which win when given. Put any override in the LaunchAgent
// plist or /etc/default/subrouter, never in the unit's flags: a rolled-back
// binary that predates the flags exits on them and launchd or systemd
// respawns it forever, while it ignores a variable it does not know. The
// stranded give-up has only its variable, for that reason.
const (
	holdDeadlineEnv    = "SUBROUTER_HOLD_DEADLINE"
	holdMaxRequestsEnv = "SUBROUTER_HOLD_MAX_REQUESTS"
	holdMaxBytesEnv    = "SUBROUTER_HOLD_MAX_BYTES"
	strandedGiveUpEnv  = "SUBROUTER_STRANDED_GIVE_UP"
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
				logger.Info("stranded give-up is on: a chat whose account cannot be reached waits for it across retries until this long after its last answered turn, then gets 503 Retry-After 3600 (stranded_gave_up) and is unlinked",
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
