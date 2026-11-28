package selectacct

import (
	"math"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/discordwell/subrouter/account"
)

func setPacedAllowanceForTest(t *testing.T, on bool) {
	t.Helper()
	prev := PacedAllowance()
	SetPacedAllowance(on)
	t.Cleanup(func() { SetPacedAllowance(prev) })
}

// legacyTaper is endgameAllowedSessions as it stood before PATCHES §72,
// frozen here so an unmeasured allowance is held to it.
func legacyTaper(s Score) int {
	s = s.drainInputs()
	spendable := s.WeeklyHeadroom
	if s.Reserve > 0 && spendable > 0 {
		spendable = math.Max(0, spendable-s.Reserve)
	}
	allowed := int(math.Ceil(spendable / claudeEndgameSessionStep))
	if allowed < 1 {
		allowed = 1
	}
	if allowed > claudeEndgameMaxSessions {
		allowed = claudeEndgameMaxSessions
	}
	if allowed > 1 && !s.shortBurnSustainable() {
		allowed = 1
	}
	return allowed
}

func pacedFixture(now time.Time, spendable float64, untilRoll time.Duration) Score {
	return drainFixture(now, spendable, untilRoll, 0.9, time.Hour)
}

func pacedVerdict(score Score, pace Pace, live int, now time.Time) NewSessionVerdict {
	return pacedScheduler(score, pace, live).NewSessionVerdict(score.Provider, score.AccountID, now)
}

func pacedScheduler(score Score, pace Pace, live int) Scheduler {
	key := ScoreKey(score.Provider, score.AccountID)
	return NewScheduler([]Score{score}).
		withPaces(map[string]Pace{key: pace}).
		WithLiveSessionCounts(map[string]int{key: live})
}

func measuredPace(burnPerLive float64) Pace {
	return Pace{BurnPerLive: burnPerLive, BurnNow: burnPerLive, Source: "account", Measured: true}
}

func randomPacedScore(rng *rand.Rand, now time.Time) Score {
	score := Score{
		AccountID:              "a",
		Provider:               account.ProviderClaude,
		Headroom:               float64(1+rng.Intn(4)) / 4,
		ShortHeadroom:          float64(rng.Intn(21)) / 20,
		ShortWindowSeconds:     int64(rng.Intn(2)) * 18000,
		ShortResetAfterSeconds: int64(rng.Intn(18001)),
		WeeklyHeadroom:         float64(rng.Intn(21)) / 20,
		WeeklyRemaining:        float64(rng.Intn(21)) / 20,
		Fresh:                  true,
	}
	if rng.Intn(4) > 0 {
		score.WeeklyResetAt = now.Add(time.Duration(rng.Intn(48*3600)) * time.Second)
	}
	if rng.Intn(4) == 0 {
		score.Reserve = float64(rng.Intn(3)) / 10
	}
	return score
}

func TestPacedAllowanceSwitchOffIsTheLegacyTaperForAnyPace(t *testing.T) {
	setPacedAllowanceForTest(t, false)
	rng := rand.New(rand.NewSource(69))
	now := time.Now()
	for i := 0; i < 5000; i++ {
		score := randomPacedScore(rng, now)
		key := ScoreKey(account.ProviderClaude, "a")
		live := rng.Intn(8)
		bare := NewScheduler([]Score{score}).WithLiveSessionCounts(map[string]int{key: live})
		pace := Pace{BurnNow: float64(rng.Intn(5)) / 100}
		if rng.Intn(2) == 0 {
			pace.BurnPerLive, pace.Source = float64(1+rng.Intn(20))/1000, "account"
		}
		if rng.Intn(2) == 0 {
			pace.Backpressure = now.Add(-time.Duration(rng.Intn(3600)) * time.Second)
		}
		withTable := pacedScheduler(score, pace, live)
		for _, sched := range []Scheduler{bare, withTable} {
			s := sched.score(account.ProviderClaude, "a")
			got := s.allowance(now)
			if got.Sessions != legacyTaper(s) || got.Paced != 0 {
				t.Fatalf("switched off, allowance %+v, the legacy taper is %d: %+v", got, legacyTaper(s), s)
			}
		}
		if a, b := bare.NewSessionVerdict(account.ProviderClaude, "a", now), withTable.NewSessionVerdict(account.ProviderClaude, "a", now); a != b {
			t.Fatalf("switched off, a pace changed the verdict: %+v, want %+v", b, a)
		}
		candidates := []account.Account{{ID: "a", Provider: account.ProviderClaude, AuthMode: account.AuthModeOAuth}}
		bareRank, bareErr := bare.rankAt(candidates, true, now)
		tableRank, tableErr := withTable.rankAt(candidates, true, now)
		if (bareErr == nil) != (tableErr == nil) || !reflect.DeepEqual(bareRank, tableRank) {
			t.Fatalf("switched off, a pace changed the ranking: %v %v, want %v %v", tableRank, tableErr, bareRank, bareErr)
		}
	}
}

