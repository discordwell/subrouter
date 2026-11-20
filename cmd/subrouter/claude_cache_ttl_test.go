package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Claude Code treats ANTHROPIC_AUTH_TOKEN as an API-key client, and an API-key
// client asks for the 5-minute prompt cache on the main thread; a signed-in
// subscriber gets the 1-hour one. Measured 2026-09-24 through subrouter: 0
// one-hour cache tokens until CLAUDE_CODE_PROMPT_CACHE_TTL=1h was set. Every
// place subrouter writes or launches a Claude config therefore asks for 1h,
// unless the user already decided (PATCHES §21).

func readClaudeSettingsForTest(t *testing.T, path string) (map[string]any, map[string]any, []byte) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, body)
	}
	env, _ := doc["env"].(map[string]any)
	return doc, env, body
}

// withoutEnvForTest removes key from this process's environment for the rest
// of t. t.Setenv records the original first, so its cleanup puts it back (or
// unsets it again) when t ends.
func withoutEnvForTest(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

// clearClaudeCacheTTLEnv takes both main-thread TTL variables, and CLAUDECODE,
// out of this process's environment. The writers and the detector read them (a
// TTL the user exported outside Claude Code is a decision), and the shell that
// runs these tests may export CLAUDE_CODE_PROMPT_CACHE_TTL itself or be a
// Claude Code tool subprocess (CLAUDECODE=1), so every test that expects the
// writer to add the key, or to leave it out, has to control them rather than
// inherit them.
func clearClaudeCacheTTLEnv(t *testing.T) {
	t.Helper()
	withoutEnvForTest(t, claudePromptCacheTTLEnv)
	withoutEnvForTest(t, claudeForcePromptCaching5mEnv)
	withoutEnvForTest(t, "CLAUDECODE")
	// The detector's "configured" also needs the tool-search decision, which an
	// export can supply as well.
	withoutEnvForTest(t, claudeToolSearchEnv)
}

func TestClaudeCacheTTLWriterAsksForOneHourOnAFreshFile(t *testing.T) {
	clearClaudeCacheTTLEnv(t)
	dir := filepath.Join(t.TempDir(), "fresh")
	if err := writeClaudeProxyEnv(dir, "http://127.0.0.1:31415", "tok"); err != nil {
		t.Fatal(err)
	}
	_, env, body := readClaudeSettingsForTest(t, filepath.Join(dir, "settings.json"))
	if got, ok := env[claudePromptCacheTTLEnv]; !ok || got != "1h" {
		t.Fatalf("%s = %v (present %v), want \"1h\":\n%s", claudePromptCacheTTLEnv, got, ok, body)
	}
	// Subagents stay on 5m by design: a 1h write costs 2x base, 5m 1.25x.
	if _, ok := env["CLAUDE_CODE_SUBAGENT_PROMPT_CACHE_TTL"]; ok {
		t.Fatalf("the writer set the subagent TTL, which must stay the user's:\n%s", body)
	}
}

// Env beats the promptCacheTtl setting inside Claude Code, so writing the env
// key over a user's setting would override them. Any recorded decision wins,
// including an empty value.
func TestClaudeCacheTTLWriterKeepsTheUsersDecision(t *testing.T) {
	cases := []struct {
		name     string
		seed     string
		wantKey  bool
		wantVal  string
		checkDoc func(t *testing.T, doc map[string]any)
	}{
		{name: "env 5m kept", seed: `{"env":{"CLAUDE_CODE_PROMPT_CACHE_TTL":"5m"}}`, wantKey: true, wantVal: "5m"},
		{name: "env empty kept", seed: `{"env":{"CLAUDE_CODE_PROMPT_CACHE_TTL":""}}`, wantKey: true, wantVal: ""},
		{name: "promptCacheTtl setting respected", seed: `{"promptCacheTtl":"5m"}`, wantKey: false,
			checkDoc: func(t *testing.T, doc map[string]any) {
				if doc["promptCacheTtl"] != "5m" {
					t.Errorf("promptCacheTtl = %v, want the user's 5m", doc["promptCacheTtl"])
				}
			}},
		{name: "FORCE_PROMPT_CACHING_5M respected", seed: `{"env":{"FORCE_PROMPT_CACHING_5M":"1"}}`, wantKey: false},
		{name: "subagent key is not a main-thread decision", seed: `{"env":{"CLAUDE_CODE_SUBAGENT_PROMPT_CACHE_TTL":"5m"}}`, wantKey: true, wantVal: "1h",
			checkDoc: func(t *testing.T, doc map[string]any) {
				env, _ := doc["env"].(map[string]any)
				if env["CLAUDE_CODE_SUBAGENT_PROMPT_CACHE_TTL"] != "5m" {
					t.Errorf("subagent TTL = %v, want the user's 5m", env["CLAUDE_CODE_SUBAGENT_PROMPT_CACHE_TTL"])
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearClaudeCacheTTLEnv(t)
			dir := t.TempDir()
			path := filepath.Join(dir, "settings.json")
			if err := os.WriteFile(path, []byte(tc.seed), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := writeClaudeProxyEnv(dir, "http://127.0.0.1:31415", "tok"); err != nil {
				t.Fatal(err)
			}
			doc, env, body := readClaudeSettingsForTest(t, path)
			got, ok := env[claudePromptCacheTTLEnv]
			if ok != tc.wantKey || (ok && got != tc.wantVal) {
				t.Fatalf("%s = %v (present %v), want present %v value %q:\n%s",
					claudePromptCacheTTLEnv, got, ok, tc.wantKey, tc.wantVal, body)
			}
			if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:31415" {
				t.Fatalf("routing was not written:\n%s", body)
			}
			if tc.checkDoc != nil {
				tc.checkDoc(t, doc)
			}
		})
	}
}

// Bringing an already-routed file up to date is one real change; after that a
// re-run must stay a byte-identical, mtime-preserving no-op (PATCHES §6).
func TestClaudeCacheTTLWriterRerunIsByteIdentical(t *testing.T) {
	clearClaudeCacheTTLEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	seed := `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:31415","ANTHROPIC_AUTH_TOKEN":"tok"}}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeProxyEnv(dir, "http://127.0.0.1:31415", "tok"); err != nil {
		t.Fatal(err)
	}
	_, env, first := readClaudeSettingsForTest(t, path)
	if env[claudePromptCacheTTLEnv] != "1h" {
		t.Fatalf("an existing routed file was not brought up to date:\n%s", first)
	}
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeProxyEnv(dir, "http://127.0.0.1:31415", "tok"); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("re-run changed content:\nfirst:  %s\nsecond: %s", first, second)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(past) {
		t.Fatalf("re-run rewrote settings.json (mtime %v, want %v)", info.ModTime(), past)
	}
}

// `sr setup` skips items already marked "=", so a routed file without the TTL
// must read as not configured, or existing users never get the key.
func TestClaudeCacheTTLDetectorRequiresTheDecision(t *testing.T) {
	clearClaudeCacheTTLEnv(t)
	home := sandboxHostConfig(t)
	dir := filepath.Join(home, ".claude")
	path := filepath.Join(dir, "settings.json")
	root := codexProxyRootURL(localBaseURL())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	routedOnly := `{"env":{"ANTHROPIC_BASE_URL":"` + root + `","ANTHROPIC_AUTH_TOKEN":"subrouter"}}`
	if err := os.WriteFile(path, []byte(routedOnly), 0o600); err != nil {
		t.Fatal(err)
	}
	// Calibration: the file really does route (the old detector said "done").
	if !clientRoutesToLocal(path, root) {
		t.Fatal("fixture does not route to the local daemon; the test proves nothing")
	}
	if routed, configured := claudeSettingsForLocal(path, root); !routed || configured {
		t.Fatalf("routed-without-TTL: routed=%v configured=%v, want routed and NOT configured", routed, configured)
	}

	if _, err := writeClaudeProxyEnvForLocal(); err != nil {
		t.Fatal(err)
	}
	if routed, configured := claudeSettingsForLocal(path, root); !routed || !configured {
		body, _ := os.ReadFile(path)
		t.Fatalf("after setup's own write: routed=%v configured=%v, want both:\n%s", routed, configured, body)
	}

	// The user's own 5m is a decision: configured, and it survives a write.
	// ENABLE_TOOL_SEARCH is decided too, so only the TTL is under test.
	userChoice := `{"env":{"ANTHROPIC_BASE_URL":"` + root + `","ANTHROPIC_AUTH_TOKEN":"subrouter","CLAUDE_CODE_PROMPT_CACHE_TTL":"5m","ENABLE_TOOL_SEARCH":"true"}}`
	if err := os.WriteFile(path, []byte(userChoice), 0o600); err != nil {
		t.Fatal(err)
	}
	if routed, configured := claudeSettingsForLocal(path, root); !routed || !configured {
		t.Fatalf("routed with the user's 5m: routed=%v configured=%v, want both", routed, configured)
	}
	if _, err := writeClaudeProxyEnvForLocal(); err != nil {
		t.Fatal(err)
	}
	if _, env, body := readClaudeSettingsForTest(t, path); env[claudePromptCacheTTLEnv] != "5m" {
		t.Fatalf("the user's 5m did not survive a write:\n%s", body)
	}

	// Not routed at all stays not routed.
	if err := os.WriteFile(path, []byte(`{"env":{"CLAUDE_CODE_PROMPT_CACHE_TTL":"1h"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if routed, configured := claudeSettingsForLocal(path, root); routed || configured {
		t.Fatalf("unrouted file: routed=%v configured=%v, want neither", routed, configured)
	}
}

// The routed-but-no-TTL case is a modification, and the review screen has to
// say what it changes rather than repeat "route through Subrouter".
func TestClaudeCacheTTLPlanItemNamesTheCacheForRoutedUsers(t *testing.T) {
	in := emptyPoolInput()
	in.LocalCodexAccounts = 1
	in.LocalClaudeAccounts = 1
	in.ClaudeRouted = true
	in.ClaudeConfigured = false
	item, ok := buildSetupPlan(in).find(actionConfigClaude)
	if !ok {
		t.Fatal("no Claude config item")
	}
	if item.Change == "=" {
		t.Fatalf("routed-without-TTL was marked done: %+v", item)
	}
	if !strings.Contains(item.Summary+item.Detail, "1-hour prompt cache") ||
		!strings.Contains(item.Detail, claudePromptCacheTTLEnv) {
		t.Fatalf("item does not name the cache change: %+v", item)
	}
	// The default routing item also mentions the 1-hour cache and the key, so
	// only the routed-specific fields prove the rewording branch ran.
	if item.Summary != "Ask Claude Code for the 1-hour prompt cache" || item.Change != "~" {
		t.Fatalf("routed-without-TTL got Summary %q Change %q, want the cache item marked \"~\": %+v",
			item.Summary, item.Change, item)
	}

	in.ClaudeConfigured = true
	if item, _ := buildSetupPlan(in).find(actionConfigClaude); item.Change != "=" {
		t.Fatalf("fully configured was not marked done: %+v", item)
	}

	// Not routed at all is still the routing change, not the cache item.
	in.ClaudeRouted = false
	in.ClaudeConfigured = false
	item, ok = buildSetupPlan(in).find(actionConfigClaude)
	if !ok {
		t.Fatal("no Claude config item for an unrouted client")
	}
	if item.Summary != "Configure Claude Code to route through Subrouter" || item.Change == "~" || item.Change == "=" {
		t.Fatalf("unrouted client got Summary %q Change %q, want the routing item: %+v", item.Summary, item.Change, item)
	}
}

func countEnvKey(env []string, key string) (int, string) {
	n, last := 0, ""
	for _, kv := range env {
		if name, value, _ := strings.Cut(kv, "="); name == key {
			n++
			last = value
		}
	}
	return n, last
}

// Env slices here are built by hand, never from os.Environ(): the shell that
// runs these tests may export CLAUDE_CODE_PROMPT_CACHE_TTL itself, and a test
// that inherits it passes without exercising the helper.
func TestClaudeCacheTTLLauncherHelper(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/nonexistent"}
	cases := []struct {
		name     string
		env      []string
		settings map[string]any
		want     int
		wantVal  string
	}{
		{name: "adds exactly one", env: base, want: 1, wantVal: "1h"},
		{name: "inherited 5m kept", env: append(append([]string(nil), base...), "CLAUDE_CODE_PROMPT_CACHE_TTL=5m"), want: 1, wantVal: "5m"},
		{name: "inherited empty kept", env: append(append([]string(nil), base...), "CLAUDE_CODE_PROMPT_CACHE_TTL="), want: 1, wantVal: ""},
		{name: "FORCE env respected", env: append(append([]string(nil), base...), "FORCE_PROMPT_CACHING_5M=1"), want: 0},
		{name: "promptCacheTtl setting respected", env: base, settings: map[string]any{"promptCacheTtl": "5m"}, want: 0},
		{name: "settings env TTL respected", env: base, settings: map[string]any{"env": map[string]any{"CLAUDE_CODE_PROMPT_CACHE_TTL": "5m"}}, want: 0},
		{name: "settings env FORCE respected", env: base, settings: map[string]any{"env": map[string]any{"FORCE_PROMPT_CACHING_5M": "1"}}, want: 0},
		{name: "subagent key is not a main-thread decision", env: append(append([]string(nil), base...), "CLAUDE_CODE_SUBAGENT_PROMPT_CACHE_TTL=5m"), want: 1, wantVal: "1h"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := append([]string(nil), tc.env...)
			got := withClaudeCacheTTL(in, tc.settings)
			n, val := countEnvKey(got, claudePromptCacheTTLEnv)
			if n != tc.want || (n > 0 && val != tc.wantVal) {
				t.Fatalf("%s appears %d time(s) with %q, want %d with %q:\n%s",
					claudePromptCacheTTLEnv, n, val, tc.want, tc.wantVal, strings.Join(got, "\n"))
			}
			if !strings.Contains(strings.Join(tc.env, "\n"), "SUBAGENT") {
				if n, _ := countEnvKey(got, "CLAUDE_CODE_SUBAGENT_PROMPT_CACHE_TTL"); n != 0 {
					t.Fatalf("the helper set the subagent TTL:\n%s", strings.Join(got, "\n"))
				}
			}
			// Applying it again must not add a second copy.
			if n2, _ := countEnvKey(withClaudeCacheTTL(got, tc.settings), claudePromptCacheTTLEnv); n2 != n {
				t.Fatalf("second application changed the count from %d to %d", n, n2)
			}
		})
	}
}

// The launcher reads the settings.json Claude Code will read: the profile's
// config dir when there is one.
func TestClaudeCacheTTLLauncherEnvironmentReadsTheProfileSettings(t *testing.T) {
	environ := []string{"PATH=/usr/bin", "HOME=/nonexistent", "ANTHROPIC_API_KEY=sk-ant-secret"}

	fresh := t.TempDir()
	env := proxyClaudeEnvironment(environ, "http://127.0.0.1:31415/v1", fresh)
	if n, val := countEnvKey(env, claudePromptCacheTTLEnv); n != 1 || val != "1h" {
		t.Fatalf("fresh profile: %s appears %d time(s) with %q, want once with 1h:\n%s",
			claudePromptCacheTTLEnv, n, val, strings.Join(env, "\n"))
	}
	if n, val := countEnvKey(env, "CLAUDE_CONFIG_DIR"); n != 1 || val != fresh {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q (%d), want %q", val, n, fresh)
	}

	decided := t.TempDir()
	if err := os.WriteFile(filepath.Join(decided, "settings.json"), []byte(`{"promptCacheTtl":"5m"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env = proxyClaudeEnvironment(environ, "http://127.0.0.1:31415/v1", decided)
	if n, _ := countEnvKey(env, claudePromptCacheTTLEnv); n != 0 {
		t.Fatalf("profile with promptCacheTtl got the env key, which would override it:\n%s", strings.Join(env, "\n"))
	}
}

// claudeDirect strips claudeRoutingEnvKeys to reach Anthropic directly; the
// TTL is the user's preference, not routing, and must survive that strip.
func TestClaudeCacheTTLIsNotARoutingKey(t *testing.T) {
	for _, key := range claudeRoutingEnvKeys {
		switch strings.ToUpper(key) {
		case claudePromptCacheTTLEnv, claudeForcePromptCaching5mEnv, "CLAUDE_CODE_SUBAGENT_PROMPT_CACHE_TTL":
			t.Fatalf("%s is in claudeRoutingEnvKeys; claudeDirect would strip the user's value", key)
		}
	}
}

// claudeTTLEnvCase is one state of the environment a writer or `sr setup` runs
// in. Claude Code applies settings.json "env" over the environment a terminal
// launch starts with, so a TTL the user exported loses to any value a writer
// puts in settings.json, and an exported TTL is a decision the writers keep
// out of the way of. But only an export that is the user's:
//   - inside Claude Code (CLAUDECODE=1, set in every tool subprocess) the
//     environment is Claude Code's: it Object.assigns every settings source's
//     env into it, and `sr claude` injects 1h into the session it launches, so
//     a value there cannot be told apart from those and is not a decision;
//   - an exported CLAUDE_CODE_PROMPT_CACHE_TTL=1h is no conflict with the 1h the
//     writer writes, and settings.json is the only channel that reaches launches
//     that do not inherit the shell (the desktop app), so it is written anyway.
//
// FORCE_PROMPT_CACHING_5M exported outside Claude Code still counts, any value.
type claudeTTLEnvCase struct {
	name    string
	env     [][2]string // applied with t.Setenv after all three are cleared
	decided bool        // the environment records the user's own decision
}

func claudeTTLEnvCases() []claudeTTLEnvCase {
	const inside = "CLAUDECODE"
	return []claudeTTLEnvCase{
		{name: "outside, nothing exported", decided: false},
		{name: "outside, exported TTL 1h", env: [][2]string{{claudePromptCacheTTLEnv, "1h"}}, decided: false},
		{name: "outside, exported TTL 5m", env: [][2]string{{claudePromptCacheTTLEnv, "5m"}}, decided: true},
		{name: "outside, exported TTL empty", env: [][2]string{{claudePromptCacheTTLEnv, ""}}, decided: true},
		{name: "outside, exported FORCE", env: [][2]string{{claudeForcePromptCaching5mEnv, "1"}}, decided: true},
		{name: "outside, exported FORCE empty", env: [][2]string{{claudeForcePromptCaching5mEnv, ""}}, decided: true},
		{name: "outside, exported TTL 1h and FORCE", env: [][2]string{{claudePromptCacheTTLEnv, "1h"}, {claudeForcePromptCaching5mEnv, "1"}}, decided: true},
		{name: "outside (CLAUDECODE empty), exported TTL 5m", env: [][2]string{{inside, ""}, {claudePromptCacheTTLEnv, "5m"}}, decided: true},
		{name: "inside Claude Code, nothing exported", env: [][2]string{{inside, "1"}}, decided: false},
		{name: "inside Claude Code, TTL 1h", env: [][2]string{{inside, "1"}, {claudePromptCacheTTLEnv, "1h"}}, decided: false},
		{name: "inside Claude Code, TTL 5m", env: [][2]string{{inside, "1"}, {claudePromptCacheTTLEnv, "5m"}}, decided: false},
		{name: "inside Claude Code, TTL empty", env: [][2]string{{inside, "1"}, {claudePromptCacheTTLEnv, ""}}, decided: false},
		{name: "inside Claude Code, FORCE", env: [][2]string{{inside, "1"}, {claudeForcePromptCaching5mEnv, "1"}}, decided: false},
	}
}

func applyClaudeTTLEnvCase(t *testing.T, tc claudeTTLEnvCase) {
	t.Helper()
	clearClaudeCacheTTLEnv(t)
	for _, kv := range tc.env {
		t.Setenv(kv[0], kv[1])
	}
}

// The writer asks for 1h unless the environment records the user's own
// decision; a decision leaves the key out so the export applies.
func TestClaudeCacheTTLWriterRespectsAnExportedDecision(t *testing.T) {
	for _, tc := range claudeTTLEnvCases() {
		t.Run(tc.name, func(t *testing.T) {
			applyClaudeTTLEnvCase(t, tc)
			dir := filepath.Join(t.TempDir(), "claude")
			if err := writeClaudeProxyEnv(dir, "http://127.0.0.1:31415", "tok"); err != nil {
				t.Fatal(err)
			}
			_, env, body := readClaudeSettingsForTest(t, filepath.Join(dir, "settings.json"))
			if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:31415" {
				t.Fatalf("routing was not written:\n%s", body)
			}
			got, ok := env[claudePromptCacheTTLEnv]
			switch {
			case tc.decided && ok:
				t.Fatalf("the user's own export is a decision, yet the writer put %s=%v in settings.json, which beats it:\n%s",
					claudePromptCacheTTLEnv, got, body)
			case !tc.decided && (!ok || got != claudeRoutedPromptCacheTTL):
				t.Fatalf("no decision of the user's in this environment, yet %s = %v (present %v), want \"1h\":\n%s",
					claudePromptCacheTTLEnv, got, ok, body)
			}
			if _, ok := env[claudeForcePromptCaching5mEnv]; ok {
				t.Fatalf("the writer wrote %s:\n%s", claudeForcePromptCaching5mEnv, body)
			}
		})
	}
}

// The detector agrees with the writer in every environment: a routed file
// without the key is configured exactly when the environment holds the user's
// decision, and after setup's own write it is configured either way, so the
// `~` item is never offered forever and never hidden while the file lacks 1h.
func TestClaudeCacheTTLDetectorCountsAnExportedDecision(t *testing.T) {
	for _, tc := range claudeTTLEnvCases() {
		t.Run(tc.name, func(t *testing.T) {
			applyClaudeTTLEnvCase(t, tc)
			home := sandboxHostConfig(t)
			dir := filepath.Join(home, ".claude")
			path := filepath.Join(dir, "settings.json")
			root := codexProxyRootURL(localBaseURL())
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			// Tool search is decided in the file, so configured turns on the
			// TTL alone.
			routedOnly := `{"env":{"ANTHROPIC_BASE_URL":"` + root + `","ANTHROPIC_AUTH_TOKEN":"subrouter","ENABLE_TOOL_SEARCH":"true"}}`
			if err := os.WriteFile(path, []byte(routedOnly), 0o600); err != nil {
				t.Fatal(err)
			}
			if routed, configured := claudeSettingsForLocal(path, root); !routed || configured != tc.decided {
				t.Fatalf("routed file without the key: routed=%v configured=%v, want routed and configured=%v",
					routed, configured, tc.decided)
			}
			if _, err := writeClaudeProxyEnvForLocal(); err != nil {
				t.Fatal(err)
			}
			_, env, body := readClaudeSettingsForTest(t, path)
			if _, ok := env[claudePromptCacheTTLEnv]; ok == tc.decided {
				t.Fatalf("setup's write: key present %v, want present %v:\n%s", ok, !tc.decided, body)
			}
			if routed, configured := claudeSettingsForLocal(path, root); !routed || !configured {
				t.Fatalf("after setup's own write: routed=%v configured=%v, want both:\n%s", routed, configured, body)
			}
		})
	}
}

// The chain this test reproduces: settings.json routes but records
// no TTL, so `sr claude` launches Claude Code with CLAUDE_CODE_PROMPT_CACHE_TTL=1h
// in its environment; an agent in that session (CLAUDECODE=1 in its tool
// subprocesses), or any other child that inherits the launcher's environment,
// re-runs setup. The inherited 1h is the launcher's, not the user's: setup
// must still offer the item and write 1h, or plain `claude` and the desktop
// app stay on the 5-minute cache while setup reports everything done.
func TestClaudeCacheTTLLauncherInjectionIsNotTheUsersExport(t *testing.T) {
	for _, tc := range []struct {
		name        string
		insideAgent bool
	}{
		{name: "an agent inside the launched session", insideAgent: true},
		{name: "a plain child of the launcher", insideAgent: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearClaudeCacheTTLEnv(t)
			home := sandboxHostConfig(t)
			dir := filepath.Join(home, ".claude")
			path := filepath.Join(dir, "settings.json")
			root := codexProxyRootURL(localBaseURL())
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			routedOnly := `{"env":{"ANTHROPIC_BASE_URL":"` + root + `","ANTHROPIC_AUTH_TOKEN":"subrouter"}}`
			if err := os.WriteFile(path, []byte(routedOnly), 0o600); err != nil {
				t.Fatal(err)
			}
			// The launcher's child environment, from a hand-built environ (never
			// os.Environ(): the shell running this may export the key itself).
			child := proxyClaudeEnvironment([]string{"PATH=/usr/bin", "HOME=" + home}, "http://127.0.0.1:31415/v1", dir)
			n, injected := countEnvKey(child, claudePromptCacheTTLEnv)
			if n != 1 || injected != claudeRoutedPromptCacheTTL {
				t.Fatalf("calibration: the launcher did not inject 1h (%d, %q); the test proves nothing:\n%s",
					n, injected, strings.Join(child, "\n"))
			}
			t.Setenv(claudePromptCacheTTLEnv, injected)
			if tc.insideAgent {
				t.Setenv("CLAUDECODE", "1")
			}
			if routed, configured := claudeSettingsForLocal(path, root); !routed || configured {
				t.Fatalf("with the launcher's 1h inherited: routed=%v configured=%v, want routed and NOT configured",
					routed, configured)
			}
			if _, err := writeClaudeProxyEnvForLocal(); err != nil {
				t.Fatal(err)
			}
			_, env, body := readClaudeSettingsForTest(t, path)
			if env[claudePromptCacheTTLEnv] != claudeRoutedPromptCacheTTL {
				t.Fatalf("setup left settings.json without 1h, so launches that do not inherit this environment get 5m:\n%s", body)
			}
			if routed, configured := claudeSettingsForLocal(path, root); !routed || !configured {
				t.Fatalf("after the write: routed=%v configured=%v, want both", routed, configured)
			}
		})
	}
}

// "Routed" is what env.ANTHROPIC_BASE_URL says, not whether the local URL
// appears somewhere in the file: a permission rule naming the local status
// page routes nothing, and must not earn a `~` cache item.
func TestClaudeCacheTTLDetectorReadsTheBaseURLNotTheText(t *testing.T) {
	clearClaudeCacheTTLEnv(t)
	home := sandboxHostConfig(t)
	dir := filepath.Join(home, ".claude")
	path := filepath.Join(dir, "settings.json")
	root := codexProxyRootURL(localBaseURL())
	port := urlPort(root)
	if port == "" {
		t.Fatalf("local root %q has no port; the loopback rows prove nothing", root)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	statusRule := "Bash(curl -s " + root + "/_subrouter/status)"
	settings := func(base string) []byte {
		doc := map[string]any{"permissions": map[string]any{"allow": []any{statusRule}}}
		if base != "" {
			doc["env"] = map[string]any{"ANTHROPIC_BASE_URL": base, "ANTHROPIC_AUTH_TOKEN": "tok"}
		}
		body, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	for _, tc := range []struct {
		name       string
		base       string
		wantRouted bool
	}{
		{name: "routed to a remote box", base: "https://box.example.com:31415"},
		{name: "not routed at all", base: ""},
		{name: "Anthropic directly", base: "https://api.anthropic.com"},
		{name: "another local port", base: "http://127.0.0.1:1"},
		{name: "the local daemon", base: root, wantRouted: true},
		{name: "the local daemon by localhost", base: "http://localhost:" + port, wantRouted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, settings(tc.base), 0o600); err != nil {
				t.Fatal(err)
			}
			// Calibration: the text match the old detector used says "routed".
			if !clientRoutesToLocal(path, root) {
				t.Fatal("fixture does not mention the local URL; the test proves nothing")
			}
			routed, configured := claudeSettingsForLocal(path, root)
			if routed != tc.wantRouted || configured {
				body, _ := os.ReadFile(path)
				t.Fatalf("routed=%v configured=%v, want routed=%v and not configured (no TTL):\n%s",
					routed, configured, tc.wantRouted, body)
			}
			in := emptyPoolInput()
			in.LocalCodexAccounts = 1
			in.LocalClaudeAccounts = 1
			in.ClaudeRouted, in.ClaudeConfigured = routed, configured
			item, ok := buildSetupPlan(in).find(actionConfigClaude)
			if !ok {
				t.Fatal("no Claude config item")
			}
			if isCacheItem := item.Change == "~"; isCacheItem != tc.wantRouted {
				t.Fatalf("Change %q Summary %q: the cache-only item belongs to routed files only", item.Change, item.Summary)
			}
		})
	}
}

// A file that routes but cannot be parsed keeps its old answer, "configured":
// the writer refuses to rewrite a file it cannot parse, so selecting the item
// would fail setup over a key it can never add.
func TestClaudeCacheTTLDetectorKeepsTheOldAnswerForAnUnparsableFile(t *testing.T) {
	clearClaudeCacheTTLEnv(t)
	home := sandboxHostConfig(t)
	dir := filepath.Join(home, ".claude")
	path := filepath.Join(dir, "settings.json")
	root := codexProxyRootURL(localBaseURL())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	routedBroken := `{"env":{"ANTHROPIC_BASE_URL":"` + root + `"}} trailing garbage`
	if err := os.WriteFile(path, []byte(routedBroken), 0o600); err != nil {
		t.Fatal(err)
	}
	// Calibration: the file really is unparsable, and the writer really refuses it.
	if _, err := readClaudeConfigFile(path); err == nil {
		t.Fatal("fixture parses; the test proves nothing")
	}
	if _, err := writeClaudeProxyEnvForLocal(); err == nil {
		t.Fatal("the writer rewrote an unparsable settings.json")
	}
	if routed, configured := claudeSettingsForLocal(path, root); !routed || !configured {
		t.Fatalf("unparsable routed file: routed=%v configured=%v, want both (the old answer)", routed, configured)
	}

	unroutedBroken := `{"env":{"ANTHROPIC_BASE_URL":"https://api.anthropic.com"}} trailing garbage`
	if err := os.WriteFile(path, []byte(unroutedBroken), 0o600); err != nil {
		t.Fatal(err)
	}
	if routed, configured := claudeSettingsForLocal(path, root); routed || configured {
		t.Fatalf("unparsable unrouted file: routed=%v configured=%v, want neither", routed, configured)
	}
}
