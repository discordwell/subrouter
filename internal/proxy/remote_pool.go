package proxy

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/discordwell/subrouter/account"
	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
	"github.com/discordwell/subrouter/session"
)

// Remote pool fallthrough: when this daemon's own pool cannot serve, hand the
// request to another Subrouter daemon (typically a shared box on a tailnet) and
// stream its answer back. The client keeps one endpoint and never relaunches.
//
// Local is always preferred: this runs only after local selection has failed or
// local failover has been exhausted, so a healthy local pool never reaches it.
//
// It forwards to the remote's PROXY FRONT DOOR with the original path intact,
// not to a provider endpoint. The remote then runs its own upstreamForRequest,
// which is why Claude (/v1/messages) and Codex (/v1/..., /backend-api/...) all
// work here with no per-provider logic.
const (
	// Deliberately NOT an X-Subrouter-* name: session.StripSubrouterHeaders
	// deletes every one of those, which would erase the loop guard on each hop
	// and let A -> B -> A recurse forever.
	remotePoolHopHeader = "X-Pool-Hop"

	// Two strikes, not one. An idle tailnet link times out its first packet
	// while the direct path re-establishes and then answers in ~100ms; a
	// one-strike breaker flaps on exactly that.
	remotePoolFailuresToOpen = 2
	remotePoolCooldown       = 30 * time.Second

	// A counted strike more than this after the previous one starts the
	// peer's silence again (PATCHES §67): in between nothing was sent to the
	// peer, or this machine slept, and nobody observed it being silent.
	remotePoolQuietGapMax = 5 * time.Minute

	// This sits on the request path, so a dead remote must cost a bounded
	// wait, not a default dial timeout.
	remotePoolDialTimeout = 2 * time.Second

	// remotePoolStreamIdleTimeout bounds SILENCE on a peer's answer once its
	// headers are in, never its length (peerStreamWatchdog, PATCHES §33).
	//
	// It used to be the client's total Timeout, and http.Client.Timeout also
	// covers reading the body: every forwarded turn was cut 5 minutes after it
	// was sent, however busily it was still streaming, and the relay dropped
	// the read error, so the client got a truncated answer that ended cleanly
	// and the log said nothing. A local turn has no total bound at all.
	//
	// Same 5 minutes, now counted while the relay waits inside a single read,
	// so at least this long after the last byte (more when the relay was busy
	// writing to a slow client in between): a stream cut by this was also cut
	// by the old total, so nothing the old client delivered is lost, and a
	// hung peer that already sent its headers costs this much waiting on it.
	// The clients' own stall detectors (Codex 300 s, Claude Code ~270-300 s)
	// usually give up first on an interactive stall; the cut line then says
	// cause=client_gone with the peer's silent_ms.
	remotePoolStreamIdleTimeout = 5 * time.Minute

	// A peer that goes SILENT -- routes yanked by a VPN flipping on, the tailnet
	// dropping under it -- is not covered by the dial and handshake bounds. They
	// only run when a connection is made, and a warm keep-alive socket makes
	// none: it just stops receiving. Without this the request waits for headers
	// until the client gives up before the caller can fall back to local
	// accounts, so a dead peer costs minutes per request and the breaker needs
	// two of those to open.
	//
	// The bound belongs on time-to-first-BYTE, not total time: a legitimate long
	// completion also runs for minutes, it just sends headers immediately and
	// then streams, so headers are exactly what separates the two. 30s is ~30x
	// the header latency of a streaming turn while cutting a blackholed peer from
	// 300s to 30s (breaker opens at ~60s, then a 30s cooldown fully local).
	//
	// A false trip is cheap by construction: remotePoolForward returns not-ok and
	// the handler re-runs local selection, so the client still gets its answer.
	// The cost is a duplicated attempt, never a failed request.
	remotePoolResponseHeaderTimeout = 30 * time.Second

	// remotePoolWriteStallTimeout bounds a SEND that stopped moving: a write to
	// the peer that cannot get its next 32 KiB into the socket within this
	// fails, and the forward with it (peerWriteStallConn, PATCHES §33).
	//
	// The header timeout's clock starts only once the request is fully written,
	// and the stream watchdog's once the headers are in. Sending the body is
	// the phase between them, and the client's old 5-minute total was its only
	// bound. Without this, a turn bigger than the socket buffers -- most are --
	// sent on a warm socket whose route was blackholed, or to a peer that
	// stopped reading, blocks in the write until the client gives up. That
	// reads as a client cancel, so the breaker never counts it and the hold
	// sends the session's next turn the same way. Progress, not length, as for
	// the answer: a large body on a slow link that keeps moving is never cut.
	remotePoolWriteStallTimeout = 30 * time.Second

	// remotePoolMaxIdleConns is how many idle connections the peer client
	// keeps for reuse. A client talks to one peer, so this is the per-host cap
	// too. The client speaks HTTP/1.1 only (a custom DialContext and no
	// ForceAttemptHTTP2), so every forward in flight holds a connection of its
	// own, and Go's default keeps two: every concurrent forward past the
	// second paid a fresh dial, TLS handshake and slow start, then threw its
	// connection away. MaxConnsPerHost stays unset: a cap there would queue
	// forwards behind each other.
	remotePoolMaxIdleConns = 32

	// remotePoolIdleConnTimeout closes a peer connection idle this long. It
	// sits just under the 620 s a peer's server keeps an idle connection
	// (workerIdleTimeout in cmd/subrouter), so this side closes first and no
	// turn is sent into a connection the peer is closing. Go checks it in wall
	// time when it hands a connection out, so a Mac that slept past it dials
	// afresh instead of sending turns into connections the peer dropped while
	// it slept: net/http does not retry a POST there, so each one was a failed
	// forward, a breaker strike and a turn served locally.
	remotePoolIdleConnTimeout = 10 * time.Minute

	// The peer hold non-Claude sessions used before the warm rule covered both
	// providers (PATCHES §21). Every agent now holds for warmSessionIdleTTL --
	// see remotePoolDelegationTTLFor; this stays only as an arbitrary hold
	// length for delegation-table tests.
	remotePoolDelegationTTL = 2 * time.Hour
)

// remotePoolTransientHeader marks a 502 whose cause was transient on the
// answering daemon (a token refresh that reached no verdict). The caller keeps
// the session's hold and serves only that turn elsewhere. A plain 503 still
// means "my pool is spent" and ends the hold.
const remotePoolTransientHeader = "X-Subrouter-Transient"

// remotePoolTransientOverload is the header's value on a TARGETED turn's
// provider-side server failure (a 529 or other 5xx), which the answering
// daemon already retried on the target (targetedOverloadRetryTransport): it
// says nothing about the account, so the decider passes it through with the
// hold kept (classifyAccountSwitch), exactly as it always passed a peer's 529.
// Without it a 503 read as a legacy peer's "my pool is spent" and ended the
// hold. PATCHES §31.
const remotePoolTransientOverload = "overload"

// remotePoolWebSocketTouchInterval bounds how often traffic on a forwarded
// socket restarts its peer hold's idle clock. Far below any hold TTL, so an
// open socket never lets its hold lapse.
const remotePoolWebSocketTouchInterval = 30 * time.Second

// remotePoolDelegationTTLFor is how long a session idles on the peer before it
// re-ranks. For every agent it is the SAME idle bound that holds a session on
// a local account (warmSessionIdleTTL), so a conversation is exactly as hard
// to move off the peer as off a local account: stuck while it is warm, free to
// re-rank once it has gone cold. A longer peer hold kept cold sessions on the
// peer past the point where moving costs nothing, and a shorter one would move
// live ones. Codex held for 2h until the warm rule covered it (PATCHES §21).
func remotePoolDelegationTTLFor(agentType string) time.Duration {
	return warmSessionIdleTTL
}

// PeerHoldIdleTTL is how long a peer hold lasts with no turn. With a
// peer-down grace this long or longer, a silent peer's holds lapse before it
// can be found down, and its chats go cold instead of being placed as
// refugees.
const PeerHoldIdleTTL = warmSessionIdleTTL

// DefaultPeerDownGrace is serve's Server.PeerDownGrace. Past 5 min every
// routed subagent's prompt cache on the box has expired, so moving it costs
// nothing more. A box silent this long is found down for its held sessions
// when it stops ranking for new ones (remotePoolDecisionBodyMaxAge, PATCHES
// §67).
const DefaultPeerDownGrace = remotePoolCarryTTL

// RemotePool is the configured peer daemon. A nil *RemotePool disables the
// whole feature, which is what keeps it inert for everyone who has not opted in.
type RemotePool struct {
	URL   *url.URL
	Token string
	// AllowFrom restricts which callers may spend the peer's accounts. Empty
	// means open to every caller.
	//
	// This matters on a SHARED daemon. Falling through to a peer spends
	// somebody's personal subscription, and on a box shared by several users an
	// unrestricted peer quietly turns one person's accounts into everyone's
	// overflow capacity. Scoping it to the owner's own addresses makes the
	// donation deliberate rather than a side effect of being dry.
	AllowFrom []*net.IPNet

	// Fingerprint pins the peer's TLS certificate: lowercase hex SHA-256 over
	// the DER leaf, the value CertificateFingerprint produces and the box logs
	// at startup. Empty means "no pin", which is NOT "trust anything" -- see
	// peerTLSConfig.
	//
	// Peers are self-signed by design (see internal/proxy/identity.go), so a pin
	// is how a peer is identified at all. There is no CA to fall back on, which
	// makes this the entire trust decision for a public peer.
	Fingerprint string

	// Name is how this peer is named in logs, holds and X-Subrouter-Peer. Set
	// from a SUBROUTER_REMOTE_POOLS_FILE entry; empty for the single-peer env
	// configuration, which keeps naming the peer by URL.Host exactly as before
	// (PATCHES §31, D0).
	Name string

	client      *http.Client
	wsDialer    *websocket.Dialer
	breaker     remotePoolBreaker
	delegations remotePoolDelegations
	capacity    remotePoolCapacityCache
	admin       remotePoolAdminCache
	peerScores  remotePoolScoreCarry
	// targets remembers, per peer, which of its accounts refused a targeted
	// turn and whether it honours targets at all -- each until the peer's next
	// usage body (remote_pool_peers.go).
	targets remotePoolTargetMarks
	// routed counts the targeted turns this daemon sent each of the peer's
	// accounts, for the live debit the ranking applies until a newer body.
	routed remotePoolRoutedTargets
	// streamIdle is how long a forwarded answer may go without a byte before
	// it is cut (remotePoolStreamIdleTimeout); per pool so a test can shrink
	// it without touching a package global.
	streamIdle time.Duration
	// egress is this machine's own upstream evidence, which the breaker reads
	// when it opens: only an outage this machine can attribute to the peer
	// ends the holds on it (localEgressObserver, PATCHES §54).
	egress  localEgress
	tailnet *peerTailnet
}

// remotePoolRoutedTargets is the peer half of the live debit (PATCHES §31, R3
// F-E). The peer publishes its MEASURED numbers (withNormalizedUsage reads the
// undebited score), so between two of its sweeps a peer account looks exactly
// as roomy after this daemon has sent it ten sessions as before the first,
// while every local account sinks by LiveDebitPerRequest per routed request.
// The ranking then drifts toward the peer until the next body. So this daemon
// debits a peer account for the targeted turns IT sent there since the body
// the candidate was read from -- the same debit, the same clearing point (a
// newer reading), and nothing it cannot see (other deciders' traffic is in the
// owner's next body, as a peer's own traffic is in ours).
type remotePoolRoutedTargets struct {
	mu      sync.Mutex
	entries map[string][]time.Time
}

// remotePoolRoutedRetention bounds the memory: a body older than this is not
// ranked on anyway (remotePoolCarryTTL).
const remotePoolRoutedRetention = remotePoolCarryTTL

func routedTargetKey(provider, account string) string {
	return provider + "\x00" + account
}

func (r *remotePoolRoutedTargets) note(provider, account string, at time.Time) {
	if account == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = map[string][]time.Time{}
	}
	key := routedTargetKey(provider, account)
	kept := r.entries[key][:0]
	for _, t := range r.entries[key] {
		if at.Sub(t) < remotePoolRoutedRetention {
			kept = append(kept, t)
		}
	}
	r.entries[key] = append(kept, at)
}

// since is how many targeted turns went to account after the body read at
// bodyAt. A zero bodyAt counts nothing: no reading, nothing to debit against.
func (r *remotePoolRoutedTargets) since(provider, account string, bodyAt time.Time) int {
	if bodyAt.IsZero() {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, t := range r.entries[routedTargetKey(provider, account)] {
		if t.After(bodyAt) {
			count++
		}
	}
	return count
}

// WithFingerprint pins the peer's certificate and rebuilds both TLS paths.
//
// A builder rather than a constructor argument so existing call sites keep
// compiling -- but note that omitting it is safe: without a pin the dialers fall
// back to ORDINARY CA VALIDATION, which refuses a self-signed peer outright.
// Forgetting the pin therefore costs a connection, never a silently unverified
// one.
func (p *RemotePool) WithFingerprint(fingerprint string) *RemotePool {
	if p == nil {
		return nil
	}
	p.Fingerprint = strings.ToLower(strings.TrimSpace(fingerprint))
	p.applyTLS()
	return p
}

// applyTLS pushes the current pin onto both client paths.
//
// BOTH, always. Codex opens every model turn as a websocket upgrade, so a pin
// applied only to the HTTP client would leave the majority of peer traffic
// unverified while every test of the HTTP path passed.
func (p *RemotePool) applyTLS() {
	if transport, ok := p.client.Transport.(*http.Transport); ok {
		transport.TLSClientConfig = p.peerTLSConfig(nil)
	}
	if p.wsDialer != nil {
		// http/1.1 is required: an upgrade cannot proceed over h2 at all.
		p.wsDialer.TLSClientConfig = p.peerTLSConfig([]string{"http/1.1"})
	}
}

// peerTLSConfig is the TLS policy for talking to this peer.
func (p *RemotePool) peerTLSConfig(nextProtos []string) *tls.Config {
	config := &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: nextProtos}
	if p.Fingerprint == "" {
		// No pin configured: keep ordinary CA validation. This is the
		// fail-closed direction -- a self-signed peer (which is what our boxes
		// are) is REFUSED rather than accepted unverified.
		return config
	}
	// With a pin, the pin IS the validation. InsecureSkipVerify disables name
	// and chain checking, which is correct here -- the certificate is
	// self-signed and carries no SANs -- but it is also exactly how this check
	// becomes decorative, so the verifier below must never return nil without
	// having compared something.
	config.InsecureSkipVerify = true
	config.VerifyPeerCertificate = pinnedCertificateVerifier(p.Fingerprint)
	return config
}

