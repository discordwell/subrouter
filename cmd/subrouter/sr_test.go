package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	baseaccount "github.com/discordwell/subrouter/account"
	"github.com/discordwell/subrouter/internal/accounts"
	agentclaude "github.com/discordwell/subrouter/internal/agents/claude"
	agentgrok "github.com/discordwell/subrouter/internal/agents/grok"
	agentkimi "github.com/discordwell/subrouter/internal/agents/kimi"
	"github.com/discordwell/subrouter/internal/agents/opencode"
	"github.com/discordwell/subrouter/internal/agents/pi"
	agentqwen "github.com/discordwell/subrouter/internal/agents/qwen"
	"github.com/discordwell/subrouter/internal/broker"
	"github.com/discordwell/subrouter/internal/proxy"
	"github.com/discordwell/subrouter/selectacct"
	"github.com/discordwell/subrouter/session"
	"github.com/mattn/go-runewidth"
)

type fakeKimiUsageStore struct {
	accounts []baseaccount.Account
	plan     string
	windows  []accounts.UsageWindow
	err      error
	fetches  *int
}

type fakeGrokStore struct {
	authorized bool
	saved      bool
	removed    bool
	refreshDid bool
	refreshes  int
	preflights int
	credential agentgrok.CredentialInfo
	account    baseaccount.Account
}

func (s *fakeGrokStore) Authorize(context.Context, *http.Client, io.Writer) (agentgrok.CredentialInfo, error) {
	s.authorized = true
	return s.credential, nil
}

func (s *fakeGrokStore) SaveCredential(credential agentgrok.CredentialInfo) (baseaccount.Account, error) {
	s.saved = true
	s.credential = credential
	return s.account, nil
}

func (s *fakeGrokStore) RemoveCredential() (baseaccount.Account, bool, error) {
	if s.account.ID == "" {
		return baseaccount.Account{}, false, nil
	}
	removed := s.account
	s.account = baseaccount.Account{}
	s.removed = true
	return removed, true, nil
}

func (s *fakeGrokStore) ListAccounts(context.Context) ([]baseaccount.Account, error) {
	if s.account.ID == "" {
		return nil, nil
	}
	return []baseaccount.Account{s.account}, nil
}

func (s *fakeGrokStore) RefreshAccount(_ context.Context, _ *http.Client, account baseaccount.Account) (baseaccount.Account, error) {
	return account, nil
}

func (s *fakeGrokStore) RefreshAccountIfNeeded(_ context.Context, _ *http.Client, account baseaccount.Account) (baseaccount.Account, bool, error) {
	s.refreshes++
	return account, s.refreshDid, nil
}

func (s *fakeGrokStore) AccountRefreshState(account baseaccount.Account, _ time.Time) (baseaccount.Account, bool, error) {
	s.preflights++
	return account, s.refreshDid, nil
}

func TestGrokSignInAuthorizesThenPublishesWithoutPrintingIdentityOrTokens(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	fake := &fakeGrokStore{
		credential: agentgrok.CredentialInfo{
			AccessToken: "secret-access", RefreshToken: "secret-refresh",
			ExpiresAt: time.Now().Add(time.Hour), Email: "private@example.com",
		},
		account: baseaccount.Account{
			ID: "grok-subscription", Provider: baseaccount.ProviderGrok,
			AuthMode: baseaccount.AuthModeOAuth, Token: "secret-access",
		},
	}
	var out bytes.Buffer
	runner := srRunner{store: store, out: &out, grok: fake}
	if err := runner.grokSignIn(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !fake.authorized || !fake.saved {
		t.Fatalf("authorize=%v saved=%v, want both lifecycle stages", fake.authorized, fake.saved)
	}
	if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); err != nil {
		t.Fatalf("Grok add did not publish account generation: %v", err)
	}
	message := out.String()
	for _, secret := range []string{"secret-access", "secret-refresh", "private@example.com"} {
		if strings.Contains(message, secret) {
			t.Fatalf("public success text leaked %q: %q", secret, message)
		}
	}
	if !strings.Contains(message, "Added Grok subscription account") {
		t.Fatalf("success text = %q", message)
	}
}

func TestRemoteAndHostedGrokAddAreExplicitlyUnsupported(t *testing.T) {
	runner := srRunner{program: "sr", out: io.Discard}
	remoteErr := runner.runRemoteAccountCommand(t.Context(), srServerConfig{Name: "test", URL: "https://example.invalid"}, []string{"add", "grok"})
	if remoteErr == nil || !strings.Contains(remoteErr.Error(), "Grok subscription import is not available yet") || !strings.Contains(remoteErr.Error(), "sr remote use local") {
		t.Fatalf("remote Grok error = %v", remoteErr)
	}
	hostedErr := runner.hostedAccountAdd(t.Context(), nil, []string{"grok"})
	if hostedErr == nil || !strings.Contains(hostedErr.Error(), "hosted Grok subscription accounts are not supported yet") || !strings.Contains(hostedErr.Error(), "sr remote use local") {
		t.Fatalf("hosted Grok error = %v", hostedErr)
	}
}

func TestLocalAccountDisplayNameHidesStableCodexKeyByDefault(t *testing.T) {
	auth := testCodexAuth("owner@example.com", "workspace-owner")
	account := accounts.StoredCodexAccount{Email: "codex-owner-stable-key", Auth: auth}

	display := account.DisplayName()
	if got := localAccountDisplayName(account, false); got != display || strings.Contains(got, "codex-owner") {
		t.Fatalf("default display name = %q, want %q", got, display)
	}
	withID := localAccountDisplayName(account, true)
	if withID != display+" [codex-owner-stable-key]" {
		t.Fatalf("ID display name = %q, want explicit stable key", withID)
	}
}

func TestListHidesStableCodexKeysUnlessRequested(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.CodexStore{Dir: filepath.Join(home, "accounts")}
	for index, workspace := range []string{"workspace-one", "workspace-two"} {
		if err := store.SaveStored(accounts.StoredCodexAccount{
			Email:   fmt.Sprintf("codex-owner-%d", index+1),
			AddedAt: "2026-01-01T00:00:00Z",
			Auth:    testCodexAuth("owner@example.com", workspace),
		}); err != nil {
			t.Fatal(err)
		}
	}

	var out bytes.Buffer
	runner := srRunner{program: "sr", store: store, out: &out}
	if err := runner.list(nil); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "owner@example.com") || !strings.Contains(text, "sr list --ids") {
		t.Fatalf("default list = %q, want email and IDs hint", text)
	}
	if strings.Contains(text, "codex-owner-1") || strings.Contains(text, "codex-owner-2") {
		t.Fatalf("default list leaked stable IDs: %q", text)
	}

	out.Reset()
	if err := runner.list([]string{"--ids"}); err != nil {
		t.Fatal(err)
	}
	text = out.String()
	if !strings.Contains(text, "] [codex-owner-1]") || !strings.Contains(text, "] [codex-owner-2]") {
		t.Fatalf("ID list = %q, want explicit stable IDs", text)
	}
}

func TestAccountListRejectsUnsupportedOptionsInEveryMode(t *testing.T) {
	runner := srRunner{program: "sr", out: io.Discard}
	args := []string{"--invalid"}
	for name, call := range map[string]func() error{
		"local":  func() error { return runner.list(args) },
		"remote": func() error { return runner.listServerAccounts(t.Context(), srServerConfig{}, args) },
		"hosted": func() error {
			_, err := runner.runTeamCredentialCommand(t.Context(), append([]string{"list"}, args...))
			return err
		},
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "usage: sr list [--ids]") {
			t.Fatalf("%s: got %v, want list usage error before reading accounts", name, err)
		}
	}
}

func TestAccountListIDsIncludesExactAPIKeySelector(t *testing.T) {
	account := accounts.StoredCodexAccount{Email: "apikey:work", Auth: accounts.CodexAuthFile{AuthMode: "apikey"}}
	if got := localAccountDisplayName(account, true); got != "work (api key) [apikey:work]" {
		t.Fatalf("ID display = %q, want exact selector", got)
	}
}

// When local credentials are served by a protected serving daemon, "sr add
// codex ..." is routed to runRemoteAccountCommand's "add" case before
// addProvider ever runs (see sr.go's routeToServingAPI check). That case used
// to hand the raw ["codex", "--device-auth"] tail straight to a flag.FlagSet,
// which stops parsing at the first non-flag argument and rejected "codex"
// itself as an unexpected argument -- so device-auth login never happened on
// this path.
func TestRemoteAddCodexWithDeviceAuthReachesIsolatedLogin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var uploaded serverAccountImportRequest
	var uploadCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/_subrouter/account-import" {
			t.Errorf("path = %q, want account import endpoint", req.URL.Path)
			http.NotFound(w, req)
			return
		}
		switch req.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `{"ok":true}`)
		case http.MethodPost:
			if err := json.NewDecoder(req.Body).Decode(&uploaded); err != nil {
				t.Error("could not decode account import payload")
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			uploadCount++
			_, _ = io.WriteString(w, `{"ok":true}`)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	auth := testCodexAuth("device@example.com", "acct_device")
	identifier, err := accounts.CodexOAuthIdentifier(auth)
	if err != nil {
		t.Fatal(err)
	}
	fake := &recordingSRCommandRunner{loginAuth: auth}
	var out bytes.Buffer
	runner := srRunner{program: "sr", out: &out, errOut: &out, cmd: fake}
	remote := srServerConfig{Name: "test", URL: server.URL}
	if err := runner.runRemoteAccountCommand(t.Context(), remote, []string{"add", "codex", "--device-auth"}); err != nil {
		t.Fatalf("add codex --device-auth on the serving path: %v", err)
	}
	if !fake.hasCommand("codex", "login", "--device-auth") {
		t.Fatalf("missing isolated device-auth login command: %#v", fake.commands)
	}
	codexPresent := uploaded.Codex != nil
	accountIDMatches := codexPresent && uploaded.Codex.Email == identifier
	completeTokens := codexPresent && uploaded.Codex.Auth.Tokens != nil &&
		uploaded.Codex.Auth.Tokens.AccessToken != "" &&
		uploaded.Codex.Auth.Tokens.RefreshToken != "" &&
		uploaded.Codex.Auth.Tokens.IDToken != ""
	if uploadCount != 1 || uploaded.Provider != accounts.ProviderCodex || !codexPresent || !accountIDMatches || !completeTokens {
		// Never include the decoded request in a failure message: it contains the
		// OAuth access, refresh, and ID tokens sent for account import.
		t.Fatalf("account import validation failed: count=%d providerMatches=%t codexPresent=%t accountIDMatches=%t completeTokens=%t", uploadCount, uploaded.Provider == accounts.ProviderCodex, codexPresent, accountIDMatches, completeTokens)
	}
}

// Bare "sr add codex" (no flag) on the same serving path must reach an
// ordinary login, not be rejected as an unexpected positional argument -- the
// parsing bug applied whether or not --device-auth was present.
func TestRemoteAddCodexWithoutDeviceAuthReachesLogin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var uploadCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/_subrouter/account-import" {
			t.Errorf("path = %q, want account import endpoint", req.URL.Path)
			http.NotFound(w, req)
			return
		}
		switch req.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `{"ok":true}`)
		case http.MethodPost:
			uploadCount++
			_, _ = io.WriteString(w, `{"ok":true}`)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	fake := &recordingSRCommandRunner{loginAuth: testCodexAuth("browser@example.com", "acct_browser")}
	var out bytes.Buffer
	runner := srRunner{program: "sr", out: &out, errOut: &out, cmd: fake}
	remote := srServerConfig{Name: "test", URL: server.URL}
	if err := runner.runRemoteAccountCommand(t.Context(), remote, []string{"add", "codex"}); err != nil {
		t.Fatalf("add codex (no flag) on the serving path: %v", err)
	}
	if !fake.hasCommand("codex", "login") {
		t.Fatalf("missing isolated login command: %#v", fake.commands)
	}
	if fake.hasCommand("codex", "login", "--device-auth") {
		t.Fatalf("bare 'sr add codex' must not pass --device-auth: %#v", fake.commands)
	}
	if uploadCount != 1 {
		t.Fatalf("account upload count = %d, want 1", uploadCount)
	}
}

func TestGrokRemovePublishesAccountGeneration(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	fake := &fakeGrokStore{account: baseaccount.Account{
		ID: "grok-subscription", Provider: baseaccount.ProviderGrok, AuthMode: baseaccount.AuthModeOAuth,
	}}
	var out bytes.Buffer
	runner := srRunner{store: store, out: &out, grok: fake}
	if err := runner.remove(t.Context(), "grok-subscription"); err != nil {
		t.Fatal(err)
	}
	if !fake.removed {
		t.Fatal("Grok credential was not removed")
	}
	if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); err != nil {
		t.Fatalf("Grok removal did not publish account generation: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Removed account: grok-subscription") {
		t.Fatalf("removal output = %q", got)
	}
}

func TestGrokRemoveAcceptsDisplayedEmail(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	fake := &fakeGrokStore{account: baseaccount.Account{
		ID: "grok-subscription", Provider: baseaccount.ProviderGrok,
		AuthMode: baseaccount.AuthModeOAuth, Email: "shown@example.com",
		Label: "Grok (shown@example.com)",
	}}
	runner := srRunner{store: store, out: io.Discard, grok: fake}
	if err := runner.remove(t.Context(), "shown@example.com"); err != nil {
		t.Fatal(err)
	}
	if !fake.removed {
		t.Fatal("displayed Grok email did not remove the subscription account")
	}
}

func TestGrokRemoveRejectsEmailSharedWithStoredAccount(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email: "shared@example.com",
		Auth:  accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "codex-key"},
	}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeGrokStore{account: baseaccount.Account{
		ID: "grok-subscription", Provider: baseaccount.ProviderGrok,
		AuthMode: baseaccount.AuthModeOAuth, Email: "shared@example.com",
	}}
	runner := srRunner{store: store, out: io.Discard, grok: fake}
	err := runner.remove(t.Context(), "shared@example.com")
	if err == nil || !strings.Contains(err.Error(), "matches both") || !strings.Contains(err.Error(), "grok-subscription") {
		t.Fatalf("ambiguous removal error = %v", err)
	}
	if fake.removed {
		t.Fatal("ambiguous selector removed the Grok credential")
	}
	if _, ok, err := store.FindStored("shared@example.com"); err != nil || !ok {
		t.Fatalf("ambiguous selector removed stored account: ok=%v err=%v", ok, err)
	}
}

type refreshingKimiUsageStore struct {
	fetchedToken   string
	needsRefresh   bool
	refreshes      int
	preflights     int
	preflightToken string
}

type committedRefreshErrorRefresher struct {
	refreshed baseaccount.Account
	err       error
}

func (s committedRefreshErrorRefresher) RefreshAccountIfNeeded(context.Context, *http.Client, baseaccount.Account) (baseaccount.Account, bool, error) {
	return s.refreshed, true, s.err
}

type partialKimiUsageStore struct{}

func (partialKimiUsageStore) ListAccounts(context.Context) ([]baseaccount.Account, error) {
	return []baseaccount.Account{{
		ID: "kimi-subscription:healthy", Provider: baseaccount.ProviderKimi, AuthMode: baseaccount.AuthModeOAuth, Label: "healthy", Token: "access",
	}}, errors.New("managed profile is unreadable")
}

func (partialKimiUsageStore) FetchUsage(context.Context, *http.Client, baseaccount.Account) (string, []accounts.UsageWindow, error) {
	return "subscription", []accounts.UsageWindow{{Name: "weekly", UsedPercent: 1}}, nil
}

func (*refreshingKimiUsageStore) ListAccounts(context.Context) ([]baseaccount.Account, error) {
	return []baseaccount.Account{{
		ID: "kimi-subscription:work", Provider: baseaccount.ProviderKimi, AuthMode: baseaccount.AuthModeOAuth, Token: "stale",
	}}, nil
}

func (s *refreshingKimiUsageStore) RefreshAccountIfNeeded(_ context.Context, _ *http.Client, acct baseaccount.Account) (baseaccount.Account, bool, error) {
	s.refreshes++
	acct.Token = "fresh"
	return acct, true, nil
}

func (s *refreshingKimiUsageStore) AccountRefreshState(acct baseaccount.Account, _ time.Time) (baseaccount.Account, bool, error) {
	s.preflights++
	if s.preflightToken != "" {
		acct.Token = s.preflightToken
	}
	return acct, s.needsRefresh, nil
}

func (s *refreshingKimiUsageStore) FetchUsage(_ context.Context, _ *http.Client, acct baseaccount.Account) (string, []accounts.UsageWindow, error) {
	s.fetchedToken = acct.Token
	return "subscription", []accounts.UsageWindow{{Name: "weekly", UsedPercent: 1}}, nil
}

func TestLocalKimiRemovalPublishesAccountGeneration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", root)
	store := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
	if _, err := agentkimi.DefaultStore().SaveManagedCredential("work", agentkimi.CredentialInfo{
		AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runner := srRunner{store: store, out: &out}
	if err := runner.kimiCommand(t.Context(), []string{"remove", "work"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); err != nil {
		t.Fatalf("Kimi removal did not publish the shared account generation: %v", err)
	}
	if _, ok, err := agentkimi.DefaultStore().ReadManagedCredential("work", time.Now()); err != nil || ok {
		t.Fatalf("Kimi profile remains after removal (ok=%v err=%v)", ok, err)
	}
}

func TestGenericRemoveStillRemovesKimiAPIKeyAccounts(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	store := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
	apiKey, _, err := store.AddAPIKeyForProvider("work", "test-kimi-key", accounts.ProviderKimi)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runner := srRunner{store: store, out: &out}
	if err := runner.remove(t.Context(), apiKey.Email); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.FindStored(apiKey.Email); err != nil || ok {
		t.Fatalf("Kimi API key remains after generic removal (ok=%v err=%v)", ok, err)
	}
}

func TestLocalKimiStatusRefreshesBeforeFetchingUsage(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	store := &refreshingKimiUsageStore{needsRefresh: true}
	runner := srRunner{
		store: accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")},
		kimi:  store,
	}
	rows, err := runner.fetchUsageRows(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if store.fetchedToken != "fresh" {
		t.Fatal("local Kimi status fetched usage with the stale access token")
	}
	if store.preflights != 1 || store.refreshes != 1 {
		t.Fatalf("Kimi refresh preflights=%d refreshes=%d, want 1/1", store.preflights, store.refreshes)
	}
	found := false
	for _, row := range rows {
		if row.email == "kimi-subscription:work" && row.err == nil {
			found = true
		}
	}
	if !found {
		t.Fatal("refreshed Kimi status row is missing")
	}
}

func TestStatusRefreshPublishesAndReturnsCommittedAccountWithPostCommitError(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	original := baseaccount.Account{ID: "kimi-code", Provider: baseaccount.ProviderKimi, AuthMode: baseaccount.AuthModeOAuth, Token: "stale"}
	committed := original
	committed.Token = "fresh"
	postCommitErr := errors.New("post-commit lock release failed")
	runner := srRunner{store: store, client: http.DefaultClient}

	got, err := runner.refreshStatusOAuthAccount(t.Context(), original, committedRefreshErrorRefresher{
		refreshed: committed,
		err:       postCommitErr,
	})
	if !errors.Is(err, postCommitErr) {
		t.Fatalf("status refresh error = %v, want post-commit error", err)
	}
	if got.Token != "fresh" {
		t.Fatalf("status refresh returned token %q, want committed token", got.Token)
	}
	if _, statErr := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); statErr != nil {
		t.Fatalf("committed refresh was not published: %v", statErr)
	}
}

func TestLocalProviderStatusDoesNotPublishFreshOAuthCredentials(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))

	kimi := &refreshingKimiUsageStore{preflightToken: "current-kimi"}
	grok := &fakeGrokStore{account: baseaccount.Account{
		ID: "grok-subscription", Provider: baseaccount.ProviderGrok,
		AuthMode: baseaccount.AuthModeOAuth, Token: "fresh-grok",
	}}
	if _, err := (srRunner{store: store, kimi: kimi, grok: grok}).fetchUsageRows(t.Context()); err != nil {
		t.Fatal(err)
	}
	if kimi.preflights != 1 || kimi.refreshes != 0 {
		t.Fatalf("Kimi preflights=%d refreshes=%d, want 1/0", kimi.preflights, kimi.refreshes)
	}
	if kimi.fetchedToken != "current-kimi" {
		t.Fatalf("Kimi status used %q, want the credential returned by preflight", kimi.fetchedToken)
	}
	if grok.preflights != 1 || grok.refreshes != 0 {
		t.Fatalf("Grok preflights=%d refreshes=%d, want 1/0", grok.preflights, grok.refreshes)
	}
	if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); !os.IsNotExist(err) {
		t.Fatalf("fresh provider status published an account generation: %v", err)
	}
}

func TestLocalClaudeStatusRefreshPublishesBeforeUsageAndReloadsAccount(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	store := accounts.DefaultCodexStore()
	claudeStore := agentclaude.DefaultStore()
	profile := seedClaudeStatusCredential(t, claudeStore, "primary", agentclaude.CredentialInfo{
		AccessToken: "stale-access", RefreshToken: "stale-refresh",
		ExpiresAt: time.Now().Add(-time.Hour).UnixMilli(), SubscriptionType: "pro",
	})

	ref, err := proxy.OpenAccountRef(store, claudeStore, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if got := accountToken(ref.All(), accounts.ProviderClaude, profile.Name); got != "stale-access" {
		t.Fatalf("initial Claude token = %q, want stale-access", got)
	}

	var refreshCalls, usageCalls atomic.Int32
	client := &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/v1/oauth/token":
			refreshCalls.Add(1)
			return srJSONResponse(req, http.StatusOK, `{"access_token":"fresh-access","refresh_token":"fresh-refresh","expires_in":3600}`), nil
		case "/api/oauth/usage":
			usageCalls.Add(1)
			if got := req.Header.Get("Authorization"); got != "Bearer fresh-access" {
				return nil, fmt.Errorf("Claude usage authorization = %q, want refreshed access token", got)
			}
			if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); err != nil {
				return nil, fmt.Errorf("Claude refresh was not published before usage: %w", err)
			}
			lockCtx, cancel := context.WithTimeout(req.Context(), time.Second)
			defer cancel()
			if err := proxy.PublishAccountDiskMutation(lockCtx, store.StoreDir(), func() (bool, error) {
				return false, nil
			}); err != nil {
				return nil, fmt.Errorf("Claude usage ran while account transaction remained locked: %w", err)
			}
			return srJSONResponse(req, http.StatusOK, `{"seven_day_oauth_apps":{"utilization":0.1,"resets_at":""}}`), nil
		default:
			return nil, fmt.Errorf("unexpected Claude status request %s %s", req.Method, req.URL)
		}
	})}

	rows, err := (srRunner{store: store, client: client}).fetchUsageRows(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if row := usageRowFor(rows, accounts.ProviderClaude, profile.Name); row == nil || row.err != nil {
		t.Fatalf("refreshed Claude status row = %+v", row)
	}
	if refreshCalls.Load() != 1 || usageCalls.Load() != 1 {
		t.Fatalf("Claude status calls refresh=%d usage=%d, want 1/1", refreshCalls.Load(), usageCalls.Load())
	}
	daemon := (proxy.Server{AccountRef: ref, AdminToken: "test-admin"}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/_subrouter/accounts", nil)
	request.Header.Set("Authorization", "Bearer test-admin")
	response := httptest.NewRecorder()
	daemon.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("daemon account lookup status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := accountToken(ref.All(), accounts.ProviderClaude, profile.Name); got != "fresh-access" {
		t.Fatalf("generation-triggered daemon reload token = %q, want fresh-access", got)
	}
}

func TestLocalClaudeStatusDoesNotPublishFreshCredential(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	store := accounts.DefaultCodexStore()
	claudeStore := agentclaude.DefaultStore()
	profile := seedClaudeStatusCredential(t, claudeStore, "fresh", agentclaude.CredentialInfo{
		AccessToken: "current-access", RefreshToken: "current-refresh",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), SubscriptionType: "max",
	})

	client := &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/api/oauth/usage" {
			return nil, fmt.Errorf("fresh Claude credential made unexpected request %s %s", req.Method, req.URL)
		}
		return srJSONResponse(req, http.StatusOK, `{"seven_day_oauth_apps":{"utilization":0.1,"resets_at":""}}`), nil
	})}
	rows, err := (srRunner{store: store, client: client}).fetchUsageRows(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if row := usageRowFor(rows, accounts.ProviderClaude, profile.Name); row == nil || row.err != nil {
		t.Fatalf("fresh Claude status row = %+v", row)
	}
	if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); !os.IsNotExist(err) {
		t.Fatalf("fresh Claude status published an account generation: %v", err)
	}
}

// A Claude Max account with no `five_hour` bucket, no Fable meter, and a Fable
// probe that answers a headerless 429, with 4% of its week left three days
// from the roll. The daemon routes it (PATCHES §62), so `sr status` must not
// call it not routable.
func TestLocalClaudeStatusRoutesAnAccountWithNoFiveHourOrFableReading(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	store := accounts.DefaultCodexStore()
	claudeStore := agentclaude.DefaultStore()
	profile := seedClaudeStatusCredential(t, claudeStore, "pixel", agentclaude.CredentialInfo{
		AccessToken: "current-access", RefreshToken: "current-refresh",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), SubscriptionType: "max",
	})
	roll := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	client := &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/oauth/usage":
			return srJSONResponse(req, http.StatusOK, `{"five_hour":null,"seven_day":{"utilization":96,"resets_at":"`+roll+`"},"seven_day_oauth_apps":null}`), nil
		case "/v1/messages":
			return srJSONResponse(req, http.StatusTooManyRequests, `{"type":"error","error":{"type":"rate_limit_error"}}`), nil
		default:
			return nil, fmt.Errorf("unexpected Claude status request %s %s", req.Method, req.URL)
		}
	})}
	rows, err := (srRunner{store: store, client: client}).fetchUsageRows(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	row := usageRowFor(rows, accounts.ProviderClaude, profile.Name)
	if row == nil || row.err != nil {
		t.Fatalf("Claude status row = %+v", row)
	}
	if got := compactPickReason(*row); got != "4% left" {
		t.Fatalf("sr status reads %q, want \"4%% left\": the daemon routes this account", got)
	}
}

func TestLocalClaudeStatusPublicationFailurePreventsRefresh(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	store := accounts.DefaultCodexStore()
	claudeStore := agentclaude.DefaultStore()
	profile := seedClaudeStatusCredential(t, claudeStore, "blocked", agentclaude.CredentialInfo{
		AccessToken: "stale-access", RefreshToken: "stale-refresh",
		ExpiresAt: time.Now().Add(-time.Hour).UnixMilli(),
	})
	if err := os.MkdirAll(filepath.Join(store.StoreDir(), ".account-generation"), 0o700); err != nil {
		t.Fatal(err)
	}

	var requests atomic.Int32
	client := &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, fmt.Errorf("credential request ran after publication failure: %s", req.URL)
	})}
	rows, err := (srRunner{store: store, client: client}).fetchUsageRows(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if row := usageRowFor(rows, accounts.ProviderClaude, profile.Name); row == nil || row.err == nil {
		t.Fatalf("Claude publication failure row = %+v", row)
	}
	if requests.Load() != 0 {
		t.Fatalf("Claude publication failure made %d credential requests, want 0", requests.Load())
	}
	credential, err := claudeStore.ReadCredential(t.Context(), claudeStore.ClaudeConfigDir(profile.Name))
	if err != nil {
		t.Fatal(err)
	}
	if credential == nil || credential.AccessToken != "stale-access" || credential.RefreshToken != "stale-refresh" {
		t.Fatalf("credential changed after publication failure: %+v", credential)
	}
}

