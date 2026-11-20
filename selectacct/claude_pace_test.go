package selectacct

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

// claudePoolWindows builds the window set a real Claude account produces after
// the proxy's base-window duplication: account-wide 5h/7d windows, the three
// feature weeklies (fable meter carries real usage; opus/sonnet report 0%),
// and a copy of each base window inside every feature pool.
func claudePoolWindows(generalUsedPct, fableUsedPct, shortUsedPct float64, weeklyResetSeconds, shortResetSeconds int64) []LimitWindow {
	const (
		fiveHour = int64(5 * 60 * 60)
		sevenDay = int64(7 * 24 * 60 * 60)
	)
	base := []LimitWindow{
		{Name: "5h", UsedPercent: shortUsedPct, LimitWindowSeconds: fiveHour, ResetAfterSeconds: shortResetSeconds},
		{Name: "7d", UsedPercent: generalUsedPct, LimitWindowSeconds: sevenDay, ResetAfterSeconds: weeklyResetSeconds},
	}
	features := []struct {
		name    string
		feature string
		used    float64
	}{
		{"oauth-apps-weekly", "claude-fable", fableUsedPct},
		{"opus-weekly", "claude-opus", 0},
		{"sonnet-weekly", "claude-sonnet", 0},
	}
	windows := append([]LimitWindow(nil), base...)
	for _, f := range features {
		windows = append(windows, LimitWindow{Name: f.name, UsedPercent: f.used, LimitWindowSeconds: sevenDay, ResetAfterSeconds: weeklyResetSeconds, Feature: f.feature})
		for _, b := range base {
			dup := b
			dup.Feature = f.feature
			windows = append(windows, dup)
		}
	}
	return windows
}

func claudeScore(id string, windows []LimitWindow) Score {
	score := ScoreFromLimitWindows(id, 0, windows)
	score.Provider = account.ProviderClaude
	return score
}

func claudeAccount(id string) account.Account {
	return account.Account{ID: id, Provider: account.ProviderClaude, AuthMode: account.AuthModeOAuth, Token: "x"}
}

func pickID(t *testing.T, scheduler Scheduler, ids ...string) string {
	t.Helper()
	accts := make([]account.Account, 0, len(ids))
	for _, id := range ids {
		accts = append(accts, claudeAccount(id))
	}
	picked, err := scheduler.Pick(accts)
	if err != nil {
		t.Fatal(err)
	}
	return picked.ID
}

const (
	fourDays    = int64(4 * 24 * 60 * 60)
	sixDays     = int64(6 * 24 * 60 * 60)
	twoHours    = int64(2 * 60 * 60)
	twentyHours = int64(20 * 60 * 60)
)

// Fable traffic goes to the account with the most fable allowance left.
func TestFableTrafficPrefersFableRichAccount(t *testing.T) {
	rich := claudeScore("rich", claudePoolWindows(50, 10, 10, fourDays, twoHours))
	poor := claudeScore("poor", claudePoolWindows(50, 80, 10, fourDays, twoHours))
	scheduler := NewScheduler([]Score{poor, rich}).ForModel("claude-fable")
	if got := pickID(t, scheduler, "poor", "rich"); got != "rich" {
		t.Fatalf("fable pick = %q, want the fable-rich account", got)
	}
}

// Opus traffic goes to the account whose fable slice is already spent, so
// fable capacity is not stranded behind a drained general budget. That is the
// Fable reservation, off by default since PATCHES §57, so this turns it on.
// It pins the reservation's direction, not its presence: with it off both read
// 0.5 and the ID tie-break still picks consumed (TestClaudeFableReservationArithmetic
// pins the presence).
func TestOpusTrafficPrefersFableConsumedAccount(t *testing.T) {
	setFableReservationForTest(t, true)
	consumed := claudeScore("consumed", claudePoolWindows(50, 90, 10, fourDays, twoHours))
	preserved := claudeScore("preserved", claudePoolWindows(50, 0, 10, fourDays, twoHours))
	scheduler := NewScheduler([]Score{preserved, consumed}).ForModel("claude-opus")
	if got := pickID(t, scheduler, "preserved", "consumed"); got != "consumed" {
		t.Fatalf("opus pick = %q, want the fable-consumed account", got)
	}
}

