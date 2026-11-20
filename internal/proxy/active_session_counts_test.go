package proxy

import (
	"path/filepath"
	"testing"

	"github.com/manaflow-ai/subrouter/session"
)

// The endgame taper regression: the session store's CountByAccount counts
// every sticky assignment ever written (nothing prunes them), so a day-old
// store already exceeds the endgame session cap on every active account and
// the drain mode would never engage. The scheduler must be fed live
// concurrency: assignments joined against the in-flight tracker.
func TestActiveSessionCountsByAccountCountsOnlyInFlightSessions(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, sessionID := range []string{"s1", "s2", "s3", "s4", "s5", "s6"} {
		if _, err := store.Put("claude", sessionID, "acct-a", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Put("claude", "s7", "acct-b", ""); err != nil {
		t.Fatal(err)
	}

	active := NewActiveSessions()
	endS1 := active.Begin("claude", "s1")
	defer endS1()
	endS3 := active.Begin("claude", "s3")
	defer endS3()

	server := Server{Sessions: store, ActiveSessions: active}
	counts := server.activeSessionCountsByAccount()
	if counts["acct-a"] != 2 {
		t.Fatalf("acct-a live count = %d, want 2 (six lifetime assignments, two in flight)", counts["acct-a"])
	}
	if counts["acct-b"] != 0 {
		t.Fatalf("acct-b live count = %d, want 0", counts["acct-b"])
	}

	// Without an in-flight tracker the helper falls back to all-time counts:
	// over-counting keeps endgame drain off, the conservative direction.
	fallback := Server{Sessions: store}
	if got := fallback.activeSessionCountsByAccount()["acct-a"]; got != 6 {
		t.Fatalf("fallback acct-a count = %d, want the all-time 6", got)
	}
}
