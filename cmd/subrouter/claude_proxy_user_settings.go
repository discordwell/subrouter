package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/discordwell/subrouter/internal/agents/claude"
)

// Pooled and managed Claude launches point CLAUDE_CONFIG_DIR at a profile or
// proxy directory (sr claude with an active profile, sr claude proxy, sr claude
// run), so Claude Code reads that directory's settings.json as the user
// settings and never sees the user's own ~/.claude/settings.json. Hooks (the
// PreToolUse guards, SessionStart integrations), permissions, auto-mode rules
// and the status line silently stopped applying under `sr claude`; on the
// laptop the active profile's settings.json holds only forceLoginMethod while
// ~/.claude/settings.json carries 12 hook events (upstream #435).
//
// sr already hands Claude one private --settings file, so the user's settings
// ride in that same file: no extra argument, no extra settings file. sr owns
// routing and credentials, so every key in claude.RoutingEnvKeys is dropped
// from the user's env (in any letter case: Windows environment names are
// case-insensitive) whether or not the launch sets it. The user's
// ~/.claude/settings.json is the file the cutover points at the local daemon,
// so its env holds ANTHROPIC_BASE_URL and the daemon token, and it may hold
// CLAUDE_CODE_USE_BEDROCK; carried over, those would re-route a pinned proxy
// launch or turn a direct profile launch into a proxied one. sr's own launch
// env is applied last, and settings that substitute a different credential
// source are dropped.
var claudeProxyUserSettingsDropped = []string{
	"apiKeyHelper",
	"awsAuthRefresh",
	"awsCredentialExport",
	"gcpAuthRefresh",
	"forceLoginMethod",
	"forceLoginOrgUUID",
}

// claudeProxyUserSettingsEnv set to 0, false or off launches without the
// user's settings. Claude rejects a whole settings file when one field fails
// its schema (an invalid permissions block, for example), and the routing
// values share that file, so a broken user settings.json makes a routed launch
// fail with "Not logged in" until it is fixed or this is set.
const claudeProxyUserSettingsEnv = "SUBROUTER_CLAUDE_USER_SETTINGS"

func claudeUserSettingsOptedOut() bool {
	value := strings.TrimSpace(os.Getenv(claudeProxyUserSettingsEnv))
	return value == "0" || strings.EqualFold(value, "false") || strings.EqualFold(value, "off")
}

// claudeProxyUserSettingsPath is the user settings file a launch through the
// srRunner store should carry: the real default store's shared Claude home,
// and "" for hermetic stores, which must never read the user's real Claude
// home (the guard prepareClaudeProxySharedState uses).
func claudeProxyUserSettingsPath(storeDir, configDir string) string {
	store := claude.DefaultStore()
	if filepath.Clean(storeDir) != filepath.Clean(store.Dir) {
		return ""
	}
	return claudeUserSettingsPathFor(store.SharedStateDir, configDir)
}

// claudeUserSettingsPathFor returns sharedStateDir/settings.json when a Claude
// launched with CLAUDE_CONFIG_DIR=configDir would not already read it, and ""
// otherwise:
//
//   - the user opted out (claudeProxyUserSettingsEnv);
//   - no shared Claude home (server-side and hermetic stores);
//   - configDir "" (no profile selected): the child is launched without
//     CLAUDE_CONFIG_DIR, so it reads ~/.claude/settings.json itself, and a
//     second copy in --settings would double every hook;
//   - configDir is that home, or its settings.json is the user's file (a
//     hand-made symlink), for the same reason.
func claudeUserSettingsPathFor(sharedStateDir, configDir string) string {
	if claudeUserSettingsOptedOut() || strings.TrimSpace(sharedStateDir) == "" || strings.TrimSpace(configDir) == "" {
		return ""
	}
	if sameClaudePath(sharedStateDir, configDir) {
		return ""
	}
	userSettings := filepath.Join(sharedStateDir, "settings.json")
	if sameClaudePath(userSettings, claudeOwnSettingsPath(configDir)) {
		return ""
	}
	return userSettings
}

