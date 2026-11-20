package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// N peers (PATCHES §31, D0; owner rule P7): a deciding daemon ranks its own
// accounts and the accounts of EVERY peer it is configured to use -- "the best
// across everything you can see". Each peer keeps its own client, dialer,
// breaker, admin cache, carry and hold table, so one slow or dead peer costs
// only its own candidates.
//
// The single-peer environment configuration (SUBROUTER_REMOTE_POOL, _TOKEN,
// _ALLOW, _FINGERPRINT) is unchanged and still yields exactly one peer named by
// its URL host; SUBROUTER_REMOTE_POOLS_FILE lists several.

// peers is every configured peer in configuration order. The ONE reader of
// Server.RemotePool outside tests: RemotePeers when set, else the single-peer
// RemotePool, else none.
func (s Server) peers() []*RemotePool {
	if len(s.RemotePeers) > 0 {
		out := make([]*RemotePool, 0, len(s.RemotePeers))
		for _, p := range s.RemotePeers {
			if p != nil && p.URL != nil {
				out = append(out, p)
			}
		}
		return out
	}
	if s.RemotePool != nil && s.RemotePool.URL != nil {
		return []*RemotePool{s.RemotePool}
	}
	return nil
}

// firstPeer is the peer the single-peer entry points (and their tests) address.
func (s Server) firstPeer() *RemotePool {
	if peers := s.peers(); len(peers) > 0 {
		return peers[0]
	}
	return nil
}

// peerForwardingAllowed is the half of the old remotePoolEligible that is
// about the REQUEST, not about any one peer: no broker or lease mode, not
// already forwarded by another daemon, not a control-plane call.
func (s Server) peerForwardingAllowed(r *http.Request) bool {
	if len(s.peers()) == 0 {
		return false
	}
	// Team/broker mode leases a short-lived credential to THIS machine and
	// bypasses the local store entirely; the peer has its own broker identity
	// and cannot honour a lease it never issued. Same reason the transport
	// install guard excludes both.
	if s.CredentialBroker != nil || s.RequireSessionLease {
		return false
	}
	// A request that already came from another daemon must never be forwarded
	// again. Two daemons pointed at each other would otherwise recurse; with N
	// peers a request that arrived from any of them is served here or refused,
	// never re-forwarded (P7).
	if remotePoolInboundHop(r) >= 1 {
		return false
	}
	// A request naming a target account is a peer's targeted forward: it is
	// served on that account or refused, and never re-forwarded, even with no
	// hop header (PATCHES §31, D3).
	if r != nil && strings.TrimSpace(r.Header.Get(remotePoolTargetAccountHeader)) != "" {
		return false
	}
	// Control-plane calls ask who the CALLER is. Answering one from the peer
	// reports the PEER's organization and its entitlements -- which the client
	// then caches as its own identity.
	// Capacity is irrelevant here: these are tiny, and being answered by the
	// wrong account is not a degraded answer, it is a wrong one.
	if r != nil && controlPlanePath(r.URL.Path) {
		return false
	}
	return true
}

// peerUsable is the half of the old remotePoolEligible that is about ONE
// peer: configured, allowed for this caller, breaker closed.
func (s Server) peerUsable(p *RemotePool, r *http.Request) bool {
	return s.peerOpenTo(p, r) && p.breaker.allow(time.Now())
}

// peerOpenTo is peerUsable without the breaker: configured and allowed for
// this caller.
func (s Server) peerOpenTo(p *RemotePool, r *http.Request) bool {
	if p == nil || p.URL == nil {
		return false
	}
	// Whose accounts get spent is a policy question, not a capacity one.
	return p.allows(clientRemoteIP(r))
}

// peerHold reports where a session is held, if anywhere: the first live hold
// in configuration order. A session is held on at most one peer (claim and set
// clear it on the others), so the order only matters for a hold loaded from
// disk that a later claim has not yet superseded.
func (s Server) peerHold(key string) (*RemotePool, remotePoolDelegation, bool) {
	if key == "" {
		return nil, remotePoolDelegation{}, false
	}
	now := time.Now()
	for _, p := range s.peers() {
		if hold, ok := p.delegations.lookup(key, now); ok {
			return p, hold, true
		}
	}
	return nil, remotePoolDelegation{}, false
}

