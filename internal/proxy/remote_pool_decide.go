package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// Cross-pool placement (PATCHES §31; owner rules P1-P9, design D2-D7).
//
// ONE ranking decides where a session goes, over everything this daemon can
// see: its own accounts and every eligible account of every configured peer,
// each peer account carrying its owner's published verdict (Score.Remote,
// Score.PeerVerdict). placeAcrossPools is that ranking: the shared comparator
// (selectacct compare, through Pick/Explain) run ONCE over the merged list,
// with local's LocalPreferenceRatio advantage on pace. With zero peers it is
// exactly this daemon's own pick (pickRoutingAccount calls it), which is what
// makes "the box's own selection" and "the laptop's cross-pool decision" the
// same function.
//
// Every re-rank site calls it: a new session, a cold session (P9, from
// migrateIdleSession), a forced switch (P8: a hold ended by force, a refusal),
// a refusal recompute (P6) and a delegated session whose account ran out (P4).
// Stickiness -- the warm rule, eviction, forced-vs-transient, the per-provider
// cold rules (selection_policy.go) -- decides only WHETHER a session re-ranks;
// it never ranks.
//
// The winner is targeted (X-Subrouter-Target-Account): the peer serves exactly
// that account or refuses before contacting upstream, and a refusal recomputes
// over what is left, at most remotePoolMaxTargetRefusals failed peer attempts
// per turn, then local.

// remotePoolTurn is one client request's cross-pool state (P6/D4), shared by
// every entry point that can forward -- the held turn, the decision, the
// no-account chain, the transient one-turn serve and the transport stage -- so
// the refusal bound is per TURN, not per entry point.
type remotePoolTurn struct {
	mu           sync.Mutex
	failed       map[remotePoolTargetKey]string // (peer, account) refused or failed this turn -> reason
	downPeers    map[string]bool                // peers unreachable this turn
	attempts     int                            // failed peer attempts; bound remotePoolMaxTargetRefusals
	stayLocal    bool                           // a held turn failed TRANSIENTLY: no more peer attempts
	forcedSwitch bool                           // a hold ended by force: re-rank the local pin too
	lastRefused  remotePoolTargetKey            // for the recompute log line
	lastReason   string
	unsentPeers  map[string]bool // peers whose request never left this turn (DNS, dial, TLS, an open breaker)
	peerReached  bool            // a peer answered this turn, or may be running it
	mayRun       *mayRunPeers    // peers that may be running this request, from any of its hold attempts
	// subagentFable: a subagent's Fable turn, which a held account whose
	// Fable pool is spent serves as Opus (serveHeldAttempt).
	subagentFable bool
}

type remotePoolTargetKey struct{ Peer, Account string }

// newRemotePoolTurn starts one attempt's cross-pool state. The peers that may
// be running the request carry over from its earlier hold attempts (PATCHES
// §67).
func newRemotePoolTurn(ctx context.Context) *remotePoolTurn {
	return &remotePoolTurn{failed: map[remotePoolTargetKey]string{}, downPeers: map[string]bool{}, unsentPeers: map[string]bool{},
		mayRun: holdMayRun(ctx)}
}

// mayRunPeers is the peers a request's forward reached without an answer (a
// header timeout, a connection cut after it left). Any of them may be running
// it, so it is never sent to them again (PATCHES §58). The hold shares one
// across a request's attempts, because each attempt starts a fresh turn
// (PATCHES §67).
type mayRunPeers struct {
	mu    sync.Mutex
	peers map[string]bool
}

func (m *mayRunPeers) add(peer string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.peers == nil {
		m.peers = map[string]bool{}
	}
	m.peers[peer] = true
}

func (m *mayRunPeers) has(peer string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peers[peer]
}

// noteReachLocked books whether a failed attempt's request left for the
// peer, and whether the peer may be running it. t.mu is held.
func (t *remotePoolTurn) noteReachLocked(peer string, out peerForwardOutcome) {
	if out.NeverSent {
		t.unsentPeers[peer] = true
		return
	}
	t.peerReached = true
	if out.Unreachable {
		t.mayRun.add(peer)
	}
}

func (t *remotePoolTurn) excluded(peer, account string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.downPeers[peer] {
		return true
	}
	_, failed := t.failed[remotePoolTargetKey{peer, account}]
	return failed
}

// peerUsable is Server.peerUsable for this turn. A peer it skips for an open
// breaker is booked as one its request never left for: the breaker may close
// before the turn is answered, and peersUnreachable must read what the turn
// found, not the breaker as it is by then.
func (t *remotePoolTurn) peerUsable(s Server, p *RemotePool, r *http.Request) bool {
	if !s.peerOpenTo(p, r) {
		return false
	}
	if p.breaker.allow(time.Now()) {
		return true
	}
	if t != nil {
		t.mu.Lock()
		t.unsentPeers[p.name()] = true
		t.mu.Unlock()
	}
	return false
}

// unreached reports a peer this turn could not ask: its request never left
// (an open breaker, a failed dial), or it may be running it.
func (t *remotePoolTurn) unreached(peer string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.unsentPeers[peer] || t.mayRun.has(peer)
}

func (t *remotePoolTurn) peerDown(peer string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.downPeers[peer] || t.mayRun.has(peer)
}

// recordFailure books one failed peer attempt. An unreachable peer is excluded
// whole for the rest of the turn (Deviation X5: it also counts toward the
// bound, or a blackholed peer would cost a header timeout per attempt); a
// refusal excludes only that account. A request that never left does not
// count toward the bound: nothing was sent, and the peer is excluded, so each
// peer costs at most one failed dial a turn and every peer gets asked.
func (t *remotePoolTurn) recordFailure(peer, account string, out peerForwardOutcome) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !out.NeverSent {
		t.attempts++
	}
	t.noteReachLocked(peer, out)
	reason := out.Switch.Reason
	if out.Refusal.Reason != "" {
		reason = out.Refusal.Reason
	}
	key := remotePoolTargetKey{peer, account}
	t.failed[key] = reason
	if out.Unreachable || out.Switch.Reason == "peer_declined" || account == "" {
		// Unreachable, or a legacy peer whose whole pool declined: nothing on
		// it is worth another attempt this turn.
		t.downPeers[peer] = true
	}
	t.lastRefused, t.lastReason = key, reason
}

// failedBefore reports a turn on which a peer account already failed -- a held
// turn refused or ended by its cached verdict -- so the next decision line is a
// recompute and names what failed (D6).
func (t *remotePoolTurn) failedBefore() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.failed) > 0
}

func (t *remotePoolTurn) boundReached() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.attempts >= remotePoolMaxTargetRefusals
}

// crossPoolCandidate is one peer account competing in the ranking.
type crossPoolCandidate struct {
	Peer *RemotePool
	// Account is the peer's account ID; empty for a LEGACY peer (one whose
	// usage body carries no verdicts), which competes with its best score and
	// is forwarded untargeted, exactly as before §31.
	Account string
	// Provider is the scheduler provider the account competes under, so it
	// keeps one ScoreKey -- and one start mark (PATCHES §86) -- in every
	// ranking, with or without local candidates. Empty in a hand-built
	// candidate, which then takes the local candidates' provider.
	Provider accounts.Provider
	Score    selectacct.Score
	Legacy   bool
	Carried  bool
	// Reason is the verdict's reason ("eligible", "burndown"), or "legacy".
	Reason string
	// BodyAt is when the usage body this candidate came from was measured; a
	// refusal is remembered until a newer one.
	BodyAt time.Time
}

// crossPoolPlacement is the ranking's answer.
type crossPoolPlacement struct {
	// Local: an account of this daemon won; Account is it.
	Local bool
	// Account is the local winner, or -- when a peer won -- the best LOCAL
	// account, which is what serves the turn if every peer attempt fails.
	Account  accounts.Account
	HasLocal bool
	// Peer is the winner when !Local; BestPeer the best-ranked peer candidate
	// either way (it names the peer_* fields of the decision log).
	Peer     *crossPoolCandidate
	BestPeer *crossPoolCandidate
	// Clause is the comparator clause that put the winner first -- over
	// RunnerUp, the candidate ranked second (Explain compares each entry with
	// the one below it). RunnerUp is a local account ID or "<peer>/<account>"
	// ("<peer>/legacy" for a peer without verdicts). Both empty when no peer
	// competed: the ranking then is this daemon's own Pick.
	Clause   string
	RunnerUp string
}

