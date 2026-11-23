package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

type UsageWindow struct {
	Name               string
	UsedPercent        float64
	LimitWindowSeconds int64
	// ResetAfterSeconds is measured from when the usage was fetched. It is
	// kept for older clients; newer ones read ResetAt.
	ResetAfterSeconds int64
	// ResetAt is the absolute reset time, taken from the provider when it
	// reports one, else fetch time plus ResetAfterSeconds. Zero from servers
	// that predate it; see ResetTime.
	ResetAt time.Time `json:"reset_at,omitzero"`
	// Feature is the upstream limit_name of the additional (per-model) rate
	// limit this window belongs to, e.g. "GPT-5.3-Codex-Spark". Empty for the
	// account-wide primary/secondary windows. Used to route a request to its
	// model-specific quota pool without matching on display strings.
	Feature string
	// ExtraUsage carries Claude's paid-usage allowance on the synthetic
	// "extra" window. It is status and fallback-routing metadata, not a
	// subscription quota window, so scoring code must exclude it.
	ExtraUsage *ExtraUsageInfo `json:"extra_usage,omitempty"`
}

// MarshalJSON writes the window as encoding/json would, except a ResetAt it
// cannot write: Time.MarshalJSON refuses a year outside [0, 9999], and one
// such window failed the whole encode, so /_subrouter/usage-status answered
// 200 with an empty body for every account (writeJSON drops the error). This
// is the PATCHES §42 rule for non-finite amounts, applied to the reset
// upstream #465 added: a reset_at in milliseconds or a far-future sentinel
// from any provider reads as unknown here, never as a broken status body.
func (w UsageWindow) MarshalJSON() ([]byte, error) {
	type plain UsageWindow
	out := plain(w)
	if !encodableTime(out.ResetAt) {
		out.ResetAt = time.Time{}
	}
	return json.Marshal(out)
}

// encodableTime reports whether encoding/json can write t (RFC 3339 years
// 0 through 9999).
func encodableTime(t time.Time) bool {
	year := t.Year()
	return year >= 0 && year <= 9999
}

// ResetTime returns when the window resets: ResetAt when set, else
// fetchedAt plus ResetAfterSeconds for payloads from older servers. It is
// zero when the reset is unknown.
func (w UsageWindow) ResetTime(fetchedAt time.Time) time.Time {
	if !w.ResetAt.IsZero() {
		return w.ResetAt
	}
	if w.ResetAfterSeconds <= 0 || fetchedAt.IsZero() {
		return time.Time{}
	}
	return fetchedAt.Add(time.Duration(w.ResetAfterSeconds) * time.Second)
}

// ResetsAsOf returns a copy of windows whose ResetAfterSeconds is recomputed
// from ResetAt as of now, so relative displays do not lag by the age of the
// reading. Windows without ResetAt keep their ResetAfterSeconds.
//
// So does a window whose clock has not started: reset at least the window's
// length (an untouched Codex weekly reads 604800/604800). Its ResetAt is the
// fetch time plus the length, a reset that slides with every reading rather
// than a fixed instant, so there is nothing to re-anchor; re-anchored, it
// read 604799 and below, and the pool board -- which reads reset >= length
// as a fuse never lit -- drew every untouched box account as RUNNING.
func ResetsAsOf(windows []UsageWindow, now time.Time) []UsageWindow {
	if windows == nil {
		return nil
	}
	out := append([]UsageWindow(nil), windows...)
	for i := range out {
		if out[i].ResetAt.IsZero() {
			continue
		}
		if out[i].LimitWindowSeconds > 0 && out[i].ResetAfterSeconds >= out[i].LimitWindowSeconds {
			continue
		}
		out[i].ResetAfterSeconds = max(0, int64(out[i].ResetAt.Sub(now).Seconds()))
	}
	return out
}

// ExtraUsageInfo describes Claude's optional paid usage budget. Anthropic
// reports MonthlyLimit and UsedCredits in US cents; Utilization is percent.
// AutoReload reports Anthropic's auto-reload toggle (null from the API reads
// as off, matching the Claude settings page). DisabledReason, CreditsBalance,
// and AutoReload are display metadata only — routing stays with Remaining.
// CreditsBalance is the prepaid credit remainder in cents; the OAuth usage
// API has only ever returned null for it, so `sr status` fills it locally
// from the claude.ai web session API (see sr_claude_balance.go).
type ExtraUsageInfo struct {
	// EnablementUnknown marks display-only balance records without OAuth settings.
	EnablementUnknown bool     `json:"enablement_unknown,omitempty"`
	IsEnabled         bool     `json:"is_enabled"`
	MonthlyLimit      *float64 `json:"monthly_limit,omitempty"`
	UsedCredits       *float64 `json:"used_credits,omitempty"`
	Utilization       *float64 `json:"utilization,omitempty"`
	// DisabledReason is Anthropic's machine reason when IsEnabled is false,
	// e.g. "out_of_credits".
	DisabledReason string `json:"disabled_reason,omitempty"`
	// CreditsBalance is the remaining prepaid credit balance in cents.
	CreditsBalance *float64 `json:"credits_balance,omitempty"`
	AutoReload     *bool    `json:"auto_reload,omitempty"`
}

