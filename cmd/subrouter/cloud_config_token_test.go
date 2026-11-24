package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/internal/broker"
)

// knownProxyToken is a stand-in for the secret the daemon enforces. Its only
// requirement is that it survives byte-for-byte: localProxyAuthorized compares
// the client's bearer token to this value with constant-time equality, so a
// changed token is indistinguishable from no token at all.
const knownProxyToken = "0123456789abcdef0123456789abcdef0123456789a"

// writeParseableInvalidConfig writes a config that is valid JSON but that
// Config.Validate rejects. This is the shape that matters: LoadConfig returns
// the same error for "not JSON" and "JSON that failed validation", so a config
// the daemon is happily enforcing today becomes "corrupt" the moment someone
// tightens a validation rule. Every field except credentialSource is
// well-formed.
func writeParseableInvalidConfig(t *testing.T, path string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"version":          1,
		"baseUrl":          "https://cmux.test",
		"localProxyToken":  knownProxyToken,
		"credentialSource": "bogus",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	// Guard the premise: if this ever parses cleanly the tests prove nothing.
	if _, err := broker.LoadConfig(path); err == nil {
		t.Fatal("fixture no longer fails validation; pick another invalid field")
	}
}

func onDiskProxyToken(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var raw struct {
		LocalProxyToken string `json:"localProxyToken"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return raw.LocalProxyToken
}

// A config that parses still holds a usable secret, so recovery has no reason
// to discard it. Dropping it here is what lets SaveConfig mint a replacement:
// the daemon then enforces a token that no already-written client config
// carries, and every running agent session 401s at once with no fallback,
// because the proxy env has overridden the native login.
func TestLoginRecoveryKeepsTheTokenWhenTheConfigParses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	configPath := filepath.Join(t.TempDir(), "cloud.json")
	writeParseableInvalidConfig(t, configPath)

	config, recovered, err := loadCloudConfigForLogin(configPath)
	if err != nil {
		t.Fatalf("loadCloudConfigForLogin: %v", err)
	}
	if !recovered {
		t.Error("an unusable config was not reported as recovered")
	}
	if config.LocalProxyToken != knownProxyToken {
		t.Fatalf(
			"recovery dropped the enforced proxy token (got %q); "+
				"saving this config mints a new one and 401s every live session",
			config.LocalProxyToken,
		)
	}
}

// The end-to-end shape of the same failure: the token on disk must be the same
// secret before and after a recovery command, because the daemon and every
// already-written client config agree on it.
func TestStorageLocalRecoveryPreservesTheTokenOnDisk(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	unlockRemoteGateForTest(t)
	configPath := filepath.Join(t.TempDir(), "cloud.json")
	writeParseableInvalidConfig(t, configPath)
	t.Setenv("SUBROUTER_CLOUD_CONFIG", configPath)

	local := healthServer(t, http.StatusOK)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")

	var out bytes.Buffer
	runner := srRunner{
		program: "sr",
		store:   accounts.CodexStore{Dir: t.TempDir()},
		in:      strings.NewReader(""),
		out:     &out,
		errOut:  &out,
		client:  local.Client(),
	}
	if err := runner.run(
		context.Background(),
		[]string{"storage", "local"},
	); err != nil {
		t.Fatalf("storage local: %v\n%s", err, out.String())
	}

	if got := onDiskProxyToken(t, configPath); got != knownProxyToken {
		t.Fatalf(
			"storage local rotated the enforced proxy token to %q; "+
				"the daemon now rejects every client config already on this machine",
			got,
		)
	}
}

// Saving is not a key ceremony. A caller that loaded a config, edited an
// unrelated field, and saved — without ever holding the token — must not cause
// a new one to be minted, because the mint takes effect at the next daemon
// restart while every written client config still carries the old secret.
func TestSaveConfigPreservesAnExistingTokenItWasNotGiven(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "cloud.json")
	if err := broker.SaveConfig(configPath, broker.Config{
		BaseURL:          "https://cmux.test",
		LocalProxyToken:  knownProxyToken,
		CredentialSource: broker.CredentialSourceLocal,
	}); err != nil {
		t.Fatal(err)
	}

	if err := broker.SaveConfig(configPath, broker.Config{
		BaseURL:          "https://cmux.test",
		CredentialSource: broker.CredentialSourceLocal,
	}); err != nil {
		t.Fatal(err)
	}

	if got := onDiskProxyToken(t, configPath); got != knownProxyToken {
		t.Fatalf("SaveConfig replaced the on-disk token with %q", got)
	}
}

// A file that does not parse has no token to keep, so minting a replacement is
// the only option — but the original bytes must survive for a human to
// inspect, because they may hold the only copy of the secret every client
// config on the machine still references.
func TestUnparseableConfigIsBackedUpBeforeRecovery(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	configPath := filepath.Join(t.TempDir(), "cloud.json")
	if err := os.WriteFile(configPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	config, recovered, err := loadCloudConfigForLogin(configPath)
	if err != nil {
		t.Fatalf("loadCloudConfigForLogin: %v", err)
	}
	if !recovered || config.BaseURL != broker.DefaultBaseURL {
		t.Fatalf("recovery = (%+v, %v), want a fresh default config", config, recovered)
	}

	backup, err := os.ReadFile(configPath + ".corrupt")
	if err != nil {
		t.Fatalf("unparseable config was discarded without a backup: %v", err)
	}
	if string(backup) != "{" {
		t.Fatalf("backup = %q, want the original bytes", backup)
	}
}
