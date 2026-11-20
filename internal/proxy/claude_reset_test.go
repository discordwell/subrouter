package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// PATCHES §46: a Claude account's saved usage-limit reset (cedar_ember).

const testResetOrg = "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"

// resetBlock is a cedar_ember block with one grant.
func resetBlock(eligible, usableNow, atLimit, paused bool, left int, ends time.Time) string {
	block := map[string]any{
		"eligible":          eligible,
		"ineligible_reason": nil,
		"at_limit":          atLimit,
		"grants": []any{map[string]any{
			"id": "opus_5_5_launch", "label": "Claude Opus 5.5 launch: one usage-limit reset for Pro and Max",
			"resets_total": 1, "resets_left": left,
			"ends_at":            ends.UTC().Format(time.RFC3339),
			"clears":             []string{"five_hour", "seven_day", "seven_day_overage_included"},
			"paused":             paused,
			"usable_now":         usableNow,
			"use_requires_limit": true,
		}},
		"next_grant_id": "opus_5_5_launch",
	}
	if atLimit {
		// At the weekly limit, which the grant clears.
		block["exhausted"] = []string{"seven_day"}
	}
	if !eligible {
		block["ineligible_reason"] = "no_grant"
		block["grants"] = []any{}
	}
	raw, _ := json.Marshal(block)
	return string(raw)
}

// anthropicFake is Anthropic as the reset path sees it. It counts every
// request by kind so a test can prove what was, and was not, sent.
type anthropicFake struct {
	t  *testing.T
	mu sync.Mutex
	// block answers every usage read's cedar_ember (sweep and claim path).
	block string
	// statusCode, when set, answers the claim path's status read.
	statusCode int
	// claimStatus/claimBody answer the claim POST.
	claimStatus int
	claimBody   string
	// afterClaim, when set, becomes the block once a claim answered reset.
	afterClaim string
	// blockOnlyWithSkipSpend answers the block only to Claude Code's own
	// read (?cedar_ember=1&skip_spend=1), as Anthropic might.
	blockOnlyWithSkipSpend bool
	// sweepStatus, when set, answers the sweep's usage read (no skip_spend).
	sweepStatus int
	// profileOrg, when set, is the organization the profile read names.
	profileOrg string
	// statusSeq, claimSeq and profileSeq answer the next claim-path status
	// reads, claim POSTs and profile reads with these codes first (a 429
	// carries rateLimitRetryAfter as Retry-After when set); then the fields
	// above answer.
	statusSeq           []int
	claimSeq            []int
	profileSeq          []int
	rateLimitRetryAfter string
	// profileDelay holds each profile read that long; profileAnsweredAt is
	// when the last one was answered.
	profileDelay      time.Duration
	profileAnsweredAt time.Time

	usageReads     []string
	claims         []map[string]any
	claimHeaders   []http.Header
	profileReads   int
	tokenExchanges int
}

func (f *anthropicFake) client() *http.Client {
	return &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		respond := func(status int, body string) (*http.Response, error) {
			header := http.Header{"Content-Type": {"application/json"}}
			if status == http.StatusTooManyRequests && f.rateLimitRetryAfter != "" {
				header.Set("Retry-After", f.rateLimitRetryAfter)
			}
			return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), Header: header,
				Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		}
		next := func(seq *[]int) int {
			if len(*seq) == 0 {
				return 0
			}
			code := (*seq)[0]
			*seq = (*seq)[1:]
			return code
		}
		const rateLimited = `{"type":"error","error":{"type":"rate_limit_error","message":"Rate limited. Please try again later"}}`
		switch {
		case req.URL.Path == "/api/oauth/usage":
			f.usageReads = append(f.usageReads, req.URL.RawQuery)
			skipSpend := req.URL.Query().Get("skip_spend") == "1"
			if skipSpend {
				if code := next(&f.statusSeq); code != 0 && code != http.StatusOK {
					return respond(code, rateLimited)
				}
			}
			if skipSpend && f.statusCode != 0 {
				return respond(f.statusCode, `{"error":"x"}`)
			}
			if !skipSpend && f.sweepStatus != 0 {
				return respond(f.sweepStatus, `{"error":"x"}`)
			}
			if strings.Contains(req.Header.Get("Authorization"), "sk-ant-oat01") {
				return respond(http.StatusForbidden, `{"type":"error","error":{"type":"permission_error","message":"OAuth token does not meet scope requirement user:profile"}}`)
			}
			week := time.Now().Add(3 * 24 * time.Hour).UTC().Format(time.RFC3339)
			body := `{"five_hour":{"utilization":34,"resets_at":"` + time.Now().Add(2*time.Hour).UTC().Format(time.RFC3339) +
				`"},"seven_day":{"utilization":78,"resets_at":"` + week + `"},"seven_day_oauth_apps":{"utilization":10,"resets_at":"` + week + `"}`
			if f.block != "" && (skipSpend || !f.blockOnlyWithSkipSpend) {
				body += `,"cedar_ember":` + f.block
			}
			return respond(http.StatusOK, body+`}`)
		case req.URL.Path == "/v1/messages":
			return respond(http.StatusOK, `{"type":"message"}`)
		case req.URL.Path == "/api/oauth/profile":
			f.profileReads++
			if f.profileDelay > 0 {
				time.Sleep(f.profileDelay)
			}
			f.profileAnsweredAt = time.Now()
			if code := next(&f.profileSeq); code != 0 && code != http.StatusOK {
				return respond(code, rateLimited)
			}
			org := f.profileOrg
			if org == "" {
				org = testResetOrg
			}
			return respond(http.StatusOK, `{"account":{"uuid":"acct","email_address":"held@example.com"},"organization":{"uuid":"`+org+`"}}`)
		case strings.HasPrefix(req.URL.Path, "/api/organizations/") && strings.HasSuffix(req.URL.Path, "/reset_rate_limits"):
			if req.URL.Path != "/api/organizations/"+testResetOrg+"/reset_rate_limits" {
				f.t.Errorf("claim sent to %s, not the previewed organization", req.URL.Path)
			}
			if req.Method != http.MethodPost {
				f.t.Errorf("claim path read with %s", req.Method)
			}
			var body map[string]any
			raw, _ := io.ReadAll(req.Body)
			_ = json.Unmarshal(raw, &body)
			f.claims = append(f.claims, body)
			f.claimHeaders = append(f.claimHeaders, req.Header.Clone())
			if code := next(&f.claimSeq); code != 0 && code != http.StatusOK {
				return respond(code, rateLimited)
			}
			status := f.claimStatus
			if status == 0 {
				status = http.StatusOK
			}
			if status == http.StatusOK && strings.Contains(f.claimBody, `"reset"`) && f.afterClaim != "" {
				f.block = f.afterClaim
			}
			return respond(status, f.claimBody)
		case strings.HasSuffix(req.URL.Path, "/oauth/token"):
			f.tokenExchanges++
			return respond(http.StatusBadRequest, `{"error":"invalid_grant"}`)
		case req.URL.Path == "/backend-api/wham/usage":
			return respond(http.StatusOK, `{"plan_type":"pro","rate_limit":{"secondary_window":{"used_percent":40,"limit_window_seconds":604800,"reset_after_seconds":3600}},"rate_limit_reset_credits":{"available_count":2}}`)
		}
		f.t.Errorf("unexpected request %s %s", req.Method, req.URL)
		return respond(http.StatusNotFound, "")
	})}
}

func (f *anthropicFake) claimCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.claims)
}

// logRecords captures the daemon's log lines in order.
type logRecords struct {
	mu      sync.Mutex
	records []slog.Record
}

func (l *logRecords) Enabled(context.Context, slog.Level) bool { return true }
func (l *logRecords) Handle(_ context.Context, record slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, record.Clone())
	return nil
}
func (l *logRecords) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logRecords) WithGroup(string) slog.Handler      { return l }

func (l *logRecords) messages() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.records))
	for _, record := range l.records {
		out = append(out, record.Message)
	}
	return out
}

func (l *logRecords) level(message string) (slog.Level, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, record := range l.records {
		if record.Message == message {
			return record.Level, true
		}
	}
	return 0, false
}

func (l *logRecords) attr(message, key string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, record := range l.records {
		if record.Message != message {
			continue
		}
		value := ""
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == key {
				value = attr.Value.String()
				return false
			}
			return true
		})
		return value
	}
	return ""
}

// resetSleeps records the claim path's waits instead of sleeping them.
type resetSleeps struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (r *resetSleeps) all() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.waits...)
}

// recordResetSleeps swaps the claim path's sleep for a recorder for the rest
// of the test: no test sits through a real 30 s backoff.
func recordResetSleeps(t *testing.T) *resetSleeps {
	t.Helper()
	sleeps := &resetSleeps{}
	previous := claudeResetSleep
	claudeResetSleep = func(ctx context.Context, d time.Duration) error {
		sleeps.mu.Lock()
		sleeps.waits = append(sleeps.waits, d)
		sleeps.mu.Unlock()
		return ctx.Err()
	}
	t.Cleanup(func() { claudeResetSleep = previous })
	return sleeps
}

// resetDaemon is a daemon holding one Claude OAuth profile ("held"). Its
// claim path's waits are recorded, never slept (resetDaemonSleeps).
func resetDaemon(t *testing.T, fake *anthropicFake) (Server, *logRecords) {
	t.Helper()
	server, logs, _ := resetDaemonSleeps(t, fake)
	return server, logs
}

