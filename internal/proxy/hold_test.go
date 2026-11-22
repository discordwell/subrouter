package proxy

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// The hold (PATCHES §58). Every test drives the whole handler through a real
// HTTP listener, as Claude Code does, against a fake Anthropic upstream; the
// failures are the shapes the laptop logged on 2026-10-20.

const holdTestProxyToken = "hold-test-proxy-token"

// holdTestPolicy holds for deadline with millisecond backoffs, so a test
// spends its time in attempts, not in waits.
func holdTestPolicy(deadline time.Duration) *HoldPolicy {
	return &HoldPolicy{
		Deadline:    deadline,
		MaxRequests: 8,
		MaxBytes:    1 << 20,
		baseBackoff: 5 * time.Millisecond,
		maxBackoff:  20 * time.Millisecond,
	}
}

// holdRig is one daemon in front of one fake upstream.
type holdRig struct {
	daemon *httptest.Server
	logs   *syncBuffer
	sched  *selectacct.SchedulerRef
	store  *session.Store
	// storePath is the session store's file, for a second rig standing for
	// the next process.
	storePath string
	dials     atomic.Int64
	handled   chan struct{} // one value per finished handler run
	// pool is the peer, when the rig has one; peerTurns counts the requests
	// it served that were not its usage read.
	pool      *RemotePool
	peerTurns atomic.Int64
}

type holdRigOptions struct {
	hold      *HoldPolicy
	upstream  http.HandlerFunc
	accounts  []accounts.Account
	failDials func(n int64) error // nth dial (1-based) fails with this, nil dials
	refresh   func(context.Context, accounts.Account) (accounts.Account, error)
	pin       string // session pinned to this account
	lifecycle *Lifecycle
	public    bool // serve PublicHandler, the box's internet-facing listener
	// peer, when set, is a peer daemon (SUBROUTER_REMOTE_POOL) that answers
	// every turn with this handler; its usage read is answered empty, so it
	// is served untargeted. spill is the outage spill (PATCHES §59).
	peer  http.HandlerFunc
	spill *OutageSpillPolicy
	// exhausted spends every local account until a known reset, so a turn
	// local cannot serve gets the pool-exhausted 503 and its Retry-After.
	exhausted bool
	// peers, when set, are the configured peers in place of peer.
	peers []*RemotePool
	// peerDownGrace is Server.PeerDownGrace (PATCHES §67); zero is off.
	peerDownGrace time.Duration
	// admission, when set, is the daemon's admission gate, so a test can
	// read its queue; nil is the default gate.
	admission *admissionControl
	// active is the daemon's session tracker; nil reads every session as
	// possibly warm.
	active *ActiveSessions
	// keepAlive keeps the daemon's connections to the upstream open between
	// turns, so failDials fails only the turns that need a new one.
	keepAlive bool
	// sessionsPath is the session store's file, shared by two rigs that
	// stand for two daemon processes; empty is the rig's own.
	sessionsPath string
	// onLog sees every log record as the daemon logs it, before the rig's
	// log has it, so a test can act at an exact point in a turn.
	onLog func(slog.Record)
	// refugeeSlots, when set, is each account's refugee cap
	// (Server.refugeeSlotsOverride).
	refugeeSlots func(peer, account, poolModel string) int
	// retryAfterMax is Server.ClientRetryAfterMax; zero is off.
	retryAfterMax time.Duration
}

// hookedHandler runs hook on each record before next handles it.
type hookedHandler struct {
	next slog.Handler
	hook func(slog.Record)
}

func (h hookedHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h hookedHandler) Handle(ctx context.Context, record slog.Record) error {
	h.hook(record)
	return h.next.Handle(ctx, record)
}

func (h hookedHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return hookedHandler{h.next.WithAttrs(attrs), h.hook}
}

func (h hookedHandler) WithGroup(name string) slog.Handler {
	return hookedHandler{h.next.WithGroup(name), h.hook}
}

func newHoldRig(t *testing.T, opts holdRigOptions) *holdRig {
	t.Helper()
	upstream := httptest.NewServer(opts.upstream)
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	rig := &holdRig{logs: &syncBuffer{}, handled: make(chan struct{}, 64)}
	if opts.peer != nil {
		peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == remotePoolUsagePath {
				return
			}
			rig.peerTurns.Add(1)
			opts.peer(w, r)
		}))
		t.Cleanup(peer.Close)
		rig.pool = NewRemotePool(peer.URL, "", "")
	}
	if len(opts.accounts) == 0 {
		opts.accounts = []accounts.Account{{ID: "one@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-one"}}
	}
	scores := make([]selectacct.Score, 0, len(opts.accounts))
	reset := time.Now().Add(90 * time.Minute)
	for _, account := range opts.accounts {
		score := selectacct.Score{AccountID: account.ID, Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1}
		if opts.exhausted {
			score = selectacct.Score{AccountID: account.ID, Provider: accounts.ProviderClaude, Fresh: true,
				WeeklyResetAt: reset, ExhaustedResetAt: reset}
		}
		scores = append(scores, score)
	}
	rig.sched = selectacct.NewSchedulerRef(selectacct.NewScheduler(scores))
	sessionsPath := opts.sessionsPath
	if sessionsPath == "" {
		sessionsPath = filepath.Join(t.TempDir(), "sessions.json")
	}
	store, err := session.NewStore(sessionsPath)
	if err != nil {
		t.Fatal(err)
	}
	rig.store, rig.storePath = store, sessionsPath
	if opts.pin != "" {
		if _, err := store.Put("claude", "hold-session", opts.pin, ""); err != nil {
			t.Fatal(err)
		}
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			n := rig.dials.Add(1)
			if opts.failDials != nil {
				if err := opts.failDials(n); err != nil {
					return nil, err
				}
			}
			return dialer.DialContext(ctx, network, addr)
		},
		// A fresh connection per attempt, so every attempt dials.
		DisableKeepAlives: !opts.keepAlive,
	}
	t.Cleanup(transport.CloseIdleConnections)
	var logHandler slog.Handler = slog.NewTextHandler(rig.logs, &slog.HandlerOptions{Level: slog.LevelDebug})
	if opts.onLog != nil {
		logHandler = hookedHandler{logHandler, opts.onLog}
	}
	server := Server{
		ClaudeUpstream:   upstreamURL,
		Accounts:         opts.accounts,
		Sessions:         store,
		SchedulerRef:     rig.sched,
		MaxBodyBytes:     1 << 20,
		Transport:        transport,
		Logger:           slog.New(logHandler),
		LocalProxyToken:  holdTestProxyToken,
		RefreshAccountFn: opts.refresh,
		Lifecycle:        opts.lifecycle,
		Hold:             opts.hold,
		RemotePool:       rig.pool,
		RemotePeers:      opts.peers,
		OutageSpill:      opts.spill,
		PeerDownGrace:    opts.peerDownGrace,
		admission:        opts.admission,
		ActiveSessions:   opts.active,

		refugeeSlotsOverride: opts.refugeeSlots,
		ClientRetryAfterMax:  opts.retryAfterMax,
	}
	handler := server.Handler()
	if opts.public {
		handler = server.PublicHandler()
	}
	rig.daemon = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { rig.handled <- struct{}{} }()
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(rig.daemon.Close)
	return rig
}

// turn is one Claude Code model turn, authenticated with the daemon's proxy
// token exactly as a client configured by ANTHROPIC_AUTH_TOKEN sends it.
func (rig *holdRig) turn(t *testing.T, ctx context.Context, extra http.Header) (*http.Response, string, error) {
	t.Helper()
	return rig.post(t, ctx, "/v1/messages", extra)
}

