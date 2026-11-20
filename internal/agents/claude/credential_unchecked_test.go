package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// A credential read that the store did not answer is not "this profile has no
// access token". On the laptop, 2026-09-27/28, the proxy's 5 s pool budget cut
// `security` reads under load; keychainCredential read every cut as "no
// keychain credential", the refresh reported `has no access token`, and the
// proxy zeroed a healthy account and moved its warm sessions for good. See
// ErrCredentialUnchecked.
//
// None of these tests reaches the real macOS keychain. The store tests swap the
// vault seam for a fake AND pin PATH to an empty directory, asserting
// `security` does not resolve (keychainOnlyProfile, noSecurityOnPath): a seam
// swap alone is not a guard, because a test with the swap edited out still
// passes on darwin while it reads the login keychain -- measured, two real
// `find-generic-password` execs under a green test. The tests that drive
// securityKeychain itself put a shim on a PATH that holds nothing else and
// prove `security` resolves to it (securityShim).

// cutKeychain wraps fakeKeychain and fails reads -- of chosen services, or all
// of them once failAll is set -- with the error a vault that did not answer
// returns.
type cutKeychain struct {
	*fakeKeychain
	mu      sync.Mutex
	failAll bool
	failFor map[string]bool
	cut     error
}

func (k *cutKeychain) read(ctx context.Context, service, account string) ([]byte, bool, error) {
	k.mu.Lock()
	fail := k.failAll || k.failFor[service]
	k.mu.Unlock()
	if fail {
		return nil, false, k.cut
	}
	return k.fakeKeychain.read(ctx, service, account)
}

func (k *cutKeychain) failEveryRead() {
	k.mu.Lock()
	k.failAll = true
	k.mu.Unlock()
}

func credentialJSON(t *testing.T, access, refresh string, expiresAt time.Time) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": access, "refreshToken": refresh, "expiresAt": expiresAt.UnixMilli(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// The vault failures a loaded Mac actually produces. The first is exactly what
// securityKeychain.read now returns for a read its clock cut; the others are
// the same "nobody answered" without a clock.
var vaultFailures = []struct {
	name     string
	err      error
	deadline bool
}{
	{"cut by the deadline", errors.Join(errors.New("security find-generic-password"), context.DeadlineExceeded, errors.New("signal: killed")), true},
	{"keychain refused", errors.New("exit status 36"), false},
	{"fork failed under load", fmt.Errorf("%w (%v)", errSecurityReadNotRun, "fork/exec /usr/bin/security: resource temporarily unavailable"), false},
}

// keychainOnlyProfile also pins PATH empty (noSecurityOnPath), so every test
// that builds its profile here fails, rather than reaching the login keychain,
// if its vault-seam swap is ever lost. A test that drives securityKeychain on
// purpose puts its shim on PATH after this returns.
func keychainOnlyProfile(t *testing.T, kc keychain, name string, credential []byte) (Store, Profile) {
	t.Helper()
	noSecurityOnPath(t)
	freshCredentialSilence(t)
	freshKeychainAnswers(t)
	store := Store{Dir: filepath.Join(t.TempDir(), ".subrouter", "codex")}
	if _, err := store.CreateProfile(name); err != nil {
		t.Fatal(err)
	}
	profile, ok := store.FindProfile(name)
	if !ok {
		t.Fatalf("profile %q not registered", name)
	}
	if credential != nil {
		if err := kc.write(context.Background(), wantKeychainService(store.ClaudeConfigDir(profile.Name)), currentUsername(t), credential); err != nil {
			t.Fatal(err)
		}
	}
	return store, profile
}

func TestRefreshReportsAKeychainThatDidNotAnswerAsUncheckedNotAsNoAccessToken(t *testing.T) {
	for _, failure := range vaultFailures {
		t.Run(failure.name, func(t *testing.T) {
			kc := &cutKeychain{fakeKeychain: newFakeKeychain(), cut: failure.err}
			swapKeychain(t, kc)
			store, profile := keychainOnlyProfile(t, kc, "work",
				credentialJSON(t, "live-access", "live-refresh", time.Now().Add(time.Hour)))
			kc.failEveryRead()

			_, _, err := store.RefreshCredentialIfExpired(context.Background(), nil, profile)
			if err == nil {
				t.Fatal("a keychain that did not answer produced a credential")
			}
			if strings.Contains(err.Error(), "no access token") {
				t.Fatalf("a keychain that did not answer was reported as a dead login: %v", err)
			}
			if !errors.Is(err, ErrCredentialUnchecked) {
				t.Fatalf("error does not carry ErrCredentialUnchecked: %v", err)
			}
			if errors.Is(err, context.DeadlineExceeded) != failure.deadline {
				t.Fatalf("errors.Is(DeadlineExceeded) = %v, want %v: %v", !failure.deadline, failure.deadline, err)
			}
		})
	}
}

