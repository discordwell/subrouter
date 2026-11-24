package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/discordwell/subrouter/account"
	"github.com/discordwell/subrouter/selectacct"
)

// Machine-sharing policy: how many distinct machines may use one account, and
// how many may use it at once.
//
// A pooled subscription reached by many machines simultaneously is the pattern
// that reads as account sharing. Two rules address it:
//
//   - a rolling-window cap on distinct machines per account
//     (selectacct.MaxMachinesPerAccount)
//   - one machine per account at a time
//
// A machine may hold many accounts at once; that is ordinary parallelism and is
// explicitly allowed. It is concurrent MULTI-MACHINE use of a single account
// that this prevents.
//
// PERFORMANCE CONSTRAINT, learned the hard way in this codebase: nothing here
// may touch the filesystem on the request path. `.WithLiveSessionCounts()`
// added a flock plus a full re-parse of sessions.json to every proxied request,
// and that store is never pruned, so it degrades as it grows. Bindings are held
// in memory and written only when a genuinely new machine appears; leases are
// in memory and never written at all.

// MachinePolicyMode selects how far the policy goes.
type MachinePolicyMode string

const (
	// MachinePolicyOff disables machine accounting entirely.
	MachinePolicyOff MachinePolicyMode = "off"
	// MachinePolicyObserve evaluates and logs decisions but routes as before.
	// This is the default: arming enforcement without first reading a week of
	// real decisions is how a too-short idle TTL becomes an outage.
	MachinePolicyObserve MachinePolicyMode = "observe"
	// MachinePolicyEnforce refuses routes the policy rejects.
	MachinePolicyEnforce MachinePolicyMode = "enforce"
)

const (
	// bindingWindow is the rolling period over which distinct machines count.
	bindingWindow = 30 * 24 * time.Hour
	// leaseIdleTTL is how long an account stays reserved to a machine after
	// its last request. The only value here that can manufacture a refusal:
	// too short loses an account mid-conversation, too long parks one behind a
	// crashed client.
	leaseIdleTTL = 10 * time.Minute
	// bindingFlushInterval bounds how often a last-seen refresh reaches disk.
	// New bindings are written immediately; keeping an existing binding warm is
	// not worth a write per request.
	bindingFlushInterval = 5 * time.Minute
)

type machineBinding struct {
	MachineID string    `json:"machineId"`
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
}

// MachinePolicy implements selectacct.MachineView over a binding file and an
// in-memory lease table.
//
// Every read path is best-effort: a fault yields "no constraint" rather than
// "deny". A bookkeeping bug must never be able to refuse traffic — the router
// refusing while every account has quota is already a known failure in this
// project's history.
type MachinePolicy struct {
	Mode MachinePolicyMode

	// issuesCredentials records whether this daemon can hand a credential to
	// another machine. It gates the whole policy, and that gate is the point:
	// see NewMachinePolicy.
	issuesCredentials bool

	path string

	mu        sync.Mutex
	bindings  map[string][]machineBinding // selectacct.ScoreKey -> bindings
	leases    map[string]machineBinding   // selectacct.ScoreKey -> current holder
	dirty     bool
	lastFlush time.Time
}

// NewMachinePolicy loads (or starts) the binding store beside the other daemon
// state. A load failure is not fatal: the policy starts empty and permissive,
// which keeps a corrupt file from taking the pool down.
//
// issuesCredentials must be true only where a credential can actually LEAVE
// this daemon -- today, multi-tenant lease issuance. The whole policy is inert
// otherwise, and that is a correctness requirement rather than an optimisation.
//
// A forwarding daemon calls the provider itself: every client's traffic egresses
// from ONE IP, so the provider sees one machine per account no matter how many
// clients there are. The machine ID recorded here is then clientRemoteIP -- an
// internal tailnet address the provider never observes -- and the binding table
// counts something that does not exist upstream. Measured on the live box: 1728
// consecutive Claude requests, all `upstream=api.anthropic.com`, all
// `remote_addr=100.64.10.3`, zero leases issued.
//
// That mattered because bindings are not merely bookkeeping -- being bound is a
// RANKING PREFERENCE. With one machine and a cap of six, no binding could ever
// approach the limit it exists to protect, yet "this machine is already bound to
// it" still outranked an account inside its weekly roll window: budget that
// expires either way lost to inertia, and lost to it circularly, since an
// account is bound precisely because it was picked before.
//
// Under leasing the premise holds again: the borrowing machine calls the
// provider directly and IS a distinct source IP, which is why this is gated
// rather than deleted.
func NewMachinePolicy(stateDir string, mode MachinePolicyMode, issuesCredentials bool) *MachinePolicy {
	policy := &MachinePolicy{
		Mode:              mode,
		issuesCredentials: issuesCredentials,
		path:              filepath.Join(stateDir, "machine-bindings.json"),
		bindings:          map[string][]machineBinding{},
		leases:            map[string]machineBinding{},
	}
	policy.loadBestEffort()
	return policy
}

