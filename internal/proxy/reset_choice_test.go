package proxy

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

// PATCHES §95.1 on facts: which of a daemon's accounts are chosen to run to
// their limit for their next saved reset, and in what order. Each case builds
// usage-status rows as the daemon's pass reads them (meters, resets and
// figures) and weighs them against the pool built from the same rows.

// choiceNow is the clock of the choice's own tests.
var choiceNow = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

// choiceAccount is one account's row at a pass: its meters read a minute
// before, with week and fable left and rolling toRoll after the pass; the
// reset it holds next, of kind ("" holds none) and expiring expires after the
// pass; and its own 24-hour burn on each meter (0 for none). A Claude account
// unless provider says Codex.
type choiceAccount struct {
	id              string
	provider        accounts.Provider
	kind            accounts.ResetKind
	week, fable     float64
	toRoll, expires time.Duration
	weekBurn        float64
	fableBurn       float64
}

func (a choiceAccount) row(now time.Time) AccountUsageStatus {
	provider := a.provider
	if provider == "" {
		provider = accounts.ProviderClaude
	}
	at := now.Add(-time.Minute)
	roll := now.Add(a.toRoll)
	row := AccountUsageStatus{
		AccountStatus: AccountStatus{ID: a.id, Provider: provider, AuthMode: accounts.AuthModeOAuth, Label: a.id},
		UsageFresh:    true, UsageMeasuredAt: at, Resets: []accounts.HeldReset{},
	}
	if provider == accounts.ProviderCodex {
		row.Windows = codexWeekRead(100*(1-a.week), int64(roll.Sub(at)/time.Second), at)
	} else {
		row.Windows = append(claudeWeek(100*(1-a.week), roll), accounts.UsageWindow{Name: agentclaude.FableWindowName,
			UsedPercent: 100 * (1 - a.fable), LimitWindowSeconds: 7 * 24 * 3600, ResetAt: roll})
	}
	burn := UsageBurn{}
	for meter, rate := range map[string]float64{usageMeterWeek: a.weekBurn, usageMeterFable: a.fableBurn} {
		if rate > 0 {
			burn[meter] = UsageMeterBurn{"24h": {Burn: figure(rate), Covered: 1}}
		}
	}
	if len(burn) > 0 {
		row.Burn = burn
	}
	if a.kind == "" {
		return row
	}
	entry := accounts.HeldReset{Kind: a.kind, Count: 1, ExpiresAt: now.Add(a.expires).UTC().Format(time.RFC3339), Expiry: accounts.ResetExpiryRead}
	if spec, ok := accounts.ResetKindNamed(provider, a.kind); ok {
		entry.RestartsClock, entry.Order = spec.RestartsClock, 1
	} else {
		entry.Kind, entry.RawType, entry.RestartsClock, entry.ManualOnly = accounts.ResetKindUnknown, string(a.kind), accounts.ClockRestartUnknown, true
	}
	row.Resets = append(row.Resets, entry)
	return row
}

// choiceResult is one pass's choice, by account ID.
type choiceResult struct {
	chosen map[string]ResetChoice
	why    map[string]string
	hold   resetCodexHold
}

// choose weighs the accounts at now against the pool their rows make, and
// chooses as a pass would after the one that left hold.
func choose(now time.Time, hold resetCodexHold, list ...choiceAccount) choiceResult {
	rows := make([]AccountUsageStatus, 0, len(list))
	for _, a := range list {
		rows = append(rows, a.row(now))
	}
	pool := Server{}.resetPoolOf(rows, now)
	ids := map[string]string{}
	var weighed []resetWeighing
	for _, row := range rows {
		c := resetCandidate{key: autoResetKey(accounts.Account{ID: row.ID, Provider: row.Provider}), provider: string(row.Provider),
			id: row.ID, label: row.Label, readings: autoResetReadingsOf(&row), burn: row.Burn}
		if len(row.Resets) == 0 {
			c.out = "it holds no saved reset"
		} else {
			c.next = row.Resets[0]
		}
		ids[c.key] = row.ID
		weighed = append(weighed, pool.weigh(c))
	}
	choosing := chooseResets(weighed, hold, now)
	out := choiceResult{chosen: map[string]ResetChoice{}, why: map[string]string{}, hold: choosing.codex}
	for key, choice := range choosing.choices {
		out.chosen[ids[key]] = choice
	}
	for key, why := range choosing.notChosen {
		out.why[ids[key]] = why
	}
	return out
}