func TestPacedAllowanceUnmeasuredIsTheDefaultBurn(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	// The reference below is measured; §110 also routes on a measured burn,
	// which the default burn never stands in for. This pins §72's sizing.
	setEarlyDrainForTest(t, false)
	rng := rand.New(rand.NewSource(69))
	now := time.Now()
	raised := 0
	for i := 0; i < 5000; i++ {
		score := randomPacedScore(rng, now)
		key := ScoreKey(account.ProviderClaude, "a")
		live := rng.Intn(8)
		bare := NewScheduler([]Score{score}).WithLiveSessionCounts(map[string]int{key: live})
		readingsOnly := pacedScheduler(score, Pace{BurnNow: float64(rng.Intn(5)) / 100}, live)
		if score.ShortWindowSeconds <= 0 || score.ShortResetAfterSeconds <= 0 {
			for _, sched := range []Scheduler{bare, readingsOnly} {
				s := sched.score(account.ProviderClaude, "a")
				if got := s.allowance(now); got.Paced != 0 || got.Pace.Source != "" || got.Sessions != legacyTaper(s) {
					t.Fatalf("no 5h timing, unmeasured allowance %+v, want the legacy taper %d and no default burn", got, legacyTaper(s))
				}
			}
			continue
		}
		atDefault := pacedScheduler(score, measuredPace(0.015), live)
		want := atDefault.score(account.ProviderClaude, "a").allowance(now)
		for _, sched := range []Scheduler{bare, readingsOnly} {
			got := sched.score(account.ProviderClaude, "a").allowance(now)
			if got.Sessions != want.Sessions || got.Paced != want.Paced || got.Needed != want.Needed ||
				got.Pace.Source != "default" || got.Pace.BurnPerLive != 0.015 || got.Danger {
				t.Fatalf("unmeasured allowance %+v, want the default burn's %+v", got, want)
			}
		}
		verdict := atDefault.NewSessionVerdict(account.ProviderClaude, "a", now)
		candidates := []account.Account{{ID: "a", Provider: account.ProviderClaude, AuthMode: account.AuthModeOAuth}}
		wantRank, wantErr := atDefault.rankAt(candidates, true, now)
		for _, sched := range []Scheduler{bare, readingsOnly} {
			if got := sched.NewSessionVerdict(account.ProviderClaude, "a", now); got != verdict {
				t.Fatalf("unmeasured verdict %+v, want the default burn's %+v", got, verdict)
			}
			if rank, err := sched.rankAt(candidates, true, now); (err == nil) != (wantErr == nil) || !reflect.DeepEqual(rank, wantRank) {
				t.Fatalf("unmeasured ranking %v %v, want the default burn's %v %v", rank, err, wantRank, wantErr)
			}
		}
		if s := bare.score(account.ProviderClaude, "a"); want.Sessions > legacyTaper(s) {
			raised++
		}
	}
	if raised == 0 {
		t.Fatal("the default burn never raised an allowance past the legacy taper; this test proves nothing")
	}
	t.Logf("raised past the legacy taper by the default burn: %d of 5000", raised)
}

