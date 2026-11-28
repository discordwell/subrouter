package selectacct

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/discordwell/subrouter/account"
)

var ErrNoAccounts = errors.New("no accounts available")

type Score struct {
	AccountID              string
	Provider               account.Provider
	Headroom               float64
	ShortHeadroom          float64
	ShortResetAfterSeconds int64
	// ShortWindowSeconds is the LENGTH of the binding short window (typically
	// 5h), as opposed to how much of it is left. Needed to turn a bare
	// headroom level into a burn rate: 20% left means something very
	// different four hours before the reset than four minutes before it.
	ShortWindowSeconds int64
	// ExhaustedResetAt is when every measured window at zero headroom has
	// reset (the latest of their reset times); zero when no window is
	// exhausted. ExhaustedResetUnknown is set when an exhausted window
	// reported no reset time. Request-time exhaustion marks are separate
	// (SchedulerRef), so callers combine the two; see ExhaustionClearsAt
	// (upstream #530). Pick never reads them: they say when, not whether.
	ExhaustedResetAt      time.Time
	ExhaustedResetUnknown bool
	// WeeklyOnly marks an account whose provider publishes a weekly meter and
	// NO short window at all -- Codex since OpenAI dropped the 5h bucket
	// around 2026-07-12. Set only where the window set was actually read and
	// found to be weekly-only, never inferred downstream from
	// ShortWindowSeconds == 0. A weekly-only account's ShortHeadroom is its
	// week, so the new-session floor guards the week. A Claude account with
	// no `five_hour` reading is also missing a short window but is not
	// weekly-only: it keeps the Claude rules, and its ShortHeadroom is 1,
	// because no 5h reading means no 5h constraint was read, not that the
	// week stands in for one (scoreFromLimitWindows).
	WeeklyOnly bool
	// Remote marks an account that lives on a PEER daemon rather than this
	// one. Remote accounts are ranked by the same comparator as local ones --
	// that is the whole point of merging the pools -- but local carries a
	// LocalPreferenceRatio advantage on weekly pace, so the peer has to be
	// decisively better before traffic crosses the network. Local is the pool
	// that still works when the tailnet does not.
	Remote bool
	// PeerVerdict is the owning daemon's own NewSessionVerdict for a Remote
	// score (PATCHES §31). Routability, burndown and the endgame cap are READ
	// from it, never re-derived from published numbers: the peer counted live
	// sessions and applied reserves this daemon cannot see (the 2026-09-27
	// incident). Ignored on a local score.
	PeerVerdict *NewSessionVerdict
	// Reserve is the fraction of this account's WEEKLY budget held back from
	// the pool for its owner's own use. Zero for the ordinary pooled account.
	//
	// This is a holdout, not a safety margin: an account whose weekly budget
	// the pool is free to spend to the floor leaves its owner with nothing
	// when the pool runs dry -- including nothing to diagnose the pool with.
	// Reserving a sliver of the one account a person can log into directly is
	// what makes that recoverable.
	Reserve        float64
	ExpiryPressure float64
	Sessions       int
	// LiveSessions is the account's current concurrency (sessions with
	// in-flight work), set at pick time from WithLiveSessionCounts. Sessions
	// above counts sessions PARKED on the account and only spreads new
	// sessions; the endgame taper must run on live concurrency.
	LiveSessions int
	ModelScores  map[string]Score
	// MissingModelSupport describes what an omitted model quota bucket means
	// for this account (upstream #467). The zero value defers to the provider
	// default (DefaultMissingModelSupport), so seed, fallback and
	// exhaustion-mark scores built without usage telemetry keep the
	// provider's meaning. Adapters with better evidence set Unsupported or
	// Unknown explicitly.
	MissingModelSupport ModelSupport
	// WeeklyHeadroom is the spendable fraction of the binding weekly budget
	// for this score's traffic class, after the fable reservation for
	// non-fable Claude pools when that reservation is on (FableReservation,
	// PATCHES §57; off by default, and then it is the measured remainder).
	// Set for any provider publishing a weekly window -- Claude and Codex
	// both do -- and only the fable reservation is Claude-specific. Zero
	// leaves the legacy ExpiryPressure/Headroom ordering in charge.
	WeeklyHeadroom float64
	// WeeklyRemaining is the account's MEASURED weekly budget left: no Fable
	// routing reservation (which WeeklyHeadroom carries for non-Fable pools
	// when §57's switch is on) and no live debit (which score() applies to WeeklyHeadroom between
	// refreshes). The owner's holdout is a hard limit that evicts warm
	// sessions, so it is judged on what was measured, never on an estimate:
	// either adjustment evicted sessions from accounts still above it.
	// Zero means unmeasured and fails open, like belowWeeklyReserve.
	WeeklyRemaining float64
	// WeeklyResetAt is the wall-clock instant the binding weekly window rolls
	// over. An absolute time, not a captured duration: scores are carried
	// forward as seeds when usage fetches fail, and a frozen countdown would
	// pin pace ranks and endgame status long after the real window moved.
	// Reads clamp an elapsed deadline to "no weekly information". Zero means
	// no weekly pacing: carried legacy seeds, optimistic defaults, and any
	// provider publishing no weekly window. Codex DOES publish one (its
	// `primary` window is 604800s) and is paced on it like Claude.
	WeeklyResetAt time.Time
	// WeeklyUnstarted marks a binding weekly window that has not started: its
	// reading gives the window's whole length as left. A Codex week starts on
	// its first request, so the next new session goes here first to start its
	// clock (PATCHES §86). Set only where the windows are read
	// (scoreFromLimitWindows), never rebuilt from WeeklyResetAt, which is
	// anchored when the score was built, not when the window was read. A
	// missing or unread window is not unstarted.
	WeeklyUnstarted bool
	// ResetHeldUntil is when the account's unclaimed saved usage-limit reset
	// (cedar_ember, PATCHES §46) that clears the general week expires. A
	// grant that clears only the Fable limit sets nothing here: its claim
	// gives back only the week that Fable usage spent, so nothing left in the
	// week is lost at the claim (PATCHES §64). While it is in the future the
	// account ranks at ResetHoldPreferenceRatio and drains early below
	// ResetBurndownWeeklyRemaining of its week, so it is spent down and the
	// reset can be claimed for a fresh week before the grant lapses (PATCHES
	// §56). Absolute like WeeklyResetAt, for the same reason: a seed carried
	// forward stops qualifying on its own at the expiry. Zero means no hold.
	// Set by the owning daemon from its cedar_ember reset info
	// (resetHeldUntil in internal/proxy): a known reading decides, and an
	// unread or unreadable one keeps the previous score's hold rather than
	// dropping it, so one "unavailable" answer does not flap the account out
	// of its burndown. A peer's arrives published.
	ResetHeldUntil time.Time
	// Fresh marks a score computed from a successful, current usage fetch, as
	// opposed to a seed carried forward from the previous scheduler (fetch
	// failed/stale) or a request-time exhaustion mark. Expiry reconciliation
	// uses it to tell "fresh evidence re-confirmed exhausted" apart from "old
	// zero score dragged along".
	Fresh bool
	// MeasuredAt is when the usage reading behind this score was taken: the
	// moment its fetch STARTED, which for windows served from the usage-window
	// cache is the cache entry's own time, not the time of the publish that
	// carries them. Exhaustion-mark reconciliation orders on it
	// (SchedulerRef.markNewerThanEvidenceLocked): headroom read before a 429
	// is not evidence the 429 is over. Zero means unknown -- a score built
	// outside the daemon's usage paths -- and falls back to the order in which
	// the publish began. A model pool's score inherits its account's.
	MeasuredAt time.Time
	// FableSpent is the used fraction, 0..1, of the account's own Fable
	// weekly meter: the oauth-apps-weekly window, not the general week the
	// Fable pool also reads, and not the lower of the two. An Opus or Sonnet
	// turn ranks the account's weekly pace by FableSpentFactor (PATCHES §65),
	// so non-Fable work lands where the Fable budget is gone and leaves the
	// accounts that can still serve Fable. Zero is untouched OR unknown -- no
	// Fable window, the unused placeholder, a peer too old to publish it --
	// and both rank at 1x. Set on the account score; its model pools inherit
	// it (inheritAccountFacts). A peer's arrives published.
	FableSpent float64
	// Pace is the account's measured weekly burn (PATCHES §72), set by
	// measuredScore from the pace table SchedulerRef.Get attaches. Zero is
	// unmeasured, and so is every Remote score: a peer's allowance is read
	// from its verdict.
	Pace Pace
	// ResetChoice is the holder's choice to run this account to its limit
	// for its next saved reset (PATCHES §95.2): a local account's from the
	// table WithResetChoices attached, a peer's as its holder published it.
	// Zero is none.
	ResetChoice ResetChoice
	queueClosed bool
	// pinOnly marks one of this daemon's own accounts that takes pinned
	// sessions only (PATCHES §98, WithPinOnlyAccounts).
	pinOnly bool
	// loading is set by the scheduler that reads the score: its ResetChoice
	// is for a meter the request spends (Scheduler.loads).
	loading bool

	// weeklyDebit and shortDebit are what ApplyLiveDebit took off
	// WeeklyHeadroom and ShortHeadroom (after its 0.01 floor); zero when
	// undebited. The drain adds them back to read the measured meters
	// (drainInputs, PATCHES §55). Unexported: no JSON or wire effect.
	weeklyDebit, shortDebit float64

	// accountWeeklyResetAt is the ACCOUNT's weekly roll on a model pool's
	// score (inheritAccountFacts), whose own WeeklyResetAt may be a meter with
	// its own reset (Fable). The holdout is a fraction of the account's week,
	// so its final-day release (reserveReleased) is timed on this roll. Zero on
	// an account score, which reads its own WeeklyResetAt.
	accountWeeklyResetAt time.Time
}