// wantChosen requires exactly the accounts named, each on meter in the order
// listed, and that each reason says what words name.
func wantChosen(t *testing.T, what string, got choiceResult, meter ResetChoiceMeter, order ...string) {
	t.Helper()
	count := 0
	for _, choice := range got.chosen {
		if choice.Meter == meter {
			count++
		}
	}
	if count != len(order) {
		t.Fatalf("%s: %d accounts chosen for %s, want %v: %+v (not chosen: %v)", what, count, meter, order, got.chosen, got.why)
	}
	for i, id := range order {
		choice, ok := got.chosen[id]
		if !ok || choice.Meter != meter || choice.Order != i+1 {
			t.Fatalf("%s: %s is %+v (chosen %v), want %s order %d; not chosen: %v", what, id, choice, ok, meter, i+1, got.why)
		}
	}
}

func wantWords(t *testing.T, what, text string, words ...string) {
	t.Helper()
	for _, w := range words {
		if !strings.Contains(text, w) {
			t.Fatalf("%s: %q does not say %q", what, text, w)
		}
	}
}

func spares(rate float64, n int) []choiceAccount {
	var out []choiceAccount
	for i := range n {
		out = append(out, choiceAccount{id: "spare" + string(rune('1'+i)), week: 0.5, fable: 0.5, toRoll: 100 * time.Hour, weekBurn: rate})
	}
	return out
}

// 1. A Claude week holder in a pool whose week need is yes, with a claim at
// its projected wall worth at least the floor, is chosen for the week: 30% of
// its week left, at the pool's 1.2 over 4 accounts, reaches its limit in 10.5
// hours, 89.5 hours before its roll, where a claim adds 0.64 of a week. The
// same account when the pool's need is no and its reset is outside its
// expiry window is not chosen, and with need unknown it is not either.
func TestAWeekHolderIsChosenWhenThePoolNeedsItsResets(t *testing.T) {
	held := choiceAccount{id: "held", kind: accounts.ResetKindClaudeWeek, week: 0.3, fable: 0.8, toRoll: 100 * time.Hour, expires: 17 * burnDay}
	at := func(rate float64) []choiceAccount {
		h := held
		h.weekBurn = rate
		return append([]choiceAccount{h}, spares(rate, 3)...)
	}
	got := choose(choiceNow, resetCodexHold{}, at(1.2)...)
	wantChosen(t, "need yes", got, ResetChoiceWeek, "held")
	wantWords(t, "need yes", got.chosen["held"].Why, "the pool needs its saved resets on the Claude week (the 24-hour pressure is 1.20",
		"it reaches its limit in 10h30m", "a claim there adds 0.64 of a week")

	got = choose(choiceNow, resetCodexHold{}, at(0.7)...)
	wantChosen(t, "need no", got, ResetChoiceWeek)
	wantWords(t, "need no", got.why["held"], "the pool does not need its saved resets on the Claude week", "the 24-hour pressure is 0.70, under 1.0")

	got = choose(choiceNow, resetCodexHold{}, at(0)...)
	wantChosen(t, "need unknown", got, ResetChoiceWeek)
	wantWords(t, "need unknown", got.why["held"], "the pool's burn on the Claude week is not known yet")
}

// 2. Three week holders with 150, 90 and 40 hours to their rolls and the
// same headroom are ordered 1, 2, 3: the one with the most time left to its
// roll first. A fourth with 20 hours, whose claim at its wall would add 0.16
// of a week and whose reset is neither in its last chance nor in its expiry
// window, is not chosen.
func TestWeekHoldersAreOrderedByTheValueAtTheirWall(t *testing.T) {
	var list []choiceAccount
	for _, hours := range []int{40, 150, 20, 90} {
		list = append(list, choiceAccount{id: "h" + strconv.Itoa(hours), kind: accounts.ResetKindClaudeWeek, week: 0.1, fable: 0.5,
			toRoll: time.Duration(hours) * time.Hour, expires: 17 * burnDay, weekBurn: 1.5})
	}
	got := choose(choiceNow, resetCodexHold{}, append(list, spares(1.5, 2)...)...)
	wantChosen(t, "three holders", got, ResetChoiceWeek, "h150", "h90", "h40")
	wantWords(t, "under the floor", got.why["h20"], "a claim there adds 0.16 of a week, under the floor of 0.25")
}

