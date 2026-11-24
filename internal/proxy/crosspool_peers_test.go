package proxy

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	agentclaude "github.com/discordwell/subrouter/internal/agents/claude"
	"github.com/discordwell/subrouter/selectacct"
	"github.com/discordwell/subrouter/session"
)

// PATCHES §31, the deciding half: the pieces with no pre-§31 counterpart --
// the forced/transient classifier, N peers, the peers file. Each was watched
// failing by a named single-piece mutation rather than on origin/main, where
// the API does not exist.

// --- D4b: the ONE forced-vs-transient classifier ----------------------------

func TestClassifyAccountSwitchTable(t *testing.T) {
	transient := http.Header{}
	transient.Set(remotePoolTransientHeader, "1")
	refused := http.Header{}
	refused.Set(remotePoolTargetRefusedHeader, "at_cap")
	for _, tc := range []struct {
		row    int
		name   string
		e      accountSwitchEvidence
		kind   accountSwitchKind
		reason string
	}{
		{1, "a client cancel says nothing about the account", accountSwitchEvidence{ClientCanceled: true, PeerErr: context.Canceled}, switchNone, "client_canceled"},
		{2, "a target refusal on any status", accountSwitchEvidence{PeerStatus: http.StatusOK, PeerHeader: refused}, switchForced, "at_cap"},
		// §54's row, the table's 21st, checked before row 3.
		{21, "the breaker opened on an outage not attributed to the peer: a blip (§54)", accountSwitchEvidence{PeerErr: errors.New("dial"), PeerBreakerOpened: true, PeerOutageUnattributed: true}, switchTransient, "peer_unattributed"},
		{3, "the breaker opened: the peer is down", accountSwitchEvidence{PeerErr: errors.New("dial"), PeerBreakerOpened: true}, switchForced, "peer_down"},
		{4, "one failed round trip is a blip", accountSwitchEvidence{PeerErr: errors.New("dial")}, switchTransient, "peer_unreachable"},
		{5, "502 + transient: refresh reached no verdict", accountSwitchEvidence{PeerStatus: http.StatusBadGateway, PeerHeader: transient}, switchTransient, "refresh_no_verdict"},
		{6, "429 + transient: limited, not spent", accountSwitchEvidence{PeerStatus: http.StatusTooManyRequests, PeerHeader: transient}, switchTransient, "rate_limited"},
		{7, "a legacy 429: the peer's pool is spent", accountSwitchEvidence{PeerStatus: http.StatusTooManyRequests, PeerHeader: http.Header{}}, switchForced, "peer_declined"},
		{7, "a legacy or lifecycle 503", accountSwitchEvidence{PeerStatus: http.StatusServiceUnavailable, PeerHeader: http.Header{}}, switchForced, "peer_declined"},
		{8, "a refresh with no verdict", accountSwitchEvidence{RefreshErr: fmt.Errorf("%w: blip", errRefreshTransient)}, switchTransient, "refresh_no_verdict"},
		{9, "a terminal refresh failure", accountSwitchEvidence{RefreshErr: errors.New("invalid_grant")}, switchForced, "auth_invalid"},
		{10, "the account is gone", accountSwitchEvidence{AccountGone: true}, switchForced, "unknown_account"},
		{11, "evicted: a spent window", accountSwitchEvidence{Evicted: "exhausted"}, switchForced, "exhausted"},
		{11, "evicted: the holdout", accountSwitchEvidence{Evicted: "holdout"}, switchForced, "holdout"},
		{12, "a credential failure", accountSwitchEvidence{CredentialFailure: true, UsageLimited: true}, switchForced, "auth_invalid"},
		{13, "a spent window, as the upstream said", accountSwitchEvidence{Exhausted: true, UsageLimited: true}, switchForced, "exhausted"},
		{14, "overload is upstream-wide", accountSwitchEvidence{Overload: true}, switchTransient, "overload"},
		{15, "limited but not spent", accountSwitchEvidence{UsageLimited: true}, switchTransient, "rate_limited"},
		{16, "nothing about the account", accountSwitchEvidence{PeerStatus: http.StatusOK, PeerHeader: http.Header{}}, switchNone, ""},
		// R3 (D4b): the rows the table was missing.
		{17, "capacity eviction: shed the model twice running (§27)", accountSwitchEvidence{CapacityEvicted: true}, switchForced, "capacity_evicted"},
		{17, "an eviction outranks a capacity release", accountSwitchEvidence{Evicted: "exhausted", CapacityEvicted: true}, switchForced, "exhausted"},
		{18, "a credential read that got no answer (§28)", accountSwitchEvidence{RefreshErr: fmt.Errorf("read: %w", agentclaude.ErrCredentialUnchecked)}, switchTransient, "credential_unchecked"},
		{18, "the same, wrapped by the request path", accountSwitchEvidence{RefreshErr: fmt.Errorf("%w: %w", errRefreshTransient, agentclaude.ErrCredentialUnchecked)}, switchTransient, "credential_unchecked"},
		{18, "a silent credential store", accountSwitchEvidence{StoreSilent: true}, switchTransient, "credential_unchecked"},
		{18, "a store that refuses is a verdict", accountSwitchEvidence{RefreshErr: fmt.Errorf("read: %w", agentclaude.ErrCredentialUnavailable)}, switchForced, "auth_invalid"},
		{19, "rejected by a model-pool window alone: forced for that model", accountSwitchEvidence{PoolRejected: true, UsageLimited: true}, switchForced, "pool_exhausted"},
	} {
		t.Run(fmt.Sprintf("row %d %s", tc.row, tc.name), func(t *testing.T) {
			got := classifyAccountSwitch(tc.e)
			if got.Kind != tc.kind || got.Reason != tc.reason {
				t.Fatalf("classifyAccountSwitch = %s/%q, want %s/%q", got.Kind, got.Reason, tc.kind, tc.reason)
			}
		})
	}
}

