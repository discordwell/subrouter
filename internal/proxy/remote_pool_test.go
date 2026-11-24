package proxy

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
)

// newTestRemotePool points a pool at a test server and returns both.
func newTestRemotePool(t *testing.T, handler http.HandlerFunc) (*httptest.Server, Server) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	pool := NewRemotePool(upstream.URL, "", "")
	if pool == nil {
		t.Fatalf("NewRemotePool returned nil for %q", upstream.URL)
	}
	return upstream, Server{RemotePool: pool}
}

func forwardRequest(t *testing.T, method, path string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	return req
}

// The path the peer receives must be the CLIENT-facing one. By the time the
// transport stage runs, pathForUpstream has already rewritten the request's own
// path for the selected account. That rewrite is identity for Claude, which is
// why this bug is invisible there -- but a Codex /backend-api/... has been
// stripped, and forwarding the stripped form makes the peer re-resolve it to a
// different endpoint.
func TestRemotePoolForwardsClientFacingPath(t *testing.T) {
	for _, tc := range []struct {
		name       string
		clientPath string
		// what the local handler would have rewritten the request path to
		rewritten string
	}{
		{"codex backend-api", "/backend-api/wham/tasks", "/wham/tasks"},
		{"codex responses", "/v1/responses", "/responses"},
		{"claude messages", "/v1/messages", "/v1/messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			_, server := newTestRemotePool(t, func(w http.ResponseWriter, r *http.Request) {
				got = r.URL.Path
				w.WriteHeader(http.StatusOK)
			})
			// Request carries the REWRITTEN path, as proxyRequest would.
			req := forwardRequest(t, http.MethodPost, tc.rewritten, []byte(`{}`))
			resp, ok := server.remotePoolForward(req, "claude", "sess-1", tc.clientPath, "", []byte(`{}`), true)
			if !ok {
				t.Fatal("forward declined")
			}
			resp.Body.Close()
			if got != tc.clientPath {
				t.Fatalf("peer saw path %q, want the client-facing %q", got, tc.clientPath)
			}
		})
	}
}

// The peer derives provider from agent type, not path, and defaults to codex.
// A Claude forward that loses this header asks the peer's codex pool and comes
// back "no accounts available" while its Claude pool sits idle.
func TestRemotePoolSetsAgentAndSessionMarkers(t *testing.T) {
	var agent, session string
	_, server := newTestRemotePool(t, func(w http.ResponseWriter, r *http.Request) {
		agent = r.Header.Get("X-Subrouter-Agent")
		session = r.Header.Get("X-Subrouter-Session")
		w.WriteHeader(http.StatusOK)
	})
	req := forwardRequest(t, http.MethodPost, "/v1/messages", []byte(`{}`))
	resp, ok := server.remotePoolForward(req, "claude", "sess-abc", "/v1/messages", "", []byte(`{}`), true)
	if !ok {
		t.Fatal("forward declined")
	}
	resp.Body.Close()
	if agent != "claude" {
		t.Fatalf("X-Subrouter-Agent = %q, want claude (peer would default to codex)", agent)
	}
	if session != "sess-abc" {
		t.Fatalf("X-Subrouter-Session = %q, want sess-abc", session)
	}
}

// The peer picks its own account and injects its own credential, so ours must
// not travel across the tailnet.
func TestRemotePoolStripsLocalCredentials(t *testing.T) {
	var auth, apiKey string
	_, server := newTestRemotePool(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		apiKey = r.Header.Get("X-Api-Key")
		w.WriteHeader(http.StatusOK)
	})
	req := forwardRequest(t, http.MethodPost, "/v1/messages", []byte(`{}`))
	req.Header.Set("Authorization", "Bearer local-oauth-token")
	req.Header.Set("X-Api-Key", "local-api-key")

	resp, ok := server.remotePoolForward(req, "claude", "s", "/v1/messages", "", []byte(`{}`), true)
	if !ok {
		t.Fatal("forward declined")
	}
	resp.Body.Close()
	if auth != "" || apiKey != "" {
		t.Fatalf("local credentials leaked: Authorization=%q X-Api-Key=%q", auth, apiKey)
	}
}

