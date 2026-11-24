package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/discordwell/subrouter/selectacct"
)

// PATCHES §104: a daemon that ranks the peers' accounts alone -- its own pool
// cannot serve the turn, the laptop's usual case -- ranks an Opus or Sonnet
// turn on each account's published Fable meter, as its own selection does
// (§65). Before §104 that ranking never learned the turn's model, so the
// Fable-spent preference applied to none of the laptop's placements there.

// fableSpareBox is a peer with two accounts whose published numbers are the
// same but for the Fable meter. "a-untouched" sorts first, so the terminal ID
// tie-break hands the pair to the account whose Fable is NOT spent: only the
// preference can put b-spent first.
func fableSpareBox(t *testing.T) *contractBox {
	t.Helper()
	untouched := crossPoolClaudeScore("a-untouched", 0.5, 72*time.Hour)
	spent := crossPoolClaudeScore("b-spent", 0.5, 72*time.Hour)
	spent.FableSpent = 1
	return newContractBox(t, boxAccount{Score: untouched}, boxAccount{Score: spent})
}

// modelTurn posts one Claude turn for model and returns the status and the
// account that served it.
func (l *crossPoolLaptop) modelTurn(t *testing.T, sessionID, model string) (int, string) {
	t.Helper()
	subrouter := httptest.NewServer(l.server.Handler())
	t.Cleanup(subrouter.Close)
	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/messages", strings.NewReader(`{"model":"`+model+`","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", sessionID)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	served := ""
	if _, rest, ok := strings.Cut(string(body), `"served_by":"`); ok {
		served, _, _ = strings.Cut(rest, `"`)
	}
	return response.StatusCode, served
}

// placedBy is the clause of the decision line that placed sessionID on a
// peer's account with local unable to serve.
func (l *crossPoolLaptop) placedBy(t *testing.T, sessionID string) string {
	t.Helper()
	for _, d := range l.logLines("remote pool decision") {
		reason, _ := d["reason"].(string)
		if d["session"] == sessionID && strings.HasPrefix(reason, "local cannot serve") {
			clause, _ := d["clause"].(string)
			return clause
		}
	}
	t.Fatalf("no local-cannot-serve decision line for %s: %v", sessionID, l.logLines("remote pool decision"))
	return ""
}

const reasonTiedByID = "tied; ordered by account id"

// The bug: a laptop with no account of its own that can serve placed an Opus
// or Sonnet session on the peer's account with Fable left, by the ID
// tie-break, though the other account's Fable meter was spent and its week
// the same.
func TestAnOpusTurnRankedAcrossPeersAloneGoesToTheFableSpentAccount(t *testing.T) {
	setFableSpentPreferenceForTest(t, true)
	for _, model := range []string{"claude-opus-5-5", "claude-sonnet-5"} {
		t.Run(model, func(t *testing.T) {
			box := fableSpareBox(t)
			laptop := newCrossPoolLaptop(t, nil, box.pool)
			status, served := laptop.modelTurn(t, "s-"+model, model)
			if status != http.StatusOK || served != "b-spent" {
				t.Fatalf("status %d served by %q, box turns %+v; want b-spent, whose Fable meter is spent", status, served, box.seen())
			}
			if clause := laptop.placedBy(t, "s-"+model); clause != reasonFableSpent {
				t.Fatalf("placed for %q, want %q", clause, reasonFableSpent)
			}
		})
	}
}

// What §104 leaves alone on the same path: a Fable or Haiku turn ranks as it
// did (the ID tie-break here, with the preference on), and with
// SUBROUTER_FABLE_SPENT_PREFERENCE off an Opus turn ranks as before §104.
func TestTurnsTheFableSpentPreferenceDoesNotCoverRankAcrossPeersAsBefore(t *testing.T) {
	for _, tc := range []struct {
		name       string
		model      string
		preference bool
	}{
		{"a Fable turn", "claude-fable-5-2", true},
		{"a Haiku turn", "claude-haiku-4-5", true},
		{"an Opus turn with the switch off", "claude-opus-5-5", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setFableSpentPreferenceForTest(t, tc.preference)
			box := fableSpareBox(t)
			laptop := newCrossPoolLaptop(t, nil, box.pool)
			status, served := laptop.modelTurn(t, "s1", tc.model)
			if status != http.StatusOK || served != "a-untouched" {
				t.Fatalf("status %d served by %q, box turns %+v; want a-untouched by the ID tie-break", status, served, box.seen())
			}
			if clause := laptop.placedBy(t, "s1"); clause != reasonTiedByID {
				t.Fatalf("placed for %q, want %q", clause, reasonTiedByID)
			}
		})
	}
}

// The preference is a factor in the steady pace clause and nothing more: a
// peer's account in its last day before the roll, or one its holder chose to
// run to its limit for a saved reset (§95.2), still takes the Opus session
// ahead of a Fable-spent account whose pace, doubled, is more than twice its
// own. a-spent sorts first, so only the drain or the choice puts the other first.
func TestTheDrainAndAChoiceStillTakeAnOpusTurnBeforeAFableSpentAccount(t *testing.T) {
	setFableSpentPreferenceForTest(t, true)
	setResetLoadingForTest(t, true)
	spent := crossPoolClaudeScore("a-spent", 0.6, 100*time.Hour)
	spent.FableSpent = 1
	rolling := crossPoolClaudeScore("b-rolling", 0.1, 20*time.Hour)
	chosen := crossPoolClaudeScore("b-chosen", 0.2, 140*time.Hour)
	chosen.ResetChoice = selectacct.ResetChoice{Meter: selectacct.ResetChoiceWeek, Order: 1, Why: "test"}
	for _, tc := range []struct {
		name   string
		other  selectacct.Score
		clause string
	}{
		{"an account inside its roll window", rolling, "inside its weekly roll window; that budget expires either way"},
		{"an account chosen for its week", chosen, reasonChosenForReset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := newContractBox(t, boxAccount{Score: spent}, boxAccount{Score: tc.other})
			laptop := newCrossPoolLaptop(t, nil, box.pool)
			status, served := laptop.modelTurn(t, "s1", "claude-opus-5-5")
			if status != http.StatusOK || served != tc.other.AccountID {
				t.Fatalf("status %d served by %q, box turns %+v; want %s", status, served, box.seen(), tc.other.AccountID)
			}
			if clause := laptop.placedBy(t, "s1"); clause != tc.clause {
				t.Fatalf("placed for %q, want %q", clause, tc.clause)
			}
		})
	}
}
