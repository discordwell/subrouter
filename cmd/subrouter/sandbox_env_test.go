package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// sandboxHostConfig fences a test off every host path the setup writers and
// probes resolve through the environment. Sandboxing HOME alone is not enough:
// defaultClaudeConfigDir prefers CLAUDE_CONFIG_DIR, and that variable is
// exported in exactly the shells these tests get run from — `sr claude env`
// prints `export CLAUDE_CONFIG_DIR=...` for users to eval, and every
// subrouter-wrapped Claude Code session carries it. A HOME-only sandbox let
// `go test ./cmd/subrouter` follow the ambient value and rewrite the user's
// real settings.json with a fixture token the live daemon rejects.
//
// Every test that reaches defaultClaudeConfigDir, defaultCodexConfigPath, or
// cloudModeConfig must call this (and may then override individual variables
// with its own temp paths, as TestClaudeConfigHonorsClaudeConfigDir does).
func sandboxHostConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Empty means "not set" to defaultClaudeConfigDir, so the writer falls
	// back to $HOME/.claude inside the sandbox.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(home, "state"))
	// A missing cloud.json loads as the default config, so pointing at a
	// not-yet-written path inside HOME is safe and keeps the real
	// ~/.config/subrouter/cloud.json (live tokens) out of reach.
	t.Setenv("SUBROUTER_CLOUD_CONFIG", filepath.Join(home, "cloud.json"))
	return home
}

// The escape this guards against: CLAUDE_CONFIG_DIR exported in the shell
// running `go test`, pointing at the live Claude Code config. The sandbox has
// to defeat it — the write must land under the sandbox HOME, and the ambient
// directory must come through byte-identical, with no .bak churn. Removing the
// CLAUDE_CONFIG_DIR neutralization from sandboxHostConfig fails here instead
// of silently re-arming the host rewrite in every test that uses the helper.
func TestHostSandboxDefeatsAmbientClaudeConfigDir(t *testing.T) {
	ambient := filepath.Join(t.TempDir(), "live-claude-config")
	if err := os.MkdirAll(ambient, 0o700); err != nil {
		t.Fatal(err)
	}
	seed := []byte(`{"env":{"ANTHROPIC_AUTH_TOKEN":"the-users-real-token"}}`)
	ambientSettings := filepath.Join(ambient, "settings.json")
	if err := os.WriteFile(ambientSettings, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	// The ambient export, set before the sandbox as it would be in a real
	// shell; the sandbox must override it.
	t.Setenv("CLAUDE_CONFIG_DIR", ambient)

	home := sandboxHostConfig(t)

	written, err := writeClaudeProxyEnvForLocal()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".claude", "settings.json"); written != want {
		t.Fatalf("sandboxed write went to %s, want %s", written, want)
	}

	body, err := os.ReadFile(ambientSettings)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, seed) {
		t.Fatalf("sandboxed test rewrote the ambient CLAUDE_CONFIG_DIR settings.json: %s", body)
	}
	if _, err := os.Stat(ambientSettings + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("sandboxed test churned a .bak in the ambient CLAUDE_CONFIG_DIR: %v", err)
	}
}
