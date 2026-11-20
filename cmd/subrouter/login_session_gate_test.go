package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/storepath"
)

// PATCHES §47: the gate's allow list, verb by verb. A verb missing from both
// lists below is refused (fail closed), so a new top-level verb needs no edit
// here; one added to the allow list without an entry here fails
// TestEveryDirectVerbIsClassifiedForAnEndedSession.
var (
	endedSessionAllowed = [][]string{
		nil, {"help"}, {"-h"}, {"--help"},
		{"status"}, {"status", "x@example.com"}, {"x@example.com"},
		{"list"}, {"ls"}, {"trace", "x@example.com"}, {"breadcrumbs", "x"}, {"why", "x"},
		{"doctor"},
		{"remote"}, {"remote", "-v"}, {"remote", "list"}, {"remote", "current"}, {"remotes", "ls"},
		{"server"}, {"server", "list"}, {"servers", "ls"}, {"server", "current"},
		{"server", "status"}, {"servers", "status"},
		{"daemon"}, {"daemon", "status"}, {"daemon", "logs"},
		{"codex", "isolation-check"}, {"codex", "isolation-check", "--json"},
		{"codex", "reset", "x@example.com"}, {"codex", "reset", "--dry-run", "x@example.com"},
		{"claude", "reset", "work"}, {"claude", "reset", "--dry-run", "work"},
		{"claude", "help"}, {"claude", "-h"}, {"claude", "--help"},
		{"kimi", "help"}, {"qwen", "--help"}, {"agy", "help"}, {"antigravity", "-h"},
		{"tenant"}, {"tenant", "help"}, {"tenants", "--help"},
	}
	endedSessionRefused = [][]string{
		{"switch"}, {"switch", "x@example.com"}, {"use", "x@example.com"}, {"g"}, {"gui", "x"}, {"gui-switch"}, {"gui-use", "x"},
		{"pick"}, {"add"}, {"add", "codex"}, {"codex", "add"}, {"add", "claude", "work"}, {"claude", "add", "work"},
		{"addr", "claude", "work"}, {"import"}, {"add-key", "codex"}, {"add-api-key"}, {"usage"}, {"usage", "7"},
		{"login"}, {"logout"}, {"team", "list"}, {"account", "list"}, {"accounts"}, {"storage"}, {"storage", "local"},
		{"setup"}, {"cleanup", "--yes"},
		{"daemon", "start"}, {"daemon", "up"}, {"daemon", "stop"}, {"daemon", "restart"}, {"daemon", "bind-state", "/x"}, {"daemon", "unbind-state"},
		{"server", "install", "box"}, {"server", "use", "box"}, {"server", "add", "box"}, {"server", "status", "box"}, {"server", "up"},
		{"server", "restart"}, {"server", "login", "box"}, {"server", "sync", "box"}, {"server", "remove", "box"},
		{"remote", "use", "box"}, {"remote", "add", "box", "http://100.64.0.9:31415"}, {"remote", "rm", "box"},
		{"claude"}, {"claude", "--model", "opus"}, {"claude", "proxy"}, {"claude", "work"}, {"claude", "run", "work"},
		{"claude", "move", "work"}, {"claude", "push", "work"}, {"claude", "upload", "work"}, {"claude", "remove", "work"},
		{"claude", "addr", "work"}, {"claude", "login"}, {"claude", "rename", "a", "b"}, {"claude", "list"},
		{"claude", "status"}, {"claude", "use", "work"}, {"claude", "switch", "work"}, {"claude", "env"}, {"claude", "env", "work"},
		{"claude", "pick"},
		{"claude", "proxy-scope"},
		{"codex", "migrate-isolation"}, {"codex", "enroll-isolated"}, {"codex", "attest-legacy"},
		{"agy"}, {"antigravity", "add"}, {"kimi"}, {"kimi", "login", "k"}, {"qwen"}, {"reset"}, {"reset", "x@example.com"},
		{"remove", "x@example.com"}, {"rm", "x@example.com"}, {"spend"}, {"cost"}, {"gemini"}, {"oai"}, {"az"},
		{"claude-aws"}, {"claude-direct"}, {"tenant", "list"}, {"add-admin-key"}, {"list-admin-keys"},
		{"remove-admin-key", "l"}, {"attach-project", "l"}, {"no-such-verb"},
	}
	// runForProgram's own verbs. serve, supervise, front and listener-transfer
	// are daemons; the rest act on launchd, the box or a credential.
	endedSessionMainRefused = [][]string{
		{"serve"}, {"supervise"}, {"front"}, {"listener-transfer"}, {"install-daemon"}, {"install-launchd"},
		{"install-systemd"}, {"naked"}, {"direct"}, {"accounts"}, {"codex"}, {"codex", "login"}, {"codex", "exec", "hi"},
		{"token"}, {"drain"}, {"resume"}, {"drain-status"}, {"cx", "switch", "x@example.com"}, {"cx", "claude", "move", "w"},
	}
	endedSessionMainAllowed = [][]string{
		{"help"}, {"probe"}, {"fingerprint"}, {"cx", "status"}, {"cx", "claude", "reset", "w"}, {"status"},
		{"codex", "reset", "x@example.com"}, {"codex", "isolation-check"},
	}
	// The same verbs under the binary's own name, where runForProgram's switch
	// comes before srRunner.run: there `accounts` is listAccounts (a read of
	// the Codex store), not sr's team-account verb.
	endedSessionSubrouterAllowed = [][]string{
		{"accounts"}, {"help"}, {"status"}, {"list"}, {"claude", "help"}, {"server", "status"}, {"x@example.com"},
		{"codex", "reset", "x@example.com"},
	}
	endedSessionSubrouterRefused = [][]string{
		{"switch", "x@example.com"}, {"codex"}, {"codex", "exec", "hi"}, {"serve"}, {"install-daemon"}, {"claude", "move", "w"},
		{"account", "list"}, {"token"},
	}
)

