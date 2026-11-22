package main

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// PATCHES §47. Every test in this package runs in a usable login session, and
// none runs launchctl: this was written from a shell whose session had ended,
// and with the real probe there every test of a gated verb refused, and the
// launchd tests asked the real launchd. A test that wants the ended session
// says so (endedLoginSession), and its daemon read must be its own fake.
func init() {
	probeLoginSession = usableLoginSession
	launchctlManagerName = aquaLaunchd
}

func usableLoginSession() loginSessionState { return loginSessionState{usable: true} }

func aquaLaunchd() (string, int, error) { return "Aqua", 0, nil }

// endedSupersetSession is what the probe returned in the shell this was
// written from.
func endedSupersetSession() loginSessionState {
	return loginSessionState{
		ended:    true,
		reason:   "user: unknown userid 502; `launchctl managername` exits 153: Could not get manager name.",
		host:     "Superset's terminal daemon (running since Sep 30 02:31)",
		hostKind: hostSuperset,
	}
}

// endedLoginSession ends this test's login session.
func endedLoginSession(t *testing.T, state loginSessionState) {
	t.Helper()
	previous := probeLoginSession
	probeLoginSession = func() loginSessionState { return state }
	t.Cleanup(func() { probeLoginSession = previous })
}

// The probe, on every input it has. It decides on the uid lookup alone and
// asks launchctl only to explain a failed one; everything but darwin is usable.
func TestLoginSessionProbe(t *testing.T) {
	manifestStart := time.Date(2026, 9, 30, 2, 31, 32, 725_000_000, time.Local)
	wantSince := manifestStart.Local().Format("Jan 2 15:04")
	unknown := func() error { return user.UnknownUserIdError(502) }
	dead := func() (string, int, error) { return "Could not get manager name.", 153, nil }
	aqua := func() (string, int, error) { return "Aqua", 0, nil }
	noLaunchctl := func() (string, int, error) {
		return "", -1, errors.New("exec: \"/bin/launchctl\": file does not exist")
	}

	type want struct {
		usable, ended bool
		kind          terminalHostKind
		host          string
		reason        []string
		situation     string
	}
	cases := []struct {
		name      string
		goos      string
		lookup    func() error
		manager   func() (string, int, error)
		env       map[string]string
		manifest  string
		want      want
		noManager bool // launchctl must not run
	}{
		{name: "linux never looks", goos: "linux", lookup: unknown, manager: dead, noManager: true,
			want: want{usable: true}},
		{name: "darwin, the lookup answers", goos: "darwin", lookup: func() error { return nil }, manager: dead, noManager: true,
			want: want{usable: true}},
		{name: "darwin, the lookup fails some other way", goos: "darwin", lookup: func() error { return errors.New("getpwuid_r: EIO") },
			manager: dead, noManager: true, want: want{usable: true}},
		{name: "ended: launchctl refuses too", goos: "darwin", lookup: unknown, manager: dead,
			want: want{ended: true, reason: []string{"unknown userid 502", "exits 153", "Could not get manager name."},
				situation: "this shell belongs to a macOS login session that has ended"}},
		{name: "not ended: launchd answers", goos: "darwin", lookup: unknown, manager: aqua,
			want: want{reason: []string{"unknown userid 502", `answers "Aqua"`}, situation: "this process cannot reach macOS login services: its user lookup failed"}},
		{name: "not ended, under Superset", goos: "darwin", lookup: unknown, manager: aqua,
			env:  map[string]string{"SUPERSET_TERMINAL_ID": "b980", "SUPERSET_ORGANIZATION_ID": "org"},
			want: want{kind: hostSuperset, host: "Superset's terminal daemon", situation: "its user lookup failed"}},
		{name: "launchctl did not run", goos: "darwin", lookup: unknown, manager: noLaunchctl,
			want: want{reason: []string{"unknown userid 502", "did not run"}, situation: "this process cannot reach macOS login services: its user lookup failed"}},
		{name: "Superset, with its manifest", goos: "darwin", lookup: unknown, manager: dead,
			env:      map[string]string{"SUPERSET_TERMINAL_ID": "b980", "SUPERSET_ORGANIZATION_ID": "org"},
			manifest: `{"pid":54677,"socketPath":"/tmp/x.sock","startedAt":` + strconv.FormatInt(manifestStart.UnixMilli(), 10) + `}`,
			want: want{ended: true, kind: hostSuperset, host: "Superset's terminal daemon (running since " + wantSince + ")",
				situation: "this shell belongs to a macOS login session that has ended"}},
		{name: "Superset, no manifest", goos: "darwin", lookup: unknown, manager: dead,
			env:  map[string]string{"SUPERSET_TERMINAL_ID": "b980", "SUPERSET_ORGANIZATION_ID": "org"},
			want: want{ended: true, kind: hostSuperset, host: "Superset's terminal daemon", situation: "has ended"}},
		{name: "Superset, a manifest that is not one", goos: "darwin", lookup: unknown, manager: dead,
			env: map[string]string{"SUPERSET_TERMINAL_ID": "b980", "SUPERSET_ORGANIZATION_ID": "org"}, manifest: `{"pid":"54677","startedAt":"soon"}`,
			want: want{ended: true, kind: hostSuperset, host: "Superset's terminal daemon", situation: "has ended"}},
		{name: "Superset, a manifest with no pid", goos: "darwin", lookup: unknown, manager: dead,
			env: map[string]string{"SUPERSET_TERMINAL_ID": "b980", "SUPERSET_ORGANIZATION_ID": "org"}, manifest: `{"pid":0,"startedAt":1790753492725}`,
			want: want{ended: true, kind: hostSuperset, host: "Superset's terminal daemon", situation: "has ended"}},
		{name: "tmux", goos: "darwin", lookup: unknown, manager: dead, env: map[string]string{"TMUX": "/tmp/tmux-502/default,1,0"},
			want: want{ended: true, kind: hostTmux, host: "a tmux server", situation: "has ended"}},
		// The innermost host is the one that outlived the login: a tmux
		// server started from a Superset tab keeps its panes in the ended
		// session whatever happens to Superset.
		{name: "tmux inside Superset", goos: "darwin", lookup: unknown, manager: dead,
			env:      map[string]string{"TMUX": "/tmp/tmux-502/default,1,0", "SUPERSET_TERMINAL_ID": "b980", "SUPERSET_ORGANIZATION_ID": "org"},
			manifest: `{"pid":54677,"startedAt":` + strconv.FormatInt(manifestStart.UnixMilli(), 10) + `}`,
			want:     want{ended: true, kind: hostTmux, host: "a tmux server", situation: "has ended"}},
		{name: "screen inside Superset", goos: "darwin", lookup: unknown, manager: dead,
			env:  map[string]string{"STY": "123.pts", "SUPERSET_TERMINAL_ID": "b980"},
			want: want{ended: true, kind: hostScreen, host: "a screen session", situation: "has ended"}},
		{name: "screen", goos: "darwin", lookup: unknown, manager: dead, env: map[string]string{"STY": "123.pts"},
			want: want{ended: true, kind: hostScreen, host: "a screen session", situation: "has ended"}},
		{name: "a terminal nobody names", goos: "darwin", lookup: unknown, manager: dead,
			want: want{ended: true, situation: "has ended"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			env := map[string]string{"SUPERSET_HOME_DIR": home}
			for key, value := range tc.env {
				env[key] = value
			}
			if tc.manifest != "" {
				dir := filepath.Join(home, "host", "org")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "pty-daemon-manifest.json"), []byte(tc.manifest), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			lookups, managerRuns := 0, 0
			got := loginSessionProbe{
				goos:      tc.goos,
				lookupUID: func() (*user.User, error) { lookups++; return nil, tc.lookup() },
				managerName: func() (string, int, error) {
					managerRuns++
					return tc.manager()
				},
				getenv:   func(key string) string { return env[key] },
				readFile: os.ReadFile,
			}.check()
			if tc.goos != "darwin" && lookups != 0 {
				t.Errorf("%s looked the user up (%d times); only darwin is checked", tc.goos, lookups)
			}
			if tc.noManager && managerRuns != 0 {
				t.Errorf("launchctl ran %d times for a session the lookup already called usable", managerRuns)
			}
			if got.usable != tc.want.usable || got.ended != tc.want.ended || got.hostKind != tc.want.kind {
				t.Fatalf("usable/ended/hostKind = %v/%v/%v, want %v/%v/%v (%+v)",
					got.usable, got.ended, got.hostKind, tc.want.usable, tc.want.ended, tc.want.kind, got)
			}
			if got.usable {
				if got != (loginSessionState{usable: true}) {
					t.Fatalf("a usable session carries more than usable: %+v", got)
				}
				return
			}
			if got.host != tc.want.host {
				t.Fatalf("host = %q, want %q", got.host, tc.want.host)
			}
			for _, fragment := range tc.want.reason {
				if !strings.Contains(got.reason, fragment) {
					t.Errorf("reason %q lacks %q", got.reason, fragment)
				}
			}
			if !strings.Contains(got.situation(), tc.want.situation) {
				t.Errorf("situation %q lacks %q", got.situation(), tc.want.situation)
			}
			// An agent reading this could turn a pid into `kill <pid>`, which
			// ends every Superset terminal, live ones included.
			if strings.Contains(got.host, "pid") || strings.Contains(got.host, "54677") {
				t.Errorf("host names the daemon's pid: %q", got.host)
			}
			checkRemedy(t, got)
		})
	}
}