// post is turn on another Claude path. /v1/messages/count_tokens has no
// usage failover, and so none of its same-account overload retries, which
// wait 1 s and 2 s on every Anthropic 5xx.
func (rig *holdRig) post(t *testing.T, ctx context.Context, path string, extra http.Header) (*http.Response, string, error) {
	t.Helper()
	return rig.send(t, ctx, path, `{"model":"claude-opus-5-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, extra)
}

func (rig *holdRig) send(t *testing.T, ctx context.Context, path, body string, extra http.Header) (*http.Response, string, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rig.daemon.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+holdTestProxyToken)
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", "hold-session")
	for key, values := range extra {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	got, readErr := io.ReadAll(resp.Body)
	return resp, string(got), readErr
}

func (rig *holdRig) countLog(fragment string) int {
	return strings.Count(rig.logs.String(), fragment)
}

func dnsFailure(host string) error {
	return &net.OpError{Op: "dial", Net: "tcp4", Err: &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}}
}

func refusedFailure() error {
	return &net.OpError{Op: "dial", Net: "tcp4", Err: &net.OpError{Op: "connect", Err: syscall.ECONNREFUSED}}
}

// The timeouts below are made by the standard library itself, never by a
// hand-written error type: a dial or a lookup that runs out of time answers
// errors.Is(err, context.DeadlineExceeded), and a stand-in without that
// method once let a classifier that dropped every deadline pass its test.
// Each helper checks the trap is present, so a fixture cannot lose it.

// realDialTimeout is the 10/20 log's `dial tcp4 160.79.104.10:443: i/o
// timeout`: a dial whose deadline has passed. An expired deadline sends
// nothing, and 192.0.2.1 is TEST-NET-1 besides.
func realDialTimeout(t *testing.T) error {
	t.Helper()
	conn, err := (&net.Dialer{Deadline: time.Now().Add(-time.Second)}).DialContext(context.Background(), "tcp4", "192.0.2.1:443")
	if err == nil {
		conn.Close()
		t.Fatal("a dial past its deadline connected")
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "dial" || !opErr.Timeout() || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want a dial *net.OpError that times out and is a context.DeadlineExceeded, got %T %v", err, err)
	}
	return err
}

// realLookupTimeout is `dial tcp4: lookup api.anthropic.com: i/o timeout`:
// the dial's clock runs out during the DNS lookup. The resolver never
// answers, and its only way out of this process is the Dial below, which
// waits for the clock.
func realLookupTimeout(t *testing.T) error {
	t.Helper()
	silent := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	conn, err := (&net.Dialer{Timeout: 10 * time.Millisecond, Resolver: silent}).DialContext(context.Background(), "tcp4", "api.anthropic.com:443")
	if err == nil {
		conn.Close()
		t.Fatal("a dial through a silent resolver connected")
	}
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) || !dnsErr.IsTimeout || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want a timed-out *net.DNSError that is a context.DeadlineExceeded, got %T %v", err, err)
	}
	return err
}

// realCanceledLookup is `dial tcp4: lookup api.anthropic.com: operation was
// canceled`: the client left during the lookup. A *net.DNSError, and a
// context.Canceled.
func realCanceledLookup(t *testing.T) error {
	t.Helper()
	silent := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn, err := (&net.Dialer{Resolver: silent}).DialContext(ctx, "tcp4", "api.anthropic.com:443")
	if err == nil {
		conn.Close()
		t.Fatal("a canceled dial connected")
	}
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) || !errors.Is(err, context.Canceled) {
		t.Fatalf("want a *net.DNSError that is a context.Canceled, got %T %v", err, err)
	}
	return err
}

// silentListener accepts connections and never writes to them.
func silentListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(io.Discard, conn)
				conn.Close()
			}()
		}
	}()
	return ln
}

// realTLSHandshakeTimeout is net/http's `TLS handshake timeout`, against a
// server that never answers the ClientHello.
func realTLSHandshakeTimeout(t *testing.T) error {
	t.Helper()
	ln := silentListener(t)
	transport := &http.Transport{TLSHandshakeTimeout: 10 * time.Millisecond}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequest(http.MethodGet, "https://"+ln.Addr().String()+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := transport.RoundTrip(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("a TLS handshake with a silent server completed")
	}
	if err.Error() != "net/http: TLS handshake timeout" {
		t.Fatalf("want net/http's TLS handshake timeout, got %T %v", err, err)
	}
	return err
}

// realHeaderTimeout is net/http's `timeout awaiting response headers`: the
// request was sent and the upstream did not answer in time. It is a
// context.DeadlineExceeded.
func realHeaderTimeout(t *testing.T) error {
	t.Helper()
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer upstream.Close()
	defer close(release)
	transport := &http.Transport{ResponseHeaderTimeout: 10 * time.Millisecond}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequest(http.MethodGet, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := transport.RoundTrip(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("an upstream that never answers answered")
	}
	if !strings.Contains(err.Error(), "timeout awaiting response headers") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want net/http's response-header timeout as a context.DeadlineExceeded, got %T %v", err, err)
	}
	return err
}

// realReadTimeout is `read tcp …: i/o timeout` on a connection that is up:
// the read deadline passed. It is NOT a context.DeadlineExceeded.
func realReadTimeout(t *testing.T) error {
	t.Helper()
	conn, err := net.Dial("tcp", silentListener(t).Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = conn.Read(make([]byte, 1))
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "read" || !opErr.Timeout() || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want a read *net.OpError that times out and is not a context.DeadlineExceeded, got %T %v", err, err)
	}
	return err
}

func answerOK(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"msg_held","type":"message"}`))
}

// The 10/20 outage, first half: `dial tcp4: lookup api.anthropic.com: no such
// host` for a few attempts, then the network is back. The client gets the
// answer, not a 502.
func TestHoldRidesOutDNSFailureUntilUpstreamAnswers(t *testing.T) {
	var hits atomic.Int64
	rig := newHoldRig(t, holdRigOptions{
		hold: holdTestPolicy(5 * time.Second),
		upstream: func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			if r.Header.Get("Authorization") != "Bearer tok-one" {
				http.Error(w, "the proxy token reached the upstream", http.StatusUnauthorized)
				return
			}
			answerOK(w, r)
		},
		failDials: func(n int64) error {
			if n <= 3 {
				return dnsFailure("api.anthropic.com")
			}
			return nil
		},
	})
	resp, body, err := rig.turn(t, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "msg_held") {
		t.Fatalf("status = %d body = %q, want the upstream's 200 after the DNS failures", resp.StatusCode, body)
	}
	if got := rig.dials.Load(); got != 4 {
		t.Fatalf("dials = %d, want 3 failed + 1 served", got)
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1", hits.Load())
	}
	logs := rig.logs.String()
	if rig.countLog("proxy hold started") != 1 || !strings.Contains(logs, "reason=upstream_unreachable:dns") {
		t.Fatalf("want one hold start naming the DNS failure; logs:\n%s", logs)
	}
	if rig.countLog("proxy hold ended") != 1 || !strings.Contains(logs, "outcome=answered") || !strings.Contains(logs, "attempts=4") {
		t.Fatalf("want one hold end, answered on attempt 4; logs:\n%s", logs)
	}
}

// The 10/20 outage, second half: the gateway refused connections, which
// reached clients as "502 upstream request failed".
func TestHoldRidesOutRefusedConnections(t *testing.T) {
	rig := newHoldRig(t, holdRigOptions{
		hold:     holdTestPolicy(5 * time.Second),
		upstream: answerOK,
		failDials: func(n int64) error {
			if n <= 2 {
				return refusedFailure()
			}
			return nil
		},
	})
	resp, body, err := rig.turn(t, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body = %q, want 200", resp.StatusCode, body)
	}
	if !strings.Contains(rig.logs.String(), "reason=upstream_unreachable:connection_refused") {
		t.Fatalf("hold start should name the refused dial; logs:\n%s", rig.logs.String())
	}
}