func TestEndedSessionAllowList(t *testing.T) {
	sandboxHostConfig(t)
	unlockRemoteGate(t)
	for _, args := range endedSessionAllowed {
		if !srVerbAllowedInEndedSession(args) {
			t.Errorf("sr %s is refused; it only prints or hands the work to a daemon", strings.Join(args, " "))
		}
		if !mainVerbAllowedInEndedSession("sr", args) {
			t.Errorf("runForProgram refuses sr %s, which srRunner.run allows", strings.Join(args, " "))
		}
	}
	for _, args := range endedSessionRefused {
		if srVerbAllowedInEndedSession(normalizeAddRefreshArgs(normalizeProviderAddArgs(args))) {
			t.Errorf("sr %s is allowed in an ended login session", strings.Join(args, " "))
		}
	}
	for _, args := range endedSessionMainRefused {
		if mainVerbAllowedInEndedSession("sr", args) {
			t.Errorf("sr %s is allowed in an ended login session", strings.Join(args, " "))
		}
	}
	for _, args := range endedSessionMainAllowed {
		if !mainVerbAllowedInEndedSession("sr", args) {
			t.Errorf("sr %s is refused; it only prints or hands the work to a daemon", strings.Join(args, " "))
		}
	}
	for _, args := range endedSessionSubrouterAllowed {
		if !mainVerbAllowedInEndedSession("subrouter", args) {
			t.Errorf("subrouter %s is refused; it only prints or hands the work to a daemon", strings.Join(args, " "))
		}
	}
	for _, args := range endedSessionSubrouterRefused {
		if mainVerbAllowedInEndedSession("subrouter", args) {
			t.Errorf("subrouter %s is allowed in an ended login session", strings.Join(args, " "))
		}
	}
}

