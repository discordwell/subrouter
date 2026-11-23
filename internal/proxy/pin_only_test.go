package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// PATCHES §98: an account listed in SUBROUTER_PIN_ONLY_ACCOUNTS takes pinned
// sessions only. Every provider here is a fake; the Claude fakes fail the test
// on any request they do not expect, a claim among them.

const pinOnlyID = "test-account"

func listPinOnly(server Server, ids ...string) { server.AccountRef.SetPinOnlyAccounts(ids) }

func (d *autoResetClaude) servedList() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.served...)
}

func (d *autoResetClaude) resetServed() {
	d.mu.Lock()
	d.served = nil
	d.mu.Unlock()
}

// freshAndSpare is the test account, nearly unused, which ranks first for a
// new session, and spare.
func freshAndSpare() map[string]*autoResetProfile {
	return map[string]*autoResetProfile{
		pinOnlyID: {week: 5, fable: 5},
		"spare":   {week: 40, fable: 10},
	}
}

// 1 and 2. An unpinned new session never lands on a listed account, even
// where it ranks first, and a pinned one does. A session already on it
// without its pin leaves it. With nothing else to take it, a new session is
// refused rather than placed there. Unset, the account takes the new session.
func TestAPinOnlyAccountTakesNoNewSessionAndServesItsPin(t *testing.T) {
	setAutoResetForTest(t, AutoResetOff, true)
	d := newAutoResetClaude(t, freshAndSpare())
	if got := d.servedBy("calibration"); got != pinOnlyID {
		t.Fatalf("calibration: unlisted, a new session went to %s, want %s, which ranks first", got, pinOnlyID)
	}
	listPinOnly(d.server, pinOnlyID)
	for i := 0; i < 3; i++ {
		if got := d.servedBy(fmt.Sprintf("new-%d", i)); got != "spare" {
			t.Fatalf("listed: new session %d went to %s, want spare", i, got)
		}
	}
	answer := d.turn("pinned", opusModel, pinOnlyID)
	if served := d.servedList(); answer.status != http.StatusOK || !strings.Contains(answer.body, "msg_"+pinOnlyID) || served[len(served)-1] != pinOnlyID {
		t.Fatalf("listed: the pinned turn got %+v, served by %v; want the test account's answer", answer, served)
	}
	// The calibration session sits on the test account from before the list:
	// without its pin, its next turn leaves.
	if got := d.servedBy("calibration"); got != "spare" {
		t.Fatalf("listed: an unpinned session already on the account was served by %s, want spare", got)
	}
	if got := d.servedBy("pinned"); got != "spare" {
		t.Fatalf("listed: the pinned session's turn without its pin was served by %s, want spare", got)
	}
	if answer := d.turn("pinned", opusModel, pinOnlyID); answer.status != http.StatusOK || !strings.Contains(answer.body, "msg_"+pinOnlyID) {
		t.Fatalf("listed: the pin again got %+v, want the test account's answer", answer)
	}

	alone := newAutoResetClaude(t, map[string]*autoResetProfile{pinOnlyID: {week: 5, fable: 5}})
	listPinOnly(alone.server, pinOnlyID)
	if answer := alone.turn("new", opusModel, ""); answer.status == http.StatusOK || len(alone.servedList()) != 0 {
		t.Fatalf("listed and alone: a new session got %d and the upstream saw %v, want a refusal and nothing sent", answer.status, alone.servedList())
	}
	if answer := alone.turn("pinned", opusModel, pinOnlyID); answer.status != http.StatusOK {
		t.Fatalf("listed and alone: the pin got %+v, want it served", answer)
	}
}

