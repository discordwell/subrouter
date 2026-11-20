package claude

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// PATCHES §47. In a shell whose macOS login session has ended (Superset's
// terminal daemon, or a tmux server, outliving a logout) `security` cannot
// create its keychain search. It says so on stderr (-50), then prints the
// "could not be found" line, and exits 44: the exit status and the words of
// the vault's answer that the item is missing. Measured from such a shell, 2026-09-30,
// against a service name that exists nowhere:
//
//	SecKeychainSearchCreateFromAttributes: One or more parameters passed to a function were not valid.
//	SecKeychainSearchCopyNext: The specified item could not be found in the keychain.
//	rc=44
//
// Read as an answer, every keychain-only profile is "has no access token" and
// every keychain delete has "succeeded". The shims below print exactly that.
const deadSessionSecurityOutput = `echo "SecKeychainSearchCreateFromAttributes: One or more parameters passed to a function were not valid." >&2
echo "SecKeychainSearchCopyNext: The specified item could not be found in the keychain." >&2
exit 44`

func TestASecurityThatCouldNotCreateItsSearchIsNotAnAnswer(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		securityShim(t, deadSessionSecurityOutput)
		answerBeforeTheClock(t)
		body, found, err := securityKeychain{}.read(context.Background(), "svc", "acct")
		if err == nil {
			t.Fatalf("read with no keychain search: found=%v body=%q err=nil; that is the vault's answer of absence, and the keychain was never searched", found, body)
		}
		if found || len(body) != 0 {
			t.Fatalf("read with no keychain search: found=%v body=%q, want nothing", found, body)
		}
		// Bounded like a read that never ran (the hour), never a refusal (the
		// quarter hour): a session with no security server fails every
		// profile's read at once.
		if !errors.Is(err, errSecurityReadNotRun) || !securityNeverAnswered(err) {
			t.Fatalf("read with no keychain search: %v, want errSecurityReadNotRun", err)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("read with no keychain search reads as a load cut: %v", err)
		}
		if !strings.Contains(err.Error(), securitySearchNotCreated) {
			t.Fatalf("read with no keychain search does not say why: %v", err)
		}
	})
	t.Run("delete", func(t *testing.T) {
		securityShim(t, deadSessionSecurityOutput)
		err := securityKeychain{}.delete(context.Background(), "svc", "acct")
		if err == nil {
			t.Fatal("delete with no keychain search reported success; nothing was deleted")
		}
		if !errors.Is(err, ErrCredentialUnchecked) || !strings.Contains(err.Error(), securitySearchNotCreated) {
			t.Fatalf("delete with no keychain search: %v, want ErrCredentialUnchecked naming the failed search", err)
		}
	})
}

// The calibration the fix must not blur: the same exit 44, and the same "could
// not be found" line, WITHOUT the search failure are the vault's real answer
// that the item is missing. Read: absent, no error. Delete: done.
func TestASecurityThatSearchedAndFoundNothingIsStillAnAnswer(t *testing.T) {
	const searchedAndMissing = `echo "security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain." >&2
exit 44`
	t.Run("read", func(t *testing.T) {
		securityShim(t, searchedAndMissing)
		answerBeforeTheClock(t)
		_, found, err := securityKeychain{}.read(context.Background(), "svc", "acct")
		if found || err != nil {
			t.Fatalf("exit 44 from a search that ran: found=%v err=%v, want an answer of absence", found, err)
		}
	})
	t.Run("delete", func(t *testing.T) {
		securityShim(t, searchedAndMissing)
		if err := (securityKeychain{}).delete(context.Background(), "svc", "acct"); err != nil {
			t.Fatalf("deleting an item the keychain says is missing: %v, want nil", err)
		}
	})
}
