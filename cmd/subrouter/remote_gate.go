package main

// The remote surface -- our self-hosted box (server/remote/tenant) and the cmux
// hosted vault (login/logout/storage/team/account) -- exists only on a machine
// holding the key.
//
// Why: on a fresh machine the box was already inert (no servers.json means no
// spill, no push, no peer pool), but it was not invisible. Every command needed
// to turn it on was listed in `sr --help` on every machine, so the only thing
// between a fresh install and a remote pool was whether its user had read the
// help text. Without the key this build is local-only: nothing that can
// send a credential off this machine is reachable, and nothing names it.
//
// What this is NOT: access control. This source is public, so anyone who reads
// this file knows the path below and could write the file. The real boundary is the box's own tokens, which are not here
// and are not derivable from here. This gate removes discovery and accident,
// which is the whole of what it claims to do.
//
// The key's contents are deliberately random and meaningless to the box -- the
// file is proof of possession, not a credential. That way copying it to a second
// machine grants nothing that reading it could not, and losing it leaks no box
// access. Tokens keep living where they live: adminToken and accountImportToken
// in servers.json, SUBROUTER_REMOTE_POOL_TOKEN in the daemon's environment. The
// admin/proxy token split is untouched.

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/discordwell/subrouter/internal/proxy"
	"github.com/discordwell/subrouter/internal/storepath"
)

const (
	remoteKeyFileName = "remote.key"
	// remoteKeyMinLength rejects `touch remote.key`. Possession of the file is
	// the gate, so an empty one must not open it.
	remoteKeyMinLength = 16
)

// remoteKeyPath resolves the store directory storepath.CodexDir() serves, the
// same one defaultSRServerStore uses for servers.json. Deliberately not a second
// spelling of "where is the store": PATCHES.md 12 records that two gates
// answering one question differently is how the fail-open admin bug happened.
// It also means SUBROUTER_STATE_DIR isolation works here for free.
//
// It asks through CodexDirForReadOnlyInspection -- the directory CodexDir would
// serve after its legacy migration, without performing that copy -- because
// the gate is consulted by read-only commands too (`sr codex isolation-check`
// reaches it through srRunner.run's dispatch guard). Asking a
// question must not import ~/.codex-accounts into an empty state dir. Once the
// store exists the two helpers name the same directory, so the verdict is the
// same either way.
func remoteKeyPath() string {
	return filepath.Join(storepath.CodexDirForReadOnlyInspection(), remoteKeyFileName)
}

// remoteGateVerdict is the one answer to "may this machine reach beyond
// itself". refusal is set only when a key exists and was refused: a machine
// that never had a key is not told the file's name.
type remoteGateVerdict struct {
	unlocked bool
	refusal  *remoteKeyRefusal
}

// remoteKeyRefusalKind is why an existing remote.key did not open the gate,
// one kind per check of resolveRemoteGate.
type remoteKeyRefusalKind uint8

const (
	keyNotRegularFile remoteKeyRefusalKind = iota + 1
	keyModeTooOpen
	keyUnreadable
	keyTooShort
)

type remoteKeyRefusal struct {
	kind remoteKeyRefusalKind
	path string
	perm os.FileMode // keyModeTooOpen only
	err  error       // keyUnreadable only
}

func (r remoteKeyRefusal) warning() string {
	switch r.kind {
	case keyNotRegularFile:
		return fmt.Sprintf("subrouter: %s is not a regular file; remote stays off", r.path)
	case keyModeTooOpen:
		return fmt.Sprintf("subrouter: %s is mode %04o; chmod 600 it to enable remote", r.path, r.perm)
	case keyUnreadable:
		return fmt.Sprintf("subrouter: cannot read %s (%v); remote stays off", r.path, r.err)
	case keyTooShort:
		return fmt.Sprintf("subrouter: %s is too short to be a key; remote stays off", r.path)
	}
	panic(fmt.Sprintf("remote key refusal of unknown kind %d", r.kind))
}

