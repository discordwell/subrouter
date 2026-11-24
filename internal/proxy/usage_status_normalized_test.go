package proxy

import (
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
)

// Raw usage `windows` carry provider-specific names -- claude reports 5h/7d,
// codex reports primary/<model>/primary/reached. Every consumer that reads them
// directly has to hardcode that mapping, and the failure when it guesses wrong
// is silent: the lookup misses, the account looks like it has no data, and no
// data renders as exhausted. That is not hypothetical -- reading codex rows with
// a "7d" lookup reported five untouched Pro accounts as spent.
//
// These tests pin the contract that removes the need to guess: usage-status
// publishes the scheduler's own normalized view, and publishes NOTHING at all
// when the scheduler has no measurement.
func TestNormalizedUsageIsProviderIndependent(t *testing.T) {
	scores := []selectacct.Score{
		{
			AccountID:              "claude-acct",
			Provider:               accounts.ProviderClaude,
			Headroom:               0.28,
			ShortHeadroom:          0.99,
			ShortResetAfterSeconds: 3600,
			WeeklyHeadroom:         0.18,
			WeeklyResetAt:          time.Now().Add(9 * time.Hour),
			Fresh:                  true,
		},
		{
			// The regression: a codex account with a full week left. Its
			// window is named "primary" and there is no "7d" anywhere on it.
			AccountID:     "codex-acct",
			Provider:      accounts.ProviderCodex,
			Headroom:      1,
			ShortHeadroom: 1,
			Fresh:         true,
		},
	}
	server := Server{Scheduler: selectacct.NewScheduler(scores)}

	statuses := []AccountUsageStatus{
		{AccountStatus: AccountStatus{ID: "claude-acct", Provider: accounts.ProviderClaude}},
		{AccountStatus: AccountStatus{ID: "codex-acct", Provider: accounts.ProviderCodex}},
		{AccountStatus: AccountStatus{ID: "never-scored", Provider: accounts.ProviderCodex}},
	}

	out := server.withNormalizedUsage(statuses)
	if len(out) != len(statuses) {
		t.Fatalf("got %d statuses, want %d", len(out), len(statuses))
	}

	byID := map[string]*NormalizedUsage{}
	for _, status := range out {
		byID[status.ID] = status.Normalized
	}

	claude := byID["claude-acct"]
	if claude == nil {
		t.Fatal("claude account has no normalized usage")
	}
	if claude.Headroom != 0.28 {
		t.Errorf("claude headroom = %v, want 0.28", claude.Headroom)
	}
	if claude.SecondsToWeeklyReset <= 0 {
		t.Errorf("claude seconds_to_weekly_reset = %d, want > 0", claude.SecondsToWeeklyReset)
	}
	if !claude.Fresh {
		t.Error("claude score was fresh; normalized output says otherwise")
	}

	// The point of the whole change: a codex account reports real capacity
	// without any consumer knowing that its weekly window is called "primary".
	codex := byID["codex-acct"]
	if codex == nil {
		t.Fatal("codex account has no normalized usage -- the exact defect this replaces")
	}
	if codex.Headroom != 1 {
		t.Errorf("untouched codex headroom = %v, want 1 (reported as spent is the bug)", codex.Headroom)
	}

	// An account the scheduler never scored must be ABSENT, not optimistic.
	// ScoreFor would hand back Headroom:1 here; publishing that would assert
	// full capacity for something nobody measured -- a worse failure than the
	// missing-window one, because it claims health instead of absence.
	if got, ok := byID["never-scored"]; !ok || got != nil {
		t.Errorf("unscored account normalized = %+v, want nil (absent means unknown)", got)
	}
}

// The input slice can be owned by AccountRef; normalizing must not write
// through to it.
func TestNormalizedUsageDoesNotMutateInput(t *testing.T) {
	server := Server{Scheduler: selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "a", Provider: accounts.ProviderClaude, Headroom: 0.5, Fresh: true},
	})}
	statuses := []AccountUsageStatus{
		{AccountStatus: AccountStatus{ID: "a", Provider: accounts.ProviderClaude}},
	}

	if out := server.withNormalizedUsage(statuses); out[0].Normalized == nil {
		t.Fatal("expected normalized usage on the returned copy")
	}
	if statuses[0].Normalized != nil {
		t.Error("withNormalizedUsage mutated its input slice")
	}
}
