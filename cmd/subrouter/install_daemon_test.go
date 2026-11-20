package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/broker"
)

func TestLaunchAgentPlistUsesMacOSLocalDefaults(t *testing.T) {
	home := "/Users/alice"
	t.Setenv(
		"SUBROUTER_CLOUD_CONFIG",
		"/Users/alice/private/subrouter-team.json",
	)
	config := daemonConfig{
		Label:            defaultDaemonLabel,
		Addr:             "127.0.0.1:31415",
		InstallPath:      "/Users/alice/bin/subrouter",
		LogDir:           "/Users/alice/Library/Logs",
		WorkingDirectory: "/Users/alice/fun/subrouter",
		SRSwitchInterval: "10m",
		Path:             defaultDaemonPath("/Users/alice/bin/subrouter"),
	}
	plist, err := launchAgentPlist(config, home)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<string>ai.manaflow.subrouter</string>",
		"<string>/Users/alice/bin/subrouter</string>",
		"<string>serve</string>",
		"<string>--addr</string>",
		"<string>127.0.0.1:31415</string>",
		"<string>--sr-switch-interval</string>",
		"<string>10m</string>",
		"<string>--cloud-config</string>",
		"<string>/Users/alice/private/subrouter-team.json</string>",
		"<string>/Users/alice/fun/subrouter</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist missing %q:\n%s", want, plist)
		}
	}
	if strings.Contains(plist, "<string>--transcripts</string>") {
		t.Fatalf("plist enabled transcripts by default:\n%s", plist)
	}
}

func TestLaunchAgentPlistIncludesConfiguredTranscripts(t *testing.T) {
	home := "/Users/alice"
	config := daemonConfig{
		Label:            defaultDaemonLabel,
		Addr:             "127.0.0.1:31415",
		InstallPath:      "/Users/alice/bin/subrouter",
		TranscriptsDir:   "/Users/alice/.subrouter/transcripts",
		LogDir:           "/Users/alice/Library/Logs",
		WorkingDirectory: "/Users/alice/fun/subrouter",
		SRSwitchInterval: "10m",
		Path:             defaultDaemonPath("/Users/alice/bin/subrouter"),
	}
	plist, err := launchAgentPlist(config, home)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<string>--transcripts</string>",
		"<string>/Users/alice/.subrouter/transcripts</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist missing %q:\n%s", want, plist)
		}
	}
}

func TestInstallDaemonWritesPlistAndInstallsExecutableWithoutStarting(t *testing.T) {
	home := t.TempDir()
	config := daemonConfig{
		Label:                defaultDaemonLabel,
		Addr:                 "127.0.0.1:31415",
		InstallPath:          filepath.Join(home, "bin", "subrouter"),
		TranscriptsDir:       filepath.Join(home, ".subrouter", "transcripts"),
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
	if err := installDaemonWithConfig(config, home, commandRunner{}); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		config.InstallPath,
		config.SRAliasPath,
		config.LegacyCXAliasPath,
		config.TranscriptsDir,
		filepath.Join(home, "Library", "LaunchAgents", "ai.manaflow.subrouter.plist"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected %s: %v", path, err)
		}
	}

	body, err := os.ReadFile(filepath.Join(home, "Library", "LaunchAgents", "ai.manaflow.subrouter.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "<string>10m</string>") {
		t.Fatalf("plist did not preserve auto-switch interval:\n%s", body)
	}
	target, err := os.Readlink(config.SRAliasPath)
	if err != nil {
		t.Fatal(err)
	}
	if target != config.InstallPath {
		t.Fatalf("sr alias target = %q, want %q", target, config.InstallPath)
	}
	target, err = os.Readlink(config.LegacyCXAliasPath)
	if err != nil {
		t.Fatal(err)
	}
	if target != config.InstallPath {
		t.Fatalf("cx alias target = %q, want %q", target, config.InstallPath)
	}
}

// A tailnet bind is the whole point of sharing a pool, so it must be installable —
// but only in a state `serve` would itself accept at startup. install-daemon used
// to refuse every non-loopback address outright, which made the chosen topology
// unreachable through the supported install path.
func TestValidateDaemonConfigRefusesNonLoopbackBindWithoutAdminToken(t *testing.T) {
	base := daemonConfig{
		Label:            defaultDaemonLabel,
		InstallPath:      "/Users/alice/bin/subrouter",
		LogDir:           "/Users/alice/Library/Logs",
		WorkingDirectory: "/Users/alice/fun/subrouter",
		SRSwitchInterval: "10m",
	}
	for _, addr := range []string{
		"100.101.102.103:31415",     // tailnet IPv4
		"mac.tail1234.ts.net:31415", // MagicDNS: unresolvable at validation time
		":31415",                    // every interface
	} {
		config := base
		config.Addr = addr
		err := validateDaemonConfig(config)
		if err == nil {
			t.Fatalf("addr %q: expected refusal without an admin token", addr)
		}
		if !strings.Contains(err.Error(), "admin token") {
			t.Fatalf("addr %q: error should name the missing admin token, got %v", addr, err)
		}
	}
}

// writeAdminTokenFile creates a token file the daemon could actually read, so
// validation exercises the real reader rather than a path that happens to
// look plausible.
func writeAdminTokenFile(t *testing.T, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "admin-token")
	if err := os.WriteFile(path, []byte("secret-admin-token\n"), mode); err != nil {
		t.Fatalf("write admin token file: %v", err)
	}
	return path
}

