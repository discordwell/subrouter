package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// PATCHES §47: `sr status` and bare `sr` from a shell whose macOS login
// session has ended read this machine's rows from its daemon.

// The daemon view is off for the whole package: a test that ends its login
// session and forgets to bring a daemon gets an error, never the machine's
// real daemon at 127.0.0.1:31415.
func init() {
	fetchLocalDaemonUsage = noLocalDaemonUsage
}

func noLocalDaemonUsage(srRunner, context.Context) ([]remoteServerUsageStatus, bool, error) {
	return nil, false, errors.New("test default: no daemon view; a test that ends the session brings its own")
}

// serveLocalDaemonUsage stands in for this machine's daemon's usage-status and
// counts the reads.
func serveLocalDaemonUsage(t *testing.T, statuses []remoteServerUsageStatus, available bool, err error) *int {
	t.Helper()
	calls := new(int)
	previous := fetchLocalDaemonUsage
	fetchLocalDaemonUsage = func(srRunner, context.Context) ([]remoteServerUsageStatus, bool, error) {
		*calls++
		return statuses, available, err
	}
	t.Cleanup(func() { fetchLocalDaemonUsage = previous })
	return calls
}

const hour, week = int64(3600), int64(7 * 24 * 3600)

func claudeWindows(fiveHourUsed, weekUsed float64) []accounts.UsageWindow {
	return []accounts.UsageWindow{
		{Name: "5h", UsedPercent: fiveHourUsed, LimitWindowSeconds: 5 * hour, ResetAfterSeconds: 2 * hour},
		{Name: "7d", UsedPercent: weekUsed, LimitWindowSeconds: week, ResetAfterSeconds: 3 * 24 * hour},
	}
}

func codexWindows(weekUsed float64) []accounts.UsageWindow {
	return []accounts.UsageWindow{
		{Name: "primary", UsedPercent: 10, LimitWindowSeconds: 5 * hour, ResetAfterSeconds: hour},
		{Name: "secondary", UsedPercent: weekUsed, LimitWindowSeconds: week, ResetAfterSeconds: 2 * 24 * hour},
	}
}

func fresh() *proxy.NormalizedUsage {
	return &proxy.NormalizedUsage{Fresh: true, Usable: true, Headroom: 0.5}
}

// daemonUsageFixture is a daemon's usage-status with every case the adapter
// has to get right.
func daemonUsageFixture() []remoteServerUsageStatus {
	return []remoteServerUsageStatus{
		// Two profiles signed in to one address, as in the live capture. The
		// daemon promoted "work" to active; the registry says "work-2".
		{ID: "work", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Email: "same@example.com",
			Active: true, PlanType: "max", Windows: claudeWindows(30, 40), Normalized: fresh()},
		{ID: "work-2", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Email: "same@example.com",
			PlanType: "max", Windows: claudeWindows(20, 50), Normalized: fresh()},
		// Spent for the week: the server converter called it cooked.
		{ID: "spent", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Email: "spent@example.com",
			PlanType: "max", Windows: claudeWindows(100, 100), Normalized: fresh()},
		// The daemon's own read failed.
		{ID: "broken", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Email: "broken@example.com",
			Error: "refresh failed: invalid_grant", Normalized: &proxy.NormalizedUsage{Fresh: false}},
		// Cut by the sweep's clock: last good numbers.
		{ID: "cut", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Email: "cut@example.com",
			PlanType: "max", SweepCut: true, Windows: claudeWindows(10, 10), Normalized: &proxy.NormalizedUsage{Fresh: false}},
		// Codex, stored under an opaque owner key, labelled "main". The daemon
		// publishes the stored key as both ID and Email; an Email that differs
		// still leaves the row named by its ID, the key ~/.codex's active mark
		// names (upstream's server-row naming, from the 2026-11 merge).
		{ID: "codex-owner-abc", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Email: "c@example.com",
			Label: "main", PlanType: "pro", Windows: codexWindows(20), Normalized: fresh()},
		{ID: "d@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Email: "d@example.com",
			Active: true, PlanType: "plus", Windows: codexWindows(30), Normalized: fresh()},
		// A key the daemon checked and the provider refused; an old daemon's
		// row with no normalized view.
		{ID: "key-1", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeAPIKey, Label: "key-1",
			AuthChecked: true, AuthValid: false},
		// A Kimi subscription the daemon has a session on.
		{ID: "kimi-1", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth, Label: "kimi",
			Active: true, AssignedSessions: 1, SessionsKnown: true, PlanType: "subscription", Normalized: fresh()},
	}
}

