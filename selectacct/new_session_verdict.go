package selectacct

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/discordwell/subrouter/account"
)

// New-session verdicts (PATCHES §31).
//
// A daemon that ranks ANOTHER daemon's accounts must not re-derive whether one
// may take a new session. Eligibility is a burn rate over a window, a holdout,
// and an endgame concurrency taper counted on the owning daemon's live
// sessions -- and the live sessions are exactly what a peer cannot see. The
// 2026-09-27 incident was that re-derivation: the laptop read the box's chip-5
// as drain-eligible with zero live sessions (usage-status never published the
// box's count), sent it ~23 sessions, and the box -- which knew chip-5 was at
// its taper cap -- spread them onto accounts with nothing left in their 5h
// windows.
//
// So each daemon publishes its OWN answer, computed by the predicates Pick
// ranks by, and a peer consumes it verbatim. One implementation, read in two
// places.

// Verdict reasons. One vocabulary for the published verdict
// (usage-status normalized.new_session.reason) and the targeted-forward
// refusal (X-Subrouter-Target-Refused), so a log line on either daemon names
// the same clause the same way. The proxy adds the reasons only it can know
// (auth_invalid, unknown_account, machine_policy, not_oauth).
const (
	VerdictEligible       = "eligible"
	VerdictBurndown       = "burndown"
	VerdictExhausted      = "exhausted"
	VerdictHoldout        = "holdout"
	VerdictAtCap          = "at_cap"
	VerdictShortFloor     = "short_floor"
	VerdictBelowReserve   = "below_reserve"
	VerdictBurnProjection = "burn_projection"
	// VerdictQueueClosed is an account whose admission queue on some model
	// holds closed_queue waiters (PATCHES §72): no new session and no refugee
	// until it drains. Its warm sessions stay and queue.
	VerdictQueueClosed = "queue_closed"
	// VerdictChosen is an account its holder chose to run to its limit for
	// its next saved reset (PATCHES §95.2), below its allowance: it takes a
	// new session below the short floor.
	VerdictChosen = "chosen"
	// VerdictPinOnly is an account that takes pinned sessions only (PATCHES
	// §98, SUBROUTER_PIN_ONLY_ACCOUNTS): no new session, refugee or failover
	// from the pool, here or from a peer. A pin by name is still served.
	VerdictPinOnly = "pin_only"
)

// NewSessionVerdict is one daemon's answer, for one of its own accounts, to
// "may a peer place a NEW session here right now?", plus the facts a peer needs
// to rank it the way this daemon would.
//
// Every field but BurndownCause is always serialised: false is an answer. A
// peer that finds the whole object absent is talking to a daemon too old to
// publish one and keeps its pre-verdict behaviour for that daemon -- absence
// is neither "eligible" nor "ineligible".
type NewSessionVerdict struct {
	// Eligible: a peer may target a new session here. It is the router's own
	// tier-0 predicate (routableForNewSession with endgame drain allowed, the
	// Pick ordering), less the hard refusal selection applies after Pick
	// (evicted: a spent window or the owner's holdout, which tier 0 can still
	// hold when the Fable-reserved WeeklyHeadroom sits above the reserve while
	// the measured WeeklyRemaining is at it), with one narrowing: an account at
	// its endgame concurrency cap is NOT eligible for a peer's new session.
	// Locally a capped account is demoted below every routable one rather than
	// refused, which differs only when nothing else is routable; a peer cannot
	// see that, and piling a peer's sessions past the cap is the incident this
	// exists to stop (owner rule P1).
	Eligible bool `json:"eligible"`
	// Reason names the deciding clause; see the Verdict* constants.
	Reason string `json:"reason"`
	// Burndown: inside the burndown window -- the 24h weekly roll, or a reset
	// holder's early burndown (PATCHES §56) -- under its taper, above the short
	// floor and the reserve (endgameDrainEligible). Ranks first.
	Burndown bool `json:"burndown"`
	// AtCap: inside the window and carrying its full taper allowance
	// (endgameAtCap), or chosen for a saved reset and carrying its allowance
	// (PATCHES §95.2).
	AtCap bool `json:"at_cap"`
	// Evicted: a session ALREADY on this account must leave it -- a spent
	// window or the owner's holdout (Scheduler.Evicted). This is the
	// continuing-session criterion (PATCHES §20), deliberately distinct from
	// !Eligible: a warm session stays on an account below the new-session
	// floor.
	Evicted bool `json:"evicted"`
	// LiveSessions is the account's current concurrency on the publishing
	// daemon: the count its taper and its session spread run on.
	LiveSessions int `json:"live_sessions"`
	// AllowedSessions is the endgame taper allowance, or a chosen account's
	// (PATCHES §95.2); 0 outside the burndown window for an account not
	// chosen.
	AllowedSessions int `json:"allowed_sessions"`
	// BurndownCause says why the account is in the burndown window, "roll" or
	// "reset" (Score.BurndownCause), whenever it is -- for logs, srq and the
	// board; routing reads Burndown and AtCap. The one field that may be
	// absent: absent is "not in the window", which is also all a daemon from
	// before PATCHES §56 could have meant.
	BurndownCause string `json:"burndown_cause,omitempty"`
	// RefugeeCap is how many warm refugees the account takes (PATCHES §72),
	// computed by its owner on its own score (Score.refugeeCap). A peer
	// places refugees on it up to this cap. Absent, from a daemon older than
	// §72, it reads as unpublished.
	RefugeeCap RefugeeCap `json:"refugee_cap"`
}

