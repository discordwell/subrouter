package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

type limitsFileFixture struct {
	t       *testing.T
	path    string
	logs    *syncBuffer
	control *admissionControl
	file    *admissionLimitsFile
}

func newLimitsFileFixture(t *testing.T) *limitsFileFixture {
	t.Helper()
	logs := &syncBuffer{}
	control := newAdmissionControl()
	control.setLimits(defaultAdmissionLimits()) // as StartAdmission does
	path := filepath.Join(t.TempDir(), "admission.json")
	logger := slog.New(slog.NewTextHandler(logs, nil))
	return &limitsFileFixture{t: t, path: path, logs: logs, control: control,
		file: newAdmissionLimitsFile(path, control, logger)}
}

func (f *limitsFileFixture) write(content string) {
	f.t.Helper()
	if err := os.WriteFile(f.path, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *limitsFileFixture) apply(content string) {
	f.t.Helper()
	f.write(content)
	f.file.poll()
}

// admitted counts how many requests key runs at once, then gives them back.
func admitted(t *testing.T, control *admissionControl, key admissionKey) int {
	t.Helper()
	var leases []*admissionLease
	for range 1000 {
		lease, ok := control.tryAcquire(key, admissionSessionKey{})
		if !ok {
			break
		}
		leases = append(leases, lease)
	}
	for _, lease := range leases {
		lease.release()
	}
	return len(leases)
}

type queuedAttempt struct {
	lease *admissionLease
	err   error
	order int
}

// queueAttempts queues n attempts on key one at a time, so their FIFO order
// is their index, and returns a channel of their outcomes as they finish.
func queueAttempts(t *testing.T, control *admissionControl, key admissionKey, n int) <-chan queuedAttempt {
	t.Helper()
	out := make(chan queuedAttempt, n)
	for i := range n {
		before := queuedOn(control, key)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			lease, err := control.acquire(ctx, key)
			out <- queuedAttempt{lease: lease, err: err, order: i}
		}()
		waitFor(t, fmt.Sprintf("attempt %d to queue", i), func() bool { return queuedOn(control, key) == before+1 })
	}
	return out
}

func queuedOn(control *admissionControl, key admissionKey) int {
	control.mu.Lock()
	defer control.mu.Unlock()
	if bucket := control.buckets[key]; bucket != nil {
		return len(bucket.waiters)
	}
	return 0
}

func expectGranted(t *testing.T, results <-chan queuedAttempt, order int) *admissionLease {
	t.Helper()
	select {
	case got := <-results:
		if got.err != nil || got.lease == nil {
			t.Fatalf("queued attempt %d: lease=%v err=%v, want a grant", got.order, got.lease, got.err)
		}
		if got.order != order {
			t.Fatalf("attempt %d granted before attempt %d: the queue was reordered", got.order, order)
		}
		return got.lease
	case <-time.After(2 * time.Second):
		t.Fatalf("attempt %d was never granted", order)
	}
	return nil
}

// expectGrantedFirst takes the next n outcomes, granted together: their
// goroutines report in any order, but they must be the first n queued.
func expectGrantedFirst(t *testing.T, results <-chan queuedAttempt, n int) []*admissionLease {
	t.Helper()
	var leases []*admissionLease
	granted := map[int]bool{}
	for range n {
		select {
		case got := <-results:
			if got.err != nil || got.lease == nil {
				t.Fatalf("queued attempt %d: lease=%v err=%v, want a grant", got.order, got.lease, got.err)
			}
			granted[got.order] = true
			leases = append(leases, got.lease)
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d queued attempts were granted", len(leases), n)
		}
	}
	for order := range n {
		if !granted[order] {
			t.Fatalf("granted %v, want the first %d queued: the queue was reordered", granted, n)
		}
	}
	return leases
}