func TestPacedAllowanceRaisesABurndownAccountBehindPace(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	now := time.Now()
	score := pacedFixture(now, 0.60, 9*time.Hour)
	pace := measuredPace(0.015)
	got := pacedScheduler(score, pace, 4).PaceAllowance(account.ProviderClaude, "d1", now)
	if math.Abs(got.Needed-0.60/9/0.015) > 1e-9 || got.Taper != 4 || got.Paced != 5 || got.Sessions != 5 {
		t.Fatalf("allowance %+v, want needed 4.44, taper 4, paced 5, sessions 5", got)
	}
	if math.Abs(got.HoursToRoll-9) > 1e-6 || math.Abs(got.ProjectedWaste-(0.60-0.015*9)) > 1e-6 || !got.Danger {
		t.Fatalf("allowance %+v, want 9h to the roll and 0.465 projected waste, in danger", got)
	}
	fast := pace
	fast.BurnNow = 0.2
	if got := pacedScheduler(score, fast, 0).PaceAllowance(account.ProviderClaude, "d1", now); got.Danger || got.ProjectedWaste >= 0 {
		t.Fatalf("an account burning 0.2 an hour wastes nothing: %+v", got)
	}
	at4 := pacedVerdict(score, pace, 4, now)
	if !at4.Eligible || !at4.Burndown || at4.AtCap || at4.AllowedSessions != 5 {
		t.Fatalf("at 4 live the raised account must take a fifth: %+v", at4)
	}
	at5 := pacedVerdict(score, pace, 5, now)
	if at5.Eligible || !at5.AtCap || at5.Reason != VerdictAtCap || at5.AllowedSessions != 5 {
		t.Fatalf("at 5 live the raised account is at its cap: %+v", at5)
	}
}

func TestPacedAllowanceStopsAtTheCeiling(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	now := time.Now()
	got := pacedScheduler(pacedFixture(now, 0.80, 2*time.Hour), measuredPace(0.015), 0).
		PaceAllowance(account.ProviderClaude, "d1", now)
	if math.Abs(got.Needed-0.80/2/0.015) > 1e-9 || got.Ceiling != 20 || got.Paced != 20 || got.Sessions != 20 {
		t.Fatalf("allowance %+v, want needed 26.7 held at the default ceiling of 20", got)
	}
}

func TestPacedAllowanceBackpressureHoldsTheTaper(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	now := time.Now()
	score := pacedFixture(now, 0.60, 9*time.Hour)
	recent := measuredPace(0.015)
	recent.Backpressure = now.Add(-5 * time.Minute)
	got := pacedScheduler(score, recent, 0).PaceAllowance(account.ProviderClaude, "d1", now)
	if !got.Backpressured || got.Ceiling != claudeEndgameMaxSessions || got.Paced != 0 || got.Sessions != 4 {
		t.Fatalf("backpressured 5 min ago: %+v, want no raise and the taper of 4", got)
	}
	if v := pacedVerdict(score, recent, 0, now); v.AllowedSessions != 4 {
		t.Fatalf("backpressured verdict %+v, want allowed 4", v)
	}
	old := measuredPace(0.015)
	old.Backpressure = now.Add(-16 * time.Minute)
	got = pacedScheduler(score, old, 0).PaceAllowance(account.ProviderClaude, "d1", now)
	if got.Backpressured || got.Ceiling != PacedSessionCeiling() || got.Paced != 5 || got.Sessions != 5 {
		t.Fatalf("backpressured 16 min ago: %+v, want the raise to 5 back", got)
	}
}

func TestPacedAllowanceUnsustainableShortWindowHoldsOne(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	now := time.Now()
	score := drainFixture(now, 0.80, 5*time.Hour, 0.3, 4*time.Hour)
	if score.shortBurnSustainable() {
		t.Fatal("fixture drift: the 5h window is sustainable")
	}
	got := pacedScheduler(score, measuredPace(0.015), 0).PaceAllowance(account.ProviderClaude, "d1", now)
	if got.Paced != 0 || got.Sessions != 1 {
		t.Fatalf("unsustainable 5h window: %+v, want paced 0 and one session", got)
	}
	if v := pacedVerdict(score, measuredPace(0.015), 0, now); v.AllowedSessions != 1 {
		t.Fatalf("unsustainable verdict %+v, want allowed 1", v)
	}
}