// The hint under a Claude row of `sr status` names the repair for the login
// that profile really holds, with its own name, through the real status read
// (PATCHES §36): the OAuth profile whose refresh the provider refused is signed
// in again with `addr`, and both setup tokens -- the expired one and the one
// whose 401 says nothing about its kind -- are re-minted with `add`.
func TestLocalClaudeStatusHintNamesTheRepairForTheLogin(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	t.Setenv("COLUMNS", "200")
	// Every credential read also asks the keychain. Answer "no such item" from
	// a stub so the test never reaches the real one.
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "security"), []byte("#!/bin/sh\nexit 44\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	store := accounts.DefaultCodexStore()
	claudeStore := agentclaude.DefaultStore()
	seedClaudeStatusCredential(t, claudeStore, "seven-alpha", agentclaude.CredentialInfo{
		AccessToken: "dead-access", RefreshToken: "dead-refresh",
		ExpiresAt: time.Now().Add(-time.Hour).UnixMilli(),
	})
	seedClaudeStatusCredential(t, claudeStore, "year-token", agentclaude.SetupTokenCredential(testSetupToken, time.Now()))
	seedClaudeStatusCredential(t, claudeStore, "old-token", agentclaude.SetupTokenCredential(testSetupToken, time.Now().AddDate(-2, 0, 0)))

	client := &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/v1/oauth/token" {
			return srJSONResponse(req, http.StatusBadRequest, `{"error":"invalid_grant","error_description":"Refresh token not found or invalid"}`), nil
		}
		return srJSONResponse(req, http.StatusUnauthorized, `{"type":"error","error":{"type":"authentication_error"}}`), nil
	})}
	rows, err := (srRunner{store: store, client: client}).fetchUsageRows(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Calibration: the year-long token failed with a bare 401, so only the
	// credential kind the row carries can name `add` for it.
	if row := usageRowFor(rows, accounts.ProviderClaude, "year-token"); row == nil || row.err == nil ||
		strings.Contains(row.err.Error(), "setup token") || !authErrorNeedsReadd(row.err) {
		t.Fatalf("year-token row = %+v, want a re-add error that does not name the setup token", row)
	}
	var out bytes.Buffer
	displayUsageRows(&out, rows, false)
	text := out.String()
	for _, want := range []string{
		"(re-auth with: sr claude addr seven-alpha)",
		"(re-add with: sr claude add year-token)",
		"(re-add with: sr claude add old-token)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("status output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "sr add claude") || strings.Contains(text, "on that server") {
		t.Fatalf("a local profile's hint names the wrong command:\n%s", text)
	}
}

