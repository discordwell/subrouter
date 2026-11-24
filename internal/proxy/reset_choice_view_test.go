package proxy

import (
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/discordwell/subrouter/selectacct"
)

// PATCHES §95.3 on the board: pool.json and the board page show which
// accounts are chosen for a reset, this daemon's and a peer's. Every provider
// is a fake that fails the test on a claim or a redeem.

// choiceViewPool is a box in real mode that has chosen its held, and a laptop
// in shadow mode that reads the box and holds mine, with a week reset, and
// plain, with none. boxChoice is the box's published choice for held.
func choiceViewPool(t *testing.T) (laptop *autoResetClaude, boxChoice *selectacct.ResetChoice) {
	t.Helper()
	box := choiceClaude(t, AutoResetReal, true, nil, nil)
	box.pass()
	rec := httptest.NewRecorder()
	box.server.handleUsageStatus(rec, loopbackRequest(http.MethodGet, remotePoolUsagePath, ""))
	if _, boxChoice = published(t, usageRows(t, box.server)["held"]); boxChoice == nil {
		t.Fatal("calibration: the box does not choose held")
	}
	laptop = choiceClaude(t, AutoResetShadow, true, map[string]float64{"mine": 1.2, "plain": 1.2}, map[string]*autoResetProfile{
		"mine":  {week: 60, fable: 20, block: cedarBlock(autoResetWeekGrant("week_reset", 1, false)), roll: 100 * time.Hour},
		"plain": {week: 40, fable: 10, roll: 100 * time.Hour},
	})
	laptop.server.RemotePool = NewRemotePool(servedPeer(t, rec.Body.String()), "", "")
	laptop.pass()
	return laptop, boxChoice
}

// mineAsPublished is what the laptop's usage-status says of mine: would_choose
// in shadow mode, reset_choice inside normalized in real mode.
func mineAsPublished(t *testing.T, laptop *autoResetClaude) (would, inside *selectacct.ResetChoice) {
	t.Helper()
	return published(t, usageRows(t, laptop.server)["mine"])
}

// 4. pool.json carries each row's choice as its holder published it: in real
// mode as reset_choice, in shadow mode as would_choose, and nothing of this
// daemon's own with the mode off or SUBROUTER_RESET_LOADING=0. A peer's row
// carries the peer's own.
func TestPoolJSONCarriesEachRowsResetChoice(t *testing.T) {
	laptop, boxChoice := choiceViewPool(t)
	want := func(t *testing.T, what string, rows map[string]poolRow, id string, choice, would *selectacct.ResetChoice) {
		t.Helper()
		row, ok := rows[id]
		if !ok {
			t.Fatalf("%s: pool.json has no row for %s", what, id)
		}
		if !reflect.DeepEqual(row.ResetChoice, choice) || !reflect.DeepEqual(row.WouldChoose, would) {
			t.Fatalf("%s: %s carries reset_choice %+v and would_choose %+v, want %+v and %+v", what, id, row.ResetChoice, row.WouldChoose, choice, would)
		}
	}
	mine, _ := mineAsPublished(t, laptop)
	if mine == nil || mine.Meter != selectacct.ResetChoiceWeek || mine.Order != 1 {
		t.Fatalf("calibration: in shadow mode the laptop would choose mine as %+v, want the week, order 1", mine)
	}
	shadow := poolRowsOf(t, laptop.server)
	want(t, "shadow", shadow, "mine", nil, mine)
	want(t, "shadow", shadow, "plain", nil, nil)
	want(t, "shadow", shadow, "held", boxChoice, nil)
	want(t, "shadow", shadow, "spare", nil, nil)
	if pools := shadow["held"].Pools; !reflect.DeepEqual(pools, []string{poolNameBox}) {
		t.Fatalf("calibration: the box's held is on %v, want the box alone", pools)
	}

	setAutoResetForTest(t, AutoResetReal, true)
	laptop.pass()
	_, chosen := mineAsPublished(t, laptop)
	if chosen == nil {
		t.Fatal("calibration: in real mode the laptop does not choose mine")
	}
	real := poolRowsOf(t, laptop.server)
	want(t, "real", real, "mine", chosen, nil)
	want(t, "real", real, "plain", nil, nil)
	want(t, "real", real, "held", boxChoice, nil)

	setAutoResetForTest(t, AutoResetOff, true)
	off := poolRowsOf(t, laptop.server)
	want(t, "off", off, "mine", nil, nil)
	want(t, "off, the peer's own choice", off, "held", boxChoice, nil)

	setAutoResetForTest(t, AutoResetReal, true)
	setResetLoadingForTest(t, false)
	want(t, "real with SUBROUTER_RESET_LOADING=0", poolRowsOf(t, laptop.server), "mine", nil, nil)

	// A peer's choice this build cannot rank on, a meter it does not know, is
	// not shown as a choice either.
	box := choiceClaude(t, AutoResetReal, true, nil, nil)
	box.pass()
	rows := usageRows(t, box.server)
	held := rows["held"]
	var normalized map[string]any
	if err := json.Unmarshal(held["normalized"], &normalized); err != nil {
		t.Fatal(err)
	}
	normalized["reset_choice"] = map[string]any{"meter": "month", "order": 1, "why": "a later build's meter"}
	held["normalized"], _ = json.Marshal(normalized)
	body, _ := json.Marshal([]map[string]json.RawMessage{held, rows["spare"]})
	laptop.server.RemotePool = NewRemotePool(servedPeer(t, string(body)), "", "")
	want(t, "a peer's choice of a meter this build does not know", poolRowsOf(t, laptop.server), "held", nil, nil)
}

