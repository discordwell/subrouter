package burnpace

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func standingsOf(m Model) []Standing {
	var out []Standing
	for _, s := range m.Sources {
		out = append(out, s.Standing)
	}
	return out
}

func noteTexts(notes []Note) []string {
	var out []string
	for _, n := range notes {
		out = append(out, n.Text)
	}
	return out
}

func requireNotes(t *testing.T, what string, got, want []Note) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s:\n got  %q\n want %q", what, got, want)
	}
}

func TestTheZeroSnapshotIsNothingRead(t *testing.T) {
	for name, s := range map[string]Snapshot{
		"zero snapshot": {},
		"zero readings": {Local: Reading{}, Boxes: []Box{{Name: "home"}}, Peers: PeersAnswered()},
	} {
		t.Run(name, func(t *testing.T) {
			m := Derive(s, thursday0701)
			if m.Outcome() != NothingRead || m.Drawn() {
				t.Fatalf("outcome %v, drawn %v; want NothingRead, not drawn", m.Outcome(), m.Drawn())
			}
			if len(m.Scopes()) != 0 || m.Accounts() != 0 || len(m.Providers) != 0 {
				t.Fatalf("an unread source was drawn as a pool: scopes %v, providers %v", m.Scopes(), m.Providers)
			}
			for _, src := range m.Sources {
				if src.Standing != Missing || src.Reason != "read failed" {
					t.Fatalf("source %+v: want Missing with reason %q", src, "read failed")
				}
			}
			if top := noteTexts(m.Top()); top[0] != "LOCAL MISSING — this machine's accounts are not in the numbers below (read failed)" {
				t.Fatalf("top = %q", top)
			}
		})
	}
}

func TestAnEmptyAnsweredPoolIsDrawn(t *testing.T) {
	m := Derive(Snapshot{Local: Answered()}, thursday0701)
	if m.Outcome() != Numbers || !m.Drawn() {
		t.Fatalf("outcome %v, drawn %v; want Numbers, drawn", m.Outcome(), m.Drawn())
	}
	want := []Source{{Kind: Local, Name: "local", Standing: Counted}}
	if !slices.Equal(m.Sources, want) {
		t.Fatalf("sources = %+v, want %+v", m.Sources, want)
	}
	if m.Accounts() != 0 || len(m.Top()) != 0 || len(m.Bottom()) != 0 {
		t.Fatalf("accounts %d, top %q, bottom %q; want 0 and no notes", m.Accounts(), m.Top(), m.Bottom())
	}
}

func TestEverySourceHasExactlyOneStanding(t *testing.T) {
	for _, p := range pages() {
		t.Run(p.golden, func(t *testing.T) {
			m := Derive(p.snapshot, thursday0701)
			if got := standingsOf(m); !slices.Equal(got, p.standings) {
				t.Fatalf("standings = %v, want %v", got, p.standings)
			}
			if m.Outcome() != p.outcome || m.Drawn() != (p.outcome == Numbers) {
				t.Fatalf("outcome %v, drawn %v; want %v", m.Outcome(), m.Drawn(), p.outcome)
			}
		})
	}
}

