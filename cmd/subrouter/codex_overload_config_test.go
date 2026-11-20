package main

import "testing"

// Upstream manaflow-ai/subrouter's test (c9303d3, #457). Since the 389/373
// port the reader always returns a config, as upstream's does; the failover
// is set here because the cap only matters with it on. PATCHES section 27.
func TestCodexOverloadConfigReadsFailoverMaxInput(t *testing.T) {
	t.Setenv("SUBROUTER_CODEX_OVERLOAD_FAILOVER", "1")
	t.Setenv("SUBROUTER_CODEX_OVERLOAD_FAILOVER_MAX_INPUT", "")
	config, err := codexOverloadFailoverConfigFromEnvironment()
	if err != nil || config.FailoverMaxInput != 0 || config.FailoverMaxInputUnlimited {
		t.Fatalf("unset: config = %+v err = %v, want the built-in default", config, err)
	}
	t.Setenv("SUBROUTER_CODEX_OVERLOAD_FAILOVER_MAX_INPUT", "200000")
	if config, err = codexOverloadFailoverConfigFromEnvironment(); err != nil || config.FailoverMaxInput != 200000 || config.FailoverMaxInputUnlimited {
		t.Fatalf("200000: config = %+v err = %v", config, err)
	}
	t.Setenv("SUBROUTER_CODEX_OVERLOAD_FAILOVER_MAX_INPUT", "0")
	if config, err = codexOverloadFailoverConfigFromEnvironment(); err != nil || !config.FailoverMaxInputUnlimited {
		t.Fatalf("0: config = %+v err = %v, want no limit", config, err)
	}
	for _, bad := range []string{"-1", "32k", "lots"} {
		t.Setenv("SUBROUTER_CODEX_OVERLOAD_FAILOVER_MAX_INPUT", bad)
		if _, err := codexOverloadFailoverConfigFromEnvironment(); err == nil {
			t.Fatalf("SUBROUTER_CODEX_OVERLOAD_FAILOVER_MAX_INPUT=%q accepted", bad)
		}
	}
}

// With the failover off the capacity retry is still configured (same account
// only). Upstream's test (#389), without its persist and client-header
// fields, which the fork does not port, and with the failover turned off
// explicitly: the fork ships it on (TestCodexOverloadFailoverShipsOn).
func TestCodexOverloadConfigDefaultsToSameAccountRetry(t *testing.T) {
	for _, key := range []string{"SUBROUTER_CODEX_OVERLOAD_MAX_ACCOUNTS", "SUBROUTER_CODEX_OVERLOAD_MARK_TTL", "SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT", "SUBROUTER_CODEX_CAPACITY_RETRY_INTERVAL"} {
		t.Setenv(key, "")
	}
	t.Setenv("SUBROUTER_CODEX_OVERLOAD_FAILOVER", "0")
	config, err := codexOverloadFailoverConfigFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if config == nil || config.Enabled {
		t.Fatalf("config = %+v, want the same-account default with the failover off", config)
	}
	t.Setenv("SUBROUTER_CODEX_OVERLOAD_FAILOVER", "1")
	if config, err = codexOverloadFailoverConfigFromEnvironment(); err != nil || !config.Enabled {
		t.Fatalf("config = %+v err = %v, want the failover opted in", config, err)
	}
}

// The fork ships the capacity failover on (owner, 2026-09-28): unset is on,
// and only an explicit 0/false/no/off turns it off. Upstream is opt-in. An
// unrecognized value fails loudly instead of silently picking a side.
func TestCodexOverloadFailoverShipsOn(t *testing.T) {
	for _, raw := range []string{"", "  ", "1", "true", "YES", "on"} {
		t.Setenv("SUBROUTER_CODEX_OVERLOAD_FAILOVER", raw)
		config, err := codexOverloadFailoverConfigFromEnvironment()
		if err != nil || config == nil || !config.Enabled {
			t.Fatalf("SUBROUTER_CODEX_OVERLOAD_FAILOVER=%q: config = %+v err = %v, want the failover on", raw, config, err)
		}
	}
	for _, raw := range []string{"0", "false", "No", "off"} {
		t.Setenv("SUBROUTER_CODEX_OVERLOAD_FAILOVER", raw)
		config, err := codexOverloadFailoverConfigFromEnvironment()
		if err != nil || config == nil || config.Enabled {
			t.Fatalf("SUBROUTER_CODEX_OVERLOAD_FAILOVER=%q: config = %+v err = %v, want the failover off (same-account ladder only)", raw, config, err)
		}
	}
	for _, raw := range []string{"2", "maybe"} {
		t.Setenv("SUBROUTER_CODEX_OVERLOAD_FAILOVER", raw)
		if _, err := codexOverloadFailoverConfigFromEnvironment(); err == nil {
			t.Fatalf("SUBROUTER_CODEX_OVERLOAD_FAILOVER=%q accepted", raw)
		}
	}
}