var (
	remoteGateOnce    sync.Once
	remoteGateCurrent remoteGateVerdict
)

// remoteGate resolves the verdict ONCE per process.
//
// The architecture rule this fork keeps re-learning is "publish the verdict;
// never re-derive it" -- eligibility lived in four places and every copy
// eventually disagreed with the router. One Once, one answer, so two call sites
// in the same run cannot reach opposite conclusions because the file changed
// between them.
func remoteGate() remoteGateVerdict {
	remoteGateOnce.Do(func() {
		remoteGateCurrent = resolveRemoteGate(os.Stderr)
	})
	return remoteGateCurrent
}

func remoteUnlocked() bool {
	return remoteGate().unlocked
}

// resetRemoteGateCache is test-only: the Once is the production contract, and
// tests need to change HOME/SUBROUTER_STATE_DIR between cases.
func resetRemoteGateCache() {
	remoteGateOnce = sync.Once{}
	remoteGateCurrent = remoteGateVerdict{}
}

func resolveRemoteGate(warn io.Writer) remoteGateVerdict {
	return resolveRemoteGateAt(remoteKeyPath(), warn)
}

// resolveRemoteGateAt is the gate for the key at path. The stand-in
// (standby.go) asks it for the state folder it was started with, which a flag
// can name; every other caller goes through remoteGate.
func resolveRemoteGateAt(path string, warn io.Writer) remoteGateVerdict {
	// Deliberately no migration: an earlier draft minted a key when servers.json
	// held a server, so a machine that had been set up before the gate kept
	// working untouched. That makes servers.json a second key -- a machine with
	// one would have remote ON without holding the key, which is the opposite of
	// what this gate is for. The key is the only key. Machines that should keep
	// their access are given one before the binary lands.
	info, err := os.Stat(path)
	if err != nil {
		// No key: total silence. Naming the file here would defeat the point.
		return remoteGateVerdict{}
	}
	// Past this line the operator already knows the file exists, so explaining
	// why it did not work leaks nothing and saves a baffling silent lock.
	refuse := func(refusal remoteKeyRefusal) remoteGateVerdict {
		refusal.path = path
		fmt.Fprintln(warn, refusal.warning())
		return remoteGateVerdict{refusal: &refusal}
	}
	if !info.Mode().IsRegular() {
		return refuse(remoteKeyRefusal{kind: keyNotRegularFile})
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return refuse(remoteKeyRefusal{kind: keyModeTooOpen, perm: perm})
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return refuse(remoteKeyRefusal{kind: keyUnreadable, err: err})
	}
	if len(strings.TrimSpace(string(body))) < remoteKeyMinLength {
		return refuse(remoteKeyRefusal{kind: keyTooShort})
	}
	return remoteGateVerdict{unlocked: true}
}

// remotePoolFromEnv builds the peer-pool config from the environment, or
// returns nil when this machine holds no key. nil is already NewRemotePool's own
// "unset" answer -- the inert state the proxy handles everywhere, not a new one
// to teach it.
//
// SUBROUTER_REMOTE_POOL_FINGERPRINT pins the peer's self-signed certificate.
// Required for an https:// peer -- without it the dialers fall back to CA
// validation, which refuses a self-signed box. Ignored for a plain-http tailnet
// peer, which is what exists today.
func remotePoolFromEnv() *proxy.RemotePool {
	if !remoteUnlocked() {
		return nil
	}
	if remotePeersFilePath() != "" {
		// The peers file IS the peer set (PATCHES §31, D0): the single-peer
		// variables are ignored, never merged, so the daemon routes to exactly
		// the peers the operator wrote down.
		return nil
	}
	pool := proxy.NewRemotePool(
		os.Getenv("SUBROUTER_REMOTE_POOL"),
		os.Getenv("SUBROUTER_REMOTE_POOL_TOKEN"),
		os.Getenv("SUBROUTER_REMOTE_POOL_ALLOW"),
	).WithFingerprint(os.Getenv("SUBROUTER_REMOTE_POOL_FINGERPRINT"))
	dialPeersOverTailnet([]*proxy.RemotePool{pool})
	return pool
}

