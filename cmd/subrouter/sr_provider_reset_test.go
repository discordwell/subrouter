package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	agentclaude "github.com/discordwell/subrouter/internal/agents/claude"
	"github.com/discordwell/subrouter/internal/proxy"
)

// PATCHES §46: the "1x reset" column for Claude, and `sr claude reset` /
// `sr codex reset`.

var resetUseBy = time.Date(2026, 10, 22, 16, 0, 0, 0, time.UTC)

// The local-daemon fill-in for `sr status` rows is off for the whole package:
// otherwise any test that lists Claude rows whose read carried no block, or
// Codex rows holding credits, would ask whatever answers at 127.0.0.1:31415
// -- on a developer machine, the real daemon. Tests that want it turn it on
// against their own fake.
func init() {
	fetchLocalDaemonResets = noLocalDaemonResets
}

func noLocalDaemonResets(srRunner, context.Context) []remoteServerUsageStatus {
	return nil
}

// serveLocalDaemonResets stands in for the fill-in's read of this machine's
// daemon and counts the reads.
func serveLocalDaemonResets(t *testing.T, statuses []remoteServerUsageStatus) *int {
	t.Helper()
	calls := new(int)
	fetchLocalDaemonResets = func(srRunner, context.Context) []remoteServerUsageStatus {
		*calls++
		return statuses
	}
	t.Cleanup(func() { fetchLocalDaemonResets = noLocalDaemonResets })
	return calls
}

const testResetOrganization = "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"

func heldResetInfo() *accounts.ComplimentaryResetInfo {
	one, zero, eligible := 1, 0, true
	return &accounts.ComplimentaryResetInfo{
		Known: true, Available: true, Remaining: &one, Total: &one, Used: &zero, Eligible: &eligible,
		Source: agentclaude.ResetProgram, ExpiresAt: resetUseBy.Format(time.RFC3339),
		Clears: []string{"five_hour", "seven_day", "seven_day_overage_included"},
	}
}

// claudeResetStatuses is a box's usage-status: one account holding its reset,
// one setup token, and one whose answer carried no block.
func claudeResetStatuses() []remoteServerUsageStatus {
	week := int64((3 * 24 * time.Hour) / time.Second)
	// The week is nearly spent (5% left) and rolls in three days, so a claim
	// of these rows just happens: no y/N (§46.11).
	windows := func() []accounts.UsageWindow {
		return []accounts.UsageWindow{
			{Name: "5h", UsedPercent: 34, LimitWindowSeconds: 5 * 3600, ResetAfterSeconds: 7200},
			{Name: "7d", UsedPercent: 95, LimitWindowSeconds: 7 * 86400, ResetAfterSeconds: week},
			{Name: agentclaude.FableWindowName, UsedPercent: 10, LimitWindowSeconds: 7 * 86400, ResetAfterSeconds: week, Feature: agentclaude.FableFeature},
		}
	}
	extra := &accounts.ExtraUsageInfo{IsEnabled: true}
	return []remoteServerUsageStatus{
		{ID: "held", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, AuthChecked: true, AuthValid: true,
			PlanType: "max", Windows: windows(), ExtraUsage: extra, ComplimentaryReset: heldResetInfo()},
		{ID: "setup", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, AuthChecked: true, AuthValid: true,
			PlanType: "max", Windows: windows(), ExtraUsage: extra, ComplimentaryReset: agentclaude.UnreadableResetInfo()},
		{ID: "unread", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, AuthChecked: true, AuthValid: true,
			PlanType: "max", Windows: windows(), ExtraUsage: extra},
	}
}

func assertResetColumnBetweenFableAndExtra(t *testing.T, where, table string) {
	t.Helper()
	var header string
	for _, line := range strings.Split(table, "\n") {
		if strings.Contains(line, "Fable wk") {
			header = line
			break
		}
	}
	fable, reset, extra := strings.Index(header, "Fable wk"), strings.Index(header, "1x reset"), strings.Index(header, "Extra usage")
	if fable < 0 || reset < 0 || extra < 0 || !(fable < reset && reset < extra) {
		t.Fatalf("%s: header %q; want Fable wk | 1x reset | Extra usage in that order\n%s", where, header, table)
	}
	if between := strings.Fields(header[fable+len("Fable wk") : reset]); len(between) != 0 {
		t.Fatalf("%s: %q sits between Fable wk and 1x reset", where, between)
	}
	if between := strings.Fields(header[reset+len("1x reset") : extra]); len(between) != 0 {
		t.Fatalf("%s: %q sits between 1x reset and Extra usage", where, between)
	}
	for _, want := range []string{"1 left " + resetUseBy.Local().Format("1/2"), "n/a"} {
		if !strings.Contains(table, want) {
			t.Fatalf("%s: table missing %q\n%s", where, want, table)
		}
	}
	for _, line := range strings.Split(table, "\n") {
		if strings.Contains(line, "unread") && !strings.Contains(line, " ?  ") {
			t.Fatalf("%s: an account whose read carried no block does not read \"?\": %q", where, line)
		}
	}
}

// The column sits between Fable wk and Extra usage wherever a Claude table is
// drawn: this machine's rows, a servers.json box, and a box seen only through
// the daemon's peer link -- the last two from the usage-status JSON itself.
func TestClaudeTablesPutTheResetColumnBetweenFableAndExtraUsage(t *testing.T) {
	t.Setenv("COLUMNS", "220")
	statuses := claudeResetStatuses()

	local := make([]srUsageRow, 0, len(statuses))
	for _, status := range statuses {
		local = append(local, srUsageRow{
			email: status.ID, provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth, planType: "max",
			windows: status.Windows, extraUsage: status.ExtraUsage, complimentaryReset: status.ComplimentaryReset,
			authChecked: true, authValid: true,
		})
	}
	var out bytes.Buffer
	displayUsageRows(&out, local, false)
	assertResetColumnBetweenFableAndExtra(t, "local", out.String())

	body, err := json.Marshal(statuses)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []remoteServerUsageStatus
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	displayUsageRows(&out, usageRowsFromServerUsageStatuses(decoded), false)
	assertResetColumnBetweenFableAndExtra(t, "server", out.String())

	section := peerAccountSection(proxy.PeerUsageView{
		Name: "box", URL: "https://203.0.113.7:31416", Reachable: true,
		AsOf: time.Now().UTC().Format(time.RFC3339), Accounts: body,
	})
	if section.err != nil {
		t.Fatal(section.err)
	}
	out.Reset()
	displayUsageRows(&out, section.rows, false)
	assertResetColumnBetweenFableAndExtra(t, "peer", out.String())
}

// Narrow tables keep the count and drop only the date.
func TestClaudeResetColumnDropsTheDateBeforeTheColumn(t *testing.T) {
	statuses := claudeResetStatuses()[:1]
	row := srUsageRow{email: "held", provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth, planType: "max",
		windows: statuses[0].Windows, complimentaryReset: statuses[0].ComplimentaryReset}
	full := claudeUsageGridColumns([]srUsageRow{row}, false, 0)
	width := usageGridWidth(full)
	narrow := claudeUsageGridColumns([]srUsageRow{row}, false, width-3)
	keys := map[string]bool{}
	for _, column := range narrow {
		keys[column.Key] = true
	}
	if keys["Reset"] || !keys["ResetShort"] {
		t.Fatalf("narrow columns = %+v; want the undated reset column", narrow)
	}
	if got := usageGridValues(row, "")["ResetShort"].Text; got != "1 left" {
		t.Fatalf("undated cell = %q", got)
	}
}

func TestClaudeResetCellReadings(t *testing.T) {
	intp := func(v int) *int { return &v }
	boolp := func(v bool) *bool { return &v }
	for _, tc := range []struct {
		name string
		info *accounts.ComplimentaryResetInfo
		want string
	}{
		{"not read", nil, "?"},
		{"setup token", agentclaude.UnreadableResetInfo(), "n/a"},
		{"held", heldResetInfo(), "1 left " + resetUseBy.Local().Format("1/2")},
		{"used", &accounts.ComplimentaryResetInfo{Known: true, Source: "cedar_ember", Eligible: boolp(true), Remaining: intp(0), Total: intp(1), Consumed: true}, "used"},
		{"expired", &accounts.ComplimentaryResetInfo{Known: true, Source: "cedar_ember", Eligible: boolp(true), Remaining: intp(0), Total: intp(1), Status: "expired"}, "expired"},
		{"no grant", &accounts.ComplimentaryResetInfo{Known: true, Source: "cedar_ember", Eligible: boolp(false), IneligibleReason: "no_grant"}, "none"},
		{"ineligible", &accounts.ComplimentaryResetInfo{Known: true, Source: "cedar_ember", Eligible: boolp(false), IneligibleReason: "tier"}, "not elig"},
		{"eligible, no grants", &accounts.ComplimentaryResetInfo{Known: true, Source: "cedar_ember", Eligible: boolp(true)}, "none"},
	} {
		row := srUsageRow{provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth, complimentaryReset: tc.info}
		if got := usageGridResetCell(row).Text; got != tc.want {
			t.Errorf("%s: cell %q, want %q", tc.name, got, tc.want)
		}
	}
	// Codex keeps its own reading.
	codex := srUsageRow{provider: accounts.ProviderCodex, authMode: accounts.AuthModeOAuth,
		complimentaryReset: &accounts.ComplimentaryResetInfo{Known: true, Available: true, Remaining: intp(2)}}
	if got := usageGridResetCell(codex).Text; got != "2 left" {
		t.Fatalf("codex cell = %q", got)
	}
}

