package proxy

import (
	"path/filepath"
	"testing"

	"github.com/manaflow-ai/subrouter/account"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func TestSchedulerSessionCountsScopesProvidersAndKeepsPiInCodex(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		agent, sessionID, accountID string
	}{
		{"codex", "codex-1", "same@example.com"},
		{"pi", "pi-1", "same@example.com"},
		{"claude", "claude-1", "same@example.com"},
		{"qwen-anthropic", "qwen-1", "team"},
	} {
		if _, err := store.Put(item.agent, item.sessionID, item.accountID, ""); err != nil {
			t.Fatal(err)
		}
	}
	counts := SchedulerSessionCounts(store)
	if got := counts[selectacct.ScoreKey(account.ProviderCodex, "same@example.com")]; got != 2 {
		t.Fatalf("Codex sessions = %d, want Codex plus Pi = 2", got)
	}
	if got := counts[selectacct.ScoreKey(account.ProviderClaude, "same@example.com")]; got != 1 {
		t.Fatalf("Claude sessions = %d, want 1", got)
	}
	if got := counts[selectacct.ScoreKey(account.ProviderQwenToken, "team")]; got != 1 {
		t.Fatalf("Qwen Token sessions = %d, want shared Anthropic endpoint = 1", got)
	}
}

// Live counts carry the same provider scoping, and only sessions with work in
// flight count. The idle Codex session proves the active filter is applied;
// the running Claude session on the shared email proves it lands under the
// Claude key and never under the Codex one.
func TestSchedulerActiveSessionCountsScopesProvidersAndCountsOnlyRunning(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		agent, sessionID, accountID string
	}{
		{"codex", "codex-running", "same@example.com"},
		{"codex", "codex-idle", "same@example.com"},
		{"claude", "claude-running", "same@example.com"},
	} {
		if _, err := store.Put(item.agent, item.sessionID, item.accountID, ""); err != nil {
			t.Fatal(err)
		}
	}
	active := NewActiveSessions()
	defer active.Begin("codex", "codex-running")()
	defer active.Begin("claude", "claude-running")()

	counts := SchedulerActiveSessionCounts(store, active)
	if got := counts[selectacct.ScoreKey(account.ProviderCodex, "same@example.com")]; got != 1 {
		t.Fatalf("running Codex sessions = %d, want 1 (the idle one must not count)", got)
	}
	if got := counts[selectacct.ScoreKey(account.ProviderClaude, "same@example.com")]; got != 1 {
		t.Fatalf("running Claude sessions = %d, want 1", got)
	}
	if got, ok := counts["same@example.com"]; ok {
		t.Fatalf("bare-ID key present (%d): a Claude session would damp the same-email Codex account", got)
	}
}
