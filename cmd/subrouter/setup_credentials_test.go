package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/broker"
)

// A first run must not hand OAuth refresh tokens to a remote vault. The upload
// happens before the user has approved anything and nothing afterwards can call
// those tokens back, so the safe answer is the only acceptable default.
func TestFreshMachineKeepsCredentialsOnThisComputer(t *testing.T) {
	got, err := resolveSetupCredentialSource(broker.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != broker.CredentialSourceLocal {
		t.Fatalf("fresh machine selected %q, want %q", got, broker.CredentialSourceLocal)
	}
}

// Choosing the hosted vault stays available; it just has to be asked for.
func TestExplicitStorageFlagStillReachesEverySource(t *testing.T) {
	unlockRemoteGateForTest(t)
	for flag, want := range map[string]broker.CredentialSource{
		"local":  broker.CredentialSourceLocal,
		"hosted": broker.CredentialSourceHosted,
		"team":   broker.CredentialSourceTeam,
	} {
		got, err := resolveSetupCredentialSource(broker.Config{}, flag)
		if err != nil {
			t.Fatalf("--storage %s: %v", flag, err)
		}
		if got != want {
			t.Errorf("--storage %s selected %q, want %q", flag, got, want)
		}
	}
}

// Re-running setup on a configured machine must not quietly move credentials.
func TestConfiguredMachineKeepsItsRecordedSource(t *testing.T) {
	for _, source := range []broker.CredentialSource{
		broker.CredentialSourceHosted,
		broker.CredentialSourceTeam,
		broker.CredentialSourceLocal,
		broker.CredentialSourceLegacy,
	} {
		got, err := resolveSetupCredentialSource(broker.Config{CredentialSource: source}, "")
		if err != nil {
			t.Fatal(err)
		}
		if got != source {
			t.Errorf("configured %q became %q", source, got)
		}
	}
}

// A machine mid-cmux-login keeps the team behavior its existing session implies,
// so the new local default cannot strand someone who already signed in.
func TestSignedInMachineWithoutAnExplicitSourceStaysOnTeam(t *testing.T) {
	config := broker.Config{
		AccessToken:  "access",
		RefreshToken: "refresh",
		TeamID:       "team",
	}
	got, err := resolveSetupCredentialSource(config, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != broker.CredentialSourceTeam {
		t.Fatalf("signed-in machine selected %q, want %q", got, broker.CredentialSourceTeam)
	}
}

func TestUnknownStorageValueIsRejected(t *testing.T) {
	if _, err := resolveSetupCredentialSource(broker.Config{}, "elsewhere"); err == nil {
		t.Fatal("an unknown --storage value was accepted")
	}
}

// The review screen is the thing a user actually approves, so the one decision
// that can move credentials off this machine has to be visible on it.
func TestReviewScreenNamesTheCredentialDestination(t *testing.T) {
	for _, testCase := range []struct {
		source broker.CredentialSource
		want   string
	}{
		{broker.CredentialSourceLocal, "Keep credentials on this computer only"},
		{broker.CredentialSourceHosted, "Store credentials in the cmux.com hosted vault"},
		{broker.CredentialSourceTeam, "Lease credentials from the cmux.com team vault"},
		{broker.CredentialSourceLegacy, "Use credentials on the selected remote Subrouter"},
	} {
		input := basePlanInput()
		input.CredentialSource = testCase.source
		var out bytes.Buffer
		renderSetupPlan(buildSetupPlan(input), &out)
		if !strings.Contains(out.String(), testCase.want) {
			t.Errorf("%s: review screen missing %q\n%s", testCase.source, testCase.want, out.String())
		}
	}
}

// "Stays on this machine" is only checkable if the user is told where to look.
func TestLocalCredentialDetailNamesTheStoreDirectory(t *testing.T) {
	input := basePlanInput()
	input.CredentialSource = broker.CredentialSourceLocal
	input.CredentialDir = "/store/dir"
	var out bytes.Buffer
	renderSetupDetails(buildSetupPlan(input), &out)
	screen := out.String()
	for _, want := range []string{"/store/dir", "never uploaded"} {
		if !strings.Contains(screen, want) {
			t.Errorf("details missing %q\n%s", want, screen)
		}
	}
}

// Credential storage is --storage's job. A togglable line would imply the review
// screen can undo an upload, and it cannot.
func TestCredentialStorageIsNotAKeystrokeToggle(t *testing.T) {
	plan := buildSetupPlan(basePlanInput())
	if plan.toggle(actionCredentials) {
		t.Fatal("credential storage was togglable from the review screen")
	}
	item, ok := plan.find(actionCredentials)
	if !ok {
		t.Fatal("plan has no credential storage item")
	}
	if item.Optional {
		t.Error("credential storage is marked optional")
	}
	if !item.Selected {
		t.Error("credential storage is not shown as selected")
	}
}

// Moving credentials is a change and must render as one, so a re-run that keeps
// the existing answer still reports nothing to do.
func TestCredentialItemMarksOnlyAnActualMove(t *testing.T) {
	unchanged := basePlanInput()
	unchanged.CredentialSource = broker.CredentialSourceLocal
	unchanged.CurrentCredentialSource = broker.CredentialSourceLocal
	item, _ := buildSetupPlan(unchanged).find(actionCredentials)
	if item.Change != "=" {
		t.Errorf("unchanged storage rendered as %q, want %q", item.Change, "=")
	}

	moved := basePlanInput()
	moved.CredentialSource = broker.CredentialSourceHosted
	moved.CurrentCredentialSource = broker.CredentialSourceLocal
	item, _ = buildSetupPlan(moved).find(actionCredentials)
	if item.Change == "=" {
		t.Error("a move to the hosted vault rendered as no change")
	}
}
