package selectacct

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/discordwell/subrouter/account"
)

// PATCHES §55: for a local, unreserved Claude account the drain's session cap,
// its 5h clamp and its 5% floor read the measured meters, not our own
// 0.02-per-request debit.

// setMeasuredInputsForTest sets §55's process-wide switch for the rest of the
// test and restores it in t.Cleanup. Only for tests that are not t.Parallel.
func setMeasuredInputsForTest(t *testing.T, on bool) {
	t.Helper()
	prev := EndgameMeasuredInputs()
	SetEndgameMeasuredInputs(on)
	t.Cleanup(func() { SetEndgameMeasuredInputs(prev) })
}

// legacyEndgameAllowedSessions is the allowance as it stood before §55. Do not
// edit: it is the baseline the switch, turned off, must reproduce.
func legacyEndgameAllowedSessions(s Score) int {
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

// drainFixture is a local Claude account as a usage reading leaves it: weekly
// headroom weekly with the roll untilRoll away, 5h headroom short resetting
// t5 from now. Headroom is the lesser meter, as scoreFromLimitWindows makes it.
func drainFixture(now time.Time, weekly float64, untilRoll time.Duration, short float64, t5 time.Duration) Score {
	return Score{
		AccountID: "d1", Provider: account.ProviderClaude, Fresh: true,
		Headroom: math.Min(weekly, short), ShortHeadroom: short,
		ShortWindowSeconds: 5 * 3600, ShortResetAfterSeconds: int64(t5.Seconds()),
		WeeklyHeadroom: weekly, WeeklyRemaining: weekly, WeeklyResetAt: now.Add(untilRoll),
	}
}

// acctPAt0003 is acct-p on the box at 00:03 CDT 10/10 (the sampler):
// WeeklyHeadroom 0.1284 undebited, 5h 46% used resetting in 3459 s, the roll
// 14259 s away.
func acctPAt0003(now time.Time) Score {
	return Score{
		AccountID: "acct-p", Provider: account.ProviderClaude, Fresh: true,
		Headroom: 0.1284, ShortHeadroom: 0.54, ShortWindowSeconds: 5 * 3600, ShortResetAfterSeconds: 3459,
		WeeklyHeadroom: 0.1284, WeeklyRemaining: 0.14, WeeklyResetAt: now.Add(14259 * time.Second),
	}
}

func acctPVerdict(score Score, live, debits int, now time.Time) NewSessionVerdict {
	key := ScoreKey(account.ProviderClaude, score.AccountID)
	return NewScheduler([]Score{score}).
		WithLiveSessionCounts(map[string]int{key: live}).
		WithLiveDebits(map[string]int{key: debits}).
		NewSessionVerdict(account.ProviderClaude, score.AccountID, now)
}

// The 10/10 pattern: four routed requests since the refresh took the debited
// ladder to 1 and held acct-p at_cap at ONE live session, while its measured
// ladder said 3.
func TestMeasuredInputsLadderIgnoresTheDebit(t *testing.T) {
	now := time.Now()
	setMeasuredInputsForTest(t, false)
	if v := acctPVerdict(acctPAt0003(now), 1, 4, now); v.Reason != VerdictAtCap || v.AllowedSessions != 1 {
		t.Fatalf("switch off: %+v, want today's at_cap at an allowance of 1", v)
	}
	setMeasuredInputsForTest(t, true)
	if v := acctPVerdict(acctPAt0003(now), 1, 4, now); v.Reason != VerdictBurndown || v.AllowedSessions != 3 {
		t.Fatalf("switch on: %+v, want burndown at the measured ladder's 3", v)
	}
}

// Thirty debits take the debited 5h meter to its 0.01 floor; the drain's 5%
// floor reads the measured meter with the switch on. A 5h window that really
// is below the floor still refuses either way.
func TestMeasuredInputsFloorIgnoresTheDebit(t *testing.T) {
	now := time.Now()
	spent := acctPAt0003(now)
	spent.ShortHeadroom = 0.04
	for _, on := range []bool{false, true} {
		setMeasuredInputsForTest(t, on)
		want := VerdictShortFloor
		if on {
			want = VerdictBurndown
		}
		if v := acctPVerdict(acctPAt0003(now), 0, 30, now); v.Reason != want {
			t.Errorf("switch %v, 30 debits: %+v, want %s", on, v, want)
		}
		if v := acctPVerdict(spent, 0, 30, now); v.Eligible {
			t.Errorf("switch %v: measured 5h at 0.04 is eligible: %+v", on, v)
		}
	}
}

// Outside the drain, an account §55 covers keeps the debit: steady-state pace
// and the new-session 5% floor read the debited meters, switch on or off.
// Thirteen debits take the 5h meter from 0.30 to 0.04, under the floor, and
// the week from 0.60 to 0.34, three days from the roll.
func TestMeasuredInputsLeavePaceAndTheNewSessionFloorDebited(t *testing.T) {
	now := time.Now()
	fixture := drainFixture(now, 0.60, 72*time.Hour, 0.30, time.Minute)
	measuredPace, ok := fixture.weeklyPace(now)
	if !ok {
		t.Fatal("calibration: no weekly pace on the undebited fixture")
	}
	working := ApplyLiveDebit(fixture, 13)
	for _, on := range []bool{false, true} {
		setMeasuredInputsForTest(t, on)
		if on && !working.drainReadsMeasured() {
			t.Fatalf("calibration: switch on, the fixture is outside §55's scope: %+v", working)
		}
		if working.usableForNewSession() {
			t.Errorf("switch %v: usable on the measured 5h (0.30), want refused on the debited 0.04", on)
		}
		if pace, ok := working.weeklyPace(now); !ok || pace >= measuredPace {
			t.Errorf("switch %v: pace %.3g not below the undebited %.3g", on, pace, measuredPace)
		}
	}
}

// The new-session burn projection keeps the debit too, switch on or off. 20
// debits take the 5h meter from 0.50 to 0.10: above the floor, so only the
// projection can refuse it (0.90 used over 4h, carried 1h, is 0.225), while
// the measured 0.50 projects 0.125 and would pass.
func TestMeasuredInputsLeaveTheNewSessionBurnProjectionDebited(t *testing.T) {
	now := time.Now()
	fixture := drainFixture(now, 0.60, 72*time.Hour, 0.50, time.Hour)
	if !fixture.usableForNewSession() {
		t.Fatalf("calibration: the undebited fixture is refused, so the measured 5h cannot pass: %+v", fixture)
	}
	working := ApplyLiveDebit(fixture, 20)
	if working.ShortHeadroom < MinShortHeadroomFloor {
		t.Fatalf("calibration: debited 5h %.3g is under the floor, so the projection never runs", working.ShortHeadroom)
	}
	for _, on := range []bool{false, true} {
		setMeasuredInputsForTest(t, on)
		if on && !working.drainReadsMeasured() {
			t.Fatalf("calibration: switch on, the fixture is outside §55's scope: %+v", working)
		}
		if working.usableForNewSession() {
			t.Errorf("switch %v: usable on the measured 5h projection (0.50); want refused on the debited 0.10", on)
		}
	}
}

// The soft reserve gate stays on the debited meter either way. A reserved
// account is outside §55's scope, so this catches only an add-back that skips
// drainReadsMeasured; an in-scope account has no reserve to gate on. The
// fixture rolls in 3h, so PATCHES §60's release is held off to keep its
// reserve.
func TestMeasuredInputsLeaveTheReserveGateDebited(t *testing.T) {
	keepReserveThroughRollForTest(t)
	now := time.Now()
	score := drainFixture(now, 0.07, 3*time.Hour, 0.80, time.Hour)
	key := ScoreKey(account.ProviderClaude, "d1")
	sched := NewScheduler([]Score{score}).
		WithReserves(map[string]float64{"d1": 0.05}).
		WithLiveDebits(map[string]int{key: 2})
	for _, on := range []bool{false, true} {
		setMeasuredInputsForTest(t, on)
		if !sched.score(account.ProviderClaude, "d1").belowWeeklyReserve() {
			t.Errorf("switch %v: 0.07 measured, 0.03 debited, reserve 0.05: not below the reserve", on)
		}
		if v := sched.NewSessionVerdict(account.ProviderClaude, "d1", now); v.Eligible {
			t.Errorf("switch %v: eligible below the reserve: %+v", on, v)
		}
	}
}

// §55 covers neither a reserved account, nor a weekly-only one, nor Codex,
// nor a peer's: each keeps the debited ladder.
func TestMeasuredInputsSkipReservedCodexAndPeers(t *testing.T) {
	setMeasuredInputsForTest(t, true)
	now := time.Now()
	twin := drainFixture(now, 0.30, 10*time.Hour, 0.90, 2*time.Hour)
	if got := ApplyLiveDebit(twin, 10).endgameAllowedSessions(time.Now()); got != 4 {
		t.Fatalf("calibration: the in-scope twin allows %d with 10 debits, want the measured ladder's 4", got)
	}
	for name, mutate := range map[string]func(*Score){
		"reserved":               func(s *Score) { s.Reserve = 0.05 },
		"weekly-only":            func(s *Score) { s.WeeklyOnly = true },
		"a peer with no verdict": func(s *Score) { s.Remote = true },
		"codex":                  func(s *Score) { s.Provider = account.ProviderCodex },
	} {
		score := twin
		mutate(&score)
		working := ApplyLiveDebit(score, 10)
		if got, want := working.endgameAllowedSessions(time.Now()), legacyEndgameAllowedSessions(working); got != want {
			t.Errorf("%s: allowed %d, want the debited ladder's %d", name, got, want)
		}
	}
}

// The ladder's 5h clamp reads the same meter as its weekly input.
func TestMeasuredInputsClampReadsTheMeasuredFiveHour(t *testing.T) {
	now := time.Now()
	// 20 debits: weekly 0.20 and 5h 0.10 debited, whose projection fails
	// (0.90 used over 4h, carried 1h, is 0.225); the measured 0.50 projects
	// 0.125 to the reset.
	score := ApplyLiveDebit(drainFixture(now, 0.60, 20*time.Hour, 0.50, time.Hour), 20)
	setMeasuredInputsForTest(t, false)
	if got := score.endgameAllowedSessions(time.Now()); got != 1 {
		t.Fatalf("switch off: allowed %d, want 1 by the clamp on the debited 5h", got)
	}
	setMeasuredInputsForTest(t, true)
	if got := score.endgameAllowedSessions(time.Now()); got != 4 {
		t.Fatalf("switch on: allowed %d, want 4 on the measured 5h", got)
	}
}

// The Fable pool's verdict reads the measured meters too. The proxy stamps
// Provider on the ACCOUNT score only (scoreFromUsageWindows), so the pool is
// covered only because ForModel's inheritAccountFacts carries it.
func TestMeasuredInputsCoverTheFablePool(t *testing.T) {
	setMeasuredInputsForTest(t, true)
	now := time.Now()
	// General 86% used and Fable 50%: the Fable pool's week is 0.14, a
	// measured ladder of 3 that four debits take to 0.06 and 1.
	acct := ScoreFromLimitWindowsAt("acct-p", 0, claudePoolWindows(86, 50, 46, 14259, 3459), now)
	acct.Provider = account.ProviderClaude
	key := ScoreKey(account.ProviderClaude, "acct-p")
	pool := NewScheduler([]Score{acct}).ForModel("claude-fable").
		WithLiveSessionCounts(map[string]int{key: 1}).
		WithLiveDebits(map[string]int{key: 4})
	if v := pool.NewSessionVerdict(account.ProviderClaude, "acct-p", now); v.Reason != VerdictBurndown || v.AllowedSessions != 3 {
		t.Fatalf("Fable pool verdict %+v, want burndown at the measured ladder's 3", v)
	}
}

// With the switch off, the allowance is exactly the pre-§55 one: on raw and
// model-pool scores, reserved or not, local or a verdict-less peer, Claude or
// Codex, with and without live debits.
func TestEndgameAllowanceSwitchOffIsToday(t *testing.T) {
	setMeasuredInputsForTest(t, false)
	setPacedAllowanceForTest(t, false)
	rng := rand.New(rand.NewSource(55))
	now := time.Now()
	for i := 0; i < 5000; i++ {
		var score Score
		if rng.Intn(4) == 0 {
			windows := claudePoolWindows(rng.Float64()*100, rng.Float64()*100, rng.Float64()*95,
				int64(rng.Intn(30*3600)), int64(1+rng.Intn(5*3600)))
			base := ScoreFromLimitWindowsAt("d1", 0, windows, now)
			base.Provider = account.ProviderClaude
			score = NewScheduler([]Score{base}).ForModel([]string{"claude-fable", "claude-opus", ""}[rng.Intn(3)]).
				measuredScore(account.ProviderClaude, "d1")
		} else {
			score = drainFixture(now, rng.Float64()*0.5, time.Duration(rng.Intn(26*3600)-3600)*time.Second,
				0.02+rng.Float64()*0.98, time.Duration(rng.Intn(5*3600+1))*time.Second)
			switch rng.Intn(8) {
			case 0:
				score.Provider = account.ProviderCodex
			case 1:
				score.WeeklyOnly = true
			case 2:
				score.Remote = true
			case 3:
				score.ShortWindowSeconds = 0
			}
		}
		if rng.Intn(3) == 0 {
			score.Reserve = []float64{0.05, 0.10, 0.20}[rng.Intn(3)]
		}
		working := ApplyLiveDebit(score, rng.Intn(31))
		if got, want := working.endgameAllowedSessions(time.Now()), legacyEndgameAllowedSessions(working); got != want {
			t.Fatalf("switch off: allowed %d, pre-§55 %d: %+v", got, want, working)
		}
	}
}
