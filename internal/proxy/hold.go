package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/session"
)

// Hold and retry (PATCHES §58).
//
// A request whose upstream cannot be reached for a TRANSIENT reason -- DNS, a
// refused or timed-out dial, a reset connection, a token refresh that got no
// answer, an upstream 500/502/503/504 -- used to reach the client as a fast
// 502 or 5xx. Claude Code retries a handful of times inside a minute, so a
// laptop whose network dropped for longer than that lost every turn in
// flight: on 2026-10-20 that was ~2,400 failed /v1/messages in four hours
// (`dial tcp4: lookup api.anthropic.com: no such host`, then
// `connect: connection refused`), and every background agent with them.
//
// Now the request is HELD instead: nothing is written to the client, and the
// whole proxy handler runs again after an exponential backoff with jitter
// (capped at holdMaxBackoff per wait) until it is answered or the hold
// deadline passes. Each attempt is a fresh run of the handler -- selection,
// the token refresh, the peer ranking, the 429/529 failover -- so an attempt
// may land on another account and still counts against the same deadline,
// and a hold never pins anything: a move commits only on a 2xx, as before.
//
// What decides "transient" is the code that saw the failure. It marks the
// attempt (noteHoldable) just before it writes the error, and the hold
// writer swallows exactly that response. Everything unmarked -- a 429, a
// 529, a drain, a refusal, an exhausted pool's 503 unless no peer could be
// reached -- reaches the client as before, and so does anything after the
// first byte: once a status has passed to the client, the attempt is the
// answer.
//
// The hold never starts for a websocket upgrade, a request with
// X-Subrouter-No-Retry, a request forwarded by a peer daemon (the daemon
// that forwarded it decides; holding there would read as a silent peer), a
// chunked request body, one larger than replayablePostMaxBodyBytes, a caller
// the proxy refuses, or anything on the public listener.
//
// A stranded chat rides the hold (PATCHES §67). An attempt on the account its
// prompt cache is on -- a box that cannot be reached, or this daemon's own
// upstream that cannot be dialed -- is held and run again on that account
// instead of moving the chat to a cold cache. A hold that ends first -- its
// deadline, no room, a drain -- answers the 502 it kept, which Claude Code
// retries, and the retry is held again: the chat waits across requests until
// its account answers, or until its box is found down and it is placed as a
// refugee (placeRefugee). The chain of holds ends GiveUp after the chat's last
// answered turn with a 503 Claude Code shows at once (unlinkStranded): a chat
// a box held then waits for room, and any other is unlinked, so its next turn
// is placed as a new session. A spent pool's chat waits for a box instead,
// and its chain ends GiveUp after its first held turn (strandedWait). A
// chain (session.HeldChain) also ends when one of the chat's model turns is
// answered, when a hold of it ends with an answer the client does not send
// again (a box's own 503, a client that left), and after a hold's deadline
// and 2 minutes with no held attempt.

const (
	// DefaultHoldDeadline is serve's default: under Claude Code's
	// API_TIMEOUT_MS (600 s to response headers), with room for the last
	// attempt's own dial.
	DefaultHoldDeadline = 8 * time.Minute
	// DefaultHoldMaxRequests bounds how many client requests may be held at
	// once. A hold that cannot get a slot fails as it did before holds.
	DefaultHoldMaxRequests = 256
	// DefaultHoldMaxBytes bounds the request bodies held at once. The body of
	// a request in flight is buffered anyway (makeRequestBodyReplayable); a
	// hold keeps it for longer, so this is what bounds the extra memory.
	DefaultHoldMaxBytes int64 = 512 << 20
	// DefaultStrandedGiveUp is how long after its last answered turn a
	// stranded chat's chain of holds lasts: the warm window. By then the prompt
	// cache it waits for is cold and a box chat's peer hold has lapsed, so the
	// give-up answer comes before an attempt that would place it cold.
	DefaultStrandedGiveUp = warmSessionIdleTTL
	// strandedRetryAfterSeconds is the give-up answer's Retry-After with the
	// cap off: over 60 s, so that Claude Code shows the 503 at once and does
	// not retry it. With CLAUDE_CODE_RETRY_WATCHDOG it sleeps the hour
	// instead, so an end client is told at most ClientRetryAfterMax (PATCHES
	// §78).
	strandedRetryAfterSeconds = 3600
	strandedGaveUpReason      = "stranded_gave_up"
	// heldChainSlack is how long past a hold's deadline a chain of held
	// turns lasts with no held attempt (HoldPolicy.chainGap). An attempt can
	// run until the deadline before it fails, and Claude Code resends the
	// 502 within seconds, so a longer silence is a chat that stopped.
	heldChainSlack = 2 * time.Minute

	holdBaseBackoff = time.Second
	holdMaxBackoff  = 30 * time.Second
	// holdMaxSwallowedBytes caps the error body a held attempt keeps for the
	// give-up replay. An error body past it is written to the client as it
	// streams, which ends the hold: nothing is retried once bytes are out.
	holdMaxSwallowedBytes = 256 << 10
)

