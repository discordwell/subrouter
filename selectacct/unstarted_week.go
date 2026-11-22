package selectacct

import (
	"sync/atomic"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

// An unstarted week (PATCHES §86).
//
// A Codex weekly window starts on the account's first request and then counts
// down whether or not anyone spends. An account whose week has not started
// therefore holds a full week that the pool cannot spend until a session
// opens it, and every day it waits is a day of budget the pool never gets.
// The owner, 2026-11-22: "accounts that are at exactly 7 days need some
// traffic as soon as possible to start it." Weekly pace ranks such an account
// like one spending at exactly even pace, behind every account with unspent
// budget and behind every account in its last day, so it could sit unstarted
// for days.
//
// The comparator now gives the next new session to an account whose reading
// says its week has not started (Score.WeeklyUnstarted) and that carries no
// start mark, ahead of every account that can also take one. The clause sits
// below machine policy, the selection tier, the capacity marks, the
// new-session test and the machine-binding clauses, and above the endgame
// drain and everything below it, in Pick and PickSteady alike.
//
// Giving it that session (NoteNewSession) marks the account for
// StartSentMarkDuration. The mark removes the priority and nothing else, so
// the account ranks exactly as it would without §86: usage is re-read at most
// every two minutes, and without the mark every new session of those minutes
// would pile onto the one account. When the mark has expired and a reading
// still says unstarted, the priority is back, so a first request that never
// reached the provider is tried again.

// StartSentMarkDuration is how long the priority stays off an unstarted
// account after a new session was given to it. Long enough for a usage
// reading to see the clock running (one 2-minute window cache, one 30 s
// sweep, and the provider's own delay), short enough that a session that
// never reached the provider is retried soon.
const StartSentMarkDuration = 10 * time.Minute

// reasonStartsWeek is the clause's decision text.
const reasonStartsWeek = "its weekly window has not started; one new session starts its clock"

// startUnstartedWeekOff is the kill switch of PATCHES §86, inverted so the
// zero value is the shipped default: on. Off, the comparator ranks as before
// §86 and no start mark is set; Score.WeeklyUnstarted is still read and
// reported. The daemon sets it once at startup from
// SUBROUTER_START_UNSTARTED_WEEK.
var startUnstartedWeekOff atomic.Bool

// SetStartUnstartedWeek turns §86's priority on or off for this process. The
// daemon calls it once at startup, before the first scheduler is built.
func SetStartUnstartedWeek(on bool) { startUnstartedWeekOff.Store(!on) }

// StartUnstartedWeek reports whether §86's priority is on.
func StartUnstartedWeek() bool { return !startUnstartedWeekOff.Load() }

// NoteNewSession records that the router gave a new session to acct, ranked
// with scheduler: if that ranking read acct's week as unstarted, the account
// carries a start mark for StartSentMarkDuration (a later session extends
// it). Call it for every new session the router places, local or on a peer's
// account; it decides whether a mark is due, and reports whether it set one.
// A peer's account is marked under the ID the merged ranking gave it, so the
// mark is per peer.
func (r *SchedulerRef) NoteNewSession(scheduler Scheduler, acct account.Account) bool {
	if r == nil || acct.ID == "" || !StartUnstartedWeek() || !scheduler.score(acct.Provider, acct.ID).WeeklyUnstarted {
		return false
	}
	now := time.Now()
	key := ScoreKey(acct.Provider, acct.ID)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.startSentUntil == nil {
		r.startSentUntil = make(map[string]time.Time)
	}
	for k, until := range r.startSentUntil {
		if !until.After(now) {
			delete(r.startSentUntil, k)
		}
	}
	r.startSentUntil[key] = now.Add(StartSentMarkDuration)
	return true
}

// StartSentUntil reports when acct's start mark expires, while it stands: a
// status surface says the priority is off until then.
func (r *SchedulerRef) StartSentUntil(provider account.Provider, accountID string) (time.Time, bool) {
	if r == nil {
		return time.Time{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	until, ok := r.startSentUntil[ScoreKey(provider, accountID)]
	return until, ok && until.After(time.Now())
}

// WithStartMarks attaches the live start marks to a scheduler that was not
// read through Get, such as the bare one a ranking of peers' accounts alone
// starts from, so a peer's marked account loses its priority there too.
func (r *SchedulerRef) WithStartMarks(scheduler Scheduler) Scheduler {
	if r == nil {
		return scheduler
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if marks := r.startSentLocked(time.Now()); marks != nil {
		return scheduler.withStartSent(marks)
	}
	return scheduler
}

// startSentLocked copies the start marks still live at now, for Get. The
// caller holds r.mu.
func (r *SchedulerRef) startSentLocked(now time.Time) map[string]time.Time {
	var out map[string]time.Time
	for key, until := range r.startSentUntil {
		if !until.After(now) {
			continue
		}
		if out == nil {
			out = make(map[string]time.Time, len(r.startSentUntil))
		}
		out[key] = until
	}
	return out
}

// withStartSent attaches start marks keyed by ScoreKey. The map must not be
// mutated afterwards.
func (s Scheduler) withStartSent(marks map[string]time.Time) Scheduler {
	next := s
	next.startSent = marks
	return next
}

// startsWeek reports whether a new session on acct would start its week:
// the switch is on, its score reads the week as unstarted, and it carries no
// start mark live at now. A function of one account, so the clause keeps the
// comparator a strict weak ordering.
func (s Scheduler) startsWeek(acct account.Account, score Score, now time.Time) bool {
	return StartUnstartedWeek() && score.WeeklyUnstarted &&
		!s.startSent[ScoreKey(acct.Provider, acct.ID)].After(now)
}
