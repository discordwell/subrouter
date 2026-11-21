package proxy

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func dropCachedUsage(pool *RemotePool) {
	pool.admin.mu.Lock()
	delete(pool.admin.entries, remotePoolUsagePath)
	pool.admin.mu.Unlock()
}

func TestALocalAccountTakesTheRefugeesItsWeekAllows(t *testing.T) {
	for _, tc := range []struct {
		week   float64 // left of it, 100 hours before its roll
		holds  int
		placed bool
	}{{0.3, 2, true}, {0.3, 3, false}, {0.05, 0, false}} {
		t.Run(fmt.Sprintf("%d held with %g of its week left", tc.holds, tc.week), func(t *testing.T) {
			policy := strandedPolicy(nil)
			rig := newWarmPeerRig(t, policy, false, func(int64) error { return refusedFailure() })
			rig.sched.Set(selectacct.NewScheduler([]selectacct.Score{{
				AccountID: "one@example.com", Provider: accounts.ProviderClaude, Fresh: true,
				Headroom: tc.week, ShortHeadroom: 0.95, WeeklyHeadroom: tc.week, WeeklyRemaining: tc.week, WeeklyResetAt: time.Now().Add(100 * time.Hour),
			}}))
			refugeeHoldsOn(rig.boxB.pool, "acct-b", refugeeSlots, time.Minute)
			localRefugeesOn(t, rig.holdRig, "one@example.com", tc.holds)
			findDown(t, rig.boxA.pool)
			answer := rig.sendAsync(t, holdTurnBody)
			if !tc.placed {
				awaitHeld(t, rig.holdRig, policy, answer, "a refugee with every slot taken")
				tries := attempts{count: func() int64 { return int64(rig.countLog("refugee not placed")) }, kick: func() {}}
				heldFor(t, rig.holdRig, answer, tries, 1, "with one@example.com at its cap")
				if _, ok := rig.store.Get("claude", "hold-session"); ok {
					t.Fatalf("hold-session was pinned locally past the cap; logs:\n%s", rig.logs.String())
				}
				return
			}
			got := notHeld(t, rig.holdRig, policy, answer, "a refugee")
			rig.settle(t, 1)
			assignment, ok := rig.store.Get("claude", "hold-session")
			if got.status != http.StatusOK || !ok || assignment.AccountID != "one@example.com" || assignment.Refugee == nil {
				t.Fatalf("status %d, assignment %+v (%v), want a refugee on one@example.com; logs:\n%s", got.status, assignment, ok, rig.logs.String())
			}
			if rig.countLog("to_account=one@example.com refugees=3 refugee_slots=3") != 1 {
				t.Fatalf("want the third refugee placed against a cap of 3; logs:\n%s", rig.logs.String())
			}
		})
	}
}

func TestABoxAccountTakesTheRefugeesItsOwnerCaps(t *testing.T) {
	for _, tc := range []struct {
		holds   int
		account string
	}{{1, "acct-b"}, {2, "one@example.com"}} {
		t.Run(fmt.Sprintf("%d held", tc.holds), func(t *testing.T) {
			policy := strandedPolicy(nil)
			rig := newWarmPeerRig(t, policy, false, func(int64) error { return refusedFailure() })
			rig.boxB.mu.Lock()
			score := &rig.boxB.accounts[0].Score
			score.Headroom, score.WeeklyHeadroom, score.WeeklyRemaining = 0.25, 0.25, 0.25
			rig.boxB.mu.Unlock()
			dropCachedUsage(rig.boxB.pool)
			if got := rig.boxB.scheduler().NewSessionVerdict(accounts.ProviderClaude, "acct-b", time.Now()).RefugeeCap; got.Slots != 2 {
				t.Fatalf("calibration: box B publishes cap %+v, want 2", got)
			}
			refugeeHoldsOn(rig.boxB.pool, "acct-b", tc.holds, time.Minute)
			findDown(t, rig.boxA.pool)
			got := notHeld(t, rig.holdRig, policy, rig.sendAsync(t, holdTurnBody), "a refugee")
			rig.settle(t, 1)
			if want := fmt.Sprintf("to_account=%s ", tc.account); got.status != http.StatusOK || rig.countLog(want) != 1 {
				t.Fatalf("status %d body %q, want it placed on %s; logs:\n%s", got.status, got.body, tc.account, rig.logs.String())
			}
		})
	}
}

