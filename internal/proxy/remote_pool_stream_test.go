package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/discordwell/subrouter/session"
)

// newPipePeer is a peer reached over in-memory pipes, so the REAL peer client
// -- its transport, its timeouts, the write-stall bound, the stream watchdog --
// runs on a synctest bubble's fake clock: minutes of streaming cost nothing,
// and "cut exactly one idle bound after the last byte" is a fact the test
// checks rather than a race against the wall clock. answer writes the raw
// response; the request has already been read. Every connection is hung up,
// and hungUp closed, before the bubble ends: an answer that paces itself with
// peerStreamPause then returns, so a regression fails on the assertion that
// found it instead of on synctest's deadlock, which would abort the rest of
// the package's tests.
func newPipePeer(t *testing.T, answer func(conn net.Conn, hungUp <-chan struct{})) *RemotePool {
	t.Helper()
	return newPipePeerReading(t, func(conn net.Conn, req *http.Request, hungUp <-chan struct{}) {
		_, _ = io.Copy(io.Discard, req.Body)
		answer(conn, hungUp)
	})
}

// newPipePeerReading is newPipePeer for a peer that reads the request body
// itself -- slowly, or not at all. Only the headers have been read.
func newPipePeerReading(t *testing.T, serve func(conn net.Conn, req *http.Request, hungUp <-chan struct{})) *RemotePool {
	t.Helper()
	pool := NewRemotePool("http://peer.pipe", "", "")
	if pool == nil {
		t.Fatal("NewRemotePool returned nil")
	}
	var mu sync.Mutex
	var conns []net.Conn
	hungUp := make(chan struct{})
	// The production wrapper around a pipe instead of a socket;
	// TestRemotePoolPeerConnectionsBoundAStalledWrite pins that NewRemotePool
	// installs it on the real dial.
	pool.client.Transport.(*http.Transport).DialContext = peerWriteStallDial(func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		mu.Lock()
		conns = append(conns, client, server)
		mu.Unlock()
		go func() {
			defer server.Close()
			req, err := http.ReadRequest(bufio.NewReader(server))
			if err != nil {
				return
			}
			serve(server, req, hungUp)
		}()
		return client, nil
	}, remotePoolWriteStallTimeout)
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		close(hungUp)
		for _, conn := range conns {
			_ = conn.Close()
		}
		pool.client.CloseIdleConnections()
	})
	return pool
}

// peerStreamPause waits d between events, or reports false once the test has
// hung up (a plain Sleep would outlive the bubble's root and deadlock it).
func peerStreamPause(hungUp <-chan struct{}, d time.Duration) bool {
	select {
	case <-hungUp:
		return false
	case <-time.After(d):
		return true
	}
}

// peerStreamHead and peerStreamEvent write a chunked SSE answer by hand, the
// shape a peer relaying a model turn sends.
func peerStreamHead(w io.Writer) {
	_, _ = io.WriteString(w, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
}

func peerStreamEvent(w io.Writer, i int) {
	event := fmt.Sprintf("data: %d\n\n", i)
	_, _ = fmt.Fprintf(w, "%x\r\n%s\r\n", len(event), event)
}

func peerStreamEvents(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "data: %d\n\n", i)
	}
	return b.String()
}

