package proxy

// The owner's rule (2026-09-28): a session moved off a spent account onto one
// that answers successfully sticks to that account, and a replacement that
// fails does not move it.
//
// A move off an existing pin is provisional (accountForSessionProviderWithOptions)
// and a streamed reply commits it only when its terminal event says success
// (newSessionCommitSSEReadCloser). The proxy used to forward Claude Code's
// Accept-Encoding, so the provider compressed the stream, Go's transport left
// it compressed, and the parser never found message_stop. The move never
// committed and every later turn re-picked from the spent pin: on the box,
// 28.6k "rerouting cold sticky session" lines a day and not one move.
//
// The upstream here negotiates the way api.anthropic.com does: it compresses
// whenever the request offers an encoding it has. Which one it picked on the
// live 200 streams is not captured: the logged 429 bodies were Brotli, while the
// client's ZlibErrors point at gzip or deflate for the streams. zstd stands in
// here because the module has no brotli encoder; the proxy decoded none of them.

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// What Claude Code 2.1.283 (Bun) sends on every request.
const stickyCommitClaudeCodeAcceptEncoding = "gzip, deflate, br, zstd"

const stickyCommitStreamStart = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"usage":{"input_tokens":12,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}` + "\n\n"

// A turn that completed.
const stickyCommitStreamSuccess = stickyCommitStreamStart +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// A 200 whose stream then fails: the replacement did not serve the turn.
const stickyCommitStreamError = stickyCommitStreamStart +
	"event: error\n" +
	`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}` + "\n\n"

// stickyCommitWriteStream answers like api.anthropic.com: compressed with the
// best encoding the request offers, flushed in small pieces.
func stickyCommitWriteStream(t *testing.T, w http.ResponseWriter, r *http.Request, stream string) {
	t.Helper()
	payload := []byte(stream)
	offered := r.Header.Get("Accept-Encoding")
	switch {
	case strings.Contains(offered, "zstd"):
		encoder, err := zstd.NewWriter(nil)
		if err != nil {
			t.Error(err)
			return
		}
		payload = encoder.EncodeAll(payload, nil)
		_ = encoder.Close()
		w.Header().Set("Content-Encoding", "zstd")
	case strings.Contains(offered, "gzip"):
		var buf bytes.Buffer
		encoder := gzip.NewWriter(&buf)
		_, _ = encoder.Write(payload)
		_ = encoder.Close()
		payload = buf.Bytes()
		w.Header().Set("Content-Encoding", "gzip")
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	for start := 0; start < len(payload); start += 64 {
		end := min(start+64, len(payload))
		_, _ = w.Write(payload[start:end])
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}

// stickyCommitDecode returns the stream the client would render, whatever
// encoding the proxy relayed it in.
func stickyCommitDecode(t *testing.T, response *http.Response, body []byte) string {
	t.Helper()
	switch response.Header.Get("Content-Encoding") {
	case "":
		return string(body)
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		return string(decoded)
	case "zstd":
		decoder, err := zstd.NewReader(nil)
		if err != nil {
			t.Fatal(err)
		}
		defer decoder.Close()
		decoded, err := decoder.DecodeAll(body, nil)
		if err != nil {
			t.Fatal(err)
		}
		return string(decoded)
	default:
		t.Fatalf("unexpected Content-Encoding %q", response.Header.Get("Content-Encoding"))
		return ""
	}
}

type stickyCommitUpstream struct {
	mu      sync.Mutex
	auths   []string
	offered []string
}

func (u *stickyCommitUpstream) note(r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.auths = append(u.auths, r.Header.Get("Authorization"))
	u.offered = append(u.offered, r.Header.Get("Accept-Encoding"))
}

func (u *stickyCommitUpstream) seen() (string, []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return strings.Join(u.auths, ","), append([]string(nil), u.offered...)
}

func stickyCommitClaudeServer(upstream string, t *testing.T, store *session.Store, logs *syncBuffer, spent, fresh selectacct.Score) Server {
	return Server{
		ClaudeUpstream: mustParseURL(t, upstream),
		Accounts: []accounts.Account{
			{ID: "spent", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "spent-token"},
			{ID: "fresh", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "fresh-token"},
		},
		Sessions:     store,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{spent, fresh})),
		MaxBodyBytes: 1 << 20,
		Logger:       slog.New(slog.NewTextHandler(logs, nil)),
	}
}

// The live state on the box: the pin's weekly window is spent, so selection
// evicts the session and reroutes it.
func stickyCommitScores() (spent, fresh selectacct.Score) {
	roll := time.Now().Add(36 * time.Hour)
	spent = selectacct.Score{AccountID: "spent", Provider: accounts.ProviderClaude, Fresh: true,
		Headroom: 0, ShortHeadroom: 1, WeeklyHeadroom: 0, WeeklyResetAt: roll}
	fresh = selectacct.Score{AccountID: "fresh", Provider: accounts.ProviderClaude, Fresh: true,
		Headroom: 0.9, ShortHeadroom: 0.9, WeeklyHeadroom: 0.9, WeeklyRemaining: 0.9, WeeklyResetAt: roll}
	return spent, fresh
}

func stickyCommitTurn(t *testing.T, target, sessionID string) (*http.Response, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, target+"/v1/messages?beta=true",
		strings.NewReader(`{"model":"claude-opus-5-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Claude-Code-Session-Id", sessionID)
	request.Header.Set("User-Agent", "claude-cli/2.1.283 (external, cli)")
	// Set explicitly, as Bun does. It also stops Go's client from decoding, so
	// the test sees exactly the bytes the proxy relayed.
	request.Header.Set("Accept-Encoding", stickyCommitClaudeCodeAcceptEncoding)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", response.StatusCode, body)
	}
	return response, stickyCommitDecode(t, response, body)
}