// 3. A session forced off its account (its turn meets the weekly wall) is
// moved to the best other account, never the listed one, and with no other
// account it is not moved there either.
func TestAForcedMoveNeverLandsOnAPinOnlyAccount(t *testing.T) {
	setAutoResetForTest(t, AutoResetOff, true)
	walled := func(other bool) *autoResetClaude {
		profiles := map[string]*autoResetProfile{
			pinOnlyID: {week: 5, fable: 5},
			"spent":   {week: 50, fable: 10, turn: claudeWall("7d")},
		}
		if other {
			profiles["other"] = &autoResetProfile{week: 60, fable: 10}
		}
		d := newAutoResetClaude(t, profiles)
		d.pinSession("conversation", "spent")
		return d
	}
	calibration := walled(true)
	calibration.turn("conversation", opusModel, "")
	if got := calibration.servedList(); !reflect.DeepEqual(got, []string{"spent", pinOnlyID}) {
		t.Fatalf("calibration: unlisted, the moved turn was served by %v, want spent then %s", got, pinOnlyID)
	}
	d := walled(true)
	listPinOnly(d.server, pinOnlyID)
	if answer := d.turn("conversation", opusModel, ""); answer.status != http.StatusOK ||
		!reflect.DeepEqual(d.servedList(), []string{"spent", "other"}) {
		t.Fatalf("listed: the moved turn got %d, served by %v; want spent then other", answer.status, d.servedList())
	}
	none := walled(false)
	listPinOnly(none.server, pinOnlyID)
	if answer := none.turn("conversation", opusModel, ""); answer.status == http.StatusOK || slices.Contains(none.servedList(), pinOnlyID) {
		t.Fatalf("listed, nothing else: the turn got %d, served by %v; want the refusal and nothing on %s", answer.status, none.servedList(), pinOnlyID)
	}
}

// newSessionVerdictOf is a usage-status row's normalized.new_session.
func newSessionVerdictOf(t *testing.T, row map[string]json.RawMessage) selectacct.NewSessionVerdict {
	t.Helper()
	var normalized struct {
		NewSession *selectacct.NewSessionVerdict `json:"new_session"`
	}
	if err := json.Unmarshal(row["normalized"], &normalized); err != nil || normalized.NewSession == nil {
		t.Fatalf("row has no new-session verdict: %s", row["normalized"])
	}
	return *normalized.NewSession
}

// 4 and the views. usage-status publishes the listed account as not eligible,
// for pin_only, with no refugee slot, and pool.json marks it. A peer that
// reads that body places a new session on its other account instead.
func TestPeersSeeAPinOnlyAccountIneligible(t *testing.T) {
	setAutoResetForTest(t, AutoResetOff, true)
	box := newAutoResetClaude(t, freshAndSpare())
	laptop := newAutoResetClaude(t, map[string]*autoResetProfile{"solo": {week: 99, fable: 10}})
	body := func() []byte {
		t.Helper()
		rec := httptest.NewRecorder()
		box.server.handleUsageStatus(rec, loopbackRequest(http.MethodGet, remotePoolUsagePath, ""))
		return rec.Body.Bytes()
	}
	place := func() string {
		t.Helper()
		now := time.Now()
		candidates, _, ok := NewRemotePool("http://peer.invalid", "", "").decisionCandidates(AdminSnapshot{Body: body(), At: now}, string(accounts.ProviderClaude), "", nil, now)
		if !ok {
			t.Fatal("the box's body gave no candidates")
		}
		placement, err := placeAcrossPools(laptop.server.selectionScheduler("", ""), laptop.server.accountList(), candidates)
		if err != nil {
			t.Fatal(err)
		}
		if placement.Local {
			return "local " + placement.Account.ID
		}
		return "peer " + placement.Peer.Account
	}
	if got := newSessionVerdictOf(t, usageRows(t, box.server)[pinOnlyID]); !got.Eligible || got.Reason != selectacct.VerdictEligible {
		t.Fatalf("calibration: unlisted verdict %+v", got)
	}
	if got := place(); got != "peer "+pinOnlyID {
		t.Fatalf("calibration: unlisted, the laptop places a new session on %q, want the box's %s", got, pinOnlyID)
	}
	if row := poolRowsOf(t, box.server)[pinOnlyID]; row.PinOnly || row.State != poolStateOK {
		t.Fatalf("calibration: unlisted pool.json row %+v", row)
	}

	listPinOnly(box.server, pinOnlyID)
	rows := usageRows(t, box.server)
	got := newSessionVerdictOf(t, rows[pinOnlyID])
	if got.Eligible || got.Reason != selectacct.VerdictPinOnly || got.Evicted || got.RefugeeCap != (selectacct.RefugeeCap{Slots: 0, Published: true}) {
		t.Fatalf("listed: verdict %+v, want not eligible for pin_only, not evicted, no refugee", got)
	}
	if spare := newSessionVerdictOf(t, rows["spare"]); !spare.Eligible {
		t.Fatalf("listed: spare's verdict %+v, want eligible", spare)
	}
	if got := place(); got != "peer spare" {
		t.Fatalf("listed: the laptop places a new session on %q, want the box's spare", got)
	}
	row := poolRowsOf(t, box.server)[pinOnlyID]
	if !row.PinOnly || row.State != poolStateBurn || !strings.Contains(row.StateNote, "takes pinned sessions only") {
		t.Fatalf("listed: pool.json row pin_only %v, state %q, note %q", row.PinOnly, row.State, row.StateNote)
	}
	if spare := poolRowsOf(t, box.server)["spare"]; spare.PinOnly {
		t.Fatal("listed: spare is marked pin-only on pool.json")
	}
}