// The classifier's peer rows are fed by the real forward, not by the table's
// hand-built evidence: every answer a peer can give lands in the row it names.
func TestPeerForwardOutcomesAreClassifiedByTheOneTable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(w http.ResponseWriter)
		kind   accountSwitchKind
		reason string
	}{
		{"409 target refusal", func(w http.ResponseWriter) { writeTargetRefusal(w, "acct-a", "at_cap", false, nil) }, switchForced, "at_cap"},
		{"502 + transient", func(w http.ResponseWriter) {
			w.Header().Set(remotePoolTransientHeader, "1")
			w.WriteHeader(http.StatusBadGateway)
		}, switchTransient, "refresh_no_verdict"},
		{"429 + transient", func(w http.ResponseWriter) {
			w.Header().Set(remotePoolTransientHeader, "1")
			w.WriteHeader(http.StatusTooManyRequests)
		}, switchTransient, "rate_limited"},
		{"legacy 503", func(w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) }, switchForced, "peer_declined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, server := newTestRemotePool(t, func(w http.ResponseWriter, r *http.Request) { tc.answer(w) })
			req := forwardRequest(t, http.MethodPost, "/v1/messages", []byte(`{}`))
			out := server.forwardToPeer(server.RemotePool, req, "claude", "s1", "/v1/messages", "", []byte(`{}`), remotePoolHoldSet, "acct-a")
			if out.Served || out.Switch.Kind != tc.kind || out.Switch.Reason != tc.reason {
				t.Fatalf("outcome served=%v %s/%q, want %s/%q", out.Served, out.Switch.Kind, out.Switch.Reason, tc.kind, tc.reason)
			}
		})
	}
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	server := Server{RemotePool: NewRemotePool(deadURL, "", "")}
	req := forwardRequest(t, http.MethodPost, "/v1/messages", []byte(`{}`))
	out := server.forwardToPeer(server.RemotePool, req, "claude", "s1", "/v1/messages", "", []byte(`{}`), remotePoolHoldSet, "acct-a")
	if !out.Unreachable || out.Switch.Kind != switchTransient || out.Switch.Reason != "peer_unreachable" {
		t.Fatalf("a refused connection = %+v, want a transient peer_unreachable", out.Switch)
	}
}

// --- D7: the box's own pick is the placement with zero peers -----------------

// CHARACTERIZATION: pickRoutingAccount is now placeAcrossPools with no peers.
// Across random pools it must pick exactly what Scheduler.Pick picked before.
func TestPickRoutingAccountIsTheZeroPeerPlacement(t *testing.T) {
	rng := rand.New(rand.NewSource(25))
	now := time.Now()
	for trial := 0; trial < 300; trial++ {
		var scores []selectacct.Score
		var candidates []accounts.Account
		for i := 0; i < 1+rng.Intn(6); i++ {
			id := fmt.Sprintf("acct-%d", i)
			scores = append(scores, selectacct.Score{
				AccountID: id, Provider: accounts.ProviderClaude, Fresh: rng.Intn(4) > 0,
				Headroom: rng.Float64(), ShortHeadroom: rng.Float64(),
				WeeklyHeadroom: rng.Float64(), WeeklyResetAt: now.Add(time.Duration(1+rng.Intn(160)) * time.Hour),
				LiveSessions: rng.Intn(3),
			})
			candidates = append(candidates, accounts.Account{ID: id, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth})
		}
		scheduler := selectacct.NewScheduler(scores)
		want, wantErr := scheduler.Pick(candidates)
		got, err := pickRoutingAccount(scheduler, candidates)
		if (wantErr != nil) != (err != nil) || got.ID != want.ID {
			t.Fatalf("trial %d: pickRoutingAccount = %q (%v), Scheduler.Pick = %q (%v)", trial, got.ID, err, want.ID, wantErr)
		}
	}
}

