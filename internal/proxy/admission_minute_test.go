package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
)

func minuteBucket(t *testing.T, report admissionMinuteReport, key admissionKey) admissionMinuteBucket {
	t.Helper()
	for _, bucket := range report.buckets {
		if bucket.key == key {
			return bucket
		}
	}
	t.Fatalf("no minute line for %+v in %+v", key, report.buckets)
	return admissionMinuteBucket{}
}

func TestAdmissionMinuteCountsABusyBucket(t *testing.T) {
	control := testAdmissionControl(1, 1)
	key := admissionKeyFor(accounts.ProviderClaude, "owner", "claude-opus")
	target := admissionKeyFor(accounts.ProviderClaude, "claw-1", "claude-opus")
	who := admissionSessionKey{agent: "claude", session: "fanout-session-1"}

	running, err := control.acquireFor(context.Background(), key, who)
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan *admissionLease, 1)
	go func() {
		lease, _ := control.acquireFor(context.Background(), key, who)
		waiting <- lease
	}()
	waitFor(t, "the second request to queue", func() bool { return queuedOn(control, key) == 1 })
	if _, err := control.acquireFor(context.Background(), key, who); !errors.Is(err, errAdmissionQueueFull) {
		t.Fatalf("third request got %v, want a full queue", err)
	}
	time.Sleep(20 * time.Millisecond)
	running.release()
	(<-waiting).release()

	spilled, ok := control.tryAcquire(target, who)
	if !ok {
		t.Fatal("spill target refused")
	}
	control.noteSpill(key, target, who)
	spilled.release()

	now := time.Now()
	control.observe(key, http.StatusTooManyRequests, http.Header{}, now)
	control.observe(key, http.StatusTooManyRequests, http.Header{}, now)

	report := control.rollMinute(now)
	got := minuteBucket(t, report, key)
	if got.served != 2 || got.waited != 1 || got.queueFull != 1 || got.spillsOut != 1 || got.pauses != 1 {
		t.Errorf("counts = served %d waited %d queue_full %d spills_out %d pauses %d, want 2 1 1 1 1",
			got.served, got.waited, got.queueFull, got.spillsOut, got.pauses)
	}
	if got.peakActive != 1 || got.peakQueued != 1 || got.peakDemand != 2 {
		t.Errorf("peaks = active %d queued %d demand %d, want 1 1 2", got.peakActive, got.peakQueued, got.peakDemand)
	}
	if got.waitMax < 20*time.Millisecond || got.waitTotal != got.waitMax {
		t.Errorf("wait max %v total %v, want one wait of at least 20ms", got.waitMax, got.waitTotal)
	}
	if got.limit != 1 || got.effectiveLimit != 0 {
		t.Errorf("limit %d effective %d, want 1 and 0 while paused", got.limit, got.effectiveLimit)
	}
	if in := minuteBucket(t, report, target); in.spillsIn != 1 || in.served != 1 {
		t.Errorf("spill target: spills_in %d served %d, want 1 1", in.spillsIn, in.served)
	}
}

func TestAdmissionMinuteCarriesLoadAndSkipsIdleBuckets(t *testing.T) {
	control := testAdmissionControl(4, 8)
	key := admissionKeyFor(accounts.ProviderClaude, "owner", "claude-opus")
	start := time.Now()
	lease, ok := control.tryAcquire(key, admissionSessionKey{})
	if !ok {
		t.Fatal("first request refused")
	}
	control.rollMinute(start)

	// Still running a minute later, with nothing new: still busy.
	held := minuteBucket(t, control.rollMinute(start.Add(time.Minute)), key)
	if held.peakActive != 1 || held.active != 1 || held.served != 0 {
		t.Fatalf("a request held across the minute reads %+v, want peak 1, active 1, served 0", held)
	}
	lease.release()
	if released := minuteBucket(t, control.rollMinute(start.Add(2*time.Minute)), key); released.peakActive != 1 || released.active != 0 {
		t.Fatalf("the minute it was released reads %+v, want peak 1, active 0", released)
	}
	if report := control.rollMinute(start.Add(3 * time.Minute)); len(report.buckets) != 0 {
		t.Fatalf("an idle minute logged %+v, want nothing", report.buckets)
	}
}