func seedClaudeStatusCredential(t *testing.T, store agentclaude.Store, name string, credential agentclaude.CredentialInfo) agentclaude.Profile {
	t.Helper()
	if _, err := store.CreateProfile(name); err != nil {
		t.Fatal(err)
	}
	configDir := store.ClaudeConfigDir(name)
	if err := os.WriteFile(filepath.Join(configDir, ".credentials.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteCredential(t.Context(), configDir, credential); err != nil {
		t.Fatal(err)
	}
	profile, ok := store.FindProfile(name)
	if !ok {
		t.Fatalf("Claude profile %q was not created", name)
	}
	return profile
}

func srJSONResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func usageRowFor(rows []srUsageRow, provider accounts.Provider, accountID string) *srUsageRow {
	for i := range rows {
		if rows[i].provider == provider && rows[i].email == accountID {
			return &rows[i]
		}
	}
	return nil
}

func accountToken(all []baseaccount.Account, provider accounts.Provider, accountID string) string {
	for _, account := range all {
		if account.Provider == provider && account.ID == accountID {
			return account.Token
		}
	}
	return ""
}

func TestLocalKimiStatusSeparatesPartialSourceErrorFromHealthyAccount(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", root)
	runner := srRunner{
		store: accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")},
		kimi:  partialKimiUsageStore{},
	}
	rows, err := runner.fetchUsageRows(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var sourceErrors, healthy int
	for _, row := range rows {
		switch row.email {
		case "kimi":
			if row.err != nil && row.displayAccount == "credential source" && !row.active {
				sourceErrors++
			}
		case "kimi-subscription:healthy":
			if row.err == nil {
				healthy++
			}
		}
	}
	if sourceErrors != 1 || healthy != 1 {
		t.Fatalf("partial Kimi rows = source-errors:%d healthy:%d", sourceErrors, healthy)
	}
}

func TestKimiListPrintsHealthyProfilesAlongsidePartialWarning(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	store := agentkimi.DefaultStore()
	if _, err := store.SaveManagedCredential("healthy", agentkimi.CredentialInfo{
		AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	brokenName := base64.RawURLEncoding.EncodeToString([]byte("broken")) + ".json"
	brokenPath := filepath.Join(root, "state", "kimi", brokenName)
	if err := os.WriteFile(brokenPath, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	runner := srRunner{store: accounts.CodexStore{Dir: filepath.Join(root, "state", "codex", "accounts")}, out: &out, errOut: &errOut}
	if err := runner.kimiCommand(t.Context(), []string{"list"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "kimi-subscription:healthy") {
		t.Fatal("Kimi list hid the healthy managed profile")
	}
	if !strings.Contains(errOut.String(), "Warning: some Kimi credentials are unavailable") {
		t.Fatal("Kimi list did not surface the partial credential error")
	}
}

func TestKimiListMarksInteractiveCLICredentialNotRouted(t *testing.T) {
	root := t.TempDir()
	kimiHome := filepath.Join(root, "kimi-home")
	t.Setenv("KIMI_CODE_HOME", kimiHome)
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	path := filepath.Join(kimiHome, "credentials", "kimi-code.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"access_token":"cli-access","refresh_token":"cli-refresh","expires_at":4102444800}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runner := srRunner{store: accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}, out: &out}
	if err := runner.kimiCommand(t.Context(), []string{"list"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "kimi-code (Kimi CLI; not routed; Kimi Code)") {
		t.Fatalf("Kimi list did not mark the interactive credential CLI-only:\n%s", out.String())
	}
}

func TestFetchUsageRowsExcludesKimiCLICredentialWithoutRefreshingIt(t *testing.T) {
	root := t.TempDir()
	kimiHome := filepath.Join(root, "kimi-home")
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("KIMI_CODE_HOME", kimiHome)
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	path := filepath.Join(kimiHome, "credentials", "kimi-code.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	before := []byte(`{"access_token":"cli-stale","refresh_token":"cli-refresh","expires_at":1}`)
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	requests := 0
	client := &http.Client{Transport: srRoundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("unexpected network request")
	})}
	runner := srRunner{store: accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}, client: client}
	rows, err := runner.fetchUsageRows(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.provider == accounts.ProviderKimi && row.email == "kimi-code" {
			t.Fatalf("status exposed non-routable interactive Kimi credential: %+v", row)
		}
	}
	if requests != 0 {
		t.Fatalf("status made %d request(s) for the CLI-only credential", requests)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("status changed the interactive Kimi CLI credential bytes")
	}
	var out bytes.Buffer
	printKimiCLIOnlyStatusHint(&out, nil)
	if !strings.Contains(out.String(), "plain 'kimi' login is direct") || !strings.Contains(out.String(), "sr kimi") {
		t.Fatalf("status hint is not actionable: %q", out.String())
	}
	out.Reset()
	printKimiCLIOnlyStatusHint(&out, []srUsageRow{{
		email: "kimi-code", provider: accounts.ProviderKimi, authMode: accounts.AuthModeOAuth,
	}})
	if !strings.Contains(out.String(), "Plain 'kimi' uses the local direct login") || !strings.Contains(out.String(), "sr kimi") || strings.Contains(out.String(), "sr kimi login") {
		t.Fatalf("status did not distinguish direct and managed Kimi launchers: %q", out.String())
	}
	out.Reset()
	printKimiCLIOnlyStatusHint(&out, []srUsageRow{{
		email: "kimi:key", provider: accounts.ProviderKimi, authMode: accounts.AuthModeAPIKey,
	}})
	if !strings.Contains(out.String(), "routed Subrouter Kimi key pool") || !strings.Contains(out.String(), "sr kimi") || strings.Contains(out.String(), "sr kimi login") {
		t.Fatalf("status did not recognize an API-key-only Kimi pool: %q", out.String())
	}
}

func (f fakeKimiUsageStore) ListAccounts(context.Context) ([]baseaccount.Account, error) {
	return f.accounts, f.err
}

func (f fakeKimiUsageStore) FetchUsage(context.Context, *http.Client, baseaccount.Account) (string, []accounts.UsageWindow, error) {
	if f.fetches != nil {
		*f.fetches++
	}
	return f.plan, f.windows, f.err
}

func TestFetchUsageRowsDoesNotSendKimiAPIKeyToUnconfiguredVendorQuota(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	if _, _, err := store.AddAPIKeyForProvider("gateway", "gateway-only-key", accounts.ProviderKimi); err != nil {
		t.Fatal(err)
	}
	fetches := 0
	runner := srRunner{store: store, kimi: fakeKimiUsageStore{fetches: &fetches}}
	rows, err := runner.fetchUsageRows(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, row := range rows {
		if row.provider == accounts.ProviderKimi && row.authMode == accounts.AuthModeAPIKey {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Kimi API-key row is missing: %+v", rows)
	}
	if fetches != 0 {
		t.Fatalf("standalone status sent Kimi API key to vendor quota endpoint %d time(s)", fetches)
	}
}

func TestAutoImportIfEmptySkipsProviderOnlyOAuthInstallations(t *testing.T) {
	for _, test := range []struct {
		name string
		kimi srKimiUsageStore
		grok srGrokStore
	}{
		{
			name: "Kimi",
			kimi: fakeKimiUsageStore{accounts: []baseaccount.Account{{
				ID: "kimi-subscription:work", Provider: baseaccount.ProviderKimi, AuthMode: baseaccount.AuthModeOAuth,
			}}},
		},
		{
			name: "Grok",
			grok: &fakeGrokStore{account: baseaccount.Account{
				ID: "grok-subscription", Provider: baseaccount.ProviderGrok, AuthMode: baseaccount.AuthModeOAuth,
			}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", filepath.Join(root, "home"))
			t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
			store := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
			var out bytes.Buffer
			runner := srRunner{store: store, out: &out, kimi: test.kimi, grok: test.grok}
			if err := runner.autoImportIfEmpty(t.Context()); err != nil {
				t.Fatal(err)
			}
			if out.Len() != 0 {
				t.Fatalf("provider-only status printed Codex import output: %q", out.String())
			}
			if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); !os.IsNotExist(err) {
				t.Fatalf("provider-only status published a Codex mutation: %v", err)
			}
		})
	}
}

func TestAutoImportIfEmptyDoesNotPublishMissingActiveCodexAuth(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex-home"))
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	store := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
	var out bytes.Buffer
	runner := srRunner{
		store: store, out: &out,
		kimi: fakeKimiUsageStore{},
		grok: &fakeGrokStore{},
	}
	if err := runner.autoImportIfEmpty(t.Context()); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("missing active auth printed false import success: %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); !os.IsNotExist(err) {
		t.Fatalf("missing active auth published a mutation: %v", err)
	}
}

func TestAutoImportIfEmptyDoesNotPublishNonOAuthActiveCodexAuth(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	if err := accounts.WriteActiveCodexAuth(accounts.CodexAuthFile{OpenAIAPIKey: "sk-test"}); err != nil {
		t.Fatal(err)
	}
	store := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
	var out bytes.Buffer
	runner := srRunner{
		store: store, out: &out,
		kimi: fakeKimiUsageStore{},
		grok: &fakeGrokStore{},
	}
	if err := runner.autoImportIfEmpty(t.Context()); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("non-OAuth auth printed false import success: %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); !os.IsNotExist(err) {
		t.Fatalf("non-OAuth auth published a mutation: %v", err)
	}
}

func TestImportActiveRejectsMissingAuthBeforePublishingGeneration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	store := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
	var out bytes.Buffer
	runner := srRunner{store: store, out: &out}
	if err := runner.importActive(t.Context()); err == nil || !strings.Contains(err.Error(), "no active Codex OAuth auth") {
		t.Fatalf("import error = %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("failed import printed success: %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); !os.IsNotExist(err) {
		t.Fatalf("failed import published a mutation: %v", err)
	}
}

func TestFetchUsageRowsIncludesLocalKimiSubscription(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUBROUTER_SESSIONS", session.DefaultStorePath())
	windows := []accounts.UsageWindow{
		{Name: "weekly", UsedPercent: 25, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAfterSeconds: 2 * 24 * 60 * 60},
		{Name: "5h", UsedPercent: 40, LimitWindowSeconds: int64((5 * time.Hour) / time.Second), ResetAfterSeconds: 2 * 60 * 60},
	}
	runner := srRunner{
		store:  accounts.CodexStore{Dir: filepath.Join(home, ".codex", "accounts")},
		client: &http.Client{Timeout: time.Second},
		kimi: fakeKimiUsageStore{
			accounts: []baseaccount.Account{{ID: "kimi-code", Provider: baseaccount.ProviderKimi, AuthMode: baseaccount.AuthModeOAuth}},
			plan:     "subscription", windows: windows,
		},
	}
	rows, err := runner.fetchUsageRows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.provider != accounts.ProviderKimi {
			continue
		}
		if row.email != "kimi-code" || row.active || row.planType != "subscription" {
			t.Fatalf("Kimi row metadata = %+v", row)
		}
		if !row.sessionsKnown {
			t.Fatal("Kimi row did not distinguish ready from an assigned session")
		}
		if !slices.Equal(row.windows, windows) {
			t.Fatalf("Kimi windows = %+v, want %+v", row.windows, windows)
		}
		return
	}
	t.Fatal("local Kimi subscription row is missing")
}

func TestFetchUsageRowsKeepsMultipleKimiAccountsDistinct(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	runner := srRunner{
		store:  accounts.CodexStore{Dir: filepath.Join(home, ".codex", "accounts")},
		client: &http.Client{Timeout: time.Second},
		kimi: fakeKimiUsageStore{
			accounts: []baseaccount.Account{
				{ID: "kimi-code:first", Provider: baseaccount.ProviderKimi, AuthMode: baseaccount.AuthModeOAuth},
				{ID: "kimi-code:second", Provider: baseaccount.ProviderKimi, AuthMode: baseaccount.AuthModeOAuth},
			},
			plan: "subscription",
			windows: []accounts.UsageWindow{{
				Name: "5h", UsedPercent: 20,
				LimitWindowSeconds: int64((5 * time.Hour) / time.Second),
			}},
		},
	}
	rows, err := runner.fetchUsageRows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, row := range rows {
		if row.provider == accounts.ProviderKimi {
			got[row.email] = row.planType == "subscription" && len(row.windows) == 1
		}
	}
	for _, accountID := range []string{"kimi-code:first", "kimi-code:second"} {
		if !got[accountID] {
			t.Fatalf("Kimi account %q was lost or incompletely updated: %v", accountID, got)
		}
	}
}

func TestFetchUsageRowsIncludesAuthOnlyGrokSubscriptionActivity(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
	t.Setenv("SUBROUTER_SESSIONS", session.DefaultStorePath())
	grokStore := &fakeGrokStore{account: baseaccount.Account{
		ID: "grok-subscription", Provider: baseaccount.ProviderGrok,
		AuthMode: baseaccount.AuthModeOAuth, Token: "access", Email: "person@example.com",
	}}
	sessions, err := session.NewStore(session.DefaultStorePath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Put("grok", "conversation", "grok-subscription", ""); err != nil {
		t.Fatal(err)
	}
	runner := srRunner{
		store: accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")},
		grok:  grokStore,
	}
	rows, err := runner.fetchUsageRows(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.provider != accounts.ProviderGrok || row.authMode != accounts.AuthModeOAuth {
			continue
		}
		if row.email != "grok-subscription" || row.displayAccount != "person@example.com" || row.planType != "subscription" {
			t.Fatalf("Grok row metadata = %+v", row)
		}
		if row.providerHealth != "stored" || row.err != nil || len(row.windows) != 0 {
			t.Fatalf("Grok row should expose only stored auth without invented validation or quota: %+v", row)
		}
		if !row.sessionsKnown || row.assignedSessions != 1 || !row.active {
			t.Fatalf("Grok activity = %+v", row)
		}
		rankUsageRows(rows)
		if row.gtoRecommended || displayRecommendedForNewSession(row) {
			t.Fatalf("unverified Grok credential was recommended: %+v", row)
		}
		if state := usageGridState(row); state != "active" {
			t.Fatalf("Grok state = %q, want only observed active session", state)
		}
		return
	}
	t.Fatal("local Grok subscription row is missing")
}

func TestUnverifiedGrokStatusIsStoredAndNeverRecommended(t *testing.T) {
	rows := []srUsageRow{{
		email: "grok-subscription", provider: accounts.ProviderGrok,
		authMode: accounts.AuthModeOAuth, providerHealth: "stored",
		score: selectacct.Score{AccountID: "grok-subscription", Headroom: 1, ShortHeadroom: 1},
	}}
	rankUsageRows(rows)
	if rows[0].gtoRecommended || displayRecommendedForNewSession(rows[0]) {
		t.Fatalf("unverified Grok credential was recommended: %+v", rows[0])
	}
	if state := usageGridState(rows[0]); state != "stored" {
		t.Fatalf("Grok state = %q, want stored", state)
	}
	if color := usageGridStateColor(rows[0]); color != ansiDim {
		t.Fatalf("Grok stored state color = %q, want informational dim", color)
	}
}

func TestFetchUsageRowsUsesOnlyConfiguredSessionStore(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	configuredPath := filepath.Join(root, "daemon", "assignments.json")
	t.Setenv("SUBROUTER_SESSIONS", configuredPath)
	configured, err := session.NewStore(configuredPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := configured.Put("kimi", "configured-session", "kimi-code", ""); err != nil {
		t.Fatal(err)
	}
	defaultStore, err := session.NewStore(session.DefaultStorePath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := defaultStore.Put("kimi", "wrong-session", "someone-else", ""); err != nil {
		t.Fatal(err)
	}
	runner := srRunner{
		store: accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")},
		kimi: fakeKimiUsageStore{accounts: []baseaccount.Account{{
			ID: "kimi-code", Provider: baseaccount.ProviderKimi, AuthMode: baseaccount.AuthModeOAuth,
		}}, plan: "subscription"},
	}
	rows, err := runner.fetchUsageRows(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.provider == accounts.ProviderKimi {
			if !row.sessionsKnown || row.assignedSessions != 1 || !row.active {
				t.Fatalf("Kimi session activity = %+v", row)
			}
			return
		}
	}
	t.Fatal("local Kimi subscription row is missing")
}

func TestFetchUsageRowsLeavesSessionsUnknownWithoutConfiguredStore(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	runner := srRunner{
		store: accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")},
		kimi: fakeKimiUsageStore{accounts: []baseaccount.Account{{
			ID: "kimi-code", Provider: baseaccount.ProviderKimi, AuthMode: baseaccount.AuthModeOAuth,
		}}, plan: "subscription"},
	}
	rows, err := runner.fetchUsageRows(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.provider == accounts.ProviderKimi {
			if row.sessionsKnown || row.active {
				t.Fatalf("Kimi session activity was inferred without a configured store: %+v", row)
			}
			return
		}
	}
	t.Fatal("local Kimi subscription row is missing")
}

func TestSRListReadsNativeCodexStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "b@example.com",
		AddedAt: "2026-04-28T00:00:00Z",
		Auth: accounts.CodexAuthFile{Tokens: &accounts.CodexTokens{
			AccessToken: "access",
			IDToken:     "id",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "apikey:paid",
		AddedAt: "2026-04-28T00:00:00Z",
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "sk-test",
		},
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"list"}); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if !strings.Contains(got, "b@example.com") {
		t.Fatalf("list output missing OAuth account:\n%s", got)
	}
	if !strings.Contains(got, "paid (api key)") {
		t.Fatalf("list output missing API-key account:\n%s", got)
	}
}

func TestSRAddKeyStoresRegistryProviderInLocalStorage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader("work\nsk-or-v1-test\n"),
		out:    &out,
		errOut: &out,
	}

	if err := runner.run(context.Background(), []string{"add-key", "--provider", "open-router"}); err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.FindStored("openrouter:work")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing provider-scoped OpenRouter account")
	}
	if stored.Provider != accounts.ProviderOpenRouter || stored.Auth.AuthMode != "apikey" || stored.Auth.OpenAIAPIKey != "sk-or-v1-test" {
		t.Fatalf("stored account provider=%q auth_mode=%q key_matches=%t, want OpenRouter API-key account", stored.Provider, stored.Auth.AuthMode, stored.Auth.OpenAIAPIKey == "sk-or-v1-test")
	}
	if _, codex, err := store.FindStored("apikey:work"); err != nil {
		t.Fatal(err)
	} else if codex {
		t.Fatal("OpenRouter key was also stored in the Codex API-key pool")
	}
}

// Upstream (977e933) routed `sr add-key`, `sr status` and bare `sr` over HTTP
// to the loopback daemon's "serving store" and hid this machine's own disk
// rows. This fork's daemon and CLI share one store, and that routing was the
// box-only view PATCHES.md 19 removed, so it was not taken in the 2026-09
// merge: these commands act on this machine's store and never ask a loopback
// daemon for its accounts -- even one that is configured as the default server
// and would answer.
func TestLocalAccountCommandsUseThisMachinesStoreNotALoopbackServingAPI(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "cli-state")}
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email: "openrouter:disk-only", Provider: accounts.ProviderOpenRouter, AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "disk-placeholder"},
	}); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		http.Error(w, "the CLI must not ask the local daemon for its accounts", http.StatusTeapot)
	}))
	defer server.Close()
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", server.URL)
	cloudPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
	if err := os.WriteFile(cloudPath, []byte(`{"version":1,"baseUrl":"https://cmux.com","credentialSource":"local"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := defaultSRServerStore(store).update(func(file *srServerFile) error {
		file.Default = "local-candidate"
		file.Servers = []srServerConfig{{Name: "local-candidate", URL: server.URL, AccountImportToken: "import-token"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader("network\napi-key-placeholder\n"), out: &out, errOut: &out, client: server.Client()}
	if err := runner.run(t.Context(), []string{"add-key", "--provider", "openrouter"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.FindStored("openrouter:network"); err != nil || !ok {
		t.Fatalf("add-key did not land in this machine's store: found=%t err=%v", ok, err)
	}
	for _, args := range [][]string{{"status"}, nil} {
		out.Reset()
		if err := runner.run(t.Context(), args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if text := out.String(); !strings.Contains(text, "disk-only") || !strings.Contains(text, "network") {
			t.Fatalf("%v did not render this machine's accounts:\n%s", args, text)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("local account commands sent %d request(s) to the loopback daemon", requests.Load())
	}
}

func TestFreshLocalServingDaemonKeepsOnboardingOnLocalCommandPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{
		"SUBROUTER_ACCOUNT_IMPORT_TOKEN", "SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE",
		"SUBROUTER_ADMIN_TOKEN", "SUBROUTER_ADMIN_TOKEN_FILE",
	} {
		t.Setenv(name, "")
	}
	store := accounts.CodexStore{Dir: filepath.Join(home, ".subrouter", "codex", "accounts")}
	var nonHealthRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/_subrouter/health" {
			authorityID, err := accounts.StoreAuthorityID(store.Dir)
			if err != nil {
				t.Fatal(err)
			}
			proof := ""
			if challenge := request.Header.Get(accounts.StoreAuthorityChallengeHeader); challenge != "" {
				proof, err = accounts.StoreAuthorityProof(store.Dir, challenge)
				if err != nil {
					t.Fatal(err)
				}
			}
			_, _ = fmt.Fprintf(w, `{"ok":true,"account_import":"disabled","account_store_id":%q,"account_store_proof":%q}`, authorityID, proof)
			return
		}
		nonHealthRequests.Add(1)
		http.Error(w, "protected account import credential required", http.StatusUnauthorized)
	}))
	defer server.Close()
	attachPrivateLocalTestListener(t, server, store)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", server.URL)
	cloudPath := filepath.Join(home, "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
	if err := os.WriteFile(cloudPath, []byte(`{"version":1,"baseUrl":"https://cmux.com","credentialSource":"local"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runner := srRunner{
		store: store,
		in:    strings.NewReader("work\nsk-or-v1-test\n"), out: &out, errOut: &out,
		client: server.Client(),
	}
	if err := runner.run(t.Context(), []string{"add-key", "--provider", "openrouter"}); err != nil {
		t.Fatal(err)
	}
	if nonHealthRequests.Load() != 0 {
		t.Fatalf("fresh local onboarding sent %d request(s) to an uncredentialed serving API", nonHealthRequests.Load())
	}
	stored, ok, err := store.FindStored("openrouter:work")
	if err != nil || !ok {
		t.Fatalf("local onboarding account found=%t err=%v", ok, err)
	}
	if stored.Provider != accounts.ProviderOpenRouter || stored.Auth.OpenAIAPIKey != "sk-or-v1-test" {
		t.Fatalf("local onboarding stored provider=%q key_matches=%t", stored.Provider, stored.Auth.OpenAIAPIKey == "sk-or-v1-test")
	}
	out.Reset()
	if err := runner.run(t.Context(), []string{"kimi", "list"}); err != nil {
		t.Fatal(err)
	}
	if nonHealthRequests.Load() != 0 || !strings.Contains(out.String(), "No Kimi subscription accounts configured") {
		t.Fatalf("fresh local Kimi management contacted serving API or missed local state: requests=%d output=%q", nonHealthRequests.Load(), out.String())
	}
}

func TestLegacyLocalServingDaemonKeepsUnprotectedOnboardingOnLocalStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{
		"SUBROUTER_ACCOUNT_IMPORT_TOKEN", "SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE",
		"SUBROUTER_ADMIN_TOKEN", "SUBROUTER_ADMIN_TOKEN_FILE",
	} {
		t.Setenv(name, "")
	}
	store := accounts.CodexStore{Dir: filepath.Join(home, ".subrouter", "codex", "accounts")}
	var nonHealthRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/_subrouter/health" {
			nonHealthRequests.Add(1)
			http.Error(w, "account import is disabled", http.StatusUnauthorized)
			return
		}
		authorityID, err := accounts.StoreAuthorityID(store.Dir)
		if err != nil {
			t.Fatal(err)
		}
		proof := ""
		if challenge := request.Header.Get(accounts.StoreAuthorityChallengeHeader); challenge != "" {
			proof, err = accounts.StoreAuthorityProof(store.Dir, challenge)
			if err != nil {
				t.Fatal(err)
			}
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"account_import":"disabled","account_store_id":%q,"account_store_proof":%q}`, authorityID, proof)
	}))
	defer server.Close()
	attachPrivateLocalTestListener(t, server, store)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", server.URL)
	cloudPath := filepath.Join(home, "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
	if err := os.WriteFile(cloudPath, []byte(`{"version":1,"baseUrl":"https://cmux.com","credentialSource":"legacy"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := srRunner{
		store: store,
		in:    strings.NewReader("work\nsk-or-v1-local-only\n"), out: io.Discard, errOut: io.Discard,
		client: server.Client(),
	}
	if err := runner.run(t.Context(), []string{"add-key", "--provider", "openrouter"}); err != nil {
		t.Fatal(err)
	}
	if nonHealthRequests.Load() != 0 {
		t.Fatalf("unprotected legacy-local onboarding sent %d HTTP mutation request(s)", nonHealthRequests.Load())
	}
	stored, ok, err := store.FindStored("openrouter:work")
	if err != nil || !ok {
		t.Fatalf("legacy-local onboarding account found=%t err=%v", ok, err)
	}
	if stored.Provider != accounts.ProviderOpenRouter || stored.Auth.OpenAIAPIKey != "sk-or-v1-local-only" {
		t.Fatalf("legacy-local onboarding stored provider=%q key_matches=%t", stored.Provider, stored.Auth.OpenAIAPIKey == "sk-or-v1-local-only")
	}
}

func TestReadyLocalServingServerStartsColdDaemon(t *testing.T) {
	var healthy atomic.Bool
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "cli-state")}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/_subrouter/health" {
			http.NotFound(w, request)
			return
		}
		if !healthy.Load() {
			http.Error(w, "cold", http.StatusServiceUnavailable)
			return
		}
		authorityID, err := accounts.StoreAuthorityID(store.Dir)
		if err != nil {
			t.Fatal(err)
		}
		proof := ""
		if challenge := request.Header.Get(accounts.StoreAuthorityChallengeHeader); challenge != "" {
			proof, err = accounts.StoreAuthorityProof(store.Dir, challenge)
			if err != nil {
				t.Fatal(err)
			}
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"account_store_id":%q,"account_store_proof":%q}`, authorityID, proof)
	}))
	defer server.Close()
	attachPrivateLocalTestListener(t, server, store)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", server.URL)

	var starts atomic.Int32
	runner := srRunner{store: store, errOut: io.Discard, client: server.Client()}
	resolved, err := runner.readyLocalServingServer(t.Context(), func() error {
		starts.Add(1)
		healthy.Store(true)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 1 || !sameEndpoint(resolved.URL, server.URL) {
		t.Fatalf("ready local server = %+v starts=%d", resolved, starts.Load())
	}
}

func TestSRAddKeyForAnotherProviderDoesNotImportActiveCodexAuth(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	store := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
	storedAuth := testCodexAuth("isolated@example.com", "stored")
	storedAuth.LastRefresh = "2026-08-28T00:00:00Z"
	stored := accounts.StoredCodexAccount{
		Email:   "isolated@example.com",
		AddedAt: "2026-08-28T00:00:00Z",
		Auth:    storedAuth,
	}
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(stored.SourcePath(store))
	if err != nil {
		t.Fatal(err)
	}
	activeAuth := testCodexAuth("isolated@example.com", "active")
	activeAuth.LastRefresh = "2026-08-29T00:00:00Z"
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store: store, in: strings.NewReader("work\nsk-or-v1-test\n"),
		out: &out, errOut: &out,
	}
	if err := runner.run(t.Context(), []string{"add-key", "--provider", "openrouter"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(stored.SourcePath(store))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("unrelated provider add rewrote the isolated Codex credential")
	}
	persisted, ok, err := store.FindStored(stored.Email)
	if err != nil || !ok {
		t.Fatalf("stored Codex account found=%t err=%v", ok, err)
	}
	if persisted.Auth.Tokens.RefreshToken != storedAuth.Tokens.RefreshToken {
		t.Fatal("unrelated provider add replaced the isolated Codex token generation")
	}
	if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); err != nil {
		t.Fatalf("provider add did not publish its account generation: %v", err)
	}
}

func TestSRAddKeyRejectedInputDoesNotMutateCodexOrPublishGeneration(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		args  []string
	}{
		{name: "empty label", input: "\n", args: []string{"add-key", "--provider", "openrouter"}},
		{name: "empty key", input: "work\n\n", args: []string{"add-key", "--provider", "openrouter"}},
		{name: "invalid Codex key", input: "work\nnot-an-api-key\n", args: []string{"add-key"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", filepath.Join(root, "home"))
			store := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
			stored := accounts.StoredCodexAccount{
				Email:   "isolated@example.com",
				AddedAt: "2026-08-28T00:00:00Z",
				Auth:    testCodexAuth("isolated@example.com", "stored"),
			}
			if err := store.SaveStored(stored); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(stored.SourcePath(store))
			if err != nil {
				t.Fatal(err)
			}
			activeAuth := testCodexAuth("isolated@example.com", "active")
			activeAuth.LastRefresh = "2026-08-29T00:00:00Z"
			if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			runner := srRunner{store: store, in: strings.NewReader(test.input), out: &out, errOut: &out}
			if err := runner.run(t.Context(), test.args); err == nil {
				t.Fatal("rejected add-key input unexpectedly succeeded")
			}
			after, err := os.ReadFile(stored.SourcePath(store))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Fatal("rejected add-key input rewrote the isolated Codex credential")
			}
			if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); !os.IsNotExist(err) {
				t.Fatalf("rejected add-key input published an account generation: %v", err)
			}
		})
	}
}

func TestSRAddKeyStoresSharedSubscriptionUnderCredentialOwner(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader("work\ntest-provider-key\n"),
		out:    &out,
		errOut: &out,
	}

	if err := runner.run(context.Background(), []string{"add-key", "--provider", "qwen-anthropic"}); err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.FindStored("qwen-token:work")
	if err != nil || !ok {
		t.Fatalf("shared credential owner found=%t err=%v", ok, err)
	}
	if stored.Provider != accounts.ProviderQwenToken {
		t.Fatalf("stored provider = %q, want qwen-token", stored.Provider)
	}
	if _, duplicate, err := store.FindStored("qwen-anthropic:work"); err != nil || duplicate {
		t.Fatalf("protocol-specific duplicate exists=%t err=%v", duplicate, err)
	}
}

func TestSRAddKeyRejectsUnknownLocalProviderBeforePrompting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}

	err := runner.run(context.Background(), []string{"add-key", "--provider", "not/a/provider"})
	if err == nil || !strings.Contains(err.Error(), "unsupported API-key provider") {
		t.Fatalf("error = %v, want unsupported provider", err)
	}
	stored, listErr := store.ListStored()
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(stored) != 0 {
		t.Fatalf("invalid provider stored accounts: %+v", stored)
	}
}

func TestHostedAddKeyRejectsRegistryProviderInsteadOfReclassifyingIt(t *testing.T) {
	var out bytes.Buffer
	runner := srRunner{in: strings.NewReader(""), out: &out, errOut: &out}

	handled, err := runner.runTeamCredentialCommand(context.Background(), []string{"add-key", "--provider", "openrouter"})
	if !handled {
		t.Fatal("hosted add-key command was not handled")
	}
	if err == nil || !strings.Contains(err.Error(), "hosted credential storage does not support openrouter API keys") {
		t.Fatalf("error = %v, want explicit hosted OpenRouter rejection", err)
	}
}

func TestHostedAntigravityManagementFailsClosed(t *testing.T) {
	for _, command := range []string{"agy", "antigravity"} {
		handled, err := (srRunner{}).runTeamCredentialCommand(context.Background(), []string{command, "add", "work"})
		if !handled || err == nil || !strings.Contains(err.Error(), "hosted Antigravity profile management is not available") {
			t.Fatalf("%s handled=%v err=%v", command, handled, err)
		}
	}
}

func TestUsageRowsKeepProviderAPIKeysOutOfCodexSwitching(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	if _, _, err := store.AddProviderAPIKey(accounts.ProviderOpenRouter, "work", "sk-or-v1-test"); err != nil {
		t.Fatal(err)
	}
	runner := srRunner{store: store, client: http.DefaultClient}

	rows, err := runner.fetchUsageRows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].provider != accounts.ProviderOpenRouter {
		t.Fatalf("usage rows = %+v, want one OpenRouter row", rows)
	}
	if err := ensureUsageRowSwitchable(rows[0]); err == nil {
		t.Fatal("OpenRouter API key was switchable as an active Codex credential")
	}
}

func TestSRQwenKeyAccountsCanBeAddedListedAndRemoved(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.DefaultCodexStore()
	for _, input := range []string{
		"large-plan\nsk-sp-large-test\n",
		"small-plan\nsk-sp-small-test\n",
	} {
		var out bytes.Buffer
		runner := srRunner{store: store, in: strings.NewReader(input), out: &out, errOut: &out}
		if err := runner.run(context.Background(), []string{"add-key", "--provider", "qwen-token"}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "(sk-...)") {
			t.Fatalf("Qwen prompt claimed a Codex-specific key format: %s", out.String())
		}
	}
	stored, err := store.ListStored()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("stored Qwen accounts = %+v", stored)
	}
	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"list"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"qwen-token:large-plan", "qwen-token:small-plan"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("list is missing %q:\n%s", want, out.String())
		}
	}
	root := agentqwen.ConsoleRootForStore(store)
	for _, accountID := range []string{"qwen-token:large-plan", "qwen-token:small-plan"} {
		if err := agentqwen.SaveConsoleCredentialIn(root, accountID, agentqwen.ConsoleCredential{AccessToken: "console-secret"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := runner.run(context.Background(), []string{"remove", "qwen-token:small-plan"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); err != nil {
		t.Fatalf("Qwen removal did not publish account generation: %v", err)
	}
	if _, ok, err := store.FindStored("qwen-token:small-plan"); err != nil || ok {
		t.Fatalf("removed Qwen account remains: ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.FindStored("qwen-token:large-plan"); err != nil || !ok {
		t.Fatalf("unrelated Qwen account was removed: ok=%v err=%v", ok, err)
	}
	if _, err := agentqwen.ExportConsoleCredentialIn(root, "qwen-token:small-plan"); err == nil {
		t.Fatal("removed Qwen account retained its console credential")
	}
	if _, err := agentqwen.ExportConsoleCredentialIn(root, "qwen-token:large-plan"); err != nil {
		t.Fatalf("unrelated Qwen console credential was removed: %v", err)
	}
}

func TestSRQwenRemovalKeepsAccountWhenConsoleCredentialCannotBeRemoved(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	account, _, err := store.AddProviderAPIKey(accounts.ProviderQwenToken, "retry-safe", "sk-sp-test")
	if err != nil {
		t.Fatal(err)
	}
	root := agentqwen.ConsoleRootForStore(store)
	consoleDir := agentqwen.ConsoleConfigDirIn(root, account.Email)
	if err := os.MkdirAll(filepath.Dir(consoleDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(consoleDir, []byte("not a safe credential directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := srRunner{store: store, out: io.Discard, errOut: io.Discard}
	err = runner.remove(t.Context(), account.Email)
	if err == nil || !strings.Contains(err.Error(), "account remains and removal can be retried") {
		t.Fatalf("remove error = %v, want retryable console-cleanup failure", err)
	}
	if _, ok, findErr := store.FindStored(account.Email); findErr != nil || !ok {
		t.Fatalf("Qwen account disappeared before console cleanup: ok=%v err=%v", ok, findErr)
	}
	if body, readErr := os.ReadFile(consoleDir); readErr != nil || !strings.Contains(string(body), "credential") {
		t.Fatalf("failed cleanup changed console artifact: body=%q err=%v", body, readErr)
	}

	if err := os.Remove(consoleDir); err != nil {
		t.Fatal(err)
	}
	if err := runner.remove(t.Context(), account.Email); err != nil {
		t.Fatalf("retry removal: %v", err)
	}
	if _, ok, findErr := store.FindStored(account.Email); findErr != nil || ok {
		t.Fatalf("retried Qwen removal left account: ok=%v err=%v", ok, findErr)
	}
}

func TestQwenRemovalRetriesAfterAccountDeleteFailsPostConsoleCleanup(t *testing.T) {
	var calls []string
	removeConsole := func() error {
		calls = append(calls, "console")
		return nil
	}
	removeAttempts := 0
	removeStored := func() (accounts.StoredCodexAccount, bool, error) {
		calls = append(calls, "account")
		removeAttempts++
		if removeAttempts == 1 {
			return accounts.StoredCodexAccount{Email: "qwen-token:post-cleanup"}, false, errors.New("injected account delete failure")
		}
		return accounts.StoredCodexAccount{Email: "qwen-token:post-cleanup"}, true, nil
	}

	_, ok, err := removeQwenStoredAccount(removeConsole, removeStored)
	if err == nil || ok || !strings.Contains(err.Error(), "console credential removed but account remains; retry removal") {
		t.Fatalf("first removal = ok=%v err=%v, want retry guidance", ok, err)
	}
	if got := strings.Join(calls, ","); got != "console,account" {
		t.Fatalf("removal order = %s, want console before account", got)
	}
	removed, ok, err := removeQwenStoredAccount(removeConsole, removeStored)
	if err != nil || !ok || removed.Email != "qwen-token:post-cleanup" {
		t.Fatalf("retried removal = %+v ok=%v err=%v", removed, ok, err)
	}
	if got := strings.Join(calls, ","); got != "console,account,console,account" {
		t.Fatalf("retry order = %s", got)
	}
}

func TestAdditionalKeyedProvidersCanBeAddedListedAndRemoved(t *testing.T) {
	for _, provider := range []accounts.Provider{
		accounts.ProviderDeepSeek,
		accounts.ProviderTogether,
		accounts.ProviderFireworks,
		accounts.ProviderOpenCodeZen,
	} {
		t.Run(string(provider), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			store := accounts.DefaultCodexStore()
			var out bytes.Buffer
			runner := srRunner{
				store: store, in: strings.NewReader("primary\nprovider-test-key\n"),
				out: &out, errOut: &out,
			}
			if err := runner.run(t.Context(), []string{"add-key", "--provider", string(provider)}); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "provider-test-key") {
				t.Fatal("add-key output leaked the API key")
			}
			accountID := string(provider) + ":primary"
			stored, ok, err := store.FindStored(accountID)
			if err != nil || !ok || stored.Provider != provider {
				t.Fatalf("stored account = %+v ok=%v err=%v", stored, ok, err)
			}
			generationPath := filepath.Join(store.StoreDir(), ".account-generation")
			generationAfterAdd, err := os.ReadFile(generationPath)
			if err != nil || len(generationAfterAdd) == 0 {
				t.Fatalf("add did not publish account generation: %q err=%v", generationAfterAdd, err)
			}
			out.Reset()
			runner.in = strings.NewReader("")
			if err := runner.run(t.Context(), []string{"list"}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), accountID) {
				t.Fatalf("list omitted %q: %s", accountID, out.String())
			}
			if strings.Contains(out.String(), "provider-test-key") {
				t.Fatal("list output leaked the API key")
			}
			out.Reset()
			if err := runner.run(t.Context(), []string{"remove", accountID}); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := store.FindStored(accountID); err != nil || ok {
				t.Fatalf("removed account remains: ok=%v err=%v", ok, err)
			}
			generationAfterRemove, err := os.ReadFile(generationPath)
			if err != nil {
				t.Fatalf("read removal generation: %v", err)
			}
			if bytes.Equal(generationAfterAdd, generationAfterRemove) {
				t.Fatal("remove did not publish a new account generation")
			}
			if strings.Contains(out.String(), "provider-test-key") {
				t.Fatal("account lifecycle output leaked the API key")
			}
		})
	}
}

func TestAddKeyHelpListsEveryAdditionalKeyedProvider(t *testing.T) {
	var out bytes.Buffer
	runner := srRunner{store: accounts.DefaultCodexStore(), out: &out, errOut: &out}
	if err := runner.run(t.Context(), []string{"add-key", "--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("add-key --help error = %v, want flag.ErrHelp", err)
	}
	for _, provider := range []string{"deepseek", "together", "fireworks", "opencode-zen"} {
		if !strings.Contains(out.String(), provider) {
			t.Fatalf("add-key --help omits %q:\n%s", provider, out.String())
		}
		if !strings.Contains(srHelp, provider) {
			t.Fatalf("top-level sr help omits %q", provider)
		}
	}
}

func TestSRQwenHelpDocumentsGenericAccountLifecycle(t *testing.T) {
	var out bytes.Buffer
	runner := srRunner{out: &out}
	if err := runner.qwen(t.Context(), []string{"--help"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"sr add-key --provider qwen-token",
		"sr status",
		"sr remove <account>",
		"sr qwen login [--console-account <email-or-label>] <account>",
		"sr qwen label <account> <email-or-label>",
		"sr qwen proxy [qwen args...]",
		"console plan/quota metadata",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("Qwen help missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "sr qwen add") {
		t.Fatalf("Qwen help advertises a redundant add alias:\n%s", out.String())
	}
}

func TestKimiHelpSeparatesLocalLauncherFromRemoteManagement(t *testing.T) {
	var out bytes.Buffer
	runner := srRunner{out: &out}
	if err := runner.kimiCommand(t.Context(), []string{"--help"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"sr kimi [--account", "sr kimi proxy"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("local Kimi help missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if err := runner.kimiRemote(t.Context(), srServerConfig{Name: "remote"}, []string{"--help"}); err != nil {
		t.Fatal(err)
	}
	for _, localOnly := range []string{"--account", "sr kimi proxy"} {
		if strings.Contains(out.String(), localOnly) {
			t.Fatalf("remote Kimi help advertises local-only %q:\n%s", localOnly, out.String())
		}
	}
	if !strings.Contains(out.String(), "Managed profiles are stored on server remote") {
		t.Fatalf("remote Kimi help omits server-scoped management:\n%s", out.String())
	}
}

func TestHelpDistinguishesNativeProxyLaunchersFromDirectCLIs(t *testing.T) {
	for name, help := range map[string]string{
		"sr":        srHelp,
		"subrouter": usageText("subrouter"),
	} {
		for _, command := range []string{"qwen proxy", "kimi proxy"} {
			if !strings.Contains(help, command) {
				t.Errorf("%s help omits %q", name, command)
			}
		}
		nativeCommand := name + " agy"
		if strings.Contains(help, "agy proxy") || !strings.Contains(help, nativeCommand) || !strings.Contains(strings.ToLower(help), "pooled") {
			t.Errorf("%s help does not describe pooled AGY routing", name)
		}
		if !strings.Contains(help, "Gemini profiles (routing scaffold only)") {
			t.Errorf("%s help does not disclose Gemini's scaffold-only state", name)
		}
	}
}

func TestStatusHelpCoversEveryConfiguredProvider(t *testing.T) {
	for name, help := range map[string]string{
		"sr":        srHelp,
		"subrouter": usageText("subrouter"),
	} {
		if !strings.Contains(help, "status [--json]    Show usage across all configured providers") {
			t.Errorf("%s help does not describe provider-wide status", name)
		}
		if strings.Contains(help, "status             Show Codex and Claude usage") {
			t.Errorf("%s help still limits status to Codex and Claude", name)
		}
	}
}

func TestRemoteQwenKeyFingerprintRequiresSelectedAccount(t *testing.T) {
	statuses := []remoteServerUsageStatus{
		{ID: "qwen-token:other", KeyFingerprint: "key:1111111111"},
		{ID: "qwen-token:work", KeyFingerprint: "key:2222222222"},
	}
	got, err := remoteQwenKeyFingerprint(statuses, "qwen-token:work")
	if err != nil || got != "key:2222222222" {
		t.Fatalf("fingerprint = %q err=%v", got, err)
	}
	if _, err := remoteQwenKeyFingerprint(statuses, "qwen-token:missing"); err == nil {
		t.Fatal("missing selected remote Qwen account was accepted")
	}
	if _, err := remoteQwenKeyFingerprint([]remoteServerUsageStatus{{ID: "qwen-token:work"}}, "qwen-token:work"); err == nil {
		t.Fatal("empty remote Qwen fingerprint was accepted")
	}
}

func TestRemoteQwenConsoleRootIsTenantScopedWithoutExposingTenantKey(t *testing.T) {
	serverA := srServerConfig{Name: "shared", URL: "https://router.example", TenantKey: "srt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	serverB := srServerConfig{Name: "shared", URL: "https://router.example", TenantKey: "srt_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	rootA := qwenRemoteConsoleRoot(serverA)
	rootB := qwenRemoteConsoleRoot(serverB)
	if rootA == rootB {
		t.Fatal("different tenants reused one remote Qwen console credential root")
	}
	if strings.Contains(rootA, serverA.TenantKey) || strings.Contains(rootB, serverB.TenantKey) {
		t.Fatal("tenant key was exposed in a remote Qwen console credential path")
	}
}

func TestSRQwenRemovalRetainsAccountWhenCredentialCleanupFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.DefaultCodexStore()
	accountID := "qwen-token:work"
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:    accountID,
		Provider: accounts.ProviderQwenToken,
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "model-secret",
		},
	}); err != nil {
		t.Fatal(err)
	}
	root := agentqwen.ConsoleRootForStore(store)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), agentqwen.ConsoleConfigDirIn(root, accountID)); err != nil {
		t.Fatal(err)
	}
	runner := srRunner{store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	if err := runner.run(context.Background(), []string{"remove", accountID}); err == nil {
		t.Fatal("removal succeeded despite unsafe console credential directory")
	}
	if _, ok, err := store.FindStored(accountID); err != nil || !ok {
		t.Fatalf("Qwen account should remain retryable: ok=%v err=%v", ok, err)
	}
	if generation, err := os.ReadFile(filepath.Join(store.StoreDir(), ".account-generation")); err != nil || len(generation) == 0 {
		t.Fatalf("failed Qwen removal did not publish rollback invalidation: %q err=%v", generation, err)
	}
}

func TestSRQwenLoginPreparesBrowserAuthAndStoresIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:    "qwen-token:work",
		Provider: accounts.ProviderQwenToken,
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "model-secret",
		},
	}); err != nil {
		t.Fatal(err)
	}
	command := &qwenLoginCommandRunner{}
	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: command}
	if err := runner.run(context.Background(), []string{"qwen", "login", "--console-account", "person@example.com", "qwen-token:work"}); err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{"auth", "login", "--console", "--console-site", "international"}
	if command.name != "bl" || !slices.Equal(command.args, wantArgs) {
		t.Fatalf("Bailian command = %q %v", command.name, command.args)
	}
	if got := agentqwen.ConsoleAccount("qwen-token:work"); got != "person@example.com" {
		t.Fatalf("console account = %q", got)
	}
	body, err := os.ReadFile(agentqwen.ConsoleConfigPath("qwen-token:work"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "model-secret") || !strings.Contains(string(body), "console-secret") {
		t.Fatalf("console config did not strip the temporary model key: %s", body)
	}
}

func TestSRQwenReauthPreservesSavedConsoleIdentity(t *testing.T) {
	root := t.TempDir()
	accountID := "qwen-token:work"
	if err := agentqwen.SaveConsoleCredentialIn(root, accountID, agentqwen.ConsoleCredential{
		AccessToken: "expired-console-token",
		Account:     "person@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	command := &qwenLoginCommandRunner{}
	runner := srRunner{in: strings.NewReader(""), out: io.Discard, errOut: io.Discard, cmd: command}
	stored := accounts.StoredCodexAccount{
		Email: accountID, Provider: accounts.ProviderQwenToken,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "model-secret"},
	}
	// This is the exact no-flag command suggested by an expired-login status.
	if err := runner.qwenLoginStored(t.Context(), root, stored, "", nil); err != nil {
		t.Fatal(err)
	}
	credential, err := agentqwen.ExportConsoleCredentialIn(root, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccessToken != "console-secret" || credential.Account != "person@example.com" {
		t.Fatalf("reauthorized credential = %+v", credential)
	}
}

func TestSRQwenLoginPreservesCompletedAuthorizationWhenDurableSaveFails(t *testing.T) {
	root := filepath.Join(t.TempDir(), "blocked-root")
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := &qwenLoginCommandRunner{}
	runner := srRunner{in: strings.NewReader(""), out: io.Discard, errOut: io.Discard, cmd: command}
	stored := accounts.StoredCodexAccount{
		Email: "qwen-token:work", Provider: accounts.ProviderQwenToken,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "model-secret"},
	}
	err := runner.qwenLoginStored(t.Context(), root, stored, "person@example.com", nil)
	if err == nil || !strings.Contains(err.Error(), "authorization preserved at") {
		t.Fatalf("login error = %v, want preserved-authorization location", err)
	}
	stageRoot := filepath.Dir(command.configDir)
	t.Cleanup(func() { _ = os.RemoveAll(stageRoot) })
	credential, exportErr := agentqwen.ExportConsoleCredentialIn(stageRoot, stored.Email)
	if exportErr != nil {
		t.Fatalf("completed staged authorization was removed: %v", exportErr)
	}
	if credential.AccessToken != "console-secret" || credential.Account != "person@example.com" {
		t.Fatalf("preserved credential = %+v", credential)
	}
}

func TestQwenConsoleCredentialSyncsToExplicitRemote(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Remote paths exist only on a keyed machine (PATCHES 17).
	unlockRemoteGateForTest(t)
	store := accounts.DefaultCodexStore()
	if err := agentqwen.SaveConsoleCredential("qwen-token:work", agentqwen.ConsoleCredential{
		AccessToken:   "console-secret",
		ConsoleRegion: "ap-southeast-1",
		ConsoleSite:   "international",
		Account:       "person@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	received := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/_subrouter/qwen-console" || req.Header.Get("Authorization") != "Bearer admin-secret" {
			t.Error("Qwen sync used an unexpected path or credential")
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		var payload struct {
			AccountID  string                      `json:"account_id"`
			Credential agentqwen.ConsoleCredential `json:"credential"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Error("Qwen sync payload could not be decoded")
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		if payload.AccountID != "qwen-token:work" || payload.Credential.AccessToken != "console-secret" {
			t.Error("Qwen sync payload omitted expected account data")
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		received = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	if err := defaultSRServerStore(store).save(srServerFile{Default: "test", Servers: []srServerConfig{{Name: "test", URL: server.URL, AdminToken: "admin-secret"}}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_SERVER", "test")
	var out bytes.Buffer
	runner := srRunner{store: store, out: &out, errOut: &out, client: server.Client()}
	if err := runner.syncQwenConsoleToSelectedRemote(context.Background(), "qwen-token:work"); err != nil {
		t.Fatal(err)
	}
	if !received || !strings.Contains(out.String(), "Synced Qwen quota authorization to server: test") {
		t.Fatalf("sync output = %q received=%v", out.String(), received)
	}
}

func TestQwenConsoleCredentialSyncNeverFollowsRedirects(t *testing.T) {
	root := t.TempDir()
	if err := agentqwen.SaveConsoleCredentialIn(root, "qwen-token:work", agentqwen.ConsoleCredential{
		AccessToken: "console-secret", ConsoleRegion: "ap-southeast-1", ConsoleSite: "international",
	}); err != nil {
		t.Fatal(err)
	}
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	runner := srRunner{client: source.Client(), out: io.Discard}
	err := runner.syncQwenConsoleToServer(t.Context(), root, srServerConfig{
		Name: "team", URL: source.URL, AdminToken: "admin-secret",
	}, "qwen-token:work")
	if err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("Qwen redirect error = %v", err)
	}
	if redirected.Load() != 0 {
		t.Fatalf("Qwen credential request followed redirect %d time(s)", redirected.Load())
	}
}

func TestQwenConsoleCredentialSyncRedactsTenantKeyFromTransportError(t *testing.T) {
	root := t.TempDir()
	if err := agentqwen.SaveConsoleCredentialIn(root, "qwen-token:work", agentqwen.ConsoleCredential{
		AccessToken: "console-secret", ConsoleRegion: "ap-southeast-1", ConsoleSite: "international",
	}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	client := server.Client()
	server.Close()
	const tenantKey = "srt_tenant_secret"
	runner := srRunner{client: client, out: io.Discard}
	err := runner.syncQwenConsoleToServer(t.Context(), root, srServerConfig{
		Name: "tenant", URL: server.URL, TenantKey: tenantKey,
	}, "qwen-token:work")
	if err == nil {
		t.Fatal("sync unexpectedly succeeded against a closed server")
	}
	if strings.Contains(err.Error(), tenantKey) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("sync transport error exposed tenant credential: %v", err)
	}
}

func TestSRQwenLoginTargetsSelectedRemoteAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Remote paths exist only on a keyed machine (PATCHES 17).
	unlockRemoteGateForTest(t)
	store := accounts.DefaultCodexStore()
	accountID := "qwen-token:work"
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:    accountID,
		Provider: accounts.ProviderQwenToken,
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "model-secret",
		},
	}); err != nil {
		t.Fatal(err)
	}
	received := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer admin-secret" {
			t.Fatalf("authorization = %q", req.Header.Get("Authorization"))
		}
		switch req.URL.Path {
		case "/_subrouter/accounts":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id": accountID, "provider": "qwen-token", "auth_mode": "apikey",
			}})
		case "/_subrouter/usage-status":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id": accountID, "provider": "qwen-token", "auth_mode": "apikey",
				"key_fingerprint": accounts.APIKeyFingerprint("sk-sp-remote-test"),
			}})
		case "/_subrouter/qwen-console":
			received = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, req)
		}
	}))
	defer server.Close()
	if err := defaultSRServerStore(store).save(srServerFile{Default: "test", Servers: []srServerConfig{{Name: "test", URL: server.URL, AdminToken: "admin-secret"}}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_CODEX_SERVER", "test")
	command := &qwenLoginCommandRunner{}
	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader("sk-sp-remote-test\n"), out: &out, errOut: &out, client: server.Client(), cmd: command}
	if err := runner.run(context.Background(), []string{"qwen", "login", "--console-account", "person@example.com", accountID}); err != nil {
		t.Fatal(err)
	}
	if !received || !strings.Contains(out.String(), "Synced Qwen quota authorization to server: test") {
		t.Fatalf("sync output = %q received=%v", out.String(), received)
	}
}

