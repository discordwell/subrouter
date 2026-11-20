package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func TestClaudeOpusModelPreservesVersionAndSuffix(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"claude-fable-5", "claude-opus-5", true},
		// The client appends the context-window suffix; dropping it silently
		// downgrades a 1M-context session to the 200k default.
		{"claude-fable-5[1m]", "claude-opus-5[1m]", true},
		{"CLAUDE-FABLE-5[1m]", "CLAUDE-opus-5[1m]", true},
		// Opus shipped no counterpart for every Fable point release: Fable 5.1
		// launched against Opus 5, and claude-opus-5-1 does not exist. Splicing
		// the version through verbatim names a model Anthropic 404s, which costs
		// the substitution the whole rescue it exists to perform.
		{"claude-fable-5-1", "claude-opus-5", true},
		{"claude-fable-5-1[1m]", "claude-opus-5[1m]", true},
		{"claude-opus-5", "", false},
		{"", "", false},
		{"claude-sonnet-5", "", false},
	}
	for _, test := range cases {
		got, ok := claudeOpusModel(test.in)
		if ok != test.ok || got != test.want {
			t.Fatalf("claudeOpusModel(%q) = (%q, %v), want (%q, %v)", test.in, got, ok, test.want, test.ok)
		}
	}
}

func TestClaudeOpusBodyRewritesOnlyTheModel(t *testing.T) {
	in := []byte(`{"model":"claude-fable-5[1m]","max_tokens":8,"context_management":{"edits":[]},"messages":[{"role":"user","content":"hi"}]}`)
	rebuilt, from, to, ok := claudeOpusBody(in)
	if !ok {
		t.Fatal("fable body should be rewritable")
	}
	if from != "claude-fable-5[1m]" || to != "claude-opus-5[1m]" {
		t.Fatalf("from/to = %q/%q", from, to)
	}
	var got map[string]any
	if err := json.Unmarshal(rebuilt, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "claude-opus-5[1m]" {
		t.Fatalf("model = %v", got["model"])
	}
	if got["max_tokens"] != float64(8) {
		t.Fatalf("max_tokens = %v, want 8 preserved", got["max_tokens"])
	}
	// The OAuth pool accepts context_management; only the direct API-key path
	// has to strip it. Stripping it here would silently drop context edits.
	if _, present := got["context_management"]; !present {
		t.Fatalf("context_management dropped: %s", rebuilt)
	}
	var want map[string]any
	if err := json.Unmarshal(in, &want); err != nil {
		t.Fatal(err)
	}
	wantMessages, _ := json.Marshal(want["messages"])
	gotMessages, _ := json.Marshal(got["messages"])
	if string(gotMessages) != string(wantMessages) {
		t.Fatalf("messages = %s, want %s", gotMessages, wantMessages)
	}
}

func TestClaudeOpusBodyFailsClosed(t *testing.T) {
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := encoder.EncodeAll([]byte(`{"model":"claude-fable-5"}`), nil)
	encoder.Close()

	cases := map[string][]byte{
		"not json":            []byte("not json"),
		"no top-level model":  []byte(`{"max_tokens":8}`),
		"non-string model":    []byte(`{"model":123}`),
		"already opus":        []byte(`{"model":"claude-opus-5"}`),
		"model inside a turn": []byte(`{"messages":[{"role":"user","content":"{\"model\":\"claude-fable-5\"}"}]}`),
		"compressed":          compressed,
	}
	for name, body := range cases {
		rebuilt, _, _, ok := claudeOpusBody(body)
		if ok {
			t.Fatalf("%s: expected refusal, got %s", name, rebuilt)
		}
		if rebuilt != nil {
			t.Fatalf("%s: refusal must return nil bytes, got %s", name, rebuilt)
		}
	}
}

func TestRewriteRequestToClaudeOpusSetsAllThreeBodyFields(t *testing.T) {
	body := `{"model":"claude-fable-5[1m]","max_tokens":8,"messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	from, to, _, ok := Server{}.rewriteRequestToClaudeOpus(req)
	if !ok {
		t.Fatal("fable request should be rewritable")
	}
	if from != "claude-fable-5[1m]" || to != "claude-opus-5[1m]" {
		t.Fatalf("from/to = %q/%q", from, to)
	}
	if got := req.Header.Get("X-Subrouter-Model"); got != "claude-opus-5[1m]" {
		t.Fatalf("X-Subrouter-Model = %q; ExtractModel reads headers before the body", got)
	}
	fromBody, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fromBody), "claude-opus-5[1m]") || strings.Contains(string(fromBody), "fable") {
		t.Fatalf("Body = %s", fromBody)
	}
	if req.ContentLength != int64(len(fromBody)) {
		t.Fatalf("ContentLength = %d, want %d", req.ContentLength, len(fromBody))
	}
	// GetBody must carry the SAME bytes: makeRequestBodyReplayable short-circuits
	// on a non-nil GetBody, so a stale replay source sends fable on attempt 1 and
	// opus on every retry.
	rc, err := req.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	replay, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(replay) != string(fromBody) {
		t.Fatalf("GetBody = %s, want the rewritten body %s", replay, fromBody)
	}
}

func TestRewriteRequestToClaudeOpusRestoresBodyOnRefusal(t *testing.T) {
	body := `{"max_tokens":8}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if _, _, _, ok := (Server{}).rewriteRequestToClaudeOpus(req); ok {
		t.Fatal("a body with no top-level model must not be substituted")
	}
	if got := req.Header.Get("X-Subrouter-Model"); got != "" {
		t.Fatalf("X-Subrouter-Model = %q, want unset on refusal", got)
	}
	restored, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != body {
		t.Fatalf("restored body = %q, want %q", restored, body)
	}
}

// The undo the handler runs when its Opus selection fails (PATCHES §50): the
// turn goes on to the peer chain, so everything the rewrite changed is put back
// as the client sent it -- the bytes (the rewrite re-encodes the whole body, so
// the key order moves too), ContentLength, GetBody, and X-Subrouter-Model,
// whether or not the client set it. GetBody is checked both ways: absent, as a
// bare client request has it, and set, as serveClaudeFableFallback leaves it
// whenever a Fable fallback is configured and declined before this stage.
func TestRewriteRequestToClaudeOpusUndoRestoresTheClientRequest(t *testing.T) {
	const body = `{"model":"claude-fable-5","max_tokens":8,"messages":[]}`
	for _, tc := range []struct {
		clientModelHeader string
		clientGetBody     bool
	}{{"", false}, {"claude-fable-5", false}, {"", true}} {
		clientModelHeader := tc.clientModelHeader
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if clientModelHeader != "" {
			req.Header.Set("X-Subrouter-Model", clientModelHeader)
		}
		if tc.clientGetBody {
			req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }
		}
		priorGetBody := req.GetBody
		_, _, undo, ok := Server{}.rewriteRequestToClaudeOpus(req)
		if !ok || undo == nil {
			t.Fatalf("header %q: a fable request must be rewritten, with an undo", clientModelHeader)
		}
		if got := req.Header.Get("X-Subrouter-Model"); got != "claude-opus-5" {
			t.Fatalf("calibration, header %q: X-Subrouter-Model = %q after the rewrite", clientModelHeader, got)
		}
		undo()
		restored, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(restored) != body || req.ContentLength != int64(len(body)) {
			t.Fatalf("header %q: body %q (ContentLength %d) after undo, want the client's %q (%d)",
				clientModelHeader, restored, req.ContentLength, body, len(body))
		}
		if (req.GetBody == nil) != (priorGetBody == nil) {
			t.Fatalf("header %q: GetBody set=%v after undo, want set=%v as the client's request had it",
				clientModelHeader, req.GetBody != nil, priorGetBody != nil)
		}
		if req.GetBody != nil {
			replay, err := req.GetBody()
			if err != nil {
				t.Fatal(err)
			}
			if replayed, _ := io.ReadAll(replay); string(replayed) != body {
				t.Fatalf("GetBody() = %q after undo, want the client's %q", replayed, body)
			}
		}
		if values, present := req.Header["X-Subrouter-Model"]; clientModelHeader == "" && present {
			t.Fatalf("X-Subrouter-Model = %q after undo; the client never set it", values)
		} else if clientModelHeader != "" && (len(values) != 1 || values[0] != clientModelHeader) {
			t.Fatalf("X-Subrouter-Model = %q after undo, want the client's [%s]", values, clientModelHeader)
		}
	}
}