func TestSameBoxAsIsHonouredOnlyForAnAnsweredBox(t *testing.T) {
	peerRows := boxRows()[:2]
	cases := []struct {
		name      string
		boxes     []Box
		peer      Peer
		standings []Standing
		accounts  int
		bottom    []string
	}{
		{
			name:      "an answered box",
			boxes:     home(Answered(boxRows()...)),
			peer:      PeerCurrent("box.example", secondsAgo(5), peerRows...).SameBoxAs("home"),
			standings: []Standing{Counted, Counted, SameBox},
			accounts:  5,
		},
		{
			name:      "an answered box, reached by a failing link",
			boxes:     home(Answered(boxRows()...)),
			peer:      PeerLastGood("box.example", secondsAgo(600), "", peerRows...).SameBoxAs("home"),
			standings: []Standing{Counted, Counted, SameBox},
			accounts:  5,
			bottom:    []string{"this machine's peer link to box.example is failing (its latest read failed); its accounts are in the numbers via home"},
		},
		{
			name:      "a box the snapshot does not hold",
			boxes:     home(Answered(boxRows()...)),
			peer:      PeerCurrent("box.example", secondsAgo(5), peerRows...).SameBoxAs("work"),
			standings: []Standing{Counted, Counted, Counted},
			accounts:  5,
		},
		{
			name:      "a box that was not read",
			boxes:     home(Unread("No route to host")),
			peer:      PeerCurrent("box.example", secondsAgo(5), peerRows...).SameBoxAs("home"),
			standings: []Standing{Counted, Hedged, Counted},
			accounts:  5,
			bottom:    []string{"home's servers.json address is down (No route to host); a peer link stood in, see above"},
		},
		{
			name:      "a bodiless peer",
			boxes:     home(Answered(boxRows()...)),
			peer:      PeerBodiless("box.example", "").SameBoxAs("home"),
			standings: []Standing{Counted, Counted, NoReading},
			accounts:  5,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := Derive(Snapshot{Local: Answered(localRows()...), Boxes: c.boxes, Peers: PeersAnswered(c.peer)}, thursday0701)
			if got := standingsOf(m); !slices.Equal(got, c.standings) {
				t.Fatalf("standings = %v, want %v", got, c.standings)
			}
			if m.Accounts() != c.accounts {
				t.Fatalf("accounts = %d, want %d", m.Accounts(), c.accounts)
			}
			if got := noteTexts(m.Bottom()); !slices.Equal(got, c.bottom) {
				t.Fatalf("bottom = %q, want %q", got, c.bottom)
			}
		})
	}
}

func TestScopesThatShareANameKeepSeparateCounts(t *testing.T) {
	m := Derive(Snapshot{
		Local: Answered(),
		Boxes: []Box{
			{Name: "home", Reading: Answered(claude("h-one", 10, 50), claude("h-two", 20, 60))},
			{Name: "home", Reading: Answered(cutRow("claude", "h-three"))},
		},
	}, thursday0701)
	first, second := m.Sources[1], m.Sources[2]
	if first.Standing != Counted || first.Measured != 2 || first.Unmeasured != 0 {
		t.Fatalf("first home = %+v; want Counted, 2 measured, 0 unmeasured", first)
	}
	if second.Standing != AnsweredUnmeasured || second.Measured != 0 || second.Unmeasured != 1 {
		t.Fatalf("second home = %+v; want AnsweredUnmeasured, 0 measured, 1 unmeasured", second)
	}
	want := "HOME UNMEASURED — none of home's 1 accounts has a weekly reading this read (sweep cut, or not yet checked), so they are not in the numbers below"
	if top := noteTexts(m.Top()); !slices.Equal(top, []string{want}) {
		t.Fatalf("top = %q, want %q", top, want)
	}
}

// standIn derives a page where home failed and one peer may stand in for it.
func standIn(p Peer) Model {
	return Derive(Snapshot{
		Local: Answered(),
		Boxes: home(Unread("No route to host")),
		Peers: PeersAnswered(p),
	}, thursday0701)
}

