package proxy

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func publishReading(ref *selectacct.SchedulerRef, score selectacct.Score) {
	ref.Set(ref.RefreshSeed().WithScore(score))
}

func measurePace(ref *selectacct.SchedulerRef, score selectacct.Score, live int, burnPerLive float64, now time.Time) {
	key := selectacct.ScoreKey(accounts.ProviderClaude, score.AccountID)
	t0 := now.Add(-time.Hour)
	first := score
	first.MeasuredAt = t0
	first.WeeklyRemaining += burnPerLive * float64(live)
	first.WeeklyHeadroom = first.WeeklyRemaining
	ref.ObserveLiveSessions(map[string]int{key: live}, t0)
	publishReading(ref, first)
	ref.ObserveLiveSessions(map[string]int{key: live}, now)
	last := score
	last.MeasuredAt = now
	publishReading(ref, last)
}

func roomyBehindPace(now time.Time) selectacct.Score {
	return selectacct.Score{
		AccountID: "roomy", Provider: accounts.ProviderClaude, Fresh: true, Headroom: 0.8, ShortHeadroom: 0.9,
		ShortWindowSeconds: 18000, ShortResetAfterSeconds: 3600,
		WeeklyHeadroom: 0.8, WeeklyRemaining: 0.8, WeeklyResetAt: now.Add(5 * time.Hour),
	}
}

func (b *targetBox) roomyVerdict() selectacct.NewSessionVerdict {
	return b.server.selectionScheduler("", "").NewSessionVerdict(accounts.ProviderClaude, "roomy", time.Now())
}

// roomyRaised is roomyBehindPace's raise at 0.015 a live session-hour:
// ceil(0.80 / 5 h / 0.015) = ceil(10.67).
const roomyRaised = 11

func raisedTargetBox(t *testing.T) *targetBox {
	t.Helper()
	box := newTargetBox(t)
	now := time.Now()
	measurePace(box.server.SchedulerRef, roomyBehindPace(now), 2, 0.015, now)
	if v, want := box.roomyVerdict(), min(roomyRaised, selectacct.PacedSessionCeiling()); v.AllowedSessions != want {
		t.Fatalf("calibration: roomy verdict %+v, want raised to %d", v, want)
	}
	return box
}

func TestThePacedSessionCeilingSettingReachesTheVerdictAndTheLog(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	prev := selectacct.PacedSessionCeiling()
	selectacct.SetPacedSessionCeiling(10)
	t.Cleanup(func() { selectacct.SetPacedSessionCeiling(prev) })
	box := raisedTargetBox(t)
	box.server.samplePace(time.Now(), newPaceSamplerState())
	got := pacedLines(box.logs.String(), "roomy")
	if len(got) != 1 || !strings.Contains(got[0], "allowed_sessions=10") || !strings.Contains(got[0], "ceiling=10") {
		t.Fatalf("want one line for roomy raised to the ceiling of 10; got:\n%s", strings.Join(got, "\n"))
	}
}

func TestLoweringThePacedAllowanceMovesNoWarmSession(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	box := raisedTargetBox(t)
	var warm []string
	ends := map[string]func(){}
	for i := range roomyRaised {
		id := fmt.Sprintf("warm-%d", i)
		warm = append(warm, id)
		box.pin(t, id, "roomy")
		ends[id] = box.active.Begin("claude", id)
	}
	t.Cleanup(func() {
		for _, end := range ends {
			end()
		}
	})
	if v := box.roomyVerdict(); v.LiveSessions != 11 || !v.AtCap || v.AllowedSessions != 11 {
		t.Fatalf("calibration: roomy %+v, want 11 live at its raised cap of 11", v)
	}
	box.server.SchedulerRef.NotePaceBackpressure(accounts.ProviderClaude, "roomy", time.Now())
	if v := box.roomyVerdict(); v.AllowedSessions != 4 || !v.AtCap || v.Evicted {
		t.Fatalf("after backpressure roomy %+v, want lowered to 4, at cap, not evicted", v)
	}
	for _, id := range warm {
		servedOn(t, box.turn(t, id, "roomy", nil), "roomy")
		servedOn(t, box.turn(t, id, "", nil), "roomy")
		if got := box.pinnedTo(t, id); got != "roomy" {
			t.Fatalf("%s pinned to %q after its turns, want roomy", id, got)
		}
	}
	logs := box.logs.String()
	if strings.Contains(logs, "session moved") || strings.Contains(logs, "targeted placement refused") {
		t.Fatalf("a warm session was moved or refused under the lowered allowance; logs:\n%s", logs)
	}
	ends[warm[0]]()
	ends[warm[1]]()
	delete(ends, warm[0])
	delete(ends, warm[1])
	readRefusal(t, box.turn(t, "new-session", "roomy", nil), selectacct.VerdictAtCap)
}