// --- D0: N peers --------------------------------------------------------------

func twoPeerLaptop(t *testing.T, local []selectacct.Score, a, b *contractBox) *crossPoolLaptop {
	t.Helper()
	laptop := newCrossPoolLaptop(t, local, nil)
	laptop.server.RemotePeers = []*RemotePool{a.pool.WithName("boxa"), b.pool.WithName("boxb")}
	return laptop
}

func peerScore(id string, weekly float64, roll time.Duration) boxAccount {
	score := crossPoolClaudeScore(id, weekly, roll)
	score.ShortHeadroom = 1
	return boxAccount{Score: score}
}

// P7: one ranking over local and EVERY peer; a refusal on one peer excludes
// only that account and recomputes over the rest.
func TestTwoPeersOneRefusesRecomputesOverLocalAndTheOther(t *testing.T) {
	a := newContractBox(t, peerScore("a1", 1, 30*time.Hour))
	a.refuse["a1"] = "at_cap"
	b := newContractBox(t, peerScore("b1", 0.8, 30*time.Hour))
	laptop := twoPeerLaptop(t, []selectacct.Score{crossPoolClaudeScore("loc", 0.2, 150*time.Hour)}, a, b)

	status, servedBy := laptop.turn(t, "s1")
	if status != http.StatusOK || servedBy != "b1" {
		t.Fatalf("status %d served by %q; want b1 on the other peer after a1 refused", status, servedBy)
	}
	if got := targetsOf(a.seen()); len(got) != 1 || got[0] != "a1" {
		t.Fatalf("peer A targets %v, want [a1]", got)
	}
	if got := targetsOf(b.seen()); len(got) != 1 || got[0] != "b1" {
		t.Fatalf("peer B targets %v, want [b1]", got)
	}
	p, hold, ok := laptop.server.peerHold(session.ScopedSessionKey("claude", "s1"))
	if !ok || p != b.pool || hold.Account != "b1" || hold.Peer != "boxb" {
		t.Fatalf("hold = %+v on %v; want b1 on boxb", hold, p)
	}
}

// An unreachable peer is excluded for the turn (it counts toward the bound,
// Deviation X5) and the ranking falls to the next peer; its breaker is its own.
func TestTwoPeersOneUnreachableRecomputesOverTheRest(t *testing.T) {
	a := newContractBox(t, peerScore("a1", 1, 30*time.Hour))
	b := newContractBox(t, peerScore("b1", 0.8, 30*time.Hour))
	laptop := twoPeerLaptop(t, []selectacct.Score{crossPoolClaudeScore("loc", 0.2, 150*time.Hour)}, a, b)
	a.pool.AdminGet(context.Background(), remotePoolUsagePath) // A was measured, then went away
	a.srv.Close()

	status, servedBy := laptop.turn(t, "s1")
	if status != http.StatusOK || servedBy != "b1" {
		t.Fatalf("status %d served by %q; want b1 while A is unreachable", status, servedBy)
	}
	for i := 1; i < remotePoolFailuresToOpen; i++ {
		laptop.turn(t, fmt.Sprintf("s%d", i+1))
	}
	if a.pool.breaker.allow(time.Now()) {
		t.Fatalf("calibration: A's breaker did not open after %d failures", remotePoolFailuresToOpen)
	}
	if !b.pool.breaker.allow(time.Now()) {
		t.Fatal("A going down opened B's breaker")
	}
	before := len(b.seen())
	if _, servedBy := laptop.turn(t, "after-open"); servedBy != "b1" || len(b.seen()) != before+1 {
		t.Fatalf("with A's breaker open, served by %q; want B to keep serving", servedBy)
	}
}