func stickyCommitPin(t *testing.T, store *session.Store, sessionID string) string {
	t.Helper()
	assignment, ok := store.Get("claude", sessionID)
	if !ok {
		t.Fatal("sticky assignment disappeared")
	}
	return assignment.AccountID
}

func TestEvictedClaudeSessionSticksToTheAccountThatServedItsStream(t *testing.T) {
	var upstreamCalls stickyCommitUpstream
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		upstreamCalls.note(r)
		stickyCommitWriteStream(t, w, r, stickyCommitStreamSuccess)
	}))
	defer upstream.Close()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "4b06af0d-evicted"
	if _, err := store.Put("claude", sessionID, "spent", ""); err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	spent, fresh := stickyCommitScores()
	subrouter := httptest.NewServer(stickyCommitClaudeServer(upstream.URL, t, store, &logs, spent, fresh).Handler())
	defer subrouter.Close()

	_, stream := stickyCommitTurn(t, subrouter.URL, sessionID)
	if !strings.Contains(stream, "message_stop") {
		t.Fatalf("client did not receive a completed turn: %q", stream)
	}
	if !strings.Contains(logs.String(), "rerouting cold sticky session from constrained account") ||
		!strings.Contains(logs.String(), "to_account=fresh") {
		t.Fatalf("turn 1 did not take the eviction reroute; logs:\n%s", logs.String())
	}
	// The move commits while the proxy reads the terminal event, before it
	// relays it, so the client having read message_stop is the barrier.
	if pin := stickyCommitPin(t, store, sessionID); pin != "fresh" {
		t.Fatalf("pin = %q after fresh served the whole turn, want fresh (the move never committed)", pin)
	}
	if !strings.Contains(logs.String(), "session moved to another account") ||
		!strings.Contains(logs.String(), "from_account=spent to_account=fresh") {
		t.Fatalf("the committed move was not logged; logs:\n%s", logs.String())
	}

	// Turn 2 sticks: no second pick, the same account, the same pin.
	_, _ = stickyCommitTurn(t, subrouter.URL, sessionID)
	if got := strings.Count(logs.String(), "rerouting cold sticky session"); got != 1 {
		t.Fatalf("turn 2 re-picked: %d reroutes, want 1; logs:\n%s", got, logs.String())
	}
	auths, offered := upstreamCalls.seen()
	if auths != "Bearer fresh-token,Bearer fresh-token" {
		t.Fatalf("upstream auths = %q, want both turns on fresh", auths)
	}
	if pin := stickyCommitPin(t, store, sessionID); pin != "fresh" {
		t.Fatalf("pin = %q after turn 2, want fresh", pin)
	}
	// The mechanism: the client's offer never reaches the provider, so Go's
	// transport negotiates gzip itself and decodes what every inspector reads.
	for _, value := range offered {
		if value != "gzip" {
			t.Fatalf("upstream was offered Accept-Encoding %q, want Go's own gzip", value)
		}
	}
}

