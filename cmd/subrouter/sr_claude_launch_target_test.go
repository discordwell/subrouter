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

	"github.com/discordwell/subrouter/internal/agents/claude"
	"github.com/discordwell/subrouter/internal/broker"
)

// localConfig is a machine keeping its credentials on itself, which is the only
// credential source that reaches claudeLaunchTargetTo.
func localConfig(proxyToken string) broker.Config {
	return broker.Config{
		CredentialSource: broker.CredentialSourceLocal,
		LocalProxyToken:  proxyToken,
	}
}

// launchTargetEnv points localBaseURL at a test daemon and clears the pin, so a
// test observes the launch decision rather than the developer's environment.
func launchTargetEnv(t *testing.T, local string) {
	t.Helper()
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local)
	t.Setenv("SUBROUTER_DISABLE_FALLBACK", "")
}

func noopStarter() daemonStarter { return func() error { return nil } }

func capacity(has, known bool) localCapacityProbe {
	return func(context.Context) (bool, bool) { return has, known }
}

// deadServerURL is an address nothing listens on, standing in for an
// unreachable host.
func deadServerURL(t *testing.T) string {
	t.Helper()
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := dead.URL
	dead.Close()
	return url
}

func launchTarget(
	t *testing.T,
	server srServerConfig,
	hasServer bool,
	local string,
	probe localCapacityProbe,
) (string, string) {
	t.Helper()
	target, token, err := claudeLaunchTargetTo(
		context.Background(),
		fallbackHTTPClient(),
		server,
		hasServer,
		local,
		noopStarter(),
		localConfig("local-proxy-token"),
		probe,
		io.Discard,
	)
	if err != nil {
		t.Fatalf("claudeLaunchTargetTo: %v", err)
	}
	return target, token
}

// The headline behaviour: a machine with its own capacity keeps the launch,
// even when a healthy server is configured.
func TestClaudeLaunchTargetPrefersLocalWhileItHasCapacity(t *testing.T) {
	server := healthServer(t, http.StatusOK)
	local := healthServer(t, http.StatusOK)
	launchTargetEnv(t, local.URL)

	target, token := launchTarget(t,
		srServerConfig{Name: "home", URL: server.URL}, true, local.URL,
		capacity(true, true))

	if target != local.URL {
		t.Fatalf("target = %q, want the local daemon %q", target, local.URL)
	}
	if token != "local-proxy-token" {
		t.Fatalf("token = %q, want the local proxy token", token)
	}
}

func TestClaudeLaunchTargetSpillsToServerWhenLocalIsLow(t *testing.T) {
	server := healthServer(t, http.StatusOK)
	local := healthServer(t, http.StatusOK)
	launchTargetEnv(t, local.URL)

	target, token := launchTarget(t,
		srServerConfig{Name: "home", URL: server.URL}, true, local.URL,
		capacity(false, true))

	if target != server.URL {
		t.Fatalf("target = %q, want the configured server %q", target, server.URL)
	}
	// A server enforcing no token of its own still needs the variable set.
	if token != claudeProxyPlaceholderToken {
		t.Fatalf("token = %q, want placeholder %q", token, claudeProxyPlaceholderToken)
	}
}

func TestClaudeLaunchTargetSpillSendsTenantKeyToTenantScopedServer(t *testing.T) {
	server := healthServer(t, http.StatusOK)
	local := healthServer(t, http.StatusOK)
	launchTargetEnv(t, local.URL)

	config := srServerConfig{Name: "cmux", URL: server.URL, TenantKey: "tenant-abc"}
	target, token := launchTarget(t, config, true, local.URL, capacity(false, true))

	want, err := serverProxyRootURL(config)
	if err != nil {
		t.Fatalf("serverProxyRootURL: %v", err)
	}
	if target != want {
		t.Fatalf("target = %q, want tenant-scoped %q", target, want)
	}
	if token != "tenant-abc" {
		t.Fatalf("token = %q, want the tenant key", token)
	}
}