// Remaining reports the known positive balance. Both the configured limit and
// used amount must be present: unknown balance must never authorize paid use.
func (e *ExtraUsageInfo) Remaining() (float64, bool) {
	if e == nil || e.MonthlyLimit == nil || e.UsedCredits == nil || *e.MonthlyLimit < 0 || *e.UsedCredits < 0 {
		return 0, false
	}
	remaining := *e.MonthlyLimit - *e.UsedCredits
	if remaining < 0 {
		remaining = 0
	}
	return remaining, true
}

type CodexUsageDetails struct {
	PlanType           string
	Windows            []UsageWindow
	BaseWindows        []UsageWindow
	Credits            *CreditsInfo
	ComplimentaryReset *ComplimentaryResetInfo
	RawRateLimit       codexRateLimitDetails
}

// CodexUsageThrottleError preserves the provider's cooldown for a usage
// endpoint response. The proxy uses RetryAt to avoid polling the same account
// again while ChatGPT has explicitly asked us to wait.
type CodexUsageThrottleError struct {
	Status  string
	RetryAt time.Time
}

func (e *CodexUsageThrottleError) Error() string {
	if e == nil {
		return "Codex usage fetch throttled"
	}
	return fmt.Sprintf("usage fetch failed: %s", e.Status)
}

type CreditsInfo struct {
	HasCredits bool
	Unlimited  bool
	Balance    string
}

type ComplimentaryResetInfo struct {
	Known     bool   `json:"known"`
	Available bool   `json:"available,omitempty"`
	Consumed  bool   `json:"consumed,omitempty"`
	Eligible  *bool  `json:"eligible,omitempty"`
	Remaining *int   `json:"remaining,omitempty"`
	Used      *int   `json:"used,omitempty"`
	Total     *int   `json:"total,omitempty"`
	Status    string `json:"status,omitempty"`
	ResetsAt  string `json:"resets_at,omitempty"`
	Source    string `json:"source,omitempty"`
	// The fields below are set only for a Claude account's saved usage-limit
	// reset (Source "cedar_ember", PATCHES §46). Codex never sets them, so a
	// Codex row's JSON is byte-identical to what it was.
	//
	// ExpiresAt is the use-by time of the grant that expires first among
	// those with a reset left, verbatim from Anthropic (grants[].ends_at).
	ExpiresAt string `json:"expires_at,omitempty"`
	// Clears lists the limits that grant resets (grants[].clears).
	Clears []string `json:"clears,omitempty"`
	// RemainingByKind splits Remaining by what each grant's claim gives
	// back (agentclaude.ResetKind: "week", "fable", "other"; PATCHES §64).
	// A kind with nothing left is absent; the whole map is absent when no
	// grant has a reset left, and on a row from a daemon before §64.
	RemainingByKind map[string]int `json:"remaining_by_kind,omitempty"`
	// WeekHeldUntil is the soonest ends_at, verbatim, among the live grants
	// that clear the general week ("seven_day", or no clears list at all):
	// the only kind of grant that refills a spent week, so the only one the
	// §56 hold ranks and drains an account on. Live means the account is
	// eligible and the grant has a reset left, has not expired and is not
	// paused. "" when no such grant has a readable ends_at.
	WeekHeldUntil string `json:"week_held_until,omitempty"`
	// IneligibleReason is Anthropic's ineligible_reason, verbatim, or
	// "no_profile_scope" for a token that cannot read the block at all.
	IneligibleReason string `json:"ineligible_reason,omitempty"`
	// Held is every reset the block lists, by kind and expiry (HeldResets,
	// PATCHES §91.1), read with it and published beside it as the row's
	// resets, never inside this block. Empty for a known block that holds
	// none. Nil for a block not read, for one restored from JSON (the
	// persisted usage snapshot), and on a Codex reading, whose list the
	// daemon builds from the credit list.
	Held []HeldReset `json:"-"`
}

