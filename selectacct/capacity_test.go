package selectacct

import (
	"fmt"
	"testing"
	"time"

	"github.com/discordwell/subrouter/account"
)

// Ported from upstream manaflow-ai/subrouter selectacct/capacity_test.go
// (862a5a5, #382), adapted where the fork's Pick differs (PATCHES section 27):
// the fork has no PickBest and no placement spread band (Pick is deterministic,
// PATCHES section 10), so the spread assertions are replaced by their
// deterministic equivalents. The last two tests are the fork's own.

func capacityTestAccounts(n int) []account.Account {
	out := make([]account.Account, n)
	for i := range out {
		out[i] = account.Account{ID: fmt.Sprintf("acct-%d", i), Provider: account.ProviderCodex, AuthMode: account.AuthModeOAuth}
	}
	return out
}

// A capacity mark is not quota evidence: headroom, exhaustion and sticky
// retention read exactly as before, and quota marks are untouched.
func TestCapacityMarkLeavesQuotaViewUnchanged(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{
		{AccountID: "acct-0", Provider: account.ProviderCodex, Headroom: 0.8, ShortHeadroom: 0.8},
	}))
	ref.MarkCapacityUntil(account.ProviderCodex, "acct-0", "gpt-6-astra", "priority", time.Now().Add(time.Minute))
	scheduler := ref.Get().ForModel("gpt-6-astra")
	score := scheduler.ScoreFor(account.ProviderCodex, "acct-0")
	if score.Headroom != 0.8 || score.ShortHeadroom != 0.8 {
		t.Fatalf("capacity mark changed headroom: %+v", score)
	}
	if scheduler.Exhausted(account.ProviderCodex, "acct-0") || !scheduler.UsableForStickySession(account.ProviderCodex, "acct-0") {
		t.Fatal("capacity mark made the account exhausted or evicted its sticky sessions")
	}
	if _, ok := ref.ExhaustedUntilFor(account.ProviderCodex, "acct-0", "gpt-6-astra"); ok {
		t.Fatal("capacity mark recorded as a quota exhaustion mark")
	}
	marked := scheduler.WithCapacityMarks(ref.CapacityMarks(account.ProviderCodex, "gpt-6-astra", "priority"))
	if marked.CapacityEvicting(account.ProviderCodex, "acct-0") {
		t.Fatal("one capacity failure must not evict sticky sessions")
	}
	if !marked.UsableForStickySession(account.ProviderCodex, "acct-0") || marked.Exhausted(account.ProviderCodex, "acct-0") {
		t.Fatal("capacity-marked scheduler changed the quota view")
	}
	// The fork's hard gate too: a mark never reads as Evicted, so the warm
	// rule's eviction branch does not fire on it.
	if marked.Evicted(account.ProviderCodex, "acct-0") {
		t.Fatal("capacity mark read as an eviction")
	}
}

// Marks are scoped to (model, service tier): the priority tier shedding
// says nothing about the default tier or another model. The wildcard tier
// (a websocket upgrade, whose tier arrives later) sees every tier.
func TestCapacityMarksAreScopedByModelAndTier(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	until := time.Now().Add(time.Minute)
	ref.MarkCapacityUntil(account.ProviderCodex, "acct-0", "gpt-6-astra", "priority", until)
	if got := ref.CapacityMarks(account.ProviderCodex, "gpt-6-astra", "priority"); got[ScoreKey(account.ProviderCodex, "acct-0")] != 1 {
		t.Fatalf("priority marks = %v, want acct-0 once", got)
	}
	if got := ref.CapacityMarks(account.ProviderCodex, "gpt-6-astra", ""); len(got) != 0 {
		t.Fatalf("default-tier marks = %v, want none", got)
	}
	if got := ref.CapacityMarks(account.ProviderCodex, "gpt-6-other", "priority"); len(got) != 0 {
		t.Fatalf("other-model marks = %v, want none", got)
	}
	if got := ref.CapacityMarks(account.ProviderCodex, "gpt-6-astra", CapacityAnyTier); got[ScoreKey(account.ProviderCodex, "acct-0")] != 1 {
		t.Fatalf("any-tier marks = %v, want acct-0", got)
	}
	if CapacityPoolKey("GPT-6-Astra", "auto") != CapacityPoolKey("gpt-6-astra", "") {
		t.Fatal("auto and empty tiers must share the default pool key")
	}
	// Fork addition: another provider's marks never leak into a Codex read
	// (ScoreKey is provider-scoped; a Claude profile can share the ID).
	ref.MarkCapacityUntil(account.ProviderClaude, "acct-0", "gpt-6-astra", "", until)
	if got := ref.CapacityMarks(account.ProviderCodex, "gpt-6-astra", ""); len(got) != 0 {
		t.Fatalf("codex default-tier marks = %v after a claude mark, want none", got)
	}
}