// The verdict the fix must not blur: a vault that answered "no such item" is
// still a profile with no access token, on a Mac and on a vault-less host.
func TestRefreshStillReportsAGenuinelyMissingCredentialAsNoAccessToken(t *testing.T) {
	for name, kc := range map[string]keychain{
		"keychain has no item": newFakeKeychain(),
		"no vault, no file":    unavailableKeychain{},
	} {
		t.Run(name, func(t *testing.T) {
			swapKeychain(t, kc)
			store, profile := keychainOnlyProfile(t, kc, "work", nil)
			_, _, err := store.RefreshCredentialIfExpired(context.Background(), nil, profile)
			if err == nil || !strings.Contains(err.Error(), `Claude profile "work" has no access token`) {
				t.Fatalf("genuinely missing credential: %v, want has no access token", err)
			}
			if errors.Is(err, ErrCredentialUnchecked) {
				t.Fatalf("a genuine absence was marked unchecked: %v", err)
			}
		})
	}
}

// The vault's own answer of absence, through the real securityKeychain: exit
// 44 (errSecItemNotFound) and an item that is there but empty. Both are the
// same `has no access token` a missing credentials file gives on a vault-less
// host, byte for byte. The proxy's genuine-absence tests
// (proxy/credential_absence_test.go) take the missing file on a vault-less
// host and, on a Mac, a vault seam that answers found=false -- what exit 44
// returns here (KeychainWithNoItemsForTest); neither starts `security`. The
// empty item, and exit 44 through the tool itself, are pinned only here.
func TestRefreshReportsASecurityThatAnsweredNoItemAsNoAccessToken(t *testing.T) {
	for name, body := range map[string]string{
		"exit 44, errSecItemNotFound": "exit 44",
		"an empty item":               "printf '\\n'; exit 0",
	} {
		t.Run(name, func(t *testing.T) {
			swapKeychain(t, securityKeychain{})
			store, profile := keychainOnlyProfile(t, securityKeychain{}, "work", nil)
			securityShim(t, body)
			answerBeforeTheClock(t)
			_, _, err := store.RefreshCredentialIfExpired(context.Background(), nil, profile)
			if err == nil || err.Error() != `Claude profile "work" has no access token` {
				t.Fatalf("security answered absence: %v, want exactly `Claude profile \"work\" has no access token`", err)
			}
			if errors.Is(err, ErrCredentialUnchecked) || errors.Is(err, ErrCredentialUnavailable) {
				t.Fatalf("an answer of absence was filed as a store that did not answer: %v", err)
			}
		})
	}
}

