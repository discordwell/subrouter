package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// PATCHES §65: an Opus or Sonnet turn ranks an account's weekly pace by
// selectacct's FableSpentFactor. selectacct decides it from Score.FableSpent;
// these tests pin how this daemon publishes the measurement, reads a peer's,
// and shows it on the pool board.

const reasonFableSpent = "higher weekly pace after the Fable-spent preference and the live-session discount"

// setFableSpentPreferenceForTest flips the process switch for one test. Only
// for tests that are not t.Parallel.
func setFableSpentPreferenceForTest(t *testing.T, on bool) {
	t.Helper()
	prev := selectacct.FableSpentPreference()
	selectacct.SetFableSpentPreference(on)
	t.Cleanup(func() { selectacct.SetFableSpentPreference(prev) })
}

// rawNormalized is each row's normalized block of a usage-status as JSON
// objects, so a test can tell an omitted field from a zero one.
func rawNormalized(t *testing.T, box Server) map[string]map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, remotePoolUsagePath, nil)
	req.Header.Set("Authorization", "Bearer admin")
	recorder := httptest.NewRecorder()
	box.Handler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("usage-status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var rows []struct {
		ID         string         `json:"id"`
		Normalized map[string]any `json:"normalized"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, row := range rows {
		out[row.ID] = row.Normalized
	}
	return out
}

// The owner publishes its measured Fable meter at the top level of an
// account's normalized block, whatever its own switch says, and omits it when
// the meter is untouched -- including behind a general week mostly spent,
// since it is the Fable meter alone. No pool entry carries it.
func TestABoxPublishesItsFableMeterAtTheTopLevel(t *testing.T) {
	now := time.Now()
	box, _, _ := claudeUsageBox(t, map[string]string{
		"spent":     claudeUsageJSON(now, 10, 3*time.Hour, 40, 72*time.Hour, claudeFableBucket(now, 60, 72*time.Hour)),
		"untouched": claudeUsageJSON(now, 10, 3*time.Hour, 85, 72*time.Hour, claudeFableBucket(now, 0, 72*time.Hour)),
	}, nil)
	for _, on := range []bool{true, false} {
		setFableSpentPreferenceForTest(t, on)
		rows := rawNormalized(t, box)
		spent, untouched := rows["spent"], rows["untouched"]
		if spent == nil || untouched == nil || spent["pools"] == nil {
			t.Fatalf("calibration: rows without a normalized block or pools: %v", rows)
		}
		if got := spent["fable_week_spent"]; got != 0.6 {
			t.Fatalf("switch %v: spent published fable_week_spent %v, want 0.6 (its Fable meter, 60%% used)", on, got)
		}
		if got, ok := untouched["fable_week_spent"]; ok {
			t.Fatalf("switch %v: untouched published fable_week_spent %v, want it omitted: its Fable meter is unused", on, got)
		}
		for key, pool := range spent["pools"].(map[string]any) {
			if got, ok := pool.(map[string]any)["fable_week_spent"]; ok {
				t.Fatalf("pool %q published fable_week_spent %v; the meter is published at the top level only", key, got)
			}
		}
	}
}

// fableSpentPeerBody is a peer's usage-status: two accounts with identical
// published numbers, each with a Fable pool entry and, when opusPool, an Opus
// one. spent gives an account a top-level fable_week_spent, poolSpent one on
// its pool entries only (no owner publishes that). Neither field is present
// otherwise, as from a peer before §65.
func fableSpentPeerBody(t *testing.T, spent, poolSpent map[string]float64, opusPool bool) []byte {
	t.Helper()
	eligible := map[string]any{"eligible": true, "reason": selectacct.VerdictEligible}
	row := func(id string) map[string]any {
		pool := map[string]any{"headroom": 0.5, "short_headroom": 0.9, "fresh": true, "new_session": eligible}
		if value, ok := poolSpent[id]; ok {
			pool["fable_week_spent"] = value
		}
		pools := map[string]any{selectacct.ModelKey(agentclaude.FableFeature): pool}
		if opusPool {
			pools[selectacct.ModelKey(agentclaude.OpusFeature)] = pool
		}
		normalized := map[string]any{
			"headroom": 0.5, "short_headroom": 0.9, "fresh": true,
			"weekly_headroom": 0.3, "seconds_to_weekly_reset": 72 * 3600,
			"new_session": eligible, "pools": pools,
		}
		if value, ok := spent[id]; ok {
			normalized["fable_week_spent"] = value
		}
		return map[string]any{"id": id, "provider": "claude", "auth_mode": "oauth", "auth_checked": true, "auth_valid": true,
			"normalized": normalized}
	}
	// "a-untouched" sorts first, so the terminal id tie-break favours the
	// account whose Fable meter is NOT spent: only the preference can put
	// b-spent first.
	body, err := json.Marshal([]map[string]any{row("a-untouched"), row("b-spent")})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// The laptop ranks a peer's account for an Opus turn on the Fable meter the
// peer published, as its own: at equal published pace the spent account wins.
// A pool entry takes its account's value, always; a body without the field
// (a peer before §65) ranks at 1x; the laptop's switch off ignores it.
func TestALaptopRanksAPeerAccountOnItsPublishedFableMeter(t *testing.T) {
	now := time.Now()
	bodyAt := now.Add(-time.Minute)
	peer := NewRemotePool("http://peer.invalid", "", "")
	place := func(t *testing.T, body []byte, poolModel string) crossPoolPlacement {
		t.Helper()
		candidates, _, ok := peer.decisionCandidates(AdminSnapshot{Body: body, At: bodyAt}, string(accounts.ProviderClaude), poolModel, nil, now)
		if !ok || len(candidates) != 2 {
			t.Fatalf("calibration: %d candidates (ok %v), want both accounts", len(candidates), ok)
		}
		placement, err := placeAcrossPools(selectacct.NewScheduler(nil).ForModel(poolModel), nil, candidates)
		if err != nil || placement.Peer == nil {
			t.Fatalf("placement %+v err %v", placement, err)
		}
		return placement
	}
	const tied = "tied; ordered by account id"
	expect := func(t *testing.T, placement crossPoolPlacement, winner, clause string) {
		t.Helper()
		if placement.Peer.Account != winner || placement.Clause != clause {
			t.Fatalf("placed on %q (%q), want %q (%q)", placement.Peer.Account, placement.Clause, winner, clause)
		}
	}
	spent := map[string]float64{"b-spent": 1}
	opus := agentclaude.OpusFeature

	t.Run("a peer without the field ranks at 1x", func(t *testing.T) {
		expect(t, place(t, fableSpentPeerBody(t, nil, nil, false), opus), "a-untouched", tied)
	})
	t.Run("an opus turn, read off the top level", func(t *testing.T) {
		expect(t, place(t, fableSpentPeerBody(t, spent, nil, false), opus), "b-spent", reasonFableSpent)
	})
	t.Run("an opus turn on a published opus pool takes its account's meter", func(t *testing.T) {
		expect(t, place(t, fableSpentPeerBody(t, spent, nil, true), opus), "b-spent", reasonFableSpent)
	})
	t.Run("a pool entry's own field is not read", func(t *testing.T) {
		expect(t, place(t, fableSpentPeerBody(t, nil, spent, true), opus), "a-untouched", tied)
	})
	t.Run("a meter published above 1 counts as fully spent", func(t *testing.T) {
		both := map[string]float64{"a-untouched": 1, "b-spent": 3}
		expect(t, place(t, fableSpentPeerBody(t, both, nil, false), opus), "a-untouched", tied)
	})
	t.Run("a Fable turn ignores it", func(t *testing.T) {
		expect(t, place(t, fableSpentPeerBody(t, spent, nil, false), agentclaude.FableFeature), "a-untouched", tied)
	})
	t.Run("switch off here", func(t *testing.T) {
		setFableSpentPreferenceForTest(t, false)
		expect(t, place(t, fableSpentPeerBody(t, spent, nil, false), opus), "a-untouched", tied)
	})
}

// With ?model= an Opus model, the board's rank is routing's pick, made on the
// Fable meter, and the spent account's row carries the factor and says why;
// an untouched account's row carries none. Unscoped, the same pool ranks on
// pace alone.
func TestPoolBoardRanksAnOpusTurnOnTheFableMeter(t *testing.T) {
	windows := func(fableUsed float64) []accounts.UsageWindow {
		return []accounts.UsageWindow{
			shortWindow(10, 9000),
			claudeWeekly(40, 72*3600),
			{Name: agentclaude.FableWindowName, Feature: agentclaude.FableFeature, UsedPercent: fableUsed,
				LimitWindowSeconds: 604800, ResetAfterSeconds: 72 * 3600},
		}
	}
	score := func(id string, fableUsed float64) selectacct.Score {
		s := scoreFromUsageWindows(accounts.ProviderClaude, id, windows(fableUsed))
		s.Fresh = true
		return s
	}
	local := []accounts.Account{
		{ID: "a-untouched", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
		{ID: "b-spent", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
	}
	server := poolTestServer(t, local)
	server.Scheduler = selectacct.NewScheduler([]selectacct.Score{score("a-untouched", 0), score("b-spent", 100)})
	board := func(t *testing.T, model string) (map[string]poolRow, poolBoard) {
		t.Helper()
		rec := httptest.NewRecorder()
		server.handlePoolData(rec, loopbackRequest(http.MethodGet, "/_subrouter/pool.json?model="+model, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("pool.json = %d: %s", rec.Code, rec.Body.String())
		}
		var decoded poolBoard
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		rows := map[string]poolRow{}
		for _, row := range decoded.Rows {
			rows[row.ID] = row
		}
		return rows, decoded
	}

	rows, _ := board(t, "")
	if rows["a-untouched"].Rank != 1 || rows["a-untouched"].Reason != "tied; ordered by account id" {
		t.Fatalf("calibration: unscoped, rank 1 is %+v; want a-untouched on the id tie", rows["a-untouched"])
	}

	const model = "claude-opus-4-8"
	routed, err := placeAcrossPools(server.selectionScheduler(claudePoolModel(model), ""), local, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, decoded := board(t, model)
	spent, untouched := rows["b-spent"], rows["a-untouched"]
	if decoded.ScopedModel != agentclaude.OpusFeature || routed.Account.ID != "b-spent" ||
		spent.Rank != 1 || spent.Reason != reasonFableSpent {
		t.Fatalf("scoped %q: routing picked %q; board rank 1 is %q (rank %d, %q); want b-spent from both, for %q",
			decoded.ScopedModel, routed.Account.ID, spent.ID, spent.Rank, spent.Reason, reasonFableSpent)
	}
	if spent.FableSpentFactor != 2 || !strings.Contains(spent.StateNote, "2x its weekly pace: its Fable meter is 100% spent") ||
		untouched.FableSpentFactor != 0 || untouched.StateNote != "eligible for new sessions" {
		t.Fatalf("rows: b-spent factor %v note %q; a-untouched factor %v note %q; want 2 with a note naming 2x and 100%% spent, and none",
			spent.FableSpentFactor, spent.StateNote, untouched.FableSpentFactor, untouched.StateNote)
	}
}
