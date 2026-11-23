package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// PATCHES §93.2: the claim in place, and never twice. The fakes here spend as
// the providers do: a turn meets the usage-limit refusal while its meter is
// spent, and a claim of the next reset gives the meter back. No test sends a
// request to a real provider.

func setAutoResetWaitForTest(t *testing.T, claude, codex time.Duration) {
	t.Helper()
	prevClaude, prevCodex := autoResetClaudeWait, autoResetCodexWait
	autoResetClaudeWait, autoResetCodexWait = claude, codex
	t.Cleanup(func() { autoResetClaudeWait, autoResetCodexWait = prevClaude, prevCodex })
}

// setAutoResetClock moves the daemon's decision clock.
func setAutoResetClock(server Server, now func() time.Time) {
	memory := server.AccountRef.sharedAutoResets()
	memory.mu.Lock()
	memory.now = now
	memory.mu.Unlock()
}

func later(d time.Duration) func() time.Time {
	return func() time.Time { return time.Now().Add(d) }
}

// claudeClaimProfile is one Claude account as the fake Anthropic counts it.
type claudeClaimProfile struct {
	week, fable float64 // percent used
	// grants are its saved resets, next first; true is a Fable reset.
	grants []bool
	// stuck refuses every turn with the weekly limit, whatever the meters.
	stuck bool
	// reorderAtLimit lists the grants last first while at a limit.
	reorderAtLimit bool
}

// claudeClaimFake is a fake Anthropic shared by every daemon that holds its
// profiles. It counts the claims; hold, when set, keeps each claim waiting
// until it is closed; answer, when set, answers a claim instead of using the
// reset.
type claudeClaimFake struct {
	t        *testing.T
	mu       sync.Mutex
	profiles map[string]*claudeClaimProfile
	claims   int
	served   []string
	hold     chan struct{}
	answer   func() (int, string)
	claiming chan struct{}
	// gateFirst, when set, holds the first turn until it is closed.
	gateFirst chan struct{}
	gated     bool
}

func newClaudeClaimFake(t *testing.T, profiles map[string]*claudeClaimProfile) *claudeClaimFake {
	return &claudeClaimFake{t: t, profiles: profiles, claiming: make(chan struct{}, 16)}
}

func (f *claudeClaimFake) set(name string, change func(*claudeClaimProfile)) {
	f.mu.Lock()
	change(f.profiles[name])
	f.mu.Unlock()
}

func (f *claudeClaimFake) counts() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims, append([]string(nil), f.served...)
}

func (f *claudeClaimFake) grantJSON(p *claudeClaimProfile, i int) map[string]any {
	atLimit := p.week >= 100 || p.fable >= 100
	grant := map[string]any{"id": fmt.Sprintf("week_reset_%d", i), "resets_total": 1, "resets_left": 1, "usable_now": atLimit,
		"use_requires_limit": true, "ends_at": time.Now().Add(17 * 24 * time.Hour).UTC().Format(time.RFC3339),
		"clears": []string{"seven_day", "seven_day_overage_included"}}
	if p.grants[i] {
		grant["id"] = fmt.Sprintf("fable_reset_%d", i)
		grant["clears"] = []string{"seven_day_overage_included"}
		grant["ends_at"] = time.Now().Add(100 * 24 * time.Hour).UTC().Format(time.RFC3339)
	}
	return grant
}

