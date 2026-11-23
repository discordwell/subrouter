package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// PATCHES §93.1: the mode, the two moments, the owner's under-1-percent
// condition, which reset, the decision and shadow mode. Every fake here fails
// the test on a claim: no test sends one.

func setAutoResetForTest(t *testing.T, mode AutoResetMode, nearEmpty bool) {
	t.Helper()
	prev := autoResetConfig.Load()
	SetAutoReset(mode, nearEmpty)
	t.Cleanup(func() { autoResetConfig.Store(prev) })
}

// settle waits for the decisions the daemon started in the background.
func settle(t *testing.T, server Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !server.Lifecycle.WaitBackground(ctx) {
		t.Fatal("the daemon's background work did not finish")
	}
}

func slogJSON(logs *syncBuffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func wouldClaimLines(logs *syncBuffer) []map[string]any {
	return jsonLogLines(logs, autoResetWouldClaimMessage)
}

// Grants as Anthropic's cedar_ember block lists them.
func autoResetWeekGrant(id string, left int, usable bool) map[string]any {
	return map[string]any{"id": id, "resets_total": 1, "resets_left": left, "usable_now": usable, "use_requires_limit": true,
		"ends_at": time.Now().Add(17 * 24 * time.Hour).UTC().Format(time.RFC3339), "clears": []string{"seven_day", "seven_day_overage_included"}}
}

func autoResetFableGrant(id string, usable bool) map[string]any {
	return map[string]any{"id": id, "resets_total": 1, "resets_left": 1, "usable_now": usable, "use_requires_limit": true,
		"ends_at": time.Now().Add(100 * 24 * time.Hour).UTC().Format(time.RFC3339), "clears": []string{"seven_day_overage_included"}}
}

// cedarBlock lists grants with the first one next.
func cedarBlock(grants ...map[string]any) string {
	list := make([]any, 0, len(grants))
	for _, grant := range grants {
		list = append(list, grant)
	}
	block := map[string]any{"eligible": true, "at_limit": false, "grants": list}
	if len(grants) > 0 {
		block["next_grant_id"] = grants[0]["id"]
	}
	raw, _ := json.Marshal(block)
	return string(raw)
}

// claudeWall answers a Messages turn with Anthropic's usage-limit refusal on
// one window: "7d" (the week) or "7d_oi" (the Fable meter).
func claudeWall(window string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		reset := fmt.Sprint(time.Now().Add(3 * 24 * time.Hour).Unix())
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		for _, prefix := range []string{"5h", "7d", "7d_oi"} {
			status := "allowed"
			if prefix == window {
				status = "rejected"
			}
			w.Header().Set("Anthropic-Ratelimit-Unified-"+prefix+"-Status", status)
			w.Header().Set("Anthropic-Ratelimit-Unified-"+prefix+"-Reset", reset)
		}
		w.Header().Set("Retry-After", "3600")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, realisticAnthropic429Body)
	}
}

// autoResetProfile is one Claude profile as the fake Anthropic answers it.
type autoResetProfile struct {
	week, fable float64 // percent used
	block       string  // the cedar_ember block, "" for none
	turn        http.HandlerFunc
}

// autoResetClaude is a daemon holding Claude profiles measured by its own
// usage sweep, in front of a fake Anthropic that answers each profile's
// reading, reset block and turns, and fails the test on anything else: a
// claim, a profile read, a token exchange.
type autoResetClaude struct {
	t        *testing.T
	server   Server
	proxy    *httptest.Server
	logs     *syncBuffer
	mu       sync.Mutex
	profiles map[string]*autoResetProfile
	served   []string
}

func newAutoResetClaude(t *testing.T, profiles map[string]*autoResetProfile) *autoResetClaude {
	t.Helper()
	d := &autoResetClaude{t: t, profiles: profiles, logs: &syncBuffer{}}
	claudeStore := agentclaude.Store{Dir: t.TempDir()}
	for name := range profiles {
		if err := claudeStore.ImportProfileCredential(name, agentclaude.CredentialInfo{
			AccessToken: "tok-" + name, RefreshToken: "refresh-" + name, SubscriptionType: "max",
			ExpiresAt: time.Now().Add(2 * time.Hour).UnixMilli(),
		}); err != nil {
			t.Fatal(err)
		}
		dir := claudeStore.PreferredInstancePath(claudeStore.InstancePath(name))
		if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"`+name+`@example.com"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	profileOf := func(r *http.Request) (string, *autoResetProfile) {
		name := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-")
		d.mu.Lock()
		defer d.mu.Unlock()
		return name, d.profiles[name]
	}
	client := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		respond := func(status int, body string) (*http.Response, error) {
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		}
		_, profile := profileOf(req)
		if req.URL.Path != "/api/oauth/usage" || profile == nil {
			t.Errorf("the fake Anthropic got %s %s; only usage reads are expected (a claim or any other request is a failure)", req.Method, req.URL.Path)
			return respond(http.StatusNotFound, `{}`)
		}
		d.mu.Lock()
		week := time.Now().Add(3 * 24 * time.Hour).UTC().Format(time.RFC3339)
		body := fmt.Sprintf(`{"seven_day":{"utilization":%g,"resets_at":%q},"seven_day_oauth_apps":{"utilization":%g,"resets_at":%q}`,
			profile.week, week, profile.fable, week)
		if profile.block != "" {
			body += `,"cedar_ember":` + profile.block
		}
		d.mu.Unlock()
		return respond(http.StatusOK, body+`}`)
	})}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, profile := profileOf(r)
		d.mu.Lock()
		d.served = append(d.served, name)
		var turn http.HandlerFunc
		if profile != nil {
			turn = profile.turn
		}
		d.mu.Unlock()
		if turn != nil {
			turn(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_`+name+`","type":"message"}`)
	}))
	t.Cleanup(upstream.Close)
	d.server = Server{
		AccountRef:     &AccountRef{store: accounts.CodexStore{Dir: t.TempDir()}, claudeStore: claudeStore, client: client},
		ClaudeUpstream: mustURL(t, upstream.URL),
		SchedulerRef:   selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		Sessions:       newTestSessionStore(t),
		ActiveSessions: NewActiveSessions(),
		Lifecycle:      NewLifecycle(),
		UsageScoreTTL:  time.Hour,
		MaxBodyBytes:   1 << 20,
		Logger:         slogJSON(d.logs),
	}
	d.proxy = httptest.NewServer(d.server.Handler())
	t.Cleanup(d.proxy.Close)
	d.measure()
	return d
}

