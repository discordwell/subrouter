package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// What happens to a rotation after 34.1 has kept it: a rotation saved beside
// the store (rotationRescueFile) is taken back by the profile's next read,
// before anything can present the refresh token it spent, and a credentials
// file that only stands in for a keychain that would not take a write goes
// back to the keychain at the next write it takes (keychainStandInMarker).
//
// Keychain safety as everywhere in this package: the vault seam is swapped
// and PATH is empty (keychainOnlyProfile).

// tokenEndpoint answers every exchange with a rotation of its own --
// minted-access-N / minted-refresh-N, an hour long -- and records the refresh
// token each exchange presented. duringExchange (when set) runs while the
// provider "rotates", after the pre-read and before the persist.
type tokenEndpoint struct {
	mu        sync.Mutex
	presented []string
}

func (e *tokenEndpoint) client(duringExchange func()) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body struct {
			RefreshToken string `json:"refresh_token"`
		}
		raw, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(raw, &body)
		e.mu.Lock()
		e.presented = append(e.presented, body.RefreshToken)
		n := len(e.presented)
		e.mu.Unlock()
		if duringExchange != nil {
			duringExchange()
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(fmt.Sprintf(
				`{"access_token":"minted-access-%d","refresh_token":"minted-refresh-%d","expires_in":3600}`, n, n))),
			Header: http.Header{},
		}, nil
	})}
}

func (e *tokenEndpoint) tokensPresented() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.presented...)
}

// holdCredentialLock takes the profile's credential lock, as a reader stuck
// on a `security` that will not answer holds it, and returns the release.
func holdCredentialLock(t *testing.T, configDir string) func() {
	t.Helper()
	lock, err := lockProfileCredential(context.Background(), configDir)
	if err != nil {
		t.Fatalf("take the credential lock: %v", err)
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = lock.Close() }) }
	t.Cleanup(release)
	return release
}

// rescueARotation runs the 34.1 lock-held-throughout refresh on a keychain-only
// profile whose token is expired: the rotation (minted-*-1) goes to the rescue
// file, and the keychain keeps the token the rotation spent.
func rescueARotation(t *testing.T) (*fakeKeychain, Store, Profile, string, *tokenEndpoint) {
	t.Helper()
	setCredentialPersistTimeout(t, 50*time.Millisecond)
	kc := newFakeKeychain()
	swapKeychain(t, kc)
	store, profile := keychainOnlyProfile(t, kc, "work",
		credentialJSON(t, "expired-access", "about-to-be-spent", time.Now().Add(-time.Hour)))
	configDir := store.ClaudeConfigDir(profile.Name)
	endpoint := &tokenEndpoint{}
	var release func()
	_, _, err := store.RefreshCredentialIfExpired(context.Background(),
		endpoint.client(func() { release = holdCredentialLock(t, configDir) }), profile)
	if _, statErr := os.Stat(filepath.Join(configDir, rotationRescueFile)); statErr != nil {
		t.Fatalf("calibration: no rescue file (%v); refresh %v", statErr, err)
	}
	// What the refresh answered: no verdict (the account is not dead), and
	// nothing a reader could take for a clock that merely ran out.
	if !errors.Is(err, ErrCredentialUnchecked) || !errors.Is(err, errRotationRescued) ||
		errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "deadline") {
		t.Fatalf("a rescued rotation's refresh returned %v; want unchecked, errRotationRescued, and no context error in it or its text", err)
	}
	release()
	// Whatever runs next persists on a clock no loaded host stalls through:
	// only the rescue above was meant to run out of time.
	setCredentialPersistTimeout(t, 10*time.Second)
	return kc, store, profile, configDir, endpoint
}