// The verbs allowed after reading their code, run end to end in an ended
// session: each runs (no refusal), and HOME is as it was but for the Codex
// store's lock files, which any read of it takes. `subrouter accounts` is
// listAccounts and prints the account; `sr accounts` is the team-account verb
// and stays refused.
func TestEndedSessionRunsTheVerbsItAllows(t *testing.T) {
	for _, tc := range []struct {
		program string
		args    []string
		prints  string
	}{
		{"subrouter", []string{"accounts"}, "a@example.com"},
		{"sr", []string{"server", "status"}, "UNREACHABLE"},
		{"sr", []string{"daemon", "status"}, "UNREACHABLE"},
		{"sr", []string{"claude", "help"}, "sr claude"},
		{"sr", []string{"kimi", "help"}, "Usage: sr kimi"},
		{"sr", []string{"qwen", "help"}, "Qwen Token Plan"},
		{"sr", []string{"agy", "help"}, "agy"},
		{"sr", []string{"tenant"}, "tenant"},
		{"sr", []string{"tenant", "help"}, "tenant"},
	} {
		t.Run(tc.program+" "+strings.Join(tc.args, " "), func(t *testing.T) {
			home := seedEndedSessionHome(t)
			endedLoginSession(t, endedSupersetSession())
			before := snapshotTree(t, home)
			stdout, err := captureStdout(t, func() error { return runForProgram(tc.program, tc.args) })
			var refusal *loginSessionRefusal
			if errors.As(err, &refusal) {
				t.Fatalf("%s %s refused: %v", tc.program, strings.Join(tc.args, " "), err)
			}
			if err != nil {
				t.Fatalf("%s %s: %v", tc.program, strings.Join(tc.args, " "), err)
			}
			if !strings.Contains(stdout, tc.prints) {
				t.Fatalf("%s %s printed no %q:\n%s", tc.program, strings.Join(tc.args, " "), tc.prints, stdout)
			}
			if after := snapshotTree(t, home); !equalTrees(withoutLockFiles(before), withoutLockFiles(after)) {
				t.Fatalf("%s %s changed HOME:\n%s", tc.program, strings.Join(tc.args, " "), diffTrees(before, after))
			}
		})
	}
	home := seedEndedSessionHome(t)
	endedLoginSession(t, endedSupersetSession())
	before := snapshotTree(t, home)
	var refusal *loginSessionRefusal
	if err := runForProgram("sr", []string{"accounts"}); !errors.As(err, &refusal) {
		t.Fatalf("sr accounts (the team-account verb) in an ended session: %v, want the gate's refusal", err)
	}
	if after := snapshotTree(t, home); !equalTrees(before, after) {
		t.Fatalf("sr accounts changed HOME before it was refused:\n%s", diffTrees(before, after))
	}
}

// An https health probe would go through the platform verifier, which fails in
// such a shell at the handshake: UNREACHABLE would blame the server. It is not
// sent, so `sr daemon status` and `sr server status` make no TLS request here.
func TestHealthSendsNoTLSFromAnEndedSession(t *testing.T) {
	sandboxHostConfig(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var accepted atomic.Int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = conn.Close()
		}
	}()
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", "https://"+listener.Addr().String()+"/v1")

	endedLoginSession(t, endedSupersetSession())
	var out bytes.Buffer
	if err := runServerHealth(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "not checked (no TLS from this shell)") || accepted.Load() != 0 {
		t.Fatalf("health from an ended session: %d connections, printed:\n%s", accepted.Load(), out.String())
	}

	// Calibration: in a session that works the same probe connects, so the
	// zero above is the check and not a listener nothing could reach.
	probeLoginSession = usableLoginSession
	out.Reset()
	if err := runServerHealth(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for accepted.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if accepted.Load() == 0 || strings.Contains(out.String(), "not checked") {
		t.Fatalf("health from a usable session: %d connections, printed:\n%s", accepted.Load(), out.String())
	}
}

// Every verb srRunner.run dispatches, bare: only these may run in an ended
// login session. A verb added to the allow list must be added here, with the
// reason it needs no Keychain, TLS or launchd in login_session_gate.go.
func TestEveryDirectVerbIsClassifiedForAnEndedSession(t *testing.T) {
	sandboxHostConfig(t)
	unlockRemoteGate(t)
	allowedBare := map[string]bool{
		"status": true, "list": true, "ls": true, "trace": true, "breadcrumbs": true, "why": true, "doctor": true,
		"remote": true, "remotes": true, "server": true, "servers": true, "daemon": true,
		// Bare, it prints its help (srRunner.tenant).
		"tenant": true, "tenants": true,
	}
	var verbs []string
	for verb := range directSRCommands {
		verbs = append(verbs, verb)
	}
	sort.Strings(verbs)
	for _, verb := range verbs {
		if got := srVerbAllowedInEndedSession([]string{verb}); got != allowedBare[verb] {
			t.Errorf("sr %s: allowed in an ended login session = %v, want %v", verb, got, allowedBare[verb])
		}
	}
}