// The other half of the 10/20 dial failures: `dial tcp4 160.79.104.10:443:
// i/o timeout` (426 of them in the laptop log from 10/20 12:15, against 321
// refused), and its DNS twin `lookup api.anthropic.com: i/o timeout`. Both
// are context.DeadlineExceeded to errors.Is, and both are held. A request
// that never left gives back its debit.
func TestHoldRidesOutDialTimeouts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		reason string
	}{
		{"dial", realDialTimeout(t), "reason=upstream_unreachable:dial_timeout"},
		{"lookup", realLookupTimeout(t), "reason=upstream_unreachable:dns"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newHoldRig(t, holdRigOptions{
				hold:     holdTestPolicy(5 * time.Second),
				upstream: answerOK,
				failDials: func(n int64) error {
					if n <= 2 {
						return tc.err
					}
					return nil
				},
			})
			resp, body, err := rig.turn(t, context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK || !strings.Contains(body, "msg_held") {
				t.Fatalf("status = %d body = %q dials = %d, want the upstream's 200 after two timed-out dials (%v)", resp.StatusCode, body, rig.dials.Load(), tc.err)
			}
			if got := rig.dials.Load(); got != 3 {
				t.Fatalf("dials = %d, want 2 timed out + 1 served", got)
			}
			logs := rig.logs.String()
			if rig.countLog("proxy hold started") != 1 || !strings.Contains(logs, tc.reason) {
				t.Fatalf("want one hold start with %s; logs:\n%s", tc.reason, logs)
			}
			total := 0
			for _, n := range rig.sched.LiveDebits() {
				total += n
			}
			if total != 1 {
				t.Fatalf("live debits = %v, want 1 (the served attempt only)", rig.sched.LiveDebits())
			}
		})
	}
}

// A fake upstream that fails N times with a server error, then answers.
func TestHoldRetriesUpstreamServerErrorsThenServes(t *testing.T) {
	var hits atomic.Int64
	failures := []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout}
	rig := newHoldRig(t, holdRigOptions{
		hold: holdTestPolicy(5 * time.Second),
		upstream: func(w http.ResponseWriter, r *http.Request) {
			if n := hits.Add(1); n <= int64(len(failures)) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(failures[n-1])
				_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"upstream connect error"}}`))
				return
			}
			answerOK(w, r)
		},
	})
	resp, body, err := rig.post(t, context.Background(), "/v1/messages/count_tokens", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "msg_held") {
		t.Fatalf("status = %d body = %q, want 200 once the upstream recovers; logs:\n%s", resp.StatusCode, body, rig.logs.String())
	}
	if hits.Load() != 5 {
		t.Fatalf("upstream hits = %d, want 4 failures + 1", hits.Load())
	}
	logs := rig.logs.String()
	if !strings.Contains(logs, "reason=upstream_status:500") || !strings.Contains(logs, "last_reason=upstream_status:504") ||
		!strings.Contains(logs, "outcome=answered") || !strings.Contains(logs, "attempts=5") {
		t.Fatalf("want a hold from upstream_status:500 to 504, answered on attempt 5; logs:\n%s", logs)
	}
}

// On /v1/messages an Anthropic 5xx is first retried twice on the same account
// (the overload retries); the hold starts only once those give up, and its
// attempt runs them again.
func TestHoldComposesWithTheOverloadRetries(t *testing.T) {
	var hits atomic.Int64
	rig := newHoldRig(t, holdRigOptions{
		hold: holdTestPolicy(30 * time.Second),
		upstream: func(w http.ResponseWriter, r *http.Request) {
			if hits.Add(1) <= 3 {
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"upstream connect error"}}`))
				return
			}
			answerOK(w, r)
		},
	})
	resp, body, err := rig.turn(t, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body = %q, want 200; logs:\n%s", resp.StatusCode, body, rig.logs.String())
	}
	if hits.Load() != 4 || rig.countLog("retrying after provider overload") != 2 {
		t.Fatalf("hits = %d, want 1 + 2 overload retries, then the held attempt; logs:\n%s", hits.Load(), rig.logs.String())
	}
	if !strings.Contains(rig.logs.String(), "reason=upstream_status:503") || !strings.Contains(rig.logs.String(), "attempts=2") {
		t.Fatalf("want a hold on upstream_status:503 answered on attempt 2; logs:\n%s", rig.logs.String())
	}
}

// `502 refresh selected account: account token refresh did not complete:
// Post "https://platform.claude.com/v1/oauth/token": dial tcp4: lookup
// platform.claude.com: no such host` -- the refresh, not the turn, hit DNS.
func TestHoldRidesOutTokenRefreshNetworkFailure(t *testing.T) {
	var refreshes atomic.Int64
	rig := newHoldRig(t, holdRigOptions{
		hold:     holdTestPolicy(5 * time.Second),
		upstream: answerOK,
		refresh: func(_ context.Context, account accounts.Account) (accounts.Account, error) {
			if refreshes.Add(1) <= 2 {
				return account, &url.Error{Op: "Post", URL: "https://platform.claude.com/v1/oauth/token", Err: dnsFailure("platform.claude.com")}
			}
			return account, nil
		},
	})
	resp, body, err := rig.turn(t, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body = %q, want 200 after the refresh recovers", resp.StatusCode, body)
	}
	if refreshes.Load() != 3 {
		t.Fatalf("refreshes = %d, want 2 failed + 1", refreshes.Load())
	}
	if !strings.Contains(rig.logs.String(), "reason=token_refresh:dns") {
		t.Fatalf("hold start should name the refresh's DNS failure; logs:\n%s", rig.logs.String())
	}
}

// Once the client has the status line, the attempt IS the answer: a stream
// that breaks after its first event is not replayed.
func TestHoldNeverRetriesAfterFirstByte(t *testing.T) {
	var hits atomic.Int64
	rig := newHoldRig(t, holdRigOptions{
		hold: holdTestPolicy(5 * time.Second),
		upstream: func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))
			w.(http.Flusher).Flush()
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		},
	})
	resp, body, _ := rig.turn(t, context.Background(), nil)
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("response = %v, want the 200 the stream started with", resp)
	}
	if !strings.Contains(body, "message_start") {
		t.Fatalf("body = %q, want the event that was streamed before the break", body)
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1: nothing is retried after the first byte", hits.Load())
	}
	if rig.countLog("proxy hold started") != 0 {
		t.Fatalf("no hold may start after bytes reached the client; logs:\n%s", rig.logs.String())
	}
}

// An error body too large to keep is written as it streams, which ends the
// hold for that request.
func TestHoldDoesNotRetryAnErrorItHadToStream(t *testing.T) {
	var hits atomic.Int64
	big := strings.Repeat("x", holdMaxSwallowedBytes+1024)
	rig := newHoldRig(t, holdRigOptions{
		hold: holdTestPolicy(5 * time.Second),
		upstream: func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(big))
		},
	})
	resp, body, err := rig.post(t, context.Background(), "/v1/messages/count_tokens", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadGateway || len(body) != len(big) {
		t.Fatalf("status = %d len(body) = %d, want the upstream's 502 and its whole body", resp.StatusCode, len(body))
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1: no hold", hits.Load())
	}
	if rig.countLog("proxy hold started") != 0 {
		t.Fatalf("a streamed error must not start a hold; logs:\n%s", rig.logs.String())
	}
}