type codexUsageResponse struct {
	PlanType             string                     `json:"plan_type"`
	RateLimit            codexRateLimitDetails      `json:"rate_limit"`
	Credits              *codexCreditsInfo          `json:"credits"`
	AdditionalRateLimits []codexAdditionalRateLimit `json:"additional_rate_limits"`
	// SpendControl is decoded on its own, best effort, by UnmarshalJSON
	// (parseCodexSpendControl): never through the struct tags, where one
	// drifted field inside it would fail the whole response.
	SpendControl *codexSpendControl `json:"-"`
	// spendControlUnreadable records a spend_control the upstream sent that
	// could not be decoded at all (see unreadableSpendControl).
	spendControlUnreadable bool
	ComplimentaryReset     *ComplimentaryResetInfo `json:"-"`
}

type codexRateLimitDetails struct {
	Allowed         bool              `json:"allowed"`
	LimitReached    bool              `json:"limit_reached"`
	PrimaryWindow   *codexLimitWindow `json:"primary_window"`
	SecondaryWindow *codexLimitWindow `json:"secondary_window"`
}

type codexLimitWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAfterSeconds  int64   `json:"reset_after_seconds"`
	ResetAt            int64   `json:"reset_at"`
	// A missing or null usage value is unknown, not an unused quota window.
	// Keep this private so programmatically constructed windows retain their
	// existing meaning, including an explicitly supplied zero percent.
	unreportedUsedPercent bool
}

func (w *codexLimitWindow) UnmarshalJSON(data []byte) error {
	type alias codexLimitWindow
	var decoded struct {
		alias
		UsedPercent *float64 `json:"used_percent"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*w = codexLimitWindow(decoded.alias)
	w.unreportedUsedPercent = decoded.UsedPercent == nil
	if decoded.UsedPercent != nil {
		w.UsedPercent = *decoded.UsedPercent
	}
	return nil
}

func (w *codexLimitWindow) usageKnown() bool {
	return w != nil && !w.unreportedUsedPercent &&
		!math.IsNaN(w.UsedPercent) && !math.IsInf(w.UsedPercent, 0) &&
		w.UsedPercent >= 0 && w.UsedPercent <= 100
}

type codexCreditsInfo struct {
	HasCredits bool   `json:"has_credits"`
	Unlimited  bool   `json:"unlimited"`
	Balance    string `json:"balance"`
}

type codexAdditionalRateLimit struct {
	MeteredFeature string                `json:"metered_feature"`
	LimitName      string                `json:"limit_name"`
	RateLimit      codexRateLimitDetails `json:"rate_limit"`
}

// codexSpendControl is the quota object usage-based plans (plan_type
// "self_serve_business_usage_based") report. Such plans answer with
// rate_limit: null and additional_rate_limits: null; their only budget is
// the workspace spend cap below, so ignoring it makes a seat at 99% spend
// look untouched (zero windows, Headroom 1.0). Subscription seats may carry
// it too, next to included-usage windows; see spendWindows for why it is
// only scored when no included window exists.
type codexSpendControl struct {
	Reached         codexQuotaFlag   `json:"reached"`
	IndividualLimit *codexSpendLimit `json:"individual_limit"`
}

// codexSpendLimit mirrors spend_control.individual_limit. The upstream encodes
// the money amounts as JSON strings ("25000", "24745.3") while used_percent /
// remaining_percent and the reset fields are numbers; every scalar here is a
// lenient type (codexQuotaAmount) so a future switch in one direction or the
// other, or a fractional reset, costs at most that field. `source`
// ("workspace_spend_controls") is not read.
type codexSpendLimit struct {
	Limit             codexQuotaAmount `json:"limit"`
	Used              codexQuotaAmount `json:"used"`
	Remaining         codexQuotaAmount `json:"remaining"`
	UsedPercent       codexQuotaAmount `json:"used_percent"`
	RemainingPercent  codexQuotaAmount `json:"remaining_percent"`
	ResetAfterSeconds codexQuotaAmount `json:"reset_after_seconds"`
	ResetAt           codexQuotaAmount `json:"reset_at"`
}

// parseCodexSpendControl decodes spend_control on its own. It is read for
// every Codex seat, subscription seats included, and the upstream already
// mixes string and number encodings inside it, so it must never fail the
// response it sits in: a subscription seat whose spend_control drifted would
// otherwise lose its rate_limit windows and go unmeasured. Returns the control
// when it decodes, and unreadable=true for one that is present, not null, and
// not decodable (spend_control or individual_limit not an object).
func parseCodexSpendControl(data []byte) (control *codexSpendControl, unreadable bool) {
	var root struct {
		SpendControl json.RawMessage `json:"spend_control"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, false
	}
	raw := strings.TrimSpace(string(root.SpendControl))
	if raw == "" || raw == "null" {
		return nil, false
	}
	var decoded codexSpendControl
	if err := json.Unmarshal(root.SpendControl, &decoded); err != nil {
		return nil, true
	}
	return &decoded, false
}

