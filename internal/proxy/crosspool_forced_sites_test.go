package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// PATCHES §31, R3 fix 2 (D7/P10): every FORCED re-rank site ranks through the
// one ranking, over local and every peer. Four sites still ranked LOCAL
// accounts only: a dead credential found by the request-time refresh
// (retryAccount), a model the account cannot serve (the usage failover's
// compatibility reroute), the Codex websocket quota failover, and the
// Fable->Opus substitution -- whose forced-switch ranking weighed peers on the
// FABLE pool after the request had moved to Opus. Each test drives the real
// handler of a deciding daemon against a peer that publishes verdicts and
// serves targets, and each was watched failing on the pre-fix branch with the
// turn served by the local replacement.

// verdictPeerAccount is one account of a fake peer: its top-level score and,
// optionally, per-pool scores (keyed by selectacct.ModelKey). Each score's
// verdict is the shared NewSessionVerdict on that score alone.
type verdictPeerAccount struct {
	Score selectacct.Score
	Pools map[string]selectacct.Score
}

type verdictPeerTurn struct {
	Target    string
	Path      string
	Model     string
	WebSocket bool
	// Body and ContentLength are the HTTP turn as it arrived, byte for byte.
	Body          string
	ContentLength int64
}

// verdictPeer publishes verdict rows for its accounts and serves a targeted
// turn -- HTTP or websocket -- on exactly the target, naming it.
type verdictPeer struct {
	mu       sync.Mutex
	provider accounts.Provider
	accounts []verdictPeerAccount
	turns    []verdictPeerTurn
	srv      *httptest.Server
	// failUsage makes the usage endpoint answer 500 while turns are still
	// served: a peer whose usage-status fails and whose forwards work.
	failUsage atomic.Bool
}

func newVerdictPeer(t *testing.T, provider accounts.Provider, peerAccounts ...verdictPeerAccount) *verdictPeer {
	t.Helper()
	p := &verdictPeer{provider: provider, accounts: peerAccounts}
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.srv.Close)
	return p
}

func verdictPeerNormalized(provider accounts.Provider, id string, score selectacct.Score, now time.Time) map[string]any {
	score.AccountID, score.Provider, score.Fresh = id, provider, true
	verdict := selectacct.NewScheduler([]selectacct.Score{score}).NewSessionVerdict(provider, id, now)
	out := map[string]any{
		"headroom": score.Headroom, "short_headroom": score.ShortHeadroom, "fresh": true,
		"new_session": verdict,
	}
	if !score.WeeklyResetAt.IsZero() {
		out["weekly_headroom"] = score.WeeklyHeadroom
		out["seconds_to_weekly_reset"] = score.WeeklyResetAt.Sub(now).Seconds()
	}
	return out
}

func (p *verdictPeer) usageBody() []byte {
	now := time.Now()
	var rows []map[string]any
	for _, a := range p.accounts {
		normalized := verdictPeerNormalized(p.provider, a.Score.AccountID, a.Score, now)
		if len(a.Pools) > 0 {
			pools := map[string]any{}
			for key, pool := range a.Pools {
				pools[key] = verdictPeerNormalized(p.provider, a.Score.AccountID, pool, now)
			}
			normalized["pools"] = pools
		}
		rows = append(rows, map[string]any{
			"id": a.Score.AccountID, "provider": string(p.provider), "auth_mode": "oauth",
			"auth_checked": true, "auth_valid": true, "normalized": normalized,
		})
	}
	body, _ := json.Marshal(rows)
	return body
}