// resetDaemonSleeps is resetDaemon with its sleep recorder.
func resetDaemonSleeps(t *testing.T, fake *anthropicFake) (Server, *logRecords, *resetSleeps) {
	t.Helper()
	sleeps := recordResetSleeps(t)
	store := agentclaude.Store{Dir: t.TempDir()}
	if err := store.ImportProfileCredential("held", agentclaude.CredentialInfo{
		AccessToken: "held-access", RefreshToken: "held-refresh", SubscriptionType: "max",
		ExpiresAt: time.Now().Add(2 * time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	logs := &logRecords{}
	server := Server{
		AccountRef: &AccountRef{
			store:       accounts.CodexStore{Dir: t.TempDir()},
			claudeStore: store,
			client:      fake.client(),
		},
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		Logger:       slog.New(logs),
	}
	return server, logs, sleeps
}

func postClaudeReset(t *testing.T, handler http.Handler, body string) (int, ClaudeResetResponse) {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, loopbackAdminRequest(http.MethodPost, ClaudeResetPath, strings.NewReader(body)))
	var response ClaudeResetResponse
	if recorder.Code == http.StatusOK || strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("response did not decode (%d): %s", recorder.Code, recorder.Body.String())
		}
	}
	return recorder.Code, response
}

// The usage-status row carries the saved reset for Claude: from the sweep's
// own usage read (cedar_ember=1, no second request), "unreadable" for a setup
// token, and nothing at all when the answer had no block. The Codex row's
// complimentary_reset is byte-for-byte what it was.
func TestUsageStatusPublishesTheClaudeResetAndLeavesCodexAlone(t *testing.T) {
	ends := time.Now().Add(23 * 24 * time.Hour)
	fake := &anthropicFake{t: t, block: resetBlock(true, false, false, false, 1, ends)}
	claudeStore := agentclaude.Store{Dir: t.TempDir()}
	for name, credential := range map[string]agentclaude.CredentialInfo{
		"held": {AccessToken: "held-access", RefreshToken: "held-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()},
		"setup": {AccessToken: "sk-ant-oat01-" + strings.Repeat("a", 40), ExpiresAt: time.Now().Add(300 * 24 * time.Hour).UnixMilli(),
			Scopes: []string{agentclaude.SetupTokenScope}},
	} {
		if err := claudeStore.ImportProfileCredential(name, credential); err != nil {
			t.Fatal(err)
		}
	}
	codexStore := accounts.CodexStore{Dir: t.TempDir()}
	if err := codexStore.SaveStored(proxyStoredOAuthAccount("codex@example.com", "fresh", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	ref := &AccountRef{store: codexStore, claudeStore: claudeStore, client: fake.client()}
	recorder := httptest.NewRecorder()
	Server{AccountRef: ref}.Handler().ServeHTTP(recorder, loopbackAdminRequest(http.MethodGet, "/_subrouter/usage-status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("usage-status = %d %s", recorder.Code, recorder.Body.String())
	}
	var rows []struct {
		ID                 string          `json:"id"`
		ComplimentaryReset json.RawMessage `json:"complimentary_reset"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	byID := map[string]json.RawMessage{}
	for _, row := range rows {
		byID[row.ID] = row.ComplimentaryReset
	}
	var held accounts.ComplimentaryResetInfo
	if err := json.Unmarshal(byID["held"], &held); err != nil {
		t.Fatalf("held row carries no complimentary_reset: %s", recorder.Body.String())
	}
	if !held.Known || !held.Available || held.Remaining == nil || *held.Remaining != 1 || held.Source != "cedar_ember" ||
		held.Eligible == nil || !*held.Eligible || held.ExpiresAt == "" || len(held.Clears) != 3 {
		t.Fatalf("held complimentary_reset = %s", byID["held"])
	}
	var setup accounts.ComplimentaryResetInfo
	if err := json.Unmarshal(byID["setup"], &setup); err != nil || setup.Known || setup.Status != "unreadable" || setup.IneligibleReason != "no_profile_scope" {
		t.Fatalf("setup-token complimentary_reset = %s, want known:false status:unreadable", byID["setup"])
	}
	if got, want := string(bytes.TrimSpace(byID["codex@example.com"])), `{
      "known": true,
      "available": true,
      "remaining": 2,
      "source": "rate_limit_reset_credits"
    }`; got != want {
		t.Fatalf("Codex complimentary_reset changed:\n%s\nwant\n%s", got, want)
	}
	for _, query := range fake.usageReads {
		if query != "cedar_ember=1" {
			t.Fatalf("sweep usage read query = %q, want cedar_ember=1 (and no skip_spend)", query)
		}
	}

	// A usage answer with no block is not read: no row claims "none".
	fake.block = ""
	ref.forgetUsageWindows(accounts.Account{ID: "held", Provider: accounts.ProviderClaude})
	ref.InvalidateUsageStatusCache()
	for _, status := range ref.UsageStatuses(context.Background()) {
		if status.ID == "held" && status.ComplimentaryReset != nil {
			t.Fatalf("an answer without cedar_ember published %+v", status.ComplimentaryReset)
		}
	}
}

// A dry run -- asked for, or implied by a body that never says dry_run:false
// -- reads the live state and names the grant, and never sends a claim.
func TestClaudeResetDryRunNeverPosts(t *testing.T) {
	fake := &anthropicFake{t: t, block: resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour)), claimBody: `{"result":"reset"}`}
	server, _ := resetDaemon(t, fake)
	handler := server.Handler()
	for _, body := range []string{`{"account":"held","dry_run":true}`, `{"account":"held"}`} {
		code, response := postClaudeReset(t, handler, body)
		if code != http.StatusOK || !response.DryRun || !response.Claimable || response.Grant == nil || response.Grant.ID != "opus_5_5_launch" {
			t.Fatalf("%s: %d %+v", body, code, response)
		}
		if response.Claimed || response.Result != "" || response.RequestID != "" {
			t.Fatalf("%s: a dry run reported a claim: %+v", body, response)
		}
		if len(response.Windows) == 0 || response.Status == nil {
			t.Fatalf("%s: the dry run carried no live state: %+v", body, response)
		}
	}
	if n := fake.claimCount(); n != 0 {
		t.Fatalf("dry runs sent %d claims", n)
	}
	for _, query := range fake.usageReads {
		if query != "cedar_ember=1&skip_spend=1" {
			t.Fatalf("claim-path status read query = %q", query)
		}
	}
}

// A real claim: one POST in Claude Code's shape, its request_id logged before
// it went out, one WARN line after, the exhaustion marks dropped and the
// account read again.
func TestClaudeResetClaimSendsOneRequestLogsItAndClearsTheMarks(t *testing.T) {
	fake := &anthropicFake{
		t:          t,
		block:      resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour)),
		claimBody:  `{"result":"reset","resets_left":0,"cleared":["five_hour","seven_day"]}`,
		afterClaim: resetBlock(true, false, false, false, 0, time.Now().Add(20*24*time.Hour)),
	}
	server, logs := resetDaemon(t, fake)
	server.SchedulerRef.MarkExhausted(accounts.ProviderClaude, "held", "")
	if _, marked := server.SchedulerRef.ExhaustedUntilFor(accounts.ProviderClaude, "held", ""); !marked {
		t.Fatal("calibration: the mark did not take")
	}
	code, response := postClaudeReset(t, server.Handler(), `{"account":"held","dry_run":false,"confirm_account":"held","expected_organization":"0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0","grant_id":"opus_5_5_launch"}`)
	if code != http.StatusOK || response.Result != "reset" || !response.Claimed {
		t.Fatalf("claim: %d %+v", code, response)
	}
	if fake.claimCount() != 1 {
		t.Fatalf("claims sent = %d, want 1", fake.claimCount())
	}
	claim := fake.claims[0]
	if claim["program"] != "cedar_ember" || claim["grant_id"] != "opus_5_5_launch" || claim["request_id"] != response.RequestID ||
		!agentclaude.ResetRequestIDPattern.MatchString(response.RequestID) || len(claim) != 3 {
		t.Fatalf("claim body = %v, response request_id %q", claim, response.RequestID)
	}
	if got := fake.claimHeaders[0].Get("User-Agent"); got != agentclaude.ResetClientUserAgent {
		t.Fatalf("claim User-Agent = %q", got)
	}
	if response.ResetsLeftBefore == nil || *response.ResetsLeftBefore != 1 || response.ResetsLeftAfter == nil || *response.ResetsLeftAfter != 0 {
		t.Fatalf("resets left before/after = %v/%v", response.ResetsLeftBefore, response.ResetsLeftAfter)
	}
	messages := logs.messages()
	sending, answered := -1, -1
	for i, message := range messages {
		switch message {
		case "claude reset claim sending":
			sending = i
		case "claude reset claim":
			answered = i
		}
	}
	if sending < 0 || answered < sending {
		t.Fatalf("log order = %q; want the request_id logged before the claim and a WARN after", messages)
	}
	if logs.attr("claude reset claim sending", "request_id") != response.RequestID || logs.attr("claude reset claim", "result") != "reset" {
		t.Fatalf("log lines do not carry the request_id and result: %q", messages)
	}
	for _, message := range messages {
		if strings.Contains(message, "held-access") {
			t.Fatal("a log line carried the token")
		}
	}
	if _, marked := server.SchedulerRef.ExhaustedUntilFor(accounts.ProviderClaude, "held", ""); marked {
		t.Fatal("the exhaustion mark outlived the reset")
	}
	if len(response.WindowsAfter) == 0 {
		t.Fatal("the account was not read again after the reset")
	}
	if info := server.AccountRef.cachedUsageReset(accounts.Account{ID: "held", Provider: accounts.ProviderClaude}); info == nil || info.Remaining == nil || *info.Remaining != 0 {
		t.Fatalf("the re-read did not replace the cached reset: %+v", info)
	}
}

// claimJSON is a claim body for held that passes the route's own checks.
func claimJSON(extra string) string {
	return `{"account":"held","dry_run":false,"confirm_account":"held","expected_organization":"` + testResetOrg + `"` + extra + `}`
}

// The daemon claims only what Claude Code would: Anthropic's next_grant_id,
// usable now, not paused, not expired. Every refusal sends nothing. Early use
// (no limit the grant clears is exhausted) is the owner's call since §46.7:
// claimed, and said so after the fact.
func TestClaudeResetClaimsOnlyTheNextGrantAndNeverGuesses(t *testing.T) {
	future := time.Now().Add(20 * 24 * time.Hour)
	ends := future.UTC().Format(time.RFC3339)
	twoGrants := func(next string) string {
		nextJSON := "null"
		if next != "" {
			nextJSON = `"` + next + `"`
		}
		return `{"eligible":true,"at_limit":true,"exhausted":["five_hour"],"next_grant_id":` + nextJSON + `,"grants":[
		{"id":"grant_a","resets_total":1,"resets_left":1,"usable_now":true,"clears":["five_hour"],"ends_at":"` + ends + `"},
		{"id":"grant_b","resets_total":1,"resets_left":1,"usable_now":true,"clears":["five_hour"],"ends_at":"` + ends + `"}]}`
	}
	early := `{"eligible":true,"at_limit":false,"exhausted":[],"next_grant_id":"anytime","grants":[
		{"id":"anytime","resets_total":1,"resets_left":1,"usable_now":true,"use_requires_limit":false,
		 "clears":["five_hour","seven_day"],"percent_used":{"seven_day":60,"five_hour":20},"ends_at":"` + ends + `"}]}`
	opusWall := `{"eligible":true,"at_limit":true,"exhausted":["seven_day_opus"],"next_grant_id":"opus_5_5_launch","grants":[
		{"id":"opus_5_5_launch","resets_total":1,"resets_left":1,"usable_now":true,
		 "clears":["five_hour","seven_day","seven_day_overage_included"],"ends_at":"` + ends + `"}]}`
	unavailable := `{"eligible":false,"ineligible_reason":"unavailable","at_limit":true,"exhausted":["seven_day"],"next_grant_id":"opus_5_5_launch","grants":[
		{"id":"opus_5_5_launch","resets_total":1,"resets_left":1,"usable_now":true,"clears":["seven_day"],"ends_at":"` + ends + `"}]}`
	for _, tc := range []struct {
		name, block, body, want string
	}{
		{"not at a limit", resetBlock(true, false, false, false, 1, future), claimJSON(""), "not usable now"},
		{"already used", resetBlock(true, false, true, false, 0, future), claimJSON(""), "already used"},
		{"paused", resetBlock(true, true, true, true, 1, future), claimJSON(""), "paused"},
		{"expired", resetBlock(true, true, true, false, 1, time.Now().Add(-time.Hour)), claimJSON(""), "expired"},
		{"ineligible", resetBlock(false, false, false, false, 0, future), claimJSON(""), "ineligible_reason: no_grant"},
		{"no next grant", twoGrants(""), claimJSON(""), "next_grant_id is null"},
		{"named grant is not next", twoGrants("grant_a"), claimJSON(`,"grant_id":"grant_b"`), `offers grant "grant_a" next, not "grant_b"`},
		{"unknown grant", resetBlock(true, true, true, false, 1, future), claimJSON(`,"grant_id":"nope"`), `no grant "nope"`},
		{"unavailable is not ineligible", unavailable, claimJSON(""), "could not read this account's saved reset right now"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &anthropicFake{t: t, block: tc.block, claimBody: `{"result":"reset"}`}
			server, _ := resetDaemon(t, fake)
			code, response := postClaudeReset(t, server.Handler(), tc.body)
			if code != http.StatusOK || response.Claimable || !strings.Contains(response.Refusal, tc.want) {
				t.Fatalf("%d %+v; want a refusal containing %q", code, response, tc.want)
			}
			if fake.claimCount() != 0 || response.Claimed {
				t.Fatalf("a refused claim was sent (%d)", fake.claimCount())
			}
		})
	}
	// Two usable grants: the one Anthropic names next is claimed, unnamed
	// or named, exactly as Claude Code would.
	for _, body := range []string{claimJSON(""), claimJSON(`,"grant_id":"grant_b"`)} {
		fake := &anthropicFake{t: t, block: twoGrants("grant_b"), claimBody: `{"result":"reset","resets_left":0}`}
		server, _ := resetDaemon(t, fake)
		if code, response := postClaudeReset(t, server.Handler(), body); code != http.StatusOK || response.Result != "reset" ||
			fake.claimCount() != 1 || fake.claims[0]["grant_id"] != "grant_b" {
			t.Fatalf("%s: %d %+v %v", body, code, response, fake.claims)
		}
	}
	// Early use -- no limit the grant clears exhausted, including a wall on a
	// limit it does not clear -- is claimed without being asked for, and the
	// answer says so (§46.7).
	for name, block := range map[string]string{"early use": early, "binding limit not cleared": opusWall} {
		fake := &anthropicFake{t: t, block: block, claimBody: `{"result":"reset","resets_left":0}`}
		server, _ := resetDaemon(t, fake)
		if _, preview := postClaudeReset(t, server.Handler(), `{"account":"held"}`); !preview.Claimable || !preview.EarlyUse || preview.Refusal != "" {
			t.Fatalf("%s preview: %+v", name, preview)
		}
		code, response := postClaudeReset(t, server.Handler(), `{"account":"held","dry_run":false,"confirm_account":"held"}`)
		if code != http.StatusOK || response.Result != "reset" || !response.EarlyUse || fake.claimCount() != 1 {
			t.Fatalf("%s claim: %d %+v, %d claims", name, code, response, fake.claimCount())
		}
	}
}

// A claim must repeat the account, or nothing is read at all. The
// organization is optional since §46.7 (the one-call CLI sends none), and
// refused when given and wrong. The profile is read on every call.
func TestClaudeResetClaimIsBoundToTheConfirmedAccount(t *testing.T) {
	fake := &anthropicFake{t: t, block: resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour)), claimBody: `{"result":"reset","resets_left":0}`}
	server, _ := resetDaemon(t, fake)
	handler := server.Handler()
	for _, body := range []string{
		`{"account":"held","dry_run":false}`,
		`{"account":"held","dry_run":false,"confirm_account":"other","expected_organization":"` + testResetOrg + `"}`,
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, loopbackAdminRequest(http.MethodPost, ClaudeResetPath, strings.NewReader(body)))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s; want 400", body, recorder.Code, recorder.Body.String())
		}
	}
	if len(fake.usageReads) != 0 || fake.profileReads != 0 {
		t.Fatalf("an unconfirmed claim reached Anthropic: %d reads, %d profile reads", len(fake.usageReads), fake.profileReads)
	}

	// The profile names another organization than the caller expects.
	_, response := postClaudeReset(t, handler, `{"account":"held","dry_run":false,"confirm_account":"held","expected_organization":"9f8e7d6c-5b4a-3928-1706-f5e4d3c2b1a0"}`)
	if response.Claimed || response.Claimable || !strings.Contains(response.Refusal, "login changed") {
		t.Fatalf("claim expecting another organization: %+v", response)
	}
	if fake.claimCount() != 0 || fake.profileReads != 1 {
		t.Fatalf("claims %d, profile reads %d; want 0 and 1", fake.claimCount(), fake.profileReads)
	}

	// No expected_organization: the claim goes out, to the profile's own
	// organization, read again for this call.
	code, response := postClaudeReset(t, handler, `{"account":"held","dry_run":false,"confirm_account":"held"}`)
	if code != http.StatusOK || response.Result != "reset" || response.Organization != testResetOrg || fake.claimCount() != 1 || fake.profileReads != 2 {
		t.Fatalf("claim without expected_organization: %d %+v; claims %d, profile reads %d", code, response, fake.claimCount(), fake.profileReads)
	}
}

// A 401 anywhere on the claim path is reported, never refreshed and retried.
func TestClaudeReset401NeverRefreshesOrRetries(t *testing.T) {
	block := resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour))
	statusFake := &anthropicFake{t: t, block: block, statusCode: http.StatusUnauthorized, claimBody: `{"result":"reset"}`}
	server, _ := resetDaemon(t, statusFake)
	_, response := postClaudeReset(t, server.Handler(), `{"account":"held","dry_run":false,"confirm_account":"held","expected_organization":"0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"}`)
	if response.Result != "auth_error" || response.HTTPStatus != http.StatusUnauthorized || statusFake.claimCount() != 0 {
		t.Fatalf("401 status read: %+v claims=%d", response, statusFake.claimCount())
	}
	if statusFake.tokenExchanges != 0 || len(statusFake.usageReads) != 1 {
		t.Fatalf("401 status read: %d token exchanges, %d reads; want 0 and 1", statusFake.tokenExchanges, len(statusFake.usageReads))
	}

	claimFake := &anthropicFake{t: t, block: block, claimStatus: http.StatusUnauthorized, claimBody: `{"error":"unauthorized"}`}
	server, _ = resetDaemon(t, claimFake)
	_, response = postClaudeReset(t, server.Handler(), `{"account":"held","dry_run":false,"confirm_account":"held","expected_organization":"0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"}`)
	if response.Result != "auth_error" || claimFake.claimCount() != 1 || claimFake.tokenExchanges != 0 {
		t.Fatalf("401 claim: %+v claims=%d exchanges=%d; want auth_error, one claim, no exchange", response, claimFake.claimCount(), claimFake.tokenExchanges)
	}
}

// A claim that got no definite answer keeps its request_id: the retry sends
// the same one, and only a definite answer lets the next claim mint anew.
func TestClaudeResetRetryReusesTheRequestIDOfAnUnansweredClaim(t *testing.T) {
	fake := &anthropicFake{t: t, block: resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour)), claimStatus: http.StatusBadGateway}
	server, _ := resetDaemon(t, fake)
	handler := server.Handler()
	_, first := postClaudeReset(t, handler, claimJSON(""))
	_, second := postClaudeReset(t, handler, claimJSON(""))
	if first.Result != "error" || second.RequestID != first.RequestID || first.RequestID == "" || !first.Unconfirmed {
		t.Fatalf("retry request ids %q then %q (results %s, %s); want the same", first.RequestID, second.RequestID, first.Result, second.Result)
	}
	fake.mu.Lock()
	fake.claimStatus, fake.claimBody = http.StatusOK, `{"result":"not_limited","reason":"not_limited"}`
	fake.mu.Unlock()
	_, settled := postClaudeReset(t, handler, claimJSON(""))
	_, next := postClaudeReset(t, handler, claimJSON(""))
	if settled.RequestID != first.RequestID || settled.Result != "not_limited" || settled.Reason != "not_limited" || next.RequestID == first.RequestID {
		t.Fatalf("after a definite answer: %q -> %q (%s)", settled.RequestID, next.RequestID, settled.Result)
	}
}

// unavailable -- including a result outside the enum -- is Claude Code's
// "couldn't confirm the reset went through": the request_id is kept, what it
// says it cleared is applied, and an auth_error, 429 or cooldown after it
// keeps that id rather than forgetting it. A 429 on a fresh claim records
// nothing.
func TestClaudeResetUnconfirmedAnswersKeepTheRequestID(t *testing.T) {
	for _, body := range []string{
		`{"result":"unavailable","reason":"reset_unconfirmed","cleared":["five_hour","seven_day"]}`,
		`{"result":"ok","cleared":["seven_day"]}`,
	} {
		t.Run(body, func(t *testing.T) {
			fake := &anthropicFake{t: t, block: resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour)), claimBody: body}
			server, _ := resetDaemon(t, fake)
			server.SchedulerRef.MarkExhausted(accounts.ProviderClaude, "held", "")
			handler := server.Handler()
			_, first := postClaudeReset(t, handler, claimJSON(""))
			if first.Result != "unavailable" || !first.Unconfirmed || first.RequestID == "" {
				t.Fatalf("unconfirmed answer: %+v", first)
			}
			if _, marked := server.SchedulerRef.ExhaustedUntilFor(accounts.ProviderClaude, "held", ""); marked || len(first.WindowsAfter) == 0 {
				t.Fatalf("what the answer cleared was not applied (marked %v, re-read %d windows)", marked, len(first.WindowsAfter))
			}
			for _, then := range []struct {
				status int
				body   string
				result string
			}{
				{http.StatusUnauthorized, `{"error":"x"}`, "auth_error"},
				{http.StatusTooManyRequests, ``, "rate_limited"},
				{http.StatusOK, `{"result":"cooldown","cooldown_until":"2026-10-01T00:00:00Z"}`, "cooldown"},
			} {
				fake.mu.Lock()
				fake.claimStatus, fake.claimBody = then.status, then.body
				fake.mu.Unlock()
				_, response := postClaudeReset(t, handler, claimJSON(""))
				if response.Result != then.result || response.RequestID != first.RequestID || !response.Unconfirmed {
					t.Fatalf("%s after an unconfirmed claim: %+v; want request id %q kept", then.result, response, first.RequestID)
				}
			}
		})
	}
	// With nothing unconfirmed before it, a 429 records no id: the next
	// claim mints a new one, and the answer does not say it may have landed.
	fake := &anthropicFake{t: t, block: resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour)), claimStatus: http.StatusTooManyRequests}
	server, _ := resetDaemon(t, fake)
	_, limited := postClaudeReset(t, server.Handler(), claimJSON(""))
	_, again := postClaudeReset(t, server.Handler(), claimJSON(""))
	if limited.Result != "rate_limited" || limited.Unconfirmed || again.RequestID == limited.RequestID {
		t.Fatalf("429 on a fresh claim: %+v then %q", limited, again.RequestID)
	}
}

// The 10 minutes count from when the id was minted: reusing it does not
// slide the window.
func TestClaudeResetRequestIDWindowDoesNotSlide(t *testing.T) {
	ref := &AccountRef{}
	start := time.Now()
	first, reused, err := ref.claudeResetRequestID("held", "grant", "", start)
	if err != nil || reused {
		t.Fatalf("mint: %q %v %v", first, reused, err)
	}
	if again, reused, _ := ref.claudeResetRequestID("held", "grant", "", start.Add(9*time.Minute)); again != first || !reused {
		t.Fatalf("reuse at 9m: %q reused %v", again, reused)
	}
	if later, reused, _ := ref.claudeResetRequestID("held", "grant", "", start.Add(11*time.Minute)); later == first || reused {
		t.Fatalf("at 11m from the mint (2m after the last reuse) the id %q was reused (%v)", later, reused)
	}
}

// The request_id line before the POST is a WARN, so a daemon run at
// SUBROUTER_LOG_LEVEL=warn still records what was sent.
func TestClaudeResetSendingLineSurvivesWarnLevel(t *testing.T) {
	fake := &anthropicFake{t: t, block: resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour)), claimBody: `{"result":"reset","resets_left":0}`}
	server, logs := resetDaemon(t, fake)
	if _, response := postClaudeReset(t, server.Handler(), claimJSON("")); response.Result != "reset" {
		t.Fatalf("claim: %+v", response)
	}
	if level, ok := logs.level("claude reset claim sending"); !ok || level < slog.LevelWarn {
		t.Fatalf("sending line level = %v (logged %v); want WARN", level, ok)
	}
}

func TestClaudeResetRefusesASetupTokenAndAnUnknownAccount(t *testing.T) {
	fake := &anthropicFake{t: t, block: resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour))}
	server, _ := resetDaemon(t, fake)
	if err := server.AccountRef.claudeStore.ImportProfileCredential("setup", agentclaude.CredentialInfo{
		AccessToken: "sk-ant-oat01-" + strings.Repeat("b", 40), ExpiresAt: time.Now().Add(300 * 24 * time.Hour).UnixMilli(),
		Scopes: []string{agentclaude.SetupTokenScope},
	}); err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	if _, response := postClaudeReset(t, handler, `{"account":"setup","dry_run":false,"confirm_account":"setup","expected_organization":"0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"}`); !strings.Contains(response.Refusal, "setup token") || response.Claimable {
		t.Fatalf("setup token: %+v", response)
	}
	if code, response := postClaudeReset(t, handler, `{"account":"nobody"}`); code != http.StatusNotFound || !strings.Contains(response.Error, "no Claude account") {
		t.Fatalf("unknown account: %d %+v", code, response)
	}
	if fake.claimCount() != 0 || len(fake.usageReads) != 0 {
		t.Fatalf("refusals reached Anthropic: %d claims, %d reads", fake.claimCount(), len(fake.usageReads))
	}
}

// The claim route is private and admin-gated exactly like rate-limit-reset:
// absent from the public listener, and refused to a tailnet caller without
// the admin token.
func TestClaudeResetRouteIsPrivateAndAdminGated(t *testing.T) {
	fake := &anthropicFake{t: t, block: resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour)), claimBody: `{"result":"reset"}`}
	server, _ := resetDaemon(t, fake)
	server.AdminToken = "admin-token"
	body := `{"account":"held","dry_run":false,"confirm_account":"held","expected_organization":"0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"}`

	public := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, ClaudeResetPath, strings.NewReader(body))
	req.RemoteAddr = "203.0.113.9:5555"
	req.Header.Set("Authorization", "Bearer admin-token")
	server.PublicHandler().ServeHTTP(public, req)
	if public.Code == http.StatusOK {
		t.Fatalf("the public listener served the claim route: %d %s", public.Code, public.Body.String())
	}

	tailnet := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, ClaudeResetPath, strings.NewReader(body))
	req.RemoteAddr = "100.64.0.9:52011"
	server.Handler().ServeHTTP(tailnet, req)
	if tailnet.Code != http.StatusUnauthorized && tailnet.Code != http.StatusForbidden {
		t.Fatalf("a tailnet caller without the admin token got %d", tailnet.Code)
	}
	if fake.claimCount() != 0 || len(fake.usageReads) != 0 {
		t.Fatalf("an unauthorized caller reached Anthropic")
	}

	admin := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, ClaudeResetPath, strings.NewReader(`{"account":"held"}`))
	req.RemoteAddr = "100.64.0.9:52011"
	req.Header.Set("Authorization", "Bearer admin-token")
	server.Handler().ServeHTTP(admin, req)
	if admin.Code != http.StatusOK {
		t.Fatalf("the admin token over the private listener got %d %s", admin.Code, admin.Body.String())
	}
}

// publicResetDaemon is resetDaemon configured as the box: static admin and
// import tokens, named tokens, and --public-admin as given.
func publicResetDaemon(t *testing.T, fake *anthropicFake, publicAdmin bool) (Server, namedTokenFixture, *logRecords, *resetSleeps) {
	t.Helper()
	server, logs, sleeps := resetDaemonSleeps(t, fake)
	tokens, checker := newNamedTokenFixture(t)
	server.AdminToken = "admin-token"
	server.AccountImportToken = "import-token"
	server.AccessTokens = checker
	server.PublicAdmin = publicAdmin
	return server, tokens, logs, sleeps
}

func postPublicClaudeReset(t *testing.T, handler http.Handler, remote string, headers map[string]string, body string) (int, ClaudeResetResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, ClaudeResetPath, strings.NewReader(body))
	req.RemoteAddr = remote
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var response ClaudeResetResponse
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		_ = json.Unmarshal(rec.Body.Bytes(), &response)
	}
	return rec.Code, response
}

// The claim over the public listener (PATCHES §46.9, the owner's decision of
// 2026-09-30). Flag OFF: not mounted, a 404 to every credential, the named
// admin token included. Flag ON: a named admin-role token claims through
// it; the static admin and import tokens, a peer or membership token,
// loopback (a relay on the box) and no credential at all are refused 401
// before anything is read. Nothing but the named admin token reaches
// Anthropic.
func TestClaudeResetOverPublicAdminMatrix(t *testing.T) {
	block := resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour))
	claim := `{"account":"held","dry_run":false,"confirm_account":"held"}`
	preview := `{"account":"held"}`

	offFake := &anthropicFake{t: t, block: block, claimBody: `{"result":"reset"}`}
	off, offTokens, _, _ := publicResetDaemon(t, offFake, false)
	offPublic := off.PublicHandler()
	for name, headers := range map[string]map[string]string{
		"named admin":  bearer(offTokens.admin),
		"static admin": bearer("admin-token"),
		"none":         nil,
	} {
		for _, body := range []string{claim, preview} {
			if code, _ := postPublicClaudeReset(t, offPublic, publicRemote, headers, body); code != http.StatusNotFound {
				t.Errorf("flag off: %s got %d, want 404 (not mounted)", name, code)
			}
		}
	}
	if offFake.claimCount() != 0 || len(offFake.usageReads) != 0 {
		t.Fatalf("flag off: the public listener reached Anthropic")
	}
	// Flag off, the private listener is as before.
	if code, response := postPublicClaudeReset(t, off.Handler(), tailnetRemote, bearer(offTokens.admin), preview); code != http.StatusOK || !response.Claimable {
		t.Fatalf("flag off: the named admin token privately got %d %+v", code, response)
	}

	fake := &anthropicFake{t: t, block: block, claimBody: `{"result":"reset","resets_left":0}`}
	on, tokens, logs, _ := publicResetDaemon(t, fake, true)
	public := on.PublicHandler()
	for _, caller := range []struct {
		name    string
		remote  string
		headers map[string]string
	}{
		{"static admin", publicRemote, bearer("admin-token")},
		{"static admin header", publicRemote, map[string]string{"X-Subrouter-Admin-Token": "admin-token"}},
		{"import token", publicRemote, map[string]string{"X-Subrouter-Account-Import-Token": "import-token"}},
		{"import bearer", publicRemote, bearer("import-token")},
		{"static proxy", publicRemote, bearer("proxy-token")},
		{"peer", publicRemote, bearer(tokens.peer)},
		{"membership", publicRemote, bearer(tokens.membership)},
		{"loopback relay, no token", boxRelay, nil},
		{"tailnet address, no token", tailnetRemote, nil},
		{"none", publicRemote, nil},
	} {
		for _, body := range []string{claim, preview} {
			if code, _ := postPublicClaudeReset(t, public, caller.remote, caller.headers, body); code != http.StatusUnauthorized {
				t.Errorf("flag on: %s got %d, want 401", caller.name, code)
			}
		}
	}
	if fake.claimCount() != 0 || len(fake.usageReads) != 0 {
		t.Fatalf("flag on: a refused caller reached Anthropic: %d claims, %d reads", fake.claimCount(), len(fake.usageReads))
	}

	code, dry := postPublicClaudeReset(t, public, publicRemote, bearer(tokens.admin), preview)
	if code != http.StatusOK || !dry.DryRun || !dry.Claimable || fake.claimCount() != 0 {
		t.Fatalf("flag on: named admin dry run = %d %+v, %d claims", code, dry, fake.claimCount())
	}
	code, claimed := postPublicClaudeReset(t, public, publicRemote, bearer(tokens.admin), claim)
	if code != http.StatusOK || claimed.Result != "reset" || !claimed.Claimed || fake.claimCount() != 1 {
		t.Fatalf("flag on: named admin claim = %d %+v, %d claims", code, claimed, fake.claimCount())
	}
	// By name, and says it came in publicly.
	named := 0
	for _, caller := range logs.attrs("admin request by access token", "caller") {
		if caller == "laptop" {
			named++
		}
	}
	if named < 2 || logs.attr("admin request by access token", "listener") != "public" {
		t.Fatalf("the public claim was not logged by name: %q", logs.messages())
	}
}

// The public limiter applies to the mounted claim route like any other public
// request: past the burst, enforce answers 429 before the gate.
func TestClaudeResetOverPublicAdminIsRateLimited(t *testing.T) {
	fake := &anthropicFake{t: t, block: resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour))}
	server, tokens, _, _ := publicResetDaemon(t, fake, true)
	server.PublicRateLimit = RateLimitEnforce
	public := server.PublicHandler()
	limited := false
	for i := 0; i < int(publicRateBurst)+50 && !limited; i++ {
		req := httptest.NewRequest(http.MethodGet, ClaudeResetPath, nil)
		req.RemoteAddr = publicRemote
		req.Header.Set("Authorization", "Bearer "+tokens.admin)
		rec := httptest.NewRecorder()
		public.ServeHTTP(rec, req)
		limited = rec.Code == http.StatusTooManyRequests
		if i == 0 && rec.Code != http.StatusMethodNotAllowed {
			// Calibration: mounted, past the gate, the handler's own 405.
			t.Fatalf("the first GET got %d, want the handler's 405", rec.Code)
		}
	}
	if !limited {
		t.Fatal("the claim route on the public listener was never rate limited")
	}
}

// Over the public listener the daemon still waits out Anthropic's 429 on a
// claim and resends the SAME request_id; and a claim whose answer was not
// definite keeps its request_id for the retry, which also comes in publicly.
func TestClaudeResetOverPublicAdminKeepsThe429WaitAndTheRequestID(t *testing.T) {
	block := resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour))
	fake := &anthropicFake{t: t, block: block, claimSeq: []int{http.StatusTooManyRequests}, claimBody: `{"result":"reset","resets_left":0}`}
	server, tokens, _, sleeps := publicResetDaemon(t, fake, true)
	public := server.PublicHandler()
	code, response := postPublicClaudeReset(t, public, publicRemote, bearer(tokens.admin), oneCallClaim)
	if code != http.StatusOK || response.Result != "reset" || fake.claimCount() != 2 || response.ClaimAttempts != 2 {
		t.Fatalf("%d %+v; %d claims; want a 429 then the reset", code, response, fake.claimCount())
	}
	for i, claim := range fake.claims {
		if claim["request_id"] != response.RequestID || response.RequestID == "" {
			t.Fatalf("claim %d request_id %v, want %q", i, claim["request_id"], response.RequestID)
		}
	}
	if waits := sleeps.all(); len(waits) != 1 || waits[0] != 30*time.Second {
		t.Fatalf("waits = %v, want [30s]", waits)
	}

	lost := &anthropicFake{t: t, block: block, claimStatus: http.StatusBadGateway}
	server, tokens, _, _ = publicResetDaemon(t, lost, true)
	public = server.PublicHandler()
	_, first := postPublicClaudeReset(t, public, publicRemote, bearer(tokens.admin), claimJSON(""))
	_, second := postPublicClaudeReset(t, public, publicRemote, bearer(tokens.admin), claimJSON(""))
	if !first.Unconfirmed || first.RequestID == "" || second.RequestID != first.RequestID {
		t.Fatalf("public retry request ids %q then %q (unconfirmed %v); want the same", first.RequestID, second.RequestID, first.Unconfirmed)
	}
}

// usageStatusReset is held's published complimentary_reset from a fresh
// status read.
func usageStatusReset(t *testing.T, ref *AccountRef) *accounts.ComplimentaryResetInfo {
	t.Helper()
	ref.InvalidateUsageStatusCache()
	for _, status := range ref.UsageStatuses(context.Background()) {
		if status.ID == "held" {
			return status.ComplimentaryReset
		}
	}
	t.Fatal("no held row")
	return nil
}

func (f *anthropicFake) readsWithSkipSpend() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, query := range f.usageReads {
		if strings.Contains(query, "skip_spend=1") {
			n++
		}
	}
	return n
}

// If Anthropic answers the sweep's ?cedar_ember=1 (without skip_spend, which
// Claude Code never sends) with no block, the daemon reads the block with
// Claude Code's own query -- in the background, at most once per interval
// per account -- and publishes it. While the sweep's read carries the block,
// or when the fallback is off, nothing extra is sent.
func TestUsageStatusFallsBackToClaudeCodesOwnReadWhenTheSweepHasNoBlock(t *testing.T) {
	block := resetBlock(true, false, false, false, 1, time.Now().Add(20*24*time.Hour))
	fake := &anthropicFake{t: t, block: block, blockOnlyWithSkipSpend: true}
	server, _ := resetDaemon(t, fake)
	ref := server.AccountRef

	// Off (every test's default, and a daemon that never enabled it): no read
	// is started at all, not merely not finished yet.
	if info := usageStatusReset(t, ref); info != nil || fake.readsWithSkipSpend() != 0 {
		t.Fatalf("fallback off: published %+v after %d extra reads", info, fake.readsWithSkipSpend())
	}
	ref.claudeResetMu.Lock()
	started := len(ref.claudeResetStatuses)
	ref.claudeResetMu.Unlock()
	if started != 0 {
		t.Fatalf("fallback off: %d fallback reads were started", started)
	}

	ref.EnableClaudeResetFallback(time.Hour)
	ref.forgetUsageWindows(accounts.Account{ID: "held", Provider: accounts.ProviderClaude})
	_ = usageStatusReset(t, ref) // starts the background read
	deadline := time.Now().Add(5 * time.Second)
	var info *accounts.ComplimentaryResetInfo
	for time.Now().Before(deadline) {
		if info = usageStatusReset(t, ref); info != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if info == nil || !info.Known || info.Remaining == nil || *info.Remaining != 1 || info.Source != agentclaude.ResetProgram {
		t.Fatalf("fallback reset = %+v", info)
	}
	for i := 0; i < 3; i++ {
		ref.forgetUsageWindows(accounts.Account{ID: "held", Provider: accounts.ProviderClaude})
		_ = usageStatusReset(t, ref)
	}
	if n := fake.readsWithSkipSpend(); n != 1 {
		t.Fatalf("fallback reads = %d over several sweeps; want 1 per interval", n)
	}

	// The sweep's own read carries the block: no fallback read at all.
	withBlock := &anthropicFake{t: t, block: block}
	server, _ = resetDaemon(t, withBlock)
	server.AccountRef.EnableClaudeResetFallback(time.Hour)
	if info := usageStatusReset(t, server.AccountRef); info == nil || !info.Known {
		t.Fatalf("sweep block not published: %+v", info)
	}
	time.Sleep(50 * time.Millisecond)
	if n := withBlock.readsWithSkipSpend(); n != 0 {
		t.Fatalf("%d fallback reads while the sweep's read carried the block", n)
	}
}

// Last-good windows served over a failed read publish no reset: the table
// says "?" rather than an old "1 left".
func TestUsageStatusPublishesNoResetFromAServedOverRead(t *testing.T) {
	fake := &anthropicFake{t: t, block: resetBlock(true, false, false, false, 1, time.Now().Add(20*24*time.Hour))}
	server, _ := resetDaemon(t, fake)
	ref := server.AccountRef
	if info := usageStatusReset(t, ref); info == nil || !info.Known {
		t.Fatalf("calibration: fresh read published %+v", info)
	}
	// Age the cached read past its TTL (still last-good), then fail the next.
	key := "held\x00" + string(accounts.ProviderClaude)
	ref.usageWindowsMu.Lock()
	entry := ref.usageWindows[key]
	entry.at = time.Now().Add(-usageWindowsTTL - time.Second)
	ref.usageWindows[key] = entry
	ref.usageWindowsMu.Unlock()
	fake.mu.Lock()
	fake.sweepStatus = http.StatusBadGateway
	fake.mu.Unlock()
	ref.InvalidateUsageStatusCache()
	for _, status := range ref.UsageStatuses(context.Background()) {
		if status.ID != "held" {
			continue
		}
		if status.UsageFresh || len(status.Windows) == 0 {
			t.Fatalf("calibration: want last-good windows served over the failure, got fresh=%v windows=%d err=%q", status.UsageFresh, len(status.Windows), status.Error)
		}
		if status.ComplimentaryReset != nil {
			t.Fatalf("a served-over read published %+v", status.ComplimentaryReset)
		}
	}
}

// PATCHES §46.7: one call claims, and a 429 does not fail it.

// attrs is every value key took on the lines logged as message, in order.
func (l *logRecords) attrs(message, key string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, record := range l.records {
		if record.Message != message {
			continue
		}
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == key {
				out = append(out, attr.Value.String())
				return false
			}
			return true
		})
	}
	return out
}

func sumWaits(waits []time.Duration) time.Duration {
	var total time.Duration
	for _, wait := range waits {
		total += wait
	}
	return total
}

const oneCallClaim = `{"account":"held","dry_run":false,"confirm_account":"held"}`

// Seen live on 2026-09-29: the claim's own status read drew a 429 and the
// claim failed. Now the daemon waits it out -- Anthropic's Retry-After when it
// sends one, else 30 s -- reads again, and claims exactly once.
func TestClaudeResetWaitsOutARateLimitedStatusRead(t *testing.T) {
	for _, tc := range []struct {
		name, retryAfter string
		want             time.Duration
	}{
		{"Retry-After honoured", "7", 7 * time.Second},
		{"no Retry-After: 30 s", "", 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &anthropicFake{t: t, block: resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour)),
				claimBody: `{"result":"reset","resets_left":0}`, statusSeq: []int{http.StatusTooManyRequests}, rateLimitRetryAfter: tc.retryAfter}
			server, _, sleeps := resetDaemonSleeps(t, fake)
			code, response := postClaudeReset(t, server.Handler(), oneCallClaim)
			if code != http.StatusOK || response.Result != "reset" || fake.claimCount() != 1 || response.ClaimAttempts != 1 {
				t.Fatalf("%d %+v; %d claims; want one claim that answered reset", code, response, fake.claimCount())
			}
			if waits := sleeps.all(); len(waits) != 1 || waits[0] != tc.want {
				t.Fatalf("waits = %v, want [%v]", waits, tc.want)
			}
			if n := fake.readsWithSkipSpend(); n != 2 || response.StatusSource != "live" || response.RateLimitWaitSeconds != int64(tc.want/time.Second) {
				t.Fatalf("%d status reads, source %q, waited %ds", n, response.StatusSource, response.RateLimitWaitSeconds)
			}
		})
	}
}

// A fresh cached block (the sweep's own usage read, < 30 min old) offering a
// claimable grant is claimed without a live status read, so a status read
// that would 429 is never made and never waited for. A stale cache is read
// live, and when that read stays rate-limited past the bound the stale block
// stands in.
func TestClaudeResetTakesAFreshCachedBlockWithoutReadingLive(t *testing.T) {
	fake := &anthropicFake{t: t, block: resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour)),
		claimBody: `{"result":"reset","resets_left":0}`, statusCode: http.StatusTooManyRequests}
	server, _, sleeps := resetDaemonSleeps(t, fake)
	if info := usageStatusReset(t, server.AccountRef); info == nil || !info.Known {
		t.Fatalf("calibration: the sweep's read did not cache the block: %+v", info)
	}
	code, response := postClaudeReset(t, server.Handler(), oneCallClaim)
	if code != http.StatusOK || response.Result != "reset" || fake.claimCount() != 1 || response.StatusSource != "cache" {
		t.Fatalf("%d %+v; %d claims", code, response, fake.claimCount())
	}
	if n := fake.readsWithSkipSpend(); n != 0 || len(sleeps.all()) != 0 {
		t.Fatalf("a fresh cache still read live (%d reads) or waited (%v)", n, sleeps.all())
	}
	if fake.claims[0]["grant_id"] != "opus_5_5_launch" {
		t.Fatalf("claimed %v, not the cached next_grant_id", fake.claims[0])
	}

	// Stale: older than 30 minutes. Read live; the live read stays 429, the
	// waits stop at the bound, and the stale block stands in.
	fake2 := &anthropicFake{t: t, block: resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour)),
		claimBody: `{"result":"reset","resets_left":0}`, statusCode: http.StatusTooManyRequests}
	server, _, sleeps = resetDaemonSleeps(t, fake2)
	_ = usageStatusReset(t, server.AccountRef)
	key := "held\x00" + string(accounts.ProviderClaude)
	server.AccountRef.usageWindowsMu.Lock()
	entry := server.AccountRef.usageWindows[key]
	// resetAt is when the block was read; entry.at alone no longer ages it
	// (upstream #544's response evidence moves entry.at on every reply).
	entry.at = time.Now().Add(-31 * time.Minute)
	entry.resetAt = entry.at
	server.AccountRef.usageWindows[key] = entry
	server.AccountRef.usageWindowsMu.Unlock()
	code, response = postClaudeReset(t, server.Handler(), oneCallClaim)
	if code != http.StatusOK || response.Result != "reset" || fake2.claimCount() != 1 || response.StatusSource != "cache" || response.StatusAgeSeconds < 30*60 {
		t.Fatalf("stale cache after a rate-limited read: %d %+v", code, response)
	}
	if n := fake2.readsWithSkipSpend(); n != 4 || sumWaits(sleeps.all()) > claudeResetRateLimitBound {
		t.Fatalf("stale cache: %d live reads, waits %v; want 4 reads inside the bound", n, sleeps.all())
	}
}

// A refusal is never taken from the cache: a cached block that says the
// grant is not usable yet (not at a limit then) is read again live, and the
// live block -- at a limit now -- is claimed.
func TestClaudeResetReadsLiveBeforeRefusingOnACachedBlock(t *testing.T) {
	future := time.Now().Add(20 * 24 * time.Hour)
	fake := &anthropicFake{t: t, block: resetBlock(true, false, false, false, 1, future), claimBody: `{"result":"reset","resets_left":0}`}
	server, _ := resetDaemon(t, fake)
	_ = usageStatusReset(t, server.AccountRef)
	fake.mu.Lock()
	fake.block = resetBlock(true, true, true, false, 1, future)
	fake.mu.Unlock()
	code, response := postClaudeReset(t, server.Handler(), oneCallClaim)
	if code != http.StatusOK || response.Result != "reset" || response.StatusSource != "live" || fake.claimCount() != 1 || fake.readsWithSkipSpend() != 1 {
		t.Fatalf("%d %+v; %d claims, %d live reads", code, response, fake.claimCount(), fake.readsWithSkipSpend())
	}
}

// A claim POST that drew a 429 touched nothing: the daemon waits and resends
// it with the SAME request_id, logging each send before it goes out.
func TestClaudeResetResendsTheSameRequestIDAfterARateLimitedClaim(t *testing.T) {
	fake := &anthropicFake{t: t, block: resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour)),
		claimSeq: []int{http.StatusTooManyRequests, http.StatusTooManyRequests}, claimBody: `{"result":"reset","resets_left":0}`}
	server, logs, sleeps := resetDaemonSleeps(t, fake)
	code, response := postClaudeReset(t, server.Handler(), oneCallClaim)
	if code != http.StatusOK || response.Result != "reset" || fake.claimCount() != 3 || response.ClaimAttempts != 3 {
		t.Fatalf("%d %+v; %d claims; want two 429s then the reset", code, response, fake.claimCount())
	}
	for i, claim := range fake.claims {
		if claim["request_id"] != response.RequestID || response.RequestID == "" {
			t.Fatalf("claim %d request_id %v, want %q on every try", i, claim["request_id"], response.RequestID)
		}
	}
	if waits := sleeps.all(); len(waits) != 2 || waits[0] != 30*time.Second || waits[1] != 60*time.Second {
		t.Fatalf("waits = %v, want [30s 1m0s]", waits)
	}
	sent := logs.attrs("claude reset claim sending", "request_id")
	if len(sent) != 3 || sent[0] != response.RequestID || sent[2] != response.RequestID {
		t.Fatalf("sending lines = %q; want three, each with %q", sent, response.RequestID)
	}
}

// The waits are bounded: a status read that stays 429 with nothing cached
// gives up after 30 s + 60 s + 90 s (the next 90 s would pass 4 minutes) and
// sends no claim; a claim that stays 429 stops the same way, every try with
// the one request_id, and records nothing that may have landed.
func TestClaudeResetRateLimitWaitsAreBounded(t *testing.T) {
	// The call's own budget outlasts the longest wait plus the claim, so a
	// wait never eats the claim's time (and the CLI waits longer still).
	if ClaudeResetBudget <= claudeResetRateLimitBound+claudeResetClaimReserve {
		t.Fatalf("budget %v leaves no room past %v of waits and %v for the claim", ClaudeResetBudget, claudeResetRateLimitBound, claudeResetClaimReserve)
	}
	future := time.Now().Add(20 * 24 * time.Hour)
	fake := &anthropicFake{t: t, block: resetBlock(true, true, true, false, 1, future), claimBody: `{"result":"reset"}`,
		statusSeq: []int{429, 429, 429, 429, 429, 429, 429, 429}}
	server, _, sleeps := resetDaemonSleeps(t, fake)
	_, response := postClaudeReset(t, server.Handler(), oneCallClaim)
	if fake.claimCount() != 0 || !strings.Contains(response.Error, "429") || !strings.Contains(response.Error, "rate_limit_error") {
		t.Fatalf("status read 429 throughout: %+v, %d claims", response, fake.claimCount())
	}
	if waits := sleeps.all(); len(waits) != 3 || sumWaits(waits) != 180*time.Second || response.RateLimitWaitSeconds != 180 {
		t.Fatalf("waits = %v (reported %ds); want 30s, 1m, 1m30s", waits, response.RateLimitWaitSeconds)
	}

	claimFake := &anthropicFake{t: t, block: resetBlock(true, true, true, false, 1, future), claimStatus: http.StatusTooManyRequests}
	server, _, sleeps = resetDaemonSleeps(t, claimFake)
	_, limited := postClaudeReset(t, server.Handler(), oneCallClaim)
	if limited.Result != "rate_limited" || limited.Unconfirmed || claimFake.claimCount() != 4 || sumWaits(sleeps.all()) > claudeResetRateLimitBound {
		t.Fatalf("claim 429 throughout: %+v; %d claims, waits %v", limited, claimFake.claimCount(), sleeps.all())
	}
	for _, claim := range claimFake.claims {
		if claim["request_id"] != limited.RequestID {
			t.Fatalf("a retry minted a new request_id: %v", claimFake.claims)
		}
	}
}

// A definite answer (here already_used) outdates the block the claim went
// by: the next claim reads live rather than go by the cache again.
func TestClaudeResetReadsLiveAfterADefiniteAnswer(t *testing.T) {
	fake := &anthropicFake{t: t, block: resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour)),
		claimBody: `{"result":"already_used","reason":"already_used"}`}
	server, _ := resetDaemon(t, fake)
	for i := 0; i < 2; i++ {
		if _, response := postClaudeReset(t, server.Handler(), oneCallClaim); response.Result != "already_used" || response.StatusSource != "live" {
			t.Fatalf("claim %d: %+v", i, response)
		}
	}
	if n := fake.readsWithSkipSpend(); n != 2 {
		t.Fatalf("%d live reads over two claims; the second went by the outdated cache", n)
	}
}

// A 429 on the profile read is waited out like one on the status read.
func TestClaudeResetWaitsOutARateLimitedProfileRead(t *testing.T) {
	fake := &anthropicFake{t: t, block: resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour)),
		profileSeq: []int{http.StatusTooManyRequests}, rateLimitRetryAfter: "12", claimBody: `{"result":"reset","resets_left":0}`}
	server, _, sleeps := resetDaemonSleeps(t, fake)
	code, response := postClaudeReset(t, server.Handler(), oneCallClaim)
	if code != http.StatusOK || response.Result != "reset" || fake.claimCount() != 1 || fake.profileReads != 2 {
		t.Fatalf("%d %+v; %d claims, %d profile reads", code, response, fake.claimCount(), fake.profileReads)
	}
	if waits := sleeps.all(); len(waits) != 1 || waits[0] != 12*time.Second {
		t.Fatalf("waits = %v, want [12s]", waits)
	}
}

// A dry run never waits: a rate-limited read comes straight back.
func TestClaudeResetDryRunNeverWaits(t *testing.T) {
	fake := &anthropicFake{t: t, block: resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour)),
		statusSeq: []int{http.StatusTooManyRequests}, claimBody: `{"result":"reset"}`}
	server, _, sleeps := resetDaemonSleeps(t, fake)
	_, response := postClaudeReset(t, server.Handler(), `{"account":"held"}`)
	if !response.DryRun || !strings.Contains(response.Error, "429") || fake.claimCount() != 0 || len(sleeps.all()) != 0 {
		t.Fatalf("dry run: %+v; %d claims, waits %v", response, fake.claimCount(), sleeps.all())
	}
}

// A caller that has gone (Ctrl-C) before the claim went out spends nothing.
func TestClaudeResetSendsNothingOnceTheCallerHasGone(t *testing.T) {
	fake := &anthropicFake{t: t, block: resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour)), claimBody: `{"result":"reset","resets_left":0}`}
	server, _ := resetDaemon(t, fake)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, loopbackAdminRequest(http.MethodPost, ClaudeResetPath, strings.NewReader(oneCallClaim)).WithContext(ctx))
	if fake.claimCount() != 0 {
		t.Fatalf("a claim went out for a caller that had gone: %d %s", recorder.Code, recorder.Body.String())
	}
	// Calibration: the same request from a caller still there claims.
	if _, response := postClaudeReset(t, server.Handler(), oneCallClaim); response.Result != "reset" || fake.claimCount() != 1 {
		t.Fatalf("calibration: %+v", response)
	}
}

// Review of §46.7: a block read before the account's last claim POST is never
// gone by again -- not by a claim, not by a dry run, not as the stale
// fallback. Such a claim may have landed, and the block cannot know.
func TestClaudeResetNeverGoesByABlockReadBeforeTheLastClaim(t *testing.T) {
	future := time.Now().Add(20 * 24 * time.Hour)
	used := func(fake *anthropicFake) {
		fake.mu.Lock()
		fake.block = resetBlock(true, false, false, false, 0, future)
		fake.claimStatus, fake.claimBody = http.StatusOK, `{"result":"already_used","reason":"already_used"}`
		fake.mu.Unlock()
	}
	t.Run("an unconfirmed claim, then a dry run and a retry past the id's 10 minutes", func(t *testing.T) {
		fake := &anthropicFake{t: t, block: resetBlock(true, true, true, false, 1, future), claimStatus: http.StatusBadGateway}
		server, _ := resetDaemon(t, fake)
		handler := server.Handler()
		_ = usageStatusReset(t, server.AccountRef)
		_, first := postClaudeReset(t, handler, oneCallClaim)
		if first.Result != "error" || !first.Unconfirmed || first.StatusSource != "cache" || fake.claimCount() != 1 {
			t.Fatalf("calibration: %+v", first)
		}
		used(fake) // it landed after all
		_, preview := postClaudeReset(t, handler, `{"account":"held"}`)
		if preview.StatusSource != "live" || preview.Claimable || !strings.Contains(preview.Refusal, "already used") {
			t.Fatalf("the dry run after an unconfirmed claim went by the block before it: %+v", preview)
		}
		ref := server.AccountRef
		ref.claudeResetMu.Lock()
		for key, pending := range ref.claudeResetPending {
			pending.at = pending.at.Add(-11 * time.Minute)
			ref.claudeResetPending[key] = pending
		}
		ref.claudeResetMu.Unlock()
		_, second := postClaudeReset(t, handler, oneCallClaim)
		if fake.claimCount() != 1 || second.Claimed || !strings.Contains(second.Refusal, "already used") {
			t.Fatalf("a retry past the id's window sent another claim (%d sent): %+v", fake.claimCount(), second)
		}
	})
	t.Run("the stale fallback", func(t *testing.T) {
		fake := &anthropicFake{t: t, block: resetBlock(true, true, true, false, 1, future), claimStatus: http.StatusBadGateway}
		server, _ := resetDaemon(t, fake)
		handler := server.Handler()
		_ = usageStatusReset(t, server.AccountRef)
		if _, first := postClaudeReset(t, handler, oneCallClaim); !first.Unconfirmed || first.StatusSource != "cache" {
			t.Fatalf("calibration: %+v", first)
		}
		fake.mu.Lock()
		fake.statusCode = http.StatusTooManyRequests
		fake.mu.Unlock()
		_, second := postClaudeReset(t, handler, oneCallClaim)
		if fake.claimCount() != 1 || second.Claimed || !strings.Contains(second.Error, "429") {
			t.Fatalf("an unreadable live read fell back to the block before the claim (%d sent): %+v", fake.claimCount(), second)
		}
	})
	t.Run("a read begun before the claim that lands after it", func(t *testing.T) {
		fake := &anthropicFake{t: t, block: resetBlock(true, true, true, false, 1, future),
			claimBody: `{"result":"reset","resets_left":0}`, afterClaim: resetBlock(true, false, false, false, 0, future)}
		server, _ := resetDaemon(t, fake)
		handler := server.Handler()
		ref := server.AccountRef
		_ = usageStatusReset(t, ref)
		key := "held\x00" + string(accounts.ProviderClaude)
		ref.usageWindowsMu.Lock()
		before := ref.usageWindows[key]
		ref.usageWindowsMu.Unlock()
		if before.resetStatus == nil {
			t.Fatal("calibration: the sweep cached no block")
		}
		if _, claimed := postClaudeReset(t, handler, oneCallClaim); claimed.Result != "reset" {
			t.Fatalf("calibration: %+v", claimed)
		}
		// A sweep read sent before the POST comes back after the claim's
		// re-read and puts the block it saw back.
		ref.usageWindowsMu.Lock()
		ref.usageWindows[key] = before
		ref.usageWindowsMu.Unlock()
		_, preview := postClaudeReset(t, handler, `{"account":"held"}`)
		if preview.StatusSource != "live" || preview.Claimable {
			t.Fatalf("a block read before the claim was gone by after it: %+v", preview)
		}
	})
}

// A cached block belongs to the login that read it: after the profile is
// logged in again (another token), the claim reads live.
func TestClaudeResetCachedBlockIsBoundToTheLogin(t *testing.T) {
	fake := &anthropicFake{t: t, block: resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour)),
		claimBody: `{"result":"reset","resets_left":0}`}
	server, _ := resetDaemon(t, fake)
	_ = usageStatusReset(t, server.AccountRef)
	if err := server.AccountRef.claudeStore.ImportProfileCredential("held", agentclaude.CredentialInfo{
		AccessToken: "relogin-access", RefreshToken: "relogin-refresh", SubscriptionType: "max",
		ExpiresAt: time.Now().Add(2 * time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	_, response := postClaudeReset(t, server.Handler(), oneCallClaim)
	if response.Result != "reset" || response.StatusSource != "live" || fake.readsWithSkipSpend() != 1 {
		t.Fatalf("a claim after a new login went by the old login's block: %+v, %d live reads", response, fake.readsWithSkipSpend())
	}
}

// A new request_id is stamped when the claim goes out, after the profile read
// (which may wait out 429s), so its 10 minutes are not already spent.
func TestClaudeResetStampsANewRequestIDWhenTheClaimGoesOut(t *testing.T) {
	fake := &anthropicFake{t: t, block: resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour)),
		claimStatus: http.StatusBadGateway, profileDelay: 5 * time.Millisecond}
	server, _ := resetDaemon(t, fake)
	_, response := postClaudeReset(t, server.Handler(), oneCallClaim)
	ref := server.AccountRef
	ref.claudeResetMu.Lock()
	pending, ok := ref.claudeResetPending["held\x00opus_5_5_launch"]
	ref.claudeResetMu.Unlock()
	fake.mu.Lock()
	answered := fake.profileAnsweredAt
	fake.mu.Unlock()
	if !ok || pending.requestID != response.RequestID || answered.IsZero() || pending.at.Before(answered) {
		t.Fatalf("request id %q stamped %v, profile answered %v", pending.requestID, pending.at, answered)
	}
}

// A claim is sent only with time left to send it and read its answer; the
// wait for another claim on the account ends in time for that.
func TestClaudeResetSendsNothingWithoutTimeLeftForTheClaim(t *testing.T) {
	fake := &anthropicFake{t: t, block: resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour)),
		claimBody: `{"result":"reset","resets_left":0}`}
	server, _ := resetDaemon(t, fake)
	claim := false
	request := ClaudeResetRequest{Account: "held", DryRun: &claim, ConfirmAccount: "held"}
	short, cancel := context.WithTimeout(context.Background(), claudeResetClaimReserve-5*time.Second)
	defer cancel()
	response, _ := server.claudeReset(short, short, request)
	if fake.claimCount() != 0 || response.Claimed || !strings.Contains(response.Refusal, "nothing was sent") || response.RequestID != "" {
		t.Fatalf("a claim went out with %v left: %+v, %d sent", claudeResetClaimReserve-5*time.Second, response, fake.claimCount())
	}

	unlock, err := server.AccountRef.lockClaudeResetClaim(context.Background(), "held")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	held, cancelHeld := context.WithTimeout(context.Background(), claudeResetClaimReserve+300*time.Millisecond)
	defer cancelHeld()
	done := make(chan ClaudeResetResponse, 1)
	go func() {
		response, _ := server.claudeReset(held, held, request)
		done <- response
	}()
	select {
	case response := <-done:
		if fake.claimCount() != 0 || !strings.Contains(response.Error, "another claim") {
			t.Fatalf("while another claim held the account: %+v, %d sent", response, fake.claimCount())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait for the account's claim lock ran into the time the claim needs")
	}
}

// The fallback read is stamped when it was sent, and one sent before a claim
// on the account is dropped rather than published or gone by.
func TestClaudeResetFallbackReadBegunBeforeAClaimIsDropped(t *testing.T) {
	status, err := agentclaude.ParseResetStatus(json.RawMessage(resetBlock(true, true, true, false, 1, time.Now().Add(20*24*time.Hour))))
	if err != nil {
		t.Fatal(err)
	}
	ref := &AccountRef{}
	started := time.Now()
	ref.noteClaudeResetClaimSent("held", started.Add(time.Second))
	ref.finishClaudeResetFallback("held", "held-access", started, nil, status, nil)
	ref.claudeResetMu.Lock()
	entry := ref.claudeResetStatuses["held"]
	ref.claudeResetMu.Unlock()
	if entry.info != nil || entry.status != nil || ref.cachedClaudeResetState("held", "held-access").status != nil {
		t.Fatalf("a fallback read sent before the claim was kept: %+v", entry)
	}
	later := started.Add(2 * time.Second)
	ref.finishClaudeResetFallback("held", "held-access", later, nil, status, nil)
	if got := ref.cachedClaudeResetState("held", "held-access"); got.status == nil || !got.at.Equal(later) {
		t.Fatalf("a fallback read sent after the claim: %+v; want it kept, stamped %v", got, later)
	}
	if got := ref.cachedClaudeResetState("held", "another-login"); got.status != nil {
		t.Fatalf("another login's token went by this block: %+v", got)
	}
}
