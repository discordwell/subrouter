package proxy

import (
	"net/http"
	"testing"
	"time"

	"github.com/discordwell/subrouter/selectacct"
)

// End to end with SEVERAL peers (PATCHES §31, owner rule P7): the laptop ranks
// its own accounts and every account of every configured peer, each a real
// measured box-role Server (crosspool_e2e_test.go). These need Server.
// RemotePeers, which origin/main does not have, so they were watched failing by
// mutation instead: peers() narrowed to the first peer turns each one red.

// e2eTwoPeers configures the laptop with both boxes, named, in that order, and
// reads both usage bodies.
func e2eTwoPeers(t *testing.T, laptop *e2eLaptop, a, b *e2eBox, prime bool) (*RemotePool, *RemotePool) {
	t.Helper()
	aPool, bPool := a.newPool().WithName("box-a"), b.newPool().WithName("box-b")
	laptop.server.RemotePeers = []*RemotePool{aPool, bPool}
	if prime {
		e2ePrime(t, aPool)
		e2ePrime(t, bPool)
	}
	return aPool, bPool
}

// P6 + P7: the best account across everything the laptop can see is on box A,
// and A refuses it at request time (it took a session since it published).
// One recompute over local and every OTHER account decides by rank: box B's
// account when it outranks local, local when it does not.
func TestE2ETwoPeersTheBestRefusesAndTheRankDecides(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		bUsage map[string]string
		want   string
	}{
		{"box B outranks local", map[string]string{"b2d": e2eUsage(now, 0, 4*time.Hour, 0, 48*time.Hour)}, "b2d"},
		{"local outranks box B", map[string]string{"b3d": e2eUsage(now, 0, 4*time.Hour, 0, 72*time.Hour)}, "loc5d"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newE2EBox(t, map[string]string{"a-burn": e2eUsage(now, 10, 150*time.Minute, 96, 16*time.Hour)})
			b := newE2EBox(t, tc.bUsage)
			laptop := newE2ELaptop(t, e2eFiveDays(), nil)
			aPool, bPool := e2eTwoPeers(t, laptop, a, b, true)
			a.calibrate(t, map[string]string{"a-burn": selectacct.VerdictBurndown})
			defer a.live(t, "a-burn")()

			got := laptop.turn(t, "s1")
			if got.Status != http.StatusOK || got.ServedBy != tc.want {
				t.Fatalf("status %d served by %q; want %s", got.Status, got.ServedBy, tc.want)
			}
			if turns := a.seen(); len(turns) != 1 || turns[0].Target != "a-burn" || turns[0].Refused != selectacct.VerdictAtCap {
				t.Fatalf("box A turns %+v; want a-burn targeted once and refused at_cap", turns)
			}
			if _, held := e2eHold(aPool, "s1"); held {
				t.Fatal("a hold on the refusing peer")
			}
			bTurns := b.seen()
			if tc.want == "loc5d" {
				if len(bTurns) != 0 {
					t.Fatalf("box B turns %+v; local outranks it", bTurns)
				}
				return
			}
			if len(bTurns) != 1 || bTurns[0].Target != tc.want || got.Peer != "box-b" {
				t.Fatalf("box B turns %+v, peer %q; want %s on box-b, targeted", bTurns, got.Peer, tc.want)
			}
			if account, held := e2eHold(bPool, "s1"); !held || account != tc.want {
				t.Fatalf("hold on box B = %q/%v; want %s", account, held, tc.want)
			}
		})
	}
}

// D0: one peer that is down, slow or gone mid-turn costs only its own
// accounts; the decision against the other is neither blocked nor delayed.
func TestE2EOnePeerUnreachableDoesNotBlockTheOther(t *testing.T) {
	now := time.Now()
	aUsage := map[string]string{"a-burn": e2eUsage(now, 10, 150*time.Minute, 96, 16*time.Hour)}
	bUsage := map[string]string{"b2d": e2eUsage(now, 0, 4*time.Hour, 0, 48*time.Hour)}

	t.Run("box A is down before the first decision", func(t *testing.T) {
		a, b := newE2EBox(t, aUsage), newE2EBox(t, bUsage)
		laptop := newE2ELaptop(t, e2eFiveDays(), nil)
		_, bPool := e2eTwoPeers(t, laptop, a, b, false)
		a.srv.Close()
		e2ePrime(t, bPool)
		started := time.Now()
		got := laptop.turn(t, "s1")
		if got.Status != http.StatusOK || got.ServedBy != "b2d" || got.Peer != "box-b" {
			t.Fatalf("status %d served by %q on %q; want b2d on box-b", got.Status, got.ServedBy, got.Peer)
		}
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Fatalf("the turn took %v with one peer down", elapsed)
		}
	})
	t.Run("box A's usage endpoint hangs", func(t *testing.T) {
		a, b := newE2EBox(t, aUsage), newE2EBox(t, bUsage)
		a.usageDelay = 3 * time.Second
		laptop := newE2ELaptop(t, e2eFiveDays(), nil)
		_, bPool := e2eTwoPeers(t, laptop, a, b, false)
		e2ePrime(t, bPool)
		started := time.Now()
		got := laptop.turn(t, "s1")
		if got.Status != http.StatusOK || got.ServedBy != "b2d" {
			t.Fatalf("status %d served by %q; want b2d", got.Status, got.ServedBy)
		}
		// remotePoolDecisionFetchBudget (250 ms) plus the turn itself.
		if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
			t.Fatalf("the decision waited %v on a blackholed peer", elapsed)
		}
		if turns := a.seen(); len(turns) != 0 {
			t.Fatalf("box A turns %+v; it was never measured", turns)
		}
	})
	t.Run("box A goes down after publishing its best account", func(t *testing.T) {
		a, b := newE2EBox(t, aUsage), newE2EBox(t, bUsage)
		laptop := newE2ELaptop(t, e2eFiveDays(), nil)
		aPool, bPool := e2eTwoPeers(t, laptop, a, b, true)
		a.srv.Close() // a-burn (burndown) is still the best in the laptop's view
		got := laptop.turn(t, "s1")
		if got.Status != http.StatusOK || got.ServedBy != "b2d" || got.Peer != "box-b" {
			t.Fatalf("status %d served by %q on %q; want the recompute to land b2d on box-b", got.Status, got.ServedBy, got.Peer)
		}
		if account, held := e2eHold(bPool, "s1"); !held || account != "b2d" {
			t.Fatalf("hold on box B = %q/%v; want b2d", account, held)
		}
		if _, held := e2eHold(aPool, "s1"); held {
			t.Fatal("a hold on the peer that never answered")
		}
	})
}

