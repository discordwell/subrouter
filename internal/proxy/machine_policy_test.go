package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func TestNoteCreatesABindingOnceAndRenewsAfter(t *testing.T) {
	policy := NewMachinePolicy(t.TempDir(), MachinePolicyEnforce, true)

	if isNew := policy.Note(account.ProviderClaude, "acct", "m1"); !isNew {
		t.Fatal("first use of an account by a machine must register a new binding")
	}
	if isNew := policy.Note(account.ProviderClaude, "acct", "m1"); isNew {
		t.Fatal("a machine already bound must not consume a second slot")
	}
	if got := policy.Bindings(account.ProviderClaude, "acct"); len(got) != 1 {
		t.Fatalf("bindings = %v, want exactly one", got)
	}
}

// The same bare ID can name one account per provider, so bindings must be keyed
// by provider as well — otherwise a Codex account silently consumes a Claude
// account's slots.
func TestBindingsAreScopedByProvider(t *testing.T) {
	policy := NewMachinePolicy(t.TempDir(), MachinePolicyEnforce, true)
	policy.Note(account.ProviderClaude, "shared-id", "m1")

	if got := policy.Bindings(account.ProviderCodex, "shared-id"); len(got) != 0 {
		t.Fatalf("codex bindings = %v, want none: the claude binding must not leak across providers", got)
	}
}

func TestLeaseHolderExpiresAfterIdleTTL(t *testing.T) {
	policy := NewMachinePolicy(t.TempDir(), MachinePolicyEnforce, true)
	policy.Note(account.ProviderClaude, "acct", "m1")

	if holder, held := policy.LeaseHolder(account.ProviderClaude, "acct"); !held || holder != "m1" {
		t.Fatalf("holder = %q held = %v, want m1 held", holder, held)
	}

	// Age the lease past the TTL.
	key := selectacct.ScoreKey(account.ProviderClaude, "acct")
	policy.mu.Lock()
	lease := policy.leases[key]
	lease.LastSeen = time.Now().Add(-leaseIdleTTL - time.Minute)
	policy.leases[key] = lease
	policy.mu.Unlock()

	if _, held := policy.LeaseHolder(account.ProviderClaude, "acct"); held {
		t.Fatal("an idle lease must expire, or a crashed machine parks an account forever")
	}
	// The binding survives the lease: they have different lifetimes.
	if got := policy.Bindings(account.ProviderClaude, "acct"); len(got) != 1 {
		t.Fatalf("bindings = %v, want the binding to outlive the lease", got)
	}
}

// Without pruning the cap ratchets: slots spent months ago never return and the
// pool eventually refuses everything.
func TestBindingsOutsideTheWindowStopCounting(t *testing.T) {
	policy := NewMachinePolicy(t.TempDir(), MachinePolicyEnforce, true)
	policy.Note(account.ProviderClaude, "acct", "old")
	policy.Note(account.ProviderClaude, "acct", "recent")

	key := selectacct.ScoreKey(account.ProviderClaude, "acct")
	policy.mu.Lock()
	for i := range policy.bindings[key] {
		if policy.bindings[key][i].MachineID == "old" {
			policy.bindings[key][i].LastSeen = time.Now().Add(-bindingWindow - time.Hour)
		}
	}
	policy.mu.Unlock()

	got := policy.Bindings(account.ProviderClaude, "acct")
	if len(got) != 1 || got[0] != "recent" {
		t.Fatalf("bindings = %v, want only \"recent\": the stale slot must be reclaimed", got)
	}
}

func TestBindingsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	first := NewMachinePolicy(dir, MachinePolicyEnforce, true)
	first.Note(account.ProviderClaude, "acct", "m1")

	second := NewMachinePolicy(dir, MachinePolicyEnforce, true)
	if got := second.Bindings(account.ProviderClaude, "acct"); len(got) != 1 || got[0] != "m1" {
		t.Fatalf("after reload bindings = %v, want [m1]: a restart must not silently free a slot", got)
	}
}

// Fail open, every way it can fail.
func TestCorruptStoreIsPermissiveNotDenying(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "machine-bindings.json"), []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := NewMachinePolicy(dir, MachinePolicyEnforce, true)
	if got := policy.Bindings(account.ProviderClaude, "acct"); len(got) != 0 {
		t.Fatalf("bindings = %v, want none — a corrupt file must read as no constraint", got)
	}
	if _, held := policy.LeaseHolder(account.ProviderClaude, "acct"); held {
		t.Fatal("a corrupt file must not manufacture a lease")
	}
}

func TestNilAndOffPolicyImposeNothing(t *testing.T) {
	var nilPolicy *MachinePolicy
	if got := nilPolicy.Bindings(account.ProviderClaude, "acct"); got != nil {
		t.Fatal("a nil policy must impose no bindings")
	}
	if _, held := nilPolicy.LeaseHolder(account.ProviderClaude, "acct"); held {
		t.Fatal("a nil policy must impose no lease")
	}
	if nilPolicy.Active() || nilPolicy.Enforcing() {
		t.Fatal("a nil policy is neither active nor enforcing")
	}

	off := NewMachinePolicy(t.TempDir(), MachinePolicyOff, true)
	off.Note(account.ProviderClaude, "acct", "m1")
	if got := off.Bindings(account.ProviderClaude, "acct"); len(got) != 0 {
		t.Fatalf("bindings = %v, want none when the policy is off", got)
	}
}

// Observe mode must record everything and refuse nothing, or the observation
// week is not measuring what enforcement would do.
func TestObserveModeRecordsButDoesNotEnforce(t *testing.T) {
	policy := NewMachinePolicy(t.TempDir(), MachinePolicyObserve, true)
	policy.Note(account.ProviderClaude, "acct", "m1")

	if !policy.Active() {
		t.Fatal("observe mode must be active")
	}
	if policy.Enforcing() {
		t.Fatal("observe mode must not enforce")
	}
	if got := policy.Bindings(account.ProviderClaude, "acct"); len(got) != 1 {
		t.Fatalf("bindings = %v, want observe mode to still record", got)
	}
}

// The scheduler's cap check and this store must agree on what "full" means.
func TestCapBoundaryMatchesTheScheduler(t *testing.T) {
	policy := NewMachinePolicy(t.TempDir(), MachinePolicyEnforce, true)
	for i := 0; i < selectacct.MaxMachinesPerAccount; i++ {
		policy.Note(account.ProviderClaude, "acct", string(rune('a'+i)))
	}
	// Isolate the cap from the lease: the last Note left one machine holding
	// the lease, and a lease refusal would otherwise be mistaken for a cap
	// refusal. They are separate rules with separate lifetimes.
	key := selectacct.ScoreKey(account.ProviderClaude, "acct")
	policy.mu.Lock()
	delete(policy.leases, key)
	policy.mu.Unlock()

	view := selectacct.Scheduler{}.WithMachine("newcomer", policy)
	reason, refused := view.MachineRefusal(account.Account{ID: "acct", Provider: account.ProviderClaude})
	if !refused {
		t.Fatalf("an account at %d machines must refuse a newcomer", selectacct.MaxMachinesPerAccount)
	}
	if !strings.Contains(reason, "rolling window") {
		t.Fatalf("refusal reason %q should name the cap, not some other rule", reason)
	}

	held := selectacct.Scheduler{}.WithMachine("a", policy)
	if _, refused := held.MachineRefusal(account.Account{ID: "acct", Provider: account.ProviderClaude}); refused {
		t.Fatal("a machine already holding a slot must still be admitted at the cap")
	}
}
