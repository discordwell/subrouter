package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
)

// PATCHES §98 as serve runs it: serve reads SUBROUTER_PIN_ONLY_ACCOUNTS from
// its own environment, and the pool, the views and the sr auto-switch all
// see the list. serve runs in a child of the test binary over two stored
// Codex accounts with the first listed. Their usage reads go to a fake
// chatgpt.com here, and the listed account reads the most headroom, so it is
// the one anything unaware of the list would pick.
func TestServeKeepsTheAccountItsEnvironmentListsForPinnedSessions(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, "state", "codex", "accounts")}
	weekUsed := map[string]float64{"acct_listed": 5, "acct_spare": 60}
	for _, name := range []string{"listed", "spare"} {
		email := name + "@example.com"
		if err := store.SaveStored(accounts.StoredCodexAccount{
			Email:                 email,
			OAuthCredentialOrigin: accounts.CodexOAuthOriginIsolatedServerLogin,
			AddedAt:               time.Now().UTC().Format(time.RFC3339),
			Auth:                  codexAuthValidFor(email, "acct_"+name, 30*24*time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := store.List()
	if err != nil || len(stored) != 2 {
		t.Fatalf("stored Codex accounts %+v (err %v), want the two saved", stored, err)
	}
	idOf := map[string]string{}
	for _, account := range stored {
		idOf[account.AccountID] = account.ID
	}
	listed, spare := idOf["acct_listed"], idOf["acct_spare"]
	if listed == "" || spare == "" {
		t.Fatalf("stored Codex accounts %+v, want acct_listed and acct_spare", stored)
	}

	now := time.Now()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		used, ok := weekUsed[r.Header.Get("ChatGPT-Account-ID")]
		if r.Method != http.MethodGet || r.URL.Path != "/backend-api/wham/usage" || !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan_type": "pro",
			"rate_limit": map[string]any{
				"limit_reached":    false,
				"primary_window":   map[string]any{"used_percent": used / 2, "limit_window_seconds": 18000, "reset_at": now.Add(2 * time.Hour).Unix()},
				"secondary_window": map[string]any{"used_percent": used, "limit_window_seconds": 604800, "reset_at": now.Add(72 * time.Hour).Unix()},
			},
			"rate_limit_reset_credits": map[string]any{"available_count": 0},
		})
	}))
	t.Cleanup(fake.Close)

	env := append(serveChildSandbox(home),
		serveChildChatGPTEnv+"="+fake.Listener.Addr().String(),
		"SUBROUTER_PIN_ONLY_ACCOUNTS="+listed,
		"SUBROUTER_SELF_START_WEEK=0", "SUBROUTER_AUTO_RESET=")
	child := startServeChild(t, env, "--sessions", filepath.Join(home, "sessions", "sessions.json"),
		"--sr-switch-interval=2s", "--reset-credit-autospend", "off")

	get := func(path string, into any) {
		t.Helper()
		resp, err := http.Get(child.base + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || json.Unmarshal(body, into) != nil {
			t.Fatalf("%s = %d %s; serve's output:\n%s", path, resp.StatusCode, body, child.output.String())
		}
	}

	// What a peer reads: the listed account is not eligible, for pin_only.
	var statuses []struct {
		ID         string `json:"id"`
		Normalized struct {
			NewSession *selectacct.NewSessionVerdict `json:"new_session"`
		} `json:"normalized"`
	}
	get("/_subrouter/usage-status?refresh=1", &statuses)
	verdicts := map[string]*selectacct.NewSessionVerdict{}
	for _, status := range statuses {
		verdicts[status.ID] = status.Normalized.NewSession
	}
	if got := verdicts[listed]; got == nil || got.Eligible || got.Reason != selectacct.VerdictPinOnly {
		t.Fatalf("usage-status verdict for the listed account %+v, want not eligible for pin_only; serve's output:\n%s", got, child.output.String())
	}
	if got := verdicts[spare]; got == nil || !got.Eligible {
		t.Fatalf("usage-status verdict for the other account %+v, want eligible; serve's output:\n%s", got, child.output.String())
	}

	var board struct {
		Rows []struct {
			ID      string `json:"id"`
			PinOnly bool   `json:"pin_only"`
		} `json:"rows"`
	}
	get("/_subrouter/pool.json", &board)
	marked := map[string]bool{}
	for _, row := range board.Rows {
		marked[row.ID] = row.PinOnly
	}
	if !marked[listed] || marked[spare] {
		t.Fatalf("pool.json pin_only %v, want the listed account %s alone", marked, listed)
	}

	// The sr auto-switch sweeps every 2 s and logs its pick.
	const picked = "sr auto-switch selected account"
	for deadline := time.Now().Add(60 * time.Second); ; {
		var line string
		for _, candidate := range strings.Split(child.output.String(), "\n") {
			if strings.Contains(candidate, picked) {
				line = candidate
				break
			}
		}
		if line != "" {
			if !strings.Contains(line, spare) || strings.Contains(line, listed) {
				t.Fatalf("serve's sr auto-switch: %q, want the other account %s, never the listed %s", line, spare, listed)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve's sr auto-switch logged no pick within 60s; its output:\n%s", child.output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}
