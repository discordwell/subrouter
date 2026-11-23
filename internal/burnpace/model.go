package burnpace

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

const (
	weekSeconds   = 604800.0
	weekHours     = 168.0
	staleAfter    = 120.0 // s; a peer body this old or older shows its age
	lastGoodMax   = 900.0 // s; a peer body older than this is not counted
	minShare      = 0.05  // window-weeks a pool must have run to have signal
	meterStep     = 0.005 // a meter reads in 0.5 % steps ...
	maxStepShare  = 0.10  // ... and a pool's steps may be at most this share of what it drew
	conflictUsed  = 0.02
	conflictHours = 1.0
	expiringMin   = 0.02 // an account this early in its window is not "expiring"
	expiringCount = 3
)

// Model is what a Snapshot means: facts, no layout. Everything computable
// from the fields is a method, so nothing in it can fall out of step.
type Model struct {
	// Now is the instant every age and reset was computed against. The page's
	// clock and day labels are printed from it, in its location.
	Now time.Time

	// Sources is every source the snapshot named, with the one verdict on
	// each, in visit order: local, the boxes in snapshot order, then the peers
	// in peer-view order.
	Sources []Source

	// PeerViewFailure is why the daemon's peer view could not be read, or ""
	// when it was not asked, was absent, or answered.
	PeerViewFailure string

	// Config is the snapshot's configuration notes, unchanged.
	Config []ConfigNote

	// Providers are the pools with at least one merged account, in weekRules
	// order.
	Providers []Provider

	// Folded counts later copies of an account already merged: one per extra
	// copy, not one per account.
	Folded int
	// LeftOut are the named rows that are not in the numbers, one per account
	// with the first reason seen, sorted by Name as strings.
	LeftOut []LeftOut
	// Stale counts merged accounts that some scope reported as sweep-cut.
	Stale int
	// Conflicts name the account of each later copy that disagreed with the
	// copy kept, by the kept copy's Name, in visit order, not de-duplicated.
	Conflicts []string
}

// SourceKind is where a source's rows came from.
type SourceKind uint8

const (
	Local    SourceKind = iota + 1 // this machine's daemon
	Direct                         // a servers.json box, read at its own address
	PeerLink                       // a peer of this machine's daemon, read from its peer view
)

// Standing is the one verdict on a source. Counted and AnsweredUnmeasured are
// in the page; the rest are not.
type Standing uint8

const (
	// Counted answered and is behind the numbers. An empty pool counts.
	Counted Standing = iota + 1
	// AnsweredUnmeasured answered, but none of its rows was measured this read.
	AnsweredUnmeasured
	// Missing is a direct read that failed, with no peer standing in.
	Missing
	// Hedged is a box whose own address failed while at least as many peers
	// stood in as boxes failed: it may be one of them.
	Hedged
	// Outnumbered is a box that failed along with others, with fewer peers
	// standing in than boxes failed.
	Outnumbered
	// SameBox is a peer that is a box already read directly.
	SameBox
	// TooOld is a peer body older than 15 minutes, or a last-good body of
	// unknown age.
	TooOld
	// NoReading is a peer the daemon holds no body for.
	NoReading
)

var standingWords = [...]string{
	Counted:            "counted",
	AnsweredUnmeasured: "answered, unmeasured",
	Missing:            "missing",
	Hedged:             "hedged",
	Outnumbered:        "outnumbered",
	SameBox:            "same box",
	TooOld:             "too old",
	NoReading:          "no reading",
}

func (s Standing) String() string { return word(standingWords[:], s, "Standing") }

// InPage reports whether a source with this standing is a scope of the page.
func (s Standing) InPage() bool { return s == Counted || s == AnsweredUnmeasured }

// Source is one thing the snapshot was told about, and the verdict on it.
type Source struct {
	Kind SourceKind
	// Name is "local", the box's servers.json name, or the peer's name.
	Name     string
	Standing Standing
	// Link and AsOf are a PeerLink source's own state: what the daemon holds,
	// and when it measured it (zero for no usable stamp).
	Link LinkState
	AsOf time.Time
	// Reason is why a direct read failed, why a peer's link is failing, or
	// why a peer has no body; "" when none of those applies.
	Reason string
	// SameAs is the box a SameBox peer is.
	SameAs string
	// Measured counts this source's rows that entered the numbers, folded
	// copies included. Unmeasured counts its distinct accounts left out as
	// unmeasured that no source measured. Both are 0 off the page.
	Measured, Unmeasured int
}

