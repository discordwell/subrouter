package proxy

import (
	"log/slog"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// providerSelectionPolicy states, per provider, WHEN a COLD session leaves the
// account that holds its upstream prompt cache -- never WHERE it goes. Where
// is the one cross-pool ranking (placeAcrossPools, PATCHES §31 D7); this table
// only decides whether a session reaches it. Modelled on upstream's table of
// the same name (internal/proxy/proxy.go, providerSelectionPolicy), and
// encoding exactly the rules reuseStickyAssignment applied before it existed.
//
// What no table entry can change, because it precedes the table: a session
// whose account is EVICTED (a spent window, the owner's holdout) always
// leaves, and a WARM session -- a turn in flight, its last turn inside
// warmSessionIdleTTL, or unseen since a restart that recent -- always stays
// (the owner's rule, PATCHES §20/§21). A non-OAuth account keeps its sessions.
//
// Every knob defaults to false, which keeps a cold session on its account
// short of eviction: the safe choice for a provider whose cache and quota
// economics are unknown.
type providerSelectionPolicy struct {
	// retentionFloorGated: a cold session that is NOT stale stays while its
	// account is UsableForStickySession -- the new-session predicate on the
	// MEASURED score (upstream 1a1fcee, T1) -- and otherwise re-ranks. Only
	// Codex's shared keyless bucket reaches it (never warm, rarely idle a full
	// TTL); every conversation key past the warm check is stale and goes to
	// coldRerank. Codex (upstream #216).
	retentionFloorGated bool
	// coldRerank: a STALE session (idle a full warmSessionIdleTTL) re-ranks
	// outright, and moves if anything -- another local account or any peer
	// account -- now outranks its own (migrateIdleSession). Its upstream prompt
	// cache is gone by then, so a move costs nothing (PATCHES §20; owner rule
	// P9, which names no provider). Claude and, since the cp3 fix-up, Codex:
	// its warm rule (PATCHES §21) already stops the 15-second churn that the
	// retention floor was first kept for.
	coldRerank bool
	// transientFailoverKeepsPin: a turn that failed over for a TRANSIENT
	// reason -- a 429 that is not a spent window (classifyAccountSwitch) -- is
	// served on the replacement and the session KEEPS its account; the
	// replacement is not committed (usageLimitRetryTransport.
	// commitSuccessfulFailover, and for Codex the capacity layer above it,
	// codexOverloadFailoverTransport). Owner rule P8: forced moves the pin,
	// transient trouble does not. Claude, and Codex since the owner extended
	// P8 to it (2026-09-28, PATCHES §35): its headerless 429 keeps the pin,
	// while its capacity switch -- made only after the account shed the turn
	// twice in a row, §27's eviction threshold -- is forced and commits.
	transientFailoverKeepsPin bool
	// Upstream's capacity-eviction rule is in, for Codex, outside this table
	// (PATCHES §27: Scheduler.CapacityEvicting in reuseStickyAssignment, and
	// keepCapacityEvictedSession). Its constrainedMoveGated (a move only to a
	// materially better account: a retention floor on the destination plus a
	// +10pp headroom gain) is the one knob not taken; it waits on the owner.
}

var providerSelectionPolicies = map[accounts.Provider]providerSelectionPolicy{
	accounts.ProviderCodex:  {coldRerank: true, retentionFloorGated: true, transientFailoverKeepsPin: true},
	accounts.ProviderClaude: {coldRerank: true, transientFailoverKeepsPin: true},
}

func selectionPolicyFor(account accounts.Account) providerSelectionPolicy {
	return providerSelectionPolicies[accountProviderOrCodex(account)]
}

// transientFailoverKeepsPin is the pin decision after a failover served the
// turn, for both layers that commit one (usageLimitRetryTransport.
// commitSuccessfulFailover and codexOverloadFailoverTransport): the session
// keeps its account when it left it for a TRANSIENT reason and the provider's
// policy says so. A move already under way (pendingCommit: selection or the
// refresh left a spent pin) commits whatever the reason: that session was
// leaving its old account anyway (PATCHES §31, §35).
func transientFailoverKeepsPin(provider accounts.Provider, pendingCommit bool, leave accountSwitch) bool {
	return !pendingCommit && leave.Kind == switchTransient &&
		selectionPolicyFor(accounts.Account{Provider: provider}).transientFailoverKeepsPin
}

// logTransientFailoverKept is the one line a kept pin logs, whichever layer
// kept it, so counting it per session counts every kept pin.
func logTransientFailoverKept(logger *slog.Logger, agent, session, account, servedAccount string, leave accountSwitch) {
	if logger == nil {
		return
	}
	logger.Info("transient failover served this turn; the session keeps its account",
		"agent", agent, "session", session, "account", account,
		"served_account", servedAccount, "reason", leave.Reason)
}