// opusPoolScores mirrors what scoreFromUsageWindows produces for a Claude
// subscription: account-wide windows with healthy headroom, plus a per-feature
// quota pool for fable and opus that can be cooked independently. That is the
// exact shape the substitution turns on - Anthropic meters Fable in its own
// weekly bucket, so "fable cooked, opus healthy" is the routine production state.
func opusPoolScores(fableHeadroom, opusHeadroom float64, accountIDs ...string) []selectacct.Score {
	scores := make([]selectacct.Score, 0, len(accountIDs))
	for _, id := range accountIDs {
		pool := func(headroom float64) selectacct.Score {
			return selectacct.Score{AccountID: id, Provider: accounts.ProviderClaude, Headroom: headroom, ShortHeadroom: headroom}
		}
		scores = append(scores, selectacct.Score{
			AccountID:     id,
			Provider:      accounts.ProviderClaude,
			Headroom:      1,
			ShortHeadroom: 1,
			ModelScores: map[string]selectacct.Score{
				selectacct.ModelKey(agentclaude.FableFeature): pool(fableHeadroom),
				selectacct.ModelKey(agentclaude.OpusFeature):  pool(opusHeadroom),
			},
		})
	}
	return scores
}

// opusPoolServer builds a two-account Claude subscription pool whose fable and
// opus quota headroom are set independently.
func opusPoolServer(t *testing.T, fableHeadroom, opusHeadroom float64) (Server, *session.Store, *bytes.Buffer) {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	logBuf := &bytes.Buffer{}
	server := Server{
		ClaudeUpstream: &url.URL{Scheme: "https", Host: "api.anthropic.com"},
		Accounts: []accounts.Account{
			{ID: "one@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-one"},
			{ID: "two@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-two"},
		},
		Sessions: store,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(
			opusPoolScores(fableHeadroom, opusHeadroom, "one@example.com", "two@example.com"))),
		MaxBodyBytes: 1 << 20,
		Logger:       slog.New(slog.NewTextHandler(logBuf, nil)),
	}
	return server, store, logBuf
}

func opusSubstituteRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	raw := []byte(body)
	req := httptest.NewRequest(http.MethodPost, "http://subrouter/v1/messages", bytes.NewReader(raw))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil }
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok-fable-cooked")
	return req
}

func TestClaudeOpusSubstituteResponseServesFromOpusPool(t *testing.T) {
	server, _, logBuf := opusPoolServer(t, 0, 1.0)
	var captured *http.Request
	var capturedBody string
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		captured = req
		b, _ := io.ReadAll(req.Body)
		capturedBody = string(b)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"id":"msg_1","type":"message"}`)),
			Header:     http.Header{},
		}
	}}
	server.Transport = stub

	req := opusSubstituteRequest(t, `{"model":"claude-fable-5[1m]","max_tokens":8,"messages":[]}`)
	req.Header.Set("X-Subrouter-Session", "session-opus")
	resp, ok := server.claudeOpusSubstituteResponse(req, "claude", "session-opus", "", "one@example.com", "no_alternate_account")
	if !ok {
		t.Fatalf("expected the opus pool to serve the request; logs=\n%s", logBuf.String())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(capturedBody, `"model":"claude-opus-5[1m]"`) {
		t.Fatalf("forwarded body = %s, want the opus model", capturedBody)
	}
	if auth := captured.Header.Get("Authorization"); auth != "Bearer tok-one" && auth != "Bearer tok-two" {
		t.Fatalf("Authorization = %q, want the opus-selected pool account's token", auth)
	}
	if got := captured.Header.Get("Anthropic-Beta"); !strings.Contains(got, claudeOAuthBetaHeader) {
		t.Fatalf("Anthropic-Beta = %q, want the OAuth beta", got)
	}
	if got := captured.Header.Get("X-Api-Key"); got != "" {
		t.Fatalf("X-Api-Key = %q, want empty on an OAuth route", got)
	}
	if got := captured.URL.String(); got != "https://api.anthropic.com/v1/messages" {
		t.Fatalf("upstream URL = %q", got)
	}
	for key := range captured.Header {
		if strings.HasPrefix(strings.ToLower(key), "x-subrouter-") {
			t.Fatalf("routing header %q leaked to Anthropic", key)
		}
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "substituting claude opus for exhausted fable pool") || !strings.Contains(logs, "stage=transport") {
		t.Fatalf("expected the loud substitution log; logs=\n%s", logs)
	}
}

func TestClaudeOpusSubstituteResponseDeclinesWhenOpusPoolExhausted(t *testing.T) {
	server, _, logBuf := opusPoolServer(t, 0, 0)
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		t.Error("no upstream request should be made without opus capacity")
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}
	}}
	server.Transport = stub

	req := opusSubstituteRequest(t, `{"model":"claude-fable-5","max_tokens":8,"messages":[]}`)
	if _, ok := server.claudeOpusSubstituteResponse(req, "claude", "session-opus", "", "one@example.com", "max_attempts"); ok {
		t.Fatal("no 5h/weekly headroom means no substitution")
	}
	if stub.calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", stub.calls)
	}
	if logs := logBuf.String(); !strings.Contains(logs, "opus pool has no capacity") {
		t.Fatalf("expected the no-capacity log; logs=\n%s", logs)
	}
}

func TestClaudeOpusSubstituteResponseDeclinesOnNon2xx(t *testing.T) {
	cases := map[string]func() *http.Response{
		"429": func() *http.Response {
			return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}
		},
		// A stale/wrong opus model id must degrade to today's behavior rather
		// than leak a 404 to the client.
		"404 wrong model id": func() *http.Response {
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"not_found_error"}}`)), Header: http.Header{}}
		},
		"200 rejected": func() *http.Response {
			h := http.Header{}
			h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Header: h}
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			server, _, logBuf := opusPoolServer(t, 0, 1.0)
			server.Transport = &stubRoundTripper{responses: func(req *http.Request) *http.Response { return build() }}
			req := opusSubstituteRequest(t, `{"model":"claude-fable-5","max_tokens":8,"messages":[]}`)
			if _, ok := server.claudeOpusSubstituteResponse(req, "claude", "session-opus", "", "one@example.com", "no_alternate_account"); ok {
				t.Fatal("only a clean 2xx may replace the pool's own error")
			}
			if logs := logBuf.String(); !strings.Contains(logs, "declined by upstream") {
				t.Fatalf("expected the decline log; logs=\n%s", logs)
			}
		})
	}
}