// ModelSupport is the provider-normalized meaning of an omitted model quota
// bucket.
type ModelSupport uint8

const (
	// ModelSupportProviderDefault resolves through DefaultMissingModelSupport.
	ModelSupportProviderDefault ModelSupport = iota
	// ModelSupportUnsupported excludes the account from a pool it lacks.
	ModelSupportUnsupported
	// ModelSupportUnknown keeps the account eligible on its account-level score.
	ModelSupportUnknown
)

// DefaultMissingModelSupport is the provider table for scores that do not
// declare MissingModelSupport (upstream #467, with this fork's table).
//
// Claude publishes a per-model bucket only where the model has a limit of its
// own. Today that is Fable alone (the Fable limit); Opus and Sonnet spend
// the general 5h/7d budget and their buckets come back null, and a setup token
// cannot read the usage endpoint at all (PATCHES §24). Antigravity omits
// disabled, unavailable, and sometimes merely unreported buckets. For both,
// absence is "no separate limit" or "unknown", never proof the account cannot
// serve a pool another account exposes, so both default to Unknown. Every
// other provider treats absence as unsupported.
//
// Upstream's table marks only Antigravity Unknown. Keeping Claude Unknown here
// is what lets Opus and Sonnet route on the account-wide budget (§24); a
// caller that reads this table -- upstream's Claude retry evidence among them
// -- must not see a missing Opus bucket as an account that cannot serve Opus.
func DefaultMissingModelSupport(provider account.Provider) ModelSupport {
	if provider == account.ProviderClaude || provider == account.ProviderAntigravity {
		return ModelSupportUnknown
	}
	return ModelSupportUnsupported
}

// missingModelSupport resolves the score's declaration through the provider
// table.
func (s Score) missingModelSupport() ModelSupport {
	if s.MissingModelSupport != ModelSupportProviderDefault {
		return s.MissingModelSupport
	}
	return DefaultMissingModelSupport(s.Provider)
}

type Scheduler struct {
	scores        map[string]Score
	sessionCounts map[string]int
	// liveSessionCounts is per-account CURRENT concurrency (sessions with
	// in-flight work), distinct from sessionCounts (sessions parked on the
	// account, which spreads new sessions). The endgame taper runs on live
	// concurrency: parked counts grow monotonically and would keep drain
	// mode permanently off.
	liveSessionCounts map[string]int
	liveDebits        map[string]int
	// reserves is the weekly holdout per account, keyed the way the operator
	// configured it: a bare account ID, a ScoreKey, or one of the account's
	// aliases (its login email or label -- see accountAliases). Absent means
	// the account is fully poolable.
	reserves map[string]float64
	// accountAliases maps ScoreKey(provider, id) to the other names an
	// operator may use for that account in configuration. A Codex account
	// enrolled with owner claims is keyed "codex-owner-<hash>", so a holdout
	// written against its login email would otherwise silently stop applying
	// the day the account is re-enrolled -- failing open with no error.
	accountAliases map[string][]string
	// machineID identifies the client machine this pick is for, and machines
	// answers questions about which machines are bound to or holding an
	// account. Both are empty/nil for callers that are not routing a remote
	// client (sr auto-switch rotates a local profile and has no machine), and
	// every machine-aware term is skipped in that case -- absence means "no
	// opinion", never "ineligible".
	machineID string
	machines  MachineView
	// capacity is one (model, service tier) pool's consecutive capacity
	// failures per ScoreKey, attached by WithCapacityMarks at pick time (see
	// capacity.go). Absent means no account is shedding that pool. Every
	// With* copies the struct, so the table rides along untouched.
	capacity map[string]int
	// nonFableClaudeTurn marks a scheduler ForModel scoped to the Opus or
	// Sonnet pool: a Claude turn that does not spend the Fable meter, the
	// only kind ranked with FableSpentFactor (PATCHES §65). Haiku is left
	// out on purpose: Claude Code's first request of a session is often a
	// Haiku call, and it must not pin a session that will run Fable to an
	// account whose Fable is spent.
	nonFableClaudeTurn bool
	// fablePool marks a scheduler ForModel scoped to the Fable pool, the only
	// one an account chosen for its Fable meter is loaded on (PATCHES §95.2).
	fablePool bool
	// resetChoices is this daemon's choices for its own accounts, keyed by
	// ScoreKey (WithResetChoices).
	resetChoices   map[string]ResetChoice
	paces          map[string]Pace
	closedAccounts map[string]bool
	// pinOnly is this daemon's own accounts that take pinned sessions only,
	// keyed by bare account ID (WithPinOnlyAccounts, PATCHES §98).
	pinOnly map[string]bool
	// startSent is when each account's start mark expires (PATCHES §86),
	// keyed by ScoreKey. SchedulerRef.Get attaches it.
	startSent map[string]time.Time
}

// MachineView answers the two machine-scoped questions selection needs, without
// the scheduler taking a dependency on where that state lives. The proxy backs
// it with a file-backed binding store and an in-memory lease table; tests pass
// fakes.
//
// A nil MachineView means the caller has no machine policy at all, and every
// account is eligible. That is the fail-open direction, and it is deliberate: a
// bookkeeping fault must never be able to refuse traffic.
type MachineView interface {
	// Bindings lists machine IDs that have used this account inside the
	// rolling window, oldest binding first.
	Bindings(provider account.Provider, accountID string) []string
	// LeaseHolder names the machine currently holding this account, if any.
	// An expired lease reports no holder.
	LeaseHolder(provider account.Provider, accountID string) (string, bool)
}

// MaxMachinesPerAccount is the rolling-window cap on distinct machines per
// account. The operator's own machines count: the upstream provider cannot
// distinguish them from anyone else's, so neither does this.
const MaxMachinesPerAccount = 6

// WithMachine returns a scheduler that ranks for a specific client machine.
// Follows the same immutable-copy shape as WithSessionCounts and friends.
func (s Scheduler) WithMachine(machineID string, view MachineView) Scheduler {
	next := s
	next.machineID = strings.TrimSpace(machineID)
	next.machines = view
	return next
}

// MachineActive reports whether machine-aware ranking applies at all, exported
// for the same reason UsableForNewSession is: callers that need to know whether
// the machine term is in play must ask the scheduler rather than re-deriving it
// from the policy and the caller's arguments. Those two answers drifted once
// already -- the board passed a machine ID a forwarding daemon's policy did not
// honour, and ranked on bindings the router had stopped consulting.
func (s Scheduler) MachineActive() bool {
	return s.machineActive()
}

// machineActive reports whether machine-aware ranking applies at all.
func (s Scheduler) machineActive() bool {
	return s.machines != nil && s.machineID != ""
}

func (s Scheduler) bindingsFor(acct account.Account) []string {
	if !s.machineActive() {
		return nil
	}
	return s.machines.Bindings(acct.Provider, acct.ID)
}

// machineBound reports whether this machine already occupies one of the
// account's slots, so routing here costs nothing.
func (s Scheduler) machineBound(acct account.Account) bool {
	for _, id := range s.bindingsFor(acct) {
		if id == s.machineID {
			return true
		}
	}
	return false
}

// MachineRefusal explains why this account is unavailable to this machine, or
// returns ok=true when it is available. Selection never drops candidates on
// this basis -- it sorts them last -- so that the caller can decide whether to
// enforce, and can report a cause instead of a bare 503.
func (s Scheduler) MachineRefusal(acct account.Account) (reason string, refused bool) {
	if !s.machineActive() {
		return "", false
	}
	if holder, held := s.machines.LeaseHolder(acct.Provider, acct.ID); held && holder != s.machineID {
		return "account is held by machine " + holder + " (one machine per account at a time)", true
	}
	bindings := s.bindingsFor(acct)
	if len(bindings) >= MaxMachinesPerAccount && !s.machineBound(acct) {
		return "account has reached " + strconv.Itoa(MaxMachinesPerAccount) +
			" machines in the rolling window and this machine is not one of them", true
	}
	return "", false
}

// machineAllows is MachineRefusal as a sort key.
func (s Scheduler) machineAllows(acct account.Account) bool {
	_, refused := s.MachineRefusal(acct)
	return !refused
}

// There is deliberately no flat headroom floor any more.
//
// MinNewSessionHeadroom used to require 40% of the short window before an
// account could take a new session, and three separate copies of that rule
// existed (here, cmd/subrouter, internal/proxy) which did not agree with each
// other. All of them refused capacity the pool actually had: an account is
// either about to run out -- which is a RATE question the burn projection
// answers -- or it is not, and a level says nothing about which. On a weekly
// window it was strictly destructive, forfeiting the last 40% of a budget that
// expires at the roll.
//
// What remains is a thin exhaustion floor (MinShortHeadroomFloor) plus the burn
// rate where the window's timing is known. Ranking is weekly pace. Eligibility
// answers "can this account survive a session starting now", and nothing else.

