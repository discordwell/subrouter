package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
)

// PATCHES §102: sr status's 1x reset cell of a row whose saved resets its
// daemon knows from the account's last landed claim, not from a reading, ends
// in "^", dated or not, for Claude and Codex, and for a Claude row this
// process read itself and completed from this machine's daemon. A row with no
// mark, a daemon from before §102 among them, reads as before.
func TestTheResetCellMarksResetsKnownFromAClaim(t *testing.T) {
	fableEnds := time.Date(2027, 3, 9, 7, 59, 0, 0, time.UTC)
	claude := func(id string, fromClaim bool) string {
		row := `{"id":"` + id + `","provider":"claude","auth_mode":"oauth","source":"peer","auth_checked":true,"auth_valid":true,` +
			`"complimentary_reset":{"known":true,"available":true,"eligible":true,"remaining":1,"source":"cedar_ember",` +
			`"expires_at":"` + fableEnds.Format(time.RFC3339) + `","remaining_by_kind":{"fable":1}}`
		if fromClaim {
			row += `,"resets_from_claim":true,"not_read_since_claim":["7d"]`
		}
		return row + `}`
	}
	codex := func(id string, remaining string, fromClaim bool) string {
		row := `{"id":"` + id + `","provider":"codex","auth_mode":"oauth","source":"peer","auth_checked":true,"auth_valid":true,` +
			`"complimentary_reset":{"known":true,"available":true,"remaining":` + remaining + `,"source":"rate_limit_reset_credits"}`
		if fromClaim {
			row += `,"resets_from_claim":true`
		}
		return row + `}`
	}
	body := `[` + claude("claude-claimed", true) + `,` + claude("claude-read", false) + `,` +
		codex("codex-claimed", "1", true) + `,` + codex("codex-last", "0", true) + `,` + codex("codex-read", "1", false) + `]`
	var statuses []remoteServerUsageStatus
	if err := json.Unmarshal([]byte(body), &statuses); err != nil {
		t.Fatal(err)
	}
	fable := "1 Fable " + fableEnds.Local().Format("1/2")
	for _, c := range []struct{ id, dated, undated string }{
		{"claude-claimed", fable + "^", "1 Fable^"},
		{"claude-read", fable, "1 Fable"},
		{"codex-claimed", "1 left^", "1 left^"},
		{"codex-last", "used^", "used^"},
		{"codex-read", "1 left", "1 left"},
	} {
		var row *srUsageRow
		rows := usageRowsFromServerUsageStatuses(statuses)
		for i := range rows {
			if rows[i].email == c.id {
				row = &rows[i]
			}
		}
		if row == nil {
			t.Fatalf("calibration: no row for %s", c.id)
		}
		if got := usageGridResetCell(*row).Text; got != c.dated {
			t.Errorf("%s: dated cell %q, want %q", c.id, got, c.dated)
		}
		if got := usageGridResetCellUndated(*row).Text; got != c.undated {
			t.Errorf("%s: undated cell %q, want %q", c.id, got, c.undated)
		}
	}

	// A Claude row this process read itself, whose read carried no block,
	// takes the daemon's block and its mark.
	serveLocalDaemonResets(t, statuses[:1])
	local := []srUsageRow{{email: "claude-claimed", provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth}}
	fillResetsFromLocalDaemon(context.Background(), srRunner{}, local)
	if got := usageGridResetCell(local[0]).Text; got != fable+"^" {
		t.Fatalf("a local row completed from the daemon: cell %q, want %q", got, fable+"^")
	}
}
