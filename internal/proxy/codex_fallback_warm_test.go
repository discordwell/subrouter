package proxy

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/session"
)

// A keyless Codex request (no recognised session header, no id in the body)
// gets a per-client `fallback:<hash>` key, and Codex collapses every one of
// them into the single codex:fallback pin -- it is what keeps the identity
// calls (wham, plugins) on one account. That bucket is not a conversation: one
// client's keyless turn says nothing about another client's cache. So it is
// never warm; it keeps the pre-merge cold Codex rule (a turn in flight keeps the
// account, otherwise the account must still take a new session, and a
// below-floor account is always offered to the peer comparator).
//
// Claude does not collapse its fallback keys: each is one client's session and
// the warm rule applies to it exactly as before.

const (
	codexKeylessClientA = "fallback:aaaaaaaaaaaaaaaaaaaaaaaa"
	codexKeylessClientB = "fallback:bbbbbbbbbbbbbbbbbbbbbbbb"
)

// sharedFallbackFixture pins the shared bucket (through client A's key) to the
// burn-refused "hot" account, with the tracker up for three hours so nothing
// reads as possibly-warm merely for being unseen since a restart.
func sharedFallbackFixture(t *testing.T) (Server, *ActiveSessions, *session.Store) {
	t.Helper()
	server, active, store := hotCodexFixture(t, burnRefusedCodexScore())
	active.startedAt = time.Now().Add(-3 * time.Hour)
	if _, err := store.Put("codex", codexKeylessClientA, "hot", ""); err != nil {
		t.Fatal(err)
	}
	// Calibration: the two clients' keys really are one pin, so client B's
	// turn is judged on the bucket client A placed.
	if got, ok := store.Get("codex", codexKeylessClientB); !ok || got.AccountID != "hot" {
		t.Fatalf("calibration: keyless Codex keys do not share one pin: %+v %v", got, ok)
	}
	return server, active, store
}

func TestAnotherKeylessCodexClientsRecentTurnDoesNotMakeTheBucketWarm(t *testing.T) {
	server, active, store := sharedFallbackFixture(t)
	markCodexIdle(active, codexKeylessClientA, 5*time.Minute)
	// Calibration: the tracker collapses the keys too, so client B's key does
	// read client A's turn five minutes ago.
	if idle, seen := active.IdleSince("codex", codexKeylessClientB); !seen || idle > 6*time.Minute {
		t.Fatalf("calibration: client B's key does not see client A's turn: idle %v seen %v", idle, seen)
	}
	account, _, _, _, err := server.accountForSessionProvider(accounts.ProviderCodex, "codex", codexKeylessClientB, codexResponsesRequest())
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != "cool" {
		t.Fatalf("account = %q, want cool: another client's keyless turn kept the bucket on a burn-refused account", account.ID)
	}
	if got, _ := store.Get("codex", codexKeylessClientB); got.AccountID != "cool" {
		t.Fatalf("bucket pin = %q, want cool", got.AccountID)
	}
}

func TestKeylessCodexBucketInFlightStillOffersABelowFloorAccountToThePeer(t *testing.T) {
	server, active, _ := sharedFallbackFixture(t)
	end := active.Begin("codex", codexKeylessClientA)
	defer end()
	account, capacity, offered := offeredToHealthyPeer(t, server, accounts.ProviderCodex, "codex", codexKeylessClientB, codexResponsesRequest())
	// The pre-merge rule: a keyless turn in flight keeps the bucket's account...
	if account.ID != "hot" {
		t.Fatalf("account = %q, want hot (a turn in flight keeps the bucket, as before the merge)", account.ID)
	}
	// ...but the bucket is not warm, so the comparator still sees its
	// below-floor capacity and may send the turn to the peer.
	if !capacity.BelowFloor {
		t.Fatal("the comparator was handed no capacity for a below-floor account: the keyless bucket read as warm")
	}
	if !offered {
		t.Fatal("a healthy peer was not preferred over the burn-refused bucket account")
	}
}

func TestClaudeFallbackKeyIsItsOwnSessionAndStaysWarm(t *testing.T) {
	server, store, _, _ := coldClaudeSideFixture(t, nil)
	server.ActiveSessions.startedAt = time.Now().Add(-3 * time.Hour)
	const warmKey, coldKey = "fallback:cccccccccccccccccccccccc", "fallback:dddddddddddddddddddddddd"
	for _, key := range []string{warmKey, coldKey} {
		if _, err := store.Put("claude", key, "hot", ""); err != nil {
			t.Fatal(err)
		}
	}
	markIdle(server.ActiveSessions, warmKey, 5*time.Minute)

	// The warm client keeps its burn-refused account and is not offered to
	// the peer: the warm rule, unchanged.
	account, _, offered := offeredToHealthyPeer(t, server, accounts.ProviderClaude, "claude", warmKey, claudeMessagesRequest())
	if account.ID != "hot" {
		t.Fatalf("warm Claude fallback key: account = %q, want hot", account.ID)
	}
	if offered {
		t.Fatal("warm Claude fallback key was offered to the peer")
	}

	// A different Claude client's fallback key is its own session: the first
	// client's turn does not warm it, so, idle past the bound, it re-ranks.
	account, _, _, _, err := server.accountForSessionProvider(accounts.ProviderClaude, "claude", coldKey, claudeMessagesRequest())
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != "cool" {
		t.Fatalf("cold Claude fallback key: account = %q, want cool (Claude fallback keys are per client)", account.ID)
	}
}
