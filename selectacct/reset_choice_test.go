package selectacct

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

// PATCHES §95.2: ranking honours a choice. A chosen account takes the next
// new session before every steady account and after the drain tier, in its
// holder's order; its allowance is the paced ceiling, held down by the 5h
// brake and backpressure; the short floor does not stop it; and §65 does not
// rank an account chosen for its Fable meter up for Opus work.

func chooseFor(meter ResetChoiceMeter, order int, provider account.Provider, ids ...string) map[string]ResetChoice {
	out := map[string]ResetChoice{}
	for _, id := range ids {
		out[ScoreKey(provider, id)] = ResetChoice{Meter: meter, Order: order, Why: "test"}
	}
	return out
}

func choiceRanking(t *testing.T, explain func([]account.Account) ([]Explanation, error), accts ...account.Account) ([]string, []string) {
	t.Helper()
	explained, err := explain(accts)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	var ids, reasons []string
	for _, e := range explained {
		ids = append(ids, e.Account.ID)
		reasons = append(reasons, e.Reason)
	}
	return ids, reasons
}

func wantFirst(t *testing.T, what string, scheduler Scheduler, want, reason string, accts ...account.Account) {
	t.Helper()
	picked, err := scheduler.Pick(accts)
	if err != nil {
		t.Fatalf("%s: pick: %v", what, err)
	}
	ids, reasons := choiceRanking(t, scheduler.Explain, accts...)
	if picked.ID != want || ids[0] != want || (reason != "" && reasons[0] != reason) {
		t.Fatalf("%s: picked %s, explained %v %q; want %s first for %q", what, picked.ID, ids, reasons, want, reason)
	}
}

// 1. A chosen account takes the new session ahead of steady accounts with a
// better pace, and Explain names the choice. An account in its last day
// before the roll and one in §56's early burndown still rank above it. A
// steady pick (sr's interactive switch) does not park on it.
func TestAChosenAccountTakesTheNewSessionAheadOfBetterPace(t *testing.T) {
	setResetHoldPriorityForTest(t, true)
	now := time.Now()
	hold := now.Add(10 * 24 * time.Hour)
	steady := resetScore("steady", 0.8, 0.8, now.Add(72*time.Hour), time.Time{})
	faster := resetScore("faster", 0.7, 0.7, now.Add(48*time.Hour), time.Time{})
	chosen := resetScore("chosen", 0.5, 0.5, now.Add(150*time.Hour), hold)
	lastDay := resetScore("lastday", 0.5, 0.5, now.Add(12*time.Hour), time.Time{})
	burndown := resetScore("burndown", 0.2, 0.2, now.Add(72*time.Hour), hold)
	accts := []account.Account{claudeAccount("steady"), claudeAccount("faster"), claudeAccount("chosen")}
	plain := NewScheduler([]Score{steady, faster, chosen})
	wantFirst(t, "calibration: no choice", plain, "faster", "", accts...)

	loaded := plain.WithResetChoices(chooseFor(ResetChoiceWeek, 1, account.ProviderClaude, "chosen"))
	wantFirst(t, "chosen for its week", loaded, "chosen", reasonChosen, accts...)

	all := append(accts, claudeAccount("lastday"), claudeAccount("burndown"))
	withDrain := NewScheduler([]Score{steady, faster, chosen, lastDay, burndown}).
		WithResetChoices(chooseFor(ResetChoiceWeek, 1, account.ProviderClaude, "chosen"))
	ids, reasons := choiceRanking(t, withDrain.Explain, all...)
	if fmt.Sprint(ids[:3]) != "[lastday burndown chosen]" || reasons[1] != reasonEarlyBurndown || reasons[2] != reasonChosen {
		t.Fatalf("with a last-day account and an early burndown: %v %q, want both ahead of the chosen account", ids, reasons)
	}

	// Steady mode: the chosen account would win on pace, and is not where a
	// long-lived interactive session is parked.
	quick := resetScore("chosen", 0.9, 0.9, now.Add(30*time.Hour), hold)
	pair := []account.Account{claudeAccount("steady"), claudeAccount("chosen")}
	steadyPlain := NewScheduler([]Score{steady, quick})
	if ids, _ := choiceRanking(t, steadyPlain.ExplainSteady, pair...); ids[0] != "chosen" {
		t.Fatalf("calibration: steady mode without the choice ranks %v", ids)
	}
	ids, reasons = choiceRanking(t, steadyPlain.WithResetChoices(chooseFor(ResetChoiceWeek, 1, account.ProviderClaude, "chosen")).ExplainSteady, pair...)
	if ids[0] != "steady" || reasons[0] != reasonSteadyChosen {
		t.Fatalf("steady mode with the choice: %v %q, want steady first for %q", ids, reasons, reasonSteadyChosen)
	}
}

