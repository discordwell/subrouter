package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// opusMovePoolServer is opusPoolServer with the session pinned to
// one@example.com, whose Fable and Opus pools are both spent, so any Opus pick
// is two@example.com: a move, not a stay.
func opusMovePoolServer(t *testing.T, sessionID string) (Server, *session.Store) {
	t.Helper()
	server, store, _ := opusPoolServer(t, 0, 1.0)
	scores := opusPoolScores(0, 1.0, "one@example.com", "two@example.com")
	scores[0].ModelScores[selectacct.ModelKey(agentclaude.OpusFeature)] = selectacct.Score{
		AccountID: "one@example.com", Provider: accounts.ProviderClaude,
	}
	server.SchedulerRef.Set(selectacct.NewScheduler(scores))
	if _, err := store.Put("claude", sessionID, "one@example.com", ""); err != nil {
		t.Fatal(err)
	}
	return server, store
}

func pinnedAccount(t *testing.T, store *session.Store, sessionID string) string {
	t.Helper()
	assignment, ok := store.Get("claude", sessionID)
	if !ok {
		t.Fatalf("session %q has no pin", sessionID)
	}
	return assignment.AccountID
}

// An Opus account that refuses the substitution never served the turn, so the
// session stays where it was. The stage used to pin the session to its pick
// before sending, and a decline left it there.
func TestClaudeOpusSubstituteDeclineLeavesThePinWhereItWas(t *testing.T) {
	server, store := opusMovePoolServer(t, "session-opus-decline")
	server.Transport = &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		if got := req.Header.Get("Authorization"); got != "Bearer tok-two" {
			t.Errorf("Authorization = %q, want the Opus pick two@example.com", got)
		}
		return opusRejected429()
	}}
	req := opusSubstituteRequest(t, `{"model":"claude-fable-5","max_tokens":8,"messages":[]}`)
	if _, ok := server.claudeOpusSubstituteResponse(req, "claude", "session-opus-decline", "", "one@example.com", "no_alternate_account"); ok {
		t.Fatal("a 429 from the Opus account must decline the substitution")
	}
	if got := pinnedAccount(t, store, "session-opus-decline"); got != "one@example.com" {
		t.Fatalf("pin = %q after a declined Opus substitution, want one@example.com (unchanged)", got)
	}
}

// A successful substitution moves the session (the Fable window is spent, a
// forced switch), but only once the answer has completed: before the stream's
// terminal event the pin is where the turn found it.
func TestClaudeOpusSubstituteCommitsOnlyAfterTheAnswerCompletes(t *testing.T) {
	server, store := opusMovePoolServer(t, "session-opus-commit")
	server.Transport = &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		header := http.Header{}
		header.Set("Content-Type", "text/event-stream")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     header,
			Body: io.NopCloser(strings.NewReader(
				"event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
					"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")),
		}
	}}
	req := opusSubstituteRequest(t, `{"model":"claude-fable-5","max_tokens":8,"messages":[]}`)
	resp, ok := server.claudeOpusSubstituteResponse(req, "claude", "session-opus-commit", "", "one@example.com", "no_alternate_account")
	if !ok {
		t.Fatal("the Opus pool should serve the request")
	}
	if got := pinnedAccount(t, store, "session-opus-commit"); got != "one@example.com" {
		_ = resp.Body.Close()
		t.Fatalf("pin = %q before the Opus answer was read, want one@example.com (nothing committed yet)", got)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := pinnedAccount(t, store, "session-opus-commit"); got != "two@example.com" {
		t.Fatalf("pin = %q after the Opus answer completed, want two@example.com", got)
	}
}

// The handler's own Opus stage (the Fable pool could not even start the
// request) re-runs selection on the Opus pool. That selection used to write the
// move at once; an Opus account that then refused left the session on it.
func TestClaudeFableHandlerOpusSelectionDoesNotMoveThePinBeforeAnAnswer(t *testing.T) {
	rejected := http.Header{}
	rejected.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	upstream := newOpusHandlerUpstream(t, http.StatusTooManyRequests, rejected)
	server, store := opusMovePoolServer(t, "session-opus-select")
	upstreamURL, err := url.Parse(upstream.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	server.ClaudeUpstream = upstreamURL

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, opusHandlerRequest(t, "session-opus-select", `{"model":"claude-fable-5","max_tokens":8,"messages":[]}`))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the Opus 429 surfaced; body=%s", recorder.Code, recorder.Body.String())
	}
	if len(upstream.models) == 0 || upstream.models[0] != "claude-opus-5" {
		t.Fatalf("upstream models = %v, want the Opus substitution sent", upstream.models)
	}
	if got := pinnedAccount(t, store, "session-opus-select"); got != "one@example.com" {
		t.Fatalf("pin = %q after the Opus account refused, want one@example.com (unchanged)", got)
	}
}

// The same stage commits once the Opus account answers 2xx.
func TestClaudeFableHandlerOpusSelectionCommitsOnSuccess(t *testing.T) {
	upstream := newOpusHandlerUpstream(t, http.StatusOK, nil)
	server, store := opusMovePoolServer(t, "session-opus-select-ok")
	upstreamURL, err := url.Parse(upstream.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	server.ClaudeUpstream = upstreamURL

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, opusHandlerRequest(t, "session-opus-select-ok", `{"model":"claude-fable-5","max_tokens":8,"messages":[]}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 via the Opus pool; body=%s", recorder.Code, recorder.Body.String())
	}
	if got := pinnedAccount(t, store, "session-opus-select-ok"); got != "two@example.com" {
		t.Fatalf("pin = %q after the Opus account answered, want two@example.com", got)
	}
}
