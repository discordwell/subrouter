package selectacct

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/discordwell/subrouter/account"
)

// PATCHES §86: a Codex week starts on its first request. An account whose
// reading gives its whole week as left gets the next new session ahead of
// every other account that can take one, so its clock starts, and the
// priority ends as soon as that session is placed.

const (
	weekSeconds = int64(7 * 24 * 60 * 60)
	// The decision line's clause for the priority, as the operator reads it.
	reasonWeekNotStarted = "its weekly window has not started; one new session starts its clock"
)

// setStartUnstartedWeekForTest sets §86's process-wide switch for the rest of
// the test and restores it in t.Cleanup. Only for tests that are not
// t.Parallel.
func setStartUnstartedWeekForTest(t *testing.T, on bool) {
	t.Helper()
	prev := StartUnstartedWeek()
	SetStartUnstartedWeek(on)
	t.Cleanup(func() { SetStartUnstartedWeek(prev) })
}

// codexScore is a local Codex account as a usage reading at now leaves it:
// used% of its week spent, its week rolling reset seconds from now.
func codexReading(now time.Time, id string, used float64, reset int64) Score {
	score := ScoreFromLimitWindowsAt(id, 0, codexWindows(used, reset), now)
	score.Provider = account.ProviderCodex
	score.Fresh = true
	return score
}

func codexAccount(id string) account.Account {
	return account.Account{ID: id, Provider: account.ProviderCodex, AuthMode: account.AuthModeOAuth}
}

// The rival accounts an unstarted week must now beat, each of which today's
// router ranks ahead of it.
func startRivals(now time.Time) map[string]Score {
	holder := resetScore("holder", 0.2, 0.2, now.Add(3*24*time.Hour), now.Add(2*24*time.Hour))
	return map[string]Score{
		// 10% spent a day in: budget above even pace.
		"midweek": codexReading(now, "midweek", 10, 6*24*3600),
		// Half its week left with 12 hours to the roll: the endgame drain.
		"lastday": codexReading(now, "lastday", 50, 12*3600),
		// A reset holder under a quarter of its week: early burndown (§56).
		"holder": holder,
	}
}

func TestAnUnstartedWeekIsReadFromTheWindow(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name    string
		windows []LimitWindow
		want    bool
	}{
		{"the whole week is left", codexWindows(0, weekSeconds), true},
		{"a few seconds of upstream clock skew past the length", codexWindows(0, weekSeconds+3), true},
		{"one second of the week has gone", codexWindows(0, weekSeconds-1), false},
		{"mid-week", codexWindows(40, 300000), false},
		// Read from the window, not from the provider's name.
		{"a Claude week read the same way", claudePoolWindows(0, 0, 10, weekSeconds, 3600), true},
		{"a full short window is not a week", []LimitWindow{
			{Name: "primary", LimitWindowSeconds: 5 * 3600, ResetAfterSeconds: 5 * 3600}}, false},
		{"no window at all", nil, false},
		{"an unread week reports no reset", []LimitWindow{
			{Name: "primary", LimitWindowSeconds: weekSeconds}}, false},
		{"a reading from before its own reset", []LimitWindow{
			{Name: "primary", LimitWindowSeconds: weekSeconds, ResetAfterSeconds: weekSeconds, ResetAt: now.Add(-time.Minute)}}, false},
		{"a usage-based seat's spend window", spendCapWindows(0, 30*24*3600), false},
		{"a started week binds over an untouched one", append(codexWindows(0, weekSeconds),
			LimitWindow{Name: "secondary", UsedPercent: 5, LimitWindowSeconds: weekSeconds, ResetAfterSeconds: 400000}), false},
		{"a started week binds over an untouched one of equal budget", append(codexWindows(0, weekSeconds),
			LimitWindow{Name: "secondary", LimitWindowSeconds: weekSeconds, ResetAfterSeconds: 400000}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ScoreFromLimitWindowsAt("seat", 0, tc.windows, now).WeeklyUnstarted; got != tc.want {
				t.Fatalf("WeeklyUnstarted = %v, want %v", got, tc.want)
			}
		})
	}

	// The Spark pool is a week of its own (priming `primary` does not start
	// it, as the pool board's fuse says), so each pool's score reads its own
	// window, and a pool whose window is unread is not unstarted even while it
	// ranks on the account's week.
	const spark = "gpt53codexspark"
	started := ScoreFromLimitWindowsAt("seat", 0, codexWindows(30, 400000), now)
	if started.WeeklyUnstarted || !started.ModelScores[spark].WeeklyUnstarted {
		t.Fatalf("account week started, Spark week untouched: account %v, Spark %v; want false, true",
			started.WeeklyUnstarted, started.ModelScores[spark].WeeklyUnstarted)
	}
	scoped := NewScheduler([]Score{started}).ForModel("gpt-5.3-codex-spark")
	if !scoped.score(account.ProviderCodex, "seat").WeeklyUnstarted {
		t.Fatal("a Spark turn's ranking lost its pool's untouched week")
	}
	unreadPool := []LimitWindow{codexWindows(0, weekSeconds)[0],
		{Name: "GPT-5.3-Codex-Spark/primary", Feature: "GPT-5.3-Codex-Spark", LimitWindowSeconds: weekSeconds}}
	scoped = NewScheduler([]Score{ScoreFromLimitWindowsAt("seat", 0, unreadPool, now)}).ForModel("gpt-5.3-codex-spark")
	if pool := scoped.score(account.ProviderCodex, "seat"); pool.WeeklyResetAt.IsZero() || pool.WeeklyUnstarted {
		t.Fatalf("a Spark pool with an unread window: week inherited %v, unstarted %v; want true, false",
			!pool.WeeklyResetAt.IsZero(), pool.WeeklyUnstarted)
	}
}

