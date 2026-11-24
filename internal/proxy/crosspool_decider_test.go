package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// The deciding half of PATCHES §31 (owner rules P1-P9), driven end to end
// through Server.Handler() against a peer that speaks the contract.
//
// These tests use only what origin/main already had -- Server.RemotePool, the
// handler, the hold table -- so each one runs unchanged on the pre-§31 tree and
// is watched failing there (or is marked a characterization test of behaviour
// that must not change). The peer computes its published verdicts with the
// REAL selectacct.Scheduler.NewSessionVerdict over its own scores and its own
// live session counts, never with hand-written booleans: the laptop must agree
// with the box because it reads the box's answer, not because a fixture
// restated the rules.

// --- the peer: a box that publishes verdicts and honours targets ----------

type boxAccount struct {
	Score selectacct.Score
	// Live is the box's own live-session count for the account: exactly what
	// the laptop cannot see, and what 2026-09-27 turned on.
	Live int
}

type boxTurn struct {
	Target  string
	Session string
	Body    string
	Status  int
	Served  string
}

type contractBox struct {
	mu       sync.Mutex
	accounts []boxAccount
	// legacy: an old box -- no new_session in its usage body, and a target
	// header is ignored (it serves untargeted, on substitute).
	legacy bool
	// ignoreTarget: publishes verdicts but serves a targeted turn on
	// substitute anyway (a peer that does not honour targets).
	ignoreTarget bool
	substitute   string
	// refuse: accounts this box refuses on a targeted turn whatever its
	// published verdict said (it changed between the publish and the request).
	refuse map[string]string
	// declineUntargeted answers an untargeted turn with a 503, as a box
	// whose own pool is spent does.
	declineUntargeted bool
	// turnStatus answers every turn with this status (transient adds
	// X-Subrouter-Transient).
	turnStatus    int
	turnTransient bool
	// usageDelay stalls the usage endpoint (a slow or blackholed peer).
	usageDelay time.Duration
	// pins: the box's own session -> account pins, for "continuing".
	pins  map[string]string
	turns []boxTurn
	pool  *RemotePool
	srv   *httptest.Server
	// provider is the box's accounts' provider; empty is Claude.
	provider accounts.Provider
	// withholdUnstarted: a box from before PATCHES §86, which publishes no
	// weekly_unstarted.
	withholdUnstarted bool
}

func (b *contractBox) accountProvider() accounts.Provider {
	if b.provider == "" {
		return accounts.ProviderClaude
	}
	return b.provider
}

func newContractBox(t *testing.T, accounts ...boxAccount) *contractBox {
	t.Helper()
	box := &contractBox{accounts: accounts, refuse: map[string]string{}, pins: map[string]string{}}
	box.srv = httptest.NewServer(http.HandlerFunc(box.serve))
	t.Cleanup(box.srv.Close)
	box.pool = NewRemotePool(box.srv.URL, "", "")
	return box
}

// scheduler is the box's own selection view: its scores and its live counts.
func (b *contractBox) scheduler() selectacct.Scheduler {
	scores := make([]selectacct.Score, 0, len(b.accounts))
	live := map[string]int{}
	for _, a := range b.accounts {
		score := a.Score
		score.Provider = b.accountProvider()
		scores = append(scores, score)
		live[selectacct.ScoreKey(score.Provider, score.AccountID)] = a.Live
	}
	return selectacct.NewScheduler(scores).WithLiveSessionCounts(live)
}

func (b *contractBox) known(id string) bool {
	for _, a := range b.accounts {
		if a.Score.AccountID == id {
			return true
		}
	}
	return false
}

func (b *contractBox) usageBody() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	scheduler := b.scheduler()
	rows := make([]map[string]any, 0, len(b.accounts))
	for _, a := range b.accounts {
		s := a.Score
		normalized := map[string]any{
			"headroom": s.Headroom, "short_headroom": s.ShortHeadroom, "fresh": true,
			"weekly_headroom": s.WeeklyHeadroom, "seconds_to_weekly_reset": s.WeeklyResetAt.Sub(now).Seconds(),
		}
		if s.ShortWindowSeconds > 0 {
			normalized["short_window_seconds"] = s.ShortWindowSeconds
			normalized["seconds_to_short_reset"] = s.ShortResetAfterSeconds
		}
		if !b.legacy {
			normalized["new_session"] = scheduler.NewSessionVerdict(b.accountProvider(), s.AccountID, now)
		}
		if s.WeeklyUnstarted && !b.withholdUnstarted {
			normalized["weekly_unstarted"] = true
		}
		// As the owner publishes them: the Fable meter at the top level when
		// it is used (PATCHES §65), a choice inside normalized (§95.2).
		if s.FableSpent > 0 {
			normalized["fable_week_spent"] = s.FableSpent
		}
		if s.ResetChoice.Known() {
			normalized["reset_choice"] = s.ResetChoice
		}
		rows = append(rows, map[string]any{
			"id": s.AccountID, "provider": string(b.accountProvider()), "auth_mode": "oauth",
			"auth_checked": true, "auth_valid": true, "normalized": normalized,
		})
	}
	body, _ := json.Marshal(rows)
	return body
}

func (b *contractBox) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == remotePoolUsagePath {
		b.mu.Lock()
		delay := b.usageDelay
		b.mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		_, _ = w.Write(b.usageBody())
		return
	}
	body, _ := io.ReadAll(r.Body)
	target := r.Header.Get("X-Subrouter-Target-Account")
	sessionID := r.Header.Get("X-Subrouter-Session")
	b.mu.Lock()
	defer b.mu.Unlock()
	turn := boxTurn{Target: target, Session: sessionID, Body: string(body)}
	record := func(status int, served string) {
		turn.Status, turn.Served = status, served
		b.turns = append(b.turns, turn)
	}
	if b.turnStatus != 0 {
		if b.turnTransient {
			w.Header().Set(remotePoolTransientHeader, "1")
		}
		record(b.turnStatus, "")
		http.Error(w, "box says no", b.turnStatus)
		return
	}
	account := b.substitute
	if target != "" && !b.legacy && !b.ignoreTarget {
		// The contract (§3 B2): a targeted turn is served on exactly that
		// account or refused before upstream; a continuing session only for
		// eviction, a new placement on the full verdict.
		if !b.known(target) {
			record(http.StatusConflict, "")
			writeTargetRefusal(w, target, "unknown_account", false, nil)
			return
		}
		verdict := b.scheduler().NewSessionVerdict(b.accountProvider(), target, time.Now())
		continuing := b.pins[sessionID] == target
		if reason, refused := b.refuse[target]; refused {
			record(http.StatusConflict, "")
			writeTargetRefusal(w, target, reason, continuing, &verdict)
			return
		}
		if (continuing && verdict.Evicted) || (!continuing && !verdict.Eligible) {
			record(http.StatusConflict, "")
			writeTargetRefusal(w, target, verdict.Reason, continuing, &verdict)
			return
		}
		account = target
	}
	if target == "" && b.declineUntargeted {
		record(http.StatusServiceUnavailable, "")
		http.Error(w, "no non-exhausted claude accounts available", http.StatusServiceUnavailable)
		return
	}
	if account == "" && len(b.accounts) > 0 {
		account = b.accounts[0].Score.AccountID
	}
	b.pins[sessionID] = account
	record(http.StatusOK, account)
	w.Header().Set(subrouterAccountHeader, account)
	_, _ = io.WriteString(w, `{"id":"msg_box","served_by":"`+account+`"}`)
}

