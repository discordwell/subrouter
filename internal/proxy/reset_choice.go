package proxy

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// Loading work onto the accounts chosen for a reset (PATCHES §95.1). A saved
// reset is worth something only on an account that reaches its limit, so the
// daemon that holds an account chooses the ones this pool should run to their
// limit, so that their next saved reset is claimed there (§93, §94). This file
// decides which accounts are chosen and in what order, logs each change and
// publishes the choice; ranking honours a choice published in real mode
// (§95.2, selectacct/reset_choice.go). Ruling 2026-11-22: "with resets in hand
// the aim is to load, not to balance", and for Codex "pick one account, burn
// it to the ground, use its reset, then pick a new account".

// resetLoadingOn is SUBROUTER_RESET_LOADING: on, the daemon chooses accounts
// (in shadow or real mode) and ranks on the choices published in real mode,
// its own and its peers'; off, it chooses none and ranks on none. Never set,
// it is off, so a Server that serve did not set up chooses nothing; serve
// sets it on unless the switch says 0.
var resetLoadingOn atomic.Bool

// SetResetLoading turns the choosing of accounts on or off for the process.
func SetResetLoading(on bool) { resetLoadingOn.Store(on) }

// ResetLoading reports whether this daemon chooses accounts to load and
// honours a published choice.
func ResetLoading() bool { return resetLoadingOn.Load() }

// resetPeerRefreshAge is how old a peer's last usage body may be before the
// background pass reads it again (part I): the pool the choice and §94's
// decision read must not be one daemon's accounts alone.
const resetPeerRefreshAge = 5 * time.Minute

// resetChoiceMeterOf names the choice for provider's meter.
func resetChoiceMeterOf(provider string, meter accounts.ResetMeter) selectacct.ResetChoiceMeter {
	switch {
	case provider == string(accounts.ProviderCodex):
		return selectacct.ResetChoiceCodexWeek
	case meter == accounts.ResetMeterFable:
		return selectacct.ResetChoiceFable
	}
	return selectacct.ResetChoiceWeek
}

// resetKindWords names a known kind for a person.
func resetKindWords(kind accounts.ResetKind) string {
	switch kind {
	case accounts.ResetKindClaudeWeek:
		return "week reset"
	case accounts.ResetKindClaudeFable:
		return "Fable reset"
	case accounts.ResetKindCodexRateLimits:
		return "Codex reset credit"
	}
	return string(kind) + " reset"
}

// resetCandidate is one of this daemon's own accounts as the choice reads it.
type resetCandidate struct {
	// key is the account's selectacct.ScoreKey; provider is the history's
	// name for its provider: claude or codex.
	key, provider, id, label string
	// out says why the account cannot be chosen whatever the pool says: a
	// dead credential, an owner's reserve, an automatic claim not settled,
	// no saved reset. "" when it is weighed.
	out string
	// next is the provider's next reset on the account: the one a claim at
	// its limit spends.
	next accounts.HeldReset
	// readings are its meters as its last reading gave them.
	readings map[autoResetMeter]autoResetReading
	// burn is its own figures, by meter.
	burn UsageBurn
}

// resetWeighing is what the pool says of one candidate.
type resetWeighing struct {
	candidate resetCandidate
	meter     selectacct.ResetChoiceMeter
	// qualifies is a candidate the pool should load; why says why, or why
	// not.
	qualifies bool
	why       string
	// window is a reset inside its expiry window (§94.1).
	window  bool
	expires time.Time
	// value is the share of a week a claim adds at the wall the account is
	// heading for, -1 when not known; toRoll is the time from that wall to
	// the roll, unknown at resetUnknownToRoll.
	value  float64
	toRoll time.Duration
	// unused is a reset that expires before the account could reach its
	// limit, even with the pool's whole demand on it.
	unused bool
	// onPool is a candidate that fails on the pool alone: its need on the
	// meter is no or unknown, or its demand is not known (PATCHES §96).
	onPool bool
	// waiting is a choice that keeps its place through such a gap, and place
	// the order it keeps when it qualifies again, 0 for none
	// (chooseAcrossGaps).
	waiting bool
	place   int
}

const resetUnknownToRoll = time.Duration(-1 << 62)

