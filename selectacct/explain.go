package selectacct

import (
	"sort"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

// Ranking explanation.
//
// The comparator below IS the comparator Pick sorts with -- pick() calls rank()
// and takes element zero, and rank() sorts with compare(). There is deliberately
// no second implementation: a reporter that re-derives "why was this chosen"
// from the same numbers is a second ranking rule, it drifts the first time the
// real one changes, and a verifier written from the same misunderstanding agrees
// with itself. TestExplainMatchesPick pins the invariant that Explain's winner
// is Pick's winner across randomised pools.
//
// compare returns the decision AND the clause that made it, so the reason is a
// byproduct of the actual comparison rather than a reconstruction of it.

// Explanation is one account's place in the ranking, with the clause that put
// it ahead of the next account down.
type Explanation struct {
	Account account.Account
	// Score is the account's score as the comparator saw it -- after session
	// spread, live debits and the optimistic default for unscored accounts.
	Score Score
	// Reason names the comparator clause that decided this account outranks
	// the one below it. The last entry has no account below it and reports
	// "last". A reason of "tied; ordered by account id" is the honest terminal
	// case: nothing above it separated the two.
	Reason string
	// Usable mirrors the eligibility gate, so a caller can tell "ranked first
	// among candidates that can serve" from "ranked first among candidates
	// that cannot".
	Usable bool
}

// Explain ranks candidates exactly as Pick does and reports why. The clock is
// read once for the whole ranking, like pick's, so no boundary (endgame, weekly
// roll) can move mid-sort.
func (s Scheduler) Explain(candidates []account.Account) ([]Explanation, error) {
	return s.explain(candidates, true)
}

// ExplainSteady is Explain for the PickSteady ordering: endgame drain disabled.
func (s Scheduler) ExplainSteady(candidates []account.Account) ([]Explanation, error) {
	return s.explain(candidates, false)
}

func (s Scheduler) explain(candidates []account.Account, allowEndgame bool) ([]Explanation, error) {
	now := time.Now()
	sorted, err := s.rankAt(candidates, allowEndgame, now)
	if err != nil {
		return nil, err
	}
	out := make([]Explanation, 0, len(sorted))
	for i, acct := range sorted {
		reason := "last"
		if i+1 < len(sorted) {
			// The clause that put THIS account ahead of the next one. Comparing
			// against the immediate successor is the only comparison that
			// actually happened in the ordering sense; comparing against the
			// winner would report a clause that never ran for this pair.
			_, reason = s.compare(acct, sorted[i+1], now, allowEndgame)
		}
		score := s.score(acct.Provider, acct.ID)
		out = append(out, Explanation{
			Account: acct,
			Score:   score,
			Reason:  reason,
			Usable:  score.routableForNewSession(now, allowEndgame),
		})
	}
	return out, nil
}

// rankAt sorts candidates best-first. Exposed to pick and explain only; both
// must go through it so neither can hold a private ordering.
func (s Scheduler) rankAt(candidates []account.Account, allowEndgame bool, now time.Time) ([]account.Account, error) {
	if len(candidates) == 0 {
		return nil, ErrNoAccounts
	}
	sorted := append([]account.Account(nil), candidates...)
	sort.SliceStable(sorted, func(i, j int) bool {
		less, _ := s.compare(sorted[i], sorted[j], now, allowEndgame)
		return less
	})
	return sorted, nil
}

// compare is the ranking rule. It reports whether left sorts before right, and
// which clause decided.
//
// Transcribed unchanged from the sort closure this replaced; every comment there
// is preserved here because they document why each clause precedes the next.
func (s Scheduler) compare(left, right account.Account, now time.Time, allowEndgame bool) (bool, string) {
	leftScore := s.score(left.Provider, left.ID)
	rightScore := s.score(right.Provider, right.ID)

	// Machine policy outranks everything, including the selection tier: an
	// account this machine may not use is not a candidate no matter how much
	// quota it has. This has to precede the tier comparison, or a capped
	// account sitting at full headroom (tier 0) outranks an eligible one that
	// is merely low (tier 2). Sorted last rather than dropped, so the caller
	// can still see it and say why.
	leftAllowed := s.machineAllows(left)
	rightAllowed := s.machineAllows(right)
	if leftAllowed != rightAllowed {
		return leftAllowed, "machine policy allows it and refuses the other"
	}
	leftTier := selectionTier(left, leftScore, now, allowEndgame)
	rightTier := selectionTier(right, rightScore, now, allowEndgame)
	if leftTier != rightTier {
		return leftTier < rightTier, "higher selection tier"
	}
	// Within a tier, an account shedding this pool's requests goes after the
	// ones that are not; fewer consecutive capacity failures first (upstream
	// 862a5a5, ported as written -- see capacity.go). Only a request whose
	// pool has live marks carries any (WithCapacityMarks), so this clause is
	// inert everywhere else, the pool board included.
	//
	// The position is upstream's: right after the selection tier. Upstream
	// has no machine-binding or endgame-drain clause; here the mark therefore
	// outranks both. That is intended: an account that is shedding the model
	// fails the request that lands on it, whatever machine slot or expiring
	// budget it would have saved, and the mark lasts minutes, not the rolling
	// month a binding does. A marked account is demoted, never excluded -- a
	// sole candidate, or a pool where every account carries the same count,
	// ranks exactly as it would unmarked.
	leftCapacity := s.CapacityFailures(left.Provider, left.ID)
	rightCapacity := s.CapacityFailures(right.Provider, right.ID)
	if leftCapacity != rightCapacity {
		return leftCapacity < rightCapacity, "not shedding this model's requests; the other is at capacity"
	}
	leftUsable := leftScore.routableForNewSession(now, allowEndgame)
	rightUsable := rightScore.routableForNewSession(now, allowEndgame)
	if leftUsable != rightUsable {
		return leftUsable, "can take a new session and the other cannot"
	}
	if leftUsable && rightUsable {
		// A machine slot is the scarcest resource here: it is held for a
		// rolling month, while quota refills weekly. So reusing an existing
		// binding outranks every quota consideration below, including endgame.
		if s.machineActive() {
			leftBound := s.machineBound(left)
			rightBound := s.machineBound(right)
			if leftBound != rightBound {
				return leftBound, "this machine is already bound to it"
			}
			if !leftBound {
				// Neither is bound, so whichever we pick spends a slot. Pack:
				// spend it on an account that is ALREADY shared rather than
				// contaminating a pristine one.
				leftBindings := len(s.bindingsFor(left))
				rightBindings := len(s.bindingsFor(right))
				if leftBindings != rightBindings {
					return leftBindings > rightBindings, "already shared, so binding it costs no fresh slot"
				}
			}
		}
		bothDraining := false
		if allowEndgame {
			// Endgame drain: an account within a day of its weekly roll is
			// spending use-it-or-lose-it budget, so it outranks every
			// steady-state account while it still admits sessions under the
			// concurrency taper; with several draining, the earliest roll
			// leads but SHARES. At the taper cap it instead ranks BELOW every
			// other routable account. A reset holder in early burndown
			// (PATCHES §56) drains in the same tier, behind any account whose
			// roll lands first (endgameUrgency).
			leftEndgame := leftScore.endgameDrainEligible(now)
			rightEndgame := rightScore.endgameDrainEligible(now)
			if leftEndgame != rightEndgame {
				winner := leftScore
				if rightEndgame {
					winner = rightScore
				}
				if winner.BurndownCause(now) == BurndownCauseReset {
					return leftEndgame, "early burndown: holds an unclaimed saved reset and under 25% of its week is left"
				}
				return leftEndgame, "inside its weekly roll window; that budget expires either way"
			}
			if leftEndgame && rightEndgame {
				// Both draining. The earlier roll still leads -- that part was
				// right -- but the comparison now runs through spreadAdjusted
				// like every other rate term in this function, so the lead is a
				// lead and not a monopoly.
				//
				// Comparing raw reset times made this the ONE rate term here
				// with no session component, so the earliest-rolling account
				// won every pick until it hit endgameAtCap and fell off a
				// cliff, while a sibling hours behind it sat idle forfeiting
				// its own week. Two accounts inside the 24h window are both
				// losing budget; both should be draining. One live session
				// discounts urgency ~13%, which is enough to hand the next
				// pick to a sibling rolling a couple of hours later and keep
				// alternating, and not enough to reorder rolls that are
				// genuinely far apart.
				//
				// Bucketed for the same reason weeklyPace is: WeeklyResetAt is
				// stamped from a per-account time.Now(), so an exact time
				// comparison let microseconds of clock skew decide. Equal
				// urgency falls to the budget at stake instead.
				bothDraining = true
				leftUrgency := leftScore.spreadAdjusted(leftScore.endgameUrgency(now))
				rightUrgency := rightScore.spreadAdjusted(rightScore.endgameUrgency(now))
				if leftBucket, rightBucket := rankBucket(leftUrgency), rankBucket(rightUrgency); leftBucket != rightBucket {
					return leftBucket > rightBucket, "both draining; this one rolls sooner for the load it is carrying"
				}
				// Rolling together -- or the leader's live-session discount has
				// brought it level with a sibling. The one with more budget to
				// lose drains first: its RAW pace, not rankingPace. Inside the
				// burndown tier there is no local 2x (PATCHES §31 D2, §41): a
				// peer's account forfeits at its roll exactly as a local one
				// does, and the pace clause below would hand every such tie to
				// the local account unless the peer held twice its budget. That
				// clause is skipped for a draining pair; this one replaces it.
				leftStake, _ := leftScore.weeklyPace(now)
				rightStake, _ := rightScore.weeklyPace(now)
				leftStake, rightStake = leftScore.spreadAdjusted(leftStake), rightScore.spreadAdjusted(rightStake)
				if leftBucket, rightBucket := rankBucket(leftStake), rankBucket(rightStake); leftBucket != rightBucket {
					return leftBucket > rightBucket, "both draining and rolling together; this one has more budget to lose"
				}
			}
			leftCapped := leftScore.endgameAtCap(now)
			rightCapped := rightScore.endgameAtCap(now)
			if leftCapped != rightCapped {
				return rightCapped, "the other is draining but already at its concurrency cap"
			}
		} else {
			// Steady mode: rank endgame accounts LAST, the mirror of the
			// drain-first ordering above. PickSteady chooses the account an
			// interactive session will sit on for hours, and parking it on one
			// that rolls tonight is how that session goes dry -- or on a reset
			// holder the router is running into its wall days early.
			leftDraining := leftScore.inBurndownWindow(now)
			rightDraining := rightScore.inBurndownWindow(now)
			if leftDraining != rightDraining {
				drainer := rightScore
				if leftDraining {
					drainer = leftScore
				}
				if drainer.BurndownCause(now) == BurndownCauseReset {
					return rightDraining, "the other is in early burndown on a saved reset; not a place to park a session"
				}
				return rightDraining, "the other rolls within a day; not a place to park a session"
			}
		}
		leftPace, leftHasPace := leftScore.weeklyPace(now)
		rightPace, rightHasPace := rightScore.weeklyPace(now)
		if leftHasPace != rightHasPace {
			// Class before value keeps the comparator a strict weak ordering
			// when pace-scored and pressure-scored accounts share a list.
			return leftHasPace, "has a weekly pace and the other has none"
		}
		// Sessions discount the primary signal rather than waiting for it to
		// tie. Pace is near-continuous, so a session term below it would decide
		// essentially never. A draining pair has already been ranked on its
		// own pace, without the local factor, above.
		if leftHasPace && !bothDraining {
			// Local preference and the reset-hold factor are applied HERE, as
			// factors on the primary ranking signal, rather than as separate
			// spill gates.
			leftAdj := leftScore.spreadAdjusted(s.rankingPace(leftScore, leftPace, now))
			rightAdj := rightScore.spreadAdjusted(s.rankingPace(rightScore, rightPace, now))
			if leftBucket, rightBucket := rankBucket(leftAdj), rankBucket(rightAdj); leftBucket != rightBucket {
				winner, loser := leftScore, rightScore
				if rightBucket > leftBucket {
					winner, loser = rightScore, leftScore
				}
				if winner.holdsReset(now) && !loser.holdsReset(now) {
					return leftBucket > rightBucket, "higher weekly pace after the reset-hold 2x and the live-session discount"
				}
				if s.fableSpentDecides(leftScore, rightScore, leftPace, rightPace, leftBucket > rightBucket, now) {
					return leftBucket > rightBucket, "higher weekly pace after the Fable-spent preference and the live-session discount"
				}
				return leftBucket > rightBucket, "higher weekly pace after the live-session discount"
			}
		}
		// Short-window expiry, second. Among accounts of comparable weekly pace
		// the 5h window still separates them: quota in a window about to reset
		// is worth spending now, because it refills either way.
		leftShort := leftScore.spreadAdjusted(leftScore.shortExpiry())
		rightShort := rightScore.spreadAdjusted(rightScore.shortExpiry())
		if leftBucket, rightBucket := rankBucket(leftShort), rankBucket(rightShort); leftBucket != rightBucket {
			return leftBucket > rightBucket, "its short window expires sooner, so that budget is worth spending now"
		}
	}
	// Local preference outranks every SECONDARY signal. It sits below the rate
	// terms -- weekly pace already carries the LocalPreferenceRatio advantage,
	// and an expiring peer budget is a real reason to cross the network -- but
	// above headroom, because "the peer holds more" is not. Otherwise a peer
	// sitting exactly at the preference ratio takes the traffic on a raw
	// headroom tiebreak, which is not what preferring local means.
	//
	// Inert within a single pool, where every candidate is local.
	if leftScore.Remote != rightScore.Remote {
		return !leftScore.Remote, "local pool preferred; the rate signals tied"
	}
	// Sessions modulate headroom rather than outranking it.
	leftRoom := leftScore.spreadAdjusted(leftScore.Headroom)
	rightRoom := rightScore.spreadAdjusted(rightScore.Headroom)
	if leftRoom != rightRoom {
		return leftRoom > rightRoom, "more headroom after the live-session discount"
	}
	if leftScore.Sessions != rightScore.Sessions {
		return leftScore.Sessions < rightScore.Sessions, "carrying fewer sticky sessions"
	}
	// The honest terminal case. On a pool of untouched weekly-only accounts --
	// every Codex account reading 100% with rolls seconds apart -- nothing above
	// separates them and the ordering really is alphabetical. Saying so is more
	// useful than inventing a reason.
	return left.ID < right.ID, "tied; ordered by account id"
}

// fableSpentDecides reports whether the Fable-spent factor is what decided a
// pace-clause comparison (PATCHES §65): ranked without it, the pair ties or
// goes the other way. The reason names the factor only then, as it names the
// reset hold only when the winner holds and the loser does not.
func (s Scheduler) fableSpentDecides(left, right Score, leftPace, rightPace float64, leftWins bool, now time.Time) bool {
	if !s.nonFableClaudeTurn {
		return false
	}
	without := s
	without.nonFableClaudeTurn = false
	leftBucket := rankBucket(left.spreadAdjusted(without.rankingPace(left, leftPace, now)))
	rightBucket := rankBucket(right.spreadAdjusted(without.rankingPace(right, rightPace, now)))
	if leftWins {
		return leftBucket <= rightBucket
	}
	return rightBucket <= leftBucket
}
