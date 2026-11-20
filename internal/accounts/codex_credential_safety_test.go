package accounts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A caller that disconnects after the token endpoint rotated the single-use
// refresh token must not lose the new pair.
func TestRefreshStoredPersistsRotatedPairWhenCallerCancelsMidRefresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	originalURL := codexOAuthTokenURL
	defer func() { codexOAuthTokenURL = originalURL }()

	store := CodexStore{Dir: t.TempDir()}
	stale := storedOAuthAccount("cancel@example.com", "old", time.Now().Add(-time.Hour))
	if err := store.SaveStored(stale); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		// Hold the response until the client has either abandoned the
		// connection (caller cancellation reached the transport) or clearly
		// kept waiting for it.
		select {
		case <-r.Context().Done():
		case <-time.After(300 * time.Millisecond):
		}
		body, _ := json.Marshal(map[string]string{
			"access_token":  testCodexJWT("cancel@example.com", "new-access", time.Now().Add(time.Hour)),
			"refresh_token": "new-refresh",
			"id_token":      testCodexJWT("cancel@example.com", "new-id", time.Now().Add(time.Hour)),
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer server.Close()
	codexOAuthTokenURL = server.URL

	_, _, refreshErr := store.RefreshStoredIfExpired(ctx, server.Client(), stale)

	got, ok, err := store.FindStored(stale.Email)
	if err != nil || !ok {
		t.Fatalf("stored account found = %v, err = %v", ok, err)
	}
	if got.Auth.Tokens.RefreshToken != "new-refresh" {
		t.Fatalf("stored refresh token = %q (refresh err: %v); want the rotated new-refresh persisted", got.Auth.Tokens.RefreshToken, refreshErr)
	}
}
