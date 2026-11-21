// Package burnpace turns what each usage source said into the 7-day burn-pace
// read-out: Snapshot (what was read), Derive to a Model (what it means), then
// text.
//
// The package reads no clock, environment, file or socket. Every rule of the
// read-out lives here: which window is the week, which sources count, what is
// too old to count, how duplicates merge, and every word the page prints. One
// rule is deliberately not here: whether a peer is a box that was already read
// directly. The CLI decides that once, for `sr status` and for this package,
// and hands the answer in with Peer.SameBoxAs.
package burnpace

import "time"

// Snapshot is what one read-out was told, source by source, before any
// judgement. The zero Snapshot is "nothing was read".
type Snapshot struct {
	// Local is this machine's daemon.
	Local Reading
	// Boxes are the servers.json sections that were planned, in the order the
	// server store returns them. A section the gates excluded is absent, not
	// Unread.
	Boxes []Box
	// Peers is the daemon's peer view. The zero value is "not asked".
	Peers PeerView
	// Config says why remote reads were not planned, when the page says so.
	Config []ConfigNote
}

// Reading is what one direct source said: its rows, or why there are none.
// The zero Reading is unread, so an unread source can never be drawn as an
// empty pool; an empty pool is built on purpose with Answered().
type Reading struct {
	state  readState
	rows   []Row
	reason string
}

type readState uint8

const (
	readUnread readState = iota
	readAnswered
)

// Answered is a source that answered. No rows means a measured, empty pool.
func Answered(rows ...Row) Reading { return Reading{state: readAnswered, rows: rows} }

// Unread is a source that did not answer. reason is printed as given, so the
// caller makes it short and safe first. An empty reason prints as
// "read failed".
func Unread(reason string) Reading { return Reading{state: readUnread, reason: reason} }

func (r Reading) answered() bool { return r.state == readAnswered }

// Box is one servers.json section and what its own address said. Name keeps
// its case; MISSING and NOT READ lines upper-case it.
type Box struct {
	Name    string
	Reading Reading
}

// PeerView is the daemon's answer to "what do your peers report". The zero
// value is "not asked". An absent view (a daemon older than the route) adds
// nothing and says nothing. A failed view is said only when some box also
// failed.
type PeerView struct {
	state  peerViewState
	peers  []Peer
	reason string
}

type peerViewState uint8

const (
	peersNotAsked peerViewState = iota
	peersAbsent
	peersFailed
	peersAnswered
)

func PeersAbsent() PeerView { return PeerView{state: peersAbsent} }

// PeersFailed is a peer view that could not be read. An empty reason prints
// as "read failed".
func PeersFailed(reason string) PeerView { return PeerView{state: peersFailed, reason: reason} }

// PeersAnswered is the daemon's peers, in its order.
func PeersAnswered(peers ...Peer) PeerView { return PeerView{state: peersAnswered, peers: peers} }

// Peer is one entry of the peer view, built by exactly one of PeerCurrent,
// PeerLastGood and PeerBodiless. The zero Peer is not a peer and is skipped.
type Peer struct {
	name   string
	link   LinkState
	rows   []Row
	asOf   time.Time
	reason string
	sameAs string
}

// LinkState is what the daemon holds for a peer.
type LinkState uint8

const (
	// LinkNone is the zero value: not a peer.
	LinkNone LinkState = iota
	// LinkCurrent is a body the daemon's latest read returned.
	LinkCurrent
	// LinkLastGood is an older body the daemon kept because its latest read
	// of the peer failed.
	LinkLastGood
	// LinkBodiless is a peer the daemon holds no body for.
	LinkBodiless
)

// PeerCurrent is a peer the daemon reached, with the body it read at asOf.
// A zero asOf counts with no age and no rebase of its reset clocks.
func PeerCurrent(name string, asOf time.Time, rows ...Row) Peer {
	return Peer{name: name, link: LinkCurrent, rows: rows, asOf: asOf}
}

// PeerLastGood is a peer the daemon's latest read failed on, with the last
// body it holds. A zero asOf is "of unknown age" and is never counted. An
// empty reason prints as "its latest read failed".
func PeerLastGood(name string, asOf time.Time, reason string, rows ...Row) Peer {
	return Peer{name: name, link: LinkLastGood, rows: rows, asOf: asOf, reason: reason}
}

// PeerBodiless is a peer the daemon holds no body for. An empty reason prints
// as "no reading yet".
func PeerBodiless(name, reason string) Peer {
	return Peer{name: name, link: LinkBodiless, reason: reason}
}

// SameBoxAs returns the peer marked as the box of that name: the same pool,
// reached a second way, so its accounts are already in the snapshot.
//
// The caller decides this with the roster rule it also uses for the status
// table. Derive honours the mark only when box names a Box of the same
// Snapshot whose Reading answered, and never on a bodiless peer; otherwise
// the peer is judged as its own source. That is the safe direction: the merge
// counts a shared account once either way, while a wrongly honoured mark would
// drop accounts.
func (p Peer) SameBoxAs(box string) Peer {
	p.sameAs = box
	return p
}

// Row is one account as a daemon published it: the facts the read-out uses
// and the windows. Which window is the week is this package's rule
// (weekRules), so a Row carries all of them.
type Row struct {
	Provider    string // only providers in weekRules are counted or named
	ID          string // "" is never counted and never named
	AuthMode    string // "", "oauth", or anything else; gates only the naming of a left-out row
	AuthChecked bool
	AuthValid   bool
	SweepCut    bool
	Error       string
	Windows     []Window
}

// Window is one quota window of a Row, in the daemon's own units.
type Window struct {
	Name              string
	UsedPercent       float64
	LimitSeconds      int64
	ResetAfterSeconds int64 // relative to the reading, not to now
}

// ConfigNote is a reason remote reads were not planned. It is a closed set,
// so the wording stays in this package.
type ConfigNote struct {
	kind   configKind
	why    KeyRefusal
	path   string
	detail string
}

type configKind uint8

const (
	configServersSwitchedOff configKind = iota + 1
	configServersFileUnreadable
	configKeyRefused
)

// ServersSwitchedOff is SR_STATUS_SERVERS turned off: local only, said dimly.
func ServersSwitchedOff() ConfigNote { return ConfigNote{kind: configServersSwitchedOff} }

// ServersFileUnreadable is a servers.json that could not be loaded: its boxes
// were not read, the peer view still was. kind is a short fixed phrase such
// as "not valid JSON", never file content.
func ServersFileUnreadable(kind string) ConfigNote {
	return ConfigNote{kind: configServersFileUnreadable, detail: kind}
}

// KeyRefusal is why a remote key that exists did not open the gate. A key
// that does not exist is not a refusal, and has no note.
type KeyRefusal uint8

const (
	KeyNotRegularFile KeyRefusal = iota + 1
	// KeyModeTooOpen takes the permission bits as four octal digits, e.g. "0644".
	KeyModeTooOpen
	// KeyUnreadable takes a short fixed phrase for the error, never its full text.
	KeyUnreadable
	KeyTooShort
)

// RemoteKeyRefused is the gate's published refusal. The caller passes what the
// gate found; it never inspects the key itself.
func RemoteKeyRefused(why KeyRefusal, path, detail string) ConfigNote {
	return ConfigNote{kind: configKeyRefused, why: why, path: path, detail: detail}
}
