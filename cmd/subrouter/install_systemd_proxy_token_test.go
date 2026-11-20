package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// install-systemd rewrites /etc/default/<service> WHOLESALE. Any key absent
// from its template is deleted on the next run -- which is what happened to
// SUBROUTER_PROXY_TOKEN, the credential that gates /v1/* and therefore spends
// every pooled subscription.
//
// The resulting failure is not a leak (the non-loopback startup guard refuses
// to serve without it) but it IS an outage, arriving one unrelated re-install
// later and reading as "the upgrade broke the box".

func TestSystemdDefaultsCarryTheProxyToken(t *testing.T) {
	defaults := systemdDefaults(systemdConfig{
		Addr:             "100.64.0.1:31415",
		Home:             "/var/lib/subrouter",
		SessionsPath:     "/var/lib/subrouter/sessions.json",
		SRSwitchInterval: "10m",
		AdminToken:       "admin-secret",
		ProxyToken:       "proxy-secret",
	})
	if !strings.Contains(defaults, "SUBROUTER_PROXY_TOKEN=") {
		t.Fatalf("rendered defaults have no SUBROUTER_PROXY_TOKEN key:\n%s", defaults)
	}
	if !strings.Contains(defaults, "proxy-secret") {
		t.Fatalf("proxy token value missing from rendered defaults:\n%s", defaults)
	}
}

// The preservation path: a re-run that does not re-pass the token must keep the
// one already on disk. This is the actual regression -- bootstrap re-running to
// upgrade a binary should not silently disarm the daemon.
func TestReinstallPreservesAnExistingProxyToken(t *testing.T) {
	dir := t.TempDir()
	defaultPath := filepath.Join(dir, "subrouter")
	existing := "SUBROUTER_ADMIN_TOKEN=admin-secret\n" +
		"SUBROUTER_PROXY_TOKEN=proxy-secret\n" +
		"SUBROUTER_EXTRA_ARGS=--public-addr 0.0.0.0:31416\n"
	if err := os.WriteFile(defaultPath, []byte(existing), 0o600); err != nil {
		t.Fatalf("seed defaults: %v", err)
	}

	config := systemdConfig{} // no tokens re-passed, exactly like a plain re-run
	applyExistingSystemdDefaults(&config, defaultPath)

	if config.ProxyToken != "proxy-secret" {
		t.Fatalf("proxy token = %q after re-install, want it preserved; losing it stops the "+
			"daemon from starting on a non-loopback bind", config.ProxyToken)
	}
	// Sanity: the keys that already worked must keep working, so a regression
	// here is attributable to this change rather than assumed away.
	if config.AdminToken != "admin-secret" {
		t.Errorf("admin token = %q, want preserved", config.AdminToken)
	}
	if !strings.Contains(config.ExtraArgs, "--public-addr") {
		t.Errorf("extra args = %q, want the public listener preserved", config.ExtraArgs)
	}
}

// Clearing extra args must be possible, or a public listener cannot be removed.
//
// `--extra-args ""` and omitting the flag are the same empty string, so without
// tracking which one happened the preservation path restores the old value. The
// operator is then told the listener was removed while it keeps serving --- a
// failure whose direction is "still exposed".
func TestExplicitlyEmptyExtraArgsClearsAPublicListener(t *testing.T) {
	dir := t.TempDir()
	defaultPath := filepath.Join(dir, "subrouter")
	stored := "SUBROUTER_EXTRA_ARGS=--public-addr 0.0.0.0:31416\n"
	if err := os.WriteFile(defaultPath, []byte(stored), 0o600); err != nil {
		t.Fatalf("seed defaults: %v", err)
	}

	cleared := systemdConfig{ExtraArgs: "", ExtraArgsExplicit: true, ReplaceLegacy: false}
	applyExistingSystemdDefaults(&cleared, defaultPath)
	if cleared.ExtraArgs != "" {
		t.Fatalf("extra args = %q after an explicit clear, want empty: the public listener "+
			"would keep running while the operator is told it was removed", cleared.ExtraArgs)
	}

	// Omitting the flag must still preserve, which is what makes a binary-only
	// upgrade safe.
	preserved := systemdConfig{ExtraArgs: "", ExtraArgsExplicit: false, ReplaceLegacy: false}
	applyExistingSystemdDefaults(&preserved, defaultPath)
	if !strings.Contains(preserved.ExtraArgs, "--public-addr") {
		t.Fatalf("extra args = %q when the flag was omitted, want the stored value preserved",
			preserved.ExtraArgs)
	}
}

