package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// End to end (PATCHES §31): a real laptop-role Server whose peer is a real
// box-role Server behind httptest, each in front of its own fake Anthropic.
// The box measures its accounts through its real usage sweep (an AccountRef
// over a Claude profile store, the usage endpoint faked), publishes its own
// verdicts from its own selection scheduler, and serves or refuses targeted
// turns through its real handler; the laptop reads that body over HTTP and
// decides through its real handler. Nothing here restates a rule: the only
// fixture inputs are usage percentages, reset times, live sessions and what
// the upstream answers.
//
// This file names only what main already had plus the shared
// wire files (selectacct/new_session_verdict.go and internal/proxy/
// remote_pool_target.go, which only ADD a type, a method and constants), so
// every test in it runs unchanged against origin/main + those files and was
// watched failing there -- or is marked a CHARACTERIZATION test of behaviour
// that must not change, and was shown red by a single-piece mutation instead.
// Tests that need the N-peer configuration live in crosspool_e2e_peers_test.go.

const e2eModelTurn = `{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}]}`

// e2eUsage is the Claude usage body the box's sweep reads for one account:
// utilization percentages and reset times for the 5h and 7-day windows.
func e2eUsage(now time.Time, fiveHourUsed float64, fiveHourReset time.Duration, weeklyUsed float64, weeklyReset time.Duration) string {
	window := func(used float64, reset time.Duration) string {
		return `{"utilization":` + strconv.FormatFloat(used, 'f', -1, 64) +
			`,"resets_at":"` + now.Add(reset).UTC().Format(time.RFC3339) + `"}`
	}
	return `{"five_hour":` + window(fiveHourUsed, fiveHourReset) + `,"seven_day":` + window(weeklyUsed, weeklyReset) + `}`
}

// e2eUpstream is one daemon's fake Anthropic: it names the account each hit
// spent (by bearer token) and can be told to answer an account differently.
type e2eUpstream struct {
	mu   sync.Mutex
	hits []string
	// answer may answer a hit itself (return true); nil or false is a plain
	// allowed 200 naming the account.
	answer func(account string, w http.ResponseWriter) bool
}

func (u *e2eUpstream) serve(w http.ResponseWriter, r *http.Request) {
	account := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-")
	u.mu.Lock()
	u.hits = append(u.hits, account)
	answer := u.answer
	u.mu.Unlock()
	if answer != nil && answer(account, w) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"msg","served_by":"`+account+`"}`)
}

func (u *e2eUpstream) setAnswer(answer func(account string, w http.ResponseWriter) bool) {
	u.mu.Lock()
	u.answer = answer
	u.mu.Unlock()
}

func (u *e2eUpstream) accounts() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.hits...)
}

// e2eRejected answers as Anthropic does for a spent window: 429, rejected.
func e2eRejected(w http.ResponseWriter) {
	w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
	w.Header().Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10))
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"limit reached"}}`)
}

// e2eLimited answers a 429 that leaves the account limited, not spent.
func e2eLimited(w http.ResponseWriter) {
	w.Header().Set("anthropic-ratelimit-unified-status", "allowed_warning")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
}

// --- the box: a real Server, measured by its real sweep ------------------------

type e2eBoxTurn struct {
	Target  string
	Hop     string
	Session string
	Body    string
	Status  int
	Account string
	Refused string
}

type e2eBox struct {
	server   Server
	store    *session.Store
	active   *ActiveSessions
	upstream *e2eUpstream
	logs     *syncBuffer
	srv      *httptest.Server

	mu    sync.Mutex
	turns []e2eBoxTurn
	// legacy makes the box look like one built before verdicts: its usage body
	// carries no new_session (stripped on the way out) and a target header is
	// dropped on the way in, so it places the turn by its own rules.
	legacy bool
	// ignoreTarget keeps the verdicts but drops the target header: a peer that
	// publishes verdicts yet does not honour a target.
	ignoreTarget bool
	// usageDelay stalls the usage endpoint: a slow or blackholed peer.
	usageDelay time.Duration
	busy       int
}

// e2eMeasuredServer is one daemon whose accounts are Claude profiles measured
// by the real usage sweep (usage maps profile name -> usage body), in front of
// its own fake Anthropic. The box and the laptop are both built this way, so
// both sides of every comparison come out of the same scoring code -- the
// Fable reservation on a non-Fable request's weekly headroom included
// (NormalizedUsage.WeeklyHeadroom): an untouched week publishes 1.0 on either
// side under PATCHES §57's default, 0.5 with SUBROUTER_FABLE_RESERVATION=1, and
// P2's arithmetic is a ratio of the two, so it holds either way.
func e2eMeasuredServer(t *testing.T, usage map[string]string) (Server, *session.Store, *e2eUpstream, *syncBuffer) {
	t.Helper()
	claudeStore := agentclaude.Store{Dir: t.TempDir()}
	for name := range usage {
		credential := agentclaude.CredentialInfo{
			AccessToken: "tok-" + name, RefreshToken: "refresh-" + name,
			ExpiresAt: time.Now().Add(2 * time.Hour).UnixMilli(),
		}
		if err := claudeStore.ImportProfileCredential(name, credential); err != nil {
			t.Fatal(err)
		}
		dir := claudeStore.PreferredInstancePath(claudeStore.InstancePath(name))
		config := `{"oauthAccount":{"emailAddress":"` + name + `@example.com"}}`
		if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ref := &AccountRef{
		store:       accounts.CodexStore{Dir: t.TempDir()},
		claudeStore: claudeStore,
		client: &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			name := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer tok-")
			body, ok := usage[name]
			if !ok || strings.Contains(req.URL.Path, "/oauth/token") {
				return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	up := &e2eUpstream{}
	upstream := httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(upstream.Close)
	logs := &syncBuffer{}
	return Server{
		AccountRef:     ref,
		ClaudeUpstream: mustURL(t, upstream.URL),
		SchedulerRef:   selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		Sessions:       store,
		ActiveSessions: NewActiveSessions(),
		AdminToken:     "admin",
		MaxBodyBytes:   1 << 20,
		// Debug, so a test sees the kept-turn decision lines too and pins
		// their level (PATCHES §39) rather than their absence.
		Logger: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}, store, up, logs
}

// e2eMeasure runs the daemon's own usage sweep once, as its first usage-status
// read does, so its scheduler holds the measured scores.
func e2eMeasure(t *testing.T, server Server) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, remotePoolUsagePath, nil)
	req.Header.Set("Authorization", "Bearer admin")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("calibration: usage-status = %d: %s", recorder.Code, recorder.Body.String())
	}
}

