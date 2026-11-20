package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// peerBodies serves whichever usage-status body the test has set, so one pool
// can watch its peer go from measured to unmeasured and back.
type peerBodies struct {
	mu     sync.Mutex
	body   string
	status int // 0 means 200
}

func (p *peerBodies) set(body string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.body = body
	p.status = 0
}

func (p *peerBodies) fail(status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status = status
}

func (p *peerBodies) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != remotePoolUsagePath {
			t.Errorf("probed %q, want %q", r.URL.Path, remotePoolUsagePath)
		}
		p.mu.Lock()
		body, status := p.body, p.status
		p.mu.Unlock()
		if status != 0 {
			http.Error(w, "peer down", status)
			return
		}
		_, _ = io.WriteString(w, body)
	}
}

// A peer that measured everything.
const measuredPeer = `[
  {"id":"cdx","provider":"codex","auth_checked":true,"auth_valid":true,
   "normalized":{"headroom":0.9,"short_headroom":1,"fresh":true}},
  {"id":"good","provider":"claude","auth_checked":true,"auth_valid":true,
   "normalized":{"headroom":0.75,"short_headroom":1,"fresh":true,
                 "weekly_headroom":0.5,"seconds_to_weekly_reset":100000}}
]`

// A current peer whose sweep was cut by its own clock, and says so.
const cutPeerCurrent = `[
  {"id":"cdx","provider":"codex","auth_checked":true,"auth_valid":true,
   "normalized":{"headroom":0.9,"short_headroom":1,"fresh":true}},
  {"id":"good","provider":"claude","auth_checked":false,"auth_valid":false,
   "error":"Post \"https://console.anthropic.com/v1/oauth/token\": context deadline exceeded",
   "normalized":{"headroom":0.75,"short_headroom":1,"fresh":false}}
]`

// An older peer: the cut is published as a verdict and only the error text
// tells. This is the exact row that emptied a healthy Claude pool.
const cutPeerLegacy = `[
  {"id":"cdx","provider":"codex","auth_checked":true,"auth_valid":true,
   "normalized":{"headroom":0.9,"short_headroom":1,"fresh":true}},
  {"id":"good","provider":"claude","auth_checked":true,"auth_valid":false,"error":"context canceled",
   "normalized":{"headroom":0.75,"short_headroom":1,"fresh":false}}
]`

// A real verdict: the peer looked, and the credential is dead.
const revokedPeer = `[
  {"id":"cdx","provider":"codex","auth_checked":true,"auth_valid":true,
   "normalized":{"headroom":0.9,"short_headroom":1,"fresh":true}},
  {"id":"good","provider":"claude","auth_checked":true,"auth_valid":false,
   "error":"Claude OAuth refresh failed: 400 Bad Request: {\"error\":\"invalid_grant\"}",
   "normalized":{"headroom":0.75,"short_headroom":1,"fresh":false}}
]`

func expireAdminCache(pool *RemotePool) {
	pool.admin.mu.Lock()
	defer pool.admin.mu.Unlock()
	for path, entry := range pool.admin.entries {
		entry.at = time.Time{}
		pool.admin.entries[path] = entry
	}
}

func expireCapacity(pool *RemotePool) {
	pool.capacity.mu.Lock()
	defer pool.capacity.mu.Unlock()
	pool.capacity.at = time.Time{}
}

