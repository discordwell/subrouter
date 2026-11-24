package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/discordwell/subrouter/internal/storepath"
)

// systemdServiceHome is install-systemd's default --home, the state directory
// the box's service runs in (its bootstrap script never overrides it). A
// variable for tests.
var systemdServiceHome = "/var/lib/subrouter"

// drainCommandTimeout bounds one drain-socket request. A variable for tests.
var drainCommandTimeout = 10 * time.Second

// drainSocketDialed runs once the drain socket has accepted the connection,
// before the request is written. A seam for tests.
var drainSocketDialed = func() {}

// errDrainSocketUnanswered: the drain socket accepted the connection and then
// closed it without an answer, which is how its listener refuses a peer
// (drainSocketListener closes it before reading a byte). Which error that
// surfaces as is a race between the close and this side's write: a reset or
// EOF when it reads, EPIPE or (darwin) ENOTCONN when it writes, net/http's
// unexported "server closed idle connection" when the close lands first. So
// it is recognised by what happened, a connection that was accepted and got no
// answer, not by an errno.
var errDrainSocketUnanswered = errors.New("the drain socket closed the connection without answering")

// drainCommand is `subrouter drain|resume|drain-status [--socket PATH]`: the
// daemon's drain socket, from a shell on the daemon's machine, with no token
// (PATCHES section 38.1). On the box: `sudo subrouter drain`.
func drainCommand(verb string, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet(verb, flag.ContinueOnError)
	flags.SetOutput(stderr)
	socketFlag := flags.String("socket", "", "the daemon's drain socket; default: $"+drainSocketEnv+", else drain.sock in the state directory ($SUBROUTER_STATE_DIR, ~/.subrouter, then the systemd service's)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%s takes no arguments, got %q", verb, flags.Args())
	}
	socket, _, err := resolveDrainSocket(*socketFlag, os.Getenv)
	if err != nil {
		return err
	}
	// A socket, not a symlink: as root this talks to a path in a directory the
	// service user owns, and a link there could point it at any listener.
	if info, err := os.Lstat(socket); err == nil && info.Mode().Type() != os.ModeSocket {
		return fmt.Errorf("%s is not a socket (%s): refusing to talk to it. The daemon binds a socket there itself", socket, fileKind(info.Mode()))
	}
	method := http.MethodPost
	if verb == "drain-status" {
		method = http.MethodGet
	}
	code, body, err := drainSocketRequest(socket, method, "/_subrouter/"+verb)
	if err != nil {
		return drainSocketDialError(socket, err)
	}
	// What answers is not vouched for: under sudo this is root reading a
	// socket the service user controls. Nothing it sent reaches the terminal
	// raw, so escape sequences cannot drive root's terminal (OSC 52 writes the
	// clipboard).
	if code != http.StatusOK {
		return fmt.Errorf("%s over %s: HTTP %d: %q", verb, socket, code, strings.TrimSpace(body))
	}
	printable, ok := reencodeDrainStatus(body)
	if !ok {
		return fmt.Errorf("%s over %s: HTTP 200, but the answer is not the daemon's JSON: %q", verb, socket, strings.TrimSpace(body))
	}
	fmt.Fprint(stdout, printable)
	switch verb {
	case "drain":
		fmt.Fprintln(stderr, "drained: new sessions get 503 until `subrouter resume` or the next restart; sessions already running keep being served")
	case "resume":
		fmt.Fprintln(stderr, "resumed: new sessions are admitted again")
	}
	return nil
}

// reencodeDrainStatus is a drain-socket answer as the terminal may see it:
// the JSON object decoded and encoded again, with every character that is not
// printable escaped, and false when the answer is no JSON object.
// encoding/json already escapes the C0 controls; the C1 controls (U+0080 to
// U+009F, among them the one-byte CSI) and other unprintable runes it leaves
// raw, so they are escaped here.
func reencodeDrainStatus(body string) (string, bool) {
	var value map[string]any
	if err := json.Unmarshal([]byte(body), &value); err != nil {
		return "", false
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", false
	}
	var out strings.Builder
	for _, r := range string(encoded) {
		if r == '\n' || (r >= ' ' && r < 0x7f) || (r >= utf8.RuneSelf && unicode.IsPrint(r)) {
			out.WriteRune(r)
			continue
		}
		// Only inside a string can such a rune appear (encoding/json escapes
		// the C0 controls, and leaves DEL and the rest raw), and \u is a
		// valid escape there.
		if r1, r2 := utf16.EncodeRune(r); r1 != unicode.ReplacementChar {
			fmt.Fprintf(&out, `\u%04x\u%04x`, r1, r2)
		} else {
			fmt.Fprintf(&out, `\u%04x`, r)
		}
	}
	out.WriteByte('\n')
	return out.String(), true
}

func fileKind(mode fs.FileMode) string {
	switch mode.Type() {
	case fs.ModeSymlink:
		return "a symlink"
	case fs.ModeDir:
		return "a directory"
	case 0:
		return "a regular file"
	}
	return "mode " + mode.Type().String()
}