// anthropic answers the daemon's own calls: usage reads, the profile, claims.
func (f *claudeClaimFake) anthropic(req *http.Request) (*http.Response, error) {
	respond := func(status int, body string) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	}
	name := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer tok-")
	f.mu.Lock()
	p := f.profiles[name]
	f.mu.Unlock()
	if p == nil {
		f.t.Errorf("the fake Anthropic got %s %s for no profile", req.Method, req.URL.Path)
		return respond(http.StatusNotFound, `{}`)
	}
	switch {
	case req.URL.Path == "/api/oauth/usage":
		f.mu.Lock()
		defer f.mu.Unlock()
		grants := []any{}
		for i := range p.grants {
			grants = append(grants, f.grantJSON(p, i))
		}
		atLimit := p.week >= 100 || p.fable >= 100
		if atLimit && p.reorderAtLimit {
			for i, j := 0, len(grants)-1; i < j; i, j = i+1, j-1 {
				grants[i], grants[j] = grants[j], grants[i]
			}
		}
		block := map[string]any{"eligible": true, "at_limit": atLimit, "grants": grants}
		if len(grants) > 0 {
			block["next_grant_id"] = grants[0].(map[string]any)["id"]
		}
		week := time.Now().Add(3 * 24 * time.Hour).UTC().Format(time.RFC3339)
		body, _ := json.Marshal(map[string]any{"seven_day": map[string]any{"utilization": p.week, "resets_at": week},
			"seven_day_oauth_apps": map[string]any{"utilization": p.fable, "resets_at": week}, "cedar_ember": block})
		return respond(http.StatusOK, string(body))
	case req.URL.Path == "/api/oauth/profile":
		return respond(http.StatusOK, `{"account":{"uuid":"acct-`+name+`-0001","email_address":"`+name+`@example.com"},"organization":{"uuid":"org-`+name+`-0001"}}`)
	case req.Method == http.MethodPost && req.URL.Path == "/api/organizations/org-"+name+"-0001/reset_rate_limits":
		f.mu.Lock()
		f.claims++
		hold, answer := f.hold, f.answer
		f.mu.Unlock()
		f.claiming <- struct{}{}
		if hold != nil {
			select {
			case <-hold:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
		if answer != nil {
			status, body := answer()
			return respond(status, body)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(p.grants) == 0 {
			return respond(http.StatusOK, `{"result":"already_used","resets_left":0}`)
		}
		fable := p.grants[0]
		p.grants = p.grants[1:]
		cleared := `["seven_day","seven_day_overage_included"]`
		if fable {
			p.fable, cleared = 0, `["seven_day_overage_included"]`
		} else {
			p.week, p.fable = 0, 0
		}
		return respond(http.StatusOK, fmt.Sprintf(`{"result":"reset","resets_left":%d,"cleared":%s}`, len(p.grants), cleared))
	}
	f.t.Errorf("the fake Anthropic got %s %s; only usage and profile reads and claims are expected", req.Method, req.URL.Path)
	return respond(http.StatusNotFound, `{}`)
}

// turn answers a Messages turn: the usage-limit refusal of the meter that is
// spent (the week, or for a Fable turn the Fable meter), else a message.
func (f *claudeClaimFake) turn(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-")
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	if gate := f.gateFirst; gate != nil && !f.gated {
		f.gated = true
		f.mu.Unlock()
		<-gate
		f.mu.Lock()
	}
	p := f.profiles[name]
	spent := ""
	switch {
	case p == nil:
	case p.week >= 100 || p.stuck:
		spent = "7d"
	case p.fable >= 100 && strings.Contains(string(body), "claude-fable"):
		spent = "7d_oi"
	}
	status := "200"
	if spent != "" {
		status = "429"
	}
	f.served = append(f.served, name+" "+status)
	f.mu.Unlock()
	if spent != "" {
		claudeWall(spent)(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"msg_`+name+`","type":"message"}`)
}

// claudeClaimDaemon is one daemon (one worker) in front of the fake. Two of
// them with one state directory are the old and the new worker of a swap.
type claudeClaimDaemon struct {
	t      *testing.T
	server Server
	proxy  *httptest.Server
	logs   *syncBuffer
}

func (f *claudeClaimFake) daemon(t *testing.T, stateDir string) *claudeClaimDaemon {
	t.Helper()
	d := &claudeClaimDaemon{t: t, logs: &syncBuffer{}}
	claudeStore := agentclaude.Store{Dir: t.TempDir()}
	for name := range f.profiles {
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
	upstream := httptest.NewServer(http.HandlerFunc(f.turn))
	t.Cleanup(upstream.Close)
	d.server = Server{
		AccountRef:        &AccountRef{store: accounts.CodexStore{Dir: t.TempDir()}, claudeStore: claudeStore, client: &http.Client{Transport: proxyRoundTripFunc(f.anthropic)}},
		ClaudeUpstream:    mustURL(t, upstream.URL),
		SchedulerRef:      selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		Sessions:          newTestSessionStore(t),
		ActiveSessions:    NewActiveSessions(),
		Lifecycle:         NewLifecycle(),
		UsageScoreTTL:     time.Hour,
		MaxBodyBytes:      1 << 20,
		Logger:            slogJSON(d.logs),
		AutoResetStateDir: stateDir,
	}
	d.proxy = httptest.NewServer(d.server.Handler())
	t.Cleanup(d.proxy.Close)
	d.measure()
	return d
}

func (d *claudeClaimDaemon) measure() {
	d.t.Helper()
	d.server.AccountRef.InvalidateUsageWindowsCache()
	d.server.AccountRef.InvalidateUsageStatusCache()
	ctx := context.Background()
	d.server.updateSchedulerFromUsageStatusesContext(ctx, d.server.AccountRef.UsageStatuses(ctx))
}

func (d *claudeClaimDaemon) pinSession(session, account string) {
	d.t.Helper()
	if _, err := d.server.Sessions.Put("claude", session, account, ""); err != nil {
		d.t.Fatal(err)
	}
}

func (d *claudeClaimDaemon) sessionAccount(session string) string {
	assignment, _ := d.server.Sessions.Get("claude", session)
	return assignment.AccountID
}

func (d *claudeClaimDaemon) clearMarks(account string) {
	d.server.SchedulerRef.ClearExhausted(accounts.ProviderClaude, account)
	d.server.admissionGate().clearAccount(schedulerAccountProvider(accounts.ProviderClaude), account)
}

func (d *claudeClaimDaemon) decision(account string) AutoResetDecision {
	decision, _ := d.server.LastAutoResetDecision(accounts.ProviderClaude, account)
	return decision
}

// ask sends one Messages turn and returns what the client got, without
// waiting for the daemon's background work.
func (d *claudeClaimDaemon) ask(session, model string, header http.Header) claudeAnswer {
	req, err := http.NewRequest(http.MethodPost, d.proxy.URL+"/v1/messages",
		strings.NewReader(`{"model":"`+model+`","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		d.t.Error(err)
		return claudeAnswer{}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", session)
	for name, values := range header {
		req.Header[name] = values
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		d.t.Error(err)
		return claudeAnswer{}
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return claudeAnswer{status: response.StatusCode, body: string(body)}
}

// turn is ask, then the daemon's background work settled.
func (d *claudeClaimDaemon) turn(session, model string) claudeAnswer {
	d.t.Helper()
	answer := d.ask(session, model, nil)
	settle(d.t, d.server)
	return answer
}

func waitClaiming(t *testing.T, f *claudeClaimFake) {
	t.Helper()
	select {
	case <-f.claiming:
	case <-time.After(20 * time.Second):
		t.Fatal("no claim reached the fake Anthropic")
	}
}

// heldWeek is an account holding a week reset and then a Fable reset, read
// at 95% of its week, and a spare with room.
func heldWeek() map[string]*claudeClaimProfile {
	return map[string]*claudeClaimProfile{
		"held":  {week: 95, fable: 20, grants: []bool{false, true}},
		"spare": {week: 40, fable: 10},
	}
}

// 1 and 10. A session's turn meets the weekly refusal on an account holding
// a week reset. Real mode claims once, reads usage again and sends the same
// turn again on the same account, which serves it: one answer, no error, the
// session stays, no other account sees the turn. Off and shadow make no
// claim and move the turn, as before.
func TestRealClaimsAtTheClaudeWeekWallAndServesTheTurnInPlace(t *testing.T) {
	for _, mode := range []AutoResetMode{AutoResetOff, AutoResetShadow, AutoResetReal} {
		setAutoResetForTest(t, mode, true)
		f := newClaudeClaimFake(t, heldWeek())
		d := f.daemon(t, "")
		d.pinSession("conversation", "held")
		// Anthropic refuses now; the daemon last read the week at 95%.
		f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
		answer := d.turn("conversation", opusModel)
		claims, served := f.counts()
		if mode != AutoResetReal {
			if claims != 0 || !reflect.DeepEqual(served, []string{"held 429", "spare 200"}) ||
				!strings.Contains(answer.body, "msg_spare") || d.sessionAccount("conversation") != "spare" {
				t.Fatalf("mode %s: %d claims, served %v, answer %+v, session on %q; want no claim and the turn moved to spare",
					mode, claims, served, answer, d.sessionAccount("conversation"))
			}
			continue
		}
		if claims != 1 || !reflect.DeepEqual(served, []string{"held 429", "held 200"}) {
			t.Fatalf("real: %d claims and served %v, want one claim and the turn sent again on held", claims, served)
		}
		if answer.status != http.StatusOK || !strings.Contains(answer.body, "msg_held") || d.sessionAccount("conversation") != "held" {
			t.Fatalf("real: the client got %+v and the session is on %q, want held's answer and the session where it was",
				answer, d.sessionAccount("conversation"))
		}
		decision := d.decision("held")
		if decision.Verdict != AutoResetClaimed || decision.Kind != "week" || decision.Reset != "week_reset_0" ||
			decision.Turn != autoResetTurnWaits || !strings.Contains(decision.Reason, "the week back") {
			t.Fatalf("real: last decision %+v, want the week reset claimed and the week read back", decision)
		}
		if lines := jsonLogLines(d.logs, autoResetFinishedMessage); len(lines) != 1 || lines[0]["verdict"] != "claimed" || lines[0]["level"] != "WARN" {
			t.Fatalf("real: finish lines %v", lines)
		}
	}
}

// 2. Three more turns for the account arrive while the claim runs: two
// sessions' and a new session's. Each waits before anything is sent, and all
// four are served on the account after the claim, which is sent once.
func TestTurnsThatArriveWhileTheClaimRunsWaitOnTheSameAccount(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	f := newClaudeClaimFake(t, map[string]*claudeClaimProfile{"held": {week: 95, grants: []bool{false}}})
	f.hold = make(chan struct{})
	d := f.daemon(t, "")
	for _, session := range []string{"one", "two", "three"} {
		d.pinSession(session, "held")
	}
	f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
	answers := make(chan claudeAnswer, 4)
	go func() { answers <- d.ask("one", opusModel, nil) }()
	waitClaiming(t, f)
	for _, session := range []string{"two", "three", "new"} {
		go func() { answers <- d.ask(session, opusModel, nil) }()
	}
	deadline := time.Now().Add(20 * time.Second)
	for d.server.Lifecycle.ActiveProxyRequests() < 4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	if _, served := f.counts(); !reflect.DeepEqual(served, []string{"held 429"}) {
		t.Fatalf("while the claim ran Anthropic served %v, want only the first turn's refusal", served)
	}
	close(f.hold)
	for range 4 {
		select {
		case answer := <-answers:
			if answer.status != http.StatusOK || !strings.Contains(answer.body, "msg_held") {
				t.Fatalf("a turn got %+v, want held's answer", answer)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("a waiting turn was never answered")
		}
	}
	settle(t, d.server)
	claims, served := f.counts()
	if claims != 1 || !reflect.DeepEqual(served, []string{"held 429", "held 200", "held 200", "held 200", "held 200"}) {
		t.Fatalf("%d claims, served %v; want one claim and every turn served on held", claims, served)
	}
	for _, session := range []string{"one", "two", "three", "new"} {
		if got := d.sessionAccount(session); got != "held" {
			t.Fatalf("session %s is on %q, want held", session, got)
		}
	}
}

// codexClaimAccount is one Codex account as the fake ChatGPT counts it.
type codexClaimAccount struct {
	used    float64
	credits []accounts.RateLimitResetCredit
}

// codexClaimFake is a fake ChatGPT whose accounts spend: a turn meets the
// usage limit while the week is spent, and a consume gives the week back.
type codexClaimFake struct {
	t        *testing.T
	mu       sync.Mutex
	accounts map[string]*codexClaimAccount
	byAuth   map[string]string
	consumes int
	creates  int
	served   []string
	// consumeStatus, when set, answers a consume with that status instead.
	consumeStatus int
	// outputFirst makes a socket turn forward output before its refusal.
	outputFirst bool
	// unknownAfterFirstList lists every credit with a reset_type this daemon
	// does not know from the second credit-list read on.
	unknownAfterFirstList bool
	creditLists           int
	// consumeHold, when set, keeps each consume waiting until it is closed;
	// consuming gets a value as each one arrives.
	consumeHold chan struct{}
	consuming   chan struct{}
	server      Server
	proxy       *httptest.Server
	logs        *syncBuffer
}

func newCodexClaimFake(t *testing.T, held map[string]*codexClaimAccount) *codexClaimFake {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	for id := range held {
		if err := store.SaveStored(proxyStoredOAuthAccount(id, strings.Split(id, "@")[0], time.Now().Add(time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	f := &codexClaimFake{t: t, accounts: held, byAuth: map[string]string{}, logs: &syncBuffer{}, consuming: make(chan struct{}, 16)}
	stored := storedPoolAccounts(t, store)
	for _, account := range stored {
		f.byAuth[account.AuthorizationHeader()] = account.ID
	}
	upstream := httptest.NewServer(http.HandlerFunc(f.turn))
	t.Cleanup(upstream.Close)
	f.server = Server{
		AccountRef:                NewAccountRef(store, stored, &http.Client{Transport: proxyRoundTripFunc(f.chatgpt)}),
		CodexUpstream:             mustURL(t, upstream.URL),
		Sessions:                  newTestSessionStore(t),
		ActiveSessions:            NewActiveSessions(),
		Lifecycle:                 NewLifecycle(),
		SchedulerRef:              selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		UsageScoreTTL:             time.Hour,
		MaxBodyBytes:              1 << 20,
		CodexOverloadFailover:     withoutCapacityRetry(),
		AllowUnauthenticatedAdmin: true,
		Logger:                    slogJSON(f.logs),
	}.normalized()
	f.proxy = httptest.NewServer(f.server.Handler())
	t.Cleanup(f.proxy.Close)
	f.measure()
	return f
}

func (f *codexClaimFake) measure() {
	f.t.Helper()
	f.server.AccountRef.InvalidateUsageWindowsCache()
	f.server.AccountRef.InvalidateUsageStatusCache()
	ctx := context.Background()
	f.server.updateSchedulerFromUsageStatusesContext(ctx, f.server.AccountRef.UsageStatuses(ctx))
}

func (f *codexClaimFake) set(id string, change func(*codexClaimAccount)) {
	f.mu.Lock()
	change(f.accounts[id])
	f.mu.Unlock()
}

func (f *codexClaimFake) counts() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.consumes, append([]string(nil), f.served...)
}

func (f *codexClaimFake) pinSession(session, account string) {
	f.t.Helper()
	if _, err := f.server.Sessions.Put("codex", session, account, ""); err != nil {
		f.t.Fatal(err)
	}
}

func (f *codexClaimFake) sessionAccount(session string) string {
	assignment, _ := f.server.Sessions.Get("codex", session)
	return assignment.AccountID
}

func (f *codexClaimFake) clearMarks(account string) {
	f.server.SchedulerRef.ClearExhausted(accounts.ProviderCodex, account)
	f.server.admissionGate().clearAccount(schedulerAccountProvider(accounts.ProviderCodex), account)
}

func (f *codexClaimFake) decision(account string) AutoResetDecision {
	decision, _ := f.server.LastAutoResetDecision(accounts.ProviderCodex, account)
	return decision
}

func (f *codexClaimFake) post(session string) (int, string) {
	f.t.Helper()
	status, body := codexEgressPost(f.t, f.proxy.URL, session)
	settle(f.t, f.server)
	return status, body
}

func (f *codexClaimFake) chatgpt(req *http.Request) (*http.Response, error) {
	respond := func(status int, body []byte) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(string(body))), Request: req}, nil
	}
	if req.URL.Path == "/backend-api/wham/rate-limit-reset-credits/consume" {
		f.mu.Lock()
		hold := f.consumeHold
		f.mu.Unlock()
		f.consuming <- struct{}{}
		if hold != nil {
			<-hold
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.byAuth[req.Header.Get("Authorization")]
	account := f.accounts[id]
	if account == nil {
		f.t.Errorf("a request %s %s on an unknown credential", req.Method, req.URL.Path)
		return respond(http.StatusNotFound, nil)
	}
	available := 0
	for _, credit := range account.credits {
		if credit.Status == "available" {
			available++
		}
	}
	switch req.URL.Path {
	case "/backend-api/wham/usage":
		body, _ := json.Marshal(map[string]any{"plan_type": "pro",
			"rate_limit": map[string]any{"limit_reached": account.used >= 100, "primary_window": map[string]any{
				"used_percent": account.used, "limit_window_seconds": 604800, "reset_after_seconds": 518400}},
			"rate_limit_reset_credits": map[string]any{"available_count": available}})
		return respond(http.StatusOK, body)
	case "/backend-api/wham/rate-limit-reset-credits":
		f.creditLists++
		credits := append([]accounts.RateLimitResetCredit(nil), account.credits...)
		if f.unknownAfterFirstList && f.creditLists > 1 {
			for i := range credits {
				credits[i].ResetType = "codex_something_else"
			}
		}
		body, _ := json.Marshal(map[string]any{"credits": credits})
		return respond(http.StatusOK, body)
	case "/backend-api/wham/rate-limit-reset-credits/consume":
		f.consumes++
		if f.consumeStatus != 0 {
			return respond(f.consumeStatus, []byte(`{"error":"no"}`))
		}
		var consume struct {
			CreditID string `json:"credit_id"`
		}
		_ = json.NewDecoder(req.Body).Decode(&consume)
		for i := range account.credits {
			if account.credits[i].ID == consume.CreditID && account.credits[i].Status == "available" {
				account.credits[i].Status = "redeemed"
				account.used = 0
				body, _ := json.Marshal(map[string]any{"credit": account.credits[i]})
				return respond(http.StatusOK, body)
			}
		}
		return respond(http.StatusBadRequest, []byte(`{"error":"unknown credit"}`))
	}
	f.t.Errorf("the fake ChatGPT got %s %s", req.Method, req.URL.Path)
	return respond(http.StatusNotFound, nil)
}

// turn answers a Codex turn over HTTP or a socket: the usage limit while the
// week is spent, else a completed answer.
func (f *codexClaimFake) turn(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	id := f.byAuth[r.Header.Get("Authorization")]
	account := f.accounts[id]
	f.mu.Unlock()
	spent := func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return account != nil && account.used >= 100
	}
	if websocket.IsWebSocketUpgrade(r) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			f.mu.Lock()
			f.creates++
			outputFirst := f.outputFirst
			f.mu.Unlock()
			if !spent() {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_`+id+`"}}`))
				continue
			}
			if outputFirst {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"partial"}`))
			}
			_ = conn.WriteMessage(websocket.TextMessage,
				[]byte(`{"type":"error","error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":518400}}`))
		}
	}
	status := "200"
	if spent() {
		status = "429"
	}
	f.mu.Lock()
	f.served = append(f.served, id+" "+status)
	f.mu.Unlock()
	if status == "429" {
		codexWall(w, r)
		return
	}
	_, _ = io.Copy(io.Discard, r.Body)
	codexEgressWriteCompleted(w, id)
}

func heldCodexCredits(ids ...string) []accounts.RateLimitResetCredit {
	credits := make([]accounts.RateLimitResetCredit, 0, len(ids))
	for i, id := range ids {
		credits = append(credits, autoResetTestCredit(id, time.Duration(5+i)*24*time.Hour))
	}
	return credits
}

// 3, Codex over HTTP: the usage-limit refusal on an account with a credit.
// The credit is redeemed once and the same turn is served on the account.
func TestRealRedeemsACodexCreditAtTheWallAndServesTheTurnInPlace(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	f := newCodexClaimFake(t, map[string]*codexClaimAccount{
		heldCodex:  {used: 90, credits: heldCodexCredits("credit-soon", "credit-late")},
		spareCodex: {used: 30},
	})
	f.pinSession("conversation", heldCodex)
	f.set(heldCodex, func(a *codexClaimAccount) { a.used = 100 })
	status, body := f.post("conversation")
	consumes, served := f.counts()
	if status != http.StatusOK || !strings.Contains(body, "served-from-"+heldCodex) || consumes != 1 ||
		!reflect.DeepEqual(served, []string{heldCodex + " 429", heldCodex + " 200"}) || f.sessionAccount("conversation") != heldCodex {
		t.Fatalf("the turn got %d %.200q; %d consumes, served %v, session on %q; want one redeem and the turn served on held",
			status, body, consumes, served, f.sessionAccount("conversation"))
	}
	if decision := f.decision(heldCodex); decision.Verdict != AutoResetClaimed || decision.Reset != "credit-soon" ||
		!strings.Contains(decision.Reason, "the credit credit-soon was redeemed") {
		t.Fatalf("last decision %+v, want credit-soon claimed", decision)
	}
}

// 3, the socket and the peer. A Codex socket turn at the wall, and a turn
// another daemon forwarded, do not wait: each gets today's answer (the
// socket is closed for a reconnect onto another account; the forwarded turn
// is moved), and the claim still runs, once.
func TestTurnsThatCannotWaitGetTodaysAnswerAndTheClaimStillRuns(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	t.Run("a codex socket turn", func(t *testing.T) {
		f := newCodexClaimFake(t, map[string]*codexClaimAccount{
			heldCodex:  {used: 90, credits: heldCodexCredits("credit")},
			spareCodex: {used: 30},
		})
		f.pinSession("socket", heldCodex)
		f.set(heldCodex, func(a *codexClaimAccount) { a.used = 100 })
		conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(f.proxy.URL, "http")+"/v1/responses",
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
		if _, body, err := conn.ReadMessage(); !websocket.IsCloseError(err, websocket.CloseTryAgainLater) {
			t.Fatalf("the socket got %q, %v; want today's 1013 for a reconnect", body, err)
		}
		settle(t, f.server)
		if consumes, _ := f.counts(); consumes != 1 {
			t.Fatalf("%d consumes, want the claim to run once", consumes)
		}
		if decision := f.decision(heldCodex); decision.Verdict != AutoResetClaimed {
			t.Fatalf("last decision %+v, want claimed", decision)
		}
		// The relay marked the account at the wall; the redeem clears it.
		f.pinSession("after", heldCodex)
		if _, body := f.post("after"); !strings.Contains(body, "served-from-"+heldCodex) {
			t.Fatalf("a turn after the claim got %.200q, want it served on the revived account", body)
		}
	})
	t.Run("a turn a peer forwarded", func(t *testing.T) {
		f := newClaudeClaimFake(t, heldWeek())
		d := f.daemon(t, "")
		d.pinSession("forwarded", "held")
		f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
		answer := d.ask("forwarded", opusModel, http.Header{remotePoolHopHeader: {"1"}})
		settle(t, d.server)
		claims, served := f.counts()
		if !strings.Contains(answer.body, "msg_spare") || !reflect.DeepEqual(served, []string{"held 429", "spare 200"}) || claims != 1 {
			t.Fatalf("the forwarded turn got %+v; served %v and %d claims; want today's move to spare and one claim", answer, served, claims)
		}
	})
}

// 4. A claim that fails, may have landed, or outlasts the turn's wait leaves
// the turn exactly today's answer (moved to spare), makes no second claim on
// the next wall, and marks no credential dead.
func TestAFailedClaimLeavesTheTurnWithTodaysAnswer(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	setAutoResetWaitForTest(t, 500*time.Millisecond, 500*time.Millisecond)
	for _, c := range []struct {
		name    string
		answer  func() (int, string)
		hold    bool
		verdict AutoResetVerdict
		reason  string
	}{
		{"anthropic refuses the token", func() (int, string) { return http.StatusForbidden, `{}` }, false, AutoResetFailed, "auth_error"},
		{"a 5xx that may have landed", func() (int, string) { return http.StatusInternalServerError, `{}` }, false, AutoResetMayHaveLanded, "may have landed"},
		{"not limited", func() (int, string) { return http.StatusOK, `{"result":"not_limited"}` }, false, AutoResetFailed, "not_limited"},
		{"the wait passes its bound", nil, true, AutoResetClaimed, "Anthropic used the reset"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newClaudeClaimFake(t, map[string]*claudeClaimProfile{
				"held":  {week: 95, fable: 20, grants: []bool{false, false}},
				"spare": {week: 40, fable: 10},
			})
			f.answer = c.answer
			if c.hold {
				f.hold = make(chan struct{})
			}
			d := f.daemon(t, "")
			d.pinSession("conversation", "held")
			f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
			answer := d.ask("conversation", opusModel, nil)
			if c.hold {
				close(f.hold)
			}
			settle(t, d.server)
			claims, served := f.counts()
			if answer.status != http.StatusOK || !strings.Contains(answer.body, "msg_spare") || d.sessionAccount("conversation") != "spare" ||
				!reflect.DeepEqual(served, []string{"held 429", "spare 200"}) || claims != 1 {
				t.Fatalf("the turn got %+v, session on %q; served %v, %d claims; want today's move to spare after one claim",
					answer, d.sessionAccount("conversation"), served, claims)
			}
			if decision := d.decision("held"); decision.Verdict != c.verdict || !strings.Contains(decision.Reason, c.reason) {
				t.Fatalf("last decision %+v, want %s saying %q", decision, c.verdict, c.reason)
			}
			// The next wall on the account, its next reset read again since
			// the claim: no second claim.
			f.set("held", func(p *claudeClaimProfile) { p.week = 99 })
			d.measure()
			f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
			d.clearMarks("held")
			d.pinSession("second", "held")
			second := d.turn("second", opusModel)
			if claims, _ := f.counts(); claims != 1 || !strings.Contains(second.body, "msg_spare") {
				t.Fatalf("a second wall: %d claims and %+v, want no second claim and today's move", claims, second)
			}
			if decision := d.decision("held"); decision.Verdict != AutoResetNoClaim || !strings.Contains(decision.Reason, "never twice") {
				t.Fatalf("the second wall's decision %+v, want never twice", decision)
			}
			account, _ := d.server.autoResetOwnAccount(accounts.ProviderClaude, "held")
			if dead := d.server.autoResetDeadCredential(account); dead != "" {
				t.Fatalf("the account's credential is marked dead: %s", dead)
			}
		})
	}
	t.Run("a codex credit of an unknown kind at the consume", func(t *testing.T) {
		f := newCodexClaimFake(t, map[string]*codexClaimAccount{
			heldCodex:  {used: 90, credits: heldCodexCredits("credit")},
			spareCodex: {used: 30},
		})
		f.unknownAfterFirstList = true
		f.pinSession("conversation", heldCodex)
		f.set(heldCodex, func(a *codexClaimAccount) { a.used = 100 })
		status, body := f.post("conversation")
		if consumes, _ := f.counts(); consumes != 0 || status != http.StatusOK || !strings.Contains(body, "served-from-"+spareCodex) {
			t.Fatalf("%d consumes and %d %.200q, want nothing redeemed and today's move", consumes, status, body)
		}
		if decision := f.decision(heldCodex); decision.Verdict != AutoResetFailed || !strings.Contains(decision.Reason, "nothing was sent") ||
			!strings.Contains(decision.Reason, "does not know (reset_type codex_something_else)") {
			t.Fatalf("last decision %+v", decision)
		}
	})
	t.Run("a codex consume refused", func(t *testing.T) {
		f := newCodexClaimFake(t, map[string]*codexClaimAccount{
			heldCodex:  {used: 90, credits: heldCodexCredits("credit")},
			spareCodex: {used: 30},
		})
		f.consumeStatus = http.StatusBadRequest
		f.pinSession("conversation", heldCodex)
		f.set(heldCodex, func(a *codexClaimAccount) { a.used = 100 })
		status, body := f.post("conversation")
		consumes, served := f.counts()
		if status != http.StatusOK || !strings.Contains(body, "served-from-"+spareCodex) || consumes != 1 ||
			!reflect.DeepEqual(served, []string{heldCodex + " 429", spareCodex + " 200"}) {
			t.Fatalf("the turn got %d %.200q; %d consumes, served %v; want today's move after one consume", status, body, consumes, served)
		}
		if decision := f.decision(heldCodex); decision.Verdict != AutoResetMayHaveLanded || !strings.Contains(decision.Reason, "it may have landed") {
			t.Fatalf("last decision %+v", decision)
		}
	})
}

// 5. Never twice. After a claim that landed, a wall inside 10 minutes makes
// no claim; past 10 minutes, with the reading after the claim showing the
// week back, the next wall claims. After a claim that may have landed, a
// wall past 10 minutes makes no claim until a reading shows the week back.
func TestNoSecondClaimInsideTenMinutesOrBeforeAReadingShowsTheMeterBack(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	// wall reads the account at 99% (so its session stays on it), then
	// spends it at the provider and sends a turn.
	wall := func(f *codexClaimFake, session string) (int, string) {
		f.set(heldCodex, func(a *codexClaimAccount) { a.used = 99 })
		f.measure()
		f.set(heldCodex, func(a *codexClaimAccount) { a.used = 100 })
		f.clearMarks(heldCodex)
		f.pinSession(session, heldCodex)
		return f.post(session)
	}
	t.Run("after a claim that landed", func(t *testing.T) {
		f := newCodexClaimFake(t, map[string]*codexClaimAccount{
			heldCodex:  {used: 90, credits: heldCodexCredits("credit-1", "credit-2")},
			spareCodex: {used: 30},
		})
		if _, body := wall(f, "first"); !strings.Contains(body, "served-from-"+heldCodex) {
			t.Fatalf("the first wall: %.200q, want the turn served in place", body)
		}
		setAutoResetClock(f.server, later(5*time.Minute))
		if _, body := wall(f, "second"); !strings.Contains(body, "served-from-"+spareCodex) {
			t.Fatalf("a wall 5 minutes later: %.200q, want today's move", body)
		}
		if consumes, _ := f.counts(); consumes != 1 || !strings.Contains(f.decision(heldCodex).Reason, "no other follows within 10m0s") {
			t.Fatalf("a wall 5 minutes later: %d consumes, decision %+v; want no second claim", consumes, f.decision(heldCodex))
		}
		setAutoResetClock(f.server, later(11*time.Minute))
		if _, body := wall(f, "third"); !strings.Contains(body, "served-from-"+heldCodex) {
			t.Fatalf("a wall 11 minutes later: %.200q, want a claim and the turn served in place", body)
		}
		if consumes, _ := f.counts(); consumes != 2 {
			t.Fatalf("%d consumes, want the second claim past the floor", consumes)
		}
	})
	t.Run("after a claim that may have landed", func(t *testing.T) {
		f := newCodexClaimFake(t, map[string]*codexClaimAccount{
			heldCodex:  {used: 90, credits: heldCodexCredits("credit-1")},
			spareCodex: {used: 30},
		})
		f.consumeStatus = http.StatusBadGateway
		wall(f, "first")
		f.consumeStatus = 0
		setAutoResetClock(f.server, later(11*time.Minute))
		if _, body := wall(f, "second"); !strings.Contains(body, "served-from-"+spareCodex) {
			t.Fatalf("a wall 11 minutes later: %.200q, want today's move", body)
		}
		if consumes, _ := f.counts(); consumes != 1 || !strings.Contains(f.decision(heldCodex).Reason, "only a reading that shows the week back settles it") {
			t.Fatalf("a wall 11 minutes later: %d consumes, decision %+v; want no claim before a reading", consumes, f.decision(heldCodex))
		}
		// The week reads back (the consume had landed after all).
		f.set(heldCodex, func(a *codexClaimAccount) { a.used = 0 })
		f.measure()
		f.server.autoResetReadings(context.Background())
		if _, body := wall(f, "third"); !strings.Contains(body, "served-from-"+heldCodex) {
			t.Fatalf("a wall after the reading: %.200q, want a claim and the turn served in place", body)
		}
		if consumes, _ := f.counts(); consumes != 2 {
			t.Fatalf("%d consumes, want the claim once the reading settled the first", consumes)
		}
	})
}

// 6. A socket turn that has forwarded output when it meets the wall is
// passed its refusal as today and never sent again; the claim runs.
func TestATurnThatHasSentOutputIsNeverHeldOrSentAgain(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	f := newCodexClaimFake(t, map[string]*codexClaimAccount{
		heldCodex:  {used: 90, credits: heldCodexCredits("credit")},
		spareCodex: {used: 30},
	})
	f.outputFirst = true
	f.pinSession("socket", heldCodex)
	f.set(heldCodex, func(a *codexClaimAccount) { a.used = 100 })
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(f.proxy.URL, "http")+"/v1/responses",
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
	var got []string
	for len(got) < 2 {
		_, body, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("after %q the socket ended: %v", got, err)
		}
		got = append(got, string(body))
	}
	if !strings.Contains(got[0], "partial") || !strings.Contains(got[1], "usage_limit_reached") {
		t.Fatalf("the client got %q, want the output and then the refusal", got)
	}
	settle(t, f.server)
	f.mu.Lock()
	creates, consumes := f.creates, f.consumes
	f.mu.Unlock()
	if creates != 1 || consumes != 1 {
		t.Fatalf("the upstream got %d turns and %d consumes, want the turn once and the claim once", creates, consumes)
	}
}

// 7. The owner's condition holds in real mode: a Fable wall whose account's
// next reset is a week reset, with the week read at 98% used, claims nothing,
// and the turn gets what it gets with automatic resets off.
func TestTheOwnersConditionHoldsInRealMode(t *testing.T) {
	type outcome struct {
		answer claudeAnswer
		served []string
	}
	outcomes := map[AutoResetMode]outcome{}
	for _, mode := range []AutoResetMode{AutoResetOff, AutoResetReal} {
		setAutoResetForTest(t, mode, true)
		f := newClaudeClaimFake(t, map[string]*claudeClaimProfile{
			"held":  {week: 98, fable: 95, grants: []bool{false}},
			"spare": {week: 40, fable: 10},
		})
		d := f.daemon(t, "")
		d.pinSession("conversation", "held")
		f.set("held", func(p *claudeClaimProfile) { p.fable = 100 })
		answer := d.turn("conversation", "claude-fable-5-2")
		claims, served := f.counts()
		if claims != 0 {
			t.Fatalf("mode %s: %d claims, want none", mode, claims)
		}
		outcomes[mode] = outcome{answer: answer, served: served}
		if mode == AutoResetReal {
			if decision := d.decision("held"); decision.Verdict != AutoResetRefusedByCondition || !strings.Contains(decision.Reason, "the week has 2.0% left") {
				t.Fatalf("last decision %+v, want refused by the condition", decision)
			}
		}
	}
	if !reflect.DeepEqual(outcomes[AutoResetReal], outcomes[AutoResetOff]) || outcomes[AutoResetOff].served[0] != "held 429" {
		t.Fatalf("real mode: %+v; with automatic resets off: %+v; want the same, after held's refusal", outcomes[AutoResetReal], outcomes[AutoResetOff])
	}
}

// 9. A draining daemon starts no claim. A drain that begins while turns wait
// lets them go with today's answer; the claim already sent goes on.
func TestADrainingDaemonStartsNoClaimAndLetsWaitingTurnsGo(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	t.Run("draining before the wall", func(t *testing.T) {
		f := newClaudeClaimFake(t, heldWeek())
		d := f.daemon(t, "")
		d.pinSession("conversation", "held")
		f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
		d.server.Lifecycle.Drain()
		answer := d.turn("conversation", opusModel)
		if claims, _ := f.counts(); claims != 0 || !strings.Contains(answer.body, "msg_spare") {
			t.Fatalf("%d claims and %+v, want no claim and today's move", claims, answer)
		}
	})
	t.Run("a draining worker's reading leaves nothing for the next", func(t *testing.T) {
		state := t.TempDir()
		f := newCodexClaimFake(t, map[string]*codexClaimAccount{heldCodex: {used: 99.6, credits: heldCodexCredits("credit")}})
		f.server.AutoResetStateDir = state
		f.pinSession("warm", heldCodex)
		f.server.Lifecycle.Drain()
		f.server.autoResetReadings(context.Background())
		settle(t, f.server)
		entries, _ := os.ReadDir(filepath.Join(state, "auto-reset"))
		if consumes, _ := f.counts(); consumes != 0 || len(entries) != 0 || !strings.Contains(f.decision(heldCodex).Reason, "draining") {
			t.Fatalf("%d consumes, %d records, decision %+v; want no claim and no record", consumes, len(entries), f.decision(heldCodex))
		}
	})
	t.Run("draining while a turn waits", func(t *testing.T) {
		f := newClaudeClaimFake(t, heldWeek())
		f.hold = make(chan struct{})
		d := f.daemon(t, "")
		d.pinSession("conversation", "held")
		f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
		answers := make(chan claudeAnswer, 1)
		go func() { answers <- d.ask("conversation", opusModel, nil) }()
		waitClaiming(t, f)
		d.server.Lifecycle.Drain()
		select {
		case answer := <-answers:
			if !strings.Contains(answer.body, "msg_spare") {
				t.Fatalf("the waiting turn got %+v, want today's move", answer)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("the drain did not let the waiting turn go")
		}
		close(f.hold)
		settle(t, d.server)
		if claims, _ := f.counts(); claims != 1 {
			t.Fatalf("%d claims, want the one that was running", claims)
		}
	})
}

// 9, the worker swap. The new worker of a swap shares the old one's state
// directory. A wall on the new worker while the old one's claim runs makes no
// claim, nor one after it if that claim may have landed and no reading since
// shows the week back.
func TestANewWorkerDoesNotRepeatAClaimInFlightWithoutAReading(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	f := newClaudeClaimFake(t, heldWeek())
	f.hold = make(chan struct{})
	f.answer = func() (int, string) { return http.StatusBadGateway, `{}` }
	state := t.TempDir()
	old, fresh := f.daemon(t, state), f.daemon(t, state)
	old.pinSession("conversation", "held")
	fresh.pinSession("other", "held")
	f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
	answers := make(chan claudeAnswer, 1)
	go func() { answers <- old.ask("conversation", opusModel, nil) }()
	waitClaiming(t, f)
	if answer := fresh.turn("other", opusModel); !strings.Contains(answer.body, "msg_spare") {
		t.Fatalf("the new worker's turn got %+v, want today's move", answer)
	}
	if claims, _ := f.counts(); claims != 1 || !strings.Contains(fresh.decision("held").Reason, "never twice") {
		t.Fatalf("%d claims, the new worker decided %+v; want no second claim", claims, fresh.decision("held"))
	}
	close(f.hold)
	<-answers
	settle(t, old.server)
	setAutoResetClock(fresh.server, later(11*time.Minute))
	fresh.clearMarks("held")
	fresh.pinSession("third", "held")
	if answer := fresh.turn("third", opusModel); !strings.Contains(answer.body, "msg_spare") {
		t.Fatalf("the new worker's turn after the old claim got %+v, want today's move", answer)
	}
	if claims, _ := f.counts(); claims != 1 || !strings.Contains(fresh.decision("held").Reason, "only a reading") {
		t.Fatalf("%d claims, the new worker decided %+v; want none before a reading", claims, fresh.decision("held"))
	}
}

// 2, a turn already sent when the claim starts: it meets the wall during the
// claim and waits on the claim that runs, and both turns are served on the
// account after it.
func TestATurnInFlightWhenTheClaimStartsWaitsOnIt(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	f := newClaudeClaimFake(t, map[string]*claudeClaimProfile{"held": {week: 95, grants: []bool{false}}})
	f.hold, f.gateFirst = make(chan struct{}), make(chan struct{})
	d := f.daemon(t, "")
	d.pinSession("one", "held")
	d.pinSession("two", "held")
	f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
	answers := make(chan claudeAnswer, 2)
	go func() { answers <- d.ask("two", opusModel, nil) }()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		f.mu.Lock()
		gated := f.gated
		f.mu.Unlock()
		if gated || time.Now().After(deadline) {
			break
		}
	}
	go func() { answers <- d.ask("one", opusModel, nil) }()
	waitClaiming(t, f)
	close(f.gateFirst)
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if _, served := f.counts(); len(served) == 2 {
			break
		}
	}
	close(f.hold)
	for range 2 {
		if answer := <-answers; answer.status != http.StatusOK || !strings.Contains(answer.body, "msg_held") {
			t.Fatalf("a turn got %+v, want held's answer", answer)
		}
	}
	settle(t, d.server)
	if claims, served := f.counts(); claims != 1 || !reflect.DeepEqual(served, []string{"held 429", "held 429", "held 200", "held 200"}) {
		t.Fatalf("%d claims, served %v; want one claim and both turns served on held after their refusals", claims, served)
	}
}

// 1, sent again once: a turn sent again after the claim that meets the wall
// again gets today's answer (moved to spare), with no second claim.
func TestATurnSentAgainThatMeetsTheWallAgainGetsTodaysAnswer(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	f := newClaudeClaimFake(t, heldWeek())
	d := f.daemon(t, "")
	d.pinSession("conversation", "held")
	f.set("held", func(p *claudeClaimProfile) { p.week, p.stuck = 100, true })
	answer := d.turn("conversation", opusModel)
	claims, served := f.counts()
	if claims != 1 || !strings.Contains(answer.body, "msg_spare") || !reflect.DeepEqual(served, []string{"held 429", "held 429", "spare 200"}) {
		t.Fatalf("%d claims, served %v, answer %+v; want one claim, one resend, then today's move", claims, served, answer)
	}
}

// D, through the claim: the claim names the grant the decision judged, so
// when Anthropic offers another next (here a Fable reset) nothing is sent,
// and the turn gets today's answer.
func TestTheClaimSpendsOnlyTheResetTheDecisionJudged(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	f := newClaudeClaimFake(t, map[string]*claudeClaimProfile{
		"held":  {week: 95, fable: 20, grants: []bool{false, true}, reorderAtLimit: true},
		"spare": {week: 40, fable: 10},
	})
	d := f.daemon(t, "")
	d.pinSession("conversation", "held")
	f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
	answer := d.turn("conversation", opusModel)
	if claims, _ := f.counts(); claims != 0 || !strings.Contains(answer.body, "msg_spare") {
		t.Fatalf("%d claims and %+v, want nothing sent and today's move", claims, answer)
	}
	if decision := d.decision("held"); decision.Verdict != AutoResetFailed || !strings.Contains(decision.Reason, "only the next grant can be claimed") {
		t.Fatalf("last decision %+v", decision)
	}
}

// B(2) in real mode, Codex: a reading at 99.6% used with a credit and a warm
// session claims, with the cooked rule lifted (the account is not at its
// limit); the live reading before the consume must still have under 1% left.
func TestRealClaimsOnACodexReadingAtTheLimit(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	for _, c := range []struct {
		name     string
		liveUsed float64
		consumes int
		verdict  AutoResetVerdict
	}{
		{"the live week has 0.4% left", 99.6, 1, AutoResetClaimed},
		{"the live week has 5% left", 95, 0, AutoResetFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newCodexClaimFake(t, map[string]*codexClaimAccount{heldCodex: {used: 99.6, credits: heldCodexCredits("credit")}})
			f.pinSession("warm", heldCodex)
			f.set(heldCodex, func(a *codexClaimAccount) { a.used = c.liveUsed })
			f.server.autoResetReadings(context.Background())
			settle(t, f.server)
			decision := f.decision(heldCodex)
			if consumes, _ := f.counts(); consumes != c.consumes || decision.Verdict != c.verdict || decision.Moment != "reading" {
				t.Fatalf("%d consumes, decision %+v; want %d and %s", consumes, decision, c.consumes, c.verdict)
			}
			if c.consumes == 0 && !strings.Contains(decision.Reason, "live reading") {
				t.Fatalf("decision %+v, want the live reading named", decision)
			}
		})
	}
}

// 3, the marks other turns left. While a turn waits on a redeem, a socket
// turn on the same account meets the wall and the relay marks the account;
// the redeem clears the mark, so the waiting turn is still served in place.
func TestARedeemClearsTheMarksAnotherTurnLeftWhileItRan(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	f := newCodexClaimFake(t, map[string]*codexClaimAccount{
		heldCodex:  {used: 90, credits: heldCodexCredits("credit")},
		spareCodex: {used: 30},
	})
	f.consumeHold = make(chan struct{})
	f.pinSession("conversation", heldCodex)
	f.pinSession("socket", heldCodex)
	f.set(heldCodex, func(a *codexClaimAccount) { a.used = 100 })
	type answer struct {
		status int
		body   string
	}
	answers := make(chan answer, 1)
	go func() {
		status, body := codexEgressPost(t, f.proxy.URL, "conversation")
		answers <- answer{status, body}
	}()
	select {
	case <-f.consuming:
	case <-time.After(20 * time.Second):
		t.Fatal("no consume reached the fake")
	}
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(f.proxy.URL, "http")+"/v1/responses",
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
	if _, _, err := conn.ReadMessage(); !websocket.IsCloseError(err, websocket.CloseTryAgainLater) {
		t.Fatalf("the socket got %v, want today's 1013", err)
	}
	close(f.consumeHold)
	got := <-answers
	settle(t, f.server)
	consumes, served := f.counts()
	if got.status != http.StatusOK || !strings.Contains(got.body, "served-from-"+heldCodex) || consumes != 1 ||
		!reflect.DeepEqual(served, []string{heldCodex + " 429", heldCodex + " 200"}) {
		t.Fatalf("the waiting turn got %d %.200q; %d consumes, served %v; want one redeem and the turn served on held", got.status, got.body, consumes, served)
	}
}