// projectedWall is when an account with left of a meter reaches its limit if
// the pool's whole present demand on the meter went to it: left over the
// pool's pressure times the accounts counted, per week. False when the demand
// is not known or is nothing.
func projectedWall(left float64, demand resetDemand, now time.Time) (time.Time, bool) {
	switch {
	case demand.pressure.state == pressureAllAtLimit:
		return now, true
	case demand.pressure.state != pressureMeasured || demand.accounts == 0 || demand.pressure.value <= 0:
		return time.Time{}, false
	}
	weeks := max(left, 0) / (demand.pressure.value * float64(demand.accounts))
	return now.Add(time.Duration(weeks * float64(usageMeterLength))), true
}

// weigh is whether the pool should load c (PATCHES §95.1). A candidate
// qualifies when its next reset is of a known kind whose clock is known,
// §103's gate would let a claim at the wall it is heading for through (its
// roll read and at least 24 hours past that wall, or the reset's last chance;
// for a Fable reset, at least 25% of its week left now), and either the reset
// is inside its expiry window, or the pool needs its saved resets on the
// reset's meter and §94's rule would claim at that wall: the reset is in its
// last chance, or a claim at the projected wall adds at least resetValueFloor
// of a week. With the wall not known, the gate reads the roll from now.
// Outside the window the pool's need is read before the gate.
func (p resetPool) weigh(c resetCandidate) resetWeighing {
	w := resetWeighing{candidate: c, value: -1, toRoll: resetUnknownToRoll}
	if c.out != "" {
		w.why = c.out
		return w
	}
	spec, known := accounts.ResetKindNamed(accounts.Provider(c.provider), c.next.Kind)
	if !known {
		w.why = "its next reset is of a kind this daemon does not know (" + cmp.Or(c.next.RawType, string(c.next.Kind)) +
			"); such a reset is never claimed automatically"
		return w
	}
	w.meter = resetChoiceMeterOf(c.provider, spec.Meter)
	if _, clock := spec.RestartsClock.Restarts(); !clock {
		w.why = "whether a claim of its next reset restarts the seven days is not known, so a claim has no value it can be judged by"
		return w
	}
	now := p.now
	pool := p.meter(c.provider, spec.Meter)
	need := needOf(nil, nil)
	if pool.Need != nil {
		need = *pool.Need
	}
	w.expires, _ = c.next.Expires()
	kind := resetKindWords(c.next.Kind)
	if window, ok := p.window(c.provider, c.next); ok && !now.Before(window.OpensAt) {
		w.window = true
		w.why = fmt.Sprintf("its %s ending %s is inside its expiry window, open since %s: every holder of it is loaded until it is used",
			kind, w.expires.UTC().Format(time.RFC3339), window.OpensAt.UTC().Format(time.RFC3339))
	}
	poolPressure, _ := longestPressure(need.Pressure)
	rate := claimRateOf(c.burn[string(spec.Meter)], poolPressure)
	meter := autoResetMeter(spec.Meter)
	reading, read := c.readings[meter]
	rollRead := read && !reading.roll.IsZero()
	wall, wallKnown := time.Time{}, false
	if read {
		wall, wallKnown = projectedWall(reading.left, meterDemand(pool), now)
	}
	last := rollRead && lastChance(w.expires, reading.roll, rate)
	var value claimValue
	if wallKnown && rollRead {
		w.toRoll = reading.roll.Sub(wall)
		value = claimValueOf(spec.RestartsClock, w.toRoll, rate)
		w.value = value.value
	}
	needs := fmt.Sprintf("the pool needs its saved resets on the %s (%s)", pool.Label, need.Why)
	heading, limit := "", now
	if wallKnown {
		heading = fmt.Sprintf("at the pool's whole demand it reaches its limit in %s", shortDuration(wall.Sub(now)))
		limit = wall
	}
	// §103's gate, as a claim at the limit the account is heading for would
	// meet it: nothing is loaded toward a claim the gate would hold, inside
	// the expiry window too. Outside the window the pool's need is read first,
	// so a choice that stops on a pause in need alone keeps its place (§96).
	week, weekRead := c.readings[autoResetWeek]
	hold := resetHoldOf(meter, reading.roll.Sub(limit), rollRead, last,
		resetWeek{left: week.left, known: weekRead, why: "its week is unread"})
	held := func(lead string) string {
		why := "a claim of its " + kind + " at its limit would be held: " + hold.why
		if heading != "" {
			why = heading + "; " + why
		}
		return lead + ", but " + why
	}
	switch {
	case w.window && hold.by != "":
		w.why = held(w.why)
	case w.window:
		w.qualifies = true
	case need.Answer == ResetNeedUnknown:
		w.why, w.onPool = fmt.Sprintf("the pool's burn on the %s is not known yet (%s)", pool.Label, need.Why), true
	case need.Answer == ResetNeedNo:
		w.why, w.onPool = fmt.Sprintf("the pool does not need its saved resets on the %s (%s)", pool.Label, need.Why), true
	case hold.by != "":
		w.why = held(needs)
	case last:
		w.qualifies = true
		w.why = fmt.Sprintf("%s, and it is its %s's last chance: it expires at %s, before the account could meet its next wall after the roll at %s",
			needs, kind, w.expires.UTC().Format(time.RFC3339), reading.roll.UTC().Format(time.RFC3339))
	case !wallKnown:
		w.why = fmt.Sprintf("%s, but the pool's demand on it is not known, so where the account reaches its limit is not known", needs)
		w.onPool = true
	case value.value < resetValueFloor:
		w.why = fmt.Sprintf("%s, but %s, and a claim there adds %.2f of a week, under the floor of %.2f (%s)",
			needs, heading, value.value, resetValueFloor, value.why)
	default:
		w.qualifies = true
		w.why = fmt.Sprintf("%s; %s, and a claim there adds %.2f of a week (%s)", needs, heading, value.value, value.why)
	}
	if w.qualifies && wallKnown && !w.expires.IsZero() && wall.After(w.expires) {
		w.unused = true
		w.why += fmt.Sprintf("; its %s is likely to expire unused: even with the pool's whole demand it reaches its limit at %s, after the reset expires at %s",
			kind, wall.UTC().Format(time.RFC3339), w.expires.UTC().Format(time.RFC3339))
	}
	return w
}

