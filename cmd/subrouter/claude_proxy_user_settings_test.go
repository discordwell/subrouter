package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/internal/agents/claude"
	"github.com/discordwell/subrouter/internal/storepath"
)

// Upstream #435's unit tests, adapted: every claude.RoutingEnvKeys entry is
// owned whether or not the launch sets it, and a launch with no env of its own
// must not get the user's raw env back. Upstream #483's tests that merge the
// proxy home's own settings into the overlay with the user's scalars winning
// (TestWithClaudeUserSettingsKeepsProxyConfigChoicesAndRoutingCase,
// TestClaudeProxySettingsRefreshAndMergeHomeValues) are not here: this fork
// keeps the directory's own single-value choices (PATCHES 29.9.2).

func TestWithClaudeUserSettingsCarriesHooksAndKeepsRoutingAuthoritative(t *testing.T) {
	userSettings := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(userSettings, []byte(userClaudeSettingsFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	launch, err := proxyClaudeLaunchSettings("http://127.0.0.1:31415", "token", "/tmp/proxy-config")
	if err != nil {
		t.Fatal(err)
	}
	body, err := withClaudeUserSettings(launch, userSettings, "")
	if err != nil {
		t.Fatal(err)
	}
	var merged map[string]any
	if err := json.Unmarshal(body, &merged); err != nil {
		t.Fatal(err)
	}
	env, _ := merged["env"].(map[string]any)
	assertUserSettingsCarried(t, merged, env)
	if merged["theme"] != "dark" {
		t.Fatalf("user theme missing: %s", body)
	}
	if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:31415" || env["ANTHROPIC_AUTH_TOKEN"] != "token" || env["CLAUDE_CONFIG_DIR"] != "/tmp/proxy-config" {
		t.Fatalf("routing env not authoritative: %s", body)
	}
}

func TestWithClaudeUserSettingsIgnoresMissingOrInvalidFile(t *testing.T) {
	launch := []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:1"}}`)
	dir := t.TempDir()
	invalid := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(invalid, []byte(`[1,2]`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", filepath.Join(dir, "missing.json"), invalid} {
		body, err := withClaudeUserSettings(launch, path, "")
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != string(launch) {
			t.Fatalf("%q changed launch settings: %s", path, body)
		}
	}
}

func TestWithClaudeUserSettingsKeepsOwnConfigChoicesAndRoutingCase(t *testing.T) {
	dir := t.TempDir()
	userSettings := filepath.Join(dir, "user.json")
	if err := os.WriteFile(userSettings, []byte(`{
		"theme": "dark",
		"model": "user-model",
		"hooks": {"SessionStart": []},
		"env": {"anthropic_base_url": "https://elsewhere.example", "USER_ONLY": "kept"}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ownSettings := filepath.Join(dir, "own.json")
	if err := os.WriteFile(ownSettings, []byte(`{"theme": "light", "hooks": {"Stop": []}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	launch := []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:1"}}`)
	body, err := withClaudeUserSettings(launch, userSettings, ownSettings)
	if err != nil {
		t.Fatal(err)
	}
	var merged map[string]any
	if err := json.Unmarshal(body, &merged); err != nil {
		t.Fatal(err)
	}
	if _, ok := merged["theme"]; ok {
		t.Fatalf("user theme hid the profile's own choice: %s", body)
	}
	if hooks, _ := merged["hooks"].(map[string]any); merged["model"] != "user-model" || hooks["SessionStart"] == nil {
		t.Fatalf("user settings missing: %s", body)
	}
	env, _ := merged["env"].(map[string]any)
	if _, ok := env["anthropic_base_url"]; ok || env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:1" || env["USER_ONLY"] != "kept" {
		t.Fatalf("env = %v, want routing keys owned by sr in any case", env)
	}
}

// Upstream kept the user's raw env whenever the combined env came out empty.
// A direct profile launch has no env of its own, so a user env holding only
// the cutover's routing keys would have been carried whole.
func TestWithClaudeUserSettingsNeverRestoresTheUsersRoutingEnv(t *testing.T) {
	userSettings := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(userSettings, []byte(`{"theme":"dark","env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:31415","ANTHROPIC_AUTH_TOKEN":"the-daemon-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	body, err := withClaudeUserSettings([]byte(`{}`), userSettings, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "the-daemon-token") || strings.Contains(string(body), "ANTHROPIC_BASE_URL") {
		t.Fatalf("a launch with no env of its own got the user's routing env back: %s", body)
	}
	if string(body) != `{"theme":"dark"}` {
		t.Fatalf("body = %s, want only the theme", body)
	}
}

func TestClaudeUserSettingsPathSkipsLaunchesThatAlreadyReadIt(t *testing.T) {
	withoutEnvForTest(t, claudeProxyUserSettingsEnv)
	shared := t.TempDir()
	if err := os.WriteFile(filepath.Join(shared, "settings.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := t.TempDir()
	if got, want := claudeUserSettingsPathFor(shared, profile), filepath.Join(shared, "settings.json"); got != want {
		t.Fatalf("profile launch path = %q, want %q", got, want)
	}
	for name, configDir := range map[string]string{
		"no profile (the child reads ~/.claude itself)": "",
		"the shared home itself":                        shared,
	} {
		if got := claudeUserSettingsPathFor(shared, configDir); got != "" {
			t.Fatalf("%s: path = %q, want none (hooks would run twice)", name, got)
		}
	}
	linked := t.TempDir()
	if err := os.Symlink(filepath.Join(shared, "settings.json"), filepath.Join(linked, "settings.json")); err != nil {
		t.Fatal(err)
	}
	if got := claudeUserSettingsPathFor(shared, linked); got != "" {
		t.Fatalf("profile whose settings.json links to the user's: path = %q, want none", got)
	}
	if got := claudeUserSettingsPathFor("", profile); got != "" {
		t.Fatalf("store with no shared home: path = %q, want none", got)
	}
	t.Setenv(claudeProxyUserSettingsEnv, "off")
	if got := claudeUserSettingsPathFor(shared, profile); got != "" {
		t.Fatalf("opted-out launch read user settings at %q", got)
	}
}

func TestClaudeProxyUserSettingsPathIgnoresHermeticStores(t *testing.T) {
	withoutEnvForTest(t, claudeProxyUserSettingsEnv)
	home := sandboxHostConfig(t)
	if got := claudeProxyUserSettingsPath(t.TempDir(), t.TempDir()); got != "" {
		t.Fatalf("hermetic store read user settings at %q", got)
	}
	if got, want := claudeProxyUserSettingsPath(claude.DefaultStore().Dir, t.TempDir()), filepath.Join(home, ".claude", "settings.json"); got != want {
		t.Fatalf("user settings path = %q, want %q", got, want)
	}
}

// --setting-sources picks which of the user/project/local sources load, and
// flag settings load regardless, so a caller who left "user" out must not get
// it back through sr's --settings file.
func TestClaudeProxyLaunchHonorsSettingSources(t *testing.T) {
	clearClaudeCacheTTLEnv(t)
	home := sandboxHostConfig(t)
	writeUserClaudeSettings(t, home)
	recordDir := writeRecordingClaudeCLI(t)
	runner := srRunner{
		program: "sr",
		store:   accounts.CodexStore{Dir: filepath.Join(storepath.CodexDir(), "accounts")},
		in:      strings.NewReader(""), out: io.Discard, errOut: io.Discard,
	}
	for _, args := range [][]string{{"--setting-sources", "project"}, {"--setting-sources=project,local"}} {
		if err := runner.runProxyClaude(t.Context(), args, "http://127.0.0.1:31415/v1", "tok", t.TempDir(), "", ""); err != nil {
			t.Fatal(err)
		}
		doc, _ := readRecordedLaunchSettings(t, filepath.Join(recordDir, "settings.json"))
		if _, ok := doc["hooks"]; ok {
			t.Fatalf("%q: the user's settings rode in although the caller chose the sources: %v", args, doc)
		}
	}
	if err := runner.runProxyClaude(t.Context(), []string{"--", "--setting-sources", "project"}, "http://127.0.0.1:31415/v1", "tok", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	if doc, _ := readRecordedLaunchSettings(t, filepath.Join(recordDir, "settings.json")); doc["hooks"] == nil {
		t.Fatalf("a --setting-sources after -- is a prompt, not a flag, yet the user's settings were dropped: %v", doc)
	}
}

// The user's settings now ride in the private --settings file, so a
// promptCacheTtl there is the user's TTL decision (§21): Claude Code's env
// beats the setting, so injecting 1h into the environment would override it.
func TestClaudeProxyLaunchKeepsTheUsersPromptCacheTTLSetting(t *testing.T) {
	clearClaudeCacheTTLEnv(t)
	home := sandboxHostConfig(t)
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"promptCacheTtl":"5m"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	recordDir := writeRecordingClaudeCLI(t)
	runner := srRunner{
		program: "sr",
		store:   accounts.CodexStore{Dir: filepath.Join(storepath.CodexDir(), "accounts")},
		in:      strings.NewReader(""), out: io.Discard, errOut: io.Discard,
	}
	if err := runner.runProxyClaude(t.Context(), nil, "http://127.0.0.1:31415/v1", "tok", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	doc, _ := readRecordedLaunchSettings(t, filepath.Join(recordDir, "settings.json"))
	if doc["promptCacheTtl"] != "5m" {
		t.Fatalf("calibration: the user's promptCacheTtl did not ride in the launch settings: %v", doc)
	}
	body, err := os.ReadFile(filepath.Join(recordDir, "env"))
	if err != nil {
		t.Fatal(err)
	}
	if n, val := countEnvKey(strings.Split(strings.TrimSpace(string(body)), "\n"), claudePromptCacheTTLEnv); n != 0 {
		t.Fatalf("the launcher injected %s=%s over the user's promptCacheTtl", claudePromptCacheTTLEnv, val)
	}
}

func TestWithClaudeUserSettingsLetsSRStatusLineWin(t *testing.T) {
	userSettings := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(userSettings, []byte(`{"statusLine": {"type": "command", "command": "user-status"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	launch, err := json.Marshal(map[string]any{"env": map[string]string{}, "statusLine": map[string]any{"type": "command", "command": "sr-status"}})
	if err != nil {
		t.Fatal(err)
	}
	body, err := withClaudeUserSettings(launch, userSettings, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "sr-status") || strings.Contains(string(body), "user-status") {
		t.Fatalf("status line = %s, want sr's", body)
	}
}

func TestAccountPickerUsageDoesNotWaitOnSlowServer(t *testing.T) {
	previousWait := accountPickerUsageWait
	accountPickerUsageWait = 300 * time.Millisecond
	t.Cleanup(func() { accountPickerUsageWait = previousWait })
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		select {
		case <-release:
		case <-req.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	storeDir := t.TempDir()
	runner := srRunner{
		store:  accounts.CodexStore{Dir: filepath.Join(storeDir, "accounts")},
		out:    io.Discard,
		errOut: io.Discard,
		client: http.DefaultClient,
	}
	config := srServerConfig{Name: "team", URL: server.URL}

	started := time.Now()
	if statuses, notice := runner.accountPickerUsage(context.Background(), config); statuses != nil || notice != "" {
		t.Fatalf("statuses = %v, notice = %q, want none without a cache", statuses, notice)
	}
	if elapsed := time.Since(started); elapsed > accountPickerUsageWait+time.Second {
		t.Fatalf("picker waited %s on a slow server", elapsed)
	}

	ledger := newSessionLedger(runner.store.StoreDir())
	cached := []remoteServerUsageStatus{{}}
	cached[0].ID = "cached-account"
	if err := ledger.writeJSON(sessionUsageCachePath(ledger, config.Name), sessionUsageCache{FetchedAt: time.Now().Add(-time.Minute), Statuses: cached}); err != nil {
		t.Fatal(err)
	}
	statuses, notice := runner.accountPickerUsage(context.Background(), config)
	if len(statuses) != 1 || statuses[0].ID != "cached-account" || !strings.Contains(notice, "from 1m") {
		t.Fatalf("statuses = %+v, notice = %q, want the recent cached copy", statuses, notice)
	}

	if err := ledger.writeJSON(sessionUsageCachePath(ledger, config.Name), sessionUsageCache{FetchedAt: time.Now().Add(-time.Hour), Statuses: cached}); err != nil {
		t.Fatal(err)
	}
	if statuses, _ := runner.accountPickerUsage(context.Background(), config); statuses != nil {
		t.Fatalf("statuses = %+v, want none from an hour-old cache", statuses)
	}
}

func TestWithClaudeUserSettingsAllowsSharedProjectMemory(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, "projects")
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	projects, err := filepath.EvalSymlinks(projects)
	if err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(root, "settings.json")
	if err := os.WriteFile(settings, []byte(`{"permissions":{"additionalDirectories":["/tmp/other"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	body, err := withClaudeProxyMemoryDirectory([]byte(`{"permissions":{"additionalDirectories":["/tmp/other"]}}`), root)
	if err != nil {
		t.Fatal(err)
	}
	var merged map[string]any
	if err := json.Unmarshal(body, &merged); err != nil {
		t.Fatal(err)
	}
	permissions, _ := merged["permissions"].(map[string]any)
	directories, _ := permissions["additionalDirectories"].([]any)
	if len(directories) != 2 || directories[0] != "/tmp/other" || directories[1] != projects {
		t.Fatalf("additionalDirectories = %v, want user and shared projects", directories)
	}
}

func TestClaudeProxyMemoryDirectoryResolvesSymlinksAndDeduplicates(t *testing.T) {
	root := t.TempDir()
	real := t.TempDir()
	if err := os.Symlink(real, filepath.Join(root, "projects")); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"permissions":{"allow":["Read(example)"]}}`)
	for i := 0; i < 2; i++ {
		var err error
		body, err = withClaudeProxyMemoryDirectory(body, root)
		if err != nil {
			t.Fatal(err)
		}
	}
	var got struct {
		Permissions struct {
			AdditionalDirectories []string
			Allow                 []string
		}
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Permissions.AdditionalDirectories) != 1 || got.Permissions.AdditionalDirectories[0] != resolved || len(got.Permissions.Allow) != 1 {
		t.Fatalf("settings: %s", body)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["env"].(map[string]any)["CLAUDE_CODE_REMOTE_MEMORY_DIR"] != root {
		t.Fatalf("memory root env missing: %s", body)
	}
}

// Upstream #498's managed-launch test, adapted to this fork's runClaude, which
// layers the user's settings with withClaudeUserSettings directly (PATCHES
// 29.9.2): a routed profile keeps sr's route, and a direct one gets none of
// the user's routing keys back (upstream's withManagedClaudeUserSettings let
// the user's ANTHROPIC_BASE_URL through to a direct launch).
func TestManagedClaudeLaunchCarriesUserSettings(t *testing.T) {
	userPath := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(userPath, []byte(`{
		"env": {
			"CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS": "200",
			"CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH": "5",
			"ANTHROPIC_BASE_URL": "https://not-the-router.example"
		},
		"forceLoginMethod": "console",
		"includeCoAuthoredBy": false
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	launch, err := managedClaudeLaunchSettings("https://router.example", "/tmp/profile")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		body    []byte
		baseURL string
	}{
		{name: "routed", body: launch, baseURL: "https://router.example"},
		{name: "direct", body: []byte(`{}`), baseURL: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := withClaudeUserSettings(tc.body, userPath, "")
			if err != nil {
				t.Fatal(err)
			}
			var settings map[string]any
			if err := json.Unmarshal(body, &settings); err != nil {
				t.Fatal(err)
			}
			env, _ := settings["env"].(map[string]any)
			if env["CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS"] != "200" || env["CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH"] != "5" {
				t.Fatalf("user env missing: %v", env)
			}
			if got, _ := env["ANTHROPIC_BASE_URL"].(string); got != tc.baseURL {
				t.Fatalf("ANTHROPIC_BASE_URL = %q, want %q", got, tc.baseURL)
			}
			if _, ok := settings["forceLoginMethod"]; ok {
				t.Fatal("credential-source setting must be dropped")
			}
			if settings["includeCoAuthoredBy"] != false {
				t.Fatalf("non-env user setting missing: %v", settings)
			}
		})
	}
}