func (b *contractBox) seen() []boxTurn {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]boxTurn(nil), b.turns...)
}

// --- the deciding daemon: local accounts, a local upstream, the box -------

type localUpstream struct {
	mu     sync.Mutex
	served []string
	// answer, when set, may answer a request for an account itself.
	answer func(account string, w http.ResponseWriter) bool
}

func (u *localUpstream) accounts() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.served...)
}

type crossPoolLaptop struct {
	server   Server
	store    *session.Store
	active   *ActiveSessions
	upstream *localUpstream
	logs     *syncBuffer
}

func newCrossPoolLaptop(t *testing.T, local []selectacct.Score, peer *RemotePool) *crossPoolLaptop {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	up := &localUpstream{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-")
		up.mu.Lock()
		up.served = append(up.served, account)
		answer := up.answer
		up.mu.Unlock()
		if answer != nil && answer(account, w) {
			return
		}
		_, _ = io.WriteString(w, `{"id":"msg_local","served_by":"`+account+`"}`)
	}))
	t.Cleanup(upstream.Close)
	var list []accounts.Account
	scores := make([]selectacct.Score, 0, len(local))
	for _, score := range local {
		score.Provider, score.Fresh = accounts.ProviderClaude, true
		scores = append(scores, score)
		list = append(list, accounts.Account{ID: score.AccountID, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-" + score.AccountID})
	}
	active := NewActiveSessions()
	// Up for longer than the idle bound, so a session this process never saw
	// is COLD, not possibly-warm; tests that want a warm one mark it.
	active.startedAt = time.Now().Add(-3 * time.Hour)
	logs := &syncBuffer{}
	return &crossPoolLaptop{
		server: Server{
			ClaudeUpstream: mustURL(t, upstream.URL),
			Accounts:       list,
			Sessions:       store,
			ActiveSessions: active,
			SchedulerRef:   selectacct.NewSchedulerRef(selectacct.NewScheduler(scores)),
			RefreshAccountFn: func(_ context.Context, account accounts.Account) (accounts.Account, error) {
				return account, nil
			},
			RemotePool:   peer,
			MaxBodyBytes: 1 << 20,
			// Debug: see e2eMeasuredServer.
			Logger: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		},
		store:    store,
		active:   active,
		upstream: up,
		logs:     logs,
	}
}

// turn posts one Claude turn and returns the status and who served it.
func (l *crossPoolLaptop) turn(t *testing.T, sessionID string) (int, string) {
	t.Helper()
	response := postClaudeTurn(t, l.server.Handler(), sessionID)
	body, _ := io.ReadAll(response.Body)
	var payload struct {
		ServedBy string `json:"served_by"`
	}
	_ = json.Unmarshal(body, &payload)
	return response.StatusCode, payload.ServedBy
}

func (l *crossPoolLaptop) logLines(message string) []map[string]any {
	var out []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(l.logs.String()))
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var line map[string]any
		if json.Unmarshal(scanner.Bytes(), &line) == nil && line["msg"] == message {
			out = append(out, line)
		}
	}
	return out
}

func crossPoolClaudeScore(id string, weekly float64, roll time.Duration) selectacct.Score {
	return selectacct.Score{
		AccountID: id, Provider: accounts.ProviderClaude, Fresh: true,
		Headroom: weekly, ShortHeadroom: 0.95,
		WeeklyHeadroom: weekly, WeeklyRemaining: weekly, WeeklyResetAt: time.Now().Add(roll),
	}
}

const heldSession = "held-session"

func holdOn(t *testing.T, pool *RemotePool, sessionID, account string) {
	t.Helper()
	pool.delegations.set(session.ScopedSessionKey("claude", sessionID), time.Now(), remotePoolDelegationTTLFor("claude"), account, pool.name())
}

func holdAccount(pool *RemotePool, sessionID string) (string, bool) {
	hold, ok := pool.delegations.lookup(session.ScopedSessionKey("claude", sessionID), time.Now())
	return hold.Account, ok
}

func targetsOf(turns []boxTurn) []string {
	out := make([]string, len(turns))
	for i, turn := range turns {
		out[i] = turn.Target
	}
	return out
}

// --- the box of 2026-09-27 ----------------------------------------------------

// incidentBox is the box as the laptop should have seen it on 2026-09-27: its
// best-looking account chip-5 is in burndown (~4% of the week left, 16h to the
// roll) and already carries the ONE session its taper allows; every other box
// account is spent, below the 5h floor, or burning its 5h window too fast.
func incidentBox(t *testing.T, chip5Live int) *contractBox {
	t.Helper()
	now := time.Now()
	chip5 := crossPoolClaudeScore("chip-5", 0.04, 16*time.Hour)
	chip5.ShortHeadroom = 0.9
	spent := crossPoolClaudeScore("chip-1", 0.5, 100*time.Hour)
	spent.Headroom, spent.ShortHeadroom = 0, 0
	floor := crossPoolClaudeScore("chip-2", 0.6, 90*time.Hour)
	floor.Headroom, floor.ShortHeadroom = 0.02, 0.02
	burning := crossPoolClaudeScore("chip-3", 0.76, 120*time.Hour)
	burning.Headroom, burning.ShortHeadroom = 0.2, 0.2
	burning.ShortWindowSeconds, burning.ShortResetAfterSeconds = 5*60*60, 4*60*60
	box := newContractBox(t,
		boxAccount{Score: chip5, Live: chip5Live},
		boxAccount{Score: spent}, boxAccount{Score: floor}, boxAccount{Score: burning})
	box.substitute = "chip-2" // where the incident's sessions actually landed
	// Calibration: the box's OWN verdicts, from the shared function.
	s := box.scheduler()
	for id, want := range map[string]string{"chip-1": "exhausted", "chip-2": "short_floor", "chip-3": "burn_projection"} {
		if got := s.NewSessionVerdict(accounts.ProviderClaude, id, now); got.Eligible || got.Reason != want {
			t.Fatalf("calibration: box %s verdict = %+v, want ineligible %s", id, got, want)
		}
	}
	chip5Verdict := s.NewSessionVerdict(accounts.ProviderClaude, "chip-5", now)
	if chip5Live >= 1 && (chip5Verdict.Eligible || !chip5Verdict.AtCap) {
		t.Fatalf("calibration: chip-5 with %d live must be at its taper cap: %+v", chip5Live, chip5Verdict)
	}
	if chip5Live == 0 && (!chip5Verdict.Eligible || !chip5Verdict.Burndown) {
		t.Fatalf("calibration: idle chip-5 must be burndown-eligible: %+v", chip5Verdict)
	}
	return box
}