// A refused verb returns before any credential, network or launchd step runs:
// exit 2, and the state dir, the Codex home and the Claude config byte for
// byte as they were (the seeded store needs no legacy migration, and the CLI
// log is main's, which runForProgram does not open). PATH is empty,
// so even a verb the gate let through by mistake could not reach launchctl,
// `security`, a browser or an agent binary from here.
//
// Not driven end to end: the daemons (serve, supervise, front,
// listener-transfer), and the verbs that would talk to cmux.com or a box if the
// gate were ever lost (login, team, account, setup, server install|login|sync,
// token, drain): TestEndedSessionAllowList holds their classification.
func TestEndedSessionRefusesBeforeAnythingRuns(t *testing.T) {
	skipEndToEnd := map[string]bool{
		"serve": true, "supervise": true, "front": true, "listener-transfer": true, "login": true, "team": true,
		"account": true, "setup": true, "token": true, "drain": true, "resume": true, "drain-status": true,
		"server install": true, "server login": true, "server sync": true,
	}
	cases := append(append([][]string{}, endedSessionRefused...), endedSessionMainRefused...)
	for _, args := range cases {
		key := args[0]
		if len(args) > 1 {
			key += " " + args[1]
		}
		if skipEndToEnd[args[0]] || skipEndToEnd[key] {
			continue
		}
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			home := seedEndedSessionHome(t)
			endedLoginSession(t, endedSupersetSession())
			before := snapshotTree(t, home)
			err := runForProgram("sr", args)
			var refusal *loginSessionRefusal
			if !errors.As(err, &refusal) {
				t.Fatalf("sr %s in an ended login session: %v, want the gate's refusal", strings.Join(args, " "), err)
			}
			var stderr bytes.Buffer
			if code := commandExit(&stderr, "subrouter", err); code != 2 {
				t.Fatalf("exit status %d, want 2", code)
			}
			if after := snapshotTree(t, home); !equalTrees(before, after) {
				t.Fatalf("sr %s changed HOME before it was refused:\n%s", strings.Join(args, " "), diffTrees(before, after))
			}
		})
	}
}

// `cx` reaches srRunner.run without passing runForProgram, so run holds the
// same gate.
func TestEndedSessionRefusesInSrRunnerToo(t *testing.T) {
	home := seedEndedSessionHome(t)
	endedLoginSession(t, endedSupersetSession())
	before := snapshotTree(t, home)
	var out bytes.Buffer
	runner := srRunner{program: "cx", store: accounts.DefaultCodexStore(), out: &out, errOut: &out, in: strings.NewReader("")}
	err := runner.run(t.Context(), []string{"claude", "use", "other"})
	var refusal *loginSessionRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("cx claude use in an ended login session: %v, want the gate's refusal", err)
	}
	if after := snapshotTree(t, home); !equalTrees(before, after) {
		t.Fatalf("cx claude use changed HOME before it was refused:\n%s", diffTrees(before, after))
	}
}

// Calibration for the two tests above: in a session that works, the same
// verbs write -- so an unchanged HOME there is the gate, not verbs that never
// write.
func TestAUsableSessionRunsWhatTheGateRefuses(t *testing.T) {
	for _, args := range [][]string{{"claude", "use", "other"}, {"claude", "rename", "other", "renamed"}, {"storage", "local"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			home := seedEndedSessionHome(t)
			before := snapshotTree(t, home)
			if err := runForProgram("sr", args); err != nil {
				t.Fatalf("sr %s in a usable session: %v", strings.Join(args, " "), err)
			}
			if after := snapshotTree(t, home); equalTrees(before, after) {
				t.Fatalf("sr %s wrote nothing in a usable session; the refusal tests prove nothing with it", strings.Join(args, " "))
			}
		})
	}
}

// On a keyless machine a remote verb is answered as a typo (PATCHES 17), and
// the login-session gate must not name it either.
func TestEndedSessionGateNamesNoRemoteVerbOnAKeylessMachine(t *testing.T) {
	sandboxHostConfig(t)
	resetRemoteGateCache()
	t.Cleanup(resetRemoteGateCache)
	t.Setenv("PATH", t.TempDir())
	endedLoginSession(t, endedSupersetSession())
	for _, args := range [][]string{{"server", "install", "box"}, {"remote", "use", "box"}, {"storage", "local"}} {
		err := runForProgram("sr", args)
		if err == nil || err.Error() != unknownRemoteCommandError(args[0]).Error() {
			t.Errorf("keyless sr %s in an ended session: %v, want exactly %v", strings.Join(args, " "), err, unknownRemoteCommandError(args[0]))
		}
	}
}