func TestUsageStatusPublishesTheRefugeeCap(t *testing.T) {
	now := time.Now()
	fable := `"seven_day_oauth_apps":{"utilization":80,"resets_at":"` + now.Add(100*time.Hour).UTC().Format(time.RFC3339) + `"}`
	server, _, _ := claudeUsageBox(t, map[string]string{
		"acct": claudeUsageJSON(now, 10, 4*time.Hour, 55, 100*time.Hour, fable),
	}, nil)
	row := readUsageStatus(t, server)["acct"]
	if row.Normalized == nil || row.Normalized.NewSession == nil {
		t.Fatalf("no verdict: %+v", row.Normalized)
	}
	if got := row.Normalized.NewSession.RefugeeCap; !got.Published || got.Slots != 4 {
		t.Fatalf("account refugee cap %+v, want 4 for 45%% of the week", got)
	}
	pool, ok := row.Normalized.Pools["claudefable"]
	if !ok || pool.NewSession == nil {
		t.Fatalf("no claudefable verdict: %+v", row.Normalized.Pools)
	}
	if got := pool.NewSession.RefugeeCap; !got.Published || got.Slots != 2 {
		t.Fatalf("Fable pool refugee cap %+v, want 2 for its 20%%", got)
	}
}

func TestTheLaptopReadsABoxsPublishedRefugeeCap(t *testing.T) {
	pool := NewRemotePool("http://box.invalid", "", "")
	server := Server{RemotePeers: []*RemotePool{pool}}
	for _, tc := range []struct {
		name, verdict, poolModel string
		want                     int
	}{
		{"published 0", `{"eligible":false,"reason":"eligible","refugee_cap":0}`, "", 0},
		{"published 7", `{"eligible":true,"reason":"eligible","refugee_cap":7}`, "", 7},
		{"absent", `{"eligible":true,"reason":"eligible"}`, "", refugeeSlots},
		{"the Fable pool's own", `{"eligible":true,"reason":"eligible","refugee_cap":7}`, "claude-fable", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `[{"id":"acct","provider":"claude","auth_mode":"oauth","normalized":{"headroom":1,"short_headroom":1,"new_session":` + tc.verdict +
				`,"pools":{"claudefable":{"headroom":1,"short_headroom":1,"new_session":{"eligible":true,"reason":"eligible","refugee_cap":1}}}}}]`
			pool.admin.store(remotePoolUsagePath, []byte(body), nil)
			if got := server.refugeeSlotsFor(pool.name(), "acct", tc.poolModel); got != tc.want {
				t.Fatalf("cap %d, want %d", got, tc.want)
			}
		})
	}
	if got := server.refugeeSlotsFor(pool.name(), "unknown", ""); got != refugeeSlots {
		t.Fatalf("an account missing from the body reads cap %d, want the legacy %d", got, refugeeSlots)
	}
}

func darkLines(logs string) []string {
	var out []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "refugee went dark") {
			out = append(out, line)
		}
	}
	return out
}