// measure runs the daemon's usage sweep again now.
func (d *autoResetClaude) measure() {
	d.t.Helper()
	d.server.AccountRef.InvalidateUsageWindowsCache()
	d.server.AccountRef.InvalidateUsageStatusCache()
	ctx := context.Background()
	d.server.updateSchedulerFromUsageStatusesContext(ctx, d.server.AccountRef.UsageStatuses(ctx))
}

func (d *autoResetClaude) set(name string, change func(*autoResetProfile)) {
	d.mu.Lock()
	change(d.profiles[name])
	d.mu.Unlock()
}

// claudeAnswer is what a client got for a turn.
type claudeAnswer struct {
	status int
	header http.Header
	body   string
}

// turn sends one Messages turn for session on model; pin, when set, is the
// caller's own pin on that account.
func (d *autoResetClaude) turn(session, model, pin string) claudeAnswer {
	d.t.Helper()
	req, err := http.NewRequest(http.MethodPost, d.proxy.URL+"/v1/messages",
		strings.NewReader(`{"model":"`+model+`","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		d.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", session)
	if pin != "" {
		req.Header.Set("X-Subrouter-Account-ID", pin)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		d.t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	header := response.Header.Clone()
	header.Del("Date")
	settle(d.t, d.server)
	return claudeAnswer{status: response.StatusCode, header: header, body: string(body)}
}

// pinSession places session on account, as an earlier turn would have.
func (d *autoResetClaude) pinSession(session, account string) {
	d.t.Helper()
	if _, err := d.server.Sessions.Put("claude", session, account, ""); err != nil {
		d.t.Fatal(err)
	}
}

// clearMarks drops the marks and the admission pause a refusal left on
// account, so the next pinned turn reaches it again.
func (d *autoResetClaude) clearMarks(account string) {
	d.server.SchedulerRef.ClearExhausted(accounts.ProviderClaude, account)
	d.server.admissionGate().clearAccount(schedulerAccountProvider(accounts.ProviderClaude), account)
}

func (d *autoResetClaude) decision(account string) (AutoResetDecision, bool) {
	return d.server.LastAutoResetDecision(accounts.ProviderClaude, account)
}

const opusModel = "claude-opus-5-5"

// heldAndSpare is two profiles: held, whose next reset is a week reset read
// before any limit (so not usable now), and spare with room.
func heldAndSpare(heldTurn http.HandlerFunc) map[string]*autoResetProfile {
	return map[string]*autoResetProfile{
		"held":  {week: 95, fable: 20, block: cedarBlock(autoResetWeekGrant("week_reset", 1, false), autoResetFableGrant("fable_reset", false)), turn: heldTurn},
		"spare": {week: 40, fable: 10},
	}
}

// 2 and 8. Shadow, at the wall: a turn on an account that holds a week reset
// meets the weekly usage-limit refusal. The daemon logs one would-claim line
// and keeps the decision, sends no claim, and the turn gets exactly what it
// gets with automatic resets off, and with them real (which in this build
// claims nothing either). Off decides and logs nothing.
func TestShadowLogsAWouldClaimAtTheClaudeWeekWallAndTheTurnIsUnchanged(t *testing.T) {
	answers := map[AutoResetMode]claudeAnswer{}
	for _, mode := range []AutoResetMode{AutoResetOff, AutoResetShadow, AutoResetReal} {
		setAutoResetForTest(t, mode, true)
		d := newAutoResetClaude(t, heldAndSpare(claudeWall("7d")))
		d.pinSession("conversation", "held")
		answers[mode] = d.turn("conversation", opusModel, "")
		d.mu.Lock()
		served := append([]string(nil), d.served...)
		d.mu.Unlock()
		if !reflect.DeepEqual(served, []string{"held", "spare"}) {
			t.Fatalf("mode %s: Anthropic served %v, want held's refusal then spare", mode, served)
		}
		lines := wouldClaimLines(d.logs)
		decision, decided := d.decision("held")
		if mode == AutoResetOff {
			if len(lines) != 0 || decided {
				t.Fatalf("mode off: %d would-claim lines and decision %+v, want neither", len(lines), decision)
			}
			continue
		}
		if len(lines) != 1 {
			t.Fatalf("mode %s: %d would-claim lines, want 1:\n%s", mode, len(lines), d.logs.String())
		}
		line := lines[0]
		for key, want := range map[string]any{"level": "INFO", "account": "held", "provider": "claude", "mode": mode.String(),
			"moment": "wall", "kind": "week", "reset": "week_reset", "left": "0.0%",
			"turn": "moved to another account (spare)"} {
			if line[key] != want {
				t.Fatalf("mode %s: line %s = %v, want %v: %v", mode, key, line[key], want, line)
			}
		}
		if reason, _ := line["reason"].(string); !strings.Contains(reason, "a request met the limit of the week") ||
			!strings.Contains(reason, "a week reset, which gives back the week") {
			t.Fatalf("mode %s: reason %q", mode, reason)
		}
		if !decided || decision.Verdict != AutoResetWouldClaim || decision.Kind != "week" || decision.Moment != "wall" ||
			decision.Turn != "moved to another account (spare)" || decision.Reason != line["reason"] {
			t.Fatalf("mode %s: last decision %+v, want the line's would-claim", mode, decision)
		}
	}
	for _, mode := range []AutoResetMode{AutoResetShadow, AutoResetReal} {
		if !reflect.DeepEqual(answers[mode], answers[AutoResetOff]) {
			t.Fatalf("mode %s answered the turn %+v; with automatic resets off it is %+v", mode, answers[mode], answers[AutoResetOff])
		}
	}
	if answers[AutoResetOff].status != http.StatusOK || !strings.Contains(answers[AutoResetOff].body, "msg_spare") {
		t.Fatalf("calibration: the turn got %+v, want spare's answer", answers[AutoResetOff])
	}
}

// A pinned turn has no failover: the refusal reaches the client, and the wall
// is seen there.
func TestShadowSeesTheWallOfAPinnedTurn(t *testing.T) {
	setAutoResetForTest(t, AutoResetShadow, true)
	d := newAutoResetClaude(t, heldAndSpare(claudeWall("7d")))
	answer := d.turn("pinned", opusModel, "held")
	if answer.status != http.StatusTooManyRequests {
		t.Fatalf("the pinned turn got %d, want the provider's 429", answer.status)
	}
	decision, ok := d.decision("held")
	if !ok || decision.Verdict != AutoResetWouldClaim || decision.Turn != "answered 429" || decision.Kind != "week" {
		t.Fatalf("decision %+v, want a would-claim whose turn was answered 429", decision)
	}
	if lines := wouldClaimLines(d.logs); len(lines) != 1 {
		t.Fatalf("%d would-claim lines, want 1", len(lines))
	}
}

// 3. Shadow, the reading: a fresh reading at 99.6% of the week with a grant
// usable now is a moment, once per reading; 99.4%, or a grant not usable now,
// is none. A moment with no warm session on the account is no demand. The
// pass looks at each reading once.
func TestShadowLogsAWouldClaimOnAReadingAtTheLimit(t *testing.T) {
	setAutoResetForTest(t, AutoResetShadow, true)
	for _, c := range []struct {
		name      string
		week      float64
		usable    bool
		session   bool
		wantLines int
		wantWhy   string
	}{
		{name: "99.6% used, usable now", week: 99.6, usable: true, session: true, wantLines: 1},
		{name: "99.4% used", week: 99.4, usable: true, session: true},
		{name: "not usable now", week: 99.6, usable: false, session: true},
		{name: "no warm session", week: 99.6, usable: true, wantWhy: "no demand"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newAutoResetClaude(t, map[string]*autoResetProfile{
				"held": {week: c.week, fable: 30, block: cedarBlock(autoResetWeekGrant("week_reset", 1, c.usable))},
			})
			if c.session {
				d.pinSession("conversation", "held")
			}
			// Three passes a minute apart over the same reading: it is one
			// moment, decided at the first.
			memory := d.server.AccountRef.sharedAutoResets()
			first := time.Now()
			for pass := 0; pass < 3; pass++ {
				at := first.Add(time.Duration(pass) * time.Minute)
				memory.mu.Lock()
				memory.now = func() time.Time { return at }
				memory.mu.Unlock()
				d.server.autoResetReadings(context.Background())
			}
			if decision, decided := d.decision("held"); decided && !decision.At.Equal(first) {
				t.Fatalf("the reading was decided again at %v; one reading is one moment, at %v", decision.At, first)
			}
			lines := wouldClaimLines(d.logs)
			if len(lines) != c.wantLines {
				t.Fatalf("%d would-claim lines over three passes, want %d:\n%s", len(lines), c.wantLines, d.logs.String())
			}
			decision, decided := d.decision("held")
			switch {
			case c.wantLines == 1:
				if lines[0]["moment"] != "reading" || lines[0]["kind"] != "week" || lines[0]["left"] != "0.4%" || lines[0]["turn"] != nil ||
					!strings.Contains(lines[0]["reason"].(string), "a reading says the week is 99.6% used") {
					t.Fatalf("line %v", lines[0])
				}
			case c.wantWhy != "":
				if !decided || decision.Verdict != AutoResetNoClaim || !strings.Contains(decision.Reason, c.wantWhy) {
					t.Fatalf("decision %+v, want no claim for %q", decision, c.wantWhy)
				}
			case decided:
				t.Fatalf("a reading that is no moment decided %+v", decision)
			}
		})
	}
}

// autoResetFactsFor is a moment on an account with demand, a live credential
// and next as its next reset.
func autoResetFactsFor(moment autoResetMoment, wall autoResetMeter, next autoResetNext, readings map[autoResetMeter]autoResetReading) autoResetFacts {
	return autoResetFacts{moment: moment, wallMeter: wall, demand: true, next: next, readings: readings, nearEmpty: true, now: time.Now()}
}

func readingOf(left float64, age time.Duration) autoResetReading {
	return autoResetReading{left: left, at: time.Now().Add(-age)}
}

// 4. The owner's condition: while it is on, the meter the next reset gives
// back must have under 1% left, known from the wall's own refusal or from a
// reading at most five minutes old. 2% left, unread, or read six minutes ago
// is refused by the condition; with the condition off each is a would-claim.
func TestTheUnderOnePercentCondition(t *testing.T) {
	week := autoResetNext{kind: autoResetKindWeek, id: "week_reset"}
	for _, c := range []struct {
		name     string
		readings map[autoResetMeter]autoResetReading
		want     string
	}{
		{"2% left", map[autoResetMeter]autoResetReading{autoResetWeek: readingOf(0.02, time.Minute)}, "the week has 2.0% left"},
		{"unread", map[autoResetMeter]autoResetReading{}, "the week is unread"},
		{"read six minutes ago", map[autoResetMeter]autoResetReading{autoResetWeek: readingOf(0.004, 6*time.Minute)}, "the week was last read 6m0s ago"},
	} {
		t.Run(c.name, func(t *testing.T) {
			// A Fable wall: the refusal says nothing about the week.
			f := autoResetFactsFor(autoResetAtTheWall, autoResetFable, week, c.readings)
			got := decideAutoReset(f)
			if got.Verdict != AutoResetRefusedByCondition || !strings.HasSuffix(got.Reason, "refused by the under-1-percent condition: "+c.want) {
				t.Fatalf("decision %+v, want refused by the condition: %s", got, c.want)
			}
			f.nearEmpty = false
			if got := decideAutoReset(f); got.Verdict != AutoResetWouldClaim {
				t.Fatalf("condition off: decision %+v, want a would-claim", got)
			}
		})
	}
	// Under 1% by a fresh reading, and the wall's own meter, pass.
	if got := decideAutoReset(autoResetFactsFor(autoResetAtTheWall, autoResetFable, week,
		map[autoResetMeter]autoResetReading{autoResetWeek: readingOf(0.009, 4*time.Minute)})); got.Verdict != AutoResetWouldClaim || got.Left != "0.9%" {
		t.Fatalf("0.9%% left read 4 minutes ago: %+v, want a would-claim", got)
	}
	if got := decideAutoReset(autoResetFactsFor(autoResetAtTheWall, autoResetWeek, week, nil)); got.Verdict != AutoResetWouldClaim || got.Left != "0.0%" {
		t.Fatalf("the week's own wall: %+v, want a would-claim with nothing left", got)
	}

	// The same through the daemon: a Fable wall, a week reset next, the week
	// read at 98% used.
	for _, nearEmpty := range []bool{true, false} {
		setAutoResetForTest(t, AutoResetShadow, nearEmpty)
		d := newAutoResetClaude(t, map[string]*autoResetProfile{
			"held": {week: 98, fable: 99, block: cedarBlock(autoResetWeekGrant("week_reset", 1, true)), turn: claudeWall("7d_oi")},
		})
		d.turn("pinned", "claude-fable-5-2", "held")
		decision, _ := d.decision("held")
		want := AutoResetRefusedByCondition
		if !nearEmpty {
			want = AutoResetWouldClaim
		}
		if decision.Verdict != want || decision.Kind != "week" {
			t.Fatalf("condition on %v: decision %+v, want %s on the week reset", nearEmpty, decision, want)
		}
		if lines := wouldClaimLines(d.logs); (len(lines) == 1) != !nearEmpty {
			t.Fatalf("condition on %v: %d would-claim lines", nearEmpty, len(lines))
		}
	}
}

// 5. Kinds. The condition is judged for the kind of the next reset.
func TestTheNextResetsKindDecides(t *testing.T) {
	setAutoResetForTest(t, AutoResetShadow, true)
	for _, c := range []struct {
		name        string
		week, fable float64
		block       string
		want        AutoResetVerdict
		kind        string
		reason      string
	}{
		{"a Fable reset next, the Fable meter spent", 60, 99, cedarBlock(autoResetFableGrant("fable_reset", false)), AutoResetWouldClaim, "fable",
			"the next reset is a Fable reset, which gives back the Fable meter"},
		{"a week reset next, the week at 40% left", 60, 99, cedarBlock(autoResetWeekGrant("week_reset", 1, true), autoResetFableGrant("fable_reset", false)),
			AutoResetRefusedByCondition, "week", "the week has 40.0% left"},
		{"a week reset next, the week under 1% left", 99.6, 99, cedarBlock(autoResetWeekGrant("week_reset", 1, true), autoResetFableGrant("fable_reset", false)),
			AutoResetWouldClaim, "week", "the next reset is a week reset"},
		{"a kind this daemon does not know", 99.6, 99, cedarBlock(map[string]any{"id": "session_reset", "resets_left": 1, "usable_now": true,
			"clears": []string{"five_hour"}}), AutoResetNoClaim, "unknown",
			"the next reset is of a kind this daemon does not know (clears five_hour); such a reset is never claimed automatically"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newAutoResetClaude(t, map[string]*autoResetProfile{
				"held": {week: c.week, fable: c.fable, block: c.block, turn: claudeWall("7d_oi")},
			})
			d.turn("pinned", "claude-fable-5-2", "held")
			decision, _ := d.decision("held")
			if decision.Verdict != c.want || decision.Kind != c.kind || !strings.Contains(decision.Reason, c.reason) {
				t.Fatalf("decision %+v, want %s kind %s with %q", decision, c.want, c.kind, c.reason)
			}
			if lines := wouldClaimLines(d.logs); (len(lines) == 1) != (c.want == AutoResetWouldClaim) {
				t.Fatalf("%d would-claim lines for %s", len(lines), c.want)
			}
		})
	}
}

// autoResetCodexAccount is one Codex account as the fakes answer it.
type autoResetCodexAccount struct {
	used    float64 // weekly percent used
	reset   int64   // seconds to the weekly reset
	credits []accounts.RateLimitResetCredit
	turn    http.HandlerFunc
}

// autoResetCodex is a Codex daemon over stored accounts, measured through its
// real usage read, in front of fakes that answer each account's reading,
// credit list and turns, and fail the test on a redeem.
type autoResetCodex struct {
	t           *testing.T
	server      Server
	proxy       *httptest.Server
	logs        *syncBuffer
	mu          sync.Mutex
	accounts    map[string]*autoResetCodexAccount
	byAuth      map[string]string
	creditLists int
	served      []string
}

func newAutoResetCodex(t *testing.T, held map[string]*autoResetCodexAccount) *autoResetCodex {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	for id := range held {
		if err := store.SaveStored(proxyStoredOAuthAccount(id, strings.Split(id, "@")[0], time.Now().Add(time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	d := &autoResetCodex{t: t, accounts: held, byAuth: map[string]string{}, logs: &syncBuffer{}}
	stored := storedPoolAccounts(t, store)
	for _, account := range stored {
		d.byAuth[account.AuthorizationHeader()] = account.ID
	}
	accountOf := func(r *http.Request) (string, *autoResetCodexAccount) {
		d.mu.Lock()
		defer d.mu.Unlock()
		id := d.byAuth[r.Header.Get("Authorization")]
		return id, d.accounts[id]
	}
	client := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		respond := func(body []byte) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(string(body))), Request: req}, nil
		}
		_, account := accountOf(req)
		if account == nil {
			t.Errorf("a request %s %s on an unknown credential", req.Method, req.URL.Path)
			return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		switch req.URL.Path {
		case "/backend-api/wham/usage":
			body, _ := json.Marshal(map[string]any{"plan_type": "pro",
				"rate_limit": map[string]any{"limit_reached": account.used >= 100, "primary_window": map[string]any{
					"used_percent": account.used, "limit_window_seconds": 604800, "reset_after_seconds": account.reset}},
				"rate_limit_reset_credits": map[string]any{"available_count": len(account.credits)}})
			return respond(body)
		case "/backend-api/wham/rate-limit-reset-credits":
			d.creditLists++
			body, _ := json.Marshal(map[string]any{"credits": account.credits})
			return respond(body)
		}
		t.Errorf("the fake ChatGPT got %s %s; a redeem or any other request is a failure", req.Method, req.URL.Path)
		return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	})}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, account := accountOf(r)
		d.mu.Lock()
		d.served = append(d.served, id)
		var turn http.HandlerFunc
		if account != nil {
			turn = account.turn
		}
		d.mu.Unlock()
		if turn != nil {
			turn(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		codexEgressWriteCompleted(w, id)
	}))
	t.Cleanup(upstream.Close)
	d.server = Server{
		AccountRef:                NewAccountRef(store, stored, client),
		CodexUpstream:             mustURL(t, upstream.URL),
		Sessions:                  newTestSessionStore(t),
		ActiveSessions:            NewActiveSessions(),
		Lifecycle:                 NewLifecycle(),
		SchedulerRef:              selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		UsageScoreTTL:             time.Hour,
		MaxBodyBytes:              1 << 20,
		CodexOverloadFailover:     withoutCapacityRetry(),
		AllowUnauthenticatedAdmin: true,
		Logger:                    slogJSON(d.logs),
	}.normalized()
	d.proxy = httptest.NewServer(d.server.Handler())
	t.Cleanup(d.proxy.Close)
	d.measure()
	return d
}

func (d *autoResetCodex) measure() {
	d.t.Helper()
	d.server.AccountRef.InvalidateUsageWindowsCache()
	d.server.AccountRef.InvalidateUsageStatusCache()
	ctx := context.Background()
	d.server.updateSchedulerFromUsageStatusesContext(ctx, d.server.AccountRef.UsageStatuses(ctx))
}

func (d *autoResetCodex) pinSession(session, account string) {
	d.t.Helper()
	if _, err := d.server.Sessions.Put("codex", session, account, ""); err != nil {
		d.t.Fatal(err)
	}
}

func (d *autoResetCodex) turn(session string) (int, string) {
	d.t.Helper()
	status, body := codexEgressPost(d.t, d.proxy.URL, session)
	settle(d.t, d.server)
	return status, body
}

func (d *autoResetCodex) decision(account string) (AutoResetDecision, bool) {
	return d.server.LastAutoResetDecision(accounts.ProviderCodex, account)
}

func (d *autoResetCodex) account(id string) accounts.Account {
	d.t.Helper()
	for _, account := range d.server.accountList() {
		if account.ID == id {
			return account
		}
	}
	d.t.Fatalf("no account %s", id)
	return accounts.Account{}
}

// codexWall answers a turn with the usage-limit refusal of a week that
// resets in six days.
func codexWall(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":518400}}`)
}

func autoResetTestCredit(id string, expires time.Duration) accounts.RateLimitResetCredit {
	return accounts.RateLimitResetCredit{ID: id, ResetType: codexRateLimitsResetType, Status: "available",
		ExpiresAt: time.Now().Add(expires).UTC().Format(time.RFC3339)}
}

const (
	heldCodex  = "held@example.com"
	spareCodex = "spare@example.com"
)

// 5, Codex. An account at its usage limit with credits: a would-claim of kind
// codex_credit naming the credit that expires soonest, and the turn moves as
// it does today. A credit of a type the daemon does not know is never claimed.
func TestShadowNamesTheCodexCreditThatExpiresSoonest(t *testing.T) {
	setAutoResetForTest(t, AutoResetShadow, true)
	d := newAutoResetCodex(t, map[string]*autoResetCodexAccount{
		heldCodex: {used: 90, reset: 518400, turn: codexWall,
			credits: []accounts.RateLimitResetCredit{autoResetTestCredit("credit-late", 20*24*time.Hour), autoResetTestCredit("credit-soon", 5*24*time.Hour)}},
		spareCodex: {used: 30, reset: 300000},
	})
	d.pinSession("conversation", heldCodex)
	status, body := d.turn("conversation")
	if status != http.StatusOK || !strings.Contains(body, "served-from-"+spareCodex) {
		t.Fatalf("the turn got %d %.200q, want spare's answer after the failover", status, body)
	}
	decision, ok := d.decision(heldCodex)
	if !ok || decision.Verdict != AutoResetWouldClaim || decision.Kind != "codex_credit" || decision.Reset != "credit-soon" ||
		decision.Moment != "wall" || decision.Left != "0.0%" || decision.Turn != "moved to another account ("+spareCodex+")" {
		t.Fatalf("decision %+v, want a would-claim of credit-soon at the wall", decision)
	}
	lines := wouldClaimLines(d.logs)
	if len(lines) != 1 || lines[0]["reset"] != "credit-soon" || lines[0]["kind"] != "codex_credit" || lines[0]["account"] != heldCodex {
		t.Fatalf("would-claim lines %v", lines)
	}

	unknown := newAutoResetCodex(t, map[string]*autoResetCodexAccount{
		heldCodex: {used: 90, reset: 518400, turn: codexWall, credits: []accounts.RateLimitResetCredit{
			{ID: "credit-other", ResetType: "codex_something_else", Status: "available", ExpiresAt: time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)}}},
		spareCodex: {used: 30, reset: 300000},
	})
	unknown.pinSession("conversation", heldCodex)
	unknown.turn("conversation")
	decision, _ = unknown.decision(heldCodex)
	if decision.Verdict != AutoResetNoClaim || decision.Kind != "unknown" ||
		!strings.Contains(decision.Reason, "does not know (reset_type codex_something_else)") {
		t.Fatalf("decision %+v, want never for an unknown credit type", decision)
	}
	if lines := wouldClaimLines(unknown.logs); len(lines) != 0 {
		t.Fatalf("an unknown credit type logged %v", lines)
	}
}