// 2. Among chosen accounts the holder's order decides: the second takes a new
// session only when the first carries its allowance. The local-over-peer
// factor, the live-session spread and the live debit do not reorder them.
func TestChosenAccountsAreLoadedInTheirOrder(t *testing.T) {
	setPacedSessionCeilingForTest(t, 20)
	now := time.Now()
	// Paces: second ahead of first, and first ahead of steady even with
	// twenty live sessions discounting it.
	first := resetScore("first", 0.9, 0.9, now.Add(40*time.Hour), time.Time{})
	second := resetScore("second", 0.95, 0.95, now.Add(30*time.Hour), time.Time{})
	steady := resetScore("steady", 0.3, 0.3, now.Add(120*time.Hour), time.Time{})
	accts := []account.Account{claudeAccount("first"), claudeAccount("second"), claudeAccount("steady")}
	choices := chooseFor(ResetChoiceWeek, 1, account.ProviderClaude, "first")
	for key, choice := range chooseFor(ResetChoiceWeek, 2, account.ProviderClaude, "second") {
		choices[key] = choice
	}
	base := NewScheduler([]Score{first, second, steady}).WithResetChoices(choices)
	live := func(firstLive int) Scheduler {
		return base.WithLiveSessionCounts(map[string]int{ScoreKey(account.ProviderClaude, "first"): firstLive})
	}
	wantFirst(t, "first idle", live(0), "first", reasonChosenOrder, accts...)
	// Nineteen live sessions would discount its pace by 74%, and forty
	// requests routed since its reading debit its whole week: neither moves it.
	wantFirst(t, "first with 19 live and its week debited", live(19).WithLiveDebits(map[string]int{ScoreKey(account.ProviderClaude, "first"): 40}),
		"first", reasonChosenOrder, accts...)
	ids, reasons := choiceRanking(t, live(20).Explain, accts...)
	if fmt.Sprint(ids) != "[second steady first]" || reasons[0] != reasonChosen || reasons[1] != reasonChosenAtCap {
		t.Fatalf("first at its allowance: %v %q, want second, then steady, then first", ids, reasons)
	}

	// A peer's order 1 goes before a local order 2: the local 2x does not
	// enter, and the peer's eligibility is its holder's verdict.
	peer := first
	peer.Remote = true
	peer.ResetChoice = ResetChoice{Meter: ResetChoiceWeek, Order: 1}
	peer.PeerVerdict = &NewSessionVerdict{Eligible: true, Reason: VerdictChosen, LiveSessions: 12, AllowedSessions: 20}
	peer.LiveSessions = 12
	merged := NewScheduler([]Score{peer, second, steady}).WithResetChoices(chooseFor(ResetChoiceWeek, 2, account.ProviderClaude, "second"))
	wantFirst(t, "a peer's order 1 against a local order 2", merged, "first", reasonChosenOrder, accts...)
	// At the same place in two holders' orders, the longer time to the roll
	// goes first, whichever is local.
	sameOrder := NewScheduler([]Score{peer, second, steady}).WithResetChoices(chooseFor(ResetChoiceWeek, 1, account.ProviderClaude, "second"))
	wantFirst(t, "a peer's order 1 and a local order 1", sameOrder, "first", reasonChosenLongRoll, accts...)
	full := peer
	full.PeerVerdict = &NewSessionVerdict{Reason: VerdictAtCap, AtCap: true, LiveSessions: 20, AllowedSessions: 20}
	wantFirst(t, "the peer's account at its allowance", NewScheduler([]Score{full, second, steady}).
		WithResetChoices(chooseFor(ResetChoiceWeek, 2, account.ProviderClaude, "second")), "second", reasonChosen, accts...)
}

