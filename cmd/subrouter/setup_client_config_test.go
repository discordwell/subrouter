package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/internal/broker"
)

// The plan offers to route Claude Code through Subrouter and separately reports
// whether that is already done. Those two have to agree. The detector looked for
// the /v1 base URL, which the writer never records, so the item stayed pending
// no matter how many times setup ran.
func TestClaudeConfigWriteAndDetectAgree(t *testing.T) {
	home := sandboxHostConfig(t)

	settingsPath := filepath.Join(home, ".claude", "settings.json")
	root := codexProxyRootURL(localBaseURL())
	if clientRoutesToLocal(settingsPath, root) {
		t.Fatal("an unwritten config was reported as already configured")
	}

	written, err := writeClaudeProxyEnvForLocal()
	if err != nil {
		t.Fatal(err)
	}
	if written != settingsPath {
		t.Fatalf("wrote %s, want %s", written, settingsPath)
	}
	if !clientRoutesToLocal(settingsPath, root) {
		body, _ := os.ReadFile(settingsPath)
		t.Fatalf("setup's own write is not detected as configured: %s", body)
	}
}

// Claude Code appends /v1 itself, so the stored value must be the bare root or
// every request goes to /v1/v1.
func TestClaudeBaseURLIsTheProxyRootNotTheV1Base(t *testing.T) {
	home := sandboxHostConfig(t)
	if _, err := writeClaudeProxyEnvForLocal(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatal(err)
	}
	got := settings.Env["ANTHROPIC_BASE_URL"]
	if want := codexProxyRootURL(localBaseURL()); got != want {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want %q", got, want)
	}
	if strings.HasSuffix(got, "/v1") {
		t.Error("stored the /v1 base URL; Claude Code would request /v1/v1")
	}
}

// settings.json also holds the user's own environment, and setup must not be a
// reason to lose it.
func TestClaudeConfigPreservesUnrelatedSettings(t *testing.T) {
	home := sandboxHostConfig(t)
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	seed := `{"env":{"AWS_PROFILE":"mine"},"statusLine":"keep me"}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := writeClaudeProxyEnvForLocal(); err != nil {
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
	env, _ := settings["env"].(map[string]any)
	if env["AWS_PROFILE"] != "mine" {
		t.Errorf("unrelated env var was dropped: %s", body)
	}
	if settings["statusLine"] != "keep me" {
		t.Errorf("unrelated setting was dropped: %s", body)
	}
	if env["ANTHROPIC_BASE_URL"] == nil {
		t.Errorf("proxy env was not written: %s", body)
	}
}

// CLAUDE_CONFIG_DIR relocates Claude Code's config dir, and Subrouter already
// exports it to every Claude child process — so the writer and the planner's
// already-done probe must follow it too, to the same file, while $HOME/.claude
// stays untouched. Writing ~/.claude/settings.json for a user whose Claude
// Code reads $CLAUDE_CONFIG_DIR/settings.json configures nothing and reports
// the wrong file as pending forever.
func TestClaudeConfigHonorsClaudeConfigDir(t *testing.T) {
	home := sandboxHostConfig(t)
	configDir := filepath.Join(t.TempDir(), "claude-config")
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)

	written, err := writeClaudeProxyEnvForLocal()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(configDir, "settings.json"); written != want {
		t.Fatalf("wrote %s, want %s", written, want)
	}

	// The planner must read the same file the writer wrote: the item comes
	// back "already done" (=), not pending.
	plan, err := planForSetup(
		context.Background(),
		accounts.CodexStore{Dir: t.TempDir()},
		setupPlanOptions{
			wantBackground:   true,
			wantConfig:       true,
			credentialSource: broker.CredentialSourceLocal,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	item, ok := plan.find(actionConfigClaude)
	if !ok {
		t.Fatal("plan has no Claude config item")
	}
	if item.Change != "=" {
		t.Fatalf("planner did not detect the CLAUDE_CONFIG_DIR write as done (change %q)", item.Change)
	}

	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Fatalf("$HOME/.claude was touched despite CLAUDE_CONFIG_DIR: %v", err)
	}
}

// The Codex side records the /v1 base URL, and its detector reads the same
// value, so a configured machine stops offering to configure it again.
func TestCodexConfigWriteAndDetectAgree(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)

	path := filepath.Join(codexHome, "config.toml")
	if clientRoutesToLocal(path, localBaseURL()) {
		t.Fatal("an unwritten config was reported as already configured")
	}
	written, err := writeCodexConfigForLocal()
	if err != nil {
		t.Fatal(err)
	}
	if written != path {
		t.Fatalf("wrote %s, want %s", written, path)
	}
	if !clientRoutesToLocal(path, localBaseURL()) {
		body, _ := os.ReadFile(path)
		t.Fatalf("setup's own write is not detected as configured: %s", body)
	}
}
