package proxy

import (
	"math"
	"strings"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
	"github.com/discordwell/subrouter/session"
)

type refugeeStay struct {
	agent, session, origin, peer, account string
	placedAt, firstSeen                   time.Time
	firstRemainingPct, remainingPct       float64
	local                                 localRefugeeStay
	box                                   boxRefugeeStay
}

// localRefugeeStay is what a stay on a local account integrates: the live
// session-hours the pace tracker had when the stay was first seen.
type localRefugeeStay struct {
	startLiveHours float64
	haveStart      bool
}

// boxRefugeeStay is what a stay on a box account samples: the live sessions
// the box publishes, summed over the sampler's ticks.
type boxRefugeeStay struct {
	liveSessionsSum int
	samples         int
}

type refugeeSighting struct {
	agent, session, peer, account string
	mark                          session.Refugee
	counting                      bool
}

func refugeeStayKey(peer, agent, sessionID string, placedAt time.Time) string {
	return peer + "\x00" + agent + "\x00" + sessionID + "\x00" + placedAt.UTC().Format(time.RFC3339Nano)
}

func (s Server) trackRefugeeStays(scheduler selectacct.Scheduler, now time.Time, state *paceSamplerState) {
	marked, counting := map[string]bool{}, map[string]bool{}
	for _, seen := range s.refugeeSightings(now) {
		key := refugeeStayKey(seen.peer, seen.agent, seen.session, seen.mark.PlacedAt)
		marked[key] = true
		if !seen.counting || state.dark[key] {
			continue
		}
		counting[key] = true
		reading := s.refugeeAccountReading(scheduler, seen.peer, seen.account, now)
		stay, tracked := state.stays[key]
		if !tracked {
			stay = &refugeeStay{agent: seen.agent, session: seen.session, origin: seen.mark.Origin, peer: seen.peer,
				account: seen.account, placedAt: seen.mark.PlacedAt, firstSeen: now, firstRemainingPct: reading.remainingPct}
			if seen.peer == "" {
				stay.local = localRefugeeStay{startLiveHours: reading.liveHours, haveStart: reading.haveLiveHours}
			}
			state.stays[key] = stay
		}
		stay.remainingPct = reading.remainingPct
		if seen.peer != "" && reading.haveLiveSessions {
			stay.box.liveSessionsSum += reading.liveSessions
			stay.box.samples++
		}
	}
	for key, stay := range state.stays {
		if counting[key] {
			continue
		}
		delete(state.stays, key)
		state.dark[key] = true
		reading := s.refugeeAccountReading(scheduler, stay.peer, stay.account, now)
		if reading.ok {
			stay.remainingPct = reading.remainingPct
		}
		avgLive := 0.0
		if stay.peer == "" {
			if span := now.Sub(stay.firstSeen).Hours(); span > 0 && stay.local.haveStart && reading.haveLiveHours {
				avgLive = (reading.liveHours - stay.local.startLiveHours) / span
			}
		} else if stay.box.samples > 0 {
			avgLive = float64(stay.box.liveSessionsSum) / float64(stay.box.samples)
		}
		drop := stay.firstRemainingPct - stay.remainingPct
		s.Logger.Info("refugee went dark",
			"agent", stay.agent, "session", stay.session, "origin", stay.origin, "peer", stay.peer, "account", stay.account,
			"placed_at", stay.placedAt.UTC().Format(time.RFC3339), "first_seen_remaining_pct", roundPace(stay.firstRemainingPct),
			"remaining_pct_now", roundPace(stay.remainingPct), "lifetime_minutes", roundPace(now.Sub(stay.placedAt).Minutes()),
			"avg_live_sessions", roundPace(avgLive), "est_session_burn_pct", roundPace(drop/math.Max(1, avgLive)))
	}
	for key := range state.dark {
		if !marked[key] {
			delete(state.dark, key)
		}
	}
}

func (s Server) refugeeSightings(now time.Time) []refugeeSighting {
	var out []refugeeSighting
	if s.Sessions != nil {
		for _, assignment := range s.Sessions.All() {
			if assignment.Refugee == nil {
				continue
			}
			out = append(out, refugeeSighting{agent: assignment.AgentType, session: assignment.SessionID,
				account: assignment.AccountID, mark: *assignment.Refugee, counting: s.countsAsLocalRefugee(assignment)})
		}
	}
	for _, pool := range s.peers() {
		pool.delegations.mu.Lock()
		for key, entry := range pool.delegations.entries {
			if entry.Refugee == nil {
				continue
			}
			agent, sessionID, _ := strings.Cut(key, ":")
			out = append(out, refugeeSighting{agent: agent, session: sessionID, peer: pool.name(),
				account: entry.Account, mark: *entry.Refugee, counting: !now.After(entry.Until)})
		}
		pool.delegations.mu.Unlock()
	}
	return out
}

// refugeeAccountReading is one account's reading for the dark line. A local
// account reads the live session-hours its pace tracker integrated; a box
// account reads the live sessions its owner publishes.
type refugeeAccountReading struct {
	remainingPct     float64
	ok               bool
	liveHours        float64
	haveLiveHours    bool
	liveSessions     int
	haveLiveSessions bool
}

func (s Server) refugeeAccountReading(scheduler selectacct.Scheduler, peer, account string, now time.Time) refugeeAccountReading {
	var out refugeeAccountReading
	if peer == "" {
		if _, ok := scheduler.LookupScore(accounts.ProviderClaude, account); ok {
			out.remainingPct = scheduler.MeasuredSpendable(accounts.ProviderClaude, account) * 100
			out.ok = true
		}
		out.liveHours, out.haveLiveHours = s.SchedulerRef.LiveSessionHours(accounts.ProviderClaude, account, now)
		return out
	}
	row, ok := s.cachedPeerRow(peer, account)
	if !ok {
		return out
	}
	if weekly := row.Normalized.WeeklyHeadroom; weekly != nil {
		out.remainingPct, out.ok = *weekly*100, true
	}
	if verdict := row.Normalized.NewSession; verdict != nil {
		out.liveSessions, out.haveLiveSessions = verdict.LiveSessions, true
	}
	return out
}
