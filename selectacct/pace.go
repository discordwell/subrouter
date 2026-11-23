package selectacct

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

const (
	// DefaultPacedSessionCeiling is the most live sessions a paced allowance
	// grants one account, across all its models, unless
	// SetPacedSessionCeiling says otherwise. The owner set it at 20 "to
	// start" (2026-11-20); a non-quota 429 still holds the account at 4.
	DefaultPacedSessionCeiling = 20
	paceBackpressureWindow     = 15 * time.Minute
	paceWindow                 = 2 * time.Hour
	paceMinSpan                = 30 * time.Minute
	paceMinLiveHours           = 0.5
	defaultPaceWasteThreshold  = 0.05
	// defaultBurnPerLive sizes the need of an account with no measured burn:
	// 1.5 points of the week per hour per live session, the middle of the
	// 2026-11-19 box readings (10 to 10.7 points an hour over 6 to 8 live).
	defaultBurnPerLive = 0.015
)

// Pace is one local account's measured weekly burn, as its owning daemon
// measured it.
type Pace struct {
	// BurnPerLive is the weekly fraction spent per hour per live session; 0 is
	// unmeasured.
	BurnPerLive float64
	// BurnNow is the account's weekly fraction spent per hour over the window.
	BurnNow float64
	// Source is "account", "pool" (bootstrapped from this daemon's other
	// accounts), "default" in an Allowance sized on defaultBurnPerLive, or ""
	// when BurnPerLive is unmeasured.
	Source string
	// Backpressure is the last non-quota 429 on the account; zero is none.
	Backpressure time.Time
}

// Allowance is the burndown session allowance with every input that sized it.
type Allowance struct {
	// Sessions is the burndown allowance (endgameAllowedSessions).
	Sessions int
	// Taper is the drain taper alone: ceil(Spendable/step), 1 to 4.
	Taper int
	// Paced is the raise over Taper the time left asks for; 0 is no raise.
	Paced int
	// Ceiling bounds Paced: PacedSessionCeiling(), or claudeEndgameMaxSessions
	// while Backpressured.
	Ceiling       int
	Backpressured bool
	// Needed is the live sessions that would spend Spendable by the roll.
	Needed      float64
	Spendable   float64
	HoursToRoll float64
	// ProjectedWaste is Spendable less what the current burn spends by the
	// roll. Danger is a measured account projected to waste more than
	// PaceWasteThreshold.
	ProjectedWaste float64
	Danger         bool
	Pace           Pace
}

func (s Score) allowance(now time.Time) Allowance {
	s = s.drainInputs()
	spendable := s.spendable()
	taper := int(math.Ceil(spendable / claudeEndgameSessionStep))
	if taper < 1 {
		taper = 1
	}
	if taper > claudeEndgameMaxSessions {
		taper = claudeEndgameMaxSessions
	}
	out := Allowance{Taper: taper, Ceiling: PacedSessionCeiling(), Spendable: spendable, Pace: s.Pace}
	if !s.Pace.Backpressure.IsZero() && now.Sub(s.Pace.Backpressure) < paceBackpressureWindow {
		out.Backpressured, out.Ceiling = true, min(out.Ceiling, claudeEndgameMaxSessions)
	}
	if seconds := s.weeklyResetSecondsLeft(now); seconds > 0 {
		out.HoursToRoll = seconds / 3600
	}
	measured, on := s.Pace.BurnPerLive > 0, PacedAllowance()
	// The tracker measures local Claude accounts only; nobody has measured
	// what a session of any other burns. A reading that is not Fresh (a
	// restored seed, a fetch that keeps failing) may be hours old, so its
	// headroom is no evidence the account is behind pace. With no 5h timing
	// the 5h brake cannot act, so a guessed burn would raise unchecked: the
	// endgame sim's s5 then breaches §62's accepted forced moves.
	hasShortTiming := s.ShortWindowSeconds > 0 && s.ShortResetAfterSeconds > 0
	if on && !measured && s.Fresh && hasShortTiming && s.Provider == account.ProviderClaude && !s.Remote {
		out.Pace.BurnPerLive, out.Pace.Source = defaultBurnPerLive, "default"
	}
	sustainable := s.shortBurnSustainable()
	if out.HoursToRoll > 0 && out.Pace.BurnPerLive > 0 {
		if measured {
			out.ProjectedWaste = spendable - s.Pace.BurnNow*out.HoursToRoll
			out.Danger = out.ProjectedWaste > PaceWasteThreshold()
		}
		if on {
			out.Needed = spendable / out.HoursToRoll / out.Pace.BurnPerLive
			if paced := int(math.Min(roundNeededSessions(out.Needed), float64(out.Ceiling))); spendable > 0 && sustainable &&
				!out.Backpressured && paced > taper {
				out.Paced = paced
			}
		}
	}
	allowed := max(taper, out.Paced)
	if s.loading {
		// A chosen account is run to its limit (PATCHES §95.2): the ceiling,
		// held down by backpressure and the 5h brake like any raise.
		allowed = out.Ceiling
	}
	// Slots beyond the first need a 5h window that survives them. The drain
	// admits an account past usableForNewSession's burn projection (only the
	// short floor applies), which is right for its final task and wrong for a
	// dogpile: when the 5h window is already projected to run dry, the week
	// cannot drain faster than that window lets it, so every extra session
	// only brings the 5h wall sooner and is forced off mid-turn when it lands.
	if allowed > 1 && !sustainable {
		allowed = 1
	}
	out.Sessions = allowed
	return out
}