// A turn that streams for 7 minutes, one event every 20 s, runs well past the
// peer client's old 5-minute total and never goes quiet for anything like the
// idle bound. It must arrive whole on both relays. Before §33 the client's
// Timeout also covered the body: the read failed at 5:00, the handler relay
// dropped the error and ended the chunked body cleanly, and the client got the
// first 5 minutes as if that were the answer.
func TestRemotePoolLongStreamOutlivesTheOldTotalCap(t *testing.T) {
	const (
		events = 22
		gap    = 20 * time.Second
	)
	streamed := time.Duration(events-1) * gap // 7 minutes
	if streamed <= 5*time.Minute || gap >= remotePoolStreamIdleTimeout {
		t.Fatalf("calibration: stream %v must outlast the old 5-minute total with gaps (%v) under the idle bound (%v)",
			streamed, gap, remotePoolStreamIdleTimeout)
	}
	answer := func(conn net.Conn, hungUp <-chan struct{}) {
		peerStreamHead(conn)
		for i := 0; i < events; i++ {
			if i > 0 && !peerStreamPause(hungUp, gap) {
				return
			}
			peerStreamEvent(conn, i)
		}
		_, _ = io.WriteString(conn, "0\r\n\r\n")
	}
	want := peerStreamEvents(events)

	t.Run("handler relay", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pool := newPipePeer(t, answer)
			var logs syncBuffer
			server := Server{RemotePool: pool, Logger: slog.New(slog.NewTextHandler(&logs, nil))}
			req := forwardRequest(t, http.MethodPost, "/v1/messages", []byte(`{}`))
			rec := httptest.NewRecorder()

			start := time.Now()
			done := make(chan peerForwardOutcome, 1)
			go func() {
				done <- server.serveOnePeerAttempt(rec, req, pool, "claude", "sess-long", []byte(`{}`), remotePoolHoldSet, "")
			}()
			select {
			case out := <-done:
				if !out.Served {
					t.Fatalf("the peer's 200 was not served: %+v", out)
				}
			case <-time.After(streamed + time.Hour):
				t.Fatal("the relay never finished a stream the peer ended")
			}
			if elapsed := time.Since(start); elapsed != streamed {
				t.Fatalf("relay took %v, want the stream's own %v", elapsed, streamed)
			}
			if got := rec.Body.String(); got != want {
				t.Fatalf("the client got %d of %d events: a long stream was cut (last bytes %q)",
					strings.Count(got, "data: "), events, peerStreamTail(got))
			}
			if strings.Contains(logs.String(), "remote pool stream cut") {
				t.Fatalf("a whole stream logged a cut:\n%s", logs.String())
			}
		})
	})

	t.Run("transport-level stage", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pool := newPipePeer(t, answer)
			server := Server{RemotePool: pool}
			req := forwardRequest(t, http.MethodPost, "/v1/messages", []byte(`{}`))
			resp, ok := server.remotePoolForward(req, "claude", "sess-long", "/v1/messages", "", []byte(`{}`), true)
			if !ok {
				t.Fatal("forward declined")
			}
			defer resp.Body.Close()
			// ReverseProxy reads the stage's answer exactly like this.
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("reading a %v stream failed after %d of %d events: %v",
					streamed, strings.Count(string(got), "data: "), events, err)
			}
			if string(got) != want {
				t.Fatalf("the stage's answer has %d of %d events", strings.Count(string(got), "data: "), events)
			}
		})
	})
}