// The finding against 34.1 as first written: the rescue file was read by
// nothing, so the profile's next refresh -- seconds later, since the rotation
// happened because the token was expiring -- read the keychain's spent copy
// and presented it: invalid_grant, and if the provider revokes a chain whose
// spent token is presented, the rescued rotation with it. The next read now
// takes the rescue back first, and nothing ever presents the spent token.
func TestTheNextReadAdoptsARescuedRotation(t *testing.T) {
	kc, store, profile, configDir, endpoint := rescueARotation(t)

	account, didRefresh, err := store.RefreshCredentialIfExpired(context.Background(), endpoint.client(nil), profile)
	if err != nil || didRefresh || account.Token != "minted-access-1" {
		t.Fatalf("refresh after the rescue: %q didRefresh %v %v; want the rescued rotation, served without an exchange", account.Token, didRefresh, err)
	}
	if got := endpoint.tokensPresented(); len(got) != 1 || got[0] != "about-to-be-spent" {
		t.Fatalf("exchanges presented %q; the one rotation, and never its spent token again", got)
	}
	if _, err := os.Stat(filepath.Join(configDir, rotationRescueFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the rescue file is still there after its adoption (%v)", err)
	}
	file, err := os.ReadFile(filepath.Join(configDir, ".credentials.json"))
	if err != nil || !strings.Contains(string(file), "minted-refresh-1") || strings.Contains(string(file), "replaces") {
		t.Fatalf("credentials file %q (%v); want the rescued rotation in the file's own form", file, err)
	}
	if !keychainStandIn(configDir) {
		t.Fatal("a keychain-only profile's adopted rotation is not marked a keychain stand-in")
	}

	// The next rotation presents the rescued refresh token, and lands back in
	// the keychain, which takes writes again: the profile is keychain-only.
	account, didRefresh, err = store.ForceRefreshCredential(context.Background(), endpoint.client(nil), profile)
	if err != nil || !didRefresh || account.Token != "minted-access-2" {
		t.Fatalf("the rotation after the adoption: %q %v %v", account.Token, didRefresh, err)
	}
	if got := endpoint.tokensPresented(); len(got) != 2 || got[1] != "minted-refresh-1" {
		t.Fatalf("exchanges presented %q; want the rescued refresh token next", got)
	}
	stored := kc.items[keychainItemKey{wantKeychainService(configDir), currentUsername(t)}]
	if !strings.Contains(string(stored), "minted-refresh-2") {
		t.Fatalf("keychain holds %q; want the rotation back in the keychain", stored)
	}
	for _, leftover := range []string{".credentials.json", keychainStandInMarker} {
		if _, err := os.Stat(filepath.Join(configDir, leftover)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s is still there once the keychain took the rotation (%v)", leftover, err)
		}
	}
}

// A rescue applies only while the store still holds the credential it
// replaced. One that has changed since -- here an import of a credential that
// expires before the rescued one, which a later-expiry rule would clobber --
// wins, and the rescue is removed.
func TestARescuedRotationTheStoreHasMovedPastIsDropped(t *testing.T) {
	_, store, profile, configDir, endpoint := rescueARotation(t)
	imported := CredentialInfo{AccessToken: "imported-access", RefreshToken: "imported-refresh",
		ExpiresAt: time.Now().Add(30 * time.Minute).UnixMilli()}
	if err := store.ImportProfileCredential(profile.Name, imported); err != nil {
		t.Fatal(err)
	}
	// The import removes a rescue it finds; one written after it (the persist
	// was still waiting on the lock the import held) is what the read meets.
	if err := os.WriteFile(filepath.Join(configDir, rotationRescueFile), mustRescuePayload(t,
		CredentialInfo{AccessToken: "expired-access", RefreshToken: "about-to-be-spent"},
		CredentialInfo{AccessToken: "minted-access-1", RefreshToken: "minted-refresh-1", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}), 0o600); err != nil {
		t.Fatal(err)
	}

	account, _, err := store.RefreshCredentialIfExpired(context.Background(), endpoint.client(nil), profile)
	if err != nil || account.Token != "imported-access" {
		t.Fatalf("refresh after an import superseded the rescue: %q %v; want the import", account.Token, err)
	}
	if _, err := os.Stat(filepath.Join(configDir, rotationRescueFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a rescue that no longer applies was left in place (%v)", err)
	}
	if len(endpoint.tokensPresented()) != 1 {
		t.Fatalf("exchanges presented %q; want none after the rescue", endpoint.tokensPresented())
	}
}

func mustRescuePayload(t *testing.T, before, refreshed CredentialInfo) []byte {
	t.Helper()
	body, err := rescuePayload(before, refreshed)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// Up to twenty seconds of lock waits separate the exchange from the rescue.
// A profile removed (and added again) meanwhile is not the one that rotated,
// and a removal that has taken the instance directory away must not see it
// recreated by the rescue.
func TestARescueIsNotWrittenForAProfileThatIsGone(t *testing.T) {
	for name, meanwhile := range map[string]func(t *testing.T, store Store, profile Profile, configDir string){
		"removed and added again": func(t *testing.T, store Store, profile Profile, _ string) {
			data := store.readProfiles()
			readded := data.Profiles[profile.Name]
			readded.CreatedAt = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			data.Profiles[profile.Name] = readded
			if err := store.writeProfiles(data); err != nil {
				t.Error(err)
			}
		},
		"its directory taken away": func(t *testing.T, _ Store, _ Profile, configDir string) {
			if err := os.RemoveAll(configDir); err != nil {
				t.Error(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			setCredentialPersistTimeout(t, 50*time.Millisecond)
			kc := newFakeKeychain()
			swapKeychain(t, kc)
			store, profile := keychainOnlyProfile(t, kc, "work",
				credentialJSON(t, "expired-access", "about-to-be-spent", time.Now().Add(-time.Hour)))
			configDir := store.ClaudeConfigDir(profile.Name)
			log := captureLog(t)
			// The change lands while the persist waits on its second clock,
			// from inside its own log line: after the exchange, before the
			// rescue, by construction.
			log.onRecord = func(record slog.Record) {
				attempt := 0
				record.Attrs(func(a slog.Attr) bool {
					if a.Key == "attempt" {
						attempt = int(a.Value.Int64())
					}
					return true
				})
				if strings.Contains(record.Message, "lock not taken to persist") && attempt == 2 {
					meanwhile(t, store, profile, configDir)
				}
			}
			_, _, err := store.RefreshCredentialIfExpired(context.Background(),
				(&tokenEndpoint{}).client(func() { holdCredentialLock(t, configDir) }), profile)
			if err == nil {
				t.Fatal("a rotation for a profile that is gone reported success")
			}
			if _, statErr := os.Stat(filepath.Join(configDir, rotationRescueFile)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("a rescue was written for a profile that is gone (%v)", statErr)
			}
			for _, token := range []string{"minted-access", "minted-refresh", "about-to-be-spent"} {
				if strings.Contains(log.String(), token) {
					t.Fatalf("the log carries a token (%s):\n%s", token, log.String())
				}
			}
		})
	}
}

// One slow write used to make a keychain-only profile file-backed for good:
// every later rotation went only to the file, so the keychain item -- which a
// direct Claude Code launch on the profile reads -- held a spent chain after
// every rotation, and the profile ran `security` on every read and never
// reused an answer. The file is now a stand-in, and the first rotation the
// keychain takes puts the profile back.
func TestAStandInCredentialsFileGoesBackToTheKeychain(t *testing.T) {
	setup := func(t *testing.T) (*flakyWriteKeychain, Store, Profile, string, *tokenEndpoint) {
		t.Helper()
		fastKeychainWriteRetries(t)
		// Long enough that a stalled goroutine does not miss the lock, short
		// enough that the refused writes run out of it quickly.
		setCredentialPersistTimeout(t, 300*time.Millisecond)
		kc := &flakyWriteKeychain{fakeKeychain: newFakeKeychain(), err: hungWrite}
		swapKeychain(t, kc)
		store, profile := keychainOnlyProfile(t, kc, "work",
			credentialJSON(t, "expired-access", "about-to-be-spent", time.Now().Add(-time.Hour)))
		configDir := store.ClaudeConfigDir(profile.Name)
		captureLog(t)
		endpoint := &tokenEndpoint{}
		kc.failNext(-1)
		if _, _, err := store.RefreshCredentialIfExpired(context.Background(), endpoint.client(nil), profile); err != nil {
			t.Fatal(err)
		}
		if !keychainStandIn(configDir) {
			t.Fatal("calibration: the rotation the keychain would not take is not marked a stand-in")
		}
		kc.failNext(0)
		setCredentialPersistTimeout(t, 10*time.Second)
		return kc, store, profile, configDir, endpoint
	}

	t.Run("the next rotation the keychain takes", func(t *testing.T) {
		kc, store, profile, configDir, endpoint := setup(t)
		account, didRefresh, err := store.ForceRefreshCredential(context.Background(), endpoint.client(nil), profile)
		if err != nil || !didRefresh || account.Token != "minted-access-2" {
			t.Fatalf("second rotation: %q %v %v", account.Token, didRefresh, err)
		}
		stored := kc.items[keychainItemKey{wantKeychainService(configDir), currentUsername(t)}]
		if !strings.Contains(string(stored), "minted-refresh-2") {
			t.Fatalf("keychain holds %q; want the second rotation back in the keychain", stored)
		}
		for _, leftover := range []string{".credentials.json", keychainStandInMarker} {
			if _, err := os.Stat(filepath.Join(configDir, leftover)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s is still there once the keychain took a rotation (%v): the profile stays file-backed", leftover, err)
			}
		}
		// Keychain-only again, so the answer is served without a read.
		if account, _, err := store.RefreshCredentialIfExpired(context.Background(), nil, profile); err != nil || account.Token != "minted-access-2" {
			t.Fatalf("refresh after the return: %q %v", account.Token, err)
		}
		keychainAnswers.Lock()
		_, remembered := keychainAnswers.entries[filepath.Clean(configDir)]
		keychainAnswers.Unlock()
		if !remembered {
			t.Fatal("a profile back in the keychain is not served from its keychain answer")
		}
	})

	t.Run("an import makes the file the store on purpose", func(t *testing.T) {
		kc, store, profile, configDir, endpoint := setup(t)
		if err := store.ImportProfileCredential(profile.Name, CredentialInfo{AccessToken: "imported-access",
			RefreshToken: "imported-refresh", ExpiresAt: time.Now().Add(-time.Hour).UnixMilli()}); err != nil {
			t.Fatal(err)
		}
		if keychainStandIn(configDir) {
			t.Fatal("an imported credentials file is still marked a keychain stand-in")
		}
		if _, _, err := store.RefreshCredentialIfExpired(context.Background(), endpoint.client(nil), profile); err != nil {
			t.Fatal(err)
		}
		file, err := os.ReadFile(filepath.Join(configDir, ".credentials.json"))
		if err != nil || !strings.Contains(string(file), "minted-refresh-2") {
			t.Fatalf("credentials file %q (%v); an imported profile rotates in its file", file, err)
		}
		if _, ok := kc.items[keychainItemKey{wantKeychainService(configDir), currentUsername(t)}]; ok {
			t.Fatal("an imported profile's rotation was written to the keychain the import cleared")
		}
	})
}

// A re-read with nothing to compare -- the keychain item rewritten during the
// exchange into a payload that will not parse, or deleted -- used to return
// without writing: the rotation was dropped with no rescue and no ERROR line,
// and the profile's next refresh presented the spent token. Nothing on disk is
// newer than the rotation, so it is written.
func TestARotationIsKeptWhenTheReReadHasNothingToCompare(t *testing.T) {
	for name, meanwhile := range map[string]func(kc *fakeKeychain, key keychainItemKey){
		"item rewritten unreadable": func(kc *fakeKeychain, key keychainItemKey) { kc.items[key] = []byte("not a credential") },
		"item deleted":              func(kc *fakeKeychain, key keychainItemKey) { delete(kc.items, key) },
	} {
		t.Run(name, func(t *testing.T) {
			kc := newFakeKeychain()
			swapKeychain(t, kc)
			store, profile := keychainOnlyProfile(t, kc, "work",
				credentialJSON(t, "expired-access", "about-to-be-spent", time.Now().Add(-time.Hour)))
			configDir := store.ClaudeConfigDir(profile.Name)
			key := keychainItemKey{wantKeychainService(configDir), currentUsername(t)}
			captureLog(t)

			account, didRefresh, err := store.RefreshCredentialIfExpired(context.Background(),
				(&tokenEndpoint{}).client(func() { meanwhile(kc, key) }), profile)
			if err != nil || !didRefresh || account.Token != "minted-access-1" {
				t.Fatalf("refresh whose re-read had nothing to compare: %q %v %v (the rotation was dropped)", account.Token, didRefresh, err)
			}
			if !strings.Contains(string(kc.items[key]), "minted-refresh-1") {
				t.Fatalf("keychain holds %q; want the rotation", kc.items[key])
			}
		})
	}
}
