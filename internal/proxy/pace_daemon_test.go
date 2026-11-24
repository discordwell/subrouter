package proxy

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	agentclaude "github.com/discordwell/subrouter/internal/agents/claude"
	"github.com/discordwell/subrouter/selectacct"
)

func daemonShaped(t *testing.T, box *targetBox) {
	t.Helper()
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
	box.server.ActiveSessions = nil
	box.server.admission = nil
	box.active = nil
}

func pacedLines(logs, account string) []string {
	var out []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "paced allowance") && strings.Contains(line, "account="+account+" ") {
			out = append(out, line)
		}
	}
	return out
}

func TestThePaceSamplerReadsTheDaemonsSharedSessionsAndGate(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	box := raisedTargetBox(t)
	daemonShaped(t, box)
	gate := box.server.AccountRef.sharedAdmission()
	key := opusKey("roomy")
	var leases []*admissionLease
	for range gate.limits.Limit - 1 {
		lease, err := gate.acquire(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, lease)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	box.upstream.respond = func(account string, w http.ResponseWriter) {
		if account == "roomy" && held.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		w.Header().Set("anthropic-ratelimit-unified-status", "allowed")
		_, _ = w.Write([]byte(`{"id":"msg"}`))
	}
	handler := httptest.NewServer(box.server.Handler())
	t.Cleanup(handler.Close)
	var turns sync.WaitGroup
	send := func(sessionID string) {
		box.pin(t, sessionID, "roomy")
		turns.Add(1)
		go func() {
			defer turns.Done()
			if response, err := http.DefaultClient.Do(targetTurnRequest(t, handler.URL, sessionID, "")); err == nil {
				response.Body.Close()
			}
		}()
	}
	// Each turn writes its pin when it ends; let both finish before TempDir
	// cleanup.
	defer func() {
		close(release)
		for _, lease := range leases {
			lease.release()
		}
		turns.Wait()
	}()
	send("admitted")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the admitted turn never reached roomy upstream")
	}
	send("queued-1")
	send("queued-2")
	waiting := &filledQueue{gate: gate, key: key}
	waiting.await(t, 2)
	box.pin(t, "parked", "roomy")

	state := newPaceSamplerState()
	now := time.Now()
	box.server.samplePace(now, state)
	before, _ := box.server.SchedulerRef.LiveSessionHours(accounts.ProviderClaude, "roomy", now)
	box.server.samplePace(now.Add(30*time.Minute), state)
	after, sampled := box.server.SchedulerRef.LiveSessionHours(accounts.ProviderClaude, "roomy", now.Add(30*time.Minute))
	if hours := after - before; !sampled || hours < 0.499 || hours > 0.501 {
		t.Fatalf("roomy gained %v live session-hours (sampled %v), want its one admitted turn for half an hour = 0.5", hours, sampled)
	}
	if allowance := box.server.SchedulerRef.Get().PaceAllowance(accounts.ProviderClaude, "roomy", time.Now()); allowance.Backpressured || allowance.Paced != roomyRaised {
		t.Fatalf("roomy allowance %+v, want the queued turns to leave it raised to %d", allowance, roomyRaised)
	}
	lines := pacedLines(box.logs.String(), "roomy")
	if len(lines) == 0 || !strings.Contains(lines[0], "live_sessions=3") || !strings.Contains(lines[0], "backpressured=false") {
		t.Fatalf("want roomy's first paced allowance line with 3 live and not backpressured; got:\n%s", strings.Join(lines, "\n"))
	}
}

func TestThePaceSamplerReadsTheDaemonsSharedCostStore(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	box := raisedTargetBox(t)
	daemonShaped(t, box)
	box.server.AccountRef.claudeCostStore().markExhausted("roomy")
	readRefusal(t, box.turn(t, "new-session", "roomy", nil), selectacct.VerdictExhausted)
	box.server.samplePace(time.Now(), newPaceSamplerState())
	lines := pacedLines(box.logs.String(), "roomy")
	if len(lines) != 1 || !strings.Contains(lines[0], "in_burndown=false") || !strings.Contains(lines[0], "paced_sessions=0") {
		t.Fatalf("want one paced allowance line reading roomy spent: out of burndown, no raise; got:\n%s", strings.Join(lines, "\n"))
	}
}

