package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

// PATCHES §103 through the daemon: its usage history gives the account's own
// Fable rate and the pool's need, its readings the roll, and Anthropic's
// block the reset and its expiry. Every provider is a fake; the shadow fake
// fails the test on a claim.

// fableHistory is a usage history in which each account of rates spends its
// Fable meter at its rate: over the last 24 hours it rises at that rate for
// six hours to its limit and stays there, so its 24-hour pressure is the
// rate. The week stands at half used. The history's clock stands 100 hours
// before the live readings, which add nothing to its windows.
func fableHistory(t *testing.T, rates map[string]float64) *UsageHistory {
	t.Helper()
	now := time.Now().Add(-100 * time.Hour).Truncate(time.Second)
	h := burnHistory(t, now)
	roll := now.Add(3 * burnDay)
	for id, rate := range rates {
		rising := line(1-rate*weekShare(6*time.Hour), rate, 24*time.Hour)
		seedSeries(h, accounts.Account{ID: id, Provider: accounts.ProviderClaude}, now, 24*time.Hour, 0,
			func(ago time.Duration, _ time.Time) []accounts.UsageWindow {
				return append(claudeWeek(50, roll), accounts.UsageWindow{Name: agentclaude.FableWindowName,
					UsedPercent: 100 * min(1, rising(ago)), LimitWindowSeconds: 7 * 24 * 3600, ResetAt: roll})
			})
	}
	return h
}

// fableGrantsEnding are n Fable resets ending at ends, as Anthropic's block
// lists them.
func fableGrantsEnding(n int, ends time.Time) []map[string]any {
	var out []map[string]any
	for i := range n {
		grant := autoResetFableGrant("fable_reset_"+string(rune('a'+i)), false)
		grant["ends_at"] = ends.UTC().Format(time.RFC3339)
		out = append(out, grant)
	}
	return out
}

// liveFableDaemon is a shadow daemon of the live case: held spends its Fable
// meter at 7.07, read at 95% with its roll 6h23m away, holding grants, and
// its turn meets the Fable meter's wall; spare has room.
func liveFableDaemon(t *testing.T, grants []map[string]any) *autoResetClaude {
	t.Helper()
	setAutoResetForTest(t, AutoResetShadow, true)
	setAutoResetWorthForTest(t, true)
	d := newAutoResetClaude(t, map[string]*autoResetProfile{
		"held":  {week: 50, fable: 95, block: cedarBlock(grants...), turn: claudeWall("7d_oi"), roll: 6*time.Hour + 23*time.Minute},
		"spare": {week: 40, fable: 10},
	})
	d.server.AccountRef.SetUsageHistory(fableHistory(t, map[string]float64{"held": 7.07}))
	return d
}

// lastRule is the rule of the last decision on account's usage-status row.
func lastRule(t *testing.T, d *autoResetClaude, account string) *AutoResetRule {
	t.Helper()
	var auto struct {
		Last *AutoResetDecision `json:"last"`
	}
	if err := json.Unmarshal(usageRows(t, d.server)[account]["auto_reset"], &auto); err != nil || auto.Last == nil {
		t.Fatalf("usage-status carries no last decision for %s: %v", account, err)
	}
	return auto.Last.Rule
}