// PinnedTLSConfig is the peer link's pinned TLS policy for another client of
// a box's public listener (`sr`'s public admin path, PATCHES §43.2): the
// certificate must hash to fingerprint (lowercase hex SHA-256 of the DER
// leaf) and nothing else is checked. An empty or wrong pin refuses every
// server, inside the handshake, before a byte of the request is written, so
// a credential is never sent to an endpoint that failed the pin.
func PinnedTLSConfig(fingerprint string) *tls.Config {
	return &tls.Config{
		MinVersion:            tls.VersionTLS12,
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: pinnedCertificateVerifier(strings.ToLower(strings.TrimSpace(fingerprint))),
	}
}

var errPeerPinMismatch = errors.New("does not match the pin")

// pinnedCertificateVerifier compares the presented leaf against the pin.
//
// Constant-time, and it refuses on every path that is not a positive match: no
// certificate, no pin, wrong pin. A verifier that returns nil on any of those
// alongside InsecureSkipVerify accepts any TLS server on the internet as this
// peer, while looking, in review, exactly like a certificate check.
func pinnedCertificateVerifier(want string) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if want == "" {
			return errors.New("peer certificate pin is empty; refusing to verify")
		}
		if len(rawCerts) == 0 {
			return errors.New("peer presented no certificate")
		}
		got := CertificateFingerprint(rawCerts[0])
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			return fmt.Errorf("peer certificate fingerprint %s %w", got, errPeerPinMismatch)
		}
		return nil
	}
}

// Peer control-plane reads, for the pool board. Distinct from probeCapacity,
// which reduces the peer to two routing numbers and is on the request path;
// these return the peer's raw admin JSON for rendering.
const (
	// remotePoolAdminTimeout bounds the WHOLE request, not just the dial.
	//
	// p.client has no total timeout (a model answer streams for as long as it
	// streams, PATCHES §33), and the stream watchdog is on forwarded turns
	// only. A control read borrowing that client and relying on
	// remotePoolDialTimeout is bounded only while CONNECTING, and the header
	// timeout ends at the headers: a half-open tailnet connection that answers
	// and then stalls its body blocks the read indefinitely -- inside a handler
	// on the daemon every agent's traffic routes through. The context is the
	// actual bound, body included.
	remotePoolAdminTimeout = 3 * time.Second
	// remotePoolAdminTTL keeps an open board from hammering the peer's
	// daemon. Shared across page loads, so N tabs cost the same as one.
	remotePoolAdminTTL = 10 * time.Second
	// remotePoolAdminMaxBody matches probeCapacity's LimitReader: a peer
	// cannot make this daemon allocate without bound.
	remotePoolAdminMaxBody = 1 << 20
)

// remotePoolAdminCache holds the last GOOD response per path.
//
// The retention is the point, and it mirrors probeCapacity's measured=false
// discipline: on any failure -- dial, status, decode -- the previous payload is
// returned with its original timestamp and Reachable false. Returning empty
// would render a reachable-but-blipping peer as a pool with no accounts, which
// is the one reading that must never be produced: "the box is empty" and "the
// box did not answer" are opposite operational facts.
type remotePoolAdminCache struct {
	mu      sync.Mutex
	entries map[string]remotePoolAdminEntry
	// inflight is the single-flight background fetch per path started by
	// usageForDecision; closed when it has written its result here.
	inflight map[string]chan struct{}
}

type remotePoolAdminEntry struct {
	body []byte
	at   time.Time
	err  string
	// tried is when the last fetch FINISHED, success or not. The decision
	// path's refresh clock (usageForDecision): a peer whose fetches fail is
	// retried once per remotePoolAdminTTL, not on every decision.
	tried time.Time
}

// AdminSnapshot is a peer admin read: the body, when it was actually measured,
// and whether THIS read reached the peer. A false Reachable with a non-zero At
// is stale-but-real data; a false Reachable with a zero At is no data at all.
type AdminSnapshot struct {
	Body      []byte
	At        time.Time
	Reachable bool
	Err       string
}

// AdminGet reads an admin path from the peer, cached and last-good-retaining.
//
// The bearer token never leaves this package: callers name a path and get bytes.
func (p *RemotePool) AdminGet(ctx context.Context, path string) AdminSnapshot {
	if p == nil || p.URL == nil {
		return AdminSnapshot{Err: "no peer configured"}
	}
	p.admin.mu.Lock()
	if p.admin.entries == nil {
		p.admin.entries = map[string]remotePoolAdminEntry{}
	}
	cached, hasCached := p.admin.entries[path]
	if hasCached && time.Since(cached.at) < remotePoolAdminTTL && cached.err == "" {
		p.admin.mu.Unlock()
		return AdminSnapshot{Body: cached.body, At: cached.at, Reachable: true}
	}
	p.admin.mu.Unlock()

	body, err := p.adminFetch(ctx, path)
	return p.admin.store(path, body, err)
}

// store records one fetch's result under the last-good rule and returns what
// a reader now sees. Shared by AdminGet and the decision path's background
// fetch, so both write the one cache the same way.
func (c *remotePoolAdminCache) store(path string, body []byte, err error) AdminSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]remotePoolAdminEntry{}
	}
	if err != nil {
		// Keep the last good body and its ORIGINAL timestamp, so the board can
		// say how stale it is rather than implying it is current.
		cached := c.entries[path]
		cached.err = err.Error()
		cached.tried = time.Now()
		c.entries[path] = cached
		return AdminSnapshot{Body: cached.body, At: cached.at, Reachable: false, Err: err.Error()}
	}
	now := time.Now()
	entry := remotePoolAdminEntry{body: body, at: now, tried: now}
	c.entries[path] = entry
	return AdminSnapshot{Body: entry.body, At: entry.at, Reachable: true}
}

func (p *RemotePool) adminFetch(ctx context.Context, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, remotePoolAdminTimeout)
	defer cancel()
	target := *p.URL
	target.Path = strings.TrimSuffix(target.Path, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	if p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	// Any answer is the peer answering, whatever the status: it restarts the
	// grace before the peer is found down (PATCHES §67).
	p.breaker.noteAnswer(time.Now())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Deliberately does not include the body: a peer's error page is not
		// something to splice into this daemon's own output.
		return nil, fmt.Errorf("peer returned %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, remotePoolAdminMaxBody))
}

// remotePoolCapacityCache holds the peer's best headroom per provider, so the
// spill decision can compare two pools instead of guessing about one.
//
// Without this the daemon spills on a local-only signal and forwards blind: a
// peer that is drier than we are still gets the request, and the client pays a
// network hop to be told no. Refreshed at most once per TTL because this sits
// on the request path.
type remotePoolCapacityCache struct {
	mu       sync.Mutex
	at       time.Time
	best     map[string]remotePoolProviderCapacity
	measured bool
	inflight bool
}

// remotePoolProviderCapacity is a peer pool reduced to the two numbers the
// spill rule can compare. Pace is the primary signal and Headroom the fallback
// for pools that publish no weekly window (Codex, carried seeds).
type remotePoolProviderCapacity struct {
	Pace     float64
	PaceOK   bool
	Headroom float64
	// MeasuredAt is when these numbers were actually read off the peer. A
	// carried entry (see remotePoolCarryTTL) keeps its original stamp, so the
	// carry ages out instead of being refreshed by the act of carrying it.
	MeasuredAt time.Time
	// SecondsToRefresh is how long until this pool's soonest weekly window
	// rolls. For a weekly-only provider it replaces Pace as the ordering
	// signal: with no second window to price against, "how much is left" says
	// nothing about urgency and "when does it evaporate" says everything.
	SecondsToRefresh float64
	RefreshOK        bool
}

// routingCapacity describes the LOCAL pool at the moment of selection: not
// merely whether the pick cleared the floor, but how much was actually left.
// The number is the point -- a bare below-floor bool cannot answer "is the peer
// meaningfully better than this?", which is the question that decides whether
// local gets drained to nothing while a healthy peer sits idle.
type routingCapacity struct {
	// BelowFloor is selection's own verdict: this pick is draining the pool
	// rather than spending it.
	BelowFloor bool
	// Headroom is the lower of the selected account's long and short windows.
	Headroom float64
	// Known is false when the number above is a seed, an optimistic default,
	// or an entry the daemon has not yet filled from a usage fetch, rather
	// than a live measurement. Presence of a score is NOT enough: a restarted
	// daemon holds entries reading 1.0 until its first fetch lands.
	Known bool
	// Pace is the selected account's weekly spend rate -- remaining weekly
	// budget per second until the roll -- and is the PRIMARY signal. Headroom
	// answers "how much is left", pace answers "how much is left that must be
	// spent soon", and only the second one says where a request belongs.
	Pace float64
	// PaceOK is false for pools with no weekly window to price: Codex scores
	// and carried seeds. The comparison falls back to Headroom there.
	PaceOK bool
	// Endgame marks the selected account as inside its burndown window: the
	// 24h weekly roll, where the budget expires whether or not it is spent, a
	// reset holder's early burndown (PATCHES §56), or an early burndown on its
	// pace (§110). The decision line's local_endgame.
	Endgame bool
	// Score is the selected account's full score, carried so the local/remote
	// decision can run the SAME comparator selection runs rather than a second
	// approximation of it built out of pool-level aggregates.
	Score selectacct.Score
	// WeeklyOnly marks a provider whose accounts publish a weekly meter and no
	// short window -- Codex since 2026-07-12. Such a pool is ordered on time to
	// refresh rather than on pace: every account holds the same kind of budget
	// under the same kind of deadline, so the ratio of headroom to time left
	// degenerates into "whose window opened longer ago" and the deadline itself
	// is the real signal.
	WeeklyOnly bool
	// SecondsToRefresh is how long until the selected account's weekly window
	// rolls, and with it whatever budget is left.
	SecondsToRefresh float64
	RefreshOK        bool
}

// remotePoolCapacityTTL is short enough to notice a peer draining mid-session
// and long enough that a busy client is not probing on every turn.
const remotePoolCapacityTTL = 60 * time.Second

// remotePoolUsagePath is the peer's account snapshot. It is admin-gated, so a
// peer that enforces a token needs SUBROUTER_REMOTE_POOL_TOKEN set here;
// without it the probe reads unmeasured and the caller falls back to the
// absolute floor rather than to a fabricated capacity number.
const remotePoolUsagePath = "/_subrouter/usage-status"

// remotePoolCarryTTL bounds how long a provider's last MEASURED reading stands
// in for rows the peer could not measure.
//
// A peer whose sweep was cut short (its own clock, or on an older binary a
// reader's) publishes rows that reached no verdict. Those rows are not "dead"
// and they are not "empty" -- they are unknown, and the only honest number for
// an unknown pool is the last one actually measured, for as long as it is
// plausibly still true. Matches the peer's own last-good horizon
// (usageStatusLastGoodTTL) so the two ends of the wire age a reading alike.
const remotePoolCarryTTL = 15 * time.Minute

// remotePoolAccountUsage mirrors the peer's usage-status rows. Only the fields
// the decision needs are modelled; the endpoint carries considerably more.
type remotePoolAccountUsage struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	// AuthMode tells an OAuth row from an API-key one. Only OAuth rows carry
	// a verdict at all: an API-key row is never checked, so its
	// auth_checked:false is not a cut sweep and must not read as one.
	AuthMode string `json:"auth_mode"`
	// SweepCut is the peer's own statement that its sweep ran out of clock
	// before this row was measured this pass. It survives the peer's
	// last-good backfill, which is what makes it the reliable tell: the two
	// below are erased by that backfill.
	SweepCut bool `json:"sweep_cut"`
	// AuthChecked is false when the peer reached NO verdict for this row --
	// its sweep ran out of clock before the credential was looked at. Absent
	// on a peer too old to publish it, which is read as checked.
	AuthChecked *bool `json:"auth_checked"`
	// Error is the peer's own message for a row it could not score. Only its
	// SHAPE is read here: a context error names a sweep that was cut, not a
	// credential that failed.
	Error string `json:"error"`
	// AuthValid is the only usable "can this account serve?" signal on the
	// row. Do NOT filter on `active`: that field marks the peer's CURRENTLY
	// SELECTED account and is absent on all the others, so treating it as
	// "enabled" collapses the whole pool down to whichever account happens to
	// be serving -- reporting the peer's current headroom as its best, and
	// systematically understating a peer we are deciding whether to spill to.
	AuthValid  *bool                `json:"auth_valid"`
	Normalized remotePoolNormalized `json:"normalized"`
}