func TestValidateDaemonConfigAllowsLoopbackAndTokenedTailnetBinds(t *testing.T) {
	tokenFile := writeAdminTokenFile(t, 0o600)
	base := daemonConfig{
		Label:            defaultDaemonLabel,
		InstallPath:      "/Users/alice/bin/subrouter",
		LogDir:           "/Users/alice/Library/Logs",
		WorkingDirectory: "/Users/alice/fun/subrouter",
		SRSwitchInterval: "10m",
	}
	cases := []struct {
		name   string
		mutate func(*daemonConfig)
	}{
		{"loopback needs no token", func(c *daemonConfig) { c.Addr = "127.0.0.1:31415" }},
		{"localhost needs no token", func(c *daemonConfig) { c.Addr = "localhost:31415" }},
		{"tailnet with token file", func(c *daemonConfig) {
			c.Addr = "100.101.102.103:31415"
			c.AdminTokenFile = tokenFile
		}},
		{"tailnet with inline token", func(c *daemonConfig) {
			c.Addr = "100.101.102.103:31415"
			c.AdminToken = "secret-admin-token"
		}},
	}
	for _, tc := range cases {
		config := base
		tc.mutate(&config)
		if err := validateDaemonConfig(config); err != nil {
			t.Fatalf("%s: unexpected error %v", tc.name, err)
		}
	}
}

func TestValidateDaemonConfigRejectsBothTokenForms(t *testing.T) {
	config := daemonConfig{
		Label:            defaultDaemonLabel,
		Addr:             "100.101.102.103:31415",
		AdminToken:       "secret-admin-token",
		AdminTokenFile:   writeAdminTokenFile(t, 0o600),
		InstallPath:      "/Users/alice/bin/subrouter",
		LogDir:           "/Users/alice/Library/Logs",
		WorkingDirectory: "/Users/alice/fun/subrouter",
		SRSwitchInterval: "10m",
	}
	if err := validateDaemonConfig(config); err == nil {
		t.Fatal("expected an error when both token forms are passed")
	}
}

