package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/tenant"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func TestCredentialLeaseReportsCodexTokenExpiryFromTheJWT(t *testing.T) {
	t.Parallel()

	expiry := time.Now().Add(37 * time.Minute).UTC().Truncate(time.Second)
	server := newCredentialLeaseExpiryServer(t, accounts.Account{
		ID: "codex-a", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth,
		Token:     proxyTestCodexJWT("user@example.com", "jti-lease", expiry),
		AccountID: "provider-account",
	})

	lease := issueCredentialLeaseForExpiry(
		t, server, `{"provider":"codex","agentType":"codex","sessionId":"session-1"}`,
	)

	reported, err := time.Parse(time.RFC3339Nano, leaseStringField(t, lease, "credentialExpiresAt"))
	if err != nil {
		t.Fatal(err)
	}
	// The lease TTL is 5 minutes regardless; only the token's own exp tells the
	// borrower whether the credential outlives the work it is about to start.
	if !reported.Equal(expiry) {
		t.Fatalf("credentialExpiresAt = %s, want %s", reported, expiry)
	}
	if got := leaseStringField(t, lease, "providerAccountId"); got != "provider-account" {
		t.Fatalf("providerAccountId = %q, want the account's ChatGPT-Account-ID", got)
	}
}

func TestCredentialLeaseOmitsUnknownCredentialExpiry(t *testing.T) {
	t.Parallel()

	// A guessed expiry retires a live token, so every path that cannot read a
	// real one has to drop the field rather than emit a zero value.
	for name, testCase := range map[string]struct {
		leased  accounts.Account
		request string
	}{
		"codex token is not a jwt": {
			leased: accounts.Account{
				ID: "codex-a", Provider: accounts.ProviderCodex,
				AuthMode: accounts.AuthModeOAuth, Token: "opaque-access-token",
			},
			request: `{"provider":"codex","agentType":"codex","sessionId":"session-1"}`,
		},
		"claude account has no stored credential path": {
			leased: accounts.Account{
				ID: "claude-a", Provider: accounts.ProviderClaude,
				AuthMode: accounts.AuthModeOAuth, Token: "claude-access",
			},
			request: `{"provider":"claude","agentType":"claude","sessionId":"session-1"}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			lease := issueCredentialLeaseForExpiry(
				t, newCredentialLeaseExpiryServer(t, testCase.leased), testCase.request,
			)
			if raw, ok := lease["credentialExpiresAt"]; ok {
				t.Fatalf("credentialExpiresAt present with unknown expiry: %s", raw)
			}
		})
	}
}

// TestCredentialLeaseParsesThroughTheBorrowersOwnGuard runs the handler's real
// bytes through broker.Client rather than a copied fixture: leaseWire is
// unexported and package broker cannot import package proxy (proxy imports
// broker), so the client's public Lease call over an httptest origin is the
// only way to reach leaseWire.UnmarshalJSON with output the handler actually
// produced.
func TestCredentialLeaseParsesThroughTheBorrowersOwnGuard(t *testing.T) {
	t.Parallel()

	expiry := time.Now().Add(29 * time.Minute).UTC().Truncate(time.Second)
	server := newCredentialLeaseExpiryServer(t, accounts.Account{
		ID: "codex-a", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth,
		Token:     proxyTestCodexJWT("user@example.com", "jti-roundtrip", expiry),
		AccountID: "provider-account",
	})
	store := newTenantCredentialLeaseStore()
	var injectKey atomic.Value
	injectKey.Store("")
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := httptest.NewRecorder()
		store.handleIssue(server, tenant.Tenant{ID: "team-1"}, recorder, r)
		body := recorder.Body.Bytes()
		if key, _ := injectKey.Load().(string); key != "" {
			body = withExtraLeaseField(body, key)
		}
		w.Header().Set("Content-Type", recorder.Header().Get("Content-Type"))
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(body)
	}))
	defer origin.Close()

	client := broker.NewClient(broker.Config{
		Version: 1, BaseURL: broker.DefaultBaseURL,
		AccessToken: "cmux-access", RefreshToken: "cmux-refresh",
		TeamID: "team-1", CredentialSource: broker.CredentialSourceTeam,
		HostedURL: origin.URL, TenantKey: "srt_0123456789abcdef0123456789abcdef",
	})
	client.HTTPClient = origin.Client()

	lease, err := client.Lease(context.Background(), broker.LeaseRequest{
		Provider: account.ProviderCodex, RequiredAuthMode: account.AuthModeOAuth,
		AgentType: "codex", SessionID: "session-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !lease.CredentialExpiresAt.Equal(expiry) {
		t.Fatalf("CredentialExpiresAt = %s, want %s", lease.CredentialExpiresAt, expiry)
	}
	if lease.Account.AccountID != "provider-account" {
		t.Fatalf("ProviderAccountID = %q, want provider-account", lease.Account.AccountID)
	}

	// The clean parse above only means something if the guard can reject at
	// all; leaseWire tolerates unknown keys, so a parser that had stopped
	// inspecting the reject list would still read as green. Each probe needs a
	// fresh session id to get past the client's lease cache.
	//
	// The benign key is the control: doHostedJSON rewrites the guard's message
	// into a generic "invalid response", so without it a rejection caused by
	// re-encoding the body here would look identical to the guard firing.
	injectKey.Store("leaseNotes")
	if _, err := client.Lease(context.Background(), broker.LeaseRequest{
		Provider: account.ProviderCodex, RequiredAuthMode: account.AuthModeOAuth,
		AgentType: "codex", SessionID: "session-2",
	}); err != nil {
		t.Fatalf("borrower rejected a re-encoded lease with a harmless field: %v", err)
	}
	injectKey.Store("refreshToken")
	if _, err := client.Lease(context.Background(), broker.LeaseRequest{
		Provider: account.ProviderCodex, RequiredAuthMode: account.AuthModeOAuth,
		AgentType: "codex", SessionID: "session-3",
	}); err == nil {
		t.Fatal("borrower accepted a lease carrying refreshToken")
	}
}

func withExtraLeaseField(body []byte, key string) []byte {
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		return body
	}
	lease, ok := envelope["lease"].(map[string]any)
	if !ok {
		return body
	}
	lease[key] = "steal"
	injected, err := json.Marshal(envelope)
	if err != nil {
		return body
	}
	return injected
}

func TestCredentialLeaseNeverCarriesForbiddenCredentialFields(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		leased  accounts.Account
		request string
	}{
		"codex lease with every optional field populated": {
			leased: accounts.Account{
				ID: "codex-a", Provider: accounts.ProviderCodex,
				AuthMode: accounts.AuthModeOAuth, Label: "work", Email: "user@example.com",
				Token: proxyTestCodexJWT("user@example.com", "jti-forbidden",
					time.Now().Add(time.Hour)),
				AccountID: "provider-account", Source: "/tmp/codex",
			},
			request: `{"provider":"codex","agentType":"codex","sessionId":"session-1"}`,
		},
		"claude lease": {
			leased: accounts.Account{
				ID: "claude-a", Provider: accounts.ProviderClaude,
				AuthMode: accounts.AuthModeOAuth, Label: "work",
				Email: "user@example.com", Token: "claude-access",
			},
			request: `{"provider":"claude","agentType":"claude","sessionId":"session-1"}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			request := httptest.NewRequest(
				http.MethodPost, "/_subrouter/leases", strings.NewReader(testCase.request),
			)
			response := httptest.NewRecorder()
			newTenantCredentialLeaseStore().handleIssue(
				newCredentialLeaseExpiryServer(t, testCase.leased),
				tenant.Tenant{ID: "team"}, response, request,
			)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			var decoded any
			if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			// Walks the whole envelope, not just the lease object: the borrower's
			// guard only inspects the lease, so a secret parked one level out
			// would ship unnoticed.
			assertNoForbiddenLeaseKeys(t, decoded, "")
		})
	}
}