// LocalPreferenceRatio is how much better a REMOTE account must be, on weekly
// pace, before it outranks a local one: local's pace is multiplied by this for
// ranking, so a peer needs to beat it outright to win.
//
// A ratio on pace rather than a spill threshold, because pace already is the
// currency -- remaining weekly budget per second until it expires -- and this
// makes "prefer local" a term in the ranking instead of a separate gate with
// its own margins and unknown-rules bolted alongside. It also self-regulates:
// liveDebits lower local pace as requests are routed, so the draw on the peer
// rises as local depletes and falls as it recovers, with no threshold to tune.
const LocalPreferenceRatio = 2.0

// ResetHoldPreferenceRatio is the weekly-pace factor an account holding an
// unclaimed saved reset ranks with (PATCHES §56). The reset is worth a fresh
// week only to an account that has spent this one, and it lapses unclaimed at
// its expiry, so a holder's budget is worth more spent now than a non-holder's
// at the same pace. The same currency as LocalPreferenceRatio, and it takes
// precedence over it (the owner, 2026-10-15: "whether they're remote or
// on-laptop they're 2x the on-laptop"): a holder ranks at twice a LOCAL
// non-holder wherever it lives -- 4x a peer non-holder -- so a box holder
// draws laptop sessions away from the laptop's own non-holders. Between two
// holders the local one still wins a pace tie ("local pool preferred"). A
// factor, not a tier: a non-holder spending more than twice as fast still
// wins.
const ResetHoldPreferenceRatio = 2.0

// ResetBurndownWeeklyRemaining is where a reset holder enters early burndown:
// strictly below this MEASURED fraction of its week left (WeeklyRemaining), it
// joins the drain tier days before its roll, because the reset it holds
// refills that week when claimed. Measured, not WeeklyHeadroom: WeeklyHeadroom
// carries this daemon's live debit between refreshes, and, with the Fable
// reservation on (SUBROUTER_FABLE_RESERVATION=1, PATCHES §57), the
// reservation too -- for most of the week about half of what is left (0.195
// against 0.39 measured on fifteen-beta on 10/15), so 25% of it would
// fire at about half the week. With §57's default (off) the two differ by the
// debit alone, and the trigger reads the measured week either way.
const ResetBurndownWeeklyRemaining = 0.25

// FableSpentPreferenceRatio is the weekly-pace factor an Opus or Sonnet turn
// ranks an account at when its own Fable meter is fully spent (PATCHES §65),
// rising linearly from 1x at an untouched or unknown meter. Non-Fable work
// spends the general week either way; on an account whose Fable is gone that
// week can serve nothing else, while on one with Fable left it is also what
// Fable needs. A factor, not a tier: an account with Fable untouched still
// wins at more than twice the pace.
const FableSpentPreferenceRatio = 2.0

// FableSpentFactor is the factor an Opus or Sonnet turn multiplies this
// account's weekly pace by in the ranking (PATCHES §65): 1 with the switch
// off, else 1 + (FableSpentPreferenceRatio - 1) x FableSpent. Exported so a
// reporter shows the factor routing ranks on instead of re-deriving it.
func (s Score) FableSpentFactor() float64 {
	if !FableSpentPreference() {
		return 1
	}
	return 1 + (FableSpentPreferenceRatio-1)*s.FableSpent
}

// rankingPace is weekly pace with the local advantage or the reset-hold
// advantage already applied. A holder is priced against a local non-holder
// whether it is local or a peer's, so the hold replaces the local factor
// rather than stacking on it. An Opus or Sonnet turn also multiplies by the
// Fable-spent factor, which stacks on either. Each is a factor on one score's
// own pace, so the comparator stays a strict weak ordering.
func (s Scheduler) rankingPace(score Score, pace float64, now time.Time) float64 {
	// 2026-11-24 owner: route Opus and other non-Fable sessions onto accounts
	// that are out of Fable but still have week left, so Fable headroom
	// elsewhere is kept for Fable sessions (PATCHES §65, §104).
	// An account chosen for its Fable meter is not ranked up for the Opus or
	// Sonnet work that would eat the week under its reset (PATCHES §95.2).
	if s.nonFableClaudeTurn && score.ResetChoice.Meter != ResetChoiceFable {
		pace *= score.FableSpentFactor()
	}
	switch {
	case score.holdsReset(now):
		return pace * LocalPreferenceRatio * ResetHoldPreferenceRatio
	case !score.Remote:
		return pace * LocalPreferenceRatio
	}
	return pace
}

// ShortBurnTolerance is how much of the projected overspend is forgiven: 1.0
// refuses an account whose current burn, carried to the reset, would spend
// more than it has left. Above 1.0 tolerates accounts projected to run out
// early, on the theory that they will slow down.
const ShortBurnTolerance = 1.0

// shortBurnMinElapsedFraction floors the elapsed window used to infer a burn
// rate, so the projection is not computed off a few seconds of evidence.
const shortBurnMinElapsedFraction = 0.2

// MinShortHeadroomFloor is an absolute backstop under the rate rule. However
// close the reset is, an account with almost nothing left should not be
// handed a new session -- the reset is a promise about the future, and the
// next few requests happen now.
const MinShortHeadroomFloor = 0.05

// ScoreKey is the scheduler's identity for an account. A Codex account's ID is
// its bare email and a Claude account's ID is its profile name, which can also
// be an email, so the same string routinely identifies one account per
// provider (e.g. lawrence@cmux.com exists as both a Codex account and a Claude
// profile). Keying scores by the bare ID alone lets one provider's score
// silently overwrite the other's; scoping the key by provider keeps them
// distinct.
func ScoreKey(provider account.Provider, accountID string) string {
	if provider == "" {
		provider = account.ProviderCodex
	}
	return string(provider) + "\x00" + accountID
}

func NewScheduler(scores []Score) Scheduler {
	byID := make(map[string]Score, len(scores))
	for _, score := range scores {
		byID[ScoreKey(score.Provider, score.AccountID)] = score
	}
	return Scheduler{scores: byID}
}

func (s Scheduler) WithScore(score Score) Scheduler {
	// Copy-then-modify, never a fresh literal. A literal silently drops every
	// field it forgets to name: machineID and machines were already being lost
	// this way, harmless only because WithMachine happens to run last in the
	// live chain. Copying makes each builder's blast radius the field it sets.
	next := s
	next.scores = make(map[string]Score, len(s.scores)+1)
	for key, existing := range s.scores {
		next.scores[key] = existing
	}
	next.scores[ScoreKey(score.Provider, score.AccountID)] = score
	return next
}

// WithSessionCounts attaches parked-session counts per account. Keys may be
// ScoreKey(provider, id) -- what the proxy supplies, so a Codex account and a
// Claude profile sharing an email do not damp each other's placement -- or a
// bare account ID; score() reads the ScoreKey first and falls back to the bare
// ID.
//
// Copy-then-modify, never a fresh literal: a literal here dropped reserves,
// liveSessionCounts and the machine view, which erased the holdout on every
// failover pick built as ForModel(...).WithReserves(...).WithSessionCounts(...).
func (s Scheduler) WithSessionCounts(counts map[string]int) Scheduler {
	next := s
	next.sessionCounts = map[string]int{}
	for accountKey, count := range counts {
		next.sessionCounts[accountKey] = count
	}
	return next
}

// WithReserves attaches per-account weekly holdouts, keyed as configured: a
// bare account ID, a ScoreKey, or an account alias (login email or label, see
// WithAccountAliases). An account listed here stops taking new sessions once
// its weekly budget falls to the reserve, and is excluded from the endgame
// drain that would otherwise spend exactly this budget -- except in the last
// day before its weekly roll, when the reserve is lifted (reserveReleased).
func (s Scheduler) WithReserves(reserves map[string]float64) Scheduler {
	next := s
	next.reserves = map[string]float64{}
	for accountKey, fraction := range reserves {
		next.reserves[accountKey] = fraction
	}
	return next
}

// WithAccountAliases records, for each account, the names other than its ID
// that an operator may use for it in configuration: its email when that
// differs from the ID, its label, and the login email a Codex display label
// carries ("lawrence@example.com [pro]"). Reserves resolve through them, so a
// holdout keyed by email keeps holding when the account's ID becomes an opaque
// "codex-owner-<hash>". SchedulerRef.Get attaches the aliases of the accounts
// it was last synced with; a scheduler built without them resolves reserves by
// ID alone, which is the pre-alias behaviour.
func (s Scheduler) WithAccountAliases(accounts []account.Account) Scheduler {
	next := s
	next.accountAliases = AccountAliases(accounts)
	return next
}

// withAccountAliasMap attaches an alias map built by AccountAliases without
// copying it. The map must not be mutated afterwards.
func (s Scheduler) withAccountAliasMap(aliases map[string][]string) Scheduler {
	next := s
	next.accountAliases = aliases
	return next
}

