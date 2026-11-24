package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/internal/transcript"
	"github.com/discordwell/subrouter/selectacct"
	"github.com/discordwell/subrouter/session"
	"github.com/gorilla/websocket"
)

// The client-boundary transcript (PATCHES §71): every exchange is recorded
// once, as the client saw it, whoever served it.

const (
	exchangeClientToken = "client-secret-token"
	exchangePeerToken   = "peer-secret-token"
	exchangeIDEToken    = "ide-secret-token"
)

// exchangeRig is a daemon with --transcripts in front of a fake Anthropic and,
// optionally, a peer daemon.
type exchangeRig struct {
	daemon        *httptest.Server
	handler       http.Handler
	recorder      *transcript.Recorder
	pool          *RemotePool
	upstreamHits  atomic.Int64
	peerHits      atomic.Int64
	peerAuthSeen  atomic.Value // string: the Authorization the peer received
	localAccount  string
	responseBytes []byte
}

type exchangeRigOptions struct {
	local    bool // one local Claude account; otherwise none, and the peer serves
	peer     bool
	hold     *HoldPolicy
	upstream func(rig *exchangeRig, w http.ResponseWriter, r *http.Request) // default: the SSE answer
	extra    *accounts.Account                                              // a second local account, ranked after the first
}

// exchangeSSE is a streamed answer large enough to cross several 64 KiB
// recording chunks.
func exchangeSSE() []byte {
	var out bytes.Buffer
	out.WriteString("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_x\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n")
	for i := 0; i < 40; i++ {
		out.WriteString("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"")
		out.WriteString(strings.Repeat("é", 2000))
		out.WriteString("\"}}\n\n")
	}
	out.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	return out.Bytes()
}

// writeSSE streams body in pieces with a flush after each, as an upstream does.
func writeSSE(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Request-Id", "req_upstream_1")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for len(body) > 0 {
		piece := body[:min(7000, len(body))]
		_, _ = w.Write(piece)
		if flusher != nil {
			flusher.Flush()
		}
		body = body[len(piece):]
	}
}