// newE2EBox builds a box: a measured daemon behind httptest.
func newE2EBox(t *testing.T, usage map[string]string) *e2eBox {
	t.Helper()
	server, store, up, logs := e2eMeasuredServer(t, usage)
	box := &e2eBox{server: server, store: store, upstream: up, logs: logs}
	box.active = box.server.ActiveSessions
	box.srv = httptest.NewServer(http.HandlerFunc(box.serve))
	t.Cleanup(box.srv.Close)
	return box
}

func (b *e2eBox) serve(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	legacy, ignoreTarget, delay := b.legacy, b.ignoreTarget, b.usageDelay
	b.mu.Unlock()
	handler := b.server.Handler()
	if r.URL.Path == remotePoolUsagePath {
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		if !legacy {
			handler.ServeHTTP(w, r)
			return
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		var rows []map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err == nil {
			for _, row := range rows {
				if normalized, ok := row["normalized"].(map[string]any); ok {
					delete(normalized, "new_session")
					if pools, ok := normalized["pools"].(map[string]any); ok {
						for _, pool := range pools {
							if entry, ok := pool.(map[string]any); ok {
								delete(entry, "new_session")
							}
						}
					}
				}
			}
			body, _ := json.Marshal(rows)
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(body)
			return
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
		return
	}
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	turn := e2eBoxTurn{
		Target:  r.Header.Get("X-Subrouter-Target-Account"),
		Hop:     r.Header.Get(remotePoolHopHeader),
		Session: r.Header.Get("X-Subrouter-Session"),
		Body:    string(body),
	}
	if legacy || ignoreTarget {
		r.Header.Del("X-Subrouter-Target-Account")
	}
	// Recorded on ARRIVAL, completed after: a forwarding loop shows up as turns
	// even while its innermost hop is still hanging.
	b.mu.Lock()
	index := len(b.turns)
	b.turns = append(b.turns, turn)
	b.mu.Unlock()
	recorder := &e2eStatusRecorder{ResponseWriter: w}
	handler.ServeHTTP(recorder, r)
	b.mu.Lock()
	b.turns[index].Status = recorder.status
	b.turns[index].Account = recorder.Header().Get(subrouterAccountHeader)
	b.turns[index].Refused = recorder.Header().Get("X-Subrouter-Target-Refused")
	b.mu.Unlock()
}

type e2eStatusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *e2eStatusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *e2eStatusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(p)
}

func (r *e2eStatusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (b *e2eBox) seen() []e2eBoxTurn {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]e2eBoxTurn(nil), b.turns...)
}

func (b *e2eBox) targets() []string {
	var out []string
	for _, turn := range b.seen() {
		out = append(out, turn.Target)
	}
	return out
}

// newPool is how the laptop is configured today: one peer, URL only.
func (b *e2eBox) newPool() *RemotePool {
	return NewRemotePool(b.srv.URL, "", "")
}

// live puts one more session in flight on account, on the box: pinned there and
// mid-turn. Only the box can count it; that is what 2026-09-27 turned on.
func (b *e2eBox) live(t *testing.T, account string) func() {
	t.Helper()
	b.mu.Lock()
	b.busy++
	sessionID := "box-busy-" + strconv.Itoa(b.busy)
	b.mu.Unlock()
	if _, err := b.store.Put("claude", sessionID, account, ""); err != nil {
		t.Fatal(err)
	}
	return b.active.Begin("claude", sessionID)
}

// verdict is the box's own new-session verdict on account right now, read off
// its selection scheduler: the fixture's calibration, independent of whether
// (or how) the box publishes it.
func (b *e2eBox) verdict(account string) selectacct.NewSessionVerdict {
	return b.server.selectionScheduler("", "").NewSessionVerdict(accounts.ProviderClaude, account, time.Now())
}

func (b *e2eBox) calibrate(t *testing.T, want map[string]string) {
	t.Helper()
	for account, reason := range want {
		if got := b.verdict(account); got.Reason != reason {
			t.Fatalf("calibration: box %s verdict %+v, want %s", account, got, reason)
		}
	}
}

func (b *e2eBox) logLines(message string) []map[string]any {
	return e2eLogLines(b.logs, message)
}

func e2eLogLines(logs *syncBuffer, message string) []map[string]any {
	var out []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(logs.String()))
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var line map[string]any
		if json.Unmarshal(scanner.Bytes(), &line) == nil && line["msg"] == message {
			out = append(out, line)
		}
	}
	return out
}

