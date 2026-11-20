package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The 2026-09 upstream merge moved `sr claude`'s routing values out of the
// child environment into a private --settings file (launchProxyClaude), so the
// 1-hour prompt cache (PATCHES §21) rides on the credential-free child
// environment instead. These pin that composition end to end: the launched
// Claude Code sees the TTL in its environment, never the proxy token there,
// and the private settings file carries no TTL that would outrank the user's
// own settings.json.

func writeRecordingClaudeCLI(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the recording Claude CLI is a POSIX shell script")
	}
	binDir := filepath.Join(t.TempDir(), "bin")
	recordDir := filepath.Join(t.TempDir(), "records")
	for _, dir := range []string{binDir, recordDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
settings=
previous=
for arg in "$@"; do
  if [ "$previous" = "--settings" ]; then settings=$arg; fi
  previous=$arg
done
env > "$RECORD_DIR/env"
cp "$settings" "$RECORD_DIR/settings.json"
`
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RECORD_DIR", recordDir)
	return recordDir
}

func launchRecordedProxyClaude(t *testing.T, recordDir, configDir string) (env []string, overlay map[string]string) {
	t.Helper()
	runner := srRunner{program: "sr", in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	if err := runner.runProxyClaude(t.Context(), nil, "http://127.0.0.1:31415/v1", "tok", configDir, "", ""); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(recordDir, "env"))
	if err != nil {
		t.Fatal(err)
	}
	settingsBody, err := os.ReadFile(filepath.Join(recordDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(settingsBody, &doc); err != nil {
		t.Fatalf("parse private settings: %v\n%s", err, settingsBody)
	}
	return strings.Split(strings.TrimSpace(string(body)), "\n"), doc.Env
}

func TestClaudeCacheTTLReachesTheLaunchedClaude(t *testing.T) {
	clearClaudeCacheTTLEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	recordDir := writeRecordingClaudeCLI(t)

	env, overlay := launchRecordedProxyClaude(t, recordDir, t.TempDir())
	if overlay["ANTHROPIC_AUTH_TOKEN"] != "tok" {
		t.Fatalf("calibration: the private settings file does not carry the proxy token: %v", overlay)
	}
	if n, val := countEnvKey(env, claudePromptCacheTTLEnv); n != 1 || val != claudeRoutedPromptCacheTTL {
		t.Fatalf("launched Claude Code saw %s %d time(s) with %q, want once with 1h:\n%s",
			claudePromptCacheTTLEnv, n, val, strings.Join(env, "\n"))
	}
	if n, _ := countEnvKey(env, "ANTHROPIC_AUTH_TOKEN"); n != 0 {
		t.Fatalf("the proxy token leaked into the child environment:\n%s", strings.Join(env, "\n"))
	}
	if _, ok := overlay[claudePromptCacheTTLEnv]; ok {
		t.Fatalf("the private --settings file carries %s, which outranks the user's settings.json: %v",
			claudePromptCacheTTLEnv, overlay)
	}

	decided := t.TempDir()
	if err := os.WriteFile(filepath.Join(decided, "settings.json"), []byte(`{"promptCacheTtl":"5m"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env, _ = launchRecordedProxyClaude(t, recordDir, decided)
	if n, _ := countEnvKey(env, claudePromptCacheTTLEnv); n != 0 {
		t.Fatalf("profile with promptCacheTtl got the env key, which would override it:\n%s", strings.Join(env, "\n"))
	}
}

// Without a profile the child is launched with CLAUDE_CONFIG_DIR stripped (a
// routing key), so it reads ~/.claude/settings.json, whatever this process's
// CLAUDE_CONFIG_DIR says. The TTL decision must be read from that same file.
func TestClaudeCacheTTLProfilelessLaunchReadsTheHomeSettings(t *testing.T) {
	clearClaudeCacheTTLEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(elsewhere, "settings.json"), []byte(`{"promptCacheTtl":"5m"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", elsewhere)
	environ := []string{"PATH=/usr/bin", "HOME=" + home, "CLAUDE_CONFIG_DIR=" + elsewhere}

	env := proxyClaudeEnvironment(environ, "http://127.0.0.1:31415/v1", "")
	if n, _ := countEnvKey(env, "CLAUDE_CONFIG_DIR"); n != 0 {
		t.Fatalf("calibration: the profileless child kept CLAUDE_CONFIG_DIR, so it does not read ~/.claude:\n%s",
			strings.Join(env, "\n"))
	}
	if n, val := countEnvKey(env, claudePromptCacheTTLEnv); n != 1 || val != claudeRoutedPromptCacheTTL {
		t.Fatalf("~/.claude records no TTL, yet %s appears %d time(s) with %q (the launcher read %s instead):\n%s",
			claudePromptCacheTTLEnv, n, val, elsewhere, strings.Join(env, "\n"))
	}

	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"env":{"FORCE_PROMPT_CACHING_5M":"1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env = proxyClaudeEnvironment(environ, "http://127.0.0.1:31415/v1", "")
	if n, _ := countEnvKey(env, claudePromptCacheTTLEnv); n != 0 {
		t.Fatalf("~/.claude records a decision, yet the launcher injected the key:\n%s", strings.Join(env, "\n"))
	}
}