func TestAnUnstartedWeekTakesTheNextNewSessionFirst(t *testing.T) {
	setResetHoldPriorityForTest(t, true)
	now := time.Now()
	rivals := startRivals(now)
	peerUnstarted := codexReading(now, "box-seat", 0, weekSeconds)
	peerUnstarted.Remote = true
	peerUnstarted.PeerVerdict = &NewSessionVerdict{Eligible: true, Reason: VerdictEligible}
	for _, tc := range []struct {
		name      string
		unstarted Score
		rival     Score
		rivalAcct account.Account
	}{
		{"against an account mid-week with budget left", codexReading(now, "fresh", 0, weekSeconds), rivals["midweek"], codexAccount("midweek")},
		{"against an account in its last day that still admits sessions", codexReading(now, "fresh", 0, weekSeconds), rivals["lastday"], codexAccount("lastday")},
		{"against a reset holder in early burndown", codexReading(now, "fresh", 0, weekSeconds), rivals["holder"],
			account.Account{ID: "holder", Provider: account.ProviderClaude, AuthMode: account.AuthModeOAuth}},
		{"a peer's against a local account mid-week", peerUnstarted, rivals["midweek"], codexAccount("midweek")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheduler := NewScheduler([]Score{tc.unstarted, tc.rival})
			accts := []account.Account{tc.rivalAcct, codexAccount(tc.unstarted.AccountID)}

			setStartUnstartedWeekForTest(t, false)
			if explained, _ := scheduler.Explain(accts); explained[0].Account.ID != tc.rivalAcct.ID {
				t.Fatalf("calibration: with the switch off %q led (%q), want the rival %q",
					explained[0].Account.ID, explained[0].Reason, tc.rivalAcct.ID)
			}
			setStartUnstartedWeekForTest(t, true)
			explained, err := scheduler.Explain(accts)
			if err != nil {
				t.Fatalf("Explain: %v", err)
			}
			if explained[0].Account.ID != tc.unstarted.AccountID || explained[0].Reason != reasonWeekNotStarted {
				t.Fatalf("led with %q (%q), want %q (%q)",
					explained[0].Account.ID, explained[0].Reason, tc.unstarted.AccountID, reasonWeekNotStarted)
			}
		})
	}
}

