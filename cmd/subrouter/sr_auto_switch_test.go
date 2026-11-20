package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func TestSRAutoSwitchPicksBestOAuthAccount(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	var switchedTo string

	picked, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{
			{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth},
			{ID: "b@example.com", AuthMode: accounts.AuthModeOAuth},
			{ID: "apikey:paid", AuthMode: accounts.AuthModeAPIKey},
		},
		Sessions:     store,
		SchedulerRef: schedulerRef,
		FetchScores: func(_ context.Context, candidates []accounts.Account) ([]selectacct.Score, int) {
			for _, candidate := range candidates {
				if candidate.AuthMode != accounts.AuthModeOAuth {
					t.Fatalf("auto-switch scored non-OAuth account: %#v", candidate)
				}
			}
			return []selectacct.Score{
				{AccountID: "a@example.com", Headroom: 0.50, ShortHeadroom: 0.50},
				{AccountID: "b@example.com", Headroom: 0.90, ShortHeadroom: 0.90},
				{AccountID: "apikey:paid", Headroom: 1.00, ShortHeadroom: 1.00},
			}, 2
		},
		SwitchActive: func(_ context.Context, accountID string) error {
			switchedTo = accountID
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if picked != "b@example.com" {
		t.Fatalf("picked = %q, want b@example.com", picked)
	}
	if switchedTo != "b@example.com" {
		t.Fatalf("switchedTo = %q, want b@example.com", switchedTo)
	}
	best, err := schedulerRef.Get().PickSteady([]accounts.Account{
		{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth},
		{ID: "b@example.com", AuthMode: accounts.AuthModeOAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if best.ID != "b@example.com" {
		t.Fatalf("scheduler ref pick = %q, want b@example.com", best.ID)
	}
}

func TestSRAutoSwitchWithoutAccountManagerHookOnlyPublishesScores(t *testing.T) {
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	picked, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts:     []accounts.Account{{ID: "healthy@example.com", AuthMode: accounts.AuthModeOAuth}},
		SchedulerRef: schedulerRef,
		FetchScores: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			return []selectacct.Score{{AccountID: "healthy@example.com", Headroom: 0.9, ShortHeadroom: 0.9}}, 1
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if picked != "healthy@example.com" {
		t.Fatalf("picked = %q", picked)
	}
	selected, err := schedulerRef.Get().PickSteady([]accounts.Account{{ID: "healthy@example.com", AuthMode: accounts.AuthModeOAuth}})
	if err != nil || selected.ID != picked {
		t.Fatalf("published scheduler selected %+v, err=%v", selected, err)
	}
}

// A Claude profile may share an email with a Codex account. The sweep scores
// both (each provider rotates its own slot), and the shared ID must not leak
// across providers: the Codex twin's exhaustion must not sink the Claude pick,
// and the Claude twin's headroom must not resurrect the exhausted Codex one.
func TestSRAutoSwitchIgnoresClaudeAccountWithSameID(t *testing.T) {
	var switchedTo, claudeSwitchedTo string
	picked, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{
			{ID: "shared@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
			{ID: "healthy@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
			{ID: "shared@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
		},
		FetchScores: func(_ context.Context, candidates []accounts.Account) ([]selectacct.Score, int) {
			if len(candidates) != 3 {
				t.Fatalf("candidates = %#v, want two Codex accounts and the Claude twin", candidates)
			}
			for _, candidate := range candidates {
				if candidate.Provider != accounts.ProviderCodex && candidate.Provider != accounts.ProviderClaude {
					t.Fatalf("auto-switch scored a provider it does not rotate: %#v", candidate)
				}
			}
			return []selectacct.Score{
				{AccountID: "shared@example.com", Provider: accounts.ProviderCodex, Headroom: 0, ShortHeadroom: 0},
				{AccountID: "healthy@example.com", Provider: accounts.ProviderCodex, Headroom: 0.30, ShortHeadroom: 0.30},
				{AccountID: "shared@example.com", Provider: accounts.ProviderClaude, Headroom: 0.90, ShortHeadroom: 0.90},
			}, 3
		},
		SwitchActive: func(_ context.Context, accountID string) error {
			switchedTo = accountID
			return nil
		},
		SwitchActiveClaude: func(_ context.Context, accountID string) error {
			claudeSwitchedTo = accountID
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if switchedTo != "healthy@example.com" {
		t.Fatalf("codex switchedTo=%q, want healthy@example.com", switchedTo)
	}
	if claudeSwitchedTo != "shared@example.com" {
		t.Fatalf("claude switchedTo=%q, want the Claude twin shared@example.com", claudeSwitchedTo)
	}
	if picked != "healthy@example.com, shared@example.com" {
		t.Fatalf("picked=%q, want both providers' picks", picked)
	}
}

func TestSRAutoSwitchPreservesOtherProviderScores(t *testing.T) {
	nonCodexScores := []selectacct.Score{
		{AccountID: "claude", Provider: accounts.ProviderClaude, Headroom: 0.21, ShortHeadroom: 0.22},
		{AccountID: "kimi", Provider: accounts.ProviderKimi, Headroom: 0.31, ShortHeadroom: 0.32},
		{AccountID: "grok", Provider: accounts.ProviderGrok, Headroom: 0.41, ShortHeadroom: 0.42},
		{AccountID: "antigravity", Provider: accounts.ProviderAntigravity, Headroom: 0.51, ShortHeadroom: 0.52},
	}
	initialScores := append([]selectacct.Score{
		{AccountID: "codex-a", Provider: accounts.ProviderCodex, Headroom: 0.95, ShortHeadroom: 0.95},
		{AccountID: "codex-b", Provider: accounts.ProviderCodex, Headroom: 0.05, ShortHeadroom: 0.05},
	}, nonCodexScores...)
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(initialScores))

	picked, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{
			{ID: "codex-a", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
			{ID: "codex-b", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
			{ID: "claude", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
			{ID: "kimi", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth},
			{ID: "grok", Provider: accounts.ProviderGrok, AuthMode: accounts.AuthModeOAuth},
			{ID: "antigravity", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth},
		},
		SchedulerRef:       schedulerRef,
		SwitchActive:       func(context.Context, string) error { return nil },
		SwitchActiveClaude: func(context.Context, string) error { return nil },
		FetchScores: func(_ context.Context, candidates []accounts.Account) ([]selectacct.Score, int) {
			// The sweep owns the two rotating providers only; Kimi, Grok and
			// Antigravity are the proxy refresh's to score.
			if len(candidates) != 3 {
				t.Fatalf("candidates = %#v, want the two Codex accounts and the Claude profile", candidates)
			}
			for _, candidate := range candidates {
				if candidate.Provider != accounts.ProviderCodex && candidate.Provider != accounts.ProviderClaude {
					t.Fatalf("auto-switch scored a provider it does not rotate: %#v", candidate)
				}
			}
			// The Claude measurement is absent from the batch (as when its
			// fetch fails), so its published score must survive the merge.
			return []selectacct.Score{
				{AccountID: "codex-a", Provider: accounts.ProviderCodex, Headroom: 0.10, ShortHeadroom: 0.10},
				{AccountID: "codex-b", Provider: accounts.ProviderCodex, Headroom: 0.90, ShortHeadroom: 0.90},
			}, 2
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if picked != "codex-b, claude" {
		t.Fatalf("picked = %q, want codex-b for Codex and claude for Claude", picked)
	}

	published := schedulerRef.Get()
	for _, want := range nonCodexScores {
		got := published.ScoreFor(want.Provider, want.AccountID)
		if got.Headroom != want.Headroom || got.ShortHeadroom != want.ShortHeadroom {
			t.Errorf("published %s score = %+v, want preserved %+v", want.Provider, got, want)
		}
	}
	if got := published.ScoreFor(accounts.ProviderCodex, "codex-a"); got.Headroom != 0.10 {
		t.Errorf("published Codex score = %+v, want refreshed headroom 0.10", got)
	}
}

func TestSRAutoSwitchRetriesPublicationWithoutRefetchingAfterConcurrentFullRefresh(t *testing.T) {
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{{
		AccountID: "codex-a", Provider: accounts.ProviderCodex, Headroom: 0.5, ShortHeadroom: 0.5,
	}}))
	fetches := 0
	picked, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{{
			ID: "codex-a", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth,
		}},
		SchedulerRef: schedulerRef,
		SwitchActive: func(context.Context, string) error { return nil },
		FetchScores: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			fetches++
			if fetches == 1 {
				revision := schedulerRef.ScoreRevision()
				if !schedulerRef.SetForAccountGenerationAtScoreRevision(selectacct.NewScheduler([]selectacct.Score{{
					AccountID: "codex-a", Provider: accounts.ProviderCodex, Headroom: 0.8, ShortHeadroom: 0.8,
				}}), 0, revision) {
					t.Fatal("concurrent full refresh was rejected")
				}
			}
			return []selectacct.Score{{
				AccountID: "codex-a", Provider: accounts.ProviderCodex, Headroom: 0.2, ShortHeadroom: 0.2,
			}}, 1
		},
	})
	if err != nil || picked != "codex-a" {
		t.Fatalf("auto-switch picked %q with error %v", picked, err)
	}
	if fetches != 1 {
		t.Fatalf("score fetches = %d, want one fetch with publication retry", fetches)
	}
	if got := schedulerRef.Get().ScoreFor(accounts.ProviderCodex, "codex-a").Headroom; got != 0.2 {
		t.Fatalf("Codex headroom = %v, want retried fresh value 0.2", got)
	}
}

func TestSRAutoSwitchScoreRevisionConflictDoesNotRepeatUsageSweep(t *testing.T) {
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	fetches := 0
	picked, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{{
			ID: "codex-a", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth,
		}},
		SchedulerRef: schedulerRef,
		FetchScores: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			fetches++
			revision := schedulerRef.ScoreRevision()
			if !schedulerRef.SetForAccountGenerationAtScoreRevision(selectacct.NewScheduler(nil), 0, revision) {
				t.Fatal("test could not advance scheduler revision")
			}
			return []selectacct.Score{{
				AccountID: "codex-a", Provider: accounts.ProviderCodex, Headroom: 0.8, ShortHeadroom: 0.8,
			}}, 1
		},
	})
	if err != nil || picked != "codex-a" {
		t.Fatalf("auto-switch picked %q with error %v", picked, err)
	}
	if fetches != 1 {
		t.Fatalf("score fetches = %d, want one fetch across CAS retry", fetches)
	}
}

func TestSRAutoSwitchLocalGenerationConflictDoesNotRepeatStableAccountSweep(t *testing.T) {
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	generation := uint64(0)
	fetches := 0
	picked, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		AccountsSnapshotFunc: func() ([]accounts.Account, uint64) {
			return []accounts.Account{{
				ID: "codex-a", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth,
			}}, generation
		},
		SchedulerRef: schedulerRef,
		FetchScores: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			fetches++
			generation++
			schedulerRef.AdvanceAccountGeneration(generation)
			return []selectacct.Score{{
				AccountID: "codex-a", Provider: accounts.ProviderCodex, Headroom: 0.8, ShortHeadroom: 0.8,
			}}, 1
		},
	})
	if err != nil || picked != "codex-a" {
		t.Fatalf("auto-switch picked %q with error %v", picked, err)
	}
	if fetches != 1 {
		t.Fatalf("score fetches = %d, want one stable account-set fetch", fetches)
	}
}

