package session

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Local tests for the durable-state port (PATCHES.md section 28). The upstream
// tests for the same change live, unmodified, in store_durability_test.go.

// A zero-length sessions.json is the specific wreck a power loss leaves behind
// when the file was written without an fsync before its rename: APFS (and ext4
// mounted without auto_da_alloc) can commit the rename before the data blocks.
// NewStore used to fail on it, and NewStore failing is fatal to `serve`
// (cmd/subrouter/main.go) and to loading a tenant (internal/proxy/multitenant.go),
// so launchd/systemd restarted the daemon into the same parse error forever.
func TestNewStoreStartsOnZeroLengthStoreLeftByPowerLoss(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore on the zero-length file a power loss leaves: %v", err)
	}
	if _, err := store.Put("claude", "s1", "acct-1", ""); err != nil {
		t.Fatalf("Put after starting on a zero-length file: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "sessions.json.corrupt-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("corrupt copies = %v, want exactly one", matches)
	}
}

// Upstream's recovery starts EMPTY whenever the file does not parse. That is
// right for a process that has nothing better (a daemon starting after the
// crash), but loadLocked also runs on every Get, Put, Touch and CompareAndPut of
// a running daemon. Starting empty there would drop every warm session's
// assignment, so each one's next turn would be re-picked cold: a move that is
// not an eviction, which the hour-long warm rule (PATCHES.md sections 20-21)
// forbids. A running daemon's own in-memory assignments are the newest state
// anyone has, so it keeps them and writes them back over the quarantined file.
func TestCorruptionAfterStartKeepsInMemoryAssignments(t *testing.T) {
	cases := []struct {
		name string
		// firstTouch is the first store operation after the out-of-band
		// corruption. Get is the read path every proxied request takes; Put is
		// a new session being assigned before any request reads the store.
		firstTouch func(t *testing.T, store *Store)
	}{
		{
			name: "read path first",
			firstTouch: func(t *testing.T, store *Store) {
				got, ok := store.Get("claude", "warm-1")
				if !ok || got.AccountID != "acct-1" {
					t.Fatalf("Get after corrupt reload = %+v, %v; a warm session lost its account and would be re-picked cold", got, ok)
				}
			},
		},
		{
			name: "write path first",
			firstTouch: func(t *testing.T, store *Store) {
				if _, err := store.Put("claude", "new-3", "acct-3", ""); err != nil {
					t.Fatalf("Put after corruption: %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "sessions.json")
			store, err := NewStore(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Put("claude", "warm-1", "acct-1", ""); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Put("codex", "warm-2", "acct-2", ""); err != nil {
				t.Fatal(err)
			}
			corrupt := []byte("{\"claude:warm-1\": {\"agent_ty")
			if err := os.WriteFile(path, corrupt, 0o600); err != nil {
				t.Fatal(err)
			}

			tc.firstTouch(t, store)

			counts := store.CountByAccount()
			if counts["acct-1"] != 1 || counts["acct-2"] != 1 {
				t.Fatalf("CountByAccount after corruption = %v; the spread signal lost the warm sessions", counts)
			}
			// A process started now -- a daemon restart, or `sr` reading
			// SUBROUTER_SESSIONS -- must read the assignments the running daemon
			// wrote back, not an empty or unparseable file.
			reopened, err := NewStore(path)
			if err != nil {
				t.Fatalf("NewStore after mid-run corruption: %v", err)
			}
			for _, want := range []struct{ agent, session, account string }{
				{"claude", "warm-1", "acct-1"},
				{"codex", "warm-2", "acct-2"},
			} {
				got, ok := reopened.Get(want.agent, want.session)
				if !ok || got.AccountID != want.account {
					t.Fatalf("reopened %s:%s = %+v, %v; want %s", want.agent, want.session, got, ok, want.account)
				}
			}
			matches, err := filepath.Glob(filepath.Join(dir, "sessions.json.corrupt-*"))
			if err != nil {
				t.Fatal(err)
			}
			if len(matches) != 1 {
				t.Fatalf("corrupt copies = %v, want exactly one", matches)
			}
			body, err := os.ReadFile(matches[0])
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), "agent_ty") {
				t.Fatalf("quarantined file lost the original bytes: %q", body)
			}
		})
	}
}

// If writing memory back over the torn file fails (a full or failing disk),
// the torn file must stay live and memory must survive until a later access
// can write. Moving the torn file away first -- upstream's quarantine -- would
// make the next Get read "no file", which loadLocked treats as "no
// assignments": every warm session would be re-picked on exactly the disk
// where the re-picks' own writes fail too, so they would churn per request.
func TestFailedRewriteAfterCorruptionKeepsMemoryAndRetries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "warm-1", "acct-1", ""); err != nil {
		t.Fatal(err)
	}
	corrupt := []byte("{\"claude:warm-1\": {\"agent_ty")
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	store.writeFile = func(string, []byte, os.FileMode) error {
		return errors.New("no space left on device")
	}
	for attempt := 1; attempt <= 2; attempt++ {
		got, ok := store.Get("claude", "warm-1")
		if !ok || got.AccountID != "acct-1" {
			t.Fatalf("Get %d while the rewrite fails = %+v, %v; memory was dropped", attempt, got, ok)
		}
	}
	if _, err := store.Put("claude", "new-2", "acct-2", ""); err == nil {
		t.Fatal("Put succeeded although every write fails")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("live file after failed rewrites: %v", err)
	}
	if !bytes.Equal(body, corrupt) {
		t.Fatalf("live file = %q, want the torn bytes left in place", body)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "sessions.json.corrupt-*")); len(matches) != 0 {
		t.Fatalf("failed rewrites left copies behind: %v", matches)
	}

	store.writeFile = nil
	if got, ok := store.Get("claude", "warm-1"); !ok || got.AccountID != "acct-1" {
		t.Fatalf("Get once writes work again = %+v, %v", got, ok)
	}
	reopened, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore after the rewrite succeeded: %v", err)
	}
	if got, ok := reopened.Get("claude", "warm-1"); !ok || got.AccountID != "acct-1" {
		t.Fatalf("reopened warm-1 = %+v, %v", got, ok)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "sessions.json.corrupt-*")); len(matches) != 1 {
		t.Fatalf("corrupt copies = %v, want exactly one", matches)
	}
}
