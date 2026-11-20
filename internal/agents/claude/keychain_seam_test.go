package claude

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// keychainItemKey addresses one fake keychain item the way `security` does.
type keychainItemKey struct {
	service string
	account string
}

// fakeKeychain emulates the observable behavior of macOS `security` for
// generic passwords, including the sharp edge this store was cut on:
// `security find-generic-password -w` prints item data verbatim only when
// every byte is printable ASCII, and hands anything else back hex-encoded.
// Tests swap it in via swapKeychain so the keychain-backed store paths run on
// every OS instead of only on a Mac with a login keychain.
type fakeKeychain struct {
	items     map[keychainItemKey][]byte
	writes    []keychainItemKey
	deletes   []keychainItemKey
	readErr   error
	writeErr  error
	deleteErr error
}

func newFakeKeychain() *fakeKeychain {
	return &fakeKeychain{items: map[keychainItemKey][]byte{}}
}

func (k *fakeKeychain) available() bool { return true }

func (k *fakeKeychain) read(_ context.Context, service, account string) ([]byte, bool, error) {
	if k.readErr != nil {
		// An operational failure (locked keychain, denied prompt), which
		// securityKeychain reports for any exit status other than 44.
		return nil, false, k.readErr
	}
	payload, ok := k.items[keychainItemKey{service, account}]
	if !ok || len(bytes.TrimSpace(payload)) == 0 {
		return nil, false, nil
	}
	if securityPrintsVerbatim(payload) {
		return append(append([]byte(nil), payload...), '\n'), true, nil
	}
	return []byte(hex.EncodeToString(payload) + "\n"), true, nil
}

func (k *fakeKeychain) write(_ context.Context, service, account string, payload []byte) error {
	if k.writeErr != nil {
		return k.writeErr
	}
	key := keychainItemKey{service, account}
	k.writes = append(k.writes, key)
	k.items[key] = append([]byte(nil), payload...)
	return nil
}

func (k *fakeKeychain) delete(_ context.Context, service, account string) error {
	key := keychainItemKey{service, account}
	k.deletes = append(k.deletes, key)
	if k.deleteErr != nil {
		return k.deleteErr
	}
	// Deleting a missing item is not an error, matching securityKeychain's
	// tolerance of "could not be found".
	delete(k.items, key)
	return nil
}

func securityPrintsVerbatim(payload []byte) bool {
	for _, b := range payload {
		if b < 0x20 || b > 0x7e {
			return false
		}
	}
	return true
}

func swapKeychain(t *testing.T, kc keychain) {
	t.Helper()
	previous := systemKeychain
	systemKeychain = kc
	t.Cleanup(func() { systemKeychain = previous })
}

func (k *fakeKeychain) deleted(service, account string) bool {
	for _, key := range k.deletes {
		if key.service == service && key.account == account {
			return true
		}
	}
	return false
}

func currentUsername(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatalf("user.Current: %v", err)
	}
	return u.Username
}

// wantKeychainService is the service name Claude Code itself addresses for an
// instance path: the literal "Claude Code-credentials-" prefix plus the first
// 8 hex characters of sha256(instancePath). It is spelled out here from
// stdlib primitives — never via keychainService or keychainHash — because the
// production helpers are the code under test. Expectations computed through
// them are circular: a drifted prefix or hash input keeps every test green
// while reads start returning nothing and deletes orphan the user's real
// keychain items.
func wantKeychainService(instancePath string) string {
	sum := sha256.Sum256([]byte(instancePath))
	return "Claude Code-credentials-" + hex.EncodeToString(sum[:])[:8]
}

// The service string is an external contract shared with Claude Code, which
// reads and rewrites the same keychain entry, so it is pinned against a fully
// hard-coded golden value: no production helper, no recomputed hash. Any
// drift in the prefix, hash algorithm, hash input, or truncation length lands
// here.
func TestKeychainServiceIsTheSharedClaudeCodeLiteral(t *testing.T) {
	const instancePath = "/Users/example/.subrouter/codex/claude/work"
	const want = "Claude Code-credentials-74f8fc66"
	if got := keychainService(instancePath); got != want {
		t.Fatalf("keychainService(%q) = %q, want %q — this string is shared with Claude Code; changing it orphans every existing credential", instancePath, got, want)
	}
	if got := wantKeychainService(instancePath); got != want {
		t.Fatalf("wantKeychainService(%q) = %q, want %q — the test helper drifted from the contract it exists to pin", instancePath, got, want)
	}
}