// crossPoolCandidateLabel names a ranked candidate for the decision line.
func crossPoolCandidateLabel(peer crossPoolCandidate) string {
	account := peer.Account
	if account == "" {
		account = "legacy"
	}
	return peer.Peer.name() + "/" + account
}

// crossPoolCandidateID is a peer account's name inside the merged ranking. The
// NUL separators cannot occur in a local account ID, so a peer account can
// never collide with (or be decorated as) a local one.
func crossPoolCandidateID(peer, account string) string {
	return "peer\x00" + peer + "\x00" + account
}

// placeAcrossPools is THE placement ranking (D7): local candidates and peer
// candidates in one list, one comparator, one Pick.
//
// scheduler is the scheduler selection built for this request -- ForModel
// narrowed, live debits, parked and live session counts, reserves, the machine
// term -- so local accounts are ranked exactly as this daemon's own selection
// ranks them. Peer scores are merged into it as Remote scores, which the
// scheduler leaves undecorated: their live counts and reserves are the owning
// daemon's, carried in its verdict.
//
// With no peers this IS pickRoutingAccount (the box's own selection).
func placeAcrossPools(scheduler selectacct.Scheduler, local []accounts.Account, peers []crossPoolCandidate) (crossPoolPlacement, error) {
	if len(peers) == 0 {
		picked, err := scheduler.Pick(SchedulerAccounts(local))
		if err != nil {
			return crossPoolPlacement{}, err
		}
		account, err := matchPickedAccount(local, picked)
		if err != nil {
			return crossPoolPlacement{}, err
		}
		return crossPoolPlacement{Local: true, Account: account, HasLocal: true}, nil
	}
	ranked, byID, err := explainAcrossPools(scheduler, local, peers)
	if err != nil || len(ranked) == 0 {
		return crossPoolPlacement{}, err
	}
	out := crossPoolPlacement{Clause: ranked[0].Reason}
	if len(ranked) > 1 {
		if i, isPeer := byID[ranked[1].Account.ID]; isPeer {
			out.RunnerUp = crossPoolCandidateLabel(peers[i])
		} else {
			out.RunnerUp = ranked[1].Account.ID
		}
	}
	for _, entry := range ranked {
		if i, isPeer := byID[entry.Account.ID]; isPeer {
			if out.BestPeer == nil {
				candidate := peers[i]
				out.BestPeer = &candidate
			}
			continue
		}
		if !out.HasLocal {
			account, err := matchPickedAccount(local, entry.Account)
			if err != nil {
				return crossPoolPlacement{}, err
			}
			out.Account, out.HasLocal = account, true
		}
	}
	if _, isPeer := byID[ranked[0].Account.ID]; isPeer {
		out.Peer = out.BestPeer
	} else {
		out.Local = true
	}
	return out, nil
}

// explainAcrossPools ranks local and peer candidates in one list, best first.
// byID maps each peer candidate's ranked ID to its index in peers.
func explainAcrossPools(scheduler selectacct.Scheduler, local []accounts.Account, peers []crossPoolCandidate) (ranked []selectacct.Explanation, byID map[string]int, err error) {
	// The peers compete in the request's scheduler provider, so their scores
	// and their placeholder accounts share one ScoreKey space with the local
	// candidates.
	provider := accounts.Provider("")
	if len(local) > 0 {
		provider = accountProviderFor(local[0].Provider)
	}
	merged := scheduler
	candidates := SchedulerAccounts(local)
	byID = make(map[string]int, len(peers))
	for i, peer := range peers {
		placeholder := peer.placeholder(provider)
		score := peer.Score
		score.AccountID, score.Provider, score.Remote = placeholder.ID, placeholder.Provider, true
		merged = merged.WithScore(score)
		candidates = append(candidates, placeholder)
		byID[placeholder.ID] = i
	}
	ranked, err = merged.Explain(candidates)
	return ranked, byID, err
}

// placeholder is the account a peer candidate competes as in the merged
// ranking: its crossPoolCandidateID under its own provider, or fallback (the
// local candidates') when it carries none.
func (c crossPoolCandidate) placeholder(fallback accounts.Provider) accounts.Account {
	provider := fallback
	if c.Provider != "" {
		provider = c.Provider
	}
	return accounts.Account{ID: crossPoolCandidateID(c.Peer.name(), c.Account), Provider: provider, AuthMode: accounts.AuthModeOAuth}
}

// matchPickedAccount maps the scheduler's winner back to the full local
// account (pickRoutingAccount's own lookup).
func matchPickedAccount(local []accounts.Account, picked accounts.Account) (accounts.Account, error) {
	for _, candidate := range local {
		if candidate.ID == picked.ID && accountProviderFor(candidate.Provider) == picked.Provider {
			return candidate, nil
		}
	}
	return accounts.Account{}, errUnknownPickedAccount(picked.ID)
}

type errUnknownPickedAccount string

func (e errUnknownPickedAccount) Error() string {
	return "scheduler returned unknown account " + strconv.Quote(string(e))
}

// --- the per-turn ranking -----------------------------------------------------

// Why selection kept a session where it is without ranking it, for the
// decision log (P9: the log must say warm or cold, never "warm" for a cold
// session).
const (
	crossPoolKeptWarm         = "session is warm on its account; staying local"
	crossPoolKeptColdRetained = "cold session retained on its account by its provider's rule; not re-ranked"
	crossPoolNotRanked        = "selection did not rank this turn; staying local"
	// A peer without verdicts cannot be targeted, and this turn is one local
	// can keep, where origin/main never asked it (targetedOnly).
	crossPoolReasonLegacyNotAsked = "no targetable peer account is eligible (a peer without verdicts is not asked on this turn); staying local"
)

// crossPoolRanking carries one turn's ranking inputs so every recompute is the
// SAME ranking with the failed candidates removed. Built by the handler only
// when this turn may go to a peer at all; nil everywhere else, which makes
// every ranking a zero-peer one.
type crossPoolRanking struct {
	server    Server
	request   *http.Request
	provider  accounts.Provider
	poolModel string
	turn      *remotePoolTurn

	fetched    bool
	snapshots  []crossPoolPeerSnapshot
	considered int

	// Set by the last ranking selection ran.
	ranked       bool
	scheduler    selectacct.Scheduler
	local        []accounts.Account
	placement    crossPoolPlacement
	candidates   int
	excluded     int
	measured     bool
	kept         string
	sessionState string

	// targetedOnly drops every LEGACY peer candidate (a peer with no verdicts,
	// or one seen ignoring a target) from this turn's rankings, recomputes
	// included. A legacy peer can only be sent an UNTARGETED forward, which it
	// places by its own pre-§31 rules -- past a burndown account's cap, onto
	// accounts whose 5h windows are spent (the 9/27 incident) -- so it may win
	// only where origin/main already consulted it (a new session, an account
	// that left the pool, an evicted pin, a cold session its local ranking
	// moves, local failover giving up). Set where the turn stays on an account
	// local can serve and origin/main never asked the peer: a forced switch
	// inside local failover (next serves it) and a cold session whose own
	// account is still the best LOCAL one (P9). D1: absent verdicts keep
	// today's behaviour for that peer -- they never widen it. legacyDropped
	// says the last ranking did so, for the decision line.
	targetedOnly  bool
	legacyDropped bool

	// localStart is selection's pick while a peer's account is tried first:
	// it is marked if the turn comes back to it (PATCHES §86.2).
	localStart *deferredLocalStart
}

type crossPoolPeerSnapshot struct {
	peer     *RemotePool
	snapshot AdminSnapshot
}

