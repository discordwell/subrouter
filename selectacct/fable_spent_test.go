package selectacct

import (
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/discordwell/subrouter/account"
)

// PATCHES §65: an Opus or Sonnet turn ranks an account's weekly pace at
// 1 + (FableSpentPreferenceRatio - 1) x FableSpent, so non-Fable work lands on
// accounts whose own Fable meter is spent and leaves Fable budget where it can
// still be used.

const reasonFableSpent = "higher weekly pace after the Fable-spent preference and the live-session discount"

// setFableSpentPreferenceForTest sets §65's process-wide switch for the rest
// of the test and restores it in t.Cleanup. Only for tests that are not
// t.Parallel.
func setFableSpentPreferenceForTest(t *testing.T, on bool) {
	t.Helper()
	prev := FableSpentPreference()
	SetFableSpentPreference(on)
	t.Cleanup(func() { SetFableSpentPreference(prev) })
}

// fableMeterWindows is a Claude account's reading as the proxy scores it: the
// 5h window, the general week rolling in roll seconds, and the Fable meter
// with its own reset, with the two account-wide windows copied into the Fable
// feature. Opus and Sonnet publish no pool of their own, so an Opus turn takes
// ForModel's early return. fableUsed 0 with fableReset 0 is the unused
// placeholder the proxy appends when Anthropic reports no Fable bucket.
func fableMeterWindows(generalUsed, fableUsed float64, roll, fableReset int64) []LimitWindow {
	const (
		fiveHour = int64(5 * 3600)
		sevenDay = int64(7 * 24 * 3600)
	)
	base := []LimitWindow{
		{Name: "5h", UsedPercent: 10, LimitWindowSeconds: fiveHour, ResetAfterSeconds: 2 * 3600},
		{Name: "7d", UsedPercent: generalUsed, LimitWindowSeconds: sevenDay, ResetAfterSeconds: roll},
	}
	windows := append([]LimitWindow(nil), base...)
	windows = append(windows, LimitWindow{Name: "oauth-apps-weekly", UsedPercent: fableUsed, LimitWindowSeconds: sevenDay,
		ResetAfterSeconds: fableReset, Feature: "claude-fable"})
	for _, b := range base {
		b.Feature = "claude-fable"
		windows = append(windows, b)
	}
	return windows
}

func fableScore(now time.Time, id string, windows []LimitWindow) Score {
	score := ScoreFromLimitWindowsAt(id, 0, windows, now)
	score.Provider = account.ProviderClaude
	score.Fresh = true
	return score
}

func explainedFirst(t *testing.T, scheduler Scheduler, accts ...account.Account) Explanation {
	t.Helper()
	explained, err := scheduler.Explain(accts)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	return explained[0]
}

// Equal weekly pace, both local, no hold: an Opus or Sonnet turn picks the
// account whose Fable meter is spent, and says why. Named so the id
// tie-break hands the pair to the untouched account when the preference is
// gone. Both shapes: no Opus pool (the live one, ForModel's early return) and
// an Opus pool published (the pool view must inherit the account's meter).
func TestAnOpusTurnPicksTheFableSpentAccountAtEqualPace(t *testing.T) {
	now := time.Now()
	roll := int64(72 * 3600)
	for _, shape := range []struct {
		name    string
		windows func(general, fable float64) []LimitWindow
	}{
		{"no opus pool", func(g, f float64) []LimitWindow { return fableMeterWindows(g, f, roll, roll) }},
		{"an opus pool", func(g, f float64) []LimitWindow { return claudePoolWindows(g, f, 10, roll, 2*3600) }},
	} {
		t.Run(shape.name, func(t *testing.T) {
			untouched := fableScore(now, "alpha", shape.windows(40, 0))
			spent := fableScore(now, "bravo", shape.windows(40, 100))
			accts := []account.Account{claudeAccount("alpha"), claudeAccount("bravo")}
			base := NewScheduler([]Score{untouched, spent})
			if got := explainedFirst(t, base, accts...); got.Account.ID != "alpha" {
				t.Fatalf("calibration: unscoped, the equal-pace tie must fall to alpha, got %q (%q)", got.Account.ID, got.Reason)
			}
			for _, model := range []string{"claude-opus", "claude-sonnet"} {
				scheduler := base.ForModel(model)
				picked, err := scheduler.Pick(accts)
				if err != nil {
					t.Fatalf("pick: %v", err)
				}
				got := explainedFirst(t, scheduler, accts...)
				if picked.ID != "bravo" || got.Account.ID != "bravo" || got.Reason != reasonFableSpent {
					t.Fatalf("%s: picked %q, explained %q (%q); want bravo, whose Fable meter is spent, for %q",
						model, picked.ID, got.Account.ID, got.Reason, reasonFableSpent)
				}
			}
		})
	}
}

