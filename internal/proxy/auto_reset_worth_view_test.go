package proxy

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// PATCHES §94.3: the views of §94.2's rule. Every fake is the one §93 and
// §94.2 use; none answers for a real provider.

// ruleOf is what a decision's rule says on the wire, with trend set apart:
// the 6-hour trend of a steady history is about 0, and only its key is
// asserted.
func ruleOf(t *testing.T, rule *AutoResetRule) map[string]any {
	t.Helper()
	if rule == nil {
		return nil
	}
	raw, err := json.Marshal(rule)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["trend"]; !ok {
		t.Fatalf("the rule %s carries no trend", raw)
	}
	delete(out, "trend")
	return out
}

// sameRule compares a rule on the wire with want, its numbers to within the
// usage history's rounding.
func sameRule(got map[string]any, want string) bool {
	var expected map[string]any
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		panic(err)
	}
	if len(got) != len(expected) {
		return false
	}
	for key, value := range expected {
		number, isNumber := value.(float64)
		other, otherNumber := got[key].(float64)
		switch {
		case isNumber && otherNumber:
			if math.Abs(number-other) > 0.002 {
				return false
			}
		case isNumber || otherNumber || got[key] != value:
			return false
		}
	}
	return true
}

// 1, on facts. Each step of the rule names itself, with the pool's need and
// pressure, what a claim adds and the hours to the roll. A decision the rule
// did not make carries no rule: the rule off, or the owner's condition first.
func TestEachDecisionCarriesWhatTheRuleUsed(t *testing.T) {
	unknownClock := windowOpen(weekAt12)
	unknownClock.restarts = accounts.ClockRestartUnknown
	for _, c := range []struct {
		name    string
		facts   autoResetFacts
		verdict AutoResetVerdict
		want    string
	}{
		{"pressure 0.7", weekAt07.facts(true), AutoResetHeldBack,
			`{"decided_by":"need","need":"no","pressure":0.7,"window":"24h","value":0.25,"hours_to_roll":60}`},
		{"10 hours to the roll", weekNearRoll.facts(true), AutoResetHeldBack,
			`{"decided_by":"roll_gate","need":"yes","pressure":1.2,"window":"24h","value":0.071,"hours_to_roll":10}`},
		{"pressure 1.2, 60 hours to the roll", weekAt12.facts(true), AutoResetWouldClaim,
			`{"decided_by":"value","need":"yes","pressure":1.2,"window":"24h","value":0.429,"hours_to_roll":60}`},
		{"no figure", weekNoFigure.facts(true), AutoResetNoClaim,
			`{"decided_by":"need","need":"unknown","pressure":null,"value":0.357,"hours_to_roll":60}`},
		{"inside the expiry window", windowOpen(weekAt07).facts(true), AutoResetWouldClaim,
			`{"decided_by":"expiry_window","need":"no","pressure":0.7,"window":"24h","value":0.25,"hours_to_roll":60}`},
		{"expiring before the roll", expiring(weekAt07, 30*time.Hour).facts(true), AutoResetWouldClaim,
			`{"decided_by":"last_chance","need":"no","pressure":0.7,"window":"24h","value":0.25,"hours_to_roll":60}`},
		{"a Codex credit, 30 hours to the roll", creditNear.facts(true), AutoResetHeldBack,
			`{"decided_by":"value","need":"yes","pressure":1.3,"window":"24h","value":0.179,"hours_to_roll":30}`},
		{"a kind whose clock is not known", unknownClock.facts(true), AutoResetNoClaim,
			`{"decided_by":"unknown_clock","need":"yes","pressure":1.2,"window":"24h","value":null,"hours_to_roll":60}`},
		{"the roll not read", func() autoResetFacts {
			f := weekAt12.facts(true)
			f.worth.roll = time.Time{}
			return f
		}(), AutoResetNoClaim, `{"decided_by":"roll_unread","need":"yes","pressure":1.2,"window":"24h","value":null,"hours_to_roll":null}`},
		{"the rule off", weekAt07.facts(false), AutoResetWouldClaim, ""},
		{"the owner's condition first", withLeft(weekAt07, 0.02).facts(true), AutoResetRefusedByCondition, ""},
	} {
		d := decideAutoReset(c.facts)
		got := ruleOf(t, d.Rule)
		if d.Verdict != c.verdict || (c.want == "") != (got == nil) || (got != nil && !sameRule(got, c.want)) {
			t.Errorf("%s: %s with rule %v, want %s with %s", c.name, d.Verdict, got, c.verdict, c.want)
		}
	}
}