func (s Server) newCrossPoolRanking(r *http.Request, provider accounts.Provider, poolModel string, turn *remotePoolTurn) *crossPoolRanking {
	return &crossPoolRanking{server: s, request: r, provider: provider, poolModel: poolModel, turn: turn}
}

// keep records that selection kept the session without ranking it. Nil-safe:
// selection calls it whether or not this turn may go to a peer.
func (c *crossPoolRanking) keep(reason, state string) {
	if c == nil {
		return
	}
	c.kept, c.sessionState = reason, state
}

// keptWarm reports that selection kept the session on its account because
// it is warm there.
func (c *crossPoolRanking) keptWarm() bool {
	return c != nil && c.kept == crossPoolKeptWarm
}

// targetedPeersOnly makes every ranking left in this turn exclude legacy peers
// (targetedOnly). Nil-safe.
func (c *crossPoolRanking) targetedPeersOnly() {
	if c != nil {
		c.targetedOnly = true
	}
}

func (c *crossPoolRanking) rankedAlready() bool {
	return c != nil && c.ranked
}

// invalidate forgets a ranking whose selection then failed: the turn is no
// longer placed by it.
func (c *crossPoolRanking) invalidate() {
	if c == nil {
		return
	}
	c.ranked = false
	c.kept = ""
}

// fetch reads every usable peer's usage body, in parallel, waiting at most
// remotePoolDecisionFetchBudget for any that are not cached (X6). Once per turn.
func (c *crossPoolRanking) fetch() {
	if c.fetched {
		return
	}
	c.fetched = true
	var peers []*RemotePool
	for _, p := range c.server.peers() {
		if c.turn.peerDown(p.name()) || !c.turn.peerUsable(c.server, p, c.request) {
			continue
		}
		peers = append(peers, p)
	}
	c.considered = len(peers)
	c.snapshots = make([]crossPoolPeerSnapshot, len(peers))
	deadline := time.Now().Add(remotePoolDecisionFetchBudget)
	var wg sync.WaitGroup
	for i, p := range peers {
		wg.Add(1)
		go func(i int, p *RemotePool) {
			defer wg.Done()
			c.snapshots[i] = crossPoolPeerSnapshot{peer: p, snapshot: p.usageForDecision(deadline)}
		}(i, p)
	}
	wg.Wait()
}

// peerCandidates is every peer account that may compete right now: fetched
// once per turn, filtered on every call by this turn's failures and each
// peer's refusal marks.
func (c *crossPoolRanking) peerCandidates() (out []crossPoolCandidate, excluded int, measured bool) {
	if c == nil {
		return nil, 0, false
	}
	c.fetch()
	now := time.Now()
	c.legacyDropped = false
	for _, entry := range c.snapshots {
		if c.turn.peerDown(entry.peer.name()) {
			continue
		}
		candidates, skipped, ok := entry.peer.decisionCandidates(entry.snapshot, string(c.provider), c.poolModel, c.turn, now)
		if c.targetedOnly {
			targeted := candidates[:0]
			for _, candidate := range candidates {
				if candidate.Legacy {
					skipped++
					c.legacyDropped = true
					continue
				}
				targeted = append(targeted, candidate)
			}
			candidates = targeted
		}
		out = append(out, candidates...)
		excluded += skipped
		measured = measured || ok
	}
	return out, excluded, measured
}

// rank runs the ONE ranking over local and every peer candidate, and records it
// as this turn's placement.
func (c *crossPoolRanking) rank(scheduler selectacct.Scheduler, local []accounts.Account, state string) (crossPoolPlacement, error) {
	peers, excluded, measured := c.peerCandidates()
	placement, err := placeAcrossPools(scheduler, local, peers)
	if err != nil {
		return placement, err
	}
	c.ranked = true
	c.scheduler, c.local, c.placement = scheduler, local, placement
	c.candidates, c.excluded, c.measured = len(local)+len(peers), excluded, measured
	if state != "" {
		c.sessionState = state
	}
	c.kept = ""
	return placement, nil
}

// recompute re-runs the last ranking with this turn's failures removed (P6).
// With no local candidates (the no-account chain, the transport stage) it
// ranks the peers alone.
func (c *crossPoolRanking) recompute() (crossPoolPlacement, error) {
	scheduler, local := c.scheduler, c.local
	if !c.ranked {
		scheduler = c.peersOnlyScheduler()
	}
	return c.rank(scheduler, local, "")
}

// peersOnlyScheduler is the scheduler a ranking of peers' accounts alone
// starts from: no local score, but this daemon's start marks (PATCHES §86),
// so a peer's account it just sent a new session to loses its priority here
// too, and the turn's model (PATCHES §104), so an Opus or Sonnet turn ranks
// each peer account on the Fable meter its owner published (§65), as this
// daemon's own selection does. Without it the laptop, whose own pool usually
// cannot serve, ranked every such turn with the preference off.
//
// A Fable turn stays unscoped: scoping it would also load a peer's Fable
// choice for it (§95.2), which this path has never done (KNOWN-ISSUES).
func (c *crossPoolRanking) peersOnlyScheduler() selectacct.Scheduler {
	scheduler := c.server.SchedulerRef.WithStartMarks(selectacct.NewScheduler(nil))
	// 2026-11-24 owner: route Opus and other non-Fable sessions onto accounts
	// that are out of Fable but still have week left, so Fable headroom
	// elsewhere is kept for Fable sessions.
	if selectacct.ModelKey(c.poolModel) == selectacct.ModelKey(agentclaude.FableFeature) {
		return scheduler
	}
	return scheduler.ForModel(c.poolModel)
}

// forcedRerank is the ranking of a FORCED switch that local can serve (P8):
// next -- the best local replacement, which serves the turn if no peer account
// outranks it -- against every targetable peer account. Every forced re-rank
// site with a local replacement in hand calls it: the usage failover
// (forcedSwitchAcrossPools), a dead credential (the handler, after
// refreshSelectedAccount failed over) and the Opus substitution stage. next
// stays in every recompute after a refusal (P6), and a peer without verdicts
// is not in it (targetedOnly): origin/main served these turns on next without
// asking the peer, and an untargeted forward is placed by the peer's own
// pre-§31 rules.
func (c *crossPoolRanking) forcedRerank(scheduler selectacct.Scheduler, next accounts.Account) (crossPoolPlacement, error) {
	c.targetedPeersOnly()
	return c.rank(scheduler, []accounts.Account{next}, "forced")
}

// rankPeersOnly ranks the peers with no local candidate: local cannot serve.
func (c *crossPoolRanking) rankPeersOnly() (crossPoolPlacement, error) {
	return c.rank(c.peersOnlyScheduler(), nil, "")
}

// rankedPeers is every peer candidate left in this turn, best first.
func (c *crossPoolRanking) rankedPeers() []crossPoolCandidate {
	peers, _, _ := c.peerCandidates()
	if len(peers) == 0 {
		return nil
	}
	ranked, byID, err := explainAcrossPools(c.peersOnlyScheduler(), nil, peers)
	if err != nil {
		return nil
	}
	out := make([]crossPoolCandidate, 0, len(ranked))
	for _, entry := range ranked {
		out = append(out, peers[byID[entry.Account.ID]])
	}
	return out
}

// placeForSession is where selection ranks. ranking nil is this daemon's own
// pool alone.
//
// Selection always needs a LOCAL account -- it is what serves the turn when no
// peer does, and what the session is pinned to meanwhile -- so an empty local
// pool is selection's ErrNoAccounts exactly as before, whatever the peers hold:
// the handler's no-account chain then ranks the peers alone (serveAcrossPeers).
func (s Server) placeForSession(scheduler selectacct.Scheduler, local []accounts.Account, ranking *crossPoolRanking, state string) (crossPoolPlacement, error) {
	if len(local) == 0 {
		return crossPoolPlacement{}, selectacct.ErrNoAccounts
	}
	if ranking == nil {
		return placeAcrossPools(scheduler, local, nil)
	}
	return ranking.rank(scheduler, local, state)
}

