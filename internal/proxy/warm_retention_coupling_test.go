package proxy

import (
	"testing"

	"github.com/discordwell/subrouter/session"
)

// The session store's size prune spares every assignment younger than
// session.WarmAssignmentRetention. A warm session -- of either provider since
// PATCHES section 21 -- is one whose last turn is inside warmSessionIdleTTL, so
// the retention must cover that whole window: otherwise a warm session could
// lose its assignment to the size cap and be re-picked cold, a move that is
// not an eviction.
func TestSessionStoreRetentionCoversTheWarmWindow(t *testing.T) {
	if session.WarmAssignmentRetention < warmSessionIdleTTL {
		t.Fatalf("session.WarmAssignmentRetention = %s, shorter than warmSessionIdleTTL = %s: the store could prune a warm session",
			session.WarmAssignmentRetention, warmSessionIdleTTL)
	}
}
