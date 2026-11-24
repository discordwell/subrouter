package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
)

// The cedar_ember block as Anthropic answered it on 2026-09-29 (read-only,
// ?cedar_ember=1&skip_spend=1), ids and times kept, nothing else changed.
const liveResetBlock = `{
  "eligible": true,
  "ineligible_reason": null,
  "at_limit": false,
  "exhausted": [],
  "grants": [{
    "id": "opus_5_5_launch",
    "label": "Claude Opus 5.5 launch: one usage-limit reset for Pro and Max",
    "resets_total": 1,
    "resets_left": 1,
    "starts_at": "2026-09-24T16:00:00+00:00",
    "ends_at": "2026-10-22T16:00:00+00:00",
    "clears": ["five_hour", "seven_day", "seven_day_overage_included"],
    "paused": false,
    "usable_now": false,
    "use_requires_limit": true,
    "percent_used": {"five_hour": 12, "seven_day": 40},
    "blocking": []
  }],
  "next_grant_id": "opus_5_5_launch",
  "weekly_resets_at": "2026-10-01T09:00:00+00:00",
  "cooldown_until": null
}`

func TestParseResetStatusDecodesTheLiveBlock(t *testing.T) {
	status, err := ParseResetStatus(json.RawMessage(liveResetBlock))
	if err != nil || status == nil {
		t.Fatalf("ParseResetStatus = %+v, %v", status, err)
	}
	if !status.Eligible || status.IneligibleReason != "" || status.AtLimit {
		t.Fatalf("status flags = %+v", status)
	}
	if len(status.Grants) != 1 {
		t.Fatalf("grants = %+v", status.Grants)
	}
	grant := status.Grants[0]
	if grant.ID != "opus_5_5_launch" || grant.ResetsLeft != 1 || grant.ResetsTotal != 1 ||
		grant.EndsAt != "2026-10-22T16:00:00+00:00" || grant.UsableNow || !grant.UseRequiresLimit ||
		strings.Join(grant.Clears, ",") != "five_hour,seven_day,seven_day_overage_included" {
		t.Fatalf("grant = %+v", grant)
	}
	if status.NextGrantID != grant.ID || status.WeeklyResetsAt == "" || status.CooldownUntil != "" {
		t.Fatalf("status = %+v", status)
	}
	ends, ok := ParseResetTime(grant.EndsAt)
	if !ok || !ends.Equal(time.Date(2026, 10, 22, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("ends_at parsed as %v %v", ends, ok)
	}
}

// Absent and null are "not read" (nil, no error), never a block that says
// "no reset"; a block Claude Code would call malformed is an error; a grant
// Claude Code would drop is dropped, and defaults are Claude Code's.
func TestParseResetStatusReadsAbsentNullAndDriftAsClaudeCodeDoes(t *testing.T) {
	for _, raw := range []string{"", "null", "  null  "} {
		status, err := ParseResetStatus(json.RawMessage(raw))
		if status != nil || err != nil {
			t.Fatalf("%q: got %+v, %v; want nil, nil", raw, status, err)
		}
	}
	for _, raw := range []string{`"yes"`, `[]`, `{}`, `{"eligible":"true"}`, `{"eligible":null}`} {
		if status, err := ParseResetStatus(json.RawMessage(raw)); err == nil {
			t.Fatalf("%s: got %+v, want malformed", raw, status)
		}
	}
	status, err := ParseResetStatus(json.RawMessage(`{"eligible":false,"ineligible_reason":"no_grant","grants":[
		{"id":"BAD ID","resets_left":1},
		{"id":"ok_one","resets_left":1.5},
		{"id":"ok_two","resets_left":-1},
		{"id":"kept","resets_left":0}
	],"next_grant_id":"not_there"}`))
	if err != nil {
		t.Fatal(err)
	}
	if status.Eligible || status.IneligibleReason != "no_grant" {
		t.Fatalf("status = %+v", status)
	}
	if len(status.Grants) != 1 || status.Grants[0].ID != "kept" || !status.Grants[0].UseRequiresLimit {
		t.Fatalf("grants = %+v, want only the valid one with use_requires_limit defaulting true", status.Grants)
	}
	if status.NextGrantID != "" {
		t.Fatalf("next_grant_id naming no grant was kept: %q", status.NextGrantID)
	}
}

func TestUsageResponseCarriesTheResetBlockAndNullReadsUnknown(t *testing.T) {
	var usage UsageResponse
	body := `{"five_hour":{"utilization":12,"resets_at":"2026-09-29T20:00:00Z"},"cedar_ember":` + liveResetBlock + `}`
	if err := json.Unmarshal([]byte(body), &usage); err != nil {
		t.Fatal(err)
	}
	status, err := usage.ResetStatus()
	if err != nil || status == nil || len(status.Grants) != 1 {
		t.Fatalf("ResetStatus = %+v, %v", status, err)
	}
	var plain UsageResponse
	if err := json.Unmarshal([]byte(`{"cedar_ember":null}`), &plain); err != nil {
		t.Fatal(err)
	}
	if status, err := plain.ResetStatus(); status != nil || err != nil {
		t.Fatalf("plain usage read: %+v, %v; want nil, nil", status, err)
	}
	// A block that does not decode must not fail the usage read routing needs.
	var drifted UsageResponse
	if err := json.Unmarshal([]byte(`{"five_hour":{"utilization":1,"resets_at":""},"cedar_ember":{"eligible":7}}`), &drifted); err != nil {
		t.Fatalf("a drifted cedar_ember failed the whole usage read: %v", err)
	}
	if drifted.FiveHour == nil {
		t.Fatal("a drifted cedar_ember lost the five_hour window")
	}
}

func TestResetInfoMapsTheBlockOntoTheUsageRow(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	status, _ := ParseResetStatus(json.RawMessage(liveResetBlock))
	info := ResetInfo(status, now)
	if info == nil || !info.Known || !info.Available || info.Source != ResetProgram ||
		info.Remaining == nil || *info.Remaining != 1 || info.Total == nil || *info.Total != 1 ||
		info.Eligible == nil || !*info.Eligible || info.ExpiresAt != "2026-10-22T16:00:00+00:00" ||
		len(info.Clears) != 3 || info.Consumed || info.Status != "" {
		t.Fatalf("info = %+v", info)
	}

	used, _ := ParseResetStatus(json.RawMessage(`{"eligible":true,"grants":[{"id":"g","resets_total":1,"resets_left":0,"ends_at":"2026-10-22T16:00:00+00:00"}]}`))
	info = ResetInfo(used, now)
	if info.Available || !info.Consumed || *info.Remaining != 0 || info.Status != "" {
		t.Fatalf("used info = %+v", info)
	}

	expired := ResetInfo(status, time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC))
	if expired.Available || *expired.Remaining != 0 || expired.Status != "expired" {
		t.Fatalf("expired info = %+v", expired)
	}

	none, _ := ParseResetStatus(json.RawMessage(`{"eligible":false,"ineligible_reason":"no_grant"}`))
	info = ResetInfo(none, now)
	if info.Available || info.Eligible == nil || *info.Eligible || info.IneligibleReason != "no_grant" || info.Remaining != nil {
		t.Fatalf("no-grant info = %+v", info)
	}

	if ResetInfo(nil, now) != nil {
		t.Fatal("an unread block produced a row")
	}
	unreadable := UnreadableResetInfo()
	if unreadable.Known || unreadable.Status != "unreadable" || unreadable.IneligibleReason != ResetUnreadableReason {
		t.Fatalf("unreadable = %+v", unreadable)
	}
}