// A factor, graded and bounded: a half-spent meter ranks at 1.5x, and an
// account with its Fable untouched still wins at more than twice the pace.
// The reason names the preference only where it decided.
func TestTheFableSpentPreferenceIsGradedAndBounded(t *testing.T) {
	now := time.Now()
	roll := int64(72 * 3600)
	accts := []account.Account{claudeAccount("alpha"), claudeAccount("bravo")}
	if got := fableScore(now, "x", fableMeterWindows(40, 50, roll, roll)).FableSpentFactor(); got != 1.5 {
		t.Fatalf("a half-spent Fable meter ranks at %v, want 1.5", got)
	}
	// Anthropic reports the Fable meter past its limit (116% seen live).
	if got := fableScore(now, "x", fableMeterWindows(40, 116, roll, roll)).FableSpentFactor(); got != 2 {
		t.Fatalf("a Fable meter read at 116%% ranks at %v, want the 2x of a fully spent one", got)
	}
	for _, tc := range []struct {
		name           string
		alphaRemaining float64 // general week left; its Fable is untouched
		bravoRemaining float64
		bravoFableUsed float64
		want, reason   string
	}{
		{"fully spent beats untouched at 1.9x its pace", 0.57, 0.30, 100, "bravo", reasonFableSpent},
		{"untouched more than twice as fast wins", 0.66, 0.30, 100, "alpha", "higher weekly pace after the live-session discount"},
		{"half spent beats untouched at 1.4x its pace", 0.42, 0.30, 50, "bravo", reasonFableSpent},
		{"untouched at 1.6x beats half spent", 0.48, 0.30, 50, "alpha", "higher weekly pace after the live-session discount"},
		{"spent and faster: the pace decides, not the preference", 0.30, 0.40, 100, "bravo", "higher weekly pace after the live-session discount"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			alpha := fableScore(now, "alpha", fableMeterWindows(100-tc.alphaRemaining*100, 0, roll, roll))
			bravo := fableScore(now, "bravo", fableMeterWindows(100-tc.bravoRemaining*100, tc.bravoFableUsed, roll, roll))
			got := explainedFirst(t, NewScheduler([]Score{alpha, bravo}).ForModel("claude-opus"), accts...)
			if got.Account.ID != tc.want || got.Reason != tc.reason {
				t.Fatalf("ranked %q first (%q), want %q (%q)", got.Account.ID, got.Reason, tc.want, tc.reason)
			}
		})
	}
}

// Unknown is neutral, and the meter is the Fable meter alone: no Fable
// window, a Codex account, the unused placeholder and a Fable meter untouched
// behind a general week 85% spent all read 0 and rank at 1x; the last two
// lose to an equal-pace account with half its Fable spent.
func TestAnUnknownOrUntouchedFableMeterRanksAt1x(t *testing.T) {
	now := time.Now()
	roll := int64(72 * 3600)
	codex := ScoreFromLimitWindowsAt("cx", 0, []LimitWindow{{Name: "primary", UsedPercent: 60, LimitWindowSeconds: 7 * 24 * 3600, ResetAfterSeconds: roll}}, now)
	codex.Provider = account.ProviderCodex
	for _, tc := range []struct {
		name    string
		score   Score
		general float64 // > 0: ranked against an equal-pace account with half its Fable spent
	}{
		{"no Fable window", fableScore(now, "alpha", fableMeterWindows(40, 0, roll, roll)[:2]), 0},
		{"a Codex account", codex, 0},
		{"the unused placeholder", fableScore(now, "alpha", fableMeterWindows(40, 0, roll, 0)), 40},
		{"untouched Fable behind a general week 85% spent", fableScore(now, "alpha", fableMeterWindows(85, 0, roll, roll)), 85},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.score.FableSpent != 0 || tc.score.FableSpentFactor() != 1 {
				t.Fatalf("FableSpent %v factor %v, want 0 and 1: an unknown or untouched meter is neutral",
					tc.score.FableSpent, tc.score.FableSpentFactor())
			}
			if tc.general == 0 {
				return
			}
			half := fableScore(now, "bravo", fableMeterWindows(tc.general, 50, roll, roll))
			accts := []account.Account{claudeAccount("alpha"), claudeAccount("bravo")}
			got := explainedFirst(t, NewScheduler([]Score{tc.score, half}).ForModel("claude-opus"), accts...)
			if got.Account.ID != "bravo" {
				t.Fatalf("ranked %q first (%q), want bravo: half its Fable spent against a neutral 1x", got.Account.ID, got.Reason)
			}
		})
	}
}

