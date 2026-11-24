package proxy

import (
	"net/http"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
)

// PATCHES §31, R3 fix 7 (F-F): a verdict body has a maximum age. The admin
// cache keeps a peer's last good usage body for as long as its usage endpoint
// fails, and the endpoint can fail while forwards still work. Ranked on that
// body, an account in its burndown window stayed first in every ranking --
// its verdict said burndown, and its weekly deadline, re-counted from each
// decision's clock, never arrived -- long after its week had rolled.

// ageDecisionBody makes the cached body as old as age, as a run of failed
// usage fetches leaves it (store keeps the ORIGINAL timestamp), with the last
// attempt long enough ago that the next decision tries again -- and fails.
func ageDecisionBody(t *testing.T, pool *RemotePool, age time.Duration) {
	t.Helper()
	pool.admin.mu.Lock()
	defer pool.admin.mu.Unlock()
	entry, ok := pool.admin.entries[remotePoolUsagePath]
	if !ok || len(entry.body) == 0 {
		t.Fatal("calibration: no cached body to age")
	}
	entry.at = time.Now().Add(-age)
	entry.tried = time.Now().Add(-age)
	pool.admin.entries[remotePoolUsagePath] = entry
}

func TestAStaleVerdictBodyStopsRanking(t *testing.T) {
	for _, tc := range []struct {
		name string
		roll time.Duration // p's weekly roll, from when its body was read
		age  time.Duration // how old the body is at the next decision
	}{
		// Inside its burndown window when read, and the body is past the
		// peer's own last-good horizon.
		{"a body past the max age", 16 * time.Hour, remotePoolDecisionBodyMaxAge + time.Minute},
		// A young body, but p's week has rolled since it was read: its
		// burndown verdict describes a week that is over.
		{"a body whose week has rolled", 90 * time.Second, 3 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := newVerdictPeer(t, accounts.ProviderClaude,
				verdictPeerAccount{Score: crossPoolClaudeScore("p", 0.3, tc.roll)})
			laptop := newCrossPoolLaptop(t, incidentLocal(), peer.pool())
			if status, served := laptop.turn(t, "s1"); status != http.StatusOK || served != "p" {
				t.Fatalf("calibration: status %d served by %q; p's burndown must win while its body is fresh", status, served)
			}
			peer.failUsage.Store(true)
			ageDecisionBody(t, laptop.server.RemotePool, tc.age)

			status, served := laptop.turn(t, "s2")
			if status != http.StatusOK || (served != "fourteen" && served != "fifteen") {
				t.Fatalf("status %d served by %q; p's verdict is %v old (roll %v after it) and must not rank", status, served, tc.age, tc.roll)
			}
			if turns := peer.seen(); len(turns) != 1 {
				t.Fatalf("peer turns %+v; only the first session, on the fresh body", turns)
			}
			// Calibration: the decision really did run on the old body, its
			// refetch having failed.
			if got := laptop.server.RemotePool.cachedUsage(); got.Reachable {
				t.Fatalf("calibration: the usage fetch should have failed (%+v)", got)
			}
		})
	}
}
