package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/broker"
)

// The gate in front of every verb when this process cannot use macOS login
// services (PATCHES §47, login_session.go).
//
// It is an ALLOW list on purpose. A verb added later is refused by default in
// such a shell, which costs one confusing refusal; the opposite default costs
// a half-finished move, a switch made blind, or a daemon booted out with no
// way to bootstrap it again -- the failures this exists to stop.
//
// Everything allowed below only prints, or hands the work to a daemon over
// loopback or pinned TLS (checked verb by verb, 2026-09-30):
//
//   - help; `sr status`, bare `sr` and `sr <selector>`, which read this
//     machine's rows from its daemon here (sr_status_daemon_view.go), with no
//     prompt and nothing numbered to switch to;
//   - `sr plot`: reads this machine's rows and its peer view from its daemon
//     over loopback with no credential, and each servers.json box the way
//     `sr status` reads it; it only prints. No Keychain, no refresh, no
//     launchd;
//   - `sr list` and `sr trace`: Codex store files (on a legacy machine the
//     selected server's list, over its pinned path);
//   - `sr remote [-v|list|current]`, `sr server [list|current]`: servers.json;
//   - `sr daemon [status|logs]` and `sr server status` (no name): health probes
//     (runServerHealth; an https address is not probed here,
//     serverHealthLabel) and `tail` of the log files;
//   - `subrouter accounts` under any name but sr: listAccounts, the Codex
//     store's files, printed (`sr accounts` is the cmux team-account verb,
//     refused below);
//   - the help forms: `sr claude help`, `sr kimi|qwen|agy|tenant help`, and
//     bare `sr tenant`, which prints its help;
//   - `sr doctor`, which says FAIL for this condition first;
//   - `sr codex isolation-check`: a read-only inspection of the Codex store;
//   - `sr claude reset` / `sr codex reset` (and --dry-run): the holding
//     daemon claims the reset; this process only reads usage-status and posts
//     the claim, over loopback HTTP, a tailnet address, or the box's pinned
//     public path. No Keychain, no launchd. An https URL with no pin would need
//     the platform verifier, which fails at the handshake, before the claim
//     is sent.
//
// Refused, with the reason it is not in the list:
//   - every Claude/Codex/Kimi/Qwen/Antigravity/Grok launch, `sr claude` and
//     `sr codex` included: the launch paths autostart the daemon through
//     launchd when a 1.5 s probe misses, lease team credentials from cmux.com
//     over system TLS, and the bare `sr claude` picker reads every profile's
//     Keychain item; not all of that could be shown to stay off the login
//     services, so they fail closed;
//   - `sr claude rename`, `sr claude switch|use`: registry writes that would
//     work (a rename keeps the profile's directory, so its Keychain service
//     does not move), but they are writes to the store and none needs this
//     shell; `sr claude run`: `claude auth status` reads the Keychain through
//     `security`, says "not logged in", and the error steers the user to add
//     a new profile;
//   - `sr claude list|status` and `sr usage`: they print, but every row they
//     print here is a Keychain or TLS failure dressed as the account's state;
//     `sr status` shows the same accounts from the daemon;
//   - `sr server status <name>`: not runServerHealth but serverStatusFor,
//     which first heals the entry (healAdminServer may rewrite servers.json
//     from Tailscale) and, with the Claude web balance opted in, scrapes
//     balances and posts them to the box;
//   - `sr claude env`: it prints one line, but resolving the directory
//     (ClaudeConfigDir) migrates the profile's shared Claude state and takes
//     its locks, and the line exists to point a `claude auth login` at a
//     profile, a Keychain write this shell cannot make;
//   - everything that writes credentials, auth files, the store, a plist or
//     launchd, or decides a write from a Keychain or TLS answer: add, import,
//     login, logout, move, push, remove, switch, pick, setup, cleanup, daemon
//     start|stop|restart, server install, serve, supervise, front, the
//     installers.

// srVerbAllowedInEndedSession is the allow list for srRunner.run's verbs.
// args are run's, after its provider-first folds.
func srVerbAllowedInEndedSession(args []string) bool {
	if len(args) == 0 {
		return true
	}
	sub := ""
	if len(args) > 1 {
		sub = args[1]
	}
	switch args[0] {
	case "help", "-h", "--help", "status", "plot", "list", "ls", "trace", "breadcrumbs", "why", "doctor":
		return true
	case "remote", "remotes":
		return oneOf(sub, "", "-v", "list", "ls", "current", "default", "help", "-h", "--help")
	case "server", "servers":
		return oneOf(sub, "", "list", "ls", "current", "default", "help", "-h", "--help") ||
			(sub == "status" && len(args) == 2)
	case "daemon":
		return oneOf(sub, "", "status", "logs", "help", "-h", "--help")
	case "codex":
		return oneOf(sub, "isolation-check", "reset")
	case "claude":
		return oneOf(sub, "reset", "help", "-h", "--help")
	case "kimi", "qwen", "agy", "antigravity":
		// run answers these before anything else; bare, each is a launch.
		return oneOf(sub, "help", "-h", "--help")
	case "tenant", "tenants":
		return oneOf(sub, "", "help", "-h", "--help")
	}
	// `sr <selector>`: one account's row (statusOne), read like `sr status`.
	return strings.Contains(args[0], "@")
}