// PATCHES §64: the 1x reset cell names a reset that clears only the Fable
// limit as a Fable reset, never as one that gives back the week. The rows are
// the daemon's reading of the two shapes the pool held on 2026-11-22, sent
// over usage-status as JSON; a row from a daemon before §64 reads as before.
func TestClaudeResetCellNamesAFableResetAsOne(t *testing.T) {
	now := time.Date(2026, 11, 22, 20, 0, 0, 0, time.UTC)
	week := `{"id":"week_reset","resets_total":1,"resets_left":1,"ends_at":"2026-12-10T22:00:00Z","clears":["seven_day","seven_day_overage_included"]}`
	fable := func(left int) string {
		return fmt.Sprintf(`{"id":"fable_5_2_launch","resets_total":%d,"resets_left":%d,"ends_at":"2027-03-09T07:59:00Z","clears":["seven_day_overage_included"]}`, left, left)
	}
	row := func(t *testing.T, grants ...string) srUsageRow {
		t.Helper()
		status, err := agentclaude.ParseResetStatus([]byte(`{"eligible":true,"grants":[` + strings.Join(grants, ",") + `]}`))
		if err != nil || status == nil {
			t.Fatalf("calibration: %v %+v", err, status)
		}
		raw, _ := json.Marshal(agentclaude.ResetInfo(status, now))
		var info accounts.ComplimentaryResetInfo
		if err := json.Unmarshal(raw, &info); err != nil {
			t.Fatal(err)
		}
		return srUsageRow{provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth, complimentaryReset: &info}
	}
	weekDate := time.Date(2026, 12, 10, 22, 0, 0, 0, time.UTC).Local().Format("1/2")
	fableDate := time.Date(2027, 3, 9, 7, 59, 0, 0, time.UTC).Local().Format("1/2")
	// A Fable reset that ends before the week reset: the soonest grant is
	// the Fable one, and the cell still dates the week reset.
	soonFable := `{"id":"fable_soon","resets_total":1,"resets_left":1,"ends_at":"2026-12-01T12:00:00Z","clears":["seven_day_overage_included"]}`
	older := row(t, week, fable(1))
	older.complimentaryReset.RemainingByKind = nil
	for _, tc := range []struct {
		name           string
		row            srUsageRow
		dated, undated string
	}{
		{"Fable resets alone", row(t, fable(2)), "2 Fable " + fableDate, "2 Fable"},
		{"the week reset and a Fable reset queued behind it", row(t, week, fable(1)), "1 +1F " + weekDate, "1 +1F"},
		{"the week reset and a Fable reset that ends first", row(t, week, soonFable), "1 +1F " + weekDate, "1 +1F"},
		{"the week reset alone", row(t, week), "1 left " + weekDate, "1 left"},
		{"a daemon from before §64", older, "2 left " + weekDate, "2 left"},
	} {
		if got := usageGridResetCell(tc.row).Text; got != tc.dated {
			t.Errorf("%s: dated cell %q, want %q", tc.name, got, tc.dated)
		}
		if got := usageGridResetCellUndated(tc.row).Text; got != tc.undated {
			t.Errorf("%s: undated cell %q, want %q", tc.name, got, tc.undated)
		}
	}
}

// PATCHES §64: a claim names which kind of reset it spent and what Anthropic
// says it cleared, with the Fable meter beside the week. The Fable claim is
// shaped as the one measured on 2026-11-09: the week 100% -> 42% used, the
// Fable meter 116% -> 0%. An older daemon's answer with no grant reads as
// before, with what it cleared.
func TestClaudeResetSaysWhichKindItClaimedAndWhatItGaveBack(t *testing.T) {
	opts := providerResetOptions{provider: accounts.ProviderClaude, account: "held"}
	weekly := resetWeekly.Local().Format("Mon Jan 2 15:04 MST")
	fable := claimedResponse()
	one := 1
	fable.ResetsLeftAfter = &one
	fable.Grant = &agentclaude.ResetGrant{ID: "fable_5_2_launch", ResetsTotal: 2, ResetsLeft: 2, EndsAt: resetUseBy.Format(time.RFC3339),
		Clears: []string{"seven_day_overage_included"}, GivesBackWeekly: true, UsableNow: true, UseRequiresLimit: true}
	fable.Cleared = []string{"seven_day_overage_included"}
	fable.Windows = []accounts.UsageWindow{{Name: "5h", UsedPercent: 3}, {Name: "7d", UsedPercent: 100}, {Name: agentclaude.FableWindowName, UsedPercent: 116}}
	fable.WindowsAfter = []accounts.UsageWindow{{Name: "5h", UsedPercent: 3}, {Name: "7d", UsedPercent: 42}, {Name: agentclaude.FableWindowName, UsedPercent: 0}}
	noGrant := claimedResponse()
	noGrant.Grant = nil
	for _, tc := range []struct {
		name   string
		result proxy.ClaudeResetResponse
		want   string
	}{
		{"a Fable reset", fable, "held (local): Fable reset claimed — cleared seven_day_overage_included; 7d 100% -> 42% used, 5h 3%, " +
			"Fable 116% -> 0%; weekly reset still " + weekly + "; 1 reset left"},
		{"a week reset", claimedResponse(), claimedLine("local")},
		{"an older daemon's answer", noGrant, "held (local): reset claimed — cleared five_hour, seven_day; 7d 100% -> 0% used, 5h 0%; " +
			"weekly reset still " + weekly + "; 0 resets left"},
	} {
		line, claimed := claudeResetOutcome(tc.result, "held", "local", opts, time.Now())
		if !claimed || line != tc.want {
			t.Errorf("%s: %v %q\nwant %q", tc.name, claimed, line, tc.want)
		}
	}

	// The same line end to end: one call to the holding daemon, which named a
	// Fable grant next and claimed it.
	daemon := &resetDaemonFake{t: t, statuses: claudeResetStatuses(), preview: claimablePreview(), claim: fable}
	store := resetMachine(t, daemon)
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	got, err := runReset(t, store, devNull, "claude", "reset", "held")
	if err != nil || !strings.HasPrefix(strings.TrimSpace(got), "held (local): Fable reset claimed — cleared seven_day_overage_included;") {
		t.Fatalf("claim: %v\n%s", err, got)
	}
}

// resetDaemonFake is a daemon (local or a box) as the reset verbs see it.
type resetDaemonFake struct {
	t        *testing.T
	token    string
	statuses []remoteServerUsageStatus
	peers    []proxy.PeerUsageView
	// preview/claim answer the claude-reset route.
	preview proxy.ClaudeResetResponse
	claim   proxy.ClaudeResetResponse
	// codexPreview/codexRedeem answer rate-limit-reset.
	codexPreview string
	codexRedeem  string
	// dropClaim reads a claim (dry_run:false, or a Codex redeem) and hangs
	// up without an answer: the answer is lost after the request went out.
	dropClaim bool
	// claimDelay holds a claim's answer that long (a daemon waiting out
	// Anthropic's 429s).
	claimDelay time.Duration
	// olderDaemon answers a claim as a §46.5 daemon does: 400, plain text,
	// asking for the preview's organization.
	olderDaemon bool

	mu            sync.Mutex
	claudeBodies  []proxy.ClaudeResetRequest
	claudeAuth    []string
	codexQueries  []url.Values
	usageReads    int
	peerViewReads int
}

func (d *resetDaemonFake) serve(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(d.handler())
	t.Cleanup(server.Close)
	return server
}

// servePublic stands the daemon up as a box's public listener: TLS, and the
// pin of its certificate.
func (d *resetDaemonFake) servePublic(t *testing.T) (string, string) {
	t.Helper()
	server := httptest.NewTLSServer(d.handler())
	t.Cleanup(server.Close)
	return server.URL, proxy.CertificateFingerprint(server.Certificate().Raw)
}

func (d *resetDaemonFake) handler() http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.token != "" && req.Header.Get("Authorization") != "Bearer "+d.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/_subrouter/usage-status":
			d.usageReads++
			_ = json.NewEncoder(w).Encode(d.statuses)
		case proxy.PeersUsageStatusPath:
			d.peerViewReads++
			_ = json.NewEncoder(w).Encode(proxy.PeersUsageStatus{Peers: d.peers})
		case proxy.ClaudeResetPath:
			var body proxy.ClaudeResetRequest
			raw, _ := io.ReadAll(req.Body)
			if err := json.Unmarshal(raw, &body); err != nil {
				d.t.Errorf("claude-reset body %s: %v", raw, err)
			}
			d.claudeBodies = append(d.claudeBodies, body)
			d.claudeAuth = append(d.claudeAuth, req.Header.Get("Authorization"))
			if body.DryRun == nil || *body.DryRun {
				_ = json.NewEncoder(w).Encode(d.preview)
				return
			}
			if d.dropClaim {
				hangUp(w)
				return
			}
			if d.olderDaemon {
				http.Error(w, "a claim (dry_run:false) must carry expected_organization from a preview; nothing was claimed", http.StatusBadRequest)
				return
			}
			time.Sleep(d.claimDelay)
			_ = json.NewEncoder(w).Encode(d.claim)
		case "/_subrouter/rate-limit-reset":
			query := req.URL.Query()
			d.codexQueries = append(d.codexQueries, query)
			if query.Get("dry_run") == "true" {
				_, _ = io.WriteString(w, d.codexPreview)
				return
			}
			if d.dropClaim {
				hangUp(w)
				return
			}
			_, _ = io.WriteString(w, d.codexRedeem)
		default:
			http.NotFound(w, req)
		}
	})
}

// hangUp closes the connection without writing a response.
func hangUp(w http.ResponseWriter) {
	if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
		_ = conn.Close()
	}
}

func (d *resetDaemonFake) claudeRequests() []proxy.ClaudeResetRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]proxy.ClaudeResetRequest(nil), d.claudeBodies...)
}

func claimablePreview() proxy.ClaudeResetResponse {
	return proxy.ClaudeResetResponse{
		Account: "held", DryRun: true, Claimable: true,
		Organization: testResetOrganization, AccountEmail: "held@example.com",
		Status: &agentclaude.ResetStatus{Eligible: true, AtLimit: true, Exhausted: []string{"five_hour"}},
		Grant: &agentclaude.ResetGrant{ID: "opus_5_5_launch", Label: "Claude Opus 5.5 launch: one usage-limit reset for Pro and Max",
			ResetsTotal: 1, ResetsLeft: 1, EndsAt: resetUseBy.Format(time.RFC3339), Clears: []string{"five_hour", "seven_day"}, UsableNow: true, UseRequiresLimit: true},
		Windows: []accounts.UsageWindow{
			{Name: "5h", UsedPercent: 100, ResetAfterSeconds: 3600},
			{Name: "7d", UsedPercent: 78, ResetAfterSeconds: int64((3 * 24 * time.Hour) / time.Second)},
		},
	}
}

// resetWeekly is the weekly roll a claim answer names.
var resetWeekly = time.Date(2026, 10, 4, 19, 0, 0, 0, time.UTC)

