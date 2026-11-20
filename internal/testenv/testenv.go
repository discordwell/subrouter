// Package testenv is test-process setup shared by more than one package's
// TestMain. Only _test.go files import it, so it never reaches a binary.
package testenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// HomeOverrides names the variables that relocate a store the code otherwise
// derives from HOME: the state dir (every pooled Codex and Claude record), the
// cloud config, the Codex CLI home (auth.json, config.toml) and the Claude Code
// config dir (settings.json, .claude.json). Most tests isolate themselves with
// t.Setenv("HOME", t.TempDir()) alone. An exported value of any of these beats
// that fallback, so on a host that exports one, those tests read and write the
// live store instead of their temp dir. With a read-only decoy exported, 159
// tests across cmd/subrouter, internal/accounts and internal/proxy failed on
// their first write into it (PATCHES.md §45). A Superset shell exports
// CODEX_HOME, so a bare go test there writes test credentials into that Codex
// home's auth.json. Every subrouter-wrapped Claude Code session exports
// CLAUDE_CONFIG_DIR, so a bare go test there rewrote that profile's
// settings.json with a token no daemon accepts.
var HomeOverrides = []string{"SUBROUTER_STATE_DIR", "SUBROUTER_CLOUD_CONFIG", "CODEX_HOME", "CLAUDE_CONFIG_DIR"}

// IsolateHome unsets HomeOverrides and points HOME at a new empty directory for
// the rest of the process, and returns the func that removes that directory. A
// package whose tests reach one of those stores calls it from TestMain, around
// m.Run, so a test's HOME decides where those stores live, and a test that sets
// no HOME finds an empty one. The developer's HOME reached such tests before:
// internal/proxy builds an AccountRef on agentclaude.DefaultStore(), so a test
// that set no HOME listed the live Claude profiles, and its failure message
// printed their refresh tokens (PATCHES.md §61). A test that wants HOME or an
// override still sets it with t.Setenv.
func IsolateHome() (remove func()) {
	for _, key := range HomeOverrides {
		_ = os.Unsetenv(key)
	}
	home, err := os.MkdirTemp("", "subrouter-test-home-")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("HOME", home); err != nil {
		panic(err)
	}
	return func() { _ = os.RemoveAll(home) }
}

const (
	probeEnv     = "SUBROUTER_TESTENV_HOME_OVERRIDE_PROBE"
	probeHomeEnv = "SUBROUTER_TESTENV_DECOY_HOME"
)

// CheckHomeIsolated is the whole body of a test that proves its package's
// TestMain calls IsolateHome. CI exports no override and has no live store in
// HOME, so a check made in place could not fail there. Instead it re-runs the
// test binary for this one test with HOME and every override exported at a
// decoy, the way a developer shell does, and fails unless that run passes.
// Inside the re-run it is the probe: it fails if an override or the decoy HOME
// survived TestMain, and otherwise sets HOME to a temp dir and requires every
// path resolve returns to lie under it.
func CheckHomeIsolated(t *testing.T, resolve func() (map[string]string, error)) {
	t.Helper()
	if os.Getenv(probeEnv) == "1" {
		for _, key := range HomeOverrides {
			if value, ok := os.LookupEnv(key); ok {
				t.Fatalf("%s=%q reached a test; TestMain must call testenv.IsolateHome", key, value)
			}
		}
		if home := os.Getenv("HOME"); home == os.Getenv(probeHomeEnv) {
			t.Fatalf("HOME=%q, the developer's, reached a test; TestMain must call testenv.IsolateHome", home)
		}
		home := t.TempDir()
		t.Setenv("HOME", home)
		paths, err := resolve()
		if err != nil {
			t.Fatal(err)
		}
		for name, path := range paths {
			if !strings.HasPrefix(path, home+string(os.PathSeparator)) {
				t.Fatalf("%s = %q, want a path under the test's HOME %q", name, path, home)
			}
		}
		return
	}

	decoy := t.TempDir()
	probe := exec.Command(os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1", "-test.v")
	probe.Env = append(os.Environ(),
		probeEnv+"=1",
		"HOME="+filepath.Join(decoy, "home"),
		probeHomeEnv+"="+filepath.Join(decoy, "home"),
		"SUBROUTER_STATE_DIR="+filepath.Join(decoy, "state"),
		"SUBROUTER_CLOUD_CONFIG="+filepath.Join(decoy, "config", "cloud.json"),
		"CODEX_HOME="+filepath.Join(decoy, "codex"),
		"CLAUDE_CONFIG_DIR="+filepath.Join(decoy, "claude"),
	)
	out, err := probe.CombinedOutput()
	if err != nil {
		t.Fatalf("a test run with HOME and the overrides exported saw them: %v\n%s", err, out)
	}
	// A -test.run that matched nothing also exits 0.
	if !strings.Contains(string(out), "--- PASS: "+t.Name()) {
		t.Fatalf("the probe run did not report %s passing:\n%s", t.Name(), out)
	}
	if entries, err := os.ReadDir(decoy); err != nil || len(entries) != 0 {
		t.Fatalf("the probe run wrote into the decoy: entries=%v err=%v", entries, err)
	}
}
