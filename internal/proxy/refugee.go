package proxy

import (
	"net/http"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/session"
)

// Refugee placement (PATCHES §67; owner decisions D1 to D4, 2026-11-19). A
// refugee is a chat held on a box that was found down: its prompt cache is on
// an account this machine cannot reach. It goes to the other box first and to
// a local account last (D3), to an account holding fewer than refugeeSlots
// warm refugees (D1), and it never goes back (D4). When every account is
// full it keeps its hold, its turn waits, and each held attempt tries again
// (D2). New and cold sessions are not refugees and do not count.

// refugeeSlots is how many warm refugees one account takes (owner, D1: "any
// given home chat can only take 4 refugees").
const refugeeSlots = 4

const (
	refugeePlacedMessage    = "refugee placed"
	refugeeNotPlacedMessage = "refugee not placed: every account holds its refugees; the chat keeps its hold and waits"
)

// placeRefugee places a held turn of a chat whose box (origin) was found down,
// and reports whether it answered the client. Placed on another box, the turn
// is served there and the chat is held there, marked. Placed on a local
// account, the chat is pinned there, marked, its hold ends, and it reports
// false so the handler serves the turn on that pin, warm. With no slot
// anywhere the turn is held as a stranded chat's, on its home hold.
func (s Server) placeRefugee(w http.ResponseWriter, r *http.Request, provider accounts.Provider, poolModel, agentType, sessionID string, origin *RemotePool, home remotePoolDelegation, turn *remotePoolTurn) bool {
	key := session.ScopedSessionKey(agentType, sessionID)
	mark := session.Refugee{Origin: origin.name() + "/" + home.Account, PlacedAt: time.Now().UTC()}
	turn.mu.Lock()
	turn.downPeers[origin.name()] = true
	turn.mu.Unlock()
	if body, ok := replayableForwardBody(r); ok && s.peerForwardingAllowed(r) {
		ranking := s.newCrossPoolRanking(r, provider, poolModel, turn)
		// A refugee is counted on the account it goes to, so it goes only
		// where the forward can name one.
		ranking.targetedPeersOnly()
		placement, err := ranking.rankPeersOnly()
		for ; err == nil && placement.Peer != nil && !turn.boundReached(); placement, err = ranking.rankPeersOnly() {
			candidate := *placement.Peer
			claim, held := candidate.Peer.delegations.claimRefugee(key, time.Now(), remotePoolDelegationTTLFor(agentType), candidate.Peer.name(), candidate.Account, mark, refugeeSlots)
			if claim == 0 {
				turn.mu.Lock()
				turn.failed[remotePoolTargetKey{candidate.Peer.name(), candidate.Account}] = "refugee_slots_full"
				turn.mu.Unlock()
				continue
			}
			out := s.attemptCandidate(w, r, agentType, sessionID, body, remotePoolHoldSet, candidate, ranking)
			if out.Served {
				s.logRefugeePlaced(agentType, sessionID, mark, candidate.Peer.name(), candidate.Account, held)
				return true
			}
			candidate.Peer.delegations.release(key, claim)
			if out.clientGone() {
				origin.delegations.restore(key, home)
				return true
			}
		}
	}
	if s.Sessions != nil {
		for _, account := range s.refugeeLocalCandidates(r, provider, poolModel) {
			held, placed, err := s.Sessions.PutRefugee(agentType, sessionID, account.ID, mark, refugeeSlots, s.countsAsLocalRefugee)
			if err != nil {
				break
			}
			if !placed {
				continue
			}
			s.clearHoldElsewhere(key, nil)
			s.Hold.placedHere(key)
			turn.mu.Lock()
			turn.stayLocal = true
			turn.mu.Unlock()
			s.logRefugeePlaced(agentType, sessionID, mark, "", account.ID, held)
			return false
		}
	}
	origin.delegations.restore(key, home)
	if s.Logger != nil {
		s.Logger.Warn(refugeeNotPlacedMessage,
			"agent", agentType, "session", sessionID, "origin", mark.Origin, "refugee_slots", refugeeSlots)
	}
	holdStranded(w, r, agentType, sessionID, home.Account, "refugee_overflow",
		"peer "+origin.name()+" is down and every account holds its refugees")
	return true
}

// countsAsLocalRefugee is a local refugee that still takes its account's slot:
// warm by the 70-minute rule, and not held on a peer since (its local pin is
// then a placeholder).
func (s Server) countsAsLocalRefugee(assignment session.Assignment) bool {
	if !s.sessionWarm(assignment.AgentType, assignment.SessionID) {
		return false
	}
	_, _, held := s.peerHold(session.ScopedSessionKey(assignment.AgentType, assignment.SessionID))
	return !held
}

// refugeeLocalCandidates is the local accounts a refugee may be placed on, in
// the order selection ranks them for a new session.
func (s Server) refugeeLocalCandidates(r *http.Request, provider accounts.Provider, poolModel string) []accounts.Account {
	scheduler := s.selectionSchedulerFrom(s.scheduler(), poolModel, "")
	refused := func(account accounts.Account) bool { return selectionRefusesAccount(provider, scheduler, account) }
	placeable := placeableAccounts(filterAccountsForProvider(s.accountListContext(r.Context()), provider), refused)
	ranked, err := scheduler.Explain(SchedulerAccounts(placeable))
	if err != nil {
		return nil
	}
	var out []accounts.Account
	for _, entry := range ranked {
		account, err := matchPickedAccount(placeable, entry.Account)
		if err == nil && !refused(account) {
			out = append(out, account)
		}
	}
	return out
}

// logRefugeePlaced is the line for each placement, with the account's count
// of warm refugees, this one included (owner: expect slots to stay taken up
// to 70 minutes after a blip).
func (s Server) logRefugeePlaced(agentType, sessionID string, mark session.Refugee, peer, account string, held int) {
	if s.Logger == nil {
		return
	}
	s.Logger.Warn(refugeePlacedMessage,
		"agent", agentType, "session", sessionID, "origin", mark.Origin,
		"to_peer", peer, "to_account", account, "refugees", held, "refugee_slots", refugeeSlots)
}
