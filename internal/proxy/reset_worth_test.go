package proxy

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// PATCHES §94.1: pressure, need, the value of a claim, last chance and the
// expiry window. Every provider here is a fake, and nothing decides on any of
// it yet.

func figure(v float64) *float64 { return &v }

func measured(v float64) Pressure { return Pressure{state: pressureMeasured, value: v} }

var allAtLimit = Pressure{state: pressureAllAtLimit}

// Behaviour 1. Pressure is the burn among the accounts that could still
// spend: burn over the share of account-time not at the limit.
func TestPressureIsTheBurnOfTheAccountsThatCouldStillSpend(t *testing.T) {
	for _, tc := range []struct {
		name   string
		window UsagePoolWindow
		want   string
	}{
		{"0.84 with 30% at the limit", UsagePoolWindow{Burn: figure(0.84), AtLimit: figure(0.3)}, "1.20"},
		{"none at the limit", UsagePoolWindow{Burn: figure(0.84), AtLimit: figure(0)}, "0.84"},
		{"no share published", UsagePoolWindow{Burn: figure(0.84)}, "0.84"},
		{"all of it at the limit", UsagePoolWindow{Burn: figure(0), AtLimit: figure(1)}, "all at the limit"},
		{"too little history", UsagePoolWindow{Accounts: 0, NotEnoughHistory: 2}, "not known"},
	} {
		if got := poolWindowPressure(tc.window).String(); got != tc.want {
			t.Errorf("%s: pressure %s, want %s", tc.name, got, tc.want)
		}
	}
	raw, err := json.Marshal(map[string]Pressure{"24h": poolWindowPressure(UsagePoolWindow{Burn: figure(0.84), AtLimit: figure(0.3)}),
		"6h": allAtLimit, "1h": {}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"1h":null,"24h":1.2,"6h":"all_at_limit"}`; string(raw) != want {
		t.Fatalf("published as %s, want %s", raw, want)
	}
	var back map[string]Pressure
	if err := json.Unmarshal(raw, &back); err != nil || back["24h"] != measured(1.2) || back["6h"] != allAtLimit || back["1h"].known() {
		t.Fatalf("read back as %v (%v)", back, err)
	}
}

// Behaviour 2. Need: yes at a 24-hour pressure of 1.0 or all at the limit,
// or when the 6-hour pressure plus its trend reaches 1.1; with too little
// history over 24 hours the 6-hour pressure alone decides at 1.0, then the
// 1-hour one; with no figure it is unknown. Each answer says why, with its
// numbers.
func TestNeedFollowsThePressureAndItsTrend(t *testing.T) {
	pressures := func(day, six, hour Pressure) map[string]Pressure {
		return map[string]Pressure{"24h": day, "6h": six, "1h": hour}
	}
	for _, tc := range []struct {
		name     string
		pressure map[string]Pressure
		trend    *float64
		want     ResetNeed
		window   string
		says     []string
	}{
		{"24-hour pressure 1.0", pressures(measured(1.0), measured(0.5), measured(0.5)), figure(0), ResetNeedYes, "24h",
			[]string{"24-hour pressure is 1.00, at or above 1.0"}},
		{"24-hour pressure 0.99", pressures(measured(0.99), measured(0.99), measured(0.99)), figure(0), ResetNeedNo, "24h",
			[]string{"24-hour pressure is 0.99, under 1.0", "0.99 plus its trend +0.00 is 0.99, under 1.1"}},
		{"all at the limit", pressures(allAtLimit, allAtLimit, allAtLimit), nil, ResetNeedYes, "24h",
			[]string{"every counted account was at its limit through the 24-hour window"}},
		{"rising by 0.25", pressures(measured(0.8), measured(0.9), measured(1.2)), figure(0.25), ResetNeedYes, "6h",
			[]string{"24-hour pressure is 0.80, under 1.0, but the 6-hour pressure 0.90 plus its trend +0.25 is 1.15, at or above 1.1"}},
		{"rising by 0.1", pressures(measured(0.8), measured(0.9), measured(1.2)), figure(0.1), ResetNeedNo, "24h",
			[]string{"0.90 plus its trend +0.10 is 1.00, under 1.1"}},
		{"no trend yet", pressures(measured(0.8), measured(0.9), measured(1.2)), nil, ResetNeedNo, "24h",
			[]string{"24-hour pressure is 0.80, under 1.0", "6-hour trend is not known yet"}},
		{"the last 6 hours all at the limit", pressures(measured(0.8), allAtLimit, allAtLimit), nil, ResetNeedYes, "6h",
			[]string{"limit through the 6-hour window, above 1.1 whatever the trend"}},
		{"no 24-hour figure, 6 hours at 1.0", pressures(Pressure{}, measured(1.0), measured(0.2)), nil, ResetNeedYes, "6h",
			[]string{"24-hour window has too little history", "6-hour pressure is 1.00, at or above 1.0"}},
		{"no 24-hour figure, 6 hours at 0.95", pressures(Pressure{}, measured(0.95), measured(3)), figure(0.5), ResetNeedNo, "6h",
			[]string{"6-hour pressure is 0.95, under 1.0"}},
		{"only the last hour, at 1.3", pressures(Pressure{}, Pressure{}, measured(1.3)), nil, ResetNeedYes, "1h",
			[]string{"1-hour pressure is 1.30, at or above 1.0"}},
		{"only the last hour, at 0.4", pressures(Pressure{}, Pressure{}, measured(0.4)), nil, ResetNeedNo, "1h",
			[]string{"1-hour pressure is 0.40, under 1.0"}},
		{"no figure at all", pressures(Pressure{}, Pressure{}, Pressure{}), nil, ResetNeedUnknown, "",
			[]string{"no window has enough history for a pressure yet"}},
	} {
		need := needOf(tc.pressure, tc.trend)
		if need.Answer != tc.want || need.Window != tc.window {
			t.Errorf("%s: need %s decided by %q (%s), want %s by %q", tc.name, need.Answer, need.Window, need.Why, tc.want, tc.window)
		}
		for _, words := range tc.says {
			if !strings.Contains(need.Why, words) {
				t.Errorf("%s: says %q, want it to say %q", tc.name, need.Why, words)
			}
		}
	}
}

// Behaviour 3. burn.json carries need per meter, over this daemon's accounts
// and its peers' as the pool figure is: the pressures, the trend the rule
// read and why. The trend is the pressure's: here the burn's own trend reads
// +0.15 because one account is spent, and the pressure's reads +0.30.
func TestTheBurnRoutePublishesNeedPerMeter(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	roll := now.Add(3 * burnDay)
	span := 49 * time.Hour
	codex := func(id string) accounts.Account { return accounts.Account{ID: id, Provider: accounts.ProviderCodex} }

	laptop := newUnstartedCodexDaemon(t, map[string][2]float64{"rising@example.com": {20, 300000}, "spent@example.com": {100, 300000}})
	h := burnHistory(t, now)
	laptop.server.AccountRef.SetUsageHistory(h)
	// 0.6 until 6 hours ago, 0.9 since.
	seedSeries(h, codex("rising@example.com"), now, span, 0, codexRising(func(ago time.Duration) float64 {
		if ago > 6*time.Hour {
			return line(0.10, 0.6, span)(ago)
		}
		return line(0.10+0.6*weekShare(span-6*time.Hour), 0.9, 6*time.Hour)(ago)
	}, fixedRoll(roll)))
	seedSeries(h, codex("spent@example.com"), now, span, 0, codexRising(line(1, 0, span), fixedRoll(roll)))
	claudePeer, _ := claudeBurnPeer(t, now, "opus@example.com", 0.5, 1.5)
	laptop.server.RemotePeers = []*RemotePool{claudePeer}

	report := burnReport(t, laptop.server)
	week := poolMeter(t, report, "codex", usageMeterWeek)
	if week.Need == nil {
		t.Fatalf("the Codex week publishes no need: %+v", week)
	}
	if !burnNear(week.Trend["6h"].Change, 0.15, 0.002) {
		t.Fatalf("calibration: the Codex week's burn trend is %v, want +0.15", week.Trend["6h"])
	}
	day, six := week.Need.Pressure["24h"], week.Need.Pressure["6h"]
	if week.Need.Answer != ResetNeedYes || week.Need.Window != "6h" || !burnNear(week.Need.Trend, 0.3, 0.002) ||
		day.state != pressureMeasured || math.Abs(day.value-0.676) > 0.002 || six != measured(0.9) {
		t.Errorf("the Codex week's need = %+v, want yes on the 6 hours: pressure 0.68 over 24 hours, 0.90 over 6 rising by 0.30", *week.Need)
	}
	if !strings.Contains(week.Need.Why, "0.90 plus its trend +0.30 is 1.20, at or above 1.1") {
		t.Errorf("the Codex week's need says %q", week.Need.Why)
	}
	claudeWeekNeed := poolMeter(t, report, "claude", usageMeterWeek).Need
	if claudeWeekNeed == nil || claudeWeekNeed.Answer != ResetNeedNo || claudeWeekNeed.Pressure["24h"] != measured(0.5) ||
		!strings.Contains(claudeWeekNeed.Why, "24-hour pressure is 0.50, under 1.0") {
		t.Errorf("the Claude week, from the peer alone, needs %+v; want no at 0.50", claudeWeekNeed)
	}
	if fable := poolMeter(t, report, "claude", usageMeterFable).Need; fable == nil || fable.Answer != ResetNeedYes ||
		!strings.Contains(fable.Why, "24-hour pressure is 1.50, at or above 1.0") {
		t.Errorf("the Fable meter, from the peer alone, needs %+v; want yes at 1.50", fable)
	}

	fresh := newUnstartedCodexDaemon(t, map[string][2]float64{"new@example.com": {20, 300000}})
	freshHistory := burnHistory(t, now)
	fresh.server.AccountRef.SetUsageHistory(freshHistory)
	seedSeries(freshHistory, codex("new@example.com"), now, 20*time.Minute, 0, codexRising(line(0.10, 1, 20*time.Minute), fixedRoll(roll)))
	for _, meter := range burnReport(t, fresh.server).Meters {
		if meter.Need == nil || meter.Need.Answer != ResetNeedUnknown || meter.Need.Trend != nil ||
			!strings.Contains(meter.Need.Why, "no window has enough history") {
			t.Errorf("%s with 20 minutes of history needs %+v, want unknown", meter.Label, meter.Need)
		}
	}
	_, body := burnRoute(t, fresh.server)
	var compact bytes.Buffer
	if err := json.Compact(&compact, body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(compact.String(), `"need":{"answer":"unknown","pressure":{"1h":null,"24h":null,"6h":null},"trend":null,"why":`) {
		t.Errorf("burn.json spells an unknown need as %s", compact.String())
	}
}

// Behaviour 4. A claim that keeps the roll is worth what the account can
// spend again before its roll; one that restarts the seven days, the wait it
// skips; a kind nobody knows this of, nothing.
func TestTheValueOfAClaim(t *testing.T) {
	burning := func(burn, atLimit float64) UsageMeterBurn {
		return UsageMeterBurn{"24h": {Burn: figure(burn), Covered: 1, AtLimit: atLimit}}
	}
	keeps := accounts.ClockRestartMeasuredNo
	for _, tc := range []struct {
		name     string
		restarts accounts.ClockRestart
		own      UsageMeterBurn
		pool     Pressure
		toRoll   time.Duration
		want     float64
	}{
		{"burning 3.0, 28 hours to the roll", keeps, burning(3, 0), Pressure{}, 28 * time.Hour, 0.5},
		{"burning 3.0, 70 hours to the roll", keeps, burning(3, 0), Pressure{}, 70 * time.Hour, 1},
		{"no figure in a pool at 1.5", keeps, nil, measured(1.5), 28 * time.Hour, 0.25},
		{"own 0.5 in a pool at 1.5", keeps, burning(0.5, 0), measured(1.5), 28 * time.Hour, 0.25},
		{"nothing known", keeps, nil, Pressure{}, 28 * time.Hour, 0.167},
		{"own burn 1.0 with half of it at the limit", keeps, burning(1, 0.5), Pressure{}, 28 * time.Hour, 0.333},
		{"a pool all at its limit", accounts.ClockRestartAssumedNo, nil, allAtLimit, 3 * time.Hour, 1},
		{"a Codex credit, 42 hours to the roll", accounts.ClockRestartAssumedYes, burning(3, 0), Pressure{}, 42 * time.Hour, 0.25},
		{"a Codex credit in a pool all at its limit", accounts.ClockRestartAssumedYes, nil, allAtLimit, 42 * time.Hour, 0.25},
	} {
		value := claimValueOf(tc.restarts, tc.toRoll, claimRateOf(tc.own, tc.pool))
		if !value.known || math.Abs(value.value-tc.want) > 0.001 {
			t.Errorf("%s: value %.3f (known %v: %s), want %.3f", tc.name, value.value, value.known, value.why, tc.want)
		}
	}
	if value := claimValueOf(accounts.ClockRestartUnknown, 28*time.Hour, claimRateOf(burning(3, 0), measured(2))); value.known {
		t.Errorf("an unknown restarts_clock is worth %.2f (%s), want no value", value.value, value.why)
	}
	value := claimValueOf(keeps, 28*time.Hour, claimRateOf(burning(3, 0), measured(1.5)))
	if !strings.Contains(value.why, "28h") || !strings.Contains(value.why, "3.00 (its own 24-hour rate)") || !strings.Contains(value.why, "0.50 of a week") {
		t.Errorf("the value says %q", value.why)
	}
}

// Behaviour 5. A reset is in its last chance when it expires before the
// account could meet its next wall: its roll plus a week over the rate, the
// rate floored at 1.
func TestLastChance(t *testing.T) {
	roll := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	rate := func(v float64) claimRate { return claimRate{value: v} }
	for _, tc := range []struct {
		name    string
		expires time.Time
		rate    claimRate
		want    bool
	}{
		{"expires before the roll", roll.Add(-time.Hour), rate(1), true},
		{"two days after the roll at rate 1", roll.Add(48 * time.Hour), rate(1), true},
		{"two days after the roll at rate 7", roll.Add(48 * time.Hour), rate(7), false},
		{"ten days after the roll at rate 0.5, floored at 1", roll.Add(10 * 24 * time.Hour), rate(0.5), false},
		{"eight days after the roll at rate 1", roll.Add(8 * 24 * time.Hour), rate(1), false},
		{"after the roll in a pool all at its limit", roll.Add(time.Hour), claimRate{unbounded: true}, false},
		{"no date", time.Time{}, rate(1), false},
	} {
		if got := lastChance(tc.expires, roll, tc.rate); got != tc.want {
			t.Errorf("%s: last chance %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Behaviour 6, the function. The lead is 1.5 times the time the pool needs
// to spend what the holders have left, never under 24 hours and never more
// than the time left; 24 hours when the demand is not known.
func TestTheExpiryWindowsLead(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	expires := now.Add(20 * 24 * time.Hour)
	demand := func(p Pressure, n int) resetDemand {
		return resetDemand{label: "Claude week", pressure: p, window: "24h", accounts: n}
	}
	for _, tc := range []struct {
		name     string
		expires  time.Time
		demand   resetDemand
		headroom float64
		lead     time.Duration
		says     string
	}{
		{"11 holders with 6.0 weeks in 25 accounts at 1.0", expires, demand(measured(1), 25), 6,
			time.Duration(1.5 * 6 / 25 * float64(usageMeterLength)), "11 holders have 6.0 weeks of the Claude week left between them; at the pool's 24-hour pressure 1.00 over 25 accounts they need 40h19m to spend it, and 1.5 times that is 60h29m"},
		{"the same pool at 4.0", expires, demand(measured(4), 25), 6, 24 * time.Hour, "1.5 times that is 15h7m, so the 24-hour floor"},
		{"demand not known", expires, demand(Pressure{}, 0), 6, 24 * time.Hour, "demand on the Claude week is not known yet"},
		{"all at the limit", expires, demand(allAtLimit, 25), 6, 24 * time.Hour, "every counted account was at its limit"},
		{"40 hours left", now.Add(40 * time.Hour), demand(measured(1), 25), 6, 40 * time.Hour, "more than the 40h left, so the window is open"},
		{"10 hours left, demand not known", now.Add(10 * time.Hour), demand(Pressure{}, 0), 6, 10 * time.Hour, "not known yet"},
		{"a pool that spends nothing", expires, demand(measured(0), 25), 6, 20 * 24 * time.Hour, "spends nothing on it"},
	} {
		window := expiryWindow(tc.expires, now, 11, tc.headroom, tc.demand)
		lead := tc.lead.Round(time.Second)
		if window.LeadSeconds != int64(lead/time.Second) || !window.OpensAt.Equal(tc.expires.Add(-lead)) || !strings.Contains(window.Why, tc.says) {
			t.Errorf("%s: opens %s, lead %ds: %q; want lead %s, saying %q", tc.name, window.OpensAt, window.LeadSeconds, window.Why, lead, tc.says)
		}
	}
	if lead := time.Duration(expiryWindow(expires, now, 11, 6, demand(measured(1), 25)).LeadSeconds) * time.Second; lead < 60*time.Hour || lead > 61*time.Hour {
		t.Errorf("11 holders with 6 weeks in 25 accounts at 1.0 open %s before, want about 60 hours", lead)
	}
}

// Behaviour 6, the holders. What the holders of one kind and date have left
// is the sum of what each has left on the kind's meter, a whole week when
// that is unread, and a week more for each reset of the kind and date beyond
// the first. An account two pools hold counts once, from the first; a kind
// the table does not know and a date already past count nothing.
func TestTheHoldersHeadroom(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	ends := now.Add(17 * 24 * time.Hour).Format(time.RFC3339)
	held := func(kind accounts.ResetKind, count int, expires string) accounts.HeldReset {
		return accounts.HeldReset{Kind: kind, Count: count, ExpiresAt: expires, Expiry: accounts.ResetExpiryRead}
	}
	week, fable := accounts.ResetKindClaudeWeek, accounts.ResetKindClaudeFable
	holders := []resetHolder{
		{provider: "claude", id: "a", resets: []accounts.HeldReset{held(week, 1, ends), held(fable, 2, ends)},
			left: map[accounts.ResetMeter]float64{accounts.ResetMeterWeek: 0.25, accounts.ResetMeterFable: 0.5}},
		{provider: "claude", id: "b", resets: []accounts.HeldReset{held(week, 1, ends)}, left: map[accounts.ResetMeter]float64{}},
		{provider: "claude", id: "a", resets: []accounts.HeldReset{held(week, 1, ends)},
			left: map[accounts.ResetMeter]float64{accounts.ResetMeterWeek: 0.9}},
		{provider: "claude", id: "c", resets: []accounts.HeldReset{held(accounts.ResetKindUnknown, 1, ends),
			held(week, 1, now.Add(-time.Hour).Format(time.RFC3339))}, left: map[accounts.ResetMeter]float64{}},
	}
	expires, _ := held(week, 1, ends).Expires()
	got := resetHoldings(holders, now)
	key := func(kind accounts.ResetKind) resetHoldingKey {
		return resetHoldingKey{provider: "claude", kind: kind, expires: expires.UnixMicro()}
	}
	if weekHolding := got[key(week)]; weekHolding.holders != 2 || math.Abs(weekHolding.headroom-1.25) > 1e-9 {
		t.Errorf("the week resets' holders = %+v, want 2 holding 0.25 and an unread 1.0", weekHolding)
	}
	if fableHolding := got[key(fable)]; fableHolding.holders != 1 || math.Abs(fableHolding.headroom-1.5) > 1e-9 {
		t.Errorf("the Fable resets' holders = %+v, want 1 holding 0.5 and a week for the second reset", fableHolding)
	}
	if len(got) != 2 {
		t.Errorf("holdings %+v, want the week and the Fable resets only", got)
	}
}

// expiryWindowsOf is each row's expiry windows from usage-status, by expiry.
func expiryWindowsOf(t *testing.T, body []byte) map[string]map[string]*accounts.ResetExpiryWindow {
	t.Helper()
	var rows []struct {
		ID     string               `json:"id"`
		Resets []accounts.HeldReset `json:"resets"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("usage-status: %v: %s", err, body)
	}
	out := map[string]map[string]*accounts.ResetExpiryWindow{}
	for _, row := range rows {
		out[row.ID] = map[string]*accounts.ResetExpiryWindow{}
		for _, entry := range row.Resets {
			out[row.ID][string(entry.Kind)+" "+entry.ExpiresAt+" "+string(entry.Expiry)] = entry.ExpiryWindow
		}
	}
	return out
}

func usageStatusBody(t *testing.T, server Server, path string) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, loopbackRequest(http.MethodGet, path, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s = %d: %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

// Behaviour 6, through the daemon. Each of this daemon's own rows that holds
// a reset carries, per kind and expiry, when its window opens and why. The
// holders and the demand are the pool's: this daemon's accounts and its
// counted peer's. A peer's rows keep the peer's own values.
func TestUsageStatusCarriesEachResetsExpiryWindow(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	soon, later := base.Add(5*24*time.Hour), base.Add(12*24*time.Hour)
	expiry := func(at time.Time) string { return "codex_rate_limits " + at.Format(time.RFC3339) + " read" }
	span := 49 * time.Hour
	seed := func(d *creditDaemon, used map[string]float64) {
		h := burnHistory(t, base)
		d.server.AccountRef.SetUsageHistory(h)
		for id, now := range used {
			d.setUsed(id, now*100)
			// Each burns 1.0 and reads what its usage reading reads now.
			seedSeries(h, accounts.Account{ID: id, Provider: accounts.ProviderCodex}, base, span, 0,
				codexRising(line(now-weekShare(span), 1, span), fixedRoll(base.Add(3*burnDay))))
		}
		d.read(t)
	}

	// The box: one account, half its week left, one credit ending soon.
	box := newCreditDaemon(t, map[string]int{"box@example.com": 1},
		map[string][]map[string]any{"box@example.com": {codexCredit("RateLimitResetCredit_box", "available", soon)}})
	seed(box, map[string]float64{"box@example.com": 0.5})
	box.pass(base)
	boxOwn := expiryWindowsOf(t, usageStatusBody(t, box.server, "/_subrouter/usage-status"))["box@example.com"][expiry(soon)]
	if boxOwn == nil {
		t.Fatalf("the box publishes no window for its own credit")
	}

	// The laptop: one account at 40% used with a credit ending soon and two
	// ending later, one with a credit ending soon at 70%, and one with none.
	laptop := newCreditDaemon(t, map[string]int{creditAccount: 3, otherCreditAccount: 1, "none@example.com": 0},
		map[string][]map[string]any{
			creditAccount: {codexCredit("RateLimitResetCredit_a", "available", soon),
				codexCredit("RateLimitResetCredit_b", "available", later), codexCredit("RateLimitResetCredit_b2", "available", later)},
			otherCreditAccount: {codexCredit("RateLimitResetCredit_c", "available", soon)},
		})
	seed(laptop, map[string]float64{creditAccount: 0.4, otherCreditAccount: 0.7, "none@example.com": 0.4})
	laptop.pass(base)
	boxPool := NewRemotePool(box.proxy.URL, "", "")
	boxPool.Name = "box"
	laptop.server.RemotePeers = []*RemotePool{boxPool}
	// The laptop has read the box, as a routing decision does.
	if body := boxPool.usageForDecision(time.Now().Add(5 * time.Second)).Body; len(body) == 0 {
		t.Fatal("calibration: the laptop never read the box")
	}
	laptop.memory().now = func() time.Time { return base }
	windows := expiryWindowsOf(t, usageStatusBody(t, laptop.server, "/_subrouter/usage-status"))

	// Soon: 0.6 + 0.3 here and 0.5 on the box is 1.4 weeks over 3 holders;
	// 4 accounts at 1.0 spend 4 weeks a week: 1.5 x 1.4 / 4 weeks.
	// Later: one holder of two, which spends its 0.6 weeks and then a whole
	// week to use the second: 1.5 x 1.6 / 4 weeks.
	week := float64(usageMeterLength)
	for _, tc := range []struct {
		account string
		at      time.Time
		lead    time.Duration
		says    string
	}{
		{creditAccount, soon, time.Duration(1.5 * 1.4 / 4 * week), "3 holders have 1.4 weeks of the Codex week left between them; at the pool's 24-hour pressure 1.00 over 4 accounts"},
		{otherCreditAccount, soon, time.Duration(1.5 * 1.4 / 4 * week), "3 holders have 1.4 weeks"},
		{creditAccount, later, time.Duration(1.5 * 1.6 / 4 * week), "1 holder has 1.6 weeks"},
	} {
		got := windows[tc.account][expiry(tc.at)]
		lead := tc.lead.Round(time.Second)
		if got == nil || math.Abs(float64(got.LeadSeconds)-lead.Seconds()) > 2 || got.OpensAt.Sub(tc.at.Add(-lead)).Abs() > 2*time.Second ||
			!strings.Contains(got.Why, tc.says) {
			t.Errorf("%s's credit ending %s: window %+v; want it to open %s before, saying %q", tc.account, tc.at, got, lead, tc.says)
		}
	}
	if len(windows["none@example.com"]) != 0 {
		t.Errorf("an account with no reset lists %v", windows["none@example.com"])
	}

	// The box's row, as the laptop relays it, keeps the box's own window,
	// which counts only the box: 1.5 x 0.5 weeks is more than the 5 days left.
	var view PeersUsageStatus
	if err := json.Unmarshal(usageStatusBody(t, laptop.server, PeersUsageStatusPath), &view); err != nil || len(view.Peers) != 1 {
		t.Fatalf("the laptop's view of its peers: %+v (%v)", view, err)
	}
	boxRelayed := expiryWindowsOf(t, view.Peers[0].Accounts)["box@example.com"][expiry(soon)]
	if boxRelayed == nil || *boxRelayed != *boxOwn || boxOwn.LeadSeconds == windows[creditAccount][expiry(soon)].LeadSeconds ||
		!strings.Contains(boxOwn.Why, "so the window is open") {
		t.Errorf("the box's window as the laptop relays it = %+v, want the box's own %+v, which opens the window now", boxRelayed, boxOwn)
	}
}

// Behaviour 7. The Claude week kind's restarts_clock is a measured no, and
// usage-status publishes it; Fable stays assumed no and Codex assumed yes.
func TestTheClaudeWeekResetKeepsItsRollAsMeasured(t *testing.T) {
	for _, tc := range []struct {
		provider accounts.Provider
		kind     accounts.ResetKind
		want     accounts.ClockRestart
		meter    accounts.ResetMeter
	}{
		{accounts.ProviderClaude, accounts.ResetKindClaudeWeek, accounts.ClockRestartMeasuredNo, accounts.ResetMeterWeek},
		{accounts.ProviderClaude, accounts.ResetKindClaudeFable, accounts.ClockRestartAssumedNo, accounts.ResetMeterFable},
		{accounts.ProviderCodex, accounts.ResetKindCodexRateLimits, accounts.ClockRestartAssumedYes, accounts.ResetMeterWeek},
	} {
		spec, ok := accounts.ResetKindNamed(tc.provider, tc.kind)
		if !ok || spec.RestartsClock != tc.want || spec.Meter != tc.meter {
			t.Errorf("%s %s: restarts_clock %q on the %q meter, want %q on %q", tc.provider, tc.kind, spec.RestartsClock, spec.Meter, tc.want, tc.meter)
		}
	}
	now := time.Now()
	box, _, _ := claudeUsageBox(t, map[string]string{
		"a-both": resetHoldUsage(now, 50, cedarEmberBlock(fableGrant(1, fableGrantEnds(now)), decemberWeekGrant(1, now.Add(17*24*time.Hour)))),
	}, nil)
	body, _ := resetRows(t, box)
	var rows []struct {
		Resets []struct {
			Kind          string `json:"kind"`
			RestartsClock string `json:"restarts_clock"`
		} `json:"resets"`
	}
	if err := json.Unmarshal(body, &rows); err != nil || len(rows) != 1 {
		t.Fatalf("usage-status: %v: %s", err, body)
	}
	published := map[string]string{}
	for _, entry := range rows[0].Resets {
		published[entry.Kind] = entry.RestartsClock
	}
	if published["week"] != "measured_no" || published["fable"] != "assumed_no" {
		t.Errorf("usage-status lists restarts_clock %v, want the week reset measured_no and the Fable reset assumed_no", published)
	}
}
