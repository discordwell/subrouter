package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// A subagent's Fable turn in a session pinned to an account whose Fable meter
// is spent, but which still serves Opus, is served as Opus on that account.
// The session keeps its pin: no re-rank, no move. A main-thread Fable turn may
// still move the session to an account with Fable left. Owner, 2026-11-20:
// "Ideally, it doesn't switch. It just pins to Opus. The only thing that
// forces you to move to a Fable box is if you're switching the main model
// from Opus [to] Fable."
//
// Measured 2026-11-20 (session 5b3cd8a6): a subagent's Fable turn, held on
// the box account sixteen (Fable 100 % spent, week 73 %), was refused 409
// exhausted, the hold ended, the turn was re-ranked onto another box account
// with Fable left, and the main thread followed it there on a cold cache.

const (
	fablePinSession = "fable-pin-session"
	fablePinAgentID = "a7f3e9c2b1d04e5f"
)

// fablePinBox is a peer that publishes verdict rows (verdictPeer's body) and
// answers a targeted turn as a §31 daemon does: a Fable turn on an account
// whose Fable pool is spent is refused before upstream, 409 exhausted, and
// every other turn is served on exactly the target.
type fablePinBox struct {
	mu        sync.Mutex
	published []verdictPeerAccount
	// fableSpent: accounts that refuse a targeted FABLE turn, whatever the
	// published body said (it changed after the publish).
	fableSpent map[string]bool
	// refuse: accounts that refuse every targeted turn with this reason.
	refuse map[string]string
	turns  []fablePinBoxTurn
	pool   *RemotePool
	// A gate (holdNext) keeps one turn in flight: the first turn of gateAgent
	// whose model contains gateModel signals arrived, then waits for release
	// before it is answered.
	gateAgent, gateModel string
	gateOnce             sync.Once
	arrived, release     chan struct{}
}

// holdNext gates the next turn of agentID whose model contains model.
func (b *fablePinBox) holdNext(agentID, model string) {
	b.gateAgent, b.gateModel = agentID, model
	b.arrived, b.release = make(chan struct{}), make(chan struct{})
}

type fablePinBoxTurn struct {
	Target string
	Model  string
	Status int
	Body   string
}

func newFablePinBox(t *testing.T, published ...verdictPeerAccount) *fablePinBox {
	t.Helper()
	box := &fablePinBox{published: published, fableSpent: map[string]bool{}, refuse: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(box.serve))
	t.Cleanup(srv.Close)
	box.pool = NewRemotePool(srv.URL, "", "")
	return box
}

// fablePinBoxAccount is a healthy box account whose Fable pool is published
// spent or healthy; its Opus pool is healthy either way.
func fablePinBoxAccount(id string, fableSpent bool) verdictPeerAccount {
	healthy := crossPoolClaudeScore(id, 0.9, 50*time.Hour)
	fable := healthy
	if fableSpent {
		fable.Headroom, fable.ShortHeadroom, fable.WeeklyHeadroom, fable.WeeklyRemaining = 0, 0, 0, 0
	}
	return verdictPeerAccount{Score: healthy, Pools: map[string]selectacct.Score{
		selectacct.ModelKey(agentclaude.FableFeature): fable,
		selectacct.ModelKey(agentclaude.OpusFeature):  healthy,
	}}
}