// Scope is the source's name on the accounts line and in UNMEASURED lines.
func (s Source) Scope() string {
	if s.Kind == PeerLink {
		return s.Name + " (peer link)"
	}
	return s.Name
}

// Scopes are the sources in the page, in visit order.
func (m Model) Scopes() []Source {
	var in []Source
	for _, s := range m.Sources {
		if s.Standing.InPage() {
			in = append(in, s)
		}
	}
	return in
}

// Label is a scope's text in the headline, without its measured count.
func (m Model) Label(s Source) string {
	if s.Kind != PeerLink {
		return s.Name
	}
	label := s.Name + " via peer link"
	age, aged := ageOf(m.Now, s.AsOf)
	switch {
	case !aged:
	case s.Link == LinkLastGood:
		label += ", last good as of " + ago(age)
	case age >= staleAfter:
		label += ", as of " + ago(age)
	}
	return label
}

// Note is one status sentence.
type Note struct {
	Tone Tone
	Text string
}

// Tone is how loud a Note is.
type Tone uint8

const (
	Amber Tone = iota + 1 // something is missing from the numbers
	Dim                   // context; nothing is missing
	Grey                  // a caveat on numbers that are present
)

// Provider is one provider's merged pool.
type Provider struct {
	Name     string
	Accounts []Account // merge order: first seen first
}

// Account is one account's weekly reading as it entered the numbers: the
// first copy seen, with a peer copy's reset already moved on by its age.
type Account struct {
	ID           string
	Label        string  // the kept copy's label; "" when it had none
	Used         float64 // share of the week's limit spent; not clamped
	ElapsedHours float64 // of its 168 h window; negative when the reset is over a week out
	ResetHours   float64 // until the window rolls; always > 0
}

// Name is what the page calls the account: its label, else its ID. A Codex
// account enrolled with owner claims has an opaque "codex-owner-<hash>" ID and
// its login email as its label.
func (a Account) Name() string { return cmp.Or(a.Label, a.ID) }

// Drawn is the sum of Used across the pool.
func (p Provider) Drawn() float64 {
	return pySum(p.Accounts, func(a Account) float64 { return a.Used })
}

// Share is the pool's elapsed window time in window-weeks.
func (p Provider) Share() float64 {
	return pySum(p.Accounts, func(a Account) float64 { return a.ElapsedHours }) / weekHours
}

// Signal reports whether Pace means anything: enough window has run, and the
// mean draw is well above the meter's step.
func (p Provider) Signal() bool {
	drawn := p.Drawn()
	return p.Share() >= minShare && drawn > 0 && float64(len(p.Accounts))*meterStep/drawn <= maxStepShare
}

// Pace is Drawn over Share: 1.00 is on course to end the week at exactly
// 100 %. Only meaningful when Signal.
func (p Provider) Pace() float64 { return p.Drawn() / p.Share() }

// CeilingAt is what Pace would read hours from now if every account were
// fully spent then, each window having rolled over at its reset. ok is false
// when no window has run at all.
func (p Provider) CeilingAt(hours float64) (ceiling float64, ok bool) {
	if len(p.Accounts) == 0 {
		return 0, false
	}
	run := pySum(p.Accounts, func(a Account) float64 { return floorMod(a.ElapsedHours+hours, weekHours) })
	if run < 1e-9 {
		return 0, false
	}
	return float64(len(p.Accounts)) * weekHours / run, true
}

// LeftOut is a row that is named on the page and is not in the numbers.
type LeftOut struct {
	Provider string
	ID       string
	Label    string // the first left-out copy's label; "" when it had none
	Why      LeftOutReason
}

// Name is what the page calls the account, as Account.Name.
func (l LeftOut) Name() string { return cmp.Or(l.Label, l.ID) }