// Weekly pace, not 5h-reset timing, orders Claude accounts: an account with
// far more weekly budget left wins even when the other's 5h window resets
// much sooner (the old pressure ordering picked by short-reset timing).
func TestClaudeWeeklyPaceOutranksShortWindowTiming(t *testing.T) {
	slowPace := claudeScore("slow-pace", claudePoolWindows(70, 50, 10, 5*24*60*60, 600))
	fastPace := claudeScore("fast-pace", claudePoolWindows(20, 50, 10, 5*24*60*60, 17000))
	for _, model := range []string{"claude-opus", "claude-fable"} {
		scheduler := NewScheduler([]Score{slowPace, fastPace}).ForModel(model)
		if got := pickID(t, scheduler, "slow-pace", "fast-pace"); got != "fast-pace" {
			t.Fatalf("%s pick = %q, want weekly pace to outrank 5h timing", model, got)
		}
	}
}

// An account within a day of its weekly roll drains first, even when it is
// below the new-session headroom threshold and a fresher account exists.
func TestClaudeEndgameAccountDrainsFirst(t *testing.T) {
	endgame := claudeScore("endgame", claudePoolWindows(70, 70, 10, twentyHours, twoHours))
	fresh := claudeScore("fresh", claudePoolWindows(10, 10, 10, sixDays, twoHours))
	scheduler := NewScheduler([]Score{fresh, endgame}).ForModel("claude-opus")
	// The endgame account holds only 30% of its week but a 90%-fresh 5h
	// window, so it IS usable for a new session: eligibility keys on the short
	// window, and the weekly level decides preference, not permission. What
	// this test pins is that the endgame ordering still puts it first.
	if !scheduler.UsableForNewSession(account.ProviderClaude, "endgame") {
		t.Fatal("fixture broken: a fresh 5h window should be usable regardless of weekly level")
	}
	if got := pickID(t, scheduler, "fresh", "endgame"); got != "endgame" {
		t.Fatalf("pick = %q, want the endgame account despite low headroom", got)
	}
}

// With several accounts draining, the earliest weekly roll exhausts first,
// even when a later-rolling one has more budget left.
func TestClaudeEndgamePrefersEarlierReset(t *testing.T) {
	early := claudeScore("early", claudePoolWindows(80, 80, 10, 10*60*60, twoHours))
	late := claudeScore("late", claudePoolWindows(40, 40, 10, twentyHours, twoHours))
	scheduler := NewScheduler([]Score{late, early}).ForModel("claude-opus")
	if got := pickID(t, scheduler, "late", "early"); got != "early" {
		t.Fatalf("pick = %q, want the earlier-rolling account", got)
	}
}

// The drain-mode concurrency taper: a nearly-spent endgame account admits one
// session; at its cap, traffic spills to the next-draining account, and with
// every endgame account at cap it spills to steady-state accounts.
func TestClaudeEndgameTaperSpillsWhenBusy(t *testing.T) {
	eg1 := claudeScore("eg1", claudePoolWindows(80, 100, 10, twentyHours, twoHours))
	eg2 := claudeScore("eg2", claudePoolWindows(70, 100, 10, 22*60*60, twoHours))
	normal := claudeScore("normal", claudePoolWindows(10, 10, 10, sixDays, twoHours))
	base := NewScheduler([]Score{eg1, eg2, normal}).ForModel("claude-opus")

	idle := base.WithLiveSessionCounts(map[string]int{})
	if got := pickID(t, idle, "eg1", "eg2", "normal"); got != "eg1" {
		t.Fatalf("idle pick = %q, want the earliest-rolling endgame account", got)
	}
	// Fill each account to ITS OWN allowance rather than a hardcoded count:
	// the taper step sizes that allowance, so a literal here silently stops
	// meaning "at cap" the moment the step is retuned.
	eg1Cap := base.score(eg1.Provider, "eg1").endgameAllowedSessions()
	eg2Cap := base.score(eg2.Provider, "eg2").endgameAllowedSessions()
	eg1Busy := base.WithLiveSessionCounts(map[string]int{"eg1": eg1Cap})
	if got := pickID(t, eg1Busy, "eg1", "eg2", "normal"); got != "eg2" {
		t.Fatalf("eg1-at-cap pick = %q, want spill to the next endgame account", got)
	}
	bothBusy := base.WithLiveSessionCounts(map[string]int{"eg1": eg1Cap, "eg2": eg2Cap})
	if got := pickID(t, bothBusy, "eg1", "eg2", "normal"); got != "normal" {
		t.Fatalf("all-at-cap pick = %q, want spill to the steady-state account", got)
	}
	// The taper runs on LIVE concurrency only. Parked-session counts (the
	// session store's all-time assignments, which never shrink) must not
	// gate it — that conflation was the bug that kept endgame drain
	// permanently off on any pool more than a day old.
	parked := base.WithSessionCounts(map[string]int{"eg1": 14, "eg2": 11}).
		WithLiveSessionCounts(map[string]int{})
	if got := pickID(t, parked, "eg1", "eg2", "normal"); got != "eg1" {
		t.Fatalf("parked-heavy pick = %q, want lifetime counts to leave the taper alone", got)
	}
}

