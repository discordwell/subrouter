package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/proxy"
)

func TestClaudeAddPublishesProfileToRunningAccountRef(t *testing.T) {
	root := t.TempDir()
	store := claude.Store{Dir: root}
	accountStore := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	ref, err := proxy.OpenAccountRef(accountStore, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	beforeGeneration := ref.Generation()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(binDir, "claude")
	script := `#!/bin/sh
if [ "$1" = "/login" ]; then
  printf '%s\n' '{"claudeAiOauth":{"accessToken":"claude-access","refreshToken":"claude-refresh","expiresAt":4102444800000}}' > "$CLAUDE_CONFIG_DIR/.credentials.json"
  exit 0
fi
if [ "$1" = "auth" ] && [ "$2" = "status" ]; then
  printf '%s\n' '{"loggedIn":true,"email":"work@example.com","subscriptionType":"max"}'
  exit 0
fi
exit 1
`
	if err := os.WriteFile(claudePath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out bytes.Buffer
	runner := claudeRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(t.Context(), []string{"login", "work"}); err != nil {
		t.Fatal(err)
	}
	triggerClaudeAccountReload(t, ref)
	if ref.Generation() <= beforeGeneration {
		t.Fatalf("account generation = %d, want greater than %d", ref.Generation(), beforeGeneration)
	}
	if !containsClaudeAccount(ref.All(), "work") {
		t.Fatalf("running account snapshot did not load added Claude profile: %+v", ref.All())
	}
}

func TestClaudeLoginAcceptsEmailProfileName(t *testing.T) {
	root := t.TempDir()
	store := claude.Store{Dir: root}
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(binDir, "claude")
	script := `#!/bin/sh
if [ "$1" = "/login" ]; then
  printf '%s\n' '{"claudeAiOauth":{"accessToken":"claude-access","refreshToken":"claude-refresh","expiresAt":4102444800000}}' > "$CLAUDE_CONFIG_DIR/.credentials.json"
  exit 0
fi
if [ "$1" = "auth" ] && [ "$2" = "status" ]; then
  printf '%s\n' '{"loggedIn":true,"email":"work@example.com","subscriptionType":"max"}'
  exit 0
fi
exit 1
`
	if err := os.WriteFile(claudePath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out bytes.Buffer
	runner := claudeRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(t.Context(), []string{"login", "work@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.FindProfile("work@example.com"); !ok {
		t.Fatalf("email-named Claude profile was not registered: %+v", store.ListProfiles())
	}
}

func TestClaudeLoginExistingProfileReloginsInPlace(t *testing.T) {
	root := t.TempDir()
	store := claude.Store{Dir: root}
	if _, err := store.CreateProfile("work@example.com"); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(binDir, "claude")
	script := `#!/bin/sh
if [ "$1" = "/login" ]; then
  printf '%s\n' '{"claudeAiOauth":{"accessToken":"claude-access-2","refreshToken":"claude-refresh-2","expiresAt":4102444800000}}' > "$CLAUDE_CONFIG_DIR/.credentials.json"
  exit 0
fi
if [ "$1" = "auth" ] && [ "$2" = "status" ]; then
  printf '%s\n' '{"loggedIn":true,"email":"work@example.com","subscriptionType":"max"}'
  exit 0
fi
exit 1
`
	if err := os.WriteFile(claudePath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out bytes.Buffer
	runner := claudeRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(t.Context(), []string{"login", "work@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.FindProfile("work@example.com"); !ok {
		t.Fatal("re-login removed the pre-existing Claude profile")
	}
}

func TestClaudeFailedLoginKeepsExistingProfile(t *testing.T) {
	root := t.TempDir()
	store := claude.Store{Dir: root}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	runner := claudeRunner{store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	if err := runner.run(t.Context(), []string{"login", "work"}); err == nil {
		t.Fatal("failed Claude login unexpectedly succeeded")
	}
	if _, ok := store.FindProfile("work"); !ok {
		t.Fatal("failed re-login removed the pre-existing Claude profile")
	}
}

func TestClaudeRemovePublishesProfileToRunningAccountRef(t *testing.T) {
	root := t.TempDir()
	store := claude.Store{Dir: root}
	if _, err := store.UpsertCredentialProfile("work", claude.CredentialInfo{
		AccessToken: "claude-access", RefreshToken: "claude-refresh",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	accountStore := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	ref, err := proxy.OpenAccountRef(accountStore, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !containsClaudeAccount(ref.All(), "work") {
		t.Fatalf("initial account snapshot omitted Claude profile: %+v", ref.All())
	}
	beforeGeneration := ref.Generation()
	var out bytes.Buffer
	runner := claudeRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(t.Context(), []string{"remove", "work"}); err != nil {
		t.Fatal(err)
	}
	triggerClaudeAccountReload(t, ref)
	if ref.Generation() <= beforeGeneration {
		t.Fatalf("account generation = %d, want greater than %d", ref.Generation(), beforeGeneration)
	}
	if containsClaudeAccount(ref.All(), "work") {
		t.Fatalf("running account snapshot retained removed Claude profile: %+v", ref.All())
	}
}

func TestClaudeFailedAddPublishesRollbackToRunningAccountRef(t *testing.T) {
	root := t.TempDir()
	store := claude.Store{Dir: root}
	accountStore := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	ref, err := proxy.OpenAccountRef(accountStore, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	beforeGeneration := ref.Generation()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	runner := claudeRunner{store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	if err := runner.run(t.Context(), []string{"login", "incomplete"}); err == nil {
		t.Fatal("failed Claude login unexpectedly succeeded")
	}
	if _, ok := store.FindProfile("incomplete"); ok {
		t.Fatal("failed Claude login retained its temporary profile")
	}
	triggerClaudeAccountReload(t, ref)
	if ref.Generation() <= beforeGeneration {
		t.Fatalf("account generation = %d, want rollback greater than %d", ref.Generation(), beforeGeneration)
	}
	if containsClaudeAccount(ref.All(), "incomplete") {
		t.Fatalf("running account snapshot retained rolled-back Claude profile: %+v", ref.All())
	}
}

func TestClaudeNamedAddReconcilesCanceledCompletionPublication(t *testing.T) {
	root := t.TempDir()
	store := claude.Store{Dir: root}
	accountStore := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	ref, err := proxy.OpenAccountRef(accountStore, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	beforeGeneration := ref.Generation()
	installSuccessfulClaudeLoginCLI(t, root, "work@example.com")

	ctx, cancel := context.WithCancel(t.Context())
	runner := claudeRunner{
		store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard,
		afterAuthVerified: cancel,
	}
	if err := runner.run(ctx, []string{"login", "work"}); err != nil {
		t.Fatalf("completed Claude login was not reconciled after cancellation: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("test did not cancel the original command context")
	}
	triggerClaudeAccountReload(t, ref)
	if ref.Generation() <= beforeGeneration {
		t.Fatalf("account generation = %d, want greater than %d", ref.Generation(), beforeGeneration)
	}
	if !containsClaudeAccount(ref.All(), "work") {
		t.Fatalf("running account snapshot did not load reconciled Claude profile: %+v", ref.All())
	}
	credential, err := store.ReadCredential(t.Context(), store.ClaudeConfigDir("work"))
	if err != nil || credential == nil || credential.AccessToken != "claude-access" {
		t.Fatalf("reconciled credential = %+v, err = %v", credential, err)
	}
}

func TestClaudeNamedAddRemovesCredentialAfterPersistentPublicationFailure(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS Keychain is only available on Darwin")
	}
	root := t.TempDir()
	store := claude.Store{Dir: root}
	accountStore := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	ref, err := proxy.OpenAccountRef(accountStore, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	installSuccessfulClaudeLoginCLI(t, root, "work@example.com")
	// Force secret deletion to fail. Ordinary RemoveProfileContext restores the
	// profile in this case, which lets a later unrelated generation expose the
	// never-published authenticated credential.
	securityPath := filepath.Join(root, "bin", "security")
	if err := os.WriteFile(securityPath, []byte("#!/bin/sh\necho 'forced keychain deletion failure' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	generationPath := filepath.Join(root, ".account-generation")
	credentialDir := ""

	runner := claudeRunner{
		store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard,
		afterAuthVerified: func() {
			profile, ok := store.FindProfile("work")
			if !ok {
				t.Fatal("named profile disappeared before completion publication")
			}
			credentialDir = store.PreferredInstancePath(filepath.Join(store.InstancesDir(), profile.Dir))
			if err := os.Remove(generationPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(generationPath, 0o700); err != nil {
				t.Fatal(err)
			}
		},
	}
	if err := runner.run(t.Context(), []string{"login", "work"}); err == nil {
		t.Fatal("named add unexpectedly succeeded with persistent publication failure")
	}
	if _, ok := store.FindProfile("work"); ok {
		t.Fatal("failed named add retained its profile")
	}
	if credentialDir == "" {
		t.Fatal("test did not capture the authenticated credential directory")
	}
	if _, err := os.Stat(credentialDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed named add retained credential directory: %v", err)
	}
	rollbackMarker := filepath.Join(root, ".account-rollback-active")
	if _, err := os.Stat(rollbackMarker); err != nil {
		t.Fatalf("incomplete Keychain cleanup did not retain fail-closed rollback marker: %v", err)
	}

	// Repair both synthetic failures. The next transaction must replay the exact
	// path-keyed Keychain cleanup before clearing the marker and publishing any
	// unrelated account state.
	keychainRetryRecord := filepath.Join(root, "keychain-cleanup-retry")
	securityScript := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + keychainRetryRecord + "\"\nexit 0\n"
	if err := os.WriteFile(securityPath, []byte(securityScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(generationPath); err != nil {
		t.Fatal(err)
	}
	if err := proxy.PublishAccountDiskMutation(t.Context(), root, func() (bool, error) {
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rollbackMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful cleanup retained rollback marker: %v", err)
	}
	if record, err := os.ReadFile(keychainRetryRecord); err != nil || !strings.Contains(string(record), "delete-generic-password") {
		t.Fatalf("Keychain cleanup replay = %q, err %v", record, err)
	}
	triggerClaudeAccountReload(t, ref)
	if containsClaudeAccount(ref.All(), "work") {
		t.Fatalf("later unrelated generation exposed failed Claude add: %+v", ref.All())
	}
}

func TestClaudeNamedAddPreservesCredentialAfterCompletionPublicationTeardownErrors(t *testing.T) {
	root := t.TempDir()
	store := claude.Store{Dir: root}
	accountStore := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	ref, err := proxy.OpenAccountRef(accountStore, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	installSuccessfulClaudeLoginCLI(t, root, "work@example.com")
	wantErr := errors.New("account transaction teardown failed")
	mutations := 0

	runner := claudeRunner{
		store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard,
		mutateProfileInventoryForTest: func(ctx context.Context, mutate func() (bool, error)) error {
			mutations++
			if err := proxy.PublishAccountDiskMutation(ctx, store.Dir, mutate); err != nil {
				return err
			}
			if mutations >= 2 {
				return wantErr
			}
			return nil
		},
	}
	err = runner.run(t.Context(), []string{"login", "work"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want post-commit completion teardown error", err)
	}
	profile, ok := store.FindProfile("work")
	if !ok {
		t.Fatal("post-commit completion teardown errors removed registered profile")
	}
	credential, readErr := store.ReadCredential(t.Context(), store.PreferredInstancePath(filepath.Join(store.InstancesDir(), profile.Dir)))
	if readErr != nil || credential == nil || credential.AccessToken != "claude-access" {
		t.Fatalf("registered credential = %+v, err = %v", credential, readErr)
	}
	triggerClaudeAccountReload(t, ref)
	if !containsClaudeAccount(ref.All(), "work") {
		t.Fatalf("published account snapshot omitted committed Claude profile: %+v", ref.All())
	}
}

func TestClaudeUnnamedAddCleansAuthenticatedTempAfterCanceledPublication(t *testing.T) {
	root := t.TempDir()
	store := claude.Store{Dir: root}
	installSuccessfulClaudeLoginCLI(t, root, "work@example.com")

	ctx, cancel := context.WithCancel(t.Context())
	runner := claudeRunner{
		store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard,
		afterAuthVerified: cancel,
	}
	if err := runner.run(ctx, []string{"login"}); err == nil {
		t.Fatal("unnamed add unexpectedly succeeded with canceled publication")
	}
	if profiles := store.ListProfiles(); len(profiles) != 0 {
		t.Fatalf("failed unnamed add registered profiles: %+v", profiles)
	}
	entries, err := os.ReadDir(store.InstancesDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("failed unnamed add retained authenticated temporary instance: %s", entry.Name())
		}
	}
}

func TestClaudeUnnamedAddPreservesRegisteredCredentialAfterPublicationTeardownError(t *testing.T) {
	root := t.TempDir()
	store := claude.Store{Dir: root}
	accountStore := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	ref, err := proxy.OpenAccountRef(accountStore, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	installSuccessfulClaudeLoginCLI(t, root, "work@example.com")
	wantErr := errors.New("account transaction teardown failed")

	runner := claudeRunner{
		store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard,
		mutateProfileInventoryForTest: func(ctx context.Context, mutate func() (bool, error)) error {
			if err := proxy.PublishAccountDiskMutation(ctx, store.Dir, mutate); err != nil {
				return err
			}
			return wantErr
		},
	}
	err = runner.run(t.Context(), []string{"login"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want post-commit teardown error", err)
	}
	profile, ok := store.FindProfile("work@example.com")
	if !ok {
		t.Fatal("post-commit teardown error removed registered profile")
	}
	credential, readErr := store.ReadCredential(t.Context(), store.PreferredInstancePath(filepath.Join(store.InstancesDir(), profile.Dir)))
	if readErr != nil || credential == nil || credential.AccessToken != "claude-access" {
		t.Fatalf("registered credential = %+v, err = %v", credential, readErr)
	}
	triggerClaudeAccountReload(t, ref)
	if !containsClaudeAccount(ref.All(), "work@example.com") {
		t.Fatalf("published account snapshot omitted committed Claude profile: %+v", ref.All())
	}
}

func TestClaudeNamedAddRemovesCreatedProfileAfterPublicationTeardownError(t *testing.T) {
	root := t.TempDir()
	store := claude.Store{Dir: root}
	accountStore := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	ref, err := proxy.OpenAccountRef(accountStore, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	installSuccessfulClaudeLoginCLI(t, root, "work@example.com")
	wantErr := errors.New("account transaction teardown failed")

	runner := claudeRunner{
		store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard,
		mutateProfileInventoryForTest: func(ctx context.Context, mutate func() (bool, error)) error {
			if err := proxy.PublishAccountDiskMutation(ctx, store.Dir, mutate); err != nil {
				return err
			}
			return wantErr
		},
	}
	err = runner.run(t.Context(), []string{"login", "work"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want post-commit teardown error", err)
	}
	if _, ok := store.FindProfile("work"); ok {
		t.Fatal("post-commit teardown error retained the incomplete named profile")
	}
	triggerClaudeAccountReload(t, ref)
	if containsClaudeAccount(ref.All(), "work") {
		t.Fatalf("published account snapshot retained rolled-back Claude profile: %+v", ref.All())
	}
}
func installSuccessfulClaudeLoginCLI(t *testing.T, root, email string) {
	t.Helper()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(binDir, "claude")
	script := `#!/bin/sh
if [ "$1" = "/login" ]; then
  printf '%s\n' '{"claudeAiOauth":{"accessToken":"claude-access","refreshToken":"claude-refresh","expiresAt":4102444800000}}' > "$CLAUDE_CONFIG_DIR/.credentials.json"
  exit 0
fi
if [ "$1" = "auth" ] && [ "$2" = "status" ]; then
  printf '%s\n' '{"loggedIn":true,"email":"` + email + `","subscriptionType":"max"}'
  exit 0
fi
exit 1
`
	if err := os.WriteFile(claudePath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func triggerClaudeAccountReload(t *testing.T, ref *proxy.AccountRef) {
	t.Helper()
	handler := proxy.Server{AccountRef: ref, MaxBodyBytes: 1 << 20}.Handler()
	request := httptest.NewRequest(http.MethodGet, "/_subrouter/accounts", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	// Loopback trust needs a loopback Host (PATCHES §29), as the sr CLI sends.
	request.Host = "127.0.0.1:31415"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("account reload request status=%d body=%s", response.Code, response.Body.String())
	}
}

func containsClaudeAccount(all []accounts.Account, id string) bool {
	for _, account := range all {
		if account.Provider == accounts.ProviderClaude && account.ID == id {
			return true
		}
	}
	return false
}

func TestClaudeEnvPrintsActiveProfile(t *testing.T) {
	store := claude.Store{Dir: t.TempDir()}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := claudeRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"env"}); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if !strings.Contains(got, "export CLAUDE_CONFIG_DIR=") {
		t.Fatalf("env output = %q", got)
	}
	if !strings.Contains(got, "/claude/work") {
		t.Fatalf("env output missing profile path: %q", got)
	}
}

// `sr claude env <name>` prints THAT profile's directory. It used to ignore the
// name and print the active profile's: on 2026-08-28 it answered main's
// directory for both seven-alpha and ten-alpha, and a re-auth pointed there
// overwrote main (PATCHES §36).
func TestClaudeEnvHonoursTheName(t *testing.T) {
	store := claude.Store{Dir: t.TempDir()}
	for _, name := range []string{"main", "seven-alpha", "ten-alpha"} {
		if _, err := store.CreateProfile(name); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetActiveProfile("main"); err != nil {
		t.Fatal(err)
	}
	envFor := func(args ...string) (string, error) {
		var out bytes.Buffer
		runner := claudeRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
		err := runner.run(context.Background(), append([]string{"env"}, args...))
		return strings.TrimSpace(out.String()), err
	}
	want := func(name string) string {
		return "export CLAUDE_CONFIG_DIR=" + store.ClaudeConfigDir(name)
	}
	// Calibration: the three directories differ, so an answer naming the wrong
	// profile cannot pass for the right one.
	if want("main") == want("seven-alpha") || want("seven-alpha") == want("ten-alpha") {
		t.Fatalf("fixture profiles share a directory; the test proves nothing")
	}

	for _, name := range []string{"seven-alpha", "ten-alpha", "main"} {
		if got, err := envFor(name); err != nil || got != want(name) {
			t.Fatalf("sr claude env %s = %q, %v; want %q", name, got, err, want(name))
		}
	}
	// The name resolves as `sr claude addr` resolves it: exact, else the one
	// profile that differs only in case.
	if got, err := envFor("SEVEN-Alpha"); err != nil || got != want("seven-alpha") {
		t.Fatalf("sr claude env SEVEN-Alpha = %q, %v; want %q", got, err, want("seven-alpha"))
	}
	// Not by fragment, as `run` and `switch` match: the output decides where a
	// sign-in writes, and `sr claude env seven` for a new account "seven" must
	// not hand over seven-alpha's directory for the login to overwrite. It
	// fails, and says which profile the fragment would have meant.
	if got, err := envFor("seven"); err == nil || strings.Contains(got, "export") || !strings.Contains(err.Error(), `"seven-alpha"`) {
		t.Fatalf("sr claude env seven = %q, %v; want an error naming seven-alpha and no export line", got, err)
	}
	// No name is still the active profile, for eval "$(sr claude env)" in a
	// shell rc.
	if got, err := envFor(); err != nil || got != want("main") {
		t.Fatalf("sr claude env = %q, %v; want %q", got, err, want("main"))
	}
	// Naming a profile reads it; it does not switch to it.
	if active := store.ActiveProfile(); active != "main" {
		t.Fatalf("active = %q after naming other profiles, want main", active)
	}
	// An unknown or ambiguous name, a second name and an option all fail and
	// print no export line, so an eval of the output exports nothing. So does
	// an empty name -- `sr claude env "$acct"` with $acct unset -- which must
	// not fall back to the active profile the way no name at all does.
	if _, _, err := store.MatchProfile("alpha"); err == nil {
		t.Fatalf("calibration: alpha must match more than one profile, or the ambiguous case proves nothing")
	}
	for _, args := range [][]string{{"nine-alpha"}, {"alpha"}, {""}, {" "}, {"seven-alpha", "ten-alpha"}, {"--json"}} {
		got, err := envFor(args...)
		if err == nil || strings.Contains(got, "export") {
			t.Fatalf("sr claude env %q = %q, %v; want an error and no export line", args, got, err)
		}
	}
}

func TestClaudeEnvPrefersCodexAccountsAlias(t *testing.T) {
	home := t.TempDir()
	store := claude.Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(store.Dir, filepath.Join(home, ".codex-accounts")); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := claudeRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"env"}); err != nil {
		t.Fatal(err)
	}

	want := "export CLAUDE_CONFIG_DIR=" + filepath.Join(home, ".codex-accounts", "claude", "work")
	if got := strings.TrimSpace(out.String()); got != want {
		t.Fatalf("env output = %q, want %q", got, want)
	}
}

func TestClaudeSwitchSupportsPartialProfile(t *testing.T) {
	store := claude.Store{Dir: t.TempDir()}
	if _, err := store.CreateProfile("personal"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := claudeRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"switch", "pers"}); err != nil {
		t.Fatal(err)
	}

	if active := store.ActiveProfile(); active != "personal" {
		t.Fatalf("active = %q, want personal", active)
	}
	if !strings.Contains(out.String(), "Active Claude profile: personal") {
		t.Fatalf("switch output = %q", out.String())
	}
}

func TestClaudeSwitchProtectedPlaintextPrintsWrappedLaunch(t *testing.T) {
	store := claude.Store{Dir: t.TempDir()}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}
	configDir := store.ClaudeConfigDir("work")
	settings := `{"env":{"ANTHROPIC_BASE_URL":"` + managedClaudeBlockedBaseURL + `","ANTHROPIC_AUTH_TOKEN":"` + testTenantKey + `","` + managedClaudeServerURLEnv + `":"http://m3.example","` + managedClaudeTailscaleNodeEnv + `":"node-m3"}}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runner := claudeRunner{store: store, out: &out, errOut: &out}
	if err := runner.switchProfile("work"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "sr claude run work [claude args...]") {
		t.Fatalf("switch output omitted safe launch: %q", got)
	}
	if strings.Contains(got, "export CLAUDE_CONFIG_DIR") || strings.Contains(got, "sr claude env") {
		t.Fatalf("switch output advertised unsafe plain-Claude launch: %q", got)
	}
	out.Reset()
	legacy := `{"env":{"ANTHROPIC_BASE_URL":"http://legacy.example:31415","ANTHROPIC_AUTH_TOKEN":"subrouter"}}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runner.switchProfile("work"); err != nil {
		t.Fatal(err)
	}
	got = out.String()
	if !strings.Contains(got, "sr claude push work") || !strings.Contains(got, "sr claude run work") {
		t.Fatalf("legacy switch output omitted migration guidance: %q", got)
	}
	if strings.Contains(got, "export CLAUDE_CONFIG_DIR") {
		t.Fatalf("legacy switch output advertised unsafe plain-Claude launch: %q", got)
	}
}

func TestClaudeFlagsRunActiveProfile(t *testing.T) {
	home := t.TempDir()
	store := claude.Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}

	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(home, "claude-run.txt")
	claudePath := filepath.Join(binDir, "claude")
	script := "#!/bin/sh\n{ printf 'config=%s\\nargs=%s\\n' \"$CLAUDE_CONFIG_DIR\" \"$*\"; env | grep -E '^(ANTHROPIC_BASE_URL|ANTHROPIC_AUTH_TOKEN|CLAUDE_CODE_OAUTH_TOKEN)=' || true; } > " + shellQuote(recordPath) + "\n"
	if err := os.WriteFile(claudePath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ANTHROPIC_BASE_URL", "http://subrouter-team:31415")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "subrouter")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "direct-token")
	t.Setenv("CLAUDE_CONFIG_DIR", "/old/config")

	var out bytes.Buffer
	runner := claudeRunner{
		store: store, in: strings.NewReader(""), out: &out, errOut: &out,
		authStatus: func(context.Context, string, string) (*claude.AuthStatus, error) {
			return &claude.AuthStatus{LoggedIn: true}, nil
		},
	}
	err := runner.run(context.Background(), []string{"--dangerously-skip-permissions", "--resume", "1721c0ce-b3bd-4d73-8b33-b3d02b677074"})
	if err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, "config="+store.ClaudeConfigDir("work")) {
		t.Fatalf("Claude did not receive active config dir:\n%s", got)
	}
	if !strings.Contains(got, "args=--dangerously-skip-permissions --resume 1721c0ce-b3bd-4d73-8b33-b3d02b677074") {
		t.Fatalf("Claude did not receive flags:\n%s", got)
	}
	for _, needle := range []string{"ANTHROPIC_BASE_URL=", "ANTHROPIC_AUTH_TOKEN=", "CLAUDE_CODE_OAUTH_TOKEN="} {
		if strings.Contains(got, needle) {
			t.Fatalf("Claude inherited %s env:\n%s", needle, got)
		}
	}
}

func TestClaudeRunRejectsLoggedOutLocalProfileBeforeAnyFilesystemMutation(t *testing.T) {
	home := t.TempDir()
	store := claude.Store{
		Dir:            filepath.Join(home, ".subrouter", "codex"),
		SharedStateDir: filepath.Join(home, ".subrouter", "claude-shared"),
	}
	if _, err := store.CreateProfile("ready"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateProfile("mydonorkid"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetActiveProfile("ready"); err != nil {
		t.Fatal(err)
	}

	// Leave one shared-state link absent. ClaudeConfigDir would recreate it,
	// giving the test a concrete write to detect before login acceptance.
	profileDir := store.PreferredInstancePath(store.InstancePath("mydonorkid"))
	missingLink := filepath.Join(profileDir, "projects")
	if err := os.Remove(missingLink); err != nil {
		t.Fatal(err)
	}
	profilesBefore, err := os.ReadFile(store.ProfilesPath())
	if err != nil {
		t.Fatal(err)
	}

	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	launchMarker := filepath.Join(home, "claude-launched")
	script := "#!/bin/sh\ntouch " + shellQuote(launchMarker) + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	authChecked := false
	runner := claudeRunner{
		store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard,
		authStatus: func(_ context.Context, claudePath, configDir string) (*claude.AuthStatus, error) {
			authChecked = true
			if filepath.Base(claudePath) != "claude" {
				t.Fatalf("auth preflight CLI = %q", claudePath)
			}
			if configDir != profileDir {
				t.Fatalf("auth preflight config dir = %q, want %q", configDir, profileDir)
			}
			return &claude.AuthStatus{LoggedIn: false}, nil
		},
	}
	err = runner.runClaude(t.Context(), "mydonorkid", []string{"--resume", "session-a"})
	if err == nil {
		t.Fatal("logged-out local profile launched Claude")
	}
	for _, want := range []string{
		`local managed Claude profile "mydonorkid" is not logged in`,
		"server-pool availability is separate",
		"sr claude proxy --account 'mydonorkid'",
		"sr add claude <new-name>",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("launch error missing %q: %v", want, err)
		}
	}
	if !authChecked {
		t.Fatal("local auth readiness was not checked")
	}
	if _, statErr := os.Lstat(missingLink); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("login rejection recreated shared-state link: %v", statErr)
	}
	profilesAfter, readErr := os.ReadFile(store.ProfilesPath())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(profilesAfter, profilesBefore) {
		t.Fatalf("login rejection mutated profile registry:\nbefore: %s\nafter: %s", profilesBefore, profilesAfter)
	}
	if _, statErr := os.Stat(launchMarker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Claude launched before readiness rejection: %v", statErr)
	}
}

func TestClaudeManagedProfileRejectsLegacyRemoteHTTPTenantBeforeLaunch(t *testing.T) {
	home := t.TempDir()
	store := claude.Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	if _, err := store.CreateProfile("legacy"); err != nil {
		t.Fatal(err)
	}
	configDir := store.ClaudeConfigDir("legacy")
	settings := `{"env":{"ANTHROPIC_BASE_URL":"http://192.168.1.10:31415/t/` + testTenantKey + `","ANTHROPIC_AUTH_TOKEN":"` + testTenantKey + `"}}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(home, "launched")
	script := "#!/bin/sh\ntouch " + shellQuote(marker) + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	runner := claudeRunner{
		store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard,
		authStatus: func(context.Context, string, string) (*claude.AuthStatus, error) {
			return &claude.AuthStatus{LoggedIn: true}, nil
		},
	}
	err := runner.runClaude(t.Context(), "legacy", []string{"--print", "hello"})
	if err == nil || !strings.Contains(err.Error(), "unsafe proxy transport") {
		t.Fatalf("managed Claude launch error = %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Claude launched before transport rejection: %v", statErr)
	}

	apiKeySettings := `{"env":{"ANTHROPIC_BASE_URL":"http://192.168.1.10:31415","ANTHROPIC_API_KEY":"api-secret"}}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(apiKeySettings), 0o600); err != nil {
		t.Fatal(err)
	}
	err = runner.runClaude(t.Context(), "legacy", []string{"--print", "hello"})
	if err == nil || !strings.Contains(err.Error(), "unsafe proxy transport") {
		t.Fatalf("managed Claude API-key launch error = %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Claude launched with an API key over unsafe transport: %v", statErr)
	}

	customHeaderSettings := `{"env":{"ANTHROPIC_BASE_URL":"http://192.168.1.10:31415","ANTHROPIC_AUTH_TOKEN":"subrouter","ANTHROPIC_CUSTOM_HEADERS":"Authorization: Bearer secret"}}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(customHeaderSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	err = runner.runClaude(t.Context(), "legacy", []string{"--print", "hello"})
	if err == nil || !strings.Contains(err.Error(), "unsafe proxy transport") {
		t.Fatalf("managed Claude custom-header launch error = %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Claude launched with a custom header over unsafe transport: %v", statErr)
	}

	tokenlessSettings := `{"env":{"ANTHROPIC_BASE_URL":"http://legacy.example:31415","ANTHROPIC_AUTH_TOKEN":"subrouter"}}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(tokenlessSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	err = runner.runClaude(t.Context(), "legacy", []string{"--resume", "session-a"})
	if err == nil || !strings.Contains(err.Error(), "missing an exact durable identity") {
		t.Fatalf("managed Claude tokenless legacy launch error = %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Claude launched tokenless legacy plaintext before migration: %v", statErr)
	}
}

func TestSecureManagedClaudeTransportDoesNotRewriteCredentialSettings(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	body := []byte(`{"theme":"dark","env":{"ANTHROPIC_BASE_URL":"http://localhost:` + port + `","ANTHROPIC_AUTH_TOKEN":"custom-auth","ANTHROPIC_CUSTOM_HEADERS":"Authorization: Bearer custom-header","FOO":"bar"}}`)
	if err := os.WriteFile(settingsPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	secureBaseURL, err := secureManagedClaudeProfileTransport(dir)
	if err != nil {
		t.Fatal(err)
	}
	if secureBaseURL != "http://127.0.0.1:"+port {
		t.Fatalf("secure base URL = %q", secureBaseURL)
	}
	after, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(body) {
		t.Fatalf("transport validation rewrote managed credentials:\nbefore: %s\nafter: %s", body, after)
	}
}

func TestSecureManagedClaudeTransportNeverRoutesCredentialsAsTenantKeys(t *testing.T) {
	load := func(context.Context) ([]byte, error) {
		return []byte(`{"Self":{"ID":"self","Online":true},"Peer":{"node-m3":{"ID":"node-m3","DNSName":"m3.example.ts.net.","TailscaleIPs":["100.88.0.9"],"Online":true}}}`), nil
	}
	lookup := func(context.Context, string) ([]net.IPAddr, error) {
		t.Fatal("exact node identity must avoid DNS")
		return nil, nil
	}
	for _, tc := range []struct {
		name string
		env  string
	}{
		{name: "tokenless", env: `"ANTHROPIC_AUTH_TOKEN":"subrouter"`},
		{name: "custom auth", env: `"ANTHROPIC_AUTH_TOKEN":"custom-secret"`},
		{name: "api key", env: `"ANTHROPIC_API_KEY":"api-secret"`},
		{name: "custom header", env: `"ANTHROPIC_AUTH_TOKEN":"subrouter","ANTHROPIC_CUSTOM_HEADERS":"Authorization: Bearer header-secret"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			settings := `{"env":{"ANTHROPIC_BASE_URL":"` + managedClaudeBlockedBaseURL + `",` + tc.env + `,"` + managedClaudeServerURLEnv + `":"http://m3.example.ts.net.:31415","` + managedClaudeTailscaleNodeEnv + `":"node-m3"}}`
			if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settings), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := secureManagedClaudeProfileTransportWithResolvers(dir, lookup, load)
			if err != nil {
				t.Fatal(err)
			}
			if got != "http://100.88.0.9:31415" {
				t.Fatalf("secured base URL = %q, credential became a tenant path", got)
			}
			for _, secret := range []string{"custom-secret", "api-secret", "header-secret", "protected-managed-credential"} {
				if strings.Contains(got, secret) {
					t.Fatalf("secured base URL leaked %q: %q", secret, got)
				}
			}
		})
	}
}

func TestRunClaudeUsesAuthoritativeSettingsOverrideAndPreservesResumeArgs(t *testing.T) {
	home := t.TempDir()
	tempRoot := filepath.Join(home, "tmp")
	if err := os.MkdirAll(tempRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tempRoot)
	store := claude.Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}
	configDir := store.ClaudeConfigDir("work")
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	settings := `{"theme":"dark","env":{"ANTHROPIC_BASE_URL":"http://localhost:` + port + `","ANTHROPIC_AUTH_TOKEN":"secret"}}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(home, "args")
	overridePath := filepath.Join(home, "override")
	envPath := filepath.Join(home, "env")
	attackerSettingsPath := filepath.Join(home, "attacker-settings.json")
	if err := os.WriteFile(attackerSettingsPath, []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://attacker.invalid"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellQuote(argsPath) + "\nenv | grep -E '^(ANTHROPIC_BASE_URL|ANTHROPIC_AUTH_TOKEN|ANTHROPIC_CUSTOM_HEADERS|ANTHROPIC_API_KEY|CLAUDE_CODE_OAUTH_TOKEN|CLAUDE_CODE_API_KEY|CLAUDE_CODE_AUTH_TOKEN|CLAUDE_CODE_BASE_URL)=' > " + shellQuote(envPath) + " || true\nprev=''\nfor arg in \"$@\"; do\n  if [ \"$prev\" = '--settings' ]; then settings=$arg; break; fi\n  prev=$arg\ndone\ncat \"$settings\" > " + shellQuote(overridePath) + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	runner := claudeRunner{
		store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard,
		authStatus: func(context.Context, string, string) (*claude.AuthStatus, error) {
			return &claude.AuthStatus{LoggedIn: true}, nil
		},
	}
	if err := runner.runClaude(t.Context(), "work", []string{"--managed-settings", attackerSettingsPath, "--resume", "session-a"}); err != nil {
		t.Fatal(err)
	}
	argsBody, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(argsBody)), "\n")
	if len(args) != 4 || args[0] != "--settings" || args[2] != "--resume" || args[3] != "session-a" {
		t.Fatalf("Claude args = %#v", args)
	}
	if strings.Contains(string(argsBody), "secret") {
		t.Fatalf("credential leaked through Claude argv: %q", argsBody)
	}
	envBody, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(envBody) != 0 || bytes.Contains(envBody, []byte("secret")) {
		t.Fatalf("credential-bearing routing environment survived managed launch: %q", envBody)
	}
	if strings.Contains(string(argsBody), attackerSettingsPath) || strings.Contains(string(argsBody), "attacker.invalid") {
		t.Fatalf("attacker managed settings survived Claude argv sanitization: %q", argsBody)
	}
	if strings.Contains(args[1], "secret") || !strings.HasSuffix(args[1], filepath.Join("", "settings.json")) {
		t.Fatalf("verified settings argument is not an opaque scoped path: %q", args[1])
	}
	var override struct {
		Env map[string]string `json:"env"`
	}
	overrideBody, err := os.ReadFile(overridePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(overrideBody, &override); err != nil {
		t.Fatalf("settings override = %q: %v", overrideBody, err)
	}
	// Claude does not fall through an empty override to the profile settings,
	// so the private override must restate the profile's own credential.
	if got := override.Env["ANTHROPIC_AUTH_TOKEN"]; got != "secret" {
		t.Fatalf("settings override dropped the profile credential: %+v", override)
	}
	if got := override.Env["ANTHROPIC_BASE_URL"]; got != "http://127.0.0.1:"+port {
		t.Fatalf("settings override base URL = %q", got)
	}
	leftovers, err := filepath.Glob(filepath.Join(tempRoot, claudeSettingsDirPrefix+"*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("verified settings left named artifacts: %v", leftovers)
	}
}

func TestManagedClaudeLaunchArgsMakesVerifiedSettingsFinal(t *testing.T) {
	got, err := managedClaudeLaunchArgs([]string{
		"--settings", "user-a.json",
		"--managed-settings", "policy-a.json",
		"--resume", "session-a",
		"--settings=user-b.json",
		"--managed-settings=policy-b.json",
		"--", "--settings", "literal-prompt-text",
	}, "/tmp/verified.json")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--settings", "/tmp/verified.json",
		"--resume", "session-a",
		"--", "--settings", "literal-prompt-text",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("launch args = %#v, want %#v", got, want)
	}
	if _, err := managedClaudeLaunchArgs([]string{"--settings"}, "/tmp/verified.json"); err == nil {
		t.Fatal("missing user --settings value was silently accepted")
	}
	if _, err := managedClaudeLaunchArgs([]string{"--settings", "--resume", "session-a"}, "/tmp/verified.json"); err == nil {
		t.Fatal("option-looking --settings value consumed --resume")
	}
	if _, err := managedClaudeLaunchArgs([]string{"--settings="}, "/tmp/verified.json"); err == nil {
		t.Fatal("empty --settings=value was silently accepted")
	}
	if _, err := managedClaudeLaunchArgs([]string{"--managed-settings"}, "/tmp/verified.json"); err == nil {
		t.Fatal("missing --managed-settings value was silently accepted")
	}
	if _, err := managedClaudeLaunchArgs([]string{"--managed-settings", "--resume", "session-a"}, "/tmp/verified.json"); err == nil {
		t.Fatal("option-looking --managed-settings value consumed --resume")
	}
	if _, err := managedClaudeLaunchArgs([]string{"--managed-settings="}, "/tmp/verified.json"); err == nil {
		t.Fatal("empty --managed-settings=value was silently accepted")
	}
	subcommand, err := managedClaudeLaunchArgs([]string{"mcp", "list"}, "/tmp/verified.json")
	if err != nil || !slices.Equal(subcommand, []string{"--settings", "/tmp/verified.json", "mcp", "list"}) {
		t.Fatalf("subcommand launch args = %#v, %v", subcommand, err)
	}
}

func TestProxyClaudeLaunchSettingsNeutralizeHostilePersistedRouting(t *testing.T) {
	hostile := make(map[string]string, len(claudeRoutingEnvKeys))
	for _, key := range claudeRoutingEnvKeys {
		hostile[key] = "hostile-" + strings.ToLower(key)
	}
	hostile["CLAUDE_SECURESTORAGE_CONFIG_DIR"] = "hostile-securestorage"
	persistedBody, err := json.Marshal(map[string]any{
		"theme": "dark",
		"env":   hostile,
	})
	if err != nil {
		t.Fatal(err)
	}
	configDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), persistedBody, 0o600); err != nil {
		t.Fatal(err)
	}

	overrideBody, err := proxyClaudeLaunchSettings("https://subrouter.example/v1", "route-token", configDir)
	if err != nil {
		t.Fatal(err)
	}
	var persisted, override struct {
		Env map[string]string `json:"env"`
	}
	body, err := os.ReadFile(filepath.Join(configDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &persisted); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(overrideBody, &override); err != nil {
		t.Fatal(err)
	}
	effective := maps.Clone(persisted.Env)
	for key, value := range override.Env {
		effective[key] = value
	}

	want := map[string]string{
		"ANTHROPIC_BASE_URL":       "https://subrouter.example",
		"ANTHROPIC_AUTH_TOKEN":     "route-token",
		"ANTHROPIC_CUSTOM_HEADERS": "X-Subrouter-Agent: claude",
		"CLAUDE_CONFIG_DIR":        configDir,
		"CLAUDE_CODE_CONFIG_DIR":   configDir,
		// Not empty: Claude Code reads an empty one as the bare
		// `Claude Code-credentials` item, the user's own login (PATCHES §73).
		"CLAUDE_SECURESTORAGE_CONFIG_DIR": configDir,
	}
	for key := range hostile {
		if got := effective[key]; got != want[key] {
			t.Fatalf("effective routing setting %s = %q, want %q", key, got, want[key])
		}
		if strings.Contains(effective[key], "hostile-") {
			t.Fatalf("hostile persisted routing survived for %s", key)
		}
	}
}

func TestClaudeEnvRejectsProtectedPlaintextManagedServer(t *testing.T) {
	store := claude.Store{Dir: t.TempDir()}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetActiveProfile("work"); err != nil {
		t.Fatal(err)
	}
	configDir := store.ClaudeConfigDir("work")
	settings := `{"env":{"ANTHROPIC_BASE_URL":"` + managedClaudeBlockedBaseURL + `","ANTHROPIC_AUTH_TOKEN":"` + testTenantKey + `","` + managedClaudeServerURLEnv + `":"http://m3.example","` + managedClaudeTailscaleNodeEnv + `":"node-m3"}}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runner := claudeRunner{store: store, out: &out, errOut: &out}
	err := runner.env("")
	if err == nil || !strings.Contains(err.Error(), "sr claude run work") {
		t.Fatalf("env error = %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("unsafe shell exports were printed: %q", out.String())
	}
	legacy := `{"env":{"ANTHROPIC_BASE_URL":"http://legacy.example:31415","ANTHROPIC_AUTH_TOKEN":"subrouter"}}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	err = runner.env("")
	if err == nil || !strings.Contains(err.Error(), "sr claude push work") || !strings.Contains(err.Error(), "sr claude run work") {
		t.Fatalf("legacy env error = %v", err)
	}
	// Named, the same refusal: the name picks the profile, not the checks.
	err = runner.env("work")
	if err == nil || !strings.Contains(err.Error(), "sr claude push work") || !strings.Contains(err.Error(), "sr claude run work") {
		t.Fatalf("named legacy env error = %v", err)
	}
}

func TestProxyClaudeRunKeepsNamedProfileSemantics(t *testing.T) {
	store := claude.Store{Dir: t.TempDir()}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}

	configDir, args, err := proxyClaudeInvocation(
		store,
		[]string{"run", "work", "--resume", "session-a"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if configDir != store.ClaudeConfigDir("work") {
		t.Fatalf("config dir = %q, want work profile", configDir)
	}
	if got := strings.Join(args, " "); got != "--resume session-a" {
		t.Fatalf("Claude args = %q", got)
	}
}

func TestProxyClaudeProfileShorthandKeepsNamedProfileSemantics(t *testing.T) {
	store := claude.Store{Dir: t.TempDir()}
	if _, err := store.CreateProfile("personal"); err != nil {
		t.Fatal(err)
	}

	configDir, args, err := proxyClaudeInvocation(
		store,
		[]string{"personal", "--verbose"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if configDir != store.ClaudeConfigDir("personal") {
		t.Fatalf("config dir = %q, want personal profile", configDir)
	}
	if got := strings.Join(args, " "); got != "--verbose" {
		t.Fatalf("Claude args = %q", got)
	}
}

func TestProxyClaudeAllowsProfilelessFlagAndRunInvocations(t *testing.T) {
	store := claude.Store{Dir: t.TempDir()}
	for _, input := range [][]string{
		{"--print", "hello"},
		{"run", "--print", "hello"},
	} {
		configDir, args, err := proxyClaudeInvocation(store, input)
		if err != nil {
			t.Fatalf("proxyClaudeInvocation(%v): %v", input, err)
		}
		if configDir != "" {
			t.Fatalf("config dir for %v = %q, want default", input, configDir)
		}
		want := input
		if input[0] == "run" {
			want = input[1:]
		}
		if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("args for %v = %v, want %v", input, args, want)
		}
	}
}

func TestSRClaudeProxyUsesSelectedRemoteWithoutLocalProfile(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tenantKey string
		wantBase  string
		wantToken string
	}{
		{name: "trusted legacy", wantBase: "https://m3.example", wantToken: "subrouter"},
		{name: "tenant", tenantKey: "srt_team", wantBase: "https://m3.example/t/srt_team", wantToken: "srt_team"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			store := accounts.CodexStore{Dir: filepath.Join(home, "state", "codex", "accounts")}
			serverStore := defaultSRServerStore(store)
			if err := serverStore.save(srServerFile{
				Default: "m3-pilot",
				Servers: []srServerConfig{{
					Name:      "m3-pilot",
					URL:       "https://m3.example/v1",
					TenantKey: tc.tenantKey,
				}},
			}); err != nil {
				t.Fatal(err)
			}

			binDir := filepath.Join(home, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			recordPath := filepath.Join(home, "claude-proxy.txt")
			settingsCopyPath := filepath.Join(home, "claude-proxy-settings.json")
			claudePath := filepath.Join(binDir, "claude")
			script := "#!/bin/sh\n{ printf 'config=%s\\nargs=%s\\n' \"$CLAUDE_CONFIG_DIR\" \"$*\"; env | grep -E '^(ANTHROPIC_BASE_URL|ANTHROPIC_AUTH_TOKEN|ANTHROPIC_CUSTOM_HEADERS|CLAUDE_CODE_OAUTH_TOKEN|CLAUDE_CODE_API_KEY|CLAUDE_CODE_AUTH_TOKEN|CLAUDE_CODE_BASE_URL|ANTHROPIC_API_KEY)=' || true; } > " + shellQuote(recordPath) + "\ncat \"$2\" > " + shellQuote(settingsCopyPath) + "\n"
			if err := os.WriteFile(claudePath, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CLAUDE_CONFIG_DIR", "/must-not-leak")
			t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "X-Subrouter-Agent: stale")
			t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "personal-oauth-must-not-leak")
			t.Setenv("CLAUDE_CODE_API_KEY", "personal-api-must-not-leak")
			t.Setenv("CLAUDE_CODE_AUTH_TOKEN", "personal-auth-must-not-leak")
			t.Setenv("CLAUDE_CODE_BASE_URL", "https://personal-route.invalid")
			t.Setenv("ANTHROPIC_API_KEY", "personal-anthropic-key-must-not-leak")
			pinClaudeProxyToSelectedServer(t)

			runner := srRunner{program: "sr", store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
			if err := runner.claude(context.Background(), []string{"proxy", "--resume", "session-a", "--model", "opus"}); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(recordPath)
			if err != nil {
				t.Fatal(err)
			}
			got := string(body)
			for _, want := range []string{
				"args=--settings ",
				" --resume session-a --model opus\n",
			} {
				if !strings.Contains(got, want) {
					t.Fatalf("proxy invocation missing %q:\n%s", want, got)
				}
			}
			for _, forbidden := range []string{
				"ANTHROPIC_BASE_URL=", "ANTHROPIC_AUTH_TOKEN=", "ANTHROPIC_CUSTOM_HEADERS=",
				"CLAUDE_CODE_OAUTH_TOKEN=", "CLAUDE_CODE_API_KEY=",
				"CLAUDE_CODE_AUTH_TOKEN=", "CLAUDE_CODE_BASE_URL=",
				"ANTHROPIC_API_KEY=", "personal-",
			} {
				if strings.Contains(got, forbidden) {
					t.Fatalf("proxy invocation inherited %q:\n%s", forbidden, got)
				}
			}
			if strings.Contains(got, tc.wantBase) || (tc.tenantKey != "" && strings.Contains(got, tc.tenantKey)) {
				t.Fatalf("proxy invocation exposed routing URL or tenant key through argv/env:\n%s", got)
			}
			settingsBody, err := os.ReadFile(settingsCopyPath)
			if err != nil {
				t.Fatal(err)
			}
			var overlay struct {
				Env map[string]string `json:"env"`
			}
			if err := json.Unmarshal(settingsBody, &overlay); err != nil {
				t.Fatal(err)
			}
			if overlay.Env["ANTHROPIC_BASE_URL"] != tc.wantBase ||
				overlay.Env["ANTHROPIC_AUTH_TOKEN"] != tc.wantToken ||
				overlay.Env["ANTHROPIC_CUSTOM_HEADERS"] != "X-Subrouter-Agent: claude" {
				t.Fatalf("proxy authoritative settings = %+v", overlay.Env)
			}
			configLine := strings.SplitN(got, "\n", 2)[0]
			configDir := strings.TrimPrefix(configLine, "config=")
			if configDir == "" || configDir == "/must-not-leak" || filepath.Dir(configDir) != filepath.Join(store.StoreDir(), "claude-proxy") {
				t.Fatalf("proxy did not use an isolated config directory: %q", configLine)
			}
			if tc.tenantKey != "" && strings.Contains(configDir, tc.tenantKey) {
				t.Fatalf("proxy config path exposed tenant credential: %q", configDir)
			}
			if info, err := os.Stat(configDir); err != nil || !info.IsDir() {
				t.Fatalf("durable proxy config is unavailable after launch: info=%v err=%v", info, err)
			}
			sessionMarker := filepath.Join(configDir, "session-marker")
			if err := os.WriteFile(sessionMarker, []byte("resume-me"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := runner.claude(context.Background(), []string{"proxy", "--resume", "session-a"}); err != nil {
				t.Fatal(err)
			}
			if marker, err := os.ReadFile(sessionMarker); err != nil || string(marker) != "resume-me" {
				t.Fatalf("proxy launch did not preserve resumable session state: marker=%q err=%v", marker, err)
			}
			secondBody, err := os.ReadFile(recordPath)
			if err != nil {
				t.Fatal(err)
			}
			if secondConfig := strings.TrimPrefix(strings.SplitN(string(secondBody), "\n", 2)[0], "config="); secondConfig != configDir {
				t.Fatalf("proxy config changed across resume: first=%q second=%q", configDir, secondConfig)
			}
		})
	}
}

// pinClaudeProxyToSelectedServer makes `sr claude proxy` take the selected
// server, for tests about remote launch mechanics. Upstream sent every
// profileless launch to the selected remote; this fork routes it local-first
// (resolveClaudeProxyRoute), with SUBROUTER_DISABLE_FALLBACK as the documented
// pin to the configured server, and a remote exists only on a machine holding
// the key (PATCHES 17). Call it after the test has set HOME, if it does.
func pinClaudeProxyToSelectedServer(t *testing.T) {
	t.Helper()
	unlockRemoteGateForTest(t)
	t.Setenv("SUBROUTER_DISABLE_FALLBACK", "1")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", filepath.Join(t.TempDir(), "missing-cloud.json"))
}

func TestResolveClaudeProxyAccountSelectorFailsClosed(t *testing.T) {
	inventory := []remoteServerAccount{
		{ID: "claude-profile-1", Label: "account-one", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
		{ID: "claude-profile-2", Label: "account-two", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
		{ID: "claude-profile-1", Label: "same-id-codex", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
		{ID: "codex-profile", Label: "codex-only", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
		{ID: "claude-api", Label: "metered", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey},
	}
	for selector, want := range map[string]string{
		"claude-profile-1": "claude-profile-1",
		"ACCOUNT-TWO":      "claude-profile-2",
		"profile-2":        "claude-profile-2",
	} {
		got, err := resolveClaudeProxyAccountSelector(inventory, selector)
		if err != nil || got != want {
			t.Fatalf("resolve %q = %q, %v; want %q", selector, got, err, want)
		}
	}
	for _, tc := range []struct {
		selector string
		wantErr  string
	}{
		{selector: "", wantErr: "cannot be empty"},
		{selector: "missing", wantErr: "was not found"},
		{selector: "profile", wantErr: "ambiguous"},
		{selector: "codex-only", wantErr: "not Claude"},
		{selector: "metered", wantErr: "subscription profile"},
	} {
		if _, err := resolveClaudeProxyAccountSelector(inventory, tc.selector); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Fatalf("resolve %q error = %v, want %q", tc.selector, err, tc.wantErr)
		}
	}
	injected := slices.Clone(inventory)
	injected[0].ID = "good\r\nX-Injected: yes"
	if _, err := resolveClaudeProxyAccountSelector(injected, "account-one"); err == nil || !strings.Contains(err.Error(), "invalid server routing ID") {
		t.Fatalf("header-injecting account ID error = %v", err)
	}
}

func TestSRClaudeProxyPinnedAccountsLaunchInParallelWithoutSharedMutation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	recordDir := filepath.Join(home, "records")
	if err := os.MkdirAll(recordDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var requestMu sync.Mutex
	accountRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/_subrouter/accounts" {
			http.NotFound(w, req)
			return
		}
		requestMu.Lock()
		accountRequests++
		requestMu.Unlock()
		_ = json.NewEncoder(w).Encode([]remoteServerAccount{
			{ID: "claude-profile-1", Label: "account-one", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
			{ID: "claude-profile-2", Label: "account-two", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
		})
	}))
	defer server.Close()

	store := accounts.CodexStore{Dir: filepath.Join(home, "state", "codex", "accounts")}
	serverStore := defaultSRServerStore(store)
	if err := serverStore.save(srServerFile{Default: "team", Servers: []srServerConfig{{Name: "team", URL: server.URL}}}); err != nil {
		t.Fatal(err)
	}
	serverBefore, err := os.ReadFile(serverStore.Path)
	if err != nil {
		t.Fatal(err)
	}
	localProfiles := claude.DefaultStore()
	if _, err := localProfiles.CreateProfile("local-active"); err != nil {
		t.Fatal(err)
	}
	if err := localProfiles.SetActiveProfile("local-active"); err != nil {
		t.Fatal(err)
	}
	profilesBefore, err := os.ReadFile(localProfiles.ProfilesPath())
	if err != nil {
		t.Fatal(err)
	}

	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
settings=
marker=
previous=
for arg in "$@"; do
  if [ "$previous" = "--settings" ]; then settings=$arg; fi
  if [ "$previous" = "--marker" ]; then marker=$arg; fi
  previous=$arg
done
printf 'args=%s\n' "$*" > "$RECORD_DIR/$marker.process"
env >> "$RECORD_DIR/$marker.process"
cp "$settings" "$RECORD_DIR/$marker.settings.json"
printf '%s\n' "$CLAUDE_CONFIG_DIR" > "$RECORD_DIR/$marker.config"
`
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RECORD_DIR", recordDir)
	pinClaudeProxyToSelectedServer(t)

	type launch struct{ selector, marker string }
	launches := []launch{{selector: "account-one", marker: "one"}, {selector: "account-two", marker: "two"}}
	errCh := make(chan error, len(launches))
	start := make(chan struct{})
	for _, item := range launches {
		item := item
		go func() {
			<-start
			runner := srRunner{program: "sr", store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
			errCh <- runner.claude(t.Context(), []string{"proxy", "--account", item.selector, "--marker", item.marker})
		}()
	}
	close(start)
	for range launches {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}

	requestMu.Lock()
	gotRequests := accountRequests
	requestMu.Unlock()
	if gotRequests != len(launches) {
		t.Fatalf("account inventory requests = %d, want %d independent validations", gotRequests, len(launches))
	}
	configDirs := make(map[string]bool)
	for index, item := range launches {
		settingsBody, err := os.ReadFile(filepath.Join(recordDir, item.marker+".settings.json"))
		if err != nil {
			t.Fatal(err)
		}
		var overlay struct {
			Env map[string]string `json:"env"`
		}
		if err := json.Unmarshal(settingsBody, &overlay); err != nil {
			t.Fatal(err)
		}
		wantID := fmt.Sprintf("claude-profile-%d", index+1)
		wantHeaders := "X-Subrouter-Agent: claude\nX-Subrouter-Account-ID: " + wantID
		if got := overlay.Env["ANTHROPIC_CUSTOM_HEADERS"]; got != wantHeaders {
			t.Fatalf("%s forced headers = %q, want %q", item.marker, got, wantHeaders)
		}
		processBody, err := os.ReadFile(filepath.Join(recordDir, item.marker+".process"))
		if err != nil {
			t.Fatal(err)
		}
		for _, accountID := range []string{"claude-profile-1", "claude-profile-2"} {
			if bytes.Contains(processBody, []byte(accountID)) {
				t.Fatalf("%s exposed account routing ID through argv/env:\n%s", item.marker, processBody)
			}
		}
		configBody, err := os.ReadFile(filepath.Join(recordDir, item.marker+".config"))
		if err != nil {
			t.Fatal(err)
		}
		configDir := strings.TrimSpace(string(configBody))
		if configDir == "" || strings.Contains(configDir, wantID) {
			t.Fatalf("%s config dir is not opaque: %q", item.marker, configDir)
		}
		configDirs[configDir] = true
	}
	if len(configDirs) != len(launches) {
		t.Fatalf("pinned launches shared config directories: %v", configDirs)
	}
	serverAfter, err := os.ReadFile(serverStore.Path)
	if err != nil {
		t.Fatal(err)
	}
	profilesAfter, err := os.ReadFile(localProfiles.ProfilesPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(serverBefore, serverAfter) {
		t.Fatal("parallel pinned launches mutated selected-server state")
	}
	if !bytes.Equal(profilesBefore, profilesAfter) || localProfiles.ActiveProfile() != "local-active" {
		t.Fatal("parallel pinned launches mutated the active local Claude profile")
	}
}

func TestSRClaudeProxyPinnedAccountValidationFailsBeforeLaunch(t *testing.T) {
	home := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = json.NewEncoder(w).Encode([]remoteServerAccount{
			{ID: "claude-profile-1", Label: "account-one", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
		})
	}))
	defer server.Close()
	store := accounts.CodexStore{Dir: filepath.Join(home, "state", "codex", "accounts")}
	if err := defaultSRServerStore(store).save(srServerFile{Default: "team", Servers: []srServerConfig{{Name: "team", URL: server.URL}}}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(home, "launched")
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte("#!/bin/sh\ntouch "+shellQuote(marker)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	pinClaudeProxyToSelectedServer(t)
	runner := srRunner{program: "sr", store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	err := runner.claude(t.Context(), []string{"proxy", "--account", "missing", "-p", "hello"})
	if err == nil || !strings.Contains(err.Error(), "was not found") {
		t.Fatalf("missing pinned account error = %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Claude launched before pinned-account validation: %v", statErr)
	}
}

func TestProfilelessClaudePlaintextServerPinsExactNodeAtLaunch(t *testing.T) {
	home := t.TempDir()
	tempRoot := filepath.Join(home, "tmp")
	if err := os.MkdirAll(tempRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tempRoot)
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(home, "claude.txt")
	settingsCopyPath := filepath.Join(home, "launch-settings.json")
	script := "#!/bin/sh\n{ printf 'args=%s\\nconfig=%s\\n' \"$*\" \"$CLAUDE_CONFIG_DIR\"; env | grep -E '^(ANTHROPIC_BASE_URL|ANTHROPIC_AUTH_TOKEN|ANTHROPIC_CUSTOM_HEADERS)=' || true; } > " + shellQuote(recordPath) + "\ncat \"$2\" > " + shellQuote(settingsCopyPath) + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ANTHROPIC_BASE_URL", "http://attacker.invalid")
	t.Setenv("CLAUDE_CONFIG_DIR", "/personal/config")
	configDir := filepath.Join(home, "isolated")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://stale.invalid","ANTHROPIC_AUTH_TOKEN":"stale-secret","ANTHROPIC_CUSTOM_HEADERS":"Authorization: Bearer stale-secret"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	server := srServerConfig{
		Name:            "m3",
		URL:             "http://renamed.example.ts.net.:31415",
		TailscaleNodeID: "node-m3",
	}
	lookup := func(context.Context, string) ([]net.IPAddr, error) {
		t.Fatal("exact-node profileless launch performed a second DNS lookup")
		return nil, nil
	}
	load := func(context.Context) ([]byte, error) {
		return []byte(`{"Self":{"ID":"self","Online":true},"Peer":{"node-m3":{"ID":"node-m3","DNSName":"renamed.example.ts.net.","TailscaleIPs":["100.88.0.9"],"Online":true}}}`), nil
	}
	runner := srRunner{store: accounts.CodexStore{Dir: filepath.Join(home, "accounts")}, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	if err := runner.runProxyClaudeForServerWithResolvers(
		t.Context(), []string{"--resume", "session-a"}, server, "subrouter", configDir, "", "", lookup, load,
	); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	for _, want := range []string{
		"args=--settings ",
		" --resume session-a\n",
		"config=" + configDir + "\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("profileless invocation missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{
		"ANTHROPIC_BASE_URL=", "ANTHROPIC_AUTH_TOKEN=", "ANTHROPIC_CUSTOM_HEADERS=",
		"100.88.0.9", "attacker.invalid", "stale.invalid", "stale-secret", "node-m3",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("profileless invocation leaked %q:\n%s", forbidden, got)
		}
	}
	argsLine := strings.TrimPrefix(strings.SplitN(got, "\n", 2)[0], "args=")
	parts := strings.Fields(argsLine)
	if len(parts) < 2 || parts[0] != "--settings" {
		t.Fatalf("profileless args = %q", argsLine)
	}
	if strings.Contains(parts[1], "100.88.0.9") || !strings.HasSuffix(parts[1], filepath.Join("", "settings.json")) {
		t.Fatalf("verified profileless settings argument is not an opaque scoped path: %q", parts[1])
	}
	leftovers, err := filepath.Glob(filepath.Join(tempRoot, claudeSettingsDirPrefix+"*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("verified profileless settings left named artifacts: %v", leftovers)
	}
	settingsCopy, err := os.ReadFile(settingsCopyPath)
	if err != nil {
		t.Fatal(err)
	}
	var overlay struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(settingsCopy, &overlay); err != nil {
		t.Fatal(err)
	}
	if overlay.Env["ANTHROPIC_BASE_URL"] != "http://100.88.0.9:31415" ||
		overlay.Env["ANTHROPIC_AUTH_TOKEN"] != "subrouter" ||
		overlay.Env["ANTHROPIC_CUSTOM_HEADERS"] != "X-Subrouter-Agent: claude" {
		t.Fatalf("profileless authoritative settings = %+v", overlay.Env)
	}
}

func TestPrivateClaudeLaunchSettingsLeaveNothingAfterKilledProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a Unix shell; Windows coverage is platform-specific")
	}
	tempRoot := t.TempDir()
	t.Setenv("TMPDIR", tempRoot)
	copyPath := filepath.Join(tempRoot, "observed-settings.json")
	readyPath := filepath.Join(tempRoot, "ready")
	secret := "srt_must_not_survive_kill"
	body, err := proxyClaudeLaunchSettings("https://proxy.example", secret, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh")
	settingsArg, cleanup, err := attachClaudeLaunchSettings(cmd, body)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	cmd.Args = []string{"sh", "-c", `cat "$1" > "$2" && : > "$3" && exec sleep 60`, "sh", settingsArg, copyPath, readyPath}
	cmd.Env = claudeSettingsChildEnvironment([]string{
		"PATH=" + os.Getenv("PATH"),
		"ANTHROPIC_BASE_URL=https://proxy.example",
		"ANTHROPIC_AUTH_TOKEN=" + secret,
		"ANTHROPIC_CUSTOM_HEADERS=Authorization: Bearer " + secret,
	}, "https://proxy.example", filepath.Join(tempRoot, "config"))
	if strings.Contains(strings.Join(cmd.Args, "\x00"), secret) {
		t.Fatal("tenant key was exposed in the child argument vector")
	}
	if strings.Contains(strings.Join(cmd.Env, "\x00"), secret) || strings.Contains(strings.Join(cmd.Env, "\x00"), "proxy.example") {
		t.Fatal("tenant routing secret was exposed in the child environment")
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal("child did not read private settings")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel() // exec.CommandContext uses Process.Kill, matching an ungraceful exit.
	if err := cmd.Wait(); err == nil {
		t.Fatal("killed settings consumer exited successfully")
	}
	cleanup()
	observed, err := os.ReadFile(copyPath)
	if err != nil || !bytes.Contains(observed, []byte(secret)) {
		t.Fatalf("child did not receive private settings: body=%q err=%v", observed, err)
	}
	leftovers, err := filepath.Glob(filepath.Join(tempRoot, claudeSettingsDirPrefix+"*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("killed child left named settings artifacts: %v", leftovers)
	}
}

func TestProfilelessClaudePlaintextServerFailsClosedOnNodeStatus(t *testing.T) {
	server := srServerConfig{
		Name:            "m3",
		URL:             "http://m3.example.ts.net.:31415",
		TailscaleNodeID: "node-m3",
	}
	for _, tc := range []struct {
		name   string
		status string
	}{
		{name: "offline", status: `{"Self":{"ID":"self","Online":false}}`},
		{name: "wrong node", status: `{"Self":{"ID":"self","Online":true},"Peer":{"node-m3":{"ID":"node-m3","DNSName":"other.example.ts.net.","TailscaleIPs":["100.88.0.9"],"Online":true}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			marker := filepath.Join(home, "launched")
			binDir := filepath.Join(home, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte("#!/bin/sh\ntouch "+shellQuote(marker)+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			runner := srRunner{store: accounts.CodexStore{Dir: filepath.Join(home, "accounts")}, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
			err := runner.runProxyClaudeForServerWithResolvers(
				t.Context(), []string{"--resume", "session-a"}, server, "subrouter", filepath.Join(home, "isolated"), "", "",
				func(context.Context, string) ([]net.IPAddr, error) {
					t.Fatal("failed exact-node launch fell back to DNS")
					return nil, nil
				},
				func(context.Context) ([]byte, error) { return []byte(tc.status), nil },
			)
			if err == nil {
				t.Fatal("unsafe profileless launch succeeded")
			}
			if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("Claude launched before exact-node verification: %v", statErr)
			}
		})
	}
}

func TestTenantProxyTransportRequiresHTTPSOffLoopback(t *testing.T) {
	// Keep this transport-policy test independent of whatever else is listening
	// on the conventional local Subrouter port. DNS and liveness selection for
	// loopback hostnames are covered by TestTenantScopedHTTPAllowsAndPinsSafeAddresses.
	localhostLookup := func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	for _, tc := range []struct {
		name      string
		baseURL   string
		tenantKey string
		wantErr   bool
	}{
		{name: "remote HTTPS", baseURL: "https://router.example/t/srt_team", tenantKey: "srt_team"},
		{name: "loopback IPv4 HTTP", baseURL: "http://127.0.0.1:31415/t/srt_team", tenantKey: "srt_team"},
		{name: "loopback localhost HTTP", baseURL: "http://localhost:31415/t/srt_team", tenantKey: "srt_team"},
		{name: "remote HTTP", baseURL: "http://router.example/t/srt_team", tenantKey: "srt_team", wantErr: true},
		{name: "remote HTTP without secret", baseURL: "http://router.example", tenantKey: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.name == "loopback localhost HTTP" {
				_, err = secureTenantServerURLWithResolvers(
					t.Context(), tc.baseURL,
					srServerConfig{TenantKey: tc.tenantKey}, localhostLookup, nil,
				)
			} else {
				_, err = secureTenantProxyURL(t.Context(), tc.baseURL, tc.tenantKey)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateTenantProxyTransport() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestSRClaudeProxyRejectsPreviouslyStoredRemoteHTTPTenant(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, "state", "codex", "accounts")}
	if err := defaultSRServerStore(store).save(srServerFile{
		Default: "insecure",
		Servers: []srServerConfig{{
			Name: "insecure", URL: "http://router.example:31415", TenantKey: testTenantKey,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	pinClaudeProxyToSelectedServer(t)
	runner := srRunner{program: "sr", store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	err := runner.claude(context.Background(), []string{"proxy", "--print", "hello"})
	if err == nil || !strings.Contains(err.Error(), "must use HTTPS") {
		t.Fatalf("Claude proxy error = %v, want HTTPS requirement", err)
	}
}

func TestSRClaudeProxyUsesHealthySelectedLocalRoute(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, "state", "codex", "accounts")}
	initializeLocalDataTestStore(t, store)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/_subrouter/health" || request.URL.Path == proxy.StoreHandshakePath {
			writeLocalStoreAuthorityHealth(t, w, request, store, "enabled")
			return
		}
		if request.Method == http.MethodPost && request.URL.Path == "/v1/messages" {
			_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"ok"}]}`)
			return
		}
		http.NotFound(w, request)
	}))
	defer local.Close()
	attachPrivateLocalTestListener(t, local)
	t.Setenv("HOME", home)
	t.Setenv("SUBROUTER_CLOUD_CONFIG", filepath.Join(home, "missing-cloud.json"))
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")

	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(home, "claude-local.txt")
	settingsCopyPath := filepath.Join(home, "claude-local-settings.json")
	claudePath := filepath.Join(binDir, "claude")
	script := "#!/bin/sh\n{ printf 'args=%s\\n' \"$*\"; env | grep -E '^(ANTHROPIC_BASE_URL|ANTHROPIC_AUTH_TOKEN|ANTHROPIC_CUSTOM_HEADERS)=' || true; } > " + shellQuote(recordPath) + "\ncat \"$2\" > " + shellQuote(settingsCopyPath) + "\n"
	if err := os.WriteFile(claudePath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	runner := srRunner{
		program: "sr",
		store:   store,
		in:      strings.NewReader(""),
		out:     io.Discard,
		errOut:  io.Discard,
	}
	if err := runner.claude(context.Background(), []string{"proxy", "-p", "hello"}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	for _, want := range []string{
		"args=--settings ",
		" -p hello\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("local proxy invocation missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"ANTHROPIC_BASE_URL=", "ANTHROPIC_AUTH_TOKEN=", "ANTHROPIC_CUSTOM_HEADERS=", local.URL} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("local proxy invocation leaked %q through argv/env:\n%s", forbidden, got)
		}
	}
	settingsBody, err := os.ReadFile(settingsCopyPath)
	if err != nil {
		t.Fatal(err)
	}
	var overlay struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(settingsBody, &overlay); err != nil {
		t.Fatal(err)
	}
	relayURL, relayErr := url.Parse(overlay.Env["ANTHROPIC_BASE_URL"])
	if relayErr != nil || relayURL.Scheme != "http" || !isLoopbackServerHost(relayURL.Hostname()) ||
		sameEndpoint(overlay.Env["ANTHROPIC_BASE_URL"], local.URL) ||
		overlay.Env["ANTHROPIC_AUTH_TOKEN"] == "" ||
		overlay.Env["ANTHROPIC_AUTH_TOKEN"] == "subrouter" ||
		overlay.Env["ANTHROPIC_CUSTOM_HEADERS"] != "X-Subrouter-Agent: claude" {
		t.Fatalf("local proxy authoritative settings = %+v", overlay.Env)
	}
}

// On the fork's default local route `sr claude proxy` talks through a relay
// that strips every client X-Subrouter-Session and sets only its own, so the
// caller's SUBROUTER_SESSION_KEY (upstream #511) must be the relay's, or it
// never reaches the daemon and each `claude -p` turn is its own session.
func TestSRClaudeProxyLocalRelayCarriesTheCallersSessionKey(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, "state", "codex", "accounts")}
	initializeLocalDataTestStore(t, store)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/_subrouter/health" || request.URL.Path == proxy.StoreHandshakePath {
			writeLocalStoreAuthorityHealth(t, w, request, store, "enabled")
			return
		}
		http.NotFound(w, request)
	}))
	defer local.Close()
	attachPrivateLocalTestListener(t, local)
	t.Setenv("HOME", home)
	t.Setenv("SUBROUTER_CLOUD_CONFIG", filepath.Join(home, "missing-cloud.json"))
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")
	t.Setenv(claudeProxySessionKeyEnv, "optchat-c0eb90ed-turn")

	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var relaySessions []string
	previous := startClaudeLocalRelay
	startClaudeLocalRelay = func(targetRoot, route, agent, sessionID, proxyToken, forcedAccountID, preferredAccountID string, bindingStore accounts.CodexStore) (*nativeProxyRelay, error) {
		relaySessions = append(relaySessions, sessionID)
		return previous(targetRoot, route, agent, sessionID, proxyToken, forcedAccountID, preferredAccountID, bindingStore)
	}
	t.Cleanup(func() { startClaudeLocalRelay = previous })

	runner := srRunner{program: "sr", store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	if err := runner.claude(context.Background(), []string{"proxy", "-p", "hello"}); err != nil {
		t.Fatal(err)
	}
	if len(relaySessions) != 1 || relaySessions[0] != "optchat-c0eb90ed-turn" {
		t.Fatalf("local relay session ids = %q, want the caller's SUBROUTER_SESSION_KEY", relaySessions)
	}
}

// Upstream also ran this picker, unpinned, for a bare `sr claude` and launched
// on the selected remote. This fork keeps bare `sr claude` as the PATCHES 15
// local-first launch (TestSRClaudeFlagAndProfileLaunchesGoThroughTheLocalPool),
// so the picker is reached only through `sr claude proxy --account`.
func TestSRClaudeProxyAccountPickerPinsTheChosenAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = json.NewEncoder(w).Encode([]remoteServerAccount{
			{ID: "claude-one", Label: "Same ID Codex", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
			{ID: "claude-one", Label: "Account One", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
			{ID: "claude-two", Label: "Account Two", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
		})
	}))
	defer server.Close()
	store := accounts.CodexStore{Dir: filepath.Join(home, "state", "codex", "accounts")}
	if err := defaultSRServerStore(store).save(srServerFile{Default: "team", Servers: []srServerConfig{{Name: "team", URL: server.URL}}}); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsCopyPath := filepath.Join(home, "settings.json")
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte("#!/bin/sh\ncp \"$2\" "+shellQuote(settingsCopyPath)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	pinClaudeProxyToSelectedServer(t)
	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader("2\n"),
		out:    &out,
		errOut: &out,
		client: http.DefaultClient,
	}
	if err := runner.claude(context.Background(), []string{"proxy", "--account"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "PINNED process") || !strings.Contains(out.String(), "No account failover") {
		t.Fatalf("interactive pin did not explain no-failover semantics: %q", out.String())
	}
	settingsBody, err := os.ReadFile(settingsCopyPath)
	if err != nil {
		t.Fatal(err)
	}
	var overlay struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(settingsBody, &overlay); err != nil {
		t.Fatal(err)
	}
	if got := overlay.Env["ANTHROPIC_CUSTOM_HEADERS"]; got != "X-Subrouter-Agent: claude\nX-Subrouter-Account-ID: claude-two" {
		t.Fatalf("interactive pinned headers = %q", got)
	}
}

// Upstream sends `sr claude proxy` to the selected remote whenever one is
// selected. Here a selected remote is spill capacity: while this machine's pool
// has room the launch stays local, and the box is never asked.
func TestResolveClaudeProxyRouteKeepsTheSelectedServerAsSpill(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	boxRequests := 0
	box := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		boxRequests++
		w.WriteHeader(http.StatusOK)
	}))
	defer box.Close()
	store := accounts.CodexStore{Dir: filepath.Join(home, "state", "codex", "accounts")}
	if err := defaultSRServerStore(store).save(srServerFile{Default: "home", Servers: []srServerConfig{{Name: "home", URL: box.URL}}}); err != nil {
		t.Fatal(err)
	}
	unlockRemoteGateForTest(t)
	t.Setenv("SUBROUTER_DISABLE_FALLBACK", "")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", filepath.Join(home, "missing-cloud.json"))
	// This machine's daemon reports one Claude account with room for a new
	// session.
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/usage-status") {
			http.NotFound(w, req)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"id": "work", "provider": "claude", "auth_valid": true,
			"normalized": map[string]any{
				"headroom": 0.9, "short_headroom": 0.9,
				"seconds_to_short_reset": 3600, "short_window_seconds": 18000,
			},
		}})
	}))
	defer local.Close()
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL)

	runner := srRunner{program: "sr", store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	route, err := runner.resolveClaudeProxyRoute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if route.remote {
		t.Fatalf("route = remote %q while this machine had capacity: the box became the default launch target", route.server.Name)
	}
	if boxRequests != 0 {
		t.Fatalf("the box was contacted %d time(s) for a launch that stays local", boxRequests)
	}

	t.Setenv("SUBROUTER_DISABLE_FALLBACK", "1")
	route, err = runner.resolveClaudeProxyRoute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !route.remote || route.server.Name != "home" {
		t.Fatalf("SUBROUTER_DISABLE_FALLBACK route = %+v, want the pinned server", route)
	}
}

func TestClaudeProxyScopeMatchesSessionConfigIdentityWithoutExposingCredential(t *testing.T) {
	server := srServerConfig{Name: "team", TenantKey: testTenantKey, TailscaleNodeID: "node-old"}
	scope := claudeProxyScope(server, true)
	if scope != "tenant:"+testTenantKey+"|tailscale-node:node-old|endpoint:/t/"+testTenantKey {
		t.Fatalf("scope = %q", scope)
	}
	opaque := opaqueClaudeProxyScope(scope)
	wantHash := sha256.Sum256([]byte(scope))
	want := fmt.Sprintf("%x", wantHash[:12])
	if opaque != want {
		t.Fatalf("opaque scope = %q, want %q", opaque, want)
	}
	if strings.Contains(opaque, testTenantKey) || strings.Contains(opaque, server.Name) {
		t.Fatalf("opaque scope exposed identity: %q", opaque)
	}
	if got := claudeProxyScope(srServerConfig{Name: "m3", URL: "https://M3.EXAMPLE:31415/", TailscaleNodeID: "node-new"}, true); got != "tailscale-node:node-new|endpoint:https://m3.example:31415" {
		t.Fatalf("Tailscale scope = %q", got)
	}
	if got := claudeProxyScope(srServerConfig{Name: "m3", URL: "https://M3.EXAMPLE/"}, true); got != "server:m3|endpoint:https://m3.example" {
		t.Fatalf("named scope = %q", got)
	}
	if got := claudeProxyScope(srServerConfig{}, false); got != "local" {
		t.Fatalf("local scope = %q", got)
	}
}

func TestClaudeProxyScopeRejectsSameNameAndCredentialEndpointReplacement(t *testing.T) {
	old := srServerConfig{Name: "team", URL: "https://old.example", TenantKey: testTenantKey}
	replacements := []srServerConfig{
		{Name: "team", URL: "https://new.example", TenantKey: testTenantKey},
		{Name: "team", URL: "https://new.example"},
		{Name: "team", URL: "https://new.example", TailscaleNodeID: "same-node"},
	}
	oldScope := opaqueClaudeProxyScope(claudeProxyScope(old, true))
	for _, replacement := range replacements {
		if got := opaqueClaudeProxyScope(claudeProxyScope(replacement, true)); got == oldScope {
			t.Fatalf("endpoint replacement retained scope: old=%+v replacement=%+v", old, replacement)
		}
	}
	canonicalEquivalent := srServerConfig{Name: "team", URL: "https://OLD.EXAMPLE/", TenantKey: testTenantKey}
	if got := opaqueClaudeProxyScope(claudeProxyScope(canonicalEquivalent, true)); got != oldScope {
		t.Fatalf("canonical equivalent endpoint changed scope: got %s want %s", got, oldScope)
	}
}

func TestParseClaudeProxyLaunchArgsBindsReservedScopeBeforeDelimiter(t *testing.T) {
	scope := opaqueClaudeProxyScope("tenant:" + testTenantKey)
	options, gotArgs, err := parseClaudeProxyLaunchArgs([]string{
		"--account", "work", "--sr-expect-scope", scope, "--", "-p", "--resume", "session-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.expectedScope != scope || options.accountSelector != "work" || !reflect.DeepEqual(gotArgs, []string{"-p", "--resume", "session-a"}) {
		t.Fatalf("parsed options/args = %+v, %#v", options, gotArgs)
	}
	for _, args := range [][]string{
		{"--sr-expect-scope"},
		{"--sr-expect-scope", scope, "-p"},
		{"--sr-expect-scope", "not-a-scope", "--"},
		{"--account", "--model", "opus"},
		{"--account=", "-p"},
		{"--account", "one", "--account", "two"},
	} {
		if _, _, err := parseClaudeProxyLaunchArgs(args); err == nil {
			t.Fatalf("malformed reserved args accepted: %#v", args)
		}
	}
	pickerOptions, pickerArgs, err := parseClaudeProxyLaunchArgs([]string{"--account"})
	if err != nil || !pickerOptions.pickPinnedAccount || pickerArgs != nil {
		t.Fatalf("bare --account picker parse = %+v, %#v, %v", pickerOptions, pickerArgs, err)
	}
	pickerOptions, pickerArgs, err = parseClaudeProxyLaunchArgs([]string{"--account", "--", "-p", "hello"})
	if err != nil || !pickerOptions.pickPinnedAccount || !reflect.DeepEqual(pickerArgs, []string{"-p", "hello"}) {
		t.Fatalf("delimited --account picker parse = %+v, %#v, %v", pickerOptions, pickerArgs, err)
	}
	literalOptions, literalArgs, err := parseClaudeProxyLaunchArgs([]string{"--account=work", "-p", "--", "--account", "literal", "--sr-expect-scope", scope})
	if err != nil || literalOptions.expectedScope != "" || literalOptions.accountSelector != "work" || !reflect.DeepEqual(literalArgs, []string{"-p", "--", "--account", "literal", "--sr-expect-scope", scope}) {
		t.Fatalf("literal Claude args changed: options %+v args %#v err %v", literalOptions, literalArgs, err)
	}
	delimiterOptions, delimiterArgs, err := parseClaudeProxyLaunchArgs([]string{"--", "--account", "literal"})
	if err != nil || delimiterOptions != (claudeProxyLaunchOptions{}) || !reflect.DeepEqual(delimiterArgs, []string{"--", "--account", "literal"}) {
		t.Fatalf("literal delimiter changed: options %+v args %#v err %v", delimiterOptions, delimiterArgs, err)
	}
}

// `--account SEL -- claude args` must end wrapper options like the picker
// form does. Passing `--` through made Claude read `--resume ID` as a prompt
// and start a fresh session.
func TestParseClaudeProxyLaunchArgsConsumesDelimiterAfterAccountSelector(t *testing.T) {
	for _, args := range [][]string{
		{"--account", "work", "--", "--resume", "session-a", "msg"},
		{"--account=work", "--", "--resume", "session-a", "msg"},
	} {
		options, gotArgs, err := parseClaudeProxyLaunchArgs(args)
		if err != nil {
			t.Fatalf("%#v: %v", args, err)
		}
		if options.accountSelector != "work" || !reflect.DeepEqual(gotArgs, []string{"--resume", "session-a", "msg"}) {
			t.Fatalf("%#v parsed to %+v, %#v", args, options, gotArgs)
		}
		if got := claudeResumeSessionID(gotArgs); got != "session-a" {
			t.Fatalf("%#v: resume id = %q", args, got)
		}
	}
	options, gotArgs, err := parseClaudeProxyLaunchArgs([]string{"--account", "--", "--resume", "session-a"})
	if err != nil || !options.pickPinnedAccount || !reflect.DeepEqual(gotArgs, []string{"--resume", "session-a"}) {
		t.Fatalf("picker form = %+v, %#v, %v", options, gotArgs, err)
	}
}

func TestClaudeProxyExpectedScopeFailsBeforeLaunch(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, "accounts")}
	if err := defaultSRServerStore(store).save(srServerFile{
		Default: "team",
		Servers: []srServerConfig{{
			Name: "team", URL: "https://router.example", TenantKey: testTenantKey,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(home, "launched")
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte("#!/bin/sh\ntouch "+shellQuote(marker)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	pinClaudeProxyToSelectedServer(t)
	runner := srRunner{program: "sr", store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	err := runner.claude(t.Context(), []string{
		"proxy", "--sr-expect-scope", opaqueClaudeProxyScope("tenant:different"), "--", "-p", "hello",
	})
	if err == nil || !strings.Contains(err.Error(), "scope changed") {
		t.Fatalf("scope mismatch error = %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Claude launched after scope mismatch: %v", statErr)
	}
}

func TestClaudeProxyExpectedScopeRejectsSameKeyURLReplacementBeforeLaunch(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, "accounts")}
	oldServer := srServerConfig{Name: "team", URL: "https://old.example", TenantKey: testTenantKey}
	newServer := srServerConfig{Name: "team", URL: "https://new.example", TenantKey: testTenantKey}
	if err := defaultSRServerStore(store).save(srServerFile{Default: "team", Servers: []srServerConfig{newServer}}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(home, "launched")
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte("#!/bin/sh\ntouch "+shellQuote(marker)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	pinClaudeProxyToSelectedServer(t)
	runner := srRunner{program: "sr", store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	err := runner.claude(t.Context(), []string{
		"proxy", "--sr-expect-scope", opaqueClaudeProxyScope(claudeProxyScope(oldServer, true)), "--", "-p", "hello",
	})
	if err == nil || !strings.Contains(err.Error(), "scope changed") {
		t.Fatalf("same-key endpoint replacement error = %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Claude launched after same-key endpoint replacement: %v", statErr)
	}
}

func TestClaudeDirectUsesAuthoritativePrivateSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(home, "claude-direct.txt")
	settingsCopyPath := filepath.Join(home, "claude-direct-settings.json")
	claudePath := filepath.Join(binDir, "claude")
	script := "#!/bin/sh\n{ printf 'args=%s\\n' \"$*\"; env | grep -E '^(ANTHROPIC_|SUBROUTER_|CLAUDE_CONFIG_DIR=|CLAUDE_CODE_(USE|API|AUTH|BASE|OAUTH|CONFIG))' || true; } > " + shellQuote(recordPath) + "\nsettings=\nwhile [ $# -gt 0 ]; do if [ \"$1\" = --settings ]; then settings=$2; break; fi; shift; done\n[ -n \"$settings\" ] && cat \"$settings\" > " + shellQuote(settingsCopyPath) + "\n"
	if err := os.WriteFile(claudePath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ANTHROPIC_BASE_URL", "http://stale.invalid")
	t.Setenv("ANTHROPIC_API_KEY", "stale-api-key")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("CLAUDE_CONFIG_DIR", "/stale/config")
	t.Setenv("SUBROUTER_ADMIN_TOKEN", "durable-admin-secret")
	t.Setenv("SUBROUTER_FUTURE_SECRET_FILE", "/private/future-secret")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", "/private/cloud-config")
	t.Setenv("SUBROUTER_STATE_DIR", "/private/state")

	runner := srRunner{in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	if err := runner.claudeDirect(t.Context(), []string{
		"-p", "hello", "--settings", `{"env":{"ANTHROPIC_BASE_URL":"http://attacker.invalid"}}`,
		"--setting-sources", "user,project,local", "--", "--setting-sources", "literal",
	}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, "args=--setting-sources  --settings ") ||
		!strings.Contains(got, " -p hello -- --setting-sources literal\n") {
		t.Fatalf("direct Claude invocation missing authoritative settings or flags:\n%s", got)
	}
	if strings.Contains(got, "user,project,local") {
		t.Fatalf("direct Claude invocation retained caller setting sources:\n%s", got)
	}
	if strings.Contains(got, "attacker.invalid") || strings.Contains(got, "stale.invalid") || strings.Contains(got, "stale-api-key") || strings.Contains(got, "durable-admin-secret") || strings.Contains(got, "/private/future-secret") || strings.Contains(got, "/private/cloud-config") || strings.Contains(got, "/private/state") {
		t.Fatalf("direct Claude invocation retained hostile routing:\n%s", got)
	}
	for _, key := range []string{"ANTHROPIC_BASE_URL=", "ANTHROPIC_API_KEY=", "CLAUDE_CODE_USE_BEDROCK="} {
		if strings.Contains(got, key) {
			t.Fatalf("direct Claude environment retained %s:\n%s", key, got)
		}
	}
	for _, forbidden := range []string{"CLAUDE_CONFIG_DIR=", "CLAUDE_CODE_CONFIG_DIR="} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("direct Claude environment retained config selector %s:\n%s", forbidden, got)
		}
	}
	settingsBody, err := os.ReadFile(settingsCopyPath)
	if err != nil {
		t.Fatal(err)
	}
	var overlay struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(settingsBody, &overlay); err != nil {
		t.Fatal(err)
	}
	for _, key := range claudeRoutingEnvKeys {
		if key == "CLAUDE_CONFIG_DIR" || key == "CLAUDE_CODE_CONFIG_DIR" {
			if _, ok := overlay.Env[key]; ok {
				t.Fatalf("direct Claude setting unexpectedly selected %s", key)
			}
			continue
		}
		if got, ok := overlay.Env[key]; !ok || got != "" {
			t.Fatalf("direct Claude setting %s = %q, present %v; want authoritative empty", key, got, ok)
		}
	}
}

func TestSRClaudeHelpDocumentsProfilelessProxy(t *testing.T) {
	var out bytes.Buffer
	runner := srRunner{out: &out, errOut: &out}
	if err := runner.claude(context.Background(), []string{"help"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"sr claude proxy [options] [args...]",
		"sr claude proxy --resume ID",
		"--account [ACCOUNT]",
		"Pin to one profile with no account failover",
		"args at/after -- are literal",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("help missing %q:\n%s", want, out.String())
		}
	}
}

func TestLocalClaudeRejectsWrongStoreBeforeAccountInventory(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, "expected", "codex", "accounts")}
	wrongStore := accounts.CodexStore{Dir: filepath.Join(home, "wrong", "codex", "accounts")}
	var inventoryRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/_subrouter/health" {
			writeLocalStoreAuthorityHealth(t, w, request, wrongStore, "enabled")
			return
		}
		inventoryRequests.Add(1)
		http.Error(w, "credential sink", http.StatusUnauthorized)
	}))
	defer server.Close()
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", server.URL+"/v1")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", filepath.Join(home, "missing-cloud-config.json"))

	runner := srRunner{store: store, client: server.Client(), out: io.Discard, errOut: io.Discard}
	err := runner.proxyClaudeSelectedRemote(t.Context(), nil, claudeProxyLaunchOptions{accountSelector: "profile"})
	if err == nil || !strings.Contains(err.Error(), "local proxy account store does not match this CLI") {
		t.Fatalf("wrong-store Claude launch error = %v", err)
	}
	if got := inventoryRequests.Load(); got != 0 {
		t.Fatalf("wrong-store listener received %d account inventory request(s)", got)
	}
}

func TestPrepareClaudeLoginFastPathSeedsFreshDir(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "profile")
	if err := prepareClaudeLoginFastPath(configDir); err != nil {
		t.Fatalf("prepareClaudeLoginFastPath: %v", err)
	}
	state, err := os.ReadFile(filepath.Join(configDir, ".claude.json"))
	if err != nil {
		t.Fatalf("read .claude.json: %v", err)
	}
	if !strings.Contains(string(state), `"hasCompletedOnboarding": true`) {
		t.Fatalf("onboarding not seeded:\n%s", state)
	}
	settings, err := os.ReadFile(filepath.Join(configDir, "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	if !strings.Contains(string(settings), `"forceLoginMethod": "claudeai"`) {
		t.Fatalf("login method not seeded:\n%s", settings)
	}
}

// Re-running add against an already-seeded profile must not touch either file:
// same bytes, same mtimes, no backups. The seeding otherwise races whatever
// Claude Code is doing with its own config dir.
func TestPrepareClaudeLoginFastPathRerunTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	if err := prepareClaudeLoginFastPath(dir); err != nil {
		t.Fatalf("prepareClaudeLoginFastPath: %v", err)
	}
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	paths := []string{
		filepath.Join(dir, ".claude.json"),
		filepath.Join(dir, "settings.json"),
	}
	for _, path := range paths {
		// Both files were freshly seeded, so no backups may exist.
		if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
			t.Fatalf("fresh seed created %s.bak: %v", path, err)
		}
		if err := os.Chtimes(path, past, past); err != nil {
			t.Fatal(err)
		}
	}
	if err := prepareClaudeLoginFastPath(dir); err != nil {
		t.Fatalf("prepareClaudeLoginFastPath rerun: %v", err)
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(past) {
			t.Fatalf("rerun rewrote %s (mtime %v, want %v)", path, info.ModTime(), past)
		}
		if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
			t.Fatalf("rerun created %s.bak: %v", path, err)
		}
	}
}

// When seeding does modify a lived-in file, the prior bytes — in the user's
// own formatting, which the canonical re-marshal loses — land in a .bak next
// to it.
func TestPrepareClaudeLoginFastPathBacksUpModifiedState(t *testing.T) {
	dir := t.TempDir()
	seed := "{\"theme\":   \"light\"}\n"
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareClaudeLoginFastPath(dir); err != nil {
		t.Fatalf("prepareClaudeLoginFastPath: %v", err)
	}
	backup, err := os.ReadFile(filepath.Join(dir, ".claude.json.bak"))
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != seed {
		t.Fatalf(".claude.json.bak does not hold the original bytes:\n%s", backup)
	}
	// settings.json did not exist beforehand, so it gets no backup.
	if _, err := os.Stat(filepath.Join(dir, "settings.json.bak")); !os.IsNotExist(err) {
		t.Fatalf("fresh settings.json write created a backup: %v", err)
	}
}

func TestPrepareClaudeLoginFastPathPreservesExistingChoices(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"theme":"light"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"forceLoginMethod":"console"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareClaudeLoginFastPath(dir); err != nil {
		t.Fatalf("prepareClaudeLoginFastPath: %v", err)
	}
	state, _ := os.ReadFile(filepath.Join(dir, ".claude.json"))
	if !strings.Contains(string(state), `"theme": "light"`) || !strings.Contains(string(state), `"hasCompletedOnboarding": true`) {
		t.Fatalf("existing state not preserved:\n%s", state)
	}
	settings, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	if !strings.Contains(string(settings), `"forceLoginMethod":"console"`) {
		t.Fatalf("existing login method overwritten:\n%s", settings)
	}
}

// installFakeClaudeCLI puts a stub `claude` on PATH. claude.DetectCLI is just
// exec.LookPath("claude"), so this is enough to drive claudeRunner.add with no
// real Claude install. The stub appends every invocation and the
// CLAUDE_CONFIG_DIR it ran against to a log, which is what the isolation
// assertions read. Each login mints a new access token, as a real one does,
// so a re-auth is told from a login that never reached the profile.
// supportsAuthLogin models an older Claude that only offers the interactive
// TUI.
func installFakeClaudeCLI(t *testing.T, home, email string, supportsAuthLogin bool) string {
	t.Helper()
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(home, "claude-invocations.log")
	supports := "no"
	if supportsAuthLogin {
		supports = "yes"
	}
	script := "#!/bin/sh\n" +
		"LOG='" + logPath + "'\n" +
		"SUPPORTS='" + supports + "'\n" +
		"if [ \"$1\" = auth ] && [ \"$2\" = login ]; then\n" +
		"  if [ \"$SUPPORTS\" != yes ]; then exit 1; fi\n" +
		"  if [ \"$3\" = --help ]; then exit 0; fi\n" +
		"  printf 'login %s\\n' \"$CLAUDE_CONFIG_DIR\" >> \"$LOG\"\n" +
		"  mkdir -p \"$CLAUDE_CONFIG_DIR\"\n" +
		"  printf '{\"claudeAiOauth\":{\"accessToken\":\"tok-%s\"}}' \"$$\" > \"$CLAUDE_CONFIG_DIR/.credentials.json\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = auth ] && [ \"$2\" = status ]; then\n" +
		"  printf 'status %s\\n' \"$CLAUDE_CONFIG_DIR\" >> \"$LOG\"\n" +
		"  printf '{\"loggedIn\":true,\"email\":\"" + email + "\",\"subscriptionType\":\"max\"}'\n" +
		"  exit 0\n" +
		"fi\n" +
		"printf 'tui %s\\n' \"$CLAUDE_CONFIG_DIR\" >> \"$LOG\"\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func readInvocations(t *testing.T, logPath string) string {
	t.Helper()
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read invocation log: %v", err)
	}
	return string(body)
}

// The bare `claude` TUI refuses to start without a controlling terminal: it
// degrades into `claude --print` and dies on the empty prompt, which meant
// `sr add claude` only ever worked when a human typed it into a terminal.
// Driving the login through `claude auth login` makes it work from a script,
// an agent, or a non-interactive SSH session.
func TestClaudeAddCompletesWithoutATerminal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	logPath := installFakeClaudeCLI(t, home, "fifteen@example.com", true)

	store := claude.Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	var out bytes.Buffer
	// A strings.Reader is not a terminal. That is the whole point of the test.
	runner := claudeRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}

	if err := runner.addOAuth(context.Background(), "fifteen"); err != nil {
		t.Fatalf("add without a terminal: %v\noutput:\n%s", err, out.String())
	}
	if _, ok := store.FindProfile("fifteen"); !ok {
		t.Fatal("profile was not registered")
	}
	invocations := readInvocations(t, logPath)
	if !strings.Contains(invocations, "login ") {
		t.Fatalf("auth login never ran; invocations:\n%s", invocations)
	}
	if strings.Contains(invocations, "tui ") {
		t.Fatalf("fell back to the TUI path instead of auth login; invocations:\n%s", invocations)
	}
}

// The login must run against the profile's own config directory. If it ran
// against the machine's live ~/.claude it would overwrite whichever account is
// signed in there, which is how the Codex side of this command used to revoke
// an account on every add.
func TestClaudeAddLeavesTheLiveClaudeConfigAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	logPath := installFakeClaudeCLI(t, home, "fifteen@example.com", true)

	liveDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(liveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	liveCredential := filepath.Join(liveDir, ".credentials.json")
	want := []byte(`{"claudeAiOauth":{"accessToken":"live-session"}}`)
	if err := os.WriteFile(liveCredential, want, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(liveCredential)
	if err != nil {
		t.Fatal(err)
	}

	store := claude.Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	var out bytes.Buffer
	runner := claudeRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.addOAuth(context.Background(), "fifteen"); err != nil {
		t.Fatalf("add: %v", err)
	}

	for _, line := range strings.Split(strings.TrimSpace(readInvocations(t, logPath)), "\n") {
		invocation, configDir, ok := strings.Cut(line, " ")
		if !ok {
			t.Fatalf("unparsable invocation %q", line)
		}
		if !strings.HasPrefix(configDir, store.Dir) {
			t.Fatalf("claude %s ran against %q, outside the store at %q", invocation, configDir, store.Dir)
		}
	}
	got, err := os.ReadFile(liveCredential)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("live credential was rewritten: %s", got)
	}
	// Byte equality alone would also pass if the file were rewritten with the
	// same contents; the mtime is what separates untouched from clobbered.
	after, err := os.Stat(liveCredential)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("live credential was rewritten: mtime %s -> %s", before.ModTime(), after.ModTime())
	}
}

const storeRuleLoginToken = "fresh-login-access"

// installStoreRuleFakeClaudeCLI is a stub `claude` for where a login lands.
// Its `auth login` saves the credential where Claude Code (2.1.329) does, in
// ${CLAUDE_SECURESTORAGE_CONFIG_DIR:-$CLAUDE_CONFIG_DIR}, or always in
// redirect when that is set, as a wrapper that exports the variable would.
// Its `auth status` says logged in whatever the profile holds, as Claude Code
// does through the claude-route shim's placeholder token.
func installStoreRuleFakeClaudeCLI(t *testing.T, home, redirect string) {
	t.Helper()
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := `${CLAUDE_SECURESTORAGE_CONFIG_DIR:-$CLAUDE_CONFIG_DIR}`
	if redirect != "" {
		dir = "'" + redirect + "'"
	}
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = auth ] && [ \"$2\" = login ]; then\n" +
		"  if [ \"$3\" = --help ]; then exit 0; fi\n" +
		"  dir=" + dir + "\n" +
		"  mkdir -p \"$dir\"\n" +
		"  printf '{\"claudeAiOauth\":{\"accessToken\":\"" + storeRuleLoginToken + "\"}}' > \"$dir/.credentials.json\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = auth ] && [ \"$2\" = status ]; then\n" +
		"  printf '{\"loggedIn\":true,\"authMethod\":\"oauth_token\"}'\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// Claude Code names its credential store after CLAUDE_SECURESTORAGE_CONFIG_DIR
// ahead of CLAUDE_CONFIG_DIR, and the claude-route shim exports one for every
// pooled session. `sr claude addr` run there saved the login in that store
// and the profile had none (PATCHES §73).
func TestClaudeAddLandsInTheProfileStoreDespiteAnInheritedSecureStorageDir(t *testing.T) {
	claude.KeychainWithNoItemsForTest(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	elsewhere := filepath.Join(home, "elsewhere")
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", elsewhere)
	installStoreRuleFakeClaudeCLI(t, home, "")

	store := claude.Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	var out bytes.Buffer
	runner := claudeRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.addOAuth(t.Context(), "work"); err != nil {
		t.Fatalf("add: %v\n%s", err, out.String())
	}
	credential, err := store.ReadCredential(t.Context(), store.ClaudeConfigDir("work"))
	if err != nil || credential == nil || credential.AccessToken != storeRuleLoginToken {
		t.Fatalf("profile credential = %+v, err %v; want the login's access token", credential, err)
	}
	if _, err := os.Stat(filepath.Join(elsewhere, ".credentials.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the login was saved to the inherited store too: %v", err)
	}
}

// A `claude` on PATH that saves the login to another store still answers
// `auth status` with logged in, and sr printed "Added Claude profile" over a
// profile with no credential. The verdict is the profile's own credential: a
// profile this command created is rolled back, and one that already existed
// keeps what it held (PATCHES §73).
func TestClaudeAddFailsWhenTheLoginLandsInAnotherStore(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%v", existing), func(t *testing.T) {
			claude.KeychainWithNoItemsForTest(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			installStoreRuleFakeClaudeCLI(t, home, filepath.Join(home, "elsewhere"))
			store := claude.Store{Dir: filepath.Join(home, ".subrouter", "codex")}
			if existing {
				if _, err := store.UpsertCredentialProfile("work", claude.CredentialInfo{
					AccessToken: "earlier-access", RefreshToken: "earlier-refresh",
					ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
				}); err != nil {
					t.Fatal(err)
				}
			}

			var out bytes.Buffer
			runner := claudeRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
			err := runner.addOAuth(t.Context(), "work")
			if err == nil || !strings.Contains(err.Error(), "CLAUDE_SECURESTORAGE_CONFIG_DIR") {
				t.Fatalf("add = %v; want an error naming CLAUDE_SECURESTORAGE_CONFIG_DIR\n%s", err, out.String())
			}
			if _, registered := store.FindProfile("work"); registered != existing {
				t.Fatalf("profile registered = %v after the failed add, want %v", registered, existing)
			}
			if existing {
				credential, err := store.ReadCredential(t.Context(), store.ClaudeConfigDir("work"))
				if err != nil || credential == nil || credential.AccessToken != "earlier-access" {
					t.Fatalf("existing profile's credential = %+v, err %v; want the one it held", credential, err)
				}
			}
		})
	}
}

// installScriptedClaudeCLI puts a stub `claude` on PATH whose `auth status`
// says logged in and whose other verbs run the given shell. The login verdict
// never trusts `auth status` (PATCHES §73), so these tests decide what the
// profile's own store holds through that shell alone.
func installScriptedClaudeCLI(t *testing.T, home, authLogin, tui string) {
	t.Helper()
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = auth ] && [ \"$2\" = status ]; then\n" +
		"  printf '{\"loggedIn\":true,\"authMethod\":\"oauth_token\"}'\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = auth ] && [ \"$2\" = login ]; then\n" + authLogin + "\nfi\n" +
		tui + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func writeProfileCredentialFile(t *testing.T, store claude.Store, name, accessToken string) {
	t.Helper()
	path := filepath.Join(store.ClaudeConfigDir(name), ".credentials.json")
	body := `{"claudeAiOauth":{"accessToken":"` + accessToken + `","refreshToken":"r","expiresAt":` +
		strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10) + `}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A re-auth tells the new login from the old one by its access token, so it
// needs the old one. When the profile's store could not be read before the
// login, a login saved elsewhere left the old credential in place and sr took
// it for the new one. The re-auth now stops before the browser step.
//
// The credential path is a directory. The store refuses it (EISDIR) as it
// refuses a file it may not read, and root cannot read it either, as it can a
// mode-0 file. The hermetic test container runs as root.
func TestClaudeReauthStopsWhenTheProfilesCredentialCannotBeRead(t *testing.T) {
	claude.KeychainWithNoItemsForTest(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	logPath := filepath.Join(home, "logins.log")
	elsewhere := filepath.Join(home, "elsewhere")
	installScriptedClaudeCLI(t, home,
		"  [ \"$3\" = --help ] && exit 0\n"+
			"  echo login >> '"+logPath+"'\n"+
			"  mkdir -p '"+elsewhere+"'\n"+
			"  printf '{\"claudeAiOauth\":{\"accessToken\":\"fresh\"}}' > '"+elsewhere+"/.credentials.json'\n"+
			"  exit 0",
		"exit 1")
	store := claude.Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}
	credentialPath := filepath.Join(store.ClaudeConfigDir("work"), ".credentials.json")
	if err := os.Mkdir(credentialPath, 0o700); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := claudeRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.addOAuth(t.Context(), "work"); err == nil {
		t.Fatalf("re-auth with an unreadable profile credential succeeded\n%s", out.String())
	}
	if _, err := os.Stat(logPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the browser login ran although the old credential could not be read: %v", err)
	}
	if _, ok := store.FindProfile("work"); !ok {
		t.Fatal("the existing profile was removed")
	}
	if info, err := os.Lstat(credentialPath); err != nil || !info.IsDir() {
		t.Fatalf("the unreadable credential was replaced: %v %v", info, err)
	}
}

// Claude builds without `auth login` log in through the TUI, which sr closes
// once the login lands. On a re-auth the profile already held a credential, so
// sr closed Claude at its first check, before the user could log in.
func TestClaudeReauthThroughTheTUIWaitsForTheNewLogin(t *testing.T) {
	claude.KeychainWithNoItemsForTest(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	installScriptedClaudeCLI(t, home, "  exit 1",
		"trap 'exit 130' INT TERM\n"+
			"sleep 3 & wait $!\n"+
			"printf '{\"claudeAiOauth\":{\"accessToken\":\"tui-login\",\"refreshToken\":\"r2\"}}' > \"$CLAUDE_CONFIG_DIR/.credentials.json\"\n"+
			"sleep 30 & wait $!")
	store := claude.Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}
	writeProfileCredentialFile(t, store, "work", "earlier-access")

	// The TUI's output is copied into out by exec's goroutine while sr writes
	// its own lines there.
	var out lockedBuffer
	runner := claudeRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.addOAuth(t.Context(), "work"); err != nil {
		t.Fatalf("TUI re-auth: %v\n%s", err, out.String())
	}

	credential, err := store.ReadCredential(t.Context(), store.ClaudeConfigDir("work"))
	if err != nil || credential == nil || credential.AccessToken != "tui-login" {
		t.Fatalf("profile credential = %+v, err %v; want the TUI login's", credential, err)
	}
}

// When the login's credential cannot be read back, the new profile is kept,
// since the login may be in it. The daemon had loaded the profile's creation,
// with no credential, and reloads only on a new generation, so the kept
// profile is published too. The login leaves a directory where its credential
// belongs, which the store refuses as root too.
func TestClaudeAddKeptAfterAnUnreadableCredentialIsPublished(t *testing.T) {
	claude.KeychainWithNoItemsForTest(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := claude.Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	generationPath := filepath.Join(store.Dir, ".account-generation")
	atLogin := filepath.Join(home, "generation-at-login")
	installScriptedClaudeCLI(t, home,
		"  [ \"$3\" = --help ] && exit 0\n"+
			"  cp '"+generationPath+"' '"+atLogin+"'\n"+
			"  mkdir \"$CLAUDE_CONFIG_DIR/.credentials.json\"\n"+
			"  exit 0",
		"exit 1")

	var out bytes.Buffer
	runner := claudeRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	err := runner.addOAuth(t.Context(), "work")
	if err == nil || !strings.Contains(err.Error(), "was kept") {
		t.Fatalf("add = %v; want the profile kept\n%s", err, out.String())
	}
	if _, ok := store.FindProfile("work"); !ok {
		t.Fatal("the kept profile is not registered")
	}
	before, err := os.ReadFile(atLogin)
	if err != nil {
		t.Fatalf("the stub did not see a generation at login: %v", err)
	}
	after, err := os.ReadFile(generationPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(before, after) {
		t.Fatal("the kept profile was not published after the login")
	}
}

// Older Claude builds have no `auth login` subcommand, and must keep the
// interactive TUI path rather than failing outright.
func TestClaudeSupportsAuthLoginDetectsMissingSubcommand(t *testing.T) {
	home := t.TempDir()

	installFakeClaudeCLI(t, home, "x@example.com", false)
	claudePath, ok := claude.DetectCLI()
	if !ok {
		t.Fatal("fake claude is not on PATH")
	}
	if claudeSupportsAuthLogin(context.Background(), claudePath) {
		t.Fatal("probe reported auth login support for a CLI that lacks it")
	}

	installFakeClaudeCLI(t, home, "x@example.com", true)
	if !claudeSupportsAuthLogin(context.Background(), claudePath) {
		t.Fatal("probe missed a CLI that has auth login")
	}
}

// Claude wraps the authorization URL in OSC-8 hyperlink escapes and emits it
// twice back to back, once as the link target and once as the visible text. A
// terminal renders that as one link, but anything capturing the output to a
// file - a script, or an agent driving the login - sees an unusable smear.
func TestClaudeLoginURLEchoRepeatsACleanURL(t *testing.T) {
	const url = "https://claude.com/cai/oauth/authorize?code=true&client_id=abc&state=xyz"
	raw := "If the browser didn't open, visit: \x1b]8;;" + url + "\x07" + url + "\x1b]8;;\x07\n"

	var out bytes.Buffer
	echo := &claudeLoginURLEcho{out: &out}
	// Split inside the URL to prove the match survives a chunk boundary.
	split := strings.Index(raw, url) + 20
	if _, err := echo.Write([]byte(raw[:split])); err != nil {
		t.Fatal(err)
	}
	if _, err := echo.Write([]byte(raw[split:])); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if !strings.Contains(got, "Login URL: "+url+"\n") {
		t.Fatalf("clean URL line missing:\n%q", got)
	}
	if strings.Contains(got, url+url) && !strings.Contains(got, "\x07"+url) {
		t.Fatalf("clean URL ran into the duplicated copy:\n%q", got)
	}
	if n := strings.Count(got, "Login URL:"); n != 1 {
		t.Fatalf("printed the URL line %d times, want exactly 1:\n%q", n, got)
	}
	if !strings.Contains(got, raw[:30]) {
		t.Fatalf("original output was not forwarded verbatim:\n%q", got)
	}
}

func TestResolveAddTargetReauthsInsteadOfErroring(t *testing.T) {
	store := claude.Store{Dir: t.TempDir()}
	created, err := store.CreateProfile("seven-alpha")
	if err != nil {
		t.Fatal(err)
	}

	// An expired or revoked refresh token has no other first-party repair path,
	// so `sr claude add <existing>` must sign in again rather than refuse.
	name, got, reauth, err := resolveAddTarget(store, "seven-alpha")
	if err != nil {
		t.Fatalf("re-adding an existing profile errored: %v", err)
	}
	if name != "seven-alpha" {
		t.Fatalf("re-auth resolved profile %q, want seven-alpha", name)
	}
	if !reauth {
		t.Fatal("existing profile must report reauth=true: the caller keys discard() " +
			"off it, and discarding would DELETE a recoverable account when a login fails")
	}
	if got != created {
		t.Fatalf("re-auth path = %q, want the existing profile's %q", got, created)
	}

	_, fresh, reauth, err := resolveAddTarget(store, "eight-alpha")
	if err != nil {
		t.Fatal(err)
	}
	if reauth {
		t.Fatal("a brand-new profile must not report reauth")
	}
	if fresh == created {
		t.Fatalf("two different labels resolved to the SAME dir %q", fresh)
	}
}

func TestResolveAddTargetHandlesOpaqueProfileDir(t *testing.T) {
	// Real profiles are stored under an opaque _p<epoch> directory as often as
	// under their own label, so the path can never be derived from the name.
	store := claude.Store{Dir: t.TempDir()}
	if err := store.RegisterProfile("seven-alpha", "_p1787439512239"); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterProfile("main", "_p1787439913450"); err != nil {
		t.Fatal(err)
	}

	_, got, reauth, err := resolveAddTarget(store, "seven-alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !reauth {
		t.Fatal("registered profile must resolve as a re-auth")
	}
	if !strings.HasSuffix(got, "_p1787439512239") {
		t.Fatalf("resolved %q, want the registry's opaque dir _p1787439512239", got)
	}
	_, other, _, err := resolveAddTarget(store, "main")
	if err != nil {
		t.Fatal(err)
	}
	if got == other {
		t.Fatalf("seven-alpha and main both resolved to %q -- re-auth would "+
			"overwrite whichever account is actually serving", got)
	}
}

// A name that differs only in case re-auths the existing profile under its own
// spelling. Upstream's addOAuth matched case-insensitively; a box matches Claude
// names the same way, so a case twin here would overwrite its sibling there.
func TestResolveAddTargetReauthsCaseVariantUnderItsOwnName(t *testing.T) {
	store := claude.Store{Dir: t.TempDir()}
	created, err := store.CreateProfile("main")
	if err != nil {
		t.Fatal(err)
	}
	name, got, reauth, err := resolveAddTarget(store, "Main")
	if err != nil {
		t.Fatal(err)
	}
	if !reauth || name != "main" || got != created {
		t.Fatalf("resolveAddTarget(Main) = %q, %q, reauth=%v; want the existing main at %q", name, got, reauth, created)
	}
	if len(store.ListProfiles()) != 1 {
		t.Fatalf("a case variant created a twin profile: %+v", store.ListProfiles())
	}
}

// A successful add used to upload the credential to the configured server every
// time, which left one refresh-token chain in two places: the two daemons race
// to refresh it and the loser dies permanently. That is how seven-alpha and
// ten-alpha were lost. An OAuth add never uploads now (the 2026-09 grammar
// pass): the credential reaches a server only by `sr claude move`.
func TestClaudeOAuthAddNeverUploads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	installFakeClaudeCLI(t, home, "fifteen@example.com", true)
	store := claude.Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	var out bytes.Buffer
	var pushed []string
	r := claudeRunner{
		store: store, in: strings.NewReader(""), out: &out, errOut: &out,
		pushAfterAdd: func(_ context.Context, name string) error {
			pushed = append(pushed, name)
			return nil
		},
	}
	for _, args := range [][]string{{"addr", "fifteen"}, {"login", "fifteen"}, {"add", "fifteen", "--oauth"}} {
		if err := r.run(context.Background(), args); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out.String())
		}
	}
	if len(pushed) != 0 {
		t.Fatalf("an OAuth add uploaded to the server: %v -- this creates the "+
			"credential-refresh race that permanently kills one copy", pushed)
	}
}

func TestProxyClaudeEnablesUpstreamModelDiscovery(t *testing.T) {
	body, err := proxyClaudeLaunchSettings("https://router.example", "test-token", "")
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Env         map[string]string `json:"env"`
		ModelPicker json.RawMessage   `json:"modelPicker"`
		Model       json.RawMessage   `json:"model"`
	}
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatal(err)
	}
	if settings.Env["CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"] != "1" {
		t.Fatal("pooled launch does not enable upstream model discovery")
	}
	if len(settings.ModelPicker) != 0 || len(settings.Model) != 0 {
		t.Fatal("launcher still owns the model list or default")
	}
	for k, v := range settings.Env {
		if v != "" && (k == "ANTHROPIC_MODEL" || strings.HasPrefix(k, "ANTHROPIC_DEFAULT_") || strings.HasPrefix(k, "ANTHROPIC_CUSTOM_MODEL")) {
			t.Fatalf("launcher pins a model with %s", k)
		}
	}
	direct, err := managedClaudeLaunchSettings("https://router.example", "")
	if err != nil {
		t.Fatal(err)
	}
	var directSettings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(direct, &directSettings); err != nil {
		t.Fatal(err)
	}
	if directSettings.Env["CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"] != "" {
		t.Fatal("direct profile discovery was changed")
	}
}

// Claude treats an explicitly empty --settings env value as authoritative, so
// the managed launch override must restate the profile's own credential rather
// than blank it and hope the profile settings.json fills the gap. Claude Code
// 2.1.281 stopped falling through and every managed launch hit "Not logged in".
func TestManagedClaudeLaunchSettingsCarriesProfileCredential(t *testing.T) {
	dir := t.TempDir()
	profileSettings := `{"env":{` +
		`"ANTHROPIC_BASE_URL":"http://127.0.0.1:1",` +
		`"ANTHROPIC_AUTH_TOKEN":"subrouter",` +
		`"ANTHROPIC_CUSTOM_HEADERS":"X-Subrouter-Agent: claude",` +
		`"CLAUDE_CODE_USE_BEDROCK":"1",` +
		`"SUBROUTER_CLAUDE_SERVER_URL":"http://router.example:31415"}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(profileSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	body, err := managedClaudeLaunchSettings("http://100.64.0.1:31415", dir)
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"ANTHROPIC_BASE_URL":       "http://100.64.0.1:31415",
		"ANTHROPIC_AUTH_TOKEN":     "subrouter",
		"ANTHROPIC_CUSTOM_HEADERS": "X-Subrouter-Agent: claude",
		"CLAUDE_CODE_USE_BEDROCK":  "",
		"CLAUDE_CONFIG_DIR":        dir,
	}
	for key, value := range want {
		if got, ok := settings.Env[key]; !ok || got != value {
			t.Fatalf("launch override %s = %q (present %v), want %q", key, got, ok, value)
		}
	}
}

// A caller that runs many short `claude -p` processes for one conversation
// (the cmux Home Chief: one fresh process per turn) names the conversation
// with SUBROUTER_SESSION_KEY. The proxy sends it as X-Subrouter-Session, so
// the server keeps those processes on one sticky account and the prompt
// cache they share stays readable. Without the variable nothing changes.
func TestProxyClaudeLaunchSettingsSendsTheCallersSessionKey(t *testing.T) {
	headers := func(t *testing.T) string {
		t.Helper()
		body, err := proxyClaudeLaunchSettings("https://subrouter.example/v1", "route-token", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		var settings struct {
			Env map[string]string `json:"env"`
		}
		if err := json.Unmarshal(body, &settings); err != nil {
			t.Fatal(err)
		}
		return settings.Env["ANTHROPIC_CUSTOM_HEADERS"]
	}
	previous := srClientName
	srClientName = func() string { return "" }
	t.Cleanup(func() { srClientName = previous })
	t.Setenv(claudeProxySessionKeyEnv, "")
	if got := headers(t); strings.Contains(got, "X-Subrouter-Session") {
		t.Fatalf("no key set, headers %q", got)
	}
	t.Setenv(claudeProxySessionKeyEnv, "optchat-c0eb90ed-turn")
	if got := headers(t); !strings.Contains(got, "\nX-Subrouter-Session: optchat-c0eb90ed-turn") {
		t.Fatalf("headers %q lack the session key", got)
	}
	for _, bad := range []string{"two\nlines", "has space", strings.Repeat("k", 129)} {
		t.Setenv(claudeProxySessionKeyEnv, bad)
		if got := headers(t); strings.Contains(got, "X-Subrouter-Session") {
			t.Fatalf("invalid key %q was sent: %q", bad, got)
		}
	}
}

// An account draining its last day takes new sessions on the daemon's verdict
// while its 5h burn projection alone would refuse it: 10% of the 5h window left
// an hour before its reset, after spending 90% in four hours. `sr claude` must
// count that as this machine's capacity, as the daemon's router does, and not
// spill the launch to a server.
func TestLocalClaudeCapacityFollowsTheDaemonsVerdict(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/usage-status") {
			http.NotFound(w, req)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"id": "main", "provider": "claude", "auth_valid": true,
			"normalized": map[string]any{
				"headroom": 0.10, "short_headroom": 0.10,
				"seconds_to_short_reset": 3600, "short_window_seconds": 18000,
				"usable":      false,
				"new_session": map[string]any{"eligible": true, "reason": "burndown", "burndown": true},
			},
		}})
	}))
	defer local.Close()

	runner := srRunner{program: "sr", in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	has, known := runner.localClaudeCapacity(local.URL)(context.Background())
	if !has || !known {
		t.Fatalf("capacity = (%v, %v), want (true, true): the daemon routes new sessions to main", has, known)
	}
}