// A profile removal must delete the keychain item of every instance path the
// profile can live at — the canonical store path and the legacy
// ~/.codex-accounts alias — under the exact service name Claude Code uses.
func TestRemoveProfileDeletesKeychainCredentialAliases(t *testing.T) {
	kc := newFakeKeychain()
	swapKeychain(t, kc)
	home := t.TempDir()
	store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}

	instancePath, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ImportProfileCredential("work", CredentialInfo{
		AccessToken:  "access",
		RefreshToken: "refresh",
	}); err != nil {
		t.Fatal(err)
	}
	kc.deletes = nil

	removed, err := store.RemoveProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("profile was not removed")
	}
	account := currentUsername(t)
	wantServices := []string{
		wantKeychainService(instancePath),
		wantKeychainService(filepath.Join(home, ".codex-accounts", "claude", "work")),
	}
	for _, wantService := range wantServices {
		if !kc.deleted(wantService, account) {
			t.Fatalf("keychain deletes %v missing service %q for account %q", kc.deletes, wantService, account)
		}
	}
}

// A keychain deletion failure must abort the removal and put everything back:
// the registry entry, the instance directory, and the credential.
func TestRemoveProfileRollsBackWhenKeychainDeletionFails(t *testing.T) {
	kc := newFakeKeychain()
	swapKeychain(t, kc)
	home := t.TempDir()
	store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}

	instancePath, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ImportProfileCredential("work", CredentialInfo{
		AccessToken:  "access",
		RefreshToken: "refresh",
	}); err != nil {
		t.Fatal(err)
	}
	kc.deleteErr = errors.New("forced keychain deletion failure")

	removed, removeErr := store.RemoveProfile("work")
	if removeErr == nil {
		t.Fatal("profile removal ignored the keychain deletion failure")
	}
	if removed {
		t.Fatal("failed profile removal was reported as committed")
	}
	if _, ok := store.FindProfile("work"); !ok {
		t.Fatal("keychain failure left the profile absent from the registry")
	}
	credential, err := store.ReadCredential(t.Context(), instancePath)
	if err != nil {
		t.Fatal(err)
	}
	if credential == nil || credential.AccessToken != "access" || credential.RefreshToken != "refresh" {
		t.Fatalf("keychain failure did not restore the staged credential: %+v", credential)
	}
	stagingRoots, err := filepath.Glob(filepath.Join(filepath.Dir(instancePath), ".work.remove-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(stagingRoots) != 0 {
		t.Fatalf("profile rollback left staging roots: %v", stagingRoots)
	}
}

// Temp-instance cleanup after an abandoned login must delete the keychain
// items for both the canonical path and the legacy alias.
func TestCleanupInstanceDeletesKeychainCredentialAliases(t *testing.T) {
	kc := newFakeKeychain()
	swapKeychain(t, kc)
	home := t.TempDir()
	store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}

	instancePath, dir, err := store.CreateTempInstance()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CleanupInstance(dir); err != nil {
		t.Fatal(err)
	}
	account := currentUsername(t)
	wantServices := []string{
		wantKeychainService(instancePath),
		wantKeychainService(filepath.Join(home, ".codex-accounts", "claude", dir)),
	}
	for _, wantService := range wantServices {
		if !kc.deleted(wantService, account) {
			t.Fatalf("keychain deletes %v missing service %q for account %q", kc.deletes, wantService, account)
		}
	}
}

// The credential lock sits beside the instance directory rather than inside it,
// so RemoveAll cannot reach it. Every temp-instance login used to strand one:
// a machine that had run a handful of logins accumulated hundreds of zero-byte
// .credentials.lock files next to its live profiles.
func TestCleanupInstanceRemovesCredentialLock(t *testing.T) {
	swapKeychain(t, newFakeKeychain())
	home := t.TempDir()
	store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}

	instancePath, dir, err := store.CreateTempInstance()
	if err != nil {
		t.Fatal(err)
	}
	lock, err := lockProfileCredential(context.Background(), instancePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	// Resolve while the instance still exists: the lock is named after the
	// symlink-resolved instance path, which cleanup is about to remove.
	lockPath := profileCredentialLockPath(instancePath)
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("credential lock was never created: %v", err)
	}

	if err := store.CleanupInstance(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential lock survived cleanup: %v", err)
	}
}

