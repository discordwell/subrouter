package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/storepath"
)

// `sr claude`, `sr claude proxy` and `sr claude run` launch Claude Code with
// CLAUDE_CONFIG_DIR at a profile or proxy directory, so ~/.claude/settings.json
// -- hooks, permissions, the status line -- never loaded (upstream #435). These
// drive the real launchers against a recording `claude` in a sandboxed HOME and
// read the private --settings file it was given. They use only symbols that
// exist before the port, so on the pre-port tree they fail on the missing
// hooks.

// userClaudeSettingsFixture is a ~/.claude/settings.json shaped like the
// laptop's: hooks and permissions to carry, a credential helper and a forced
// login to drop, and an env that routes through the local daemon (the cutover)
// and selects Bedrock, none of which may reach a pooled or direct launch.
const userClaudeSettingsFixture = `{
	"hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "guard.sh"}]}]},
	"permissions": {"allow": ["Bash(ls:*)"]},
	"statusLine": {"type": "command", "command": "user-status"},
	"theme": "dark",
	"apiKeyHelper": "print-other-key",
	"forceLoginMethod": "console",
	"env": {
		"USER_ONLY": "kept",
		"ANTHROPIC_BASE_URL": "http://127.0.0.1:31415",
		"ANTHROPIC_AUTH_TOKEN": "the-daemon-token",
		"anthropic_custom_headers": "X-Other: 1",
		"CLAUDE_CODE_USE_BEDROCK": "1"
	}
}`

func writeUserClaudeSettings(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(userClaudeSettingsFixture), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readRecordedLaunchSettings(t *testing.T, path string) (map[string]any, map[string]any) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse launch settings: %v\n%s", err, body)
	}
	env, _ := doc["env"].(map[string]any)
	return doc, env
}

// assertUserSettingsCarried checks the private settings document: the user's
// hooks, permissions and status line are there, credential sources are not,
// and no user env entry displaced or duplicated a routing key.
func assertUserSettingsCarried(t *testing.T, doc, env map[string]any) {
	t.Helper()
	hooks, _ := doc["hooks"].(map[string]any)
	if _, ok := hooks["PreToolUse"]; !ok {
		t.Fatalf("the user's PreToolUse hook did not reach the launched Claude Code: %v", doc)
	}
	if doc["permissions"] == nil || doc["statusLine"] == nil {
		t.Fatalf("the user's permissions or status line did not reach the launched Claude Code: %v", doc)
	}
	for _, key := range []string{"apiKeyHelper", "forceLoginMethod"} {
		if _, ok := doc[key]; ok {
			t.Fatalf("credential-source setting %s leaked into the launch: %v", key, doc)
		}
	}
	if env["USER_ONLY"] != "kept" {
		t.Fatalf("the user's own env entry was dropped: %v", env)
	}
	for key := range env {
		if key == "anthropic_custom_headers" {
			t.Fatalf("a routing key in another letter case survived beside sr's: %v", env)
		}
	}
	if env["ANTHROPIC_AUTH_TOKEN"] == "the-daemon-token" || env["CLAUDE_CODE_USE_BEDROCK"] == "1" {
		t.Fatalf("the user's routing env reached the launch: %v", env)
	}
}

