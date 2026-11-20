package claude

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// A move removes a profile only while it still serves the pair the move read
// and uploaded. A replaced pair keeps the profile and says so with its own
// error; a metadata-only rewrite is the same chain and does not stop it.
func TestRemoveProfileIfCredentialRemovesOnlyTheCredentialItWasShown(t *testing.T) {
	KeychainWithNoItemsForTest(t)
	store := Store{Dir: t.TempDir()}
	const name = "moved@example.com"
	uploaded := CredentialInfo{
		AccessToken:      "uploaded-access",
		RefreshToken:     "uploaded-refresh",
		ExpiresAt:        time.Now().Add(time.Hour).UnixMilli(),
		SubscriptionType: "pro",
	}
	if err := store.ImportProfileCredential(name, uploaded); err != nil {
		t.Fatal(err)
	}
	configDir := store.ClaudeConfigDir(name)

	rotated := uploaded
	rotated.AccessToken = "rotated-access"
	rotated.RefreshToken = "rotated-refresh"
	if err := store.WriteCredential(context.Background(), configDir, rotated); err != nil {
		t.Fatal(err)
	}
	removed, err := store.RemoveProfileIfCredentialContext(context.Background(), name, uploaded)
	if removed || !errors.Is(err, ErrProfileCredentialReplaced) {
		t.Fatalf("removal of a replaced credential: removed=%v err=%v; want kept with ErrProfileCredentialReplaced", removed, err)
	}
	if current, err := store.ReadCredential(context.Background(), configDir); err != nil || current == nil ||
		current.RefreshToken != "rotated-refresh" {
		t.Fatalf("after a refused removal the profile holds %+v (err %v); want the rotated pair", current, err)
	}

	metadataOnly := rotated
	metadataOnly.SubscriptionType = "max"
	removed, err = store.RemoveProfileIfCredentialContext(context.Background(), name, metadataOnly)
	if err != nil || !removed {
		t.Fatalf("removal of the same pair under other metadata: removed=%v err=%v", removed, err)
	}
	if _, ok := store.FindProfile(name); ok {
		t.Fatal("the profile is still registered after its removal")
	}
}

// A re-read that finds nothing is not a replacement: the profile is kept, and
// the error is not ErrProfileCredentialReplaced, which a move answers by
// deleting the server's copy.
func TestRemoveProfileIfCredentialKeepsAProfileItCannotConfirm(t *testing.T) {
	KeychainWithNoItemsForTest(t)
	store := Store{Dir: t.TempDir()}
	const name = "vanished@example.com"
	uploaded := CredentialInfo{AccessToken: "a", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	if err := store.ImportProfileCredential(name, uploaded); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(store.ClaudeConfigDir(name), ".credentials.json")); err != nil {
		t.Fatal(err)
	}
	removed, err := store.RemoveProfileIfCredentialContext(context.Background(), name, uploaded)
	if removed || err == nil || errors.Is(err, ErrProfileCredentialReplaced) {
		t.Fatalf("removal with no credential to compare: removed=%v err=%v; want kept with an error that is not a replacement", removed, err)
	}
	if _, ok := store.FindProfile(name); !ok {
		t.Fatal("the profile was removed although its credential could not be compared")
	}
}

// While HoldProfileRefresh runs, a refresh of the profile waits for it, so a
// move's read, upload and removal see no rotation. A refresh that waited
// through the removal finds the profile gone and presents nothing.
func TestHoldProfileRefreshHoldsOffARefreshThroughTheRemoval(t *testing.T) {
	KeychainWithNoItemsForTest(t)
	originalURL := oauthTokenURL
	defer func() { oauthTokenURL = originalURL }()
	var exchanges atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`)
	}))
	defer server.Close()
	oauthTokenURL = server.URL

	store := Store{Dir: t.TempDir()}
	const name = "held@example.com"
	uploaded := CredentialInfo{AccessToken: "uploaded-access", RefreshToken: "uploaded-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	if err := store.ImportProfileCredential(name, uploaded); err != nil {
		t.Fatal(err)
	}
	profile, ok := store.FindProfile(name)
	if !ok {
		t.Fatal("profile not found")
	}

	refreshed := make(chan error, 1)
	err := store.HoldProfileRefresh(context.Background(), name, func() error {
		go func() {
			_, _, err := store.ForceRefreshCredential(context.Background(), server.Client(), profile)
			refreshed <- err
		}()
		select {
		case err := <-refreshed:
			return errors.Join(errors.New("a refresh ran while the refresh lock was held"), err)
		case <-time.After(500 * time.Millisecond):
		}
		removed, err := store.RemoveProfileIfCredentialContext(context.Background(), name, uploaded)
		if err != nil || !removed {
			return errors.Join(errors.New("the held removal did not remove the profile"), err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-refreshed:
		if err == nil {
			t.Fatal("the refresh that waited out the removal reported success for a removed profile")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the refresh never finished after the hold ended")
	}
	if got := exchanges.Load(); got != 0 {
		t.Fatalf("the token endpoint was asked %d times; a removed profile's refresh token must not be presented", got)
	}
}
