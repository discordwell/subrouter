package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// A pin on an account inside its owner's holdout is answered once and
// finally (PATCHES section 89). Before, it left as the no-account 503, and a
// client set to wait hours for a daemon that is gone asked again for as long.

// heldOutUpstream is a Claude upstream that answers every request and records
// the token each one carried.
func heldOutUpstream(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var tokens []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokens = append(tokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1"}`)
	}))
	t.Cleanup(upstream.Close)
	return upstream, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), tokens...)
	}
}

func TestPinnedClaudeAccountInsideItsHoldoutIsAnsweredOnceAndFinally(t *testing.T) {
	server, _, store := holdoutFixture(t, 0.04)
	upstream, seen := heldOutUpstream(t)
	server.ClaudeUpstream = mustURL(t, upstream.URL)
	roll, _ := server.scheduler().LookupScore(accounts.ProviderClaude, "held")

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, pinnedClaudeRequest("held"))

	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want a final 429; body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("X-Should-Retry"); got != "false" {
		t.Fatalf("X-Should-Retry = %q, want false: without it a client that waits for the daemon asks again for hours", got)
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
	if body.Type != "error" || body.Error.Type != "rate_limit_error" {
		t.Fatalf("body = %+v, want an Anthropic rate_limit_error", body)
	}
	for _, want := range []string{
		`pinned account "held"`,
		"inside its owner's holdout",
		"never moved to another account",
		"Its week rolls at " + roll.WeeklyResetAt.UTC().Format("2006-01-02 15:04 UTC"),
		"sr claude proxy --account",
	} {
		if !strings.Contains(body.Error.Message, want) {
			t.Fatalf("message %q does not say %q", body.Error.Message, want)
		}
	}
	// The pin still holds: nothing was sent upstream, on this account or on
	// the one with room, and the session was given to neither.
	if tokens := seen(); len(tokens) != 0 {
		t.Fatalf("upstream saw requests with tokens %v, want none", tokens)
	}
	if assignment, ok := store.Get("claude", "pinned-session"); ok {
		t.Fatalf("the refused pin left the session assigned to %q", assignment.AccountID)
	}
}

// Calibration for the test above: the same pin, with the account above its
// holdout, is served on that account.
func TestPinnedClaudeAccountAboveItsHoldoutIsServedOnIt(t *testing.T) {
	server, _, _ := holdoutFixture(t, 0.5)
	upstream, seen := heldOutUpstream(t)
	server.ClaudeUpstream = mustURL(t, upstream.URL)

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, pinnedClaudeRequest("held"))

	if response.Code != http.StatusOK || response.Header().Get("X-Should-Retry") != "" {
		t.Fatalf("status = %d, X-Should-Retry = %q, want a served turn; body = %s",
			response.Code, response.Header().Get("X-Should-Retry"), response.Body.String())
	}
	if tokens := seen(); len(tokens) != 1 || tokens[0] != "held-token" {
		t.Fatalf("upstream saw tokens %v, want one request on the pinned account", tokens)
	}
}

// A session lease names its account in the same header, but it is not the
// caller's pin: its client recovers by leasing again, and a final answer would
// tell it never to try. As for a dead credential (PATCHES section 28), the
// gate is the caller's own selector.
func TestLeasedRequestOnAHeldOutAccountStaysRetryable(t *testing.T) {
	server, _, _ := holdoutFixture(t, 0.04)
	upstream, _ := heldOutUpstream(t)
	server.ClaudeUpstream = mustURL(t, upstream.URL)
	server.sessionLeases = newSessionLeaseStore()
	server.MaxBodyBytes = 1 << 20
	lease, err := server.sessionLeases.put(sessionLease{
		ScopeKey:   "scope-held-lease",
		SessionKey: "leased-session",
		Agent:      "claude",
		Provider:   accounts.ProviderClaude,
		AccountID:  "held",
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

	if response.Code != http.StatusServiceUnavailable || response.Header().Get("X-Should-Retry") != "" {
		t.Fatalf("status = %d, X-Should-Retry = %q, want the retryable 503 the lease layer recovers from; body = %s",
			response.Code, response.Header().Get("X-Should-Retry"), response.Body.String())
	}
}

func TestPinnedCodexAccountInsideItsHoldoutIsAnsweredOnceAndFinally(t *testing.T) {
	server := Server{
		Accounts: []accounts.Account{
			{ID: "held-codex", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "held-token"},
			{ID: "free-codex", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "free-token"},
		},
		Sessions: newSessionStore(t),
		Scheduler: selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "held-codex", Provider: accounts.ProviderCodex, Fresh: true, WeeklyOnly: true,
				Headroom: 0.04, ShortHeadroom: 0.04, WeeklyHeadroom: 0.04, WeeklyRemaining: 0.04, WeeklyResetAt: time.Now().Add(50 * time.Hour)},
			{AccountID: "free-codex", Provider: accounts.ProviderCodex, Fresh: true, WeeklyOnly: true,
				Headroom: 0.6, ShortHeadroom: 0.6, WeeklyHeadroom: 0.6, WeeklyRemaining: 0.6, WeeklyResetAt: time.Now().Add(50 * time.Hour)},
		}),
		AccountReserves: map[string]float64{"held-codex": 0.05},
		MaxBodyBytes:    1 << 20,
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.4","input":"hi"}`))
	req.RemoteAddr = "127.0.0.1:5000"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "codex")
	req.Header.Set("X-Subrouter-Session", "pinned-codex-session")
	req.Header.Set("X-Subrouter-Account-ID", "held-codex")
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
	if body.Error.Type != "invalid_request_error" || body.Error.Code != "account_held_out" ||
		!strings.Contains(body.Error.Message, `pinned account "held-codex"`) ||
		!strings.Contains(body.Error.Message, "inside its owner's holdout") ||
		!strings.Contains(body.Error.Message, "SUBROUTER_CODEX_ACCOUNT_ID") {
		t.Fatalf("body = %+v", body)
	}
}
