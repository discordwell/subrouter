package selectacct

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

const (
	fourteenHours = int64(14 * 60 * 60)
	sixteenHours  = int64(16 * 60 * 60)
	oneHour       = int64(1 * 60 * 60)
)

// drainingPair mirrors the 9/2 local pool: two Claude accounts inside the 24h
// roll window, the earlier one further through its budget (main: 58% general /
// 84% fable, rolling in ~14h) and the later one nearly untouched (eight-alpha:
// 13% / 3%, rolling in ~16h). The later account is forfeiting more than twice
// as much week.
func drainingPair() (Score, Score) {
	rollsFirst := claudeScore("rolls-first", claudePoolWindows(58, 84, 33, fourteenHours, twoHours))
	rollsLater := claudeScore("rolls-later", claudePoolWindows(13, 3, 0, sixteenHours, twoHours))
	return rollsFirst, rollsLater
}

// An idle pool still hands the window to the account that rolls first. Earlier
// expiry is the correct default and the spread discount must not invert it.
func TestEarlierRollLeadsOnAnIdlePool(t *testing.T) {
	rollsFirst, rollsLater := drainingPair()
	scheduler := NewScheduler([]Score{rollsLater, rollsFirst})
	if got := pickID(t, scheduler, "rolls-later", "rolls-first"); got != "rolls-first" {
		t.Fatalf("idle pick = %q, want the earlier roll to lead", got)
	}
}

// ...but leading is not monopolising. Once the earlier account is carrying
// work, the next pick goes to its draining sibling rather than queueing behind
// it until it slams into endgameAtCap. Both accounts are inside the window and
// both forfeit whatever they do not spend, so both must drain.
//
// RED before the spread-adjusted urgency term: comparing raw WeeklyResetAt made
// this the one rate term in compare() with no session component, so
// "rolls-first" won every pick at every load.
func TestBothDrainingAccountsShareTheWindow(t *testing.T) {
	rollsFirst, rollsLater := drainingPair()
	scheduler := NewScheduler([]Score{rollsLater, rollsFirst}).
		WithLiveSessionCounts(map[string]int{"rolls-first": 1})
	if got := pickID(t, scheduler, "rolls-later", "rolls-first"); got != "rolls-later" {
		t.Fatalf("pick with the earlier roll busy = %q, want its sibling to drain too", got)
	}
}

// The sibling takes over without the leader having been demoted: it is still
// drain-eligible, still under its taper. The handoff is the session discount,
// not the concurrency cliff.
func TestTheLeaderIsStillDrainEligibleWhenItYields(t *testing.T) {
	rollsFirst, _ := drainingPair()
	scheduler := NewScheduler([]Score{rollsFirst}).
		WithLiveSessionCounts(map[string]int{"rolls-first": 1})
	score := scheduler.score(rollsFirst.Provider, "rolls-first")
	now := time.Now()
	if !score.endgameDrainEligible(now) {
		t.Fatalf("rolls-first stopped being drain-eligible at 1 live session; "+
			"the handoff would be the cap, not the spread (allowed=%d)",
			score.endgameAllowedSessions())
	}
}

// The discount is sized to move picks between comparable deadlines, never to
// hand the window to an account rolling most of a day later.
func TestSessionsDoNotReorderDistantRolls(t *testing.T) {
	imminent := claudeScore("imminent", claudePoolWindows(20, 20, 5, oneHour, twoHours))
	distant := claudeScore("distant", claudePoolWindows(20, 20, 5, twentyHours, twoHours))
	scheduler := NewScheduler([]Score{distant, imminent}).
		WithLiveSessionCounts(map[string]int{"imminent": 2})
	if got := pickID(t, scheduler, "distant", "imminent"); got != "imminent" {
		t.Fatalf("pick = %q, want the imminent roll to hold its lead under load", got)
	}
}