func rowNamed(t *testing.T, rows []srUsageRow, email string) srUsageRow {
	t.Helper()
	for _, row := range rows {
		if row.email == email {
			return row
		}
	}
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.email)
	}
	t.Fatalf("no row %q among %v", email, names)
	return srUsageRow{}
}

func TestLocalRowsFromDaemonUsage(t *testing.T) {
	rows, view := localRowsFromDaemonUsage(daemonUsageFixture(), "codex-owner-abc", "work-2")
	for _, row := range rows {
		if row.email == "same@example.com" {
			t.Fatalf("a Claude row is named by its address, which two profiles share: %+v", row)
		}
	}
	cases := []struct {
		name  string
		email string
		check func(t *testing.T, row srUsageRow)
	}{
		{"a Claude row is its profile", "work", func(t *testing.T, row srUsageRow) {
			if row.claudeProfile != "work" || row.serverClaudeProfile != "" {
				t.Fatalf("claudeProfile %q serverClaudeProfile %q, want work and none", row.claudeProfile, row.serverClaudeProfile)
			}
		}},
		{"the registry's active profile, not the daemon's promotion", "work", func(t *testing.T, row srUsageRow) {
			if row.active {
				t.Fatal("the daemon's promoted profile is marked active; the registry names work-2")
			}
		}},
		{"the registry's active profile is marked", "work-2", func(t *testing.T, row srUsageRow) {
			if !row.active {
				t.Fatal("the registry's active profile is not marked active")
			}
		}},
		{"no Codex cooked vocabulary on a Claude row", "spent", func(t *testing.T, row srUsageRow) {
			if row.cooked || row.tempCooked || row.cookedReason != "" || row.tempCookedReason != "" {
				t.Fatalf("Claude row marked cooked: %+v", row)
			}
		}},
		{"a failed Claude row is repaired here, not on a server", "broken", func(t *testing.T, row srUsageRow) {
			hint := usageRowErrorHint(row)
			if hint != " (re-auth with: sr claude addr broken)" {
				t.Fatalf("hint %q, want the local re-auth naming the profile", hint)
			}
		}},
		{"a Codex row is its stored key, not its label", "codex-owner-abc", func(t *testing.T, row srUsageRow) {
			if row.displayAccount != "" {
				t.Fatalf("displayAccount %q; the daemon's label is not shown", row.displayAccount)
			}
			if !row.active {
				t.Fatal("the account ~/.codex holds is not marked active")
			}
		}},
		{"the daemon's Codex active mark is not ~/.codex", "d@example.com", func(t *testing.T, row srUsageRow) {
			if row.active {
				t.Fatal("marked active from the daemon's view, not from ~/.codex")
			}
		}},
		{"a key the provider refused", "key-1", func(t *testing.T, row srUsageRow) {
			if row.providerHealth != "bad key" {
				t.Fatalf("providerHealth %q, want bad key", row.providerHealth)
			}
		}},
		{"a Kimi subscription keeps its live sessions", "kimi-1", func(t *testing.T, row srUsageRow) {
			if !row.active {
				t.Fatal("the daemon's session-backed Kimi mark was dropped")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.check(t, rowNamed(t, rows, tc.email)) })
	}
	if got, want := strings.Join(view.stale, ","), "broken,cut"; got != want {
		t.Errorf("stale = %q, want %q", got, want)
	}
	if !view.freshnessUnknown {
		t.Error("a row with no normalized view did not make freshness unknown")
	}
	// Ranked as the local table ranks: one recommendation per provider.
	recommended := map[accounts.Provider]int{}
	for _, row := range rows {
		if row.gtoRecommended {
			recommended[usageProvider(row)]++
		}
	}
	for provider, n := range recommended {
		if n > 1 {
			t.Errorf("%d %s rows recommended", n, provider)
		}
	}
	// With nothing active on this machine, nothing is marked.
	none, _ := localRowsFromDaemonUsage(daemonUsageFixture(), "", "")
	for _, row := range none {
		if row.active && row.provider != accounts.ProviderKimi {
			t.Errorf("%s is active with nothing active here", row.email)
		}
	}
}

// statusFixtureHome is a sandboxed HOME with a Claude profile that has no
// credential at all, and two Codex accounts with b@ in ~/.codex. Read by this
// process, the Claude row is "has no access token" (or, in an ended session,
// "credential not checked"); neither may appear when the rows are the
// daemon's. Every HTTP request this process makes fails the test.
//
// Nothing is on PATH. In a session that works, fetchUsageRows reads that
// profile's Keychain item through `security`, which a test binary runs by name
// (agentclaude's securityTool): with /usr/bin on PATH, the usable-session test
// below asked the login keychain of whatever Mac ran it, password dialogs
// included.
func statusFixtureHome(t *testing.T) (srRunner, *bytes.Buffer, string) {
	t.Helper()
	home := sandboxHostConfig(t)
	t.Setenv("PATH", t.TempDir())
	if resolved, err := exec.LookPath("security"); err == nil {
		t.Fatalf("security still resolves to %s; a status test must never reach the real keychain", resolved)
	}
	t.Setenv("SR_STATUS_SERVERS", "0")
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", "http://127.0.0.1:1/v1")
	t.Setenv("SUBROUTER_CODEX_SERVER", "")
	t.Setenv("COLUMNS", "200")
	store := accounts.DefaultCodexStore()
	for _, email := range []string{"a@example.com", "b@example.com"} {
		if err := store.SaveStored(accounts.StoredCodexAccount{
			Email: email, AddedAt: time.Now().UTC().Format(time.RFC3339), Auth: testCodexAuth(email, "acct_"+email[:1]),
		}); err != nil {
			t.Fatal(err)
		}
	}
	codexHome := os.Getenv("CODEX_HOME")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	auth, err := json.Marshal(testCodexAuth("b@example.com", "acct_b"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), auth, 0o600); err != nil {
		t.Fatal(err)
	}
	claude := agentclaude.DefaultStore()
	if _, err := claude.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}
	if err := claude.SetActiveProfile("work"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runner := srRunner{
		program: "sr", store: store, out: &out, errOut: &out,
		in: readerThatFails{t},
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Errorf("this process sent %s %s; in an ended session only the daemon reads", req.Method, req.URL)
			return nil, errors.New("no network in this test")
		})},
	}
	return runner, &out, home
}

