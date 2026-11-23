package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// PATCHES §95.1 through the daemon: its background pass chooses among its own
// accounts, logs each change, and publishes the choice by mode. Every
// provider here is a fake that fails the test on a claim or a redeem.

func setResetLoadingForTest(t *testing.T, on bool) {
	t.Helper()
	prev := ResetLoading()
	SetResetLoading(on)
	t.Cleanup(func() { SetResetLoading(prev) })
}

// choiceProfiles are held, with 40% of its week left rolling in 100 hours
// and a week reset, and spare, which holds none. With both accounts' weeks
// rising at 1.2 the pool needs its saved resets on the Claude week, held
// reaches its limit in 28 hours at the pool's demand, and a claim there adds
// 0.51 of a week: held is chosen.
func choiceProfiles(block string) map[string]*autoResetProfile {
	if block == "" {
		block = cedarBlock(autoResetWeekGrant("week_reset", 1, false))
	}
	return map[string]*autoResetProfile{
		"held":  {week: 60, fable: 20, block: block, roll: 100 * time.Hour},
		"spare": {week: 40, fable: 10, roll: 100 * time.Hour},
	}
}

func choiceClaude(t *testing.T, mode AutoResetMode, loading bool, burns map[string]float64, profiles map[string]*autoResetProfile) *autoResetClaude {
	t.Helper()
	setResetLoadingForTest(t, loading)
	if burns == nil {
		burns = map[string]float64{"held": 1.2, "spare": 1.2}
	}
	if profiles == nil {
		profiles = choiceProfiles("")
	}
	return worthClaude(t, mode, burns, profiles)
}

// pass runs one background pass of the daemon's automatic resets.
func (d *autoResetClaude) pass() { d.server.autoResetPass(context.Background()) }