func (p *MachinePolicy) loadBestEffort() {
	body, err := os.ReadFile(p.path)
	if err != nil {
		return
	}
	var stored map[string][]machineBinding
	if err := json.Unmarshal(body, &stored); err != nil {
		return
	}
	p.bindings = stored
	p.pruneLocked(time.Now())
}

// pruneLocked drops bindings whose last use falls outside the window. Without
// this the cap ratchets: slots consumed months ago would never come back and
// the pool would slowly refuse everything.
func (p *MachinePolicy) pruneLocked(now time.Time) {
	cutoff := now.Add(-bindingWindow)
	for key, list := range p.bindings {
		kept := list[:0]
		for _, binding := range list {
			if binding.LastSeen.After(cutoff) {
				kept = append(kept, binding)
			}
		}
		if len(kept) == 0 {
			delete(p.bindings, key)
			continue
		}
		p.bindings[key] = kept
	}
}

// Bindings implements selectacct.MachineView.
func (p *MachinePolicy) Bindings(provider account.Provider, accountID string) []string {
	if p == nil || p.Mode == MachinePolicyOff {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-bindingWindow)
	var out []string
	for _, binding := range p.bindings[selectacct.ScoreKey(provider, accountID)] {
		if binding.LastSeen.After(cutoff) {
			out = append(out, binding.MachineID)
		}
	}
	sort.Strings(out)
	return out
}

// LeaseHolder implements selectacct.MachineView. Expiry is lazy: an expired
// lease simply reports no holder, so a crashed machine releases its accounts
// without anyone sweeping.
func (p *MachinePolicy) LeaseHolder(provider account.Provider, accountID string) (string, bool) {
	if p == nil || p.Mode == MachinePolicyOff {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	lease, ok := p.leases[selectacct.ScoreKey(provider, accountID)]
	if !ok || time.Since(lease.LastSeen) > leaseIdleTTL {
		return "", false
	}
	return lease.MachineID, true
}

// Note records that machineID actually used this account: it renews the lease
// and refreshes (or creates) the binding. Call it AFTER a route is chosen, so
// the stores describe real usage rather than considered options.
//
// Returns true when this created a new binding — the caller logs those, because
// each one spends a slot for a month and they should be rare enough to read.
func (p *MachinePolicy) Note(provider account.Provider, accountID, machineID string) bool {
	// Active(), not just the mode: a daemon that cannot hand out credentials
	// must not accumulate bindings either. Writing them would leave a table
	// that looks authoritative, silently re-arms the preference the moment
	// anything consults it, and grows a file nothing prunes.
	if !p.Active() || machineID == "" {
		return false
	}
	key := selectacct.ScoreKey(provider, accountID)
	now := time.Now()

	p.mu.Lock()
	defer p.mu.Unlock()

	p.leases[key] = machineBinding{MachineID: machineID, LastSeen: now}

	list := p.bindings[key]
	for i := range list {
		if list[i].MachineID == machineID {
			list[i].LastSeen = now
			p.bindings[key] = list
			p.dirty = true
			p.flushIfDueLocked(now)
			return false
		}
	}
	p.bindings[key] = append(list, machineBinding{MachineID: machineID, FirstSeen: now, LastSeen: now})
	p.dirty = true
	// A new binding is the expensive event; persist it immediately rather than
	// waiting for the throttle, so a restart cannot silently free a slot.
	p.saveLocked(now)
	return true
}

func (p *MachinePolicy) flushIfDueLocked(now time.Time) {
	if !p.dirty || now.Sub(p.lastFlush) < bindingFlushInterval {
		return
	}
	p.saveLocked(now)
}

// saveLocked writes atomically. Only the daemon writes this file, so a mutex is
// sufficient and no flock is needed — unlike sessions.json, which the CLI also
// touches.
func (p *MachinePolicy) saveLocked(now time.Time) {
	p.pruneLocked(now)
	body, err := json.MarshalIndent(p.bindings, "", "  ")
	if err != nil {
		return
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, p.path); err != nil {
		return
	}
	p.dirty = false
	p.lastFlush = now
}

// Enforcing reports whether refusals should actually take effect.
func (p *MachinePolicy) Enforcing() bool {
	return p != nil && p.Mode == MachinePolicyEnforce
}

// Active reports whether the policy should be consulted at all.
func (p *MachinePolicy) Active() bool {
	return p != nil && p.Mode != MachinePolicyOff && p.issuesCredentials
}
