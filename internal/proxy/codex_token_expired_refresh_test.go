package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
)

// codexTokenExpiredBody is OpenAI's answer, as the box logged it on
// 2026-11-16, to a usage read after a ChatGPT plan change. The access token
// it rejects had 8 days left on its JWT exp, and one refresh of its chain
// brought the account back.
const codexTokenExpiredBody = `{"error":{"message":"Your authentication token has expired. Please try refreshing it.","type":"invalid_request_error","code":"token_expired","param":null},"status":401,"detail":{"code":"token_expired","message":"Your authentication token has expired. Please try refreshing it."}}`

const codexRenewedProUsageBody = `{"plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false,
	"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},
	"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":300000}}}`

// planChangeOpenAI fakes both OpenAI hosts for one Codex account whose access
// token the usage endpoint rejects with rejectBody: the usage endpoint, and
// the token endpoint that renews the chain.
type planChangeOpenAI struct {
	t       *testing.T
	stored  accounts.StoredCodexAccount
	renewed accounts.CodexTokens

	rejectBody string
	// tokenRefusal, when set, is the token endpoint's 401 body.
	tokenRefusal string
	// beforeRejecting runs with the count of rejections so far, before the
	// rejection is answered.
	beforeRejecting func(req *http.Request, rejections int)

	mu         sync.Mutex
	tokenCalls int
	rejections int
	usageReads int
}

func newPlanChangeOpenAI(t *testing.T, rejectBody string) *planChangeOpenAI {
	t.Helper()
	email := "plan-change@example.com"
	renewedExp := time.Now().Add(10 * 24 * time.Hour)
	return &planChangeOpenAI{
		t:      t,
		stored: proxyStoredOAuthAccount(email, "before-plan-change", time.Now().Add(8*24*time.Hour)),
		renewed: accounts.CodexTokens{
			AccessToken:  proxyTestCodexJWT(email, "renewed-access", renewedExp),
			RefreshToken: "renewed-refresh",
			IDToken:      proxyTestCodexJWT(email, "renewed-id", renewedExp),
		},
		rejectBody: rejectBody,
	}
}

func (o *planChangeOpenAI) RoundTrip(req *http.Request) (*http.Response, error) {
	respond := func(status int, body string) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	}
	switch {
	case req.Method == http.MethodPost && req.URL.Host == "auth.openai.com" && req.URL.Path == "/oauth/token":
		o.mu.Lock()
		o.tokenCalls++
		o.mu.Unlock()
		if o.tokenRefusal != "" {
			return respond(http.StatusUnauthorized, o.tokenRefusal)
		}
		body, err := json.Marshal(map[string]string{
			"access_token":  o.renewed.AccessToken,
			"refresh_token": o.renewed.RefreshToken,
			"id_token":      o.renewed.IDToken,
		})
		if err != nil {
			return nil, err
		}
		return respond(http.StatusOK, string(body))
	case req.Method == http.MethodGet && req.URL.Host == "chatgpt.com" && req.URL.Path == "/backend-api/wham/usage":
		o.mu.Lock()
		o.usageReads++
		o.mu.Unlock()
		switch strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ") {
		case o.stored.Auth.Tokens.AccessToken:
			o.mu.Lock()
			o.rejections++
			rejections := o.rejections
			o.mu.Unlock()
			if o.beforeRejecting != nil {
				o.beforeRejecting(req, rejections)
			}
			return respond(http.StatusUnauthorized, o.rejectBody)
		case o.renewed.AccessToken:
			return respond(http.StatusOK, codexRenewedProUsageBody)
		}
	}
	o.t.Errorf("unexpected OpenAI request %s %s", req.Method, req.URL)
	return respond(http.StatusNotFound, "")
}

func (o *planChangeOpenAI) counts() (tokenCalls, usageReads int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.tokenCalls, o.usageReads
}

