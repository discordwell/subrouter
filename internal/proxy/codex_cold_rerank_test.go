package proxy

import (
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
)

// Owner rule P9 for Codex: a session idle past warmSessionIdleTTL has no cache
// left to protect, so its next turn is a FULL re-rank across every pool --
// the one ranking (placeAcrossPools), exactly as for Claude. Codex's old cold
// row (retentionFloorGated alone) kept such a session on ANY account that
// still passed the new-session floor, so a cold Codex conversation never met
// a better local account, nor a peer account, unless its own had gone below
// the floor. A delegated Codex session whose peer hold lapsed lands on the
// same row (its next turn is decided by the local sticky path), so these
// cover it too.

// healthyButOutrankedCodexScore passes every eligibility gate -- a cold
// session under the old row stayed on it -- but ranks below the fixture's
// "cool" (less of the week left, and further from its roll) and below
// healthyPeer (which rolls in 30h with the whole week left).
func healthyButOutrankedCodexScore() selectacct.Score {
	return selectacct.Score{
		Headroom: 0.5, ShortHeadroom: 0.9,
		WeeklyHeadroom: 0.5, WeeklyRemaining: 0.5, WeeklyResetAt: time.Now().Add(150 * time.Hour),
	}
}

func TestOutrankedCodexFixtureStillPassesTheRetentionFloor(t *testing.T) {
	// Calibration: the move below must be the ranking's doing, not the
	// retention floor's -- the old row would have moved a below-floor account.
	server, _, _ := hotCodexFixture(t, healthyButOutrankedCodexScore())
	scheduler := server.scheduler().WithReserves(server.AccountReserves)
	if !scheduler.UsableForStickySession(accounts.ProviderCodex, "hot") {
		t.Fatal("hot fails the retention floor; the tests below would not isolate P9")
	}
	placement, err := server.placeForSession(scheduler, server.Accounts, nil, "cold")
	if err != nil || placement.Account.ID != "cool" {
		t.Fatalf("calibration: best local = %q (%v), want cool to outrank hot", placement.Account.ID, err)
	}
}

func TestColdCodexSessionOnAHealthyAccountReRanksOntoABetterLocalOne(t *testing.T) {
	server, active, store := hotCodexFixture(t, healthyButOutrankedCodexScore())
	markCodexIdle(active, "s1", warmSessionIdleTTL+time.Minute)
	assertCodexSessionMovesToCool(t, server, store, "cold, own account healthy but outranked")
}

func TestWarmCodexSessionOnAnOutrankedAccountStillStays(t *testing.T) {
	// The other half of P9: only a stale cache re-ranks.
	server, active, store := hotCodexFixture(t, healthyButOutrankedCodexScore())
	markCodexIdle(active, "s1", warmSessionIdleTTL-time.Minute)
	assertCodexSessionStays(t, server, store, "warm, own account outranked")
}

func TestColdCodexSessionOnTheBestLocalAccountIsRankedAgainstThePeers(t *testing.T) {
	// Only "hot" locally (cool dropped), so the local ranking alone keeps the
	// session; the peer's healthier account must still win the cold turn.
	server, active, _ := hotCodexFixture(t, healthyButOutrankedCodexScore())
	server.Accounts = server.Accounts[:1]
	markCodexIdle(active, "s1", 2*time.Hour)
	account, _, offered := offeredToHealthyPeer(t, server, accounts.ProviderCodex, "codex", "s1", codexResponsesRequest())
	if !offered {
		t.Fatalf("a cold Codex session stayed on %q without being ranked against the peer's better account", account.ID)
	}
}
