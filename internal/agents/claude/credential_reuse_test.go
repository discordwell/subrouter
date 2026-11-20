package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// A keychain answer moments old, whose token needs no refresh, is served
// without the profile's locks and without `security` (keychainAnswerReuse).
// Every turn and every sweep used to take both locks and run a read to learn
// that the token it had was still good; on a loaded Mac the read runs to its
// five-second clock and the locks queue the pool's 5 s score sweep behind it
// -- 240 sweep refreshes cut waiting for a lock in five hours on 2026-09-28.
//
// Keychain safety as everywhere in this package: the vault seam is swapped
// and PATH is empty (keychainOnlyProfile).

// countingKeychain counts the reads that reach the vault, cut or answered.
type countingKeychain struct {
	*cutKeychain
	countMu sync.Mutex
	count   int
}

func (k *countingKeychain) read(ctx context.Context, service, account string) ([]byte, bool, error) {
	k.countMu.Lock()
	k.count++
	k.countMu.Unlock()
	return k.cutKeychain.read(ctx, service, account)
}

func (k *countingKeychain) reads() int {
	k.countMu.Lock()
	defer k.countMu.Unlock()
	return k.count
}

func newCountingKeychain() *countingKeychain {
	return &countingKeychain{cutKeychain: &cutKeychain{fakeKeychain: newFakeKeychain(), failFor: map[string]bool{},
		cut: errors.New("exit status 36")}}
}