func TestAThrottleWithoutQuotaRejectionLowersThePacedAllowance(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	for _, tc := range []struct {
		name     string
		status   string
		lowers   bool
		headline string
	}{
		{"throttle", "allowed", true, "a non-quota 429"},
		{"quota", "rejected", false, "a quota rejection"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := raisedTargetBox(t)
			box.server.admission = newAdmissionControl()
			box.upstream.respond = func(account string, w http.ResponseWriter) {
				w.Header().Set("anthropic-ratelimit-unified-status", tc.status)
				w.WriteHeader(http.StatusTooManyRequests)
			}
			box.turn(t, "throttled", "roomy", nil)
			if hits := box.upstream.accounts(); len(hits) == 0 || hits[0] != "roomy" {
				t.Fatalf("calibration: upstream hits %v, want roomy first", hits)
			}
			_, recorded := box.server.admission.backpressureTimes()[selectacct.ScoreKey(accounts.ProviderClaude, "roomy")]
			if recorded != tc.lowers {
				t.Fatalf("%s recorded as backpressure: %v, want %v", tc.headline, recorded, tc.lowers)
			}
			if !tc.lowers {
				return
			}
			box.server.samplePace(time.Now(), newPaceSamplerState())
			if v := box.roomyVerdict(); v.AllowedSessions != 4 {
				t.Fatalf("after %s and a sampler tick roomy %+v, want back at the taper of 4", tc.headline, v)
			}
		})
	}
}