// Inside the endgame window the fable reservation fades, so a fable-rich
// account still receives opus traffic rather than expiring with unspent
// general budget held back for fable work that may never come. With the
// reservation on (PATCHES §57; off by default).
func TestClaudeEndgameFableReservationFades(t *testing.T) {
	setFableReservationForTest(t, true)
	endgameRich := claudeScore("endgame-rich", claudePoolWindows(50, 0, 10, 12*60*60, twoHours))
	midweek := claudeScore("midweek", claudePoolWindows(10, 10, 10, sixDays, twoHours))
	scheduler := NewScheduler([]Score{midweek, endgameRich}).ForModel("claude-opus")
	if got := pickID(t, scheduler, "midweek", "endgame-rich"); got != "endgame-rich" {
		t.Fatalf("pick = %q, want the endgame account despite its rich fable slice", got)
	}
	// The decay itself is pinned by TestClaudeFableReservationArithmetic. A
	// pick here at one live session cannot fail at the 0.0625 step: 0.375 and
	// a neutralised 0.25 both allow 4.
}

// Direct wiring assertions on the reservation arithmetic, so an inverted
// sign or a dropped decay cannot hide behind coarse pick outcomes: general
// 50% used, fable untouched -> reachable fable = min(1.0, 0.5) = 0.5, so the
// opus pool reserves 0.5*0.5*decay of the weekly budget. With the
// reservation on (PATCHES §57); TestFableReservationIsOffByDefault pins the
// default.
func TestClaudeFableReservationArithmetic(t *testing.T) {
	setFableReservationForTest(t, true)
	midweek := claudeScore("midweek", claudePoolWindows(50, 0, 10, fourDays, twoHours))
	if got := midweek.ModelScores["claudeopus"].WeeklyHeadroom; got != 0.25 {
		t.Fatalf("midweek opus spendable = %v, want 0.25 (full reservation)", got)
	}
	if got := midweek.ModelScores["claudefable"].WeeklyHeadroom; got != 0.5 {
		t.Fatalf("fable spendable = %v, want 0.5 (no reservation against itself)", got)
	}
	endgame := claudeScore("endgame", claudePoolWindows(50, 0, 10, 12*60*60, twoHours))
	if got := endgame.ModelScores["claudeopus"].WeeklyHeadroom; got != 0.375 {
		t.Fatalf("12h-to-roll opus spendable = %v, want 0.375 (half-decayed reservation)", got)
	}
}

