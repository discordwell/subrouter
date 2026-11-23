package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// PATCHES §86.2: an account whose weekly window has not started, through the
// proxy -- the wire, the peer path and the status surfaces.
// selectacct/unstarted_week_test.go proves the fact, the clause and the mark;
// these prove the daemon publishes the fact, reads a peer's, and sets a mark
// wherever it places a new session.

// reasonWeekNotStarted is the clause's decision text, as the operator reads it.
const reasonWeekNotStarted = "its weekly window has not started; one new session starts its clock"

// setStartUnstartedWeekForTest flips §86's process-wide switch for the rest of
// the test. Only for tests that are not t.Parallel.
func setStartUnstartedWeekForTest(t *testing.T, on bool) {
	t.Helper()
	prev := selectacct.StartUnstartedWeek()
	selectacct.SetStartUnstartedWeek(on)
	t.Cleanup(func() { selectacct.SetStartUnstartedWeek(prev) })
}

// codexWeek is a Codex account's weekly window as its usage reading gives it.
func codexWeek(used float64, reset int64) []accounts.UsageWindow {
	return []accounts.UsageWindow{{Name: "primary", UsedPercent: used, LimitWindowSeconds: 604800, ResetAfterSeconds: reset}}
}

// codexWeekScore is the score selection builds from that reading.
func codexWeekScore(id string, used float64, reset int64) selectacct.Score {
	score := scoreFromUsageWindows(accounts.ProviderCodex, id, codexWeek(used, reset))
	score.Fresh = true
	return score
}

// jsonLogLines is every JSON log line with message msg.
func jsonLogLines(logs *syncBuffer, message string) []map[string]any {
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

func peerMarked(server Server, peer *RemotePool, account string) bool {
	_, marked := server.SchedulerRef.StartSentUntil(accounts.ProviderCodex, crossPoolCandidateID(peer.name(), account))
	return marked
}

// --- the wire -------------------------------------------------------------

// usage-status says, per account and per quota pool, whether the week has
// started, as one boolean that is left out when false; a peer reads it into
// the account's score, and a body without it reads as started. It is the
// reading, published whatever the switch and the marks say: the deciding
// daemon keeps its own marks.
func TestUsageStatusPublishesWhetherTheWeekHasStarted(t *testing.T) {
	spark := append(codexWeek(30, 400000), accounts.UsageWindow{
		Name: "primary", Feature: sparkFeature, LimitWindowSeconds: 604800, ResetAfterSeconds: 604800,
	})
	sparkScore := scoreFromUsageWindows(accounts.ProviderCodex, "spark-unstarted", spark)
	sparkScore.Fresh = true
	noWeek := scoreFromUsageWindows(accounts.ProviderClaude, "five-hour-only", []accounts.UsageWindow{shortWindow(10, 9000)})
	noWeek.Fresh = true
	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		codexWeekScore("fresh", 0, 604800), codexWeekScore("midweek", 20, 300000), sparkScore, noWeek,
	}))
	server := Server{SchedulerRef: ref}
	statuses := []AccountUsageStatus{
		{AccountStatus: AccountStatus{ID: "fresh", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth}},
		{AccountStatus: AccountStatus{ID: "midweek", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth}},
		{AccountStatus: AccountStatus{ID: "spark-unstarted", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth}},
		{AccountStatus: AccountStatus{ID: "five-hour-only", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}},
	}
	published := func(t *testing.T) map[string]map[string]any {
		t.Helper()
		body, err := json.Marshal(server.withNormalizedUsage(statuses))
		if err != nil {
			t.Fatal(err)
		}
		var rows []struct {
			ID         string         `json:"id"`
			Normalized map[string]any `json:"normalized"`
		}
		if err := json.Unmarshal(body, &rows); err != nil {
			t.Fatal(err)
		}
		out := map[string]map[string]any{}
		for _, row := range rows {
			out[row.ID] = row.Normalized
		}
		return out
	}
	check := func(t *testing.T) {
		t.Helper()
		rows := published(t)
		if got := rows["fresh"]["weekly_unstarted"]; got != true {
			t.Fatalf("fresh: weekly_unstarted = %v, want true: %v", got, rows["fresh"])
		}
		for _, id := range []string{"midweek", "five-hour-only", "spark-unstarted"} {
			if got, present := rows[id]["weekly_unstarted"]; present {
				t.Fatalf("%s: weekly_unstarted = %v, want it left out", id, got)
			}
		}
		pools, _ := rows["spark-unstarted"]["pools"].(map[string]any)
		pool, _ := pools[selectacct.ModelKey(sparkFeature)].(map[string]any)
		if got := pool["weekly_unstarted"]; got != true {
			t.Fatalf("the Spark pool: weekly_unstarted = %v, want true: its own week has not started (%v)", got, pools)
		}
	}
	check(t)

	// A peer reads the same rows into the account's score.
	body, err := json.Marshal(server.withNormalizedUsage(statuses))
	if err != nil {
		t.Fatal(err)
	}
	var wire []remotePoolAccountUsage
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	read := map[string]selectacct.Score{}
	for _, row := range wire {
		score, ok := row.score(time.Now())
		if !ok {
			t.Fatalf("%s did not score: %+v", row.ID, row.Normalized)
		}
		read[row.ID] = score
	}
	if !read["fresh"].WeeklyUnstarted || read["midweek"].WeeklyUnstarted || read["five-hour-only"].WeeklyUnstarted {
		t.Fatalf("a peer read fresh=%v midweek=%v five-hour-only=%v, want only fresh unstarted",
			read["fresh"].WeeklyUnstarted, read["midweek"].WeeklyUnstarted, read["five-hour-only"].WeeklyUnstarted)
	}
	old := bytes.ReplaceAll(body, []byte(`"weekly_unstarted":true,`), nil)
	if bytes.Contains(old, []byte("weekly_unstarted")) {
		t.Fatalf("calibration: the field is still in %s", old)
	}
	var legacy []remotePoolAccountUsage
	if err := json.Unmarshal(old, &legacy); err != nil {
		t.Fatal(err)
	}
	for _, row := range legacy {
		if score, _ := row.score(time.Now()); score.WeeklyUnstarted {
			t.Fatalf("%s reads unstarted from a body without the field", row.ID)
		}
	}

	t.Run("a start mark here does not hide it", func(t *testing.T) {
		if !ref.NoteNewSession(ref.Get(), accounts.Account{ID: "fresh", Provider: accounts.ProviderCodex}) {
			t.Fatal("calibration: no start mark set")
		}
		check(t)
	})
	t.Run("the switch off here does not hide it", func(t *testing.T) {
		setStartUnstartedWeekForTest(t, false)
		check(t)
	})
}

