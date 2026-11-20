package claude

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Anthropic rotates the refresh token on use: once the exchange answers, the
// token the profile held is spent and the only live copy of its successor is
// the one the persist is writing. On 2026-09-27 17:41-17:44 four of those
// writes -- one `security add-generic-password` on its own five-second clock
// -- were killed on that clock under load, and two of the accounts
// (fifteen-beta, nine-alpha) went invalid_grant minutes later. The
// write now retries the idempotent -U upsert on the persist clock, and a
// rotation the keychain still will not take is written to the profile's
// credentials file, never dropped.
//
// Keychain safety as everywhere in this package: the store tests swap the
// vault seam and pin PATH empty (keychainOnlyProfile), and the tests that
// drive securityKeychain put a shim alone on PATH (securityShim).

// flakyWriteKeychain is a fakeKeychain whose writes fail with err while
// failures is not zero (failNext). It counts every attempt, landed or not.
type flakyWriteKeychain struct {
	*fakeKeychain
	mu       sync.Mutex
	failures int
	err      error
	attempts int
}

func (k *flakyWriteKeychain) write(ctx context.Context, service, account string, payload []byte) error {
	k.mu.Lock()
	k.attempts++
	fail := k.failures != 0
	if k.failures > 0 {
		k.failures--
	}
	k.mu.Unlock()
	if fail {
		return k.err
	}
	return k.fakeKeychain.write(ctx, service, account, payload)
}

// failNext refuses the next n writes (n < 0: every write from now on) and
// restarts the attempt count, so the item the test seeded is not counted.
func (k *flakyWriteKeychain) failNext(n int) {
	k.mu.Lock()
	k.failures, k.attempts = n, 0
	k.mu.Unlock()
}

func (k *flakyWriteKeychain) attempted() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.attempts
}

// hungWrite is exactly what securityKeychain.write returns for an upsert its
// own clock killed with the caller still waiting.
var hungWrite = fmt.Errorf("%w within %s (signal: killed)", errSecurityWriteHung, 5*time.Second)

func fastKeychainWriteRetries(t *testing.T) {
	t.Helper()
	previous, previousMax := keychainWriteRetryBackoff, keychainWriteRetryBackoffMax
	keychainWriteRetryBackoff, keychainWriteRetryBackoffMax = time.Millisecond, time.Millisecond
	t.Cleanup(func() { keychainWriteRetryBackoff, keychainWriteRetryBackoffMax = previous, previousMax })
}

func setCredentialPersistTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	previous := credentialPersistTimeout
	credentialPersistTimeout = d
	t.Cleanup(func() { credentialPersistTimeout = previous })
}

func setSecurityWriteTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	previous := securityWriteTimeout
	securityWriteTimeout = d
	t.Cleanup(func() { securityWriteTimeout = previous })
}

// capturedLog swaps the process-wide slog default for a text handler writing
// to a buffer, and runs onRecord (when set) for every record, on the logging
// goroutine, before the call that logged returns.
type capturedLog struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	onRecord func(slog.Record)
}

func (c *capturedLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *capturedLog) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

type capturedLogHandler struct {
	slog.Handler
	log *capturedLog
}

func (h capturedLogHandler) Handle(ctx context.Context, record slog.Record) error {
	err := h.Handler.Handle(ctx, record)
	if h.log.onRecord != nil {
		h.log.onRecord(record)
	}
	return err
}

func captureLog(t *testing.T) *capturedLog {
	t.Helper()
	captured := &capturedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(capturedLogHandler{Handler: slog.NewTextHandler(captured, nil), log: captured}))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return captured
}

// rotatingClient answers the token endpoint with a rotation, the way
// slowRefreshTransport does, and runs duringExchange (when set) while the
// provider "rotates" -- after the pre-read, before the persist.
func rotatingClient(duringExchange func()) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if duringExchange != nil {
			duringExchange()
		}
		return (&slowRefreshTransport{}).RoundTrip(req)
	})}
}