func TestPacedAllowanceNearTheFloor(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	now := time.Now()
	pace := measuredPace(0.015)
	low := pacedScheduler(pacedFixture(now, 0.10, 2*time.Hour), pace, 0).PaceAllowance(account.ProviderClaude, "d1", now)
	if math.Abs(low.Needed-0.10/2/0.015) > 1e-9 || low.Paced != 4 || low.Taper != 2 || low.Sessions != 4 {
		t.Fatalf("spendable 0.10, 2h: %+v, want needed 3.33 rounded up to 4, past the taper of 2", low)
	}
	late := pacedScheduler(pacedFixture(now, 0.15, time.Hour), pace, 0).PaceAllowance(account.ProviderClaude, "d1", now)
	if math.Abs(late.Needed-10) > 1e-9 || late.Taper != 3 || late.Paced != 10 || late.Sessions != 10 {
		t.Fatalf("spendable 0.15, 1h: %+v, want needed 10 raised past the taper of 3 to 10", late)
	}
}

func TestPacedAllowanceOutsideBurndownCapsNothing(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	now := time.Now()
	score := pacedFixture(now, 0.90, 30*time.Hour)
	// Burning 0.03 an hour it spends its 90% by the roll, so it is in no
	// early burndown either (§110).
	pace := measuredPace(0.005)
	pace.BurnNow = 0.03
	if pacedScheduler(score, pace, 0).score(account.ProviderClaude, "d1").inBurndownWindow(now) {
		t.Fatal("fixture drift: the account is inside the burndown window")
	}
	for _, live := range []int{0, 6, 12} {
		v := pacedVerdict(score, pace, live, now)
		if v.AllowedSessions != 0 || !v.Eligible || v.AtCap || v.Burndown {
			t.Fatalf("live %d outside burndown: %+v, want allowed 0, eligible whatever the count", live, v)
		}
		if a := pacedScheduler(score, pace, live).PaceAllowance(account.ProviderClaude, "d1", now); a.Paced != 6 {
			t.Fatalf("live %d outside burndown: %+v, want the raise of 6 measured for the log", live, a)
		}
	}
}

func TestPacedAllowanceLoweringUnderLoadMovesNothing(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	now := time.Now()
	score := pacedFixture(now, 0.80, 5*time.Hour)
	score.Reserve = 0
	raisedPace := measuredPace(0.015)
	loweredPace := raisedPace
	loweredPace.Backpressure = now.Add(-time.Minute)
	if v := pacedVerdict(score, raisedPace, 10, now); !v.Eligible || v.AllowedSessions != 11 {
		t.Fatalf("raised to 11, live 10 takes an eleventh: %+v", v)
	}
	raised := pacedScheduler(score, raisedPace, 11)
	lowered := pacedScheduler(score, loweredPace, 11)
	v := lowered.NewSessionVerdict(account.ProviderClaude, "d1", now)
	if v.Eligible || !v.AtCap || v.AllowedSessions != 4 || v.Evicted {
		t.Fatalf("lowered to 4 under 11 live: %+v, want at cap, refused, not evicted", v)
	}
	type exits struct{ verdictEvicted, evicted, holdout, capacity, sticky bool }
	read := func(s Scheduler) exits {
		return exits{
			verdictEvicted: s.NewSessionVerdict(account.ProviderClaude, "d1", now).Evicted,
			evicted:        s.Evicted(account.ProviderClaude, "d1"),
			holdout:        s.InHoldout(account.ProviderClaude, "d1"),
			capacity:       s.CapacityEvicting(account.ProviderClaude, "d1"),
			sticky:         s.UsableForStickySession(account.ProviderClaude, "d1"),
		}
	}
	want := exits{sticky: true}
	if got := read(raised); got != want {
		t.Fatalf("raised exits %+v, want %+v", got, want)
	}
	if got := read(lowered); got != want {
		t.Fatalf("lowering moved a session: exits %+v, want %+v", got, want)
	}
}