// 3. A chosen account's allowance is the paced ceiling, not the taper: with 3%
// of its week left it takes a twentieth live session and refuses a
// twenty-first. The 5h brake holds it at one, and backpressure at four.
func TestAChosenAccountsAllowanceIsThePacedCeiling(t *testing.T) {
	setResetHoldPriorityForTest(t, true)
	setPacedSessionCeilingForTest(t, 20)
	now := time.Now()
	key := ScoreKey(account.ProviderClaude, "chosen")
	accts := []account.Account{claudeAccount("chosen"), claudeAccount("steady")}
	steady := resetScore("steady", 0.6, 0.6, now.Add(72*time.Hour), time.Time{})
	low := resetScore("chosen", 0.03, 0.03, now.Add(100*time.Hour), now.Add(10*24*time.Hour))
	scheduler := func(score Score, live int, chosen bool) Scheduler {
		s := NewScheduler([]Score{score, steady}).WithLiveSessionCounts(map[string]int{key: live})
		if chosen {
			s = s.WithResetChoices(chooseFor(ResetChoiceWeek, 1, account.ProviderClaude, "chosen"))
		}
		return s
	}
	if v := scheduler(low, 1, false).NewSessionVerdict(account.ProviderClaude, "chosen", now); v.Eligible || v.AllowedSessions != 1 {
		t.Fatalf("calibration: not chosen, in early burndown with 3%% left, its taper is one: %+v", v)
	}
	wantFirst(t, "19 live", scheduler(low, 19, true), "chosen", reasonChosen, accts...)
	if v := scheduler(low, 19, true).NewSessionVerdict(account.ProviderClaude, "chosen", now); !v.Eligible || v.Reason != VerdictChosen ||
		v.AllowedSessions != 20 || v.Burndown || v.BurndownCause != "" {
		t.Fatalf("19 live: verdict %+v, want eligible as chosen, allowed 20, and out of §56's early burndown", v)
	}
	wantFirst(t, "20 live", scheduler(low, 20, true), "steady", "", accts...)
	if v := scheduler(low, 20, true).NewSessionVerdict(account.ProviderClaude, "chosen", now); v.Eligible || !v.AtCap || v.Reason != VerdictAtCap {
		t.Fatalf("20 live: verdict %+v, want at its cap", v)
	}

	// The 5h brake: two hours into the window, 80% of it is spent and four
	// hours remain.
	braked := resetScore("chosen", 0.4, 0.4, now.Add(100*time.Hour), time.Time{})
	braked.ShortHeadroom, braked.ShortResetAfterSeconds = 0.2, 4*3600
	wantFirst(t, "5h brake, none live", scheduler(braked, 0, true), "chosen", reasonChosen, accts...)
	if v := scheduler(braked, 1, true).NewSessionVerdict(account.ProviderClaude, "chosen", now); v.Eligible || v.AllowedSessions != 1 {
		t.Fatalf("5h brake, one live: verdict %+v, want held at one", v)
	}

	// Backpressure: a non-quota 429 a minute ago.
	pressed := resetScore("chosen", 0.4, 0.4, now.Add(100*time.Hour), time.Time{})
	pressed.Pace = Pace{Backpressure: now.Add(-time.Minute)}
	wantFirst(t, "backpressure, 3 live", scheduler(pressed, 3, true), "chosen", reasonChosen, accts...)
	if v := scheduler(pressed, 4, true).NewSessionVerdict(account.ProviderClaude, "chosen", now); v.Eligible || v.AllowedSessions != 4 {
		t.Fatalf("backpressure, 4 live: verdict %+v, want held at four", v)
	}
}