// heldTestAccount is the test account holding a week reset and then a Fable
// reset, read before any limit, whose turns meet turn; and spare.
func heldTestAccount(week, fable float64, block string, turn http.HandlerFunc) map[string]*autoResetProfile {
	return map[string]*autoResetProfile{
		pinOnlyID: {week: week, fable: fable, block: block, turn: turn},
		"spare":   {week: 40, fable: 10},
	}
}

// 5. §93 decides nothing for a listed account at either moment: a pinned
// turn at its week's or its Fable meter's wall, and a reading at 99.6% of its
// week with a grant usable now and a warm session. Unlisted, each is a
// would-claim.
func TestAutomaticResetsDecideNothingForAPinOnlyAccount(t *testing.T) {
	setAutoResetForTest(t, AutoResetShadow, true)
	both := cedarBlock(autoResetWeekGrant("week_reset", 1, false), autoResetFableGrant("fable_reset", false))
	for _, c := range []struct {
		name, model, kind string
		profiles          func() map[string]*autoResetProfile
	}{
		{"the week's wall", opusModel, "week", func() map[string]*autoResetProfile { return heldTestAccount(95, 20, both, claudeWall("7d")) }},
		{"the Fable wall", "claude-fable-5-2", "fable", func() map[string]*autoResetProfile {
			return heldTestAccount(50, 95, cedarBlock(autoResetFableGrant("fable_reset", false)), claudeWall("7d_oi"))
		}},
	} {
		for _, listed := range []bool{false, true} {
			d := newAutoResetClaude(t, c.profiles())
			if listed {
				listPinOnly(d.server, pinOnlyID)
			}
			if answer := d.turn("pinned", c.model, pinOnlyID); answer.status != http.StatusTooManyRequests {
				t.Fatalf("%s, listed %v: the pinned turn got %d, want the provider's 429", c.name, listed, answer.status)
			}
			decision, decided := d.decision(pinOnlyID)
			lines := wouldClaimLines(d.logs)
			if !listed && (!decided || decision.Verdict != AutoResetWouldClaim || decision.Kind != c.kind || len(lines) != 1) {
				t.Fatalf("calibration %s: decision %+v (%v), %d lines; want a would-claim of the %s reset", c.name, decision, decided, len(lines), c.kind)
			}
			if listed && (decided || len(lines) != 0) {
				t.Fatalf("%s, listed: decision %+v and %d lines, want none", c.name, decision, len(lines))
			}
		}
	}
	for _, listed := range []bool{false, true} {
		d := newAutoResetClaude(t, map[string]*autoResetProfile{
			pinOnlyID: {week: 99.6, fable: 30, block: cedarBlock(autoResetWeekGrant("week_reset", 1, true))},
		})
		if listed {
			listPinOnly(d.server, pinOnlyID)
		}
		d.pinSession("conversation", pinOnlyID)
		d.server.autoResetReadings(context.Background())
		decision, decided := d.decision(pinOnlyID)
		if !listed && (!decided || decision.Verdict != AutoResetWouldClaim || decision.Moment != "reading") {
			t.Fatalf("calibration, a reading: decision %+v (%v), want a would-claim", decision, decided)
		}
		if listed && decided {
			t.Fatalf("listed, a reading: decision %+v, want none", decision)
		}
	}
}

