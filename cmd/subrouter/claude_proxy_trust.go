package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// `sr claude proxy` runs Claude Code in its own config home
// (claude-proxy/<hash>), and Claude keeps per-folder trust ("Do you trust the
// files in this folder?") in that home's .claude.json. Trust the user granted
// in their normal Claude never carried over, so every proxy home asked again
// for every folder (upstream #430).
//
// Before a proxy launch, the flags the user accepted in their own .claude.json
// are copied into the proxy home's. Only true values move, and only into a
// project entry that has not decided that flag, so the proxy home gains no
// trust the user did not grant and an explicit false there is never
// overwritten.
//
// Adapted from upstream, which also copied trust accepted inside a proxy
// session back into the user's own .claude.json after the launch. That write
// is dropped: the user's .claude.json is what every plain `claude` session
// reads and rewrites, and a change to what `sr claude` writes must never alter
// plain `claude` launches (the client-config brick rule). The price is that a
// folder trusted only inside a proxy session is asked about again in plain
// `claude` and in other proxy homes; trusting it once in plain `claude` covers
// every proxy home from its next launch.
var claudeSharedProjectFlags = []string{
	"hasTrustDialogAccepted",
	"hasCompletedProjectOnboarding",
	"hasClaudeMdExternalIncludesApproved",
	"hasClaudeMdExternalIncludesWarningShown",
}

// claudeConfigLockWait bounds how long sr waits for the proxy home's
// .claude.json lock before leaving the file alone for this launch.
const claudeConfigLockWait = time.Second

// claudeUserConfigPath is the .claude.json the user's normal Claude reads:
// $CLAUDE_CONFIG_DIR/.claude.json when set, else ~/.claude.json.
func claudeUserConfigPath() string {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return filepath.Join(dir, ".claude.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude.json")
}

// syncClaudeProjectTrustToProxy copies the user's accepted project flags into
// a proxy config home's .claude.json. It only reads userConfigPath. Failures
// are returned for logging only; a launch never depends on this.
func syncClaudeProjectTrustToProxy(userConfigPath, proxyConfigDir string) error {
	if strings.TrimSpace(userConfigPath) == "" || strings.TrimSpace(proxyConfigDir) == "" {
		return nil
	}
	proxyConfigPath := filepath.Join(proxyConfigDir, ".claude.json")
	if sameClaudePath(userConfigPath, proxyConfigPath) {
		// A proxy home whose .claude.json is the user's (a hand-made link)
		// already has every flag, and writing it would write the user's file.
		return nil
	}
	accepted, err := readClaudeAcceptedProjectFlags(userConfigPath)
	if err != nil {
		return err
	}
	return mergeClaudeAcceptedProjectFlags(proxyConfigPath, accepted)
}

// readClaudeAcceptedProjectFlags returns, per project path, the shared flags
// that are true. A missing file has no accepted flags, and so does a file
// whose "projects" is null.
func readClaudeAcceptedProjectFlags(path string) (map[string][]string, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var config struct {
		Projects map[string]map[string]json.RawMessage `json:"projects"`
	}
	if err := json.Unmarshal(body, &config); err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for project, fields := range config.Projects {
		for _, flag := range claudeSharedProjectFlags {
			if bytes.Equal(bytes.TrimSpace(fields[flag]), []byte("true")) {
				out[project] = append(out[project], flag)
			}
		}
	}
	return out, nil
}

// mergeClaudeAcceptedProjectFlags sets each accepted flag in the proxy home's
// config at path, creating the file when missing and preserving every other
// field byte for byte (values stay json.RawMessage). A link (a dotfile
// manager) is written through, a file Claude holds locked is left alone, and
// an unchanged file is not rewritten.
func mergeClaudeAcceptedProjectFlags(path string, accepted map[string][]string) error {
	if len(accepted) == 0 {
		return nil
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	release, locked := lockClaudeConfig(path)
	if !locked {
		return nil
	}
	defer release()
	info, statErr := os.Stat(path)
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		body = []byte("{}")
	} else if err != nil {
		return err
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(body, &config); err != nil {
		return err
	}
	if config == nil {
		config = map[string]json.RawMessage{}
	}
	projects := map[string]map[string]json.RawMessage{}
	if raw, ok := config["projects"]; ok {
		if err := json.Unmarshal(raw, &projects); err != nil {
			return err
		}
		if projects == nil {
			projects = map[string]map[string]json.RawMessage{}
		}
	}
	changed := false
	for project, flags := range accepted {
		fields := projects[project]
		if fields == nil {
			// Claude fills the rest of a project entry with its defaults.
			fields = map[string]json.RawMessage{}
		}
		for _, flag := range flags {
			// Only fill a flag this side has never decided. An explicit
			// false (trust revoked by hand) is left alone.
			if _, decided := fields[flag]; !decided {
				fields[flag] = json.RawMessage("true")
				changed = true
			}
		}
		projects[project] = fields
	}
	if !changed {
		return nil
	}
	encodedProjects, err := json.Marshal(projects)
	if err != nil {
		return err
	}
	config["projects"] = encodedProjects
	out, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if statErr == nil {
		mode = info.Mode().Perm()
	}
	return writeFileAtomic(path, append(out, '\n'), mode)
}

// lockClaudeConfig takes a <file>.lock directory lock, the proper-lockfile
// convention upstream matched to Claude Code's own config saves (2.1.283
// bundles proper-lockfile's lockSync; that it guards .claude.json is
// upstream's reading, not re-verified here). Either way two `sr claude proxy`
// launches sharing one proxy home never interleave their read-modify-write.
// It reports false when the lock stays held, so sr skips the write rather
// than race. A lock older than a minute is stale.
func lockClaudeConfig(path string) (func(), bool) {
	lockPath := path + ".lock"
	deadline := time.Now().Add(claudeConfigLockWait)
	for {
		if err := os.Mkdir(lockPath, 0o700); err == nil {
			return func() { _ = os.Remove(lockPath) }, true
		}
		if info, err := os.Stat(lockPath); err == nil && time.Since(info.ModTime()) > time.Minute {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return nil, false
		}
		time.Sleep(25 * time.Millisecond)
	}
}
