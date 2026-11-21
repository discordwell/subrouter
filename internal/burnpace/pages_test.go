package burnpace

import (
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// thursday0701 is the clock of every captured page: only "Thu 07:01" and the
// day labels depend on it.
var thursday0701 = time.Date(2026, 11, 19, 7, 1, 0, 0, time.FixedZone("page", -6*3600))

func secondsAgo(seconds float64) time.Time {
	return thursday0701.Add(-time.Duration(seconds * float64(time.Second)))
}

// claude and codex are a measured oauth row with one weekly window, as the
// reference script's fixtures build them.
func claude(id string, usedPercent, resetHours float64) Row {
	return weekly("claude", "7d", id, usedPercent, resetHours)
}

func codex(id string, usedPercent, resetHours float64) Row {
	return weekly("codex", "primary", id, usedPercent, resetHours)
}

func weekly(provider, window, id string, usedPercent, resetHours float64) Row {
	return Row{
		Provider: provider, ID: id, AuthMode: "oauth", AuthChecked: true, AuthValid: true,
		Windows: []Window{{Name: window, UsedPercent: usedPercent, LimitSeconds: 604800,
			ResetAfterSeconds: int64(math.Round(resetHours * 3600))}},
	}
}

func localRows() []Row {
	return []Row{claude("l-one", 50, 40), claude("l-two", 30, 100), codex("shared-codex", 10, 90)}
}

func boxRows() []Row {
	return []Row{claude("b-one", 60, 30), claude("b-two", 20, 120), codex("shared-codex", 10, 90)}
}

// cutRow is a row the daemon's sweep never reached: unchecked, not valid, no
// windows.
func cutRow(provider, id string) Row {
	return Row{Provider: provider, ID: id, AuthMode: "oauth", SweepCut: true}
}

func home(r Reading) []Box { return []Box{{Name: "home", Reading: r}} }

const ioTimeout = "dial tcp 203.0.113.7:31416: i/o timeout"

// page is one drawn page of the reference script's fixture run, and the
// snapshot that draws it.
type page struct {
	// golden is the file under testdata/ref/, cut from the captured run.
	golden   string
	snapshot Snapshot
	outcome  Outcome
	// standings is the Standing of each source, in Model.Sources order.
	standings []Standing
	// dropped are lines of the golden this implementation does not print on
	// purpose. They are removed from the expected text before comparing, so the
	// golden stays as captured.
	dropped []string
}

// pages is every captured page. Peer ages are whole seconds; the captured run
// stamped peers to the second and read its clock a fraction of a second later.
func pages() []page {
	noRoute := Unread("No route to host")
	refused := Unread("Connection refused")
	allCut := make([]Row, 12)
	for i := range allCut {
		allCut[i] = cutRow("claude", fmt.Sprintf("cut-%d", i))
	}
	boxPeer := func(seconds float64) PeerView {
		return PeersAnswered(PeerCurrent("box.example", secondsAgo(seconds), boxRows()...))
	}
	sameBoxPeer := PeersAnswered(PeerCurrent("box.example", secondsAgo(5), boxRows()...).SameBoxAs("home"))

	return []page{
		{
			golden: "01-local-failed",
			snapshot: Snapshot{
				Local: Unread("timeout after 65s"),
				Boxes: home(Answered(boxRows()...)),
				Peers: PeersFailed("timeout after 15s"),
			},
			outcome:   Numbers,
			standings: []Standing{Missing, Counted},
		},
		{
			golden: "02-cold-local",
			snapshot: Snapshot{
				Local: Answered(localRows()...),
				Boxes: home(Answered(boxRows()...)),
				Peers: PeersAbsent(),
			},
			outcome:   Numbers,
			standings: []Standing{Counted, Counted},
		},
		{
			golden:    "03-box-through-peer-view",
			snapshot:  Snapshot{Local: Answered(localRows()...), Boxes: home(noRoute), Peers: boxPeer(5)},
			outcome:   Numbers,
			standings: []Standing{Counted, Hedged, Counted},
		},
		{
			golden:    "03b-a-dated",
			snapshot:  Snapshot{Local: Answered(localRows()...), Boxes: home(noRoute), Peers: boxPeer(300)},
			outcome:   Numbers,
			standings: []Standing{Counted, Hedged, Counted},
		},
		{
			golden: "03b-b-last-good",
			snapshot: Snapshot{
				Local: Answered(localRows()...),
				Boxes: home(noRoute),
				Peers: PeersAnswered(PeerLastGood("box.example", secondsAgo(600), ioTimeout, boxRows()...)),
			},
			outcome:   Numbers,
			standings: []Standing{Counted, Hedged, Counted},
		},
		{
			golden: "03c-too-old",
			snapshot: Snapshot{
				Local: Answered(localRows()...),
				Boxes: home(noRoute),
				Peers: PeersAnswered(PeerLastGood("box.example", secondsAgo(6*3600), ioTimeout,
					claude("b-one", 95, 1), claude("b-two", 90, 2), codex("shared-codex", 10, 96))),
			},
			outcome:   Numbers,
			standings: []Standing{Counted, Missing, TooOld},
		},
		{
			golden: "03d-rebased",
			snapshot: Snapshot{
				Local: Answered(localRows()...),
				Boxes: home(noRoute),
				Peers: PeersAnswered(PeerCurrent("box.example", secondsAgo(600),
					claude("b-one", 60, 0.1), claude("b-two", 20, 3))),
			},
			outcome:   Numbers,
			standings: []Standing{Counted, Hedged, Counted},
		},
		{
			golden: "04-other-box-counts",
			snapshot: Snapshot{
				Local: Answered(localRows()...),
				Boxes: home(Answered(boxRows()...)),
				Peers: PeersAnswered(
					PeerCurrent("box.example", secondsAgo(5), boxRows()[:2]...).SameBoxAs("home"),
					PeerCurrent("other-box", secondsAgo(5), claude("o-one", 70, 20), claude("b-two", 20, 120)),
				),
			},
			outcome:   Numbers,
			standings: []Standing{Counted, Counted, SameBox, Counted},
		},
		{
			golden: "04b-a-bodiless",
			snapshot: Snapshot{
				Local: Answered(localRows()...),
				Boxes: home(Answered(boxRows()...)),
				Peers: PeersAnswered(PeerBodiless("home", "dial tcp: connection refused")),
			},
			outcome:   Numbers,
			standings: []Standing{Counted, Counted, NoReading},
		},
		{
			golden: "04b-b-own-scope",
			snapshot: Snapshot{
				Local: Answered(localRows()...),
				Boxes: home(Answered(boxRows()...)),
				Peers: PeersAnswered(PeerCurrent("home", secondsAgo(5), claude("h-extra", 30, 50))),
			},
			outcome:   Numbers,
			standings: []Standing{Counted, Counted, Counted},
		},
		{
			golden: "04c-two-boxes-one-peer",
			snapshot: Snapshot{
				Local: Answered(localRows()...),
				Boxes: []Box{{Name: "home", Reading: noRoute}, {Name: "work", Reading: noRoute}},
				Peers: PeersAnswered(PeerCurrent("boxpeer", secondsAgo(5), boxRows()...)),
			},
			outcome:   Numbers,
			standings: []Standing{Counted, Outnumbered, Outnumbered, Counted},
		},
		{
			golden: "05-unmeasured-named",
			snapshot: Snapshot{
				Local: Answered(
					claude("l-one", 50, 40),
					claude("l-two", 30, 100),
					cutRow("claude", "cut-new"),
					Row{Provider: "claude", ID: "cut-old", AuthMode: "oauth", AuthChecked: true, Error: "context deadline exceeded"},
					Row{Provider: "claude", ID: "dead-one", AuthMode: "oauth", AuthChecked: true, Error: "invalid_grant"},
					Row{Provider: "codex", ID: "key-row", AuthMode: "apikey"},
					Row{Provider: "kimi", ID: "kimi", SweepCut: true},
					cutRow("codex", "shared-codex"),
				),
				Boxes: home(Answered(boxRows()...)),
				Peers: PeersAbsent(),
			},
			outcome:   Numbers,
			standings: []Standing{Counted, Counted},
		},
		{
			golden: "05b-no-weekly-window",
			snapshot: Snapshot{
				Local: Answered(append(localRows(), Row{
					Provider: "codex", ID: "codex-5h", AuthMode: "oauth", AuthChecked: true, AuthValid: true,
					Windows: []Window{
						{Name: "primary", UsedPercent: 5, LimitSeconds: 18000, ResetAfterSeconds: 900},
						{Name: "secondary", UsedPercent: 30, LimitSeconds: 604800, ResetAfterSeconds: 200000},
					},
				})...),
				Boxes: home(Answered(boxRows()...)),
				Peers: PeersAbsent(),
			},
			outcome:   Numbers,
			standings: []Standing{Counted, Counted},
		},
		{
			golden: "05c-a-local-unmeasured",
			snapshot: Snapshot{
				Local: Answered(allCut...),
				Boxes: home(Answered(boxRows()[:2]...)),
				Peers: PeersAbsent(),
			},
			outcome:   Numbers,
			standings: []Standing{AnsweredUnmeasured, Counted},
		},
		{
			golden:    "05c-b-nothing-measured",
			snapshot:  Snapshot{Local: Answered(allCut...), Boxes: home(noRoute), Peers: PeersAbsent()},
			outcome:   NothingMeasured,
			standings: []Standing{AnsweredUnmeasured, Missing},
		},
		{
			golden:    "06-peer-view-absent",
			snapshot:  Snapshot{Local: Answered(localRows()...), Boxes: home(refused), Peers: PeersAbsent()},
			outcome:   Numbers,
			standings: []Standing{Counted, Missing},
		},
		{
			golden:    "07-no-fallback",
			snapshot:  Snapshot{Local: Answered(localRows()...), Boxes: home(refused), Peers: PeersFailed("HTTP 500")},
			outcome:   Numbers,
			standings: []Standing{Counted, Missing},
		},
		{
			golden: "08-nothing-read",
			snapshot: Snapshot{
				Local: Unread("timeout after 65s"),
				Boxes: home(refused),
				Peers: PeersFailed("timeout after 15s"),
			},
			outcome:   NothingRead,
			standings: []Standing{Missing, Missing},
		},
		{
			golden: "09-reasons",
			snapshot: Snapshot{
				Local: Answered(localRows()...),
				Boxes: []Box{{Name: "home", Reading: Unread("ValueError")}, {Name: "lab", Reading: Unread("InvalidURL")}},
				Peers: PeersAbsent(),
			},
			outcome:   Numbers,
			standings: []Standing{Counted, Missing, Missing},
		},
		{
			golden: "10-redirect",
			snapshot: Snapshot{
				Local: Answered(localRows()...),
				Boxes: home(Unread("redirected to another address")),
				Peers: PeersAbsent(),
			},
			outcome:   Numbers,
			standings: []Standing{Counted, Missing},
		},
		{
			golden: "11c-built-in-name",
			snapshot: Snapshot{
				Local: Answered(localRows()...),
				Boxes: home(Answered(boxRows()...)),
				Peers: sameBoxPeer,
			},
			outcome:   Numbers,
			standings: []Standing{Counted, Counted, SameBox},
		},
		{
			golden: "11d-next-entry-read",
			snapshot: Snapshot{
				Local: Answered(localRows()...),
				Boxes: []Box{
					{Name: "home", Reading: Answered(boxRows()...)},
					{Name: "work", Reading: Answered(claude("w-one", 10, 50))},
				},
				Peers: sameBoxPeer,
			},
			outcome:   Numbers,
			standings: []Standing{Counted, Counted, Counted, SameBox},
			dropped:   []string{"servers.json entry #2 unreadable (KeyError); not read"},
		},
	}
}

func pageNamed(t *testing.T, golden string) page {
	t.Helper()
	for _, p := range pages() {
		if p.golden == golden {
			return p
		}
	}
	t.Fatalf("no page %q", golden)
	return page{}
}

// goldenText is a captured page with its dropped lines removed.
func goldenText(t *testing.T, p page) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/ref/" + p.golden + ".txt")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(raw), "\n")
	kept := lines[:0]
	for _, line := range lines {
		dropped := false
		for _, d := range p.dropped {
			dropped = dropped || strings.TrimSpace(line) == d
		}
		if !dropped {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "")
}

// pagesFrame is the frame every captured page was drawn at.
var pagesFrame = Page{Cols: 100, Lines: 40}

func render(t *testing.T, m Model, p Page) string {
	t.Helper()
	var page strings.Builder
	if err := Render(&page, m, p); err != nil {
		t.Fatal(err)
	}
	return page.String()
}

// requirePage fails with the first differing line and both whole pages.
func requirePage(t *testing.T, got, want string) {
	t.Helper()
	if got == want {
		return
	}
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	line := 0
	for line < min(len(g), len(w)) && g[line] == w[line] {
		line++
	}
	t.Fatalf("first difference at line %d\n--- want\n%s--- got\n%s", line+1, want, got)
}

func TestRenderMatchesTheCapturedPages(t *testing.T) {
	for _, p := range pages() {
		t.Run(p.golden, func(t *testing.T) {
			m := Derive(p.snapshot, thursday0701)
			requirePage(t, render(t, m, pagesFrame), goldenText(t, p))
			if m.Outcome() != p.outcome || m.Drawn() != (p.outcome == Numbers) {
				t.Fatalf("outcome %v, drawn %v; want %v", m.Outcome(), m.Drawn(), p.outcome)
			}
		})
	}
}