func TestAdmissionMinuteListsTheBusiestSessions(t *testing.T) {
	control := testAdmissionControl(64, 64)
	var leases []*admissionLease
	for i := range 7 {
		who := admissionSessionKey{agent: "claude", session: fmt.Sprintf("session%d-0000-4000-8000-000000000000", i)}
		for range i + 1 {
			key := admissionKeyFor(accounts.ProviderClaude, fmt.Sprintf("account-%d", len(leases)%3), "claude-opus")
			lease, ok := control.tryAcquire(key, who)
			if !ok {
				t.Fatal("refused")
			}
			leases = append(leases, lease)
		}
	}
	report := control.rollMinute(time.Now())
	if len(report.sessions) != admissionMinuteSessions {
		t.Fatalf("listed %d sessions, want the top %d", len(report.sessions), admissionMinuteSessions)
	}
	for i, session := range report.sessions {
		if want := 7 - i; session.peak != want || session.active != want {
			t.Errorf("session %d: peak %d active %d, want %d", i, session.peak, session.active, want)
		}
	}
	for _, lease := range leases {
		lease.release()
	}
	control.rollMinute(time.Now())
	if report := control.rollMinute(time.Now()); len(report.sessions) != 0 {
		t.Fatalf("idle sessions still listed: %+v", report.sessions)
	}
}

func TestAdmissionMinuteLogLines(t *testing.T) {
	control := testAdmissionControl(2, 8)
	logs := &syncBuffer{}
	key := admissionKeyFor(accounts.ProviderClaude, "owner", "claude-opus")
	who := admissionSessionKey{agent: "claude", session: "fanoutse-7c1f-4b0e-9d55-2f1e5a7b9c3d"}
	lease, ok := control.tryAcquire(key, who)
	if !ok {
		t.Fatal("refused")
	}
	defer lease.release()
	control.logMinute(slog.New(slog.NewTextHandler(logs, nil)), time.Now())
	lines := logs.String()
	for _, want := range []string{
		`msg="admission minute"`, "provider=claude", "account=owner", "model=claude-opus", "limit=2",
		"effective_limit=2", "active=1", "queued=0", "peak_active=1", "peak_queued=0", "peak_demand=1",
		"served=1", "waited=0", "wait_avg_ms=0", "wait_max_ms=0", "spills_out=0", "spills_in=0",
		"queue_full=0", "pauses=0", "abandoned=0",
		`msg="admission minute session"`, "agent=claude", "session_prefix=fanoutse", "peak=1",
	} {
		if !strings.Contains(lines, want) {
			t.Errorf("minute lines lack %q:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, who.session) {
		t.Errorf("minute lines carry the whole session id:\n%s", lines)
	}
}

func TestAdmissionMinuteCarriesASessionsLoad(t *testing.T) {
	control := testAdmissionControl(4, 8)
	who := admissionSessionKey{agent: "claude", session: "holding-0000-4000-8000-000000000000"}
	key := admissionKeyFor(accounts.ProviderClaude, "owner", "claude-opus")
	var leases []*admissionLease
	for range 3 {
		lease, ok := control.tryAcquire(key, who)
		if !ok {
			t.Fatal("refused")
		}
		leases = append(leases, lease)
	}
	control.rollMinute(time.Now())
	report := control.rollMinute(time.Now())
	if len(report.sessions) != 1 || report.sessions[0].peak != 3 {
		t.Fatalf("a session holding 3 across the minute reads %+v, want peak 3", report.sessions)
	}
	for _, lease := range leases {
		lease.release()
	}
}

func TestAdmissionMinuteCountsAbandonedWaits(t *testing.T) {
	control := testAdmissionControl(1, 8)
	key := admissionKeyFor(accounts.ProviderClaude, "owner", "claude-opus")
	lease, ok := control.tryAcquire(key, admissionSessionKey{})
	if !ok {
		t.Fatal("refused")
	}
	defer lease.release()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := control.acquire(ctx, key)
		done <- err
	}()
	waitFor(t, "the request to queue", func() bool { return queuedOn(control, key) == 1 })
	cancel()
	<-done
	if got := minuteBucket(t, control.rollMinute(time.Now()), key); got.abandoned != 1 || got.waited != 0 {
		t.Fatalf("a client that gave up while queued reads abandoned %d waited %d, want 1 and 0", got.abandoned, got.waited)
	}
}