// incidentLocal is the laptop's roomy local pool that night.
func incidentLocal() []selectacct.Score {
	return []selectacct.Score{
		crossPoolClaudeScore("fourteen", 0.88, 152*time.Hour),
		crossPoolClaudeScore("fifteen", 0.98, 159*time.Hour),
	}
}

// P1/P3: the laptop sent ~23 sessions to the box because chip-5 re-derived as
// burndown-eligible from its published numbers, with none of the box's live
// sessions in them; the box then spread them onto accounts whose 5h windows
// were spent. Read from the box's own verdicts, nothing on the box is eligible
// and every new session stays on the roomy local pool.
func TestCrossPoolIncidentReplay20260927(t *testing.T) {
	box := incidentBox(t, 1)
	laptop := newCrossPoolLaptop(t, incidentLocal(), box.pool)
	for _, sessionID := range []string{"s1", "s2", "s3", "s4", "s5"} {
		status, servedBy := laptop.turn(t, sessionID)
		if status != http.StatusOK {
			t.Fatalf("%s: status %d", sessionID, status)
		}
		if servedBy != "fourteen" && servedBy != "fifteen" {
			t.Fatalf("%s served by %q; want a local account (the box has nothing eligible)", sessionID, servedBy)
		}
	}
	if turns := box.seen(); len(turns) != 0 {
		t.Fatalf("forwarded %d turns to the box (%+v); every box account is ineligible by its own verdict", len(turns), turns)
	}
}

// P2: burndown first, wherever it lives. The same box with chip-5 idle: its
// expiring budget outranks the steady local pool, and the laptop names it.
func TestCrossPoolBurndownPeerOutranksSteadyLocal(t *testing.T) {
	box := incidentBox(t, 0)
	laptop := newCrossPoolLaptop(t, incidentLocal(), box.pool)
	status, servedBy := laptop.turn(t, "s1")
	if status != http.StatusOK || servedBy != "chip-5" {
		t.Fatalf("status %d served by %q; want chip-5, the box's burndown account", status, servedBy)
	}
	turns := box.seen()
	if len(turns) != 1 || turns[0].Target != "chip-5" {
		t.Fatalf("box turns %+v; want exactly one, TARGETED at chip-5", turns)
	}
}

// P2's arithmetic: remaining% x 7 / days, local doubled. 100%/5d local is 140,
// or 280 with the 2x; a peer at 100%/2d is 350 and wins, one at 100%/3d is 233
// and loses.
func TestCrossPoolLocalPreferenceIsTwoXOnPace(t *testing.T) {
	for _, tc := range []struct {
		name     string
		peerRoll time.Duration
		wantPeer bool
	}{
		{"peer 100%/2d = 350 beats local 140x2 = 280", 48 * time.Hour, true},
		{"peer 100%/3d = 233 loses to local 280", 72 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := crossPoolClaudeScore("p1", 1, tc.peerRoll)
			peer.ShortHeadroom = 1
			box := newContractBox(t, boxAccount{Score: peer})
			local := crossPoolClaudeScore("loc", 1, 120*time.Hour)
			local.ShortHeadroom = 1
			laptop := newCrossPoolLaptop(t, []selectacct.Score{local}, box.pool)
			_, servedBy := laptop.turn(t, "s1")
			turns := box.seen()
			if tc.wantPeer {
				if servedBy != "p1" || len(turns) != 1 || turns[0].Target != "p1" {
					t.Fatalf("served by %q, box turns %+v; want p1, targeted", servedBy, turns)
				}
				return
			}
			if servedBy != "loc" || len(turns) != 0 {
				t.Fatalf("served by %q, box turns %+v; want local", servedBy, turns)
			}
		})
	}
}

// P3: only ELIGIBLE box accounts compete, by the box's own verdict. The box's
// best-looking account is at its taper cap on sessions only the box can count;
// the laptop targets the next one instead of the one it would re-derive.
func TestCrossPoolOnlyEligiblePeerAccountsCompete(t *testing.T) {
	capped := crossPoolClaudeScore("best", 0.2, 20*time.Hour)
	capped.ShortHeadroom = 1
	floor := crossPoolClaudeScore("floored", 1, 30*time.Hour)
	floor.Headroom, floor.ShortHeadroom = 0.03, 0.03
	second := crossPoolClaudeScore("second", 1, 30*time.Hour)
	second.ShortHeadroom = 1
	// Fill it to ITS OWN allowance: the taper step sizes that allowance
	// (PATCHES §41), so a literal count silently stops meaning "at cap" the
	// moment the step is retuned -- as 1 did here.
	allowed := selectacct.NewScheduler([]selectacct.Score{capped}).
		NewSessionVerdict(accounts.ProviderClaude, "best", time.Now()).AllowedSessions
	box := newContractBox(t, boxAccount{Score: capped, Live: allowed}, boxAccount{Score: floor}, boxAccount{Score: second})
	laptop := newCrossPoolLaptop(t, []selectacct.Score{crossPoolClaudeScore("loc", 0.3, 150*time.Hour)}, box.pool)
	_, servedBy := laptop.turn(t, "s1")
	turns := box.seen()
	if servedBy != "second" || len(turns) != 1 || turns[0].Target != "second" {
		t.Fatalf("served by %q, box turns %+v; want the one eligible box account, targeted", servedBy, turns)
	}
}

