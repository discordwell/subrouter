package claude

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Anthropic rotates the refresh token on use. Once the provider has accepted
// the old token, the only copy of the new one is in the response in flight; a
// caller abandoning the read at that moment strands the profile on a token
// that no longer exists. The caller may leave -- the exchange may not.
func TestRefreshCredentialExchangeOutlivesCallerCancellation(t *testing.T) {
	originalURL := oauthTokenURL
	defer func() { oauthTokenURL = originalURL }()

	store := Store{Dir: t.TempDir()}
	if err := store.ImportProfileCredential("founders@example.com", CredentialInfo{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The provider holds the old token now: from its side the rotation is
		// committed. The caller leaves before the answer lands.
		cancel()
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`)
	}))
	defer server.Close()
	oauthTokenURL = server.URL

	profile, ok := store.FindProfile("founders@example.com")
	if !ok {
		t.Fatal("profile not found")
	}
	account, didRefresh, err := store.RefreshCredentialIfExpired(ctx, server.Client(), profile)
	if err != nil {
		t.Fatalf("a caller leaving mid-exchange aborted the rotation: %v", err)
	}
	if !didRefresh || account.Token != "rotated-access" {
		t.Fatalf("refreshed=%v access=%q, want a rotated credential", didRefresh, account.Token)
	}
	stored, err := store.ReadCredential(context.Background(), store.ClaudeConfigDir(profile.Name))
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.RefreshToken != "rotated-refresh" {
		t.Fatalf("the rotated refresh token was not persisted: %+v", stored)
	}
}

// A rewrite of the same login with new metadata (plan, tier, scopes) that lands
// while the exchange is in flight must not cost the rotated pair. The provider
// has already spent the old refresh token, so dropping the rotation leaves the
// profile on a dead chain; keeping the old metadata would lose the rewrite.
// Both survive: the new tokens under the new metadata.
func TestRefreshKeepsRotatedPairAcrossMetadataOnlyRewrite(t *testing.T) {
	originalURL := oauthTokenURL
	defer func() { oauthTokenURL = originalURL }()
	swapKeychain(t, newFakeKeychain())

	store := Store{Dir: t.TempDir()}
	original := CredentialInfo{
		AccessToken:      "spent-access",
		RefreshToken:     "spent-refresh",
		SubscriptionType: "pro",
		ExpiresAt:        time.Now().Add(-time.Hour).UnixMilli(),
	}
	if err := store.ImportProfileCredential("meta@example.com", original); err != nil {
		t.Fatal(err)
	}
	profile, ok := store.FindProfile("meta@example.com")
	if !ok {
		t.Fatal("profile not found")
	}
	configDir := store.ClaudeConfigDir(profile.Name)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The same pair, recorded again with the plan the account now has.
		rewritten := original
		rewritten.SubscriptionType = "max"
		rewritten.RateLimitTier = "default_claude_max_20x"
		rewritten.Scopes = []string{"user:inference", "user:profile"}
		if err := store.WriteCredential(context.Background(), configDir, rewritten); err != nil {
			t.Errorf("metadata rewrite: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`)
	}))
	defer server.Close()
	oauthTokenURL = server.URL

	account, didRefresh, err := store.RefreshCredentialIfExpired(context.Background(), server.Client(), profile)
	if err != nil {
		t.Fatal(err)
	}
	if !didRefresh || account.Token != "rotated-access" {
		t.Fatalf("refreshed=%v access=%q, want the rotated credential served", didRefresh, account.Token)
	}
	stored, err := store.ReadCredential(context.Background(), configDir)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.AccessToken != "rotated-access" || stored.RefreshToken != "rotated-refresh" {
		t.Fatalf("stored credential = %+v; want the rotated pair, not the spent one", stored)
	}
	if stored.SubscriptionType != "max" || stored.RateLimitTier != "default_claude_max_20x" || len(stored.Scopes) != 2 {
		t.Fatalf("stored metadata = %+v; want the concurrent metadata rewrite kept", stored)
	}
	if stored.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("stored ExpiresAt = %d; want the refreshed expiry", stored.ExpiresAt)
	}
}
