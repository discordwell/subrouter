package claude

import (
	"encoding/json"
	"testing"
	"time"
)

// The row's resets list every grant the account holds, one entry per kind
// and expiry (PATCHES §91.1): a week reset ending on one date and two Fable
// resets ending later are two entries, the week one first, whatever order
// Anthropic lists them in. A grant spent or past its date is not held. A
// block that holds none lists none; a block not read lists nothing at all.
func TestResetInfoListsEveryResetByKindAndExpiry(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	read := func(t *testing.T, grants ...map[string]any) []byte {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"eligible": true, "grants": grants})
		status, err := ParseResetStatus(raw)
		if err != nil || status == nil {
			t.Fatalf("calibration: %s decoded as %+v, %v", raw, status, err)
		}
		info := ResetInfo(status, now)
		if info.Held == nil {
			t.Fatalf("a known block listed nil resets, which reads as not read")
		}
		listed, _ := json.Marshal(info.Held)
		return listed
	}
	grant := func(id string, clears []string, left int, ends string) map[string]any {
		return map[string]any{"id": id, "resets_total": max(left, 1), "resets_left": left, "ends_at": ends,
			"clears": clears, "use_requires_limit": true}
	}
	fable := grant("fable_launch", []string{"seven_day_overage_included"}, 2, "2027-03-09T07:59:00Z")
	week := grant("week_reset", []string{"seven_day", "seven_day_overage_included"}, 1, "2026-12-10T22:00:00Z")
	want := `[` +
		`{"kind":"week","count":1,"expires_at":"2026-12-10T22:00:00Z","expiry":"read","gives_back":["seven_day","seven_day_overage_included"],"restarts_clock":"measured_no","needs_limit":true,"order":1},` +
		`{"kind":"fable","count":2,"expires_at":"2027-03-09T07:59:00Z","expiry":"read","gives_back":["seven_day_overage_included"],"restarts_clock":"measured_no","needs_limit":true,"order":2}` +
		`]`
	if got := string(read(t, fable, week)); got != want {
		t.Fatalf("a week reset and two Fable resets\n got %s\nwant %s", got, want)
	}
	spent := grant("week_reset", []string{"seven_day"}, 0, "2026-12-10T22:00:00Z")
	lapsed := grant("october", []string{"five_hour", "seven_day", "seven_day_overage_included"}, 1, "2026-10-22T16:00:00Z")
	want = `[{"kind":"fable","count":2,"expires_at":"2027-03-09T07:59:00Z","expiry":"read","gives_back":["seven_day_overage_included"],"restarts_clock":"measured_no","needs_limit":true,"order":1}]`
	if got := string(read(t, spent, lapsed, fable)); got != want {
		t.Fatalf("a spent and a lapsed grant beside Fable resets\n got %s\nwant %s", got, want)
	}
	early := map[string]any{"id": "early", "resets_left": 1, "clears": []string{"seven_day"}, "use_requires_limit": false}
	want = `[{"kind":"week","count":1,"expiry":"none_given","gives_back":["seven_day"],"restarts_clock":"measured_no","needs_limit":false,"order":1}]`
	if got := string(read(t, early)); got != want {
		t.Fatalf("a grant with no end that can be used early\n got %s\nwant %s", got, want)
	}
	if got := string(read(t)); got != `[]` {
		t.Fatalf("a block with no grants listed %s, want []", got)
	}
	// A grant of a type Claude Code does not read is listed with that type
	// as Anthropic wrote it, though the grant's Type stays as Claude Code
	// reads it.
	cowork := map[string]any{"id": "cowork_reset", "resets_left": 1, "type": "cowork_boost", "ends_at": "2026-12-20T00:00:00Z",
		"clears": []string{"seven_day_cowork"}, "use_requires_limit": true}
	want = `[{"kind":"unknown","raw_type":"cowork_boost","count":1,"expires_at":"2026-12-20T00:00:00Z","expiry":"read","gives_back":["seven_day_cowork"],"restarts_clock":"unknown","needs_limit":true,"manual_only":true}]`
	if got := string(read(t, cowork)); got != want {
		t.Fatalf("a grant of a new type\n got %s\nwant %s", got, want)
	}
	raw, _ := json.Marshal(map[string]any{"eligible": true, "grants": []any{cowork}})
	if status, _ := ParseResetStatus(raw); status.Grants[0].Type != "" {
		t.Fatalf("the grant's Type is %q, want it empty as Claude Code reads an unknown type", status.Grants[0].Type)
	}
	unread, _ := ParseResetStatus(json.RawMessage(`{"eligible":false,"ineligible_reason":"unavailable","grants":[]}`))
	if info := ResetInfo(unread, now); info == nil || info.Held != nil {
		t.Fatalf("Anthropic's failed read listed %#v, want no list", info)
	}
}