type qwenLoginCommandRunner struct {
	name      string
	args      []string
	configDir string
}

func (r *qwenLoginCommandRunner) Run(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return r.RunWithEnv(ctx, name, args, nil, stdin, stdout, stderr)
}

func (r *qwenLoginCommandRunner) RunWithEnv(_ context.Context, name string, args, env []string, _ io.Reader, _, _ io.Writer) error {
	r.name = name
	r.args = append([]string(nil), args...)
	var configDir string
	for _, value := range env {
		if strings.HasPrefix(value, "BAILIAN_CONFIG_DIR=") {
			configDir = strings.TrimPrefix(value, "BAILIAN_CONFIG_DIR=")
		}
	}
	r.configDir = configDir
	body, err := os.ReadFile(filepath.Join(configDir, "config.json"))
	if err != nil {
		return err
	}
	var config map[string]any
	if err := json.Unmarshal(body, &config); err != nil {
		return err
	}
	config["access_token"] = "console-secret"
	body, err = json.Marshal(config)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(configDir, "config.json"), body, 0o600)
}

func (r *qwenLoginCommandRunner) Output(context.Context, string, []string) ([]byte, error) {
	return nil, nil
}

func TestSRTraceShowsOAuthBreadcrumbs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "a@example.com",
		AddedAt: "2026-04-28T00:00:00Z",
		Auth: accounts.CodexAuthFile{AuthMode: "chatgpt", Tokens: &accounts.CodexTokens{
			AccessToken:  "access-token",
			RefreshToken: "raw-refresh-token",
			IDToken:      "id-token",
			AccountID:    "acct_123",
		}},
		Breadcrumbs: []accounts.CodexAuthBreadcrumb{{
			At:              "2026-05-19T08:00:00Z",
			Event:           "refresh_terminal_failure",
			Source:          "oauth_refresh",
			Reason:          "proxy.score-accounts",
			Host:            "subrouter-team",
			PID:             123,
			PPID:            1,
			Executable:      "/usr/local/bin/subrouter",
			WorkingDir:      "/var/lib/subrouter",
			SourcePath:      "/var/lib/subrouter/codex/accounts/a@example.com.json",
			RefreshFP:       "refreshfp",
			OldRefreshFP:    "oldrefreshfp",
			StatusCode:      http.StatusUnauthorized,
			ProviderType:    "invalid_request_error",
			ProviderCode:    "refresh_token_reused",
			ProviderMessage: "Your refresh token has already been used to generate a new access token. Please try signing in again.",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"trace", "a@example.com"}); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	for _, want := range []string{
		"OAuth breadcrumbs for a@example.com",
		"refresh_terminal_failure",
		"reason=\"proxy.score-accounts\"",
		"host=\"subrouter-team\"",
		"pid=\"123\"",
		"refresh=\"refreshfp\"",
		"old_refresh=\"oldrefreshfp\"",
		"provider_code=\"refresh_token_reused\"",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("trace output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "raw-refresh-token") {
		t.Fatalf("trace output leaked raw refresh token:\n%s", got)
	}
}

func TestSRSwitchAPIKeyWritesCodexAuthJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	store := accounts.DefaultCodexStore()
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "apikey:paid",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "sk-test",
			Tokens:       &accounts.CodexTokens{IDToken: ""},
		},
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"switch", "paid"}); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var active map[string]any
	if err := json.Unmarshal(body, &active); err != nil {
		t.Fatal(err)
	}
	if active["auth_mode"] != "apikey" {
		t.Fatalf("auth_mode = %v, want apikey", active["auth_mode"])
	}
	if active["OPENAI_API_KEY"] != "sk-test" {
		t.Fatalf("OPENAI_API_KEY = %v, want sk-test", active["OPENAI_API_KEY"])
	}
	if _, ok := active["tokens"]; ok {
		t.Fatal("tokens should be stripped for API-key auth")
	}
	var openCodeAuth map[string]map[string]any
	readJSONFile(t, filepath.Join(home, ".local", "share", "opencode", "auth.json"), &openCodeAuth)
	if got := openCodeAuth["openai"]["type"]; got != "api" {
		t.Fatalf("opencode API-key type = %v, want api", got)
	}
	if got := openCodeAuth["openai"]["key"]; got != "sk-test" {
		t.Fatalf("opencode API key was not synced")
	}
	var piAuth map[string]map[string]any
	readJSONFile(t, filepath.Join(home, ".pi", "agent", "auth.json"), &piAuth)
	if got := piAuth["openai"]["type"]; got != "api_key" {
		t.Fatalf("pi API-key type = %v, want api_key", got)
	}
	if got := piAuth["openai"]["key"]; got != "sk-test" {
		t.Fatalf("pi API key was not synced")
	}
	if !strings.Contains(out.String(), "Switched to apikey:paid") {
		t.Fatalf("missing switch confirmation:\n%s", out.String())
	}
}

func TestSRSwitchPublishesOAuthIsolationDowngradeToRunningServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	auth := testCodexAuth("isolated@example.test", "acct_isolated")
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:                 "isolated@example.test",
		AddedAt:               time.Now().UTC().Format(time.RFC3339),
		OAuthCredentialOrigin: accounts.CodexOAuthOriginIsolatedServerLogin,
		Auth:                  auth,
	}); err != nil {
		t.Fatal(err)
	}

	servingStore := store
	servingStore.DisableActiveAuthSync = true
	servingStore.RequireIsolatedOAuth = true
	ref, err := proxy.OpenAccountRef(servingStore, agentclaude.Store{Dir: filepath.Join(home, "claude-store")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	beforeGeneration := ref.Generation()
	handler := (proxy.Server{AccountRef: ref}).Handler()

	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.switchAccount(context.Background(), "isolated@example.test", srSwitchOptions{}); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "http://subrouter.local/_subrouter/accounts", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	// Loopback trust needs a loopback Host (PATCHES §29), as the sr CLI sends.
	request.Host = "127.0.0.1:31415"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("account reload status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if got := ref.Generation(); got <= beforeGeneration {
		t.Fatalf("running server generation = %d, want > %d after switch", got, beforeGeneration)
	}

	stored, ok, err := store.FindStored("isolated@example.test")
	if err != nil || !ok {
		t.Fatalf("stored account found = %v, err = %v", ok, err)
	}
	if stored.OAuthCredentialOrigin != accounts.CodexOAuthOriginInteractiveImport {
		t.Fatalf("stored OAuth origin = %q, want interactive import", stored.OAuthCredentialOrigin)
	}
	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil || !ok {
		t.Fatalf("active auth found = %v, err = %v", ok, err)
	}
	if active.Tokens == nil || active.Tokens.RefreshToken != auth.Tokens.RefreshToken {
		t.Fatal("active auth does not contain switched credential")
	}

	loaded := ref.All()
	if len(loaded) != 1 {
		t.Fatalf("running server account count = %d, want one", len(loaded))
	}
	_, refreshErr := ref.Refresh(context.Background(), loaded[0])
	var unisolated *accounts.CodexUnisolatedCredentialError
	if !errors.As(refreshErr, &unisolated) {
		t.Fatalf("running server refresh error = %v, want isolation rejection", refreshErr)
	}
}

func TestSRSwitchDoesNotWriteActiveAuthOrDowngradeWhenPublicationFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	store := accounts.DefaultCodexStore()
	account := accounts.StoredCodexAccount{
		Email:                 "isolated@example.test",
		AddedAt:               time.Now().UTC().Format(time.RFC3339),
		OAuthCredentialOrigin: accounts.CodexOAuthOriginIsolatedServerLogin,
		Auth:                  testCodexAuth("isolated@example.test", "acct_isolated"),
	}
	if err := store.SaveStored(account); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(store.StoreDir(), ".account-generation"), 0o700); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.switchAccount(context.Background(), account.Email, srSwitchOptions{}); err == nil {
		t.Fatal("switch unexpectedly succeeded when generation publication failed")
	}
	stored, ok, err := store.FindStored(account.Email)
	if err != nil || !ok {
		t.Fatalf("stored account found = %v, err = %v", ok, err)
	}
	if stored.OAuthCredentialOrigin != accounts.CodexOAuthOriginIsolatedServerLogin {
		t.Fatalf("stored OAuth origin = %q, want isolated server login", stored.OAuthCredentialOrigin)
	}
	if _, err := os.Stat(accounts.DefaultCodexAuthPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active auth was written despite publication failure: %v", err)
	}
}

func TestSRSwitchDoesNotSyncActiveAuthWhenPublicationFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	account := accounts.StoredCodexAccount{
		Email:                 "isolated@example.test",
		AddedAt:               time.Now().UTC().Format(time.RFC3339),
		OAuthCredentialOrigin: accounts.CodexOAuthOriginIsolatedServerLogin,
		Auth:                  testCodexAuth("isolated@example.test", "acct_isolated"),
	}
	if err := store.SaveStored(account); err != nil {
		t.Fatal(err)
	}
	interactive := testCodexAuth("isolated@example.test", "acct_interactive")
	if err := accounts.WriteActiveCodexAuth(interactive); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(store.StoreDir(), ".account-generation"), 0o700); err != nil {
		t.Fatal(err)
	}

	runner := srRunner{store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	if err := runner.switchAccount(context.Background(), account.Email, srSwitchOptions{}); err == nil {
		t.Fatal("switch unexpectedly succeeded when active-sync publication failed")
	}
	stored, ok, err := store.FindStored(account.Email)
	if err != nil || !ok {
		t.Fatalf("stored account found = %v, err = %v", ok, err)
	}
	if stored.OAuthCredentialOrigin != accounts.CodexOAuthOriginIsolatedServerLogin {
		t.Fatalf("stored OAuth origin = %q, want isolated server login", stored.OAuthCredentialOrigin)
	}
	if stored.Auth.Tokens == nil || stored.Auth.Tokens.RefreshToken != account.Auth.Tokens.RefreshToken {
		t.Fatal("active auth was imported despite publication failure")
	}
	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil || !ok || active.Tokens == nil || active.Tokens.RefreshToken != interactive.Tokens.RefreshToken {
		t.Fatal("publication failure changed the pre-existing active auth")
	}
}

func TestSRSwitchDoesNotRefreshStoredAuthWhenPublicationFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	auth := testCodexAuth("expired@example.test", "acct_expired")
	auth.Tokens.AccessToken = testJWT(map[string]any{
		"exp": time.Now().Add(-time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_expired",
		},
	})
	account := accounts.StoredCodexAccount{
		Email:   "expired@example.test",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    auth,
	}
	if err := store.SaveStored(account); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(store.StoreDir(), ".account-generation"), 0o700); err != nil {
		t.Fatal(err)
	}
	refreshRequests := 0
	client := &http.Client{Transport: srRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "auth.openai.com" && request.URL.Path == "/oauth/token" {
			refreshRequests++
		}
		return nil, errors.New("refresh endpoint should not be called")
	})}

	runner := srRunner{store: store, client: client, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	if err := runner.switchAccount(context.Background(), account.Email, srSwitchOptions{}); err == nil {
		t.Fatal("switch unexpectedly succeeded when refresh publication failed")
	}
	if refreshRequests != 0 {
		t.Fatalf("refresh endpoint requests = %d, want zero", refreshRequests)
	}
	stored, ok, err := store.FindStored(account.Email)
	if err != nil || !ok {
		t.Fatalf("stored account found = %v, err = %v", ok, err)
	}
	if stored.Auth.Tokens == nil || stored.Auth.Tokens.RefreshToken != account.Auth.Tokens.RefreshToken {
		t.Fatal("stored credential changed despite publication failure")
	}
}

func TestSRSwitchDoesNotActivateStaleAuthAfterCommittedRefreshTeardownFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()

	activeAccount := accounts.StoredCodexAccount{
		Email:   "active@example.test",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    testCodexAuth("active@example.test", "acct_active"),
	}
	if err := store.SaveStored(activeAccount); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WriteActiveCodexAuth(activeAccount.Auth); err != nil {
		t.Fatal(err)
	}

	staleAuth := testCodexAuth("target@example.test", "acct_target")
	staleAuth.Tokens.AccessToken = testJWT(map[string]any{
		"exp": time.Now().Add(-time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_target",
		},
	})
	target := accounts.StoredCodexAccount{
		Email:   "target@example.test",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    staleAuth,
	}
	if err := store.SaveStored(target); err != nil {
		t.Fatal(err)
	}

	freshAccess := testJWT(map[string]any{
		"exp": time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_target",
		},
	})
	freshID := testJWT(map[string]any{
		"exp":                         time.Now().Add(time.Hour).Unix(),
		"email":                       target.Email,
		"https://api.openai.com/auth": map[string]any{"chatgpt_user_id": "user:" + target.Email, "chatgpt_account_id": "acct_target"},
	})
	client := &http.Client{Transport: srRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "auth.openai.com" && request.URL.Path == "/oauth/token" {
			return srJSONResponse(request, http.StatusOK, fmt.Sprintf(
				`{"access_token":%q,"refresh_token":%q,"id_token":%q}`,
				freshAccess, "fresh-refresh", freshID,
			)), nil
		}
		return nil, errors.New("usage unavailable")
	})}

	teardownErr := errors.New("close account publication transaction")
	var out bytes.Buffer
	runner := srRunner{
		store: store, client: client, in: strings.NewReader(""), out: &out, errOut: &out,
		withCodexRefreshPublication: func(
			ctx context.Context,
			storeDir string,
			mutate func(func() error) error,
		) error {
			if err := proxy.WithAccountDiskMutationPublication(ctx, storeDir, mutate); err != nil {
				return err
			}
			return teardownErr
		},
	}

	err := runner.switchAccount(context.Background(), target.Email, srSwitchOptions{})
	if !errors.Is(err, teardownErr) {
		t.Fatalf("switch error = %v, want committed-refresh teardown error", err)
	}
	stored, ok, err := store.FindStored(target.Email)
	if err != nil || !ok {
		t.Fatalf("stored target found = %v, err = %v", ok, err)
	}
	if stored.Auth.Tokens == nil || stored.Auth.Tokens.RefreshToken != "fresh-refresh" {
		t.Fatalf("stored refresh token = %v, want committed fresh credential", stored.Auth.Tokens)
	}
	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil || !ok {
		t.Fatalf("active auth found = %v, err = %v", ok, err)
	}
	if active.Tokens == nil || active.Tokens.RefreshToken != activeAccount.Auth.Tokens.RefreshToken {
		t.Fatal("failed switch replaced active auth after refresh transaction teardown")
	}
	if _, err := os.Stat(filepath.Join(store.StoreDir(), ".account-generation")); err != nil {
		t.Fatalf("committed refresh generation was not published: %v", err)
	}
	if strings.Contains(out.String(), "Switched to") {
		t.Fatalf("failed switch reported success:\n%s", out.String())
	}
}
func TestSRSwitchSyncsOpenCodeAndPiAuth(t *testing.T) {
	home := t.TempDir()
	xdgData := t.TempDir()
	piDir := filepath.Join(home, "pi-agent")
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", xdgData)
	t.Setenv("PI_CODING_AGENT_DIR", piDir)

	access := testJWT(map[string]any{
		"exp": time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_test",
		},
	})
	id := testJWT(map[string]any{
		"exp":   time.Now().Add(time.Hour).Unix(),
		"email": "sync@example.com",
	})
	store := accounts.DefaultCodexStore()
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "sync@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth: accounts.CodexAuthFile{Tokens: &accounts.CodexTokens{
			AccessToken:  access,
			RefreshToken: "refresh-test",
			IDToken:      id,
		}},
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"switch", "sync@example.com"}); err != nil {
		t.Fatal(err)
	}

	var openCodeAuth map[string]map[string]any
	readJSONFile(t, filepath.Join(xdgData, "opencode", "auth.json"), &openCodeAuth)
	if got := openCodeAuth["openai"]["type"]; got != "oauth" {
		t.Fatalf("opencode openai type = %v, want oauth", got)
	}
	if got := openCodeAuth["openai"]["access"]; got != access {
		t.Fatalf("opencode access was not synced")
	}
	if got := openCodeAuth["openai"]["accountId"]; got != "acct_test" {
		t.Fatalf("opencode accountId = %v, want acct_test", got)
	}

	var piAuth map[string]map[string]any
	readJSONFile(t, filepath.Join(piDir, "auth.json"), &piAuth)
	if got := piAuth["openai-codex"]["type"]; got != "oauth" {
		t.Fatalf("pi openai-codex type = %v, want oauth", got)
	}
	if got := piAuth["openai-codex"]["access"]; got != access {
		t.Fatalf("pi access was not synced")
	}
	if got := piAuth["openai-codex"]["accountId"]; got != "acct_test" {
		t.Fatalf("pi accountId = %v, want acct_test", got)
	}
}

// Adding an account must not write to the machine's Codex home at all. The
// login is what revokes a session, so "restore it afterwards" is not good
// enough: by the time the restore runs the displaced account has already been
// superseded server-side. Asserting the active auth file was never touched is
// the only check that distinguishes an isolated login from a clobber-and-repair.
// The new record must also carry upstream's isolated-login provenance, or the
// serving daemon (RequireIsolatedOAuth) refuses it on its first request.
func TestSRAddUsesIsolatedLoginAndLeavesActiveAccountUnchanged(t *testing.T) {
	home := t.TempDir()
	xdgData := t.TempDir()
	piDir := filepath.Join(home, "pi-agent")
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", xdgData)
	t.Setenv("PI_CODING_AGENT_DIR", piDir)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("alice@example.com", "acct_active")
	addedAuth := testCodexAuth("founders@example.com", "acct_added")
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "alice@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    activeAuth,
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}
	installFakeCodexLogin(t, home, addedAuth)

	activePath := accounts.DefaultCodexAuthPath()
	before, err := os.Stat(activePath)
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"add", "codex"}); err != nil {
		t.Fatal(err)
	}

	after, err := os.Stat(activePath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("active auth file was rewritten during add (%s -> %s); the login was not isolated",
			before.ModTime(), after.ModTime())
	}

	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing active auth")
	}
	email, err := accounts.ExtractEmailFromJWT(active.Tokens.IDToken)
	if err != nil {
		t.Fatal(err)
	}
	if email != "alice@example.com" {
		t.Fatalf("active email = %q, want alice@example.com", email)
	}
	added, found, err := store.FindStored("founders@example.com")
	if err != nil {
		t.Fatal(err)
	} else if !found {
		t.Fatal("newly logged-in account was not imported")
	}
	if added.OAuthCredentialOrigin != accounts.CodexOAuthOriginIsolatedServerLogin {
		t.Fatalf("OAuth origin = %q, want isolated server login", added.OAuthCredentialOrigin)
	}
	if !strings.Contains(out.String(), "Local Codex auth was left unchanged") {
		t.Fatalf("missing isolation message:\n%s", out.String())
	}
}

// The fork used to make the FIRST added account active on a fresh machine,
// writing it into ~/.codex/auth.json. Under the isolated-login model (owner
// decision 1 of the 2026-09 merge) that file belongs to the human's own Codex
// login: a pooled chain written there is shared with the Codex app, and the
// serving daemon then refuses it as shared_active_refresh_token. So even a
// fresh machine's first add leaves no active auth behind.
func TestSRAddOnFreshMachineLeavesNoActiveCodexAuth(t *testing.T) {
	home := t.TempDir()
	xdgData := t.TempDir()
	piDir := filepath.Join(home, "pi-agent")
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", xdgData)
	t.Setenv("PI_CODING_AGENT_DIR", piDir)

	store := accounts.DefaultCodexStore()
	addedAuth := testCodexAuth("founders@example.com", "acct_added")
	installFakeCodexLogin(t, home, addedAuth)

	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"add", "codex"}); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := accounts.ReadActiveCodexAuth(); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("the first added account was written into ~/.codex/auth.json")
	}
	added, found, err := store.FindStored("founders@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("newly logged-in account was not imported")
	}
	if added.OAuthCredentialOrigin != accounts.CodexOAuthOriginIsolatedServerLogin {
		t.Fatalf("OAuth origin = %q, want isolated server login", added.OAuthCredentialOrigin)
	}
}

// Upstream's bare `sr` switched automatically when the active Codex login was
// exhausted. A switch exports the target's refresh-token chain to ~/.codex,
// OpenCode and pi and demotes pool records out of serving, which the
// isolated-Codex model (decision 1) allows only on an explicit request. So
// bare `sr` recommends and changes nothing; the answer typed at the prompt is
// the explicit switch.
func TestSRRecommendsButDoesNotSwitchWhenActiveCodexLoginIsExhausted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, ".pi", "agent"))

	store := accounts.DefaultCodexStore()
	// alice's pool chain and ~/.codex are separate logins (isolated model), so
	// ~/.codex holds a personal chain of alice's account.
	poolAlice := testCodexAuth("alice@example.com", "acct_active")
	poolAlice.Tokens.RefreshToken = "pool-isolated-alice"
	personalAlice := testCodexAuth("alice@example.com", "acct_active")
	personalAlice.Tokens.RefreshToken = "personal-chain-alice"
	poolBob := testCodexAuth("bob@example.com", "acct_best")
	poolBob.Tokens.RefreshToken = "pool-isolated-bob"
	for email, auth := range map[string]accounts.CodexAuthFile{
		"alice@example.com": poolAlice,
		"bob@example.com":   poolBob,
	} {
		if err := store.SaveStored(accounts.StoredCodexAccount{
			Email:                 email,
			AddedAt:               time.Now().UTC().Format(time.RFC3339),
			Auth:                  auth,
			OAuthCredentialOrigin: accounts.CodexOAuthOriginIsolatedServerLogin,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := accounts.WriteActiveCodexAuth(personalAlice); err != nil {
		t.Fatal(err)
	}
	personal := [...]string{
		filepath.Join(home, ".codex", "auth.json"),
		opencode.DefaultAuthPath(),
		pi.DefaultAuthPath(),
	}
	read := func(path string) string {
		body, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return "<missing>"
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	var before [len(personal)]string
	for i, path := range personal {
		before[i] = read(path)
	}

	usageByToken := map[string]float64{
		poolAlice.Tokens.AccessToken:     100,
		personalAlice.Tokens.AccessToken: 100,
		poolBob.Tokens.AccessToken:       1,
	}
	newRunner := func(in string, out *bytes.Buffer) srRunner {
		return srRunner{
			program: "sr",
			store:   store,
			in:      strings.NewReader(in),
			out:     out,
			errOut:  out,
			client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				used := usageByToken[strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")]
				return usageResponse(used), nil
			})},
		}
	}

	var out bytes.Buffer
	if err := newRunner("", &out).run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for i, path := range personal {
		if after := read(path); after != before[i] {
			t.Fatalf("bare sr rewrote %s:\nbefore %q\nafter  %q", path, before[i], after)
		}
	}
	for email, refresh := range map[string]string{
		"alice@example.com": "pool-isolated-alice",
		"bob@example.com":   "pool-isolated-bob",
	} {
		record, found, err := store.FindStored(email)
		if err != nil || !found {
			t.Fatalf("%s: found=%v err=%v", email, found, err)
		}
		if record.OAuthCredentialOrigin != accounts.CodexOAuthOriginIsolatedServerLogin {
			t.Fatalf("bare sr demoted %s: origin %q", email, record.OAuthCredentialOrigin)
		}
		if record.Auth.Tokens == nil || record.Auth.Tokens.RefreshToken != refresh {
			t.Fatalf("bare sr replaced %s's pool chain: %+v", email, record.Auth.Tokens)
		}
	}
	if strings.Contains(out.String(), "Auto-switched") || strings.Contains(out.String(), "Switched to") {
		t.Fatalf("bare sr switched on its own:\n%s", out.String())
	}
	for _, want := range []string{
		"Active Codex login alice@example.com is exhausted; recommended: bob@example.com",
		"takes it out of serving",
		"`sr switch bob@example.com`",
		"Switch to (#):",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}

	// Answering the prompt IS the explicit switch.
	out.Reset()
	if err := newRunner("bob@example.com\n", &out).run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil || !ok {
		t.Fatalf("active auth: ok=%v err=%v", ok, err)
	}
	email, err := accounts.ExtractEmailFromJWT(active.Tokens.IDToken)
	if err != nil {
		t.Fatal(err)
	}
	if email != "bob@example.com" || !strings.Contains(out.String(), "Switched to bob@example.com") {
		t.Fatalf("the typed answer did not switch: active=%q\n%s", email, out.String())
	}
}

// The recommendation is about ~/.codex, so only the active CODEX row counts: an
// exhausted active Claude profile says nothing about it.
func TestRecommendOverExhaustedActiveReadsOnlyTheActiveCodexRow(t *testing.T) {
	oauth := accounts.AuthModeOAuth
	bob := srUsageRow{email: "bob@example.com", provider: accounts.ProviderCodex, authMode: oauth, gtoRecommended: true,
		score: selectacct.Score{AccountID: "bob@example.com", Headroom: 0.9, ShortHeadroom: 0.9}}
	claudeSpent := srUsageRow{email: "work", provider: accounts.ProviderClaude, authMode: oauth, active: true,
		score: selectacct.Score{AccountID: "work"}}
	aliceFresh := srUsageRow{email: "alice@example.com", provider: accounts.ProviderCodex, authMode: oauth, active: true,
		score: selectacct.Score{AccountID: "alice@example.com", Headroom: 0.8, ShortHeadroom: 0.8}}
	aliceSpent := srUsageRow{email: "alice@example.com", provider: accounts.ProviderCodex, authMode: oauth, active: true,
		score: selectacct.Score{AccountID: "alice@example.com"}}
	// gtoRecommended marks one row per provider heading: with every Codex row
	// spent, the recommended row is a Claude profile ~/.codex cannot hold, and
	// the recommendation must not name it (recommendedForNewSession refuses
	// non-Codex rows).
	claudeFresh := srUsageRow{email: "spare", provider: accounts.ProviderClaude, authMode: oauth, gtoRecommended: true,
		score: selectacct.Score{AccountID: "spare", Headroom: 0.9, ShortHeadroom: 0.9}}
	for _, tc := range []struct {
		name string
		rows []srUsageRow
		want string
	}{
		{"spent Claude profile, no active Codex login", []srUsageRow{claudeSpent, bob}, ""},
		{"spent Claude profile, fresh Codex login", []srUsageRow{claudeSpent, aliceFresh, bob}, ""},
		{"spent Codex login", []srUsageRow{claudeSpent, aliceSpent, bob}, "Active Codex login alice@example.com is exhausted; recommended: bob@example.com"},
		{"spent Codex login, only a Claude profile recommended", []srUsageRow{aliceSpent, claudeFresh}, ""},
		{"spent Codex login, Claude recommended first", []srUsageRow{claudeFresh, aliceSpent, bob}, "recommended: bob@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			srRunner{program: "sr", out: &out}.recommendOverExhaustedActive(tc.rows)
			if tc.want == "" && out.Len() != 0 {
				t.Fatalf("recommended over a login that is not spent: %q", out.String())
			}
			if tc.want != "" && !strings.Contains(out.String(), tc.want) {
				t.Fatalf("output %q lacks %q", out.String(), tc.want)
			}
		})
	}
}