func TestALocalRefugeeIsLoggedOnceWhenItGoesDark(t *testing.T) {
	box, gate := gatedTargetBox(t)
	now := time.Now()
	t0 := now.Add(-time.Hour)
	if _, placed, err := box.store.PutRefugee("claude", "refugee-1", "roomy", session.Refugee{Origin: "box-a/acct-a", PlacedAt: t0},
		refugeeSlots, func(session.Assignment) bool { return true }); err != nil || !placed {
		t.Fatalf("calibration: refugee placed=%v err=%v", placed, err)
	}
	end := box.active.Begin("claude", "refugee-1")
	release := admitTurn(t, gate, "refugee-1", "roomy")
	state := newPaceSamplerState()
	box.server.samplePace(t0, state)
	box.server.samplePace(t0.Add(30*time.Minute), state)
	if lines := darkLines(box.logs.String()); len(lines) != 0 {
		t.Fatalf("a counting refugee was logged dark: %v", lines)
	}
	roomy := targetFixtureScores(now)[0]
	roomy.MeasuredAt, roomy.Headroom, roomy.WeeklyHeadroom, roomy.WeeklyRemaining = now, 0.78, 0.78, 0.78
	publishReading(box.server.SchedulerRef, roomy)
	end()
	release()
	if _, err := box.store.Delete("claude", "refugee-1"); err != nil {
		t.Fatal(err)
	}
	box.server.samplePace(now, state)
	box.server.samplePace(now.Add(time.Minute), state)
	lines := darkLines(box.logs.String())
	if len(lines) != 1 {
		t.Fatalf("want one dark line, got %d:\n%s", len(lines), box.logs.String())
	}
	for _, field := range []string{
		"agent=claude", "session=refugee-1", "origin=box-a/acct-a", `peer=""`, "account=roomy",
		"first_seen_remaining_pct=80", "remaining_pct_now=78", "lifetime_minutes=60", "avg_live_sessions=1", "est_session_burn_pct=2",
	} {
		if !strings.Contains(lines[0], field) {
			t.Fatalf("dark line lacks %q: %s", field, lines[0])
		}
	}
}

func TestABoxRefugeeIsLoggedWhenItsHoldLapses(t *testing.T) {
	policy := strandedPolicy(nil)
	rig := newWarmPeerRig(t, policy, false, func(int64) error { return refusedFailure() })
	server := Server{Sessions: rig.store, SchedulerRef: rig.sched, RemotePeers: []*RemotePool{rig.boxA.pool, rig.boxB.pool},
		Logger: slog.New(slog.NewTextHandler(rig.logs, nil)), ActiveSessions: NewActiveSessions()}
	rig.boxB.pool.usageForDecision(time.Now().Add(5 * time.Second))
	refugeeHoldsOn(rig.boxB.pool, "acct-b", 1, time.Minute)
	state := newPaceSamplerState()
	now := time.Now()
	server.samplePace(now, state)
	if lines := darkLines(rig.logs.String()); len(lines) != 0 {
		t.Fatalf("a live hold was logged dark: %v", lines)
	}
	later := now.Add(remotePoolDelegationTTLFor("claude"))
	server.samplePace(later, state)
	server.samplePace(later.Add(time.Minute), state)
	lines := darkLines(rig.logs.String())
	if len(lines) != 1 || !strings.Contains(lines[0], "peer="+rig.boxB.pool.name()) || !strings.Contains(lines[0], "account=acct-b") ||
		!strings.Contains(lines[0], "first_seen_remaining_pct=45") || !strings.Contains(lines[0], "remaining_pct_now=45") ||
		!strings.Contains(lines[0], "avg_live_sessions=0") || !strings.Contains(lines[0], "est_session_burn_pct=0") {
		t.Fatalf("want one dark line for acct-b on box B; logs:\n%s", rig.logs.String())
	}
}

func TestTheDarkLineReadsTheMeasuredWeekOfAReservedAccount(t *testing.T) {
	box := newTargetBox(t)
	box.server.AccountReserves = map[string]float64{"held": 0.3, "roomy": 0.05}
	for range 18 {
		box.server.SchedulerRef.NoteRouted(accounts.ProviderClaude, "roomy")
	}
	now := time.Now()
	if _, placed, err := box.store.PutRefugee("claude", "refugee-1", "roomy", session.Refugee{Origin: "box-a/acct-a", PlacedAt: now},
		refugeeSlots, func(session.Assignment) bool { return true }); err != nil || !placed {
		t.Fatalf("calibration: refugee placed=%v err=%v", placed, err)
	}
	state := newPaceSamplerState()
	box.server.samplePace(now, state)
	if _, err := box.store.Delete("claude", "refugee-1"); err != nil {
		t.Fatal(err)
	}
	box.server.samplePace(now.Add(time.Minute), state)
	lines := darkLines(box.logs.String())
	if len(lines) != 1 || !strings.Contains(lines[0], "first_seen_remaining_pct=75 ") || !strings.Contains(lines[0], "remaining_pct_now=75 ") {
		t.Fatalf("want one dark line at the measured 75 points; logs:\n%s", box.logs.String())
	}
}
