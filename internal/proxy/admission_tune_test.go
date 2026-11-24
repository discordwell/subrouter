package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
)

// fill starts n requests on key and returns them.
func fill(t *testing.T, control *admissionControl, key admissionKey, n int) []*admissionLease {
	t.Helper()
	leases := make([]*admissionLease, 0, n)
	for range n {
		lease, ok := control.tryAcquire(key, admissionSessionKey{})
		if !ok {
			t.Fatalf("request %d refused on %+v", len(leases), key)
		}
		leases = append(leases, lease)
	}
	t.Cleanup(func() {
		for _, lease := range leases {
			lease.release()
		}
	})
	return leases
}

func bucketLimit(control *admissionControl, key admissionKey) int {
	control.mu.Lock()
	defer control.mu.Unlock()
	if bucket := control.buckets[key]; bucket != nil {
		return bucket.limit
	}
	return control.limits.limitFor(key)
}

// throttle answers a transient Claude 429 on key at at: no quota headers.
func throttle(control *admissionControl, key admissionKey, at time.Time) {
	control.observe(key, http.StatusTooManyRequests, http.Header{}, at)
}

func tunedControl(t *testing.T, content string) (*admissionControl, *syncBuffer) {
	t.Helper()
	f := newLimitsFileFixture(t)
	f.apply(content)
	logs := &syncBuffer{}
	f.control.setLogger(slog.New(slog.NewTextHandler(logs, nil)))
	return f.control, logs
}

func TestAdmissionTuneRaisesABucketHeldAtItsLimit(t *testing.T) {
	control, logs := tunedControl(t, `{}`)
	key := admissionKeyFor(accounts.ProviderClaude, "owner", "claude-opus")
	fill(t, control, key, 6)
	results := queueAttempts(t, control, key, 1)
	now := time.Now()

	control.tune(now)
	if got := bucketLimit(control, key); got != 7 {
		t.Fatalf("a bucket with a request waiting at its limit tuned to %d, want 7", got)
	}
	expectGranted(t, results, 0).release()
	if line := logs.String(); !strings.Contains(line, `msg="admission limit raised"`) ||
		!strings.Contains(line, "account=owner") || !strings.Contains(line, "from=6") || !strings.Contains(line, "to=7") {
		t.Fatalf("the raise was not logged with its account and values:\n%s", line)
	}

	control.tune(now.Add(admissionTuneStep))
	if got := bucketLimit(control, key); got != 7 {
		t.Fatalf("a step with nothing waiting raised the limit to %d, want it held at 7", got)
	}
}

func TestAdmissionTuneStopsAtItsMax(t *testing.T) {
	control, _ := tunedControl(t, `{"tune": {"max": 7}}`)
	key := admissionKeyFor(accounts.ProviderClaude, "owner", "claude-opus")
	fill(t, control, key, 6)
	now := time.Now()
	for step := range 3 {
		results := queueAttempts(t, control, key, 1)
		control.tune(now.Add(time.Duration(step) * admissionTuneStep))
		if step == 0 {
			expectGranted(t, results, 0)
		}
	}
	if got := bucketLimit(control, key); got != 7 {
		t.Fatalf("limit tuned to %d past max 7", got)
	}
}

func TestAdmissionTuneBacksOffOnAThrottleAndWaitsBeforeRising(t *testing.T) {
	control, logs := tunedControl(t, `{"limit": 12}`)
	key := admissionKeyFor(accounts.ProviderClaude, "owner", "claude-opus")
	// An hour ago, so the pauses these throttles set are long over.
	t0 := time.Now().Add(-time.Hour)

	for _, step := range []struct {
		at   time.Duration
		want int
	}{
		{0, 9},                // 12 less a quarter
		{5 * time.Second, 9},  // inside the hold: one burst is one backoff
		{11 * time.Second, 7}, // 9 less 2
		{22 * time.Second, 6}, // less at least 1
		{33 * time.Second, 5}, //
		{44 * time.Second, 4}, // the floor
		{55 * time.Second, 4}, // stays at the floor
	} {
		throttle(control, key, t0.Add(step.at))
		if got := bucketLimit(control, key); got != step.want {
			t.Fatalf("after a throttle at +%v the limit is %d, want %d", step.at, got, step.want)
		}
	}
	if line := logs.String(); !strings.Contains(line, `msg="admission limit lowered"`) || !strings.Contains(line, "from=12") ||
		!strings.Contains(line, "to=9") {
		t.Fatalf("the backoff was not logged with its values:\n%s", line)
	}

	lastThrottle := t0.Add(55 * time.Second)
	fill(t, control, key, 4)
	results := queueAttempts(t, control, key, 1)
	control.tune(lastThrottle.Add(admissionTuneQuiet - time.Second))
	if got := bucketLimit(control, key); got != 4 {
		t.Fatalf("raised to %d within the quiet time after a throttle, want 4", got)
	}
	control.tune(lastThrottle.Add(admissionTuneQuiet + admissionTuneStep))
	if got := bucketLimit(control, key); got != 5 {
		t.Fatalf("still %d after a quiet stretch with a request waiting, want 5", got)
	}
	expectGranted(t, results, 0).release()
}

