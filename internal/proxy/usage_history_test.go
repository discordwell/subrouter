package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// PATCHES §92.1: the durable usage history, its segments and the claim
// records.

func testUsageHistory(t *testing.T, options ...usageHistoryOption) (*UsageHistory, *syncBuffer, string) {
	t.Helper()
	logs := &syncBuffer{}
	path := filepath.Join(t.TempDir(), UsageHistoryFile)
	return newUsageHistory(path, slog.New(slog.NewJSONHandler(logs, nil)), options...), logs, path
}

func historyPoints(h *UsageHistory, provider, account, meter string) []usageMeterReading {
	h.mu.Lock()
	defer h.mu.Unlock()
	if series := h.series[usageSeriesKey{provider, account, meter}]; series != nil {
		return append([]usageMeterReading(nil), series.points...)
	}
	return nil
}

func historyAccounts(h *UsageHistory) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for key := range h.series {
		if !slices.Contains(out, key.account) {
			out = append(out, key.account)
		}
	}
	slices.Sort(out)
	return out
}

func historyClaims(h *UsageHistory) []usageClaim {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]usageClaim(nil), h.claims...)
}

func historyFileLines(t *testing.T, path string) []usageHistoryLine {
	t.Helper()
	lines, err := readUsageHistoryFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return lines
}

func claudeWeek(used float64, roll time.Time) []accounts.UsageWindow {
	return []accounts.UsageWindow{{Name: "7d", UsedPercent: used, LimitWindowSeconds: 7 * 24 * 3600, ResetAt: roll}}
}

// codexWeekRead is a Codex week as a usage read taken at at reports it.
func codexWeekRead(used float64, resetAfter int64, at time.Time) []accounts.UsageWindow {
	return []accounts.UsageWindow{{Name: "primary", UsedPercent: used, LimitWindowSeconds: 7 * 24 * 3600,
		ResetAfterSeconds: resetAfter, ResetAt: at.Add(time.Duration(resetAfter) * time.Second)}}
}

func timesWithin(a, b time.Time, within time.Duration) bool {
	d := a.Sub(b)
	return d <= within && d >= -within
}

// Behaviour 1. Over the daemon's own usage read against a fake whose readings
// change: one point per changed reading, at the reading's own time, with the
// roll time the reading gives.
func TestTheSeriesHoldsOnePointPerChangedReadingAtTheReadingsOwnTime(t *testing.T) {
	d := newUnstartedCodexDaemon(t, map[string][2]float64{midweekAccount: {20, 300000}})
	h, _, _ := testUsageHistory(t)
	d.server.AccountRef.SetUsageHistory(h)
	// Each read answers 300 ms after it was sent, so a point stamped when
	// the answer came, not when the read went out, is told apart.
	inner := d.server.AccountRef.client.Transport
	d.server.AccountRef.client = &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		time.Sleep(300 * time.Millisecond)
		return inner.RoundTrip(req)
	})}
	ctx := context.Background()
	measured := func() time.Time {
		t.Helper()
		for _, status := range d.server.AccountRef.UsageStatuses(ctx) {
			if status.ID == midweekAccount {
				return status.UsageMeasuredAt
			}
		}
		t.Fatal("no usage row for the account")
		return time.Time{}
	}
	week := func() []usageMeterReading { return historyPoints(h, "codex", midweekAccount, usageMeterWeek) }

	d.read(t)
	first := measured()
	points := week()
	if len(points) != 1 || !timesWithin(time.UnixMicro(points[0].At), first, 100*time.Millisecond) || points[0].Used != 0.20 || points[0].Unstarted {
		t.Fatalf("after the first reading the series is %+v, want one point at the reading's time %v, 20 %% used", points, first)
	}
	if roll := time.Unix(points[0].Roll, 0); !timesWithin(roll, first.Add(300000*time.Second), 2*time.Second) {
		t.Fatalf("the point rolls at %v, want the reading's time plus its 300000 s, %v", roll, first.Add(300000*time.Second))
	}

	// The next sweep reads the account live again and is told the same. A
	// cached reading never reaches the series: only live reads feed it.
	d.server.AccountRef.InvalidateUsageStatusCache()
	d.server.updateSchedulerFromUsageStatusesContext(ctx, d.server.AccountRef.UsageStatuses(ctx))
	if again := week(); !reflect.DeepEqual(again, points) {
		t.Fatalf("the cached reading served again changed the series: %+v, was %+v", again, points)
	}
	// A new reading that says the same adds no point.
	d.read(t)
	if same := week(); !reflect.DeepEqual(same, points) {
		t.Fatalf("an unchanged reading added a point: %+v", same)
	}
	d.setReading(midweekAccount, 25, 299000)
	d.read(t)
	changed := measured()
	points = week()
	if len(points) != 2 || !timesWithin(time.UnixMicro(points[1].At), changed, 100*time.Millisecond) || points[1].Used != 0.25 {
		t.Fatalf("after a changed reading the series is %+v, want a second point at %v, 25 %% used", points, changed)
	}
}

