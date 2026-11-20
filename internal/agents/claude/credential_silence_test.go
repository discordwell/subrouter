package claude

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// The benefit of the doubt ErrCredentialUnchecked gives a store that did not
// answer is bounded (ErrCredentialUnavailable). Before the bound, a store that
// can NEVER answer was transient forever: every turn on the account was a 502,
// its seed was carried, new sessions were still placed on it, and nothing but
// a Warn line said so.
//
// Keychain safety: every test here swaps the vault seam for a fake AND pins
// PATH to an empty directory, asserting `security` does not resolve, so even
// a darwin run with the seam swap edited out cannot reach the login keychain.

func noSecurityOnPath(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	if resolved, err := exec.LookPath("security"); err == nil {
		t.Fatalf("security still resolves to %s; these tests must never reach the real keychain", resolved)
	}
}

// credentialsFileThatDoesNotAnswer replaces path with a symlink to a Unix
// socket. open(2) on a socket fails -- ENXIO on Linux, EOPNOTSUPP on darwin --
// with an errno outside the refused set: the portable stand-in for EIO or
// EMFILE on a loaded host, a file that fails without saying anything lasting
// about itself. (A directory, the old stand-in, is a refusal: EISDIR.)
func credentialsFileThatDoesNotAnswer(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a Unix socket node behind a symlink")
	}
	dir, err := os.MkdirTemp("", "srsock")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.RemoveAll(dir)
	})
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(socket, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(path); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stand-in reads as %v, want a failure that is not absence", err)
	}
}

// freshCredentialSilence empties the process-wide streak table for the test
// and again after it. Streak keys are instance paths, unique per test, but a
// mutation that collapses the key (one streak for every profile) used to be
// caught only by streaks leaking from one test into the next -- each test run
// alone passed. With every test starting from an empty table, the per-profile
// key is pinned by TestSilenceStreakIsPerProfile alone, as it should be.
func freshCredentialSilence(t *testing.T) {
	t.Helper()
	reset := func() {
		credentialSilence.Lock()
		credentialSilence.streaks = map[string]*credentialSilenceStreak{}
		credentialSilence.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

// withSilenceClock drives observeCredentialRead's clock, from an empty streak
// table (freshCredentialSilence).
func withSilenceClock(t *testing.T, start time.Time) *time.Time {
	t.Helper()
	freshCredentialSilence(t)
	now := start
	previous := credentialSilenceNow
	credentialSilenceNow = func() time.Time { return now }
	t.Cleanup(func() { credentialSilenceNow = previous })
	return &now
}

func (k *cutKeychain) answerAgain() {
	k.mu.Lock()
	k.failAll = false
	k.mu.Unlock()
}

// A keychain that keeps refusing (a locked login keychain: exit 36 on every
// read) is given fifteen minutes and three reads, then read as a verdict --
// and the first read that answers again ends it.
func TestACredentialStoreThatStaysSilentBecomesAVerdict(t *testing.T) {
	noSecurityOnPath(t)
	kc := &cutKeychain{fakeKeychain: newFakeKeychain(), cut: errors.New("exit status 36")}
	swapKeychain(t, kc)
	store, profile := keychainOnlyProfile(t, kc, "work",
		credentialJSON(t, "live-access", "live-refresh", time.Now().Add(time.Hour)))
	configDir := store.ClaudeConfigDir(profile.Name)
	t0 := time.Now()
	now := withSilenceClock(t, t0)
	kc.failEveryRead()

	for _, at := range []time.Duration{0, 5 * time.Minute, 10 * time.Minute, 15*time.Minute - time.Second} {
		*now = t0.Add(at)
		_, err := store.ReadCredential(context.Background(), configDir)
		if !errors.Is(err, ErrCredentialUnchecked) || errors.Is(err, ErrCredentialUnavailable) {
			t.Fatalf("silent for %s: %v, want still unchecked", at, err)
		}
	}

	*now = t0.Add(15 * time.Minute)
	credential, err := store.ReadCredential(context.Background(), configDir)
	if credential != nil || !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("silent for 15m over 5 reads: credential %v err %v, want ErrCredentialUnavailable", credential != nil, err)
	}
	if errors.Is(err, ErrCredentialUnchecked) || strings.Contains(err.Error(), "not checked") {
		t.Fatalf("the verdict still reads as unchecked, which every caller treats as transient: %v", err)
	}
	if !strings.Contains(err.Error(), "no access token") || !strings.Contains(err.Error(), "exit status 36") {
		t.Fatalf("verdict text %q: want the auth-shaped marker the status publisher reads, and the cause", err)
	}

	*now = t0.Add(16 * time.Minute)
	if _, _, err := store.RefreshCredentialIfExpired(context.Background(), nil, profile); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("refresh past the bound: %v, want the verdict", err)
	}

	kc.answerAgain()
	*now = t0.Add(17 * time.Minute)
	if credential, err := store.ReadCredential(context.Background(), configDir); err != nil || credential == nil || credential.AccessToken != "live-access" {
		t.Fatalf("store answering again: credential %v err %v", credential, err)
	}
	kc.failEveryRead()
	*now = t0.Add(18 * time.Minute)
	if _, err := store.ReadCredential(context.Background(), configDir); !errors.Is(err, ErrCredentialUnchecked) || errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("a new silence right after an answer: %v, want unchecked (the answer ended the old streak)", err)
	}
}

