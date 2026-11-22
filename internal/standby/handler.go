package standby

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"path"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/session"
)

// retryAfterSeconds is the Retry-After of a 503 that names none, the stand-in's
// own among them, and what a Retry-After above retryAfterMax is lowered to.
// Without the client's retry watchdog, Claude Code fails a request at once
// when a 503 asks for more than 60 s; with it, the session sleeps the whole
// value (PATCHES §79.1).
const (
	retryAfterSeconds = 30
	retryAfterMax     = 60
)

// route is what the stand-in does with one request, decided by routeFor
// alone.
type route int

const (
	routeBox           route = iota // sent to a box
	routeHealth                     // the stand-in's own health body
	routeDaemonSurface              // the daemon's other /_subrouter/ routes: 503
	routeControlPlane               // /api/*, the client's account and policy calls: 503
	routePinned                     // pinned to one account: 503, never sent
)

// routeFor is the stand-in's whole policy for what does not go to a box.
//
// /api/* is the client's control plane. A box answers it with its own
// identity account, the client caches whichever identity answered, and a
// wrong one has hidden tools and models on this Mac before (control_plane.go).
// A 503 keeps the box's identity out of every file on the Mac.
//
// A request that names an account (claude <account>) was promised that
// account and no other, so it waits for the daemon.
func routeFor(r *http.Request) route {
	clean := path.Clean("/" + r.URL.Path)
	switch {
	case clean == "/_subrouter/health":
		return routeHealth
	case clean == "/_subrouter" || strings.HasPrefix(clean, "/_subrouter/"):
		return routeDaemonSurface
	case clean == "/api" || strings.HasPrefix(clean, "/api/"):
		return routeControlPlane
	}
	if _, pinned, _ := session.ExtractAccountIDWithPresence(r); pinned {
		return routePinned
	}
	return routeBox
}

// Handler answers a Claude Code request, as it arrives on the Mac's Claude
// socket, from a box.
type Handler struct {
	boxes   Boxes
	logger  *slog.Logger
	guarded http.Handler
}

// NewHandler returns the box handler over boxes, logging to logger (nil:
// slog.Default).
func NewHandler(boxes Boxes, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	h := &Handler{boxes: boxes, logger: logger}
	reverse := &httputil.ReverseProxy{
		Rewrite:       rewriteForBox,
		Transport:     roundTripperFunc(h.send),
		FlushInterval: -1,
		ErrorHandler:  h.sendFailed,
		ErrorLog:      slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	// The daemon's guard: a 401, or a 403 that says the token was revoked,
	// would make Claude Code drop its socket credential for the human's own
	// login. It answers 503 instead.
	guard := proxy.NewClaudeSocketGuard(logger)
	h.guarded = guard.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch routeFor(r) {
		case routeHealth:
			writeHealth(w)
		case routeDaemonSurface:
			writeUnavailable(w, "standby_daemon_surface",
				"subrouter standby: the local daemon is down, and the stand-in serves only /_subrouter/health.")
		case routeControlPlane:
			writeUnavailable(w, "standby_control_plane",
				"subrouter standby: account and policy calls are answered only by the local daemon, which is down. Retry shortly.")
		case routePinned:
			writeUnavailable(w, "standby_pinned_account",
				"subrouter standby: this session is pinned to one account, and the stand-in never sends a pinned session to a box. Retry when the local daemon is back.")
		default:
			reverse.ServeHTTP(w, r)
		}
	}))
	return h
}

// ServeHTTP serves one request. Every request it sees came through the Claude
// socket, so the guard is always on.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(proxy.ClaudeSocketConnContext(r.Context()))
	h.guarded.ServeHTTP(&retryAfterWriter{ResponseWriter: w}, r)
}

// rewriteForBox sets the headers a box reads, the rules of the peer forward
// (forwardToPeer) without a hop count or a target account: the box treats the
// request as one from a session of its own. The box and its token are set per
// attempt, in send.
func rewriteForBox(pr *httputil.ProxyRequest) {
	header := pr.Out.Header
	sessionID := session.ExtractRoutingID(pr.In)
	session.StripSubrouterHeaders(header)
	for name := range header {
		if strings.HasPrefix(http.CanonicalHeaderKey(name), "X-Subrouter-") {
			delete(header, name)
		}
	}
	header.Del("Authorization")
	header.Del("X-Api-Key")
	header.Del("X-Pool-Hop")
	// Without it the box assumes Codex.
	header.Set("X-Subrouter-Agent", "claude")
	if sessionID != "" {
		header.Set("X-Subrouter-Session", sessionID)
	}
}

var errNoBox = errors.New("no box is usable")

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// errSentThenFailed is a round trip that failed after the box may have read
// the request: it is never sent again.
type errSentThenFailed struct{ err error }

func (e errSentThenFailed) Error() string {
	return "the box failed after the request was sent: " + e.err.Error()
}
func (e errSentThenFailed) Unwrap() error { return e.err }