func TestPeerAgeBoundaries(t *testing.T) {
	rows := []Row{claude("p-one", 50, 10)}
	unrebased := 10.0
	cases := []struct {
		name     string
		peer     Peer
		standing Standing
		label    string  // in the page
		reset    float64 // in the page: p-one's ResetHours
		top      string  // off the page: its MISSING line
	}{
		{name: "119 s", peer: PeerCurrent("box", secondsAgo(119), rows...), standing: Counted,
			label: "box via peer link", reset: (36000 - 119) / 3600.0},
		{name: "120 s", peer: PeerCurrent("box", secondsAgo(120), rows...), standing: Counted,
			label: "box via peer link, as of 2m ago", reset: (36000 - 120) / 3600.0},
		{name: "900 s", peer: PeerCurrent("box", secondsAgo(900), rows...), standing: Counted,
			label: "box via peer link, as of 15m ago", reset: (36000 - 900) / 3600.0},
		{name: "901 s", peer: PeerCurrent("box", secondsAgo(901), rows...), standing: TooOld,
			top: "BOX MISSING — this machine's peer link last read it 15m ago, too old to count"},
		{name: "901 s, last good", peer: PeerLastGood("box", secondsAgo(901), ioTimeout, rows...), standing: TooOld,
			top: "BOX MISSING — this machine's peer link last read it 15m ago, too old to count (" + ioTimeout + ")"},
		{name: "a stamp in the future", peer: PeerCurrent("box", thursday0701.Add(time.Minute), rows...), standing: Counted,
			label: "box via peer link", reset: unrebased},
		{name: "no stamp", peer: PeerCurrent("box", time.Time{}, rows...), standing: Counted,
			label: "box via peer link", reset: unrebased},
		{name: "no stamp, last good", peer: PeerLastGood("box", time.Time{}, "", rows...), standing: TooOld,
			top: "BOX MISSING — this machine's peer link holds a last-good reading of unknown age, too old to count (its latest read failed)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := standIn(c.peer)
			peer := m.Sources[2]
			if peer.Standing != c.standing {
				t.Fatalf("standing = %v, want %v", peer.Standing, c.standing)
			}
			if c.standing == TooOld {
				if top := noteTexts(m.Top()); top[0] != c.top {
					t.Fatalf("top = %q, want first %q", top, c.top)
				}
				if m.Accounts() != 0 {
					t.Fatalf("a peer too old to count put %d accounts in the numbers", m.Accounts())
				}
				return
			}
			if got := m.Label(peer); got != c.label {
				t.Fatalf("label = %q, want %q", got, c.label)
			}
			if got := m.Providers[0].Accounts[0].ResetHours; got != c.reset {
				t.Fatalf("reset = %v h, want %v h", got, c.reset)
			}
		})
	}
}

func TestAgoWording(t *testing.T) {
	inPage := []struct {
		seconds float64
		want    string
	}{
		{0, "0s ago"}, {89.9, "89s ago"}, {90, "1m ago"}, {119.9, "1m ago"}, {899, "14m ago"},
	}
	for _, c := range inPage {
		m := standIn(PeerLastGood("box", secondsAgo(c.seconds), "", claude("p-one", 50, 100)))
		if got, want := m.Label(m.Sources[2]), "box via peer link, last good as of "+c.want; got != want {
			t.Errorf("%v s: label %q, want %q", c.seconds, got, want)
		}
	}
	tooOld := []struct {
		seconds float64
		want    string
	}{
		{5399, "89m ago"}, {5400, "2h ago"}, {9000, "2h ago"}, {12600, "4h ago"}, {21600, "6h ago"},
	}
	for _, c := range tooOld {
		m := standIn(PeerCurrent("box", secondsAgo(c.seconds), claude("p-one", 50, 100)))
		want := "BOX MISSING — this machine's peer link last read it " + c.want + ", too old to count"
		if got := noteTexts(m.Top())[0]; got != want {
			t.Errorf("%v s: top %q, want %q", c.seconds, got, want)
		}
	}
}

func poolOf(rows ...Row) Provider {
	m := Derive(Snapshot{Local: Answered(rows...)}, thursday0701)
	if len(m.Providers) != 1 {
		panic(fmt.Sprintf("rows made %d pools", len(m.Providers)))
	}
	return m.Providers[0]
}

func TestCeilingWrapsNegativeElapsed(t *testing.T) {
	cases := []struct {
		name  string
		pool  Provider
		hours float64
		want  float64
		ok    bool
	}{
		{"a reset over a week out", poolOf(claude("far", 50, 178)), 0, 168.0 / 158, true},
		{"the same account 20 h on", poolOf(claude("far", 50, 178)), 20, 168.0 / 10, true},
		{"two windows, one rolling over", poolOf(claude("a", 50, 68), claude("b", 50, 108)), 100, 336.0 / 192, true},
		{"a window that has not run", poolOf(claude("fresh", 50, 168)), 0, 0, false},
		{"the same window an hour on", poolOf(claude("fresh", 50, 168)), 1, 168, true},
		{"no accounts", Provider{Name: "claude"}, 0, 0, false},
	}
	for _, c := range cases {
		got, ok := c.pool.CeilingAt(c.hours)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: CeilingAt(%v) = %v, %v; want %v, %v", c.name, c.hours, got, ok, c.want, c.ok)
		}
	}
}