// 6. Never for an account whose credential is dead. A reading at the limit
// with a credit and a warm session is a would-claim (the calibration); the
// same reading after the account's credential refresh was refused is no
// claim.
func TestNeverForAnAccountWithADeadCredential(t *testing.T) {
	setAutoResetForTest(t, AutoResetShadow, true)
	for _, dead := range []bool{false, true} {
		d := newAutoResetCodex(t, map[string]*autoResetCodexAccount{
			heldCodex: {used: 100, reset: 518400, credits: []accounts.RateLimitResetCredit{autoResetTestCredit("credit", 5*24*time.Hour)}},
		})
		d.pinSession("conversation", heldCodex)
		if dead {
			d.server.AccountRef.noteCredResult(d.account(heldCodex), &accounts.CodexStoredRefreshFailureError{})
		}
		d.server.autoResetReadings(context.Background())
		decision, _ := d.decision(heldCodex)
		lines := wouldClaimLines(d.logs)
		if !dead {
			if decision.Verdict != AutoResetWouldClaim || len(lines) != 1 {
				t.Fatalf("calibration: decision %+v and %d lines, want a would-claim", decision, len(lines))
			}
			continue
		}
		if decision.Verdict != AutoResetNoClaim || !strings.HasPrefix(decision.Reason, "its credential is dead") || len(lines) != 0 {
			t.Fatalf("dead credential: decision %+v and %d lines, want no claim", decision, len(lines))
		}
	}
}