func TestPacedAllowanceSwitchOffKeepsTheLegacyNumbers(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		spendable float64
		untilRoll time.Duration
		pace      Pace
		onAllowed int
	}{
		{0.60, 9 * time.Hour, measuredPace(0.015), 5},
		{0.80, 5 * time.Hour, measuredPace(0.015), 11},
		{0.15, time.Hour, measuredPace(0.015), 10},
		{0.17, 90 * time.Minute, Pace{}, 8},
	} {
		score := pacedFixture(now, tc.spendable, tc.untilRoll)
		pace := tc.pace
		setPacedAllowanceForTest(t, true)
		if got := pacedScheduler(score, pace, 0).PaceAllowance(account.ProviderClaude, "d1", now); got.Sessions != tc.onAllowed {
			t.Fatalf("switch on: %+v, want %d", got, tc.onAllowed)
		}
		SetPacedAllowance(false)
		sched := pacedScheduler(score, pace, 0)
		got := sched.PaceAllowance(account.ProviderClaude, "d1", now)
		if got.Paced != 0 || got.Sessions != legacyTaper(sched.score(account.ProviderClaude, "d1")) {
			t.Fatalf("switch off: %+v, want the legacy taper and no raise", got)
		}
		if v := sched.NewSessionVerdict(account.ProviderClaude, "d1", now); v.AllowedSessions != legacyTaper(sched.score(account.ProviderClaude, "d1")) {
			t.Fatalf("switch off verdict %+v, want the legacy taper", v)
		}
	}
}

func paceRef(now time.Time) (*SchedulerRef, *time.Time) {
	clock := now
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.pace.now = func() time.Time { return clock }
	return ref, &clock
}

func reading(id string, measuredAt, resetAt time.Time, remaining float64) Score {
	return Score{
		AccountID: id, Provider: account.ProviderClaude, Fresh: true, MeasuredAt: measuredAt,
		Headroom: remaining, ShortHeadroom: 0.9, ShortWindowSeconds: 5 * 3600, ShortResetAfterSeconds: 3600,
		WeeklyHeadroom: remaining, WeeklyRemaining: remaining, WeeklyResetAt: resetAt,
	}
}

func refPace(ref *SchedulerRef, id string) Pace {
	return ref.Get().score(account.ProviderClaude, id).Pace
}

func TestPaceTrackerReadsBurnPerLiveSession(t *testing.T) {
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	reset := t0.Add(9 * time.Hour)
	ref, _ := paceRef(t0.Add(time.Hour))
	key := ScoreKey(account.ProviderClaude, "a")
	ref.ObserveLiveSessions(map[string]int{key: 2}, t0)
	ref.Set(NewScheduler([]Score{reading("a", t0, reset, 0.63)}))
	ref.ObserveLiveSessions(map[string]int{key: 2}, t0.Add(30*time.Minute))
	ref.ObserveLiveSessions(map[string]int{key: 2}, t0.Add(time.Hour))
	ref.Set(NewScheduler([]Score{reading("a", t0.Add(time.Hour), reset, 0.60)}))
	got := refPace(ref, "a")
	if math.Abs(got.BurnPerLive-0.015) > 1e-9 || math.Abs(got.BurnNow-0.03) > 1e-9 || got.Source != "account" {
		t.Fatalf("pace %+v, want 0.015 per live session-hour and 0.03 per hour from the account", got)
	}
}

func TestPaceTrackerNewEpochResetsTheSeries(t *testing.T) {
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	reset := t0.Add(9 * time.Hour)
	ref, _ := paceRef(t0.Add(70 * time.Minute))
	key := ScoreKey(account.ProviderClaude, "a")
	ref.ObserveLiveSessions(map[string]int{key: 2}, t0)
	ref.Set(NewScheduler([]Score{reading("a", t0, reset, 0.63)}))
	ref.ObserveLiveSessions(map[string]int{key: 2}, t0.Add(time.Hour))
	ref.Set(NewScheduler([]Score{reading("a", t0.Add(time.Hour), reset, 0.60)}))
	if refPace(ref, "a").BurnPerLive == 0 {
		t.Fatal("fixture drift: the first epoch is unmeasured")
	}
	ref.Set(NewScheduler([]Score{reading("a", t0.Add(70*time.Minute), reset.Add(7*24*time.Hour), 1)}))
	if got := refPace(ref, "a"); got != (Pace{}) {
		t.Fatalf("after the roll the series restarts unmeasured, got %+v", got)
	}
}