func newExchangeRig(t *testing.T, opts exchangeRigOptions) *exchangeRig {
	t.Helper()
	rig := &exchangeRig{responseBytes: exchangeSSE()}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rig.upstreamHits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		if opts.upstream != nil {
			opts.upstream(rig, w, r)
			return
		}
		writeSSE(w, rig.responseBytes)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	if opts.peer {
		peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == remotePoolUsagePath {
				return
			}
			rig.peerHits.Add(1)
			rig.peerAuthSeen.Store(r.Header.Get("Authorization"))
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set(subrouterAccountHeader, "peer-acct")
			writeSSE(w, rig.responseBytes)
		}))
		t.Cleanup(peer.Close)
		rig.pool = NewRemotePool(peer.URL, exchangePeerToken, "")
	}
	var pooled []accounts.Account
	var scores []selectacct.Score
	if opts.local {
		rig.localAccount = "local@example.com"
		pooled = []accounts.Account{{ID: rig.localAccount, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-local"}}
		scores = []selectacct.Score{{AccountID: rig.localAccount, Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1, Fresh: true}}
		if opts.extra != nil {
			pooled = append(pooled, *opts.extra)
			scores = append(scores, selectacct.Score{AccountID: opts.extra.ID, Provider: accounts.ProviderClaude, Headroom: 0.5, ShortHeadroom: 0.5, Fresh: true})
		}
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	rig.recorder = transcript.NewRecorder(filepath.Join(t.TempDir(), "transcripts"))
	t.Cleanup(func() { _ = rig.recorder.Close() })
	server := Server{
		ClaudeUpstream:  upstreamURL,
		Accounts:        pooled,
		Sessions:        store,
		SchedulerRef:    selectacct.NewSchedulerRef(selectacct.NewScheduler(scores)),
		MaxBodyBytes:    1 << 20,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		LocalProxyToken: exchangeClientToken,
		Hold:            opts.hold,
		RemotePool:      rig.pool,
		Transcripts:     rig.recorder,
	}
	rig.handler = server.Handler()
	rig.daemon = newProxyTestServer(t, rig.handler)
	return rig
}

// claudeTurnBody is a request body of a few hundred KiB, so the request is
// recorded in several chunks too.
func claudeTurnBody() []byte {
	return []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"` + strings.Repeat("ü", 90000) + `"}]}`)
}

// turn sends one Claude Code subagent turn and returns what the client read.
func (rig *exchangeRig) turn(t *testing.T, path, sessionID string, body []byte) []byte {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, rig.daemon.URL+path+"?beta=true", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+exchangeClientToken)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("X-Claude-Code-Session-Id", sessionID)
	req.Header.Set("X-Claude-Code-Agent-Id", "agent-7")
	req.Header.Set("X-Claude-Code-Parent-Agent-Id", "agent-main")
	req.Header.Set("X-Claude-Code-Agent-Type", "general-purpose")
	req.Header.Set("X-Claude-Code-Request-Class", "subagent")
	req.Header.Set("X-Client-Request-Id", "creq-1")
	req.Header.Set("X-Claude-Code-Ide-Authorization", exchangeIDEToken)
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
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %.300q", resp.StatusCode, got)
	}
	return got
}

// exchangeEvents waits until the session's file holds the response summary
// (written once the handler has returned) and returns every event.
func exchangeEvents(t *testing.T, recorder *transcript.Recorder, agentType, sessionID string) ([]map[string]any, []byte) {
	t.Helper()
	path := recorder.PathForSession(agentType, sessionID)
	deadline := time.Now().Add(10 * time.Second)
	for {
		_ = recorder.Flush()
		raw, _ := os.ReadFile(path)
		var events []map[string]any
		done := false
		for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal(line, &event); err != nil {
				t.Fatalf("transcript line does not parse: %v", err)
			}
			events = append(events, event)
			payload, _ := event["payload"].(map[string]any)
			if event["type"] == "http_body" && payload["direction"] == "upstream_to_client" {
				done = true
			}
		}
		if done {
			return events, raw
		}
		if time.Now().After(deadline) {
			t.Fatalf("no response summary in %s after 10s; file:\n%.2000s", path, raw)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func eventsOf(events []map[string]any, eventType, direction string) []map[string]any {
	var out []map[string]any
	for _, event := range events {
		payload, _ := event["payload"].(map[string]any)
		if event["type"] == eventType && (direction == "" || payload["direction"] == direction) {
			out = append(out, payload)
		}
	}
	return out
}

// requireOneExchange checks that the file holds exactly one exchange: one
// meta, one request stream and one response stream, all naming the same
// exchange id, with the bytes the client sent and received. It returns the
// meta and the response summary.
func requireOneExchange(t *testing.T, events []map[string]any, raw []byte, path string, sent, received []byte) (map[string]any, map[string]any) {
	t.Helper()
	metas := eventsOf(events, "subrouter_meta", "")
	requests := eventsOf(events, "http_body", "client_to_upstream")
	responses := eventsOf(events, "http_body", "upstream_to_client")
	if len(metas) != 1 || len(requests) != 1 || len(responses) != 1 {
		t.Fatalf("metas %d, request streams %d, response streams %d; want exactly one of each (recorded once)\n%.3000s", len(metas), len(requests), len(responses), raw)
	}
	meta, request, response := metas[0], requests[0], responses[0]
	id, _ := meta["exchange_id"].(string)
	if id == "" || meta["request_id"] != id || meta["boundary"] != "client" {
		t.Fatalf("meta exchange_id %q request_id %v boundary %v", id, meta["request_id"], meta["boundary"])
	}
	for _, event := range append(eventsOf(events, "http_body_chunk", ""), request, response) {
		if event["exchange_id"] != id || event["request_id"] != id {
			t.Fatalf("a stream event names exchange %v / request %v, want %s", event["exchange_id"], event["request_id"], id)
		}
	}
	assertTranscriptPayload(t, events, "http_body", "client_to_upstream", sent)
	assertTranscriptPayload(t, events, "http_body", "upstream_to_client", received)
	if request["partial"] != nil || request["truncated"] != nil {
		t.Fatalf("request stream marked partial/truncated: %v", request)
	}
	if chunks, _ := request["chunks"].(float64); chunks < 2 {
		t.Fatalf("request recorded in %v chunks; the test body should take several", request["chunks"])
	}
	if chunks, _ := response["chunks"].(float64); chunks < 2 {
		t.Fatalf("response recorded in %v chunks; the test answer should take several", response["chunks"])
	}
	if response["status"] != float64(http.StatusOK) || response["aborted"] != nil {
		t.Fatalf("response summary status %v aborted %v", response["status"], response["aborted"])
	}
	// The agent fields, lifted from Claude Code's headers.
	for field, want := range map[string]string{
		"cc_session_id": "sess-x", "agent_id": "agent-7", "parent_agent_id": "agent-main",
		"cc_agent_type": "general-purpose", "request_class": "subagent", "client_request_id": "creq-1",
		"query": "beta=true", "path": path, "method": http.MethodPost,
	} {
		if meta[field] != want {
			t.Fatalf("meta %s = %v, want %q", field, meta[field], want)
		}
	}
	headers := meta["headers"].(map[string]any)
	for _, name := range []string{"Authorization", "X-Claude-Code-Ide-Authorization"} {
		if got := headers[name].([]any); len(got) != 1 || got[0] != "<redacted>" {
			t.Fatalf("meta header %s = %v, want redacted", name, got)
		}
	}
	// No credential reaches the file, whichever event might have carried it.
	for _, secret := range []string{exchangeClientToken, exchangePeerToken, exchangeIDEToken, "tok-local"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("the transcript contains the credential %q", secret)
		}
	}
	return meta, response
}

// The gap this closes: a Claude turn the peer served was not recorded at
// all. Now the client's request and the peer's answer, byte for byte, are in
// the session's file once, with the peer named and its token absent.
func TestClientBoundaryTranscriptRecordsAPeerServedTurn(t *testing.T) {
	for _, tc := range []struct {
		name string
		hold *HoldPolicy
	}{
		{name: "hold on", hold: holdTestPolicy(2 * time.Second)},
		{name: "hold off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newExchangeRig(t, exchangeRigOptions{peer: true, hold: tc.hold})
			sent := claudeTurnBody()
			received := rig.turn(t, "/v1/messages", "sess-x", sent)
			if !bytes.Equal(received, rig.responseBytes) {
				t.Fatalf("calibration: the client did not get the peer's answer")
			}
			if rig.peerHits.Load() != 1 || rig.upstreamHits.Load() != 0 {
				t.Fatalf("calibration: peer hits %d, local upstream hits %d; want the peer to serve", rig.peerHits.Load(), rig.upstreamHits.Load())
			}
			if got, _ := rig.peerAuthSeen.Load().(string); got != "Bearer "+exchangePeerToken {
				t.Fatalf("calibration: the peer saw Authorization %q", got)
			}
			events, raw := exchangeEvents(t, rig.recorder, "claude", "sess-x")
			_, response := requireOneExchange(t, events, raw, "/v1/messages", sent, received)
			if response["served_by"] != "peer" || response["peer"] != rig.pool.name() ||
				response["peer_account"] != "peer-acct" || response["account"] != "peer:"+rig.pool.name() {
				t.Fatalf("response summary served_by %v peer %v peer_account %v account %v",
					response["served_by"], response["peer"], response["peer_account"], response["account"])
			}
			headers, _ := response["response_headers"].(map[string]any)
			if got, _ := headers[subrouterPoolHeader].([]any); len(got) != 1 || got[0] != "remote" {
				t.Fatalf("response headers as the client got them: %s = %v", subrouterPoolHeader, got)
			}
		})
	}
}

// A turn the local pool served is recorded once, at the client boundary:
// the local recorders stand down, and the summary names the account.
func TestClientBoundaryTranscriptRecordsALocalTurnOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		hold *HoldPolicy
	}{
		{name: "hold on", hold: holdTestPolicy(2 * time.Second)},
		{name: "hold off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newExchangeRig(t, exchangeRigOptions{local: true, hold: tc.hold})
			sent := claudeTurnBody()
			received := rig.turn(t, "/v1/messages", "sess-x", sent)
			if rig.upstreamHits.Load() != 1 {
				t.Fatalf("calibration: local upstream hits %d, want 1", rig.upstreamHits.Load())
			}
			events, raw := exchangeEvents(t, rig.recorder, "claude", "sess-x")
			_, response := requireOneExchange(t, events, raw, "/v1/messages", sent, received)
			if response["served_by"] != "local" || response["account"] != rig.localAccount || response["provider"] != "claude" {
				t.Fatalf("response summary served_by %v account %v provider %v", response["served_by"], response["account"], response["provider"])
			}
		})
	}
}

// A turn the hold retried is one exchange: the failed attempt the client
// never saw is not recorded, and the summary names who answered last.
// count_tokens has no same-account overload retries, so the hold itself
// runs the second attempt.
func TestClientBoundaryTranscriptRecordsTheAnswerAHeldTurnGot(t *testing.T) {
	var failed atomic.Bool
	rig := newExchangeRig(t, exchangeRigOptions{local: true, hold: holdTestPolicy(5 * time.Second),
		upstream: func(rig *exchangeRig, w http.ResponseWriter, r *http.Request) {
			if failed.CompareAndSwap(false, true) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadGateway)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"upstream connect error"}}`)
				return
			}
			writeSSE(w, rig.responseBytes)
		}})
	sent := claudeTurnBody()
	received := rig.turn(t, "/v1/messages/count_tokens", "sess-x", sent)
	if !bytes.Equal(received, rig.responseBytes) || rig.upstreamHits.Load() != 2 {
		t.Fatalf("calibration: upstream hits %d; want a failed attempt the hold retried, then the answer", rig.upstreamHits.Load())
	}
	events, raw := exchangeEvents(t, rig.recorder, "claude", "sess-x")
	_, response := requireOneExchange(t, events, raw, "/v1/messages/count_tokens", sent, received)
	if response["served_by"] != "local" || response["account"] != rig.localAccount {
		t.Fatalf("response summary served_by %v account %v", response["served_by"], response["account"])
	}
	if bytes.Contains(raw, []byte("upstream connect error")) {
		t.Fatal("the attempt the hold swallowed was recorded")
	}
}