// Every sentence of the refusal must be true in the case it prints in
// (PATCHES §47). These pin each case's own words and the words it must not
// carry.
func TestTheRefusalSaysWhatHappenedAndWhatToDo(t *testing.T) {
	refusal := &loginSessionRefusal{program: "sr", verb: refusedVerb([]string{"claude", "move", "work"}), state: endedSupersetSession()}
	text := refusal.Error()
	for _, want := range []string{
		"sr claude move: refused: this shell belongs to a macOS login session that has ended (user: unknown userid 502",
		"It runs under Superset's terminal daemon (running since Sep 30 02:31), which outlived that login.",
		"From here sr cannot use the Keychain, verify TLS or reach launchd, so claude move could stop half-way.",
		"The subrouter daemon is not affected (it runs in your current login); `sr status` still works here (it reads the daemon).",
		"open a new Terminal.app (or other non-Superset terminal app) window",
		"New Superset tabs share the ended session until Superset's terminal daemon is restarted",
		"(a person does this in Superset > Settings > Terminal > Terminal daemon > Restart daemon; it closes every Superset terminal. Do not signal the daemon process.)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("refusal lacks %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"pid", "54677", "opened since your last login"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("refusal says %q:\n%s", unwanted, text)
		}
	}
	if got := refusedVerb([]string{"switch", "x@example.com"}); got != "switch" {
		t.Errorf("refusedVerb names the selector: %q", got)
	}

	t.Run("not ended: launchd still answers", func(t *testing.T) {
		state := loginSessionState{reason: "user: unknown userid 502; `launchctl managername` answers \"Aqua\"",
			host: "Superset's terminal daemon", hostKind: hostSuperset}
		text := (&loginSessionRefusal{program: "sr", verb: "switch", state: state}).Error()
		for _, want := range []string{
			"sr switch: refused: this process cannot reach macOS login services: its user lookup failed (user: unknown userid 502",
			"Without that lookup sr cannot use the Keychain, so switch could stop half-way.",
			"run it from a normal terminal window, outside any sandbox",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("refusal lacks %q:\n%s", want, text)
			}
		}
		// launchd answered, so the session has not ended, launchd is not out
		// of reach, and restarting Superset's daemon would change nothing.
		for _, unwanted := range []string{"has ended", "reach launchd", "verify TLS", "Restart daemon", "Superset tabs", "outlived"} {
			if strings.Contains(text, unwanted) {
				t.Errorf("a session launchd still answers for says %q:\n%s", unwanted, text)
			}
		}
	})

	t.Run("tmux inside Superset", func(t *testing.T) {
		probe := loginSessionProbe{
			goos:        "darwin",
			lookupUID:   func() (*user.User, error) { return nil, user.UnknownUserIdError(502) },
			managerName: func() (string, int, error) { return "Could not get manager name.", 153, nil },
			getenv: func(key string) string {
				return map[string]string{"TMUX": "/tmp/tmux-502/default,1,0", "SUPERSET_TERMINAL_ID": "b980"}[key]
			},
			readFile: os.ReadFile,
		}
		text := (&loginSessionRefusal{program: "sr", verb: "pick", state: probe.check()}).Error()
		for _, want := range []string{
			"It runs under a tmux server, which outlived that login.",
			"open a new Terminal.app (or other non-Superset terminal app) window and start a new tmux server there (`tmux -L <name>`)",
			"plain `tmux` would join the old server",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("refusal lacks %q:\n%s", want, text)
			}
		}
		// A new Superset tab or a restarted Superset daemon leaves the tmux
		// server, and every pane of it, where it is.
		if strings.Contains(text, "Restart daemon") || strings.Contains(text, "Superset tabs") || strings.Contains(text, "Superset's terminal daemon") {
			t.Errorf("refusal for a tmux pane sends the user to Superset:\n%s", text)
		}
	})

	// On team or hosted credential storage `sr status` is cloudStatus, which
	// never reaches the daemon view: the refusal must not send the user there.
	t.Run("team credential storage", func(t *testing.T) {
		sandboxHostConfig(t)
		unlockRemoteGate(t)
		endedLoginSession(t, endedSupersetSession())
		allowNothing := func([]string) bool { return false }
		local := refuseInUnusableLoginSession("sr", []string{"pick"}, allowNothing)
		if local == nil || !strings.Contains(local.Error(), "`sr status` still works here") {
			t.Fatalf("on local storage the refusal does not offer `sr status`: %v", local)
		}
		if err := broker.SaveConfig(os.Getenv("SUBROUTER_CLOUD_CONFIG"), broker.Config{
			BaseURL: "https://cmux.com", AccessToken: "stack-access", RefreshToken: "stack-refresh", TeamID: "team-a",
			CredentialSource: broker.CredentialSourceTeam,
		}); err != nil {
			t.Fatal(err)
		}
		team := refuseInUnusableLoginSession("sr", []string{"pick"}, allowNothing)
		if team == nil {
			t.Fatal("no refusal on team storage")
		}
		text := team.Error()
		if strings.Contains(text, "still works") {
			t.Errorf("the refusal offers `sr status` on team storage, where it asks cmux.com:\n%s", text)
		}
		for _, want := range []string{"`sr status` does not read it here: this machine's credential storage is team", "`srq pool` (ops/bin/srq) reads the daemon"} {
			if !strings.Contains(text, want) {
				t.Errorf("refusal on team storage lacks %q:\n%s", want, text)
			}
		}
	})
}

