package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

// A forwarded answer streams for as long as the peer keeps sending (PATCHES
// §33). The peer client has no total timeout -- it used to, and it cut every
// forwarded turn at 5 minutes -- so what bounds a peer that sent its headers
// and then went quiet is this watchdog: it cuts the answer once a read has
// waited remotePoolStreamIdleTimeout for a byte. And every cut seen while
// reading is logged, by whichever relay was reading: the handler's
// flushingCopy and the transport-level stage's ReverseProxy both read through
// it. (A client that hangs up while the relay is writing to it ends the copy
// on the write side, and the body is closed without another read: no line.)
//
// The send before it is bounded the same way, by progress: peerWriteStallConn
// fails a write that stops moving, which neither the header timeout (its clock
// starts once the body is written) nor the watchdog (once the headers are in)
// covers.

// errRemotePoolStreamIdle marks a peer answer the watchdog cut because it
// delivered nothing for the pool's stream idle bound.
var errRemotePoolStreamIdle = errors.New("remote pool stream idle past the watchdog deadline")

// errRemotePoolWriteStalled marks a send to the peer that stopped moving: one
// piece of a write did not get into the socket within
// remotePoolWriteStallTimeout.
var errRemotePoolWriteStalled = errors.New("remote pool peer stopped taking the request")

// peerWriteChunk is the most one write deadline covers (peerWriteStallConn).
// It passes a TLS record (16 KiB plus overhead) whole.
const peerWriteChunk = 32 << 10

// peerWriteStallDial wraps dial so every connection to the peer fails a write
// that stops moving (peerWriteStallConn).
func peerWriteStallDial(dial func(context.Context, string, string) (net.Conn, error), stall time.Duration) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &peerWriteStallConn{Conn: conn, stall: stall}, nil
	}
}

// peerWriteStallConn fails a write that cannot get one piece of at most
// peerWriteChunk bytes into the socket within stall -- a send that has
// stopped -- and names it errRemotePoolWriteStalled.
//
// The deadline is set afresh before each piece, so it bounds a send that
// STOPPED, never one that is merely long: anything faster than a piece per
// stall (about 1 KiB/s) is never cut. net/http writes a request in io.Copy's
// 32 KiB pieces today; the cap keeps that true for any writer, since one
// deadline on a whole body would cut a large one on a slow link. A deadline
// the caller sets itself (crypto/tls bounds its close_notify with one) still
// applies when it is sooner. Reads are untouched: the answer has the header
// timeout, then the stream watchdog.
type peerWriteStallConn struct {
	net.Conn
	stall time.Duration

	// mu orders the caller's deadline against the one Write sets, so a
	// caller's sooner deadline is never overwritten by a piece's.
	mu     sync.Mutex
	caller time.Time
}

func (c *peerWriteStallConn) Write(p []byte) (n int, err error) {
	for len(p) > 0 {
		piece := p[:min(len(p), peerWriteChunk)]
		deadline := time.Now().Add(c.stall)
		c.mu.Lock()
		ours := c.caller.IsZero() || deadline.Before(c.caller)
		if !ours {
			deadline = c.caller
		}
		err = c.Conn.SetWriteDeadline(deadline)
		c.mu.Unlock()
		if err != nil {
			return n, err
		}
		var m int
		m, err = c.Conn.Write(piece)
		n += m
		if err != nil {
			if ours && errors.Is(err, os.ErrDeadlineExceeded) {
				err = fmt.Errorf("%w for %s: %w", errRemotePoolWriteStalled, c.stall, err)
			}
			return n, err
		}
		p = p[m:]
	}
	return n, nil
}

func (c *peerWriteStallConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.caller = t
	return c.Conn.SetDeadline(t)
}

func (c *peerWriteStallConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.caller = t
	return c.Conn.SetWriteDeadline(t)
}

// streamIdleTimeout is the pool's silence bound on a forwarded answer.
func (p *RemotePool) streamIdleTimeout() time.Duration {
	if p == nil || p.streamIdle <= 0 {
		return remotePoolStreamIdleTimeout
	}
	return p.streamIdle
}

// peerStreamCut is one forwarded answer that ended before its end.
type peerStreamCut struct {
	Err error
	// Idle: the watchdog cut it (Err wraps errRemotePoolStreamIdle).
	Idle bool
	// Bytes read before the cut; Elapsed since the headers arrived; Silent
	// since the last byte (or the headers, when none came).
	Bytes   int64
	Elapsed time.Duration
	Silent  time.Duration
}

// peerStreamWatchdog closes a peer's answer body when a Read has waited the
// idle bound without a byte, which errors the blocked Read, and reports the
// first read error that is not the normal end.
//
// Only time spent INSIDE Read counts. A relay blocked writing to a slow client
// is not reading, and a slow client is not a silent peer: counting from the
// last byte instead would cut a healthy stream whenever its reader fell
// behind. The fire callback re-checks under the mutex, so a timer that fires
// as its Read returns (or after the next Read began) cuts nothing early.
type peerStreamWatchdog struct {
	body  io.ReadCloser
	idle  time.Duration
	peer  string
	onCut func(peerStreamCut)

	mu        sync.Mutex
	timer     *time.Timer
	started   time.Time
	lastByte  time.Time
	readStart time.Time
	bytes     int64
	inRead    bool
	fired     bool
	closed    bool
	reported  bool
}