// --transcripts unset: the proxy route is exactly the handler it wraps.
func TestClientBoundaryTranscriptIsOffWithoutTranscripts(t *testing.T) {
	next := &countingHandler{}
	if got := (Server{}).recordClientExchange(next); got != http.Handler(next) {
		t.Fatalf("with no recorder the route is wrapped (%T)", got)
	}
	recorder := transcript.NewRecorder(t.TempDir())
	t.Cleanup(func() { _ = recorder.Close() })
	if got := (Server{Transcripts: recorder}).recordClientExchange(next); got == http.Handler(next) {
		t.Fatal("calibration: with a recorder the route is not wrapped")
	}
}

type countingHandler struct{ hits atomic.Int64 }

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.hits.Add(1)
	w.WriteHeader(http.StatusNoContent)
}

// A refused caller and a health probe are not turns: nothing is recorded,
// and a refused caller's body is still not read.
func TestClientBoundaryTranscriptSkipsWhatIsNotATurn(t *testing.T) {
	rig := newExchangeRig(t, exchangeRigOptions{local: true})
	read := &countingReader{Reader: strings.NewReader(`{"model":"claude-sonnet-4-5"}`)}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:31415/v1/messages", read)
	req.ContentLength = 29
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer wrong")
	req.Header.Set("X-Claude-Code-Session-Id", "sess-refused")
	recorder := httptest.NewRecorder()
	rig.handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("calibration: status %d, want 401", recorder.Code)
	}
	if read.n.Load() != 0 {
		t.Fatalf("the refused caller's body was read (%d bytes)", read.n.Load())
	}
	_ = rig.recorder.Flush()
	if _, err := os.Stat(rig.recorder.PathForSession("claude", "sess-refused")); !os.IsNotExist(err) {
		t.Fatalf("a refused request was recorded (stat err %v)", err)
	}
}

