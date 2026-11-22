package selectacct

import (
	"strings"
	"sync/atomic"
	"time"
)

type LimitWindow struct {
	Name               string
	UsedPercent        float64
	LimitWindowSeconds int64
	ResetAfterSeconds  int64
	// ResetAt is the absolute reset time when the upstream reported one; zero
	// means unknown. It feeds ExhaustionClearsAt (upstream #530), and a window
	// whose ResetAt has already passed is a reading from before its reset: it
	// is skipped when scoring (liveWindow), so a stale 100% cannot keep the
	// account cooked past the reset (upstream 4e80ede7).
	ResetAt time.Time
	// Feature is the upstream limit_name of the model-specific quota pool this
	// window belongs to (e.g. "GPT-5.3-Codex-Spark"); empty for account-wide
	// windows.
	Feature string
	// SpendCap marks a billed spend cap -- a usage-based Codex seat's
	// spend_control (PATCHES.md §42) -- rather than a quota that expires. It
	// gates Headroom like any window, but it is never the weekly binding:
	// unspent cap is money not spent, not budget forfeited at a roll, so it
	// earns no weekly pace and no endgame drain.
	SpendCap bool
}

// ScoreFromLimitWindows computes an account's base score from its account-wide
// windows, plus a per-feature score for every model-specific quota pool present
// in the windows. Model-specific windows are excluded from the base score so a
// request for a regular model is not penalized by an unrelated pool's usage, and
// a request for a metered model (e.g. Spark) is scored only against its own pool.
// This is fully general: any additional rate limit the upstream reports becomes
// its own pool, keyed by the limit name, with no per-model special cases.
func ScoreFromLimitWindows(accountID string, sessions int, windows []LimitWindow) Score {
	return ScoreFromLimitWindowsAt(accountID, sessions, windows, time.Now())
}

// ScoreFromLimitWindowsAt anchors the weekly reset deadlines to the supplied
// clock; the windows' ResetAfterSeconds are durations relative to it.
func ScoreFromLimitWindowsAt(accountID string, sessions int, windows []LimitWindow, now time.Time) Score {
	claude := claudeWeeklyContextFromWindows(windows, now)
	base := scoreFromLimitWindows(accountID, sessions, filterWindowsByModelKey(windows, ""), "", claude, now)
	var modelScores map[string]Score
	for _, key := range distinctModelKeys(windows) {
		if modelScores == nil {
			modelScores = make(map[string]Score)
		}
		modelScores[key] = scoreFromLimitWindows(accountID, sessions, filterWindowsByModelKey(windows, key), key, claude, now)
	}
	base.ModelScores = modelScores
	base.FableSpent = claudeFableMeterSpent(windows, now)
	return base
}

// claudeFableMeterSpent is Score.FableSpent: the used fraction of the
// account's own Fable weekly meter. The Fable pool's window set also holds a
// copy of every account-wide window (the proxy and sr duplicate them into each
// feature, so the pool's headroom sees the general budget); a copy is not the
// meter and is skipped, so a spent general week never reads as spent Fable.
// No Fable window reads 0, and so does the unused placeholder (0% used): an
// unknown meter ranks like an untouched one. So does a meter whose reported
// reset (ResetAt) has passed: that reading is from before the roll, and the
// meter has refilled since (liveWindow, upstream 4e80ede7), as the Fable pool's
// own score and claudeWeeklyContextFromWindows already read it.
func claudeFableMeterSpent(windows []LimitWindow, now time.Time) float64 {
	spent := 0.0
	for _, window := range windows {
		if limitWindowModelKey(window) != claudeFablePoolKey || !isWeeklyWindow(window) || !liveWindow(window, now) ||
			copiesAccountWindow(window, windows) {
			continue
		}
		if used := clampPercent(window.UsedPercent) / 100; used > spent {
			spent = used
		}
	}
	return spent
}

// copiesAccountWindow reports whether window is an account-wide window
// duplicated into a feature: the same reading under the same name.
func copiesAccountWindow(window LimitWindow, windows []LimitWindow) bool {
	for _, base := range windows {
		if limitWindowModelKey(base) == "" && base.Name == window.Name && base.UsedPercent == window.UsedPercent &&
			base.LimitWindowSeconds == window.LimitWindowSeconds && base.ResetAfterSeconds == window.ResetAfterSeconds {
			return true
		}
	}
	return false
}

