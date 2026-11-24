package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/agents/claude"
)

func TestUploadServerClaudeProfileUsesProtectedHTTPOnly(t *testing.T) {
	var preflightRequests, importRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/_subrouter/account-import" {
			t.Errorf("path = %q, want account import endpoint", req.URL.Path)
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		if got := req.Header.Get("Authorization"); got != "Bearer import-secret" {
			t.Error("Authorization header did not match the expected protected import credential")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch req.Method {
		case http.MethodGet:
			preflightRequests.Add(1)
		case http.MethodPost:
			importRequests.Add(1)
			var payload struct {
				Provider string `json:"provider"`
				Claude   *struct {
					Name       string                `json:"name"`
					Credential claude.CredentialInfo `json:"credential"`
				} `json:"claude"`
			}
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				t.Error(err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if payload.Provider != "claude" || payload.Claude == nil || payload.Claude.Name != "founders@manaflow.ai" {
				t.Errorf("unexpected Claude import payload: provider=%q profile set=%t", payload.Provider, payload.Claude != nil)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if payload.Claude.Credential.RefreshToken != "claude-refresh" {
				t.Error("server did not receive the fresh Claude refresh-token chain")
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
		default:
			t.Errorf("method = %s, want GET preflight or POST import", req.Method)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	var out bytes.Buffer
	fake := &recordingSRCommandRunner{}
	runner := srRunner{out: &out, errOut: &out, cmd: fake, client: server.Client()}
	err := runner.uploadServerClaudeProfile(
		t.Context(),
		srServerConfig{Name: "gcp-team", URL: server.URL, AdminToken: "import-secret"},
		claude.Store{Dir: t.TempDir()},
		claude.Profile{Name: "founders@manaflow.ai", CreatedAt: time.Now().UTC().Format(time.RFC3339)},
		claude.CredentialInfo{AccessToken: "claude-access", RefreshToken: "claude-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()},
	)
	if err != nil {
		t.Fatal(err)
	}
	if preflightRequests.Load() != 1 || importRequests.Load() != 1 {
		t.Fatalf("account import requests = preflight:%d post:%d, want 1 each", preflightRequests.Load(), importRequests.Load())
	}
	for _, forbidden := range []string{"ssh", "scp", "gcloud"} {
		if fake.hasCommandPrefix(forbidden) {
			t.Fatalf("Claude upload must never execute %s: %#v", forbidden, fake.commands)
		}
	}
}

func TestWriteClaudeProxyEnvMergesSettings(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"theme":"dark","env":{"FOO":"bar","ANTHROPIC_CUSTOM_HEADERS":"Authorization: stale-secret"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeProxyEnv(dir, "http://subrouter-team:31415/", ""); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["theme"] != "dark" {
		t.Fatalf("theme clobbered: %v", settings["theme"])
	}
	env := settings["env"].(map[string]any)
	if env["FOO"] != "bar" {
		t.Fatalf("unrelated env clobbered: %v", env["FOO"])
	}
	if env["ANTHROPIC_BASE_URL"] != "http://subrouter-team:31415" {
		t.Fatalf("base url = %v (trailing slash should be trimmed)", env["ANTHROPIC_BASE_URL"])
	}
	if env["ANTHROPIC_AUTH_TOKEN"] != "subrouter" {
		t.Fatalf("auth token = %v", env["ANTHROPIC_AUTH_TOKEN"])
	}
	if env["ANTHROPIC_CUSTOM_HEADERS"] != "X-Subrouter-Agent: claude" {
		t.Fatalf("stale custom headers survived: %v", env["ANTHROPIC_CUSTOM_HEADERS"])
	}

	// Second write is idempotent and keeps a custom token if one exists.
	env["ANTHROPIC_AUTH_TOKEN"] = "custom"
	body, _ = json.Marshal(settings)
	if err := os.WriteFile(settingsPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeProxyEnv(dir, "http://subrouter-team:31415", ""); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(settingsPath)
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatal(err)
	}
	if got := settings["env"].(map[string]any)["ANTHROPIC_AUTH_TOKEN"]; got != "custom" {
		t.Fatalf("custom token clobbered: %v", got)
	}
}

func TestWriteClaudeProxyEnvCreatesSettings(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fresh")
	if err := writeClaudeProxyEnv(dir, "http://subrouter-team:31415", ""); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatal(err)
	}
	env := settings["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != "http://subrouter-team:31415" {
		t.Fatalf("base url = %v", env["ANTHROPIC_BASE_URL"])
	}
	// Upstream #463 asserts ENABLE_TOOL_SEARCH=true here. writeClaudeProxyEnv
	// writes the user's own config dir (profileOwned=false), the settings.json
	// plain `claude` reads, and the fork adds tool search only to profile
	// directories (PATCHES §29.9.1; claude_tool_search_test.go).
	if value, ok := env[claudeToolSearchEnv]; ok {
		t.Fatalf("%s = %v written into the user's config dir", claudeToolSearchEnv, value)
	}
}

func TestWriteClaudeProxyEnvKeepsUserToolSearchChoice(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"env":{"ENABLE_TOOL_SEARCH":"false"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeProxyEnv(dir, "http://subrouter-team:31415", ""); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatal(err)
	}
	if got := settings.Env[claudeToolSearchEnv]; got != "false" {
		t.Fatalf("%s = %q, want the user's false", claudeToolSearchEnv, got)
	}
}

// An identical re-run must be a true no-op: same bytes, same mtime, no backup
// churn. Anything else means the writer replaced settings.json underneath a
// possibly-running Claude Code for no reason. Crash-safety of the write that
// does happen is claude.WritePrivateFileAtomic's contract (fsync + rename),
// covered with the credential-store tests; here we pin the no-op side.
func TestWriteClaudeProxyEnvIdenticalRerunTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	if err := writeClaudeProxyEnv(dir, "http://subrouter-team:31415", ""); err != nil {
		t.Fatal(err)
	}
	// The first write had no prior file, so there is nothing to back up.
	if _, err := os.Stat(settingsPath + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("a fresh write created a backup: %v", err)
	}
	first, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(settingsPath, past, past); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeProxyEnv(dir, "http://subrouter-team:31415", ""); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(past) {
		t.Fatalf("identical re-run rewrote settings.json (mtime %v, want %v)", info.ModTime(), past)
	}
	second, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("identical re-run changed content:\nfirst:  %s\nsecond: %s", first, second)
	}
	if _, err := os.Stat(settingsPath + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("identical re-run created a backup: %v", err)
	}
}

// A real change snapshots the prior bytes — including the user's original
// formatting, which the canonical re-marshal loses — to settings.json.bak.
func TestWriteClaudeProxyEnvBacksUpPriorContentOnRealChange(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	seed := "{\"theme\":  \"dark\",\n  \"env\": {\"FOO\": \"bar\"}}\n"
	if err := os.WriteFile(settingsPath, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeProxyEnv(dir, "http://subrouter-team:31415", ""); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(settingsPath + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != seed {
		t.Fatalf("backup does not hold the user's original bytes:\n%s", backup)
	}
	afterFirst, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeProxyEnv(dir, "http://subrouter-other:31415", ""); err != nil {
		t.Fatal(err)
	}
	backup, err = os.ReadFile(settingsPath + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backup, afterFirst) {
		t.Fatalf("backup after a second change does not hold the prior content:\n%s", backup)
	}
}

// A settings.json that cannot be parsed must fail the write and stay
// byte-identical on disk. Merging into an empty map and writing that out would
// replace whatever the user had with only the proxy env.
func TestWriteClaudeProxyEnvRefusesToClobberUnparsableSettings(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	corrupt := `{"env": not-json`
	if err := os.WriteFile(settingsPath, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeProxyEnv(dir, "http://subrouter-team:31415", ""); err == nil {
		t.Fatal("corrupt settings.json did not fail the write")
	}
	body, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != corrupt {
		t.Fatalf("failed write modified settings.json: %s", body)
	}
	if _, err := os.Stat(settingsPath + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("failed write left a backup: %v", err)
	}
}

func TestWriteClaudeProxyEnvForServerPreservesCanonicalHostname(t *testing.T) {
	dir := t.TempDir()
	server := srServerConfig{
		URL:             "http://m3.example.ts.net.:31415",
		TenantKey:       testTenantKey,
		TailscaleNodeID: "node-m3",
	}
	lookup := func(context.Context, string) ([]net.IPAddr, error) {
		t.Fatal("node-pinned enrollment must not use DNS")
		return nil, nil
	}
	load := func(context.Context) ([]byte, error) {
		return []byte(`{"Self":{"ID":"self","Online":true},"Peer":{"node-m3":{"ID":"node-m3","DNSName":"m3.example.ts.net.","TailscaleIPs":["100.88.0.9"],"Online":true}}}`), nil
	}
	if err := writeClaudeProxyEnvForServerWithResolvers(dir, server, false, lookup, load); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatal(err)
	}
	if got := settings.Env["ANTHROPIC_BASE_URL"]; got != managedClaudeBlockedBaseURL {
		t.Fatalf("persisted base URL = %q, want closed loopback %q", got, managedClaudeBlockedBaseURL)
	}
	if got := settings.Env["ANTHROPIC_AUTH_TOKEN"]; got != testTenantKey {
		t.Fatal("persisted auth token did not match the expected tenant key")
	}
	if got := settings.Env[managedClaudeServerURLEnv]; got != server.URL {
		t.Fatalf("managed server URL = %q", got)
	}
	if got := settings.Env[managedClaudeTailscaleNodeEnv]; got != "node-m3" {
		t.Fatalf("managed Tailscale node = %q", got)
	}
	secureBaseURL, err := secureManagedClaudeProfileTransportWithResolvers(dir, lookup, load)
	if err != nil || secureBaseURL != "http://100.88.0.9:31415/t/"+testTenantKey {
		t.Fatalf("runtime secured base URL = %q, %v", secureBaseURL, err)
	}
}

func TestWriteClaudeProxyEnvForServerRejectsMissingNodeBeforeMutation(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	original := []byte(`{"theme":"dark","env":{"ANTHROPIC_AUTH_TOKEN":"original"}}`)
	if err := os.WriteFile(settingsPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	err := writeClaudeProxyEnvForServerWithResolvers(
		dir,
		srServerConfig{Name: "legacy", URL: "http://remote.example:31415", TenantKey: testTenantKey},
		false,
		func(context.Context, string) ([]net.IPAddr, error) {
			t.Fatal("missing exact node identity must reject before DNS")
			return nil, nil
		},
		func(context.Context) ([]byte, error) {
			t.Fatal("missing exact node identity must reject before Tailscale status")
			return nil, nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "no exact Tailscale node identity") {
		t.Fatalf("missing-node error = %v", err)
	}
	after, readErr := os.ReadFile(settingsPath)
	if readErr != nil || !bytes.Equal(after, original) {
		t.Fatalf("settings changed on rejected enrollment: %q, %v", after, readErr)
	}
}

func TestWriteClaudeProxyEnvForServerHTTPSClearsPlaintextIdentityAtomically(t *testing.T) {
	dir := t.TempDir()
	if err := writeClaudeProxyEnvCanonicalForServer(
		dir, managedClaudeBlockedBaseURL, testTenantKey,
		&srServerConfig{URL: "http://m3.example:31415", TailscaleNodeID: "node-m3"}, false,
	); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeProxyEnvCanonical(dir, "https://secure.example/t/"+testTenantKey, testTenantKey, false); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatal(err)
	}
	if got := settings.Env["ANTHROPIC_BASE_URL"]; got != "https://secure.example/t/"+testTenantKey {
		t.Fatalf("HTTPS base URL = %q", got)
	}
	if settings.Env[managedClaudeServerURLEnv] != "" || settings.Env[managedClaudeTailscaleNodeEnv] != "" {
		t.Fatalf("stale plaintext identity survived HTTPS transition: %+v", settings.Env)
	}
}