func TestAnUnstartedWeekDoesNotJumpTheClausesAboveIt(t *testing.T) {
	setStartUnstartedWeekForTest(t, true)
	now := time.Now()
	scores := []Score{codexReading(now, "fresh", 0, weekSeconds), codexReading(now, "plain", 50, weekSeconds/2)}
	oauth := []account.Account{codexAccount("plain"), codexAccount("fresh")}
	keyed := []account.Account{
		{ID: "plain", Provider: account.ProviderCodex, AuthMode: account.AuthModeAPIKey},
		{ID: "fresh", Provider: account.ProviderCodex, AuthMode: account.AuthModeAPIKey},
	}
	if explained, _ := NewScheduler(scores).Explain(oauth); explained[0].Account.ID != "fresh" {
		t.Fatalf("calibration: with nothing above the clause in the way, %q led (%q), want fresh",
			explained[0].Account.ID, explained[0].Reason)
	}
	freshKey := ScoreKey(account.ProviderCodex, "fresh")
	for _, tc := range []struct {
		name       string
		scheduler  Scheduler
		accts      []account.Account
		wantReason string
	}{
		{"machine policy refuses it", NewScheduler(scores).WithMachine("mine", fakeMachines{
			holders: map[string]string{"fresh": "another"}}), oauth,
			"machine policy allows it and refuses the other"},
		{"a worse selection tier", NewScheduler(scores), []account.Account{
			codexAccount("plain"), {ID: "fresh", Provider: account.ProviderCodex, AuthMode: account.AuthModeAPIKey}},
			"higher selection tier"},
		{"its admission queue is closed", NewScheduler(scores).WithClosedAccounts(map[string]bool{freshKey: true}), oauth,
			"higher selection tier"},
		{"a capacity mark", NewScheduler(scores).WithCapacityMarks(map[string]int{freshKey: 1}), oauth,
			"not shedding this model's requests; the other is at capacity"},
		{"it cannot take a new session", NewScheduler(scores).WithClosedAccounts(map[string]bool{freshKey: true}), keyed,
			"can take a new session and the other cannot"},
		{"this machine is bound to the other", NewScheduler(scores).WithMachine("mine", fakeMachines{
			bindings: map[string][]string{"plain": {"mine"}}}), oauth,
			"this machine is already bound to it"},
		{"the other is already shared", NewScheduler(scores).WithMachine("mine", fakeMachines{
			bindings: map[string][]string{"plain": {"another"}}}), oauth,
			"already shared, so binding it costs no fresh slot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			explained, err := tc.scheduler.Explain(tc.accts)
			if err != nil {
				t.Fatalf("Explain: %v", err)
			}
			if explained[0].Account.ID != "plain" || explained[0].Reason != tc.wantReason {
				t.Fatalf("led with %q (%q), want plain (%q)", explained[0].Account.ID, explained[0].Reason, tc.wantReason)
			}
		})
	}
}

func TestPickSteadyGivesAnUnstartedWeekTheSamePriority(t *testing.T) {
	now := time.Now()
	scheduler := NewScheduler([]Score{codexReading(now, "fresh", 0, weekSeconds), startRivals(now)["midweek"]})
	accts := []account.Account{codexAccount("midweek"), codexAccount("fresh")}

	setStartUnstartedWeekForTest(t, false)
	if picked, _ := scheduler.PickSteady(accts); picked.ID != "midweek" {
		t.Fatalf("calibration: with the switch off PickSteady chose %q, want midweek", picked.ID)
	}
	setStartUnstartedWeekForTest(t, true)
	explained, err := scheduler.ExplainSteady(accts)
	if err != nil {
		t.Fatalf("ExplainSteady: %v", err)
	}
	if picked, _ := scheduler.PickSteady(accts); picked.ID != "fresh" ||
		explained[0].Account.ID != "fresh" || explained[0].Reason != reasonWeekNotStarted {
		t.Fatalf("PickSteady chose %q; ExplainSteady led with %q (%q); want fresh (%q)",
			picked.ID, explained[0].Account.ID, explained[0].Reason, reasonWeekNotStarted)
	}
}

