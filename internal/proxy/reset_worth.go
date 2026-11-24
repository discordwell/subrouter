package proxy

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// When an automatic claim is worth making (PATCHES §94.1): each meter's pool
// pressure, whether the pool needs its saved resets (held near its bar by
// §96), what a claim now is worth, a reset's last chance, and the window
// before a reset expires. Need is published on burn.json and each window on
// this daemon's own usage-status rows. Nothing ranks, routes or decides on
// any of it here.

// Ruling 2026-11-22: "only reset if we are over burn rate, or the second
// derivative is taking it well over 1.0 to 1.1".
const (
	// resetNeedAt is the pressure at which the pool needs its saved resets:
	// the accounts that can still spend do so as fast as the week lasts.
	resetNeedAt = 1.0
	// resetNeedRisingAt is the bar for the 6-hour pressure plus its trend.
	resetNeedRisingAt = 1.1
)

const (
	// resetNeedHoldAt is where a yes ends (PATCHES §96): once the pool needs
	// its saved resets, it needs them until the pressure that decides falls
	// under this, so a pressure that sits at the bar does not turn the
	// choices on and off with every reading.
	resetNeedHoldAt = 0.9
	// resetNeedUnknownHold is how long a yes holds through readings that have
	// no figure at all.
	resetNeedUnknownHold = 30 * time.Minute
)

const (
	// resetWindowMargin multiplies the time the pool needs to spend what the
	// holders of a reset have left: the window opens that much earlier.
	resetWindowMargin = 1.5
	// resetWindowFloor is the shortest expiry window. Ruling 2026-11-22: a
	// reset burndown starts "about 24 hours before a reset expires".
	resetWindowFloor = 24 * time.Hour
)

// resetNeedWindows are the windows need reads, longest first: the 24 hours
// decide, and while they have too little history the 6 hours, then the hour.
var resetNeedWindows = []string{"24h", "6h", "1h"}

func windowWords(window string) string {
	switch window {
	case "1h":
		return "1-hour"
	case "6h":
		return "6-hour"
	}
	return "24-hour"
}

type pressureState uint8

const (
	// pressureUnknown is a window with too little history.
	pressureUnknown pressureState = iota
	pressureMeasured
	// pressureAllAtLimit is a window every counted account spent at its
	// limit: demand above anything the accounts could serve.
	pressureAllAtLimit
)

// Pressure is the burn among the accounts that could still spend: a window's
// burn divided by the share of account-time not at the limit. An account at
// its limit burns nothing, so burn alone reads low exactly when the pool is
// short. Published as a number, "all_at_limit", or null.
type Pressure struct {
	state pressureState
	value float64
}

const pressureAllAtLimitJSON = "all_at_limit"

// pressureOf reads one window's burn and the share of its covered time at the
// limit.
func pressureOf(burn *float64, atLimit float64) Pressure {
	switch {
	case burn == nil:
		return Pressure{}
	case atLimit >= 1:
		return Pressure{state: pressureAllAtLimit}
	}
	return Pressure{state: pressureMeasured, value: roundBurn(*burn / (1 - atLimit))}
}

// poolWindowPressure is a pool window's pressure.
func poolWindowPressure(w UsagePoolWindow) Pressure {
	share := 0.0
	if w.AtLimit != nil {
		share = *w.AtLimit
	}
	return pressureOf(w.Burn, share)
}

func (p Pressure) known() bool { return p.state != pressureUnknown }

// atLeast reports a pressure at or above bar; all at the limit is above any.
func (p Pressure) atLeast(bar float64) bool {
	return p.state == pressureAllAtLimit || (p.state == pressureMeasured && p.value >= bar)
}

// plus is the pressure with trend added. All at the limit stays so whatever
// the trend; a measured pressure needs a trend.
func (p Pressure) plus(trend *float64) (Pressure, bool) {
	switch {
	case p.state == pressureAllAtLimit:
		return p, true
	case p.state == pressureMeasured && trend != nil:
		return Pressure{state: pressureMeasured, value: roundBurn(p.value + *trend)}, true
	}
	return Pressure{}, false
}

func (p Pressure) String() string {
	switch p.state {
	case pressureMeasured:
		return fmt.Sprintf("%.2f", p.value)
	case pressureAllAtLimit:
		return "all at the limit"
	}
	return "not known"
}

