package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// Ported from upstream manaflow-ai/subrouter #370 (6bcbcbd), adapted to this
// fork: the extra-args explicit/empty distinction (install-systemd's
// ExtraArgsExplicit) rides the same flag.Visit, and `sr server install` sits
// behind the remote gate (PATCHES 17), so the server-install tests unlock it --
// without that the SSH refusal test would pass on the gate's own error. They
// also pass --allow-upstream-release, since the command refuses upstream's
// release binary without it (PATCHES 37), for the same reason.

// useTempSystemdEtc points the systemd installer and controller at a scratch
// /etc so tests never read or write the host's configuration.
func useTempSystemdEtc(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	previous := systemdEtcDir
	systemdEtcDir = dir
	t.Cleanup(func() { systemdEtcDir = previous })
	for _, sub := range []string{"default", filepath.Join("systemd", "system")} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// saw reports whether exactly this command line was run.
func (r *recordingRunner) saw(call string) bool {
	return r.indexOf(call) >= 0
}

// indexOf returns the position of the first exact match, or -1.
func (r *recordingRunner) indexOf(call string) int {
	for index, got := range r.calls {
		if got == call {
			return index
		}
	}
	return -1
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A reinstall or credential rotation that does not pass --addr must keep the
// existing listen address, switch interval and session store instead of
// silently rebinding the flag defaults (0.0.0.0:31415, 10m).
func TestInstallSystemdPreservesExistingAddrIntervalAndSessions(t *testing.T) {
	useTempSystemdEtc(t)
	writeTestFile(t, systemdDefaultPath(systemdConfig{ServiceName: defaultSystemdServiceName}), strings.Join([]string{
		"SUBROUTER_ADDR=127.0.0.1:4000",
		"SUBROUTER_STATE_DIR=/var/lib/subrouter",
		"SUBROUTER_SESSIONS=/srv/subrouter/sessions.json",
		"SUBROUTER_SR_SWITCH_INTERVAL=0",
		`SUBROUTER_EXTRA_ARGS="--public-addr 0.0.0.0:31416"`,
		"",
	}, "\n"))

	config, err := parseInstallSystemdArgs([]string{"--replace-legacy=false"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if config.Addr != "127.0.0.1:4000" {
		t.Errorf("addr = %q, want preserved 127.0.0.1:4000: a rotation would rebind every interface", config.Addr)
	}
	if config.SRSwitchInterval != "0" {
		t.Errorf("sr switch interval = %q, want preserved 0: a disabled sweep would come back", config.SRSwitchInterval)
	}
	if config.SessionsPath != "/srv/subrouter/sessions.json" {
		t.Errorf("sessions = %q, want preserved /srv/subrouter/sessions.json", config.SessionsPath)
	}
	// The public listener is ours (owner decision), carried in extra args. The
	// explicit map now also drives ExtraArgsExplicit; an omitted flag must
	// still preserve it...
	if config.ExtraArgs != "--public-addr 0.0.0.0:31416" || config.ExtraArgsExplicit {
		t.Errorf("extra args = %q (explicit=%v), want the public listener preserved", config.ExtraArgs, config.ExtraArgsExplicit)
	}

	explicit, err := parseInstallSystemdArgs([]string{
		"--replace-legacy=false",
		"--addr", "0.0.0.0:9000",
		"--cx-switch-interval", "5m",
		"--sessions", "/tmp/sessions.json",
		"--extra-args", "",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if explicit.Addr != "0.0.0.0:9000" || explicit.SRSwitchInterval != "5m" || explicit.SessionsPath != "/tmp/sessions.json" {
		t.Errorf("explicit flags were overridden by the EnvironmentFile: %+v", explicit)
	}
	// ...and an explicit empty one must still clear it, or a public listener
	// cannot be removed.
	if explicit.ExtraArgs != "" || !explicit.ExtraArgsExplicit {
		t.Errorf("extra args = %q (explicit=%v) after --extra-args \"\", want cleared", explicit.ExtraArgs, explicit.ExtraArgsExplicit)
	}

	// Passing a flag's own default is still an explicit choice.
	defaulted, err := parseInstallSystemdArgs([]string{"--replace-legacy=false", "--addr", "0.0.0.0:31415", "--sr-switch-interval", "10m"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if defaulted.Addr != "0.0.0.0:31415" || defaulted.SRSwitchInterval != "10m" {
		t.Errorf("a flag passed at its default value was overridden by the EnvironmentFile: addr=%q interval=%q", defaulted.Addr, defaulted.SRSwitchInterval)
	}
}

// boxEnvironmentFile is /etc/default/subrouter as install-systemd renders it,
// plus keys only a hand edit puts there: install-systemd has no flag for them.
const boxEnvironmentFile = `SUBROUTER_ADDR=100.64.0.1:31415
SUBROUTER_STATE_DIR=/var/lib/subrouter
SUBROUTER_SESSIONS=/var/lib/subrouter/sessions.json
SUBROUTER_TRANSCRIPTS=
SUBROUTER_TRANSCRIPT_ARGS=""
SUBROUTER_SR_SWITCH_INTERVAL=10m
SUBROUTER_ADMIN_TOKEN="admin-secret"
SUBROUTER_ACCOUNT_IMPORT_TOKEN="import-secret"
SUBROUTER_PROXY_TOKEN=""
SUBROUTER_EXTRA_ARGS="--public-addr 0.0.0.0:31416"

# reservations, set by hand
SUBROUTER_RESERVED_ACCOUNTS="alice bob"
CODEX_HOME=/var/lib/subrouter/.codex-pool
SUBROUTER_IDENTITY_ACCOUNT=main
SUBROUTER_PROXY_TOKEN_FILE=/etc/subrouter/proxy-token
`

func reinstallSystemdDefaults(t *testing.T, args ...string) (string, os.FileMode) {
	t.Helper()
	config, err := parseInstallSystemdArgs(append([]string{"--replace-legacy=false", "--start=false"}, args...), nil)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := readSystemdEnvironmentFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := applySystemdUnits(config, systemdDefaults(config), previous, "[Unit]\n", "[Socket]\n", nil, &recordingRunner{}); err != nil {
		t.Fatal(err)
	}
	path := systemdDefaultPath(config)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body), info.Mode().Perm()
}

// install-systemd rewrote /etc/default/subrouter wholesale and deleted every
// key it has no flag for. The flags it has were already backfilled from the
// file; a hand-set CODEX_HOME, a reservation or a *_TOKEN_FILE was simply gone
// after the next run of the box's bootstrap script. PATCHES section 37.
func TestInstallSystemdKeepsHandAddedEnvironmentFileKeys(t *testing.T) {
	useTempSystemdEtc(t)
	path := systemdDefaultPath(systemdConfig{ServiceName: defaultSystemdServiceName})
	writeTestFile(t, path, boxEnvironmentFile)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}

	body, mode := reinstallSystemdDefaults(t)
	for key, want := range map[string]string{
		"SUBROUTER_RESERVED_ACCOUNTS": "alice bob",
		"CODEX_HOME":                  "/var/lib/subrouter/.codex-pool",
		// An empty proxy token leaves the hand-set file to supply it: dropping
		// the file is a box that refuses to start on its tailnet bind.
		"SUBROUTER_PROXY_TOKEN_FILE": "/etc/subrouter/proxy-token",
		"SUBROUTER_ADDR":             "100.64.0.1:31415",
		"SUBROUTER_ADMIN_TOKEN":      "admin-secret",
	} {
		if got := readDefaultValue(path, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	for _, line := range []string{"# reservations, set by hand", "SUBROUTER_IDENTITY_ACCOUNT=main", `SUBROUTER_RESERVED_ACCOUNTS="alice bob"`} {
		if !strings.Contains(body, line+"\n") {
			t.Errorf("kept line %q missing or respelled:\n%s", line, body)
		}
	}
	if strings.Count(body, "SUBROUTER_ADDR=") != 1 || strings.Count(body, "SUBROUTER_PROXY_TOKEN=") != 1 {
		t.Errorf("a rendered key was written twice:\n%s", body)
	}
	if mode != 0o600 {
		t.Errorf("defaults mode = %#o, want 0600", mode)
	}

	// A rerun is a fixed point.
	again, _ := reinstallSystemdDefaults(t)
	if again != body {
		t.Errorf("second install changed the file:\nfirst:\n%s\nsecond:\n%s", body, again)
	}

	// A token the install writes takes its _FILE twin's place: serve refuses
	// SUBROUTER_PROXY_TOKEN and SUBROUTER_PROXY_TOKEN_FILE together.
	rotated, _ := reinstallSystemdDefaults(t, "--proxy-token", "new-proxy-secret")
	if strings.Contains(rotated, "SUBROUTER_PROXY_TOKEN_FILE") {
		t.Errorf("--proxy-token kept the old SUBROUTER_PROXY_TOKEN_FILE, so serve would refuse both:\n%s", rotated)
	}
	if readDefaultValue(path, "SUBROUTER_PROXY_TOKEN") != "new-proxy-secret" || readDefaultValue(path, "CODEX_HOME") == "" {
		t.Errorf("rotation lost the new token or a kept key:\n%s", rotated)
	}
}

// A kept *_TOKEN value makes the file private, and the file is never made
// readable to more users than it was.
func TestInstallSystemdEnvironmentFileModeCoversKeptKeys(t *testing.T) {
	for name, test := range map[string]struct {
		existing os.FileMode
		body     string
		want     os.FileMode
	}{
		"kept token, readable file": {0o644, "SUBROUTER_REMOTE_POOL_TOKEN=peer-secret\n", 0o600},
		"nothing secret, readable":  {0o644, "CODEX_HOME=/var/lib/subrouter/.codex-pool\n", 0o644},
		"nothing secret, private":   {0o600, "CODEX_HOME=/var/lib/subrouter/.codex-pool\n", 0o600},
	} {
		t.Run(name, func(t *testing.T) {
			useTempSystemdEtc(t)
			path := systemdDefaultPath(systemdConfig{ServiceName: defaultSystemdServiceName})
			writeTestFile(t, path, test.body)
			if err := os.Chmod(path, test.existing); err != nil {
				t.Fatal(err)
			}
			if _, mode := reinstallSystemdDefaults(t); mode != test.want {
				t.Fatalf("defaults mode = %#o, want %#o", mode, test.want)
			}
		})
	}
}

// An EnvironmentFile that is there but cannot be read stops the install before
// anything is installed: not the binary, not the aliases, not the units. It
// used to be read in applySystemdUnits, after the binary had been replaced,
// so a refusal left the new binary under the old units. A directory at the
// path is unreadable even to root.
func TestInstallSystemdRefusesAnEnvironmentFileItCannotReadBeforeInstallingAnything(t *testing.T) {
	useTempSystemdEtc(t)
	previousEUID := systemdInstallerEUID
	systemdInstallerEUID = func() int { return 0 }
	t.Cleanup(func() { systemdInstallerEUID = previousEUID })

	home := t.TempDir()
	config, err := parseInstallSystemdArgs([]string{
		"--replace-legacy=false", "--start=false", "--addr", "127.0.0.1:4000",
		"--home", home, "--sessions", filepath.Join(home, "sessions.json"),
		"--install-path", filepath.Join(home, "bin", "subrouter"),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(systemdDefaultPath(config), 0o755); err != nil {
		t.Fatal(err)
	}

	err = installSystemdWithConfig(config, commandRunner{})
	if err == nil || !strings.Contains(err.Error(), "would drop every key") {
		t.Fatalf("err = %v, want a refusal to rewrite an unreadable EnvironmentFile", err)
	}
	for _, path := range []string{config.InstallPath, systemdUnitPath(config), systemdSocketPath(config)} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("%s written despite the refusal (stat err %v)", path, statErr)
		}
	}
}

// The box's bootstrap script always pipes three non-empty tokens (admin,
// import, proxy), minting any it cannot find inline in the file or its own
// state file. So on the box the rendered tokens always win: every
// *_TOKEN_FILE twin is replaced by the inline token, as before PATCHES
// section 37, while the rest of the hand-added keys are kept. This pins what
// the §37 docs promise the box.
func TestInstallSystemdBootstrapTokensReplaceTokenFileTwinsAndKeepTheRest(t *testing.T) {
	useTempSystemdEtc(t)
	path := systemdDefaultPath(systemdConfig{ServiceName: defaultSystemdServiceName})
	writeTestFile(t, path, boxEnvironmentFile+"SUBROUTER_ADMIN_TOKEN_FILE=/etc/subrouter/admin-token\nSUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE=/etc/subrouter/import-token\n")

	config, err := parseInstallSystemdArgs([]string{
		"--replace-legacy=false", "--start=false", "--addr", "100.64.0.1:31415",
		"--admin-token-stdin", "--account-import-token-stdin", "--proxy-token-stdin",
	}, strings.NewReader("minted-admin\nminted-import\nminted-proxy\n"))
	if err != nil {
		t.Fatal(err)
	}
	previous, err := readSystemdEnvironmentFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := applySystemdUnits(config, systemdDefaults(config), previous, "[Unit]\n", "[Socket]\n", nil, &recordingRunner{}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "_TOKEN_FILE") {
		t.Errorf("a *_TOKEN_FILE survived next to the inline token bootstrap passed; serve refuses both:\n%s", body)
	}
	for key, want := range map[string]string{
		"SUBROUTER_ADMIN_TOKEN":          "minted-admin",
		"SUBROUTER_ACCOUNT_IMPORT_TOKEN": "minted-import",
		"SUBROUTER_PROXY_TOKEN":          "minted-proxy",
		"CODEX_HOME":                     "/var/lib/subrouter/.codex-pool",
		"SUBROUTER_IDENTITY_ACCOUNT":     "main",
	} {
		if got := readDefaultValue(path, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// Rewriting the socket unit with a new ListenStream has no effect on an active
// socket until the socket unit itself is restarted.
func TestApplySystemdUnitsRestartsSocketWhenItChanged(t *testing.T) {
	useTempSystemdEtc(t)
	config := systemdConfig{ServiceName: defaultSystemdServiceName, User: "subrouter", Group: "subrouter", Addr: "127.0.0.1:4000", Start: true}
	oldSocket, err := systemdSocket(systemdConfig{ServiceName: defaultSystemdServiceName, Addr: "0.0.0.0:31415"})
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, systemdSocketPath(config), oldSocket)
	newSocket, err := systemdSocket(config)
	if err != nil {
		t.Fatal(err)
	}

	runner := &recordingRunner{}
	if err := applySystemdUnits(config, "SUBROUTER_ADDR=127.0.0.1:4000\n", "", "[Unit]\n", newSocket, nil, runner); err != nil {
		t.Fatal(err)
	}
	stopService := runner.indexOf("systemctl stop subrouter")
	restartSocket := runner.indexOf("systemctl restart subrouter.socket")
	restartService := runner.indexOf("systemctl restart subrouter")
	if restartSocket < 0 {
		t.Fatalf("changed socket unit was not restarted, so the old address keeps listening: %v", runner.calls)
	}
	if restartService < 0 {
		t.Fatalf("service was not restarted: %v", runner.calls)
	}
	// The service holds the inherited listener; it has to let go before the
	// socket rebinds, and come back only after.
	if stopService < 0 || stopService > restartSocket || restartSocket > restartService {
		t.Fatalf("want stop service, restart socket, restart service in that order: %v", runner.calls)
	}
	written, err := os.ReadFile(systemdSocketPath(config))
	if err != nil || string(written) != newSocket {
		t.Fatalf("socket unit on disk = %q (%v), want the new one", written, err)
	}

	// Unchanged socket: only the service restarts, so an unrelated reinstall
	// does not drop the listening socket and its connection queue.
	unchanged := &recordingRunner{}
	if err := applySystemdUnits(config, "SUBROUTER_ADDR=127.0.0.1:4000\n", "", "[Unit]\n", newSocket, nil, unchanged); err != nil {
		t.Fatal(err)
	}
	if unchanged.sawContaining("restart subrouter.socket") || unchanged.saw("systemctl stop subrouter") {
		t.Fatalf("unchanged socket unit was restarted: %v", unchanged.calls)
	}
	if !unchanged.saw("systemctl restart subrouter") {
		t.Fatalf("service was not restarted: %v", unchanged.calls)
	}
}

// `sr up` against an agent that is already loaded must not kill the healthy
// running daemon with `kickstart -k`.
func TestLaunchdStartLeavesLoadedAgentRunning(t *testing.T) {
	runner := &recordingRunner{fail: map[string]error{
		"launchctl bootstrap": errors.New("Bootstrap failed: 5: Input/output error (already loaded)"),
	}}
	controller := launchdController{label: defaultDaemonLabel, home: t.TempDir(), runner: runner}
	if err := controller.start(); err != nil {
		t.Fatalf("start of a loaded agent failed: %v", err)
	}
	if runner.sawContaining("kickstart -k") {
		t.Fatalf("start killed the running daemon: %v", runner.calls)
	}
	if !runner.sawContaining("launchctl kickstart gui/") {
		t.Fatalf("start did not ensure the loaded agent is running: %v", runner.calls)
	}
}

// A bootstrap failure for an agent that is not loaded is a real error, not
// something to paper over with a kickstart.
func TestLaunchdStartReportsBootstrapFailureWhenNotLoaded(t *testing.T) {
	bootstrapErr := errors.New("Bootstrap failed: 5: Input/output error")
	runner := &recordingRunner{fail: map[string]error{
		"launchctl bootstrap": bootstrapErr,
		"launchctl print":     errors.New("Could not find service"),
	}}
	controller := launchdController{label: defaultDaemonLabel, home: t.TempDir(), runner: runner}
	err := controller.start()
	if err == nil {
		t.Fatalf("start succeeded although the agent could not be loaded: %v", runner.calls)
	}
	if !errors.Is(err, bootstrapErr) {
		t.Fatalf("start error = %v, want the bootstrap error itself", err)
	}
	if runner.sawContaining("kickstart") {
		t.Fatalf("start kickstarted an agent that is not loaded: %v", runner.calls)
	}
}

// markerAtEachCall records, at every launchctl call, the watchdog stop marker
// (ops/PATCHES.md §81) as the watchdog job would find it then.
type markerAtEachCall struct {
	marker string
	seen   []string
	fail   map[string]error
}

func (r *markerAtEachCall) Run(name string, args ...string) error {
	body, err := os.ReadFile(r.marker)
	state := "none"
	if err == nil {
		state = string(body)
	}
	r.seen = append(r.seen, args[0]+": "+state)
	return r.fail[args[0]]
}

// `sr daemon restart` boots the job out and bootstraps it at once. The watchdog
// must not load it in that gap, or the restart's own bootstrap fails, so the
// stop is announced before the bootout. It is withdrawn after the bootstrap,
// and after a failed one too, so a restart that could not load the job leaves
// it to the watchdog rather than holding the watchdog off.
func TestRestartLaunchAgentAnnouncesItsBootoutToTheWatchdog(t *testing.T) {
	for _, bootstrapFails := range []bool{false, true} {
		home := t.TempDir()
		runner := &markerAtEachCall{marker: filepath.Join(home, ".subrouter", "watchdog-stop.test.subrouter.restart")}
		if bootstrapFails {
			runner.fail = map[string]error{"bootstrap": errors.New("Bootstrap failed: 5: Input/output error")}
		}
		before := time.Now().Unix()
		err := restartLaunchAgent(home, filepath.Join(home, "job.plist"), "test.subrouter.restart", runner)
		if (err != nil) != bootstrapFails {
			t.Fatalf("bootstrap fails %v: restart error = %v", bootstrapFails, err)
		}
		if len(runner.seen) < 2 {
			t.Fatalf("bootstrap fails %v: launchctl calls %q, want a bootout and a bootstrap", bootstrapFails, runner.seen)
		}
		for _, call := range runner.seen[:2] {
			verb, body, _ := strings.Cut(call, ": ")
			fields := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
			if len(fields) != 3 || fields[0] != "label=test.subrouter.restart" || !strings.HasPrefix(fields[2], "by=") {
				t.Fatalf("bootstrap fails %v: at the %s the marker was %q, want label=, until= and by= lines", bootstrapFails, verb, body)
			}
			until, err := strconv.ParseInt(strings.TrimPrefix(fields[1], "until="), 10, 64)
			if !strings.HasPrefix(fields[1], "until=") || err != nil || until < before+60 || until > time.Now().Unix()+86400 {
				t.Fatalf("bootstrap fails %v: at the %s the marker held %q, want an end a minute to a day ahead", bootstrapFails, verb, fields[1])
			}
		}
		if _, err := os.Stat(runner.marker); !os.IsNotExist(err) {
			t.Fatalf("bootstrap fails %v: the marker is still there after the restart (%v)", bootstrapFails, err)
		}
	}
}

// fakeLaunchd installs a `launchctl` on PATH that keeps a tiny launchd model in
// a scratch directory: whether the agent is loaded, and which daemon INSTANCE is
// running (a counter bumped every time a process is started). The controller
// under test uses the real commandRunner, so this drives the exact exec path
// `sr up` takes on a Mac. The instance counter is what a user feels: if it
// moves while a session is open, that session's daemon was killed under it.
type fakeLaunchd struct {
	state string
}

const fakeLaunchctlScript = `#!/bin/sh
[ "$1" = "__probe__" ] && exit 0
state="$FAKE_LAUNCHD_STATE"
echo "launchctl $*" >> "$state/calls"
start_instance() {
  n=0
  [ -f "$state/instance" ] && n=$(cat "$state/instance")
  echo $((n + 1)) > "$state/instance"
  : > "$state/running"
}
case "$1" in
  bootstrap)
    if [ -e "$state/loaded" ]; then
      echo "Bootstrap failed: 5: Input/output error" >&2
      exit 5
    fi
    : > "$state/loaded"
    start_instance
    ;;
  print)
    [ -e "$state/loaded" ] || { echo "Could not find service" >&2; exit 113; }
    ;;
  kickstart)
    [ -e "$state/loaded" ] || { echo "Could not find service" >&2; exit 113; }
    if [ "$2" = "-k" ]; then
      start_instance
    elif [ ! -e "$state/running" ]; then
      start_instance
    fi
    ;;
  bootout)
    rm -f "$state/loaded" "$state/running"
    ;;
esac
exit 0
`

func newFakeLaunchd(t *testing.T) fakeLaunchd {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the launchctl fake is a POSIX shell script")
	}
	bin := t.TempDir()
	state := t.TempDir()
	path := filepath.Join(bin, "launchctl")
	if err := os.WriteFile(path, []byte(fakeLaunchctlScript), 0o755); err != nil {
		t.Fatal(err)
	}
	// Exec it once before the code under test does. Exec'ing a file right
	// after writing it races any fork, anywhere in this test binary, that
	// happened while the write fd was open: the child holds the fd until its
	// own exec, and ours fails with ETXTBSY (golang/go#22315). Once one exec
	// succeeds no writer remains and none can appear, so the run under test
	// is deterministic. Only ETXTBSY is retried; anything else is a real error.
	for attempt := 0; ; attempt++ {
		err := exec.Command(path, "__probe__").Run()
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.ETXTBSY) || attempt >= 100 {
			t.Fatalf("fake launchctl is not runnable: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_LAUNCHD_STATE", state)
	return fakeLaunchd{state: state}
}

// loadAndRun puts the model in "agent loaded, daemon instance 1 running".
func (f fakeLaunchd) loadAndRun(t *testing.T, running bool) {
	t.Helper()
	writeTestFile(t, filepath.Join(f.state, "loaded"), "")
	writeTestFile(t, filepath.Join(f.state, "instance"), "1\n")
	if running {
		writeTestFile(t, filepath.Join(f.state, "running"), "")
	}
}

func (f fakeLaunchd) instance(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.state, "instance"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (f fakeLaunchd) running() bool {
	_, err := os.Stat(filepath.Join(f.state, "running"))
	return err == nil
}

func (f fakeLaunchd) calls(t *testing.T) string {
	t.Helper()
	raw, _ := os.ReadFile(filepath.Join(f.state, "calls"))
	return string(raw)
}

// The live laptop bug, end to end through `sr up` (runServerLifecycleWith) and
// the real commandRunner. The launcher's autostart reaches the same start()
// whenever a 1.5 s health probe misses -- a busy or draining daemon, not only a
// dead one -- so the old `kickstart -k` fallback killed a healthy daemon and
// every live `sr claude` / `sr codex` session on it.
func TestSrUpLeavesARunningLaunchdDaemonAndItsSessionsAlone(t *testing.T) {
	home := t.TempDir()
	controller := launchdController{label: defaultDaemonLabel, home: home, runner: commandRunner{}}
	if err := os.MkdirAll(filepath.Dir(controller.plist()), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, controller.plist(), "<plist/>\n")

	cases := []struct {
		name         string
		loaded       bool
		running      bool
		wantInstance int
	}{
		// The case that bit: loaded and serving. It must be the same process after.
		{name: "loaded and running", loaded: true, running: true, wantInstance: 1},
		// Guards the fix from over-correcting: a loaded agent whose process is
		// gone still has to be started by `sr up`.
		{name: "loaded but stopped", loaded: true, running: false, wantInstance: 2},
		{name: "not loaded", loaded: false, running: false, wantInstance: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeLaunchd(t)
			if tc.loaded {
				fake.loadAndRun(t, tc.running)
			}
			var out bytes.Buffer
			err := runServerLifecycleWith(controller, "up", fake.running, &out)
			if err != nil {
				t.Fatalf("sr up: %v\nlaunchctl calls:\n%s", err, fake.calls(t))
			}
			if !fake.running() {
				t.Fatalf("sr up left no daemon running\nlaunchctl calls:\n%s", fake.calls(t))
			}
			if got := fake.instance(t); got != tc.wantInstance {
				t.Fatalf("daemon instance = %d, want %d: sr up replaced the daemon process, killing every live session on it\nlaunchctl calls:\n%s",
					got, tc.wantInstance, fake.calls(t))
			}
		})
	}
}

// For the socket-activated system install, stopping only the service leaves
// subrouter.socket listening (and it restarts the service on the next
// connection); removing only the service leaves the socket unit behind, which
// then makes refuseSystemSystemdConflict block a later `sr setup`.
func TestSystemdControllerSystemStopAndRemoveIncludeSocket(t *testing.T) {
	useTempSystemdEtc(t)
	config := systemdConfig{ServiceName: defaultSystemdServiceName}
	writeTestFile(t, systemdUnitPath(config), "[Unit]\n")
	writeTestFile(t, systemdSocketPath(config), "[Socket]\n")

	runner := &recordingRunner{}
	controller := systemdController{service: defaultSystemdServiceName, home: t.TempDir(), runner: runner}
	if err := controller.stop(); err != nil {
		t.Fatal(err)
	}
	if !runner.saw("systemctl stop subrouter.socket subrouter") {
		t.Fatalf("stop left the socket listening (or stopped it after the service): %v", runner.calls)
	}

	runner.calls = nil
	if err := controller.remove(); err != nil {
		t.Fatal(err)
	}
	if !runner.saw("systemctl disable --now subrouter.socket subrouter") {
		t.Fatalf("remove left the socket enabled: %v", runner.calls)
	}
	if _, err := os.Stat(systemdSocketPath(config)); !os.IsNotExist(err) {
		t.Fatalf("socket unit file still present after remove: %v", err)
	}
	if _, err := os.Stat(systemdUnitPath(config)); !os.IsNotExist(err) {
		t.Fatalf("service unit file still present after remove: %v", err)
	}
	if err := refuseSystemSystemdConflict(systemdUnitPath(config), systemdSocketPath(config)); err != nil {
		t.Fatalf("setup would still be blocked after remove: %v", err)
	}
}

// The per-user systemd unit has no socket, so its commands must not mention one.
func TestSystemdControllerUserStopDoesNotTouchSocket(t *testing.T) {
	useTempSystemdEtc(t)
	home := t.TempDir()
	unit := userSystemdUnitPath(home, defaultSystemdServiceName)
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, unit, "[Unit]\n")
	runner := &recordingRunner{}
	controller := systemdController{service: defaultSystemdServiceName, home: home, runner: runner}
	if err := controller.stop(); err != nil {
		t.Fatal(err)
	}
	if !runner.saw("systemctl --user stop subrouter") {
		t.Fatalf("user stop did not stop the user unit: %v", runner.calls)
	}
	if err := controller.remove(); err != nil {
		t.Fatal(err)
	}
	if runner.sawContaining(".socket") {
		t.Fatalf("user unit commands mention a socket: %v", runner.calls)
	}
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Fatalf("user unit still present after remove: %v", err)
	}
}

// The scheduled task runs %LOCALAPPDATA%\Subrouter\bin\subrouter.exe, so the
// installer has to put the executable there.
func TestInstallWindowsTaskInstallsExecutable(t *testing.T) {
	paths := newWindowsPaths(t.TempDir())
	runner := &recordingRunner{}
	if err := installWindowsTask(paths, windowsTaskConfig{Addr: "127.0.0.1:31415"}, runner); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(paths.Exe)
	if err != nil {
		t.Fatalf("task executable was not installed, so the task can never start: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("task executable is empty")
	}
	// A running daemon holds the file open on Windows; it has to be ended
	// before the replace, and the task registered after.
	end := runner.indexOf("schtasks /End /TN " + defaultWindowsTaskName)
	create := -1
	for index, call := range runner.calls {
		if strings.HasPrefix(call, "schtasks /Create") {
			create = index
		}
	}
	if end < 0 || create < 0 || end > create {
		t.Fatalf("want schtasks /End before /Create: %v", runner.calls)
	}
}

func saveGCPServer(t *testing.T) accounts.CodexStore {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Remote paths exist only on a keyed machine (PATCHES 17).
	unlockRemoteGateForTest(t)
	store := accounts.DefaultCodexStore()
	if err := defaultSRServerStore(store).save(srServerFile{
		Servers: []srServerConfig{{
			Name:        "prod",
			URL:         "http://100.64.0.1:31415",
			GCPInstance: "subrouter-prod",
			GCPZone:     "us-central1-a",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

// Only flags the operator passed are forwarded to install-systemd, so a
// credential rotation keeps the host's configured address and interval.
func TestServerInstallGCPForwardsOnlyExplicitFlags(t *testing.T) {
	store := saveGCPServer(t)
	var out bytes.Buffer
	fake := &stdinCapturingRunner{}
	runner := srRunner{program: "sr", store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake}
	if err := runner.run(context.Background(), []string{"server", "install", "prod", "--allow-upstream-release"}); err != nil {
		t.Fatal(err)
	}
	if len(fake.commands) != 1 || fake.commands[0][0] != "gcloud" {
		t.Fatalf("commands = %v, want one gcloud invocation", fake.commands)
	}
	script := fake.commands[0][len(fake.commands[0])-1]
	for _, flag := range []string{"--addr", "switch-interval", "--extra-args"} {
		if strings.Contains(script, flag) {
			t.Fatalf("unrequested %s forwarded; it would override the host's config:\n%s", flag, script)
		}
	}
	if !strings.Contains(script, "sr install-systemd --admin-token-stdin --account-import-token-stdin") {
		t.Fatalf("install-systemd invocation missing or reshaped:\n%s", script)
	}

	fake = &stdinCapturingRunner{}
	runner.cmd = fake
	if err := runner.run(context.Background(), []string{
		"server", "install", "prod", "--allow-upstream-release",
		"--addr", "10.0.0.5:31415", "--sr-switch-interval", "0", "--extra-args", "--fetch-usage=false",
	}); err != nil {
		t.Fatal(err)
	}
	script = fake.commands[0][len(fake.commands[0])-1]
	for _, want := range []string{"--addr '10.0.0.5:31415'", "--sr-switch-interval '0'", "--extra-args '--fetch-usage=false'"} {
		if !strings.Contains(script, want) {
			t.Fatalf("explicit flag %q not forwarded:\n%s", want, script)
		}
	}
}

// The SSH install path cannot honor --addr and friends on every platform it
// supports, so it must refuse them rather than silently drop them.
func TestServerInstallOverSSHRejectsUnforwardedFlags(t *testing.T) {
	newRunner := func(t *testing.T) (srRunner, *stdinCapturingRunner) {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		// Unlocked so a refusal below is the flag check's, not the remote
		// gate's: on a locked machine every case would "pass" on the gate.
		unlockRemoteGateForTest(t)
		store := accounts.DefaultCodexStore()
		if err := defaultSRServerStore(store).save(srServerFile{
			Servers: []srServerConfig{{Name: "mac-mini", URL: "http://100.64.0.9:31415", SSHHost: "worker@mac-mini"}},
		}); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		fake := &stdinCapturingRunner{}
		return srRunner{program: "sr", store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake}, fake
	}

	// Control: without the flags the same setup reaches ssh.
	runner, fake := newRunner(t)
	if err := runner.run(context.Background(), []string{"server", "install", "mac-mini", "--allow-upstream-release"}); err != nil {
		t.Fatalf("plain SSH install failed, so the refusals below prove nothing: %v", err)
	}
	if len(fake.commands) != 1 || fake.commands[0][0] != "ssh" {
		t.Fatalf("commands = %v, want one ssh invocation", fake.commands)
	}

	for _, args := range [][]string{
		{"--addr", "127.0.0.1:4000"},
		{"--sr-switch-interval", "0"},
		{"--cx-switch-interval", "0"},
		{"--extra-args", "--fetch-usage=false"},
	} {
		runner, fake := newRunner(t)
		err := runner.run(context.Background(), append([]string{"server", "install", "mac-mini", "--allow-upstream-release"}, args...))
		if err == nil {
			t.Fatalf("%v was silently ignored on the SSH path", args)
		}
		if !strings.Contains(err.Error(), "not supported when installing mac-mini over SSH") {
			t.Fatalf("%v: refused for the wrong reason: %v", args, err)
		}
		if len(fake.commands) != 0 {
			t.Fatalf("%v: ran %v despite rejecting the flag", args, fake.commands)
		}
	}
}

// Both install paths pipe upstream's public install.sh, which replaces this
// fork's binary with upstream's release: none of this fork's patches, none of
// the guards that refuse an unauthenticated non-loopback bind. So the command
// refuses unless the operator names that outcome, and a refusal runs nothing
// and mints no tokens. PATCHES section 37.
func TestServerInstallRefusesUpstreamReleaseWithoutOverride(t *testing.T) {
	for name, server := range map[string]srServerConfig{
		"gcp": {Name: "prod", URL: "http://100.64.0.1:31415", GCPInstance: "subrouter-prod", GCPZone: "us-central1-a"},
		"ssh": {Name: "prod", URL: "http://100.64.0.1:31415", SSHHost: "worker@box"},
	} {
		t.Run(name, func(t *testing.T) {
			newRunner := func(t *testing.T) (srRunner, *stdinCapturingRunner, srServerStore) {
				t.Helper()
				t.Setenv("HOME", t.TempDir())
				// Unlocked so the refusal below is this one, not the remote gate's.
				unlockRemoteGateForTest(t)
				store := accounts.DefaultCodexStore()
				servers := defaultSRServerStore(store)
				if err := servers.save(srServerFile{Servers: []srServerConfig{server}}); err != nil {
					t.Fatal(err)
				}
				var out bytes.Buffer
				fake := &stdinCapturingRunner{}
				return srRunner{program: "sr", store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake}, fake, servers
			}

			for _, args := range [][]string{{}, {"--version", "1.2.3"}} {
				runner, fake, servers := newRunner(t)
				err := runner.run(context.Background(), append([]string{"server", "install", "prod"}, args...))
				if err == nil || !strings.Contains(err.Error(), "--allow-upstream-release") || !strings.Contains(err.Error(), "from a source build") {
					t.Fatalf("%v: err = %v, want a refusal naming --allow-upstream-release and this fork's deploy", args, err)
				}
				if len(fake.commands) != 0 {
					t.Fatalf("%v: ran %v despite refusing", args, fake.commands)
				}
				stored, _, findErr := servers.find("prod")
				if findErr != nil || stored.AdminToken != "" || stored.AccountImportToken != "" {
					t.Fatalf("%v: refusal minted control tokens (err %v)", args, findErr)
				}
			}

			// Control: the override reaches the upstream install script.
			runner, fake, _ := newRunner(t)
			if err := runner.run(context.Background(), []string{"server", "install", "prod", "--allow-upstream-release"}); err != nil {
				t.Fatalf("install with --allow-upstream-release failed, so the refusal above proves nothing: %v", err)
			}
			if len(fake.commands) != 1 || !strings.Contains(strings.Join(fake.commands[0], " "), publicInstallScriptURL) {
				t.Fatalf("commands = %v, want one invocation piping %s", fake.commands, publicInstallScriptURL)
			}
		})
	}
}