// usageRows is usage-status as a loopback reader gets it, each row's fields
// by account ID.
func usageRows(t *testing.T, server Server) map[string]map[string]json.RawMessage {
	t.Helper()
	rec := httptest.NewRecorder()
	server.handleUsageStatus(rec, loopbackRequest(http.MethodGet, "/_subrouter/usage-status", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("usage-status = %d: %s", rec.Code, rec.Body.String())
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]json.RawMessage{}
	for _, row := range rows {
		var id string
		_ = json.Unmarshal(row["id"], &id)
		out[id] = row
	}
	return out
}

// published is what a row says of its choice: the would_choose beside
// normalized, and the reset_choice inside it.
func published(t *testing.T, row map[string]json.RawMessage) (would, inside *selectacct.ResetChoice) {
	t.Helper()
	if raw, ok := row["would_choose"]; ok {
		would = &selectacct.ResetChoice{}
		if err := json.Unmarshal(raw, would); err != nil {
			t.Fatal(err)
		}
	}
	var normalized map[string]json.RawMessage
	if raw, ok := row["normalized"]; ok {
		if err := json.Unmarshal(raw, &normalized); err != nil {
			t.Fatal(err)
		}
	}
	if raw, ok := normalized["reset_choice"]; ok {
		inside = &selectacct.ResetChoice{}
		if err := json.Unmarshal(raw, inside); err != nil {
			t.Fatal(err)
		}
	}
	return would, inside
}

func choiceLines(logs *syncBuffer) []map[string]any {
	var out []map[string]any
	for _, message := range []string{resetChooseMessage, resetUnchooseMessage, resetWouldChooseMessage, resetWouldUnchooseMessage} {
		out = append(out, jsonLogLines(logs, message)...)
	}
	return out
}

// 7. The modes. Off decides and publishes nothing. Shadow logs a "would
// choose" line and publishes would_choose on the account's row, outside
// normalized, which stays as it was. Real publishes the choice inside
// normalized. With SUBROUTER_RESET_LOADING=0 nothing is chosen in any mode.
func TestTheChoiceIsPublishedAsTheModeSays(t *testing.T) {
	for _, mode := range []AutoResetMode{AutoResetOff, AutoResetShadow, AutoResetReal} {
		for _, loading := range []bool{true, false} {
			d := choiceClaude(t, mode, loading, nil, nil)
			d.pass()
			rows := usageRows(t, d.server)
			would, inside := published(t, rows["held"])
			spareWould, spareInside := published(t, rows["spare"])
			lines := choiceLines(d.logs)
			what := mode.String() + map[bool]string{true: "", false: " with loading off"}[loading]
			if spareWould != nil || spareInside != nil {
				t.Fatalf("%s: spare publishes %+v and %+v, want nothing", what, spareWould, spareInside)
			}
			if mode == AutoResetOff || !loading {
				if would != nil || inside != nil || len(lines) != 0 {
					t.Fatalf("%s: would_choose %+v, reset_choice %+v and %d lines, want none", what, would, inside, len(lines))
				}
				continue
			}
			got, other := would, inside
			message := resetWouldChooseMessage
			if mode == AutoResetReal {
				got, other, message = inside, would, resetChooseMessage
			}
			if got == nil || other != nil || got.Meter != selectacct.ResetChoiceWeek || got.Order != 1 ||
				!strings.Contains(got.Why, "the pool needs its saved resets on the Claude week") {
				t.Fatalf("%s: would_choose %+v and reset_choice %+v", what, would, inside)
			}
			if len(lines) != 1 || lines[0]["msg"] != message {
				t.Fatalf("%s: lines %v, want one %q", what, lines, message)
			}
		}
	}
}

// 7. In shadow mode normalized is byte for byte what it was: the choice goes
// beside it. In real mode the choice is the only change inside it.
func TestShadowLeavesNormalizedAsItWas(t *testing.T) {
	d := choiceClaude(t, AutoResetShadow, true, nil, nil)
	d.pass()
	snapshot, _ := d.server.AccountRef.UsageStatusSnapshot()
	rows := d.server.withNormalizedUsage(snapshot)
	before, _ := json.Marshal(rows)
	shadow := d.server.withResetChoices(rows)
	setAutoResetForTest(t, AutoResetReal, true)
	real := d.server.withResetChoices(rows)
	after, _ := json.Marshal(rows)
	if string(before) != string(after) {
		t.Fatal("publishing the choice changed the rows it was given")
	}
	chosen := 0
	for i := range rows {
		was, _ := json.Marshal(rows[i].Normalized)
		now, _ := json.Marshal(shadow[i].Normalized)
		if rows[i].Normalized == nil || string(was) != string(now) {
			t.Fatalf("%s: shadow's normalized\n%s\nwant\n%s", rows[i].ID, now, was)
		}
		inside := *real[i].Normalized
		inside.ResetChoice = nil
		without, _ := json.Marshal(inside)
		if string(without) != string(was) {
			t.Fatalf("%s: real mode changed normalized beyond the choice:\n%s\nwant\n%s", rows[i].ID, without, was)
		}
		if shadow[i].WouldChoose != nil {
			chosen++
			if real[i].Normalized.ResetChoice == nil || *real[i].Normalized.ResetChoice != *shadow[i].WouldChoose {
				t.Fatalf("%s: real mode publishes %+v, shadow %+v", rows[i].ID, real[i].Normalized.ResetChoice, shadow[i].WouldChoose)
			}
		}
	}
	if chosen != 1 {
		t.Fatalf("calibration: %d accounts would be chosen, want held", chosen)
	}
}

// 8. One Info line when an account becomes chosen, with its label, meter,
// order and reason, and one when it stops; none while the choice is
// unchanged. In shadow mode the lines say "would".
func TestAChoiceLogsOnceWhenItBeginsAndOnceWhenItStops(t *testing.T) {
	for _, mode := range []AutoResetMode{AutoResetShadow, AutoResetReal} {
		d := choiceClaude(t, mode, true, nil, nil)
		choose, stop := resetChooseMessage, resetUnchooseMessage
		if mode == AutoResetShadow {
			choose, stop = resetWouldChooseMessage, resetWouldUnchooseMessage
		}
		for range 3 {
			d.pass()
		}
		lines := choiceLines(d.logs)
		if len(lines) != 1 {
			t.Fatalf("%s: %d lines over three passes, want one:\n%s", mode, len(lines), d.logs.String())
		}
		for key, want := range map[string]any{"level": "INFO", "msg": choose, "account": "held", "provider": "claude", "mode": mode.String(),
			"meter": "week", "order": float64(1)} {
			if lines[0][key] != want {
				t.Fatalf("%s: line %s = %v, want %v: %v", mode, key, lines[0][key], want, lines[0])
			}
		}
		wantWords(t, mode.String(), lines[0]["reason"].(string), "the pool needs its saved resets on the Claude week", "a claim there adds 0.51 of a week")
		d.set("held", func(p *autoResetProfile) { p.block = cedarBlock() })
		d.measure()
		d.pass()
		d.pass()
		lines = choiceLines(d.logs)
		if len(lines) != 2 || lines[1]["msg"] != stop || lines[1]["account"] != "held" || lines[1]["meter"] != "week" ||
			lines[1]["reason"] != "it holds no saved reset" {
			t.Fatalf("%s: after held's reset is used, lines %v, want one %q saying it holds none", mode, lines, stop)
		}
	}
}

// 6. A peer's account is never chosen: only the daemon that holds an account
// chooses it. A laptop whose peer publishes a qualifying holder chooses
// nothing for it, and is handed the peer's row it refuses it as a candidate;
// the same account held here is chosen.
func TestAPeersAccountIsNeverChosen(t *testing.T) {
	d := choiceClaude(t, AutoResetShadow, true, map[string]float64{"solo": 1.2}, map[string]*autoResetProfile{
		"solo": {week: 40, fable: 10, roll: 100 * time.Hour},
	})
	box := choiceClaude(t, AutoResetShadow, true, nil, nil)
	box.pass()
	if would, _ := published(t, usageRows(t, box.server)["held"]); would == nil {
		t.Fatal("calibration: held is not chosen by the daemon that holds it")
	}
	body := httptest.NewRecorder()
	box.server.handleUsageStatus(body, loopbackRequest(http.MethodGet, remotePoolUsagePath, ""))
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body.Body.Bytes())
	}))
	t.Cleanup(peer.Close)
	d.server.RemotePool = NewRemotePool(peer.URL, "", "")
	d.pass()
	if usage := d.server.RemotePool.cachedUsage(); len(usage.Body) == 0 {
		t.Fatal("calibration: the laptop never read the box")
	}
	for id, row := range usageRows(t, d.server) {
		if would, inside := published(t, row); would != nil || inside != nil {
			t.Fatalf("the laptop publishes a choice on %s", id)
		}
	}
	if lines := choiceLines(d.logs); len(lines) != 0 {
		t.Fatalf("the laptop logged %v", lines)
	}
	var peerRows []AccountUsageStatus
	if err := json.Unmarshal(body.Body.Bytes(), &peerRows); err != nil {
		t.Fatal(err)
	}
	scheduler := d.server.scheduler()
	for _, row := range peerRows {
		if _, ok := d.server.resetCandidateOf(&row, scheduler); ok {
			t.Fatalf("the laptop takes the box's %s for a candidate", row.ID)
		}
	}
	own, _ := d.server.AccountRef.UsageStatusSnapshot()
	if _, ok := d.server.resetCandidateOf(&own[0], scheduler); !ok {
		t.Fatalf("calibration: the laptop's own %s is no candidate", own[0].ID)
	}
}