// Endgame slots are per-account and additive, so the pool's total drain
// capacity scales with the number of accounts inside the window. Both of a
// draining pair must be able to reach the ceiling; throttling each one below it
// spills the difference onto healthy accounts that are days from rolling, which
// spends fresh budget while doomed budget expires.
//
// RED at claudeEndgameSessionStep = 0.25, where the step bound across the whole
// range and the cap could never bind (WeeklyHeadroom <= 1, so ceil(1/0.25) is
// already 4): the pair offered 2+3 = 5 slots, not 4+4 = 8.
func TestDrainingPairOffersTheFullCeilingEach(t *testing.T) {
	rollsFirst, rollsLater := drainingPair()
	scheduler := NewScheduler([]Score{rollsLater, rollsFirst})
	total := 0
	for _, id := range []string{"rolls-first", "rolls-later"} {
		score := scheduler.score(rollsFirst.Provider, id)
		allowed := score.endgameAllowedSessions()
		if allowed != claudeEndgameMaxSessions {
			t.Errorf("%s allows %d endgame sessions, want the full ceiling of %d "+
				"(weekly headroom %.3f)", id, allowed, claudeEndgameMaxSessions, score.WeeklyHeadroom)
		}
		total += allowed
	}
	if want := 2 * claudeEndgameMaxSessions; total != want {
		t.Fatalf("draining pair offers %d slots, want %d", total, want)
	}
}

// The taper still glides an emptying account down to a single final task, so
// the last of its budget is not dogpiled by four sessions at once.
func TestTaperStillGlidesAnEmptyingAccount(t *testing.T) {
	for _, tc := range []struct {
		headroom float64
		want     int
	}{{0.05, 1}, {0.10, 2}, {0.15, 3}, {0.50, 4}} {
		got := Score{WeeklyHeadroom: tc.headroom}.endgameAllowedSessions()
		if got != tc.want {
			t.Errorf("headroom %.2f allows %d sessions, want %d", tc.headroom, got, tc.want)
		}
	}
}

// PATCHES §31 D2: inside the burndown tier there is no local 2x. A draining
// pair whose deadlines land in the same urgency bucket -- rolling together, or
// the leader's live-session discount just catching it up to a sibling -- is
// decided by the budget each has to lose, and a peer holding more than the
// local account takes the pick. Both forfeit whatever they hold at the roll,
// wherever they live.
//
// RED with the tie falling through to the pace clause, whose rankingPace
// doubles the local account: it won every such tie holding two thirds of the
// peer's budget. The "a minute apart" case is green on the raw-WeeklyResetAt
// comparison this replaced (the peer rolls first) and went red only once
// urgency was bucketed; "the same instant" was already red there.
func TestDrainingAccountsThatRollTogetherTieOnBudgetNotOnLocalPreference(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name     string
		localLag time.Duration
	}{
		{"a minute apart", time.Minute},
		{"the same instant", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peerRoll := now.Add(16 * time.Hour)
			local := Score{
				AccountID: "here", Provider: account.ProviderClaude,
				Headroom: 1, ShortHeadroom: 1, Fresh: true,
				WeeklyHeadroom: 0.30, WeeklyRemaining: 0.30, WeeklyResetAt: peerRoll.Add(tc.localLag),
			}
			peer := Score{
				AccountID: "there", Provider: account.ProviderClaude, Remote: true,
				Headroom: 1, ShortHeadroom: 1, Fresh: true,
				WeeklyHeadroom: 0.45, WeeklyRemaining: 0.45, WeeklyResetAt: peerRoll,
				PeerVerdict: &NewSessionVerdict{Eligible: true, Reason: VerdictBurndown, Burndown: true, AllowedSessions: 4},
			}
			// Calibration: both drain, their urgencies tie, and the local 2x
			// is what would decide -- so the pick below is about that tie.
			if !local.endgameDrainEligible(now) || !peer.endgameDrainEligible(now) {
				t.Fatal("calibration: both accounts must be drain-eligible")
			}
			if rankBucket(local.endgameUrgency(now)) != rankBucket(peer.endgameUrgency(now)) {
				t.Fatal("calibration: the two deadlines must share an urgency bucket")
			}
			localPace, _ := local.weeklyPace(now)
			peerPace, _ := peer.weeklyPace(now)
			if (Scheduler{}).rankingPace(local, localPace, now) <= (Scheduler{}).rankingPace(peer, peerPace, now) || localPace >= peerPace {
				t.Fatal("calibration: the local 2x must outweigh the peer's larger budget")
			}
			got, err := NewScheduler([]Score{local, peer}).Pick([]account.Account{claudeAccount("here"), claudeAccount("there")})
			if err != nil || got.ID != "there" {
				t.Fatalf("picked %q (%v), want the peer: it has more to lose and no local 2x applies between draining accounts", got.ID, err)
			}
		})
	}
}