// peerHolds is every live hold by session key, each answered as peerHold
// answers it: the first peer in configuration order holding the key.
func (s Server) peerHolds() map[string]remotePoolDelegation {
	now := time.Now()
	out := map[string]remotePoolDelegation{}
	for _, p := range s.peers() {
		for key, hold := range p.delegations.live(now) {
			if _, seen := out[key]; !seen {
				out[key] = hold
			}
		}
	}
	return out
}

// clearHoldElsewhere keeps one owner per session: a claim or hold on keep
// clears the key on every other peer.
func (s Server) clearHoldElsewhere(key string, keep *RemotePool) {
	for _, p := range s.peers() {
		if p != keep {
			p.delegations.clear(key)
		}
	}
}

// WithName names the peer for logs, holds and X-Subrouter-Peer.
func (p *RemotePool) WithName(name string) *RemotePool {
	if p == nil {
		return nil
	}
	p.Name = strings.TrimSpace(name)
	return p
}

// --- decision-time usage reads -------------------------------------------

// remotePoolDecisionFetchBudget is the most a routing decision waits for peer
// usage fetches that were not already cached. Fetches run in parallel and in
// the background, so one slow or blackholed peer costs at most this once per
// remotePoolAdminTTL and never delays the others; a peer not back in time
// contributes its last good body (stale but real) or, with none, nothing.
const remotePoolDecisionFetchBudget = 250 * time.Millisecond

// remotePoolHoldVerdictMaxAge bounds how old a cached usage body may be before
// a held turn stops trusting its verdict on the hold's account (D5). Older
// than this and the held turn is simply forwarded: the peer judges it.
const remotePoolHoldVerdictMaxAge = 60 * time.Second

// usageForDecision returns the cached usage body, starting ONE background
// fetch when it is older than remotePoolAdminTTL (or absent) and waiting at
// most until deadline for it. A zero deadline never waits.
func (p *RemotePool) usageForDecision(deadline time.Time) AdminSnapshot {
	if p == nil || p.URL == nil {
		return AdminSnapshot{Err: "no peer configured"}
	}
	path := remotePoolUsagePath
	p.admin.mu.Lock()
	cached, ok := p.admin.entries[path]
	if ok && cached.err == "" && time.Since(cached.at) < remotePoolAdminTTL {
		p.admin.mu.Unlock()
		return AdminSnapshot{Body: cached.body, At: cached.at, Reachable: true}
	}
	if ok && cached.err != "" && time.Since(cached.tried) < remotePoolAdminTTL {
		// A fetch failed moments ago: the last good body (or none) stands until
		// the TTL has passed, so an unreachable peer costs one fetch per TTL,
		// not the decision budget on every request.
		p.admin.mu.Unlock()
		return AdminSnapshot{Body: cached.body, At: cached.at, Reachable: false, Err: cached.err}
	}
	if p.admin.inflight == nil {
		p.admin.inflight = map[string]chan struct{}{}
	}
	done, running := p.admin.inflight[path]
	if !running {
		done = make(chan struct{})
		p.admin.inflight[path] = done
		go func() {
			// Its own context: the result outlives the request that started
			// it, exactly like probeCapacity.
			body, err := p.adminFetch(context.Background(), path)
			p.admin.store(path, body, err)
			p.admin.mu.Lock()
			delete(p.admin.inflight, path)
			p.admin.mu.Unlock()
			close(done)
		}()
	}
	p.admin.mu.Unlock()
	if wait := time.Until(deadline); wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-done:
		case <-timer.C:
		}
		timer.Stop()
	}
	return p.cachedUsage()
}

