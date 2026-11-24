package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
)

// A pinned account (the caller's own X-Subrouter-Account-ID: `sr claude proxy
// --account`, SUBROUTER_CODEX_ACCOUNT_ID, `sr kimi --account`) whose credential
// is dead must fail ONCE with a final, readable error. The pin disarms every
// failover stage, so before this the request fell through to the no-account
// 503 -- which Claude Code retried ten times, each retry re-running the same
// doomed token refresh, and the user saw a bare refresh error at the end.
// Ported from upstream #429 (19bb33b), PATCHES section 28.

// pinnedDeadCredentialServer is claudeFailoverServer with a refresh that fails
// for the pinned account only, and a count of every refresh the handler ran.
func pinnedDeadCredentialServer(t *testing.T, refreshErr error) (Server, *[]string) {
	t.Helper()
	server, _ := claudeFailoverServer(t)
	refreshed := []string{}
	server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		refreshed = append(refreshed, account.ID)
		if account.ID == "cooked@example.com" {
			return account, refreshErr
		}
		return account, nil
	}
	return server, &refreshed
}

func pinnedClaudeRequest(accountID string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5"}`))
	req.RemoteAddr = "127.0.0.1:5000"
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Claude-Code-Session-Id", "pinned-session")
	if accountID != "" {
		req.Header.Set("X-Subrouter-Account-ID", accountID)
	}
	return req
}

func TestPinnedClaudeAccountWithDeadCredentialFailsFast(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refresh string
		want    string
		// hint is the repair command the message must name, with the pinned
		// account's own name (PATCHES §36): an OAuth login is signed in again
		// with `addr`, and only an expired setup token is re-minted with `add`.
		hint string
	}{
		{name: "invalid_grant", refresh: `Claude OAuth refresh failed: 400 Bad Request: {"error": "invalid_grant", "error_description": "Refresh token not found or invalid"}`, want: "needs re-login",
			hint: "re-auth it with 'sr claude addr cooked@example.com'"},
		{name: "setup token expired", refresh: `Claude profile "cooked@example.com" has no usable credential: setup token expired 2026-09-01; re-add with 'sr claude add cooked@example.com'`, want: "setup token has expired",
			hint: "re-add it with 'sr claude add cooked@example.com'"},
		// Not on isTerminalCredentialError's list: an unpinned request does not
		// treat it as a credential verdict (see PATCHES section 28). For a pin it
		// is final all the same -- no retry lifts Anthropic's hold.
		{name: "account_on_hold", refresh: `Claude OAuth refresh failed: 403 Forbidden: {"error": {"type": "account_on_hold"}}`, want: "restricted by Anthropic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, refreshed := pinnedDeadCredentialServer(t, errors.New(tc.refresh))
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, pinnedClaudeRequest("cooked@example.com"))

			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want a final 403; body = %s", response.Code, response.Body.String())
			}
			if got := response.Header().Get("X-Should-Retry"); got != "false" {
				t.Fatalf("X-Should-Retry = %q, want false: without it Claude Code retries the dead refresh", got)
			}
			var body struct {
				Type  string `json:"type"`
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not Anthropic-shaped JSON: %v: %s", err, response.Body.String())
			}
			if body.Type != "error" || body.Error.Type != "permission_error" ||
				!strings.Contains(body.Error.Message, tc.want) ||
				!strings.Contains(body.Error.Message, "cooked@example.com") ||
				!strings.Contains(body.Error.Message, "sr claude proxy --account") {
				t.Fatalf("body = %+v", body)
			}
			// `sr add claude` is the setup-token verb with no name: it would turn
			// an OAuth account into a setup token, and refuses without a name.
			if !strings.Contains(body.Error.Message, tc.hint) || strings.Contains(body.Error.Message, "sr add claude") {
				t.Fatalf("message %q does not name the repair %q", body.Error.Message, tc.hint)
			}
			// The pin still holds: the dead account's refresh ran once and no
			// other account was tried in its place.
			if len(*refreshed) != 1 || (*refreshed)[0] != "cooked@example.com" {
				t.Fatalf("refreshes = %v, want exactly one, on the pinned account", *refreshed)
			}
		})
	}
}

