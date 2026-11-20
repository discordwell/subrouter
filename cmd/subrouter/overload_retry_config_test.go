package main

import (
	"strings"
	"testing"
	"time"
)

// Upstream manaflow-ai/subrouter's TestOverloadMaxWaitEnvironment and
// TestOverloadRetryIntervalEnvironment (#389, as at c9303d3), Codex halves
// only: the fork does not port the Claude overload ladder (PATCHES section 27).

func TestOverloadMaxWaitEnvironment(t *testing.T) {
	t.Setenv("SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT", "")
	codex, err := codexOverloadFailoverConfigFromEnvironment()
	if err != nil || codex.StayMaxWait != 0 || codex.StayUnbounded {
		t.Fatalf("codex = %+v err = %v, want the default", codex, err)
	}

	t.Setenv("SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT", "90s")
	if codex, err = codexOverloadFailoverConfigFromEnvironment(); err != nil || codex.StayMaxWait != 90*time.Second || codex.StayUnbounded {
		t.Fatalf("codex = %+v err = %v, want 90s", codex, err)
	}

	for _, zero := range []string{"0", "0s"} {
		t.Setenv("SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT", zero)
		if codex, err = codexOverloadFailoverConfigFromEnvironment(); err != nil || !codex.StayUnbounded {
			t.Fatalf("codex %q = %+v err = %v, want no cap", zero, codex, err)
		}
	}

	for _, bad := range []string{"soon", "-1m"} {
		t.Setenv("SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT", bad)
		if _, err := codexOverloadFailoverConfigFromEnvironment(); err == nil {
			t.Fatalf("SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT=%q accepted", bad)
		}
		t.Setenv("SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT", "")
	}
}

func TestOverloadRetryIntervalEnvironment(t *testing.T) {
	for _, key := range []string{"SUBROUTER_CODEX_CAPACITY_RETRY_INTERVAL", "SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT"} {
		t.Setenv(key, "")
	}
	t.Setenv("SUBROUTER_CODEX_CAPACITY_RETRY_INTERVAL", "500ms")
	codex, err := codexOverloadFailoverConfigFromEnvironment()
	if err != nil || codex.StayInterval != 500*time.Millisecond {
		t.Fatalf("codex = %+v err = %v, want the 500ms floor accepted", codex, err)
	}

	// Below the floor is rejected, not clamped.
	for _, bad := range []string{"499ms", "0", "fast"} {
		t.Setenv("SUBROUTER_CODEX_CAPACITY_RETRY_INTERVAL", bad)
		if _, err := codexOverloadFailoverConfigFromEnvironment(); err == nil {
			t.Fatalf("SUBROUTER_CODEX_CAPACITY_RETRY_INTERVAL=%q accepted", bad)
		} else if bad == "499ms" && !strings.Contains(err.Error(), "at least 500ms") {
			t.Fatalf("error = %v, want it to name the 500ms floor", err)
		}
		t.Setenv("SUBROUTER_CODEX_CAPACITY_RETRY_INTERVAL", "")
	}
}