func TestEvictedClaudeSessionStaysWhenItsReplacementFails(t *testing.T) {
	for _, test := range []struct {
		name, stream, reason string
	}{
		{name: "error event", stream: stickyCommitStreamError, reason: `reason="stream reported failure"`},
		{name: "truncated", stream: stickyCommitStreamStart, reason: `reason="stream ended without a terminal event"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				stickyCommitWriteStream(t, w, r, test.stream)
			}))
			defer upstream.Close()
			store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
			if err != nil {
				t.Fatal(err)
			}
			const sessionID = "9930db38-replacement-failed"
			if _, err := store.Put("claude", sessionID, "spent", ""); err != nil {
				t.Fatal(err)
			}
			var logs syncBuffer
			spent, fresh := stickyCommitScores()
			subrouter := httptest.NewServer(stickyCommitClaudeServer(upstream.URL, t, store, &logs, spent, fresh).Handler())
			defer subrouter.Close()

			_, _ = stickyCommitTurn(t, subrouter.URL, sessionID)
			if !strings.Contains(logs.String(), "to_account=fresh") {
				t.Fatalf("the reroute path was not taken; logs:\n%s", logs.String())
			}
			if pin := stickyCommitPin(t, store, sessionID); pin != "spent" {
				t.Fatalf("pin = %q after fresh failed the turn, want spent", pin)
			}
			// Not silent: the lost move says why.
			if !strings.Contains(logs.String(), "provisional session move not committed") ||
				!strings.Contains(logs.String(), test.reason) {
				t.Fatalf("the uncommitted move was not logged with %s; logs:\n%s", test.reason, logs.String())
			}
		})
	}
}

// The other commit site: the pin still looks healthy, answers 429 rejected, and
// the usage-limit retry serves the turn on fresh (the box's "retrying
// replayable upstream request after usage limit ... attempt=2").
func TestUsageLimitFailoverSticksToTheAccountThatServedItsStream(t *testing.T) {
	var upstreamCalls stickyCommitUpstream
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		upstreamCalls.note(r)
		if r.Header.Get("Authorization") == "Bearer spent-token" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
			w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."}}`)
			return
		}
		stickyCommitWriteStream(t, w, r, stickyCommitStreamSuccess)
	}))
	defer upstream.Close()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "1ccb2937-failover"
	if _, err := store.Put("claude", sessionID, "spent", ""); err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	roll := time.Now().Add(36 * time.Hour)
	// Both look healthy, as in a process with no usage scores yet: only the
	// upstream's 429 says otherwise.
	spent := selectacct.Score{AccountID: "spent", Provider: accounts.ProviderClaude, Fresh: true,
		Headroom: 0.9, ShortHeadroom: 0.9, WeeklyHeadroom: 0.9, WeeklyRemaining: 0.9, WeeklyResetAt: roll}
	fresh := selectacct.Score{AccountID: "fresh", Provider: accounts.ProviderClaude, Fresh: true,
		Headroom: 0.8, ShortHeadroom: 0.8, WeeklyHeadroom: 0.8, WeeklyRemaining: 0.8, WeeklyResetAt: roll}
	subrouter := httptest.NewServer(stickyCommitClaudeServer(upstream.URL, t, store, &logs, spent, fresh).Handler())
	defer subrouter.Close()

	_, stream := stickyCommitTurn(t, subrouter.URL, sessionID)
	if !strings.Contains(stream, "message_stop") {
		t.Fatalf("client did not receive a completed turn: %q", stream)
	}
	if !strings.Contains(logs.String(), "retrying replayable upstream request after usage limit") {
		t.Fatalf("the usage-limit failover was not taken; logs:\n%s", logs.String())
	}
	if pin := stickyCommitPin(t, store, sessionID); pin != "fresh" {
		t.Fatalf("pin = %q after the failover served the whole turn on fresh, want fresh", pin)
	}

	_, _ = stickyCommitTurn(t, subrouter.URL, sessionID)
	auths, _ := upstreamCalls.seen()
	if auths != "Bearer spent-token,Bearer fresh-token,Bearer fresh-token" {
		t.Fatalf("upstream auths = %q, want turn 2 straight on fresh", auths)
	}
	if strings.Contains(logs.String(), "rerouting cold sticky session") {
		t.Fatalf("turn 2 re-picked instead of sticking to fresh; logs:\n%s", logs.String())
	}
}