func (p *verdictPeer) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == remotePoolUsagePath {
		if p.failUsage.Load() {
			http.Error(w, "usage sweep failed", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(p.usageBody())
		return
	}
	target := r.Header.Get(remotePoolTargetAccountHeader)
	if websocket.IsWebSocketUpgrade(r) {
		p.mu.Lock()
		p.turns = append(p.turns, verdictPeerTurn{Target: target, Path: r.URL.Path, WebSocket: true})
		p.mu.Unlock()
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, http.Header{subrouterAccountHeader: []string{target}})
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","served_by":"`+target+`"}`))
		_, _, _ = conn.ReadMessage()
		return
	}
	body, _ := io.ReadAll(r.Body)
	var payload struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &payload)
	p.mu.Lock()
	p.turns = append(p.turns, verdictPeerTurn{Target: target, Path: r.URL.Path, Model: payload.Model, Body: string(body), ContentLength: r.ContentLength})
	p.mu.Unlock()
	w.Header().Set(subrouterAccountHeader, target)
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"msg_peer","served_by":"`+target+`"}`)
}

func (p *verdictPeer) seen() []verdictPeerTurn {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]verdictPeerTurn(nil), p.turns...)
}

func (p *verdictPeer) pool() *RemotePool {
	return NewRemotePool(p.srv.URL, "", "")
}

func servedByOf(body []byte) string {
	var payload struct {
		ServedBy string `json:"served_by"`
	}
	_ = json.Unmarshal(body, &payload)
	return payload.ServedBy
}

// --- a dead credential --------------------------------------------------------

// The request-time refresh finds the session's account dead (invalid_grant).
// retryAccount names the best LOCAL replacement, b; a peer account outranks b
// (though not a, which is why the session was local), so the forced switch
// lands there, targeted.
func TestADeadCredentialIsRankedAcrossEveryPool(t *testing.T) {
	peer := newVerdictPeer(t, accounts.ProviderClaude,
		verdictPeerAccount{Score: crossPoolClaudeScore("p", 0.9, 100*time.Hour)})
	laptop := newCrossPoolLaptop(t, []selectacct.Score{
		crossPoolClaudeScore("a", 0.9, 150*time.Hour),
		crossPoolClaudeScore("b", 0.3, 150*time.Hour),
	}, peer.pool())
	if status, served := laptop.turn(t, "s1"); status != http.StatusOK || served != "a" {
		t.Fatalf("calibration: turn 1 status %d served by %q; want a (it outranks p after the 2x)", status, served)
	}
	laptop.server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		if account.ID == "a" {
			return account, errors.New("Claude OAuth refresh failed: 400 Bad Request: {\"error\":\"invalid_grant\"}")
		}
		return account, nil
	}
	status, served := laptop.turn(t, "s1")
	if status != http.StatusOK || served != "p" {
		t.Fatalf("turn 2 status %d served by %q; a's dead credential is a forced switch and p outranks the local replacement b", status, served)
	}
	turns := peer.seen()
	if len(turns) != 1 || turns[0].Target != "p" {
		t.Fatalf("peer turns %+v; want one, targeted at p", turns)
	}
	var forced bool
	for _, line := range laptop.logLines("remote pool decision") {
		if line["session_state"] == "forced" && line["target_account"] == "p" {
			forced = true
		}
	}
	if !forced {
		t.Fatalf("no forced decision line targeting p: %v", laptop.logLines("remote pool decision"))
	}
}

// --- a model the account cannot serve ----------------------------------------

func codexVerdictScore(id string, weekly float64, roll time.Duration) selectacct.Score {
	return selectacct.Score{
		AccountID: id, Provider: accounts.ProviderCodex, Fresh: true,
		Headroom: weekly, ShortHeadroom: weekly,
		WeeklyHeadroom: weekly, WeeklyRemaining: weekly, WeeklyResetAt: time.Now().Add(roll),
	}
}

// codexCrossPoolLaptop is a Codex deciding daemon: c1 (best) and c2 (thin)
// in front of a fake upstream that answer may override per account.
func codexCrossPoolLaptop(t *testing.T, peer *RemotePool, answer func(account string, w http.ResponseWriter, r *http.Request) bool) (Server, *session.Store, *syncBuffer) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-")
		if answer != nil && answer(account, w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp","served_by":"`+account+`"}`)
	}))
	t.Cleanup(upstream.Close)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	active := NewActiveSessions()
	active.startedAt = time.Now().Add(-3 * time.Hour)
	logs := &syncBuffer{}
	return Server{
		Upstream:      mustURL(t, upstream.URL),
		CodexUpstream: mustURL(t, upstream.URL),
		Accounts: []accounts.Account{
			{ID: "c1", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "tok-c1"},
			{ID: "c2", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "tok-c2"},
		},
		Sessions:       store,
		ActiveSessions: active,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			codexVerdictScore("c1", 0.9, 30*time.Hour),
			codexVerdictScore("c2", 0.2, 100*time.Hour),
		})),
		RefreshAccountFn: func(_ context.Context, account accounts.Account) (accounts.Account, error) {
			return account, nil
		},
		RemotePool:   peer,
		MaxBodyBytes: 1 << 20,
		Logger:       slog.New(slog.NewJSONHandler(logs, nil)),
	}, store, logs
}