// codexQuotaFlag is a JSON boolean that may arrive as a bool or as the string
// "true"/"false". Anything else reads as false, and decoding never fails, for
// the same reason as codexQuotaAmount.
type codexQuotaFlag bool

func (f *codexQuotaFlag) UnmarshalJSON(data []byte) error {
	*f = false
	var value bool
	if err := json.Unmarshal(data, &value); err == nil {
		*f = codexQuotaFlag(value)
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return nil
	}
	if parsed, err := strconv.ParseBool(strings.TrimSpace(text)); err == nil {
		*f = codexQuotaFlag(parsed)
	}
	return nil
}

// codexQuotaAmount is a JSON number that may arrive as a bare number, a
// numeric string, or null/absent. Valid is false for null, absent, empty or
// unparseable values so callers can tell "0" from "not reported". Decoding
// never fails: a malformed spend_control field must not sink the rest of the
// usage response (rate_limit, credits), it just yields no spend window.
type codexQuotaAmount struct {
	Value float64
	Valid bool
}

func (a *codexQuotaAmount) UnmarshalJSON(data []byte) error {
	*a = codexQuotaAmount{}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var value float64
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return nil
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err != nil {
			return nil
		}
		value = parsed
	} else if err := json.Unmarshal(data, &value); err != nil {
		return nil
	}
	// strconv.ParseFloat accepts "NaN", "Inf", "-inf", "infinity" with a nil
	// error. Neither value can travel through this pipeline: encoding/json
	// refuses to marshal them, so one seat with a NaN window would truncate
	// /_subrouter/usage-status for every account, and clampPercent's
	// comparisons are all false for NaN, so a NaN used_percent would score as
	// Headroom 1.0 -- the phantom health this decoder exists to remove. Treat
	// them like any other unparseable value: not reported.
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	*a = codexQuotaAmount{Value: value, Valid: true}
	return nil
}

// seconds reads the amount as a whole number of seconds (a duration or a unix
// timestamp). Anything not reported, not positive, or too large for an int64
// reads as 0, which remainingUntilReset treats as unknown.
func (a codexQuotaAmount) seconds() int64 {
	if !a.Valid || a.Value <= 0 || a.Value >= math.MaxInt64 {
		return 0
	}
	return int64(a.Value)
}

// codexSpendWindowSeconds classifies the spend window for the scheduler and
// for `sr status`. Spend controls are a monthly budget (reset_at lands on the
// first of the next month) and the upstream reports no window length, so a
// nominal 30 days is used: anything above 6h is a long window for scoring
// purposes and anything at or above 6d is a "long quota" for display, so the
// exact month length does not matter.
const codexSpendWindowSeconds int64 = 30 * 24 * 60 * 60

// CodexSpendWindowName and CodexSpendReachedWindowName are the windows
// spendWindows publishes for a usage-based seat. Exported so a consumer that
// must tell a spend cap from a rate limit (IsSpendWindow) matches the names
// this file emits rather than a copy of them.
const (
	CodexSpendWindowName        = "spend"
	CodexSpendReachedWindowName = "spend/reached"
)

func (u *codexUsageResponse) UnmarshalJSON(data []byte) error {
	type alias codexUsageResponse
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*u = codexUsageResponse(decoded)
	u.SpendControl, u.spendControlUnreadable = parseCodexSpendControl(data)
	u.ComplimentaryReset = parseComplimentaryResetInfo(data)
	return nil
}

func FetchCodexUsage(ctx context.Context, client *http.Client, account Account) ([]UsageWindow, error) {
	details, err := FetchCodexUsageDetails(ctx, client, account)
	if err != nil {
		return nil, err
	}
	return details.Windows, nil
}

// CodexUsageStatusError is a non-2xx answer from the Codex usage endpoint.
// Code is OpenAI's error code, read only from a 401.
type CodexUsageStatusError struct {
	StatusCode int
	Status     string
	Code       string
}

func (e *CodexUsageStatusError) Error() string {
	return "usage fetch failed: " + e.Status
}

// CodexAccessTokenRejectedAsExpired reports whether OpenAI refused the usage
// read because it expired the access token. After a ChatGPT plan change it
// does this days before the token's JWT exp, while the refresh token still
// works.
func CodexAccessTokenRejectedAsExpired(err error) bool {
	var statusErr *CodexUsageStatusError
	return errors.As(err, &statusErr) &&
		statusErr.StatusCode == http.StatusUnauthorized &&
		statusErr.Code == "token_expired"
}

