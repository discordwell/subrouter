package proxy

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// An unstarted week, the proxy's half (PATCHES §86.2). selectacct reads the
// fact, ranks on it and keeps the start marks; this file sets a mark wherever
// the router places a new session, and logs it: on a local account when
// selection places it, on a peer's account when the peer accepts the targeted
// forward, and, on the peer, when it accepts a targeted placement
// (selectTargetedAccount).

// startMarkSetMessage is the decision log line of a start mark: the router
// gave a new session to an account whose weekly window had not started.
const startMarkSetMessage = "weekly window not started; this new session starts its clock"

// noteNewSession marks account, a local account a new session was placed on,
// when scheduler (the ranking that placed it) read its week as unstarted.
func (s Server) noteNewSession(scheduler selectacct.Scheduler, account accounts.Account, agentType, sessionID string) {
	s.noteStartMark(scheduler, schedulerAccount(account), "", account.ID, agentType, sessionID)
}

// noteNewLocalSession is noteNewSession for selection's own pick. When the
// cross-pool ranking sent the turn to a peer's account, the pick only holds
// the session's pin, so its mark waits until the turn comes back to it
// (noteDeferredLocalSession).
func (s Server) noteNewLocalSession(ranking *crossPoolRanking, scheduler selectacct.Scheduler, account accounts.Account, agentType, sessionID string) {
	if ranking != nil && ranking.ranked && ranking.placement.Peer != nil {
		ranking.localStart = &deferredLocalStart{scheduler: scheduler, account: account}
		return
	}
	s.noteNewSession(scheduler, account, agentType, sessionID)
}

// deferredLocalStart is selection's pick, kept while a peer's account is
// tried first.
type deferredLocalStart struct {
	scheduler selectacct.Scheduler
	account   accounts.Account
}

// noteDeferredLocalSession marks selection's pick once every peer attempt of
// the turn failed and the pick serves it.
func (s Server) noteDeferredLocalSession(ranking *crossPoolRanking, agentType, sessionID string) {
	if ranking == nil || ranking.localStart == nil {
		return
	}
	start := ranking.localStart
	ranking.localStart = nil
	s.noteNewSession(start.scheduler, start.account, agentType, sessionID)
}

// peerStartKey carries, on a forwarded request's context, the peer's account
// a new session is placed on and the score the ranking held for it, so the
// forward marks it the moment the peer accepts the target: before the answer
// is relayed, which for a websocket is the whole conversation.
type peerStartKey struct{}

type peerStart struct {
	server               Server
	candidate            crossPoolCandidate
	agentType, sessionID string
}

// withPeerStart is r for a targeted attempt on candidate.
func withPeerStart(r *http.Request, s Server, candidate crossPoolCandidate, agentType, sessionID string) *http.Request {
	if r == nil || candidate.Account == "" || candidate.Peer == nil {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), peerStartKey{}, peerStart{
		server: s, candidate: candidate, agentType: agentType, sessionID: sessionID,
	}))
}

// notePeerTargetServed marks the peer's account a forward was targeted at,
// once the peer served it on that account. A refusal or a failed attempt
// never gets here, and an answer the peer served on another account marks
// nothing: the session did not start the target's week.
func notePeerTargetServed(ctx context.Context, pool *RemotePool, target string, out peerForwardOutcome) {
	start, ok := ctx.Value(peerStartKey{}).(peerStart)
	if !ok || !out.Served || target == "" || (out.ServedAccount != "" && !strings.EqualFold(out.ServedAccount, target)) ||
		start.candidate.Peer != pool || !strings.EqualFold(start.candidate.Account, target) {
		return
	}
	candidate := start.candidate
	placeholder := candidate.placeholder(candidate.Provider)
	score := candidate.Score
	score.AccountID, score.Provider, score.Remote = placeholder.ID, placeholder.Provider, true
	start.server.noteStartMark(selectacct.NewScheduler([]selectacct.Score{score}), placeholder,
		pool.name(), candidate.Account, start.agentType, start.sessionID)
}

// noteStartMark sets the mark under key and logs it. account and peer name
// the account as the operator knows it.
func (s Server) noteStartMark(scheduler selectacct.Scheduler, key accounts.Account, peer, account, agentType, sessionID string) {
	if !s.SchedulerRef.NoteNewSession(scheduler, key) || s.Logger == nil {
		return
	}
	until, _ := s.SchedulerRef.StartSentUntil(key.Provider, key.ID)
	s.Logger.Info(startMarkSetMessage,
		"agent", agentType,
		"session", sessionID,
		"account", account,
		"peer", peer,
		"provider", string(accountProviderOrCodex(key)),
		"priority_off_until", until.UTC().Format(time.RFC3339))
}
