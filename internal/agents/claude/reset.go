package claude

// Anthropic's saved usage-limit reset (PATCHES §46).
//
// For the Claude Opus 5.5 launch Anthropic gave every Pro and Max account ONE
// saved reset ("one usage-limit reset for Pro and Max", use by 2026-10-22). Its
// program name on the wire is cedar_ember. The state rides on the OAuth usage
// endpoint when the caller opts in (?cedar_ember=1); the plain endpoint answers
// cedar_ember:null. The claim is one POST, copied here from Claude Code 2.1.284:
//
//	POST /api/organizations/{organizationUuid}/reset_rate_limits
//	{"program":"cedar_ember","grant_id":<id>,"request_id":<id>}
//
// with the OAuth bearer and Claude Code's headers. A reset is one-time and
// irreversible, so nothing in this file retries, refreshes, or claims on its
// own: ClaimReset sends exactly one request and reports what came back. A 429
// carries Anthropic's Retry-After back to the caller (the daemon's claim path
// waits it out and resends the same request_id, PATCHES §46.7).

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

const (
	// ResetProgram is the program Claude Code names in a reset claim, and the
	// Source a Claude row's complimentary_reset carries.
	ResetProgram = "cedar_ember"
	// DefaultClaudeCodeVersion is the Claude Code build whose request shape
	// the reset reads and the claim copy, and the version the daemon's
	// Claude usage reads present (ClaudeCodeVersionEnv overrides it).
	DefaultClaudeCodeVersion = "2.1.284"
	// ClaudeCodeVersionEnv overrides DefaultClaudeCodeVersion without a
	// rebuild: if Anthropic starts answering ineligible_reason "cli_version",
	// set it to the current Claude Code version in the daemon's environment
	// and restart the daemon (PATCHES §46).
	ClaudeCodeVersionEnv = "SUBROUTER_CLAUDE_CODE_VERSION"
	// ProfileScope is the OAuth scope the usage endpoint requires. A setup
	// token carries user:inference alone and is refused there.
	ProfileScope = "user:profile"
	// ResetUnreadableReason marks a row whose token cannot read the reset.
	ResetUnreadableReason = "no_profile_scope"
	// ResetUnavailableReason is the ineligible_reason Anthropic sends when it
	// could not read the reset itself. Claude Code treats it as a failed
	// read, not as a verdict on the account, and so does subrouter.
	ResetUnavailableReason = "unavailable"

	// resetStatusPath is Claude Code's own reset read ("cedar_ember" mode).
	resetStatusPath  = "/api/oauth/usage?cedar_ember=1&skip_spend=1"
	oauthProfilePath = "/api/oauth/profile"
	// resetClaimTimeout is Claude Code's claim timeout.
	resetClaimTimeout  = 25 * time.Second
	resetReadTimeout   = 10 * time.Second
	resetResponseLimit = 1 << 20
)

var (
	// ResetGrantIDPattern and ResetRequestIDPattern are Claude Code's own
	// guards; it refuses to send a claim that fails either.
	ResetGrantIDPattern    = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
	ResetRequestIDPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	organizationUUIDShape  = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)
	claudeCodeVersionShape = regexp.MustCompile(`^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,6}$`)

	// ResetClientUserAgent is Claude Code's User-Agent for
	// DefaultClaudeCodeVersion, or for ClaudeCodeVersionEnv when that holds a
	// version. Anthropic grades reset eligibility by surface and CLI version
	// (ineligible_reason "surface", "cli_version"), and the status read that
	// proved every pool account eligible sent this one.
	ResetClientUserAgent = claudeCodeUserAgent(os.Getenv(ClaudeCodeVersionEnv))

	// resetAPIBase is where the reset reads and the claim go. A var only so a
	// test can point it at a fake; nothing in production changes it.
	resetAPIBase = "https://api.anthropic.com"
)

// claudeCodeUserAgent is Claude Code's User-Agent for version, or for
// DefaultClaudeCodeVersion when version is not a plain x.y.z.
func claudeCodeUserAgent(version string) string {
	version = strings.TrimSpace(version)
	if !claudeCodeVersionShape.MatchString(version) {
		version = DefaultClaudeCodeVersion
	}
	return "claude-cli/" + version + " (external, cli)"
}

// errNoResetClient refuses a reset call that was handed no HTTP client. The
// callers always pass the daemon's (or a test's fake); a silent fallback to
// a fresh client would let a test that forgot one reach Anthropic for real.
var errNoResetClient = errors.New("no HTTP client for the reset call")