// The security property that motivates the env-var plumbing: launchd
// ProgramArguments are visible in `ps` to every local user, so an admin token
// must never be an argv element.
func TestLaunchAgentPlistKeepsAdminTokenOutOfProgramArguments(t *testing.T) {
	home := "/Users/alice"
	config := daemonConfig{
		Label:            defaultDaemonLabel,
		Addr:             "100.101.102.103:31415",
		AdminToken:       "super-secret-admin-token",
		InstallPath:      "/Users/alice/bin/subrouter",
		LogDir:           "/Users/alice/Library/Logs",
		WorkingDirectory: "/Users/alice/fun/subrouter",
		SRSwitchInterval: "10m",
		Path:             defaultDaemonPath("/Users/alice/bin/subrouter"),
	}
	plist, err := launchAgentPlist(config, home)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plist, "<key>SUBROUTER_ADMIN_TOKEN</key>") {
		t.Fatal("expected the token to be exported as an environment variable")
	}
	args := plist[strings.Index(plist, "<key>ProgramArguments</key>"):]
	args = args[:strings.Index(args, "</array>")]
	if strings.Contains(args, config.AdminToken) {
		t.Fatal("admin token leaked into ProgramArguments, where ps exposes it to every local user")
	}
	if mode := launchAgentMode(config); mode != 0o600 {
		t.Fatalf("a plist carrying an inline token must be 0600, got %#o", mode)
	}
}

func TestLaunchAgentPlistPrefersTokenFileAndLeavesPlistReadable(t *testing.T) {
	home := "/Users/alice"
	tokenFile := writeAdminTokenFile(t, 0o600)
	config := daemonConfig{
		Label:            defaultDaemonLabel,
		Addr:             "mac.tail1234.ts.net:31415",
		AdminTokenFile:   tokenFile,
		InstallPath:      "/Users/alice/bin/subrouter",
		LogDir:           "/Users/alice/Library/Logs",
		WorkingDirectory: "/Users/alice/fun/subrouter",
		SRSwitchInterval: "10m",
		Path:             defaultDaemonPath("/Users/alice/bin/subrouter"),
	}
	plist, err := launchAgentPlist(config, home)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plist, "<key>SUBROUTER_ADMIN_TOKEN_FILE</key>") ||
		!strings.Contains(plist, "<string>"+tokenFile+"</string>") {
		t.Fatal("expected SUBROUTER_ADMIN_TOKEN_FILE in the plist environment")
	}
	if strings.Contains(plist, "<key>SUBROUTER_ADMIN_TOKEN</key>") {
		t.Fatal("token-file mode must not also emit an inline SUBROUTER_ADMIN_TOKEN")
	}
	// No secret in the plist, so it does not need tightening.
	if mode := launchAgentMode(config); mode != 0o644 {
		t.Fatalf("token-file mode should leave the plist at 0644, got %#o", mode)
	}
}

// An admin token file the daemon cannot read has to fail here, not at boot.
// The plist sets KeepAlive, so serve would exit, launchd would restart it, and
// the operator's only signal would be churn in subrouter.err.log.
func TestValidateDaemonConfigRejectsUnusableAdminTokenFile(t *testing.T) {
	dir := t.TempDir()

	empty := filepath.Join(dir, "empty-token")
	if err := os.WriteFile(empty, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		path string
	}{
		{"missing", filepath.Join(dir, "does-not-exist")},
		{"directory", dir},
		{"empty once trimmed", empty},
	}
	for _, tc := range cases {
		config := daemonConfig{
			Label:            defaultDaemonLabel,
			Addr:             "100.101.102.103:31415",
			AdminTokenFile:   tc.path,
			InstallPath:      "/Users/alice/bin/subrouter",
			LogDir:           "/Users/alice/Library/Logs",
			WorkingDirectory: "/Users/alice/fun/subrouter",
			SRSwitchInterval: "10m",
		}
		if err := validateDaemonConfig(config); err == nil {
			t.Fatalf("%s: expected install to refuse an unusable admin token file", tc.name)
		}
	}
}

// launchd resolves relative paths against WorkingDirectory, not the shell the
// installer ran in, so the plist has to carry an absolute path or it silently
// reads a different file — or none.
func TestResolveAdminTokenFileReturnsAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "admin-token"), []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	resolved, err := resolveAdminTokenFile("admin-token")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(resolved) {
		t.Fatalf("resolved token path %q is not absolute", resolved)
	}
	if filepath.Base(resolved) != "admin-token" {
		t.Fatalf("resolved token path %q lost the file name", resolved)
	}
}