// --- a Codex daemon measured through its usage read -------------------------

// unstartedCodexDaemon is a Codex daemon over stored accounts, measured
// through its real usage read: the fake usage endpoint answers each account
// with the weekly reading set for it, and the fake upstream names the account
// each turn spent.
type unstartedCodexDaemon struct {
	server   Server
	proxy    *httptest.Server
	upstream *httptest.Server
	logs     *syncBuffer

	mu       sync.Mutex
	readings map[string][2]float64 // account -> used percent, reset seconds
	byAuth   map[string]string     // Authorization header -> account
	served   []string
	// requests is every request the fake upstream received, in order.
	requests []upstreamRequest
	// answer, when set, answers the upstream's requests in place of a
	// completed turn.
	answer http.HandlerFunc
}

// upstreamRequest is one request as the fake upstream received it.
type upstreamRequest struct {
	account string
	path    string
	header  http.Header
	body    []byte
}

func newUnstartedCodexDaemon(t *testing.T, readings map[string][2]float64) *unstartedCodexDaemon {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	for id := range readings {
		if err := store.SaveStored(proxyStoredOAuthAccount(id, strings.Split(id, "@")[0], time.Now().Add(time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	d := &unstartedCodexDaemon{readings: readings, byAuth: map[string]string{}, logs: &syncBuffer{}}
	stored := storedPoolAccounts(t, store)
	for _, account := range stored {
		d.byAuth[account.AuthorizationHeader()] = account.ID
	}
	usage := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		header := http.Header{"Content-Type": {"application/json"}}
		if req.URL.Path != "/backend-api/wham/usage" {
			return &http.Response{StatusCode: http.StatusNotFound, Header: header, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
		}
		d.mu.Lock()
		reading, ok := d.readings[d.byAuth[req.Header.Get("Authorization")]]
		d.mu.Unlock()
		if !ok {
			t.Errorf("usage read for an unknown credential")
		}
		body, _ := json.Marshal(map[string]any{
			"plan_type": "pro",
			"rate_limit": map[string]any{
				"limit_reached": false,
				"primary_window": map[string]any{
					"used_percent": reading[0], "limit_window_seconds": 604800, "reset_after_seconds": int64(reading[1]),
				},
			},
		})
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
	})}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		account := d.byAuth[r.Header.Get("Authorization")]
		d.served = append(d.served, account)
		d.requests = append(d.requests, upstreamRequest{account: account, path: r.URL.Path, header: r.Header.Clone(), body: body})
		answer := d.answer
		d.mu.Unlock()
		if answer != nil {
			answer(w, r)
			return
		}
		codexEgressWriteCompleted(w, account)
	}))
	t.Cleanup(upstream.Close)
	d.upstream = upstream
	upstreamURL, _ := url.Parse(upstream.URL)
	d.server = Server{
		AccountRef:    NewAccountRef(store, stored, usage),
		CodexUpstream: upstreamURL,
		Sessions:      newTestSessionStore(t),
		SchedulerRef:  selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		// Scores change only when the test reads usage again (read).
		UsageScoreTTL:             time.Hour,
		MaxBodyBytes:              1 << 20,
		CodexOverloadFailover:     withoutCapacityRetry(),
		AllowUnauthenticatedAdmin: true,
		Logger:                    slog.New(slog.NewJSONHandler(d.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}.normalized()
	d.proxy = httptest.NewServer(d.server.Handler())
	t.Cleanup(d.proxy.Close)
	d.read(t)
	return d
}

// read makes the daemon read every account's usage again now, as its sweep
// does once the 2-minute window cache has expired.
func (d *unstartedCodexDaemon) read(t *testing.T) {
	t.Helper()
	d.server.AccountRef.InvalidateUsageWindowsCache()
	d.server.AccountRef.InvalidateUsageStatusCache()
	ctx := context.Background()
	d.server.updateSchedulerFromUsageStatusesContext(ctx, d.server.AccountRef.UsageStatuses(ctx))
}

func (d *unstartedCodexDaemon) setReading(account string, used float64, reset int64) {
	d.mu.Lock()
	d.readings[account] = [2]float64{used, float64(reset)}
	d.mu.Unlock()
}

// newSession sends a new session's first turn and returns the account that
// served it.
func (d *unstartedCodexDaemon) newSession(t *testing.T, sessionID string) string {
	t.Helper()
	d.mu.Lock()
	before := len(d.served)
	d.mu.Unlock()
	status, body := codexEgressPost(t, d.proxy.URL, sessionID)
	d.mu.Lock()
	defer d.mu.Unlock()
	if status != http.StatusOK || len(d.served) != before+1 {
		t.Fatalf("session %s: status %d, upstream hits %v, body %.200q", sessionID, status, d.served[before:], body)
	}
	return d.served[before]
}

func (d *unstartedCodexDaemon) board(t *testing.T) map[string]poolRow {
	t.Helper()
	rows := map[string]poolRow{}
	for _, row := range d.poolBoard(t).Rows {
		rows[row.ID] = row
	}
	return rows
}

// poolBoard is the whole pool.json document.
func (d *unstartedCodexDaemon) poolBoard(t *testing.T) poolBoard {
	t.Helper()
	var board poolBoard
	if err := json.Unmarshal(d.poolJSON(t), &board); err != nil {
		t.Fatal(err)
	}
	return board
}

// poolJSON is pool.json as the daemon serves it.
func (d *unstartedCodexDaemon) poolJSON(t *testing.T) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	d.server.handlePoolData(rec, loopbackRequest(http.MethodGet, "/_subrouter/pool.json", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("pool.json = %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

func (d *unstartedCodexDaemon) usageStatus(t *testing.T) map[string]map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	d.server.handleUsageStatus(rec, loopbackRequest(http.MethodGet, "/_subrouter/usage-status", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("usage-status = %d: %s", rec.Code, rec.Body.String())
	}
	var rows []struct {
		ID         string         `json:"id"`
		Normalized map[string]any `json:"normalized"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, row := range rows {
		out[row.ID] = row.Normalized
	}
	return out
}

const (
	freshAccount   = "fresh@example.com"
	midweekAccount = "midweek@example.com"
)

// The goal end to end on one daemon: the usage endpoint reports a whole week
// left for one account (reset equal to the window's length) and a week with
// budget above even pace for the other. The first new session goes to the
// unstarted account and marks it; the second, a moment later, goes where it
// would without §86; once a reading shows the clock running the account is
// one like any other. usage-status, pool.json and the decision log line say
// so throughout.
func TestAnUnstartedCodexWeekTakesOneNewSession(t *testing.T) {
	setStartUnstartedWeekForTest(t, true)
	d := newUnstartedCodexDaemon(t, map[string][2]float64{
		freshAccount:   {0, 604800},
		midweekAccount: {20, 300000},
	})

	rows := d.board(t)
	fresh, midweek := rows[freshAccount], rows[midweekAccount]
	if !fresh.WeeklyUnstarted || fresh.State != poolStatePristine || fresh.Fuse == nil || fresh.Fuse.State != poolFusePristine ||
		!strings.Contains(fresh.StateNote, "the next new session goes here first") || fresh.Rank != 1 || fresh.Reason != reasonWeekNotStarted {
		t.Fatalf("pool.json row before any session: %+v; want weekly_unstarted, pristine, rank 1 for %q", fresh, reasonWeekNotStarted)
	}
	if midweek.WeeklyUnstarted || midweek.State == poolStatePristine {
		t.Fatalf("pool.json row for a running week: %+v", midweek)
	}
	if got := d.usageStatus(t)[freshAccount]["weekly_unstarted"]; got != true {
		t.Fatalf("usage-status weekly_unstarted = %v, want true", got)
	}

	if got := d.newSession(t, "session-one"); got != freshAccount {
		t.Fatalf("the first new session went to %q, want the unstarted week %q", got, freshAccount)
	}
	if got := d.newSession(t, "session-two"); got != midweekAccount {
		t.Fatalf("the second new session went to %q, want %q: the unstarted week already has its session", got, midweekAccount)
	}
	marks := jsonLogLines(d.logs, startMarkSetMessage)
	if len(marks) != 1 || marks[0]["account"] != freshAccount || marks[0]["peer"] != "" {
		t.Fatalf("start mark log lines %v, want one for %s", marks, freshAccount)
	}
	if note := d.board(t)[freshAccount].StateNote; !strings.Contains(note, "a new session was sent here") {
		t.Fatalf("pool.json note after the session: %q, want it to say the session was sent", note)
	}

	d.setReading(freshAccount, 1, 604000)
	d.read(t)
	if row := d.board(t)[freshAccount]; row.WeeklyUnstarted || row.State == poolStatePristine || row.Fuse.State != poolFuseRunning {
		t.Fatalf("pool.json row once the clock runs: %+v", row)
	}
	if got, present := d.usageStatus(t)[freshAccount]["weekly_unstarted"]; present {
		t.Fatalf("usage-status weekly_unstarted = %v once the clock runs, want it left out", got)
	}
	if got := d.newSession(t, "session-three"); got != midweekAccount {
		t.Fatalf("a new session went to %q with the clock running, want %q on pace", got, midweekAccount)
	}
	if marks := jsonLogLines(d.logs, startMarkSetMessage); len(marks) != 1 {
		t.Fatalf("start mark log lines %v, want only the first session's", marks)
	}
}

// With SUBROUTER_START_UNSTARTED_WEEK=0 the same pool places as before §86,
// and the fact is still published.
func TestTheStartSwitchOffPlacesANewCodexSessionAsBefore(t *testing.T) {
	setStartUnstartedWeekForTest(t, false)
	d := newUnstartedCodexDaemon(t, map[string][2]float64{
		freshAccount:   {0, 604800},
		midweekAccount: {20, 300000},
	})
	if got := d.newSession(t, "session-one"); got != midweekAccount {
		t.Fatalf("the first new session went to %q, want %q on pace with the switch off", got, midweekAccount)
	}
	if marks := jsonLogLines(d.logs, startMarkSetMessage); len(marks) != 0 {
		t.Fatalf("start mark log lines %v with the switch off", marks)
	}
	if got := d.usageStatus(t)[freshAccount]["weekly_unstarted"]; got != true {
		t.Fatalf("usage-status weekly_unstarted = %v with the switch off, want the fact still published", got)
	}
	row := d.board(t)[freshAccount]
	if !row.WeeklyUnstarted || row.State != poolStatePristine || !strings.Contains(row.StateNote, "SUBROUTER_START_UNSTARTED_WEEK=0") {
		t.Fatalf("pool.json row with the switch off: %+v", row)
	}
}

// --- a Codex daemon with a peer ---------------------------------------------

// codexLaptop is a deciding daemon whose local Codex accounts are scored as
// given and whose peer is a contract box speaking for Codex.
type codexLaptop struct {
	server Server
	proxy  *httptest.Server
	logs   *syncBuffer
	mu     sync.Mutex
	served []string
}

func newCodexLaptop(t *testing.T, local []selectacct.Score, peer *RemotePool) *codexLaptop {
	t.Helper()
	l := &codexLaptop{logs: &syncBuffer{}}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-")
		if websocket.IsWebSocketUpgrade(r) {
			serveOneWebSocketTurn(w, r, account)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		l.mu.Lock()
		l.served = append(l.served, account)
		l.mu.Unlock()
		codexEgressWriteCompleted(w, account)
	}))
	t.Cleanup(upstream.Close)
	store := newTestSessionStore(t)
	var list []accounts.Account
	for _, score := range local {
		list = append(list, accounts.Account{ID: score.AccountID, Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "tok-" + score.AccountID})
	}
	active := NewActiveSessions()
	active.startedAt = time.Now().Add(-3 * time.Hour)
	l.server = Server{
		CodexUpstream:  mustURL(t, upstream.URL),
		Accounts:       list,
		Sessions:       store,
		ActiveSessions: active,
		SchedulerRef:   selectacct.NewSchedulerRef(selectacct.NewScheduler(local)),
		RefreshAccountFn: func(_ context.Context, account accounts.Account) (accounts.Account, error) {
			return account, nil
		},
		RemotePool:            peer,
		MaxBodyBytes:          1 << 20,
		CodexOverloadFailover: withoutCapacityRetry(),
		Logger:                slog.New(slog.NewJSONHandler(l.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	l.proxy = httptest.NewServer(l.server.Handler())
	t.Cleanup(l.proxy.Close)
	return l
}

// newSession sends a new session's first turn and returns the local account
// that served it, "" when the box did.
func (l *codexLaptop) newSession(t *testing.T, sessionID string) string {
	t.Helper()
	l.mu.Lock()
	before := len(l.served)
	l.mu.Unlock()
	status, body := codexEgressPost(t, l.proxy.URL, sessionID)
	if status != http.StatusOK {
		t.Fatalf("session %s: status %d body %.200q", sessionID, status, body)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.served) == before {
		return ""
	}
	return l.served[before]
}

// serveOneWebSocketTurn takes a Codex upgrade on account and answers its first
// turn naming the account, then holds the socket until the client closes it.
func serveOneWebSocketTurn(w http.ResponseWriter, r *http.Request, account string) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	if _, _, err := conn.ReadMessage(); err != nil {
		return
	}
	_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","served_by":"`+account+`"}`))
	_, _, _ = conn.ReadMessage()
}

// newWebSocketSession opens a new session's socket, as Codex opens its model
// turns, sends its first turn and returns the account that answered it, local
// or the box's. The socket stays open until the test ends, as a conversation's
// does.
func (l *codexLaptop) newWebSocketSession(t *testing.T, sessionID string) string {
	t.Helper()
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(l.proxy.URL, "http")+"/backend-api/codex/responses",
		http.Header{"Session-Id": []string{sessionID}})
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("session %s: dial: %v (status %d); logs:\n%s", sessionID, err, status, l.logs.String())
	}
	response.Body.Close()
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatalf("session %s: write: %v", sessionID, err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, body, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("session %s: read: %v", sessionID, err)
	}
	return servedByOf(body)
}

func codexBox(t *testing.T, scores ...selectacct.Score) *contractBox {
	t.Helper()
	var held []boxAccount
	for _, score := range scores {
		held = append(held, boxAccount{Score: score})
	}
	box := newContractBox(t, held...)
	box.provider = accounts.ProviderCodex
	return box
}

// Through a peer: the box publishes one account whose week has not started.
// The laptop's first new session is forwarded targeted at it, the decision
// line names the clause, and the laptop marks it per peer; the second goes
// where it would without §86. A box from before §86, or the switch off here,
// changes nothing from before.
func TestANewSessionIsTargetedAtAPeersUnstartedWeekOnce(t *testing.T) {
	setStartUnstartedWeekForTest(t, true)
	setup := func(t *testing.T) (*contractBox, *codexLaptop) {
		box := codexBox(t, codexWeekScore("box-fresh", 0, 604800), codexWeekScore("box-other", 50, 400000))
		laptop := newCodexLaptop(t, []selectacct.Score{codexWeekScore("lap-midweek", 20, 300000)}, box.pool)
		return box, laptop
	}
	box, laptop := setup(t)
	if got := laptop.newSession(t, "session-one"); got != "" {
		t.Fatalf("the first new session was served locally on %q, want the box's unstarted week", got)
	}
	if turns := box.seen(); len(turns) != 1 || turns[0].Target != "box-fresh" || turns[0].Served != "box-fresh" {
		t.Fatalf("box turns %+v, want one targeted at box-fresh", turns)
	}
	decided := false
	for _, line := range jsonLogLines(laptop.logs, "remote pool decision") {
		if line["target_account"] == "box-fresh" && line["clause"] == reasonWeekNotStarted {
			decided = true
		}
	}
	if !decided {
		t.Fatalf("no decision line names %q for box-fresh: %v", reasonWeekNotStarted, jsonLogLines(laptop.logs, "remote pool decision"))
	}
	if got := laptop.newSession(t, "session-two"); got != "lap-midweek" {
		t.Fatalf("the second new session went to %q (box turns %+v), want lap-midweek on pace", got, box.seen())
	}
	if turns := box.seen(); len(turns) != 1 {
		t.Fatalf("box turns %+v, want the second session kept off box-fresh", turns)
	}
	if !peerMarked(laptop.server, box.pool, "box-fresh") {
		t.Fatal("box-fresh carries no start mark on the laptop")
	}
	if marks := jsonLogLines(laptop.logs, startMarkSetMessage); len(marks) != 1 || marks[0]["peer"] != box.pool.name() || marks[0]["account"] != "box-fresh" {
		t.Fatalf("start mark log lines %v, want one for the box's box-fresh", marks)
	}

	t.Run("a box from before §86", func(t *testing.T) {
		box, laptop := setup(t)
		box.withholdUnstarted = true
		if got := laptop.newSession(t, "session-one"); got != "lap-midweek" || len(box.seen()) != 0 {
			t.Fatalf("served on %q, box turns %+v; want lap-midweek as before §86", got, box.seen())
		}
	})
	t.Run("the switch off here", func(t *testing.T) {
		setStartUnstartedWeekForTest(t, false)
		box, laptop := setup(t)
		if got := laptop.newSession(t, "session-one"); got != "lap-midweek" || len(box.seen()) != 0 {
			t.Fatalf("served on %q, box turns %+v; want lap-midweek as before §86", got, box.seen())
		}
	})
	t.Run("a box that serves another account marks nothing", func(t *testing.T) {
		box, laptop := setup(t)
		box.ignoreTarget, box.substitute = true, "box-other"
		if got := laptop.newSession(t, "session-one"); got != "" {
			t.Fatalf("served locally on %q, want the box", got)
		}
		if turns := box.seen(); len(turns) != 1 || turns[0].Target != "box-fresh" || turns[0].Served != "box-other" {
			t.Fatalf("box turns %+v, want box-fresh targeted and box-other serving", turns)
		}
		if peerMarked(laptop.server, box.pool, "box-fresh") || peerMarked(laptop.server, box.pool, "box-other") {
			t.Fatal("a substituted answer left a start mark")
		}
	})
}

// The same over a websocket, the way Codex opens its model turns and holds a
// conversation. The first new session's upgrade is forwarded targeted at the
// box's unstarted account, and the laptop marks it once the box takes the
// upgrade, not when the socket closes: a second new session opened while the
// first is still open goes where it would without §86.
func TestANewWebSocketSessionIsTargetedAtAPeersUnstartedWeekOnce(t *testing.T) {
	setStartUnstartedWeekForTest(t, true)
	box := newVerdictPeer(t, accounts.ProviderCodex,
		verdictPeerAccount{Score: codexWeekScore("box-fresh", 0, 604800)},
		verdictPeerAccount{Score: codexWeekScore("box-other", 50, 400000)})
	pool := box.pool()
	laptop := newCodexLaptop(t, []selectacct.Score{codexWeekScore("lap-midweek", 20, 300000)}, pool)

	if got := laptop.newWebSocketSession(t, "ws-one"); got != "box-fresh" {
		t.Fatalf("the first new websocket session was answered by %q (box turns %+v), want the box's unstarted week", got, box.seen())
	}
	if turns := box.seen(); len(turns) != 1 || !turns[0].WebSocket || turns[0].Target != "box-fresh" {
		t.Fatalf("box turns %+v, want one upgrade targeted at box-fresh", turns)
	}
	if got := laptop.newWebSocketSession(t, "ws-two"); got != "lap-midweek" {
		t.Fatalf("the second new websocket session, opened while the first is open, was answered by %q (box turns %+v), want lap-midweek on pace",
			got, box.seen())
	}
	if !peerMarked(laptop.server, pool, "box-fresh") {
		t.Fatal("box-fresh carries no start mark on the laptop")
	}
	if marks := jsonLogLines(laptop.logs, startMarkSetMessage); len(marks) != 1 || marks[0]["peer"] != pool.name() || marks[0]["account"] != "box-fresh" {
		t.Fatalf("start mark log lines %v, want one for the box's box-fresh", marks)
	}
}

// With no local account that can serve, the box's accounts are ranked alone,
// and the start mark holds there too: the mark is keyed as every ranking keys
// the account, whatever the provider, and the ranking of peers alone carries
// this daemon's marks.
func TestAPeersUnstartedWeekTakesOneSessionWhenLocalCannotServe(t *testing.T) {
	setStartUnstartedWeekForTest(t, true)
	t.Run("codex", func(t *testing.T) {
		box := codexBox(t, codexWeekScore("box-fresh", 0, 604800), codexWeekScore("box-other", 50, 200000))
		laptop := newCodexLaptop(t, nil, box.pool)
		for _, sessionID := range []string{"session-one", "session-two"} {
			if got := laptop.newSession(t, sessionID); got != "" {
				t.Fatalf("%s served locally on %q with no local account", sessionID, got)
			}
		}
		if turns := box.seen(); len(turns) != 2 || turns[0].Target != "box-fresh" || turns[1].Target != "box-other" {
			t.Fatalf("box turns %+v, want box-fresh then box-other", turns)
		}
	})
	t.Run("claude", func(t *testing.T) {
		// The rule reads the window, not the provider: a Claude week that read
		// this way would rank the same.
		claudeWeek := func(id string, used float64, reset int64) selectacct.Score {
			score := scoreFromUsageWindows(accounts.ProviderClaude, id, []accounts.UsageWindow{shortWindow(5, 9000), claudeWeekly(used, reset)})
			score.Fresh = true
			return score
		}
		box := newContractBox(t, boxAccount{Score: claudeWeek("box-fresh", 0, 604800)}, boxAccount{Score: claudeWeek("box-other", 50, 200000)})
		laptop := newCrossPoolLaptop(t, nil, box.pool)
		for _, sessionID := range []string{"session-one", "session-two"} {
			if status, served := laptop.turn(t, sessionID); status != http.StatusOK || served == "" {
				t.Fatalf("%s: status %d served by %q", sessionID, status, served)
			}
		}
		if turns := box.seen(); len(turns) != 2 || turns[0].Target != "box-fresh" || turns[1].Target != "box-other" {
			t.Fatalf("box turns %+v, want box-fresh then box-other", turns)
		}
	})
}

// A refused target marks nothing: the recompute serves the turn elsewhere,
// and once the box's next body clears the refusal, the account still takes
// the next new session first.
func TestARefusedTargetOnAPeersUnstartedWeekMarksNothing(t *testing.T) {
	setStartUnstartedWeekForTest(t, true)
	box := codexBox(t, codexWeekScore("box-fresh", 0, 604800), codexWeekScore("box-other", 50, 400000))
	box.refuse["box-fresh"] = "at_cap"
	laptop := newCodexLaptop(t, []selectacct.Score{codexWeekScore("lap-midweek", 20, 300000)}, box.pool)

	if got := laptop.newSession(t, "session-one"); got != "lap-midweek" {
		t.Fatalf("served on %q, box turns %+v; want the refusal recomputed to lap-midweek", got, box.seen())
	}
	if turns := box.seen(); len(turns) != 1 || turns[0].Target != "box-fresh" || turns[0].Status != http.StatusConflict {
		t.Fatalf("box turns %+v, want one refused target on box-fresh", turns)
	}

	box.mu.Lock()
	delete(box.refuse, "box-fresh")
	box.mu.Unlock()
	box.pool.admin.mu.Lock()
	delete(box.pool.admin.entries, remotePoolUsagePath)
	box.pool.admin.mu.Unlock()
	if got := laptop.newSession(t, "session-two"); got != "" {
		t.Fatalf("served locally on %q, box turns %+v; want box-fresh's priority intact", got, box.seen())
	}
	if turns := box.seen(); len(turns) != 2 || turns[1].Target != "box-fresh" || turns[1].Served != "box-fresh" {
		t.Fatalf("box turns %+v, want the second session targeted at box-fresh", turns)
	}
	if marks := jsonLogLines(laptop.logs, startMarkSetMessage); len(marks) != 1 {
		t.Fatalf("start mark log lines %v, want only the served session's", marks)
	}
}

// Selection's pick only holds the pin while a peer's account is tried: a
// session the box serves leaves the pick unmarked, and one that comes back to
// the pick after a refusal is a new session there after all, so it marks it.
func TestOnlyATurnBackFromAPeerMarksTheLocalUnstartedWeek(t *testing.T) {
	setStartUnstartedWeekForTest(t, true)
	box := codexBox(t, codexWeekScore("box-other", 50, 400000))
	laptop := newCodexLaptop(t, []selectacct.Score{codexWeekScore("lap-fresh", 0, 604800)}, box.pool)
	// A capacity mark ranks the local account behind the box's above the
	// unstarted clause, so the box's account wins first.
	laptop.server.SchedulerRef.MarkCapacityUntil(accounts.ProviderCodex, "lap-fresh", "gpt-6-astra", "", time.Now().Add(time.Hour))
	localMarked := func() bool {
		_, marked := laptop.server.SchedulerRef.StartSentUntil(accounts.ProviderCodex, "lap-fresh")
		return marked
	}

	if got := laptop.newSession(t, "session-one"); got != "" {
		t.Fatalf("served locally on %q, box turns %+v; want box-other", got, box.seen())
	}
	if localMarked() {
		t.Fatal("lap-fresh is marked for a session the box served")
	}

	box.mu.Lock()
	box.refuse["box-other"] = "at_cap"
	box.mu.Unlock()
	if got := laptop.newSession(t, "session-two"); got != "lap-fresh" {
		t.Fatalf("served on %q, box turns %+v; want the refusal to hand the turn to lap-fresh", got, box.seen())
	}
	if turns := box.seen(); len(turns) != 2 || turns[1].Target != "box-other" || turns[1].Status != http.StatusConflict {
		t.Fatalf("box turns %+v, want box-other targeted and refused", turns)
	}
	if !localMarked() {
		t.Fatal("lap-fresh served a new session on its unstarted week and carries no start mark")
	}
}

// The peer half: a daemon that accepts a targeted placement on its own
// unstarted account marks it, so its own next pick does not follow.
func TestAPeerMarksItsUnstartedWeekWhenItAcceptsATarget(t *testing.T) {
	setStartUnstartedWeekForTest(t, true)
	var mu sync.Mutex
	var seen []string
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen = append(seen, token)
		mu.Unlock()
		codexEgressWriteCompleted(w, token)
	}))
	t.Cleanup(pool.Close)
	server := codexOverloadServer(t, mustURL(t, pool.URL), 2, true)
	logs := &syncBuffer{}
	server.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		codexWeekScore("codex-account-0", 0, 604800), codexWeekScore("codex-account-1", 20, 300000),
	}))

	response := codexTargetedPost(t, server, "codex-account-0")
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "served-from-oauth-token-0") {
		t.Fatalf("targeted turn: status %d body %.200q", response.StatusCode, body)
	}
	proxy := httptest.NewServer(server.Handler())
	t.Cleanup(proxy.Close)
	status, body2 := codexEgressPost(t, proxy.URL, "session-two")
	if status != http.StatusOK || !strings.Contains(body2, "served-from-oauth-token-1") {
		t.Fatalf("its own next new session: status %d body %.200q, want codex-account-1", status, body2)
	}
	if marks := jsonLogLines(logs, startMarkSetMessage); len(marks) != 1 || marks[0]["account"] != "codex-account-0" {
		t.Fatalf("start mark log lines %v, want one for codex-account-0", marks)
	}
	statuses := []AccountUsageStatus{{AccountStatus: AccountStatus{ID: "codex-account-0", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth}}}
	if normalized := server.withNormalizedUsage(statuses)[0].Normalized; normalized == nil || !normalized.WeeklyUnstarted {
		t.Fatalf("published %+v while marked, want the reading still published unstarted", normalized)
	}
}

