package proxy

import (
	"cmp"
	"net/http"
	"slices"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// Refugee placement (PATCHES §67, D1 to D4). A refugee is a chat held on a
// box that was found down: its prompt cache is on an account this machine
// cannot reach. Refugees spread across every account on the other boxes, the
// one with the most free slots first, and reach a local account only when
// every other box is found down, full or not an option. A box that cannot be
// asked right now keeps the chat on its home hold. Each account takes
// refugeeSlotsFor warm refugees (D1), and the warm rule keeps a refugee where
// it is placed, so it does not return when its box does (D4). With no room it
// keeps its hold, its turn waits, and each held attempt tries again (D2). New
// and cold sessions are not refugees and do not count.

const refugeeSlots = selectacct.UnmeasuredRefugeeCap

// refugeeSlotsFor is how many warm refugees account takes: a box account, or
// a local one when peer is "" (the paced allowance of PATCHES §72). An
// account whose cap is at or below its warm refugees is full.
func (s Server) refugeeSlotsFor(peer, account, poolModel string) int {
	switch {
	case s.refugeeSlotsOverride != nil:
		return s.refugeeSlotsOverride(peer, account, poolModel)
	case peer == "":
		return s.localRefugeeCap(account, poolModel)
	default:
		return s.peerRefugeeCap(peer, account, poolModel)
	}
}

const (
	refugeePlacedMessage           = "refugee placed"
	refugeeNotPlacedMessage        = "refugee not placed; the chat keeps its hold and waits"
	waitingRefugeeNotPlacedMessage = "waiting refugee not placed; answered the give-up again"
)

// refugeeOutcome is how a placement attempt ended.
type refugeeOutcome int

const (
	refugeeNotPlaced refugeeOutcome = iota
	refugeeServed
	refugeeLocal
	refugeeClientGone
)

type refugeePlacement struct {
	outcome    refugeeOutcome
	unaskedBox string
	laptopFree int
}

// why is the not-placed line's reason: the first box that could not be
// asked, or every box full.
func (p refugeePlacement) why() string {
	if p.unaskedBox != "" {
		return "box_unreachable:" + p.unaskedBox
	}
	return "boxes_full"
}

type boxSlot struct {
	crossPoolCandidate
	rank, slots, free int
}

type localSlot struct {
	account     accounts.Account
	slots, free int
}

// placeRefugee places a held turn of a chat whose box (origin) was found down,
// and reports whether it answered the client. Placed on another box, the turn
// is served there and the chat is held there, marked. Placed on a local
// account, the chat is pinned there, marked, its hold ends, and it reports
// false so the handler serves the turn on that pin. Otherwise the turn is
// held as a stranded chat's, on its home hold. One turn of a chat places it
// at a time: another that comes meanwhile waits (waitForPlacement).
func (s Server) placeRefugee(w http.ResponseWriter, r *http.Request, provider accounts.Provider, poolModel, agentType, sessionID string, origin *RemotePool, home remotePoolDelegation, turn *remotePoolTurn) bool {
	key := session.ScopedSessionKey(agentType, sessionID)
	from := strandedHome{origin.name(), home.Account}
	if !s.Hold.beginPlacing(key, from) {
		waitForPlacement(w, r, agentType, sessionID, from)
		return true
	}
	defer s.Hold.endPlacing(key)
	mark := session.Refugee{Origin: origin.name() + "/" + home.Account, PlacedAt: time.Now().UTC()}
	placed := s.spreadRefugee(w, r, provider, poolModel, agentType, sessionID, origin, mark, turn, map[string]bool{})
	switch placed.outcome {
	case refugeeServed:
		return true
	case refugeeLocal:
		return false
	}
	origin.delegations.restore(key, home)
	if placed.outcome == refugeeClientGone {
		return true
	}
	if s.Logger != nil {
		s.Logger.Warn(refugeeNotPlacedMessage,
			"agent", agentType, "session", sessionID, "origin", mark.Origin,
			"why", placed.why(), "laptop_free_slots", placed.laptopFree)
	}
	holdStranded(w, r, waitsForItsCache, agentType, sessionID, origin.name(), home.Account, "refugee_overflow",
		"peer "+origin.name()+" is down and no account can take this chat yet")
	return true
}

// waitForPlacement holds a model turn of a chat another turn is placing as a
// refugee. Until that placement ends, the chat's home hold is cleared or its
// claim on an account is pending, and the turn would read its local pin as
// the chat's account. Its next attempt follows the chat where it went, or
// places it.
func waitForPlacement(w http.ResponseWriter, r *http.Request, agentType, sessionID string, from strandedHome) {
	holdStranded(w, r, waitsForItsCache, agentType, sessionID, from.peer, from.account, "refugee_placing",
		"peer "+from.peer+" is down and this chat is being placed on another account")
}

// resumeWaitingRefugee serves a model turn of a chat that gave up waiting for
// its box (session.Waiting), and reports whether it answered. The turn goes
// home when the home box's breaker lets a request through, found down or
// not, as a held turn's does, and its account serves it, which takes no
// slot. Otherwise it is placed by the spread, as a held refugee's turn is,
// and a home box not found down is one the laptop waits for. When the spread
// places nothing (every account full, or a box that cannot be asked yet) it
// gets the give-up answer at once, with no hold, and the chat stops again.
// It reports false, for the handler to go on, for any other turn, and for
// one placed on a local account, which the handler serves on that pin.
func (s Server) resumeWaitingRefugee(w http.ResponseWriter, r *http.Request, provider accounts.Provider, poolModel, agentType, sessionID string, mode remotePoolHoldMode, turn *remotePoolTurn) bool {
	if mode != remotePoolHoldSet || s.Sessions == nil {
		return false
	}
	waiting, ok := s.waitingHome(agentType, sessionID)
	if !ok {
		return false
	}
	var home *RemotePool
	for _, p := range s.peers() {
		if p.name() == waiting.Peer {
			home = p
			break
		}
	}
	unasked := map[string]bool{}
	if body, ok := replayableForwardBody(r); ok && home != nil && s.peerForwardingAllowed(r) && s.peerOpenTo(home, r) {
		if !home.breaker.allow(time.Now()) {
			unasked[home.name()] = true
		} else {
			out := s.serveOnePeerAttempt(w, r, home, agentType, sessionID, body, remotePoolHoldSet, waiting.Account)
			if out.Served {
				s.endWait(agentType, sessionID)
				return true
			}
			if out.clientGone() {
				return true
			}
			s.bookFailedAttempt(home, waiting.Account, home.cachedUsage().At, out, turn, provider, agentType, sessionID, false)
			unasked[home.name()] = out.leftBoxUnasked()
		}
	}
	mark := session.Refugee{Origin: waiting.Peer + "/" + waiting.Account, PlacedAt: time.Now().UTC()}
	placed := s.spreadRefugee(w, r, provider, poolModel, agentType, sessionID, home, mark, turn, unasked)
	switch placed.outcome {
	case refugeeLocal:
		s.endWait(agentType, sessionID)
		return false
	case refugeeServed:
		s.endWait(agentType, sessionID)
		return true
	case refugeeClientGone:
		return true
	}
	if s.Logger != nil {
		s.Logger.Warn(waitingRefugeeNotPlacedMessage,
			"agent", agentType, "session", sessionID, "origin", mark.Origin,
			"why", placed.why(), "laptop_free_slots", placed.laptopFree)
	}
	w.Header().Set("Retry-After", strandedRetryAfter)
	w.Header().Set(subrouterReasonHeader, strandedGaveUpReason)
	http.Error(w, "subrouter: the account this chat's prompt cache was on still cannot be reached, and no account can take it yet; restart it later",
		http.StatusServiceUnavailable)
	return true
}

// waitingHome is the chat's wait for room: the session store's, or the one
// this process keeps when the store could not save it (unlinkStranded).
func (s Server) waitingHome(agentType, sessionID string) (session.Waiting, bool) {
	if assignment, ok := s.Sessions.Get(agentType, sessionID); ok && assignment.Waiting != nil {
		return *assignment.Waiting, true
	}
	return s.Hold.waitingFor(session.ScopedSessionKey(agentType, sessionID))
}

// endWait ends a waiting chat's record once it is placed: a box served its
// turn, whatever the box answered, or a local account took it. It stays
// there now (D4).
func (s Server) endWait(agentType, sessionID string) {
	s.Hold.endWait(session.ScopedSessionKey(agentType, sessionID))
	if err := s.Sessions.EndWait(agentType, sessionID); err != nil && s.Logger != nil {
		s.Logger.Warn("waiting chat's record not ended; its next turn tries its home box first again",
			"agent", agentType, "session", sessionID, "error", err)
	}
}

// spreadRefugee tries to place a refugee from origin, marked mark: on the
// account with the most free slots on any box but origin, then, when no box
// can take it (refugeeBoxUnasked), on the local account with the most free
// slots. Ties go to the better ranked account. unasked holds the boxes the
// caller could not ask, and gains those this placement could not.
func (s Server) spreadRefugee(w http.ResponseWriter, r *http.Request, provider accounts.Provider, poolModel, agentType, sessionID string, origin *RemotePool, mark session.Refugee, turn *remotePoolTurn, unasked map[string]bool) refugeePlacement {
	key := session.ScopedSessionKey(agentType, sessionID)
	if origin != nil {
		turn.mu.Lock()
		turn.downPeers[origin.name()] = true
		turn.mu.Unlock()
	}
	var ranking *crossPoolRanking
	if body, ok := replayableForwardBody(r); ok && s.peerForwardingAllowed(r) {
		ranking = s.newCrossPoolRanking(r, provider, poolModel, turn)
		// A refugee is counted on the account it goes to, so it goes only
		// where the forward can name one.
		ranking.targetedPeersOnly()
		targets := s.refugeeBoxSlots(ranking, key, poolModel)
		for len(targets) > 0 {
			target := targets[0]
			peer := target.Peer.name()
			if turn.boundReached() {
				for _, left := range targets {
					if left.free > 0 {
						unasked[left.Peer.name()] = true
					}
				}
				break
			}
			if turn.excluded(peer, target.Account) {
				targets = targets[1:]
				continue
			}
			claim, held := target.Peer.delegations.claimRefugee(key, time.Now(), remotePoolDelegationTTLFor(agentType), peer, target.Account, mark, target.slots, target.slots-target.free)
			if claim == 0 && held < target.slots {
				// Another refugee took a slot on it since the count.
				recountBoxSlots(targets, key)
				continue
			}
			targets = targets[1:]
			if claim == 0 {
				continue
			}
			out := s.attemptCandidate(w, r, agentType, sessionID, body, remotePoolHoldSet, target.crossPoolCandidate, ranking)
			if out.Served {
				s.logRefugeePlaced(agentType, sessionID, mark, peer, target.Account, held, target.slots)
				return refugeePlacement{outcome: refugeeServed}
			}
			target.Peer.delegations.release(key, claim)
			if out.clientGone() {
				return refugeePlacement{outcome: refugeeClientGone}
			}
			if out.leftBoxUnasked() {
				unasked[peer] = true
			}
		}
	}
	var locals []localSlot
	laptopFree := 0
	if s.Sessions != nil {
		locals = s.refugeeLocalSlots(r, provider, poolModel, agentType, sessionID)
		for _, slot := range locals {
			laptopFree += max(slot.free, 0)
		}
	}
	if box := s.refugeeBoxUnasked(r, ranking, unasked); box != "" {
		return refugeePlacement{unaskedBox: box, laptopFree: laptopFree}
	}
	for _, slot := range locals {
		held, placed, err := s.Sessions.PutRefugee(agentType, sessionID, slot.account.ID, mark, slot.slots, s.countsAsLocalRefugee)
		if err != nil {
			break
		}
		if !placed {
			continue
		}
		s.clearHoldElsewhere(key, nil)
		s.Hold.placedHere(key)
		// Selection re-ranks a chat idle past the warm window, uncapped.
		s.ActiveSessions.markSeen(agentType, sessionID)
		turn.mu.Lock()
		turn.stayLocal = true
		turn.mu.Unlock()
		s.logRefugeePlaced(agentType, sessionID, mark, "", slot.account.ID, held, slot.slots)
		return refugeePlacement{outcome: refugeeLocal}
	}
	return refugeePlacement{laptopFree: laptopFree}
}

// refugeeBoxSlots is every account on a box but the turn's down ones that a
// refugee may be placed on, most free slots first, the better ranked first
// among equals. A box account's free slots are its cap less the live
// refugees this daemon holds on it.
func (s Server) refugeeBoxSlots(ranking *crossPoolRanking, key, poolModel string) []boxSlot {
	now := time.Now()
	var out []boxSlot
	for rank, candidate := range ranking.rankedPeers() {
		slots := s.refugeeSlotsFor(candidate.Peer.name(), candidate.Account, poolModel)
		out = append(out, boxSlot{candidate, rank, slots, slots - candidate.Peer.delegations.refugeesOn(key, candidate.Account, now)})
	}
	slices.SortFunc(out, mostFreeBoxSlotFirst)
	return out
}

// recountBoxSlots counts each account's free slots again and sorts them as
// refugeeBoxSlots does.
func recountBoxSlots(slots []boxSlot, key string) {
	now := time.Now()
	for i := range slots {
		slots[i].free = slots[i].slots - slots[i].Peer.delegations.refugeesOn(key, slots[i].Account, now)
	}
	slices.SortFunc(slots, mostFreeBoxSlotFirst)
}

func mostFreeBoxSlotFirst(a, b boxSlot) int {
	return cmp.Or(cmp.Compare(b.free, a.free), cmp.Compare(a.rank, b.rank))
}

// refugeeLocalSlots is the local accounts a refugee may be placed on, most
// free slots first.
func (s Server) refugeeLocalSlots(r *http.Request, provider accounts.Provider, poolModel, agentType, sessionID string) []localSlot {
	held := s.Sessions.RefugeesByAccount(agentType, sessionID, s.countsAsLocalRefugee)
	var out []localSlot
	for _, account := range s.refugeeLocalCandidates(r, provider, poolModel) {
		slots := s.refugeeSlotsFor("", account.ID, poolModel)
		out = append(out, localSlot{account, slots, slots - held[account.ID]})
	}
	slices.SortStableFunc(out, func(a, b localSlot) int { return cmp.Compare(b.free, a.free) })
	return out
}

// refugeeBoxUnasked names the first box not found down, in configuration
// order, that this placement could not ask: its breaker open, its dial
// failed, it may be running the turn, it answered transiently or was not
// reached before the refusal bound (unasked), or its usage is unknown. A
// waiting chat's home box is one when it is not found down. While one is
// named the laptop takes nothing. "" when every box is found down, has every
// account full, refused or declined, or is not an option: closed to this
// caller, ignoring targets, or no forward possible (ranking nil).
func (s Server) refugeeBoxUnasked(r *http.Request, ranking *crossPoolRanking, unasked map[string]bool) string {
	if ranking == nil {
		return ""
	}
	fetched := map[*RemotePool]AdminSnapshot{}
	for _, entry := range ranking.snapshots {
		fetched[entry.peer] = entry.snapshot
	}
	now := time.Now()
	for _, p := range s.peers() {
		if !s.peerOpenTo(p, r) || p.breaker.foundDown() {
			continue
		}
		name := p.name()
		if unasked[name] || ranking.turn.unreached(name) {
			return name
		}
		if snapshot, ok := fetched[p]; ok && (len(snapshot.Body) == 0 || !verdictsCurrent(snapshot.At, now)) {
			return name
		}
	}
	return ""
}

// leftBoxUnasked reports a failed attempt that did not learn whether the box
// can take the chat: the box was not reached, or it answered transiently.
func (o peerForwardOutcome) leftBoxUnasked() bool {
	return o.Unreachable || (o.Refusal.Reason == "" && o.Switch.Kind != switchForced)
}

// countsAsLocalRefugee is a local refugee that still takes its account's slot:
// warm (sessionWarm), and not held on a peer since (its local pin is then a
// placeholder).
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

// logRefugeePlaced is the line for each placement: the account's warm
// refugees, this one included, its cap, and the slots it had free before
// this one, all as the claim counted them.
func (s Server) logRefugeePlaced(agentType, sessionID string, mark session.Refugee, peer, account string, held, slots int) {
	if s.Logger == nil {
		return
	}
	s.Logger.Warn(refugeePlacedMessage,
		"agent", agentType, "session", sessionID, "origin", mark.Origin,
		"to_peer", peer, "to_account", account, "refugees", held, "refugee_slots", slots, "free_slots", slots-held+1)
}