// A pin that reached this daemon from another machine -- `sr claude proxy
// --account` with a server selected launches straight at the server -- is read
// there, and `sr claude addr <name>` run there signs in a NEW local profile and
// leaves this daemon's copy, and the pin, dead (PATCHES §36). So the hint names
// this machine. Over loopback it does not: the reader is here.
func TestPinnedClaudeHintNamesTheServerForARemoteCaller(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		t.Skipf("no host name to assert on: %v", err)
	}
	for _, tc := range []struct {
		name, refresh, hint string
	}{
		{name: "OAuth", refresh: `Claude OAuth refresh failed: 400 Bad Request: {"error": "invalid_grant"}`,
			hint: "re-auth it on the server that holds it (host " + strings.TrimSpace(host) + ") with 'sr claude addr cooked@example.com'"},
		{name: "setup token", refresh: `Claude profile "cooked@example.com" has no usable credential: setup token expired 2026-09-01`,
			hint: "re-add it on the server that holds it (host " + strings.TrimSpace(host) + ") with 'sr claude add cooked@example.com'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := pinnedDeadCredentialServer(t, errors.New(tc.refresh))
			req := pinnedClaudeRequest("cooked@example.com")
			req.RemoteAddr = "100.64.0.7:5000" // a tailnet peer: the laptop
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, req)
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want the final 403; body = %s", response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), tc.hint) {
				t.Fatalf("a remote caller's hint does not name this machine %q:\n%s", tc.hint, response.Body.String())
			}
		})
	}
}

// Our CLI has no `sr codex proxy`: a Codex pin comes from
// SUBROUTER_CODEX_ACCOUNT_ID. Upstream's hint named the command that does not
// exist here, so the message would send the user nowhere.
func TestPinnedCodexAccountWithDeadCredentialFailsFastWithAHintThatExists(t *testing.T) {
	server := Server{
		Accounts: []accounts.Account{
			{ID: "dead@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "dead-token"},
			{ID: "live@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "live-token"},
		},
		Sessions:     newSessionStore(t),
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		MaxBodyBytes: 1 << 20,
	}
	refreshed := []string{}
	server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		refreshed = append(refreshed, account.ID)
		if account.ID == "dead@example.com" {
			return account, &accounts.CodexAuthRefreshError{StatusCode: http.StatusUnauthorized,
				Body: `{"error":{"code":"refresh_token_expired","message":"Your refresh token has expired."}}`}
		}
		return account, nil
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.4","input":"hi"}`))
	req.RemoteAddr = "127.0.0.1:5000"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "codex")
	req.Header.Set("X-Subrouter-Session", "pinned-codex-session")
	req.Header.Set("X-Subrouter-Account-ID", "dead@example.com")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, req)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want a final 403; body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("X-Should-Retry"); got != "false" {
		t.Fatalf("X-Should-Retry = %q, want false", got)
	}
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not OpenAI-shaped JSON: %v: %s", err, response.Body.String())
	}
	message := body.Error.Message
	if body.Error.Code != "account_unusable" || !strings.Contains(message, "dead@example.com") ||
		!strings.Contains(message, "needs re-login") || !strings.Contains(message, "sr add codex") ||
		!strings.Contains(message, "SUBROUTER_CODEX_ACCOUNT_ID") {
		t.Fatalf("body = %+v", body)
	}
	if strings.Contains(message, "sr codex proxy") {
		t.Fatalf("hint names `sr codex proxy`, which this CLI does not have: %q", message)
	}
	if len(refreshed) != 1 || refreshed[0] != "dead@example.com" {
		t.Fatalf("refreshes = %v, want exactly one, on the pinned account", refreshed)
	}
}