// remotePoolNormalized is a peer row's normalized usage, and the shape of each
// of its per-pool entries.
type remotePoolNormalized struct {
	Headroom      *float64 `json:"headroom"`
	ShortHeadroom *float64 `json:"short_headroom"`
	// Fresh distinguishes a real usage fetch from a carried seed or an
	// optimistic default, exactly as the local scheduler's Score.Fresh
	// does. A peer that has just restarted reports every account at 1.0
	// until its first fetch lands; believing that would send traffic to
	// a pool whose capacity is unknown, not known-good.
	Fresh *bool `json:"fresh"`
	// The weekly pair, which is what the pace rule is priced on.
	// seconds_to_weekly_reset is the same deadline the local scheduler
	// holds as WeeklyResetAt.
	WeeklyHeadroom      *float64 `json:"weekly_headroom"`
	SecondsToWeeklyRoll *float64 `json:"seconds_to_weekly_reset"`
	// SecondsToHeldWeekResetExpiry is the peer's own statement that the
	// account holds an unclaimed saved reset that clears the general week
	// until then (PATCHES §56, §64), which ranks it at
	// selectacct.ResetHoldPreferenceRatio here. A peer since §64 publishes
	// SecondsToHeldResetExpiry beside it with the same value, and neither key
	// for a grant that clears only the Fable limit. A body with the legacy
	// key alone is from a peer before §64, which holds an account on any
	// grant, and ranks as it did before (heldWeekResetSeconds). The peer
	// publishes the hold at the top level only; a pool entry takes its
	// account's.
	SecondsToHeldWeekResetExpiry *float64 `json:"seconds_to_held_week_reset_expiry"`
	SecondsToHeldResetExpiry     *float64 `json:"seconds_to_held_reset_expiry"`
	// The short pair. ShortWindowSeconds is absent on a peer running an
	// older binary -- the live box does not publish it today -- which
	// means the burn gate cannot be evaluated for that account from here.
	// That is tolerable only because forwards are UNPINNED: the peer
	// applies its own eligibility to its own accounts.
	SecondsToShortReset *float64 `json:"seconds_to_short_reset"`
	ShortWindowSeconds  *float64 `json:"short_window_seconds"`
	// FableWeekSpent is the peer's measured Fable meter, 0..1 (PATCHES §65),
	// which an Opus or Sonnet turn here ranks the account on. Absent -- an
	// untouched meter, or a peer too old to publish one -- is 0 and ranks at
	// 1x. The peer publishes it at the top level only; a pool entry takes its
	// account's.
	FableWeekSpent float64 `json:"fable_week_spent"`
	// ResetChoice is the peer's choice to run this account to its limit for
	// its next saved reset (PATCHES §95), published in real mode only, which
	// this daemon ranks on as it ranks a choice of its own unless
	// SUBROUTER_RESET_LOADING=0. Absent -- not chosen, or a peer before §95 --
	// is none. The peer publishes it at the top level only; a pool entry
	// takes its account's.
	ResetChoice *selectacct.ResetChoice `json:"reset_choice"`
	// WeeklyUnstarted is the peer's reading that the account's weekly window
	// has not started (PATCHES §86): this daemon's clause then gives the
	// account the next new session, unless this daemon marked it for one it
	// sent there. Absent -- a running week, or a peer before §86 -- is false.
	// A pool entry's is its own pool's week, never its account's.
	WeeklyUnstarted bool `json:"weekly_unstarted"`
	// NewSession is the peer's OWN verdict on a new session placed on this
	// account now (PATCHES §31): computed by its selection scheduler, with
	// the live session counts and reserves this daemon cannot see. A
	// pointer so a peer too old to publish it reads as "no verdict" --
	// never as eligible, never as ineligible.
	NewSession *selectacct.NewSessionVerdict `json:"new_session"`
	// Pools carries the same fields per quota pool (claudefable, ...),
	// keyed by selectacct.ModelKey; one level deep.
	Pools map[string]remotePoolNormalized `json:"pools"`
}

// unmeasured reports whether the peer reached no verdict for this row. Such a
// row is skipped AND remembered: a provider whose every row is unmeasured keeps
// its last measured reading (see remotePoolCarryTTL) instead of vanishing.
//
// Three tells, because two generations of peer publish the cut differently: a
// current peer sets sweep_cut (and clears auth_checked when the cut landed
// before the credential was looked at); an older one leaves auth_checked:true
// and auth_valid:false with "context canceled" in error -- which is exactly
// the row that read as "revoked" and emptied a healthy Claude pool from the
// router's point of view.
func (u remotePoolAccountUsage) unmeasured() bool {
	if u.AuthMode != "" && u.AuthMode != string(account.AuthModeOAuth) {
		return false
	}
	if u.SweepCut {
		return true
	}
	if u.AuthChecked != nil && !*u.AuthChecked {
		return true
	}
	return isContextErrorMessage(u.Error)
}

// knownInvalid is the peer's verdict that this credential does not work. It
// is deliberately evaluated AFTER unmeasured: auth_valid:false alone is not a
// verdict when the row also says nobody looked.
func (u remotePoolAccountUsage) knownInvalid() bool {
	return !u.unmeasured() && u.AuthValid != nil && !*u.AuthValid
}

// isContextErrorMessage matches the two context errors by their canonical
// text. The peer publishes a string, and both messages are stable Go
// vocabulary; anything wrapping them keeps the phrase.
func isContextErrorMessage(message string) bool {
	return strings.Contains(message, context.Canceled.Error()) ||
		strings.Contains(message, context.DeadlineExceeded.Error())
}

// remotePoolScoreCarry keeps the last measured BestPeerScore per provider so a
// cut sweep on the peer does not read as an empty peer on the request path.
type remotePoolScoreCarry struct {
	mu      sync.Mutex
	entries map[string]remotePoolScoreEntry
}

type remotePoolScoreEntry struct {
	score selectacct.Score
	at    time.Time
}

func (c *remotePoolScoreCarry) remember(provider string, score selectacct.Score, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]remotePoolScoreEntry{}
	}
	c.entries[provider] = remotePoolScoreEntry{score: score, at: now}
}

// carried returns the last measured score for provider while it is younger
// than remotePoolCarryTTL.
func (c *remotePoolScoreCarry) carried(provider string, now time.Time) (selectacct.Score, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[provider]
	if !ok || now.Sub(entry.at) >= remotePoolCarryTTL {
		return selectacct.Score{}, false
	}
	return entry.score, true
}

// effectiveHeadroom is the lower of the two windows, matching what
// usableForNewSession gates on locally. A nil field means the peer did not
// report that window; treat it as unknown rather than as zero or as full.
func (u remotePoolAccountUsage) effectiveHeadroom() (float64, bool) {
	if u.Normalized.Headroom == nil || u.Normalized.ShortHeadroom == nil {
		return 0, false
	}
	if u.Normalized.Fresh != nil && !*u.Normalized.Fresh {
		return 0, false
	}
	if *u.Normalized.Headroom < *u.Normalized.ShortHeadroom {
		return *u.Normalized.Headroom, true
	}
	return *u.Normalized.ShortHeadroom, true
}

// weeklyPace mirrors selectacct.Score.weeklyPace: remaining weekly budget per
// second until the roll. Absent weekly fields mean the peer published no
// weekly window for this account (Codex, or a carried seed), not a pace of
// zero -- those are very different answers to "where should this request go".
func (u remotePoolAccountUsage) weeklyPace() (float64, bool) {
	if u.Normalized.WeeklyHeadroom == nil || u.Normalized.SecondsToWeeklyRoll == nil {
		return 0, false
	}
	if *u.Normalized.SecondsToWeeklyRoll <= 0 {
		return 0, false
	}
	return *u.Normalized.WeeklyHeadroom / *u.Normalized.SecondsToWeeklyRoll, true
}

// score converts a peer row into the same currency selection uses locally.
//
// ShortWindowSeconds is deliberately absent: a peer running an older binary
// does not publish it, so the burn gate cannot be evaluated here. That is
// acceptable precisely because forwards are UNPINNED -- the peer applies its
// own eligibility, its own fable reservation (when on, PATCHES §57) and its
// own machine policy to its own accounts. These scores decide WHERE a request goes, not whether the peer
// may serve it.
func (u remotePoolAccountUsage) score(now time.Time) (selectacct.Score, bool) {
	return u.Normalized.score(u.ID, now)
}

// score is remotePoolAccountUsage.score for one normalized block -- the row's
// top level or one of its pool entries -- so a pool entry is priced exactly
// like the account it belongs to.
//
// The published verdict rides along (PATCHES §31): with it, routability,
// burndown and the endgame cap are the peer's own answer (Score.PeerVerdict),
// and LiveSessions is the peer's live count, so the session spread discounts a
// busy peer account exactly as the peer itself does. Without it (an older
// peer) nothing changes from before.
func (n remotePoolNormalized) score(id string, now time.Time) (selectacct.Score, bool) {
	if n.Headroom == nil || n.ShortHeadroom == nil {
		return selectacct.Score{}, false
	}
	if n.Fresh != nil && !*n.Fresh {
		return selectacct.Score{}, false
	}
	score := selectacct.Score{
		AccountID:     id,
		Remote:        true,
		Headroom:      *n.Headroom,
		ShortHeadroom: *n.ShortHeadroom,
		Fresh:         true,
	}
	if n.SecondsToShortReset != nil {
		score.ShortResetAfterSeconds = int64(*n.SecondsToShortReset)
	}
	if n.ShortWindowSeconds != nil {
		score.ShortWindowSeconds = int64(*n.ShortWindowSeconds)
	}
	if n.WeeklyHeadroom != nil && n.SecondsToWeeklyRoll != nil && *n.SecondsToWeeklyRoll > 0 {
		score.WeeklyHeadroom = *n.WeeklyHeadroom
		score.WeeklyResetAt = now.Add(time.Duration(*n.SecondsToWeeklyRoll) * time.Second)
	}
	// Anchored where the weekly roll is, so a body's hold ends when the
	// peer's does. It feeds the 2x only: early burndown is the owner's call,
	// carried in its verdict.
	if seconds := heldWeekResetSeconds(n.SecondsToHeldWeekResetExpiry, n.SecondsToHeldResetExpiry); seconds != nil && *seconds > 0 {
		score.ResetHeldUntil = now.Add(time.Duration(*seconds * float64(time.Second)))
	}
	if n.NewSession != nil {
		verdict := *n.NewSession
		score.PeerVerdict = &verdict
		score.LiveSessions = verdict.LiveSessions
	}
	score.FableSpent = min(max(n.FableWeekSpent, 0), 1)
	score.WeeklyUnstarted = n.WeeklyUnstarted
	if n.ResetChoice != nil && n.ResetChoice.Known() && ResetLoading() {
		score.ResetChoice = *n.ResetChoice
	}
	return score, true
}

// BestPeerScore ranks the peer's accounts for a provider with the SAME
// comparator selection uses, and returns the winner.
//
// Ranking rather than max-headroom: the account holding the most budget is
// routinely not the one whose budget expires soonest, and pace is what decides
// that. Returns false when the peer could not be measured -- which is not the
// same as the peer being empty.
func (p *RemotePool) BestPeerScore(ctx context.Context, provider string) (selectacct.Score, bool) {
	score, _, ok := p.bestPeerScore(ctx, provider)
	return score, ok
}

// bestPeerScore is BestPeerScore with the carry made visible: the middle
// result is true when the score is the last measured reading standing in for
// a peer that did not get to measure this provider.
func (p *RemotePool) bestPeerScore(ctx context.Context, provider string) (selectacct.Score, bool, bool) {
	if p == nil {
		return selectacct.Score{}, false, false
	}
	return p.bestPeerScoreFrom(p.AdminGet(ctx, remotePoolUsagePath), provider)
}

// bestPeerScoreFrom is bestPeerScore over a snapshot the caller already holds,
// so the cross-pool decision ranks a legacy peer from the same budgeted read
// it uses for every other peer (usageForDecision) instead of a blocking one.
func (p *RemotePool) bestPeerScoreFrom(snapshot AdminSnapshot, provider string) (selectacct.Score, bool, bool) {
	if p == nil || len(snapshot.Body) == 0 {
		return selectacct.Score{}, false, false
	}
	var rows []remotePoolAccountUsage
	if err := json.Unmarshal(snapshot.Body, &rows); err != nil {
		return selectacct.Score{}, false, false
	}
	now := time.Now()
	var scores []selectacct.Score
	var candidates []account.Account
	unmeasured := false
	verdictAgainst := map[string]bool{}
	for _, row := range rows {
		if row.Provider != provider {
			continue
		}
		if row.unmeasured() {
			unmeasured = true
			continue
		}
		if row.knownInvalid() {
			verdictAgainst[row.ID] = true
			continue
		}
		score, ok := row.score(now)
		if !ok {
			continue
		}
		scores = append(scores, score)
		candidates = append(candidates, account.Account{ID: row.ID, AuthMode: account.AuthModeOAuth})
	}
	if len(candidates) == 0 {
		// Nothing scored. If that is because the peer did not get to look,
		// the last reading it DID reach stands in -- unless this body carries
		// a verdict against that very account; if every row was a real
		// verdict (revoked, exhausted, absent) the pool is measured-empty.
		if unmeasured {
			if carried, ok := p.peerScores.carried(provider, now); ok && !verdictAgainst[carried.AccountID] {
				return carried, true, true
			}
		}
		return selectacct.Score{}, false, false
	}
	winner, err := selectacct.NewScheduler(scores).Pick(candidates)
	if err != nil {
		return selectacct.Score{}, false, false
	}
	for _, score := range scores {
		if score.AccountID == winner.ID {
			// Stamped with when the BODY was measured, not with now: AdminGet
			// retains its last good body across peer failures, and a reading
			// re-derived from an hour-old body is an hour old.
			p.peerScores.remember(provider, score, snapshot.At)
			return score, false, true
		}
	}
	return selectacct.Score{}, false, false
}

// BestCapacity reports the peer's healthiest account for a provider. The bool
// is false when the peer could not be measured -- unreachable, token refused,
// or no scored account for that provider. Callers must not read a false as
// "empty peer": an unmeasured pool is unknown, not dry.
func (p *RemotePool) BestCapacity(provider string) (remotePoolProviderCapacity, bool) {
	if p == nil {
		return remotePoolProviderCapacity{}, false
	}
	p.capacity.mu.Lock()
	fresh := time.Since(p.capacity.at) < remotePoolCapacityTTL
	if fresh || p.capacity.inflight {
		best, ok := p.capacity.best[provider]
		measured := p.capacity.measured
		p.capacity.mu.Unlock()
		return best, measured && ok
	}
	p.capacity.inflight = true
	p.capacity.mu.Unlock()

	best, unmeasured, measured := p.probeCapacity()

	p.capacity.mu.Lock()
	now := time.Now()
	p.capacity.at = now
	p.capacity.inflight = false
	if measured {
		// A provider the peer did not get to measure this time keeps its
		// previous reading while that is younger than remotePoolCarryTTL.
		// Only providers with NO scored row are eligible: one measured row
		// is a measurement, and it replaces the carry.
		for provider, previous := range p.capacity.best {
			if _, scored := best[provider]; scored || !unmeasured[provider] {
				continue
			}
			if now.Sub(previous.MeasuredAt) < remotePoolCarryTTL {
				best[provider] = previous
			}
		}
		p.capacity.best = best
		p.capacity.measured = true
	}
	value, ok := p.capacity.best[provider]
	got := p.capacity.measured && ok
	p.capacity.mu.Unlock()
	return value, got
}