// A Codex socket relayed to a peer gets a meta naming the peer, and the
// relay's message records name the same request id. Before, the messages were
// recorded with nothing saying what the socket was.
func TestPeerWebSocketTranscriptHasMetaLinkedToItsMessages(t *testing.T) {
	peer := accountStampingPeer(t, nil)
	recorder := transcript.NewRecorder(filepath.Join(t.TempDir(), "transcripts"))
	t.Cleanup(func() { _ = recorder.Close() })
	pool := NewRemotePool(peer.URL, exchangePeerToken, "")
	front, done := poolFront(t, Server{RemotePool: pool, Transcripts: recorder}, remotePoolHoldSet)
	header := http.Header{"Authorization": {"Bearer " + exchangeClientToken}, "X-Subrouter-Lease": {"lease-secret"}}
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http")+"/v1/responses", header)
	if err != nil {
		t.Fatalf("websocket dial failed: %v", err)
	}
	_ = resp.Body.Close()
	echoOnce(t, conn, "hello")
	_ = conn.Close()
	waitForSocketDone(t, done)

	_ = recorder.Flush()
	raw, err := os.ReadFile(recorder.PathForSession("codex", wsHoldSession))
	if err != nil {
		t.Fatal(err)
	}
	var metas, messages []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		payload := event["payload"].(map[string]any)
		switch event["type"] {
		case "subrouter_meta":
			metas = append(metas, payload)
		case "websocket_message":
			messages = append(messages, payload)
		}
	}
	if len(metas) != 1 || len(messages) != 2 {
		t.Fatalf("metas %d messages %d, want 1 and 2:\n%s", len(metas), len(messages), raw)
	}
	meta := metas[0]
	id, _ := meta["request_id"].(string)
	if id == "" || meta["exchange_id"] != id || meta["transport"] != "websocket" || meta["served_by"] != "peer" ||
		meta["peer"] != pool.name() || meta["peer_account"] != "peer-acct" {
		t.Fatalf("meta = %v", meta)
	}
	for _, message := range messages {
		if message["request_id"] != id {
			t.Fatalf("message request_id %v, want the meta's %s", message["request_id"], id)
		}
	}
	for _, secret := range []string{exchangeClientToken, exchangePeerToken, "lease-secret"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("the transcript contains the credential %q", secret)
		}
	}
}