func TestSRAutoSwitchRefetchesAfterAccountSetChanges(t *testing.T) {
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	generation := uint64(0)
	accountID := "codex-a"
	fetches := 0
	picked, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		AccountsSnapshotFunc: func() ([]accounts.Account, uint64) {
			return []accounts.Account{{
				ID: accountID, Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth,
			}}, generation
		},
		SchedulerRef: schedulerRef,
		FetchScores: func(_ context.Context, candidates []accounts.Account) ([]selectacct.Score, int) {
			fetches++
			measured := candidates[0].ID
			if fetches == 1 {
				accountID = "codex-b"
				generation++
				schedulerRef.AdvanceAccountGeneration(generation)
			}
			return []selectacct.Score{{
				AccountID: measured, Provider: accounts.ProviderCodex, Headroom: .8, ShortHeadroom: .8,
			}}, 1
		},
	})
	if err != nil || picked != "codex-b" {
		t.Fatalf("auto-switch picked %q with error %v", picked, err)
	}
	if fetches != 2 {
		t.Fatalf("score fetches = %d, want one fetch per account-set generation", fetches)
	}
}

func TestSRAutoSwitchUsesLiveAccountsFunc(t *testing.T) {
	var switchedTo string
	picked, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{
			{ID: "old@example.com", AuthMode: accounts.AuthModeOAuth},
		},
		AccountsFunc: func() []accounts.Account {
			return []accounts.Account{
				{ID: "new@example.com", AuthMode: accounts.AuthModeOAuth},
			}
		},
		FetchScores: func(_ context.Context, candidates []accounts.Account) ([]selectacct.Score, int) {
			if len(candidates) != 1 || candidates[0].ID != "new@example.com" {
				t.Fatalf("candidates = %#v, want live account", candidates)
			}
			return []selectacct.Score{{AccountID: "new@example.com", Headroom: 0.90, ShortHeadroom: 0.90}}, 1
		},
		SwitchActive: func(_ context.Context, accountID string) error {
			switchedTo = accountID
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if picked != "new@example.com" || switchedTo != "new@example.com" {
		t.Fatalf("picked=%q switched=%q, want new@example.com", picked, switchedTo)
	}
}

func TestSRAutoSwitchSkipsWhenUsageUnavailable(t *testing.T) {
	ran := false
	_, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth}},
		FetchScores: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			return []selectacct.Score{{AccountID: "a@example.com", Headroom: 0, ShortHeadroom: 0}}, 0
		},
		SwitchActive: func(context.Context, string) error {
			ran = true
			return nil
		},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if ran {
		t.Fatal("sr switch ran despite missing fresh usage")
	}
}