func TestANewSessionOnAnUnstartedWeekMarksItForTenMinutes(t *testing.T) {
	setStartUnstartedWeekForTest(t, true)
	setResetHoldPriorityForTest(t, true)
	now := time.Now()
	rivals := startRivals(now)
	scores := []Score{codexReading(now, "fresh", 0, weekSeconds), rivals["midweek"], rivals["lastday"], rivals["holder"]}
	accts := []account.Account{codexAccount("midweek"), codexAccount("lastday"), codexAccount("fresh"),
		{ID: "holder", Provider: account.ProviderClaude, AuthMode: account.AuthModeOAuth}}
	ref := NewSchedulerRef(NewScheduler(scores))
	// todaysOrder is the ranking with §86 switched off and no start mark, in
	// both modes: a marked account must rank as if neither existed.
	todaysOrder := func(at time.Time) [2][]string {
		SetStartUnstartedWeek(false)
		defer SetStartUnstartedWeek(true)
		unmarked := ref.Get().withStartSent(nil)
		return [2][]string{rankedIDs(t, unmarked, at, true, accts...), rankedIDs(t, unmarked, at, false, accts...)}
	}
	ranking := func(at time.Time) [2][]string {
		return [2][]string{rankedIDs(t, ref.Get(), at, true, accts...), rankedIDs(t, ref.Get(), at, false, accts...)}
	}

	picked, err := ref.Get().Pick(accts)
	if err != nil || picked.ID != "fresh" {
		t.Fatalf("the first new session went to %q (%v), want fresh", picked.ID, err)
	}
	before := time.Now()
	ref.NoteNewSession(ref.Get(), picked)
	after := time.Now()

	for _, at := range []time.Time{after, before.Add(10*time.Minute - time.Second)} {
		if got, want := ranking(at), todaysOrder(at); !reflect.DeepEqual(got, want) {
			t.Fatalf("%v after the session, with the reading unchanged: ranked %v, want today's %v",
				at.Sub(before).Round(time.Second), got, want)
		}
	}

	expired := after.Add(10 * time.Minute)
	explained := ref.Get().explanationsAt(t, accts, true, expired)
	if explained[0].Account.ID != "fresh" || explained[0].Reason != reasonWeekNotStarted {
		t.Fatalf("10 minutes on with the reading still unstarted, led with %q (%q), want fresh (%q)",
			explained[0].Account.ID, explained[0].Reason, reasonWeekNotStarted)
	}

	// A reading that says the clock runs ends it, mark or no mark.
	scores[0] = codexReading(now, "fresh", 0, weekSeconds-150)
	ref.Set(NewScheduler(scores))
	if got, want := ranking(expired), todaysOrder(expired); !reflect.DeepEqual(got, want) {
		t.Fatalf("with the week running and the mark gone: ranked %v, want today's %v", got, want)
	}

	t.Run("a session on a running week leaves no mark", func(t *testing.T) {
		running := NewSchedulerRef(NewScheduler([]Score{codexReading(now, "fresh", 0, weekSeconds-150), rivals["midweek"]}))
		pair := []account.Account{codexAccount("midweek"), codexAccount("fresh")}
		running.NoteNewSession(running.Get(), codexAccount("fresh"))
		// Its week rolls and the next reading is untouched: it leads at once.
		running.Set(NewScheduler([]Score{codexReading(now, "fresh", 0, weekSeconds), rivals["midweek"]}))
		if picked, _ := running.Get().Pick(pair); picked.ID != "fresh" {
			t.Fatalf("picked %q, want fresh: no mark was due for a session on a running week", picked.ID)
		}
	})
}

// explanationsAt is Explain at a chosen clock, the way the package's tests
// move time for a ranking.
func (s Scheduler) explanationsAt(t *testing.T, accts []account.Account, allowEndgame bool, now time.Time) []Explanation {
	t.Helper()
	sorted, err := s.rankAt(accts, allowEndgame, now)
	if err != nil {
		t.Fatalf("rank: %v", err)
	}
	out := make([]Explanation, 0, len(sorted))
	for i, acct := range sorted {
		reason := "last"
		if i+1 < len(sorted) {
			_, reason = s.compare(acct, sorted[i+1], now, allowEndgame)
		}
		out = append(out, Explanation{Account: acct, Reason: reason})
	}
	return out
}

func TestTwoUnstartedWeeksTakeOneNewSessionEach(t *testing.T) {
	setStartUnstartedWeekForTest(t, true)
	now := time.Now()
	// Named so the account-id tiebreak disagrees with the clauses below the
	// priority: alpha carries a live session, so its pace ranks after zulu's.
	alpha := codexReading(now, "alpha", 0, weekSeconds)
	alpha.LiveSessions = 1
	scores := []Score{alpha, codexReading(now, "zulu", 0, weekSeconds), startRivals(now)["midweek"]}
	accts := []account.Account{codexAccount("alpha"), codexAccount("midweek"), codexAccount("zulu")}
	ref := NewSchedulerRef(NewScheduler(scores))

	var placed []string
	for i := 0; i < 3; i++ {
		picked, err := ref.Get().Pick(accts)
		if err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
		placed = append(placed, picked.ID)
		ref.NoteNewSession(ref.Get(), picked)
	}
	if want := []string{"zulu", "alpha", "midweek"}; !reflect.DeepEqual(placed, want) {
		t.Fatalf("three new sessions went to %v, want %v: one each to the unstarted weeks, zulu first on the clauses below, then today's order", placed, want)
	}
}