// resetCodexHold is the Codex account this daemon loads now, one at a time,
// and since when.
type resetCodexHold struct {
	key   string
	since time.Time
}

// resetChoosing is one pass's choice: each account chosen, by key, and why
// each weighed account that is not chosen is not.
type resetChoosing struct {
	choices   map[string]selectacct.ResetChoice
	notChosen map[string]string
	codex     resetCodexHold
}

// chooseResets is which of the weighed accounts this daemon chooses, and in
// what order (PATCHES §95.1). Every Claude account that qualifies is chosen,
// per meter. Of the Codex accounts that qualify one is chosen at a time, the
// one whose claim at its wall is worth the most, and it stands for as long as
// it qualifies, whatever another is worth: "pick one account, burn it to the
// ground, use its reset, then pick a new account". Every Codex holder inside
// its expiry window is chosen too.
// Within a meter, the order is the holders inside their expiry window,
// soonest expiry first, then the rest by the value of a claim at their wall,
// highest first, and last the holders whose reset is likely to expire unused;
// within each, an account back from a gap in the pool's need takes its old
// place first (§96). A hold on an account waiting out such a gap stands.
func chooseResets(weighed []resetWeighing, hold resetCodexHold, now time.Time) resetChoosing {
	out := resetChoosing{choices: map[string]selectacct.ResetChoice{}, notChosen: map[string]string{}}
	byMeter := map[selectacct.ResetChoiceMeter][]resetWeighing{}
	var codex []resetWeighing
	holdWaits := false
	for _, w := range weighed {
		switch {
		case !w.qualifies:
			out.notChosen[w.candidate.key] = w.why
			holdWaits = holdWaits || (w.waiting && w.candidate.key == hold.key)
		case w.meter == selectacct.ResetChoiceCodexWeek:
			codex = append(codex, w)
		default:
			byMeter[w.meter] = append(byMeter[w.meter], w)
		}
	}
	single, since := codexSingle(codex, hold, holdWaits, now)
	if single != "" {
		out.codex = resetCodexHold{key: single, since: since}
	}
	for _, w := range codex {
		switch {
		case w.candidate.key == single && !w.window:
			w.why += fmt.Sprintf("; Codex accounts are loaded one at a time, and this one is loaded since %s", since.UTC().Format(time.RFC3339))
			fallthrough
		case w.window:
			byMeter[selectacct.ResetChoiceCodexWeek] = append(byMeter[selectacct.ResetChoiceCodexWeek], w)
		default:
			out.notChosen[w.candidate.key] = "Codex accounts are loaded one at a time, and another one is loaded now"
		}
	}
	for meter, list := range byMeter {
		slices.SortFunc(list, compareChosen)
		for i, w := range list {
			out.choices[w.candidate.key] = selectacct.ResetChoice{Meter: meter, Order: i + 1, Why: w.why}
		}
	}
	return out
}