// Consecutive failures accumulate while the mark is live; the first success
// on that account+model clears every tier's mark; an expired mark starts
// over.
func TestCapacityMarkCountsFailuresAndClearsOnSuccess(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	until := time.Now().Add(time.Minute)
	if n := ref.MarkCapacityUntil(account.ProviderCodex, "acct-0", "m", "", until); n != 1 {
		t.Fatalf("first failure count = %d", n)
	}
	if n := ref.MarkCapacityUntil(account.ProviderCodex, "acct-0", "m", "", until); n != 2 {
		t.Fatalf("second failure count = %d", n)
	}
	ref.MarkCapacityUntil(account.ProviderCodex, "acct-0", "m", "priority", until)
	ref.MarkCapacityUntil(account.ProviderCodex, "acct-0", "other", "", until)
	if !ref.ClearCapacity(account.ProviderCodex, "acct-0", "m") {
		t.Fatal("clear reported nothing cleared")
	}
	for _, tier := range []string{"", "priority"} {
		if _, _, ok := ref.CapacityMarkFor(account.ProviderCodex, "acct-0", "m", tier); ok {
			t.Fatalf("tier %q mark survived a success", tier)
		}
	}
	if _, _, ok := ref.CapacityMarkFor(account.ProviderCodex, "acct-0", "other", ""); !ok {
		t.Fatal("success on one model cleared another model's mark")
	}
	ref.MarkCapacityUntil(account.ProviderCodex, "acct-1", "m", "", time.Now().Add(-time.Second))
	if n := ref.MarkCapacityUntil(account.ProviderCodex, "acct-1", "m", "", until); n != 1 {
		t.Fatalf("failure after an expired mark counted %d, want a fresh 1", n)
	}
}

// A capacity-marked account ranks below every unmarked account of the same
// selection tier, without being excluded.
//
// Adapted: upstream asserts PickBest and 500 spread Picks never reach the
// marked account, and that with every account marked placement spreads over
// the whole pool. The fork's Pick is deterministic with no spread band, so
// one Pick and the Explain winner carry the first claim, and the second
// becomes its deterministic equivalent: equal marks everywhere rank the pool
// exactly as no marks do.
func TestCapacityMarkLowersRankWithoutExcluding(t *testing.T) {
	candidates := capacityTestAccounts(4)
	scores := make([]Score, len(candidates))
	for i, candidate := range candidates {
		scores[i] = Score{AccountID: candidate.ID, Provider: account.ProviderCodex, Headroom: 0.9, ShortHeadroom: 0.9}
	}
	// The marked account is also the roomiest: rank must still put it last.
	scores[0].Headroom, scores[0].ShortHeadroom = 1, 1
	base := NewScheduler(scores)
	if picked, err := base.Pick(candidates); err != nil || picked.ID != "acct-0" {
		t.Fatalf("fixture drift: unmarked Pick = %v (%v), want the roomiest acct-0", picked.ID, err)
	}
	marked := base.WithCapacityMarks(map[string]int{ScoreKey(account.ProviderCodex, "acct-0"): 1})
	picked, err := marked.Pick(candidates)
	if err != nil || picked.ID == "acct-0" {
		t.Fatalf("Pick = %v (%v), want an unmarked account", picked.ID, err)
	}
	explained, err := marked.Explain(candidates)
	if err != nil {
		t.Fatal(err)
	}
	if explained[0].Account.ID != picked.ID || explained[len(explained)-1].Account.ID != "acct-0" {
		t.Fatalf("Explain order %v, want Pick's winner first and the marked account last", explainedIDs(explained))
	}
	if reason := explained[len(explained)-2].Reason; reason != "not shedding this model's requests; the other is at capacity" {
		t.Fatalf("reason above the marked account = %q, want the capacity clause", reason)
	}
	only, err := marked.Pick(candidates[:1])
	if err != nil || only.ID != "acct-0" {
		t.Fatalf("sole candidate = %v (%v), want the marked account still pickable", only.ID, err)
	}
	all := map[string]int{}
	for _, candidate := range candidates {
		all[ScoreKey(account.ProviderCodex, candidate.ID)] = 1
	}
	everyMarked := base.WithCapacityMarks(all)
	wantOrder, _ := base.Explain(candidates)
	gotOrder, _ := everyMarked.Explain(candidates)
	if fmt.Sprint(explainedIDs(gotOrder)) != fmt.Sprint(explainedIDs(wantOrder)) {
		t.Fatalf("with every account marked alike the order is %v, want the unmarked order %v",
			explainedIDs(gotOrder), explainedIDs(wantOrder))
	}
}