// The exhaustion marks Fable failover just wrote are pool-scoped, so they must
// not follow the request into the Opus pick.
func TestClaudeOpusSubstituteResponseUsesFreshTriedSet(t *testing.T) {
	server, _, logBuf := opusPoolServer(t, 1.0, 1.0)
	until := time.Now().Add(time.Hour)
	for _, id := range []string{"one@example.com", "two@example.com"} {
		server.SchedulerRef.MarkExhaustedUntil(accounts.ProviderClaude, id, selectacct.ModelKey(agentclaude.FableFeature), until)
	}
	server.Transport = &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":"msg_1"}`)), Header: http.Header{}}
	}}
	req := opusSubstituteRequest(t, `{"model":"claude-fable-5","max_tokens":8,"messages":[]}`)
	resp, ok := server.claudeOpusSubstituteResponse(req, "claude", "session-opus", "", "one@example.com", "no_alternate_account")
	if !ok {
		t.Fatalf("fable-pool exhaustion marks must not exclude accounts from the opus pick; logs=\n%s", logBuf.String())
	}
	resp.Body.Close()
	if !server.SchedulerRef.Get().ForModel(agentclaude.FableFeature).Exhausted(accounts.ProviderClaude, "one@example.com") {
		t.Fatal("the fable-pool mark should still be in place")
	}
}