type heldTurns struct {
	t       *testing.T
	url     string
	entered chan struct{}
	release chan struct{}
	done    sync.WaitGroup
}

func holdTurns(t *testing.T, box *targetBox) *heldTurns {
	t.Helper()
	turns := &heldTurns{t: t, entered: make(chan struct{}, 64), release: make(chan struct{})}
	box.upstream.respond = func(account string, w http.ResponseWriter) {
		turns.entered <- struct{}{}
		<-turns.release
		w.Header().Set("anthropic-ratelimit-unified-status", "allowed")
		_, _ = w.Write([]byte(`{"id":"msg"}`))
	}
	handler := httptest.NewServer(box.server.Handler())
	turns.url = handler.URL
	t.Cleanup(func() {
		close(turns.release)
		turns.done.Wait()
		handler.Close()
	})
	return turns
}

func (h *heldTurns) send(sessionID string) {
	h.done.Add(1)
	go func() {
		defer h.done.Done()
		if response, err := http.DefaultClient.Do(targetTurnRequest(h.t, h.url, sessionID, "")); err == nil {
			response.Body.Close()
		}
	}()
}

func (h *heldTurns) awaitEntered(n int) {
	h.t.Helper()
	for range n {
		select {
		case <-h.entered:
		case <-time.After(5 * time.Second):
			h.t.Fatal("a turn never reached upstream")
		}
	}
}

func TestAQueuedBurstAddsNoLiveSessionHours(t *testing.T) {
	setPacedAllowanceForTest(t, true)
	box, gate := gatedTargetBox(t, "roomy")
	ref := box.server.SchedulerRef
	key := opusKey("roomy")
	turns := holdTurns(t, box)
	var leases []*admissionLease
	for range gate.limits.Limit - 2 {
		lease, err := gate.acquire(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, lease)
	}
	t.Cleanup(func() {
		for _, lease := range leases {
			lease.release()
		}
	})
	for _, id := range []string{"admitted-1", "admitted-2", "queued-1", "queued-2", "queued-3", "queued-4"} {
		box.pin(t, id, "roomy")
	}
	turns.send("admitted-1")
	turns.send("admitted-2")
	turns.awaitEntered(2)
	waiting := &filledQueue{gate: gate, key: key}
	for i := range 4 {
		turns.send(fmt.Sprintf("queued-%d", i+1))
		waiting.await(t, i+1)
	}

	t0 := time.Now()
	reset := t0.Add(106*time.Minute + 8*time.Hour)
	readAt := func(at time.Duration, remaining float64) {
		score := roomyBehindPace(t0)
		score.MeasuredAt, score.WeeklyResetAt = t0.Add(at), reset
		score.Headroom, score.WeeklyHeadroom, score.WeeklyRemaining = remaining, remaining, remaining
		publishReading(ref, score)
	}
	state := newPaceSamplerState()
	box.server.samplePace(t0, state)
	readAt(0, 0.4525)
	leases[0].release()
	leases = leases[1:]
	turns.awaitEntered(1)
	waiting.await(t, 3)
	box.server.samplePace(t0.Add(time.Hour), state)
	readAt(time.Hour, 0.4225)
	if got := ref.Get().PaceAllowance(accounts.ProviderClaude, "roomy", t0.Add(time.Hour)).Pace; got.Source != "account" || math.Abs(got.BurnPerLive-0.015) > 1e-9 {
		t.Fatalf("an hour of 2 admitted and 4 queued turns read %+v, want 0.03 over 2 admitted session-hours = 0.015", got)
	}
	box.server.samplePace(t0.Add(90*time.Minute), state)
	readAt(90*time.Minute, 0.40)
	box.server.samplePace(t0.Add(106*time.Minute), state)
	got := ref.Get().PaceAllowance(accounts.ProviderClaude, "roomy", t0.Add(106*time.Minute))
	if got.Backpressured {
		t.Fatalf("calibration: the queue's backpressure has not lapsed 106 minutes on: %+v", got)
	}
	if got.Pace.Source != "account" || math.Abs(got.Pace.BurnPerLive-0.015) > 1e-9 || got.Paced != 0 || got.Sessions != 4 {
		t.Fatalf("after a waiter was granted %+v, want 0.0525 over 3.5 admitted session-hours = 0.015, no raise, the taper of 4", got)
	}
}
