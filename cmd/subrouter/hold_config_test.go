package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// The hold's settings (PATCHES §58): flag > variable > default, and a
// variable that does not parse falls back to the default with a warning
// instead of stopping the daemon.
func TestResolveHoldSettings(t *testing.T) {
	defaults := holdSettings{deadline: proxy.DefaultHoldDeadline, maxRequests: proxy.DefaultHoldMaxRequests, maxBytes: proxy.DefaultHoldMaxBytes, giveUp: 60 * time.Minute}
	cases := []struct {
		name     string
		env      map[string]string
		flags    func() (time.Duration, bool, int, bool, string, bool)
		want     holdSettings
		warnings int
	}{
		{name: "defaults", want: defaults},
		{
			name: "variables",
			env:  map[string]string{holdDeadlineEnv: "3m", holdMaxRequestsEnv: "12", holdMaxBytesEnv: "64MiB", strandedGiveUpEnv: "90m"},
			want: holdSettings{deadline: 3 * time.Minute, maxRequests: 12, maxBytes: 64 << 20, giveUp: 90 * time.Minute},
		},
		{
			name: "zero turns it off",
			env:  map[string]string{holdDeadlineEnv: "0"},
			want: holdSettings{deadline: 0, maxRequests: proxy.DefaultHoldMaxRequests, maxBytes: proxy.DefaultHoldMaxBytes, giveUp: 60 * time.Minute},
		},
		{
			name: "zero is no limit on a stranded chat's holds",
			env:  map[string]string{strandedGiveUpEnv: "0"},
			want: holdSettings{deadline: proxy.DefaultHoldDeadline, maxRequests: proxy.DefaultHoldMaxRequests, maxBytes: proxy.DefaultHoldMaxBytes},
		},
		{
			name:     "an unreadable variable keeps the default",
			env:      map[string]string{holdDeadlineEnv: "eight minutes", holdMaxRequestsEnv: "many", holdMaxBytesEnv: "lots", strandedGiveUpEnv: "an hour"},
			want:     defaults,
			warnings: 4,
		},
		{
			name: "flags win over variables",
			env:  map[string]string{holdDeadlineEnv: "3m", holdMaxRequestsEnv: "12", holdMaxBytesEnv: "64MiB"},
			flags: func() (time.Duration, bool, int, bool, string, bool) {
				return 0, true, 4, true, "1MiB", true
			},
			want: holdSettings{deadline: 0, maxRequests: 4, maxBytes: 1 << 20, giveUp: 60 * time.Minute},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deadline, deadlineSet, maxRequests, maxRequestsSet, maxBytes, maxBytesSet := proxy.DefaultHoldDeadline, false, proxy.DefaultHoldMaxRequests, false, "512MiB", false
			if tc.flags != nil {
				deadline, deadlineSet, maxRequests, maxRequestsSet, maxBytes, maxBytesSet = tc.flags()
			}
			var logs bytes.Buffer
			got, err := resolveHoldSettings(deadline, deadlineSet, maxRequests, maxRequestsSet, maxBytes, maxBytesSet,
				func(name string) string { return tc.env[name] }, slog.New(slog.NewTextHandler(&logs, nil)))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("settings = %+v, want %+v", got, tc.want)
			}
			if n := strings.Count(logs.String(), "ignoring an unreadable hold setting"); n != tc.warnings {
				t.Fatalf("warnings = %d, want %d; logs:\n%s", n, tc.warnings, logs.String())
			}
		})
	}
	if _, err := resolveHoldSettings(time.Minute, false, 1, false, "lots", true, func(string) string { return "" }, nil); err == nil {
		t.Fatal("an unreadable --hold-max-bytes flag must be refused")
	}
}

func TestHoldSettingsPolicy(t *testing.T) {
	if policy := (holdSettings{deadline: 0}).policy(nil); policy != nil {
		t.Fatalf("a zero deadline must turn holding off, got %+v", policy)
	}
	policy := holdSettings{deadline: time.Minute, maxRequests: 0, maxBytes: 0, giveUp: 2 * time.Hour}.policy(nil)
	if policy == nil || policy.Deadline != time.Minute || policy.MaxRequests != proxy.DefaultHoldMaxRequests || policy.MaxBytes != proxy.DefaultHoldMaxBytes ||
		policy.GiveUp != 2*time.Hour {
		t.Fatalf("policy = %+v, want the deadline and the give-up with default caps", policy)
	}
}

// PATCHES §67: serve says at start how long a stranded chat's holds last
// and what the variable's 0 turns off, and warns when the give-up is longer
// than the warm window, which would unlink a stranded chat first.
// SUBROUTER_WARM_WAIT, the window the owner's rework removed, is read no
// more.
func TestStrandedGiveUpStartLine(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings holdSettings
		want     []string
		warns    bool
	}{
		{"the default", holdSettings{deadline: proxy.DefaultHoldDeadline, giveUp: 60 * time.Minute},
			[]string{"stranded give-up is on", "stranded_give_up=1h0m0s", "last answered turn", "Retry-After 3600", "unlinked", "env=" + strandedGiveUpEnv}, false},
		{"no limit", holdSettings{deadline: proxy.DefaultHoldDeadline},
			[]string{"stranded give-up is off", "0 turns off only the limit", "env=" + strandedGiveUpEnv}, false},
		{"longer than the warm window", holdSettings{deadline: proxy.DefaultHoldDeadline, giveUp: 61 * time.Minute},
			[]string{"stranded give-up is on", "level=WARN", "unlinked first", "stranded_give_up=1h1m0s", "warm_idle=1h0m0s", "env=" + strandedGiveUpEnv}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			tc.settings.policy(slog.New(slog.NewTextHandler(&logs, nil)))
			for _, want := range tc.want {
				if !strings.Contains(logs.String(), want) {
					t.Fatalf("start lines lack %q:\n%s", want, logs.String())
				}
			}
			if strings.Contains(logs.String(), "warm wait") || strings.Contains(logs.String(), "level=WARN") != tc.warns {
				t.Fatalf("start lines name the warm wait, or warn = %v:\n%s", !tc.warns, logs.String())
			}
		})
	}
	got, err := resolveHoldSettings(proxy.DefaultHoldDeadline, false, proxy.DefaultHoldMaxRequests, false, "512MiB", false,
		func(name string) string { return map[string]string{"SUBROUTER_WARM_WAIT": "90s"}[name] }, nil)
	if err != nil || got.giveUp != proxy.DefaultStrandedGiveUp {
		t.Fatalf("settings %+v err %v with SUBROUTER_WARM_WAIT set; want it ignored", got, err)
	}
}
