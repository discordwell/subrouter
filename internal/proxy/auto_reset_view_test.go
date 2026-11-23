package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// PATCHES §93.3: the views of automatic resets, and the manual claims beside
// them. No fake here answers a request to a real provider.

// autoResetRows is each usage-status row's auto_reset, by account id, as a
// reader of the route decodes it.
func autoResetRows(t *testing.T, server Server) map[string]*AutoResetStatus {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, loopbackAdminRequest(http.MethodGet, "/_subrouter/usage-status?snapshot=1", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("usage-status answered %d: %s", recorder.Code, recorder.Body.String())
	}
	var rows []struct {
		ID        string           `json:"id"`
		AutoReset *AutoResetStatus `json:"auto_reset"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
		t.Fatalf("usage-status did not decode: %v", err)
	}
	out := map[string]*AutoResetStatus{}
	for _, row := range rows {
		out[row.ID] = row.AutoReset
	}
	return out
}

// 1. Each row of an account this daemon holds carries the mode, the owner's
// condition and the last automatic decision: when, what it came to, the
// kind and the reason. An account with no decision carries the mode alone.
func TestUsageStatusCarriesTheAutoResetModeAndTheLastDecision(t *testing.T) {
	t.Run("shadow, a would-claim at the wall", func(t *testing.T) {
		setAutoResetForTest(t, AutoResetShadow, true)
		d := newAutoResetClaude(t, heldAndSpare(claudeWall("7d")))
		d.pinSession("conversation", "held")
		d.turn("conversation", opusModel, "")
		rows := autoResetRows(t, d.server)
		held, spare := rows["held"], rows["spare"]
		if held == nil || held.Mode != "shadow" || !held.NearEmpty || held.Claiming || held.Last == nil {
			t.Fatalf("held's auto_reset %+v, want shadow with the condition on and a last decision", held)
		}
		if last := held.Last; last.Verdict != AutoResetWouldClaim || last.Kind != "week" || last.Moment != "wall" ||
			last.At.IsZero() || last.Left != "0.0%" || !strings.Contains(last.Reason, "a request met the limit of the week") {
			t.Fatalf("held's last decision %+v, want the would-claim at the week's wall", last)
		}
		if spare == nil || spare.Mode != "shadow" || !spare.NearEmpty || spare.Last != nil {
			t.Fatalf("spare's auto_reset %+v, want the mode and no decision", spare)
		}
	})
	t.Run("off, and the condition off", func(t *testing.T) {
		setAutoResetForTest(t, AutoResetOff, false)
		d := newAutoResetClaude(t, heldAndSpare(claudeWall("7d")))
		d.pinSession("conversation", "held")
		d.turn("conversation", opusModel, "")
		for name, row := range autoResetRows(t, d.server) {
			if row == nil || row.Mode != "off" || row.NearEmpty || row.Last != nil {
				t.Fatalf("%s's auto_reset %+v, want mode off, the condition off and no decision", name, row)
			}
		}
	})
	t.Run("real, refused by the condition", func(t *testing.T) {
		setAutoResetForTest(t, AutoResetReal, true)
		f := newClaudeClaimFake(t, map[string]*claudeClaimProfile{
			"held":  {week: 98, fable: 95, grants: []bool{false}},
			"spare": {week: 40, fable: 10},
		})
		d := f.daemon(t, "")
		d.pinSession("conversation", "held")
		f.set("held", func(p *claudeClaimProfile) { p.fable = 100 })
		d.turn("conversation", "claude-fable-5-2")
		held := autoResetRows(t, d.server)["held"]
		if held == nil || held.Mode != "real" || held.Last == nil || held.Last.Verdict != AutoResetRefusedByCondition ||
			held.Last.Kind != "week" || !strings.Contains(held.Last.Reason, "the week has 2.0% left") {
			t.Fatalf("held's auto_reset %+v, want real and the week reset refused by the condition", held)
		}
	})
	for _, c := range []struct {
		name    string
		answer  func() (int, string)
		verdict AutoResetVerdict
		reason  string
	}{
		{"real, claimed", nil, AutoResetClaimed, "Anthropic used the reset"},
		{"real, failed", func() (int, string) { return http.StatusForbidden, `{}` }, AutoResetFailed, "auth_error"},
		{"real, may have landed", func() (int, string) { return http.StatusInternalServerError, `{}` }, AutoResetMayHaveLanded, "may have landed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			setAutoResetForTest(t, AutoResetReal, true)
			f := newClaudeClaimFake(t, heldWeek())
			f.answer = c.answer
			d := f.daemon(t, "")
			d.pinSession("conversation", "held")
			f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
			d.turn("conversation", opusModel)
			held := autoResetRows(t, d.server)["held"]
			if held == nil || held.Mode != "real" || held.Claiming || held.Last == nil || held.Last.Verdict != c.verdict ||
				held.Last.Kind != "week" || held.Last.Reset != "week_reset_0" || !strings.Contains(held.Last.Reason, c.reason) {
				t.Fatalf("held's auto_reset %+v (last %+v), want real and the week reset %s", held, held.Last, c.verdict)
			}
		})
	}
	t.Run("real, while the claim runs", func(t *testing.T) {
		setAutoResetForTest(t, AutoResetReal, true)
		f := newClaudeClaimFake(t, heldWeek())
		f.hold = make(chan struct{})
		d := f.daemon(t, "")
		d.pinSession("conversation", "held")
		f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
		answered := make(chan claudeAnswer, 1)
		go func() { answered <- d.ask("conversation", opusModel, nil) }()
		waitClaiming(t, f)
		held := autoResetRows(t, d.server)["held"]
		close(f.hold)
		<-answered
		settle(t, d.server)
		if held == nil || !held.Claiming || held.Last == nil || held.Last.Verdict != AutoResetWouldClaim ||
			!strings.Contains(held.Last.Reason, "claiming it now") {
			t.Fatalf("held's auto_reset while the claim ran: %+v, want claiming", held)
		}
		if after := autoResetRows(t, d.server)["held"]; after == nil || after.Claiming || after.Last == nil || after.Last.Verdict != AutoResetClaimed {
			t.Fatalf("held's auto_reset after the claim: %+v, want claimed and no longer claiming", after)
		}
	})
	t.Run("real, a Codex credit", func(t *testing.T) {
		setAutoResetForTest(t, AutoResetReal, true)
		f := newCodexClaimFake(t, map[string]*codexClaimAccount{
			heldCodex:  {used: 90, credits: heldCodexCredits("credit-soon")},
			spareCodex: {used: 30},
		})
		f.pinSession("conversation", heldCodex)
		f.set(heldCodex, func(a *codexClaimAccount) { a.used = 100 })
		f.post("conversation")
		rows := autoResetRows(t, f.server)
		held, spare := rows[heldCodex], rows[spareCodex]
		if held == nil || held.Mode != "real" || held.Last == nil || held.Last.Verdict != AutoResetClaimed ||
			held.Last.Kind != "codex_credit" || held.Last.Reset != "credit-soon" || held.Last.Provider != accounts.ProviderCodex {
			t.Fatalf("held's auto_reset %+v, want the Codex credit claimed", held)
		}
		if spare == nil || spare.Mode != "real" || spare.Last != nil {
			t.Fatalf("spare's auto_reset %+v, want the mode and no decision", spare)
		}
	})
}

// 2. The health route names the mode and whether the owner's condition is on.
func TestHealthNamesTheAutoResetModeAndTheCondition(t *testing.T) {
	for _, c := range []struct {
		mode      AutoResetMode
		nearEmpty bool
		want      string
	}{
		{AutoResetShadow, true, `{"mode":"shadow","near_empty":true}`},
		{AutoResetOff, true, `{"mode":"off","near_empty":true}`},
		{AutoResetReal, false, `{"mode":"real","near_empty":false}`},
	} {
		setAutoResetForTest(t, c.mode, c.nearEmpty)
		recorder := httptest.NewRecorder()
		Server{}.handleHealth(recorder, httptest.NewRequest(http.MethodGet, "/_subrouter/health", nil))
		var body struct {
			AutoReset map[string]any `json:"auto_reset"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode health: %v", err)
		}
		var want map[string]any
		_ = json.Unmarshal([]byte(c.want), &want)
		if !reflect.DeepEqual(body.AutoReset, want) {
			t.Fatalf("mode %s, condition %v: health auto_reset = %v, want %s", c.mode, c.nearEmpty, body.AutoReset, c.want)
		}
	}
}