// A Fable meter whose reported reset (ResetAt) has passed is a reading from
// before its roll: the meter has refilled, and the Fable pool's own score
// already skips it (liveWindow, from the 2026-11 upstream merge). §65 must read
// it the same way, or an Opus turn keeps preferring, at up to 2x, an account
// whose Fable budget is whole again. The same 100% with its reset still ahead
// reads fully spent.
func TestAFableMeterPastItsResetIsNotSpent(t *testing.T) {
	now := time.Now()
	roll := int64(72 * 3600)
	meter := func(resetAt time.Time) []LimitWindow {
		windows := fableMeterWindows(40, 100, roll, roll)
		for i := range windows {
			if windows[i].Name == "oauth-apps-weekly" {
				windows[i].ResetAt = resetAt
			}
		}
		return windows
	}
	if live := fableScore(now, "alpha", meter(now.Add(time.Hour))); live.FableSpent != 1 {
		t.Fatalf("a 100%% Fable meter resetting in an hour reads FableSpent %v, want 1", live.FableSpent)
	}
	elapsed := fableScore(now, "alpha", meter(now.Add(-time.Minute)))
	if elapsed.FableSpent != 0 || elapsed.FableSpentFactor() != 1 {
		t.Fatalf("a 100%% Fable meter whose reset passed a minute ago reads FableSpent %v factor %v, want 0 and 1",
			elapsed.FableSpent, elapsed.FableSpentFactor())
	}
}

// Two accounts draining inside their 24h roll order on urgency and then raw
// stake, as before: the preference rides the pace clause only, which a
// draining pair skips.
func TestTwoDrainingAccountsIgnoreTheFableSpentPreference(t *testing.T) {
	now := time.Now()
	roll := int64(16 * 3600)
	untouched := fableScore(now, "alpha", fableMeterWindows(40, 0, roll, roll))
	spent := fableScore(now, "bravo", fableMeterWindows(40, 100, roll, roll))
	scheduler := NewScheduler([]Score{untouched, spent}).ForModel("claude-opus")
	if !scheduler.score(account.ProviderClaude, "alpha").endgameDrainEligible(now) ||
		!scheduler.score(account.ProviderClaude, "bravo").endgameDrainEligible(now) {
		t.Fatal("calibration: both accounts must be draining")
	}
	got := explainedFirst(t, scheduler, claudeAccount("alpha"), claudeAccount("bravo"))
	if got.Account.ID != "alpha" {
		t.Fatalf("ranked %q first (%q), want alpha on the id tie: a draining pair ranks on raw stake", got.Account.ID, got.Reason)
	}
}

// randomFablePool is a random pool for the §65 invariants: Claude and Codex,
// local and peer (half the peers with a verdict), deadlines across and outside
// the 24h window, owner reserves, and Fable meters from untouched to spent.
func randomFablePool(rng *rand.Rand, now time.Time, count int) ([]account.Account, []Score, map[string]float64) {
	accts := make([]account.Account, 0, count)
	scores := make([]Score, 0, count)
	reserves := map[string]float64{}
	for i := 0; i < count; i++ {
		id := "acct-" + strconv.Itoa(i)
		provider := account.ProviderClaude
		if rng.Intn(5) == 0 {
			provider = account.ProviderCodex
		}
		short := 0.02 + rng.Float64()*0.98
		remaining := rng.Float64()
		score := Score{
			AccountID: id, Provider: provider, Fresh: true,
			Headroom: math.Max(0.01, math.Min(short, remaining)), ShortHeadroom: short,
			ShortWindowSeconds: 18000, ShortResetAfterSeconds: int64(rng.Intn(18001)),
			WeeklyHeadroom: remaining, WeeklyRemaining: remaining,
			LiveSessions: rng.Intn(4),
		}
		if provider == account.ProviderCodex {
			score.WeeklyOnly, score.ShortWindowSeconds, score.ShortResetAfterSeconds = true, 0, 0
		} else if rng.Intn(4) > 0 {
			score.FableSpent = []float64{0.1, 0.5, 1, rng.Float64()}[rng.Intn(4)]
		}
		if rng.Intn(5) > 0 {
			score.WeeklyResetAt = now.Add(time.Duration(rng.Intn(7*24*3600)) * time.Second)
		}
		if rng.Intn(4) == 0 {
			reserves[id] = rng.Float64() * 0.3
		}
		if rng.Intn(4) == 0 {
			score.Remote = true
			if rng.Intn(2) == 0 {
				score.PeerVerdict = &NewSessionVerdict{Eligible: rng.Intn(2) == 0, Reason: VerdictEligible, LiveSessions: score.LiveSessions}
			}
		}
		accts = append(accts, account.Account{ID: id, Provider: provider, AuthMode: account.AuthModeOAuth})
		scores = append(scores, score)
	}
	return accts, scores, reserves
}