func TestAnAdmissionWaitDoesNotLowerThePacedAllowance(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	box := raisedTargetBox(t)
	gate := newAdmissionControl()
	box.server.admission = gate
	key := admissionKeyFor(accounts.ProviderClaude, "roomy", "claude-opus-4-8")
	for range gate.limits.Limit {
		lease, err := gate.acquire(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.release()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := gate.acquire(ctx, key); err == nil {
		t.Fatal("calibration: a request past the limit was admitted")
	}
	if got := gate.backpressureTimes(); len(got) != 0 {
		t.Fatalf("a queued request recorded backpressure: %v", got)
	}
	box.server.samplePace(time.Now(), newPaceSamplerState())
	if v := box.roomyVerdict(); v.AllowedSessions != 11 {
		t.Fatalf("after a queued request and a sampler tick roomy %+v, want still raised to 11", v)
	}
}

func TestThePacedAllowanceIsLoggedWhenItChanges(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	box := raisedTargetBox(t)
	logged := newPaceSamplerState()
	lines := func() []string {
		var out []string
		for _, line := range strings.Split(box.logs.String(), "\n") {
			if strings.Contains(line, "paced allowance") && strings.Contains(line, "account=roomy") {
				out = append(out, line)
			}
		}
		return out
	}
	box.server.samplePace(time.Now(), logged)
	got := lines()
	if len(got) != 1 {
		t.Fatalf("want one paced allowance line for roomy, got %d; logs:\n%s", len(got), box.logs.String())
	}
	for _, field := range []string{
		"in_burndown=true", "burndown_cause=roll", "allowed_sessions=11", "paced_sessions=11", "taper=4", "ceiling=20",
		"backpressured=false", "needed=10.6", "burn_per_live=0.015", "burn_source=account", "burn_now=0.03",
		"spendable=0.8", "hours_to_roll=", "projected_waste=", "danger=true", "live_sessions=0",
	} {
		if !strings.Contains(got[0], field) {
			t.Fatalf("paced allowance line lacks %q: %s", field, got[0])
		}
	}
	box.server.samplePace(time.Now(), logged)
	if n := len(lines()); n != 1 {
		t.Fatalf("an unchanged allowance was logged again: %d lines; logs:\n%s", n, box.logs.String())
	}
	box.server.SchedulerRef.NotePaceBackpressure(accounts.ProviderClaude, "roomy", time.Now())
	box.server.samplePace(time.Now(), logged)
	got = lines()
	if len(got) != 2 || !strings.Contains(got[1], "allowed_sessions=4") || !strings.Contains(got[1], "paced_sessions=0") ||
		!strings.Contains(got[1], "ceiling=4") || !strings.Contains(got[1], "backpressured=true") {
		t.Fatalf("want a second line at the lowered allowance; got:\n%s", strings.Join(got, "\n"))
	}
}

func TestAnUnmeasuredBurndownAccountIsLoggedOnTheDefaultBurn(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	box := newTargetBox(t)
	publishReading(box.server.SchedulerRef, roomyBehindPace(time.Now()))
	box.server.samplePace(time.Now(), newPaceSamplerState())
	got := pacedLines(box.logs.String(), "roomy")
	if len(got) != 1 {
		t.Fatalf("want one paced allowance line for roomy, got %d; logs:\n%s", len(got), box.logs.String())
	}
	for _, field := range []string{
		"in_burndown=true", "allowed_sessions=11", "paced_sessions=11", "taper=4", "ceiling=20", "needed=10.6",
		"burn_per_live=0.015", "burn_source=default", "burn_now=0", "projected_waste=0", "danger=false",
	} {
		if !strings.Contains(got[0], field) {
			t.Fatalf("paced allowance line lacks %q: %s", field, got[0])
		}
	}
}

func TestThePaceSamplerAndReadingsRaiseABurndownAccount(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	box, gate := gatedTargetBox(t)
	for _, id := range []string{"live-1", "live-2"} {
		box.pin(t, id, "roomy")
		t.Cleanup(box.active.Begin("claude", id))
		t.Cleanup(admitTurn(t, gate, id, "roomy"))
	}
	now := time.Now()
	t0 := now.Add(-time.Hour)
	logged := newPaceSamplerState()
	slow := func(at time.Time, remaining float64) selectacct.Score {
		score := roomyBehindPace(now)
		score.MeasuredAt, score.WeeklyRemaining, score.WeeklyHeadroom = at, remaining, remaining
		score.WeeklyResetAt = now.Add(18 * time.Hour)
		return score
	}
	box.server.samplePace(t0, logged)
	publishReading(box.server.SchedulerRef, slow(t0, 0.81))
	box.server.samplePace(t0.Add(30*time.Minute), logged)
	if v := box.roomyVerdict(); v.AllowedSessions != 4 {
		t.Fatalf("one reading in, roomy %+v, want the taper of 4 the default burn does not beat", v)
	}
	box.server.samplePace(now, logged)
	publishReading(box.server.SchedulerRef, slow(now, 0.80))
	v := box.roomyVerdict()
	if v.AllowedSessions != 9 || !v.Eligible || v.LiveSessions != 2 {
		t.Fatalf("after an hour at two live sessions roomy %+v, want 0.80 / 18 h / 0.005 = 8.9 raised to 9 and eligible", v)
	}
	pace := box.server.SchedulerRef.Get().PaceAllowance(accounts.ProviderClaude, "roomy", now).Pace
	if pace.Source != "account" || pace.BurnPerLive < 0.0049 || pace.BurnPerLive > 0.0051 {
		t.Fatalf("pace %+v, want 0.005 per live session-hour from roomy's own readings", pace)
	}
}

func admitTurn(t *testing.T, gate *admissionControl, sessionID, account string) func() {
	t.Helper()
	lease, err := gate.acquireFor(context.Background(), opusKey(account), admissionSessionKey{agent: "claude", session: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	return lease.release
}

func setPacedAllowanceForTest(t *testing.T, on bool) {
	t.Helper()
	prev := selectacct.PacedAllowance()
	selectacct.SetPacedAllowance(on)
	t.Cleanup(func() { selectacct.SetPacedAllowance(prev) })
}

func TestACeilingFlipAloneOutsideBurndownIsNotLogged(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	box := newTargetBox(t)
	state := newPaceSamplerState()
	box.server.samplePace(time.Now(), state)
	box.server.SchedulerRef.NotePaceBackpressure(accounts.ProviderClaude, "steady", time.Now())
	if allowance := box.server.SchedulerRef.Get().PaceAllowance(accounts.ProviderClaude, "steady", time.Now()); allowance.Ceiling != 4 || allowance.Paced != 0 {
		t.Fatalf("calibration: steady %+v, want its ceiling lowered to 4 and no raise", allowance)
	}
	box.server.samplePace(time.Now(), state)
	if lines := pacedLines(box.logs.String(), "steady"); len(lines) != 0 {
		t.Fatalf("steady, outside burndown with no raise, was logged for its ceiling alone:\n%s", strings.Join(lines, "\n"))
	}
}

func TestTheSwitchOffLogsNoNeedOrDefaultBurn(t *testing.T) {
	setPacedAllowanceForTest(t, false)
	box := newTargetBox(t)
	publishReading(box.server.SchedulerRef, roomyBehindPace(time.Now()))
	box.server.samplePace(time.Now(), newPaceSamplerState())
	got := pacedLines(box.logs.String(), "roomy")
	if len(got) != 1 {
		t.Fatalf("want one paced allowance line for roomy, got %d; logs:\n%s", len(got), box.logs.String())
	}
	for _, field := range []string{"allowed_sessions=4", "paced_sessions=0", "needed=0 ", "burn_per_live=0 ", `burn_source=""`} {
		if !strings.Contains(got[0], field) {
			t.Fatalf("switch off: paced allowance line lacks %q: %s", field, got[0])
		}
	}
}