// HoldPolicy is serve's hold configuration and the live count of holds. Nil,
// or a non-positive Deadline, turns holding off: the handler is exactly what
// it was before §58.
type HoldPolicy struct {
	Deadline    time.Duration
	MaxRequests int
	MaxBytes    int64
	// GiveUp ends a stranded chat's chain of holds (noteStrandedAttempt). Zero
	// is no limit.
	GiveUp time.Duration

	// Backoff bounds; zero means holdBaseBackoff / holdMaxBackoff. Tests
	// shrink them.
	baseBackoff time.Duration
	maxBackoff  time.Duration
	// now is the deadline's clock; nil is time.Now. Tests move it.
	now func() time.Time

	mu    sync.Mutex
	holds int
	bytes int64
	runs  map[string]strandedRun
	// answeredAt is when each session's last model turn was answered in this
	// process, exact where the session store's AnsweredAt is coalesced.
	answeredAt map[string]time.Time
	// chains is the chain of held turns of each stranded session the session
	// store has no assignment for (Server.noteStrandedAttempt). One with an
	// assignment keeps its chain there, so the next process carries it on;
	// one without starts over in a new process.
	chains map[string]session.HeldChain
	// placing is each chat a turn is placing as a refugee right now
	// (Server.placeRefugee), with the box and account its cache was on.
	placing map[string]strandedHome
	// waiting is the wait for room of each chat that gave up on its box
	// (Server.unlinkStranded) when the session store could not save it. A
	// restart starts it over.
	waiting map[string]session.Waiting
}

// strandedHome is the box and account a stranded chat's cache is on.
type strandedHome struct{ peer, account string }

// strandedRun is a session's run of failed attempts, by scoped session key:
// from its first failure until one of its model turns is answered or it gives
// up (forget). Claude
// Code sends a request the hold answered again, so the run belongs to the
// session, not to a request.
type strandedRun struct {
	began time.Time
	// warmHere: the session was warm on its local account when the run began
	// (localWarm), or was placed there as a refugee since (placedHere). Judged
	// once, because every attempt stamps the session as seen: a new session's
	// second attempt would read as warm.
	warmHere bool
}

// NewHoldPolicy returns the policy serve runs with, or nil (holding off) for
// a non-positive deadline. Non-positive caps take the defaults; a
// non-positive give-up is no limit.
func NewHoldPolicy(deadline time.Duration, maxRequests int, maxBytes int64, giveUp time.Duration) *HoldPolicy {
	if deadline <= 0 {
		return nil
	}
	if maxRequests <= 0 {
		maxRequests = DefaultHoldMaxRequests
	}
	if maxBytes <= 0 {
		maxBytes = DefaultHoldMaxBytes
	}
	return &HoldPolicy{Deadline: deadline, MaxRequests: maxRequests, MaxBytes: maxBytes, GiveUp: max(giveUp, 0)}
}

func (p *HoldPolicy) enabled() bool {
	return p != nil && p.Deadline > 0
}

func (p *HoldPolicy) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// acquire takes a hold slot for a request with a body of size bytes. The
// returned refusal names the cap that refused it.
func (p *HoldPolicy) acquire(size int64) (release func(), active int, refusal string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.holds >= p.MaxRequests {
		return nil, p.holds, "max_requests"
	}
	if p.bytes+size > p.MaxBytes {
		return nil, p.holds, "max_bytes"
	}
	p.holds++
	p.bytes += size
	active = p.holds
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			p.holds--
			p.bytes -= size
			p.mu.Unlock()
		})
	}, active, ""
}

// Active reports the requests held right now.
func (p *HoldPolicy) Active() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.holds
}

// backoff is the wait before the attempt after attempt n (n >= 1): 1 s
// doubling to holdMaxBackoff, with equal jitter -- half fixed, half random --
// so held requests do not come back in lockstep when the network returns.
func (p *HoldPolicy) backoff(n int, random func() float64) time.Duration {
	base, ceiling := holdBaseBackoff, holdMaxBackoff
	if p != nil && p.baseBackoff > 0 {
		base = p.baseBackoff
	}
	if p != nil && p.maxBackoff > 0 {
		ceiling = p.maxBackoff
	}
	if n < 1 {
		n = 1
	}
	wait := ceiling
	if n <= 30 {
		if d := base << (n - 1); d > 0 && d < ceiling {
			wait = d
		}
	}
	half := wait / 2
	return half + time.Duration(random()*float64(wait-half))
}

// runLocked is the session's run, begun now when it has none. p.mu is held.
func (p *HoldPolicy) runLocked(sessionKey string, now time.Time, warmHere bool) strandedRun {
	if run, ok := p.runs[sessionKey]; ok {
		return run
	}
	if len(p.runs) > lastSeenSweepThreshold {
		for key, run := range p.runs {
			if now.Sub(run.began) > lastSeenRetention {
				delete(p.runs, key)
			}
		}
	}
	if p.runs == nil {
		p.runs = map[string]strandedRun{}
	}
	run := strandedRun{began: now, warmHere: warmHere}
	p.runs[sessionKey] = run
	return run
}

// localWarm reports whether the session was warm on its local account when
// its run began, beginning the run with warmNow when it has none.
func (p *HoldPolicy) localWarm(sessionKey string, warmNow bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runLocked(sessionKey, p.clock(), warmNow).warmHere
}