// 6. Never for an account with no reset left: the same Claude wall with the
// week reset already used, against one reset left.
func TestNeverForAnAccountWithNoResetLeft(t *testing.T) {
	setAutoResetForTest(t, AutoResetShadow, true)
	for _, left := range []int{1, 0} {
		d := newAutoResetClaude(t, map[string]*autoResetProfile{
			"held": {week: 95, block: cedarBlock(autoResetWeekGrant("week_reset", left, false)), turn: claudeWall("7d")},
		})
		d.turn("pinned", opusModel, "held")
		decision, _ := d.decision("held")
		lines := wouldClaimLines(d.logs)
		if left == 1 {
			if decision.Verdict != AutoResetWouldClaim || len(lines) != 1 {
				t.Fatalf("calibration: decision %+v and %d lines, want a would-claim", decision, len(lines))
			}
			continue
		}
		if decision.Verdict != AutoResetNoClaim || decision.Reason != "this account's saved reset is already used" || len(lines) != 0 {
			t.Fatalf("no reset left: decision %+v and %d lines, want no claim", decision, len(lines))
		}
	}
}

// 6. Never for the daemon's own tiny request of §90: it is not demand. A
// client's turn meeting the same refusal is a would-claim (the calibration).
func TestNeverForTheDaemonsOwnSelfStartRequest(t *testing.T) {
	setAutoResetForTest(t, AutoResetShadow, true)
	d := newAutoResetCodex(t, map[string]*autoResetCodexAccount{
		heldCodex: {used: 0, reset: 604800, turn: codexWall, credits: []accounts.RateLimitResetCredit{autoResetTestCredit("credit", 5*24*time.Hour)}},
	})
	status, err := d.server.sendSelfStart(context.Background(), d.account(heldCodex), SelfStartDefaultModel, selfStartTimeout)
	if err != nil || status != http.StatusTooManyRequests {
		t.Fatalf("the self-start got %d %v, want the refusal", status, err)
	}
	settle(t, d.server)
	if decision, decided := d.decision(heldCodex); decided {
		t.Fatalf("the daemon's own request decided %+v", decision)
	}
	d.pinSession("conversation", heldCodex)
	d.turn("conversation")
	if decision, _ := d.decision(heldCodex); decision.Verdict != AutoResetWouldClaim {
		t.Fatalf("calibration: a client's turn on the same refusal decided %+v, want a would-claim", decision)
	}
}

