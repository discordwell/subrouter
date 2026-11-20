package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/session"
)

// The Claude socket's guard: no refusal Claude Code answers with the human's
// own login, and a canary for the sessions that already did.
//
// Claude Code in socket mode (SUBROUTER_CLAUDE_SOCKET, PATCHES section 29)
// holds Claude Code's placeholder bearer, ssh-placeholder, and the daemon
// serves it from the pool. Two answers make Claude Code 2.1.328 swap that
// placeholder for the access token of the login stored on this machine (the
// human's own `Claude Code-credentials`): a 401, and a 403 whose message says
// the token "has been revoked". Its model retry loop and its API helper both
// run their 401 recovery on exactly those, and in socket mode the guard that
// should keep the environment's token never fires. Every direct call the
// session makes after that -- remote settings, connectors -- runs as the human.
// On 2026-11-19 the laptop's daemon gave 39 such answers between 01:09 and
// 03:09, 33 of them after the socket opened: an identity whose token Anthropic
// had revoked answered /api/oauth/profile and /api/claude_code/policy_limits
// with `401 OAuth access token has been revoked`.
//
// So a connection on the Claude socket never receives either. The guard sits
// outside the private listener's whole handler, so it covers every path and
// every source -- an Anthropic 401 on a pool account or the identity, a
// peer's relayed 401, the daemon's own refusal of a caller -- and answers 503
// instead, with a body of its own (the original carries the very words Claude
// Code matches) and no header of the original's. Claude Code retries a 503
// like any server error, and each retry is a fresh selection; a 401 on a
// model turn has already marked the credential that gave it.
//
// The 503 is not held (PATCHES §58). The 401s that reach a client are the
// ones the handler's own failover could not get past, and the ones that repeat
// -- a peer that refuses this daemon's token, the pinned identity's
// credential, a session lease -- would hold every request for the whole
// deadline, and Claude Code would start a new hold with each retry of the
// answer.
//
// Claude Code honours `x-claude-code-route-final-answer` only under a route
// registered in-process (runner mode), so that header cannot stop the
// recovery in a socket session; a 503 can.
//
// The canary: a socket client whose Authorization is anything but the
// placeholder has, almost certainly, adopted a stored login already. Each
// such request is counted (health: claude_socket_guard), and the first one
// per session is a WARN that names the credential's kind, never its value.
// The daemon's own surface (/_subrouter/...) is not counted: Claude Code never
// calls it, and a tool reading it over the socket may send a daemon token.

// ClaudeSocketPlaceholderToken is the bearer Claude Code sends over its Unix
// socket transport. It is Claude Code's own constant, not a credential: the
// daemon replaces Authorization with the pooled account's.
const ClaudeSocketPlaceholderToken = "ssh-placeholder"

// claudeSocketRefusedReason is X-Subrouter-Reason on the guard's 503.
const claudeSocketRefusedReason = "claude_socket_refused"

// claudeSocketCanarySessionsMax bounds the sessions the canary remembers
// having warned about. Past it the memory starts over, so a session still
// running may be warned about once more.
const claudeSocketCanarySessionsMax = 4096

type claudeSocketConnKey struct{}

// ClaudeSocketConnContext marks a connection that came through the Claude
// socket.
func ClaudeSocketConnContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, claudeSocketConnKey{}, true)
}

func claudeSocketRequest(r *http.Request) bool {
	on, _ := r.Context().Value(claudeSocketConnKey{}).(bool)
	return on
}

// ClaudeSocketGuard is the guard's state, shared by the private listener's
// handler and its health. A configured Claude socket builds one
// (cmd/subrouter newClaudeSocket); without a socket there is none.
type ClaudeSocketGuard struct {
	logger            *slog.Logger
	canaryRequests    atomic.Uint64
	canarySessions    atomic.Uint64
	refusalsRewritten atomic.Uint64

	mu     sync.Mutex
	warned map[string]struct{}
}

// ClaudeSocketGuardStatus is claude_socket_guard in the private listener's
// health. Both canary counts are zero on a healthy machine.
type ClaudeSocketGuardStatus struct {
	// AdoptionCanaryRequests counts socket requests whose Authorization was not
	// the placeholder.
	AdoptionCanaryRequests uint64 `json:"adoption_canary_requests"`
	// AdoptionCanarySessions counts the sessions warned about.
	AdoptionCanarySessions uint64 `json:"adoption_canary_sessions"`
	// RefusalsAnswered503 counts the 401s and revoked-token 403s answered 503.
	RefusalsAnswered503 uint64 `json:"refusals_answered_503"`
}

