package proxy

import (
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
)

// The pre-§31 two-candidate decider ranked the local pool against the peer by
// handing two PLACEHOLDER scores to a two-candidate scheduler. NewScheduler keys a score by
// ScoreKey(Provider, AccountID), and the candidates it is asked to Pick carry
// no provider -- so a score that keeps its Provider while its AccountID is
// swapped for a placeholder is filed under a key the lookup never uses.
// Scheduler.score then substitutes its optimistic default for the missing
// entry: Headroom 1, ShortHeadroom 1, and NO weekly pace. The comparator's
// first rate clause is "has a weekly pace and the other has none", so the peer
// took that clause outright however good the local pool was.
//
// One-sided rather than a wash because the two halves are asymmetric: the peer
// score is built by remotePoolAccountUsage.score, which never sets Provider, so
// the peer hit its entry while local missed. Every real local score carries a
// Provider -- scoreAccounts stamps it.
//
// The existing decision table never caught this: its fixture builds scores with
// no Provider at all, and it has no case where the local pool is the better one.
// This uses the shape production actually produces, with local at 4x the peer's
// pace -- a margin no working comparator could rule against -- and now runs it
// through the one ranking that replaced that decider (placeAcrossPools files
// the peer under the request's provider for the same reason).
func TestPeerDecisionReadsALocalScoreThatCarriesItsProvider(t *testing.T) {
	now := time.Now()
	peer := selectacct.Score{
		AccountID: "peer-b", Remote: true, Fresh: true,
		Headroom: 0.2, ShortHeadroom: 0.2,
		WeeklyHeadroom: 0.2, WeeklyResetAt: now.Add(100000 * time.Second),
	}
	local := selectacct.Score{
		AccountID: "local-a",
		// The field the bug turned on. Nothing else here is unusual.
		Provider:       accounts.ProviderClaude,
		Headroom:       0.8,
		ShortHeadroom:  0.8,
		WeeklyHeadroom: 0.8,
		WeeklyResetAt:  now.Add(100000 * time.Second),
		Fresh:          true,
	}

	got := placeLocalAgainstOnePeer(t, local, peer, true)

	if got.Peer != nil {
		t.Fatalf("a local pool at 4x the peer's pace spilled anyway (%s)", got.Clause)
	}
}