// readerThatFails is a stdin nothing may read: a prompt in an ended session
// would.
type readerThatFails struct{ t *testing.T }

func (r readerThatFails) Read([]byte) (int, error) {
	r.t.Errorf("stdin was read: something prompted")
	return 0, errors.New("no stdin in this test")
}

func daemonStatusesForFixtureHome() []remoteServerUsageStatus {
	return []remoteServerUsageStatus{
		{ID: "work", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Email: "w@example.com",
			PlanType: "max", Windows: claudeWindows(30, 40), Normalized: fresh()},
		{ID: "a@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Email: "a@example.com",
			PlanType: "pro", Windows: codexWindows(20), Normalized: fresh()},
		{ID: "b@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Email: "b@example.com",
			PlanType: "plus", Windows: codexWindows(30), SweepCut: true, Normalized: &proxy.NormalizedUsage{}},
	}
}

func TestStatusInAnEndedSessionShowsTheDaemonsRows(t *testing.T) {
	runner, out, _ := statusFixtureHome(t)
	endedLoginSession(t, endedSupersetSession())
	calls := serveLocalDaemonUsage(t, daemonStatusesForFixtureHome(), true, nil)
	if err := runner.status(t.Context(), srStatusOptions{}); err != nil {
		t.Fatalf("status: %v\n%s", err, out.String())
	}
	got := out.String()
	if *calls != 1 {
		t.Fatalf("daemon read %d times, want once", *calls)
	}
	for _, want := range []string{
		"Local (this machine), read from its daemon",
		"This shell belongs to a macOS login session that has ended",
		"Superset's terminal daemon (running since Sep 30 02:31)",
		"Restart daemon",
		"not a fresh reading", "b@example.com",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status lacks %q:\n%s", want, got)
		}
	}
	// The daemon's meters, on the rows they belong to.
	for prefix, want := range map[string]string{"work ": "60%/3d", "a@example.com ": "80%/2d", "b@example.com ": "70%/2d"} {
		found := false
		for _, line := range strings.Split(got, "\n") {
			if strings.HasPrefix(line, prefix) {
				found = strings.Contains(line, want)
				break
			}
		}
		if !found {
			t.Errorf("no %q row showing the daemon's %s:\n%s", strings.TrimSpace(prefix), want, got)
		}
	}
	for _, unwanted := range []string{"credential not checked", "has no access token", "usage unavailable", "x509", "Switch to (#)"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("status shows %q, so this process read accounts itself:\n%s", unwanted, got)
		}
	}
	if banner := strings.Index(got, "read from its daemon"); banner < 0 || banner > strings.Index(got, "Claude profiles") {
		t.Errorf("the banner is not above the table:\n%s", got)
	}
	lines := strings.Split(got, "\n")
	for i, line := range lines {
		if strings.Contains(line, "read from its daemon") {
			if i+4 < len(lines) && strings.HasPrefix(lines[i+4], "  ") && strings.TrimSpace(lines[i+4]) != "" {
				t.Errorf("the banner runs past four lines:\n%s", strings.Join(lines[i:i+5], "\n"))
			}
		}
	}

	t.Run("one account", func(t *testing.T) {
		out.Reset()
		if err := runner.run(t.Context(), []string{"a@example.com"}); err != nil {
			t.Fatalf("sr a@example.com: %v", err)
		}
		if text := out.String(); !strings.Contains(text, "(read from this machine's daemon:") || !strings.Contains(text, "a@example.com") ||
			strings.Contains(text, "b@example.com") {
			t.Fatalf("sr a@example.com in an ended session:\n%s", text)
		}
	})
}

