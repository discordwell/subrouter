package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
)

// PATCHES §91.2 on the board: every reset of this daemon's accounts and of
// its peer's is on their pool.json rows, compact; a peer that publishes no
// list shows what its complimentary_reset says, marked partial; and the board
// page draws each kind with its soonest expiry. Every provider is a fake, and
// nothing is claimed or redeemed.

// oldPeerAccounts is the usage-status a peer before §91 publishes: §64's
// complimentary_reset and no resets list.
func oldPeerAccounts(weekEnds, fableEnds time.Time) string {
	week, fable := weekEnds.UTC().Format(time.RFC3339), fableEnds.UTC().Format(time.RFC3339)
	row := func(id, provider, reset string) string {
		body := `{"id":"` + id + `","provider":"` + provider + `","auth_mode":"oauth","source":"peer","auth_checked":true,"auth_valid":true`
		if reset != "" {
			body += `,"complimentary_reset":` + reset
		}
		return body + `}`
	}
	return `[` + strings.Join([]string{
		row("old-both", "claude", `{"known":true,"available":true,"eligible":true,"remaining":3,"used":0,"total":3,"source":"cedar_ember",`+
			`"expires_at":"`+week+`","clears":["seven_day","seven_day_overage_included"],"remaining_by_kind":{"fable":2,"week":1},"week_held_until":"`+week+`"}`),
		row("old-fable", "claude", `{"known":true,"available":true,"eligible":true,"remaining":2,"used":0,"total":2,"source":"cedar_ember",`+
			`"expires_at":"`+fable+`","clears":["seven_day_overage_included"],"remaining_by_kind":{"fable":2}}`),
		row("old-before-64", "claude", `{"known":true,"available":true,"eligible":true,"remaining":2,"source":"cedar_ember",`+
			`"expires_at":"`+week+`","clears":["seven_day","seven_day_overage_included"]}`),
		row("old-none", "claude", `{"known":true,"eligible":true,"remaining":0,"used":1,"total":1,"source":"cedar_ember"}`),
		row("old-unread", "claude", ""),
		row("old-codex", "codex", codexCreditCount),
	}, ",") + `]`
}

// servedPeer is a peer that answers its usage-status with body and nothing
// else.
func servedPeer(t *testing.T, body string) string {
	t.Helper()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != remotePoolUsagePath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(peer.Close)
	return peer.URL
}

// poolRowsOf is pool.json as server serves it, by account.
func poolRowsOf(t *testing.T, server Server) map[string]poolRow {
	t.Helper()
	rec := httptest.NewRecorder()
	server.handlePoolData(rec, loopbackRequest(http.MethodGet, "/_subrouter/pool.json", ""))
	var board poolBoard
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &board) != nil {
		t.Fatalf("pool.json = %d: %s", rec.Code, rec.Body.String())
	}
	rows := map[string]poolRow{}
	for _, row := range board.Rows {
		rows[row.ID] = row
	}
	return rows
}

func wantPoolResets(t *testing.T, what string, row poolRow, partial bool, want ...poolReset) {
	t.Helper()
	if want == nil {
		want = []poolReset{}
	}
	if !reflect.DeepEqual(row.Resets, want) || row.ResetsPartial != partial {
		got, _ := json.Marshal(row.Resets)
		raw, _ := json.Marshal(want)
		t.Fatalf("%s: pool.json resets\n got %s (partial %v)\nwant %s (partial %v)", what, got, row.ResetsPartial, raw, partial)
	}
}

func codexPoolReset(count int, expires time.Time, expiry accounts.ResetExpiry) poolReset {
	entry := poolReset{Kind: accounts.ResetKindCodexRateLimits, Count: count, Expiry: expiry}
	if !expires.IsZero() {
		entry.ExpiresAt = expires.UTC().Format(time.RFC3339)
	}
	return entry
}