// Behaviour 1. A meter whose readings do not change is still written once
// every 10 minutes while readings arrive.
func TestAnUnchangedMeterIsWrittenAtLeastEveryTenMinutes(t *testing.T) {
	h, _, _ := testUsageHistory(t)
	account := accounts.Account{ID: "held", Provider: accounts.ProviderClaude}
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	roll := start.Add(48 * time.Hour)
	for _, step := range []struct {
		after time.Duration
		used  float64
	}{{0, 40}, {4 * time.Minute, 40}, {9 * time.Minute, 40}, {11 * time.Minute, 40}, {12 * time.Minute, 41}, {13 * time.Minute, 41}} {
		h.observe(account, claudeWeek(step.used, roll), start.Add(step.after))
	}
	var got []time.Duration
	for _, point := range historyPoints(h, "claude", "held", usageMeterWeek) {
		got = append(got, time.UnixMicro(point.At).Sub(start))
		if point.Roll != roll.Unix() {
			t.Fatalf("point %+v rolls at %d, want %d", point, point.Roll, roll.Unix())
		}
	}
	if want := []time.Duration{0, 11 * time.Minute, 12 * time.Minute}; !slices.Equal(got, want) {
		t.Fatalf("points at %v after the first reading, want %v", got, want)
	}
}

// Behaviour 2. A second daemon on the same state directory loads what the
// first wrote; a point two workers both wrote is stored once.
func TestTheSeriesSurvivesARestartAndAPointWrittenTwiceIsStoredOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), UsageHistoryFile)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	account := accounts.Account{ID: "worker@example.com", Provider: accounts.ProviderCodex}
	at := time.Now().Add(-time.Hour)
	// A retiring worker and the one replacing it read the same reading.
	retiring, replacing := newUsageHistory(path, logger), newUsageHistory(path, logger)
	retiring.observe(account, codexWeekRead(30, 200000, at), at)
	replacing.observe(account, codexWeekRead(30, 200000, at), at)
	retiring.observe(account, codexWeekRead(35, 199700, at.Add(5*time.Minute)), at.Add(5*time.Minute))
	for _, h := range []*UsageHistory{retiring, replacing} {
		if err := h.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	if lines := historyFileLines(t, path); len(lines) != 3 {
		t.Fatalf("calibration: the two workers wrote %d lines, want 3 (one point twice)", len(lines))
	}

	restarted := newUsageHistory(path, logger)
	points := historyPoints(restarted, "codex", account.ID, usageMeterWeek)
	if len(points) != 2 || points[0].At != at.UnixMicro() || points[0].Used != 0.30 ||
		points[1].At != at.Add(5*time.Minute).UnixMicro() || points[1].Used != 0.35 {
		t.Fatalf("the restarted daemon loaded %+v, want the two points once each", points)
	}
	if lines := historyFileLines(t, path); len(lines) != 2 {
		t.Fatalf("after the restart the file holds %d lines, want 2", len(lines))
	}
	// It goes on from the loaded points: the same reading again, and an
	// unchanged one 2 minutes later, add nothing.
	restarted.observe(account, codexWeekRead(35, 199700, at.Add(5*time.Minute)), at.Add(5*time.Minute))
	restarted.observe(account, codexWeekRead(35, 199580, at.Add(7*time.Minute)), at.Add(7*time.Minute))
	if again := historyPoints(restarted, "codex", account.ID, usageMeterWeek); len(again) != 2 {
		t.Fatalf("after the restart the series is %+v, want the same two points", again)
	}
}

// Behaviour 2. When the daemon stops (its flush loop's context ends and it
// calls Close), what waits is written: the last points, and a claim still
// waiting for a reading after it, without one.
func TestStoppingTheDaemonWritesWhatWaits(t *testing.T) {
	h, logs, path := testUsageHistory(t)
	ctx, stop := context.WithCancel(context.Background())
	h.RunFlushLoop(ctx)
	held := accounts.Account{ID: "held", Provider: accounts.ProviderClaude}
	at := time.Now().Add(-time.Minute)
	h.observe(held, claudeWeek(99, at.Add(48*time.Hour)), at)
	h.finishClaim(h.startClaim(held, "week", time.Now()), "reset", true)
	stop()
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	var points, claims []usageHistoryLine
	for _, line := range historyFileLines(t, path) {
		if line.Claim != nil {
			claims = append(claims, line)
		} else {
			points = append(points, line)
		}
	}
	if len(points) != 1 || len(claims) != 1 {
		t.Fatalf("after the stop the file holds %d points and %d claims, want 1 and 1", len(points), len(claims))
	}
	if claim := claims[0].Claim; claim.Failed || len(claim.After) != 0 || len(claim.Before) != 1 || claim.Before[0].Used != 0.99 {
		t.Fatalf("the claim written at stop: %+v, want the landed claim with its reading before and none after", claim)
	}
	if lines := jsonLogLines(logs, "reset claim recorded"); len(lines) != 1 {
		t.Fatalf("logged %d claim lines at stop, want 1", len(lines))
	}
}