// The silence the bound measures must be SUSTAINED. One failed read, a quiet
// stretch longer than the sweep cadence, and one turn -- the sweep's read and
// the pick's, two more -- is three failures, not a quarter hour of silence.
// Measured through the real handler before credentialSilenceMaxGap: one exit
// 36, twenty quiet minutes, one turn, and the warm session was moved for good
// (`silent_for=20m0s reads=3`), the forced switch P8 keeps for a store that
// really cannot answer. A counted failure more than credentialSilenceMaxGap
// after the previous one starts the streak over; a store that is silent at
// every read, read at the sweep's ten-minute cadence, still reaches the
// verdict.
func TestSilenceMustBeSustainedNotFailuresFarApart(t *testing.T) {
	refusingProfile := func(t *testing.T) (Store, string, *time.Time, time.Time) {
		t.Helper()
		kc := &cutKeychain{fakeKeychain: newFakeKeychain(), cut: errors.New("exit status 36")}
		swapKeychain(t, kc)
		store, profile := keychainOnlyProfile(t, kc, "work",
			credentialJSON(t, "live-access", "live-refresh", time.Now().Add(time.Hour)))
		t0 := time.Now()
		now := withSilenceClock(t, t0)
		kc.failEveryRead()
		return store, store.ClaudeConfigDir(profile.Name), now, t0
	}
	readAt := func(t *testing.T, store Store, configDir string, now *time.Time, at time.Time) error {
		t.Helper()
		*now = at
		_, err := store.ReadCredential(context.Background(), configDir)
		return err
	}

	for _, gap := range []time.Duration{20 * time.Minute, credentialSilenceMaxGap + time.Second, 8 * time.Hour} {
		t.Run("one failure, "+gap.String()+" quiet, then one turn", func(t *testing.T) {
			store, configDir, now, t0 := refusingProfile(t)
			for _, at := range []time.Duration{0, gap, gap + time.Second} {
				err := readAt(t, store, configDir, now, t0.Add(at))
				if !errors.Is(err, ErrCredentialUnchecked) || errors.Is(err, ErrCredentialUnavailable) {
					t.Fatalf("read at +%s: %v, want still unchecked (three failures with a %s hole are not sustained silence)", at, err, gap)
				}
			}
			// The streak restarted at the turn: it is a verdict only once the
			// store has stayed silent for the bound from there.
			for _, at := range []time.Duration{gap + 5*time.Minute, gap + 10*time.Minute} {
				if err := readAt(t, store, configDir, now, t0.Add(at)); errors.Is(err, ErrCredentialUnavailable) {
					t.Fatalf("read at +%s: %v, want unchecked (%s into the new streak)", at, err, at-gap)
				}
			}
			if err := readAt(t, store, configDir, now, t0.Add(gap+15*time.Minute)); !errors.Is(err, ErrCredentialUnavailable) {
				t.Fatalf("a quarter hour of sustained silence after the turn: %v, want the verdict", err)
			}
		})
	}

	t.Run("a silent store read at the sweep cadence reaches the verdict", func(t *testing.T) {
		store, configDir, now, t0 := refusingProfile(t)
		for _, at := range []time.Duration{0, 10 * time.Minute} {
			if err := readAt(t, store, configDir, now, t0.Add(at)); errors.Is(err, ErrCredentialUnavailable) {
				t.Fatalf("read at +%s: %v, want unchecked", at, err)
			}
		}
		if err := readAt(t, store, configDir, now, t0.Add(20*time.Minute)); !errors.Is(err, ErrCredentialUnavailable) {
			t.Fatalf("silent at every ten-minute sweep for twenty minutes: %v, want the verdict", err)
		}
	})

	t.Run("the gap is measured between counted failures, not from the first", func(t *testing.T) {
		store, configDir, now, t0 := refusingProfile(t)
		// Each failure is within the gap of the one before it, though the
		// last is twice the gap from the first: one sustained silence.
		for _, at := range []time.Duration{0, credentialSilenceMaxGap} {
			if err := readAt(t, store, configDir, now, t0.Add(at)); errors.Is(err, ErrCredentialUnavailable) {
				t.Fatalf("read at +%s: %v, want unchecked", at, err)
			}
		}
		if err := readAt(t, store, configDir, now, t0.Add(2*credentialSilenceMaxGap)); !errors.Is(err, ErrCredentialUnavailable) {
			t.Fatalf("failures %s apart for %s: %v, want the verdict", credentialSilenceMaxGap, 2*credentialSilenceMaxGap, err)
		}
	})
}

// The false readings of 2026-09-27/28 were reads a clock cut. However long a
// loaded host keeps cutting them, they never add up to a verdict: a cut says
// the host was slow, not that the store refused.
func TestReadsCutByADeadlineNeverAddUpToAVerdict(t *testing.T) {
	noSecurityOnPath(t)
	kc := &cutKeychain{fakeKeychain: newFakeKeychain(),
		cut: errors.Join(errors.New("security find-generic-password"), context.DeadlineExceeded, errors.New("signal: killed"))}
	swapKeychain(t, kc)
	store, profile := keychainOnlyProfile(t, kc, "work",
		credentialJSON(t, "live-access", "live-refresh", time.Now().Add(time.Hour)))
	configDir := store.ClaudeConfigDir(profile.Name)
	t0 := time.Now()
	now := withSilenceClock(t, t0)
	kc.failEveryRead()

	for at := time.Duration(0); at <= 2*time.Hour; at += 2 * time.Minute {
		*now = t0.Add(at)
		_, err := store.ReadCredential(context.Background(), configDir)
		if errors.Is(err, ErrCredentialUnavailable) || !errors.Is(err, ErrCredentialUnchecked) {
			t.Fatalf("cut reads for %s: %v, want still unchecked", at, err)
		}
	}
	// A refused read after all that starts a streak of its own; the cuts
	// before it counted for nothing.
	kc.mu.Lock()
	kc.cut = errors.New("exit status 36")
	kc.mu.Unlock()
	if _, err := store.ReadCredential(context.Background(), configDir); errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("first refused read after two hours of cuts: %v, want unchecked", err)
	}
}