// 4. The short floor does not stop a chosen account: a Codex weekly-only
// account with 2% of its week left takes a new session, where the same
// account not chosen does not. An exhausted chosen account takes none, and
// neither does one whose admission queue is closed.
func TestTheFloorsDoNotStopAChosenAccount(t *testing.T) {
	now := time.Now()
	low := codexReading(now, "low", 98, 100*3600)
	other := codexReading(now, "other", 60, 50*3600)
	accts := []account.Account{codexAccount("low"), codexAccount("other")}
	plain := NewScheduler([]Score{low, other})
	wantFirst(t, "calibration: not chosen", plain, "other", "", accts...)
	if v := plain.NewSessionVerdict(account.ProviderCodex, "low", now); v.Eligible || v.Reason != VerdictShortFloor {
		t.Fatalf("calibration: verdict %+v, want refused at the short floor", v)
	}
	choice := chooseFor(ResetChoiceCodexWeek, 1, account.ProviderCodex, "low")
	wantFirst(t, "chosen with 2% left", plain.WithResetChoices(choice), "low", reasonChosen, accts...)
	if v := plain.WithResetChoices(choice).NewSessionVerdict(account.ProviderCodex, "low", now); !v.Eligible || v.Reason != VerdictChosen {
		t.Fatalf("chosen with 2%% left: verdict %+v, want eligible as chosen", v)
	}
	spent := codexReading(now, "low", 100, 100*3600)
	wantFirst(t, "chosen and exhausted", NewScheduler([]Score{spent, other}).WithResetChoices(choice), "other", "", accts...)
	closed := plain.WithResetChoices(choice).WithClosedAccounts(map[string]bool{ScoreKey(account.ProviderCodex, "low"): true})
	wantFirst(t, "chosen with its queue closed", closed, "other", "", accts...)
	if v := closed.NewSessionVerdict(account.ProviderCodex, "low", now); v.Eligible || v.Reason != VerdictQueueClosed {
		t.Fatalf("chosen with its queue closed: verdict %+v", v)
	}
}

// 5. §65: an Opus turn ranks an account chosen for its Fable meter without
// the Fable-spent factor, and still ranks a Fable-spent account that is not
// chosen with it.
func TestTheFableSpentPreferenceSkipsAnAccountChosenForFable(t *testing.T) {
	setFableSpentPreferenceForTest(t, true)
	now := time.Now()
	roll := int64(72 * 3600)
	untouched := fableScore(now, "alpha", fableMeterWindows(43, 0, roll, roll))
	spent := fableScore(now, "bravo", fableMeterWindows(70, 100, roll, roll))
	accts := []account.Account{claudeAccount("alpha"), claudeAccount("bravo")}
	base := NewScheduler([]Score{untouched, spent})
	wantFirst(t, "not chosen, Fable spent", base.ForModel("claude-opus"), "bravo", reasonFableSpent, accts...)
	forFable := base.WithResetChoices(chooseFor(ResetChoiceFable, 1, account.ProviderClaude, "bravo"))
	wantFirst(t, "chosen for its Fable meter", forFable.ForModel("claude-opus"), "alpha", "higher weekly pace after the live-session discount", accts...)
}

// 6. A Fable choice loads the Fable pool only; a week choice loads every pool
// of the account.
func TestAFableChoiceLoadsOnlyTheFablePool(t *testing.T) {
	now := time.Now()
	roll := int64(72 * 3600)
	fast := fableScore(now, "alpha", fableMeterWindows(10, 0, roll, roll))
	slow := fableScore(now, "bravo", fableMeterWindows(60, 0, roll, roll))
	accts := []account.Account{claudeAccount("alpha"), claudeAccount("bravo")}
	base := NewScheduler([]Score{fast, slow})
	models := []string{"claude-fable", "claude-opus", "claude-sonnet", "claude-haiku", ""}
	for _, model := range models {
		wantFirst(t, "calibration on "+model, base.ForModel(model), "alpha", "", accts...)
	}
	for _, model := range models {
		forFable := base.WithResetChoices(chooseFor(ResetChoiceFable, 1, account.ProviderClaude, "bravo")).ForModel(model)
		if model == "claude-fable" {
			wantFirst(t, "a Fable choice on "+model, forFable, "bravo", reasonChosen, accts...)
		} else {
			wantFirst(t, "a Fable choice on "+model, forFable, "alpha", "", accts...)
		}
		forWeek := base.WithResetChoices(chooseFor(ResetChoiceWeek, 1, account.ProviderClaude, "bravo")).ForModel(model)
		wantFirst(t, "a week choice on "+model, forWeek, "bravo", reasonChosen, accts...)
	}
	// On the Fable pool a Fable choice goes before a week choice, whatever
	// the orders say.
	both := chooseFor(ResetChoiceWeek, 1, account.ProviderClaude, "alpha")
	for key, choice := range chooseFor(ResetChoiceFable, 2, account.ProviderClaude, "bravo") {
		both[key] = choice
	}
	wantFirst(t, "a week choice and a Fable choice on the Fable pool", base.WithResetChoices(both).ForModel("claude-fable"), "bravo", reasonChosenFable, accts...)
	wantFirst(t, "a week choice and a Fable choice on the Opus pool", base.WithResetChoices(both).ForModel("claude-opus"), "alpha", reasonChosen, accts...)
}

