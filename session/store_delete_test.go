package session

import (
	"os"
	"path/filepath"
	"testing"
)

// Delete/DeleteMany are the only new write primitives on this store and have no
// other caller, so nothing else in the suite would catch a bug in them.

func newTempStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

func TestDeleteRemovesAssignment(t *testing.T) {
	store := newTempStore(t)
	if _, err := store.Put("claude", "abc", "twelve-alpha", ""); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, ok := store.Get("claude", "abc"); !ok {
		t.Fatal("assignment missing after Put")
	}
	removed, err := store.Delete("claude", "abc")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !removed {
		t.Fatal("Delete reported nothing removed")
	}
	if _, ok := store.Get("claude", "abc"); ok {
		t.Fatal("assignment still present after Delete")
	}
	// Deleting again is not an error, it is just false. Prune runs concurrently
	// with a live daemon and must tolerate a row vanishing underneath it.
	removed, err = store.Delete("claude", "abc")
	if err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if removed {
		t.Fatal("second Delete reported a removal")
	}
}

// A codex assignment is keyed on the session ID truncated at the first colon, so
// a caller must be able to delete it by either spelling. Getting this wrong is
// how a prune silently leaves the row it meant to remove.
func TestDeleteNormalizesCodexSessionKey(t *testing.T) {
	store := newTempStore(t)
	if _, err := store.Put("codex", "0198f2a7:child-1", "seat-l12", ""); err != nil {
		t.Fatalf("Put: %v", err)
	}
	removed, err := store.Delete("codex", "0198f2a7:child-9")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !removed {
		t.Fatal("sibling session ID did not resolve to the same assignment")
	}
	if _, ok := store.Get("codex", "0198f2a7"); ok {
		t.Fatal("base assignment survived")
	}
}

func TestDeleteManyIsOneRewrite(t *testing.T) {
	store := newTempStore(t)
	for _, id := range []string{"a", "b", "c"} {
		if _, err := store.Put("claude", id, "seven-alpha", ""); err != nil {
			t.Fatalf("Put %s: %v", id, err)
		}
	}
	removed, err := store.DeleteMany([]Ref{
		{AgentType: "claude", SessionID: "a"},
		{AgentType: "claude", SessionID: "c"},
		{AgentType: "claude", SessionID: "missing"},
	})
	if err != nil {
		t.Fatalf("DeleteMany: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2 (the absent key must not count)", removed)
	}
	if _, ok := store.Get("claude", "b"); !ok {
		t.Fatal("unlisted assignment was removed")
	}
	if all := store.All(); len(all) != 1 {
		t.Fatalf("store holds %d assignments, want 1", len(all))
	}
}

// A prune that matches nothing must not touch the file at all: the daemon
// re-reads sessions.json on every proxied request, and a pointless tmp+rename
// is a pointless race.
func TestDeleteManyNoMatchDoesNotRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	store, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := store.Put("claude", "keep", "main", ""); err != nil {
		t.Fatalf("Put: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	removed, err := store.DeleteMany([]Ref{{AgentType: "claude", SessionID: "nope"}})
	if err != nil {
		t.Fatalf("DeleteMany: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("no-op DeleteMany rewrote the store")
	}
}

// The deletion must reach disk, not just the in-memory map: another process
// (the running daemon) is the reader that matters.
func TestDeletePersistsToDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	store, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := store.Put("claude", "gone", "main", ""); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := store.Delete("claude", "gone"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	reopened, err := NewStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, ok := reopened.Get("claude", "gone"); ok {
		t.Fatal("assignment came back after reopening the store")
	}
}