// A peer that streams past the old cap and then goes quiet with its socket
// open is cut exactly one idle bound after its last byte, and the cut is
// LOGGED -- with the peer, the turn, the bytes and how long the peer had been
// silent. On the handler relay the client gets what arrived; on the
// transport-level stage the reader gets an error naming the peer (ReverseProxy
// logs it and hangs up).
func TestRemotePoolStalledStreamIsCutAtTheIdleBoundAndLogged(t *testing.T) {
	const (
		events = 22
		gap    = 20 * time.Second
	)
	lastByte := time.Duration(events-1) * gap
	answer := func(conn net.Conn, hungUp <-chan struct{}) {
		peerStreamHead(conn)
		for i := 0; i < events; i++ {
			if i > 0 && !peerStreamPause(hungUp, gap) {
				return
			}
			peerStreamEvent(conn, i)
		}
		// Silent, socket open: this returns only when the client hangs up.
		_, _ = io.Copy(io.Discard, conn)
	}
	want := peerStreamEvents(events)

	t.Run("handler relay", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pool := newPipePeer(t, answer)
			var logs syncBuffer
			server := Server{RemotePool: pool, Logger: slog.New(slog.NewTextHandler(&logs, nil))}
			req := forwardRequest(t, http.MethodPost, "/v1/messages", []byte(`{}`))
			rec := httptest.NewRecorder()

			start := time.Now()
			done := make(chan struct{})
			go func() {
				defer close(done)
				server.serveOnePeerAttempt(rec, req, pool, "claude", "sess-stall", []byte(`{}`), remotePoolHoldSet, "acct-a")
			}()
			select {
			case <-done:
			case <-time.After(lastByte + remotePoolStreamIdleTimeout + time.Hour):
				t.Fatal("a peer that went silent mid-stream was never cut: nothing bounds it")
			}
			if elapsed, bound := time.Since(start), lastByte+remotePoolStreamIdleTimeout; elapsed != bound {
				t.Fatalf("cut after %v, want exactly one idle bound after the last byte (%v)", elapsed, bound)
			}
			if got := rec.Body.String(); got != want {
				t.Fatalf("the client got %d of the %d events that arrived before the stall", strings.Count(got, "data: "), events)
			}
			line := peerStreamLogLine(logs.String(), "remote pool stream cut")
			if line == "" {
				t.Fatalf("the cut was silent; logs:\n%s", logs.String())
			}
			for _, field := range []string{
				"level=WARN", "remote=peer.pipe", "session=sess-stall", "target_account=acct-a",
				"cause=idle_timeout", fmt.Sprintf("bytes=%d", len(want)),
				fmt.Sprintf("silent_ms=%d", remotePoolStreamIdleTimeout.Milliseconds()),
			} {
				if !strings.Contains(line, field) {
					t.Fatalf("cut line lacks %q:\n%s", field, line)
				}
			}
		})
	})

	t.Run("transport-level stage", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pool := newPipePeer(t, answer)
			var logs syncBuffer
			server := Server{RemotePool: pool, Logger: slog.New(slog.NewTextHandler(&logs, nil))}
			req := forwardRequest(t, http.MethodPost, "/v1/messages", []byte(`{}`))
			resp, ok := server.remotePoolForward(req, "claude", "sess-stall", "/v1/messages", "", []byte(`{}`), true)
			if !ok {
				t.Fatal("forward declined")
			}
			defer resp.Body.Close()
			type result struct {
				got []byte
				err error
			}
			done := make(chan result, 1)
			go func() {
				got, err := io.ReadAll(resp.Body)
				done <- result{got, err}
			}()
			var res result
			select {
			case res = <-done:
			case <-time.After(lastByte + remotePoolStreamIdleTimeout + time.Hour):
				t.Fatal("a peer that went silent mid-stream was never cut: nothing bounds it")
			}
			if !errors.Is(res.err, errRemotePoolStreamIdle) || !strings.Contains(res.err.Error(), "peer.pipe") {
				t.Fatalf("read error = %v, want the idle cut naming the peer", res.err)
			}
			if string(res.got) != want {
				t.Fatalf("the reader got %d of the %d events that arrived before the stall", strings.Count(string(res.got), "data: "), events)
			}
			if line := peerStreamLogLine(logs.String(), "remote pool stream cut"); !strings.Contains(line, "cause=idle_timeout") {
				t.Fatalf("the stage's cut was not logged as an idle cut; logs:\n%s", logs.String())
			}
		})
	})
}

// The handler relay has already sent a 200 and part of the body when the peer
// breaks. Returning normally lets net/http end the chunked body cleanly, so the
// client reads a truncated answer as a whole one; it must be hung up on
// instead, as ReverseProxy does on the transport-level stage and on every local
// turn. Real servers on both sides, because only net/http's server turns the
// abort into a torn connection.
func TestRemotePoolCutStreamHangsUpOnTheClient(t *testing.T) {
	release := make(chan struct{})
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Header and first event leave in one flush, so the first event is
		// already buffered when the relay starts reading: no race on it.
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	// Close blocks until the handler returns; release must run first (LIFO).
	defer peer.Close()
	defer close(release)

	pool := NewRemotePool(peer.URL, "", "")
	pool.streamIdle = time.Second
	var logs syncBuffer
	server := Server{RemotePool: pool, Logger: slog.New(slog.NewTextHandler(&logs, nil))}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		server.serveOnePeerAttempt(w, r, pool, "claude", "sess-cut", body, remotePoolHoldSet, "")
	}))
	defer front.Close()

	// A ceiling far above the 1 s bound: failing it means nothing cut the
	// stream, which the error below then says instead of hanging the suite.
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if !strings.Contains(string(got), "data: first") {
		t.Fatalf("the client did not get the event that arrived before the cut: %q", got)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("client read error = %v, want a torn body (unexpected EOF): a cut peer stream must not end cleanly", err)
	}
	if line := peerStreamLogLine(logs.String(), "remote pool stream cut"); !strings.Contains(line, "cause=idle_timeout") {
		t.Fatalf("the cut was not logged; logs:\n%s", logs.String())
	}
}

