package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/internal/agents/claude"
	"github.com/discordwell/subrouter/internal/proxy"
	"github.com/discordwell/subrouter/selectacct"
	"github.com/discordwell/subrouter/session"
)

const defaultSRSwitchInterval = 10 * time.Minute

const srAutoSwitchPublishAttempts = 3

type srAutoSwitchConfig struct {
	Interval     time.Duration
	Accounts     []accounts.Account
	AccountsFunc func() []accounts.Account
	// AccountsSnapshotFunc couples dynamic accounts to the generation used for
	// scheduler publication. It supersedes AccountsFunc when configured.
	AccountsSnapshotFunc func() ([]accounts.Account, uint64)
	Sessions             *session.Store
	SchedulerRef         *selectacct.SchedulerRef
	Logger               *slog.Logger
	FetchScores          func(context.Context, []accounts.Account) ([]selectacct.Score, int)
	// SwitchActive is an explicit account-manager hook used by callers and
	// tests that intentionally synchronize interactive Codex auth. Nil keeps
	// the Codex half of the sweep read-only: it still scores, publishes and
	// reports the best Codex account, but never writes ~/.codex/auth.json.
	// serve leaves it nil. Serving stores run with DisableActiveAuthSync and
	// RequireIsolatedOAuth, and a pooled chain exported into auth.json is
	// exactly the shared refresh-token chain that gate refuses
	// (shared_active_refresh_token), so a daemon-side writer would knock every
	// account it rotated to out of serving.
	SwitchActive func(context.Context, string) error
	// SwitchActiveClaude rotates the store's active Claude profile — the
	// default that `sr claude` commands and `sr claude launch` operate on.
	// Nil means the serving Claude store's SetActiveProfile.
	SwitchActiveClaude func(context.Context, string) error
	// Lease keeps the sweep singleton across concurrently live workers.
	Lease srAutoSwitchLease
	// DelayFirstSweep is used by serving workers whose startup score coordinator
	// already owns the immediate shared sweep. Periodic refreshes begin after one
	// interval instead of duplicating startup work.
	DelayFirstSweep bool
	// ScoreSnapshots publishes successful periodic refreshes for successor
	// workers. Nil leaves one-shot and test callers unchanged.
	ScoreSnapshots *srUsageScoreStore
	// ClaudeIdentityOnly names the Claude account SUBROUTER_IDENTITY_ONLY keeps
	// for the control plane (Server.IdentityOnlyAccountID), "" for none. It is
	// never made the active Claude profile: a direct `sr claude` launch on it
	// would spend it, and would run a second refresher on its login.
	ClaudeIdentityOnly func() string
	// PinOnly reports whether an account takes pinned sessions only
	// (SUBROUTER_PIN_ONLY_ACCOUNTS, PATCHES §98; AccountRef.PinOnlyAccount).
	// Such an account is scored like any other but is never the pick for
	// either provider: as the active Claude profile, a bare `sr claude` launch
	// would spend it with no pin. Nil lists none.
	PinOnly func(accountID string) bool
}

func runSRAutoSwitch(ctx context.Context, cfg srAutoSwitchConfig) {
	if cfg.Interval <= 0 {
		return
	}
	firstDelay := time.Duration(0)
	if cfg.DelayFirstSweep {
		firstDelay = cfg.Interval
	}
	timer := time.NewTimer(firstDelay)
	defer timer.Stop()
	// A broken lease fails the same way every tick; log each distinct error
	// once, and again after it clears and recurs.
	lastLeaseErr := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			claimed, leaseErr := cfg.Lease.acquire(cfg.Interval)
			if leaseErr != nil {
				if msg := leaseErr.Error(); msg != lastLeaseErr {
					lastLeaseErr = msg
					logSRAutoSwitch(cfg.Logger, slog.LevelWarn, "sr auto-switch lease unavailable, sweeping anyway", "error", leaseErr)
				}
			} else if lastLeaseErr != "" {
				lastLeaseErr = ""
				logSRAutoSwitch(cfg.Logger, slog.LevelInfo, "sr auto-switch lease recovered")
			}
			if !claimed {
				// Another live worker already swept within this interval.
				timer.Reset(cfg.Interval)
				continue
			}
			picked, err := srAutoSwitchOnce(ctx, cfg)
			switch {
			case err != nil && picked != "":
				logSRAutoSwitch(cfg.Logger, slog.LevelWarn, "sr auto-switch rotated some providers and failed on others", "selected", picked, "error", err)
			case err != nil:
				logSRAutoSwitch(cfg.Logger, slog.LevelWarn, "sr auto-switch failed", "error", err)
			case picked == "":
				logSRAutoSwitch(cfg.Logger, slog.LevelInfo, "sr auto-switch published scores; nothing to rotate")
			default:
				logSRAutoSwitch(cfg.Logger, slog.LevelInfo, "sr auto-switch selected account", "account", picked)
			}
			timer.Reset(cfg.Interval)
		}
	}
}

