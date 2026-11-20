package main

// Kept from the retired house-grammar tests (sr_grammar_test.go, 2026-09): the
// status view and locked-help guards are not grammar, and still hold.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/storepath"
)

// PATCHES.md 17 claims a keyless machine names no remote. It did not: the help
// still printed `sr export <name> [--copy] [--server <name>]`, `sr remove
// <email> [--server <name>]`, and three lines of prose about "the server",
// because the filter only ever inspected each line's SECOND field.
//
// Asserted through helpText(), the production path. PATCHES.md records that the
// earlier version of this test called stripRemoteHelp directly and let the
// mutant survive.
func TestLockedHelpNamesNoRemoteSurface(t *testing.T) {
	sandboxHostConfig(t)
	resetRemoteGateCache()
	t.Cleanup(resetRemoteGateCache)
	if remoteUnlocked() {
		t.Fatal("sandbox has no remote.key but the gate read unlocked")
	}
	runner := srRunner{program: "sr"}
	help := strings.ToLower(runner.helpText())
	for _, token := range []string{"--server", "<server>", "server", "remote", "tenant", "cmux"} {
		if strings.Contains(help, token) {
			t.Errorf("locked help names the remote surface via %q", token)
		}
	}
	// Calibration: the filter must not have simply emptied the help.
	for _, token := range []string{"sr add", "sr status"} {
		if !strings.Contains(help, token) {
			t.Errorf("locked help lost %q, so this test proves nothing", token)
		}
	}
}

// `sr` and `sr status` showed the BOX and nothing else on any machine with a
// configured server -- a laptop whose own Claude profiles were actively
// serving traffic reported only the box's accounts in its total.
//
// Three copies of one early return caused it: status, defaultInteractive, and
// the dispatch routing (shouldRouteSRCommand returns true by default, so
// `sr status` never even reached status). The combined renderer existed the
// whole time -- the help text has always promised "Local accounts first, then
// each configured server" -- and only those returns stood in front of it.
func TestUsageViewsShowLocalAndServerTogether(t *testing.T) {
	sandboxHostConfig(t)
	resetRemoteGateCache()
	t.Cleanup(resetRemoteGateCache)

	codexDir := storepath.CodexDir()
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "remote.key"),
		[]byte(strings.Repeat("k", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	if !remoteUnlocked() {
		t.Fatal("fixture failed to unlock the remote gate")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	servers := fmt.Sprintf(`{"servers":[{"name":"box","url":%q,"adminToken":"t"}],"default":"box"}`, srv.URL)
	if err := os.WriteFile(filepath.Join(codexDir, "servers.json"), []byte(servers), 0o600); err != nil {
		t.Fatal(err)
	}

	// The credential source here is Legacy -- the default for a machine with no
	// usable cloud.json, which is exactly the machine that lost its local view.
	config, err := cloudModeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := config.EffectiveCredentialSource(); got != broker.CredentialSourceLegacy {
		t.Fatalf("fixture is not exercising the broken path: credential source = %v", got)
	}

	var out strings.Builder
	runner := srRunner{
		program: "sr", in: strings.NewReader(""),
		out: &out, errOut: &out,
		store:  accounts.DefaultCodexStore(),
		client: srv.Client(),
	}
	if err := runner.status(context.Background(), srStatusOptions{}); err != nil {
		t.Fatalf("status: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Local (this machine)") {
		t.Errorf("status dropped this machine's accounts on a legacy source:\n%s", got)
	}
	if !strings.Contains(got, "Server: box") {
		t.Errorf("status dropped the server section, so this test proves nothing:\n%s", got)
	}
}

// The other half: status must actually REACH status on a legacy machine.
func TestStatusIsNotRoutedAwayToTheServer(t *testing.T) {
	if shouldRouteSRCommand("status") {
		t.Error("status is routed to the server, which replaces the combined view with the box alone")
	}
}
