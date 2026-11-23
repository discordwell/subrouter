package proxy

import (
	"cmp"
	"slices"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

// The board's view of every saved reset (PATCHES §91.2). A pool.json row
// carries its account's resets list in a compact form, for this daemon's rows
// and a peer's alike, so one board shows every reset in the pool. A row whose
// daemon publishes no list (a build before §91, or a block restored from the
// persisted usage snapshot) carries what its complimentary_reset says, marked
// partial. It is shown, never ranked on.

// poolReset is one entry of a pool.json row's resets: what the account holds
// of one kind ending at one time.
type poolReset struct {
	Kind      accounts.ResetKind   `json:"kind"`
	Count     int                  `json:"count"`
	ExpiresAt string               `json:"expires_at,omitempty"`
	Expiry    accounts.ResetExpiry `json:"expiry"`
}

// poolResetsOf is a row's resets as its daemon published them or, when that
// daemon published no list, what its complimentary_reset says, with partial
// set when that holds any. Nil when the row has no reading.
func poolResetsOf(status AccountUsageStatus) (resets []poolReset, partial bool) {
	if status.Resets == nil {
		resets = resetsFromComplimentary(status.Provider, status.ComplimentaryReset)
		return resets, len(resets) > 0
	}
	resets = make([]poolReset, 0, len(status.Resets))
	for _, entry := range status.Resets {
		resets = append(resets, poolReset{Kind: entry.Kind, Count: entry.Count, ExpiresAt: entry.ExpiresAt, Expiry: entry.Expiry})
	}
	return resets, false
}

// resetsFromComplimentary is what a complimentary_reset block says of the
// resets an account holds. It guesses nothing. A Codex count is the kind its
// key names, with its expiry not read. A Claude kind is one remaining_by_kind
// names (§64), and a date is given only where the block ties it to that kind:
// week_held_until to the week reset, expires_at to the only kind left. A
// Claude block from before §64 names no kinds, so its count is one entry of
// kind unknown with the soonest date.
func resetsFromComplimentary(provider accounts.Provider, info *accounts.ComplimentaryResetInfo) []poolReset {
	if info == nil || !info.Known || info.Remaining == nil {
		return nil
	}
	if *info.Remaining <= 0 {
		return []poolReset{}
	}
	if info.Source != agentclaude.ResetProgram {
		if provider != accounts.ProviderCodex {
			return nil
		}
		kind := accounts.ResetKindUnknown
		if spec, ok := accounts.ClassifyReset(accounts.ResetReading{Provider: provider, Source: info.Source}); ok {
			kind = spec.Kind
		}
		return []poolReset{{Kind: kind, Count: *info.Remaining, Expiry: accounts.ResetExpiryNotRead}}
	}
	live := 0
	for _, count := range info.RemainingByKind {
		if count > 0 {
			live++
		}
	}
	if live == 0 {
		return []poolReset{datedPoolReset(accounts.ResetKindUnknown, *info.Remaining, info.ExpiresAt)}
	}
	order := map[accounts.ResetKind]int{}
	for _, spec := range accounts.ResetKinds() {
		if spec.Provider == accounts.ProviderClaude {
			order[spec.Kind] = spec.UseOrder
		}
	}
	rank := func(kind accounts.ResetKind) int {
		if place, ok := order[kind]; ok {
			return place
		}
		return len(order) + 1
	}
	out := make([]poolReset, 0, live)
	for name, count := range info.RemainingByKind {
		if count <= 0 {
			continue
		}
		kind := accounts.ResetKind(name)
		if name == string(agentclaude.ResetKindOther) {
			kind = accounts.ResetKindUnknown
		}
		expiresAt := ""
		switch {
		case kind == accounts.ResetKindClaudeWeek && info.WeekHeldUntil != "":
			expiresAt = info.WeekHeldUntil
		case live == 1:
			expiresAt = info.ExpiresAt
		}
		out = append(out, datedPoolReset(kind, count, expiresAt))
	}
	slices.SortFunc(out, func(a, b poolReset) int {
		return cmp.Or(cmp.Compare(rank(a.Kind), rank(b.Kind)), cmp.Compare(a.Kind, b.Kind))
	})
	return out
}

func datedPoolReset(kind accounts.ResetKind, count int, expiresAt string) poolReset {
	entry := poolReset{Kind: kind, Count: count, ExpiresAt: expiresAt, Expiry: accounts.ResetExpiryRead}
	if expiresAt == "" {
		entry.Expiry = accounts.ResetExpiryNoneGiven
	}
	return entry
}