// An explicitly passed token wins over the stored one, or rotation is impossible.
func TestExplicitProxyTokenOverridesTheStoredOne(t *testing.T) {
	dir := t.TempDir()
	defaultPath := filepath.Join(dir, "subrouter")
	if err := os.WriteFile(defaultPath, []byte("SUBROUTER_PROXY_TOKEN=old\n"), 0o600); err != nil {
		t.Fatalf("seed defaults: %v", err)
	}
	config := systemdConfig{ProxyToken: "rotated"}
	applyExistingSystemdDefaults(&config, defaultPath)
	if config.ProxyToken != "rotated" {
		t.Fatalf("proxy token = %q, want the explicitly passed value: otherwise it cannot be rotated",
			config.ProxyToken)
	}
}

// The env file must not be world-readable when it carries a pool-spending
// credential. This held only incidentally before -- the mode keyed on the admin
// and import tokens, so it was 0600 because bootstrap sets an admin token too,
// not because the proxy token required it.
func TestDefaultsFileIsPrivateWhenItCarriesOnlyTheProxyToken(t *testing.T) {
	mode := systemdDefaultsMode(systemdConfig{ProxyToken: "proxy-secret"})
	if mode != 0o600 {
		t.Fatalf("defaults mode = %#o with only a proxy token, want 0600: that token spends every "+
			"pooled subscription and must not be world-readable", mode)
	}
	// Unchanged for a genuinely secretless config.
	if mode := systemdDefaultsMode(systemdConfig{}); mode != 0o644 {
		t.Errorf("defaults mode = %#o with no tokens, want 0644 (unchanged)", mode)
	}
}

// A newline in the token would inject a line into the env file, so it is
// rejected like every other rendered value.
func TestProxyTokenWithANewlineIsRejected(t *testing.T) {
	err := validateSystemdConfig(systemdConfig{
		ServiceName:      "subrouter",
		User:             "subrouter",
		Group:            "subrouter",
		Addr:             "100.64.0.1:31415",
		Home:             "/var/lib/subrouter",
		InstallPath:      "/usr/local/bin/subrouter",
		SessionsPath:     "/var/lib/subrouter/sessions.json",
		SRSwitchInterval: "10m",
		ProxyToken:       "secret\nSUBROUTER_ADMIN_TOKEN=attacker",
	})
	if err == nil {
		t.Fatal("a proxy token containing a newline was accepted; it would inject an env-file line")
	}
	if !strings.Contains(err.Error(), "proxy-token") {
		t.Errorf("error = %q, want it to name proxy-token", err)
	}
}

// Stdin ordering is a compatibility surface: bootstrap scripts pipe admin
// then import. The proxy token is appended LAST, so those callers keep
// working; inserting it earlier would silently reassign their tokens.
func TestProxyTokenIsReadLastFromStdin(t *testing.T) {
	dir := t.TempDir()
	input := strings.NewReader("admin-value\nimport-value\nproxy-value\n")
	err := installSystemdWithInput([]string{
		"--dry-run",
		"--service", "subrouter",
		"--home", dir,
		"--sessions", filepath.Join(dir, "sessions.json"),
		"--admin-token-stdin",
		"--account-import-token-stdin",
		"--proxy-token-stdin",
	}, input)
	if err != nil {
		t.Fatalf("install-systemd --dry-run: %v", err)
	}
}

func TestProxyTokenFlagAndStdinConflict(t *testing.T) {
	err := installSystemdWithInput([]string{
		"--dry-run", "--proxy-token", "a", "--proxy-token-stdin",
	}, strings.NewReader("b\n"))
	if err == nil {
		t.Fatal("passing --proxy-token and --proxy-token-stdin together was accepted")
	}
}
