package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// 2. A credential this daemon checked and found dead cannot serve anything,
// new or continuing, whatever score the scheduler still holds for it. A cut
// sweep's auth_valid:false is not that verdict (the same tells the peer reader
// uses), so it keeps the score's own answer.
//
// Driven through withNewSessionVerdicts rather than the handler: on the live
// sweep a refresh verdict against a Claude profile also drops its score, so
// the row publishes no normalized block at all (checked below) and the clause
// guards the remaining shapes -- a carried score beside a dead credential.
func TestUsageStatusVerdictMarksACheckedDeadCredentialIneligible(t *testing.T) {
	now := time.Now()
	score := selectacct.Score{AccountID: "acct", Provider: accounts.ProviderClaude, Fresh: true,
		Headroom: 0.8, ShortHeadroom: 0.9, WeeklyHeadroom: 0.8, WeeklyRemaining: 0.8,
		WeeklyResetAt: now.Add(100 * time.Hour)}
	server := Server{SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{score}))}
	row := func(mutate func(*AccountUsageStatus)) AccountUsageStatus {
		status := AccountUsageStatus{AccountStatus: AccountStatus{
			ID: "acct", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth,
			AuthChecked: true, AuthValid: true,
		}}
		mutate(&status)
		return status
	}
	for _, tc := range []struct {
		name       string
		mutate     func(*AccountUsageStatus)
		wantReason string
	}{
		{"checked dead", func(s *AccountUsageStatus) {
			s.AuthValid = false
			s.Error = `Claude OAuth refresh failed: {"error":"invalid_grant"}`
		}, targetRefusedAuthInvalid},
		{"cut sweep is not a verdict", func(s *AccountUsageStatus) {
			s.AuthValid, s.SweepCut, s.AuthChecked = false, true, false
		}, selectacct.VerdictEligible},
		{"context error is not a verdict", func(s *AccountUsageStatus) {
			s.AuthValid, s.Error = false, "context deadline exceeded"
		}, selectacct.VerdictEligible},
		{"healthy", func(*AccountUsageStatus) {}, selectacct.VerdictEligible},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := server.withNewSessionVerdicts(server.withNormalizedUsage([]AccountUsageStatus{row(tc.mutate)}))
			verdict := out[0].Normalized.NewSession
			if verdict == nil {
				t.Fatal("no verdict attached to a scored OAuth row")
			}
			if verdict.Reason != tc.wantReason {
				t.Fatalf("verdict %+v, want reason %q", *verdict, tc.wantReason)
			}
			if tc.wantReason == targetRefusedAuthInvalid && (verdict.Eligible || !verdict.Evicted) {
				t.Fatalf("dead credential verdict %+v, want ineligible and evicted (a continuing session cannot stay either)", *verdict)
			}
		})
	}

	// Through the live sweep: the dead profile publishes nothing a peer could
	// read as eligible, and its healthy neighbour does publish a verdict.
	server2, _, _ := claudeUsageBox(t, map[string]string{
		"dead": claudeUsageJSON(now, 10, 150*time.Minute, 20, 100*time.Hour, ""),
		"live": claudeUsageJSON(now, 10, 150*time.Minute, 20, 100*time.Hour, ""),
	}, map[string]bool{"dead": true})
	server2.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{score}))
	rows := readUsageStatus(t, server2)
	if dead := rows["dead"]; dead.Normalized != nil && dead.Normalized.NewSession != nil && dead.Normalized.NewSession.Eligible {
		t.Fatalf("dead profile published an eligible verdict: %+v", *dead.Normalized.NewSession)
	}
	if live := rows["live"]; live.Normalized == nil || live.Normalized.NewSession == nil || !live.Normalized.NewSession.Eligible {
		t.Fatalf("live: %+v, want an eligible verdict (calibration)", live.Normalized)
	}
}

// The verdict is attached to a COPY: usage-status rows may be owned by
// AccountRef's cache, and a verdict written through would be served, stale, on
// the next cache hit.
func TestWithNewSessionVerdictsDoesNotMutateInput(t *testing.T) {
	score := selectacct.Score{AccountID: "acct", Provider: accounts.ProviderClaude, Fresh: true, Headroom: 0.5, ShortHeadroom: 0.5}
	server := Server{Scheduler: selectacct.NewScheduler([]selectacct.Score{score})}
	in := server.withNormalizedUsage([]AccountUsageStatus{{AccountStatus: AccountStatus{
		ID: "acct", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth,
	}}})
	out := server.withNewSessionVerdicts(in)
	if out[0].Normalized.NewSession == nil {
		t.Fatal("no verdict on the returned copy")
	}
	if in[0].Normalized.NewSession != nil {
		t.Fatal("withNewSessionVerdicts wrote through to its input")
	}
	// API-key rows carry no verdict: the laptop never targets them.
	apiKey := server.withNewSessionVerdicts(server.withNormalizedUsage([]AccountUsageStatus{{AccountStatus: AccountStatus{
		ID: "acct", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey,
	}}}))
	if apiKey[0].Normalized != nil && apiKey[0].Normalized.NewSession != nil {
		t.Fatal("an API-key row carries a verdict")
	}
}

// The pass-through stamp on a targeted turn's upstream answer, per provider.
// A Codex usage_limit_reached is marked by the passive capture only once the
// body is read -- after the stamp runs -- so the stamp reads the body itself,
// and must hand every byte of it on.
func TestStampTargetedResponse(t *testing.T) {
	codex := accounts.Account{ID: "cx", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth}
	server := Server{SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "cx", Provider: accounts.ProviderCodex, Fresh: true, Headroom: 0.6, ShortHeadroom: 0.6},
	}))}
	const usageLimit = `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`
	for _, tc := range []struct {
		name, body            string
		status                int
		wantRefused, wantBody string
		wantTransient         bool
	}{
		{name: "usage limit reached", status: http.StatusTooManyRequests, body: usageLimit,
			wantRefused: selectacct.VerdictExhausted, wantBody: usageLimit},
		{name: "headerless burst 429", status: http.StatusTooManyRequests, body: `{"error":{"type":"rate_limit"}}`,
			wantTransient: true, wantBody: `{"error":{"type":"rate_limit"}}`},
		{name: "dead credential", status: http.StatusUnauthorized, body: `{}`,
			wantRefused: targetRefusedAuthInvalid, wantBody: `{}`},
		{name: "served", status: http.StatusOK, body: `{"id":"r"}`, wantBody: `{"id":"r"}`},
		{name: "client error", status: http.StatusBadRequest, body: `{"error":"bad"}`, wantBody: `{"error":"bad"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{StatusCode: tc.status, Header: http.Header{},
				Body: io.NopCloser(strings.NewReader(tc.body))}
			server.stampTargetedResponse(response, codex, "")
			if got := response.Header.Get(remotePoolTargetRefusedHeader); got != tc.wantRefused {
				t.Fatalf("refused = %q, want %q", got, tc.wantRefused)
			}
			if got := response.Header.Get(remotePoolTransientHeader) != ""; got != tc.wantTransient {
				t.Fatalf("transient = %v, want %v", got, tc.wantTransient)
			}
			body, _ := io.ReadAll(response.Body)
			if string(body) != tc.wantBody {
				t.Fatalf("body after the stamp = %q, want it intact: %q", body, tc.wantBody)
			}
		})
	}
}