// codexUsageErrorCode reads error.code from a usage error body, or detail.code
// when that is absent.
func codexUsageErrorCode(body io.Reader) string {
	var payload struct {
		Error  json.RawMessage `json:"error"`
		Detail json.RawMessage `json:"detail"`
	}
	if json.NewDecoder(body).Decode(&payload) != nil {
		return ""
	}
	for _, raw := range []json.RawMessage{payload.Error, payload.Detail} {
		var field struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(raw, &field) == nil && field.Code != "" {
			return field.Code
		}
	}
	return ""
}

func FetchCodexUsageDetails(ctx context.Context, client *http.Client, account Account) (CodexUsageDetails, error) {
	if account.AuthMode != AuthModeOAuth {
		return CodexUsageDetails{}, fmt.Errorf("usage is only available for OAuth accounts")
	}
	if account.Token == "" {
		return CodexUsageDetails{}, fmt.Errorf("account has no access token")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageURL, nil)
	if err != nil {
		return CodexUsageDetails{}, err
	}
	req.Header.Set("Authorization", account.AuthorizationHeader())
	if account.AccountID != "" {
		req.Header.Set("ChatGPT-Account-ID", account.AccountID)
	}

	res, err := client.Do(req)
	if err != nil {
		return CodexUsageDetails{}, err
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		status := strings.TrimSpace(res.Status)
		if status == "" {
			status = fmt.Sprintf("%d %s", res.StatusCode, http.StatusText(res.StatusCode))
		}
		if res.StatusCode == http.StatusTooManyRequests {
			return CodexUsageDetails{}, &CodexUsageThrottleError{
				Status:  status,
				RetryAt: codexUsageRetryAfter(res.Header.Get("Retry-After"), time.Now()),
			}
		}
		statusErr := &CodexUsageStatusError{StatusCode: res.StatusCode, Status: status}
		if res.StatusCode == http.StatusUnauthorized {
			statusErr.Code = codexUsageErrorCode(io.LimitReader(res.Body, 16<<10))
		}
		return CodexUsageDetails{}, statusErr
	}

	var usage codexUsageResponse
	if err := json.NewDecoder(res.Body).Decode(&usage); err != nil {
		return CodexUsageDetails{}, err
	}
	if usage.unreadableSpendControl() {
		return CodexUsageDetails{}, fmt.Errorf("unreadable spend_control on a seat with no rate_limit window (plan %q): its spend cap is unknown", usage.PlanType)
	}
	if !usage.RateLimit.LimitReached && !usage.accountQuotaReported() {
		return CodexUsageDetails{}, fmt.Errorf("Codex usage quota is unknown: account-wide utilization was not reported")
	}
	details := CodexUsageDetails{
		PlanType:           usage.PlanType,
		Windows:            usage.displayWindows(),
		BaseWindows:        usage.windows(),
		ComplimentaryReset: usage.ComplimentaryReset,
		RawRateLimit:       usage.RateLimit,
	}
	if usage.Credits != nil {
		details.Credits = &CreditsInfo{
			HasCredits: usage.Credits.HasCredits,
			Unlimited:  usage.Credits.Unlimited,
			Balance:    usage.Credits.Balance,
		}
	}
	return details, nil
}

func codexUsageRetryAfter(raw string, now time.Time) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds >= 0 {
		const maxRetryAfter = time.Duration(1<<63 - 1)
		if seconds > int64(maxRetryAfter/time.Second) {
			return now.Add(maxRetryAfter)
		}
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if when, err := http.ParseTime(raw); err == nil {
		return when
	}
	return time.Time{}
}

func parseComplimentaryResetInfo(data []byte) *ComplimentaryResetInfo {
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil
	}
	return findComplimentaryResetInfo(root, "")
}

func findComplimentaryResetInfo(value any, path string) *ComplimentaryResetInfo {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	for key, child := range object {
		childPath := key
		if path != "" {
			childPath = path + "." + key
		}
		if isComplimentaryResetKey(key) {
			if info := parseComplimentaryResetCandidate(child, childPath); info != nil {
				return info
			}
		}
	}
	for key, child := range object {
		if isRateLimitResetKey(key) {
			continue
		}
		childPath := key
		if path != "" {
			childPath = path + "." + key
		}
		if info := findComplimentaryResetInfo(child, childPath); info != nil {
			return info
		}
	}
	return nil
}

func isComplimentaryResetKey(key string) bool {
	normalized := normalizeUsageKey(key)
	return (strings.Contains(normalized, "complimentary") && strings.Contains(normalized, "reset")) ||
		(strings.Contains(normalized, "onetime") && strings.Contains(normalized, "reset")) ||
		strings.Contains(normalized, "resetcredit")
}

func isRateLimitResetKey(key string) bool {
	normalized := normalizeUsageKey(key)
	return normalized == "resetafterseconds" || normalized == "resetat" || normalized == "resetsat"
}

