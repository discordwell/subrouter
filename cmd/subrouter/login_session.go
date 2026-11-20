package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A shell in a macOS login session that has ended (PATCHES §47).
//
// Superset's terminal daemon (pty-daemon.js) outlives a logout, and so do a
// tmux or screen server. Every shell under one stays attached to the ended
// session's launchd bootstrap namespace, and from there the user lookup fails
// (`user: unknown userid 502`), `security` cannot create a keychain search
// (-50), Go's platform TLS verifier answers `x509: OSStatus -26276`, and
// launchctl refuses (`managername` exit 153, `print` exit 141 "Reentrancy
// avoided"). Superset's own guard probes the namespace once, when its daemon
// starts, so a daemon started earlier in a login that later ended is adopted
// by the next login as healthy. The subrouter daemon is a launchd job of the
// CURRENT login and is unaffected.
//
// From such a shell `sr status` rendered every local row as an error, and
// verbs stopped half-way: `sr claude move` of a file-backed profile uploaded
// it, verified the server held it, then could not delete the local Keychain
// copy -- one refresh-token chain refreshed in two places, which is a dead
// account at the next rotation. deploy-laptop.sh printed DEPLOY OK while the
// old binary kept serving.
//
// So the CLI asks once whether this process can use the login services every
// local credential path needs. When it cannot, `sr status` reads its local
// rows from the daemon (sr_status_daemon_view.go) and every other verb is
// refused before any credential, network or launchd step runs
// (login_session_gate.go).

// loginSessionState is what probeLoginSession found. Every field but usable
// is set only when usable is false.
type loginSessionState struct {
	// usable is false when this process cannot use macOS login services.
	usable bool
	// ended is launchctl's word on it: `launchctl managername` exited non-zero,
	// so the launchd session this shell belongs to is gone. False with usable
	// false means launchd still answers and only the user lookup failed
	// (a sandbox that denies mach lookups is one way to get there).
	ended bool
	// reason is what was measured, for the user to check: the lookup's error
	// and launchctl's answer.
	reason string
	// host names what kept this shell alive, when the environment says.
	host string
	// hostKind is which host that is. The way to a working shell differs by
	// host: a new tmux server, a new screen session, or a new window of a
	// terminal app that is not Superset.
	hostKind terminalHostKind
}

// terminalHostKind is the process a shell runs under, as far as its
// environment says.
type terminalHostKind int

const (
	hostUnnamed terminalHostKind = iota
	hostTmux
	hostScreen
	hostSuperset
)

// probeLoginSession is the check. A variable only so this package's tests
// default it to a usable session (login_session_test.go): run from a shell
// whose session has ended -- where this was written -- every test of a gated
// verb would otherwise refuse. Production asks once per process; a process's
// bootstrap namespace does not change under it.
var probeLoginSession = onceLoginSessionProbe(func() loginSessionState { return defaultLoginSessionProbe().check() })

// defaultLoginSessionProbe is the probe production runs: this OS, this
// process's uid, the real environment. TestTheProductionProbeAsksThisProcess
// holds each input to it.
func defaultLoginSessionProbe() loginSessionProbe {
	return loginSessionProbe{
		goos:        runtime.GOOS,
		lookupUID:   lookupCurrentUID,
		managerName: func() (string, int, error) { return launchctlManagerName() },
		getenv:      os.Getenv,
		readFile:    os.ReadFile,
	}
}

func onceLoginSessionProbe(probe func() loginSessionState) func() loginSessionState {
	var once sync.Once
	var state loginSessionState
	return func() loginSessionState {
		once.Do(func() { state = probe() })
		return state
	}
}

// loginSessionProbe holds every input of the check, so a test can drive the
// darwin branch on any OS.
type loginSessionProbe struct {
	goos string
	// lookupUID is user.LookupId of this process's uid. It returns the user
	// too, which check ignores, so a test can tell the real lookup from a stub
	// that answers nil on a machine where the lookup works.
	lookupUID func() (*user.User, error)
	// managerName is `launchctl managername`: its output, its exit status
	// (-1 when it did not run to one) and why it did not.
	managerName func() (string, int, error)
	getenv      func(string) string
	readFile    func(string) ([]byte, error)
}