// assertNoForbiddenLeaseKeys mirrors the reject list in
// broker.leaseWire.UnmarshalJSON, which is unexported and cannot be shared.
func assertNoForbiddenLeaseKeys(t *testing.T, value any, path string) {
	t.Helper()
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			for _, forbidden := range []string{
				"refreshToken", "refresh_token", "idToken", "id_token",
				"credentials", "tokens", "apiKey",
			} {
				if key == forbidden {
					t.Fatalf("lease response carried forbidden field %q at %s", key, path+"/"+key)
				}
			}
			assertNoForbiddenLeaseKeys(t, child, path+"/"+key)
		}
	case []any:
		for _, child := range typed {
			assertNoForbiddenLeaseKeys(t, child, path+"/[]")
		}
	}
}

func newCredentialLeaseExpiryServer(t *testing.T, leased accounts.Account) *Server {
	t.Helper()
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &Server{
		Accounts:  []accounts.Account{leased},
		Sessions:  sessions,
		Scheduler: selectacct.NewScheduler(nil),
	}
}

func issueCredentialLeaseForExpiry(
	t *testing.T,
	server *Server,
	body string,
) map[string]json.RawMessage {
	t.Helper()
	request := httptest.NewRequest(
		http.MethodPost, "/_subrouter/leases", strings.NewReader(body),
	)
	response := httptest.NewRecorder()
	newTenantCredentialLeaseStore().handleIssue(
		server, tenant.Tenant{ID: "team"}, response, request,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var envelope struct {
		Lease map[string]json.RawMessage `json:"lease"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Lease
}

func leaseStringField(t *testing.T, lease map[string]json.RawMessage, key string) string {
	t.Helper()
	raw, ok := lease[key]
	if !ok {
		t.Fatalf("lease is missing %q", key)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}