func normalizeUsageKey(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}

func parseComplimentaryResetCandidate(value any, source string) *ComplimentaryResetInfo {
	info := &ComplimentaryResetInfo{Known: true, Source: source}
	switch typed := value.(type) {
	case bool:
		normalizedSource := normalizeUsageKey(source)
		if strings.Contains(normalizedSource, "used") ||
			strings.Contains(normalizedSource, "consumed") ||
			strings.Contains(normalizedSource, "redeemed") {
			info.Consumed = typed
			info.Available = !typed
		} else {
			info.Available = typed
			info.Consumed = !typed
		}
	case string:
		applyComplimentaryResetStatus(info, typed)
	case map[string]any:
		applyComplimentaryResetObject(info, typed)
	default:
		return nil
	}
	if info.Remaining != nil {
		info.Available = *info.Remaining > 0
		if *info.Remaining == 0 {
			info.Consumed = true
		}
	}
	if info.Total != nil && info.Used != nil && *info.Total > 0 {
		info.Consumed = *info.Used >= *info.Total
		info.Available = *info.Used < *info.Total
	}
	return info
}

func applyComplimentaryResetObject(info *ComplimentaryResetInfo, object map[string]any) {
	if value, ok := stringField(object, "status", "state"); ok {
		info.Status = value
		applyComplimentaryResetStatus(info, value)
	}
	if value, ok := boolField(object, "available", "is_available", "can_reset", "can_use", "claimable"); ok {
		info.Available = value
	}
	if value, ok := boolField(object, "consumed", "is_consumed", "used", "is_used", "redeemed", "is_redeemed"); ok {
		info.Consumed = value
	}
	if value, ok := boolField(object, "eligible", "is_eligible"); ok {
		info.Eligible = &value
		if !value {
			info.Available = false
		}
	}
	if value, ok := intField(object, "remaining", "remaining_count", "available_count"); ok {
		info.Remaining = &value
	}
	if value, ok := intField(object, "used_count", "uses", "redeemed_count"); ok {
		info.Used = &value
	}
	if value, ok := intField(object, "total", "total_count", "limit"); ok {
		info.Total = &value
	}
	if value, ok := stringField(object, "resets_at", "reset_at", "expires_at"); ok {
		info.ResetsAt = value
	}
}

func applyComplimentaryResetStatus(info *ComplimentaryResetInfo, status string) {
	normalized := normalizeUsageKey(status)
	switch normalized {
	case "available", "eligible", "unused", "unclaimed", "claimable":
		info.Available = true
		info.Consumed = false
	case "consumed", "used", "redeemed", "claimed":
		info.Consumed = true
		info.Available = false
	case "ineligible", "unavailable", "disabled":
		value := false
		info.Eligible = &value
		info.Available = false
	}
}

func boolField(object map[string]any, names ...string) (bool, bool) {
	for _, name := range names {
		value, ok := object[name]
		if !ok {
			continue
		}
		if parsed, ok := value.(bool); ok {
			return parsed, true
		}
	}
	return false, false
}

func intField(object map[string]any, names ...string) (int, bool) {
	for _, name := range names {
		value, ok := object[name]
		if !ok {
			continue
		}
		switch parsed := value.(type) {
		case float64:
			return int(parsed), true
		case int:
			return parsed, true
		}
	}
	return 0, false
}

func stringField(object map[string]any, names ...string) (string, bool) {
	for _, name := range names {
		value, ok := object[name]
		if !ok {
			continue
		}
		if parsed, ok := value.(string); ok {
			return parsed, true
		}
	}
	return "", false
}

func (u codexUsageResponse) windows() []UsageWindow {
	now := time.Now()
	var windows []UsageWindow
	appendDetails := func(prefix string, details codexRateLimitDetails) {
		if details.PrimaryWindow.usageKnown() {
			windows = append(windows, UsageWindow{
				Name:               prefix + "primary",
				UsedPercent:        details.PrimaryWindow.UsedPercent,
				LimitWindowSeconds: details.PrimaryWindow.LimitWindowSeconds,
				ResetAfterSeconds:  details.PrimaryWindow.resetAfterSeconds(now),
				ResetAt:            details.PrimaryWindow.resetAt(now),
			})
		}
		if details.SecondaryWindow.usageKnown() {
			windows = append(windows, UsageWindow{
				Name:               prefix + "secondary",
				UsedPercent:        details.SecondaryWindow.UsedPercent,
				LimitWindowSeconds: details.SecondaryWindow.LimitWindowSeconds,
				ResetAfterSeconds:  details.SecondaryWindow.resetAfterSeconds(now),
				ResetAt:            details.SecondaryWindow.resetAt(now),
			})
		}
		if details.LimitReached {
			windows = append(windows, UsageWindow{Name: prefix + "reached", UsedPercent: 100})
		}
	}

	appendDetails("", u.RateLimit)
	windows = append(windows, u.spendWindows(now)...)
	return windows
}