func opusRejected429() *http.Response {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`)),
		Header:     h,
	}
}

func opusFableRetryTransport(t *testing.T, server *Server, stub *stubRoundTripper, logger *slog.Logger) (usageLimitRetryTransport, *http.Request) {
	t.Helper()
	fableBody := []byte(`{"model":"claude-fable-5","max_tokens":8,"messages":[]}`)
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(fableBody))
	if err != nil {
		t.Fatal(err)
	}
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(fableBody)), nil }
	req.Header.Set("Authorization", "Bearer tok-one")
	return usageLimitRetryTransport{
		base:        stub,
		server:      server,
		logger:      logger,
		provider:    accounts.ProviderClaude,
		agent:       "claude",
		session:     "session-fable",
		account:     "one@example.com",
		method:      http.MethodPost,
		path:        "/v1/messages",
		maxAttempts: 4,
		sleep:       func(context.Context, time.Duration) error { return nil },
	}, req
}

// The transport terminal is the one that fires when scores were stale: fable
// still looks healthy, so failover walks the whole pool and only the upstream's
// own rejections reveal that the fable quota is gone.
func TestUsageLimitRetryTransportSubstitutesOpusAfterFablePoolExhausted(t *testing.T) {
	server, store, logBuf := opusPoolServer(t, 1.0, 1.0)
	if _, err := store.Put("claude", "session-fable", "one@example.com", ""); err != nil {
		t.Fatal(err)
	}
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response { return opusRejected429() }}
	transport, req := opusFableRetryTransport(t, &server, stub, server.Logger)
	transport.poolModel = agentclaude.FableFeature
	transport.opusSubstitute = func(reason string) (*http.Response, bool) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":"msg_1"}`)), Header: http.Header{}}, true
	}
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the opus substitution; logs=\n%s", response.StatusCode, logBuf.String())
	}
	if stub.calls != 2 {
		t.Fatalf("pool attempts = %d, want 2 (both OAuth accounts tried before the opus stage)", stub.calls)
	}
	if logs := logBuf.String(); !strings.Contains(logs, "stage=opus") {
		t.Fatalf("expected the chain marker to name the opus stage; logs=\n%s", logs)
	}
}

func TestUsageLimitRetryTransportPrefersFableChainOverOpusSubstitution(t *testing.T) {
	server, store, _ := opusPoolServer(t, 0, 1.0)
	if _, err := store.Put("claude", "session-fable", "one@example.com", ""); err != nil {
		t.Fatal(err)
	}
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response { return opusRejected429() }}
	transport, req := opusFableRetryTransport(t, &server, stub, server.Logger)
	transport.fableFallback = func() (*http.Response, bool) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"type":"message"}`)), Header: http.Header{}}, true
	}
	transport.opusSubstitute = func(reason string) (*http.Response, bool) {
		t.Error("opus must stay last: Bedrock/API key keep priority when configured")
		return nil, false
	}
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the configured chain", response.StatusCode)
	}
}

// A 529 is Anthropic-wide capacity, not account quota: a different model cannot
// help, and retrying there would amplify load during an incident.
func TestUsageLimitRetryTransportSkipsOpusSubstitutionOnOverload(t *testing.T) {
	server, store, _ := opusPoolServer(t, 0, 1.0)
	if _, err := store.Put("claude", "session-fable", "one@example.com", ""); err != nil {
		t.Fatal(err)
	}
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		return &http.Response{
			StatusCode: 529,
			Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"overloaded_error"}}`)),
			Header:     http.Header{},
		}
	}}
	transport, req := opusFableRetryTransport(t, &server, stub, server.Logger)
	transport.opusSubstitute = func(reason string) (*http.Response, bool) {
		t.Errorf("opus substitution must not run on overload (reason=%q)", reason)
		return nil, false
	}
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 529 {
		t.Fatalf("status = %d, want the 529 passed through", response.StatusCode)
	}
}