// 6. Never for a peer's account. The box's account and the laptop's own read
// the same: the week spent, a credit left, a session on it. The laptop reads
// the box's body, and its pass decides for its own account only.
func TestNeverForAPeersAccount(t *testing.T) {
	setAutoResetForTest(t, AutoResetShadow, true)
	const boxAccount, laptopAccount = "box-held@example.com", "laptop-held@example.com"
	spent := func() *autoResetCodexAccount {
		return &autoResetCodexAccount{used: 100, reset: 518400, credits: []accounts.RateLimitResetCredit{autoResetTestCredit("credit", 5*24*time.Hour)}}
	}
	box := newAutoResetCodex(t, map[string]*autoResetCodexAccount{boxAccount: spent()})
	laptop := newAutoResetCodex(t, map[string]*autoResetCodexAccount{laptopAccount: spent()})
	laptop.server.RemotePool = NewRemotePool(box.proxy.URL, "", "")
	laptop.pinSession("on-the-box", boxAccount)
	laptop.pinSession("on-the-laptop", laptopAccount)
	var board poolBoard
	if err := json.Unmarshal(func() []byte {
		rec := httptest.NewRecorder()
		laptop.server.handlePoolData(rec, loopbackRequest(http.MethodGet, "/_subrouter/pool.json", ""))
		return rec.Body.Bytes()
	}(), &board); err != nil {
		t.Fatal(err)
	}
	ingested := false
	for _, row := range board.Rows {
		ingested = ingested || row.ID == boxAccount
	}
	if !ingested {
		t.Fatalf("calibration: the laptop's board has no row for the box's account: %+v", board.Rows)
	}
	laptop.server.autoResetReadings(context.Background())
	if decision, _ := laptop.decision(laptopAccount); decision.Verdict != AutoResetWouldClaim {
		t.Fatalf("calibration: the laptop's own account decided %+v, want a would-claim", decision)
	}
	if decision, decided := laptop.decision(boxAccount); decided {
		t.Fatalf("the laptop decided for the box's account: %+v", decision)
	}
	for _, line := range wouldClaimLines(laptop.logs) {
		if line["account"] != laptopAccount {
			t.Fatalf("the laptop logged a would-claim for %v", line["account"])
		}
	}
}