// claimedResponse is the daemon's answer to a claim that reset held, shaped
// like the first real claim (2026-09-29): a grant that clears the week, 7d
// 100% -> 0%, no resets left.
func claimedResponse() proxy.ClaudeResetResponse {
	left := 0
	return proxy.ClaudeResetResponse{
		Account: "held", Claimable: true, Claimed: true, Result: "reset", RequestID: "req-1", ClaimAttempts: 1,
		Grant: &agentclaude.ResetGrant{ID: "opus_5_5_launch", ResetsTotal: 1, ResetsLeft: 1, EndsAt: resetUseBy.Format(time.RFC3339),
			Clears: []string{"five_hour", "seven_day"}, UsableNow: true, UseRequiresLimit: true},
		ResetsLeftAfter: &left, Cleared: []string{"five_hour", "seven_day"}, WeeklyResetsAt: resetWeekly.Format(time.RFC3339),
		Status:       &agentclaude.ResetStatus{Eligible: true, AtLimit: true, Exhausted: []string{"seven_day"}},
		Windows:      []accounts.UsageWindow{{Name: "5h", UsedPercent: 0}, {Name: "7d", UsedPercent: 100}},
		WindowsAfter: []accounts.UsageWindow{{Name: "5h", UsedPercent: 0}, {Name: "7d", UsedPercent: 0}},
	}
}

// claimedLine is what `sr claude reset held` prints for claimedResponse.
func claimedLine(label string) string {
	return "held (" + label + "): week reset claimed — cleared five_hour, seven_day; 7d 100% -> 0% used, 5h 0%; weekly reset still " +
		resetWeekly.Local().Format("Mon Jan 2 15:04 MST") + "; 0 resets left"
}

// resetMachine is a scratch machine whose local daemon is daemon.
func resetMachine(t *testing.T, daemon *resetDaemonFake) accounts.CodexStore {
	t.Helper()
	isolateGate(t)
	t.Setenv("COLUMNS", "200")
	if daemon != nil {
		local := daemon.serve(t)
		t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")
	}
	return accounts.DefaultCodexStore()
}

func runReset(t *testing.T, store accounts.CodexStore, in io.Reader, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	runner := srRunner{store: store, in: in, out: &out, errOut: &out,
		client: &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}}
	err := runner.run(context.Background(), args)
	return out.String(), err
}

// PATCHES §46.7: one command, no confirmation. `sr claude reset held` sends
// ONE request to the holding daemon -- a claim, naming the account twice and
// nothing else -- and prints one final line. No preview, no question, even
// with no terminal.
func TestClaudeResetClaimsInOneCall(t *testing.T) {
	daemon := &resetDaemonFake{t: t, statuses: claudeResetStatuses(), preview: claimablePreview(), claim: claimedResponse()}
	store := resetMachine(t, daemon)
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	got, err := runReset(t, store, devNull, "claude", "reset", "held")
	if err != nil {
		t.Fatalf("claim: %v\n%s", err, got)
	}
	requests := daemon.claudeRequests()
	if len(requests) != 1 {
		t.Fatalf("requests = %+v; want exactly one", requests)
	}
	claim := requests[0]
	if claim.DryRun == nil || *claim.DryRun || claim.Account != "held" || claim.ConfirmAccount != "held" ||
		claim.GrantID != "" || claim.ExpectedOrganization != "" || claim.RequestID != "" {
		t.Fatalf("claim request = %+v; want dry_run:false with the account confirmed and nothing else", claim)
	}
	if strings.TrimSpace(got) != claimedLine("local") {
		t.Fatalf("output:\n%s\nwant\n%s", got, claimedLine("local"))
	}

	// Early use and a wait on Anthropic's rate limit are said after the fact.
	early := claimedResponse()
	early.EarlyUse, early.RateLimitWaitSeconds = true, 90
	early.Status.Exhausted = nil
	daemon.mu.Lock()
	daemon.claim = early
	daemon.mu.Unlock()
	got, err = runReset(t, store, devNull, "claude", "reset", "held")
	if err != nil || !strings.Contains(got, "; note: it was not at a limit this reset clears (exhausted: none)") ||
		!strings.Contains(got, "(waited 1m on Anthropic's rate limit)") {
		t.Fatalf("early claim: %v\n%s", err, got)
	}
}

// --dry-run is the opt-in preview: one dry-run request, never a claim, and
// it exits 0 either way.
func TestClaudeResetDryRunNeverClaims(t *testing.T) {
	daemon := &resetDaemonFake{t: t, statuses: claudeResetStatuses(), preview: claimablePreview(), claim: claimedResponse()}
	store := resetMachine(t, daemon)
	got, err := runReset(t, store, strings.NewReader(""), "claude", "reset", "held", "--dry-run")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, got)
	}
	requests := daemon.claudeRequests()
	if len(requests) != 1 || requests[0].DryRun == nil || !*requests[0].DryRun {
		t.Fatalf("dry run sent %+v; want exactly one dry run", requests)
	}
	for _, want := range []string{
		"Claude account held, held by this machine's daemon (dry run)",
		"Claude Opus 5.5 launch", "1 of 1, use by", "clears:       five_hour, seven_day",
		"5h 100% used", "7d 78% used", "claimable:    yes",
		"login:        held@example.com (organization " + testResetOrganization + ")",
		"= 16:00 UTC", "at a limit:   yes (exhausted: five_hour)",
		"Only the 5h limit is exhausted, and it resets on its own in 1h",
		"Dry run: nothing was sent to Anthropic. `sr claude reset held` claims it (one-time).",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("dry run missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "--confirm") || strings.Contains(got, "reset claimed") {
		t.Fatalf("dry run output:\n%s", got)
	}

	refused := claimablePreview()
	refused.Claimable = false
	refused.Refusal = "this account's saved reset is already used"
	daemon.mu.Lock()
	daemon.preview = refused
	daemon.mu.Unlock()
	got, err = runReset(t, store, strings.NewReader(""), "claude", "reset", "held", "--dry-run")
	if err != nil || !strings.Contains(got, "claimable:    no -- this account's saved reset is already used") || !strings.Contains(got, "nothing to claim") {
		t.Fatalf("refused dry run: %v\n%s", err, got)
	}
	for _, request := range daemon.claudeRequests() {
		if request.DryRun == nil || !*request.DryRun {
			t.Fatalf("--dry-run sent a claim: %+v", request)
		}
	}
}

// Nothing is sent at all for a sweep flag, two accounts or none.
func TestProviderResetRefusesBeforeAnyRequest(t *testing.T) {
	daemon := &resetDaemonFake{t: t, statuses: claudeResetStatuses(), preview: claimablePreview(), claim: claimedResponse()}
	store := resetMachine(t, daemon)
	for _, args := range [][]string{
		{"claude", "reset", "--all"},
		{"claude", "reset", "held", "setup"},
		{"claude", "reset"},
		{"claude", "reset", "held", "--bogus"},
		{"codex", "reset", "codex@example.com", "--gto"},
		{"codex", "reset", "codex@example.com", "--best"},
	} {
		if got, err := runReset(t, store, strings.NewReader("y\n"), args...); err == nil {
			t.Fatalf("%v: no error\n%s", args, got)
		}
	}
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.claudeBodies) != 0 || len(daemon.codexQueries) != 0 || daemon.usageReads != 0 {
		t.Fatalf("a refused command reached the daemon: %d claude, %d codex, %d usage reads",
			len(daemon.claudeBodies), len(daemon.codexQueries), daemon.usageReads)
	}
}

// The first version's --confirm, --yes and --early are accepted and ignored:
// an old invocation claims in one call, with no question, like the new one.
func TestClaudeResetOldConfirmationFlagsAreAcceptedAndIgnored(t *testing.T) {
	daemon := &resetDaemonFake{t: t, statuses: claudeResetStatuses(), preview: claimablePreview(), claim: claimedResponse()}
	store := resetMachine(t, daemon)
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	for i, args := range [][]string{
		{"claude", "reset", "held", "--confirm", "held"},
		{"claude", "reset", "held", "--confirm", "held", "--yes"},
		{"claude", "reset", "held", "--confirm=held", "-y", "--early"},
		{"claude", "reset", "held", "--yes"},
		{"claude", "reset", "held", "--early", "--confirm", "held"},
	} {
		got, err := runReset(t, store, devNull, args...)
		if err != nil || strings.TrimSpace(got) != claimedLine("local") {
			t.Fatalf("%v: %v\n%s", args, err, got)
		}
		requests := daemon.claudeRequests()
		if len(requests) != i+1 || *requests[i].DryRun || requests[i].Account != "held" {
			t.Fatalf("%v sent %+v; want one claim per run", args, requests)
		}
	}
	codex := &resetDaemonFake{t: t, statuses: []remoteServerUsageStatus{{ID: "codex@example.com", Email: "codex@example.com",
		Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, AuthChecked: true, AuthValid: true}},
		codexRedeem: `{"reset":1,"results":[{"email":"codex@example.com","eligible":true,"reset":true}]}`}
	store = resetMachine(t, codex)
	if got, err := runReset(t, store, devNull, "codex", "reset", "codex@example.com", "--confirm", "codex@example.com", "--yes"); err != nil ||
		!strings.Contains(got, "codex@example.com (local): reset credit redeemed") {
		t.Fatalf("old codex invocation: %v\n%s", err, got)
	}
}

// The CLI outwaits the daemon: the daemon's budget includes up to four
// minutes of waiting out Anthropic's 429s, and its answer must arrive -- even
// though sr's runner carries a shorter client timeout (120 s in srForProgram,
// scaled down here), which a claim must not inherit.
func TestClaudeResetCLIOutwaitsTheDaemon(t *testing.T) {
	if claudeResetCLITimeout <= proxy.ClaudeResetBudget {
		t.Fatalf("CLI timeout %v <= the daemon's budget %v", claudeResetCLITimeout, proxy.ClaudeResetBudget)
	}
	daemon := &resetDaemonFake{t: t, statuses: claudeResetStatuses(), claim: claimedResponse(), claimDelay: 400 * time.Millisecond}
	store := resetMachine(t, daemon)
	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out,
		client: &http.Client{Timeout: 100 * time.Millisecond, Transport: http.DefaultTransport.(*http.Transport).Clone()}}
	if err := runner.run(context.Background(), []string{"claude", "reset", "held"}); err != nil || strings.TrimSpace(out.String()) != claimedLine("local") {
		t.Fatalf("a claim answered after the runner's client timeout: %v\n%s", err, out.String())
	}
}

// A holding daemon still on §46.5 answers the one-call claim 400 (it wants a
// preview's organization): the command says to deploy this build there,
// not to preview.
func TestClaudeResetNamesAnOlderHoldingDaemon(t *testing.T) {
	daemon := &resetDaemonFake{t: t, statuses: claudeResetStatuses(), olderDaemon: true}
	store := resetMachine(t, daemon)
	got, err := runReset(t, store, strings.NewReader(""), "claude", "reset", "held")
	if err == nil || !strings.Contains(err.Error(), "held (local): not claimed: the holding daemon runs an older build") ||
		!strings.Contains(err.Error(), "restart") || strings.Contains(err.Error(), "from a preview") {
		t.Fatalf("older daemon: %v\n%s", err, got)
	}
}

