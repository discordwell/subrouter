package main

import (
	"strings"
	"testing"
)

func TestPacedAllowanceFromEnvironment(t *testing.T) {
	const name = "SUBROUTER_PACED_ALLOWANCE"
	for _, c := range []struct {
		raw  []string
		want bool
	}{
		{[]string{"", "  ", "1", "true", "YES", "on"}, true},
		{[]string{"0", "false", "No", "OFF"}, false},
	} {
		for _, raw := range c.raw {
			t.Setenv(name, raw)
			if got, err := pacedAllowanceEnabled(); err != nil || got != c.want {
				t.Fatalf("%s=%q: got %v err %v, want %v", name, raw, got, err, c.want)
			}
		}
	}
	for _, raw := range []string{"2", "maybe", "disabled"} {
		t.Setenv(name, raw)
		if _, err := pacedAllowanceEnabled(); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s=%q accepted (err %v)", name, raw, err)
		}
	}
}

func TestPaceWasteThresholdFromEnvironment(t *testing.T) {
	const name = "SUBROUTER_PACE_WASTE_THRESHOLD"
	for raw, want := range map[string]float64{"": 0.05, " ": 0.05, "5": 0.05, "0": 0, "2.5": 0.025, " 10 ": 0.10, "100": 1} {
		t.Setenv(name, raw)
		if got, err := paceWasteThreshold(); err != nil || got < want-1e-12 || got > want+1e-12 {
			t.Fatalf("%s=%q: got %v err %v, want %v", name, raw, got, err, want)
		}
	}
	for _, raw := range []string{"-1", "101", "five", "5%", "NaN", "Inf"} {
		t.Setenv(name, raw)
		if _, err := paceWasteThreshold(); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s=%q accepted (err %v)", name, raw, err)
		}
	}
}

func TestPacedSessionCeilingFromEnvironment(t *testing.T) {
	const name = "SUBROUTER_PACED_SESSION_CEILING"
	for raw, want := range map[string]int{"": 20, " ": 20, "6": 6, "1": 1, "10": 10, " 64 ": 64} {
		t.Setenv(name, raw)
		if got, err := pacedSessionCeiling(); err != nil || got != want {
			t.Fatalf("%s=%q: got %d err %v, want %d", name, raw, got, err, want)
		}
	}
	for _, raw := range []string{"0", "65", "-1", "six", "6.5", "1e1"} {
		t.Setenv(name, raw)
		if _, err := pacedSessionCeiling(); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s=%q accepted (err %v)", name, raw, err)
		}
	}
}

func TestEarlyDrainFromEnvironment(t *testing.T) {
	const name = "SUBROUTER_EARLY_DRAIN"
	for raw, want := range map[string]bool{"": true, " ": true, "1": true, " 1 ": true, "0": false} {
		t.Setenv(name, raw)
		if got, err := earlyDrainEnabled(); err != nil || got != want {
			t.Fatalf("%s=%q: got %v err %v, want %v", name, raw, got, err, want)
		}
	}
	for _, raw := range []string{"2", "true", "off", "yes"} {
		t.Setenv(name, raw)
		if _, err := earlyDrainEnabled(); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s=%q accepted (err %v)", name, raw, err)
		}
	}
}

func TestEarlyDrainHoursFromEnvironment(t *testing.T) {
	const name = "SUBROUTER_EARLY_DRAIN_HOURS"
	for raw, want := range map[string]int{"": 72, " ": 72, "72": 72, "25": 25, " 48 ": 48, "168": 168} {
		t.Setenv(name, raw)
		if got, err := earlyDrainHours(); err != nil || got != want {
			t.Fatalf("%s=%q: got %d err %v, want %d", name, raw, got, err, want)
		}
	}
	for _, raw := range []string{"24", "0", "-1", "169", "48.5", "two days"} {
		t.Setenv(name, raw)
		if _, err := earlyDrainHours(); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s=%q accepted (err %v)", name, raw, err)
		}
	}
}