// roundNeededSessions is a var so the endgame sim's trade table can price
// other roundings against the shipped one.
var roundNeededSessions = math.Ceil

func (s Score) spendable() float64 {
	spendable := s.WeeklyHeadroom
	if s.Reserve > 0 && spendable > 0 {
		spendable = math.Max(0, spendable-s.Reserve)
	}
	return spendable
}

func (s Score) measuredSpendable() float64 {
	s.WeeklyHeadroom += s.weeklyDebit
	return s.spendable()
}

func (s Scheduler) MeasuredSpendable(provider account.Provider, accountID string) float64 {
	return s.score(provider, accountID).measuredSpendable()
}

// UnmeasuredRefugeeCap is the owner's original four refugees an account
// (PATCHES §67, D1), kept where no cap is measured or published.
const UnmeasuredRefugeeCap = 4

func (s Score) refugeeCap(now time.Time) int {
	if s.queueClosed {
		return 0
	}
	if !PacedAllowance() || (s.WeeklyHeadroom <= 0 && s.WeeklyResetAt.IsZero()) {
		return UnmeasuredRefugeeCap
	}
	const epsilon = 1e-9
	points := s.measuredSpendable() * 100
	slots := int(math.Floor(points/10 + epsilon))
	if hours := s.weeklyResetSecondsLeft(now) / 3600; hours > 0 && s.inBurndownWindow(now) {
		slots = max(slots, int(math.Floor(points*24/(5*hours)+epsilon)))
	}
	return slots
}

func (s Scheduler) PaceAllowance(provider account.Provider, accountID string, now time.Time) Allowance {
	return s.score(provider, accountID).allowance(now)
}

func (s Scheduler) withPaces(paces map[string]Pace) Scheduler {
	next := s
	next.paces = paces
	return next
}

var pacedAllowanceOff atomic.Bool

// SetPacedAllowance turns §72's paced allowance on or off for this process.
func SetPacedAllowance(on bool) { pacedAllowanceOff.Store(!on) }

func PacedAllowance() bool { return !pacedAllowanceOff.Load() }

var pacedSessionCeiling atomic.Int64

// SetPacedSessionCeiling sets the most sessions a paced allowance grants for
// this process; 0 restores DefaultPacedSessionCeiling.
func SetPacedSessionCeiling(sessions int) { pacedSessionCeiling.Store(int64(sessions)) }

func PacedSessionCeiling() int {
	if sessions := pacedSessionCeiling.Load(); sessions > 0 {
		return int(sessions)
	}
	return DefaultPacedSessionCeiling
}

var paceWasteThreshold atomic.Pointer[float64]

// SetPaceWasteThreshold sets the projected waste, as a fraction of the week,
// above which a measured account is in danger.
func SetPaceWasteThreshold(fraction float64) { paceWasteThreshold.Store(&fraction) }

// PaceWasteThreshold is the danger threshold, a fraction of the week.
func PaceWasteThreshold() float64 {
	if threshold := paceWasteThreshold.Load(); threshold != nil {
		return *threshold
	}
	return defaultPaceWasteThreshold
}

type paceTracker struct {
	mu           sync.Mutex
	series       map[string]*paceSeries
	backpressure map[string]time.Time
	now          func() time.Time
	snapshot     atomic.Pointer[map[string]Pace]
}

type paceSeries struct {
	epoch     time.Time
	points    []pacePoint
	liveHours float64
	lastAt    time.Time
	lastCount int
}

type pacePoint struct {
	at        time.Time
	used      float64
	liveHours float64
}

func (p *paceTracker) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *paceTracker) seriesLocked(key string) *paceSeries {
	if p.series == nil {
		p.series = make(map[string]*paceSeries)
	}
	series := p.series[key]
	if series == nil {
		series = &paceSeries{}
		p.series[key] = series
	}
	return series
}