// A `security` that never ran to an answer, with neither clock fired, is not
// the keychain refusing: the keychain was never asked. It could not be started
// (a fork that got EAGAIN or ENOMEM on a Mac out of processes or memory, or a
// tool missing from its path), or a signal nobody here sent ended it (jetsam).
// It is never the vault's answer of absence -- that would give every
// keychain-only profile a dead-login verdict at once -- and it is not a
// refusal either: a refusal starts the quarter hour and takes the account out
// of new placements at once, and a fork that fails for want of processes fails
// for every profile together. It is load, bounded like a hang: unchecked past
// the quarter hour, never CredentialStoreNotAnswering, a verdict only at the
// hour (PATCHES §34.6).
//
// On a Mac a stripped PATH cannot produce it: the store runs /usr/bin/security
// by path (TestAMacRunsTheSystemSecurityNeverOneFromPATH). A test binary runs
// `security` by name, so emptying PATH here is the missing tool.
func TestASecurityThatCannotBeRunIsLoadNotARefusal(t *testing.T) {
	for _, run := range []struct {
		name string
		// arrange runs after keychainOnlyProfile has emptied PATH.
		arrange func(t *testing.T)
		cause   string
	}{
		{"not found by name", func(t *testing.T) {}, "executable file not found"},
		{"missing from its absolute path", func(t *testing.T) {
			setSecurityTool(t, filepath.Join(t.TempDir(), "no-such-dir", "security"))
		}, "no such file or directory"},
		{"killed by a signal nobody here sent", func(t *testing.T) {
			// kill is a shell builtin, so the shim needs nothing else on PATH.
			securityShim(t, "kill -KILL $$")
			answerBeforeTheClock(t)
		}, "signal: killed"},
	} {
		t.Run(run.name, func(t *testing.T) {
			swapKeychain(t, securityKeychain{})
			store, profile := keychainOnlyProfile(t, securityKeychain{}, "work", nil)
			run.arrange(t)
			configDir := store.ClaudeConfigDir(profile.Name)
			t0 := time.Now()
			now := withSilenceClock(t, t0)
			read := func(at time.Duration) error {
				t.Helper()
				*now = t0.Add(at)
				_, _, err := store.RefreshCredentialIfExpired(context.Background(), nil, profile)
				if err == nil {
					t.Fatalf("at %s: a keychain nobody could ask produced a credential", at)
				}
				return err
			}

			err := read(0)
			if strings.Contains(err.Error(), "no access token") || errors.Is(err, ErrCredentialUnavailable) {
				t.Fatalf("a `security` that never ran was read as a dead login: %v", err)
			}
			if !errors.Is(err, ErrCredentialUnchecked) || !errors.Is(err, errSecurityReadNotRun) {
				t.Fatalf("error is not an unchecked read that never ran: %v", err)
			}
			if !strings.Contains(err.Error(), run.cause) {
				t.Fatalf("error does not name the cause %q: %v", run.cause, err)
			}
			// Started by absolute path, a missing tool is ENOENT: the store's
			// own spelling of "no credentials file" must not come through.
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errSecurityReadHung) {
				t.Fatalf("a `security` that never ran was named absence, a cut or a hang: %v", err)
			}
			if CredentialStoreNotAnswering(configDir) {
				t.Fatal("one read that never ran took the account out of new placements")
			}

			// Past the quarter hour, three reads and more: still load.
			for _, at := range []time.Duration{10 * time.Minute, 20 * time.Minute, 30 * time.Minute, 40 * time.Minute, 50 * time.Minute} {
				if err := read(at); !errors.Is(err, ErrCredentialUnchecked) || errors.Is(err, ErrCredentialUnavailable) {
					t.Fatalf("never ran for %s: %v, want still unchecked (load keeps the account until the hour)", at, err)
				}
				if CredentialStoreNotAnswering(configDir) {
					t.Fatalf("never ran for %s: the account was taken out of new placements", at)
				}
			}
			// The hour: a `security` that never once ran to an answer is not load.
			if err := read(time.Hour); !errors.Is(err, ErrCredentialUnavailable) {
				t.Fatalf("an hour with no answer: %v, want the verdict", err)
			}
		})
	}
}

// A fork that got EAGAIN or ENOMEM cannot be staged with a real process, so
// the classification is pinned on the error exec returns for it, and then
// through the store and the silence bound like the real ones above.
func TestAForkThatFailedUnderLoadIsBoundedLikeAHang(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EAGAIN, syscall.ENOMEM} {
		t.Run(errno.Error(), func(t *testing.T) {
			forkErr := &fs.PathError{Op: "fork/exec", Path: systemSecurityTool, Err: errno}
			classified := securityReadFailure(context.Background(), context.Background(), forkErr)
			if !errors.Is(classified, errSecurityReadNotRun) || errors.Is(classified, errno) {
				t.Fatalf("fork %v = %v, want errSecurityReadNotRun carrying only the text", errno, classified)
			}
			kc := &cutKeychain{fakeKeychain: newFakeKeychain(), cut: classified}
			swapKeychain(t, kc)
			store, _ := keychainOnlyProfile(t, kc, "busy",
				credentialJSON(t, "busy-access", "busy-refresh", time.Now().Add(time.Hour)))
			dir := store.ClaudeConfigDir("busy")
			t0 := time.Now()
			now := withSilenceClock(t, t0)
			kc.failEveryRead()
			for _, at := range []time.Duration{0, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute, 40 * time.Minute, 50 * time.Minute} {
				*now = t0.Add(at)
				_, err := store.ReadCredential(context.Background(), dir)
				if !errors.Is(err, ErrCredentialUnchecked) || errors.Is(err, ErrCredentialUnavailable) {
					t.Fatalf("forks failing for %s: %v, want still unchecked", at, err)
				}
				if CredentialStoreNotAnswering(dir) {
					t.Fatalf("forks failing for %s: the account was taken out of new placements", at)
				}
			}
			*now = t0.Add(time.Hour)
			if _, err := store.ReadCredential(context.Background(), dir); !errors.Is(err, ErrCredentialUnavailable) {
				t.Fatalf("an hour of failed forks: %v, want the verdict", err)
			}
		})
	}
	// The control: a `security` that ran and exited with a status of its own
	// is the keychain refusing, not load.
	securityShim(t, "exit 36")
	answerBeforeTheClock(t)
	_, _, err := securityKeychain{}.read(context.Background(), "svc", "acct")
	if err == nil || errors.Is(err, errSecurityReadNotRun) || errors.Is(err, errSecurityReadHung) || !strings.Contains(err.Error(), "exit status 36") {
		t.Fatalf("security exit 36 = %v, want the refusal itself", err)
	}
}