// The request-path reader. A peer that did not get to look is not a peer with
// nothing to offer: the last reading it DID reach stands in, bounded by
// remotePoolCarryTTL. A real verdict is never carried over.
func TestBestPeerScoreCarriesTheLastReadingWhileThePeerIsUnmeasured(t *testing.T) {
	bodies := &peerBodies{body: measuredPeer}
	server := httptest.NewServer(bodies.handler(t))
	defer server.Close()
	pool := NewRemotePool(server.URL, "", "")
	ctx := context.Background()

	score, ok := pool.BestPeerScore(ctx, "claude")
	if !ok || score.AccountID != "good" {
		t.Fatalf("measured peer: ok=%v id=%q, want the good account", ok, score.AccountID)
	}

	for _, cut := range []struct{ name, body string }{
		{"current peer clears auth_checked", cutPeerCurrent},
		{"older peer publishes the cut as auth_valid:false", cutPeerLegacy},
	} {
		bodies.set(cut.body)
		expireAdminCache(pool)
		got, ok := pool.BestPeerScore(ctx, "claude")
		if !ok || got.AccountID != "good" {
			t.Fatalf("%s: an unmeasured peer read as EMPTY (ok=%v id=%q); the last measured reading should stand in",
				cut.name, ok, got.AccountID)
		}
	}

	// A verdict is not a cut. Revoked stays revoked and nothing is carried over it.
	bodies.set(revokedPeer)
	expireAdminCache(pool)
	if _, ok := pool.BestPeerScore(ctx, "claude"); ok {
		t.Fatal("a revoked pool was carried as measured")
	}

	// And a carry ages out on the same horizon the peer ages its own last-good.
	bodies.set(cutPeerCurrent)
	expireAdminCache(pool)
	pool.peerScores.mu.Lock()
	entry := pool.peerScores.entries["claude"]
	entry.at = time.Now().Add(-remotePoolCarryTTL - time.Minute)
	pool.peerScores.entries["claude"] = entry
	pool.peerScores.mu.Unlock()
	if _, ok := pool.BestPeerScore(ctx, "claude"); ok {
		t.Fatal("a carry older than remotePoolCarryTTL was still served")
	}
}

// The spill reader. Same rule, and the measured half of the same body must
// still be read fresh: a cut Claude phase says nothing about Codex.
func TestBestCapacityCarriesTheLastReadingWhileThePeerIsUnmeasured(t *testing.T) {
	bodies := &peerBodies{body: measuredPeer}
	server := httptest.NewServer(bodies.handler(t))
	defer server.Close()
	pool := NewRemotePool(server.URL, "", "")

	first, ok := pool.BestCapacity("claude")
	if !ok || first.Headroom != 0.75 {
		t.Fatalf("measured peer: ok=%v headroom=%v, want 0.75", ok, first.Headroom)
	}

	bodies.set(cutPeerCurrent)
	expireCapacity(pool)
	carried, ok := pool.BestCapacity("claude")
	if !ok || carried.Headroom != 0.75 {
		t.Fatalf("an unmeasured claude pool read as EMPTY (ok=%v headroom=%v); the last reading should stand in",
			ok, carried.Headroom)
	}
	if !carried.MeasuredAt.Equal(first.MeasuredAt) {
		t.Fatalf("a carried entry must keep its ORIGINAL stamp: %v became %v", first.MeasuredAt, carried.MeasuredAt)
	}
	codex, ok := pool.BestCapacity("codex")
	if !ok || codex.Headroom != 0.9 {
		t.Fatalf("the measured codex half of the same body was not read: ok=%v headroom=%v", ok, codex.Headroom)
	}
	if codex.MeasuredAt.Before(carried.MeasuredAt) {
		t.Fatal("the fresh codex reading carries an older stamp than the carried claude one")
	}

	// A carry older than remotePoolCarryTTL is dropped, not served.
	pool.capacity.mu.Lock()
	stale := pool.capacity.best["claude"]
	stale.MeasuredAt = time.Now().Add(-remotePoolCarryTTL - time.Minute)
	pool.capacity.best["claude"] = stale
	pool.capacity.mu.Unlock()
	expireCapacity(pool)
	if _, ok := pool.BestCapacity("claude"); ok {
		t.Fatal("a carry older than remotePoolCarryTTL was still served")
	}

	// A verdict is not a cut: nothing is carried over a revoked pool.
	bodies.set(measuredPeer)
	expireCapacity(pool)
	if _, ok := pool.BestCapacity("claude"); !ok {
		t.Fatal("re-measured peer did not read")
	}
	bodies.set(revokedPeer)
	expireCapacity(pool)
	if _, ok := pool.BestCapacity("claude"); ok {
		t.Fatal("a revoked pool was carried as measured")
	}
}

