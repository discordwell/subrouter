package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// The Claude socket's guard (claude_socket_guard.go). Each test drives the
// whole private handler through a real listener whose connections are marked
// as serve marks the Claude socket's, sending what Claude Code 2.1.328 sends in
// socket mode. The calibration for each refusal is the same request on an
// unmarked listener in front of a fresh daemon: it must get the 401 (or the
// revoked 403) the guard keeps from the socket.

const (
	socketGuardAccount    = "solo@example.com"
	socketGuardProxyToken = "socket-guard-proxy-token"
	socketGuardOAuthToken = "sk-ant-oat01-SOCKETGUARDSECRET"
)

// socketGuardRig is the private listener twice over one handler: socket,
// whose connections are marked, and tcp, whose are not.
type socketGuardRig struct {
	server       Server
	socket, tcp  *httptest.Server
	guard        *ClaudeSocketGuard
	logs         *syncBuffer
	upstreamHits atomic.Int64
	peerHits     atomic.Int64
}

type socketGuardOptions struct {
	// upstream stands in for Anthropic; nil answers 200 {}.
	upstream http.HandlerFunc
	// peer, when set, is a peer daemon (SUBROUTER_REMOTE_POOL). Its usage read
	// is answered empty, so it is served untargeted.
	peer http.HandlerFunc
	// noAccounts leaves the daemon without a Claude account of its own.
	noAccounts bool
	// refuseLoopback is SUBROUTER_ALLOW_LOOPBACK_PROXY off: the placeholder
	// cannot pass the proxy token.
	refuseLoopback bool
	hold           *HoldPolicy
}

