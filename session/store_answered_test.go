package session

import (
	"path/filepath"
	"testing"
	"time"
)

// A session's answered time (PATCHES §67) is the clock a stranded chat gives
// up on. Another process reads it from the file, so it is written there, at
// most once a minute per session; it belongs to the session, so every write
// of the assignment keeps it; and an unlinked session takes it with it.
func TestAnsweredAtIsPersistedCoalescedAndKeptByEveryWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 19, 12, 46, 42, 0, time.UTC)
	if err := store.NoteAnswered("claude", "unpinned", at); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get("claude", "unpinned"); ok {
		t.Fatal("an answer created an assignment; only placement does")
	}
	if _, err := store.Put("claude", "s1", "owner", ""); err != nil {
		t.Fatal(err)
	}
	answered := func(what string) time.Time {
		t.Helper()
		reopened, err := NewStore(path)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := reopened.Get("claude", "s1")
		if !ok {
			t.Fatalf("%s: no assignment", what)
		}
		return got.AnsweredAt
	}
	if err := store.NoteAnswered("claude", "s1", at); err != nil {
		t.Fatal(err)
	}
	if got := answered("the first answer"); !got.Equal(at) {
		t.Fatalf("the first answer: AnsweredAt = %v, want %v", got, at)
	}
	if err := store.NoteAnswered("claude", "s1", at.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := answered("an answer half a minute later"); !got.Equal(at) {
		t.Fatalf("an answer half a minute later: AnsweredAt = %v, want %v kept until a minute has passed", got, at)
	}
	later := at.Add(time.Minute)
	if err := store.NoteAnswered("claude", "s1", later); err != nil {
		t.Fatal(err)
	}
	if err := store.NoteAnswered("claude", "s1", at.Add(2*time.Minute).Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := answered("an answer a minute later, then an older one"); !got.Equal(later) {
		t.Fatalf("AnsweredAt = %v, want %v: an older answer never moves it back", got, later)
	}
	all := func(Assignment) bool { return true }
	for _, write := range []struct {
		name string
		do   func() error
	}{
		{"Put on another account", func() error { _, err := store.Put("claude", "s1", "other", ""); return err }},
		{"CompareAndPut", func() error { _, _, err := store.CompareAndPut("claude", "s1", "other", "owner", ""); return err }},
		{"PutRefugee", func() error {
			_, _, err := store.PutRefugee("claude", "s1", "third", Refugee{Origin: "box-a/acct-a", PlacedAt: later}, 4, all)
			return err
		}},
	} {
		if err := write.do(); err != nil {
			t.Fatal(err)
		}
		if got := answered(write.name); !got.Equal(later) {
			t.Fatalf("after %s: AnsweredAt = %v, want %v kept", write.name, got, later)
		}
	}
	if _, err := store.Delete("claude", "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "s1", "owner", ""); err != nil {
		t.Fatal(err)
	}
	if got := answered("a placement after the session was unlinked"); !got.IsZero() {
		t.Fatalf("a placement after the session was unlinked: AnsweredAt = %v, want none", got)
	}
}
