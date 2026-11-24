package selectacct

import (
	"testing"
	"time"

	"github.com/discordwell/subrouter/account"
)

func TestPickPrefersHeadroom(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "a", Headroom: 0.20, Sessions: 0},
		{AccountID: "b", Headroom: 0.90, Sessions: 5},
	})

	got, err := scheduler.Pick([]account.Account{{ID: "a"}, {ID: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "b" {
		t.Fatalf("got %q, want b", got.ID)
	}
}

func TestPickPrefersSoonExpiringHealthyHeadroom(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "later-full", Headroom: 1.00, ShortHeadroom: 1.00, ShortResetAfterSeconds: 5 * 60 * 60, ExpiryPressure: 1.00 / float64(5*60*60)},
		{AccountID: "soon-healthy", Headroom: 0.93, ShortHeadroom: 0.93, ShortResetAfterSeconds: 3 * 60 * 60, ExpiryPressure: 0.93 / float64(3*60*60)},
	})

	got, err := scheduler.Pick([]account.Account{{ID: "later-full"}, {ID: "soon-healthy"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "soon-healthy" {
		t.Fatalf("got %q, want soon-healthy", got.ID)
	}
}

func TestPickProtectsLowShortWindowHeadroom(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "soon-low", Headroom: 0.27, ShortHeadroom: 0.27, ShortResetAfterSeconds: 157 * 60, ExpiryPressure: 0.27 / float64(157*60)},
		{AccountID: "soon-full", Headroom: 1.00, ShortHeadroom: 1.00, ShortResetAfterSeconds: 164 * 60, ExpiryPressure: 1.00 / float64(164*60)},
	})

	got, err := scheduler.Pick([]account.Account{{ID: "soon-low"}, {ID: "soon-full"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "soon-full" {
		t.Fatalf("got %q, want soon-full", got.ID)
	}
}

func TestPickBreaksTiesByFewestSessions(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "a", Headroom: 0.75, Sessions: 4},
		{AccountID: "b", Headroom: 0.75, Sessions: 1},
	})

	got, err := scheduler.Pick([]account.Account{{ID: "a"}, {ID: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "b" {
		t.Fatalf("got %q, want b", got.ID)
	}
}

func TestWithSessionCountsUsesLiveAssignments(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "a", Headroom: 0.75, Sessions: 0},
		{AccountID: "b", Headroom: 0.75, Sessions: 0},
	}).WithSessionCounts(map[string]int{ScoreKey(account.ProviderCodex, "a"): 2})

	got, err := scheduler.Pick([]account.Account{{ID: "a"}, {ID: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "b" {
		t.Fatalf("got %q, want b", got.ID)
	}
}

func TestWithSessionCountsSeparatesProvidersSharingAnAccountID(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "same@example.com", Provider: account.ProviderCodex, Headroom: 0.75},
		{AccountID: "same@example.com", Provider: account.ProviderClaude, Headroom: 0.75},
	}).WithSessionCounts(map[string]int{
		ScoreKey(account.ProviderCodex, "same@example.com"):  2,
		ScoreKey(account.ProviderClaude, "same@example.com"): 9,
	})
	if got := scheduler.measuredScore(account.ProviderCodex, "same@example.com").Sessions; got != 2 {
		t.Fatalf("Codex sessions = %d, want 2", got)
	}
	if got := scheduler.measuredScore(account.ProviderClaude, "same@example.com").Sessions; got != 9 {
		t.Fatalf("Claude sessions = %d, want 9", got)
	}
}