// Only an OAuth row can be unmeasured. An API-key row is never checked, so its
// auth_checked:false is the ordinary state of such a row, not a cut sweep --
// reading it as one carried a measured-dead pool as healthy.
func TestUnmeasuredIsAnOAuthVerdictOnly(t *testing.T) {
	f, tr := false, true
	cases := []struct {
		name string
		row  remotePoolAccountUsage
		want bool
	}{
		{"api-key row, unchecked", remotePoolAccountUsage{AuthMode: "apikey", AuthChecked: &f, AuthValid: &f}, false},
		{"oauth row, unchecked", remotePoolAccountUsage{AuthMode: "oauth", AuthChecked: &f, AuthValid: &f}, true},
		{"older peer, no auth_mode, unchecked", remotePoolAccountUsage{AuthChecked: &f, AuthValid: &f}, true},
		{"oauth row, checked, context error", remotePoolAccountUsage{AuthMode: "oauth", AuthChecked: &tr, AuthValid: &f, Error: "context canceled"}, true},
		{"oauth row, checked, real verdict", remotePoolAccountUsage{AuthMode: "oauth", AuthChecked: &tr, AuthValid: &f, Error: "invalid_grant"}, false},
		{"oauth row, backfilled over a cut", remotePoolAccountUsage{AuthMode: "oauth", AuthChecked: &tr, AuthValid: &tr, SweepCut: true}, true},
		{"api-key row, sweep_cut set anyway", remotePoolAccountUsage{AuthMode: "apikey", SweepCut: true}, false},
	}
	for _, c := range cases {
		if got := c.row.unmeasured(); got != c.want {
			t.Errorf("%s: unmeasured=%v, want %v", c.name, got, c.want)
		}
	}
}

const apiKeyBesideRevokedPeer = `[
  {"id":"key","provider":"claude","auth_mode":"apikey","auth_checked":false,"auth_valid":false},
  {"id":"good","provider":"claude","auth_mode":"oauth","auth_checked":true,"auth_valid":false,
   "error":"Claude OAuth refresh failed: 400 Bad Request: {\"error\":\"invalid_grant\"}",
   "normalized":{"headroom":0.75,"short_headroom":1,"fresh":false}}
]`

func TestAnAPIKeyRowDoesNotMakeARevokedPoolLookUnmeasured(t *testing.T) {
	bodies := &peerBodies{body: measuredPeer}
	server := httptest.NewServer(bodies.handler(t))
	defer server.Close()
	pool := NewRemotePool(server.URL, "", "")
	ctx := context.Background()
	if _, ok := pool.BestPeerScore(ctx, "claude"); !ok {
		t.Fatal("measured peer did not read")
	}
	bodies.set(apiKeyBesideRevokedPeer)
	expireAdminCache(pool)
	if _, ok := pool.BestPeerScore(ctx, "claude"); ok {
		t.Fatal("a revoked OAuth pool was carried because an API-key row beside it reads auth_checked:false")
	}
}

// The carry is a stand-in for the account the peer did not get to look at.
// A body that DID look at that account and found it dead evicts the carry,
// whatever the other rows say.
const carriedAccountRevokedPeer = `[
  {"id":"good","provider":"claude","auth_mode":"oauth","auth_checked":true,"auth_valid":false,
   "error":"Claude OAuth refresh failed: 400 Bad Request: {\"error\":\"invalid_grant\"}",
   "normalized":{"headroom":0.75,"short_headroom":1,"fresh":false}},
  {"id":"other","provider":"claude","auth_mode":"oauth","auth_checked":false,"auth_valid":false,
   "error":"context deadline exceeded"}
]`

func TestAVerdictAgainstTheCarriedAccountEvictsTheCarry(t *testing.T) {
	bodies := &peerBodies{body: measuredPeer}
	server := httptest.NewServer(bodies.handler(t))
	defer server.Close()
	pool := NewRemotePool(server.URL, "", "")
	ctx := context.Background()
	if score, ok := pool.BestPeerScore(ctx, "claude"); !ok || score.AccountID != "good" {
		t.Fatalf("measured peer: ok=%v id=%q", ok, score.AccountID)
	}
	bodies.set(carriedAccountRevokedPeer)
	expireAdminCache(pool)
	if score, ok := pool.BestPeerScore(ctx, "claude"); ok {
		t.Fatalf("the revoked account %q was carried as the peer's best", score.AccountID)
	}
}