// fableRankingFingerprint is both orderings of a scoped pool with the clause
// behind each step.
func fableRankingFingerprint(t *testing.T, scheduler Scheduler, accts []account.Account, now time.Time) string {
	t.Helper()
	var b strings.Builder
	for _, allowEndgame := range []bool{true, false} {
		sorted, err := scheduler.rankAt(accts, allowEndgame, now)
		if err != nil {
			t.Fatalf("rank: %v", err)
		}
		for i, acct := range sorted {
			b.WriteString(acct.ID)
			if i+1 < len(sorted) {
				_, reason := scheduler.compare(acct, sorted[i+1], now, allowEndgame)
				b.WriteString(" (" + reason + ") ")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// gateFingerprint is everything the preference must never move: per account,
// the verdict (eligibility, burndown, the allowed-session taper, the reserve),
// the weekly figures, and the gates as the comparator reads them.
func gateFingerprint(scheduler Scheduler, accts []account.Account, now time.Time) string {
	var b strings.Builder
	for _, acct := range accts {
		score := scheduler.score(acct.Provider, acct.ID)
		fmt.Fprintf(&b, "%s %+v headroom=%v remaining=%v allowed=%d reserve=%v routable=%v/%v\n",
			acct.ID, scheduler.NewSessionVerdict(acct.Provider, acct.ID, now),
			score.WeeklyHeadroom, score.WeeklyRemaining, score.endgameAllowedSessions(time.Now()), score.belowWeeklyReserve(),
			score.routableForNewSession(now, true), score.routableForNewSession(now, false))
	}
	return b.String()
}

// A Fable turn, an unscoped pick, a Haiku turn and a Codex turn rank every
// pool exactly as they would with no Fable meter read, and so does an Opus or
// Sonnet turn with SUBROUTER_FABLE_SPENT_PREFERENCE=0. With the switch on or
// off, no turn's verdicts, weekly figures, taper or reserve gate move.
// Calibrated by the Opus and Sonnet turns with the switch on: the same pools
// must rank differently somewhere, or this parity proves nothing.
func TestOnlyAnOpusOrSonnetTurnRanksOnTheFableMeter(t *testing.T) {
	setFableSpentPreferenceForTest(t, true)
	rng := rand.New(rand.NewSource(65))
	now := time.Now()
	moved := map[string]int{}
	for trial := 0; trial < 1500; trial++ {
		accts, scores, reserves := randomFablePool(rng, now, 2+rng.Intn(5))
		stripped := make([]Score, len(scores))
		for i, score := range scores {
			score.FableSpent = 0
			stripped[i] = score
		}
		with := NewScheduler(scores).WithReserves(reserves)
		without := NewScheduler(stripped).WithReserves(reserves)
		for _, model := range []string{"", "claude-fable", "claude-haiku-4-5", "haiku", "gpt-5.3-codex", "claude-opus", "claude-sonnet"} {
			nonFable := model == "claude-opus" || model == "claude-sonnet"
			for _, on := range []bool{true, false} {
				SetFableSpentPreference(on)
				scoped, baseline := with.ForModel(model), without.ForModel(model)
				if got, want := gateFingerprint(scoped, accts, now), gateFingerprint(baseline, accts, now); got != want {
					t.Fatalf("trial %d model %q switch %v: the Fable meter moved a verdict or a gate:\nwith:\n%s\nwithout:\n%s",
						trial, model, on, got, want)
				}
				got, want := fableRankingFingerprint(t, scoped, accts, now), fableRankingFingerprint(t, baseline, accts, now)
				if nonFable && on {
					if got != want {
						moved[model]++
					}
					continue
				}
				if got != want {
					t.Fatalf("trial %d model %q switch %v: the Fable meter changed the ranking:\nwith:\n%s\nwithout:\n%s",
						trial, model, on, got, want)
				}
			}
		}
	}
	SetFableSpentPreference(true)
	if moved["claude-opus"] == 0 || moved["claude-sonnet"] == 0 {
		t.Fatalf("calibration: with the switch on an Opus or Sonnet turn never ranked differently (%v); the parity above proves nothing", moved)
	}
}