// Owner follow-up F2: LOCAL candidates are ranked on exactly the score local
// selection ranks them on -- live debits for requests already routed, and the
// owner's per-account weekly reserve -- not on the raw measured headroom. Before
// the one ranking, the local half of the comparison was built from the
// undebited base score with no Reserve, so an account the local scheduler
// would not start a session on (or ranks below the peer) still beat the peer
// on its raw numbers. In every case here the raw numbers alone make local win
// (calibrated), and the scheduler's own view makes the peer account win.
func TestCrossPoolRanksLocalOnItsDebitedReserveAwareScore(t *testing.T) {
	for _, tc := range []struct {
		name  string
		local func() selectacct.Score
		// reserve is the owner's weekly holdout for loc; debits are requests
		// already routed to loc since the last usage refresh.
		reserve float64
		debits  int
		// usable is what the local scheduler's own verdict must say about loc,
		// so each case exercises the clause it names.
		usable bool
	}{
		{
			// Weekly headroom (after the Fable reservation) inside the reserve:
			// the soft new-session gate, not the holdout (WeeklyRemaining is
			// well above it), so nothing evicts and selection still returns loc.
			name: "reserve makes local ineligible for a new session",
			local: func() selectacct.Score {
				s := crossPoolClaudeScore("loc", 0.9, 100*time.Hour)
				s.WeeklyHeadroom = 0.4
				return s
			},
			reserve: 0.5,
		},
		{
			// Six routed requests take the 5h window from 0.15 to 0.03, under
			// MinShortHeadroomFloor.
			name: "live debits put local under the short floor",
			local: func() selectacct.Score {
				s := crossPoolClaudeScore("loc", 0.9, 100*time.Hour)
				s.ShortHeadroom, s.ShortWindowSeconds, s.ShortResetAfterSeconds = 0.15, 5*60*60, 30*60
				return s
			},
			debits: 6,
		},
		{
			// Still eligible, but the debits take weekly pace from 0.3 to 0.18
			// per 100h: 0.36 with the 2x, under the peer's 0.5.
			name: "live debits rank local below the peer",
			local: func() selectacct.Score {
				return crossPoolClaudeScore("loc", 0.3, 100*time.Hour)
			},
			debits: 6,
			usable: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := crossPoolClaudeScore("p1", 0.5, 100*time.Hour)
			peer.ShortHeadroom = 1
			box := newContractBox(t, boxAccount{Score: peer})
			local := tc.local()
			laptop := newCrossPoolLaptop(t, []selectacct.Score{local}, box.pool)
			if tc.reserve > 0 {
				laptop.server.AccountReserves = map[string]float64{"loc": tc.reserve}
			}
			for i := 0; i < tc.debits; i++ {
				laptop.server.SchedulerRef.NoteRouted(accounts.ProviderClaude, "loc")
			}
			// Calibration: on the RAW score local wins (what the pre-§31
			// comparison saw), and the scheduler's own view is as the case says.
			raw := local
			raw.Provider = ""
			rawPeer := peer
			rawPeer.AccountID, rawPeer.Provider, rawPeer.Remote = "peer", "", true
			if winner, err := selectacct.NewScheduler([]selectacct.Score{raw, rawPeer}).Pick([]accounts.Account{
				{ID: "loc", AuthMode: accounts.AuthModeOAuth}, {ID: "peer", AuthMode: accounts.AuthModeOAuth},
			}); err != nil || winner.ID != "loc" {
				t.Fatalf("calibration: on raw numbers local must win, got %q (%v)", winner.ID, err)
			}
			if got := laptop.server.selectionScheduler("", "").UsableForNewSession(accounts.ProviderClaude, "loc"); got != tc.usable {
				t.Fatalf("calibration: local scheduler says loc usable=%v, want %v", got, tc.usable)
			}

			status, servedBy := laptop.turn(t, "s1")
			turns := box.seen()
			if status != http.StatusOK || servedBy != "p1" || len(turns) != 1 || turns[0].Target != "p1" {
				t.Fatalf("status %d served by %q, box turns %+v; want p1, targeted: local must not win on its raw headroom", status, servedBy, turns)
			}
		})
	}
}

// Owner follow-up F4 and P9: the decision line and the pool board report what
// the ONE ranking decided, never the "warm" answer a missing score used to
// produce. A cold session whose own account still outranks every eligible peer
// account says cold, names the peer runner-up, and stays; the board's spill,
// asked where a new session would go, names the peer account it would target.
func TestCrossPoolDecisionReportsTheRealRanking(t *testing.T) {
	t.Run("cold session, local wins: the line says cold and names the runner-up", func(t *testing.T) {
		weak := crossPoolClaudeScore("p1", 0.2, 150*time.Hour)
		weak.ShortHeadroom = 1
		box := newContractBox(t, boxAccount{Score: weak})
		laptop := newCrossPoolLaptop(t, []selectacct.Score{crossPoolClaudeScore("fifteen", 0.98, 159*time.Hour)}, box.pool)
		if _, err := laptop.store.Put("claude", "s1", "fifteen", ""); err != nil {
			t.Fatal(err)
		}
		markIdle(laptop.active, "s1", 2*time.Hour)

		if _, servedBy := laptop.turn(t, "s1"); servedBy != "fifteen" {
			t.Fatalf("served by %q; local outranks the weak peer account", servedBy)
		}
		decisions := laptop.logLines("remote pool decision")
		if len(decisions) != 1 {
			t.Fatalf("decision lines %v; want one", decisions)
		}
		d := decisions[0]
		if d["reason"] == "session is warm on its account; staying local" || d["session_state"] != "cold" ||
			d["prefer_peer"] != false || d["peer_known"] != true || d["account"] != "fifteen" {
			t.Fatalf("decision %v; want a cold, ranked decision: local fifteen won, the peer account measured as runner-up", d)
		}
	})
	t.Run("board spill names the peer account the ranking would target", func(t *testing.T) {
		box := incidentBox(t, 0)
		laptop := newCrossPoolLaptop(t, incidentLocal(), box.pool)
		board := laptop.server.poolBoard(t.Context(), "", "")
		// Through JSON, as the board is served, so the field names are the
		// wire's.
		raw, err := json.Marshal(board.Spill)
		if err != nil {
			t.Fatal(err)
		}
		var spills []map[string]any
		if err := json.Unmarshal(raw, &spills); err != nil {
			t.Fatal(err)
		}
		if len(spills) != 1 {
			t.Fatalf("spills %s; want one (claude)", raw)
		}
		s := spills[0]
		if s["prefer_peer"] != true || s["target_account"] != "chip-5" || s["peer_known"] != true ||
			s["reason"] == "session is warm on its account; staying local" {
			t.Fatalf("spill %v; want the box's burndown chip-5 as the target, with the ranking's reason", s)
		}
	})
}

