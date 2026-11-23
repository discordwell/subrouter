package proxy

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// Window builders. The upstream type carries NO json tags, so these also stand
// in for what the peer's HTTP surface really returns.

func weeklyWindow(usedPercent float64, resetAfter int64) accounts.UsageWindow {
	return accounts.UsageWindow{Name: "primary", UsedPercent: usedPercent, LimitWindowSeconds: 604800, ResetAfterSeconds: resetAfter}
}

func shortWindow(usedPercent float64, resetAfter int64) accounts.UsageWindow {
	return accounts.UsageWindow{Name: "5h", UsedPercent: usedPercent, LimitWindowSeconds: 18000, ResetAfterSeconds: resetAfter}
}

func claudeWeekly(usedPercent float64, resetAfter int64) accounts.UsageWindow {
	return accounts.UsageWindow{Name: "7d", UsedPercent: usedPercent, LimitWindowSeconds: 604800, ResetAfterSeconds: resetAfter}
}

// ---------------------------------------------------------------------------
// The fuse: PRISTINE vs RUNNING
// ---------------------------------------------------------------------------

// A Codex weekly window is a tumbling fuse. Both accounts below read 100% left
// on any headroom meter; only one of them has anything at risk. If this test
// stops distinguishing them the board has lost its whole reason to exist. The
// board reads it as the score does, the fact ranking reads (PATCHES §86).
func TestFuseSeparatesPristineFromRunning(t *testing.T) {
	_, row := classifyFor(t, accounts.ProviderCodex, []accounts.UsageWindow{weeklyWindow(0, 604800)})
	pristine := row.Fuse
	if pristine == nil {
		t.Fatal("no fuse read from a weekly window")
	}
	if pristine.State != poolFusePristine || !row.WeeklyUnstarted {
		t.Fatalf("reset == window length must read pristine, got %q (weekly_unstarted %v)", pristine.State, row.WeeklyUnstarted)
	}
	if pristine.Elapsed != 0 || pristine.Forfeit != 0 {
		t.Fatalf("a pristine fuse has burned nothing: elapsed=%v forfeit=%v", pristine.Elapsed, pristine.Forfeit)
	}

	// One second off the full window and the clock is lit: the remaining budget
	// now forfeits at the roll whether or not anyone spends it.
	_, row = classifyFor(t, accounts.ProviderCodex, []accounts.UsageWindow{weeklyWindow(0, 604799)})
	if row.Fuse.State != poolFuseRunning || row.WeeklyUnstarted {
		t.Fatalf("reset one second below the window must read running, got %q (weekly_unstarted %v)", row.Fuse.State, row.WeeklyUnstarted)
	}
}

// The board and the router read one fact, not two rules (PATCHES §86). The
// fuse draws the weekly window that rolls first; the router reads the one it
// binds on, the most spent, and only while its reading is live. Where the
// drawn window alone says the opposite of the router, PRISTINE follows the
// router: the board must never promise the next new session to an account the
// router will not give it, or hide one it will.
func TestTheFuseFollowsTheRouterWhereItsOwnWindowSaysOtherwise(t *testing.T) {
	const fortnight = 14 * 24 * 60 * 60
	for _, tc := range []struct {
		name      string
		windows   []accounts.UsageWindow
		unstarted bool
	}{
		{
			name: "the week it binds on has started",
			windows: []accounts.UsageWindow{weeklyWindow(0, 604800),
				{Name: "secondary", UsedPercent: 40, LimitWindowSeconds: fortnight, ResetAfterSeconds: 1000000}},
		},
		{
			name: "a reading from before its own reset",
			windows: []accounts.UsageWindow{{Name: "primary", LimitWindowSeconds: 604800, ResetAfterSeconds: 604800,
				ResetAt: time.Now().Add(-time.Minute)}},
		},
		{
			name: "the week it binds on has not started",
			windows: []accounts.UsageWindow{weeklyWindow(0, 300000),
				{Name: "secondary", UsedPercent: 10, LimitWindowSeconds: fortnight, ResetAfterSeconds: fortnight}},
			unstarted: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, row := classifyFor(t, accounts.ProviderCodex, tc.windows)
			if row.Fuse == nil {
				t.Fatal("no fuse read")
			}
			if byItsWindow := row.Fuse.ResetSeconds >= row.Fuse.WindowSeconds; byItsWindow == tc.unstarted {
				t.Fatalf("calibration: the drawn window alone reads unstarted=%v, the same as the router", byItsWindow)
			}
			if row.WeeklyUnstarted != tc.unstarted || (row.Fuse.State == poolFusePristine) != tc.unstarted || (state == poolStatePristine) != tc.unstarted {
				t.Fatalf("weekly_unstarted %v, fuse %q, state %q (%s); want the router's unstarted=%v throughout",
					row.WeeklyUnstarted, row.Fuse.State, state, row.StateNote, tc.unstarted)
			}
		})
	}
}

// The forfeit band is the number no ordinary meter shows: the span between what
// has been SPENT and what the clock has ELAPSED is budget already out of reach
// at an even pace.
func TestFuseForfeitIsElapsedMinusSpent(t *testing.T) {
	// Half the week gone, a tenth of the budget spent.
	fuse := readFuse([]accounts.UsageWindow{weeklyWindow(10, 302400)}, "", false)
	if fuse.State != poolFuseRunning {
		t.Fatalf("state = %q", fuse.State)
	}
	if diff := fuse.Elapsed - 0.5; diff > 0.001 || diff < -0.001 {
		t.Fatalf("elapsed = %v, want 0.5", fuse.Elapsed)
	}
	if diff := fuse.Spent - 0.1; diff > 0.001 || diff < -0.001 {
		t.Fatalf("spent = %v, want 0.1", fuse.Spent)
	}
	if diff := fuse.Forfeit - 0.4; diff > 0.001 || diff < -0.001 {
		t.Fatalf("forfeit = %v, want 0.4", fuse.Forfeit)
	}

	// Spending FASTER than the clock is not a negative forfeit, it is none.
	ahead := readFuse([]accounts.UsageWindow{weeklyWindow(80, 302400)}, "", false)
	if ahead.Forfeit != 0 {
		t.Fatalf("forfeit = %v, want 0 when spending outruns the clock", ahead.Forfeit)
	}
}