// A warm session keeps its account; a cold one is ranked again anyway, so the
// clause can place it on an unstarted week, which marks it.
func TestOnlyAColdSessionMovesToAnUnstartedWeek(t *testing.T) {
	setStartUnstartedWeekForTest(t, true)
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		codexEgressWriteCompleted(w, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	}))
	t.Cleanup(pool.Close)
	server := codexOverloadServer(t, mustURL(t, pool.URL), 2, true)
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		codexWeekScore("codex-account-0", 0, 604800), codexWeekScore("codex-account-1", 20, 300000),
	}))
	active := NewActiveSessions()
	active.startedAt = time.Now().Add(-3 * time.Hour)
	server.ActiveSessions = active
	for _, sessionID := range []string{"warm", "cold"} {
		if _, err := server.Sessions.Put("codex", sessionID, "codex-account-1", ""); err != nil {
			t.Fatal(err)
		}
	}
	active.markSeen("codex", "warm")
	proxy := httptest.NewServer(server.Handler())
	t.Cleanup(proxy.Close)

	if status, body := codexEgressPost(t, proxy.URL, "warm"); status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-1") {
		t.Fatalf("warm session: status %d body %.200q, want it kept on codex-account-1", status, body)
	}
	if _, marked := server.SchedulerRef.StartSentUntil(accounts.ProviderCodex, "codex-account-0"); marked {
		t.Fatal("a warm turn marked the unstarted week")
	}
	if status, body := codexEgressPost(t, proxy.URL, "cold"); status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-0") {
		t.Fatalf("cold session: status %d body %.200q, want it ranked onto the unstarted week", status, body)
	}
	if _, marked := server.SchedulerRef.StartSentUntil(accounts.ProviderCodex, "codex-account-0"); !marked {
		t.Fatal("the cold session's move onto the unstarted week set no start mark")
	}
}

