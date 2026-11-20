package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Claude Code turns tool search off whenever ANTHROPIC_BASE_URL names a
// non-Anthropic host, and says so: "[ToolSearch:optimistic] disabled:
// ANTHROPIC_BASE_URL=... is not a first-party Anthropic host. Set
// ENABLE_TOOL_SEARCH=true ... if your proxy forwards tool_reference blocks"
// (2.1.283). Every Claude Code session routed through Subrouter therefore ran
// without deferred tools, loading every MCP schema up front (upstream #463).
// These drive the real writer, launcher and setup detector.
//
// The key is spelled out rather than taken from the production constant so the
// file compiles against the tree before the port, where each test fails on the
// missing key rather than on a missing symbol.
const toolSearchKeyForTest = "ENABLE_TOOL_SEARCH"

// clearToolSearchEnv takes ENABLE_TOOL_SEARCH, the TTL keys and CLAUDECODE out
// of this process's environment: the writer and detector read an export, and
// the shell running the tests may be a Claude Code session exporting any of
// them.
func clearToolSearchEnv(t *testing.T) {
	t.Helper()
	clearClaudeCacheTTLEnv(t)
	withoutEnvForTest(t, toolSearchKeyForTest)
}

func TestClaudeToolSearchReachesTheLaunchedClaude(t *testing.T) {
	clearToolSearchEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	recordDir := writeRecordingClaudeCLI(t)

	env, overlay := launchRecordedProxyClaude(t, recordDir, t.TempDir())
	if overlay["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:31415" {
		t.Fatalf("calibration: the launch was not routed through the proxy: %v", overlay)
	}
	if n, val := countEnvKey(env, toolSearchKeyForTest); n != 1 || val != "true" {
		t.Fatalf("routed Claude Code saw %s %d time(s) with %q, want once with \"true\"; without it Claude Code turns tool search off behind a non-Anthropic base URL:\n%s",
			toolSearchKeyForTest, n, val, strings.Join(env, "\n"))
	}
	// In the private --settings file the value would outrank the user's and the
	// profile's settings.json, where a user's own "false" has to win.
	if _, ok := overlay[toolSearchKeyForTest]; ok {
		t.Fatalf("the private --settings file carries %s, which outranks the user's settings.json: %v",
			toolSearchKeyForTest, overlay)
	}

	for _, value := range []string{"false", "auto:5", ""} {
		t.Setenv(toolSearchKeyForTest, value)
		env, _ = launchRecordedProxyClaude(t, recordDir, t.TempDir())
		if n, val := countEnvKey(env, toolSearchKeyForTest); n != 1 || val != value {
			t.Fatalf("the user's own %s=%q became %d entries, last %q:\n%s",
				toolSearchKeyForTest, value, n, val, strings.Join(env, "\n"))
		}
	}
}

// A direct profile launch (no base URL) talks to Anthropic, where Claude Code
// enables tool search itself; the launcher adds nothing there.
func TestClaudeToolSearchIsOnlyForRoutedLaunches(t *testing.T) {
	clearToolSearchEnv(t)
	direct := claudeSettingsChildEnvironment([]string{"PATH=/usr/bin"}, "", "/isolated/profile")
	if n, _ := countEnvKey(direct, toolSearchKeyForTest); n != 0 {
		t.Fatalf("direct profile launch got %s:\n%s", toolSearchKeyForTest, strings.Join(direct, "\n"))
	}
	routed := claudeSettingsChildEnvironment([]string{"PATH=/usr/bin"}, "http://127.0.0.1:31415", "/isolated/profile")
	if n, val := countEnvKey(routed, toolSearchKeyForTest); n != 1 || val != "true" {
		t.Fatalf("routed profile launch: %s %d time(s) with %q, want once with \"true\":\n%s",
			toolSearchKeyForTest, n, val, strings.Join(routed, "\n"))
	}
}

// In a Subrouter profile directory (profileOwned) the writer asks for tool
// search, and a value already in the file, any value, is the user's decision.
// The profile's settings.json is what a launch pointed at the profile reads.
func TestClaudeToolSearchWriterAsksForTrueUnlessTheUserChose(t *testing.T) {
	clearToolSearchEnv(t)
	dir := filepath.Join(t.TempDir(), "claude")
	path := filepath.Join(dir, "settings.json")
	if err := writeClaudeProxyEnvCanonical(dir, "http://127.0.0.1:31415", "tok", true); err != nil {
		t.Fatal(err)
	}
	_, env, first := readClaudeSettingsForTest(t, path)
	if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:31415" {
		t.Fatalf("calibration: routing was not written:\n%s", first)
	}
	if env[toolSearchKeyForTest] != "true" {
		t.Fatalf("a launch on this routed profile reads this file, yet %s = %v:\n%s",
			toolSearchKeyForTest, env[toolSearchKeyForTest], first)
	}
	// Once written the key is the decision: a re-run is a byte-identical no-op.
	if err := writeClaudeProxyEnvCanonical(dir, "http://127.0.0.1:31415", "tok", true); err != nil {
		t.Fatal(err)
	}
	if _, _, again := readClaudeSettingsForTest(t, path); !bytes.Equal(first, again) {
		t.Fatalf("re-run rewrote the file:\n%s\n---\n%s", first, again)
	}

	for _, value := range []string{"false", "auto:5", ""} {
		dir := filepath.Join(t.TempDir(), "claude")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		seed := `{"env":{"` + toolSearchKeyForTest + `":"` + value + `"}}`
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(seed), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := writeClaudeProxyEnvCanonical(dir, "http://127.0.0.1:31415", "tok", true); err != nil {
			t.Fatal(err)
		}
		if _, env, body := readClaudeSettingsForTest(t, filepath.Join(dir, "settings.json")); env[toolSearchKeyForTest] != value {
			t.Fatalf("the user's %s=%q did not survive a write:\n%s", toolSearchKeyForTest, value, body)
		}
	}
}