// 7. At most one would-claim line per account and kind in ten minutes,
// however many requests meet the wall; each one is still the account's last
// decision. Another kind gets its own line.
func TestOneWouldClaimLinePerAccountAndKindInTenMinutes(t *testing.T) {
	setAutoResetForTest(t, AutoResetShadow, true)
	d := newAutoResetClaude(t, map[string]*autoResetProfile{
		"held": {week: 95, fable: 99, block: cedarBlock(autoResetWeekGrant("week_reset", 1, false)), turn: claudeWall("7d")},
	})
	memory := d.server.AccountRef.sharedAutoResets()
	start := time.Now()
	clock := start
	memory.mu.Lock()
	memory.now = func() time.Time { return clock }
	memory.mu.Unlock()
	at := func(offset time.Duration) {
		memory.mu.Lock()
		clock = start.Add(offset)
		memory.mu.Unlock()
	}
	wall := func(session, model string) {
		d.clearMarks("held")
		if answer := d.turn(session, model, "held"); answer.status != http.StatusTooManyRequests {
			t.Fatalf("turn %s got %d, want held's refusal", session, answer.status)
		}
	}
	for i := 0; i < 5; i++ {
		wall(fmt.Sprintf("pinned-%d", i), opusModel)
	}
	if lines := wouldClaimLines(d.logs); len(lines) != 1 {
		t.Fatalf("five walls logged %d would-claim lines, want 1", len(lines))
	}
	at(9 * time.Minute)
	wall("pinned-late", opusModel)
	if decision, _ := d.decision("held"); !decision.At.Equal(start.Add(9*time.Minute)) || decision.Verdict != AutoResetWouldClaim {
		t.Fatalf("the last decision %+v, want the would-claim at nine minutes", decision)
	}
	if lines := wouldClaimLines(d.logs); len(lines) != 1 {
		t.Fatalf("a wall nine minutes later logged a line: %d lines", len(lines))
	}
	// The next reset becomes a Fable reset: its own kind, its own line.
	d.set("held", func(p *autoResetProfile) {
		p.block, p.turn = cedarBlock(autoResetFableGrant("fable_reset", true)), claudeWall("7d_oi")
	})
	d.measure()
	wall("pinned-fable", "claude-fable-5-2")
	if lines := wouldClaimLines(d.logs); len(lines) != 2 || lines[1]["kind"] != "fable" {
		t.Fatalf("a Fable reset's first wall: lines %v, want a second line of kind fable", lines)
	}
	d.set("held", func(p *autoResetProfile) {
		p.block, p.turn = cedarBlock(autoResetWeekGrant("week_reset", 1, false)), claudeWall("7d")
	})
	d.measure()
	at(11 * time.Minute)
	wall("pinned-later", opusModel)
	if lines := wouldClaimLines(d.logs); len(lines) != 3 || lines[2]["kind"] != "week" {
		t.Fatalf("a week wall eleven minutes after the first line: lines %v, want a third of kind week", lines)
	}
}