func (p Pressure) MarshalJSON() ([]byte, error) {
	switch p.state {
	case pressureMeasured:
		return json.Marshal(p.value)
	case pressureAllAtLimit:
		return json.Marshal(pressureAllAtLimitJSON)
	}
	return []byte("null"), nil
}

func (p *Pressure) UnmarshalJSON(raw []byte) error {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	switch v := value.(type) {
	case float64:
		*p = Pressure{state: pressureMeasured, value: v}
	case string:
		if v != pressureAllAtLimitJSON {
			return fmt.Errorf("pressure %q is neither a number nor %q", v, pressureAllAtLimitJSON)
		}
		*p = Pressure{state: pressureAllAtLimit}
	default:
		*p = Pressure{}
	}
	return nil
}

// ResetNeed is whether a meter's pool needs its saved resets.
type ResetNeed string

const (
	ResetNeedYes     ResetNeed = "yes"
	ResetNeedNo      ResetNeed = "no"
	ResetNeedUnknown ResetNeed = "unknown"
)

// MeterNeed is one meter's need, as burn.json publishes it.
type MeterNeed struct {
	Answer ResetNeed `json:"answer"`
	// Window is the window that decided: 24h, 6h or 1h; absent when unknown.
	Window string `json:"window,omitempty"`
	// Pressure is the pool's pressure over 1h, 6h and 24h.
	Pressure map[string]Pressure `json:"pressure"`
	// Trend is the change in 6-hour pressure from the 6 hours before, over
	// the accounts with a figure in both; null when it is not known.
	Trend *float64 `json:"trend"`
	Why   string   `json:"why"`
	// Held is a yes this daemon keeps from an earlier reading (PATCHES §96):
	// the figures alone would answer no or unknown.
	Held bool `json:"held,omitempty"`
}

// needOf is whether the pool needs its saved resets on one meter, from its
// pressure per window and its 6-hour trend. Yes when the 24-hour pressure is
// at or above 1.0, or the 6-hour pressure plus its trend is at or above 1.1.
// While the 24 hours have too little history the 6-hour pressure alone
// decides at 1.0, then the 1-hour one. With no pressure at all it is unknown.
func needOf(pressure map[string]Pressure, trend *float64) MeterNeed {
	need := MeterNeed{Answer: ResetNeedUnknown, Pressure: pressure, Trend: trend,
		Why: "no window has enough history for a pressure yet"}
	answer := func(yes bool) ResetNeed {
		if yes {
			return ResetNeedYes
		}
		return ResetNeedNo
	}
	day := pressure["24h"]
	if day.known() {
		need.Window = "24h"
		if day.atLeast(resetNeedAt) {
			need.Answer, need.Why = ResetNeedYes, pressureSentence("24h", day, resetNeedAt)
			return need
		}
		six := pressure["6h"]
		rising, ok := six.plus(trend)
		switch {
		case !six.known():
			need.Answer, need.Why = ResetNeedNo, pressureSentence("24h", day, resetNeedAt)+", and the 6-hour pressure is not known"
		case !ok:
			need.Answer, need.Why = ResetNeedNo, pressureSentence("24h", day, resetNeedAt)+", and the 6-hour trend is not known yet"
		default:
			need.Answer = answer(rising.atLeast(resetNeedRisingAt))
			link := ", and"
			if need.Answer == ResetNeedYes {
				need.Window, link = "6h", ", but"
			}
			need.Why = pressureSentence("24h", day, resetNeedAt) + link + risingSentence(six, trend, rising)
		}
		return need
	}
	for _, window := range resetNeedWindows[1:] {
		if p := pressure[window]; p.known() {
			need.Window = window
			need.Answer = answer(p.atLeast(resetNeedAt))
			need.Why = "the 24-hour window has too little history; " + pressureSentence(window, p, resetNeedAt)
			return need
		}
	}
	return need
}

// pressureSentence says a window's pressure against bar.
func pressureSentence(window string, p Pressure, bar float64) string {
	if p.state == pressureAllAtLimit {
		return fmt.Sprintf("every counted account was at its limit through the %s window", windowWords(window))
	}
	side := "under"
	if p.atLeast(bar) {
		side = "at or above"
	}
	return fmt.Sprintf("the %s pressure is %.2f, %s %.1f", windowWords(window), p.value, side, bar)
}