// A .credentials.json on disk wins over the keychain on write: file-backed
// profiles (server pools, imports) must never grow a diverging keychain copy.
func TestWriteCredentialPrefersCredentialsFileOverKeychain(t *testing.T) {
	kc := newFakeKeychain()
	swapKeychain(t, kc)
	store := Store{Dir: t.TempDir()}
	if err := store.ImportProfileCredential("work", CredentialInfo{
		AccessToken:  "file-access",
		RefreshToken: "file-refresh",
	}); err != nil {
		t.Fatal(err)
	}
	instancePath := store.ClaudeConfigDir("work")
	if err := store.WriteCredential(t.Context(), instancePath, CredentialInfo{
		AccessToken:  "updated-access",
		RefreshToken: "updated-refresh",
	}); err != nil {
		t.Fatal(err)
	}
	if len(kc.writes) != 0 {
		t.Fatalf("file-backed credential write went to the keychain: %v", kc.writes)
	}
	credential, err := store.ReadCredential(t.Context(), instancePath)
	if err != nil {
		t.Fatal(err)
	}
	if credential == nil || credential.AccessToken != "updated-access" {
		t.Fatalf("file credential not updated: %+v", credential)
	}
}

// Without a .credentials.json the write lands in the keychain, and the stored
// payload must be the compact single-printable-line form — the indented form
// is what `security` hex-mangles on the next read (the third brick).
func TestWriteCredentialStoresCompactKeychainPayload(t *testing.T) {
	kc := newFakeKeychain()
	swapKeychain(t, kc)
	store := Store{Dir: t.TempDir()}
	instancePath := filepath.Join(store.Dir, "claude", "work")
	want := CredentialInfo{AccessToken: "keychain-access", RefreshToken: "keychain-refresh"}
	if err := store.WriteCredential(t.Context(), instancePath, want); err != nil {
		t.Fatal(err)
	}
	account := currentUsername(t)
	// The lookup key is computed independently of the production naming
	// helpers, so this doubles as the proof that the store's keychain write
	// addresses the exact item Claude Code shares.
	payload, ok := kc.items[keychainItemKey{wantKeychainService(instancePath), account}]
	if !ok {
		t.Fatalf("keychain write missing; wrote %v", kc.writes)
	}
	if bytes.ContainsRune(payload, '\n') || !securityPrintsVerbatim(payload) {
		t.Fatalf("keychain payload is not one printable line: %q", payload)
	}
	credential, err := store.ReadCredential(t.Context(), instancePath)
	if err != nil {
		t.Fatal(err)
	}
	if credential == nil || !credential.Equal(want) {
		t.Fatalf("keychain credential did not round-trip: %+v", credential)
	}
}

// slowRefreshTransport serves the OAuth token endpoint with a deliberate delay
// and counts refresh POSTs, so a test can prove how many network refreshes a
// set of concurrent callers actually performed.
type slowRefreshTransport struct {
	mu    sync.Mutex
	posts int
	delay time.Duration
}

