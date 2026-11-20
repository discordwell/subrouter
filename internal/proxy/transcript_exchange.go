package proxy

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/transcript"
	"github.com/manaflow-ai/subrouter/session"
)

// The client-boundary transcript (PATCHES §71).
//
// The JSONL recorder (--transcripts DIR) used to write a turn down only where
// the LOCAL pool served it: proxyHandler's meta and request body, and the
// reverse proxy's response body. A turn handed to a peer, Bedrock, the Fable
// fallback or Azure was never written, and on a machine with peers most turns
// are served by the peers. recordClientExchange records each HTTP exchange
// once, as the client saw it: the request bytes the client sent and the
// response bytes the client received, whoever served them. Serving routes only
// annotate the exchange (noteExchangeServed and friends), and the local
// recorders stand down for any request that carries one, so a locally served
// turn is still written once.
//
// Off unless --transcripts is set. Segment mode (the Azure stream) keeps its
// own capture points. A websocket upgrade bypasses it: the socket relays
// record their own messages.

type clientExchangeKey struct{}

// clientExchangeFrom is the exchange the client-boundary recorder opened for
// this request, or nil when it is not recording one (transcripts off, segment
// mode, a websocket, a handler mounted without it).
func clientExchangeFrom(ctx context.Context) *clientExchange {
	if ctx == nil {
		return nil
	}
	x, _ := ctx.Value(clientExchangeKey{}).(*clientExchange)
	return x
}

type exchangeState int

const (
	// exchangePending: the handler has not yet said which session this is.
	// Request bytes read meanwhile are held (up to requestBodyMaxWhole).
	exchangePending exchangeState = iota
	// exchangeRecording: identified and worth recording.
	exchangeRecording
	// exchangeSkipped: never identified as a turn, or a session that is not
	// recorded (the Codex model catalog). Nothing is written.
	exchangeSkipped
)

// exchangeServed is who answered the client, as the serving routes report it.
// The last report wins: a failed attempt the hold swallowed is replaced by the
// one the client got.
type exchangeServed struct {
	route    string // local, peer, fallback, fable-bedrock-primary, fable-fallback, azure-codex
	account  string // local account id
	provider string
	upstream string
	peer     string
	peerAcct string
}

// clientExchange is one HTTP request recorded at the client boundary. Every
// event it writes carries exchange_id (also written as request_id, the name
// the readers join on), so the meta, the request stream and the response
// stream of one exchange link without guessing.
type clientExchange struct {
	recorder   *transcript.Recorder
	id         string
	started    time.Time
	remoteAddr string
	meta       map[string]any // built at arrival; headers already redacted
	encoding   string

	mu        sync.Mutex
	state     exchangeState
	agentType string
	sessionID string
	served    exchangeServed

	hasBody       bool
	reqStream     string
	reqHash       hash.Hash
	reqPending    []byte
	reqOverflow   bool // more than requestBodyMaxWhole arrived before identification
	reqBytes      int64
	reqOffset     int64 // bytes written out as chunks
	reqChunks     int
	reqEOF        bool
	reqDone       bool
	respStream    string
	respHash      hash.Hash
	respStatus    int
	respHeaders   map[string][]string
	respBytes     int64
	respChunks    int
	respChunkMeta map[string]any
}

// recordClientExchange wraps the provider proxy (the "/" route, outside the
// hold) in the client-boundary recorder. With transcripts off, or in segment
// mode, it returns next unchanged.
func (s Server) recordClientExchange(next http.Handler) http.Handler {
	recorder := s.Transcripts
	if recorder == nil || !recorder.Enabled() || recorder.SegmentMode() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			next.ServeHTTP(w, r)
			return
		}
		x := newClientExchange(recorder, r)
		r = r.WithContext(context.WithValue(r.Context(), clientExchangeKey{}, x))
		if r.Body != nil && r.Body != http.NoBody {
			x.hasBody = true
			r.Body = &exchangeRequestBody{ReadCloser: r.Body, x: x}
		}
		completed := false
		defer func() {
			// Runs on a panic too (an aborted relay): what the client got is
			// written down, marked aborted, and the panic goes on.
			x.finish(!completed, r.Context().Err() != nil)
		}()
		next.ServeHTTP(&exchangeResponseWriter{ResponseWriter: w, x: x}, r)
		completed = true
	})
}

