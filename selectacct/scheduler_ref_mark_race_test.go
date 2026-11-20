package selectacct

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

// Ported from upstream #398 (the numbered-mark half) and #560 (a stale or
// missing score keeps a mark). Upstream's weekly tests are not ported: this
// fork skipped T3, so there is no weekly-cooked mark (MarkWeeklyExhaustedUntil)
// to reconcile.
//
// finishBegunRefresh and publishRefresh live in scheduler_ref_test.go, where
// upstream moved them (#379); this file defined the same two helpers before
// the 2026-11 merge brought upstream's copies.

func codexScore(id string, headroom float64) Score {
	return Score{AccountID: id, Provider: account.ProviderCodex, Headroom: headroom, ShortHeadroom: headroom, Fresh: true}
}

func beginRefresh(t *testing.T, ref *SchedulerRef) {
	t.Helper()
	ref.SetUpdatedAt(time.Time{})
	if !ref.BeginRefreshIfStale(time.Minute) {
		t.Fatal("refresh did not begin")
	}
}

// A 429 that lands while a refresh is fetching usage is newer than every
// score in that refresh. Publishing the refresh must not read the account's
// pre-429 headroom as recovery and route straight back into the rejection.
func TestRefreshKeepsExhaustionMarkSetWhileItRan(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{codexScore("a", 0.5)}))
	beginRefresh(t, ref)
	ref.MarkExhaustedUntil(account.ProviderCodex, "a", "", time.Now().Add(5*time.Hour))
	if !finishBegunRefresh(t, ref, NewScheduler([]Score{codexScore("a", 0.3)}), true) {
		t.Fatal("refresh was not published")
	}
	if _, ok := ref.ExhaustedUntilFor(account.ProviderCodex, "a", ""); !ok {
		t.Fatal("the mark set during the refresh was dropped")
	}
	if !ref.Get().Exhausted(account.ProviderCodex, "a") {
		t.Fatal("routing reads the account as usable right after upstream rejected it")
	}
}

// A mark from before the refresh began is still superseded by the fresh
// headroom the refresh measured: the account recovered.
func TestRefreshStillClearsMarkItMeasuredPast(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{codexScore("a", 0.5)}))
	ref.MarkExhaustedUntil(account.ProviderCodex, "a", "", time.Now().Add(5*time.Hour))
	publishRefresh(t, ref, NewScheduler([]Score{codexScore("a", 0.6)}), true)
	if _, ok := ref.ExhaustedUntilFor(account.ProviderCodex, "a", ""); ok {
		t.Fatal("fresh headroom measured after the mark should clear it")
	}
}

// The partial-refresh path has the same race: scores computed at a
// revision read before the 429 must not clear it.
func TestMergeKeepsExhaustionMarkNewerThanItsScores(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{codexScore("a", 0.5)}))
	revision := ref.ScoreRevision()
	ref.MarkExhaustedUntil(account.ProviderCodex, "a", "", time.Now().Add(5*time.Hour))
	if _, ok := ref.MergeScoresForAccountGenerationAtScoreRevision([]Score{codexScore("a", 0.3)}, 0, revision); !ok {
		t.Fatal("merge was not published")
	}
	if !ref.Get().Exhausted(account.ProviderCodex, "a") {
		t.Fatal("merge cleared a mark set after its scores were measured")
	}
}

// A carried-forward score showing headroom did not measure the account's
// recovery (upstream #560, as written).
func TestRefreshKeepsExhaustionMarkOnStaleScore(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{codexScore("a", 0.5)}))
	ref.MarkExhaustedUntil(account.ProviderCodex, "a", "", time.Now().Add(5*time.Hour))
	stale := codexScore("a", 0.9)
	stale.Fresh = false
	publishRefresh(t, ref, NewScheduler([]Score{stale}), true)
	if !ref.Get().Exhausted(account.ProviderCodex, "a") {
		t.Fatal("stale score dropped an upstream exhaustion mark")
	}
}

// Neither did a publish that carries no score for the account at all
// (upstream #560; its test is a weekly-mark one, adapted here to the plain
// mark this fork has). The mark holds until it lapses or the account leaves
// the snapshot (upstream #561).
func TestRefreshWithoutTheAccountKeepsItsExhaustionMark(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{codexScore("a", 0.5), codexScore("ghost", 0.5)}))
	ref.MarkExhaustedUntil(account.ProviderCodex, "ghost", "", time.Now().Add(5*time.Hour))
	publishRefresh(t, ref, NewScheduler([]Score{codexScore("a", 0.5)}), true)
	if !ref.Get().Exhausted(account.ProviderCodex, "ghost") {
		t.Fatal("a refresh with no score for the account dropped its exhaustion mark")
	}
	// The account leaving the snapshot is what ends the mark early.
	ref.AdvanceAccountGenerationWithAccounts(1, 1, []account.Account{{ID: "a", Provider: account.ProviderCodex}})
	if _, ok := ref.ExhaustedUntilFor(account.ProviderCodex, "ghost", ""); ok {
		t.Fatal("the mark outlived its account's removal from the snapshot")
	}
}