func (s *slowRefreshTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.posts++
	s.mu.Unlock()
	time.Sleep(s.delay)
	body := `{"access_token":"minted-access","refresh_token":"minted-refresh","expires_in":3600}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
	}, nil
}

// Anthropic rotates refresh tokens on use: two callers that both POST the same
// refresh token race each other's chains, and only one can survive. The store
// used to release the profile lock during the network call, so a boot-time
// sweep and the serving path could each send the same token. The lock is now
// held across the refresh — the loser blocks, re-reads the winner's fresh
// credential, and returns it without a second POST.
func TestConcurrentRefreshesSendExactlyOneTokenRequest(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	profile, err := store.UpsertCredentialProfile("pool", CredentialInfo{
		AccessToken:  "expired-access",
		RefreshToken: "shared-refresh-token",
		ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}

	transport := &slowRefreshTransport{delay: 150 * time.Millisecond}
	client := &http.Client{Transport: transport}

	const racers = 3
	results := make([]accounts.Account, racers)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _, errs[i] = store.RefreshCredentialIfExpired(context.Background(), client, profile)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", i, err)
		}
	}
	if transport.posts != 1 {
		t.Fatalf("refresh POSTs = %d, want exactly 1; extra POSTs reuse a rotated refresh token and can kill the stored chain", transport.posts)
	}
	for i, account := range results {
		if account.Token != "minted-access" {
			t.Fatalf("racer %d got token %q, want the single minted chain", i, account.Token)
		}
	}

	stored, err := store.ReadCredential(context.Background(), store.ClaudeConfigDir(profile.Name))
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.RefreshToken != "minted-refresh" {
		t.Fatalf("stored refresh token = %+v, want the minted chain", stored)
	}
}

// A stale .credentials.json used to shadow the keychain unconditionally, so
// every `sr claude add` re-auth of that profile was a silent no-op and the
// account stayed invalid_grant. ten-alpha lost a day to exactly this.
func TestReadCredentialPrefersTheFresherStore(t *testing.T) {
	payload := func(token string, expiresAt int64) []byte {
		body, err := json.Marshal(map[string]any{
			"claudeAiOauth": map[string]any{
				"accessToken": token, "refreshToken": token + "-r", "expiresAt": expiresAt,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	u, err := user.Current()
	if err != nil {
		t.Skip("no current user")
	}

	setup := func(t *testing.T, fileExpiry, keyExpiry int64) *CredentialInfo {
		t.Helper()
		dir := t.TempDir()
		kc := newFakeKeychain()
		swapKeychain(t, kc)
		if fileExpiry > 0 {
			if err := os.WriteFile(filepath.Join(dir, ".credentials.json"),
				payload("from-file", fileExpiry), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if keyExpiry > 0 {
			if err := kc.write(context.Background(), keychainService(dir), u.Username,
				payload("from-keychain", keyExpiry)); err != nil {
				t.Fatal(err)
			}
		}
		got, err := Store{Dir: dir}.readCredential(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	if got := setup(t, 1000, 2000); got == nil || got.AccessToken != "from-keychain" {
		t.Fatalf("a STALE file must not shadow a fresher keychain credential; got %+v", got)
	}
	if got := setup(t, 3000, 2000); got == nil || got.AccessToken != "from-file" {
		t.Fatalf("a fresher file must still win; got %+v", got)
	}
	if got := setup(t, 1000, 0); got == nil || got.AccessToken != "from-file" {
		t.Fatalf("file-only must still resolve; got %+v", got)
	}
	if got := setup(t, 0, 2000); got == nil || got.AccessToken != "from-keychain" {
		t.Fatalf("keychain-only must still resolve; got %+v", got)
	}
}

// A setup token's recorded expiry is issue+365d, so comparing ExpiresAt across
// credential kinds would let a setup-token file shadow every later OAuth
// re-auth (which Claude Code writes to the keychain) for a year -- the
// ten-alpha failure again, stretched. Across kinds the refreshable credential
// wins; within a kind the later expiry still does.
func TestReadCredentialComparesExpiryOnlyWithinACredentialKind(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Skip("no current user")
	}
	now := time.Now()
	oauth := func(token string, expiresAt time.Time) CredentialInfo {
		return CredentialInfo{AccessToken: token, RefreshToken: token + "-r", ExpiresAt: expiresAt.UnixMilli()}
	}
	setupToken := func(token string, issuedAt time.Time) CredentialInfo {
		return SetupTokenCredential(token, issuedAt)
	}
	read := func(t *testing.T, file, keychain CredentialInfo) *CredentialInfo {
		t.Helper()
		dir := t.TempDir()
		kc := newFakeKeychain()
		swapKeychain(t, kc)
		body, err := credentialPayload(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), body, 0o600); err != nil {
			t.Fatal(err)
		}
		compact, err := compactCredentialPayload(keychain)
		if err != nil {
			t.Fatal(err)
		}
		if err := kc.write(context.Background(), wantKeychainService(dir), u.Username, compact); err != nil {
			t.Fatal(err)
		}
		got, err := Store{Dir: dir}.readCredential(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	if got := read(t, setupToken("setup-file", now.Add(-time.Hour)), oauth("oauth-keychain", now.Add(time.Hour))); got == nil || got.AccessToken != "oauth-keychain" {
		t.Fatalf("a setup-token file must not shadow an OAuth re-auth in the keychain; got %+v", got)
	}
	if got := read(t, oauth("oauth-file", now.Add(time.Hour)), setupToken("setup-keychain", now)); got == nil || got.AccessToken != "oauth-file" {
		t.Fatalf("a refreshable file must beat a setup token in the keychain; got %+v", got)
	}
	if got := read(t, setupToken("older-mint", now.Add(-48*time.Hour)), setupToken("newer-mint", now)); got == nil || got.AccessToken != "newer-mint" {
		t.Fatalf("between two setup tokens the later mint must win; got %+v", got)
	}
	if got := read(t, setupToken("newer-mint", now), setupToken("older-mint", now.Add(-48*time.Hour))); got == nil || got.AccessToken != "newer-mint" {
		t.Fatalf("a newer setup-token file must beat an older keychain one; got %+v", got)
	}
}

// ImportProfileCredential writes the file and must clear the keychain item
// under every spelling of the instance path. When ~/.codex-accounts is a
// symlink to the canonical store, the profile launches under the legacy
// spelling, so that is the keychain item readCredential consults; leaving it
// behind let a stale-but-later-expiring login shadow the credential just
// imported.
func TestImportClearsTheKeychainItemUnderAnAliasedLegacySpelling(t *testing.T) {
	kc := newFakeKeychain()
	swapKeychain(t, kc)
	home := t.TempDir()
	store := Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	if err := os.MkdirAll(store.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(store.Dir, filepath.Join(home, ".codex-accounts")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := store.ImportProfileCredential("work", CredentialInfo{
		AccessToken: "first", RefreshToken: "first-r", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	configDir := store.ClaudeConfigDir("work")
	if !strings.Contains(configDir, ".codex-accounts") {
		t.Fatalf("fixture: profile launches under %s, want the legacy spelling", configDir)
	}
	stale, err := compactCredentialPayload(CredentialInfo{
		AccessToken: "stale", RefreshToken: "stale-r", ExpiresAt: time.Now().Add(24 * time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := kc.write(context.Background(), wantKeychainService(configDir), currentUsername(t), stale); err != nil {
		t.Fatal(err)
	}

	if err := store.ImportProfileCredential("work", CredentialInfo{
		AccessToken: "imported", RefreshToken: "imported-r", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	credential, err := store.ReadCredential(t.Context(), configDir)
	if err != nil {
		t.Fatal(err)
	}
	if credential == nil || credential.AccessToken != "imported" {
		t.Fatalf("a keychain item under the legacy spelling shadowed the import: %+v", credential)
	}
}

// Removal fingerprints the credential it is about to delete. A vault that
// cannot be asked is "unknown", never "absent", or a locked keychain would let
// removal commit against a credential it never saw. Upstream pins this with a
// darwin-only PATH shim; the seam runs it everywhere.
func TestProfileRemovalSnapshotDistinguishesKeychainFailureFromAbsence(t *testing.T) {
	kc := newFakeKeychain()
	swapKeychain(t, kc)
	store := Store{Dir: filepath.Join(t.TempDir(), ".subrouter", "codex")}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}

	snapshot, found, err := store.SnapshotProfileRemovalContext(t.Context(), "work")
	if err != nil || !found || snapshot.CredentialVersion == "" {
		t.Fatalf("absent keychain item: found %v snapshot %+v err %v", found, snapshot, err)
	}

	kc.readErr = errors.New("keychain is locked")
	_, found, err = store.SnapshotProfileRemovalContext(t.Context(), "work")
	if err == nil || found || !strings.Contains(err.Error(), "read Claude credential from keychain") {
		t.Fatalf("operational keychain failure: found %v err %v", found, err)
	}
}

// The refresh lock (a second sibling of the instance directory, held across
// the OAuth round trip) is stranded by RemoveAll exactly like the credential
// lock was.
func TestCleanupInstanceRemovesRefreshLock(t *testing.T) {
	swapKeychain(t, newFakeKeychain())
	store := Store{Dir: t.TempDir()}
	instancePath, dir, err := store.CreateTempInstance()
	if err != nil {
		t.Fatal(err)
	}
	lock, err := lockProfileRefresh(context.Background(), instancePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	lockPath := profileRefreshLockPath(instancePath)
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("refresh lock was never created at %s: %v", lockPath, err)
	}
	if err := store.CleanupInstance(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refresh lock survived cleanup: %v", err)
	}
}
