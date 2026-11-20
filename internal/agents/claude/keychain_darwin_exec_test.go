//go:build darwin

package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The seam-fake tests cover the store's keychain logic on every OS; what they
// cannot cover is the exec wiring inside securityKeychain itself. This keeps
// that wiring pinned against a fake `security` on PATH — the exact argument
// order, the -U upsert flag, and the -w read — so a drifted invocation cannot
// hide behind the fakes.
func TestSecurityKeychainDrivesSecurityCLI(t *testing.T) {
	fakeBin := t.TempDir()
	recordPath := filepath.Join(t.TempDir(), "security-arguments")
	payloadPath := filepath.Join(t.TempDir(), "payload")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"$SUBROUTER_KEYCHAIN_TEST_RECORD\"\n" +
		"case \"$1\" in\n" +
		"find-generic-password) cat \"$SUBROUTER_KEYCHAIN_TEST_PAYLOAD\" ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "security"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	stored := `{"claudeAiOauth":{"accessToken":"stored"}}`
	if err := os.WriteFile(payloadPath, []byte(stored+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SUBROUTER_KEYCHAIN_TEST_RECORD", recordPath)
	t.Setenv("SUBROUTER_KEYCHAIN_TEST_PAYLOAD", payloadPath)

	kc := securityKeychain{}
	ctx := context.Background()
	if err := kc.write(ctx, "svc", "acct", []byte(`{"written":true}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	body, ok, err := kc.read(ctx, "svc", "acct")
	if err != nil || !ok {
		t.Fatalf("read found nothing: found=%v err=%v", ok, err)
	}
	if got := string(body); got != stored+"\n" {
		t.Fatalf("read = %q, want the script's payload verbatim", got)
	}
	if err := kc.delete(ctx, "svc", "acct"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	record, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	wantLines := []string{
		`add-generic-password -U -s svc -a acct -w {"written":true}`,
		`find-generic-password -s svc -a acct -w`,
		`delete-generic-password -s svc -a acct`,
	}
	gotLines := strings.Split(strings.TrimRight(string(record), "\n"), "\n")
	if len(gotLines) != len(wantLines) {
		t.Fatalf("security invocations = %q, want %q", gotLines, wantLines)
	}
	for i, want := range wantLines {
		if gotLines[i] != want {
			t.Fatalf("security invocation %d = %q, want %q", i, gotLines[i], want)
		}
	}
}

// `security delete-generic-password` on a missing item is not a failure; any
// other non-zero exit is.
func TestSecurityKeychainDeleteDistinguishesMissingFromFailure(t *testing.T) {
	fakeBin := t.TempDir()
	scriptPath := filepath.Join(fakeBin, "security")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	kc := securityKeychain{}

	missing := "#!/bin/sh\n" +
		"echo 'security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.' >&2\n" +
		"exit 44\n"
	if err := os.WriteFile(scriptPath, []byte(missing), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := kc.delete(context.Background(), "svc", "acct"); err != nil {
		t.Fatalf("missing item reported as failure: %v", err)
	}

	failing := "#!/bin/sh\n" +
		"echo 'security: unable to open the login keychain' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(scriptPath, []byte(failing), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := kc.delete(context.Background(), "svc", "acct"); err == nil {
		t.Fatal("real deletion failure was swallowed")
	}
}

// `security find-generic-password` exits 44 for a missing item. That is
// "absent"; any other failure (a locked keychain, a denied prompt) is an error
// removal must not read as absent.
func TestSecurityKeychainReadDistinguishesMissingFromFailure(t *testing.T) {
	fakeBin := t.TempDir()
	scriptPath := filepath.Join(fakeBin, "security")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	kc := securityKeychain{}

	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 44\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if body, found, err := kc.read(context.Background(), "svc", "acct"); err != nil || found || body != nil {
		t.Fatalf("missing item = found %v body %q err %v, want absent", found, body, err)
	}

	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho locked >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, found, err := kc.read(context.Background(), "svc", "acct"); err == nil || found {
		t.Fatalf("locked keychain = found %v err %v, want an error", found, err)
	}
}