// An account selector names an account in THIS pool. On the peer a forced
// selector is a strict pin (an unknown id is a 400, and a forced pick disables
// the peer's own failover) and a preferred one steers its pick, so neither may
// ride the hop: the peer chooses from its own accounts.
func TestRemotePoolNeverForwardsAccountSelectors(t *testing.T) {
	selectors := []string{"X-Subrouter-Account", "X-Subrouter-Account-ID", "X-Subrouter-Preferred-Account-ID"}
	var seen http.Header
	_, server := newTestRemotePool(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	})
	req := forwardRequest(t, http.MethodPost, "/v1/messages", []byte(`{}`))
	for _, name := range selectors {
		req.Header.Set(name, "local-account")
	}

	resp, ok := server.remotePoolForward(req, "claude", "s", "/v1/messages", "", []byte(`{}`), true)
	if !ok {
		t.Fatal("forward declined")
	}
	resp.Body.Close()
	if seen == nil {
		t.Fatal("the peer never received the forward")
	}
	for _, name := range selectors {
		if got := seen.Get(name); got != "" {
			t.Errorf("%s = %q reached the peer; a local account selector must not cross the hop", name, got)
		}
	}
}

func TestNewRemotePoolConfigParsing(t *testing.T) {
	if pool := NewRemotePool("", "", ""); pool != nil {
		t.Fatal("empty config must disable the feature")
	}
	if pool := NewRemotePool("not a url", "", ""); pool != nil {
		t.Fatal("unparseable config must disable the feature")
	}
	if pool := NewRemotePool("http://box.example:31415/", "", ""); pool == nil {
		t.Fatal("a valid peer must be accepted")
	} else if pool.URL.Path != "" {
		t.Fatalf("trailing slash must be trimmed, got path %q", pool.URL.Path)
	}
	// Loopback is intentionally permitted: two daemons on one host with
	// different ports are legitimate, and a genuine self-target terminates via
	// the hop guard rather than being rejected here. See the constructor.
	if pool := NewRemotePool("http://127.0.0.1:31999", "", ""); pool == nil {
		t.Fatal("loopback peers must be allowed; the hop counter handles loops")
	}
}

// Whose subscription gets spent is a policy question. On a shared box an
// unrestricted peer quietly turns one person's personal accounts into everyone
// else's overflow capacity.
func TestRemotePoolSourceFilter(t *testing.T) {
	for _, tc := range []struct {
		name, allow, from string
		want              bool
	}{
		{"open by default", "", "100.99.99.99", true},
		{"explicit star is open", "*", "100.99.99.99", true},
		{"single ip allowed", "100.64.10.3", "100.64.10.3", true},
		{"single ip blocks others", "100.64.10.3", "100.64.10.5", false},
		{"cidr allowed", "100.64.10.0/30", "100.64.10.3", true},
		{"cidr blocks outside", "100.64.10.0/30", "100.64.10.5", false},
		{"list matches second", "10.0.0.1,100.64.10.3", "100.64.10.3", true},
		// A source we cannot parse cannot be shown to be permitted, and this
		// gate guards someone's subscription.
		{"unparseable source fails closed", "100.64.10.3", "not-an-ip", false},
		{"unparseable source still open when unfiltered", "", "not-an-ip", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := NewRemotePool("http://box.example:31415", "", tc.allow)
			if pool == nil {
				t.Fatal("pool must be configured")
			}
			if got := pool.allows(tc.from); got != tc.want {
				t.Fatalf("allows(%q) with allow=%q = %v, want %v", tc.from, tc.allow, got, tc.want)
			}
		})
	}

	// A malformed entry must not silently widen the filter to everything.
	pool := NewRemotePool("http://box.example:31415", "", "garbage,100.64.10.3")
	if pool.allows("100.64.10.5") {
		t.Fatal("a malformed entry must not open the filter")
	}
	if !pool.allows("100.64.10.3") {
		t.Fatal("the valid entry must still apply")
	}
}