// --admin-token-file exists to keep the secret out of the plist. A token file
// every other local user can read gives that back, so it must not pass quietly.
func TestAdminTokenFileWarningFlagsGroupAndWorldReadableModes(t *testing.T) {
	if adminTokenFileWarning("/tmp/admin-token", 0o600) != "" {
		t.Fatal("0600 should not warn")
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o666} {
		if adminTokenFileWarning("/tmp/admin-token", mode) == "" {
			t.Fatalf("mode %04o should warn", mode)
		}
	}
}

// laptopPlist is the LaunchAgent as the laptop actually has it: install-daemon's
// template, then the quickstart script's plistlib rewrite (keys sorted) adding
// the peer, and the rest added by hand. Every SUBROUTER_* key here has no
// install-daemon flag, which is why it was added by hand.
const laptopPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>/Users/previous</string>
		<key>PATH</key>
		<string>/previous/bin</string>
		<key>SUBROUTER_ALLOW_LOOPBACK_PROXY</key>
		<string>1</string>
		<key>SUBROUTER_CLAUDE_SOCKET</key>
		<string>/Users/alice/.subrouter/claude.sock</string>
		<key>SUBROUTER_IDENTITY_ACCOUNT</key>
		<string>main</string>
		<key>SUBROUTER_REMOTE_POOL</key>
		<string>https://203.0.113.7:31416</string>
		<key>SUBROUTER_REMOTE_POOL_FINGERPRINT</key>
		<string>sha256:0f1e2d</string>
		<key>SUBROUTER_REMOTE_POOL_TOKEN</key>
		<string>pool&amp;token&lt;secret&gt;</string>
		<key>SUBROUTER_RESERVED_ACCOUNTS</key>
		<string/>
	</dict>
	<key>KeepAlive</key>
	<true/>
	<key>Label</key>
	<string>ai.manaflow.subrouter</string>
	<key>ProgramArguments</key>
	<array>
		<string>/Users/alice/bin/subrouter</string>
		<string>serve</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