// LeftOutReason is a closed set. Its order is the order the kinds are counted
// in on the page.
type LeftOutReason uint8

const (
	LeftUnmeasured LeftOutReason = iota + 1
	LeftAuthInvalid
	LeftNoWeeklyWindow
	LeftRolled
)

var leftOutWords = [...]string{
	LeftUnmeasured:     "unmeasured",
	LeftAuthInvalid:    "auth-invalid",
	LeftNoWeeklyWindow: "no weekly window",
	LeftRolled:         "rolled since the reading",
}

// String is the page's wording.
func (r LeftOutReason) String() string { return word(leftOutWords[:], r, "LeftOutReason") }

// Outcome is why a page has, or does not have, numbers.
type Outcome uint8

const (
	Numbers         Outcome = iota + 1 // at least one Counted scope
	NothingRead                        // no source is in the page
	NothingMeasured                    // sources are in the page; none is Counted
)

func (m Model) Outcome() Outcome {
	switch {
	case slices.ContainsFunc(m.Sources, func(s Source) bool { return s.Standing == Counted }):
		return Numbers
	case slices.ContainsFunc(m.Sources, func(s Source) bool { return s.Standing.InPage() }):
		return NothingMeasured
	default:
		return NothingRead
	}
}

// Drawn reports whether the page has numbers, which is the command's exit
// status. An empty but measured pool is Drawn.
func (m Model) Drawn() bool { return m.Outcome() == Numbers }

// Accounts is the number of merged accounts across Providers.
func (m Model) Accounts() int {
	n := 0
	for _, p := range m.Providers {
		n += len(p.Accounts)
	}
	return n
}

// Charted is the provider whose ceiling is charted: among those with Signal,
// the one with the most accounts, the first on a tie.
func (m Model) Charted() (p Provider, ok bool) {
	for _, candidate := range m.Providers {
		if candidate.Signal() && (!ok || len(candidate.Accounts) > len(p.Accounts)) {
			p, ok = candidate, true
		}
	}
	return p, ok
}

// Expiring is up to three accounts closest to their reset, across every
// provider, leaving out any that started its window under 3.36 h ago. Ties
// keep pool order.
func (m Model) Expiring() []Account {
	var soon []Account
	for _, p := range m.Providers {
		for _, a := range p.Accounts {
			if a.ElapsedHours/weekHours >= expiringMin {
				soon = append(soon, a)
			}
		}
	}
	slices.SortStableFunc(soon, func(a, b Account) int { return cmp.Compare(a.ResetHours, b.ResetHours) })
	return soon[:min(len(soon), expiringCount)]
}

