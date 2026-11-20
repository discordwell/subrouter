package claude

import (
	"errors"
	"fmt"
	"io/fs"
	"testing"
)

// systemSecurityTool is where macOS ships `security`: /usr/bin, on the sealed
// system volume, on every release. The store runs it by this path, never by
// looking `security` up on PATH (PATCHES §34.6).
//
// By name, the tool was whatever this process's PATH found first. A daemon
// whose PATH lacks /usr/bin (`sr install-daemon --path` overridden, a
// hand-edited plist, `sr serve` from a stripped environment) could not start
// it, and every keychain-only read went unchecked. Unchecked is not safe
// there: the store stays "silent" for as long as the process lives, so
// observeCredentialRead's bound turns it into ErrCredentialUnavailable for
// every keychain-only profile at once -- the dead-login verdict that moves
// warm sessions for good, only later. And a `security` earlier on PATH
// (defaultDaemonPath puts the install dir, ~/bin, /opt/homebrew/bin and
// /usr/local/bin first) answered for the login keychain: its exit 44 is an
// immediate verdict. By this path, a failed start is a broken OS or a host
// that cannot fork, never a PATH accident.
const systemSecurityTool = "/usr/bin/security"

// securityTool is the `security` securityKeychain runs.
//
// A test binary runs it by name instead, through PATH. Every test that drives
// securityKeychain puts a shim alone on PATH, and every test that must not
// reach the login keychain empties PATH and checks that `security` does not
// resolve (keychainOnlyProfile, the proxy's isolateFromSecurityBinary). Run by
// absolute path, each of those would reach the real keychain; a package that
// forgot a TestMain override would too, silently. So the choice is made from
// testing.Testing(), which is true only in a `go test` binary, and a test that
// exercises the production path sets this var to an absolute shim itself
// (TestAMacRunsTheSystemSecurityNeverOneFromPATH).
var securityTool = securityToolFor(testing.Testing())

func securityToolFor(inTestBinary bool) string {
	if inTestBinary {
		return "security"
	}
	return systemSecurityTool
}

// securityStartFailure keeps a `security` that could not be started from
// posing as a missing file. Started by absolute path, a missing tool is a
// *fs.PathError wrapping ENOENT, and errors.Is reads that as fs.ErrNotExist
// -- the store's own spelling of "no credentials file", an answer of absence.
// Only the text is kept. (By name through PATH it was exec.ErrNotFound, which
// is not fs.ErrNotExist.)
func securityStartFailure(err error) error {
	if err != nil && errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s could not be started (%v)", securityTool, err)
	}
	return err
}