// ResetGrant is one grant in the cedar_ember block.
type ResetGrant struct {
	ID               string   `json:"id"`
	Label            string   `json:"label,omitempty"`
	ResetsTotal      int      `json:"resets_total"`
	ResetsLeft       int      `json:"resets_left"`
	StartsAt         string   `json:"starts_at,omitempty"`
	EndsAt           string   `json:"ends_at,omitempty"`
	Clears           []string `json:"clears,omitempty"`
	Paused           bool     `json:"paused"`
	UsableNow        bool     `json:"usable_now"`
	UseRequiresLimit bool     `json:"use_requires_limit"`
	Blocking         []string `json:"blocking,omitempty"`
	// PercentUsed is how much of each limit the grant clears is used, as
	// Claude Code reads it (whole percents 0-100, known limits only).
	PercentUsed map[string]int `json:"percent_used,omitempty"`
	// Type is "session", "weekly" or "full"; anything else reads as "".
	Type string `json:"type,omitempty"`
	// RawType is the grant's type as Anthropic wrote it, any value kept, so
	// a grant of a type Claude Code does not know is listed with it
	// (PATCHES §91.1). Never encoded: every block and answer stays as it was.
	RawType string `json:"-"`
	// GivesBackWeekly is a grant whose claim also hands back to the general
	// week what the limits it clears had spent of it: Claude Code calls such
	// a grant a Fable reset.
	GivesBackWeekly bool `json:"gives_back_weekly,omitempty"`
	// UnavailableReason is why the grant cannot be used now, one of
	// resetUnavailableReasons; anything else reads as "".
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

// ClearsExhausted reports whether any limit the grant clears is one the
// status reads as exhausted. When none is, claiming spends the reset early:
// Claude Code then asks "use your reset anyway?".
func (g ResetGrant) ClearsExhausted(status *ResetStatus) bool {
	if status == nil {
		return false
	}
	for _, clear := range g.Clears {
		for _, exhausted := range status.Exhausted {
			if clear == exhausted {
				return true
			}
		}
	}
	return false
}

// ResetKind is what claiming a grant gives back, read from its clears list
// (PATCHES §64) by the reset kinds table (accounts.ClassifyReset, §91.1).
type ResetKind string

const (
	// ResetKindWeek refills the general weekly limit ("seven_day").
	ResetKindWeek ResetKind = ResetKind(accounts.ResetKindClaudeWeek)
	// ResetKindFable clears the Fable limit ("seven_day_overage_included")
	// and not the week: the claim zeroes the Fable meter and gives back only
	// the part of the week that Fable usage spent.
	ResetKindFable ResetKind = ResetKind(accounts.ResetKindClaudeFable)
	// ResetKindOther is a grant the table does not know.
	ResetKindOther ResetKind = "other"
)

// Kind is what claiming the grant gives back.
func (g ResetGrant) Kind() ResetKind {
	if spec, ok := accounts.ClassifyReset(g.reading()); ok {
		return ResetKind(spec.Kind)
	}
	return ResetKindOther
}

// reading is the grant as the kinds table reads it. A grant decoded from an
// answer's JSON has no RawType, so its Type stands in.
func (g ResetGrant) reading() accounts.ResetReading {
	return accounts.ResetReading{Provider: accounts.ProviderClaude, Source: ResetProgram, Type: cmp.Or(g.RawType, g.Type), Clears: g.Clears}
}

// held is what the grant leaves the account holding, for the row's resets.
func (g ResetGrant) held() accounts.HeldResetReading {
	expiry := accounts.ResetExpiryRead
	if strings.TrimSpace(g.EndsAt) == "" {
		expiry = accounts.ResetExpiryNoneGiven
	}
	needsLimit := g.UseRequiresLimit
	return accounts.HeldResetReading{
		ResetReading: g.reading(),
		Count:        g.ResetsLeft,
		ExpiresAt:    g.EndsAt,
		Expiry:       expiry,
		NeedsLimit:   &needsLimit,
	}
}

// Expired reports whether the grant's use-by time has passed. A grant with
// no readable ends_at never expires here; Anthropic decides.
func (g ResetGrant) Expired(now time.Time) bool {
	ends, ok := parseResetTime(g.EndsAt)
	return ok && !now.Before(ends)
}

// ResetStatus is the cedar_ember block, decoded the way Claude Code decodes
// it: a block without a boolean eligible is malformed as a whole, a grant
// without a valid id or a whole resets_left is dropped, and every other field
// falls back to Claude Code's default. ineligible_reason is kept verbatim.
type ResetStatus struct {
	Eligible         bool         `json:"eligible"`
	IneligibleReason string       `json:"ineligible_reason,omitempty"`
	AtLimit          bool         `json:"at_limit"`
	Exhausted        []string     `json:"exhausted,omitempty"`
	Grants           []ResetGrant `json:"grants,omitempty"`
	NextGrantID      string       `json:"next_grant_id,omitempty"`
	WeeklyResetsAt   string       `json:"weekly_resets_at,omitempty"`
	CooldownUntil    string       `json:"cooldown_until,omitempty"`
}

// ErrMalformedResetStatus is a cedar_ember block Claude Code would ignore.
var ErrMalformedResetStatus = errors.New("malformed cedar_ember block")

// ResetStatus decodes the response's cedar_ember block. It returns nil, nil
// when the block is absent or null (the plain endpoint, or a server that did
// not answer the opt-in), which must read as "not known", never as "none".
func (u *UsageResponse) ResetStatus() (*ResetStatus, error) {
	if u == nil {
		return nil, nil
	}
	return ParseResetStatus(u.CedarEmber)
}

// ParseResetStatus decodes a raw cedar_ember value. See ResetStatus.
func ParseResetStatus(raw json.RawMessage) (*ResetStatus, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var object map[string]any
	if err := json.Unmarshal(trimmed, &object); err != nil || object == nil {
		return nil, ErrMalformedResetStatus
	}
	eligible, ok := object["eligible"].(bool)
	if !ok {
		return nil, ErrMalformedResetStatus
	}
	status := &ResetStatus{Eligible: eligible}
	status.IneligibleReason = resetString(object["ineligible_reason"])
	status.AtLimit, _ = object["at_limit"].(bool)
	status.Exhausted = resetStrings(object["exhausted"])
	if grants, ok := object["grants"].([]any); ok {
		for _, value := range grants {
			if grant, ok := parseResetGrant(value); ok {
				status.Grants = append(status.Grants, grant)
			}
		}
	}
	if next := resetString(object["next_grant_id"]); ResetGrantIDPattern.MatchString(next) {
		for _, grant := range status.Grants {
			if grant.ID == next {
				status.NextGrantID = next
				break
			}
		}
	}
	status.WeeklyResetsAt = resetString(object["weekly_resets_at"])
	status.CooldownUntil = resetString(object["cooldown_until"])
	return status, nil
}

func parseResetGrant(value any) (ResetGrant, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return ResetGrant{}, false
	}
	id := resetString(object["id"])
	if !ResetGrantIDPattern.MatchString(id) {
		return ResetGrant{}, false
	}
	left, ok := resetWhole(object["resets_left"])
	if !ok {
		return ResetGrant{}, false
	}
	grant := ResetGrant{
		ID:               id,
		Label:            resetString(object["label"]),
		ResetsLeft:       left,
		StartsAt:         resetString(object["starts_at"]),
		EndsAt:           resetString(object["ends_at"]),
		Clears:           resetStrings(object["clears"]),
		Blocking:         resetStrings(object["blocking"]),
		UseRequiresLimit: true,
	}
	if total, ok := resetWhole(object["resets_total"]); ok {
		grant.ResetsTotal = total
	}
	grant.Paused, _ = object["paused"].(bool)
	grant.UsableNow, _ = object["usable_now"].(bool)
	grant.GivesBackWeekly, _ = object["gives_back_weekly"].(bool)
	grant.RawType = resetString(object["type"])
	if resetGrantTypes[grant.RawType] {
		grant.Type = grant.RawType
	}
	if reason := resetString(object["unavailable_reason"]); resetUnavailableReasons[reason] {
		grant.UnavailableReason = reason
	}
	if requires, ok := object["use_requires_limit"].(bool); ok {
		grant.UseRequiresLimit = requires
	}
	if used, ok := object["percent_used"].(map[string]any); ok {
		for limit, value := range used {
			if percent, ok := resetWhole(value); ok && percent <= 100 && resetLimitNames[limit] {
				if grant.PercentUsed == nil {
					grant.PercentUsed = map[string]int{}
				}
				grant.PercentUsed[limit] = percent
			}
		}
	}
	return grant, true
}