// freshKeychainAnswers empties the process-wide answer table for the test and
// again after it, as freshCredentialSilence does for the streaks.
func freshKeychainAnswers(t *testing.T) {
	t.Helper()
	reset := func() {
		keychainAnswers.Lock()
		keychainAnswers.entries = map[string]keychainAnswer{}
		keychainAnswers.rotating = map[string]int{}
		keychainAnswers.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

// holdProfileLocks takes the profile's refresh and credential locks, as a
// turn whose `security` read is running to its clock holds them, until the
// test ends.
func holdProfileLocks(t *testing.T, configDir string) {
	t.Helper()
	refresh, err := lockProfileRefresh(context.Background(), configDir)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := lockProfileCredential(context.Background(), configDir)
	if err != nil {
		_ = refresh.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = credential.Close()
		_ = refresh.Close()
	})
}

// The sweep's shape: the pool budget is 5 s, and a profile's locks are held by
// a read that will run out its clock. A caller that just had the keychain's
// answer gets it back at once -- no lock wait for the budget to cut, no
// `security` -- instead of `wait for the Claude credential lock: context
// deadline exceeded` and no fresh score.
func TestAKeychainAnswerIsServedWithoutTheLocksOrSecurity(t *testing.T) {
	kc := newCountingKeychain()
	swapKeychain(t, kc)
	store, profile := keychainOnlyProfile(t, kc, "work",
		credentialJSON(t, "live-access", "live-refresh", time.Now().Add(time.Hour)))
	configDir := store.ClaudeConfigDir(profile.Name)

	if account, _, err := store.RefreshCredentialIfExpired(context.Background(), nil, profile); err != nil || account.Token != "live-access" {
		t.Fatalf("first refresh: %q %v", account.Token, err)
	}
	if kc.reads() != 1 {
		t.Fatalf("calibration: first refresh read the keychain %d times, want 1", kc.reads())
	}
	holdProfileLocks(t, configDir)
	budget, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	account, details, didRefresh, err := store.RefreshCredentialDetailsIfExpired(budget, nil, profile)
	if err != nil || didRefresh || account.Token != "live-access" || details == nil || details.RefreshToken != "live-refresh" {
		t.Fatalf("refresh inside a 100 ms budget with the locks held: %q %v; want the answer the keychain just gave", account.Token, err)
	}
	if kc.reads() != 1 {
		t.Fatalf("the keychain was read %d times, want 1: a fresh answer is not asked again", kc.reads())
	}
}

// Reuse must never stand in for a read the caller needs, and never blur the
// silent-store streak: every case here goes back to the keychain.
func TestAKeychainAnswerIsReadAgainWhenItCannotBeTrusted(t *testing.T) {
	prime := func(t *testing.T, kc *countingKeychain, expiresAt time.Time) (Store, Profile, string) {
		t.Helper()
		store, profile := keychainOnlyProfile(t, kc, "work", credentialJSON(t, "live-access", "live-refresh", expiresAt))
		if _, err := store.ReadCredential(context.Background(), store.ClaudeConfigDir(profile.Name)); err != nil {
			t.Fatal(err)
		}
		return store, profile, store.ClaudeConfigDir(profile.Name)
	}

	t.Run("past its window", func(t *testing.T) {
		kc := newCountingKeychain()
		swapKeychain(t, kc)
		store, profile, _ := prime(t, kc, time.Now().Add(time.Hour))
		previous := keychainAnswerReuse
		keychainAnswerReuse = 0
		t.Cleanup(func() { keychainAnswerReuse = previous })
		if _, _, err := store.RefreshCredentialIfExpired(context.Background(), nil, profile); err != nil {
			t.Fatal(err)
		}
		if kc.reads() != 2 {
			t.Fatalf("reads = %d, want 2: an answer past its window is asked again", kc.reads())
		}
	})

	t.Run("after a read that got no answer", func(t *testing.T) {
		kc := newCountingKeychain()
		swapKeychain(t, kc)
		store, profile, configDir := prime(t, kc, time.Now().Add(time.Hour))
		kc.failEveryRead()
		if _, err := store.ReadCredential(context.Background(), configDir); !errors.Is(err, ErrCredentialUnchecked) {
			t.Fatalf("calibration: silent read %v", err)
		}
		// The next caller reads -- and gets the silence, exactly as it did
		// before reuse existed -- instead of the answer from before it.
		_, _, err := store.RefreshCredentialIfExpired(context.Background(), nil, profile)
		if !errors.Is(err, ErrCredentialUnchecked) || kc.reads() != 3 {
			t.Fatalf("refresh after a silent read: %v after %d reads, want the keychain asked again (3 reads, unchecked)", err, kc.reads())
		}
	})

	t.Run("after a read that answered no credential", func(t *testing.T) {
		kc := newCountingKeychain()
		swapKeychain(t, kc)
		store, profile, configDir := prime(t, kc, time.Now().Add(time.Hour))
		delete(kc.items, keychainItemKey{wantKeychainService(configDir), currentUsername(t)})
		if credential, err := store.ReadCredential(context.Background(), configDir); credential != nil || err != nil {
			t.Fatalf("calibration: read of a deleted item: %v %v", credential != nil, err)
		}
		_, _, err := store.RefreshCredentialIfExpired(context.Background(), nil, profile)
		if err == nil || !strings.Contains(err.Error(), "has no access token") {
			t.Fatalf("refresh after the item was answered gone: %v, want has no access token", err)
		}
	})

	t.Run("a token inside the refresh window", func(t *testing.T) {
		kc := newCountingKeychain()
		swapKeychain(t, kc)
		store, profile, _ := prime(t, kc, time.Now().Add(30*time.Second))
		account, didRefresh, err := store.RefreshCredentialIfExpired(context.Background(), rotatingClient(nil), profile)
		if err != nil || !didRefresh || account.Token != "minted-access" {
			t.Fatalf("refresh of a token 30 s from expiry: %q %v %v, want the locked path and a rotation", account.Token, didRefresh, err)
		}
		// The rotation this process persisted is the answer now.
		before := kc.reads()
		if account, _, err := store.RefreshCredentialIfExpired(context.Background(), nil, profile); err != nil || account.Token != "minted-access" {
			t.Fatalf("refresh after the rotation: %q %v", account.Token, err)
		}
		if kc.reads() != before {
			t.Fatalf("reads %d -> %d: the credential this process just wrote was read back", before, kc.reads())
		}
	})

	t.Run("a forced refresh, and a turn during it", func(t *testing.T) {
		kc := newCountingKeychain()
		swapKeychain(t, kc)
		store, profile, _ := prime(t, kc, time.Now().Add(time.Hour))
		// A turn arriving while the provider rotates: the answer it would
		// reuse holds the token being replaced. It queues on the refresh lock
		// the rotation holds (its 100 ms budget cuts that wait here) instead
		// of being handed the old token.
		var during string
		var duringErr error
		account, didRefresh, err := store.ForceRefreshCredential(context.Background(), rotatingClient(func() {
			budget, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			var turn accounts.Account
			turn, _, duringErr = store.RefreshCredentialIfExpired(budget, nil, profile)
			during = turn.Token
		}), profile)
		if err != nil || !didRefresh || account.Token != "minted-access" {
			t.Fatalf("forced refresh: %q %v %v, want a rotation", account.Token, didRefresh, err)
		}
		if during == "live-access" || duringErr == nil {
			t.Fatalf("a turn during the rotation got %q (%v); want it kept off the token being replaced", during, duringErr)
		}
		if after, _, err := store.RefreshCredentialIfExpired(context.Background(), nil, profile); err != nil || after.Token != "minted-access" {
			t.Fatalf("a turn after the rotation: %q %v, want the rotation", after.Token, err)
		}
	})

	t.Run("a profile with a credentials file", func(t *testing.T) {
		kc := newCountingKeychain()
		swapKeychain(t, kc)
		store, profile, configDir := prime(t, kc, time.Now().Add(time.Hour))
		path := filepath.Join(configDir, ".credentials.json")
		if err := os.WriteFile(path, credentialJSON(t, "file-access", "file-refresh", time.Now().Add(2*time.Hour)), 0o600); err != nil {
			t.Fatal(err)
		}
		if account, _, err := store.RefreshCredentialIfExpired(context.Background(), nil, profile); err != nil || account.Token != "file-access" {
			t.Fatalf("refresh after a credentials file appeared: %q %v, want the file read", account.Token, err)
		}
		if err := os.WriteFile(path, credentialJSON(t, "newer-access", "newer-refresh", time.Now().Add(3*time.Hour)), 0o600); err != nil {
			t.Fatal(err)
		}
		if account, _, err := store.RefreshCredentialIfExpired(context.Background(), nil, profile); err != nil || account.Token != "newer-access" {
			t.Fatalf("second refresh of a file-backed profile: %q %v, want the file read every time", account.Token, err)
		}
	})

	t.Run("a rotation rescued by another process", func(t *testing.T) {
		kc := newCountingKeychain()
		swapKeychain(t, kc)
		store, profile, configDir := prime(t, kc, time.Now().Add(time.Hour))
		if _, _, err := store.RefreshCredentialIfExpired(context.Background(), nil, profile); err != nil {
			t.Fatal(err)
		}
		// A CLI's forced refresh rotated live-* and could only rescue it: this
		// process still remembers live-access, whose refresh token is spent.
		body, err := rescuePayload(
			CredentialInfo{AccessToken: "live-access", RefreshToken: "live-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()},
			CredentialInfo{AccessToken: "rescued-access", RefreshToken: "rescued-refresh", ExpiresAt: time.Now().Add(2 * time.Hour).UnixMilli()})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(configDir, rotationRescueFile), body, 0o600); err != nil {
			t.Fatal(err)
		}
		if account, _, err := store.RefreshCredentialIfExpired(context.Background(), nil, profile); err != nil || account.Token != "rescued-access" {
			t.Fatalf("refresh beside a rescue: %q %v; want the rescue adopted, not the remembered token", account.Token, err)
		}
	})

	t.Run("a rotation kept in the credentials file", func(t *testing.T) {
		fastKeychainWriteRetries(t)
		setCredentialPersistTimeout(t, 300*time.Millisecond)
		inner := newCountingKeychain()
		kc := &flakyWriteKeychain{fakeKeychain: inner.fakeKeychain, err: hungWrite}
		swapKeychain(t, kc)
		store, profile := keychainOnlyProfile(t, kc, "work",
			credentialJSON(t, "expired-access", "about-to-be-spent", time.Now().Add(-time.Hour)))
		kc.failNext(-1)
		if _, _, err := store.RefreshCredentialIfExpired(context.Background(), rotatingClient(nil), profile); err != nil {
			t.Fatal(err)
		}
		keychainAnswers.Lock()
		_, remembered := keychainAnswers.entries[filepath.Clean(store.ClaudeConfigDir(profile.Name))]
		keychainAnswers.Unlock()
		if remembered {
			t.Fatal("a profile whose rotation went to its credentials file is still served from a keychain answer")
		}
	})
}

// The daemon's store has a SharedStateDir, and ClaudeConfigDir then prepares
// the shared history under the one shared-state lock every profile shares and,
// holding it, the profile's credential lock -- both with no deadline. The fast
// path went through ClaudeConfigDir, so in production it waited on exactly the
// locks it exists to skip, and every profile waited behind whichever one had a
// slow `security` read in flight. Measured before the fix: this test's
// refresh blocked until the locks were released.
func TestAKeychainAnswerSkipsTheLocksInTheDaemonsStoreShape(t *testing.T) {
	kc := newCountingKeychain()
	swapKeychain(t, kc)
	store, profile := keychainOnlyProfile(t, kc, "work",
		credentialJSON(t, "live-access", "live-refresh", time.Now().Add(time.Hour)))
	store.SharedStateDir = filepath.Join(t.TempDir(), "shared")
	if _, err := store.CreateProfile("other"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.RefreshCredentialIfExpired(context.Background(), nil, profile); err != nil {
		t.Fatal(err)
	}
	// work's own locks are held by a read running to its clock, and a
	// ClaudeConfigDir for another profile is queued behind other's held
	// credential lock while it holds the shared-state lock.
	queued := make(chan struct{})
	t.Cleanup(func() { <-queued }) // after the locks are released below
	holdProfileLocks(t, store.claudeInstancePath(profile.Name))
	holdProfileLocks(t, store.claudeInstancePath("other"))
	go func() {
		defer close(queued)
		_ = store.ClaudeConfigDir("other")
	}()
	time.Sleep(50 * time.Millisecond)

	type result struct {
		token   string
		err     error
		details *CredentialInfo
	}
	for name, call := range map[string]func(ctx context.Context) result{
		"a turn or sweep refresh": func(ctx context.Context) result {
			account, _, err := store.RefreshCredentialIfExpired(ctx, nil, profile)
			return result{token: account.Token, err: err}
		},
		"the sweep's pre-read": func(ctx context.Context) result {
			credential, err := store.ProfileCredential(ctx, profile)
			return result{details: credential, err: err}
		},
	} {
		t.Run(name, func(t *testing.T) {
			done := make(chan result, 1)
			go func() {
				budget, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				done <- call(budget)
			}()
			select {
			case got := <-done:
				if got.err != nil || (got.token != "live-access" && (got.details == nil || got.details.AccessToken != "live-access")) {
					t.Fatalf("got %q / %+v, %v; want the answer from a moment ago", got.token, got.details, got.err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("blocked on the profile's locks through ClaudeConfigDir's shared-state preparation")
			}
			if kc.reads() != 1 {
				t.Fatalf("the keychain was read %d times, want 1", kc.reads())
			}
		})
	}
}

// ProfileCredential is ReadCredential when there is no answer to reuse.
func TestProfileCredentialReadsWhenThereIsNoAnswer(t *testing.T) {
	kc := newCountingKeychain()
	swapKeychain(t, kc)
	store, profile := keychainOnlyProfile(t, kc, "work",
		credentialJSON(t, "live-access", "live-refresh", time.Now().Add(time.Hour)))
	credential, err := store.ProfileCredential(context.Background(), profile)
	if err != nil || credential == nil || credential.AccessToken != "live-access" || kc.reads() != 1 {
		t.Fatalf("first ProfileCredential: %+v %v after %d reads; want one read", credential, err, kc.reads())
	}
	kc.failEveryRead()
	if _, err := store.ProfileCredential(context.Background(), profile); !errors.Is(err, ErrCredentialUnchecked) || kc.reads() != 2 {
		t.Fatalf("ProfileCredential with an unstamped answer: %v after %d reads; want the silent keychain read", err, kc.reads())
	}
}

// An answer is served only to the profile it was read for. A profile removed
// and added again under the same name within the window -- by `sr claude rm`
// and `sr claude add` in another process, whose forgetting this process never
// sees -- is a new profile with a new login, and gets it, not the old one's.
func TestAKeychainAnswerIsServedOnlyToTheProfileItWasReadFor(t *testing.T) {
	kc := newCountingKeychain()
	swapKeychain(t, kc)
	store, profile := keychainOnlyProfile(t, kc, "work",
		credentialJSON(t, "old-login-access", "old-login-refresh", time.Now().Add(time.Hour)))
	configDir := store.claudeInstancePath(profile.Name)
	if account, _, err := store.RefreshCredentialIfExpired(context.Background(), nil, profile); err != nil || account.Token != "old-login-access" {
		t.Fatalf("first refresh: %q %v", account.Token, err)
	}
	// The other process: the profile re-registered, its item holding the new
	// login. The instance path is the same (the same name).
	data := store.readProfiles()
	readded := data.Profiles[profile.Name]
	readded.CreatedAt = time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	data.Profiles[profile.Name] = readded
	if err := store.writeProfiles(data); err != nil {
		t.Fatal(err)
	}
	if err := kc.fakeKeychain.write(context.Background(), wantKeychainService(configDir), currentUsername(t),
		credentialJSON(t, "new-login-access", "new-login-refresh", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	account, _, err := store.RefreshCredentialIfExpired(context.Background(), nil, readded)
	if err != nil || account.Token != "new-login-access" {
		t.Fatalf("refresh of the re-added profile: %q %v; want the new login, not the removed profile's answer", account.Token, err)
	}
}

// A lock that could not be taken is a read that got no answer, and forgets the
// answer like one: reloads saw the lock failure while turns were still served
// the remembered token.
func TestACredentialLockThatCannotBeTakenForgetsTheAnswer(t *testing.T) {
	kc := newCountingKeychain()
	swapKeychain(t, kc)
	store, profile := keychainOnlyProfile(t, kc, "work",
		credentialJSON(t, "live-access", "live-refresh", time.Now().Add(time.Hour)))
	configDir := store.claudeInstancePath(profile.Name)
	if _, _, err := store.RefreshCredentialIfExpired(context.Background(), nil, profile); err != nil {
		t.Fatal(err)
	}
	// A lock file the daemon cannot open (a directory stands in for a
	// root-owned file left by `sudo sr`).
	lockPath := profileCredentialLockPath(configDir)
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.Mkdir(lockPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadCredential(context.Background(), configDir); err == nil {
		t.Fatal("calibration: a read whose lock cannot be opened succeeded")
	}
	account, _, err := store.RefreshCredentialIfExpired(context.Background(), nil, profile)
	if err == nil || account.Token == "live-access" {
		t.Fatalf("turn after the lock failed: %q %v; want the lock failure, not the remembered token", account.Token, err)
	}
}

// hookedReadKeychain runs onRead (once) inside the next keychain read.
type hookedReadKeychain struct {
	*countingKeychain
	mu     sync.Mutex
	onRead func()
}

func (k *hookedReadKeychain) read(ctx context.Context, service, account string) ([]byte, bool, error) {
	k.mu.Lock()
	hook := k.onRead
	k.onRead = nil
	k.mu.Unlock()
	if hook != nil {
		hook()
	}
	return k.countingKeychain.read(ctx, service, account)
}

// A forced refresh is most often a token the provider just refused. A turn
// arriving while the forced refresh is still READING, before its exchange,
// used to be handed that token from the remembered answer.
func TestATurnDuringAForcedRefreshsReadIsNotHandedTheOldToken(t *testing.T) {
	kc := &hookedReadKeychain{countingKeychain: newCountingKeychain()}
	swapKeychain(t, kc)
	store, profile := keychainOnlyProfile(t, kc, "work",
		credentialJSON(t, "refused-access", "live-refresh", time.Now().Add(time.Hour)))
	if _, _, err := store.RefreshCredentialIfExpired(context.Background(), nil, profile); err != nil {
		t.Fatal(err)
	}
	var during string
	var duringErr error
	kc.mu.Lock()
	kc.onRead = func() {
		budget, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		var turn accounts.Account
		turn, _, duringErr = store.RefreshCredentialIfExpired(budget, nil, profile)
		during = turn.Token
	}
	kc.mu.Unlock()
	if _, _, err := store.ForceRefreshCredential(context.Background(), rotatingClient(nil), profile); err != nil {
		t.Fatal(err)
	}
	if during == "refused-access" || duringErr == nil {
		t.Fatalf("a turn during the forced refresh's read got %q (%v); want it kept off the refused token", during, duringErr)
	}
}