// sameClaudePath reports whether two paths name the same file or directory,
// following links; paths that do not both exist compare by their clean form.
func sameClaudePath(a, b string) bool {
	left, errLeft := os.Stat(a)
	right, errRight := os.Stat(b)
	if errLeft == nil && errRight == nil {
		return os.SameFile(left, right)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func claudeOwnSettingsPath(configDir string) string {
	if strings.TrimSpace(configDir) == "" {
		return ""
	}
	return filepath.Join(configDir, "settings.json")
}

// claudeArgsChooseSettingSources reports whether the caller passed
// --setting-sources before the option terminator. That flag says which of the
// user, project and local sources load; flag settings load regardless, so
// carrying the user's settings in --settings would override a caller who left
// "user" out. Such a launch gets no user settings from sr.
func claudeArgsChooseSettingSources(args []string) bool {
	_, chosen := claudeArgsSettingSources(args)
	return chosen
}

// claudeArgsSettingSources returns the --setting-sources value the caller
// passed before the option terminator, and whether one was passed (an empty
// value -- no sources at all -- is still a choice).
func claudeArgsSettingSources(args []string) (string, bool) {
	for i, arg := range args {
		if arg == "--" {
			return "", false
		}
		if arg == "--setting-sources" {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", true
		}
		if value, ok := strings.CutPrefix(arg, "--setting-sources="); ok {
			return value, true
		}
	}
	return "", false
}

// withClaudeUserSettings layers sr's launch settings over the user's settings
// file. A single-value key the launch directory's own settings.json sets (a
// /config change made inside a pooled session) is left to that file, so the
// user's value does not hide it; objects and lists (hooks, permissions,
// enabledPlugins) are kept, because Claude combines those across files. A
// missing or unreadable user file leaves the launch settings as they were: a
// launch never depends on it.
//
// The launch body may carry the caller's --settings (withClaudeCLISettings,
// upstream #502), so a launch object (hooks, permissions) is merged into the
// user's rather than replacing it: hooks from every source reach Claude, with
// the order user file, then --settings, then sr's routing values.
func withClaudeUserSettings(settingsBody []byte, userSettingsPath, ownSettingsPath string) ([]byte, error) {
	if strings.TrimSpace(userSettingsPath) == "" {
		return settingsBody, nil
	}
	merged, ok := readClaudeSettingsObject(userSettingsPath)
	if !ok {
		return settingsBody, nil
	}
	var launch map[string]any
	if err := json.Unmarshal(settingsBody, &launch); err != nil {
		return nil, err
	}
	for _, key := range claudeProxyUserSettingsDropped {
		delete(merged, key)
	}
	if strings.TrimSpace(ownSettingsPath) != "" {
		if own, ok := readClaudeSettingsObject(ownSettingsPath); ok {
			for key, value := range own {
				switch value.(type) {
				case map[string]any, []any:
				default:
					delete(merged, key)
				}
			}
		}
	}
	launchEnv, _ := launch["env"].(map[string]any)
	owned := make(map[string]bool, len(claudeRoutingEnvKeys)+len(launchEnv))
	for _, key := range claudeRoutingEnvKeys {
		owned[strings.ToUpper(key)] = true
	}
	for key := range launchEnv {
		owned[strings.ToUpper(key)] = true
	}
	env := map[string]any{}
	if userEnv, ok := merged["env"].(map[string]any); ok {
		for key, value := range userEnv {
			if !owned[strings.ToUpper(key)] {
				env[key] = value
			}
		}
	}
	// Upstream left the user's raw env in place whenever the combined env came
	// out empty. For a launch with no env of its own (a direct profile run),
	// that put the user's routing keys back; the env is rebuilt from the
	// filtered copy or dropped.
	delete(merged, "env")
	for key, value := range launchEnv {
		env[key] = value
	}
	for key, value := range launch {
		if key == "env" {
			continue
		}
		if launchMap, ok := value.(map[string]any); ok {
			if existing, ok := merged[key].(map[string]any); ok {
				mergeClaudeSettingsMap(existing, launchMap)
				continue
			}
		}
		merged[key] = value
	}
	if len(env) > 0 {
		merged["env"] = env
	}
	return json.Marshal(merged)
}

// overlayClaudeLaunchSettings applies sr's launch settings over a lower
// precedence settings object (upstream #502; withClaudeCLISettings uses it for
// the caller's --settings). Credential-source settings are dropped from the
// base, every launch env key wins (case-insensitively), and launch maps merge
// into the base so collections such as hooks keep both sides. The base env
// here is the caller's --settings env, whose routing keys
// withClaudeCLISettings has already removed.
func overlayClaudeLaunchSettings(base, launch map[string]any) ([]byte, error) {
	for _, key := range claudeProxyUserSettingsDropped {
		delete(base, key)
	}
	launchEnv, _ := launch["env"].(map[string]any)
	owned := make(map[string]bool, len(launchEnv))
	for key := range launchEnv {
		owned[strings.ToUpper(key)] = true
	}
	env := map[string]any{}
	if baseEnv, ok := base["env"].(map[string]any); ok {
		for key, value := range baseEnv {
			// Windows environment names are case-insensitive: a base entry
			// that differs from a routing key only in case must not survive
			// beside it.
			if !owned[strings.ToUpper(key)] {
				env[key] = value
			}
		}
	}
	for key, value := range launchEnv {
		env[key] = value
	}
	for key, value := range launch {
		if key == "env" {
			continue
		}
		if launchMap, ok := value.(map[string]any); ok {
			if existing, ok := base[key].(map[string]any); ok {
				mergeClaudeSettingsMap(existing, launchMap)
				continue
			}
		}
		base[key] = value
	}
	// Rebuilt, never the base's raw env (the rule withClaudeUserSettings keeps).
	delete(base, "env")
	if len(env) > 0 {
		base["env"] = env
	}
	return json.Marshal(base)
}

// withClaudeProxyMemoryDirectory grants a launch whose config directory links
// projects/ to the shared Claude home the resolved shared projects root, and
// names the shared root as Claude Code's memory root (upstream #484), so
// auto-memory writes under projects/<project>/memory follow the link without
// a symlink-escape prompt. The per-project layout is kept; Claude's
// autoMemoryDirectory would flatten every project into one directory.
func withClaudeProxyMemoryDirectory(body []byte, sharedDir string) ([]byte, error) {
	if sharedDir == "" {
		return body, nil
	}
	projects, err := filepath.EvalSymlinks(filepath.Join(sharedDir, "projects"))
	if err != nil {
		return nil, err
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil {
		return nil, err
	}
	permissions, _ := settings["permissions"].(map[string]any)
	if permissions == nil {
		permissions = map[string]any{}
		settings["permissions"] = permissions
	}
	mergeClaudeSettingsMap(permissions, map[string]any{"additionalDirectories": []any{projects}})
	// Claude Code derives one project-specific memory directory below this
	// root. This environment setting makes the root explicit while preserving
	// the per-project layout.
	env, _ := settings["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
		settings["env"] = env
	}
	env["CLAUDE_CODE_REMOTE_MEMORY_DIR"] = sharedDir
	return json.Marshal(settings)
}

// mergeClaudeSettingsMap merges src into dst: maps recursively, lists as a
// union that keeps dst's order, and any other value replaced by src's.
func mergeClaudeSettingsMap(dst, src map[string]any) {
	for key, value := range src {
		// These are individual command definitions, not collection maps.
		if key == "statusLine" {
			dst[key] = value
			continue
		}
		if srcMap, ok := value.(map[string]any); ok {
			if dstMap, ok := dst[key].(map[string]any); ok {
				mergeClaudeSettingsMap(dstMap, srcMap)
				continue
			}
		}
		if srcList, ok := value.([]any); ok {
			if dstList, ok := dst[key].([]any); ok {
				for _, item := range srcList {
					found := false
					for _, previous := range dstList {
						if reflect.DeepEqual(previous, item) {
							found = true
							break
						}
					}
					if !found {
						dstList = append(dstList, item)
					}
				}
				dst[key] = dstList
				continue
			}
		}
		dst[key] = value
	}
}

func readClaudeSettingsObject(path string) (map[string]any, bool) {
	body, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("read Claude settings for launch", "path", path, "error", err)
		}
		return nil, false
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil || settings == nil {
		slog.Warn("ignore Claude settings for launch: not a JSON object", "path", path)
		return nil, false
	}
	return settings, true
}