// AccountAliases builds the alias table WithAccountAliases attaches, keyed by
// ScoreKey(provider, id).
func AccountAliases(accounts []account.Account) map[string][]string {
	if len(accounts) == 0 {
		return nil
	}
	out := make(map[string][]string, len(accounts))
	for _, acct := range accounts {
		id := strings.TrimSpace(acct.ID)
		if id == "" {
			continue
		}
		var aliases []string
		add := func(name string) {
			name = strings.TrimSpace(name)
			if name == "" || strings.EqualFold(name, id) {
				return
			}
			for _, existing := range aliases {
				if strings.EqualFold(existing, name) {
					return
				}
			}
			aliases = append(aliases, name)
		}
		add(acct.Email)
		add(acct.Label)
		add(labelLoginName(acct.Label))
		if len(aliases) > 0 {
			key := ScoreKey(acct.Provider, id)
			out[key] = append(out[key], aliases...)
		}
	}
	return out
}

// labelLoginName strips a trailing bracketed qualifier from a display label:
// "lawrence@example.com [pro]" and "lawrence@example.com [workspace 1a2b3c4d]"
// both name the login lawrence@example.com.
func labelLoginName(label string) string {
	label = strings.TrimSpace(label)
	if !strings.HasSuffix(label, "]") {
		return ""
	}
	open := strings.LastIndex(label, " [")
	if open <= 0 {
		return ""
	}
	return strings.TrimSpace(label[:open])
}

// reserveFor resolves an account's holdout: the ScoreKey, then the bare ID,
// then each alias (case-insensitively -- emails are). The first configured
// match wins.
func (s Scheduler) reserveFor(provider account.Provider, accountID string) float64 {
	if len(s.reserves) == 0 {
		return 0
	}
	key := ScoreKey(provider, accountID)
	if fraction, ok := s.reserves[key]; ok {
		return fraction
	}
	if fraction, ok := s.reserves[accountID]; ok {
		return fraction
	}
	for _, alias := range s.accountAliases[key] {
		if fraction, ok := s.reserves[alias]; ok {
			return fraction
		}
		for configured, fraction := range s.reserves {
			if strings.EqualFold(configured, alias) {
				return fraction
			}
		}
	}
	return 0
}

// countFor reads a per-account count keyed by ScoreKey, falling back to the
// bare account ID for callers that still key by ID.
func countFor(counts map[string]int, key, accountID string) int {
	if count, ok := counts[key]; ok {
		return count
	}
	return counts[accountID]
}

// WithLiveSessionCounts attaches per-account current-concurrency counts
// (sessions with in-flight work right now) for the endgame taper. Keys may be
// ScoreKeys or bare account IDs, like WithSessionCounts.
func (s Scheduler) WithLiveSessionCounts(counts map[string]int) Scheduler {
	next := s
	next.liveSessionCounts = map[string]int{}
	for accountKey, count := range counts {
		next.liveSessionCounts[accountKey] = count
	}
	return next
}

// WithClosedAccounts attaches the local accounts, keyed by ScoreKey, whose
// admission queue is closed: they take no new session and no refugee, and
// every session already on them stays. The map must not be mutated
// afterwards.
func (s Scheduler) WithClosedAccounts(closed map[string]bool) Scheduler {
	next := s
	next.closedAccounts = closed
	return next
}

// ForModel narrows scoring to a model's dedicated quota pool when one exists.
// If the model maps to no known pool (the common case: regular models), the
// base scheduler is returned unchanged so account-wide quota is used. When a
// pool exists but a given account lacks it, an account whose missing-bucket
// support is Unknown -- by default a Claude or Antigravity account
// (DefaultMissingModelSupport) -- is scored on its account-wide budget; any
// other account scores zero so it is not picked for a model it cannot serve.
func (s Scheduler) ForModel(model string) Scheduler {
	key := ModelKey(model)
	// Set before the early return: Opus and Sonnet publish no pool of their
	// own (absentPoolInheritsAccountScore), so that return is the usual Opus
	// path.
	s.nonFableClaudeTurn = claudeFamilyPoolKeys[key] && key != claudeFablePoolKey
	s.fablePool = key == claudeFablePoolKey
	if key == "" || !s.hasModelScore(key) {
		return s
	}
	// Copy-then-modify: a literal here dropped reserves, the alias table and
	// the machine view, so WithReserves(...).ForModel(...) quietly released
	// the owner's holdout for exactly the model pool that was busy.
	next := s
	next.scores = make(map[string]Score, len(s.scores))
	for scoreKey, score := range s.scores {
		modelScore, ok := score.ModelScores[key]
		if !ok {
			if score.missingModelSupport() == ModelSupportUnknown {
				modelScore = score
				modelScore.ModelScores = nil
			} else {
				modelScore = Score{AccountID: score.AccountID, Provider: score.Provider, Headroom: 0, ShortHeadroom: 0}
			}
		}
		next.scores[scoreKey] = inheritAccountFacts(modelScore, score)
	}
	return next
}

// absentPoolInheritsAccountScore reports whether an account of this provider
// that publishes no window for a model pool is, by default, governed for that
// model by its account-wide windows -- rather than being unable to serve a
// pool another account exposes. It is DefaultMissingModelSupport's answer, for
// the call sites that hold only a provider (an exhaustion-mark key); where a
// score is in hand, its own declaration (Score.missingModelSupport) decides.
func absentPoolInheritsAccountScore(provider account.Provider) bool {
	return DefaultMissingModelSupport(provider) == ModelSupportUnknown
}

// inheritAccountFacts carries the ACCOUNT-level properties of a score into its
// model-scoped view.
//
// A model pool narrows which QUOTA a request draws from. It does not move the
// account to a different machine, repeal its weekly roll, or release its
// owner's holdout -- those are facts about the account, and a sub-score that
// silently redefines them makes the router answer a different question than
// the one it was asked.
//
// The weekly window is the one that broke in production. Providers publish
// per-model windows with no reset time (every live Claude account read
// opus-weekly reset_seconds=0), and WeeklyResetAt zero means "no weekly
// pacing" by construction -- so scoping deleted weeklyPace AND
// inEndgameWindow for every candidate simultaneously. With the primary signal
// dead across the whole field, the comparator fell through to the 5h window,
// which cannot see a weekly roll at all: an account holding 72% of its week
// and rolling in 13h ranked BELOW one holding 47% with 155h left, and its
// budget was forfeited on schedule while the daemon logged forfeit=0.919
// against it.
//
// Inheriting the weekly window cannot resurrect a model-exhausted account:
// Headroom and ShortHeadroom stay model-scoped, and exhausted() reads those.
// This only restores ORDERING among accounts the model pool already allows.
func inheritAccountFacts(modelScore, accountScore Score) Score {
	if modelScore.WeeklyResetAt.IsZero() {
		modelScore.WeeklyResetAt = accountScore.WeeklyResetAt
		modelScore.WeeklyHeadroom = accountScore.WeeklyHeadroom
	}
	// Remote decides local-vs-peer preference; a model sub-score that drops it
	// silently relocates a peer account into the local pool and hands it the
	// LocalPreferenceRatio advantage it must not have.
	modelScore.Remote = accountScore.Remote
	// Reserve is the owner's holdout. Losing it inside a model pool is how a
	// reserve that holds everywhere else quietly stops holding for the one
	// model that is actually busy.
	modelScore.Reserve = accountScore.Reserve
	// The holdout is a fraction of the ACCOUNT's week, so it is judged on the
	// account's measured remainder whichever model pool is asking, and lifted
	// on the account's roll (reserveReleased).
	modelScore.WeeklyRemaining = accountScore.WeeklyRemaining
	modelScore.accountWeeklyResetAt = accountScore.WeeklyResetAt
	// WeeklyOnly describes the provider's published window set, not the model's.
	modelScore.WeeklyOnly = accountScore.WeeklyOnly
	// The Fable meter is the account's: the Opus and Sonnet views that rank
	// on it carry no Fable window of their own.
	modelScore.FableSpent = accountScore.FableSpent
	// Provider: the proxy stamps it on the account score only, and §55's
	// measured drain inputs cover Claude accounts alone (drainReadsMeasured).
	// Without it the Fable pool's drain cap would still read the per-request
	// debit.
	modelScore.Provider = accountScore.Provider
	// The hold is set only for a saved reset that clears the general week,
	// which every model pool draws on, and the proxy sets it on the account
	// score only. Without it every model pool would rank a holder at 1x and
	// never drain it early -- on the Fable pool, which every Fable request
	// narrows to.
	modelScore.ResetHeldUntil = accountScore.ResetHeldUntil
	return modelScore
}

// HasModelPool reports whether the model maps to a dedicated quota pool present
// on at least one scored account.
func (s Scheduler) HasModelPool(model string) bool {
	key := ModelKey(model)
	return key != "" && s.hasModelScore(key)
}

// HasModelPoolFor reports whether a model pool is present within one provider's
// scores. Pool names are not globally authoritative: two providers can expose
// the same model spelling while grouping quota differently.
func (s Scheduler) HasModelPoolFor(provider account.Provider, model string) bool {
	if provider == "" {
		provider = account.ProviderCodex
	}
	key := ModelKey(model)
	if key == "" {
		return false
	}
	for _, score := range s.scores {
		if score.Provider != provider {
			continue
		}
		if _, ok := score.ModelScores[key]; ok {
			return true
		}
	}
	return false
}

func (s Scheduler) hasModelScore(key string) bool {
	for _, score := range s.scores {
		if _, ok := score.ModelScores[key]; ok {
			return true
		}
	}
	return false
}

