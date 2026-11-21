package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// Sticky DNS and the outage spill (PATCHES §59) and the peer-down grace
// (PATCHES §67): variable > default, "0" is off, and a variable that does not
// parse keeps the default with a warning instead of stopping the daemon.
// SUBROUTER_PEER_HOLD_WAIT is ignored.
func TestResolveOutageSettings(t *testing.T) {
	defaults := outageSettings{stickyDNSMaxAge: proxy.DefaultStickyDNSMaxAge, spillRate: proxy.DefaultOutageSpillRate, spillBurst: proxy.DefaultOutageSpillBurst, peerDownGrace: 15 * time.Minute}
	for _, tc := range []struct {
		name     string
		env      map[string]string
		want     outageSettings
		warnings int
	}{
		{name: "defaults", want: defaults},
		{
			name: "variables",
			env:  map[string]string{stickyDNSMaxAgeEnv: "24h", outageSpillRateEnv: "2.5", outageSpillBurstEnv: "4", peerDownGraceEnv: "40m"},
			want: outageSettings{stickyDNSMaxAge: 24 * time.Hour, spillRate: 2.5, spillBurst: 4, peerDownGrace: 40 * time.Minute},
		},
		{
			name: "zero turns each off",
			env:  map[string]string{stickyDNSMaxAgeEnv: "0", outageSpillRateEnv: "0", peerDownGraceEnv: "0"},
			want: outageSettings{stickyDNSMaxAge: 0, spillRate: 0, spillBurst: proxy.DefaultOutageSpillBurst, peerDownGrace: 0},
		},
		{
			name:     "a rate that is not a finite number keeps the default",
			env:      map[string]string{outageSpillRateEnv: "NaN"},
			want:     defaults,
			warnings: 1,
		},
		{
			name:     "an infinite rate keeps the default",
			env:      map[string]string{outageSpillRateEnv: "+Inf"},
			want:     defaults,
			warnings: 1,
		},
		{
			name:     "an unreadable variable keeps the default",
			env:      map[string]string{stickyDNSMaxAgeEnv: "a week", outageSpillRateEnv: "five", outageSpillBurstEnv: "ten", peerDownGraceEnv: "fifteen minutes"},
			want:     defaults,
			warnings: 4,
		},
		{
			// A negative grace would read as off with no word said.
			name:     "a negative grace keeps the default",
			env:      map[string]string{peerDownGraceEnv: "-15m"},
			want:     defaults,
			warnings: 1,
		},
		{
			name: "the peer hold wait is not read",
			env:  map[string]string{"SUBROUTER_PEER_HOLD_WAIT": "90s"},
			want: defaults,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			got := resolveOutageSettings(func(name string) string { return tc.env[name] }, slog.New(slog.NewTextHandler(&logs, nil)))
			if got != tc.want {
				t.Fatalf("settings = %+v, want %+v", got, tc.want)
			}
			if n := strings.Count(logs.String(), "ignoring an unreadable outage setting"); n != tc.warnings {
				t.Fatalf("%d warnings, want %d:\n%s", n, tc.warnings, logs.String())
			}
		})
	}
}

// apply says at start which way each part runs, and a zero rate is no policy.
func TestOutageSettingsApply(t *testing.T) {
	t.Cleanup(func() { proxy.SetStickyDNSMaxAge(proxy.DefaultStickyDNSMaxAge) })
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	spill := outageSettings{stickyDNSMaxAge: time.Hour, spillRate: 3, spillBurst: 6}.apply(logger)
	if spill == nil || spill.Rate != 3 || spill.Burst != 6 {
		t.Fatalf("policy %+v, want rate 3 burst 6", spill)
	}
	for _, want := range []string{"sticky dns is on", "max_age=1h0m0s", "outage spill is on", "rate=3", "burst=6"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("start lines lack %q:\n%s", want, logs.String())
		}
	}
	logs.Reset()
	if spill := (outageSettings{spillRate: 0, spillBurst: 6}).apply(logger); spill != nil {
		t.Fatalf("a zero rate gave policy %+v, want none", spill)
	}
	if !strings.Contains(logs.String(), "sticky dns is off") || !strings.Contains(logs.String(), "outage spill is off") {
		t.Fatalf("start lines:\n%s", logs.String())
	}
}

// PATCHES §67: the peer-down grace says at start which way it runs, and warns
// when it is as long as a peer hold's idle bound, because the holds then lapse
// before the peer can be found down.
func TestPeerDownGraceStartLine(t *testing.T) {
	t.Cleanup(func() { proxy.SetStickyDNSMaxAge(proxy.DefaultStickyDNSMaxAge) })
	for _, tc := range []struct {
		grace time.Duration
		want  []string
		warn  bool
	}{
		{15 * time.Minute, []string{"peer down grace is on", "grace=15m0s", "env=" + peerDownGraceEnv}, false},
		{proxy.PeerHoldIdleTTL - time.Minute, []string{"peer down grace is on", "grace=" + (proxy.PeerHoldIdleTTL - time.Minute).String()}, false},
		{proxy.PeerHoldIdleTTL, []string{"peer down grace is on", "grace=" + proxy.PeerHoldIdleTTL.String(), "level=WARN", "hold_idle=" + proxy.PeerHoldIdleTTL.String()}, true},
		{0, []string{"peer down grace is off", "gateway attribution", "env=" + peerDownGraceEnv}, false},
	} {
		var logs bytes.Buffer
		outageSettings{peerDownGrace: tc.grace}.apply(slog.New(slog.NewTextHandler(&logs, nil)))
		for _, want := range tc.want {
			if !strings.Contains(logs.String(), want) {
				t.Fatalf("grace %v: start lines lack %q:\n%s", tc.grace, want, logs.String())
			}
		}
		if warned := strings.Contains(logs.String(), "level=WARN"); warned != tc.warn {
			t.Fatalf("grace %v: warned=%v, want %v:\n%s", tc.grace, warned, tc.warn, logs.String())
		}
	}
}

// PATCHES §67: no start line names the peer hold wait.
func TestNoStartLineNamesThePeerHoldWait(t *testing.T) {
	t.Cleanup(func() { proxy.SetStickyDNSMaxAge(proxy.DefaultStickyDNSMaxAge) })
	var logs bytes.Buffer
	outageSettings{peerDownGrace: proxy.DefaultPeerDownGrace}.apply(slog.New(slog.NewTextHandler(&logs, nil)))
	if strings.Contains(logs.String(), "peer hold wait") || strings.Contains(logs.String(), "SUBROUTER_PEER_HOLD_WAIT") {
		t.Fatalf("start lines name the peer hold wait:\n%s", logs.String())
	}
}