func TestCommandExitStatus(t *testing.T) {
	var out bytes.Buffer
	refusal := &loginSessionRefusal{program: "sr", verb: "pick", state: endedSupersetSession()}
	if code := commandExit(&out, "subrouter", refusal); code != 2 || strings.HasPrefix(out.String(), "subrouter:") ||
		!strings.HasPrefix(out.String(), "sr pick: refused:") {
		t.Fatalf("refusal: exit %d, printed %q; want 2 and the message alone", code, out.String())
	}
	out.Reset()
	if code := commandExit(&out, "cx", errors.New("boom")); code != 1 || out.String() != "cx: boom\n" {
		t.Fatalf("other error: exit %d, printed %q; want 1 and \"cx: boom\"", code, out.String())
	}
}

// runMainChild is main() in a child of the test binary, with the login
// session ended, for TestARefusedVerbExitsTwoFromMain. argv is JSON, its
// program name included, so the child runs as sr, subrouter or cx. The
// parent's sandboxed environment (HOME, the store variables, an empty PATH) is
// the child's.
func runMainChild(argv string) {
	var args []string
	if err := json.Unmarshal([]byte(argv), &args); err != nil || len(args) == 0 {
		fmt.Fprintln(os.Stderr, "main child: bad argv:", argv, err)
		os.Exit(97)
	}
	probeLoginSession = endedSupersetSession
	os.Args = args
	main()
	os.Exit(0)
}

// The exit status a script sees, from main() itself: 2 for a verb the gate
// refused, printed once, with no "subrouter:" or "cx:" prefix in front of a
// message that names the command; 1, with the prefix, for a verb that ran and
// failed. commandExit's own test holds the mapping; this one holds main's use
// of it, for `sr`/`subrouter` and for `cx`, which main dispatches apart.
func TestARefusedVerbExitsTwoFromMain(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		argv   []string
		code   int
		stderr string
	}{
		{[]string{"sr", "pick"}, 2, "sr pick: refused: this shell belongs to a macOS login session that has ended"},
		{[]string{"subrouter", "claude", "move", "work"}, 2, "subrouter claude move: refused: "},
		{[]string{"cx", "switch", "x@example.com"}, 2, "cx switch: refused: "},
		// Calibration: allowed, and fails on its own (no account named).
		{[]string{"sr", "trace"}, 1, "subrouter: usage: subrouter trace <email>"},
	} {
		t.Run(strings.Join(tc.argv, " "), func(t *testing.T) {
			seedEndedSessionHome(t)
			argv, err := json.Marshal(tc.argv)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(binary, "-test.run=^$")
			cmd.Env = append(os.Environ(), "SUBROUTER_TEST_MAIN_CHILD="+string(argv))
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			runErr := cmd.Run()
			code := 0
			var exitErr *exec.ExitError
			if errors.As(runErr, &exitErr) {
				code = exitErr.ExitCode()
			} else if runErr != nil {
				t.Fatalf("child did not run: %v", runErr)
			}
			if code != tc.code || !strings.HasPrefix(stderr.String(), tc.stderr) {
				t.Fatalf("%s: exit %d, stderr %q; want exit %d and a message starting %q (stdout %q)",
					strings.Join(tc.argv, " "), code, stderr.String(), tc.code, tc.stderr, stdout.String())
			}
			if tc.code == 2 && (strings.Contains(stderr.String(), "subrouter: ") || strings.Contains(stderr.String(), "cx: ") ||
				strings.Count(stderr.String(), ": refused: ") != 1) {
				t.Fatalf("%s: the refusal is prefixed or printed twice: %q", strings.Join(tc.argv, " "), stderr.String())
			}
		})
	}
}