func TestSRAutoSwitchUsesBestNonExhaustedOAuthBelowProtectedHeadroom(t *testing.T) {
	var switchedTo string
	picked, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{
			{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth},
			{ID: "b@example.com", AuthMode: accounts.AuthModeOAuth},
		},
		FetchScores: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			return []selectacct.Score{
				{AccountID: "a@example.com", Headroom: 0.12, ShortHeadroom: 0.12},
				{AccountID: "b@example.com", Headroom: 0.39, ShortHeadroom: 0.39},
			}, 2
		},
		SwitchActive: func(_ context.Context, accountID string) error {
			switchedTo = accountID
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if picked != "b@example.com" || switchedTo != "b@example.com" {
		t.Fatalf("picked=%q switchedTo=%q, want b@example.com", picked, switchedTo)
	}
}

func TestSRAutoSwitchSkipsWhenOAuthAccountsExhausted(t *testing.T) {
	ran := false
	_, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{
			{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth},
			{ID: "b@example.com", AuthMode: accounts.AuthModeOAuth},
		},
		FetchScores: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			return []selectacct.Score{
				{AccountID: "a@example.com", Headroom: 0, ShortHeadroom: 0},
				{AccountID: "b@example.com", Headroom: 0, ShortHeadroom: 0},
			}, 2
		},
		SwitchActive: func(context.Context, string) error {
			ran = true
			return nil
		},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if ran {
		t.Fatal("sr switch ran despite exhausted OAuth accounts")
	}
}

