package selectacct

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

// Claude publishes a per-model bucket only where a model has a limit of its
// own. An account that publishes no Opus bucket -- a setup token, which cannot
// read the usage endpoint, or any account whose Opus bucket came back null --
// spends Opus from its account-wide 5h/7d budget, so ForModel must route it on
// that budget instead of zero-filling it behind a sibling that does publish one.
func TestForModelClaudeAccountWithoutPoolInheritsAccountScore(t *testing.T) {
	published := Score{
		AccountID: "published", Provider: account.ProviderClaude, Headroom: 0.3, ShortHeadroom: 0.3,
		ModelScores: map[string]Score{
			ModelKey("claude-opus"): {AccountID: "published", Provider: account.ProviderClaude, Headroom: 0.3, ShortHeadroom: 0.3},
		},
	}
	unpublished := Score{AccountID: "unpublished", Provider: account.ProviderClaude, Headroom: 0.9, ShortHeadroom: 0.9}
	opus := NewScheduler([]Score{published, unpublished}).ForModel("claude-opus")

	if opus.Exhausted(account.ProviderClaude, "unpublished") {
		t.Fatal("a Claude account with no Opus bucket was zero-filled out of Opus routing")
	}
	if !opus.UsableForNewSession(account.ProviderClaude, "unpublished") {
		t.Fatal("a Claude account with no Opus bucket cannot start Opus sessions")
	}
	if got := opus.ScoreFor(account.ProviderClaude, "unpublished").Headroom; got != 0.9 {
		t.Fatalf("Opus headroom = %v, want the account-wide 0.9", got)
	}
}

// Codex model pools stay strict: an account that publishes no bucket for a
// model (Spark, for one) cannot serve it, so absence still zero-fills.
func TestForModelCodexAccountWithoutPoolStaysZeroFilled(t *testing.T) {
	spark := ModelKey("gpt-5.3-codex-spark")
	published := Score{
		AccountID: "published", Provider: account.ProviderCodex, Headroom: 0.3, ShortHeadroom: 0.3,
		ModelScores: map[string]Score{
			spark: {AccountID: "published", Provider: account.ProviderCodex, Headroom: 0.3, ShortHeadroom: 0.3},
		},
	}
	unpublished := Score{AccountID: "unpublished", Provider: account.ProviderCodex, Headroom: 0.9, ShortHeadroom: 0.9}
	pool := NewScheduler([]Score{published, unpublished}).ForModel("gpt-5.3-codex-spark")

	if !pool.Exhausted(account.ProviderCodex, "unpublished") {
		t.Fatal("a Codex account with no Spark bucket was admitted to Spark routing")
	}
}

// A pool-scoped mark on a Claude account that publishes no bucket for that
// pool is judged by the account-wide score, the same score ForModel routes it
// on: when the mark expires while that budget is still spent, the recovery
// probe is armed, and a fresh score confirming it re-arms the mark.
func TestExpiredClaudePoolMarkWithoutPoolIsJudgedOnAccountScore(t *testing.T) {
	opusKey := ModelKey("claude-opus")
	published := Score{
		AccountID: "published", Provider: account.ProviderClaude, Headroom: 0.5, ShortHeadroom: 0.5,
		ModelScores: map[string]Score{
			opusKey: {AccountID: "published", Provider: account.ProviderClaude, Headroom: 0.5, ShortHeadroom: 0.5},
		},
	}
	spent := Score{AccountID: "unpublished", Provider: account.ProviderClaude, Headroom: 0, ShortHeadroom: 0}
	ref := NewSchedulerRef(NewScheduler([]Score{published, spent}))
	ref.MarkExhaustedUntil(account.ProviderClaude, "unpublished", opusKey, time.Now().Add(-time.Second))
	_ = ref.Get()

	key := poolScopedExhaustionKey(account.ProviderClaude, "unpublished", opusKey)
	if _, ready := ref.recoveryProbeReady[key]; !ready {
		t.Fatal("expired Opus mark on a spent account with no Opus bucket armed no recovery probe")
	}

	spent.Fresh = true
	ref.Set(NewScheduler([]Score{published, spent}))
	if _, ok := ref.ExhaustedUntilFor(account.ProviderClaude, "unpublished", opusKey); !ok {
		t.Fatal("a fresh score confirming the account-wide budget is spent did not re-arm the Opus mark")
	}
}

// An Opus mark on one Claude account publishes an exhausted Opus pool on that
// account only. Its Claude siblings route Opus on their account-wide budget
// without a copied pool, and no other provider's account shows one: a model
// with no limit of its own shows no pool.
func TestClaudePoolMarkPublishesThePoolOnlyOnTheMarkedAccount(t *testing.T) {
	opusKey := ModelKey("claude-opus")
	ref := NewSchedulerRef(NewScheduler([]Score{
		{AccountID: "marked", Provider: account.ProviderClaude, Headroom: 0.8, ShortHeadroom: 0.8},
		{AccountID: "sibling", Provider: account.ProviderClaude, Headroom: 0.6, ShortHeadroom: 0.6},
		{AccountID: "codex", Provider: account.ProviderCodex, Headroom: 0.9, ShortHeadroom: 0.9},
	}))
	ref.MarkExhaustedUntil(account.ProviderClaude, "marked", opusKey, time.Now().Add(time.Hour))
	published := ref.Get()

	for _, other := range []struct {
		provider account.Provider
		id       string
	}{{account.ProviderClaude, "sibling"}, {account.ProviderCodex, "codex"}} {
		if _, ok := published.ScoreFor(other.provider, other.id).ModelScores[opusKey]; ok {
			t.Fatalf("%s publishes an Opus pool with no Opus limit behind it", other.id)
		}
	}
	opus := published.ForModel("claude-opus")
	if !opus.Exhausted(account.ProviderClaude, "marked") {
		t.Fatal("the marked account was routed Opus while its mark is live")
	}
	if opus.Exhausted(account.ProviderClaude, "sibling") || opus.ScoreFor(account.ProviderClaude, "sibling").Headroom != 0.6 {
		t.Fatalf("sibling Opus score = %+v, want its account-wide 0.6", opus.ScoreFor(account.ProviderClaude, "sibling"))
	}
}