// `sr status --json` (upstream #499) came in beside the daemon view and read
// the rows itself, through fetchUsageRows: Keychain reads and token refreshes
// over TLS from a shell where both fail. In an ended session its rows are the
// daemon's too; stdout stays one JSON document, and the banner and the
// stale-row note go to stderr.
func TestStatusJSONInAnEndedSessionShowsTheDaemonsRows(t *testing.T) {
	runner, _, _ := statusFixtureHome(t)
	var stdout, stderr bytes.Buffer
	runner.out, runner.errOut = &stdout, &stderr
	endedLoginSession(t, endedSupersetSession())
	calls := serveLocalDaemonUsage(t, daemonStatusesForFixtureHome(), true, nil)
	if err := runner.run(t.Context(), []string{"status", "--json"}); err != nil {
		t.Fatalf("status --json: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	if *calls != 1 {
		t.Fatalf("daemon read %d times, want once", *calls)
	}
	var doc srStatusJSON
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		t.Fatalf("stdout is not one status document: %v\n%s", err, stdout.String())
	}
	if decoder.More() {
		t.Fatalf("stdout carries more than one document:\n%s", stdout.String())
	}
	weekly := map[string]float64{}
	for _, provider := range doc.Providers {
		for _, account := range provider.Accounts {
			if account.State == srStatusStateError {
				t.Errorf("%s is an error row, so this process read it itself:\n%s", account.Label, stdout.String())
			}
			if account.WeeklyLeftPct != nil {
				weekly[account.Label] = *account.WeeklyLeftPct
			}
		}
	}
	// The daemon's weekly meters, on the accounts they belong to.
	for label, want := range map[string]float64{"work": 60, "a@example.com": 80, "b@example.com": 70} {
		if got, ok := weekly[label]; !ok || got != want {
			t.Errorf("%s weekly_left_pct = %v (present %v), want the daemon's %v:\n%s", label, got, ok, want, stdout.String())
		}
	}
	for _, want := range []string{"Local (this machine), read from its daemon", "not a fresh reading", "b@example.com"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr.String())
		}
	}
}