// check decides on one signal: user.LookupId of this process's uid returning
// user.UnknownUserIdError. It is the exact precondition the Claude store needs
// before every Keychain call (store.go, its user.Current sites), so the gate
// and the failure cannot disagree. LookupId rather than user.Current, which
// caches its first answer for the life of the process. No cgo: os/user reaches
// libc through the syscall trampolines on darwin, so CGO_ENABLED=0 (CI) and
// the Makefile's cgo build answer alike; both were measured failing the same
// way in an ended session.
//
// Every other GOOS is usable: the box is Linux, and nothing changes there.
//
// launchctl runs only once the lookup has failed, and only to say why: a
// non-zero exit means the session is gone; an answer means launchd is still
// there and something else stops the lookup. Neither changes the verdict.
//
// What it must never grow: a $USER fallback for the lookup, or file-based TLS
// roots. Each would make one symptom go away without letting this process use
// the Keychain, and each removes the net that today stops the store from
// reading every Keychain item as absent (PATCHES §47, store.go
// securityCouldNotSearch).
func (p loginSessionProbe) check() loginSessionState {
	if p.goos != "darwin" {
		return loginSessionState{usable: true}
	}
	_, err := p.lookupUID()
	var unknown user.UnknownUserIdError
	if !errors.As(err, &unknown) {
		return loginSessionState{usable: true}
	}
	state := loginSessionState{reason: err.Error()}
	name, code, runErr := p.managerName()
	switch {
	case runErr == nil && code == 0:
		state.reason += fmt.Sprintf("; `launchctl managername` answers %q", name)
	case code > 0:
		state.ended = true
		detail := ""
		if name != "" {
			detail = ": " + name
		}
		state.reason += fmt.Sprintf("; `launchctl managername` exits %d%s", code, detail)
	default:
		state.reason += fmt.Sprintf("; `launchctl managername` did not run (%v)", runErr)
	}
	state.host, state.hostKind = p.terminalHost()
	return state
}

func lookupCurrentUID() (*user.User, error) {
	return user.LookupId(strconv.Itoa(os.Getuid()))
}

// terminalHost names what kept this shell alive: the innermost host, because
// that is the process that outlived the login and the one to replace. A tmux
// server started from a Superset tab carries both TMUX and
// SUPERSET_TERMINAL_ID, and its panes stay in the ended session whatever
// happens to Superset; a new tmux server is what gets them out. So TMUX first,
// then STY, then Superset.
//
// Superset is named by its own variables: it sets TERM_PROGRAM=kitty on
// purpose, so that cannot tell it apart. Its manifest adds the daemon's start
// time, which is what a user compares against their last login; a missing or
// odd manifest only leaves it out. Never the daemon's pid: these messages are
// read by agents too, and `kill <pid>` from one ends every Superset terminal,
// live sessions in the current login included.
func (p loginSessionProbe) terminalHost() (string, terminalHostKind) {
	switch {
	case strings.TrimSpace(p.getenv("TMUX")) != "":
		return "a tmux server", hostTmux
	case strings.TrimSpace(p.getenv("STY")) != "":
		return "a screen session", hostScreen
	case strings.TrimSpace(p.getenv("SUPERSET_TERMINAL_ID")) != "":
		host := "Superset's terminal daemon"
		if since := p.supersetDaemonStarted(); since != "" {
			host += " (running since " + since + ")"
		}
		return host, hostSuperset
	}
	return "", hostUnnamed
}

// supersetDaemonStarted is the start time in the daemon's manifest, when the
// manifest names a daemon (a pid and a start time) at all.
func (p loginSessionProbe) supersetDaemonStarted() string {
	home := strings.TrimSpace(p.getenv("SUPERSET_HOME_DIR"))
	org := strings.TrimSpace(p.getenv("SUPERSET_ORGANIZATION_ID"))
	if home == "" || org == "" || p.readFile == nil {
		return ""
	}
	body, err := p.readFile(filepath.Join(home, "host", org, "pty-daemon-manifest.json"))
	if err != nil {
		return ""
	}
	var manifest struct {
		PID       int   `json:"pid"`
		StartedAt int64 `json:"startedAt"`
	}
	if json.Unmarshal(body, &manifest) != nil || manifest.PID <= 0 || manifest.StartedAt <= 0 {
		return ""
	}
	return time.UnixMilli(manifest.StartedAt).Local().Format("Jan 2 15:04")
}