// 1 and 2. A daemon's pool.json carries the list of each of its own accounts
// and of each account its peer holds, in a compact form, as the peer
// published it. A peer before §91 publishes no list: its rows carry what
// their complimentary_reset says, marked partial, and a kind or a date the
// block does not give is not made up.
func TestThePoolBoardCarriesEveryResetOfThisDaemonAndItsPeer(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	box := newCreditDaemon(t, map[string]int{creditAccount: 3}, map[string][]map[string]any{creditAccount: threeCredits(base)})
	box.pass(base)
	laptop := newCreditDaemon(t, map[string]int{otherCreditAccount: 2},
		map[string][]map[string]any{otherCreditAccount: threeCredits(base)[2:]})
	laptop.pass(base)
	laptop.server.RemotePool = NewRemotePool(box.proxy.URL, "", "")

	rows := poolRowsOf(t, laptop.server)
	wantPoolResets(t, "this daemon's own account", rows[otherCreditAccount], false,
		codexPoolReset(1, base.Add(5*24*time.Hour), accounts.ResetExpiryRead),
		codexPoolReset(1, base.Add(12*24*time.Hour), accounts.ResetExpiryRead))
	wantPoolResets(t, "the peer's account", rows[creditAccount], false,
		codexPoolReset(1, base.Add(5*24*time.Hour), accounts.ResetExpiryRead),
		codexPoolReset(1, base.Add(12*24*time.Hour), accounts.ResetExpiryRead),
		codexPoolReset(1, base.Add(20*24*time.Hour), accounts.ResetExpiryRead))
	if row := rows[creditAccount]; !reflect.DeepEqual(row.Pools, []string{poolNameBox}) {
		t.Fatalf("calibration: the peer's account is on %v, want the box alone", row.Pools)
	}

	// The CLI's read of every peer passes the peer's list on as it came.
	rec := httptest.NewRecorder()
	laptop.server.handlePeersUsageStatus(rec, loopbackRequest(http.MethodGet, PeersUsageStatusPath, ""))
	var view PeersUsageStatus
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &view) != nil || len(view.Peers) != 1 {
		t.Fatalf("peers/usage-status = %d: %s", rec.Code, rec.Body.String())
	}
	direct := httptest.NewRecorder()
	box.server.handleUsageStatus(direct, loopbackRequest(http.MethodGet, remotePoolUsagePath, ""))
	if got, want := usageStatusResets(t, view.Peers[0].Accounts)[creditAccount], usageStatusResets(t, direct.Body.Bytes())[creditAccount]; !got.present || got.resets != want.resets {
		t.Fatalf("peers/usage-status carries the peer's resets\n%s\nwant as the peer publishes them\n%s", got.resets, want.resets)
	}

	weekEnds, fableEnds := base.Add(17*24*time.Hour), fableGrantEnds(base)
	week, fable := weekEnds.Format(time.RFC3339), fableEnds.Format(time.RFC3339)
	laptop.server.RemotePool = NewRemotePool(servedPeer(t, oldPeerAccounts(weekEnds, fableEnds)), "", "")
	rows = poolRowsOf(t, laptop.server)
	wantPoolResets(t, "a week reset with two Fable resets behind it", rows["old-both"], true,
		poolReset{Kind: accounts.ResetKindClaudeWeek, Count: 1, ExpiresAt: week, Expiry: accounts.ResetExpiryRead},
		poolReset{Kind: accounts.ResetKindClaudeFable, Count: 2, Expiry: accounts.ResetExpiryNoneGiven})
	wantPoolResets(t, "Fable resets alone", rows["old-fable"], true,
		poolReset{Kind: accounts.ResetKindClaudeFable, Count: 2, ExpiresAt: fable, Expiry: accounts.ResetExpiryRead})
	wantPoolResets(t, "a block from before §64, which names no kinds", rows["old-before-64"], true,
		poolReset{Kind: accounts.ResetKindUnknown, Count: 2, ExpiresAt: week, Expiry: accounts.ResetExpiryRead})
	wantPoolResets(t, "a block that holds none", rows["old-none"], false)
	if row := rows["old-unread"]; row.Resets != nil || row.ResetsPartial {
		t.Fatalf("a row with no reading carries resets %+v (partial %v), want none", row.Resets, row.ResetsPartial)
	}
	wantPoolResets(t, "a Codex count", rows["old-codex"], true, codexPoolReset(3, time.Time{}, accounts.ResetExpiryNotRead))
}