// Pick chooses an account for placing work (a new session, a failover
// retry): the leading candidate of the shared comparator (weekly pace, local
// preference, live-session spread). Deterministic -- upstream's equal-pressure
// placement spreading is deliberately not ported; see PATCHES.md section 10.
func (s Scheduler) Pick(candidates []account.Account) (account.Account, error) {
	return s.pick(candidates, true)
}

// PickSteady picks with endgame drain disabled: no widening below the
// new-session threshold and no drain-first precedence. For consumers choosing
// a long-lived interactive default (sr auto-switch rotating the active
// profile) rather than routing a disposable request — parking a human on an
// account that is deliberately being run into the ground is the opposite of
// what rotation is for.
func (s Scheduler) PickSteady(candidates []account.Account) (account.Account, error) {
	return s.pick(candidates, false)
}

func (s Scheduler) pick(candidates []account.Account, allowEndgame bool) (account.Account, error) {
	// One clock for the whole sort: per-comparison clock reads would let the
	// endgame boundary move mid-sort and break strict weak ordering.
	//
	// The ordering itself lives in compare() in explain.go, so Pick and Explain
	// cannot disagree -- a reporter with its own copy of this ladder is a second
	// ranking rule that drifts the first time this one changes.
	sorted, err := s.rankAt(candidates, allowEndgame, time.Now())
	if err != nil {
		return account.Account{}, err
	}
	return sorted[0], nil
}

func (s Scheduler) UsableForNewSession(provider account.Provider, accountID string) bool {
	return s.score(provider, accountID).usableForNewSession()
}

// UsableForStickySession reports whether an IDLE session already assigned to
// this account may stay on it. It is the same eligibility predicate as
// UsableForNewSession -- one rule, not a second retention threshold -- read on
// the MEASURED score instead of the live-debited one. The debit floors headroom
// at 0.01, so on any busy account it sits below MinShortHeadroomFloor and would
// evict the session the debit was only meant to steer new picks away from (a
// Codex account at 30% moved every idle session after ~15 routed requests).
//
// Warm sessions never consult this: they move only when Evicted (PATCHES.md
// section 20).
func (s Scheduler) UsableForStickySession(provider account.Provider, accountID string) bool {
	return s.measuredScore(provider, accountID).usableForNewSession()
}

func (s Scheduler) Exhausted(provider account.Provider, accountID string) bool {
	return s.score(provider, accountID).exhausted()
}

// Evicted reports whether an account must give up its sessions: a window is
// spent (Exhausted) or its weekly budget has reached the owner's holdout.
//
// The holdout is a HARD limit, the same as a spent 5h or weekly window: a
// session already on the account is evicted when it gets there, and nothing
// new is started on it. It is otherwise only a new-session refusal
// (usableForNewSession), which a warm session never consults -- so a long
// conversation would spend the holdout down to the real weekly limit.
//
// Exhausted is kept separate on purpose: it also drives exhaustion-mark
// lifecycles and "which window died" reporting, where a holdout is not a dead
// window.
func (s Scheduler) Evicted(provider account.Provider, accountID string) bool {
	return s.score(provider, accountID).evicted()
}

// QueueClosed reports whether the account's admission queue is closed to new
// sessions and refugees (PATCHES §72). Never a reason to move a session.
func (s Scheduler) QueueClosed(provider account.Provider, accountID string) bool {
	return s.score(provider, accountID).queueClosed
}

// InHoldout reports whether an account's weekly budget has reached its
// owner's holdout.
func (s Scheduler) InHoldout(provider account.Provider, accountID string) bool {
	return s.score(provider, accountID).inHoldout()
}

// LookupScore returns the stored score for an account and whether one is known.
//
// ScoreFor deliberately substitutes an optimistic default (fully healthy) for an
// unknown account, which is right for seeding a refresh but wrong for reporting:
// published as-is it asserts that an account the scheduler has never scored is
// at full capacity. Reporters -- notably the usage-status endpoint -- use this
// instead and omit the field entirely when ok is false, so absent means unknown
// rather than healthy.
func (s Scheduler) LookupScore(provider account.Provider, accountID string) (Score, bool) {
	score, ok := s.scores[ScoreKey(provider, accountID)]
	return score, ok
}

// KnownScore returns the measured score (measuredScore: no live debit, with
// the session counts and holdout the scheduler was built with) and whether a
// usage refresh or an exhaustion mark produced one. An unknown account reads
// as fully healthy, which status views must not mistake for measured quota
// (upstream #600).
func (s Scheduler) KnownScore(provider account.Provider, accountID string) (Score, bool) {
	_, ok := s.scores[ScoreKey(provider, accountID)]
	return s.measuredScore(provider, accountID), ok
}

func selectionTier(acct account.Account, score Score, now time.Time, allowEndgame bool) int {
	if acct.AuthMode == account.AuthModeOAuth && score.routableForNewSession(now, allowEndgame) {
		return 0
	}
	if acct.AuthMode == account.AuthModeAPIKey {
		return 1
	}
	// A holdout account ranks with the exhausted ones: when nothing is
	// routable, Pick falls back to this tier, and starting a session inside
	// the owner's holdout is exactly what the holdout forbids.
	if acct.AuthMode == account.AuthModeOAuth && !score.evicted() && !score.queueClosed && !score.pinOnly {
		return 2
	}
	if acct.AuthMode == account.AuthModeOAuth {
		return 3
	}
	return 4
}

// ScoreFor returns the stored score for an account, or an optimistic default
// (treated as fully healthy) when none is known. Callers use it to seed a
// refresh so accounts whose fresh usage could not be fetched preserve their
// last known score instead of being clobbered with low-confidence data.
// WeeklyPace exports the primary Claude ordering signal so cross-pool routing
// can rank one pool against another on the same basis Pick ranks accounts
// within a pool. Comparing pools on headroom instead would ignore the whole
// point of the signal: an account sitting on a large budget with a week to
// spend it is worth less right now than a smaller budget rolling tonight.
func (s Score) WeeklyPace(now time.Time) (float64, bool) {
	return s.weeklyPace(now)
}

// InEndgameWindow reports whether the account is within a day of its weekly
// roll, where budget expires whether or not anyone spends it. Exported because
// a pool's pace is dominated by any endgame account it holds, and a caller
// comparing pools needs to be able to say so rather than silently reporting a
// near-roll account as boundless capacity.
func (s Score) InEndgameWindow(now time.Time) bool {
	return s.inEndgameWindow(now)
}

// HoldsReset reports whether the account holds an unclaimed saved reset that
// the router is ranking it on (PATCHES §56): the hold has not expired and the
// switch is on. Exported so a reporter marks exactly the accounts that rank at
// ResetHoldPreferenceRatio instead of re-deriving it from the reset info.
func (s Score) HoldsReset(now time.Time) bool {
	return s.holdsReset(now)
}

// Burndown causes: why an account is in the burndown window, the one the
// drain tier, its taper and the steady-mode demotion run on.
const (
	// BurndownCauseRoll is the 24h weekly-roll window (inEndgameWindow).
	BurndownCauseRoll = "roll"
	// BurndownCauseReset is the early burndown of a reset holder below
	// ResetBurndownWeeklyRemaining of its week (inResetBurndown, PATCHES §56).
	BurndownCauseReset = "reset"
	// BurndownCausePace is the early burndown of an account whose own burn
	// would leave budget unspent at its roll (inPaceBurndown, PATCHES §110).
	BurndownCausePace = "pace"
)

// BurndownCause names why the account is in the burndown window, or "" when
// it is not. The roll wins over the others: that budget expires first whatever
// the reset or the burn does; the reset wins over the pace. A peer's account
// carries its owner's cause in its verdict, verbatim; one from a daemon too
// old to publish a cause reads "". Display and logging only -- routing reads
// inBurndownWindow and the verdict.
func (s Score) BurndownCause(now time.Time) string {
	if verdict, ok := s.peerVerdict(); ok {
		return verdict.BurndownCause
	}
	switch {
	case s.inEndgameWindow(now):
		return BurndownCauseRoll
	case s.inResetBurndown(now):
		return BurndownCauseReset
	case s.inPaceBurndown(now):
		return BurndownCausePace
	}
	return ""
}

// SecondsToWeeklyReset is how long until this account's weekly window rolls,
// taking whatever is left with it. Exported alongside WeeklyPace because a
// weekly-only provider cannot be ranked on pace: with no second window to
// price the weekly budget against, headroom-per-second collapses to
// 1/time-left and orders accounts by nothing but which window opened first.
// The deadline itself is the signal there. False means no weekly window, or
// one whose deadline has already elapsed.
func (s Score) SecondsToWeeklyReset(now time.Time) (float64, bool) {
	seconds := s.weeklyResetSecondsLeft(now)
	if seconds <= 0 {
		return 0, false
	}
	return seconds, true
}

func (s Scheduler) ScoreFor(provider account.Provider, accountID string) Score {
	if score, ok := s.scores[ScoreKey(provider, accountID)]; ok {
		return score
	}
	return Score{AccountID: accountID, Provider: provider, Headroom: 1, ShortHeadroom: 1}
}

