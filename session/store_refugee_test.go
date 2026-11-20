package session

import (
	"path/filepath"
	"testing"
	"time"
)

// A refugee mark (PATCHES §67) stays with its assignment while the session
// keeps the account, and goes when it moves.
func TestARefugeeMarkStaysOnlyWhileTheSessionKeepsItsAccount(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	mark := Refugee{Origin: "box-a/acct-a", PlacedAt: time.Now().UTC().Truncate(time.Second)}
	all := func(Assignment) bool { return true }
	if _, placed, err := store.PutRefugee("claude", "s1", "owner", mark, 4, all); err != nil || !placed {
		t.Fatalf("placed=%v err=%v", placed, err)
	}
	if got, err := store.Put("claude", "s1", "owner", ""); err != nil || got.Refugee == nil || *got.Refugee != mark {
		t.Fatalf("Put on the same account = %+v (%v), want the mark kept", got, err)
	}
	if got, swapped, err := store.CompareAndPut("claude", "s1", "owner", "owner", ""); err != nil || !swapped || got.Refugee == nil {
		t.Fatalf("CompareAndPut on the same account = %+v swapped=%v (%v), want the mark kept", got, swapped, err)
	}
	if got, err := store.Put("claude", "s1", "other", ""); err != nil || got.Refugee != nil {
		t.Fatalf("Put on another account = %+v (%v), want the mark gone", got, err)
	}
}

// An account's refugee count is the marked assignments the caller counts
// (warm, and not held elsewhere): a placement takes a slot only while fewer
// than slots count, and the session being placed never counts against
// itself.
func TestPutRefugeeCountsOnlyWhatTheCallerCounts(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	mark := Refugee{Origin: "box-a/acct-a", PlacedAt: time.Now().UTC()}
	all := func(Assignment) bool { return true }
	for _, id := range []string{"warm1", "warm2", "cold1", "cold2"} {
		if _, placed, err := store.PutRefugee("claude", id, "owner", mark, 99, all); err != nil || !placed {
			t.Fatalf("%s: placed=%v err=%v", id, placed, err)
		}
	}
	warm := func(a Assignment) bool { return a.SessionID != "cold1" && a.SessionID != "cold2" }
	if held, placed, err := store.PutRefugee("claude", "next", "owner", mark, 3, warm); err != nil || !placed || held != 3 {
		t.Fatalf("held=%d placed=%v err=%v, want the third slot taken", held, placed, err)
	}
	if held, placed, err := store.PutRefugee("claude", "last", "owner", mark, 3, warm); err != nil || placed || held != 3 {
		t.Fatalf("held=%d placed=%v err=%v, want owner full", held, placed, err)
	}
	if held, placed, err := store.PutRefugee("claude", "next", "owner", mark, 3, warm); err != nil || !placed || held != 3 {
		t.Fatalf("held=%d placed=%v err=%v, want a session already placed to keep its own slot", held, placed, err)
	}
}
