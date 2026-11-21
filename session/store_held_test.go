package session

import (
	"path/filepath"
	"testing"
	"time"
)

// A stranded chat's chain of held turns (PATCHES §67) is kept with its
// assignment, so the next process carries it on. A new chain is written at
// once and a later held attempt at most once a minute; an answered turn
// ends it, and so does EndHeld. Every write of the assignment keeps it, and
// an unlinked session takes it with it.
func TestHeldChainIsPersistedCoalescedAndEnded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	const gap = 10 * time.Minute
	at := time.Date(2026, 11, 19, 12, 46, 42, 0, time.UTC)
	if _, ok, err := store.NoteHeld("claude", "unpinned", at, gap); err != nil || ok {
		t.Fatalf("a held attempt of a session with no assignment: ok = %v, err %v; want nothing kept", ok, err)
	}
	if _, ok := store.Get("claude", "unpinned"); ok {
		t.Fatal("a held attempt created an assignment; only placement does")
	}
	if _, err := store.Put("claude", "s1", "owner", ""); err != nil {
		t.Fatal(err)
	}
	stored := func(what string) HeldChain {
		t.Helper()
		reopened, err := NewStore(path)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := reopened.Get("claude", "s1")
		if !ok {
			t.Fatalf("%s: no assignment", what)
		}
		return got.Held
	}
	held := func(now time.Time, want HeldChain, what string) {
		t.Helper()
		got, ok, err := store.NoteHeld("claude", "s1", now, gap)
		if err != nil || !ok {
			t.Fatalf("%s: ok = %v, err %v", what, ok, err)
		}
		if !got.Held.Since.Equal(want.Since) || !got.Held.Last.Equal(want.Last) {
			t.Fatalf("%s: the chain is %+v, want %+v", what, got.Held, want)
		}
	}
	same := func(got, want HeldChain) bool {
		return got.Since.Equal(want.Since) && got.Last.Equal(want.Last)
	}

	held(at, HeldChain{at, at}, "the first held turn")
	if got := stored("the first held turn"); !same(got, HeldChain{at, at}) {
		t.Fatalf("the first held turn: stored %+v, want it written at once", got)
	}
	held(at.Add(30*time.Second), HeldChain{at, at.Add(30 * time.Second)}, "an attempt half a minute later")
	if got := stored("an attempt half a minute later"); !same(got, HeldChain{at, at}) {
		t.Fatalf("an attempt half a minute later: stored %+v, want the first kept until a minute has passed", got)
	}
	minute := at.Add(time.Minute)
	held(minute, HeldChain{at, minute}, "an attempt a minute later")
	if got := stored("an attempt a minute later"); !same(got, HeldChain{at, minute}) {
		t.Fatalf("an attempt a minute later: stored %+v, want its time written", got)
	}
	held(minute.Add(gap-time.Second), HeldChain{at, minute.Add(gap - time.Second)}, "an attempt a second inside the gap")
	fresh := minute.Add(gap - time.Second).Add(gap)
	held(fresh, HeldChain{fresh, fresh}, "an attempt after a gap")
	if got := stored("an attempt after a gap"); !same(got, HeldChain{fresh, fresh}) {
		t.Fatalf("an attempt after a gap: stored %+v, want the new chain written at once", got)
	}

	all := func(Assignment) bool { return true }
	for _, write := range []struct {
		name string
		do   func() error
	}{
		{"Touch", func() error { _, _, err := store.Touch("claude", "s1"); return err }},
		{"Put on another account", func() error { _, err := store.Put("claude", "s1", "other", ""); return err }},
		{"CompareAndPut", func() error { _, _, err := store.CompareAndPut("claude", "s1", "other", "owner", ""); return err }},
		{"PutRefugee", func() error {
			_, _, err := store.PutRefugee("claude", "s1", "third", Refugee{Origin: "box-a/acct-a", PlacedAt: fresh}, 4, all)
			return err
		}},
	} {
		if err := write.do(); err != nil {
			t.Fatal(err)
		}
		if got := stored(write.name); !same(got, HeldChain{fresh, fresh}) {
			t.Fatalf("after %s: the chain is %+v, want %+v kept", write.name, got, HeldChain{fresh, fresh})
		}
	}

	answered := fresh.Add(time.Second)
	for _, answer := range []struct {
		name string
		at   time.Time
	}{
		{"an answered turn", answered},
		{"a turn answered within a minute of the last", answered.Add(20 * time.Second)},
	} {
		if !answer.at.Equal(answered) {
			heldAt := answered.Add(10 * time.Second)
			held(heldAt, HeldChain{heldAt, heldAt}, "a held turn after the answer")
		}
		if err := store.NoteAnswered("claude", "s1", answer.at); err != nil {
			t.Fatal(err)
		}
		if got := stored(answer.name); !got.IsZero() {
			t.Fatalf("%s: the chain is %+v, want it ended", answer.name, got)
		}
	}
	again := answered.Add(time.Minute)
	held(again, HeldChain{again, again}, "a held turn after the answers")
	if err := store.EndHeld("claude", "s1"); err != nil {
		t.Fatal(err)
	}
	if got := stored("EndHeld"); !got.IsZero() {
		t.Fatalf("EndHeld: the chain is %+v, want it ended", got)
	}
	held(again.Add(time.Second), HeldChain{again.Add(time.Second), again.Add(time.Second)}, "a held turn after EndHeld")
	if _, err := store.Delete("claude", "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "s1", "owner", ""); err != nil {
		t.Fatal(err)
	}
	if got := stored("a placement after the session was unlinked"); !got.IsZero() {
		t.Fatalf("a placement after the session was unlinked: the chain is %+v, want none", got)
	}
}
