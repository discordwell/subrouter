package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestParseWaitDuration(t *testing.T) {
	for raw, want := range map[string]int64{"": 0, "1d": 86400, "12h": 43200, "1d12h": 129600, "90m": 5400} {
		got, err := parseWaitDuration(raw)
		if err != nil || got != want {
			t.Fatalf("parseWaitDuration(%q) = %d, %v; want %d", raw, got, err, want)
		}
	}
	for _, raw := range []string{"x", "-1h", "1dx", "d", "999999999999d"} {
		if _, err := parseWaitDuration(raw); err == nil {
			t.Fatalf("parseWaitDuration(%q) accepted", raw)
		}
	}
}

// resetSweepServer serves a dry-run preview listing two eligible accounts and
// records every redeem request.
func resetSweepServer(t *testing.T, withWaits bool) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var redeems []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("dry_run") == "true" {
			if withWaits {
				_, _ = io.WriteString(w, `{"dry_run":true,"results":[
					{"email":"a@example.com","eligible":true,"dry_run":true,"weekly_wait_seconds":400000},
					{"email":"b@example.com","eligible":true,"dry_run":true,"weekly_wait_seconds":100000}]}`)
			} else {
				_, _ = io.WriteString(w, `{"dry_run":true,"results":[{"email":"a@example.com","eligible":true,"dry_run":true}]}`)
			}
			return
		}
		mu.Lock()
		redeems = append(redeems, r.URL.RawQuery)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"reset":1,"results":[{"email":"`+q.Get("email")+`","eligible":true,"reset":true}]}`)
	}))
	t.Cleanup(server.Close)
	return server, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), redeems...) }
}

func TestResetAllRefusesWithoutConfirmation(t *testing.T) {
	server, redeems := resetSweepServer(t, true)
	var out bytes.Buffer
	runner := srRunner{out: &out, errOut: &out, client: server.Client()}
	err := runner.resetAgainstServer(t.Context(), []string{"--all"}, &srServerConfig{Name: "test", URL: server.URL})
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err = %v, want a request for --yes", err)
	}
	if got := redeems(); len(got) != 0 {
		t.Fatalf("redeemed without confirmation: %v", got)
	}
}

func TestResetAllRedeemsExactlyThePreviewedAccounts(t *testing.T) {
	server, redeems := resetSweepServer(t, true)
	var out bytes.Buffer
	runner := srRunner{out: &out, errOut: &out, in: strings.NewReader("y\n"), client: server.Client()}
	if err := runner.resetAgainstServer(t.Context(), []string{"--all", "--min-wait", "1d"}, &srServerConfig{Name: "test", URL: server.URL}); err != nil {
		t.Fatalf("%v (%s)", err, out.String())
	}
	got := redeems()
	if len(got) != 2 || !strings.Contains(got[0], "email=a%40example.com") || !strings.Contains(got[1], "email=b%40example.com") {
		t.Fatalf("redeem requests = %v, want one per previewed account", got)
	}
	for _, q := range got {
		if strings.Contains(q, "all=") {
			t.Fatalf("redeem request %q re-sent all=true", q)
		}
	}
	if !strings.Contains(out.String(), "waits ") {
		t.Fatalf("preview did not show waits: %s", out.String())
	}
}

func TestResetAllMinWaitRefusesOlderServer(t *testing.T) {
	server, redeems := resetSweepServer(t, false)
	var out bytes.Buffer
	runner := srRunner{out: &out, errOut: &out, client: server.Client()}
	err := runner.resetAgainstServer(t.Context(), []string{"--all", "--yes", "--min-wait", "1d"}, &srServerConfig{Name: "test", URL: server.URL})
	if err == nil || !strings.Contains(err.Error(), "--min-wait") {
		t.Fatalf("err = %v, want an older-server --min-wait refusal", err)
	}
	if got := redeems(); len(got) != 0 {
		t.Fatalf("redeemed on an older server: %v", got)
	}
}

func TestResetCandidatesListsWithoutRedeeming(t *testing.T) {
	server, redeems := resetSweepServer(t, true)
	var out bytes.Buffer
	runner := srRunner{out: &out, errOut: &out, client: server.Client()}
	if err := runner.resetAgainstServer(t.Context(), []string{"--candidates"}, &srServerConfig{Name: "test", URL: server.URL}); err != nil {
		t.Fatal(err)
	}
	if len(redeems()) != 0 || !strings.Contains(out.String(), "2 account(s) eligible") {
		t.Fatalf("redeems = %v, output = %s", redeems(), out.String())
	}
}

// Each conflicting flag set is refused before anything reaches the server,
// and by its own check. The server records every request: an old version of
// this test aimed at a closed port and asserted only err != nil, so connection
// refused satisfied it and deleting any one of these checks left it green --
// while on a real server `sr reset a@x --min-wait 1d` silently dropped
// --min-wait and redeemed the credit anyway. Asserting the refusal text pins
// the specific check; asserting zero requests pins "before any request".
func TestResetFlagConflicts(t *testing.T) {
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.String())
		mu.Unlock()
		_, _ = io.WriteString(w, `{"reset":1,"results":[{"email":"a@example.com","eligible":true,"reset":true}]}`)
	}))
	t.Cleanup(server.Close)
	fixed := &srServerConfig{Name: "test", URL: server.URL}

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--local"}, "--local cannot be used when this command is bound to a running server"},
		{[]string{"a@example.com", "--min-wait", "1d"}, "--min-wait applies to the default pick, --all, and --candidates"},
		{[]string{"--gto", "--min-wait", "1d"}, "--min-wait applies to the default pick, --all, and --candidates"},
		{[]string{"--candidates", "--all"}, "--candidates lists accounts itself"},
		{[]string{"--candidates", "a@example.com"}, "--candidates lists accounts itself"},
		{[]string{"--min-wait", "soon"}, "--min-wait: "},
	} {
		var out bytes.Buffer
		runner := srRunner{out: &out, errOut: &out, client: server.Client()}
		err := runner.resetAgainstServer(t.Context(), tc.args, fixed)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: error = %v, want the refusal %q", tc.args, err, tc.want)
		}
		mu.Lock()
		got := append([]string(nil), requests...)
		requests = nil
		mu.Unlock()
		if len(got) != 0 {
			t.Errorf("%v: reached the server before refusing: %v", tc.args, got)
		}
	}

	// Calibration: the same server does receive a request when nothing
	// conflicts, so "zero requests" above is a real observation.
	var out bytes.Buffer
	runner := srRunner{out: &out, errOut: &out, client: server.Client()}
	_ = runner.resetAgainstServer(t.Context(), []string{"a@example.com"}, fixed)
	mu.Lock()
	defer mu.Unlock()
	if len(requests) == 0 {
		t.Fatalf("calibration: a plain `reset a@example.com` never reached the recording server; output:\n%s", out.String())
	}
}

// bestIgnoringMinWaitServer is a server that understands best=true but not
// min_wait_seconds: it always offers the same account with a short wait.
func bestIgnoringMinWaitServer(t *testing.T, reportWait bool) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var redeems []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("dry_run") == "true" {
			wait := ""
			if reportWait {
				wait = `,"weekly_wait_seconds":3600`
			}
			_, _ = io.WriteString(w, `{"dry_run":true,"results":[{"email":"soon@example.com","eligible":true,"dry_run":true`+wait+`}]}`)
			return
		}
		mu.Lock()
		redeems = append(redeems, r.URL.RawQuery)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"reset":1,"results":[{"email":"soon@example.com","eligible":true,"reset":true}]}`)
	}))
	t.Cleanup(server.Close)
	return server, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), redeems...) }
}