// At the deadline the client gets exactly what it got before holds existed.
// The deadline's clock jumps past it after the third failed dial, so the
// test does not depend on how fast an attempt runs on a loaded machine.
func TestHoldDeadlineAnswersTodaysError(t *testing.T) {
	alwaysDNS := func(int64) error { return dnsFailure("api.anthropic.com") }
	today := newHoldRig(t, holdRigOptions{upstream: answerOK, failDials: alwaysDNS})
	wantResp, wantBody, err := today.turn(t, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if today.dials.Load() != 1 {
		t.Fatalf("without a hold: dials = %d, want 1", today.dials.Load())
	}

	policy := holdTestPolicy(time.Minute)
	var pastDeadline atomic.Bool
	start := time.Now()
	policy.now = func() time.Time {
		if pastDeadline.Load() {
			return start.Add(time.Hour)
		}
		return start
	}
	held := newHoldRig(t, holdRigOptions{hold: policy, upstream: answerOK, failDials: func(n int64) error {
		if n == 3 {
			pastDeadline.Store(true)
		}
		return dnsFailure("api.anthropic.com")
	}})
	gotResp, gotBody, err := held.turn(t, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if held.dials.Load() != 3 {
		t.Fatalf("dials = %d, want 3: the hold retries until the deadline, then stops", held.dials.Load())
	}
	if gotResp.StatusCode != wantResp.StatusCode || gotBody != wantBody {
		t.Fatalf("held answer %d %q, want today's %d %q", gotResp.StatusCode, gotBody, wantResp.StatusCode, wantBody)
	}
	for _, header := range []string{"Content-Type", "X-Content-Type-Options"} {
		if gotResp.Header.Get(header) != wantResp.Header.Get(header) {
			t.Fatalf("%s = %q, want today's %q", header, gotResp.Header.Get(header), wantResp.Header.Get(header))
		}
	}
	logs := held.logs.String()
	if !strings.Contains(logs, "outcome=deadline") || !strings.Contains(logs, "attempts=3") || !strings.Contains(logs, "status=502") {
		t.Fatalf("hold end should say deadline after 3 attempts with today's 502; logs:\n%s", logs)
	}
}

// The token-refresh 502 keeps its X-Subrouter-Transient and Retry-After at the
// deadline too: a peer reads that header as "no verdict".
func TestHoldDeadlineKeepsTheRefreshFailureHeaders(t *testing.T) {
	failing := func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		return account, &url.Error{Op: "Post", URL: "https://platform.claude.com/v1/oauth/token", Err: dnsFailure("platform.claude.com")}
	}
	rig := newHoldRig(t, holdRigOptions{hold: holdTestPolicy(150 * time.Millisecond), upstream: answerOK, refresh: failing})
	resp, body, err := rig.turn(t, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get(remotePoolTransientHeader) != "1" || resp.Header.Get("Retry-After") != "2" {
		t.Fatalf("status = %d headers = %v, want today's 502 + X-Subrouter-Transient + Retry-After", resp.StatusCode, resp.Header)
	}
	if !strings.Contains(body, "account token refresh did not complete") {
		t.Fatalf("body = %q, want today's refresh error", body)
	}
}

// The client closing the connection ends the hold at once.
func TestHoldEndsWhenTheClientGoesAway(t *testing.T) {
	policy := holdTestPolicy(time.Minute)
	rig := newHoldRig(t, holdRigOptions{
		hold:      policy,
		upstream:  answerOK,
		failDials: func(int64) error { return dnsFailure("api.anthropic.com") },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clientDone := make(chan error, 1)
	go func() {
		_, _, err := rig.turn(t, ctx, nil)
		clientDone <- err
	}()
	waitFor(t, "the hold to start", func() bool { return policy.Active() == 1 })
	cancel()
	if err := <-clientDone; err == nil {
		t.Fatal("the client gave up; Do should report it")
	}
	select {
	case <-rig.handled:
	case <-time.After(5 * time.Second):
		t.Fatal("the hold outlived the client by 5s")
	}
	if policy.Active() != 0 {
		t.Fatalf("active holds = %d after the client left, want 0", policy.Active())
	}
	dialsAtEnd := rig.dials.Load()
	time.Sleep(100 * time.Millisecond)
	if rig.dials.Load() != dialsAtEnd {
		t.Fatal("attempts continued after the hold ended")
	}
	if !strings.Contains(rig.logs.String(), "outcome=client_canceled") {
		t.Fatalf("hold end should say client_canceled; logs:\n%s", rig.logs.String())
	}
}

// A client that hangs up the moment it has the held turn's answer was
// answered; one that left before the answer went out was not. The handler
// here stands in for the proxy's and the test cancels the request itself,
// because over a real socket the hang-up races the end of the handler.
func TestHoldEndSaysAnsweredWhenTheClientHangsUpAfterTheAnswer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		hangUp  func(w http.ResponseWriter, leave context.CancelFunc)
		wantEnd string
	}{
		{"after the answer", func(w http.ResponseWriter, leave context.CancelFunc) {
			_, _ = io.WriteString(w, `{"id":"msg_upstream"}`)
			leave()
		}, "outcome=answered status=200 attempts=2"},
		{"before the answer", func(w http.ResponseWriter, leave context.CancelFunc) {
			leave()
			http.Error(w, "upstream request failed: context canceled", http.StatusBadGateway)
		}, "outcome=client_canceled status=502 attempts=2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &syncBuffer{}
			server := Server{
				Hold:            holdTestPolicy(time.Minute),
				LocalProxyToken: holdTestProxyToken,
				Logger:          slog.New(slog.NewTextHandler(logs, nil)),
			}
			ctx, leave := context.WithCancel(context.Background())
			defer leave()
			var attempts atomic.Int64
			held := server.holdOnTransientFailure(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if attempts.Add(1) == 1 {
					noteHoldable(r.Context(), holdMark{status: http.StatusBadGateway, reason: "upstream_unreachable:dns"})
					http.Error(w, "upstream request failed", http.StatusBadGateway)
					return
				}
				tc.hangUp(w, leave)
			}))
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5"}`))
			req.Header.Set("Authorization", "Bearer "+holdTestProxyToken)
			held.ServeHTTP(httptest.NewRecorder(), req)
			if !strings.Contains(logs.String(), tc.wantEnd) {
				t.Fatalf("want the hold's end line to say %q; logs:\n%s", tc.wantEnd, logs.String())
			}
		})
	}
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A drain (a restart) ends every hold: the client retries the next process.
func TestHoldEndsWhenTheDaemonDrains(t *testing.T) {
	lifecycle := NewLifecycle()
	rig := newHoldRig(t, holdRigOptions{
		hold:      holdTestPolicy(time.Minute),
		upstream:  answerOK,
		lifecycle: lifecycle,
		failDials: func(n int64) error {
			if n == 2 {
				lifecycle.Drain()
			}
			return dnsFailure("api.anthropic.com")
		},
	})
	resp, _, err := rig.turn(t, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want today's 502 when the drain ends the hold", resp.StatusCode)
	}
	if !strings.Contains(rig.logs.String(), "outcome=draining") {
		t.Fatalf("hold end should say draining; logs:\n%s", rig.logs.String())
	}
}