// The comparator stays a strict weak ordering with choices in the pool: over
// random pools of chosen, capped, draining, peer and steady accounts, it is
// irreflexive, asymmetric and transitive, and every order of the candidates
// ranks the same.
func TestTheRankingWithChoicesIsAStrictWeakOrdering(t *testing.T) {
	setPacedSessionCeilingForTest(t, 4)
	now := time.Now()
	r := rand.New(rand.NewSource(95))
	meters := []ResetChoiceMeter{"", "", ResetChoiceWeek, ResetChoiceWeek, ResetChoiceFable}
	for round := range 200 {
		var scores []Score
		var accts []account.Account
		choices := map[string]ResetChoice{}
		live := map[string]int{}
		for i := range 6 {
			id := fmt.Sprintf("a%d", i)
			score := resetScore(id, 0.02+r.Float64()*0.9, 0.5, now.Add(time.Duration(2+r.Intn(160))*time.Hour), time.Time{})
			if r.Intn(4) == 0 {
				score.ResetHeldUntil = now.Add(240 * time.Hour)
				score.WeeklyRemaining = score.WeeklyHeadroom
			}
			if meter := meters[r.Intn(len(meters))]; meter != "" {
				choice := ResetChoice{Meter: meter, Order: 1 + r.Intn(2)}
				if r.Intn(3) == 0 {
					score.Remote, score.ResetChoice = true, choice
					atCap := r.Intn(2) == 0
					score.PeerVerdict = &NewSessionVerdict{Eligible: !atCap, AtCap: atCap, LiveSessions: r.Intn(5)}
				} else {
					choices[ScoreKey(account.ProviderClaude, id)] = choice
				}
			}
			live[ScoreKey(account.ProviderClaude, id)] = r.Intn(6)
			scores = append(scores, score)
			accts = append(accts, claudeAccount(id))
		}
		for _, model := range []string{"claude-fable", "claude-opus"} {
			s := NewScheduler(scores).WithResetChoices(choices).WithLiveSessionCounts(live).ForModel(model)
			for _, a := range accts {
				if less, _ := s.compare(a, a, now, true); less {
					t.Fatalf("round %d %s: %s sorts before itself", round, model, a.ID)
				}
				for _, b := range accts {
					ab, _ := s.compare(a, b, now, true)
					ba, _ := s.compare(b, a, now, true)
					if ab && ba {
						t.Fatalf("round %d %s: %s and %s each sort first", round, model, a.ID, b.ID)
					}
					for _, c := range accts {
						bc, _ := s.compare(b, c, now, true)
						ac, _ := s.compare(a, c, now, true)
						if ab && bc && !ac {
							t.Fatalf("round %d %s: %s < %s < %s but not %s < %s", round, model, a.ID, b.ID, c.ID, a.ID, c.ID)
						}
					}
				}
			}
			want := rankedIDs(t, s, now, true, accts...)
			shuffled := append([]account.Account(nil), accts...)
			r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			if got := rankedIDs(t, s, now, true, shuffled...); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("round %d %s: %v from one order, %v from another", round, model, want, got)
			}
		}
	}
}