// Spark is a SECOND, independently-armed fuse on the same account: priming
// `primary` does not start it. Reading them off the same window set must not
// mix them up.
func TestFuseReadsSparkIndependently(t *testing.T) {
	windows := []accounts.UsageWindow{
		weeklyWindow(35, 200000),
		{Name: "primary", UsedPercent: 0, LimitWindowSeconds: 604800, ResetAfterSeconds: 604800, Feature: sparkFeature},
	}
	_, row := classifyFor(t, accounts.ProviderCodex, windows)
	if row.Fuse == nil || row.Fuse.State != poolFuseRunning {
		t.Fatalf("primary = %+v, want running", row.Fuse)
	}
	if row.Spark == nil || row.Spark.State != poolFusePristine {
		t.Fatalf("spark = %+v, want pristine: priming primary does not arm spark", row.Spark)
	}
}

// A 5h window is not a fuse and must not be read as one.
func TestFuseIgnoresShortWindows(t *testing.T) {
	if fuse := readFuse([]accounts.UsageWindow{shortWindow(50, 9000)}, "", false); fuse != nil {
		t.Fatalf("a 5h window produced a fuse reading: %+v", fuse)
	}
}

// ---------------------------------------------------------------------------
// State classification
// ---------------------------------------------------------------------------

func classifyFor(t *testing.T, provider accounts.Provider, windows []accounts.UsageWindow) (string, poolRow) {
	t.Helper()
	status := AccountUsageStatus{
		AccountStatus: AccountStatus{ID: "acct", Provider: provider, AuthMode: accounts.AuthModeOAuth, AuthValid: true},
		Windows:       windows,
		Normalized:    &NormalizedUsage{Fresh: true},
	}
	scheduler := schedulerFromStatuses([]AccountUsageStatus{status}, time.Now())
	row := poolRow{Provider: string(provider), ID: "acct"}
	applyPoolReadings(&row, status, scheduler, true, time.Now())
	return row.State, row
}

func TestClassifyPoolStates(t *testing.T) {
	cases := []struct {
		name     string
		provider accounts.Provider
		windows  []accounts.UsageWindow
		want     string
	}{
		{
			// An untouched Codex account. Reads identically to a fresh one on a
			// headroom meter; the board must say the fuse is unlit.
			name:     "codex untouched is pristine",
			provider: accounts.ProviderCodex,
			windows:  []accounts.UsageWindow{weeklyWindow(0, 604800)},
			want:     poolStatePristine,
		},
		{
			name:     "codex primed is ok, not pristine",
			provider: accounts.ProviderCodex,
			windows:  []accounts.UsageWindow{weeklyWindow(2, 600000)},
			want:     poolStateOK,
		},
		{
			name:     "codex spent is exhausted on the week",
			provider: accounts.ProviderCodex,
			windows:  []accounts.UsageWindow{weeklyWindow(100, 300000)},
			want:     poolStateWeek,
		},
		{
			// Inside the 24h roll window: use-it-or-lose-it.
			name:     "codex near roll is draining",
			provider: accounts.ProviderCodex,
			windows:  []accounts.UsageWindow{weeklyWindow(40, 30000)},
			want:     poolStateDrain,
		},
		{
			name:     "claude healthy",
			provider: accounts.ProviderClaude,
			windows:  []accounts.UsageWindow{shortWindow(5, 9000), claudeWeekly(20, 500000)},
			want:     poolStateOK,
		},
		{
			name:     "claude short window at zero",
			provider: accounts.ProviderClaude,
			windows:  []accounts.UsageWindow{shortWindow(100, 6000), claudeWeekly(20, 500000)},
			want:     poolStateShort,
		},
		{
			name:     "claude weekly at zero",
			provider: accounts.ProviderClaude,
			windows:  []accounts.UsageWindow{shortWindow(10, 9000), claudeWeekly(100, 500000)},
			want:     poolStateWeek,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := classifyFor(t, tc.provider, tc.windows)
			if got != tc.want {
				t.Fatalf("state = %q, want %q", got, tc.want)
			}
		})
	}
}