func risingSentence(six Pressure, trend *float64, rising Pressure) string {
	if six.state == pressureAllAtLimit {
		return " every counted account was at its limit through the 6-hour window, above 1.1 whatever the trend"
	}
	side := "under"
	if rising.atLeast(resetNeedRisingAt) {
		side = "at or above"
	}
	return fmt.Sprintf(" the 6-hour pressure %.2f plus its trend %+.2f is %.2f, %s %.1f", six.value, *trend, rising.value, side, resetNeedRisingAt)
}

// meterNeed is a pool meter's need: its pressure over each window, and its
// 6-hour pressure trend.
func meterNeed(windows map[string]UsagePoolWindow, held []UsageAccountBurn) *MeterNeed {
	pressure := map[string]Pressure{}
	for _, window := range usageBurnPoolWindows {
		pressure[window] = poolWindowPressure(windows[window])
	}
	need := needOf(pressure, pressureTrend(held, "6h", "6h_prev"))
	return &need
}

// holdNeed is a meter's need with what this daemon remembers of it (PATCHES
// §96): yesAt is when need last answered yes on a figure, zero when it has not
// since the daemon started. A yes stays yes until the pressure that decides it
// falls under resetNeedHoldAt, and through readings with no figure for
// resetNeedUnknownHold. It returns the answer, and yesAt for the next reading.
func holdNeed(raw MeterNeed, yesAt, now time.Time) (MeterNeed, time.Time) {
	switch {
	case raw.Answer == ResetNeedYes:
		return raw, now
	case yesAt.IsZero():
		return raw, time.Time{}
	case raw.Answer == ResetNeedNo:
		window, figure, ok := needHeldBy(raw.Pressure, raw.Trend)
		if !ok {
			return raw, time.Time{}
		}
		raw.Answer, raw.Window, raw.Held = ResetNeedYes, window, true
		raw.Why = fmt.Sprintf("still yes: %s, and it turns no under %.1f", figure, resetNeedHoldAt)
		return raw, now
	case now.Sub(yesAt) > resetNeedUnknownHold:
		return raw, time.Time{}
	}
	raw.Answer, raw.Held = ResetNeedYes, true
	raw.Why = fmt.Sprintf("still yes: no window has a pressure now, and the last one said yes %s ago; without a figure a yes holds for %s",
		shortDuration(now.Sub(yesAt)), shortDuration(resetNeedUnknownHold))
	return raw, yesAt
}

// needHeldBy is the figure that keeps a yes at resetNeedHoldAt, with its
// window and in words: the 24-hour pressure, or the 6-hour pressure plus its
// trend; without a 24-hour figure, the window that decides need. False when
// none reaches it.
func needHeldBy(pressure map[string]Pressure, trend *float64) (string, string, bool) {
	if day := pressure["24h"]; day.known() {
		if day.atLeast(resetNeedHoldAt) {
			return "24h", "the 24-hour pressure is " + day.String(), true
		}
		six := pressure["6h"]
		if rising, ok := six.plus(trend); ok && trend != nil && rising.atLeast(resetNeedHoldAt) {
			return "6h", fmt.Sprintf("the 24-hour pressure is %s, and the 6-hour pressure %s plus its trend %+.2f is %s",
				day, six, *trend, rising), true
		}
		return "", "", false
	}
	for _, window := range resetNeedWindows[1:] {
		if p := pressure[window]; p.known() {
			return window, "the 24-hour window has too little history; the " + windowWords(window) + " pressure is " + p.String(),
				p.atLeast(resetNeedHoldAt)
		}
	}
	return "", "", false
}

// resetNeeds is what this daemon remembers of each meter's need (PATCHES §96):
// when it last answered yes on a figure. burn.json, §94's decision and §95's
// choice all read need through it, so the three read one answer. It lives in
// memory alone: a daemon that has just started answers from the figures.
type resetNeeds struct {
	mu    sync.Mutex
	yesAt map[string]time.Time
}

func (r *AccountRef) sharedResetNeeds() *resetNeeds {
	if r == nil {
		return &resetNeeds{}
	}
	if needs := r.resetNeedMemory.Load(); needs != nil {
		return needs
	}
	r.resetNeedMemory.CompareAndSwap(nil, &resetNeeds{})
	return r.resetNeedMemory.Load()
}