// The probe production runs asks this process: this OS, and user.LookupId of
// this uid. Every other test drives the probe with stubs, so without this one
// a wiring that never looked (goos fixed to "linux", a lookup that answers
// nil) would pass the whole suite while the gate never fired. The lookup is
// compared by result -- user and error type -- with a direct call, which tells
// the real lookup from a stub on a machine where the lookup works (a user) and
// where it fails (UnknownUserIdError) alike.
func TestTheProductionProbeAsksThisProcess(t *testing.T) {
	probe := defaultLoginSessionProbe()
	if probe.goos != runtime.GOOS {
		t.Fatalf("the production probe checks GOOS %q on %s", probe.goos, runtime.GOOS)
	}
	wantUser, wantErr := user.LookupId(strconv.Itoa(os.Getuid()))
	gotUser, gotErr := probe.lookupUID()
	if fmt.Sprintf("%T", gotErr) != fmt.Sprintf("%T", wantErr) || fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
		t.Fatalf("the production lookup returned %T %v; user.LookupId(%d) returns %T %v", gotErr, gotErr, os.Getuid(), wantErr, wantErr)
	}
	if (gotUser == nil) != (wantUser == nil) || (gotUser != nil && (gotUser.Uid != wantUser.Uid || gotUser.Username != wantUser.Username)) {
		t.Fatalf("the production lookup returned user %+v; user.LookupId(%d) returns %+v", gotUser, os.Getuid(), wantUser)
	}
	// launchctl through the package's seam (so tests never run it), the real
	// environment and the real file system.
	asked := 0
	previous := launchctlManagerName
	launchctlManagerName = func() (string, int, error) { asked++; return "Seam", 0, nil }
	t.Cleanup(func() { launchctlManagerName = previous })
	if name, _, _ := probe.managerName(); name != "Seam" || asked != 1 {
		t.Fatalf("the production probe's launchctl is not launchctlManagerName (answered %q, seam asked %d times)", name, asked)
	}
	t.Setenv("SUBROUTER_TEST_PROBE_ENV", "from the environment")
	if got := probe.getenv("SUBROUTER_TEST_PROBE_ENV"); got != "from the environment" {
		t.Fatalf("the production probe's getenv answered %q", got)
	}
	path := filepath.Join(t.TempDir(), "manifest.json")
	writeTestFile(t, path, "on disk")
	if body, err := probe.readFile(path); err != nil || string(body) != "on disk" {
		t.Fatalf("the production probe's readFile: %q, %v", body, err)
	}
}