// Claude weekly-pace scheduling. A Claude subscription's binding budget is
// weekly: the plan is balanced for ~25h/wk of use, heavy users burn 3x that,
// and unspent weekly quota is lost at the roll. The 5h window self-heals many
// times a day, so it gates usability (ShortHeadroom) but must not drive
// ranking. Claude-family scores therefore carry WeeklyHeadroom and
// WeeklyResetAfterSeconds, and the Pick comparator orders them by pace —
// spendable budget per second until the roll — instead of ExpiryPressure.
const (
	claudeFablePoolKey = "claudefable"
	// claudeFableSliceOfWeekly is the fraction of the weekly limit the Fable
	// pool may consume (Anthropic caps Fable at 50% of the weekly budget).
	// With the Fable reservation on (FableReservation, PATCHES §57), non-fable
	// traffic reserves this much of an account's remaining general budget per
	// unit of unspent fable meter, steering opus/sonnet toward fable-consumed
	// accounts so fable capacity is not stranded behind an exhausted general
	// budget.
	//
	// The reservation is OFF by default. The owner, 2026-10-15: "Fable
	// allocation isn't important anymore! Eventually anthropic will release a
	// new fable and the fable weekly limit will matter again but for now it's
	// all about the opus". Off, a non-fable pool's WeeklyHeadroom is its
	// measured binding remainder. Turn it back on
	// (SUBROUTER_FABLE_RESERVATION=1) when a new Fable ships whose weekly limit
	// binds again, so Fable capacity is worth keeping out of Opus's reach.
	claudeFableSliceOfWeekly = 0.5
	// claudeEndgameWindowSeconds is how close to the weekly roll an account
	// enters drain mode: unspent budget is about to be lost, so it takes
	// routing priority. With the Fable reservation on (PATCHES §57) it is
	// also the span across which the reservation decays to zero (capacity
	// that cannot be spent before the roll is not worth reserving); off, the
	// decay never runs.
	claudeEndgameWindowSeconds = int64(24 * 60 * 60)
	// claudeEndgameSessionStep sizes the drain-mode concurrency taper: an
	// endgame account admits ceil(spendable/step) concurrent sessions (capped
	// below), spendable being its weekly headroom above the owner's reserve
	// (for an account §55 covers, the measured meter rather than our own
	// per-request debit: drainReadsMeasured), so it glides to its holdout --
	// or to exhaustion, unreserved -- carrying roughly one final task instead
	// of being dogpiled by every new session at once (endgameAllowedSessions).
	//
	// The step governs the END of the budget; claudeEndgameMaxSessions governs
	// everything above it. At the original 0.25 that was inverted -- the step
	// bound across the whole range and the cap could never bind at all
	// (WeeklyHeadroom <= 1, so ceil(1/0.25) is already 4), which read as a
	// 4-session allowance while actually granting 2. It also implied one
	// session consumes a quarter of a weekly budget; measured accounts on this
	// pool spend 1-15% of their week in total, so the throttle was costing
	// recoverable budget to guard against an overshoot that does not happen.
	//
	// At 0.0625 the taper acts over the last quarter of the budget -- 3 / 2 / 1
	// sessions below 18.75% / 12.5% / 6.25% remaining, so the glide to one
	// final task is intact -- and the cap governs above that. Endgame slots are
	// per-account and additive, so N draining accounts now offer 4N rather than
	// silently throttling each one and spilling the difference onto healthy
	// accounts that are days from rolling.
	claudeEndgameSessionStep = 0.0625
	claudeEndgameMaxSessions = 4
)

// endgameMeasuredInputsOff is the kill switch of PATCHES §55, inverted so the
// zero value is the shipped default: the drain's session cap, its 5h clamp
// and its 5% floor read the measured meters for the accounts
// drainReadsMeasured covers. The daemon sets it once at startup from
// SUBROUTER_ENDGAME_MEASURED_INPUTS.
var endgameMeasuredInputsOff atomic.Bool

// SetEndgameMeasuredInputs turns §55's measured inputs on or off for this
// process. The daemon calls it once at startup, before the first scheduler is
// built.
func SetEndgameMeasuredInputs(on bool) { endgameMeasuredInputsOff.Store(!on) }

// EndgameMeasuredInputs reports whether §55's measured inputs are on.
func EndgameMeasuredInputs() bool { return !endgameMeasuredInputsOff.Load() }