func srAutoSwitchOnce(ctx context.Context, cfg srAutoSwitchConfig) (string, error) {
	// No default: the only correct scorer is the proxy's provider-aware sweep
	// (Server.ScoreOAuthAccounts), and this package cannot reach it without
	// the caller wiring it in. The old fallback was a codex-shaped sweep that
	// fetched every account's usage from the Codex endpoint, so a config that
	// forgot FetchScores silently 401'd and zeroed every Claude account.
	fetchScores := cfg.FetchScores
	if fetchScores == nil {
		return "", errors.New("sr auto-switch requires FetchScores; wire Server.ScoreOAuthAccounts")
	}
	var scheduler selectacct.Scheduler
	var candidates []accounts.Account
	var scores []selectacct.Score
	var successful int
	var fetchedGenerationKey string
	lastConflict := "scheduler score snapshot changed"
	for attempt := 1; attempt <= srAutoSwitchPublishAttempts; attempt++ {
		allAccounts := cfg.Accounts
		accountGeneration := uint64(0)
		if cfg.AccountsSnapshotFunc != nil {
			allAccounts, accountGeneration = cfg.AccountsSnapshotFunc()
		} else if cfg.AccountsFunc != nil {
			allAccounts = cfg.AccountsFunc()
		}
		// Both rotating providers are scored, not only Codex: the Claude half
		// rotates the active profile and needs honest scores to do it (see
		// PATCHES.md §9).
		candidates = autoSwitchOAuthAccounts(allAccounts)
		if len(candidates) == 0 {
			return "", fmt.Errorf("no OAuth accounts available for sr auto-switch")
		}
		generationKey := codexScoreGenerationKey(accountGeneration, candidates)
		scoreRevision := uint64(0)
		if cfg.SchedulerRef != nil {
			scoreRevision = cfg.SchedulerRef.ScoreRevision()
		}

		// Score-revision conflicts do not invalidate provider measurements. Keep
		// the fetched scores and retry only the atomic publication. A changed
		// account or credential generation is the sole reason to re-fetch.
		if fetchedGenerationKey != generationKey {
			scores, successful = fetchScores(ctx, candidates)
			if successful == 0 {
				return "", fmt.Errorf("no fresh OAuth usage scores available")
			}
			// OAuth refresh may rotate the credential used by the successful
			// measurement. The scores describe that post-refresh credential, so
			// adopt its key without issuing the same usage requests again. A real
			// account-set change invalidates the batch and is re-fetched.
			if cfg.AccountsSnapshotFunc != nil {
				currentAccounts, currentGeneration := cfg.AccountsSnapshotFunc()
				currentCandidates := autoSwitchOAuthAccounts(currentAccounts)
				if !sameCodexScoreAccounts(candidates, currentCandidates) {
					fetchedGenerationKey = ""
					if attempt == srAutoSwitchPublishAttempts {
						return "", fmt.Errorf("sr auto-switch account pool changed during usage fetch")
					}
					continue
				}
				candidates = currentCandidates
				accountGeneration = currentGeneration
				generationKey = codexScoreGenerationKey(accountGeneration, candidates)
				if cfg.SchedulerRef != nil {
					scoreRevision = cfg.SchedulerRef.ScoreRevision()
				}
			}
			fetchedGenerationKey = generationKey
		}

		scheduler = selectacct.NewScheduler(scores)
		if cfg.SchedulerRef == nil {
			break
		}
		// SchedulerRef is shared by every provider. Atomically overlay only the
		// freshly fetched scores so a concurrent full-provider refresh -- and the
		// scores of providers this sweep does not own (Kimi, Grok, ...) -- cannot
		// be erased.
		var published bool
		scheduler, published = cfg.SchedulerRef.MergeScoresForAccountGenerationAtScoreRevision(
			scores, accountGeneration, scoreRevision,
		)
		if published {
			break
		}
		if cfg.AccountsSnapshotFunc != nil {
			currentAccounts, currentGeneration := cfg.AccountsSnapshotFunc()
			currentKey := codexScoreGenerationKey(currentGeneration, autoSwitchOAuthAccounts(currentAccounts))
			if currentKey != generationKey {
				lastConflict = "account pool changed"
				fetchedGenerationKey = ""
			} else {
				lastConflict = "scheduler score snapshot changed"
			}
		}
		if attempt == srAutoSwitchPublishAttempts {
			return "", fmt.Errorf(
				"sr auto-switch could not publish fresh usage after %d attempts because the %s concurrently",
				srAutoSwitchPublishAttempts, lastConflict,
			)
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}
	// The per-account score lines are the operator's only steady-state view of
	// what the scheduler believes; a wall of zeros here is how the
	// provider-blind sweep was eventually diagnosed.
	for _, score := range scores {
		logSRAutoSwitch(cfg.Logger, slog.LevelInfo, "account score",
			"account", score.AccountID, "provider", score.Provider,
			"headroom", score.Headroom, "fresh", score.Fresh)
	}
	if cfg.ScoreSnapshots != nil {
		// The shared snapshot is the startup coordinator's Codex-only
		// contract (ensureStartupScores keys it on the Codex OAuth set), so
		// publish exactly that slice under that key. A failed write costs a
		// successor worker one duplicate startup sweep; it must not cost this
		// sweep its Claude rotation, so it is logged rather than returned.
		if err := publishCodexScoreSnapshot(cfg.ScoreSnapshots, candidates, scores); err != nil {
			logSRAutoSwitch(cfg.Logger, slog.LevelWarn, "sr auto-switch could not publish shared usage score snapshot", "error", err)
		}
	}
	if cfg.SchedulerRef != nil {
		// Rank what the ROUTER sees, not the raw snapshot just published.
		// Request-time exhaustion -- an upstream 429 carrying
		// usage_limit_reached -- is held as an overlay inside SchedulerRef and
		// applied only by Get(). Ranking the bare NewScheduler(scores) made
		// every such mark invisible to the rotation, so an account the request
		// path had just been refused by was still made the interactive
		// default, once per interval, for as long as its usage snapshot kept
		// reading healthy.
		scheduler = cfg.SchedulerRef.Get()
	}
	if cfg.Sessions != nil {
		// Provider-scoped keys: a Codex account and a Claude profile may share
		// an email, and bare-ID counts would let one damp the other's pick.
		scheduler = scheduler.WithSessionCounts(proxy.SchedulerSessionCounts(cfg.Sessions))
	}

	// Scoring covers both providers, and each provider picks its own active
	// slot independently: one provider's exhausted pool or failed switch must
	// not cost the other provider its rotation.
	//
	// Codex is read-only unless an explicit SwitchActive hook is configured:
	// the pick is reported, never written into ~/.codex/auth.json (see the
	// SwitchActive field). An explicit `sr switch` / `sr g` is the only path
	// that exports a pooled account to the Codex app, and it demotes that
	// account out of serving when it does.
	//
	// Claude rotates the store's active profile -- the same move `sr claude
	// pick` performs by hand -- so direct `sr claude` use always defaults to
	// the account with the most headroom.
	switchClaude := cfg.SwitchActiveClaude
	if switchClaude == nil {
		switchClaude = func(_ context.Context, accountID string) error {
			// The serving process's own view of the store: DefaultStore would
			// run the account manager's one-time legacy import from inside the
			// daemon, which serve deliberately never does.
			store := claude.DefaultStoreForReadOnlyInspection()
			if store.ActiveProfile() == accountID {
				return nil
			}
			return store.SetActiveProfile(accountID)
		}
	}

	rotate := func(provider string, providerCandidates []accounts.Account, switchFn func(context.Context, string) error) (string, error) {
		// PickSteady, not Pick: rotation chooses the long-lived interactive
		// default (active Claude profile; the reported Codex pick), and the
		// endgame drain widening is for disposable request routing — parking
		// the user's next `sr claude launch` on an account being deliberately
		// run into the ground would be the opposite of a good default.
		picked, err := scheduler.PickSteady(providerCandidates)
		if err != nil {
			return "", err
		}
		if !scheduler.UsableForNewSession(picked.Provider, picked.ID) {
			if scheduler.Exhausted(picked.Provider, picked.ID) {
				return "", fmt.Errorf("no usable OAuth %s accounts available", provider)
			}
			logSRAutoSwitch(cfg.Logger, slog.LevelWarn, "sr auto-switch selected an account the router would not start a new session on", "account", picked.ID)
		}
		if switchFn != nil {
			if err := switchFn(ctx, picked.ID); err != nil {
				return "", err
			}
		}
		return picked.ID, nil
	}

	identityOnly := ""
	if cfg.ClaudeIdentityOnly != nil {
		identityOnly = cfg.ClaudeIdentityOnly()
	}
	var codexCandidates, claudeCandidates []accounts.Account
	for _, account := range candidates {
		if cfg.PinOnly != nil && cfg.PinOnly(account.ID) {
			continue
		}
		switch account.Provider {
		case "", accounts.ProviderCodex:
			codexCandidates = append(codexCandidates, account)
		case accounts.ProviderClaude:
			if identityOnly != "" && account.ID == identityOnly {
				continue
			}
			claudeCandidates = append(claudeCandidates, account)
		}
	}

	var picks []string
	var rotationErrs []error
	if len(codexCandidates) > 0 {
		if picked, err := rotate("codex", codexCandidates, cfg.SwitchActive); err != nil {
			rotationErrs = append(rotationErrs, fmt.Errorf("codex: %w", err))
		} else {
			picks = append(picks, picked)
		}
	}
	if len(claudeCandidates) > 0 {
		if picked, err := rotate("claude", claudeCandidates, switchClaude); err != nil {
			rotationErrs = append(rotationErrs, fmt.Errorf("claude: %w", err))
		} else {
			picks = append(picks, picked)
		}
	}
	return strings.Join(picks, ", "), errors.Join(rotationErrs...)
}

// publishCodexScoreSnapshot writes the Codex slice of a sweep to the shared
// startup snapshot, keyed exactly as ensureStartupScores reads it. A sweep with
// no Codex OAuth accounts has nothing that coordinator could load.
func publishCodexScoreSnapshot(store *srUsageScoreStore, candidates []accounts.Account, scores []selectacct.Score) error {
	codexCandidates := codexOAuthAccounts(candidates)
	if len(codexCandidates) == 0 {
		return nil
	}
	codexScores := make([]selectacct.Score, 0, len(codexCandidates))
	for _, score := range scores {
		if score.Provider == "" || score.Provider == accounts.ProviderCodex {
			codexScores = append(codexScores, score)
		}
	}
	return store.write(srUsageScoreSnapshot{
		SchemaVersion: scoreSnapshotSchemaVersion,
		// The Codex key ignores the account generation, so the zero is exact.
		GenerationKey: codexScoreGenerationKey(0, codexCandidates),
		FetchedAt:     time.Now().UTC(),
		Scores:        codexScores,
	})
}

// autoSwitchOAuthAccounts is the sweep's candidate set: OAuth accounts of the
// two providers that have an interactive active slot (Codex, including legacy
// provider-less records, and Claude). Keyed and credential-only providers
// (Kimi, Grok, Antigravity, ...) are scored by the proxy's own refresh and
// left untouched here.
func autoSwitchOAuthAccounts(all []accounts.Account) []accounts.Account {
	out := make([]accounts.Account, 0, len(all))
	for _, account := range all {
		if account.AuthMode != accounts.AuthModeOAuth {
			continue
		}
		switch account.Provider {
		case "", accounts.ProviderCodex, accounts.ProviderClaude:
			out = append(out, account)
		}
	}
	return out
}

// codexOAuthAccounts is the startup score coordinator's candidate set
// (sr_startup_scores.go) and the readiness predicate's.
func codexOAuthAccounts(all []accounts.Account) []accounts.Account {
	out := make([]accounts.Account, 0, len(all))
	for _, account := range all {
		if account.AuthMode == accounts.AuthModeOAuth &&
			(account.Provider == "" || account.Provider == accounts.ProviderCodex) {
			out = append(out, account)
		}
	}
	return out
}

func logSRAutoSwitch(logger *slog.Logger, level slog.Level, message string, args ...any) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.Log(context.Background(), level, message, args...)
}
