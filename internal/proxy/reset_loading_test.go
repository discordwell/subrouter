package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// PATCHES §95.2 through the daemon: in real mode new sessions go to the
// accounts chosen for a reset, its own and its peers', and nothing moves a
// session that already has an account. Every provider here is a fake that
// fails the test on a claim or a redeem.

// reasonChosenForReset is the clause selection names when a choice decides.
const reasonChosenForReset = "chosen to run to its limit for its next saved reset"

// chooseForTest makes the daemon's last pass have chosen one account, as
// chooseResetAccounts keeps a choice.
func chooseForTest(server Server, provider accounts.Provider, id string, choice selectacct.ResetChoice) {
	memory := server.AccountRef.sharedAutoResets()
	memory.mu.Lock()
	defer memory.mu.Unlock()
	memory.choices = map[string]resetChosen{
		autoResetKey(accounts.Account{ID: id, Provider: provider}): {choice: choice, label: id, provider: string(provider)},
	}
}

// servedBy sends one turn for session and names the account that served it.
func (d *autoResetClaude) servedBy(session string) string {
	d.t.Helper()
	if answer := d.turn(session, opusModel, ""); answer.status != http.StatusOK {
		d.t.Fatalf("%s: status %d: %s", session, answer.status, answer.body)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.served[len(d.served)-1]
}

// 7. A warm session never moves for a choice: a session on a steady account
// stays there when another account becomes chosen, and a session on a chosen
// account stays when the choice ends. New sessions follow the choice.
func TestAWarmSessionNeverMovesForAChoice(t *testing.T) {
	profiles := choiceProfiles("")
	profiles["spare"].week = 10
	d := choiceClaude(t, AutoResetReal, true, nil, profiles)
	if got := d.servedBy("early"); got != "spare" {
		t.Fatalf("calibration: before any choice a new session goes to %s, want spare", got)
	}
	d.pass()
	if _, inside := published(t, usageRows(t, d.server)["held"]); inside == nil {
		t.Fatal("calibration: held is not chosen")
	}
	if got := d.servedBy("fresh"); got != "held" {
		t.Fatalf("a new session goes to %s, want the chosen held", got)
	}
	if got := d.servedBy("early"); got != "spare" {
		t.Fatalf("the warm session on spare moved to %s when held became chosen", got)
	}
	d.set("held", func(p *autoResetProfile) { p.block = cedarBlock() })
	d.measure()
	d.pass()
	if _, inside := published(t, usageRows(t, d.server)["held"]); inside != nil {
		t.Fatal("calibration: held is still chosen with no reset")
	}
	if got := d.servedBy("fresh"); got != "held" {
		t.Fatalf("the warm session on held moved to %s when its choice ended", got)
	}
	if got := d.servedBy("later"); got != "spare" {
		t.Fatalf("calibration: with the choice ended a new session goes to %s, want spare", got)
	}
}

// 8. A peer's choice is honoured here exactly as a local one: the box's own
// selection and the laptop's both put the box's chosen account first for a
// new session, for the same reason, whatever the laptop's own mode. A body
// from before §95 changes nothing, and SUBROUTER_RESET_LOADING=0 ignores
// every choice.
func TestAPeersChoiceIsHonouredAsALocalOne(t *testing.T) {
	box := choiceClaude(t, AutoResetReal, true, nil, nil)
	box.pass()
	own, err := box.server.selectionScheduler("", "").Explain(SchedulerAccounts(box.server.accountList()))
	if err != nil {
		t.Fatal(err)
	}
	if own[0].Account.ID != "held" || own[0].Reason != reasonChosenForReset {
		t.Fatalf("the box ranks %s first for %q, want held for %q", own[0].Account.ID, own[0].Reason, reasonChosenForReset)
	}
	rec := httptest.NewRecorder()
	box.server.handleUsageStatus(rec, loopbackRequest(http.MethodGet, remotePoolUsagePath, ""))
	body := rec.Body.Bytes()
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		delete(row["normalized"].(map[string]any), "reset_choice")
	}
	before95, _ := json.Marshal(rows)

	laptop := choiceClaude(t, AutoResetReal, true, map[string]float64{"solo": 1.2}, map[string]*autoResetProfile{
		"solo": {week: 10, fable: 10, roll: 100 * time.Hour},
	})
	placeOn := func(body []byte, poolModel string) string {
		t.Helper()
		now := time.Now()
		candidates, _, ok := NewRemotePool("http://peer.invalid", "", "").decisionCandidates(AdminSnapshot{Body: body, At: now}, string(accounts.ProviderClaude), poolModel, nil, now)
		if !ok || len(candidates) != 2 {
			t.Fatalf("calibration: %d peer candidates (ok %v)", len(candidates), ok)
		}
		placement, err := placeAcrossPools(laptop.server.selectionScheduler(poolModel, ""), laptop.server.accountList(), candidates)
		if err != nil {
			t.Fatal(err)
		}
		if placement.Local {
			return "local " + placement.Account.ID
		}
		return fmt.Sprintf("peer %s: %s", placement.Peer.Account, placement.Clause)
	}
	place := func(body []byte) string { t.Helper(); return placeOn(body, "") }
	want := "peer held: " + reasonChosenForReset
	for _, mode := range []AutoResetMode{AutoResetOff, AutoResetShadow, AutoResetReal} {
		setAutoResetForTest(t, mode, true)
		if got := place(body); got != want {
			t.Fatalf("the laptop in %s mode places a new session on %q, want %q", mode, got, want)
		}
	}
	if got := place(before95); got != "local solo" {
		t.Fatalf("a body from before §95 places on %q, want the laptop's own solo", got)
	}
	// The Fable pool: the box publishes its choice at the top level, and the
	// account's pool entry takes it.
	if _, ok := rows[0]["normalized"].(map[string]any)["pools"].(map[string]any)["claudefable"]; !ok {
		t.Fatalf("calibration: the box publishes no Fable pool entry: %v", rows[0]["normalized"])
	}
	if got := placeOn(body, "claude-fable"); got != want {
		t.Fatalf("a Fable turn places on %q, want %q", got, want)
	}
	// A choice this build cannot rank on is ignored: an order under 1.
	var unranked []map[string]any
	if err := json.Unmarshal(body, &unranked); err != nil {
		t.Fatal(err)
	}
	for _, row := range unranked {
		if choice, ok := row["normalized"].(map[string]any)["reset_choice"].(map[string]any); ok {
			choice["order"] = 0
		}
	}
	orderZero, _ := json.Marshal(unranked)
	if got := place(orderZero); got != "local solo" {
		t.Fatalf("a choice at order 0 places on %q, want the laptop's own solo", got)
	}
	setResetLoadingForTest(t, false)
	if got := place(body); got != "local solo" {
		t.Fatalf("with SUBROUTER_RESET_LOADING=0 the laptop places on %q, want its own solo", got)
	}
}