// codexSingle is the Codex account loaded one at a time, and since when: the
// account held, while it qualifies or waits out a gap in the pool's need
// (holdWaits); else the one worth the most.
func codexSingle(qualified []resetWeighing, hold resetCodexHold, holdWaits bool, now time.Time) (string, time.Time) {
	switch {
	case holdWaits:
		return hold.key, hold.since
	case len(qualified) == 0:
		return "", time.Time{}
	}
	for _, w := range qualified {
		if w.candidate.key == hold.key {
			return hold.key, hold.since
		}
	}
	return slices.MinFunc(qualified, compareValue).candidate.key, now
}

// resetChoiceGroup is where a chosen account falls in its meter's order.
func resetChoiceGroup(w resetWeighing) int {
	switch {
	case w.unused:
		return 2
	case w.window:
		return 0
	}
	return 1
}

// compareChosen orders the accounts chosen for one meter.
func compareChosen(a, b resetWeighing) int {
	if ga, gb := resetChoiceGroup(a), resetChoiceGroup(b); ga != gb {
		return ga - gb
	}
	if pa, pb := resetChoicePlace(a), resetChoicePlace(b); pa != pb {
		return cmp.Compare(pa, pb)
	}
	if resetChoiceGroup(a) != 1 && !a.expires.Equal(b.expires) {
		switch {
		case a.expires.IsZero():
			return 1
		case b.expires.IsZero():
			return -1
		case a.expires.Before(b.expires):
			return -1
		}
		return 1
	}
	return compareValue(a, b)
}

// resetChoicePlace sorts an account back in its place from before a gap in
// the pool's need by that place, ahead of every account chosen afresh.
func resetChoicePlace(w resetWeighing) int {
	if w.place > 0 {
		return w.place
	}
	return math.MaxInt
}

// resetChoiceGap is how long a choice keeps its place once it stops
// qualifying only because the pool's need on its meter turned no or unknown
// (PATCHES §96), so a need that flickers does not swap the account loaded for
// another.
const resetChoiceGap = 30 * time.Minute

// resetGap is a choice keeping its place through a gap in the pool's need:
// the choice as it stood when the gap began, and when that was.
type resetGap struct {
	chosen resetChosen
	since  time.Time
}

