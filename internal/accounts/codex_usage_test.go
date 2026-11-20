package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

type codexUsageTestTransport func(*http.Request) (*http.Response, error)

func (f codexUsageTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestFetchCodexUsageRejectsUnknownAccountQuota(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{"missing rate limit", `{"plan_type":"pro"}`},
		{"empty rate limit", `{"plan_type":"pro","rate_limit":{}}`},
		{"null utilization", `{"rate_limit":{"primary_window":{"used_percent":null,"limit_window_seconds":18000}}}`},
		{"omitted utilization", `{"rate_limit":{"primary_window":{"limit_window_seconds":18000}}}`},
		{"negative utilization", `{"rate_limit":{"primary_window":{"used_percent":-1}}}`},
		{"over-100 utilization", `{"rate_limit":{"primary_window":{"used_percent":101}}}`},
		{"unknown weekly alongside known session", `{"rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":18000},"secondary_window":{"used_percent":null,"limit_window_seconds":604800}}}`},
		{"model quota without account quota", `{"rate_limit":{},"additional_rate_limits":[{"limit_name":"model","rate_limit":{"primary_window":{"used_percent":0}}}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: codexUsageTestTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(test.body))}, nil
			})}
			details, err := FetchCodexUsageDetails(context.Background(), client, Account{AuthMode: AuthModeOAuth, Token: "test-token"})
			if err == nil || !strings.Contains(err.Error(), "quota is unknown") {
				t.Fatalf("unknown quota returned details %+v, error %v", details, err)
			}
		})
	}
}

func TestFetchCodexUsageKeepsExplicitZeroAndReached(t *testing.T) {
	for _, body := range []string{
		`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000}}}`,
		`{"rate_limit":{"limit_reached":true,"primary_window":{"used_percent":null}}}`,
	} {
		client := &http.Client{Transport: codexUsageTestTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		details, err := FetchCodexUsageDetails(context.Background(), client, Account{AuthMode: AuthModeOAuth, Token: "test-token"})
		if err != nil || len(details.Windows) != 1 {
			t.Fatalf("known quota returned details %+v, error %v", details, err)
		}
		if strings.Contains(body, "limit_reached") {
			if details.Windows[0].Name != "reached" || details.Windows[0].UsedPercent != 100 {
				t.Fatalf("explicit exhaustion = %+v, want reached at 100%%", details.Windows)
			}
		} else if details.Windows[0].UsedPercent != 0 {
			t.Fatalf("explicit zero utilization = %+v", details.Windows)
		}
	}
}

func TestFetchCodexUsageThrottlePreservesRetryAfter(t *testing.T) {
	now := time.Now()
	client := &http.Client{Transport: codexUsageTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Status:     "429 Too Many Requests",
			Header:     http.Header{"Retry-After": []string{"1800"}},
			Body:       io.NopCloser(strings.NewReader(`{"type":"error"}`)),
		}, nil
	})}
	_, err := FetchCodexUsageDetails(context.Background(), client, Account{AuthMode: AuthModeOAuth, Token: "test-token"})
	var throttle *CodexUsageThrottleError
	if !errors.As(err, &throttle) {
		t.Fatalf("error = %v, want CodexUsageThrottleError", err)
	}
	if throttle.RetryAt.Before(now.Add(1799*time.Second)) || throttle.RetryAt.After(now.Add(1801*time.Second)) {
		t.Fatalf("RetryAt = %s, want about 1800s after request", throttle.RetryAt)
	}
}

func TestCodexUsageRetryAfterParsesHTTPDate(t *testing.T) {
	now := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
	want := now.Add(5 * time.Minute)
	got := codexUsageRetryAfter(want.Format(http.TimeFormat), now)
	if !got.Equal(want) {
		t.Fatalf("RetryAt = %s, want %s", got, want)
	}
	if got := codexUsageRetryAfter("9223372036854775807", now); !got.After(now) {
		t.Fatalf("overflowing Retry-After = %s, want a future saturated deadline", got)
	}
}

func TestCodexDisplayWindowsOmitsUnknownModelUtilization(t *testing.T) {
	var usage codexUsageResponse
	if err := json.Unmarshal([]byte(`{"rate_limit":{"primary_window":{"used_percent":20}},"additional_rate_limits":[{"limit_name":"model","rate_limit":{"primary_window":{"used_percent":null}}}]}`), &usage); err != nil {
		t.Fatal(err)
	}
	if windows := usage.displayWindows(); len(windows) != 1 || windows[0].UsedPercent != 20 || windows[0].Feature != "" {
		t.Fatalf("display windows = %+v, want only the known account utilization", windows)
	}
}

func TestDisplayWindowsTagsAdditionalLimitsWithFeature(t *testing.T) {
	// Mirrors a real chatgpt usage response: an account-wide rate_limit plus one
	// additional rate limit whose machine name is a rotating codename
	// (codex_bengalfox) but whose limit_name is the model display name. The
	// Feature must be set from limit_name so routing keys off it rather than the
	// codename or a display-string substring.
	raw := `{
		"plan_type": "pro",
		"rate_limit": {
			"allowed": true,
			"primary_window": {"used_percent": 7, "limit_window_seconds": 18000, "reset_after_seconds": 3600},
			"secondary_window": {"used_percent": 2, "limit_window_seconds": 604800, "reset_after_seconds": 600000}
		},
		"additional_rate_limits": [
			{
				"metered_feature": "codex_bengalfox",
				"limit_name": "GPT-5.3-Codex-Spark",
				"rate_limit": {
					"primary_window": {"used_percent": 0, "limit_window_seconds": 18000, "reset_after_seconds": 3600}
				}
			}
		]
	}`
	var usage codexUsageResponse
	if err := json.Unmarshal([]byte(raw), &usage); err != nil {
		t.Fatal(err)
	}

	accountWide := 0
	featureWindows := 0
	for _, w := range usage.displayWindows() {
		if w.Feature == "" {
			accountWide++
			continue
		}
		if w.Feature != "GPT-5.3-Codex-Spark" {
			t.Fatalf("window %q has feature %q, want GPT-5.3-Codex-Spark", w.Name, w.Feature)
		}
		featureWindows++
	}
	if accountWide == 0 {
		t.Fatal("expected account-wide windows tagged with an empty Feature")
	}
	if featureWindows == 0 {
		t.Fatal("expected the additional rate limit window tagged with its limit_name as Feature")
	}
}

func TestCodexUsageParsesComplimentaryResetInfo(t *testing.T) {
	raw := `{
		"plan_type": "pro",
		"rate_limit": {"allowed": true},
		"complimentary_session_reset": {
			"eligible": true,
			"available": false,
			"consumed": true,
			"used_count": 1,
			"total_count": 1
		}
	}`
	var usage codexUsageResponse
	if err := json.Unmarshal([]byte(raw), &usage); err != nil {
		t.Fatal(err)
	}

	info := usage.ComplimentaryReset
	if info == nil || !info.Known {
		t.Fatalf("complimentary reset info missing: %+v", usage.ComplimentaryReset)
	}
	if !info.Consumed || info.Available {
		t.Fatalf("reset state = consumed:%v available:%v, want consumed only", info.Consumed, info.Available)
	}
	if info.Eligible == nil || !*info.Eligible {
		t.Fatalf("eligible = %+v, want true", info.Eligible)
	}
	if info.Used == nil || *info.Used != 1 || info.Total == nil || *info.Total != 1 {
		t.Fatalf("counts = used:%v total:%v, want 1/1", info.Used, info.Total)
	}
	if info.Source != "complimentary_session_reset" {
		t.Fatalf("source = %q, want complimentary_session_reset", info.Source)
	}
}

func TestCodexUsageParsesOneTimeResetRemainingCount(t *testing.T) {
	raw := `{
		"plan_type": "pro",
		"rate_limit": {"allowed": true},
		"rewards": {
			"one_time_reset": {
				"remaining": 1,
				"total": 1
			}
		}
	}`
	var usage codexUsageResponse
	if err := json.Unmarshal([]byte(raw), &usage); err != nil {
		t.Fatal(err)
	}

	info := usage.ComplimentaryReset
	if info == nil || !info.Available || info.Consumed {
		t.Fatalf("reset state = %+v, want available", info)
	}
	if info.Source != "rewards.one_time_reset" {
		t.Fatalf("source = %q, want rewards.one_time_reset", info.Source)
	}
}

func TestExtraUsageRemainingRejectsNegativeInputs(t *testing.T) {
	for name, info := range map[string]ExtraUsageInfo{
		"negative limit": {MonthlyLimit: float64Ptr(-1), UsedCredits: float64Ptr(0)},
		"negative used":  {MonthlyLimit: float64Ptr(1), UsedCredits: float64Ptr(-1)},
	} {
		t.Run(name, func(t *testing.T) {
			if remaining, ok := info.Remaining(); ok || remaining != 0 {
				t.Fatalf("Remaining() = %v, %v; want 0, false", remaining, ok)
			}
		})
	}
}

func float64Ptr(value float64) *float64 { return &value }

// codexUsageSpendControlFixture is a sanitized copy of a real
// GET /backend-api/wham/usage response for a usage-based business seat
// (plan_type self_serve_business_usage_based, captured 2026-09-05). Only
// user_id, account_id and email were replaced; everything else is verbatim.
// Note rate_limit and additional_rate_limits are null: the seat's only quota
// is spend_control, whose money amounts are JSON strings.
const codexUsageSpendControlFixture = `{
  "user_id": "user-redacted",
  "account_id": "acct-redacted",
  "email": "seat@example.com",
  "plan_type": "self_serve_business_usage_based",
  "rate_limit": null,
  "code_review_rate_limit": null,
  "additional_rate_limits": null,
  "model_usage": {
    "gpt-6-astra": {
      "available": true,
      "available_at": null,
      "credits_would_enable": false
    }
  },
  "credits": {
    "has_credits": true,
    "unlimited": false,
    "overage_limit_reached": false,
    "balance": null,
    "approx_local_messages": null,
    "approx_cloud_messages": null
  },
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
  "rate_limit_reset_credits": {
    "available_count": 0,
    "applicable_available_count": 0
  }
}`

func decodeCodexUsage(t *testing.T, raw string) codexUsageResponse {
	t.Helper()
	// Always go through json.Unmarshal on the outer type so the custom
	// UnmarshalJSON (and its alias-type detour) is what decodes the fixture,
	// exactly as FetchCodexUsageDetails does.
	var usage codexUsageResponse
	if err := json.Unmarshal([]byte(raw), &usage); err != nil {
		t.Fatalf("decode usage: %v", err)
	}
	return usage
}

// withoutResetAt returns a copy of windows with ResetAt cleared. A rate_limit
// window's ResetAt is anchored to the fetch time (TestCodexWindowsCarryAbsoluteResetAt
// covers it), so comparisons about which windows a response yields drop it.
// Spend windows carry no ResetAt, and the tests that compare only those keep
// the exact comparison so that stays pinned.
func withoutResetAt(windows []UsageWindow) []UsageWindow {
	if windows == nil {
		return nil
	}
	out := append([]UsageWindow(nil), windows...)
	for i := range out {
		out[i].ResetAt = time.Time{}
	}
	return out
}

func findUsageWindow(windows []UsageWindow, name string) (UsageWindow, bool) {
	for _, w := range windows {
		if w.Name == name {
			return w, true
		}
	}
	return UsageWindow{}, false
}

// Regression: a usage-based seat reports rate_limit: null and carries its only
// quota in spend_control. Before spend_control was decoded this fixture
// produced zero windows, so a seat at 99% spend scored Headroom 1.0 and won
// every new-session pick over seats with real, lower usage.
func TestCodexUsageSpendControlBecomesAccountWideWindow(t *testing.T) {
	usage := decodeCodexUsage(t, codexUsageSpendControlFixture)
	if usage.PlanType != "self_serve_business_usage_based" {
		t.Fatalf("plan_type = %q", usage.PlanType)
	}
	if usage.SpendControl == nil || usage.SpendControl.IndividualLimit == nil {
		t.Fatalf("spend_control did not decode through the alias type: %+v", usage.SpendControl)
	}

	windows := usage.windows()
	if len(windows) != 1 {
		t.Fatalf("windows() = %+v, want exactly the spend window (rate_limit is null)", windows)
	}
	spend, ok := findUsageWindow(windows, "spend")
	if !ok {
		t.Fatalf("windows() = %+v, missing %q", windows, "spend")
	}
	want := UsageWindow{Name: "spend", UsedPercent: 99, LimitWindowSeconds: 30 * 24 * 3600, ResetAfterSeconds: 2190075}
	if spend != want {
		t.Fatalf("spend window = %+v, want %+v", spend, want)
	}
	if spend.LimitWindowSeconds != 2592000 {
		t.Fatalf("LimitWindowSeconds = %d, want 2592000", spend.LimitWindowSeconds)
	}
	if spend.Feature != "" {
		t.Fatalf("spend window must be account-wide (Feature empty), got %q", spend.Feature)
	}
	if _, reached := findUsageWindow(windows, "spend/reached"); reached {
		t.Fatalf("reached=false must not emit spend/reached: %+v", windows)
	}

	// displayWindows() is what the proxy stores as status.Windows and scores
	// from, so the spend window has to surface there too.
	display := usage.displayWindows()
	got, ok := findUsageWindow(display, "spend")
	if !ok {
		t.Fatalf("displayWindows() = %+v, missing %q", display, "spend")
	}
	if got != want {
		t.Fatalf("displayWindows() spend = %+v, want %+v", got, want)
	}
	for _, w := range display {
		if w.Feature != "" {
			t.Fatalf("no window may be model-tagged from model_usage; got %+v", w)
		}
	}
}

// A rate-limited (subscription) response without spend_control must produce
// exactly the windows it produced before spend_control was decoded, whether
// the key is absent or explicitly null.
func TestCodexUsageWindowsUnchangedWithoutSpendControl(t *testing.T) {
	base := `
		"plan_type": "pro",
		"rate_limit": {
			"allowed": true,
			"limit_reached": true,
			"primary_window": {"used_percent": 7, "limit_window_seconds": 18000, "reset_after_seconds": 3600},
			"secondary_window": {"used_percent": 100, "limit_window_seconds": 604800, "reset_after_seconds": 600000}
		},
		"additional_rate_limits": [
			{
				"metered_feature": "codex_bengalfox",
				"limit_name": "GPT-5.3-Codex-Spark",
				"rate_limit": {
					"primary_window": {"used_percent": 12, "limit_window_seconds": 18000, "reset_after_seconds": 1800}
				}
			}
		]`
	wantBase := []UsageWindow{
		{Name: "primary", UsedPercent: 7, LimitWindowSeconds: 18000, ResetAfterSeconds: 3600},
		{Name: "secondary", UsedPercent: 100, LimitWindowSeconds: 604800, ResetAfterSeconds: 600000},
		{Name: "reached", UsedPercent: 100},
	}
	wantDisplay := append(append([]UsageWindow(nil), wantBase...),
		UsageWindow{Name: "GPT-5.3-Codex-Spark/primary", Feature: "GPT-5.3-Codex-Spark", UsedPercent: 12, LimitWindowSeconds: 18000, ResetAfterSeconds: 1800},
	)
	for name, raw := range map[string]string{
		"absent": "{" + base + "}",
		"null":   "{" + base + `, "spend_control": null}`,
	} {
		usage := decodeCodexUsage(t, raw)
		if got := withoutResetAt(usage.windows()); !reflect.DeepEqual(got, wantBase) {
			t.Fatalf("%s: windows() = %+v, want %+v", name, got, wantBase)
		}
		if got := withoutResetAt(usage.displayWindows()); !reflect.DeepEqual(got, wantDisplay) {
			t.Fatalf("%s: displayWindows() = %+v, want %+v", name, got, wantDisplay)
		}
	}
}

func TestCodexUsageSpendControlReachedEmitsHardStop(t *testing.T) {
	raw := `{
		"plan_type": "self_serve_business_usage_based",
		"rate_limit": null,
		"spend_control": {
			"reached": true,
			"individual_limit": {
				"source": "workspace_spend_controls",
				"limit": "25000",
				"used": "25000",
				"remaining": "0",
				"used_percent": 100,
				"remaining_percent": 0,
				"reset_after_seconds": 86400,
				"reset_at": 1790812800
			}
		}
	}`
	usage := decodeCodexUsage(t, raw)
	want := []UsageWindow{
		{Name: "spend", UsedPercent: 100, LimitWindowSeconds: 30 * 24 * 3600, ResetAfterSeconds: 86400},
		{Name: "spend/reached", UsedPercent: 100},
	}
	if got := usage.windows(); !reflect.DeepEqual(got, want) {
		t.Fatalf("windows() = %+v, want %+v", got, want)
	}
	if got := usage.displayWindows(); !reflect.DeepEqual(got, want) {
		t.Fatalf("displayWindows() = %+v, want %+v", got, want)
	}

	// reached without a decodable individual_limit still hard-stops.
	usage = decodeCodexUsage(t, `{"plan_type": "self_serve_business_usage_based", "spend_control": {"reached": true, "individual_limit": null}}`)
	want = []UsageWindow{{Name: "spend/reached", UsedPercent: 100}}
	if got := usage.windows(); !reflect.DeepEqual(got, want) {
		t.Fatalf("null individual_limit: windows() = %+v, want %+v", got, want)
	}
}

func TestCodexUsageSpendControlAmountsDecodeAsStringOrNumber(t *testing.T) {
	// No used_percent in either variant, so the window must be derived from
	// used/limit and both encodings must agree.
	asStrings := `{"spend_control": {"reached": false, "individual_limit": {
		"limit": "25000", "used": "24745.309142947197", "remaining": "254.69085705280304",
		"reset_after_seconds": 2190075, "reset_at": 1790812800}}}`
	asNumbers := `{"spend_control": {"reached": false, "individual_limit": {
		"limit": 25000, "used": 24745.309142947197, "remaining": 254.69085705280304,
		"reset_after_seconds": 2190075, "reset_at": 1790812800}}}`

	fromStrings := decodeCodexUsage(t, asStrings)
	fromNumbers := decodeCodexUsage(t, asNumbers)
	for name, usage := range map[string]codexUsageResponse{"strings": fromStrings, "numbers": fromNumbers} {
		limit := usage.SpendControl.IndividualLimit
		if !limit.Limit.Valid || limit.Limit.Value != 25000 {
			t.Fatalf("%s: limit = %+v, want 25000", name, limit.Limit)
		}
		if !limit.Used.Valid || limit.Used.Value != 24745.309142947197 {
			t.Fatalf("%s: used = %+v", name, limit.Used)
		}
		if !limit.Remaining.Valid || limit.Remaining.Value != 254.69085705280304 {
			t.Fatalf("%s: remaining = %+v", name, limit.Remaining)
		}
		if limit.UsedPercent.Valid {
			t.Fatalf("%s: used_percent absent must decode as invalid, got %+v", name, limit.UsedPercent)
		}
		spend, ok := findUsageWindow(usage.windows(), "spend")
		if !ok {
			t.Fatalf("%s: windows() = %+v, missing spend", name, usage.windows())
		}
		if spend.UsedPercent < 98.9 || spend.UsedPercent > 99.0 {
			t.Fatalf("%s: derived UsedPercent = %v, want used/limit*100 ~ 98.98", name, spend.UsedPercent)
		}
	}
	if fromStrings.windows()[0] != fromNumbers.windows()[0] {
		t.Fatalf("string and numeric encodings disagree: %+v vs %+v", fromStrings.windows()[0], fromNumbers.windows()[0])
	}

	// An explicit used_percent wins over the derived value, and a zero or
	// missing limit with no used_percent yields no window rather than NaN.
	explicit := decodeCodexUsage(t, `{"spend_control": {"reached": false, "individual_limit": {"limit": "100", "used": "10", "used_percent": "42"}}}`)
	if spend, ok := findUsageWindow(explicit.windows(), "spend"); !ok || spend.UsedPercent != 42 {
		t.Fatalf("explicit used_percent (string) = %+v, want 42", spend)
	}
	noLimit := decodeCodexUsage(t, `{"spend_control": {"reached": false, "individual_limit": {"limit": "0", "used": "10"}}}`)
	if got := noLimit.windows(); len(got) != 0 {
		t.Fatalf("zero limit without used_percent must emit no window, got %+v", got)
	}
	empty := decodeCodexUsage(t, `{"spend_control": {"reached": false, "individual_limit": {"limit": "", "used": null}}}`)
	if got := empty.windows(); len(got) != 0 {
		t.Fatalf("empty/null amounts must emit no window, got %+v", got)
	}

	// Malformed amounts must not fail the whole usage decode: the rate_limit
	// windows alongside them still have to come through.
	malformed := decodeCodexUsage(t, `{
		"rate_limit": {"primary_window": {"used_percent": 5, "limit_window_seconds": 18000, "reset_after_seconds": 60}},
		"spend_control": {"reached": false, "individual_limit": {"limit": "unlimited", "used": true, "remaining": {"x": 1}}}
	}`)
	if got := withoutResetAt(malformed.windows()); !reflect.DeepEqual(got, []UsageWindow{{Name: "primary", UsedPercent: 5, LimitWindowSeconds: 18000, ResetAfterSeconds: 60}}) {
		t.Fatalf("malformed spend amounts: windows() = %+v, want only the rate_limit window", got)
	}

	// strconv.ParseFloat accepts NaN/Inf spellings with a nil error. They must
	// decode as "not reported": a NaN UsedPercent scores as Headroom 1.0
	// (clampPercent's comparisons are all false) and encoding/json refuses to
	// marshal NaN/Inf, which would truncate /_subrouter/usage-status for every
	// seat. Covers every spelling in each amount slot, string and (for
	// symmetry) the derived used/limit path.
	for name, raw := range map[string]string{
		"used_percent NaN":   `{"spend_control": {"reached": false, "individual_limit": {"limit": "Inf", "used": "NaN", "used_percent": "nan"}}}`,
		"used_percent +Inf":  `{"spend_control": {"reached": false, "individual_limit": {"used_percent": "+Inf"}}}`,
		"used_percent -inf":  `{"spend_control": {"reached": false, "individual_limit": {"used_percent": "-inf"}}}`,
		"used_percent infty": `{"spend_control": {"reached": false, "individual_limit": {"used_percent": "infinity"}}}`,
		"limit Inf derived":  `{"spend_control": {"reached": false, "individual_limit": {"limit": "Inf", "used": "10"}}}`,
		"used Inf derived":   `{"spend_control": {"reached": false, "individual_limit": {"limit": "100", "used": "Inf"}}}`,
		"used NaN derived":   `{"spend_control": {"reached": false, "individual_limit": {"limit": "100", "used": "NaN"}}}`,
	} {
		usage := decodeCodexUsage(t, raw)
		limit := usage.SpendControl.IndividualLimit
		for field, amount := range map[string]codexQuotaAmount{"limit": limit.Limit, "used": limit.Used, "used_percent": limit.UsedPercent} {
			if amount.Valid && (math.IsNaN(amount.Value) || math.IsInf(amount.Value, 0)) {
				t.Fatalf("%s: %s decoded as Valid non-finite %+v", name, field, amount)
			}
		}
		if got := usage.windows(); len(got) != 0 {
			t.Fatalf("%s: non-finite amounts must emit no window, got %+v", name, got)
		}
		for _, w := range usage.displayWindows() {
			if math.IsNaN(w.UsedPercent) || math.IsInf(w.UsedPercent, 0) {
				t.Fatalf("%s: non-finite UsedPercent reached displayWindows: %+v", name, w)
			}
		}
		// The whole status payload must still be JSON-encodable.
		if _, err := json.Marshal(usage.displayWindows()); err != nil {
			t.Fatalf("%s: displayWindows not encodable: %v", name, err)
		}
	}
	// A finite but out-of-range literal ("1e400") is rejected by both
	// strconv.ParseFloat (ErrRange) and encoding/json; it must not become Inf.
	huge := decodeCodexUsage(t, `{"spend_control": {"reached": false, "individual_limit": {"used_percent": "1e400"}}}`)
	if huge.SpendControl.IndividualLimit.UsedPercent.Valid {
		t.Fatalf("1e400 must decode as invalid, got %+v", huge.SpendControl.IndividualLimit.UsedPercent)
	}
}

// A subscription (Business/Team) seat can report included-usage windows
// (rate_limit.primary/secondary) AND spend_control together: codex-cli's own
// RateLimitSnapshot carries primary/secondary alongside individual_limit and
// spend_control_reached. Spend controls cap credit/overage spend, which is only
// consumed after included usage is gone, so on such a seat the spend cap is not
// the binding quota. Folding it into the base score (MIN remaining across
// windows) would demote a seat at 20% included usage to Headroom 0.10 on a 90%
// spend cap, or hard-stop it on reached=true while 80% of its included quota is
// free. Seats with included windows must produce exactly the windows they did
// before spend_control was decoded.
func TestCodexUsageSpendControlIgnoredWhenIncludedUsageWindowsExist(t *testing.T) {
	spend := `"spend_control": {
		"reached": true,
		"individual_limit": {
			"source": "workspace_spend_controls",
			"limit": "500", "used": "450", "remaining": "50",
			"used_percent": 90, "remaining_percent": 10,
			"reset_after_seconds": 2190075, "reset_at": 1790812800
		}
	}`
	primary := `"primary_window": {"used_percent": 20, "limit_window_seconds": 18000, "reset_after_seconds": 3600}`
	secondary := `"secondary_window": {"used_percent": 15, "limit_window_seconds": 604800, "reset_after_seconds": 600000}`
	wantPrimary := UsageWindow{Name: "primary", UsedPercent: 20, LimitWindowSeconds: 18000, ResetAfterSeconds: 3600}
	wantSecondary := UsageWindow{Name: "secondary", UsedPercent: 15, LimitWindowSeconds: 604800, ResetAfterSeconds: 600000}

	cases := []struct {
		name string
		raw  string
		want []UsageWindow
	}{
		{
			name: "primary and secondary",
			raw:  `{"plan_type": "business", "rate_limit": {"allowed": true, "limit_reached": false, ` + primary + `, ` + secondary + `}, ` + spend + `}`,
			want: []UsageWindow{wantPrimary, wantSecondary},
		},
		{
			name: "primary only",
			raw:  `{"plan_type": "business", "rate_limit": {"allowed": true, "limit_reached": false, ` + primary + `}, ` + spend + `}`,
			want: []UsageWindow{wantPrimary},
		},
		{
			name: "secondary only",
			raw:  `{"plan_type": "business", "rate_limit": {"allowed": true, "limit_reached": false, ` + secondary + `}, ` + spend + `}`,
			want: []UsageWindow{wantSecondary},
		},
		{
			// Included usage exhausted: the rate_limit hard stop already
			// carries the verdict; spend adds nothing.
			name: "included limit_reached",
			raw:  `{"plan_type": "business", "rate_limit": {"allowed": false, "limit_reached": true, ` + primary + `, ` + secondary + `}, ` + spend + `}`,
			want: []UsageWindow{wantPrimary, wantSecondary, {Name: "reached", UsedPercent: 100}},
		},
	}
	for _, tc := range cases {
		usage := decodeCodexUsage(t, tc.raw)
		if usage.SpendControl == nil || !usage.SpendControl.Reached {
			t.Fatalf("%s: fixture lost spend_control: %+v", tc.name, usage.SpendControl)
		}
		if got := withoutResetAt(usage.windows()); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: windows() = %+v, want rate_limit windows only %+v", tc.name, got, tc.want)
		}
		if got := withoutResetAt(usage.displayWindows()); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: displayWindows() = %+v, want rate_limit windows only %+v", tc.name, got, tc.want)
		}
		for _, w := range usage.windows() {
			if w.Name == "spend" || w.Name == "spend/reached" {
				t.Fatalf("%s: spend window leaked onto a seat with included usage: %+v", tc.name, w)
			}
		}
	}

	// The guard keys on included WINDOWS, not on rate_limit being non-null: a
	// present-but-empty rate_limit object still leaves spend as the only quota.
	for name, raw := range map[string]string{
		"rate_limit null":         `{"plan_type": "self_serve_business_usage_based", "rate_limit": null, ` + spend + `}`,
		"rate_limit absent":       `{"plan_type": "self_serve_business_usage_based", ` + spend + `}`,
		"rate_limit empty":        `{"plan_type": "self_serve_business_usage_based", "rate_limit": {}, ` + spend + `}`,
		"rate_limit windows null": `{"plan_type": "self_serve_business_usage_based", "rate_limit": {"allowed": true, "limit_reached": false, "primary_window": null, "secondary_window": null}, ` + spend + `}`,
	} {
		usage := decodeCodexUsage(t, raw)
		want := []UsageWindow{
			{Name: "spend", UsedPercent: 90, LimitWindowSeconds: 30 * 24 * 3600, ResetAfterSeconds: 2190075},
			{Name: "spend/reached", UsedPercent: 100},
		}
		if got := usage.windows(); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: windows() = %+v, want spend windows %+v", name, got, want)
		}
	}
	// rate_limit with only the hard stop and no windows: both stops coexist.
	both := decodeCodexUsage(t, `{"rate_limit": {"allowed": false, "limit_reached": true}, `+spend+`}`)
	want := []UsageWindow{
		{Name: "reached", UsedPercent: 100},
		{Name: "spend", UsedPercent: 90, LimitWindowSeconds: 30 * 24 * 3600, ResetAfterSeconds: 2190075},
		{Name: "spend/reached", UsedPercent: 100},
	}
	if got := both.windows(); !reflect.DeepEqual(got, want) {
		t.Fatalf("reached without windows: windows() = %+v, want %+v", got, want)
	}
}

func TestCodexUsageSpendControlDerivesResetFromResetAt(t *testing.T) {
	// reset_after_seconds missing: fall back to reset_at relative to now, as
	// codexLimitWindow already does for rate_limit windows.
	raw := `{"spend_control": {"reached": false, "individual_limit": {"used_percent": 50, "reset_at": 4102444800}}}`
	usage := decodeCodexUsage(t, raw)
	spend, ok := findUsageWindow(usage.windows(), "spend")
	if !ok {
		t.Fatalf("windows() = %+v, missing spend", usage.windows())
	}
	if spend.ResetAfterSeconds <= 0 {
		t.Fatalf("ResetAfterSeconds = %d, want derived from reset_at in the future", spend.ResetAfterSeconds)
	}
	past := decodeCodexUsage(t, `{"spend_control": {"reached": false, "individual_limit": {"used_percent": 50, "reset_at": 1}}}`)
	if spend, _ := findUsageWindow(past.windows(), "spend"); spend.ResetAfterSeconds != 0 {
		t.Fatalf("past reset_at must clamp to 0, got %d", spend.ResetAfterSeconds)
	}
}

// fetchCodexUsageBody runs FetchCodexUsageDetails -- the one entry point every
// sweep, `sr status` and reset path uses -- over a canned usage body.
func fetchCodexUsageBody(t *testing.T, body string) (CodexUsageDetails, error) {
	t.Helper()
	client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, body), nil
	})}
	return FetchCodexUsageDetails(context.Background(), client, Account{ID: "seat", Provider: ProviderCodex, AuthMode: AuthModeOAuth, Token: "test-token"})
}

// spend_control is decoded for every Codex seat, subscription seats included,
// and the upstream already mixes string and number encodings inside it. A
// field that drifts type there must never fail the response it sits in: on
// a subscription seat that would drop the rate_limit windows and leave the
// seat unmeasured, for a quota it does not even score. Before, each of these
// failed json.Unmarshal for the whole response.
func TestCodexUsageSpendControlDriftNeverSinksTheResponse(t *testing.T) {
	rateLimit := `"rate_limit": {"allowed": true, "limit_reached": false,
		"primary_window": {"used_percent": 20, "limit_window_seconds": 18000, "reset_after_seconds": 3600},
		"secondary_window": {"used_percent": 15, "limit_window_seconds": 604800, "reset_after_seconds": 600000}}`
	want := []UsageWindow{
		{Name: "primary", UsedPercent: 20, LimitWindowSeconds: 18000, ResetAfterSeconds: 3600},
		{Name: "secondary", UsedPercent: 15, LimitWindowSeconds: 604800, ResetAfterSeconds: 600000},
	}
	for name, spend := range map[string]string{
		"reset_at ISO string":        `{"reached": false, "individual_limit": {"used_percent": 90, "reset_at": "2026-10-01T00:00:00Z"}}`,
		"fractional reset_after":     `{"reached": false, "individual_limit": {"used_percent": 90, "reset_after_seconds": 2190075.5}}`,
		"string reset_after":         `{"reached": false, "individual_limit": {"used_percent": 90, "reset_after_seconds": "2190075"}}`,
		"reached as a word":          `{"reached": "no", "individual_limit": {"used_percent": 90}}`,
		"reached as a string bool":   `{"reached": "false", "individual_limit": {"used_percent": 90}}`,
		"source an object":           `{"reached": false, "individual_limit": {"source": {"kind": "workspace"}, "used_percent": 90}}`,
		"individual_limit a string":  `{"reached": false, "individual_limit": "25000"}`,
		"spend_control a string":     `"capped"`,
		"spend_control an array":     `[1, 2]`,
		"individual_limit an array":  `{"reached": true, "individual_limit": [90]}`,
		"reached an object":          `{"reached": {"value": true}, "individual_limit": {"used_percent": 90}}`,
		"remaining_percent a string": `{"reached": false, "individual_limit": {"used_percent": 90, "remaining_percent": "ten"}}`,
	} {
		raw := `{"plan_type": "business", ` + rateLimit + `, "spend_control": ` + spend + `}`
		var usage codexUsageResponse
		if err := json.Unmarshal([]byte(raw), &usage); err != nil {
			t.Fatalf("%s: a drifted spend_control failed the whole response: %v", name, err)
		}
		if got := withoutResetAt(usage.windows()); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: windows() = %+v, want the rate_limit windows %+v", name, got, want)
		}
		details, err := fetchCodexUsageBody(t, raw)
		if err != nil {
			t.Fatalf("%s: FetchCodexUsageDetails failed a subscription seat on its spend_control: %v", name, err)
		}
		if got := withoutResetAt(details.Windows); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: fetched windows = %+v, want %+v", name, got, want)
		}
	}
}

// On a usage-based seat the same drift costs at most the drifted field: a
// fractional or string reset still dates the window, and reached as the
// string "true" still hard-stops the seat.
func TestCodexUsageSpendControlLenientScalarsKeepTheSpendWindow(t *testing.T) {
	for name, raw := range map[string]string{
		"fractional reset_after": `{"spend_control": {"reached": false, "individual_limit": {"used_percent": 99, "reset_after_seconds": 2190075.5}}}`,
		"string reset_after":     `{"spend_control": {"reached": false, "individual_limit": {"used_percent": 99, "reset_after_seconds": "2190075"}}}`,
	} {
		usage := decodeCodexUsage(t, raw)
		want := []UsageWindow{{Name: "spend", UsedPercent: 99, LimitWindowSeconds: 30 * 24 * 3600, ResetAfterSeconds: 2190075}}
		if got := usage.windows(); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: windows() = %+v, want %+v", name, got, want)
		}
	}
	reached := decodeCodexUsage(t, `{"spend_control": {"reached": "true", "individual_limit": {"used_percent": 100}}}`)
	if _, ok := findUsageWindow(reached.windows(), CodexSpendReachedWindowName); !ok {
		t.Fatalf(`reached "true": windows() = %+v, want spend/reached`, reached.windows())
	}
	// A reset too large for an int64 is unknown, never a wrapped negative.
	huge := decodeCodexUsage(t, `{"spend_control": {"reached": false, "individual_limit": {"used_percent": 50, "reset_after_seconds": 1e300, "reset_at": 1e300}}}`)
	if spend, ok := findUsageWindow(huge.windows(), "spend"); !ok || spend.ResetAfterSeconds != 0 {
		t.Fatalf("1e300 reset: spend window = %+v (found %v), want ResetAfterSeconds 0", spend, ok)
	}
}

// A usage-based seat (no included-usage window) whose spend_control arrives
// but yields no spend window must not be published as zero windows: those
// score Headroom 1.0 and Fresh, the phantom this parser exists to remove. The
// fetch fails instead, so the seat reads unmeasured and the error is shown
// and logged. Before, each of these fetched cleanly with no windows.
func TestCodexUsageUnreadableSpendControlLeavesTheSeatUnmeasured(t *testing.T) {
	for name, spend := range map[string]string{
		"used_percent NaN":             `{"reached": false, "individual_limit": {"used_percent": "NaN"}}`,
		"every amount null":            `{"reached": false, "individual_limit": {"used_percent": null, "limit": null, "used": null}}`,
		"zero limit, no percentage":    `{"reached": false, "individual_limit": {"limit": "0", "used": "10"}}`,
		"empty individual_limit":       `{"reached": false, "individual_limit": {}}`,
		"individual_limit a string":    `{"reached": false, "individual_limit": "25000"}`,
		"spend_control a string":       `"capped"`,
		"reached an object, no amount": `{"reached": {"value": true}, "individual_limit": {"used_percent": "unknown"}}`,
	} {
		raw := `{"plan_type": "self_serve_business_usage_based", "rate_limit": null, "spend_control": ` + spend + `}`
		details, err := fetchCodexUsageBody(t, raw)
		if err == nil {
			t.Fatalf("%s: fetched cleanly with windows %+v; an unreadable cap must leave the seat unmeasured, not full", name, details.Windows)
		}
		if !strings.Contains(err.Error(), "spend_control") {
			t.Fatalf("%s: err = %v, want it to name spend_control", name, err)
		}
	}

	// What still fetches cleanly. A readable percentage, obviously. reached on
	// its own is authoritative. And individual_limit: null is what a seat with
	// no per-member cap would send: zero windows is then the truth, not a gap.
	for name, tc := range map[string]struct {
		spend string
		want  []UsageWindow
	}{
		"readable":                     {`{"reached": false, "individual_limit": {"used_percent": 40}}`, []UsageWindow{{Name: "spend", UsedPercent: 40, LimitWindowSeconds: 30 * 24 * 3600}}},
		"reached, amounts unreadable":  {`{"reached": true, "individual_limit": {"used_percent": "NaN"}}`, []UsageWindow{{Name: "spend/reached", UsedPercent: 100}}},
		"reached, no individual_limit": {`{"reached": true, "individual_limit": null}`, []UsageWindow{{Name: "spend/reached", UsedPercent: 100}}},
		"no individual cap":            {`{"reached": false, "individual_limit": null}`, nil},
	} {
		raw := `{"plan_type": "self_serve_business_usage_based", "rate_limit": null, "spend_control": ` + tc.spend + `}`
		details, err := fetchCodexUsageBody(t, raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(details.Windows, tc.want) {
			t.Fatalf("%s: windows = %+v, want %+v", name, details.Windows, tc.want)
		}
	}

	// A null spend_control is not an unreadable one, but with rate_limit null
	// too the seat reported no quota at all. Zero windows would score it
	// Headroom 1.0 and Fresh, so the fetch fails as an unknown quota (upstream
	// #497), never with the spend_control message.
	_, err := fetchCodexUsageBody(t, `{"plan_type": "self_serve_business_usage_based", "rate_limit": null, "spend_control": null}`)
	if err == nil || !strings.Contains(err.Error(), "quota is unknown") || strings.Contains(err.Error(), "spend_control") {
		t.Fatalf("spend_control null with rate_limit null: err = %v, want the unknown-quota error", err)
	}
}
