package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/discordwell/subrouter/account"
	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
)

// codexSpendControlUsageFixture is a sanitized copy of a real
// GET /backend-api/wham/usage response for a usage-based business seat
// (plan_type self_serve_business_usage_based). Identity fields were replaced;
// the quota shape is verbatim: rate_limit is null and the only budget is
// spend_control.individual_limit with string-encoded amounts.
const codexSpendControlUsageFixture = `{
  "user_id": "user-redacted",
  "account_id": "acct-redacted",
  "email": "seat@example.com",
  "plan_type": "self_serve_business_usage_based",
  "rate_limit": null,
  "code_review_rate_limit": null,
  "additional_rate_limits": null,
  "model_usage": {"gpt-6-astra": {"available": true, "available_at": null, "credits_would_enable": false}},
  "credits": {"has_credits": true, "unlimited": false, "overage_limit_reached": false, "balance": null},
  "spend_control": {
    "reached": false,
    "individual_limit": {
      "source": "workspace_spend_controls",
      "limit": "25000",
      "used": "24745.309142947197",
      "remaining": "254.69085705280304",
      "used_percent": 99,
      "remaining_percent": 1,
      "reset_after_seconds": 2190075,
      "reset_at": 1790812800
    }
  },
  "rate_limit_reached_type": null,
  "promo": null,
  "rate_limit_reset_credits": {"available_count": 0, "applicable_available_count": 0}
}`

type spendControlRoundTripper string

func (body spendControlRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(body))),
	}, nil
}

