package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discordwell/subrouter/internal/broker"
	"github.com/discordwell/subrouter/internal/storepath"
)

// TestDockerHarnessCanaryResolvesEveryConfigSurfaceUnderTheAgentHome is the
// in-container isolation proof for the Docker test harness. It only runs when
// SUBROUTER_DOCKER_TEST_HARNESS=1, which the Dockerfile test stage bakes in;
// on a developer host it skips, because exercising real home resolution there
// is exactly what the harness exists to prevent. deploy/docker/test.sh
// --self-check runs this test by name with -v and fails when it reports
// anything other than PASS, so a silent skip inside the harness cannot hide.
func TestDockerHarnessCanaryResolvesEveryConfigSurfaceUnderTheAgentHome(t *testing.T) {
	if os.Getenv("SUBROUTER_DOCKER_TEST_HARNESS") != "1" {
		t.Skip("canary only runs inside the Docker test harness")
	}

	const agentHome = "/home/agent"
	// TestMain gives the process an empty HOME of its own (testenv.IsolateHome),
	// so check the one the container started it with.
	if realHomeAtStartup != agentHome {
		t.Fatalf("HOME at startup = %q, want %q", realHomeAtStartup, agentHome)
	}
	t.Setenv("HOME", realHomeAtStartup)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir() = %v, want the container agent home", err)
	}
	if home != agentHome {
		t.Fatalf("os.UserHomeDir() = %q, want %q", home, agentHome)
	}

	// Every path the runtime derives for an agent config surface must live
	// under the throwaway container home, whether it comes from an explicit
	// env override or from a HOME-based fallback.
	stateDir := storepath.StateDir()
	if !strings.HasPrefix(stateDir, agentHome+string(os.PathSeparator)) {
		t.Fatalf("storepath.StateDir() = %q, want a path under %q", stateDir, agentHome)
	}
	cloudConfig, err := broker.DefaultConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cloudConfig, agentHome+string(os.PathSeparator)) {
		t.Fatalf("broker.DefaultConfigPath() = %q, want a path under %q", cloudConfig, agentHome)
	}
	// The Dockerfile exports CLAUDE_CONFIG_DIR, but TestMain clears it with the
	// other store overrides (testenv.HomeOverrides), so this is the HOME fallback.
	claudeDir, err := defaultClaudeConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(claudeDir, agentHome+string(os.PathSeparator)) {
		t.Fatalf("defaultClaudeConfigDir() = %q, want a path under %q", claudeDir, agentHome)
	}

	// Prove the resolved home is really writable container scratch space by
	// writing through it and reading the write back.
	canaryPath := filepath.Join(home, ".subrouter", "docker-harness-canary")
	if err := os.MkdirAll(filepath.Dir(canaryPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canaryPath, []byte("isolated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(canaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "isolated\n" {
		t.Fatalf("canary readback = %q, want %q", body, "isolated\n")
	}
}