func TestRemotePoolEligibilityHonoursSourceFilter(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	server := Server{RemotePool: NewRemotePool(upstream.URL, "", "100.64.10.3")}

	allowed := forwardRequest(t, http.MethodPost, "/v1/messages", []byte(`{}`))
	allowed.RemoteAddr = "100.64.10.3:51000"
	if !server.remotePoolEligible(allowed) {
		t.Fatal("an allowed source must be eligible")
	}

	blocked := forwardRequest(t, http.MethodPost, "/v1/messages", []byte(`{}`))
	blocked.RemoteAddr = "100.64.10.5:51000"
	if server.remotePoolEligible(blocked) {
		t.Fatal("a source outside the allowlist must not spend the peer's accounts")
	}
}

// Without this, two daemons pointed at each other recurse forever.
func TestRemotePoolRefusesForwardedRequest(t *testing.T) {
	_, server := newTestRemotePool(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := forwardRequest(t, http.MethodPost, "/v1/messages", []byte(`{}`))
	req.Header.Set(remotePoolHopHeader, "1")
	if server.remotePoolEligible(req) {
		t.Fatal("a request that already arrived from a peer must not be forwarded again")
	}
}

func TestRemotePoolSetsOutboundHop(t *testing.T) {
	var hop string
	_, server := newTestRemotePool(t, func(w http.ResponseWriter, r *http.Request) {
		hop = r.Header.Get(remotePoolHopHeader)
		w.WriteHeader(http.StatusOK)
	})
	req := forwardRequest(t, http.MethodPost, "/v1/messages", []byte(`{}`))
	resp, ok := server.remotePoolForward(req, "claude", "s", "/v1/messages", "", []byte(`{}`), true)
	if !ok {
		t.Fatal("forward declined")
	}
	resp.Body.Close()
	if hop != "1" {
		t.Fatalf("outbound hop = %q, want 1", hop)
	}
}

// peerDownStrike is one failed peer request, sent at started and seen to fail
// at failedAt, while this machine's own upstream has answered all along: an
// outage the breaker attributes to the peer, as it did every outage before
// PATCHES §54. It reports whether the breaker opened, and an opening must say
// the peer is down.
func peerDownStrike(t *testing.T, breaker *remotePoolBreaker, started, failedAt time.Time) bool {
	t.Helper()
	var answering localEgress
	answering.noteAnswerOnANewConnection(started.Add(-time.Hour))
	answering.noteAnswerOnANewConnection(failedAt)
	opened, peerDown, _ := breaker.failure(started, failedAt, errPeerTimeout, &answering, 0)
	if opened != peerDown {
		t.Fatalf("opened=%v peerDown=%v with this machine's upstream answering throughout; an opening is the peer's", opened, peerDown)
	}
	return opened
}

// errPeerTimeout is a peer dial that timed out: not this machine's no-route.
var errPeerTimeout = &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}

// Two strikes, not one: an idle tailnet link times out its first packet while
// the direct path re-establishes, then answers in ~100ms. A one-strike breaker
// flaps on exactly that.
func TestRemotePoolBreakerOpensOnSecondFailureOnly(t *testing.T) {
	var breaker remotePoolBreaker
	now := time.Now()
	if !breaker.allow(now) {
		t.Fatal("a fresh breaker must allow")
	}
	// Sequential: each request is sent after the previous one failed.
	if opened := peerDownStrike(t, &breaker, now, now); opened {
		t.Fatal("one failure must not open the breaker")
	}
	if !breaker.allow(now) {
		t.Fatal("breaker must stay closed after a single failure")
	}
	if opened := peerDownStrike(t, &breaker, now, now); !opened {
		t.Fatal("second consecutive failure must open the breaker")
	}
	if breaker.allow(now) {
		t.Fatal("an open breaker must refuse")
	}
	if !breaker.allow(now.Add(remotePoolCooldown + time.Second)) {
		t.Fatal("breaker must reclose after the cooldown")
	}

	// A success between failures resets the count, so intermittent blips never
	// accumulate into an open breaker.
	var intermittent remotePoolBreaker
	peerDownStrike(t, &intermittent, now, now)
	intermittent.success(now)
	if opened := peerDownStrike(t, &intermittent, now, now); opened {
		t.Fatal("a success must reset the consecutive-failure count")
	}
}