// SUBROUTER_START_UNSTARTED_WEEK=0 is today's router: over random pools with
// unstarted weeks and start marks, every ranking and every reason equals the
// one with neither. Calibrated by the switch on: the same pools must rank
// differently somewhere, or the parity would hold for a clause that does
// nothing.
func TestTheStartSwitchOffRanksAsBefore(t *testing.T) {
	setResetHoldPriorityForTest(t, true)
	setStartUnstartedWeekForTest(t, false)
	rng := rand.New(rand.NewSource(86))
	now := time.Now()
	movedWithSwitchOn := 0
	for trial := 0; trial < 1500; trial++ {
		accts, scores, marks := randomStartPool(rng, now, 2+rng.Intn(5))
		stripped := make([]Score, len(scores))
		for i, score := range scores {
			score.WeeklyUnstarted = false
			stripped[i] = score
		}
		withFact := NewScheduler(scores).withStartSent(marks)
		without := NewScheduler(stripped)
		SetStartUnstartedWeek(false)
		if got, want := startFingerprint(t, withFact, accts, now), startFingerprint(t, without, accts, now); got != want {
			t.Fatalf("trial %d: switch off, the unstarted weeks changed the ranking:\nwith them:\n%s\nwithout:\n%s", trial, got, want)
		}
		SetStartUnstartedWeek(true)
		if startFingerprint(t, withFact, accts, now) != startFingerprint(t, without, accts, now) {
			movedWithSwitchOn++
		}
	}
	if movedWithSwitchOn == 0 {
		t.Fatal("calibration: with the switch on no random pool ranked differently; the parity above proves nothing")
	}
}

// The comparator must stay a strict weak ordering with the priority in it:
// over shuffled mixes of unstarted, marked, draining, capped, capacity-marked
// and plain accounts, local and peer, every permutation ranks one order in
// both modes, and no pair sorts each before the other.
func TestTheStartClauseKeepsTheRankingAStrictWeakOrdering(t *testing.T) {
	setStartUnstartedWeekForTest(t, true)
	setResetHoldPriorityForTest(t, true)
	rng := rand.New(rand.NewSource(8601))
	now := time.Now()
	decided := 0
	for trial := 0; trial < 300; trial++ {
		accts, scores, marks := randomStartPool(rng, now, 3+rng.Intn(3))
		capacity := map[string]int{}
		for _, acct := range accts {
			if rng.Intn(5) == 0 {
				capacity[ScoreKey(acct.Provider, acct.ID)] = 1 + rng.Intn(2)
			}
		}
		scheduler := NewScheduler(scores).WithCapacityMarks(capacity).withStartSent(marks)
		for _, allowEndgame := range []bool{true, false} {
			want := rankedIDs(t, scheduler, now, allowEndgame, accts...)
			for _, left := range accts {
				for _, right := range accts {
					leftFirst, _ := scheduler.compare(left, right, now, allowEndgame)
					rightFirst, _ := scheduler.compare(right, left, now, allowEndgame)
					if leftFirst && rightFirst {
						t.Fatalf("trial %d (endgame=%v): %s and %s each sort before the other", trial, allowEndgame, left.ID, right.ID)
					}
				}
			}
			ranked, _ := scheduler.rankAt(accts, allowEndgame, now)
			for i := 0; i+1 < len(ranked); i++ {
				if _, reason := scheduler.compare(ranked[i], ranked[i+1], now, allowEndgame); reason == reasonWeekNotStarted {
					decided++
				}
			}
			for _, order := range permutations(len(accts)) {
				shuffled := make([]account.Account, 0, len(order))
				for _, i := range order {
					shuffled = append(shuffled, accts[i])
				}
				if got := rankedIDs(t, scheduler, now, allowEndgame, shuffled...); !reflect.DeepEqual(got, want) {
					t.Fatalf("trial %d (endgame=%v): order %v ranked %v, the identity order ranked %v; "+
						"the comparator is not a strict weak ordering", trial, allowEndgame, order, got, want)
				}
			}
		}
	}
	if decided == 0 {
		t.Fatal("the random pools never ran through the priority")
	}
	t.Logf("the priority decided %d steps", decided)
}

// randomStartPool is randomResetPool (draining, capped and peer accounts, live
// and expired holds) with a third of its accounts reading an unstarted week,
// and half of those carrying a start mark, live or expired.
func randomStartPool(rng *rand.Rand, now time.Time, count int) ([]account.Account, []Score, map[string]time.Time) {
	accts, scores := randomResetPool(rng, now, count, true)
	marks := map[string]time.Time{}
	for i := range scores {
		if rng.Intn(3) != 0 {
			continue
		}
		scores[i].WeeklyUnstarted = true
		switch rng.Intn(4) {
		case 0:
			marks[ScoreKey(accts[i].Provider, accts[i].ID)] = now.Add(StartSentMarkDuration / 2)
		case 1:
			marks[ScoreKey(accts[i].Provider, accts[i].ID)] = now.Add(-time.Second)
		}
	}
	return accts, scores, marks
}

// startFingerprint is both orderings of a pool with the clause behind each
// step.
func startFingerprint(t *testing.T, scheduler Scheduler, accts []account.Account, now time.Time) string {
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
