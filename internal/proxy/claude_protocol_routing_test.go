package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/session"
)

// TestAnthropicProtocolRequestsReachClaudeUpstream drives Server.Handler()
// with the requests Claude Code sends that carry no Claude user agent or
// session header: its Bun-runtime connectivity check (HEAD /api/hello) and an
// API call identified only by anthropic-version. Both must reach the Claude
// upstream; before, they fell through to the Codex default and chatgpt.com
// answered with an HTML page. A plain Codex request must still go to Codex.
func TestAnthropicProtocolRequestsReachClaudeUpstream(t *testing.T) {
	var mu sync.Mutex
	hits := map[string][]string{}
	upstream := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits[name] = append(hits[name], r.Method+" "+r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
	}
	claude, codex := upstream("claude"), upstream("codex")
	defer claude.Close()
	defer codex.Close()
	claudeURL, err := url.Parse(claude.URL)
	if err != nil {
		t.Fatal(err)
	}
	codexURL, err := url.Parse(codex.URL)
	if err != nil {
		t.Fatal(err)
	}

	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		Sessions:       store,
		ClaudeUpstream: claudeURL,
		CodexUpstream:  codexURL,
		Accounts: []accounts.Account{
			{ID: "claude@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-claude"},
			{ID: "codex@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "tok-codex"},
		},
		MaxBodyBytes: 1 << 20,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	send := func(method, path string, header map[string]string) {
		t.Helper()
		req, err := http.NewRequest(method, subrouter.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range header {
			req.Header.Set(key, value)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Logf("%s %s -> %d %.200s", method, path, response.StatusCode, body)
	}
	send(http.MethodHead, "/api/hello", map[string]string{"User-Agent": "Bun/1.4.3"})
	send(http.MethodGet, "/v1/models", map[string]string{"User-Agent": "Bun/1.4.3", "Anthropic-Version": "2023-06-01"})
	send(http.MethodGet, "/backend-api/codex/models", map[string]string{"User-Agent": "codex-tui/0.162.1"})
	// A provider path prefix still decides the provider before any
	// Anthropic evidence does.
	send(http.MethodPost, "/kimi/v1/messages", map[string]string{"Anthropic-Version": "2023-06-01"})

	mu.Lock()
	defer mu.Unlock()
	claudeHits := strings.Join(hits["claude"], ", ")
	if !strings.Contains(claudeHits, "HEAD /api/hello") || !strings.Contains(claudeHits, "GET /v1/models") {
		t.Fatalf("claude upstream got %q, want the connectivity check and the anthropic-version request", claudeHits)
	}
	for _, hit := range hits["codex"] {
		if strings.Contains(hit, "/api/hello") || strings.Contains(hit, "/v1/models") {
			t.Fatalf("codex upstream got Anthropic traffic %q; all codex hits: %v", hit, hits["codex"])
		}
	}
	if strings.Contains(claudeHits, "/kimi/") || strings.Contains(claudeHits, "POST /v1/messages") {
		t.Fatalf("claude upstream got the Kimi-prefixed request: %q", claudeHits)
	}
	if len(hits["codex"]) != 1 {
		t.Fatalf("codex upstream hits = %v, want the one Codex request", hits["codex"])
	}
}

// The same requests on this fork's live shape: Claude Code reaches the daemon
// over the Claude socket, so the Claude control plane answers only from the
// pinned identity, and a peer is configured. The connectivity check is under
// /api/, so it is a Claude control-plane call: it goes to Anthropic on the
// pinned identity, never to chatgpt.com on the Codex identity (where it went
// before) and never to a peer. A request known only by anthropic-beta never
// reaches Codex either; if the peer serves it, it is told the agent is Claude.
func TestClaudeCodeHelloUsesThePinnedClaudeIdentity(t *testing.T) {
	claude, claudeURL := newIdentityUpstream(t)
	var mu sync.Mutex
	var codexHits, peerHits []string
	codex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		codexHits = append(codexHits, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
	}))
	defer codex.Close()
	codexURL, err := url.Parse(codex.URL)
	if err != nil {
		t.Fatal(err)
	}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_subrouter/") {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		peerHits = append(peerHits, r.Method+" "+r.URL.Path+" agent="+r.Header.Get("X-Subrouter-Agent"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer peer.Close()

	server := identityServer(t, claudeURL, "mid@example.com", true, false)
	server.CodexUpstream = codexURL
	server.Accounts = append(server.Accounts, accounts.Account{
		ID: "codex@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "tok-codex",
	})
	server.RemotePool = NewRemotePool(peer.URL, "", "")
	subrouter := httptest.NewServer(server.Handler())
	defer subrouter.Close()

	send := func(method, path string, header map[string]string) int {
		t.Helper()
		req, err := http.NewRequest(method, subrouter.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range header {
			req.Header.Set(key, value)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return response.StatusCode
	}

	if status := send(http.MethodHead, "/api/hello", map[string]string{"User-Agent": "Bun/1.3.5"}); status != http.StatusOK {
		t.Fatalf("HEAD /api/hello = %d, want the Claude upstream's 200", status)
	}
	if got, want := strings.Join(claude.seen(), ", "), "HEAD /api/hello tok-mid"; got != want {
		t.Fatalf("claude upstream saw %q, want %q: the connectivity check is answered by the pinned identity", got, want)
	}
	send(http.MethodGet, "/v1/models", map[string]string{"User-Agent": "Bun/1.3.5", "Anthropic-Beta": "oauth-2025-04-20"})

	mu.Lock()
	defer mu.Unlock()
	if len(codexHits) != 0 {
		t.Fatalf("codex upstream got %v, want no Anthropic traffic", codexHits)
	}
	for _, hit := range peerHits {
		if strings.Contains(hit, "/api/") || !strings.HasSuffix(hit, " agent=claude") {
			t.Fatalf("peer got %q (all: %v): a control-plane call must stay local, and a forward must name the Claude agent", hit, peerHits)
		}
	}
	served := len(peerHits)
	for _, call := range claude.seen() {
		if strings.HasPrefix(call, "GET /v1/models ") {
			served++
		}
	}
	if served != 1 {
		t.Fatalf("GET /v1/models reached Claude %d times (upstream %v, peer %v), want once", served, claude.seen(), peerHits)
	}
}
