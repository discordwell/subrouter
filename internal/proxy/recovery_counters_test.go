package proxy

import (
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
)

func TestRecoveryCounterStoreKeepsOneHourPerProvider(t *testing.T) {
	store := &recoveryCounterStore{}
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	store.add(accounts.ProviderClaude, "held", now.Add(-59*time.Minute))
	store.add(accounts.ProviderClaude, "persistent", now.Add(-61*time.Minute))
	store.add(accounts.ProviderCodex, "handoff_503", now)
	store.add(accounts.ProviderCodex, "exhausted", now)
	got := store.snapshot(now)
	if got["claude"].RetriesHeld != 1 || got["claude"].PersistentRetries != 0 {
		t.Fatalf("claude counters = %+v", got["claude"])
	}
	if got["codex"].Retryable503Handoffs != 1 || got["codex"].Exhausted != 1 {
		t.Fatalf("codex counters = %+v", got["codex"])
	}
}

// Only the providers that feed the counters in this fork are listed when idle:
// Codex. A Claude row would always read 0 (PATCHES §27), so it appears only
// once something records a Claude event.
func TestRecoveryCounterStoreListsNoIdleClaudeRow(t *testing.T) {
	got := (&recoveryCounterStore{}).snapshot(time.Now())
	if _, ok := got[string(accounts.ProviderClaude)]; ok {
		t.Fatalf("snapshot = %+v, want no idle Claude row", got)
	}
	if _, ok := got[string(accounts.ProviderCodex)]; !ok {
		t.Fatalf("snapshot = %+v, want the Codex row", got)
	}
}