// A peers-file field would make builds already deployed reject the whole file
// and arm no peer at all.
const peerTailnetEnv = "SUBROUTER_PEER_TAILNET"

func dialPeersOverTailnet(peers []*proxy.RemotePool) {
	if err := proxy.DialPeersOverTailnet(peers, os.Getenv(peerTailnetEnv)); err != nil {
		slog.Warn(peerTailnetEnv+" ignored; every peer dials its URL", "error", err)
	}
}

// remotePeersFileEnv names a JSON file listing SEVERAL peers (PATCHES §31, D0;
// owner rule P7: rank "the best across everything you can see").
const remotePeersFileEnv = "SUBROUTER_REMOTE_POOLS_FILE"

// singlePeerEnv is the single-peer configuration the peers file supersedes.
var singlePeerEnv = []string{
	"SUBROUTER_REMOTE_POOL", "SUBROUTER_REMOTE_POOL_TOKEN",
	"SUBROUTER_REMOTE_POOL_ALLOW", "SUBROUTER_REMOTE_POOL_FINGERPRINT",
}

func remotePeersFilePath() string {
	return strings.TrimSpace(os.Getenv(remotePeersFileEnv))
}

// remotePeersFromEnv builds the peer set from SUBROUTER_REMOTE_POOLS_FILE, or
// returns nil when this machine holds no key or the variable is unset (the
// single-peer variables then apply, unchanged: remotePoolFromEnv).
//
// Behind the same remote.key gate as the single peer: a file on disk must not
// arm what an environment variable cannot. Fails CLOSED -- a file that does not
// parse, is readable by others, or has any invalid entry yields NO peers, and
// the single-peer variables are not consulted as a fallback, because the
// operator asked for the file. Every message names variables, indexes, peer
// names and fields; never a URL's credentials, a token or a fingerprint.
func remotePeersFromEnv(warn io.Writer) []*proxy.RemotePool {
	if !remoteUnlocked() {
		return nil
	}
	path := remotePeersFilePath()
	if path == "" {
		return nil
	}
	var ignored []string
	for _, name := range singlePeerEnv {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			ignored = append(ignored, name)
		}
	}
	if len(ignored) > 0 {
		fmt.Fprintf(warn, "subrouter: %s is set; ignoring %s\n", remotePeersFileEnv, strings.Join(ignored, ", "))
	}
	peers, err := proxy.LoadRemotePeersFile(path)
	if err != nil {
		fmt.Fprintf(warn, "subrouter: %s: %v; no remote peers are configured\n", remotePeersFileEnv, err)
		return nil
	}
	dialPeersOverTailnet(peers)
	return peers
}

// remoteCommands is the ONE list. Dispatch, both help texts, and the tests all
// key on it, so a command cannot be hidden from one surface and left visible in
// another -- which is exactly the drift this repo has shipped before.
var remoteCommands = []string{
	// our self-hosted box
	"server", "servers",
	"remote", "remotes",
	"tenant", "tenants",
	// the cmux hosted vault
	"login", "logout",
	"storage",
	"team",
	"account", "accounts",
	// upstream #570: reads and sets a selected server's Claude API-key
	// credit grants, so it only ever reaches another machine.
	"key-credit", "key-credits",
	// `sr claude move` and `sr claude push` are gated inside the claude
	// namespace (claudeRunner.run), and `sr claude remove --server` by
	// resolveServer: this list keys on the top-level verb only.
}

func isRemoteCommand(name string) bool {
	name = strings.TrimSpace(strings.ToLower(name))
	for _, candidate := range remoteCommands {
		if name == candidate {
			return true
		}
	}
	return false
}