// --confirm is ignored as a question, but a value naming another account
// is a mistake, refused before any request.
func TestClaudeResetRefusesAConfirmNamingAnotherAccount(t *testing.T) {
	daemon := &resetDaemonFake{t: t, statuses: claudeResetStatuses(), claim: claimedResponse()}
	store := resetMachine(t, daemon)
	for _, args := range [][]string{
		{"claude", "reset", "held", "--confirm", "setup"},
		{"claude", "reset", "held", "--confirm=unread", "--yes"},
		{"claude", "reset", "--confirm", "setup", "held"},
	} {
		if got, err := runReset(t, store, strings.NewReader(""), args...); err == nil || !strings.Contains(err.Error(), "--confirm") {
			t.Fatalf("%v: %v\n%s", args, err, got)
		}
	}
	daemon.mu.Lock()
	sent, reads := len(daemon.claudeBodies), daemon.usageReads
	daemon.mu.Unlock()
	if sent != 0 || reads != 0 {
		t.Fatalf("a mismatched --confirm reached the daemon: %d claims, %d reads", sent, reads)
	}
	if got, err := runReset(t, store, strings.NewReader(""), "claude", "reset", "held", "--confirm", "HELD"); err != nil || len(daemon.claudeRequests()) != 1 {
		t.Fatalf("--confirm in another case: %v\n%s", err, got)
	}
}

// A name that matches nothing gets a pointer to the account it is closest to,
// and nothing is sent: a typo is never claimed on its own.
func TestProviderResetSuggestsTheNearestAccountAndSendsNothing(t *testing.T) {
	statuses := claudeResetStatuses()
	statuses[0].Email = "held@example.com"
	daemon := &resetDaemonFake{t: t, statuses: statuses, claim: claimedResponse()}
	store := resetMachine(t, daemon)
	for name, want := range map[string]string{
		"hled":             `did you mean "held"?`,
		"held@exmaple.com": `did you mean "held@example.com"?`,
		"setpu":            `did you mean "setup"?`,
	} {
		got, err := runReset(t, store, strings.NewReader(""), "claude", "reset", name)
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "nothing was sent") {
			t.Fatalf("%q: %v\n%s; want %q", name, err, got, want)
		}
	}
	if got, err := runReset(t, store, strings.NewReader(""), "claude", "reset", "zzzzzz"); err == nil || strings.Contains(err.Error(), "did you mean") {
		t.Fatalf("a name close to nothing: %v\n%s", err, got)
	}
	if n := len(daemon.claudeRequests()); n != 0 {
		t.Fatalf("a near miss reached the claim route %d times", n)
	}
}

// Only the profile name (id) and the account email name an account: a row's
// label does not.
func TestProviderResetDoesNotMatchALabel(t *testing.T) {
	codex := &resetDaemonFake{t: t, statuses: []remoteServerUsageStatus{{ID: "codex@example.com", Email: "codex@example.com", Label: "work",
		Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, AuthChecked: true, AuthValid: true}},
		codexRedeem: `{"reset":1,"results":[{"email":"codex@example.com","eligible":true,"reset":true}]}`}
	store := resetMachine(t, codex)
	if got, err := runReset(t, store, strings.NewReader(""), "codex", "reset", "work"); err == nil || !strings.Contains(err.Error(), "holds a Codex account \"work\"") {
		t.Fatalf("a label named the account: %v\n%s", err, got)
	}
	codex.mu.Lock()
	defer codex.mu.Unlock()
	if len(codex.codexQueries) != 0 {
		t.Fatalf("a label reached rate-limit-reset: %v", codex.codexQueries)
	}
}

// Nothing claimed exits non-zero with the reason verbatim: a daemon refusal,
// and each answer Anthropic can give that is not "reset".
func TestClaudeResetNotClaimedExitsNonZero(t *testing.T) {
	refused := proxy.ClaudeResetResponse{Account: "held", Refusal: "this account's saved reset is already used"}
	notLimited := proxy.ClaudeResetResponse{Account: "held", Claimed: true, Result: "not_limited", Reason: "not_limited", RequestID: "r"}
	for want, claim := range map[string]proxy.ClaudeResetResponse{
		"held (local): not claimed: this account's saved reset is already used":   refused,
		"held (local): not claimed: Anthropic answered not_limited (not_limited)": notLimited,
	} {
		daemon := &resetDaemonFake{t: t, statuses: claudeResetStatuses(), preview: claimablePreview(), claim: claim}
		store := resetMachine(t, daemon)
		got, err := runReset(t, store, strings.NewReader(""), "claude", "reset", "held")
		if err == nil || err.Error() != want || strings.Contains(got, "reset claimed") {
			t.Fatalf("err %v, want %q\n%s", err, want, got)
		}
		if n := len(daemon.claudeRequests()); n != 1 {
			t.Fatalf("%d requests; want the one claim", n)
		}
	}
}

// A claim whose answer never came back may have landed: the command says so
// and exits non-zero. A daemon that could not be reached took nothing.
func TestClaudeResetLostAnswerSaysMayHaveLanded(t *testing.T) {
	daemon := &resetDaemonFake{t: t, statuses: claudeResetStatuses(), dropClaim: true}
	store := resetMachine(t, daemon)
	got, err := runReset(t, store, strings.NewReader(""), "claude", "reset", "held")
	if err == nil || !strings.Contains(err.Error(), "held (local): may have landed: the claim's answer was lost") ||
		!strings.HasSuffix(err.Error(), "; "+resetLandedCheck) || strings.Contains(err.Error(), "dry-run") {
		t.Fatalf("lost answer: %v\n%s", err, got)
	}
	if n := len(daemon.claudeRequests()); n != 1 {
		t.Fatalf("%d requests; want the one claim", n)
	}

	codex := &resetDaemonFake{t: t, statuses: []remoteServerUsageStatus{{ID: "codex@example.com", Email: "codex@example.com",
		Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, AuthChecked: true, AuthValid: true,
		Windows: []accounts.UsageWindow{{Name: "secondary", UsedPercent: 100, LimitWindowSeconds: 604800, ResetAfterSeconds: 200000}}}}, dropClaim: true}
	store = resetMachine(t, codex)
	if got, err := runReset(t, store, strings.NewReader(""), "codex", "reset", "codex@example.com"); err == nil ||
		!strings.Contains(err.Error(), "codex@example.com (local): may have landed") {
		t.Fatalf("lost codex answer: %v\n%s", err, got)
	}

	if !resetRequestNeverSent(&net.OpError{Op: "dial", Err: errors.New("connection refused")}) || resetRequestNeverSent(io.ErrUnexpectedEOF) {
		t.Fatal("a dial error is never sent; an EOF after sending may have landed")
	}
}

// The account is named exactly: its profile name or its account email,
// ignoring case. A fragment or prefix of either matches nothing, and nothing
// is sent.
func TestProviderResetNamesAnAccountExactly(t *testing.T) {
	statuses := claudeResetStatuses()
	statuses[0].Email = "held@example.com"
	daemon := &resetDaemonFake{t: t, statuses: statuses, preview: claimablePreview(), claim: claimedResponse()}
	store := resetMachine(t, daemon)
	for _, name := range []string{"hel", "eld", "held@example", "held@", "example.com", "held@example.co"} {
		got, err := runReset(t, store, strings.NewReader(""), "claude", "reset", name)
		if err == nil || !strings.Contains(err.Error(), "no daemon this machine can reach holds a Claude account") {
			t.Fatalf("fragment %q: %v\n%s", name, err, got)
		}
	}
	if n := len(daemon.claudeRequests()); n != 0 {
		t.Fatalf("a fragment reached the claim route %d times", n)
	}
	for i, name := range []string{"held@example.com", "HELD@Example.COM", "HELD"} {
		got, err := runReset(t, store, strings.NewReader(""), "claude", "reset", name)
		requests := daemon.claudeRequests()
		if err != nil || len(requests) != i+1 || requests[i].Account != "held" || requests[i].ConfirmAccount != "held" {
			t.Fatalf("%q: %v, requests %+v\n%s", name, err, requests, got)
		}
	}
}