// An empty pool plus a personal ~/.codex login: status, statusOne, bare `sr`
// and pick must never pull that login into the pool (upstream auto-imported it
// in all four). `sr import` stays the explicit way.
func TestSRViewsNeverImportThePersonalCodexLogin(t *testing.T) {
	// `sr <email>` is statusOne.
	for _, args := range [][]string{{"status"}, {"alice@example.com"}, nil, {"pick"}, {"switch"}} {
		t.Run(strings.Join(append([]string{"sr"}, args...), " "), func(t *testing.T) {
			root := t.TempDir()
			home := filepath.Join(root, "home")
			t.Setenv("HOME", home)
			t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
			t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(root, "state"))
			t.Setenv("SUBROUTER_CLOUD_CONFIG", filepath.Join(root, "cloud.json"))
			t.Setenv("SUBROUTER_SESSIONS", "")
			if err := accounts.WriteActiveCodexAuth(testCodexAuth("alice@example.com", "acct_personal")); err != nil {
				t.Fatal(err)
			}
			store := accounts.CodexStore{Dir: filepath.Join(root, "state", "codex", "accounts")}
			var out bytes.Buffer
			runner := srRunner{
				program: "sr",
				store:   store,
				in:      strings.NewReader(""),
				out:     &out,
				errOut:  &out,
				kimi:    fakeKimiUsageStore{},
				grok:    &fakeGrokStore{},
				client: &http.Client{Transport: srRoundTripFunc(func(*http.Request) (*http.Response, error) {
					return usageResponse(1), nil
				})},
			}
			// pick and `sr <email>` refuse an empty pool; the error is not the point.
			_ = runner.run(context.Background(), args)
			stored, err := store.ListStored()
			if err != nil {
				t.Fatal(err)
			}
			if len(stored) != 0 {
				t.Fatalf("sr %v imported the personal ~/.codex login into the pool: %+v\n%s", args, stored, out.String())
			}
			if strings.Contains(out.String(), "Auto-imported") {
				t.Fatalf("sr %v reported an import:\n%s", args, out.String())
			}
		})
	}
}

func TestSRPickSwitchesToRecommendedAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("alice@example.com", "acct_active")
	bestAuth := testCodexAuth("bob@example.com", "acct_best")
	for email, auth := range map[string]accounts.CodexAuthFile{
		"alice@example.com": activeAuth,
		"bob@example.com":   bestAuth,
	} {
		if err := store.SaveStored(accounts.StoredCodexAccount{
			Email:   email,
			AddedAt: time.Now().UTC().Format(time.RFC3339),
			Auth:    auth,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	usageByToken := map[string]float64{
		activeAuth.Tokens.AccessToken: 80,
		bestAuth.Tokens.AccessToken:   1,
	}
	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			used := usageByToken[strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")]
			return usageResponse(used), nil
		})},
	}

	if err := runner.run(context.Background(), []string{"pick"}); err != nil {
		t.Fatal(err)
	}

	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing active auth")
	}
	email, err := accounts.ExtractEmailFromJWT(active.Tokens.IDToken)
	if err != nil {
		t.Fatal(err)
	}
	if email != "bob@example.com" {
		t.Fatalf("active email = %q, want bob@example.com", email)
	}
	if !strings.Contains(out.String(), "Picked recommended account: bob@example.com") {
		t.Fatalf("missing pick confirmation:\n%s", out.String())
	}
	if strings.Contains(out.String(), "alice@example.com") {
		t.Fatalf("successful pick should only display the picked row:\n%s", out.String())
	}
}

func TestSRPickSucceedsWhenRecommendedActiveHasQuota(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("alice@example.com", "acct_active")
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "alice@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    activeAuth,
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return usageResponseWindows(20, 20), nil
		})},
	}

	if err := runner.run(context.Background(), []string{"pick"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Already using recommended account: alice@example.com") {
		t.Fatalf("missing already using confirmation:\n%s", out.String())
	}
}

func TestSRPickFailsWhenNoRecommendedAccountHasQuota(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("alice@example.com", "acct_active")
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "alice@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    activeAuth,
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return usageResponseWindows(100, 100), nil
		})},
	}

	err := runner.run(context.Background(), []string{"pick"})
	if err == nil || !strings.Contains(err.Error(), "no recommended account has quota") {
		t.Fatalf("err = %v, want no quota failure", err)
	}
	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing active auth")
	}
	email, err := accounts.ExtractEmailFromJWT(active.Tokens.IDToken)
	if err != nil {
		t.Fatal(err)
	}
	if email != "alice@example.com" {
		t.Fatalf("active email = %q, want alice@example.com", email)
	}
}

func TestSRDefaultShowsCookedAccountAndDoesNotPromptWhenAllCooked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("alice@example.com", "acct_active")
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "alice@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    activeAuth,
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return usageResponseWindows(0, 100), nil
		})},
	}

	if err := runner.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if !strings.Contains(got, "All of your OAuth accounts are cooked") {
		t.Fatalf("missing all-cooked warning:\n%s", got)
	}
	if !strings.Contains(got, "active, cooked") || !strings.Contains(got, "cooked, canno") {
		t.Fatalf("missing cooked row state:\n%s", got)
	}
	if strings.Contains(got, "Switch to (#):") {
		t.Fatalf("should not prompt when all OAuth accounts are cooked:\n%s", got)
	}
}

func TestSRDefaultShowsTemporarilyCookedAccountWhenShortWindowConsumed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("alice@example.com", "acct_active")
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "alice@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    activeAuth,
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader("\n"),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return usageResponseWindows(100, 55), nil
		})},
	}

	if err := runner.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if !strings.Contains(got, "active, temp") {
		t.Fatalf("missing temporarily cooked marker:\n%s", got)
	}
	if !strings.Contains(got, "temp cooked") {
		t.Fatalf("missing temporarily cooked reason:\n%s", got)
	}
	if strings.Contains(got, "active, cooked") || strings.Contains(got, "All of your OAuth accounts are cooked") {
		t.Fatalf("short-window saturation should not be treated as permanently cooked:\n%s", got)
	}
	if strings.Contains(got, "recommended") {
		t.Fatalf("temporarily cooked account should not be recommended:\n%s", got)
	}
	if strings.Contains(got, "Switch to (#):") {
		t.Fatalf("should not prompt when every OAuth account is unavailable for new sessions:\n%s", got)
	}
}

func TestModelScopedShortWindowDoesNotTemporarilyCookWholeAccount(t *testing.T) {
	windows := []accounts.UsageWindow{
		{Name: "primary", UsedPercent: 28, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
		{
			Name:               "GPT-5.3-Codex-Spark/primary",
			Feature:            "GPT-5.3-Codex-Spark",
			UsedPercent:        100,
			LimitWindowSeconds: int64((5 * time.Hour) / time.Second),
			ResetAfterSeconds:  int64((4 * time.Hour) / time.Second),
		},
		{
			Name:               "GPT-5.3-Codex-Spark/secondary",
			Feature:            "GPT-5.3-Codex-Spark",
			UsedPercent:        48,
			LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second),
		},
	}

	tempCooked, reason := tempCookedFromWindows(windows)
	if tempCooked {
		t.Fatalf("account should remain switchable when only a model-scoped short window is exhausted (reason=%q)", reason)
	}
}

func TestSRInteractiveRefusesTemporarilyCookedSelection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("active@example.com", "acct_active")
	tempCookedAuth := testCodexAuth("temp@example.com", "acct_temp")
	for email, auth := range map[string]accounts.CodexAuthFile{
		"active@example.com": activeAuth,
		"temp@example.com":   tempCookedAuth,
	} {
		if err := store.SaveStored(accounts.StoredCodexAccount{
			Email:   email,
			AddedAt: time.Now().UTC().Format(time.RFC3339),
			Auth:    auth,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader("2\n"),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
			if token == tempCookedAuth.Tokens.AccessToken {
				return usageResponseWindows(100, 20), nil
			}
			return usageResponseWindows(0, 20), nil
		})},
	}

	err := runner.run(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "account is temporarily cooked") {
		t.Fatalf("err = %v, want temporarily cooked refusal", err)
	}

	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing active auth")
	}
	email, err := accounts.ExtractEmailFromJWT(active.Tokens.IDToken)
	if err != nil {
		t.Fatal(err)
	}
	if email != "active@example.com" {
		t.Fatalf("active email = %q, want active@example.com", email)
	}
}

func TestSRInteractiveRefusesCookedSelection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("active@example.com", "acct_active")
	cookedAuth := testCodexAuth("cooked@example.com", "acct_cooked")
	for email, auth := range map[string]accounts.CodexAuthFile{
		"active@example.com": activeAuth,
		"cooked@example.com": cookedAuth,
	} {
		if err := store.SaveStored(accounts.StoredCodexAccount{
			Email:   email,
			AddedAt: time.Now().UTC().Format(time.RFC3339),
			Auth:    auth,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader("2\n"),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
			if token == cookedAuth.Tokens.AccessToken {
				return usageResponseWindows(0, 100), nil
			}
			return usageResponseWindows(0, 0), nil
		})},
	}

	err := runner.run(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "account is cooked") {
		t.Fatalf("err = %v, want cooked refusal", err)
	}

	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing active auth")
	}
	email, err := accounts.ExtractEmailFromJWT(active.Tokens.IDToken)
	if err != nil {
		t.Fatal(err)
	}
	if email != "active@example.com" {
		t.Fatalf("active email = %q, want active@example.com", email)
	}
}

func TestSRSwitchRefusesCookedAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := accounts.DefaultCodexStore()
	activeAuth := testCodexAuth("active@example.com", "acct_active")
	cookedAuth := testCodexAuth("cooked@example.com", "acct_cooked")
	for email, auth := range map[string]accounts.CodexAuthFile{
		"active@example.com": activeAuth,
		"cooked@example.com": cookedAuth,
	} {
		if err := store.SaveStored(accounts.StoredCodexAccount{
			Email:   email,
			AddedAt: time.Now().UTC().Format(time.RFC3339),
			Auth:    auth,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := accounts.WriteActiveCodexAuth(activeAuth); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return usageResponseWindows(0, 100), nil
		})},
	}

	err := runner.run(context.Background(), []string{"switch", "cooked@example.com"})
	if err == nil || !strings.Contains(err.Error(), "account is cooked") {
		t.Fatalf("err = %v, want cooked refusal", err)
	}

	active, ok, err := accounts.ReadActiveCodexAuth()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("missing active auth")
	}
	email, err := accounts.ExtractEmailFromJWT(active.Tokens.IDToken)
	if err != nil {
		t.Fatal(err)
	}
	if email != "active@example.com" {
		t.Fatalf("active email = %q, want active@example.com", email)
	}
}

func TestSRRankRowsKeepsAPIKeyAsFallback(t *testing.T) {
	rows := []srUsageRow{
		{
			email:    "apikey:paid",
			authMode: accounts.AuthModeAPIKey,
			score:    selectacct.Score{AccountID: "apikey:paid", Headroom: 1, ShortHeadroom: 1},
		},
		{
			email:    "later@example.com",
			authMode: accounts.AuthModeOAuth,
			score: selectacct.Score{
				AccountID:              "later@example.com",
				Headroom:               1,
				ShortHeadroom:          1,
				ShortResetAfterSeconds: int64((5 * time.Hour) / time.Second),
				ExpiryPressure:         1 / float64((5*time.Hour)/time.Second),
			},
		},
		{
			email:    "soon@example.com",
			authMode: accounts.AuthModeOAuth,
			score: selectacct.Score{
				AccountID:              "soon@example.com",
				Headroom:               0.90,
				ShortHeadroom:          0.90,
				ShortResetAfterSeconds: int64((2 * time.Hour) / time.Second),
				ExpiryPressure:         0.90 / float64((2*time.Hour)/time.Second),
			},
		},
	}

	rankUsageRows(rows)

	if got := recommendedUsageRowEmails(rows); !slices.Equal(got, []string{"soon@example.com"}) {
		t.Fatalf("rec rows = %v, want only soon@example.com, never the API key", got)
	}
}

func recommendedUsageRowEmails(rows []srUsageRow) []string {
	var emails []string
	for _, row := range rows {
		if row.gtoRecommended {
			emails = append(emails, row.email)
		}
	}
	return emails
}

func TestSRRankRowsFallsBackToAPIKeyBeforeExhaustedOAuth(t *testing.T) {
	rows := []srUsageRow{
		{
			email:    "empty@example.com",
			authMode: accounts.AuthModeOAuth,
			score:    selectacct.Score{AccountID: "empty@example.com", Headroom: 0, ShortHeadroom: 0},
		},
		{
			email:    "apikey:paid",
			authMode: accounts.AuthModeAPIKey,
			score:    selectacct.Score{AccountID: "apikey:paid", Headroom: 0.01, ShortHeadroom: 0.01},
		},
	}

	rankUsageRows(rows)

	if got := recommendedUsageRowEmails(rows); !slices.Equal(got, []string{"apikey:paid"}) {
		t.Fatalf("rec rows = %v, want the API-key fallback when OAuth is exhausted", got)
	}
}

func TestSRRankRowsRecommendsNeitherCookedNorTemporarilyCooked(t *testing.T) {
	rows := []srUsageRow{
		{
			email:        "cooked@example.com",
			authMode:     accounts.AuthModeOAuth,
			score:        selectacct.Score{AccountID: "cooked@example.com", Headroom: 0, ShortHeadroom: 1, ShortResetAfterSeconds: int64((5 * time.Hour) / time.Second)},
			cooked:       true,
			cookedReason: "7d limit fully consumed",
		},
		{
			email:            "temp@example.com",
			authMode:         accounts.AuthModeOAuth,
			score:            selectacct.Score{AccountID: "temp@example.com", Headroom: 0, ShortHeadroom: 0, ShortResetAfterSeconds: int64((3 * time.Hour) / time.Second)},
			tempCooked:       true,
			tempCookedReason: "5h limit fully consumed",
		},
	}

	rankUsageRows(rows)

	if got := recommendedUsageRowEmails(rows); len(got) != 0 {
		t.Fatalf("rec rows = %v, want none: both are unavailable", got)
	}
}

func TestSRRankRowsGroupsProvidersSeparately(t *testing.T) {
	rows := []srUsageRow{
		{
			email:    "claude-inactive@example.com",
			authMode: accounts.AuthModeOAuth,
			provider: accounts.ProviderClaude,
			score:    selectacct.Score{AccountID: "claude-inactive@example.com", Headroom: 1, ShortHeadroom: 1},
		},
		{
			email:    "codex-low@example.com",
			authMode: accounts.AuthModeOAuth,
			provider: accounts.ProviderCodex,
			score:    selectacct.Score{AccountID: "codex-low@example.com", Headroom: 0.3, ShortHeadroom: 0.3},
		},
		{
			email:    "claude-active@example.com",
			active:   true,
			authMode: accounts.AuthModeOAuth,
			provider: accounts.ProviderClaude,
			score:    selectacct.Score{AccountID: "claude-active@example.com", Headroom: 0.5, ShortHeadroom: 0.5},
		},
		{
			email:    "codex-high@example.com",
			authMode: accounts.AuthModeOAuth,
			provider: accounts.ProviderCodex,
			score:    selectacct.Score{AccountID: "codex-high@example.com", Headroom: 0.9, ShortHeadroom: 0.9},
		},
	}

	rankUsageRows(rows)

	got := []string{rows[0].email, rows[1].email, rows[2].email, rows[3].email}
	want := []string{"codex-high@example.com", "codex-low@example.com", "claude-active@example.com", "claude-inactive@example.com"}
	if !slices.Equal(got, want) {
		t.Fatalf("ranked rows = %#v, want %#v", got, want)
	}
	if !rows[0].gtoRecommended {
		t.Fatal("best Codex row should still be recommended")
	}
	if rows[2].gtoRecommended || rows[3].gtoRecommended {
		t.Fatalf("Claude rows should not be marked as Codex recommendations: %#v", rows)
	}
}

func TestSRStatusShowsATelemetryThrottledCookedClaudeRowAsCooked(t *testing.T) {
	row := srUsageRow{
		email: "cooked@example.com", provider: accounts.ProviderClaude,
		authMode: accounts.AuthModeOAuth, usageThrottled: true, cooked: true,
		score: selectacct.Score{AccountID: "cooked@example.com", Headroom: 0, ShortHeadroom: 0},
	}
	if got := usageGridState(row); got != "cooked" {
		t.Fatalf("cooked Claude state = %q, want cooked", got)
	}
}

func TestSRRankRowsMovesCheckedAuthFailureRowsToLowSignalGroup(t *testing.T) {
	rows := []srUsageRow{
		{email: "auth-failed", provider: accounts.ProviderCodex, authMode: accounts.AuthModeOAuth, authChecked: true, authValid: false,
			score: selectacct.Score{AccountID: "auth-failed", Headroom: 1, ShortHeadroom: 1}},
		{email: "healthy", provider: accounts.ProviderCodex, authMode: accounts.AuthModeOAuth, authChecked: true, authValid: true,
			score: selectacct.Score{AccountID: "healthy", Headroom: 0.5, ShortHeadroom: 0.5}},
	}

	rankUsageRows(rows)
	if got := []string{rows[0].email, rows[1].email}; !slices.Equal(got, []string{"healthy", "auth-failed"}) {
		t.Fatalf("ranked rows = %#v, want healthy then auth-failed", got)
	}
}

func TestSRRankRowsMovesProviderFailureRowsToLowSignalGroup(t *testing.T) {
	rows := []srUsageRow{
		{email: "bad-key", provider: accounts.ProviderCodex, authMode: accounts.AuthModeAPIKey, providerHealth: "invalid key",
			score: selectacct.Score{AccountID: "bad-key", Headroom: 1, ShortHeadroom: 1}},
		{email: "healthy", provider: accounts.ProviderCodex, authMode: accounts.AuthModeAPIKey, providerHealth: "auth ok",
			score: selectacct.Score{AccountID: "healthy", Headroom: 0.5, ShortHeadroom: 0.5}},
	}

	rankUsageRows(rows)
	if got := []string{rows[0].email, rows[1].email}; !slices.Equal(got, []string{"healthy", "bad-key"}) {
		t.Fatalf("ranked rows = %#v, want healthy then bad-key", got)
	}
}

func TestSRRankRowsMovesReauthRowsToLowSignalGroup(t *testing.T) {
	rows := []srUsageRow{
		{
			email: "needs-reauth@example.com", provider: accounts.ProviderCodex,
			authMode: accounts.AuthModeOAuth, err: errors.New("401 Unauthorized"),
			score: selectacct.Score{AccountID: "needs-reauth@example.com", Headroom: 1, ShortHeadroom: 1},
		},
		{
			email: "healthy@example.com", provider: accounts.ProviderCodex,
			authMode: accounts.AuthModeOAuth,
			score:    selectacct.Score{AccountID: "healthy@example.com", Headroom: 0.5, ShortHeadroom: 0.5},
		},
	}

	rankUsageRows(rows)
	if rows[0].email != "healthy@example.com" || rows[1].email != "needs-reauth@example.com" {
		t.Fatalf("ranked rows = %#v, want healthy then reauth", rows)
	}
	if usageRowStatusTier(rows[1]) <= usageRowStatusTier((srUsageRow{usageThrottled: true})) {
		t.Fatal("reauth rows should remain below telemetry-throttled rows")
	}
}

// weekUsageRow is an OAuth row with a 10%-used 5h window and a weekly window
// weeklyUsed% used that resets in weeklyReset (0: no reset time reported).
func weekUsageRow(provider accounts.Provider, email string, weeklyUsed float64, weeklyReset time.Duration) srUsageRow {
	short, long := "primary", "secondary"
	if provider == accounts.ProviderClaude {
		short, long = "5h", "7d"
	}
	windows := []accounts.UsageWindow{
		{Name: short, UsedPercent: 10, LimitWindowSeconds: int64(5 * time.Hour / time.Second), ResetAfterSeconds: int64(2 * time.Hour / time.Second)},
		{Name: long, UsedPercent: weeklyUsed, LimitWindowSeconds: int64(7 * 24 * time.Hour / time.Second), ResetAfterSeconds: int64(weeklyReset / time.Second)},
	}
	return srUsageRow{email: email, provider: provider, authMode: accounts.AuthModeOAuth, windows: windows, score: scoreFromWindows(email, windows)}
}

func requireListedInOrder(t *testing.T, text string, names ...string) {
	t.Helper()
	last := -1
	for _, name := range names {
		at := strings.Index(text, name)
		if at < 0 || at < last {
			t.Fatalf("want rows listed as %v:\n%s", names, text)
		}
		last = at
	}
}

var recStateMarker = regexp.MustCompile(`\brec\b`)

func TestSRStatusListsEachHeadingByWeeklyRollLongestFirst(t *testing.T) {
	day := 24 * time.Hour
	rows := []srUsageRow{
		weekUsageRow(accounts.ProviderCodex, "cx-1h", 10, time.Hour),
		weekUsageRow(accounts.ProviderClaude, "cl-3d", 30, 3*day),
		weekUsageRow(accounts.ProviderCodex, "cx-6d", 50, 6*day),
		weekUsageRow(accounts.ProviderClaude, "cl-1h", 10, time.Hour),
		weekUsageRow(accounts.ProviderCodex, "cx-3d", 30, 3*day),
		weekUsageRow(accounts.ProviderClaude, "cl-6d", 50, 6*day),
	}

	rankUsageRows(rows)
	var out bytes.Buffer
	displayUsageRows(&out, rows, true)
	text := out.String()

	requireListedInOrder(t, text, "cx-6d", "cx-3d", "cx-1h", "cl-6d", "cl-3d", "cl-1h")
	// The recommendation is unchanged by the display order: the Codex account
	// with the most headroom whose week rolls soonest, now listed last.
	var recommended []string
	for _, line := range strings.Split(text, "\n") {
		if recStateMarker.MatchString(line) {
			recommended = append(recommended, strings.TrimSpace(line))
		}
	}
	if len(recommended) != 1 || !strings.Contains(recommended[0], "cx-1h") {
		t.Fatalf("rows marked rec = %q, want only cx-1h:\n%s", recommended, text)
	}
	if target := recommendedUsableUsageRow(rows); target == nil || target.email != "cx-1h" {
		t.Fatalf("sr pick target = %+v, want cx-1h", target)
	}
}

func TestSRStatusListsRowsWithoutAWeeklyResetBelowAndErrorsLast(t *testing.T) {
	day := 24 * time.Hour
	broken := weekUsageRow(accounts.ProviderCodex, "broken", 10, 6*day)
	broken.err = errors.New("401 Unauthorized")
	authFailed := weekUsageRow(accounts.ProviderCodex, "auth-failed", 10, time.Hour)
	authFailed.authChecked = true
	noWeek := weekUsageRow(accounts.ProviderCodex, "no-week", 0, 0)
	noWeek.windows = noWeek.windows[:1]
	rows := []srUsageRow{
		authFailed,
		broken,
		noWeek,
		weekUsageRow(accounts.ProviderCodex, "unknown-roll", 30, 0),
		weekUsageRow(accounts.ProviderCodex, "tie-less-left", 70, 2*day),
		weekUsageRow(accounts.ProviderCodex, "tie-more-left", 20, 2*day),
	}

	rankUsageRows(rows)
	var out bytes.Buffer
	displayUsageRows(&out, rows, false)

	requireListedInOrder(t, out.String(), "tie-more-left", "tie-less-left", "unknown-roll", "no-week", "broken", "auth-failed")
}

func TestScoreFromWindowsUsesAllDisplayedRateLimits(t *testing.T) {
	score := scoreFromWindows("a@example.com", []accounts.UsageWindow{
		{Name: "primary", UsedPercent: 10, LimitWindowSeconds: int64((5 * time.Hour) / time.Second), ResetAfterSeconds: int64((3 * time.Hour) / time.Second)},
		{Name: "secondary", UsedPercent: 10, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAfterSeconds: int64((6 * 24 * time.Hour) / time.Second)},
		{Name: "GPT-5.3-Codex-Spark (weekly)", UsedPercent: 100, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAfterSeconds: int64((2 * 24 * time.Hour) / time.Second)},
	})

	if score.Headroom != 0 {
		t.Fatalf("headroom = %.2f, want 0", score.Headroom)
	}
}

func TestRenderBarSupportsANSIColors(t *testing.T) {
	plain := renderBar(25, false)
	if !strings.HasPrefix(plain, "[") || !strings.HasSuffix(plain, "]") {
		t.Fatalf("plain bar = %q", plain)
	}

	colored := renderBar(25, true)
	if !strings.Contains(colored, ansiBGGreen) || !strings.Contains(colored, ansiBGGray) || !strings.HasSuffix(colored, ansiReset) {
		t.Fatalf("colored bar missing ANSI segments: %q", colored)
	}
}

func TestDisplayUsageRowsGridWhenForced(t *testing.T) {
	t.Setenv("COLUMNS", "200")
	var out bytes.Buffer
	displayUsageRows(&out, []srUsageRow{
		{
			email:          "lawrence@cmux.com",
			planType:       "pro",
			active:         true,
			gtoRecommended: true,
			gtoReason:      "45% bottleneck left, 5h resets in 1h",
			authMode:       accounts.AuthModeOAuth,
			windows: []accounts.UsageWindow{
				{Name: "primary", UsedPercent: 55, LimitWindowSeconds: int64((5 * time.Hour) / time.Second), ResetAfterSeconds: int64(time.Hour / time.Second)},
				{Name: "secondary", UsedPercent: 20, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAfterSeconds: int64((4 * 24 * time.Hour) / time.Second)},
				{Name: "GPT-5.3-Codex-Spark/primary", Feature: "GPT-5.3-Codex-Spark", UsedPercent: 8, LimitWindowSeconds: int64((5 * time.Hour) / time.Second), ResetAfterSeconds: int64((30 * time.Minute) / time.Second)},
				{Name: "GPT-5.3-Codex-Spark/secondary", Feature: "GPT-5.3-Codex-Spark", UsedPercent: 2, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAfterSeconds: int64((6 * 24 * time.Hour) / time.Second)},
			},
			credits:            &accounts.CreditsInfo{Balance: "0"},
			complimentaryReset: &accounts.ComplimentaryResetInfo{Known: true, Consumed: true},
		},
	}, true)

	got := out.String()
	if !strings.Contains(got, "Account") || !strings.Contains(got, "1x reset") {
		t.Fatalf("grid header missing:\n%s", got)
	}
	if strings.Contains(got, "Spark") {
		t.Fatalf("grid should not render retired Spark columns:\n%s", got)
	}
	if !strings.Contains(got, "lawrence@cmux.com") || !strings.Contains(got, "active rec") || !strings.Contains(got, "used") {
		t.Fatalf("grid row missing state:\n%s", got)
	}
	if strings.Contains(got, "pick") {
		t.Fatalf("forced grid should not render detailed pick label:\n%s", got)
	}
	lines := strings.Split(got, "\n")
	separator := ""
	for _, line := range lines {
		if strings.Contains(line, "───") {
			separator = line
			break
		}
	}
	if separator == "" || strings.Contains(separator, "===") || strings.Contains(separator, "---") {
		t.Fatalf("grid separator should be a thin Unicode rule:\n%s", got)
	}
}