func postCodexResponses(t *testing.T, handler http.Handler, sessionID string) (int, string) {
	t.Helper()
	front := newProxyTestServer(t, handler)
	defer front.Close()
	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Session", sessionID)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, servedByOf(body)
}

// c1 answers "this model is not supported with a ChatGPT account": a forced
// switch for the model. The compatibility reroute names c2 locally; the peer's
// account outranks c2 (though not c1), so the turn is served there, targeted.
func TestAModelTheAccountCannotServeIsRankedAcrossEveryPool(t *testing.T) {
	peer := newVerdictPeer(t, accounts.ProviderCodex,
		verdictPeerAccount{Score: codexVerdictScore("px", 0.9, 50*time.Hour)})
	unsupported := false
	var mu sync.Mutex
	server, _, _ := codexCrossPoolLaptop(t, peer.pool(), func(account string, w http.ResponseWriter, _ *http.Request) bool {
		mu.Lock()
		defer mu.Unlock()
		if account != "c1" || !unsupported {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."}}`)
		return true
	})
	if status, served := postCodexResponses(t, server.Handler(), "s1"); status != http.StatusOK || served != "c1" {
		t.Fatalf("calibration: turn 1 status %d served by %q; want c1", status, served)
	}
	mu.Lock()
	unsupported = true
	mu.Unlock()
	status, served := postCodexResponses(t, server.Handler(), "s1")
	if status != http.StatusOK || served != "px" {
		t.Fatalf("turn 2 status %d served by %q; a model c1 cannot serve is a forced switch and px outranks c2", status, served)
	}
	if turns := peer.seen(); len(turns) != 1 || turns[0].Target != "px" {
		t.Fatalf("peer turns %+v; want one, targeted at px", turns)
	}
}

// --- the Codex websocket quota failover --------------------------------------

// Mid-socket, a usage_limit_reached on c1 closes the connection (1013) after
// committing c2; the reconnect is the forced switch, ranked across every pool,
// so it dials the peer's account (which outranks c2) instead of riding c2's
// fresh pin as a warm session.
func TestACodexWebSocketQuotaFailoverReconnectIsRankedAcrossEveryPool(t *testing.T) {
	peer := newVerdictPeer(t, accounts.ProviderCodex,
		verdictPeerAccount{Score: codexVerdictScore("px", 0.9, 50*time.Hour)})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var mu sync.Mutex
	var dials []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-")
		mu.Lock()
		dials = append(dials, account)
		mu.Unlock()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if account == "c1" {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.failed","response":{"error":{"code":"usage_limit_reached"}}}`))
		} else {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","served_by":"`+account+`"}`))
		}
		_, _, _ = conn.ReadMessage()
	}))
	defer upstream.Close()
	server, store, wsLogs := codexCrossPoolLaptop(t, peer.pool(), nil)
	server.CodexUpstream, server.Upstream = mustURL(t, upstream.URL), mustURL(t, upstream.URL)
	front := newProxyTestServer(t, server.Handler())
	defer front.Close()
	wsURL := "ws" + strings.TrimPrefix(front.URL, "http") + "/backend-api/codex/responses"
	header := http.Header{"Session-Id": []string{"ws-forced"}}

	conn, response, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		status, detail := 0, ""
		if response != nil {
			status = response.StatusCode
			raw, _ := io.ReadAll(response.Body)
			detail = string(raw)
		}
		t.Fatalf("dial: %v (status %d: %s) peer %+v logs:\n%s", err, status, detail, peer.seen(), wsLogs.String())
	}
	response.Body.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, body, err := conn.ReadMessage(); err == nil {
		t.Fatalf("client received %q; want the quota event absorbed behind a reconnect close", body)
	}
	_ = conn.Close()
	mu.Lock()
	first := append([]string(nil), dials...)
	mu.Unlock()
	if len(first) != 1 || first[0] != "c1" {
		t.Fatalf("calibration: first dial went to %v; want c1", first)
	}
	if pinned, _ := store.Get("codex", "ws-forced"); pinned.AccountID != "c2" {
		t.Fatalf("calibration: pin after the quota failover = %q; want the local alternate c2", pinned.AccountID)
	}

	retry, retryResponse, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	retryResponse.Body.Close()
	defer retry.Close()
	if err := retry.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatal(err)
	}
	_ = retry.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, body, err := retry.ReadMessage()
	if err != nil {
		t.Fatalf("read after reconnect: %v", err)
	}
	if served := servedByOf(body); served != "px" {
		t.Fatalf("reconnect served by %q (%s); a spent window is a forced switch, and px outranks c2", served, body)
	}
	if turns := peer.seen(); len(turns) != 1 || !turns[0].WebSocket || turns[0].Target != "px" {
		t.Fatalf("peer turns %+v; want the reconnect, a websocket targeted at px", turns)
	}
}

