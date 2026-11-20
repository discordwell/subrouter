package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// codexEgressConfigFromEnvironment reads SUBROUTER_CODEX_EGRESS_PROXIES, a
// comma-separated list of HTTP CONNECT proxy URLs in other regions
// (for example http://cmux-egress-fra:3128,http://cmux-egress-tyo:3128).
// Unset or empty means the feature is off and the Codex path is unchanged.
// SUBROUTER_CODEX_EGRESS_MAX_ATTEMPTS caps the egresses one request tries
// (default proxy.DefaultCodexEgressMaxAttempts).
func codexEgressConfigFromEnvironment(sessionPath string) (*proxy.CodexEgressConfig, error) {
	raw := strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_EGRESS_PROXIES"))
	if raw == "" {
		return nil, nil
	}
	proxies, err := proxy.ParseCodexEgressProxies(raw)
	if err != nil {
		return nil, err
	}
	if len(proxies) == 0 {
		return nil, nil
	}
	config := &proxy.CodexEgressConfig{
		Proxies:      proxies,
		PinStorePath: filepath.Join(filepath.Dir(sessionPath), "codex-egress-pins.json"),
	}
	if raw := strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_EGRESS_MAX_ATTEMPTS")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("SUBROUTER_CODEX_EGRESS_MAX_ATTEMPTS=%q: want a positive integer", raw)
		}
		config.MaxAttempts = n
	}
	return config, nil
}

// codexOverloadFailoverConfigFromEnvironment reads the Codex capacity retry
// settings. Capacity failures (before any output) are always retried on the
// session's own account, keeping its prompt cache, for up to
// SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT (default 4m; 0 = until the client
// disconnects; ~10s when an egress or Azure fallback is configured), with
// steady gaps of SUBROUTER_CODEX_CAPACITY_RETRY_INTERVAL (default ~9s, at
// least 500ms) after the ramp.
// The account failover (~10s ladder) is ON unless
// SUBROUTER_CODEX_OVERLOAD_FAILOVER=0 turns it off (codexOverloadFailoverEnabled
// says why the fork ships it on where upstream is opt-in), with optional
// SUBROUTER_CODEX_OVERLOAD_MAX_ACCOUNTS and
// SUBROUTER_CODEX_OVERLOAD_MARK_TTL (Go duration). A conversation of more
// than SUBROUTER_CODEX_OVERLOAD_FAILOVER_MAX_INPUT estimated input tokens
// (default 32000; 0 = no limit) keeps its account and its prompt cache: it
// takes the same-account wait even with the failover on. Upstream #389 /
// #454 as at c9303d3, without persist mode and the client retry headers
// (PATCHES section 27). The config is never nil any more: the same-account
// ladder needs it with the failover off too.
func codexOverloadFailoverConfigFromEnvironment() (*proxy.CodexOverloadFailoverConfig, error) {
	enabled, err := codexOverloadFailoverEnabled()
	if err != nil {
		return nil, err
	}
	config := &proxy.CodexOverloadFailoverConfig{Enabled: enabled}
	if raw := strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_OVERLOAD_MAX_ACCOUNTS")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("SUBROUTER_CODEX_OVERLOAD_MAX_ACCOUNTS=%q: want a positive integer", raw)
		}
		config.MaxAccounts = n
	}
	if raw := strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_OVERLOAD_MARK_TTL")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("SUBROUTER_CODEX_OVERLOAD_MARK_TTL=%q: want a positive duration", raw)
		}
		config.MarkTTL = d
	}
	if raw := strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_OVERLOAD_FAILOVER_MAX_INPUT")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("SUBROUTER_CODEX_OVERLOAD_FAILOVER_MAX_INPUT=%q: want a number of input tokens, or 0 for no limit", raw)
		}
		config.FailoverMaxInput, config.FailoverMaxInputUnlimited = n, n == 0
	}
	maxWait, unbounded, _, err := overloadMaxWaitFromEnvironment("SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT")
	if err != nil {
		return nil, err
	}
	config.StayMaxWait, config.StayUnbounded = maxWait, unbounded
	if config.StayInterval, err = overloadIntervalFromEnvironment("SUBROUTER_CODEX_CAPACITY_RETRY_INTERVAL"); err != nil {
		return nil, err
	}
	return config, nil
}

// codexOverloadFailoverEnabled reads SUBROUTER_CODEX_OVERLOAD_FAILOVER. The
// fork ships the capacity failover ON (owner, 2026-09-28: "ship it on"):
// unset means on, and 0/false/no/off turns it off. Upstream is opt-in (unset
// means off). On, a small conversation's capacity failure gets one quick
// same-account retry and then moves to an account that is not shedding
// (marks, capacity eviction after two in a row); a conversation above the
// size cap (SUBROUTER_CODEX_OVERLOAD_FAILOVER_MAX_INPUT, 32k) still waits on
// its own account, keeping its prompt cache. Off, every conversation waits on
// its account. An unrecognized value is an error rather than a silent guess,
// like the other capacity knobs.
func codexOverloadFailoverEnabled() (bool, error) {
	raw := os.Getenv("SUBROUTER_CODEX_OVERLOAD_FAILOVER")
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("SUBROUTER_CODEX_OVERLOAD_FAILOVER=%q: want 1 (the default) or 0", raw)
}