// Only time spent waiting on the PEER counts. A relay that is slow to come back
// for the next chunk -- blocked writing to a slow client -- is not a silent
// peer, and a watchdog that counted from the last byte would cut a healthy
// stream whenever its reader fell behind.
func TestPeerStreamWatchdogCountsOnlyTimeSpentWaitingOnThePeer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const idle = time.Minute
		pr, pw := io.Pipe()
		var cuts []peerStreamCut
		body := newPeerStreamWatchdog(pr, idle, "box", func(cut peerStreamCut) { cuts = append(cuts, cut) })
		go func() {
			_, _ = pw.Write([]byte("a"))
			_, _ = pw.Write([]byte("b"))
			_ = pw.Close()
		}()
		buf := make([]byte, 8)
		if n, err := body.Read(buf); err != nil || string(buf[:n]) != "a" {
			t.Fatalf("first read = %q, %v", buf[:n], err)
		}
		time.Sleep(3 * idle) // the relay is busy elsewhere, not reading
		if n, err := body.Read(buf); err != nil || string(buf[:n]) != "b" {
			t.Fatalf("a read after a slow reader's pause = %q, %v; the watchdog cut a peer that never went quiet", buf[:n], err)
		}
		if _, err := body.Read(buf); err != io.EOF {
			t.Fatalf("end of stream = %v, want io.EOF", err)
		}
		if len(cuts) != 0 {
			t.Fatalf("a whole stream reported cuts: %+v", cuts)
		}
		_ = body.Close()
	})
}

// A peer that stops taking the request mid-body -- a warm socket whose route
// was blackholed, a peer that stopped reading -- must fail the forward one
// write-stall bound after the send stopped, and count a breaker strike. The
// header timeout cannot see it (its clock starts only once the body is
// written) and neither can the stream watchdog (once the headers are in).
// Without a bound of its own the send waits for the client to give up, which
// reads as a client cancel: no strike, the hold kept, and the session's next
// turn sent the same way.
func TestRemotePoolStalledUploadFailsTheForwardAndCountsAStrike(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Reads the request's headers and never its body.
		pool := newPipePeerReading(t, func(_ net.Conn, _ *http.Request, hungUp <-chan struct{}) { <-hungUp })
		var logs syncBuffer
		server := Server{RemotePool: pool, Logger: slog.New(slog.NewTextHandler(&logs, nil))}
		// A turn with a long context; a pipe holds none of it.
		body := []byte(`{"messages":"` + strings.Repeat("x", 1<<20) + `"}`)
		req := forwardRequest(t, http.MethodPost, "/v1/messages", body)

		start := time.Now()
		done := make(chan peerForwardOutcome, 1)
		go func() {
			done <- server.serveOnePeerAttempt(httptest.NewRecorder(), req, pool, "claude", "sess-upload", body, remotePoolHoldSet, "")
		}()
		var out peerForwardOutcome
		select {
		case out = <-done:
		case <-time.After(time.Hour):
			t.Fatal("a send the peer stopped taking was never cut: nothing bounds it until the client gives up")
		}
		if elapsed := time.Since(start); elapsed != remotePoolWriteStallTimeout {
			t.Fatalf("the forward failed after %v, want one write-stall bound (%v) after the send stopped", elapsed, remotePoolWriteStallTimeout)
		}
		if out.Served || !out.Unreachable || out.clientGone() {
			t.Fatalf("outcome = %+v, want an unreachable peer, not a served turn or a client cancel", out)
		}
		pool.breaker.mu.Lock()
		failures := pool.breaker.failures
		pool.breaker.mu.Unlock()
		if failures != 1 {
			t.Fatalf("breaker failures = %d, want 1: a peer that stopped reading is a peer failure", failures)
		}
		line := peerStreamLogLine(logs.String(), "remote pool forward failed")
		if !strings.Contains(line, errRemotePoolWriteStalled.Error()) {
			t.Fatalf("the failed forward does not name the stalled send; logs:\n%s", logs.String())
		}
	})
}