// probeCapacity fetches and reduces the peer's snapshot. Any failure returns
// measured=false, which keeps the previous reading rather than replacing it
// with zeros -- a transient tailnet blip must not read as a dry peer.
func (p *RemotePool) probeCapacity() (map[string]remotePoolProviderCapacity, map[string]bool, bool) {
	// Bounded explicitly. p.client has no total timeout (PATCHES §33), and
	// remotePoolDialTimeout only covers CONNECTING -- so a half-open tailnet
	// connection that dials fine and then goes silent stalled this probe, and
	// the proxied request waiting on it, for the old five-minute client
	// budget; without this it would stall for good. Its own context, not the
	// request's: the cached result outlives the request that populated it.
	ctx, cancel := context.WithTimeout(context.Background(), remotePoolAdminTimeout)
	defer cancel()
	target := *p.URL
	target.Path = strings.TrimSuffix(target.Path, "/") + remotePoolUsagePath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, nil, false
	}
	if p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, false
	}
	var rows []remotePoolAccountUsage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rows); err != nil {
		return nil, nil, false
	}
	now := time.Now()
	best := map[string]remotePoolProviderCapacity{}
	// unmeasured names the providers with at least one row the peer reached no
	// verdict on; capacity() uses it to decide whether an absent provider is
	// measured-empty or merely unread.
	unmeasured := map[string]bool{}
	for _, row := range rows {
		if row.unmeasured() {
			unmeasured[row.Provider] = true
			continue
		}
		if row.knownInvalid() {
			continue
		}
		headroom, ok := row.effectiveHeadroom()
		if !ok {
			continue
		}
		pace, paceOK := row.weeklyPace()
		current, seen := best[row.Provider]
		if !seen || headroom > current.Headroom {
			current.Headroom = headroom
		}
		// Pace and headroom are maximised INDEPENDENTLY: the account holding
		// the most budget is routinely not the one whose budget rolls soonest,
		// and pinning pace to whichever account won on headroom would report
		// the pool as slower than it is.
		if paceOK && (!current.PaceOK || pace > current.Pace) {
			current.Pace, current.PaceOK = pace, true
		}
		// Minimised, not maximised: the pool's deadline is its SOONEST one,
		// because that is the budget about to be forfeited. Independent of both
		// terms above for the same reason they are independent of each other.
		if row.Normalized.SecondsToWeeklyRoll != nil && *row.Normalized.SecondsToWeeklyRoll > 0 {
			if !current.RefreshOK || *row.Normalized.SecondsToWeeklyRoll < current.SecondsToRefresh {
				current.SecondsToRefresh, current.RefreshOK = *row.Normalized.SecondsToWeeklyRoll, true
			}
		}
		current.MeasuredAt = now
		best[row.Provider] = current
	}
	return best, unmeasured, true
}

// parseAllowFrom accepts a comma-separated list of CIDRs and bare IPs.
// "*" (or an empty string) means open. An entry that does not parse is dropped
// rather than silently widening the filter.
func parseAllowFrom(raw string) []*net.IPNet {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "*" {
		return nil
	}
	var nets []*net.IPNet
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if _, block, err := net.ParseCIDR(entry); err == nil {
			nets = append(nets, block)
			continue
		}
		if ip := net.ParseIP(entry); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
		}
	}
	return nets
}

// allows reports whether a caller at this address may use the peer.
func (p *RemotePool) allows(remoteIP string) bool {
	if p == nil {
		return false
	}
	if len(p.AllowFrom) == 0 {
		return true // open
	}
	ip := net.ParseIP(strings.TrimSpace(remoteIP))
	if ip == nil {
		// An unparseable source cannot be shown to be permitted, and this gate
		// guards someone's subscription. Fail closed.
		return false
	}
	for _, block := range p.AllowFrom {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

// NewRemotePool builds a peer from a base URL, or returns nil when unset.
// allowFrom is a comma-separated list of CIDRs/IPs; "" or "*" leaves it open.
func NewRemotePool(rawURL, token, allowFrom string) *RemotePool {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil
	}
	// Loopback is deliberately allowed. Refusing it outright looks like a
	// self-target guard but is the wrong test: two daemons on one host with
	// different ports are a legitimate setup, and it would also reject every
	// httptest peer. The loop hazard is handled where it actually lives -- the
	// hop counter. A daemon misconfigured to forward to its own listener sends
	// the request to itself with hop=1, refuses to forward it a second time,
	// re-runs local selection, and returns the ordinary 503. It terminates.
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return &RemotePool{
		URL:       parsed,
		Token:     strings.TrimSpace(token),
		AllowFrom: parseAllowFrom(allowFrom),
		wsDialer:  newPeerWebSocketDialer(),
		// No Timeout: it would also bound reading the body, which is how every
		// forwarded turn was cut at 5 minutes. Each phase carries its own bound
		// instead -- dial, TLS, a send that stops moving (peerWriteStallConn)
		// and time-to-headers here; the stream watchdog on a forwarded answer
		// (streamIdle); remotePoolAdminTimeout on the control reads.
		streamIdle: remotePoolStreamIdleTimeout,
		client: &http.Client{
			Transport: &http.Transport{
				DialContext:           peerWriteStallDial((&net.Dialer{Timeout: remotePoolDialTimeout}).DialContext, remotePoolWriteStallTimeout),
				TLSHandshakeTimeout:   remotePoolDialTimeout,
				ResponseHeaderTimeout: remotePoolResponseHeaderTimeout,
				MaxIdleConns:          remotePoolMaxIdleConns,
				MaxIdleConnsPerHost:   remotePoolMaxIdleConns,
				IdleConnTimeout:       remotePoolIdleConnTimeout,
			},
			// Never follow a redirect into somewhere unintended.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (p *RemotePool) name() string {
	if p == nil {
		return ""
	}
	if p.Name != "" {
		return p.Name
	}
	if p.URL == nil {
		return ""
	}
	return p.URL.Host
}

// remotePoolBreaker bounds what an unreachable peer costs.
type remotePoolBreaker struct {
	mu        sync.Mutex
	failures  int
	openUntil time.Time
	// lastFailureAt is when the last COUNTED failure was seen. A request sent
	// before it was in flight during that same blip, so its failure is the
	// same event and does not count again.
	lastFailureAt time.Time
	// seriesStarted is when the first failure of the outage an opening judges
	// was sent (PATCHES §54): the first failure counted since the last
	// success, unless that one is more than a cooldown older than the next on
	// the wall clock.
	seriesStarted time.Time
	// unattributed is set by an opening that kept the holds. It keeps that
	// opening's series for the strikes that re-decide it, however long after
	// the cooldown they come, and makes a re-opening that would end the holds
	// need two strikes sent since this machine came back, as a local failure
	// seen since the process started does.
	unattributed bool
	// lastStrikeStarted is when the last counted failure's request was sent.
	lastStrikeStarted time.Time
	// lastAnswerAt is the peer's last answer of any kind: response headers
	// on a forward or a websocket dial, whatever the status, or a usage body
	// (PATCHES §67).
	lastAnswerAt time.Time
	// quietSince is when the silence began: the first counted strike since
	// that answer, or the first after a gap over remotePoolQuietGapMax; zero
	// while the peer answers. Both are wall-clock times, so a sleep shows as a
	// gap.
	quietSince time.Time
	// down: the last opening found the peer down, and it has not answered
	// since (PATCHES §67).
	down bool
}

// sentSinceBack reports whether a request sent at started left after this
// machine came back: upSince is set and not later than the send.
func sentSinceBack(started time.Time, egress egressView) bool {
	return !egress.upSince.IsZero() && !started.Before(egress.upSince)
}

func (b *remotePoolBreaker) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return now.After(b.openUntil)
}

// success records the peer answering a forward or a websocket dial.
func (b *remotePoolBreaker) success(at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.openUntil = time.Time{}
	b.seriesStarted = time.Time{}
	b.unattributed = false
	b.answeredLocked(at)
}

// noteAnswer is the peer answering where no forward is judged: a usage
// body. It restarts the grace and leaves the strike count and the cooldown
// alone.
func (b *remotePoolBreaker) noteAnswer(at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.answeredLocked(at)
}

func (b *remotePoolBreaker) answeredLocked(at time.Time) {
	if at = at.Round(0); at.After(b.lastAnswerAt) {
		b.lastAnswerAt = at
	}
	b.quietSince = time.Time{}
	b.down = false
}

// foundDown reports that the breaker's last opening found the peer down and
// the peer has answered nothing since: a chat held there has nothing to wait
// for (PATCHES §67).
func (b *remotePoolBreaker) foundDown() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.down
}

// quietFor is how long the peer has been silent at now, as this machine
// observed it: since the latest of the first strike after its last answer,
// that answer, and this machine coming back (egress upSince), so time this
// machine spent offline never counts (PATCHES §67).
func (b *remotePoolBreaker) quietFor(now time.Time, egress egressView) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.quietForLocked(now, egress)
}

func (b *remotePoolBreaker) quietForLocked(now time.Time, egress egressView) time.Duration {
	from := b.quietSince
	for _, t := range []time.Time{b.lastAnswerAt, egress.upSince} {
		if t.After(from) {
			from = t
		}
	}
	if from.IsZero() {
		return 0
	}
	return now.Round(0).Sub(from)
}

// failure records a failed request to the peer, sent at started and seen to
// fail at failedAt with err, and reports whether it opened the breaker and,
// if so, whether the peer is DOWN: whether the outage is attributable to the
// peer rather than to this machine's own network (whyNotThePeers, with egress
// read when the failure is seen). An opening that keeps the holds says why
// (keptWhy: no_route, no_answer, network_back_mid_strike,
// one_strike_since_back, no_new_connection or within_grace).
//
// One failure per blip, not per request: a request sent before the last
// counted failure was in flight when that blip hit, so it fails for the same
// reason and is not a second strike. Requests to one peer travel in bursts --
// a spilling turn and every request following its pending claim go together
// -- and counting each of them opened the breaker on a single blip, which
// evicts every session held on the peer. A request sent AFTER the last counted
// failure that fails too is a second strike.
//
// An opening this machine cannot attribute to the peer still opens (the cost
// is bounded as before) but evicts nothing, and leaves the breaker one strike
// from re-opening, so the next failure after the cooldown re-opens it at once
// and that turn is served locally too. A peer success or an opening that ends
// the holds clears this.
//
// An opening ends the holds only when it is attributable AND it and the
// strike before it were both sent since this machine came back
// (sentSinceBack): one failure right after recovery is what a cold tailnet
// link or a dead pooled keep-alive looks like, and it keeps them (PATCHES
// §54). That applies once this machine has been seen offline since the
// process started (a local failure on egress) or an opening has kept the
// holds. A process that has seen neither has come back from nothing, so a
// restarted laptop that is online finds a dead box at the second strike.
//
// A counted no-route strike, and an opening kept for no_answer, note a local
// failure on egress at failedAt: this machine may have been offline then, so
// it comes back only at its next local answer. Otherwise an upSince from
// before a drop nothing local saw fail would count that strike, or one sent
// before the network returned, as sent since recovery.
//
// The outage an opening judges starts at the first failure of its series. A
// strike left over from an earlier blip, more than a cooldown before this
// one on the wall clock with no peer success since, does not date it: the
// box's sessions may have idled while this machine kept answering or slept,
// and an answer from just before this machine's own network dropped is not
// one "during" the outage. The series then restarts at this strike, and the
// count stands, so the opening comes when it did. An unattributed opening's
// series is kept for the strikes that re-decide it, which come a cooldown or
// more after it.
//
// With a grace, an opening that rule would blame on the peer does not find it
// down (within_grace) until the peer has been silent for the grace (quietFor;
// PATCHES §67). A gateway that refuses new connections while
// open ones keep working looks like a dead peer to those rules, because
// this machine's own pooled answers count as it being online, and the
// peer's answers on its open connections restart the grace instead.
func (b *remotePoolBreaker) failure(started, failedAt time.Time, err error, egress *localEgress, grace time.Duration) (opened, peerDown bool, keptWhy string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if started.Before(b.lastFailureAt) {
		return false, false, ""
	}
	// Round(0): the wall clock. darwin's monotonic clock stops while the
	// machine sleeps, so a strike from before a sleep would read as seconds
	// old on waking and date the outage after it.
	if b.failures == 0 || (!b.unattributed && started.Round(0).Sub(b.lastFailureAt.Round(0)) > remotePoolCooldown) {
		b.seriesStarted = started
	}
	previous := b.lastStrikeStarted
	if b.quietSince.IsZero() || started.Round(0).Sub(previous.Round(0)) > remotePoolQuietGapMax {
		b.quietSince = started.Round(0)
	}
	b.lastStrikeStarted = started
	b.lastFailureAt = failedAt
	b.failures++
	if noRoute(err) {
		egress.noteFailure(failedAt)
	}
	if b.failures < remotePoolFailuresToOpen {
		return false, false, ""
	}
	b.openUntil = failedAt.Add(remotePoolCooldown)
	view := egress.view()
	why := whyNotThePeers(err, view, started, b.seriesStarted)
	if why == "" && (b.unattributed || !view.lastFailure.IsZero()) && !sentSinceBack(previous, view) {
		why = keptOneStrikeSinceBack
	}
	if why == "" && grace > 0 && b.quietForLocked(failedAt, view) < grace {
		why = keptWithinGrace
	}
	if why != "" {
		if why == keptNoAnswer {
			egress.noteFailure(failedAt)
		}
		b.failures = remotePoolFailuresToOpen - 1
		b.unattributed = true
		b.down = false
		return true, false, why
	}
	b.failures = 0
	b.unattributed = false
	b.seriesStarted = time.Time{}
	b.down = true
	return true, true, ""
}

// PersistHolds keeps this pool's session holds across a daemon restart.
//
// Holds used to be memory-only, so every restart (every deploy) sent each
// session held on the peer back to its local placeholder account: a warm
// conversation moved and cold-cached with nothing having been evicted. Local
// assignments already persist (sessions.json); this is the peer half.
//
// Written at most every holdsFlushInterval and only when something changed, so
// the hot path never touches disk; a crash loses at most that much. Nil-safe:
// a machine with no peer returns nil unchanged.
//
// When generation ends, the holds are written once more and never again.
func (p *RemotePool) PersistHolds(generation context.Context, path string) *RemotePool {
	return p.persistHoldsEvery(generation, path, holdsFlushInterval)
}