// Behaviour 3. Points older than 35 days are dropped, and readings that keep
// arriving keep the file under its size bound with the newest kept.
func TestTheSeriesKeepsThirtyFiveDaysUnderItsSizeBound(t *testing.T) {
	now := time.Now()
	clock := withUsageHistoryClock(func() time.Time { return now })
	h, _, path := testUsageHistory(t, clock)
	account := accounts.Account{ID: "held", Provider: accounts.ProviderClaude}
	for i, age := range []time.Duration{40 * 24 * time.Hour, 36 * 24 * time.Hour, 34 * 24 * time.Hour, 24 * time.Hour} {
		at := now.Add(-age)
		h.observe(account, claudeWeek(float64(10+i), at.Add(time.Hour)), at)
	}
	if err := h.Flush(); err != nil {
		t.Fatal(err)
	}
	restarted := newUsageHistory(path, slog.New(slog.NewJSONHandler(io.Discard, nil)), clock)
	var ages []time.Duration
	for _, point := range historyPoints(restarted, "claude", "held", usageMeterWeek) {
		ages = append(ages, now.Sub(time.UnixMicro(point.At)).Round(time.Hour))
	}
	if want := []time.Duration{34 * 24 * time.Hour, 24 * time.Hour}; !slices.Equal(ages, want) {
		t.Fatalf("the history kept points %v old, want %v", ages, want)
	}
	if lines := historyFileLines(t, path); len(lines) != 2 {
		t.Fatalf("the file kept %d lines, want 2", len(lines))
	}

	const bound = 4096
	bounded, _, boundedPath := testUsageHistory(t, clock, withUsageHistoryMaxBytes(bound))
	start := now.Add(-10 * time.Hour)
	var last time.Time
	for i := 0; i < 600; i++ {
		last = start.Add(time.Duration(i) * time.Minute)
		bounded.observe(account, claudeWeek(float64(i%97), now.Add(time.Hour)), last)
		if i%20 != 19 {
			continue
		}
		if err := bounded.Flush(); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(boundedPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > bound {
			t.Fatalf("after %d readings the file is %d bytes, over its bound of %d", i+1, info.Size(), bound)
		}
	}
	lines := historyFileLines(t, boundedPath)
	if len(lines) == 0 || lines[len(lines)-1].At != last.UnixMicro() {
		t.Fatalf("the bounded file lost its newest point: %d lines", len(lines))
	}
	if points := historyPoints(bounded, "claude", "held", usageMeterWeek); len(points) > len(lines) || points[len(points)-1].At != last.UnixMicro() {
		t.Fatalf("the bounded history holds %d points in memory for %d in the file", len(points), len(lines))
	}
}

// Behaviour 4. A peer's rows, which this daemon reads to place a new
// session and for its board, never enter its series.
func TestOnlyThisDaemonsOwnAccountsEnterTheSeries(t *testing.T) {
	d := newUnstartedCodexDaemon(t, map[string][2]float64{midweekAccount: {20, 300000}})
	box := codexBox(t)
	// The peer's rows carry their windows, as a real peer's usage-status does.
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != remotePoolUsagePath {
			box.serve(w, r)
			return
		}
		writeJSON(w, []AccountUsageStatus{{
			AccountStatus: AccountStatus{ID: "box-late", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, AuthChecked: true, AuthValid: true},
			Windows:       codexWeekRead(90, 400000, time.Now()),
			Normalized:    &NormalizedUsage{Fresh: true},
		}})
	}))
	t.Cleanup(peer.Close)
	d.server.RemotePool = NewRemotePool(peer.URL, "", "")
	h, _, _ := testUsageHistory(t)
	d.server.AccountRef.SetUsageHistory(h)
	laptop := httptest.NewServer(d.server.Handler())
	t.Cleanup(laptop.Close)
	d.read(t)
	if status, body := codexEgressPost(t, laptop.URL, "own-accounts-only"); status != http.StatusOK {
		t.Fatalf("new session: %d %.200q", status, body)
	}
	_ = d.usageStatus(t)
	if _, ok := d.board(t)["box-late"]; !ok {
		t.Fatalf("calibration: the board did not read the peer's account: %v", d.board(t))
	}
	if got := historyAccounts(h); !slices.Equal(got, []string{midweekAccount}) {
		t.Fatalf("the series holds accounts %v, want only this daemon's own %v", got, []string{midweekAccount})
	}
}