// Bare `sr` is the switch picker. In an ended session it is the table alone:
// no numbers, no recommendation, no prompt, and ~/.codex and the store as
// they were -- a switch from here would refresh nothing and check nothing.
func TestBareSRInAnEndedSessionNeverPrompts(t *testing.T) {
	runner, out, home := statusFixtureHome(t)
	endedLoginSession(t, endedSupersetSession())
	serveLocalDaemonUsage(t, daemonStatusesForFixtureHome(), true, nil)
	authPath := filepath.Join(os.Getenv("CODEX_HOME"), "auth.json")
	authBefore, err := os.Stat(authPath)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, home)
	if err := runner.run(t.Context(), nil); err != nil {
		t.Fatalf("bare sr: %v\n%s", err, out.String())
	}
	got := out.String()
	if strings.Contains(got, "Switch to (#)") || strings.Contains(got, "pick its number") {
		t.Errorf("bare sr prompted in an ended session:\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			t.Errorf("bare sr numbered its rows in an ended session: %q", line)
		}
	}
	if !strings.Contains(got, "Switching is off in this shell") || !strings.Contains(got, "read from its daemon") {
		t.Errorf("bare sr does not say switching is off, or where the rows came from:\n%s", got)
	}
	authAfter, err := os.Stat(authPath)
	if err != nil {
		t.Fatal(err)
	}
	if !authAfter.ModTime().Equal(authBefore.ModTime()) {
		t.Errorf("~/.codex/auth.json was rewritten")
	}
	// The Codex store takes its batch lock file on a read, as fetchUsageRows'
	// reads do in any session; a lock file is not a change to an account.
	if after := snapshotTree(t, home); !equalTrees(withoutLockFiles(before), withoutLockFiles(after)) {
		t.Errorf("bare sr changed HOME in an ended session:\n%s", diffTrees(before, after))
	}
}

func withoutLockFiles(tree map[string]string) map[string]string {
	kept := map[string]string{}
	for path, value := range tree {
		if !strings.HasSuffix(path, ".lock") {
			kept[path] = value
		}
	}
	return kept
}

// A daemon that does not answer, or is too old to, is one error naming both
// causes -- never a table of unknowns.
func TestStatusInAnEndedSessionWithoutTheDaemonsRows(t *testing.T) {
	for _, tc := range []struct {
		name      string
		available bool
		err       error
		says      string
	}{
		{"daemon down", false, errors.New("dial tcp 127.0.0.1:31415: connect: connection refused"), "did not answer either (dial tcp"},
		{"daemon too old", false, nil, "too old to report them"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, out, _ := statusFixtureHome(t)
			endedLoginSession(t, endedSupersetSession())
			serveLocalDaemonUsage(t, nil, tc.available, tc.err)
			for _, args := range [][]string{{"status"}, nil} {
				out.Reset()
				err := runner.run(t.Context(), args)
				if err == nil {
					t.Fatalf("sr %v with no daemon rows succeeded:\n%s", args, out.String())
				}
				for _, want := range []string{"has ended", "cannot read this machine's accounts itself", tc.says, "Restart daemon"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("sr %v: %v\nlacks %q", args, err, want)
					}
				}
				if strings.Contains(out.String(), "Claude profiles") || strings.Contains(out.String(), "Codex accounts") {
					t.Errorf("sr %v printed a table with no rows to show:\n%s", args, out.String())
				}
			}
		})
	}
}

// The daemon view through the real read: localDaemonUsageStatuses, over
// loopback HTTP with no credential, against a stand-in daemon that serves a
// usage-status with a cut row and a row with no normalized view (a daemon
// from before per-row freshness). Each footnote must reach the table.
func TestStatusInAnEndedSessionReadsTheLoopbackDaemon(t *testing.T) {
	runner, out, _ := statusFixtureHome(t)
	endedLoginSession(t, endedSupersetSession())
	previous := fetchLocalDaemonUsage
	fetchLocalDaemonUsage = srRunner.localDaemonUsageStatuses
	t.Cleanup(func() { fetchLocalDaemonUsage = previous })

	// The daemon's own wire type, marshalled as serve marshals it.
	status := func(id string, provider accounts.Provider, plan string, windows []accounts.UsageWindow) proxy.AccountUsageStatus {
		return proxy.AccountUsageStatus{
			AccountStatus: proxy.AccountStatus{ID: id, Provider: provider, AuthMode: accounts.AuthModeOAuth, Email: id},
			PlanType:      plan, Windows: windows,
		}
	}
	work := status("work", accounts.ProviderClaude, "max", claudeWindows(30, 40))
	work.Email, work.Normalized = "w@example.com", fresh()
	cut := status("a@example.com", accounts.ProviderCodex, "pro", codexWindows(20))
	cut.SweepCut, cut.Normalized = true, &proxy.NormalizedUsage{Fresh: false}
	old := status("b@example.com", accounts.ProviderCodex, "plus", codexWindows(30)) // no normalized view
	payload, err := json.Marshal([]proxy.AccountUsageStatus{work, cut, old})
	if err != nil {
		t.Fatal(err)
	}
	body := string(payload)
	var requests []string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests = append(requests, req.Method+" "+req.URL.Path+" auth="+req.Header.Get("Authorization"))
		if req.URL.Path != "/_subrouter/usage-status" {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(daemon.Close)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", daemon.URL+"/v1")
	daemonAddr := strings.TrimPrefix(daemon.URL, "http://")
	dialer := &net.Dialer{}
	runner.client = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != daemonAddr {
			t.Errorf("this process dialed %s; in an ended session only the daemon at %s is read", address, daemonAddr)
			return nil, errors.New("no network in this test")
		}
		return dialer.DialContext(ctx, network, address)
	}}}

	if err := runner.status(t.Context(), srStatusOptions{}); err != nil {
		t.Fatalf("status: %v\n%s", err, out.String())
	}
	got := out.String()
	if len(requests) != 1 || requests[0] != "GET /_subrouter/usage-status auth=" {
		t.Fatalf("the daemon saw %v; want one GET of usage-status with no credential", requests)
	}
	for prefix, want := range map[string]string{"work ": "60%/3d", "a@example.com ": "80%/2d", "b@example.com ": "70%/2d"} {
		found := false
		for _, line := range strings.Split(got, "\n") {
			if strings.HasPrefix(line, prefix) {
				found = strings.Contains(line, want)
				break
			}
		}
		if !found {
			t.Errorf("no %q row showing the daemon's %s:\n%s", strings.TrimSpace(prefix), want, got)
		}
	}
	for _, want := range []string{
		"Local (this machine), read from its daemon",
		"not a fresh reading (the daemon's latest read failed or its sweep was cut; these show its last good one, up to 15 min old): a@example.com",
		"freshness unknown: this machine's daemon predates per-row freshness",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status lacks %q:\n%s", want, got)
		}
	}
}

