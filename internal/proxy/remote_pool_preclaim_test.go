package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// A spilled request's session follows it to the peer (PATCHES §21). A new
// Codex session's fresh pick writes a local pin, the comparator then sends the
// turn to the peer, and the hold was set only once the peer answered. Every
// request for the same session in between -- Codex opens the turn's websocket
// and posts on the bare thread id in the same second -- found the pin, read it
// as warm and was served LOCALLY, splitting one conversation across two pools.
// The forward now claims the hold before it leaves, and drops a claim it made
// if the peer did not serve.

func TestAForwardedTurnClaimsTheHoldBeforeThePeerAnswers(t *testing.T) {
	key := session.ScopedSessionKey("codex", "s1")
	var pool *RemotePool
	var heldAtArrival atomic.Bool
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		heldAtArrival.Store(pool.delegations.active(key, time.Now()))
		w.Header().Set(subrouterAccountHeader, "peer-acct")
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(peer.Close)
	pool = NewRemotePool(peer.URL, "", "")
	server := Server{RemotePool: pool}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	resp, ok := server.remotePoolForward(req, "codex", "s1", "/v1/responses", "", []byte(`{}`), true)
	if !ok {
		t.Fatal("the peer's 200 was not served")
	}
	_ = resp.Body.Close()
	if !heldAtArrival.Load() {
		t.Fatal("the session was not held while its first turn was on the peer; a concurrent turn would read the local pin as warm")
	}
	if entry, ok := pool.delegations.lookup(key, time.Now()); !ok || entry.Account != "peer-acct" {
		t.Fatalf("hold after the peer served = %+v (live %v), want one naming peer-acct", entry, ok)
	}
}

func TestAForwardedWebSocketClaimsTheHoldBeforeThePeerAnswers(t *testing.T) {
	var pool *RemotePool
	var heldAtArrival atomic.Bool
	peer := accountStampingPeer(t, func(*http.Request) {
		heldAtArrival.Store(pool.delegations.active(wsHoldKey(), time.Now()))
	})
	pool = NewRemotePool(peer.URL, "", "")
	front, _ := poolFront(t, Server{RemotePool: pool}, remotePoolHoldSet)

	conn := dialFront(t, front)
	echoOnce(t, conn, "turn")
	if !heldAtArrival.Load() {
		t.Fatal("the session was not held while its socket was being opened on the peer")
	}
}

