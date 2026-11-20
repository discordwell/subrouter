//go:build !windows

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Upstream #430's tests, adapted to the one-way sync: the user's .claude.json
// is only ever read, so every case that upstream used to prove a write-back
// now proves the user's file is byte-identical afterwards.

func writeClaudeConfig(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readClaudeConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func projectFlag(config map[string]any, project, flag string) any {
	projects, _ := config["projects"].(map[string]any)
	fields, _ := projects[project].(map[string]any)
	return fields[flag]
}

func TestSyncClaudeProjectTrustCopiesAcceptedTrustIntoTheProxyOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	userPath := claudeUserConfigPath()
	if userPath != filepath.Join(home, ".claude.json") {
		t.Fatalf("user config path = %q", userPath)
	}
	writeClaudeConfig(t, userPath, map[string]any{
		"numStartups":  42,
		"oauthAccount": map[string]any{"emailAddress": "leo@example.com"},
		"projects": map[string]any{
			"/work/trusted":   map[string]any{"hasTrustDialogAccepted": true, "hasCompletedProjectOnboarding": true, "allowedTools": []string{"Bash"}},
			"/work/untrusted": map[string]any{"hasTrustDialogAccepted": false},
		},
	})
	userBefore, _ := os.ReadFile(userPath)
	proxyDir := filepath.Join(home, ".subrouter", "codex", "claude-proxy", "scope")
	proxyPath := filepath.Join(proxyDir, ".claude.json")
	writeClaudeConfig(t, proxyPath, map[string]any{
		"userID": "proxy-user",
		"projects": map[string]any{
			"/work/untrusted":  map[string]any{"history": []string{"x"}},
			"/work/proxy-only": map[string]any{"hasTrustDialogAccepted": true},
		},
	})

	if err := syncClaudeProjectTrustToProxy(userPath, proxyDir); err != nil {
		t.Fatal(err)
	}

	proxy := readClaudeConfig(t, proxyPath)
	if projectFlag(proxy, "/work/trusted", "hasTrustDialogAccepted") != true || projectFlag(proxy, "/work/trusted", "hasCompletedProjectOnboarding") != true {
		t.Fatalf("user trust did not reach the proxy config: %v", proxy["projects"])
	}
	if projectFlag(proxy, "/work/trusted", "allowedTools") != nil {
		t.Fatal("only trust flags may be copied, not tool permissions")
	}
	if got := projectFlag(proxy, "/work/untrusted", "hasTrustDialogAccepted"); got != nil {
		t.Fatalf("untrusted folder gained trust in the proxy: %v", got)
	}
	if proxy["userID"] != "proxy-user" || projectFlag(proxy, "/work/untrusted", "history") == nil ||
		projectFlag(proxy, "/work/proxy-only", "hasTrustDialogAccepted") != true {
		t.Fatalf("proxy config lost its own fields: %v", proxy)
	}
	// The adaptation: trust accepted inside a proxy session stays there. The
	// user's file is what every plain `claude` reads, and sr never writes it.
	if userAfter, _ := os.ReadFile(userPath); !bytes.Equal(userBefore, userAfter) {
		t.Fatalf("the user's .claude.json changed:\n%s\n---\n%s", userBefore, userAfter)
	}

	// A second sync is a no-op.
	proxyBefore, _ := os.ReadFile(proxyPath)
	if err := syncClaudeProjectTrustToProxy(userPath, proxyDir); err != nil {
		t.Fatal(err)
	}
	if proxyAfter, _ := os.ReadFile(proxyPath); !bytes.Equal(proxyBefore, proxyAfter) {
		t.Fatal("an idempotent sync rewrote the proxy config")
	}
}