// Every result the daemon can report maps to one line carrying Anthropic's
// result and reason verbatim, and only "reset" counts as claimed. unavailable
// and a 5xx or unanswered error "may have landed", never "not claimed"; the
// request id is promised only when the daemon kept it.
func TestClaudeResetOutcomeLines(t *testing.T) {
	opts := providerResetOptions{provider: accounts.ProviderClaude, account: "held"}
	for _, tc := range []struct {
		name        string
		result      string
		reason      string
		unconfirmed bool
		httpStatus  int
		want        []string
		not         []string
	}{
		{"already_used", "already_used", "already_used", false, 0, []string{"held (server home): not claimed: Anthropic answered already_used (already_used)"}, []string{"may"}},
		{"not_limited", "not_limited", "not_limited", false, 0, []string{"not claimed: Anthropic answered not_limited (not_limited)"}, nil},
		{"cooldown", "cooldown", "", false, 0, []string{"not claimed: Anthropic answered cooldown until 2026-09-30T00:00:00Z"}, []string{"may still have landed"}},
		{"surface", "ineligible", "surface", false, 0, []string{"not claimed: Anthropic answered ineligible (surface)", "claude.ai can claim it"}, nil},
		{"cli_version", "ineligible", "cli_version", false, 0, []string{"presents claude-cli/", "SUBROUTER_CLAUDE_CODE_VERSION"}, nil},
		{"unavailable", "unavailable", "reset_unconfirmed", true, 0, []string{"may have landed — Anthropic answered unavailable (reset_unconfirmed)", "resends request id req-9", resetLandedCheck}, []string{"not claimed", "dry-run"}},
		{"auth_error", "auth_error", "", false, 401, []string{"not claimed: Anthropic refused the account's token (HTTP 401); not refreshed or retried"}, []string{"may still have landed"}},
		{"auth_error after an unconfirmed claim", "auth_error", "", true, 401, []string{"an earlier claim may still have landed", "req-9"}, nil},
		{"rate_limited", "rate_limited", "", false, 429, []string{"not claimed: Anthropic still rate-limited the claim (HTTP 429)", "nothing was used"}, []string{"req-9", "may"}},
		{"error 5xx", "error", "", true, 502, []string{"may have landed — no definite answer: claim failed: 502", "req-9"}, []string{"not claimed"}},
		{"error 4xx", "error", "", true, 400, []string{"held (server home): most likely not claimed: claim failed: 502", "keeps request id req-9"}, []string{"may have landed", "earlier"}},
	} {
		response := proxy.ClaudeResetResponse{Account: "held", Claimed: true, Result: tc.result, Reason: tc.reason, RequestID: "req-9",
			CooldownUntil: "2026-09-30T00:00:00Z", HTTPStatus: tc.httpStatus, Error: "claim failed: 502", Unconfirmed: tc.unconfirmed}
		line, claimed := claudeResetOutcome(response, "held", "server home", opts, time.Now())
		if claimed {
			t.Errorf("%s: counted as claimed: %q", tc.name, line)
		}
		for _, want := range tc.want {
			if !strings.Contains(line, want) {
				t.Errorf("%s: %q; want it to contain %q", tc.name, line, want)
			}
		}
		for _, not := range tc.not {
			if strings.Contains(line, not) {
				t.Errorf("%s: %q; must not contain %q", tc.name, line, not)
			}
		}
	}
	line, claimed := claudeResetOutcome(claimedResponse(), "held", "server home", opts, time.Now())
	if !claimed || line != claimedLine("server home") {
		t.Fatalf("reset: %v %q\nwant %q", claimed, line, claimedLine("server home"))
	}
	// A claim that went by the daemon's cached block says the "before" and
	// the early-use note are as of that read.
	fromCache := claimedResponse()
	fromCache.StatusSource, fromCache.StatusAgeSeconds, fromCache.EarlyUse = "cache", 600, true
	fromCache.Status.Exhausted = nil
	if line, claimed := claudeResetOutcome(fromCache, "held", "local", opts, time.Now()); !claimed ||
		!strings.Contains(line, "7d 100% -> 0% used, 5h 0% (before: the daemon's read 10m ago)") ||
		!strings.Contains(line, "note: as of that read it was not at a limit this reset clears (exhausted: none)") {
		t.Fatalf("claim from the cache: %v %q", claimed, line)
	}
	cached := proxy.ClaudeResetResponse{Account: "held", Refusal: "Anthropic says resets are cooling down until 2026-09-30T00:00:00Z", StatusSource: "cache", StatusAgeSeconds: 240}
	if line, claimed := claudeResetOutcome(cached, "held", "local", opts, time.Now()); claimed || !strings.HasSuffix(line, "(the daemon's read 4m ago)") {
		t.Fatalf("cached refusal: %v %q", claimed, line)
	}
}

// `sr codex reset --dry-run` previews; a dry run with nothing to redeem says
// so and never offers --confirm.
func TestCodexResetDryRun(t *testing.T) {
	statuses := []remoteServerUsageStatus{{ID: "codex@example.com", Email: "codex@example.com", Provider: accounts.ProviderCodex,
		AuthMode: accounts.AuthModeOAuth, AuthChecked: true, AuthValid: true}}
	codex := &resetDaemonFake{t: t, statuses: statuses,
		codexPreview: `{"dry_run":true,"reset":0,"results":[{"email":"codex@example.com","eligible":false,"dry_run":true,"error":"no reset credits"}]}`}
	store := resetMachine(t, codex)
	got, err := runReset(t, store, strings.NewReader(""), "codex", "reset", "codex@example.com", "--dry-run")
	if err != nil || !strings.Contains(got, "codex@example.com (local): nothing to redeem: no reset credits") || strings.Contains(got, "--confirm") {
		t.Fatalf("codex dry run: %v\n%s", err, got)
	}
	codex.mu.Lock()
	defer codex.mu.Unlock()
	if len(codex.codexQueries) != 1 || codex.codexQueries[0].Get("dry_run") != "true" {
		t.Fatalf("codex dry run sent %v", codex.codexQueries)
	}
}

// The claim goes to the daemon that holds the account: this machine's when it
// does, else the servers.json box that does (with its admin token). A box
// seen only through the peer link has no admin path, and the command says so
// without sending anything.
func TestProviderResetGoesToTheDaemonThatHoldsTheAccount(t *testing.T) {
	t.Run("local", func(t *testing.T) {
		local := &resetDaemonFake{t: t, statuses: claudeResetStatuses(), preview: claimablePreview(), claim: claimedResponse()}
		store := resetMachine(t, local)
		unlockRemoteGateForTest(t)
		boxStatuses := claudeResetStatuses()[1:] // the box has no "held"
		box := &resetDaemonFake{t: t, token: "box-token", statuses: boxStatuses, preview: claimablePreview(), claim: claimedResponse()}
		var scratch bytes.Buffer
		addBoxServer(t, store, &scratch, box.serve(t).URL)
		if got, err := runReset(t, store, strings.NewReader(""), "claude", "reset", "held"); err != nil || len(local.claudeRequests()) != 1 || len(box.claudeRequests()) != 0 ||
			strings.TrimSpace(got) != claimedLine("local") {
			t.Fatalf("err %v, local %d, box %d\n%s", err, len(local.claudeRequests()), len(box.claudeRequests()), got)
		}
	})
	t.Run("server", func(t *testing.T) {
		local := &resetDaemonFake{t: t, statuses: nil}
		store := resetMachine(t, local)
		unlockRemoteGateForTest(t)
		preview := claimablePreview()
		box := &resetDaemonFake{t: t, token: "box-token", statuses: claudeResetStatuses(), preview: preview, claim: claimedResponse()}
		boxServer := box.serve(t)
		var scratch bytes.Buffer
		addBoxServer(t, store, &scratch, boxServer.URL)
		got, err := runReset(t, store, strings.NewReader(""), "claude", "reset", "held")
		if err != nil || len(box.claudeRequests()) != 1 || len(local.claudeRequests()) != 0 {
			t.Fatalf("err %v, box %d, local %d\n%s", err, len(box.claudeRequests()), len(local.claudeRequests()), got)
		}
		if box.claudeAuth[0] != "Bearer box-token" || strings.TrimSpace(got) != claimedLine("server box") {
			t.Fatalf("box request auth %q\n%s", box.claudeAuth[0], got)
		}
	})
	t.Run("peer only", func(t *testing.T) {
		body, _ := json.Marshal(claudeResetStatuses())
		local := &resetDaemonFake{t: t, statuses: nil, peers: []proxy.PeerUsageView{{
			Name: "home", URL: "https://203.0.113.7:31416", Reachable: true, AsOf: time.Now().UTC().Format(time.RFC3339), Accounts: body,
		}}}
		store := resetMachine(t, local)
		unlockRemoteGateForTest(t)
		useLocalPeerView(t)
		got, err := runReset(t, store, strings.NewReader(""), "claude", "reset", "held", "--confirm", "held", "--yes")
		if err == nil || !strings.Contains(err.Error(), "only through its daemon's peer link") || !strings.Contains(err.Error(), "no admin path") {
			t.Fatalf("peer-only account: %v\n%s", err, got)
		}
		if len(local.claudeRequests()) != 0 || local.peerViewReads != 1 {
			t.Fatalf("peer-only: %d claude-reset requests, %d peer reads", len(local.claudeRequests()), local.peerViewReads)
		}
	})
	// Names are unique only within one store: the same name on this machine
	// and on a box is two logins, and nothing is guessed.
	t.Run("same name here and on a box", func(t *testing.T) {
		local := &resetDaemonFake{t: t, statuses: claudeResetStatuses(), preview: claimablePreview(), claim: claimedResponse()}
		store := resetMachine(t, local)
		unlockRemoteGateForTest(t)
		box := &resetDaemonFake{t: t, token: "box-token", statuses: claudeResetStatuses(), preview: claimablePreview(), claim: claimedResponse()}
		var scratch bytes.Buffer
		addBoxServer(t, store, &scratch, box.serve(t).URL)
		got, err := runReset(t, store, strings.NewReader(""), "claude", "reset", "held", "--confirm", "held", "--yes")
		if err == nil || !strings.Contains(err.Error(), "is on this machine's daemon and also on box") || !strings.Contains(err.Error(), "--server local or --server <name>") {
			t.Fatalf("collision: %v\n%s", err, got)
		}
		if len(local.claudeRequests()) != 0 || len(box.claudeRequests()) != 0 {
			t.Fatalf("a collision reached a daemon: local %d, box %d", len(local.claudeRequests()), len(box.claudeRequests()))
		}
		// Naming the daemon settles it, and a dry run's hint keeps the name.
		if got, err := runReset(t, store, strings.NewReader(""), "claude", "reset", "HELD", "--dry-run", "--server", "box"); err != nil ||
			!strings.Contains(got, "`sr claude reset HELD --server box` claims it") {
			t.Fatalf("--dry-run --server box: %v\n%s", err, got)
		}
		if got, err := runReset(t, store, strings.NewReader(""), "claude", "reset", "held", "--server", "box"); err != nil || len(box.claudeRequests()) != 2 {
			t.Fatalf("--server box: %v\n%s", err, got)
		}
	})
	// A local daemon that did not answer, or answers no usage-status, may
	// hold the account: the command stops rather than try the box's.
	t.Run("local daemon silent", func(t *testing.T) {
		for name, handler := range map[string]http.HandlerFunc{
			"down": nil,
			"old":  func(w http.ResponseWriter, req *http.Request) { http.NotFound(w, req) },
		} {
			store := resetMachine(t, nil)
			local := httptest.NewServer(handler)
			if handler == nil {
				local.Close()
			} else {
				t.Cleanup(local.Close)
			}
			t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")
			unlockRemoteGateForTest(t)
			box := &resetDaemonFake{t: t, token: "box-token", statuses: claudeResetStatuses(), preview: claimablePreview(), claim: claimedResponse()}
			var scratch bytes.Buffer
			addBoxServer(t, store, &scratch, box.serve(t).URL)
			got, err := runReset(t, store, strings.NewReader(""), "claude", "reset", "held", "--confirm", "held", "--yes")
			if err == nil || !strings.Contains(err.Error(), "this machine's daemon") || !strings.Contains(err.Error(), "nothing was sent") {
				t.Fatalf("%s local daemon: %v\n%s", name, err, got)
			}
			if n := len(box.claudeRequests()); n != 0 {
				t.Fatalf("%s local daemon: fell through to the box (%d requests)", name, n)
			}
		}
	})
	// The peer link reaches a box whose servers.json address cannot be read:
	// the message says why the direct read failed rather than "add it".
	t.Run("peer only, servers.json box unreadable", func(t *testing.T) {
		body, _ := json.Marshal(claudeResetStatuses())
		local := &resetDaemonFake{t: t, statuses: nil, peers: []proxy.PeerUsageView{{
			Name: "home", URL: "https://203.0.113.7:31416", Reachable: true, AsOf: time.Now().UTC().Format(time.RFC3339), Accounts: body,
		}}}
		store := resetMachine(t, local)
		unlockRemoteGateForTest(t)
		useLocalPeerView(t)
		gone := httptest.NewServer(http.NotFoundHandler())
		gone.Close()
		var scratch bytes.Buffer
		addBoxServer(t, store, &scratch, gone.URL)
		got, err := runReset(t, store, strings.NewReader(""), "claude", "reset", "held")
		if err == nil || !strings.Contains(err.Error(), "only through its daemon's peer link") ||
			!strings.Contains(err.Error(), "servers.json lists box, which could not be read directly") || strings.Contains(err.Error(), "add it to servers.json") {
			t.Fatalf("peer-only with an unreadable box: %v\n%s", err, got)
		}
	})
	t.Run("nowhere", func(t *testing.T) {
		local := &resetDaemonFake{t: t, statuses: claudeResetStatuses()}
		store := resetMachine(t, local)
		if got, err := runReset(t, store, strings.NewReader(""), "claude", "reset", "nobody"); err == nil || !strings.Contains(err.Error(), "no daemon this machine can reach holds a Claude account \"nobody\"") {
			t.Fatalf("unknown account: %v\n%s", err, got)
		}
	})
}