// 8. Mode 0: a reading at the limit decides nothing and logs nothing.
func TestModeOffDecidesNothingOnAReading(t *testing.T) {
	setAutoResetForTest(t, AutoResetOff, true)
	d := newAutoResetClaude(t, map[string]*autoResetProfile{
		"held": {week: 100, block: cedarBlock(autoResetWeekGrant("week_reset", 1, true))},
	})
	d.pinSession("conversation", "held")
	d.server.autoResetReadings(context.Background())
	if decision, decided := d.decision("held"); decided || len(wouldClaimLines(d.logs)) != 0 {
		t.Fatalf("mode off decided %+v", decision)
	}
	setAutoResetForTest(t, AutoResetShadow, true)
	d.server.autoResetReadings(context.Background())
	if decision, _ := d.decision("held"); decision.Verdict != AutoResetWouldClaim {
		t.Fatalf("calibration: shadow decided %+v, want a would-claim", decision)
	}
}

// codexWebSocketWall takes a Codex upgrade and answers its first turn with
// the usage-limit event of a week that resets in six days.
func codexWebSocketWall(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	if _, _, err := conn.ReadMessage(); err != nil {
		return
	}
	_ = conn.WriteMessage(websocket.TextMessage,
		[]byte(`{"type":"error","error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":518400}}`))
	_, _, _ = conn.ReadMessage()
}