func TestPaceTrackerIgnoresAnOlderOrEqualReading(t *testing.T) {
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	reset := t0.Add(9 * time.Hour)
	ref, _ := paceRef(t0.Add(time.Hour))
	key := ScoreKey(account.ProviderClaude, "a")
	ref.ObserveLiveSessions(map[string]int{key: 2}, t0)
	ref.Set(NewScheduler([]Score{reading("a", t0, reset, 0.63)}))
	ref.ObserveLiveSessions(map[string]int{key: 2}, t0.Add(time.Hour))
	ref.Set(NewScheduler([]Score{reading("a", t0.Add(time.Hour), reset, 0.60)}))
	want := refPace(ref, "a")
	ref.Set(NewScheduler([]Score{reading("a", t0.Add(time.Hour), reset, 0.40)}))
	ref.Set(NewScheduler([]Score{reading("a", t0.Add(30*time.Minute), reset, 0.20)}))
	if got := refPace(ref, "a"); got != want || want.BurnPerLive == 0 {
		t.Fatalf("an equal or older reading moved the pace: %+v, want %+v", got, want)
	}
}

func TestPaceTrackerLowerUsedNeverBurnsNegative(t *testing.T) {
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	reset := t0.Add(9 * time.Hour)
	ref, clock := paceRef(t0.Add(time.Hour))
	key := ScoreKey(account.ProviderClaude, "a")
	ref.ObserveLiveSessions(map[string]int{key: 2}, t0)
	ref.Set(NewScheduler([]Score{reading("a", t0, reset, 0.60)}))
	ref.ObserveLiveSessions(map[string]int{key: 2}, t0.Add(time.Hour))
	ref.Set(NewScheduler([]Score{reading("a", t0.Add(time.Hour), reset, 0.62)}))
	got := refPace(ref, "a")
	if got.BurnNow != 0 || got.BurnPerLive != 0 {
		t.Fatalf("a lagging lower reading burned %+v, want zero", got)
	}
	*clock = t0.Add(2 * time.Hour)
	ref.ObserveLiveSessions(map[string]int{key: 2}, t0.Add(2*time.Hour))
	ref.Set(NewScheduler([]Score{reading("a", t0.Add(2*time.Hour), reset, 0.57)}))
	got = refPace(ref, "a")
	if math.Abs(got.BurnNow-0.015) > 1e-9 || math.Abs(got.BurnPerLive-0.0075) > 1e-9 {
		t.Fatalf("after the lag %+v, want 0.03 over 2h from the higher first reading", got)
	}
}

func TestPaceTrackerShortSpanIsUnmeasured(t *testing.T) {
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	reset := t0.Add(9 * time.Hour)
	ref, clock := paceRef(t0.Add(20 * time.Minute))
	key := ScoreKey(account.ProviderClaude, "a")
	ref.ObserveLiveSessions(map[string]int{key: 6}, t0)
	ref.Set(NewScheduler([]Score{reading("a", t0, reset, 0.63)}))
	ref.ObserveLiveSessions(map[string]int{key: 6}, t0.Add(20*time.Minute))
	ref.Set(NewScheduler([]Score{reading("a", t0.Add(20*time.Minute), reset, 0.60)}))
	if got := refPace(ref, "a"); got != (Pace{}) {
		t.Fatalf("a 20-minute series read %+v, want unmeasured", got)
	}
	*clock = t0.Add(30 * time.Minute)
	ref.ObserveLiveSessions(map[string]int{key: 6}, t0.Add(30*time.Minute))
	ref.Set(NewScheduler([]Score{reading("a", t0.Add(30*time.Minute), reset, 0.60)}))
	if got := refPace(ref, "a"); math.Abs(got.BurnPerLive-0.01) > 1e-9 {
		t.Fatalf("a 30-minute series read %+v, want 0.03 over 3 session-hours", got)
	}
}