// resetHoldPriorityOff is the kill switch of PATCHES §56, inverted the same
// way: the zero value is the shipped default, under which an account holding
// an unclaimed saved reset ranks at ResetHoldPreferenceRatio and drains early
// below ResetBurndownWeeklyRemaining of its week. Off, holdsReset is false in
// this process -- its own holds and peer-published ones alike -- so ranking is
// exactly what it was before §56. A peer's burndown verdict is still honoured:
// that account is its owner's to size. The daemon sets it once at startup from
// SUBROUTER_RESET_HOLD_PRIORITY.
var resetHoldPriorityOff atomic.Bool

// SetResetHoldPriority turns §56's reset-hold priority on or off for this
// process. The daemon calls it once at startup, before the first scheduler is
// built.
func SetResetHoldPriority(on bool) { resetHoldPriorityOff.Store(!on) }

// ResetHoldPriority reports whether §56's reset-hold priority is on.
func ResetHoldPriority() bool { return !resetHoldPriorityOff.Load() }

// fableReservationOn is the switch of PATCHES §57. Unlike §55's and §56's it
// is NOT inverted: the zero value, off, is the shipped default, under which a
// non-Fable Claude pool's WeeklyHeadroom is its measured binding remainder
// with no Fable reservation taken out (claudeFableSliceOfWeekly says why it is
// off and when to turn it back on). On, that pool's WeeklyHeadroom is less
// claudeFableSliceOfWeekly of the unspent Fable meter, decayed across the last
// day before the roll. The Fable pool's own score is the same either way. The
// daemon sets it once at startup from SUBROUTER_FABLE_RESERVATION.
var fableReservationOn atomic.Bool

// SetFableReservation turns §57's Fable reservation on or off for this
// process. The daemon calls it once at startup, before the first scheduler is
// built.
func SetFableReservation(on bool) { fableReservationOn.Store(on) }

// FableReservation reports whether §57's Fable reservation is on.
func FableReservation() bool { return fableReservationOn.Load() }

// reserveKeptFinalDay is the kill switch of PATCHES §60, inverted like §55's
// and §56's so the zero value is the shipped default: an owner's holdout is
// lifted while the account's weekly window rolls within
// claudeEndgameWindowSeconds (Score.reserveReleased). The daemon sets it once
// at startup from SUBROUTER_RELEASE_RESERVE_FINAL_DAY.
var reserveKeptFinalDay atomic.Bool

// SetReleaseReserveFinalDay turns §60's final-day release on or off for this
// process. The daemon calls it once at startup, before the first scheduler is
// built.
func SetReleaseReserveFinalDay(on bool) { reserveKeptFinalDay.Store(!on) }

// ReleaseReserveFinalDay reports whether §60's final-day release is on.
func ReleaseReserveFinalDay() bool { return !reserveKeptFinalDay.Load() }

// fableSpentPreferenceOff is the kill switch of PATCHES §65, inverted like
// §56's so the zero value is the shipped default: an Opus or Sonnet turn ranks
// an account at FableSpentFactor of its weekly pace. Off, the factor is 1 for
// every account, local and peer alike. Score.FableSpent is still measured and
// published either way: it is a reading, and each reader decides. The daemon
// sets it once at startup from SUBROUTER_FABLE_SPENT_PREFERENCE.
var fableSpentPreferenceOff atomic.Bool

// SetFableSpentPreference turns §65's Fable-spent preference on or off for
// this process. The daemon calls it once at startup, before the first
// scheduler is built.
func SetFableSpentPreference(on bool) { fableSpentPreferenceOff.Store(!on) }

// FableSpentPreference reports whether §65's Fable-spent preference is on.
func FableSpentPreference() bool { return !fableSpentPreferenceOff.Load() }

// claudeWeeklyContext carries cross-pool facts every per-pool score needs:
// whether this is a Claude-family account at all, and how much of the fable
// slice is unspent — the non-fable pools cannot see the fable window after
// filtering, but their reservation, when it is on (PATCHES §57), depends on
// it.
type claudeWeeklyContext struct {
	family         bool
	fableRemaining float64
}

// claudeFamilyPoolKeys are the weekly quota pools a Claude subscription
// reports. Family membership is detected by ANY of them. In practice it is the
// fable window: Opus and Sonnet buckets come back null and publish no window
// (PATCHES §24), and the proxy always carries a fable window -- reported,
// probed, or its unused fallback. A family account's non-fable pools reserve
// the fable slice of the weekly budget while the Fable reservation is on
// (PATCHES §57, off by default), and its ranking ignores 5h expiry pressure.
var claudeFamilyPoolKeys = map[string]bool{
	claudeFablePoolKey: true,
	"claudeopus":       true,
	"claudesonnet":     true,
}