func newClientExchange(recorder *transcript.Recorder, r *http.Request) *clientExchange {
	id := nextTranscriptRequestID()
	started := time.Now()
	meta := map[string]any{
		"transport":   "http",
		"boundary":    "client",
		"exchange_id": id,
		"request_id":  id,
		"received_at": started.UTC().Format(time.RFC3339Nano),
		"method":      r.Method,
		"path":        r.URL.Path,
		"headers":     transcript.RedactedHeaders(r.Header),
	}
	if query := redactedTranscriptQuery(r.URL.RawQuery); query != "" {
		meta["query"] = query
	}
	addTranscriptAgentFields(meta, r.Header)
	addTranscriptClient(meta, transcriptClientName(r), session.ExtractUserEmail(r))
	return &clientExchange{
		recorder: recorder, id: id, started: started, remoteAddr: r.RemoteAddr,
		meta: meta, encoding: r.Header.Get("Content-Encoding"),
		reqStream: nextTranscriptStreamID(), reqHash: sha256.New(),
		respStream: nextTranscriptStreamID(), respHash: sha256.New(),
	}
}

// transcriptAgentHeaders are the client's own names for the agent behind a
// request, lifted to top-level meta fields so a reader can file each request
// under its agent without parsing headers. Every header stays in "headers"
// too. agent_type and session_id are taken (the recorder's own), hence the
// cc_ names.
var transcriptAgentHeaders = []struct{ field, header string }{
	{"cc_session_id", "X-Claude-Code-Session-Id"},
	{"agent_id", "X-Claude-Code-Agent-Id"},
	{"parent_agent_id", "X-Claude-Code-Parent-Agent-Id"},
	{"cc_agent_type", "X-Claude-Code-Agent-Type"},
	{"request_class", "X-Claude-Code-Request-Class"},
	{"request_class_1p", "X-Claude-1p-Request-Class"},
	{"auxiliary_type", "X-Claude-Code-Auxiliary-Type"},
	{"prompt_id", "X-Claude-Code-Prompt-Id"},
	{"client_request_id", "X-Client-Request-Id"},
}

func addTranscriptAgentFields(meta map[string]any, header http.Header) {
	for _, field := range transcriptAgentHeaders {
		if value := strings.TrimSpace(header.Get(field.header)); value != "" {
			meta[field.field] = value
		}
	}
}

// redactedTranscriptQuery is the client's query string with the value of any
// parameter whose name looks like a credential replaced. Unchanged when none
// does (Claude's ?beta=true).
func redactedTranscriptQuery(raw string) string {
	if raw == "" {
		return ""
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return "<unparsed>"
	}
	redacted := false
	for name := range values {
		// An OAuth "code" is a credential in a query, though not in a
		// header name (X-Claude-Code-*).
		if transcript.SensitiveName(name) || strings.EqualFold(name, "code") {
			values[name] = []string{"<redacted>"}
			redacted = true
		}
	}
	if !redacted {
		return raw
	}
	return values.Encode()
}

// identifyClientExchange tells the exchange riding r which session file it
// belongs in, once the handler has settled it (lease, realtime call and
// catalog rewrites included). The first call decides; the hold's later
// attempts of the same request repeat it harmlessly. Each call starts an
// attempt, so who served is forgotten: the client gets the last attempt's
// answer, and only that attempt's reports may name who gave it.
func identifyClientExchange(r *http.Request, agentType, sessionID, model string) {
	x := clientExchangeFrom(r.Context())
	if x == nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.served = exchangeServed{}
	if x.state != exchangePending {
		return
	}
	if !transcriptWorthRecording(sessionID) {
		x.state = exchangeSkipped
		x.reqPending = nil
		return
	}
	x.state = exchangeRecording
	x.agentType, x.sessionID = agentType, sessionID
	if model != "" {
		x.meta["model"] = model
	}
	x.recorder.RecordRequestMeta(agentType, sessionID, x.remoteAddr, x.meta)
	x.meta = nil
	if !x.reqOverflow && len(x.reqPending) > 0 {
		x.recordRequestChunksLocked(x.reqPending)
	}
	x.reqPending = nil
	if x.reqEOF {
		x.finishRequestLocked()
	}
}

// noteExchangeServed records who answered (the last report wins).
func noteExchangeServed(ctx context.Context, served exchangeServed) {
	if x := clientExchangeFrom(ctx); x != nil {
		x.mu.Lock()
		x.served = served
		x.mu.Unlock()
	}
}

func localExchangeServed(account accounts.Account, upstream *url.URL) exchangeServed {
	return exchangeServed{route: "local", account: account.ID, provider: string(account.Provider), upstream: redactedTranscriptURL(upstream)}
}