func TestSyncClaudeProjectTrustCreatesProxyConfigButNeverUserConfig(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, "custom-claude")
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	userPath := claudeUserConfigPath()
	if userPath != filepath.Join(configDir, ".claude.json") {
		t.Fatalf("CLAUDE_CONFIG_DIR not respected: %q", userPath)
	}
	writeClaudeConfig(t, userPath, map[string]any{"projects": map[string]any{"/a": map[string]any{"hasTrustDialogAccepted": true}}})
	proxyDir := filepath.Join(home, "proxy")
	if err := syncClaudeProjectTrustToProxy(userPath, proxyDir); err != nil {
		t.Fatal(err)
	}
	if projectFlag(readClaudeConfig(t, filepath.Join(proxyDir, ".claude.json")), "/a", "hasTrustDialogAccepted") != true {
		t.Fatal("a fresh proxy config should start with the user's trust")
	}

	missingUser := filepath.Join(home, "nobody", ".claude.json")
	writeClaudeConfig(t, filepath.Join(proxyDir, ".claude.json"), map[string]any{"projects": map[string]any{"/b": map[string]any{"hasTrustDialogAccepted": true}}})
	if err := syncClaudeProjectTrustToProxy(missingUser, proxyDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(missingUser)); !os.IsNotExist(err) {
		t.Fatal("sr must never create the user's Claude config or its directory")
	}
}

func TestSyncClaudeProjectTrustLeavesLockedConfigAlone(t *testing.T) {
	home := t.TempDir()
	userPath := filepath.Join(home, ".claude.json")
	writeClaudeConfig(t, userPath, map[string]any{"projects": map[string]any{"/a": map[string]any{"hasTrustDialogAccepted": true}}})
	proxyDir := filepath.Join(home, "proxy")
	proxyPath := filepath.Join(proxyDir, ".claude.json")
	writeClaudeConfig(t, proxyPath, map[string]any{"projects": map[string]any{"/a": map[string]any{}}})
	if err := os.Mkdir(proxyPath+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syncClaudeProjectTrustToProxy(userPath, proxyDir); err != nil {
		t.Fatal(err)
	}
	if projectFlag(readClaudeConfig(t, proxyPath), "/a", "hasTrustDialogAccepted") != nil {
		t.Fatal("sr wrote a config while its lock was held")
	}
}

func TestSyncClaudeProjectTrustEdgeCases(t *testing.T) {
	home := t.TempDir()
	userPath := filepath.Join(home, ".claude.json")
	writeClaudeConfig(t, userPath, map[string]any{"projects": map[string]any{
		"/revoked": map[string]any{"hasTrustDialogAccepted": true},
		"/new":     map[string]any{"hasTrustDialogAccepted": true},
	}})
	// The proxy home's config lives elsewhere and .claude.json links to it.
	proxyDir := filepath.Join(home, "proxy")
	real := filepath.Join(home, "dotfiles", "proxy-claude.json")
	writeClaudeConfig(t, real, map[string]any{"projects": map[string]any{
		"/revoked": map[string]any{"hasTrustDialogAccepted": false},
	}})
	if err := os.MkdirAll(proxyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(proxyDir, ".claude.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := syncClaudeProjectTrustToProxy(userPath, proxyDir); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the config symlink was replaced by a file")
	}
	proxy := readClaudeConfig(t, real)
	if projectFlag(proxy, "/revoked", "hasTrustDialogAccepted") != false {
		t.Fatal("the user's trust overwrote an explicit false in the proxy home")
	}
	if projectFlag(proxy, "/new", "hasTrustDialogAccepted") != true {
		t.Fatal("the user's trust was not written through the symlink")
	}
	if _, err := os.Stat(real + ".lock"); !os.IsNotExist(err) {
		t.Fatal("the config lock was not released")
	}

	// "projects": null must not panic, on either side.
	writeClaudeConfig(t, real, map[string]any{"projects": nil})
	if err := syncClaudeProjectTrustToProxy(userPath, proxyDir); err != nil {
		t.Fatal(err)
	}
	if projectFlag(readClaudeConfig(t, real), "/new", "hasTrustDialogAccepted") != true {
		t.Fatal("trust did not reach a proxy config whose projects were null")
	}
	writeClaudeConfig(t, userPath, map[string]any{"projects": nil})
	if err := syncClaudeProjectTrustToProxy(userPath, proxyDir); err != nil {
		t.Fatal(err)
	}
}