// One blip is one strike however many requests it catches: a request sent
// before the last counted failure was in flight during it. A request sent
// after that failure is a new strike. The cooldown runs from when the failure
// was seen, not from when its request left (a silent peer fails a request
// remotePoolResponseHeaderTimeout after it left, which would leave no cooldown).
func TestRemotePoolBreakerCountsOneFailurePerBlip(t *testing.T) {
	var breaker remotePoolBreaker
	t0 := time.Now()
	failedAt := t0.Add(remotePoolResponseHeaderTimeout)

	if opened := peerDownStrike(t, &breaker, t0, failedAt); opened {
		t.Fatal("one failure must not open the breaker")
	}
	// Sent before failedAt, so in flight during that blip.
	for _, started := range []time.Time{t0, t0.Add(time.Millisecond), failedAt.Add(-time.Millisecond)} {
		if opened := peerDownStrike(t, &breaker, started, failedAt.Add(time.Millisecond)); opened {
			t.Fatalf("a request sent %v before the counted failure opened the breaker", failedAt.Sub(started))
		}
	}
	if !breaker.allow(failedAt.Add(time.Millisecond)) {
		t.Fatal("one blip that failed several in-flight requests opened the breaker")
	}

	// Sent after the counted failure; the peer is silent, so it too fails a
	// header timeout after it left.
	secondStarted := failedAt.Add(time.Second)
	secondAt := secondStarted.Add(remotePoolResponseHeaderTimeout)
	if opened := peerDownStrike(t, &breaker, secondStarted, secondAt); !opened {
		t.Fatal("a failure sent after the last counted failure must be the second strike")
	}
	if breaker.allow(secondAt.Add(remotePoolCooldown - time.Second)) {
		t.Fatal("the cooldown must run from when the failure was seen, not from when its request left")
	}
	if !breaker.allow(secondAt.Add(remotePoolCooldown + time.Second)) {
		t.Fatal("breaker must reclose after the cooldown")
	}

	// A request that was in flight when the breaker opened fails as part of
	// that same outage; it must not start the count for the next one.
	if opened := peerDownStrike(t, &breaker, secondStarted.Add(time.Second), secondAt.Add(time.Second)); opened {
		t.Fatal("a stale failure reopened the breaker")
	}
	afterCooldown := secondAt.Add(remotePoolCooldown + time.Second)
	if opened := peerDownStrike(t, &breaker, afterCooldown, afterCooldown); opened {
		t.Fatal("a stale failure counted toward the next opening")
	}

	// A success resets the count but not the blip's time: a request that was
	// in flight during a counted blip and fails only after another request
	// succeeded is still that blip, so it must not count. Otherwise blip,
	// success, late member of the blip, blip opens the breaker on two blips a
	// success apart -- exactly what the success reset exists to prevent.
	var afterSuccess remotePoolBreaker
	b0 := time.Now()
	blipAt := b0.Add(time.Second)
	if opened := peerDownStrike(t, &afterSuccess, b0, blipAt); opened {
		t.Fatal("one failure must not open the breaker")
	}
	afterSuccess.success(blipAt)
	// Sent before blipAt (in flight during that blip), seen to fail after the success.
	if opened := peerDownStrike(t, &afterSuccess, b0.Add(500*time.Millisecond), blipAt.Add(time.Second)); opened {
		t.Fatal("a stale failure after a success opened the breaker")
	}
	nextStarted := blipAt.Add(2 * time.Second)
	if opened := peerDownStrike(t, &afterSuccess, nextStarted, nextStarted.Add(time.Second)); opened {
		t.Fatal("a stale failure after a success counted: blip, success, blip opened the breaker")
	}
	// The new failure did count: one more new failure is the second strike.
	thirdStarted := nextStarted.Add(2 * time.Second)
	if opened := peerDownStrike(t, &afterSuccess, thirdStarted, thirdStarted.Add(time.Second)); !opened {
		t.Fatal("two new failures after a success must open the breaker")
	}
}

