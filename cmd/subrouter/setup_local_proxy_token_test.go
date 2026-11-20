package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/broker"
)

// readClaudeEnv returns the env block setup wrote, so a test can assert on the
// two variables that decide whether Claude Code can reach the daemon at all.
func readClaudeEnv(t *testing.T, settingsPath string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read %s: %v", settingsPath, err)
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatalf("parse %s: %v", settingsPath, err)
	}
	env, _ := settings["env"].(map[string]any)
	if env == nil {
		t.Fatalf("no env block in %s", settingsPath)
	}
	return env
}

// The daemon's localProxyAuthorized compares the client's bearer token against
// cloud.json's localProxyToken by constant-time equality, so writing the dummy
// while the daemon enforces a token 401s every request. Claude Code has no
// fallback once the proxy env overrides its login, so this is the difference
// between a working install and a machine whose every session is dead.
func TestLocalClaudeEnvCarriesTheTokenTheDaemonEnforces(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef0123456789a"

	config := broker.Config{
		Version:          1,
		BaseURL:          broker.DefaultBaseURL,
		LocalProxyToken:  token,
		CredentialSource: broker.CredentialSourceLocal,
	}

	client := cloudClientProxyToken(config, localBaseURL())
	server := cloudServerProxyToken(config)

	if client != token {
		t.Fatalf("client token = %q, want the enforced token %q", client, token)
	}
	// The pair has to agree: a client token the server does not require is
	// harmless, but a server token the client does not send is the 401.
	if server != client {
		t.Fatalf("server enforces %q but client sends %q", server, client)
	}

	dir := t.TempDir()
	if err := writeClaudeProxyEnv(dir, codexProxyRootURL(localBaseURL()), client); err != nil {
		t.Fatalf("writeClaudeProxyEnv: %v", err)
	}
	env := readClaudeEnv(t, filepath.Join(dir, "settings.json"))

	if got := env["ANTHROPIC_AUTH_TOKEN"]; got != token {
		t.Errorf("ANTHROPIC_AUTH_TOKEN = %v, want the enforced token", got)
	}
	if got, _ := env["ANTHROPIC_AUTH_TOKEN"].(string); got == "subrouter" {
		t.Error("wrote the dummy token while the daemon enforces a real one; every request 401s")
	}
}

// The regression guard for the brick. writeClaudeProxyEnvForLocal is the code
// that chooses what token to hand Claude Code, and it used to hardcode the
// empty tenant key, so it wrote the dummy no matter what the daemon enforced.
// Asserting on writeClaudeProxyEnv with a token passed in cannot catch that;
// the choice has to be exercised, which means driving the real function against
// an isolated HOME and cloud.json.
func TestSetupWritesTheEnforcedTokenForLocalClaude(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef0123456789a"

	home := sandboxHostConfig(t)

	configPath := filepath.Join(home, ".config", "subrouter", "cloud.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatalf("mkdir cloud config: %v", err)
	}
	body, err := json.Marshal(broker.Config{
		Version:          1,
		BaseURL:          broker.DefaultBaseURL,
		LocalProxyToken:  token,
		CredentialSource: broker.CredentialSourceLocal,
	})
	if err != nil {
		t.Fatalf("marshal cloud config: %v", err)
	}
	if err := os.WriteFile(configPath, body, 0o600); err != nil {
		t.Fatalf("write cloud config: %v", err)
	}
	t.Setenv("SUBROUTER_CLOUD_CONFIG", configPath)

	written, err := writeClaudeProxyEnvForLocal()
	if err != nil {
		t.Fatalf("writeClaudeProxyEnvForLocal: %v", err)
	}
	if want := filepath.Join(home, ".claude", "settings.json"); written != want {
		t.Fatalf("wrote %s, want %s", written, want)
	}

	env := readClaudeEnv(t, written)
	got, _ := env["ANTHROPIC_AUTH_TOKEN"].(string)
	if got == "subrouter" {
		t.Fatal("setup wrote the dummy token while the daemon enforces a real one; this is the 401 that bricked every session")
	}
	if got != token {
		t.Fatalf("ANTHROPIC_AUTH_TOKEN = %q, want the enforced token %q", got, token)
	}
}