// 1, 9 and 12 through a shadow daemon at the Fable meter's wall. With §103
// the decision is held back, with its reason, no would-claim line, and the
// rule on usage-status carrying the later value and the later wall's time.
// With the switch off the decision is §94's, a would-claim adding 0.27 of a
// week. The turn gets in both what it gets with automatic resets off.
func TestAShadowDaemonHoldsTheLiveFableClaimBackForTheNextWall(t *testing.T) {
	ends := time.Now().Add(15 * 7 * burnDay)
	off := newAutoResetClaude(t, map[string]*autoResetProfile{
		"held":  {week: 50, fable: 95, block: cedarBlock(fableGrantsEnding(1, ends)...), turn: claudeWall("7d_oi"), roll: 6*time.Hour + 23*time.Minute},
		"spare": {week: 40, fable: 10},
	})
	off.pinSession("conversation", "held")
	today := off.turn("conversation", opusModel, "")
	if today.status != 200 || !strings.Contains(today.body, "msg_spare") {
		t.Fatalf("calibration: with automatic resets off the turn got %+v, want spare's answer", today)
	}
	for _, on := range []bool{true, false} {
		setAutoResetWaitForBetterForTest(t, on)
		d := liveFableDaemon(t, fableGrantsEnding(1, ends))
		d.pinSession("conversation", "held")
		if answer := d.turn("conversation", opusModel, ""); !reflect.DeepEqual(answer, today) {
			t.Fatalf("§103 %v: the turn got %+v; with automatic resets off it got %+v", on, answer, today)
		}
		decision, _ := d.decision("held")
		lines := wouldClaimLines(d.logs)
		if !on {
			wantDecision(t, "the switch off", decision, AutoResetWouldClaim, AutoResetByValue, "a claim adds 0.27 of a week", "its own 24-hour rate")
			if decision.Rule.LaterValue != nil || len(lines) != 1 {
				t.Fatalf("the switch off: rule %+v and %d would-claim lines, want §94's claim and its line", decision.Rule, len(lines))
			}
			continue
		}
		wantDecision(t, "§103 on", decision, AutoResetHeldBack, AutoResetByLaterWall,
			"a request met the limit of the Fable meter", "held back: a claim adds 0.27 of a week now, and the same reset is expected to add 1.00 at the account's next wall",
			"23h46m after its roll", "7.07 (its own 24-hour rate)", "so it waits for that wall")
		if len(lines) != 0 {
			t.Fatalf("a held-back decision logged %d would-claim lines:\n%s", len(lines), d.logs)
		}
		rule := lastRule(t, d, "held")
		roll := decision.At.Add(6*time.Hour + 23*time.Minute)
		if rule == nil || rule.DecidedBy != AutoResetByLaterWall || rule.LaterValue == nil || *rule.LaterValue != 1 || rule.LaterWallAt == nil ||
			!timesWithin(*rule.LaterWallAt, roll.Add(23*time.Hour+46*time.Minute), 2*time.Minute) {
			t.Fatalf("usage-status's rule %+v, want later_wall with 1.0 at about %s", rule, roll.Add(23*time.Hour+46*time.Minute).Format(time.RFC3339))
		}
	}
}

// rollingIn answers the fake's usage reads as f does, with both meters
// rolling in roll.
func rollingIn(f *claudeClaimFake, roll time.Duration) func(*http.Request) (*http.Response, error) {
	return func(req *http.Request) (*http.Response, error) {
		response, err := f.anthropic(req)
		if err != nil || req.URL.Path != "/api/oauth/usage" {
			return response, err
		}
		var body map[string]any
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			return nil, err
		}
		at := time.Now().Add(roll).UTC().Format(time.RFC3339)
		for _, meter := range []string{"seven_day", "seven_day_oauth_apps"} {
			if window, ok := body[meter].(map[string]any); ok {
				window["resets_at"] = at
			}
		}
		raw, _ := json.Marshal(body)
		response.Body = io.NopCloser(bytes.NewReader(raw))
		return response, nil
	}
}

// 10. Real mode. A turn meets the Fable meter's wall on the live case: with
// §103 the decision is held back, the fake's claim endpoint is never called,
// nothing waits, and the turn is moved to spare as with automatic resets off.
// With the switch off the reset is claimed and the turn served in place.
func TestRealModeClaimsNothingForAClaimThatWaitsForTheNextWall(t *testing.T) {
	setAutoResetWorthForTest(t, true)
	type outcome struct {
		claims int
		served []string
		on     string
		body   string
	}
	run := func(mode AutoResetMode, wait bool) (outcome, *claudeClaimDaemon) {
		setAutoResetForTest(t, mode, true)
		setAutoResetWaitForBetterForTest(t, wait)
		f := newClaudeClaimFake(t, map[string]*claudeClaimProfile{
			"held":  {week: 50, fable: 95, grants: []bool{true}},
			"spare": {week: 40, fable: 10},
		})
		d := f.daemon(t, "")
		d.server.AccountRef.client = &http.Client{Transport: proxyRoundTripFunc(rollingIn(f, 6*time.Hour+23*time.Minute))}
		d.server.AccountRef.SetUsageHistory(fableHistory(t, map[string]float64{"held": 7.07}))
		d.measure()
		d.pinSession("conversation", "held")
		f.set("held", func(p *claudeClaimProfile) { p.fable = 100 })
		answer := d.turn("conversation", "claude-fable-5-2")
		claims, served := f.counts()
		return outcome{claims: claims, served: served, on: d.sessionAccount("conversation"), body: answer.body}, d
	}
	today, _ := run(AutoResetOff, true)
	if today.claims != 0 || !reflect.DeepEqual(today.served, []string{"held 429", "spare 200"}) {
		t.Fatalf("calibration: with automatic resets off %+v, want held's refusal and spare's answer", today)
	}
	got, d := run(AutoResetReal, true)
	if !reflect.DeepEqual(got, today) {
		t.Fatalf("real mode with §103: %+v; with automatic resets off: %+v", got, today)
	}
	wantDecision(t, "real mode with §103", d.decision("held"), AutoResetHeldBack, AutoResetByLaterWall, "so it waits for that wall")
	if lines := jsonLogLines(d.logs, "auto reset: claiming the next saved reset"); len(lines) != 0 {
		t.Fatalf("a held-back decision started %d claims", len(lines))
	}
	claimed, d := run(AutoResetReal, false)
	if claimed.claims != 1 || !reflect.DeepEqual(claimed.served, []string{"held 429", "held 200"}) || claimed.on != "held" {
		t.Fatalf("real mode with the switch off: %+v, want one claim and the turn served on held", claimed)
	}
	if decision := d.decision("held"); decision.Verdict != AutoResetClaimed {
		t.Fatalf("real mode with the switch off: decision %+v, want claimed", decision)
	}
}