// cachedUsage is the last good usage body with no fetch at all.
func (p *RemotePool) cachedUsage() AdminSnapshot {
	if p == nil {
		return AdminSnapshot{}
	}
	p.admin.mu.Lock()
	defer p.admin.mu.Unlock()
	cached, ok := p.admin.entries[remotePoolUsagePath]
	if !ok {
		return AdminSnapshot{}
	}
	return AdminSnapshot{Body: cached.body, At: cached.at, Reachable: cached.err == "", Err: cached.err}
}

// --- per-peer target marks -------------------------------------------------

// remotePoolTargetMarks is what one peer has told this daemon about targeting
// since its last usage body. Both kinds of mark lapse on the next body: a
// fresh verdict supersedes a refusal, and a peer that ignored a target once
// may have been upgraded since.
type remotePoolTargetMarks struct {
	mu sync.Mutex
	// refused: (provider, account) -> the usage body time the refusal was
	// judged against, so a body measured after it lifts the mark.
	refused map[string]time.Time
	// untargetedThrough: the peer served a target on a DIFFERENT account (a
	// peer that ignores the header) while its body was this old; until a newer
	// body it is ranked as a legacy peer and sent no target (D4).
	untargetedThrough time.Time
}

func targetMarkKey(provider, account string) string {
	return provider + "\x00" + account
}

// markRefused records that account refused a targeted turn, until a usage
// body newer than bodyAt.
func (p *RemotePool) markRefused(provider, account string, bodyAt time.Time) {
	if p == nil || account == "" {
		return
	}
	p.targets.mu.Lock()
	defer p.targets.mu.Unlock()
	if p.targets.refused == nil {
		p.targets.refused = map[string]time.Time{}
	}
	p.targets.refused[targetMarkKey(provider, account)] = bodyAt
}

// refusedFor reports whether account's refusal still stands against a body
// measured at bodyAt.
func (p *RemotePool) refusedFor(provider, account string, bodyAt time.Time) bool {
	if p == nil {
		return false
	}
	p.targets.mu.Lock()
	defer p.targets.mu.Unlock()
	at, ok := p.targets.refused[targetMarkKey(provider, account)]
	if !ok {
		return false
	}
	if bodyAt.After(at) {
		delete(p.targets.refused, targetMarkKey(provider, account))
		return false
	}
	return true
}

// markTargetIgnored records that the peer served a target on another account.
func (p *RemotePool) markTargetIgnored(bodyAt time.Time) {
	if p == nil {
		return
	}
	p.targets.mu.Lock()
	defer p.targets.mu.Unlock()
	if bodyAt.After(p.targets.untargetedThrough) {
		p.targets.untargetedThrough = bodyAt
	}
	if p.targets.untargetedThrough.IsZero() {
		// No body yet: stand until the first one.
		p.targets.untargetedThrough = time.Unix(0, 1)
	}
}

// ignoresTargets reports whether a body measured at bodyAt predates evidence
// that this peer ignores targets.
func (p *RemotePool) ignoresTargets(bodyAt time.Time) bool {
	if p == nil {
		return false
	}
	p.targets.mu.Lock()
	defer p.targets.mu.Unlock()
	return !p.targets.untargetedThrough.IsZero() && !bodyAt.After(p.targets.untargetedThrough)
}

// --- SUBROUTER_REMOTE_POOLS_FILE -------------------------------------------

// remotePeersFileMaxBytes bounds the peers file; remotePeersMax bounds its
// entries. Both are far above any real configuration.
const (
	remotePeersFileMaxBytes = 64 << 10
	remotePeersMax          = 16
)

var remotePeerNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type remotePeersFile struct {
	Peers []remotePeerEntry `json:"peers"`
}

type remotePeerEntry struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Token       string `json:"token"`
	Fingerprint string `json:"fingerprint"`
	Allow       string `json:"allow"`
}