// 4. `sr claude reset` and `sr codex reset` claim exactly as before in every
// mode: through the same routes, with nothing of §93 in the way. In real mode
// an automatic claim has just landed on the account, and its "never twice"
// stops a second automatic claim, not a manual one.
func TestManualResetsAreTheSameInEveryMode(t *testing.T) {
	for _, mode := range []AutoResetMode{AutoResetOff, AutoResetShadow, AutoResetReal} {
		t.Run("claude "+mode.String(), func(t *testing.T) {
			setAutoResetForTest(t, mode, true)
			f := newClaudeClaimFake(t, map[string]*claudeClaimProfile{
				"held":  {week: 95, fable: 20, grants: []bool{false, true}},
				"spare": {week: 40, fable: 10},
			})
			d := f.daemon(t, t.TempDir())
			automatic := 0
			if mode == AutoResetReal {
				d.pinSession("conversation", "held")
				f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
				d.turn("conversation", opusModel)
				if claims, _ := f.counts(); claims != 1 || d.decision("held").Verdict != AutoResetClaimed {
					t.Fatalf("calibration: %d automatic claims, decision %+v; want one that landed", claims, d.decision("held"))
				}
				automatic = 1
				// The next automatic decision would be refused: never twice.
				f.set("held", func(p *claudeClaimProfile) { p.fable = 100 })
				d.turn("conversation", "claude-fable-5-2")
				if claims, _ := f.counts(); claims != 1 || !strings.Contains(d.decision("held").Reason, "never twice") {
					t.Fatalf("calibration: %d claims, decision %+v; want the second automatic claim stopped", claims, d.decision("held"))
				}
			} else {
				// A grant is usable at a limit (§46): the same state the
				// manual claim meets in real mode.
				f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
			}
			code, response := postClaudeReset(t, d.server.Handler(), `{"account":"held","dry_run":false,"confirm_account":"held"}`)
			claims, _ := f.counts()
			if code != http.StatusOK || !response.Claimed || response.Result != "reset" || claims != automatic+1 {
				t.Fatalf("the manual claim: %d %+v and %d claims at Anthropic, want it claimed with one request", code, response, claims-automatic)
			}
		})
		t.Run("codex "+mode.String(), func(t *testing.T) {
			setAutoResetForTest(t, mode, true)
			f := newCodexClaimFake(t, map[string]*codexClaimAccount{
				heldCodex:  {used: 40, credits: heldCodexCredits("credit-1", "credit-2")},
				spareCodex: {used: 30},
			})
			automatic := 0
			if mode == AutoResetReal {
				f.pinSession("conversation", heldCodex)
				f.set(heldCodex, func(a *codexClaimAccount) { a.used = 100 })
				f.post("conversation")
				if consumes, _ := f.counts(); consumes != 1 || f.decision(heldCodex).Verdict != AutoResetClaimed {
					t.Fatalf("calibration: %d automatic redeems, decision %+v; want one that landed", consumes, f.decision(heldCodex))
				}
				automatic = 1
				f.set(heldCodex, func(a *codexClaimAccount) { a.used = 40 })
				f.measure()
			}
			recorder := httptest.NewRecorder()
			f.server.Handler().ServeHTTP(recorder, loopbackAdminRequest(http.MethodPost, "/_subrouter/rate-limit-reset?email="+heldCodex+"&anytime=true", nil))
			var answer struct {
				Results []RateLimitResetResult `json:"results"`
			}
			_ = json.Unmarshal(recorder.Body.Bytes(), &answer)
			consumes, _ := f.counts()
			if recorder.Code != http.StatusOK || len(answer.Results) != 1 || !answer.Results[0].Reset || consumes != automatic+1 {
				t.Fatalf("the manual redeem: %d %s and %d consumes, want it redeemed with one consume", recorder.Code, recorder.Body.String(), consumes-automatic)
			}
		})
	}
}