func claudeWeeklyContextFromWindows(windows []LimitWindow, now time.Time) claudeWeeklyContext {
	ctx := claudeWeeklyContext{}
	sawFable := false
	for _, window := range windows {
		key := limitWindowModelKey(window)
		if !claudeFamilyPoolKeys[key] || !isWeeklyWindow(window) {
			continue
		}
		// Family is the shape of the window set, so an elapsed window still
		// counts for it; its reading does not (liveWindow).
		ctx.family = true
		if key != claudeFablePoolKey || !liveWindow(window, now) {
			continue
		}
		// The fable pool's windows include both the fable meter and the
		// duplicated account-wide weekly; min over them is the fable capacity
		// actually reachable (a rich fable meter behind a drained general
		// budget cannot be spent, so it is not worth reserving either).
		remaining := 1 - clampPercent(window.UsedPercent)/100
		if !sawFable || remaining < ctx.fableRemaining {
			ctx.fableRemaining = remaining
		}
		sawFable = true
	}
	if !sawFable {
		// No fable window at all: the meter is unknown, so reserve nothing.
		// Zero is the conservative direction for routing — non-fable traffic
		// simply competes on the full general budget.
		ctx.fableRemaining = 0
	}
	return ctx
}

// claudeReservationDecay scales the fable reservation by time to the weekly
// roll: full reservation with a day or more left, fading linearly to zero at
// the roll itself, when preserving fable capacity no longer buys anything.
// It applies only while the reservation is on (FableReservation, PATCHES §57,
// off by default since the owner's 2026-10-15 "for now it's all about the
// opus").
func claudeReservationDecay(resetAfterSeconds int64) float64 {
	if resetAfterSeconds <= 0 {
		return 0
	}
	if resetAfterSeconds >= claudeEndgameWindowSeconds {
		return 1
	}
	return float64(resetAfterSeconds) / float64(claudeEndgameWindowSeconds)
}

// bindingWeekly finds the most constrained weekly (non-short) window: the one
// with the least remaining, preferring a window that reports a real reset
// time when remainders tie (synthetic 0%-used windows carry no reset). A spend
// cap is long but never binds (LimitWindow.SpendCap).
type weeklyBinding struct {
	remaining float64
	reset     int64
	// unstarted is Score.WeeklyUnstarted: the binding window's reading gives
	// its whole length as left (PATCHES §86).
	unstarted bool
	found     bool
}

func bindingWeekly(windows []LimitWindow, now time.Time) weeklyBinding {
	binding := weeklyBinding{}
	for _, window := range windows {
		if !isWeeklyWindow(window) || window.SpendCap || !liveWindow(window, now) {
			continue
		}
		remaining := 1 - clampPercent(window.UsedPercent)/100
		// >=, as the pool board's fuse reads it: a second of upstream clock
		// skew must not turn an untouched week into a running one.
		unstarted := window.ResetAfterSeconds >= window.LimitWindowSeconds
		switch {
		case !binding.found || remaining < binding.remaining:
			binding = weeklyBinding{remaining: remaining, reset: window.ResetAfterSeconds, unstarted: unstarted, found: true}
		case remaining == binding.remaining && betterWeeklyReset(window.ResetAfterSeconds, binding.reset):
			binding.reset, binding.unstarted = window.ResetAfterSeconds, unstarted
		}
	}
	return binding
}

func betterWeeklyReset(candidate, current int64) bool {
	if current <= 0 {
		return candidate > 0
	}
	return candidate > 0 && candidate < current
}

// isWeeklyWindow is the long-window rule (upstream #379): at least
// longWindowMinSeconds, the ">= 6 days" rule internal/accounts and sr status
// use (IsLongQuotaWindow). A mid-length cap between a session window and that
// (OpenRouter "daily", Kimi "1d") still lowers Headroom, so an exhausted daily
// cap blocks routing, but it is not a weekly budget: read as one it would earn
// weekly pace, and, rolling within a day, a permanent seat in the endgame
// drain. Claude's and Codex's long windows are 7 days and unaffected.
func isWeeklyWindow(window LimitWindow) bool {
	return window.LimitWindowSeconds >= longWindowMinSeconds
}