// RefugeeCap is a published refugee cap that remembers whether it was
// published at all: 0 is a cap (an account nearly drained takes no refugee),
// and absence is a daemon too old to publish one. Comparable, so a verdict
// read off the wire equals the one its owner computed.
type RefugeeCap struct {
	Slots     int
	Published bool
}

// Or is the cap, or legacy when none was published.
func (c RefugeeCap) Or(legacy int) int {
	if !c.Published {
		return legacy
	}
	return c.Slots
}

// MarshalJSON writes the cap as a number, or null when unpublished.
func (c RefugeeCap) MarshalJSON() ([]byte, error) {
	if !c.Published {
		return []byte("null"), nil
	}
	return strconv.AppendInt(nil, int64(c.Slots), 10), nil
}

// UnmarshalJSON reads a number as a published cap; null is unpublished, and
// an absent key never reaches here.
func (c *RefugeeCap) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*c = RefugeeCap{}
		return nil
	}
	var slots int
	if err := json.Unmarshal(data, &slots); err != nil {
		return err
	}
	*c = RefugeeCap{Slots: slots, Published: true}
	return nil
}

// NewSessionVerdict evaluates the verdict on this scheduler's working view of
// the account -- live debits, parked and live session counts and reserves, as
// attached -- with the SAME predicates Pick ranks by. Callers pass the
// scheduler selection builds (the proxy's selectionSchedulerFrom), so the
// verdict is exactly as narrow as routing.
//
// The eligibility DECISION is usableForNewSession / endgameDrainEligible /
// endgameAtCap / evicted / a closed admission queue / pin-only; the reason for an
// ineligible, non-evicted, not-capped, open account is a label for which
// clause of usableForNewSession refused it, read in that function's own order.
func (s Scheduler) NewSessionVerdict(provider account.Provider, accountID string, now time.Time) NewSessionVerdict {
	score := s.score(provider, accountID)
	verdict := NewSessionVerdict{
		Burndown:      score.endgameDrainEligible(now),
		AtCap:         score.endgameAtCap(now) || score.loadAtCap(now),
		Evicted:       score.evicted(),
		LiveSessions:  score.LiveSessions,
		BurndownCause: score.BurndownCause(now),
	}
	if score.inBurndownWindow(now) || score.loading {
		verdict.AllowedSessions = score.endgameAllowedSessions(now)
	}
	verdict.RefugeeCap = RefugeeCap{Slots: score.refugeeCap(now), Published: true}
	if published, ok := score.peerVerdict(); ok {
		verdict.RefugeeCap = published.RefugeeCap
	}
	switch {
	case score.pinOnly:
		verdict.Reason = VerdictPinOnly
	case score.exhausted():
		verdict.Reason = VerdictExhausted
	case score.inHoldout():
		verdict.Reason = VerdictHoldout
	case score.queueClosed:
		verdict.Reason = VerdictQueueClosed
	case verdict.AtCap:
		verdict.Reason = VerdictAtCap
	case verdict.Burndown:
		verdict.Eligible, verdict.Reason = true, VerdictBurndown
	case score.loadEligible(now):
		verdict.Eligible, verdict.Reason = true, VerdictChosen
	case score.usableForNewSession():
		verdict.Eligible, verdict.Reason = true, VerdictEligible
	case score.ShortHeadroom < MinShortHeadroomFloor:
		verdict.Reason = VerdictShortFloor
	case score.belowWeeklyReserve():
		verdict.Reason = VerdictBelowReserve
	default:
		// usableForNewSession admits WeeklyOnly and timing-less scores outright,
		// so past the floor and the reserve the only clause left that refuses is
		// the 5h burn projection.
		verdict.Reason = VerdictBurnProjection
	}
	return verdict
}