// On a Mac the store runs /usr/bin/security by path, never whatever `security`
// this process's PATH finds first. By name, a daemon whose PATH lacks /usr/bin
// could not start it, and an unanswered keychain for as long as the process
// lived became a dead-login verdict for every keychain-only profile; and a
// `security` earlier on PATH answered for the login keychain (PATCHES §34.6).
// A test binary runs it by name (securityTool), so this test swaps an absolute
// shim in for /usr/bin/security and puts a decoy on PATH that must never run.
func TestAMacRunsTheSystemSecurityNeverOneFromPATH(t *testing.T) {
	if got := securityToolFor(false); got != "/usr/bin/security" {
		t.Fatalf("outside tests the store runs %q, want /usr/bin/security by path", got)
	}
	if got := securityToolFor(true); got != "security" {
		t.Fatalf("in a test binary the store runs %q, want `security` by name, where every shim and every PATH isolation works", got)
	}
	if securityTool != "security" {
		t.Fatalf("this test binary runs %q: the shims and isolation helpers here would not hold", securityTool)
	}
	if runtime.GOOS == "windows" {
		t.Skip("the shims are shell scripts")
	}

	stored := credentialJSON(t, "system-access", "system-refresh", time.Now().Add(time.Hour))
	swapKeychain(t, securityKeychain{})
	store, profile := keychainOnlyProfile(t, securityKeychain{}, "work", nil)

	systemDir := t.TempDir()
	record := filepath.Join(t.TempDir(), "system-calls")
	payload := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(payload, append(stored, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	system := filepath.Join(systemDir, "security")
	if err := os.WriteFile(system, []byte("#!/bin/sh\n"+
		"printf '%s\\n' \"$1\" >> '"+record+"'\n"+
		"case \"$1\" in find-generic-password) while read -r line; do printf '%s\\n' \"$line\"; done < '"+payload+"' ;; esac\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	setSecurityTool(t, system)
	decoyRecord := filepath.Join(t.TempDir(), "decoy-calls")
	securityShim(t, "printf 'decoy\\n' >> '"+decoyRecord+"'; exit 36")
	answerBeforeTheClock(t)

	account, _, err := store.RefreshCredentialIfExpired(context.Background(), nil, profile)
	if err != nil || account.Token != "system-access" {
		t.Fatalf("with only a decoy on PATH: token %q err %v, want the system tool's credential", account.Token, err)
	}
	if err := (securityKeychain{}).write(context.Background(), "svc", "acct", []byte("x")); err != nil {
		t.Fatalf("write through the system tool: %v", err)
	}
	if err := (securityKeychain{}).delete(context.Background(), "svc", "acct"); err != nil {
		t.Fatalf("delete through the system tool: %v", err)
	}
	if body, err := os.ReadFile(decoyRecord); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the `security` on PATH ran (%q, %v); the store must run the system tool by path", body, err)
	}
	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"find-generic-password", "add-generic-password", "delete-generic-password"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("the system tool never ran %s (it ran %q)", want, body)
		}
	}
}

// Whether a vault exists is the platform's fact, decided once and never by
// probing for the tool: on a Mac an unresolvable `security` is this process's
// PATH (or a broken OS), and the login keychain still holds Claude Code's
// items; a probe would make that a vault-less host, where a missing file is a
// dead login for every keychain-only profile. Elsewhere a `security` on PATH
// is not a vault.
func TestDefaultKeychainIsThePlatformsNeverAProbeForTheTool(t *testing.T) {
	want := keychain(unavailableKeychain{})
	if runtime.GOOS == "darwin" {
		want = securityKeychain{}
	}
	t.Run("no security on PATH or at its path", func(t *testing.T) {
		noSecurityOnPath(t)
		setSecurityTool(t, filepath.Join(t.TempDir(), "no-such-dir", "security"))
		if got := defaultKeychain(); got != want {
			t.Fatalf("defaultKeychain on %s with no tool anywhere = %T, want %T", runtime.GOOS, got, want)
		}
	})
	t.Run("a security alone on PATH", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("the shim is a shell script")
		}
		securityShim(t, "exit 44")
		if got := defaultKeychain(); got != want {
			t.Fatalf("defaultKeychain on %s with a security on PATH = %T, want %T", runtime.GOOS, got, want)
		}
	})
}