// ...and a pair that ties on budget as well is not handed to the local 2x on
// the way down. Past the stake clause the next RATE term is short expiry, and a
// peer whose 5h window resets in ten minutes has budget there that refills
// either way; spending it first is what that clause is for. The pace clause
// would have decided before it, doubling the local account.
//
// RED with the draining pair allowed back into the rankingPace clause: the
// stake clause ties, rankingPace doubles "here", and it wins on a budget
// identical to the peer's.
func TestDrainingAccountsTiedOnBudgetFallToShortExpiryNotToLocalPreference(t *testing.T) {
	now := time.Now()
	roll := now.Add(16 * time.Hour)
	local := Score{
		AccountID: "here", Provider: account.ProviderClaude,
		Headroom: 1, ShortHeadroom: 1, Fresh: true, ShortResetAfterSeconds: 4 * 60 * 60,
		WeeklyHeadroom: 0.40, WeeklyRemaining: 0.40, WeeklyResetAt: roll,
	}
	peer := Score{
		AccountID: "there", Provider: account.ProviderClaude, Remote: true,
		Headroom: 1, ShortHeadroom: 1, Fresh: true, ShortResetAfterSeconds: 10 * 60,
		WeeklyHeadroom: 0.40, WeeklyRemaining: 0.40, WeeklyResetAt: roll,
		PeerVerdict: &NewSessionVerdict{Eligible: true, Reason: VerdictBurndown, Burndown: true, AllowedSessions: 4},
	}
	// Calibration: both drain, urgency AND stake tie, and the local 2x is what
	// would decide past them -- so the pick below is about the pace clause.
	if !local.endgameDrainEligible(now) || !peer.endgameDrainEligible(now) {
		t.Fatal("calibration: both accounts must be drain-eligible")
	}
	if rankBucket(local.endgameUrgency(now)) != rankBucket(peer.endgameUrgency(now)) {
		t.Fatal("calibration: the two deadlines must share an urgency bucket")
	}
	localPace, _ := local.weeklyPace(now)
	peerPace, _ := peer.weeklyPace(now)
	if rankBucket(localPace) != rankBucket(peerPace) {
		t.Fatal("calibration: the two budgets must tie")
	}
	if rankBucket(Scheduler{}.rankingPace(local, localPace, now)) <= rankBucket(Scheduler{}.rankingPace(peer, peerPace, now)) {
		t.Fatal("calibration: the local 2x must separate them in the pace clause")
	}
	got, err := NewScheduler([]Score{local, peer}).Pick([]account.Account{claudeAccount("here"), claudeAccount("there")})
	if err != nil || got.ID != "there" {
		t.Fatalf("picked %q (%v), want the peer: its short window expires first and no local 2x applies between draining accounts", got.ID, err)
	}
}

// A reserved account's glide ends at its HOLDOUT, not at zero. The owner's
// reserve is budget the drain may not spend (belowWeeklyReserve), so sizing the
// taper on the whole of WeeklyHeadroom put the full ceiling on an account a
// few points above its holdout -- four sessions spending past it until the
// next refresh, then inHoldout evicting all four warm sessions at once -- and
// with a reserve of 0.10 or more the taper never reached one before the reserve
// did.
//
// RED on the absolute-headroom taper: 0.20 over a 0.05 reserve allowed 4, and
// 0.12 over 0.10 allowed 2.
func TestTaperGlideEndsAtTheOwnersHoldout(t *testing.T) {
	for _, tc := range []struct {
		name              string
		headroom, reserve float64
		want              int
	}{
		{"fifteen points above the default holdout", 0.20, 0.05, 3},
		{"two points above a 10% holdout", 0.12, 0.10, 1},
		{"a quarter above a 10% holdout", 0.35, 0.10, 4},
		// The unreserved ladder is unchanged.
		{"unreserved at the same level", 0.20, 0, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Score{WeeklyHeadroom: tc.headroom, Reserve: tc.reserve}.endgameAllowedSessions()
			if got != tc.want {
				t.Fatalf("headroom %.2f over reserve %.2f allows %d sessions, want %d",
					tc.headroom, tc.reserve, got, tc.want)
			}
		})
	}
}