// The 9/27 shape at the store: the keychain write is killed on its own clock,
// twice, and lands on the third try, inside the persist clock. The refresh
// succeeds and the keychain holds the rotated refresh token. Before the fix the
// first kill was the persist's answer: the refresh failed and the keychain
// kept the spent token.
func TestAKilledKeychainWriteOfARotationIsRetried(t *testing.T) {
	fastKeychainWriteRetries(t)
	kc := &flakyWriteKeychain{fakeKeychain: newFakeKeychain(), err: hungWrite}
	swapKeychain(t, kc)
	store, profile := keychainOnlyProfile(t, kc, "work",
		credentialJSON(t, "expired-access", "about-to-be-spent", time.Now().Add(-time.Hour)))
	kc.failNext(2)
	configDir := store.ClaudeConfigDir(profile.Name)
	service := wantKeychainService(configDir)
	log := captureLog(t)

	account, didRefresh, err := store.RefreshCredentialIfExpired(context.Background(), rotatingClient(nil), profile)
	if err != nil {
		t.Fatalf("refresh whose keychain write was killed twice: %v (the rotation was lost)", err)
	}
	if !didRefresh || account.Token != "minted-access" {
		t.Fatalf("account token %q didRefresh %v, want the minted credential", account.Token, didRefresh)
	}
	stored := kc.items[keychainItemKey{service, currentUsername(t)}]
	if !strings.Contains(string(stored), "minted-refresh") {
		t.Fatalf("keychain holds %q, want the rotated refresh token", stored)
	}
	if got := kc.attempted(); got != 3 {
		t.Fatalf("keychain write attempts = %d, want 3 (two killed, one landed)", got)
	}
	if _, err := os.Stat(filepath.Join(configDir, ".credentials.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a write that landed in the keychain also left a credentials file (%v): the profile must stay keychain-backed", err)
	}
	for _, token := range []string{"minted-access", "minted-refresh", "about-to-be-spent"} {
		if strings.Contains(log.String(), token) {
			t.Fatalf("the log carries a token (%s):\n%s", token, log.String())
		}
	}
	if !strings.Contains(log.String(), "landed on a retry") {
		t.Fatalf("a write that needed retries was not logged:\n%s", log.String())
	}
}

// A keychain that takes no write for the whole persist clock -- the loaded
// Mac that kills every `security`, or one that answers "locked" throughout --
// no longer costs the rotation: it is written to the profile's credentials
// file, which every later read prefers as the fresher credential, and one
// ERROR line names the profile. The refresh succeeds, and the next refresh of
// the profile presents the rotated token, not the spent one.
func TestARotationTheKeychainWillNotTakeIsKeptInTheCredentialsFile(t *testing.T) {
	for name, failure := range map[string]error{
		"killed on its own clock": hungWrite,
		"keychain locked":         errors.New("exit status 36"),
	} {
		t.Run(name, func(t *testing.T) {
			fastKeychainWriteRetries(t)
			setCredentialPersistTimeout(t, 300*time.Millisecond)
			kc := &flakyWriteKeychain{fakeKeychain: newFakeKeychain(), err: failure}
			swapKeychain(t, kc)
			store, profile := keychainOnlyProfile(t, kc, "work",
				credentialJSON(t, "expired-access", "about-to-be-spent", time.Now().Add(-time.Hour)))
			kc.failNext(-1)
			configDir := store.ClaudeConfigDir(profile.Name)
			log := captureLog(t)

			account, didRefresh, err := store.RefreshCredentialIfExpired(context.Background(), rotatingClient(nil), profile)
			if err != nil {
				t.Fatalf("refresh with a keychain that takes no write: %v (the rotation was dropped)", err)
			}
			if !didRefresh || account.Token != "minted-access" {
				t.Fatalf("account token %q didRefresh %v, want the minted credential", account.Token, didRefresh)
			}
			if kc.attempted() < 2 {
				t.Fatalf("keychain write attempts = %d, want it retried on the persist clock", kc.attempted())
			}
			file, err := os.ReadFile(filepath.Join(configDir, ".credentials.json"))
			if err != nil || !strings.Contains(string(file), "minted-refresh") {
				t.Fatalf("credentials file %q (%v), want the rotated refresh token", file, err)
			}
			if info, err := os.Stat(filepath.Join(configDir, ".credentials.json")); err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("credentials file mode %v (%v), want 0600", info.Mode().Perm(), err)
			}
			current, err := store.ReadCredential(context.Background(), configDir)
			if err != nil || current == nil || current.RefreshToken != "minted-refresh" {
				t.Fatalf("read after the rescue: %+v %v, want the rotated credential over the keychain's spent one", current, err)
			}
			text := log.String()
			if !strings.Contains(text, "level=ERROR") || !strings.Contains(text, "profile=work") || !strings.Contains(text, ".credentials.json") {
				t.Fatalf("no loud line naming the profile and the file:\n%s", text)
			}
			for _, token := range []string{"minted-access", "minted-refresh", "about-to-be-spent"} {
				if strings.Contains(text, token) {
					t.Fatalf("the log carries a token (%s):\n%s", token, text)
				}
			}
		})
	}
}