// An unreachable peer must cost a bounded wait, and the caller must still get
// its original local error rather than a hang.
func TestRemotePoolUnreachablePeerDeclinesQuickly(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // nothing is listening now

	server := Server{RemotePool: NewRemotePool(deadURL, "", "")}
	req := forwardRequest(t, http.MethodPost, "/v1/messages", []byte(`{}`))

	start := time.Now()
	_, ok := server.remotePoolForward(req, "claude", "s", "/v1/messages", "", []byte(`{}`), true)
	elapsed := time.Since(start)
	if ok {
		t.Fatal("a dead peer must decline")
	}
	if elapsed > remotePoolDialTimeout+2*time.Second {
		t.Fatalf("declining took %s; a dead peer must be bounded by the dial timeout", elapsed)
	}
}

// A peer that answers 429/503 has exhausted its own pool. Keeping the session
// pinned there would send the next turn straight back into a dry pool.
func TestRemotePoolDeclinesAndUnpinsOnPeerExhaustion(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		_, server := newTestRemotePool(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		})
		req := forwardRequest(t, http.MethodPost, "/v1/messages", []byte(`{}`))
		server.RemotePool.delegations.set("claude\x00s", time.Now(), remotePoolDelegationTTL, "", "")

		if _, ok := server.remotePoolForward(req, "claude", "s", "/v1/messages", "", []byte(`{}`), true); ok {
			t.Fatalf("status %d from the peer must decline", status)
		}
		if server.remotePoolDelegated("claude", "s") {
			t.Fatalf("status %d must clear the delegation so the next turn re-decides", status)
		}
		// Reachability is proven by the answer itself, so a quota reply must not
		// count toward opening the breaker.
		if !server.RemotePool.breaker.allow(time.Now()) {
			t.Fatalf("status %d must not open the unreachability breaker", status)
		}
	}
}

func TestRemotePoolDelegationIsStickyThenExpires(t *testing.T) {
	var d remotePoolDelegations
	now := time.Now()
	if d.active("k", now) {
		t.Fatal("unknown session must not be delegated")
	}
	d.set("k", now, remotePoolDelegationTTL, "", "")
	if !d.active("k", now.Add(time.Minute)) {
		t.Fatal("a delegated session must stay delegated within the TTL")
	}
	if d.active("k", now.Add(remotePoolDelegationTTL+time.Second)) {
		t.Fatal("delegation must expire so prefer-local resumes")
	}
	d.set("k", now, remotePoolDelegationTTL, "", "")
	d.clear("k")
	if d.active("k", now) {
		t.Fatal("cleared delegation must not be active")
	}
}

// replayablePostRetryTransport wraps usageLimitRetryTransport on the OUTSIDE, so
// one network blip replays the whole RoundTrip on the same transport value.
// Without this guard a single client request could burn several forwards.
func TestRemotePoolStageRunsAtMostOnce(t *testing.T) {
	calls := 0
	var mu sync.Mutex
	stage := onceRemotePoolStage(func(string) (*http.Response, bool) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, true
	})
	if _, ok := stage("max_attempts"); !ok {
		t.Fatal("first call must run")
	}
	if _, ok := stage("max_attempts"); ok {
		t.Fatal("second call must decline")
	}
	if calls != 1 {
		t.Fatalf("stage ran %d times, want 1", calls)
	}
}

func TestRemotePoolDisabledWhenUnset(t *testing.T) {
	server := Server{}
	req := forwardRequest(t, http.MethodPost, "/v1/messages", []byte(`{}`))
	if server.remotePoolEligible(req) {
		t.Fatal("an unconfigured pool must be inert")
	}
	if server.remotePoolDelegated("claude", "s") {
		t.Fatal("an unconfigured pool must report no delegation")
	}
	if _, ok := server.remotePoolStageResponse(req, "claude", "s", "/v1/messages", "", "max_attempts"); ok {
		t.Fatal("an unconfigured pool must decline the stage")
	}
	// This is what keeps every existing give-up-log test passing untouched.
	if w := httptest.NewRecorder(); server.serveRemotePoolFallthrough(w, req, "claude", "s") {
		t.Fatal("an unconfigured pool must decline the handler hook")
	}
}