// LoadRemotePeersFile reads SUBROUTER_REMOTE_POOLS_FILE: a regular file, mode
// 0600 or tighter (the remote.key rule), at most 64 KiB of strict JSON
// {"peers":[{name,url,token,fingerprint,allow}]}. Names are required, unique
// case-insensitively and match remotePeerNamePattern; URLs parse like
// SUBROUTER_REMOTE_POOL and are unique; at most 16 peers.
//
// FAILS CLOSED: any invalid entry returns no peers at all, with an error that
// names the index, the name and the field -- never a token, a fingerprint or a
// URL's credentials. A half-applied peer list would route to a subset the
// operator did not write down.
func LoadRemotePeersFile(path string) ([]*RemotePool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("remote peers file: %w", errors.Unwrap(err))
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("remote peers file is not a regular file")
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("remote peers file is mode %04o; chmod 600 it", perm)
	}
	if info.Size() > remotePeersFileMaxBytes {
		return nil, fmt.Errorf("remote peers file is larger than %d bytes", remotePeersFileMaxBytes)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("remote peers file could not be read")
	}
	return parseRemotePeers(body)
}

func parseRemotePeers(body []byte) ([]*RemotePool, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var file remotePeersFile
	if err := decoder.Decode(&file); err != nil {
		// The decoder's message can quote the offending token, which may be a
		// secret; say only where it went wrong.
		var syntax *json.SyntaxError
		var field *json.UnmarshalTypeError
		switch {
		case errors.As(err, &syntax):
			return nil, fmt.Errorf("remote peers file is not valid JSON (offset %d)", syntax.Offset)
		case errors.As(err, &field):
			return nil, fmt.Errorf("remote peers file field %q has the wrong type", field.Field)
		case strings.HasPrefix(err.Error(), "json: unknown field"):
			return nil, errors.New("remote peers file has an unknown field (allowed: peers[].name, url, token, fingerprint, allow)")
		default:
			return nil, errors.New("remote peers file is not valid JSON")
		}
	}
	if decoder.More() {
		return nil, errors.New("remote peers file has trailing data after the JSON object")
	}
	if len(file.Peers) == 0 {
		return nil, errors.New("remote peers file lists no peers")
	}
	if len(file.Peers) > remotePeersMax {
		return nil, fmt.Errorf("remote peers file lists %d peers; at most %d", len(file.Peers), remotePeersMax)
	}
	names := map[string]bool{}
	urls := map[string]bool{}
	peers := make([]*RemotePool, 0, len(file.Peers))
	for i, entry := range file.Peers {
		name := strings.TrimSpace(entry.Name)
		if !remotePeerNamePattern.MatchString(name) {
			return nil, fmt.Errorf("remote peers file entry %d: field name is missing or not [A-Za-z0-9._-]{1,64}", i)
		}
		if names[strings.ToLower(name)] {
			return nil, fmt.Errorf("remote peers file entry %d (%s): field name duplicates an earlier peer", i, name)
		}
		names[strings.ToLower(name)] = true
		p := NewRemotePool(entry.URL, entry.Token, entry.Allow)
		if p == nil {
			return nil, fmt.Errorf("remote peers file entry %d (%s): field url is not an absolute scheme://host URL", i, name)
		}
		identity := strings.ToLower(p.URL.Scheme + "://" + p.URL.Host + p.URL.Path)
		if urls[identity] {
			return nil, fmt.Errorf("remote peers file entry %d (%s): field url duplicates an earlier peer", i, name)
		}
		urls[identity] = true
		peers = append(peers, p.WithFingerprint(entry.Fingerprint).WithName(name))
	}
	return peers, nil
}

// PersistHoldsAt keeps each peer's holds in its own file: the first peer in
// holdsPath (the existing peer-holds.json, so a single-peer daemon is
// unchanged), every other one in peer-holds-<name>.json beside it. Each stops
// writing when generation ends (PersistHolds).
func PersistHoldsAt(generation context.Context, peers []*RemotePool, holdsPath string) []*RemotePool {
	for i, p := range peers {
		if i == 0 {
			p.PersistHolds(generation, holdsPath)
			continue
		}
		ext := filepath.Ext(holdsPath)
		p.PersistHolds(generation, strings.TrimSuffix(holdsPath, ext)+"-"+p.name()+ext)
	}
	return peers
}