// A Claude account whose usage reading has no `five_hour` bucket has no 5h
// window to blame when it dies: its week is spent, which nothing refills until
// the roll. Read off its meter or off an upstream 429's exhaustion mark (which
// zeroes every level), and with no 5h column standing in for a meter.
func TestClaudeWithNoFiveHourReadingDiesOnItsWeek(t *testing.T) {
	fable := func(used float64) accounts.UsageWindow {
		return accounts.UsageWindow{Name: "oauth-apps-weekly", Feature: "claude-fable", UsedPercent: used, LimitWindowSeconds: 604800, ResetAfterSeconds: 50000}
	}
	cases := []struct {
		name    string
		weekly  float64
		marked  bool
		wantSub string
	}{
		{"meter at 100", 100, false, "weekly window at zero"},
		{"429 mark on a 99 reading", 99, true, "weekly window at zero"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status := AccountUsageStatus{
				AccountStatus: AccountStatus{ID: "pixel", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, AuthValid: true},
				Windows:       []accounts.UsageWindow{claudeWeekly(tc.weekly, 50000), fable(100)},
				Normalized:    &NormalizedUsage{Fresh: true},
			}
			ref := selectacct.NewSchedulerRef(schedulerFromStatuses([]AccountUsageStatus{status}, time.Now()))
			if tc.marked {
				ref.MarkExhaustedUntil(accounts.ProviderClaude, "pixel", "", time.Now().Add(time.Hour))
			}
			row := poolRow{Provider: string(accounts.ProviderClaude), ID: "pixel"}
			applyPoolReadings(&row, status, ref.Get(), true, time.Now())
			if row.State != poolStateWeek || !strings.Contains(row.StateNote, tc.wantSub) {
				t.Fatalf("state = %q (%s), want %q with %q", row.State, row.StateNote, poolStateWeek, tc.wantSub)
			}
			if row.ShortLeft != nil {
				t.Fatalf("short_left = %v, want no 5h figure for an account with no 5h window", *row.ShortLeft)
			}
		})
	}
}

// The laptop's main: no 5h reading, 4% of its week left days from the roll,
// and a 5% owner holdout. The reserve is what refuses it, and the note must
// say so rather than name a floor that does not apply.
func TestBurnNoteNamesTheReserveWhenThereIsNoFiveHourTiming(t *testing.T) {
	status := AccountUsageStatus{
		AccountStatus: AccountStatus{ID: "main", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, AuthValid: true},
		Windows: []accounts.UsageWindow{
			claudeWeekly(96, 3*24*60*60),
			{Name: "oauth-apps-weekly", Feature: "claude-fable", UsedPercent: 0, LimitWindowSeconds: 604800, ResetAfterSeconds: 3 * 24 * 60 * 60},
		},
		Normalized: &NormalizedUsage{Fresh: true},
	}
	scheduler := schedulerFromStatuses([]AccountUsageStatus{status}, time.Now()).WithReserves(map[string]float64{"main": 0.05})
	row := poolRow{Provider: string(accounts.ProviderClaude), ID: "main"}
	applyPoolReadings(&row, status, scheduler, true, time.Now())
	if row.State != poolStateBurn || !strings.Contains(row.StateNote, "owner's weekly reserve") {
		t.Fatalf("state = %q (%s), want %q naming the owner's reserve", row.State, row.StateNote, poolStateBurn)
	}
}

// The measured Claude account from the operator's own pool: 42% of the 5h
// window left with 3h19m to go. It is NOT exhausted, so it reads healthy on
// every existing tool -- but the burn projection refuses it for new sessions,
// and calling that "ok" is the specific misreport this board exists to prevent.
// It must equally not read as exhausted: a pin to a burn account genuinely
// holds, and refusing one would be the opposite error.
func TestClassifyBurnIsNeitherOKNorExhausted(t *testing.T) {
	state, row := classifyFor(t, accounts.ProviderClaude, []accounts.UsageWindow{
		shortWindow(58, 11933),
		claudeWeekly(65, 562732),
	})
	if state != poolStateBurn {
		t.Fatalf("state = %q, want %q", state, poolStateBurn)
	}
	if !strings.Contains(row.StateNote, "burn projection") {
		t.Fatalf("burn note does not show the projection: %q", row.StateNote)
	}
	if !strings.Contains(row.StateNote, "a pin here still holds") {
		t.Fatalf("burn note must say the pin holds, got %q", row.StateNote)
	}
	decidePinnable(&poolRow{Pools: []string{poolNameLocal}, Measured: true, Fresh: true, State: state})
	target := poolRow{Pools: []string{poolNameLocal}, Measured: true, Fresh: true, State: state}
	decidePinnable(&target)
	if !target.Pinnable {
		t.Fatalf("a burn account must remain a legal pin target: %q", target.PinRefusal)
	}
}

