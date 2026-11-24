package selectacct

import (
	"time"

	"github.com/discordwell/subrouter/account"
)

// Loading the accounts chosen for a reset (PATCHES §95.2). The daemon that
// holds an account chooses the ones the pool should run to their limit, so
// that their next saved reset is claimed there (§95.1), and publishes each
// choice. Ruling 2026-11-22: "with resets in hand the aim is to load, not to
// balance". Ranking honours a choice for a new session: a chosen account that
// can take one ranks above every steady account and below the drain tier, the
// order its holder gave decides among chosen accounts, its allowance is the
// paced ceiling, and the short floor does not stop it. Nothing here moves a
// session that already has an account.

// ResetChoiceMeter is the meter a chosen account is run to its limit on.
type ResetChoiceMeter string

const (
	// ResetChoiceWeek is a Claude account chosen for its week: it is loaded
	// on every model pool of the account.
	ResetChoiceWeek ResetChoiceMeter = "week"
	// ResetChoiceFable is a Claude account whose next reset is a Fable reset,
	// chosen for its Fable meter: it is loaded on the Fable pool only.
	ResetChoiceFable ResetChoiceMeter = "fable"
	// ResetChoiceCodexWeek is a Codex account chosen for its week.
	ResetChoiceCodexWeek ResetChoiceMeter = "codex_week"
)

// ResetChoice is an account its holder chose to run to its limit, as its
// usage-status row publishes it: inside normalized in real mode, where every
// daemon that ranks the account reads it, and as would_choose beside it in
// shadow mode, where nothing ranks on it.
type ResetChoice struct {
	Meter ResetChoiceMeter `json:"meter"`
	// Order is the account's place among its holder's accounts chosen for
	// the same meter, 1 first: the next takes new sessions only when the
	// ones before it are at their allowance.
	Order int `json:"order"`
	// Why says, in one sentence, why the account is chosen.
	Why string `json:"why"`
}

// Known reports a choice this build ranks on: a meter it knows and an order
// from 1. A peer's other choices are ignored.
func (c ResetChoice) Known() bool {
	switch c.Meter {
	case ResetChoiceWeek, ResetChoiceFable, ResetChoiceCodexWeek:
		return c.Order >= 1
	}
	return false
}

// WithResetChoices attaches the choices this daemon made for its own
// accounts, keyed by ScoreKey; a peer's account carries its holder's choice
// on its score. The caller attaches them only when they rank (real mode, the
// loading switch on). The map must not be mutated afterwards.
func (s Scheduler) WithResetChoices(choices map[string]ResetChoice) Scheduler {
	next := s
	next.resetChoices = choices
	return next
}

// LoadsForReset reports whether this scheduler loads the account for its
// holder's choice: chosen for a meter this pool spends.
func (s Scheduler) LoadsForReset(provider account.Provider, accountID string) bool {
	return s.measuredScore(provider, accountID).loading
}

// loads reports whether a request this scheduler ranks for spends the meter
// choice was made for: a week on every pool of the account, the Fable meter
// on the Fable pool only.
func (s Scheduler) loads(choice ResetChoice) bool {
	switch choice.Meter {
	case ResetChoiceWeek, ResetChoiceCodexWeek:
		return true
	case ResetChoiceFable:
		return s.fablePool
	}
	return false
}

// loadEligible is a chosen account that takes the next new session first:
// below its allowance, and neither exhausted nor in its owner's holdout. The
// short floor does not apply: it takes new sessions down to its limit. A
// closed queue refuses it before ranking does (routableForNewSession). A
// peer's account is its holder's answer, read from its verdict.
func (s Score) loadEligible(now time.Time) bool {
	if !s.loading {
		return false
	}
	if verdict, ok := s.peerVerdict(); ok {
		return verdict.Eligible && !verdict.AtCap
	}
	return !s.evicted() && s.LiveSessions < s.allowance(now).Sessions
}

// loadAtCap is a chosen account already carrying its allowance. Like a
// draining account at its cap, it ranks below every other routable account.
func (s Score) loadAtCap(now time.Time) bool {
	if !s.loading {
		return false
	}
	if verdict, ok := s.peerVerdict(); ok {
		return verdict.AtCap
	}
	return s.LiveSessions >= s.allowance(now).Sessions
}

// Reasons the comparator gives for a choice.
const (
	reasonChosen         = "chosen to run to its limit for its next saved reset"
	reasonChosenFable    = "both chosen for a saved reset; this one for its Fable meter, which only Fable work spends"
	reasonChosenOrder    = "both chosen for a saved reset; this one is first in its holder's order"
	reasonChosenLongRoll = "both chosen for a saved reset at the same place in the order; this one has longer to its roll"
	reasonChosenTied     = "both chosen for a saved reset at the same place in the order; ordered by account id"
	reasonChosenAtCap    = "the other is chosen for a saved reset but already carries its allowance"
	reasonSteadyChosen   = "the other is chosen to run to its limit for a saved reset; not a place to park a session"
)

// compareChosen orders two accounts that both take the next new session for
// a choice: on the Fable pool a Fable choice first, then the holder's order,
// then the longer time to the roll, then the account ID. The local-over-peer
// factor, the live-session spread and the live debit do not enter.
func compareChosen(left, right account.Account, leftScore, rightScore Score, now time.Time) (bool, string) {
	leftFable, rightFable := leftScore.ResetChoice.Meter == ResetChoiceFable, rightScore.ResetChoice.Meter == ResetChoiceFable
	if leftFable != rightFable {
		return leftFable, reasonChosenFable
	}
	if leftScore.ResetChoice.Order != rightScore.ResetChoice.Order {
		return leftScore.ResetChoice.Order < rightScore.ResetChoice.Order, reasonChosenOrder
	}
	leftRoll, rightRoll := rankBucket(leftScore.weeklyResetSecondsLeft(now)), rankBucket(rightScore.weeklyResetSecondsLeft(now))
	if leftRoll != rightRoll {
		return leftRoll > rightRoll, reasonChosenLongRoll
	}
	return left.ID < right.ID, reasonChosenTied
}