// An unreadable local pool must not be mistaken for an exhausted one: spending
// the server's pool on a guess is worse than staying put.
func TestClaudeLaunchTargetStaysLocalWhenCapacityIsUnknown(t *testing.T) {
	server := healthServer(t, http.StatusOK)
	local := healthServer(t, http.StatusOK)
	launchTargetEnv(t, local.URL)

	target, _ := launchTarget(t,
		srServerConfig{Name: "home", URL: server.URL}, true, local.URL,
		capacity(false, false))

	if target != local.URL {
		t.Fatalf("target = %q, want the local daemon %q on an unknown local pool", target, local.URL)
	}
}

// Local is low but the server is unreachable: stay local and let the local
// daemon report exhaustion, rather than launching against a dead host.
func TestClaudeLaunchTargetStaysLocalWhenSpillTargetIsUnreachable(t *testing.T) {
	local := healthServer(t, http.StatusOK)
	launchTargetEnv(t, local.URL)

	target, token := launchTarget(t,
		srServerConfig{Name: "home", URL: deadServerURL(t)}, true, local.URL,
		capacity(false, true))

	if target != local.URL {
		t.Fatalf("target = %q, want the local daemon %q", target, local.URL)
	}
	if token != "local-proxy-token" {
		t.Fatalf("token = %q, want the local proxy token", token)
	}
}

func TestClaudeLaunchTargetWithoutConfiguredServerUsesLocal(t *testing.T) {
	local := healthServer(t, http.StatusOK)
	launchTargetEnv(t, local.URL)

	target, token := launchTarget(t, srServerConfig{}, false, local.URL, capacity(false, true))

	if target != local.URL {
		t.Fatalf("target = %q, want the local daemon %q", target, local.URL)
	}
	if token != "local-proxy-token" {
		t.Fatalf("token = %q, want the local proxy token", token)
	}
}

// A server entry that already addresses this machine must not be treated as
// spill capacity, and must still receive the local proxy token.
func TestClaudeLaunchTargetTreatsLocalServerEntryAsLocal(t *testing.T) {
	local := healthServer(t, http.StatusOK)
	launchTargetEnv(t, local.URL)

	target, token := launchTarget(t,
		srServerConfig{Name: "self", URL: local.URL}, true, local.URL,
		capacity(false, true))

	if target != local.URL {
		t.Fatalf("target = %q, want the local daemon %q", target, local.URL)
	}
	if token != "local-proxy-token" {
		t.Fatalf("token = %q, want the local proxy token", token)
	}
}

// SUBROUTER_DISABLE_FALLBACK keeps its documented meaning of pinning the
// configured server, which here means skipping the local-first preference
// entirely — even while local still has capacity.
func TestClaudeLaunchTargetPinSkipsLocalPreference(t *testing.T) {
	server := healthServer(t, http.StatusOK)
	local := healthServer(t, http.StatusOK)
	launchTargetEnv(t, local.URL)
	t.Setenv("SUBROUTER_DISABLE_FALLBACK", "1")

	target, _ := launchTarget(t,
		srServerConfig{Name: "home", URL: server.URL}, true, local.URL,
		capacity(true, true))

	if target != server.URL {
		t.Fatalf("target = %q, want the pinned server %q", target, server.URL)
	}
}

// The local daemon's proxy token authenticates this machine's own daemon. It
// must never be handed to another host, which is what cloudClientProxyToken's
// loopback check exists to guarantee.
func TestClaudeLaunchTargetNeverSendsLocalProxyTokenToRemote(t *testing.T) {
	server := healthServer(t, http.StatusOK)
	local := healthServer(t, http.StatusOK)
	launchTargetEnv(t, local.URL)

	_, token, err := claudeLaunchTargetTo(
		context.Background(),
		fallbackHTTPClient(),
		srServerConfig{Name: "home", URL: server.URL},
		true,
		local.URL,
		noopStarter(),
		localConfig("secret-local-proxy-token"),
		capacity(false, true),
		io.Discard,
	)
	if err != nil {
		t.Fatalf("claudeLaunchTargetTo: %v", err)
	}
	if token == "secret-local-proxy-token" {
		t.Fatal("the local proxy token was sent to a remote server")
	}
}