func TestPaceTrackerFallsBackToThePool(t *testing.T) {
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	reset := t0.Add(9 * time.Hour)
	ref, _ := paceRef(t0.Add(time.Hour))
	busy := ScoreKey(account.ProviderClaude, "busy")
	ref.ObserveLiveSessions(map[string]int{busy: 2}, t0)
	ref.Set(NewScheduler([]Score{reading("busy", t0, reset, 0.63), reading("idle", t0, reset, 0.80)}))
	ref.ObserveLiveSessions(map[string]int{busy: 2}, t0.Add(time.Hour))
	ref.Set(NewScheduler([]Score{reading("busy", t0.Add(time.Hour), reset, 0.60), reading("idle", t0.Add(time.Hour), reset, 0.79)}))
	got := refPace(ref, "idle")
	if got.Source != "pool" || math.Abs(got.BurnPerLive-0.02) > 1e-9 || math.Abs(got.BurnNow-0.01) > 1e-9 {
		t.Fatalf("idle pace %+v, want the pool's (0.03+0.01)/2 = 0.02 per live session-hour and its own 0.01 per hour", got)
	}
	if own := refPace(ref, "busy"); own.Source != "account" || math.Abs(own.BurnPerLive-0.015) > 1e-9 {
		t.Fatalf("busy pace %+v, want its own 0.015", own)
	}
}

func TestPaceTrackerRaisesTheVerdictOnGet(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	now := time.Now()
	t0 := now.Add(-time.Hour)
	reset := now.Add(9 * time.Hour)
	ref, _ := paceRef(now)
	key := ScoreKey(account.ProviderClaude, "a")
	withFable := func(score Score) Score {
		fable := score
		fable.ModelScores = nil
		score.ModelScores = map[string]Score{claudeFablePoolKey: fable}
		return score
	}
	ref.ObserveLiveSessions(map[string]int{key: 2}, t0)
	ref.Set(NewScheduler([]Score{withFable(reading("a", t0, reset, 0.63))}))
	ref.ObserveLiveSessions(map[string]int{key: 2}, now)
	ref.Set(NewScheduler([]Score{withFable(reading("a", now, reset, 0.60))}))
	live := map[string]int{key: 4}
	verdict := ref.Get().WithLiveSessionCounts(live).NewSessionVerdict(account.ProviderClaude, "a", now)
	if !verdict.Eligible || verdict.AllowedSessions != 5 {
		t.Fatalf("verdict on ref.Get() %+v, want raised to 5 and eligible at 4 live", verdict)
	}
	general := ref.Get().PaceAllowance(account.ProviderClaude, "a", now).Pace
	fable := ref.Get().ForModel("claude-fable").PaceAllowance(account.ProviderClaude, "a", now).Pace
	if general.BurnPerLive == 0 || fable != general {
		t.Fatalf("Fable pool pace %+v, want the account's %+v", fable, general)
	}
	if v := ref.Get().ForModel("claude-fable").WithLiveSessionCounts(live).NewSessionVerdict(account.ProviderClaude, "a", now); v.AllowedSessions != 5 {
		t.Fatalf("Fable pool verdict %+v, want allowed 5", v)
	}
}

func TestPaceTrackerReadsTheRefreshPath(t *testing.T) {
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	reset := t0.Add(9 * time.Hour)
	ref, clock := paceRef(t0)
	key := ScoreKey(account.ProviderClaude, "a")
	ref.ObserveLiveSessions(map[string]int{key: 2}, t0)
	ref.FinishRefresh(NewScheduler([]Score{reading("a", t0, reset, 0.63)}), true)
	*clock = t0.Add(time.Hour)
	ref.ObserveLiveSessions(map[string]int{key: 2}, t0.Add(time.Hour))
	ref.FinishRefresh(NewScheduler([]Score{reading("a", t0.Add(time.Hour), reset, 0.60)}), true)
	got := refPace(ref, "a")
	if math.Abs(got.BurnPerLive-0.015) > 1e-9 || math.Abs(got.BurnNow-0.03) > 1e-9 || got.Source != "account" {
		t.Fatalf("pace after two refreshes %+v, want 0.015 per live session-hour and 0.03 per hour from the account", got)
	}
	*clock = t0.Add(90 * time.Minute)
	ref.ObserveLiveSessions(map[string]int{key: 2}, t0.Add(90*time.Minute))
	ref.FinishRefresh(NewScheduler([]Score{reading("a", t0.Add(90*time.Minute), reset, 0.40)}), false)
	if after := refPace(ref, "a"); after != got {
		t.Fatalf("a failed refresh moved the pace to %+v, want %+v", after, got)
	}
}