// Broker/lease mode leases a short-lived credential to THIS machine; the peer
// cannot honour a lease it never issued.
func TestRemotePoolSkippedForLeaseAndBrokerModes(t *testing.T) {
	_, base := newTestRemotePool(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := forwardRequest(t, http.MethodPost, "/v1/messages", []byte(`{}`))
	if !base.remotePoolEligible(req) {
		t.Fatal("baseline must be eligible")
	}
	leased := base
	leased.RequireSessionLease = true
	if leased.remotePoolEligible(req) {
		t.Fatal("lease mode must not forward")
	}
}

// placeLocalAgainstOnePeer is the one cross-pool ranking (placeAcrossPools)
// over a single local account and, when peerOK, a single peer account. An
// unmeasured peer contributes no candidate at all (decisionCandidates skips
// it), which is how "never route on an unknown peer" reads now.
func placeLocalAgainstOnePeer(t *testing.T, local, peer selectacct.Score, peerOK bool) crossPoolPlacement {
	t.Helper()
	if local.Provider == "" {
		local.Provider = accounts.ProviderClaude
	}
	var peers []crossPoolCandidate
	if peerOK {
		peers = []crossPoolCandidate{{Peer: NewRemotePool("http://peer.invalid", "", ""), Account: "peer-acct", Score: peer, Reason: "eligible"}}
	}
	placement, err := placeAcrossPools(selectacct.NewScheduler([]selectacct.Score{local}),
		[]accounts.Account{{ID: local.AccountID, Provider: local.Provider, AuthMode: accounts.AuthModeOAuth}}, peers)
	if err != nil {
		t.Fatal(err)
	}
	return placement
}

// The spill rule, now the one cross-pool ranking. The primary comparison is
// weekly PACE at a 2x ratio -- local is the pool that still works when the
// tailnet does not, so it is held in reserve unless the peer is decisively the
// better place to spend.
func TestPeerDecisionRunsTheSharedComparator(t *testing.T) {
	now := time.Now()
	weekly := func(headroom float64, secondsToRoll int64) selectacct.Score {
		return selectacct.Score{
			Headroom: headroom, ShortHeadroom: headroom, Fresh: true,
			WeeklyHeadroom: headroom,
			WeeklyResetAt:  now.Add(time.Duration(secondsToRoll) * time.Second),
		}
	}
	// pace = weeklyHeadroom / secondsToRoll. Local is multiplied by
	// LocalPreferenceRatio for ranking, so the peer must beat that outright.
	localPace := func(s selectacct.Score) float64 {
		p, _ := s.WeeklyPace(now)
		return p * selectacct.LocalPreferenceRatio
	}

	for _, tc := range []struct {
		name       string
		local      selectacct.Score
		peer       selectacct.Score
		peerOK     bool
		wantPrefer bool
	}{
		// Equal pace: local's 2x advantage keeps it home.
		{"equal pace", weekly(0.50, 100000), weekly(0.50, 100000), true, false},
		// Peer twice as good on pace exactly ties local's advantage -- local
		// wins ties, which is what "preference" means.
		{"peer at exactly 2x", weekly(0.25, 100000), weekly(0.50, 100000), true, false},
		// Decisively better: crosses the preference and takes the traffic.
		{"peer at 4x", weekly(0.20, 100000), weekly(0.80, 100000), true, true},
		// A spent local pool has ~zero pace, so any live peer wins without a
		// separate below-floor rule.
		{"local spent", weekly(0.0, 100000), weekly(0.60, 100000), true, true},
		// Both dead: do not hand a dead peer the request.
		{"both spent", weekly(0.0, 100000), weekly(0.0, 100000), true, false},
		// An unmeasured peer is unknown, not empty. Never route on it.
		{"peer unmeasured", weekly(0.05, 100000), selectacct.Score{}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := tc.local
			local.AccountID = "local-acct"
			got := placeLocalAgainstOnePeer(t, local, tc.peer, tc.peerOK)
			if (got.Peer != nil) != tc.wantPrefer {
				t.Fatalf("peer won=%v want %v (clause %q; local adj pace %.3e, peer pace %.3e)",
					got.Peer != nil, tc.wantPrefer, got.Clause, localPace(tc.local), func() float64 {
						p, _ := tc.peer.WeeklyPace(now)
						return p
					}())
			}
		})
	}
}
func TestProbeCapacityCountsNonActiveAccounts(t *testing.T) {
	body := `[
	  {"id":"serving","provider":"claude","active":true,
	   "normalized":{"headroom":0.56,"short_headroom":0.89,"fresh":true,
	                 "weekly_headroom":0.46,"seconds_to_weekly_reset":279274}},
	  {"id":"idle-but-rich","provider":"claude",
	   "normalized":{"headroom":0.75,"short_headroom":1,"fresh":true,
	                 "weekly_headroom":0.485,"seconds_to_weekly_reset":549274}},
	  {"id":"near-roll","provider":"claude",
	   "normalized":{"headroom":0.09,"short_headroom":0.77,"fresh":true,
	                 "weekly_headroom":0.09,"seconds_to_weekly_reset":45275}},
	  {"id":"revoked","provider":"claude","auth_valid":false,
	   "normalized":{"headroom":1,"short_headroom":1,"fresh":true,
	                 "weekly_headroom":1,"seconds_to_weekly_reset":1000}}
	]`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != remotePoolUsagePath {
			t.Errorf("probed %q, want %q", r.URL.Path, remotePoolUsagePath)
		}
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()

	pool := NewRemotePool(server.URL, "", "")
	got, ok := pool.BestCapacity("claude")
	if !ok {
		t.Fatal("peer reported unmeasured despite a 200 with usable rows")
	}
	if got.Headroom != 0.75 {
		t.Fatalf("best headroom = %v, want 0.75 (the idle account, not the serving one)", got.Headroom)
	}
	// near-roll: 0.09/45275 beats idle-but-rich 0.485/549274.
	wantPace := 0.09 / 45275.0
	if !got.PaceOK || got.Pace != wantPace {
		t.Fatalf("best pace = %v (ok=%v), want %v from the near-roll account", got.Pace, got.PaceOK, wantPace)
	}
}