// latchFiles is what the state directory holds of §93.2's latches.
func latchFiles(t *testing.T, stateDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(stateDir, "auto-reset"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// 5 in real mode (SUBROUTER_AUTO_RESET=1, as on every daemon): a pinned turn
// that meets the wall on a listed account holding a reset, a Fable reset
// included, gets the provider's own answer, as §89 leaves a pin the provider
// limits, and nothing is claimed, held or latched. Unlisted, the same turn
// gets the same answer and the claim runs.
func TestARealModePinnedTurnAtAPinOnlyWallClaimsAndHoldsNothing(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	for _, c := range []struct {
		name, model string
		profile     func() *claudeClaimProfile
		spend       func(*claudeClaimProfile)
	}{
		{"a week reset at the week's wall", opusModel,
			func() *claudeClaimProfile {
				return &claudeClaimProfile{week: 95, fable: 20, grants: []bool{false, true}}
			},
			func(p *claudeClaimProfile) { p.week = 100 }},
		{"a Fable reset at the Fable wall", "claude-fable-5-2",
			func() *claudeClaimProfile { return &claudeClaimProfile{week: 50, fable: 95, grants: []bool{true}} },
			func(p *claudeClaimProfile) { p.fable = 100 }},
	} {
		answers := map[bool]claudeAnswer{}
		for _, listed := range []bool{false, true} {
			what := fmt.Sprintf("%s, listed %v", c.name, listed)
			f := newClaudeClaimFake(t, map[string]*claudeClaimProfile{pinOnlyID: c.profile(), "spare": {week: 40, fable: 10}})
			stateDir := t.TempDir()
			d := f.daemon(t, stateDir)
			if listed {
				listPinOnly(d.server, pinOnlyID)
			}
			f.set(pinOnlyID, c.spend)
			answer := d.ask("pinned", c.model, http.Header{"X-Subrouter-Account-Id": {pinOnlyID}})
			settle(t, d.server)
			claims, served := f.counts()
			answers[listed] = answer
			if answer.status != http.StatusTooManyRequests || !reflect.DeepEqual(served, []string{pinOnlyID + " 429"}) {
				t.Fatalf("%s: the client got %d and Anthropic served %v; want the provider's 429 from the test account alone", what, answer.status, served)
			}
			if !listed {
				if claims != 1 {
					t.Fatalf("calibration %s: %d claims, want the claim to run", what, claims)
				}
				continue
			}
			decision, decided := d.server.LastAutoResetDecision(accounts.ProviderClaude, pinOnlyID)
			if claims != 0 || decided || len(latchFiles(t, stateDir)) != 0 {
				t.Fatalf("%s: %d claims, decision %+v (%v), latches %v; want none", what, claims, decision, decided, latchFiles(t, stateDir))
			}
			if d.server.autoResetRunningClaim(accounts.Account{ID: pinOnlyID, Provider: accounts.ProviderClaude}) != nil {
				t.Fatalf("%s: a claim is held on the account", what)
			}
			// The next pin is not held either: it is answered at once, and it
			// is not served.
			start := time.Now()
			if again := d.ask("pinned", c.model, http.Header{"X-Subrouter-Account-Id": {pinOnlyID}}); again.status == http.StatusOK || time.Since(start) > 10*time.Second {
				t.Fatalf("%s: the next pin got %d after %v", what, again.status, time.Since(start))
			}
			settle(t, d.server)
			if claims, _ := f.counts(); claims != 0 {
				t.Fatalf("%s: the next pin made %d claims", what, claims)
			}
		}
		if answers[true].body != answers[false].body {
			t.Fatalf("%s: listed the client got %q, unlisted %q; want the provider's same answer", c.name, answers[true].body, answers[false].body)
		}
	}
}

// 6. §95 never chooses a listed account: in shadow and real mode the same
// account is chosen unlisted, and listed publishes nothing.
func TestAPinOnlyAccountIsNeverChosenForAReset(t *testing.T) {
	for _, mode := range []AutoResetMode{AutoResetShadow, AutoResetReal} {
		for _, listed := range []bool{false, true} {
			d := choiceClaude(t, mode, true, nil, nil)
			if listed {
				listPinOnly(d.server, "held")
			}
			d.pass()
			would, inside := published(t, usageRows(t, d.server)["held"])
			chosen := would != nil || inside != nil
			if chosen == listed {
				t.Fatalf("%s, listed %v: would_choose %+v, reset_choice %+v", mode, listed, would, inside)
			}
			if listed && len(choiceLines(d.logs)) != 0 {
				t.Fatalf("%s, listed: choice lines %v, want none", mode, choiceLines(d.logs))
			}
		}
	}
}

// burn24 is a meter's 24-hour pool burn, or -1 when none.
func burn24(m UsagePoolMeter) float64 {
	if w, ok := m.Windows["24h"]; ok && w.Burn != nil {
		return *w.Burn
	}
	return -1
}

func meterNamed(t *testing.T, meters []UsagePoolMeter, provider, meter string) UsagePoolMeter {
	t.Helper()
	for _, m := range meters {
		if m.Provider == provider && m.Meter == meter {
			return m
		}
	}
	t.Fatalf("no %s %s meter", provider, meter)
	return UsagePoolMeter{}
}

// 7. The pool figures §94 to §96 read leave a listed account out: the
// pressure behind need, the expiry window's holders, burn.json, and a peer's
// reading of this daemon's body. The account's own burn stays on its row.
func TestThePoolFiguresLeaveAPinOnlyAccountOut(t *testing.T) {
	ends := time.Now().Add(10 * 24 * time.Hour).UTC().Format(time.RFC3339)
	grant := func(id string) string {
		g := autoResetWeekGrant(id, 1, false)
		g["ends_at"] = ends
		return cedarBlock(g)
	}
	type figures struct {
		pool, burnJSON float64
		holders        int
		accounts       []string
		peerRows       []string
		ownBurn        bool
	}
	read := func(listed bool) (figures, float64) {
		d := worthClaude(t, AutoResetShadow, map[string]float64{pinOnlyID: 3.0, "spare": 1.2}, map[string]*autoResetProfile{
			pinOnlyID: {week: 30, fable: 10, block: grant("test_reset"), roll: 100 * time.Hour},
			"spare":   {week: 40, fable: 10, block: grant("spare_reset"), roll: 100 * time.Hour},
		})
		if listed {
			listPinOnly(d.server, pinOnlyID)
		}
		snapshot, _ := d.server.AccountRef.UsageStatusSnapshot()
		rows := d.server.AccountRef.withUsageBurn(d.server.withHeldResets(snapshot))
		pool := d.server.resetPoolOf(rows, d.server.AccountRef.sharedAutoResets().clock())
		var out figures
		out.pool = burn24(pool.meter(string(accounts.ProviderClaude), accounts.ResetMeterWeek))
		for _, holding := range pool.holdings {
			out.holders += holding.holders
		}
		report := burnReport(t, d.server)
		week := meterNamed(t, report.Meters, string(accounts.ProviderClaude), usageMeterWeek)
		out.burnJSON = burn24(week)
		for _, account := range week.Accounts {
			out.accounts = append(out.accounts, account.ID)
		}
		status := usageRows(t, d.server)
		_, out.ownBurn = status[pinOnlyID]["burn"]
		rec := httptest.NewRecorder()
		d.server.handleUsageStatus(rec, loopbackRequest(http.MethodGet, remotePoolUsagePath, ""))
		for _, row := range readPeerBurn("box", AdminSnapshot{Body: rec.Body.Bytes(), At: time.Now()}).rows {
			out.peerRows = append(out.peerRows, row.id)
		}
		slices.Sort(out.accounts)
		slices.Sort(out.peerRows)
		var spareRow []UsageAccountBurn
		for _, account := range week.Accounts {
			if account.ID == "spare" {
				spareRow = append(spareRow, account)
			}
		}
		return out, burn24(UsagePoolMeter{Windows: poolBurnWindows(spareRow)})
	}
	unlisted, spareAlone := read(false)
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	if unlisted.pool <= spareAlone+0.1 || unlisted.burnJSON <= spareAlone+0.1 || unlisted.holders != 2 ||
		!reflect.DeepEqual(unlisted.accounts, []string{"spare", pinOnlyID}) || !reflect.DeepEqual(unlisted.peerRows, []string{"spare", pinOnlyID}) {
		t.Fatalf("calibration: unlisted %+v (spare alone %.3f); want both accounts counted", unlisted, spareAlone)
	}
	listed, _ := read(true)
	if spareAlone <= 0 || !near(listed.pool, spareAlone) || !near(listed.burnJSON, spareAlone) || listed.holders != 1 ||
		!reflect.DeepEqual(listed.accounts, []string{"spare"}) || !reflect.DeepEqual(listed.peerRows, []string{"spare"}) {
		t.Fatalf("listed: %+v, want spare's figures alone (%.3f), one holder, and only spare in burn.json and a peer's read", listed, spareAlone)
	}
	if !listed.ownBurn || !unlisted.ownBurn {
		t.Fatalf("the test account's own burn on its row: listed %v, unlisted %v; want it kept", listed.ownBurn, unlisted.ownBurn)
	}
}

// 8. The daemon sends a listed account nothing of its own: no paid Claude
// Messages probe and no §90 start of a Codex week.
func TestThePoolSendsAPinOnlyAccountNothingOfItsOwn(t *testing.T) {
	transport := &claudeProbeTransport{}
	transport.set(http.StatusInternalServerError, `{}`)
	ref := cacheTestAccountRef(t, transport)
	listed := accounts.Account{ID: pinOnlyID, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-listed"}
	other := accounts.Account{ID: "other", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-other"}
	ref.SetPinOnlyAccounts([]string{pinOnlyID})
	if windows, err := ref.claudeFableProbe(context.Background(), ref.client, listed); err != nil || windows != nil {
		t.Fatalf("the listed account's Fable probe = %v, %v; want nothing", windows, err)
	}
	ref.probeClaudeLiveness(context.Background(), ref.client, listed)
	if got := transport.calls("tok-listed"); got != 0 {
		t.Fatalf("Messages probes on the listed account = %d, want 0", got)
	}
	_, _ = ref.claudeFableProbe(context.Background(), ref.client, other)
	if got := transport.calls("tok-other"); got != 1 {
		t.Fatalf("calibration: probes on an unlisted account = %d, want 1", got)
	}

	server := Server{AccountRef: &AccountRef{}, SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))}
	codex := accounts.Account{ID: pinOnlyID, Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "tok"}
	if !server.selfStartAllowed(codex, accounts.ProviderCodex) {
		t.Fatal("calibration: an unlisted Codex account may not be started")
	}
	listPinOnly(server, pinOnlyID)
	if server.selfStartAllowed(codex, accounts.ProviderCodex) {
		t.Fatal("a listed Codex account may be started by the daemon's own turn")
	}
}

// Unset changes nothing: an empty or blank list marks no account.
func TestAnEmptyPinOnlyListMarksNoAccount(t *testing.T) {
	ref := &AccountRef{}
	for _, ids := range [][]string{nil, {}, {"", "  "}} {
		ref.SetPinOnlyAccounts(ids)
		if got := ref.pinOnlyAccounts(); got != nil {
			t.Fatalf("SetPinOnlyAccounts(%q) = %v, want none", ids, got)
		}
	}
	ref.SetPinOnlyAccounts([]string{" " + pinOnlyID + " "})
	if !ref.PinOnlyAccount(pinOnlyID) || ref.PinOnlyAccount("spare") {
		t.Fatalf("the list reads %v", ref.pinOnlyAccounts())
	}
	var nilRef *AccountRef
	if nilRef.PinOnlyAccount(pinOnlyID) {
		t.Fatal("a nil ref marks an account")
	}
}

// askWith sends one Messages turn for session with extra headers and names
// the account that served it, "" for none.
func (d *autoResetClaude) askWith(session string, header http.Header) (int, string) {
	d.t.Helper()
	d.resetServed()
	req, err := http.NewRequest(http.MethodPost, d.proxy.URL+"/v1/messages",
		strings.NewReader(`{"model":"`+opusModel+`","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		d.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", session)
	for name, values := range header {
		req.Header[name] = values
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		d.t.Fatal(err)
	}
	_ = response.Body.Close()
	settle(d.t, d.server)
	served := d.servedList()
	if len(served) == 0 {
		return response.StatusCode, ""
	}
	return response.StatusCode, served[len(served)-1]
}

// 1. A caller's preference is softer than a pin: a new session that prefers
// a listed account starts on the scheduler's pick instead.
func TestAPreferenceForAPinOnlyAccountIsNotHonoured(t *testing.T) {
	setAutoResetForTest(t, AutoResetOff, true)
	for _, listed := range []bool{false, true} {
		d := newAutoResetClaude(t, map[string]*autoResetProfile{
			pinOnlyID: {week: 60, fable: 10},
			"spare":   {week: 5, fable: 5},
		})
		if listed {
			listPinOnly(d.server, pinOnlyID)
		}
		status, served := d.askWith(fmt.Sprintf("prefers-%v", listed), http.Header{"X-Subrouter-Preferred-Account-Id": {pinOnlyID}})
		want := pinOnlyID
		if listed {
			want = "spare"
		}
		if status != http.StatusOK || served != want {
			t.Fatalf("listed %v: a new session preferring the test account got %d on %q, want %s", listed, status, served, want)
		}
	}
}

// 3. An admission spill (one request of a saturated account, sent to the
// least loaded free account) never lands on a listed account.
func TestASpillNeverLandsOnAPinOnlyAccount(t *testing.T) {
	setAutoResetForTest(t, AutoResetOff, true)
	for _, listed := range []bool{false, true} {
		d := newAutoResetClaude(t, map[string]*autoResetProfile{
			"busy":    {week: 40, fable: 10},
			pinOnlyID: {week: 5, fable: 5},
			"zeta":    {week: 60, fable: 10},
		})
		control := testAdmissionSpillControl(2, 4, 1)
		d.server.admission = control
		d.proxy = httptest.NewServer(d.server.Handler())
		t.Cleanup(d.proxy.Close)
		if listed {
			listPinOnly(d.server, pinOnlyID)
		}
		d.pinSession("fan-out", "busy")
		// busy is full with a request waiting, zeta carries one request and is
		// free, and the test account carries none: the least loaded.
		control.mu.Lock()
		bucket := control.bucketLocked(admissionKeyFor(accounts.ProviderClaude, "busy", "claude-opus"))
		bucket.active = bucket.limit
		bucket.waiters = append(bucket.waiters, &admissionWaiter{ready: make(chan struct{})})
		control.bucketLocked(admissionKeyFor(accounts.ProviderClaude, "zeta", "claude-opus")).active = 1
		control.mu.Unlock()
		status, served := d.askWith("fan-out", nil)
		want := pinOnlyID
		if listed {
			want = "zeta"
		}
		if status != http.StatusOK || served != want {
			t.Fatalf("listed %v: the saturated account's request got %d on %q, want it spilled to %s", listed, status, served, want)
		}
	}
}

// 3. The cooked Codex pool's failover redeem (§29.5) spends no credit on a
// listed account and moves no session there.
func TestTheCookedCodexRedeemSkipsAPinOnlyAccount(t *testing.T) {
	for _, listed := range []bool{true, false} {
		var consume int
		server, _ := cookedPoolServer(t, true, &consume)
		server.SchedulerRef.MarkExhausted(accounts.ProviderCodex, "cooked@example.com", "")
		if listed {
			listPinOnly(server, "cooked@example.com")
		}
		_, ok := server.redeemForFailover(context.Background(), accounts.ProviderCodex, "codex", "s1", "", nil)
		if ok != !listed || consume != map[bool]int{true: 0, false: 1}[listed] {
			t.Fatalf("listed %v: revived %v with %d consume calls", listed, ok, consume)
		}
		if _, placed := server.Sessions.Get("codex", "s1"); placed == listed {
			t.Fatalf("listed %v: the session placed %v", listed, placed)
		}
	}
}