// The Fable 5.2 launch block's shape as the pool read it on 2026-11-09: every
// grant clears only the Fable limit. Ids and resets_left are illustrative.
const liveFableResetBlock = `{
  "eligible": true,
  "ineligible_reason": null,
  "at_limit": false,
  "exhausted": [],
  "grants": [{
    "id": "fable_5_2_launch",
    "label": "New model launch: one Fable reset for Max, Team Premium and Enterprise Premium",
    "resets_total": 2,
    "resets_left": 2,
    "ends_at": "2027-03-09T07:59:00Z",
    "clears": ["seven_day_overage_included"],
    "paused": false,
    "usable_now": false,
    "use_requires_limit": true
  }, {
    "id": "fable_5_2_launch_b",
    "label": "New model launch: one Fable reset for Max, Team Premium and Enterprise Premium",
    "resets_total": 1,
    "resets_left": 1,
    "ends_at": "2027-03-09T07:59:00Z",
    "clears": ["seven_day_overage_included"],
    "paused": false,
    "usable_now": false,
    "use_requires_limit": true
  }],
  "next_grant_id": "fable_5_2_launch"
}`

// Only a live grant that clears the general week holds the week, read over
// every grant whatever their order and whichever one the row shows.
func TestResetInfoHoldsTheWeekOnlyOnGrantsThatClearIt(t *testing.T) {
	now := time.Date(2026, 11, 9, 20, 0, 0, 0, time.UTC)
	read := func(t *testing.T, block string) *accounts.ComplimentaryResetInfo {
		t.Helper()
		status, err := ParseResetStatus(json.RawMessage(block))
		if err != nil || status == nil {
			t.Fatalf("calibration: %s decoded as %+v, %v", block, status, err)
		}
		return ResetInfo(status, now)
	}
	expect := func(t *testing.T, info *accounts.ComplimentaryResetInfo, week string) {
		t.Helper()
		if info.WeekHeldUntil != week {
			t.Fatalf("week held until %q, want %q", info.WeekHeldUntil, week)
		}
	}

	t.Run("the Fable 5.2 launch block", func(t *testing.T) {
		info := read(t, liveFableResetBlock)
		if !info.Known || !info.Available || info.Remaining == nil || *info.Remaining != 3 {
			t.Fatalf("calibration: the Fable block read as %+v", info)
		}
		expect(t, info, "")
	})

	grant := func(id string, clears []string, left int, ends string, paused bool) map[string]any {
		g := map[string]any{"id": id, "resets_total": 1, "resets_left": left, "ends_at": ends, "paused": paused}
		if clears != nil {
			g["clears"] = clears
		}
		return g
	}
	block := func(eligible bool, grants ...map[string]any) string {
		raw, _ := json.Marshal(map[string]any{"eligible": eligible, "grants": grants})
		return string(raw)
	}
	fable := []string{"seven_day_overage_included"}
	week := []string{"five_hour", "seven_day", "seven_day_overage_included"}
	fableSoon := grant("fable_soon", fable, 1, "2026-11-20T00:00:00Z", false)
	weekLater := grant("week_later", week, 1, "2026-12-01T00:00:00Z", false)
	// Each of these clears the week sooner than weekLater but cannot be
	// claimed: spent, expired, paused.
	spent := grant("week_spent", week, 0, "2026-11-12T00:00:00Z", false)
	expired := grant("week_expired", week, 1, "2026-11-01T00:00:00Z", false)
	paused := grant("week_paused", week, 1, "2026-11-13T00:00:00Z", true)

	t.Run("a Fable grant first, a week grant later", func(t *testing.T) {
		info := read(t, block(true, fableSoon, weekLater))
		if info.ExpiresAt != "2026-11-20T00:00:00Z" {
			t.Fatalf("calibration: the row shows the grant ending %q, want the Fable grant's", info.ExpiresAt)
		}
		expect(t, info, "2026-12-01T00:00:00Z")
		expect(t, read(t, block(true, fableSoon, weekLater, spent, expired, paused)), "2026-12-01T00:00:00Z")
		expect(t, read(t, block(true, paused, expired, spent, weekLater, fableSoon)), "2026-12-01T00:00:00Z")
	})
	t.Run("two live week grants hold to the sooner, in either order", func(t *testing.T) {
		weekSoon := grant("week_soon", week, 1, "2026-11-25T00:00:00Z", false)
		expect(t, read(t, block(true, weekLater, weekSoon)), "2026-11-25T00:00:00Z")
		expect(t, read(t, block(true, weekSoon, weekLater)), "2026-11-25T00:00:00Z")
	})
	t.Run("the week grant claimed, a Fable grant left", func(t *testing.T) {
		expect(t, read(t, block(true, fableSoon, grant("week_later", week, 0, "2026-12-01T00:00:00Z", false))), "")
	})
	t.Run("a grant with no clears list counts as a week grant", func(t *testing.T) {
		expect(t, read(t, block(true, grant("unlisted", nil, 1, "2026-11-25T00:00:00Z", false))), "2026-11-25T00:00:00Z")
	})
	t.Run("ineligible holds nothing", func(t *testing.T) {
		expect(t, read(t, block(false, fableSoon, weekLater)), "")
	})
}