// Top is the sentences printed right under the rule, in page order.
func (m Model) Top() []Note {
	var notes []Note
	say := func(tone Tone, format string, args ...any) {
		notes = append(notes, Note{Tone: tone, Text: fmt.Sprintf(format, args...)})
	}

	for _, s := range m.Sources {
		if s.Kind == Local && s.Standing == Missing {
			say(Amber, "LOCAL MISSING — this machine's accounts are not in the numbers below (%s)", s.Reason)
		}
	}

	for _, s := range m.Sources {
		if s.Kind != PeerLink {
			continue
		}
		switch {
		case s.Standing == TooOld:
			when := "holds a last-good reading of unknown age"
			if age, aged := ageOf(m.Now, s.AsOf); aged {
				when = "last read it " + ago(age)
			}
			because := ""
			if s.Link == LinkLastGood {
				because = " (" + s.Reason + ")"
			}
			say(Amber, "%s MISSING — this machine's peer link %s, too old to count%s", strings.ToUpper(s.Name), when, because)
		case s.Standing.InPage() && s.Link == LinkLastGood:
			say(Amber, "%s is a last-good reading — this machine's daemon cannot reach it now (%s)", s.Name, s.Reason)
		}
	}

	var standIns, answeredBoxes []string
	var failedBoxes []Source
	for _, s := range m.Sources {
		switch {
		case s.Kind == PeerLink && s.Standing.InPage():
			standIns = append(standIns, s.Scope())
		case s.Kind == Direct && s.Standing.InPage():
			answeredBoxes = append(answeredBoxes, s.Name)
		case s.Kind == Direct:
			failedBoxes = append(failedBoxes, s)
		}
	}
	isAre, oneSome := "is", "one"
	if len(standIns) > 1 {
		isAre, oneSome = "are", "some"
	}
	var outnumbered, outnumberedWhy []string
	for _, s := range failedBoxes {
		switch s.Standing {
		case Hedged:
			say(Amber, "%s NOT READ (%s) — %s %s in the numbers; if that is %s, nothing is missing",
				strings.ToUpper(s.Name), s.Reason, strings.Join(standIns, " and "), isAre, s.Name)
		case Outnumbered:
			outnumbered = append(outnumbered, strings.ToUpper(s.Name))
			outnumberedWhy = append(outnumberedWhy, s.Name+": "+s.Reason)
		default:
			say(Amber, "%s MISSING — %s's accounts are not in the numbers below (%s)", strings.ToUpper(s.Name), s.Name, s.Reason)
		}
	}
	if len(outnumbered) > 0 {
		say(Amber, "AT LEAST %d OF %s MISSING — none was read at its servers.json address (%s); %s %s in the numbers and may be %s of them",
			len(failedBoxes)-len(standIns), strings.Join(outnumbered, ", "), strings.Join(outnumberedWhy, "; "),
			strings.Join(standIns, " and "), isAre, oneSome)
	}

	for _, s := range m.Sources {
		if s.Standing != NoReading {
			continue
		}
		if len(answeredBoxes) > 0 {
			say(Amber, "peer link to %s has no reading (%s) — unless it is %s, its accounts are not in the numbers below",
				s.Name, s.Reason, strings.Join(answeredBoxes, " or "))
		} else {
			say(Amber, "%s MISSING — this machine's peer link to it has no reading, so its accounts are not in the numbers below (%s)",
				strings.ToUpper(s.Name), s.Reason)
		}
	}

	if len(failedBoxes) > 0 && m.PeerViewFailure != "" {
		say(Dim, "no fallback through this machine's daemon: %s", m.PeerViewFailure)
	}

	for _, c := range m.Config {
		if n, ok := c.note(); ok {
			notes = append(notes, n)
		}
	}

	for _, s := range m.Sources {
		if s.Standing != AnsweredUnmeasured {
			continue
		}
		who := "this machine's"
		if s.Kind != Local {
			who = s.Scope() + "'s"
		}
		say(Amber, "%s UNMEASURED — none of %s %d accounts has a weekly reading this read (sweep cut, or not yet checked), so they are not in the numbers below",
			strings.ToUpper(s.Scope()), who, s.Unmeasured)
	}
	return notes
}

// Bottom is the sentences printed at the very end, in page order. A page with
// nothing read does not print them.
func (m Model) Bottom() []Note {
	var notes []Note
	for _, s := range m.Sources {
		if s.Standing == SameBox && s.Link == LinkLastGood {
			notes = append(notes, Note{Tone: Dim, Text: fmt.Sprintf(
				"this machine's peer link to %s is failing (%s); its accounts are in the numbers via %s", s.Name, s.Reason, s.SameAs)})
		}
	}
	for _, s := range m.Sources {
		switch {
		case s.Kind == PeerLink || s.Standing.InPage():
		case s.Standing == Hedged:
			notes = append(notes, Note{Tone: Dim, Text: fmt.Sprintf(
				"%s's servers.json address is down (%s); a peer link stood in, see above", s.Name, s.Reason)})
		default:
			notes = append(notes, Note{Tone: Amber, Text: fmt.Sprintf("%s unreachable — %s", s.Name, s.Reason)})
		}
	}
	return notes
}