// 429 and 529 keep their existing path: account failover for a spent
// account, the overload retries for 529, and then the client gets the
// answer. The hold never starts, and the upstream sees what it saw before.
func TestHoldLeaves429And529Alone(t *testing.T) {
	twoAccounts := []accounts.Account{
		{ID: "cooked@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-cooked"},
		{ID: "fresh@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-fresh"},
	}
	type outcome struct {
		status     int
		body       string
		hits       string
		holdStarts int
	}
	run := func(t *testing.T, hold *HoldPolicy, upstream func(*sync.Map) http.HandlerFunc) outcome {
		hits := &sync.Map{}
		rig := newHoldRig(t, holdRigOptions{hold: hold, accounts: twoAccounts, pin: "cooked@example.com", upstream: upstream(hits)})
		resp, body, err := rig.turn(t, context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		var counts []string
		for _, token := range []string{"tok-cooked", "tok-fresh"} {
			n, _ := hits.Load(token)
			counts = append(counts, fmt.Sprintf("%s=%v", token, n))
		}
		return outcome{status: resp.StatusCode, body: body, hits: strings.Join(counts, " "), holdStarts: rig.countLog("proxy hold started")}
	}
	count := func(hits *sync.Map, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		n, _ := hits.LoadOrStore(token, new(atomic.Int64))
		n.(*atomic.Int64).Add(1)
	}
	cases := map[string]func(*sync.Map) http.HandlerFunc{
		"429 fails over": func(hits *sync.Map) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				count(hits, r)
				_, _ = io.Copy(io.Discard, r.Body)
				if r.Header.Get("Authorization") == "Bearer tok-cooked" {
					w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"limit"}}`))
					return
				}
				_, _ = w.Write([]byte(`{"id":"msg_fresh"}`))
			}
		},
		"429 everywhere reaches the client": func(hits *sync.Map) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				count(hits, r)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"limit"}}`))
			}
		},
		"529 reaches the client": func(hits *sync.Map) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				count(hits, r)
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(529)
				_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
			}
		},
	}
	for name, upstream := range cases {
		t.Run(name, func(t *testing.T) {
			without := run(t, nil, upstream)
			with := run(t, holdTestPolicy(5*time.Second), upstream)
			if with.holdStarts != 0 {
				t.Fatalf("a hold started on %s", name)
			}
			if with.status != without.status || with.body != without.body || with.hits != without.hits {
				t.Fatalf("with the hold: %d %q [%s]; without: %d %q [%s]", with.status, with.body, with.hits, without.status, without.body, without.hits)
			}
		})
	}
}