func TestAdmissionTuneWaitsOutARecoveryRamp(t *testing.T) {
	control, _ := tunedControl(t, `{}`)
	key := admissionKeyFor(accounts.ProviderClaude, "owner", "claude-opus")
	// A queue built while the bucket ramps back from a pause is the ramp's,
	// not the limit's: it must not count as demand for a higher limit.
	control.mu.Lock()
	bucket := control.bucketLocked(key)
	bucket.pausedTil, bucket.recoverSlowly = time.Now().Add(-500*time.Millisecond), true
	control.mu.Unlock()
	fill(t, control, key, 1)
	results := queueAttempts(t, control, key, 1)
	control.tune(time.Now())
	if got := bucketLimit(control, key); got != 6 {
		t.Fatalf("a queue behind the recovery ramp raised the limit to %d, want 6", got)
	}
	expectGranted(t, results, 0).release()
}

func TestAdmissionTuneLeavesOtherProvidersAlone(t *testing.T) {
	control, _ := tunedControl(t, `{}`)
	key := admissionKeyFor(accounts.ProviderCodex, "owner", "gpt-5.5")
	fill(t, control, key, 6)
	results := queueAttempts(t, control, key, 1)
	control.tune(time.Now())
	throttle(control, key, time.Now().Add(-time.Hour))
	if got := bucketLimit(control, key); got != 6 {
		t.Fatalf("a Codex bucket tuned to %d, want its configured 6", got)
	}
	expectNoGrant(t, results)
}

func TestAdmissionTuneOffKeepsTheConfiguredLimit(t *testing.T) {
	control, _ := tunedControl(t, `{"tune": {"on": false}}`)
	key := admissionKeyFor(accounts.ProviderClaude, "owner", "claude-opus")
	fill(t, control, key, 6)
	queueAttempts(t, control, key, 1)
	control.tune(time.Now())
	throttle(control, key, time.Now().Add(-time.Hour))
	if got := bucketLimit(control, key); got != 6 {
		t.Fatalf("tuning off still moved the limit to %d", got)
	}
}

func TestAdmissionTuneAStartAboveMaxIsItsBucketsCeiling(t *testing.T) {
	control, _ := tunedControl(t, `{"per_account": {"claw-1": 30}}`)
	key := admissionKeyFor(accounts.ProviderClaude, "claw-1", "claude-opus")
	if got := admitted(t, control, key); got != 30 {
		t.Fatalf("claw-1 admits %d at once, want 30 though tune max is 24", got)
	}
	throttle(control, key, time.Now().Add(-time.Hour))
	if got := bucketLimit(control, key); got != 23 {
		t.Fatalf("a throttle took 30 to %d, want 23", got)
	}
	fill(t, control, key, 23)
	for step := range 10 {
		results := queueAttempts(t, control, key, 1)
		control.tune(time.Now().Add(time.Duration(step) * admissionTuneStep))
		if step < 7 {
			expectGranted(t, results, 0)
		}
	}
	if got := bucketLimit(control, key); got != 30 {
		t.Fatalf("tuned back to %d, want its configured 30", got)
	}
}

