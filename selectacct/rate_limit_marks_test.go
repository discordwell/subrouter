package selectacct

import (
	"testing"
	"time"

	"github.com/discordwell/subrouter/account"
)

// Fork (PATCHES section 35.3): a rate-limit mark ranks the account after
// unmarked ones for any model, like one capacity failure, and changes
// nothing else -- no quota view, no eviction, however many 429s arrive.
func TestRateLimitMarkRanksLastAndNeverEvicts(t *testing.T) {
	candidates := capacityTestAccounts(2)
	ref := NewSchedulerRef(NewScheduler([]Score{
		// The marked account is the roomiest: rank must still put it last.
		{AccountID: "acct-0", Provider: account.ProviderCodex, Headroom: 1, ShortHeadroom: 1},
		{AccountID: "acct-1", Provider: account.ProviderCodex, Headroom: 0.6, ShortHeadroom: 0.6},
	}))
	if picked, err := ref.Get().Pick(candidates); err != nil || picked.ID != "acct-0" {
		t.Fatalf("fixture drift: unmarked Pick = %v (%v), want the roomiest acct-0", picked.ID, err)
	}
	if ref.HasRankMarks() {
		t.Fatal("HasRankMarks with no mark")
	}
	until := time.Now().Add(time.Minute)
	for range 2 * CapacityStickyEvictFailures {
		ref.MarkRateLimitedUntil(account.ProviderCodex, "acct-0", until)
	}
	if !ref.HasRankMarks() {
		t.Fatal("HasRankMarks misses a live rate-limit mark, so selection would not attach it")
	}
	// Per account, not per pool: every model and tier sees it.
	for _, pool := range [][2]string{{"gpt-6-astra", ""}, {"another-model", "priority"}, {"m", CapacityAnyTier}} {
		marks := ref.RankMarks(account.ProviderCodex, pool[0], pool[1])
		if marks[ScoreKey(account.ProviderCodex, "acct-0")] != 1 {
			t.Fatalf("RankMarks(%v) = %v, want acct-0 at one", pool, marks)
		}
		scheduler := ref.Get().ForModel(pool[0]).WithCapacityMarks(marks)
		if picked, err := scheduler.Pick(candidates); err != nil || picked.ID != "acct-1" {
			t.Fatalf("Pick(%v) = %v (%v), want the unmarked acct-1", pool, picked.ID, err)
		}
		if scheduler.CapacityEvicting(account.ProviderCodex, "acct-0") || scheduler.Evicted(account.ProviderCodex, "acct-0") ||
			scheduler.Exhausted(account.ProviderCodex, "acct-0") || !scheduler.UsableForStickySession(account.ProviderCodex, "acct-0") {
			t.Fatal("a rate-limit mark reached the eviction, exhaustion or sticky-retention view")
		}
	}
	if _, ok := ref.ExhaustedUntilFor(account.ProviderCodex, "acct-0", ""); ok {
		t.Fatal("rate-limit mark recorded as a quota exhaustion mark")
	}
	if len(ref.CapacityMarkList()) != 0 {
		t.Fatal("rate-limit mark recorded as a capacity mark")
	}
	// Another provider's pool does not see it.
	if marks := ref.RankMarks(account.ProviderClaude, "m", ""); len(marks) != 0 {
		t.Fatalf("Claude RankMarks = %v, want none", marks)
	}
}

// A rate-limit mark never adds to a capacity count: a capacity-marked
// account keeps its own count (one failure stays one, short of eviction),
// and the mark lasts until its expiry, extended but never shortened.
func TestRateLimitMarkLeavesCapacityCountsAlone(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	now := time.Now()
	ref.MarkCapacityUntil(account.ProviderCodex, "acct-0", "m", "", now.Add(time.Minute))
	ref.MarkRateLimitedUntil(account.ProviderCodex, "acct-0", now.Add(time.Minute))
	scheduler := ref.Get().WithCapacityMarks(ref.RankMarks(account.ProviderCodex, "m", ""))
	if got := scheduler.CapacityFailures(account.ProviderCodex, "acct-0"); got != 1 {
		t.Fatalf("capacity failures = %d, want the capacity mark's own 1", got)
	}
	if scheduler.CapacityEvicting(account.ProviderCodex, "acct-0") {
		t.Fatal("a capacity failure plus a rate-limit mark evicted the sticky sessions")
	}
	ref.MarkRateLimitedUntil(account.ProviderCodex, "acct-1", now.Add(2*time.Minute))
	ref.MarkRateLimitedUntil(account.ProviderCodex, "acct-1", now.Add(time.Minute))
	if until, ok := ref.RateLimitedUntilFor(account.ProviderCodex, "acct-1"); !ok || !until.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("mark until %v (%v), want the later expiry kept", until, ok)
	}
	ref.MarkRateLimitedUntil(account.ProviderCodex, "acct-2", now.Add(-time.Second))
	if _, ok := ref.RateLimitedUntilFor(account.ProviderCodex, "acct-2"); ok {
		t.Fatal("a mark already past was recorded")
	}
}
