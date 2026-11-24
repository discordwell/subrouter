package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/discordwell/subrouter/internal/broker"
)

func emptyPoolInput() setupPlanInput {
	in := basePlanInput()
	in.CredentialSource = broker.CredentialSourceLocal
	in.LocalCodexAccounts = 0
	in.LocalClaudeAccounts = 0
	in.TeamModeReady = false
	return in
}

// Pointing an agent at an empty pool does not degrade, it fails every request:
// the client stops talking to the provider and the daemon has nothing to
// authenticate with. Setup must install the daemon and stop there.
func TestEmptyLocalPoolDoesNotReconfigureClients(t *testing.T) {
	plan := buildSetupPlan(emptyPoolInput())
	for _, action := range []setupAction{actionConfigCodex, actionConfigClaude} {
		if plan.selected(action) {
			t.Errorf("%s was applied against an empty account pool", action)
		}
	}
	// The daemon itself is still worth installing; only the traffic is deferred.
	if !plan.selected(actionBackground) {
		t.Error("an empty pool also blocked the daemon install")
	}
}

// One account is enough to make routing useful, so the block must lift on its
// own once the pool is non-empty.
func TestOneAccountUnblocksClientConfiguration(t *testing.T) {
	in := emptyPoolInput()
	in.LocalCodexAccounts = 1
	in.LocalClaudeAccounts = 1
	plan := buildSetupPlan(in)
	for _, action := range []setupAction{actionConfigCodex, actionConfigClaude} {
		if !plan.selected(action) {
			t.Errorf("%s stayed blocked with an account available", action)
		}
	}
}

// Team and hosted pools live in the vault, so their gate is the login rather
// than the local store.
func TestVaultSourcesGateOnTheLoginNotTheLocalStore(t *testing.T) {
	for _, source := range []broker.CredentialSource{
		broker.CredentialSourceTeam,
		broker.CredentialSourceHosted,
	} {
		blocked := emptyPoolInput()
		blocked.CredentialSource = source
		if buildSetupPlan(blocked).selected(actionConfigClaude) {
			t.Errorf("%s configured clients before the vault was ready", source)
		}

		ready := blocked
		ready.TeamModeReady = true
		if !buildSetupPlan(ready).selected(actionConfigClaude) {
			t.Errorf("%s stayed blocked with a ready vault", source)
		}
	}
}

// A dash with no reason reads as "you turned this off" and sends someone
// hunting for the toggle that switches it back on.
func TestBlockedItemsExplainThemselvesOnScreen(t *testing.T) {
	var out bytes.Buffer
	renderSetupPlan(buildSetupPlan(emptyPoolInput()), &out)
	screen := out.String()
	// The reason names the provider, because the two pools are independent:
	// "waiting for an account" leaves someone guessing which login to add.
	for _, want := range []string{"waiting for a Codex account", "waiting for a Claude account"} {
		if !strings.Contains(screen, want) {
			t.Errorf("review screen missing %q:\n%s", want, screen)
		}
	}
	if !strings.Contains(screen, "sr add") {
		t.Errorf("review screen did not name the unblocking command:\n%s", screen)
	}
}

// Claude Code's proxy env overrides an existing claude.ai login, which is not
// recoverable by guessing, so the details view has to say it.
func TestClaudeDetailWarnsThatItOverridesAnExistingLogin(t *testing.T) {
	in := emptyPoolInput()
	in.LocalCodexAccounts = 1
	in.LocalClaudeAccounts = 1
	var out bytes.Buffer
	renderSetupDetails(buildSetupPlan(in), &out)
	if !strings.Contains(out.String(), "overrides an existing claude.ai login") {
		t.Errorf("details did not warn about the login override:\n%s", out.String())
	}
}

// A blocked item must not be togglable, or the review screen would offer to
// apply something the apply path refuses to run.
func TestBlockedItemsCannotBeToggledOn(t *testing.T) {
	plan := buildSetupPlan(emptyPoolInput())
	if plan.toggle(actionConfigClaude) {
		t.Fatal("a blocked item accepted a toggle")
	}
	if plan.selected(actionConfigClaude) {
		t.Fatal("a blocked item became selected")
	}
	var out bytes.Buffer
	renderSetupOptions(plan, &out)
	if !strings.Contains(out.String(), "waiting for a Claude account") {
		t.Errorf("options screen did not explain the blocked item:\n%s", out.String())
	}
}

// With only blocked work left, setup must not claim there is something to apply.
func TestPlanWithOnlyBlockedWorkIsNotPending(t *testing.T) {
	in := emptyPoolInput()
	in.DaemonInstalled = true
	in.CredentialSource = broker.CredentialSourceLocal
	in.CurrentCredentialSource = broker.CredentialSourceLocal
	plan := buildSetupPlan(in)
	if plan.pending() {
		t.Error("a plan whose only remaining work is blocked reported as pending")
	}
	var out bytes.Buffer
	renderSetupPlan(plan, &out)
	if !strings.Contains(out.String(), "Nothing to do") {
		t.Errorf("screen offered to apply blocked-only work:\n%s", out.String())
	}
}