// newPlanChangeServer serves the account from a store configured as `serve`
// configures it.
func newPlanChangeServer(t *testing.T, openai *planChangeOpenAI) (Server, []accounts.Account) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SUBROUTER_STATE_DIR", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir(), DisableActiveAuthSync: true, RequireIsolatedOAuth: true}
	if err := store.SaveStored(openai.stored); err != nil {
		t.Fatal(err)
	}
	pool := storedPoolAccounts(t, store)
	return Server{
		AccountRef:   NewAccountRef(store, pool, &http.Client{Transport: openai}),
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
	}, pool
}

func planChangeUsageStatusRow(t *testing.T, server Server, id string) AccountUsageStatus {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.handleUsageStatus(recorder, httptest.NewRequest(http.MethodGet, "/_subrouter/usage-status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("usage-status = %d %s", recorder.Code, recorder.Body.String())
	}
	var rows []AccountUsageStatus
	if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode usage-status: %v\n%s", err, recorder.Body.String())
	}
	for _, row := range rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("usage-status has no row for %s: %s", id, recorder.Body.String())
	return AccountUsageStatus{}
}

func usedPercentByWindow(windows []accounts.UsageWindow) map[string]float64 {
	out := make(map[string]float64, len(windows))
	for _, window := range windows {
		out[window.Name] = window.UsedPercent
	}
	return out
}

// After a plan change OpenAI rejects the access token as expired days before
// its JWT exp. The request path's 401 has already marked that credential
// exhausted, and the usage read fails on it. One usage-status read renews the
// chain, publishes the account's usage, and puts it back in the pool.
func TestUsageStatusRenewsACodexTokenOpenAIRejectedAsExpired(t *testing.T) {
	openai := newPlanChangeOpenAI(t, codexTokenExpiredBody)
	server, pool := newPlanChangeServer(t, openai)
	email := openai.stored.Email
	server.markAccountExhaustedCredentialForAccount(pool[0])
	if !server.SchedulerRef.Get().Exhausted(accounts.ProviderCodex, email) {
		t.Fatal("calibration: the request path's mark did not take the account out of the pool")
	}

	row := planChangeUsageStatusRow(t, server, email)

	if row.Error != "" || !row.AuthValid || !row.Refreshed {
		t.Fatalf("usage-status row: error=%q auth_valid=%v refreshed=%v, want a renewed, valid login with no error",
			row.Error, row.AuthValid, row.Refreshed)
	}
	if row.PlanType != "pro" {
		t.Fatalf("plan = %q, want pro", row.PlanType)
	}
	if used := usedPercentByWindow(row.Windows); len(used) != 2 || used["primary"] != 10 || used["secondary"] != 20 {
		t.Fatalf("windows = %+v, want primary 10%% and secondary 20%%", row.Windows)
	}
	stored, ok, err := server.AccountRef.store.FindStored(email)
	if err != nil || !ok {
		t.Fatalf("stored record: ok=%v err=%v", ok, err)
	}
	if stored.Auth.Tokens.AccessToken != openai.renewed.AccessToken || stored.Auth.Tokens.RefreshToken != openai.renewed.RefreshToken {
		t.Fatal("the stored record does not hold the renewed access and refresh tokens")
	}
	if tokenCalls, _ := openai.counts(); tokenCalls != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", tokenCalls)
	}
	scheduler := server.SchedulerRef.Get()
	if scheduler.Exhausted(accounts.ProviderCodex, email) || !scheduler.UsableForNewSession(accounts.ProviderCodex, email) {
		score, _ := scheduler.LookupScore(accounts.ProviderCodex, email)
		t.Fatalf("the renewed account is still out of the pool: fresh=%v headroom=%v", score.Fresh, score.Headroom)
	}
}

