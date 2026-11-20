package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// PATCHES §39: a request that failed because its client hung up first is Warn,
// not Error. On the laptop ~80 Error lines a day were Codex abandoning
// /api/hello and /wham probes ("context canceled"), which buried the Error
// lines that do need a look.

// codexProbePath is a Codex probe: GET /api/hello reads as Claude Code's
// connectivity check since upstream #601 (session.ExtractAgentType), so the
// Codex /wham usage probe stands in for it here.
const codexProbePath = "/backend-api/wham/usage"

func requestFailureTestServer(t *testing.T, upstream *httptest.Server, logs *syncBuffer) http.Handler {
	t.Helper()
	codexUpstream, err := url.Parse(upstream.URL + "/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	return Server{
		CodexUpstream: codexUpstream,
		Accounts: []accounts.Account{{
			ID:       "codex-account",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "oauth-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1 << 20,
		Logger:       slog.New(slog.NewJSONHandler(logs, nil)),
	}.Handler()
}

func requestFailedLines(logs *syncBuffer) []map[string]any {
	var out []map[string]any
	for _, raw := range strings.Split(logs.String(), "\n") {
		var line map[string]any
		if json.Unmarshal([]byte(raw), &line) == nil && line["msg"] == "proxy request failed" {
			out = append(out, line)
		}
	}
	return out
}

func TestProxyRequestFailedIsWarnWhenTheClientHungUp(t *testing.T) {
	arrived := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case arrived <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer upstream.Close()
	logs := &syncBuffer{}
	handler := requestFailureTestServer(t, upstream, logs)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, codexProbePath, nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}()
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never reached the upstream")
	}
	cancel()
	<-done

	lines := requestFailedLines(logs)
	if len(lines) != 1 || lines[0]["level"] != "WARN" || lines[0]["error"] != "context canceled" {
		t.Fatalf("proxy request failed lines %v; want one WARN (the client hung up)\nlogs:\n%s", lines, logs.String())
	}
}

func TestProxyRequestFailedStaysErrorWhenTheUpstreamFailed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	defer upstream.Close()
	logs := &syncBuffer{}
	handler := requestFailureTestServer(t, upstream, logs)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, codexProbePath, nil))
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("calibration: status %d, want 502 from the ErrorHandler", recorder.Code)
	}
	lines := requestFailedLines(logs)
	if len(lines) != 1 || lines[0]["level"] != "ERROR" {
		t.Fatalf("proxy request failed lines %v; want one ERROR (the upstream hung up on a live client)\nlogs:\n%s", lines, logs.String())
	}
}

// The replayable POST transport's last-attempt line follows the same rule: a
// client that hung up is Warn; a dial that failed under a live client stays
// Error (TestExhaustedRequestLogsTraceAttributes pins the rest of that line).
func TestReplayableExhaustedIsWarnWhenTheClientHungUp(t *testing.T) {
	for _, tc := range []struct {
		name      string
		canceled  bool
		wantLevel string
	}{
		{"client hung up", true, "level=WARN"},
		{"upstream unreachable", false, "level=ERROR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			transport := replayablePostRetryTransport{
				base:        &http.Transport{},
				logger:      slog.New(slog.NewTextHandler(&logs, nil)),
				method:      http.MethodPost,
				path:        "/responses",
				maxAttempts: 1,
			}
			listener := httptest.NewServer(http.NotFoundHandler())
			addr := listener.URL
			listener.Close() // nothing is listening, so a live request's dial fails
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, addr, strings.NewReader("body"))
			if err != nil {
				t.Fatal(err)
			}
			if response, roundTripErr := transport.RoundTrip(request); roundTripErr == nil {
				response.Body.Close()
				t.Fatal("request unexpectedly succeeded")
			}
			got := logs.String()
			if !strings.Contains(got, "replayable upstream request exhausted") || !strings.Contains(got, tc.wantLevel) {
				t.Fatalf("want an exhaustion line at %s\nlogs:\n%s", tc.wantLevel, got)
			}
		})
	}
}