// A refresh that reached NO verdict -- the token endpoint's 429/5xx, a network
// failure before it answered -- says nothing about the account, so a pin on it
// must stay the retryable 503 it was: the next attempt may well succeed.
// refreshSelectedAccount classifies transient FIRST (PATCHES section 20) and
// the 403 obeys the same order. The last case is contrived on purpose: it is
// the one shape on which the two classifiers disagree (a 5xx whose body echoes
// a terminal marker), so it is what fails if the transient guard is dropped.
func TestPinnedAccountRefreshWithoutAVerdictStaysRetryable(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "token endpoint 503", err: errors.New(`Claude OAuth refresh failed: 503 Service Unavailable: upstream connect error`)},
		{name: "token endpoint 429", err: errors.New(`Claude OAuth refresh failed: 429 Too Many Requests: {"error":{"type":"rate_limit_error"}}`)},
		{name: "network unreachable", err: &url.Error{Op: "Post", URL: "https://platform.claude.com/v1/oauth/token",
			Err: &net.OpError{Op: "dial", Net: "tcp4", Err: errors.New("connect: network is unreachable")}}},
		{name: "5xx echoing a terminal marker", err: errors.New(`Claude OAuth refresh failed: 502 Bad Gateway: {"error": "invalid_grant"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "5xx echoing a terminal marker" && !pinnedAccountNeedsReauthUnguarded(tc.err) {
				t.Fatal("calibration: this error must match the terminal list, or the case proves nothing")
			}
			server, _ := pinnedDeadCredentialServer(t, tc.err)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, pinnedClaudeRequest("cooked@example.com"))

			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want the retryable 503; body = %s", response.Code, response.Body.String())
			}
			if got := response.Header().Get("X-Should-Retry"); got != "" {
				t.Fatalf("X-Should-Retry = %q on a refresh that reached no verdict", got)
			}
		})
	}
}

// pinnedAccountNeedsReauthUnguarded is upstream's predicate verbatim, kept
// here only to calibrate the contrived case above.
func pinnedAccountNeedsReauthUnguarded(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "account_on_hold") || isTerminalCredentialError(err)
}

// A session lease installs X-Subrouter-Account-ID itself (proxyHandler), but it
// is not the caller's pin: the lease layer recovers from a dead account by
// re-leasing onto another one
// (TestIdempotentSessionLeaseReplacesLeaseAfterTerminalAccountFailure), and a
// final 403 would tell its client never to try. The gate is the caller's own
// selector, never the header's mere presence.
func TestLeasedRequestOnADeadCredentialIsNotAFinal403(t *testing.T) {
	server, _ := pinnedDeadCredentialServer(t, errors.New(`Claude OAuth refresh failed: 400 Bad Request: {"error": "invalid_grant"}`))
	server.sessionLeases = newSessionLeaseStore()
	// The lease checks the body's model against its own, which needs a body
	// limit; without one the request is refused as the wrong model (a 403 of
	// its own that would pass the first assertion below for no reason).
	server.MaxBodyBytes = 1 << 20
	lease, err := server.sessionLeases.put(sessionLease{
		ScopeKey:   "scope-pinned-lease",
		SessionKey: "leased-session",
		Agent:      "claude",
		Provider:   accounts.ProviderClaude,
		AccountID:  "cooked@example.com",
		AuthMode:   accounts.AuthModeOAuth,
		Model:      "claude-sonnet-4-5",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5"}`))
	req.RemoteAddr = "127.0.0.1:5000"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Lease", lease.Token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, req)

	if response.Code == http.StatusForbidden || response.Header().Get("X-Should-Retry") == "false" {
		t.Fatalf("a leased request got the pinned-account 403 (status %d, X-Should-Retry %q): %s",
			response.Code, response.Header().Get("X-Should-Retry"), response.Body.String())
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the retryable 503 the lease layer recovers from; body = %s", response.Code, response.Body.String())
	}
}