// Claude Code appends its own /v1. Recording the /v1 base URL that Codex stores
// would address every request to /v1/v1, so the two clients cannot share a
// value even though they share a daemon.
func TestLocalClaudeBaseURLOmitsTheVersionSegment(t *testing.T) {
	dir := t.TempDir()
	if err := writeClaudeProxyEnv(dir, codexProxyRootURL(localBaseURL()), "tok"); err != nil {
		t.Fatalf("writeClaudeProxyEnv: %v", err)
	}
	env := readClaudeEnv(t, filepath.Join(dir, "settings.json"))

	base, _ := env["ANTHROPIC_BASE_URL"].(string)
	if base == "" {
		t.Fatal("no ANTHROPIC_BASE_URL written")
	}
	if got := localBaseURL(); base == got {
		t.Errorf("ANTHROPIC_BASE_URL = %q, which is Codex's /v1 form; Claude appends its own", base)
	}
}

// When the daemon enforces nothing, the dummy is correct: Claude Code requires
// the variable to be set, and an unenforced token is ignored. This is the case
// the old code assumed was the only one.
func TestUnenforcedDaemonStillGetsTheDummyToken(t *testing.T) {
	config := broker.Config{
		Version:          1,
		BaseURL:          broker.DefaultBaseURL,
		CredentialSource: broker.CredentialSourceLocal,
	}
	if got := cloudServerProxyToken(config); got != "" {
		t.Fatalf("server token = %q, want empty for this fixture", got)
	}

	dir := t.TempDir()
	client := cloudClientProxyToken(config, localBaseURL())
	if err := writeClaudeProxyEnv(dir, codexProxyRootURL(localBaseURL()), client); err != nil {
		t.Fatalf("writeClaudeProxyEnv: %v", err)
	}
	env := readClaudeEnv(t, filepath.Join(dir, "settings.json"))

	if got := env["ANTHROPIC_AUTH_TOKEN"]; got != "subrouter" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN = %v, want the dummy when nothing is enforced", got)
	}
}

// A Codex login says nothing about whether Claude Code can be served: the two
// draw from separate stores. Counting them as one pool is what let setup point
// Claude Code at a daemon holding no Claude credential.
func TestCodexAccountDoesNotUnblockClaudeClientConfig(t *testing.T) {
	in := basePlanInput()
	in.CredentialSource = broker.CredentialSourceLocal
	in.TeamModeReady = false
	in.LocalCodexAccounts = 1
	in.LocalClaudeAccounts = 0

	plan := buildSetupPlan(in)

	if !plan.selected(actionConfigCodex) {
		t.Error("Codex stayed blocked despite having a Codex account")
	}
	if plan.selected(actionConfigClaude) {
		t.Error("a Codex account unblocked Claude Code; that is the cross-provider bug")
	}
	item, ok := plan.find(actionConfigClaude)
	if !ok {
		t.Fatal("no Claude client item in the plan")
	}
	if item.Blocked == "" {
		t.Fatal("Claude item carries no blocked reason")
	}
	// The reason has to name the provider, or it sends someone to `sr add`
	// without saying which login is missing.
	if want := "Claude"; !strings.Contains(item.Blocked, want) {
		t.Errorf("blocked reason %q does not name %q", item.Blocked, want)
	}
}

// The mirror case: a Claude login must not unblock Codex.
func TestClaudeAccountDoesNotUnblockCodexClientConfig(t *testing.T) {
	in := basePlanInput()
	in.CredentialSource = broker.CredentialSourceLocal
	in.TeamModeReady = false
	in.LocalCodexAccounts = 0
	in.LocalClaudeAccounts = 1

	plan := buildSetupPlan(in)

	if plan.selected(actionConfigCodex) {
		t.Error("a Claude account unblocked Codex")
	}
	if !plan.selected(actionConfigClaude) {
		t.Error("Claude stayed blocked despite having a Claude account")
	}
}