func TestDoctorSaysFailFirstInAnEndedSession(t *testing.T) {
	sandboxHostConfig(t)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", "http://127.0.0.1:1/v1")
	endedLoginSession(t, endedSupersetSession())
	var out bytes.Buffer
	_ = runDoctorWith(t.Context(), nil, errors.New("no supervisor in this test"), accounts.DefaultCodexStore(), &out)
	first, _, _ := strings.Cut(out.String(), "\n")
	if !strings.HasPrefix(first, "FAIL  login session") || !strings.Contains(first, "has ended") || !strings.Contains(first, "Restart daemon") ||
		!strings.Contains(first, "a few read-only listings and `claude|codex reset`, and refuses every other verb (exit 2)") {
		t.Fatalf("doctor's first line in an ended login session: %q\n%s", first, out.String())
	}
	// Calibration: a usable session has no such line.
	probeLoginSession = usableLoginSession
	out.Reset()
	_ = runDoctorWith(t.Context(), nil, errors.New("no supervisor in this test"), accounts.DefaultCodexStore(), &out)
	if strings.Contains(out.String(), "login session") {
		t.Fatalf("doctor reports the login session in a usable one:\n%s", out.String())
	}
}

// seedEndedSessionHome is a sandboxed HOME holding what the refused verbs
// would change: a Codex account, two Claude profiles, servers.json and the
// remote key (so the remote verbs reach the gate rather than being answered as
// typos). PATH is empty, and the local daemon address is a closed port.
func seedEndedSessionHome(t *testing.T) string {
	t.Helper()
	home := sandboxHostConfig(t)
	unlockRemoteGate(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", "http://127.0.0.1:1/v1")
	t.Setenv("SR_STATUS_SERVERS", "0")
	t.Setenv("SUBROUTER_CODEX_SERVER", "")
	store := accounts.DefaultCodexStore()
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email: "a@example.com", AddedAt: time.Now().UTC().Format(time.RFC3339), Auth: testCodexAuth("a@example.com", "acct_a"),
	}); err != nil {
		t.Fatal(err)
	}
	claude := agentclaude.DefaultStore()
	for _, name := range []string{"work", "other"} {
		if _, err := claude.CreateProfile(name); err != nil {
			t.Fatal(err)
		}
	}
	if err := claude.SetActiveProfile("work"); err != nil {
		t.Fatal(err)
	}
	servers := `{"servers":[{"name":"box","url":"http://100.64.0.9:31415","adminToken":"t"}]}`
	if err := os.WriteFile(filepath.Join(storepath.CodexDir(), "servers.json"), []byte(servers), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func unlockRemoteGate(t *testing.T) {
	t.Helper()
	dir := storepath.CodexDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "remote.key"), []byte(strings.Repeat("k", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	resetRemoteGateCache()
	t.Cleanup(resetRemoteGateCache)
	if !remoteUnlocked() {
		t.Fatal("fixture failed to unlock the remote gate")
	}
}

// snapshotTree is every path under root with its mode and bytes.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		value := info.Mode().String()
		if info.Mode().IsRegular() {
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += " " + string(body)
		}
		tree[rel] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func equalTrees(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if other, ok := b[key]; !ok || other != value {
			return false
		}
	}
	return true
}

func diffTrees(before, after map[string]string) string {
	var lines []string
	for key, value := range after {
		if old, ok := before[key]; !ok {
			lines = append(lines, "+ "+key)
		} else if old != value {
			lines = append(lines, "~ "+key)
		}
	}
	for key := range before {
		if _, ok := after[key]; !ok {
			lines = append(lines, "- "+key)
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
