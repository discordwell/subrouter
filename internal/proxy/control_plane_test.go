package proxy

import (
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
	"github.com/discordwell/subrouter/session"
)

func controlPlaneTestServer(t *testing.T, identity string) Server {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Three healthy accounts and a scheduler that has every reason to spread
	// across them: identical providers, all with headroom.
	list := []accounts.Account{
		{ID: "zeta@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "z"},
		{ID: "alpha@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "a"},
		{ID: "mid@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "m"},
	}
	scores := make([]selectacct.Score, 0, len(list))
	for _, account := range list {
		scores = append(scores, selectacct.Score{
			AccountID: account.ID, Provider: accounts.ProviderClaude,
			Headroom: 1, ShortHeadroom: 1,
		})
	}
	return Server{
		Accounts:          list,
		Sessions:          store,
		SchedulerRef:      selectacct.NewSchedulerRef(selectacct.NewScheduler(scores)),
		MaxBodyBytes:      1024,
		IdentityAccountID: identity,
	}
}

func controlPlaneRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "https://subrouter.test"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// The bug: control-plane calls were scheduled like inference, so consecutive
// calls were answered by different accounts and the client cached whichever
// organization replied as its own identity.
func TestControlPlaneAlwaysAnswersFromOneAccount(t *testing.T) {
	server := controlPlaneTestServer(t, "")
	seen := map[string]int{}
	for i := 0; i < 25; i++ {
		// A fresh session id each time, exactly like a client that opens a new
		// session: nothing sticky is available to hold the account still.
		account, _, _, _, err := server.accountForSessionProvider(
			accounts.ProviderClaude, "claude", "session-"+strings.Repeat("x", i),
			controlPlaneRequest(t, "/api/claude_cli/bootstrap"))
		if err != nil {
			t.Fatalf("selection %d failed: %v", i, err)
		}
		seen[account.ID]++
	}
	if len(seen) != 1 {
		t.Fatalf("control-plane calls were answered by %d different accounts (%v); "+
			"the client caches the first identity it is given, so any rotation is wrong", len(seen), seen)
	}
	if _, ok := seen["alpha@example.com"]; !ok {
		t.Fatalf("deterministic pick = %v, want the lowest account id", seen)
	}
}

// IdentityAccountID names which account is the stable one.
func TestControlPlaneHonoursTheNamedIdentityAccount(t *testing.T) {
	server := controlPlaneTestServer(t, "mid@example.com")
	account, _, _, _, err := server.accountForSessionProvider(
		accounts.ProviderClaude, "claude", "s", controlPlaneRequest(t, "/api/oauth/profile"))
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != "mid@example.com" {
		t.Fatalf("identity account = %q, want mid@example.com", account.ID)
	}
}

// Inference must be UNAFFECTED -- pinning the control plane must not pin the
// pool, or one account would serve every request and the pool would be pointless.
func TestControlPlanePinDoesNotPinInference(t *testing.T) {
	server := controlPlaneTestServer(t, "mid@example.com")
	req, err := http.NewRequest(http.MethodPost, "https://subrouter.test/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	account, _, _, _, err := server.accountForSessionProvider(accounts.ProviderClaude, "claude", "inference-1", req)
	if err != nil {
		t.Fatalf("inference selection failed: %v", err)
	}
	if account.ID == "" {
		t.Fatal("inference selection returned no account")
	}
	// The control-plane pin must not have bound the inference session.
	if assignment, ok := server.Sessions.Get("claude", "inference-1"); !ok || assignment.AccountID == "" {
		t.Fatal("inference selection should still record a sticky assignment")
	}
}

// A control-plane call answered by the PEER reports the peer's organization.
func TestControlPlaneNeverSpillsToThePeer(t *testing.T) {
	peer, err := url.Parse("https://peer.test:31416")
	if err != nil {
		t.Fatal(err)
	}
	server := controlPlaneTestServer(t, "")
	server.RemotePool = &RemotePool{URL: peer}

	if server.remotePoolEligible(controlPlaneRequest(t, "/api/claude_cli/bootstrap")) {
		t.Fatal("a control-plane call must never be forwarded to the peer: it would " +
			"report the PEER's org identity and entitlements, which the client caches as its own")
	}
	if server.remotePoolEligible(controlPlaneRequest(t, "/api/oauth/profile")) {
		t.Fatal("/api/oauth/profile must never be forwarded to the peer")
	}
	// Inference still spills, or the remote pool does nothing.
	inference, err := http.NewRequest(http.MethodPost, "https://subrouter.test/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !server.remotePoolEligible(inference) {
		t.Fatal("inference must still be eligible for the peer")
	}
}

func TestControlPlanePathClassification(t *testing.T) {
	for _, path := range []string{"/api", "/api/", "/api/oauth/profile", "/api/claude_cli/bootstrap", "/api/claude_code/skills"} {
		if !controlPlanePath(path) {
			t.Errorf("controlPlanePath(%q) = false, want true", path)
		}
	}
	// Inference, and Codex's backend, must not be caught.
	for _, path := range []string{"/v1/messages", "/v1/responses", "/backend-api/codex/responses", "/apifoo", "/"} {
		if controlPlanePath(path) {
			t.Errorf("controlPlanePath(%q) = true, want false", path)
		}
	}
}