// resetGrantTypes and resetUnavailableReasons are the values Claude Code
// 2.1.321 accepts for a grant's type and unavailable_reason; it reads any
// other value as absent, and so does this decoder.
var resetGrantTypes = map[string]bool{"session": true, "weekly": true, "full": true}

var resetUnavailableReasons = map[string]bool{
	"cooldown": true, "blocked_five_hour": true, "blocked_seven_day": true, "blocked_opus": true,
	"blocked_sonnet": true, "blocked_other": true, "cannot_clear": true, "needs_limit": true, "unavailable": true,
}

// resetLimitNames are the limits Claude Code knows a grant can clear.
var resetLimitNames = map[string]bool{
	"five_hour": true, "seven_day": true, "seven_day_overage_included": true, "seven_day_opus": true,
	"seven_day_sonnet": true, "seven_day_cowork": true, "seven_day_omelette": true, "seven_day_oauth_apps": true,
}

func resetString(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func resetStrings(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range items {
		if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
			out = append(out, strings.TrimSpace(text))
		}
	}
	return out
}

func resetWhole(value any) (int, bool) {
	number, ok := value.(float64)
	if !ok || number < 0 || number != float64(int(number)) || number > 1<<20 {
		return 0, false
	}
	return int(number), true
}

func parseResetTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999999Z0700"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