// The observability bug this fixes, stated as a test: a delegated session's
// local assignment names the account selection picked BEFORE the request left,
// which is not the account that answered it. Reporting that placeholder as fact
// is how `srq serving` claimed a session was on twelve-alpha while every one
// of its requests was being served by sixteen-beta on the peer.
func TestDelegationRecordsThePeerAccountNotTheLocalPlaceholder(t *testing.T) {
	pool := NewRemotePool("http://box.example:31415", "", "")
	now := time.Now()
	key := "claude\x00sess-1"

	pool.delegations.set(key, now, remotePoolDelegationTTL, "sixteen-beta", "box")
	got, ok := pool.DelegatedTo(key)
	if !ok {
		t.Fatal("delegation not recorded")
	}
	if got.Account != "sixteen-beta" {
		t.Fatalf("served account = %q, want the PEER's account", got.Account)
	}

	// A peer too old to name its account must not downgrade what we already
	// knew: unknown is honest, but stale-precise beats freshly-vague.
	pool.delegations.set(key, now, remotePoolDelegationTTL, "", "box")
	if got, _ := pool.DelegatedTo(key); got.Account != "sixteen-beta" {
		t.Fatalf("a nameless refresh erased the known account: %q", got.Account)
	}

	// An expired delegation reports nothing rather than a stale location.
	pool.delegations.set(key, now.Add(-2*remotePoolDelegationTTL), remotePoolDelegationTTL, "sixteen-beta", "box")
	if _, ok := pool.DelegatedTo(key); ok {
		t.Fatal("expired delegation still reported as serving")
	}
}
