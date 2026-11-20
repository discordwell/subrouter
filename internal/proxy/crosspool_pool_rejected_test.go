package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/selectacct"
)

// PATCHES §31, R3 fix 4 (D4b): a Claude 429 rejected by a MODEL-POOL window
// alone -- Fable's weekly 7d_oi bucket said rejected, the account-wide 5h/7d
// did not -- is a spent window for that model, so it is a FORCED switch.
// claudeAccountExhaustedByResponse keeps Exhausted false for it (the account
// still serves every other model), and before this row the classifier read it
// as a limited-not-spent 429: the failover served the turn elsewhere and the
// session KEPT the account (transientFailoverKeepsPin), so every later Fable
// turn went back to an account that rejects Fable until its week rolls.

const fableTurn = `{"model":"claude-fable-5","messages":[{"role":"user","content":"hi"}]}`

func fablePoolRejected(w http.ResponseWriter) {
	w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
	w.Header().Set("anthropic-ratelimit-unified-7d_oi-status", "rejected")
	w.Header().Set("anthropic-ratelimit-unified-5h-status", "allowed")
	w.Header().Set("anthropic-ratelimit-unified-7d-status", "allowed")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"weekly fable limit"}}`)
}

func postFableTurn(t *testing.T, handler http.Handler, sessionID string) (int, string) {
	t.Helper()
	front := httptest.NewServer(handler)
	defer front.Close()
	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/messages", strings.NewReader(fableTurn))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", sessionID)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	served := ""
	if i := strings.Index(string(body), `"served_by":"`); i >= 0 {
		rest := string(body)[i+len(`"served_by":"`):]
		served = rest[:strings.Index(rest, `"`)]
	}
	return response.StatusCode, served
}

func TestAFableOnlyRejectionMovesTheSessionForThatModel(t *testing.T) {
	laptop := newCrossPoolLaptop(t, []selectacct.Score{
		crossPoolClaudeScore("a", 0.9, 150*time.Hour),
		crossPoolClaudeScore("b", 0.6, 150*time.Hour),
	}, nil)
	if status, served := postFableTurn(t, laptop.server.Handler(), "s1"); status != http.StatusOK || served != "a" {
		t.Fatalf("calibration: turn 1 status %d served by %q; want a, the best account", status, served)
	}
	var rejected atomic.Int32
	laptop.upstream.mu.Lock()
	laptop.upstream.answer = func(account string, w http.ResponseWriter) bool {
		if account != "a" {
			return false
		}
		rejected.Add(1)
		fablePoolRejected(w)
		return true
	}
	laptop.upstream.mu.Unlock()

	if status, served := postFableTurn(t, laptop.server.Handler(), "s1"); status != http.StatusOK || served != "b" {
		t.Fatalf("turn 2 status %d served by %q; want the failover to b", status, served)
	}
	if rejected.Load() != 1 {
		t.Fatalf("calibration: a answered %d rejections; want the one that forced turn 2 off it", rejected.Load())
	}
	if pinned, _ := laptop.store.Get("claude", "s1"); pinned.AccountID != "b" {
		t.Fatalf("pin after a Fable-only rejection = %q; a spent model-pool window is a forced switch, so b is committed", pinned.AccountID)
	}
	if kept := laptop.logLines("transient failover served this turn; the session keeps its account"); len(kept) != 0 {
		t.Fatalf("the rejection was read as transient: %v", kept)
	}
	if status, served := postFableTurn(t, laptop.server.Handler(), "s1"); status != http.StatusOK || served != "b" || rejected.Load() != 1 {
		t.Fatalf("turn 3 status %d served by %q after %d rejections; the session must stay on b, not return to a", status, served, rejected.Load())
	}
}
