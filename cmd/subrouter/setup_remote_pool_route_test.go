package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/internal/broker"
	"github.com/discordwell/subrouter/internal/proxy"
)

// PATCHES §3.1: an armed remote pool is a Claude route, the same as a local
// account. A remote-only machine holds no Claude account; its daemon forwards
// every request to a peer pool. §3's gate counted only local profiles, so
// setup never routed Claude Code there, and `claude` fell back to direct
// Anthropic auth with no credentials.
//
// These run setup's planner against a real daemon handler whose peer set is
// built the way `serve` builds it (main.go: RemotePool from remotePoolFromEnv,
// RemotePeers from remotePeersFromEnv), from this test's environment. The
// planner is a separate process in production and never sees that
// environment; here it shares the process, so each test says what the
// daemon armed rather than letting the planner read it by accident.

// remotePoolMachine is a scratch machine with no account of either kind and a
// running daemon, given a peer when withPeer is set: the remote-only box's
// shape. withKey decides whether remote.key is on it before the daemon reads
// its environment, which is the whole difference between a configured peer
// and an armed one.
func remotePoolMachine(t *testing.T, withKey, withPeer bool) {
	t.Helper()
	sandboxHostConfig(t)
	resetRemoteGateCache()
	t.Cleanup(resetRemoteGateCache)
	if withKey {
		writeRemoteKey(t)
	}

	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "[]")
	}))
	t.Cleanup(peer.Close)
	peerURL := ""
	if withPeer {
		peerURL = peer.URL
	}
	t.Setenv("SUBROUTER_REMOTE_POOL", peerURL)
	t.Setenv("SUBROUTER_REMOTE_POOL_TOKEN", "peer-token")
	t.Setenv("SUBROUTER_REMOTE_POOL_ALLOW", "")
	t.Setenv("SUBROUTER_REMOTE_POOL_FINGERPRINT", "")
	t.Setenv(remotePeersFileEnv, "")

	daemon := httptest.NewServer(proxy.Server{
		RemotePool:  remotePoolFromEnv(),
		RemotePeers: remotePeersFromEnv(io.Discard),
	}.Handler())
	t.Cleanup(daemon.Close)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", daemon.URL+"/v1")
	useLocalPeerView(t)
}

func planRemotePoolMachine(t *testing.T) setupPlan {
	t.Helper()
	plan, err := planForSetup(
		context.Background(),
		accounts.CodexStore{Dir: t.TempDir()},
		setupPlanOptions{
			wantBackground:   true,
			wantConfig:       true,
			credentialSource: broker.CredentialSourceLocal,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// The bug: no local Claude account, a peer the daemon has armed, and setup
// still refused to route Claude Code.
func TestSetupRoutesClaudeThroughAnArmedRemotePool(t *testing.T) {
	remotePoolMachine(t, true, true)
	plan := planRemotePoolMachine(t)
	item, _ := plan.find(actionConfigClaude)
	if !plan.selected(actionConfigClaude) {
		t.Fatalf("Claude Code was not routed through a daemon with an armed peer (blocked: %q)", item.Blocked)
	}
	// Claude only: the peer is not taken as a Codex route.
	if plan.selected(actionConfigCodex) {
		t.Error("an armed peer routed Codex too")
	}
}

// A peer in the environment without remote.key is not armed: the daemon
// leaves it off (§17), so setup must still refuse, and must not name the
// remote on a keyless machine.
func TestSetupRefusesAPeerConfiguredWithoutTheRemoteKey(t *testing.T) {
	remotePoolMachine(t, false, true)
	plan := planRemotePoolMachine(t)
	item, _ := plan.find(actionConfigClaude)
	if plan.selected(actionConfigClaude) {
		t.Fatal("Claude Code was routed on a peer the daemon does not use (no remote.key)")
	}
	if !strings.Contains(item.Blocked, "waiting for a Claude account; run 'sr add', then re-run setup") {
		t.Errorf("blocked reason = %q, want the account refusal", item.Blocked)
	}
	for _, word := range []string{"remote", "SUBROUTER_REMOTE_POOL", "peer"} {
		if strings.Contains(item.Blocked, word) {
			t.Errorf("a keyless machine's refusal names %q: %q", word, item.Blocked)
		}
	}
}

// With the key but no peer, the refusal stays, and names both ways out.
func TestSetupRefusalNamesBothWaysOutOnAKeyedMachine(t *testing.T) {
	remotePoolMachine(t, true, false)
	plan := planRemotePoolMachine(t)
	item, _ := plan.find(actionConfigClaude)
	if plan.selected(actionConfigClaude) {
		t.Fatal("Claude Code was routed with neither a local account nor an armed peer")
	}
	for _, want := range []string{"sr add", "SUBROUTER_REMOTE_POOL", remotePeersFileEnv, "re-run setup"} {
		if !strings.Contains(item.Blocked, want) {
			t.Errorf("blocked reason %q does not name %q", item.Blocked, want)
		}
	}
}