func TestPaceSignalChartedAndExpiring(t *testing.T) {
	pool := poolOf(claude("l-one", 50, 40), claude("l-two", 30, 100))
	if pace := fmt.Sprintf("%.4f", pool.Pace()); !pool.Signal() || pace != "0.6857" {
		t.Fatalf("signal %v, pace %s; want signal, pace 0.6857: 80%% drawn over 196 h of window", pool.Signal(), pace)
	}
	for name, p := range map[string]Provider{
		"under meter resolution": poolOf(claude("a", 2, 60), claude("b", 3, 60)),
		"under 5% of a week run": poolOf(claude("a", 90, 167.5)),
		"nothing drawn":          poolOf(claude("a", 0, 60)),
	} {
		if p.Signal() {
			t.Errorf("%s: signal, want none", name)
		}
	}

	charted := []struct {
		name string
		rows []Row
		want string
	}{
		{"most accounts", []Row{claude("c", 50, 60), codex("x", 50, 60), codex("y", 50, 60)}, "codex"},
		{"a tie", []Row{codex("x", 50, 60), claude("c", 50, 60)}, "claude"},
		{"the larger pool has no signal", []Row{claude("c", 50, 60), codex("x", 0, 60), codex("y", 0, 60)}, "claude"},
		{"no signal anywhere", []Row{claude("c", 0, 60)}, ""},
	}
	for _, c := range charted {
		p, ok := Derive(Snapshot{Local: Answered(c.rows...)}, thursday0701).Charted()
		if p.Name != c.want || ok != (c.want != "") {
			t.Errorf("%s: charted %q, %v; want %q", c.name, p.Name, ok, c.want)
		}
	}

	m := Derive(Snapshot{Local: Answered(
		codex("x-one", 50, 60), codex("x-two", 50, 60),
		claude("c-one", 50, 60), claude("c-new", 50, 166),
		claude("c-tie", 40, 60), claude("c-late", 40, 100),
	)}, thursday0701)
	var ids []string
	for _, a := range m.Expiring() {
		ids = append(ids, a.ID)
	}
	if want := []string{"c-one", "c-tie", "x-one"}; !slices.Equal(ids, want) {
		t.Fatalf("expiring = %v, want %v: claude first, ties in pool order, c-new too early, three at most", ids, want)
	}
}

func TestWeekRulesAreTheOnlyWeek(t *testing.T) {
	valid := func(provider, id string, windows ...Window) Row {
		return Row{Provider: provider, ID: id, AuthMode: "oauth", AuthChecked: true, AuthValid: true, Windows: windows}
	}
	m := Derive(Snapshot{Local: Answered(
		valid("codex", "five-hour-primary",
			Window{Name: "primary", UsedPercent: 5, LimitSeconds: 18000, ResetAfterSeconds: 900},
			Window{Name: "secondary", UsedPercent: 30, LimitSeconds: 604800, ResetAfterSeconds: 200000}),
		valid("codex", "spend", Window{Name: "spend", UsedPercent: 30, LimitSeconds: 2592000, ResetAfterSeconds: 200000}),
		valid("codex", "short-primary", Window{Name: "primary", UsedPercent: 30, LimitSeconds: 604799, ResetAfterSeconds: 200000}),
		valid("claude", "weekly", Window{Name: "weekly", UsedPercent: 30, LimitSeconds: 604800, ResetAfterSeconds: 200000}),
		valid("claude", "five-then-week",
			Window{Name: "5h", UsedPercent: 90, LimitSeconds: 18000, ResetAfterSeconds: 900},
			Window{Name: "7d", UsedPercent: 30, LimitSeconds: 604800, ResetAfterSeconds: 216000}),
		valid("claude", "no-limit", Window{Name: "7d", UsedPercent: 20, ResetAfterSeconds: 216000}),
		valid("codex", "week", Window{Name: "primary", UsedPercent: 10, LimitSeconds: 604800, ResetAfterSeconds: 216000}),
		valid("kimi", "kimi", Window{Name: "7d", UsedPercent: 10, LimitSeconds: 604800, ResetAfterSeconds: 216000}),
	)}, thursday0701)

	wantLeft := []LeftOut{
		{"codex", "five-hour-primary", LeftNoWeeklyWindow},
		{"codex", "short-primary", LeftNoWeeklyWindow},
		{"codex", "spend", LeftNoWeeklyWindow},
		{"claude", "weekly", LeftNoWeeklyWindow},
	}
	if !slices.Equal(m.LeftOut, wantLeft) {
		t.Fatalf("left out = %v, want %v", m.LeftOut, wantLeft)
	}
	wantPools := []Provider{
		{Name: "claude", Accounts: []Account{
			{ID: "five-then-week", Used: 0.3, ElapsedHours: 108, ResetHours: 60},
			{ID: "no-limit", Used: 0.2, ElapsedHours: 108, ResetHours: 60},
		}},
		{Name: "codex", Accounts: []Account{{ID: "week", Used: 0.1, ElapsedHours: 108, ResetHours: 60}}},
	}
	if fmt.Sprint(m.Providers) != fmt.Sprint(wantPools) {
		t.Fatalf("pools = %v, want %v", m.Providers, wantPools)
	}
}