// 3. An account whose next reset is a Fable reset, in a pool whose Fable
// need is yes, is chosen for the Fable meter. An account whose next reset is
// a week reset is never chosen for Fable, even with its Fable meter spent:
// with the week's need no it is not chosen at all, and with the week's need
// yes it is chosen for the week.
func TestAFableHolderIsChosenForTheFableMeterAndAWeekHolderNeverIs(t *testing.T) {
	pair := func(weekRate float64) []choiceAccount {
		return []choiceAccount{
			{id: "fabled", kind: accounts.ResetKindClaudeFable, week: 0.6, fable: 0.2, toRoll: 100 * time.Hour, expires: 100 * burnDay,
				weekBurn: weekRate, fableBurn: 1.5},
			{id: "weekly", kind: accounts.ResetKindClaudeWeek, week: 0.6, fable: 0, toRoll: 100 * time.Hour, expires: 17 * burnDay,
				weekBurn: weekRate, fableBurn: 1.5},
		}
	}
	got := choose(choiceNow, resetCodexHold{}, pair(0.5)...)
	wantChosen(t, "the week's need no", got, ResetChoiceFable, "fabled")
	wantChosen(t, "the week's need no", got, ResetChoiceWeek)
	wantWords(t, "the Fable holder", got.chosen["fabled"].Why, "the pool needs its saved resets on the Claude Fable")
	wantWords(t, "the week holder", got.why["weekly"], "the pool does not need its saved resets on the Claude week")

	got = choose(choiceNow, resetCodexHold{}, pair(1.5)...)
	wantChosen(t, "the week's need yes", got, ResetChoiceFable, "fabled")
	wantChosen(t, "the week's need yes", got, ResetChoiceWeek, "weekly")
}

// codexHolders are three Codex accounts holding a credit, half their week
// left, rolling in 150, 100 and 60 hours, in a pool at 1.3: each reaches its
// limit in 21.5 hours, where a claim is worth 0.77, 0.47 and 0.23 of a week.
func codexHolders(change func(*choiceAccount)) []choiceAccount {
	var out []choiceAccount
	for _, hours := range []int{150, 100, 60} {
		a := choiceAccount{id: "c" + strconv.Itoa(hours), provider: accounts.ProviderCodex, kind: accounts.ResetKindCodexRateLimits, week: 0.5,
			toRoll: time.Duration(hours) * time.Hour, expires: 20 * burnDay, weekBurn: 1.3}
		if change != nil {
			change(&a)
		}
		out = append(out, a)
	}
	return out
}

// 4. Of three Codex accounts holding credits exactly one is chosen, the one
// whose claim at its projected wall is worth the most. When its credit is
// spent the next is chosen. A choice stands for at least 30 minutes while the
// account still qualifies, even when another account's value passes it; after
// that it gives way to the one worth more.
func TestOneCodexAccountIsChosenAtATime(t *testing.T) {
	first := choose(choiceNow, resetCodexHold{}, codexHolders(nil)...)
	wantChosen(t, "three holders", first, ResetChoiceCodexWeek, "c150")
	wantWords(t, "the chosen one", first.chosen["c150"].Why, "a claim there adds 0.77 of a week", "Codex accounts are loaded one at a time")
	wantWords(t, "the next one", first.why["c100"], "Codex accounts are loaded one at a time, and another one is loaded now")
	wantWords(t, "the third", first.why["c60"], "under the floor of 0.25")

	spent := choose(choiceNow.Add(5*time.Minute), first.hold, codexHolders(func(a *choiceAccount) {
		if a.id == "c150" {
			a.kind = ""
		}
	})...)
	wantChosen(t, "the first one's credit spent", spent, ResetChoiceCodexWeek, "c100")
	wantWords(t, "the spent one", spent.why["c150"], "it holds no saved reset")

	passed := func(a *choiceAccount) {
		if a.id == "c100" {
			a.toRoll = 200 * time.Hour
		}
	}
	held := choose(choiceNow.Add(29*time.Minute), first.hold, codexHolders(passed)...)
	wantChosen(t, "another passes it within 30 minutes", held, ResetChoiceCodexWeek, "c150")
	moved := choose(choiceNow.Add(31*time.Minute), held.hold, codexHolders(passed)...)
	wantChosen(t, "another passes it after 30 minutes", moved, ResetChoiceCodexWeek, "c100")
	if moved.hold.key == first.hold.key || !moved.hold.since.Equal(choiceNow.Add(31*time.Minute)) {
		t.Fatalf("the hold after the move is %+v, want c100 since the pass", moved.hold)
	}
	stays := choose(choiceNow.Add(31*time.Minute), held.hold, codexHolders(nil)...)
	wantChosen(t, "none worth more after 30 minutes", stays, ResetChoiceCodexWeek, "c150")
}