// The scheduler's sweep and the usage-status sweep each read usage with the
// rejected token before either renews it. OpenAI rotates the refresh token on
// use, so the chain must be renewed once, and the scheduler scores the
// account on the renewed token's usage.
func TestConcurrentSweepsRenewARejectedCodexTokenOnce(t *testing.T) {
	openai := newPlanChangeOpenAI(t, codexTokenExpiredBody)
	bothRejected := make(chan struct{})
	openai.beforeRejecting = func(req *http.Request, rejections int) {
		if rejections == 2 {
			close(bothRejected)
		}
		select {
		case <-bothRejected:
		case <-req.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}
	server, pool := newPlanChangeServer(t, openai)
	email := openai.stored.Email

	var scores []selectacct.Score
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		scores, _ = server.scoreAccounts(context.Background(), pool)
	}()
	row := planChangeUsageStatusRow(t, server, email)
	wg.Wait()

	score, ok := selectacct.NewScheduler(scores).LookupScore(accounts.ProviderCodex, email)
	if !ok || !score.Fresh || score.Headroom < 0.79 || score.Headroom > 0.81 {
		t.Fatalf("scheduler sweep score: known=%v fresh=%v headroom=%v, want fresh with headroom 0.80 (min of 1-0.10 and 1-0.20)",
			ok, score.Fresh, score.Headroom)
	}
	if row.Error != "" {
		t.Fatalf("usage-status row error = %q, want none", row.Error)
	}
	if tokenCalls, _ := openai.counts(); tokenCalls != 1 {
		t.Fatalf("token endpoint calls = %d, want 1 across both sweeps", tokenCalls)
	}
}

// Only OpenAI's token_expired renews a chain. Any other 401 from the usage
// endpoint is published as it was, and the token endpoint is never called.
func TestUsageStatusLeavesACodexToken401ThatIsNotTokenExpired(t *testing.T) {
	openai := newPlanChangeOpenAI(t, `{"error":{"message":"Your authentication token has been invalidated. Please try signing in again.","type":"invalid_request_error","code":"token_invalidated","param":null},"status":401}`)
	server, _ := newPlanChangeServer(t, openai)

	row := planChangeUsageStatusRow(t, server, openai.stored.Email)

	if row.Error != "usage fetch failed: 401 Unauthorized" {
		t.Fatalf("usage-status row error = %q, want the usage endpoint's 401", row.Error)
	}
	tokenCalls, usageReads := openai.counts()
	if usageReads == 0 {
		t.Fatal("calibration: the sweep never read usage")
	}
	if tokenCalls != 0 {
		t.Fatalf("token endpoint calls = %d, want 0", tokenCalls)
	}
}

// A rejected token whose chain OpenAI also refuses is a dead login: the next
// sweep reports it invalid without asking OpenAI again.
func TestUsageStatusReportsARejectedCodexTokenWithADeadChainAsInvalid(t *testing.T) {
	openai := newPlanChangeOpenAI(t, codexTokenExpiredBody)
	openai.tokenRefusal = `{"error":{"message":"Your refresh token has already been used to generate a new access token. Please try signing in again.","type":"invalid_request_error","param":null,"code":"refresh_token_reused"}}`
	server, _ := newPlanChangeServer(t, openai)
	email := openai.stored.Email

	first := planChangeUsageStatusRow(t, server, email)
	if !strings.Contains(first.Error, "refresh_token_reused") {
		t.Fatalf("first sweep error = %q, want the token endpoint's refusal", first.Error)
	}
	tokenCalls, usageReads := openai.counts()
	if tokenCalls != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", tokenCalls)
	}

	server.AccountRef.InvalidateUsageStatusCache()
	second := planChangeUsageStatusRow(t, server, email)

	if second.AuthValid || !strings.Contains(second.Error, "refresh_token_reused") {
		t.Fatalf("second sweep: auth_valid=%v error=%q, want an invalid login with the refusal", second.AuthValid, second.Error)
	}
	if nextTokenCalls, nextUsageReads := openai.counts(); nextTokenCalls != tokenCalls || nextUsageReads != usageReads {
		t.Fatalf("second sweep asked OpenAI again: token calls %d -> %d, usage reads %d -> %d",
			tokenCalls, nextTokenCalls, usageReads, nextUsageReads)
	}
}
