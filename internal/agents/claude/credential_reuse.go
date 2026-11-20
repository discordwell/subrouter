package claude

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// keychainAnswerReuse is how long a credential the keychain gave is served
// again without asking `security`, while its access token needs no refresh.
//
// Every Claude turn, and every account in every score sweep, went through
// refreshProfileCredential, which took the profile's refresh lock and then its
// credential lock and ran `security find-generic-password` under both -- to
// learn, nearly every time, that the token it already had was still good. On
// a loaded Mac that read takes up to its five-second clock, and the locks
// queue everything else behind it. The laptop, 2026-09-28 (five hours):
// 313 `account reload refresh failed`, 240 of them a sweep that ran
// out of its 5 s pool budget waiting for a profile's lock and 70 a `security`
// read killed, 16 sweeps that measured nothing fresh, and warm turns whose
// own read ran out its clock sent to the box for the turn, prompt cache cold
// (errRefreshTransient -> serveAcrossPeers).
//
// Only a keychain-only profile is served this way (readCredential remembers
// an answer only when there is no credentials file, and a file that has
// appeared since ends the reuse). A profile with a file keeps reading on
// every call, as the box's always have -- on this host that read still runs
// `security`, to compare the file with the item beside it -- and so does one
// with a rotation waiting to be adopted (rotationRescueFile).
//
// What keeps it honest:
//   - Only an ANSWER is reused: a read that came back with a credential, or a
//     write this process made. Any read that did not answer with a keychain
//     credential -- a cut, a hang, a refusal, a verdict, a genuine absence,
//     a credentials file -- forgets the answer, so the next caller
//     reads, and the silent-store streak and its verdict
//     (observeCredentialRead) see exactly the reads they saw before, only
//     fewer of them while the store answers.
//   - A token inside the refresh window is never reused; that caller takes the
//     refresh lock and reads, as every caller used to. Reuse never feeds an
//     exchange, so a reused credential can never present a spent refresh
//     token.
//   - A forced refresh never reuses, and while this process is rotating a
//     profile's credential (from the exchange to the end of its persist) no
//     caller reuses that profile's answer: the token it holds is the one
//     being replaced. Such a caller queues on the refresh lock and reads the
//     rotation, as every caller did before.
//   - Writes this process makes update or forget the answer: a keychain
//     write that landed replaces it, a failed one, a file write or a keychain
//     delete forgets it (writeCredential, deleteKeychainCredentialContext),
//     and so does a lock that could not be taken for a read. A change made by
//     another process -- Claude Code rotating the item itself, an `sr claude
//     addr` re-auth in place -- is seen at most keychainAnswerReuse late, or
//     at once by any read in between (a reload), which replaces the answer.
//   - An answer is served only to the profile it was read for: the locked
//     path stamps it with the profile's CreatedAt (stampKeychainAnswer), and
//     a profile removed and added again under the same name -- by another
//     process, whose forgetting this one never sees -- does not match.
//   - A credential the provider refused (a 401, which the proxy marks) is
//     forgotten at the mark (ForgetKeychainAnswer), so the turns after it read
//     the store -- where a rotation another process made shows up -- as they
//     did before this existed.
//
// A var so a test can move the window.
var keychainAnswerReuse = 60 * time.Second

type keychainAnswer struct {
	credential CredentialInfo
	at         time.Time
	// createdAt is the CreatedAt of the profile the answer was read for,
	// stamped by the locked path; "" until then, and an unstamped answer is
	// never reused.
	createdAt string
}

// keychainAnswers is process-wide for the reason credentialSilence is: Store
// is a value type, copied into every caller. Keys are instance paths.
// rotating counts this process's rotations in flight per instance path
// (keychainRotationStarted).
var keychainAnswers = struct {
	sync.Mutex
	entries  map[string]keychainAnswer
	rotating map[string]int
}{entries: map[string]keychainAnswer{}, rotating: map[string]int{}}

// keychainRotationStarted stops reuse of instancePath's answer until the
// returned func is called. refreshProfileCredential calls it before the
// exchange and ends it when the persist has returned; a read during the
// exchange (a reload) may remember the pre-rotation credential again, and
// this is what keeps a turn from being handed it.
func keychainRotationStarted(instancePath string) (ended func()) {
	key := filepath.Clean(instancePath)
	keychainAnswers.Lock()
	keychainAnswers.rotating[key]++
	keychainAnswers.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			keychainAnswers.Lock()
			if keychainAnswers.rotating[key]--; keychainAnswers.rotating[key] <= 0 {
				delete(keychainAnswers.rotating, key)
			}
			keychainAnswers.Unlock()
		})
	}
}