func TestPickPrefersOAuthBeforeAPIKey(t *testing.T) {
	scheduler := NewScheduler(nil)

	got, err := scheduler.Pick([]account.Account{
		{ID: "apikey:first", AuthMode: account.AuthModeAPIKey},
		{ID: "z@example.com", AuthMode: account.AuthModeOAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "z@example.com" {
		t.Fatalf("got %q, want OAuth account", got.ID)
	}
}

// API-key accounts cost real money per token; OAuth subscription accounts are
// already paid for. Usable OAuth accounts stay ahead of API-key fallback.
func TestPickKeepsUsableOAuthBeforeAPIKey(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "oauth-usable", Headroom: 0.50, ShortHeadroom: 0.50, Sessions: 9},
		{AccountID: "apikey:flush", Headroom: 0.99, Sessions: 0},
	})

	got, err := scheduler.Pick([]account.Account{
		{ID: "apikey:flush", AuthMode: account.AuthModeAPIKey},
		{ID: "oauth-usable", AuthMode: account.AuthModeOAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "oauth-usable" {
		t.Fatalf("got %q, want usable OAuth account", got.ID)
	}
}

func TestPickHealthyOAuthBeforeExhaustedOAuthAndAPIKey(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "alice@example.com", Headroom: 0.90, ShortHeadroom: 0.90},
		{AccountID: "frank@example.com", Headroom: 0, ShortHeadroom: 0},
		{AccountID: "apikey:team-codex-1", Headroom: 0.01, ShortHeadroom: 0.01},
	})

	got, err := scheduler.Pick([]account.Account{
		{ID: "frank@example.com", AuthMode: account.AuthModeOAuth},
		{ID: "apikey:team-codex-1", AuthMode: account.AuthModeAPIKey},
		{ID: "alice@example.com", AuthMode: account.AuthModeOAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "alice@example.com" {
		t.Fatalf("got %q, want alice@example.com", got.ID)
	}
}

func TestPickFallsBackToAPIKeyBeforeExhaustedOAuth(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "oauth-empty", Headroom: 0, ShortHeadroom: 0},
		{AccountID: "apikey:paid", Headroom: 0.01, ShortHeadroom: 0.01},
	})

	got, err := scheduler.Pick([]account.Account{
		{ID: "oauth-empty", AuthMode: account.AuthModeOAuth},
		{ID: "apikey:paid", AuthMode: account.AuthModeAPIKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "apikey:paid" {
		t.Fatalf("got %q, want API-key fallback", got.ID)
	}
}

func TestPickKeepsConstrainedOAuthBeforeExhaustedOAuth(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "short-empty@example.com", Headroom: 0.79, ShortHeadroom: 0},
		{AccountID: "near-threshold@example.com", Headroom: 0.39, ShortHeadroom: 0.39},
	})

	got, err := scheduler.Pick([]account.Account{
		{ID: "short-empty@example.com", AuthMode: account.AuthModeOAuth},
		{ID: "near-threshold@example.com", AuthMode: account.AuthModeOAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "near-threshold@example.com" {
		t.Fatalf("got %q, want constrained but non-exhausted OAuth account", got.ID)
	}
}

// API-key accounts are still picked when no OAuth candidate exists.
func TestPickFallsBackToAPIKeyWhenNoOAuth(t *testing.T) {
	scheduler := NewScheduler(nil)

	got, err := scheduler.Pick([]account.Account{
		{ID: "apikey:only", AuthMode: account.AuthModeAPIKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "apikey:only" {
		t.Fatalf("got %q, want apikey:only", got.ID)
	}
}

// A Codex account's ID is its bare email and a Claude account's ID is its
// profile name, which is often the same email, so both providers routinely
// share one ID. The scheduler must score them independently; keying by the
// bare ID alone let one provider's score clobber the other's, which made a
// healthy Codex account look exhausted (and vice versa) whenever a same-named
// Claude profile existed.
func TestScoresAreIsolatedPerProvider(t *testing.T) {
	const shared = "lawrence@cmux.com"
	scheduler := NewScheduler([]Score{
		{AccountID: shared, Provider: account.ProviderCodex, Headroom: 1, ShortHeadroom: 1},
		{AccountID: shared, Provider: account.ProviderClaude, Headroom: 0, ShortHeadroom: 0},
	})

	if scheduler.Exhausted(account.ProviderCodex, shared) {
		t.Fatal("healthy Codex account must not be exhausted by a same-named Claude profile")
	}
	if !scheduler.UsableForNewSession(account.ProviderCodex, shared) {
		t.Fatal("healthy Codex account must stay usable for a new session")
	}
	if !scheduler.Exhausted(account.ProviderClaude, shared) {
		t.Fatal("exhausted Claude profile must stay exhausted")
	}

	got, err := scheduler.Pick([]account.Account{
		{ID: shared, Provider: account.ProviderCodex, AuthMode: account.AuthModeOAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != shared || got.Provider != account.ProviderCodex {
		t.Fatalf("got %q/%s, want the Codex account", got.ID, got.Provider)
	}
}

// Independent of ordering: a healthy Claude profile must survive an exhausted
// same-named Codex account (the reverse clobber direction).
func TestScoresAreIsolatedPerProviderReversed(t *testing.T) {
	const shared = "austin@manaflow.ai"
	scheduler := NewScheduler([]Score{
		{AccountID: shared, Provider: account.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
		{AccountID: shared, Provider: account.ProviderCodex, Headroom: 0, ShortHeadroom: 0},
	})

	if scheduler.Exhausted(account.ProviderClaude, shared) {
		t.Fatal("healthy Claude profile must not be exhausted by a same-named Codex account")
	}
	if !scheduler.Exhausted(account.ProviderCodex, shared) {
		t.Fatal("exhausted Codex account must stay exhausted")
	}
}

func TestLiveDebitsReorderPickWithoutExhausting(t *testing.T) {
	accountA := account.Account{ID: "a", Provider: account.ProviderClaude, AuthMode: account.AuthModeOAuth, Token: "x"}
	accountB := account.Account{ID: "b", Provider: account.ProviderClaude, AuthMode: account.AuthModeOAuth, Token: "x"}
	scheduler := NewScheduler([]Score{
		{AccountID: "a", Provider: account.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
		{AccountID: "b", Provider: account.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
	})

	// Equal scores: deterministic tie falls to "a".
	picked, err := scheduler.Pick([]account.Account{accountA, accountB})
	if err != nil || picked.ID != "a" {
		t.Fatalf("baseline pick = %v %v, want a", picked.ID, err)
	}

	// Five requests routed to "a" since the snapshot: the debit flips the pick.
	debited := scheduler.WithLiveDebits(map[string]int{ScoreKey(account.ProviderClaude, "a"): 5})
	picked, err = debited.Pick([]account.Account{accountA, accountB})
	if err != nil || picked.ID != "b" {
		t.Fatalf("debited pick = %v %v, want b", picked.ID, err)
	}

	// Even a huge debit never exhausts the account or resurrects an exhausted one.
	heavy := scheduler.WithLiveDebits(map[string]int{ScoreKey(account.ProviderClaude, "a"): 1000})
	if heavy.Exhausted(account.ProviderClaude, "a") {
		t.Fatal("a live debit must never mark an account exhausted")
	}
	cooked := NewScheduler([]Score{{AccountID: "a", Provider: account.ProviderClaude, Headroom: 0, ShortHeadroom: 0}}).
		WithLiveDebits(map[string]int{ScoreKey(account.ProviderClaude, "a"): 3})
	if !cooked.Exhausted(account.ProviderClaude, "a") {
		t.Fatal("a live debit must not resurrect an exhausted account")
	}
}

func TestLiveDebitsSurviveForModelAndSessionCounts(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "a", Provider: account.ProviderClaude, Headroom: 1, ShortHeadroom: 1,
			ModelScores: map[string]Score{"claudefable": {AccountID: "a", Provider: account.ProviderClaude, Headroom: 1, ShortHeadroom: 1}}},
		{AccountID: "b", Provider: account.ProviderClaude, Headroom: 1, ShortHeadroom: 1,
			ModelScores: map[string]Score{"claudefable": {AccountID: "b", Provider: account.ProviderClaude, Headroom: 1, ShortHeadroom: 1}}},
	}).WithLiveDebits(map[string]int{ScoreKey(account.ProviderClaude, "a"): 5}).
		WithSessionCounts(map[string]int{})
	accountA := account.Account{ID: "a", Provider: account.ProviderClaude, AuthMode: account.AuthModeOAuth, Token: "x"}
	accountB := account.Account{ID: "b", Provider: account.ProviderClaude, AuthMode: account.AuthModeOAuth, Token: "x"}
	picked, err := scheduler.ForModel("claude-fable").Pick([]account.Account{accountA, accountB})
	if err != nil || picked.ID != "b" {
		t.Fatalf("pool pick = %v %v, want b (debits must survive ForModel and WithSessionCounts)", picked.ID, err)
	}
}

func TestFablePickPrefersSoonerWeeklyReset(t *testing.T) {
	const fable = "claudefable"
	soon := ScoreFromLimitWindows("soon@example.com", 0, []LimitWindow{
		{Name: "5h", Feature: fable, UsedPercent: 10, LimitWindowSeconds: 5 * 60 * 60, ResetAfterSeconds: 5 * 60 * 60},
		{Name: "oauth-apps-weekly", Feature: fable, UsedPercent: 10, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAfterSeconds: 2 * 60 * 60},
	})
	soon.Provider = account.ProviderClaude
	later := ScoreFromLimitWindows("later@example.com", 0, []LimitWindow{
		{Name: "5h", Feature: fable, UsedPercent: 10, LimitWindowSeconds: 5 * 60 * 60, ResetAfterSeconds: 5 * 60 * 60},
		{Name: "oauth-apps-weekly", Feature: fable, UsedPercent: 10, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAfterSeconds: 5 * 24 * 60 * 60},
	})
	later.Provider = account.ProviderClaude
	scheduler := NewScheduler([]Score{later, soon}).ForModel(fable)
	picked, err := scheduler.Pick([]account.Account{
		{ID: "later@example.com", Provider: account.ProviderClaude, AuthMode: account.AuthModeOAuth, Token: "x"},
		{ID: "soon@example.com", Provider: account.ProviderClaude, AuthMode: account.AuthModeOAuth, Token: "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "soon@example.com" {
		t.Fatalf("picked %q, want sooner-resetting fable account", picked.ID)
	}
}

// Opus routing's weekly behavior is pinned in claude_pace_test.go
// (TestOpusTrafficPrefersFableConsumedAccount and friends): opus now follows
// weekly pace (with a fable reservation while PATCHES §57's switch is on)
// rather than ignoring weekly drain.

func TestSchedulerRefLiveDebitLifecycle(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.NoteRouted(account.ProviderClaude, "a")
	ref.NoteRouted(account.ProviderClaude, "a")
	ref.NoteRouted(account.ProviderCodex, "a")
	debits := ref.LiveDebits()
	if debits[ScoreKey(account.ProviderClaude, "a")] != 2 || debits[ScoreKey(account.ProviderCodex, "a")] != 1 {
		t.Fatalf("debits = %v", debits)
	}
	// A failed refresh keeps the debits (the stale snapshot still applies)...
	ref.FinishRefresh(Scheduler{}, false)
	if ref.LiveDebits() == nil {
		t.Fatal("failed refresh must keep live debits")
	}
	// ...a successful one supersedes them.
	ref.FinishRefresh(NewScheduler(nil), true)
	if ref.LiveDebits() != nil {
		t.Fatal("fresh refresh must clear live debits")
	}
}

// A usage reading served after its window's absolute reset describes a
// period that is over (upstream 4e80ede7, adapted: this fork has no
// WeeklyCooked). The stale 100% must neither exhaust the account nor bind
// its week.
func TestScoreFromLimitWindowsClearsExpiredAbsoluteReset(t *testing.T) {
	weekly := int64(7 * 24 * 60 * 60)
	now := time.Now()
	score := ScoreFromLimitWindowsAt("acct", 0, []LimitWindow{{
		Name: "7d", UsedPercent: 100, LimitWindowSeconds: weekly, ResetAfterSeconds: 60,
		ResetAt: now.Add(-time.Minute),
	}}, now)
	if score.exhausted() {
		t.Fatalf("expired weekly window stayed cooked: %+v", score)
	}
	if !score.WeeklyResetAt.IsZero() || score.WeeklyRemaining != 0 {
		t.Fatalf("expired weekly window still binds the week: %+v", score)
	}
	// Control: the same window before its reset is spent.
	live := ScoreFromLimitWindowsAt("acct", 0, []LimitWindow{{
		Name: "7d", UsedPercent: 100, LimitWindowSeconds: weekly, ResetAfterSeconds: 60,
		ResetAt: now.Add(time.Minute),
	}}, now)
	if !live.exhausted() {
		t.Fatalf("a live 100%% weekly window must exhaust the account: %+v", live)
	}
}

// An elapsed reading leaves the window set's shape alone: an account whose
// 5h reading is past its reset still publishes a 5h window, so it is not
// WeeklyOnly, and that window reads as refilled rather than aliased to the
// week.
func TestScoreFromLimitWindowsKeepsWindowShapePastAnElapsedReading(t *testing.T) {
	now := time.Now()
	score := ScoreFromLimitWindowsAt("acct", 0, []LimitWindow{
		{Name: "5h", UsedPercent: 100, LimitWindowSeconds: 5 * 60 * 60, ResetAfterSeconds: 60, ResetAt: now.Add(-time.Minute)},
		{Name: "7d", UsedPercent: 50, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAfterSeconds: 3 * 24 * 60 * 60},
	}, now)
	if score.exhausted() || score.ShortHeadroom != 1 {
		t.Fatalf("an elapsed 5h reading still holds the short window: %+v", score)
	}
	if score.WeeklyOnly {
		t.Fatalf("an elapsed 5h reading made the account weekly-only: %+v", score)
	}
	if score.Headroom != 0.5 || score.WeeklyRemaining != 0.5 {
		t.Fatalf("the live weekly reading was lost: %+v", score)
	}
}

// ExhaustionClearsAt (upstream #530) names when the measured windows stop
// holding a score exhausted: the latest reset among its exhausted windows,
// unknown when one of them reported none.
func TestExhaustionClearsAtFollowsTheLatestExhaustedWindowReset(t *testing.T) {
	now := time.Now()
	shortReset := now.Add(2 * time.Hour)
	weeklyReset := now.Add(3 * 24 * time.Hour)
	both := ScoreFromLimitWindowsAt("acct", 0, []LimitWindow{
		{Name: "5h", UsedPercent: 100, LimitWindowSeconds: 5 * 60 * 60, ResetAfterSeconds: 7200, ResetAt: shortReset},
		{Name: "7d", UsedPercent: 100, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAfterSeconds: 3 * 24 * 60 * 60, ResetAt: weeklyReset},
	}, now)
	if at, known := both.ExhaustionClearsAt(); !known || !at.Equal(weeklyReset) {
		t.Fatalf("ExhaustionClearsAt = %v, %v; want the weekly reset %v", at, known, weeklyReset)
	}
	unknown := ScoreFromLimitWindowsAt("acct", 0, []LimitWindow{
		{Name: "5h", UsedPercent: 100, LimitWindowSeconds: 5 * 60 * 60, ResetAfterSeconds: 7200},
	}, now)
	if _, known := unknown.ExhaustionClearsAt(); known {
		t.Fatalf("an exhausted window with no reset time must read unknown: %+v", unknown)
	}
	healthy := ScoreFromLimitWindowsAt("acct", 0, []LimitWindow{
		{Name: "5h", UsedPercent: 10, LimitWindowSeconds: 5 * 60 * 60, ResetAfterSeconds: 7200},
	}, now)
	if at, known := healthy.ExhaustionClearsAt(); !known || !at.IsZero() {
		t.Fatalf("a score that is not exhausted must clear now: %v, %v", at, known)
	}
}

// A long window is at least six days (upstream #379, adapted), the rule
// internal/accounts and sr status share. A mid-length cap (OpenRouter
// "daily", Kimi "1d") still blocks routing when it is spent, but it is not
// the week: read as one it would earn weekly pace and, rolling within a day,
// a permanent seat in the endgame drain.
func TestWeeklyWindowUsesSixDayLongWindowThreshold(t *testing.T) {
	day := int64(24 * 60 * 60)
	session := int64(5 * 60 * 60)
	now := time.Now()

	dailyCooked := ScoreFromLimitWindowsAt("acct", 0, []LimitWindow{
		{Name: "5h", UsedPercent: 10, LimitWindowSeconds: session, ResetAfterSeconds: 3600},
		{Name: "1d", UsedPercent: 100, LimitWindowSeconds: day, ResetAfterSeconds: 3600},
		{Name: "7d", UsedPercent: 50, LimitWindowSeconds: 7 * day, ResetAfterSeconds: 3 * day},
	}, now)
	if dailyCooked.WeeklyRemaining != 0.5 || !dailyCooked.WeeklyResetAt.Equal(now.Add(3*24*time.Hour)) {
		t.Fatalf("the daily window bound the week: %+v", dailyCooked)
	}
	if !dailyCooked.exhausted() {
		t.Fatalf("a spent daily window must still exhaust the account: %+v", dailyCooked)
	}

	dailyOnly := ScoreFromLimitWindowsAt("acct", 0, []LimitWindow{
		{Name: "daily", UsedPercent: 40, LimitWindowSeconds: day, ResetAfterSeconds: 3600},
	}, now)
	if !dailyOnly.WeeklyResetAt.IsZero() || dailyOnly.InEndgameWindow(now) {
		t.Fatalf("a daily-only account was paced as a week rolling within the day: %+v", dailyOnly)
	}

	for _, tc := range []struct {
		name   string
		length int64
		weekly bool
	}{
		{"6d", 6 * day, true},
		{"just under 6d", 6*day - 1, false},
		{"monthly", 30 * day, true},
	} {
		score := ScoreFromLimitWindowsAt("acct", 0, []LimitWindow{
			{Name: tc.name, UsedPercent: 50, LimitWindowSeconds: tc.length, ResetAfterSeconds: day},
		}, now)
		if got := !score.WeeklyResetAt.IsZero(); got != tc.weekly {
			t.Fatalf("%s window: weekly = %v, want %v (%+v)", tc.name, got, tc.weekly, score)
		}
	}
}

// Missing model buckets follow the score's declaration and, with none, this
// fork's provider table (upstream #467, adapted): Claude and Antigravity
// absence is unknown -- Opus and Sonnet route on the account-wide budget
// (PATCHES §24) -- and any other provider's absence is unsupported. An
// explicit declaration overrides the table either way.
func TestForModelMissingBucketsFollowDeclarationAndForkProviderTable(t *testing.T) {
	const model = "provider-model"
	measured := func(provider account.Provider) Score {
		return Score{AccountID: "measured", Provider: provider, Headroom: 0.8, ShortHeadroom: 0.8,
			ModelScores: map[string]Score{ModelKey(model): {AccountID: "measured", Provider: provider, Headroom: 0.2, ShortHeadroom: 0.2}}}
	}
	for _, tc := range []struct {
		name     string
		provider account.Provider
		declared ModelSupport
		eligible bool
	}{
		{"claude default", account.ProviderClaude, ModelSupportProviderDefault, true},
		{"claude declared unsupported", account.ProviderClaude, ModelSupportUnsupported, false},
		{"antigravity default", account.ProviderAntigravity, ModelSupportProviderDefault, true},
		{"codex default", account.ProviderCodex, ModelSupportProviderDefault, false},
		{"codex declared unknown", account.ProviderCodex, ModelSupportUnknown, true},
	} {
		absent := Score{AccountID: "absent", Provider: tc.provider, Headroom: 0.8, ShortHeadroom: 0.8, MissingModelSupport: tc.declared}
		scheduler := NewScheduler([]Score{absent, measured(tc.provider)}).ForModel(model)
		if got := !scheduler.Exhausted(tc.provider, "absent"); got != tc.eligible {
			t.Fatalf("%s: eligible = %v, want %v", tc.name, got, tc.eligible)
		}
		if got := scheduler.score(tc.provider, "measured").Headroom; got != 0.2 {
			t.Fatalf("%s: measured model bucket was not selected: headroom %v", tc.name, got)
		}
	}
	if DefaultMissingModelSupport(account.ProviderClaude) != ModelSupportUnknown {
		t.Fatal("Claude's missing buckets must default to unknown (PATCHES §24)")
	}
}