// checkRemedy holds every remedy to the case it prints in. A new Superset tab
// is opened by the same old terminal daemon, and a new tmux window by the
// same old server, so "a terminal opened since your last login" sends the
// user back into the ended session; a new window of a terminal app does not.
func checkRemedy(t *testing.T, state loginSessionState) {
	t.Helper()
	remedy := state.remedy()
	if strings.Contains(remedy, "opened since your last login") {
		t.Errorf("remedy offers a terminal opened since the last login, which a new Superset tab or tmux pane is: %q", remedy)
	}
	button := state.ended && state.hostKind == hostSuperset
	if strings.Contains(remedy, "Restart daemon") != button {
		t.Errorf("remedy %q: names Superset's button = %v, want %v", remedy, !button, button)
	}
	if !state.ended {
		for _, unwanted := range []string{"launchd", "Terminal.app", "ended"} {
			if strings.Contains(remedy+state.situation(), unwanted) {
				t.Errorf("launchd answers here, yet the message says %q: %s / %s", unwanted, state.situation(), remedy)
			}
		}
		if !strings.Contains(remedy, "outside any sandbox") {
			t.Errorf("remedy for a lookup that fails while launchd answers: %q", remedy)
		}
		return
	}
	if !strings.Contains(remedy, "open a new Terminal.app (or other non-Superset terminal app) window") {
		t.Errorf("remedy names no new terminal app window: %q", remedy)
	}
	switch state.hostKind {
	case hostSuperset:
		for _, want := range []string{"New Superset tabs share the ended session", "a person does this in Superset > Settings > Terminal > Terminal daemon > Restart daemon",
			"it closes every Superset terminal", "Do not signal the daemon process."} {
			if !strings.Contains(remedy, want) {
				t.Errorf("Superset remedy lacks %q: %s", want, remedy)
			}
		}
	case hostTmux:
		if !strings.Contains(remedy, "start a new tmux server there (`tmux -L <name>`)") {
			t.Errorf("tmux remedy does not start a new server: %s", remedy)
		}
	case hostScreen:
		if !strings.Contains(remedy, "start a new screen session there") {
			t.Errorf("screen remedy does not start a new session: %s", remedy)
		}
	}
}