// launchctlManagerName runs `launchctl managername`, by path, under a 2 s
// clock. A variable so this package's tests never run launchctl
// (login_session_test.go answers "Aqua"); requireAquaLaunchd and the probe's
// explanation share it.
var launchctlManagerName = runLaunchctlManagerName

func runLaunchctlManagerName() (string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "/bin/launchctl", "managername")
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return strings.TrimSpace(stdout.String()), 0, nil
	case ctx.Err() != nil:
		return "", -1, fmt.Errorf("no answer within 2s")
	case errors.As(err, &exitErr) && exitErr.Exited():
		return strings.TrimSpace(stderr.String() + stdout.String()), exitErr.ExitCode(), nil
	default:
		return "", -1, err
	}
}

// situation is the sentence's subject, with what was measured. Every sentence
// built from these must be true in the case it prints in: when launchd still
// answers, all that is known is that the user lookup failed, so nothing here
// says the session ended or that launchd is out of reach.
func (s loginSessionState) situation() string {
	if s.ended {
		return "this shell belongs to a macOS login session that has ended (" + s.reason + ")"
	}
	return "this process cannot reach macOS login services: its user lookup failed (" + s.reason + ")"
}

// cannotClause is what that costs the refused verb.
func (s loginSessionState) cannotClause(program, verb string) string {
	if s.ended {
		return fmt.Sprintf("From here %s cannot use the Keychain, verify TLS or reach launchd, so %s could stop half-way.", program, verb)
	}
	// The Claude store resolves the user before every Keychain call
	// (store.go, its user.Current sites); that much is known.
	return fmt.Sprintf("Without that lookup %s cannot use the Keychain, so %s could stop half-way.", program, verb)
}

// hostClause says what kept the shell alive, or nothing when nothing does.
func (s loginSessionState) hostClause() string {
	switch {
	case s.host != "" && s.ended:
		return "It runs under " + s.host + ", which outlived that login."
	case s.host != "":
		return "It runs under " + s.host + "."
	case s.ended:
		return "Its terminal outlived that login."
	default:
		return ""
	}
}

// remedy is what gets the user a shell that works. "A terminal opened since
// your last login" is not one: a new Superset tab is opened by the same old
// terminal daemon, and a new tmux or screen window by the same old server, so
// each is in the ended session too. A new window of a terminal app is not,
// because logout quits every app. Superset's daemon is the exception that
// survives, and restarting it is a person's call: it closes every Superset
// terminal, so the text says so and asks that nobody signal the process.
// Quitting and relaunching Superset is not enough either, because the
// relaunched app adopts the old daemon, shells and all.
//
// When launchd still answers (not ended), none of that applies: something
// around this process stops the lookup, and a normal terminal is the way out.
func (s loginSessionState) remedy() string {
	const newWindow = "open a new Terminal.app (or other non-Superset terminal app) window"
	if !s.ended {
		return "To use sr, run it from a normal terminal window, outside any sandbox."
	}
	switch s.hostKind {
	case hostTmux:
		return "To get a working shell, " + newWindow + " and start a new tmux server there (`tmux -L <name>`): " +
			"plain `tmux` would join the old server, which belongs to the ended session."
	case hostScreen:
		return "To get a working shell, " + newWindow + " and start a new screen session there " +
			"(`screen`, not `screen -r`, which would reattach to the old one)."
	case hostSuperset:
		return "To get a working shell, " + newWindow + ". New Superset tabs share the ended session until " +
			"Superset's terminal daemon is restarted (a person does this in Superset > Settings > Terminal > " +
			"Terminal daemon > Restart daemon; it closes every Superset terminal. Do not signal the daemon process.)"
	default:
		return "To get a working shell, " + newWindow + "."
	}
}

// endedSessionRemedyHere is the remedy for this process's own terminal host,
// for a caller that knows the session has ended without having probed it
// (requireAquaLaunchd, from launchctl's own refusal).
func endedSessionRemedyHere() string {
	state := loginSessionState{ended: true}
	probe := defaultLoginSessionProbe()
	state.host, state.hostKind = probe.terminalHost()
	return state.remedy()
}

// daemonWhere is why the daemon still works when this shell does not.
func (s loginSessionState) daemonWhere() string {
	if s.ended {
		return "it runs in your current login"
	}
	return "it does not run in this process"
}

func joinSentences(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, " ")
}

func capitalize(text string) string {
	if text == "" {
		return text
	}
	return strings.ToUpper(text[:1]) + text[1:]
}