// --- the board's note for a box row ------------------------------------------

// A box row reads the box's published fact: a box from before §86 publishes
// none, so this daemon gives it no priority, and a box account this daemon
// sent a session to is off the priority until its mark expires.
func TestTheBoardSaysWhatThisDaemonDoesWithABoxsUnstartedWeek(t *testing.T) {
	setStartUnstartedWeekForTest(t, true)
	box := codexBox(t, codexWeekScore("box-fresh", 0, 604800))
	server := Server{RemotePool: box.pool, SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))}
	status := func(published bool) AccountUsageStatus {
		return AccountUsageStatus{
			AccountStatus: AccountStatus{ID: "box-fresh", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
			Windows:       codexWeek(0, 604800),
			Normalized:    &NormalizedUsage{Fresh: true, WeeklyUnstarted: published},
		}
	}
	note := func(published bool) string {
		row := &poolRow{}
		applyPoolReadings(row, status(published), schedulerFromStatuses([]AccountUsageStatus{status(published)}, time.Now()), true, time.Now())
		server.noteUnstartedRow(row, status(published), poolNameBox, time.Now())
		if row.State != poolStatePristine || !row.WeeklyUnstarted {
			t.Fatalf("box row %+v, want pristine and weekly_unstarted", row)
		}
		return row.StateNote
	}
	if got := note(true); !strings.Contains(got, "the next new session goes here first") {
		t.Fatalf("published, unmarked: %q", got)
	}
	if got := note(false); !strings.Contains(got, "does not publish weekly_unstarted") {
		t.Fatalf("a box from before §86: %q", got)
	}
	candidate := crossPoolCandidate{Peer: box.pool, Account: "box-fresh", Provider: accounts.ProviderCodex, Score: codexWeekScore("box-fresh", 0, 604800)}
	req := withPeerStart(httptest.NewRequest(http.MethodPost, "/responses", nil), server, candidate, "codex", "s1")
	notePeerTargetServed(req.Context(), box.pool, "box-fresh", peerForwardOutcome{Served: true})
	if got := note(true); !strings.Contains(got, "a new session was sent here") {
		t.Fatalf("marked: %q", got)
	}
}