func (c ConfigNote) note() (Note, bool) {
	switch c.kind {
	case configServersSwitchedOff:
		return Note{Tone: Dim, Text: "SR_STATUS_SERVERS=0: local only; the box and the daemon's peer view were not read"}, true
	case configServersFileUnreadable:
		return Note{Tone: Amber, Text: "servers.json unreadable (" + c.detail + "); its boxes were not read"}, true
	case configKeyRefused:
		switch c.why {
		case KeyNotRegularFile:
			return Note{Tone: Amber, Text: c.path + " is not a regular file; the box was not read"}, true
		case KeyModeTooOpen:
			return Note{Tone: Amber, Text: c.path + " is mode " + c.detail + "; chmod 600 it to read the box"}, true
		case KeyUnreadable:
			return Note{Tone: Amber, Text: "cannot read " + c.path + " (" + c.detail + "); the box was not read"}, true
		case KeyTooShort:
			return Note{Tone: Amber, Text: c.path + " is too short to be a key; the box was not read"}, true
		}
	}
	return Note{}, false
}

// Derive is the whole judgement. It cannot fail, and it gives the same Model
// for the same snapshot and now.
func Derive(s Snapshot, now time.Time) Model {
	sources, scopes := resolveSources(s, now)
	m := Model{Now: now, Sources: sources, Config: s.Config}
	if s.Peers.state == peersFailed {
		m.PeerViewFailure = cmp.Or(s.Peers.reason, "read failed")
	}
	mergeScopes(&m, scopes)
	return m
}

// scope is a source in the page with the rows it brings and how many seconds
// old they are.
type scope struct {
	source int
	rows   []Row
	shift  float64
}

func resolveSources(s Snapshot, now time.Time) ([]Source, []scope) {
	var sources []Source
	var scopes []scope
	direct := func(kind SourceKind, name string, r Reading) {
		src := Source{Kind: kind, Name: name}
		if r.answered() {
			scopes = append(scopes, scope{source: len(sources), rows: r.rows})
		} else {
			src.Standing = Missing
			src.Reason = cmp.Or(r.reason, "read failed")
		}
		sources = append(sources, src)
	}
	direct(Local, "local", s.Local)
	for _, b := range s.Boxes {
		direct(Direct, b.Name, b.Reading)
	}

	standIns := 0
	if s.Peers.state == peersAnswered {
		for _, p := range s.Peers.peers {
			if p.link == LinkNone {
				continue
			}
			src := Source{Kind: PeerLink, Name: p.name, Link: p.link, AsOf: p.asOf}
			switch p.link {
			case LinkLastGood:
				src.Reason = cmp.Or(p.reason, "its latest read failed")
			case LinkBodiless:
				src.Reason = cmp.Or(p.reason, "no reading yet")
			}
			age, aged := ageOf(now, p.asOf)
			switch {
			case p.link == LinkBodiless:
				src.Standing = NoReading
			case p.sameAs != "" && boxAnswered(s.Boxes, p.sameAs):
				src.Standing, src.SameAs = SameBox, p.sameAs
			case (p.link == LinkLastGood && !aged) || age > lastGoodMax:
				src.Standing = TooOld
			default:
				scopes = append(scopes, scope{source: len(sources), rows: p.rows, shift: age})
				standIns++
			}
			sources = append(sources, src)
		}
	}

	failed := 0
	for _, src := range sources {
		if src.Kind == Direct && src.Standing == Missing {
			failed++
		}
	}
	for i := range sources {
		if sources[i].Kind != Direct || sources[i].Standing != Missing || standIns == 0 {
			continue
		}
		if failed <= standIns {
			sources[i].Standing = Hedged
		} else {
			sources[i].Standing = Outnumbered
		}
	}
	return sources, scopes
}

func boxAnswered(boxes []Box, name string) bool {
	return slices.ContainsFunc(boxes, func(b Box) bool { return b.Name == name && b.Reading.answered() })
}

type accountKey struct{ provider, id string }

