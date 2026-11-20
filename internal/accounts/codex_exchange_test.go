package accounts

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The Codex half of the same contract: a caller may leave the queue for the
// lock, but once the old refresh token has been presented the read of the new
// one is not the caller's to abandon.
func TestRefreshStoredExchangeOutlivesCallerCancellation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	previous := codexOAuthTokenURL
	t.Cleanup(func() { codexOAuthTokenURL = previous })

	store := CodexStore{Dir: t.TempDir()}
	stale := storedOAuthAccount("founders@example.com", "old", time.Now().Add(-time.Hour))
	if err := store.SaveStored(stale); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel() // the caller leaves with the rotation already committed upstream
		time.Sleep(50 * time.Millisecond)
		res := refreshResponse("new", "founders@example.com", time.Now().Add(time.Hour))
		defer res.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.Copy(w, res.Body)
	}))
	defer server.Close()
	codexOAuthTokenURL = server.URL

	got, didRefresh, err := store.RefreshStoredIfExpired(ctx, server.Client(), stale)
	if err != nil {
		t.Fatalf("a caller leaving mid-exchange aborted the rotation: %v", err)
	}
	if !didRefresh || got.Auth.Tokens == nil || got.Auth.Tokens.RefreshToken != "new-refresh" {
		t.Fatalf("refreshed=%v tokens=%+v, want the rotated chain", didRefresh, got.Auth.Tokens)
	}
	stored, ok, err := store.FindStored("founders@example.com")
	if err != nil || !ok {
		t.Fatalf("stored account missing after refresh: ok=%v err=%v", ok, err)
	}
	if stored.Auth.Tokens == nil || stored.Auth.Tokens.RefreshToken != "new-refresh" {
		t.Fatalf("the rotated refresh token was not persisted: %+v", stored.Auth.Tokens)
	}
}

// attest-legacy rotates a stored chain once, from an operator's terminal. A
// Ctrl-C that lands after the old refresh token was presented must still
// persist the rotated chain, or the account is stranded on a dead token.
func TestAttestStoredLegacyOAuthExchangeOutlivesCallerCancellation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	previous := codexOAuthTokenURL
	t.Cleanup(func() { codexOAuthTokenURL = previous })

	store := CodexStore{Dir: t.TempDir(), DisableActiveAuthSync: true, RequireIsolatedOAuth: true}
	legacy := storedOAuthAccount("legacy@example.com", "legacy", time.Now().Add(time.Hour))
	if err := store.SaveStored(legacy); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel() // the operator leaves with the rotation already committed upstream
		time.Sleep(50 * time.Millisecond)
		res := refreshResponse("rotated", "legacy@example.com", time.Now().Add(time.Hour))
		defer res.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.Copy(w, res.Body)
	}))
	defer server.Close()
	codexOAuthTokenURL = server.URL

	attested, err := store.AttestStoredLegacyOAuth(ctx, server.Client(), legacy.Email)
	if err != nil {
		t.Fatalf("a caller leaving mid-exchange aborted the attestation: %v", err)
	}
	if attested.Auth.Tokens == nil || attested.Auth.Tokens.RefreshToken != "rotated-refresh" {
		t.Fatalf("tokens=%+v, want the rotated chain", attested.Auth.Tokens)
	}
	stored, ok, err := store.FindStored(legacy.Email)
	if err != nil || !ok {
		t.Fatalf("stored account missing after attestation: ok=%v err=%v", ok, err)
	}
	if stored.Auth.Tokens == nil || stored.Auth.Tokens.RefreshToken != "rotated-refresh" ||
		stored.OAuthCredentialOrigin != CodexOAuthOriginServerAttested {
		t.Fatalf("the rotated, attested chain was not persisted: origin=%q", stored.OAuthCredentialOrigin)
	}
}