// placedHere records that the session was placed on its local account as a
// refugee (placeRefugee): its run reads as warm here from now on, so a failed
// dial waits for that account's upstream instead of spilling the turn to a
// box (PATCHES §67, D4). The run is not restarted: placement answers no turn.
func (p *HoldPolicy) placedHere(sessionKey string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	run := p.runLocked(sessionKey, p.clock(), true)
	run.warmHere = true
	p.runs[sessionKey] = run
}

// lastAnswered is when the session's last model turn was answered: the
// later of this process's record and persisted (another process's).
func (p *HoldPolicy) lastAnswered(sessionKey string, persisted time.Time) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if answered := p.answeredAt[sessionKey]; answered.After(persisted) {
		return answered
	}
	return persisted
}

// chainGap is how long a chain of held turns lasts with no held attempt.
func (p *HoldPolicy) chainGap() time.Duration {
	return p.Deadline + heldChainSlack
}

// heldAttempt records a held attempt at now of the chain this process keeps
// for the session, and returns it.
func (p *HoldPolicy) heldAttempt(sessionKey string, now time.Time) session.HeldChain {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.chains) > lastSeenSweepThreshold {
		for key, chain := range p.chains {
			if now.Sub(chain.Last) >= p.chainGap() {
				delete(p.chains, key)
			}
		}
	}
	if p.chains == nil {
		p.chains = map[string]session.HeldChain{}
	}
	chain := p.chains[sessionKey].HeldAt(now, p.chainGap())
	p.chains[sessionKey] = chain
	return chain
}

// endChain ends the chain this process keeps for the session.
func (p *HoldPolicy) endChain(sessionKey string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.chains, sessionKey)
}

// answered ends the session's run, its chain and its wait: one of its model
// turns was answered at at.
func (p *HoldPolicy) answered(sessionKey string, at time.Time) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.runs, sessionKey)
	delete(p.chains, sessionKey)
	delete(p.waiting, sessionKey)
	if len(p.answeredAt) > lastSeenSweepThreshold {
		for key, answered := range p.answeredAt {
			if at.Sub(answered) > lastSeenRetention {
				delete(p.answeredAt, key)
			}
		}
	}
	if p.answeredAt == nil {
		p.answeredAt = map[string]time.Time{}
	}
	p.answeredAt[sessionKey] = at
}

// beginPlacing records that a turn is placing the session as a refugee from
// home, and reports false when another turn is placing it already. With
// holding off every turn places, as no turn can wait for another.
func (p *HoldPolicy) beginPlacing(sessionKey string, home strandedHome) bool {
	if !p.enabled() {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, placing := p.placing[sessionKey]; placing {
		return false
	}
	if p.placing == nil {
		p.placing = map[string]strandedHome{}
	}
	p.placing[sessionKey] = home
	return true
}

// endPlacing records that the turn placing the session is done.
func (p *HoldPolicy) endPlacing(sessionKey string) {
	if !p.enabled() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.placing, sessionKey)
}

