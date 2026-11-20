package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// writeAssignments seeds a store file directly, the way a long-running daemon
// would have left it, so the prune runs on the next load.
func writeAssignments(t *testing.T, path string, assignments []Assignment) {
	t.Helper()
	data := make(map[string]Assignment, len(assignments))
	for _, assignment := range assignments {
		data[ScopedSessionKey(assignment.AgentType, assignment.SessionID)] = assignment
	}
	body, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// coldHistory is n assignments all newer than a day-old one but outside the
// warm window, so a prune by UpdatedAt alone ranks the day-old one last.
func coldHistory(now time.Time, n int) []Assignment {
	out := make([]Assignment, 0, n)
	for index := 0; index < n; index++ {
		at := now.Add(-3*time.Hour + time.Duration(index)*time.Second)
		out = append(out, Assignment{
			AgentType: "claude", SessionID: "cold-" + strconv.Itoa(index), AccountID: "a",
			CreatedAt: at, UpdatedAt: at,
		})
	}
	return out
}

// PATCHES.md section 20: a warm session moves only when its account is
// evicted. The 512-entry prune is not an eviction; dropping a warm session's
// assignment re-picks it cold. A session this daemon is serving (a Get per
// turn) survives the prune even though its persisted UpdatedAt -- the day it
// was assigned, when the proxy does not Touch -- is the oldest in the store.
func TestPruneKeepsSessionInUseEvenWhenItsUpdatedAtIsOldest(t *testing.T) {
	now := time.Now().UTC()
	assignedYesterday := now.Add(-20 * time.Hour)
	seed := func(t *testing.T) *Store {
		path := filepath.Join(t.TempDir(), "sessions.json")
		writeAssignments(t, path, append(coldHistory(now, MaxRetainedAssignments-1), Assignment{
			AgentType: "claude", SessionID: "warm", AccountID: "b",
			CreatedAt: assignedYesterday, UpdatedAt: assignedYesterday,
		}))
		store, err := NewStore(path)
		if err != nil {
			t.Fatal(err)
		}
		return store
	}

	t.Run("served session survives", func(t *testing.T) {
		store := seed(t)
		if _, ok := store.Get("claude", "warm"); !ok {
			t.Fatal("setup: warm session missing at the cap")
		}
		// A new session is assigned: the store goes over the cap and prunes.
		if _, err := store.Put("claude", "brand-new", "a", ""); err != nil {
			t.Fatal(err)
		}
		if _, ok := store.Get("claude", "warm"); !ok {
			t.Fatal("the size prune dropped a session in active use")
		}
		if got := len(store.All()); got != MaxRetainedAssignments {
			t.Fatalf("store holds %d, want the cap %d (a cold entry pays instead)", got, MaxRetainedAssignments)
		}
	})

	// Calibration: the same store without the lookup prunes the day-old
	// assignment, which proves the lookup is what kept it above.
	t.Run("unseen session is pruned", func(t *testing.T) {
		store := seed(t)
		if _, err := store.Put("claude", "brand-new", "a", ""); err != nil {
			t.Fatal(err)
		}
		for _, assignment := range store.All() {
			if assignment.SessionID == "warm" {
				t.Fatal("calibration: an unseen, day-old assignment survived the prune")
			}
		}
	})
}

// Across processes and restarts the in-memory record is gone, so warmth rides
// on the persisted UpdatedAt: anything updated within WarmAssignmentRetention
// is kept even when that leaves the store over the cap, and a store over the
// cap only because of warm entries is not rewritten on every read.
func TestPruneNeverDropsRecentlyUpdatedAssignments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	now := time.Now().UTC()
	warm := make([]Assignment, 0, MaxRetainedAssignments+20)
	for index := 0; index < MaxRetainedAssignments+20; index++ {
		at := now.Add(-WarmAssignmentRetention + time.Minute + time.Duration(index)*time.Millisecond)
		warm = append(warm, Assignment{
			AgentType: "claude", SessionID: "warm-" + strconv.Itoa(index), AccountID: "a",
			CreatedAt: at, UpdatedAt: at,
		})
	}
	writeAssignments(t, path, warm)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(store.All()); got != MaxRetainedAssignments+20 {
		t.Fatalf("retained %d assignments, want all %d warm ones", got, MaxRetainedAssignments+20)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatal("a load that pruned nothing rewrote the store")
	}
}

// Touch coalesces writes, but the interval must sit inside the warm window or
// a persisted UpdatedAt can lag a session's last turn by more than the window
// the prune protects.
func TestTouchIntervalFitsInsideTheWarmWindow(t *testing.T) {
	if sessionActivityWriteInterval >= warmSessionWindow {
		t.Fatalf("sessionActivityWriteInterval %s must be shorter than the %s warm window", sessionActivityWriteInterval, warmSessionWindow)
	}
}