// Behaviour 5. A roll, a recorded claim and a fall with neither each start a
// segment with that cause; a claim that failed explains nothing.
func TestSegmentsStartAtARollAClaimAndADrop(t *testing.T) {
	h, _, _ := testUsageHistory(t)
	causes := func(provider, account, meter string) ([]string, []int) {
		var names []string
		var sizes []int
		for _, segment := range h.segments(provider, account, meter) {
			names, sizes = append(names, segment.Cause), append(sizes, len(segment.Points))
		}
		return names, sizes
	}
	held := accounts.Account{ID: "held", Provider: accounts.ProviderClaude}
	start := time.Now().Add(-10 * time.Hour).Truncate(time.Second)
	first := start.Add(2 * time.Hour)
	second := first.Add(7 * 24 * time.Hour)
	h.observe(held, claudeWeek(50, first), start)
	h.observe(held, claudeWeek(60, first), start.Add(time.Hour))
	h.observe(held, claudeWeek(2, second), start.Add(3*time.Hour))
	h.observe(held, claudeWeek(30, second), start.Add(4*time.Hour))
	h.finishClaim(h.startClaim(held, "week", start.Add(4*time.Hour+time.Minute)), "reset", true)
	h.observe(held, claudeWeek(0, second), start.Add(4*time.Hour+2*time.Minute))
	h.observe(held, claudeWeek(10, second), start.Add(5*time.Hour))
	h.observe(held, claudeWeek(5, second), start.Add(6*time.Hour))
	h.finishClaim(h.startClaim(held, "week", start.Add(6*time.Hour+time.Minute)), "not_limited", false)
	h.observe(held, claudeWeek(1, second), start.Add(7*time.Hour))
	names, sizes := causes("claude", "held", usageMeterWeek)
	if want := []string{"", usageSegmentRoll, usageSegmentClaim, usageSegmentDrop, usageSegmentDrop}; !slices.Equal(names, want) || !slices.Equal(sizes, []int{2, 2, 2, 1, 1}) {
		t.Fatalf("Claude segments %v of %v points, want %v of [2 2 2 1 1]", names, sizes, want)
	}

	// A Codex week idle to its roll, whose next week reads more used at a
	// roll 7 days on: a roll with no fall. Then a redeem that leaves the
	// week unstarted.
	codex := accounts.Account{ID: "cooked@example.com", Provider: accounts.ProviderCodex}
	read := func(used float64, resetAfter int64, after time.Duration) {
		at := start.Add(after)
		h.observe(codex, codexWeekRead(used, resetAfter, at), at)
	}
	read(0, 1800, 0)
	read(5, 7*24*3600-1200, time.Hour)
	read(100, 7*24*3600-1200-7200, 3*time.Hour)
	h.finishClaim(h.startClaim(codex, "codex_rate_limits", start.Add(3*time.Hour+time.Minute)), "redeemed", true)
	read(0, 7*24*3600, 3*time.Hour+2*time.Minute)
	names, sizes = causes("codex", codex.ID, usageMeterWeek)
	if want := []string{"", usageSegmentRoll, usageSegmentClaim}; !slices.Equal(names, want) || !slices.Equal(sizes, []int{1, 2, 1}) {
		t.Fatalf("Codex segments %v of %v points, want %v of [1 2 1]", names, sizes, want)
	}
}

// claudeClaimUsage answers fake's usage reads as a claim leaves them, after
// §64's measured claim: before any claim the week is spent and the Fable
// meter at 116 %, after it the week reads 42 % and the Fable meter 0, both
// rolling at roll. Every other request goes to fake.
func claudeClaimUsage(fake *anthropicFake, roll time.Time) *http.Client {
	inner := fake.client().Transport
	return &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/api/oauth/usage" {
			return inner.RoundTrip(req)
		}
		week, fable := 100.0, 116.0
		if fake.claimCount() > 0 {
			week, fable = 42, 0
		}
		fake.mu.Lock()
		block := fake.block
		fake.mu.Unlock()
		resets := roll.UTC().Format(time.RFC3339)
		body := fmt.Sprintf(`{"seven_day":{"utilization":%v,"resets_at":%q},"seven_day_oauth_apps":{"utilization":%v,"resets_at":%q}`, week, resets, fable, resets)
		if block != "" {
			body += `,"cedar_ember":` + block
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(body + "}")), Request: req}, nil
	})}
}