// AdminGet keeps its last good body across peer failures, and a score derived
// from that body is as old as the body. The carry must be stamped with the
// body's measured time, or a peer that was down for an hour comes back cut
// and the hour-old reading is served as if measured a moment ago.
func TestCarryIsStampedWithTheBodysMeasuredTimeNotNow(t *testing.T) {
	bodies := &peerBodies{body: measuredPeer}
	server := httptest.NewServer(bodies.handler(t))
	defer server.Close()
	pool := NewRemotePool(server.URL, "", "")
	ctx := context.Background()
	if _, ok := pool.BestPeerScore(ctx, "claude"); !ok {
		t.Fatal("measured peer did not read")
	}

	// The body AdminGet holds was measured long ago, and the peer is down, so
	// every read from here serves that old body with its original stamp.
	pool.admin.mu.Lock()
	entry := pool.admin.entries[remotePoolUsagePath]
	entry.at = time.Now().Add(-remotePoolCarryTTL - time.Minute)
	pool.admin.entries[remotePoolUsagePath] = entry
	pool.admin.mu.Unlock()
	bodies.fail(http.StatusInternalServerError)
	if _, ok := pool.BestPeerScore(ctx, "claude"); !ok {
		t.Fatal("stale-but-real body did not score (pre-existing AdminGet retention)")
	}

	// The peer returns, cut. The only reading on hand is the old one.
	bodies.set(cutPeerCurrent)
	expireAdminCache(pool)
	if score, ok := pool.BestPeerScore(ctx, "claude"); ok {
		t.Fatalf("an hour-old reading (%q) was carried as if measured now", score.AccountID)
	}
}

// The decision line must be able to say a peer score is a carry.
func TestBestPeerScoreReportsWhetherItIsCarried(t *testing.T) {
	bodies := &peerBodies{body: measuredPeer}
	server := httptest.NewServer(bodies.handler(t))
	defer server.Close()
	pool := NewRemotePool(server.URL, "", "")
	ctx := context.Background()
	if _, carried, ok := pool.bestPeerScore(ctx, "claude"); !ok || carried {
		t.Fatalf("measured peer: ok=%v carried=%v, want measured", ok, carried)
	}
	bodies.set(cutPeerCurrent)
	expireAdminCache(pool)
	if _, carried, ok := pool.bestPeerScore(ctx, "claude"); !ok || !carried {
		t.Fatalf("cut peer: ok=%v carried=%v, want a visible carry", ok, carried)
	}
	candidates, _, _ := pool.decisionCandidates(pool.cachedUsage(), "claude", "", nil, time.Now())
	if len(candidates) != 1 || !candidates[0].Carried {
		t.Fatalf("the ranking candidate did not mark the carry: %+v", candidates)
	}
}

// A current peer that backfilled a cut row publishes last-good windows,
// auth_valid:true and fresh:false -- the exact shape of a restarted peer's
// stale seed, which must stay unroutable. sweep_cut is what tells them apart,
// and the carry engages on it.
const backfilledCutPeer = `[
  {"id":"cdx","provider":"codex","auth_mode":"oauth","auth_checked":true,"auth_valid":true,
   "normalized":{"headroom":0.9,"short_headroom":1,"fresh":true}},
  {"id":"good","provider":"claude","auth_mode":"oauth","auth_checked":false,"auth_valid":true,"sweep_cut":true,
   "windows":[{"Name":"7d","UsedPercent":25,"ResetAfterSeconds":100000}],
   "normalized":{"headroom":0.75,"short_headroom":1,"fresh":false}}
]`

const restartedPeer = `[
  {"id":"good","provider":"claude","auth_mode":"oauth","auth_checked":true,"auth_valid":true,
   "normalized":{"headroom":1,"short_headroom":1,"fresh":false}}
]`

func TestCarryEngagesThroughTheBackfillButNotForARestartedPeer(t *testing.T) {
	bodies := &peerBodies{body: measuredPeer}
	server := httptest.NewServer(bodies.handler(t))
	defer server.Close()
	pool := NewRemotePool(server.URL, "", "")
	ctx := context.Background()
	if score, ok := pool.BestPeerScore(ctx, "claude"); !ok || score.AccountID != "good" {
		t.Fatalf("measured peer: ok=%v id=%q", ok, score.AccountID)
	}

	bodies.set(backfilledCutPeer)
	expireAdminCache(pool)
	got, carried, ok := pool.bestPeerScore(ctx, "claude")
	if !ok || !carried || got.AccountID != "good" {
		t.Fatalf("a backfilled cut row did not engage the carry: ok=%v carried=%v id=%q", ok, carried, got.AccountID)
	}

	// Same stale shape without the tell: a restarted peer's optimistic seed.
	// Unroutable, and NOT a reason to carry either.
	bodies.set(restartedPeer)
	expireAdminCache(pool)
	if _, ok := pool.BestPeerScore(ctx, "claude"); ok {
		t.Fatal("a restarted peer's stale seed was routed to or carried")
	}
}