// 1, through usage-status. The row of the account a shadow daemon decided
// for carries what the rule used in its last decision; the rule off, none. A
// real claim keeps what the rule used. A peer's rows, relayed, keep the
// peer's own values.
func TestUsageStatusCarriesWhatTheRuleUsed(t *testing.T) {
	ends := time.Now().Add(17 * burnDay)
	for _, c := range []struct {
		name       string
		worth      bool
		burns      map[string]float64
		ends       time.Time
		roll       time.Duration
		spareGrant bool
		verdict    AutoResetVerdict
		want       string
	}{
		{name: "pressure 0.7", worth: true, burns: map[string]float64{"spare": 0.7}, ends: ends, verdict: AutoResetHeldBack,
			want: `{"decided_by":"need","need":"no","pressure":0.7,"window":"24h","value":0.25,"hours_to_roll":60}`},
		{name: "pressure 1.2, 10 hours to the roll", worth: true, burns: map[string]float64{"spare": 1.2}, ends: ends, roll: 10 * time.Hour,
			verdict: AutoResetHeldBack, want: `{"decided_by":"roll_gate","need":"yes","pressure":1.2,"window":"24h","value":0.071,"hours_to_roll":10}`},
		{name: "pressure 1.2, 60 hours to the roll", worth: true, burns: map[string]float64{"spare": 1.2}, ends: ends, verdict: AutoResetWouldClaim,
			want: `{"decided_by":"value","need":"yes","pressure":1.2,"window":"24h","value":0.429,"hours_to_roll":60}`},
		{name: "no history", worth: true, ends: ends, verdict: AutoResetNoClaim,
			want: `{"decided_by":"need","need":"unknown","pressure":null,"value":0.357,"hours_to_roll":60}`},
		{name: "pressure 0.7, expiring before the roll", worth: true, burns: map[string]float64{"spare": 0.7}, ends: time.Now().Add(50 * time.Hour),
			verdict: AutoResetWouldClaim, want: `{"decided_by":"last_chance","need":"no","pressure":0.7,"window":"24h","value":0.25,"hours_to_roll":60}`},
		{name: "pressure 0.5, inside the expiry window", worth: true, burns: map[string]float64{"spare": 0.5}, ends: time.Now().Add(300 * time.Hour),
			spareGrant: true, verdict: AutoResetWouldClaim,
			want: `{"decided_by":"expiry_window","need":"no","pressure":0.5,"window":"24h","value":0.179,"hours_to_roll":60}`},
		{name: "pressure 0.7, the rule off", burns: map[string]float64{"spare": 0.7}, ends: ends, verdict: AutoResetWouldClaim},
	} {
		t.Run(c.name, func(t *testing.T) {
			setAutoResetWorthForTest(t, c.worth)
			d := worthClaude(t, AutoResetShadow, c.burns, heldAtTheWall(c.ends, c.spareGrant, c.roll))
			d.pinSession("conversation", "held")
			d.turn("conversation", opusModel, "")
			rows := autoResetRows(t, d.server)
			held, spare := rows["held"], rows["spare"]
			if held == nil || held.Worth != c.worth || held.Last == nil || held.Last.Verdict != c.verdict {
				t.Fatalf("held's auto_reset %+v, want worth %v and a %s decision", held, c.worth, c.verdict)
			}
			got := ruleOf(t, held.Last.Rule)
			if (c.want == "") != (got == nil) || (got != nil && !sameRule(got, c.want)) {
				t.Fatalf("held's last decision carries the rule %v, want %s (reason %q)", got, c.want, held.Last.Reason)
			}
			if spare == nil || spare.Worth != c.worth || spare.Last != nil {
				t.Fatalf("spare's auto_reset %+v, want worth %v and no decision", spare, c.worth)
			}
		})
	}

	t.Run("real, a claim keeps what the rule used", func(t *testing.T) {
		setAutoResetWorthForTest(t, true)
		setAutoResetForTest(t, AutoResetReal, true)
		f := newClaudeClaimFake(t, heldWeek())
		d := f.daemon(t, "")
		h, move := switchedHistory(t)
		move(true)
		d.server.AccountRef.SetUsageHistory(h)
		d.pinSession("conversation", "held")
		f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
		d.turn("conversation", opusModel)
		held := autoResetRows(t, d.server)["held"]
		if held == nil || held.Last == nil || held.Last.Verdict != AutoResetClaimed || held.Last.Rule == nil ||
			held.Last.Rule.DecidedBy != AutoResetByValue || held.Last.Rule.Need != ResetNeedYes || !held.Last.Rule.Pressure.atLeast(1.3) {
			t.Fatalf("held's auto_reset %+v, want claimed with the rule's need yes at pressure 1.40", held)
		}
	})

	t.Run("a peer's rows keep the peer's own values", func(t *testing.T) {
		setAutoResetWorthForTest(t, true)
		box := worthClaude(t, AutoResetShadow, map[string]float64{"spare": 0.7}, heldAtTheWall(ends, false, 0))
		box.pinSession("conversation", "held")
		box.turn("conversation", opusModel, "")
		laptop := newAutoResetClaude(t, heldAtTheWall(ends, false, 0))
		boxPool := NewRemotePool(box.proxy.URL, "", "")
		boxPool.Name = "box"
		laptop.server.RemotePeers = []*RemotePool{boxPool}
		if body := boxPool.usageForDecision(time.Now().Add(5 * time.Second)).Body; len(body) == 0 {
			t.Fatal("calibration: the laptop never read the box")
		}
		// The laptop decides by §93's rule alone; its own rows say so.
		setAutoResetWorthForTest(t, false)
		if own := autoResetRows(t, laptop.server)["held"]; own == nil || own.Worth || own.Last != nil {
			t.Fatalf("the laptop's own held row %+v, want the rule off and no decision", own)
		}
		recorder := httptest.NewRecorder()
		laptop.server.Handler().ServeHTTP(recorder, loopbackRequest(http.MethodGet, PeersUsageStatusPath, ""))
		var view PeersUsageStatus
		if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil || len(view.Peers) != 1 {
			t.Fatalf("the laptop's view of its peers: %s (%v)", recorder.Body.String(), err)
		}
		var rows []struct {
			ID        string           `json:"id"`
			AutoReset *AutoResetStatus `json:"auto_reset"`
		}
		if err := json.Unmarshal(view.Peers[0].Accounts, &rows); err != nil {
			t.Fatalf("the box's rows did not decode: %v", err)
		}
		for _, row := range rows {
			if row.ID != "held" {
				continue
			}
			if row.AutoReset == nil || !row.AutoReset.Worth || row.AutoReset.Last == nil ||
				!sameRule(ruleOf(t, row.AutoReset.Last.Rule), `{"decided_by":"need","need":"no","pressure":0.7,"window":"24h","value":0.25,"hours_to_roll":60}`) {
				t.Fatalf("the box's held row as the laptop relays it: %+v, want the box's own rule and its held-back decision", row.AutoReset)
			}
			return
		}
		t.Fatalf("the box's rows as the laptop relays them have no held: %s", view.Peers[0].Accounts)
	})
}

