package proxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	agentclaude "github.com/discordwell/subrouter/internal/agents/claude"
	"github.com/discordwell/subrouter/selectacct"
)

// PATCHES §64 through the real sweep, scheduler and usage-status publisher:
// an account's Fable meter is the usage answer's limits[] Fable row when the
// answer has one, whatever seven_day_oauth_apps says, and seven_day_oauth_apps
// when it has none. The claudefable verdict and §65's fable_week_spent follow
// the meter.
func TestTheFableMeterIsTheLimitsFableRowWhenTheAnswerHasOne(t *testing.T) {
	now := time.Now()
	fableRow := func(percent int) string {
		return `"limits":[{"kind":"weekly_scoped","group":"weekly","percent":` + strconv.Itoa(percent) +
			`,"resets_at":"` + now.Add(100*time.Hour).UTC().Format("2006-01-02T15:04:05.000Z") +
			`","severity":"normal","is_active":false,"scope":{"model":{"display_name":"Fable"}}}]`
	}
	usage := func(extra string) string {
		return claudeUsageJSON(now, 10, 150*time.Minute, 20, 100*time.Hour, extra)
	}
	box, _, _ := claudeUsageBox(t, map[string]string{
		"row-spent": usage(claudeFableBucket(now, 0, 100*time.Hour) + "," + fableRow(100)),
		"row-left":  usage(claudeFableBucket(now, 100, 100*time.Hour) + "," + fableRow(10)),
		"no-row":    usage(claudeFableBucket(now, 100, 100*time.Hour)),
	}, nil)
	rows := map[string]remotePoolAccountUsage{}
	for _, row := range readUsageRows(t, box) {
		rows[row.ID] = row
	}
	for id, want := range map[string]struct {
		fableEligible bool
		spent         float64
	}{
		"row-spent": {false, 1},
		"row-left":  {true, 0.1},
		"no-row":    {false, 1},
	} {
		row, ok := rows[id]
		if !ok {
			t.Fatalf("%s: no usage-status row in %v", id, rows)
		}
		fable, ok := row.Normalized.Pools[selectacct.ModelKey(agentclaude.FableFeature)]
		if !ok || fable.NewSession == nil {
			t.Fatalf("%s: no claudefable verdict in %+v", id, row.Normalized.Pools)
		}
		if fable.NewSession.Eligible != want.fableEligible {
			t.Errorf("%s: claudefable verdict %+v, want eligible=%v", id, *fable.NewSession, want.fableEligible)
		}
		if math.Abs(row.Normalized.FableWeekSpent-want.spent) > 1e-9 {
			t.Errorf("%s: fable_week_spent = %v, want %v", id, row.Normalized.FableWeekSpent, want.spent)
		}
	}
}

// The daemon says, at WARN, when an account's limits[] Fable row and its
// seven_day_oauth_apps differ by more than a point, naming both values; at
// most once an hour per account, and never for answers within a point.
func TestClaudeFableMeterDisagreementWarnsOncePerAccountPerHour(t *testing.T) {
	var logs bytes.Buffer
	now := time.Date(2026, 11, 9, 20, 0, 0, 0, time.UTC)
	saved := fableMeterDisagreement
	fableMeterDisagreement = &fableMeterWarner{
		now:    func() time.Time { return now },
		logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	t.Cleanup(func() { fableMeterDisagreement = saved })

	read := func(id string, rowPercent, appsPercent string) {
		t.Helper()
		body := `{"seven_day":{"utilization":42,"resets_at":"2030-01-02T00:00:00Z"},` +
			`"seven_day_oauth_apps":{"utilization":` + appsPercent + `,"resets_at":"2030-01-02T00:00:00Z"},` +
			`"limits":[{"kind":"weekly_scoped","group":"weekly","percent":` + rowPercent + `,"resets_at":"2030-01-02T00:00:00.000Z","severity":"normal","is_active":false,"scope":{"model":{"display_name":"Fable"}}}]}`
		client := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
		})}
		if _, err := fetchAccountUsageWindowsLive(context.Background(), client, accounts.Account{
			ID: id, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok",
		}); err != nil {
			t.Fatal(err)
		}
	}
	lines := func() []string {
		return strings.FieldsFunc(logs.String(), func(r rune) bool { return r == '\n' })
	}

	read("acct-a", "0", "1")    // within a point: the same meter
	read("acct-a", "58", "116") // differ: one WARN
	got := lines()
	if len(got) != 1 {
		t.Fatalf("logs after a within-a-point read and a disagreement:\n%s\nwant exactly one line", logs.String())
	}
	for _, want := range []string{"level=WARN", "account=acct-a", "limits_row_percent=58", "seven_day_oauth_apps_percent=116"} {
		if !strings.Contains(got[0], want) {
			t.Fatalf("warn line %q lacks %q", got[0], want)
		}
	}

	now = now.Add(59 * time.Minute)
	read("acct-a", "60", "116")
	read("acct-b", "0", "94")
	got = lines()
	if len(got) != 2 || !strings.Contains(got[1], "account=acct-b") {
		t.Fatalf("logs within the hour:\n%s\nwant acct-a quiet and acct-b's first line", logs.String())
	}

	now = now.Add(time.Minute)
	read("acct-a", "60", "116")
	if got = lines(); len(got) != 3 || !strings.Contains(got[2], "account=acct-a") {
		t.Fatalf("logs an hour after acct-a's first line:\n%s\nwant it logged again", logs.String())
	}

	read("two-points", "40", "42")
	if got = lines(); len(got) != 4 || !strings.Contains(got[3], "account=two-points") {
		t.Fatalf("logs after a two-point gap:\n%s\nwant one more line, for two-points", logs.String())
	}
}

// An answer with a Fable row and no seven_day_oauth_apps to compare it with
// (the key absent, or its utilization null) reads, its Fable window is the
// row, and nothing is logged.
func TestClaudeFableMeterIsQuietWithoutSevenDayOAuthApps(t *testing.T) {
	var logs bytes.Buffer
	saved := fableMeterDisagreement
	fableMeterDisagreement = &fableMeterWarner{
		logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	t.Cleanup(func() { fableMeterDisagreement = saved })

	for name, apps := range map[string]string{
		"absent":           ``,
		"null utilization": `"seven_day_oauth_apps":{"utilization":null,"resets_at":"2030-01-05T00:00:00Z"},`,
	} {
		body := `{"seven_day":{"utilization":42,"resets_at":"2030-01-02T00:00:00Z"},` + apps +
			`"limits":[{"kind":"weekly_scoped","group":"weekly","percent":58,"resets_at":"2030-01-02T00:00:00.000Z","severity":"normal","is_active":false,"scope":{"model":{"display_name":"Fable"}}}]}`
		client := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/api/oauth/usage" {
				t.Fatalf("%s: unexpected request to %s: the row is the Fable meter, no probe", name, req.URL.Path)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
		})}
		windows, err := fetchAccountUsageWindowsLive(context.Background(), client, accounts.Account{
			ID: "acct-a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok",
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var fable []accounts.UsageWindow
		for _, window := range windows {
			if window.Name == agentclaude.FableWindowName {
				fable = append(fable, window)
			}
		}
		if len(fable) != 1 || fable[0].UsedPercent != 58 {
			t.Fatalf("%s: Fable windows = %+v, want one at 58%% used from the row", name, fable)
		}
	}
	if logs.Len() != 0 {
		t.Fatalf("logs:\n%s\nwant none: there is no seven_day_oauth_apps to disagree with", logs.String())
	}
}