// Fork additions (PATCHES.md §29). Upstream orders a mark against when the
// publish BEGAN. Our publishes routinely carry older readings -- the 2-minute
// usage-window cache, the 30 s sweep cache every usage-status read republishes
// -- so a score that says when it was measured is ordered on that instead.

func measuredCodexScore(id string, headroom float64, measuredAt time.Time) Score {
	score := codexScore(id, headroom)
	score.MeasuredAt = measuredAt
	return score
}

// The usage-status republication (PATCHES.md §26) reads the score revision
// AFTER the 429 and publishes a sweep measured BEFORE it. By publish order
// the scores are newer than the mark; by measurement they are older, and
// measurement is what decides.
func TestRepublicationKeepsAMarkNewerThanTheReadingItCarries(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{codexScore("a", 0.5)}))
	measured := time.Now()
	time.Sleep(time.Millisecond)
	ref.MarkExhaustedUntil(account.ProviderCodex, "a", "", time.Now().Add(5*time.Hour))
	revision := ref.ScoreRevision()
	if !ref.PublishWithoutRefreshForAccountGenerationAtScoreRevision(
		NewScheduler([]Score{measuredCodexScore("a", 0.9, measured)}), 0, revision) {
		t.Fatal("republication was not published")
	}
	if !ref.Get().Exhausted(account.ProviderCodex, "a") {
		t.Fatal("a reading taken before the 429 cleared its mark because it was published after it")
	}

	// A reading taken after the mark supersedes it on the same path.
	time.Sleep(time.Millisecond)
	if !ref.PublishWithoutRefreshForAccountGenerationAtScoreRevision(
		NewScheduler([]Score{measuredCodexScore("a", 0.9, time.Now())}), 0, ref.ScoreRevision()) {
		t.Fatal("republication was not published")
	}
	if _, ok := ref.ExhaustedUntilFor(account.ProviderCodex, "a", ""); ok {
		t.Fatal("fresh headroom measured after the 429 did not clear the mark")
	}
}

// Measurement time also wins in the other direction: a refresh that BEGAN
// before the mark but whose reading was taken after it may clear it. Publish
// order alone would keep it.
func TestRefreshWhoseReadingFollowsTheMarkClearsItWhereverItBegan(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{codexScore("a", 0.5)}))
	beginRefresh(t, ref)
	ref.MarkExhaustedUntil(account.ProviderCodex, "a", "", time.Now().Add(5*time.Hour))
	time.Sleep(time.Millisecond)
	if !finishBegunRefresh(t, ref, NewScheduler([]Score{measuredCodexScore("a", 0.9, time.Now())}), true) {
		t.Fatal("refresh was not published")
	}
	if _, ok := ref.ExhaustedUntilFor(account.ProviderCodex, "a", ""); ok {
		t.Fatal("a reading taken after the mark did not clear it")
	}
}

// A model-pool mark is judged against its pool's score, which comes from the
// same usage reading as the account's and inherits its measurement time.
func TestPoolMarkInheritsItsAccountsMeasurementTime(t *testing.T) {
	const pool = "gpt-5"
	scored := func(measuredAt time.Time) Scheduler {
		score := measuredCodexScore("a", 0.9, measuredAt)
		// Keyed as the proxy keys them: marks are stored under ModelKey.
		score.ModelScores = map[string]Score{ModelKey(pool): codexScore("a", 0.9)}
		return NewScheduler([]Score{score})
	}
	measured := time.Now()
	ref := NewSchedulerRef(scored(measured))
	time.Sleep(time.Millisecond)
	ref.MarkExhaustedUntil(account.ProviderCodex, "a", pool, time.Now().Add(5*time.Hour))
	publishRefresh(t, ref, scored(measured), true)
	if _, ok := ref.ExhaustedUntilFor(account.ProviderCodex, "a", pool); !ok {
		t.Fatal("a pool score read before the pool's 429 cleared the mark")
	}
	time.Sleep(time.Millisecond)
	publishRefresh(t, ref, scored(time.Now()), true)
	if _, ok := ref.ExhaustedUntilFor(account.ProviderCodex, "a", pool); ok {
		t.Fatal("a pool score read after the pool's 429 did not clear the mark")
	}
}

// The tokenless FinishRefresh (kept here, dropped upstream) publishes without
// a Begin, so it begins at the moment it publishes: it must order after every
// mark already set, not after the refreshMarkSeq an earlier refresh left.
func TestTokenlessFinishRefreshOrdersAfterMarksSetBeforeIt(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{codexScore("a", 0.5)}))
	publishRefresh(t, ref, NewScheduler([]Score{codexScore("a", 0.5)}), true)
	ref.MarkExhaustedUntil(account.ProviderCodex, "a", "", time.Now().Add(5*time.Hour))
	ref.FinishRefresh(NewScheduler([]Score{codexScore("a", 0.9)}), true)
	if _, ok := ref.ExhaustedUntilFor(account.ProviderCodex, "a", ""); ok {
		t.Fatal("a publish made after the mark kept it on a stale refresh boundary")
	}
}