// `sr codex reset` is the same one command on the existing rate-limit-reset
// route: ONE redeem of the named account, no preview first, no question.
func TestCodexResetRedeemsInOneCall(t *testing.T) {
	two := 2
	statuses := []remoteServerUsageStatus{{
		ID: "codex@example.com", Email: "codex@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth,
		AuthChecked: true, AuthValid: true, ComplimentaryReset: &accounts.ComplimentaryResetInfo{Known: true, Available: true, Remaining: &two},
		Windows: []accounts.UsageWindow{{Name: "secondary", UsedPercent: 100, LimitWindowSeconds: 604800, ResetAfterSeconds: 200000}},
	}}
	daemon := &resetDaemonFake{t: t, statuses: statuses,
		codexPreview: `{"dry_run":true,"reset":0,"results":[{"email":"codex@example.com","eligible":true,"dry_run":true}]}`,
		codexRedeem: `{"reset":1,"results":[{"email":"codex@example.com","eligible":true,"reset":true,` +
			`"windows_before":[{"Name":"secondary","UsedPercent":100,"LimitWindowSeconds":604800,"ResetAfterSeconds":200000}],` +
			`"windows_after":[{"Name":"secondary","UsedPercent":0,"LimitWindowSeconds":604800,"ResetAfterSeconds":604800}]}]}`,
	}
	store := resetMachine(t, daemon)
	got, err := runReset(t, store, strings.NewReader(""), "codex", "reset", "codex@example.com")
	if err != nil || strings.TrimSpace(got) != "codex@example.com (local): reset credit redeemed — 7d 100% -> 0% used" {
		t.Fatalf("redeem: %v\n%s", err, got)
	}
	daemon.mu.Lock()
	queries := append([]url.Values(nil), daemon.codexQueries...)
	daemon.mu.Unlock()
	if len(queries) != 1 || queries[0].Get("email") != "codex@example.com" || queries[0].Has("dry_run") || queries[0].Has("all") || queries[0].Has("best") ||
		queries[0].Get("anytime") != "true" {
		t.Fatalf("queries = %v; want one anytime redeem of the one named account", queries)
	}

	daemon.mu.Lock()
	daemon.codexRedeem = `{"reset":0,"results":[{"email":"codex@example.com","eligible":false,"error":"not at the weekly limit"}]}`
	daemon.mu.Unlock()
	if got, err := runReset(t, store, strings.NewReader(""), "codex", "reset", "codex@example.com"); err == nil ||
		err.Error() != "codex@example.com (local): not redeemed: not at the weekly limit" {
		t.Fatalf("nothing redeemed: %v\n%s", err, got)
	}
}

// Both verbs are account commands, never launches: `sr codex reset` must not
// start Codex with "reset" as its prompt, nor `sr claude reset` a session on
// a profile named "reset".
func TestResetVerbsAreNeverLaunches(t *testing.T) {
	if !isCodexAccountCommand([]string{"codex", "reset", "codex@example.com"}) {
		t.Fatal("sr codex reset would launch Codex")
	}
	if claudeLaunchesAgent([]string{"reset", "held"}) {
		t.Fatal("sr claude reset would launch Claude")
	}
}

// This machine's own `sr status` rows read the reset from the CLI's usage
// read (the same request, with cedar_ember=1): known when the answer carried
// the block, nil ("?") when it did not. The grant ends a week from now: read
// past its end it is expired, with none left (TestResetInfoMapsTheBlockOntoTheUsageRow).
func TestLocalClaudeUsageReadCarriesTheReset(t *testing.T) {
	endsAt := time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	block := `{"eligible":true,"grants":[{"id":"opus_5_5_launch","resets_total":1,"resets_left":1,"ends_at":"` + endsAt + `","clears":["five_hour"]}]}`
	for _, tc := range []struct {
		name, cedar string
		want        bool
	}{{"with block", `,"cedar_ember":` + block, true}, {"null block", `,"cedar_ember":null`, false}, {"no block", ``, false}} {
		var queries []string
		client := &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			body := `{"type":"message"}`
			if req.URL.Path == "/api/oauth/usage" {
				queries = append(queries, req.URL.RawQuery)
				body = `{"five_hour":{"utilization":10,"resets_at":"2026-09-29T20:00:00Z"},"seven_day_oauth_apps":{"utilization":5,"resets_at":"2026-10-01T00:00:00Z"}` + tc.cedar + `}`
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		windows, reset, err := fetchClaudeUsageWindowsAndReset(context.Background(), client, "access-token")
		if err != nil || len(windows) == 0 {
			t.Fatalf("%s: %v %v", tc.name, windows, err)
		}
		if (reset != nil) != tc.want {
			t.Fatalf("%s: reset = %+v, want present %v", tc.name, reset, tc.want)
		}
		if tc.want && (reset.Remaining == nil || *reset.Remaining != 1 || reset.Source != agentclaude.ResetProgram) {
			t.Fatalf("%s: reset = %+v", tc.name, reset)
		}
		if len(queries) != 1 || queries[0] != "cedar_ember=1" {
			t.Fatalf("%s: usage reads %q; want one, with cedar_ember=1", tc.name, queries)
		}
	}
}

// A row with no id is never a match: an empty email sent to rate-limit-reset
// is the route's smart pick, which would spend some other account's credit.
func TestCodexResetNeverSendsARowWithoutAnID(t *testing.T) {
	two := 2
	statuses := []remoteServerUsageStatus{{
		ID: "", Email: "codex@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth,
		AuthChecked: true, ComplimentaryReset: &accounts.ComplimentaryResetInfo{Known: true, Available: true, Remaining: &two},
	}}
	daemon := &resetDaemonFake{t: t, statuses: statuses,
		codexPreview: `{"dry_run":true,"reset":0,"results":[{"email":"other@example.com","eligible":true,"dry_run":true}]}`,
		codexRedeem:  `{"reset":1,"results":[{"email":"other@example.com","eligible":true,"reset":true}]}`}
	store := resetMachine(t, daemon)
	got, err := runReset(t, store, strings.NewReader("y\n"), "codex", "reset", "codex@example.com", "--confirm", "codex@example.com", "--yes")
	if err == nil {
		t.Fatalf("a row without an id was redeemed:\n%s", got)
	}
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.codexQueries) != 0 {
		t.Fatalf("rate-limit-reset was asked %v", daemon.codexQueries)
	}
}

// Cells that must survive without color: a held reset Anthropic calls
// ineligible says so in words, and Anthropic's own "unavailable" is "?".
func TestClaudeResetCellSaysIneligibleInWords(t *testing.T) {
	one, eligible := 1, false
	held := heldResetInfo()
	held.Eligible = &eligible
	held.Available = false
	held.IneligibleReason = "surface"
	row := srUsageRow{provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth, complimentaryReset: held}
	if got := usageGridResetCell(row).Text; got != "1 left !elig" {
		t.Fatalf("dated ineligible cell = %q", got)
	}
	if got := usageGridResetCellUndated(row).Text; got != "not elig" {
		t.Fatalf("undated ineligible cell = %q", got)
	}
	_ = one
	status, _ := agentclaude.ParseResetStatus([]byte(`{"eligible":false,"ineligible_reason":"unavailable","grants":[{"id":"g","resets_total":1,"resets_left":1}]}`))
	row.complimentaryReset = agentclaude.ResetInfo(status, time.Now())
	if got := usageGridResetCell(row).Text; got != "?" {
		t.Fatalf("unavailable cell = %q, want ?", got)
	}
}

// On a narrow terminal the reset column gives way before Extra usage, $ and
// Auto-reload: at the width where those fitted before the column existed,
// they still do.
func TestClaudeResetColumnNeverPushesOutPaidUsage(t *testing.T) {
	limit, used := 5000.0, 2199.0
	statuses := claudeResetStatuses()
	var rows []srUsageRow
	for _, status := range statuses {
		rows = append(rows, srUsageRow{
			email: status.ID, provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth, planType: "max",
			windows: status.Windows, complimentaryReset: status.ComplimentaryReset, authChecked: true, authValid: true,
			extraUsage: &accounts.ExtraUsageInfo{IsEnabled: true, MonthlyLimit: &limit, UsedCredits: &used},
		})
	}
	full := claudeUsageGridColumns(rows, false, 0)
	keys := func(columns []usageGridColumn) map[string]bool {
		out := map[string]bool{}
		for _, column := range columns {
			out[column.Key] = true
		}
		return out
	}
	// Upstream #529 folded the separate "$" (ExtraSpend) column into Extra
	// usage, so Extra is the paid-usage column the reset must never push out.
	if all := keys(full); !all["Reset"] || !all["Extra"] || all["ExtraSpend"] {
		t.Fatalf("calibration: wide columns = %+v", full)
	}
	var withoutReset []usageGridColumn
	for _, column := range full {
		if column.Key != "Reset" {
			withoutReset = append(withoutReset, column)
		}
	}
	width := usageGridWidth(withoutReset)
	narrow := keys(claudeUsageGridColumns(rows, false, width))
	if !narrow["Extra"] {
		t.Fatalf("at width %d the reset column pushed out paid usage: %v", width, narrow)
	}
	if narrow["Reset"] {
		t.Fatalf("at width %d the dated reset column still fitted: %v", width, narrow)
	}
}

