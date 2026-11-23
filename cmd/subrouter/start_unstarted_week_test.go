package main

import (
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// PATCHES §86's kill switch reads exactly like §65's: unset is on, only an
// explicit 0/false/no/off turns it off, and an unrecognized value fails
// loudly instead of silently picking a side.
func TestStartUnstartedWeekFromEnvironment(t *testing.T) {
	const name = "SUBROUTER_START_UNSTARTED_WEEK"
	for _, c := range []struct {
		raw  []string
		want bool
	}{
		{[]string{"", "  ", "1", "true", "YES", "on"}, true},
		{[]string{"0", "false", "No", "OFF"}, false},
	} {
		for _, raw := range c.raw {
			t.Setenv(name, raw)
			if got, err := startUnstartedWeekEnabled(); err != nil || got != c.want {
				t.Fatalf("%s=%q: got %v err %v, want %v", name, raw, got, err, c.want)
			}
		}
	}
	for _, raw := range []string{"2", "maybe", "disabled"} {
		t.Setenv(name, raw)
		if _, err := startUnstartedWeekEnabled(); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s=%q accepted (err %v)", name, raw, err)
		}
	}
}

// serve sets the switch for the process before any scheduler exists, says so
// once at startup when it is off and not at all when it is on, and refuses to
// start on a value it cannot read. The fixture is §55's (serveMustReturn).
func TestServeSetsTheStartUnstartedWeek(t *testing.T) {
	prev := selectacct.StartUnstartedWeek()
	t.Cleanup(func() { selectacct.SetStartUnstartedWeek(prev) })
	clearControlSecretEnv(t)
	dir := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", dir)
	t.Setenv("SUBROUTER_CLOUD_CONFIG", dir)
	args := []string{"--addr", "127.0.0.1:0", "--fetch-usage=false", "--sr-switch-interval=0"}
	const logLine = "unstarted week priority disabled by SUBROUTER_START_UNSTARTED_WEEK"

	for _, env := range []string{"", "0"} {
		t.Setenv("SUBROUTER_START_UNSTARTED_WEEK", env)
		want := env == ""
		// The opposite of what serve should set, so a serve that never sets
		// the switch is red here.
		selectacct.SetStartUnstartedWeek(!want)
		log := captureDefaultLog(t)
		if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), "cmux.com config") {
			t.Fatalf("env %q: serve returned %v, want the deliberate cloud-config failure", env, err)
		}
		if got := selectacct.StartUnstartedWeek(); got != want {
			t.Fatalf("env %q: serve left the unstarted week priority %v, want %v", env, got, want)
		}
		wantLines := 0
		if !want {
			wantLines = 1
		}
		if got := strings.Count(log.String(), logLine); got != wantLines ||
			(!want && !strings.Contains(log.String(), "unset SUBROUTER_START_UNSTARTED_WEEK and restart")) {
			t.Fatalf("env %q: startup logged %q %d times, want %d with the re-enable hint:\n%s", env, logLine, got, wantLines, log)
		}
	}

	t.Setenv("SUBROUTER_START_UNSTARTED_WEEK", "disabled")
	if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), "SUBROUTER_START_UNSTARTED_WEEK") {
		t.Fatalf("serve with SUBROUTER_START_UNSTARTED_WEEK=disabled returned %v, want a refusal naming it", err)
	}
}

// PATCHES §90's kill switch reads exactly like §86's, and independently of
// it.
func TestSelfStartWeekFromEnvironment(t *testing.T) {
	const name = "SUBROUTER_SELF_START_WEEK"
	t.Setenv("SUBROUTER_START_UNSTARTED_WEEK", "0")
	for _, c := range []struct {
		raw  []string
		want bool
	}{
		{[]string{"", "  ", "1", "true", "YES", "on"}, true},
		{[]string{"0", "false", "No", "OFF"}, false},
	} {
		for _, raw := range c.raw {
			t.Setenv(name, raw)
			if got, err := selfStartWeekEnabled(); err != nil || got != c.want {
				t.Fatalf("%s=%q: got %v err %v, want %v", name, raw, got, err, c.want)
			}
		}
	}
	for _, raw := range []string{"2", "maybe", "disabled"} {
		t.Setenv(name, raw)
		if _, err := selfStartWeekEnabled(); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s=%q accepted (err %v)", name, raw, err)
		}
	}
}

// serve sets §90's switch and model for the process, says once at startup
// when the switch is off, and refuses to start on a value it cannot read.
func TestServeSetsTheSelfStartWeek(t *testing.T) {
	prevOn, prevModel := proxy.SelfStartWeek()
	t.Cleanup(func() { proxy.SetSelfStartWeek(prevOn, prevModel) })
	clearControlSecretEnv(t)
	dir := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", dir)
	t.Setenv("SUBROUTER_CLOUD_CONFIG", dir)
	args := []string{"--addr", "127.0.0.1:0", "--fetch-usage=false", "--sr-switch-interval=0"}
	const logLine = "self-start of unstarted weeks disabled by SUBROUTER_SELF_START_WEEK"

	for _, c := range []struct{ env, model, wantModel string }{
		{"", "", proxy.SelfStartDefaultModel},
		{"", "gpt-test-model", "gpt-test-model"},
		{"0", "", proxy.SelfStartDefaultModel},
	} {
		t.Setenv("SUBROUTER_SELF_START_WEEK", c.env)
		t.Setenv("SUBROUTER_SELF_START_MODEL", c.model)
		want := c.env == ""
		// The opposite of what serve should set, so a serve that never sets
		// it is red here.
		proxy.SetSelfStartWeek(!want, "not-the-model")
		log := captureDefaultLog(t)
		if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), "cmux.com config") {
			t.Fatalf("env %q: serve returned %v, want the deliberate cloud-config failure", c.env, err)
		}
		if on, model := proxy.SelfStartWeek(); on != want || model != c.wantModel {
			t.Fatalf("env %q model %q: serve left the self-start %v with model %q, want %v with %q", c.env, c.model, on, model, want, c.wantModel)
		}
		wantLines := 0
		if !want {
			wantLines = 1
		}
		if got := strings.Count(log.String(), logLine); got != wantLines ||
			(!want && !strings.Contains(log.String(), "unset SUBROUTER_SELF_START_WEEK and restart")) {
			t.Fatalf("env %q: startup logged %q %d times, want %d with the re-enable hint:\n%s", c.env, logLine, got, wantLines, log)
		}
	}

	t.Setenv("SUBROUTER_SELF_START_WEEK", "disabled")
	if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), "SUBROUTER_SELF_START_WEEK") {
		t.Fatalf("serve with SUBROUTER_SELF_START_WEEK=disabled returned %v, want a refusal naming it", err)
	}
}