// 3. The board page the daemon serves, run by node on the pool.json it serves,
// marks a chosen account on its row with its meter and order, and a would-be
// choice in shadow mode in other words, each with its reason in the title; a
// peer's account the peer chose is marked as the peer published it, and an
// account no daemon chose is not marked. Without node, see nodeForTest.
func TestTheBoardPageMarksAChosenAccount(t *testing.T) {
	node := nodeForTest(t)
	laptop, boxChoice := choiceViewPool(t)
	mine, _ := mineAsPublished(t, laptop)
	if mine == nil {
		t.Fatal("calibration: in shadow mode the laptop would not choose mine")
	}
	drawn := boardRowsByName(t, laptop.server, node)
	wantMark(t, "shadow, this daemon's own", drawn["mine"], "wouldchoose", "would choose week #1", mine.Why)
	wantMark(t, "shadow, the peer's", drawn["held"], "chosen", "chosen week #1", boxChoice.Why)
	for _, id := range []string{"plain", "spare"} {
		if row, ok := drawn[id]; !ok || strings.Contains(row, "choose") || strings.Contains(row, `class="chosen"`) {
			t.Fatalf("the page marks %s, which no daemon chose, or draws no row for it:\n%s", id, row)
		}
	}

	setAutoResetForTest(t, AutoResetReal, true)
	laptop.pass()
	_, chosen := mineAsPublished(t, laptop)
	if chosen == nil {
		t.Fatal("calibration: in real mode the laptop does not choose mine")
	}
	wantMark(t, "real, this daemon's own", boardRowsByName(t, laptop.server, node)["mine"], "chosen", "chosen week #1", chosen.Why)
}

// boardRowsByName is renderPoolPage's rows keyed by the account's name.
func boardRowsByName(t *testing.T, server Server, node string) map[string]string {
	t.Helper()
	rows := map[string]string{}
	for title, row := range renderPoolPage(t, server, node) {
		rows[strings.TrimSuffix(title, "@example.com")] = row
	}
	return rows
}

// wantMark fails unless row holds one mark of class reading text, whose title
// holds why.
func wantMark(t *testing.T, what, row, class, text, why string) {
	t.Helper()
	mark := `<span class="` + class + `" title="`
	at := strings.Index(row, mark)
	if at < 0 || !strings.Contains(row[at:], `">`+text+`</span>`) {
		t.Fatalf("%s: the page drew\n%s\nwant a %q mark %q", what, row, class, text)
	}
	title, _, _ := strings.Cut(row[at+len(mark):], `"`)
	if !strings.Contains(html.UnescapeString(title), why) {
		t.Fatalf("%s: the mark's title is %q, want it to hold the reason %q", what, title, why)
	}
}
