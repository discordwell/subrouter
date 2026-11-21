package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
//
// The command's contract IS its stdout -- bootstrap reads it through a command
// substitution -- so asserting on the printed bytes is asserting the thing that
// can actually break. A test that called LoadBoxIdentity directly would pass
// while the command printed a label, a prefix, or nothing at all.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	// The pipe is read while fn prints: fn blocks once the pipe is full.
	type capture struct {
		out []byte
		err error
	}
	captured := make(chan capture, 1)
	go func() {
		out, err := io.ReadAll(read)
		captured <- capture{out, err}
	}()
	saved := os.Stdout
	os.Stdout = write
	runErr := fn()
	os.Stdout = saved
	if err := write.Close(); err != nil {
		t.Fatalf("close write end: %v", err)
	}
	got := <-captured
	if got.err != nil {
		t.Fatalf("read captured output: %v", got.err)
	}
	return string(got.out), runErr
}

// A command can print more than a pipe holds: at most 64 KiB on Linux and
// macOS, and 8 KiB on Linux once a user's pipes pass fs.pipe-user-pages-soft,
// as they do in the Docker test harness on a busy machine. `sr plot`'s
// page is about 26 KiB, and with nothing reading the pipe it blocked on the
// full pipe until the test binary timed out.
func TestCaptureStdoutTakesMoreThanAPipeHolds(t *testing.T) {
	page := strings.Repeat("burn pace row\n", 1<<16)
	stdout := os.Stdout
	captured := make(chan string, 1)
	go func() {
		out, _ := captureStdout(t, func() error {
			_, err := io.WriteString(os.Stdout, page)
			return err
		})
		captured <- out
	}()
	select {
	case out := <-captured:
		if out != page {
			t.Fatalf("captured %d bytes, want the %d printed", len(out), len(page))
		}
	case <-time.After(30 * time.Second):
		// The print is still blocked on the full pipe, so the test binary's
		// own report goes to the real stdout instead.
		os.Stdout = stdout
		t.Fatalf("a %d-byte print did not finish in 30 s: captureStdout reads nothing until the command returns", len(page))
	}
}

// The pin an operator pastes into a peer must be the pin the daemon actually
// presents. Two derivations of "the fingerprint" -- say one hashing the PEM and
// one hashing the DER -- produce a value that looks entirely plausible and
// matches nothing, failing later as a bare TLS handshake error.
func TestFingerprintCommandPrintsTheServedPin(t *testing.T) {
	stateDir := t.TempDir()
	served, err := proxy.LoadOrCreateBoxIdentity(stateDir)
	if err != nil {
		t.Fatalf("create identity: %v", err)
	}

	out, err := captureStdout(t, func() error {
		// --sessions, not --state-dir: serve derives the state dir from the
		// session path, and this command must resolve it the same way or it
		// reads an identity nothing serves.
		return fingerprintCommand([]string{"--sessions", filepath.Join(stateDir, "sessions.json")})
	})
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}

	printed := strings.TrimSpace(out)
	if printed != served.Fingerprint {
		t.Fatalf("printed pin %q, but the listener serves %q", printed, served.Fingerprint)
	}
	// Bare hex and nothing else. A label or trailing prose survives a substring
	// comparison but lands inside the value an operator pastes.
	if out != served.Fingerprint+"\n" {
		t.Errorf("stdout = %q, want exactly the pin and a newline: bootstrap captures this verbatim", out)
	}
	if len(printed) != 64 {
		t.Errorf("pin is %d chars, want 64 hex", len(printed))
	}
}

// The command must never CREATE an identity.
//
// It is normally run as root while the daemon runs unprivileged, so a creating
// read leaves a root-owned 0600 keypair the daemon cannot open --- and
// LoadOrCreateBoxIdentity refuses an unreadable identity rather than replacing
// it, so the daemon then fails to start rather than degrading. The failure would
// appear one restart later, attributed to the restart.
func TestFingerprintCommandNeverCreatesAnIdentity(t *testing.T) {
	stateDir := t.TempDir()

	_, err := captureStdout(t, func() error {
		return fingerprintCommand([]string{"--sessions", filepath.Join(stateDir, "sessions.json")})
	})
	if err == nil {
		t.Fatal("no identity present, but the command succeeded; it must not generate one")
	}
	if !strings.Contains(err.Error(), "no TLS identity") {
		t.Errorf("error = %q, want it to say the box has no public listener yet", err)
	}

	entries, readErr := os.ReadDir(stateDir)
	if readErr != nil {
		t.Fatalf("read state dir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("command wrote %d file(s) into the state dir: %v; a root-run read would leave "+
			"them root-owned and stop the daemon from starting", len(entries), entries)
	}
}

// Reading is idempotent: the same box prints the same pin every time. If this
// ever rotates, every configured peer breaks at once and the symptom is a peer
// outage rather than a certificate change.
func TestFingerprintCommandIsStableAcrossCalls(t *testing.T) {
	stateDir := t.TempDir()
	if _, err := proxy.LoadOrCreateBoxIdentity(stateDir); err != nil {
		t.Fatalf("create identity: %v", err)
	}
	sessions := filepath.Join(stateDir, "sessions.json")

	first, err := captureStdout(t, func() error {
		return fingerprintCommand([]string{"--sessions", sessions})
	})
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	second, err := captureStdout(t, func() error {
		return fingerprintCommand([]string{"--sessions", sessions})
	})
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if first != second {
		t.Fatalf("pin changed between reads: %q then %q", first, second)
	}
}

// The subcommand has to be reachable by name. Wiring the function without adding
// it to the dispatch switch leaves every unit test above passing while
// `subrouter fingerprint` reports "unknown command" on the box.
func TestFingerprintIsReachableThroughDispatch(t *testing.T) {
	err := run([]string{"fingerprint", "--sessions", filepath.Join(t.TempDir(), "sessions.json")})
	if err == nil {
		t.Fatal("expected the missing-identity error, got success")
	}
	if strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("`subrouter fingerprint` is not dispatched: %v", err)
	}
}