// The write-stall bound is on PROGRESS, not length: a large body that keeps
// moving on a slow link is sent whole however long it takes. Here 1 MiB goes
// 32 KiB every 20 s, over ten minutes -- twice the old 5-minute total, which
// cut it at 5m0s -- and the peer answers it.
func TestRemotePoolSlowUploadThatKeepsMovingIsNotCut(t *testing.T) {
	const (
		size  = 1 << 20
		piece = 32 << 10
		gap   = 20 * time.Second
	)
	if gap >= remotePoolWriteStallTimeout || piece > peerWriteChunk {
		t.Fatalf("calibration: the peer must take a whole write piece (%d) within the bound (%v); it takes %d every %v",
			peerWriteChunk, remotePoolWriteStallTimeout, piece, gap)
	}
	synctest.Test(t, func(t *testing.T) {
		pool := newPipePeerReading(t, func(conn net.Conn, req *http.Request, hungUp <-chan struct{}) {
			buf := make([]byte, piece)
			var got int
			for {
				n, err := io.ReadFull(req.Body, buf)
				got += n
				if err != nil {
					break
				}
				if !peerStreamPause(hungUp, gap) {
					return
				}
			}
			answer := fmt.Sprintf("got %d", got)
			_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(answer), answer)
		})
		var logs syncBuffer
		server := Server{RemotePool: pool, Logger: slog.New(slog.NewTextHandler(&logs, nil))}
		body := make([]byte, size)
		req := forwardRequest(t, http.MethodPost, "/v1/messages", body)
		rec := httptest.NewRecorder()

		start := time.Now()
		done := make(chan peerForwardOutcome, 1)
		go func() {
			done <- server.serveOnePeerAttempt(rec, req, pool, "claude", "sess-slow-upload", body, remotePoolHoldSet, "")
		}()
		select {
		case out := <-done:
			if !out.Served {
				t.Fatalf("a slow upload that kept moving was not served: %+v; logs:\n%s", out, logs.String())
			}
		case <-time.After(time.Hour):
			t.Fatal("the slow upload never finished")
		}
		if elapsed := time.Since(start); elapsed <= 2*remotePoolStreamIdleTimeout {
			t.Fatalf("calibration: the upload took %v, want it to outlast twice the old 5-minute total", elapsed)
		}
		if got, want := rec.Body.String(), fmt.Sprintf("got %d", size); got != want {
			t.Fatalf("peer answered %q, want %q: the body did not arrive whole", got, want)
		}
	})
}

// One Write bigger than a piece gets a fresh deadline per piece, so a writer
// that hands over a whole body at once is bounded on progress too. (net/http
// writes a request in io.Copy's 32 KiB pieces today; the bound must not rest
// on that.) 1 MiB in one Write, taken 32 KiB every 20 s, is not cut at 30 s.
func TestPeerWriteStallConnBoundsEachPieceNotTheWholeWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, server := net.Pipe()
		defer server.Close()
		conn := &peerWriteStallConn{Conn: client, stall: remotePoolWriteStallTimeout}
		defer conn.Close()
		const gap = 20 * time.Second
		stop := make(chan struct{})
		defer close(stop) // before the pipes close: the reader must not outlive the bubble
		go func() {
			buf := make([]byte, peerWriteChunk)
			for {
				if _, err := io.ReadFull(server, buf); err != nil || !peerStreamPause(stop, gap) {
					return
				}
			}
		}()
		n, err := conn.Write(make([]byte, 1<<20))
		if err != nil || n != 1<<20 {
			t.Fatalf("a write that kept moving failed after %d bytes: %v", n, err)
		}
	})
}

// A deadline the caller sets itself still applies when it is sooner than the
// stall bound: crypto/tls bounds its close_notify to 5 s that way, and a piece's
// own 30 s deadline must not stretch it. A caller's deadline that fires is the
// caller's, not a stalled send.
func TestPeerWriteStallConnKeepsTheCallersSoonerDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, server := net.Pipe()
		defer server.Close()
		conn := &peerWriteStallConn{Conn: client, stall: remotePoolWriteStallTimeout}
		defer conn.Close()
		const soon = 5 * time.Second
		if err := conn.SetWriteDeadline(time.Now().Add(soon)); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, err := conn.Write([]byte("nobody reads this")) // nothing reads the pipe
		if elapsed := time.Since(start); elapsed != soon {
			t.Fatalf("write failed after %v, want the caller's own %v", elapsed, soon)
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, errRemotePoolWriteStalled) {
			t.Fatalf("write error = %v, want the caller's deadline, not a stalled send", err)
		}
	})
}

