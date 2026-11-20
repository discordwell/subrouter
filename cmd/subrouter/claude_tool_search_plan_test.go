package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The routed-but-incomplete case is a modification, and the review screen has
// to say which key it adds: a TTL-era install needs only tool search, and
// "Ask Claude Code for the 1-hour prompt cache" would describe a change setup
// is not making.
func TestClaudeToolSearchPlanItemNamesWhatIsMissing(t *testing.T) {
	in := emptyPoolInput()
	in.LocalCodexAccounts = 1
	in.LocalClaudeAccounts = 1
	in.ClaudeRouted = true
	in.ClaudeConfigured = false

	for _, tc := range []struct {
		missing []string
		summary string
		keys    []string
		absent  []string
	}{
		{
			missing: []string{claudeToolSearchEnv},
			summary: "Turn Claude Code's tool search back on",
			keys:    []string{claudeToolSearchEnv},
			absent:  []string{claudePromptCacheTTLEnv},
		},
		{
			missing: []string{claudePromptCacheTTLEnv, claudeToolSearchEnv},
			summary: "Ask Claude Code for the 1-hour prompt cache and turn tool search back on",
			keys:    []string{claudePromptCacheTTLEnv, claudeToolSearchEnv},
		},
		{
			missing: []string{claudePromptCacheTTLEnv},
			summary: "Ask Claude Code for the 1-hour prompt cache",
			keys:    []string{claudePromptCacheTTLEnv},
			absent:  []string{claudeToolSearchEnv},
		},
	} {
		in.ClaudeMissing = tc.missing
		item, ok := buildSetupPlan(in).find(actionConfigClaude)
		if !ok {
			t.Fatal("no Claude config item")
		}
		if item.Summary != tc.summary || item.Change != "~" {
			t.Fatalf("missing %v: Summary %q Change %q, want %q marked \"~\"", tc.missing, item.Summary, item.Change, tc.summary)
		}
		for _, key := range tc.keys {
			if !strings.Contains(item.Detail, key) {
				t.Fatalf("missing %v: detail does not name %s: %q", tc.missing, key, item.Detail)
			}
		}
		for _, key := range tc.absent {
			if strings.Contains(item.Detail, key) {
				t.Fatalf("missing %v: detail names %s, which setup is not writing: %q", tc.missing, key, item.Detail)
			}
		}
	}

	// A fresh routing item writes the user's own ~/.claude, which never gets
	// tool search (claudeRoutedToolSearch), so it must not promise it.
	in.ClaudeRouted, in.ClaudeMissing = false, nil
	if item, _ := buildSetupPlan(in).find(actionConfigClaude); strings.Contains(item.Detail, claudeToolSearchEnv) {
		t.Fatalf("routing item promises tool search, which setup does not write to ~/.claude: %q", item.Detail)
	}
}

// The observer feeds the item exactly the keys the writer would add, from the
// same predicate, so the item never names a key setup then leaves alone. The
// observed file is the user's own ~/.claude, so tool search is never among
// them, whatever the file says about it.
func TestClaudeToolSearchDetectorNamesTheMissingKeys(t *testing.T) {
	clearToolSearchEnv(t)
	home := sandboxHostConfig(t)
	dir := filepath.Join(home, ".claude")
	path := filepath.Join(dir, "settings.json")
	root := codexProxyRootURL(localBaseURL())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		extra string
		want  []string
	}{
		{extra: ``, want: []string{claudePromptCacheTTLEnv}},
		{extra: `,"CLAUDE_CODE_PROMPT_CACHE_TTL":"1h"`, want: nil},
		{extra: `,"ENABLE_TOOL_SEARCH":""`, want: []string{claudePromptCacheTTLEnv}},
		{extra: `,"CLAUDE_CODE_PROMPT_CACHE_TTL":"5m","ENABLE_TOOL_SEARCH":"false"`, want: nil},
	} {
		body := `{"env":{"ANTHROPIC_BASE_URL":"` + root + `","ANTHROPIC_AUTH_TOKEN":"subrouter"` + tc.extra + `}}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		routed, missing := claudeRoutedSettingsForLocal(path, root)
		if !routed || !slices.Equal(missing, tc.want) {
			t.Fatalf("%s: routed=%v missing=%v, want routed and %v", body, routed, missing, tc.want)
		}
	}
}
