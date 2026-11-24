package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/discordwell/subrouter/internal/proxy"
)

// The Codex same-account capacity ladder's operator knobs, from upstream
// manaflow-ai/subrouter's overload_retry_config.go (#389, as at c9303d3). The
// Claude reader in that file (SUBROUTER_CLAUDE_OVERLOAD_*) is not ported: the
// fork has not taken upstream's Claude overload ladder (PATCHES section 27).

// overloadMaxWaitFromEnvironment reads a same-account overload wall-clock
// cap: a Go duration, where 0 means no cap (wait until the client
// disconnects). Unset returns ok=false so the default applies.
func overloadMaxWaitFromEnvironment(key string) (maxWait time.Duration, unbounded bool, ok bool, err error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, false, false, nil
	}
	d, parseErr := time.ParseDuration(raw)
	if parseErr != nil && raw == "0" {
		d, parseErr = 0, nil
	}
	if parseErr != nil || d < 0 {
		return 0, false, false, fmt.Errorf("%s=%q: want a duration such as 8m, or 0 for no cap", key, raw)
	}
	if d == 0 {
		return 0, true, true, nil
	}
	return d, false, true, nil
}

// overloadIntervalFromEnvironment reads a same-account overload steady gap:
// a Go duration of at least 500ms. A smaller value is rejected rather than
// raised silently. Unset returns 0 so the default applies.
func overloadIntervalFromEnvironment(key string) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: want a duration such as 15s", key, raw)
	}
	if d < proxy.OverloadRetryMinInterval {
		return 0, fmt.Errorf("%s=%q: the retry interval must be at least %s", key, raw, proxy.OverloadRetryMinInterval)
	}
	return d, nil
}