// A keychain that never answers is silence, not load. The laptop daemon is a
// per-user LaunchAgent, so a locked login keychain or an access-control
// prompt can present as `security` blocked behind a SecurityAgent dialog
// nobody is there to click, rather than as exit 36 (inferred; confirming it
// would mean touching the keychain). Every read then ends on its own clock
// while the caller is still waiting. Those reads used to be filed with the
// load cuts, which never count, so the account was transient forever: every
// turn waited five seconds for a 502, its seed was carried, and new sessions
// were still placed on it -- probed, 49 reads over four hours, all unchecked.
// A hang counts now, against the hang bound: still unchecked a quarter hour
// in, where a refusing store would already be a verdict, and a verdict at
// the hour.
func TestAKeychainThatHangsOnEveryReadBecomesAVerdictAtTheHour(t *testing.T) {
	never := neverAnswers(t)
	swapKeychain(t, securityKeychain{})
	store, profile := keychainOnlyProfile(t, securityKeychain{}, "work", nil)
	securityShim(t, never)
	setSecurityReadTimeout(t, 100*time.Millisecond)
	configDir := store.ClaudeConfigDir(profile.Name)
	t0 := time.Now()
	now := withSilenceClock(t, t0)

	// Read at the sweep's ten-minute cadence, inside credentialSilenceMaxGap:
	// one sustained hang.
	for _, at := range []time.Duration{0, 10 * time.Minute, 15 * time.Minute, 20 * time.Minute, 30 * time.Minute, 40 * time.Minute, 50 * time.Minute, time.Hour - time.Second} {
		*now = t0.Add(at)
		_, err := store.ReadCredential(context.Background(), configDir)
		if !errors.Is(err, ErrCredentialUnchecked) || errors.Is(err, ErrCredentialUnavailable) {
			t.Fatalf("hung for %s: %v, want still unchecked (a hang gets the hour, not the quarter hour)", at, err)
		}
	}

	*now = t0.Add(time.Hour)
	credential, err := store.ReadCredential(context.Background(), configDir)
	if credential != nil || !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("hung for an hour over 9 reads: credential %v err %v, want ErrCredentialUnavailable", credential != nil, err)
	}
	// The verdict must not read as a cut anywhere: callers test the
	// sentinels, but the proxy's sweepCut and the peers' unmeasured test
	// also match context text, and would publish this verdict as a cut.
	for _, text := range []string{"not checked", context.DeadlineExceeded.Error(), context.Canceled.Error()} {
		if strings.Contains(err.Error(), text) {
			t.Fatalf("hang verdict %q carries %q, which reads as transient", err, text)
		}
	}
	if !strings.Contains(err.Error(), "no access token") {
		t.Fatalf("hang verdict %q: want the auth-shaped marker the status publisher reads", err)
	}

	*now = t0.Add(time.Hour + time.Minute)
	if _, _, err := store.RefreshCredentialIfExpired(context.Background(), nil, profile); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("refresh past the hang bound: %v, want the verdict", err)
	}
}