func TestRowVerdicts(t *testing.T) {
	apiKey := func(r Row) Row { r.AuthMode = "apikey"; return r }
	invalid := func(r Row) Row { r.AuthValid = false; return r }
	type verdict struct {
		counted *Account
		left    LeftOutReason
		stale   int
	}
	cases := []struct {
		name string
		row  Row
		want verdict
	}{
		{"measured", claude("a", 40, 60), verdict{counted: &Account{ID: "a", Used: 0.4, ElapsedHours: 108, ResetHours: 60}}},
		{"over its limit", claude("a", 101, 60), verdict{counted: &Account{ID: "a", Used: 1.01, ElapsedHours: 108, ResetHours: 60}}},
		{"a reset over a week out", claude("a", 10, 170), verdict{counted: &Account{ID: "a", Used: 0.1, ElapsedHours: -2, ResetHours: 170}}},
		{"sweep cut, with a valid week", func() Row { r := claude("a", 40, 60); r.SweepCut = true; return r }(),
			verdict{counted: &Account{ID: "a", Used: 0.4, ElapsedHours: 108, ResetHours: 60}, stale: 1}},
		{"an API key with a valid week", apiKey(claude("a", 40, 60)), verdict{counted: &Account{ID: "a", Used: 0.4, ElapsedHours: 108, ResetHours: 60}}},
		{"no ID", claude("", 40, 60), verdict{}},
		{"no ID, not valid", invalid(claude("", 40, 60)), verdict{}},
		{"an API key, not valid", apiKey(invalid(claude("a", 40, 60))), verdict{}},
		{"an API key with no week", apiKey(Row{Provider: "codex", ID: "a", AuthChecked: true, AuthValid: true}), verdict{}},
		{"a provider with no week", Row{Provider: "kimi", ID: "a", SweepCut: true}, verdict{}},
		{"sweep cut", cutRow("claude", "a"), verdict{left: LeftUnmeasured}},
		{"not checked", Row{Provider: "claude", ID: "a", AuthValid: true}, verdict{left: LeftUnmeasured}},
		{"context canceled", Row{Provider: "claude", ID: "a", AuthChecked: true, Error: "usage: context canceled"}, verdict{left: LeftUnmeasured}},
		{"context deadline exceeded", Row{Provider: "claude", ID: "a", AuthChecked: true, Error: "context deadline exceeded"}, verdict{left: LeftUnmeasured}},
		{"auth invalid", invalid(claude("a", 40, 60)), verdict{left: LeftAuthInvalid}},
		{"checked, valid, no week", Row{Provider: "claude", ID: "a", AuthChecked: true, AuthValid: true}, verdict{left: LeftNoWeeklyWindow}},
		{"reset 0 on a direct read", claude("a", 40, 0), verdict{left: LeftRolled}},
		{"an API key that rolled", apiKey(claude("a", 40, 0)), verdict{left: LeftRolled}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := Derive(Snapshot{Local: Answered(c.row)}, thursday0701)
			var got verdict
			if len(m.Providers) > 0 {
				got.counted = &m.Providers[0].Accounts[0]
			}
			if len(m.LeftOut) > 0 {
				got.left = m.LeftOut[0].Why
			}
			got.stale = m.Stale
			if fmt.Sprint(got.counted) != fmt.Sprint(c.want.counted) || got.left != c.want.left || got.stale != c.want.stale ||
				m.Accounts()+len(m.LeftOut) > 1 {
				t.Fatalf("got counted %v, left %v, stale %d (pools %v, left %v); want counted %v, left %v, stale %d",
					got.counted, got.left, got.stale, m.Providers, m.LeftOut, c.want.counted, c.want.left, c.want.stale)
			}
		})
	}
}

