package selectacct

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

// These pin the merge of upstream's provider-scoped identity (ScoreKey-keyed
// session counts, codex-owner-<hash> account IDs) into this fork's scheduler,
// whose counts, live counts and reserves were bare-ID keyed. Each one fails
// silently in production -- a zero count or an absent holdout raises no error --
// so the only guard is a test that constructs the keying and reads it back.

func weeklyScore(provider account.Provider, id string, remaining float64) Score {
	return Score{
		AccountID: id, Provider: provider, Fresh: true, WeeklyOnly: true,
		Headroom: remaining, ShortHeadroom: remaining,
		WeeklyHeadroom: remaining, WeeklyRemaining: remaining,
		WeeklyResetAt: time.Now().Add(48 * time.Hour),
	}
}

// score() must find parked counts, live counts, reserves and live debits under
// ScoreKey(provider, id) -- how the proxy keys SchedulerSessionCounts and
// NoteRouted -- and under the bare ID, how the fork's own maps were keyed.
func TestScoreReadsPerAccountMapsUnderScoreKeyAndBareID(t *testing.T) {
	base := NewScheduler([]Score{weeklyScore(account.ProviderCodex, "a@example.com", 0.9)})
	key := ScoreKey(account.ProviderCodex, "a@example.com")
	for name, k := range map[string]string{"score key": key, "bare id": "a@example.com"} {
		t.Run(name, func(t *testing.T) {
			scheduler := base.
				WithSessionCounts(map[string]int{k: 3}).
				WithLiveSessionCounts(map[string]int{k: 2}).
				WithReserves(map[string]float64{k: 0.25}).
				WithLiveDebits(map[string]int{k: 5})
			got := scheduler.score(account.ProviderCodex, "a@example.com")
			if got.Sessions != 3 || got.LiveSessions != 2 || got.Reserve != 0.25 {
				t.Fatalf("sessions=%d live=%d reserve=%v, want 3/2/0.25", got.Sessions, got.LiveSessions, got.Reserve)
			}
			if got.Headroom >= 0.9 {
				t.Fatalf("live debit keyed by %s was not applied: headroom %v", name, got.Headroom)
			}
		})
	}
}

// One email can be both a Codex account and a Claude profile. ScoreKey-keyed
// maps keep their counts and holdouts apart.
func TestScoreKeyedMapsDoNotLeakAcrossProvidersSharingAnID(t *testing.T) {
	const id = "same@example.com"
	scheduler := NewScheduler([]Score{
		weeklyScore(account.ProviderCodex, id, 0.9),
		weeklyScore(account.ProviderClaude, id, 0.9),
	}).
		WithLiveSessionCounts(map[string]int{ScoreKey(account.ProviderClaude, id): 4}).
		WithReserves(map[string]float64{ScoreKey(account.ProviderClaude, id): 0.95})
	codex := scheduler.score(account.ProviderCodex, id)
	claude := scheduler.score(account.ProviderClaude, id)
	if codex.LiveSessions != 0 || codex.Reserve != 0 {
		t.Fatalf("Claude's live sessions/reserve leaked onto the Codex account: %+v", codex)
	}
	if claude.LiveSessions != 4 || claude.Reserve != 0.95 {
		t.Fatalf("Claude profile lost its own live sessions/reserve: %+v", claude)
	}
	if !scheduler.InHoldout(account.ProviderClaude, id) || scheduler.InHoldout(account.ProviderCodex, id) {
		t.Fatal("holdout must apply to the Claude profile only")
	}
}

// A Codex account enrolled with owner claims is keyed "codex-owner-<hash>";
// SUBROUTER_RESERVED_ACCOUNTS names it by email. The holdout must still apply,
// resolved through the account list the SchedulerRef was synced with.
func TestReserveResolvesCodexOwnerIDByLoginEmail(t *testing.T) {
	const ownerID = "codex-owner-0123456789abcdef"
	owner := account.Account{
		ID: ownerID, Provider: account.ProviderCodex, AuthMode: account.AuthModeOAuth,
		Email: ownerID, Label: "Owner@Example.com [pro]", Token: "tok",
	}
	reserves := map[string]float64{"owner@example.com": 0.2}
	ref := NewSchedulerRef(NewScheduler([]Score{weeklyScore(account.ProviderCodex, ownerID, 0.15)}))

	// Calibration: without the account list the email cannot reach the hash ID.
	if ref.Get().WithReserves(reserves).InHoldout(account.ProviderCodex, ownerID) {
		t.Fatal("calibration: holdout applied before any alias was known")
	}

	ref.AdvanceAccountGenerationWithAccounts(1, 1, []account.Account{owner})
	scheduler := ref.Get().WithReserves(reserves)
	if !scheduler.InHoldout(account.ProviderCodex, ownerID) || !scheduler.Evicted(account.ProviderCodex, ownerID) {
		t.Fatal("email-keyed holdout did not reach the codex-owner account")
	}
	if scheduler.UsableForNewSession(account.ProviderCodex, ownerID) {
		t.Fatal("a new session started inside the email-keyed holdout")
	}

	// A published refresh replaces the stored scheduler; the aliases must
	// survive it because they ride on Get, not on the snapshot.
	ref.Set(NewScheduler([]Score{weeklyScore(account.ProviderCodex, ownerID, 0.15)}))
	if !ref.Get().WithReserves(reserves).InHoldout(account.ProviderCodex, ownerID) {
		t.Fatal("a score refresh dropped the alias table")
	}

	// The standalone builder resolves the same way.
	direct := NewScheduler([]Score{weeklyScore(account.ProviderCodex, ownerID, 0.15)}).
		WithAccountAliases([]account.Account{owner}).
		WithReserves(reserves)
	if !direct.InHoldout(account.ProviderCodex, ownerID) {
		t.Fatal("WithAccountAliases did not resolve the email-keyed holdout")
	}
}

