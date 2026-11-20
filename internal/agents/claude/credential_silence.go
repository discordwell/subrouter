package claude

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrCredentialUnavailable is the verdict that ends ErrCredentialUnchecked's
// benefit of the doubt: this profile's credential cannot be read, and nothing
// will change that until someone fixes the store. The proxy treats it as a
// dead login -- the account is zeroed and its sessions move once, the forced
// switch the owner's P8 allows -- instead of the transient it would otherwise
// keep answering with a marked 502 forever.
//
// Two ways to reach it:
//
//   - The credentials file answered, and the answer is a refusal the file
//     system will repeat on every retry: EACCES/EPERM (the box daemon runs as
//     User=subrouter; a root-run write renames a root-owned 0600 file into
//     place), EISDIR, ENOTDIR, ELOOP (credentialFileRefused).
//   - The store stayed silent past credentialSilenceVerdictAfter, across at
//     least credentialSilenceVerdictReads reads, with no answer in between
//     (observeCredentialRead): a locked login keychain (exit 36 on every
//     read), a file that keeps failing with an errno outside the refused set
//     -- or past credentialHangVerdictAfter when every one of those reads was
//     a `security` that never answered (errSecurityReadHung, or
//     errSecurityReadNotRun when it never ran).
//
// Without it a store that can never answer was transient forever: the seed was
// carried, the pin was kept, the account was kept on every reload, and a new
// session was still placed on it, so every one of its turns was a 502 --
// measured 20 of 20 warm turns and 5 of 5 new sessions, where origin/main
// served them all from the next account.
//
// Its text carries "no access token" on purpose. The usage-status publisher
// decides by TEXT whether a failed row may be papered over with last-good
// windows (authLikeUsageError); an auth-shaped error may not, so the peer sees
// auth_valid:false, not a healthy row resurrected. The text carries no
// terminal marker ("unreadable credential", "no usable credential"), so the
// one-hour credential-failure hold does not apply: the first read that
// answers again brings the account back.
var ErrCredentialUnavailable = errors.New("no access token can be read")

// credentialSilenceVerdictAfter and credentialSilenceVerdictReads bound how
// long a store that keeps failing to answer is given the benefit of the doubt.
//
// Measured on the laptop 2026-09-28 01:22-03:36, at loads of 88 to 260: every
// false reading was a read the pool budget CUT, and cut reads never count
// here; the account's next read that got a slot answered. An account the
// proxy serves from is read on every turn, so a streak on it ends at its
// next turn. Fifteen minutes is far past any cut burst seen.
//
// The silence has to be SUSTAINED, which is what credentialSilenceMaxGap is
// for. The bound used to measure from a streak's first counted failure to its
// latest one and never look at the time between, so one failed read, a quiet
// gap, and one turn (a sweep read plus the pick's read: two more) crossed it
// -- probed: one exit 36, twenty quiet minutes, one turn, and the warm
// session was moved for good, `silent_for=20m0s reads=3`. Three reads alone
// do not say the store was silent for fifteen minutes; they say it failed
// three times. Every profile is read at least once per auto-switch sweep
// (--sr-switch-interval, 10 m on both daemons) and a served one on every
// turn, so a streak whose counted failures are further apart than that has a
// stretch in which every read was cut or none was taken -- no evidence of
// silence at all -- and it starts over at the next counted failure. (The gap
// is not a sleeping laptop: Go's monotonic clock, which Sub reads, stops
// while darwin sleeps.)
//
// credentialHangVerdictAfter is the bound for a streak made only of reads
// that ran out their own clock with the caller still waiting
// (errSecurityReadHung). Those have to count -- a keychain blocked behind a
// prompt on every read is otherwise transient forever -- but own-clock expiry
// is also what a loaded Mac produces, and the owner's rule is that load must
// keep the account. An hour of `security` never once answering within five
// seconds, on a profile read every turn and every sweep, is not load; any
// answer in that hour, from any caller, ends the streak. One refused read in
// the streak says the store itself is refusing, and the quarter hour applies
// -- measured from that refusal, never from the first hang. It used to be
// measured from the streak's start, so fifty minutes of load-shaped hangs and
// then one read that failed some other way was a verdict at once, "has not
// answered for 50m0s", and every warm session on the account was moved
// (review r4, R4-3). The hour still runs from the first hang, so a refusal
// can only bring the verdict nearer. A `security` that never ran to an answer
// with neither clock fired -- killed by jetsam, a fork that got EAGAIN -- is
// not a refusal but load, and is bounded like a hang (errSecurityReadNotRun,
// PATCHES §34.6).
var (
	credentialSilenceVerdictAfter = 15 * time.Minute
	credentialHangVerdictAfter    = time.Hour
	credentialSilenceVerdictReads = 3
	// credentialSilenceMaxGap is the longest stretch between two counted
	// failures that still reads as one silence: the 10-minute sweep cadence
	// plus slack for a sweep that ran late.
	credentialSilenceMaxGap = 12 * time.Minute
	// credentialSilenceNow is a var so a test can walk the streak through
	// fifteen minutes without waiting for them.
	credentialSilenceNow = time.Now
)