func TestUsageLimitRetryTransportOpusSubstitutionRunsAtMostOnce(t *testing.T) {
	server, store, _ := opusPoolServer(t, 0, 1.0)
	if _, err := store.Put("claude", "session-fable", "one@example.com", ""); err != nil {
		t.Fatal(err)
	}
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response { return opusRejected429() }}
	transport, req := opusFableRetryTransport(t, &server, stub, server.Logger)
	calls := 0
	transport.opusSubstitute = onceOpusSubstitute(func(reason string) (*http.Response, bool) {
		calls++
		return nil, false
	})
	// replayablePostRetryTransport wraps this transport on the OUTSIDE, so a
	// network error replays the whole RoundTrip on the same transport value.
	for i := 0; i < 2; i++ {
		response, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
	}
	if calls != 1 {
		t.Fatalf("opus substitution attempts = %d, want exactly 1 per client request", calls)
	}
}

func TestUsageLimitRetryTransportSurfacesFableErrorWhenOpusDeclines(t *testing.T) {
	server, store, logBuf := opusPoolServer(t, 0, 1.0)
	if _, err := store.Put("claude", "session-fable", "one@example.com", ""); err != nil {
		t.Fatal(err)
	}
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response { return opusRejected429() }}
	transport, req := opusFableRetryTransport(t, &server, stub, server.Logger)
	transport.opusSubstitute = func(reason string) (*http.Response, bool) { return nil, false }
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the pool's own 429 unchanged", response.StatusCode)
	}
	if logs := logBuf.String(); !strings.Contains(logs, "claude rate-limit returned to client after failover exhausted") {
		t.Fatalf("expected the client-facing failover-exhaustion signal; logs=\n%s", logs)
	}
}

// With more than replayablePostMaxAttempts Claude accounts the attempt budget
// runs out before oauthRetryAccount does, so the max_attempts terminal is the
// one that fires. It must reach the opus stage too.
func TestUsageLimitRetryTransportSubstitutesOpusOnMaxAttempts(t *testing.T) {
	server, store, _ := opusPoolServer(t, 0, 1.0)
	if _, err := store.Put("claude", "session-fable", "one@example.com", ""); err != nil {
		t.Fatal(err)
	}
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response { return opusRejected429() }}
	transport, req := opusFableRetryTransport(t, &server, stub, server.Logger)
	transport.maxAttempts = 1
	substituted := false
	transport.opusSubstitute = func(reason string) (*http.Response, bool) {
		if reason != "max_attempts" {
			t.Errorf("reason = %q, want max_attempts", reason)
		}
		substituted = true
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":"msg_1"}`)), Header: http.Header{}}, true
	}
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if !substituted || response.StatusCode != http.StatusOK {
		t.Fatalf("substituted=%v status=%d, want the opus stage to run on max_attempts", substituted, response.StatusCode)
	}
}

// opusHandlerUpstream records the model and headers every forwarded request
// carried, so the handler tests can prove what actually reached Anthropic.
type opusHandlerUpstream struct {
	models  []string
	headers []http.Header
	server  *httptest.Server
}

func newOpusHandlerUpstream(t *testing.T, status int, header http.Header) *opusHandlerUpstream {
	t.Helper()
	recorder := &opusHandlerUpstream{}
	recorder.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &payload)
		recorder.models = append(recorder.models, payload.Model)
		recorder.headers = append(recorder.headers, r.Header.Clone())
		for key, values := range header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"id":"msg_ok","type":"message"}`))
	}))
	t.Cleanup(recorder.server.Close)
	return recorder
}

func opusHandlerRequest(t *testing.T, sessionID, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://subrouter/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", sessionID)
	return req
}

func TestClaudeFableHandlerSubstitutesOpusWhenFablePoolExhausted(t *testing.T) {
	upstream := newOpusHandlerUpstream(t, http.StatusOK, nil)
	server, _, logBuf := opusPoolServer(t, 0, 1.0)
	upstreamURL, err := url.Parse(upstream.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	server.ClaudeUpstream = upstreamURL

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, opusHandlerRequest(t, "session-sub", `{"model":"claude-fable-5[1m]","max_tokens":8,"messages":[]}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 via the opus pool; body=%s logs=\n%s", recorder.Code, recorder.Body.String(), logBuf.String())
	}
	if len(upstream.models) != 1 || upstream.models[0] != "claude-opus-5[1m]" {
		t.Fatalf("upstream models = %v, want one claude-opus-5[1m]", upstream.models)
	}
	for _, header := range []string{"X-Subrouter-Model", "X-Model"} {
		if got := upstream.headers[0].Get(header); got != "" {
			t.Fatalf("%s = %q leaked to Anthropic", header, got)
		}
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "substituting claude opus for exhausted fable pool") || !strings.Contains(logs, "stage=pool_selection") {
		t.Fatalf("expected the pool-selection substitution log; logs=\n%s", logs)
	}
}