</dict>
</plist>
`

func writeExistingLaunchAgent(t *testing.T, home, body string, mode os.FileMode) string {
	t.Helper()
	path := launchAgentPath(home, defaultDaemonLabel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func launchAgentEnvMap(t *testing.T, body []byte) map[string]string {
	t.Helper()
	entries, err := launchAgentEnvironment(body)
	if err != nil {
		t.Fatalf("read back the written plist: %v\n%s", err, body)
	}
	env := map[string]string{}
	for _, entry := range entries {
		env[entry.Key] = entry.Value
	}
	return env
}

// install-daemon re-renders the plist from its template, and macOS `sr setup`
// runs it. It used to delete every EnvironmentVariables key the template does
// not write, which on the laptop is the remote pool, the loopback-proxy
// exemption, the Claude socket and the account reservations: the daemon came
// back healthy and quietly stopped peering. PATCHES section 37.
func TestInstallDaemonKeepsHandAddedPlistEnvironment(t *testing.T) {
	home := t.TempDir()
	config := testDaemonConfig(home)
	path := writeExistingLaunchAgent(t, home, laptopPlist, 0o600)

	if err := installDaemonWithConfig(config, home, commandRunner{}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	env := launchAgentEnvMap(t, body)
	for key, want := range map[string]string{
		"SUBROUTER_ALLOW_LOOPBACK_PROXY":    "<string>1</string>",
		"SUBROUTER_CLAUDE_SOCKET":           "<string>/Users/alice/.subrouter/claude.sock</string>",
		"SUBROUTER_IDENTITY_ACCOUNT":        "<string>main</string>",
		"SUBROUTER_REMOTE_POOL":             "<string>https://203.0.113.7:31416</string>",
		"SUBROUTER_REMOTE_POOL_FINGERPRINT": "<string>sha256:0f1e2d</string>",
		// Escaped exactly as the file had it, not decoded and re-encoded.
		"SUBROUTER_REMOTE_POOL_TOKEN": "<string>pool&amp;token&lt;secret&gt;</string>",
		"SUBROUTER_RESERVED_ACCOUNTS": "<string/>",
		// HOME and PATH are the template's, not the old plist's.
		"HOME": "<string>" + home + "</string>",
		"PATH": "<string>" + config.Path + "</string>",
	} {
		if got := env[key]; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if strings.Count(string(body), "<key>HOME</key>") != 1 || strings.Count(string(body), "<key>PATH</key>") != 1 {
		t.Errorf("HOME or PATH written twice:\n%s", body)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("plist mode = %#o, want 0600: it now carries the remote pool token", got)
	}

	// A second parser, where there is one: plutil must accept the file and
	// decode the carried token to the original value.
	if plutil, lookErr := exec.LookPath("plutil"); lookErr == nil {
		if out, err := exec.Command(plutil, "-lint", path).CombinedOutput(); err != nil {
			t.Fatalf("plutil -lint: %v\n%s\n%s", err, out, body)
		}
		out, err := exec.Command(plutil, "-extract", "EnvironmentVariables.SUBROUTER_REMOTE_POOL_TOKEN", "raw", "-o", "-", path).Output()
		if err != nil {
			t.Fatalf("plutil -extract: %v", err)
		}
		if got := strings.TrimSpace(string(out)); got != "pool&token<secret>" {
			t.Errorf("plutil decodes the carried token as %q, want %q", got, "pool&token<secret>")
		}
	}

	// A rerun is a fixed point: nothing is carried twice.
	if err := installDaemonWithConfig(config, home, commandRunner{}); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(body) {
		t.Errorf("second install changed the plist:\nfirst:\n%s\nsecond:\n%s", body, again)
	}
}

// A plist this cannot parse would lose its environment to an overwrite, so
// the install stops before it changes anything at all.
func TestInstallDaemonRefusesAPlistItCannotRead(t *testing.T) {
	home := t.TempDir()
	config := testDaemonConfig(home)
	binary := "bplist00\xd1\x01\x02_\x10\x14EnvironmentVariables"
	path := writeExistingLaunchAgent(t, home, binary, 0o644)

	err := installDaemonWithConfig(config, home, commandRunner{})
	if err == nil || !strings.Contains(err.Error(), "plutil -convert xml1") {
		t.Fatalf("err = %v, want a refusal naming plutil -convert xml1", err)
	}
	if body, readErr := os.ReadFile(path); readErr != nil || string(body) != binary {
		t.Fatalf("unreadable plist changed: %q, %v", body, readErr)
	}
	if _, statErr := os.Stat(config.InstallPath); !os.IsNotExist(statErr) {
		t.Fatalf("binary installed before the refusal (stat err %v)", statErr)
	}
}

// serve refuses SUBROUTER_ADMIN_TOKEN and SUBROUTER_ADMIN_TOKEN_FILE together,
// so the pair is the flags' whenever one is given; given neither, the plist's
// own is kept, as install-systemd keeps an existing admin token.
func TestLaunchAgentKeptEnvironmentLeavesTheAdminTokenPairToTheFlags(t *testing.T) {
	existing := []launchAgentEnvEntry{
		{Key: "HOME", Value: "<string>/Users/previous</string>"},
		{Key: "SUBROUTER_ADMIN_TOKEN_FILE", Value: "<string>/etc/old-token</string>"},
		{Key: "SUBROUTER_ADMIN_TOKEN", Value: "<string>old-inline</string>"},
		{Key: "SUBROUTER_REMOTE_POOL", Value: "<string>https://203.0.113.7:31416</string>"},
	}
	for name, test := range map[string]struct {
		config daemonConfig
		want   []string
	}{
		"no admin flag":      {daemonConfig{}, []string{"SUBROUTER_ADMIN_TOKEN_FILE", "SUBROUTER_ADMIN_TOKEN", "SUBROUTER_REMOTE_POOL"}},
		"--admin-token":      {daemonConfig{AdminToken: "new"}, []string{"SUBROUTER_REMOTE_POOL"}},
		"--admin-token-file": {daemonConfig{AdminTokenFile: "/etc/new-token"}, []string{"SUBROUTER_REMOTE_POOL"}},
	} {
		t.Run(name, func(t *testing.T) {
			kept, _ := launchAgentKeptEnvironment(existing, test.config, false)
			got := strings.Join(launchAgentEnvKeys(kept), ",")
			if want := strings.Join(test.want, ","); got != want {
				t.Fatalf("kept %s, want %s", got, want)
			}
		})
	}
}

// The plist's mode on disk is the computed one even when the plist already
// existed (os.WriteFile never chmods an existing file, so an inline token used
// to stay 0644), and it is never wider for other users than it was.
func TestInstallDaemonPlistModeTightensAndNeverWidens(t *testing.T) {
	for name, test := range map[string]struct {
		existing os.FileMode
		body     string
		token    string
		want     os.FileMode
	}{
		"inline admin token over a readable plist": {0o644, laptopPlistWithout(t, "SUBROUTER_REMOTE_POOL_TOKEN"), "new-admin-token", 0o600},
		"kept pool token over a readable plist":    {0o644, laptopPlist, "", 0o600},
		"nothing secret, readable plist":           {0o644, laptopPlistWithout(t, "SUBROUTER_REMOTE_POOL_TOKEN"), "", 0o644},
		"nothing secret, private plist":            {0o600, laptopPlistWithout(t, "SUBROUTER_REMOTE_POOL_TOKEN"), "", 0o600},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			config := testDaemonConfig(home)
			config.AdminToken = test.token
			path := writeExistingLaunchAgent(t, home, test.body, test.existing)
			if err := installDaemonWithConfig(config, home, commandRunner{}); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != test.want {
				t.Fatalf("plist mode = %#o, want %#o", got, test.want)
			}
		})
	}
}

func laptopPlistWithout(t *testing.T, key string) string {
	t.Helper()
	line := "\t\t<key>" + key + "</key>\n"
	start := strings.Index(laptopPlist, line)
	if start < 0 {
		t.Fatalf("fixture has no %s", key)
	}
	end := start + len(line) + strings.Index(laptopPlist[start+len(line):], "\n") + 1
	return laptopPlist[:start] + laptopPlist[end:]
}

// A plist symlinked from elsewhere (a dotfiles checkout) is replaced at its
// target, as the in-place write it replaced did, and stays a symlink.
func TestInstallDaemonWritesThroughASymlinkedPlist(t *testing.T) {
	home := t.TempDir()
	config := testDaemonConfig(home)
	target := filepath.Join(t.TempDir(), "subrouter.plist")
	if err := os.WriteFile(target, []byte(laptopPlist), 0o600); err != nil {
		t.Fatal(err)
	}
	path := launchAgentPath(home, defaultDaemonLabel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := installDaemonWithConfig(config, home, commandRunner{}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("plist symlink replaced by a regular file (%v)", err)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if env := launchAgentEnvMap(t, body); env["SUBROUTER_REMOTE_POOL"] == "" || env["HOME"] != "<string>"+home+"</string>" {
		t.Fatalf("symlink target not rewritten with the kept environment:\n%s", body)
	}
}

// A dry run prints the template and names what it would keep, and never
// prints a kept value: that is where a hand-added pool token lives.
func TestInstallDaemonDryRunNamesKeptKeysWithoutTheirValues(t *testing.T) {
	home := t.TempDir()
	config := testDaemonConfig(home)
	config.DryRun = true
	path := writeExistingLaunchAgent(t, home, laptopPlist, 0o600)

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	savedStderr := os.Stderr
	os.Stderr = write
	stdout, runErr := captureStdout(t, func() error { return installDaemonWithConfig(config, home, commandRunner{}) })
	os.Stderr = savedStderr
	_ = write.Close()
	stderrBody, _ := io.ReadAll(read)
	stderr := string(stderrBody)
	if runErr != nil {
		t.Fatal(runErr)
	}
	for _, output := range []string{stdout, stderr} {
		if strings.Contains(output, "pool&amp;token") || strings.Contains(output, "pool&token") {
			t.Fatalf("dry run printed the kept pool token:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
		}
	}
	if !strings.Contains(stderr, "SUBROUTER_REMOTE_POOL_TOKEN") || !strings.Contains(stderr, "SUBROUTER_CLAUDE_SOCKET") {
		t.Fatalf("dry run did not name the kept keys: %q", stderr)
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != laptopPlist {
		t.Fatalf("dry run changed the plist: %v", err)
	}
}

// laptopPlistWith is the laptop's plist with one more EnvironmentVariables
// entry, whose value element is given as the file would spell it.
func laptopPlistWith(t *testing.T, key, value string) string {
	t.Helper()
	anchor := "\t</dict>\n\t<key>KeepAlive</key>"
	if !strings.Contains(laptopPlist, anchor) {
		t.Fatal("fixture has no EnvironmentVariables end")
	}
	return strings.Replace(laptopPlist, anchor, "\t\t<key>"+key+"</key>\n\t\t"+value+"\n"+anchor, 1)
}

// `sr setup` mints the local proxy secret into cloud.json and then runs this
// install. A plist that still carries a SUBROUTER_PROXY_TOKEN from before (a
// daemon once bound to its tailnet address with a hand-set token) holds a
// different value, and serve refuses the two when they differ: carrying it
// over is a restart loop where the old delete-everything render came up on
// the cloud config's secret. So the pair is the cloud config's whenever it
// holds one, and stays the plist's when it does not. PATCHES section 37.
func TestInstallDaemonLeavesTheProxyTokenToACloudConfigThatHoldsOne(t *testing.T) {
	for name, test := range map[string]struct {
		mint     bool
		wantKept bool
	}{
		"cloud config holds a proxy secret": {mint: true, wantKept: false},
		"no cloud config":                   {mint: false, wantKept: true},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			cloudPath := filepath.Join(home, ".config", "subrouter", "cloud.json")
			t.Setenv("SUBROUTER_CLOUD_CONFIG", cloudPath)
			if test.mint {
				// What `sr setup --storage local` does just before it installs.
				if err := broker.SaveConfig(cloudPath, broker.Config{CredentialSource: broker.CredentialSourceLocal}); err != nil {
					t.Fatal(err)
				}
				if _, err := broker.EnsureLocalProxyToken(cloudPath); err != nil {
					t.Fatal(err)
				}
			}
			for _, key := range []string{"SUBROUTER_PROXY_TOKEN", "SUBROUTER_PROXY_TOKEN_FILE"} {
				tokenFile := filepath.Join(home, "proxy-token")
				writeTestFile(t, tokenFile, "old-proxy-secret\n")
				value := "<string>old-proxy-secret</string>"
				if key == "SUBROUTER_PROXY_TOKEN_FILE" {
					value = "<string>" + tokenFile + "</string>"
				}
				path := writeExistingLaunchAgent(t, home, laptopPlistWith(t, key, value), 0o600)
				out, err := captureStdout(t, func() error {
					return installDaemonWithConfig(testDaemonConfig(home), home, commandRunner{})
				})
				if err != nil {
					t.Fatal(err)
				}
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				env := launchAgentEnvMap(t, body)
				if _, kept := env[key]; kept != test.wantKept {
					t.Fatalf("%s kept = %v, want %v:\n%s", key, kept, test.wantKept, body)
				}
				if env["SUBROUTER_REMOTE_POOL_TOKEN"] == "" {
					t.Fatalf("%s: the rest of the environment went with it:\n%s", key, body)
				}
				if dropped := strings.Contains(out, "Dropped "+key+": "+cloudPath); dropped == test.wantKept {
					t.Fatalf("%s: install output does not say what happened to it:\n%s", key, out)
				}
			}
		})
	}
}

// A kept key can outlive the flag or working directory it depended on,
// because ProgramArguments and WorkingDirectory are still re-rendered. serve
// then refuses to start, which under KeepAlive is a restart loop that `sr
// setup` reports as a health timeout. The install refuses first, naming the
// key, and changes nothing. PATCHES section 37.
func TestInstallDaemonRefusesKeptEnvironmentServeWouldRefuse(t *testing.T) {
	for name, test := range map[string]struct {
		key, value  string
		transcripts bool
		refuse      string
	}{
		"missing token file":              {key: "SUBROUTER_ADMIN_TOKEN_FILE", value: "<string>{{dir}}/gone</string>", refuse: "no such file"},
		"empty token file":                {key: "SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE", value: "<string>{{dir}}/empty</string>", refuse: "secret is empty"},
		"relative to the old directory":   {key: "SUBROUTER_ADMIN_TOKEN_FILE", value: "<string>elsewhere/token</string>", refuse: "no such file"},
		"relative to the new directory":   {key: "SUBROUTER_ADMIN_TOKEN_FILE", value: "<string>token</string>"},
		"readable token file":             {key: "SUBROUTER_ADMIN_TOKEN_FILE", value: "<string>{{dir}}/token</string>"},
		"azure transcripts, no dir":       {key: "SUBROUTER_TRANSCRIPT_AZURE_URL", value: "<string>https://a.blob.core.windows.net/c</string>", refuse: "--transcripts"},
		"azure transcripts, dir":          {key: "SUBROUTER_TRANSCRIPT_AZURE_URL", value: "<string>https://a.blob.core.windows.net/c</string>", transcripts: true},
		"tailnet auth, never allowlisted": {key: "SUBROUTER_TAILSCALE_AUTH", value: "<string>1</string>", refuse: "--tailscale-auth-users"},
		"tailnet auth off":                {key: "SUBROUTER_TAILSCALE_AUTH", value: "<string>0</string>"},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			config := testDaemonConfig(home)
			writeTestFile(t, filepath.Join(home, "token"), "admin-secret\n")
			writeTestFile(t, filepath.Join(home, "empty"), " \n")
			if test.transcripts {
				config.TranscriptsDir = filepath.Join(home, "transcripts")
			}
			plist := laptopPlistWith(t, test.key, strings.ReplaceAll(test.value, "{{dir}}", home))
			path := writeExistingLaunchAgent(t, home, plist, 0o600)

			err := installDaemonWithConfig(config, home, commandRunner{})
			if test.refuse == "" {
				if err != nil {
					t.Fatalf("refused a key serve accepts: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.key) || !strings.Contains(err.Error(), test.refuse) {
				t.Fatalf("err = %v, want a refusal naming %s and %q", err, test.key, test.refuse)
			}
			if body, readErr := os.ReadFile(path); readErr != nil || string(body) != plist {
				t.Fatalf("refused install changed the plist: %v", readErr)
			}
			if _, statErr := os.Stat(config.InstallPath); !os.IsNotExist(statErr) {
				t.Fatalf("binary installed before the refusal (stat err %v)", statErr)
			}
		})
	}
}

