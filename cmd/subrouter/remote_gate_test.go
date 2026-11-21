package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/storepath"
)

// The bug class these guard against is not "one chokepoint is wrong" -- it is
// "someone adds a thirteenth remote call site, or a new command, and forgets the
// gate." So the assertions enumerate the whole surface off the single
// remoteCommands list rather than testing one site at a time. Adding a gated
// command to that list without gating it fails these.
//
// remoteGateOnce is package state, so these cannot run in parallel.

// realHomeAtStartup is captured before any test can t.Setenv over it, so the
// guard in unlockRemoteGateForTest can tell a sandboxed HOME from the real one.
var realHomeAtStartup = os.Getenv("HOME")

// isolatedHome is the empty HOME TestMain gives the process
// (testenv.IsolateHome). Like the real one, it is a HOME no test chose.
var isolatedHome string

// isolateGate points HOME and the state dir at a scratch tree and clears the
// resolved verdict. Returns the state dir so a case can plant files in it.
func isolateGate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	state := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUBROUTER_STATE_DIR", state)
	t.Setenv("SUBROUTER_CLOUD_CONFIG", filepath.Join(home, "cloud.json"))
	t.Setenv("SUBROUTER_CODEX_SERVER", "")
	resetRemoteGateCache()
	t.Cleanup(resetRemoteGateCache)
	return state
}

func writeRemoteKey(t *testing.T) {
	t.Helper()
	unlockRemoteGateForTest(t)
}