// rememberKeychainAnswer records what the keychain holds for instancePath;
// a nil or token-less credential forgets instead. The same credential read
// again keeps its stamp; any other starts unstamped.
func rememberKeychainAnswer(instancePath string, credential *CredentialInfo) {
	if credential == nil || credential.AccessToken == "" {
		forgetKeychainAnswer(instancePath)
		return
	}
	key := filepath.Clean(instancePath)
	keychainAnswers.Lock()
	answer := keychainAnswer{credential: *credential, at: time.Now()}
	if previous, ok := keychainAnswers.entries[key]; ok && previous.credential.Equal(*credential) {
		answer.createdAt = previous.createdAt
	}
	keychainAnswers.entries[key] = answer
	keychainAnswers.Unlock()
}

// stampKeychainAnswer records that instancePath's answer, when it is still
// credential, was read for the profile created at createdAt.
// refreshProfileCredential stamps what it read or wrote under its locks,
// where it has just found the profile current.
func stampKeychainAnswer(instancePath, createdAt string, credential *CredentialInfo) {
	if credential == nil {
		return
	}
	key := filepath.Clean(instancePath)
	keychainAnswers.Lock()
	if answer, ok := keychainAnswers.entries[key]; ok && answer.credential.Equal(*credential) {
		answer.createdAt = createdAt
		keychainAnswers.entries[key] = answer
	}
	keychainAnswers.Unlock()
}

func forgetKeychainAnswer(instancePath string) {
	keychainAnswers.Lock()
	delete(keychainAnswers.entries, filepath.Clean(instancePath))
	keychainAnswers.Unlock()
}

// ForgetKeychainAnswer drops what this process remembers of instancePath's
// keychain item, so the profile's next caller reads the store. The proxy calls
// it when the provider refuses the profile's credential: served on from
// memory, a token another process has since replaced would be refused on
// every turn of the window, where a read finds the replacement at once.
func ForgetKeychainAnswer(instancePath string) {
	if instancePath != "" {
		forgetKeychainAnswer(instancePath)
	}
}

// ProfileCredential is ReadCredential for a caller that only needs to know
// which credential the profile holds (the usage sweep's pre-read, which runs
// serially under the sweep's budget): a keychain answer reuseKeychainAnswer
// would serve is served the same way, without the locks or `security`, and
// anything else is read.
func (s Store) ProfileCredential(ctx context.Context, profile Profile) (*CredentialInfo, error) {
	configDir := s.claudeInstancePath(profile.Name)
	if _, credential, ok := s.reuseKeychainAnswer(profile, configDir); ok {
		return credential, nil
	}
	return s.ReadCredential(ctx, configDir)
}

// reuseKeychainAnswer is refreshProfileCredential's way around its locks: the
// account built from a keychain answer younger than keychainAnswerReuse, when
// that answer's access token needs no refresh and the profile still lives at
// configDir. ok=false sends the caller down the locked path.
func (s Store) reuseKeychainAnswer(profile Profile, configDir string) (accounts.Account, *CredentialInfo, bool) {
	now := time.Now()
	key := filepath.Clean(configDir)
	keychainAnswers.Lock()
	answer, found := keychainAnswers.entries[key]
	rotating := keychainAnswers.rotating[key] > 0
	keychainAnswers.Unlock()
	if !found || rotating || now.Sub(answer.at) >= keychainAnswerReuse {
		return accounts.Account{}, nil, false
	}
	// A credentials file makes the file the profile's store (readCredential):
	// from the moment one exists, however it got there, the profile is read
	// every time. A rescued rotation is taken back only by a read
	// (adoptRescuedRotation), and the answer here holds the token it replaced.
	for _, file := range []string{".credentials.json", rotationRescueFile} {
		if _, err := os.Lstat(filepath.Join(configDir, file)); !errors.Is(err, fs.ErrNotExist) {
			return accounts.Account{}, nil, false
		}
	}
	credential := answer.credential
	// An unknown expiry is read every time, as before: nothing here can say
	// the token is still good.
	if credential.ExpiresAt <= 0 || credentialExpiredAt(&credential, 60*time.Second, now) ||
		longLivedCredentialError(profile.Name, &credential, now) != nil {
		return accounts.Account{}, nil, false
	}
	current, ok := s.FindProfile(profile.Name)
	if !ok || answer.createdAt == "" || current.CreatedAt != answer.createdAt ||
		profileInstancePathKey(s.claudeInstancePath(current.Name)) != profileInstancePathKey(configDir) {
		return accounts.Account{}, nil, false
	}
	account, ok := profileAccount(current, configDir, &credential)
	if !ok {
		return accounts.Account{}, nil, false
	}
	return account, &credential, true
}
