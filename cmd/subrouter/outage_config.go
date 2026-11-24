package main

import (
	"errors"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/discordwell/subrouter/internal/proxy"
)

// Sticky DNS and the outage spill (PATCHES §59), and the peer-down grace
// (PATCHES §67), are configured by these variables only. There are no flags,
// for the reason hold_config.go gives: a rolled-back binary ignores a
// variable it does not know, and exits on a flag.
const (
	stickyDNSMaxAgeEnv  = "SUBROUTER_STICKY_DNS_MAX_AGE"
	outageSpillRateEnv  = "SUBROUTER_OUTAGE_SPILL_RATE"
	outageSpillBurstEnv = "SUBROUTER_OUTAGE_SPILL_BURST"
	peerDownGraceEnv    = "SUBROUTER_PEER_DOWN_GRACE"
)

// outageSettings is serve's §59 and §67 configuration before it is applied.
type outageSettings struct {
	stickyDNSMaxAge time.Duration
	spillRate       float64
	spillBurst      int
	peerDownGrace   time.Duration
}

// resolveOutageSettings applies variable > default to each setting. A
// variable that does not parse is reported and ignored, as the hold's are: a
// typo in the plist must not stop the daemon. "0" turns sticky DNS, the
// spill or the grace off.
func resolveOutageSettings(getenv func(string) string, logger *slog.Logger) outageSettings {
	settings := outageSettings{
		stickyDNSMaxAge: proxy.DefaultStickyDNSMaxAge,
		spillRate:       proxy.DefaultOutageSpillRate,
		spillBurst:      proxy.DefaultOutageSpillBurst,
		peerDownGrace:   proxy.DefaultPeerDownGrace,
	}
	ignored := func(name, value string, err error) {
		if logger != nil {
			logger.Warn("ignoring an unreadable outage setting; using the default",
				"env", name, "value", value, "error", err)
		}
	}
	if raw := strings.TrimSpace(getenv(stickyDNSMaxAgeEnv)); raw != "" {
		if raw == "0" {
			settings.stickyDNSMaxAge = 0
		} else if parsed, err := time.ParseDuration(raw); err != nil {
			ignored(stickyDNSMaxAgeEnv, raw, err)
		} else {
			settings.stickyDNSMaxAge = parsed
		}
	}
	if raw := strings.TrimSpace(getenv(outageSpillRateEnv)); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err == nil && (math.IsNaN(parsed) || math.IsInf(parsed, 0)) {
			// ParseFloat reads "NaN" and "Inf", and either would be a
			// bucket that never refuses.
			err = errors.New("not a finite number")
		}
		if err != nil {
			ignored(outageSpillRateEnv, raw, err)
		} else {
			settings.spillRate = parsed
		}
	}
	if raw := strings.TrimSpace(getenv(outageSpillBurstEnv)); raw != "" {
		if parsed, err := strconv.Atoi(raw); err != nil {
			ignored(outageSpillBurstEnv, raw, err)
		} else {
			settings.spillBurst = parsed
		}
	}
	if raw := strings.TrimSpace(getenv(peerDownGraceEnv)); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err == nil && parsed < 0 {
			err = errors.New("negative")
		}
		if err != nil {
			ignored(peerDownGraceEnv, raw, err)
		} else {
			settings.peerDownGrace = parsed
		}
	}
	return settings
}

// apply sets the process's sticky DNS age, says at start which way each part
// runs, and returns the spill policy serve runs with (nil is off).
func (o outageSettings) apply(logger *slog.Logger) *proxy.OutageSpillPolicy {
	proxy.SetStickyDNSMaxAge(o.stickyDNSMaxAge)
	spill := proxy.NewOutageSpillPolicy(o.spillRate, o.spillBurst)
	if logger == nil {
		return spill
	}
	if o.stickyDNSMaxAge > 0 {
		logger.Info("sticky dns is on: a failed upstream lookup dials the host's last good address",
			"max_age", o.stickyDNSMaxAge.String())
	} else {
		logger.Info("sticky dns is off: a failed upstream lookup fails the dial", "env", stickyDNSMaxAgeEnv)
	}
	if spill != nil {
		logger.Info("outage spill is on: a turn whose upstream cannot be reached may go to a peer",
			"rate", spill.Rate, "burst", spill.Burst)
	} else {
		logger.Info("outage spill is off: a turn whose upstream cannot be reached stays local", "env", outageSpillRateEnv)
	}
	if o.peerDownGrace > 0 {
		logger.Info("peer down grace is on: a peer is found down only after it answers nothing for the grace",
			"grace", o.peerDownGrace.String(), "env", peerDownGraceEnv)
		if o.peerDownGrace >= proxy.PeerHoldIdleTTL {
			logger.Warn("peer down grace is as long as a peer hold's idle bound: holds lapse first, so a silent peer's chats go cold before it can be found down",
				"grace", o.peerDownGrace.String(), "hold_idle", proxy.PeerHoldIdleTTL.String())
		}
	} else {
		logger.Info("peer down grace is off: a peer is found down at the first opening gateway attribution and §54 blame on it; "+
			"0 turns off only the grace's silence rule", "env", peerDownGraceEnv)
	}
	return spill
}