func TestRowVerdictsMergeAcrossScopes(t *testing.T) {
	cut := func(r Row) Row { r.SweepCut = true; return r }
	m := Derive(Snapshot{
		Local: Answered(
			claude("a", 40, 60),
			claude("dup", 10, 50),
			claude("fresh", 30, 80),
			cutRow("claude", "u"),
			cutRow("claude", "gone"),
			Row{Provider: "claude", ID: "cut-2", AuthMode: "oauth", AuthChecked: true},
			Row{Provider: "claude", ID: "cut-10", AuthMode: "oauth", AuthChecked: true},
			cutRow("claude", "cut-10"),
		),
		Boxes: home(Answered(
			claude("a", 40, 60),
			claude("dup", 15, 50),
			claude("u", 20, 70),
			cut(claude("fresh", 30, 80)),
			codex("a", 10, 90),
		)),
		Peers: PeersAnswered(PeerCurrent("box", secondsAgo(5), claude("a", 40, 62), cut(claude("dup", 10, 0.001)))),
	}, thursday0701)

	if m.Folded != 4 {
		t.Errorf("folded = %d, want 4: a and dup and fresh on home, a on the peer", m.Folded)
	}
	if want := []string{"dup", "a"}; !slices.Equal(m.Conflicts, want) {
		t.Errorf("conflicts = %v, want %v: dup's use differs by 5 points, the peer's a resets 2 h later", m.Conflicts, want)
	}
	if m.Stale != 2 {
		t.Errorf("stale = %d, want 2: home's copy of fresh and the peer's rolled copy of dup were sweep-cut, though local's copies are kept", m.Stale)
	}
	wantLeft := []LeftOut{
		{"claude", "cut-10", LeftAuthInvalid},
		{"claude", "cut-2", LeftAuthInvalid},
		{"claude", "gone", LeftUnmeasured},
	}
	if !slices.Equal(m.LeftOut, wantLeft) {
		t.Errorf("left out = %v, want %v: sorted as strings, first reason wins, u was measured on home", m.LeftOut, wantLeft)
	}
	local, box, peer := m.Sources[0], m.Sources[1], m.Sources[2]
	if local.Measured != 3 || local.Unmeasured != 2 || box.Measured != 5 || peer.Measured != 1 {
		t.Errorf("measured/unmeasured: local %d/%d, home %d/%d, peer %d/%d; want 3/2 (gone and cut-10), 5/0, 1/0",
			local.Measured, local.Unmeasured, box.Measured, box.Unmeasured, peer.Measured, peer.Unmeasured)
	}
	if got := m.Providers[0].Accounts[0]; got != (Account{ID: "a", Used: 0.4, ElapsedHours: 108, ResetHours: 60}) {
		t.Errorf("a = %+v, want local's copy: the first copy is kept", got)
	}
	if m.Accounts() != 5 {
		t.Errorf("accounts = %d, want 5: claude a, dup, fresh and u, and codex a", m.Accounts())
	}
}

func TestConfigNotes(t *testing.T) {
	m := Derive(Snapshot{Local: Answered(), Config: []ConfigNote{
		ServersSwitchedOff(),
		ServersFileUnreadable("not valid JSON"),
		RemoteKeyRefused(KeyNotRegularFile, "/state/codex/remote.key", ""),
		RemoteKeyRefused(KeyModeTooOpen, "/state/codex/remote.key", "0644"),
		RemoteKeyRefused(KeyUnreadable, "/state/codex/remote.key", "permission denied"),
		RemoteKeyRefused(KeyTooShort, "/state/codex/remote.key", ""),
		{},
	}}, thursday0701)
	requireNotes(t, "top", m.Top(), []Note{
		{Dim, "SR_STATUS_SERVERS=0: local only; the box and the daemon's peer view were not read"},
		{Amber, "servers.json unreadable (not valid JSON); its boxes were not read"},
		{Amber, "/state/codex/remote.key is not a regular file; the box was not read"},
		{Amber, "/state/codex/remote.key is mode 0644; chmod 600 it to read the box"},
		{Amber, "cannot read /state/codex/remote.key (permission denied); the box was not read"},
		{Amber, "/state/codex/remote.key is too short to be a key; the box was not read"},
	})
}