// WithLiveDebits attaches per-account routed-request counts accumulated since
// the last usage refresh. score() debits LiveDebitPerRequest of headroom per
// routed request, so between refreshes the scheduler sees its OWN traffic
// draining the snapshot instead of herding every pick onto the same account.
// Keys are ScoreKey(provider, accountID); a bare account ID is read as a
// fallback, like WithSessionCounts.
func (s Scheduler) WithLiveDebits(debits map[string]int) Scheduler {
	next := s
	next.liveDebits = debits
	return next
}

// SessionSpreadWeight is how much one session CURRENTLY RUNNING on an account
// discounts its ranking value: one session costs ~13%, two ~23%, four ~37%.
//
// Sized to influence, not to decide. It must be able to move a pick between
// accounts of comparable pace, and must NOT be able to override a genuine pace
// difference -- an account whose budget is about to expire should still win
// over an idle account with a week to spend, however many sessions it carries.
const SessionSpreadWeight = 0.15

// spreadAdjusted discounts a ranking value by the account's LIVE concurrency.
//
// Live, not lifetime. Score.Sessions counts every sticky assignment ever
// written to the session store, which is never pruned (see the comment on
// activeSessionCountsByAccount in internal/proxy: "sticky assignments are
// never pruned, so its counts grow monotonically"). Measured on a
// week-old store those counts spanned 1..45, which turns this "±13% nudge"
// into a 6.7x multiplier swing (0.870 vs 0.129) decided by eight-day-old
// history rather than by anything the account is doing now -- and because
// nothing prunes, the skew only ever deepens. LiveSessions is bounded by real
// concurrency, which is the range this weight was sized against.
//
// Multiplicative, because pace spans orders of magnitude (~1e-6/s normally,
// far higher inside an endgame window) and any fixed subtraction would be
// noise at one end and decisive at the other. A ratio costs the same
// proportion everywhere.
//
// Ranking only. This deliberately does NOT touch Headroom or ShortHeadroom
// the way liveDebits does: those gate usableForNewSession at the floor, and a
// debit there would let merely-running sessions mark a healthy account
// unusable. liveDebits may do that because they model spend that really
// happened; an open session has spent nothing yet.
func (s Score) spreadAdjusted(value float64) float64 {
	if s.LiveSessions <= 0 {
		return value
	}
	return value / (1 + SessionSpreadWeight*float64(s.LiveSessions))
}

// score is the scheduler's working view of an account: the measured score plus
// the live debit for requests routed since the last usage refresh. Ranking and
// new-session eligibility read this; anything that EVICTS work reads
// measuredScore or a debit-free field (WeeklyRemaining) instead.
func (s Scheduler) score(provider account.Provider, accountID string) Score {
	score := s.measuredScore(provider, accountID)
	if score.Remote {
		// Our live debits count requests WE routed to OUR accounts. A peer
		// account arrives already debited for the targeted turns this daemon
		// sent it since the peer's body (decisionCandidates, ApplyLiveDebit);
		// its owner's live counts ride in its verdict.
		return score
	}
	return ApplyLiveDebit(score, countFor(s.liveDebits, ScoreKey(provider, accountID), accountID))
}

// ApplyLiveDebit takes count routed requests' worth of LiveDebitPerRequest off
// a score's headroom, short headroom and weekly headroom: the one debit, for a
// local account (Scheduler.score) and for a peer account this daemon routed
// targeted turns to since the peer's usage body was read (PATCHES §31, R3 F-E:
// without it every local account sank as it was used while a peer account
// stood at its measured value until the next body, and the ranking tilted to
// the peer between sweeps).
//
// A soft, self-correcting signal: it reorders picks and can push an account
// below the new-session threshold, but never onto (or off of) the exhaustion
// floor: routing our own optimism into an account is recoverable; falsely
// marking it exhausted, or resurrecting a genuinely exhausted one, is not.
func ApplyLiveDebit(score Score, count int) Score {
	if count <= 0 {
		return score
	}
	debit := LiveDebitPerRequest * float64(count)
	before := score
	if score.Headroom > 0.01 {
		score.Headroom = math.Max(0.01, score.Headroom-debit)
	}
	// A Claude account with no 5h reading holds ShortHeadroom at 1 because no
	// 5h constraint was read (scoreFromLimitWindows), so there is no window for
	// the debit to model, and debiting it would refuse the account at the floor
	// after 48 requests between readings. The headroom and weekly debits still
	// spread load across it.
	if score.ShortHeadroom > 0.01 && !(score.Provider == account.ProviderClaude && score.ShortWindowSeconds <= 0) {
		score.ShortHeadroom = math.Max(0.01, score.ShortHeadroom-debit)
	}
	if score.WeeklyHeadroom > 0.01 {
		// Keeps weekly pace and belowWeeklyReserve responsive to our own
		// routing between usage refreshes, same as the headroom debits. The
		// endgame taper reads it only for an account §55 does not cover
		// (drainInputs): the debit is a 5h-window unit sized for spreading,
		// ~200x what a request really costs that window, and on the week it
		// held a busy draining account at one session.
		score.WeeklyHeadroom = math.Max(0.01, score.WeeklyHeadroom-debit)
	}
	score.weeklyDebit += before.WeeklyHeadroom - score.WeeklyHeadroom
	score.shortDebit += before.ShortHeadroom - score.ShortHeadroom
	return score
}

// measuredScore is the score from the last usage refresh, with no live debit
// applied, decorated with the per-account facts the scheduler was built with:
// parked and live session counts and the owner's holdout. The debit is our own
// optimism about requests already in flight, not evidence that an account is
// empty, so decisions that evict work rather than merely reorder picks read
// this instead of score().
//
// Counts are keyed by ScoreKey(provider, id) -- the proxy's
// SchedulerSessionCounts keys them that way so one email that is both a Codex
// account and a Claude profile does not share a count -- with the bare ID read
// as a fallback for callers that still key by ID. Reading only one spelling
// silently zeroes the other's counts: no error, the spread and the endgame
// taper just stop seeing sessions.
func (s Scheduler) measuredScore(provider account.Provider, accountID string) Score {
	key := ScoreKey(provider, accountID)
	score, ok := s.scores[key]
	if !ok {
		score = Score{AccountID: accountID, Provider: provider, Headroom: 1, ShortHeadroom: 1}
	}
	if ok && score.Remote {
		// A peer's account, merged into this daemon's ranking (PATCHES §31).
		// Its session counts, live concurrency and holdout are the OWNING
		// daemon's -- published in its verdict and carried on the score -- and
		// this daemon's tables are keyed by its own accounts, so decorating it
		// here would overwrite the peer's live count with a zero and erase the
		// very concurrency the 2026-09-27 incident could not see. So is its
		// reset choice.
		score.loading = s.loads(score.ResetChoice)
		return score
	}
	score.ResetChoice = s.resetChoices[key]
	score.loading = s.loads(score.ResetChoice)
	if s.sessionCounts != nil {
		score.Sessions = countFor(s.sessionCounts, key, accountID)
	}
	if s.liveSessionCounts != nil {
		score.LiveSessions = countFor(s.liveSessionCounts, key, accountID)
	}
	if s.paces != nil {
		score.Pace = s.paces[key]
	}
	score.queueClosed = s.closedAccounts[key]
	score.pinOnly = s.pinOnly[accountID]
	if s.reserves != nil {
		score.Reserve = s.reserveFor(provider, accountID)
		if score.reserveReleased(time.Now()) {
			score.Reserve = 0
		}
	}
	return score
}

// reserveReleased reports whether the owner's holdout is lifted at now
// (PATCHES §60): the account's week rolls within claudeEndgameWindowSeconds,
// the day the drain spends whatever an account still holds. The roll refills
// the week and wipes a holdout kept through it, so for that day the account is
// unreserved everywhere the reserve is read -- the new-session gate, the
// drain, its taper and the hard holdout alike. A reading carried past the roll
// describes a week that is gone, and stays released like inHoldout fails open
// on it; an unknown roll (zero) keeps the reserve.
func (s Score) reserveReleased(now time.Time) bool {
	if !ReleaseReserveFinalDay() {
		return false
	}
	roll := s.WeeklyResetAt
	if !s.accountWeeklyResetAt.IsZero() {
		roll = s.accountWeeklyResetAt
	}
	return !roll.IsZero() && roll.Sub(now) <= time.Duration(claudeEndgameWindowSeconds)*time.Second
}

// usableForNewSession gates on the SHORT window only. The 5h window is what
// decides whether an account can sustain a session starting now; the weekly
// window decides which account is preferable, and that belongs in ranking
// (weeklyPace) rather than eligibility.
//
// Applying the 40% floor to the long/weekly window meant refusing to start
// sessions on an account still holding 39% of its week -- stranding roughly
// the last 40% of every account's weekly budget, spendable only by sessions
// already stuck to it or by the endgame widening. On this pool it read as
// "0/6 above floor" while five of the six had a full or near-full 5h window.
//
// exhausted() stays as a hard exclusion because selectionTier consults
// routability BEFORE exhaustion: without it an account whose weekly window is
// at literal zero would rank top and 429 every request.
// paceEqualityBand is the relative difference below which two paces count as
// the same, so a sharper signal decides instead.
//
// Exact float comparison here is a bug, not a precision win. Every score
// stamps WeeklyResetAt from its own time.Now(), so two accounts holding
// identical weekly budget and rolling at the same time still produce paces
// differing in the tenth decimal -- and that microsecond of clock noise then
// outranks headroom. It picked a 90%-consumed account over a 90%-FREE one
// because their paces were "different". Anything inside this band falls
// through to headroom, which is the signal that actually distinguishes them.
const paceEqualityBand = 0.01