// 6. An account with an owner's reserve is never chosen; the same account
// without one is.
func TestAnAccountWithAnOwnersReserveIsNeverChosen(t *testing.T) {
	for _, reserved := range []bool{false, true} {
		d := choiceClaude(t, AutoResetShadow, true, nil, nil)
		if reserved {
			d.server.AccountReserves = map[string]float64{"held": 0.05}
		}
		d.pass()
		would, _ := published(t, usageRows(t, d.server)["held"])
		if reserved != (would == nil) {
			t.Fatalf("reserved %v: would_choose %+v", reserved, would)
		}
	}
}

// 6. An account whose next reset is of a kind this daemon does not know is
// never chosen, even when it holds a week reset behind it: the grant
// Anthropic names next decides, not the list's order. The same grants with
// the week reset next are chosen.
func TestAnAccountWhoseNextResetIsOfAnUnknownKindIsNeverChosen(t *testing.T) {
	short := map[string]any{"id": "short_reset", "resets_total": 1, "resets_left": 1, "use_requires_limit": true,
		"ends_at": time.Now().Add(10 * burnDay).UTC().Format(time.RFC3339), "clears": []string{"five_hour"}}
	week := autoResetWeekGrant("week_reset", 1, false)
	for _, c := range []struct {
		name   string
		block  string
		chosen bool
	}{
		{"the week reset next", cedarBlock(week, short), true},
		{"the unknown reset next", cedarBlock(short, week), false},
		{"an unknown reset alone", cedarBlock(short), false},
	} {
		d := choiceClaude(t, AutoResetShadow, true, nil, choiceProfiles(c.block))
		d.pass()
		would, _ := published(t, usageRows(t, d.server)["held"])
		if c.chosen != (would != nil) {
			t.Fatalf("%s: would_choose %+v, want chosen %v", c.name, would, c.chosen)
		}
	}
}