func explainedIDs(rows []Explanation) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Account.ID)
	}
	return out
}

// Stampede: a burst of new sessions arrives while the model sheds load on
// part of the pool. Marked accounts take none of the burst, the rest share
// it rather than one of them absorbing everything.
//
// Adapted: upstream's 0.15-0.35 share band comes from its random spread. The
// fork spreads new sessions deterministically through the session-count
// discount (spreadAdjusted), which rotates the burst across the unmarked
// accounts; the band is kept and holds.
func TestCapacityMarksSpreadStampedeAcrossUnmarkedAccounts(t *testing.T) {
	candidates := capacityTestAccounts(7)
	ref := NewSchedulerRef(NewScheduler(nil))
	until := time.Now().Add(time.Minute)
	for _, id := range []string{"acct-0", "acct-1", "acct-2"} {
		ref.MarkCapacityUntil(account.ProviderCodex, id, "gpt-6-astra", "", until)
	}
	placements := map[string]int{}
	sessions := map[string]int{}
	for range 400 {
		scheduler := ref.Get().ForModel("gpt-6-astra").
			WithSessionCounts(sessions).
			WithCapacityMarks(ref.CapacityMarks(account.ProviderCodex, "gpt-6-astra", ""))
		picked, err := scheduler.Pick(candidates)
		if err != nil {
			t.Fatal(err)
		}
		placements[picked.ID]++
		sessions[ScoreKey(account.ProviderCodex, picked.ID)]++
	}
	for _, id := range []string{"acct-0", "acct-1", "acct-2"} {
		if placements[id] != 0 {
			t.Fatalf("marked %s took %d placements: %v", id, placements[id], placements)
		}
	}
	for _, id := range []string{"acct-3", "acct-4", "acct-5", "acct-6"} {
		if share := float64(placements[id]) / 400; share < 0.15 || share > 0.35 {
			t.Fatalf("%s share %.2f, want the burst spread over the 4 unmarked accounts: %v", id, share, placements)
		}
	}
}

// Sticky retention: one capacity failure keeps the session on its account
// (its prompt cache lives there); the same account failing again evicts.
func TestCapacityStickyEvictionNeedsRepeatedFailures(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	until := time.Now().Add(time.Minute)
	ref.MarkCapacityUntil(account.ProviderCodex, "acct-0", "m", "", until)
	scheduler := ref.Get().WithCapacityMarks(ref.CapacityMarks(account.ProviderCodex, "m", ""))
	if scheduler.CapacityEvicting(account.ProviderCodex, "acct-0") {
		t.Fatal("one failure evicted the sticky session")
	}
	ref.MarkCapacityUntil(account.ProviderCodex, "acct-0", "m", "", until)
	scheduler = ref.Get().WithCapacityMarks(ref.CapacityMarks(account.ProviderCodex, "m", ""))
	if !scheduler.CapacityEvicting(account.ProviderCodex, "acct-0") {
		t.Fatalf("%d consecutive failures did not evict", CapacityStickyEvictFailures)
	}
	// Capacity marks survive score publication (usage refresh) because they
	// live beside the snapshot, not in it.
	ref.Set(NewScheduler([]Score{{AccountID: "acct-0", Provider: account.ProviderCodex, Headroom: 1, ShortHeadroom: 1}}))
	if _, failures, ok := ref.CapacityMarkFor(account.ProviderCodex, "acct-0", "m", ""); !ok || failures != 2 {
		t.Fatalf("capacity mark after a refresh: failures=%d ok=%v", failures, ok)
	}
}