// CHARACTERIZATION (passes on origin/main by design): a peer too old to
// publish verdicts is ranked exactly as before -- its best score against
// local -- and is never sent a target (Deviation X1: an old daemon would pass
// the header straight upstream).
func TestCrossPoolLegacyPeerKeepsTodaysDecisionAndSendsNoTarget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		weekly   float64
		wantPeer bool
	}{
		{"a much better legacy peer still takes the session", 1, true},
		{"a worse legacy peer does not", 0.05, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := crossPoolClaudeScore("old1", tc.weekly, 30*time.Hour)
			peer.ShortHeadroom = 1
			box := newContractBox(t, boxAccount{Score: peer})
			box.legacy = true
			laptop := newCrossPoolLaptop(t, []selectacct.Score{crossPoolClaudeScore("loc", 0.3, 150*time.Hour)}, box.pool)
			_, servedBy := laptop.turn(t, "s1")
			turns := box.seen()
			if !tc.wantPeer {
				if servedBy != "loc" || len(turns) != 0 {
					t.Fatalf("served by %q, box turns %+v; want local", servedBy, turns)
				}
				return
			}
			if servedBy != "old1" || len(turns) != 1 {
				t.Fatalf("served by %q, box turns %+v; want one forward to the legacy peer", servedBy, turns)
			}
			if turns[0].Target != "" {
				t.Fatalf("a legacy peer was sent target %q", turns[0].Target)
			}
		})
	}
}

// D3/P4/D5: a session placed on a box account is held on THAT account, and
// every held turn names it.
func TestTargetedForwardNamesTheAccountAndHoldsIt(t *testing.T) {
	box := incidentBox(t, 0)
	laptop := newCrossPoolLaptop(t, incidentLocal(), box.pool)
	if _, servedBy := laptop.turn(t, "s1"); servedBy != "chip-5" {
		t.Fatalf("first turn served by %q, want chip-5", servedBy)
	}
	if account, ok := holdAccount(box.pool, "s1"); !ok || account != "chip-5" {
		t.Fatalf("hold = %q (%v), want the session held on chip-5", account, ok)
	}
	if _, servedBy := laptop.turn(t, "s1"); servedBy != "chip-5" {
		t.Fatalf("held turn served by %q, want chip-5", servedBy)
	}
	if got := targetsOf(box.seen()); len(got) != 2 || got[1] != "chip-5" {
		t.Fatalf("box targets %v; the held turn must name chip-5 again", got)
	}
}

func TestHeldTurnTargetsTheHoldAccount(t *testing.T) {
	a := crossPoolClaudeScore("acct-a", 0.6, 80*time.Hour)
	b := crossPoolClaudeScore("acct-b", 0.9, 30*time.Hour)
	box := newContractBox(t, boxAccount{Score: a}, boxAccount{Score: b})
	laptop := newCrossPoolLaptop(t, incidentLocal(), box.pool)
	// Held on acct-a, which is NOT the box's best account: the held turn must
	// still name it (P4), not whatever the box or the ranking would pick now.
	holdOn(t, box.pool, heldSession, "acct-a")
	box.pins[heldSession] = "acct-a"
	if _, servedBy := laptop.turn(t, heldSession); servedBy != "acct-a" {
		t.Fatalf("held turn served by %q, want acct-a", servedBy)
	}
	if got := targetsOf(box.seen()); len(got) != 1 || got[0] != "acct-a" {
		t.Fatalf("box targets %v, want [acct-a]", got)
	}
}

// P6: "any time I try to hit an account and get a 'nope' I recompute
// overall." The box refuses the target (it filled up between its last publish
// and this request); the laptop recomputes and targets the next account with
// the same bytes, and the client sees one answer.
func TestTargetRefusalRecomputesToTheNextAccount(t *testing.T) {
	first := crossPoolClaudeScore("acct-a", 1, 30*time.Hour)
	first.ShortHeadroom = 1
	second := crossPoolClaudeScore("acct-b", 0.8, 30*time.Hour)
	second.ShortHeadroom = 1
	box := newContractBox(t, boxAccount{Score: first}, boxAccount{Score: second})
	box.refuse["acct-a"] = "at_cap"
	laptop := newCrossPoolLaptop(t, []selectacct.Score{crossPoolClaudeScore("loc", 0.2, 150*time.Hour)}, box.pool)

	status, servedBy := laptop.turn(t, "s1")
	if status != http.StatusOK || servedBy != "acct-b" {
		t.Fatalf("status %d served by %q; want acct-b after acct-a refused", status, servedBy)
	}
	turns := box.seen()
	if len(turns) != 2 || turns[0].Target != "acct-a" || turns[0].Status != http.StatusConflict || turns[1].Target != "acct-b" {
		t.Fatalf("box turns %+v; want acct-a refused, then acct-b", turns)
	}
	if turns[0].Body != turns[1].Body || !strings.Contains(turns[1].Body, "claude-opus") {
		t.Fatalf("the recompute did not replay the same bytes: %q vs %q", turns[0].Body, turns[1].Body)
	}
	if account, _ := holdAccount(box.pool, "s1"); account != "acct-b" {
		t.Fatalf("held on %q, want acct-b (the account that served)", account)
	}
	// D6: the decision names its target; the refusal names account and reason;
	// the recompute names what it chose.
	decisions := laptop.logLines("remote pool decision")
	refusals := laptop.logLines("remote pool target refused")
	if len(decisions) != 2 || len(refusals) != 1 {
		t.Fatalf("logged %d decisions and %d refusals, want 2 and 1:\n%s", len(decisions), len(refusals), laptop.logs.String())
	}
	if decisions[0]["target_account"] != "acct-a" || decisions[0]["recompute"] != false {
		t.Fatalf("first decision %v; want target_account=acct-a recompute=false", decisions[0])
	}
	if refusals[0]["target_account"] != "acct-a" || refusals[0]["reason"] != "at_cap" {
		t.Fatalf("refusal line %v; want acct-a / at_cap", refusals[0])
	}
	if decisions[1]["target_account"] != "acct-b" || decisions[1]["recompute"] != true ||
		decisions[1]["refused_account"] != "acct-a" || decisions[1]["refused_reason"] != "at_cap" {
		t.Fatalf("recompute decision %v; want acct-b, recompute=true, refused acct-a/at_cap", decisions[1])
	}
}

