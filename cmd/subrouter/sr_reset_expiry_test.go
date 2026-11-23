package main

import (
	"encoding/json"
	"testing"
	"time"
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