func (s *paceSeries) liveHoursAt(at time.Time) float64 {
	if s.lastAt.IsZero() {
		return s.liveHours
	}
	return s.liveHours + float64(s.lastCount)*at.Sub(s.lastAt).Hours()
}

func (p *paceTracker) observeScores(scheduler Scheduler) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, score := range scheduler.scores {
		if score.Remote || score.Provider != account.ProviderClaude || !score.Fresh || score.MeasuredAt.IsZero() ||
			score.WeeklyRemaining <= 0 || score.WeeklyResetAt.IsZero() || !score.WeeklyResetAt.After(score.MeasuredAt) {
			continue
		}
		series := p.seriesLocked(key)
		if delta := score.WeeklyResetAt.Sub(series.epoch); delta > time.Minute || delta < -time.Minute {
			series.epoch, series.points = score.WeeklyResetAt, nil
		}
		used := 1 - score.WeeklyRemaining
		if n := len(series.points); n > 0 {
			last := series.points[n-1]
			if !score.MeasuredAt.After(last.at) {
				continue
			}
			// The header and endpoint readings lag each other.
			used = math.Max(used, last.used)
		}
		series.points = append(series.points, pacePoint{at: score.MeasuredAt, used: used, liveHours: series.liveHoursAt(score.MeasuredAt)})
	}
	p.publishLocked()
}

// ObserveLiveSessions records the live session count of each account, keyed
// by ScoreKey, at at. An account absent from counts has none.
func (r *SchedulerRef) ObserveLiveSessions(counts map[string]int, at time.Time) {
	p := &r.pace
	p.mu.Lock()
	defer p.mu.Unlock()
	for key := range counts {
		p.seriesLocked(key)
	}
	for key, series := range p.series {
		series.liveHours = series.liveHoursAt(at)
		series.lastAt, series.lastCount = at, counts[key]
	}
	p.publishLocked()
}

// NotePaceBackpressure records a non-quota throttle on the account at at.
func (r *SchedulerRef) NotePaceBackpressure(provider account.Provider, accountID string, at time.Time) {
	p := &r.pace
	p.mu.Lock()
	defer p.mu.Unlock()
	key := ScoreKey(provider, accountID)
	if !at.After(p.backpressure[key]) {
		return
	}
	if p.backpressure == nil {
		p.backpressure = make(map[string]time.Time)
	}
	p.backpressure[key] = at
	p.publishLocked()
}

func (s *paceSeries) span() (used, hours, liveHours float64, ok bool) {
	points := s.points
	if len(points) < 2 {
		return 0, 0, 0, false
	}
	first, last := points[0], points[len(points)-1]
	elapsed := last.at.Sub(first.at)
	if elapsed < paceMinSpan {
		return 0, 0, 0, false
	}
	return last.used - first.used, elapsed.Hours(), last.liveHours - first.liveHours, true
}

func (p *paceTracker) publishLocked() {
	cutoff := p.clock().Add(-paceWindow)
	poolUsed, poolLive := 0.0, 0.0
	for _, series := range p.series {
		drop := 0
		for drop < len(series.points) && series.points[drop].at.Before(cutoff) {
			drop++
		}
		series.points = series.points[drop:]
		if used, _, live, ok := series.span(); ok {
			poolUsed += used
			poolLive += live
		}
	}
	paces := make(map[string]Pace, len(p.series)+len(p.backpressure))
	for key, series := range p.series {
		used, hours, live, ok := series.span()
		if !ok {
			continue
		}
		pace := Pace{BurnNow: used / hours}
		switch {
		case live >= paceMinLiveHours:
			pace.BurnPerLive, pace.Source = used/live, "account"
		case poolLive >= paceMinLiveHours:
			pace.BurnPerLive, pace.Source = poolUsed/poolLive, "pool"
		}
		paces[key] = pace
	}
	for key, at := range p.backpressure {
		pace := paces[key]
		pace.Backpressure = at
		paces[key] = pace
	}
	p.snapshot.Store(&paces)
}

// LiveSessionHours is the account's live session-hours the tracker has
// integrated up to at, and whether it has sampled the account at all.
func (r *SchedulerRef) LiveSessionHours(provider account.Provider, accountID string, at time.Time) (float64, bool) {
	p := &r.pace
	p.mu.Lock()
	defer p.mu.Unlock()
	series, ok := p.series[ScoreKey(provider, accountID)]
	if !ok || series.lastAt.IsZero() {
		return 0, false
	}
	return series.liveHoursAt(at), true
}