func TestAdmissionLimitsChangeRestartsTuningOnlyWhereTheStartChanged(t *testing.T) {
	f := newLimitsFileFixture(t)
	f.apply(`{}`)
	opus := admissionKeyFor(accounts.ProviderClaude, "owner", "claude-opus")
	sonnet := admissionKeyFor(accounts.ProviderClaude, "owner", "claude-sonnet")
	now := time.Now()
	for _, key := range []admissionKey{opus, sonnet} {
		fill(t, f.control, key, 6)
		for step := range 2 {
			results := queueAttempts(t, f.control, key, 1)
			f.control.tune(now.Add(time.Duration(step) * admissionTuneStep))
			expectGranted(t, results, 0)
		}
	}
	if bucketLimit(f.control, opus) != 8 || bucketLimit(f.control, sonnet) != 8 {
		t.Fatalf("setup: tuned to %d and %d, want 8 and 8", bucketLimit(f.control, opus), bucketLimit(f.control, sonnet))
	}

	f.apply(`{"per_model": {"opus": 10}}`)
	if got := bucketLimit(f.control, opus); got != 10 {
		t.Fatalf("a new opus start left its bucket at %d, want 10", got)
	}
	if got := bucketLimit(f.control, sonnet); got != 8 {
		t.Fatalf("an unrelated change reset sonnet's tuned limit to %d, want 8 kept", got)
	}

	f.apply(`{"per_model": {"opus": 10}, "tune": {"max": 7}}`)
	if got := bucketLimit(f.control, sonnet); got != 7 {
		t.Fatalf("a lower max left sonnet at %d, want 7", got)
	}
	f.apply(`{"per_model": {"opus": 10}, "tune": {"on": false}}`)
	if got := bucketLimit(f.control, sonnet); got != 6 {
		t.Fatalf("tuning off left sonnet at %d, want its configured 6", got)
	}
}

func TestAdmissionLimitsParseTune(t *testing.T) {
	limits, err := parseAdmissionLimits([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if limits.Tune != (admissionTune{On: true, Min: 4, Max: 24}) {
		t.Fatalf("default tuning = %+v, want on, 4 to 24", limits.Tune)
	}
	limits, err = parseAdmissionLimits([]byte(`{"tune": {"min": 2}}`))
	if err != nil || limits.Tune != (admissionTune{On: true, Min: 2, Max: 24}) {
		t.Fatalf(`{"tune": {"min": 2}} = %+v, %v`, limits.Tune, err)
	}
	for _, bad := range []string{`{"tune": {"min": 0}}`, `{"tune": {"min": 9, "max": 8}}`, `{"tune": {"step": 3}}`, `{"tune": 1}`} {
		if got, err := parseAdmissionLimits([]byte(bad)); err == nil {
			t.Errorf("%s parsed to %s, want an error", bad, got)
		}
	}
}

// A gate StartAdmission never started (a tenant pool's) has no tuning clock
// to raise it again, so throttles must not lower it either: before the fix
// it fell to the floor and stayed there until a restart.
func TestAdmissionTuneNeedsTheGatesClock(t *testing.T) {
	control := newAdmissionControl()
	key := admissionKeyFor(accounts.ProviderClaude, "owner", "claude-opus")
	t0 := time.Now().Add(-time.Hour)
	for step := range 4 {
		throttle(control, key, t0.Add(time.Duration(step)*11*time.Second))
	}
	if got := admitted(t, control, key); got != 6 {
		t.Fatalf("an unstarted gate admits %d after four throttles, want its configured 6", got)
	}
}

func TestAdmissionTuneIgnoresARequestQueuedBehindTheRamp(t *testing.T) {
	control, _ := tunedControl(t, `{}`)
	key := admissionKeyFor(accounts.ProviderClaude, "owner", "claude-opus")
	control.mu.Lock()
	bucket := control.bucketLocked(key)
	bucket.pausedTil, bucket.recoverSlowly = time.Now().Add(-200*time.Millisecond), true
	control.mu.Unlock()
	leases := fill(t, control, key, 1)
	results := queueAttempts(t, control, key, 1)
	leases[0].release()
	expectGranted(t, results, 0).release()
	control.mu.Lock()
	bucket.pausedTil = time.Now().Add(-time.Hour)
	control.mu.Unlock()
	control.tune(time.Now())
	if got := bucketLimit(control, key); got != 6 {
		t.Fatalf("a request that queued behind the ramp raised the limit to %d, want 6", got)
	}
}

func TestAdmissionTuneCountsAQueueFullRefusalAsWaiting(t *testing.T) {
	control, _ := tunedControl(t, `{"queue": 0}`)
	key := admissionKeyFor(accounts.ProviderClaude, "owner", "claude-opus")
	fill(t, control, key, 6)
	if _, err := control.acquire(context.Background(), key); !errors.Is(err, errAdmissionQueueFull) {
		t.Fatalf("a seventh request with no queue got %v, want queue full", err)
	}
	control.tune(time.Now())
	if got := bucketLimit(control, key); got != 7 {
		t.Fatalf("with no queue a refusal at the limit left it at %d, want 7", got)
	}
}
