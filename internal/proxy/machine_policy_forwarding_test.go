package proxy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/manaflow-ai/subrouter/account"
)

// A FORWARDING daemon calls the provider itself, so every client's traffic
// leaves from one IP and the provider sees one machine per account however many
// clients there are. The machine ID this policy records is clientRemoteIP -- an
// internal tailnet address the provider never observes -- so the binding table
// counts something that does not exist upstream.
//
// Measured on the live box before this gate: 1728 consecutive Claude requests,
// every one `upstream=api.anthropic.com` from `remote_addr=100.64.10.3`, and
// zero leases issued. One machine, cap of six, and yet "this machine is already
// bound to it" outranked an account inside its weekly roll window -- so budget
// that expires either way lost to inertia, circularly, because an account is
// bound precisely for having been picked before.
//
// Gated rather than deleted: under leasing the borrowing machine calls the
// provider directly and IS a distinct source IP, and then the cap protects
// something real.

func TestForwardingDaemonHasNoMachinePolicy(t *testing.T) {
	for _, mode := range []MachinePolicyMode{MachinePolicyObserve, MachinePolicyEnforce} {
		t.Run(string(mode), func(t *testing.T) {
			policy := NewMachinePolicy(t.TempDir(), mode, false)
			if policy.Active() {
				t.Fatalf("policy is Active on a forwarding daemon in mode %q: the binding "+
					"preference re-arms and ranks inertia above an expiring weekly window", mode)
			}
		})
	}
}

// Leasing is the case the cap was written for, and it must still arm.
func TestLeasingDaemonKeepsMachinePolicy(t *testing.T) {
	policy := NewMachinePolicy(t.TempDir(), MachinePolicyEnforce, true)
	if !policy.Active() {
		t.Fatal("policy is inert while issuing credentials: borrowing machines are real " +
			"distinct source IPs and the per-account cap must apply to them")
	}
}

// Off stays off regardless -- the gate narrows the policy, it must never widen it.
func TestOffStaysOffEvenWhenIssuingCredentials(t *testing.T) {
	policy := NewMachinePolicy(t.TempDir(), MachinePolicyOff, true)
	if policy.Active() {
		t.Fatal("explicitly disabled policy became active")
	}
}

// A forwarding daemon must not accumulate bindings either. A written table
// looks authoritative, silently re-arms the preference the moment anything
// consults it, and nothing prunes the file.
func TestForwardingDaemonWritesNoBindings(t *testing.T) {
	dir := t.TempDir()
	policy := NewMachinePolicy(dir, MachinePolicyEnforce, false)

	if isNew := policy.Note(account.ProviderClaude, "acct-p", "100.64.10.3"); isNew {
		t.Fatal("Note reported a new binding on a forwarding daemon")
	}
	if got := policy.Bindings(account.ProviderClaude, "acct-p"); len(got) != 0 {
		t.Fatalf("bindings recorded on a forwarding daemon: %v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "machine-bindings.json")); !os.IsNotExist(err) {
		t.Fatalf("machine-bindings.json was written by a forwarding daemon (stat err = %v)", err)
	}
}

// The same call on a leasing daemon must still record, or the gate has broken
// the mechanism instead of scoping it.
func TestLeasingDaemonStillRecordsBindings(t *testing.T) {
	dir := t.TempDir()
	policy := NewMachinePolicy(dir, MachinePolicyEnforce, true)

	if isNew := policy.Note(account.ProviderClaude, "acct-p", "100.64.10.3"); !isNew {
		t.Fatal("Note did not report a new binding while issuing credentials")
	}
	if got := policy.Bindings(account.ProviderClaude, "acct-p"); len(got) != 1 {
		t.Fatalf("bindings = %v, want exactly the one machine", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "machine-bindings.json")); err != nil {
		t.Fatalf("a new binding was not persisted: %v", err)
	}
}

// Every scheduler the daemon builds must honour the gate, not just the routing
// one. The board passes the viewer's IP unconditionally, so if the gate lived
// at the call sites instead of in the constructor, the board would rank on
// bindings the router no longer consults -- which is what shipped for one
// deploy and put "this machine is already bound to it" back at rank 2 on a
// forwarding daemon.
func TestForwardingDaemonDropsMachineIDInEverySchedulerItBuilds(t *testing.T) {
	server := Server{MachinePolicy: NewMachinePolicy(t.TempDir(), MachinePolicyEnforce, false)}
	if server.selectionScheduler("", "100.64.10.3").MachineActive() {
		t.Fatal("a machine ID passed by a caller survived onto a forwarding daemon's scheduler")
	}
}

func TestLeasingDaemonKeepsMachineIDInTheScheduler(t *testing.T) {
	server := Server{MachinePolicy: NewMachinePolicy(t.TempDir(), MachinePolicyEnforce, true)}
	if !server.selectionScheduler("", "100.64.10.3").MachineActive() {
		t.Fatal("machine term dropped while issuing credentials; the cap no longer applies")
	}
}