func (p *RemotePool) persistHoldsEvery(generation context.Context, path string, interval time.Duration) *RemotePool {
	if p == nil || path == "" {
		return p
	}
	p.delegations.loadFor(path, time.Now(), p.name())
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				p.delegations.flush(path, time.Now())
			case <-generation.Done():
				p.delegations.flush(path, time.Now())
				return
			}
		}
	}()
	return p
}

const holdsFlushInterval = 15 * time.Second

func (d *remotePoolDelegations) load(path string, now time.Time) {
	d.loadFor(path, now, "")
}

// loadFor restores the live holds in path that belong to peer: an entry naming
// a different peer (the file of a peer since renamed or re-pointed) is dropped
// rather than attributed to this one. An entry with no peer name predates the
// field and is kept; an empty peer keeps everything.
func (d *remotePoolDelegations) loadFor(path string, now time.Time, peer string) {
	body, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var stored map[string]remotePoolDelegation
	if json.Unmarshal(body, &stored) != nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.entries == nil {
		d.entries = map[string]remotePoolDelegation{}
	}
	for key, entry := range stored {
		if entry.Peer != "" && peer != "" && entry.Peer != peer {
			continue
		}
		if now.Before(entry.Until) {
			d.entries[key] = entry
		}
	}
}

// flush writes the live holds if anything changed since the last write.
func (d *remotePoolDelegations) flush(path string, now time.Time) {
	d.mu.Lock()
	if !d.dirty {
		d.mu.Unlock()
		return
	}
	live := make(map[string]remotePoolDelegation, len(d.entries))
	for key, entry := range d.entries {
		// A pending claim is a forward in flight, not a session the peer has
		// served; it is written once the peer confirms it.
		if now.Before(entry.Until) && entry.claim == 0 {
			live[key] = entry
		}
	}
	d.dirty = false
	d.mu.Unlock()
	body, err := json.Marshal(live)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, body, 0o600) != nil {
		d.markDirty()
		return
	}
	if os.Rename(tmp, path) != nil {
		d.markDirty()
	}
}

func (d *remotePoolDelegations) markDirty() {
	d.mu.Lock()
	d.dirty = true
	d.mu.Unlock()
}

// remotePoolDelegations remembers which sessions are currently being served by
// the peer.
//
// This exists for prompt-cache warmth, which is per ACCOUNT, not per pool.
// Without it, every turn re-runs local selection, so the moment a local 5h
// window rolls mid-conversation the session snaps back to a local account and
// cold-caches. Warmth on the peer's side needs nothing extra: forwarding a
// stable X-Subrouter-Session lets the peer's own sticky assignment keep the
// conversation on one of its accounts.
type remotePoolDelegations struct {
	mu      sync.Mutex
	entries map[string]remotePoolDelegation
	// dirty is set by every change so PersistHolds writes only when needed.
	dirty bool
	// lastClaim numbers pre-claims so a forward can drop only its own.
	lastClaim uint64
}

// remotePoolDelegation records not just THAT a session is delegated but WHERE
// it landed. Without the account, the only answer this daemon could give about
// a delegated session was its local sticky assignment -- the account selection
// picked before the request left -- which is not the account that served it.
// `srq serving` reported that placeholder as fact.
type remotePoolDelegation struct {
	Until time.Time
	// Account is the PEER's account, as named by its X-Subrouter-Account
	// response header. Empty when the peer is older than that header, which is
	// honestly "somewhere on the peer" rather than a wrong local name.
	Account string
	Peer    string
	// Targeted: the peer served this exact account when it was targeted at
	// it. A restart loses the peer's usage body, and with no body the peer
	// cannot be known to honour targets (Deviation X1), so a held turn used
	// to go untargeted until the first body arrived. A hold the peer has
	// already honoured a target for keeps its target (PATCHES §54). Persisted;
	// a file from before the field reads false, which is the old behaviour.
	Targeted bool `json:",omitempty"`
	// Refugee: the session was placed here because the box its cache was on
	// was found down (placeRefugee, PATCHES §67). Persisted with the hold, so
	// a restart keeps the account's refugee count.
	Refugee *session.Refugee `json:",omitempty"`
	// AnsweredAt is when one of the session's model turns was last answered
	// (Server.noteAnswered), the stranded give-up's clock: a chat a box holds
	// has no local assignment to keep it, so the hold carries it to the next
	// worker (PATCHES §67).
	AnsweredAt time.Time `json:",omitzero"`
	// claim is non-zero while the entry is only a forward's pre-claim (see
	// claim): the peer has not served the session yet. A set or touch -- the
	// peer serving it -- confirms the entry and zeroes this. Unexported, so
	// never persisted.
	claim uint64
}

// holdUntil is a hold's deadline on the WALL clock. now.Add keeps now's
// monotonic reading, and Go compares two times by that reading alone when both
// carry one. On darwin the monotonic clock stops while the machine sleeps, so
// a hold set before a two-hour lid-close was still live on waking and sent the
// session to a peer account whose cache had gone cold. Stripped, every
// comparison with Until reads the wall clock, and the idle bound includes
// sleep, as it always included the time the daemon was down across a restart
// (peer-holds.json stores the wall-clock deadline; a restart ends no hold).
// PATCHES §54.
func holdUntil(now time.Time, ttl time.Duration) time.Time {
	return now.Add(ttl).Round(0)
}

func (d *remotePoolDelegations) active(key string, now time.Time) bool {
	_, ok := d.lookup(key, now)
	return ok
}

// lookup returns the live delegation for a session, so callers can report
// where it is actually being served.
func (d *remotePoolDelegations) lookup(key string, now time.Time) (remotePoolDelegation, bool) {
	if key == "" {
		return remotePoolDelegation{}, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	entry, ok := d.entries[key]
	if !ok {
		return remotePoolDelegation{}, false
	}
	if now.After(entry.Until) {
		delete(d.entries, key)
		return remotePoolDelegation{}, false
	}
	return entry, true
}

func (d *remotePoolDelegations) set(key string, now time.Time, ttl time.Duration, account, peer string) {
	d.setServed(key, now, ttl, account, peer, false)
}

// setServed is set for a turn the peer served, targeted when the forward named
// account and the peer served it there. An untargeted serve on the same
// account keeps the hold's Targeted bit: an untargeted forward is not evidence
// that the peer stopped honouring targets.
func (d *remotePoolDelegations) setServed(key string, now time.Time, ttl time.Duration, account, peer string, targeted bool) {
	if key == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.setServedLocked(key, now, ttl, account, peer, targeted)
}

// setServedIfHeld is setServed for a TARGETED turn the peer served on
// account: it writes only while the session's live entry still names that
// account (or names none, a hold from a peer too old to say). A turn that was
// in flight while another turn of its session moved the hold elsewhere, or
// ended it, must not put the session back where it was: the next turn would
// go there and be refused again (a subagent's turn answered after the main
// thread had moved the session off a Fable-spent account; PATCHES §71.4).
// It reports whether it wrote.
func (d *remotePoolDelegations) setServedIfHeld(key string, now time.Time, ttl time.Duration, account, peer string) bool {
	if key == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	entry, ok := d.entries[key]
	if !ok || now.After(entry.Until) || !holdNames(entry, account) {
		return false
	}
	d.setServedLocked(key, now, ttl, account, peer, true)
	return true
}

// clearIfHeldOn ends the session's hold only while it names account (or
// none), and returns the entry it removed: a targeted turn refused on account
// ends that hold, never a newer one another turn of the session has set
// elsewhere since this turn left.
func (d *remotePoolDelegations) clearIfHeldOn(key, account string) (remotePoolDelegation, bool) {
	if key == "" {
		return remotePoolDelegation{}, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	entry, ok := d.entries[key]
	if !ok || !holdNames(entry, account) {
		return remotePoolDelegation{}, false
	}
	delete(d.entries, key)
	d.dirty = true
	return entry, true
}

// holdNames reports a hold on account, or one that names no account.
func holdNames(entry remotePoolDelegation, account string) bool {
	return entry.Account == "" || strings.EqualFold(entry.Account, account)
}

func (d *remotePoolDelegations) setServedLocked(key string, now time.Time, ttl time.Duration, account, peer string, targeted bool) {
	if d.entries == nil {
		d.entries = map[string]remotePoolDelegation{}
	}
	// Prune while we hold the lock; nothing else sweeps this map.
	for existing, entry := range d.entries {
		if now.After(entry.Until) {
			delete(d.entries, existing)
		}
	}
	// Keep the last known account when a refresh cannot name one, rather than
	// downgrading a precise answer to a vague one.
	if prev, ok := d.entries[key]; ok {
		if account == "" {
			account = prev.Account
		}
		if !targeted && prev.Targeted && strings.EqualFold(prev.Account, account) {
			targeted = true
		}
	}
	entry := remotePoolDelegation{Until: holdUntil(now, ttl), Account: account, Peer: peer, Targeted: targeted}
	if prev, ok := d.entries[key]; ok {
		entry.AnsweredAt = prev.AnsweredAt
		if strings.EqualFold(prev.Account, account) {
			entry.Refugee = prev.Refugee
		}
	}
	d.entries[key] = entry
	d.dirty = true
}

// answered records at as the session's last answered turn on its live hold,
// pending claim included, and creates none.
func (d *remotePoolDelegations) answered(key string, at time.Time) {
	if key == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	entry, ok := d.entries[key]
	if !ok || time.Now().After(entry.Until) || !at.After(entry.AnsweredAt) {
		return
	}
	entry.AnsweredAt = at.Round(0)
	d.entries[key] = entry
	d.dirty = true
}

// claimRefugee is claimTarget for a refugee (placeRefugee): it claims the
// session for account, marked, only while account holds fewer than slots
// live refugees other than this session, and no more than counted, the
// refugees the caller counted there when it chose account. The count and the
// claim happen under one lock, so two placements cannot both take the last
// slot; a pending refugee claim counts, which is what reserves the slot. An
// account that gained a refugee since the caller counted may no longer have
// the most free slots, so the caller counts again. held is the account's
// refugees with this one, or without it when not claimed.
func (d *remotePoolDelegations) claimRefugee(key string, now time.Time, ttl time.Duration, peer, account string, mark session.Refugee, slots, counted int) (id uint64, held int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	held = d.refugeesOnLocked(key, account, now)
	if held >= slots || held > counted {
		return 0, held
	}
	if d.entries == nil {
		d.entries = map[string]remotePoolDelegation{}
	}
	d.lastClaim++
	d.entries[key] = remotePoolDelegation{Until: holdUntil(now, ttl), Account: account, Peer: peer, Refugee: &mark, claim: d.lastClaim}
	d.dirty = true
	return d.lastClaim, held + 1
}

// refugeesOn is how many live refugees other than the session key account
// holds: the count claimRefugee checks, read without claiming.
func (d *remotePoolDelegations) refugeesOn(key, account string, now time.Time) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.refugeesOnLocked(key, account, now)
}

func (d *remotePoolDelegations) refugeesOnLocked(key, account string, now time.Time) int {
	held := 0
	for other, entry := range d.entries {
		if other != key && entry.Refugee != nil && strings.EqualFold(entry.Account, account) && !now.After(entry.Until) {
			held++
		}
	}
	return held
}

// restore puts back a hold a refugee's failed placement cleared, unless the
// session is held here again by then (PATCHES §67, D2).
func (d *remotePoolDelegations) restore(key string, hold remotePoolDelegation) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.entries[key]; ok {
		return
	}
	if d.entries == nil {
		d.entries = map[string]remotePoolDelegation{}
	}
	d.entries[key] = hold
	d.dirty = true
}

// claim holds a session for the peer BEFORE a forward leaves, so a concurrent
// request for the same session takes the delegated branch and follows it there
// instead of reading the fresh pick's local pin as warm and splitting the
// conversation across two pools.
//
// It only ever inserts: when a live hold already exists it changes nothing and
// returns 0, so an existing hold keeps its own semantics. Otherwise it inserts
// a pending entry and returns a non-zero id that only this call holds -- the
// forward passes it to release when the peer did not serve. Check and insert
// happen under one lock, so "created" can never mean a hold someone else made.
func (d *remotePoolDelegations) claim(key string, now time.Time, ttl time.Duration, peer string) uint64 {
	return d.claimTarget(key, now, ttl, peer, "")
}

// claimTarget is claim naming the peer account the forward targets, so a
// concurrent request of the session that follows the pending claim targets the
// SAME account rather than whichever one the peer would pick (PATCHES §31).
func (d *remotePoolDelegations) claimTarget(key string, now time.Time, ttl time.Duration, peer, account string) uint64 {
	if key == "" {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if entry, ok := d.entries[key]; ok && !now.After(entry.Until) {
		return 0
	}
	if d.entries == nil {
		d.entries = map[string]remotePoolDelegation{}
	}
	d.lastClaim++
	d.entries[key] = remotePoolDelegation{Until: holdUntil(now, ttl), Account: account, Peer: peer, claim: d.lastClaim}
	d.dirty = true
	return d.lastClaim
}

// release drops a pre-claim the peer never served: there is no warmth there to
// protect. It removes the entry only while it is still exactly that pending
// claim. Once a request the peer served has set or touched it, or it was
// cleared and claimed again by another forward, it is not this call's to drop.
func (d *remotePoolDelegations) release(key string, id uint64) {
	if key == "" || id == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if entry, ok := d.entries[key]; ok && entry.claim == id {
		delete(d.entries, key)
		d.dirty = true
	}
}

// touch restarts a live delegation's idle clock without creating one, so a
// turn that ends after its delegation was cleared (a failed forward, a peer
// decline) cannot resurrect it.
func (d *remotePoolDelegations) touch(key string, now time.Time, ttl time.Duration) {
	if key == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	entry, ok := d.entries[key]
	if !ok || now.After(entry.Until) {
		return
	}
	entry.Until = holdUntil(now, ttl)
	// Only a served turn touches, so a pending claim is now confirmed.
	entry.claim = 0
	d.entries[key] = entry
	d.dirty = true
}

// delegationTouchingBody restarts the delegation's idle clock when the
// forwarded response is closed. The local idle clock is stamped when a turn
// ENDS (ActiveSessions.Begin's release), so the peer's must be too: stamped at
// send time, a long streaming turn would spend its own duration out of the
// idle budget and leave the peer hold shorter than the local one.
//
// It also touches while the answer streams, at most once per
// remotePoolWebSocketTouchInterval, as the socket relay does for its traffic.
// The hold was stamped when the turn was sent, and since §33 lifted the
// 5-minute cap an answer can outlive the hold's TTL: without this the hold
// lapses mid-answer, the touch on Close cannot revive it, and the session's
// next turn re-ranks as cold.
type delegationTouchingBody struct {
	io.ReadCloser
	once      sync.Once
	touch     func()
	lastTouch time.Time
}

// Read is only ever called by the one relay copying the answer, so lastTouch
// needs no lock.
func (b *delegationTouchingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		if now := time.Now(); now.Sub(b.lastTouch) >= remotePoolWebSocketTouchInterval {
			b.lastTouch = now
			b.touch()
		}
	}
	return n, err
}