// The 5h window still gates: a cooked short window makes the account
// exhausted for its pools no matter how good its weekly pace looks.
func TestClaudeFiveHourExhaustionStillGates(t *testing.T) {
	cooked := claudeScore("cooked", claudePoolWindows(10, 10, 100, fourDays, twoHours))
	steady := claudeScore("steady", claudePoolWindows(70, 70, 10, fourDays, twoHours))
	scheduler := NewScheduler([]Score{steady, cooked}).ForModel("claude-fable")
	if !scheduler.Exhausted(account.ProviderClaude, "cooked") {
		t.Fatal("cooked 5h window must exhaust the account's pools")
	}
	if got := pickID(t, scheduler, "cooked", "steady"); got != "steady" {
		t.Fatalf("pick = %q, want the account with 5h budget left", got)
	}
}

// An endgame account that is still above the usable threshold but at its
// session allowance must not win on pace — its near-roll reset makes pace
// enormous, and without the at-cap demotion every new session would dogpile
// it. At cap it ranks below steady-state accounts.
func TestClaudeAtCapEndgameDemotedBelowSteady(t *testing.T) {
	endgame := claudeScore("endgame", claudePoolWindows(50, 100, 10, twentyHours, twoHours))
	steady := claudeScore("steady", claudePoolWindows(10, 10, 10, sixDays, twoHours))
	base := NewScheduler([]Score{steady, endgame}).ForModel("claude-opus")
	idle := base.WithLiveSessionCounts(map[string]int{})
	if got := pickID(t, idle, "steady", "endgame"); got != "endgame" {
		t.Fatalf("idle pick = %q, want the endgame account", got)
	}
	// At its allowance it is capped and must lose to the steady account
	// despite ~8x the pace. Read the allowance rather than restating it: it is
	// a function of the taper step, not a constant.
	cap := base.score(endgame.Provider, "endgame").endgameAllowedSessions()
	capped := base.WithLiveSessionCounts(map[string]int{"endgame": cap})
	if got := pickID(t, capped, "steady", "endgame"); got != "steady" {
		t.Fatalf("at-cap pick = %q, want the steady account", got)
	}
}

// A Claude account whose usage response carries no fable window (the
// upstream reports feature buckets as null until first use, and the probe
// can decline) must still be recognized as Claude-family: opus/sonnet
// windows are enough. It gets no reservation (meter unknown) and competes on
// pace instead of sorting below every pace-scored sibling.
func TestClaudeFamilyDetectedWithoutFableWindow(t *testing.T) {
	const (
		fiveHour = int64(5 * 60 * 60)
		sevenDay = int64(7 * 24 * 60 * 60)
	)
	base := []LimitWindow{
		{Name: "5h", UsedPercent: 10, LimitWindowSeconds: fiveHour, ResetAfterSeconds: twoHours},
		{Name: "7d", UsedPercent: 40, LimitWindowSeconds: sevenDay, ResetAfterSeconds: fourDays},
	}
	windows := append([]LimitWindow(nil), base...)
	for _, feature := range []string{"claude-opus", "claude-sonnet"} {
		windows = append(windows, LimitWindow{Name: feature + "-weekly", LimitWindowSeconds: sevenDay, ResetAfterSeconds: fourDays, Feature: feature})
		for _, b := range base {
			dup := b
			dup.Feature = feature
			windows = append(windows, dup)
		}
	}
	// "Reserves nothing" is a claim about the Fable reservation, which is
	// off by default since PATCHES §57: with it off 0.6 cannot fail.
	setFableReservationForTest(t, true)
	noFable := claudeScore("no-fable", windows)
	if noFable.WeeklyResetAt.IsZero() {
		t.Fatal("account without a fable window fell out of the pace class")
	}
	if got := noFable.ModelScores["claudeopus"].WeeklyHeadroom; got != 0.6 {
		t.Fatalf("opus spendable = %v, want 0.6 (unknown fable meter reserves nothing)", got)
	}
	// And it competes on pace: with the better weekly pace it must win the
	// pick against a fully-reported sibling, not sort below it by class.
	sibling := claudeScore("sibling", claudePoolWindows(80, 90, 10, fourDays, twoHours))
	scheduler := NewScheduler([]Score{sibling, noFable}).ForModel("claude-opus")
	if got := pickID(t, scheduler, "sibling", "no-fable"); got != "no-fable" {
		t.Fatalf("pick = %q, want the fable-less account to compete on pace", got)
	}
}