// placingFrom reports whether a turn is placing the session as a refugee,
// and from which home.
func (p *HoldPolicy) placingFrom(sessionKey string) (strandedHome, bool) {
	if !p.enabled() {
		return strandedHome{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	home, placing := p.placing[sessionKey]
	return home, placing
}

// wait keeps the session's wait for room, which the session store could not.
func (p *HoldPolicy) wait(sessionKey string, home session.Waiting) {
	if !p.enabled() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waiting == nil {
		p.waiting = map[string]session.Waiting{}
	}
	p.waiting[sessionKey] = home
}

// waitingFor is the session's wait for room this process keeps.
func (p *HoldPolicy) waitingFor(sessionKey string) (session.Waiting, bool) {
	if !p.enabled() {
		return session.Waiting{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	home, ok := p.waiting[sessionKey]
	return home, ok
}

// endWait ends the session's wait for room this process keeps.
func (p *HoldPolicy) endWait(sessionKey string) {
	if !p.enabled() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.waiting, sessionKey)
}

// forget drops what the policy knows of a session that gave up: its next
// turn starts as a new session's.
func (p *HoldPolicy) forget(sessionKey string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.runs, sessionKey)
	delete(p.answeredAt, sessionKey)
	delete(p.chains, sessionKey)
	delete(p.waiting, sessionKey)
}

// holdStranded marks the attempt as a stranded chat's (PATCHES §67) and
// answers it 502: the hold runs it again, and a client the hold does not
// carry retries it. peer is the box the account is on, "" for a local one.
func holdStranded(w http.ResponseWriter, r *http.Request, wait strandedWait, agentType, sessionID, peer, account, reason, message string) {
	noteHoldable(r.Context(), holdMark{status: http.StatusBadGateway, reason: reason,
		agent: agentType, session: sessionID, peer: peer, account: account, stranded: wait})
	http.Error(w, message, http.StatusBadGateway)
}

// strandedWait is what a stranded chat's chain of holds waits for. It sets
// the give-up's clock (Server.noteStrandedAttempt) and what the give-up says.
type strandedWait int

const (
	notStranded strandedWait = iota
	// waitsForItsCache: the account its prompt cache is on (a box that
	// cannot be reached, its local upstream), or a refugee slot. It gives up
	// an hour after its last answered turn, when that cache is cold.
	waitsForItsCache
	// waitsForABox: no account here can serve it and no box could be asked
	// (a spent pool). Its own account is spent, so no cache is at stake, and
	// it gives up an hour after its first held turn.
	waitsForABox
)

func (wait strandedWait) gaveUpMessage() string {
	if wait == waitsForABox {
		return "subrouter: no account here could serve this chat, and no box could be reached for over an hour"
	}
	return "subrouter: the account this chat's prompt cache is on could not be reached for over an hour; its cache is cold"
}

// holdMark is what the code that saw a transient failure says about it.
type holdMark struct {
	// status is the status the marked error is about to be written with; the
	// hold swallows only that.
	status  int
	reason  string
	agent   string
	session string
	// peer is the box a stranded chat's account is on, "" for this daemon's
	// own: a chat whose box it gave up on waits for room (unlinkStranded).
	peer    string
	account string
	// stranded: a chat's wait whose chain of holds gives up, or notStranded.
	stranded strandedWait
}

// holdAttempt rides one attempt's request context. The handler's failure
// sites mark it; the hold writer reads it.
type holdAttempt struct {
	mu   sync.Mutex
	mark holdMark
	set  bool
	// mayRun is the peers that may be running the request, shared by every
	// attempt of it (PATCHES §67).
	mayRun *mayRunPeers
	// retry: an attempt after the request's first (holdRetry).
	retry bool
}

type holdAttemptKey struct{}

// heldByAHold reports that a hold carries the request: it will run the
// handler again after an answer the attempt marks holdable.
func heldByAHold(r *http.Request) bool {
	attempt, ok := r.Context().Value(holdAttemptKey{}).(*holdAttempt)
	return ok && attempt != nil
}

// holdRetry reports an attempt the hold makes after the request's first. The
// hold admitted it against a quiesce (holdOnTransientFailure), and a drain
// turns away new requests, not this one, which the attempt's failure then
// ends with the answer the hold kept (PATCHES §67).
func holdRetry(r *http.Request) bool {
	attempt, ok := r.Context().Value(holdAttemptKey{}).(*holdAttempt)
	return ok && attempt != nil && attempt.retry
}

// holdMayRun is the peers that may be running the request ctx carries,
// shared by every attempt the hold makes of it; a request the hold does not
// carry gets its own.
func holdMayRun(ctx context.Context) *mayRunPeers {
	if attempt, ok := ctx.Value(holdAttemptKey{}).(*holdAttempt); ok && attempt != nil {
		return attempt.mayRun
	}
	return &mayRunPeers{}
}

// noteHoldable marks the attempt carried by ctx as failed for a transient
// reason, about to be answered with status. Without a hold in ctx (holding
// off, a request that cannot be held) it does nothing.
func noteHoldable(ctx context.Context, mark holdMark) {
	if ctx == nil {
		return
	}
	attempt, ok := ctx.Value(holdAttemptKey{}).(*holdAttempt)
	if !ok || attempt == nil {
		return
	}
	attempt.mu.Lock()
	attempt.mark, attempt.set = mark, true
	attempt.mu.Unlock()
}

func (a *holdAttempt) holdable(status int) (holdMark, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mark, a.set && a.mark.status == status
}

// holdableUpstreamStatus is an upstream answer the hold waits out: a server
// failure that says nothing about the account. 529 (overloaded) is NOT one:
// it keeps its existing same-account retries and then reaches the client.
func holdableUpstreamStatus(status int) bool {
	switch status {
	case http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// transientNetworkCause classifies an error from a round trip that produced
// no response. ok is false for anything that is not a network failure -- a
// cancel, a deadline on a request that was already sent, a certificate the
// far end should not have presented, a local error. neverSent reports a
// failure before the request could leave (DNS, dial, TLS handshake), so the
// upstream cannot have served it.
//
// The client's own deadline is not excluded here: every caller that marks a
// hold does so only while the client's context has no error.
func transientNetworkCause(err error) (cause string, neverSent, ok bool) {
	if err == nil || errors.Is(err, context.Canceled) {
		return "", false, false
	}
	// A certificate that does not verify is a network that is lying (a
	// captive portal, an intercepting proxy), not one that is down. Waiting
	// it out would hide it for eight minutes.
	var certErr *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var invalidCert x509.CertificateInvalidError
	if errors.As(err, &certErr) || errors.As(err, &unknownAuthority) ||
		errors.As(err, &hostnameErr) || errors.As(err, &invalidCert) {
		return "", false, false
	}
	// A lookup or a dial is classified before the deadline check below,
	// because one that ran out of time is a context.DeadlineExceeded to
	// errors.Is: a timed-out dial wraps net's timeout error, whose Is matches
	// it, and a timed-out lookup's DNSError unwraps to it. Both are the
	// network being down -- `dial tcp4 160.79.104.10:443: i/o timeout` was
	// more than half of the 10/20 dial failures.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns", true, true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		switch {
		case errors.Is(err, syscall.ECONNREFUSED):
			return "connection_refused", true, true
		case opErr.Timeout():
			return "dial_timeout", true, true
		default:
			return "dial", true, true
		}
	}
	// Any other deadline ran out on a request that was already sent:
	// net/http's response-header timeout, or a context deadline this process
	// set. The upstream may be running it, and waiting longer is not the
	// hold's call.
	if errors.Is(err, context.DeadlineExceeded) {
		return "", false, false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		if errors.Is(err, peerHandshakeTimeout{}) || strings.Contains(err.Error(), "TLS handshake timeout") {
			return "tls_handshake_timeout", true, true
		}
		return "timeout", false, true
	}
	if opErr != nil {
		return "connection", false, true
	}
	switch {
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE),
		errors.Is(err, syscall.ECONNABORTED), errors.Is(err, net.ErrClosed):
		return "connection", false, true
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection_closed", false, true
	}
	return "", false, false
}

// refreshHoldReason names a token refresh that reached no verdict
// (errRefreshTransient) for the hold's log line.
func refreshHoldReason(err error) string {
	if cause, _, ok := transientNetworkCause(err); ok {
		return "token_refresh:" + cause
	}
	return "token_refresh:no_verdict"
}

type holdWriterState int

const (
	holdPending   holdWriterState = iota // nothing written yet
	holdSwallowed                        // a marked failure, kept off the client
	holdCommitted                        // the client has the status line: this attempt is the answer
)

// holdWriter stands between one attempt and the client. It passes every
// response through except the one the attempt marked as transient, which it
// keeps (status, headers, at most holdMaxSwallowedBytes of body) so the hold
// can either drop it and retry or replay it unchanged when it gives up.
type holdWriter struct {
	client    http.ResponseWriter
	clientCtx context.Context // the client's request: done once it has left
	attempt   *holdAttempt
	header    http.Header
	state     holdWriterState
	status    int
	body      bytes.Buffer
	mark      holdMark
	// clientLeft: the client had already left when the status went out.
	clientLeft bool
}

func newHoldWriter(client http.ResponseWriter, clientCtx context.Context, attempt *holdAttempt) *holdWriter {
	return &holdWriter{client: client, clientCtx: clientCtx, attempt: attempt, header: make(http.Header)}
}

func (h *holdWriter) Header() http.Header {
	if h.state == holdCommitted {
		return h.client.Header()
	}
	return h.header
}

func (h *holdWriter) WriteHeader(code int) {
	switch h.state {
	case holdCommitted:
		h.client.WriteHeader(code)
		return
	case holdSwallowed:
		return
	}
	if code >= 100 && code < 200 {
		// An informational response would reach the client before the final
		// status is known; nothing this proxy serves needs one.
		return
	}
	if mark, ok := h.attempt.holdable(code); ok {
		h.state, h.status, h.mark = holdSwallowed, code, mark
		return
	}
	h.commit(code)
}

func (h *holdWriter) commit(code int) {
	dst := h.client.Header()
	for key, values := range h.header {
		dst[key] = values
	}
	h.client.WriteHeader(code)
	h.state, h.status = holdCommitted, code
	h.clientLeft = h.clientCtx.Err() != nil
}

func (h *holdWriter) Write(p []byte) (int, error) {
	switch h.state {
	case holdPending:
		h.WriteHeader(http.StatusOK)
		return h.Write(p)
	case holdSwallowed:
		if h.body.Len()+len(p) <= holdMaxSwallowedBytes {
			return h.body.Write(p)
		}
		// Too big to keep: the client gets it as it comes, and the hold is
		// over for this request.
		if err := h.spill(); err != nil {
			return 0, err
		}
	}
	return h.client.Write(p)
}

// spill writes a swallowed response to the client: the give-up replay, and
// the overflow above.
func (h *holdWriter) spill() error {
	h.commit(h.status)
	if h.body.Len() == 0 {
		return nil
	}
	_, err := h.client.Write(h.body.Bytes())
	h.body.Reset()
	return err
}

func (h *holdWriter) Flush() {
	_ = h.FlushError()
}

func (h *holdWriter) FlushError() error {
	switch h.state {
	case holdSwallowed:
		return nil
	case holdPending:
		h.WriteHeader(http.StatusOK)
		if h.state != holdCommitted {
			return nil
		}
	}
	return http.NewResponseController(h.client).Flush()
}

// Unwrap lets http.ResponseController reach the connection's deadlines.
// Flush never takes this path: holdWriter answers it above.
func (h *holdWriter) Unwrap() http.ResponseWriter {
	return h.client
}

// holdBody buffers the client's body once so every attempt sends the same
// bytes. ok is false for a request the hold cannot replay -- a chunked or an
// oversized body -- and r is then left exactly as it came in.
func holdBody(r *http.Request) (body []byte, hasBody, ok bool, err error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, false, true, nil
	}
	if r.ContentLength < 0 || r.ContentLength > replayablePostMaxBodyBytes {
		return nil, false, false, nil
	}
	body, err = io.ReadAll(io.LimitReader(r.Body, replayablePostMaxBodyBytes+1))
	if err != nil {
		return nil, false, false, err
	}
	if int64(len(body)) > replayablePostMaxBodyBytes {
		r.Body = prefixReadCloser{Reader: io.MultiReader(bytes.NewReader(body), r.Body), Closer: r.Body}
		return nil, false, false, nil
	}
	_ = r.Body.Close()
	return body, true, true, nil
}