// A grant's kind is read from what it clears. The pool read two shapes on
// 2026-11-22: one reset per account that clears seven_day and the Fable limit
// (use by 2026-12-10), and Fable resets that clear the Fable limit alone (one
// account's also cleared five_hour).
func TestResetGrantKindIsWhatItsClaimGivesBack(t *testing.T) {
	for _, tc := range []struct {
		clears []string
		want   ResetKind
	}{
		{[]string{"seven_day", "seven_day_overage_included"}, ResetKindWeek},
		{[]string{"five_hour", "seven_day", "seven_day_overage_included"}, ResetKindWeek},
		{nil, ResetKindWeek},
		{[]string{"seven_day_overage_included"}, ResetKindFable},
		{[]string{"five_hour", "seven_day_overage_included"}, ResetKindFable},
		{[]string{"five_hour"}, ResetKindOther},
		{[]string{"seven_day_opus"}, ResetKindOther},
	} {
		if got := (ResetGrant{Clears: tc.clears}).Kind(); got != tc.want {
			t.Errorf("clears %v: kind %q, want %q", tc.clears, got, tc.want)
		}
	}
}

// The row splits what is left by kind, over the grants that count toward
// Remaining with a reset left: the week reset with a Fable reset queued
// behind it reads 1 and 1, not 2 of one kind.
func TestResetInfoCountsWhatIsLeftByKind(t *testing.T) {
	now := time.Date(2026, 11, 22, 20, 0, 0, 0, time.UTC)
	grant := func(id string, clears []string, left int, ends string) map[string]any {
		return map[string]any{"id": id, "resets_total": max(left, 1), "resets_left": left, "ends_at": ends, "clears": clears}
	}
	read := func(t *testing.T, grants ...map[string]any) *accounts.ComplimentaryResetInfo {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"eligible": true, "grants": grants})
		status, err := ParseResetStatus(raw)
		if err != nil || status == nil {
			t.Fatalf("calibration: %s decoded as %+v, %v", raw, status, err)
		}
		return ResetInfo(status, now)
	}
	week := grant("week", []string{"seven_day", "seven_day_overage_included"}, 1, "2026-12-10T22:00:00Z")
	fable := grant("fable", []string{"seven_day_overage_included"}, 2, "2027-03-09T07:59:00Z")
	for _, tc := range []struct {
		name   string
		grants []map[string]any
		want   map[string]int
	}{
		{"a week reset and Fable resets", []map[string]any{week, fable}, map[string]int{"week": 1, "fable": 2}},
		{"Fable resets alone", []map[string]any{fable}, map[string]int{"fable": 2}},
		{"the week reset claimed", []map[string]any{grant("week", []string{"seven_day"}, 0, "2026-12-10T22:00:00Z"), fable}, map[string]int{"fable": 2}},
		{"an expired week reset", []map[string]any{grant("week", []string{"seven_day"}, 1, "2026-11-01T00:00:00Z"), fable}, map[string]int{"fable": 2}},
		{"another kind", []map[string]any{grant("short", []string{"five_hour"}, 1, "2026-12-01T00:00:00Z")}, map[string]int{"other": 1}},
		{"nothing left", []map[string]any{grant("week", []string{"seven_day"}, 0, "2026-12-10T22:00:00Z")}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := read(t, tc.grants...)
			if !maps.Equal(info.RemainingByKind, tc.want) {
				t.Fatalf("remaining by kind %v, want %v", info.RemainingByKind, tc.want)
			}
			sum := 0
			for _, left := range info.RemainingByKind {
				sum += left
			}
			if info.Remaining == nil || *info.Remaining != sum {
				t.Fatalf("remaining %v; the kinds add up to %d", info.Remaining, sum)
			}
		})
	}
}