// A plist symlink whose target does not exist yet is written through, as the
// in-place write did, not replaced by a regular file.
func TestInstallDaemonWritesThroughADanglingPlistSymlink(t *testing.T) {
	for name, relative := range map[string]bool{"absolute link": false, "relative link": true} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			config := testDaemonConfig(home)
			path := launchAgentPath(home, defaultDaemonLabel)
			targetDir := filepath.Join(home, "dotfiles")
			for _, dir := range []string{filepath.Dir(path), targetDir} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			target := filepath.Join(targetDir, "subrouter.plist")
			link := target
			if relative {
				link = filepath.Join("..", "..", "dotfiles", "subrouter.plist")
			}
			if err := os.Symlink(link, path); err != nil {
				t.Fatal(err)
			}
			if err := installDaemonWithConfig(config, home, commandRunner{}); err != nil {
				t.Fatal(err)
			}
			if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("dangling plist symlink replaced by a regular file (%v)", err)
			}
			body, err := os.ReadFile(target)
			if err != nil {
				t.Fatalf("symlink target not written: %v", err)
			}
			if env := launchAgentEnvMap(t, body); env["HOME"] != "<string>"+home+"</string>" {
				t.Fatalf("symlink target is not the rendered plist:\n%s", body)
			}
		})
	}
}