// noteExchangeLocalAnswer is the reverse proxy's report of the account that
// answered (routedResponseAccount). An empty account is a fallback stage
// inside the transport: a peer stage has reported itself already, and any
// other stage replaces the placement this attempt reported.
func noteExchangeLocalAnswer(ctx context.Context, account accounts.Account, upstream *url.URL) {
	x := clientExchangeFrom(ctx)
	if x == nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	switch {
	case account.ID != "":
		x.served = localExchangeServed(account, upstream)
	case x.served.route == "local":
		x.served = exchangeServed{route: "fallback"}
	}
}

func (x *clientExchange) streamPayload() map[string]any {
	return map[string]any{"exchange_id": x.id, "request_id": x.id}
}

// requestRead is called with every slice the handler reads from the client's
// body, in order, each byte once.
func (x *clientExchange) requestRead(p []byte, eof bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.reqDone || x.state == exchangeSkipped {
		return
	}
	if len(p) > 0 {
		_, _ = x.reqHash.Write(p)
		x.reqBytes += int64(len(p))
		switch {
		case x.reqOverflow:
		case x.state == exchangeRecording:
			x.recordRequestChunksLocked(p)
		case len(x.reqPending)+len(p) > requestBodyMaxWhole:
			// Not held whole before the handler named the session: the
			// size and digest are still recorded, the content is not.
			x.reqOverflow = true
			x.reqPending = nil
		default:
			x.reqPending = append(x.reqPending, p...)
		}
	}
	if eof {
		x.reqEOF = true
		if x.state == exchangeRecording {
			x.finishRequestLocked()
		}
	}
}

func (x *clientExchange) recordRequestChunksLocked(body []byte) {
	payload := x.streamPayload()
	if x.encoding != "" {
		payload["content_encoding"] = x.encoding
	}
	for len(body) > 0 {
		chunk := body
		if len(chunk) > transcriptHTTPChunkBytes {
			chunk = body[:transcriptHTTPChunkBytes]
		}
		x.recorder.RecordPayloadChunk(x.agentType, x.sessionID, "http_body", "client_to_upstream", x.reqStream, x.reqChunks, x.reqOffset, chunk, payload)
		x.reqChunks++
		x.reqOffset += int64(len(chunk))
		body = body[len(chunk):]
	}
}

func (x *clientExchange) finishRequestLocked() {
	if x.reqDone || !x.hasBody {
		return
	}
	x.reqDone = true
	payload := x.streamPayload()
	if x.encoding != "" {
		payload["content_encoding"] = x.encoding
	}
	if !x.reqEOF {
		// The handler stopped reading (a refusal, a client that left).
		payload["partial"] = true
	}
	if x.reqOverflow {
		payload["truncated"] = true
	}
	x.recorder.RecordPayloadSummary(x.agentType, x.sessionID, "http_body", "client_to_upstream", x.reqStream, x.reqBytes, hex.EncodeToString(x.reqHash.Sum(nil)), x.reqChunks, payload)
}

// responseBegin notes the status and headers the client is sent: the first
// final status, before it goes out. An informational 1xx is passed through
// and not taken as the status.
func (x *clientExchange) responseBegin(status int, header http.Header) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.respStatus != 0 || status < http.StatusOK {
		return
	}
	x.respStatus = status
	if x.state == exchangeRecording {
		x.respHeaders = transcript.RedactedHeaders(header)
	}
}

func (x *clientExchange) responseWrote(p []byte) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.state != exchangeRecording || len(p) == 0 {
		return
	}
	_, _ = x.respHash.Write(p)
	if x.respChunkMeta == nil {
		x.respChunkMeta = x.streamPayload()
		x.respChunkMeta["status"] = x.respStatus
	}
	offset := x.respBytes
	for body := p; len(body) > 0; {
		chunk := body
		if len(chunk) > transcriptHTTPChunkBytes {
			chunk = body[:transcriptHTTPChunkBytes]
		}
		x.recorder.RecordPayloadChunk(x.agentType, x.sessionID, "http_body", "upstream_to_client", x.respStream, x.respChunks, offset, chunk, x.respChunkMeta)
		x.respChunks++
		offset += int64(len(chunk))
		body = body[len(chunk):]
	}
	x.respBytes = offset
}