// In a session that works nothing changes: the table is this process's own
// reading, and the daemon view is never asked.
func TestStatusInAUsableSessionNeverReadsTheDaemonView(t *testing.T) {
	runner, out, _ := statusFixtureHome(t)
	calls := serveLocalDaemonUsage(t, daemonStatusesForFixtureHome(), true, nil)
	runner.client = &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("offline")
	})}
	if err := runner.status(t.Context(), srStatusOptions{}); err != nil {
		t.Fatalf("status: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("a usable session read the daemon view %d times", *calls)
	}
	if strings.Contains(out.String(), "read from its daemon") {
		t.Fatalf("a usable session printed the daemon view's banner:\n%s", out.String())
	}
}

// The CLI's usage-status type must take every field the daemon publishes. It
// lacked sweep_cut, so a strict decode of a full status failed and every
// reader took a cut row for a measured one. Every exported field is filled by
// reflection, so a field added to proxy.AccountUsageStatus later fails here
// until the CLI learns it.
func TestUsageStatusWireTypeHasEveryDaemonField(t *testing.T) {
	var full proxy.AccountUsageStatus
	fillEveryField(reflect.ValueOf(&full).Elem(), 0)
	body, err := json.Marshal([]proxy.AccountUsageStatus{full})
	if err != nil {
		t.Fatalf("marshal a full status: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var decoded []remoteServerUsageStatus
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("a full proxy.AccountUsageStatus does not strict-decode into remoteServerUsageStatus: %v\n%s", err, body)
	}
	if len(decoded) != 1 || !decoded[0].SweepCut {
		t.Fatalf("sweep_cut did not come through: %+v", decoded)
	}
}

// fillEveryField sets every exported field reachable from v to a non-zero
// value, one level of self-nesting deep (NormalizedUsage.Pools).
func fillEveryField(v reflect.Value, depth int) {
	if depth > 3 {
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(time.Date(2026, 9, 30, 19, 6, 27, 0, time.UTC)))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				fillEveryField(v.Field(i), depth)
			}
		}
	case reflect.Pointer:
		elem := reflect.New(v.Type().Elem())
		fillEveryField(elem.Elem(), depth+1)
		v.Set(elem)
	case reflect.Slice:
		slice := reflect.MakeSlice(v.Type(), 1, 1)
		fillEveryField(slice.Index(0), depth+1)
		v.Set(slice)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		key := reflect.New(v.Type().Key()).Elem()
		fillEveryField(key, depth+1)
		value := reflect.New(v.Type().Elem()).Elem()
		fillEveryField(value, depth+1)
		m.SetMapIndex(key, value)
		v.Set(m)
	case reflect.String:
		v.SetString(fmt.Sprintf("x%d", depth))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(0.5)
	}
}