// verdictsCurrent reports a usage body read at at that is young enough for
// its verdicts to rank at now; a body with no time is read as of now.
func verdictsCurrent(at, now time.Time) bool {
	return at.IsZero() || now.Sub(at) <= remotePoolDecisionBodyMaxAge
}

// remotePoolDecisionBodyMaxAge is how old a verdict body may be and still
// rank: the peer's own last-good horizon (usageStatusLastGoodTTL, which
// remotePoolCarryTTL matches), so the two ends of the wire age a reading
// alike.
const remotePoolDecisionBodyMaxAge = remotePoolCarryTTL

// decisionCandidates turns one peer usage body into ranking candidates.
//
// A VERDICT peer (some row of the provider carries new_session) contributes
// every account that is measured, fresh, not known-invalid, and ELIGIBLE by its
// own verdict (P3) -- and not refused this turn or since this body. The score
// is the row's pool entry for this request's quota pool when the peer publishes
// one, else its top level.
//
// A LEGACY peer contributes one untargeted candidate, its best score exactly
// as before (bestPeerScoreFrom, carry included): absence of verdicts is never
// read as eligible or as ineligible.
func (p *RemotePool) decisionCandidates(snapshot AdminSnapshot, provider, poolModel string, turn *remotePoolTurn, now time.Time) ([]crossPoolCandidate, int, bool) {
	if p == nil || len(snapshot.Body) == 0 {
		return nil, 0, false
	}
	var rows []remotePoolAccountUsage
	if err := json.Unmarshal(snapshot.Body, &rows); err != nil {
		return nil, 0, false
	}
	verdictPeer := false
	for _, row := range rows {
		if row.Provider == provider && row.Normalized.NewSession != nil {
			verdictPeer = true
			break
		}
	}
	if !verdictPeer || p.ignoresTargets(snapshot.At) {
		if turn.excluded(p.name(), "") {
			return nil, 1, true
		}
		score, carried, ok := p.bestPeerScoreFrom(snapshot, provider)
		if !ok {
			return nil, 0, false
		}
		return []crossPoolCandidate{{
			Peer: p, Provider: accountProviderFor(accounts.Provider(provider)), Score: score, Legacy: true, Carried: carried, Reason: "legacy", BodyAt: snapshot.At,
		}}, 0, true
	}
	// A verdict is a statement about NOW on the peer: eligible, burndown, at
	// its cap. The admin cache keeps the last good body for as long as the
	// usage endpoint fails -- which it can while forwards still work -- and a
	// verdict read off that body kept an account in the burndown tier, first
	// in every ranking, hours after its week rolled. Past
	// remotePoolDecisionBodyMaxAge (the peer's own last-good horizon) the
	// body ranks nothing: this peer is unmeasured, as if it had not answered.
	if !verdictsCurrent(snapshot.At, now) {
		return nil, 0, false
	}
	bodyAt := snapshot.At
	if bodyAt.IsZero() {
		bodyAt = now
	}
	poolKey := selectacct.ModelKey(poolModel)
	var out []crossPoolCandidate
	excluded := 0
	for _, row := range rows {
		if row.Provider != provider {
			continue
		}
		if row.unmeasured() || row.knownInvalid() {
			excluded++
			continue
		}
		normalized := row.Normalized
		if pool, ok := row.Normalized.Pools[poolKey]; ok && poolKey != "" {
			normalized = inheritPeerAccountFacts(pool, row.Normalized)
		}
		verdict := normalized.NewSession
		if verdict == nil || !verdict.Eligible {
			excluded++
			continue
		}
		if turn.excluded(p.name(), row.ID) || p.refusedFor(provider, row.ID, snapshot.At) {
			excluded++
			continue
		}
		// The body's seconds are counted from when it was read, not from
		// now: a deadline re-anchored to the clock never arrives.
		score, ok := normalized.score(row.ID, bodyAt)
		if !ok {
			excluded++
			continue
		}
		if !score.WeeklyResetAt.IsZero() && !score.WeeklyResetAt.After(now) {
			// The week this verdict judged has rolled since the body: its
			// burndown tier, its cap and its headroom describe a week that is
			// over. The next body judges the new one.
			excluded++
			continue
		}
		// The peer's numbers are measured; the targeted turns this daemon
		// sent the account since that body are debited here exactly as a
		// local account's routed requests are (ApplyLiveDebit).
		score = selectacct.ApplyLiveDebit(score, p.routed.since(provider, row.ID, snapshot.At))
		out = append(out, crossPoolCandidate{
			Peer: p, Account: row.ID, Provider: accountProviderFor(accounts.Provider(provider)), Score: score, Reason: verdict.Reason, BodyAt: snapshot.At,
		})
	}
	return out, excluded, true
}

// inheritPeerAccountFacts is selectacct's inheritAccountFacts for a published
// pool entry: a pool narrows which QUOTA a request draws from, it does not
// repeal the account's weekly roll, and a pool entry without its own verdict
// is judged by the account's (the owner refuses at request time if the pool
// itself is spent).
func inheritPeerAccountFacts(pool, account remotePoolNormalized) remotePoolNormalized {
	if pool.WeeklyHeadroom == nil || pool.SecondsToWeeklyRoll == nil {
		pool.WeeklyHeadroom, pool.SecondsToWeeklyRoll = account.WeeklyHeadroom, account.SecondsToWeeklyRoll
	}
	if pool.NewSession == nil {
		pool.NewSession = account.NewSession
	}
	// The Fable meter is the account's, published at the top level only
	// (PATCHES §65), like the reset hold below.
	pool.FableWeekSpent = account.FableWeekSpent
	// Freshness is the account's, always: a pool comes from the same usage
	// reading. A peer before PATCHES §52 published every measured pool
	// fresh:false under a fresh account, so the pool's own field is not read.
	pool.Fresh = account.Fresh
	// So is the reset hold: it is held only on a grant that clears the
	// general week, which every pool draws on, and the owner publishes it at
	// the top level only (PATCHES §56, §64). A pool entry's own fields are
	// never read, so a pool can neither gain a hold its account lacks nor
	// lose one its account has.
	pool.SecondsToHeldWeekResetExpiry = account.SecondsToHeldWeekResetExpiry
	pool.SecondsToHeldResetExpiry = account.SecondsToHeldResetExpiry
	// A reset choice is the account's too (PATCHES §95.2); its meter says
	// which pools load it.
	pool.ResetChoice = account.ResetChoice
	return pool
}

// --- acting on the ranking ----------------------------------------------------

// crossPoolLocal describes the local side for the decision log.
type crossPoolLocal struct {
	Account  string
	Capacity routingCapacity
	// Cause is why local cannot serve this turn, when it cannot (the
	// transient refresh, the no-account chain): the decision line's
	// local_cause. Empty when local was ranked.
	Cause string
	// FailedOverFrom is, on the transient refresh, the account selection
	// chose when its refresh found it dead and Account is the failover
	// candidate whose refresh then reached no verdict: the decision line's
	// local_failed_over_from. Empty otherwise.
	FailedOverFrom string
}