// 3. The health route says whether §94.2's rule decides, beside the mode and
// the owner's condition.
func TestHealthSaysWhetherTheRuleDecides(t *testing.T) {
	setAutoResetForTest(t, AutoResetShadow, true)
	for _, worth := range []bool{true, false} {
		setAutoResetWorthForTest(t, worth)
		recorder := httptest.NewRecorder()
		Server{}.handleHealth(recorder, httptest.NewRequest(http.MethodGet, "/_subrouter/health", nil))
		var body struct {
			AutoReset map[string]any `json:"auto_reset"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode health: %v", err)
		}
		if got, ok := body.AutoReset["worth"].(bool); !ok || got != worth || body.AutoReset["mode"] != "shadow" {
			t.Fatalf("the rule %v: health auto_reset = %v, want worth %v beside the mode", worth, body.AutoReset, worth)
		}
	}
}

// 5. `sr claude reset` and `sr codex reset` claim exactly as before with
// §94.2's rule on in real mode, where the older Codex spenders are folded:
// each manual claim sends one request and lands, through the same routes.
func TestManualResetsAreTheSameWithTheRuleOn(t *testing.T) {
	setAutoResetWorthForTest(t, true)
	setAutoResetForTest(t, AutoResetReal, true)
	f := newClaudeClaimFake(t, map[string]*claudeClaimProfile{
		"held":  {week: 100, fable: 20, grants: []bool{false}},
		"spare": {week: 40, fable: 10},
	})
	d := f.daemon(t, t.TempDir())
	code, response := postClaudeReset(t, d.server.Handler(), `{"account":"held","dry_run":false,"confirm_account":"held"}`)
	if claims, _ := f.counts(); code != http.StatusOK || !response.Claimed || response.Result != "reset" || claims != 1 {
		t.Fatalf("the manual Claude claim: %d %+v and %d claims at Anthropic, want it claimed with one request", code, response, claims)
	}

	codex := newCodexClaimFake(t, map[string]*codexClaimAccount{
		heldCodex:  {used: 40, credits: heldCodexCredits("credit-1")},
		spareCodex: {used: 30},
	})
	recorder := httptest.NewRecorder()
	codex.server.Handler().ServeHTTP(recorder, loopbackAdminRequest(http.MethodPost, "/_subrouter/rate-limit-reset?email="+heldCodex+"&anytime=true", nil))
	var answer struct {
		Results []RateLimitResetResult `json:"results"`
	}
	_ = json.Unmarshal(recorder.Body.Bytes(), &answer)
	if consumes, _ := codex.counts(); recorder.Code != http.StatusOK || len(answer.Results) != 1 || !answer.Results[0].Reset || consumes != 1 {
		t.Fatalf("the manual Codex redeem: %d %s and %d consumes, want it redeemed with one consume", recorder.Code, recorder.Body.String(), consumes)
	}
}