// A hang alone says nothing about the store, which is why it gets the hour;
// a refusal says the store is refusing. A streak that has had one refused
// read is held to the quarter hour, however many hangs came first -- a
// quarter hour of refusals, dated from the first one (review r4, R4-3).
func TestAHangingKeychainThatAlsoRefusesGetsTheQuarterHour(t *testing.T) {
	never := neverAnswers(t)
	swapKeychain(t, securityKeychain{})
	store, profile := keychainOnlyProfile(t, securityKeychain{}, "work", nil)
	refuse := filepath.Join(t.TempDir(), "refuse")
	securityShim(t, "if [ -e '"+refuse+"' ]; then exit 36; fi\n"+never)
	setSecurityReadTimeout(t, 100*time.Millisecond)
	configDir := store.ClaudeConfigDir(profile.Name)
	t0 := time.Now()
	now := withSilenceClock(t, t0)

	for _, at := range []time.Duration{0, 5 * time.Minute, 10 * time.Minute} {
		*now = t0.Add(at)
		if _, err := store.ReadCredential(context.Background(), configDir); !errors.Is(err, ErrCredentialUnchecked) || errors.Is(err, ErrCredentialUnavailable) {
			t.Fatalf("hung for %s: %v, want still unchecked", at, err)
		}
	}
	if err := os.WriteFile(refuse, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The refusal has to reach the read as a refusal. On the 100 ms clock the
	// hangs above need, `exit 36` raced that clock and lost under load (see
	// answerBeforeTheClock); with the clock out of reach, only the shim's
	// answer can end this read.
	answerBeforeTheClock(t)
	// The first refusal, a quarter hour into the streak, starts the quarter
	// hour; it is not already over.
	for _, at := range []time.Duration{15 * time.Minute, 20 * time.Minute, 29 * time.Minute} {
		*now = t0.Add(at)
		if _, err := store.ReadCredential(context.Background(), configDir); !errors.Is(err, ErrCredentialUnchecked) || errors.Is(err, ErrCredentialUnavailable) {
			t.Fatalf("refused since 15m, now %s: %v, want still unchecked (the quarter hour runs from the first refusal)", at, err)
		}
	}
	*now = t0.Add(30 * time.Minute)
	_, err := store.ReadCredential(context.Background(), configDir)
	if !errors.Is(err, ErrCredentialUnavailable) || !strings.Contains(err.Error(), "exit status 36") {
		t.Fatalf("a quarter hour of refusals after a hung streak: %v, want the verdict naming the refusal", err)
	}
}

// Review r4, R4-3: fifty minutes of load-shaped hangs and then ONE read the
// keychain refused used to be a verdict at once: the quarter hour was
// measured from the first hang, so it was long over, and every warm session
// on the account moved. The quarter hour is dated from the first refusal, and
// the hour still from the first hang, so the verdict comes at whichever bound
// ends first. (R4-3 named a `security` killed by jetsam and a fork that got
// EAGAIN as the other failure; since PATCHES §34.6 those are not refusals but
// a `security` that never ran, bounded like a hang --
// TestAForkThatFailedUnderLoadIsBoundedLikeAHang,
// TestASecurityThatCannotBeRunIsLoadNotARefusal.)
func TestOneOtherFailureAfterAHangStreakIsNotAnInstantVerdict(t *testing.T) {
	hang := fmt.Errorf("%w within %s (signal: killed)", errSecurityReadHung, 5*time.Second)
	for _, other := range []struct {
		name string
		err  error
	}{
		{"the keychain is locked (exit 36)", errors.New("exit status 36")},
		{"access denied (exit 51)", errors.New("exit status 51")},
	} {
		t.Run(other.name, func(t *testing.T) {
			read := func(t *testing.T, kc *cutKeychain, store Store, dir string, failure error) error {
				t.Helper()
				kc.mu.Lock()
				kc.cut = failure
				kc.mu.Unlock()
				_, err := store.ReadCredential(context.Background(), dir)
				return err
			}
			stillUnchecked := func(t *testing.T, err error, what string) {
				t.Helper()
				if !errors.Is(err, ErrCredentialUnchecked) || errors.Is(err, ErrCredentialUnavailable) {
					t.Fatalf("%s: %v, want still unchecked", what, err)
				}
			}

			t.Run("the hour from the first hang ends first", func(t *testing.T) {
				kc := &cutKeychain{fakeKeychain: newFakeKeychain(), failFor: map[string]bool{}, cut: hang}
				swapKeychain(t, kc)
				store, _ := keychainOnlyProfile(t, kc, "slow",
					credentialJSON(t, "slow-access", "slow-refresh", time.Now().Add(time.Hour)))
				dir := store.ClaudeConfigDir("slow")
				t0 := time.Now()
				now := withSilenceClock(t, t0)
				kc.failEveryRead()
				for _, at := range []time.Duration{0, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute, 40 * time.Minute, 50 * time.Minute} {
					*now = t0.Add(at)
					stillUnchecked(t, read(t, kc, store, dir, hang), fmt.Sprintf("hung for %s", at))
				}
				*now = t0.Add(50 * time.Minute)
				stillUnchecked(t, read(t, kc, store, dir, other.err), "fifty minutes of hangs, then one other failure")
				*now = t0.Add(55 * time.Minute)
				stillUnchecked(t, read(t, kc, store, dir, other.err), "five minutes after it")
				*now = t0.Add(time.Hour)
				if err := read(t, kc, store, dir, hang); !errors.Is(err, ErrCredentialUnavailable) {
					t.Fatalf("an hour with no answer: %v, want the verdict (the hour runs from the first hang)", err)
				}
			})

			t.Run("the quarter hour from the other failure ends first", func(t *testing.T) {
				kc := &cutKeychain{fakeKeychain: newFakeKeychain(), failFor: map[string]bool{}, cut: hang}
				swapKeychain(t, kc)
				store, _ := keychainOnlyProfile(t, kc, "slow",
					credentialJSON(t, "slow-access", "slow-refresh", time.Now().Add(time.Hour)))
				dir := store.ClaudeConfigDir("slow")
				t0 := time.Now()
				now := withSilenceClock(t, t0)
				kc.failEveryRead()
				for _, at := range []time.Duration{0, 10 * time.Minute} {
					*now = t0.Add(at)
					stillUnchecked(t, read(t, kc, store, dir, hang), fmt.Sprintf("hung for %s", at))
				}
				for _, at := range []time.Duration{20 * time.Minute, 25 * time.Minute, 34 * time.Minute} {
					*now = t0.Add(at)
					stillUnchecked(t, read(t, kc, store, dir, other.err), fmt.Sprintf("failing since 20m, now %s", at))
				}
				*now = t0.Add(35 * time.Minute)
				if err := read(t, kc, store, dir, other.err); !errors.Is(err, ErrCredentialUnavailable) {
					t.Fatalf("a quarter hour of other failures after two hangs: %v, want the verdict", err)
				}
			})
		})
	}
}

// A credentials file the file system refuses, and will refuse on every retry,
// is an answer: the account cannot serve until someone fixes the file. The box
// daemon runs as User=subrouter and a root-run write renames a root-owned 0600
// file into place (EACCES). It is a verdict at once, not fifteen minutes of
// 502s, and a keychain copy beside it is still not consulted.
func TestACredentialsFileTheFileSystemRefusesIsAVerdict(t *testing.T) {
	noSecurityOnPath(t)
	refusals := map[string]func(t *testing.T, path string){
		"a directory (EISDIR)": func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"a symlink loop (ELOOP)": func(t *testing.T, path string) {
			if err := os.Symlink(filepath.Base(path), path); err != nil {
				t.Fatal(err)
			}
		},
		"a path through a file (ENOTDIR)": func(t *testing.T, path string) {
			regular := filepath.Join(filepath.Dir(path), "regular")
			if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(regular, "credentials.json"), path); err != nil {
				t.Fatal(err)
			}
		},
		"no read permission (EACCES)": func(t *testing.T, path string) {
			if runtime.GOOS == "windows" || os.Geteuid() == 0 {
				t.Skip("root reads a 0000 file; run as a user to exercise EACCES")
			}
			if err := os.WriteFile(path, credentialJSON(t, "file-access", "file-refresh", time.Now().Add(time.Hour)), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0); err != nil {
				t.Fatal(err)
			}
		},
	}
	for refusal, arrange := range refusals {
		for vault, withKeychainCopy := range map[string]bool{"vault-less host": false, "keychain copy beside it": true} {
			t.Run(refusal+", "+vault, func(t *testing.T) {
				if runtime.GOOS == "windows" {
					t.Skip("POSIX errnos")
				}
				var kc keychain = unavailableKeychain{}
				if withKeychainCopy {
					kc = newFakeKeychain()
				}
				swapKeychain(t, kc)
				store, profile := keychainOnlyProfile(t, kc, "work", nil)
				configDir := store.ClaudeConfigDir(profile.Name)
				if withKeychainCopy {
					if err := kc.write(context.Background(), wantKeychainService(configDir), currentUsername(t),
						credentialJSON(t, "keychain-access", "keychain-refresh", time.Now().Add(time.Hour))); err != nil {
						t.Fatal(err)
					}
				}
				arrange(t, filepath.Join(configDir, ".credentials.json"))

				credential, err := store.ReadCredential(context.Background(), configDir)
				if credential != nil || !errors.Is(err, ErrCredentialUnavailable) || errors.Is(err, ErrCredentialUnchecked) {
					t.Fatalf("refused credentials file: credential %v err %v, want ErrCredentialUnavailable and not unchecked", credential != nil, err)
				}
				_, _, err = store.RefreshCredentialIfExpired(context.Background(), nil, profile)
				if !errors.Is(err, ErrCredentialUnavailable) || errors.Is(err, ErrCredentialUnchecked) || !strings.Contains(err.Error(), "no access token") {
					t.Fatalf("refresh over a refused credentials file: %v", err)
				}
			})
		}
	}
}