// ParseResetTime reads a cedar_ember timestamp (ends_at, cooldown_until,
// weekly_resets_at).
func ParseResetTime(value string) (time.Time, bool) {
	return parseResetTime(value)
}

// CredentialCannotReadResets reports a credential whose token Anthropic will
// not let read the reset: a scope list that lacks user:profile, as a setup
// token's user:inference alone does. A credential with NO scope list is not
// such a token -- many stored OAuth logins simply never recorded one.
func CredentialCannotReadResets(credential *CredentialInfo) bool {
	if credential == nil || len(credential.Scopes) == 0 {
		return false
	}
	for _, scope := range credential.Scopes {
		if strings.TrimSpace(scope) == ProfileScope {
			return false
		}
	}
	return true
}

// ResetHTTPError is a non-2xx answer from one of the reset calls. Detail is
// Anthropic's error type and message, bounded (resetErrorDetail); the rest of
// the body is dropped.
type ResetHTTPError struct {
	Op         string
	StatusCode int
	Status     string
	Detail     string
	// RetryAfter is Anthropic's Retry-After on a 429, 0 when it sent none.
	RetryAfter time.Duration
}

func (e *ResetHTTPError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("%s failed: %s: %s", e.Op, e.Status, e.Detail)
	}
	return fmt.Sprintf("%s failed: %s", e.Op, e.Status)
}

// resetErrorDetailLimit bounds the error detail kept from a non-2xx body.
const resetErrorDetailLimit = 200

// resetErrorDetail is the error type and message of an Anthropic error body
// ({"type":"error","error":{"type":...,"message":...}}, or {"error":"..."}),
// bounded to resetErrorDetailLimit. It never carries the request's token:
// Anthropic's error bodies name the problem, not the credential. "" when the
// body is not one of those shapes.
func resetErrorDetail(raw []byte) string {
	var body struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(bytes.TrimSpace(raw), &body) != nil {
		return ""
	}
	var detail string
	var nested struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	var flat string
	switch {
	case json.Unmarshal(body.Error, &nested) == nil && (nested.Type != "" || nested.Message != ""):
		detail = strings.TrimSpace(strings.Trim(nested.Type+": "+nested.Message, ": "))
	case json.Unmarshal(body.Error, &flat) == nil && flat != "":
		detail = strings.TrimSpace(flat)
	case body.Message != "":
		detail = strings.TrimSpace(body.Message)
	}
	detail = strings.Join(strings.Fields(detail), " ")
	if len(detail) > resetErrorDetailLimit {
		detail = detail[:resetErrorDetailLimit] + "..."
	}
	return detail
}

func newResetHTTPError(op string, res *http.Response) *ResetHTTPError {
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	out := &ResetHTTPError{Op: op, StatusCode: res.StatusCode, Status: res.Status, Detail: resetErrorDetail(raw)}
	if res.StatusCode == http.StatusTooManyRequests {
		out.RetryAfter = ParseRetryAfter(res.Header.Get("Retry-After"), time.Now())
	}
	return out
}