func TestPaceTrackerExtrapolatesLiveHoursToAReadingBetweenTicks(t *testing.T) {
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	reset := t0.Add(9 * time.Hour)
	ref, clock := paceRef(t0)
	key := ScoreKey(account.ProviderClaude, "a")
	ref.ObserveLiveSessions(map[string]int{key: 4}, t0)
	ref.Set(NewScheduler([]Score{reading("a", t0, reset, 0.63)}))
	*clock = t0.Add(time.Hour)
	ref.ObserveLiveSessions(map[string]int{key: 4}, t0.Add(time.Hour))
	*clock = t0.Add(90 * time.Minute)
	ref.Set(NewScheduler([]Score{reading("a", t0.Add(90*time.Minute), reset, 0.54)}))
	got := refPace(ref, "a")
	if math.Abs(got.BurnPerLive-0.015) > 1e-9 || got.Source != "account" {
		t.Fatalf("pace %+v, want 0.09 over 6 extrapolated session-hours = 0.015", got)
	}
}

type paceStep struct {
	at        time.Duration
	live      map[string]int
	remaining map[string]float64
}

func runPaceSteps(ref *SchedulerRef, clock *time.Time, t0, reset time.Time, steps ...paceStep) {
	for _, step := range steps {
		at := t0.Add(step.at)
		*clock = at
		counts := map[string]int{}
		for id, live := range step.live {
			counts[ScoreKey(account.ProviderClaude, id)] = live
		}
		ref.ObserveLiveSessions(counts, at)
		var scores []Score
		for id, remaining := range step.remaining {
			scores = append(scores, reading(id, at, reset, remaining))
		}
		ref.Set(NewScheduler(scores))
	}
}

func TestPaceTrackerBackpressureKeepsTheAccountsBurn(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	reset := t0.Add(9 * time.Hour)
	ref, clock := paceRef(t0)
	step := func(at time.Duration, remaining float64) paceStep {
		return paceStep{at: at, live: map[string]int{"a": 2}, remaining: map[string]float64{"a": remaining}}
	}
	runPaceSteps(ref, clock, t0, reset, step(0, 0.47), step(time.Hour, 0.44))
	ref.NotePaceBackpressure(account.ProviderClaude, "a", t0.Add(time.Hour))
	got := ref.Get().PaceAllowance(account.ProviderClaude, "a", t0.Add(time.Hour))
	if !got.Backpressured || got.Ceiling != claudeEndgameMaxSessions || got.Pace.Source != "account" || math.Abs(got.Pace.BurnPerLive-0.015) > 1e-9 {
		t.Fatalf("backpressured %+v, want the ceiling at 4 and the account's own 0.015 per live session-hour", got)
	}
	runPaceSteps(ref, clock, t0, reset, step(76*time.Minute, 0.432))
	got = ref.Get().PaceAllowance(account.ProviderClaude, "a", t0.Add(76*time.Minute))
	if got.Backpressured || got.Ceiling != PacedSessionCeiling() || got.Pace.Source != "account" || math.Abs(got.Pace.BurnPerLive-0.015) > 1e-9 {
		t.Fatalf("16 minutes on %+v, want the ceiling back at the paced ceiling on the same 0.015 per live session-hour", got)
	}
}

func TestPaceTrackerAStaleReadingDoesNotStretchTheSpan(t *testing.T) {
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	reset := t0.Add(20 * time.Hour)
	ref, clock := paceRef(t0)
	step := func(at time.Duration, remaining float64) paceStep {
		return paceStep{at: at, live: map[string]int{"a": 2}, remaining: map[string]float64{"a": remaining}}
	}
	runPaceSteps(ref, clock, t0, reset, step(0, 0.63), step(4*time.Hour, 0.60))
	if got := refPace(ref, "a"); got != (Pace{}) {
		t.Fatalf("one reading in the window and one four hours old read %+v, want unmeasured", got)
	}
	runPaceSteps(ref, clock, t0, reset, step(4*time.Hour+40*time.Minute, 0.59))
	got := refPace(ref, "a")
	if math.Abs(got.BurnNow-0.015) > 1e-9 || math.Abs(got.BurnPerLive-0.0075) > 1e-9 {
		t.Fatalf("pace %+v, want 0.01 over the window's 40 minutes: 0.015 an hour, 0.0075 per live session-hour", got)
	}
}