// mainVerbAllowedInEndedSession is the allow list for runForProgram, which
// dispatches by program as well as verb: it walks runForProgram's own order,
// so each verb is judged by the code that would run it. A verb handed to
// srRunner.run is judged by run's list, so the two cannot disagree. Under any
// name but sr, runForProgram's own switch comes first, and `accounts` there
// is listAccounts, not sr's team-account verb. The rest of that switch --
// serve, supervise, front, the installers, the Codex launcher, naked, token,
// drain -- is refused, and a daemon born in such a shell would find every
// Keychain-only profile unreadable.
func mainVerbAllowedInEndedSession(program string, args []string) bool {
	if len(args) == 0 {
		return true
	}
	if isCodexAccountCommand(args) || (program == "sr" && (isDirectSRCommand(args[0]) || strings.Contains(args[0], "@"))) {
		return runnerVerbAllowedInEndedSession(args)
	}
	switch args[0] {
	case "help", "-h", "--help", "probe", "fingerprint", "accounts":
		return true
	case "cx":
		return runnerVerbAllowedInEndedSession(args[1:])
	case "naked", "direct", "serve", "supervise", "front", "listener-transfer", "codex", "install-daemon",
		"install-systemd", "install-launchd", "drain", "resume", "drain-status", "token":
		return false
	}
	if isDirectSRCommand(args[0]) || strings.Contains(args[0], "@") {
		return runnerVerbAllowedInEndedSession(args)
	}
	return false
}

// runnerVerbAllowedInEndedSession judges a verb runForProgram hands to
// srRunner.run, by run's list and after run's folds. A remote verb on a
// keyless machine passes: run answers it as a typo before its own gate, and a
// refusal here would name a remote command on a machine that must name none
// (PATCHES 17).
func runnerVerbAllowedInEndedSession(args []string) bool {
	args = normalizeAddRefreshArgs(normalizeProviderAddArgs(args))
	if len(args) > 0 && !remoteUnlocked() && isRemoteCommand(args[0]) {
		return true
	}
	return srVerbAllowedInEndedSession(args)
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

// refuseInUnusableLoginSession returns a loginSessionRefusal for a verb the
// allow list does not name, when this process cannot use macOS login
// services, and nil otherwise. The probe runs only for a verb the list would
// refuse, and costs one uid lookup in a session that works.
func refuseInUnusableLoginSession(program string, args []string, allowed func([]string) bool) error {
	if allowed(args) {
		return nil
	}
	state := probeLoginSession()
	if state.usable {
		return nil
	}
	return &loginSessionRefusal{program: program, verb: refusedVerb(args), state: state, statusElsewhere: statusAsksElsewhere()}
}

// statusAsksElsewhere says why `sr status` would not read this machine's
// daemon here, or "" when it would. srRunner.run hands `status` to cloudStatus
// on team or hosted credential storage, before the daemon view is ever
// reached, so there the refusal must not send the user to it. A file read
// (cloud.json, the remote key): nothing here needs the login services.
func statusAsksElsewhere() string {
	config, err := cloudModeConfig()
	if err != nil {
		return fmt.Sprintf("it cannot load this machine's credential storage setting (%v)", err)
	}
	switch source := config.EffectiveCredentialSource(); source {
	case broker.CredentialSourceTeam, broker.CredentialSourceHosted:
		return fmt.Sprintf("this machine's credential storage is %s, which it asks instead of the daemon", source)
	}
	return ""
}

// loginSessionRefusal is a verb refused by the gate. main exits 2 on it, and
// prints it without its own "subrouter:" prefix: the message names the
// command itself.
type loginSessionRefusal struct {
	program string
	verb    string
	state   loginSessionState
	// statusElsewhere is statusAsksElsewhere's answer when the verb was
	// refused: why `status` would not read the daemon here, or "".
	statusElsewhere string
}

func (e *loginSessionRefusal) Error() string {
	program := e.program
	if strings.TrimSpace(program) == "" {
		program = "subrouter"
	}
	daemon := fmt.Sprintf("The subrouter daemon is not affected (%s); `%s status` still works here (it reads the daemon).",
		e.state.daemonWhere(), program)
	if e.statusElsewhere != "" {
		daemon = fmt.Sprintf("The subrouter daemon is not affected (%s), but `%s status` does not read it here: %s. "+
			"`srq pool` (ops/bin/srq) reads the daemon.", e.state.daemonWhere(), program, e.statusElsewhere)
	}
	return joinSentences(
		fmt.Sprintf("%s %s: refused: %s.", program, e.verb, e.state.situation()),
		e.state.hostClause(),
		e.state.cannotClause(program, e.verb),
		daemon,
		e.state.remedy(),
	)
}

// refusedVerb is the command as the user would name it: the verb, and its
// subcommand for the verbs that have them. A launch's own arguments and a
// selector are left out.
func refusedVerb(args []string) string {
	if len(args) == 0 {
		return ""
	}
	verb := args[0]
	if len(args) > 1 && !strings.HasPrefix(args[1], "-") && !strings.Contains(args[1], "@") {
		switch verb {
		case "claude", "codex", "daemon", "server", "servers", "remote", "remotes", "kimi", "qwen",
			"agy", "antigravity", "tenant", "tenants", "team", "account", "accounts", "storage", "add", "gemini":
			verb += " " + args[1]
		}
	}
	return verb
}

// commandExit prints a failed command's error the way main always has and
// returns the exit status: 2 for a verb the login-session gate refused, which
// a script can tell from a verb that ran and failed (1).
func commandExit(w io.Writer, prefix string, err error) int {
	var refusal *loginSessionRefusal
	if errors.As(err, &refusal) {
		fmt.Fprintln(w, err)
		return 2
	}
	var quiet quietFailure
	if errors.As(err, &quiet) {
		return 1
	}
	fmt.Fprintln(w, prefix+":", err)
	return 1
}

// quietFailure is exit 1 for a command whose own output has already said why
// it failed, so a "subrouter: ..." line would say it twice. what is never
// printed by commandExit.
type quietFailure struct{ what string }

func (e quietFailure) Error() string { return e.what }