// From a shell whose login session has ended launchctl refuses (managername
// 153, print 141), and from an ssh session it answers "Background": there a
// bootout of the gui/<uid> agent works and the bootstrap after it is refused,
// which leaves the daemon down with nothing in that shell able to start it
// again. Every launchd path in gui/<uid> asks managername first and runs
// nothing unless it answers Aqua (PATCHES §47).
func TestLaunchdIsNeverAskedOutsideTheGUILogin(t *testing.T) {
	answers := []struct {
		name   string
		answer func() (string, int, error)
		says   string
	}{
		{"ended login session", func() (string, int, error) { return "Could not get manager name.", 153, nil }, "exits 153"},
		{"ssh session", func() (string, int, error) { return "Background", 0, nil }, `"Background"`},
		{"launchctl did not run", func() (string, int, error) { return "", -1, errors.New("no answer within 2s") }, "did not run"},
	}
	actions := []struct {
		name string
		run  func(c launchdController) error
	}{
		{"start", launchdController.start},
		{"stop", launchdController.stop},
		{"restart", launchdController.restart},
		{"remove", launchdController.remove},
		{"restartLaunchAgent", func(c launchdController) error { return restartLaunchAgent(c.home, c.plist(), c.label, c.runner) }},
	}
	// In the GUI login a restart announces its stop to the watchdog, so the
	// job is a scratch one (ops/PATCHES.md §81).
	label := "test.subrouter.login-session"
	for _, answer := range answers {
		for _, action := range actions {
			t.Run(answer.name+"/"+action.name, func(t *testing.T) {
				previous := launchctlManagerName
				launchctlManagerName = answer.answer
				t.Cleanup(func() { launchctlManagerName = previous })
				runner := &recordingRunner{}
				controller := launchdController{label: label, home: t.TempDir(), runner: runner}
				writeLaunchAgentPlist(t, controller)
				err := action.run(controller)
				if err == nil {
					t.Fatalf("%s from a %s went ahead (launchctl calls: %v)", action.name, answer.name, runner.calls)
				}
				if len(runner.calls) != 0 {
					t.Fatalf("%s from a %s ran launchctl anyway: %v", action.name, answer.name, runner.calls)
				}
				if !strings.Contains(err.Error(), answer.says) || !strings.Contains(err.Error(), "launchd was not asked to do anything") {
					t.Fatalf("%s from a %s: %v, want it to say %q and that nothing was run", action.name, answer.name, err, answer.says)
				}
				if answer.name == "ended login session" && (strings.Contains(err.Error(), "opened since your last login") ||
					!strings.Contains(err.Error(), "open a new Terminal.app (or other non-Superset terminal app) window")) {
					t.Fatalf("%s from an ended login session sends the user to a terminal that may share it: %v", action.name, err)
				}
				if _, statErr := os.Stat(controller.plist()); statErr != nil {
					t.Fatalf("%s from a %s removed the plist: %v", action.name, answer.name, statErr)
				}
			})
		}
	}
	// Calibration: in the GUI login every action reaches launchctl, so the
	// zero calls above are the preflight and not a runner that records nothing.
	for _, action := range actions {
		runner := &recordingRunner{}
		controller := launchdController{label: label, home: t.TempDir(), runner: runner}
		writeLaunchAgentPlist(t, controller)
		_ = action.run(controller)
		if !runner.sawContaining("launchctl ") {
			t.Fatalf("%s in the GUI login ran no launchctl (%v); the refusals above prove nothing", action.name, runner.calls)
		}
	}
}