// `sr status` rows whose own read carried no block take the reset this
// machine's daemon publishes for the same profile -- one loopback read, only
// when a row needs it.
func TestLocalRowsTakeTheDaemonsResetWhenTheirReadHasNone(t *testing.T) {
	daemonRow := func(id string) remoteServerUsageStatus {
		return remoteServerUsageStatus{ID: id, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, ComplimentaryReset: heldResetInfo()}
	}
	calls := serveLocalDaemonResets(t, []remoteServerUsageStatus{daemonRow("missing"), daemonRow("known")})
	own := &accounts.ComplimentaryResetInfo{Known: true, Source: agentclaude.ResetProgram}
	rows := []srUsageRow{
		{email: "missing", provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth},
		{email: "known", provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth, complimentaryReset: own},
		{email: "failed", provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth, err: errors.New("x")},
	}
	fillResetsFromLocalDaemon(context.Background(), srRunner{}, rows)
	if *calls != 1 || rows[0].complimentaryReset == nil || rows[1].complimentaryReset != own || rows[2].complimentaryReset != nil {
		t.Fatalf("calls %d, rows %+v %+v %+v", *calls, rows[0].complimentaryReset, rows[1].complimentaryReset, rows[2].complimentaryReset)
	}
	fillResetsFromLocalDaemon(context.Background(), srRunner{}, rows[1:2])
	if *calls != 1 {
		t.Fatal("the daemon was asked although every row had its reset")
	}

	// Against a fake daemon: the real read gives the Claude rows its resets.
	daemon := &resetDaemonFake{t: t, statuses: claudeResetStatuses()}
	resetMachine(t, daemon)
	fetchLocalDaemonResets = srRunner.localDaemonResetRows
	rows = []srUsageRow{
		{email: "held", provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth},
		{email: "unread", provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth},
	}
	fillResetsFromLocalDaemon(context.Background(), srRunner{}, rows)
	if held := rows[0].complimentaryReset; held == nil || held.Remaining == nil || *held.Remaining != 1 || rows[1].complimentaryReset != nil {
		t.Fatalf("daemon resets = %+v, %+v", rows[0].complimentaryReset, rows[1].complimentaryReset)
	}
}

// Codex anytime (PATCHES §46.8) comes from the explicit one-account verb and
// nowhere else: `sr codex reset <name|email>` asks for it, dry run included;
// `sr reset <email>`, its smart pick and --all never do, so they keep the
// daemon's cooked-only rule.
func TestOnlyTheCodexResetVerbRedeemsAnytime(t *testing.T) {
	var mu sync.Mutex
	var queries []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_subrouter/rate-limit-reset" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		queries = append(queries, r.URL.Query())
		mu.Unlock()
		_, _ = io.WriteString(w, `{"reset":1,"results":[{"email":"a@example.com","eligible":true,"reset":true}]}`)
	}))
	defer server.Close()
	take := func() []url.Values {
		mu.Lock()
		defer mu.Unlock()
		got := queries
		queries = nil
		return got
	}
	fixed := &srServerConfig{Name: "test", URL: server.URL}
	for _, args := range [][]string{{"a@example.com"}, nil, {"--all", "--yes"}, {"a@example.com", "--dry-run"}} {
		var out bytes.Buffer
		runner := srRunner{out: &out, errOut: &out, in: strings.NewReader(""), client: server.Client()}
		_ = runner.resetAgainstServer(t.Context(), args, fixed)
		got := take()
		if len(got) == 0 {
			t.Fatalf("sr reset %v sent nothing:\n%s", args, out.String())
		}
		for _, query := range got {
			if query.Has("anytime") {
				t.Fatalf("sr reset %v sent anytime: %v", args, query)
			}
		}
	}
	// Calibration: the explicit verb, against the same daemon, does.
	runner := srRunner{out: io.Discard, errOut: io.Discard, client: server.Client()}
	holder := resetHolder{server: *fixed, label: "server test", row: remoteServerUsageStatus{ID: "a@example.com"}}
	for _, dryRun := range []bool{false, true} {
		_ = runner.codexResetAt(t.Context(), providerResetOptions{provider: accounts.ProviderCodex, account: "a@example.com", dryRun: dryRun}, holder)
		got := take()
		if len(got) != 1 || got[0].Get("anytime") != "true" || got[0].Get("email") != "a@example.com" {
			t.Fatalf("sr codex reset (dry run %v) sent %v; want one anytime request for the named account", dryRun, got)
		}
	}
}

// A daemon from before §46.8 ignores anytime and refuses an account below its
// weekly limit as before; the verb says the daemon needs this build.
func TestCodexResetNamesAnOlderDaemon(t *testing.T) {
	statuses := []remoteServerUsageStatus{{ID: "codex@example.com", Email: "codex@example.com", Provider: accounts.ProviderCodex,
		AuthMode: accounts.AuthModeOAuth, AuthChecked: true, AuthValid: true}}
	daemon := &resetDaemonFake{t: t, statuses: statuses,
		codexRedeem: `{"reset":0,"results":[{"email":"codex@example.com","eligible":false,"error":"account is not cooked; skipping"}]}`}
	store := resetMachine(t, daemon)
	_, err := runReset(t, store, strings.NewReader(""), "codex", "reset", "codex@example.com", "--yes")
	if err == nil || !strings.Contains(err.Error(), "not redeemed: account is not cooked; skipping (the holding daemon runs a build that redeems only an account at its weekly limit") {
		t.Fatalf("older daemon: %v", err)
	}
}

// PATCHES §46.10: `sr reset claude <name>` is `sr claude reset <name>`. It read
// "claude" as a second Codex account ("reset takes at most one account, got 2:
// claude held") and sent nothing.
func TestVerbFirstClaudeResetIsTheClaudeVerb(t *testing.T) {
	daemon := &resetDaemonFake{t: t, statuses: claudeResetStatuses(), preview: claimablePreview(), claim: claimedResponse()}
	store := resetMachine(t, daemon)
	for _, word := range []string{"claude", "Claude", "anthropic"} {
		got, err := runReset(t, store, strings.NewReader(""), "reset", word, "held", "--dry-run")
		if err != nil || !strings.Contains(got, "Claude account held, held by this machine's daemon (dry run)") {
			t.Fatalf("sr reset %s held --dry-run: %v\n%s", word, err, got)
		}
	}
	for _, request := range daemon.claudeRequests() {
		if request.DryRun == nil || !*request.DryRun {
			t.Fatalf("--dry-run sent a claim: %+v", request)
		}
	}
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	got, err := runReset(t, store, devNull, "reset", "claude", "held")
	if err != nil || strings.TrimSpace(got) != claimedLine("local") {
		t.Fatalf("sr reset claude held: %v\n%s", err, got)
	}
	requests := daemon.claudeRequests()
	if len(requests) != 4 || requests[3].DryRun == nil || *requests[3].DryRun || requests[3].Account != "held" {
		t.Fatalf("requests = %+v; want three dry runs, then one claim of held", requests)
	}
}

// The fold runs before the credential-source routing: with a server selected,
// `sr reset` goes to that server's Codex route, and `sr reset claude held`
// must instead reach the daemon that holds held.
func TestVerbFirstClaudeResetIsNeverRoutedAsACodexReset(t *testing.T) {
	local := &resetDaemonFake{t: t, statuses: nil}
	store := resetMachine(t, local)
	unlockRemoteGateForTest(t)
	box := &resetDaemonFake{t: t, token: "box-token", statuses: claudeResetStatuses(), preview: claimablePreview(), claim: claimedResponse()}
	var scratch bytes.Buffer
	addBoxServer(t, store, &scratch, box.serve(t).URL)
	t.Setenv("SUBROUTER_CODEX_SERVER", "box")
	got, err := runReset(t, store, strings.NewReader(""), "reset", "claude", "held")
	if err != nil || len(box.claudeRequests()) != 1 || strings.TrimSpace(got) != claimedLine("server box") {
		t.Fatalf("err %v, box claude-reset requests %d\n%s", err, len(box.claudeRequests()), got)
	}
	box.mu.Lock()
	defer box.mu.Unlock()
	if len(box.codexQueries) != 0 {
		t.Fatalf("routed to the box's Codex reset: %v", box.codexQueries)
	}
}

// The fold is normalizeProviderAddArgs's for reset: `reset <provider> …` is
// `<provider> reset …`, and nothing else moves.
func TestProviderResetFoldIsTheAddFold(t *testing.T) {
	for input, want := range map[string]string{
		"reset claude held --dry-run": "claude reset held --dry-run",
		"reset Anthropic held":        "claude reset held",
		"reset codex a@example.com":   "codex reset a@example.com",
		"reset OpenAI a@example.com":  "codex reset a@example.com",
		"reset chatgpt":               "codex reset",
		"reset":                       "reset",
		"reset a@example.com":         "reset a@example.com",
		"reset --dry-run claude held": "reset --dry-run claude held",
		"claude reset held":           "claude reset held",
		"reset held":                  "reset held",
	} {
		args := strings.Fields(input)
		before := strings.Join(args, " ")
		if got := strings.Join(normalizeProviderResetArgs(args), " "); got != want {
			t.Errorf("%q folded to %q, want %q", input, got, want)
		}
		if strings.Join(args, " ") != before {
			t.Errorf("%q: the fold mutated the caller's arguments", input)
		}
	}
}