// hold puts each meter's held need in place of its need from the figures, at
// now, and remembers it.
func (n *resetNeeds) hold(meters []UsagePoolMeter, now time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.yesAt == nil {
		n.yesAt = map[string]time.Time{}
	}
	for i := range meters {
		if meters[i].Need == nil {
			continue
		}
		key := meters[i].Provider + "\x00" + meters[i].Meter
		need, yesAt := holdNeed(*meters[i].Need, n.yesAt[key], now)
		if yesAt.IsZero() {
			delete(n.yesAt, key)
		} else {
			n.yesAt[key] = yesAt
		}
		meters[i].Need = &need
	}
}

// pressureTrend is the change in pressure from the window before, over the
// accounts with a figure in both, as §92.2 takes the burn trend. Burn's own
// trend reads low when accounts reach their limit, the case pressure is for.
func pressureTrend(held []UsageAccountBurn, last, before string) *float64 {
	var both []UsageAccountBurn
	for _, account := range held {
		if account.Windows[last].Burn != nil && account.Windows[before].Burn != nil {
			both = append(both, account)
		}
	}
	now, then := poolWindowPressure(poolWindowOf(both, last)), poolWindowPressure(poolWindowOf(both, before))
	if now.state != pressureMeasured || then.state != pressureMeasured {
		return nil
	}
	change := roundBurn(now.value - then.value)
	return &change
}

// longestPressure is the pressure over the longest window that has one.
func longestPressure(pressure map[string]Pressure) (Pressure, string) {
	for _, window := range resetNeedWindows {
		if p := pressure[window]; p.known() {
			return p, window
		}
	}
	return Pressure{}, ""
}

// claimRate is the rate a claim's value is read at, in weeks of budget a
// week.
type claimRate struct {
	value float64
	// unbounded is a pool all at its limit: whatever a claim gives back is
	// spent.
	unbounded bool
	from      string
}

// claimRateOf is an account's rate on one meter: its own pressure over the
// longest window that has one, never under the pool's pressure when that is
// known; 1.0 when neither is. The account's own pressure, not its burn: an
// account deciding a claim is at its limit, and its time there burned
// nothing.
func claimRateOf(own UsageMeterBurn, pool Pressure) claimRate {
	var rate claimRate
	found := false
	for _, window := range resetNeedWindows {
		figure, ok := own[window]
		if !ok {
			continue
		}
		if p := pressureOf(figure.Burn, figure.AtLimit); p.state == pressureMeasured {
			rate, found = claimRate{value: p.value, from: "its own " + windowWords(window) + " rate"}, true
			break
		}
	}
	switch {
	case pool.state == pressureAllAtLimit:
		return claimRate{unbounded: true, from: "the pool is all at its limit"}
	case pool.state == pressureMeasured && (!found || pool.value > rate.value):
		return claimRate{value: pool.value, from: "the pool's pressure"}
	case found:
		return rate
	}
	return claimRate{value: 1, from: "no figure, so 1.0"}
}

func (r claimRate) String() string {
	if r.unbounded {
		return r.from
	}
	return fmt.Sprintf("%.2f (%s)", r.value, r.from)
}

// claimValue is what a claim now is worth, as a share of one week's budget.
type claimValue struct {
	known bool
	value float64
	why   string
}

// claimValueOf is what claiming a reset now is worth (§94.1). A reset that
// keeps the roll gives back what the account can spend again before its roll:
// the rate times the time to the roll, at most a week. One that restarts the
// seven days skips the wait to the roll. A kind whose clock nobody knows has
// no value, and is never claimed automatically.
func claimValueOf(restarts accounts.ClockRestart, toRoll time.Duration, rate claimRate) claimValue {
	toRoll = max(toRoll, 0)
	share := toRoll.Hours() / usageMeterLength.Hours()
	restart, known := restarts.Restarts()
	switch {
	case !known:
		return claimValue{why: "whether a claim of this kind restarts the seven days is not known, so a claim has no value it can be judged by"}
	case restart:
		value := roundBurn(min(1, share))
		return claimValue{known: true, value: value,
			why: fmt.Sprintf("a claim restarts the seven days and skips the %s to the roll: %.2f of a week", shortDuration(toRoll), value)}
	}
	value := 0.0
	switch {
	case rate.unbounded && toRoll > 0:
		value = 1
	case !rate.unbounded:
		value = roundBurn(min(1, rate.value*share))
	}
	return claimValue{known: true, value: value,
		why: fmt.Sprintf("the roll is %s away and the rate is %s: a claim gives back %.2f of a week that can be spent before it",
			shortDuration(toRoll), rate, value)}
}