// holdAttemptRequest is one attempt's request: a deep copy of the client's,
// because the handler rewrites the copy it is given -- the account's
// Authorization replaces the client's proxy token, and a second attempt on
// the same headers would be refused as unauthorized.
func holdAttemptRequest(r *http.Request, body []byte, hasBody bool, attempt *holdAttempt) *http.Request {
	req := r.Clone(context.WithValue(r.Context(), holdAttemptKey{}, attempt))
	if hasBody {
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		req.ContentLength = int64(len(body))
	}
	return req
}

// runHoldAttempt serves one attempt, and then ends its admission. A handler
// that aborts (http.ErrAbortHandler) while its response is still swallowed --
// an upstream error body that broke mid-copy -- has written nothing to the
// client, so the abort is kept for the give-up and the attempt can still be
// retried. Any other panic, or an abort after the client has bytes,
// propagates as it always did.
func runHoldAttempt(next http.Handler, w *holdWriter, r *http.Request, endAdmission func()) (aborted bool) {
	defer endAdmission()
	defer func() {
		if recovered := recover(); recovered != nil {
			if recovered == http.ErrAbortHandler && w.state == holdSwallowed {
				aborted = true
				return
			}
			panic(recovered)
		}
	}()
	next.ServeHTTP(w, r)
	return false
}