// finish writes the request stream's summary (if the body never reached EOF)
// and the response stream's summary, which names who served the client.
func (x *clientExchange) finish(aborted, clientGone bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.state == exchangePending {
		// Never named as a turn (a refusal, a health probe, an early error).
		x.state = exchangeSkipped
	}
	x.reqPending = nil
	if x.state != exchangeRecording {
		x.reqDone = true
		return
	}
	x.finishRequestLocked()
	status := x.respStatus
	if status == 0 && !aborted {
		// The handler returned without writing: net/http sends an empty 200.
		status = http.StatusOK
	}
	payload := x.streamPayload()
	payload["status"] = status
	payload["duration_ms"] = time.Since(x.started).Milliseconds()
	if x.respHeaders != nil {
		payload["response_headers"] = x.respHeaders
		if encoding := firstHeader(x.respHeaders, "Content-Encoding"); encoding != "" {
			payload["content_encoding"] = encoding
		}
	}
	if aborted {
		payload["aborted"] = true
	}
	if clientGone {
		payload["client_canceled"] = true
	}
	served := x.served
	if served.route != "" {
		payload["served_by"] = served.route
	}
	switch served.route {
	case "":
	case "local":
		// "account" is a LOCAL account id: the analytics debit it.
		payload["account"] = served.account
		if served.provider != "" {
			payload["provider"] = served.provider
		}
		if served.upstream != "" {
			payload["upstream"] = served.upstream
		}
	case "peer":
		payload["account"] = "peer:" + served.peer
		payload["peer"] = served.peer
		if served.peerAcct != "" {
			payload["peer_account"] = served.peerAcct
		}
	default:
		payload["account"] = served.route
	}
	x.recorder.RecordPayloadSummary(x.agentType, x.sessionID, "http_body", "upstream_to_client", x.respStream, x.respBytes, hex.EncodeToString(x.respHash.Sum(nil)), x.respChunks, payload)
}

func firstHeader(headers map[string][]string, name string) string {
	if values := headers[http.CanonicalHeaderKey(name)]; len(values) > 0 {
		return values[0]
	}
	return ""
}

// exchangeRequestBody tees the client's body into the exchange as the handler
// reads it. It never reads on its own: what the handler does not read is not
// recorded (and is marked partial).
type exchangeRequestBody struct {
	io.ReadCloser
	x *clientExchange
}

func (b *exchangeRequestBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.x.requestRead(p[:n], err == io.EOF)
	return n, err
}

// exchangeResponseWriter records what reaches the client. Flush, Hijack and
// Unwrap pass through, so streaming and http.ResponseController behave as
// before.
type exchangeResponseWriter struct {
	http.ResponseWriter
	x *clientExchange
}

func (w *exchangeResponseWriter) WriteHeader(status int) {
	w.x.responseBegin(status, w.ResponseWriter.Header())
	w.ResponseWriter.WriteHeader(status)
}

func (w *exchangeResponseWriter) Write(p []byte) (int, error) {
	w.x.responseBegin(http.StatusOK, w.ResponseWriter.Header())
	n, err := w.ResponseWriter.Write(p)
	if n > 0 {
		w.x.responseWrote(p[:n])
	}
	return n, err
}

func (w *exchangeResponseWriter) Flush() {
	_ = w.FlushError()
}

// FlushError keeps a streaming write failure visible to ResponseController.
func (w *exchangeResponseWriter) FlushError() error {
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if !errors.Is(err, http.ErrNotSupported) {
		// A flush before any status commits net/http's implicit 200.
		w.x.responseBegin(http.StatusOK, w.ResponseWriter.Header())
	}
	return err
}

func (w *exchangeResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *exchangeResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

// recordPeerWebSocketMeta writes the meta of a websocket relayed to a peer
// and returns r carrying its request id, which the relay's message records
// then name. Before, the messages were recorded with nothing saying what the
// socket was or who served it. r comes back unchanged when nothing records.
func (s Server) recordPeerWebSocketMeta(r *http.Request, peer, peerAccount, agentType, sessionID string) *http.Request {
	if s.Transcripts == nil || !s.Transcripts.Enabled() || s.Transcripts.SegmentMode() || !transcriptWorthRecording(sessionID) {
		return r
	}
	requestID := nextTranscriptRequestID()
	metadata := map[string]any{
		"transport":   "websocket",
		"exchange_id": requestID,
		"request_id":  requestID,
		"served_by":   "peer",
		"account":     "peer:" + peer,
		"peer":        peer,
		"method":      r.Method,
		"path":        r.URL.Path,
		"headers":     transcript.RedactedHeaders(r.Header),
	}
	if peerAccount != "" {
		metadata["peer_account"] = peerAccount
	}
	if query := redactedTranscriptQuery(r.URL.RawQuery); query != "" {
		metadata["query"] = query
	}
	addTranscriptAgentFields(metadata, r.Header)
	addTranscriptClient(metadata, transcriptClientName(r), session.ExtractUserEmail(r))
	s.Transcripts.RecordRequestMeta(agentType, sessionID, r.RemoteAddr, metadata)
	return r.WithContext(withTranscriptRequestID(r.Context(), requestID))
}
