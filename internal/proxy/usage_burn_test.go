package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

// PATCHES §92.2: burn per account, the pool figure and its trend, and the
// route that serves them.

const burnDay = 24 * time.Hour

// weekShare is the used fraction a meter spends in d at burn 1.
func weekShare(d time.Duration) float64 { return d.Hours() / (7 * 24) }

// seedSeries writes one reading every 10 minutes from from ago to to ago,
// with the windows windows gives for each.
func seedSeries(h *UsageHistory, account accounts.Account, now time.Time, from, to time.Duration,
	windows func(ago time.Duration, at time.Time) []accounts.UsageWindow) {
	for ago := from; ago >= to; ago -= 10 * time.Minute {
		at := now.Add(-ago)
		h.observe(account, windows(ago, at), at)
	}
}

// codexRising is a Codex week whose used fraction is used(ago), rolling at
// roll(ago).
func codexRising(used func(ago time.Duration) float64, roll func(ago time.Duration) time.Time) func(time.Duration, time.Time) []accounts.UsageWindow {
	return func(ago time.Duration, at time.Time) []accounts.UsageWindow {
		return codexWeekRead(used(ago)*100, int64(roll(ago).Sub(at)/time.Second), at)
	}
}

func fixedRoll(roll time.Time) func(time.Duration) time.Time {
	return func(time.Duration) time.Time { return roll }
}

// line rises from base at from ago by rate weeks of budget per week.
func line(base, rate float64, from time.Duration) func(time.Duration) float64 {
	return func(ago time.Duration) float64 { return base + rate*weekShare(from-ago) }
}

func burnHistory(t *testing.T, now time.Time) *UsageHistory {
	t.Helper()
	h, _, _ := testUsageHistory(t, withUsageHistoryClock(func() time.Time { return now }))
	return h
}

// burnReport is the route as a loopback reader gets it.
func burnReport(t *testing.T, server Server) UsageBurnReport {
	t.Helper()
	status, body := burnRoute(t, server)
	if status != http.StatusOK {
		t.Fatalf("%s = %d: %s", UsageBurnPath, status, body)
	}
	var report UsageBurnReport
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatalf("%s did not decode: %v\n%s", UsageBurnPath, err, body)
	}
	return report
}

