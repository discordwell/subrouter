package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
)

// PATCHES §91.2: on a row read from a daemon, sr status's Codex 1x reset cell
// shows the soonest expiry the daemon read from the account's credit list
// beside the count, "3 left 12/14". A count whose dates were not read, and a
// daemon from before §91, read "3 left" as before, and a Claude cell stays as
// §64 left it.
func TestTheCodexResetCellShowsTheSoonestReadExpiry(t *testing.T) {
	soon := time.Date(2026, 12, 14, 9, 13, 0, 0, time.UTC)
	later := soon.Add(7 * 24 * time.Hour)
	weekEnds := time.Date(2026, 12, 10, 22, 0, 0, 0, time.UTC)
	credit := func(expiry string, expires time.Time) string {
		entry := `{"kind":"codex_rate_limits","count":1,"expiry":"` + expiry + `","restarts_clock":"assumed_yes","order":1`
		if !expires.IsZero() {
			entry += `,"expires_at":"` + expires.Format(time.RFC3339) + `"`
		}
		return entry + `}`
	}
	codex := func(id, resets string) string {
		row := `{"id":"` + id + `","provider":"codex","auth_mode":"oauth","source":"peer","auth_checked":true,"auth_valid":true,` +
			`"complimentary_reset":{"known":true,"available":true,"remaining":3,"source":"rate_limit_reset_credits"}`
		if resets != "" {
			row += `,"resets":` + resets
		}
		return row + `}`
	}
	claude := `{"id":"claude-both","provider":"claude","auth_mode":"oauth","source":"peer","auth_checked":true,"auth_valid":true,` +
		`"complimentary_reset":{"known":true,"available":true,"eligible":true,"remaining":2,"source":"cedar_ember",` +
		`"expires_at":"` + weekEnds.Format(time.RFC3339) + `","remaining_by_kind":{"fable":1,"week":1},"week_held_until":"` + weekEnds.Format(time.RFC3339) + `"},` +
		`"resets":[{"kind":"week","count":1,"expires_at":"` + weekEnds.Format(time.RFC3339) + `","expiry":"read","restarts_clock":"assumed_no","order":1},` +
		`{"kind":"fable","count":1,"expires_at":"2027-03-09T07:59:00Z","expiry":"read","restarts_clock":"assumed_no","order":2}]}`
	body := `[` + codex("read", `[`+credit("read", later)+`,`+credit("read", soon)+`,`+credit("not_read", time.Time{})+`]`) + `,` +
		codex("last-good", `[`+credit("last_good", soon)+`,`+credit("not_read", time.Time{})+`,`+credit("not_read", time.Time{})+`]`) + `,` +
		codex("not-read", `[{"kind":"codex_rate_limits","count":3,"expiry":"not_read","restarts_clock":"assumed_yes","order":1}]`) + `,` +
		codex("before-91", "") + `,` + claude + `]`
	var statuses []remoteServerUsageStatus
	if err := json.Unmarshal([]byte(body), &statuses); err != nil {
		t.Fatal(err)
	}
	dated := "3 left " + soon.Local().Format("1/2")
	want := map[string]string{
		"read":        dated,
		"last-good":   dated,
		"not-read":    "3 left",
		"before-91":   "3 left",
		"claude-both": "1 +1F " + weekEnds.Local().Format("1/2"),
	}
	rows := usageRowsFromServerUsageStatuses(statuses)
	if len(rows) != len(want) {
		t.Fatalf("calibration: %d rows from %d statuses", len(rows), len(statuses))
	}
	for _, row := range rows {
		if got := usageGridResetCell(row).Text; got != want[row.email] {
			t.Errorf("%s: 1x reset cell %q, want %q", row.email, got, want[row.email])
		}
	}
}