func newPeerStreamWatchdog(body io.ReadCloser, idle time.Duration, peer string, onCut func(peerStreamCut)) *peerStreamWatchdog {
	now := time.Now()
	return &peerStreamWatchdog{body: body, idle: idle, peer: peer, onCut: onCut, started: now, lastByte: now}
}

func (b *peerStreamWatchdog) Read(p []byte) (n int, err error) {
	b.mu.Lock()
	// Once fired, the body is closed: every later Read is the cut, including
	// the one after a Read the timer raced as it returned with bytes.
	armed := !b.fired
	if armed {
		b.inRead, b.readStart = true, time.Now()
		if b.timer == nil {
			b.timer = time.AfterFunc(b.idle, b.fire)
		} else {
			b.timer.Reset(b.idle)
		}
	}
	b.mu.Unlock()

	if armed {
		n, err = b.body.Read(p)
	}

	b.mu.Lock()
	b.inRead = false
	if b.timer != nil {
		b.timer.Stop()
	}
	now := time.Now()
	silent := now.Sub(b.lastByte)
	if n > 0 {
		b.bytes += int64(n)
		b.lastByte = now
	}
	if b.fired && (err != nil || !armed) {
		err = b.idleErr()
	}
	var cut *peerStreamCut
	// A read after the reader's own Close is the reader's doing, not a cut.
	if err != nil && err != io.EOF && !b.reported && !b.closed {
		b.reported = true
		cut = &peerStreamCut{Err: err, Idle: b.fired, Bytes: b.bytes, Elapsed: now.Sub(b.started), Silent: silent}
	}
	b.mu.Unlock()
	if cut != nil && b.onCut != nil {
		b.onCut(*cut)
	}
	return n, err
}

func (b *peerStreamWatchdog) fire() {
	b.mu.Lock()
	if b.closed || b.fired || !b.inRead {
		b.mu.Unlock()
		return
	}
	if waited := time.Since(b.readStart); waited < b.idle {
		b.timer.Reset(b.idle - waited)
		b.mu.Unlock()
		return
	}
	b.fired = true
	b.mu.Unlock()
	_ = b.body.Close()
}

func (b *peerStreamWatchdog) Close() error {
	b.mu.Lock()
	b.closed = true
	if b.timer != nil {
		b.timer.Stop()
	}
	b.mu.Unlock()
	return b.body.Close()
}

// idleErr names the peer and the bound, so the transport-level stage's
// ReverseProxy line -- which names the local account it gave up on, not the
// peer -- still says which peer went quiet.
func (b *peerStreamWatchdog) idleErr() error {
	return fmt.Errorf("%w: peer %s sent nothing for %s", errRemotePoolStreamIdle, b.peer, b.idle)
}

// watchPeerAnswer puts resp's body behind the stream watchdog and logs the
// answer's cut, if it is cut, with the turn it belonged to.
//
// cause is a fixed word: idle_timeout (the watchdog), client_gone (the
// client's request ended first: an Esc, or its own stall detector -- silent_ms
// says how long the peer had been quiet by then) or peer_error (a reset, a
// truncated chunk, anything else the peer's side broke with). Info for
// client_gone, which is not the peer failing; Warn otherwise.
func (s Server) watchPeerAnswer(pool *RemotePool, r *http.Request, resp *http.Response, agentType, sessionID, clientPath, target string) {
	status := resp.StatusCode
	served := resp.Header.Get(subrouterAccountHeader)
	resp.Body = newPeerStreamWatchdog(resp.Body, pool.streamIdleTimeout(), pool.name(), func(cut peerStreamCut) {
		if s.Logger == nil {
			return
		}
		cause, level := "peer_error", slog.LevelWarn
		switch {
		case cut.Idle:
			cause = "idle_timeout"
		case r.Context().Err() != nil:
			cause, level = "client_gone", slog.LevelInfo
		}
		s.Logger.Log(r.Context(), level, "remote pool stream cut",
			"remote", pool.name(), "agent", agentType, "session", sessionID,
			"path", clientPath, "status", status, "target_account", target, "served_account", served,
			"cause", cause, "bytes", cut.Bytes, "elapsed_ms", cut.Elapsed.Milliseconds(),
			"silent_ms", cut.Silent.Milliseconds(), "error", cut.Err)
	})
}

// abortPeerRelay hangs up on the client after the peer's answer broke mid-body,
// as ReverseProxy does for the transport-level stage and every local turn.
// Returning instead lets net/http end the chunked body cleanly, and the client
// takes a truncated answer for a whole one. Only under net/http's server --
// the same test ReverseProxy applies -- because nothing else recovers
// ErrAbortHandler.
func abortPeerRelay(r *http.Request) {
	if r.Context().Value(http.ServerContextKey) != nil {
		panic(http.ErrAbortHandler)
	}
}
