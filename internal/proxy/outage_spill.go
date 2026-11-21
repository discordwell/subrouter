package proxy

import (
	"math"
	"net/http"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/session"
)

// The outage spill (PATCHES §59).
//
// A turn whose LOCAL upstream cannot be reached -- the lookup, the dial or the
// TLS handshake failed, so the request never left -- is served by a peer for
// this one turn, the way a token refresh that reached no verdict already is
// (serveAcrossPeers with the one-turn hold mode): the session keeps its local
// account, no peer hold is set, and its next turn tries local first again.
//
// On 2026-10-20 the laptop could not reach api.anthropic.com for about three
// hours while the peer, dialed by IP, kept answering. A failed dial used to be
// answered 502 and, since §58, held and retried on the same local account,
// which could not succeed until the network came back.
//
// The spill takes a token from a bucket first, so an outage on a busy machine
// cannot put more than the bucket's rate on the peer. With no token the turn
// is held (§58) exactly as before, and every held attempt that fails locally
// asks for a token again.

const (
	// DefaultOutageSpillRate is the sustained spill rate, per second: under
	// the box's 20 requests a second per token (publicRatePerSecond), and
	// about three times the laptop's average of 1.8 turns a second
	// (2026-10-21).
	DefaultOutageSpillRate = 5.0
	// DefaultOutageSpillBurst lets the turns in flight when the network
	// drops go at once.
	DefaultOutageSpillBurst = 10
)

// OutageSpillPolicy is serve's spill configuration and its bucket. Nil, or a
// non-positive rate, turns the spill off: a failed local dial is answered as
// before §59.
type OutageSpillPolicy struct {
	Rate   float64
	Burst  int
	bucket *rateLimiter
}

// NewOutageSpillPolicy returns the policy serve runs with, or nil (the spill
// off) for a rate that is not a positive finite number. A burst below one is
// one.
func NewOutageSpillPolicy(rate float64, burst int) *OutageSpillPolicy {
	// NaN passes a <= 0 test, and a NaN or infinite rate fills a bucket that
	// never refuses: no cap at all.
	if !(rate > 0) || math.IsInf(rate, 0) {
		return nil
	}
	if burst < 1 {
		burst = 1
	}
	return &OutageSpillPolicy{
		Rate:  rate,
		Burst: burst,
		// The public listener's token bucket, enforcing, with one key.
		bucket: &rateLimiter{
			mode:    RateLimitEnforce,
			rate:    rate,
			burst:   float64(burst),
			now:     time.Now,
			buckets: map[string]*rateBucket{},
		},
	}
}

func (p *OutageSpillPolicy) enabled() bool {
	return p != nil && p.Rate > 0 && p.bucket != nil
}

func (p *OutageSpillPolicy) take() bool {
	return p.bucket.allow("outage-spill")
}

// spillUnreachableTurn serves a turn whose local upstream could not be reached
// from a peer, for this turn only. r is the handler's request; its body went
// to the failed attempt with proxyRequest, so the peer is sent the client's
// bytes from proxyRequest.GetBody. It reports whether the client was answered;
// otherwise nothing was written, and the caller answers as before.
func (s Server) spillUnreachableTurn(w http.ResponseWriter, r, proxyRequest *http.Request, provider accounts.Provider, poolModel, agentType, sessionID string, mode remotePoolHoldMode, turn *remotePoolTurn, accountID, cause string) bool {
	policy := s.OutageSpill
	if !policy.enabled() || turnStaysLocal(turn) || !s.peerForwardingAllowed(r) || !restoreSpillBody(r, proxyRequest) {
		return false
	}
	reason := "upstream_unreachable:" + cause
	fields := []any{"reason", reason, "agent", agentType, "session", sessionID, "account", accountID,
		"method", r.Method, "path", r.URL.Path, "remote_addr", clientRemoteIP(r)}
	if !policy.take() {
		s.logOutageSpill(append(fields, "outcome", "capped", "rate", policy.Rate, "burst", policy.Burst)...)
		return false
	}
	served := s.serveAcrossPeers(w, r, provider, poolModel, agentType, sessionID, mode, turn,
		crossPoolLocal{Account: accountID, Cause: reason})
	outcome := "served"
	if !served {
		outcome = "not_served"
	}
	s.logOutageSpill(append(fields, "outcome", outcome)...)
	return served
}

// waitsForItsUpstream is a model turn of a chat warm on this account (warm:
// selection kept it here) whose upstream could not be dialed. It waits for the
// upstream, under the hold and across the client's retries, and never goes to
// a box: the boxes need the same network, and a chat moved there for a turn
// would rewrite its cache twice (PATCHES §67, D4). Warmth is judged when the
// session's failures begin (HoldPolicy.localWarm), and a refugee placed on
// this account is warm here from its placement (placedHere). A session a peer
// holds has its cache on the peer (its local pin is a placeholder), so it is
// not warm here. A side request neither waits nor begins the session's run,
// and a turn no hold carries (holding off) is spilled as before.
func (s Server) waitsForItsUpstream(r *http.Request, agentType, sessionID string, mode remotePoolHoldMode, warm bool) bool {
	if mode == remotePoolHoldReadOnly || !heldByAHold(r) {
		return false
	}
	key := session.ScopedSessionKey(agentType, sessionID)
	if warm {
		_, _, peerHeld := s.peerHold(key)
		warm = !peerHeld
	}
	return s.Hold.localWarm(key, warm)
}

// turnStaysLocal is a held turn whose peer attempt failed transiently
// (serveHeldTurn): that peer may have the request, so no peer is asked again
// this turn, as the cross-pool ranking already observes.
func turnStaysLocal(turn *remotePoolTurn) bool {
	if turn == nil {
		return false
	}
	turn.mu.Lock()
	defer turn.mu.Unlock()
	return turn.stayLocal
}

func (s Server) logOutageSpill(fields ...any) {
	if s.Logger != nil {
		s.Logger.Warn("outage spill", fields...)
	}
}

// restoreSpillBody gives r a fresh reader over the client's body. A body the
// handler could not make replayable is not sent anywhere else.
func restoreSpillBody(r, proxyRequest *http.Request) bool {
	if proxyRequest != nil && proxyRequest.GetBody != nil {
		body, err := proxyRequest.GetBody()
		if err != nil {
			return false
		}
		r.Body = body
		return true
	}
	return r.Body == nil || r.Body == http.NoBody
}