// Unauthorized reports a 401 or 403.
func (e *ResetHTTPError) Unauthorized() bool {
	return e != nil && (e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden)
}

// RateLimited reports a 429. Anthropic's usage endpoint limits reads per
// login (seen 2026-09-29: two reads a few seconds apart, the second 429
// "rate_limit_error: Rate limited. Please try again later").
func (e *ResetHTTPError) RateLimited() bool {
	return e != nil && e.StatusCode == http.StatusTooManyRequests
}

// retryAfterLimit caps a Retry-After this package reports: an absurd one
// is not a wait anyone should be told to sit through.
const retryAfterLimit = time.Hour

// ParseRetryAfter reads a Retry-After header: delta-seconds or an HTTP date.
// 0 when absent, unreadable, or already past.
func ParseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	var wait time.Duration
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds > int64(retryAfterLimit/time.Second) {
			return retryAfterLimit
		}
		wait = time.Duration(seconds) * time.Second
	} else if at, err := http.ParseTime(value); err == nil {
		wait = at.Sub(now)
	}
	if wait <= 0 {
		return 0
	}
	if wait > retryAfterLimit {
		return retryAfterLimit
	}
	return wait
}

func newResetRequest(ctx context.Context, method, path, accessToken string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, resetAPIBase+path, body)
	if err != nil {
		return nil, err
	}
	// Claude Code's first-party API client: its User-Agent, the OAuth bearer
	// with the OAuth beta, and the call's own Content-Type (axios adds the
	// Accept below).
	req.Header.Set("User-Agent", ResetClientUserAgent)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("anthropic-beta", oauthBetaHeader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	return req, nil
}

// FetchResetStatus is Claude Code's reset read: GET
// /api/oauth/usage?cedar_ember=1&skip_spend=1. It returns the whole usage
// response (for the 5h and 7d meters) and its decoded block. A 401 or 403 is
// returned as *ResetHTTPError and is never refreshed and retried here.
func FetchResetStatus(ctx context.Context, client *http.Client, accessToken string) (*UsageResponse, *ResetStatus, error) {
	if strings.TrimSpace(accessToken) == "" {
		return nil, nil, errors.New("no access token")
	}
	if client == nil {
		return nil, nil, errNoResetClient
	}
	ctx, cancel := context.WithTimeout(ctx, resetReadTimeout)
	defer cancel()
	req, err := newResetRequest(ctx, http.MethodGet, resetStatusPath, accessToken, nil)
	if err != nil {
		return nil, nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, nil, newResetHTTPError("reset status read", res)
	}
	var usage UsageResponse
	if err := json.NewDecoder(io.LimitReader(res.Body, resetResponseLimit)).Decode(&usage); err != nil {
		return nil, nil, fmt.Errorf("reset status read: %w", err)
	}
	status, err := usage.ResetStatus()
	if err != nil {
		return &usage, nil, err
	}
	return &usage, status, nil
}

// ResetProfile is the identity a claim is addressed to and checked against,
// from GET /api/oauth/profile: the organization Claude Code takes its
// organizationUuid from, and the account that token belongs to.
type ResetProfile struct {
	OrganizationUUID string `json:"organization_uuid"`
	AccountUUID      string `json:"account_uuid,omitempty"`
	Email            string `json:"email,omitempty"`
}