// holdEligible is a request the hold may retry at all.
func holdEligible(r *http.Request) bool {
	return !websocket.IsWebSocketUpgrade(r) &&
		!subrouterNoRetryRequest(r) &&
		remotePoolInboundHop(r) == 0
}

// holdOnTransientFailure wraps the proxy handler with the hold. Off (nil
// policy) it returns next unchanged, and so it does on the public listener:
// that one is reachable from the internet, and its only callers that hold a
// turn worth keeping are peer daemons, which hold their own.
func (s Server) holdOnTransientFailure(next http.Handler) http.Handler {
	policy := s.Hold
	if !policy.enabled() || s.publicSurface {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !holdEligible(r) {
			next.ServeHTTP(w, r)
			return
		}
		// A caller the proxy will refuse gets its 401 with its body unread, as
		// before the hold: buffering first would let anyone who can reach the
		// listener make it read 128 MiB per request.
		if _, authorized := s.proxyCaller(r); !authorized {
			next.ServeHTTP(w, r)
			return
		}
		body, hasBody, replayable, err := holdBody(r)
		if err != nil {
			http.Error(w, "buffer retryable request body: "+err.Error(), http.StatusBadGateway)
			return
		}
		if !replayable {
			next.ServeHTTP(w, r)
			return
		}
		deadline := policy.clock().Add(policy.Deadline)
		var (
			release     func()
			heldSince   time.Time
			first, last holdMark
		)
		defer func() {
			if release != nil {
				release()
			}
		}()
		mayRun := &mayRunPeers{}
		// The handler admits the first attempt; the hold admits each retry
		// before it runs it (below).
		endAdmission := func() {}
		for attemptNo := 1; ; attemptNo++ {
			attempt := &holdAttempt{mayRun: mayRun, retry: attemptNo > 1}
			hw := newHoldWriter(w, r.Context(), attempt)
			aborted := runHoldAttempt(next, hw, holdAttemptRequest(r, body, hasBody, attempt), endAdmission)
			if hw.state != holdSwallowed {
				if hw.state == holdPending {
					// The handler returned without writing: net/http's
					// empty 200, with whatever headers it set.
					hw.commit(http.StatusOK)
				}
				if release != nil {
					outcome := "answered"
					if hw.clientLeft {
						// The client left before the attempt's status went
						// out, so its answer went nowhere. A client that
						// hangs up once it has the answer was answered.
						outcome = "client_canceled"
					}
					s.logHoldEnded(r, first, last, outcome, hw.status, attemptNo, heldSince)
				}
				return
			}
			last = hw.mark
			now := policy.clock()
			giveUpAt := s.noteStrandedAttempt(last, now)
			attemptDeadline := deadline
			if !giveUpAt.IsZero() && giveUpAt.Before(deadline) {
				attemptDeadline = giveUpAt
			}
			giveUp := ""
			switch {
			case r.Context().Err() != nil:
				giveUp = "client_canceled"
			case release != nil && s.Lifecycle.ShuttingDown():
				giveUp = "shutdown_502"
			case s.Lifecycle.Draining():
				giveUp = "draining"
			case !giveUpAt.IsZero() && !now.Before(giveUpAt):
				giveUp = strandedGaveUpReason
			case !now.Before(deadline):
				giveUp = "deadline"
			}
			if release == nil && giveUp == "" {
				var active int
				var refusal string
				release, active, refusal = policy.acquire(int64(len(body)))
				if release == nil {
					if s.Logger != nil {
						s.Logger.Warn("proxy hold refused; failing this request now",
							holdLogAttrs(r, last, "refused", refusal, "status", hw.status,
								"active_holds", active, "max_requests", policy.MaxRequests,
								"max_bytes", policy.MaxBytes, "body_bytes", len(body))...)
					}
					giveUp = "refused"
				} else {
					heldSince, first = time.Now(), last
					if s.Logger != nil {
						s.Logger.Warn("proxy hold started",
							holdLogAttrs(r, first, "status", hw.status,
								"deadline", deadline.Sub(policy.clock()).Round(time.Second).String(),
								"active_holds", active)...)
					}
				}
			}
			if giveUp == "" {
				wait := policy.backoff(attemptNo, rand.Float64)
				if remaining := attemptDeadline.Sub(policy.clock()); wait > remaining {
					wait = remaining
				}
				var admitted bool
				switch {
				case !sleepForRetry(r.Context(), wait):
					giveUp = "client_canceled"
				case s.noteAttemptAndGaveUp(last, policy.clock()):
					// Read after the sleep, not from giveUpAt: a turn of the
					// chat answered during the sleep ends the chain.
					giveUp = strandedGaveUpReason
				default:
					// Admitted here, in one step with a quiesce, and not by
					// the handler's gate: a quiesce admits no attempt after
					// it, and the hold ends with the answer it kept.
					if endAdmission, admitted = s.Lifecycle.TryBeginProxyRequest(); admitted {
						continue
					}
					giveUp = "quiesced"
				}
			}
			if giveUp == "client_canceled" {
				// The client left and does not send the turn again.
				s.endHeldChain(last)
			}
			if giveUp == strandedGaveUpReason {
				s.unlinkStranded(last)
				retryAfter := strconv.FormatInt(s.clientRetryAfter(r, strandedRetryAfterSeconds), 10)
				w.Header().Set("Retry-After", retryAfter)
				w.Header().Set(subrouterReasonHeader, strandedGaveUpReason)
				http.Error(w, last.stranded.gaveUpMessage(), http.StatusServiceUnavailable)
				s.logStrandedGaveUp(r, last, attemptNo, policy.GiveUp, retryAfter)
				if release != nil {
					s.logHoldEnded(r, first, last, giveUp, http.StatusServiceUnavailable, attemptNo, heldSince)
				}
				return
			}
			status := hw.status
			switch {
			case giveUp == "shutdown_502":
				// Claude Code retries a 502 but shows a 503 whose Retry-After
				// is over 60 s at once, and the kept failure can be that 503.
				status = http.StatusBadGateway
				http.Error(w, "subrouter is shutting down; retry", status)
			default:
				// Give up: the client gets the failure the hold kept -- the same
				// status, headers and body it would have had without the hold.
				_ = hw.spill()
			}
			if release != nil {
				s.logHoldEnded(r, first, last, giveUp, status, attemptNo, heldSince)
			}
			if aborted {
				panic(http.ErrAbortHandler)
			}
			return
		}
	})
}