func logGroup(line map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		next, _ := line[key].(map[string]any)
		line = next
	}
	return line
}

// Behaviour 6. A Claude claim through the daemon's handler records each
// meter's used and roll before it and after the first reading that follows,
// and one Info line says the same.
func TestAClaudeClaimRecordsEachMeterBeforeAndAfter(t *testing.T) {
	roll := time.Now().Add(3 * 24 * time.Hour).Truncate(time.Second)
	fake := &anthropicFake{
		t:          t,
		block:      resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour)),
		claimBody:  `{"result":"reset","resets_left":0,"cleared":["five_hour","seven_day"]}`,
		afterClaim: resetBlock(true, false, false, false, 0, time.Now().Add(20*24*time.Hour)),
	}
	server, _ := resetDaemon(t, fake)
	server.AccountRef.client = claudeClaimUsage(fake, roll)
	h, logs, path := testUsageHistory(t)
	server.AccountRef.SetUsageHistory(h)
	sent := time.Now()
	code, response := postClaudeReset(t, server.Handler(), claimJSON(""))
	if code != http.StatusOK || response.Result != "reset" {
		t.Fatalf("claim: %d %+v", code, response)
	}
	claims := historyClaims(h)
	if len(claims) != 1 {
		t.Fatalf("recorded %d claims, want 1", len(claims))
	}
	claim := claims[0]
	if claim.Provider != "claude" || claim.Kind != "week" || claim.Result != "reset" || claim.Failed || claim.At < sent.UnixMicro() {
		t.Fatalf("claim record %+v", claim)
	}
	meters := func(readings []usageMeterReading) map[string]usageMeterReading {
		out := map[string]usageMeterReading{}
		for _, reading := range readings {
			out[reading.Meter] = reading
		}
		return out
	}
	before, after := meters(claim.Before), meters(claim.After)
	for meter, want := range map[string][2]float64{usageMeterWeek: {1, 0.42}, usageMeterFable: {1.16, 0}} {
		b, a := before[meter], after[meter]
		if b.Used != want[0] || b.Roll != roll.Unix() || b.At >= claim.At || b.At < sent.UnixMicro() {
			t.Errorf("%s before the claim: %+v, want %.2f used rolling at %d, read after %d and before the claim at %d", meter, b, want[0], roll.Unix(), sent.UnixMicro(), claim.At)
		}
		if a.Used != want[1] || a.Roll != roll.Unix() || a.At < claim.At {
			t.Errorf("%s after the claim: %+v, want %.2f used rolling at %d, read after the claim at %d", meter, a, want[1], roll.Unix(), claim.At)
		}
	}
	lines := jsonLogLines(logs, "reset claim recorded")
	if len(lines) != 1 {
		t.Fatalf("logged %d claim lines, want 1:\n%s", len(lines), logs.String())
	}
	line := lines[0]
	if line["level"] != "INFO" || line["account"] != claim.Account || line["kind"] != "week" || line["result"] != "reset" || line["failed"] != false {
		t.Fatalf("claim line %v", line)
	}
	for meter, want := range map[string][2]float64{usageMeterWeek: {1, 0.42}, usageMeterFable: {1.16, 0}} {
		b, a := logGroup(line, "before", meter), logGroup(line, "after", meter)
		if b["used"] != want[0] || a["used"] != want[1] || b["roll"] != roll.UTC().Format(time.RFC3339) || a["roll"] != b["roll"] {
			t.Errorf("claim line's %s: before %v after %v, want used %.2f then %.2f rolling at %s", meter, b, a, want[0], want[1], roll.UTC().Format(time.RFC3339))
		}
	}
	if err := h.Flush(); err != nil {
		t.Fatal(err)
	}
	var written []usageClaim
	for _, line := range historyFileLines(t, path) {
		if line.Claim != nil {
			written = append(written, *line.Claim)
		}
	}
	if !reflect.DeepEqual(written, claims) {
		t.Fatalf("the file holds claims %+v, want %+v", written, claims)
	}
}

func codexRedeemDaemon(t *testing.T, transport http.RoundTripper) (Server, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	stored := proxyStoredOAuthAccount("oauth-identity@example.com", "fresh", time.Now().Add(time.Hour))
	stored.Email = "stable-routing-id"
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	return Server{AccountRef: NewAccountRef(store, nil, &http.Client{Transport: transport}), MaxBodyBytes: 1024}, "stable-routing-id"
}