// choiceCodex is a Codex daemon of held, with half its week left rolling in
// 150 hours and one credit, and spare with none, whose usage history has both
// weeks rising at 1.3: held reaches its limit in 32 hours at the pool's
// demand, where a claim skips 118 hours, 0.70 of a week.
func choiceCodex(t *testing.T) *autoResetCodex {
	t.Helper()
	setAutoResetForTest(t, AutoResetShadow, true)
	setResetLoadingForTest(t, true)
	d := newAutoResetCodex(t, map[string]*autoResetCodexAccount{
		heldCodex:  {used: 50, reset: 150 * 3600, credits: []accounts.RateLimitResetCredit{autoResetTestCredit("credit", 15*burnDay)}},
		spareCodex: {used: 50, reset: 150 * 3600},
	})
	now := time.Now().Add(-100 * time.Hour).Truncate(time.Second)
	h := burnHistory(t, now)
	for _, id := range []string{heldCodex, spareCodex} {
		seedSeries(h, accounts.Account{ID: id, Provider: accounts.ProviderCodex}, now, 49*time.Hour, 0,
			codexRising(line(0.10, 1.3, 49*time.Hour), fixedRoll(now.Add(3*burnDay))))
	}
	d.server.AccountRef.SetUsageHistory(h)
	return d
}

// 6. An account with a dead credential is never chosen: the Codex holder is
// chosen, and when its credential dies it stops, with a line that says so.
func TestAnAccountWithADeadCredentialIsNeverChosen(t *testing.T) {
	d := choiceCodex(t)
	d.server.autoResetPass(context.Background())
	would, _ := published(t, usageRows(t, d.server)[heldCodex])
	if would == nil || would.Meter != selectacct.ResetChoiceCodexWeek || would.Order != 1 {
		t.Fatalf("calibration: held's would_choose is %+v, want the Codex week", would)
	}
	d.server.AccountRef.noteCredResult(d.account(heldCodex), &accounts.CodexStoredRefreshFailureError{})
	d.server.autoResetPass(context.Background())
	if would, _ := published(t, usageRows(t, d.server)[heldCodex]); would != nil {
		t.Fatalf("a dead credential: would_choose %+v", would)
	}
	stops := jsonLogLines(d.logs, resetWouldUnchooseMessage)
	if len(stops) != 1 || !strings.HasPrefix(stops[0]["reason"].(string), "its credential is dead") {
		t.Fatalf("stop lines %v, want one naming the dead credential", stops)
	}
}