// NewClaudeSocketGuard returns a guard that logs to logger, or, when nil, to
// slog.Default as it is when a line is logged.
func NewClaudeSocketGuard(logger *slog.Logger) *ClaudeSocketGuard {
	return &ClaudeSocketGuard{logger: logger, warned: map[string]struct{}{}}
}

func (g *ClaudeSocketGuard) log() *slog.Logger {
	if g.logger != nil {
		return g.logger
	}
	return slog.Default()
}

// Status reads the counters.
func (g *ClaudeSocketGuard) Status() ClaudeSocketGuardStatus {
	return ClaudeSocketGuardStatus{
		AdoptionCanaryRequests: g.canaryRequests.Load(),
		AdoptionCanarySessions: g.canarySessions.Load(),
		RefusalsAnswered503:    g.refusalsRewritten.Load(),
	}
}

// Handler wraps the private listener's handler (nil is net/http's
// DefaultServeMux). A request from any other connection, or any request when g
// is nil, reaches next untouched.
func (g *ClaudeSocketGuard) Handler(next http.Handler) http.Handler {
	if next == nil {
		next = http.DefaultServeMux
	}
	if g == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !claudeSocketRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		g.observeCredential(r)
		guarded := &claudeSocketWriter{client: w, guard: g, request: r}
		next.ServeHTTP(guarded, r)
		guarded.finish()
	})
}

func (g *ClaudeSocketGuard) observeCredential(r *http.Request) {
	if r.URL.Path == "/_subrouter" || strings.HasPrefix(r.URL.Path, "/_subrouter/") {
		return
	}
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if value == "" || bearerToken(value) == ClaudeSocketPlaceholderToken {
		return
	}
	g.canaryRequests.Add(1)
	sessionID := session.ExtractRoutingID(r)
	if !g.firstWarning(sessionID) {
		return
	}
	g.canarySessions.Add(1)
	g.log().Warn("claude socket client sent a credential that is not the socket placeholder; Claude Code may have adopted a stored claude.ai login",
		"session", sessionID, "method", r.Method, "path", r.URL.Path,
		"credential", claudeSocketCredentialKind(value),
		"user_agent", agentclaude.BoundedToken(r.UserAgent()))
}

func (g *ClaudeSocketGuard) firstWarning(sessionID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, seen := g.warned[sessionID]; seen {
		return false
	}
	if len(g.warned) >= claudeSocketCanarySessionsMax {
		clear(g.warned)
	}
	g.warned[sessionID] = struct{}{}
	return true
}

func (g *ClaudeSocketGuard) noteRefusal(r *http.Request, status int, reason string) {
	g.refusalsRewritten.Add(1)
	g.log().Warn("claude socket answered 503 in place of a refusal Claude Code would answer with the human's own login",
		"status", status, "reason", reason, "session", session.ExtractRoutingID(r),
		"method", r.Method, "path", r.URL.Path)
}

// claudeSocketCredentialKind names what an Authorization value is, in fixed
// words, so the canary's log line can tell an adopted login from a stray
// daemon token without carrying any of the value.
func claudeSocketCredentialKind(value string) string {
	token := bearerToken(value)
	switch {
	case token == "":
		return "not_bearer"
	case strings.HasPrefix(token, "sk-ant-oat"):
		return "claude_oauth_access_token"
	case strings.HasPrefix(token, "sk-ant-api"):
		return "anthropic_api_key"
	case strings.HasPrefix(token, "sk-ant-"):
		return "anthropic_other"
	default:
		return "other"
	}
}

// claudeSocketRefusalBody is the guard's 503 body: Anthropic's error shape,
// api_error (a server error to Claude Code), in words none of Claude Code's
// authentication matchers look for.
func claudeSocketRefusalBody(status int) []byte {
	message := fmt.Sprintf("subrouter: the daemon behind Claude Code's socket refused this request (HTTP %d). "+
		"The socket answers 503 instead, so that Claude Code keeps its socket credential and does not switch "+
		"to the claude.ai login stored on this machine. The daemon log names the refusal.", status)
	body, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": "api_error", "message": message},
	})
	return body
}

// claudeSocketRevokedAnswer reports whether a 403 body (encoded as the header
// says) names a revoked token, the 403 Claude Code recovers from with the
// stored login.
func claudeSocketRevokedAnswer(body []byte, header http.Header) bool {
	decoded := decodedUpstreamBody(body, header)
	return bytes.Contains(bytes.ToLower(decoded), []byte("has been revoked"))
}