func TestClaudeProxyLaunchCarriesTheUsersSettings(t *testing.T) {
	clearClaudeCacheTTLEnv(t)
	home := sandboxHostConfig(t)
	writeUserClaudeSettings(t, home)
	recordDir := writeRecordingClaudeCLI(t)
	// The runner's store is the real default store under the sandboxed HOME:
	// the user's settings are read only for it, never for a hermetic store.
	runner := srRunner{
		program: "sr",
		store:   accounts.CodexStore{Dir: filepath.Join(storepath.CodexDir(), "accounts")},
		in:      strings.NewReader(""), out: io.Discard, errOut: io.Discard,
	}
	if filepath.Clean(runner.store.StoreDir()) != filepath.Clean(claude.DefaultStore().Dir) {
		t.Fatalf("calibration: runner store %s is not the default store %s", runner.store.StoreDir(), claude.DefaultStore().Dir)
	}
	profile := t.TempDir()
	if err := os.WriteFile(filepath.Join(profile, "settings.json"), []byte(`{"forceLoginMethod":"claudeai"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runner.runProxyClaude(t.Context(), nil, "http://127.0.0.1:31415/v1", "tok", profile, "", ""); err != nil {
		t.Fatal(err)
	}
	doc, env := readRecordedLaunchSettings(t, filepath.Join(recordDir, "settings.json"))
	if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:31415" || env["ANTHROPIC_AUTH_TOKEN"] != "tok" ||
		env["CLAUDE_CONFIG_DIR"] != profile || env["CLAUDE_CODE_USE_BEDROCK"] != "" {
		t.Fatalf("routing env is not sr's: %v", env)
	}
	assertUserSettingsCarried(t, doc, env)
}

func newUserSettingsClaudeRunner(t *testing.T, home string) claudeRunner {
	t.Helper()
	store := claude.Store{
		Dir:            filepath.Join(home, ".subrouter", "codex"),
		SharedStateDir: filepath.Join(home, ".claude"),
	}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}
	return claudeRunner{
		store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard,
		authStatus: func(context.Context, string, string) (*claude.AuthStatus, error) {
			return &claude.AuthStatus{LoggedIn: true}, nil
		},
	}
}

func readRecordedArgs(t *testing.T, recordDir string) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(recordDir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(body)), "\n")
}

// writeRecordingClaudeCLIWithArgs is writeRecordingClaudeCLI that also records
// argv, and tolerates a launch with no --settings file.
func writeRecordingClaudeCLIWithArgs(t *testing.T) string {
	t.Helper()
	recordDir := writeRecordingClaudeCLI(t)
	script := `#!/bin/sh
printf '%s\n' "$@" > "$RECORD_DIR/args"
settings=
previous=
for arg in "$@"; do
  if [ "$previous" = "--settings" ]; then settings=$arg; fi
  previous=$arg
done
env > "$RECORD_DIR/env"
if [ -n "$settings" ]; then cp "$settings" "$RECORD_DIR/settings.json"; fi
`
	binDir := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return recordDir
}

// A direct `sr claude run <profile>` (the profile logs in to Anthropic itself)
// gets the user's settings in a file of its own, with nothing that could route
// it, and the caller's arguments untouched.
func TestClaudeRunDirectProfileCarriesTheUsersSettings(t *testing.T) {
	clearClaudeCacheTTLEnv(t)
	home := sandboxHostConfig(t)
	writeUserClaudeSettings(t, home)
	recordDir := writeRecordingClaudeCLIWithArgs(t)
	runner := newUserSettingsClaudeRunner(t, home)

	if err := runner.runClaude(t.Context(), "work", []string{"--resume", "session-a"}); err != nil {
		t.Fatal(err)
	}
	args := readRecordedArgs(t, recordDir)
	if len(args) != 4 || args[0] != "--settings" || args[2] != "--resume" || args[3] != "session-a" {
		t.Fatalf("direct profile launch args = %q, want --settings <file> --resume session-a", args)
	}
	doc, env := readRecordedLaunchSettings(t, filepath.Join(recordDir, "settings.json"))
	assertUserSettingsCarried(t, doc, env)
	for key := range env {
		for _, routing := range claude.RoutingEnvKeys() {
			if strings.EqualFold(key, routing) {
				t.Fatalf("a direct profile launch got routing key %s from the user's settings: %v", key, env)
			}
		}
	}
}

// A routed profile (its settings.json points at the proxy) already launches
// with the authoritative overlay; the user's settings join it.
func TestClaudeRunRoutedProfileCarriesTheUsersSettings(t *testing.T) {
	clearClaudeCacheTTLEnv(t)
	home := sandboxHostConfig(t)
	writeUserClaudeSettings(t, home)
	recordDir := writeRecordingClaudeCLIWithArgs(t)
	runner := newUserSettingsClaudeRunner(t, home)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	configDir := runner.store.ClaudeConfigDir("work")
	routed := `{"env":{"ANTHROPIC_BASE_URL":"http://localhost:` + port + `"}}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(routed), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := runner.runClaude(t.Context(), "work", nil); err != nil {
		t.Fatal(err)
	}
	doc, env := readRecordedLaunchSettings(t, filepath.Join(recordDir, "settings.json"))
	// The overlay pins a credential-bearing loopback route to its IP and keeps
	// a credential-free one as written; either way it names this listener.
	if base, _ := env["ANTHROPIC_BASE_URL"].(string); !strings.HasSuffix(base, ":"+port) || env["ANTHROPIC_AUTH_TOKEN"] != "" {
		t.Fatalf("routing env is not the verified overlay's: %v", env)
	}
	assertUserSettingsCarried(t, doc, env)
}