// Builders are copy-then-modify: a literal-built scheduler silently dropped
// reserves, which released the holdout on every failover pick.
func TestBuildersKeepTheHoldoutWhateverTheOrder(t *testing.T) {
	score := weeklyScore(account.ProviderClaude, "held", 0.1)
	score.ModelScores = map[string]Score{"claudeopus": weeklyScore(account.ProviderClaude, "held", 0.1)}
	reserves := map[string]float64{"held": 0.2}
	chains := map[string]Scheduler{
		"reserves then ForModel":      NewScheduler([]Score{score}).WithReserves(reserves).ForModel("claude-opus"),
		"reserves then session count": NewScheduler([]Score{score}).ForModel("claude-opus").WithReserves(reserves).WithSessionCounts(map[string]int{"held": 1}),
		"reserves then overlay":       applyExhaustionMarks(NewScheduler([]Score{score}).WithReserves(reserves), map[string]time.Time{poolScopedExhaustionKey(account.ProviderClaude, "other", ""): time.Now().Add(time.Hour)}, time.Now()),
	}
	for name, scheduler := range chains {
		if !scheduler.InHoldout(account.ProviderClaude, "held") {
			t.Errorf("%s: holdout lost", name)
		}
	}
}

// In this fork WeeklyHeadroom 0 means "weekly unmeasured" and fails open.
// Upstream's paid-fallback field of the same name defaulted to 1; no default
// of 1 may land on ours, or an unmeasured account reads as a full budget.
func TestNoPathDefaultsWeeklyHeadroomToOne(t *testing.T) {
	shortOnly := ScoreFromLimitWindows("a", 0, []LimitWindow{
		{Name: "5h", UsedPercent: 10, LimitWindowSeconds: 5 * 60 * 60, ResetAfterSeconds: 3600},
	})
	if shortOnly.WeeklyHeadroom != 0 || shortOnly.WeeklyRemaining != 0 {
		t.Fatalf("no weekly window, yet weekly = %v/%v", shortOnly.WeeklyHeadroom, shortOnly.WeeklyRemaining)
	}
	if got := NewScheduler(nil).ScoreFor(account.ProviderClaude, "ghost").WeeklyHeadroom; got != 0 {
		t.Fatalf("optimistic default WeeklyHeadroom = %v, want 0 (unmeasured)", got)
	}
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(account.ProviderClaude, "unscored", "", time.Now().Add(time.Hour))
	ref.MarkExhaustedUntil(account.ProviderClaude, "unscored-pool", "claude-opus", time.Now().Add(time.Hour))
	for _, id := range []string{"unscored", "unscored-pool"} {
		if got := ref.Get().ScoreFor(account.ProviderClaude, id).WeeklyHeadroom; got != 0 {
			t.Fatalf("%s: exhaustion overlay invented WeeklyHeadroom = %v", id, got)
		}
	}
}

// Sticky retention is the same predicate as new-session eligibility, read on
// the measured score: a busy account's live debit must not evict its idle
// sessions, and a holdout still does.
func TestUsableForStickySessionIsMeasuredNewSessionEligibility(t *testing.T) {
	scheduler := NewScheduler([]Score{
		weeklyScore(account.ProviderCodex, "busy", 0.3),
		weeklyScore(account.ProviderCodex, "held", 0.3),
	}).
		WithLiveDebits(map[string]int{ScoreKey(account.ProviderCodex, "busy"): 15}).
		WithReserves(map[string]float64{"held": 0.3})
	if scheduler.UsableForNewSession(account.ProviderCodex, "busy") {
		t.Fatal("calibration: 15 debited requests should push busy below the new-session floor")
	}
	if !scheduler.UsableForStickySession(account.ProviderCodex, "busy") {
		t.Fatal("the live debit evicted an idle session from a 30%-measured account")
	}
	if scheduler.UsableForStickySession(account.ProviderCodex, "held") {
		t.Fatal("an idle session stayed on an account at its holdout")
	}
}
