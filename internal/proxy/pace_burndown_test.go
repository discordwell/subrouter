package proxy

import (
	"strings"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
)

// PATCHES §110 on the pool board: an account in early burndown on its pace
// reads drain with a note naming the cause, for a local row (this daemon's
// pace tracker measured it) and for a box row (the box published the cause;
// a scheduler rebuilt from windows cannot measure the box's burn).
func TestPoolBoardShowsThePaceBurndown(t *testing.T) {
	now := time.Now()
	const note = "early burndown: at its present burn its week would roll with budget unspent"
	windows := []accounts.UsageWindow{shortWindow(10, 9000), claudeWeekly(0, 51*3600)}
	status := AccountUsageStatus{
		AccountStatus: AccountStatus{ID: "idle", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, AuthValid: true},
		Windows:       windows,
	}
	reading := func(at time.Time) selectacct.Scheduler {
		score := scoreFromUsageWindows(accounts.ProviderClaude, "idle", windows)
		score.Fresh, score.MeasuredAt = true, at
		return selectacct.NewScheduler([]selectacct.Score{score})
	}
	read := func(scheduler selectacct.Scheduler, status AccountUsageStatus) poolRow {
		row := poolRow{Provider: "claude", ID: "idle", Pools: []string{poolNameLocal}}
		applyPoolReadings(&row, status, scheduler, true, now)
		return row
	}

	// Calibration: one reading, so its own burn is unmeasured.
	if row := read(reading(now), status); row.State != poolStateOK {
		t.Fatalf("unmeasured: state %q note %q, want %q", row.State, row.StateNote, poolStateOK)
	}

	t.Run("local", func(t *testing.T) {
		ref := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
		ref.Set(reading(now.Add(-time.Hour)))
		ref.Set(reading(now))
		if row := read(ref.Get(), status); row.State != poolStateDrain || !strings.Contains(row.StateNote, note) {
			t.Fatalf("state %q note %q, want %q naming %q", row.State, row.StateNote, poolStateDrain, note)
		}
	})

	t.Run("box", func(t *testing.T) {
		published := status
		published.Normalized = &NormalizedUsage{Headroom: 0.9, ShortHeadroom: 0.9, Fresh: true,
			NewSession: &selectacct.NewSessionVerdict{Eligible: true, Reason: selectacct.VerdictBurndown, Burndown: true,
				AllowedSessions: 4, BurndownCause: selectacct.BurndownCausePace}}
		if row := read(reading(now), published); row.State != poolStateDrain || !strings.Contains(row.StateNote, note) {
			t.Fatalf("state %q note %q, want %q naming %q", row.State, row.StateNote, poolStateDrain, note)
		}
	})
}

// The decision line's local_endgame is the burndown window as selection read
// it, so a local win in early burndown on its pace does not log as a steady
// pick.
func TestRoutingCapacityEndgameCoversThePaceBurndown(t *testing.T) {
	now := time.Now()
	account := accounts.Account{ID: "idle", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}
	windows := []accounts.UsageWindow{shortWindow(10, 9000), claudeWeekly(0, 51*3600)}
	reading := func(at time.Time) selectacct.Scheduler {
		score := scoreFromUsageWindows(accounts.ProviderClaude, "idle", windows)
		score.Fresh, score.MeasuredAt = true, at
		return selectacct.NewScheduler([]selectacct.Score{score})
	}
	if unmeasured := reading(now); (Server{}).routingCapacityFor(unmeasured, unmeasured, account).Endgame {
		t.Fatal("calibration: an unmeasured account 51 hours from its roll logged local_endgame")
	}
	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	ref.Set(reading(now.Add(-time.Hour)))
	ref.Set(reading(now))
	if measured := ref.Get(); !(Server{}).routingCapacityFor(measured, measured, account).Endgame {
		t.Fatal("an idle account in early burndown on its pace logged local_endgame=false")
	}
}