// FetchResetProfile reads the token's account and organization from GET
// /api/oauth/profile. Read-only. It is read on every preview and every claim,
// never cached: a profile logged in again as another account must not be
// claimed against the organization of the login before.
func FetchResetProfile(ctx context.Context, client *http.Client, accessToken string) (ResetProfile, error) {
	if client == nil {
		return ResetProfile{}, errNoResetClient
	}
	ctx, cancel := context.WithTimeout(ctx, resetReadTimeout)
	defer cancel()
	req, err := newResetRequest(ctx, http.MethodGet, oauthProfilePath, accessToken, nil)
	if err != nil {
		return ResetProfile{}, err
	}
	req.Header.Del("anthropic-beta")
	req.Header.Set("Cache-Control", "no-cache")
	res, err := client.Do(req)
	if err != nil {
		return ResetProfile{}, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return ResetProfile{}, newResetHTTPError("organization read", res)
	}
	var profile struct {
		Account struct {
			UUID         string `json:"uuid"`
			EmailAddress string `json:"email_address"`
			Email        string `json:"email"`
		} `json:"account"`
		Organization struct {
			UUID string `json:"uuid"`
		} `json:"organization"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, resetResponseLimit)).Decode(&profile); err != nil {
		return ResetProfile{}, fmt.Errorf("organization read: %w", err)
	}
	out := ResetProfile{
		OrganizationUUID: strings.TrimSpace(profile.Organization.UUID),
		AccountUUID:      strings.TrimSpace(profile.Account.UUID),
		Email:            strings.TrimSpace(profile.Account.EmailAddress),
	}
	if out.Email == "" {
		out.Email = strings.TrimSpace(profile.Account.Email)
	}
	if !organizationUUIDShape.MatchString(out.OrganizationUUID) {
		return ResetProfile{}, errors.New("organization read: the profile names no organization")
	}
	return out, nil
}

// FetchOrganizationUUID is FetchResetProfile's organization alone.
func FetchOrganizationUUID(ctx context.Context, client *http.Client, accessToken string) (string, error) {
	profile, err := FetchResetProfile(ctx, client, accessToken)
	return profile.OrganizationUUID, err
}

// Reset claim results. The first six are Anthropic's enum; the last three
// are Claude Code's own names for a claim that got no such answer.
const (
	ResetResultReset       = "reset"
	ResetResultAlreadyUsed = "already_used"
	ResetResultNotLimited  = "not_limited"
	ResetResultCooldown    = "cooldown"
	ResetResultIneligible  = "ineligible"
	ResetResultUnavailable = "unavailable"
	ResetResultAuthError   = "auth_error"
	ResetResultRateLimited = "rate_limited"
	ResetResultError       = "error"
)

var resetAnswerResults = map[string]bool{
	ResetResultReset: true, ResetResultAlreadyUsed: true, ResetResultNotLimited: true,
	ResetResultCooldown: true, ResetResultIneligible: true, ResetResultUnavailable: true,
}

// ResetClaim is what one claim came back with.
type ResetClaim struct {
	Result string `json:"result"`
	// AnsweredResult is the result string Anthropic sent when it is outside
	// the enum (Result then reads unavailable, as Claude Code reads it).
	AnsweredResult string   `json:"answered_result,omitempty"`
	Reason         string   `json:"reason,omitempty"`
	ResetsLeft     *int     `json:"resets_left,omitempty"`
	Cleared        []string `json:"cleared,omitempty"`
	WeeklyResetsAt string   `json:"weekly_resets_at,omitempty"`
	CooldownUntil  string   `json:"cooldown_until,omitempty"`
	// HTTPStatus is set when Anthropic answered with a non-2xx status.
	HTTPStatus int `json:"http_status,omitempty"`
	// RetryAfter is Anthropic's Retry-After on a 429 (rate_limited), 0 when
	// it sent none. Not part of the answer's JSON.
	RetryAfter time.Duration `json:"-"`
}

// Settled reports an answer that says what happened to the grant: reset,
// already_used, not_limited or ineligible. Only a settled claim lets its
// request_id go; Claude Code drops its unsettled claim on exactly these.
func (c ResetClaim) Settled() bool {
	switch c.Result {
	case ResetResultReset, ResetResultAlreadyUsed, ResetResultNotLimited, ResetResultIneligible:
		return true
	}
	return false
}

// MayHaveLanded reports a claim Anthropic could not confirm either way:
// unavailable (including a result outside the enum, and the reasons
// stamp_indeterminate and reset_unconfirmed) and error (no answer, an
// unreadable one, a non-2xx other than 401/403/429). Claude Code keeps the
// request_id of such a claim for its retry, and so does the daemon.
//
// The rest -- auth_error, rate_limited and cooldown -- neither settle nor
// land: the grant was not touched by this request, but an EARLIER claim that
// may have landed keeps its request_id through them.
func (c ResetClaim) MayHaveLanded() bool {
	return c.Result == ResetResultUnavailable || c.Result == ResetResultError
}

type resetClaimBody struct {
	Program   string `json:"program"`
	GrantID   string `json:"grant_id"`
	RequestID string `json:"request_id"`
}

// ClaimReset sends ONE reset claim and reports what came back. It never
// refreshes the token and never retries: a 401/403 is ResetResultAuthError, a
// 429 ResetResultRateLimited (with Anthropic's Retry-After), anything else
// unanswered ResetResultError with the error. The caller logs requestID
// before calling, and reuses it on a retry so Anthropic can recognise the
// same claim.
func ClaimReset(ctx context.Context, client *http.Client, accessToken, organizationUUID, grantID, requestID string) (ResetClaim, error) {
	if !organizationUUIDShape.MatchString(organizationUUID) {
		return ResetClaim{Result: ResetResultError}, errors.New("refusing to claim: no valid organization id")
	}
	if !ResetGrantIDPattern.MatchString(grantID) || !ResetRequestIDPattern.MatchString(requestID) {
		return ResetClaim{Result: ResetResultError}, errors.New("refusing to claim with a malformed grant or request id")
	}
	if strings.TrimSpace(accessToken) == "" {
		return ResetClaim{Result: ResetResultError}, errors.New("refusing to claim: no access token")
	}
	if client == nil {
		return ResetClaim{Result: ResetResultError}, errNoResetClient
	}
	body, err := json.Marshal(resetClaimBody{Program: ResetProgram, GrantID: grantID, RequestID: requestID})
	if err != nil {
		return ResetClaim{Result: ResetResultError}, err
	}
	ctx, cancel := context.WithTimeout(ctx, resetClaimTimeout)
	defer cancel()
	req, err := newResetRequest(ctx, http.MethodPost, "/api/organizations/"+organizationUUID+"/reset_rate_limits", accessToken, bytes.NewReader(body))
	if err != nil {
		return ResetClaim{Result: ResetResultError}, err
	}
	res, err := client.Do(req)
	if err != nil {
		return ResetClaim{Result: ResetResultError}, fmt.Errorf("claim not answered: %w", err)
	}
	defer res.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(res.Body, resetResponseLimit))
	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return ResetClaim{Result: ResetResultAuthError, HTTPStatus: res.StatusCode}, nil
	case res.StatusCode == http.StatusTooManyRequests:
		return ResetClaim{Result: ResetResultRateLimited, HTTPStatus: res.StatusCode,
			RetryAfter: ParseRetryAfter(res.Header.Get("Retry-After"), time.Now())}, nil
	case res.StatusCode >= 400 && res.StatusCode < 500:
		// Anthropic read the request and turned it down. Claude Code still
		// files it under error (and keeps its request_id); the words differ.
		message := fmt.Sprintf("Anthropic rejected the claim request: %s", res.Status)
		if detail := resetErrorDetail(raw); detail != "" {
			message += ": " + detail
		}
		return ResetClaim{Result: ResetResultError, HTTPStatus: res.StatusCode}, errors.New(message)
	case res.StatusCode < 200 || res.StatusCode >= 300:
		message := fmt.Sprintf("claim failed: %s", res.Status)
		if detail := resetErrorDetail(raw); detail != "" {
			message += ": " + detail
		}
		return ResetClaim{Result: ResetResultError, HTTPStatus: res.StatusCode}, errors.New(message)
	}
	if readErr != nil {
		return ResetClaim{Result: ResetResultError}, fmt.Errorf("unreadable claim response: %w", readErr)
	}
	claim, err := parseResetClaim(raw)
	if err != nil {
		return ResetClaim{Result: ResetResultError}, err
	}
	return claim, nil
}

func parseResetClaim(raw []byte) (ResetClaim, error) {
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return ResetClaim{}, errors.New("unreadable claim response")
	}
	claim := ResetClaim{Result: resetString(object["result"])}
	// Claude Code reads a result outside the enum as unavailable. What was
	// actually answered is kept for the operator.
	if !resetAnswerResults[claim.Result] {
		claim.AnsweredResult = claim.Result
		if claim.AnsweredResult == "" {
			claim.AnsweredResult = "(none)"
		}
		claim.Result = ResetResultUnavailable
	}
	claim.Reason = resetString(object["reason"])
	if left, ok := resetWhole(object["resets_left"]); ok {
		claim.ResetsLeft = &left
	}
	claim.Cleared = resetStrings(object["cleared"])
	claim.WeeklyResetsAt = resetString(object["weekly_resets_at"])
	claim.CooldownUntil = resetString(object["cooldown_until"])
	return claim, nil
}

// ResetInfo maps a decoded block onto the usage-status row's
// complimentary_reset (Source "cedar_ember"). Remaining and Total count every
// grant that has not expired; ExpiresAt and Clears are the grant with a reset
// left that expires first. RemainingByKind splits Remaining by what each
// grant's claim gives back. WeekHeldUntil is read over every grant, not the
// one that expires first: which grant expires first says nothing about
// whether another clears the week. Held lists every grant with a reset left
// by kind and expiry (PATCHES §91.1). Nil in, nil out: a block that was not
// read is not a block that says "none".
func ResetInfo(status *ResetStatus, now time.Time) *accounts.ComplimentaryResetInfo {
	if status == nil {
		return nil
	}
	if ResetStatusUnread(status) {
		// Claude Code reads this as a failed read, not as ineligible.
		return &accounts.ComplimentaryResetInfo{
			Source:           ResetProgram,
			Status:           ResetUnavailableReason,
			IneligibleReason: ResetUnavailableReason,
		}
	}
	eligible := status.Eligible
	info := &accounts.ComplimentaryResetInfo{
		Known:            true,
		Source:           ResetProgram,
		Eligible:         &eligible,
		IneligibleReason: status.IneligibleReason,
	}
	remaining, total, expiredLeft := 0, 0, 0
	byKind := map[string]int{}
	var held []accounts.HeldResetReading
	var soonest time.Time
	var chosen *ResetGrant
	for i := range status.Grants {
		grant := status.Grants[i]
		grantTotal := grant.ResetsTotal
		if grantTotal < grant.ResetsLeft {
			grantTotal = grant.ResetsLeft
		}
		total += grantTotal
		if grant.Expired(now) {
			expiredLeft += grant.ResetsLeft
			continue
		}
		remaining += grant.ResetsLeft
		if grant.ResetsLeft <= 0 {
			continue
		}
		byKind[string(grant.Kind())] += grant.ResetsLeft
		held = append(held, grant.held())
		ends, ok := parseResetTime(grant.EndsAt)
		if chosen == nil || (ok && (soonest.IsZero() || ends.Before(soonest))) {
			chosen = &status.Grants[i]
			if ok {
				soonest = ends
			}
		}
	}
	if len(status.Grants) > 0 {
		used := total - remaining
		if used < 0 {
			used = 0
		}
		info.Remaining = &remaining
		info.Total = &total
		info.Used = &used
		info.Consumed = total > 0 && remaining == 0
		if len(byKind) > 0 {
			info.RemainingByKind = byKind
		}
	}
	info.Available = eligible && remaining > 0
	if remaining == 0 && expiredLeft > 0 {
		// Left unused past its use-by date: not the same as spent.
		info.Status = "expired"
	}
	if chosen != nil {
		info.ExpiresAt = chosen.EndsAt
		info.Clears = append([]string(nil), chosen.Clears...)
	}
	if eligible {
		info.WeekHeldUntil = weekHeldUntil(status.Grants, now)
	}
	info.Held = accounts.HeldResets(held, now)
	return info
}

// weekHeldUntil is the soonest ends_at, verbatim, among the grants that clear
// the week and can still be claimed: a reset left, not expired, not paused.
// An ends_at that does not parse holds nothing: there is no expiry to hold to.
func weekHeldUntil(grants []ResetGrant, now time.Time) string {
	held, heldEnds := "", time.Time{}
	for _, grant := range grants {
		if grant.ResetsLeft <= 0 || grant.Expired(now) || grant.Paused || grant.Kind() != ResetKindWeek {
			continue
		}
		ends, ok := parseResetTime(grant.EndsAt)
		if ok && (held == "" || ends.Before(heldEnds)) {
			held, heldEnds = grant.EndsAt, ends
		}
	}
	return held
}

// ResetStatusUnread reports a block that is Anthropic's "could not read it
// right now" (ineligible_reason "unavailable"): not known, never ineligible.
func ResetStatusUnread(status *ResetStatus) bool {
	return status != nil && !status.Eligible && status.IneligibleReason == ResetUnavailableReason
}

// UnreadableResetInfo is the row for a token that cannot read the block (a
// setup token): known false, so no reader takes it for "no reset".
func UnreadableResetInfo() *accounts.ComplimentaryResetInfo {
	return &accounts.ComplimentaryResetInfo{
		Source:           ResetProgram,
		Status:           "unreadable",
		IneligibleReason: ResetUnreadableReason,
	}
}

// NewResetRequestID mints a claim's request_id the way Claude Code does (a
// random UUID). It fails rather than fall back to anything guessable: two
// claims must never share one by accident.
func NewResetRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint reset request id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