type claudeSocketWriterState int

const (
	claudeSocketPending    claudeSocketWriterState = iota // nothing decided yet
	claudeSocketPassing                                   // the client has the handler's own status
	claudeSocketInspecting                                // a 403, kept back until its body is read
	claudeSocketAnswered                                  // the client has the guard's 503
)

// claudeSocketWriter stands between the private handler and a socket client.
// It passes every response through except a 401, which it answers 503 at
// once, and a 403, which it keeps (at most claudeErrorBodyMax of body) until
// the handler returns: one that names a revoked token is answered 503, any
// other goes out unchanged.
type claudeSocketWriter struct {
	client  http.ResponseWriter
	guard   *ClaudeSocketGuard
	request *http.Request
	state   claudeSocketWriterState
	body    bytes.Buffer
	discard http.Header
}

func (w *claudeSocketWriter) Header() http.Header {
	if w.state == claudeSocketAnswered {
		// Nothing the handler sets after the guard answered may reach the
		// client, a trailer included.
		if w.discard == nil {
			w.discard = make(http.Header)
		}
		return w.discard
	}
	return w.client.Header()
}

func (w *claudeSocketWriter) WriteHeader(code int) {
	switch w.state {
	case claudeSocketPassing:
		w.client.WriteHeader(code)
		return
	case claudeSocketInspecting, claudeSocketAnswered:
		return
	}
	switch {
	case code >= 100 && code < 200 && code != http.StatusSwitchingProtocols:
		// Informational: the final status is still to come.
		w.client.WriteHeader(code)
	case code == http.StatusUnauthorized:
		w.answer(code)
	case code == http.StatusForbidden:
		w.state = claudeSocketInspecting
	default:
		w.state = claudeSocketPassing
		w.client.WriteHeader(code)
	}
}

func (w *claudeSocketWriter) Write(p []byte) (int, error) {
	switch w.state {
	case claudeSocketPending:
		w.WriteHeader(http.StatusOK)
		return w.Write(p)
	case claudeSocketAnswered:
		return len(p), nil
	case claudeSocketInspecting:
		if w.body.Len()+len(p) <= claudeErrorBodyMax {
			return w.body.Write(p)
		}
		// Longer than any error answer: it goes out as it is.
		if err := w.release(); err != nil {
			return 0, err
		}
	}
	return w.client.Write(p)
}

// release writes a kept 403 to the client unchanged.
func (w *claudeSocketWriter) release() error {
	w.state = claudeSocketPassing
	w.client.WriteHeader(http.StatusForbidden)
	if w.body.Len() == 0 {
		return nil
	}
	_, err := w.client.Write(w.body.Bytes())
	w.body.Reset()
	return err
}

// finish decides a 403 kept to the end. It runs only when the handler
// returned: a handler that panicked has written nothing, and net/http drops
// the connection.
func (w *claudeSocketWriter) finish() {
	if w.state != claudeSocketInspecting {
		return
	}
	if claudeSocketRevokedAnswer(w.body.Bytes(), w.client.Header()) {
		w.answer(http.StatusForbidden)
		return
	}
	_ = w.release()
}

// answer writes the guard's 503 in place of a refusal with status, dropping
// every header the handler set for it.
func (w *claudeSocketWriter) answer(status int) {
	header := w.client.Header()
	reason := header.Get(subrouterReasonHeader)
	clear(header)
	body := claudeSocketRefusalBody(status)
	header.Set("Content-Type", "application/json")
	header.Set("Content-Length", strconv.Itoa(len(body)))
	header.Set(subrouterReasonHeader, claudeSocketRefusedReason)
	w.state = claudeSocketAnswered
	w.body.Reset()
	w.client.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.client.Write(body)
	w.guard.noteRefusal(w.request, status, reason)
}

func (w *claudeSocketWriter) Flush() {
	_ = w.FlushError()
}

func (w *claudeSocketWriter) FlushError() error {
	switch w.state {
	case claudeSocketInspecting, claudeSocketAnswered:
		return nil
	case claudeSocketPending:
		w.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(w.client).Flush()
}

// Unwrap lets http.ResponseController reach the connection's deadlines.
// Flush and Hijack never take this path: the writer answers both itself.
func (w *claudeSocketWriter) Unwrap() http.ResponseWriter {
	return w.client
}

func (w *claudeSocketWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.client).Hijack()
}