func TestCredentialCannotReadResetsOnlyForAScopeListWithoutProfile(t *testing.T) {
	for _, tc := range []struct {
		name       string
		credential *CredentialInfo
		want       bool
	}{
		{"setup token", &CredentialInfo{AccessToken: "t", Scopes: []string{SetupTokenScope}}, true},
		{"no scopes recorded", &CredentialInfo{AccessToken: "t", RefreshToken: "r"}, false},
		{"full login", &CredentialInfo{AccessToken: "t", RefreshToken: "r", Scopes: []string{"user:inference", "user:profile"}}, false},
		{"nil", nil, false},
	} {
		if got := CredentialCannotReadResets(tc.credential); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The sweep's usage read opts into the block at no extra request, keeps the
// spend block (no skip_spend), and presents Claude Code's identity.
func TestFetchUsageOptsIntoTheResetBlock(t *testing.T) {
	var got *http.Request
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		got = req
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(`{"cedar_ember":` + liveResetBlock + `}`))}, nil
	})}
	usage, err := FetchUsage(context.Background(), client, "access-token")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("usage read made %d requests, want 1", calls)
	}
	if got.URL.Path != "/api/oauth/usage" || got.URL.Query().Get("cedar_ember") != "1" || got.URL.Query().Has("skip_spend") {
		t.Fatalf("usage read URL = %s; want cedar_ember=1 and no skip_spend", got.URL)
	}
	if ua := got.Header.Get("User-Agent"); ua != ResetClientUserAgent {
		t.Fatalf("User-Agent = %q, want %q", ua, ResetClientUserAgent)
	}
	if status, err := usage.ResetStatus(); err != nil || status == nil {
		t.Fatalf("the read's block did not decode: %+v %v", status, err)
	}
}

type recordedResetRequest struct {
	method string
	path   string
	query  string
	header http.Header
	body   []byte
}

func recordingResetClient(t *testing.T, answer func(req *http.Request) (int, string)) (*http.Client, func() []recordedResetRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []recordedResetRequest
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body []byte
		if req.Body != nil {
			body, _ = io.ReadAll(req.Body)
		}
		mu.Lock()
		seen = append(seen, recordedResetRequest{method: req.Method, path: req.URL.Path, query: req.URL.RawQuery, header: req.Header.Clone(), body: body})
		mu.Unlock()
		status, text := answer(req)
		return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(text))}, nil
	})}
	return client, func() []recordedResetRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedResetRequest(nil), seen...)
	}
}

