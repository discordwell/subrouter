package proxy

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/discordwell/subrouter/selectacct"
)

// PATCHES §31, R3 fix 6 (F-E): pace symmetry between sweeps. The peer
// publishes its MEASURED numbers; a local account sinks by
// LiveDebitPerRequest for every request routed to it until the next usage
// refresh, and a peer account must sink the same way for the targeted turns
// this daemon sent it until the peer's next body -- otherwise, between two
// sweeps, the ranking keeps sending new sessions to a peer account that looks
// exactly as roomy after the tenth as before the first.
//
// p starts just ahead of local (0.64 of its week over 60h against loc's 0.5
// over 100h with the 2x); each targeted session takes 0.02 off its weekly
// headroom, so after a few the local account outranks it again -- with the
// peer's body never re-read in between -- and from there the two alternate,
// each debited for what it takes.
func TestAPeerAccountIsDebitedForTheSessionsSentToItBetweenBodies(t *testing.T) {
	box := newContractBox(t, boxAccount{Score: crossPoolClaudeScore("p", 0.64, 60*time.Hour)})
	laptop := newCrossPoolLaptop(t, []selectacct.Score{crossPoolClaudeScore("loc", 0.5, 100*time.Hour)}, box.pool)
	e2ePrime(t, box.pool)
	bodyAt := box.pool.cachedUsage().At

	var served []string
	for i := 1; i <= 8; i++ {
		status, servedBy := laptop.turn(t, "new-"+strconv.Itoa(i))
		if status != http.StatusOK {
			t.Fatalf("session %d: status %d", i, status)
		}
		served = append(served, servedBy)
	}
	if got := box.pool.cachedUsage().At; !got.Equal(bodyAt) {
		t.Fatalf("calibration: the peer's body was re-read (%v -> %v); this test is about the gap between bodies", bodyAt, got)
	}
	if served[0] != "p" {
		t.Fatalf("calibration: session 1 served by %q; p must start ahead of loc (served %v)", served[0], served)
	}
	toPeer := 0
	for _, s := range served {
		if s == "p" {
			toPeer++
		}
	}
	if toPeer == len(served) {
		t.Fatalf("all %d new sessions went to p with no re-read in between (%v): a peer account is never debited for what this daemon sends it", len(served), served)
	}
	// Both sides are debited alike, so from there the two share the new
	// sessions, each taking the next one while its debited pace leads.
	if len(served)-toPeer < 2 {
		t.Fatalf("served %v; with both sides debited, loc must take its share once p's lead is spent", served)
	}
}