// A claim the forward made is warmth the peer never received when the peer did
// not serve: it must not outlive the attempt. (A hold that existed before keeps
// §20's rules; see TestOneFailedForwardKeepsTheHoldAndADownPeerIsFoundDown and the
// websocket dial-error tests.)
func TestAClaimThePeerNeverServedIsDropped(t *testing.T) {
	type peerKind int
	const (
		deadPeer peerKind = iota
		declinePeer
		transientPeer
	)
	cases := []struct {
		name      string
		kind      peerKind
		cancelled bool
	}{
		{name: "one dead round trip", kind: deadPeer},
		{name: "503 decline", kind: declinePeer},
		{name: "transient 502", kind: transientPeer},
		{name: "client cancel", kind: deadPeer, cancelled: true},
	}
	for _, tc := range cases {
		for _, transport := range []string{"http", "websocket"} {
			t.Run(tc.name+"/"+transport, func(t *testing.T) {
				peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					if tc.kind == transientPeer {
						w.Header().Set(remotePoolTransientHeader, "1")
						http.Error(w, "refresh reached no verdict", http.StatusBadGateway)
						return
					}
					http.Error(w, "pool spent", http.StatusServiceUnavailable)
				}))
				peerURL := peer.URL
				if tc.kind == deadPeer {
					peer.Close()
				} else {
					t.Cleanup(peer.Close)
				}
				server := Server{RemotePool: NewRemotePool(peerURL, "", "")}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if tc.cancelled {
					cancel()
				}

				served := false
				if transport == "http" {
					req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`)).WithContext(ctx)
					var resp *http.Response
					resp, served = server.remotePoolForward(req, "codex", wsHoldSession, "/v1/responses", "", []byte(`{}`), true)
					if resp != nil {
						_ = resp.Body.Close()
					}
				} else {
					served = server.serveRemotePool(httptest.NewRecorder(), webSocketUpgradeRequest(ctx), "codex", wsHoldSession, remotePoolHoldSet)
				}
				if served {
					t.Fatal("calibration: the peer served the request")
				}
				if server.RemotePool.delegations.active(wsHoldKey(), time.Now()) {
					t.Fatal("a session the peer never served is left held there; its next turn would go to the peer for nothing")
				}
				if !server.RemotePool.breaker.allow(time.Now()) {
					t.Fatal("calibration: one attempt opened the breaker")
				}
			})
		}
	}
}

const spillThread = "019a4d2e-7c1b-7f00-9c3e-5b2a1d0e8f77"

// strongCodexPeer is a peer whose codex account paces far ahead of local "loc".
const strongCodexPeer = `[
  {"id":"cdx","provider":"codex","auth_checked":true,"auth_valid":true,
   "normalized":{"headroom":0.95,"short_headroom":1,"fresh":true,
                 "weekly_headroom":0.95,"seconds_to_weekly_reset":360000}}
]`

// spillPeer counts the model requests forwarded to it. With gate set, it holds
// the FIRST one until a second arrives (or 3 s pass), so a test can put a
// concurrent turn inside the window the first one is on the peer.
type spillPeer struct {
	hits         atomic.Int32
	heldAtFirst  atomic.Bool
	firstArrived chan struct{}
	second       chan struct{}
	gate         bool
}

func spillFixture(t *testing.T, gate bool) (Server, *atomic.Int32, *spillPeer) {
	t.Helper()
	key := session.ScopedSessionKey("codex", spillThread+":0")
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	echo := func(w http.ResponseWriter, r *http.Request, header http.Header) {
		conn, err := upgrader.Upgrade(w, r, header)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if conn.WriteMessage(messageType, append([]byte("peer:"), payload...)) != nil {
				return
			}
		}
	}

	var local atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		local.Add(1)
		if websocket.IsWebSocketUpgrade(r) {
			echo(w, r, nil)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(upstream.Close)

	var pool *RemotePool
	peer := &spillPeer{firstArrived: make(chan struct{}), second: make(chan struct{}), gate: gate}
	peerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == remotePoolUsagePath {
			_, _ = io.WriteString(w, strongCodexPeer)
			return
		}
		switch peer.hits.Add(1) {
		case 1:
			peer.heldAtFirst.Store(pool.delegations.active(key, time.Now()))
			close(peer.firstArrived)
			if peer.gate {
				select {
				case <-peer.second:
				case <-time.After(3 * time.Second):
				}
			}
		case 2:
			close(peer.second)
		}
		header := http.Header{subrouterAccountHeader: []string{"peer-acct"}}
		if websocket.IsWebSocketUpgrade(r) {
			echo(w, r, header)
			return
		}
		w.Header().Set(subrouterAccountHeader, "peer-acct")
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(peerServer.Close)
	pool = NewRemotePool(peerServer.URL, "", "")

	server := Server{
		Accounts: []accounts.Account{
			{ID: "loc", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "loc-token"},
		},
		Sessions:       newSessionStore(t),
		ActiveSessions: NewActiveSessions(),
		Scheduler: selectacct.NewScheduler([]selectacct.Score{{
			AccountID: "loc", Provider: accounts.ProviderCodex, Fresh: true,
			Headroom: 0.35, ShortHeadroom: 0.5, WeeklyHeadroom: 0.35, WeeklyRemaining: 0.35,
			WeeklyResetAt: time.Now().Add(100 * time.Hour),
		}}),
		Upstream:     mustURL(t, upstream.URL),
		RemotePool:   pool,
		MaxBodyBytes: 1024,
	}

	// Calibration: a NEW session's fresh pick is "loc", and the comparator
	// sends it to the peer. Without both, the tests below prove nothing.
	account, _, _, capacity, err := server.accountForSessionProvider(accounts.ProviderCodex, "codex", "calibration", codexResponsesRequest())
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != "loc" || capacity.Score.AccountID == "" {
		t.Fatalf("calibration: fresh pick = %q with score %q; want loc with a rankable score", account.ID, capacity.Score.AccountID)
	}
	peers, _, _ := pool.decisionCandidates(pool.AdminGet(context.Background(), remotePoolUsagePath), string(accounts.ProviderCodex), "", nil, time.Now())
	if placement, err := placeAcrossPools(server.scheduler(), []accounts.Account{account}, peers); err != nil || placement.Peer == nil {
		t.Fatalf("calibration: the ranking kept a new session local (%v): %+v", err, placement)
	}
	return server, &local, peer
}

func codexTurnHeader() http.Header {
	return http.Header{
		"X-Subrouter-Agent": []string{"codex"},
		"X-Codex-Window-Id": []string{spillThread + ":0"},
		"Content-Type":      []string{"application/json"},
	}
}

func postCodexTurn(frontURL string) (int, error) {
	req, err := http.NewRequest(http.MethodPost, frontURL+"/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		return 0, err
	}
	req.Header = codexTurnHeader()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// The evidence row (L-8): the turn's websocket spills to the peer, then the
// POST on the same thread is served locally off the pin the fresh pick wrote.
func TestASpilledCodexSessionFollowsItsWebSocketTurnToThePeer(t *testing.T) {
	server, local, peer := spillFixture(t, false)
	front := httptest.NewServer(server.Handler())
	t.Cleanup(front.Close)

	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http")+"/v1/responses", codexTurnHeader())
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("websocket dial failed (status %d): %v", status, err)
	}
	defer conn.Close()
	echoOnce(t, conn, "turn")
	if status, err := postCodexTurn(front.URL); err != nil || status != http.StatusOK {
		t.Fatalf("POST on the same thread: status %d, err %v", status, err)
	}

	if !peer.heldAtFirst.Load() {
		t.Error("the session was not held while its first turn was on the peer")
	}
	if got, want := local.Load(), int32(0); got != want {
		t.Errorf("local upstream served %d requests of a conversation that lives on the peer; want %d", got, want)
	}
	if got, want := peer.hits.Load(), int32(2); got != want {
		t.Errorf("peer served %d requests; want %d (the socket and the POST)", got, want)
	}
}

// The race itself: a second turn arrives while the first is still on the peer
// (before its headers). It must follow the first there, not split the thread.
func TestAConcurrentTurnFollowsAPendingSpillToThePeer(t *testing.T) {
	server, local, peer := spillFixture(t, true)
	front := httptest.NewServer(server.Handler())
	t.Cleanup(front.Close)

	type result struct {
		status int
		err    error
	}
	first := make(chan result, 1)
	go func() {
		status, err := postCodexTurn(front.URL)
		first <- result{status, err}
	}()
	select {
	case <-peer.firstArrived:
	case <-time.After(5 * time.Second):
		t.Fatal("calibration: the first turn never reached the peer")
	}
	if status, err := postCodexTurn(front.URL); err != nil || status != http.StatusOK {
		t.Fatalf("second turn: status %d, err %v", status, err)
	}
	if r := <-first; r.err != nil || r.status != http.StatusOK {
		t.Fatalf("first turn: status %d, err %v", r.status, r.err)
	}

	if got, want := local.Load(), int32(0); got != want {
		t.Errorf("local upstream served %d turns of a thread whose first turn was on the peer; want %d", got, want)
	}
	if got, want := peer.hits.Load(), int32(2); got != want {
		t.Errorf("peer served %d turns; want %d", got, want)
	}
}

// The claim primitive. "Created" means this call inserted a new live entry;
// dropping it is only ever this call's own pending claim.
func TestAClaimOnlyEverInsertsAndOnlyItsOwnerDropsIt(t *testing.T) {
	const key = "codex:s1"
	ttl := remotePoolDelegationTTLFor("codex")
	now := time.Now()

	t.Run("a live hold is left alone", func(t *testing.T) {
		var d remotePoolDelegations
		d.set(key, now, ttl, "peer-acct", "box")
		if id := d.claim(key, now, ttl, "box"); id != 0 {
			t.Fatalf("claim over a live hold returned %d; it must not count as created", id)
		}
		d.release(key, 1)
		if entry, ok := d.lookup(key, now); !ok || entry.Account != "peer-acct" {
			t.Fatalf("the existing hold changed: %+v (live %v)", entry, ok)
		}
	})
	t.Run("an expired hold counts as absent", func(t *testing.T) {
		var d remotePoolDelegations
		d.set(key, now.Add(-2*ttl), ttl, "old", "box")
		if id := d.claim(key, now, ttl, "box"); id == 0 {
			t.Fatal("claim over an expired hold did not create one")
		}
	})
	t.Run("its owner drops an unserved claim", func(t *testing.T) {
		var d remotePoolDelegations
		id := d.claim(key, now, ttl, "box")
		if id == 0 || !d.active(key, now) {
			t.Fatal("claim on a free session did not hold it")
		}
		d.release(key, id)
		if d.active(key, now) {
			t.Fatal("release left the unserved claim in place")
		}
	})
	t.Run("a served turn confirms the claim", func(t *testing.T) {
		for name, confirm := range map[string]func(d *remotePoolDelegations){
			"set":   func(d *remotePoolDelegations) { d.set(key, now, ttl, "peer-acct", "box") },
			"touch": func(d *remotePoolDelegations) { d.touch(key, now, ttl) },
		} {
			var d remotePoolDelegations
			id := d.claim(key, now, ttl, "box")
			confirm(&d)
			d.release(key, id)
			if !d.active(key, now) {
				t.Fatalf("after a %s by a served request, the first forward's release dropped the hold", name)
			}
		}
	})
	t.Run("a claim made after a clear is not the old owner's", func(t *testing.T) {
		var d remotePoolDelegations
		first := d.claim(key, now, ttl, "box")
		d.clear(key)
		second := d.claim(key, now, ttl, "box")
		if second == 0 || second == first {
			t.Fatalf("second claim id = %d (first %d); want a new, distinct id", second, first)
		}
		d.release(key, first)
		if !d.active(key, now) {
			t.Fatal("the first forward dropped a claim another forward made")
		}
	})
	t.Run("concurrent claims create exactly one", func(t *testing.T) {
		var d remotePoolDelegations
		var created atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if d.claim(key, time.Now(), ttl, "box") != 0 {
					created.Add(1)
				}
			}()
		}
		wg.Wait()
		if got := created.Load(); got != 1 {
			t.Fatalf("%d concurrent claims reported created; want exactly 1", got)
		}
	})
	t.Run("a pending claim is not persisted", func(t *testing.T) {
		var d remotePoolDelegations
		d.claim(key, now, ttl, "box")
		path := t.TempDir() + "/holds.json"
		d.flush(path, now)
		var reloaded remotePoolDelegations
		reloaded.load(path, now)
		if reloaded.active(key, now) {
			t.Fatal("a forward still in flight was written out as a hold the peer served")
		}
		d.set(key, now, ttl, "peer-acct", "box")
		d.flush(path, now)
		reloaded.load(path, now)
		if !reloaded.active(key, now) {
			t.Fatal("the confirmed hold was not written")
		}
	})
}