// resolveDrainSocket picks the socket to talk to: --socket, then
// SUBROUTER_DRAIN_SOCKET, then SUBROUTER_STATE_DIR's drain.sock -- each used as
// given -- and otherwise the first of these that exists:
//
//   - ~/.subrouter/drain.sock, this user's own daemon (a laptop);
//   - the systemd service's, from /etc/default/subrouter when this user can
//     read it (root, on the box);
//   - /var/lib/subrouter/drain.sock, install-systemd's default home (the
//     service user, or a user who cannot read that file and is told to sudo),
//     unless that file sets SUBROUTER_DRAIN_SOCKET: then the service's socket
//     is what it says, and none when it says off or a relative path (the
//     daemon refuses one). A stale socket at the default path must not stand
//     in for it.
//
// A path this user may see is preferred; failing that, one it may not look at
// counts too, so that the permission error says to use sudo rather than that
// there is no socket. When none exists the error names every place it looked;
// tried lists them.
func resolveDrainSocket(flagValue string, getenv func(string) string) (string, []string, error) {
	explicit := func(path, from string) (string, []string, error) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", nil, fmt.Errorf("%s: %w", from, err)
		}
		return abs, []string{abs}, nil
	}
	if value := strings.TrimSpace(flagValue); value != "" {
		return explicit(value, "--socket")
	}
	if value := strings.TrimSpace(getenv(drainSocketEnv)); value != "" {
		if drainSocketDisabled(value) {
			return "", nil, fmt.Errorf("%s=%s: the daemon serves no drain socket; pass --socket PATH to name one", drainSocketEnv, value)
		}
		return explicit(value, drainSocketEnv)
	}
	if value := strings.TrimSpace(getenv("SUBROUTER_STATE_DIR")); value != "" {
		return explicit(filepath.Join(value, drainSocketName), "SUBROUTER_STATE_DIR")
	}
	var candidates []string
	add := func(path string) {
		path = filepath.Clean(path)
		for _, seen := range candidates {
			if seen == path {
				return
			}
		}
		candidates = append(candidates, path)
	}
	add(filepath.Join(storepath.StateDir(), drainSocketName))
	envFile := systemdDefaultPath(systemdConfig{ServiceName: defaultSystemdServiceName})
	// serviceNone says why the service serves no socket, when its env file
	// says so.
	var serviceNone error
	switch value := strings.TrimSpace(readDefaultValue(envFile, drainSocketEnv)); {
	case value == "":
		if state := readDefaultValue(envFile, "SUBROUTER_STATE_DIR"); filepath.IsAbs(state) {
			add(filepath.Join(state, drainSocketName))
		}
		add(filepath.Join(systemdServiceHome, drainSocketName))
	case drainSocketDisabled(value):
		serviceNone = fmt.Errorf("%s sets %s=%s: the daemon serves no drain socket. Drain with the admin token, or set it to a path and restart", envFile, drainSocketEnv, value)
	case !filepath.IsAbs(value):
		serviceNone = fmt.Errorf("%s sets %s=%s, which is not an absolute path: the daemon serves no drain socket (it logged why at startup). Drain with the admin token, or set an absolute path and restart", envFile, drainSocketEnv, value)
	default:
		add(value)
	}
	// A socket this user can see wins over one it cannot: under `sudo -u
	// subrouter` a HOME left pointing at the operator's own directory hides
	// ~/.subrouter, and that must not shadow the service's socket.
	for _, candidate := range candidates {
		if _, err := os.Lstat(candidate); err == nil {
			return candidate, candidates, nil
		}
	}
	for _, candidate := range candidates {
		if _, err := os.Lstat(candidate); !errors.Is(err, fs.ErrNotExist) {
			return candidate, candidates, nil
		}
	}
	if serviceNone != nil {
		return "", candidates, serviceNone
	}
	return "", candidates, drainSocketNotFound(strings.Join(candidates, ", "))
}

func drainSocketNotFound(where string) error {
	return fmt.Errorf("no drain socket at %s: no daemon runs here, or it runs a build from before the drain socket (PATCHES §38.1), or it serves one elsewhere (--socket PATH, %s). From another machine, drain with the admin token", where, drainSocketEnv)
}

// drainSocketRequest makes one HTTP request over the Unix socket at socket. A
// connection accepted and then closed with no answer, and no timeout, is
// errDrainSocketUnanswered.
func drainSocketRequest(socket, method, path string) (int, string, error) {
	var connected atomic.Bool
	client := &http.Client{
		Timeout: drainCommandTimeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				conn, err := dialer.DialContext(ctx, "unix", socket)
				if err == nil {
					connected.Store(true)
					drainSocketDialed()
				}
				return conn, err
			},
		},
	}
	request, err := http.NewRequest(method, "http://drain-socket"+path, nil)
	if err != nil {
		return 0, "", err
	}
	response, err := client.Do(request)
	if err != nil {
		var timeout interface{ Timeout() bool }
		if connected.Load() && !(errors.As(err, &timeout) && timeout.Timeout()) {
			return 0, "", fmt.Errorf("%w (%w)", errDrainSocketUnanswered, err)
		}
		return 0, "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return 0, "", err
	}
	return response.StatusCode, string(body), nil
}

// drainSocketDialError says what a failed connection to the drain socket
// means for the operator, and what to do about it.
func drainSocketDialError(socket string, err error) error {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("permission denied on %s: the drain socket belongs to the daemon's user. Run this as root (sudo) or as that user (sudo -u subrouter on the box)", socket)
	case errors.Is(err, fs.ErrNotExist):
		return drainSocketNotFound(socket)
	case errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("nothing is listening on %s: the daemon that left it there is not running", socket)
	case errors.Is(err, errDrainSocketUnanswered):
		// The listener closes a peer whose uid is neither root's nor its own
		// before reading a byte (drainSocketListener).
		return fmt.Errorf("the daemon refused this user on %s: only root and the daemon's own user may use its drain socket (%v)", socket, err)
	}
	return fmt.Errorf("drain socket %s: %w", socket, err)
}