// lastChance reports a reset that expires before the account could meet its
// next wall: its roll plus a week at the rate, the rate floored at 1, so at
// most a week after the roll. A reset with no date has none.
func lastChance(expires, roll time.Time, rate claimRate) bool {
	if expires.IsZero() {
		return false
	}
	next := roll
	if !rate.unbounded {
		next = roll.Add(time.Duration(float64(usageMeterLength) / max(1, rate.value)))
	}
	return expires.Before(next)
}

// laterWall is the same reset claimed at the account's next wall after its
// roll (PATCHES §103).
type laterWall struct {
	// at is when the account, starting a fresh week at its roll, is at its
	// limit again: the roll plus a week over the rate.
	at time.Time
	// value is what a claim there adds, as a share of a week.
	value float64
	// walls is how many walls the account can expect, one a week from at,
	// before the reset expires or its expiry window opens; -1 when neither
	// has a date.
	walls int
}

// laterWallOf is the account's next wall after roll, at the rate a claim now
// is valued at, and what the same reset adds there. A reset that keeps the
// roll gives back the rate times the share of the week still ahead, min(1,
// rate-1); one that restarts the seven days skips the rest of the week,
// 1-1/rate. A pool all at its limit is at the wall at the roll, where either
// gives back a week. False at a rate of 1 or under: no later wall is
// expected.
func laterWallOf(restarts accounts.ClockRestart, roll time.Time, rate claimRate) (laterWall, bool) {
	restart, known := restarts.Restarts()
	switch {
	case !known || roll.IsZero():
		return laterWall{}, false
	case rate.unbounded:
		return laterWall{at: roll, value: 1}, true
	case rate.value <= 1:
		return laterWall{}, false
	}
	later := laterWall{at: roll.Add(time.Duration(float64(usageMeterLength) / rate.value)).Round(time.Second)}
	if restart {
		later.value = roundBurn(1 - 1/rate.value)
	} else {
		later.value = roundBurn(min(1, rate.value-1))
	}
	return later, true
}

// resetWait is what §103's rule reads about a claim of one reset besides
// what the claim adds now.
type resetWait struct {
	restarts accounts.ClockRestart
	roll     time.Time
	rate     claimRate
	// expires is when the reset expires, zero for never; window is its expiry
	// window, nil for none.
	expires time.Time
	window  *accounts.ResetExpiryWindow
	// held is how many resets of the kind the account holds that expire no
	// later than this one (heldThrough).
	held int
	// stood is a claim on this reset that passed the comparison before.
	stood bool
}

// waitForBetter is the rule of PATCHES §103, once every check of §94 has
// said claim: a claim that adds now under resetWaitShare of what the same
// reset is expected to add at the account's next wall waits for that wall.
// The later wall counts only while the reset is still there: before it
// expires, and before its expiry window opens, where any value is claimed.
// And only while the account can expect at least as many walls before then
// as it holds resets of the kind: with more resets than walls, waiting would
// let one expire. A claim that passed the comparison before keeps passing
// down to resetStayShare of the bar, so a value near it does not turn the
// decision back and forth. It returns the later wall, whether it counted, and
// whether the claim waits for it.
func waitForBetter(w resetWait, now float64) (later laterWall, counts, waits bool) {
	later, ok := laterWallOf(w.restarts, w.roll, w.rate)
	if !ok {
		return later, false, false
	}
	limit := w.expires
	if w.window != nil && (limit.IsZero() || w.window.OpensAt.Before(limit)) {
		limit = w.window.OpensAt
	}
	later.walls = -1
	if !limit.IsZero() {
		// A later wall at or after the limit counts no wall at all.
		later.walls = int(math.Ceil(float64(limit.Sub(later.at)) / float64(usageMeterLength)))
		if max(w.held, 1) > later.walls {
			return later, false, false
		}
	}
	return later, true, now < roundBurn(waitBar(w.stood)*later.value)
}