func TestClaudeFableHandlerReturnsOriginalErrorWhenOpusAlsoExhausted(t *testing.T) {
	upstream := newOpusHandlerUpstream(t, http.StatusOK, nil)
	server, _, _ := opusPoolServer(t, 0, 0)
	upstreamURL, err := url.Parse(upstream.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	server.ClaudeUpstream = upstreamURL

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, opusHandlerRequest(t, "session-none", `{"model":"claude-fable-5","max_tokens":8,"messages":[]}`))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the existing 503", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "no non-exhausted claude accounts available") {
		t.Fatalf("body = %q, want the fable error byte-for-byte", recorder.Body.String())
	}
	if len(upstream.models) != 0 {
		t.Fatalf("upstream models = %v, want no request at all", upstream.models)
	}
}

// A bound lease pins the model, so substituting a different one would violate
// the lease contract.
func TestClaudeFableHandlerDoesNotSubstituteUnderSessionLease(t *testing.T) {
	rejected := http.Header{}
	rejected.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	upstream := newOpusHandlerUpstream(t, http.StatusTooManyRequests, rejected)
	server, _, logBuf := opusPoolServer(t, 1.0, 1.0)
	upstreamURL, err := url.Parse(upstream.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	server.ClaudeUpstream = upstreamURL
	server.sessionLeases = newSessionLeaseStore()
	server.AdminToken = "service-admin-token"

	handler := server.Handler()
	lease, _ := issueSessionLease(t, handler, "claude", "claude-fable-5")
	// Cook the fable pool only after the lease is bound to the fable model.
	server.SchedulerRef.Set(selectacct.NewScheduler(
		opusPoolScores(0, 1.0, "one@example.com", "two@example.com")))

	req := httptest.NewRequest(http.MethodPost, "http://subrouter/v1/messages", strings.NewReader(`{"model":"claude-fable-5","max_tokens":8,"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+lease.Environment["CLOUDMUX_SUBROUTER_LEASE_TOKEN"])
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	if len(upstream.models) == 0 {
		t.Fatalf("the leased request never reached upstream; status=%d body=%s logs=\n%s", recorder.Code, recorder.Body.String(), logBuf.String())
	}
	for _, model := range upstream.models {
		if model != "claude-fable-5" {
			t.Fatalf("upstream models = %v, want the leased model only", upstream.models)
		}
	}
	if logs := logBuf.String(); strings.Contains(logs, "substituting claude opus") {
		t.Fatalf("a bound lease must never be substituted; logs=\n%s", logs)
	}
}

func TestClaudeFableHandlerDoesNotSubstituteNonFableModels(t *testing.T) {
	upstream := newOpusHandlerUpstream(t, http.StatusOK, nil)
	server, _, logBuf := opusPoolServer(t, 1.0, 0)
	upstreamURL, err := url.Parse(upstream.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	server.ClaudeUpstream = upstreamURL

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, opusHandlerRequest(t, "session-opus-only", `{"model":"claude-opus-5","max_tokens":8,"messages":[]}`))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	if logs := logBuf.String(); strings.Contains(logs, "substituting claude opus") {
		t.Fatalf("an already-opus request must not be rewritten; logs=\n%s", logs)
	}
}

// Once the handler has moved the request onto the Opus pool the transport stage
// is disarmed, so a rejected Opus response surfaces instead of triggering a
// second substitution.
func TestClaudeFableHandlerSubstitutionDisarmsTransportStage(t *testing.T) {
	rejected := http.Header{}
	rejected.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	upstream := newOpusHandlerUpstream(t, http.StatusTooManyRequests, rejected)
	server, _, logBuf := opusPoolServer(t, 0, 1.0)
	upstreamURL, err := url.Parse(upstream.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	server.ClaudeUpstream = upstreamURL

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, opusHandlerRequest(t, "session-disarm", `{"model":"claude-fable-5","max_tokens":8,"messages":[]}`))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the opus 429 surfaced", recorder.Code)
	}
	logs := logBuf.String()
	if got := strings.Count(logs, "substituting claude opus for exhausted fable pool"); got != 1 {
		t.Fatalf("substitution logs = %d, want exactly 1 (handler only); logs=\n%s", got, logs)
	}
}

// The substitution sends a full completion to the picked Opus account, so the
// scheduler has to see that traffic. Live debits are keyed account-wide, so an
// unrecorded substitution would leave the account looking unloaded to every
// pool and invite the balancer to pile more work onto it.
func TestClaudeOpusSubstituteResponseDebitsThePickedAccount(t *testing.T) {
	server, _, logBuf := opusPoolServer(t, 0, 1.0)
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"id":"msg_1","type":"message"}`)),
			Header:     http.Header{},
		}
	}}
	server.Transport = stub

	if debits := server.SchedulerRef.LiveDebits(); len(debits) != 0 {
		t.Fatalf("live debits = %v, want none before the substitution", debits)
	}
	req := opusSubstituteRequest(t, `{"model":"claude-fable-5","max_tokens":8,"messages":[]}`)
	resp, ok := server.claudeOpusSubstituteResponse(req, "claude", "session-debit", "", "one@example.com", "usage_limit")
	if !ok {
		t.Fatalf("expected the opus pool to serve the request; logs=\n%s", logBuf.String())
	}
	defer resp.Body.Close()

	debits := server.SchedulerRef.LiveDebits()
	total := 0
	for _, count := range debits {
		total += count
	}
	if total != 1 {
		t.Fatalf("live debits = %v (total %d), want exactly one for the substituted request", debits, total)
	}
}