// credentialFileRefused names only errnos the file system repeats until
// someone changes the file. Anything else -- the load-shaped errnos, and any
// errno nobody anticipated -- stays unchecked and gets the bounded doubt.
func TestCredentialFileRefusedNamesOnlyErrnosThatRepeat(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX errnos")
	}
	for errno, refused := range map[syscall.Errno]bool{
		syscall.EACCES: true, syscall.EPERM: true, syscall.EISDIR: true, syscall.ENOTDIR: true, syscall.ELOOP: true,
		syscall.EMFILE: false, syscall.ENFILE: false, syscall.EIO: false, syscall.EAGAIN: false,
		syscall.EINTR: false, syscall.ENOMEM: false, syscall.ENXIO: false,
	} {
		err := &fs.PathError{Op: "open", Path: "/x/.credentials.json", Err: errno}
		if got := credentialFileRefused(err); got != refused {
			t.Errorf("%v: refused = %v, want %v", errno, got, refused)
		}
	}
}

// The rotation re-read after an exchange must persist whatever the file says:
// the provider has already rotated, and `refreshed` is the only live copy. A
// root-run write that lands during the exchange leaves an EACCES file; the
// rename that persists replaces it with one the daemon owns.
func TestRefreshPersistsTheRotationWhenTheReReadIsRefused(t *testing.T) {
	noSecurityOnPath(t)
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file; run as a user to exercise EACCES")
	}
	swapKeychain(t, unavailableKeychain{})
	store, profile := keychainOnlyProfile(t, unavailableKeychain{}, "work", nil)
	path := filepath.Join(store.ClaudeConfigDir(profile.Name), ".credentials.json")
	if err := os.WriteFile(path, credentialJSON(t, "expired-access", "about-to-be-spent", time.Now().Add(-time.Hour)), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		response, err := (&slowRefreshTransport{}).RoundTrip(req)
		if chmodErr := os.Chmod(path, 0); chmodErr != nil {
			t.Error(chmodErr)
		}
		return response, err
	})}

	account, didRefresh, err := store.RefreshCredentialIfExpired(context.Background(), client, profile)
	if err != nil {
		t.Fatalf("refresh with a refused re-read: %v (the rotated credential was dropped)", err)
	}
	if !didRefresh || account.Token != "minted-access" {
		t.Fatalf("account token %q didRefresh %v, want the minted credential", account.Token, didRefresh)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("credentials file after the persist: %v, want readable again", err)
	}
	defer file.Close()
	body, _ := io.ReadAll(file)
	if !strings.Contains(string(body), "minted-refresh") {
		t.Fatalf("credentials file holds %q, want the rotated refresh token", body)
	}
}