// One slow peer costs at most the decision budget, once, and never blocks the
// decision against the others (D0: fetches off the hot path).
func TestSlowPeerDoesNotDelayDecisionsAgainstTheOthers(t *testing.T) {
	a := newContractBox(t, peerScore("a1", 0.6, 30*time.Hour))
	b := newContractBox(t, peerScore("b1", 1, 30*time.Hour))
	laptop := twoPeerLaptop(t, []selectacct.Score{crossPoolClaudeScore("loc", 0.2, 150*time.Hour)}, a, b)
	if _, servedBy := laptop.turn(t, "warmup"); servedBy != "b1" {
		t.Fatalf("calibration: served by %q, want b1", servedBy)
	}
	a.mu.Lock()
	a.usageDelay = 2500 * time.Millisecond
	a.mu.Unlock()
	expireAdminCache(a.pool)
	expireAdminCache(b.pool)

	start := time.Now()
	status, servedBy := laptop.turn(t, "s1")
	elapsed := time.Since(start)
	if status != http.StatusOK || servedBy != "b1" {
		t.Fatalf("status %d served by %q; want b1", status, servedBy)
	}
	if elapsed > remotePoolDecisionFetchBudget+700*time.Millisecond {
		t.Fatalf("the decision took %s behind a peer stalled for 2.5s; want about the %s budget", elapsed, remotePoolDecisionFetchBudget)
	}
}

// A session is held in one place: a claim on one peer clears it on the others.
func TestAPeerHoldHasOneOwner(t *testing.T) {
	a := newContractBox(t, peerScore("a1", 1, 30*time.Hour))
	b := newContractBox(t, peerScore("b1", 1, 30*time.Hour))
	server := Server{RemotePeers: []*RemotePool{a.pool.WithName("boxa"), b.pool.WithName("boxb")}}
	key := session.ScopedSessionKey("claude", "s1")
	a.pool.delegations.set(key, time.Now(), warmSessionIdleTTL, "a1", "boxa")

	req := forwardRequest(t, http.MethodPost, "/v1/messages", []byte(`{}`))
	out := server.forwardToPeer(b.pool, req, "claude", "s1", "/v1/messages", "", []byte(`{}`), remotePoolHoldSet, "b1")
	if !out.Served {
		t.Fatalf("forward to B failed: %+v", out.Switch)
	}
	out.Resp.Body.Close()
	if a.pool.delegations.active(key, time.Now()) {
		t.Fatal("the session is still held on A after B took it")
	}
	if p, hold, ok := server.peerHold(key); !ok || p != b.pool || hold.Account != "b1" {
		t.Fatalf("peerHold = %v %+v %v; want b1 on B", p, hold, ok)
	}
	views := Server{RemotePeers: server.RemotePeers, Sessions: mustSessionStore(t, "claude", "s1", "placeholder")}.sessionViews()
	if len(views) != 1 || views[0].ServedPeer != "boxb" || views[0].ServedAccount != "b1" {
		t.Fatalf("sessions view %+v; want served by boxb on b1", views)
	}
}

func mustSessionStore(t *testing.T, agent, sessionID, account string) *session.Store {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(agent, sessionID, account, ""); err != nil {
		t.Fatal(err)
	}
	return store
}

// Each peer keeps its holds in its own file; the first keeps peer-holds.json so
// a single-peer daemon is unchanged, and an entry naming another peer is never
// attributed to this one.
func TestPeerHoldsPersistPerPeer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "peer-holds.json")
	now := time.Now()
	key := session.ScopedSessionKey("claude", "s1")
	first := NewRemotePool("http://box.example:31416", "", "").WithName("box")
	first.delegations.set(key, now, warmSessionIdleTTL, "a1", "box")
	first.delegations.set(session.ScopedSessionKey("claude", "stray"), now, warmSessionIdleTTL, "x", "box2")
	first.delegations.flush(path, now)
	second := NewRemotePool("http://box2.example:31416", "", "").WithName("box2")
	second.delegations.set(key, now, warmSessionIdleTTL, "b1", "box2")
	second.delegations.flush(filepath.Join(dir, "peer-holds-box2.json"), now)

	loaded := PersistHoldsAt(context.Background(), []*RemotePool{
		NewRemotePool("http://box.example:31416", "", "").WithName("box"),
		NewRemotePool("http://box2.example:31416", "", "").WithName("box2"),
	}, path)
	if hold, ok := loaded[0].delegations.lookup(key, now); !ok || hold.Account != "a1" {
		t.Fatalf("first peer loaded %+v %v; want a1 from peer-holds.json", hold, ok)
	}
	if loaded[0].delegations.active(session.ScopedSessionKey("claude", "stray"), now) {
		t.Fatal("a hold naming box2 was attributed to box")
	}
	if hold, ok := loaded[1].delegations.lookup(key, now); !ok || hold.Account != "b1" {
		t.Fatalf("second peer loaded %+v %v; want b1 from peer-holds-box2.json", hold, ok)
	}
}

// --- SUBROUTER_REMOTE_POOLS_FILE ---------------------------------------------