// rankBucket quantises a ranking value into log-spaced buckets one
// paceEqualityBand wide, so two values inside the same band compare equal and
// a sharper signal decides instead.
//
// Bucketing rather than a pairwise tolerance, because a tolerance is NOT
// transitive and the comparator must be a strict weak ordering. With paces
// 1.000e-6 / 1.008e-6 / 1.016e-6 each neighbouring pair sits inside a 1% band
// while the outer pair does not, giving A~B, B~C, C>A -- and sort.SliceStable
// then returns whichever element the candidate list happened to leave in
// front. Measured across the six permutations of one three-account list, that
// bug elected all three accounts. A bucket index is a function of a single
// value, so equality is an equivalence relation by construction and no
// permutation can change the winner.
//
// Log-spaced for the same reason the band was relative: pace spans orders of
// magnitude (~1e-6/s normally, far higher inside an endgame window), so a
// linear bucket width would be meaningless at one end and decisive at the
// other. Each bucket is a constant RATIO step, costing the same proportion
// everywhere.
func rankBucket(value float64) int {
	if value <= 0 {
		// Non-positive values carry no rate information and must all compare
		// equal to each other, below every positive bucket.
		return math.MinInt32
	}
	return int(math.Floor(math.Log(value) / math.Log(1+paceEqualityBand)))
}

// shortExpiry is the 5h-window counterpart of weeklyPace: remaining short
// budget per second until that window resets. Prefers ExpiryPressure where it
// was computed, and derives the same quantity from the short window where it
// was not -- Claude-family scores deliberately leave ExpiryPressure at zero so
// it cannot drive their primary ordering, but they still have a 5h window and
// it still matters once weekly pace has tied.
func (s Score) shortExpiry() float64 {
	if s.ExpiryPressure > 0 {
		return s.ExpiryPressure
	}
	if s.ShortResetAfterSeconds <= 0 {
		return 0
	}
	return s.ShortHeadroom / float64(s.ShortResetAfterSeconds)
}

// UsableForNewSession is the router's own eligibility answer for a single
// score. Exported so callers stop keeping their own copy: cmd/subrouter and
// internal/proxy each carried a private `Headroom >= 0.40 && ShortHeadroom >=
// 0.40`, which meant the CLI's "usable" column, the status label, and the
// account the proxy marked Active all disagreed with where traffic actually
// went. One predicate, one answer.
func (s Score) UsableForNewSession() bool {
	return s.usableForNewSession()
}

func (s Score) usableForNewSession() bool {
	if s.exhausted() || s.ShortHeadroom < MinShortHeadroomFloor {
		return false
	}
	if s.belowWeeklyReserve() {
		return false
	}
	return s.shortBurnSustainable()
}

// shortBurnSustainable is usableForNewSession's rate clause on its own: false
// when the burn already observed in the 5h window, carried to its reset, would
// spend more than the window has left. The endgame taper reads it too -- a
// drain account keeps its final task past this clause, but not a dogpile
// (endgameAllowedSessions).
func (s Score) shortBurnSustainable() bool {
	// Weekly-only accounts are gated by exhaustion alone, checked by the
	// caller.
	//
	// A Codex weekly window is a TUMBLING window: it starts on first use and
	// then counts down to zero at 1s/s whether or not anyone spends (measured
	// on a live account reading used=0 while its reset fell 577390 -> 574981
	// over 37 minutes). So budget on an opened account does not keep -- it
	// expires at the roll. Applying a flat headroom floor here, as the fallback
	// below used to, refused every account still holding 39% of
	// its week and forfeited that budget outright: the exact stranding
	// usableForNewSession's own comment claims to have removed, reintroduced for
	// Codex because scoreFromLimitWindows aliases ShortHeadroom to the weekly
	// level while dropping the timing that would have made the rate reachable.
	//
	// No burn rate here either: the rate gate exists to stop a session
	// starting on a window that will not survive it, which is a question about
	// a 5h bucket refilling shortly. Over seven days it just re-refuses the
	// same budget more slowly.
	if s.WeeklyOnly {
		return true
	}
	// Without window timing there is no rate to compute. Admit the account on
	// the exhaustion floor the caller checked rather than inventing a level:
	// a flat threshold here was refusing accounts holding a third of their
	// window purely because the daemon could not see how much of that window
	// was left, which is a gap in OUR bookkeeping and not a property of the
	// account. The upstream's own 429 is the backstop if the guess is wrong.
	if s.ShortWindowSeconds <= 0 || s.ShortResetAfterSeconds <= 0 {
		return true
	}
	elapsed := float64(s.ShortWindowSeconds - s.ShortResetAfterSeconds)
	// Damp the start of the window. Ten percent spent in the first five
	// minutes is an ordinary session warming up, but extrapolated off five
	// minutes of evidence it projects to twenty times the window and would
	// refuse every account in the fleet. Pretending at least a fifth of the
	// window has run keeps early bursts from being read as trends.
	if floor := float64(s.ShortWindowSeconds) * shortBurnMinElapsedFraction; elapsed < floor {
		elapsed = floor
	}
	consumed := 1 - s.ShortHeadroom
	if consumed <= 0 {
		return true
	}
	// Project the observed burn across the rest of the window. If it would
	// spend more than what is left, something already in flight is ripping
	// through this account -- a monster session, a fleet pointed at it -- and
	// adding another just races both into the wall. Refused however good its
	// weekly numbers look, because the weekly window is not what it is about
	// to run out of.
	projected := (consumed / elapsed) * float64(s.ShortResetAfterSeconds)
	return projected <= s.ShortHeadroom*ShortBurnTolerance
}

// routableForNewSession widens usableForNewSession for endgame drain: an
// account inside its burndown window keeps taking new sessions below the
// usual headroom threshold because its remaining budget is about to be lost
// anyway (or, for a reset holder, refilled by the claim; or, on its pace,
// left unspent at the roll). Everything else keeps the conservative gate.
func (s Score) routableForNewSession(now time.Time, allowEndgame bool) bool {
	if verdict, ok := s.peerVerdict(); ok {
		return verdict.Eligible
	}
	if s.queueClosed || s.pinOnly {
		return false
	}
	return s.usableForNewSession() || (allowEndgame && (s.endgameDrainEligible(now) || s.loadEligible(now)))
}

// weeklyPace is spendable weekly budget per second until the roll — the
// primary Claude ordering signal. Reported false when the score carries no
// weekly information (codex scores, carried legacy seeds) or when the
// deadline has already passed (a stale carried seed whose real window rolled),
// which falls the comparator back to the legacy ExpiryPressure ordering.
func (s Score) weeklyPace(now time.Time) (float64, bool) {
	seconds := s.weeklyResetSecondsLeft(now)
	if seconds <= 0 {
		return 0, false
	}
	return s.WeeklyHeadroom / seconds, true
}

func (s Score) weeklyResetSecondsLeft(now time.Time) float64 {
	if s.WeeklyResetAt.IsZero() {
		return 0
	}
	return s.WeeklyResetAt.Sub(now).Seconds()
}

// endgameDrainEligible marks an account that should be aggressively spent:
// in its burndown window (within a day of its weekly roll, or an early
// burndown on a saved reset or on its pace: inBurndownWindow), not exhausted
// (the 5h gate and upstream 429 marks still apply through exhausted()), and
// below its concurrency allowance. The allowance shrinks with remaining
// budget so the account glides to exhaustion carrying roughly one final task
// rather than being dogpiled the moment it enters the window; at the cap,
// traffic spills to the next-draining account, then to steady-state ones.
// Sessions must be a live-concurrency count (the proxy joins sticky
// assignments against its in-flight tracker) — an all-time assignment count
// grows monotonically and would keep this permanently false. Deliberately not
// gated on Fresh: staleness is bounded by the per-assignment usage refresh
// and by the absolute WeeklyResetAt deadline, and a wrong drain pick is
// corrected by the upstream's own limit response. (The pace burndown's entry
// does read Fresh: its projection is not bounded by a deadline.)
// A reserved account at its holdout is excluded here. Inside the 24h roll
// window the holdout is already lifted (reserveReleased, PATCHES §60), so the
// clause binds in an early burndown, which can start days before the roll:
// there the holdout would otherwise be drained with a week still to run.
func (s Score) endgameDrainEligible(now time.Time) bool {
	if verdict, ok := s.peerVerdict(); ok {
		return verdict.Burndown
	}
	return s.inBurndownWindow(now) &&
		// The floor is absolute, and this is the case it was written for.
		// MinShortHeadroomFloor says in as many words that however close the
		// reset is, an account with almost nothing left must not take a new
		// session -- but routableForNewSession ORs this predicate with
		// usableForNewSession, so drain eligibility used to route around the
		// very floor that names it. A Codex account holding 1% of its week
		// was promoted to selection tier 0 and then ranked above every
		// healthy account: chosen because it was nearly empty. For an account
		// §55 covers, the floor reads the measured 5h meter (drainInputs). A
		// Claude account with no 5h reading has no 5h meter here (ShortHeadroom
		// 1), so it drains to its weekly wall, where exhausted() stops it.
		s.drainInputs().ShortHeadroom >= MinShortHeadroomFloor &&
		s.LiveSessions < s.endgameAllowedSessions(now) &&
		!s.belowWeeklyReserve()
}