// --- Fable -> Opus ------------------------------------------------------------

// opusCrossPoolLaptop is a Claude deciding daemon with one and two, whose
// Fable pool is at fableHeadroom and Opus pool at 0.3 with no weekly reading;
// its upstream rejects every Fable turn by the Fable window alone when
// rejectFable, and serves the rest naming the account.
func opusCrossPoolLaptop(t *testing.T, peer *RemotePool, fableHeadroom float64, rejectFable bool) (Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	served := &[]string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &payload)
		account := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-")
		mu.Lock()
		*served = append(*served, account+":"+payload.Model)
		mu.Unlock()
		if rejectFable && strings.Contains(payload.Model, "fable") {
			fablePoolRejected(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg","served_by":"`+account+`"}`)
	}))
	t.Cleanup(upstream.Close)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	active := NewActiveSessions()
	active.startedAt = time.Now().Add(-3 * time.Hour)
	return Server{
		ClaudeUpstream: mustURL(t, upstream.URL),
		Accounts: []accounts.Account{
			{ID: "one", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-one"},
			{ID: "two", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-two"},
		},
		Sessions:       store,
		ActiveSessions: active,
		SchedulerRef:   selectacct.NewSchedulerRef(selectacct.NewScheduler(opusPoolScores(fableHeadroom, 0.3, "one", "two"))),
		RefreshAccountFn: func(_ context.Context, account accounts.Account) (accounts.Account, error) {
			return account, nil
		},
		RemotePool:   peer,
		MaxBodyBytes: 1 << 20,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, served
}

// opusPeer is a peer whose account p has a spent FABLE pool and a healthy
// Opus pool: a Fable ranking must not offer it, an Opus ranking must.
func opusPeer(t *testing.T) *verdictPeer {
	t.Helper()
	healthy := crossPoolClaudeScore("p", 0.9, 50*time.Hour)
	spent := healthy
	spent.Headroom, spent.ShortHeadroom, spent.WeeklyHeadroom, spent.WeeklyRemaining = 0, 0, 0, 0
	peer := newVerdictPeer(t, accounts.ProviderClaude, verdictPeerAccount{
		Score: healthy,
		Pools: map[string]selectacct.Score{
			selectacct.ModelKey(agentclaude.FableFeature): spent,
			selectacct.ModelKey(agentclaude.OpusFeature):  healthy,
		},
	})
	for key, eligible := range map[string]bool{selectacct.ModelKey(agentclaude.FableFeature): false, selectacct.ModelKey(agentclaude.OpusFeature): true} {
		var rows []remotePoolAccountUsage
		if err := json.Unmarshal(peer.usageBody(), &rows); err != nil {
			t.Fatal(err)
		}
		if v := rows[0].Normalized.Pools[key].NewSession; v == nil || v.Eligible != eligible {
			t.Fatalf("calibration: p's %s verdict %+v, want eligible=%v", key, v, eligible)
		}
	}
	return peer
}

func postFableTurnBody(t *testing.T, handler http.Handler, sessionID string) (int, []byte) {
	t.Helper()
	front := newProxyTestServer(t, handler)
	defer front.Close()
	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/messages", bytes.NewReader([]byte(`{"model":"claude-fable-5","max_tokens":8,"messages":[]}`)))
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
	return response.StatusCode, body
}

// Selection finds the local Fable pool spent and re-runs for Opus
// (stage=pool_selection). The Opus selection is the one ranking on the OPUS
// pool: p's healthy Opus pool outranks the local Opus picks, so the rewritten
// Opus body is targeted there. (Ranked on the Fable pool, p is ineligible.)
func TestTheOpusSelectionStageRanksEveryPoolOnTheOpusPool(t *testing.T) {
	peer := opusPeer(t)
	server, served := opusCrossPoolLaptop(t, peer.pool(), 0, false)
	status, body := postFableTurnBody(t, server.Handler(), "s1")
	if status != http.StatusOK || servedByOf(body) != "p" {
		t.Fatalf("status %d served by %q (local upstream saw %v); p's Opus pool outranks the local Opus picks", status, servedByOf(body), *served)
	}
	turns := peer.seen()
	if len(turns) != 1 || turns[0].Target != "p" || !strings.Contains(turns[0].Model, "opus") {
		t.Fatalf("peer turns %+v; want one Opus turn targeted at p", turns)
	}
}

// Selection places the Fable turn locally, and the upstream rejects Fable on
// every local account by the Fable window alone; the failover gives up and the
// transport-level Opus stage picks an Opus account. That pick is a forced
// switch ranked on the Opus pool: p wins, and the rewritten body goes there.
func TestTheOpusTransportStageRanksEveryPoolOnTheOpusPool(t *testing.T) {
	peer := opusPeer(t)
	server, served := opusCrossPoolLaptop(t, peer.pool(), 0.5, true)
	status, body := postFableTurnBody(t, server.Handler(), "s1")
	if status != http.StatusOK || servedByOf(body) != "p" {
		t.Fatalf("status %d served by %q (local upstream saw %v); the Opus stage must rank p's Opus pool", status, servedByOf(body), *served)
	}
	for _, hit := range *served {
		if strings.Contains(hit, "opus") {
			t.Fatalf("local upstream served Opus (%v); p outranks the local Opus pick", *served)
		}
	}
	turns := peer.seen()
	if len(turns) != 1 || turns[0].Target != "p" || !strings.Contains(turns[0].Model, "opus") {
		t.Fatalf("peer turns %+v; want one Opus turn targeted at p (never a Fable one: its Fable pool is spent)", turns)
	}
}

// PATCHES §50. Local selection finds no Fable account and the handler re-runs
// it for Opus, which rewrites the request in place; no local Opus account can
// take it either, so the turn falls through to the peer chain, ranked on the
// peers' FABLE pool. The peer must get the request the client sent, byte for
// byte: it used to get the Opus rewrite, so the first turn of the session ran
// on Opus and the held turns after it -- forwarded before any rewrite -- on
// Fable, a model flip and a cold cache one turn in.
func TestAFailedOpusSubstitutionForwardsTheClientRequestToThePeer(t *testing.T) {
	const clientBody = `{"model":"claude-fable-5","max_tokens":8,"messages":[]}`
	fablePeer := func(t *testing.T) *verdictPeer {
		healthy := crossPoolClaudeScore("p", 0.9, 50*time.Hour)
		return newVerdictPeer(t, accounts.ProviderClaude, verdictPeerAccount{
			Score: healthy,
			Pools: map[string]selectacct.Score{
				selectacct.ModelKey(agentclaude.FableFeature): healthy,
				selectacct.ModelKey(agentclaude.OpusFeature):  healthy,
			},
		})
	}
	for _, tc := range []struct {
		name   string
		local  bool
		scores []selectacct.Score
		peer   func(*testing.T) *verdictPeer
		// target is the peer account the turns go to: p, ranked on its Fable
		// pool, or none -- the untargeted last resort, when no peer account
		// has Fable left either.
		target string
	}{
		// A zero-local-account machine: the first turn of every Fable session.
		{name: "no local accounts", peer: fablePeer, target: "p"},
		// Local Fable spent and no local Opus account able to take it.
		{name: "local fable and opus spent", local: true, scores: opusPoolScores(0, 0, "one", "two"), peer: fablePeer, target: "p"},
		// The peer's Fable is spent too: the untargeted forward lets the peer
		// run its own chain on the request, its own Opus stage included.
		{name: "no local accounts, peer fable spent", peer: opusPeer, target: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := tc.peer(t)
			server, served := opusCrossPoolLaptop(t, peer.pool(), 0, false)
			logs := &bytes.Buffer{}
			server.Logger = slog.New(slog.NewTextHandler(logs, nil))
			server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler(tc.scores))
			if !tc.local {
				server.Accounts = nil
			}
			for turn := 1; turn <= 2; turn++ {
				if status, body := postFableTurnBody(t, server.Handler(), "s1"); status != http.StatusOK || servedByOf(body) != tc.target {
					t.Fatalf("turn %d: status %d served by %q, want the peer (target %q); logs:\n%s", turn, status, servedByOf(body), tc.target, logs)
				}
			}
			if !strings.Contains(logs.String(), "claude opus substitution unavailable") {
				t.Fatalf("calibration: the handler's Opus stage never ran and failed; logs:\n%s", logs)
			}
			if len(*served) != 0 {
				t.Fatalf("local upstream served %v; no local account can take Fable or Opus", *served)
			}
			turns := peer.seen()
			if len(turns) != 2 {
				t.Fatalf("peer turns %+v; want both turns", turns)
			}
			for i, got := range turns {
				if got.Body != clientBody || got.ContentLength != int64(len(clientBody)) || got.Target != tc.target {
					t.Fatalf("peer turn %d = model %q target %q body %q (Content-Length %d); want the client's request %q (%d), target %q",
						i+1, got.Model, got.Target, got.Body, got.ContentLength, clientBody, len(clientBody), tc.target)
				}
			}
		})
	}
}

