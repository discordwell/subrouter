package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	agentclaude "github.com/discordwell/subrouter/internal/agents/claude"
	"github.com/discordwell/subrouter/selectacct"
)

func TestPublicListenerRefusesAtCapForATurnInFlightOnThePrivateListener(t *testing.T) {
	box := newTargetBox(t)
	box.server.ActiveSessions = nil
	claudeStore := agentclaude.Store{Dir: t.TempDir()}
	for _, account := range box.server.Accounts {
		if err := claudeStore.ImportProfileCredential(account.ID, agentclaude.CredentialInfo{
			AccessToken: account.Token, RefreshToken: "refresh-" + account.ID,
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	box.server.AccountRef = &AccountRef{accounts: box.server.Accounts,
		store: accounts.CodexStore{Dir: t.TempDir()}, claudeStore: claudeStore}
	awaitLastOKWrite(t, box.server.AccountRef)
	box.server.Accounts = nil
	box.pin(t, "busy", "chip-5")

	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	box.upstream.respond = func(account string, w http.ResponseWriter) {
		if account == "chip-5" && held.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		w.Header().Set("anthropic-ratelimit-unified-status", "allowed")
		_, _ = w.Write([]byte(`{"id":"msg"}`))
	}

	private := httptest.NewServer(box.server.Handler())
	t.Cleanup(private.Close)
	public := httptest.NewServer(box.server.PublicHandler())
	t.Cleanup(public.Close)

	go func() {
		defer close(done)
		response, err := http.DefaultClient.Do(targetTurnRequest(t, private.URL, "busy", ""))
		if err == nil {
			response.Body.Close()
		}
	}()
	// The held turn writes its pin when it ends; let it finish before TempDir cleanup.
	defer func() { close(release); <-done }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the private listener's turn never reached chip-5 upstream")
	}

	response, err := http.DefaultClient.Do(targetTurnRequest(t, public.URL, "new-session", "chip-5"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	refusal := readRefusal(t, response, selectacct.VerdictAtCap)
	if v := refusal.Target.Verdict; v == nil || v.LiveSessions != 1 || v.AllowedSessions != 1 {
		t.Fatalf("refusal verdict %+v, want at cap on 1 of 1 live sessions", refusal.Target.Verdict)
	}
}

func targetTurnRequest(t *testing.T, baseURL, sessionID, target string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/messages",
		strings.NewReader(`{"model":"claude-opus-4-8","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", sessionID)
	if target != "" {
		req.Header.Set(remotePoolTargetAccountHeader, target)
	}
	return req
}

// awaitLastOKWrite holds the account store's TempDir cleanup until the
// last-ok file a served Claude turn writes in the background
// (claudeCredentialHealth.noteOK) has landed.
func awaitLastOKWrite(t *testing.T, ref *AccountRef) {
	t.Helper()
	t.Cleanup(func() {
		health := ref.claudeHealth()
		deadline := time.Now().Add(5 * time.Second)
		for {
			health.mu.Lock()
			flushing := health.flushing
			health.mu.Unlock()
			if !flushing {
				return
			}
			if time.Now().After(deadline) {
				t.Error("the last-ok write did not finish in 5s")
				return
			}
			time.Sleep(time.Millisecond)
		}
	})
}