// liveWindow reports whether a window's reading still describes its current
// period: false once the absolute reset it reported (ResetAt) is at or before
// now. Usage snapshots can be served after their provider reset when the
// telemetry endpoint is throttled, and the window has since refilled
// (upstream 4e80ede7). A window with no ResetAt is live.
func liveWindow(window LimitWindow, now time.Time) bool {
	return window.ResetAt.IsZero() || window.ResetAt.After(now)
}

// ModelKey reduces a request model (or a window feature name) to a comparison
// key by lowercasing and dropping non-alphanumeric characters, so a request
// model "gpt-5.3-codex-spark" and the upstream quota name "GPT-5.3-Codex-Spark"
// resolve to the same key. Matching is strict equality on this key: a regular
// model key ("gpt53codex") is a prefix of the Spark key ("gpt53codexspark"), so
// any looser (substring) match would misroute regular traffic into the Spark pool.
func ModelKey(model string) string {
	return normalizeModelKey(model)
}

func limitWindowModelKey(window LimitWindow) string {
	return normalizeModelKey(window.Feature)
}

func normalizeModelKey(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}

func distinctModelKeys(windows []LimitWindow) []string {
	seen := make(map[string]bool)
	var keys []string
	for _, window := range windows {
		key := limitWindowModelKey(window)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	return keys
}

func filterWindowsByModelKey(windows []LimitWindow, key string) []LimitWindow {
	filtered := make([]LimitWindow, 0, len(windows))
	for _, window := range windows {
		if limitWindowModelKey(window) == key {
			filtered = append(filtered, window)
		}
	}
	return filtered
}

func scoreFromLimitWindows(accountID string, sessions int, windows []LimitWindow, modelKey string, claude claudeWeeklyContext, now time.Time) Score {
	headroom := 1.0
	shortHeadroom := 1.0
	shortResetAfterSeconds := int64(0)
	shortWindowSeconds := int64(0)
	hasShortWindow := false
	hasSpendCap := false
	// exhaustedResetAt is when every window at zero headroom has reset: the
	// latest of their resets, since the account stays exhausted until the
	// last one clears (upstream #530).
	var exhaustedResetAt time.Time
	exhaustedResetUnknown := false
	for _, window := range windows {
		// The window set's shape (a short window, a spend cap) is recorded
		// for every window: WeeklyOnly is a fact about what the provider
		// publishes. Only a live window's reading counts (liveWindow).
		if window.SpendCap {
			hasSpendCap = true
		}
		if isShortWindow(window) {
			hasShortWindow = true
		}
		if !liveWindow(window, now) {
			continue
		}
		remaining := 1 - clampPercent(window.UsedPercent)/100
		if remaining <= 0 {
			if window.ResetAt.IsZero() {
				exhaustedResetUnknown = true
			} else if window.ResetAt.After(exhaustedResetAt) {
				exhaustedResetAt = window.ResetAt
			}
		}
		if remaining < headroom {
			headroom = remaining
		}
		if isShortWindow(window) {
			if remaining <= shortHeadroom {
				shortHeadroom = remaining
				shortResetAfterSeconds = window.ResetAfterSeconds
				shortWindowSeconds = window.LimitWindowSeconds
			}
		}
	}
	// With no short window, a non-Claude provider is metered on its week alone
	// (WeeklyOnly below), so the week stands in for the short window and the
	// new-session floor guards it. A Claude account with no 5h reading keeps
	// ShortHeadroom at 1: no reading is no 5h constraint, and the week read as
	// one would refuse the last 5% of a budget that expires at the roll. A
	// spent week still reads exhausted through Headroom, and an upstream 429
	// through its exhaustion mark.
	if !hasShortWindow && !claude.family {
		shortHeadroom = headroom
	}
	score := Score{
		AccountID:              accountID,
		Headroom:               headroom,
		ShortHeadroom:          shortHeadroom,
		ShortResetAfterSeconds: shortResetAfterSeconds,
		ShortWindowSeconds:     shortWindowSeconds,
		ExhaustedResetAt:       exhaustedResetAt,
		ExhaustedResetUnknown:  exhaustedResetUnknown,
		Sessions:               sessions,
	}
	if claude.family {
		// Claude ordering runs on weekly pace in the Pick comparator, so
		// ExpiryPressure (a short-window signal) stays zero: the 5h window
		// gates usability via ShortHeadroom but never drives ranking.
		if binding := bindingWeekly(windows, now); binding.found && binding.reset > 0 {
			spendable := binding.remaining
			// The Fable reservation (PATCHES §57): off by default, so a
			// non-Fable pool spends its measured remainder.
			if modelKey != claudeFablePoolKey && FableReservation() {
				spendable -= claudeFableSliceOfWeekly * claude.fableRemaining * claudeReservationDecay(binding.reset)
				if spendable < 0 {
					spendable = 0
				}
			}
			score.WeeklyHeadroom = spendable
			score.WeeklyRemaining = binding.remaining
			score.WeeklyResetAt = now.Add(time.Duration(binding.reset) * time.Second)
			score.WeeklyUnstarted = binding.unstarted
		}
		return score
	}
	// Non-Claude providers meter on weekly windows too -- a Codex plan's
	// `primary` window is 604800s -- so they get the same weekly pace and the
	// same 24h endgame drain. Only the Fable reservation above (when it is
	// on, PATCHES §57) is Claude-specific; the pace signal is not.
	//
	// Without this the weekly fields stayed zero for Codex purely because
	// bindingWeekly was called inside the Claude branch, and the comparator
	// (which asks weeklyPace of any score) silently demoted every Codex
	// account to the no-pace class. The visible cost: a Codex account coming
	// off its weekly cooldown ranked no higher than one that had just been
	// spent, and use-it-or-lose-it budget expired unspent.
	if binding := bindingWeekly(windows, now); binding.found && binding.reset > 0 {
		score.WeeklyHeadroom = binding.remaining
		score.WeeklyRemaining = binding.remaining
		score.WeeklyResetAt = now.Add(time.Duration(binding.reset) * time.Second)
		score.WeeklyUnstarted = binding.unstarted
		// A weekly meter with no short window beside it is the whole shape of
		// the account. Recorded here, where the window set is in hand, because
		// a Claude account with no 5h reading lacks a short window too and is
		// not weekly-only: it keeps the Claude rules (it returned above).
		score.WeeklyOnly = !hasShortWindow
	} else if hasSpendCap && !hasShortWindow {
		// A usage-based Codex seat: its spend cap is the whole shape of the
		// account, as a weekly meter is a subscription seat's, so it reads
		// WeeklyOnly (gated by exhaustion and the floor alone) -- but it has
		// no weekly fields, hence no pace and no endgame drain. It ranks where
		// it did before its cap was read: below every paced usable seat.
		// Draining a billed cap at the month's end spends money; that is not a
		// default this router makes (PATCHES.md §42).
		score.WeeklyOnly = true
	}
	score.ExpiryPressure = expiryPressure(headroom, shortResetAfterSeconds)
	return score
}

// Window length classes (upstream #379). A window up to shortWindowMaxSeconds
// is a session (5h) limit. A window of at least longWindowMinSeconds is a
// weekly (or longer) limit; this matches the ">= 6 days" long-window rule used
// by internal/accounts and sr status (IsLongQuotaWindow), kept as a local
// constant because selectacct scores its own LimitWindow type. Windows in
// between are mid-length caps (isWeeklyWindow).
const (
	shortWindowMaxSeconds = 6 * 60 * 60
	longWindowMinSeconds  = 6 * 24 * 60 * 60
)

func isShortWindow(window LimitWindow) bool {
	if window.LimitWindowSeconds > 0 {
		return window.LimitWindowSeconds <= shortWindowMaxSeconds
	}
	return false
}

func expiryPressure(headroom float64, resetAfterSeconds int64) float64 {
	if resetAfterSeconds <= 0 {
		return 0
	}
	return headroom / float64(resetAfterSeconds)
}

func clampPercent(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 100:
		return 100
	default:
		return value
	}
}

// VelocityProjectionMinutes is how far ahead a refresh-delta velocity signal
// projects headroom (kept for the experiment's comparison policies), and
// LiveDebitPerRequest is the assumed cost of one routed request as a fraction
// of an account's short window: Pick debits the snapshot headroom by it for
// every request the proxy itself routed to the account since the last usage
// refresh, so concurrent fleets spread across accounts instead of herding
// onto the snapshot's single best account until it cooks. Deliberately an
// over-estimate (real mean is ~0.3%): the experiment shows 2% minimizes
// user-visible failovers (-31% vs no debit), with degradation on both sides
// (0.6%: -23%, 5%: -27%, 10%: -22%). Chosen by TestVelocityPolicyExperiment
// (a deterministic 24h 3-user/19-thread fleet-burst workload over 24 seeds);
// rerun with -v when tuning.
const (
	VelocityProjectionMinutes = 20.0
	LiveDebitPerRequest       = 0.020
)