// P8 with every pool: a spent window on the local pin is a FORCED switch, and
// the recompute is one ranking over local and EVERY peer. fourteen (100%/5d,
// 280) wins the first turn; once its window is spent, the local failover
// candidate fifteen (50%/6.25d, 112) is weighed against box A's a4d (100%/4d,
// 175) and box B's b3d (100%/3d, 233): b3d, on the second peer, wins -- and
// the session is held there.
func TestE2EForcedSwitchRecomputesAcrossLocalAndEveryPeer(t *testing.T) {
	now := time.Now()
	a := newE2EBox(t, map[string]string{"a4d": e2eUsage(now, 0, 4*time.Hour, 0, 96*time.Hour)})
	b := newE2EBox(t, map[string]string{"b3d": e2eUsage(now, 0, 4*time.Hour, 0, 72*time.Hour)})
	laptop := newE2ELaptop(t, map[string]string{
		"fourteen": e2eUsage(now, 0, 4*time.Hour, 0, 120*time.Hour),
		"fifteen":  e2eUsage(now, 0, 4*time.Hour, 50, 150*time.Hour),
	}, nil)
	_, bPool := e2eTwoPeers(t, laptop, a, b, true)
	if got := laptop.turn(t, "s1"); got.ServedBy != "fourteen" {
		t.Fatalf("turn 1 served by %q; want fourteen", got.ServedBy)
	}
	laptop.upstream.setAnswer(func(account string, w http.ResponseWriter) bool {
		if account == "fourteen" {
			e2eRejected(w)
			return true
		}
		return false
	})
	// Served through the transport-level peer stage, whose response has
	// always carried the local stamps (X-Subrouter-Pool: local, no
	// X-Subrouter-Peer): which peer served it is read off box B and the hold.
	got := laptop.turn(t, "s1")
	if got.Status != http.StatusOK || got.ServedBy != "b3d" || len(b.seen()) != 1 {
		t.Fatalf("turn 2: status %d served by %q, box B turns %+v; want b3d on box-b", got.Status, got.ServedBy, b.seen())
	}
	if account, held := e2eHold(bPool, "s1"); !held || account != "b3d" {
		t.Fatalf("hold = %q/%v; want b3d on box-b", account, held)
	}
	if third := laptop.turn(t, "s1"); third.ServedBy != "b3d" {
		t.Fatalf("turn 3 served by %q; want b3d (held)", third.ServedBy)
	}
	if turns := a.seen(); len(turns) != 0 {
		t.Fatalf("box A turns %+v; it was outranked", turns)
	}
	for _, turn := range b.seen() {
		if turn.Target != "b3d" {
			t.Fatalf("box B turns %+v; want every one targeted at b3d", b.seen())
		}
	}
}

// D0: SUBROUTER_REMOTE_POOL alone (cmd/subrouter builds Server.RemotePool and
// no RemotePeers -- TestRemotePeersFileOverridesSinglePeerEnv) is exactly one
// peer, named by its host:port, and it is the peer every path uses.
// CHARACTERIZATION of the compatibility rule.
func TestE2ESinglePeerEnvFormIsExactlyOnePeer(t *testing.T) {
	box := newE2EBox(t, map[string]string{"b2d": e2eUsage(time.Now(), 0, 4*time.Hour, 0, 48*time.Hour)})
	pool := box.newPool()
	laptop := newE2ELaptop(t, e2eFiveDays(), pool)
	if peers := laptop.server.peers(); len(peers) != 1 || peers[0] != pool || laptop.server.firstPeer() != pool {
		t.Fatalf("peers() = %v; want exactly the one RemotePool", peers)
	}
	if name, host := pool.name(), mustURL(t, box.srv.URL).Host; name != host {
		t.Fatalf("peer name %q; want its host %q, as before", name, host)
	}
}