// The other half of the old KNOWN-ISSUES entry: "the same loss follows a
// persist whose credential lock cannot be taken after the exchange". A persist
// that never got the lock read and wrote nothing, so it asks again on a fresh
// clock; a lock that is still held after that is something sitting on it, and
// the rotation goes, without the lock, to the rescue file beside the
// credentials, where it clobbers nothing. The lock is released (or not) from
// inside the persist's own log line, so which attempt gets it is decided by
// construction, not by a race with the clock.
func TestARotationWhoseCredentialLockIsHeldIsNotDropped(t *testing.T) {
	setup := func(t *testing.T) (*fakeKeychain, Store, Profile, string, *capturedLog, func() func()) {
		t.Helper()
		setCredentialPersistTimeout(t, 50*time.Millisecond)
		kc := newFakeKeychain()
		swapKeychain(t, kc)
		store, profile := keychainOnlyProfile(t, kc, "work",
			credentialJSON(t, "expired-access", "about-to-be-spent", time.Now().Add(-time.Hour)))
		configDir := store.ClaudeConfigDir(profile.Name)
		log := captureLog(t)
		// hold takes the profile's credential lock during the exchange -- the
		// refresh holds only its refresh lock then -- and returns the release.
		hold := func() func() {
			lock, err := lockProfileCredential(context.Background(), configDir)
			if err != nil {
				t.Fatalf("take the credential lock: %v", err)
			}
			var once sync.Once
			release := func() { once.Do(func() { _ = lock.Close() }) }
			t.Cleanup(release)
			return release
		}
		return kc, store, profile, configDir, log, hold
	}

	t.Run("free on the second clock", func(t *testing.T) {
		kc, store, profile, configDir, log, hold := setup(t)
		var release func()
		log.onRecord = func(record slog.Record) {
			if strings.Contains(record.Message, "lock not taken to persist") && release != nil {
				release()
			}
		}
		account, didRefresh, err := store.RefreshCredentialIfExpired(context.Background(),
			rotatingClient(func() { release = hold() }), profile)
		if err != nil || !didRefresh || account.Token != "minted-access" {
			t.Fatalf("refresh whose first persist clock found the lock held: %q %v %v, want the minted credential", account.Token, didRefresh, err)
		}
		stored := kc.items[keychainItemKey{wantKeychainService(configDir), currentUsername(t)}]
		if !strings.Contains(string(stored), "minted-refresh") {
			t.Fatalf("keychain holds %q, want the rotated refresh token", stored)
		}
		if _, err := os.Stat(filepath.Join(configDir, rotationRescueFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a rescue file was written (%v) though the second clock got the lock", err)
		}
	})

	t.Run("held throughout", func(t *testing.T) {
		kc, store, profile, configDir, log, hold := setup(t)
		_, _, err := store.RefreshCredentialIfExpired(context.Background(), rotatingClient(func() { hold() }), profile)
		if err == nil {
			t.Fatal("refresh whose credential lock was never free reported success")
		}
		rescued, readErr := os.ReadFile(filepath.Join(configDir, rotationRescueFile))
		if readErr != nil || !strings.Contains(string(rescued), "minted-refresh") {
			t.Fatalf("rescue file %q (%v), want the rotated refresh token kept", rescued, readErr)
		}
		if info, statErr := os.Stat(filepath.Join(configDir, rotationRescueFile)); statErr != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("rescue file mode %v (%v), want 0600", info.Mode().Perm(), statErr)
		}
		if _, statErr := os.Stat(filepath.Join(configDir, ".credentials.json")); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("the live credentials file was written without the lock (%v)", statErr)
		}
		stored := kc.items[keychainItemKey{wantKeychainService(configDir), currentUsername(t)}]
		if !strings.Contains(string(stored), "about-to-be-spent") {
			t.Fatalf("keychain holds %q; nothing may write the live store without its lock", stored)
		}
		text := log.String()
		if !strings.Contains(text, "level=ERROR") || !strings.Contains(text, "profile=work") || !strings.Contains(text, rotationRescueFile) {
			t.Fatalf("no loud line naming the profile and the rescue file:\n%s", text)
		}
		for _, token := range []string{"minted-access", "minted-refresh", "about-to-be-spent"} {
			if strings.Contains(text, token) {
				t.Fatalf("the log carries a token (%s):\n%s", token, text)
			}
		}
	})
}