// PATCHES §91.2: a Codex row that sr status reads itself, as it does in a
// working login session, shows the soonest expiry this machine's daemon read
// beside its own count, from one loopback read of the daemon's usage-status.
// A daemon that counted another number of credits dates none of them, and a
// row holding no credit is not filled.
func TestALocalCodexRowTakesItsCreditDatesFromThisMachinesDaemon(t *testing.T) {
	sandboxHostConfig(t)
	counts := map[string]int{"dated@example.com": 3, "moved@example.com": 2, "spent@example.com": 0}
	store := accounts.DefaultCodexStore()
	byAuth := map[string]string{}
	for email := range counts {
		auth := codexAuthValidFor(email, "acct_"+strings.Split(email, "@")[0], 30*24*time.Hour)
		if err := store.SaveStored(accounts.StoredCodexAccount{Email: email, AddedAt: time.Now().UTC().Format(time.RFC3339), Auth: auth}); err != nil {
			t.Fatal(err)
		}
		byAuth["Bearer "+auth.Tokens.AccessToken] = email
	}
	runner := srRunner{program: "sr", store: store, out: io.Discard, errOut: io.Discard,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			email, known := byAuth[req.Header.Get("Authorization")]
			status, body := http.StatusOK, fmt.Sprintf(`{"plan_type":"pro","rate_limit":{"limit_reached":false,"secondary_window":`+
				`{"used_percent":40,"limit_window_seconds":604800,"reset_after_seconds":300000}},"rate_limit_reset_credits":{"available_count":%d}}`, counts[email])
			if req.URL.Path != "/backend-api/wham/usage" || !known {
				t.Errorf("sr sent %s %s; only a usage read of a stored account is answered", req.Method, req.URL.Path)
				status, body = http.StatusNotFound, `{}`
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		})}}
	soon := time.Date(2026, 12, 14, 9, 13, 0, 0, time.UTC)
	credit := func(expires time.Time) accounts.HeldReset {
		return accounts.HeldReset{Kind: accounts.ResetKindCodexRateLimits, Count: 1, ExpiresAt: expires.Format(time.RFC3339),
			Expiry: accounts.ResetExpiryRead, RestartsClock: accounts.ClockRestartAssumedYes}
	}
	daemonRow := func(id string, count int, resets ...accounts.HeldReset) remoteServerUsageStatus {
		return remoteServerUsageStatus{ID: id, Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Resets: resets,
			ComplimentaryReset: &accounts.ComplimentaryResetInfo{Known: true, Available: true, Remaining: &count, Source: accounts.CodexResetCountSource}}
	}
	three := []accounts.HeldReset{credit(soon.Add(7 * 24 * time.Hour)), credit(soon), credit(soon.Add(24 * time.Hour))}
	daemonRows := []remoteServerUsageStatus{
		daemonRow("dated@example.com", 3, three...),
		// The daemon has not yet seen the credit this row's own read finds spent.
		daemonRow("moved@example.com", 3, three...),
		daemonRow("spent@example.com", 1, credit(soon)),
	}
	calls := serveLocalDaemonResets(t, daemonRows)

	rows, err := runner.fetchUsageRows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"dated@example.com": "3 left " + soon.Local().Format("1/2"), "moved@example.com": "2 left", "spent@example.com": "used"}
	if len(rows) != len(want) {
		t.Fatalf("calibration: %d rows, want the %d stored accounts", len(rows), len(want))
	}
	for _, row := range rows {
		if got := usageGridResetCell(row).Text; got != want[row.email] {
			t.Errorf("%s: 1x reset cell %q, want %q", row.email, got, want[row.email])
		}
	}
	if *calls != 1 {
		t.Fatalf("this machine's daemon was read %d times, want once for every row", *calls)
	}

	// The real read asks the daemon for its last sweep as it stands, so no
	// sweep, and no provider request, starts for it. It needs a transport it
	// can pin to loopback, which the fake above is not, so it fills rows as
	// fetchUsageRows left them.
	var mu sync.Mutex
	var queries []string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		queries = append(queries, req.URL.Path+"?"+req.URL.RawQuery)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(daemonRows)
	}))
	t.Cleanup(daemon.Close)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", daemon.URL+"/v1")
	fetchLocalDaemonResets = srRunner.localDaemonResetRows
	for i := range rows {
		rows[i].resets = nil
	}
	fillResetsFromLocalDaemon(context.Background(), srRunner{}, rows)
	for _, row := range rows {
		if got := usageGridResetCell(row).Text; got != want[row.email] {
			t.Errorf("through the loopback read, %s: 1x reset cell %q, want %q", row.email, got, want[row.email])
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(queries) != 1 || queries[0] != "/_subrouter/usage-status?snapshot=1" {
		t.Fatalf("the daemon was asked %q, want one read of its snapshot", queries)
	}
}