// claudeLaunchSpills is the one PATCHES 15 decision, shared by `sr claude` and
// upstream's profileless `sr claude proxy` (resolveClaudeProxyRoute). Upstream
// sent every profileless launch to the selected remote; here the selected
// remote is only spill capacity.
func TestClaudeLaunchSpillsOnlyWhenLocalIsLow(t *testing.T) {
	server := healthServer(t, http.StatusOK)
	local := healthServer(t, http.StatusOK)
	launchTargetEnv(t, local.URL)
	home := srServerConfig{Name: "home", URL: server.URL}
	spills := func(server srServerConfig, hasServer bool, probe localCapacityProbe) bool {
		return claudeLaunchSpills(context.Background(), fallbackHTTPClient(), server, hasServer, local.URL, probe, io.Discard)
	}

	if spills(home, true, capacity(true, true)) {
		t.Fatal("spilled to the server while this machine still had capacity: the box became the default target")
	}
	if spills(home, true, capacity(false, false)) {
		t.Fatal("spilled on an unreadable local pool")
	}
	if !spills(home, true, capacity(false, true)) {
		t.Fatal("did not spill once local accounts were below the new-session floor")
	}
	if spills(srServerConfig{Name: "home", URL: deadServerURL(t)}, true, capacity(false, true)) {
		t.Fatal("spilled to an unreachable server")
	}
	if spills(srServerConfig{}, false, capacity(false, true)) {
		t.Fatal("spilled with no server configured")
	}
	t.Setenv("SUBROUTER_DISABLE_FALLBACK", "1")
	if !spills(home, true, capacity(true, true)) {
		t.Fatal("SUBROUTER_DISABLE_FALLBACK did not pin the configured server")
	}
}

// Upstream narrowed claudeLaunchesAgent to bare and `proxy` (f39439a). That
// merges without a conflict and sends `sr claude --flag` and `sr claude <name>`
// straight to a local profile, past the local daemon -- no PATCHES 15 spill,
// no PATCHES 16 peer fall-through. This drives srRunner.claude itself, because
// claudeLaunchTargetTo's own tests stay green through that revert.
func TestSRClaudeFlagAndProfileLaunchesGoThroughTheLocalPool(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "flag", args: []string{"--model", "opus"}},
		{name: "profile", args: []string{"work", "-p", "hi"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(home, "state"))
			// Keyless, so cloudModeConfig resolves Local storage (PATCHES 17
			// C6) and no box is ever consulted.
			t.Setenv("SUBROUTER_CLOUD_CONFIG", filepath.Join(home, "missing-cloud.json"))
			resetRemoteGateCache()
			t.Cleanup(resetRemoteGateCache)
			local := healthServer(t, http.StatusOK)
			launchTargetEnv(t, local.URL)
			if _, err := claude.DefaultStore().CreateProfile("work"); err != nil {
				t.Fatal(err)
			}

			binDir := filepath.Join(home, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			settingsCopy := filepath.Join(home, "launch-settings.json")
			script := "#!/bin/sh\nprevious=\nfor arg in \"$@\"; do\n  if [ \"$previous\" = --settings ]; then cat \"$arg\" > " +
				shellQuote(settingsCopy) + "; fi\n  previous=$arg\ndone\n"
			if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			runner := srRunner{program: "sr", in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
			if err := runner.claude(context.Background(), tc.args); err != nil {
				t.Fatalf("sr claude %v: %v", tc.args, err)
			}
			body, err := os.ReadFile(settingsCopy)
			if err != nil {
				t.Fatalf("Claude was not launched with pool settings: %v", err)
			}
			var settings struct {
				Env map[string]string `json:"env"`
			}
			if err := json.Unmarshal(body, &settings); err != nil {
				t.Fatal(err)
			}
			if !sameEndpoint(settings.Env["ANTHROPIC_BASE_URL"], local.URL) {
				t.Fatalf("sr claude %v launched against %q, want the local daemon %q",
					tc.args, settings.Env["ANTHROPIC_BASE_URL"], local.URL)
			}
			if settings.Env["ANTHROPIC_CUSTOM_HEADERS"] != "X-Subrouter-Agent: claude" {
				t.Fatalf("pool launch lost the agent marker: %+v", settings.Env)
			}
		})
	}
}