// awaitLogLines waits, at most 5 s, until logs holds n lines with message,
// and returns them all: a turn placed on a box is logged after its answer
// has gone out.
func awaitLogLines(t *testing.T, logs *syncBuffer, message string, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		lines := e2eLogLines(logs, message)
		if len(lines) >= n {
			return lines
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d %q lines after 5 s, want %d; logs:\n%s", len(lines), message, n, logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// --- the laptop: a real Server with a local pool and one peer -----------------

type e2eLaptop struct {
	server   Server
	store    *session.Store
	active   *ActiveSessions
	upstream *e2eUpstream
	logs     *syncBuffer
}

// newE2ELaptop builds the deciding daemon: a measured daemon (usage maps its
// local profile -> usage body) with one peer, the configuration origin/main
// already had.
func newE2ELaptop(t *testing.T, usage map[string]string, peer *RemotePool) *e2eLaptop {
	t.Helper()
	server, store, up, logs := e2eMeasuredServer(t, usage)
	server.RemotePool = peer
	// Up for longer than the idle timer, so a session this process never saw
	// is cold; tests that want a warm one take a turn or mark it.
	server.ActiveSessions.startedAt = time.Now().Add(-3 * time.Hour)
	e2eMeasure(t, server)
	return &e2eLaptop{server: server, store: store, active: server.ActiveSessions, upstream: up, logs: logs}
}

// prime reads the peer's usage body into the laptop's cache, as the laptop's
// first decision would, so a decision never races the peer's first sweep.
// It also primes the peer's scheduler (its sweep feeds it).
func e2ePrime(t *testing.T, pool *RemotePool) {
	t.Helper()
	if snapshot := pool.AdminGet(context.Background(), remotePoolUsagePath); len(snapshot.Body) == 0 {
		t.Fatalf("calibration: the peer's usage body is empty (%s)", snapshot.Err)
	}
}

type e2eResult struct {
	Status   int
	ServedBy string
	Pool     string
	Peer     string
	Header   http.Header
}

func (l *e2eLaptop) turn(t *testing.T, sessionID string) e2eResult {
	t.Helper()
	return e2eTurn(t, l.server.Handler(), sessionID, nil)
}

func e2eTurn(t *testing.T, handler http.Handler, sessionID string, extra http.Header) e2eResult {
	t.Helper()
	front := httptest.NewServer(handler)
	defer front.Close()
	result, err := e2eDo(front.URL, sessionID, extra)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// e2eDo posts one Claude model turn to a daemon's URL. No *testing.T, so a
// turn can run on its own goroutine.
func e2eDo(baseURL, sessionID string, extra http.Header) (e2eResult, error) {
	return e2eDoBody(context.Background(), baseURL, sessionID, e2eModelTurn, extra)
}

// e2eDoBody is e2eDo with the turn's body given, on ctx: a test whose hold
// runs on a clock that stands still cancels it, or a held turn would keep
// its daemon from closing.
func e2eDoBody(ctx context.Context, baseURL, sessionID, turn string, extra http.Header) (e2eResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/messages", strings.NewReader(turn))
	if err != nil {
		return e2eResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", sessionID)
	for name, values := range extra {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return e2eResult{}, err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	var payload struct {
		ServedBy string `json:"served_by"`
	}
	_ = json.Unmarshal(body, &payload)
	return e2eResult{
		Status: response.StatusCode, ServedBy: payload.ServedBy,
		Pool: response.Header.Get(subrouterPoolHeader), Peer: response.Header.Get(subrouterPeerHeader),
		Header: response.Header,
	}, nil
}

func (l *e2eLaptop) decisions() []map[string]any {
	return e2eLogLines(l.logs, "remote pool decision")
}

func (l *e2eLaptop) pinnedTo(sessionID string) string {
	assignment, ok := l.store.Get("claude", sessionID)
	if !ok {
		return ""
	}
	return assignment.AccountID
}

// --- the box of 2026-09-27 ----------------------------------------------------

// e2eIncidentBox is the box as it stood on 2026-09-27 ~19:00 UTC: chip-5 has
// ~4% of its week left and rolls in 16h (burndown), with a healthy 5h window;
// every other account's 5h window is spent (chip-1), under the 5% floor
// (chip-2) or burning too fast to last (chip-3). The numbers are the live
// pool's that evening, rounded.
func e2eIncidentBox(t *testing.T) *e2eBox {
	t.Helper()
	now := time.Now()
	return newE2EBox(t, map[string]string{
		"chip-5": e2eUsage(now, 10, 150*time.Minute, 96, 16*time.Hour),
		"chip-1": e2eUsage(now, 100, 2*time.Hour, 50, 100*time.Hour),
		"chip-2": e2eUsage(now, 98, time.Hour, 40, 90*time.Hour),
		"chip-3": e2eUsage(now, 80, 4*time.Hour, 24, 120*time.Hour),
	})
}

// e2eIncidentLocal is the laptop's pool that night: fourteen-beta ~11%
// of its week used, fifteen-beta ~1%.
func e2eIncidentLocal() map[string]string {
	now := time.Now()
	return map[string]string{
		"fourteen": e2eUsage(now, 5, 3*time.Hour, 11, 152*time.Hour),
		"fifteen":  e2eUsage(now, 5, 3*time.Hour, 1, 159*time.Hour),
	}
}

// e2eFiveDays is the owner's P2 example for local: 100% of the week left,
// five days to the reset.
func e2eFiveDays() map[string]string {
	return map[string]string{"loc5d": e2eUsage(time.Now(), 0, 4*time.Hour, 0, 120*time.Hour)}
}

// E1 (P1/P3), the incident: the laptop read chip-5 as a burndown account with
// room and sent ~23 sessions; chip-5 already carried the one session its taper
// allows, so the box spread them onto accounts whose 5h windows were spent
// (23,195 "routing optimistically" lines that day). Judged by the box's own
// verdicts nothing on the box is eligible, so every session stays on the
// roomy local pool and the box is never asked.
func TestE2EIncidentReplay20260927(t *testing.T) {
	box := e2eIncidentBox(t)
	laptop := newE2ELaptop(t, e2eIncidentLocal(), box.newPool())
	defer box.live(t, "chip-5")()
	e2ePrime(t, laptop.server.RemotePool)
	box.calibrate(t, map[string]string{
		"chip-5": selectacct.VerdictAtCap, "chip-1": selectacct.VerdictExhausted,
		"chip-2": selectacct.VerdictShortFloor, "chip-3": selectacct.VerdictBurnProjection,
	})

	for i := 1; i <= 23; i++ {
		sessionID := "incident-" + strconv.Itoa(i)
		got := laptop.turn(t, sessionID)
		if got.Status != http.StatusOK || (got.ServedBy != "fourteen" && got.ServedBy != "fifteen") {
			t.Fatalf("%s: status %d served by %q (pool %q); want the roomy local pool", sessionID, got.Status, got.ServedBy, got.Pool)
		}
	}
	if turns := box.seen(); len(turns) != 0 {
		t.Fatalf("the box was sent %d turns (%+v); nothing on it is eligible by its own verdict", len(turns), turns)
	}
	if lines := box.logLines("selected OAuth account below new-session headroom; routing optimistically"); len(lines) != 0 {
		t.Fatalf("the box routed %d turns optimistically onto spent accounts", len(lines))
	}
}

// e2eHold is the laptop's hold on sessionID, if any: which peer account the
// session was sent to.
func e2eHold(pool *RemotePool, sessionID string) (string, bool) {
	hold, ok := pool.delegations.lookup(session.ScopedSessionKey("claude", sessionID), time.Now())
	return hold.Account, ok
}

// e2eRefetch drops the laptop's cached peer body and reads a fresh one: the
// laptop's next decision sees the box as it is now.
func e2eRefetch(t *testing.T, pool *RemotePool) {
	t.Helper()
	pool.admin.mu.Lock()
	delete(pool.admin.entries, remotePoolUsagePath)
	pool.admin.mu.Unlock()
	e2ePrime(t, pool)
}

func e2eLocalAccount(servedBy string) bool {
	return servedBy == "fourteen" || servedBy == "fifteen" || servedBy == "loc5d"
}

// E1b (P1): with chip-5 idle its expiring week outranks the roomy local pool,
// so the first session is TARGETED there and pinned. A second session while
// the first is still in flight asks for chip-5 on the same (now stale) cached
// verdict; the box counts its live session, refuses at its cap before
// contacting Anthropic, and the laptop recomputes -- nothing else on the box
// is eligible, so it lands local. The cap that applies on the box is the cap
// the laptop is held to.
func TestE2EBurndownPeerWinsUnderItsCapAndItsCapRefusesTheNext(t *testing.T) {
	box := e2eIncidentBox(t)
	laptop := newE2ELaptop(t, e2eIncidentLocal(), box.newPool())
	e2ePrime(t, laptop.server.RemotePool)
	box.calibrate(t, map[string]string{"chip-5": selectacct.VerdictBurndown})

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()
	time.AfterFunc(10*time.Second, releaseAll) // never deadlock, whatever the tree does
	var holding atomic.Bool
	box.upstream.setAnswer(func(account string, w http.ResponseWriter) bool {
		if account == "chip-5" && holding.CompareAndSwap(false, true) {
			<-release // the first turn on chip-5 stays in flight
		}
		return false
	})
	front := httptest.NewServer(laptop.server.Handler())
	defer front.Close()
	type outcome struct {
		result e2eResult
		err    error
	}
	first := make(chan outcome, 1)
	go func() {
		result, err := e2eDo(front.URL, "s1", nil)
		first <- outcome{result, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !holding.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	second := laptop.turn(t, "s2")
	releaseAll()
	got := <-first
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.result.Status != http.StatusOK || got.result.ServedBy != "chip-5" {
		t.Fatalf("s1: status %d served by %q; want chip-5, the box's burndown account", got.result.Status, got.result.ServedBy)
	}
	if second.Status != http.StatusOK || !e2eLocalAccount(second.ServedBy) {
		t.Fatalf("s2: status %d served by %q; chip-5 is at its cap with s1 in flight, so s2 belongs local", second.Status, second.ServedBy)
	}
	var s1, s2 []e2eBoxTurn
	for _, turn := range box.seen() {
		switch turn.Session {
		case "s1":
			s1 = append(s1, turn)
		case "s2":
			s2 = append(s2, turn)
		}
	}
	if len(s1) != 1 || s1[0].Target != "chip-5" || s1[0].Status != http.StatusOK {
		t.Fatalf("box turns for s1 %+v; want one, targeted at chip-5 and served", s1)
	}
	if len(s2) != 1 || s2[0].Target != "chip-5" || s2[0].Status != http.StatusConflict || s2[0].Refused != selectacct.VerdictAtCap {
		t.Fatalf("box turns for s2 %+v; want one targeted at chip-5 and refused at_cap", s2)
	}
	if hits := box.upstream.accounts(); len(hits) != 1 {
		t.Fatalf("box upstream hits %v; the refusal must come before Anthropic is contacted", hits)
	}
	if assignment, ok := box.store.Get("claude", "s1"); !ok || assignment.AccountID != "chip-5" {
		t.Fatalf("box pin for s1 = %+v; want chip-5", assignment)
	}
	refused := e2eLogLines(laptop.logs, "remote pool target refused")
	if len(refused) != 1 || refused[0]["target_account"] != "chip-5" || refused[0]["reason"] != selectacct.VerdictAtCap {
		t.Fatalf("refusal lines %v; want one naming chip-5 and at_cap", refused)
	}
}

// P2 in the owner's arithmetic, against a real box: remaining% x 7 / days to
// the weekly reset, local doubled. Local 100%/5d = 140, x2 = 280. A box
// account at 100%/2d = 350 wins; one at 100%/3d = 233 loses; a box burndown
// account under its cap outranks both (burndown tier first).
func TestE2ELocalPreferenceIsTwoXAgainstARealBox(t *testing.T) {
	now := time.Now()
	twoDays := e2eUsage(now, 0, 4*time.Hour, 0, 48*time.Hour)
	threeDays := e2eUsage(now, 0, 4*time.Hour, 0, 72*time.Hour)
	burndown := e2eUsage(now, 0, 4*time.Hour, 70, 20*time.Hour)
	for _, tc := range []struct {
		name  string
		usage map[string]string
		want  string
	}{
		{"box 100%/2d (350) beats local 100%/5d (280)", map[string]string{"b2d": twoDays}, "b2d"},
		// CHARACTERIZATION half: origin/main's two-candidate decider made the same call.
		{"box 100%/3d (233) loses to local (280)", map[string]string{"b3d": threeDays}, "loc5d"},
		{"box burndown under its cap outranks both", map[string]string{"b2d": twoDays, "bburn": burndown}, "bburn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := newE2EBox(t, tc.usage)
			laptop := newE2ELaptop(t, e2eFiveDays(), box.newPool())
			e2ePrime(t, laptop.server.RemotePool)
			if _, ok := tc.usage["bburn"]; ok {
				box.calibrate(t, map[string]string{"bburn": selectacct.VerdictBurndown, "b2d": selectacct.VerdictEligible})
			}
			got := laptop.turn(t, "s1")
			turns := box.seen()
			if tc.want == "loc5d" {
				if got.ServedBy != "loc5d" || len(turns) != 0 {
					t.Fatalf("served by %q, box turns %+v; want local", got.ServedBy, turns)
				}
				return
			}
			if got.Status != http.StatusOK || got.ServedBy != tc.want || len(turns) != 1 || turns[0].Target != tc.want {
				t.Fatalf("status %d served by %q, box turns %+v; want %s, targeted", got.Status, got.ServedBy, turns, tc.want)
			}
		})
	}
}

// P6: the decider TARGETS an account; a refusal recomputes the SAME ranking
// without it, and the winner of the recompute may be another box account or
// local. The box published chip-5 as burndown-eligible; by the time the turn
// arrives it has taken a session, so it refuses at its cap.
func TestE2ETargetRefusalRecomputesToTheNextBestAccount(t *testing.T) {
	now := time.Now()
	chip5 := e2eUsage(now, 10, 150*time.Minute, 96, 16*time.Hour)
	for _, tc := range []struct {
		name  string
		usage map[string]string
		want  string
	}{
		{"the next best is another box account", map[string]string{"chip-5": chip5, "b2d": e2eUsage(now, 0, 4*time.Hour, 0, 48*time.Hour)}, "b2d"},
		{"the next best is local", map[string]string{"chip-5": chip5, "b3d": e2eUsage(now, 0, 4*time.Hour, 0, 72*time.Hour)}, "loc5d"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := newE2EBox(t, tc.usage)
			laptop := newE2ELaptop(t, e2eFiveDays(), box.newPool())
			e2ePrime(t, laptop.server.RemotePool) // chip-5 published idle: burndown-eligible
			box.calibrate(t, map[string]string{"chip-5": selectacct.VerdictBurndown})
			defer box.live(t, "chip-5")()
			box.calibrate(t, map[string]string{"chip-5": selectacct.VerdictAtCap})

			got := laptop.turn(t, "s1")
			if got.Status != http.StatusOK || got.ServedBy != tc.want {
				t.Fatalf("status %d served by %q; want %s after chip-5's refusal", got.Status, got.ServedBy, tc.want)
			}
			turns := box.seen()
			if len(turns) == 0 || turns[0].Target != "chip-5" || turns[0].Status != http.StatusConflict || turns[0].Refused != selectacct.VerdictAtCap {
				t.Fatalf("box turns %+v; want chip-5 targeted first and refused at_cap", turns)
			}
			if tc.want == "loc5d" {
				if len(turns) != 1 {
					t.Fatalf("box turns %+v; want only the refused one", turns)
				}
			} else {
				if len(turns) != 2 || turns[1].Target != tc.want || turns[1].Status != http.StatusOK {
					t.Fatalf("box turns %+v; want the recompute to target %s", turns, tc.want)
				}
				if turns[0].Body != turns[1].Body || turns[0].Body == "" {
					t.Fatal("the recompute forwarded a different body than the refused attempt")
				}
			}
			decisions := laptop.decisions()
			last := decisions[len(decisions)-1]
			if last["recompute"] != true || last["refused_account"] != "chip-5" || last["refused_reason"] != selectacct.VerdictAtCap {
				t.Fatalf("last decision %v; want a recompute naming chip-5's at_cap refusal", last)
			}
			wantTarget := tc.want
			if tc.want == "loc5d" {
				wantTarget = ""
			}
			if last["target_account"] != wantTarget {
				t.Fatalf("last decision target %v; want %q", last["target_account"], wantTarget)
			}
		})
	}
}

// P6's bound: a peer whose published verdicts keep saying yes and whose turns
// keep saying no costs at most remotePoolMaxTargetRefusals (2) attempts per
// turn, then the turn is served locally.
func TestE2ETargetRefusalsAreBoundedThenServedLocally(t *testing.T) {
	now := time.Now()
	burndown := e2eUsage(now, 10, 150*time.Minute, 96, 16*time.Hour)
	box := newE2EBox(t, map[string]string{"x1": burndown, "x2": burndown, "x3": burndown})
	laptop := newE2ELaptop(t, e2eIncidentLocal(), box.newPool())
	e2ePrime(t, laptop.server.RemotePool) // all three published idle
	for _, account := range []string{"x1", "x2", "x3"} {
		defer box.live(t, account)()
	}
	box.calibrate(t, map[string]string{"x1": selectacct.VerdictAtCap, "x2": selectacct.VerdictAtCap, "x3": selectacct.VerdictAtCap})

	got := laptop.turn(t, "s1")
	if got.Status != http.StatusOK || !e2eLocalAccount(got.ServedBy) {
		t.Fatalf("status %d served by %q; want local after the bound", got.Status, got.ServedBy)
	}
	turns := box.seen()
	if len(turns) != 2 {
		t.Fatalf("box turns %+v; want exactly 2 attempts", turns)
	}
	for _, turn := range turns {
		if turn.Status != http.StatusConflict || turn.Target == "" {
			t.Fatalf("box turn %+v; want a targeted, refused attempt", turn)
		}
	}
	if hits := box.upstream.accounts(); len(hits) != 0 {
		t.Fatalf("box upstream hits %v; every attempt was refused before Anthropic", hits)
	}
	decisions := laptop.decisions()
	if reason, _ := decisions[len(decisions)-1]["reason"].(string); !strings.Contains(reason, "refusal bound reached") {
		t.Fatalf("last decision %v; want the bound named", decisions[len(decisions)-1])
	}
}

// P4: a session sent to the box is tied to the ACCOUNT it was sent for. Every
// held turn targets chip-5; when chip-5's window is spent (Anthropic answers
// rejected) the box passes the answer back stamped refused -- it does not
// fail over on its own -- and the laptop re-ranks BOTH pools, lands where the
// one ranking says, and stays there.
func TestE2EHeldSessionWhoseAccountRunsOutIsRerankedAcrossBothPools(t *testing.T) {
	now := time.Now()
	chip5 := e2eUsage(now, 10, 150*time.Minute, 96, 16*time.Hour)
	for _, tc := range []struct {
		name  string
		usage map[string]string
		want  string
	}{
		{"local now outranks what is left on the box", map[string]string{"chip-5": chip5, "b3d": e2eUsage(now, 0, 4*time.Hour, 0, 72*time.Hour)}, "loc5d"},
		{"another box account outranks local", map[string]string{"chip-5": chip5, "b2d": e2eUsage(now, 0, 4*time.Hour, 0, 48*time.Hour)}, "b2d"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := newE2EBox(t, tc.usage)
			laptop := newE2ELaptop(t, e2eFiveDays(), box.newPool())
			e2ePrime(t, laptop.server.RemotePool)

			if got := laptop.turn(t, "s1"); got.ServedBy != "chip-5" {
				t.Fatalf("turn 1 served by %q; want chip-5 (burndown)", got.ServedBy)
			}
			if account, held := e2eHold(laptop.server.RemotePool, "s1"); !held || account != "chip-5" {
				t.Fatalf("hold after turn 1 = %q/%v; want chip-5", account, held)
			}
			box.upstream.setAnswer(func(account string, w http.ResponseWriter) bool {
				if account == "chip-5" {
					e2eRejected(w)
					return true
				}
				return false
			})
			got := laptop.turn(t, "s1")
			if got.Status != http.StatusOK || got.ServedBy != tc.want {
				t.Fatalf("turn 2 status %d served by %q; want %s by the re-rank", got.Status, got.ServedBy, tc.want)
			}
			turns := box.seen()
			if len(turns) < 2 || turns[1].Target != "chip-5" || turns[1].Refused != selectacct.VerdictExhausted {
				t.Fatalf("box turns %+v; turn 2 must target chip-5 and come back refused exhausted", turns)
			}
			for _, hit := range box.upstream.accounts()[1:] {
				if hit == tc.want {
					continue
				}
				if hit != "chip-5" {
					t.Fatalf("box upstream hits %v; the box substituted on its own", box.upstream.accounts())
				}
			}
			third := laptop.turn(t, "s1")
			if third.ServedBy != tc.want {
				t.Fatalf("turn 3 served by %q; the session must stay where the re-rank put it (%s)", third.ServedBy, tc.want)
			}
			after := box.seen()
			if tc.want == "loc5d" {
				if len(after) != len(turns) {
					t.Fatalf("box turns after landing local: %+v", after)
				}
				if _, held := e2eHold(laptop.server.RemotePool, "s1"); held {
					t.Fatal("the hold outlived the forced switch")
				}
			} else if last := after[len(after)-1]; last.Target != tc.want {
				t.Fatalf("turn 3 box turn %+v; want it targeted at %s", last, tc.want)
			}
		})
	}
}

// P5's intent on the box side of a hold, and the cap for NEW placements at the
// same moment: once chip-5 carries another live session it is at its cap, and
// the laptop's fresh body says so. A new session stays local; the WARM session
// already on chip-5 keeps it (the continuing-session criterion is eviction, not
// the new-session floor or the cap).
func TestE2EWarmHeldSessionIsNotMovedForTheNewSessionFloor(t *testing.T) {
	box := e2eIncidentBox(t)
	laptop := newE2ELaptop(t, e2eIncidentLocal(), box.newPool())
	e2ePrime(t, laptop.server.RemotePool)
	if got := laptop.turn(t, "warm"); got.ServedBy != "chip-5" {
		t.Fatalf("turn 1 served by %q; want chip-5", got.ServedBy)
	}
	defer box.live(t, "chip-5")()
	e2eRefetch(t, laptop.server.RemotePool)
	box.calibrate(t, map[string]string{"chip-5": selectacct.VerdictAtCap})

	if got := laptop.turn(t, "new"); got.Status != http.StatusOK || !e2eLocalAccount(got.ServedBy) {
		t.Fatalf("a new session: status %d served by %q; chip-5 is at its cap", got.Status, got.ServedBy)
	}
	got := laptop.turn(t, "warm")
	if got.Status != http.StatusOK || got.ServedBy != "chip-5" {
		t.Fatalf("warm turn 2: status %d served by %q; a warm session keeps its account short of eviction", got.Status, got.ServedBy)
	}
	var warm []e2eBoxTurn
	for _, turn := range box.seen() {
		if turn.Session == "warm" {
			warm = append(warm, turn)
		}
	}
	if len(warm) != 2 || warm[1].Target != "chip-5" || warm[1].Status != http.StatusOK {
		t.Fatalf("box turns for the warm session %+v; want turn 2 targeted at chip-5 and served", warm)
	}
}

// P8: transient trouble keeps the account; only a forced switch recomputes.
func TestE2ETransientTroubleKeepsThePinAndASpentWindowRecomputes(t *testing.T) {
	now := time.Now()
	// fourteen 100%/5d = 280 with the 2x; fifteen 50%/6.25d = 56, x2 = 112;
	// the box's b3d 100%/3d = 233 sits between them.
	local := func() map[string]string {
		return map[string]string{
			"fourteen": e2eUsage(now, 0, 4*time.Hour, 0, 120*time.Hour),
			"fifteen":  e2eUsage(now, 0, 4*time.Hour, 50, 150*time.Hour),
		}
	}
	newPair := func(t *testing.T) (*e2eBox, *e2eLaptop) {
		box := newE2EBox(t, map[string]string{"b3d": e2eUsage(now, 0, 4*time.Hour, 0, 72*time.Hour)})
		laptop := newE2ELaptop(t, local(), box.newPool())
		e2ePrime(t, laptop.server.RemotePool)
		if got := laptop.turn(t, "s1"); got.ServedBy != "fourteen" {
			t.Fatalf("turn 1 served by %q; want fourteen (280 > 233)", got.ServedBy)
		}
		return box, laptop
	}

	t.Run("a 529 on a held turn keeps the hold", func(t *testing.T) {
		box := e2eIncidentBox(t)
		laptop := newE2ELaptop(t, e2eIncidentLocal(), box.newPool())
		e2ePrime(t, laptop.server.RemotePool)
		if got := laptop.turn(t, "s1"); got.ServedBy != "chip-5" {
			t.Fatalf("turn 1 served by %q; want chip-5", got.ServedBy)
		}
		var once sync.Once
		box.upstream.setAnswer(func(account string, w http.ResponseWriter) bool {
			answered := false
			once.Do(func() {
				answered = true
				w.WriteHeader(529)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
			})
			return answered
		})
		laptop.turn(t, "s1") // served or passed through as the 529; either way nothing moves
		if account, held := e2eHold(laptop.server.RemotePool, "s1"); !held || account != "chip-5" {
			t.Fatalf("hold after the 529 = %q/%v; want chip-5 kept", account, held)
		}
		if got := laptop.turn(t, "s1"); got.ServedBy != "chip-5" {
			t.Fatalf("turn 3 served by %q; want chip-5", got.ServedBy)
		}
		for _, turn := range box.seen() {
			if turn.Target != "chip-5" {
				t.Fatalf("box turns %+v; every held turn targets chip-5", box.seen())
			}
		}
	})

	// CHARACTERIZATION (origin/main already served one turn on the peer here).
	t.Run("a refresh blip serves one turn on the box and keeps the local pin", func(t *testing.T) {
		box, laptop := newPair(t)
		laptop.server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
			return account, &url.Error{Op: "Post", URL: "https://platform.claude.com/v1/oauth/token",
				Err: &net.OpError{Op: "dial", Net: "tcp4", Err: errors.New("no such host")}}
		}
		if got := laptop.turn(t, "s1"); got.Status != http.StatusOK || got.Pool != "remote" {
			t.Fatalf("turn 2: status %d pool %q; the blip turn is served on the box", got.Status, got.Pool)
		}
		if pinned := laptop.pinnedTo("s1"); pinned != "fourteen" {
			t.Fatalf("pin after the blip = %q; want fourteen kept", pinned)
		}
		if _, held := e2eHold(laptop.server.RemotePool, "s1"); held {
			t.Fatal("a one-turn serve left a hold on the box")
		}
		laptop.server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
			return account, nil
		}
		if got := laptop.turn(t, "s1"); got.ServedBy != "fourteen" {
			t.Fatalf("turn 3 served by %q; want fourteen", got.ServedBy)
		}
		if turns := box.seen(); len(turns) != 1 {
			t.Fatalf("box turns %+v; want only the blip turn", turns)
		}
	})

	// Upstream #582/#645 (taken): a transient Claude 429 is retried on the
	// same account first, where the prompt cache is, so the one 429 is served
	// by fourteen itself after a short backoff; before the merge it was
	// served by fifteen for this one turn. Either way the pin stays and the
	// peer is not consulted (P8).
	t.Run("a limited-not-spent 429 is retried on its account and keeps the local pin", func(t *testing.T) {
		box, laptop := newPair(t)
		var once sync.Once
		laptop.upstream.setAnswer(func(account string, w http.ResponseWriter) bool {
			answered := false
			if account == "fourteen" {
				once.Do(func() { answered = true; e2eLimited(w) })
			}
			return answered
		})
		if got := laptop.turn(t, "s1"); got.Status != http.StatusOK || got.ServedBy != "fourteen" {
			t.Fatalf("turn 2: status %d served by %q; want fourteen after the same-account retry", got.Status, got.ServedBy)
		}
		if pinned := laptop.pinnedTo("s1"); pinned != "fourteen" {
			t.Fatalf("pin after a limited 429 = %q; a transient 429 must not move the session", pinned)
		}
		if got := laptop.turn(t, "s1"); got.ServedBy != "fourteen" {
			t.Fatalf("turn 3 served by %q; want fourteen", got.ServedBy)
		}
		if turns := box.seen(); len(turns) != 0 {
			t.Fatalf("box turns %+v; a transient failover never consults the peer", turns)
		}
	})

	t.Run("a spent window recomputes across local and the box", func(t *testing.T) {
		box, laptop := newPair(t)
		laptop.upstream.setAnswer(func(account string, w http.ResponseWriter) bool {
			if account == "fourteen" {
				e2eRejected(w)
				return true
			}
			return false
		})
		if got := laptop.turn(t, "s1"); got.Status != http.StatusOK || got.ServedBy != "b3d" {
			t.Fatalf("turn 2: status %d served by %q; forced off fourteen, b3d (233) outranks fifteen (112)", got.Status, got.ServedBy)
		}
		if account, held := e2eHold(laptop.server.RemotePool, "s1"); !held || account != "b3d" {
			t.Fatalf("hold = %q/%v; want b3d", account, held)
		}
		if got := laptop.turn(t, "s1"); got.ServedBy != "b3d" {
			t.Fatalf("turn 3 served by %q; want b3d (held)", got.ServedBy)
		}
		for _, turn := range box.seen() {
			if turn.Target != "b3d" {
				t.Fatalf("box turns %+v; want every one targeted at b3d", box.seen())
			}
		}
	})
}

// P9: the idle timer (warmSessionIdleTTL, an hour) decides WHETHER a
// session re-ranks; the one ranking decides where. Cold on a healthy local
// account that is still the best LOCAL one, it is still weighed against every
// peer account; warm, it stays and the log says warm; a delegated session
// whose hold lapsed on the same timer is a full re-rank too.
func TestE2EColdSessionIsRerankedAcrossEveryPoolAndAWarmOneIsNot(t *testing.T) {
	t.Run("idle 71 minutes on the best local account moves to the box's burndown account", func(t *testing.T) {
		box := e2eIncidentBox(t)
		laptop := newE2ELaptop(t, e2eIncidentLocal(), box.newPool())
		e2ePrime(t, laptop.server.RemotePool)
		if _, err := laptop.store.Put("claude", "s1", "fifteen", ""); err != nil {
			t.Fatal(err)
		}
		markIdle(laptop.active, "s1", warmSessionIdleTTL+time.Minute)
		got := laptop.turn(t, "s1")
		if got.Status != http.StatusOK || got.ServedBy != "chip-5" {
			t.Fatalf("status %d served by %q; a cold session is ranked against the box", got.Status, got.ServedBy)
		}
		if turns := box.seen(); len(turns) != 1 || turns[0].Target != "chip-5" {
			t.Fatalf("box turns %+v; want one, targeted at chip-5", turns)
		}
		decisions := laptop.decisions()
		if len(decisions) != 1 || decisions[0]["session_state"] != "cold" || decisions[0]["target_account"] != "chip-5" ||
			decisions[0]["level"] != "INFO" {
			t.Fatalf("decisions %v; want one cold INFO decision targeting chip-5", decisions)
		}
	})
	// The behaviour is origin/main's; red there only on the session_state key
	// this branch adds to the line (the reason used to say "warm" for cold
	// sessions too).
	t.Run("idle 50 minutes stays and says warm", func(t *testing.T) {
		box := e2eIncidentBox(t)
		laptop := newE2ELaptop(t, e2eIncidentLocal(), box.newPool())
		e2ePrime(t, laptop.server.RemotePool)
		if _, err := laptop.store.Put("claude", "s1", "fifteen", ""); err != nil {
			t.Fatal(err)
		}
		markIdle(laptop.active, "s1", 50*time.Minute)
		if got := laptop.turn(t, "s1"); got.ServedBy != "fifteen" {
			t.Fatalf("served by %q; a warm session keeps its account", got.ServedBy)
		}
		if turns := box.seen(); len(turns) != 0 {
			t.Fatalf("box turns %+v", turns)
		}
		decisions := laptop.decisions()
		if len(decisions) != 1 || decisions[0]["reason"] != "session is warm on its account; staying local" || decisions[0]["session_state"] != "warm" ||
			decisions[0]["level"] != "DEBUG" {
			t.Fatalf("decisions %v; want one DEBUG decision saying warm", decisions)
		}
	})
	t.Run("a delegated session idle past the timer is re-ranked and may land local", func(t *testing.T) {
		box := e2eIncidentBox(t)
		laptop := newE2ELaptop(t, e2eIncidentLocal(), box.newPool())
		key := session.ScopedSessionKey("claude", "s1")
		lapsed := time.Now().Add(-(warmSessionIdleTTL + 10*time.Minute))
		laptop.server.RemotePool.delegations.set(key, lapsed, remotePoolDelegationTTLFor("claude"), "chip-5", laptop.server.RemotePool.name())
		if _, err := box.store.Put("claude", "s1", "chip-5", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := laptop.store.Put("claude", "s1", "fourteen", ""); err != nil {
			t.Fatal(err)
		}
		markIdle(laptop.active, "s1", warmSessionIdleTTL+10*time.Minute)
		defer box.live(t, "chip-5")() // since then chip-5 took its one allowed session
		e2ePrime(t, laptop.server.RemotePool)
		if _, held := e2eHold(laptop.server.RemotePool, "s1"); held {
			t.Fatal("calibration: the hold must have lapsed")
		}
		got := laptop.turn(t, "s1")
		if got.Status != http.StatusOK || got.ServedBy != "fifteen" {
			t.Fatalf("status %d served by %q; want the re-rank's winner, fifteen (chip-5 is at its cap)", got.Status, got.ServedBy)
		}
		if turns := box.seen(); len(turns) != 0 {
			t.Fatalf("box turns %+v; nothing on the box is eligible", turns)
		}
	})
}

// P7: two daemons configured as each other's peer, both publishing verdicts.
// A request that arrived from a peer is served on its target or refused, never
// forwarded again: at most one hop, whatever either pool holds.
func TestE2EMutualPeersNeverLoop(t *testing.T) {
	now := time.Now()
	spent := e2eUsage(now, 100, 2*time.Hour, 50, 100*time.Hour)
	for _, tc := range []struct {
		name     string
		bUsage   string
		wantCode int
	}{
		// CHARACTERIZATION of the hop rule; red on origin/main only because
		// the forward names no target.
		{"B can serve: one targeted hop", e2eUsage(now, 0, 4*time.Hour, 0, 72*time.Hour), http.StatusOK},
		// CHARACTERIZATION: both spent, one hop, a retryable answer.
		{"both spent: one hop, no loop", spent, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newE2EBox(t, map[string]string{"a1": spent})
			b := newE2EBox(t, map[string]string{"b1": tc.bUsage})
			a.server.RemotePool = b.newPool()
			b.server.RemotePool = a.newPool()
			e2ePrime(t, b.server.RemotePool) // A measures itself (its own scheduler)
			e2ePrime(t, a.server.RemotePool) // and B, as A's peer

			got, err := e2eDo(a.srv.URL, "s1", nil)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tc.wantCode {
				t.Fatalf("status %d served by %q; want %d", got.Status, got.ServedBy, tc.wantCode)
			}
			aTurns, bTurns := a.seen(), b.seen()
			if len(aTurns) != 1 || aTurns[0].Hop != "" {
				t.Fatalf("A saw %+v; want only the client's turn", aTurns)
			}
			if len(bTurns) > 1 || (len(bTurns) == 1 && bTurns[0].Hop != "1") {
				t.Fatalf("B saw %+v; want at most one forward at hop 1", bTurns)
			}
			if tc.wantCode == http.StatusOK && (got.ServedBy != "b1" || len(bTurns) != 1 || bTurns[0].Target != "b1") {
				t.Fatalf("served by %q, B saw %+v; want b1, targeted", got.ServedBy, bTurns)
			}
		})
	}
}

// D0: the single-peer configuration (SUBROUTER_REMOTE_POOL, one RemotePool) is
// unchanged: the peer is named by its host:port everywhere it is named -- the
// X-Subrouter-Peer header, the hold, and the decision line's new target_peer
// (the one part origin/main cannot pass: it named no target).
func TestE2ESinglePeerConfigurationIsUnchanged(t *testing.T) {
	now := time.Now()
	box := newE2EBox(t, map[string]string{"b2d": e2eUsage(now, 0, 4*time.Hour, 0, 48*time.Hour)})
	pool := NewRemotePool(box.srv.URL, "", "")
	laptop := newE2ELaptop(t, e2eFiveDays(), pool)
	e2ePrime(t, pool)
	host := mustURL(t, box.srv.URL).Host
	got := laptop.turn(t, "s1")
	if got.ServedBy != "b2d" || got.Pool != "remote" || got.Peer != host {
		t.Fatalf("served by %q pool %q peer %q; want b2d from the peer named %s", got.ServedBy, got.Pool, got.Peer, host)
	}
	hold, ok := pool.delegations.lookup(session.ScopedSessionKey("claude", "s1"), time.Now())
	if !ok || hold.Peer != host || hold.Account != "b2d" {
		t.Fatalf("hold %+v/%v; want b2d on %s", hold, ok, host)
	}
	decisions := laptop.decisions()
	if len(decisions) != 1 || decisions[0]["target_peer"] != host || decisions[0]["target_account"] != "b2d" {
		t.Fatalf("decisions %v; want one naming %s / b2d", decisions, host)
	}
}

// Mixed versions, new laptop + old box. An old box publishes no verdicts and
// does not know the target header: the laptop ranks it as today (its best
// score, re-derived) and forwards UNTARGETED -- it never sends a header an old
// box would pass upstream. CHARACTERIZATION: origin/main forwarded exactly
// this way; red there only on the decision line's target_legacy key.
func TestE2ENewLaptopWithAnOldBox(t *testing.T) {
	now := time.Now()
	box := newE2EBox(t, map[string]string{
		"chip-5": e2eUsage(now, 10, 150*time.Minute, 96, 16*time.Hour),
		"b3d":    e2eUsage(now, 0, 4*time.Hour, 0, 72*time.Hour),
	})
	box.legacy = true
	laptop := newE2ELaptop(t, e2eIncidentLocal(), box.newPool())
	e2ePrime(t, laptop.server.RemotePool)
	got := laptop.turn(t, "s1")
	if got.Status != http.StatusOK || got.ServedBy != "chip-5" {
		t.Fatalf("status %d served by %q; want today's call: the box, on its own pick", got.Status, got.ServedBy)
	}
	for _, turn := range box.seen() {
		if turn.Target != "" {
			t.Fatalf("an old box was sent a target: %+v", turn)
		}
	}
	decisions := laptop.decisions()
	if len(decisions) != 1 || decisions[0]["target_legacy"] != true {
		t.Fatalf("decisions %v; want one legacy decision", decisions)
	}
}

// Mixed versions, a box that publishes verdicts but does not honour a target
// (D4): the laptop targets b2d and the box serves chip-5 instead. The turn is
// kept -- it is already served -- but the session is not held on an account
// the laptop never ranked, and the next turn re-ranks (the peer is treated as
// legacy until its next body). On origin/main the substitute was held.
func TestE2EPeerThatSubstitutesTheTargetIsReRankedNextTurn(t *testing.T) {
	now := time.Now()
	box := newE2EBox(t, map[string]string{
		"chip-5": e2eUsage(now, 10, 150*time.Minute, 96, 16*time.Hour),
		"b2d":    e2eUsage(now, 0, 4*time.Hour, 0, 48*time.Hour),
	})
	box.ignoreTarget = true
	laptop := newE2ELaptop(t, e2eFiveDays(), box.newPool())
	end := box.live(t, "chip-5") // published at its cap
	e2ePrime(t, laptop.server.RemotePool)
	end() // and free again when the turn arrives: the box's own pick is chip-5
	box.calibrate(t, map[string]string{"chip-5": selectacct.VerdictBurndown})

	got := laptop.turn(t, "s1")
	if got.Status != http.StatusOK || got.ServedBy != "chip-5" {
		t.Fatalf("turn 1 status %d served by %q; the substitute's answer is kept", got.Status, got.ServedBy)
	}
	if turns := box.seen(); len(turns) != 1 || turns[0].Target != "b2d" {
		t.Fatalf("box turns %+v; want one targeted at b2d", turns)
	}
	if _, held := e2eHold(laptop.server.RemotePool, "s1"); held {
		t.Fatal("the session was held on an account the laptop never ranked")
	}
	before := len(laptop.decisions())
	laptop.turn(t, "s1")
	if after := len(laptop.decisions()); after != before+1 {
		t.Fatalf("turn 2 made %d decisions; want a fresh ranking", after-before)
	}
	if turns := box.seen(); len(turns) == 2 && turns[1].Target != "" {
		t.Fatalf("turn 2 targeted a peer seen ignoring targets: %+v", turns[1])
	}
}

// Mixed versions, old laptop + new box. An old laptop forwards untargeted at
// hop 1, exactly as origin/main's forwardToPeer builds it; the new box serves
// it by today's rules (its own pick, sticky), stamps nothing, refuses nothing.
// CHARACTERIZATION: passes on origin/main by construction.
func TestE2EOldLaptopWithANewBox(t *testing.T) {
	now := time.Now()
	box := newE2EBox(t, map[string]string{
		"chip-5": e2eUsage(now, 10, 150*time.Minute, 96, 16*time.Hour),
		"b2d":    e2eUsage(now, 0, 4*time.Hour, 0, 48*time.Hour),
	})
	e2ePrime(t, box.newPool()) // the old laptop's own usage read
	hop := http.Header{}
	hop.Set(remotePoolHopHeader, "1")
	for turn := 1; turn <= 2; turn++ {
		got, err := e2eDo(box.srv.URL, "s1", hop)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != http.StatusOK || got.ServedBy != "chip-5" {
			t.Fatalf("turn %d: status %d served by %q; want the box's own pick, chip-5", turn, got.Status, got.ServedBy)
		}
		if refused := got.Header.Get("X-Subrouter-Target-Refused"); refused != "" {
			t.Fatalf("an untargeted turn was stamped refused (%q)", refused)
		}
	}
	if assignment, ok := box.store.Get("claude", "s1"); !ok || assignment.AccountID != "chip-5" {
		t.Fatalf("box pin %+v; want chip-5", assignment)
	}
	if lines := box.logLines("targeted placement"); len(lines) != 0 {
		t.Fatalf("an untargeted turn logged a targeted placement: %v", lines)
	}
}