func newSocketGuardRig(t *testing.T, opts socketGuardOptions) *socketGuardRig {
	t.Helper()
	rig := &socketGuardRig{logs: &syncBuffer{}}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rig.upstreamHits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		if opts.upstream != nil {
			opts.upstream(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	var pool *RemotePool
	if opts.peer != nil {
		peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == remotePoolUsagePath {
				return
			}
			rig.peerHits.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			opts.peer(w, r)
		}))
		t.Cleanup(peer.Close)
		pool = NewRemotePool(peer.URL, "", "")
	}
	var pooled []accounts.Account
	var scores []selectacct.Score
	if !opts.noAccounts {
		pooled = []accounts.Account{{ID: socketGuardAccount, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-solo"}}
		scores = []selectacct.Score{{AccountID: socketGuardAccount, Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1, Fresh: true}}
	}
	dir := t.TempDir()
	store, err := session.NewStore(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(rig.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rig.guard = NewClaudeSocketGuard(logger)
	rig.server = Server{
		ClaudeUpstream:                 upstreamURL,
		Accounts:                       pooled,
		Sessions:                       store,
		SchedulerRef:                   selectacct.NewSchedulerRef(selectacct.NewScheduler(scores)),
		MaxBodyBytes:                   1 << 20,
		Logger:                         logger,
		LocalProxyToken:                socketGuardProxyToken,
		AllowLoopbackProxyWithoutToken: !opts.refuseLoopback,
		IdentityAccountID:              socketGuardAccount,
		IdentityPinRequired:            true,
		Hold:                           opts.hold,
		RemotePool:                     pool,
		ClaudeSocketGuard:              rig.guard,
		claudeCosts:                    newClaudeCostStore(filepath.Join(dir, "claude-cost-state.json")),
	}
	handler := rig.guard.Handler(rig.server.Handler())
	rig.socket = httptest.NewUnstartedServer(handler)
	rig.socket.Config.ConnContext = func(ctx context.Context, conn net.Conn) context.Context {
		return ClaudeSocketConnContext(LocalDataConnContext(ctx, conn))
	}
	rig.socket.Start()
	t.Cleanup(rig.socket.Close)
	rig.tcp = httptest.NewUnstartedServer(handler)
	rig.tcp.Config.ConnContext = LocalDataConnContext
	rig.tcp.Start()
	t.Cleanup(rig.tcp.Close)
	return rig
}

type socketReply struct {
	status int
	header http.Header
	body   []byte
}

// claudeCode sends one request the way Claude Code does in socket mode. A
// POST carries a model turn. bearer "" sends no Authorization (the
// policy_limits call); session "" sends no session header.
func claudeCode(t *testing.T, baseURL, method, path, bearer, sessionID string) socketReply {
	t.Helper()
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	}
	req, err := http.NewRequest(method, baseURL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", "claude-cli/2.1.328 (external, cli)")
	// What Bun's fetch offers; the proxy relays the upstream's encoding.
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	if sessionID != "" {
		req.Header.Set("X-Claude-Code-Session-Id", sessionID)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	// The raw body, as the proxy relays it: no transparent gunzip.
	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DisableKeepAlives: true, DisableCompression: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return socketReply{status: resp.StatusCode, header: resp.Header, body: got}
}

// answerError writes an Anthropic error answer with the headers Anthropic
// sends with one.
func answerError(status int, errorType, message string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		body, _ := json.Marshal(map[string]any{
			"type":       "error",
			"error":      map[string]string{"type": errorType, "message": message},
			"request_id": "req_socketguard",
		})
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", "req_socketguard")
		w.Header().Set("X-Should-Retry", "false")
		if status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Bearer realm="anthropic"`)
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// gzipped is answerError with the body gzip-encoded, as Anthropic answers a
// client that accepts it.
func gzipped(status int, errorType, message string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		body, _ := json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]string{"type": errorType, "message": message},
		})
		var packed bytes.Buffer
		zw := gzip.NewWriter(&packed)
		_, _ = zw.Write(body)
		_ = zw.Close()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(status)
		_, _ = w.Write(packed.Bytes())
	}
}

// requireGuardAnswer checks the guard's 503 for a refusal with status: none
// of the original's headers, and a body that none of Claude Code's
// authentication matchers can read as a refused credential.
func requireGuardAnswer(t *testing.T, reply socketReply, status int) {
	t.Helper()
	if reply.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body %q; want 503", reply.status, reply.body)
	}
	if got := reply.header.Get(subrouterReasonHeader); got != claudeSocketRefusedReason {
		t.Fatalf("X-Subrouter-Reason = %q, want %q", got, claudeSocketRefusedReason)
	}
	if got := reply.header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	for _, name := range []string{"X-Should-Retry", "WWW-Authenticate", "Request-Id", "Content-Encoding", "X-Subrouter-Account"} {
		if got := reply.header.Values(name); len(got) > 0 {
			t.Fatalf("the 503 carries the refusal's %s %q", name, got)
		}
	}
	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(reply.body, &envelope); err != nil {
		t.Fatalf("body %q is not an Anthropic error: %v", reply.body, err)
	}
	if envelope.Type != "error" || envelope.Error.Type != "api_error" {
		t.Fatalf("body %q: want an api_error", reply.body)
	}
	if want := fmt.Sprintf("(HTTP %d)", status); !strings.Contains(envelope.Error.Message, want) {
		t.Fatalf("message %q does not name %s", envelope.Error.Message, want)
	}
	lower := strings.ToLower(string(reply.body))
	for _, word := range []string{"revoked", "oauth", "expired", "authentication_error", "permission_error", "rate_limit_error", "overloaded_error"} {
		if strings.Contains(lower, word) {
			t.Fatalf("the 503's body says %q, a word Claude Code's matchers look for: %s", word, reply.body)
		}
	}
}

// Every source of a 401 a socket client could meet is answered 503: Anthropic
// refusing the pool account on a model turn (with the hold on, which must not
// hold it), Anthropic refusing the control-plane identity, the daemon refusing
// the caller, and a peer relaying its own 401. Before the guard, each reached
// Claude Code as the 401 its calibration shows, which Claude Code answers by
// sending the human's stored login whenever that login's token is fresh.
func TestClaudeSocketAnswers503ForEvery401(t *testing.T) {
	revoked := answerError(http.StatusUnauthorized, "authentication_error", "OAuth access token has been revoked.")
	for _, tc := range []struct {
		name   string
		opts   socketGuardOptions
		method string
		path   string
		// upstreamHits and peerHits are what the socket request may cost: one
		// attempt, never a hold's repeats.
		upstreamHits, peerHits int64
	}{
		{
			name:         "anthropic refuses the pool account on a model turn",
			opts:         socketGuardOptions{upstream: revoked, hold: holdTestPolicy(2 * time.Second)},
			method:       http.MethodPost,
			path:         "/v1/messages?beta=true",
			upstreamHits: 1,
		},
		{
			name:         "anthropic refuses the control-plane identity",
			opts:         socketGuardOptions{upstream: revoked, hold: holdTestPolicy(2 * time.Second)},
			method:       http.MethodGet,
			path:         "/api/oauth/profile",
			upstreamHits: 1,
		},
		{
			name:   "the daemon refuses the caller",
			opts:   socketGuardOptions{refuseLoopback: true, hold: holdTestPolicy(2 * time.Second)},
			method: http.MethodPost,
			path:   "/v1/messages?beta=true",
		},
		{
			name: "a peer relays a 401",
			opts: socketGuardOptions{
				noAccounts: true,
				peer:       answerError(http.StatusUnauthorized, "authentication_error", "invalid x-api-key"),
				hold:       holdTestPolicy(2 * time.Second),
			},
			method:   http.MethodPost,
			path:     "/v1/messages?beta=true",
			peerHits: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calibration := newSocketGuardRig(t, tc.opts)
			unguarded := claudeCode(t, calibration.tcp.URL, tc.method, tc.path, ClaudeSocketPlaceholderToken, "sess-401")
			if unguarded.status != http.StatusUnauthorized {
				t.Fatalf("calibration: off the socket the request got %d %q, want the 401 the guard replaces", unguarded.status, unguarded.body)
			}

			rig := newSocketGuardRig(t, tc.opts)
			reply := claudeCode(t, rig.socket.URL, tc.method, tc.path, ClaudeSocketPlaceholderToken, "sess-401")
			requireGuardAnswer(t, reply, http.StatusUnauthorized)
			if got := rig.upstreamHits.Load(); got != tc.upstreamHits {
				t.Fatalf("upstream hits = %d, want %d: a socket 401 is answered at once, never held", got, tc.upstreamHits)
			}
			if got := rig.peerHits.Load(); got != tc.peerHits {
				t.Fatalf("peer hits = %d, want %d", got, tc.peerHits)
			}
			if strings.Contains(rig.logs.String(), "proxy hold started") {
				t.Fatalf("the 401 started a hold:\n%s", rig.logs.String())
			}
			if got := rig.guard.Status().RefusalsAnswered503; got != 1 {
				t.Fatalf("refusals_answered_503 = %d, want 1", got)
			}
			if !strings.Contains(rig.logs.String(), "claude socket answered 503 in place of a refusal") ||
				!strings.Contains(rig.logs.String(), "status=401") {
				t.Fatalf("no log line for the rewrite:\n%s", rig.logs.String())
			}
		})
	}
}

// A 403 is answered 503 only when it names a revoked token, the one 403
// Claude Code also answers with the stored login; the body may be
// gzip-encoded. Every other 403 -- the model-access refusal Claude Code falls
// back on, one too long to be an error answer -- reaches the client exactly
// as the daemon would send it off the socket.
func TestClaudeSocketAnswers503ForARevoked403Only(t *testing.T) {
	t.Run("revoked", func(t *testing.T) {
		opts := socketGuardOptions{upstream: gzipped(http.StatusForbidden, "permission_error", "OAuth token has been revoked. Please obtain a new token.")}
		calibration := newSocketGuardRig(t, opts)
		unguarded := claudeCode(t, calibration.tcp.URL, http.MethodPost, "/v1/messages?beta=true", ClaudeSocketPlaceholderToken, "sess-403")
		if unguarded.status != http.StatusForbidden ||
			!strings.Contains(string(decodedUpstreamBody(unguarded.body, unguarded.header)), "OAuth token has been revoked") {
			t.Fatalf("calibration: off the socket got %d %q, want the revoked 403", unguarded.status, unguarded.body)
		}
		rig := newSocketGuardRig(t, opts)
		reply := claudeCode(t, rig.socket.URL, http.MethodPost, "/v1/messages?beta=true", ClaudeSocketPlaceholderToken, "sess-403")
		requireGuardAnswer(t, reply, http.StatusForbidden)
	})
	for _, tc := range []struct {
		name     string
		upstream http.HandlerFunc
	}{
		{"model access", answerError(http.StatusForbidden, "permission_error", "model: claude-opus-4-8 is not available to this organization")},
		{"longer than an error answer", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write(bytes.Repeat([]byte("forbidden by a proxy in front of the API. "), 4096))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := socketGuardOptions{upstream: tc.upstream}
			calibration := newSocketGuardRig(t, opts)
			want := claudeCode(t, calibration.tcp.URL, http.MethodPost, "/v1/messages?beta=true", ClaudeSocketPlaceholderToken, "sess-403")
			if want.status != http.StatusForbidden {
				t.Fatalf("calibration: off the socket got %d, want 403", want.status)
			}
			rig := newSocketGuardRig(t, opts)
			got := claudeCode(t, rig.socket.URL, http.MethodPost, "/v1/messages?beta=true", ClaudeSocketPlaceholderToken, "sess-403")
			if got.status != want.status || !bytes.Equal(got.body, want.body) ||
				got.header.Get("Content-Type") != want.header.Get("Content-Type") ||
				got.header.Get("X-Should-Retry") != want.header.Get("X-Should-Retry") {
				t.Fatalf("over the socket: %d %q (%v)\noff it: %d %q (%v)", got.status, truncate(got.body), got.header, want.status, truncate(want.body), want.header)
			}
			if n := rig.guard.Status().RefusalsAnswered503; n != 0 {
				t.Fatalf("refusals_answered_503 = %d for a 403 that names no revoked token", n)
			}
		})
	}
}

// A relayed answer can reach the guard still encoded -- a peer's relay copies
// Content-Encoding and the bytes -- so a 403 is read through its encoding.
func TestClaudeSocketGuardReadsAnEncoded403(t *testing.T) {
	for _, encoding := range []string{"gzip", ""} {
		t.Run("encoding="+encoding, func(t *testing.T) {
			guard := NewClaudeSocketGuard(slog.New(slog.NewTextHandler(io.Discard, nil)))
			handler := guard.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if encoding == "" {
					answerError(http.StatusForbidden, "permission_error", "OAuth token has been revoked.")(w, r)
					return
				}
				gzipped(http.StatusForbidden, "permission_error", "OAuth token has been revoked.")(w, r)
			}))
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
			request = request.WithContext(ClaudeSocketConnContext(request.Context()))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			requireGuardAnswer(t, socketReply{status: recorder.Code, header: recorder.Header(), body: recorder.Body.Bytes()}, http.StatusForbidden)
		})
	}
}

func truncate(body []byte) string {
	if len(body) > 200 {
		return string(body[:200]) + "..."
	}
	return string(body)
}

// The guard does not buffer what it passes: a streamed answer reaches the
// socket client event by event, as it reaches a TCP client.
func TestClaudeSocketStreamsAnswersThrough(t *testing.T) {
	for _, listener := range []string{"tcp", "socket"} {
		t.Run(listener, func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			done := func() { once.Do(func() { close(release) }) }
			t.Cleanup(done)
			rig := newSocketGuardRig(t, socketGuardOptions{upstream: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				// The proxy holds a Claude stream until its first decisive
				// event (the in-stream overload peek), so the interlock starts
				// after the first content event.
				_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-sonnet-4-5\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
				_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-time.After(10 * time.Second):
				}
				_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			}})
			baseURL := rig.tcp.URL
			if listener == "socket" {
				baseURL = rig.socket.URL
			}
			req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/messages?beta=true",
				strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("anthropic-version", "2023-06-01")
			req.Header.Set("User-Agent", "claude-cli/2.1.328 (external, cli)")
			req.Header.Set("X-Claude-Code-Session-Id", "sess-stream")
			req.Header.Set("Authorization", "Bearer "+ClaudeSocketPlaceholderToken)
			// The upstream holds the stream open for 10 s unless released, and
			// releases only after the client has read the first event: a
			// writer that buffers (status line included) delivers it at 10 s.
			sent := time.Now()
			resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			lines := make(chan string, 16)
			go func() {
				scanner := bufio.NewScanner(resp.Body)
				for scanner.Scan() {
					lines <- scanner.Text()
				}
				close(lines)
			}()
			select {
			case line := <-lines:
				if line != "event: message_start" {
					t.Fatalf("first line = %q", line)
				}
			case <-time.After(5 * time.Second):
			}
			if took := time.Since(sent); took > 5*time.Second {
				t.Fatalf("the first event reached the client %v after the request, not while the stream was open", took.Round(time.Millisecond))
			}
			done()
			var rest []string
			for line := range lines {
				rest = append(rest, line)
			}
			if !strings.Contains(strings.Join(rest, "\n"), "event: message_stop") {
				t.Fatalf("the stream's end never arrived: %q", rest)
			}
		})
	}
}

// The adoption canary: a socket client whose Authorization is not the
// placeholder is counted per request and warned about once per session, by
// kind and never by value. The placeholder, no credential at all, the
// daemon's own surface and every TCP client are not counted.
func TestClaudeSocketAdoptionCanary(t *testing.T) {
	rig := newSocketGuardRig(t, socketGuardOptions{})
	send := func(baseURL, method, path, bearer, sessionID string) {
		t.Helper()
		if reply := claudeCode(t, baseURL, method, path, bearer, sessionID); reply.status != http.StatusOK {
			t.Fatalf("%s %s = %d %q", method, path, reply.status, reply.body)
		}
	}
	send(rig.socket.URL, http.MethodPost, "/v1/messages?beta=true", ClaudeSocketPlaceholderToken, "sess-a")
	send(rig.socket.URL, http.MethodGet, "/api/claude_code/policy_limits", "", "")
	send(rig.socket.URL, http.MethodGet, "/_subrouter/health", socketGuardProxyToken, "")
	send(rig.tcp.URL, http.MethodPost, "/v1/messages?beta=true", socketGuardOAuthToken, "sess-tcp")
	if got := rig.guard.Status(); got != (ClaudeSocketGuardStatus{}) {
		t.Fatalf("status after the placeholder, no credential, the daemon's surface and a TCP client = %+v, want zero", got)
	}

	send(rig.socket.URL, http.MethodPost, "/v1/messages?beta=true", socketGuardOAuthToken, "sess-a")
	send(rig.socket.URL, http.MethodPost, "/v1/messages/count_tokens?beta=true", socketGuardOAuthToken, "sess-a")
	send(rig.socket.URL, http.MethodPost, "/v1/messages?beta=true", socketGuardOAuthToken, "sess-b")
	want := ClaudeSocketGuardStatus{AdoptionCanaryRequests: 3, AdoptionCanarySessions: 2}
	if got := rig.guard.Status(); got != want {
		t.Fatalf("status = %+v, want %+v", got, want)
	}
	logs := rig.logs.String()
	if n := strings.Count(logs, "may have adopted a stored claude.ai login"); n != 2 {
		t.Fatalf("canary warnings = %d, want one per session (2):\n%s", n, logs)
	}
	if !strings.Contains(logs, "credential=claude_oauth_access_token") || !strings.Contains(logs, "session=sess-b") {
		t.Fatalf("the warning does not name the credential's kind and session:\n%s", logs)
	}
	if strings.Contains(logs, "SOCKETGUARDSECRET") {
		t.Fatalf("the log carries the credential:\n%s", logs)
	}

	health := func(handler http.Handler) map[string]json.RawMessage {
		t.Helper()
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/_subrouter/health", nil))
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatalf("health %q: %v", recorder.Body.String(), err)
		}
		return payload
	}
	var reported ClaudeSocketGuardStatus
	if err := json.Unmarshal(health(rig.server.Handler())["claude_socket_guard"], &reported); err != nil || reported != want {
		t.Fatalf("private health claude_socket_guard = %+v (%v), want %+v", reported, err, want)
	}
	if raw, ok := health(rig.server.PublicHandler())["claude_socket_guard"]; ok {
		t.Fatalf("public health carries claude_socket_guard: %s", raw)
	}
}

func TestClaudeSocketCredentialKind(t *testing.T) {
	for value, want := range map[string]string{
		"Bearer sk-ant-oat01-abc":  "claude_oauth_access_token",
		"Bearer sk-ant-api03-abc":  "anthropic_api_key",
		"bearer sk-ant-ort01-abc":  "anthropic_other",
		"Bearer some-proxy-token":  "other",
		"Basic dXNlcjpwYXNz":       "not_bearer",
		"sk-ant-oat01-no-bearer-x": "not_bearer",
	} {
		if got := claudeSocketCredentialKind(value); got != want {
			t.Fatalf("claudeSocketCredentialKind(%q) = %q, want %q", value, got, want)
		}
	}
}