// 5. Inside a kind's expiry window every holder of it is chosen whatever the
// need (here no, at 0.5), ordered by expiry, then by value; for Codex more
// than one at a time. A holder whose projected wall falls after the expiry is
// chosen last, with a sentence that says its reset is likely to expire
// unused.
func TestInsideTheExpiryWindowEveryHolderIsChosen(t *testing.T) {
	codex := func(id string, left float64, toRoll, expires time.Duration) choiceAccount {
		return choiceAccount{id: id, provider: accounts.ProviderCodex, kind: accounts.ResetKindCodexRateLimits, week: left,
			toRoll: toRoll, expires: expires, weekBurn: 0.5}
	}
	got := choose(choiceNow, resetCodexHold{},
		codex("x20a", 0.1, 100*time.Hour, 20*time.Hour),
		codex("x20b", 1.0, 150*time.Hour, 20*time.Hour),
		codex("x10", 0.05, 100*time.Hour, 10*time.Hour),
		codex("x20c", 0.1, 150*time.Hour, 20*time.Hour))
	wantChosen(t, "Codex holders in their windows", got, ResetChoiceCodexWeek, "x10", "x20c", "x20a", "x20b")
	wantWords(t, "a holder in its window", got.chosen["x10"].Why, "is inside its expiry window", "every holder of it is loaded")
	wantWords(t, "the last", got.chosen["x20b"].Why, "is likely to expire unused", "after the reset expires at")
	for _, id := range []string{"x10", "x20a", "x20c"} {
		if strings.Contains(got.chosen[id].Why, "likely to expire unused") {
			t.Fatalf("%s says its reset is likely to expire unused: %s", id, got.chosen[id].Why)
		}
	}

	week := func(id string, toRoll time.Duration) choiceAccount {
		return choiceAccount{id: id, kind: accounts.ResetKindClaudeWeek, week: 0.02, fable: 0.5, toRoll: toRoll, expires: 20 * time.Hour, weekBurn: 0.5}
	}
	got = choose(choiceNow, resetCodexHold{}, week("b", 60*time.Hour), week("a", 120*time.Hour))
	wantChosen(t, "Claude holders in their window", got, ResetChoiceWeek, "a", "b")

	outside := choose(choiceNow, resetCodexHold{}, codex("x9d", 0.1, 100*time.Hour, 9*burnDay), codex("y9d", 0.1, 150*time.Hour, 9*burnDay))
	wantChosen(t, "calibration: the same pool with its expiry far away", outside, ResetChoiceCodexWeek)
}

// 6, on facts. An account whose next reset is of a kind this daemon does not
// know is never chosen; the same account with a known kind is.
func TestANextResetOfAnUnknownKindIsNeverChosen(t *testing.T) {
	account := func(kind accounts.ResetKind) []choiceAccount {
		return append([]choiceAccount{{id: "held", kind: kind, week: 0.3, fable: 0.5, toRoll: 100 * time.Hour, expires: 17 * burnDay, weekBurn: 1.2}},
			spares(1.2, 3)...)
	}
	wantChosen(t, "calibration: a week reset", choose(choiceNow, resetCodexHold{}, account(accounts.ResetKindClaudeWeek)...), ResetChoiceWeek, "held")
	got := choose(choiceNow, resetCodexHold{}, account("five_hour_grant")...)
	if len(got.chosen) != 0 {
		t.Fatalf("an unknown kind: %+v chosen", got.chosen)
	}
	wantWords(t, "an unknown kind", got.why["held"], "its next reset is of a kind this daemon does not know (five_hour_grant)")
}