// Started by absolute path, a missing tool fails with ENOENT, and errors.Is
// reads that as fs.ErrNotExist: the store's own word for "no credentials
// file". No keychain call may pass it on.
func TestAMissingSecurityToolIsNeverAMissingFile(t *testing.T) {
	noSecurityOnPath(t)
	setSecurityTool(t, filepath.Join(t.TempDir(), "no-such-dir", "security"))
	answerBeforeTheClock(t)
	kc := securityKeychain{}
	_, found, readErr := kc.read(context.Background(), "svc", "acct")
	if found || readErr == nil {
		t.Fatalf("read with no tool: found %v err %v, want an error", found, readErr)
	}
	for op, err := range map[string]error{
		"read":   readErr,
		"write":  kc.write(context.Background(), "svc", "acct", []byte("x")),
		"delete": kc.delete(context.Background(), "svc", "acct"),
	} {
		if err == nil || errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "no such file or directory") {
			t.Errorf("%s with no tool at its path = %v, want the start failure as text, never fs.ErrNotExist", op, err)
		}
	}
}

func setSecurityTool(t *testing.T, tool string) {
	t.Helper()
	previous := securityTool
	securityTool = tool
	t.Cleanup(func() { securityTool = previous })
}

// A .credentials.json that exists but will not read (a socket here; EMFILE or
// EIO on a loaded host) is not absence. On the box -- no vault -- it read as
// `has no access token`; on a Mac it fell through to a keychain copy the
// daemon stopped rotating once the file existed.
func TestACredentialsFileThatWillNotReadIsUncheckedNotAbsent(t *testing.T) {
	for name, withStaleKeychain := range map[string]bool{
		"vault-less host":         false,
		"keychain copy beside it": true,
	} {
		t.Run(name, func(t *testing.T) {
			var kc keychain = unavailableKeychain{}
			if withStaleKeychain {
				kc = newFakeKeychain()
			}
			swapKeychain(t, kc)
			store, profile := keychainOnlyProfile(t, kc, "work", nil)
			configDir := store.ClaudeConfigDir(profile.Name)
			if withStaleKeychain {
				if err := kc.write(context.Background(), wantKeychainService(configDir), currentUsername(t),
					credentialJSON(t, "keychain-access", "keychain-refresh", time.Now().Add(time.Hour))); err != nil {
					t.Fatal(err)
				}
			}
			credentialsFileThatDoesNotAnswer(t, filepath.Join(configDir, ".credentials.json"))

			credential, err := store.ReadCredential(context.Background(), configDir)
			if credential != nil || !errors.Is(err, ErrCredentialUnchecked) {
				t.Fatalf("unreadable credentials file: credential %v err %v, want unchecked and no credential", credential != nil, err)
			}
			_, _, err = store.RefreshCredentialIfExpired(context.Background(), nil, profile)
			if err == nil || strings.Contains(err.Error(), "no access token") || !errors.Is(err, ErrCredentialUnchecked) {
				t.Fatalf("refresh over an unreadable credentials file: %v", err)
			}
		})
	}
}

// Anthropic rotates the refresh token on use. A re-read cut after the exchange
// used to drop the rotated credential ("no credential on disk, nothing to
// compare"), leaving the profile on a spent token: the next refresh is a real
// invalid_grant, manufactured from a slow `security`.
func TestRefreshPersistsTheRotatedCredentialWhenTheReReadIsUnchecked(t *testing.T) {
	kc := &cutKeychain{fakeKeychain: newFakeKeychain(),
		cut: errors.Join(errors.New("security find-generic-password"), context.DeadlineExceeded, errors.New("signal: killed"))}
	swapKeychain(t, kc)
	store, profile := keychainOnlyProfile(t, kc, "work",
		credentialJSON(t, "expired-access", "about-to-be-spent", time.Now().Add(-time.Hour)))
	service := wantKeychainService(store.ClaudeConfigDir(profile.Name))

	// The provider rotates, and from then on the keychain stops answering.
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		response, err := (&slowRefreshTransport{}).RoundTrip(req)
		kc.failEveryRead()
		return response, err
	})}

	account, didRefresh, err := store.RefreshCredentialIfExpired(context.Background(), client, profile)
	if err != nil {
		t.Fatalf("refresh with a cut re-read: %v (the rotated credential was dropped)", err)
	}
	if !didRefresh || account.Token != "minted-access" {
		t.Fatalf("account token %q didRefresh %v, want the minted credential", account.Token, didRefresh)
	}
	stored, ok := kc.items[keychainItemKey{service, currentUsername(t)}]
	if !ok || !strings.Contains(string(stored), "minted-refresh") {
		t.Fatalf("keychain holds %q, want the rotated refresh token persisted", stored)
	}
}