// P6's bound: at most remotePoolMaxTargetRefusals (2) failed peer attempts per
// turn, then local serves it.
func TestTargetRefusalsAreBoundedThenServedLocally(t *testing.T) {
	var peers []boxAccount
	for _, id := range []string{"acct-a", "acct-b", "acct-c", "acct-d"} {
		score := crossPoolClaudeScore(id, 1, 30*time.Hour)
		score.ShortHeadroom = 1
		peers = append(peers, boxAccount{Score: score})
	}
	box := newContractBox(t, peers...)
	for _, a := range peers {
		box.refuse[a.Score.AccountID] = "at_cap"
	}
	laptop := newCrossPoolLaptop(t, []selectacct.Score{crossPoolClaudeScore("loc", 0.2, 150*time.Hour)}, box.pool)
	status, servedBy := laptop.turn(t, "s1")
	if status != http.StatusOK || servedBy != "loc" {
		t.Fatalf("status %d served by %q; want local after the bound", status, servedBy)
	}
	turns := box.seen()
	if len(turns) != 2 || turns[0].Target == "" || turns[1].Target == "" || turns[0].Target == turns[1].Target {
		t.Fatalf("box turns %+v; want exactly two targeted attempts on two different accounts", turns)
	}
	if _, held := holdAccount(box.pool, "s1"); held {
		t.Fatal("a refused session is still held on the box")
	}
}

// D5: a held session whose account the box's own (recent) verdict says is
// EVICTED leaves it without a round trip, and the next account is chosen
// across both pools -- here, local.
func TestHeldTurnEndsOnACachedEvictedVerdictAndReranks(t *testing.T) {
	spent := crossPoolClaudeScore("acct-a", 0.6, 80*time.Hour)
	spent.Headroom, spent.ShortHeadroom = 0, 0
	box := newContractBox(t, boxAccount{Score: spent})
	laptop := newCrossPoolLaptop(t, incidentLocal(), box.pool)
	holdOn(t, box.pool, heldSession, "acct-a")
	box.pins[heldSession] = "acct-a"
	box.pool.AdminGet(context.Background(), remotePoolUsagePath) // a fresh body in the cache

	status, servedBy := laptop.turn(t, heldSession)
	if status != http.StatusOK || (servedBy != "fourteen" && servedBy != "fifteen") {
		t.Fatalf("status %d served by %q; want the session re-ranked onto local", status, servedBy)
	}
	if turns := box.seen(); len(turns) != 0 {
		t.Fatalf("box turns %+v; an evicted held account must not be tried", turns)
	}
	if _, held := holdAccount(box.pool, heldSession); held {
		t.Fatal("the hold on an evicted account survived")
	}
}

// §20's warm rule on the peer: a held session whose account is merely below
// the NEW-session floor keeps it. Only eviction moves a warm session.
func TestHeldWarmTurnBelowFloorStaysOnItsAccount(t *testing.T) {
	low := crossPoolClaudeScore("acct-a", 0.6, 80*time.Hour)
	low.Headroom, low.ShortHeadroom = 0.03, 0.03
	box := newContractBox(t, boxAccount{Score: low})
	laptop := newCrossPoolLaptop(t, incidentLocal(), box.pool)
	holdOn(t, box.pool, heldSession, "acct-a")
	box.pins[heldSession] = "acct-a"
	box.pool.AdminGet(context.Background(), remotePoolUsagePath)
	if v := box.scheduler().NewSessionVerdict(accounts.ProviderClaude, "acct-a", time.Now()); v.Eligible || v.Evicted {
		t.Fatalf("calibration: acct-a must be below the floor without being evicted: %+v", v)
	}

	if _, servedBy := laptop.turn(t, heldSession); servedBy != "acct-a" {
		t.Fatalf("held turn served by %q, want acct-a", servedBy)
	}
	if got := targetsOf(box.seen()); len(got) != 1 || got[0] != "acct-a" {
		t.Fatalf("box targets %v, want [acct-a]", got)
	}
	if account, _ := holdAccount(box.pool, heldSession); account != "acct-a" {
		t.Fatalf("hold moved to %q", account)
	}
}

// P8: a refusal of the held account is a FORCED switch, so the session's local
// pin -- a placeholder, however recently written -- is not treated as warm:
// the next account is ranked across both pools.
func TestHeldTurnRefusedRecomputesAcrossBothPools(t *testing.T) {
	a := crossPoolClaudeScore("acct-a", 0.9, 30*time.Hour)
	b := crossPoolClaudeScore("acct-b", 1, 30*time.Hour)
	b.ShortHeadroom = 1
	box := newContractBox(t, boxAccount{Score: a}, boxAccount{Score: b})
	box.refuse["acct-a"] = "exhausted"
	laptop := newCrossPoolLaptop(t, []selectacct.Score{crossPoolClaudeScore("loc", 0.2, 150*time.Hour)}, box.pool)
	holdOn(t, box.pool, heldSession, "acct-a")
	if _, err := laptop.store.Put("claude", heldSession, "loc", ""); err != nil {
		t.Fatal(err)
	}
	markIdle(laptop.active, heldSession, 2*time.Minute) // the placeholder pin reads warm

	status, servedBy := laptop.turn(t, heldSession)
	if status != http.StatusOK || servedBy != "acct-b" {
		t.Fatalf("status %d served by %q; want acct-b, the best account left across both pools", status, servedBy)
	}
	if got := targetsOf(box.seen()); len(got) != 2 || got[0] != "acct-a" || got[1] != "acct-b" {
		t.Fatalf("box targets %v, want [acct-a acct-b]", got)
	}
	if account, _ := holdAccount(box.pool, heldSession); account != "acct-b" {
		t.Fatalf("hold = %q, want acct-b", account)
	}
	// D6: the refusal is logged, and the decision after it is a recompute
	// that names what failed and what it chose.
	decisions := laptop.logLines("remote pool decision")
	if len(laptop.logLines("remote pool target refused")) != 1 || len(decisions) != 1 ||
		decisions[0]["recompute"] != true || decisions[0]["refused_account"] != "acct-a" ||
		decisions[0]["target_account"] != "acct-b" || decisions[0]["session_state"] != "forced" {
		t.Fatalf("decision lines %v; want one forced recompute from acct-a to acct-b\n%s", decisions, laptop.logs.String())
	}
}

// P8: transient trouble on the held account -- a refresh blip, a 429 the box
// stamped transient -- keeps the hold, serves this one turn locally, and asks
// no one else. The next turn is back on the account.
func TestTransientPeerBlipKeepsTheHoldAndServesOneTurnLocally(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			a := crossPoolClaudeScore("acct-a", 1, 30*time.Hour)
			a.ShortHeadroom = 1
			box := newContractBox(t, boxAccount{Score: a})
			box.turnStatus, box.turnTransient = status, true
			laptop := newCrossPoolLaptop(t, []selectacct.Score{crossPoolClaudeScore("loc", 0.2, 150*time.Hour)}, box.pool)
			holdOn(t, box.pool, heldSession, "acct-a")
			box.pins[heldSession] = "acct-a"

			if _, servedBy := laptop.turn(t, heldSession); servedBy != "loc" {
				t.Fatalf("blip turn served by %q, want local", servedBy)
			}
			if turns := box.seen(); len(turns) != 1 {
				t.Fatalf("box turns %+v; a transient blip must cost exactly the one held attempt", turns)
			}
			if account, held := holdAccount(box.pool, heldSession); !held || account != "acct-a" {
				t.Fatalf("hold = %q (%v); a transient blip must keep it on acct-a", account, held)
			}
			box.mu.Lock()
			box.turnStatus = 0
			box.mu.Unlock()
			if _, servedBy := laptop.turn(t, heldSession); servedBy != "acct-a" {
				t.Fatalf("next turn served by %q, want acct-a again", servedBy)
			}
			if got := targetsOf(box.seen()); len(got) != 2 || got[1] != "acct-a" {
				t.Fatalf("box targets %v; the next turn must name acct-a", got)
			}
		})
	}
}