// E. A peer's list ranks nothing: the cross-pool ranking reads the same
// candidates from the peer's body with the list and without it.
func TestAPeersResetsListChangesNoRanking(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	box := newCreditDaemon(t, map[string]int{creditAccount: 3, otherCreditAccount: 1},
		map[string][]map[string]any{creditAccount: threeCredits(base), otherCreditAccount: threeCredits(base)[2:3]})
	box.setUsed(otherCreditAccount, 70)
	box.read(t)
	box.pass(base)
	rec := httptest.NewRecorder()
	box.server.handleUsageStatus(rec, loopbackRequest(http.MethodGet, remotePoolUsagePath, ""))
	withList := rec.Body.Bytes()
	if rows := usageStatusResets(t, withList); !strings.Contains(rows[creditAccount].resets, `"expiry":"read"`) {
		t.Fatalf("calibration: the box lists %s, want its read dates", rows[creditAccount].resets)
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(withList, &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		delete(row, "resets")
	}
	without, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	peer := NewRemotePool("http://peer.invalid", "", "")
	ranked := func(body []byte) []crossPoolCandidate {
		candidates, _, ok := peer.decisionCandidates(AdminSnapshot{Body: body, At: base}, string(accounts.ProviderCodex), "", nil, base)
		if !ok || len(candidates) != 2 {
			t.Fatalf("calibration: %d peer candidates (ok %v)", len(candidates), ok)
		}
		for i := range candidates {
			candidates[i].Peer = nil
		}
		return candidates
	}
	if got, want := ranked(withList), ranked(without); !reflect.DeepEqual(got, want) {
		t.Fatalf("the peer's list changed its candidates:\n%+v\nwithout it\n%+v", got, want)
	}
}

// 4. The board page, run by node on the pool.json the daemon serves, marks
// each kind an account holds with its count and its soonest expiry, in the
// browser's time: a Codex account's credits, first not read, then read, then
// from the last good read; a Claude account's week and Fable resets beside
// the 2x; and a peer that publishes no list, marked ~. Without node, see
// nodeForTest.
func TestTheBoardPageMarksEachResetKindWithItsSoonestExpiry(t *testing.T) {
	node := nodeForTest(t)
	base := time.Now().UTC().Truncate(time.Second)
	d := newCreditDaemon(t, map[string]int{creditAccount: 3, otherCreditAccount: 0},
		map[string][]map[string]any{creditAccount: threeCredits(base)})
	soonest := base.Add(5 * 24 * time.Hour).Format("1/2")
	drawn := func(t *testing.T, when, account, mark string) {
		t.Helper()
		row := renderPoolPage(t, d.server, node)[account]
		if mark == "" {
			if strings.Contains(row, `class="creset"`) {
				t.Fatalf("%s, the page drew %s as\n%s\nwant no reset mark", when, account, row)
			}
			return
		}
		if !strings.Contains(row, `">`+mark+`</span></td>`) || !strings.Contains(row, `<span class="creset" title="`) {
			t.Fatalf("%s, the page drew %s as\n%s\nwant the mark %q", when, account, row, mark)
		}
	}
	drawn(t, "before the list is read", creditAccount, "R3 ?")
	drawn(t, "before the list is read", otherCreditAccount, "")
	d.pass(base)
	drawn(t, "once the list is read", creditAccount, "R3 "+soonest)
	d.answer(creditAccount, creditAnswer{status: http.StatusInternalServerError})
	d.pass(base.Add(2 * time.Hour))
	drawn(t, "after a failed read", creditAccount, "R3 "+soonest+"*")

	weekEnds, fableEnds := base.Add(17*24*time.Hour), fableGrantEnds(base)
	claude, _, _ := claudeUsageBox(t, map[string]string{
		"a-both":  resetHoldUsage(base, 50, cedarEmberBlock(fableGrant(2, fableEnds), decemberWeekGrant(1, weekEnds))),
		"b-fable": resetHoldUsage(base, 50, cedarEmberBlock(fableGrant(2, fableEnds))),
	}, nil)
	ctx := context.Background()
	claude.updateSchedulerFromUsageStatusesContext(ctx, claude.AccountRef.UsageStatuses(ctx))
	claude.RemotePool = NewRemotePool(servedPeer(t, oldPeerAccounts(weekEnds, fableEnds)), "", "")
	drawnRows := renderPoolPage(t, claude, node)
	rows := map[string]string{}
	for title, row := range drawnRows {
		rows[strings.TrimSuffix(title, "@example.com")] = row
	}
	week, fable := weekEnds.Format("1/2"), fableEnds.Format("1/2")
	for account, marks := range map[string][]string{
		"a-both":    {`<span class="wreset" title="1 week ending ` + weekEnds.Format(time.RFC3339) + ` (read)">W1 ` + week + `</span>`, `>F2 ` + fable + `</span>`},
		"b-fable":   {`<span class="freset" title="2 fable ending ` + fableEnds.Format(time.RFC3339) + ` (read)">F2 ` + fable + `</span></td>`},
		"old-both":  {`>W1 ` + week + `</span> <span class="freset" title="2 fable (none given)">F2</span> <span class="rpart"`},
		"old-codex": {`>R3 ?</span> <span class="rpart"`},
	} {
		for _, mark := range marks {
			if !strings.Contains(rows[account], mark) {
				t.Errorf("the page drew %s as\n%s\nwant it to contain\n%s", account, rows[account], mark)
			}
		}
	}
	if none, drawn := rows["old-none"]; !drawn || strings.Contains(none, `reset" title=`) || strings.Contains(rows["a-both"], "rpart") {
		t.Errorf("a full list is marked partial, or a row that holds none is marked:\n%s\n%s", rows["a-both"], none)
	}
	if !strings.Contains(rows["a-both"], `<span class="hold">2x</span> <span class="wreset"`) {
		t.Errorf("the week mark does not follow the 2x on a week holder:\n%s", rows["a-both"])
	}
}
