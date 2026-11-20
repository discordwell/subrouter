package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// homeDirCallers is the reviewed set of files allowed to call
// os.UserHomeDir. Paths are slash-separated and relative to the module root.
// FILE-level on purpose: line numbers churn, the set of home-resolving files
// should not.
var homeDirCallers = map[string]bool{
	"cmd/subrouter-transport-observer/golden.go": true,
	// Harvest §29.9.4 (upstream #430, adapted one way). claudeUserConfigPath
	// honours CLAUDE_CONFIG_DIR first, as Claude Code itself does, and falls
	// back to ~/.claude.json only to READ the user's accepted folder trust.
	// Nothing is ever written there: upstream's write-back was dropped so a
	// plain `claude` launch reads exactly what it read before. Tests point
	// HOME/CLAUDE_CONFIG_DIR at temp dirs.
	"cmd/subrouter/claude_proxy_trust.go": true,
	// Upstream merge 2026-11. codexHomeConfiguresNotify honours CODEX_HOME
	// first and only READS config.toml's notify key.
	"cmd/subrouter/codex.go":        true,
	"cmd/subrouter/codex_config.go": true,
	// Upstream merge 2026-11 (#434). defaultCodexHome is the read source of
	// the shared Codex home, which itself lives under storepath.StateDir;
	// codexSourceHome honours CODEX_HOME first.
	"cmd/subrouter/codex_shared_home.go": true,
	// Upstream merge 2026-11. recoveryRoots honours
	// SUBROUTER_CLAUDE_SESSION_ROOT and only reads transcripts for `sr
	// recover`.
	"cmd/subrouter/recovery.go": true,
	// Upstream merge 2026-11. `sr host` is not dispatched in this fork (it
	// installs upstream's release on another machine); the file stays for
	// serverHeading. Its LaunchAgents/Logs paths are test seams.
	"cmd/subrouter/sr_host.go": true,
	// Upstream merge 2026-11 (#429). The status-line hook honours
	// CLAUDE_CONFIG_DIR first and only reads settings.json.
	"cmd/subrouter/sr_session_status.go": true,
	// Upstream merge 2026-11. `sr update`, `sr rollback` and the auto-update
	// trigger are not dispatched in this fork (PATCHES §23, §37.3); the files
	// are kept unwired, like the installers §23 merged.
	"cmd/subrouter/sr_update.go":        true,
	"cmd/subrouter/sr_update_auto.go":   true,
	"cmd/subrouter/install_daemon.go":   true,
	"cmd/subrouter/lifecycle.go":        true,
	"cmd/subrouter/main.go":             true,
	"cmd/subrouter/sr_claude_upload.go": true,
	"cmd/subrouter/sr_cloud.go":         true,
	"cmd/subrouter/sr_daemon.go":        true,
	// Upstream merge 2026-09. kimiSourceHome honours KIMI_CODE_HOME, then the
	// launch environment's HOME, before os.UserHomeDir.
	"cmd/subrouter/sr_native_proxy.go":  true,
	"cmd/subrouter/sr_setup.go":         true,
	"internal/accounts/codex_switch.go": true,
	// Upstream merge 2026-09: agyBinaryPath only stats ~/.local/bin/agy (a
	// read-only binary lookup, no agent config touched).
	"internal/agents/antigravity/store.go": true,
	"internal/agents/claude/store.go":      true,
	// Upstream merge 2026-09: honours KIMI_CODE_HOME first.
	"internal/agents/kimi/store.go":    true,
	"internal/agents/opencode/auth.go": true,
	"internal/agents/pi/auth.go":       true,
	"internal/broker/config.go":        true,
	"internal/storepath/storepath.go":  true,
	"session/store.go":                 true,
}

// TestHomeDirResolutionStaysInsideTheAllowlist walks every non-test Go file
// in the module and fails when the set of files calling os.UserHomeDir
// drifts from the allowlist above. Every new home-resolving path is a new
// way for this codebase to touch a developer's live agent config (~/.claude,
// ~/.codex, ~/.subrouter, ~/.config/subrouter), so it must be either
// justified and allowlisted here, or given an env override so tests and the
// Docker harness can point it somewhere disposable.
func TestHomeDirResolutionStaysInsideTheAllowlist(t *testing.T) {
	root := moduleRoot(t)
	found := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := entry.Name()
		if entry.IsDir() {
			if path == root {
				return nil
			}
			if name == "testdata" || name == "node_modules" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		if !fileCallsUserHomeDir(t, path) {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		found[filepath.ToSlash(relative)] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var added, removed []string
	for path := range found {
		if !homeDirCallers[path] {
			added = append(added, path)
		}
	}
	for path := range homeDirCallers {
		if !found[path] {
			removed = append(removed, path)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	if len(added) > 0 {
		t.Errorf("new os.UserHomeDir callers: %v\n"+
			"Every new home-resolving path is a new way to touch a developer's live agent config. "+
			"Either resolve the path through an existing env-overridable helper (storepath.StateDir, "+
			"broker.DefaultConfigPath, CODEX_HOME/CLAUDE_CONFIG_DIR), or justify the call and add the "+
			"file to homeDirCallers in %s.", added, "cmd/subrouter/homedir_allowlist_test.go")
	}
	if len(removed) > 0 {
		t.Errorf("allowlisted files no longer call os.UserHomeDir: %v\n"+
			"Prune them from homeDirCallers so the allowlist stays an accurate review record.", removed)
	}
}

func fileCallsUserHomeDir(t *testing.T, path string) bool {
	t.Helper()
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	calls := false
	ast.Inspect(file, func(node ast.Node) bool {
		if calls {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		receiver, ok := selector.X.(*ast.Ident)
		if ok && receiver.Name == "os" && selector.Sel.Name == "UserHomeDir" {
			calls = true
			return false
		}
		return true
	})
	return calls
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test working directory")
		}
		dir = parent
	}
}