// spendWindows converts spend_control into account-wide windows so a
// usage-based seat is scored on the one quota it actually has.
//
// It only does so when the seat has no included-usage windows. A subscription
// (Business/Team) seat can report rate_limit.primary/secondary AND
// spend_control together -- codex-cli's own RateLimitSnapshot carries
// primary/secondary alongside individual_limit and spend_control_reached. On
// such a seat the spend cap governs credit/overage spend, which is only
// consumed after the included windows are exhausted, so it is not the binding
// quota: folding it into the base score (which takes the MIN remaining across
// windows) would demote a seat at 20% included usage to Headroom 0.10 on a 90%
// spend cap, or hard-stop it on reached=true while 80% of its included quota
// is still free. Seats with included windows therefore produce exactly the
// windows they did before spend_control was decoded; the included windows
// already hit 100%/reached when that fallback would matter.
//
// The windows are deliberately account-wide (Feature == ""). The response
// also carries model_usage (e.g. {"gpt-6-astra": {"available": true}}), but
// nothing is derived from it: selectacct.Scheduler.ForModel zero-scores
// every account that lacks a model pool present on any other account, so
// tagging a window with a model name on a usage-based seat would starve every
// rate-limited seat for that model. A spend cap is not a per-model pool; it
// applies to everything the seat does, which is exactly what a base window
// expresses.
func (u codexUsageResponse) spendWindows(now time.Time) []UsageWindow {
	control := u.SpendControl
	if control == nil {
		return nil
	}
	if u.RateLimit.PrimaryWindow != nil || u.RateLimit.SecondaryWindow != nil {
		return nil
	}
	var windows []UsageWindow
	if limit := control.IndividualLimit; limit != nil {
		if usedPercent, ok := limit.usedPercent(); ok {
			// ResetAt stays zero (unknown), as before UsageWindow carried
			// one, so every reader keeps using ResetAfterSeconds here. A
			// ResetAt in the past makes WeeklyCookedWindow and the scheduler
			// skip a window, and on a usage-based seat that skip leaves no
			// window at all, which scores Headroom 1.0.
			windows = append(windows, UsageWindow{
				Name:               CodexSpendWindowName,
				UsedPercent:        usedPercent,
				LimitWindowSeconds: codexSpendWindowSeconds,
				ResetAfterSeconds:  remainingUntilReset(limit.ResetAfterSeconds.seconds(), limit.ResetAt.seconds(), now),
			})
		}
	}
	if control.Reached {
		// Mirrors rate_limit.limit_reached -> "reached": a hard stop the
		// scheduler must treat as exhausted regardless of the percentage.
		windows = append(windows, UsageWindow{Name: CodexSpendReachedWindowName, UsedPercent: 100})
	}
	return windows
}

// unreadableSpendControl reports a seat whose only quota could not be read:
// it has no included-usage window, it sent a spend_control, and nothing came
// of it -- spend_control or individual_limit was not an object, or
// individual_limit carried no usable percentage (NaN, null, a zero limit) and
// reached was not true. Dropping the window alone would publish zero windows,
// which score Headroom 1.0 and Fresh: the phantom this parser exists to
// remove, back on the first schema drift. FetchCodexUsageDetails fails the
// fetch instead, so the seat reads unmeasured and the fetch error is logged
// and shown on its row.
//
// individual_limit: null is not unreadable. It is what a seat with no
// per-member cap would send, and zero windows is then the truth; reached
// still hard-stops it.
func (u codexUsageResponse) unreadableSpendControl() bool {
	if u.RateLimit.PrimaryWindow != nil || u.RateLimit.SecondaryWindow != nil {
		return false
	}
	if u.spendControlUnreadable {
		return true
	}
	return u.SpendControl != nil && u.SpendControl.IndividualLimit != nil && len(u.spendWindows(time.Now())) == 0
}

// accountQuotaReported reports whether the response carries a readable
// account-wide quota. Every included-usage window it sends must report a
// utilization between 0 and 100: a missing or null used_percent is unknown,
// not an unused window. With no included window at all, a decoded
// spend_control is the seat's quota (PATCHES.md §42), including an
// individual_limit of null, which reads as zero windows by design; a seat
// with neither reported nothing, and zero windows would score it Headroom 1.0
// and Fresh. FetchCodexUsageDetails asks only when limit_reached is false:
// that flag alone is a usable verdict.
func (u codexUsageResponse) accountQuotaReported() bool {
	primary, secondary := u.RateLimit.PrimaryWindow, u.RateLimit.SecondaryWindow
	if primary == nil && secondary == nil {
		return u.SpendControl != nil
	}
	return (primary == nil || primary.usageKnown()) && (secondary == nil || secondary.usageKnown())
}