func writePeersFile(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "peers.json")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRemotePeersFileParsing(t *testing.T) {
	const secret = "tok-s3cr3t-do-not-log"
	valid := `{"peers":[
		{"name":"box","url":"http://100.64.10.1:31416","token":"` + secret + `","allow":"100.64.0.0/10"},
		{"name":"box2","url":"https://203.0.113.7:31416","token":"t2","fingerprint":"ab12"}]}`
	t.Run("two peers, named, in order", func(t *testing.T) {
		peers, err := LoadRemotePeersFile(writePeersFile(t, valid, 0o600))
		if err != nil {
			t.Fatal(err)
		}
		if len(peers) != 2 || peers[0].name() != "box" || peers[1].name() != "box2" ||
			peers[0].Token != secret || peers[1].Fingerprint == "" || len(peers[0].AllowFrom) != 1 {
			t.Fatalf("parsed %+v", peers)
		}
	})
	for _, tc := range []struct {
		name string
		body string
		mode os.FileMode
		want string
	}{
		{"readable by others", valid, 0o644, "chmod 600"},
		{"unknown field", `{"peers":[{"name":"box","url":"http://a:1","token":"` + secret + `","tokne":"x"}]}`, 0o600, "unknown field"},
		{"duplicate name, any case", `{"peers":[{"name":"box","url":"http://a:1"},{"name":"BOX","url":"http://b:1"}]}`, 0o600, "duplicates an earlier peer"},
		{"duplicate url", `{"peers":[{"name":"a","url":"http://a:1"},{"name":"b","url":"http://a:1"}]}`, 0o600, "field url duplicates"},
		{"bad name", `{"peers":[{"name":"../etc","url":"http://a:1"}]}`, 0o600, "field name"},
		{"no url", `{"peers":[{"name":"a","url":""}]}`, 0o600, "field url"},
		{"no peers", `{"peers":[]}`, 0o600, "lists no peers"},
		{"trailing data", valid + `{}`, 0o600, "trailing data"},
		{"not JSON, quoting a secret", `{"peers":[{"name":"a","token":` + secret + `}]}`, 0o600, "not valid JSON"},
		{"one bad entry among good ones fails closed", `{"peers":[{"name":"good","url":"http://a:1","token":"` + secret + `"},{"name":"bad","url":"nope"}]}`, 0o600, "entry 1 (bad)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peers, err := LoadRemotePeersFile(writePeersFile(t, tc.body, tc.mode))
			if err == nil || peers != nil {
				t.Fatalf("got %d peers, err %v; want no peers and an error", len(peers), err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not say %q", err, tc.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("the error leaked a token: %q", err)
			}
		})
	}
	t.Run("seventeen peers", func(t *testing.T) {
		var entries []string
		for i := 0; i < remotePeersMax+1; i++ {
			entries = append(entries, fmt.Sprintf(`{"name":"p%d","url":"http://p%d:1"}`, i, i))
		}
		if peers, err := LoadRemotePeersFile(writePeersFile(t, `{"peers":[`+strings.Join(entries, ",")+`]}`, 0o600)); err == nil || peers != nil {
			t.Fatalf("accepted %d peers", len(peers))
		}
	})
}

// --- the one reader of Server.RemotePool --------------------------------------

// Every production path reaches the peer set through s.peers(); a new direct
// read of Server.RemotePool would silently serve the first peer only.
func TestNoProductionCodeReadsServerRemotePoolDirectly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	direct := regexp.MustCompile(`\b(s|server|srv)\.RemotePool\b`)
	var offenders []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") || file == "remote_pool_peers.go" {
			continue
		}
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if direct.MatchString(line) {
				offenders = append(offenders, fmt.Sprintf("%s:%d: %s", file, i+1, strings.TrimSpace(line)))
			}
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("read Server.RemotePool directly instead of through s.peers():\n%s", strings.Join(offenders, "\n"))
	}
}

// A peer whose usage fetch fails is retried once per remotePoolAdminTTL, not
// on every decision: otherwise a down peer charges every request the decision
// budget.
func TestAFailingPeerIsFetchedOncePerTTL(t *testing.T) {
	var hits int
	var mu sync.Mutex
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		http.Error(w, "down for maintenance", http.StatusInternalServerError)
	}))
	defer peer.Close()
	pool := NewRemotePool(peer.URL, "", "")
	for i := 0; i < 5; i++ {
		if snapshot := pool.usageForDecision(time.Now().Add(remotePoolDecisionFetchBudget)); len(snapshot.Body) != 0 {
			t.Fatalf("a failing peer produced a body: %q", snapshot.Body)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Fatalf("the failing usage endpoint was fetched %d times in one TTL, want 1", hits)
	}
}
