package selectacct

import (
	"math"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

func setPacedSessionCeilingForTest(t *testing.T, ceiling int) {
	t.Helper()
	prev := PacedSessionCeiling()
	SetPacedSessionCeiling(ceiling)
	t.Cleanup(func() { SetPacedSessionCeiling(prev) })
}

func pacedAllowance(score Score, pace Pace, now time.Time) Allowance {
	return pacedScheduler(score, pace, 0).PaceAllowance(account.ProviderClaude, score.AccountID, now)
}

func TestPacedAllowanceCountsTimeLeftWithNoMeasuredBurn(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	now := time.Now()
	score := pacedFixture(now, 0.17, 90*time.Minute)
	got := pacedAllowance(score, Pace{}, now)
	if math.Abs(got.Needed-0.17/1.5/0.015) > 1e-9 || got.Taper != 3 || got.Paced != 8 || got.Sessions != 8 {
		t.Fatalf("17%% left 1.5h from the roll, unmeasured: %+v, want needed 7.56 rounded up to 8 over the taper of 3", got)
	}
	if got.Pace.Source != "default" || got.Pace.BurnPerLive != 0.015 || got.Pace.BurnNow != 0 || got.Danger || got.ProjectedWaste != 0 {
		t.Fatalf("unmeasured pace %+v danger %v waste %v, want the default 0.015 and no measured burn or danger", got.Pace, got.Danger, got.ProjectedWaste)
	}
	if v := pacedVerdict(score, Pace{}, 7, now); !v.Eligible || v.AllowedSessions != 8 {
		t.Fatalf("at 7 live the unmeasured account takes an eighth: %+v", v)
	}
}

func TestPacedAllowanceRaisesWheneverTheNeedBeatsTheTaper(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	now := time.Now()
	for _, tc := range []struct {
		name      string
		spendable float64
		untilRoll time.Duration
		pace      Pace
		needed    float64
		taper     int
		paced     int
	}{
		{"10% 2h unmeasured", 0.10, 2 * time.Hour, Pace{}, 0.10 / 2 / 0.015, 2, 4},
		{"10% 20h unmeasured", 0.10, 20 * time.Hour, Pace{}, 0.10 / 20 / 0.015, 2, 0},
		{"17% 1.5h measured 0.03", 0.17, 90 * time.Minute, measuredPace(0.03), 0.17 / 1.5 / 0.03, 3, 4},
	} {
		got := pacedAllowance(pacedFixture(now, tc.spendable, tc.untilRoll), tc.pace, now)
		want := max(tc.taper, tc.paced)
		if math.Abs(got.Needed-tc.needed) > 1e-9 || got.Taper != tc.taper || got.Paced != tc.paced || got.Sessions != want {
			t.Fatalf("%s: %+v, want needed %.2f, taper %d, paced %d, sessions %d", tc.name, got, tc.needed, tc.taper, tc.paced, want)
		}
	}
}

func TestPacedAllowanceTransientThrottleHoldsTheTaperWithNoMeasuredBurn(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	now := time.Now()
	got := pacedAllowance(pacedFixture(now, 0.17, 90*time.Minute), Pace{Backpressure: now.Add(-5 * time.Minute)}, now)
	if !got.Backpressured || got.Ceiling != claudeEndgameMaxSessions || got.Paced != 0 || got.Sessions != 3 {
		t.Fatalf("a throttle 5 min ago: %+v, want no raise and the taper of 3", got)
	}
}

func TestPacedAllowanceShortWindowBrakeHoldsOneWithNoMeasuredBurn(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	now := time.Now()
	score := drainFixture(now, 0.17, 90*time.Minute, 0.3, 4*time.Hour)
	if score.shortBurnSustainable() {
		t.Fatal("fixture drift: the 5h window is sustainable")
	}
	if got := pacedAllowance(score, Pace{}, now); got.Paced != 0 || got.Sessions != 1 {
		t.Fatalf("unsustainable 5h window, unmeasured: %+v, want one session", got)
	}
}

func TestPacedSessionCeilingIsASetting(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	now := time.Now()
	score := pacedFixture(now, 0.17, 90*time.Minute)
	setPacedSessionCeilingForTest(t, 5)
	if got := pacedAllowance(score, Pace{}, now); got.Ceiling != 5 || got.Paced != 5 || got.Sessions != 5 {
		t.Fatalf("ceiling 5: %+v, want needed 7.56 held at 5", got)
	}
	if v := pacedVerdict(score, Pace{}, 4, now); !v.Eligible || v.AllowedSessions != 5 {
		t.Fatalf("ceiling 5, 4 live: %+v, want a fifth taken", v)
	}
	if v := pacedVerdict(score, Pace{}, 5, now); v.Eligible || !v.AtCap {
		t.Fatalf("ceiling 5, 5 live: %+v, want at cap", v)
	}
	if got := pacedAllowance(score, Pace{Backpressure: now.Add(-time.Minute)}, now); got.Ceiling != claudeEndgameMaxSessions || got.Paced != 0 || got.Sessions != 3 {
		t.Fatalf("ceiling 5 under a throttle: %+v, want the ceiling clamped to 4 and the taper of 3", got)
	}
	SetPacedSessionCeiling(2)
	if got := pacedAllowance(score, Pace{}, now); got.Ceiling != 2 || got.Paced != 0 || got.Sessions != 3 {
		t.Fatalf("ceiling 2: %+v, want no raise and the taper of 3", got)
	}
	if got := pacedAllowance(score, Pace{Backpressure: now.Add(-time.Minute)}, now); got.Ceiling != 2 {
		t.Fatalf("ceiling 2 under a throttle: %+v, want the ceiling left at 2", got)
	}
	SetPacedSessionCeiling(0)
	if got := pacedAllowance(score, Pace{}, now); got.Ceiling != DefaultPacedSessionCeiling || got.Sessions != 8 {
		t.Fatalf("ceiling 0: %+v, want the default ceiling, which needed 7.56 does not reach, so 8", got)
	}
}

func TestTheDefaultBurnSizesOnlyALocalClaudeAccount(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	now := time.Now()
	codex := pacedFixture(now, 0.17, 90*time.Minute)
	codex.Provider = account.ProviderCodex
	peer := pacedFixture(now, 0.17, 90*time.Minute)
	peer.Remote = true
	for name, score := range map[string]Score{"codex": codex, "a verdict-less peer": peer} {
		got := NewScheduler([]Score{score}).PaceAllowance(score.Provider, score.AccountID, now)
		if got.Paced != 0 || got.Sessions != 3 || got.Pace.Source != "" {
			t.Fatalf("%s, 17%% left 1.5h from the roll: %+v, want the taper of 3 and no default burn", name, got)
		}
	}
}

func TestTheDefaultBurnSizesOnlyAFreshReading(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	now := time.Now()
	stale := pacedFixture(now, 0.17, 90*time.Minute)
	stale.Fresh = false
	if got := pacedAllowance(stale, Pace{}, now); got.Paced != 0 || got.Sessions != 3 || got.Pace.Source != "" || got.Needed != 0 {
		t.Fatalf("a restored or failing reading, 17%% left 1.5h from the roll, unmeasured: %+v, want the taper of 3 and no default burn", got)
	}
	if got := pacedAllowance(stale, measuredPace(0.015), now); got.Paced != 8 || got.Sessions != 8 {
		t.Fatalf("the same reading with a measured burn: %+v, want the measured raise to 8", got)
	}
}

func TestTheSwitchOffReportsNoNeedAndNoDefaultBurn(t *testing.T) {
	setPacedAllowanceForTest(t, false)
	now := time.Now()
	score := pacedFixture(now, 0.17, 90*time.Minute)
	if got := pacedAllowance(score, Pace{}, now); got.Sessions != 3 || got.Needed != 0 || got.Pace.Source != "" || got.Pace.BurnPerLive != 0 {
		t.Fatalf("switch off, unmeasured: %+v, want the taper of 3 with no need and no default burn", got)
	}
	if got := pacedAllowance(score, measuredPace(0.015), now); got.Sessions != 3 || got.Needed != 0 || got.Pace.Source != "account" {
		t.Fatalf("switch off, measured: %+v, want the taper of 3, no need, and the measurement itself", got)
	}
}

func TestTheDefaultBurnDoesNotRaiseAnAccountWithNoFiveHourReading(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	now := time.Now()
	score := pacedFixture(now, 0.17, 90*time.Minute)
	score.ShortWindowSeconds, score.ShortResetAfterSeconds = 0, 0
	if !score.shortBurnSustainable() {
		t.Fatal("fixture drift: with no 5h timing the 5h brake should not act")
	}
	if got := pacedAllowance(score, Pace{}, now); got.Paced != 0 || got.Sessions != 3 || got.Pace.Source != "" {
		t.Fatalf("no 5h reading, 17%% left 1.5h from the roll, unmeasured: %+v, want the taper of 3 and no default burn", got)
	}
	if got := pacedAllowance(score, measuredPace(0.015), now); got.Paced != 8 || got.Sessions != 8 {
		t.Fatalf("no 5h reading with a measured burn: %+v, want the measured raise to 8", got)
	}
}