// serveCrossPoolDecision acts on the placement selection made for this turn:
// local wins -> false (serve locally); a peer wins -> targeted forward, and on
// a failed attempt the SAME ranking recomputes without it, until something
// serves, local wins, or the bound is reached (then local).
func (s Server) serveCrossPoolDecision(w http.ResponseWriter, r *http.Request, agentType, sessionID string, mode remotePoolHoldMode, local crossPoolLocal, ranking *crossPoolRanking) (served bool) {
	clientGone := false
	defer func() {
		// Every peer attempt failed and selection's pick serves the turn: it
		// is a new session there after all.
		if !served && !clientGone {
			s.noteDeferredLocalSession(ranking, agentType, sessionID)
		}
	}()
	if !ranking.ranked {
		reason := ranking.kept
		if reason == "" {
			reason = crossPoolNotRanked
		}
		s.logCrossPoolDecision(r, agentType, sessionID, local, ranking, crossPoolPlacement{Local: true}, reason, false)
		return false
	}
	placement := ranking.placement
	var body []byte
	bodyRead := false
	for recompute := ranking.turn.failedBefore(); ; recompute = true {
		if placement.Local || placement.Peer == nil {
			s.logCrossPoolDecision(r, agentType, sessionID, local, ranking, placement, crossPoolReason(placement, ranking), recompute)
			return false
		}
		if !bodyRead {
			// Buffered once, only now that a peer won: every attempt of the
			// turn sends byte-identical bytes, and the local path still reads
			// the restored body.
			var ok bool
			if body, ok = replayableForwardBody(r); !ok {
				s.logCrossPoolDecision(r, agentType, sessionID, local, ranking, crossPoolPlacement{Local: true, BestPeer: placement.Peer},
					"request body exceeds the replay buffer; staying local", recompute)
				return false
			}
			bodyRead = true
		}
		if ranking.turn.boundReached() {
			s.logCrossPoolDecision(r, agentType, sessionID, local, ranking, crossPoolPlacement{Local: true, BestPeer: placement.Peer},
				"refusal bound reached ("+strconv.Itoa(remotePoolMaxTargetRefusals)+" failed peer attempts); serving locally", recompute)
			return false
		}
		s.logCrossPoolDecision(r, agentType, sessionID, local, ranking, placement, crossPoolReason(placement, ranking), recompute)
		out := s.attemptCandidate(w, r, agentType, sessionID, body, mode, *placement.Peer, ranking)
		if out.Served {
			return true
		}
		if out.clientGone() {
			clientGone = true
			return false
		}
		next, err := ranking.recompute()
		if err != nil {
			return false
		}
		placement = next
	}
}

// attemptCandidate makes one attempt on a ranked peer candidate and books a
// failure against the turn and, for a refusal, against the peer's view.
func (s Server) attemptCandidate(w http.ResponseWriter, r *http.Request, agentType, sessionID string, body []byte, mode remotePoolHoldMode, candidate crossPoolCandidate, ranking *crossPoolRanking) peerForwardOutcome {
	target := candidate.Account
	out := s.serveOnePeerAttempt(w, withPeerStart(r, s, candidate, agentType, sessionID), candidate.Peer, agentType, sessionID, body, mode, target)
	if out.Served || out.clientGone() {
		return out
	}
	s.bookFailedAttempt(candidate.Peer, target, candidate.BodyAt, out, ranking.turn, ranking.provider, agentType, sessionID, false)
	return out
}

// bookFailedAttempt records a failed attempt on the turn and, for a refusal,
// marks the account on the peer until its next usage body, and logs it.
func (s Server) bookFailedAttempt(p *RemotePool, target string, bodyAt time.Time, out peerForwardOutcome, turn *remotePoolTurn, provider accounts.Provider, agentType, sessionID string, continuing bool) {
	turn.recordFailure(p.name(), target, out)
	refused := target != "" && (out.Refusal.Reason != "" || out.Switch.Kind == switchForced)
	if refused {
		p.markRefused(string(provider), target, bodyAt)
	}
	if s.Logger != nil && refused {
		attempts := 0
		if turn != nil {
			turn.mu.Lock()
			attempts = turn.attempts
			turn.mu.Unlock()
		}
		reason := out.Switch.Reason
		if out.Refusal.Reason != "" {
			reason = out.Refusal.Reason
			continuing = out.Refusal.Continuing
		}
		s.Logger.Warn("remote pool target refused",
			"remote", p.name(), "agent", agentType, "session", sessionID,
			"target_account", target, "reason", reason, "switch", out.Switch.Kind.String(),
			"continuing", continuing, "status", out.Status, "attempts", attempts)
	}
}

// crossPoolReason says why the placement landed where it did, in the terms the
// old decision line used so existing log readers keep working -- and, wherever
// a peer competed, which comparator clause decided and over whom (F4): "local
// still outranks every eligible peer account" alone could not say whether the
// weekly pace, the burndown tier or a tie decided it, or against which account.
func crossPoolReason(placement crossPoolPlacement, ranking *crossPoolRanking) string {
	decided := ""
	if placement.Clause != "" {
		decided = " (" + placement.Clause
		if placement.RunnerUp != "" {
			decided += "; runner-up " + placement.RunnerUp
		}
		decided += ")"
	}
	switch {
	case placement.Peer != nil:
		return "peer outranks local on the shared comparator, after the " +
			strconv.FormatFloat(selectacct.LocalPreferenceRatio, 'g', -1, 64) + "x local preference" + decided
	case !placement.HasLocal:
		return "no peer account is eligible"
	case ranking != nil && !ranking.measured && ranking.considered > 0:
		return "peer could not be measured"
	case placement.BestPeer == nil && ranking != nil && ranking.legacyDropped:
		return crossPoolReasonLegacyNotAsked
	case placement.BestPeer == nil:
		return "no peer account is eligible; staying local"
	default:
		return "local still outranks every eligible peer account" + decided
	}
}