// withNormalizedUsage omits the normalized block for an unmeasured account on
// purpose. Absence is not health: it must render unknown and must not be
// offered as a pin target.
func TestUnmeasuredRendersUnknownAndIsNotPinnable(t *testing.T) {
	status := AccountUsageStatus{
		AccountStatus: AccountStatus{ID: "never-scored", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
	}
	row := poolRow{Provider: "codex", ID: "never-scored", Pools: []string{poolNameLocal}}
	applyPoolReadings(&row, status, selectacct.NewScheduler(nil), true, time.Now())
	if row.State != poolStateUnknown {
		t.Fatalf("state = %q, want unknown", row.State)
	}
	if row.Headroom != nil || row.ShortLeft != nil {
		t.Fatal("an unmeasured row published headroom numbers")
	}
	decidePinnable(&row)
	if row.Pinnable {
		t.Fatal("an unmeasured account was offered as a pin target")
	}

	// A carried seed is measured but not evidence, and must read the same way.
	seeded := AccountUsageStatus{
		AccountStatus: AccountStatus{ID: "seeded", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
		Windows:       []accounts.UsageWindow{weeklyWindow(0, 604800)},
		Normalized:    &NormalizedUsage{Fresh: false},
	}
	seedRow := poolRow{Provider: "codex", ID: "seeded", Pools: []string{poolNameLocal}}
	applyPoolReadings(&seedRow, seeded, schedulerFromStatuses([]AccountUsageStatus{seeded}, time.Now()), true, time.Now())
	if seedRow.State != poolStateUnknown {
		t.Fatalf("a non-fresh score rendered %q, want unknown", seedRow.State)
	}
	decidePinnable(&seedRow)
	if seedRow.Pinnable {
		t.Fatal("a stale-scored account was offered as a pin target")
	}
}

// The untagged-window trap: upstream marshals `UsedPercent`, not `used_percent`.
// A row decoded from the peer must still produce correct snake_case output, and
// must not silently read every account at zero.
func TestPeerWindowsDecodeThroughUntaggedNames(t *testing.T) {
	wire := `[{"id":"peer-acct","provider":"codex","auth_mode":"oauth","auth_valid":true,
	  "windows":[{"Name":"primary","UsedPercent":25,"LimitWindowSeconds":604800,"ResetAfterSeconds":300000,"Feature":""}],
	  "normalized":{"headroom":0.75,"short_headroom":0.75,"fresh":true}}]`
	var statuses []AccountUsageStatus
	if err := json.Unmarshal([]byte(wire), &statuses); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(statuses) != 1 || len(statuses[0].Windows) != 1 {
		t.Fatalf("decoded %d statuses", len(statuses))
	}
	if statuses[0].Windows[0].UsedPercent != 25 {
		t.Fatalf("UsedPercent = %v; the capitalized wire name did not bind", statuses[0].Windows[0].UsedPercent)
	}
	row := poolRow{Provider: "codex", ID: "peer-acct", Pools: []string{poolNameBox}}
	applyPoolReadings(&row, statuses[0], schedulerFromStatuses(statuses, time.Now()), true, time.Now())
	if row.Headroom == nil || *row.Headroom != 0.75 {
		t.Fatalf("headroom = %v, want 0.75 -- a zero here is the silent-failure mode", row.Headroom)
	}
	body, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"used_percent"`, `"window_seconds"`, `"reset_seconds"`} {
		if !strings.Contains(string(body), key) {
			t.Fatalf("projected row is missing %s: %s", key, body)
		}
	}
	if strings.Contains(string(body), `"UsedPercent"`) {
		t.Fatalf("provider vocabulary leaked into the published row: %s", body)
	}
}

// The binding window is the cheapest fix for the headroom:0 / short_headroom:1
// confusion -- it names the window actually refusing traffic.
func TestBindingWindowNamesTheBottleneck(t *testing.T) {
	name := bindingWindowName([]accounts.UsageWindow{shortWindow(10, 9000), claudeWeekly(97, 500000)})
	if name != "7d" {
		t.Fatalf("binding window = %q, want 7d", name)
	}
	name = bindingWindowName([]accounts.UsageWindow{shortWindow(99, 9000), claudeWeekly(10, 500000)})
	if name != "5h" {
		t.Fatalf("binding window = %q, want 5h", name)
	}
}

// ---------------------------------------------------------------------------
// Dedup: distinct accounts, not loaded instances
// ---------------------------------------------------------------------------

// An account loaded on both daemons is ONE upstream meter seen twice. Counting
// it twice is the difference between reporting 24 account-weeks of budget and
// 26 that do not exist.
func TestRollupDedupesSharedAccounts(t *testing.T) {
	rows := []poolRow{
		{Provider: "codex", ID: "shared", Pools: []string{poolNameLocal, poolNameBox}, Shared: true, State: poolStateOK},
		{Provider: "codex", ID: "local-only", Pools: []string{poolNameLocal}, State: poolStateOK},
		{Provider: "claude", ID: "box-only", Pools: []string{poolNameBox}, State: poolStateWeek},
	}
	rollup := poolRollupFrom(rows)
	if rollup.Accounts != 3 {
		t.Fatalf("accounts = %d, want 3 distinct", rollup.Accounts)
	}
	if rollup.Instances != 4 {
		t.Fatalf("instances = %d, want 4 loaded", rollup.Instances)
	}
	if rollup.Shared != 1 {
		t.Fatalf("shared = %d, want 1", rollup.Shared)
	}
	if rollup.TotalLocal != 2 || rollup.ReadyLocal != 2 {
		t.Fatalf("local = %d/%d, want 2/2", rollup.ReadyLocal, rollup.TotalLocal)
	}
	if rollup.TotalBox != 2 || rollup.ReadyBox != 1 {
		t.Fatalf("box = %d/%d, want 1/2", rollup.ReadyBox, rollup.TotalBox)
	}
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func poolTestServer(t *testing.T, localAccounts []accounts.Account) Server {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatalf("session store: %v", err)
	}
	return Server{
		Accounts:       localAccounts,
		Sessions:       store,
		ActiveSessions: NewActiveSessions(),
		Lifecycle:      NewLifecycle(),
		Scheduler: selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "local-codex", Provider: accounts.ProviderCodex, Headroom: 1, ShortHeadroom: 1, WeeklyOnly: true, Fresh: true},
			{AccountID: "dry-codex", Provider: accounts.ProviderCodex, Headroom: 0, ShortHeadroom: 0, WeeklyOnly: true, Fresh: true},
		}),
		AllowUnauthenticatedAdmin: true,
	}
}

// loopbackRequest is the board's own request: a loopback peer naming this
// machine, as the page does when it is opened at 127.0.0.1:31415. The Host is
// required since PATCHES §29; httptest's default, example.com, is a foreign
// name.
func loopbackRequest(method, target string, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "127.0.0.1:31415"
	return req
}

func TestPoolPinWritesStickyAssignment(t *testing.T) {
	server := poolTestServer(t, []accounts.Account{
		{ID: "local-codex", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
	})
	rec := httptest.NewRecorder()
	server.handlePoolPin(rec, loopbackRequest(http.MethodPost, "/_subrouter/pool/pin",
		`{"account_id":"local-codex","sessions":[{"agent_type":"codex","session_id":"abc"}]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	assignment, ok := server.Sessions.Get("codex", "abc")
	if !ok {
		t.Fatal("no assignment written")
	}
	if assignment.AccountID != "local-codex" {
		t.Fatalf("account = %q", assignment.AccountID)
	}
}

// Pinning a local session to a peer account writes a row findAccount cannot
// resolve; selection then picks locally and overwrites it on the same turn.
// That is where the existing orphans came from, so the board refuses to make
// more of them.
func TestPoolPinRefusesCrossPoolTarget(t *testing.T) {
	server := poolTestServer(t, []accounts.Account{
		{ID: "local-codex", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
	})
	rec := httptest.NewRecorder()
	server.handlePoolPin(rec, loopbackRequest(http.MethodPost, "/_subrouter/pool/pin",
		`{"account_id":"box-only","sessions":[{"agent_type":"codex","session_id":"abc"}]}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "spill") {
		t.Fatalf("refusal should point at remote-pool spill: %s", rec.Body.String())
	}
	if _, ok := server.Sessions.Get("codex", "abc"); ok {
		t.Fatal("a cross-pool pin was written anyway")
	}
}

// A pin to an exhausted account is self-erasing: reuseStickyAssignment drops it
// and selection overwrites the row. Accepting it would show a switch that
// silently did not happen.
func TestPoolPinRefusesExhaustedTarget(t *testing.T) {
	server := poolTestServer(t, []accounts.Account{
		{ID: "dry-codex", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
	})
	rec := httptest.NewRecorder()
	server.handlePoolPin(rec, loopbackRequest(http.MethodPost, "/_subrouter/pool/pin",
		`{"account_id":"dry-codex","sessions":[{"agent_type":"codex","session_id":"abc"}]}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if _, ok := server.Sessions.Get("codex", "abc"); ok {
		t.Fatal("a pin to an exhausted account was written")
	}
}

func TestPoolUnpinReleasesSession(t *testing.T) {
	server := poolTestServer(t, []accounts.Account{
		{ID: "local-codex", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
	})
	if _, err := server.Sessions.Put("codex", "abc", "local-codex", ""); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rec := httptest.NewRecorder()
	server.handlePoolUnpin(rec, loopbackRequest(http.MethodPost, "/_subrouter/pool/unpin",
		`{"sessions":[{"agent_type":"codex","session_id":"abc"}]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if _, ok := server.Sessions.Get("codex", "abc"); ok {
		t.Fatal("assignment survived unpin")
	}
}

// Prune's SELECTION PREDICATE is the thing that has to be right, because it
// drives a destructive write. It removes rows pointing at accounts this daemon
// does not hold -- verifiable against the live account list -- and nothing else.
// In particular it must not touch a healthy pin however old the row is: there is
// no last-turn stamp in this process, so age carries no information about use.
func TestPoolPruneRemovesOrphansOnly(t *testing.T) {
	server := poolTestServer(t, []accounts.Account{
		{ID: "local-codex", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
	})
	if _, err := server.Sessions.Put("codex", "keep", "local-codex", ""); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := server.Sessions.Put("codex", "orphan", "gone-from-this-daemon", ""); err != nil {
		t.Fatalf("Put: %v", err)
	}

	dry := httptest.NewRecorder()
	server.handlePoolPrune(dry, loopbackRequest(http.MethodPost, "/_subrouter/pool/prune", `{"dry_run":true}`))
	var preview poolMutationResponse
	if err := json.Unmarshal(dry.Body.Bytes(), &preview); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if preview.OK != 1 || !preview.DryRun {
		t.Fatalf("preview = %+v, want 1 row and dry_run", preview)
	}
	if _, ok := server.Sessions.Get("codex", "orphan"); !ok {
		t.Fatal("dry run deleted a row")
	}

	rec := httptest.NewRecorder()
	server.handlePoolPrune(rec, loopbackRequest(http.MethodPost, "/_subrouter/pool/prune", `{}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if _, ok := server.Sessions.Get("codex", "orphan"); ok {
		t.Fatal("orphan survived prune")
	}
	if _, ok := server.Sessions.Get("codex", "keep"); !ok {
		t.Fatal("prune deleted a pin to an account this daemon holds")
	}
}

// An orphan with work in flight is left alone: its turn resolved before the
// prune ran and the row may be rewritten underneath us.
func TestPoolPruneSkipsLiveSessions(t *testing.T) {
	server := poolTestServer(t, []accounts.Account{
		{ID: "local-codex", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
	})
	if _, err := server.Sessions.Put("codex", "busy", "gone-from-this-daemon", ""); err != nil {
		t.Fatalf("Put: %v", err)
	}
	release := server.ActiveSessions.Begin("codex", "busy")
	defer release()

	rec := httptest.NewRecorder()
	server.handlePoolPrune(rec, loopbackRequest(http.MethodPost, "/_subrouter/pool/prune", `{}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if _, ok := server.Sessions.Get("codex", "busy"); !ok {
		t.Fatal("prune deleted a session with work in flight")
	}
}

// The three mutating routes take the same loopback gate handleDrain uses. A
// bearer token proves possession of a secret, not presence at the machine.
func TestPoolMutationsAreLoopbackOnly(t *testing.T) {
	server := poolTestServer(t, nil)
	handler := server.Handler()
	for _, path := range []string{"/_subrouter/pool/pin", "/_subrouter/pool/unpin", "/_subrouter/pool/prune"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.RemoteAddr = "100.64.10.6:41000"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s from the tailnet returned %d, want 403", path, rec.Code)
		}
	}
}

// The page is served with nothing interpolated, and the peer's bearer token
// must never appear in it. This is the one output a browser renders verbatim.
func TestPoolPageServesEmbeddedHTMLWithoutSecrets(t *testing.T) {
	server := poolTestServer(t, nil)
	server.RemotePool = NewRemotePool("http://100.64.10.6:31415", "super-secret-peer-token", "")
	rec := httptest.NewRecorder()
	server.handlePoolPage(rec, loopbackRequest(http.MethodGet, "/_subrouter/pool", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "SWITCH") {
		t.Fatal("page did not render")
	}
	if strings.Contains(body, "super-secret-peer-token") {
		t.Fatal("the peer token reached the browser")
	}
	if strings.Contains(body, "http://") && strings.Contains(body, "100.64.10.6") {
		t.Fatal("the page contains a peer URL; the browser must never talk to the box")
	}
}

// A peer that stops answering must render as stale, never as an empty pool:
// "the box is empty" and "the box did not answer" are opposite operational
// facts, and only one of them means stop routing there.
func TestBoardKeepsLastGoodPeerReadingWhenUnreachable(t *testing.T) {
	payload := `[{"id":"box-codex","provider":"codex","auth_mode":"oauth","auth_valid":true,
	  "windows":[{"Name":"primary","UsedPercent":10,"LimitWindowSeconds":604800,"ResetAfterSeconds":500000}],
	  "normalized":{"headroom":0.9,"short_headroom":0.9,"fresh":true}}]`
	alive := true
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !alive {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		if r.URL.Path == poolPeerUsagePath {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(payload))
			return
		}
		_, _ = w.Write([]byte(`{"draining":false,"active_proxy_requests":3}`))
	}))
	defer peer.Close()

	server := poolTestServer(t, nil)
	server.RemotePool = NewRemotePool(peer.URL, "token", "")

	board := server.poolBoard(t.Context(), "", "")
	if !board.Box.Reachable {
		t.Fatalf("peer should be reachable: %s", board.Box.Error)
	}
	if len(board.Rows) != 1 || board.Rows[0].ID != "box-codex" {
		t.Fatalf("rows = %+v", board.Rows)
	}
	firstAsOf := board.Box.AsOf

	// Kill the peer and expire the cache so the next board really re-fetches.
	alive = false
	server.RemotePool.admin.mu.Lock()
	for path, entry := range server.RemotePool.admin.entries {
		entry.at = time.Now().Add(-time.Hour)
		server.RemotePool.admin.entries[path] = entry
	}
	server.RemotePool.admin.mu.Unlock()

	board = server.poolBoard(t.Context(), "", "")
	if board.Box.Reachable {
		t.Fatal("a dead peer reported reachable")
	}
	if len(board.Rows) != 1 || board.Rows[0].ID != "box-codex" {
		t.Fatalf("an unreachable peer rendered as an empty pool: %+v", board.Rows)
	}
	if board.Box.AsOf.After(firstAsOf) {
		t.Fatal("stale data was stamped with a fresh timestamp")
	}
}

// An in-flight count nobody read is unknown, never 0. The box's comes from its
// drain-status, which its public listener 404s for every token, so a laptop
// peered over https read "0 in flight" for a box that was serving -- the same
// shape as "the box is empty" for "the box did not answer", one level up.
func TestBoardBoxInFlightIsUnknownUnlessDrainStatusWasRead(t *testing.T) {
	payload := `[{"id":"box-codex","provider":"codex","auth_mode":"oauth","auth_valid":true,
	  "windows":[{"Name":"primary","UsedPercent":10,"LimitWindowSeconds":604800,"ResetAfterSeconds":500000}],
	  "normalized":{"headroom":0.9,"short_headroom":0.9,"fresh":true}}]`
	var drainServed atomic.Bool
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == poolPeerUsagePath:
			_, _ = w.Write([]byte(payload))
		case r.URL.Path == poolPeerDrainPath && drainServed.Load():
			_, _ = w.Write([]byte(`{"draining":false,"active_proxy_requests":3}`))
		default:
			// What the public listener answers for drain-status.
			http.NotFound(w, r)
		}
	}))
	defer peer.Close()

	server := poolTestServer(t, nil)
	server.RemotePool = NewRemotePool(peer.URL, "token", "")
	expire := func() {
		server.RemotePool.admin.mu.Lock()
		defer server.RemotePool.admin.mu.Unlock()
		for path, entry := range server.RemotePool.admin.entries {
			entry.at = time.Now().Add(-time.Hour)
			server.RemotePool.admin.entries[path] = entry
		}
	}
	published := func(board poolBoard) string {
		raw, err := json.Marshal(board.Box)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	board := server.poolBoard(t.Context(), "", "")
	if !board.Box.Reachable || len(board.Rows) != 1 {
		t.Fatalf("the usage read should have worked: reachable=%v rows=%d (%s)", board.Box.Reachable, len(board.Rows), board.Box.Error)
	}
	if board.Box.ActiveProxyRequests != nil {
		t.Fatalf("box in flight = %d from a drain-status that 404'd; want unknown", *board.Box.ActiveProxyRequests)
	}
	if got := published(board); !strings.Contains(got, `"active_proxy_requests":null`) {
		t.Fatalf("an unread count must publish null, not a number: %s", got)
	}
	// The local panel reads its own lifecycle, and 0 there IS a reading.
	if board.Local.ActiveProxyRequests == nil || *board.Local.ActiveProxyRequests != 0 {
		t.Fatalf("local in flight = %v, want a measured 0", board.Local.ActiveProxyRequests)
	}

	// Calibration: a private-listener peer that serves drain-status is read.
	drainServed.Store(true)
	expire()
	board = server.poolBoard(t.Context(), "", "")
	if board.Box.ActiveProxyRequests == nil || *board.Box.ActiveProxyRequests != 3 {
		t.Fatalf("box in flight = %v, want the 3 its drain-status said", board.Box.ActiveProxyRequests)
	}

	// A failed read keeps the last good body; its count is an old instant, not now.
	drainServed.Store(false)
	expire()
	board = server.poolBoard(t.Context(), "", "")
	if board.Box.ActiveProxyRequests != nil {
		t.Fatalf("box in flight = %d from the last good body of a failed read; want unknown", *board.Box.ActiveProxyRequests)
	}
}

// The operator's live pool, measured off the running daemon. These are the
// shapes the board has to get right on day one, and they pin the distinction
// that matters most on Claude: "dead until the weekly roll, 1.5 days out" and
// "dead for 74 minutes" are the same headroom:0 to every existing tool.
func TestClassifyLivePoolShapes(t *testing.T) {
	cases := []struct {
		name string
		// used percent / reset seconds for the 5h and 7d windows
		fiveUsed, weekUsed   float64
		fiveReset, weekReset int64
		want                 string
	}{
		// 7d spent, 5h completely fresh. Exhausted on the WEEK: nothing here
		// recovers for a day and a half.
		{"weekly dead with a fresh 5h", 0, 100, 259, 128659, poolStateWeek},
		// The mirror: a fifth of the week still unspent, but the 5h window is
		// cooked. Back in ~74 minutes, and calling this "week" would send the
		// operator hunting for capacity they already have.
		{"5h dead with a fifth of the week left", 100, 79, 4458, 506658, poolStateShort},
		// Not exhausted on either window, so every existing tool reads it as
		// available -- but 89% of the 5h window is gone with 2h44m still to
		// run, and the burn projection refuses it for new sessions.
		{"burn: healthy-looking but projected through the wall", 89, 58, 9831, 560631, poolStateBurn},
		{"comfortable", 18, 40, 4432, 423832, poolStateOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, row := classifyFor(t, accounts.ProviderClaude, []accounts.UsageWindow{
				shortWindow(tc.fiveUsed, tc.fiveReset),
				claudeWeekly(tc.weekUsed, tc.weekReset),
			})
			if got != tc.want {
				t.Fatalf("state = %q, want %q (note: %s)", got, tc.want, row.StateNote)
			}
		})
	}
}

// A loaded account with no usage reading must still appear, as unknown. Letting
// it fall off the board is the same error as painting it healthy, just quieter:
// the pool looks smaller than it is and nobody learns which account went unread.
func TestBoardShowsLoadedAccountsWithNoUsageReading(t *testing.T) {
	server := poolTestServer(t, []accounts.Account{
		{ID: "unread", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
	})
	board := server.poolBoard(t.Context(), "", "")
	if len(board.Rows) != 1 {
		t.Fatalf("rows = %d, want the loaded account to still be listed", len(board.Rows))
	}
	row := board.Rows[0]
	if row.ID != "unread" || row.State != poolStateUnknown {
		t.Fatalf("row = %+v, want unread/unknown", row)
	}
	if row.Pinnable {
		t.Fatal("an unmeasured account was offered as a pin target")
	}
	if board.Rollup.TotalLocal != 1 || board.Rollup.ReadyLocal != 0 {
		t.Fatalf("rollup = %+v, want 0/1 ready", board.Rollup)
	}
}

// The board's own dedup: one account on both daemons is one row with two pools.
func TestBoardMergesSharedAccountIntoOneRow(t *testing.T) {
	payload := `[{"id":"local-codex","provider":"codex","auth_mode":"oauth","auth_valid":true,
	  "windows":[{"Name":"primary","UsedPercent":10,"LimitWindowSeconds":604800,"ResetAfterSeconds":500000}],
	  "normalized":{"headroom":0.9,"short_headroom":0.9,"fresh":true}}]`
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == poolPeerUsagePath {
			_, _ = w.Write([]byte(payload))
			return
		}
		_, _ = w.Write([]byte(`{"draining":false,"active_proxy_requests":0}`))
	}))
	defer peer.Close()

	server := poolTestServer(t, []accounts.Account{
		{ID: "local-codex", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
	})
	server.RemotePool = NewRemotePool(peer.URL, "token", "")

	board := server.poolBoard(t.Context(), "", "")
	if len(board.Rows) != 1 {
		t.Fatalf("a shared account produced %d rows, want 1", len(board.Rows))
	}
	row := board.Rows[0]
	if !row.Shared || len(row.Pools) != 2 {
		t.Fatalf("row = %+v, want shared across two pools", row)
	}
	if board.Rollup.Accounts != 1 || board.Rollup.Instances != 2 {
		t.Fatalf("rollup = %+v, want 1 account / 2 instances", board.Rollup)
	}
}

// An owner-keyed Codex account's ID is an opaque hash, and its row carries the
// hash in email too. The board names it, on its row and on each session pinned
// to it, by the label this daemon and its peer publish.
func TestBoardCarriesEachAccountsLabel(t *testing.T) {
	owner := func(c string) string { return "codex-owner-" + strings.Repeat(c, 64) }
	payload := `[{"id":"` + owner("b") + `","label":"owner-b@example.com","email":"` + owner("b") + `",
	  "provider":"codex","auth_mode":"oauth","auth_valid":true,
	  "windows":[{"Name":"primary","UsedPercent":10,"LimitWindowSeconds":604800,"ResetAfterSeconds":500000}],
	  "normalized":{"headroom":0.9,"short_headroom":0.9,"fresh":true}}]`
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == poolPeerUsagePath {
			_, _ = w.Write([]byte(payload))
			return
		}
		_, _ = w.Write([]byte(`{"draining":false,"active_proxy_requests":0}`))
	}))
	defer peer.Close()

	server := poolTestServer(t, []accounts.Account{{ID: owner("a"), Provider: accounts.ProviderCodex,
		AuthMode: accounts.AuthModeOAuth, Label: "owner-a@example.com", Email: owner("a")}})
	server.RemotePool = NewRemotePool(peer.URL, "token", "")
	for sessionID, accountID := range map[string]string{"here": owner("a"), "there": owner("b"), "nowhere": "gone-from-this-daemon"} {
		if _, err := server.Sessions.Put("codex", sessionID, accountID, ""); err != nil {
			t.Fatal(err)
		}
	}

	board := server.poolBoard(t.Context(), "", "")
	labels := map[string]string{}
	for _, row := range board.Rows {
		labels[row.ID] = row.Label
	}
	want := map[string]string{owner("a"): "owner-a@example.com", owner("b"): "owner-b@example.com"}
	if !maps.Equal(labels, want) {
		t.Fatalf("labels by ID = %v, want %v: this daemon's account and the peer's", labels, want)
	}
	sessionLabels := map[string]string{}
	for _, entry := range board.Sessions {
		sessionLabels[entry.SessionID] = entry.AccountLabel
	}
	wantSessions := map[string]string{"here": "owner-a@example.com", "there": "owner-b@example.com", "nowhere": ""}
	if !maps.Equal(sessionLabels, wantSessions) {
		t.Fatalf("account labels by session = %v, want %v: pinned here, pinned to the peer's account, pinned to no account", sessionLabels, wantSessions)
	}
}

// A usage-based seat's spend cap (PATCHES.md §42) is not a tumbling fuse. Read
// as one, the nominal 30-day length against a real reset_at made a 31-day
// month's first day read PRISTINE ("nothing is at risk yet") with 40% of the
// cap spent, and mid-month it showed unspent billed cap as Forfeit -- budget
// "lost" that is in fact money not spent.
func TestSpendCapIsNotAFuse(t *testing.T) {
	spend := func(usedPercent float64, resetAfter int64) accounts.UsageWindow {
		return accounts.UsageWindow{Name: accounts.CodexSpendWindowName, UsedPercent: usedPercent, LimitWindowSeconds: 30 * 24 * 60 * 60, ResetAfterSeconds: resetAfter}
	}
	// Oct 1 06:00Z, cap resets Nov 1 00:00Z: 2656800s, more than 30 days.
	state, row := classifyFor(t, accounts.ProviderCodex, []accounts.UsageWindow{spend(40, 2656800)})
	if state == poolStatePristine {
		t.Fatalf("a cap 40%% spent reads %q: %s", state, row.StateNote)
	}
	if state != poolStateOK {
		t.Fatalf("state = %q (%s), want %q", state, row.StateNote, poolStateOK)
	}
	for _, windows := range [][]accounts.UsageWindow{{spend(40, 2656800)}, {spend(10, 15*24*60*60)}} {
		if fuse := readFuse(windows, "", false); fuse != nil {
			t.Fatalf("spend window %+v read as a fuse: %+v", windows[0], fuse)
		}
	}
}

// A box row reaches the laptop's board through the box's usage-status, which
// re-anchors resets to the moment it answers (withWeeklyCooked, upstream
// #465). An untouched Codex weekly reads reset == length, and its ResetAt is
// only fetch time plus the window, so re-anchoring it made the board draw a
// pristine box account as RUNNING the moment the reading was a second old.
func TestFuseOfAnUntouchedWindowStaysPristineThroughUsageStatus(t *testing.T) {
	fetchedAt := time.Now().Add(-5 * time.Second)
	untouched := accounts.UsageWindow{
		Name: "secondary", LimitWindowSeconds: 604800, ResetAfterSeconds: 604800,
		ResetAt: fetchedAt.Add(604800 * time.Second),
	}
	served := withWeeklyCooked([]AccountUsageStatus{{
		AccountStatus: AccountStatus{ID: "box@example.com", Provider: accounts.ProviderCodex},
		Windows:       []accounts.UsageWindow{untouched},
	}})
	_, row := classifyFor(t, accounts.ProviderCodex, served[0].Windows)
	if row.Fuse == nil || row.Fuse.State != poolFusePristine {
		t.Fatalf("fuse = %+v, want an untouched window to stay pristine through usage-status", row.Fuse)
	}
	// A running window still counts down to now.
	running := untouched
	running.ResetAfterSeconds = 3600
	running.ResetAt = fetchedAt.Add(time.Hour)
	served = withWeeklyCooked([]AccountUsageStatus{{Windows: []accounts.UsageWindow{running}}})
	if got := served[0].Windows[0].ResetAfterSeconds; got >= 3600 || got < 3590 {
		t.Fatalf("running window reset = %d, want it re-anchored to now", got)
	}
}

// The board and the router agree on what a weekly window is: at least six
// days (selectacct's isWeeklyWindow since upstream #379). A daily cap is no
// weekly fuse.
func TestFuseIgnoresADailyCap(t *testing.T) {
	daily := accounts.UsageWindow{Name: "daily", UsedPercent: 30, LimitWindowSeconds: 86400, ResetAfterSeconds: 3600}
	if fuse := readFuse([]accounts.UsageWindow{daily}, "", false); fuse != nil {
		t.Fatalf("daily cap read as a weekly fuse: %+v", fuse)
	}
	weekly := weeklyWindow(10, 302400)
	if fuse := readFuse([]accounts.UsageWindow{daily, weekly}, "", false); fuse == nil || fuse.WindowSeconds != 604800 {
		t.Fatalf("fuse = %+v, want the weekly window beside the daily cap", fuse)
	}
}