func TestUsageGridCreditsHidesPlaceholderZero(t *testing.T) {
	if got := usageGridCreditsCell(srUsageRow{credits: &accounts.CreditsInfo{Balance: "0"}}); got.Text != "" {
		t.Fatalf("placeholder credits = %q, want hidden", got.Text)
	}
	if got := usageGridCreditsCell(srUsageRow{credits: &accounts.CreditsInfo{Balance: "0", HasCredits: true}}); got.Text != "$0" {
		t.Fatalf("real zero balance = %q, want $0", got.Text)
	}
	if got := usageGridCreditsCell(srUsageRow{credits: &accounts.CreditsInfo{Balance: "12.50"}}); got.Text != "$12.50" {
		t.Fatalf("non-zero balance = %q, want $12.50", got.Text)
	}
}

func TestUsageRowStaleAgeUsesProviderObservationTime(t *testing.T) {
	row := srUsageRow{displayAccount: "stale@example.com", usageFetchedAt: time.Now().Add(-3 * time.Minute)}
	age, ok := usageRowStaleAge(row)
	if !ok || age != "3m ago" {
		t.Fatalf("stale age = %q, %v; want 3m ago, true", age, ok)
	}
	row.usageFetchedAt = time.Now().Add(-time.Minute)
	if _, ok := usageRowStaleAge(row); ok {
		t.Fatal("recent provider observation was marked stale")
	}
}

func TestDisplayUsageRowsDoesNotPrintStaleUsageNote(t *testing.T) {
	var out bytes.Buffer
	displayUsageRows(&out, []srUsageRow{{
		displayAccount: "stale@example.com",
		provider:       accounts.ProviderCodex,
		authMode:       accounts.AuthModeOAuth,
		usageFetchedAt: time.Now().Add(-3 * time.Minute),
		windows: []accounts.UsageWindow{{
			Name: "primary", UsedPercent: 50, LimitWindowSeconds: 7 * 24 * 60 * 60,
		}},
	}}, false)
	if got := out.String(); strings.Contains(got, "usage last fetched") || strings.Contains(got, "showing last known values") {
		t.Fatalf("stale usage note should be omitted:\n%s", got)
	}
}

