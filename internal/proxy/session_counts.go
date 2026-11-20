package proxy

import (
	"github.com/manaflow-ai/subrouter/account"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// SchedulerSessionCounts returns persistent assignments keyed the same way as
// scheduler scores. Account labels are not globally unique: a Codex account
// and a Claude profile can legitimately share an email, so bare-ID counts
// would make one provider's sessions damp placement for another. Agent types
// such as pi still map to Codex because they use the Codex credential pool.
func SchedulerSessionCounts(store *session.Store) map[string]int {
	counts := map[string]int{}
	if store == nil {
		return counts
	}
	for _, assignment := range store.All() {
		provider := providerForStoredSession(assignment.AgentType)
		counts[selectacct.ScoreKey(provider, assignment.AccountID)]++
	}
	return counts
}

// SchedulerActiveSessionCounts is SchedulerSessionCounts restricted to the
// sessions with in-flight work -- the scheduler's LIVE concurrency -- and keyed
// the same provider-scoped way, so a running Claude session never damps a Codex
// account that happens to share its email. Without an ActiveSessions tracker it
// counts every assignment, as activeSessionCountsByAccount does.
func SchedulerActiveSessionCounts(store *session.Store, active *ActiveSessions) map[string]int {
	if active == nil {
		return SchedulerSessionCounts(store)
	}
	counts := map[string]int{}
	if store == nil {
		return counts
	}
	running := active.ActiveKeys()
	for _, assignment := range store.All() {
		if !running[session.ScopedSessionKey(assignment.AgentType, assignment.SessionID)] {
			continue
		}
		provider := providerForStoredSession(assignment.AgentType)
		counts[selectacct.ScoreKey(provider, assignment.AccountID)]++
	}
	return counts
}

func providerForStoredSession(agentType string) account.Provider {
	normalized := session.NormalizeAgentType(agentType)
	if entry, ok := keyedProviderForName(normalized); ok {
		return accountProviderFor(entry.Provider)
	}
	if normalized == string(account.ProviderAntigravity) {
		return account.ProviderAntigravity
	}
	return providerForAgent(normalized)
}