// Regression: seat-z@example.com (usage-based plan) sat at 99% of its workspace
// spend cap yet scored Headroom 1.0, because the usage parser only read
// rate_limit (null on that plan) and produced zero windows. The scheduler then
// preferred it over every seat with real usage for new sessions. The fetched
// windows for that response must score as nearly exhausted and unusable for
// new sessions, and lose a Pick against a seat with genuine headroom.
func TestCodexSpendControlSeatScoresNearlyExhausted(t *testing.T) {
	client := &http.Client{Transport: spendControlRoundTripper(codexSpendControlUsageFixture)}
	seat := accounts.Account{ID: "x", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "test-token"}
	details, err := accounts.FetchCodexUsageDetails(context.Background(), client, seat)
	if err != nil {
		t.Fatal(err)
	}
	if len(details.Windows) == 0 {
		t.Fatalf("usage-based seat produced no windows; spend_control was ignored")
	}

	score := scoreFromUsageWindows(accounts.ProviderCodex, "x", details.Windows)
	if score.Headroom > 0.02 {
		t.Fatalf("Headroom = %v, want <= 0.02 for a seat at 99%% spend (windows %+v)", score.Headroom, details.Windows)
	}
	if score.Headroom >= selectacct.MinShortHeadroomFloor {
		t.Fatalf("Headroom = %v, must be below MinShortHeadroomFloor %v", score.Headroom, selectacct.MinShortHeadroomFloor)
	}
	if scoreUsableForNewSession(score) {
		t.Fatalf("score %+v must not be usable for a new session", score)
	}
	if len(score.ModelScores) != 0 {
		t.Fatalf("spend window must be account-wide, got model pools %v (would starve other seats via ForModel)", score.ModelScores)
	}

	healthy := scoreFromUsageWindows(accounts.ProviderCodex, "healthy", []accounts.UsageWindow{
		{Name: "primary", UsedPercent: 40, LimitWindowSeconds: 18000, ResetAfterSeconds: 3600},
		{Name: "secondary", UsedPercent: 35, LimitWindowSeconds: 604800, ResetAfterSeconds: 600000},
	})
	scheduler := selectacct.NewScheduler([]selectacct.Score{score, healthy})
	if scheduler.UsableForNewSession(accounts.ProviderCodex, "x") {
		t.Fatalf("scheduler reports the 99%%-spend seat usable for new sessions")
	}
	if !scheduler.UsableForNewSession(accounts.ProviderCodex, "healthy") {
		t.Fatalf("control seat should be usable: %+v", healthy)
	}
	picked, err := scheduler.Pick([]account.Account{
		{ID: "x", Provider: account.ProviderCodex, AuthMode: account.AuthModeOAuth},
		{ID: "healthy", Provider: account.ProviderCodex, AuthMode: account.AuthModeOAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "healthy" {
		t.Fatalf("Pick chose %q, want the seat with real headroom", picked.ID)
	}
}

// The scoring contract for the spend window on its own: a 30-day window is
// not "short", so it drives Headroom (and, absent any short window,
// ShortHeadroom) straight from used_percent.
func TestScoreFromUsageWindowsCodexSpendWindow(t *testing.T) {
	windows := []accounts.UsageWindow{
		{Name: "spend", UsedPercent: 99, LimitWindowSeconds: 30 * 24 * 3600, ResetAfterSeconds: 2190075},
	}
	score := scoreFromUsageWindows(accounts.ProviderCodex, "x", windows)
	if score.Headroom > 0.02 || score.Headroom < 0 {
		t.Fatalf("Headroom = %v, want ~0.01", score.Headroom)
	}
	if score.ShortHeadroom != score.Headroom {
		t.Fatalf("ShortHeadroom = %v, want to mirror Headroom %v when no short window exists", score.ShortHeadroom, score.Headroom)
	}
	if score.Headroom >= selectacct.MinShortHeadroomFloor || scoreUsableForNewSession(score) {
		t.Fatalf("score %+v must be below MinShortHeadroomFloor %v", score, selectacct.MinShortHeadroomFloor)
	}

	reached := scoreFromUsageWindows(accounts.ProviderCodex, "x", append(windows, accounts.UsageWindow{Name: "spend/reached", UsedPercent: 100}))
	if reached.Headroom != 0 {
		t.Fatalf("spend/reached must zero Headroom, got %v", reached.Headroom)
	}
	if !selectacct.NewScheduler([]selectacct.Score{reached}).Exhausted(accounts.ProviderCodex, "x") {
		t.Fatalf("spend/reached score must register as exhausted: %+v", reached)
	}
}

// codexMixedSubscriptionUsageFixture is a Business/Team-shaped response that
// carries BOTH included-usage windows and workspace spend controls (the shape
// codex-cli's RateLimitSnapshot models: primary/secondary next to
// individual_limit + spend_control_reached). Included usage is 80% free; the
// spend cap, which only governs overage credits consumed after included usage
// is gone, is at 90% and reached.
const codexMixedSubscriptionUsageFixture = `{
  "plan_type": "business",
  "rate_limit": {
    "allowed": true,
    "limit_reached": false,
    "primary_window": {"used_percent": 20, "limit_window_seconds": 18000, "reset_after_seconds": 3600},
    "secondary_window": {"used_percent": 15, "limit_window_seconds": 604800, "reset_after_seconds": 600000}
  },
  "additional_rate_limits": null,
  "credits": {"has_credits": true, "unlimited": false, "overage_limit_reached": true, "balance": null},
  "spend_control": {
    "reached": true,
    "individual_limit": {
      "source": "workspace_spend_controls",
      "limit": "500", "used": "450", "remaining": "50",
      "used_percent": 90, "remaining_percent": 10,
      "reset_after_seconds": 2190075, "reset_at": 1790812800
    }
  }
}`

// A subscription seat with spend controls next to its included windows must be
// scored on the included windows alone: Headroom 0.80 (min of 1-0.20, 1-0.15),
// usable for new sessions, not exhausted. Folding the spend window in would
// give Headroom 0.10 (tier 2, every cold sticky request re-Picks) or, with
// reached=true, Headroom 0 (Exhausted, tier 3) on a seat whose included quota
// is 80% free -- silently defeating the point of adding such seats.
func TestCodexSpendControlDoesNotDemoteSubscriptionSeat(t *testing.T) {
	client := &http.Client{Transport: spendControlRoundTripper(codexMixedSubscriptionUsageFixture)}
	seat := accounts.Account{ID: "mixed", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "test-token"}
	details, err := accounts.FetchCodexUsageDetails(context.Background(), client, seat)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range details.Windows {
		if w.Name == "spend" || w.Name == "spend/reached" {
			t.Fatalf("spend window emitted for a seat with included-usage windows: %+v", details.Windows)
		}
	}
	if len(details.Windows) != 2 {
		t.Fatalf("Windows = %+v, want exactly primary+secondary", details.Windows)
	}

	score := scoreFromUsageWindows(accounts.ProviderCodex, "mixed", details.Windows)
	if score.Headroom < 0.79 || score.Headroom > 0.81 {
		t.Fatalf("Headroom = %v, want 0.80 from the included windows (windows %+v)", score.Headroom, details.Windows)
	}
	if !scoreUsableForNewSession(score) {
		t.Fatalf("score %+v must be usable for a new session; spend cap is not the binding quota", score)
	}
	scheduler := selectacct.NewScheduler([]selectacct.Score{score})
	if scheduler.Exhausted(accounts.ProviderCodex, "mixed") {
		t.Fatalf("spend_control.reached hard-stopped a seat with 80%% included quota free: %+v", score)
	}
	if !scheduler.UsableForNewSession(accounts.ProviderCodex, "mixed") {
		t.Fatalf("scheduler reports the mixed seat unusable for new sessions: %+v", score)
	}
}

// spendPoolUsage serves GET /backend-api/wham/usage per account, keyed by the
// email in the bearer JWT (emailFromTestJWT), so one fake upstream measures a
// whole pool through the daemon's real usage sweep.
func spendPoolUsage(t *testing.T, bodies map[string]string) proxyRoundTripFunc {
	t.Helper()
	return proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, ok := bodies[emailFromTestJWT(req.Header.Get("Authorization"))]
		if req.URL.Path != "/backend-api/wham/usage" || !ok {
			return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})
}

// The daemon's own plane, end to end: its usage sweep reads the real
// usage-based response, and what it publishes and routes on must say the seat
// is 1% from its cap. On origin/main the seat produced no windows, so the
// sweep scored it Headroom 1.0 and Fresh, usage-status published it usable and
// eligible for a peer's new session (the verdict a deciding daemon reads,
// PATCHES §31), and -- once every rate-limited seat sat below the new-session
// floor -- it was the only "usable" account, so every new session went to it.
// Scored, it ranks with the other near-empty seats on headroom, and the one
// with more left wins.
func TestUsageSweepPublishesASpendCappedSeatAsUnusable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// handleUsageStatus also sweeps the Claude profiles, and the profile store
	// honours SUBROUTER_STATE_DIR before HOME.
	t.Setenv("SUBROUTER_STATE_DIR", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	for _, email := range []string{"spend@example.com", "low@example.com"} {
		if err := store.SaveStored(proxyStoredOAuthAccount(email, "fresh", time.Now().Add(time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	client := &http.Client{Transport: spendPoolUsage(t, map[string]string{
		"spend@example.com": codexSpendControlUsageFixture,
		// A Pro seat with 4% of its week left: below MinShortHeadroomFloor, so
		// not usable either, but further from empty than the spend seat.
		"low@example.com": `{"plan_type": "pro", "rate_limit": {"allowed": true, "limit_reached": false,
			"primary_window": {"used_percent": 96, "limit_window_seconds": 604800, "reset_after_seconds": 200000}}}`,
	})}
	pool := storedPoolAccounts(t, store)
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	schedulerRef.SetUpdatedAt(time.Time{})
	server := Server{
		AccountRef:    NewAccountRef(store, pool, client),
		SchedulerRef:  schedulerRef,
		UsageScoreTTL: time.Nanosecond,
		// The stored tokens are an hour from expiry; keep the sweep off the
		// token endpoint so only the usage read is faked.
		RefreshAccountFn: func(_ context.Context, account accounts.Account) (accounts.Account, error) {
			return account, nil
		},
	}

	// The sweep selection runs before it places a session: the daemon's own
	// scoreAccounts over the fake usage upstream.
	server.refreshUsageScoresIfStale(context.Background())
	scheduler := server.SchedulerRef.Get()
	score, ok := scheduler.LookupScore(accounts.ProviderCodex, "spend@example.com")
	if !ok || !score.Fresh {
		t.Fatalf("sweep did not measure the spend seat: %+v (known %v)", score, ok)
	}
	if score.Headroom > 0.02 {
		t.Fatalf("sweep scored the 99%%-spent seat Headroom %v, want <= 0.02", score.Headroom)
	}
	if scheduler.UsableForNewSession(accounts.ProviderCodex, "spend@example.com") {
		t.Fatalf("sweep left the 99%%-spent seat usable for new sessions: %+v", score)
	}
	picked, err := pickRoutingAccount(scheduler, pool)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "low@example.com" {
		t.Fatalf("new session placed on %q, want low@example.com (4%% left beats 1%% left)", picked.ID)
	}

	recorder := httptest.NewRecorder()
	server.handleUsageStatus(recorder, httptest.NewRequest(http.MethodGet, "/_subrouter/usage-status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("usage-status = %d %s", recorder.Code, recorder.Body.String())
	}
	var rows []AccountUsageStatus
	if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode usage-status: %v\n%s", err, recorder.Body.String())
	}
	var row *AccountUsageStatus
	for i := range rows {
		if rows[i].ID == "spend@example.com" {
			row = &rows[i]
		}
	}
	if row == nil {
		t.Fatalf("usage-status has no row for the spend seat: %s", recorder.Body.String())
	}
	var spend *accounts.UsageWindow
	for i := range row.Windows {
		if row.Windows[i].Name == accounts.CodexSpendWindowName {
			spend = &row.Windows[i]
		}
	}
	if spend == nil || spend.UsedPercent != 99 || spend.Feature != "" {
		t.Fatalf("usage-status windows = %+v, want an account-wide spend window at 99%%", row.Windows)
	}
	norm := row.Normalized
	if norm == nil || !norm.Fresh {
		t.Fatalf("usage-status normalized = %+v, want a fresh measurement", norm)
	}
	if norm.Usable || norm.Headroom > 0.02 {
		t.Fatalf("usage-status publishes the 99%%-spent seat as usable=%v headroom=%v", norm.Usable, norm.Headroom)
	}
	if norm.NewSession == nil || norm.NewSession.Eligible {
		t.Fatalf("usage-status offers the 99%%-spent seat to a peer: %+v", norm.NewSession)
	}
}

// The daemon's scoring of a fetched usage-based seat must not pace its spend
// cap: the conversion carries the mark (selectacct.LimitWindow.SpendCap) that
// keeps it out of the weekly binding. Paced, a seat half through its cap 12h
// before reset_at was in endgame drain -- tier 0, drained first, the verdict
// peers read said burndown -- and won the pick over a Pro seat with 60% of its
// prepaid week left: billed usage spent while prepaid budget expired.
func TestSpendCappedSeatNeverOutranksASubscriptionWeekAtMonthEnd(t *testing.T) {
	body := strings.NewReplacer(`"used_percent": 99`, `"used_percent": 50`, `"reset_after_seconds": 2190075`, `"reset_after_seconds": 43200`).Replace(codexSpendControlUsageFixture)
	client := &http.Client{Transport: spendControlRoundTripper(body)}
	details, err := accounts.FetchCodexUsageDetails(context.Background(), client, accounts.Account{ID: "billed", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	if len(details.Windows) != 1 || details.Windows[0].Name != accounts.CodexSpendWindowName ||
		details.Windows[0].UsedPercent != 50 || details.Windows[0].ResetAfterSeconds != 43200 {
		t.Fatalf("fixture drift: windows = %+v, want one spend window at 50%% with 12h to reset", details.Windows)
	}

	now := time.Now()
	billed := scoreFromUsageWindows(accounts.ProviderCodex, "billed", details.Windows)
	billed.Fresh = true
	subscription := scoreFromUsageWindows(accounts.ProviderCodex, "subscription", []accounts.UsageWindow{
		{Name: "primary", UsedPercent: 40, LimitWindowSeconds: 604800, ResetAfterSeconds: 5 * 24 * 60 * 60},
	})
	subscription.Fresh = true
	scheduler := selectacct.NewScheduler([]selectacct.Score{billed, subscription})

	if billed.InEndgameWindow(now) {
		t.Fatalf("a spend cap 12h from reset_at is in the endgame window: %+v", billed)
	}
	if verdict := scheduler.NewSessionVerdict(accounts.ProviderCodex, "billed", now); verdict.Burndown || !verdict.Eligible {
		t.Fatalf("published verdict = %+v, want eligible, not burndown", verdict)
	}
	picked, err := scheduler.Pick([]account.Account{
		{ID: "billed", Provider: account.ProviderCodex, AuthMode: account.AuthModeOAuth},
		{ID: "subscription", Provider: account.ProviderCodex, AuthMode: account.AuthModeOAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "subscription" {
		t.Fatalf("Pick chose %q, want the subscription seat: its prepaid week expires, the billed cap does not", picked.ID)
	}
}

// Where the consequence lands for an unreadable cap: the daemon's sweep. A
// usage-based seat whose spend_control arrives without a usable percentage
// must not be scored as a fresh reading of zero windows (Headroom 1.0, the
// phantom §42 removes); the fetch fails, so the seat keeps its seed and reads
// unmeasured, like any other failed fetch.
func TestUsageSweepLeavesAnUnreadableSpendCapUnmeasured(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SUBROUTER_STATE_DIR", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	for _, email := range []string{"unreadable@example.com", "readable@example.com"} {
		if err := store.SaveStored(proxyStoredOAuthAccount(email, "fresh", time.Now().Add(time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	client := &http.Client{Transport: spendPoolUsage(t, map[string]string{
		// No usable percentage: used_percent NaN and no limit to derive one.
		"unreadable@example.com": strings.NewReplacer(`"used_percent": 99`, `"used_percent": "NaN"`, `"limit": "25000"`, `"limit": null`).Replace(codexSpendControlUsageFixture),
		// A readable seat in the same sweep, so the sweep publishes a scheduler
		// and the unreadable seat's entry in it is its seed.
		"readable@example.com": codexSpendControlUsageFixture,
	})}
	pool := storedPoolAccounts(t, store)
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	schedulerRef.SetUpdatedAt(time.Time{})
	server := Server{
		AccountRef:    NewAccountRef(store, pool, client),
		SchedulerRef:  schedulerRef,
		UsageScoreTTL: time.Nanosecond,
		RefreshAccountFn: func(_ context.Context, account accounts.Account) (accounts.Account, error) {
			return account, nil
		},
	}
	server.refreshUsageScoresIfStale(context.Background())
	scheduler := server.SchedulerRef.Get()
	if readable, ok := scheduler.LookupScore(accounts.ProviderCodex, "readable@example.com"); !ok || !readable.Fresh {
		t.Fatalf("calibration: the sweep did not measure the readable seat: %+v (known %v)", readable, ok)
	}
	score, ok := scheduler.LookupScore(accounts.ProviderCodex, "unreadable@example.com")
	if !ok {
		t.Fatal("the sweep did not seed the unreadable seat")
	}
	if score.Fresh {
		t.Fatalf("an unreadable spend cap was scored as a fresh reading: %+v", score)
	}
}