// logHoldEnded is the hold's second line: how it ended (answered, deadline,
// client_canceled, draining, quiesced), the status the client got, and how
// long it was held, which is from the first failure, not from the request's
// arrival.
func (s Server) logHoldEnded(r *http.Request, first, last holdMark, outcome string, status, attempts int, heldSince time.Time) {
	if s.Logger == nil {
		return
	}
	level := slog.LevelInfo
	if outcome != "answered" {
		level = slog.LevelWarn
	}
	s.Logger.Log(context.Background(), level, "proxy hold ended",
		holdLogAttrs(r, first, "outcome", outcome, "status", status, "attempts", attempts,
			"held", time.Since(heldSince).Round(time.Millisecond).String(),
			"last_reason", last.reason)...)
}

// logStrandedGaveUp is the line for a stranded chat's give-up answer, with
// the Retry-After it was sent.
func (s Server) logStrandedGaveUp(r *http.Request, last holdMark, attempts int, giveUp time.Duration, retryAfter string) {
	if s.Logger == nil {
		return
	}
	s.Logger.Warn("stranded chat gave up; answered 503",
		holdLogAttrs(r, last, "attempts", attempts, "give_up", giveUp.String(), "retry_after", retryAfter)...)
}

// noteAnswered records that one of the session's model turns was answered:
// a turn whose 2xx, or whose websocket response.completed, the client gets
// from an account's upstream, which is the stranded give-up's clock (PATCHES
// §67), and which ends the session's chain of held turns. With no give-up
// nothing reads it. The time is on the wall clock (noteStrandedAttempt).
func (s Server) noteAnswered(agentType, sessionID string) {
	if !s.Hold.enabled() || s.Hold.GiveUp <= 0 {
		return
	}
	at := s.Hold.clock().Round(0)
	key := session.ScopedSessionKey(agentType, sessionID)
	s.Hold.answered(key, at)
	for _, peer := range s.peers() {
		peer.delegations.answered(key, at)
	}
	if s.Sessions == nil {
		return
	}
	if err := s.Sessions.NoteAnswered(agentType, sessionID, at); err != nil && s.Logger != nil {
		s.Logger.Warn("session answered time not saved; the stranded give-up keeps this process's",
			"agent", agentType, "session", sessionID, "error", err)
	}
}