func writeLaunchAgentPlist(t *testing.T, controller launchdController) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(controller.plist()), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, controller.plist(), "<plist/>\n")
}

// install-daemon that will start the agent asks managername before it writes
// the binary or the plist: an install that wrote both and could not load them
// would leave the old daemon serving beside a plist that says otherwise.
func TestInstallDaemonWritesNothingWhenItCouldNotStartTheAgent(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no launchctl, whatever happens below
	previous := launchctlManagerName
	launchctlManagerName = func() (string, int, error) { return "Could not get manager name.", 153, nil }
	t.Cleanup(func() { launchctlManagerName = previous })

	home := t.TempDir()
	config := testDaemonConfig(home)
	config.Start = true
	plist := launchAgentPath(home, config.Label)
	err := installDaemonWithConfig(config, home, commandRunner{})
	if err == nil || !strings.Contains(err.Error(), "launchd was not asked to do anything") {
		t.Fatalf("install-daemon from an ended login session: %v, want the launchd refusal", err)
	}
	for _, path := range []string{config.InstallPath, plist, config.SRAliasPath} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Errorf("install-daemon wrote %s before it knew it could start the agent (stat err %v)", path, statErr)
		}
	}
	// Calibration: the same install that will not start the agent writes
	// them, so the absence above is the preflight.
	config.Start = false
	if err := installDaemonWithConfig(config, home, commandRunner{}); err != nil {
		t.Fatalf("install-daemon without start: %v", err)
	}
	if _, statErr := os.Stat(plist); statErr != nil {
		t.Fatalf("install-daemon without start wrote no plist (%v); the check above proves nothing", statErr)
	}
}

// `install-daemon --dry-run` prints the plist and asks launchd nothing, so the
// preflight is not its business: it runs on Linux, where there is no
// launchctl, and from ssh. --start stays at its default (true) here, as it
// does on the command line.
func TestInstallDaemonDryRunAsksLaunchdNothing(t *testing.T) {
	sandboxHostConfig(t)
	t.Setenv("PATH", t.TempDir())
	for _, answer := range []struct {
		name   string
		answer func() (string, int, error)
	}{
		{"no launchctl (Linux)", func() (string, int, error) { return "", -1, errors.New(`exec: "/bin/launchctl": file does not exist`) }},
		{"ssh session", func() (string, int, error) { return "Background", 0, nil }},
		{"ended login session", func() (string, int, error) { return "Could not get manager name.", 153, nil }},
	} {
		t.Run(answer.name, func(t *testing.T) {
			asked := 0
			previous := launchctlManagerName
			launchctlManagerName = func() (string, int, error) { asked++; return answer.answer() }
			t.Cleanup(func() { launchctlManagerName = previous })
			stdout, err := captureStdout(t, func() error { return installDaemon([]string{"--dry-run"}) })
			if err != nil {
				t.Fatalf("install-daemon --dry-run (%s): %v", answer.name, err)
			}
			if !strings.Contains(stdout, "<plist") {
				t.Fatalf("install-daemon --dry-run printed no plist:\n%s", stdout)
			}
			if asked != 0 {
				t.Fatalf("install-daemon --dry-run asked launchctl managername %d times", asked)
			}
		})
	}
}