// A lock ReadCredential or the refresh takes before reading is part of the
// read (credentialLockFailure). A lock file that cannot be opened -- ENFILE on
// eleven-alpha.credentials.lock, 2026-09-06 02:45 -- used to come back raw,
// ahead of every classifier: the reload dropped the account (a false
// "removed", which moves every session on it), and the pick path answered 503
// every turn without ever evicting it. Now it is unchecked like any read that
// got no answer, bounded like one, and kept on reload; a lock the file system
// refuses is a verdict; a wait the caller's clock cut is a cut.
func TestACredentialLockThatCannotBeTakenIsTheReadFailing(t *testing.T) {
	for _, lock := range []struct {
		name string
		path func(string) string
		// listed: ListAccountsKeeping reads through this lock too.
		listed bool
	}{
		{"the credential lock", profileCredentialLockPath, true},
		{"the refresh lock", profileRefreshLockPath, false},
	} {
		t.Run(lock.name+" does not answer: unchecked, kept, bounded", func(t *testing.T) {
			kc := newFakeKeychain()
			swapKeychain(t, kc)
			store, profile := keychainOnlyProfile(t, kc, "work",
				credentialJSON(t, "live-access", "live-refresh", time.Now().Add(time.Hour)))
			configDir := store.ClaudeConfigDir(profile.Name)
			t0 := time.Now()
			now := withSilenceClock(t, t0)
			// A symlink to a socket: open(2) fails ENXIO (Linux) or EOPNOTSUPP
			// (darwin), outside the refused set -- the stand-in for ENFILE.
			credentialsFileThatDoesNotAnswer(t, lock.path(configDir))

			unchecked := func(what string, err error) {
				t.Helper()
				if !errors.Is(err, ErrCredentialUnchecked) || errors.Is(err, ErrCredentialUnavailable) {
					t.Fatalf("%s: %v, want unchecked", what, err)
				}
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					t.Fatalf("%s: %v reads as a cut, which never counts: a lock that cannot be opened must reach the bound", what, err)
				}
				if strings.Contains(err.Error(), configDir) || strings.Contains(err.Error(), "no access token") {
					t.Fatalf("%s: %q carries the path or a dead-login marker", what, err)
				}
			}
			for _, at := range []time.Duration{0, 5 * time.Minute, 10 * time.Minute} {
				*now = t0.Add(at)
				_, _, err := store.RefreshCredentialIfExpired(context.Background(), nil, profile)
				unchecked(fmt.Sprintf("refresh at +%s", at), err)
				if lock.listed {
					_, err := store.ReadCredential(context.Background(), configDir)
					unchecked(fmt.Sprintf("read at +%s", at), err)
				}
			}
			if lock.listed {
				kept := accounts.Account{ID: "work", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth,
					Token: "live-access", Source: configDir}
				listed, err := store.ListAccountsKeeping(context.Background(), func(id string) (accounts.Account, bool) {
					return kept, id == "work"
				})
				if err != nil || len(listed) != 1 || listed[0].Token != "live-access" {
					t.Fatalf("reload over a lock that does not answer: %+v err %v, want the account kept", listed, err)
				}
			}
			*now = t0.Add(15 * time.Minute)
			_, _, err := store.RefreshCredentialIfExpired(context.Background(), nil, profile)
			if !errors.Is(err, ErrCredentialUnavailable) || errors.Is(err, ErrCredentialUnchecked) || !strings.Contains(err.Error(), "no access token") {
				t.Fatalf("a lock that has not answered for a quarter hour: %v, want the verdict", err)
			}
		})
	}

	t.Run("a lock the file system refuses is a verdict", func(t *testing.T) {
		kc := newFakeKeychain()
		swapKeychain(t, kc)
		store, profile := keychainOnlyProfile(t, kc, "work",
			credentialJSON(t, "live-access", "live-refresh", time.Now().Add(time.Hour)))
		configDir := store.ClaudeConfigDir(profile.Name)
		path := profileCredentialLockPath(configDir)
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil { // EISDIR on open(O_RDWR)
			t.Fatal(err)
		}
		_, err := store.ReadCredential(context.Background(), configDir)
		if !errors.Is(err, ErrCredentialUnavailable) || errors.Is(err, ErrCredentialUnchecked) || strings.Contains(err.Error(), configDir) {
			t.Fatalf("refused credential lock: %v, want ErrCredentialUnavailable without the path", err)
		}
	})

	t.Run("a lock wait the caller's clock cut is a cut", func(t *testing.T) {
		kc := newFakeKeychain()
		swapKeychain(t, kc)
		store, profile := keychainOnlyProfile(t, kc, "work",
			credentialJSON(t, "live-access", "live-refresh", time.Now().Add(time.Hour)))
		configDir := store.ClaudeConfigDir(profile.Name)
		// Held for the whole read: the caller's clock is the only way out.
		held, err := lockProfileCredential(context.Background(), configDir)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err = store.ReadCredential(ctx, configDir)
		if !errors.Is(err, ErrCredentialUnchecked) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("lock wait cut by the caller: %v, want an unchecked cut", err)
		}
	})
}