func (b *delegationTouchingBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.touch)
	return err
}

// DelegatedTo reports where a session is currently being served, if it has been
// handed to the peer.
func (p *RemotePool) DelegatedTo(key string) (remotePoolDelegation, bool) {
	if p == nil {
		return remotePoolDelegation{}, false
	}
	return p.delegations.lookup(key, time.Now())
}

// logHoldsKept is the one line an opening that keeps the holds writes
// (PATCHES §54): the breaker is open, and every hold on the peer is kept. why
// is the reason failure gave. held counts every live hold, pending pre-claims
// included: on the HTTP forward that is the tripping forward's own claim too
// when it made one (released right after this line); the websocket dial has
// released its own by then.
func (s Server) logHoldsKept(pool *RemotePool, agentType, sessionID string, err error, why string) {
	if s.Logger == nil {
		return
	}
	now := time.Now()
	ageMS := int64(-1)
	if egress := pool.egress.view(); !egress.lastAnswer.IsZero() {
		ageMS = now.Sub(egress.lastAnswer).Milliseconds()
	}
	s.Logger.Warn("remote pool breaker opened; holds kept: outage not attributed to the peer",
		"remote", pool.name(), "agent", agentType, "session", sessionID, "why", why,
		"held", len(pool.delegations.live(now)), "no_route", noRoute(err), "local_answer_age_ms", ageMS,
		"quiet_for_ms", pool.breaker.quietFor(now, pool.egress.view()).Milliseconds(), "grace_ms", s.PeerDownGrace.Milliseconds())
}

const peerFoundDownMessage = "remote pool peer found down; its chats are placed as refugees on their next turns"

// logPeerDown is the line for a peer found down. Its holds stay: each chat
// held there is placed as a refugee on its next turn, within the cap of the
// account it goes to (refugeeSlotsFor), or keeps its hold and waits (PATCHES
// §67, D1, D2).
func (s Server) logPeerDown(pool *RemotePool, agentType, sessionID string) {
	if s.Logger == nil {
		return
	}
	held := 0
	for _, hold := range pool.delegations.live(time.Now()) {
		if hold.claim == 0 {
			held++
		}
	}
	s.Logger.Warn(peerFoundDownMessage,
		"remote", pool.name(), "agent", agentType, "session", sessionID,
		"held", held)
}

func (d *remotePoolDelegations) clear(key string) {
	if key == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.entries, key)
	d.dirty = true
}

// live copies every hold lookup would still answer for: not expired, pending
// claims included.
func (d *remotePoolDelegations) live(now time.Time) map[string]remotePoolDelegation {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]remotePoolDelegation, len(d.entries))
	for key, entry := range d.entries {
		if !now.After(entry.Until) {
			out[key] = entry
		}
	}
	return out
}

// remotePoolInboundHop reads the loop-guard depth off an incoming request.
func remotePoolInboundHop(r *http.Request) int {
	if r == nil {
		return 0
	}
	hop, err := strconv.Atoi(strings.TrimSpace(r.Header.Get(remotePoolHopHeader)))
	if err != nil || hop < 0 {
		return 0
	}
	return hop
}

// remotePoolEligible gates the single-peer entry points: forwarding allowed for
// this request and the first peer usable for this caller. The N-peer paths ask
// the two halves separately (peerForwardingAllowed, peerUsable), because one
// peer being down or closed to a caller says nothing about the others.
func (s Server) remotePoolEligible(r *http.Request) bool {
	return s.peerForwardingAllowed(r) && s.peerUsable(s.firstPeer(), r)
}

// remotePoolDelegated reports whether this session is already being served by
// a peer and should skip local selection entirely.
func (s Server) remotePoolDelegated(agentType, sessionID string) bool {
	_, _, ok := s.peerHold(session.ScopedSessionKey(agentType, sessionID))
	return ok
}

// remotePoolForward sends one request to the peer and returns its response with
// the body UNREAD, so callers can stream it.
//
// Returning an unread body is what makes streaming work for free: every
// re-dispatch decision upstream inspects status and headers only, and
// ReverseProxy writes nothing to the client until RoundTrip returns for good.
// clientPath and rawQuery MUST be the path the CLIENT sent, not
// proxyRequest.URL.Path. By the time the transport-level stage runs, the handler
// has already rewritten the path for the chosen provider via pathForUpstream.
// For Claude that rewrite is identity so the distinction is invisible; for Codex
// it is not -- a client's /backend-api/wham/tasks has become /wham/tasks, and
// forwarding that makes the peer re-resolve it to backend-api/codex/wham/tasks,
// a different endpoint. The peer must see exactly what the client sent so it can
// run its own upstreamForRequest/pathForUpstream against its own account.
// hold=false forwards one turn without creating or refreshing the session's
// hold: the session stays bound where it was (remotePoolHoldNone).
func (s Server) remotePoolForward(r *http.Request, agentType, sessionID, clientPath, rawQuery string, body []byte, hold bool) (*http.Response, bool) {
	mode := remotePoolHoldNone
	if hold {
		mode = remotePoolHoldSet
	}
	return s.remotePoolForwardMode(r, agentType, sessionID, clientPath, rawQuery, body, mode)
}

// remotePoolHoldMode is what one forward may do to the session's peer hold.
type remotePoolHoldMode int

const (
	// remotePoolHoldSet: a model turn the peer now owns. The hold is set (or
	// re-armed) when the peer answers, its idle clock restarts when the turn
	// ends, and a decline (429/503) ends it.
	remotePoolHoldSet remotePoolHoldMode = iota
	// remotePoolHoldNone: one model turn served elsewhere for a reason that is
	// not an eviction. Nothing is set or touched; a decline still ends an
	// existing hold, because the peer's pool answered for the session.
	remotePoolHoldNone
	// remotePoolHoldReadOnly: a side request (telemetry, count_tokens, MCP)
	// following wherever the conversation already lives. It never sets,
	// touches or clears the hold -- a 429 from an analytics endpoint is that
	// endpoint's answer, not the peer's pool being spent. Only a model turn
	// decides where a conversation lives. The breaker still opens on a dead
	// peer and ends every hold: that is the peer being down, whoever noticed
	// (when this machine can attribute the outage to the peer, PATCHES §54).
	remotePoolHoldReadOnly
)

// remotePoolForwardMode is remotePoolForward with an explicit hold mode: an
// UNTARGETED forward to the first peer, exactly as before targets existed.
func (s Server) remotePoolForwardMode(r *http.Request, agentType, sessionID, clientPath, rawQuery string, body []byte, mode remotePoolHoldMode) (*http.Response, bool) {
	out := s.forwardToPeer(s.firstPeer(), r, agentType, sessionID, clientPath, rawQuery, body, mode, "")
	return out.Resp, out.Served
}

// peerForwardOutcome is one attempt on one peer (PATCHES §31). Served means
// Resp is the peer's answer for the client, body unread. Otherwise Switch says
// why not -- forced (the account or the peer cannot serve the session),
// transient (serve this turn elsewhere, move nothing) or none (the client went
// away) -- and nothing was written anywhere.
type peerForwardOutcome struct {
	Resp   *http.Response
	Served bool
	Switch accountSwitch
	Status int
	// Refusal is the peer's refusal of the target, when it refused.
	Refusal remotePoolTargetRefusalDetail
	// endedHold is the hold the refusal ended: the session's entry, while it
	// still named the target. nil when the refusal ended none.
	endedHold *remotePoolDelegation
	// ServedAccount is the peer account that answered (X-Subrouter-Account),
	// and Substituted is true when that is not the target: a peer that ignores
	// targets. The answer is kept -- the turn is served -- but the session is
	// not held there, so the next turn re-ranks (D4 mixed versions).
	ServedAccount string
	Substituted   bool
	// Unreachable: the peer did not answer at all this attempt. NeverSent:
	// the request did not leave either (DNS, the dial, the TLS handshake),
	// so the peer cannot be running it.
	Unreachable bool
	NeverSent   bool
}

// clientGone reports an attempt that ended because the client cancelled.
func (o peerForwardOutcome) clientGone() bool {
	return !o.Served && o.Switch.Kind == switchNone && o.Switch.Reason == "client_canceled"
}