func mergeScopes(m *Model, scopes []scope) {
	type leftRow struct {
		key    accountKey
		label  string
		why    LeftOutReason
		source int
	}
	merged := map[accountKey]Account{}
	var order []accountKey
	var left []leftRow
	staleKeys := map[accountKey]bool{}

	for _, sc := range scopes {
		for _, row := range sc.rows {
			key := accountKey{row.Provider, row.ID}
			v := judgeRow(row, sc.shift)
			if v.stale {
				staleKeys[key] = true
			}
			switch v.kind {
			case rowLeftOut:
				left = append(left, leftRow{key: key, label: row.Label, why: v.why, source: sc.source})
			case rowCounted:
				m.Sources[sc.source].Measured++
				first, seen := merged[key]
				if !seen {
					merged[key] = v.account
					order = append(order, key)
					continue
				}
				m.Folded++
				if math.Abs(first.Used-v.account.Used) > conflictUsed || math.Abs(first.ResetHours-v.account.ResetHours) > conflictHours {
					m.Conflicts = append(m.Conflicts, first.Name())
				}
			}
		}
	}

	unmeasuredBy := map[int]map[accountKey]bool{}
	named := map[accountKey]bool{}
	for _, l := range left {
		if _, in := merged[l.key]; in {
			continue
		}
		if l.why == LeftUnmeasured {
			if unmeasuredBy[l.source] == nil {
				unmeasuredBy[l.source] = map[accountKey]bool{}
			}
			unmeasuredBy[l.source][l.key] = true
		}
		if !named[l.key] {
			named[l.key] = true
			m.LeftOut = append(m.LeftOut, LeftOut{Provider: l.key.provider, ID: l.key.id, Label: l.label, Why: l.why})
		}
	}
	slices.SortStableFunc(m.LeftOut, func(a, b LeftOut) int { return strings.Compare(a.Name(), b.Name()) })

	for key := range staleKeys {
		if _, in := merged[key]; in {
			m.Stale++
		}
	}

	for _, rule := range weekRules {
		var accounts []Account
		for _, key := range order {
			if key.provider == rule.provider {
				accounts = append(accounts, merged[key])
			}
		}
		if len(accounts) > 0 {
			m.Providers = append(m.Providers, Provider{Name: rule.provider, Accounts: accounts})
		}
	}

	for _, sc := range scopes {
		src := &m.Sources[sc.source]
		src.Unmeasured = len(unmeasuredBy[sc.source])
		if src.Measured == 0 && src.Unmeasured > 0 {
			src.Standing = AnsweredUnmeasured
		} else {
			src.Standing = Counted
		}
	}
}

// ageOf is how many seconds before now asOf was, never negative. aged is
// false for a zero asOf: the daemon gave no usable stamp.
func ageOf(now, asOf time.Time) (seconds float64, aged bool) {
	if asOf.IsZero() {
		return 0, false
	}
	return max(0, now.Sub(asOf).Seconds()), true
}

// ago is the page's age wording. The minutes are Python's float floor
// division, which never rounds up to the next minute, and %.0f rounds half to
// even as Python's does: 2.5 h is "2h", 3.5 h is "4h".
func ago(seconds float64) string {
	switch {
	case seconds < 90:
		return fmt.Sprintf("%ds ago", int(seconds))
	case seconds < 5400:
		return fmt.Sprintf("%dm ago", int((seconds-math.Mod(seconds, 60))/60))
	default:
		return fmt.Sprintf("%.0fh ago", seconds/3600)
	}
}

// pySum adds as Python 3.12's sum() does over floats, with Neumaier's
// compensation, so pace, share and ceiling match the reference script's to the
// last bit.
func pySum[T any](items []T, value func(T) float64) float64 {
	var total, compensation float64
	for _, item := range items {
		x := value(item)
		t := total + x
		if math.Abs(total) >= math.Abs(x) {
			compensation += (total - t) + x
		} else {
			compensation += (x - t) + total
		}
		total = t
	}
	if compensation != 0 && !math.IsInf(compensation, 0) && !math.IsNaN(compensation) {
		total += compensation
	}
	return total
}

// floorMod is the remainder with the divisor's sign, as Python's % gives it,
// so an account whose reset is over a week out still wraps into [0, d).
func floorMod(x, d float64) float64 {
	r := math.Mod(x, d)
	if r < 0 {
		r += d
	}
	return r
}

func word[T ~uint8](words []string, v T, typeName string) string {
	if int(v) < len(words) && words[v] != "" {
		return words[v]
	}
	return fmt.Sprintf("%s(%d)", typeName, v)
}
