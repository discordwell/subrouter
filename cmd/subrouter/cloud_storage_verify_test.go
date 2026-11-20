package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
)

// tokenProbeDaemon fakes the daemon's proxy-root probe: HEAD / answers 204
// when the bearer token matches and 401 when it does not, mirroring
// baseURLProbeRequest handling in internal/proxy.
func tokenProbeDaemon(t *testing.T, token string) (*httptest.Server, *atomic.Value) {
	t.Helper()
	var sawAuth atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead || (r.URL.Path != "/" && r.URL.Path != "") {
			http.NotFound(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		sawAuth.Store(auth)
		if auth != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	return server, &sawAuth
}

func saveLocalConfigWithToken(t *testing.T, token string) string {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", configPath)
	if err := broker.SaveConfig(configPath, broker.Config{
		BaseURL:          "https://cmux.test",
		CredentialSource: broker.CredentialSourceLocal,
		LocalProxyToken:  token,
	}); err != nil {
		t.Fatal(err)
	}
	return configPath
}

func storageRunner(t *testing.T, client *http.Client, out, errOut *bytes.Buffer) srRunner {
	t.Helper()
	return srRunner{
		program: "sr",
		store:   accounts.CodexStore{Dir: t.TempDir()},
		in:      strings.NewReader(""),
		out:     out,
		errOut:  errOut,
		client:  client,
	}
}

// After a storage switch into an enforcing source, the probe proves the daemon
// accepts the exact secret the client configs were just given.
func TestStorageLocalVerifiesTheTokenAgainstTheDaemon(t *testing.T) {
	sandboxHostConfig(t)
	unlockRemoteGateForTest(t)
	saveLocalConfigWithToken(t, knownProxyToken)
	daemon, sawAuth := tokenProbeDaemon(t, knownProxyToken)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", daemon.URL+"/v1")

	var out, errOut bytes.Buffer
	runner := storageRunner(t, daemon.Client(), &out, &errOut)
	if err := runner.run(context.Background(), []string{"storage", "local"}); err != nil {
		t.Fatalf("storage local: %v\n%s%s", err, out.String(), errOut.String())
	}
	if got, _ := sawAuth.Load().(string); got != "Bearer "+knownProxyToken {
		t.Fatalf("probe sent %q, want the enforced bearer token", got)
	}
	if !strings.Contains(out.String(), "verified") {
		t.Fatalf("verification success was not reported:\n%s", out.String())
	}
	if strings.Contains(errOut.String(), "WARNING") {
		t.Fatalf("verified switch still warned:\n%s", errOut.String())
	}
}

// A 401 from the daemon means the running daemon and the just-written client
// configs disagree (or session leases are required, which the message names).
// The command still succeeds — the writes are complete and consistent — but
// the mismatch is reported loudly NOW, not at the next 401ing agent request.
func TestStorageLocalWarnsLoudlyWhenTheDaemonRejectsTheToken(t *testing.T) {
	sandboxHostConfig(t)
	unlockRemoteGateForTest(t)
	saveLocalConfigWithToken(t, knownProxyToken)
	daemon, _ := tokenProbeDaemon(t, "a-different-secret-entirely-0123456789abcdef")
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", daemon.URL+"/v1")

	var out, errOut bytes.Buffer
	runner := storageRunner(t, daemon.Client(), &out, &errOut)
	if err := runner.run(context.Background(), []string{"storage", "local"}); err != nil {
		t.Fatalf("storage local should not hard-fail on a probe 401: %v", err)
	}
	got := errOut.String()
	if !strings.Contains(got, "rejected the local proxy secret") ||
		!strings.Contains(got, "401") {
		t.Fatalf("no loud mismatch warning:\n%s", got)
	}
	if !strings.Contains(got, "session leases") {
		t.Fatalf("warning must name the --require-session-leases false-alarm case:\n%s", got)
	}
	if !strings.Contains(got, "daemon restart") || !strings.Contains(got, "setup") {
		t.Fatalf("warning carries no repair commands:\n%s", got)
	}
}

// No daemon answering is not an error: the switch is complete, and the next
// daemon start re-checks the secret. The skip is reported as an info line.
func TestStorageLocalSkipsVerificationWhenTheDaemonIsUnreachable(t *testing.T) {
	sandboxHostConfig(t)
	unlockRemoteGateForTest(t)
	saveLocalConfigWithToken(t, knownProxyToken)
	daemon := httptest.NewServer(http.NotFoundHandler())
	client := daemon.Client()
	url := daemon.URL
	daemon.Close()
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", url+"/v1")

	var out, errOut bytes.Buffer
	runner := storageRunner(t, client, &out, &errOut)
	if err := runner.run(context.Background(), []string{"storage", "local"}); err != nil {
		t.Fatalf("storage local failed on an unreachable daemon: %v\n%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "could not reach") {
		t.Fatalf("skipped verification was not reported:\n%s", out.String())
	}
}

// A settings.json that already routes Claude Code through this machine's
// daemon persists the proxy token, so a storage switch must hand it the secret
// the daemon now enforces — this is the strand that turned recoveries into
// bricked sessions.
func TestStorageSwitchRewritesClaudeSettingsRoutedThroughTheDaemon(t *testing.T) {
	// sandboxHostConfig, not a bare HOME sandbox: the container harness pins
	// CLAUDE_CONFIG_DIR, and an ambient value would otherwise punch through.
	home := sandboxHostConfig(t)
	unlockRemoteGateForTest(t)
	saveLocalConfigWithToken(t, knownProxyToken)
	daemon, _ := tokenProbeDaemon(t, knownProxyToken)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", daemon.URL+"/v1")

	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	seed, err := json.Marshal(map[string]any{
		"unrelatedSetting": true,
		"env": map[string]any{
			"ANTHROPIC_BASE_URL":   codexProxyRootURL(localBaseURL()),
			"ANTHROPIC_AUTH_TOKEN": "stale-rotated-away-secret",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(claudeDir, "settings.json")
	if err := os.WriteFile(settingsPath, seed, 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	runner := storageRunner(t, daemon.Client(), &out, &errOut)
	if err := runner.run(context.Background(), []string{"storage", "local"}); err != nil {
		t.Fatalf("storage local: %v\n%s%s", err, out.String(), errOut.String())
	}

	env := readClaudeEnv(t, settingsPath)
	if got, _ := env["ANTHROPIC_AUTH_TOKEN"].(string); got != knownProxyToken {
		t.Fatalf(
			"ANTHROPIC_AUTH_TOKEN = %q, want the enforced secret; Claude Code is stranded on the stale one",
			got,
		)
	}
	body, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["unrelatedSetting"] != true {
		t.Fatalf("unrelated settings were not preserved:\n%s", body)
	}
}

// A settings.json routed anywhere but this machine's daemon (or absent) is not
// a storage switch's to rewrite; hijacking a native or hosted Claude Code
// setup would be its own outage.
func TestStorageSwitchLeavesForeignClaudeSettingsAlone(t *testing.T) {
	// sandboxHostConfig, not a bare HOME sandbox: the container harness pins
	// CLAUDE_CONFIG_DIR, and an ambient value would otherwise punch through.
	home := sandboxHostConfig(t)
	unlockRemoteGateForTest(t)
	saveLocalConfigWithToken(t, knownProxyToken)
	daemon, _ := tokenProbeDaemon(t, knownProxyToken)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", daemon.URL+"/v1")

	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	seed := []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://sr.example.com","ANTHROPIC_AUTH_TOKEN":"customer-token"}}`)
	settingsPath := filepath.Join(claudeDir, "settings.json")
	if err := os.WriteFile(settingsPath, seed, 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	runner := storageRunner(t, daemon.Client(), &out, &errOut)
	if err := runner.run(context.Background(), []string{"storage", "local"}); err != nil {
		t.Fatalf("storage local: %v", err)
	}
	after, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, seed) {
		t.Fatalf("foreign settings.json was rewritten:\n%s", after)
	}
}

// serve's fail-loud stance, extended to local storage: a config file that
// selects local storage without a proxy secret means a writer skipped the key
// ceremony, and serving unauthenticated (or minting behind the clients' backs)
// is the outage. The daemon refuses with the repair command instead.
func TestServeRefusesLocalStorageWithoutProxySecret(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "cloud.json")
	if err := broker.SaveConfig(configPath, broker.Config{
		CredentialSource: broker.CredentialSourceLocal,
	}); err != nil {
		t.Fatal(err)
	}
	err := serve([]string{
		"--cloud-config", configPath,
		"--addr", "invalid:::",
	})
	if err == nil || !strings.Contains(err.Error(), "local proxy secret") ||
		!strings.Contains(err.Error(), "sr setup") {
		t.Fatalf("serve error = %v, want a local proxy secret refusal naming the repair", err)
	}
}

// A machine with no config file at all still serves unenforced, exactly as
// before: there is nothing recorded to disagree with.
func TestServeStillServesUnenforcedWithoutAConfigFile(t *testing.T) {
	sandboxHostConfig(t)
	t.Setenv("SUBROUTER_STATE_DIR", t.TempDir())
	err := serve([]string{
		"--cloud-config", filepath.Join(t.TempDir(), "missing.json"),
		"--cloud-credential-source", "local",
		"--addr", "invalid:::",
		"--fetch-usage=false",
		"--sr-switch-interval=0",
	})
	if err == nil {
		t.Fatal("serve unexpectedly listened on an invalid address")
	}
	if strings.Contains(err.Error(), "proxy secret") {
		t.Fatalf("fresh machine was refused for a missing proxy secret: %v", err)
	}
}