// A request another daemon forwarded is never held here: the forwarding
// daemon decides, and a peer that goes quiet for minutes reads as a dead one.
func TestHoldNeverHoldsAPeerForward(t *testing.T) {
	rig := newHoldRig(t, holdRigOptions{
		hold:      holdTestPolicy(5 * time.Second),
		upstream:  answerOK,
		failDials: func(int64) error { return dnsFailure("api.anthropic.com") },
	})
	resp, _, err := rig.turn(t, context.Background(), http.Header{remotePoolHopHeader: {"1"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadGateway || rig.dials.Load() != 1 {
		t.Fatalf("status = %d dials = %d, want an immediate 502 after one dial", resp.StatusCode, rig.dials.Load())
	}
}

// The public listener (the box's :31416) never holds: it is reachable from the
// internet, and the peer daemons that call it hold their own turns.
func TestHoldNeverHoldsOnThePublicListener(t *testing.T) {
	rig := newHoldRig(t, holdRigOptions{
		hold:      holdTestPolicy(5 * time.Second),
		upstream:  answerOK,
		public:    true,
		failDials: func(int64) error { return dnsFailure("api.anthropic.com") },
	})
	resp, _, err := rig.turn(t, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadGateway || rig.dials.Load() != 1 {
		t.Fatalf("status = %d dials = %d, want an immediate 502 after one dial", resp.StatusCode, rig.dials.Load())
	}
}

// A caller the proxy refuses gets its 401 before a byte of its body is read:
// the hold buffers only an authorized request.
func TestHoldReadsNoBodyBeforeTheCallerIsAuthorized(t *testing.T) {
	server := Server{
		LocalProxyToken: holdTestProxyToken,
		MaxBodyBytes:    1 << 20,
		Hold:            holdTestPolicy(5 * time.Second),
	}
	body := &countingReader{Reader: strings.NewReader(strings.Repeat("x", 4096))}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:31415/v1/messages", body)
	// A known length, as Claude Code sends: a chunked body is never held, so
	// without it this test could not fail.
	req.ContentLength = 4096
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer not-the-token")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if body.n.Load() != 0 {
		t.Fatalf("read %d body bytes before refusing the caller, want 0", body.n.Load())
	}
}

type countingReader struct {
	io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// Past the cap a failure is answered at once, and the refusal is logged.
func TestHoldCapAnswersTheOverflowAtOnce(t *testing.T) {
	policy := holdTestPolicy(time.Minute)
	policy.MaxRequests = 1
	rig := newHoldRig(t, holdRigOptions{
		hold:      policy,
		upstream:  answerOK,
		failDials: func(int64) error { return dnsFailure("api.anthropic.com") },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _, _ = rig.turn(t, ctx, nil) }()
	waitFor(t, "the first request's hold", func() bool { return policy.Active() == 1 })
	started := time.Now()
	resp, _, err := rig.turn(t, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadGateway || time.Since(started) > 5*time.Second {
		t.Fatalf("status = %d after %v, want an immediate 502", resp.StatusCode, time.Since(started))
	}
	if !strings.Contains(rig.logs.String(), "refused=max_requests") {
		t.Fatalf("want the refusal logged; logs:\n%s", rig.logs.String())
	}
}

// An attempt that never left gives its live debit back: a held request that
// failed twice and was then served debits its account once, not three times.
func TestHoldRefundsTheDebitOfAttemptsThatNeverLeft(t *testing.T) {
	rig := newHoldRig(t, holdRigOptions{
		hold:     holdTestPolicy(5 * time.Second),
		upstream: answerOK,
		failDials: func(n int64) error {
			if n <= 2 {
				return dnsFailure("api.anthropic.com")
			}
			return nil
		},
	})
	resp, _, err := rig.turn(t, context.Background(), nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("turn: %v %v", resp, err)
	}
	total := 0
	for _, n := range rig.sched.LiveDebits() {
		total += n
	}
	if total != 1 {
		t.Fatalf("live debits = %v, want 1 (the served attempt only)", rig.sched.LiveDebits())
	}
}

func TestHoldBackoffDoublesWithJitterAndCaps(t *testing.T) {
	policy := &HoldPolicy{Deadline: time.Minute}
	for n := 1; n <= 40; n++ {
		low, high := policy.backoff(n, func() float64 { return 0 }), policy.backoff(n, func() float64 { return 0.999999 })
		want := holdMaxBackoff
		if n <= 5 {
			want = holdBaseBackoff << (n - 1)
		}
		if low != want/2 || high > want || high < want-time.Millisecond {
			t.Fatalf("attempt %d: wait in [%v, %v], want [%v, %v]", n, low, high, want/2, want)
		}
	}
}

func TestTransientNetworkCause(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		cause     string
		neverSent bool
		ok        bool
	}{
		{"dns", dnsFailure("api.anthropic.com"), "dns", true, true},
		{"dns inside a url error", &url.Error{Op: "Post", URL: "https://platform.claude.com", Err: dnsFailure("platform.claude.com")}, "dns", true, true},
		{"refused", refusedFailure(), "connection_refused", true, true},
		{"dial timeout", realDialTimeout(t), "dial_timeout", true, true},
		{"lookup timeout", realLookupTimeout(t), "dns", true, true},
		{"lookup timeout inside a url error", &url.Error{Op: "Post", URL: "https://platform.claude.com/v1/oauth/token", Err: realLookupTimeout(t)}, "dns", true, true},
		{"addr not available", &net.OpError{Op: "read", Net: "tcp4", Err: syscall.EADDRNOTAVAIL}, "connection", false, true},
		{"reset", &net.OpError{Op: "read", Net: "tcp4", Err: syscall.ECONNRESET}, "connection", false, true},
		{"unexpected eof", io.ErrUnexpectedEOF, "connection_closed", false, true},
		{"tls handshake timeout", realTLSHandshakeTimeout(t), "tls_handshake_timeout", true, true},
		{"read timeout", realReadTimeout(t), "timeout", false, true},
		// Sent, and then a clock this process set ran out: not a network
		// that is down, and Claude Code's own 600 s wait is the bound.
		{"header timeout", realHeaderTimeout(t), "", false, false},
		{"client cancel", context.Canceled, "", false, false},
		{"lookup canceled", realCanceledLookup(t), "", false, false},
		{"deadline", fmt.Errorf("wrapped: %w", context.DeadlineExceeded), "", false, false},
		{"bad certificate", &url.Error{Op: "Post", URL: "https://api.anthropic.com", Err: x509.UnknownAuthorityError{}}, "", false, false},
		{"local error", errors.New("persist successful session reassignment: disk full"), "", false, false},
	}
	for _, tc := range cases {
		cause, neverSent, ok := transientNetworkCause(tc.err)
		if cause != tc.cause || neverSent != tc.neverSent || ok != tc.ok {
			t.Errorf("%s: got (%q, %v, %v), want (%q, %v, %v)", tc.name, cause, neverSent, ok, tc.cause, tc.neverSent, tc.ok)
		}
	}
}

// A spent local pool sends every turn to a peer, and a gateway can time out or
// refuse the dials to all of them. A peer this daemon cannot reach is waited
// out like an unreachable api.anthropic.com, not answered with the
// pool-exhausted 503, whose Retry-After Claude Code does not wait for.

// cutPeer reads pool's usage, as the daemon had it before the outage, then
// puts the gateway in the way: the nth dial to the peer fails with fail(n),
// or connects when that is nil. Every forward dials.
func cutPeer(t *testing.T, pool *RemotePool, fail func(n int64) error) *atomic.Int64 {
	t.Helper()
	if snapshot := pool.usageForDecision(time.Now().Add(5 * time.Second)); !snapshot.Reachable {
		t.Fatalf("the peer's usage read failed before the outage: %s", snapshot.Err)
	}
	transport := pool.client.Transport.(*http.Transport)
	transport.CloseIdleConnections()
	transport.DisableKeepAlives = true
	dial := transport.DialContext
	dials := new(atomic.Int64)
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if err := fail(dials.Add(1)); err != nil {
			return nil, err
		}
		return dial(ctx, network, addr)
	}
	return dials
}

func peerDownFor(dials int64, err error) func(int64) error {
	return func(n int64) error {
		if n <= dials {
			return err
		}
		return nil
	}
}

func noLocalUpstream(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a spent pool reached the upstream: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func eligibleBoxAccount(id string) boxAccount {
	score := crossPoolClaudeScore(id, 1, 30*time.Hour)
	score.ShortHeadroom = 1
	return boxAccount{Score: score}
}

// spentPoolAnswer is what a client got from a daemon whose pool is spent.
type spentPoolAnswer struct {
	status     int
	body       string
	retryAfter string
	reason     string
}

var digits = regexp.MustCompile(`\d+`)

// spentPoolAnswerOf keeps what the client sees, with the seconds to the reset
// written as N: two daemons a moment apart count down from the same reset.
func spentPoolAnswerOf(resp *http.Response, body string) spentPoolAnswer {
	return spentPoolAnswer{
		status:     resp.StatusCode,
		body:       digits.ReplaceAllString(body, "N"),
		retryAfter: digits.ReplaceAllString(resp.Header.Get("Retry-After"), "N"),
		reason:     resp.Header.Get(subrouterReasonHeader),
	}
}

// Both peers time out or refuse the dial, as on 11/19: the turn reaches the
// refusal bound and local cannot serve it. It is held, and the client gets
// the peer's answer once a dial gets through.
func TestHoldRidesOutUnreachablePeersWhenThePoolIsSpent(t *testing.T) {
	boxA := newContractBox(t, eligibleBoxAccount("acct-a"))
	boxB := newContractBox(t, eligibleBoxAccount("acct-b"))
	dialsA := cutPeer(t, boxA.pool, peerDownFor(1, realDialTimeout(t)))
	dialsB := cutPeer(t, boxB.pool, peerDownFor(1, refusedFailure()))
	rig := newHoldRig(t, holdRigOptions{
		hold:      holdTestPolicy(5 * time.Second),
		upstream:  noLocalUpstream(t),
		exhausted: true,
		peers:     []*RemotePool{boxA.pool, boxB.pool},
	})
	resp, body, err := rig.turn(t, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rig.settle(t, 1)
	logs := rig.logs.String()
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "msg_box") {
		t.Fatalf("status = %d body = %q, want a peer's 200 once its dial got through; logs:\n%s", resp.StatusCode, body, logs)
	}
	if dialsA.Load() < 1 || dialsB.Load() < 1 {
		t.Fatalf("dials A=%d B=%d: the first attempt should fail at both peers' dials; logs:\n%s", dialsA.Load(), dialsB.Load(), logs)
	}
	if served := len(boxA.seen()) + len(boxB.seen()); served != 1 {
		t.Fatalf("the peers saw %d turns, want 1", served)
	}
	if rig.countLog("proxy hold started") != 1 || !strings.Contains(logs, "reason=peers_unreachable:pool_exhausted") {
		t.Fatalf("want one hold start naming the unreachable peers; logs:\n%s", logs)
	}
	if rig.countLog("proxy hold ended") != 1 || !strings.Contains(logs, "outcome=answered") {
		t.Fatalf("want one hold end, answered; logs:\n%s", logs)
	}
}

func setBreakerOpenUntil(pool *RemotePool, until time.Time) {
	pool.breaker.mu.Lock()
	pool.breaker.openUntil = until
	pool.breaker.mu.Unlock()
}

// After the outage's first strikes the breakers are open and nothing is
// dialed at all. That turn is held too, and goes to the peer once its
// breaker lets it. The breaker closes at the worst moment for the hold: the
// second attempt has skipped the peer for it and has not been answered yet.
// The hold's clock stands still, so only the peer ends the hold.
func TestHoldWaitsOutAnOpenPeerBreakerWhenThePoolIsSpent(t *testing.T) {
	policy := holdTestPolicy(5 * time.Second)
	policy.now = standingClock()
	var peer atomic.Pointer[RemotePool]
	var refusals atomic.Int64
	rig := newHoldRig(t, holdRigOptions{
		hold:      policy,
		upstream:  noLocalUpstream(t),
		exhausted: true,
		peer:      peerAnswer,
		onLog: func(record slog.Record) {
			if record.Message == "account selection failed" && refusals.Add(1) == 2 {
				setBreakerOpenUntil(peer.Load(), time.Time{})
			}
		},
	})
	peer.Store(rig.pool)
	dials := cutPeer(t, rig.pool, func(int64) error { return nil })
	setBreakerOpenUntil(rig.pool, time.Now().Add(time.Hour))
	resp, body, err := rig.turn(t, promptly(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	rig.settle(t, 1)
	logs := rig.logs.String()
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "msg_peer") {
		t.Fatalf("status = %d body = %q, want the peer's 200 after its breaker closed; logs:\n%s", resp.StatusCode, body, logs)
	}
	if dials.Load() != 1 || rig.peerTurns.Load() != 1 {
		t.Fatalf("dials = %d peer turns = %d, want 1 each: nothing is dialed while the breaker is open", dials.Load(), rig.peerTurns.Load())
	}
	if rig.countLog("proxy hold started") != 1 || !strings.Contains(logs, "reason=peers_unreachable:pool_exhausted") ||
		rig.countLog("outcome=answered status=200 attempts=3 ") != 1 {
		t.Fatalf("want one hold, answered by the attempt after the breaker closed; logs:\n%s", logs)
	}
}

// retryAfterOver60 reports a Retry-After that Claude Code shows to the user
// and stops on, instead of retrying.
func retryAfterOver60(resp *http.Response) bool {
	seconds, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	return err == nil && seconds > 60
}

func TestASpentPoolHoldAnswers502HoweverItEnds(t *testing.T) {
	today := newHoldRig(t, holdRigOptions{upstream: noLocalUpstream(t), exhausted: true, peer: peerAnswer})
	cutPeer(t, today.pool, func(int64) error { return refusedFailure() })
	todayResp, todayBody, err := today.turn(t, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if unheld := spentPoolAnswerOf(todayResp, todayBody); unheld.status != http.StatusServiceUnavailable || !retryAfterOver60(todayResp) ||
		unheld.reason != "pool_exhausted" || !strings.HasPrefix(todayBody, "no non-exhausted claude accounts available; next account frees up in") {
		t.Fatalf("calibration: the unheld answer is %+v with Retry-After %q, want the spent pool's 503 with one over 60 s",
			unheld, todayResp.Header.Get("Retry-After"))
	}
	for _, end := range []struct {
		name         string
		maxRequests  int
		dial         func(t *testing.T) error
		atSecondDial func(*Lifecycle, *movableClock)
		logged       string
	}{
		{"deadline", 8, nil, func(_ *Lifecycle, clock *movableClock) { clock.set(warmWaitStart.Add(time.Hour)) }, "outcome=deadline status=502"},
		{"deadline after dial timeouts", 8, realDialTimeout, func(_ *Lifecycle, clock *movableClock) { clock.set(warmWaitStart.Add(time.Hour)) }, "outcome=deadline status=502"},
		{"no room", 0, nil, nil, "proxy hold refused"},
		{"drain", 8, nil, func(lifecycle *Lifecycle, _ *movableClock) { lifecycle.Drain() }, "outcome=draining status=502"},
		{"shutdown", 8, nil, func(lifecycle *Lifecycle, _ *movableClock) { lifecycle.BeginShutdown() }, "outcome=shutdown_502 status=502"},
	} {
		t.Run(end.name, func(t *testing.T) {
			clock := newMovableClock(warmWaitStart)
			policy := holdTestPolicy(time.Minute)
			policy.MaxRequests = end.maxRequests
			policy.now = clock.now
			lifecycle := NewLifecycle()
			rig := newHoldRig(t, holdRigOptions{hold: policy, upstream: noLocalUpstream(t), exhausted: true, peer: peerAnswer, lifecycle: lifecycle})
			failure := refusedFailure()
			if end.dial != nil {
				failure = end.dial(t)
			}
			cutPeer(t, rig.pool, func(n int64) error {
				if n == 2 && end.atSecondDial != nil {
					end.atSecondDial(lifecycle, clock)
				}
				return failure
			})
			resp, body, err := rig.turn(t, context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			rig.settle(t, 1)
			logs := rig.logs.String()
			if resp.StatusCode != http.StatusBadGateway || retryAfterOver60(resp) {
				t.Fatalf("status = %d Retry-After %q body %q, want a 502 Claude Code retries; logs:\n%s",
					resp.StatusCode, resp.Header.Get("Retry-After"), body, logs)
			}
			if !strings.Contains(logs, "reason=peers_unreachable:pool_exhausted") || !strings.Contains(logs, end.logged) {
				t.Fatalf("want the spent pool's hold ended with %q; logs:\n%s", end.logged, logs)
			}
			if started, want := rig.countLog("proxy hold started"), min(end.maxRequests, 1); started != want {
				t.Fatalf("%d proxy hold started lines, want %d; logs:\n%s", started, want, logs)
			}
		})
	}
}

// A Fable turn whose Opus substitution found no local account either goes
// to the peer as the client sent it, on every attempt of the hold.
func TestPeerOutageHoldSendsThePeerTheFableTurnTheClientSent(t *testing.T) {
	var mu sync.Mutex
	var peerBodies []string
	rig := newHoldRig(t, holdRigOptions{
		hold:      holdTestPolicy(5 * time.Second),
		upstream:  noLocalUpstream(t),
		exhausted: true,
		peer: func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			peerBodies = append(peerBodies, string(body))
			mu.Unlock()
			peerAnswer(w, r)
		},
	})
	cutPeer(t, rig.pool, peerDownFor(1, refusedFailure()))
	sent := `{"model":"claude-fable-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	resp, body, err := rig.send(t, context.Background(), "/v1/messages", sent, nil)
	if err != nil {
		t.Fatal(err)
	}
	rig.settle(t, 1)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "msg_peer") {
		t.Fatalf("status = %d body = %q, want the peer's 200; logs:\n%s", resp.StatusCode, body, rig.logs.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(peerBodies) != 1 || peerBodies[0] != sent {
		t.Fatalf("the peer got %q, want exactly the client's Fable turn %q", peerBodies, sent)
	}
	if rig.countLog("proxy hold started") != 1 || rig.countLog("outcome=answered") != 1 {
		t.Fatalf("want one hold, answered; logs:\n%s", rig.logs.String())
	}
}

func TestPeerOutageHoldEndsOnClientCancelAndRespectsTheCap(t *testing.T) {
	policy := holdTestPolicy(time.Minute)
	policy.MaxRequests = 1
	rig := newHoldRig(t, holdRigOptions{hold: policy, upstream: noLocalUpstream(t), exhausted: true, peer: peerAnswer})
	cutPeer(t, rig.pool, func(int64) error { return refusedFailure() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clientDone := make(chan error, 1)
	go func() {
		_, _, err := rig.turn(t, ctx, nil)
		clientDone <- err
	}()
	waitFor(t, "the first turn's hold", func() bool { return policy.Active() == 1 })

	started := time.Now()
	resp, body, err := rig.turn(t, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rig.settle(t, 1)
	if resp.StatusCode != http.StatusBadGateway || retryAfterOver60(resp) || time.Since(started) > 5*time.Second {
		t.Fatalf("status = %d Retry-After %q body = %q after %v, want an immediate 502", resp.StatusCode, resp.Header.Get("Retry-After"), body, time.Since(started))
	}
	if !strings.Contains(rig.logs.String(), "refused=max_requests") {
		t.Fatalf("want the refusal logged; logs:\n%s", rig.logs.String())
	}

	cancel()
	if err := <-clientDone; err == nil {
		t.Fatal("the client gave up; Do should report it")
	}
	rig.settle(t, 1)
	if policy.Active() != 0 || !strings.Contains(rig.logs.String(), "outcome=client_canceled") {
		t.Fatalf("active holds = %d, want 0 and a client_canceled hold end; logs:\n%s", policy.Active(), rig.logs.String())
	}
}

// A spent pool that a peer answered for -- a decline, a quota answer, a
// refusal of the target -- is answered at once, as before: the pool really
// is out, and the hold must not sit on it for minutes.
func TestSpentPoolAnswersAtOnceWhenAPeerAnswered(t *testing.T) {
	answers := map[string]http.HandlerFunc{
		"peer declines with 503": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "no non-exhausted claude accounts available", http.StatusServiceUnavailable)
		},
		"peer answers 429": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
		},
		"peer refuses the target": func(w http.ResponseWriter, r *http.Request) {
			writeTargetRefusal(w, "peer-acct", selectacct.VerdictExhausted, false, nil)
		},
	}
	for name, answer := range answers {
		t.Run(name, func(t *testing.T) {
			run := func(hold *HoldPolicy) (spentPoolAnswer, *holdRig) {
				rig := newHoldRig(t, holdRigOptions{hold: hold, upstream: noLocalUpstream(t), exhausted: true, peer: answer})
				cutPeer(t, rig.pool, func(int64) error { return nil })
				resp, body, err := rig.turn(t, context.Background(), nil)
				if err != nil {
					t.Fatal(err)
				}
				rig.settle(t, 1)
				return spentPoolAnswerOf(resp, body), rig
			}
			without, _ := run(nil)
			with, rig := run(holdTestPolicy(5 * time.Second))
			if with != without || with.status != http.StatusServiceUnavailable || with.reason != "pool_exhausted" {
				t.Fatalf("with the hold %+v, without %+v: want the same pool-exhausted 503", with, without)
			}
			if rig.countLog("proxy hold started") != 0 || rig.peerTurns.Load() != 1 {
				t.Fatalf("peer turns = %d, want 1 and no hold; logs:\n%s", rig.peerTurns.Load(), rig.logs.String())
			}
		})
	}
	t.Run("one peer unreachable, the other refuses", func(t *testing.T) {
		boxA := newContractBox(t, eligibleBoxAccount("acct-a"))
		boxB := newContractBox(t, eligibleBoxAccount("acct-b"))
		boxB.refuse["acct-b"] = selectacct.VerdictExhausted
		boxB.declineUntargeted = true
		cutPeer(t, boxA.pool, func(int64) error { return realDialTimeout(t) })
		cutPeer(t, boxB.pool, func(int64) error { return nil })
		rig := newHoldRig(t, holdRigOptions{
			hold:      holdTestPolicy(5 * time.Second),
			upstream:  noLocalUpstream(t),
			exhausted: true,
			peers:     []*RemotePool{boxA.pool, boxB.pool},
		})
		started := time.Now()
		resp, body, err := rig.turn(t, context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		rig.settle(t, 1)
		if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get(subrouterReasonHeader) != "pool_exhausted" || time.Since(started) > 5*time.Second {
			t.Fatalf("status = %d body = %q after %v, want an immediate pool-exhausted 503", resp.StatusCode, body, time.Since(started))
		}
		turns := boxB.seen()
		if len(turns) == 0 || turns[0].Status != http.StatusConflict {
			t.Fatalf("box B saw %+v, want its target refusal first", turns)
		}
		for _, turn := range turns {
			if turn.Status == http.StatusOK {
				t.Fatalf("box B saw %+v, want no turn served", turns)
			}
		}
		if rig.countLog("proxy hold started") != 0 {
			t.Fatalf("a hold started though a peer answered; logs:\n%s", rig.logs.String())
		}
	})
}

// A peer that took the request and then said nothing until the header
// timeout may be running the turn. It is never sent again (PATCHES §58): a
// forward of a spent pool's turn is not held. A session the peer already
// holds is a stranded chat (PATCHES §67): its turn waits for the peer
// without being sent there again, and the hold's deadline answers 502. The
// held session's peer has failed once before, so the timeout opens its
// breaker.
func TestSpentPoolNeverHoldsATurnAPeerMayBeRunning(t *testing.T) {
	quiet := func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}
	newQuietRig := func(t *testing.T) (*holdRig, *HoldPolicy) {
		policy := holdTestPolicy(5 * time.Second)
		rig := newHoldRig(t, holdRigOptions{hold: policy, upstream: noLocalUpstream(t), exhausted: true, peer: quiet})
		cutPeer(t, rig.pool, func(int64) error { return nil })
		rig.pool.client.Transport.(*http.Transport).ResponseHeaderTimeout = 50 * time.Millisecond
		return rig, policy
	}
	immediate503 := func(t *testing.T, rig *holdRig) {
		t.Helper()
		started := time.Now()
		resp, body, err := rig.turn(t, context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		rig.settle(t, 1)
		if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get(subrouterReasonHeader) != "pool_exhausted" || time.Since(started) > 5*time.Second {
			t.Fatalf("status = %d body = %q after %v, want an immediate pool-exhausted 503", resp.StatusCode, body, time.Since(started))
		}
	}
	t.Run("forwarded", func(t *testing.T) {
		rig, _ := newQuietRig(t)
		immediate503(t, rig)
		if rig.peerTurns.Load() != 1 || rig.countLog("proxy hold started") != 0 {
			t.Fatalf("peer turns = %d, want 1 and no hold; logs:\n%s", rig.peerTurns.Load(), rig.logs.String())
		}
	})
	t.Run("held on the peer", func(t *testing.T) {
		rig, policy := newQuietRig(t)
		immediate503(t, rig)
		holdOn(t, rig.pool, "hold-session", "peer-acct")
		clock := newMovableClock(warmWaitStart)
		policy.now = clock.now
		answer := rig.sendAsync(t, holdTurnBody)
		awaitHeld(t, rig, policy, answer, "a stranded chat's turn its box may be running")
		heldFor(t, rig, answer, attempts{count: clock.reads.Load, kick: func() {}}, 20, "the box may be running it")
		if rig.pool.breaker.allow(time.Now()) {
			t.Fatal("the second timeout should have opened the peer's breaker")
		}
		if n := rig.peerTurns.Load(); n != 2 {
			t.Fatalf("peer turns = %d, want 2: the box may be running the held turn and is never sent it again", n)
		}
		clock.set(warmWaitStart.Add(5*time.Second + time.Millisecond))
		retryable(t, <-answer, http.StatusBadGateway)
	})
}

// What the peer outage never holds, with its one peer refusing every dial
// and its breaker open: a side request (count_tokens), a canary
// (X-Subrouter-No-Retry), a turn pinned to an account, a caller the peer is
// not for, and a daemon with no peer. Their spent pool answers at once as
// it always did.
func TestPeerOutageHoldLeavesUnheldTurnsAlone(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		extra     http.Header
		peer      http.HandlerFunc
		allowFrom string
	}{
		{"count_tokens", "/v1/messages/count_tokens", nil, peerAnswer, ""},
		{"canary", "/v1/messages", http.Header{subrouterNoRetryHeader: {"1"}}, peerAnswer, ""},
		{"pinned account", "/v1/messages", http.Header{"X-Subrouter-Account-ID": {"nobody@example.com"}}, peerAnswer, ""},
		{"peer not for this caller", "/v1/messages", nil, peerAnswer, "100.64.0.0/10"},
		{"no peer", "/v1/messages", nil, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := func(hold *HoldPolicy) (spentPoolAnswer, *holdRig) {
				rig := newHoldRig(t, holdRigOptions{hold: hold, upstream: noLocalUpstream(t), exhausted: true, peer: tc.peer})
				if rig.pool != nil {
					cutPeer(t, rig.pool, func(int64) error { return realDialTimeout(t) })
					rig.pool.breaker.mu.Lock()
					rig.pool.breaker.openUntil = time.Now().Add(time.Minute)
					rig.pool.breaker.mu.Unlock()
					if tc.allowFrom != "" {
						rig.pool.AllowFrom = parseAllowFrom(tc.allowFrom)
					}
				}
				resp, body, err := rig.post(t, context.Background(), tc.path, tc.extra)
				if err != nil {
					t.Fatal(err)
				}
				rig.settle(t, 1)
				return spentPoolAnswerOf(resp, body), rig
			}
			without, _ := run(nil)
			with, rig := run(holdTestPolicy(5 * time.Second))
			if with != without || with.status < 400 {
				t.Fatalf("with the hold %+v, without %+v: want the same failure", with, without)
			}
			if rig.countLog("proxy hold started") != 0 {
				t.Fatalf("a hold started; logs:\n%s", rig.logs.String())
			}
		})
	}
}
