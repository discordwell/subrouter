package session

import (
	"path/filepath"
	"testing"
	"time"
)

// A chat that gave up waiting for its box keeps a waiting record (PATCHES
// §67) in the file, so the next process resumes it. A turn answered ends
// it, even within a minute of the last answered one, when the answered time
// itself is not written; so do EndWait and any new placement. Touch and a
// held turn keep it.
func TestAWaitEndsAtAnAnsweredTurnOrAPlacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	answered := time.Date(2026, 11, 19, 12, 46, 42, 0, time.UTC)
	home := Waiting{Peer: "box-a", Account: "acct-a", Since: answered.Add(time.Hour)}
	if _, err := store.Put("claude", "s1", "owner", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.NoteAnswered("claude", "s1", answered); err != nil {
		t.Fatal(err)
	}
	waiting := func(what string) *Waiting {
		t.Helper()
		reopened, err := NewStore(path)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := reopened.Get("claude", "s1")
		if !ok {
			t.Fatalf("%s: no assignment", what)
		}
		return got.Waiting
	}
	wait := func(what string) {
		t.Helper()
		if err := store.Wait("claude", "s1", home); err != nil {
			t.Fatal(err)
		}
		if got := waiting(what); got == nil || got.Peer != home.Peer || got.Account != home.Account || !got.Since.Equal(home.Since) {
			t.Fatalf("%s: waiting = %+v, want %+v", what, got, home)
		}
	}
	wait("a wait")
	if _, _, err := store.Touch("claude", "s1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.NoteHeld("claude", "s1", answered.Add(2*time.Hour), time.Minute); err != nil {
		t.Fatal(err)
	}
	if got := waiting("Touch and a held turn"); got == nil {
		t.Fatal("Touch and a held turn ended the wait")
	}
	wait("a wait again, which ends the chain of held turns")
	if err := store.NoteAnswered("claude", "s1", answered.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := waiting("a turn answered half a minute after the last"); got != nil {
		t.Fatalf("a turn answered half a minute after the last: waiting = %+v, want it ended", got)
	}
	all := func(Assignment) bool { return true }
	for _, end := range []struct {
		name string
		do   func() error
	}{
		{"EndWait", func() error { return store.EndWait("claude", "s1") }},
		{"Put", func() error { _, err := store.Put("claude", "s1", "other", ""); return err }},
		{"PutRefugee", func() error {
			_, _, err := store.PutRefugee("claude", "s1", "third", Refugee{Origin: "box-a/acct-a", PlacedAt: answered}, 4, all)
			return err
		}},
	} {
		wait("a wait before " + end.name)
		if err := end.do(); err != nil {
			t.Fatal(err)
		}
		if got := waiting(end.name); got != nil {
			t.Fatalf("after %s: waiting = %+v, want it ended", end.name, got)
		}
	}
}