// `sr reset codex <email>` is `sr codex reset <email>`, as `sr add codex` is
// `sr codex add` (§46.11): one anytime redeem of the named account.
func TestVerbFirstCodexResetIsTheCodexVerb(t *testing.T) {
	two := 2
	statuses := []remoteServerUsageStatus{{
		ID: "codex@example.com", Email: "codex@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth,
		AuthChecked: true, AuthValid: true, ComplimentaryReset: &accounts.ComplimentaryResetInfo{Known: true, Available: true, Remaining: &two},
		Windows: []accounts.UsageWindow{{Name: "secondary", UsedPercent: 100, LimitWindowSeconds: 604800, ResetAfterSeconds: 200000}},
	}}
	daemon := &resetDaemonFake{t: t, statuses: statuses,
		codexRedeem: `{"reset":1,"results":[{"email":"codex@example.com","eligible":true,"reset":true,` +
			`"windows_before":[{"Name":"secondary","UsedPercent":100,"LimitWindowSeconds":604800,"ResetAfterSeconds":200000}],` +
			`"windows_after":[{"Name":"secondary","UsedPercent":0,"LimitWindowSeconds":604800,"ResetAfterSeconds":604800}]}]}`,
	}
	store := resetMachine(t, daemon)
	for _, word := range []string{"codex", "OpenAI"} {
		got, err := runReset(t, store, strings.NewReader(""), "reset", word, "codex@example.com")
		if err != nil || strings.TrimSpace(got) != "codex@example.com (local): reset credit redeemed — 7d 100% -> 0% used" {
			t.Fatalf("sr reset %s codex@example.com: %v\n%s", word, err, got)
		}
	}
	daemon.mu.Lock()
	queries := append([]url.Values(nil), daemon.codexQueries...)
	daemon.mu.Unlock()
	if len(queries) != 2 || queries[0].Get("email") != "codex@example.com" || queries[0].Get("anytime") != "true" || queries[0].Has("best") {
		t.Fatalf("queries = %v; want two anytime redeems of the named account", queries)
	}
}

// The owner's rule (2026-10-01, §46.11): a reset asks first when its week has
// more than 10% left or rolls on its own within a day; otherwise it just
// happens.
func TestResetWasteReasons(t *testing.T) {
	week := func(name string, used float64, roll time.Duration) accounts.UsageWindow {
		return accounts.UsageWindow{Name: name, UsedPercent: used, LimitWindowSeconds: 604800, ResetAfterSeconds: int64(roll / time.Second)}
	}
	fiveHour := accounts.UsageWindow{Name: "5h", UsedPercent: 100, LimitWindowSeconds: 5 * 3600, ResetAfterSeconds: 3600}
	fable := accounts.UsageWindow{Name: agentclaude.FableWindowName, UsedPercent: 100, LimitWindowSeconds: 604800, ResetAfterSeconds: 3600, Feature: agentclaude.FableFeature}
	for _, tc := range []struct {
		name     string
		provider accounts.Provider
		windows  []accounts.UsageWindow
		want     string
	}{
		{"claude spent", accounts.ProviderClaude, []accounts.UsageWindow{fiveHour, week("7d", 100, 72*time.Hour)}, ""},
		{"claude at 10% left", accounts.ProviderClaude, []accounts.UsageWindow{week("7d", 90, 72*time.Hour)}, ""},
		{"claude at 11% left", accounts.ProviderClaude, []accounts.UsageWindow{week("7d", 89, 72*time.Hour)}, "its week is 89% used (11% left)"},
		{"claude rolls in 10h", accounts.ProviderClaude, []accounts.UsageWindow{week("7d", 100, 10*time.Hour)}, "its week rolls on its own in 10h"},
		{"claude rolls in a day", accounts.ProviderClaude, []accounts.UsageWindow{week("7d", 100, 24*time.Hour)}, ""},
		{"claude both", accounts.ProviderClaude, []accounts.UsageWindow{week("7d", 50, 10*time.Hour)}, "its week is 50% used (50% left), and its week rolls on its own in 10h"},
		// The Fable week is its own meter: it never stands in for the 7d.
		{"claude unread week", accounts.ProviderClaude, []accounts.UsageWindow{fiveHour, fable}, "its weekly meter has not been read"},
		{"codex spent", accounts.ProviderCodex, []accounts.UsageWindow{week("secondary", 100, 55*time.Hour)}, ""},
		{"codex half", accounts.ProviderCodex, []accounts.UsageWindow{week("secondary", 40, 55*time.Hour)}, "its week is 40% used (60% left)"},
		{"codex unread", accounts.ProviderCodex, nil, "its weekly meter has not been read"},
		// A usage-based seat's monthly spend cap is no week (§42), however
		// spent: a reset credit cannot lift it.
		{"codex spend seat", accounts.ProviderCodex, []accounts.UsageWindow{
			{Name: accounts.CodexSpendWindowName, UsedPercent: 95, LimitWindowSeconds: 30 * 86400, ResetAfterSeconds: 20 * 86400},
			{Name: accounts.CodexSpendReachedWindowName, UsedPercent: 100}}, "its weekly meter has not been read"},
	} {
		if got := strings.Join(resetWasteReasons(tc.provider, tc.windows), ", and "); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A reset its week does not need yet asks y/N, and nothing is sent until the
// answer is y. Without a terminal it refuses; --yes answers it. A dry run
// never asks. A spent week (the fixture's 95%) just resets, which the other
// claim tests cover.
func TestProviderResetAsksOnlyWhenPartlyWasted(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	t.Run("claude", func(t *testing.T) {
		statuses := claudeResetStatuses()
		statuses[0].Windows[1].UsedPercent = 54
		daemon := &resetDaemonFake{t: t, statuses: statuses, preview: claimablePreview(), claim: claimedResponse()}
		store := resetMachine(t, daemon)
		const why = "held (local): its week is 54% used (46% left)"
		for _, spelling := range [][]string{{"claude", "reset", "held"}, {"reset", "claude", "held"}} {
			if got, err := runReset(t, store, devNull, spelling...); err == nil ||
				err.Error() != "held (local): not reset: its week is 54% used (46% left); re-run with --yes to reset it anyway" {
				t.Fatalf("sr %v without a terminal: %v\n%s", spelling, err, got)
			}
		}
		got, err := runReset(t, store, strings.NewReader("n\n"), "claude", "reset", "held")
		if err == nil || err.Error() != "aborted; nothing was reset" || !strings.Contains(got, why+". Reset it now? [y/N]: ") {
			t.Fatalf("answered n: %v\n%s", err, got)
		}
		if n := len(daemon.claudeRequests()); n != 0 {
			t.Fatalf("%d requests before a yes", n)
		}
		if got, err := runReset(t, store, strings.NewReader("y\n"), "claude", "reset", "held"); err != nil || !strings.HasSuffix(strings.TrimSpace(got), claimedLine("local")) {
			t.Fatalf("answered y: %v\n%s", err, got)
		}
		if got, err := runReset(t, store, devNull, "claude", "reset", "held", "--yes"); err != nil || strings.TrimSpace(got) != claimedLine("local") {
			t.Fatalf("--yes: %v\n%s", err, got)
		}
		if got, err := runReset(t, store, devNull, "claude", "reset", "held", "--dry-run"); err != nil || strings.Contains(got, "[y/N]") {
			t.Fatalf("dry run: %v\n%s", err, got)
		}
		requests := daemon.claudeRequests()
		if len(requests) != 3 || *requests[0].DryRun || *requests[1].DryRun || !*requests[2].DryRun {
			t.Fatalf("requests = %+v; want two claims, then one dry run", requests)
		}

		statuses[0].Windows[1].UsedPercent, statuses[0].Windows[1].ResetAfterSeconds = 100, 10*3600
		daemon.mu.Lock()
		daemon.statuses = statuses
		daemon.mu.Unlock()
		if got, err := runReset(t, store, devNull, "claude", "reset", "held"); err == nil ||
			!strings.Contains(err.Error(), "held (local): not reset: its week rolls on its own in ") {
			t.Fatalf("rolls within a day: %v\n%s", err, got)
		}
	})
	t.Run("codex", func(t *testing.T) {
		statuses := []remoteServerUsageStatus{{ID: "codex@example.com", Email: "codex@example.com", Provider: accounts.ProviderCodex,
			AuthMode: accounts.AuthModeOAuth, AuthChecked: true, AuthValid: true,
			Windows: []accounts.UsageWindow{{Name: "secondary", UsedPercent: 40, LimitWindowSeconds: 604800, ResetAfterSeconds: 200000}}}}
		daemon := &resetDaemonFake{t: t, statuses: statuses,
			codexRedeem: `{"reset":1,"results":[{"email":"codex@example.com","eligible":true,"reset":true,"early_use":true}]}`}
		store := resetMachine(t, daemon)
		if got, err := runReset(t, store, devNull, "codex", "reset", "codex@example.com"); err == nil ||
			err.Error() != "codex@example.com (local): not reset: its week is 40% used (60% left); re-run with --yes to reset it anyway" {
			t.Fatalf("without a terminal: %v\n%s", err, got)
		}
		daemon.mu.Lock()
		before := len(daemon.codexQueries)
		daemon.mu.Unlock()
		if before != 0 {
			t.Fatalf("%d redeems before a yes", before)
		}
		if got, err := runReset(t, store, strings.NewReader("y\n"), "codex", "reset", "codex@example.com"); err != nil || !strings.Contains(got, "reset credit redeemed") {
			t.Fatalf("answered y: %v\n%s", err, got)
		}
	})
}

// --dry-run is a testing flag (the owner, 2026-10-01): no help or usage text
// for a reset verb names it. Only the unrelated attest-legacy and account
// import commands keep theirs.
func TestResetHelpNeverNamesDryRun(t *testing.T) {
	isolateGate(t)
	var resetHelp bytes.Buffer
	if err := (srRunner{out: &resetHelp, errOut: &resetHelp}).resetAgainstServer(t.Context(), []string{"-h"}, nil); err != nil {
		t.Fatal(err)
	}
	texts := map[string]string{
		"sr help":            visibleUsageText("sr"),
		"srHelp":             srHelp,
		"sr claude help":     srClaudeHelp,
		"sr reset -h":        resetHelp.String(),
		"sr claude reset -h": providerResetUsage(accounts.ProviderClaude),
		"sr codex reset -h":  providerResetUsage(accounts.ProviderCodex),
		"sr codex usage":     codexAccountUsage,
	}
	unlockRemoteGateForTest(t)
	texts["sr claude reset -h, keyed"] = providerResetUsage(accounts.ProviderClaude)
	for name, text := range texts {
		for _, line := range strings.Split(text, "\n") {
			for _, part := range strings.Split(line, ", ") {
				if strings.Contains(part, "dry-run") && !strings.Contains(part, "attest-legacy") && !strings.Contains(part, "account import") {
					t.Errorf("%s names --dry-run: %q", name, strings.TrimSpace(part))
				}
			}
		}
	}
	help := visibleUsageText("sr")
	for _, want := range []string{"sr claude reset <name|email>", "sr reset claude <name|email>", "sr codex reset <name|email>", "sr reset codex <name|email>"} {
		if !strings.Contains(help, want) {
			t.Errorf("sr help is missing %q", want)
		}
	}
}