func (b *fablePinBox) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == remotePoolUsagePath {
		b.mu.Lock()
		defer b.mu.Unlock()
		_, _ = w.Write((&verdictPeer{provider: accounts.ProviderClaude, accounts: b.published}).usageBody())
		return
	}
	body, _ := io.ReadAll(r.Body)
	var payload struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &payload)
	target := r.Header.Get(remotePoolTargetAccountHeader)
	if b.gateAgent != "" && r.Header.Get("X-Claude-Code-Agent-Id") == b.gateAgent && strings.Contains(payload.Model, b.gateModel) {
		gated := false
		b.gateOnce.Do(func() { gated = true })
		if gated {
			close(b.arrived)
			<-b.release
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	record := func(status int) {
		b.turns = append(b.turns, fablePinBoxTurn{Target: target, Model: payload.Model, Status: status, Body: string(body)})
	}
	if reason, refused := b.refuse[target]; refused {
		record(http.StatusConflict)
		writeTargetRefusal(w, target, reason, true, nil)
		return
	}
	if claudeFableModel(payload.Model) && b.fableSpent[target] {
		record(http.StatusConflict)
		writeTargetRefusal(w, target, selectacct.VerdictExhausted, true, nil)
		return
	}
	account := target
	if account == "" && len(b.published) > 0 {
		account = b.published[0].Score.AccountID
	}
	record(http.StatusOK)
	w.Header().Set(subrouterAccountHeader, account)
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"msg_box","served_by":"`+account+`"}`)
}

func (b *fablePinBox) seen() []fablePinBoxTurn {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]fablePinBoxTurn(nil), b.turns...)
}

// postFablePinTurn posts one Claude turn of model; a non-empty agentID marks it
// a subagent's, the two headers Claude Code sends on every subagent request
// and never on the main thread's.
func postFablePinTurn(t *testing.T, handler http.Handler, sessionID, model, agentID string) (int, string) {
	t.Helper()
	front := newProxyTestServer(t, handler)
	defer front.Close()
	got := fablePinTurnAt(front.URL, sessionID, model, agentID)
	if got.err != nil {
		t.Fatal(got.err)
	}
	return got.status, got.servedBy
}

type fablePinTurnResult struct {
	status   int
	servedBy string
	err      error
}

// fablePinTurnAt posts one turn to a running front; safe off the test
// goroutine.
func fablePinTurnAt(url, sessionID, model, agentID string) fablePinTurnResult {
	req, err := http.NewRequest(http.MethodPost, url+"/v1/messages", strings.NewReader(`{"model":"`+model+`","max_tokens":8,"messages":[]}`))
	if err != nil {
		return fablePinTurnResult{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", sessionID)
	if agentID != "" {
		req.Header.Set("X-Claude-Code-Agent-Id", agentID)
		req.Header.Set("X-Claude-Code-Agent-Type", "workflow-subagent")
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return fablePinTurnResult{err: err}
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return fablePinTurnResult{status: response.StatusCode, servedBy: servedByOf(body)}
}

// fablePinHeldLaptop is the incident's laptop: one local account it cannot
// serve this turn on (Fable and Opus both spent, as the held-back account
// was), and the session held on the box's sixteen.
func fablePinHeldLaptop(t *testing.T, box *fablePinBox) *crossPoolLaptop {
	t.Helper()
	laptop := newCrossPoolLaptop(t, opusPoolScores(0, 0, "loc"), box.pool)
	holdOn(t, box.pool, fablePinSession, "sixteen")
	// A fresh body in the cache, as the box's last publish.
	box.pool.AdminGet(context.Background(), remotePoolUsagePath)
	return laptop
}

func fablePinTurnsString(turns []fablePinBoxTurn) string {
	var out []string
	for _, turn := range turns {
		out = append(out, turn.Target+"/"+turn.Model+"/"+http.StatusText(turn.Status))
	}
	return "[" + strings.Join(out, " ") + "]"
}

func fablePinLogLines(logs string, message string) []map[string]any {
	var out []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(logs))
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var line map[string]any
		if json.Unmarshal(scanner.Bytes(), &line) == nil && line["msg"] == message {
			out = append(out, line)
		}
	}
	return out
}

const fablePinLogMessage = "claude subagent fable served as opus on pinned account"

func requireFablePinLog(t *testing.T, logs, path, remote, account string) {
	t.Helper()
	lines := fablePinLogLines(logs, fablePinLogMessage)
	if len(lines) != 1 {
		t.Fatalf("%d %q lines, want 1:\n%s", len(lines), fablePinLogMessage, logs)
	}
	line := lines[0]
	// A local pin names no peer: the field is absent.
	var wantRemote any = remote
	if remote == "" {
		wantRemote = nil
	}
	if line["level"] != "INFO" || line["path"] != path || line["session"] != fablePinSession ||
		line["agent_id"] != fablePinAgentID || line["remote"] != wantRemote || line["account"] != account {
		t.Fatalf("log line %v; want INFO, path=%s, session=%s, agent_id=%s, remote=%s, account=%s",
			line, path, fablePinSession, fablePinAgentID, remote, account)
	}
}

// (a) The box's cached verdict already says sixteen's Fable pool is spent and
// its Opus pool serves: the subagent's Fable turn goes there as Opus, targeted
// at sixteen, before any Fable attempt, and the hold stays.
func TestASubagentFableTurnHeldOnAFableSpentAccountIsForwardedThereAsOpus(t *testing.T) {
	box := newFablePinBox(t, fablePinBoxAccount("sixteen", true), fablePinBoxAccount("acct-p", false))
	box.fableSpent["sixteen"] = true
	laptop := fablePinHeldLaptop(t, box)

	status, servedBy := postFablePinTurn(t, laptop.server.Handler(), fablePinSession, "claude-fable-5-2[1m]", fablePinAgentID)
	if status != http.StatusOK || servedBy != "sixteen" {
		t.Fatalf("status %d served by %q, box turns %s; want sixteen, as Opus", status, servedBy, fablePinTurnsString(box.seen()))
	}
	turns := box.seen()
	if len(turns) != 1 || turns[0].Target != "sixteen" || turns[0].Model != "claude-opus-5-5[1m]" || turns[0].Status != http.StatusOK {
		t.Fatalf("box turns %s; want exactly one, an Opus turn targeted at sixteen (the [1m] suffix kept)", fablePinTurnsString(turns))
	}
	if account, held := holdAccount(box.pool, fablePinSession); !held || account != "sixteen" {
		t.Fatalf("hold = %q (%v); the session must stay held on sixteen", account, held)
	}
	requireFablePinLog(t, laptop.logs.String(), "pre_send", box.pool.name(), "sixteen")
}

// (b) The same turn from the main thread keeps today's behaviour: it is sent
// as Fable, sixteen refuses it, the hold ends, and the turn is re-ranked onto
// the box account with Fable left.
func TestAMainThreadFableTurnHeldOnAFableSpentAccountStillMoves(t *testing.T) {
	box := newFablePinBox(t, fablePinBoxAccount("sixteen", true), fablePinBoxAccount("acct-p", false))
	box.fableSpent["sixteen"] = true
	laptop := fablePinHeldLaptop(t, box)

	status, servedBy := postFablePinTurn(t, laptop.server.Handler(), fablePinSession, "claude-fable-5-2[1m]", "")
	if status != http.StatusOK || servedBy != "acct-p" {
		t.Fatalf("status %d served by %q, box turns %s; want the turn re-ranked onto acct-p", status, servedBy, fablePinTurnsString(box.seen()))
	}
	turns := box.seen()
	if len(turns) != 2 || turns[0].Target != "sixteen" || turns[0].Model != "claude-fable-5-2[1m]" || turns[0].Status != http.StatusConflict ||
		turns[1].Target != "acct-p" || !claudeFableModel(turns[1].Model) || turns[1].Status != http.StatusOK {
		t.Fatalf("box turns %s; want sixteen refusing the Fable turn, then acct-p serving Fable", fablePinTurnsString(turns))
	}
	if account, held := holdAccount(box.pool, fablePinSession); !held || account != "acct-p" {
		t.Fatalf("hold = %q (%v); want it moved to acct-p", account, held)
	}
	if lines := fablePinLogLines(laptop.logs.String(), fablePinLogMessage); len(lines) != 0 {
		t.Fatalf("a main-thread turn logged a substitution: %v", lines)
	}
}

// (c) The cached body still shows sixteen's Fable pool healthy (it was spent
// after the publish), so the Fable turn goes out and sixteen refuses it 409
// exhausted. A subagent's turn is sent once more, to sixteen, as Opus, and the
// hold stays; the main thread's next turn is still on sixteen. Any other
// refusal ends the hold as before, and so does an Opus refusal.
func TestASubagentFableTurnRefusedForItsFablePoolIsResentAsOpusToTheSameAccount(t *testing.T) {
	t.Run("fable spent, opus serves", func(t *testing.T) {
		box := newFablePinBox(t, fablePinBoxAccount("sixteen", false), fablePinBoxAccount("acct-p", false))
		box.fableSpent["sixteen"] = true
		laptop := fablePinHeldLaptop(t, box)

		status, servedBy := postFablePinTurn(t, laptop.server.Handler(), fablePinSession, "claude-fable-5", fablePinAgentID)
		if status != http.StatusOK || servedBy != "sixteen" {
			t.Fatalf("status %d served by %q, box turns %s; want sixteen, as Opus", status, servedBy, fablePinTurnsString(box.seen()))
		}
		turns := box.seen()
		if len(turns) != 2 || turns[0].Target != "sixteen" || turns[0].Model != "claude-fable-5" || turns[0].Status != http.StatusConflict ||
			turns[1].Target != "sixteen" || turns[1].Model != "claude-opus-5-5" || turns[1].Status != http.StatusOK {
			t.Fatalf("box turns %s; want sixteen refusing Fable, then sixteen serving the same turn as Opus", fablePinTurnsString(turns))
		}
		// claudeOpusBody keeps every other field's value byte for byte (the
		// object's keys come back sorted).
		var fable, opus map[string]json.RawMessage
		if json.Unmarshal([]byte(turns[0].Body), &fable) != nil || json.Unmarshal([]byte(turns[1].Body), &opus) != nil || len(fable) != len(opus) {
			t.Fatalf("the Opus resend is not the same request: %q -> %q", turns[0].Body, turns[1].Body)
		}
		for key, value := range fable {
			if key != "model" && string(opus[key]) != string(value) {
				t.Fatalf("the Opus resend changed %q: %q -> %q", key, turns[0].Body, turns[1].Body)
			}
		}
		if account, held := holdAccount(box.pool, fablePinSession); !held || account != "sixteen" {
			t.Fatalf("hold = %q (%v); the session must stay held on sixteen", account, held)
		}
		requireFablePinLog(t, laptop.logs.String(), "after_refusal", box.pool.name(), "sixteen")
		if lines := laptop.logLines("remote pool hold ended"); len(lines) != 0 {
			t.Fatalf("the hold was reported ended: %v", lines)
		}

		// The main thread's next turn finds the session where it was.
		response := postClaudeTurn(t, laptop.server.Handler(), fablePinSession)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("main-thread turn status %d", response.StatusCode)
		}
		if turns := box.seen(); len(turns) != 3 || turns[2].Target != "sixteen" || turns[2].Status != http.StatusOK {
			t.Fatalf("box turns %s; the main thread's next turn must go to sixteen", fablePinTurnsString(turns))
		}
	})
	for _, tc := range []struct {
		name   string
		reason string
		// opusTried: the refusal reads as Fable-spent, so Opus is asked once.
		opusTried bool
	}{
		{name: "dead credential", reason: targetRefusedAuthInvalid},
		{name: "whole account spent", reason: selectacct.VerdictExhausted, opusTried: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := newFablePinBox(t, fablePinBoxAccount("sixteen", false), fablePinBoxAccount("acct-p", false))
			box.refuse["sixteen"] = tc.reason
			laptop := fablePinHeldLaptop(t, box)

			status, servedBy := postFablePinTurn(t, laptop.server.Handler(), fablePinSession, "claude-fable-5", fablePinAgentID)
			if status != http.StatusOK || servedBy != "acct-p" {
				t.Fatalf("status %d served by %q, box turns %s; want the forced switch onto acct-p", status, servedBy, fablePinTurnsString(box.seen()))
			}
			var sixteen []fablePinBoxTurn
			for _, turn := range box.seen() {
				if turn.Target == "sixteen" {
					sixteen = append(sixteen, turn)
				}
			}
			want := 1
			if tc.opusTried {
				want = 2
			}
			if len(sixteen) != want || sixteen[0].Model != "claude-fable-5" || (tc.opusTried && sixteen[1].Model != "claude-opus-5-5") {
				t.Fatalf("box turns %s; want sixteen asked for Fable%s", fablePinTurnsString(box.seen()), map[bool]string{true: " and then once for Opus", false: " only"}[tc.opusTried])
			}
			if account, held := holdAccount(box.pool, fablePinSession); !held || account != "acct-p" {
				t.Fatalf("hold = %q (%v); want it moved to acct-p", account, held)
			}
			if lines := fablePinLogLines(laptop.logs.String(), fablePinLogMessage); len(lines) != 0 {
				t.Fatalf("logged a substitution that did not serve: %v", lines)
			}
		})
	}
}

// A turn in flight on the held account while the main thread moves the
// session must not move it back when it answers (PATCHES §71.4). The main
// thread's Fable turn is refused on sixteen and the session moves to acct-p;
// then the in-flight turn answers. Its Opus answer used to set the hold back
// to sixteen, and its Fable refusal used to end acct-p's hold and restore
// sixteen's, so the main thread's next turn went to sixteen to be refused
// first. The same held for any other targeted turn in flight.
func TestATurnInFlightOnTheHeldAccountDoesNotUndoTheMainThreadsMove(t *testing.T) {
	for _, tc := range []struct {
		name string
		// fableSpentPublished: the cached body already shows sixteen's Fable
		// spent, so the subagent's turn goes as Opus (pre_send); otherwise it
		// goes as Fable and is refused (after_refusal).
		fableSpentPublished bool
		model, gateModel    string
		wantServedBy        string
	}{
		{name: "subagent fable sent as opus", fableSpentPublished: true, model: "claude-fable-5", gateModel: "opus", wantServedBy: "sixteen"},
		// The refusal ended no hold (the session is on acct-p now): the turn
		// is not resent to sixteen as Opus, it follows the session.
		{name: "subagent fable refused", model: "claude-fable-5", gateModel: "fable", wantServedBy: "acct-p"},
		{name: "any other turn", fableSpentPublished: true, model: "claude-opus-4-8", gateModel: "opus", wantServedBy: "sixteen"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := newFablePinBox(t, fablePinBoxAccount("sixteen", tc.fableSpentPublished), fablePinBoxAccount("acct-p", false))
			box.fableSpent["sixteen"] = true
			box.holdNext(fablePinAgentID, tc.gateModel)
			laptop := fablePinHeldLaptop(t, box)
			front := newProxyTestServer(t, laptop.server.Handler())

			inFlight := make(chan fablePinTurnResult, 1)
			go func() { inFlight <- fablePinTurnAt(front.URL, fablePinSession, tc.model, fablePinAgentID) }()
			select {
			case <-box.arrived:
			case <-time.After(20 * time.Second):
				t.Fatalf("the in-flight turn never reached the box; turns %s", fablePinTurnsString(box.seen()))
			}

			main := fablePinTurnAt(front.URL, fablePinSession, "claude-fable-5", "")
			if main.err != nil || main.status != http.StatusOK || main.servedBy != "acct-p" {
				t.Fatalf("calibration: main-thread Fable turn %+v, box turns %s; want it moved to acct-p", main, fablePinTurnsString(box.seen()))
			}
			if account, held := holdAccount(box.pool, fablePinSession); !held || account != "acct-p" {
				t.Fatalf("calibration: after the main thread's move the hold is %q (%v); want acct-p", account, held)
			}

			close(box.release)
			var got fablePinTurnResult
			select {
			case got = <-inFlight:
			case <-time.After(20 * time.Second):
				t.Fatal("the in-flight turn never returned")
			}
			if got.err != nil || got.status != http.StatusOK || got.servedBy != tc.wantServedBy {
				t.Fatalf("in-flight turn %+v, box turns %s; want it served by %s", got, fablePinTurnsString(box.seen()), tc.wantServedBy)
			}
			if account, held := holdAccount(box.pool, fablePinSession); !held || account != "acct-p" {
				t.Fatalf("the in-flight turn left the hold on %q (%v), box turns %s; want acct-p, where the main thread moved it",
					account, held, fablePinTurnsString(box.seen()))
			}

			before := len(box.seen())
			next := fablePinTurnAt(front.URL, fablePinSession, "claude-fable-5", "")
			after := box.seen()[before:]
			if next.err != nil || next.status != http.StatusOK || len(after) != 1 || after[0].Target != "acct-p" || after[0].Status != http.StatusOK {
				t.Fatalf("main thread's next turn %+v went %s; want straight to acct-p", next, fablePinTurnsString(after))
			}
		})
	}
}

// fablePinLocalLaptop is a laptop whose session is pinned, warm, to its own
// account one: Fable spent, Opus healthy. two, when present, has Fable left.
func fablePinLocalLaptop(t *testing.T, peer *RemotePool, withTwo bool) (Server, *[]string, *syncBuffer) {
	t.Helper()
	server, served := opusCrossPoolLaptop(t, peer, 0, false)
	scores := opusPoolScores(0, 0.9, "one")
	if withTwo {
		scores = append(scores, opusPoolScores(0.9, 0.9, "two")...)
	} else {
		server.Accounts = server.Accounts[:1]
	}
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler(scores))
	logs := &syncBuffer{}
	server.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	if _, err := server.Sessions.Put("claude", fablePinSession, "one", ""); err != nil {
		t.Fatal(err)
	}
	markIdle(server.ActiveSessions, fablePinSession, 2*time.Minute)
	return server, served, logs
}

// (d) The session is pinned, warm, to a LOCAL account whose Fable meter is
// spent and which still serves Opus: a subagent's Fable turn is served there
// as Opus, whether the Fable left is on another local account or on a box
// account. (With no local Fable left, §11's Opus stage already kept the box
// case on one; only the log line is new there.) A cold session, and a
// main-thread Fable turn, still move.
func TestASubagentFableTurnPinnedToAFableSpentLocalAccountIsServedThereAsOpus(t *testing.T) {
	t.Run("another local account has fable left", func(t *testing.T) {
		server, served, logs := fablePinLocalLaptop(t, nil, true)
		status, servedBy := postFablePinTurn(t, server.Handler(), fablePinSession, "claude-fable-5", fablePinAgentID)
		if status != http.StatusOK || servedBy != "one" || len(*served) != 1 || (*served)[0] != "one:claude-opus-5-5" {
			t.Fatalf("status %d served by %q (upstream saw %v); want one serving claude-opus-5-5", status, servedBy, *served)
		}
		if pinned, _ := server.Sessions.Get("claude", fablePinSession); pinned.AccountID != "one" {
			t.Fatalf("pin moved to %q; want it kept on one", pinned.AccountID)
		}
		requireFablePinLog(t, logs.String(), "local_pin", "", "one")
	})
	t.Run("a box account has fable left", func(t *testing.T) {
		box := newFablePinBox(t, fablePinBoxAccount("acct-p", false))
		box.pool.AdminGet(context.Background(), remotePoolUsagePath)
		server, served, logs := fablePinLocalLaptop(t, box.pool, false)
		status, servedBy := postFablePinTurn(t, server.Handler(), fablePinSession, "claude-fable-5", fablePinAgentID)
		if status != http.StatusOK || servedBy != "one" || len(*served) != 1 || (*served)[0] != "one:claude-opus-5-5" {
			t.Fatalf("status %d served by %q (upstream saw %v, box %s); want one serving claude-opus-5-5", status, servedBy, *served, fablePinTurnsString(box.seen()))
		}
		if turns := box.seen(); len(turns) != 0 {
			t.Fatalf("box turns %s; the turn must not leave the pinned account", fablePinTurnsString(turns))
		}
		if _, held := holdAccount(box.pool, fablePinSession); held {
			t.Fatal("the session is now held on the box")
		}
		if pinned, _ := server.Sessions.Get("claude", fablePinSession); pinned.AccountID != "one" {
			t.Fatalf("pin moved to %q; want it kept on one", pinned.AccountID)
		}
		requireFablePinLog(t, logs.String(), "local_pin", "", "one")
	})
	t.Run("a cold session is re-ranked as before", func(t *testing.T) {
		server, served, logs := fablePinLocalLaptop(t, nil, true)
		markIdle(server.ActiveSessions, fablePinSession, warmSessionIdleTTL+time.Minute)
		status, servedBy := postFablePinTurn(t, server.Handler(), fablePinSession, "claude-fable-5", fablePinAgentID)
		if status != http.StatusOK || servedBy != "two" || len(*served) != 1 || (*served)[0] != "two:claude-fable-5" {
			t.Fatalf("status %d served by %q (upstream saw %v); a cold session has no cache to keep: want two serving Fable", status, servedBy, *served)
		}
		if lines := fablePinLogLines(logs.String(), fablePinLogMessage); len(lines) != 0 {
			t.Fatalf("a cold session's turn logged a substitution: %v", lines)
		}
	})
	t.Run("a main-thread fable turn still moves", func(t *testing.T) {
		server, served, logs := fablePinLocalLaptop(t, nil, true)
		status, servedBy := postFablePinTurn(t, server.Handler(), fablePinSession, "claude-fable-5", "")
		if status != http.StatusOK || servedBy != "two" || len(*served) != 1 || (*served)[0] != "two:claude-fable-5" {
			t.Fatalf("status %d served by %q (upstream saw %v); want two serving Fable", status, servedBy, *served)
		}
		if lines := fablePinLogLines(logs.String(), fablePinLogMessage); len(lines) != 0 {
			t.Fatalf("a main-thread turn logged a substitution: %v", lines)
		}
	})
}

// (e) A subagent's turn on any other model is forwarded or served exactly as
// the client sent it, on the held account and on the local pin alike.
func TestASubagentNonFableTurnIsUnchanged(t *testing.T) {
	const model = "claude-opus-4-8"
	t.Run("held on the box", func(t *testing.T) {
		box := newFablePinBox(t, fablePinBoxAccount("sixteen", true), fablePinBoxAccount("acct-p", false))
		box.fableSpent["sixteen"] = true
		laptop := fablePinHeldLaptop(t, box)
		status, servedBy := postFablePinTurn(t, laptop.server.Handler(), fablePinSession, model, fablePinAgentID)
		turns := box.seen()
		if status != http.StatusOK || servedBy != "sixteen" || len(turns) != 1 || turns[0].Target != "sixteen" ||
			turns[0].Body != `{"model":"`+model+`","max_tokens":8,"messages":[]}` {
			t.Fatalf("status %d served by %q, box turns %s; want the client's request on sixteen, byte for byte", status, servedBy, fablePinTurnsString(turns))
		}
		if lines := fablePinLogLines(laptop.logs.String(), fablePinLogMessage); len(lines) != 0 {
			t.Fatalf("a non-Fable turn logged a substitution: %v", lines)
		}
	})
	t.Run("pinned locally", func(t *testing.T) {
		server, served, logs := fablePinLocalLaptop(t, nil, true)
		status, servedBy := postFablePinTurn(t, server.Handler(), fablePinSession, model, fablePinAgentID)
		if status != http.StatusOK || servedBy != "one" || len(*served) != 1 || (*served)[0] != "one:"+model {
			t.Fatalf("status %d served by %q (upstream saw %v); want one serving %s", status, servedBy, *served, model)
		}
		if lines := fablePinLogLines(logs.String(), fablePinLogMessage); len(lines) != 0 {
			t.Fatalf("a non-Fable turn logged a substitution: %v", lines)
		}
	})
}