// A carried seed whose weekly deadline has already passed in real time
// (WeeklyResetAt is absolute) drops out of pace ordering and endgame
// eligibility instead of riding a frozen countdown.
func TestClaudeStaleSeedDeadlineExpires(t *testing.T) {
	stale := Score{
		AccountID: "stale", Provider: account.ProviderClaude,
		Headroom: 0.2, ShortHeadroom: 0.9,
		WeeklyHeadroom: 0.2, WeeklyResetAt: time.Now().Add(-2 * time.Hour),
	}
	fresh := claudeScore("fresh", claudePoolWindows(50, 50, 10, fourDays, twoHours))
	scheduler := NewScheduler([]Score{stale, fresh})
	// The stale account is below the usable threshold and its endgame window
	// has lapsed, so it must not be endgame-widened past the fresh account.
	if got := pickID(t, scheduler, "stale", "fresh"); got != "fresh" {
		t.Fatalf("pick = %q, want the fresh account over a lapsed endgame seed", got)
	}
}

// PickSteady serves consumers choosing a long-lived interactive default (sr
// rotation): endgame widening and drain-first precedence are disabled, so a
// sub-threshold draining account is never picked while a usable one exists.
func TestPickSteadyIgnoresEndgameDrain(t *testing.T) {
	endgame := claudeScore("endgame", claudePoolWindows(70, 70, 10, twentyHours, twoHours))
	fresh := claudeScore("fresh", claudePoolWindows(10, 10, 10, sixDays, twoHours))
	scheduler := NewScheduler([]Score{fresh, endgame}).ForModel("claude-opus")
	if got := pickID(t, scheduler, "fresh", "endgame"); got != "endgame" {
		t.Fatalf("Pick = %q, want the endgame account for request routing", got)
	}
	steady, err := scheduler.PickSteady([]account.Account{claudeAccount("fresh"), claudeAccount("endgame")})
	if err != nil {
		t.Fatal(err)
	}
	if steady.ID != "fresh" {
		t.Fatalf("PickSteady = %q, want the usable account for the interactive default", steady.ID)
	}
}

// Codex scores carry no weekly pacing fields, so the legacy ExpiryPressure
// ordering still decides between them (pinned separately by the GTO and
// velocity simulations; this is the direct guard).
// An account publishing NO weekly window keeps the legacy short-expiry
// ordering: sooner reset first.
//
// This was named TestCodexOrderingUnchangedByWeeklyPace and described its
// fixture as "codex-shaped", which was wrong twice over. Codex has never
// published a bare 5h window -- before 2026-07-12 it sent primary=5h plus
// secondary=7d, and since then it sends a lone 604800s primary and no short
// window at all -- and since 2026-08-11 a real Codex account is paced on its
// weekly window exactly like Claude. The fixture is a no-weekly-window
// account, so that is what the name now says. Real Codex ordering is pinned
// by TestCodexPrefersTheSoonestRefresh in codex_weekly_test.go.
func TestNoWeeklyWindowKeepsShortExpiryOrdering(t *testing.T) {
	soon := ScoreFromLimitWindows("soon", 0, []LimitWindow{
		{Name: "5h", UsedPercent: 40, LimitWindowSeconds: 5 * 60 * 60, ResetAfterSeconds: 600},
	})
	later := ScoreFromLimitWindows("later", 0, []LimitWindow{
		{Name: "5h", UsedPercent: 40, LimitWindowSeconds: 5 * 60 * 60, ResetAfterSeconds: 17000},
	})
	if !soon.WeeklyResetAt.IsZero() || !later.WeeklyResetAt.IsZero() {
		t.Fatal("an account with no weekly window must not grow weekly pacing fields")
	}
	scheduler := NewScheduler([]Score{later, soon})
	picked, err := scheduler.Pick([]account.Account{
		{ID: "later", AuthMode: account.AuthModeOAuth, Token: "x"},
		{ID: "soon", AuthMode: account.AuthModeOAuth, Token: "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "soon" {
		t.Fatalf("picked %q, want the legacy pressure ordering (sooner reset first)", picked.ID)
	}
}