// The pipe peers above install peerWriteStallDial themselves; this pins that
// NewRemotePool puts it on the dial the real transport uses.
func TestRemotePoolPeerConnectionsBoundAStalledWrite(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	pool := NewRemotePool("http://"+listener.Addr().String(), "", "")
	conn, err := pool.client.Transport.(*http.Transport).DialContext(context.Background(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	wrapped, ok := conn.(*peerWriteStallConn)
	if !ok {
		t.Fatalf("the peer transport dials a bare %T: a send the peer stops taking is unbounded", conn)
	}
	if wrapped.stall != remotePoolWriteStallTimeout {
		t.Fatalf("write-stall bound = %v, want %v", wrapped.stall, remotePoolWriteStallTimeout)
	}
}

// A forwarded answer that streams past the hold's TTL keeps the hold: the hold
// was stamped when the turn was sent, the touch on Close cannot revive an
// expired entry, and once the 5-minute cap was gone (§33) nothing stopped an
// answer from outliving it. Mid-stream, the hold must still be there.
func TestRemotePoolHoldOutlivesAnAnswerLongerThanItsTTL(t *testing.T) {
	ttl := remotePoolDelegationTTLFor("claude")
	const gap = time.Minute
	events := int((ttl+5*time.Minute)/gap) + 1
	synctest.Test(t, func(t *testing.T) {
		pool := newPipePeer(t, func(conn net.Conn, hungUp <-chan struct{}) {
			peerStreamHead(conn)
			for i := 0; i < events; i++ {
				if i > 0 && !peerStreamPause(hungUp, gap) {
					return
				}
				peerStreamEvent(conn, i)
			}
			_, _ = io.WriteString(conn, "0\r\n\r\n")
		})
		server := Server{RemotePool: pool}
		req := forwardRequest(t, http.MethodPost, "/v1/messages", []byte(`{}`))
		key := session.ScopedSessionKey("claude", "sess-marathon")

		done := make(chan peerForwardOutcome, 1)
		go func() {
			done <- server.serveOnePeerAttempt(httptest.NewRecorder(), req, pool, "claude", "sess-marathon", []byte(`{}`), remotePoolHoldSet, "")
		}()
		time.Sleep(ttl + 2*time.Minute)
		if !pool.delegations.active(key, time.Now()) {
			t.Fatalf("the hold lapsed %v into an answer still streaming (TTL %v): the next turn re-ranks as cold", ttl+2*time.Minute, ttl)
		}
		select {
		case out := <-done:
			if !out.Served {
				t.Fatalf("the long answer was not served: %+v", out)
			}
		case <-time.After(time.Hour):
			t.Fatal("the long answer never finished")
		}
		if !pool.delegations.active(key, time.Now()) {
			t.Fatal("the hold is gone after the answer ended")
		}
	})
}

// Guard: the peer client lost its total timeout, so each control read must
// bring its own bound, BODY included. A peer that answers the admin and usage
// reads with headers and then stalls mid-body must still cost at most
// remotePoolAdminTimeout, or a pool-board load and every routing decision
// waiting on the usage probe hang with it.
func TestRemotePoolControlReadsStayBoundedWithoutAClientTimeout(t *testing.T) {
	stalledBody := func(conn net.Conn, _ <-chan struct{}) {
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n[")
		_, _ = io.Copy(io.Discard, conn)
	}

	synctest.Test(t, func(t *testing.T) {
		pool := newPipePeer(t, stalledBody)
		start := time.Now()
		done := make(chan AdminSnapshot, 1)
		go func() { done <- pool.AdminGet(context.Background(), remotePoolUsagePath) }()
		select {
		case snapshot := <-done:
			if snapshot.Reachable {
				t.Fatalf("a stalled body read as reachable: %+v", snapshot)
			}
		case <-time.After(time.Hour):
			t.Fatal("an admin read of a stalled body never returned")
		}
		if elapsed := time.Since(start); elapsed != remotePoolAdminTimeout {
			t.Fatalf("admin read took %v, want its own %v bound", elapsed, remotePoolAdminTimeout)
		}
	})

	synctest.Test(t, func(t *testing.T) {
		pool := newPipePeer(t, stalledBody)
		start := time.Now()
		done := make(chan bool, 1)
		go func() {
			_, _, measured := pool.probeCapacity()
			done <- measured
		}()
		select {
		case measured := <-done:
			if measured {
				t.Fatal("a stalled body was measured")
			}
		case <-time.After(time.Hour):
			t.Fatal("the capacity probe of a stalled body never returned")
		}
		if elapsed := time.Since(start); elapsed != remotePoolAdminTimeout {
			t.Fatalf("capacity probe took %v, want its own %v bound", elapsed, remotePoolAdminTimeout)
		}
	})
}

// peerStreamLogLine returns the first log line containing msg, or "".
func peerStreamLogLine(logs, msg string) string {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, msg) {
			return line
		}
	}
	return ""
}

func peerStreamTail(s string) string {
	if len(s) > 40 {
		return s[len(s)-40:]
	}
	return s
}
