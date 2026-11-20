package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// newAdmissionSpillFixtureScored is newAdmissionSpillFixtureWith with each
// account's own usage windows and the owner's holdout (AccountReserves).
func newAdmissionSpillFixtureScored(t *testing.T, ids []string, windows map[string][]accounts.UsageWindow, reserves map[string]float64) *admissionSpillFixture {
	t.Helper()
	var accountList []accounts.Account
	var scores []selectacct.Score
	for _, id := range ids {
		accountList = append(accountList, accounts.Account{
			ID: id, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-" + id,
		})
		scores = append(scores, scoreFromUsageWindows(accounts.ProviderClaude, id, windows[id]))
	}
	f := &admissionSpillFixture{control: testAdmissionSpillControl(1, 4, 1)}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.upstream = append(f.upstream, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","content":[]}`))
	}))
	t.Cleanup(upstream.Close)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	f.store = store
	f.server = Server{
		Sessions:        store,
		ClaudeUpstream:  mustParseURL(t, upstream.URL),
		Accounts:        accountList,
		SchedulerRef:    selectacct.NewSchedulerRef(selectacct.NewScheduler(scores)),
		AccountReserves: reserves,
		MaxBodyBytes:    1 << 20,
		admission:       f.control,
	}
	f.handler = f.server.Handler()
	return f
}

// The admission spill (upstream #608/#609) starts one request on another
// account. It must see the owner's holdout like every other pick (PATCHES
// §20: "both failover pickers built schedulers without WithReserves" was a
// holdout leak). Without reserves the held-out account -- idle, because
// selection starts nothing on it, so the least loaded -- was the spill's first
// choice.
func TestAdmissionSpillNeverLandsInTheOwnersHoldout(t *testing.T) {
	now := time.Now()
	healthy := []accounts.UsageWindow{fiveHour(10, now.Add(2*time.Hour)), sevenDay(10, now.Add(72*time.Hour))}
	windows := map[string][]accounts.UsageWindow{
		"a@example.com": healthy,
		"b@example.com": healthy,
		// 10% of the week left, under its 30% holdout; the 5h window is idle.
		"held@example.com": {fiveHour(0, now.Add(2*time.Hour)), sevenDay(90, now.Add(72*time.Hour))},
	}
	ids := []string{"a@example.com", "b@example.com", "held@example.com"}
	spill := func(reserves map[string]float64) (accounts.Account, bool) {
		f := newAdmissionSpillFixtureScored(t, ids, windows, reserves)
		f.saturate("a@example.com", 1) // the sticky account: spill it
		f.saturate("b@example.com", 0) // busy: no free slot
		attempt := &upstreamAttempt{
			server: &f.server, provider: accounts.ProviderClaude, poolModel: "claude-opus",
			getBody: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("{}")), nil },
		}
		transport := usageLimitRetryTransport{server: &f.server, provider: accounts.ProviderClaude, poolModel: "claude-opus", attempt: attempt}
		next, lease, ok := transport.admissionSpill(context.Background(), "a@example.com", map[string]struct{}{})
		lease.release()
		return next, ok
	}
	// Calibration: with no holdout configured the idle account is the target.
	if next, ok := spill(nil); !ok || next.ID != "held@example.com" {
		t.Fatalf("without reserves spill = %q %v, want the idle account", next.ID, ok)
	}
	if next, ok := spill(map[string]float64{"held@example.com": 0.3}); ok {
		t.Fatalf("spill went to %q; an account inside its holdout takes nothing new", next.ID)
	}
}

// Selection moving a session off a spent pin leaves the move pending until
// an account serves (PATCHES §25: the pin then moves and stays). A spill off
// selection's busy pick used to drop that move -- spilled requests never move
// a session -- so the session stayed on the spent account and every later
// turn re-picked. The account that served takes the move, and the live debit
// follows the request there (§26), off the account it never reached.
func TestAdmissionSpillDuringAPendingMoveCommitsToTheServingAccount(t *testing.T) {
	now := time.Now()
	windows := map[string][]accounts.UsageWindow{
		"spent@example.com": {fiveHour(100, now.Add(2*time.Hour)), sevenDay(40, now.Add(72*time.Hour))},
		"best@example.com":  {fiveHour(10, now.Add(2*time.Hour)), sevenDay(10, now.Add(72*time.Hour))},
		"c@example.com":     {fiveHour(10, now.Add(2*time.Hour)), sevenDay(60, now.Add(72*time.Hour))},
		"d@example.com":     {fiveHour(10, now.Add(2*time.Hour)), sevenDay(60, now.Add(72*time.Hour))},
	}
	f := newAdmissionSpillFixtureScored(t, []string{"spent@example.com", "best@example.com", "c@example.com", "d@example.com"}, windows, nil)
	const sessionID = "session-leaving-spent"
	if _, err := f.store.Put("claude", sessionID, "spent@example.com", ""); err != nil {
		t.Fatal(err)
	}
	f.saturate("best@example.com", 1)

	response := f.send(t, sessionID)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	if sessions := f.control.snapshot().Sessions; len(sessions) != 1 || sessions[0].Spilled != 1 {
		t.Fatalf("sessions = %+v; calibration: the request must have spilled off selection's pick", sessions)
	}
	served := strings.TrimPrefix(f.lastUpstreamToken(), "tok-")
	if served == "spent@example.com" || served == "best@example.com" {
		t.Fatalf("served by %q, want a spill target", served)
	}
	if assignment, _ := f.store.Get("claude", sessionID); assignment.AccountID != served {
		t.Fatalf("session pinned to %q after the spill served it on %q; the pending move off the spent pin was dropped", assignment.AccountID, served)
	}
	debits := f.server.SchedulerRef.LiveDebits()
	if debits[selectacct.ScoreKey(accounts.ProviderClaude, served)] != 1 || debits[selectacct.ScoreKey(accounts.ProviderClaude, "best@example.com")] != 0 {
		t.Fatalf("live debits = %v, want one on %s and none on the account the request never reached", debits, served)
	}
}