// The same, through Pick with the reserve attached the way the proxy attaches
// it: the reserved owner account stops taking new sessions at its reduced
// allowance and the next one spills to the healthy account. With PATCHES §60's
// release on, the owner is unreserved in its final day and takes the full
// ladder (TestFinalDayDrainSizesOnTheWholeWeek).
func TestReservedDrainingAccountSpillsBeforeItsHoldout(t *testing.T) {
	keepReserveThroughRollForTest(t)
	now := time.Now()
	owner := Score{
		AccountID: "owner", Provider: account.ProviderClaude,
		Headroom: 1, ShortHeadroom: 1, Fresh: true,
		WeeklyHeadroom: 0.20, WeeklyRemaining: 0.20, WeeklyResetAt: now.Add(10 * time.Hour),
	}
	healthy := claudeScore("healthy", claudePoolWindows(20, 20, 5, fourDays, twoHours))
	base := NewScheduler([]Score{owner, healthy}).WithReserves(map[string]float64{"owner": 0.05})
	if got := pickID(t, base.WithLiveSessionCounts(map[string]int{"owner": 2}), "owner", "healthy"); got != "owner" {
		t.Fatalf("pick with the owner carrying 2 = %q, want the owner still draining", got)
	}
	if got := pickID(t, base.WithLiveSessionCounts(map[string]int{"owner": 3}), "owner", "healthy"); got != "healthy" {
		t.Fatalf("pick with the owner carrying 3 = %q, want the spill: 15 points above its holdout allow 3", got)
	}
}

// Slots beyond the first need a 5h window that survives them. Drain
// eligibility checks the short FLOOR, not usableForNewSession's burn
// projection, so an account whose 5h window is already projected to run dry
// kept its full weekly allowance -- four sessions racing into a 5h wall that
// forces every one of them off mid-turn. The week cannot drain faster than the
// 5h window lets it, so the extra sessions buy nothing but that switch.
//
// RED without the clamp: 50% of the week and a 5h window projected to spend
// 2.8 against 0.3 left allowed 4, and took a new session at 1, 2 and 3 live
// ahead of a healthy account.
func TestAHotShortWindowDrainsOneSessionAtATime(t *testing.T) {
	now := time.Now()
	hot := Score{
		AccountID: "hot", Provider: account.ProviderClaude,
		Headroom: 0.3, ShortHeadroom: 0.3, Fresh: true,
		ShortWindowSeconds: 5 * 60 * 60, ShortResetAfterSeconds: 4 * 60 * 60,
		WeeklyHeadroom: 0.50, WeeklyRemaining: 0.50, WeeklyResetAt: now.Add(10 * time.Hour),
	}
	// Calibration: the burn projection refuses it as a steady-state account,
	// and it is still inside its window above the floor, so only the drain can
	// admit it.
	if hot.usableForNewSession() {
		t.Fatal("calibration: the 5h burn projection must refuse the hot account")
	}
	if !hot.endgameDrainEligible(now) {
		t.Fatal("calibration: the hot account must still drain its final task")
	}
	if got := hot.endgameAllowedSessions(); got != 1 {
		t.Fatalf("hot 5h window allows %d drain sessions, want 1", got)
	}
	healthy := claudeScore("healthy", claudePoolWindows(20, 20, 5, fourDays, twoHours))
	base := NewScheduler([]Score{hot, healthy})
	if got := pickID(t, base, "hot", "healthy"); got != "hot" {
		t.Fatalf("idle pick = %q, want the hot account to drain one final task", got)
	}
	if got := pickID(t, base.WithLiveSessionCounts(map[string]int{"hot": 1}), "hot", "healthy"); got != "healthy" {
		t.Fatalf("pick with the hot account carrying 1 = %q, want the spill", got)
	}
	// A cool 5h window at the same weekly level keeps the full ceiling.
	cool := hot
	cool.ShortHeadroom, cool.Headroom = 0.9, 0.9
	if got := cool.endgameAllowedSessions(); got != claudeEndgameMaxSessions {
		t.Fatalf("cool 5h window allows %d drain sessions, want the ceiling %d", got, claudeEndgameMaxSessions)
	}
}