// D4 mixed versions: a peer that publishes verdicts but serves a target on
// another account. The turn is served (it already was); the session is not
// held on an account nobody ranked, and until the peer's next usage body it is
// treated as a legacy peer (no target).
func TestPeerSubstitutedTargetIsKeptButReranksNextTurn(t *testing.T) {
	a := crossPoolClaudeScore("acct-a", 1, 30*time.Hour)
	a.ShortHeadroom = 1
	other := crossPoolClaudeScore("other", 0.5, 30*time.Hour)
	box := newContractBox(t, boxAccount{Score: a}, boxAccount{Score: other})
	box.ignoreTarget, box.substitute = true, "other"
	laptop := newCrossPoolLaptop(t, []selectacct.Score{crossPoolClaudeScore("loc", 0.2, 150*time.Hour)}, box.pool)

	status, servedBy := laptop.turn(t, "s1")
	if status != http.StatusOK || servedBy != "other" {
		t.Fatalf("status %d served by %q; the substituted answer must reach the client", status, servedBy)
	}
	if turns := box.seen(); len(turns) != 1 || turns[0].Target != "acct-a" {
		t.Fatalf("box turns %+v; want one turn targeted at acct-a", turns)
	}
	if _, held := holdAccount(box.pool, "s1"); held {
		t.Fatal("the session was held on an account the laptop never ranked")
	}
	if len(laptop.logLines("remote pool peer substituted the target")) != 1 {
		t.Fatalf("no substitution line:\n%s", laptop.logs.String())
	}
	laptop.turn(t, "s2")
	if turns := box.seen(); len(turns) != 2 || turns[1].Target != "" {
		t.Fatalf("box turns %+v; a peer seen ignoring targets must be sent none until its next body", turns)
	}
}

// Only this daemon's own ranking names a target: a client-supplied one must
// never cross a hop.
func TestRemotePoolNeverForwardsAClientSuppliedTarget(t *testing.T) {
	var seen string
	_, server := newTestRemotePool(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("X-Subrouter-Target-Account")
		w.WriteHeader(http.StatusOK)
	})
	req := forwardRequest(t, http.MethodPost, "/v1/messages", []byte(`{}`))
	req.Header.Set("X-Subrouter-Target-Account", "someone-elses-account")
	resp, ok := server.remotePoolForward(req, "claude", "s", "/v1/messages", "", []byte(`{}`), true)
	if !ok {
		t.Fatal("forward declined")
	}
	resp.Body.Close()
	if seen != "" {
		t.Fatalf("the peer received a client-supplied target %q", seen)
	}
}

// --- P8 inside local failover (L11) ------------------------------------------