// The live topology: Claude Code -> laptop daemon -> peer forward
// (serveRemotePool, which copies the client's Accept-Encoding) -> the box's
// token-gated PublicHandler over TLS -> the provider. The box's pin is the one
// that must move.
func TestEvictedClaudeSessionSticksOnThePeerAfterAForwardedStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		stickyCommitWriteStream(t, w, r, stickyCommitStreamSuccess)
	}))
	defer upstream.Close()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "ea3a8d45-peer-hop"
	if _, err := store.Put("claude", sessionID, "spent", ""); err != nil {
		t.Fatal(err)
	}
	var boxLogs syncBuffer
	spent, fresh := stickyCommitScores()
	box := stickyCommitClaudeServer(upstream.URL, t, store, &boxLogs, spent, fresh)
	box.LocalProxyToken = "peer-token"
	boxListener := httptest.NewTLSServer(box.PublicHandler())
	defer boxListener.Close()

	pool := NewRemotePool(boxListener.URL, "peer-token", "")
	if pool == nil {
		t.Fatal("NewRemotePool returned nil")
	}
	pool.client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	var laptopLogs syncBuffer
	laptop := Server{RemotePool: pool, Logger: slog.New(slog.NewTextHandler(&laptopLogs, nil))}

	request := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true",
		strings.NewReader(`{"model":"claude-opus-5-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.RemoteAddr = "127.0.0.1:5555"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Claude-Code-Session-Id", sessionID)
	request.Header.Set("User-Agent", "claude-cli/2.1.283 (external, cli)")
	request.Header.Set("Accept-Encoding", stickyCommitClaudeCodeAcceptEncoding)
	recorder := httptest.NewRecorder()
	if !laptop.serveRemotePool(recorder, request, "claude", sessionID, remotePoolHoldSet) {
		t.Fatalf("the laptop did not forward to the peer; laptop logs:\n%s\nbox logs:\n%s", laptopLogs.String(), boxLogs.String())
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q; box logs:\n%s", recorder.Code, recorder.Body.String(), boxLogs.String())
	}
	if !strings.Contains(boxLogs.String(), "rerouting cold sticky session from constrained account") ||
		!strings.Contains(boxLogs.String(), "to_account=fresh") {
		t.Fatalf("the box did not take the eviction reroute; box logs:\n%s", boxLogs.String())
	}
	if pin := stickyCommitPin(t, store, sessionID); pin != "fresh" {
		t.Fatalf("box pin = %q after fresh served a forwarded turn, want fresh", pin)
	}
}

// A commit that loses its compare-and-swap is correct (another writer moved the
// session first) but used to leave no trace at all.
func TestProvisionalSessionMoveThatLostItsPinIsLogged(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "0924bef3-lost-cas"
	if _, err := store.Put("claude", sessionID, "moved-elsewhere", ""); err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	server := Server{Sessions: store, Logger: slog.New(slog.NewTextHandler(&logs, nil))}
	swapped, err := server.commitSessionReassignment("claude", sessionID, "spent", "fresh", "")
	if err != nil {
		t.Fatal(err)
	}
	if swapped {
		t.Fatal("swapped = true against a pin that no longer names the expected account")
	}
	if pin := stickyCommitPin(t, store, sessionID); pin != "moved-elsewhere" {
		t.Fatalf("pin = %q, want the newer assignment kept", pin)
	}
	if !strings.Contains(logs.String(), "provisional session move not committed") ||
		!strings.Contains(logs.String(), `reason="pin changed"`) ||
		!strings.Contains(logs.String(), "pinned_account=moved-elsewhere") {
		t.Fatalf("the lost compare-and-swap was not logged; logs:\n%s", logs.String())
	}
}

// Each way a provisional move can end without committing is named, once; a
// commit is silent.
func TestSessionCommitWrappersReportAMissedCommitOnce(t *testing.T) {
	sse, eof := newSessionCommitSSEReadCloser, newSessionCommitEOFReadCloser
	for _, test := range []struct {
		name        string
		wrap        func(io.ReadCloser, func() error, func(string)) io.ReadCloser
		body        string
		drain       bool
		wantCommits int
		wantMissed  string
	}{
		{name: "stream completes", wrap: sse, body: stickyCommitStreamSuccess, drain: true, wantCommits: 1},
		{name: "stream reports failure", wrap: sse, body: stickyCommitStreamError, drain: true, wantMissed: "stream reported failure"},
		{name: "stream truncated", wrap: sse, body: stickyCommitStreamStart, drain: true, wantMissed: "stream ended without a terminal event"},
		{name: "stream abandoned", wrap: sse, body: stickyCommitStreamSuccess, wantMissed: "stream closed before its terminal event"},
		{name: "body completes", wrap: eof, body: `{"type":"message"}`, drain: true, wantCommits: 1},
		{name: "body abandoned", wrap: eof, body: `{"type":"message"}`, wantMissed: "body closed before EOF"},
	} {
		t.Run(test.name, func(t *testing.T) {
			commits := 0
			var missed []string
			body := test.wrap(io.NopCloser(strings.NewReader(test.body)),
				func() error { commits++; return nil },
				func(reason string) { missed = append(missed, reason) })
			if test.drain {
				if _, err := io.ReadAll(body); err != nil {
					t.Fatal(err)
				}
			} else if _, err := body.Read(make([]byte, 8)); err != nil {
				t.Fatal(err)
			}
			if err := body.Close(); err != nil {
				t.Fatal(err)
			}
			if commits != test.wantCommits {
				t.Fatalf("commits = %d, want %d", commits, test.wantCommits)
			}
			want := []string(nil)
			if test.wantMissed != "" {
				want = []string{test.wantMissed}
			}
			if strings.Join(missed, "|") != strings.Join(want, "|") {
				t.Fatalf("missed = %q, want %q", missed, want)
			}
		})
	}
}