// belowWeeklyReserve reports whether spending this account would eat into the
// holdout kept back for its owner.
//
// Fails OPEN when the weekly budget is unknown: a zero WeeklyHeadroom on a
// non-exhausted score means "no weekly window was measured", not "the week is
// gone" -- a genuinely spent week drives Headroom to zero and exhausted()
// catches it first. Refusing traffic on missing bookkeeping would turn a
// telemetry gap into an outage, which is the wrong direction for a mechanism
// whose whole job is keeping a way back in.
func (s Score) belowWeeklyReserve() bool {
	if s.Reserve <= 0 || s.WeeklyHeadroom <= 0 {
		return false
	}
	return s.WeeklyHeadroom <= s.Reserve
}

// endgameAtCap is the demoted counterpart: still draining, but already
// carrying its session allowance, so it must not win on pace (a near-roll
// reset makes pace enormous) and instead ranks below every other routable
// account until a slot frees or it exhausts.
func (s Score) endgameAtCap(now time.Time) bool {
	if verdict, ok := s.peerVerdict(); ok {
		return verdict.AtCap
	}
	return s.inBurndownWindow(now) && s.LiveSessions >= s.endgameAllowedSessions(now)
}

// endgameUrgency is how fast an account's weekly roll is arriving: the
// reciprocal of the seconds left in the window. It is the drain-mode
// counterpart of weeklyPace and deliberately carries NO headroom term --
// between two accounts that are both about to forfeit whatever they still
// hold, the one whose deadline lands first is the one whose budget cannot
// wait, however much more the other is sitting on. Headroom still decides
// below, once urgency has tied.
func (s Score) endgameUrgency(now time.Time) float64 {
	seconds := s.weeklyResetSecondsLeft(now)
	if seconds <= 0 {
		return 0
	}
	return 1 / seconds
}

// peerVerdict is the published verdict a Remote score carries, when it carries
// one. A Remote score without one comes from a peer too old to publish it and
// keeps the re-derived predicates -- absence is neither eligible nor
// ineligible (PATCHES §31) -- and a local score never consults it.
func (s Score) peerVerdict() (NewSessionVerdict, bool) {
	if !s.Remote || s.PeerVerdict == nil {
		return NewSessionVerdict{}, false
	}
	return *s.PeerVerdict, true
}

func (s Score) inEndgameWindow(now time.Time) bool {
	seconds := s.weeklyResetSecondsLeft(now)
	return seconds > 0 &&
		seconds <= float64(claudeEndgameWindowSeconds) &&
		!s.exhausted()
}

// holdsReset reports whether the account holds an unclaimed saved reset at
// now (PATCHES §56): the switch is on and the hold has not expired. The
// expiry is re-checked here, at decision time, because a seed carries the
// hold forward from a read that may be days old.
func (s Score) holdsReset(now time.Time) bool {
	return ResetHoldPriority() && !s.ResetHeldUntil.IsZero() && now.Before(s.ResetHeldUntil)
}

// inResetBurndown is a reset holder's early burndown (PATCHES §56): under
// ResetBurndownWeeklyRemaining of its MEASURED week left, on a current weekly
// reading, not exhausted. Claiming the reset refills the week whatever is left
// in it, so that last quarter is spent before the claim rather than wiped by
// it, and the grant lapses at its expiry. It drains now, under the same taper,
// floor and reserve as the 24h window.
//
// Local only: WeeklyRemaining is not published, and a peer's early burndown is
// its owner's call, read from its verdict like any other burndown. Zero
// WeeklyRemaining is unmeasured, never "spent", and stays out. An account
// chosen for a reset is loaded in its holder's order instead (PATCHES §95.2),
// which the drain's soonest-roll-first order would turn round.
func (s Score) inResetBurndown(now time.Time) bool {
	return !s.Remote && !s.loading && s.holdsReset(now) &&
		s.WeeklyRemaining > 0 && s.WeeklyRemaining < ResetBurndownWeeklyRemaining &&
		s.weeklyResetSecondsLeft(now) > 0 &&
		!s.exhausted()
}

// inPaceBurndown is the early burndown of an account projected to waste budget
// (PATCHES §110): its own burn, measured, spends less than its week holds by
// its roll, which lies past the 24h roll window and within EarlyDrainHours.
// It drains now, under the same taper, floor and reserve as the 24h window,
// behind every account whose roll lands first (endgameUrgency). The sessions
// it takes raise its burn, and it leaves the window once that burn would
// spend the week by the roll; its warm sessions stay.
//
// The projection is allowance's: Danger, on the drain's inputs, so the
// owner's reserve is never counted as waste. A reading that is not Fresh may
// be hours old, and an account the tracker has not measured (its first 30
// minutes after a restart) stays out: unmeasured fails closed here. The
// tracker measures local Claude accounts only, and a peer's early burndown is
// its owner's call, read from its verdict like any other burndown. An account
// chosen for a saved reset, for either meter, is loaded in its holder's order
// instead (PATCHES §95.2): a Fable choice is kept off Opus work that would eat
// the week under its Fable meter.
//
// 2026-11-27 owner, approving "an account projected to waste budget joins the
// drain when that projection appears, not at 24 hours": "Yeah that's a good
// fix. Go ahead and do it."
func (s Score) inPaceBurndown(now time.Time) bool {
	if !EarlyDrain() || s.Remote || s.ResetChoice.Meter != "" || !s.Fresh || s.exhausted() {
		return false
	}
	seconds := s.weeklyResetSecondsLeft(now)
	if seconds <= float64(claudeEndgameWindowSeconds) || seconds > float64(EarlyDrainHours())*3600 {
		return false
	}
	return s.allowance(now).Danger
}

// inBurndownWindow is the window the drain tier runs in: the 24h roll, a
// reset holder's early burndown, or an account's early burndown on its pace.
// Drain eligibility, the taper cap, the published allowance and the
// steady-mode demotion read it; what is about the roll itself (the Fable
// reservation's decay when §57's switch is on, endgameUrgency) stays on the
// roll.
func (s Score) inBurndownWindow(now time.Time) bool {
	return s.inEndgameWindow(now) || s.inResetBurndown(now) || s.inPaceBurndown(now)
}

// drainReadsMeasured is the scope of PATCHES §55: with its switch on (the
// default), a local Claude account that is not weekly-only and carries no
// owner reserve sizes its drain cap, 5h clamp and 5% floor on the measured
// meters. A peer's account is its owner's to size. A weekly-only account has
// no 5h window to brake the extra sessions, nobody has measured a Codex
// session's burn, and a reserved account keeps the debited ladder that stops
// short of its holdout.
func (s Score) drainReadsMeasured() bool {
	return EndgameMeasuredInputs() && !s.Remote && !s.WeeklyOnly &&
		s.Provider == account.ProviderClaude && s.Reserve <= 0
}

// drainInputs is the score the drain's cap and floor read: for an account
// drainReadsMeasured covers, its weekly and 5h headroom as the last usage
// reading left them, without our own per-request debit; otherwise the score
// as debited.
func (s Score) drainInputs() Score {
	if s.drainReadsMeasured() {
		s.WeeklyHeadroom += s.weeklyDebit
		s.ShortHeadroom += s.shortDebit
		s.weeklyDebit, s.shortDebit = 0, 0
	}
	return s
}

func (s Score) endgameAllowedSessions(now time.Time) int {
	return s.allowance(now).Sessions
}

func (s Score) evicted() bool {
	return s.exhausted() || s.inHoldout()
}

// inHoldout is the hard holdout: the MEASURED weekly remainder has reached the
// owner's reserve. belowWeeklyReserve stays the soft new-session gate; it reads
// WeeklyHeadroom, which is reduced by live debits and, with §57's switch on,
// by the Fable reservation -- conservative for starting a session, wrong for
// evicting one.
func (s Score) inHoldout() bool {
	if s.Reserve <= 0 || s.WeeklyRemaining <= 0 {
		return false
	}
	// A reading from before the week rolled is carried forward when a usage
	// fetch fails. Its remainder describes a week that no longer exists, and
	// as a HARD limit it would evict sessions from an account that just got a
	// fresh week. Stale fails open, like unmeasured.
	if !s.WeeklyResetAt.IsZero() && !time.Now().Before(s.WeeklyResetAt) {
		return false
	}
	return s.WeeklyRemaining <= s.Reserve
}

func (s Score) exhausted() bool {
	return s.Headroom <= 0 || s.ShortHeadroom <= 0
}

// ExhaustionClearsAt reports when the measured windows stop holding this
// score exhausted (upstream #530). A score that is not exhausted returns
// (zero, true). An exhausted score returns its windows' latest reset, or false
// when any exhausting window's reset is unknown (including scores with no
// window evidence at all, such as a synthetic zero for a missing model pool).
// It reads exhausted(), not evicted(): an owner's holdout is not a window that
// resets.
func (s Score) ExhaustionClearsAt() (time.Time, bool) {
	if !s.exhausted() {
		return time.Time{}, true
	}
	if s.ExhaustedResetUnknown || s.ExhaustedResetAt.IsZero() {
		return time.Time{}, false
	}
	return s.ExhaustedResetAt, true
}