// The daemon's reload must not drop an account whose read was cut: an account
// that vanishes from the pool reads as removed, and every session on it takes a
// fresh pick. A genuinely missing credential still drops, previous or not.
func TestListAccountsKeepingKeepsAProfileWhoseReadWasUnchecked(t *testing.T) {
	noSecurityOnPath(t)
	kc := &cutKeychain{fakeKeychain: newFakeKeychain(), failFor: map[string]bool{},
		cut: errors.Join(errors.New("security find-generic-password"), context.DeadlineExceeded, errors.New("signal: killed"))}
	swapKeychain(t, kc)
	store := Store{Dir: filepath.Join(t.TempDir(), ".subrouter", "codex")}
	for _, name := range []string{"cut", "gone", "healthy"} {
		if _, err := store.CreateProfile(name); err != nil {
			t.Fatal(err)
		}
		if name == "gone" {
			continue
		}
		if err := kc.write(context.Background(), wantKeychainService(store.ClaudeConfigDir(name)), currentUsername(t),
			credentialJSON(t, name+"-access", name+"-refresh", time.Now().Add(time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	previous, err := store.ListAccounts(context.Background())
	if err != nil || len(previous) != 2 {
		t.Fatalf("baseline listing = %d accounts, err %v", len(previous), err)
	}
	// "gone" had a listing once; its item is genuinely absent now.
	previous = append(previous, accounts.Account{ID: "gone", Provider: accounts.ProviderClaude,
		AuthMode: accounts.AuthModeOAuth, Token: "gone-access", Source: store.ClaudeConfigDir("gone")})
	kc.failFor[wantKeychainService(store.ClaudeConfigDir("cut"))] = true

	byID := func(id string) (accounts.Account, bool) {
		for _, account := range previous {
			if account.ID == id {
				return account, true
			}
		}
		return accounts.Account{}, false
	}
	listed, err := store.ListAccountsKeeping(context.Background(), byID)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, account := range listed {
		ids[account.ID] = account.Token
	}
	if ids["cut"] != "cut-access" {
		t.Fatalf("listing %v: the profile whose read was cut dropped out of the pool", ids)
	}
	if _, ok := ids["gone"]; ok {
		t.Fatalf("listing %v: a genuinely missing credential was kept", ids)
	}
	if ids["healthy"] != "healthy-access" {
		t.Fatalf("listing %v: healthy profile missing", ids)
	}
}

// ListAccountsKeeping keeps a previous listing through exactly one kind of
// failure: a read that reached no verdict, of the same instance directory.
// Two guards say so, and each is pinned here. A read that ANSWERED -- a blob
// that will not parse -- is a verdict and is dropped however recently the
// account was listed; keeping it would route on a token from a credential the
// store now says is unreadable. And a previous listing of a DIFFERENT instance
// directory (the profile was removed and re-added between reloads) is never
// kept: its token is an earlier login's. That profile is listed without a
// credential instead, at its current directory.
func TestListAccountsKeepingKeepsOnlyAnUncheckedReadOfTheSameInstance(t *testing.T) {
	kc := &cutKeychain{fakeKeychain: newFakeKeychain(), failFor: map[string]bool{},
		cut: errors.Join(errors.New("security find-generic-password"), context.DeadlineExceeded, errors.New("signal: killed"))}
	swapKeychain(t, kc)
	store, _ := keychainOnlyProfile(t, kc, "cut", nil)
	for _, name := range []string{"corrupt", "moved"} {
		if _, err := store.CreateProfile(name); err != nil {
			t.Fatal(err)
		}
	}
	if err := kc.write(context.Background(), wantKeychainService(store.ClaudeConfigDir("corrupt")), currentUsername(t),
		[]byte("{not a credential")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadCredential(context.Background(), store.ClaudeConfigDir("corrupt")); err == nil || errors.Is(err, ErrCredentialUnchecked) {
		t.Fatalf("the corrupt blob reads as %v, want an answer that is not unchecked", err)
	}
	kc.failFor[wantKeychainService(store.ClaudeConfigDir("cut"))] = true
	kc.failFor[wantKeychainService(store.ClaudeConfigDir("moved"))] = true
	previous := map[string]accounts.Account{
		"cut":     {ID: "cut", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "cut-old", Source: store.ClaudeConfigDir("cut")},
		"corrupt": {ID: "corrupt", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "corrupt-old", Source: store.ClaudeConfigDir("corrupt")},
		"moved":   {ID: "moved", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "moved-old", Source: filepath.Join(t.TempDir(), "an-earlier-instance")},
	}
	listed, err := store.ListAccountsKeeping(context.Background(), func(id string) (accounts.Account, bool) {
		account, ok := previous[id]
		return account, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]accounts.Account{}
	for _, account := range listed {
		byID[account.ID] = account
	}
	if byID["cut"].Token != "cut-old" {
		t.Fatalf("listing %v: the control -- an unchecked read of the same instance -- was not kept", byID)
	}
	if corrupt, ok := byID["corrupt"]; ok {
		t.Fatalf("listing of corrupt = %+v: a read that answered (unparseable) was kept or listed", corrupt)
	}
	moved, ok := byID["moved"]
	if !ok {
		t.Fatalf("listing %v: the re-added profile whose read got no answer was dropped", byID)
	}
	if moved.Token != "" || moved.Source != store.ClaudeConfigDir("moved") {
		t.Fatalf("listing of moved = %+v: the previous listing of another instance directory was kept (an earlier login's token)", moved)
	}
}

// The daemon's first listing has no previous listing to keep. A profile whose
// first read got no answer is listed anyway -- without a credential, at its own
// instance directory -- so a restart under load does not drop it from the pool
// (the "removed" that moves every session pinned to it). A genuine absence is
// still not listed, and ListAccounts -- every caller outside the daemon --
// keeps its old view.
func TestListAccountsKeepingListsAnUncheckedProfileWithNothingToKeep(t *testing.T) {
	kc := &cutKeychain{fakeKeychain: newFakeKeychain(), failFor: map[string]bool{},
		cut: errors.New("exit status 36")}
	swapKeychain(t, kc)
	store, _ := keychainOnlyProfile(t, kc, "silent", nil)
	for _, name := range []string{"gone", "healthy"} {
		if _, err := store.CreateProfile(name); err != nil {
			t.Fatal(err)
		}
	}
	if err := kc.write(context.Background(), wantKeychainService(store.ClaudeConfigDir("healthy")), currentUsername(t),
		credentialJSON(t, "healthy-access", "healthy-refresh", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	kc.failFor[wantKeychainService(store.ClaudeConfigDir("silent"))] = true

	nothingToKeep := func(string) (accounts.Account, bool) { return accounts.Account{}, false }
	listed, err := store.ListAccountsKeeping(context.Background(), nothingToKeep)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]accounts.Account{}
	for _, account := range listed {
		byID[account.ID] = account
	}
	silent, ok := byID["silent"]
	if !ok {
		t.Fatalf("listing %v: the profile whose read got no answer was dropped", byID)
	}
	if silent.Token != "" || silent.CredentialVersion != "" || silent.Source != store.ClaudeConfigDir("silent") ||
		silent.Provider != accounts.ProviderClaude || silent.AuthMode != accounts.AuthModeOAuth {
		t.Fatalf("listing of the unchecked profile = %+v, want no credential at its own instance directory", silent)
	}
	if _, ok := byID["gone"]; ok {
		t.Fatalf("listing %v: a genuinely missing credential was listed", byID)
	}
	if byID["healthy"].Token != "healthy-access" {
		t.Fatalf("listing %v: healthy profile missing", byID)
	}

	plain, err := store.ListAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range plain {
		if account.ID == "silent" {
			t.Fatalf("ListAccounts listed %+v: callers outside the daemon never list an account without a credential", account)
		}
	}
}

// securityShim puts a `security` that runs body on a PATH holding nothing
// else, and proves `security` resolves to it, so nothing a test does with
// securityKeychain can reach the real login keychain. PATH holds only the
// shim, so body must name every other binary by absolute path (sleepBinary).
func securityShim(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the shim is a shell script")
	}
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "security"), []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin)
	resolved, err := exec.LookPath("security")
	if err != nil || resolved != filepath.Join(fakeBin, "security") {
		t.Fatalf("security resolves to %q (%v), want only the shim in %s", resolved, err, fakeBin)
	}
}

// sleepBinary resolves sleep, and must run before a test pins PATH (to the
// shim alone, or to nothing): after that it cannot resolve. It fails rather
// than skips. A skip is reported ok, and a shim test that silently skipped --
// what the first draft of neverAnswers did, resolving sleep after
// keychainOnlyProfile had emptied PATH -- proves nothing while looking green.
func sleepBinary(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the shim is a shell script")
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("resolve sleep before PATH is pinned: %v", err)
	}
	return sleep
}

// The shim tests race two things -- a `security` process and a read clock --
// and each test must decide the winner by construction, never by how fast a
// loaded host happens to start `sh`. A flake here is a race, and the first cut
// of these tests had one: TestAHangingKeychainThatAlsoRefusesGetsTheQuarterHour
// ran its refusing read (`exit 36`) on the 100 ms clock its hangs needed, so
// under CPU contention sh started after the clock fired and the refusal read
// as one more hang -- measured 123 of 200 runs red in Docker at --cpus=0.2
// with eight busy loops in the container, and 0 of 200 with the helpers below.
// Each of them makes one side unable to win:
//
//   - neverAnswers is a `security` that cannot finish on its own: the only
//     thing that ends it is the kill a read clock sends. A read of it is a
//     hang (or a cut) whatever the host's load.
//   - answerBeforeTheClock gives a read that must see the shim's ANSWER an own
//     clock longer than any `go test -timeout`: the answer lands first, or the
//     harness kills the run before the clock could fire -- a timeout the run
//     reports as one, never a hang misread as a pass or a fail.
const securityClockLongerThanAnyTestRun = 24 * time.Hour

// neverAnswers resolves sleep, so like sleepBinary it must be called before
// the test pins PATH; keep its result and hand it to securityShim later.
func neverAnswers(t *testing.T) string {
	t.Helper()
	// 2^31-1 seconds: accepted by GNU and BSD sleep alike (`infinity` is not),
	// and sixty-eight years is "never" for a test.
	return "exec " + sleepBinary(t) + " 2147483647"
}

func answerBeforeTheClock(t *testing.T) {
	t.Helper()
	setSecurityReadTimeout(t, securityClockLongerThanAnyTestRun)
}

func setSecurityReadTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	previous := securityReadTimeout
	securityReadTimeout = d
	t.Cleanup(func() { securityReadTimeout = previous })
}

// securityKeychain itself: a read the CALLER's clock cut must say deadline --
// that is load, and it never counts toward a verdict (observeCredentialRead).
// A read that ran out its OWN clock while the caller was still waiting must
// not: `security` did not answer in five seconds, and a keychain that never
// answers has to reach the hang bound instead of being filed with the load
// cuts forever. exec reports either kill as "signal: killed", which wraps no
// context error, so the read has to consult the clocks itself.
func TestSecurityKeychainReadTellsTheCallersCutFromAHang(t *testing.T) {
	never := neverAnswers(t)
	hang := func(t *testing.T, ctx context.Context) {
		t.Helper()
		securityShim(t, never)
		setSecurityReadTimeout(t, 200*time.Millisecond)
		_, found, err := securityKeychain{}.read(ctx, "svc", "acct")
		if found || err == nil {
			t.Fatalf("read that ran out its own clock: found %v err %v, want an error", found, err)
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			t.Fatalf("read that ran out its own clock, caller still waiting: %v, want no context error (a hang is not a load cut)", err)
		}
		// The proxy's sweepCut and the peers' unmeasured test also match
		// context text, and a verdict carrying it would be published as a cut.
		for _, text := range []string{context.DeadlineExceeded.Error(), context.Canceled.Error()} {
			if strings.Contains(err.Error(), text) {
				t.Fatalf("hang reads %q, which carries %q", err, text)
			}
		}
	}

	t.Run("its own clock, caller with no clock", func(t *testing.T) {
		hang(t, context.Background())
	})
	t.Run("its own clock, the caller's longer clock still running", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), securityClockLongerThanAnyTestRun)
		defer cancel()
		hang(t, ctx)
	})
	t.Run("the caller's clock", func(t *testing.T) {
		securityShim(t, never)
		// The read's own clock must not be able to fire first: with both
		// within reach, a starved runtime could run the child's timer before
		// the parent's and file the caller's cut as a hang.
		setSecurityReadTimeout(t, securityClockLongerThanAnyTestRun)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_, found, err := securityKeychain{}.read(ctx, "svc", "acct")
		if found || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("read cut by the caller: found %v err %v, want context.DeadlineExceeded", found, err)
		}
	})
	t.Run("item not found is an answer", func(t *testing.T) {
		securityShim(t, "exit 44")
		answerBeforeTheClock(t)
		_, found, err := securityKeychain{}.read(context.Background(), "svc", "acct")
		if found || err != nil {
			t.Fatalf("exit 44: found %v err %v, want absent with no error", found, err)
		}
	})
	t.Run("a refusal is an error, not a deadline", func(t *testing.T) {
		securityShim(t, "exit 36")
		answerBeforeTheClock(t)
		_, found, err := securityKeychain{}.read(context.Background(), "svc", "acct")
		if found || err == nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errSecurityReadHung) {
			t.Fatalf("exit 36: found %v err %v, want a plain error: the refusal itself, not a clock", found, err)
		}
	})
}