// A mark keeps the reason of its latest failure for diagnostics (upstream
// #620, as written).
func TestCapacityMarkKeepsLastReason(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	until := time.Now().Add(time.Minute)
	ref.MarkCapacityReasonUntil(account.ProviderCodex, "acct-0", "m", "", "http_503", until)
	if n := ref.MarkCapacityReasonUntil(account.ProviderCodex, "acct-0", "m", "", "server_is_overloaded", until); n != 2 {
		t.Fatalf("failures=%d, want 2", n)
	}
	marks := ref.CapacityMarkList()
	if len(marks) != 1 || marks[0].Reason != "server_is_overloaded" || marks[0].Failures != 2 {
		t.Fatalf("marks=%+v", marks)
	}
}

// Fork: the capacity clause sits where upstream put it, right after the
// selection tier -- which in the fork places it above the machine-binding and
// endgame-drain clauses upstream does not have. A shedding account fails the
// request that lands on it, so neither a machine slot nor an expiring budget
// is worth sending the request there. Pins the position: moving the clause
// below either one turns this red.
func TestCapacityMarkOutranksMachineBindingAndEndgameDrain(t *testing.T) {
	now := time.Now()
	// Machine binding: without marks the bound account wins.
	bound := NewScheduler([]Score{
		{AccountID: "bound", Provider: account.ProviderCodex, Headroom: 0.5, ShortHeadroom: 1, WeeklyHeadroom: 0.5,
			WeeklyResetAt: now.Add(5 * 24 * time.Hour), Fresh: true},
		{AccountID: "free", Provider: account.ProviderCodex, Headroom: 0.5, ShortHeadroom: 1, WeeklyHeadroom: 0.5,
			WeeklyResetAt: now.Add(5 * 24 * time.Hour), Fresh: true},
	}).WithMachine("mine", fakeMachines{bindings: map[string][]string{"bound": {"mine"}}})
	pool := []account.Account{
		{ID: "free", Provider: account.ProviderCodex, AuthMode: account.AuthModeOAuth},
		{ID: "bound", Provider: account.ProviderCodex, AuthMode: account.AuthModeOAuth},
	}
	if picked, _ := bound.Pick(pool); picked.ID != "bound" {
		t.Fatalf("fixture drift: unmarked pick %q, want the machine-bound account", picked.ID)
	}
	marked := bound.WithCapacityMarks(map[string]int{ScoreKey(account.ProviderCodex, "bound"): 1})
	if picked, _ := marked.Pick(pool); picked.ID != "free" {
		t.Fatalf("picked %q, want the unmarked account over the capacity-marked bound one", picked.ID)
	}

	// Endgame drain: without marks the rolling account wins.
	rolling := ScoreFromLimitWindowsAt("rolling", 0, codexWindows(50, 12*60*60), now)
	healthy := ScoreFromLimitWindowsAt("healthy", 0, codexWindows(40, 6*24*60*60), now)
	drain := NewScheduler([]Score{rolling, healthy})
	pool = []account.Account{
		{ID: "healthy", AuthMode: account.AuthModeOAuth},
		{ID: "rolling", AuthMode: account.AuthModeOAuth},
	}
	if picked, _ := drain.Pick(pool); picked.ID != "rolling" {
		t.Fatalf("fixture drift: unmarked pick %q, want the endgame-draining account", picked.ID)
	}
	marked = drain.WithCapacityMarks(map[string]int{ScoreKey(account.ProviderCodex, "rolling"): 1})
	if picked, _ := marked.Pick(pool); picked.ID != "healthy" {
		t.Fatalf("picked %q, want the unmarked account over the capacity-marked draining one", picked.ID)
	}
}

// Fork: the clause never crosses a selection tier. A capacity-marked account
// that can take the session still outranks an unmarked one that cannot (an
// evicted or spent account), because the mark is a tie-break inside a tier,
// not an exclusion -- the one thing upstream's position guarantees.
func TestCapacityMarkDoesNotCrossSelectionTier(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "marked", Provider: account.ProviderCodex, Headroom: 0.9, ShortHeadroom: 0.9},
		{AccountID: "spent", Provider: account.ProviderCodex, Headroom: 0, ShortHeadroom: 0},
	}).WithCapacityMarks(map[string]int{ScoreKey(account.ProviderCodex, "marked"): 3})
	picked, err := scheduler.Pick([]account.Account{
		{ID: "spent", Provider: account.ProviderCodex, AuthMode: account.AuthModeOAuth},
		{ID: "marked", Provider: account.ProviderCodex, AuthMode: account.AuthModeOAuth},
	})
	if err != nil || picked.ID != "marked" {
		t.Fatalf("picked %v (%v), want the marked account over a spent one", picked.ID, err)
	}
}
