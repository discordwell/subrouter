package proxy

import (
	"errors"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"syscall"
	"time"
)

// A peer this machine cannot reach and a machine that cannot reach anything
// look the same from the peer forward: two failed round trips. The breaker
// used to read both as the peer being down and end every hold on it, so a
// reboot with the network still coming up, a Wi-Fi drop or a VPN flip sent
// every box-held session back to its local placeholder for good (PATCHES §54).
//
// So an opening now ends the holds only when the outage is ATTRIBUTABLE to the
// peer (whyNotThePeers, and remotePoolBreaker.failure for the last rule):
//   - the opening strike's error is not this machine's own no-route error;
//   - upSince is set and not later than the moment the opening strike's
//     request was sent. upSince is the first local answer after the last
//     local failure: a local transport failure, a counted no-route strike or
//     an opening kept for no_answer;
//   - the last local answer is not earlier than the series start: the moment
//     the first counted strike of this outage was sent. The egress is read
//     when the opening strike's failure is seen, so an answer during its
//     flight counts;
//   - when the opening strike never reached the peer (the lookup, the dial or
//     the TLS handshake failed): a new connection to anywhere completed at or
//     after the series start. An answer over a connection opened before the
//     outage does not show this machine can open one now;
//   - once a local failure has been seen since the process started, or after
//     an opening that kept the holds until a peer success or an opening that
//     ends them: the strike before the opening one was also sent at or after
//     upSince. Two strikes sent since this machine came back, not one.
//
// Any other opening still opens the breaker, so the cost is bounded exactly as
// before, but keeps every hold and leaves peer-holds.json as it was. A counted
// no-route strike and an opening kept for no_answer note a local failure
// (failure), so the next local answer, not an older one, is when this machine
// came back.
//
// The evidence is the local upstream round trip itself, observed on
// Server.transport() (localEgressObserver) and on the local Codex websocket
// dial (proxyWebSocket), and every new connection those and the peer forwards
// complete (noteNewConnections). There is no probe.

// localEgress is this machine's own upstream evidence, as unix nanoseconds. One
// per peer, so the hot path takes no lock and nothing in main.go has to wire
// it: the observer writes every configured peer's copy. A peer's breaker
// notes a failure on its own copy only.
type localEgress struct {
	// answerAt is the last local upstream round trip that returned response
	// headers, whatever the status.
	answerAt atomic.Int64
	// failureAt is the last local upstream round trip that failed at the
	// transport while its client was still there, the last counted no-route
	// peer strike, or the last breaker opening that kept the holds for
	// no_answer: each time this machine may have been offline.
	failureAt atomic.Int64
	// upSince is the first answer after the last failure; 0 when a failure
	// came after it, or nothing has answered since the process started. It
	// counts only while it is later than failureAt (see view).
	upSince atomic.Int64
	// connectedAt is the last new connection this daemon completed, its dial
	// and TLS handshake done, to the local upstream or to any peer. A request
	// on a pooled connection does not set it. A gateway can refuse new
	// connections while open ones keep answering.
	connectedAt atomic.Int64
}

func (e *localEgress) noteConnected(at time.Time) {
	storeLater(&e.connectedAt, at.UnixNano())
}

func (e *localEgress) noteAnswer(at time.Time) {
	ns := at.UnixNano()
	storeLater(&e.answerAt, ns)
	for {
		up := e.upSince.Load()
		if up != 0 && up > e.failureAt.Load() {
			return // already up since an answer after the last failure
		}
		if e.upSince.CompareAndSwap(up, ns) {
			return
		}
	}
}

func (e *localEgress) noteFailure(at time.Time) {
	storeLater(&e.failureAt, at.UnixNano())
	e.upSince.Store(0)
}

// storeLater keeps the later of v and ns: two round trips that end together
// may note out of order.
func storeLater(v *atomic.Int64, ns int64) {
	for {
		old := v.Load()
		if ns <= old || v.CompareAndSwap(old, ns) {
			return
		}
	}
}

// egressView is one read of localEgress. A zero time means "never".
type egressView struct {
	lastAnswer    time.Time
	upSince       time.Time
	lastFailure   time.Time
	lastConnected time.Time
}

