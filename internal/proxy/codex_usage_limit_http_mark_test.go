package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
)

// codexUsageLimitPool is a fake chatgpt.com Codex backend on which the first
// OAuth account is refused: every request carrying oauth-token-0 gets a 429
// with the given body, the way the HTTP responses endpoint answers a spent
// account, and every other account completes. It records the token order.
func codexUsageLimitPool(t *testing.T, refusal string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen = append(seen, token)
		mu.Unlock()
		if token == "oauth-token-0" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, refusal)
			return
		}
		codexEgressWriteCompleted(w, token)
	}))
	t.Cleanup(server.Close)
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// A Codex session on the HTTP responses path (the box pool, and every turn
// that fell back from the websocket) whose account answers 429
// usage_limit_reached is moved to another account for that turn. The account
// that refused must then be held out of routing: the replacement's 200 is the
// only response the passive inspection ever sees, so the retry transport is
// the one place the refusal can be read. Before this, a Codex 429 was
// classified "fail over, do not mark" without its body being opened, the
// account stayed routable, and every new session placed on it paid a 429 and
// a failover until its reset -- days, for a weekly window, and indefinitely
// for a workspace spend cap or depleted credits, which the usage endpoint
// keeps reporting as healthy so no sweep ever catches.
//
// The burst case is the control: a 429 without a usage-limit code or message
// still fails over without marking, exactly as before, so a provider-wide
// throttle cannot cook the pool.
func TestCodexUsageLimit429OverHTTPHoldsTheAccountOutAfterFailover(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refusal string
		// wantHold is how long the refused account must stay out; zero means
		// it must not be marked at all.
		wantHold time.Duration
		// survivesFreshRead: a workspace-level refusal is invisible in the
		// member's usage windows, so a healthy usage reading must not end it.
		survivesFreshRead bool
	}{
		{
			name:     "member window with its own reset",
			refusal:  `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":172800}}`,
			wantHold: 48 * time.Hour,
		},
		{
			name:              "workspace spend cap",
			refusal:           `{"error":{"type":"usage_limit_reached","message":"You hit your spend cap set in your workspace.","rate_limit_reached_type":"workspace_owner_usage_limit_reached"}}`,
			wantHold:          codexWorkspaceExhaustionTTL,
			survivesFreshRead: true,
		},
		{
			name:     "workspace credits depleted",
			refusal:  `{"error":{"type":"usage_limit_reached","message":"Your workspace is out of credits.","rate_limit_reached_type":"workspace_member_credits_depleted"}}`,
			wantHold: codexWorkspaceExhaustionTTL,
			// Same workspace class as the spend cap.
			survivesFreshRead: true,
		},
		{
			name:    "burst without a usage-limit code (control)",
			refusal: `{"error":{"type":"rate_limit_error","message":"Too many requests"}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, seen := codexUsageLimitPool(t, tc.refusal)
			poolURL, err := url.Parse(pool.URL)
			if err != nil {
				t.Fatal(err)
			}
			server := codexEgressServer(t, poolURL, nil, 2)
			ref := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
			server.SchedulerRef = ref
			// A live conversation already pinned to the account that is about
			// to refuse.
			if _, err := server.Sessions.Put("codex", "session-a", "codex-account-0", ""); err != nil {
				t.Fatal(err)
			}
			proxy := httptest.NewServer(server.Handler())
			defer proxy.Close()

			status, body := codexEgressPost(t, proxy.URL, "session-a")
			if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-1") {
				t.Fatalf("status=%d body=%s, want the turn served by account 1 after failover", status, body)
			}
			if got := seen(); len(got) != 2 || got[0] != "oauth-token-0" || got[1] != "oauth-token-1" {
				t.Fatalf("pool saw %v, want account 0 refused then account 1", got)
			}

			until, marked := ref.ExhaustedUntilFor(accounts.ProviderCodex, "codex-account-0", "")
			if tc.wantHold == 0 {
				if marked || ref.Get().Exhausted(accounts.ProviderCodex, "codex-account-0") {
					t.Fatalf("a burst 429 marked the account until %v; only an explicit usage limit may", until)
				}
				return
			}
			if !marked || !ref.Get().Exhausted(accounts.ProviderCodex, "codex-account-0") {
				t.Fatal("the account that answered 429 usage_limit_reached is still routable after the failover succeeded")
			}
			// The hold-out is the refusal's own: its resets_in_seconds, or the
			// workspace re-probe interval, not the short default TTL.
			if hold := time.Until(until); hold < tc.wantHold-time.Minute || hold > tc.wantHold+time.Minute {
				t.Fatalf("account held out for %s, want about %s", hold.Round(time.Second), tc.wantHold)
			}

			// A new session must not be placed on the refused account.
			status, body = codexEgressPost(t, proxy.URL, "session-b")
			if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-1") {
				t.Fatalf("new session status=%d body=%s, want account 1", status, body)
			}
			if got := seen(); len(got) != 3 || got[2] != "oauth-token-1" {
				t.Fatalf("pool saw %v, want the new session to go straight to account 1", got)
			}

			// A fresh, healthy usage reading ends a member-window mark early
			// (its reset is in the reading) but not a workspace refusal, which
			// the member's windows cannot show. This is the half no sweep could
			// ever have supplied: without the mark from the 429 there is
			// nothing here to survive.
			ref.Set(selectacct.NewScheduler([]selectacct.Score{{
				AccountID: "codex-account-0", Provider: accounts.ProviderCodex,
				Headroom: 0.9, ShortHeadroom: 0.9, Fresh: true,
			}}))
			if held := ref.Get().Exhausted(accounts.ProviderCodex, "codex-account-0"); held != tc.survivesFreshRead {
				t.Fatalf("held after a fresh healthy usage reading = %v, want %v", held, tc.survivesFreshRead)
			}
		})
	}
}

// Reading the 429's body is new; failing over on a Codex 429 is not. A body
// that breaks mid-read says nothing about the account, so it must not cost
// the client the failover the status alone always earned: upstream #399 as
// written returns the read error, and the retry transport answers an
// inspection error by handing the 429 straight back. Driven through the real
// RoundTrip: the turn must land on the second account and nothing is marked.
func TestCodex429WithUnreadableBodyStillFailsOverWithoutMarking(t *testing.T) {
	const refusedID, spareID = "codex:refused", "codex:spare"
	var attempts []string
	base := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempts = append(attempts, request.Header.Get("Authorization"))
		if len(attempts) == 1 {
			return &http.Response{StatusCode: http.StatusTooManyRequests,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(io.MultiReader(strings.NewReader(`{"error":{"type":"usage_lim`),
					iotest.ErrReader(errors.New("upstream reset")))),
				Request: request}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Request: request}, nil
	})
	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	server := &Server{
		Accounts: []accounts.Account{
			{ID: refusedID, Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "refused-token"},
			{ID: spareID, Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "spare-token"},
		},
		SchedulerRef:  ref,
		CodexUpstream: mustParseURL(t, "https://chatgpt.test/backend-api/codex"),
	}
	transport := usageLimitRetryTransport{base: base, server: server, provider: accounts.ProviderCodex,
		account: refusedID, path: "/responses", poolModel: "gpt-5", maxAttempts: 2}
	request := httptest.NewRequest(http.MethodPost, "https://chatgpt.test/backend-api/codex/responses", strings.NewReader(`{"model":"gpt-5"}`))
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(`{"model":"gpt-5"}`)), nil
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || len(attempts) != 2 {
		t.Fatalf("status %d after %d attempt(s), want the turn failed over to the second account", response.StatusCode, len(attempts))
	}
	if _, marked := ref.ExhaustedUntilFor(accounts.ProviderCodex, refusedID, ""); marked {
		t.Fatal("an unreadable 429 body marked the account; only a readable usage-limit refusal may")
	}
}