// 9. Part I. The background pass reads again a peer whose last body is 10
// minutes old, and then reads need from the whole pool: here the peer's
// fresh body takes the pool's pressure from 0.6 to 1.5, and held is chosen.
// Reading usage-status and a turn at the wall make no peer request, and with
// the mode off no pass does; a new session's turn does, which shows the turns
// can see the peer.
func TestTheBackgroundPassReadsAStalePeerAndNothingElseDoes(t *testing.T) {
	profiles := choiceProfiles("")
	profiles["held"].turn = claudeWall("7d")
	d := choiceClaude(t, AutoResetShadow, true, map[string]float64{"held": 0.6}, profiles)
	peerBody := func(rate float64) []byte {
		row := AccountUsageStatus{
			AccountStatus: AccountStatus{ID: "box-account", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Label: "box"},
			Normalized:    &NormalizedUsage{Fresh: true},
			Burn:          UsageBurn{usageMeterWeek: {"24h": {Burn: figure(rate), Covered: 1}}},
		}
		body, _ := json.Marshal([]AccountUsageStatus{row})
		return body
	}
	var requests atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write(peerBody(2.4))
	}))
	t.Cleanup(peer.Close)
	pool := NewRemotePool(peer.URL, "", "")
	stale := time.Now().Add(-10 * time.Minute)
	pool.admin.entries = map[string]remotePoolAdminEntry{remotePoolUsagePath: {body: peerBody(0.6), at: stale, tried: stale}}
	d.server.RemotePool = pool
	// Handler copies the Server it is built from, so the turns below see the
	// peer only through a handler built after RemotePool is set.
	d.proxy = httptest.NewServer(d.server.Handler())
	t.Cleanup(d.proxy.Close)

	if would, _ := published(t, usageRows(t, d.server)["held"]); would != nil || requests.Load() != 0 {
		t.Fatalf("usage-status: would_choose %+v and %d peer requests, want neither", would, requests.Load())
	}
	d.turn("pinned", opusModel, "held")
	if decision, ok := d.decision("held"); !ok || decision.Moment != "wall" {
		t.Fatalf("calibration: no decision at the wall: %+v", decision)
	}
	if requests.Load() != 0 {
		t.Fatalf("a turn at the wall made %d peer requests", requests.Load())
	}
	setAutoResetForTest(t, AutoResetOff, true)
	d.pass()
	if requests.Load() != 0 {
		t.Fatalf("a pass with the mode off made %d peer requests", requests.Load())
	}
	setAutoResetForTest(t, AutoResetShadow, true)
	d.server.chooseResetAccounts()
	if would, _ := published(t, usageRows(t, d.server)["held"]); would != nil {
		t.Fatalf("calibration: on the stale body held is chosen: %+v", would)
	}
	d.pass()
	if requests.Load() != 1 {
		t.Fatalf("the pass made %d peer requests, want one", requests.Load())
	}
	would, _ := published(t, usageRows(t, d.server)["held"])
	if would == nil || !strings.Contains(would.Why, "the 24-hour pressure is 1.50") {
		t.Fatalf("after the peer's fresh body held's would_choose is %+v, want it chosen at pressure 1.50", would)
	}
	d.pass()
	if requests.Load() != 1 {
		t.Fatalf("a pass on a fresh body made %d peer requests in all, want one", requests.Load())
	}
	fourMinutes := time.Now().Add(-4 * time.Minute)
	pool.admin.mu.Lock()
	pool.admin.entries[remotePoolUsagePath] = remotePoolAdminEntry{body: peerBody(2.4), at: fourMinutes, tried: fourMinutes}
	pool.admin.mu.Unlock()
	d.pass()
	if requests.Load() != 1 {
		t.Fatalf("a pass on a body 4 minutes old made %d peer requests in all, want one", requests.Load())
	}
	d.turn("new", opusModel, "")
	waitFor(t, "calibration: a new session's turn reads the peer, as a turn at the wall could", func() bool { return requests.Load() > 1 })
}
