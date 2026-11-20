package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// cookedPoolServer builds a Server whose only Codex account is cooked on its
// weekly window and still holds reset credits -- the shape that made every turn
// die with the upstream's quota error and no alternate to fail over to.
// storedPoolAccounts resolves saved accounts the way the daemon does at
// startup, so the Server under test routes over the same pool shape.
func storedPoolAccounts(t *testing.T, store accounts.CodexStore) []accounts.Account {
	t.Helper()
	stored, err := store.ListStored()
	if err != nil {
		t.Fatal(err)
	}
	out := make([]accounts.Account, 0, len(stored))
	for i := range stored {
		account, ok := stored[i].Account(stored[i].SourcePath(store))
		if !ok {
			t.Fatalf("could not resolve stored account %q", stored[i].Email)
		}
		out = append(out, account)
	}
	return out
}

func newTestSessionStore(t *testing.T) *session.Store {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func cookedPoolServer(t *testing.T, enabled bool, consume *int) (Server, accounts.CodexStore) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	if err := store.SaveStored(proxyStoredOAuthAccount("cooked@example.com", "fresh", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: whamResponder(t, consume, true)}
	server := Server{
		AccountRef:               NewAccountRef(store, storedPoolAccounts(t, store), client),
		Sessions:                 newTestSessionStore(t),
		SchedulerRef:             &selectacct.SchedulerRef{},
		AutoRedeemRateLimitReset: enabled,
		MaxBodyBytes:             1024,
	}.normalized()
	return server, store
}

// TestRedeemForFailoverStaysOffByDefault is the property that makes the feature
// safe to ship: the credits are finite, so a daemon that was not opted in must
// never spend one, no matter how thoroughly the pool is cooked.
func TestRedeemForFailoverStaysOffByDefault(t *testing.T) {
	var consume int
	server, _ := cookedPoolServer(t, false, &consume)

	if _, ok := server.redeemForFailover(context.Background(), accounts.ProviderCodex, "codex", "s1", "", nil); ok {
		t.Fatal("redeemed with the feature disabled")
	}
	if consume != 0 {
		t.Fatalf("consume calls = %d, want 0 while disabled", consume)
	}
}

// TestRedeemForFailoverRevivesCookedAccount covers the whole point: with every
// account cooked, spending a credit produces the alternate that failover could
// not otherwise find, and routing state is updated so the credit is not wasted.
func TestRedeemForFailoverRevivesCookedAccount(t *testing.T) {
	var consume int
	server, _ := cookedPoolServer(t, true, &consume)

	// Mark the account exhausted the way a live quota rejection would, so the
	// test proves the mark is cleared rather than merely absent.
	server.SchedulerRef.MarkExhausted(accounts.ProviderCodex, "cooked@example.com", "")

	revived, ok := server.redeemForFailover(context.Background(), accounts.ProviderCodex, "codex", "s1", "", nil)
	if !ok {
		t.Fatal("no account revived despite an available reset credit")
	}
	if revived.ID != "cooked@example.com" {
		t.Fatalf("revived account = %q, want cooked@example.com", revived.ID)
	}
	if consume != 1 {
		t.Fatalf("consume calls = %d, want exactly 1", consume)
	}
	if _, marked := server.SchedulerRef.ExhaustedUntilFor(accounts.ProviderCodex, "cooked@example.com", ""); marked {
		t.Error("exhaustion mark survived the redemption; routing would keep skipping the account we just paid to revive")
	}
	if !server.SchedulerRef.Stale(time.Nanosecond) {
		t.Error("scores were not invalidated; selection would serve the snapshot that predates the reset")
	}
	got, ok := server.Sessions.Get("codex", "s1")
	if !ok || got.AccountID != "cooked@example.com" {
		t.Errorf("session not pinned to the revived account: %+v ok=%v", got, ok)
	}
}

// TestRedeemForFailoverHonorsIntervalFloor is the bound that keeps a burst from
// draining the reserve. One interrupted turn produces several failing requests;
// only the first may spend.
func TestRedeemForFailoverHonorsIntervalFloor(t *testing.T) {
	var consume int
	server, _ := cookedPoolServer(t, true, &consume)

	if _, ok := server.redeemForFailover(context.Background(), accounts.ProviderCodex, "codex", "s1", "", nil); !ok {
		t.Fatal("first redemption failed")
	}
	if _, ok := server.redeemForFailover(context.Background(), accounts.ProviderCodex, "codex", "s2", "", nil); ok {
		t.Error("second redemption inside the interval floor was allowed")
	}
	if consume != 1 {
		t.Fatalf("consume calls = %d, want exactly 1 across two attempts", consume)
	}
}

// TestRedeemForFailoverSkipsTriedAccount asserts the credit is never spent on
// the account whose failure started the failover. Reviving the account that
// just rejected the turn buys nothing and costs a credit.
func TestRedeemForFailoverSkipsTriedAccount(t *testing.T) {
	var consume int
	server, _ := cookedPoolServer(t, true, &consume)

	tried := map[string]struct{}{"cooked@example.com": {}}
	if _, ok := server.redeemForFailover(context.Background(), accounts.ProviderCodex, "codex", "s1", "", tried); ok {
		t.Error("redeemed on an account already tried this failover")
	}
	if consume != 0 {
		t.Fatalf("consume calls = %d, want 0", consume)
	}
}

// TestRedeemForFailoverSkipsHealthyAccount asserts eligibility is decided
// against live upstream usage: an account that is not actually cooked keeps its
// credit, even when our own cached scores said the pool was empty.
func TestRedeemForFailoverSkipsHealthyAccount(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	if err := store.SaveStored(proxyStoredOAuthAccount("healthy@example.com", "fresh", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	var consume int
	// whamResponder always reports a fully-consumed weekly window, so a genuinely
	// healthy account needs its own responder: low usage, limit not reached, and
	// credits available -- the account has something to spend and no reason to.
	client := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/backend-api/wham/usage":
			body, _ := json.Marshal(map[string]any{
				"plan_type": "pro",
				"rate_limit": map[string]any{
					"limit_reached": false,
					"secondary_window": map[string]any{
						"used_percent": 5, "limit_window_seconds": 604800, "reset_after_seconds": 250000,
					},
				},
				"rate_limit_reset_credits": map[string]any{"available_count": 2},
			})
			return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
		case "/backend-api/wham/rate-limit-reset-credits/consume":
			consume++
			return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(bytes.NewReader([]byte(`{"code":"reset"}`))), Request: req}, nil
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Header: header, Body: io.NopCloser(nil), Request: req}, nil
		}
	})}
	server := Server{
		AccountRef:               NewAccountRef(store, storedPoolAccounts(t, store), client),
		Sessions:                 newTestSessionStore(t),
		SchedulerRef:             &selectacct.SchedulerRef{},
		AutoRedeemRateLimitReset: true,
		MaxBodyBytes:             1024,
	}.normalized()

	if _, ok := server.redeemForFailover(context.Background(), accounts.ProviderCodex, "codex", "s1", "", nil); ok {
		t.Error("redeemed a credit on an account that was not cooked")
	}
	if consume != 0 {
		t.Fatalf("consume calls = %d, want 0", consume)
	}
}

// TestRedeemForFailoverIgnoresNonCodexProviders asserts the Claude pool is
// untouched: reset credits are a ChatGPT concept and the wham endpoints do not
// exist for an Anthropic account.
func TestRedeemForFailoverIgnoresNonCodexProviders(t *testing.T) {
	var consume int
	server, _ := cookedPoolServer(t, true, &consume)

	if _, ok := server.redeemForFailover(context.Background(), accounts.ProviderClaude, "claude", "s1", "", nil); ok {
		t.Error("redeemed for a Claude request")
	}
	if consume != 0 {
		t.Fatalf("consume calls = %d, want 0", consume)
	}
}