func (e *localEgress) view() egressView {
	answer, failure, up := e.answerAt.Load(), e.failureAt.Load(), e.upSince.Load()
	var v egressView
	if answer != 0 {
		v.lastAnswer = time.Unix(0, answer)
	}
	if connected := e.connectedAt.Load(); connected != 0 {
		v.lastConnected = time.Unix(0, connected)
	}
	if failure != 0 {
		v.lastFailure = time.Unix(0, failure)
	}
	// An answer that raced a later failure may have set upSince after the
	// failure reset it; it is not evidence that the egress is up.
	if up != 0 && up > failure {
		v.upSince = time.Unix(0, up)
	}
	return v
}

// Why an opening kept the holds: the holds-kept line's why= (PATCHES §54).
// within_grace: the outage is the peer's, but it has been silent for less
// than the grace (PATCHES §67).
const (
	keptNoRoute              = "no_route"
	keptNoAnswer             = "no_answer"
	keptNetworkBackMidStrike = "network_back_mid_strike"
	keptOneStrikeSinceBack   = "one_strike_since_back"
	keptNoNewConnection      = "no_new_connection"
	keptWithinGrace          = "within_grace"
)

// whyNotThePeers is the rule for one opening (PATCHES §54): err is the opening
// strike's error, started when its request was sent, series when the first
// counted strike of the outage was sent. It returns "" when the outage is the
// peer's, and otherwise why not.
func whyNotThePeers(err error, egress egressView, started, series time.Time) string {
	if noRoute(err) {
		return keptNoRoute
	}
	// Nothing local has answered since the last local failure.
	if egress.upSince.IsZero() {
		return keptNoAnswer
	}
	// This machine came back while the opening request was in flight: it
	// left with no network, so its failure is this machine's.
	if egress.upSince.After(started) {
		return keptNetworkBackMidStrike
	}
	// The last local answer predates the outage: an idle laptop's answer from
	// before a network drop says nothing about now.
	if egress.lastAnswer.Before(series) {
		return keptNoAnswer
	}
	// The opening strike could not open a connection to the peer, and none
	// has been opened to anywhere since the outage began: this machine may be
	// what cannot open one.
	if _, neverSent, _ := transientNetworkCause(err); neverSent && egress.lastConnected.Before(series) {
		return keptNoNewConnection
	}
	return ""
}

// noRoute is this machine's own kernel saying it has no route or no address:
// the dial never left. 7 of the laptop's 14 breaker openings in the eight days
// to 2026-10-09 were this (`connect: network is unreachable`). Leaving the
// tailnet is this machine dropping a route too.
func noRoute(err error) bool {
	return errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.ENETDOWN) || errors.Is(err, syscall.EADDRNOTAVAIL) ||
		errors.Is(err, errLeftTailnet)
}

// noteLocalUpstream records one local upstream round trip, seen to end at at,
// on every peer's localEgress. answered: response headers came back, whatever
// the status (a 529 is the provider answering, which is all this needs).
// Otherwise it failed at the transport, which is a failure unless the client
// caused it: a client cancel says nothing about the network.
func noteLocalUpstream(peers []*RemotePool, answered, clientGone bool, at time.Time) {
	switch {
	case answered:
		for _, p := range peers {
			p.egress.noteAnswer(at)
		}
	case !clientGone:
		for _, p := range peers {
			p.egress.noteFailure(at)
		}
	}
}

// localEgressObserver notes every local upstream round trip on each peer's
// localEgress. It changes no request and no response. The local Codex
// websocket dial does not go through it, and notes itself (proxyWebSocket).
type localEgressObserver struct {
	base  http.RoundTripper
	peers []*RemotePool
}

func (o localEgressObserver) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := o.base.RoundTrip(noteNewConnections(req, o.peers))
	noteLocalUpstream(o.peers, err == nil, req.Context().Err() != nil, time.Now())
	return resp, err
}

// noteNewConnections returns req with a trace that notes, on every peer's
// localEgress, each new connection the transport completes for it.
func noteNewConnections(req *http.Request, peers []*RemotePool) *http.Request {
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		if !info.Reused {
			noteConnected(peers, time.Now())
		}
	}}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
}

func noteConnected(peers []*RemotePool, at time.Time) {
	for _, p := range peers {
		p.egress.noteConnected(at)
	}
}