// CredentialStoreNotAnswering is what the proxy reads to keep NEW sessions off
// a store that is not answering. It holds from the first counted failure --
// long before any verdict -- until a read answers or the silence goes stale
// (credentialSilenceMaxGap), is per profile, and a read a clock cut never
// makes it true: load alone must not shrink the pool new sessions see.
func TestCredentialStoreNotAnsweringFollowsTheStreak(t *testing.T) {
	kc := &cutKeychain{fakeKeychain: newFakeKeychain(), failFor: map[string]bool{}, cut: errors.New("exit status 36")}
	swapKeychain(t, kc)
	store, _ := keychainOnlyProfile(t, kc, "silent",
		credentialJSON(t, "silent-access", "silent-refresh", time.Now().Add(time.Hour)))
	if _, err := store.CreateProfile("other"); err != nil {
		t.Fatal(err)
	}
	if err := kc.write(context.Background(), wantKeychainService(store.ClaudeConfigDir("other")), currentUsername(t),
		credentialJSON(t, "other-access", "other-refresh", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	silent, other := store.ClaudeConfigDir("silent"), store.ClaudeConfigDir("other")
	t0 := time.Now()
	now := withSilenceClock(t, t0)
	read := func(dir string) {
		t.Helper()
		_, _ = store.ReadCredential(context.Background(), dir)
	}

	read(silent)
	if CredentialStoreNotAnswering(silent) {
		t.Fatal("a store that answered reads as not answering")
	}
	kc.mu.Lock()
	kc.failFor[wantKeychainService(silent)] = true
	kc.cut = errors.Join(errors.New("security find-generic-password"), context.DeadlineExceeded, errors.New("signal: killed"))
	kc.mu.Unlock()
	read(silent)
	if CredentialStoreNotAnswering(silent) {
		t.Fatal("a read a clock cut made the store read as not answering: load alone would shrink the pool")
	}
	kc.mu.Lock()
	kc.cut = errors.New("exit status 36")
	kc.mu.Unlock()
	read(silent)
	if !CredentialStoreNotAnswering(silent) {
		t.Fatal("one read that got no answer: want the store read as not answering at once, before any verdict")
	}
	read(other)
	if CredentialStoreNotAnswering(other) || !CredentialStoreNotAnswering(silent) {
		t.Fatal("the streak is per profile: another profile's answer neither inherits nor ends it")
	}
	*now = t0.Add(credentialSilenceMaxGap + time.Second)
	if CredentialStoreNotAnswering(silent) {
		t.Fatal("a silence older than credentialSilenceMaxGap still steers new sessions away")
	}
	read(silent)
	if !CredentialStoreNotAnswering(silent) {
		t.Fatal("a fresh failure after a stale silence: want not answering again")
	}
	kc.mu.Lock()
	kc.failFor[wantKeychainService(silent)] = false
	kc.mu.Unlock()
	read(silent)
	if CredentialStoreNotAnswering(silent) {
		t.Fatal("the store answered: want it placeable again")
	}
}

// A read a clock cut says the host was slow, nothing about the store: it
// neither starts, extends nor ends a silent streak. If it ENDED one, a locked
// keychain whose reads are sometimes cut under load -- the laptop at load 130
// to 260 -- would restart its quarter hour at every cut and stay transient
// forever. If it EXTENDED one, cuts would bridge the gap between two failures
// far apart and make them one sustained silence.
func TestACutReadNeitherEndsNorExtendsASilentStreak(t *testing.T) {
	refusal := errors.New("exit status 36")
	cut := errors.Join(errors.New("security find-generic-password"), context.DeadlineExceeded, errors.New("signal: killed"))
	setup := func(t *testing.T) (*cutKeychain, Store, string, *time.Time, time.Time) {
		t.Helper()
		kc := &cutKeychain{fakeKeychain: newFakeKeychain(), cut: refusal}
		swapKeychain(t, kc)
		store, profile := keychainOnlyProfile(t, kc, "work",
			credentialJSON(t, "live-access", "live-refresh", time.Now().Add(time.Hour)))
		t0 := time.Now()
		now := withSilenceClock(t, t0)
		kc.failEveryRead()
		return kc, store, store.ClaudeConfigDir(profile.Name), now, t0
	}
	readAt := func(t *testing.T, kc *cutKeychain, store Store, configDir string, now *time.Time, at time.Time, with error) error {
		t.Helper()
		kc.mu.Lock()
		kc.cut = with
		kc.mu.Unlock()
		*now = at
		_, err := store.ReadCredential(context.Background(), configDir)
		return err
	}

	t.Run("a cut does not end the streak", func(t *testing.T) {
		kc, store, configDir, now, t0 := setup(t)
		for _, step := range []struct {
			at   time.Duration
			with error
		}{{0, refusal}, {5 * time.Minute, refusal}, {7 * time.Minute, cut}, {10 * time.Minute, refusal}} {
			err := readAt(t, kc, store, configDir, now, t0.Add(step.at), step.with)
			if !errors.Is(err, ErrCredentialUnchecked) || errors.Is(err, ErrCredentialUnavailable) {
				t.Fatalf("read at +%s: %v, want unchecked", step.at, err)
			}
		}
		// Counted from the first refusal at +0, not from after the cut.
		if err := readAt(t, kc, store, configDir, now, t0.Add(15*time.Minute), refusal); !errors.Is(err, ErrCredentialUnavailable) {
			t.Fatalf("a quarter hour of refusals with one cut among them: %v, want the verdict (the cut ended the streak)", err)
		}
	})

	t.Run("a cut does not extend the streak", func(t *testing.T) {
		kc, store, configDir, now, t0 := setup(t)
		// One refusal, then only a cut until +13m: the gap between the two
		// counted failures is past credentialSilenceMaxGap, so the streak
		// starts over at +13m, whatever was cut in between.
		gapEnd := credentialSilenceMaxGap + time.Minute
		for _, step := range []struct {
			at   time.Duration
			with error
		}{{0, refusal}, {credentialSilenceMaxGap - time.Minute, cut}, {gapEnd, refusal}} {
			if err := readAt(t, kc, store, configDir, now, t0.Add(step.at), step.with); errors.Is(err, ErrCredentialUnavailable) {
				t.Fatalf("read at +%s: %v, want unchecked", step.at, err)
			}
		}
		if err := readAt(t, kc, store, configDir, now, t0.Add(15*time.Minute), refusal); errors.Is(err, ErrCredentialUnavailable) {
			t.Fatalf("three refusals, two of them after a %s hole bridged only by a cut: %v, want unchecked (the cut extended the streak)", gapEnd, err)
		}
	})
}

// A streak belongs to one profile (its instance path). Another profile's
// answers must not end it, and another profile's first failure must not
// inherit it: with one streak for every profile, a healthy account's reads
// would keep a locked one transient forever, or a single failure on a healthy
// one would read as a quarter hour of silence and move its sessions.
func TestSilenceStreakIsPerProfile(t *testing.T) {
	kc := &cutKeychain{fakeKeychain: newFakeKeychain(), failFor: map[string]bool{}, cut: errors.New("exit status 36")}
	swapKeychain(t, kc)
	store, _ := keychainOnlyProfile(t, kc, "locked",
		credentialJSON(t, "locked-access", "locked-refresh", time.Now().Add(time.Hour)))
	if _, err := store.CreateProfile("healthy"); err != nil {
		t.Fatal(err)
	}
	if err := kc.write(context.Background(), wantKeychainService(store.ClaudeConfigDir("healthy")), currentUsername(t),
		credentialJSON(t, "healthy-access", "healthy-refresh", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	locked, healthy := store.ClaudeConfigDir("locked"), store.ClaudeConfigDir("healthy")
	kc.failFor[wantKeychainService(locked)] = true
	t0 := time.Now()
	now := withSilenceClock(t, t0)

	for _, at := range []time.Duration{0, 5 * time.Minute, 10 * time.Minute} {
		*now = t0.Add(at)
		if _, err := store.ReadCredential(context.Background(), locked); !errors.Is(err, ErrCredentialUnchecked) {
			t.Fatalf("locked at +%s: %v, want unchecked", at, err)
		}
		if credential, err := store.ReadCredential(context.Background(), healthy); err != nil || credential == nil {
			t.Fatalf("healthy at +%s: %v", at, err)
		}
	}
	*now = t0.Add(15 * time.Minute)
	if _, err := store.ReadCredential(context.Background(), locked); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("locked for a quarter hour while another profile answered: %v, want the verdict (another profile's answers ended its streak)", err)
	}
	kc.mu.Lock()
	kc.failFor[wantKeychainService(healthy)] = true
	kc.mu.Unlock()
	if _, err := store.ReadCredential(context.Background(), healthy); !errors.Is(err, ErrCredentialUnchecked) || errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("the healthy profile's first failure: %v, want unchecked (it inherited another profile's streak)", err)
	}
}

// A streak made only of own-clock hangs is what a loaded Mac produces on a
// read with no caller deadline, and load alone must never take an account out
// of new placements (review r3, F2: one 5 s hang did, and with a spent peer
// that refused every new session). The hangs still build toward the hour-long
// verdict; the first read the store REFUSES makes the account unplaceable at
// once, and an answer ends it all.
func TestAHangOnlyStreakDoesNotSteerNewSessions(t *testing.T) {
	hung := fmt.Errorf("%w within %s (signal: killed)", errSecurityReadHung, 5*time.Second)
	kc := &cutKeychain{fakeKeychain: newFakeKeychain(), failFor: map[string]bool{}, cut: hung}
	swapKeychain(t, kc)
	store, _ := keychainOnlyProfile(t, kc, "slow",
		credentialJSON(t, "slow-access", "slow-refresh", time.Now().Add(time.Hour)))
	dir := store.ClaudeConfigDir("slow")
	t0 := time.Now()
	now := withSilenceClock(t, t0)
	kc.failEveryRead()
	for i := 0; i < 5; i++ {
		*now = t0.Add(time.Duration(i) * time.Minute)
		if _, err := store.ReadCredential(context.Background(), dir); !errors.Is(err, ErrCredentialUnchecked) || !errors.Is(err, errSecurityReadHung) {
			t.Fatalf("hung read %d: %v, want an unchecked hang", i, err)
		}
		if CredentialStoreNotAnswering(dir) {
			t.Fatalf("after %d own-clock hang(s) the store reads as not answering: load alone steers new sessions away", i+1)
		}
	}
	kc.mu.Lock()
	kc.cut = errors.New("exit status 36")
	kc.mu.Unlock()
	*now = t0.Add(5 * time.Minute)
	if _, err := store.ReadCredential(context.Background(), dir); !errors.Is(err, ErrCredentialUnchecked) {
		t.Fatalf("refused read: %v, want unchecked", err)
	}
	if !CredentialStoreNotAnswering(dir) {
		t.Fatal("a refusal in the streak: want the store read as not answering at once")
	}
}
