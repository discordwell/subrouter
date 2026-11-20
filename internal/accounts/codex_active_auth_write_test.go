package accounts

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A failed write must never leave the user without an auth.json. The old
// writer renamed auth.json to .bak first and then wrote a fixed auth.json.tmp;
// when that temp write failed, Codex had no login at all.
func TestWriteCodexActiveAuthNeverLeavesAuthFileMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	previous := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"previous"}}`)
	if err := os.WriteFile(path, previous, 0o600); err != nil {
		t.Fatal(err)
	}
	// A leftover directory at the old fixed temp name makes a fixed-name temp
	// write fail.
	if err := os.Mkdir(path+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}

	writeErr := writeCodexActiveAuth(path, json.RawMessage(`{"auth_mode":"chatgpt","tokens":{"access_token":"next"}}`))

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("auth.json unreadable after write (write err: %v): %v", writeErr, err)
	}
	var active CodexAuthFile
	if err := json.Unmarshal(body, &active); err != nil {
		t.Fatalf("auth.json is not valid JSON: %v", err)
	}
	if writeErr != nil {
		t.Fatalf("write failed: %v; want the unique temp file to sidestep the stale fixed one", writeErr)
	}
	if active.Tokens == nil || active.Tokens.AccessToken != "next" {
		t.Fatalf("auth.json = %s, want the new credential", body)
	}
	backup, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("missing backup: %v", err)
	}
	if string(backup) != string(previous) {
		t.Fatalf("backup = %s, want the previous auth.json", backup)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if name := entry.Name(); name != "auth.json" && name != "auth.json.bak" && name != "auth.json.tmp" {
			t.Fatalf("leftover file %q after write", name)
		}
	}
}

// A refresh of account A that read ~/.codex/auth.json before the user switched
// to B must not then write A's refreshed tokens over B. The switch lands inside
// the refresh's read-compare-write window: with the write lock it waits for the
// sync and writes last; without it, it finishes first and is overwritten.
func TestRefreshSyncDoesNotOverwriteConcurrentSwitch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	store := CodexStore{Dir: t.TempDir()}
	stale := storedOAuthAccount("a@example.com", "a-old", time.Now().Add(-time.Hour))
	other := storedOAuthAccount("b@example.com", "b", time.Now().Add(time.Hour))
	if err := store.SaveStored(stale); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveStored(other); err != nil {
		t.Fatal(err)
	}
	if err := WriteActiveCodexAuth(stale.Auth); err != nil {
		t.Fatal(err)
	}

	switchDone := make(chan error, 1)
	switchWaiting := make(chan struct{}, 1)
	activeCodexAuthWriteWaiting = func() { switchWaiting <- struct{}{} }
	afterActiveCodexAuthSyncRead = func() {
		afterActiveCodexAuthSyncRead = nil
		go func() {
			_, err := store.SwitchActiveStored(other.Email)
			switchDone <- err
		}()
		// Either the switch is parked on the write lock this sync holds, or
		// it wrote B inside the window and the sync is about to clobber it.
		select {
		case <-switchWaiting:
		case err := <-switchDone:
			switchDone <- err
		}
	}
	defer func() {
		afterActiveCodexAuthSyncRead = nil
		activeCodexAuthWriteWaiting = nil
	}()

	client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return refreshResponse("a-new", "a@example.com", time.Now().Add(time.Hour)), nil
	})}
	if _, _, err := store.RefreshStoredIfExpired(context.Background(), client, stale); err != nil {
		t.Fatal(err)
	}
	if err := <-switchDone; err != nil {
		t.Fatal(err)
	}

	active, ok, err := ReadActiveCodexAuth()
	if err != nil || !ok {
		t.Fatalf("active auth ok = %v, err = %v", ok, err)
	}
	if active.Tokens == nil || active.Tokens.RefreshToken != "b-refresh" {
		got := ""
		if active.Tokens != nil {
			got = active.Tokens.RefreshToken
		}
		t.Fatalf("active refresh token = %q; want b-refresh (the account the user switched to)", got)
	}
}

// A refresh of an account that is not the active one never writes auth.json,
// so it takes no part in the write lock: a lock file it cannot open (a root-run
// `sr` leaves one root-owned 0600; a directory at the path stands in for it
// here) must not fail it, and where there is no ~/.codex it must not create
// one. Only the sync of the active account needs the lock.
func TestRefreshOfAnInactiveAccountTakesNoActiveAuthWriteLock(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, codexHome string, active StoredCodexAccount)
		check func(t *testing.T, codexHome string)
	}{
		{
			name: "an unopenable lock file",
			setup: func(t *testing.T, _ string, active StoredCodexAccount) {
				if err := WriteActiveCodexAuth(active.Auth); err != nil {
					t.Fatal(err)
				}
				// That write left the lock file; replace it with one no open
				// can take.
				if err := os.Remove(activeCodexAuthWriteLockPath()); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(activeCodexAuthWriteLockPath(), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, _ string) {
				auth, ok, err := ReadActiveCodexAuth()
				if err != nil || !ok || auth.Tokens == nil || auth.Tokens.RefreshToken != "b-refresh" {
					t.Fatalf("active auth = %+v ok=%v err=%v; want b's, untouched", auth.Tokens, ok, err)
				}
			},
		},
		{
			name:  "no ~/.codex",
			setup: func(*testing.T, string, StoredCodexAccount) {},
			check: func(t *testing.T, codexHome string) {
				if _, err := os.Stat(codexHome); !os.IsNotExist(err) {
					t.Fatalf("stat %s: %v; a refresh of an inactive account created it", codexHome, err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			codexHome := filepath.Join(home, ".codex")
			t.Setenv("HOME", home)
			t.Setenv("CODEX_HOME", codexHome)
			store := CodexStore{Dir: t.TempDir()}
			stale := storedOAuthAccount("a@example.com", "a-old", time.Now().Add(-time.Hour))
			active := storedOAuthAccount("b@example.com", "b", time.Now().Add(time.Hour))
			if err := store.SaveStored(stale); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, codexHome, active)

			client := &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return refreshResponse("a-new", "a@example.com", time.Now().Add(time.Hour)), nil
			})}
			if _, _, err := store.RefreshStoredIfExpired(context.Background(), client, stale); err != nil {
				t.Fatalf("refresh of an inactive account: %v", err)
			}
			tc.check(t, codexHome)
		})
	}
}