func TestHedgeWording(t *testing.T) {
	cases := []struct {
		golden      string
		top, bottom []Note
	}{
		{
			golden: "03b-b-last-good",
			top: []Note{
				{Amber, "box.example is a last-good reading — this machine's daemon cannot reach it now (dial tcp 203.0.113.7:31416: i/o timeout)"},
				{Amber, "HOME NOT READ (No route to host) — box.example (peer link) is in the numbers; if that is home, nothing is missing"},
			},
			bottom: []Note{{Dim, "home's servers.json address is down (No route to host); a peer link stood in, see above"}},
		},
		{
			golden: "04b-a-bodiless",
			top: []Note{
				{Amber, "peer link to home has no reading (dial tcp: connection refused) — unless it is home, its accounts are not in the numbers below"},
			},
		},
		{
			golden: "04c-two-boxes-one-peer",
			top: []Note{
				{Amber, "AT LEAST 1 OF HOME, WORK MISSING — none was read at its servers.json address (home: No route to host; work: No route to host); boxpeer (peer link) is in the numbers and may be one of them"},
			},
			bottom: []Note{
				{Amber, "home unreachable — No route to host"},
				{Amber, "work unreachable — No route to host"},
			},
		},
		{
			golden: "08-nothing-read",
			top: []Note{
				{Amber, "LOCAL MISSING — this machine's accounts are not in the numbers below (timeout after 65s)"},
				{Amber, "HOME MISSING — home's accounts are not in the numbers below (Connection refused)"},
				{Dim, "no fallback through this machine's daemon: timeout after 15s"},
			},
			bottom: []Note{
				{Amber, "local unreachable — timeout after 65s"},
				{Amber, "home unreachable — Connection refused"},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.golden, func(t *testing.T) {
			m := Derive(pageNamed(t, c.golden).snapshot, thursday0701)
			requireNotes(t, "top", m.Top(), c.top)
			requireNotes(t, "bottom", m.Bottom(), c.bottom)
		})
	}

	twoStandIns := func(boxes ...string) Model {
		var bs []Box
		for _, b := range boxes {
			bs = append(bs, Box{Name: b, Reading: Unread("No route to host")})
		}
		return Derive(Snapshot{
			Local: Answered(),
			Boxes: bs,
			Peers: PeersAnswered(PeerCurrent("p1", secondsAgo(5)), PeerCurrent("p2", secondsAgo(5))),
		}, thursday0701)
	}
	t.Run("two stand-ins for one box", func(t *testing.T) {
		requireNotes(t, "top", twoStandIns("home").Top(), []Note{
			{Amber, "HOME NOT READ (No route to host) — p1 (peer link) and p2 (peer link) are in the numbers; if that is home, nothing is missing"},
		})
	})
	t.Run("two stand-ins for three boxes", func(t *testing.T) {
		requireNotes(t, "top", twoStandIns("home", "work", "lab").Top(), []Note{
			{Amber, "AT LEAST 1 OF HOME, WORK, LAB MISSING — none was read at its servers.json address (home: No route to host; work: No route to host; lab: No route to host); p1 (peer link) and p2 (peer link) are in the numbers and may be some of them"},
		})
	})
	t.Run("a bodiless peer with no box read", func(t *testing.T) {
		m := Derive(Snapshot{Local: Answered(), Peers: PeersAnswered(PeerBodiless("box", ""))}, thursday0701)
		requireNotes(t, "top", m.Top(), []Note{
			{Amber, "BOX MISSING — this machine's peer link to it has no reading, so its accounts are not in the numbers below (no reading yet)"},
		})
	})
	t.Run("a wholly unmeasured peer", func(t *testing.T) {
		m := Derive(Snapshot{Local: Answered(), Peers: PeersAnswered(PeerCurrent("box", secondsAgo(5), cutRow("claude", "a")))}, thursday0701)
		requireNotes(t, "top", m.Top(), []Note{
			{Amber, "BOX (PEER LINK) UNMEASURED — none of box (peer link)'s 1 accounts has a weekly reading this read (sweep cut, or not yet checked), so they are not in the numbers below"},
		})
	})
}

var squeeze = strings.NewReplacer(" ", "", "\n", "")

// TestTheModelSaysWhatEveryCapturedPageSays checks the model's facts against
// each captured page, ignoring layout: wrapping only ever moves text across
// lines, so text with its spaces removed is the same either way.
func TestTheModelSaysWhatEveryCapturedPageSays(t *testing.T) {
	expiringRow := regexp.MustCompile(`^  (\S+) +(-?\d+)%  in +(\d+\.\d)h$`)
	for _, p := range pages() {
		t.Run(p.golden, func(t *testing.T) {
			m := Derive(p.snapshot, thursday0701)
			lines := strings.Split(strings.TrimSuffix(goldenText(t, p), "\n"), "\n")
			rule := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), "─") })
			blank := rule + 1 + slices.Index(lines[rule+1:], "")
			body := strings.Join(lines[1:len(lines)-1], "\n")

			headline := squeeze.Replace(strings.Join(lines[1:rule], "\n"))
			for _, s := range m.Scopes() {
				label := m.Label(s)
				if s.Unmeasured > 0 {
					label += fmt.Sprintf(" (%d of %d measured)", s.Measured, s.Measured+s.Unmeasured)
				}
				if !strings.Contains(headline, squeeze.Replace(label)) {
					t.Errorf("headline %q lacks %q", headline, label)
				}
			}

			if got, want := squeeze.Replace(strings.Join(noteTexts(m.Top()), "")),
				squeeze.Replace(strings.Join(lines[rule+1:blank], "")); got != want {
				t.Errorf("top notes:\n got  %s\n want %s", got, want)
			}

			if p.outcome == NothingRead {
				return
			}
			bottom := squeeze.Replace(strings.Join(noteTexts(m.Bottom()), ""))
			rest, found := strings.CutSuffix(squeeze.Replace(body), bottom)
			if !found {
				t.Fatalf("the page does not end with the bottom notes %q", m.Bottom())
			}
			if p.outcome == NothingMeasured {
				if last := m.LeftOut[len(m.LeftOut)-1].ID; !strings.HasSuffix(rest, last) {
					t.Errorf("the page has a bottom line the model lacks, after %q", last)
				}
				return
			}

			if !strings.Contains(body, fmt.Sprintf(" %d accounts", m.Accounts())) {
				t.Errorf("the page does not count %d accounts", m.Accounts())
			}
			for _, prov := range m.Providers {
				ceiling, _ := prov.CeilingAt(0)
				row := fmt.Sprintf("  %-8s%5.2f× ", prov.Name, prov.Pace())
				if prov.Signal() && (!strings.Contains(body, row) || !strings.Contains(body, fmt.Sprintf("  ceil %.2f×", ceiling))) {
					t.Errorf("the page lacks %s's pace %.2f× and ceiling %.2f×", prov.Name, prov.Pace(), ceiling)
				}
			}
			if len(m.LeftOut) > 0 {
				var ids []string
				for _, l := range m.LeftOut {
					ids = append(ids, l.ID)
				}
				if !strings.Contains(squeeze.Replace(body), squeeze.Replace("— not in the numbers: "+strings.Join(ids, ", "))) {
					t.Errorf("the page does not leave out %v", ids)
				}
			}
			start := slices.Index(lines, "  unspent · expiring soonest")
			if start < 0 {
				t.Fatal("the page has no expiring list")
			}
			for i, a := range m.Expiring() {
				got := expiringRow.FindStringSubmatch(lines[start+1+i])
				want := []string{a.ID, strconv.FormatFloat((1-a.Used)*100, 'f', 0, 64), strconv.FormatFloat(a.ResetHours, 'f', 1, 64)}
				if got == nil || !slices.Equal(got[1:], want) {
					t.Errorf("expiring row %d = %q, want %v", i, lines[start+1+i], want)
				}
			}
			if !strings.HasSuffix(rest, squeeze.Replace(lines[start+len(m.Expiring())])) {
				t.Errorf("the page has a bottom line the model lacks, after the expiring list")
			}
		})
	}
}