// waitBar is the share of the later wall's value a claim must add now not to
// wait: resetWaitShare, or resetStayShare of it for a claim that passed
// before.
func waitBar(stood bool) float64 {
	if stood {
		return roundBurn(resetWaitShare * resetStayShare)
	}
	return resetWaitShare
}

// heldThrough is how many resets of kind the list holds that expire no later
// than expires, the reset itself among them: the ones that need a wall before
// that date. One whose date is not read (a Codex count before its credit
// list) may expire as soon, so it counts. At least 1.
func heldThrough(list []accounts.HeldReset, kind accounts.ResetKind, expires time.Time) int {
	held := 0
	for _, entry := range list {
		if at, dated := entry.Expires(); entry.Kind == kind && (expires.IsZero() || !dated || !at.After(expires)) {
			held += entry.Count
		}
	}
	return max(held, 1)
}

// laterWallWords says what a claim adds where it is weighed (when: "now", or
// "there" at a projected wall) against the same reset at the account's next
// wall, and how long the reset lasts.
func laterWallWords(when string, now float64, later laterWall, w resetWait) string {
	until := "the reset does not expire"
	if !w.expires.IsZero() {
		until = "the reset lasts until " + rfc3339(w.expires)
	}
	if later.walls > 0 {
		before := "before then"
		if w.window != nil && (w.expires.IsZero() || w.window.OpensAt.Before(w.expires)) {
			before = "before its expiry window opens at " + rfc3339(w.window.OpensAt)
		}
		until += fmt.Sprintf(", and the account can expect %d %s %s", later.walls, plural(later.walls, "such wall", "such walls"), before)
	}
	return fmt.Sprintf("a claim adds %.2f of a week %s, and the same reset is expected to add %.2f at the account's next wall at %s, %s after its roll at %s (the rate is %s); %s",
		now, when, later.value, rfc3339(later.at), shortDuration(later.at.Sub(w.roll)), rfc3339(w.roll), w.rate, until)
}

// resetDemand is the pool's present demand on one meter: its pressure over
// the longest window that has one, and how many accounts that window counts.
type resetDemand struct {
	label    string
	pressure Pressure
	window   string
	accounts int
}

func meterDemand(m UsagePoolMeter) resetDemand {
	demand := resetDemand{label: m.Label}
	if m.Need == nil {
		return demand
	}
	demand.pressure, demand.window = longestPressure(m.Need.Pressure)
	demand.accounts = m.Windows[demand.window].Accounts
	return demand
}