// securityKeychain.write tells its clocks apart the way read does: a write
// its OWN clock killed with the caller still waiting is errSecurityWriteHung,
// with no context error in it or its text; one the CALLER's clock cut says
// deadline; a refusal is the refusal. Each shim write has one possible winner
// (neverAnswers, or a clock longer than any test run).
func TestSecurityKeychainWriteTellsTheCallersCutFromAHang(t *testing.T) {
	never := neverAnswers(t)
	payload := []byte(`{"claudeAiOauth":{"refreshToken":"never-in-an-error"}}`)
	t.Run("its own clock, caller still waiting", func(t *testing.T) {
		securityShim(t, never)
		setSecurityWriteTimeout(t, 100*time.Millisecond)
		err := securityKeychain{}.write(context.Background(), "svc", "acct", payload)
		if !errors.Is(err, errSecurityWriteHung) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			t.Fatalf("write that ran out its own clock: %v, want errSecurityWriteHung and no context error", err)
		}
		for _, text := range []string{context.DeadlineExceeded.Error(), context.Canceled.Error(), "never-in-an-error"} {
			if strings.Contains(err.Error(), text) {
				t.Fatalf("hung write reads %q, which carries %q", err, text)
			}
		}
	})
	t.Run("the caller's clock", func(t *testing.T) {
		securityShim(t, never)
		setSecurityWriteTimeout(t, securityClockLongerThanAnyTestRun)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		err := securityKeychain{}.write(ctx, "svc", "acct", payload)
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errSecurityWriteHung) {
			t.Fatalf("write cut by the caller: %v, want context.DeadlineExceeded", err)
		}
	})
	t.Run("a refusal is the refusal", func(t *testing.T) {
		securityShim(t, "exit 36")
		setSecurityWriteTimeout(t, securityClockLongerThanAnyTestRun)
		err := securityKeychain{}.write(context.Background(), "svc", "acct", payload)
		if err == nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errSecurityWriteHung) ||
			!strings.Contains(err.Error(), "exit status 36") || strings.Contains(err.Error(), "never-in-an-error") {
			t.Fatalf("exit 36: %v, want the exit status and nothing else", err)
		}
	})
}

// The same rotation through the real securityKeychain and a `security` shim:
// the first upsert is killed (the shim kills itself, the shape jetsam leaves,
// with neither clock fired, so no clock decides the outcome) and the second
// lands. The shim records the arguments of the one that landed: the rotated
// payload, upserted with -U into the profile's own item.
func TestARotationLandsThroughSecurityWhenItsFirstWriteIsKilled(t *testing.T) {
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Fatalf("resolve cat before PATH is pinned: %v", err)
	}
	fastKeychainWriteRetries(t)
	swapKeychain(t, securityKeychain{})
	store, profile := keychainOnlyProfile(t, securityKeychain{}, "work", nil)
	configDir := store.ClaudeConfigDir(profile.Name)
	dir := t.TempDir()
	stored, marker, record := filepath.Join(dir, "stored"), filepath.Join(dir, "killed-once"), filepath.Join(dir, "landed")
	if err := os.WriteFile(stored, credentialJSON(t, "expired-access", "about-to-be-spent", time.Now().Add(-time.Hour)), 0o600); err != nil {
		t.Fatal(err)
	}
	securityShim(t, "case \"$1\" in\n"+
		"find-generic-password) exec '"+cat+"' '"+stored+"' ;;\n"+
		"add-generic-password)\n"+
		"  if [ ! -e '"+marker+"' ]; then : > '"+marker+"'; kill -9 $$; fi\n"+
		"  printf '%s\\n' \"$*\" >> '"+record+"' ;;\n"+
		"esac")
	answerBeforeTheClock(t)
	setSecurityWriteTimeout(t, securityClockLongerThanAnyTestRun)

	account, didRefresh, err := store.RefreshCredentialIfExpired(context.Background(), rotatingClient(nil), profile)
	if err != nil || !didRefresh || account.Token != "minted-access" {
		t.Fatalf("refresh whose first keychain write was killed: %q %v %v, want the minted credential", account.Token, didRefresh, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("calibration: the first write was never killed (%v)", err)
	}
	landed, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("no upsert landed: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(landed), "\n"), "\n")
	wantPrefix := "add-generic-password -U -s " + wantKeychainService(configDir) + " -a " + currentUsername(t) + " -w "
	if len(lines) != 1 || !strings.HasPrefix(lines[0], wantPrefix) || !strings.Contains(lines[0], "minted-refresh") {
		t.Fatalf("landed upserts %q, want one upsert of the rotated credential into the profile's item", lines)
	}
}