func expectNoGrant(t *testing.T, results <-chan queuedAttempt) {
	t.Helper()
	select {
	case got := <-results:
		t.Fatalf("attempt %d finished (lease=%v err=%v) while it should still wait", got.order, got.lease, got.err)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestAdmissionLimitsParse(t *testing.T) {
	valid := []struct {
		name string
		json string
		want admissionLimits
	}{
		{"empty object keeps every default", `{}`,
			admissionLimits{Limit: 6, Queue: 64, SpillQueue: 8, ClosedQueue: 16}},
		{"limit alone", `{"limit": 12}`,
			admissionLimits{Limit: 12, Queue: 64, SpillQueue: 8, ClosedQueue: 16}},
		{"closed queue follows the queue to a quarter", `{"queue": 40}`,
			admissionLimits{Limit: 6, Queue: 40, SpillQueue: 8, ClosedQueue: 10}},
		{"zero spill and closed queues turn them off", `{"spill_queue": 0, "closed_queue": 0}`,
			admissionLimits{Limit: 6, Queue: 64}},
		{"a model family alias names its pool", `{"per_model": {"opus": 10, "Sonnet": 8, "gpt-5.5": 4}}`,
			admissionLimits{Limit: 6, Queue: 64, SpillQueue: 8, ClosedQueue: 16,
				PerModel: map[string]int{"claude-opus": 10, "claude-sonnet": 8, "gpt-5.5": 4}}},
		{"an account id is trimmed", `{"per_account": {" claw-1 ": 30}}`,
			admissionLimits{Limit: 6, Queue: 64, SpillQueue: 8, ClosedQueue: 16,
				PerAccount: map[string]int{"claw-1": 30}}},
	}
	for _, tc := range valid {
		got, err := parseAdmissionLimits([]byte(tc.json))
		if err != nil {
			t.Errorf("%s: %s: %v", tc.name, tc.json, err)
			continue
		}
		tc.want.Tune = defaultAdmissionLimits().Tune
		if got.String() != tc.want.String() {
			t.Errorf("%s: %s parsed to %s, want %s", tc.name, tc.json, got, tc.want)
		}
	}
	for _, bad := range []string{
		``,
		`{`,
		`[]`,
		`{"limit": 0}`,
		`{"limit": -1}`,
		`{"limit": 6.5}`,
		`{"queue": -1}`,
		`{"spill_queue": -1}`,
		`{"closed_queue": -1}`,
		`{"limt": 6}`,
		`{"per_model": {"opus": 0}}`,
		`{"per_model": {" ": 3}}`,
		`{"per_model": {"opus": 3, "claude-opus": 4}}`,
		`{"per_account": {"a": 0}}`,
		`{"per_account": {"": 3}}`,
		`{"limit": 6} {"limit": 7}`,
		`{"limit": 6}}`,
		`{"limit": 6}]`,
	} {
		if got, err := parseAdmissionLimits([]byte(bad)); err == nil {
			t.Errorf("%q parsed to %s, want an error", bad, got)
		}
	}
}

func TestAdmissionLimitsPerAccountBeatsPerModelBeatsLimit(t *testing.T) {
	f := newLimitsFileFixture(t)
	f.apply(`{"limit": 3, "per_model": {"opus": 5}, "per_account": {"claw-1": 9}}`)
	for _, tc := range []struct {
		key  admissionKey
		want int
	}{
		{admissionKeyFor(accounts.ProviderClaude, "claw-1", "claude-opus"), 9},
		{admissionKeyFor(accounts.ProviderClaude, "claw-1", "claude-sonnet"), 9},
		{admissionKeyFor(accounts.ProviderClaude, "owner", "claude-opus"), 5},
		{admissionKeyFor(accounts.ProviderClaude, "owner", "claude-sonnet"), 3},
		{admissionKeyFor(accounts.ProviderCodex, "owner", "gpt-5.5"), 3},
	} {
		if got := admitted(t, f.control, tc.key); got != tc.want {
			t.Errorf("%+v admits %d at once, want %d", tc.key, got, tc.want)
		}
	}
}

func TestAdmissionLimitsFileMissingKeepsTodaysDefaults(t *testing.T) {
	f := newLimitsFileFixture(t)
	f.file.poll()
	limits := f.control.snapshot()
	if limits.Limit != 6 || limits.Queue != 64 || limits.SpillQueue != 8 || limits.ClosedQueue != 16 {
		t.Fatalf("limits with no file = %+v, want 6/64/8/16", limits)
	}
	if logs := f.logs.String(); logs != "" {
		t.Fatalf("a missing file logged %q; it is the normal case", logs)
	}
}

func TestAdmissionLimitsRaiseWakesQueuedRequestsInOrder(t *testing.T) {
	f := newLimitsFileFixture(t)
	f.apply(`{"limit": 2}`)
	key := admissionKeyFor(accounts.ProviderClaude, "owner", "claude-opus")
	for range 2 {
		lease, ok := f.control.tryAcquire(key, admissionSessionKey{})
		if !ok {
			t.Fatal("limit 2 refused one of the first two requests")
		}
		defer lease.release()
	}
	results := queueAttempts(t, f.control, key, 3)

	f.apply(`{"limit": 4}`)
	for _, lease := range expectGrantedFirst(t, results, 2) {
		defer lease.release()
	}
	expectNoGrant(t, results)
	if got := queuedOn(f.control, key); got != 1 {
		t.Fatalf("%d still queued after raising 2 to 4 with 3 waiting, want 1", got)
	}
	if logs := f.logs.String(); !strings.Contains(logs, "admission limits changed") ||
		!strings.Contains(logs, `old="{\"limit\":2,`) || !strings.Contains(logs, `new="{\"limit\":4,`) {
		t.Fatalf("the change was not logged with its old and new values: %s", logs)
	}
}

func TestAdmissionLimitsLowerStopsNewGrantsAndDropsNothing(t *testing.T) {
	f := newLimitsFileFixture(t)
	f.apply(`{"limit": 4}`)
	key := admissionKeyFor(accounts.ProviderClaude, "owner", "claude-opus")
	var active []*admissionLease
	for range 4 {
		lease, ok := f.control.tryAcquire(key, admissionSessionKey{})
		if !ok {
			t.Fatal("limit 4 refused one of the first four requests")
		}
		active = append(active, lease)
	}
	results := queueAttempts(t, f.control, key, 2)

	// A queue smaller than what already waits turns away only new arrivals.
	f.apply(`{"limit": 2, "queue": 0}`)
	expectNoGrant(t, results)
	if _, err := f.control.acquire(context.Background(), key); !errors.Is(err, errAdmissionQueueFull) {
		t.Fatalf("a new arrival over the lowered queue got %v, want a full queue", err)
	}
	if got := queuedOn(f.control, key); got != 2 {
		t.Fatalf("%d queued after lowering, want both waiters kept", got)
	}

	active[0].release()
	expectNoGrant(t, results) // 3 run, over the new limit of 2
	active[1].release()
	expectNoGrant(t, results) // 2 run, at the limit
	active[2].release()
	first := expectGranted(t, results, 0)
	active[3].release()
	second := expectGranted(t, results, 1)
	first.release()
	second.release()
}

func TestAdmissionLimitsInvalidFileKeepsLastGoodAndLogsOncePerContent(t *testing.T) {
	f := newLimitsFileFixture(t)
	f.apply(`{"limit": 9}`)
	if got := f.control.snapshot().Limit; got != 9 {
		t.Fatalf("limit = %d, want 9", got)
	}

	f.apply(`{"limit": 0}`)
	f.file.poll()
	f.file.poll()
	if got := f.control.snapshot().Limit; got != 9 {
		t.Fatalf("an invalid file moved the limit to %d, want the last good 9", got)
	}
	if got := strings.Count(f.logs.String(), "level=ERROR"); got != 1 {
		t.Fatalf("one invalid file logged %d errors over three polls, want 1:\n%s", got, f.logs)
	}
	if !strings.Contains(f.logs.String(), f.path) {
		t.Fatalf("the error does not name the file: %s", f.logs)
	}

	f.apply(`{"limt": 7}`)
	if got := strings.Count(f.logs.String(), "level=ERROR"); got != 2 {
		t.Fatalf("a second, different invalid file logged %d errors in all, want 2", got)
	}

	f.apply(`{"limit": 7}`)
	if got := f.control.snapshot().Limit; got != 7 {
		t.Fatalf("a valid file after an invalid one left the limit at %d, want 7", got)
	}

	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	f.file.poll()
	if got := f.control.snapshot().Limit; got != 6 {
		t.Fatalf("removing the file left the limit at %d, want the default 6", got)
	}
}

func TestAdmissionClosedOnceAQuarterOfTheQueueWaits(t *testing.T) {
	f := newLimitsFileFixture(t)
	f.apply(`{"limit": 1, "queue": 8}`)
	key := admissionKeyFor(accounts.ProviderClaude, "owner", "claude-opus")
	lease, ok := f.control.tryAcquire(key, admissionSessionKey{})
	if !ok {
		t.Fatal("first request refused")
	}
	defer lease.release()
	results := queueAttempts(t, f.control, key, 1)
	if f.control.closed(key) {
		t.Fatal("closed with 1 of 8 waiting, want open below a quarter")
	}
	more := queueAttempts(t, f.control, key, 1)
	if !f.control.closed(key) {
		t.Fatal("open with 2 of 8 waiting, want closed at a quarter")
	}
	bucket := f.control.snapshot().Accounts[0]
	if !bucket.Closed {
		t.Fatalf("traffic does not show the closed bucket: %+v", bucket)
	}
	f.apply(`{"limit": 1, "queue": 8, "closed_queue": 0}`)
	if f.control.closed(key) {
		t.Fatal("closed_queue 0 still closed the bucket")
	}
	if (*admissionControl)(nil).closed(key) || f.control.closed(admissionKey{}) {
		t.Fatal("a nil gate or an empty account reads as closed")
	}
	lease.release()
	expectGranted(t, results, 0).release()
	expectGranted(t, more, 0).release()
}

func TestTrafficShowsTheLimitsInForce(t *testing.T) {
	f := newLimitsFileFixture(t)
	f.apply(`{"limit": 7, "queue": 20, "spill_queue": 3, "per_model": {"opus": 11}, "per_account": {"claw-1": 30}}`)
	if lease, ok := f.control.tryAcquire(admissionKeyFor(accounts.ProviderClaude, "owner", "claude-opus"), admissionSessionKey{}); ok {
		defer lease.release()
	}
	raw, err := json.Marshal(f.control.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var traffic struct {
		Limit       int            `json:"limit"`
		Queue       int            `json:"queue"`
		SpillQueue  int            `json:"spill_queue"`
		ClosedQueue int            `json:"closed_queue"`
		PerModel    map[string]int `json:"per_model"`
		PerAccount  map[string]int `json:"per_account"`
		Accounts    []struct {
			Model string `json:"model"`
			Limit int    `json:"limit"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &traffic); err != nil {
		t.Fatal(err)
	}
	if traffic.Limit != 7 || traffic.Queue != 20 || traffic.SpillQueue != 3 || traffic.ClosedQueue != 5 ||
		traffic.PerModel["claude-opus"] != 11 {
		t.Fatalf("traffic limits = %s", raw)
	}
	// The endpoint is unauthenticated: accounts appear only as hashes.
	if strings.Contains(string(raw), "claw-1") || traffic.PerAccount[admissionAccountHash("claw-1")] != 30 {
		t.Fatalf("per_account must be keyed by account hash: %s", raw)
	}
	if len(traffic.Accounts) != 1 || traffic.Accounts[0].Limit != 11 {
		t.Fatalf("the opus bucket reports %s, want its limit 11", raw)
	}
}

// TestAdmissionLimitsFileChangesUnderLiveLoadDropNothing rewrites the file
// over and over while requests stream through one bucket: every request must
// still be served, whatever the limit was when it queued.
func TestAdmissionLimitsFileChangesUnderLiveLoadDropNothing(t *testing.T) {
	f := newLimitsFileFixture(t)
	f.apply(`{"limit": 3}`)
	key := admissionKeyFor(accounts.ProviderClaude, "owner", "claude-opus")
	const workers, rounds = 40, 25
	var running, peak, served, failed atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for round := range rounds {
				attemptCtx, attemptCancel := context.WithTimeout(context.Background(), 20*time.Second)
				lease, err := f.control.acquire(attemptCtx, key)
				attemptCancel()
				if err != nil {
					failed.Add(1)
					t.Errorf("round %d: %v", round, err)
					return
				}
				now := running.Add(1)
				for {
					old := peak.Load()
					if now <= old || peak.CompareAndSwap(old, now) {
						break
					}
				}
				time.Sleep(time.Duration(round%3) * 200 * time.Microsecond)
				running.Add(-1)
				served.Add(1)
				lease.release()
			}
		})
	}
	writes := []string{`{"limit": 1}`, `{"limit": 8}`, `{"limit": 2}`, `{"limit": 12}`, `{"limit": 0}`,
		`{"limit": 3, "spill_queue": 0}`, ``, `{"limit": 5}`}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	inForce := map[int]bool{}
	for i := 0; ; i++ {
		if accounts := f.control.snapshot().Accounts; len(accounts) == 1 {
			inForce[accounts[0].Limit] = true
		}
		select {
		case <-done:
			if len(inForce) < 3 {
				t.Fatalf("limits in force during the load = %v, want the file's changes to land", inForce)
			}
			if failed.Load() != 0 || served.Load() != workers*rounds {
				t.Fatalf("served %d of %d requests, %d failed", served.Load(), workers*rounds, failed.Load())
			}
			if peak.Load() > 12 {
				t.Fatalf("%d ran at once, above the highest limit the file ever set (12)", peak.Load())
			}
			snapshot := f.control.snapshot().Accounts[0]
			if snapshot.Active != 0 || snapshot.Queued != 0 {
				t.Fatalf("after the load the bucket holds %+v, want it empty", snapshot)
			}
			return
		case <-time.After(3 * time.Millisecond):
		}
		if content := writes[i%len(writes)]; content == "" {
			_ = os.Remove(f.path)
		} else {
			f.write(content)
		}
		f.file.poll()
	}
}

// TestAdmissionLimitsFileReachesTheServingGate drives real turns through the
// handler: raising the limit in the file lets the turns already queued behind
// it reach upstream, and traffic shows the new limit, with no restart.
func TestAdmissionLimitsFileReachesTheServingGate(t *testing.T) {
	now := time.Now()
	const id = "a@example.com"
	release := make(chan struct{})
	arrived := make(chan struct{}, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","content":[]}`))
	}))
	t.Cleanup(upstream.Close)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	ref := &AccountRef{}
	path := filepath.Join(t.TempDir(), "admission.json")
	if err := os.WriteFile(path, []byte(`{"limit": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ref.StartAdmission(ctx, path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := Server{
		Sessions:       store,
		ClaudeUpstream: mustParseURL(t, upstream.URL),
		Accounts: []accounts.Account{{
			ID: id, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-" + id,
		}},
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			scoreFromUsageWindows(accounts.ProviderClaude, id, []accounts.UsageWindow{
				fiveHour(10, now.Add(2*time.Hour)), sevenDay(10, now.Add(72*time.Hour)),
			}),
		})),
		// The gate StartAdmission put the file on: the one every Server copy
		// over ref shares.
		admission:    ref.sharedAdmission(),
		MaxBodyBytes: 1 << 20,
	}.Handler()
	traffic := func() admissionSnapshot {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://subrouter.local/_subrouter/traffic", nil))
		var body struct {
			Admission admissionSnapshot `json:"admission"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("traffic body: %v: %s", err, recorder.Body.String())
		}
		return body.Admission
	}

	var wg sync.WaitGroup
	statuses := make(chan int, 3)
	for i := range 3 {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodPost, "http://subrouter.local/v1/messages",
				strings.NewReader(`{"model":"claude-opus-4-8","messages":[]}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Subrouter-Agent", "claude")
			req.Header.Set("X-Subrouter-Session", fmt.Sprintf("session%d-7c1f-4b0e-9d55-2f1e5a7b9c3d", i))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			statuses <- recorder.Code
		})
	}
	waitFor(t, "one turn running and two queued at limit 1", func() bool {
		accounts := traffic().Accounts
		return len(accounts) == 1 && accounts[0].Active == 1 && accounts[0].Queued == 2
	})
	if got := len(arrived); got != 1 {
		t.Fatalf("%d turns reached upstream at limit 1", got)
	}

	if err := os.WriteFile(path, []byte(`{"limit": 3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the queued turns to start under the new limit", func() bool {
		accounts := traffic().Accounts
		return len(accounts) == 1 && accounts[0].Active == 3 && accounts[0].Queued == 0
	})
	if snapshot := traffic(); snapshot.Limit != 3 || snapshot.Accounts[0].Limit != 3 {
		t.Fatalf("traffic shows limit %d, bucket %d, want 3 and 3", snapshot.Limit, snapshot.Accounts[0].Limit)
	}
	close(release)
	wg.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("a turn answered %d, want every queued turn served", status)
		}
	}
}

// A raise that lands while a bucket ramps back from a transient 429, after
// the ramp reached the old limit: its waiters armed no timer, so before the
// fix they stayed put until a running request finished.
func TestAdmissionLimitsRaiseDuringARecoveryRampKeepsRamping(t *testing.T) {
	f := newLimitsFileFixture(t)
	f.apply(`{"limit": 2}`)
	key := admissionKeyFor(accounts.ProviderClaude, "owner", "claude-opus")
	pauseEnds := time.Now().Add(100 * time.Millisecond)
	f.control.pauseWithRecovery(key, pauseEnds, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var granted atomic.Int32
	for range 6 {
		go func() {
			if lease, err := f.control.acquire(ctx, key); err == nil && lease != nil {
				granted.Add(1)
			}
		}()
	}
	waitFor(t, "six requests to queue behind the pause", func() bool { return queuedOn(f.control, key) == 6 })
	// The ramp reaches the old limit of 2 one second after the pause.
	time.Sleep(time.Until(pauseEnds.Add(1200 * time.Millisecond)))
	f.apply(`{"limit": 8}`)
	// 4 at +2 s and 8 at +3 s: all six run by about +3 s.
	waitFor(t, "the ramp to carry on to the new limit", func() bool { return granted.Load() == 6 })
}