func TestUsageGridResetCellStates(t *testing.T) {
	ineligible := false
	cases := []struct {
		name string
		row  srUsageRow
		want string
	}{
		{
			name: "unknown",
			row:  srUsageRow{authMode: accounts.AuthModeOAuth, provider: accounts.ProviderCodex},
			want: "unknown",
		},
		{
			name: "available",
			row: srUsageRow{
				authMode:           accounts.AuthModeOAuth,
				provider:           accounts.ProviderCodex,
				complimentaryReset: &accounts.ComplimentaryResetInfo{Known: true, Available: true},
			},
			want: "avail",
		},
		{
			name: "consumed",
			row: srUsageRow{
				authMode:           accounts.AuthModeOAuth,
				provider:           accounts.ProviderCodex,
				complimentaryReset: &accounts.ComplimentaryResetInfo{Known: true, Consumed: true},
			},
			want: "used",
		},
		{
			name: "ineligible",
			row: srUsageRow{
				authMode:           accounts.AuthModeOAuth,
				provider:           accounts.ProviderCodex,
				complimentaryReset: &accounts.ComplimentaryResetInfo{Known: true, Eligible: &ineligible},
			},
			want: "not elig",
		},
		{
			name: "api key blank",
			row: srUsageRow{
				authMode:           accounts.AuthModeAPIKey,
				provider:           accounts.ProviderCodex,
				complimentaryReset: &accounts.ComplimentaryResetInfo{Known: true, Available: true},
			},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := usageGridResetCell(tc.row).Text; got != tc.want {
				t.Fatalf("reset cell = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDisplayUsageRowsGridGroupsProviders(t *testing.T) {
	t.Setenv("COLUMNS", "200")
	var out bytes.Buffer
	displayUsageRows(&out, []srUsageRow{
		{
			email:    "codex@example.com",
			planType: "pro",
			authMode: accounts.AuthModeOAuth,
			provider: accounts.ProviderCodex,
			score:    selectacct.Score{AccountID: "codex@example.com", Headroom: 0.9, ShortHeadroom: 0.9},
		},
		{
			email:    "claude@example.com",
			planType: "max",
			authMode: accounts.AuthModeOAuth,
			provider: accounts.ProviderClaude,
			score:    selectacct.Score{AccountID: "claude@example.com", Headroom: 0.8, ShortHeadroom: 0.8},
		},
	}, true)

	got := out.String()
	codexGroup := strings.Index(got, "Codex accounts")
	codexRow := strings.Index(got, "codex@example.com")
	claudeGroup := strings.Index(got, "Claude profiles")
	claudeRow := strings.Index(got, "claude@example.com")
	if codexGroup < 0 || codexRow < 0 || claudeGroup < 0 || claudeRow < 0 {
		t.Fatalf("grouped rows missing labels or accounts:\n%s", got)
	}
	if !(codexGroup < codexRow && codexRow < claudeGroup && claudeGroup < claudeRow) {
		t.Fatalf("provider grouping order is unclear:\n%s", got)
	}
}

func TestDisplayUsageRowsGridUsesClaudeLimitLabels(t *testing.T) {
	t.Setenv("COLUMNS", "160")
	var out bytes.Buffer
	displayUsageRows(&out, []srUsageRow{
		{
			email:    "primary@example.test",
			planType: "claude",
			authMode: accounts.AuthModeOAuth,
			provider: accounts.ProviderClaude,
			score:    selectacct.Score{AccountID: "lawrence@manaflow.ai", Headroom: 0.46, ShortHeadroom: 0.89, ShortResetAfterSeconds: int64((3 * time.Hour) / time.Second)},
			windows: []accounts.UsageWindow{
				{Name: "5h", UsedPercent: 11, ResetAfterSeconds: int64((3 * time.Hour) / time.Second)},
				{Name: "7d", UsedPercent: 54, ResetAfterSeconds: int64((5 * 24 * time.Hour) / time.Second)},
				{Name: "oauth-apps-weekly", UsedPercent: 100, ResetAfterSeconds: int64((5 * 24 * time.Hour) / time.Second)},
				{Name: "opus-weekly", UsedPercent: 100, ResetAfterSeconds: int64((5 * 24 * time.Hour) / time.Second)},
				{Name: "sonnet-weekly", UsedPercent: 0, ResetAfterSeconds: int64((5 * 24 * time.Hour) / time.Second)},
			},
		},
	}, false)

	got := out.String()
	if !strings.Contains(got, "5h") || !strings.Contains(got, "7d") || !strings.Contains(got, "Fable wk") || !strings.Contains(got, "Opus wk") || !strings.Contains(got, "Sonnet wk") {
		t.Fatalf("Claude grid missing Claude-specific labels:\n%s", got)
	}
	if strings.Contains(got, "Plan") || strings.Contains(got, "unknown") || strings.Contains(got, "Spark") {
		t.Fatalf("Claude grid should not use an unknown Plan column:\n%s", got)
	}
	if strings.Contains(got, "session reset") {
		t.Fatalf("Claude Use should leave reset details to the 5h column:\n%s", got)
	}
}

func TestClaudeUsageGridPrioritizesPopulatedColumnsWithoutTruncatingCore(t *testing.T) {
	t.Setenv("COLUMNS", "137")
	rows := make([]srUsageRow, 3)
	profiles := []struct {
		email        string
		plan         string
		headroom     float64
		sessionUsed  float64
		sessionReset time.Duration
		weeklyUsed   float64
		weeklyReset  time.Duration
		fableUsed    float64
		fableReset   time.Duration
		wantUse      string
		want5h       string
		want7d       string
		wantFable    string
	}{
		{
			email: "primary.account@example.test", plan: "max", headroom: .74,
			sessionReset: 4*time.Hour + 34*time.Minute, weeklyReset: 2*24*time.Hour + 6*time.Hour, fableReset: 24*time.Hour + 8*time.Hour,
			wantUse: "74% left", want5h: "100%/4h34m", want7d: "100%/2d6h", wantFable: "100%/1d8h",
		},
		{
			email: "secondary.account@example.test", plan: "pro", headroom: .83,
			sessionUsed: 11, sessionReset: 3*time.Hour + 21*time.Minute, weeklyUsed: 12, weeklyReset: 24*time.Hour + 5*time.Hour, fableUsed: 13, fableReset: 17 * time.Hour,
			wantUse: "83% left", want5h: "89%/3h21m", want7d: "88%/1d5h", wantFable: "87%/17h",
		},
		{
			email: "lab-profile", plan: "free", headroom: .92,
			sessionUsed: 22, sessionReset: 2*time.Hour + 8*time.Minute, weeklyUsed: 23, weeklyReset: 12 * time.Hour, fableUsed: 24, fableReset: 9 * time.Hour,
			wantUse: "92% left", want5h: "78%/2h8m", want7d: "77%/12h", wantFable: "76%/9h",
		},
	}
	for i, profile := range profiles {
		sessionReset := int64(profile.sessionReset / time.Second)
		rows[i] = srUsageRow{
			email: profile.email, planType: profile.plan, authMode: accounts.AuthModeOAuth, provider: accounts.ProviderClaude,
			score: selectacct.Score{AccountID: profile.email, Headroom: profile.headroom, ShortHeadroom: profile.headroom, ShortResetAfterSeconds: sessionReset},
			windows: []accounts.UsageWindow{
				{Name: "5h", UsedPercent: profile.sessionUsed, ResetAfterSeconds: sessionReset},
				{Name: "7d", UsedPercent: profile.weeklyUsed, ResetAfterSeconds: int64(profile.weeklyReset / time.Second)},
				{Name: "oauth-apps-weekly", UsedPercent: profile.fableUsed, ResetAfterSeconds: int64(profile.fableReset / time.Second)},
			},
		}
	}
	var out bytes.Buffer
	displayUsageRows(&out, rows, false)
	got := out.String()
	columns := usageGridColumnsForRows(&out, false, rows)
	for _, profile := range profiles {
		var line string
		for _, candidate := range strings.Split(got, "\n") {
			if strings.Contains(candidate, profile.email) {
				line = candidate
				break
			}
		}
		wantCells := map[string]string{
			"Account":  profile.email,
			"Pick":     profile.wantUse,
			"5h":       profile.want5h,
			"7d":       profile.want7d,
			"Fable wk": profile.wantFable,
		}
		for key, want := range wantCells {
			if cell := renderedUsageGridCell(line, columns, key); cell != want {
				t.Fatalf("Claude grid row for %q rendered %s cell %q, want %q:\n%s", profile.email, key, cell, want, got)
			}
		}
	}
	for _, want := range []string{"5h", "7d", "Fable wk"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Claude grid missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"Plan", "unknown", "Opus wk", "Sonnet wk", "Extra", "Auto-reload", "..."} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("Claude grid unexpectedly contains %q:\n%s", unwanted, got)
		}
	}
	assertUsageGridLineWidths(t, got, 137)
}

func TestClaudeUsageGridNarrowSchemaIsDeterministicAndOmitsEmptyColumns(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	rows := []srUsageRow{{
		email: "long-claude-account@example.com", planType: "max", authMode: accounts.AuthModeOAuth, provider: accounts.ProviderClaude,
		score:   selectacct.Score{AccountID: "long-claude-account@example.com", Headroom: .74, ShortHeadroom: .74, ShortResetAfterSeconds: 3600},
		windows: []accounts.UsageWindow{{Name: "5h", UsedPercent: 10, ResetAfterSeconds: 3600}, {Name: "7d", UsedPercent: 20, ResetAfterSeconds: 86400}},
	}}
	var first, second bytes.Buffer
	displayUsageRows(&first, rows, false)
	displayUsageRows(&second, rows, false)
	if first.String() != second.String() {
		t.Fatalf("narrow schema is not deterministic:\nfirst:\n%s\nsecond:\n%s", first.String(), second.String())
	}
	for _, unwanted := range []string{"Fable wk", "Opus wk", "Sonnet wk", "Extra", "Auto-reload"} {
		if strings.Contains(first.String(), unwanted) {
			t.Fatalf("narrow grid unexpectedly contains empty %q column:\n%s", unwanted, first.String())
		}
	}
	assertUsageGridLineWidths(t, first.String(), 80)

	shortRows := []srUsageRow{{
		email: "work", planType: "max", provider: accounts.ProviderClaude,
		score:   selectacct.Score{AccountID: "work", Headroom: .74, ShortHeadroom: .74, ShortResetAfterSeconds: 3600},
		windows: []accounts.UsageWindow{{Name: "5h", UsedPercent: 0, ResetAfterSeconds: 3600}, {Name: "7d", UsedPercent: 0, ResetAfterSeconds: 86400}},
	}}
	var shortOut bytes.Buffer
	displayUsageRows(&shortOut, shortRows, false)
	for _, want := range []string{"74% left", "100%/1h", "100%/1d"} {
		if !strings.Contains(shortOut.String(), want) {
			t.Fatalf("narrow short-account grid truncated %q:\n%s", want, shortOut.String())
		}
	}
	assertUsageGridLineWidths(t, shortOut.String(), 80)

	var worstOut bytes.Buffer
	displayUsageRows(&worstOut, []srUsageRow{{
		email: strings.Repeat("a", 36), planType: "enterprise", provider: accounts.ProviderClaude,
		active: true, err: errors.New("usage unavailable"),
	}}, false)
	if !strings.Contains(worstOut.String(), strings.Repeat("a", 28)) || !strings.Contains(worstOut.String(), "usage unavailable") {
		t.Fatalf("narrow worst-case grid lost higher-priority values:\n%s", worstOut.String())
	}
	assertUsageGridLineWidths(t, worstOut.String(), 80)
}

func TestClaudeUsageGridDropsLowerPriorityPopulatedWindowsBeforeTruncatingCore(t *testing.T) {
	t.Setenv("COLUMNS", "137")
	reset := int64((4*time.Hour + 34*time.Minute) / time.Second)
	row := srUsageRow{
		email: "claude@example.com", planType: "max", provider: accounts.ProviderClaude,
		score: selectacct.Score{AccountID: "claude@example.com", Headroom: .74, ShortHeadroom: .74, ShortResetAfterSeconds: reset},
		windows: []accounts.UsageWindow{
			{Name: "5h", UsedPercent: 0, ResetAfterSeconds: reset},
			{Name: "7d", UsedPercent: 0, ResetAfterSeconds: 86400},
			{Name: "oauth-apps-weekly", UsedPercent: 0, ResetAfterSeconds: 86400},
			{Name: "opus-weekly", UsedPercent: 0, ResetAfterSeconds: 86400},
			{Name: "sonnet-weekly", UsedPercent: 0, ResetAfterSeconds: 86400},
			{Name: "extra", UsedPercent: 0, ResetAfterSeconds: 86400},
		},
	}
	var out bytes.Buffer
	displayUsageRows(&out, []srUsageRow{row}, false)
	got := out.String()
	for _, want := range []string{"74% left", "100%/4h34m", "100%/1d", "5h", "7d", "Fable wk"} {
		if !strings.Contains(got, want) {
			t.Fatalf("all-window Claude grid truncated priority value %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "...") {
		t.Fatalf("all-window Claude grid truncated a retained value:\n%s", got)
	}
	assertUsageGridLineWidths(t, got, 137)
}

func TestClaudeUsageGridUsesOneSchemaForMixedRows(t *testing.T) {
	t.Setenv("COLUMNS", "137")
	rows := []srUsageRow{
		{email: "session@example.com", planType: "pro", provider: accounts.ProviderClaude, windows: []accounts.UsageWindow{{Name: "5h", UsedPercent: 10, ResetAfterSeconds: 3600}}},
		{email: "fable@example.com", planType: "free", provider: accounts.ProviderClaude, windows: []accounts.UsageWindow{{Name: "oauth-apps-weekly", UsedPercent: 20, ResetAfterSeconds: 86400}}},
	}
	var out bytes.Buffer
	displayUsageRows(&out, rows, false)
	got := out.String()
	if strings.Count(got, "5h") != 1 || strings.Count(got, "Fable wk") != 1 {
		t.Fatalf("mixed Claude rows did not share one populated schema:\n%s", got)
	}
	assertUsageGridLineWidths(t, got, 137)
}

func TestClaudeUsageGridFitsUnicodeAccountsByDisplayWidth(t *testing.T) {
	row := srUsageRow{
		email: "工程师-👩🏽‍💻-équipe@example.com", planType: "max", provider: accounts.ProviderClaude,
		score: selectacct.Score{AccountID: "unicode", Headroom: .74, ShortHeadroom: .74, ShortResetAfterSeconds: 3600},
		windows: []accounts.UsageWindow{
			{Name: "5h", UsedPercent: 0, ResetAfterSeconds: 3600},
			{Name: "7d", UsedPercent: 0, ResetAfterSeconds: 86400},
			{Name: "oauth-apps-weekly", UsedPercent: 0, ResetAfterSeconds: 86400},
		},
	}
	for _, width := range []string{"137", "80"} {
		t.Run(width, func(t *testing.T) {
			t.Setenv("COLUMNS", width)
			var out bytes.Buffer
			displayUsageRows(&out, []srUsageRow{row}, false)
			got := out.String()
			if !utf8.ValidString(got) {
				t.Fatalf("grid split a UTF-8 sequence: %q", got)
			}
			assertUsageGridLineWidths(t, got, mustAtoi(t, width))
		})
	}
}

func TestFitCellPreservesUnicodeGraphemesAndExactDisplayWidth(t *testing.T) {
	for _, tc := range []struct {
		value string
		width int
		want  string
	}{
		{value: "👩🏽‍💻abcdef", width: 5, want: "👩🏽‍💻..."},
		{value: "界x", width: 1, want: " "},
		{value: "界界界", width: 5, want: "界..."},
	} {
		got := fitCell(tc.value, tc.width)
		if got != tc.want {
			t.Fatalf("fitCell(%q, %d) = %q, want %q", tc.value, tc.width, got, tc.want)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("fitCell(%q, %d) returned invalid UTF-8: %q", tc.value, tc.width, got)
		}
		if gotWidth := runewidth.StringWidth(got); gotWidth != tc.width {
			t.Fatalf("fitCell(%q, %d) display width = %d, want %d", tc.value, tc.width, gotWidth, tc.width)
		}
	}
}

func mustAtoi(t *testing.T, value string) int {
	t.Helper()
	parsed, err := strconv.Atoi(value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func assertUsageGridLineWidths(t *testing.T, output string, width int) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		if lineWidth := runewidth.StringWidth(line); lineWidth > width {
			t.Fatalf("line width = %d, want <= %d:\n%s", lineWidth, width, line)
		}
	}
}

func renderedUsageGridCell(line string, columns []usageGridColumn, key string) string {
	offset := 0
	for _, column := range columns {
		if column.Key == key {
			prefix := runewidth.Truncate(line, offset, "")
			return strings.TrimSpace(runewidth.Truncate(line[len(prefix):], column.Width, ""))
		}
		offset += column.Width + 2
	}
	return ""
}

func TestClaudeUsageWindowsIncludeOAuthAppsWeekly(t *testing.T) {
	reset := time.Now().Add(4 * 24 * time.Hour).Format(time.RFC3339)
	windows := claudeUsageWindows(&agentclaude.UsageResponse{
		FiveHour:          &agentclaude.RateLimit{Utilization: srFloatPtr(0), ResetsAt: time.Now().Add(time.Hour).Format(time.RFC3339)},
		SevenDay:          &agentclaude.RateLimit{Utilization: srFloatPtr(60), ResetsAt: reset},
		SevenDayOAuthApps: &agentclaude.RateLimit{Utilization: srFloatPtr(100), ResetsAt: reset},
	})

	var oauthApps *accounts.UsageWindow
	for i := range windows {
		if windows[i].Name == "oauth-apps-weekly" {
			oauthApps = &windows[i]
			break
		}
	}
	if oauthApps == nil {
		t.Fatalf("missing oauth-apps-weekly window: %+v", windows)
	}
	if oauthApps.LimitWindowSeconds != int64((7*24*time.Hour)/time.Second) {
		t.Fatalf("oauth apps LimitWindowSeconds = %d, want 7d", oauthApps.LimitWindowSeconds)
	}
	if oauthApps.Feature != agentclaude.FableFeature {
		t.Fatalf("oauth apps Feature = %q, want %q", oauthApps.Feature, agentclaude.FableFeature)
	}
	score := scoreFromWindows("claude@example.com", windows)
	if score.Headroom == 0 {
		t.Fatalf("base headroom = %.2f, hidden Fable bucket should not exhaust non-Fable Claude models", score.Headroom)
	}
	fableScore, ok := score.ModelScores[selectacct.ModelKey(agentclaude.FableFeature)]
	if !ok {
		t.Fatalf("missing Fable model score: %+v", score.ModelScores)
	}
	if fableScore.Headroom != 0 {
		t.Fatalf("Fable headroom = %.2f, want 0 from saturated oauth app weekly bucket", fableScore.Headroom)
	}
	// A saturated Fable (per-model) pool must NOT cook the account: Opus/Sonnet
	// and the base weekly quota remain usable. The Use column instead notes that
	// Fable specifically is out.
	cooked, reason := cookedFromWindows(windows)
	if cooked {
		t.Fatalf("account should not be cooked when only the Fable pool is exhausted (reason=%q)", reason)
	}
	if suffix := exhaustedModelSuffix(windows); !strings.Contains(suffix, "Fable") {
		t.Fatalf("Use suffix = %q, want it to note Fable is out", suffix)
	}
}

func TestClaudeUsageWindowsTagModelWeeklyBuckets(t *testing.T) {
	windows := claudeUsageWindows(&agentclaude.UsageResponse{
		SevenDayOpus:   &agentclaude.RateLimit{Utilization: srFloatPtr(80)},
		SevenDaySonnet: &agentclaude.RateLimit{Utilization: srFloatPtr(20)},
	})
	byName := make(map[string]accounts.UsageWindow, len(windows))
	for _, window := range windows {
		byName[window.Name] = window
	}
	if got := byName["opus-weekly"].Feature; got != agentclaude.OpusFeature {
		t.Fatalf("opus-weekly Feature = %q, want %q", got, agentclaude.OpusFeature)
	}
	if got := byName["sonnet-weekly"].Feature; got != agentclaude.SonnetFeature {
		t.Fatalf("sonnet-weekly Feature = %q, want %q", got, agentclaude.SonnetFeature)
	}
	score := scoreFromWindows("claude@example.com", windows)
	if got := score.ModelScores[selectacct.ModelKey(agentclaude.OpusFeature)].Headroom; math.Abs(got-0.2) > 1e-9 {
		t.Fatalf("Opus headroom = %.2f, want 0.20", got)
	}
	if got := score.ModelScores[selectacct.ModelKey(agentclaude.SonnetFeature)].Headroom; math.Abs(got-0.8) > 1e-9 {
		t.Fatalf("Sonnet headroom = %.2f, want 0.80", got)
	}
}

// The CLI's own usage read builds the Fable window as the daemon does: from
// the limits[] Fable row ("Fable 5.2" counts), ahead of seven_day_oauth_apps.
func TestLocalClaudeUsageReadTakesTheFableWindowFromTheLimitsRow(t *testing.T) {
	rowResets := time.Now().Add(2 * 24 * time.Hour).UTC().Truncate(time.Second)
	client := &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/api/oauth/usage" {
			t.Fatalf("unexpected request to %s: the limits[] row is the Fable meter, no probe", req.URL.Path)
		}
		body := `{"seven_day":{"utilization":40,"resets_at":"2030-01-02T00:00:00Z"},` +
			`"seven_day_oauth_apps":{"utilization":0,"resets_at":"2030-01-05T00:00:00Z"},"limits":[` +
			`{"kind":"weekly_scoped","group":"weekly","percent":88,"resets_at":"` + rowResets.Format(time.RFC3339) + `","severity":"warning","is_active":false,"scope":{"model":{"display_name":"Fable 5.2"}}}]}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	windows, _, err := fetchClaudeUsageWindowsAndReset(context.Background(), client, "access-token")
	if err != nil {
		t.Fatal(err)
	}
	var fable *accounts.UsageWindow
	for i := range windows {
		if windows[i].Name == agentclaude.FableWindowName {
			fable = &windows[i]
		}
	}
	if fable == nil || fable.UsedPercent != 88 || fable.Feature != agentclaude.FableFeature {
		t.Fatalf("Fable window = %+v, want 88%% used from the Fable 5.2 row, not 0 from seven_day_oauth_apps", fable)
	}
	want := int64(2 * 24 * 60 * 60)
	if fable.ResetAfterSeconds > want || fable.ResetAfterSeconds < want-60 {
		t.Fatalf("Fable ResetAfterSeconds = %d, want ~%d from the row's resets_at", fable.ResetAfterSeconds, want)
	}
}

// sr's Fable window is a weekly_scoped row named Fable in any case; a
// weekly_all or session row scoped to Fable is not it, so the window falls
// back to seven_day_oauth_apps.
func TestLocalClaudeUsageReadTakesAWeeklyScopedRowNamedFableInAnyCase(t *testing.T) {
	row := func(kind, group, name string, percent int) string {
		return `{"kind":"` + kind + `","group":"` + group + `","percent":` + strconv.Itoa(percent) +
			`,"resets_at":"2030-01-02T00:00:00.000Z","severity":"normal","is_active":false,"scope":{"model":{"display_name":"` + name + `"}}}`
	}
	for _, tc := range []struct {
		name string
		rows []string
		want float64
	}{
		{"weekly_all and session rows named Fable", []string{row("weekly_all", "weekly", "Fable", 99), row("session", "session", "Fable", 97)}, 10},
		{"lower case", []string{row("weekly_scoped", "weekly", "fable", 30)}, 30},
		{"upper case with a version", []string{row("weekly_scoped", "weekly", "FABLE 5.2", 40)}, 40},
	} {
		body := `{"seven_day":{"utilization":40,"resets_at":"2030-01-02T00:00:00Z"},` +
			`"seven_day_oauth_apps":{"utilization":10,"resets_at":"2030-01-05T00:00:00Z"},"limits":[` + strings.Join(tc.rows, ",") + `]}`
		client := &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/api/oauth/usage" {
				t.Fatalf("%s: unexpected request to %s", tc.name, req.URL.Path)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		windows, _, err := fetchClaudeUsageWindowsAndReset(context.Background(), client, "access-token")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var fable []accounts.UsageWindow
		for _, window := range windows {
			if window.Name == agentclaude.FableWindowName {
				fable = append(fable, window)
			}
		}
		if len(fable) != 1 || fable[0].UsedPercent != tc.want {
			t.Fatalf("%s: Fable windows = %+v, want one at %v%% used", tc.name, fable, tc.want)
		}
	}
}

func TestClaudeStatusRendersExtraUsageBalanceAndDisabledState(t *testing.T) {
	t.Setenv("COLUMNS", "220")
	limit, used := 2000.0, 750.0
	autoReloadOff := false
	for _, tc := range []struct {
		name  string
		extra *accounts.ExtraUsageInfo
		want  string
	}{
		// The $ figure mirrors Claude's "Monthly spend limit: $X of $Y" line:
		// metered spend used, not remaining and not the prepaid credit balance
		// (the OAuth usage API never reports one).
		{name: "enabled", extra: &accounts.ExtraUsageInfo{IsEnabled: true, MonthlyLimit: &limit, UsedCredits: &used}, want: "$7.50/$20.00"},
		{name: "disabled", extra: &accounts.ExtraUsageInfo{IsEnabled: false, MonthlyLimit: &limit, UsedCredits: &used}, want: "off"},
		{
			name:  "spend with auto-reload",
			extra: &accounts.ExtraUsageInfo{IsEnabled: true, MonthlyLimit: &limit, UsedCredits: &used, AutoReload: &autoReloadOff},
			want:  "Auto-reload",
		},
		{
			name:  "disabled with reason",
			extra: &accounts.ExtraUsageInfo{IsEnabled: false, MonthlyLimit: &limit, UsedCredits: &used, DisabledReason: "out_of_credits"},
			want:  "off · out of credits",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			displayUsageRows(&out, []srUsageRow{{
				email: "claude@example.com", provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth,
				planType: "max", extraUsage: tc.extra,
				score: selectacct.Score{AccountID: "claude@example.com", Headroom: 0, ShortHeadroom: 0},
			}}, false)
			if !strings.Contains(out.String(), tc.want) {
				t.Fatalf("status output missing %q:\n%s", tc.want, out.String())
			}
		})
	}
}

func srFloatPtr(v float64) *float64 { return &v }

func TestDisplayUsageRowsGridCompactsForNarrowTerminals(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	var out bytes.Buffer
	displayUsageRows(&out, []srUsageRow{
		{
			email:          "test@example.invalid",
			planType:       "pro",
			gtoRecommended: true,
			authMode:       accounts.AuthModeOAuth,
			provider:       accounts.ProviderCodex,
			score:          selectacct.Score{AccountID: "test@example.invalid", Headroom: 0.67, ShortHeadroom: 0.96, ShortResetAfterSeconds: int64(time.Minute / time.Second)},
			windows: []accounts.UsageWindow{
				{Name: "primary", UsedPercent: 4, LimitWindowSeconds: int64((5 * time.Hour) / time.Second), ResetAfterSeconds: int64(time.Minute / time.Second)},
				{Name: "secondary", UsedPercent: 33, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAfterSeconds: int64((5 * 24 * time.Hour) / time.Second)},
				{Name: "GPT-5.3-Codex-Spark/primary", Feature: "GPT-5.3-Codex-Spark", UsedPercent: 0, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)},
				{Name: "GPT-5.3-Codex-Spark/secondary", Feature: "GPT-5.3-Codex-Spark", UsedPercent: 0, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
			},
			credits: &accounts.CreditsInfo{Balance: "0"},
		},
		{
			email:    "austin@manaflow.ai",
			planType: "claude",
			authMode: accounts.AuthModeOAuth,
			provider: accounts.ProviderClaude,
			score:    selectacct.Score{AccountID: "austin@manaflow.ai", Headroom: 0.95, ShortHeadroom: 0.95},
			windows: []accounts.UsageWindow{
				{Name: "5h", UsedPercent: 5, ResetAfterSeconds: int64((4 * time.Hour) / time.Second)},
				{Name: "7d", UsedPercent: 1, ResetAfterSeconds: int64((2 * 24 * time.Hour) / time.Second)},
			},
		},
	}, false)

	got := out.String()
	if strings.Contains(got, "Spark") || strings.Contains(got, "Spark wk") {
		t.Fatalf("narrow grid should omit Spark columns:\n%s", got)
	}
	if strings.HasPrefix(strings.TrimLeft(got, "\n"), "#") {
		t.Fatalf("non-numbered grid should not render blank # column:\n%s", got)
	}
	if !strings.Contains(got, "Use") || !strings.Contains(got, "Claude profiles") {
		t.Fatalf("narrow grid missing core columns or groups:\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if line == "" {
			continue
		}
		if width := runewidth.StringWidth(line); width > 80 {
			t.Fatalf("line width = %d, want <= 80:\n%s\nfull output:\n%s", width, line, got)
		}
	}
}

func TestDisplayUsageRowsGridColorsWhenForced(t *testing.T) {
	t.Setenv("SR_USAGE_GRID", "1")
	t.Setenv("FORCE_COLOR", "1")
	t.Setenv("COLUMNS", "200")
	var out bytes.Buffer
	displayUsageRows(&out, []srUsageRow{
		{
			email:          "ok@example.com",
			gtoRecommended: true,
			gtoReason:      "90% bottleneck left",
			authMode:       accounts.AuthModeOAuth,
			score:          selectacct.Score{AccountID: "ok@example.com", Headroom: 0.9, ShortHeadroom: 0.9},
			windows:        []accounts.UsageWindow{{Name: "primary", UsedPercent: 10, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)}},
		},
		{
			email:            "temp@example.com",
			gtoReason:        "temporarily cooked, cannot start new session",
			authMode:         accounts.AuthModeOAuth,
			tempCooked:       true,
			tempCookedReason: "5h limit fully consumed",
			windows:          []accounts.UsageWindow{{Name: "primary", UsedPercent: 100, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)}},
		},
		{
			email:        "cooked@example.com",
			gtoReason:    "cooked, cannot switch",
			authMode:     accounts.AuthModeOAuth,
			cooked:       true,
			windows:      []accounts.UsageWindow{{Name: "secondary", UsedPercent: 100, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)}},
			planType:     "pro",
			cookedReason: "7d limit fully consumed",
		},
	}, true)

	got := out.String()
	for _, code := range []string{ansiGreen, ansiYellow, ansiRed, ansiBold + ansiWhite, ansiDim, ansiBGRowAlt} {
		if !strings.Contains(got, code) {
			t.Fatalf("grid output missing color %q:\n%q", code, got)
		}
	}
	if strings.Contains(got, "\x1b[32mok@example.com") {
		t.Fatalf("account column should keep account styling separate from state colors:\n%q", got)
	}
}

func TestUsageGridPreservesShortWindowsByQuotaFamily(t *testing.T) {
	windows := []accounts.UsageWindow{
		{Name: "primary", UsedPercent: 10, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)},
		{Name: "secondary", UsedPercent: 100, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
		{Name: "GPT-5.3-Codex-Spark/primary", Feature: "GPT-5.3-Codex-Spark", UsedPercent: 1, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)},
		{Name: "GPT-5.3-Codex-Spark/secondary", Feature: "GPT-5.3-Codex-Spark", UsedPercent: 2, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
	}
	cooked := srUsageRow{cooked: true, windows: windows}
	if cell := usageGridShortWindowCell(cooked); cell.Text != "90%" {
		t.Fatalf("short window cell = %q, want 90%% even when general weekly quota is cooked", cell.Text)
	}

	tempCooked := srUsageRow{tempCooked: true, windows: []accounts.UsageWindow{
		{Name: "primary", UsedPercent: 100, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)},
		{Name: "secondary", UsedPercent: 2, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
		{Name: "GPT-5.3-Codex-Spark/primary", Feature: "GPT-5.3-Codex-Spark", UsedPercent: 100, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)},
		{Name: "GPT-5.3-Codex-Spark/secondary", Feature: "GPT-5.3-Codex-Spark", UsedPercent: 2, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
	}}
	if cell := usageGridShortWindowCell(tempCooked); cell.Text == "" {
		t.Fatal("temporarily cooked row should still show the short window")
	}

	sparkWeeklyCooked := srUsageRow{cooked: true, windows: []accounts.UsageWindow{
		{Name: "primary", UsedPercent: 10, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)},
		{Name: "secondary", UsedPercent: 2, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
		{Name: "GPT-5.3-Codex-Spark/primary", Feature: "GPT-5.3-Codex-Spark", UsedPercent: 1, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)},
		{Name: "GPT-5.3-Codex-Spark/secondary", Feature: "GPT-5.3-Codex-Spark", UsedPercent: 100, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
	}}
	if cell := usageGridShortWindowCell(sparkWeeklyCooked); cell.Text == "" {
		t.Fatal("general short window should remain visible when only Spark weekly quota is cooked")
	}
}

func TestDisplayUsageRowsIgnoresDetailedModeEnv(t *testing.T) {
	t.Setenv("SR_USAGE_GRID", "0")
	t.Setenv("CX_USAGE_GRID", "0")
	t.Setenv("COLUMNS", "200")
	var out bytes.Buffer
	displayUsageRows(&out, []srUsageRow{
		{
			email:     "a@example.com",
			gtoReason: "80% bottleneck left",
			authMode:  accounts.AuthModeOAuth,
		},
	}, true)

	got := out.String()
	if !strings.Contains(got, "Account") || !strings.Contains(got, "a@example.com") {
		t.Fatalf("grid output missing expected row:\n%s", got)
	}
	if strings.Contains(got, "1) a@example.com") || strings.Contains(got, "pick:") {
		t.Fatalf("detailed output should never be rendered:\n%s", got)
	}
}

func TestParseSRSwitchArgs(t *testing.T) {
	selector, opts, err := parseSRSwitchArgs([]string{"a@example.com", "--restart-gui"}, srSwitchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if selector != "a@example.com" {
		t.Fatalf("selector = %q", selector)
	}
	if !opts.restartCodexGUI {
		t.Fatal("restartCodexGUI should be true")
	}
}

func readJSONFile(t *testing.T, path string, out any) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatal(err)
	}
}

func testJWT(claims map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func testCodexAuth(email, accountID string) accounts.CodexAuthFile {
	access := testJWT(map[string]any{
		"exp": time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_user_id":    "user:" + email,
		},
		"https://api.openai.com/profile": map[string]any{
			"email": email,
		},
	})
	id := testJWT(map[string]any{
		"exp":                         time.Now().Add(time.Hour).Unix(),
		"email":                       email,
		"https://api.openai.com/auth": map[string]any{"chatgpt_user_id": "user:" + email, "chatgpt_account_id": accountID},
	})
	return accounts.CodexAuthFile{AuthMode: "chatgpt", Tokens: &accounts.CodexTokens{
		AccessToken:  access,
		RefreshToken: "refresh-" + accountID,
		IDToken:      id,
	}}
}

func installFakeCodexLogin(t *testing.T, home string, auth accounts.CodexAuthFile) {
	t.Helper()
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// Real codex writes into CODEX_HOME when it is set, falling back to
	// ~/.codex. The fake has to honor the same precedence or it cannot show
	// whether a login was correctly isolated away from the active session.
	script := "#!/bin/sh\n" +
		"test \"$1\" = login || exit 2\n" +
		"codex_home=${CODEX_HOME:-$HOME/.codex}\n" +
		"mkdir -p \"$codex_home\"\n" +
		"cat > \"$codex_home/auth.json\" <<'JSON'\n" +
		string(body) + "\nJSON\n"
	path := filepath.Join(binDir, "codex")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

type srRoundTripFunc func(*http.Request) (*http.Response, error)

func (f srRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func usageResponse(usedPercent float64) *http.Response {
	return usageResponseWindows(usedPercent, 0)
}

func usageResponseWindows(primaryUsedPercent, secondaryUsedPercent float64) *http.Response {
	body, _ := json.Marshal(map[string]any{
		"plan_type": "pro",
		"rate_limit": map[string]any{
			"primary_window": map[string]any{
				"used_percent":         primaryUsedPercent,
				"limit_window_seconds": int64((5 * time.Hour) / time.Second),
				"reset_after_seconds":  int64((3 * time.Hour) / time.Second),
			},
			"secondary_window": map[string]any{
				"used_percent":         secondaryUsedPercent,
				"limit_window_seconds": int64((7 * 24 * time.Hour) / time.Second),
				"reset_after_seconds":  int64((6 * 24 * time.Hour) / time.Second),
			},
		},
	})
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

// A credential Anthropic revoked is published with no error, so the error
// footnote never covered it, and upstream's Use cell said "re-add" -- the
// setup-token verb, which on 2026-09-27 minted setup tokens for OAuth accounts
// (PATCHES §36). A revoked OAuth row says re-auth and names the command.
func TestRevokedClaudeServerRowNamesTheReauthCommand(t *testing.T) {
	t.Setenv("COLUMNS", "200")
	rows := usageRowsFromServerUsageStatuses([]remoteServerUsageStatus{{
		ID: "chip-6", Email: "chip-6@corp.example", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth,
		RevokedSince: time.Date(2026, 10, 9, 1, 0, 0, 0, time.UTC), RevokedKind: "revoked",
	}})
	if len(rows) != 1 || rows[0].err != nil {
		t.Fatalf("rows = %+v, want one revoked row without an error", rows)
	}
	if got := compactPickReason(rows[0]); got != "re-auth" {
		t.Fatalf("Use = %q, want re-auth for a revoked OAuth login", got)
	}
	var out bytes.Buffer
	displayUsageRows(&out, rows, false)
	text := out.String()
	if !strings.Contains(text, "chip-6@corp.example [claude]: revoked since 10-09 01:00Z (re-auth on that server with: sr claude addr chip-6)") {
		t.Fatalf("revoked row names no repair command:\n%s", text)
	}
	if strings.Contains(text, "re-add") || strings.Contains(text, "sr claude add ") {
		t.Fatalf("revoked OAuth row names the setup-token verb:\n%s", text)
	}
	// An organization that disallows subscription logins is fixed by neither.
	rows[0].revokedKind = "org_oauth_disallowed"
	if got := compactPickReason(rows[0]); got != "use another" {
		t.Fatalf("Use = %q for an org-disabled login, want use another", got)
	}
}

func TestUsageErrorFootnoteShowsProviderAndReaddCommand(t *testing.T) {
	t.Setenv("COLUMNS", "200")
	// A server's rows are profiles on that server, built the way `sr status`
	// builds them. The daemon publishes a Claude profile's oauthAccount address
	// as its email, and the row shows that; the hint must name the profile ID,
	// or `sr claude addr <email>` on the server signs in a new profile beside
	// the dead one.
	serverRows := usageRowsFromServerUsageStatuses([]remoteServerUsageStatus{
		{
			ID: "chip-5", Email: "user@corp.example", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth,
			Error: "Claude OAuth refresh failed: 400 Bad Request: invalid_grant",
		},
		{
			ID: "old-token", Email: "old@corp.example", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth,
			Error: `Claude profile "old-token" has no usable credential: setup token expired 2026-09-01; re-add with 'sr claude add old-token'`,
		},
	})
	// Calibration: the rows show the emails, so a hint built from what the
	// row shows would name them. Since upstream #406 a server row keeps the
	// stable ID in email and shows the login address (displayUsageAccountName).
	if len(serverRows) != 2 || !slices.ContainsFunc(serverRows, func(row srUsageRow) bool { return displayUsageAccountName(row) == "user@corp.example" }) {
		t.Fatalf("server rows = %+v, want two rows displaying the account emails", serverRows)
	}
	var out bytes.Buffer
	displayUsageRows(&out, append([]srUsageRow{
		{
			email:    "codex-broken@example.com",
			authMode: accounts.AuthModeOAuth,
			err:      errors.New("usage fetch failed: 401 Unauthorized"),
		},
		{
			email:         "seven-alpha",
			provider:      accounts.ProviderClaude,
			claudeProfile: "seven-alpha",
			err:           errors.New("Claude OAuth refresh failed: 400 Bad Request: invalid_grant"),
		},
		{
			// A setup token whose error does not say so: only the credential
			// the row was read from knows the kind.
			email:            "year-token",
			provider:         accounts.ProviderClaude,
			claudeProfile:    "year-token",
			claudeSetupToken: true,
			err:              errors.New("usage fetch failed: 401 Unauthorized"),
		},
		{
			email:    "codex-flaky@example.com",
			authMode: accounts.AuthModeOAuth,
			err:      errors.New("usage fetch failed: connection refused"),
		},
	}, serverRows...), false)
	text := out.String()
	if !strings.Contains(text, "codex-broken@example.com [codex]: usage fetch failed: 401 Unauthorized (re-add with: sr add)") {
		t.Fatalf("codex 401 footnote missing provider/re-add hint:\n%s", text)
	}
	// The Claude hint names the repair for the row's login, with its name
	// (PATCHES §36). It said `sr add claude` for all of them: the setup-token
	// verb, nameless, which minted setup tokens for OAuth accounts on 2026-09-27.
	for _, want := range []string{
		"seven-alpha [claude]: Claude OAuth refresh failed: 400 Bad Request: invalid_grant (re-auth with: sr claude addr seven-alpha)",
		"year-token [claude]: usage fetch failed: 401 Unauthorized (re-add with: sr claude add year-token)",
		"user@corp.example [claude]: Claude OAuth refresh failed: 400 Bad Request: invalid_grant (re-auth on that server with: sr claude addr chip-5)",
		"(re-add on that server with: sr claude add old-token)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("claude footnote missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "addr user@corp.example") || strings.Contains(text, "add old@corp.example") {
		t.Fatalf("a server row's hint names the account email, not the server's profile:\n%s", text)
	}
	if strings.Contains(text, "sr add claude") {
		t.Fatalf("a Claude footnote still names the nameless setup-token verb:\n%s", text)
	}
	// Hosted cmux storage is the exception: there `sr add claude` is that
	// storage's own add, the only way back in.
	hosted := usageRowsFromHostedStatuses([]broker.UsageStatus{{
		ID: "team-claude", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth,
		Error: "Claude OAuth refresh failed: 400 Bad Request: invalid_grant",
	}})
	out.Reset()
	displayUsageRows(&out, hosted, false)
	if !strings.Contains(out.String(), "invalid_grant (re-add with: sr add claude)") {
		t.Fatalf("hosted Claude footnote lost the hosted add:\n%s", out.String())
	}
	if strings.Contains(text, "codex-flaky@example.com [codex]: usage fetch failed: connection refused (re-add") {
		t.Fatalf("transient error should not suggest re-add:\n%s", text)
	}
	if !strings.Contains(text, "codex-flaky@example.com [codex]: usage fetch failed: connection refused") {
		t.Fatalf("transient error footnote should still show provider:\n%s", text)
	}
}

// A usage row must be filed under the provider its key actually reaches.
// Hardcoding Codex listed every Qwen, Grok, and OpenRouter key under
// "Codex accounts", which misreports what the account is for.
func TestUsageRowsGroupByTheirOwnProvider(t *testing.T) {
	cases := []struct {
		provider  accounts.Provider
		wantLabel string
		wantPlan  string
	}{
		{provider: accounts.ProviderCodex, wantLabel: "Codex accounts", wantPlan: "api key"},
		{provider: "", wantLabel: "Codex accounts", wantPlan: "api key"},
		{provider: accounts.ProviderClaude, wantLabel: "Claude profiles", wantPlan: "claude key"},
		{provider: accounts.ProviderQwenToken, wantLabel: "Qwen accounts", wantPlan: "qwen-token key"},
		{provider: accounts.ProviderQwenAnthropic, wantLabel: "Qwen-anthropic accounts", wantPlan: "qwen-anthropic key"},
		{provider: accounts.ProviderGrok, wantLabel: "Grok accounts", wantPlan: "grok key"},
	}
	for _, tc := range cases {
		t.Run(string(tc.provider), func(t *testing.T) {
			row := srUsageRow{email: "acct", provider: tc.provider}
			if got := usageProviderLabel(row); got != tc.wantLabel {
				t.Fatalf("usageProviderLabel = %q, want %q", got, tc.wantLabel)
			}
			if got := apiKeyPlanLabel(tc.provider); got != tc.wantPlan {
				t.Fatalf("apiKeyPlanLabel = %q, want %q", got, tc.wantPlan)
			}
		})
	}

	// Codex and Claude keep their positions; everything else sorts after them
	// rather than interleaving.
	codex := usageProviderOrder(srUsageRow{provider: accounts.ProviderCodex})
	claude := usageProviderOrder(srUsageRow{provider: accounts.ProviderClaude})
	other := usageProviderOrder(srUsageRow{provider: accounts.ProviderQwenToken})
	if !(codex < claude && claude < other) {
		t.Fatalf("ordering = codex %d, claude %d, other %d; want codex first", codex, claude, other)
	}
}

func TestRankUsageRowsKeepsProviderHeadingsContiguous(t *testing.T) {
	rows := []srUsageRow{
		{email: "qwen-token:z", provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey},
		{email: "kimi:z-api", provider: accounts.ProviderKimi, authMode: accounts.AuthModeAPIKey},
		{email: "qwen-token:a", provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey},
		{email: "kimi:a-oauth", provider: accounts.ProviderKimi, authMode: accounts.AuthModeOAuth},
	}
	rankUsageRows(rows)
	got := make([]string, 0, len(rows))
	for _, row := range rows {
		got = append(got, usageProviderLabel(row))
	}
	want := []string{
		"Kimi API-key accounts",
		"Kimi subscription accounts",
		"Qwen accounts",
		"Qwen accounts",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("provider groups = %v, want %v", got, want)
	}
}

// The Codex windows and the scheduler's "Use" advice mean nothing for a vendor
// that publishes no quota, so a keyed-provider section gets its own columns.
func TestKeyedProviderSectionUsesItsOwnColumns(t *testing.T) {
	var out bytes.Buffer
	keyed := usageGridColumns(&out, false, srUsageRow{provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey, showShortWindow: true, showLongWindow: true})
	keys := map[string]int{}
	for _, column := range keyed {
		keys[column.Key] = column.Width
	}
	for _, want := range []string{"Account", "Plan", "State", "Pick", "5h", "7d"} {
		if _, ok := keys[want]; !ok {
			t.Fatalf("keyed section is missing the %q column: %v", want, keys)
		}
	}
	for _, unwanted := range []string{"Key ID", "Sessions", "Login", "Models", "Endpoints", "Reset", "Credits"} {
		if _, ok := keys[unwanted]; ok {
			t.Fatalf("keyed section should not carry the %q column", unwanted)
		}
	}
	// Codex keeps its own columns untouched.
	codex := usageGridColumns(&out, false, srUsageRow{provider: accounts.ProviderCodex})
	codexKeys := map[string]bool{}
	for _, column := range codex {
		codexKeys[column.Key] = true
	}
	for _, want := range []string{"Pick", "5h", "7d"} {
		if !codexKeys[want] {
			t.Fatalf("codex section lost its %q column", want)
		}
	}
	if codexKeys["Endpoints"] || codexKeys["Quota"] {
		t.Fatal("codex section must not gain the keyed-provider columns")
	}

	// A standalone sr process does not have serve's declared registry, but the
	// stored provider still identifies this as an API-key section. It must not
	// fall back to Codex quota and switch columns merely because metering is
	// unknown locally.
	custom := usageGridColumns(&out, false, srUsageRow{
		provider: accounts.Provider("acme-relay"), authMode: accounts.AuthModeAPIKey,
	})
	customKeys := map[string]bool{}
	for _, column := range custom {
		customKeys[column.Key] = true
	}
	if customKeys["Endpoints"] || customKeys["Quota"] || customKeys["5h"] || customKeys["7d"] {
		t.Fatalf("declared-provider columns = %v, want compact provider layout", customKeys)
	}
}

func TestKimiSubscriptionSectionShowsIndependentQuotaWindows(t *testing.T) {
	t.Setenv("COLUMNS", "120")
	var out bytes.Buffer
	columns := usageGridColumns(&out, false, srUsageRow{provider: accounts.ProviderKimi, authMode: accounts.AuthModeOAuth})
	keys := map[string]bool{}
	for _, column := range columns {
		keys[column.Key] = true
		if column.Key == "5h" && column.Width < len("100%/12h34m") {
			t.Fatalf("Kimi 5h width = %d, want an untruncated reset value", column.Width)
		}
	}
	for _, want := range []string{"Account", "State", "5h", "Weekly"} {
		if !keys[want] {
			t.Fatalf("Kimi section is missing %q: %+v", want, columns)
		}
	}
	for _, unwanted := range []string{"Plan", "Pick", "Models", "Endpoints", "Quota", "7d"} {
		if keys[unwanted] {
			t.Fatalf("Kimi subscription section should not carry %q", unwanted)
		}
	}

	displayUsageRows(&out, []srUsageRow{{
		email:    "kimi-code",
		provider: accounts.ProviderKimi,
		authMode: accounts.AuthModeOAuth,
		active:   true,
		planType: "subscription",
		windows: []accounts.UsageWindow{
			{Name: "5h", UsedPercent: 25, LimitWindowSeconds: int64((5 * time.Hour) / time.Second), ResetAfterSeconds: 3600},
			{Name: "weekly", UsedPercent: 40, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAfterSeconds: 2 * 86400},
		},
	}}, false)
	text := out.String()
	for _, want := range []string{"Kimi subscription accounts", "kimi-code", "75%/1h", "60%/2d"} {
		if !strings.Contains(text, want) {
			t.Fatalf("Kimi status is missing %q:\n%s", want, text)
		}
	}
}

func TestKimiSubscriptionStateDistinguishesReadyRecommendedAndActive(t *testing.T) {
	ready := srUsageRow{
		provider: accounts.ProviderKimi, authMode: accounts.AuthModeOAuth,
		sessionsKnown: true, score: selectacct.Score{Headroom: 1, ShortHeadroom: 1},
	}
	if got := usageGridState(ready); got != "ready" {
		t.Fatalf("ready Kimi state = %q", got)
	}
	ready.gtoRecommended = true
	if got := usageGridState(ready); got != "rec" {
		t.Fatalf("recommended Kimi state = %q", got)
	}
	ready.active = true
	ready.assignedSessions = 1
	if got := usageGridState(ready); got != "active, rec" {
		t.Fatalf("active recommended Kimi state = %q", got)
	}
}

func TestClaudeAPIKeyStateSaysFallback(t *testing.T) {
	row := srUsageRow{provider: accounts.ProviderClaude, authMode: accounts.AuthModeAPIKey}
	if got := usageGridState(row); got != "API key fallback" {
		t.Fatalf("Claude API-key state = %q, want explicit fallback label", got)
	}
	row.active = true
	row.sessionsKnown = true
	row.assignedSessions = 1
	if got := usageGridState(row); got != "active, API key fallback" {
		t.Fatalf("active Claude API-key state = %q, want explicit active fallback label", got)
	}
}

func TestKimiAPIKeyShowsQuotaWindowsOnlyWhenAvailable(t *testing.T) {
	withQuota := srUsageRow{
		provider: accounts.ProviderKimi, authMode: accounts.AuthModeAPIKey,
		quotaUsageKnown: true,
	}
	keys := map[string]bool{}
	for _, column := range usageGridColumns(&bytes.Buffer{}, false, withQuota) {
		keys[column.Key] = true
	}
	if !keys["5h"] || !keys["Weekly"] {
		t.Fatalf("Kimi subscription key columns = %v", keys)
	}
	withoutQuota := withQuota
	withoutQuota.quotaUsageKnown = false
	keys = map[string]bool{}
	for _, column := range usageGridColumns(&bytes.Buffer{}, false, withoutQuota) {
		keys[column.Key] = true
	}
	if keys["5h"] || keys["Weekly"] {
		t.Fatalf("Kimi key without quota exposed empty windows: %v", keys)
	}
}

func TestQwenTokenPlanNamesAndQuotaWindowsDoNotTruncate(t *testing.T) {
	t.Setenv("COLUMNS", "120")
	row := srUsageRow{
		provider:        accounts.ProviderQwenToken,
		authMode:        accounts.AuthModeAPIKey,
		planType:        "Standard",
		showShortWindow: true,
		showLongWindow:  true,
	}
	if got := usageGridPlan(row); got != "Token Plan Standard" {
		t.Fatalf("usageGridPlan = %q, want Token Plan Standard", got)
	}
	for _, column := range usageGridColumns(&bytes.Buffer{}, false, row) {
		switch column.Key {
		case "Plan":
			if column.Width < len("Token Plan Standard") {
				t.Fatalf("Qwen plan width = %d, want untruncated Token Plan Standard", column.Width)
			}
		case "5h", "7d":
			if column.Width < len("100%/12h34m") {
				t.Fatalf("Qwen %s width = %d, want an untruncated reset value", column.Key, column.Width)
			}
		}
	}
}

func TestAntigravityStatusUsesAuthAndSessionTruthWithoutFakeQuota(t *testing.T) {
	t.Setenv("COLUMNS", "120")
	statuses := []remoteServerUsageStatus{
		{
			ID: "antigravity", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth,
			AccountIdentity: "router agy login", PlanType: "subscription", AuthChecked: true, AuthValid: true,
			SessionsKnown: true,
		},
		{
			ID: "antigravity-active", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth,
			AccountIdentity: "second agy login", PlanType: "subscription", AuthChecked: true, AuthValid: true,
			AssignedSessions: 1, SessionsKnown: true,
		},
	}
	rows := usageRowsFromServerUsageStatuses(statuses)
	if len(rows) != 2 || usageGridState(rows[0]) != "ready" || usageGridState(rows[1]) != "active" {
		t.Fatalf("Antigravity states = %+v", rows)
	}
	var out bytes.Buffer
	displayUsageRows(&out, rows, false)
	text := out.String()
	for _, want := range []string{"router agy login", "second agy login", "subscription", "ready", "active", "quota not exposed"} {
		if !strings.Contains(text, want) {
			t.Fatalf("Antigravity status should show %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"subsc...", "100%", "5h", "7d"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("Antigravity status should not show %q:\n%s", unwanted, text)
		}
	}
}

func TestAntigravityStatusPreservesIndependentFamilyQuotaAtRealisticWidth(t *testing.T) {
	t.Setenv("COLUMNS", "100")
	statuses := []remoteServerUsageStatus{{
		ID: "antigravity-subscription:work", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth,
		AccountIdentity: "verified@example.com", PlanType: "Google AI Pro", AuthChecked: true, AuthValid: true,
		QuotaUsageKnown: true, Windows: []accounts.UsageWindow{
			{Name: "gemini 5h", Feature: "gemini", UsedPercent: 25, LimitWindowSeconds: 18000, ResetAfterSeconds: 3600},
			{Name: "gemini weekly", Feature: "gemini", UsedPercent: 60, LimitWindowSeconds: 604800, ResetAfterSeconds: 172800},
			{Name: "claude-gpt 5h", Feature: "claude-gpt", UsedPercent: 100, LimitWindowSeconds: 18000, ResetAfterSeconds: 1800},
			{Name: "claude-gpt weekly", Feature: "claude-gpt", UsedPercent: 10, LimitWindowSeconds: 604800, ResetAfterSeconds: 432000},
		},
	}}
	rows := usageRowsFromServerUsageStatuses(statuses)
	columns := usageGridColumnsForRows(&bytes.Buffer{}, false, rows)
	keys := map[string]bool{}
	for _, column := range columns {
		keys[column.Key] = true
	}
	for _, key := range []string{"AG Gemini 5h", "AG Gemini wk", "AG 3P 5h", "AG 3P wk"} {
		if !keys[key] {
			t.Fatalf("columns = %+v, missing %s", columns, key)
		}
	}
	if keys["Pick"] {
		t.Fatalf("columns = %+v, constrained four-lane layout should drop Use", columns)
	}
	var out bytes.Buffer
	displayUsageRows(&out, rows, false)
	got := out.String()
	for _, want := range []string{
		"verified@example.com", "Google AI Pro", "G 5h", "G wk", "C/G 5h", "C/G wk",
		"75%/1h", "40%/2d", "0%/30m", "90%/5d",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Antigravity status missing %q:\n%s", want, got)
		}
	}
	assertUsageGridLineWidths(t, got, 100)
}

func TestAntigravityStatusHidesUnavailableFamilyColumns(t *testing.T) {
	t.Setenv("COLUMNS", "100")
	row := srUsageRow{
		provider: accounts.ProviderAntigravity, authMode: accounts.AuthModeOAuth,
		accountIdentity: "verified@example.com", planType: "Paid", authChecked: true, authValid: true,
		quotaUsageKnown: true,
		windows:         []accounts.UsageWindow{{Name: "gemini 5h", Feature: "gemini", UsedPercent: 20, LimitWindowSeconds: 18000}},
	}
	columns := usageGridColumns(&bytes.Buffer{}, false, row)
	keys := map[string]bool{}
	for _, column := range columns {
		keys[column.Key] = true
	}
	if !keys["AG Gemini 5h"] {
		t.Fatalf("columns = %+v, missing available Gemini 5h", columns)
	}
	for _, unavailable := range []string{"AG Gemini wk", "AG 3P 5h", "AG 3P wk"} {
		if keys[unavailable] {
			t.Fatalf("columns = %+v, rendered unavailable %s", columns, unavailable)
		}
	}
}

func TestAntigravityLegacyModelQuotaUseDoesNotClaimBaseHundredPercent(t *testing.T) {
	row := srUsageRow{
		provider: accounts.ProviderAntigravity, authMode: accounts.AuthModeOAuth,
		quotaUsageKnown: true,
		windows: []accounts.UsageWindow{
			{Name: "claude-sonnet-4.5", Feature: "claude-sonnet-4.5", UsedPercent: 70, ResetAfterSeconds: 3600},
			{Name: "claude-opus-4.1", Feature: "claude-opus-4.1", UsedPercent: 20},
		},
	}
	if got := compactPickReason(row); got != "30% left Sonnet/1h" {
		t.Fatalf("legacy compact Use = %q", got)
	}
}

func TestCompactPickReasonUsesCachedWindowsWhenUsageRefreshFails(t *testing.T) {
	row := srUsageRow{
		provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth,
		err: errors.New("usage fetch failed: 429 Too Many Requests"),
		windows: []accounts.UsageWindow{{
			Name: "5h", UsedPercent: 26, ResetAfterSeconds: 3600,
		}},
		score: selectacct.Score{
			AccountID: "claude@example.com", Headroom: .74, ShortHeadroom: .74,
			ShortResetAfterSeconds: 3600,
		},
	}
	if got := compactPickReason(row); got != "74% left" {
		t.Fatalf("cached quota Use = %q, want compact value", got)
	}
}

func TestCompactPickReasonKeepsUnavailableWithoutCachedWindows(t *testing.T) {
	row := srUsageRow{
		provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth,
		err: errors.New("usage fetch failed: 429 Too Many Requests"),
	}
	if got := compactPickReason(row); got != "usage throttled" {
		t.Fatalf("missing quota Use = %q, want usage throttled", got)
	}
}

func TestAntigravityUnknownModelQuotaDoesNotPrintPlaceholderLabel(t *testing.T) {
	row := srUsageRow{
		provider: accounts.ProviderAntigravity, authMode: accounts.AuthModeOAuth,
		quotaUsageKnown: true,
		windows:         []accounts.UsageWindow{{Name: "model", UsedPercent: 0, ResetAfterSeconds: 604800}},
	}
	if got := compactPickReason(row); got != "100% left/7d" {
		t.Fatalf("generic compact Use = %q", got)
	}
}

func TestAntigravityFamilyColumnsOmitRedundantUseAtWideWidth(t *testing.T) {
	t.Setenv("COLUMNS", "180")
	row := srUsageRow{
		provider: accounts.ProviderAntigravity, authMode: accounts.AuthModeOAuth,
		quotaUsageKnown: true,
		windows: []accounts.UsageWindow{
			{Name: "gemini-3.1-pro", Feature: "gemini-3.1-pro", UsedPercent: 1, ResetAfterSeconds: 604800},
			{Name: "claude-opus-4.1", Feature: "claude-opus-4.1", UsedPercent: 3, ResetAfterSeconds: 604800},
		},
	}
	for _, column := range usageGridColumns(&bytes.Buffer{}, false, row) {
		if column.Key == "Pick" {
			t.Fatal("wide Antigravity family table retained redundant Use column")
		}
	}
}

func TestAntigravityLegacyModelQuotaProducesCadenceNeutralFamilyColumns(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	row := srUsageRow{
		provider: accounts.ProviderAntigravity, authMode: accounts.AuthModeOAuth,
		accountIdentity: "verified@example.com", planType: "Starter", authChecked: true, authValid: true,
		quotaUsageKnown: true,
		windows: []accounts.UsageWindow{
			{Name: "gemini-3.1-pro-high", Feature: "gemini-3.1-pro-high", UsedPercent: 0, ResetAfterSeconds: 604800},
			{Name: "gemini-3.1-pro-low", Feature: "gemini-3.1-pro-low", UsedPercent: 25, ResetAfterSeconds: 432000},
			{Name: "openai-o3", Feature: "openai-o3", UsedPercent: 20, ResetAfterSeconds: 432000},
		},
	}
	columns := usageGridColumns(&bytes.Buffer{}, false, row)
	keys := map[string]bool{}
	for _, column := range columns {
		keys[column.Key] = true
	}
	if !keys["AG Gemini model"] || !keys["AG 3P model"] || keys["AG Gemini wk"] || keys["AG 3P wk"] || keys["AG Gemini 5h"] || keys["AG 3P 5h"] {
		t.Fatalf("legacy Antigravity columns = %+v", columns)
	}
	if keys["Pick"] {
		t.Fatalf("legacy Antigravity columns = %+v, redundant Use should be omitted", columns)
	}
	if got := usageGridValues(row, "")["AG Gemini model"].Text; got != "75%/5d" {
		t.Fatalf("Gemini model family = %q, want most constrained quota", got)
	}
}

func TestAntigravityStatusSurfacesFailedAuth(t *testing.T) {
	rows := usageRowsFromServerUsageStatuses([]remoteServerUsageStatus{{
		ID: "antigravity", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth,
		AuthChecked: true, AuthValid: false, Error: "refresh rejected",
	}})
	if len(rows) != 1 || !rows[0].authChecked || rows[0].authValid || usageGridState(rows[0]) != "error" {
		t.Fatalf("failed Antigravity row = %+v", rows)
	}
}

func TestKimiAPIKeySectionDoesNotClaimSubscriptionQuota(t *testing.T) {
	columns := usageGridColumns(&bytes.Buffer{}, false, srUsageRow{provider: accounts.ProviderKimi, authMode: accounts.AuthModeAPIKey})
	keys := map[string]bool{}
	for _, column := range columns {
		keys[column.Key] = true
	}
	if keys["5h"] || keys["Weekly"] || keys["Quota"] || !keys["Pick"] {
		t.Fatalf("Kimi API-key columns = %+v", columns)
	}
	if got := usageGridProviderQuota(srUsageRow{provider: accounts.ProviderKimi, authMode: accounts.AuthModeAPIKey}); got != "OAuth only" {
		t.Fatalf("Kimi API-key quota = %q", got)
	}
	if got := usageGridProviderQuota(srUsageRow{provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey}); got != "console only" {
		t.Fatalf("Qwen Token Plan quota = %q", got)
	}
}

func TestAPIKeyProviderStatusUsesCompactRoutingColumns(t *testing.T) {
	t.Setenv("COLUMNS", "120")
	var out bytes.Buffer
	rows := []srUsageRow{
		{email: "deepseek:primary", provider: accounts.ProviderDeepSeek, authMode: accounts.AuthModeAPIKey, providerHealth: "auth ok", planType: "credits, per token", assignedSessions: 2, sessionsKnown: true},
		{email: "deepseek:reserve", provider: accounts.ProviderDeepSeek, authMode: accounts.AuthModeAPIKey, providerHealth: "auth ok", planType: "credits, per token", sessionsKnown: true},
	}
	rankUsageRows(rows)
	displayUsageRows(&out, rows, false)
	text := out.String()
	for _, want := range []string{"Deepseek accounts", "Account", "Plan", "State", "Use", "credits", "active", "rec", "ready", "quota not exposed"} {
		if !strings.Contains(text, want) {
			t.Fatalf("compact API-key provider status should show %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"Key ID", "Sessions", "Models", "Endpoints", "Quota", "5h", "7d"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("compact API-key provider status should not show %q:\n%s", unwanted, text)
		}
	}
}

func TestQwenStatusKeepsMultipleKeysAsSeparateAccounts(t *testing.T) {
	var out bytes.Buffer
	rows := []srUsageRow{
		{email: "qwen-token:team-a", provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey, planType: "Lite", providerHealth: "auth ok", quotaStatus: "live", accountIdentity: "first@example.com", keyFingerprint: "key:1111111111", assignedSessions: 2, sessionsKnown: true, windows: []accounts.UsageWindow{{Name: "7d", UsedPercent: 25, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAfterSeconds: 2 * 86400}}},
		{email: "qwen-token:team-b", provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey, planType: "Pro", providerHealth: "auth ok", quotaStatus: "live", accountIdentity: "second@example.com", keyFingerprint: "key:2222222222", assignedSessions: 0, sessionsKnown: true, windows: []accounts.UsageWindow{{Name: "5h", UsedPercent: 10, LimitWindowSeconds: int64((5 * time.Hour) / time.Second), ResetAfterSeconds: 3600}, {Name: "7d", UsedPercent: 40, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAfterSeconds: 3 * 86400}}},
	}
	for i := range rows {
		rows[i].quotaUsageKnown = true
		rows[i].score = scoreFromWindows(rows[i].email, rows[i].windows)
	}
	rankUsageRows(rows)
	displayUsageRows(&out, rows, false)
	text := out.String()
	for _, want := range []string{"Lite", "Pro", "active", "rec", "75% left", "60% left", "75%/2d", "90%/1h", "60%/3d", "first@example.com", "second@example.com"} {
		if !strings.Contains(text, want) {
			t.Fatalf("Qwen status should show %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"qwen-token:team-", "key:1111111111", "key:2222222222", "Sessions", "Login", "Key ID", "unknown"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("Qwen status should not show %q:\n%s", unwanted, text)
		}
	}
}

func TestQwenStatusOmitsUnreportedQuotaWindow(t *testing.T) {
	var out bytes.Buffer
	rows := []srUsageRow{{
		email: "qwen-token:weekly-only", provider: accounts.ProviderQwenToken,
		authMode: accounts.AuthModeAPIKey, providerHealth: "auth ok", quotaStatus: "live",
		quotaUsageKnown: true, windows: []accounts.UsageWindow{{Name: "7d", UsedPercent: 5}},
	}}
	rows[0].score = scoreFromWindows(rows[0].email, rows[0].windows)
	rankUsageRows(rows)
	displayUsageRows(&out, rows, false)
	text := out.String()
	if strings.Contains(text, "5h") || strings.Contains(text, "unknown") || !strings.Contains(text, "7d") {
		t.Fatalf("Qwen weekly-only status should omit the unreported 5h window:\n%s", text)
	}
}

func TestQwenStatusDisambiguatesSharedLoginWithSavedLabel(t *testing.T) {
	t.Setenv("COLUMNS", "160")
	var out bytes.Buffer
	displayUsageRows(&out, []srUsageRow{
		{email: "qwen-token:large", provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey, accountIdentity: "same@example.com"},
		{email: "qwen-token:small", provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey, accountIdentity: "same@example.com"},
	}, false)
	for _, want := range []string{"same@example.com (large)", "same@example.com (small)"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("duplicate Qwen login should retain saved-label disambiguation %q:\n%s", want, out.String())
		}
	}
}

func TestQwenRemoteLiveQuotaDoesNotImplyModelKeyHealth(t *testing.T) {
	row := srUsageRow{provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey, quotaStatus: "live"}
	if got := usageGridState(row); got != "quota live" {
		t.Fatalf("state = %q", got)
	}
	if got := usageGridStateColor(row); got == ansiGreen {
		t.Fatal("an unprobed remote model key was rendered healthy")
	}
}

func TestQwenRemoteStatusStillShowsQuotaStateWithoutLocalHealthProbe(t *testing.T) {
	row := srUsageRow{provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey, quotaStatus: "login needed"}
	if got := usageGridState(row); got != "login needed" {
		t.Fatalf("Qwen remote state = %q", got)
	}
}

func TestQwenValidatedKeyStaysReadyWhenConsoleLoginExpires(t *testing.T) {
	row := srUsageRow{
		provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey,
		providerHealth: "auth ok", quotaStatus: "login needed",
		err:     errors.New("Qwen console login needed"),
		windows: []accounts.UsageWindow{{Name: "7d", UsedPercent: 20, ResetAfterSeconds: 3600}},
	}
	if !displayRecommendedForNewSession(row) {
		t.Fatal("valid Qwen routing key became ineligible when optional telemetry expired")
	}
	if got := usageGridState(row); got != "ready" {
		t.Fatalf("state = %q, want ready", got)
	}
	if got := compactPickReason(row); got != "quota login needed" {
		t.Fatalf("Use = %q, want quota login needed", got)
	}
	if got := usageGridStateColor(row); got == ansiRed {
		t.Fatal("telemetry-only failure rendered valid routing key red")
	}

	row.active = true
	row.gtoRecommended = true
	if got := usageGridState(row); got != "active, rec" {
		t.Fatalf("active state = %q, want active, rec", got)
	}
}

func TestQwenValidatedKeyWithoutTelemetryIsReadyButKnownExhaustionBlocks(t *testing.T) {
	row := srUsageRow{
		provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey,
		providerHealth: "auth ok",
	}
	if usageGridState(row) != "ready" || compactPickReason(row) != "quota not exposed" ||
		!displayRecommendedForNewSession(row) {
		t.Fatalf("missing optional telemetry contaminated routing status: %+v", row)
	}

	row.quotaUsageKnown = true
	row.quotaStatus = "live"
	row.windows = []accounts.UsageWindow{{
		Name: "7d", UsedPercent: 100,
		LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second),
	}}
	row.score = scoreFromWindows("qwen-token:work", row.windows)
	if displayRecommendedForNewSession(row) {
		t.Fatal("known exhausted Qwen quota remained eligible")
	}
}

func TestRemoteQwenValidatedKeyKeepsTelemetryFailureSeparate(t *testing.T) {
	rows := usageRowsFromServerUsageStatuses([]remoteServerUsageStatus{{
		ID: "qwen-token:work", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey,
		AuthChecked: true, AuthValid: true, QuotaStatus: "login needed",
		Error: "Qwen console login needed",
	}})
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	row := rows[0]
	if row.providerHealth != "auth ok" || usageGridState(row) != "rec" ||
		!displayRecommendedForNewSession(row) || compactPickReason(row) != "quota login needed" {
		t.Fatalf("remote Qwen telemetry failure contaminated routing status: %+v", row)
	}
}

func TestLocalQwenStatusExplainsExpiredConsoleLoginOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SUBROUTER_STATE_DIR", t.TempDir())
	store := accounts.DefaultCodexStore()
	stored := accounts.StoredCodexAccount{
		Email: "qwen-token:work", Provider: accounts.ProviderQwenToken,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "model-secret"},
	}
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	if err := agentqwen.SaveConsoleCredential(stored.Email, agentqwen.ConsoleCredential{
		AccessToken: "expired-console-token", Account: "saved-account@example.test",
	}); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"code":"BailianGateway.Login.NotLogined","message":"login expired"}`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	rows, err := (srRunner{store: store, client: client}).fetchUsageRows(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.email != stored.Email {
			continue
		}
		if row.quotaStatus != "login needed" || row.accountIdentity != "saved-account@example.test" || row.err == nil ||
			!strings.Contains(row.err.Error(), "sr qwen login 'qwen-token:work'") || strings.Count(row.err.Error(), "login needed") != 1 {
			t.Fatalf("expired local Qwen row = %+v", row)
		}
		return
	}
	t.Fatal("Qwen status row was missing")
}

func TestUsageRowsFromServerPreserveKeyIdentityAndSessions(t *testing.T) {
	rows := usageRowsFromServerUsageStatuses([]remoteServerUsageStatus{{
		ID:               "qwen-token:large-plan",
		Provider:         accounts.ProviderQwenToken,
		AuthMode:         accounts.AuthModeAPIKey,
		KeyFingerprint:   "key:1234567890",
		AssignedSessions: 3,
		SessionsKnown:    true,
	}})
	if len(rows) != 1 || rows[0].keyFingerprint != "key:1234567890" || rows[0].assignedSessions != 3 || !rows[0].sessionsKnown {
		t.Fatalf("remote Qwen identity/session fields lost: %+v", rows)
	}
}

func TestUsageRowsFromServerApplyKimiAPIKeyQuota(t *testing.T) {
	rows := usageRowsFromServerUsageStatuses([]remoteServerUsageStatus{{
		ID: "kimi:work-key", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeAPIKey,
		AuthChecked: true, AuthValid: true, QuotaUsageKnown: true,
		Windows: []accounts.UsageWindow{
			{Name: "5h", UsedPercent: 100, LimitWindowSeconds: int64((5 * time.Hour) / time.Second), ResetAfterSeconds: 900},
			{Name: "weekly", UsedPercent: 20, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
		},
	}})
	if len(rows) != 1 || rows[0].providerHealth != "auth ok" || !rows[0].tempCooked || rows[0].score.ShortHeadroom != 0 {
		t.Fatalf("remote Kimi quota was not applied: %+v", rows)
	}
}

func TestUsageRowsFromServerRecommendAValidatedKimiAPIKey(t *testing.T) {
	rows := usageRowsFromServerUsageStatuses([]remoteServerUsageStatus{{
		ID: "kimi:healthy", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeAPIKey,
		AuthChecked: true, AuthValid: true, QuotaUsageKnown: true,
		Windows: []accounts.UsageWindow{
			{Name: "5h", UsedPercent: 10, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)},
			{Name: "weekly", UsedPercent: 20, LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
		},
	}})
	if len(rows) != 1 || rows[0].providerHealth != "auth ok" || !displayRecommendedForNewSession(rows[0]) {
		t.Fatalf("validated remote Kimi key was not recommendable: %+v", rows)
	}
}

func TestKeyedProviderSectionFitsNarrowTerminal(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	columns := usageGridColumns(&bytes.Buffer{}, false, srUsageRow{provider: accounts.ProviderQwenToken, authMode: accounts.AuthModeAPIKey})
	if width := usageGridWidth(columns); width > 80 {
		t.Fatalf("keyed provider grid width = %d, want <= 80", width)
	}
}

// One subscription reachable over two protocols must read as one account
// listing both endpoints, not as two accounts.
func TestProviderEndpointsCollapseASharedSubscription(t *testing.T) {
	for _, provider := range []accounts.Provider{accounts.ProviderQwenToken, accounts.ProviderQwenAnthropic} {
		got := proxy.ProviderEndpoints(provider)
		want := []string{"/qwen-anthropic", "/qwen-token"}
		if !slices.Equal(got, want) {
			t.Fatalf("provider %q lists %v, want both protocol endpoints", provider, got)
		}
	}
	if got := proxy.ProviderEndpoints(accounts.ProviderGrok); len(got) != 1 || got[0] != "/grok" {
		t.Fatalf("a provider that owns its credential lists %v, want just its own", got)
	}
}

// The health probe must classify what the vendor actually returns, since these
// providers offer no quota API and key validity is the only live signal.
func TestProbeProviderKeyClassifiesTheResponse(t *testing.T) {
	if state, models := probeProviderKey(context.Background(), nil, accounts.ProviderQwenAnthropic, "", "k"); state != "" || models != -1 {
		t.Fatalf("a provider with no health endpoint should report nothing, got %q %d", state, models)
	}
	cases := []struct {
		status int
		body   string
		state  string
		models int
	}{
		{http.StatusUnauthorized, `{}`, "bad key", -1},
		{http.StatusForbidden, `{}`, "denied", -1},
		{http.StatusBadGateway, `{}`, "http 502", -1},
		{http.StatusOK, `{"data":[{},{}]}`, "auth ok", 2},
		{http.StatusOK, `{`, "auth ok", -1},
	}
	for _, tc := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}))
		state, models := probeProviderKey(context.Background(), nil, accounts.ProviderQwenToken, server.URL, "k")
		server.Close()
		if state != tc.state || models != tc.models {
			t.Fatalf("status %d body %q => (%q, %d), want (%q, %d)", tc.status, tc.body, state, models, tc.state, tc.models)
		}
	}
	if state := usageGridState(srUsageRow{providerHealth: "bad key"}); state != "bad key" {
		t.Fatalf("a keyed provider's state should come from its probe, got %q", state)
	}
	if state := usageGridState(srUsageRow{providerHealth: "not checked"}); state != "not checked" {
		t.Fatalf("an unprobed provider should be explicit, got %q", state)
	}
	if color := usageGridStateColor(srUsageRow{providerHealth: "not checked"}); color != "" {
		t.Fatalf("an unprobed key is not known-bad and should not be red, got %q", color)
	}
	// A row with no probe result falls back to the scheduler's state.
	if state := usageGridState(srUsageRow{active: true}); state != "active" {
		t.Fatalf("a non-keyed row should keep the scheduler state, got %q", state)
	}
	if got := usageGridModels(srUsageRow{providerModels: -1}); got != "?" {
		t.Fatalf("an unknown model count should render %q, got %q", "?", got)
	}
	if got := usageGridModels(srUsageRow{providerModels: 11}); got != "11" {
		t.Fatalf("model count = %q, want 11", got)
	}
}

