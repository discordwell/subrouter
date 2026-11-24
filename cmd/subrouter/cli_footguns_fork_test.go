package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discordwell/subrouter/internal/accounts"
)

// `sr setup junk --plan` used to stop parsing at "junk", so --plan -- the flag
// whose whole promise is "change nothing" -- was silently dropped. cloudSetup
// then went past its --plan return, wrote cloud.json and minted the local proxy
// token (the key ceremony) before the review screen; only a non-terminal stdin
// kept it from going further. A stray word must fail the command before any of
// that.
func TestSetupStrayArgumentFailsBeforeWritingAnything(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cloudConfig := filepath.Join(home, ".config", "subrouter", "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudConfig)
	var out bytes.Buffer
	runner := srRunner{program: "sr", store: accounts.DefaultCodexStore(), out: &out, errOut: &out}
	err := runner.run(context.Background(), []string{"setup", "junk", "--plan"})
	if err == nil || !strings.Contains(err.Error(), "unexpected arguments: junk") {
		t.Errorf("err = %v, want an unexpected-arguments refusal", err)
	}
	if _, statErr := os.Stat(cloudConfig); !os.IsNotExist(statErr) {
		t.Errorf("setup with a stray argument wrote %s (stat err %v)", cloudConfig, statErr)
	}
	if t.Failed() {
		t.Logf("output:\n%s", out.String())
	}
}

// install-daemon deleted whatever sat at ~/bin/sr and ~/bin/cx. On this fork
// ~/bin/sr may be a status wrapper script, and an explicit install must now
// refuse to replace it (upstream #380's rule) and leave it byte-for-byte as it
// was.
func TestInstallDaemonRefusesToDeleteAForeignSR(t *testing.T) {
	home := t.TempDir()
	config := testDaemonConfig(home)
	wrapper := "#!/usr/bin/env python3\n# status wrapper\n"
	if err := os.MkdirAll(filepath.Dir(config.SRAliasPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.SRAliasPath, []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	err := installDaemonWithConfig(config, home, commandRunner{})
	if err == nil {
		t.Fatal("install-daemon replaced a foreign ~/bin/sr without --force-shims")
	}
	if body, readErr := os.ReadFile(config.SRAliasPath); readErr != nil || string(body) != wrapper {
		t.Fatalf("foreign ~/bin/sr changed: %q, %v", body, readErr)
	}
}

// sr setup on macOS installs the daemon through installDaemonForSetup. It is
// this fork's quickstart path, so a foreign sr must neither be deleted (the
// old bug) nor stop setup short of a daemon (upstream's hard refusal): the
// alias is skipped with a warning and everything else is installed.
func TestSetupDaemonInstallWarnsAndKeepsForeignSR(t *testing.T) {
	home := t.TempDir()
	config := testDaemonConfig(home)
	wrapper := "#!/usr/bin/env python3\n# status wrapper\n"
	if err := os.MkdirAll(filepath.Dir(config.SRAliasPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.SRAliasPath, []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	var warn bytes.Buffer
	config.ForeignShimWarn = &warn
	if err := installDaemonWithConfig(config, home, commandRunner{}); err != nil {
		t.Fatalf("setup's daemon install failed on a foreign ~/bin/sr: %v", err)
	}
	if body, err := os.ReadFile(config.SRAliasPath); err != nil || string(body) != wrapper {
		t.Fatalf("foreign ~/bin/sr changed: %q, %v", body, err)
	}
	if !strings.Contains(warn.String(), config.SRAliasPath) || !strings.Contains(warn.String(), "left") {
		t.Fatalf("no warning naming the skipped alias: %q", warn.String())
	}
	if target, err := os.Readlink(config.LegacyCXAliasPath); err != nil || target != config.InstallPath {
		t.Fatalf("cx alias = %q, %v; want %q (only the foreign alias is skipped)", target, err, config.InstallPath)
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents", defaultDaemonLabel+".plist")); err != nil {
		t.Fatalf("LaunchAgent not written after skipping the alias: %v", err)
	}
}

// installDaemonForSetup is the only caller that sets ForeignShimWarn; the
// explicit install-daemon command must keep upstream's refusal.
func TestInstallDaemonCommandDoesNotWarnInsteadOfRefusing(t *testing.T) {
	home := t.TempDir()
	config := testDaemonConfig(home)
	if config.ForeignShimWarn != nil {
		t.Fatal("test config must start without ForeignShimWarn")
	}
	if err := os.MkdirAll(filepath.Dir(config.LegacyCXAliasPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.LegacyCXAliasPath, []byte("foreign cx"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := installDaemonWithConfig(config, home, commandRunner{})
	if err == nil || !strings.Contains(err.Error(), "--force-shims") {
		t.Fatalf("err = %v, want a refusal naming --force-shims", err)
	}
}

func testDaemonConfig(home string) daemonConfig {
	return daemonConfig{
		Label:                defaultDaemonLabel,
		Addr:                 "127.0.0.1:31415",
		InstallPath:          filepath.Join(home, "bin", "subrouter"),
		LogDir:               filepath.Join(home, "Library", "Logs"),
		WorkingDirectory:     home,
		SRSwitchInterval:     "10m",
		Path:                 defaultDaemonPath(filepath.Join(home, "bin", "subrouter")),
		InstallSRAlias:       true,
		SRAliasPath:          filepath.Join(home, "bin", "sr"),
		InstallLegacyCXAlias: true,
		LegacyCXAliasPath:    filepath.Join(home, "bin", "cx"),
		Start:                false,
	}
}
