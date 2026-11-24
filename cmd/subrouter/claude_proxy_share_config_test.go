//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/discordwell/subrouter/internal/agents/claude"
)

// `sr claude proxy` runs Claude Code in claude-proxy/<hash>, which shared only
// history with ~/.claude, so proxy sessions started without the user's skills,
// subagents, commands, CLAUDE.md and key bindings (upstream #441). This drives
// prepareClaudeProxySharedState, the step both the local and the server proxy
// route run before launch, against the real default store in a sandboxed
// HOME. It uses only symbols that exist before the port, so on the pre-port
// tree it fails on the missing links.
func TestClaudeProxyHomeSharesTheUsersSkillsAndMemory(t *testing.T) {
	home := sandboxHostConfig(t)
	user := filepath.Join(home, ".claude")
	for _, dir := range []string{filepath.Join(user, "skills", "ci-wait"), filepath.Join(user, "agents")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		"CLAUDE.md":     "user memory\n",
		"settings.json": `{"apiKeyHelper":"leak","hooks":{}}`,
	} {
		if err := os.WriteFile(filepath.Join(user, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	storeDir := claude.DefaultStore().Dir
	proxyHome := claudeProxyConfigDir(storeDir, "local", "")
	if err := os.MkdirAll(proxyHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proxyHome, ".credentials.json"), []byte("proxy-secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := prepareClaudeProxySharedState(proxyHome, storeDir); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(filepath.Join(proxyHome, "projects")); err != nil || got != filepath.Join(user, "projects") {
		t.Fatalf("calibration: history is not shared (%q, %v), so this is not the real default store", got, err)
	}
	for _, name := range []string{"skills", "agents", "CLAUDE.md"} {
		got, err := os.Readlink(filepath.Join(proxyHome, name))
		if err != nil || got != filepath.Join(user, name) {
			t.Fatalf("proxy home %s = %q (%v), want a link to the user's %s", name, got, err, filepath.Join(user, name))
		}
	}
	// settings.json reaches launches only through the filtered --settings
	// overlay; a link would hand Claude the user's apiKeyHelper and routing.
	if _, err := os.Lstat(filepath.Join(proxyHome, "settings.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("settings.json linked or created in the proxy home: %v", err)
	}
	for _, name := range []string{"commands", "keybindings.json"} {
		if _, err := os.Lstat(filepath.Join(proxyHome, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s created without a user copy: %v", name, err)
		}
	}
	if body, _ := os.ReadFile(filepath.Join(proxyHome, ".credentials.json")); string(body) != "proxy-secret" {
		t.Fatalf("the proxy home's credential changed: %q", body)
	}
	if _, err := os.Stat(filepath.Join(user, ".credentials.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a credential appeared in the user's home: %v", err)
	}

	// A hermetic store never attaches to the user's home.
	hermetic := t.TempDir()
	if err := prepareClaudeProxySharedState(hermetic, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(hermetic); len(entries) != 0 {
		t.Fatalf("a hermetic store's proxy home got %d entries", len(entries))
	}
}