// forwardToPeer sends one request to pool and returns its response with the
// body UNREAD, so callers can stream it; target, when non-empty, names the ONE
// peer account the decider ranked first (X-Subrouter-Target-Account). Only a
// peer that publishes verdicts is ever sent a target (Deviation X1: an older
// daemon would pass an unknown header straight upstream).
//
// Every non-served answer is classified by classifyAccountSwitch and acted on
// by Kind alone: forced ends the hold (a side request's never), transient and
// a client cancel leave it. The breaker and the pre-claim follow the rules the
// single-peer forward always had.
func (s Server) forwardToPeer(pool *RemotePool, r *http.Request, agentType, sessionID, clientPath, rawQuery string, body []byte, mode remotePoolHoldMode, target string) peerForwardOutcome {
	if pool == nil || pool.URL == nil || r == nil {
		return peerForwardOutcome{Switch: accountSwitch{switchTransient, "no_peer"}}
	}
	if r.Context().Err() != nil {
		// The client left (a held turn's dial was cut short, and the handler
		// went on to rank): nothing is sent, and the claim below must not end
		// the session's hold elsewhere on behalf of a request nobody waits for.
		return peerForwardOutcome{Switch: classifyAccountSwitch(accountSwitchEvidence{ClientCanceled: true})}
	}

	destination := *pool.URL
	destination.Path = pool.URL.Path + clientPath
	destination.RawQuery = rawQuery

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, destination.String(), bytes.NewReader(body))
	if err != nil {
		return peerForwardOutcome{Switch: accountSwitch{switchTransient, "request_build_failed"}}
	}
	for key, values := range r.Header {
		if isHopByHopHeader(key) {
			continue
		}
		for _, value := range values {
			outReq.Header.Add(key, value)
		}
	}

	// The peer picks its own account and injects its own credential, so ours
	// must not travel. Leaving it on would also hand a local token to a machine
	// that has no use for it.
	session.StripSubrouterHeaders(outReq.Header)
	stripOutboundForwardingHeaders(outReq.Header)
	outReq.Header.Del("Authorization")
	outReq.Header.Del("X-Api-Key")
	// A CLIENT-supplied target must never cross a hop: only this daemon's own
	// ranking names one.
	outReq.Header.Del(remotePoolTargetAccountHeader)
	if target != "" {
		outReq.Header.Set(remotePoolTargetAccountHeader, target)
	}

	// X-Subrouter-Agent is load-bearing, not a nicety. The peer derives the
	// provider from agent type and NOT from the path, defaulting to codex; a
	// forwarded Claude request without it asks the peer's codex pool and comes
	// back "no accounts available" while its Claude pool sits idle.
	outReq.Header.Set("X-Subrouter-Agent", agentType)
	if sessionID != "" {
		// Gives the peer a stable key for its own sticky assignment, which is
		// what keeps a conversation on one of its accounts across turns.
		outReq.Header.Set("X-Subrouter-Session", sessionID)
	}
	outReq.Header.Set(remotePoolHopHeader, strconv.Itoa(remotePoolInboundHop(r)+1))
	if pool.Token != "" {
		outReq.Header.Set("Authorization", "Bearer "+pool.Token)
	}
	outReq.Host = destination.Host
	outReq.ContentLength = int64(len(body))
	outReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}

	key := session.ScopedSessionKey(agentType, sessionID)
	now := time.Now()
	ttl := remotePoolDelegationTTLFor(agentType)

	// Claim the session for the peer before the request leaves (a model turn
	// only). The hold used to appear only once the peer answered, and every
	// request for the same session in between -- Codex opens the turn's
	// websocket and posts on the bare thread id in the same second -- found
	// the local pin the fresh pick had just written, read it as warm and was
	// served locally. claimed is non-zero only when THIS call created the
	// hold; every return below where the peer did not serve drops it. The
	// claim names the target, so a concurrent request of the session follows
	// the SAME account, and it clears the session on every other peer: a
	// session is held in one place.
	var claimed uint64
	if mode == remotePoolHoldSet {
		s.clearHoldElsewhere(key, pool)
		claimed = pool.delegations.claimTarget(key, now, ttl, pool.name(), target)
	}
	notServed := func(out peerForwardOutcome) peerForwardOutcome {
		pool.delegations.release(key, claimed)
		return out
	}

	resp, err := pool.client.Do(noteNewConnections(outReq, s.peers()))
	if err != nil {
		// A cancelled client request is not the peer's fault: it must not count
		// toward opening the breaker, and it must not end the session's hold.
		// Clearing here moved a warm peer session to its local account every
		// time the user pressed Esc before the peer's headers arrived.
		if r.Context().Err() != nil {
			return notServed(peerForwardOutcome{Switch: classifyAccountSwitch(accountSwitchEvidence{ClientCanceled: true})})
		}
		// now was taken before the request left: a forward already in flight
		// when another failure was counted is that same blip, not a new strike.
		opened, peerDown, keptWhy := pool.breaker.failure(now, time.Now(), err, &pool.egress, s.PeerDownGrace)
		// One failed round trip is a blip, not an eviction: the hold survives
		// and this turn falls through. The breaker opening on an outage this
		// machine can attribute to the peer is the peer being DOWN: each chat
		// held there is a refugee on its next turn (placeRefugee), and keeps its
		// hold until it is placed, gives up (unlinkStranded) or idles out
		// (PATCHES §67). An opening that is not attributed to the peer keeps
		// every hold, and keptWhy says why:
		// no_route, no_answer, network_back_mid_strike, no_new_connection, or
		// one_strike_since_back when only one strike has been sent since this
		// machine came back (whyNotThePeers, failure; PATCHES §54), or
		// within_grace while the peer has been silent for less than the grace
		// (PATCHES §67).
		if s.Logger != nil {
			s.Logger.Warn("remote pool forward failed",
				"remote", pool.name(), "agent", agentType, "session", sessionID,
				"target_account", target, "breaker_opened", opened, "peer_down", peerDown, "error", err)
		}
		if peerDown {
			s.logPeerDown(pool, agentType, sessionID)
		}
		if keptWhy != "" {
			s.logHoldsKept(pool, agentType, sessionID, err, keptWhy)
		}
		_, neverSent, _ := transientNetworkCause(err)
		return notServed(peerForwardOutcome{
			Unreachable: true,
			NeverSent:   neverSent,
			Switch:      classifyAccountSwitch(accountSwitchEvidence{PeerErr: err, PeerBreakerOpened: opened, PeerOutageUnattributed: opened && !peerDown}),
		})
	}

	// The peer answering at all proves reachability, so the breaker closes even
	// on a 429 -- that is a quota answer, not a transport failure.
	pool.breaker.success(time.Now())
	// Every read of the peer's body from here on -- a refusal's, or the
	// streamed answer either relay copies -- is bounded by silence, not by
	// length, and a cut is logged (PATCHES §33).
	s.watchPeerAnswer(pool, r, resp, agentType, sessionID, clientPath, target)

	sideRequest := mode == remotePoolHoldReadOnly
	verdict := classifyAccountSwitch(accountSwitchEvidence{PeerStatus: resp.StatusCode, PeerHeader: resp.Header})
	if refusal, refused := readTargetRefusal(resp); refused {
		// The owning daemon will not serve the target -- refused before
		// upstream, or its account evicted by the answer it passed through. A
		// refusal is never relayed to the client: the decider recomputes. A
		// side request's refusal ends nothing; the next model turn asks for real.
		// A targeted refusal ends the hold only while it still names the
		// target: another turn of the session may have moved it since this
		// one left.
		resp.Body.Close()
		out := peerForwardOutcome{Switch: verdict, Status: resp.StatusCode, Refusal: refusal}
		if !sideRequest {
			if target == "" {
				pool.delegations.clear(key)
			} else if ended, ok := pool.delegations.clearIfHeldOn(key, target); ok {
				out.endedHold = &ended
			}
		}
		if out.Refusal.Account == "" {
			out.Refusal.Account = target
		}
		return notServed(out)
	}

	switch {
	case verdict.Kind == switchForced:
		// peer_declined: the peer exhausted its own pool (a legacy peer's 429/503,
		// or a lifecycle 503). Drop the delegation so the next turn re-decides
		// instead of pinning to a peer that has nothing left. A side request's
		// 429/503 is its own endpoint's answer and ends nothing; the
		// conversation's next model turn asks the peer's pool for real.
		if !sideRequest {
			pool.delegations.clear(key)
		}
		if s.Logger != nil {
			// A side request's decline changes nothing and repeats on every
			// telemetry POST: Info, not Warn.
			level := slog.LevelWarn
			if sideRequest {
				level = slog.LevelInfo
			}
			s.Logger.Log(r.Context(), level, "remote pool declined",
				"remote", pool.name(), "agent", agentType, "session", sessionID,
				"target_account", target, "status", resp.StatusCode, "side_request", sideRequest)
		}
		resp.Body.Close()
		return notServed(peerForwardOutcome{Switch: verdict, Status: resp.StatusCode})
	case verdict.Kind == switchTransient:
		// A transient failure on the peer's side -- its token refresh reached
		// no verdict (502), or the target is limited but not spent (429, from a
		// daemon that stamps it) -- is a blip, not an eviction: keep the hold
		// and let the caller serve this one turn elsewhere.
		if s.Logger != nil {
			s.Logger.Warn("remote pool transient failure; serving this turn locally",
				"remote", pool.name(), "agent", agentType, "session", sessionID,
				"target_account", target, "status", resp.StatusCode, "reason", verdict.Reason)
		}
		resp.Body.Close()
		return notServed(peerForwardOutcome{Switch: verdict, Status: resp.StatusCode})
	}

	// The peer names the account it used; keep it so this session can be
	// reported honestly instead of by its local placeholder.
	served := resp.Header.Get(subrouterAccountHeader)
	out := peerForwardOutcome{Resp: resp, Served: true, Status: resp.StatusCode, ServedAccount: served}
	// Every caller relays a served answer to the client, the handler's and
	// the transport stage's alike (PATCHES §71). Only a 2xx is one of the
	// session's turns answered: a relayed 500, 502, 503 or 504 is held and
	// asked again (holdableUpstreamStatus, PATCHES §67).
	noteExchangeServed(r.Context(), exchangeServed{route: "peer", peer: pool.name(), peerAcct: served})
	if !sideRequest && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		s.noteAnswered(agentType, sessionID)
	}
	pool.routed.note(string(providerForRequest(agentType, clientPath)), target, time.Now())
	// A new session placed on the target starts its week (PATCHES §86.2).
	notePeerTargetServed(r.Context(), pool, target, out)
	if target != "" && served != "" && !strings.EqualFold(served, target) {
		// A peer that ignores targets (an older daemon, or one that failed over
		// on its own) served the turn on an account this daemon never ranked.
		// The answer stands -- it is already served -- but the session is not
		// held there: the next turn re-ranks, and until the peer's next usage
		// body it is treated as a legacy peer and sent no target.
		out.Substituted = true
		if !sideRequest {
			pool.delegations.release(key, claimed)
			pool.delegations.clear(key)
		}
		pool.markTargetIgnored(pool.cachedUsage().At)
		if s.Logger != nil {
			s.Logger.Warn("remote pool peer substituted the target",
				"remote", pool.name(), "agent", agentType, "session", sessionID,
				"target_account", target, "served_account", served)
		}
		return out
	}
	if mode != remotePoolHoldSet {
		return out
	}
	if served == "" {
		served = target
	}
	// This also confirms a pre-claim. A targeted turn sets the hold only
	// while it still names the account that served (setServedIfHeld).
	if target == "" {
		pool.delegations.setServed(key, now, ttl, served, pool.name(), false)
	} else if !pool.delegations.setServedIfHeld(key, now, ttl, served, pool.name()) {
		if s.Logger != nil {
			s.Logger.Info("remote pool turn served after its hold moved; hold left as it is",
				"remote", pool.name(), "agent", agentType, "session", sessionID, "target_account", target)
		}
		return out
	}
	resp.Body = &delegationTouchingBody{
		ReadCloser: resp.Body,
		touch:      func() { pool.delegations.touch(key, time.Now(), ttl) },
	}
	return out
}

// serveRemotePoolFallthrough is the handler-level entry point: local selection
// could not produce an account at all.
//
// This is the primary path. The retry transport is only installed once an
// account has been selected, so a pool with nothing usable never reaches the
// transport-level stage.
func (s Server) serveRemotePoolFallthrough(w http.ResponseWriter, r *http.Request, agentType, sessionID string) bool {
	return s.serveRemotePool(w, r, agentType, sessionID, remotePoolHoldSet)
}

// serveRemotePool serves this request UNTARGETED from the first peer, doing to
// the session's hold only what mode allows (see remotePoolHoldMode). The
// handler itself now goes through serveAcrossPeers / serveHeldTurn, which rank
// and target; this stays as the single-peer primitive they share.
func (s Server) serveRemotePool(w http.ResponseWriter, r *http.Request, agentType, sessionID string, mode remotePoolHoldMode) bool {
	if !s.remotePoolEligible(r) {
		return false
	}
	body, ok := replayableForwardBody(r)
	if !ok {
		return false
	}
	return s.serveOnePeerAttempt(w, r, s.firstPeer(), agentType, sessionID, body, mode, "").Served
}

// replayableForwardBody buffers the request body once so every peer attempt of
// a turn -- a refusal, a recompute, the next peer -- sends byte-identical
// bytes, and restores r.Body so a decline leaves the request intact for
// whatever the handler does next. A websocket upgrade has no body (ok, nil).
//
// LimitReader would silently truncate an oversized body into a request the
// peer would answer as if it were complete. Refuse instead: the same cap
// already excludes these from the replayable path entirely.
func replayableForwardBody(r *http.Request) ([]byte, bool) {
	if websocket.IsWebSocketUpgrade(r) {
		return nil, true
	}
	if r.ContentLength > replayablePostMaxBodyBytes {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, replayablePostMaxBodyBytes+1))
	if err != nil || int64(len(body)) > replayablePostMaxBodyBytes {
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, true
}

// serveOnePeerAttempt makes one attempt on one peer and, only when it served,
// writes the peer's answer to w.
//
// A websocket upgrade cannot be replayed as a buffered request, and trying is
// not a no-op: the header copy drops Connection and Upgrade as hop-by-hop, so
// the peer received a bare GET on /v1/responses, answered 405, and the client
// burned five reconnects before falling back to HTTPS. Codex opens every model
// turn this way, so it is dialed as a socket; each attempt re-dials before the
// client is upgraded, which is what makes a refused socket replayable too.
func (s Server) serveOnePeerAttempt(w http.ResponseWriter, r *http.Request, pool *RemotePool, agentType, sessionID string, body []byte, mode remotePoolHoldMode, target string) peerForwardOutcome {
	if websocket.IsWebSocketUpgrade(r) {
		return s.serveWebSocketToPeer(w, r, pool, agentType, sessionID, mode, target)
	}
	// Handler scope: r has not been rewritten here, so its path is the
	// client-facing one.
	out := s.forwardToPeer(pool, r, agentType, sessionID, r.URL.Path, r.URL.RawQuery, body, mode, target)
	if !out.Served {
		return out
	}
	resp := out.Resp
	defer resp.Body.Close()

	if s.Logger != nil {
		s.Logger.Info("serving from remote pool",
			"remote", pool.name(), "agent", agentType, "session", sessionID,
			"method", r.Method, "path", r.URL.Path, "transport", "http",
			"status", resp.StatusCode, "target_account", target, "served_account", out.ServedAccount)
	}
	for key, values := range resp.Header {
		if isHopByHopHeader(key) {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	// Correct the peer's own stamp: it served this locally FROM ITS SIDE, but
	// from the client's side it came off the peer. The account name is the
	// peer's and is left as-is.
	w.Header().Set(subrouterPoolHeader, "remote")
	w.Header().Set(subrouterPeerHeader, pool.name())
	hideServingAccount(r, w.Header())
	if holdableUpstreamStatus(resp.StatusCode) {
		// The peer's own upstream failed (its 502 "upstream request failed",
		// a provider 5xx it already retried): this daemon holds the turn and
		// asks again (PATCHES §58). A peer never holds a forwarded request
		// itself, so the decision stays with the daemon that can serve locally.
		noteHoldable(r.Context(), holdMark{status: resp.StatusCode,
			reason: fmt.Sprintf("peer_status:%d", resp.StatusCode),
			agent:  agentType, session: sessionID, account: out.ServedAccount})
	}
	w.WriteHeader(resp.StatusCode)
	if readErr := flushingCopy(w, resp.Body, nil); readErr != nil && r.Context().Err() == nil {
		// The peer's answer broke mid-body; the watchdog has logged it. The
		// client must not receive what it has so far as a whole answer.
		abortPeerRelay(r)
	}
	return out
}

// peerWebSocketDialer dials the PEER for a forwarded upgrade.
//
// Deliberately not outboundWebSocketDialer(): that one pins tcp4 because
// chatgpt.com's edge closed dual-stack upgrades with EOF. A peer is a subrouter
// daemon we chose, usually on a tailnet, and tailnet addressing is routinely
// IPv6 -- inheriting the tcp4 pin would make an IPv6-only peer unreachable for
// websockets while plain HTTP to the same peer kept working, which is a
// miserable thing to debug.
//
// The http/1.1 ALPN pin IS kept: an upgrade cannot proceed over h2 at all, so
// that one is about the protocol rather than about any particular upstream.
// newPeerWebSocketDialer builds this peer's dialer.
//
// PER PEER, not the process-wide sync.Once it used to be: the TLS config now
// carries a pin, and pins differ between peers. A shared dialer would either
// carry one peer's pin to all of them (refusing the rest) or carry none
// (verifying nothing) -- and since Codex opens every model turn as a websocket,
// getting this wrong leaves the majority of peer traffic unverified.
func newPeerWebSocketDialer() *websocket.Dialer {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return &websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: websocketHandshakeTimeout,
		NetDialContext:   dialer.DialContext,
		TLSClientConfig:  &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}},
	}
}

// webSocketDialer returns this peer's dialer, pin included.
func (p *RemotePool) webSocketDialer() *websocket.Dialer {
	if p == nil || p.wsDialer == nil {
		return newPeerWebSocketDialer()
	}
	if p.wsDialer.NetDialTLSContext != nil && p.webSocketProxied() {
		// gorilla/websocket dials a proxy with NetDialTLSContext and then
		// sends the upgrade through the tunnel in the clear.
		proxied := *p.wsDialer
		proxied.NetDialTLSContext = nil
		return &proxied
	}
	return p.wsDialer
}

func (p *RemotePool) webSocketProxied() bool {
	if p.wsDialer.Proxy == nil {
		return false
	}
	proxyURL, err := p.wsDialer.Proxy(&http.Request{URL: p.URL})
	return err != nil || proxyURL != nil
}

// serveRemotePoolWebSocket forwards a websocket upgrade UNTARGETED to the first
// peer; see serveWebSocketToPeer.
func (s Server) serveRemotePoolWebSocket(w http.ResponseWriter, r *http.Request, agentType, sessionID string, mode remotePoolHoldMode) bool {
	return s.serveWebSocketToPeer(w, r, s.firstPeer(), agentType, sessionID, mode, "").Served
}