// chooseAcrossGaps is chooseResets with what the last pass left: its
// choices and the gaps still open (PATCHES §96). A choice that now fails only
// on the pool keeps its place for resetChoiceGap from then. It is not chosen
// while it waits, so nothing publishes or ranks it, and a Codex hold on it
// stands. When it qualifies again before its time is up it takes its old order
// back, ahead of any account chosen afresh, until that time. A choice that
// stops for any other reason is dropped at once. It returns the choice and the
// gaps to keep.
func chooseAcrossGaps(weighed []resetWeighing, before map[string]resetChosen, gaps map[string]resetGap,
	hold resetCodexHold, now time.Time) (resetChoosing, map[string]resetGap) {
	kept := map[string]resetGap{}
	for i := range weighed {
		w := &weighed[i]
		key := w.candidate.key
		gap, open := gaps[key]
		open = open && now.Sub(gap.since) <= resetChoiceGap
		was, chosen := before[key]
		switch {
		case w.qualifies && open && gap.chosen.choice.Meter == w.meter:
			w.place = gap.chosen.choice.Order
			w.why += fmt.Sprintf("; it keeps the place it had before the pool's need paused, until %s", rfc3339(gap.since.Add(resetChoiceGap)))
			kept[key] = gap
		case w.qualifies || !w.onPool:
			// Chosen afresh, or dropped at once.
		case chosen:
			gap = resetGap{chosen: was, since: now}
			fallthrough
		case open:
			w.waiting = true
			w.why += fmt.Sprintf("; it keeps its place until %s, should the need return", rfc3339(gap.since.Add(resetChoiceGap)))
			kept[key] = gap
		}
	}
	_, chosen := before[hold.key]
	if _, open := kept[hold.key]; !chosen && !open {
		// The hold waited out its gap and the need did not return in time.
		hold = resetCodexHold{}
	}
	return chooseResets(weighed, hold, now), kept
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// compareValue puts the claim worth more first, then the account with more
// time from its wall to its roll, then by label and ID so the order is fixed.
func compareValue(a, b resetWeighing) int {
	switch {
	case a.value != b.value:
		if a.value > b.value {
			return -1
		}
		return 1
	case a.toRoll != b.toRoll:
		if a.toRoll > b.toRoll {
			return -1
		}
		return 1
	case a.candidate.label != b.candidate.label:
		return strings.Compare(a.candidate.label, b.candidate.label)
	}
	return strings.Compare(a.candidate.id, b.candidate.id)
}

// resetChosen is a choice as the daemon keeps it: with the account's label
// and provider, for its log lines.
type resetChosen struct {
	choice          selectacct.ResetChoice
	label, provider string
}

// Log lines (PATCHES §95.1): one when an account becomes chosen, one when it
// stops. In shadow mode they say what real mode would choose.
const (
	resetChooseMessage        = "reset loading: chose an account to run to its limit for its next saved reset"
	resetUnchooseMessage      = "reset loading: an account is no longer chosen"
	resetWouldChooseMessage   = "reset loading: would choose an account to run to its limit for its next saved reset"
	resetWouldUnchooseMessage = "reset loading: would no longer choose an account"
)

// chooseResetAccounts is the choice of one background pass (autoResetPass,
// which runs nothing with the mode off): it weighs each of this daemon's own
// accounts against the pool, keeps what it chose, and logs each account that
// becomes chosen or stops. Only the daemon that holds an account chooses it;
// with SUBROUTER_RESET_LOADING=0 it chooses nothing.
func (s Server) chooseResetAccounts() {
	mode, _ := AutoReset()
	if !ResetLoading() || s.AccountRef == nil || s.CredentialBroker != nil {
		return
	}
	statuses, ok := s.AccountRef.UsageStatusSnapshot()
	if !ok {
		return
	}
	memory := s.AccountRef.sharedAutoResets()
	now := memory.clock()
	rows := s.AccountRef.withUsageBurn(s.withHeldResets(statuses))
	pool := s.resetPoolOf(rows, now)
	scheduler := s.scheduler().WithReserves(s.AccountReserves)
	var weighed []resetWeighing
	for i := range rows {
		if candidate, ok := s.resetCandidateOf(&rows[i], scheduler); ok {
			weighed = append(weighed, pool.weigh(candidate))
		}
	}
	memory.mu.Lock()
	last, gaps, hold := memory.choices, memory.resetGaps, memory.codexHold
	memory.mu.Unlock()
	choosing, gaps := chooseAcrossGaps(weighed, last, gaps, hold, now)
	labels := map[string]resetChosen{}
	for _, w := range weighed {
		labels[w.candidate.key] = resetChosen{label: w.candidate.label, provider: w.candidate.provider}
	}
	chosen := map[string]resetChosen{}
	for key, choice := range choosing.choices {
		held := labels[key]
		held.choice = choice
		chosen[key] = held
	}
	memory.mu.Lock()
	before := memory.choices
	memory.choices, memory.codexHold, memory.resetGaps = chosen, choosing.codex, gaps
	memory.mu.Unlock()
	s.logResetChoices(mode, before, chosen, choosing.notChosen)
}

// resetCandidateOf reads one of this daemon's own rows as a candidate; false
// for a row this daemon cannot choose: not a Claude or Codex OAuth account,
// or not one it holds.
func (s Server) resetCandidateOf(row *AccountUsageStatus, scheduler selectacct.Scheduler) (resetCandidate, bool) {
	provider := accountProviderFor(row.Provider)
	if row.AuthMode != accounts.AuthModeOAuth || (provider != accounts.ProviderClaude && provider != accounts.ProviderCodex) {
		return resetCandidate{}, false
	}
	account, own := s.autoResetOwnAccount(provider, row.ID)
	if !own {
		return resetCandidate{}, false
	}
	c := resetCandidate{key: autoResetKey(account), provider: string(provider), id: account.ID, label: autoResetLabel(row, account.ID),
		readings: autoResetReadingsOf(row), burn: row.Burn}
	next, held := s.nextHeldReset(account, row)
	score, _ := scheduler.KnownScore(schedulerAccountProvider(provider), account.ID)
	unsettled := s.autoResetUnsettledClaim(c.key, c.readings)
	switch dead := s.autoResetDeadCredential(account); {
	case s.pinOnlyAccount(account.ID):
		c.out = "it takes pinned sessions only (SUBROUTER_PIN_ONLY_ACCOUNTS)"
	case dead != "":
		c.out = "its credential is dead: " + dead
	case score.Reserve > 0:
		c.out = fmt.Sprintf("it has an owner's reserve of %.0f%% of its week, which loading would spend", score.Reserve*100)
	case unsettled != "":
		c.out = unsettled
	case row.Resets == nil:
		c.out = "its saved resets have not been read"
	case !held:
		c.out = "it holds no saved reset"
	}
	c.next = next
	return c, true
}

// nextHeldReset is the provider's next reset on account, from its row's
// resets list: for Claude the grant Anthropic names next (next_grant_id) in
// the block the claim itself reads, else the list's first; for Codex the
// credit that expires soonest, which the redeem takes first whatever its
// type. False when the account holds none.
func (s Server) nextHeldReset(account accounts.Account, row *AccountUsageStatus) (accounts.HeldReset, bool) {
	list := row.Resets
	if len(list) == 0 {
		return accounts.HeldReset{}, false
	}
	if accountProviderOrCodex(account) == accounts.ProviderCodex {
		next := list[0]
		soonest, dated := next.Expires()
		for _, entry := range list[1:] {
			if at, ok := entry.Expires(); ok && (!dated || at.Before(soonest)) {
				next, soonest, dated = entry, at, true
			}
		}
		return next, true
	}
	state := s.AccountRef.cachedClaudeResetState(account.ID, account.Token)
	if state.status == nil {
		return list[0], true
	}
	for _, grant := range state.status.Grants {
		if grant.ID == "" || grant.ID != state.status.NextGrantID {
			continue
		}
		kind := grant.Kind()
		if kind == agentclaude.ResetKindOther {
			return accounts.HeldReset{Kind: accounts.ResetKindUnknown, RawType: grant.Type, Count: 1, ExpiresAt: grant.EndsAt,
				GivesBack: grant.Clears, ManualOnly: true}, true
		}
		for _, entry := range list {
			if entry.Kind == accounts.ResetKind(kind) {
				return entry, true
			}
		}
	}
	return list[0], true
}

// logResetChoices logs each account that stopped being chosen, then each that
// became chosen; an account whose choice moved to another meter logs both. A
// choice whose order alone changed logs nothing.
func (s Server) logResetChoices(mode AutoResetMode, before, after map[string]resetChosen, notChosen map[string]string) {
	if s.Logger == nil {
		return
	}
	choose, unchoose := resetChooseMessage, resetUnchooseMessage
	if mode == AutoResetShadow {
		choose, unchoose = resetWouldChooseMessage, resetWouldUnchooseMessage
	}
	line := func(message string, held resetChosen, reason string) {
		s.Logger.Info(message, "account", held.label, "provider", held.provider, "mode", mode.String(),
			"meter", string(held.choice.Meter), "order", held.choice.Order, "reason", reason)
	}
	for _, key := range sortedKeys(before) {
		was := before[key]
		if now, ok := after[key]; ok && now.choice.Meter == was.choice.Meter {
			continue
		}
		reason, ok := notChosen[key]
		switch {
		case !ok && after[key].choice.Meter != "":
			reason = "it is chosen for its " + string(after[key].choice.Meter) + " now"
		case !ok:
			reason = "it is no longer one of this daemon's accounts"
		}
		line(unchoose, was, reason)
	}
	for _, key := range sortedKeys(after) {
		if was, ok := before[key]; ok && was.choice.Meter == after[key].choice.Meter {
			continue
		}
		line(choose, after[key], after[key].choice.Why)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// withResetChoices puts each chosen account's choice on its row (PATCHES
// §95.1): inside normalized in real mode, where every daemon that ranks the
// account reads its holder's verdicts, and as would_choose beside it in shadow
// mode, where nothing ranks on it and normalized stays as it was. Nothing with
// the mode off or SUBROUTER_RESET_LOADING=0.
func (s Server) withResetChoices(statuses []AccountUsageStatus) []AccountUsageStatus {
	mode, _ := AutoReset()
	if mode == AutoResetOff || !ResetLoading() || s.AccountRef == nil || s.CredentialBroker != nil {
		return statuses
	}
	memory := s.AccountRef.sharedAutoResets()
	memory.mu.Lock()
	// Replaced whole at each pass, never written in place.
	chosen := memory.choices
	memory.mu.Unlock()
	if len(chosen) == 0 {
		return statuses
	}
	out := append([]AccountUsageStatus(nil), statuses...)
	for i := range out {
		held, ok := chosen[autoResetKey(accounts.Account{ID: out[i].ID, Provider: accountProviderFor(out[i].Provider)})]
		if !ok || out[i].AuthMode != accounts.AuthModeOAuth {
			continue
		}
		choice := held.choice
		if mode == AutoResetShadow {
			out[i].WouldChoose = &choice
			continue
		}
		if out[i].Normalized == nil {
			continue
		}
		normalized := *out[i].Normalized
		normalized.ResetChoice = &choice
		out[i].Normalized = &normalized
	}
	return out
}

// rankedResetChoices is the choices selection ranks this daemon's own
// accounts on (PATCHES §95.2), keyed by ScoreKey: the ones it publishes inside
// normalized, so in real mode only. A choice in shadow mode is would_choose,
// and nothing ranks on it; with SUBROUTER_RESET_LOADING=0 the pass chooses
// none. A peer's choice rides on its score instead (remotePoolNormalized.score).
func (s Server) rankedResetChoices() map[string]selectacct.ResetChoice {
	if mode, _ := AutoReset(); mode != AutoResetReal || s.AccountRef == nil || s.CredentialBroker != nil {
		return nil
	}
	memory := s.AccountRef.sharedAutoResets()
	memory.mu.Lock()
	// Replaced whole at each pass, never written in place.
	chosen := memory.choices
	memory.mu.Unlock()
	if len(chosen) == 0 {
		return nil
	}
	out := make(map[string]selectacct.ResetChoice, len(chosen))
	for key, held := range chosen {
		out[key] = held.choice
	}
	return out
}

// refreshStaleResetPeers reads again each peer whose last usage body is older
// than resetPeerRefreshAge, or that has none, through the read every routing
// decision and the board share (one fetch in flight per peer, a failed one
// not retried inside remotePoolAdminTTL). Only the background pass calls it:
// never a turn at the wall, and never the usage-status handler, so two peers
// cannot set off each other's reads.
func (s Server) refreshStaleResetPeers() {
	deadline := time.Now().Add(peerViewReadWait)
	var wg sync.WaitGroup
	for _, p := range s.peers() {
		if usage := p.cachedUsage(); len(usage.Body) > 0 && time.Since(usage.At) < resetPeerRefreshAge {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.usageForDecision(deadline)
		}()
	}
	wg.Wait()
}

// autoResetPass is one background pass of automatic resets: it reads again
// each peer whose body is stale, then looks at each new reading for the
// reading moment (§93.1), then chooses the accounts to load (§95.1). With the
// mode off it does nothing.
func (s Server) autoResetPass(ctx context.Context) {
	if mode, _ := AutoReset(); mode == AutoResetOff || s.AccountRef == nil || s.CredentialBroker != nil || ctx.Err() != nil {
		return
	}
	s.refreshStaleResetPeers()
	s.autoResetReadings(ctx)
	s.chooseResetAccounts()
}
