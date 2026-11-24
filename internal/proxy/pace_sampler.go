package proxy

import (
	"context"
	"math"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
)

type paceSamplerState struct {
	logged map[string]pacedLogState
	stays  map[string]*refugeeStay
	dark   map[string]bool
}

func newPaceSamplerState() *paceSamplerState {
	return &paceSamplerState{logged: map[string]pacedLogState{}, stays: map[string]*refugeeStay{}, dark: map[string]bool{}}
}

type pacedLogState struct {
	allowed int
	paced   int
	ceiling int
	danger  bool
}

func quietPacedLogState() pacedLogState {
	return pacedLogState{ceiling: selectacct.PacedSessionCeiling()}
}

// RunPaceSampler feeds the scheduler's pace tracker (PATCHES §72) every
// interval until ctx ends: each local account's sessions with an admitted
// turn in flight, and its non-quota 429s. It logs each change in a
// local Claude account's paced allowance, and each refugee that goes dark
// with what it burned.
func (s Server) RunPaceSampler(ctx context.Context, every time.Duration) {
	if s.SchedulerRef == nil || every <= 0 {
		return
	}
	state := newPaceSamplerState()
	s.samplePace(time.Now(), state)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.samplePace(time.Now(), state)
		}
	}
}

func (s Server) samplePace(now time.Time, state *paceSamplerState) {
	ref := s.SchedulerRef
	if s.ActiveSessions == nil && s.AccountRef != nil {
		s.ActiveSessions = s.AccountRef.sharedActiveSessions()
	}
	if s.admission == nil && s.AccountRef != nil {
		s.admission = s.AccountRef.sharedAdmission()
	}
	if s.claudeCosts == nil && s.AccountRef != nil {
		s.claudeCosts = s.AccountRef.claudeCostStore()
	}
	ref.ObserveLiveSessions(s.admission.admittedSessionCounts(), now)
	list := s.accountList()
	backpressure := s.admission.backpressureTimes()
	for _, account := range list {
		provider := schedulerAccountProvider(account.Provider)
		if at, ok := backpressure[selectacct.ScoreKey(provider, account.ID)]; ok {
			ref.NotePaceBackpressure(provider, account.ID, at)
		}
	}
	if s.Logger == nil {
		return
	}
	scheduler := s.selectionSchedulerFrom(s.scheduler(), "", "")
	s.logPacedAllowances(scheduler, list, now, state)
	s.trackRefugeeStays(scheduler, now, state)
}

func (s Server) logPacedAllowances(scheduler selectacct.Scheduler, list []accounts.Account, now time.Time, state *paceSamplerState) {
	for _, account := range list {
		if account.Provider != accounts.ProviderClaude || account.AuthMode != accounts.AuthModeOAuth {
			continue
		}
		if _, ok := scheduler.LookupScore(accounts.ProviderClaude, account.ID); !ok {
			continue
		}
		verdict := scheduler.NewSessionVerdict(accounts.ProviderClaude, account.ID, now)
		allowance := scheduler.PaceAllowance(accounts.ProviderClaude, account.ID, now)
		inBurndown := verdict.BurndownCause != ""
		current := pacedLogState{paced: allowance.Paced, ceiling: allowance.Ceiling, danger: allowance.Danger}
		if inBurndown {
			current.allowed = allowance.Sessions
		} else if allowance.Paced == 0 {
			current.ceiling = quietPacedLogState().ceiling
		}
		previous, ok := state.logged[account.ID]
		if !ok {
			previous = quietPacedLogState()
		}
		if current == previous {
			continue
		}
		state.logged[account.ID] = current
		s.Logger.Info("paced allowance",
			"account", account.ID, "in_burndown", inBurndown, "burndown_cause", verdict.BurndownCause,
			"allowed_sessions", current.allowed, "paced_sessions", allowance.Paced, "taper", allowance.Taper,
			"ceiling", allowance.Ceiling, "backpressured", allowance.Backpressured,
			"needed", roundPace(allowance.Needed), "burn_per_live", roundPace(allowance.Pace.BurnPerLive),
			"burn_source", allowance.Pace.Source, "burn_now", roundPace(allowance.Pace.BurnNow),
			"spendable", roundPace(allowance.Spendable), "hours_to_roll", roundPace(allowance.HoursToRoll),
			"projected_waste", roundPace(allowance.ProjectedWaste), "danger", allowance.Danger,
			"live_sessions", verdict.LiveSessions)
	}
}

func roundPace(value float64) float64 {
	return math.Round(value*1e4) / 1e4
}
