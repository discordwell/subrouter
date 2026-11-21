package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// PATCHES §67, 5a: a turn the admission gate turns away because its account's
// queue is full is held (PATCHES §58) and runs again until a slot frees,
// instead of reaching the client as a 503.

// newDrainedHoldRig is newHoldRig for a test that runs more handlers than
// the rig's per-run channel holds: it empties that channel until the daemon
// has closed. The stop is registered first, so it runs after the rig's own
// cleanups, by which time no handler is left to send.
func newDrainedHoldRig(t *testing.T, opts holdRigOptions) *holdRig {
	t.Helper()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	rig := newHoldRig(t, opts)
	go func() {
		for {
			select {
			case <-rig.handled:
			case <-stop:
				return
			}
		}
	}()
	return rig
}

// The only account has six turns in flight and 64 queued, the gate's
// defaults. The 71st turn gets no answer while the queue is full, and is
// served once the six finish and the queue drains. Every turn is a 200.
func TestTheTurnAFullAdmissionQueueTurnsAwayIsHeldUntilASlotFrees(t *testing.T) {
	gate := newAdmissionControl()
	release := make(chan struct{})
	var served atomic.Int64
	policy := holdTestPolicy(time.Minute)
	policy.MaxRequests = DefaultHoldMaxRequests
	rig := newDrainedHoldRig(t, holdRigOptions{hold: policy, admission: gate, upstream: func(w http.ResponseWriter, r *http.Request) {
		<-release
		served.Add(1)
		answerOK(w, r)
	}})
	var freeOnce sync.Once
	free := func() { freeOnce.Do(func() { close(release) }) }
	t.Cleanup(free)
	statuses := make(chan int, defaultAdmissionLimit+defaultAdmissionQueue+1)
	send := func() {
		resp, _, err := rig.turn(t, context.Background(), nil)
		if err != nil {
			statuses <- 0
			return
		}
		statuses <- resp.StatusCode
	}
	for range defaultAdmissionLimit + defaultAdmissionQueue {
		go send()
	}
	waitFor(t, "six turns in flight and 64 queued", func() bool {
		accounts := gate.snapshot().Accounts
		return len(accounts) == 1 && accounts[0].Active == defaultAdmissionLimit && accounts[0].Queued == defaultAdmissionQueue
	})

	go send()
	deadline := time.Now().Add(10 * time.Second)
	for policy.Active() != 1 {
		select {
		case status := <-statuses:
			t.Fatalf("a turn was answered %d while the queue was full; want the 71st held; logs:\n%s", status, rig.logs.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the 71st turn was never held; logs:\n%s", rig.logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := served.Load(); n != 0 {
		t.Fatalf("calibration: %d turns served before the slots freed", n)
	}

	free()
	for range defaultAdmissionLimit + defaultAdmissionQueue + 1 {
		if status := <-statuses; status != http.StatusOK {
			t.Fatalf("a turn was answered %d, want every turn served; logs:\n%s", status, rig.logs.String())
		}
	}
	if n := served.Load(); n != defaultAdmissionLimit+defaultAdmissionQueue+1 {
		t.Fatalf("the upstream served %d turns, want %d", n, defaultAdmissionLimit+defaultAdmissionQueue+1)
	}
	logs := rig.logs.String()
	if rig.countLog("proxy hold started") != 1 || !strings.Contains(logs, "reason="+admissionReasonQueueFull) {
		t.Fatalf("want one hold start naming the full queue; logs:\n%s", logs)
	}
	if rig.countLog("proxy hold ended") != 1 || !strings.Contains(logs, "outcome=answered") {
		t.Fatalf("want one hold end, answered; logs:\n%s", logs)
	}
}

// PATCHES §67.6 leaves a paused account as it was: the turn the gate
// turns away because a rate limit paused the only account fails fast with the
// paused 503, unheld. The pause outlasts the hold's deadline, so holding it
// would only delay that 503.
func TestATurnAPausedAccountTurnsAwayIsNotHeld(t *testing.T) {
	var hits atomic.Int64
	rig := newHoldRig(t, holdRigOptions{hold: holdTestPolicy(3 * time.Second), admission: newAdmissionControl(), upstream: func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
			return
		}
		answerOK(w, r)
	}})
	if _, _, err := rig.turn(t, context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	rig.settle(t, 1)
	resp, body, err := rig.turn(t, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rig.settle(t, 1)
	logs := rig.logs.String()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get(subrouterReasonHeader) != admissionReasonPaused || hits.Load() != 1 {
		t.Fatalf("calibration: status %d reason %q body %q, upstream hits %d; want the paused 503 and the 429 that paused it; logs:\n%s",
			resp.StatusCode, resp.Header.Get(subrouterReasonHeader), body, hits.Load(), logs)
	}
	if n := rig.countLog("proxy hold started"); n != 0 {
		t.Fatalf("%d holds started; a turn a paused account turned away must fail fast; logs:\n%s", n, logs)
	}
}

// A targeted turn (X-Subrouter-Target-Account, the account a peer's ranking
// chose) that a full queue turns away gets the targeted 429 at once, unheld,
// so its sender recomputes (PATCHES §67.6). This one arrives with no hop,
// the only way a targeted turn reaches the hold at all.
func TestATargetedTurnAFullAdmissionQueueTurnsAwayIsNotHeld(t *testing.T) {
	gate := newAdmissionControl()
	release := make(chan struct{})
	policy := holdTestPolicy(3 * time.Second)
	policy.MaxRequests = DefaultHoldMaxRequests
	rig := newDrainedHoldRig(t, holdRigOptions{hold: policy, admission: gate, upstream: func(w http.ResponseWriter, r *http.Request) {
		<-release
		answerOK(w, r)
	}})
	var freeOnce sync.Once
	free := func() { freeOnce.Do(func() { close(release) }) }
	t.Cleanup(free)
	done := make(chan struct{}, defaultAdmissionLimit+defaultAdmissionQueue)
	for range defaultAdmissionLimit + defaultAdmissionQueue {
		go func() {
			_, _, _ = rig.turn(t, context.Background(), nil)
			done <- struct{}{}
		}()
	}
	waitFor(t, "six turns in flight and 64 queued", func() bool {
		accounts := gate.snapshot().Accounts
		return len(accounts) == 1 && accounts[0].Active == defaultAdmissionLimit && accounts[0].Queued == defaultAdmissionQueue
	})
	resp, body, err := rig.turn(t, context.Background(), http.Header{remotePoolTargetAccountHeader: {"one@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	logs := rig.logs.String()
	free()
	for range defaultAdmissionLimit + defaultAdmissionQueue {
		<-done
	}
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get(subrouterReasonHeader) != admissionReasonQueueFull {
		t.Fatalf("status %d reason %q body %q; want the targeted queue-full 429; logs:\n%s", resp.StatusCode, resp.Header.Get(subrouterReasonHeader), body, logs)
	}
	if n := strings.Count(logs, "proxy hold started"); n != 0 {
		t.Fatalf("%d holds started; a targeted turn a full queue turns away must get its 429 at once; logs:\n%s", n, logs)
	}
}