func postCodexRedeem(t *testing.T, server Server, account string) RateLimitResetResult {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, loopbackAdminRequest(http.MethodPost, "/_subrouter/rate-limit-reset?email="+account, nil))
	var payload struct {
		Results []RateLimitResetResult `json:"results"`
	}
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &payload) != nil || len(payload.Results) != 1 {
		t.Fatalf("redeem: %d %s", recorder.Code, recorder.Body.String())
	}
	return payload.Results[0]
}

// Behaviour 6. A Codex redeem records the week before it and the reading
// after, where the fake provider restarts the 7 days.
func TestACodexRedeemRecordsTheWeekBeforeAndTheRestartedWeekAfter(t *testing.T) {
	var consume int
	server, id := codexRedeemDaemon(t, whamResponder(t, &consume, true))
	h, logs, _ := testUsageHistory(t)
	server.AccountRef.SetUsageHistory(h)
	sent := time.Now()
	result := postCodexRedeem(t, server, id)
	done := time.Now()
	if !result.Reset || consume != 1 {
		t.Fatalf("redeem: %+v, %d consumes", result, consume)
	}
	claims := historyClaims(h)
	if len(claims) != 1 {
		t.Fatalf("recorded %d claims, want 1", len(claims))
	}
	claim := claims[0]
	if claim.Provider != "codex" || claim.Account != id || claim.Kind != "codex_rate_limits" || claim.Result != "redeemed" || claim.Failed ||
		len(claim.Before) != 1 || len(claim.After) != 1 {
		t.Fatalf("claim record %+v", claim)
	}
	before, after := claim.Before[0], claim.After[0]
	if before.Meter != usageMeterWeek || before.Used != 1 || before.Unstarted || before.At >= claim.At || before.At < sent.UnixMicro() ||
		!timesWithin(time.Unix(before.Roll, 0), time.UnixMicro(before.At).Add(250000*time.Second), 2*time.Second) {
		t.Errorf("before the redeem: %+v, want the week spent, rolling 250000 s after its reading", before)
	}
	if after.Meter != usageMeterWeek || after.Used != 0 || !after.Unstarted || after.At < claim.At || after.At > done.UnixMicro() ||
		!timesWithin(time.Unix(after.Roll, 0), time.UnixMicro(after.At).Add(7*24*time.Hour), 2*time.Second) {
		t.Errorf("after the redeem: %+v, want an unstarted week of a whole 7 days, read after the redeem", after)
	}
	lines := jsonLogLines(logs, "reset claim recorded")
	if len(lines) != 1 || lines[0]["level"] != "INFO" || lines[0]["kind"] != "codex_rate_limits" ||
		logGroup(lines[0], "before", usageMeterWeek)["used"] != 1.0 || logGroup(lines[0], "after", usageMeterWeek)["used"] != 0.0 ||
		logGroup(lines[0], "after", usageMeterWeek)["unstarted"] != true {
		t.Fatalf("claim lines %v", lines)
	}
}

// Behaviour 6. A claim that fails is recorded as failed, with the before
// values only: a Codex consume the provider refused, and a Claude claim
// Anthropic answered not_limited.
func TestAFailedClaimRecordsTheBeforeValuesOnly(t *testing.T) {
	t.Run("codex", func(t *testing.T) {
		var consume int
		wham := whamResponder(t, &consume, true)
		refusing := proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == "/backend-api/wham/rate-limit-reset-credits/consume" {
				return &http.Response{StatusCode: http.StatusInternalServerError, Status: "500 Internal Server Error",
					Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")), Request: req}, nil
			}
			return wham(req)
		})
		server, id := codexRedeemDaemon(t, refusing)
		h, logs, _ := testUsageHistory(t)
		server.AccountRef.SetUsageHistory(h)
		if result := postCodexRedeem(t, server, id); result.Reset {
			t.Fatalf("calibration: the refused consume reset the account: %+v", result)
		}
		claims := historyClaims(h)
		if len(claims) != 1 || !claims[0].Failed || len(claims[0].After) != 0 || len(claims[0].Before) != 1 ||
			claims[0].Before[0].Used != 1 || !strings.Contains(claims[0].Result, "500") {
			t.Fatalf("claims %+v, want one failed claim with the spent week before and nothing after", claims)
		}
		if lines := jsonLogLines(logs, "reset claim recorded"); len(lines) != 1 || lines[0]["failed"] != true {
			t.Fatalf("claim lines %v", lines)
		}
	})
	t.Run("claude", func(t *testing.T) {
		roll := time.Now().Add(3 * 24 * time.Hour).Truncate(time.Second)
		fake := &anthropicFake{t: t, block: resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour)),
			claimBody: `{"result":"not_limited"}`}
		server, _ := resetDaemon(t, fake)
		server.AccountRef.client = claudeClaimUsage(fake, roll)
		h, _, _ := testUsageHistory(t)
		server.AccountRef.SetUsageHistory(h)
		if code, response := postClaudeReset(t, server.Handler(), claimJSON("")); code != http.StatusOK || response.Result != "not_limited" {
			t.Fatalf("claim: %d %+v", code, response)
		}
		claims := historyClaims(h)
		if len(claims) != 1 || !claims[0].Failed || claims[0].Result != "not_limited" || len(claims[0].After) != 0 || len(claims[0].Before) != 2 {
			t.Fatalf("claims %+v, want one failed claim with both meters before and nothing after", claims)
		}
	})
}