// usedPercent prefers the upstream's own used_percent and falls back to
// deriving it from used/limit when only the amounts are present.
func (l codexSpendLimit) usedPercent() (float64, bool) {
	if l.UsedPercent.Valid {
		return l.UsedPercent.Value, true
	}
	if l.Limit.Valid && l.Used.Valid && l.Limit.Value > 0 {
		// Both operands are finite (UnmarshalJSON guarantees it) but the
		// quotient can still overflow for absurd inputs; never emit a
		// non-finite percentage.
		if percent := l.Used.Value / l.Limit.Value * 100; !math.IsInf(percent, 0) && !math.IsNaN(percent) {
			return percent, true
		}
	}
	return 0, false
}

func (u codexUsageResponse) displayWindows() []UsageWindow {
	windows := u.windows()
	now := time.Now()
	appendDetails := func(prefix, feature string, details codexRateLimitDetails) {
		if details.PrimaryWindow.usageKnown() {
			windows = append(windows, UsageWindow{
				Name:               prefix + "primary",
				Feature:            feature,
				UsedPercent:        details.PrimaryWindow.UsedPercent,
				LimitWindowSeconds: details.PrimaryWindow.LimitWindowSeconds,
				ResetAfterSeconds:  details.PrimaryWindow.resetAfterSeconds(now),
				ResetAt:            details.PrimaryWindow.resetAt(now),
			})
		}
		if details.SecondaryWindow.usageKnown() {
			windows = append(windows, UsageWindow{
				Name:               prefix + "secondary",
				Feature:            feature,
				UsedPercent:        details.SecondaryWindow.UsedPercent,
				LimitWindowSeconds: details.SecondaryWindow.LimitWindowSeconds,
				ResetAfterSeconds:  details.SecondaryWindow.resetAfterSeconds(now),
				ResetAt:            details.SecondaryWindow.resetAt(now),
			})
		}
		if details.LimitReached {
			windows = append(windows, UsageWindow{Name: prefix + "reached", Feature: feature, UsedPercent: 100})
		}
	}
	for _, additional := range u.AdditionalRateLimits {
		name := additional.LimitName
		if name == "" {
			name = additional.MeteredFeature
		}
		if name == "" {
			name = "unknown"
		}
		appendDetails(name+"/", name, additional.RateLimit)
	}
	return windows
}

// resetAt prefers the provider's absolute reset_at and otherwise anchors
// reset_after_seconds to now, the fetch time. A reset_after_seconds too large
// for a time.Duration is unknown rather than a wrapped, possibly past, time:
// a past ResetAt makes WeeklyCookedWindow and the scheduler skip the window.
// A reset_at that is no Unix-seconds time encoding/json can write -- the
// same instant in milliseconds lands in the year 58,000-odd -- is not taken
// either: the window falls back to reset_after_seconds.
func (w codexLimitWindow) resetAt(now time.Time) time.Time {
	if w.ResetAt > 0 && encodableTime(time.Unix(w.ResetAt, 0)) {
		return time.Unix(w.ResetAt, 0)
	}
	if w.ResetAfterSeconds > 0 && w.ResetAfterSeconds <= int64(math.MaxInt64/int64(time.Second)) {
		return now.Add(time.Duration(w.ResetAfterSeconds) * time.Second)
	}
	return time.Time{}
}

func (w codexLimitWindow) resetAfterSeconds(now time.Time) int64 {
	return remainingUntilReset(w.ResetAfterSeconds, w.ResetAt, now)
}

// remainingUntilReset returns the upstream's reset_after_seconds when it is
// reported, otherwise the seconds left from now until the unix reset_at
// timestamp (never negative), or 0 when neither is known.
func remainingUntilReset(resetAfterSeconds, resetAt int64, now time.Time) int64 {
	if resetAfterSeconds > 0 {
		return resetAfterSeconds
	}
	if resetAt <= 0 {
		return 0
	}
	remaining := resetAt - now.Unix()
	if remaining < 0 {
		return 0
	}
	return remaining
}

// WeeklyLimitCooked reports whether an account is blocked by its account-wide
// weekly rate-limit window. See WeeklyCookedWindow for the rule.
func WeeklyLimitCooked(details CodexUsageDetails) bool {
	_, cooked := WeeklyCookedWindow(codexUsageResponse{RateLimit: details.RawRateLimit}.windows())
	return cooked
}