// 2, the websocket relay: a Codex socket turn that meets the wall is a
// moment. With another account the relay closes the socket for a reconnect,
// without one it delivers the refusal, as today, and the decision says which.
func TestShadowSeesTheWallOfACodexWebSocketTurn(t *testing.T) {
	setAutoResetForTest(t, AutoResetShadow, true)
	for _, c := range []struct {
		name  string
		spare bool
		turn  string
	}{
		{"another account takes the turn", true, autoResetTurnReconnect},
		{"no other account", false, autoResetTurnRefusalDelivered},
	} {
		t.Run(c.name, func(t *testing.T) {
			held := map[string]*autoResetCodexAccount{
				heldCodex: {used: 90, reset: 518400, turn: codexWebSocketWall, credits: []accounts.RateLimitResetCredit{autoResetTestCredit("credit", 5*24*time.Hour)}},
			}
			if c.spare {
				held[spareCodex] = &autoResetCodexAccount{used: 30, reset: 300000}
			}
			d := newAutoResetCodex(t, held)
			d.pinSession("socket", heldCodex)
			conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(d.proxy.URL, "http")+"/v1/responses",
				http.Header{"X-Subrouter-Session": []string{"socket"}})
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			response.Body.Close()
			defer conn.Close()
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)); err != nil {
				t.Fatal(err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			_, body, err := conn.ReadMessage()
			if c.spare != websocket.IsCloseError(err, websocket.CloseTryAgainLater) || (!c.spare && !strings.Contains(string(body), "usage_limit_reached")) {
				t.Fatalf("the socket got %q, %v", body, err)
			}
			settle(t, d.server)
			decision, _ := d.decision(heldCodex)
			if decision.Verdict != AutoResetWouldClaim || decision.Moment != "wall" || decision.Kind != "codex_credit" ||
				decision.Reset != "credit" || decision.Left != "0.0%" || decision.Turn != c.turn {
				t.Fatalf("decision %+v, want a would-claim at the wall whose turn was %q", decision, c.turn)
			}
		})
	}
}