func rejectExhausted(account string) func(string, http.ResponseWriter) bool {
	return func(served string, w http.ResponseWriter) bool {
		if served != account {
			return false
		}
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"limit reached"}}`)
		return true
	}
}

func rejectTransiently(account string) func(string, http.ResponseWriter) bool {
	return func(served string, w http.ResponseWriter) bool {
		if served != account {
			return false
		}
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "allowed_warning")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
		return true
	}
}

// A warm session whose local account spends its window mid-turn is FORCED off
// it: the next account is chosen across every pool (P8), and a peer account
// that outranks the local failover candidate serves the turn, targeted.
func TestForcedLocalFailoverConsultsPeers(t *testing.T) {
	peer := crossPoolClaudeScore("acct-a", 1, 30*time.Hour)
	peer.ShortHeadroom = 1
	box := newContractBox(t, boxAccount{Score: peer})
	laptop := newCrossPoolLaptop(t, []selectacct.Score{
		crossPoolClaudeScore("warm", 0.3, 150*time.Hour), crossPoolClaudeScore("spare", 0.2, 150*time.Hour),
	}, box.pool)
	laptop.upstream.answer = rejectExhausted("warm")
	if _, err := laptop.store.Put("claude", "s1", "warm", ""); err != nil {
		t.Fatal(err)
	}
	markIdle(laptop.active, "s1", 2*time.Minute)

	status, servedBy := laptop.turn(t, "s1")
	if status != http.StatusOK || servedBy != "acct-a" {
		t.Fatalf("status %d served by %q; want the peer account that outranks spare", status, servedBy)
	}
	if got := laptop.upstream.accounts(); len(got) != 1 || got[0] != "warm" {
		t.Fatalf("local upstream saw %v; want only the exhausted warm account", got)
	}
	if got := targetsOf(box.seen()); len(got) != 1 || got[0] != "acct-a" {
		t.Fatalf("box targets %v, want [acct-a]", got)
	}
}

// CHARACTERIZATION (passes on origin/main by design): a 429 that is not a
// spent window is transient; local failover runs exactly as before and no peer
// is consulted.
func TestTransientLocalFailoverNeverConsultsPeers(t *testing.T) {
	peer := crossPoolClaudeScore("acct-a", 1, 30*time.Hour)
	peer.ShortHeadroom = 1
	box := newContractBox(t, boxAccount{Score: peer})
	laptop := newCrossPoolLaptop(t, []selectacct.Score{
		crossPoolClaudeScore("warm", 0.3, 150*time.Hour), crossPoolClaudeScore("spare", 0.2, 150*time.Hour),
	}, box.pool)
	laptop.upstream.answer = rejectTransiently("warm")
	if _, err := laptop.store.Put("claude", "s1", "warm", ""); err != nil {
		t.Fatal(err)
	}
	markIdle(laptop.active, "s1", 2*time.Minute)

	status, servedBy := laptop.turn(t, "s1")
	if status != http.StatusOK || servedBy != "spare" {
		t.Fatalf("status %d served by %q; want local failover onto spare", status, servedBy)
	}
	if turns := box.seen(); len(turns) != 0 {
		t.Fatalf("box turns %+v; a transient 429 must not consult the peers", turns)
	}
}

// --- P9: a stale (cold) session re-ranks across everything ------------------

// The gap P9 closes: a COLD Claude session whose own account is still the best
// LOCAL one kept it without the peers ever being compared -- selection handed
// the cross-pool decision a zero score, which it read as "warm". A cold turn is
// ranked across every pool: here a box burndown account under its cap wins.
func TestColdSessionOnTheBestLocalAccountIsRankedAgainstThePeers(t *testing.T) {
	box := incidentBox(t, 0)
	laptop := newCrossPoolLaptop(t, []selectacct.Score{crossPoolClaudeScore("fifteen", 0.98, 159*time.Hour)}, box.pool)
	if _, err := laptop.store.Put("claude", "s1", "fifteen", ""); err != nil {
		t.Fatal(err)
	}
	markIdle(laptop.active, "s1", 2*time.Hour)

	status, servedBy := laptop.turn(t, "s1")
	if status != http.StatusOK || servedBy != "chip-5" {
		t.Fatalf("status %d served by %q; a cold session must be ranked against the box's burndown account", status, servedBy)
	}
	decisions := laptop.logLines("remote pool decision")
	if len(decisions) != 1 || decisions[0]["session_state"] != "cold" || decisions[0]["target_account"] != "chip-5" ||
		decisions[0]["level"] != "INFO" {
		t.Fatalf("decision lines %v; want one cold INFO decision targeting chip-5", decisions)
	}
}

// P9's other half: the idle timer (warmSessionIdleTTL, an hour) keeps a
// session idle 50 minutes warm and local, and the log says so.
func TestWarmSessionUnderTheIdleTimerStaysLocalAndSaysWarm(t *testing.T) {
	box := incidentBox(t, 0)
	laptop := newCrossPoolLaptop(t, []selectacct.Score{crossPoolClaudeScore("fifteen", 0.98, 159*time.Hour)}, box.pool)
	if _, err := laptop.store.Put("claude", "s1", "fifteen", ""); err != nil {
		t.Fatal(err)
	}
	markIdle(laptop.active, "s1", 50*time.Minute)

	if _, servedBy := laptop.turn(t, "s1"); servedBy != "fifteen" {
		t.Fatalf("served by %q; a warm session keeps its account", servedBy)
	}
	if turns := box.seen(); len(turns) != 0 {
		t.Fatalf("box turns %+v; a warm session is never offered", turns)
	}
	// Debug (PATCHES §39): a kept turn is most of the log and says nothing moved.
	decisions := laptop.logLines("remote pool decision")
	if len(decisions) != 1 || decisions[0]["reason"] != "session is warm on its account; staying local" || decisions[0]["session_state"] != "warm" ||
		decisions[0]["level"] != "DEBUG" {
		t.Fatalf("decision lines %v; want one DEBUG decision saying warm", decisions)
	}
}

// CHARACTERIZATION (passes on origin/main by design): a delegated session's
// hold lapses on the same idle timer (warmSessionIdleTTL, unchanged), and its
// next turn is a FULL re-rank, not a return to its old local placeholder: here
// it moves to the better local account (the box's only account is spent).
func TestDelegatedSessionIdlePastTheTimerIsReranked(t *testing.T) {
	spent := crossPoolClaudeScore("chip-1", 0.5, 100*time.Hour)
	spent.Headroom, spent.ShortHeadroom = 0, 0
	box := newContractBox(t, boxAccount{Score: spent})
	laptop := newCrossPoolLaptop(t, incidentLocal(), box.pool)
	key := session.ScopedSessionKey("claude", heldSession)
	box.pool.delegations.set(key, time.Now().Add(-(warmSessionIdleTTL + 10*time.Minute)), remotePoolDelegationTTLFor("claude"), "chip-1", box.pool.name())
	box.pins[heldSession] = "chip-1"
	if _, err := laptop.store.Put("claude", heldSession, "fourteen", ""); err != nil {
		t.Fatal(err)
	}
	markIdle(laptop.active, heldSession, 2*time.Hour)
	if _, held := holdAccount(box.pool, heldSession); held {
		t.Fatal("calibration: the hold must have lapsed")
	}
	status, servedBy := laptop.turn(t, heldSession)
	if status != http.StatusOK || servedBy != "fifteen" {
		t.Fatalf("status %d served by %q; want the lapsed session re-ranked onto the better local account", status, servedBy)
	}
	if turns := box.seen(); len(turns) != 0 {
		t.Fatalf("box turns %+v; the spent box account must not be offered", turns)
	}
	if len(laptop.logLines("re-ranking idle session")) != 1 {
		t.Fatalf("no re-rank line:\n%s", laptop.logs.String())
	}
}

// CHARACTERIZATION (passes on origin/main by design; P7): two daemons that
// are each other's peer, both with spent pools. A request that arrived from a
// peer is served there or refused, never re-forwarded, so the client gets one
// hop at most and a 503 -- no loop.
func TestMutualPeersNeverLoop(t *testing.T) {
	type counter struct {
		mu    sync.Mutex
		turns []string // X-Pool-Hop of every inbound turn
	}
	newDaemon := func(t *testing.T, id string) (*Server, *counter, *httptest.Server) {
		spent := crossPoolClaudeScore(id, 0.5, 100*time.Hour)
		spent.Headroom, spent.ShortHeadroom = 0, 0
		laptop := newCrossPoolLaptop(t, []selectacct.Score{spent}, nil)
		server := laptop.server
		c := &counter{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != remotePoolUsagePath {
				c.mu.Lock()
				c.turns = append(c.turns, r.Header.Get(remotePoolHopHeader))
				c.mu.Unlock()
			}
			server.Handler().ServeHTTP(w, r)
		}))
		t.Cleanup(srv.Close)
		return &server, c, srv
	}
	a, aCount, aSrv := newDaemon(t, "a-spent")
	b, bCount, bSrv := newDaemon(t, "b-spent")
	a.RemotePool = NewRemotePool(bSrv.URL, "", "")
	b.RemotePool = NewRemotePool(aSrv.URL, "", "")

	req, err := http.NewRequest(http.MethodPost, aSrv.URL+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", "s1")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: nothing anywhere can serve", response.StatusCode)
	}
	aCount.mu.Lock()
	bCount.mu.Lock()
	defer aCount.mu.Unlock()
	defer bCount.mu.Unlock()
	if len(aCount.turns) != 1 || aCount.turns[0] != "" {
		t.Fatalf("A saw turns with hops %q; want only the client's", aCount.turns)
	}
	if len(bCount.turns) != 1 || bCount.turns[0] != "1" {
		t.Fatalf("B saw turns with hops %q; want exactly one forward at hop 1", bCount.turns)
	}
}