// Delegated mode (Subrouter forwarding to another Subrouter) re-adds the
// session headers after stripping them, so the far side can keep the request
// sticky. A substituted request must carry them too, or stickiness would break
// only on the substitution path.
func TestClaudeOpusSubstituteResponseKeepsDelegatedSessionHeaders(t *testing.T) {
	server, _, logBuf := opusPoolServer(t, 0, 1.0)
	server.Upstream = &url.URL{Scheme: "https", Host: "peer.subrouter.internal"}
	server.ForwardSessionHeaders = true
	var captured *http.Request
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		captured = req
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"id":"msg_1","type":"message"}`)),
			Header:     http.Header{},
		}
	}}
	server.Transport = stub

	req := opusSubstituteRequest(t, `{"model":"claude-fable-5","max_tokens":8,"messages":[]}`)
	resp, ok := server.claudeOpusSubstituteResponse(req, "claude", "session-delegated", "", "one@example.com", "usage_limit")
	if !ok {
		t.Fatalf("expected the opus pool to serve the request; logs=\n%s", logBuf.String())
	}
	defer resp.Body.Close()

	if got := captured.Header.Get("X-Subrouter-Agent"); got != "claude" {
		t.Fatalf("X-Subrouter-Agent = %q, want the delegated agent restored", got)
	}
	if got := captured.Header.Get("X-Subrouter-Session"); got != "session-delegated" {
		t.Fatalf("X-Subrouter-Session = %q, want the delegated session restored", got)
	}
	if got := captured.Header.Get("X-Subrouter-Model"); got != "" {
		t.Fatalf("X-Subrouter-Model = %q, want the routing hint stripped even in delegated mode", got)
	}
}