// noteStrandedAttempt records a held attempt at now of the stranded chat the
// mark names, in its chain of held turns (session.HeldChain), and returns
// when that chain gives up. A wait for a box counts from the chain's first
// held turn. A wait for its cache counts from the latest answered time kept
// for the session: this process's, its local assignment's or its box
// hold's; with none, from the chain's first held turn. Zero is no limit, and
// so is an attempt that is not a stranded chat's.
//
// The session store keeps the chain of a session it has an assignment for,
// so the next worker carries it on; this process keeps any other's, and one
// the store could not save, which a restart starts over. The chain is on the
// wall clock, as a box hold's lapse is (holdUntil): a laptop sleep stops the
// monotonic clock on darwin, and the give-up must still come before the
// lapse.
func (s Server) noteStrandedAttempt(mark holdMark, now time.Time) time.Time {
	if mark.stranded == notStranded || s.Hold.GiveUp <= 0 {
		return time.Time{}
	}
	key := session.ScopedSessionKey(mark.agent, mark.session)
	var chain session.HeldChain
	var answered time.Time
	stored := false
	if s.Sessions != nil {
		assignment, ok, err := s.Sessions.NoteHeld(mark.agent, mark.session, now, s.Hold.chainGap())
		if err != nil && s.Logger != nil {
			s.Logger.Warn("stranded chat's chain of held turns not saved; this process keeps it, and a new process starts it over",
				"agent", mark.agent, "session", mark.session, "error", err)
		}
		// A chain the store could not save is gone at the next attempt's
		// reload, so this process keeps it.
		chain, answered, stored = assignment.Held, assignment.AnsweredAt, ok && err == nil
	}
	if !stored {
		chain = s.Hold.heldAttempt(key, now)
	}
	if mark.stranded == waitsForABox {
		return chain.Since.Add(s.Hold.GiveUp)
	}
	for _, peer := range s.peers() {
		if hold, ok := peer.delegations.lookup(key, time.Now()); ok && hold.AnsweredAt.After(answered) {
			answered = hold.AnsweredAt
		}
	}
	if answered = s.Hold.lastAnswered(key, answered); !answered.IsZero() {
		return answered.Add(s.Hold.GiveUp)
	}
	return chain.Since.Add(s.Hold.GiveUp)
}

// noteAttemptAndGaveUp records a held attempt at now of the stranded chat the mark
// names (noteStrandedAttempt) and reports whether its chain has given up.
func (s Server) noteAttemptAndGaveUp(mark holdMark, now time.Time) bool {
	giveUpAt := s.noteStrandedAttempt(mark, now)
	return !giveUpAt.IsZero() && !now.Before(giveUpAt)
}

// endHeldChain ends the chain of held turns of the stranded chat the mark
// names: a hold of it ended with an answer the client does not retry.
func (s Server) endHeldChain(mark holdMark) {
	if mark.stranded == notStranded || s.Hold.GiveUp <= 0 {
		return
	}
	s.Hold.endChain(session.ScopedSessionKey(mark.agent, mark.session))
	if s.Sessions == nil {
		return
	}
	if err := s.Sessions.EndHeld(mark.agent, mark.session); err != nil && s.Logger != nil {
		s.Logger.Warn("stranded chat's chain of held turns not ended; its next held turn carries it on",
			"agent", mark.agent, "session", mark.session, "error", err)
	}
}

// unlinkStranded drops what a session that gave up holds: its peer holds,
// and with them a refugee's slot. Its held attempts kept it seen, so it would
// not go cold on its own. A chat whose cache was on a box keeps its
// assignment, marked waiting for room with that box and account as its home
// (resumeWaitingRefugee); when the store cannot save that, this process
// keeps it. Any other is unlinked from its local account: its next turn is
// placed as a new session.
func (s Server) unlinkStranded(mark holdMark) {
	key := session.ScopedSessionKey(mark.agent, mark.session)
	s.clearHoldElsewhere(key, nil)
	s.Hold.forget(key)
	if s.Sessions == nil {
		return
	}
	if mark.peer != "" {
		home := session.Waiting{Peer: mark.peer, Account: mark.account, Since: s.Hold.clock()}
		if err := s.Sessions.Wait(mark.agent, mark.session, home); err != nil {
			s.Hold.wait(key, home)
			if s.Logger != nil {
				s.Logger.Warn("stranded session's wait for room not saved; this process keeps it, and a new process places its next turn as a new session",
					"agent", mark.agent, "session", mark.session, "error", err)
			}
		}
		return
	}
	if _, err := s.Sessions.Delete(mark.agent, mark.session); err != nil && s.Logger != nil {
		s.Logger.Warn("stranded session not unlinked from its account; its next turn keeps it",
			"agent", mark.agent, "session", mark.session, "error", err)
	}
}

func holdLogAttrs(r *http.Request, mark holdMark, extra ...any) []any {
	attrs := []any{
		"reason", mark.reason,
		"agent", mark.agent,
		"session", mark.session,
		"account", mark.account,
		"method", r.Method,
		"path", r.URL.Path,
		"remote_addr", clientRemoteIP(r),
	}
	return append(attrs, extra...)
}

// refundUnservedDebit gives back the live debit (SchedulerRef.NoteRouted) of
// an attempt the upstream never served: a DNS or dial failure, or a server
// error. A held request makes many attempts, and each one used to debit its
// account 2% of headroom until the next successful usage read -- which an
// outage also prevents -- so a few minutes of holding would read a healthy
// account as spent.
func (s Server) refundUnservedDebit(provider accounts.Provider, accountID string) {
	if s.SchedulerRef == nil || s.CredentialBroker != nil || accountID == "" {
		return
	}
	s.SchedulerRef.RefundRouted(schedulerAccountProvider(provider), accountID)
}