// unlockRemoteGateForTest gives this test's scratch machine the remote key.
//
// Every test that calls it exercises a box or hosted-vault path, which on a
// machine without the key does not exist. Stating that per test is the point:
// a remote feature verified on a keyless machine is verifying the wrong
// machine, and a global default would hide a build whose gate had wrongly
// locked. Call it AFTER t.Setenv("HOME", ...) so the key lands in the test's
// own tree.
func unlockRemoteGateForTest(t *testing.T) {
	t.Helper()
	// PATCHES 8's rule, applied to the one file this gate adds: a test must
	// never write into the developer's real store. Dropping remote.key there
	// would not merely litter -- it would silently unlock that machine, which is
	// the exact outcome the gate exists to prevent. Tests that already sandbox
	// HOME or SUBROUTER_STATE_DIR keep theirs; the rest get one here.
	if home := os.Getenv("HOME"); strings.TrimSpace(os.Getenv("SUBROUTER_STATE_DIR")) == "" &&
		(home == "" || home == realHomeAtStartup || home == isolatedHome) {
		t.Setenv("SUBROUTER_STATE_DIR", t.TempDir())
	}
	// Materialize the store the way a real command would (CodexDir runs the
	// legacy import), so the key lands where the read-only gate looks. Writing
	// to remoteKeyPath() instead would, under a HOME holding a legacy
	// ~/.codex-accounts, name that legacy directory.
	path := filepath.Join(storepath.CodexDir(), remoteKeyFileName)
	if realHomeAtStartup != "" &&
		strings.HasPrefix(path, filepath.Clean(realHomeAtStartup)+string(os.PathSeparator)) {
		t.Fatalf("refusing to write a remote key into the real store at %s", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("test-remote-key-0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resetRemoteGateCache()
	t.Cleanup(resetRemoteGateCache)
}

// The gate is consulted by read-only commands (`sr codex isolation-check`
// reaches it through the dispatch guard), so resolving it must not run
// CodexDir's legacy import: an empty state dir stays uncreated, and the legacy
// ~/.codex-accounts stays the only copy. The verdict must still be the one
// CodexDir would serve after that import, before and after it happens.
func TestRemoteGateLookupDoesNotMigrateTheLegacyStore(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(t.TempDir(), "state")
	legacy := filepath.Join(home, ".codex-accounts")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "login.log"), []byte("legacy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("SUBROUTER_STATE_DIR", state)
	t.Setenv("SUBROUTER_CLOUD_CONFIG", filepath.Join(home, "cloud.json"))
	resetRemoteGateCache()
	t.Cleanup(resetRemoteGateCache)

	if remoteUnlocked() {
		t.Fatal("a keyless legacy store unlocked the gate")
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("resolving the gate created or migrated the state dir: %v", err)
	}

	// A key the legacy store already holds is the key CodexDir would serve.
	if err := os.WriteFile(filepath.Join(legacy, remoteKeyFileName), []byte("test-remote-key-0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resetRemoteGateCache()
	if !remoteUnlocked() {
		t.Fatal("the legacy store's key was not honoured before migration")
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("resolving the gate created or migrated the state dir: %v", err)
	}

	// Once a mutating command runs the import, the verdict does not change.
	if got := storepath.CodexDir(); got != filepath.Join(state, "codex") {
		t.Fatalf("CodexDir = %q", got)
	}
	resetRemoteGateCache()
	if !remoteUnlocked() {
		t.Fatal("the migrated store's key was not honoured")
	}
}

// plantServer writes a fully configured box into servers.json and points the
// environment at it. A locked machine must ignore all of it.
func plantServer(t *testing.T, store accounts.CodexStore) {
	t.Helper()
	path := defaultSRServerStore(store).Path
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"default":"home","servers":[{"name":"home","url":"http://100.64.10.1:31415","adminToken":"planted-admin-token"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteGateLockedIgnoresConfiguredServer(t *testing.T) {
	isolateGate(t)
	store := accounts.DefaultCodexStore()
	plantServer(t, store)
	t.Setenv("SUBROUTER_CODEX_SERVER", "home")

	runner := srRunner{store: store, in: strings.NewReader(""), out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}

	// C1: the funnel every client-side remote path goes through.
	if _, ok, err := runner.selectedRemoteServer(); err != nil || ok {
		t.Fatalf("locked machine resolved a server: ok=%v err=%v", ok, err)
	}
	// C2: and it shows none either.
	servers, err := runner.configuredRemoteServers()
	if err != nil || len(servers) != 0 {
		t.Fatalf("locked machine listed servers: %d err=%v", len(servers), err)
	}
}

func TestRemoteGateUnlockedHonorsConfiguredServer(t *testing.T) {
	// The calibration case. Every assertion above would also pass against a
	// gate that refuses unconditionally, which would prove nothing about the
	// gate and hide a build that had simply broken the remote.
	isolateGate(t)
	store := accounts.DefaultCodexStore()
	plantServer(t, store)
	writeRemoteKey(t)

	runner := srRunner{store: store, in: strings.NewReader(""), out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}

	server, ok, err := runner.selectedRemoteServer()
	if err != nil || !ok {
		t.Fatalf("unlocked machine resolved no server: ok=%v err=%v", ok, err)
	}
	if server.Name != "home" {
		t.Fatalf("resolved %q, want home", server.Name)
	}
	servers, err := runner.configuredRemoteServers()
	if err != nil || len(servers) != 1 {
		t.Fatalf("unlocked machine listed %d servers, err=%v", len(servers), err)
	}
}

func TestRemoteGateLockedRefusesEveryRemoteCommand(t *testing.T) {
	isolateGate(t)
	store := accounts.DefaultCodexStore()
	plantServer(t, store)

	for _, command := range remoteCommands {
		var out, errOut bytes.Buffer
		runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &errOut}

		// Bounded, because "refused" and "reached the network and hung" are not
		// the same result and must not look the same here. With the guard
		// removed, `login` opens a real TLS connection to cmux.com and this
		// blocks -- so an unbounded call would report a 60s package timeout
		// instead of naming the command that escaped.
		done := make(chan error, 1)
		go func() { done <- runner.run(context.Background(), []string{command}) }()
		var err error
		select {
		case err = <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("%s: locked machine did not refuse promptly; it reached a remote", command)
			continue
		}
		if err == nil {
			t.Errorf("%s: locked machine ran the command", command)
			continue
		}
		// Byte-identical to a typo: the failure must not hint that something
		// gated is behind it.
		want := "unknown account command \"" + command + "\""
		if err.Error() != want {
			t.Errorf("%s: error = %q, want %q", command, err.Error(), want)
		}
		if out.Len() != 0 {
			t.Errorf("%s: locked machine wrote output: %q", command, out.String())
		}
	}
}

func TestRemoteGateLockedHelpNamesNoRemoteCommand(t *testing.T) {
	isolateGate(t)

	// Both top-level help surfaces. sr_setup_test.go already records that these
	// two drifted apart once; the gate must not become the third thing that
	// only half of them knows about.
	// Through the production accessors, NOT stripRemoteHelp directly. An
	// earlier draft called the filter itself, so deleting the gate inside
	// helpText changed nothing here and the mutation survived -- the test was
	// checking the filter worked, never that anything used it.
	runner := srRunner{store: accounts.DefaultCodexStore(), in: strings.NewReader(""), out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	surfaces := map[string]string{
		"srHelp":    runner.helpText(),
		"usageText": visibleUsageText("subrouter"),
	}
	for name, text := range surfaces {
		for _, line := range strings.Split(text, "\n") {
			fields := strings.Fields(strings.TrimSpace(line))
			if len(fields) < 2 {
				continue
			}
			if fields[0] != "sr" && fields[0] != "subrouter" {
				continue
			}
			if isRemoteCommand(fields[1]) {
				t.Errorf("%s still offers a gated command: %q", name, line)
			}
		}
		// A heading whose body was entirely gated must go too.
		for _, orphan := range []string{"Credential storage:", "Team vault management:"} {
			if strings.Contains(text, orphan) {
				t.Errorf("%s kept the empty section %q", name, orphan)
			}
		}
		// The local surface has to survive the filter, or the gate has just
		// broken help rather than trimmed it.
		for _, keep := range []string{"setup", "doctor", "cleanup", "status", "pick"} {
			if !strings.Contains(text, keep) {
				t.Errorf("%s dropped local command %q", name, keep)
			}
		}
	}
}

func TestRemoteGateUnlockedHelpKeepsRemoteCommands(t *testing.T) {
	isolateGate(t)
	writeRemoteKey(t)

	runner := srRunner{store: accounts.DefaultCodexStore(), in: strings.NewReader(""), out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	if !strings.Contains(runner.helpText(), "sr server") {
		t.Error("unlocked srHelp lost the server commands")
	}
	if !strings.Contains(visibleUsageText("subrouter"), "subrouter remote") {
		t.Error("unlocked usageText lost the remote commands")
	}
}

func TestRemoteGateLockedDisarmsPeerPoolFromEnv(t *testing.T) {
	isolateGate(t)
	// The daemon half. Hiding the CLI would be theatre if an env var still
	// forwarded traffic to another machine.
	t.Setenv("SUBROUTER_REMOTE_POOL", "http://100.64.10.1:31416")
	t.Setenv("SUBROUTER_REMOTE_POOL_TOKEN", "planted-pool-token")
	t.Setenv("SUBROUTER_REMOTE_POOL_ALLOW", "*")
	t.Setenv("SUBROUTER_REMOTE_POOL_FINGERPRINT", "sha256:planted")

	if pool := remotePoolFromEnv(); pool != nil {
		t.Fatalf("locked daemon armed a peer pool: %+v", pool.URL)
	}

	writeRemoteKey(t)
	if pool := remotePoolFromEnv(); pool == nil {
		t.Fatal("unlocked daemon did not arm the peer pool")
	}
}

func TestRemoteGateLockedForcesLocalCredentialSource(t *testing.T) {
	isolateGate(t)
	// EffectiveCredentialSource returns Legacy for an empty source that is not
	// Ready(), and Legacy is the branch that routes commands at a remote. A
	// stale cloud.json must not reach paths whose commands are hidden.
	for _, source := range []broker.CredentialSource{
		broker.CredentialSourceHosted,
		broker.CredentialSourceTeam,
		broker.CredentialSourceLegacy,
	} {
		path := os.Getenv("SUBROUTER_CLOUD_CONFIG")
		body := `{"version":1,"credentialSource":"` + string(source) + `","localProxyToken":"0123456789abcdef0123456789abcdef"}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		config, err := cloudModeConfig()
		if err != nil {
			t.Fatalf("%s: %v", source, err)
		}
		if got := config.EffectiveCredentialSource(); got != broker.CredentialSourceLocal {
			t.Errorf("locked machine resolved source %q, want local", got)
		}
	}
}

func TestRemoteGateLockedRefusesNonLocalSetupStorage(t *testing.T) {
	isolateGate(t)
	// `setup` stays visible when locked, so --storage is the one flag that could
	// still name a destination off this machine.
	for _, storage := range []string{"hosted", "team", "legacy"} {
		if _, err := resolveSetupCredentialSource(broker.Config{}, storage); err == nil {
			t.Errorf("locked machine accepted --storage %s", storage)
		}
	}
	if source, err := resolveSetupCredentialSource(broker.Config{}, "local"); err != nil || source != broker.CredentialSourceLocal {
		t.Errorf("locked machine refused --storage local: %v %v", source, err)
	}
}

func TestRemoteKeyMustBePrivateAndNonEmpty(t *testing.T) {
	// Possession of the file is the gate, so the shapes that are trivially
	// forged or leaked must not open it.
	cases := []struct {
		name string
		body string
		mode os.FileMode
		want bool
	}{
		{"valid", "0123456789abcdef0123456789abcdef\n", 0o600, true},
		{"touched empty", "", 0o600, false},
		{"too short", "abc\n", 0o600, false},
		{"world readable", "0123456789abcdef0123456789abcdef\n", 0o644, false},
		{"group readable", "0123456789abcdef0123456789abcdef\n", 0o640, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			isolateGate(t)
			path := remoteKeyPath()
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(testCase.body), testCase.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, testCase.mode); err != nil {
				t.Fatal(err)
			}
			if got := resolveRemoteGate(&bytes.Buffer{}).unlocked; got != testCase.want {
				t.Fatalf("resolveRemoteGate = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestRemoteGateSaysNothingWhenNoKeyExists(t *testing.T) {
	isolateGate(t)
	var warn bytes.Buffer
	if resolveRemoteGate(&warn).unlocked {
		t.Fatal("gate opened with no key")
	}
	// Naming the file to someone who has never had one is exactly the
	// discoverability this gate removes.
	if warn.Len() != 0 {
		t.Fatalf("locked machine advertised the key: %q", warn.String())
	}
}

// A refused key is said on stderr, and the published verdict keeps why, so a
// command can say on its own page why it read nothing beyond this machine
// without working the gate out a second time.
func TestRemoteGateKeepsWhyAKeyWasRefused(t *testing.T) {
	const validKey = "0123456789abcdef0123456789abcdef\n"
	writeKeyFile := func(body string, mode os.FileMode) func(*testing.T, string) {
		return func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(body), mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
		}
	}
	cases := []struct {
		name  string
		plant func(*testing.T, string)
		kind  remoteKeyRefusalKind
		perm  os.FileMode
		// warning has %[1]s for the key's path and %[2]v for the read error.
		warning string
	}{
		{
			name: "a directory",
			plant: func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			kind:    keyNotRegularFile,
			warning: "subrouter: %[1]s is not a regular file; remote stays off\n",
		},
		{
			name:    "mode 0644",
			plant:   writeKeyFile(validKey, 0o644),
			kind:    keyModeTooOpen,
			perm:    0o644,
			warning: "subrouter: %[1]s is mode 0644; chmod 600 it to enable remote\n",
		},
		{
			name: "unreadable",
			plant: func(t *testing.T, path string) {
				// The harness runs as root, where no mode makes a regular file
				// unreadable. /proc/self/mem is a 0600 regular file whose first
				// page is unmapped, so reading it from the start fails.
				if _, err := os.Stat("/proc/self/mem"); err != nil {
					t.Skip("no /proc/self/mem to stand in for an unreadable key")
				}
				if err := os.Symlink("/proc/self/mem", path); err != nil {
					t.Fatal(err)
				}
			},
			kind:    keyUnreadable,
			warning: "subrouter: cannot read %[1]s (%[2]v); remote stays off\n",
		},
		{
			name:    "3 bytes",
			plant:   writeKeyFile("abc\n", 0o600),
			kind:    keyTooShort,
			warning: "subrouter: %[1]s is too short to be a key; remote stays off\n",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			isolateGate(t)
			path := remoteKeyPath()
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			testCase.plant(t, path)

			var warn bytes.Buffer
			verdict := resolveRemoteGate(&warn)
			if verdict.unlocked {
				t.Fatal("a refused key opened the gate")
			}
			refusal := verdict.refusal
			if refusal == nil {
				t.Fatalf("the verdict does not say why the key was refused; stderr said %q", warn.String())
			}
			if refusal.kind != testCase.kind || refusal.path != path || refusal.perm != testCase.perm {
				t.Fatalf("refusal = {kind %d, path %q, perm %04o}, want {kind %d, path %q, perm %04o}",
					refusal.kind, refusal.path, refusal.perm, testCase.kind, path, testCase.perm)
			}
			if (refusal.err != nil) != (testCase.kind == keyUnreadable) {
				t.Fatalf("refusal.err = %v for kind %d", refusal.err, refusal.kind)
			}
			want := fmt.Sprintf(testCase.warning, path, refusal.err)
			if warn.String() != want {
				t.Fatalf("stderr = %q, want %q", warn.String(), want)
			}
			if got := refusal.warning() + "\n"; got != want {
				t.Fatalf("warning() = %q, want the line the gate printed, %q", got, want)
			}

			var published remoteGateVerdict
			printed := captureProcessStderr(t, func() {
				published = remoteGate()
				remoteGate()
			})
			if published.unlocked || published.refusal == nil || published.refusal.kind != testCase.kind {
				t.Fatalf("published verdict = %+v, want locked with kind %d", published, testCase.kind)
			}
			if printed != want {
				t.Fatalf("the published gate printed %q to stderr, want %q once", printed, want)
			}
		})
	}

	t.Run("no key", func(t *testing.T) {
		isolateGate(t)
		var warn bytes.Buffer
		if verdict := resolveRemoteGate(&warn); verdict.unlocked || verdict.refusal != nil || warn.Len() != 0 {
			t.Fatalf("verdict = %+v, stderr = %q; want locked, no refusal, nothing printed", verdict, warn.String())
		}
	})
	t.Run("a valid key", func(t *testing.T) {
		isolateGate(t)
		path := remoteKeyPath()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		writeKeyFile(validKey, 0o600)(t, path)
		var warn bytes.Buffer
		if verdict := resolveRemoteGate(&warn); !verdict.unlocked || verdict.refusal != nil || warn.Len() != 0 {
			t.Fatalf("verdict = %+v, stderr = %q; want unlocked, no refusal, nothing printed", verdict, warn.String())
		}
	})
}

// captureProcessStderr returns what fn wrote to os.Stderr, where the
// published gate writes its warning.
func captureProcessStderr(t *testing.T, fn func()) string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = write
	fn()
	os.Stderr = saved
	_ = write.Close()
	body, err := io.ReadAll(read)
	_ = read.Close()
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