func TestResetDefaultPickMinWaitNeverSpendsOnShortWait(t *testing.T) {
	for _, reportWait := range []bool{true, false} {
		server, redeems := bestIgnoringMinWaitServer(t, reportWait)
		var out bytes.Buffer
		runner := srRunner{out: &out, errOut: &out, client: server.Client()}
		err := runner.resetAgainstServer(t.Context(), []string{"--min-wait", "1d"}, &srServerConfig{Name: "test", URL: server.URL})
		if err == nil {
			t.Fatalf("reportWait=%t: expected an error, output %s", reportWait, out.String())
		}
		if got := redeems(); len(got) != 0 {
			t.Fatalf("reportWait=%t: redeemed %v despite --min-wait 1d", reportWait, got)
		}
	}
}

func TestResetAllNonTerminalStdinNeedsYes(t *testing.T) {
	server, redeems := resetSweepServer(t, true)
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	var out bytes.Buffer
	runner := srRunner{out: &out, errOut: &out, in: devNull, client: server.Client()}
	err = runner.resetAgainstServer(t.Context(), []string{"--all"}, &srServerConfig{Name: "test", URL: server.URL})
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err = %v, want a request for --yes", err)
	}
	if got := redeems(); len(got) != 0 {
		t.Fatalf("redeemed without confirmation: %v", got)
	}
}

// On a machine with no server, `sr reset --all` redeems directly against wham
// with the local tokens. It used to spend on every cooked account the moment
// it was typed; from a non-terminal (a script, cron, a pipe) it must now
// refuse until --yes, and redeem nothing.
func TestResetLocalAllNeedsConfirmation(t *testing.T) {
	wham := &localResetWham{rateLimit: map[string]any{
		"allowed":          false,
		"limit_reached":    true,
		"primary_window":   map[string]any{"used_percent": 20, "limit_window_seconds": 18000, "reset_after_seconds": 3600},
		"secondary_window": map[string]any{"used_percent": 100, "limit_window_seconds": 604800, "reset_after_seconds": 400000},
	}}
	runner, out := localResetRunner(t, "cooked@example.com", wham)
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	runner.in = devNull
	err = runner.run(t.Context(), []string{"reset", "--all"})
	if got := wham.consumes(); got != 0 {
		t.Fatalf("`sr reset --all` spent %d credit(s) without confirmation\n%s", got, out.String())
	}
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err = %v, want a request for --yes\n%s", err, out.String())
	}

	if err := runner.run(t.Context(), []string{"reset", "--all", "--yes"}); err != nil {
		t.Fatalf("reset --all --yes: %v\n%s", err, out.String())
	}
	if got := wham.consumes(); got != 1 {
		t.Fatalf("--all --yes consumed %d credit(s), want 1\n%s", got, out.String())
	}
}