// 4. Through the daemon: a chosen Codex account with 2% of its week left takes
// a new session, which the floor refuses it unchosen, and no "routing
// optimistically" warning says otherwise. Once its credential dies it takes
// none, its choice standing or not.
func TestAChosenCodexAccountBelowTheFloorTakesANewSession(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	setResetLoadingForTest(t, true)
	d := newAutoResetCodex(t, map[string]*autoResetCodexAccount{
		heldCodex:  {used: 98, reset: 150 * 3600, credits: []accounts.RateLimitResetCredit{autoResetTestCredit("credit", 15*burnDay)}},
		spareCodex: {used: 50, reset: 150 * 3600},
	})
	newSession := func(session string) string {
		t.Helper()
		if status, body := d.turn(session); status != http.StatusOK {
			t.Fatalf("%s: status %d: %s", session, status, body)
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.served[len(d.served)-1]
	}
	if got := newSession("before"); got != spareCodex {
		t.Fatalf("calibration: unchosen, a new session goes to %s, want spare", got)
	}
	chooseForTest(d.server, accounts.ProviderCodex, heldCodex, selectacct.ResetChoice{Meter: selectacct.ResetChoiceCodexWeek, Order: 1})
	if got := newSession("chosen"); got != heldCodex {
		t.Fatalf("chosen with 2%% left, a new session goes to %s, want held", got)
	}
	if lines := jsonLogLines(d.logs, "selected OAuth account below new-session headroom; routing optimistically"); len(lines) != 0 {
		t.Fatalf("a placement on the chosen account logged %v", lines)
	}
	d.server.AccountRef.noteCredResult(d.account(heldCodex), &accounts.CodexStoredRefreshFailureError{})
	// The scoring refresh a request makes when the scores are stale, which
	// scores a known-dead credential zero.
	scores, _ := d.server.scoreAccounts(context.Background(), oauthAccounts(d.server.accountList()))
	d.server.SchedulerRef.Set(selectacct.NewScheduler(scores))
	if got := newSession("dead"); got != spareCodex {
		t.Fatalf("with its credential dead, a new session goes to %s, want spare", got)
	}
}

// 9. A choice ranks only in real mode with loading on. Off, shadow, and real
// with SUBROUTER_RESET_LOADING=0: the scheduler selection builds ranks the
// accounts the same way for routing and for a steady pick, and usage-status
// publishes the same verdicts, before and after a pass that chooses. In real
// mode the same pass puts the choice in charge.
func TestAChoiceRanksOnlyInRealMode(t *testing.T) {
	ranking := func(s Server) []string {
		t.Helper()
		scheduler := s.selectionScheduler("", "")
		candidates := SchedulerAccounts(s.accountList())
		var out []string
		for _, explain := range []func([]accounts.Account) ([]selectacct.Explanation, error){scheduler.Explain, scheduler.ExplainSteady} {
			explained, err := explain(candidates)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range explained {
				out = append(out, e.Account.ID+": "+e.Reason)
			}
		}
		picked, _ := scheduler.Pick(candidates)
		steady, _ := scheduler.PickSteady(candidates)
		out = append(out, picked.ID, steady.ID)
		rows := usageRows(t, s)
		for _, id := range sortedKeys(rows) {
			var normalized map[string]json.RawMessage
			_ = json.Unmarshal(rows[id]["normalized"], &normalized)
			out = append(out, id+" "+string(normalized["new_session"]))
		}
		return out
	}
	for _, c := range []struct {
		mode    AutoResetMode
		loading bool
	}{{AutoResetOff, true}, {AutoResetShadow, true}, {AutoResetReal, false}, {AutoResetReal, true}} {
		d := choiceClaude(t, c.mode, c.loading, nil, nil)
		before := ranking(d.server)
		d.pass()
		after := ranking(d.server)
		ranks := c.mode == AutoResetReal && c.loading
		if reflect.DeepEqual(after, before) == ranks {
			t.Fatalf("%s, loading %v: ranking before the pass\n%v\nafter\n%v\nwant a change %v", c.mode, c.loading, before, after, ranks)
		}
		if ranks && after[0] != "held: "+reasonChosenForReset {
			t.Fatalf("real mode: %q ranks first, want held for %q", after[0], reasonChosenForReset)
		}
	}
}

// Selection reads this daemon's choices while the background pass replaces
// them. Run under -race: requests build the scheduler and rank, and read
// usage-status, while passes choose, on the map the pass swaps whole under its
// lock.
func TestRankingReadsTheChoicesWhileAPassReplacesThem(t *testing.T) {
	d := choiceClaude(t, AutoResetReal, true, nil, nil)
	candidates := SchedulerAccounts(d.server.accountList())
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := d.server.selectionScheduler(opusModel, "").Explain(candidates); err != nil {
					t.Error(err)
					return
				}
				snapshot, _ := d.server.AccountRef.UsageStatusSnapshot()
				d.server.withNewSessionVerdicts(d.server.withNormalizedUsage(snapshot))
			}
		}()
	}
	for range 20 {
		d.pass()
	}
	close(stop)
	wg.Wait()
	explained, err := d.server.selectionScheduler(opusModel, "").Explain(candidates)
	if err != nil {
		t.Fatal(err)
	}
	if explained[0].Account.ID != "held" || explained[0].Reason != reasonChosenForReset {
		t.Fatalf("after the passes %s ranks first for %q, want held for %q", explained[0].Account.ID, explained[0].Reason, reasonChosenForReset)
	}
}