// send tries the session's boxes in order. It moves to the next box only
// while no request head has been written to any: the body is streamed, not
// kept, so a request a box may have read can never be sent again.
func (h *Handler) send(out *http.Request) (*http.Response, error) {
	sessionID := session.ExtractRoutingID(out)
	boxes := h.boxes.Candidates(sessionID)
	if len(boxes) == 0 {
		return nil, errNoBox
	}
	body := &unsentBody{}
	if out.Body != nil && out.Body != http.NoBody {
		body.body = out.Body
	}
	trace := &httptrace.ClientTrace{WroteHeaders: func() { body.sent.Store(true) }}
	ctx := httptrace.WithClientTrace(out.Context(), trace)
	var last error
	for _, box := range boxes {
		attempt := out.Clone(ctx)
		if body.body != nil {
			attempt.Body = body
		}
		attempt.URL.Scheme = box.base.Scheme
		attempt.URL.Host = box.base.Host
		attempt.Host = box.base.Host
		attempt.Header.Set("Authorization", "Bearer "+box.token)
		response, err := box.transport.RoundTrip(attempt)
		if err == nil {
			h.logger.Info("standby: a box answered", "box", box.name, "session", sessionID,
				"method", out.Method, "path", out.URL.Path, "status", response.StatusCode)
			return response, nil
		}
		if out.Context().Err() != nil {
			return nil, err
		}
		if body.sent.Load() {
			h.logger.Warn("standby: a box failed after the request was sent; it is not sent again",
				"box", box.name, "session", sessionID, "method", out.Method, "path", out.URL.Path, "error", err)
			return nil, errSentThenFailed{err}
		}
		h.logger.Warn("standby: a box could not be reached; trying the next",
			"box", box.name, "session", sessionID, "error", err)
		last = err
	}
	return nil, fmt.Errorf("every box failed before the request was sent: %w", last)
}

// unsentBody is the client's request body as each attempt sees it. The
// transport closes a body when its round trip fails; until a request head has
// been written that close is ignored, so the next box can still read the body
// from its first byte.
type unsentBody struct {
	body io.ReadCloser
	sent atomic.Bool
}

func (b *unsentBody) Read(p []byte) (int, error) { return b.body.Read(p) }

func (b *unsentBody) Close() error {
	if !b.sent.Load() {
		return nil
	}
	return b.body.Close()
}

// sendFailed answers a request no box answered.
func (h *Handler) sendFailed(w http.ResponseWriter, r *http.Request, err error) {
	var sentThenFailed errSentThenFailed
	switch {
	case r.Context().Err() != nil:
		// The client left; nobody reads this.
		writeUnavailable(w, "standby_client_gone", "subrouter standby: the request was cancelled.")
	case errors.Is(err, errNoBox):
		h.logger.Warn("standby: no box is usable", "session", session.ExtractRoutingID(r), "path", r.URL.Path)
		writeUnavailable(w, "standby_no_box",
			"subrouter standby: the local daemon is down and no box can take this request now. Retry shortly.")
	case errors.As(err, &sentThenFailed):
		writeUnavailable(w, "standby_box_failed",
			"subrouter standby: the box failed this request after it was sent, so it was not sent again. Retry.")
	default:
		writeUnavailable(w, "standby_box_unreachable",
			"subrouter standby: the local daemon is down and no box could be reached. Retry shortly.")
	}
}

// writeUnavailable is every 503 the stand-in makes: Anthropic's error shape,
// api_error, which Claude Code retries like any server error. retryAfterWriter
// gives it its Retry-After.
func writeUnavailable(w http.ResponseWriter, reason, message string) {
	body, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": "api_error", "message": message},
	})
	header := w.Header()
	header.Set("Content-Type", "application/json")
	header.Set("Content-Length", strconv.Itoa(len(body)))
	header.Set("X-Subrouter-Reason", reason)
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write(body)
}

// healthBody is the stand-in's /_subrouter/health. claude-route reads only
// "ok": true, so a new launch still takes the pool route; claude_socket tells
// a deploy check that the daemon does not own the socket.
var healthBody = []byte(`{"ok":true,"claude_socket":"standby"}` + "\n")

func writeHealth(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(healthBody)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(healthBody)
}

// retryAfterWriter keeps every Retry-After the client sees at 60 s or less. A
// longer one, or one that is not a number of seconds, becomes 30; a 503
// without one gets 30, so the guard's 503 carries one too.
type retryAfterWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *retryAfterWriter) WriteHeader(code int) {
	if !w.wroteHeader && code >= http.StatusOK {
		w.wroteHeader = true
		fitRetryAfter(w.Header(), code)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *retryAfterWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *retryAfterWriter) FlushError() error {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

// Unwrap lets http.ResponseController reach the connection.
func (w *retryAfterWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func fitRetryAfter(header http.Header, status int) {
	values := header.Values("Retry-After")
	if len(values) == 0 {
		if status == http.StatusServiceUnavailable {
			header.Set("Retry-After", strconv.Itoa(retryAfterSeconds))
		}
		return
	}
	seconds, err := strconv.Atoi(strings.TrimSpace(values[0]))
	if err != nil || seconds < 0 || seconds > retryAfterMax {
		seconds = retryAfterSeconds
	}
	header.Set("Retry-After", strconv.Itoa(seconds))
}