// expiryWindow is the stretch before a reset's expiry in which the reset is
// spent on any demand at the account's limit (§94.1). It opens lead before
// the expiry: the time the pool needs, at its present demand on the meter,
// to spend what the holders of those resets have left (headroom, in weeks of
// one account), times 1.5, never under 24 hours and never more than the time
// left. When the demand is not known the lead is 24 hours.
func expiryWindow(expires, now time.Time, holders int, headroom float64, demand resetDemand) accounts.ResetExpiryWindow {
	left := max(expires.Sub(now), 0)
	lead := resetWindowFloor
	var why string
	switch {
	case demand.pressure.state == pressureUnknown || demand.accounts == 0:
		why = fmt.Sprintf("the pool's demand on the %s is not known yet, so the lead is the 24-hour floor", demand.label)
	case demand.pressure.state == pressureAllAtLimit:
		why = fmt.Sprintf("every counted account was at its limit on the %s through the %s window, so the lead is the 24-hour floor",
			demand.label, windowWords(demand.window))
	default:
		holding := fmt.Sprintf("%d %s %.1f weeks of the %s left between them", holders, plural(holders, "holder has", "holders have"),
			headroom, demand.label)
		perWeek := demand.pressure.value * float64(demand.accounts)
		if perWeek <= 0 {
			lead = left
			why = fmt.Sprintf("%s, and the pool spends nothing on it (pressure 0.00 over %d accounts), so the window is open", holding, demand.accounts)
			break
		}
		needs := headroom / perWeek * usageMeterLength.Hours()
		margin := resetWindowMargin * needs
		why = fmt.Sprintf("%s; at the pool's %s pressure %.2f over %d %s they need %s to spend it, and 1.5 times that is %s",
			holding, windowWords(demand.window), demand.pressure.value, demand.accounts, plural(demand.accounts, "account", "accounts"),
			hoursWords(needs), hoursWords(margin))
		switch {
		case margin >= left.Hours():
			lead = left
			why += fmt.Sprintf(", more than the %s left, so the window is open", shortDuration(left))
		case margin < resetWindowFloor.Hours():
			why += ", so the 24-hour floor"
		default:
			lead = time.Duration(margin * float64(time.Hour))
		}
	}
	lead = min(lead, left)
	opens := expires.Add(-lead.Round(time.Second))
	if lead == left {
		// Capped at the time left: the window is open now. A rounded lead
		// could put it a fraction of a second ahead of the decision that
		// reads it; the second now falls in keeps it steady between reads.
		opens = now.Truncate(time.Second)
	}
	return accounts.ResetExpiryWindow{OpensAt: opens.UTC(), LeadSeconds: int64(lead.Round(time.Second) / time.Second), Why: why}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// hoursWords is a duration given in hours, as shortDuration says it; a
// duration past what time.Duration holds reads in weeks.
func hoursWords(hours float64) string {
	if hours >= float64(math.MaxInt64/int64(time.Hour)) {
		return fmt.Sprintf("%.0f weeks", hours/usageMeterLength.Hours())
	}
	return shortDuration(time.Duration(hours * float64(time.Hour)))
}

// shortDuration is d to the minute: 60h29m, 24h, 45m.
func shortDuration(d time.Duration) string {
	text := d.Round(time.Minute).String()
	text = strings.TrimSuffix(text, "0s")
	if strings.HasSuffix(text, "h0m") {
		text = strings.TrimSuffix(text, "0m")
	}
	if text == "" {
		return "0m"
	}
	return text
}

// resetHolder is one account that holds saved resets, as its row lists them,
// with what each meter has left.
type resetHolder struct {
	provider, id string
	resets       []accounts.HeldReset
	left         map[accounts.ResetMeter]float64
}

// resetHolderOf reads an account's resets and meters from its row; false for
// a row that lists none.
func resetHolderOf(provider, id string, resets []accounts.HeldReset, windows []accounts.UsageWindow) (resetHolder, bool) {
	if len(resets) == 0 {
		return resetHolder{}, false
	}
	holder := resetHolder{provider: provider, id: id, resets: resets, left: map[accounts.ResetMeter]float64{}}
	for meter, reading := range autoResetReadingsIn(accounts.Provider(provider), windows, time.Time{}) {
		holder.left[accounts.ResetMeter(meter)] = reading.left
	}
	return holder, true
}

// resetHoldingKey is one kind of reset ending at one time.
type resetHoldingKey struct {
	provider string
	kind     accounts.ResetKind
	expires  int64
}

// resetHolding is what the holders of one kind of reset ending at one time
// have left between them, in weeks of one account.
type resetHolding struct {
	holders  int
	headroom float64
}

// resetHoldings is each kind and expiry's holders across the pool, an
// account two pools hold once, from the first. A holder spends what is left
// on the kind's meter (a whole week when that is unread), and a week more for
// each reset of the kind and date beyond the first.
func resetHoldings(holders []resetHolder, now time.Time) map[resetHoldingKey]resetHolding {
	out := map[resetHoldingKey]resetHolding{}
	seen := map[string]bool{}
	for _, holder := range holders {
		if seen[holder.provider+"\x00"+holder.id] {
			continue
		}
		seen[holder.provider+"\x00"+holder.id] = true
		for _, entry := range holder.resets {
			key, spec, ok := holdingKeyOf(holder.provider, entry, now)
			if !ok {
				continue
			}
			left, read := holder.left[spec.Meter]
			if !read {
				left = 1
			}
			holding := out[key]
			holding.holders++
			holding.headroom += left + float64(max(entry.Count-1, 0))
			out[key] = holding
		}
	}
	return out
}

// holdingKeyOf is a list entry's kind and expiry, for a known kind with a
// date still ahead.
func holdingKeyOf(provider string, entry accounts.HeldReset, now time.Time) (resetHoldingKey, accounts.ResetKindSpec, bool) {
	spec, ok := accounts.ResetKindNamed(accounts.Provider(provider), entry.Kind)
	if !ok || entry.Count <= 0 {
		return resetHoldingKey{}, spec, false
	}
	expires, ok := entry.Expires()
	if !ok || !expires.After(now) {
		return resetHoldingKey{}, spec, false
	}
	return resetHoldingKey{provider: provider, kind: entry.Kind, expires: expires.UnixMicro()}, spec, true
}

// resetPool is the pool as the expiry windows and the decision read it: each
// meter's figures and need, and what the holders of each kind and date have
// left.
type resetPool struct {
	now      time.Time
	meters   map[string]UsagePoolMeter
	holdings map[resetHoldingKey]resetHolding
	// waitForBetter is SUBROUTER_AUTO_RESET_WAIT_FOR_BETTER as this pool was
	// read: the decision and the choice that read the pool both follow §103's
	// rule, or neither does.
	waitForBetter bool
}

// resetPoolOf reads the pool at now from statuses, this daemon's own rows
// carrying their burn and resets, and every peer counted on burn.json, each
// from its last usage body. No peer is read here: a peer that reads this
// daemon's usage-status would read it back, and a decision does not wait on
// a peer. The background pass reads a peer again when that body is more than
// resetPeerRefreshAge old (§95.1).
func (s Server) resetPoolOf(statuses []AccountUsageStatus, now time.Time) resetPool {
	pool := resetPool{now: now, meters: map[string]UsagePoolMeter{}, waitForBetter: AutoResetWaitForBetter()}
	var rows []usageBurnAccount
	var holders []resetHolder
	for _, status := range statuses {
		provider := usageHistoryProvider(accounts.Account{ID: status.ID, Provider: status.Provider})
		// A pin-only account is no part of the pool (§98): its burn and
		// headroom would count as another account's.
		if provider == "" || s.pinOnlyAccount(status.ID) {
			continue
		}
		if len(status.Burn) > 0 {
			rows = append(rows, usageBurnAccount{poolNameLocal, provider, status.ID, status.Label, status.Burn})
		}
		if holder, ok := resetHolderOf(provider, status.ID, status.Resets, status.Windows); ok {
			holders = append(holders, holder)
		}
	}
	for _, peer := range s.peers() {
		read := readPeerBurn(peer.name(), peer.cachedUsage())
		rows, holders = append(rows, read.rows...), append(holders, read.holders...)
	}
	meters := usagePoolMeters(rows)
	s.AccountRef.sharedResetNeeds().hold(meters, now)
	for _, meter := range meters {
		pool.meters[meter.Provider+"\x00"+meter.Meter] = meter
	}
	pool.holdings = resetHoldings(holders, pool.now)
	return pool
}

// meter is the pool's figures on provider's meter.
func (p resetPool) meter(provider string, meter accounts.ResetMeter) UsagePoolMeter {
	m, ok := p.meters[provider+"\x00"+string(meter)]
	if !ok {
		m.Label = usageMeterLabel(provider, string(meter))
	}
	return m
}

// window is entry's expiry window, for a known kind with a date still ahead.
func (p resetPool) window(provider string, entry accounts.HeldReset) (accounts.ResetExpiryWindow, bool) {
	key, spec, ok := holdingKeyOf(provider, entry, p.now)
	if !ok {
		return accounts.ResetExpiryWindow{}, false
	}
	holding := p.holdings[key]
	expires, _ := entry.Expires()
	return expiryWindow(expires, p.now, holding.holders, holding.headroom, meterDemand(p.meter(provider, spec.Meter))), true
}

// withResetWindows puts each reset's expiry window on this daemon's own rows.
func (s Server) withResetWindows(statuses []AccountUsageStatus) []AccountUsageStatus {
	pool := s.resetPoolOf(statuses, s.AccountRef.sharedHeldResets().clock())
	out := append([]AccountUsageStatus(nil), statuses...)
	for i := range out {
		provider := usageHistoryProvider(accounts.Account{ID: out[i].ID, Provider: out[i].Provider})
		if provider == "" || len(out[i].Resets) == 0 {
			continue
		}
		// The list may be the block's own, shared with the sweep's snapshot.
		out[i].Resets = slices.Clone(out[i].Resets)
		for j, entry := range out[i].Resets {
			if window, ok := pool.window(provider, entry); ok {
				out[i].Resets[j].ExpiryWindow = &window
			}
		}
	}
	return out
}