type credentialSilenceStreak struct {
	since time.Time
	// last is the latest counted failure: a streak whose next counted failure
	// comes more than credentialSilenceMaxGap after it starts over.
	last  time.Time
	reads int
	// hungOnly holds while every read counted in the streak was one
	// `security` never answered: a hang (errSecurityReadHung) or a read that
	// never ran (errSecurityReadNotRun; securityNeverAnswered). It only ever
	// goes from true to false, so the bound only ever shortens and a verdict,
	// once reached, stands.
	hungOnly bool
	// refusedSince and refusedReads date the quarter-hour bound: the first
	// counted failure that was not a hang, and the counted reads from it on.
	// Both are zero while hungOnly holds.
	refusedSince time.Time
	refusedReads int
	reported     bool
}

// credentialSilence is process-wide because Store is a value type, copied into
// every caller; a profile's streak has to survive whichever copy reads it.
// Keys are instance paths, so the set is bounded by the profiles ever read.
var credentialSilence = struct {
	sync.Mutex
	streaks map[string]*credentialSilenceStreak
}{streaks: map[string]*credentialSilenceStreak{}}

// observeCredentialRead is the time bound on ErrCredentialUnchecked. It sees
// the result of every ReadCredential and returns it unchanged, except once a
// profile's store has not answered for credentialSilenceVerdictAfter across
// credentialSilenceVerdictReads reads: from then on each such read returns
// ErrCredentialUnavailable, until a read answers.
//
// What counts:
//   - Any answer ends the streak: a credential, a genuine absence, a refusal,
//     a blob that will not parse.
//   - A read the caller's clock cut, or the caller cancelled
//     (DeadlineExceeded, Canceled), neither starts, extends nor ends a
//     streak. A cut read says the host was slow, not that the store refused
//     -- it is exactly the 2026-09-27/28 false reading, the pool's 5 s budget
//     -- so a budget that keeps cutting reads can never reach a verdict.
//   - A read that ran out its OWN clock with the caller still waiting
//     (errSecurityReadHung) counts, but a streak made only of those needs
//     credentialHangVerdictAfter, not the quarter hour: a keychain blocked on
//     every read must not be transient forever, and a slow host must not
//     lose its accounts to fifteen minutes of slow `security`. The quarter
//     hour, and its three reads, start at the streak's first failure that
//     was not a hang; the hour runs from its first failure of any kind.
//   - A read `security` never ran to an answer, with neither clock fired
//     (errSecurityReadNotRun: a fork that got EAGAIN or ENOMEM, a signal
//     nobody here sent, a missing tool), counts like a hang: the keychain
//     was never asked, and a Mac out of processes fails every profile's
//     fork at once.
//   - Every other unchecked read counts toward the quarter hour: `security`
//     that ran and exited other than 44, a credentials file failing with an
//     errno outside credentialFileRefused.
//   - A counted failure more than credentialSilenceMaxGap after the previous
//     one starts the streak over: the silence must be sustained, not two
//     failures far apart with nothing but cuts, or nothing, in between.
//
// It is applied in ReadCredential, which every listing, status row and
// refresh reads through. The re-read in writeRefreshedCredentialIfUnchanged
// stays outside it: that read follows an exchange the provider has already
// rotated, and it must persist the rotation whatever the store says.
func observeCredentialRead(instancePath string, err error) error {
	key := filepath.Clean(instancePath)
	if !errors.Is(err, ErrCredentialUnchecked) {
		credentialSilence.Lock()
		delete(credentialSilence.streaks, key)
		credentialSilence.Unlock()
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	now := credentialSilenceNow()
	credentialSilence.Lock()
	streak := credentialSilence.streaks[key]
	if streak == nil || now.Sub(streak.last) > credentialSilenceMaxGap {
		streak = &credentialSilenceStreak{since: now, hungOnly: true}
		credentialSilence.streaks[key] = streak
	}
	streak.last = now
	streak.reads++
	if !securityNeverAnswered(err) && streak.hungOnly {
		streak.hungOnly = false
		streak.refusedSince = now
	}
	if !streak.hungOnly {
		streak.refusedReads++
	}
	silent, reads := now.Sub(streak.since), streak.reads
	verdict := reads >= credentialSilenceVerdictReads && (silent >= credentialHangVerdictAfter ||
		(!streak.hungOnly && now.Sub(streak.refusedSince) >= credentialSilenceVerdictAfter &&
			streak.refusedReads >= credentialSilenceVerdictReads))
	firstVerdict := verdict && !streak.reported
	if firstVerdict {
		streak.reported = true
	}
	credentialSilence.Unlock()
	if !verdict {
		return err
	}
	cause := strings.TrimPrefix(err.Error(), ErrCredentialUnchecked.Error()+": ")
	if firstVerdict {
		slog.Error("Claude credential store has not answered; the profile cannot serve until it does",
			"instance", key, "silent_for", silent.Round(time.Second), "reads", reads, "error", cause)
	}
	// The cause is carried as text, never wrapped: it wraps
	// ErrCredentialUnchecked, and a verdict that still said "unchecked" would
	// be read as transient by every caller.
	return fmt.Errorf("%w: the credential store has not answered for %s (%d reads): %s",
		ErrCredentialUnavailable, silent.Round(time.Second), reads, cause)
}

// CredentialStoreNotAnswering reports whether a profile's credential store is
// in a live silent streak: its latest counted read got no answer, no read has
// answered since, and that failure is recent enough to still be part of one
// silence (credentialSilenceMaxGap). A read a clock cut never starts a streak,
// so load alone -- the 2026-09-27/28 false readings -- never makes this true.
//
// Nor does a streak made only of hangs (hungOnly: every counted read was a
// `security` that ran out its own five-second clock, errSecurityReadHung, or
// one that never ran to an answer, errSecurityReadNotRun).
// Own-clock expiry is exactly what a loaded Mac produces on the reads with no
// caller deadline -- a Background reload, the sweep's pre-read, a warm turn --
// and one of those used to be enough to take the account out of every new
// placement for up to credentialSilenceMaxGap. With a spent peer beside it
// that is the whole pool, so load alone produced the "no non-exhausted claude
// accounts" refusal P8 forbids (review r3, F2). A hang-only streak still
// counts toward its hour-long verdict (observeCredentialRead); until that
// verdict, or a read that is refused rather than slow, the account is offered
// as before. The first refusal in the streak clears hungOnly and this turns
// true at once.
//
// The proxy asks it one thing: whether to offer the account to a NEW session.
// An account in a streak keeps its score (the read reached no verdict), and a
// kept score used to keep attracting new sessions, each of which waited on the
// store and then answered 502 or went to the peer turn by turn -- probed, 5 of
// 5 new sessions over 55 minutes, with the next account idle. A new session
// has no cache to protect, so steering it elsewhere costs nothing; a warm one
// keeps its account (P8), and nothing here marks, zeroes or evicts. It is not
// a verdict and must never be read as one: ErrCredentialUnavailable is.
func CredentialStoreNotAnswering(instancePath string) bool {
	key := filepath.Clean(instancePath)
	now := credentialSilenceNow()
	credentialSilence.Lock()
	defer credentialSilence.Unlock()
	streak := credentialSilence.streaks[key]
	return streak != nil && !streak.hungOnly && now.Sub(streak.last) <= credentialSilenceMaxGap
}

// credentialFileRefused reports a credentials-file read failure the file
// system will give again on every retry until someone changes the file, its
// owner or its path. Those are answers, not silence. Everything else --
// EMFILE, ENFILE, EIO, EAGAIN, EINTR, ENOMEM, and any errno not named here --
// stays unchecked, and observeCredentialRead bounds how long it may last: an
// errno nobody anticipated gets fifteen minutes of doubt, not an immediate
// move of every session on the account.
func credentialFileRefused(err error) bool {
	return errors.Is(err, fs.ErrPermission) || // EACCES, EPERM
		errors.Is(err, syscall.EISDIR) ||
		errors.Is(err, syscall.ENOTDIR) ||
		errors.Is(err, syscall.ELOOP)
}