// serveWebSocketToPeer forwards a websocket upgrade to pool, naming target when
// set, and relays frames in both directions.
//
// Returns not-served WITHOUT having written to w when the peer cannot take it,
// so the caller recomputes or falls through to the local path exactly as a
// declined HTTP forward does. Everything that can refuse -- a targeted 409
// included, which arrives as the dial's response -- is therefore checked
// before the client is upgraded; once w has been hijacked there is no falling
// back to anything.
//
// The hold follows the HTTP path's rules (remotePoolHoldMode). A socket is one
// conversation for its whole life -- Codex carries turn after turn on it -- so
// with remotePoolHoldSet the hold is claimed before the dial, set once the
// peer's 101 has reached the client, kept fresh by traffic while the socket is
// open, and its idle clock restarted when the socket closes.
func (s Server) serveWebSocketToPeer(w http.ResponseWriter, r *http.Request, pool *RemotePool, agentType, sessionID string, mode remotePoolHoldMode, target string) peerForwardOutcome {
	if pool == nil || pool.URL == nil {
		return peerForwardOutcome{Switch: accountSwitch{switchTransient, "no_peer"}}
	}
	// Same origin rule the local path applies. A browser must not be able to
	// spend the peer's accounts just because this daemon can reach it.
	if !webSocketOriginAllowed(r) {
		return peerForwardOutcome{Switch: accountSwitch{switchTransient, "origin_refused"}}
	}

	destination := *pool.URL
	destination.Scheme = websocketScheme(pool.URL.Scheme)
	// The path the CLIENT sent, for the same reason remotePoolForward uses it:
	// the peer runs its own pathForUpstream against its own account.
	destination.Path = pool.URL.Path + r.URL.Path
	destination.RawQuery = r.URL.RawQuery

	headers := r.Header.Clone()
	// The Dialer writes its own Connection/Upgrade/Sec-WebSocket-* and rejects
	// the whole dial with "duplicate header not allowed" if we leave ours on.
	stripWebSocketDialHeaders(headers)
	session.StripSubrouterHeaders(headers)
	stripOutboundForwardingHeaders(headers)
	headers.Del("Authorization")
	headers.Del("X-Api-Key")
	// Only this daemon's ranking names a target; a client's never crosses.
	headers.Del(remotePoolTargetAccountHeader)
	if target != "" {
		headers.Set(remotePoolTargetAccountHeader, target)
	}
	headers.Set("X-Subrouter-Agent", agentType)
	if sessionID != "" {
		headers.Set("X-Subrouter-Session", sessionID)
	}
	headers.Set(remotePoolHopHeader, strconv.Itoa(remotePoolInboundHop(r)+1))
	if pool.Token != "" {
		headers.Set("Authorization", "Bearer "+pool.Token)
	}

	key := session.ScopedSessionKey(agentType, sessionID)
	ttl := remotePoolDelegationTTLFor(agentType)
	// Claimed before the dial for the same reason forwardToPeer claims before
	// its round trip: a request for this session arriving mid-handshake must
	// follow the socket to the peer, and to the same account. Non-zero only
	// when this call created the hold, and dropped on every path where the
	// peer did not serve.
	var claimed uint64
	// Taken before the dial leaves, for the breaker: a dial already in flight
	// when another failure was counted is that same blip, not a new strike.
	dialStarted := time.Now()
	if mode == remotePoolHoldSet {
		s.clearHoldElsewhere(key, pool)
		claimed = pool.delegations.claimTarget(key, dialStarted, ttl, pool.name(), target)
	}
	peerConn, response, err := pool.webSocketDialer().Dial(destination.String(), headers)
	if err != nil {
		// Mirrors forwardToPeer, through the same classifier. Only a peer that
		// cannot serve ends the hold: it refuses the target, it declines
		// (429/503, its pool is spent) or it is down (the breaker opens). A
		// client cancel, one failed dial, a transient 502 or any other refusal
		// of the upgrade is a blip for this turn: an existing hold survives. A
		// real HTTP answer proves reachability even when it refuses the
		// upgrade; a cancel is not the peer's fault.
		status := 0
		out := peerForwardOutcome{}
		opened, peerDown, keptWhy := false, false, ""
		switch {
		case response != nil:
			status = response.StatusCode
			pool.breaker.success(time.Now())
			out.Switch = classifyAccountSwitch(accountSwitchEvidence{PeerStatus: status, PeerHeader: response.Header})
			if refusal, refused := readTargetRefusal(response); refused {
				if refusal.Account == "" {
					refusal.Account = target
				}
				out.Refusal = refusal
			}
			if out.Switch.Kind == switchNone {
				// An upgrade refused for a reason that is not about the account
				// (a 405, a 500): this turn cannot be served there, and nothing
				// moves.
				out.Switch = accountSwitch{switchTransient, "upgrade_refused"}
			}
			if out.Switch.Kind == switchForced && mode != remotePoolHoldReadOnly {
				pool.delegations.clear(key)
			}
		case r.Context().Err() != nil:
			out.Switch = classifyAccountSwitch(accountSwitchEvidence{ClientCanceled: true})
		default:
			opened, peerDown, keptWhy = pool.breaker.failure(dialStarted, time.Now(), err, &pool.egress, s.PeerDownGrace)
			out.Unreachable = true
			out.Switch = classifyAccountSwitch(accountSwitchEvidence{PeerErr: err, PeerBreakerOpened: opened, PeerOutageUnattributed: opened && !peerDown})
		}
		out.Status = status
		pool.delegations.release(key, claimed)
		if s.Logger != nil {
			s.Logger.Warn("remote pool websocket dial failed",
				"remote", pool.name(), "agent", agentType, "session", sessionID,
				"path", r.URL.Path, "status", status, "target_account", target,
				"switch", out.Switch.Kind.String(), "reason", out.Switch.Reason,
				"breaker_opened", opened, "peer_down", peerDown, "error", err)
		}
		if peerDown {
			s.logPeerDown(pool, agentType, sessionID)
		}
		if keptWhy != "" {
			s.logHoldsKept(pool, agentType, sessionID, err, keptWhy)
		}
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return out
	}
	pool.breaker.success(time.Now())
	defer peerConn.Close()
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}

	responseHeader := http.Header{}
	if response != nil {
		responseHeader = cloneWebSocketResponseHeaders(response.Header)
	}
	// Same correction the HTTP path makes: from the client's side this came off
	// the peer, whatever the peer stamped it with.
	responseHeader.Set(subrouterPoolHeader, "remote")
	responseHeader.Set(subrouterPeerHeader, pool.name())
	hideServingAccount(r, responseHeader)

	peerAccount := ""
	if response != nil {
		peerAccount = response.Header.Get(subrouterAccountHeader)
	}
	out := peerForwardOutcome{Served: true, Status: http.StatusSwitchingProtocols, ServedAccount: peerAccount}
	pool.routed.note(string(providerForRequest(agentType, r.URL.Path)), target, time.Now())
	holdHere := mode == remotePoolHoldSet
	if target != "" && peerAccount != "" && !strings.EqualFold(peerAccount, target) {
		// The HTTP path's substitution rule: the socket is served, but not held.
		out.Substituted = true
		if mode != remotePoolHoldReadOnly {
			pool.delegations.release(key, claimed)
			pool.delegations.clear(key)
		}
		holdHere = false
		pool.markTargetIgnored(pool.cachedUsage().At)
		if s.Logger != nil {
			s.Logger.Warn("remote pool peer substituted the target",
				"remote", pool.name(), "agent", agentType, "session", sessionID,
				"target_account", target, "served_account", peerAccount, "transport", "websocket")
		}
	}

	clientConn, err := (&websocket.Upgrader{}).Upgrade(w, r, responseHeader)
	if err != nil {
		// Upgrade has already written its own error response; reporting served
		// stops the caller from writing a second one. No turn reached the
		// peer, so a claim this call made goes too.
		pool.delegations.release(key, claimed)
		return out
	}
	// A new session placed on the target starts its week (PATCHES §86.2):
	// marked now, not when the socket closes.
	notePeerTargetServed(r.Context(), pool, target, out)
	defer clientConn.Close()
	clientConn.SetReadLimit(maxWebSocketMessageBytes)
	peerConn.SetReadLimit(maxWebSocketMessageBytes)
	// The relay below records the messages under this socket's meta (PATCHES
	// §71); before, a socket served by a peer had none.
	r = s.recordPeerWebSocketMeta(r, pool.name(), peerAccount, agentType, sessionID)

	// The peer took the conversation: hold it there, naming the peer's account
	// when its 101 does (the target otherwise). Set before the relay starts, so
	// a hold cleared while the socket is open (a decline on another request,
	// the breaker) stays cleared: everything after this only touches, and
	// touch never resurrects. Never vetoes a message: the observer only keeps
	// the hold fresh.
	observe := func(int, []byte, []byte) error { return nil }
	if holdHere {
		account := peerAccount
		if account == "" {
			account = target
		}
		pool.delegations.setServed(key, time.Now(), ttl, account, pool.name(), target != "")
		// Traffic is the conversation being used. Without this a socket that
		// outlives the idle bound (one socket carries many turns) loses its
		// hold mid-conversation. At most one touch per interval, so a
		// streaming reply costs no lock per frame; the first frame always
		// touches.
		var lastTouch atomic.Int64
		observe = func(int, []byte, []byte) error {
			now := time.Now()
			last := lastTouch.Load()
			if last != 0 && now.Sub(time.Unix(0, last)) < remotePoolWebSocketTouchInterval {
				return nil
			}
			if lastTouch.CompareAndSwap(last, now.UnixNano()) {
				pool.delegations.touch(key, now, ttl)
			}
			return nil
		}
		// The idle clock starts when the conversation stops, which for a
		// socket is when it closes: the analogue of delegationTouchingBody.
		defer func() { pool.delegations.touch(key, time.Now(), ttl) }()
	}

	if s.Logger != nil {
		s.Logger.Info("serving from remote pool",
			"remote", pool.name(), "agent", agentType, "session", sessionID,
			"method", r.Method, "path", r.URL.Path, "status", http.StatusSwitchingProtocols,
			"transport", "websocket", "target_account", target, "served_account", peerAccount)
	}

	// A dumb relay on purpose. copyWebSocketMessages is the POLICY layer --
	// exhaustion marking, model rerouting, lease reporting -- and all of that
	// belongs to whoever owns the credential. That is the peer, which runs its
	// own copy against its own account. Duplicating it here would mark OUR
	// accounts exhausted based on a turn one of the PEER's accounts served.
	turns := &webSocketTurnGate{lifecycle: s.Lifecycle}
	defer turns.releaseAll()
	countTurns := providerForRequest(agentType, r.URL.Path) == accounts.ProviderCodex && !codexLivePath(r.URL.Path)
	if countTurns {
		stopWatching := turns.closeWhenRetired(func() {
			closeWebSocketWithServiceRestart(clientConn, "subrouter is restarting; reconnect")
			_ = peerConn.Close()
		})
		defer stopWatching()
	}
	relay := func(direction string, src, dst *websocket.Conn) {
		endFinishedTurn := func() {}
		completed := false
		notesAnswers := direction == "upstream_to_client" && mode != remotePoolHoldReadOnly
		observeTurn := func(messageType int, body, head []byte) error {
			if direction == "upstream_to_client" {
				turns.noteUpstream()
			}
			if notesAnswers {
				completed = messageType == websocket.TextMessage && codexWebSocketTurnCompleted(body, head)
			}
			if !countTurns || messageType != websocket.TextMessage {
				return observe(messageType, body, head)
			}
			switch direction {
			case "client_to_upstream":
				if _, start := codexWebSocketTurnStart(body, head); start && !turns.admit() {
					return errWebSocketTurnRefused
				}
			case "upstream_to_client":
				if codexWebSocketTurnFinished(body, head) {
					endFinishedTurn = turns.finish()
				}
			}
			return observe(messageType, body, head)
		}
		for {
			err := s.forwardWebSocketMessage(r.Context(), agentType, sessionID, direction, src, dst, observeTurn)
			if completed && err == nil {
				s.noteAnswered(agentType, sessionID)
			}
			completed = false
			endFinishedTurn()
			endFinishedTurn = func() {}
			if errors.Is(err, errWebSocketTurnRefused) {
				closeWebSocketWithServiceRestart(src, "subrouter is restarting; reconnect")
				return
			}
			if err != nil {
				forwardWebSocketClose(dst, err)
				return
			}
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		relay("client_to_upstream", clientConn, peerConn)
		_ = peerConn.Close()
	}()
	go func() {
		defer wg.Done()
		relay("upstream_to_client", peerConn, clientConn)
		_ = clientConn.Close()
	}()
	wg.Wait()
	return out
}

// remotePoolStageResponse is the transport-level entry point: we had accounts,
// and local failover burned through all of them. The single-peer form, kept
// for its callers; the handler arms remotePoolStageAcrossPeers, which ranks
// every peer and targets.
//
// Round-trips off the retry transport by construction (its own http.Client), so
// re-entry is structurally impossible -- the same discipline
// claudeOpusSubstituteResponse follows.
func (s Server) remotePoolStageResponse(r *http.Request, agentType, sessionID, clientPath, rawQuery, reason string) (*http.Response, bool) {
	if r == nil || r.GetBody == nil || !s.remotePoolEligible(r) {
		return nil, false
	}
	rc, err := r.GetBody()
	if err != nil {
		return nil, false
	}
	body, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return nil, false
	}
	resp, ok := s.remotePoolForward(r, agentType, sessionID, clientPath, rawQuery, body, true)
	if !ok {
		return nil, false
	}
	if s.Logger != nil {
		s.Logger.Warn("local pool exhausted; served from remote pool",
			"remote", s.firstPeer().name(), "agent", agentType, "session", sessionID,
			"reason", reason, "status", resp.StatusCode)
	}
	return resp, true
}

// onceRemotePoolStage bounds forwarding to one attempt per client request.
// replayablePostRetryTransport wraps usageLimitRetryTransport on the OUTSIDE, so
// a network error replays the whole RoundTrip on the same transport value; the
// Opus stage needs the same guard for the same reason.
func onceRemotePoolStage(stage func(reason string) (*http.Response, bool)) func(string) (*http.Response, bool) {
	var once sync.Once
	return func(reason string) (*http.Response, bool) {
		first := false
		once.Do(func() { first = true })
		if !first {
			return nil, false
		}
		return stage(reason)
	}
}