// crossPoolDecisionLevel is the level of a `remote pool decision` line. A turn
// selection KEPT on its account without ranking it -- warm, or a cold session
// its provider retains -- is Debug: nothing competed and nothing moved, the
// line only restates that, and it is written on every turn of every live
// session. In the laptop's last 10 hours before §39 those lines were 99% of the
// decision lines and 55% of subrouter.err.log's bytes (PATCHES §39). Every line
// a ranking produced stays Info: a peer won, local won against a peer (clause
// and runner-up say why), a recompute after a refusal, local unable to serve.
// So does a turn never ranked without a kept reason (crossPoolNotRanked): a
// ranking that failed and was invalidated, which nothing else logs.
func crossPoolDecisionLevel(ranking *crossPoolRanking, placement crossPoolPlacement, recompute bool) slog.Level {
	if ranking != nil && ranking.kept != "" && !ranking.rankedAlready() &&
		placement.Local && placement.Peer == nil && placement.BestPeer == nil && !recompute {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// logCrossPoolDecision is the `remote pool decision` line (D6). It keeps every
// key the pre-§31 line had -- the peer_* keys now describe the winning (or
// best) peer candidate -- and names the target.
func (s Server) logCrossPoolDecision(r *http.Request, agentType, sessionID string, local crossPoolLocal, ranking *crossPoolRanking, placement crossPoolPlacement, reason string, recompute bool) {
	if s.Logger == nil {
		return
	}
	level := crossPoolDecisionLevel(ranking, placement, recompute)
	if !s.Logger.Enabled(context.Background(), level) {
		return
	}
	peer := placement.Peer
	if peer == nil {
		peer = placement.BestPeer
	}
	peerHeadroom, peerPace, peerKnown, peerCarried := 0.0, 0.0, false, false
	if peer != nil {
		peerHeadroom, peerKnown, peerCarried = peer.Score.Headroom, true, peer.Carried
		if pace, ok := peer.Score.WeeklyPace(time.Now()); ok {
			peerPace = pace
		}
	}
	targetPeer, targetAccount, targetReason, targetLegacy := "", "", "", false
	if placement.Peer != nil {
		targetPeer, targetAccount = placement.Peer.Peer.name(), placement.Peer.Account
		targetReason, targetLegacy = placement.Peer.Reason, placement.Peer.Legacy
	}
	refusedPeer, refusedAccount, refusedReason := "", "", ""
	state := ""
	candidates, peers, excluded := 0, 0, 0
	if ranking != nil {
		state = ranking.sessionState
		candidates, peers, excluded = ranking.candidates, ranking.considered, ranking.excluded
		if recompute && ranking.turn != nil {
			ranking.turn.mu.Lock()
			refusedPeer, refusedAccount, refusedReason = ranking.turn.lastRefused.Peer, ranking.turn.lastRefused.Account, ranking.turn.lastReason
			ranking.turn.mu.Unlock()
		}
	}
	// Why a request did or did not leave for a peer is otherwise invisible,
	// and the answer turns on values computed three call frames away and one
	// network hop out.
	s.Logger.Log(context.Background(), level, "remote pool decision",
		"agent", agentType, "session", sessionID,
		"account", local.Account,
		"local_cause", local.Cause,
		"local_failed_over_from", local.FailedOverFrom,
		"local_headroom", local.Capacity.Headroom,
		"local_pace", local.Capacity.Pace,
		"local_endgame", local.Capacity.Endgame,
		"local_known", local.Capacity.Known,
		"below_floor", local.Capacity.BelowFloor,
		"peer_headroom", peerHeadroom,
		"peer_pace", peerPace,
		"peer_known", peerKnown,
		"peer_carried", peerCarried,
		"prefer_peer", placement.Peer != nil,
		"reason", reason,
		"clause", placement.Clause,
		"runner_up", placement.RunnerUp,
		"session_state", state,
		"target_peer", targetPeer,
		"target_account", targetAccount,
		"target_reason", targetReason,
		"target_legacy", targetLegacy,
		"candidates", candidates,
		"peers", peers,
		"excluded", excluded,
		"recompute", recompute,
		"refused_peer", refusedPeer,
		"refused_account", refusedAccount,
		"refused_reason", refusedReason)
}

// --- local cannot serve: the no-account chain, the transient one-turn serve --

// serveAcrossPeers serves a turn LOCAL CANNOT SERVE from the best eligible peer
// account (the no-account chain, the transient one-turn serve): the same
// ranking with no local candidate, the same refusal loop and bound. When no
// peer account is eligible and the bound is not reached, the last resort is
// today's untargeted forward to the first usable peer (Deviation X2): it
// replaces a 503 the client would otherwise get, and is logged as such.
//
// A side request places nothing and is forwarded untargeted, as before.
//
// local is what the caller knows of the local side: on the transient one-turn
// serve, the account whose refresh reached no verdict, as measured, the
// refresh error, and the account it failed over from if it did; on the
// no-account chain, only the cause. It is logged on the decision line and
// decides nothing.
func (s Server) serveAcrossPeers(w http.ResponseWriter, r *http.Request, provider accounts.Provider, poolModel, agentType, sessionID string, mode remotePoolHoldMode, turn *remotePoolTurn, local crossPoolLocal) bool {
	if !s.peerForwardingAllowed(r) {
		return false
	}
	body, ok := replayableForwardBody(r)
	if !ok {
		return false
	}
	if mode == remotePoolHoldReadOnly {
		p := s.firstUsablePeer(r, turn)
		return p != nil && s.serveOnePeerAttempt(w, r, p, agentType, sessionID, body, mode, "").Served
	}
	ranking := s.newCrossPoolRanking(r, provider, poolModel, turn)
	placement, err := ranking.rankPeersOnly()
	for recompute := turn.failedBefore(); ; recompute = true {
		if turn.boundReached() {
			s.logCrossPoolDecision(r, agentType, sessionID, local, ranking, crossPoolPlacement{},
				"refusal bound reached with local unable to serve", recompute)
			return false
		}
		if err != nil || placement.Peer == nil {
			return s.serveLastResort(w, r, agentType, sessionID, body, mode, turn, ranking, recompute, local)
		}
		s.logCrossPoolDecision(r, agentType, sessionID, local, ranking, placement, "local cannot serve; best eligible peer account", recompute)
		out := s.attemptCandidate(w, r, agentType, sessionID, body, mode, *placement.Peer, ranking)
		if out.Served {
			return true
		}
		if out.clientGone() {
			return false
		}
		placement, err = ranking.rankPeersOnly()
	}
}

// serveLastResort is Deviation X2: nothing is eligible anywhere and local
// cannot serve, so forward untargeted to the first usable peer, which judges
// the turn by its own (pre-§31) rules. A peer whose dial fails sent nothing,
// so the next usable peer is asked; one that answered, or may be running the
// turn, ends it.
func (s Server) serveLastResort(w http.ResponseWriter, r *http.Request, agentType, sessionID string, body []byte, mode remotePoolHoldMode, turn *remotePoolTurn, ranking *crossPoolRanking, recompute bool, local crossPoolLocal) bool {
	p := s.firstUsablePeer(r, turn)
	if p == nil {
		return false
	}
	s.logCrossPoolDecision(r, agentType, sessionID, local, ranking, crossPoolPlacement{}, "no eligible peer account and local cannot serve", recompute)
	for ; p != nil; p = s.firstUsablePeer(r, turn) {
		if s.Logger != nil {
			s.Logger.Warn("remote pool last resort; forwarding untargeted",
				"remote", p.name(), "agent", agentType, "session", sessionID)
		}
		out := s.serveOnePeerAttempt(w, r, p, agentType, sessionID, body, mode, "")
		if out.Served || out.clientGone() {
			return out.Served
		}
		turn.recordFailure(p.name(), "", out)
		if !out.NeverSent {
			return false
		}
	}
	return false
}

// firstUsablePeer is the first configured peer this caller may use and this
// turn has not found down.
func (s Server) firstUsablePeer(r *http.Request, turn *remotePoolTurn) *RemotePool {
	for _, p := range s.peers() {
		if !turn.peerDown(p.name()) && turn.peerUsable(s, p, r) {
			return p
		}
	}
	return nil
}

// peersUnreachable reports a turn no peer could be asked about: this caller
// may use at least one peer, no peer answered the turn or may be running it,
// and every peer it may use either failed before the request left or was
// skipped for its open breaker.
func (s Server) peersUnreachable(r *http.Request, turn *remotePoolTurn) bool {
	if turn == nil || !s.peerForwardingAllowed(r) {
		return false
	}
	turn.mu.Lock()
	defer turn.mu.Unlock()
	if turn.peerReached {
		return false
	}
	now := time.Now()
	allowed := false
	for _, p := range s.peers() {
		if !p.allows(clientRemoteIP(r)) {
			continue
		}
		// The turn booked each peer it skipped for its breaker. One it
		// never looked at, say a peer that may be running the request, is
		// read as its breaker stands.
		if !turn.unsentPeers[p.name()] && p.breaker.allow(now) {
			return false
		}
		allowed = true
	}
	return allowed
}

// remotePoolStageAcrossPeers is the transport-level stage (local failover
// burned through every account) through the same ranking and loop, returning
// the peer's response instead of writing it. Local cannot serve, so the peers
// are ranked alone and, when none is eligible, the untargeted last resort
// (Deviation X2) replaces the give-up the client would otherwise get.
func (s Server) remotePoolStageAcrossPeers(r *http.Request, provider accounts.Provider, poolModel, agentType, sessionID, clientPath, rawQuery, reason string, turn *remotePoolTurn) (*http.Response, bool) {
	ranking := s.newCrossPoolRanking(r, provider, poolModel, turn)
	return s.transportStageAcrossPools(r, ranking, ranking.rankPeersOnly, nil, crossPoolLocal{}, agentType, sessionID, clientPath, rawQuery, reason)
}

// forcedSwitchAcrossPools is owner rule P8 inside local failover: the account
// this turn was on was FORCED off it (a spent window, a dead credential), so
// where the turn goes is the ONE ranking over next -- local failover's
// candidate, scored by the turn's scheduler exactly as selection scores it
// (live debits, reserves) -- and every peer account. next stays in every
// recompute after a refusal (P6: any "nope" recomputes overall), so a refused
// peer account hands the turn back to next rather than to a lower peer
// account; and since local CAN serve, the untargeted last resort -- which is
// for a turn local cannot serve (Deviation X2) -- is never taken: an
// untargeted forward lets the peer place the session by its own rules, past
// the burndown cap (the 9/27 incident). false means local failover continues
// on next.
func (s Server) forcedSwitchAcrossPools(r *http.Request, ranking *crossPoolRanking, scheduler selectacct.Scheduler, next accounts.Account, agentType, sessionID, clientPath, rawQuery, reason string) (*http.Response, bool) {
	if ranking == nil {
		return nil, false
	}
	first := func() (crossPoolPlacement, error) {
		return ranking.forcedRerank(scheduler, next)
	}
	// The decision line reports next exactly as selection measures it: the
	// scheduler is the one the ranking scored it on, so local_known says what
	// the ranking knew rather than a zero value's false.
	local := crossPoolLocal{Account: next.ID, Capacity: s.routingCapacityFor(s.scheduler(), scheduler, next)}
	return s.transportStageAcrossPools(r, ranking, first, &next, local, agentType, sessionID, clientPath, rawQuery, reason)
}

// transportStageAcrossPools is the refusal loop the two transport-level entry
// points share. first is the entry's ranking. next is local failover's
// candidate on a forced switch: it is in every recompute, and whenever it
// wins -- or nothing on a peer is eligible, or the bound is reached -- the
// answer is false and local failover serves the turn on it. With next nil
// (local failover gave up) the peers are re-ranked alone and the last resort
// applies.
func (s Server) transportStageAcrossPools(r *http.Request, ranking *crossPoolRanking, first func() (crossPoolPlacement, error), next *accounts.Account, local crossPoolLocal, agentType, sessionID, clientPath, rawQuery, reason string) (*http.Response, bool) {
	if r == nil || r.GetBody == nil || !s.peerForwardingAllowed(r) {
		return nil, false
	}
	rc, err := r.GetBody()
	if err != nil {
		return nil, false
	}
	body, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return nil, false
	}
	turn := ranking.turn
	send := func(p *RemotePool, req *http.Request, target string) peerForwardOutcome {
		return s.forwardToPeer(p, req, agentType, sessionID, clientPath, rawQuery, body, remotePoolHoldSet, target)
	}
	served := func(p *RemotePool, out peerForwardOutcome) (*http.Response, bool) {
		if s.Logger != nil {
			s.Logger.Warn("local pool exhausted; served from remote pool",
				"remote", p.name(), "agent", agentType, "session", sessionID,
				"reason", reason, "status", out.Resp.StatusCode, "target_account", out.ServedAccount)
		}
		return out.Resp, true
	}
	placement, rankErr := first()
	for recompute := turn.failedBefore(); ; recompute = true {
		if next != nil && (rankErr != nil || placement.Local || placement.Peer == nil) {
			// next outranks every eligible peer account left: local failover
			// continues on it. Logged only once a peer attempt failed -- the
			// recompute's answer (D6); a forced switch no peer outranks is
			// local failover as before §31.
			if recompute && rankErr == nil {
				s.logCrossPoolDecision(r, agentType, sessionID, local, ranking, placement, crossPoolReason(placement, ranking), recompute)
			}
			return nil, false
		}
		if turn.boundReached() {
			if next != nil {
				s.logCrossPoolDecision(r, agentType, sessionID, local, ranking, crossPoolPlacement{Local: true, BestPeer: placement.Peer},
					"refusal bound reached ("+strconv.Itoa(remotePoolMaxTargetRefusals)+" failed peer attempts); local failover continues", recompute)
			}
			return nil, false
		}
		if rankErr != nil || placement.Peer == nil {
			p := s.firstUsablePeer(r, turn)
			if p == nil {
				return nil, false
			}
			if s.Logger != nil {
				s.Logger.Warn("remote pool last resort; forwarding untargeted",
					"remote", p.name(), "agent", agentType, "session", sessionID, "stage", "transport")
			}
			out := send(p, r, "")
			if !out.Served {
				turn.recordFailure(p.name(), "", out)
				return nil, false
			}
			return served(p, out)
		}
		why := "local failover exhausted; best eligible peer account"
		if next != nil {
			why = crossPoolReason(placement, ranking)
		}
		s.logCrossPoolDecision(r, agentType, sessionID, local, ranking, placement, why, recompute)
		candidate := *placement.Peer
		out := send(candidate.Peer, withPeerStart(r, s, candidate, agentType, sessionID), candidate.Account)
		if out.Served {
			return served(candidate.Peer, out)
		}
		if out.clientGone() {
			return nil, false
		}
		s.bookFailedAttempt(candidate.Peer, candidate.Account, candidate.BodyAt, out, turn, ranking.provider, agentType, sessionID, false)
		if next != nil {
			placement, rankErr = ranking.recompute()
		} else {
			placement, rankErr = ranking.rankPeersOnly()
		}
	}
}

// --- a session held on a peer (D5, P4) ----------------------------------------

// serveHeldTurn serves a turn of a session held on a peer, TARGETED at the
// account the hold names: a session sent to a peer is tied to the ACCOUNT it
// was sent for, not to the peer (P4).
//
// It ends the hold -- and hands the turn to a re-rank across every pool, with
// the local placeholder pin treated as cold (turn.forcedSwitch) -- only on a
// FORCED switch: the cached verdict says the account is evicted (a spent window,
// the holdout) or its credential is dead or it is gone, or the peer refuses the
// target, or declines. A warm session below the new-session floor or at the
// endgame cap is NOT moved: the continuing-session criterion is eviction
// (PATCHES §20). A TRANSIENT failure keeps the hold. A model turn the peer
// cannot take -- the breaker is open, the dial failed, or the peer may be
// running it already -- waits for its account until the peer is found down,
// and is then placed as a refugee, or the chat gives up (noteStrandedAttempt)
// (strandedOnPeer, PATCHES §67); one the peer
// answered but could not serve is served locally this once (turn.stayLocal).
// A model turn of a chat another turn is placing as a refugee, with no hold
// or only that placement's pending claim, waits for it (waitForPlacement).
//
// A hold with no account (set by a peer too old to name it) or on a peer that
// publishes no verdicts is forwarded untargeted, exactly as before. With no
// usage body at all (a restart), a hold the peer served targeted stays
// targeted (honouredHoldTarget, PATCHES §54).
//
// A subagent's Fable turn on a held account whose Fable pool is spent but
// whose Opus pool serves is served there as Opus, and the hold stays
// (serveHeldAttempt, claude_subagent_fable.go).
func (s Server) serveHeldTurn(w http.ResponseWriter, r *http.Request, provider accounts.Provider, poolModel, agentType, sessionID string, mode remotePoolHoldMode, turn *remotePoolTurn) bool {
	key := session.ScopedSessionKey(agentType, sessionID)
	p, hold, ok := s.peerHold(key)
	if from, placing := s.Hold.placingFrom(key); placing && mode == remotePoolHoldSet && (!ok || hold.claim != 0) {
		waitForPlacement(w, r, agentType, sessionID, from)
		return true
	}
	if !ok {
		return false
	}
	if !s.peerForwardingAllowed(r) || !s.peerOpenTo(p, r) {
		// Not this caller's to spend: this turn is served locally and the hold
		// is left as it is.
		return false
	}
	if turn.mayRun.has(p.name()) {
		// An earlier hold attempt sent this turn to the peer, which may be
		// running it: it is never sent again (PATCHES §58). It still waits for
		// the peer, since the client's resend is a new request the peer can
		// take (PATCHES §67).
		return s.strandedOnPeer(w, r, provider, poolModel, p, hold, mode, agentType, sessionID, "peer_may_run", turn, true)
	}
	if !p.breaker.allow(time.Now()) {
		return s.strandedOnPeer(w, r, provider, poolModel, p, hold, mode, agentType, sessionID, "peer_breaker_open", turn, false)
	}
	// A held turn is forwarded, not ranked, so a stale body is good enough
	// (stale but real) and only starts a background refresh, for the next
	// turn's pre-check. With NO body at all -- a restart that loaded holds from
	// disk -- a model turn waits the decision budget for the first one, as a
	// decision would (Deviation X6): without it the peer cannot be known to
	// honour targets and the held turn would go untargeted, free to land on
	// an account the session was never sent for, unless the hold itself says
	// the peer honoured its target (honouredHoldTarget). The wait stays: the
	// body still feeds the next turn's pre-check. A side request only follows
	// the hold and costs the peer nothing extra.
	snapshot := p.cachedUsage()
	if mode == remotePoolHoldSet {
		deadline := time.Time{}
		if len(snapshot.Body) == 0 {
			deadline = time.Now().Add(remotePoolDecisionFetchBudget)
		}
		snapshot = p.usageForDecision(deadline)
	}
	target := hold.Account
	if target != "" && !p.publishesVerdicts(snapshot, string(provider)) && !p.honouredHoldTarget(snapshot, hold) {
		target = ""
	}
	if target != "" && mode == remotePoolHoldSet {
		if sw, bodyAt, ended := p.cachedHoldVerdict(snapshot, string(provider), target); ended {
			p.delegations.clear(key)
			turn.mu.Lock()
			turn.forcedSwitch = true
			failedKey := remotePoolTargetKey{p.name(), target}
			turn.failed[failedKey] = sw.Reason
			turn.lastRefused, turn.lastReason = failedKey, sw.Reason
			turn.mu.Unlock()
			p.markRefused(string(provider), target, bodyAt)
			if s.Logger != nil {
				s.Logger.Warn("remote pool hold ended",
					"remote", p.name(), "agent", agentType, "session", sessionID,
					"account", target, "reason", sw.Reason, "kind", sw.Kind.String(), "source", "cached_verdict")
			}
			return false
		}
	}
	body, ok := replayableForwardBody(r)
	if !ok {
		return false
	}
	out := s.serveHeldAttempt(w, r, p, agentType, sessionID, body, mode, target, snapshot, turn)
	switch {
	case out.Served:
		return true
	case out.clientGone():
		return false
	case mode == remotePoolHoldReadOnly:
		// A side request follows the hold and never moves it.
		return false
	case out.Switch.Kind == switchForced && !out.Unreachable:
		// forwardToPeer has already ended the hold, if it still named the
		// target.
		turn.mu.Lock()
		turn.forcedSwitch = true
		turn.mu.Unlock()
		s.bookFailedAttempt(p, target, snapshot.At, out, turn, provider, agentType, sessionID, true)
		if s.Logger != nil {
			s.Logger.Warn("remote pool hold ended",
				"remote", p.name(), "agent", agentType, "session", sessionID,
				"account", target, "reason", out.Switch.Reason, "kind", out.Switch.Kind.String(), "source", "peer_answer")
		}
		return false
	default:
		// Transient, or the peer found down by this forward: the hold stays,
		// and no other peer is asked for the session's account (it is not a
		// switch of account). A request that left and got no answer may be
		// running there and is never sent again (PATCHES §58).
		turn.mu.Lock()
		turn.noteReachLocked(p.name(), out)
		turn.mu.Unlock()
		if out.Unreachable {
			why := "peer_unreachable"
			if !out.NeverSent {
				why = "peer_may_run"
			}
			return s.strandedOnPeer(w, r, provider, poolModel, p, hold, mode, agentType, sessionID, why, turn, true)
		}
		turn.mu.Lock()
		turn.stayLocal = true
		turn.mu.Unlock()
		return false
	}
}

// strandedOnPeer is a held turn its peer cannot take now: the breaker is
// open, the dial failed, or the peer may be running it. A model turn of a
// session the peer served is a stranded chat (PATCHES §67): it is held, and
// the hold (PATCHES §58) runs it again there instead of moving it to a cold
// cache, across the client's retries, until the peer answers or is found
// down, or the chat gives up (noteStrandedAttempt). Found down, it is a refugee
// (placeRefugee). A Codex websocket upgrade is
// never held, but its HTTP fallback is: Codex 0.170.0 retries a 502 upgrade
// for about 7 s, then falls back to HTTPS (measured), so it is answered 502.
// A side request, a session the peer has only been claimed for, or a turn no
// hold carries (holding off, or a request the hold cannot carry) moves:
// ranked across every pool, or,
// with keepLocal, served locally this once with the hold kept. It reports
// whether it answered.
func (s Server) strandedOnPeer(w http.ResponseWriter, r *http.Request, provider accounts.Provider, poolModel string, p *RemotePool, hold remotePoolDelegation, mode remotePoolHoldMode, agentType, sessionID, why string, turn *remotePoolTurn, keepLocal bool) bool {
	stranded := mode == remotePoolHoldSet && hold.claim == 0
	if stranded && p.breaker.foundDown() {
		return s.placeRefugee(w, r, provider, poolModel, agentType, sessionID, p, hold, turn)
	}
	carried := heldByAHold(r) || (websocket.IsWebSocketUpgrade(r) && s.Hold.enabled())
	if !stranded || !carried {
		if keepLocal {
			turn.mu.Lock()
			turn.stayLocal = true
			turn.mu.Unlock()
		}
		return false
	}
	holdStranded(w, r, waitsForItsCache, agentType, sessionID, p.name(), hold.Account, "warm_wait:"+why, "peer "+p.name()+" could not be reached")
	return true
}

// honouredHoldTarget reports a hold that may stay targeted with NO usage body
// at all: the peer served this exact account when targeted at it (the hold's
// Targeted bit, persisted), and has not been seen ignoring a target since. A
// restart loses the body, and a peer whose first usage fetch after it misses
// the decision budget -- or fails, with the network still coming up -- would
// otherwise get the held turn untargeted, free to place it on any account
// (PATCHES §54). A body that carries no verdicts is a legacy peer, and still
// blanks the target; a peer that has since stopped honouring targets is caught
// by the substitution rule and marked ignoring.
func (p *RemotePool) honouredHoldTarget(snapshot AdminSnapshot, hold remotePoolDelegation) bool {
	return len(snapshot.Body) == 0 && hold.Targeted && !p.ignoresTargets(snapshot.At)
}

// publishesVerdicts reports whether this peer's usage body (for provider)
// carries new-session verdicts and the peer has not been seen ignoring a
// target since: only then is it sent one (Deviation X1).
func (p *RemotePool) publishesVerdicts(snapshot AdminSnapshot, provider string) bool {
	if len(snapshot.Body) == 0 || p.ignoresTargets(snapshot.At) {
		return false
	}
	var rows []remotePoolAccountUsage
	if json.Unmarshal(snapshot.Body, &rows) != nil {
		return false
	}
	for _, row := range rows {
		if row.Provider == provider && row.Normalized.NewSession != nil {
			return true
		}
	}
	return false
}

// cachedHoldVerdict reads the continuing-session criterion for a held account
// off the cached usage body, when that body is recent enough to trust
// (remotePoolHoldVerdictMaxAge) and measured the account: evicted by its own
// verdict, a checked dead credential, or absent from a body that measured the
// provider -> a forced end. Anything short of that -- below the new-session
// floor, at the endgame cap, unmeasured, a stale body -- keeps the hold.
func (p *RemotePool) cachedHoldVerdict(snapshot AdminSnapshot, provider, account string) (accountSwitch, time.Time, bool) {
	if len(snapshot.Body) == 0 || snapshot.At.IsZero() || time.Since(snapshot.At) > remotePoolHoldVerdictMaxAge {
		return accountSwitch{}, time.Time{}, false
	}
	var rows []remotePoolAccountUsage
	if json.Unmarshal(snapshot.Body, &rows) != nil {
		return accountSwitch{}, time.Time{}, false
	}
	measuredProvider := false
	for _, row := range rows {
		if row.Provider != provider {
			continue
		}
		if !row.unmeasured() {
			measuredProvider = true
		}
		if row.ID != account {
			continue
		}
		if row.unmeasured() {
			return accountSwitch{}, time.Time{}, false
		}
		evidence := accountSwitchEvidence{CredentialFailure: row.knownInvalid()}
		if v := row.Normalized.NewSession; v != nil && v.Evicted {
			evidence.Evicted = v.Reason
			if evidence.Evicted == "" || v.Reason == selectacct.VerdictEligible || v.Reason == selectacct.VerdictBurndown {
				evidence.Evicted = selectacct.VerdictExhausted
			}
		}
		sw := classifyAccountSwitch(evidence)
		return sw, snapshot.At, sw.Kind == switchForced
	}
	if measuredProvider {
		sw := classifyAccountSwitch(accountSwitchEvidence{AccountGone: true})
		return sw, snapshot.At, true
	}
	return accountSwitch{}, time.Time{}, false
}