// The summary names the account that answered, not the one the turn was
// placed on: here the first account's window is spent and the transport
// fails over to the second.
func TestClientBoundaryTranscriptNamesTheAccountThatAnswered(t *testing.T) {
	var mu sync.Mutex
	var tokens []string
	rig := newExchangeRig(t, exchangeRigOptions{local: true, upstream: func(rig *exchangeRig, w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		tokens = append(tokens, token)
		mu.Unlock()
		if token == "tok-local" {
			w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
			w.Header().Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10))
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"limit reached"}}`)
			return
		}
		writeSSE(w, rig.responseBytes)
	}, extra: &accounts.Account{ID: "second@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-second"}})
	sent := claudeTurnBody()
	received := rig.turn(t, "/v1/messages", "sess-x", sent)
	mu.Lock()
	seen := append([]string(nil), tokens...)
	mu.Unlock()
	if len(seen) != 2 || seen[0] != "tok-local" || seen[1] != "tok-second" {
		t.Fatalf("calibration: upstream saw %v; want the first account refused, then the second", seen)
	}
	events, raw := exchangeEvents(t, rig.recorder, "claude", "sess-x")
	_, response := requireOneExchange(t, events, raw, "/v1/messages", sent, received)
	if response["served_by"] != "local" || response["account"] != "second@example.com" {
		t.Fatalf("response summary served_by %v account %v; want the account that answered", response["served_by"], response["account"])
	}
	if bytes.Contains(raw, []byte("limit reached")) {
		t.Fatal("the refused attempt the client never saw was recorded")
	}
}