// The wiring-level pin for the provider-blind sweep regression: a config that
// forgets FetchScores must refuse to run, not fall back. The old default was a
// codex-shaped sweep that fetched every account's usage from the Codex
// endpoint, so every Claude account 401'd, scored zero forever, and the
// scheduler ran the pool blind.
func TestAutoSwitchRefusesToRunWithoutFetchScores(t *testing.T) {
	_, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth}},
	})
	if err == nil {
		t.Fatal("auto-switch ran without FetchScores; the removed codex-shaped default must not come back")
	}
	if !strings.Contains(err.Error(), "FetchScores") {
		t.Fatalf("error %q does not name the missing wiring", err)
	}
}

// Scores cover every provider, and each provider rotates its own active
// slot: the codex switch feeds the codex CLI's auth.json, the claude switch
// moves the store's active Claude profile. A Claude account topping the pool
// rotates the CLAUDE slot; the best CODEX account still rotates codex; and
// neither provider's ID ever reaches the other provider's switch function.
func TestAutoSwitchRotatesEachProviderIndependently(t *testing.T) {
	var codexSwitched, claudeSwitched []string
	picked, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{
			{ID: "claude-top", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
			{ID: "claude-low", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
			{ID: "codex-a", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
		},
		FetchScores: func(_ context.Context, candidates []accounts.Account) ([]selectacct.Score, int) {
			if len(candidates) != 3 {
				t.Errorf("scored %d candidates, want both providers", len(candidates))
			}
			return []selectacct.Score{
				{AccountID: "claude-top", Provider: accounts.ProviderClaude, Headroom: 0.9, ShortHeadroom: 0.9, Fresh: true},
				{AccountID: "claude-low", Provider: accounts.ProviderClaude, Headroom: 0.4, ShortHeadroom: 0.4, Fresh: true},
				{AccountID: "codex-a", Provider: accounts.ProviderCodex, Headroom: 0.5, ShortHeadroom: 0.5, Fresh: true},
			}, 3
		},
		SwitchActive: func(_ context.Context, accountID string) error {
			codexSwitched = append(codexSwitched, accountID)
			return nil
		},
		SwitchActiveClaude: func(_ context.Context, accountID string) error {
			claudeSwitched = append(claudeSwitched, accountID)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("srAutoSwitchOnce: %v", err)
	}
	if len(codexSwitched) != 1 || codexSwitched[0] != "codex-a" {
		t.Fatalf("codex switch calls = %v, want exactly [codex-a]", codexSwitched)
	}
	if len(claudeSwitched) != 1 || claudeSwitched[0] != "claude-top" {
		t.Fatalf("claude switch calls = %v, want exactly [claude-top]", claudeSwitched)
	}
	if !strings.Contains(picked, "codex-a") || !strings.Contains(picked, "claude-top") {
		t.Fatalf("picked = %q, want both providers' selections reported", picked)
	}
}

// A Claude-only pool rotates the active Claude profile and must never call
// the codex switch; the published scores land either way.
func TestAutoSwitchRotatesClaudeInAClaudeOnlyPool(t *testing.T) {
	var claudeSwitched []string
	picked, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{
			{ID: "claude-only", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
		},
		FetchScores: func(_ context.Context, candidates []accounts.Account) ([]selectacct.Score, int) {
			return []selectacct.Score{
				{AccountID: "claude-only", Provider: accounts.ProviderClaude, Headroom: 0.7, ShortHeadroom: 0.7, Fresh: true},
			}, 1
		},
		SwitchActive: func(_ context.Context, accountID string) error {
			t.Errorf("codex switchActive called with %q in a claude-only pool", accountID)
			return nil
		},
		SwitchActiveClaude: func(_ context.Context, accountID string) error {
			claudeSwitched = append(claudeSwitched, accountID)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("a claude-only pool must rotate cleanly: %v", err)
	}
	if picked != "claude-only" {
		t.Fatalf("picked = %q, want claude-only", picked)
	}
	if len(claudeSwitched) != 1 || claudeSwitched[0] != "claude-only" {
		t.Fatalf("claude switch calls = %v, want exactly [claude-only]", claudeSwitched)
	}
}

// The identity-only account (SUBROUTER_IDENTITY_ONLY) is scored like any
// other but never made the active Claude profile, even with the most
// headroom: a direct `sr claude` launch would spend it and run a second
// refresher on its login.
func TestAutoSwitchNeverRotatesOntoTheIdentityOnlyAccount(t *testing.T) {
	rotate := func(identityOnly string) []string {
		var claudeSwitched []string
		var scored []string
		_, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
			Accounts: []accounts.Account{
				{ID: "identity", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
				{ID: "pool", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
			},
			FetchScores: func(_ context.Context, candidates []accounts.Account) ([]selectacct.Score, int) {
				for _, candidate := range candidates {
					scored = append(scored, candidate.ID)
				}
				return []selectacct.Score{
					{AccountID: "identity", Provider: accounts.ProviderClaude, Headroom: 0.95, ShortHeadroom: 0.95, Fresh: true},
					{AccountID: "pool", Provider: accounts.ProviderClaude, Headroom: 0.4, ShortHeadroom: 0.4, Fresh: true},
				}, 2
			},
			SwitchActiveClaude: func(_ context.Context, accountID string) error {
				claudeSwitched = append(claudeSwitched, accountID)
				return nil
			},
			ClaudeIdentityOnly: func() string { return identityOnly },
		})
		if err != nil {
			t.Fatalf("identityOnly=%q: %v", identityOnly, err)
		}
		if len(scored) != 2 {
			t.Fatalf("identityOnly=%q: scored %v, want both accounts", identityOnly, scored)
		}
		return claudeSwitched
	}
	if got := rotate(""); len(got) != 1 || got[0] != "identity" {
		t.Fatalf("calibration: rotation = %v, want [identity], the most headroom", got)
	}
	if got := rotate("identity"); len(got) != 1 || got[0] != "pool" {
		t.Fatalf("identity-only: rotation = %v, want [pool]", got)
	}
}

// Rotation picks the account a human should START a long session on, so the
// endgame-drain widening (which routes disposable requests onto accounts
// being deliberately run into the ground) must not apply: with a usable
// steady account available, rotation takes it over a sub-threshold draining
// one.
func TestAutoSwitchRotationAvoidsEndgameDrainAccounts(t *testing.T) {
	var claudeSwitched []string
	picked, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{
			{ID: "draining", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
			{ID: "steady", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
		},
		FetchScores: func(_ context.Context, candidates []accounts.Account) ([]selectacct.Score, int) {
			return []selectacct.Score{
				// Below the usable threshold but 20h from its weekly roll:
				// request routing would drain-boost it; rotation must not.
				{AccountID: "draining", Provider: accounts.ProviderClaude, Headroom: 0.2, ShortHeadroom: 0.9,
					WeeklyHeadroom: 0.2, WeeklyResetAt: time.Now().Add(20 * time.Hour), Fresh: true},
				{AccountID: "steady", Provider: accounts.ProviderClaude, Headroom: 0.8, ShortHeadroom: 0.9,
					WeeklyHeadroom: 0.8, WeeklyResetAt: time.Now().Add(5 * 24 * time.Hour), Fresh: true},
			}, 2
		},
		SwitchActiveClaude: func(_ context.Context, accountID string) error {
			claudeSwitched = append(claudeSwitched, accountID)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("srAutoSwitchOnce: %v", err)
	}
	if picked != "steady" || len(claudeSwitched) != 1 || claudeSwitched[0] != "steady" {
		t.Fatalf("picked=%q switched=%v, want the steady account as the interactive default", picked, claudeSwitched)
	}
}

// One provider's failed switch must not cost the other its rotation: a
// broken claude registry still lets codex rotate, and the error names the
// provider that failed.
func TestAutoSwitchClaudeFailureDoesNotBlockCodexRotation(t *testing.T) {
	var codexSwitched []string
	picked, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{
			{ID: "claude-top", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
			{ID: "codex-a", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
		},
		FetchScores: func(_ context.Context, candidates []accounts.Account) ([]selectacct.Score, int) {
			return []selectacct.Score{
				{AccountID: "claude-top", Provider: accounts.ProviderClaude, Headroom: 0.9, ShortHeadroom: 0.9, Fresh: true},
				{AccountID: "codex-a", Provider: accounts.ProviderCodex, Headroom: 0.5, ShortHeadroom: 0.5, Fresh: true},
			}, 2
		},
		SwitchActive: func(_ context.Context, accountID string) error {
			codexSwitched = append(codexSwitched, accountID)
			return nil
		},
		SwitchActiveClaude: func(_ context.Context, accountID string) error {
			return fmt.Errorf("registry locked")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "claude:") {
		t.Fatalf("err = %v, want a claude-attributed rotation error", err)
	}
	if len(codexSwitched) != 1 || codexSwitched[0] != "codex-a" {
		t.Fatalf("codex switch calls = %v, want codex rotation to survive the claude failure", codexSwitched)
	}
	if picked != "codex-a" {
		t.Fatalf("picked = %q, want the surviving codex selection reported", picked)
	}
}

// The rotation picks an interactive default (and, through an explicit
// SwitchActive hook, may still export it), so it must not choose an account
// the live request path has already found to be out of quota.
//
// Request-time exhaustion (an upstream 429 carrying usage_limit_reached) is
// recorded as an overlay inside SchedulerRef and applied only by Get(). The
// rotation used to rank the raw NewScheduler(scores) it had just published,
// which never carries that overlay -- so a freshly-429'd account whose usage
// snapshot still read healthy was selected and written into auth.json, once
// per interval, for as long as the stale snapshot stood.
func TestSRAutoSwitchHonorsRequestTimeExhaustionMarks(t *testing.T) {
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	// The router just took a usage-limit rejection on the account whose
	// scored headroom is the highest in the pool.
	schedulerRef.MarkExhausted(accounts.ProviderCodex, "spent@example.com", "")

	var switchedTo string
	picked, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{
			{ID: "spent@example.com", AuthMode: accounts.AuthModeOAuth},
			{ID: "healthy@example.com", AuthMode: accounts.AuthModeOAuth},
		},
		SchedulerRef: schedulerRef,
		FetchScores: func(_ context.Context, _ []accounts.Account) ([]selectacct.Score, int) {
			// The usage snapshot has not caught up with the 429 yet -- the
			// whole point of the overlay.
			return []selectacct.Score{
				{AccountID: "spent@example.com", Headroom: 0.95, ShortHeadroom: 0.95},
				{AccountID: "healthy@example.com", Headroom: 0.60, ShortHeadroom: 0.60},
			}, 2
		},
		SwitchActive: func(_ context.Context, accountID string) error {
			switchedTo = accountID
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if picked != "healthy@example.com" {
		t.Fatalf("picked = %q, want healthy@example.com (spent@ is marked exhausted)", picked)
	}
	if switchedTo != "healthy@example.com" {
		t.Fatalf("wrote %q into the active Codex auth, want healthy@example.com", switchedTo)
	}
}

// Decision 1 of the 2026-09 upstream merge: the daemon's Codex half is
// read-only. With no explicit SwitchActive hook the sweep must still pick and
// report a Codex account, and must not write ~/.codex/auth.json -- a pooled
// chain exported there is the shared refresh-token chain the serving isolation
// gate refuses, so a default writer would knock every rotated-to account out of
// serving. HOME and CODEX_HOME are isolated so a regression that restores a
// default writer lands in a temp dir, not the developer's real Codex login.
func TestSRAutoSwitchCodexHalfNeverWritesActiveAuthByDefault(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(home, ".subrouter"))

	picked, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{
			{ID: "a@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
			{ID: "b@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
		},
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		FetchScores: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			return []selectacct.Score{
				{AccountID: "a@example.com", Provider: accounts.ProviderCodex, Headroom: 0.2, ShortHeadroom: 0.2, Fresh: true},
				{AccountID: "b@example.com", Provider: accounts.ProviderCodex, Headroom: 0.8, ShortHeadroom: 0.8, Fresh: true},
			}, 2
		},
	})
	if err != nil {
		t.Fatalf("read-only Codex sweep failed: %v", err)
	}
	if picked != "b@example.com" {
		t.Fatalf("picked = %q, want b@example.com reported", picked)
	}
	for _, path := range []string{
		filepath.Join(codexHome, "auth.json"),
		accounts.DefaultCodexAuthPath(),
	} {
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("auto-switch wrote the interactive Codex login at %s (stat err %v)", path, statErr)
		}
	}
}

// The periodic sweep feeds the startup coordinator's shared snapshot for a
// successor worker. That snapshot is a Codex-only contract keyed on the Codex
// OAuth set, so a sweep that scores Claude too must publish exactly the Codex
// slice under exactly that key -- proven by a coordinator that loads it
// without issuing a single usage request.
func TestSRAutoSwitchSnapshotIsLoadableByStartupCoordinator(t *testing.T) {
	store := newSRUsageScoreStore(t.TempDir())
	pool := []accounts.Account{
		{ID: "codex-a", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
		{ID: "claude-a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
	}
	snapshot := func() ([]accounts.Account, uint64) { return pool, 0 }
	_, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		AccountsSnapshotFunc: snapshot,
		SchedulerRef:         selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		ScoreSnapshots:       store,
		SwitchActiveClaude:   func(context.Context, string) error { return nil },
		FetchScores: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			return []selectacct.Score{
				{AccountID: "codex-a", Provider: accounts.ProviderCodex, Headroom: 0.7, ShortHeadroom: 0.7, Fresh: true},
				{AccountID: "claude-a", Provider: accounts.ProviderClaude, Headroom: 0.6, ShortHeadroom: 0.6, Fresh: true},
			}, 2
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	written, ok, err := store.read(codexScoreGenerationKey(0, codexOAuthAccounts(pool)), time.Hour)
	if err != nil || !ok {
		t.Fatalf("no snapshot under the Codex key: ok=%v err=%v", ok, err)
	}
	for _, score := range written.Scores {
		if score.Provider != accounts.ProviderCodex {
			t.Fatalf("snapshot carries a %s score; the startup contract is Codex-only: %+v", score.Provider, written.Scores)
		}
	}

	successor := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	if err := ensureStartupScores(context.Background(), startupScoreConfig{
		Interval:         time.Hour,
		AccountsSnapshot: snapshot,
		SchedulerRef:     successor,
		Store:            store,
		PollInterval:     time.Millisecond,
		FetchScores: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			t.Fatal("successor re-swept instead of loading the published snapshot")
			return nil, 0
		},
	}); err != nil {
		t.Fatal(err)
	}
	if got := successor.Get().ScoreFor(accounts.ProviderCodex, "codex-a").Headroom; got != 0.7 {
		t.Fatalf("successor Codex headroom = %v, want the published 0.7", got)
	}
}

// The shared snapshot is an optimisation for a successor worker. Failing to
// write it must not cost this sweep its Claude rotation.
func TestSRAutoSwitchSnapshotFailureDoesNotBlockClaudeRotation(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var claudeSwitched string
	_, err := srAutoSwitchOnce(context.Background(), srAutoSwitchConfig{
		Accounts: []accounts.Account{
			{ID: "codex-a", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
			{ID: "claude-a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
		},
		// The store's directory is a regular file, so every write fails.
		ScoreSnapshots: newSRUsageScoreStore(filepath.Join(blocker, "state")),
		SwitchActiveClaude: func(_ context.Context, accountID string) error {
			claudeSwitched = accountID
			return nil
		},
		FetchScores: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			return []selectacct.Score{
				{AccountID: "codex-a", Provider: accounts.ProviderCodex, Headroom: 0.7, ShortHeadroom: 0.7, Fresh: true},
				{AccountID: "claude-a", Provider: accounts.ProviderClaude, Headroom: 0.6, ShortHeadroom: 0.6, Fresh: true},
			}, 2
		},
	})
	if err != nil {
		t.Fatalf("a snapshot write failure failed the sweep: %v", err)
	}
	if claudeSwitched != "claude-a" {
		t.Fatalf("claude switched to %q, want claude-a despite the snapshot failure", claudeSwitched)
	}
}