// The flag defaults and the health probe must read the same base URL, or a
// probe silently checks an address the proxy does not use.
func TestProviderDefaultUpstreamsAreDeclared(t *testing.T) {
	for _, provider := range []accounts.Provider{
		accounts.ProviderKimi, accounts.ProviderZAI, accounts.ProviderOpenRouter,
		accounts.ProviderDeepSeek, accounts.ProviderTogether, accounts.ProviderFireworks,
		accounts.ProviderOpenCodeZen,
		accounts.ProviderGrok, accounts.ProviderQwen, accounts.ProviderQwenToken,
		accounts.ProviderQwenAnthropic,
	} {
		if proxy.ProviderDefaultUpstream(provider) == "" {
			t.Fatalf("provider %q declares no default upstream", provider)
		}
		if proxy.ProviderMetering(provider) == "" {
			t.Fatalf("provider %q declares no metering description", provider)
		}
	}
}

// The name sr list prints must select the account, even when the account is
// stored under a stable codex-owner key.
func TestFindStoredAcceptsListedDisplayName(t *testing.T) {
	store := accounts.CodexStore{Dir: t.TempDir()}
	account := accounts.StoredCodexAccount{
		Email: "codex-owner-stable-key", AddedAt: "2026-01-01T00:00:00Z",
		Auth: testCodexAuth("owner@example.com", "workspace-owner"),
	}
	if err := store.SaveStored(account); err != nil {
		t.Fatal(err)
	}
	listed := localAccountDisplayName(account, false)
	found, ok, err := store.FindStored(listed)
	if err != nil || !ok || found.Email != "codex-owner-stable-key" {
		t.Fatalf("FindStored(%q) = %q, %v, %v; want the stable-key account", listed, found.Email, ok, err)
	}
}

// Two workspaces that list as the same text are ambiguous, not missing, so
// the error points at sr list --ids instead of "no account found".
func TestFindStoredReportsDuplicateDisplayNameAsAmbiguous(t *testing.T) {
	store := accounts.CodexStore{Dir: t.TempDir()}
	for _, key := range []string{"codex-owner-a", "codex-owner-b"} {
		if err := store.SaveStored(accounts.StoredCodexAccount{
			Email: key, AddedAt: "2026-01-01T00:00:00Z",
			Auth: testCodexAuth("shared@example.com", "workspace-"+key),
		}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := store.ListStored()
	if err != nil || len(all) != 2 {
		t.Fatalf("ListStored = %d, %v", len(all), err)
	}
	listed := localAccountDisplayName(all[0], false)
	if other := localAccountDisplayName(all[1], false); other != listed {
		t.Skipf("fixture accounts list differently (%q, %q)", listed, other)
	}
	_, ok, err := store.FindStored(listed)
	if ok || err == nil || !strings.Contains(err.Error(), "multiple accounts match") {
		t.Fatalf("FindStored(%q) = %v, %v; want an ambiguity error", listed, ok, err)
	}
}

// A server account is shown by its label, never by its stable key, and the
// key only appears when asked for.
func TestRemoteAccountDisplayNamePrefersLabel(t *testing.T) {
	for _, tc := range []struct {
		id, label, email string
		mode             accounts.AuthMode
		want             string
	}{
		{"codex-owner-1", "owner@example.com [team]", "codex-owner-1", accounts.AuthModeOAuth, "owner@example.com [team]"},
		{"codex-owner-1", "", "owner@example.com", accounts.AuthModeOAuth, "owner@example.com"},
		{"apikey:work", "work", "", accounts.AuthModeAPIKey, "apikey:work"},
	} {
		if got := remoteAccountDisplayName(tc.id, tc.label, tc.email, tc.mode); got != tc.want {
			t.Fatalf("remoteAccountDisplayName(%q, %q, %q) = %q, want %q", tc.id, tc.label, tc.email, got, tc.want)
		}
	}
}