// Behaviour 7. A state directory that cannot be written logs one Warn and
// the daemon serves as before; a line that does not decode is skipped.
func TestAnUnwritableStateDirectoryWarnsOnceAndACorruptLineIsSkipped(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	h := newUsageHistory(filepath.Join(blocker, UsageHistoryFile), slog.New(slog.NewJSONHandler(logs, nil)))
	d := newUnstartedCodexDaemon(t, map[string][2]float64{midweekAccount: {20, 300000}})
	d.server.AccountRef.SetUsageHistory(h)
	for i := 0; i < 3; i++ {
		d.setReading(midweekAccount, float64(21+i), 300000)
		d.read(t)
		if err := h.Flush(); err == nil {
			t.Fatal("calibration: a flush under a regular file succeeded")
		}
	}
	if warns := jsonLogLines(logs, "usage history could not be written; the daemon keeps it in memory and serves as before"); len(warns) != 1 || warns[0]["level"] != "WARN" {
		t.Fatalf("logged %d write warnings, want one Warn:\n%s", len(warns), logs.String())
	}
	if row := d.usageStatus(t)[midweekAccount]; row == nil || row["fresh"] != true {
		t.Fatalf("usage-status with an unwritable history: %v", row)
	}
	if score := d.server.SchedulerRef.Get().ScoreFor(schedulerAccountProvider(accounts.ProviderCodex), midweekAccount); !score.Fresh || math.Abs(score.WeeklyRemaining-0.77) > 1e-9 {
		t.Fatalf("the daemon's score with an unwritable history: %+v", score)
	}

	path := filepath.Join(t.TempDir(), UsageHistoryFile)
	at := time.Now().Add(-time.Hour)
	good := func(used float64, after time.Duration) string {
		raw, _ := json.Marshal(usageHistoryLine{Provider: "claude", Account: "held",
			usageMeterReading: usageMeterReading{Meter: usageMeterWeek, At: at.Add(after).UnixMicro(), Used: used}})
		return string(raw)
	}
	content := strings.Join([]string{good(0.1, 0), `{"p":"claude","a":"held","m":"week","t":`, "not json at all", `{"m":"week"}`, good(0.2, time.Minute)}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded := newUsageHistory(path, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if points := historyPoints(loaded, "claude", "held", usageMeterWeek); len(points) != 2 || points[0].Used != 0.1 || points[1].Used != 0.2 {
		t.Fatalf("loaded %+v, want the two good points", points)
	}
}

// Behaviour 7. A damaged line far longer than any the history writes is
// skipped like any other: the good lines around it load, the file is
// compacted back under its bound, and nothing warns.
func TestAnOverlongLineIsSkippedAndTheFileStillCompacts(t *testing.T) {
	now := time.Now()
	clock := withUsageHistoryClock(func() time.Time { return now })
	const bound = 64 << 10
	path := filepath.Join(t.TempDir(), UsageHistoryFile)
	good := func(used float64, after time.Duration) string {
		raw, _ := json.Marshal(usageHistoryLine{Provider: "claude", Account: "held",
			usageMeterReading: usageMeterReading{Meter: usageMeterWeek, At: now.Add(-time.Hour + after).UnixMicro(), Used: used}})
		return string(raw)
	}
	content := good(0.1, 0) + "\n" + strings.Repeat("x", 2<<20) + "\n" + good(0.2, time.Minute) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	h := newUsageHistory(path, slog.New(slog.NewJSONHandler(logs, nil)), clock, withUsageHistoryMaxBytes(bound))
	if points := historyPoints(h, "claude", "held", usageMeterWeek); len(points) != 2 || points[0].Used != 0.1 || points[1].Used != 0.2 {
		t.Fatalf("loaded %+v past a 2 MiB line, want the two good points", points)
	}
	h.observe(accounts.Account{ID: "held", Provider: accounts.ProviderClaude}, claudeWeek(30, now.Add(time.Hour)), now)
	if err := h.Flush(); err != nil {
		t.Fatalf("a flush after a 2 MiB line: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > bound {
		t.Fatalf("the file is %d bytes, want it compacted under its bound of %d", info.Size(), bound)
	}
	if warns := jsonLogLines(logs, "usage history could not be written; the daemon keeps it in memory and serves as before"); len(warns) != 0 {
		t.Fatalf("a skipped line logged %d write warnings:\n%s", len(warns), logs.String())
	}
	restarted := newUsageHistory(path, slog.New(slog.NewJSONHandler(io.Discard, nil)), clock)
	if points := historyPoints(restarted, "claude", "held", usageMeterWeek); len(points) != 3 || points[2].Used != 0.30 {
		t.Fatalf("a restart loaded %+v, want the two good points and the new one", points)
	}
}

// Behaviour 8. With the history off (no history attached, as serve leaves it
// under SUBROUTER_USAGE_HISTORY=0) a redeem and the readings around it record
// nothing, and the redeem answers as it does with the history on.
func TestWithTheHistoryOffARedeemRecordsNothingAndAnswersAsBefore(t *testing.T) {
	results := map[bool]RateLimitResetResult{}
	for _, on := range []bool{true, false} {
		var consume int
		server, id := codexRedeemDaemon(t, whamResponder(t, &consume, true))
		var h *UsageHistory
		if on {
			h, _, _ = testUsageHistory(t)
			server.AccountRef.SetUsageHistory(h)
		}
		results[on] = postCodexRedeem(t, server, id)
		if on && len(historyClaims(h)) != 1 {
			t.Fatalf("calibration: with the history on, %d claims recorded", len(historyClaims(h)))
		}
		if !on && server.AccountRef.history() != nil {
			t.Fatal("a daemon with the history off holds one")
		}
	}
	strip := func(result RateLimitResetResult) RateLimitResetResult {
		for _, windows := range [][]accounts.UsageWindow{result.WindowsBefore, result.WindowsAfter} {
			for i := range windows {
				windows[i].ResetAt = time.Time{}
			}
		}
		return result
	}
	if on, off := strip(results[true]), strip(results[false]); !reflect.DeepEqual(on, off) {
		t.Fatalf("the redeem answered %+v with the history on and %+v with it off", on, off)
	}
	var none *UsageHistory
	none.observe(accounts.Account{ID: "held", Provider: accounts.ProviderClaude}, claudeWeek(10, time.Now().Add(time.Hour)), time.Now())
	none.finishClaim(none.startClaim(accounts.Account{ID: "held", Provider: accounts.ProviderClaude}, "week", time.Now()), "reset", true)
	if err := errors.Join(none.Flush(), none.Close()); err != nil {
		t.Fatal(err)
	}
}

// Behaviour 9. The same accounts and readings rank the same, Pick and
// PickSteady and every clause, with the history on and off.
func TestSelectionIsTheSameWithTheHistoryOnAndOff(t *testing.T) {
	readings := map[string][2]float64{
		freshAccount: {0, 604800}, midweekAccount: {20, 300000},
		"late@example.com": {85, 40000}, "spent@example.com": {100, 90000},
	}
	type ranking struct {
		pick, steady string
		explain      []string
	}
	rank := func(on bool) ranking {
		copied := map[string][2]float64{}
		for id, reading := range readings {
			copied[id] = reading
		}
		d := newUnstartedCodexDaemon(t, copied)
		if on {
			h, _, _ := testUsageHistory(t)
			d.server.AccountRef.SetUsageHistory(h)
		}
		d.read(t)
		candidates, _ := d.server.AccountRef.Snapshot()
		scheduler := d.server.SchedulerRef.Get()
		var out ranking
		if pick, err := scheduler.Pick(candidates); err == nil {
			out.pick = pick.ID
		}
		if steady, err := scheduler.PickSteady(candidates); err == nil {
			out.steady = steady.ID
		}
		for _, explain := range []func([]accounts.Account) ([]selectacct.Explanation, error){scheduler.Explain, scheduler.ExplainSteady} {
			explained, err := explain(candidates)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range explained {
				out.explain = append(out.explain, fmt.Sprintf("%s usable=%v %s", entry.Account.ID, entry.Usable, entry.Reason))
			}
		}
		return out
	}
	on, off := rank(true), rank(false)
	if on.pick == "" || len(on.explain) != 2*len(readings) {
		t.Fatalf("calibration: the ranking with the history on is %+v", on)
	}
	if !reflect.DeepEqual(on, off) {
		t.Fatalf("with the history on the daemon ranks\n%+v\nand with it off\n%+v", on, off)
	}
}