// The answering half of the model_unsupported switch: the decider now ranks
// peer accounts on it but cannot see a peer account's model support, so a
// targeted Codex turn whose account does not serve the model must come back
// as a refusal it recomputes on (its local replacement still in the ranking),
// never as the 400 handed to the client. The body is restored for anything
// that still reads it.
func TestATargetedCodexModelUnsupportedAnswerIsARefusal(t *testing.T) {
	const unsupported = `{"error":{"type":"invalid_request_error","message":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."}}`
	response := &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(unsupported))}
	Server{}.stampTargetedResponse(response, accounts.Account{ID: "px", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth}, "")
	if got := response.Header.Get(remotePoolTargetRefusedHeader); got != targetRefusedModelUnsupported {
		t.Fatalf("refused header = %q, want %q", got, targetRefusedModelUnsupported)
	}
	if sw := classifyAccountSwitch(accountSwitchEvidence{PeerStatus: response.StatusCode, PeerHeader: response.Header}); sw.Kind != switchForced {
		t.Fatalf("the decider reads it as %s/%q, want a forced refusal", sw.Kind, sw.Reason)
	}
	if body, _ := io.ReadAll(response.Body); string(body) != unsupported {
		t.Fatalf("body after the stamp = %q; the inspection must restore it", body)
	}
}