func burnRoute(t *testing.T, server Server) (int, []byte) {
	t.Helper()
	srv := httptest.NewServer(server.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + UsageBurnPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func poolMeter(t *testing.T, report UsageBurnReport, provider, meter string) UsagePoolMeter {
	t.Helper()
	for _, m := range report.Meters {
		if m.Provider == provider && m.Meter == meter {
			return m
		}
	}
	t.Fatalf("the report has no %s %s meter: %+v", provider, meter, report.Meters)
	return UsagePoolMeter{}
}

func accountWindows(t *testing.T, meter UsagePoolMeter, id string) UsageMeterBurn {
	t.Helper()
	for _, account := range meter.Accounts {
		if account.ID == id {
			return account.Windows
		}
	}
	t.Fatalf("the %s meter has no account %s: %+v", meter.Label, id, meter.Accounts)
	return nil
}

// burnText prints a window's burn, or "short" without one.
func burnText(figure UsageBurnFigure) string {
	if figure.Burn == nil {
		return "short"
	}
	return fmt.Sprintf("%.3f", *figure.Burn)
}

// burnNear reports whether a published figure is there and within of want.
func burnNear(got *float64, want, within float64) bool {
	return got != nil && math.Abs(*got-want) <= within
}

// Behaviour 1. Burn is the rise inside the window over the window's share of
// the week: 1/7 of the week in 24 hours is 1.0, twice that 2.0, flat 0. A
// fall at a roll or a claim and a jump at a roll add nothing; a window the
// series covers less than half of has no figure.
func TestBurnIsTheRiseOverTheWindowsShareOfTheWeek(t *testing.T) {
	d := newUnstartedCodexDaemon(t, map[string][2]float64{
		"steady@example.com": {20, 300000}, "double@example.com": {20, 300000}, "flat@example.com": {20, 300000},
		"rolled@example.com": {20, 300000}, "jumped@example.com": {20, 300000}, "claimed@example.com": {20, 300000},
		"short@example.com": {20, 300000},
	})
	now := time.Now().Truncate(time.Second)
	h := burnHistory(t, now)
	d.server.AccountRef.SetUsageHistory(h)
	codex := func(id string) accounts.Account { return accounts.Account{ID: id, Provider: accounts.ProviderCodex} }
	roll := now.Add(3 * burnDay)
	span := 30 * time.Hour
	seedSeries(h, codex("steady@example.com"), now, span, 0, codexRising(line(0.20, 1, span), fixedRoll(roll)))
	seedSeries(h, codex("double@example.com"), now, span, 0, codexRising(line(0.20, 2, span), fixedRoll(roll)))
	seedSeries(h, codex("flat@example.com"), now, span, 0, codexRising(line(0.40, 0, span), fixedRoll(roll)))
	// The week rolls 12 hours ago: its used falls to nothing and its roll
	// moves on 7 days, and the new week rises at the same pace.
	rolledAt := 12 * time.Hour
	oldRoll := now.Add(-rolledAt - 5*time.Minute)
	rollAcross := func(ago time.Duration) time.Time {
		if ago > rolledAt {
			return oldRoll
		}
		return oldRoll.Add(7 * burnDay)
	}
	seedSeries(h, codex("rolled@example.com"), now, span, 0, codexRising(func(ago time.Duration) float64 {
		if ago > rolledAt {
			return line(0.80, 1, span)(ago)
		}
		return line(0, 1, rolledAt)(ago)
	}, rollAcross))
	// The same roll, where the new week reads more used than the old one did.
	seedSeries(h, codex("jumped@example.com"), now, span, 0, codexRising(func(ago time.Duration) float64 {
		if ago > rolledAt {
			return line(0.05, 1, span)(ago)
		}
		return line(0.60, 1, rolledAt)(ago)
	}, rollAcross))
	// A claim 6 hours ago gives back most of the week at the same roll.
	claimedAt := 6 * time.Hour
	claimed := codex("claimed@example.com")
	seedSeries(h, claimed, now, span, claimedAt, codexRising(line(0.80, 1, span), fixedRoll(roll)))
	h.finishClaim(h.startClaim(claimed, "codex_rate_limits", now.Add(-claimedAt+time.Minute)), "redeemed", true)
	seedSeries(h, claimed, now, claimedAt-10*time.Minute, 0, codexRising(line(0.05, 1, claimedAt), fixedRoll(roll)))
	// A series that started 11 hours ago covers the 6 hours, not the 24.
	seedSeries(h, codex("short@example.com"), now, 11*time.Hour, 0, codexRising(line(0.20, 1, 11*time.Hour), fixedRoll(roll)))

	week := poolMeter(t, burnReport(t, d.server), "codex", usageMeterWeek)
	type want struct {
		id               string
		hour, six, day   float64
		within           float64
		shortDayCoverage float64
	}
	// One 10-minute stretch of the 144 in 24 hours spans the roll or the
	// claim and counts nothing, so those read 1/144 under 1.
	for _, w := range []want{
		{id: "steady@example.com", hour: 1, six: 1, day: 1, within: 0.001},
		{id: "double@example.com", hour: 2, six: 2, day: 2, within: 0.001},
		{id: "flat@example.com", within: 0.001},
		{id: "rolled@example.com", hour: 1, six: 1, day: 1, within: 0.01},
		{id: "jumped@example.com", hour: 1, six: 1, day: 1, within: 0.01},
		{id: "claimed@example.com", hour: 1, six: 1, day: 1, within: 0.03},
	} {
		figures := accountWindows(t, week, w.id)
		if !burnNear(figures["1h"].Burn, w.hour, w.within) || !burnNear(figures["6h"].Burn, w.six, w.within) || !burnNear(figures["24h"].Burn, w.day, w.within) {
			t.Errorf("%s burns %s over 1h, %s over 6h, %s over 24h; want %.1f, %.1f, %.1f", w.id,
				burnText(figures["1h"]), burnText(figures["6h"]), burnText(figures["24h"]), w.hour, w.six, w.day)
		}
	}
	short := accountWindows(t, week, "short@example.com")
	if short["24h"].Burn != nil || math.Abs(short["24h"].Covered-11.0/24) > 0.01 {
		t.Errorf("a series 11 hours long reads %s over 24h with %.3f covered; want no figure, 0.458 covered", burnText(short["24h"]), short["24h"].Covered)
	}
	if !burnNear(short["6h"].Burn, 1, 0.001) {
		t.Errorf("a series 11 hours long reads %s over 6h, want 1.0", burnText(short["6h"]))
	}
}

// Behaviour 2 and the switch. usage-status carries each own account's
// figures per meter and window; a window too short has its coverage and no
// burn. With the history off no row carries any, and the route answers 404.
func TestUsageStatusPublishesEachAccountsBurn(t *testing.T) {
	readings := map[string][2]float64{"steady@example.com": {20, 300000}, "short@example.com": {20, 300000}}
	d := newUnstartedCodexDaemon(t, readings)
	now := time.Now().Truncate(time.Second)
	h := burnHistory(t, now)
	d.server.AccountRef.SetUsageHistory(h)
	roll := now.Add(3 * burnDay)
	seedSeries(h, accounts.Account{ID: "steady@example.com", Provider: accounts.ProviderCodex}, now, 49*time.Hour, 0,
		codexRising(line(0.10, 1, 49*time.Hour), fixedRoll(roll)))
	seedSeries(h, accounts.Account{ID: "short@example.com", Provider: accounts.ProviderCodex}, now, 20*time.Minute, 0,
		codexRising(line(0.10, 1, 20*time.Minute), fixedRoll(roll)))

	rows := usageStatusBurn(t, d.server)
	steady := rows["steady@example.com"]
	if len(steady) != 1 || len(steady[usageMeterWeek]) != len(usageBurnWindows) {
		t.Fatalf("the steady account's burn = %+v, want the week over %d windows", steady, len(usageBurnWindows))
	}
	for _, window := range usageBurnWindows {
		figure := steady[usageMeterWeek][window.name]
		if !burnNear(figure.Burn, 1, 0.001) || figure.Covered != 1 {
			t.Errorf("the steady account's %s = %+v (burn %s), want burn 1 over all of it", window.name, figure, burnText(figure))
		}
	}
	short := rows["short@example.com"][usageMeterWeek]
	if hour := short["1h"]; hour.Burn != nil || math.Abs(hour.Covered-20.0/60) > 0.01 {
		t.Errorf("a series 20 minutes long publishes %+v over 1h, want no burn and a third covered", hour)
	}
	if day := short["24h"]; day.Burn != nil || day.Covered > 0.02 {
		t.Errorf("a series 20 minutes long publishes %+v over 24h, want no burn", day)
	}

	off := newUnstartedCodexDaemon(t, readings)
	rec := httptest.NewRecorder()
	off.server.handleUsageStatus(rec, loopbackRequest(http.MethodGet, "/_subrouter/usage-status", ""))
	if strings.Contains(rec.Body.String(), `"burn"`) {
		t.Fatalf("with the history off usage-status carries burn: %s", rec.Body.String())
	}
	if status, body := burnRoute(t, off.server); status != http.StatusNotFound || !strings.Contains(string(body), "SUBROUTER_USAGE_HISTORY=0") {
		t.Fatalf("with the history off %s = %d %q, want 404 naming the switch", UsageBurnPath, status, body)
	}
}

func usageStatusBurn(t *testing.T, server Server) map[string]UsageBurn {
	t.Helper()
	rec := httptest.NewRecorder()
	server.handleUsageStatus(rec, loopbackRequest(http.MethodGet, "/_subrouter/usage-status", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("usage-status = %d: %s", rec.Code, rec.Body.String())
	}
	var rows []struct {
		ID   string    `json:"id"`
		Burn UsageBurn `json:"burn"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	out := map[string]UsageBurn{}
	for _, row := range rows {
		out[row.ID] = row.Burn
	}
	return out
}

// claudeBurnPeer is a peer whose one Claude account's usage-status row
// carries the figures a real history computes from its series.
func claudeBurnPeer(t *testing.T, now time.Time, id string, weekRate, fableRate float64) (*RemotePool, *httptest.Server) {
	t.Helper()
	h := burnHistory(t, now)
	roll := now.Add(3 * burnDay)
	seedSeries(h, accounts.Account{ID: id, Provider: accounts.ProviderClaude}, now, 49*time.Hour, 0,
		func(ago time.Duration, _ time.Time) []accounts.UsageWindow {
			return append(claudeWeek(100*line(0.10, weekRate, 49*time.Hour)(ago), roll),
				accounts.UsageWindow{Name: agentclaude.FableWindowName, UsedPercent: 100 * line(0.05, fableRate, 49*time.Hour)(ago),
					LimitWindowSeconds: 7 * 24 * 3600, ResetAt: roll})
		})
	ref := &AccountRef{}
	ref.SetUsageHistory(h)
	rows := ref.withUsageBurn([]AccountUsageStatus{{
		AccountStatus: AccountStatus{ID: id, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Label: "Opus seat", AuthChecked: true, AuthValid: true},
		Normalized:    &NormalizedUsage{Fresh: true},
	}})
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != remotePoolUsagePath {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, rows)
	}))
	t.Cleanup(peer.Close)
	pool := NewRemotePool(peer.URL, "", "")
	pool.Name = "claude-box"
	return pool, peer
}

// Behaviour 3. The route answers per meter over this daemon's accounts and
// every peer's: the pool burn over 1, 6 and 24 hours, the trend, the counts,
// the share at the limit and each account's figures. An account two pools
// hold is counted once, from here. A peer that publishes no burn is named as
// not counted, and its accounts are neither counted nor read as 0.
func TestTheBurnRouteCountsLocalAndPeerAccounts(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	roll := now.Add(3 * burnDay)
	span := 49 * time.Hour
	codex := func(id string) accounts.Account { return accounts.Account{ID: id, Provider: accounts.ProviderCodex} }

	// The box: a daemon of this build whose account spent at pace until a
	// day ago and twice the pace since, and which holds the laptop's steady
	// account too, read flat there.
	box := newUnstartedCodexDaemon(t, map[string][2]float64{"rising@example.com": {20, 300000}, "steady@example.com": {20, 300000}})
	boxHistory := burnHistory(t, now)
	box.server.AccountRef.SetUsageHistory(boxHistory)
	seedSeries(boxHistory, codex("rising@example.com"), now, span, 0, codexRising(func(ago time.Duration) float64 {
		if ago > burnDay {
			return line(0.10, 1, span)(ago)
		}
		return line(0.10+weekShare(span-burnDay), 2, burnDay)(ago)
	}, fixedRoll(roll)))
	seedSeries(boxHistory, codex("steady@example.com"), now, span, 0, codexRising(line(0.30, 0, span), fixedRoll(roll)))
	boxPool := NewRemotePool(box.proxy.URL, "", "")
	boxPool.Name = "box"

	old := codexBox(t, codexWeekScore("old@example.com", 50, 400000))
	old.pool.Name = "old-box"

	laptop := newUnstartedCodexDaemon(t, map[string][2]float64{
		"steady@example.com": {20, 300000}, "short@example.com": {20, 300000}, "newer@example.com": {20, 300000},
	})
	h := burnHistory(t, now)
	laptop.server.AccountRef.SetUsageHistory(h)
	seedSeries(h, codex("steady@example.com"), now, span, 0, codexRising(line(0.10, 1, span), fixedRoll(roll)))
	seedSeries(h, codex("short@example.com"), now, 11*time.Hour, 0, codexRising(line(0.40, 0, 11*time.Hour), fixedRoll(roll)))
	// A series 30 hours long has the last 24 hours and not the 24 before.
	seedSeries(h, codex("newer@example.com"), now, 30*time.Hour, 0, codexRising(line(0.05, 3, 30*time.Hour), fixedRoll(roll)))
	claudePeer, claudeServer := claudeBurnPeer(t, now, "opus@example.com", 0.5, 1.5)
	laptop.server.RemotePeers = []*RemotePool{boxPool, claudePeer, old.pool}

	report := burnReport(t, laptop.server)
	week := poolMeter(t, report, "codex", usageMeterWeek)
	// The last hour and 6 hours: the laptop's steady 1.0, flat short and newer
	// 3.0, and the box's 2.0. 24 hours: the short series has no figure.
	for _, c := range []struct {
		window       string
		burn         float64
		counted, few int
	}{{"1h", 1.5, 4, 0}, {"6h", 1.5, 4, 0}, {"24h", 2, 3, 1}} {
		got := week.Windows[c.window]
		if !burnNear(got.Burn, c.burn, 0.002) || got.Accounts != c.counted || got.NotEnoughHistory != c.few || !burnNear(got.AtLimit, 0, 0) {
			t.Errorf("Codex week over %s: %+v (burn %v); want %.1f over %d accounts, %d without enough history, none at the limit",
				c.window, got, got.Burn, c.burn, c.counted, c.few)
		}
	}
	// The box's account doubled its pace a day ago; nothing else changed.
	// The newer series has no day before, so it is not in the 24-hour trend.
	if trend := week.Trend["24h"]; !burnNear(trend.Change, 0.5, 0.002) || trend.Accounts != 2 {
		t.Errorf("Codex week 24h trend = %+v, want +0.5 over the 2 accounts with both days", trend)
	}
	if trend := week.Trend["6h"]; !burnNear(trend.Change, 0, 0.002) || trend.Accounts != 4 {
		t.Errorf("Codex week 6h trend = %+v, want 0 over 4 accounts", trend)
	}
	var held []string
	for _, account := range week.Accounts {
		held = append(held, account.Pool+" "+account.ID)
	}
	if want := []string{"local short@example.com", "local steady@example.com", "local newer@example.com", "box rising@example.com"}; !sameStrings(held, want) {
		t.Errorf("the Codex week counts %v, want %v: the shared account once, from here, and nothing of the old box", held, want)
	}
	if steady := accountWindows(t, week, "steady@example.com"); !burnNear(steady["24h"].Burn, 1, 0.002) {
		t.Errorf("the shared account reads %s over 24h, want this daemon's own 1.0", burnText(steady["24h"]))
	}

	claudeWeekMeter, fable := poolMeter(t, report, "claude", usageMeterWeek), poolMeter(t, report, "claude", usageMeterFable)
	if got := claudeWeekMeter.Windows["24h"]; !burnNear(got.Burn, 0.5, 0.002) || got.Accounts != 1 || claudeWeekMeter.Label != "Claude week" {
		t.Errorf("%s over 24h = %+v, want the peer's 0.5 over 1 account", claudeWeekMeter.Label, got)
	}
	if got := fable.Windows["24h"]; !burnNear(got.Burn, 1.5, 0.002) || got.Accounts != 1 || fable.Label != "Claude Fable" {
		t.Errorf("%s over 24h = %+v, want the peer's 1.5 over 1 account", fable.Label, got)
	}
	if got := fable.Accounts; len(got) != 1 || got[0].Label != "Opus seat" || got[0].Pool != "claude-box" {
		t.Errorf("the Fable accounts = %+v, want the peer's labelled account", got)
	}

	peers := map[string]UsageBurnPeer{}
	for _, peer := range report.Peers {
		peers[peer.Name] = peer
	}
	if p := peers["box"]; !p.Counted || p.Accounts != 2 {
		t.Errorf("the box = %+v, want counted with its 2 accounts", p)
	}
	if p := peers["claude-box"]; !p.Counted || p.Accounts != 1 {
		t.Errorf("the Claude peer = %+v, want counted with 1 account", p)
	}
	if p := peers["old-box"]; p.Counted || !strings.Contains(p.Reason, "publishes no burn") {
		t.Errorf("the old box = %+v, want not counted, saying it publishes no burn", p)
	}

	// The Claude peer goes away. Its last body is kept, and once that is more
	// than 15 minutes old its figures are no longer counted.
	claudeServer.Close()
	claudePeer.admin.mu.Lock()
	entry := claudePeer.admin.entries[remotePoolUsagePath]
	entry.at, entry.tried = entry.at.Add(-20*time.Minute), entry.tried.Add(-20*time.Minute)
	claudePeer.admin.entries[remotePoolUsagePath] = entry
	claudePeer.admin.mu.Unlock()
	gone := burnReport(t, laptop.server)
	for _, p := range gone.Peers {
		if p.Name == "claude-box" && (p.Counted || !strings.Contains(p.Reason, "last usage reading is 20m")) {
			t.Errorf("the Claude peer 20 minutes after its last body = %+v, want not counted, naming the age", p)
		}
	}
	if fable := poolMeter(t, gone, "claude", usageMeterFable); len(fable.Accounts) != 0 || fable.Windows["24h"].Burn != nil {
		t.Errorf("with the Claude peer's body 20 minutes old the Fable meter = %+v, want no accounts and no figure", fable)
	}
}

// sameStrings compares two lists as sets of lines.
func sameStrings(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// Behaviour 4. An account at its limit the whole window burns 0 and is
// counted at the limit, so a pool of spent accounts reads spent, not under
// pace.
func TestASpentPoolReadsAtTheLimitNotUnderPace(t *testing.T) {
	d := newUnstartedCodexDaemon(t, map[string][2]float64{"spent@example.com": {100, 300000}, "midway@example.com": {100, 300000}})
	now := time.Now().Truncate(time.Second)
	h := burnHistory(t, now)
	d.server.AccountRef.SetUsageHistory(h)
	roll := now.Add(3 * burnDay)
	span := 25 * time.Hour
	seedSeries(h, accounts.Account{ID: "spent@example.com", Provider: accounts.ProviderCodex}, now, span, 0,
		codexRising(line(1, 0, span), fixedRoll(roll)))

	spent := poolMeter(t, burnReport(t, d.server), "codex", usageMeterWeek)
	for _, window := range usageBurnPoolWindows {
		figure := accountWindows(t, spent, "spent@example.com")[window]
		pool := spent.Windows[window]
		if !burnNear(figure.Burn, 0, 0) || figure.AtLimit != 1 || !burnNear(pool.Burn, 0, 0) || !burnNear(pool.AtLimit, 1, 0) || pool.Accounts != 1 {
			t.Errorf("a spent pool over %s: the account %+v (burn %s), the pool %+v; want burn 0 with all of it at the limit",
				window, figure, burnText(figure), pool)
		}
	}

	// A second account spends at pace until it reaches its limit 12 hours
	// ago. It reads 99.5 % or more from 50 minutes before that, so 12h50m of
	// the 24 hours count at the limit.
	reached := 12 * time.Hour
	seedSeries(h, accounts.Account{ID: "midway@example.com", Provider: accounts.ProviderCodex}, now, span, 0,
		codexRising(func(ago time.Duration) float64 {
			return math.Min(1, line(1-weekShare(span-reached), 1, span)(ago))
		}, fixedRoll(roll)))
	both := poolMeter(t, burnReport(t, d.server), "codex", usageMeterWeek)
	midway := accountWindows(t, both, "midway@example.com")["24h"]
	atLimit := (12*time.Hour + 50*time.Minute).Hours() / 24
	if !burnNear(midway.Burn, 0.5, 0.002) || math.Abs(midway.AtLimit-atLimit) > 0.002 {
		t.Errorf("an account spent 12 hours ago reads %+v (burn %s) over 24h, want 0.5 with %.3f of it at the limit", midway, burnText(midway), atLimit)
	}
	if pool := both.Windows["24h"]; !burnNear(pool.Burn, 0.25, 0.002) || !burnNear(pool.AtLimit, (1+atLimit)/2, 0.002) || pool.Accounts != 2 {
		t.Errorf("the pool over 24h = %+v, want burn 0.25 over 2 accounts with %.3f of its account-time at the limit", pool, (1+atLimit)/2)
	}
}

// servedFrom is the account the fake upstreams name in a completed turn.
func servedFrom(body string) string {
	_, after, ok := strings.Cut(body, "served-from-")
	if !ok {
		return ""
	}
	if end := strings.IndexAny(after, `"\`); end >= 0 {
		return after[:end]
	}
	return after
}

// Behaviour 6. Nothing ranks or routes on the figures. A laptop and a box,
// each with figures for its own accounts, rank the board, decide the cross-pool
// verdict and place new sessions exactly as the same pair with the history off.
func TestSelectionIsTheSameWithBurnPublished(t *testing.T) {
	type outcome struct {
		ranks  []string
		spill  string
		served []string
	}
	run := func(on bool) outcome {
		now := time.Now().Truncate(time.Second)
		box := newUnstartedCodexDaemon(t, map[string][2]float64{"box-a@example.com": {10, 500000}, "box-b@example.com": {85, 40000}})
		laptop := newUnstartedCodexDaemon(t, map[string][2]float64{"lap-a@example.com": {20, 300000}, "lap-b@example.com": {60, 100000}})
		if on {
			for d, rates := range map[*unstartedCodexDaemon]map[string]float64{
				box:    {"box-a@example.com": 0.5, "box-b@example.com": 2},
				laptop: {"lap-a@example.com": 1, "lap-b@example.com": 3},
			} {
				h := burnHistory(t, now)
				d.server.AccountRef.SetUsageHistory(h)
				for id, rate := range rates {
					seedSeries(h, accounts.Account{ID: id, Provider: accounts.ProviderCodex}, now, 25*time.Hour, 0,
						codexRising(line(0.05, rate, 25*time.Hour), fixedRoll(now.Add(5*burnDay))))
				}
			}
		}
		pool := NewRemotePool(box.proxy.URL, "", "")
		pool.Name = "box"
		laptop.server.RemotePool = pool
		laptop.read(t)
		published := 0
		for _, d := range []*unstartedCodexDaemon{box, laptop} {
			for _, burn := range usageStatusBurn(t, d.server) {
				if burn[usageMeterWeek]["24h"].Burn != nil {
					published++
				}
			}
		}
		if want := map[bool]int{true: 4, false: 0}[on]; published != want {
			t.Fatalf("calibration: with the history on=%v, %d accounts publish a 24-hour burn, want %d", on, published, want)
		}
		var out outcome
		board := laptop.poolBoard(t)
		for _, row := range board.Rows {
			out.ranks = append(out.ranks, fmt.Sprintf("%s %v rank=%d %s", row.ID, row.Pools, row.Rank, row.Reason))
		}
		spill := codexSpill(t, board)
		out.spill = fmt.Sprintf("prefer_peer=%v target=%s/%s clause=%q local_best=%s", spill.PreferPeer, spill.TargetPeer, spill.TargetAccount, spill.Clause, spill.LocalBest)
		srv := httptest.NewServer(laptop.server.Handler())
		defer srv.Close()
		for i := range 3 {
			status, body := codexEgressPost(t, srv.URL, fmt.Sprintf("burn-ranks-nothing-%d", i))
			if status != http.StatusOK {
				t.Fatalf("new session %d: %d %.200q", i, status, body)
			}
			out.served = append(out.served, servedFrom(body))
		}
		return out
	}
	on, off := run(true), run(false)
	if len(on.ranks) != 4 || len(on.served) != 3 || on.served[0] == "" {
		t.Fatalf("calibration: with the figures published the laptop ranks %v and served %v", on.ranks, on.served)
	}
	if !reflect.DeepEqual(on, off) {
		t.Fatalf("with the figures published the laptop ranks\n%+v\nand with the history off\n%+v", on, off)
	}
}