// The claim is Claude Code 2.1.284's request, byte for byte where it counts:
// the path under the organization, the three-field body in its order, the
// OAuth bearer and beta, and Claude Code's User-Agent.
func TestClaimResetSendsClaudeCodesRequest(t *testing.T) {
	client, seen := recordingResetClient(t, func(*http.Request) (int, string) {
		return http.StatusOK, `{"result":"reset","resets_left":0,"cleared":["five_hour","seven_day"]}`
	})
	claim, err := ClaimReset(context.Background(), client, "access-token",
		"0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0", "opus_5_5_launch", "4b7e0a4e-2f0c-4a57-9d8e-2a1f0c3b4d5e")
	if err != nil {
		t.Fatal(err)
	}
	requests := seen()
	if len(requests) != 1 {
		t.Fatalf("claim sent %d requests, want exactly 1", len(requests))
	}
	req := requests[0]
	if req.method != http.MethodPost || req.path != "/api/organizations/0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0/reset_rate_limits" || req.query != "" {
		t.Fatalf("claim = %s %s?%s", req.method, req.path, req.query)
	}
	if want := `{"program":"cedar_ember","grant_id":"opus_5_5_launch","request_id":"4b7e0a4e-2f0c-4a57-9d8e-2a1f0c3b4d5e"}`; string(req.body) != want {
		t.Fatalf("claim body = %s\nwant       %s", req.body, want)
	}
	for header, want := range map[string]string{
		"Authorization":  "Bearer access-token",
		"Anthropic-Beta": "oauth-2025-04-20",
		"Content-Type":   "application/json",
		"User-Agent":     "claude-cli/2.1.284 (external, cli)",
	} {
		if got := req.header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if claim.Result != ResetResultReset || claim.ResetsLeft == nil || *claim.ResetsLeft != 0 || len(claim.Cleared) != 2 || !claim.Settled() {
		t.Fatalf("claim = %+v", claim)
	}
}

// Every answer maps to one result, verbatim where Anthropic gave one, and no
// answer -- a 401 included -- is ever retried: one call, one request. Only
// reset, already_used, not_limited and ineligible settle a claim; unavailable
// (a result outside the enum included) and error may have landed, as Claude
// Code 2.1.284 treats them.
func TestClaimResetMapsEveryAnswerAndNeverRetries(t *testing.T) {
	for _, tc := range []struct {
		status   int
		body     string
		want     string
		reason   string
		settled  bool
		landed   bool
		err      bool
		answered string
	}{
		{200, `{"result":"reset","resets_left":0}`, ResetResultReset, "", true, false, false, ""},
		{200, `{"result":"already_used","reason":"already_used"}`, ResetResultAlreadyUsed, "already_used", true, false, false, ""},
		{200, `{"result":"not_limited","reason":"not_limited"}`, ResetResultNotLimited, "not_limited", true, false, false, ""},
		{200, `{"result":"cooldown","cooldown_until":"2026-09-30T00:00:00Z"}`, ResetResultCooldown, "", false, false, false, ""},
		{200, `{"result":"ineligible","reason":"surface"}`, ResetResultIneligible, "surface", true, false, false, ""},
		{200, `{"result":"ineligible","reason":"cli_version"}`, ResetResultIneligible, "cli_version", true, false, false, ""},
		{200, `{"result":"unavailable","reason":"stamp_indeterminate"}`, ResetResultUnavailable, "stamp_indeterminate", false, true, false, ""},
		{200, `{"result":"unavailable","reason":"reset_unconfirmed","cleared":["five_hour"]}`, ResetResultUnavailable, "reset_unconfirmed", false, true, false, ""},
		{200, `{"result":"something_new"}`, ResetResultUnavailable, "", false, true, false, "something_new"},
		{200, `{"cleared":["five_hour"]}`, ResetResultUnavailable, "", false, true, false, "(none)"},
		{401, `{"error":"x"}`, ResetResultAuthError, "", false, false, false, ""},
		{403, `{"error":"x"}`, ResetResultAuthError, "", false, false, false, ""},
		{429, ``, ResetResultRateLimited, "", false, false, false, ""},
		{400, `{"type":"error","error":{"type":"invalid_request_error","message":"grant_id: unknown field"}}`, ResetResultError, "", false, true, true, ""},
		{500, ``, ResetResultError, "", false, true, true, ""},
		{200, `not json`, ResetResultError, "", false, true, true, ""},
	} {
		client, seen := recordingResetClient(t, func(*http.Request) (int, string) { return tc.status, tc.body })
		claim, err := ClaimReset(context.Background(), client, "access-token", "0b1c2d3e-4f50", "grant_a", "req_1")
		if (err != nil) != tc.err {
			t.Errorf("%d %s: err = %v, want error %v", tc.status, tc.body, err, tc.err)
		}
		if claim.Result != tc.want || claim.Reason != tc.reason || claim.Settled() != tc.settled || claim.MayHaveLanded() != tc.landed || claim.AnsweredResult != tc.answered {
			t.Errorf("%d %s: claim = %+v (settled %v, may have landed %v), want %s/%s settled %v landed %v answered %q",
				tc.status, tc.body, claim, claim.Settled(), claim.MayHaveLanded(), tc.want, tc.reason, tc.settled, tc.landed, tc.answered)
		}
		if n := len(seen()); n != 1 {
			t.Errorf("%d %s: %d requests, want exactly 1 (no retry, no refresh)", tc.status, tc.body, n)
		}
	}
}

// A 4xx says why, bounded: the operator sees Anthropic's error type and
// message rather than a bare status, and a 4xx is worded as a rejection.
func TestClaimResetReportsAnthropicsErrorBounded(t *testing.T) {
	client, _ := recordingResetClient(t, func(*http.Request) (int, string) {
		return 400, `{"type":"error","error":{"type":"invalid_request_error","message":"grant_id: unknown field"}}`
	})
	_, err := ClaimReset(context.Background(), client, "access-token", "0b1c2d3e-4f50", "grant_a", "req_1")
	if err == nil || !strings.Contains(err.Error(), "rejected the claim request") || !strings.Contains(err.Error(), "invalid_request_error: grant_id: unknown field") {
		t.Fatalf("400 claim error = %v", err)
	}
	long := strings.Repeat("x", 5000)
	client, _ = recordingResetClient(t, func(*http.Request) (int, string) {
		return 422, `{"error":{"type":"invalid_request_error","message":"` + long + `"}}`
	})
	if _, err := ClaimReset(context.Background(), client, "access-token", "0b1c2d3e-4f50", "grant_a", "req_1"); err == nil || len(err.Error()) > 400 {
		t.Fatalf("a long error body was not bounded (%d bytes)", len(fmt.Sprint(err)))
	}
	client, _ = recordingResetClient(t, func(*http.Request) (int, string) {
		return 403, `{"type":"error","error":{"type":"permission_error","message":"OAuth token does not meet scope requirement user:profile"}}`
	})
	_, _, err = FetchResetStatus(context.Background(), client, "access-token")
	if err == nil || !strings.Contains(err.Error(), "scope requirement user:profile") {
		t.Fatalf("403 status read error = %v", err)
	}
}

// No reset call quietly builds its own HTTP client: one that is handed none
// refuses, so a test that forgot its fake can never reach Anthropic.
func TestResetCallsRefuseWithoutAClient(t *testing.T) {
	if claim, err := ClaimReset(context.Background(), nil, "access-token", "0b1c2d3e-4f50", "grant_a", "req_1"); err == nil || claim.Result != ResetResultError {
		t.Fatalf("ClaimReset(nil client) = %+v %v", claim, err)
	}
	if _, _, err := FetchResetStatus(context.Background(), nil, "access-token"); err == nil {
		t.Fatal("FetchResetStatus(nil client) did not refuse")
	}
	if _, err := FetchResetProfile(context.Background(), nil, "access-token"); err == nil {
		t.Fatal("FetchResetProfile(nil client) did not refuse")
	}
}

func TestClaimResetRefusesMalformedIdsWithoutSending(t *testing.T) {
	client, seen := recordingResetClient(t, func(*http.Request) (int, string) { return 200, `{"result":"reset"}` })
	for _, tc := range []struct{ org, grant, request string }{
		{"", "grant", "req"},
		{"org/../x", "grant", "req"},
		{"0b1c2d3e-4f50", "Bad Grant", "req"},
		{"0b1c2d3e-4f50", "grant", "has space"},
		{"0b1c2d3e-4f50", "grant", ""},
	} {
		if claim, err := ClaimReset(context.Background(), client, "access-token", tc.org, tc.grant, tc.request); err == nil || claim.Result != ResetResultError {
			t.Errorf("%+v: claim = %+v, err = %v; want a refusal", tc, claim, err)
		}
	}
	if n := len(seen()); n != 0 {
		t.Fatalf("a malformed claim sent %d requests", n)
	}
}

func TestFetchResetStatusAndOrganizationAreReadOnly(t *testing.T) {
	client, seen := recordingResetClient(t, func(req *http.Request) (int, string) {
		switch req.URL.Path {
		case "/api/oauth/usage":
			return 200, `{"seven_day":{"utilization":40,"resets_at":"2026-10-01T09:00:00Z"},"cedar_ember":` + liveResetBlock + `}`
		case "/api/oauth/profile":
			return 200, `{"account":{"uuid":"a"},"organization":{"uuid":"0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"}}`
		}
		return 404, ""
	})
	usage, status, err := FetchResetStatus(context.Background(), client, "access-token")
	if err != nil || status == nil || usage.SevenDay == nil {
		t.Fatalf("FetchResetStatus = %+v %+v %v", usage, status, err)
	}
	org, err := FetchOrganizationUUID(context.Background(), client, "access-token")
	if err != nil || org != "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0" {
		t.Fatalf("FetchOrganizationUUID = %q %v", org, err)
	}
	requests := seen()
	if len(requests) != 2 {
		t.Fatalf("requests = %d", len(requests))
	}
	for _, req := range requests {
		if req.method != http.MethodGet || len(bytes.TrimSpace(req.body)) != 0 {
			t.Fatalf("%s %s is not a read", req.method, req.path)
		}
	}
	if requests[0].query != "cedar_ember=1&skip_spend=1" {
		t.Fatalf("status read query = %q, want Claude Code's cedar_ember=1&skip_spend=1", requests[0].query)
	}

	unauthorized, _ := recordingResetClient(t, func(*http.Request) (int, string) { return 401, "" })
	_, _, err = FetchResetStatus(context.Background(), unauthorized, "access-token")
	var httpErr *ResetHTTPError
	if !asResetHTTPError(err, &httpErr) || !httpErr.Unauthorized() {
		t.Fatalf("401 status read = %v, want an unauthorized ResetHTTPError", err)
	}
}

func asResetHTTPError(err error, target **ResetHTTPError) bool {
	for err != nil {
		if typed, ok := err.(*ResetHTTPError); ok {
			*target = typed
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

func TestNewResetRequestIDMatchesClaudeCodesGuard(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id, err := NewResetRequestID()
		if err != nil || !ResetRequestIDPattern.MatchString(id) || seen[id] {
			t.Fatalf("NewResetRequestID = %q %v (repeat %v)", id, err, seen[id])
		}
		seen[id] = true
	}
}

// Anthropic's ineligible_reason "unavailable" is its own failed read, which
// Claude Code shows as "couldn't check", never as ineligible: the row is not
// known ("?"), whatever grants ride along.
func TestResetInfoReadsUnavailableAsNotKnown(t *testing.T) {
	status, err := ParseResetStatus([]byte(`{"eligible":false,"ineligible_reason":"unavailable","grants":[{"id":"g","resets_total":1,"resets_left":1}]}`))
	if err != nil || status == nil {
		t.Fatalf("parse: %+v %v", status, err)
	}
	info := ResetInfo(status, time.Now())
	if info == nil || info.Known || info.Available || info.Status != "unavailable" || info.Remaining != nil {
		t.Fatalf("unavailable info = %+v; want known false, status unavailable", info)
	}
	tier, _ := ParseResetStatus([]byte(`{"eligible":false,"ineligible_reason":"tier","grants":[]}`))
	if info := ResetInfo(tier, time.Now()); info == nil || !info.Known || info.IneligibleReason != "tier" {
		t.Fatalf("a real ineligible reason = %+v; want known, verbatim", info)
	}
}

// The version the daemon presents can be moved without a rebuild, and only
// to a plain x.y.z.
func TestClaudeCodeUserAgentTakesAVersionOverride(t *testing.T) {
	for in, want := range map[string]string{
		"":                     "claude-cli/2.1.284 (external, cli)",
		"2.1.301":              "claude-cli/2.1.301 (external, cli)",
		" 2.2.0 ":              "claude-cli/2.2.0 (external, cli)",
		"2.1.301 (evil)":       "claude-cli/2.1.284 (external, cli)",
		"2.1":                  "claude-cli/2.1.284 (external, cli)",
		"2.1.301\r\nX-Evil: 1": "claude-cli/2.1.284 (external, cli)",
	} {
		if got := claudeCodeUserAgent(in); got != want {
			t.Errorf("claudeCodeUserAgent(%q) = %q, want %q", in, got, want)
		}
	}
}

// percent_used is read as Claude Code reads it (known limits, whole 0-100),
// and a grant clears an exhausted limit only when their lists meet.
func TestGrantPercentUsedAndClearsExhausted(t *testing.T) {
	status, err := ParseResetStatus([]byte(`{"eligible":true,"exhausted":["seven_day_opus"],"grants":[{"id":"g","resets_left":1,
		"clears":["five_hour","seven_day"],"percent_used":{"seven_day":60,"five_hour":101,"made_up":5,"seven_day_opus":12.5}}]}`))
	if err != nil || status == nil || len(status.Grants) != 1 {
		t.Fatalf("parse: %+v %v", status, err)
	}
	grant := status.Grants[0]
	if len(grant.PercentUsed) != 1 || grant.PercentUsed["seven_day"] != 60 {
		t.Fatalf("percent_used = %v; want only seven_day:60", grant.PercentUsed)
	}
	if grant.ClearsExhausted(status) {
		t.Fatal("a grant clearing five_hour/seven_day read as clearing an exhausted seven_day_opus")
	}
	status.Exhausted = []string{"five_hour"}
	if !grant.ClearsExhausted(status) {
		t.Fatal("a grant clearing the exhausted five_hour read as early use")
	}
}

// A grant's type, gives_back_weekly and unavailable_reason are read as Claude
// Code 2.1.321 reads them: kept when they are values it knows, absent
// (and the grant still kept) when they are missing or drifted.
func TestParseResetGrantKeepsTypeGivesBackWeeklyAndUnavailableReason(t *testing.T) {
	status, err := ParseResetStatus([]byte(`{"eligible":true,"grants":[
		{"id":"fable","resets_left":1,"clears":["seven_day_overage_included"],"type":"weekly","gives_back_weekly":true,"unavailable_reason":"needs_limit"},
		{"id":"plain","resets_left":1},
		{"id":"drifted","resets_left":1,"type":"monthly","gives_back_weekly":"yes","unavailable_reason":"some_new_reason"}
	]}`))
	if err != nil || status == nil || len(status.Grants) != 3 {
		t.Fatalf("parse: %+v %v; want all three grants kept", status, err)
	}
	for i, want := range []ResetGrant{
		{Type: "weekly", GivesBackWeekly: true, UnavailableReason: "needs_limit"},
		{},
		{},
	} {
		got := status.Grants[i]
		if got.Type != want.Type || got.GivesBackWeekly != want.GivesBackWeekly || got.UnavailableReason != want.UnavailableReason {
			t.Fatalf("grant %s: type %q gives_back_weekly %v unavailable_reason %q; want %q %v %q",
				got.ID, got.Type, got.GivesBackWeekly, got.UnavailableReason, want.Type, want.GivesBackWeekly, want.UnavailableReason)
		}
	}
}

// The profile read names the organization a claim is addressed to and the
// account it belongs to; it is a GET and nothing else.
func TestFetchResetProfileNamesTheOrganizationAndAccount(t *testing.T) {
	client, seen := recordingResetClient(t, func(*http.Request) (int, string) {
		return 200, `{"account":{"uuid":"acct-1","email_address":"held@example.com"},"organization":{"uuid":"0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"}}`
	})
	profile, err := FetchResetProfile(context.Background(), client, "access-token")
	if err != nil || profile.OrganizationUUID != "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0" || profile.Email != "held@example.com" || profile.AccountUUID != "acct-1" {
		t.Fatalf("profile = %+v %v", profile, err)
	}
	if requests := seen(); len(requests) != 1 || requests[0].method != http.MethodGet || requests[0].path != "/api/oauth/profile" {
		t.Fatalf("requests = %+v", requests)
	}
}

// A 429 carries Anthropic's Retry-After back to the caller (PATCHES §46.7): in
// seconds or as an HTTP date, on the status read, the profile read and the
// claim alike -- and none of them retries on its own: one call, one request.
func TestResetRateLimitCarriesRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 29, 20, 0, 0, 0, time.UTC)
	for value, want := range map[string]time.Duration{
		"": 0, "7": 7 * time.Second, "0": 0, "-3": 0, "soon": 0, "999999": time.Hour,
		now.Add(90 * time.Second).Format(http.TimeFormat): 90 * time.Second,
		now.Add(-time.Minute).Format(http.TimeFormat):     0,
	} {
		if got := ParseRetryAfter(value, now); got != want {
			t.Errorf("ParseRetryAfter(%q) = %v, want %v", value, got, want)
		}
	}
	var mu sync.Mutex
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return &http.Response{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests", Header: http.Header{"Retry-After": {"42"}},
			Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"Rate limited. Please try again later"}}`))}, nil
	})}
	ctx := context.Background()
	_, _, err := FetchResetStatus(ctx, client, "access-token")
	var httpErr *ResetHTTPError
	if !asResetHTTPError(err, &httpErr) || !httpErr.RateLimited() || httpErr.RetryAfter != 42*time.Second || !strings.Contains(err.Error(), "rate_limit_error") {
		t.Fatalf("429 status read = %v (%+v)", err, httpErr)
	}
	httpErr = nil
	_, err = FetchResetProfile(ctx, client, "access-token")
	if !asResetHTTPError(err, &httpErr) || !httpErr.RateLimited() || httpErr.RetryAfter != 42*time.Second {
		t.Fatalf("429 profile read = %v (%+v)", err, httpErr)
	}
	claim, err := ClaimReset(ctx, client, "access-token", "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0", "opus_5_5_launch", "4b7e0a4e-2f0c-4a57-9d8e-2a1f0c3b4d5e")
	if err != nil || claim.Result != ResetResultRateLimited || claim.HTTPStatus != http.StatusTooManyRequests || claim.RetryAfter != 42*time.Second {
		t.Fatalf("429 claim = %+v %v", claim, err)
	}
	if calls != 3 {
		t.Fatalf("three calls sent %d requests; want one each", calls)
	}
}