// unknownRemoteCommandError is the error a gated command produces when locked.
// It is byte-identical to what a typo produces, so the failure carries no hint
// that a gated feature is behind it.
func unknownRemoteCommandError(name string) error {
	return fmt.Errorf("unknown account command %q", name)
}

// namesRemoteSurface reports whether one help line mentions another machine.
//
// Keying on the gated-command list alone was not enough, and the gap was not
// hypothetical: on a keyless machine the help still printed
// "sr export <name> [--copy] [--server <name>]" and
// "sr remove <email> [--server <name>]", because the filter only ever looked at
// the SECOND field and both of those verbs are legitimately local. Two more
// lines (claude-aws, spend) named "the server" in their prose, and `sr` itself
// advertised "then each configured server". PATCHES.md 17 claims "nothing names
// it"; that claim was false in five places before this function existed.
//
// The vocabulary is deliberately blunt. A false positive hides one line of help
// on a machine that cannot use that line anyway; a false negative is the thing
// the whole gate exists to prevent.
func namesRemoteSurface(line string) bool {
	lower := strings.ToLower(line)
	for _, token := range []string{"--server", "<server>", "server", "remote", "tenant", "cmux"} {
		if strings.Contains(lower, token) {
			return true
		}
	}
	return false
}

// stripRemoteHelp removes every help line naming a gated command, plus the
// indented continuation lines beneath it and any section header left empty.
//
// Filtering rather than forking the help text is deliberate. Upstream rewrites
// this text often -- a second hand-maintained copy is the thing that drifts, and
// a rebase that edits the original would silently leave ours stale. A filter
// keyed on remoteCommands survives an upstream edit that a forked copy loses,
// and the test asserts the result names no gated command whatever upstream does
// to the wording.
func stripRemoteHelp(text, program string) string {
	program = strings.TrimSpace(program)
	if program == "" {
		program = "subrouter"
	}
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	dropping := false
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		switch {
		case trimmed == "":
			dropping = false
			kept = append(kept, line)
		case strings.HasPrefix(trimmed, program+" "):
			fields := strings.Fields(trimmed)
			dropping = (len(fields) > 1 && isRemoteCommand(fields[1])) || namesRemoteSurface(trimmed)
			if !dropping {
				kept = append(kept, line)
			}
		case dropping && line != trimmed:
			// An indented continuation of a dropped command's description.
		case line != trimmed && namesRemoteSurface(trimmed):
			// A continuation of a KEPT command that still names the remote.
			// Dropped on its own so the command survives without its remote
			// sentence -- `sr` itself is the case that matters: the verb is
			// local, only its "and then each configured server" clause is not.
		default:
			dropping = false
			kept = append(kept, line)
		}
	}
	return dropEmptySections(kept)
}

// dropEmptySections removes a "Header:" line whose body filtering emptied, along
// with the blank line that followed it, so the locked help has no bare headings.
func dropEmptySections(lines []string) string {
	kept := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		if !isHelpSectionHeader(lines[i]) {
			kept = append(kept, lines[i])
			continue
		}
		if sectionHasBody(lines, i) {
			kept = append(kept, lines[i])
			continue
		}
		// Skip the header and the blank run beneath it.
		for i+1 < len(lines) && strings.TrimSpace(lines[i+1]) == "" {
			i++
		}
	}
	return collapseBlankRuns(kept)
}

func isHelpSectionHeader(line string) bool {
	return line != "" &&
		!strings.HasPrefix(line, " ") &&
		!strings.HasPrefix(line, "\t") &&
		strings.HasSuffix(strings.TrimRight(line, " "), ":")
}

func sectionHasBody(lines []string, header int) bool {
	for i := header + 1; i < len(lines); i++ {
		if isHelpSectionHeader(lines[i]) {
			return false
		}
		if strings.TrimSpace(lines[i]) != "" {
			return true
		}
	}
	return false
}

func collapseBlankRuns(lines []string) string {
	out := make([]string, 0, len(lines))
	blank := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