// An export is the user's decision only outside Claude Code and only when it
// differs from the "true" the writer would write: settings.json env beats a
// shell export for a terminal launch, so writing "true" over "false" would
// override them, while an exported "true" is no conflict and the file is the
// only channel that reaches the desktop app.
func TestClaudeToolSearchWriterRespectsAnExportedDecision(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     [][2]string
		decided bool
	}{
		{name: "outside, nothing exported", decided: false},
		{name: "outside, exported true", env: [][2]string{{toolSearchKeyForTest, "true"}}, decided: false},
		{name: "outside, exported false", env: [][2]string{{toolSearchKeyForTest, "false"}}, decided: true},
		{name: "outside, exported empty", env: [][2]string{{toolSearchKeyForTest, ""}}, decided: true},
		{name: "inside Claude Code, false", env: [][2]string{{"CLAUDECODE", "1"}, {toolSearchKeyForTest, "false"}}, decided: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearToolSearchEnv(t)
			for _, kv := range tc.env {
				t.Setenv(kv[0], kv[1])
			}
			dir := filepath.Join(t.TempDir(), "claude")
			if err := writeClaudeProxyEnvCanonical(dir, "http://127.0.0.1:31415", "tok", true); err != nil {
				t.Fatal(err)
			}
			_, env, body := readClaudeSettingsForTest(t, filepath.Join(dir, "settings.json"))
			got, ok := env[toolSearchKeyForTest]
			switch {
			case tc.decided && ok:
				t.Fatalf("the user's own export is a decision, yet the writer put %s=%v in settings.json, which beats it:\n%s",
					toolSearchKeyForTest, got, body)
			case !tc.decided && got != "true":
				t.Fatalf("no decision of the user's here, yet %s = %v (present %v), want \"true\":\n%s",
					toolSearchKeyForTest, got, ok, body)
			}
		})
	}
}

// The user's own config dir (defaultClaudeConfigDir) is what a plain `claude`
// reads, and client-config writers there are a known brick class, so until the
// owner signs off on the routed-session canary no cutover writer puts
// ENABLE_TOOL_SEARCH there and setup does not report its absence as pending
// (claudeRoutedToolSearch). This drives the real `sr setup` writer and the real
// detector against a sandboxed HOME: a routed TTL-era file is done and
// untouched, and a fresh cutover writes routing and the TTL but not the key.
// Only the `sr claude` launch environment (withClaudeToolSearch, above) and
// Subrouter profile directories carry it.
func TestClaudeToolSearchStaysOutOfTheUserClaudeConfig(t *testing.T) {
	clearToolSearchEnv(t)
	home := sandboxHostConfig(t)
	dir := filepath.Join(home, ".claude")
	path := filepath.Join(dir, "settings.json")
	root := codexProxyRootURL(localBaseURL())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ttlEra := `{"env":{"ANTHROPIC_BASE_URL":"` + root + `","ANTHROPIC_AUTH_TOKEN":"subrouter","ANTHROPIC_CUSTOM_HEADERS":"X-Subrouter-Agent: claude","CLAUDE_CODE_PROMPT_CACHE_TTL":"1h"}}`
	if err := os.WriteFile(path, []byte(ttlEra), 0o600); err != nil {
		t.Fatal(err)
	}
	if routed, missing := claudeRoutedSettingsForLocal(path, root); !routed || len(missing) != 0 {
		t.Fatalf("routed TTL-era ~/.claude: routed=%v missing=%v, want routed with nothing missing; setup would otherwise re-open every finished cutover to add %s to the file plain `claude` reads",
			routed, missing, toolSearchKeyForTest)
	}
	written, err := writeClaudeProxyEnvForLocal()
	if err != nil {
		t.Fatal(err)
	}
	if written != path {
		t.Fatalf("calibration: setup wrote %s, want the sandboxed %s", written, path)
	}
	_, env, body := readClaudeSettingsForTest(t, path)
	if _, ok := env[toolSearchKeyForTest]; ok {
		t.Fatalf("setup's write put %s into the settings.json plain `claude` reads:\n%s", toolSearchKeyForTest, body)
	}

	// A fresh cutover: routing and the TTL, still no tool search.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := writeClaudeProxyEnvForLocal(); err != nil {
		t.Fatal(err)
	}
	_, env, body = readClaudeSettingsForTest(t, path)
	if env["ANTHROPIC_BASE_URL"] != root || env["CLAUDE_CODE_PROMPT_CACHE_TTL"] != "1h" {
		t.Fatalf("calibration: the fresh cutover did not route or ask for the TTL:\n%s", body)
	}
	if _, ok := env[toolSearchKeyForTest]; ok {
		t.Fatalf("a fresh cutover put %s into the settings.json plain `claude` reads:\n%s", toolSearchKeyForTest, body)
	}
	if routed, configured := claudeSettingsForLocal(path, root); !routed || !configured {
		t.Fatalf("after setup's own write: routed=%v configured=%v, want both:\n%s", routed, configured, body)
	}

	// CLAUDE_CONFIG_DIR names the user's config dir too: same rule.
	custom := filepath.Join(t.TempDir(), "custom-claude")
	t.Setenv("CLAUDE_CONFIG_DIR", custom)
	if _, err := writeClaudeProxyEnvForLocal(); err != nil {
		t.Fatal(err)
	}
	if _, env, body := readClaudeSettingsForTest(t, filepath.Join(custom, "settings.json")); env[toolSearchKeyForTest] != nil {
		t.Fatalf("setup put %s into $CLAUDE_CONFIG_DIR/settings.json:\n%s", toolSearchKeyForTest, body)
	}
}