// 8, through the daemon's memory of its decisions. At the Fable meter's wall
// a claim adding 0.79 of the next wall's value waits; at 0.81 it would be
// claimed; the next wall on the same reset at 0.79 is still a claim, since
// the last decision let it through; at 0.63 it waits, and from there 0.79
// waits too.
func TestTheNextDecisionOnAResetThatPassedKeepsPassing(t *testing.T) {
	setAutoResetWaitForBetterForTest(t, true)
	d := liveFableDaemon(t, fableGrantsEnding(1, time.Now().Add(15*7*burnDay)))
	for i, tc := range []struct {
		roll    time.Duration
		verdict AutoResetVerdict
	}{
		{18*time.Hour + 45*time.Minute, AutoResetHeldBack},
		{19*time.Hour + 20*time.Minute, AutoResetWouldClaim},
		{18*time.Hour + 45*time.Minute, AutoResetWouldClaim},
		{15 * time.Hour, AutoResetHeldBack},
		{18*time.Hour + 45*time.Minute, AutoResetHeldBack},
	} {
		d.set("held", func(p *autoResetProfile) { p.roll = tc.roll })
		d.measure()
		if decision := d.wallDecision("wall" + string(rune('a'+i))); decision.Verdict != tc.verdict {
			t.Fatalf("wall %d, %s to the roll: %s, want %s: %s", i+1, tc.roll, decision.Verdict, tc.verdict, decision.Reason)
		}
	}
}

// 5 and 7 through the daemon's pass: the choice counts the account's resets
// too. Rolling 12 hours away, held reaches its limit about 10h50m before its
// roll, where a claim adds 0.46 of a week against a week at its next wall.
// Holding one Fable reset it is not chosen; holding three with two walls
// before their window opens it is.
func TestTheChoiceCountsTheAccountsResetsAgainstItsWalls(t *testing.T) {
	setAutoResetWaitForBetterForTest(t, true)
	setResetLoadingForTest(t, true)
	ends := time.Now().Add(12*time.Hour + 300*time.Hour)
	for _, held := range []int{1, 3} {
		d := liveFableDaemon(t, fableGrantsEnding(held, ends))
		d.set("held", func(p *autoResetProfile) { p.roll = 12 * time.Hour })
		d.measure()
		d.pass()
		would, _ := published(t, usageRows(t, d.server)["held"])
		if (would != nil) != (held == 3) {
			t.Fatalf("holding %d Fable resets: would choose %+v, want a choice only when holding 3:\n%s", held, would, d.logs)
		}
	}
}

// 5 through the daemon: the account's own resets are counted from Anthropic's
// block. Three Fable resets ending 300 hours after the roll, with two walls
// expected before their window opens, are claimed now; one waits.
func TestTheAccountsResetsAreCountedAgainstItsWalls(t *testing.T) {
	setAutoResetWaitForBetterForTest(t, true)
	ends := time.Now().Add(6*time.Hour + 23*time.Minute + 300*time.Hour)
	for _, tc := range []struct {
		held    int
		verdict AutoResetVerdict
		by      AutoResetRuleStep
	}{
		{3, AutoResetWouldClaim, AutoResetByValue},
		{1, AutoResetHeldBack, AutoResetByLaterWall},
	} {
		d := liveFableDaemon(t, fableGrantsEnding(tc.held, ends))
		decision := d.wallDecision("conversation")
		wantDecision(t, "holding "+string(rune('0'+tc.held)), decision, tc.verdict, tc.by)
		if tc.held == 1 {
			wantWords(t, "holding 1", decision.Reason, "the account can expect 2 such walls before its expiry window opens")
		}
	}
}
