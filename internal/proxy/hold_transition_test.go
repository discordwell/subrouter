package proxy

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAHoldEndedByShutdownAnswersARetryable502NotTheSpentPool503(t *testing.T) {
	today := newHoldRig(t, holdRigOptions{upstream: noLocalUpstream(t), exhausted: true, peer: peerAnswer})
	cutPeer(t, today.pool, func(int64) error { return realDialTimeout(t) })
	todayResp, _, err := today.turn(t, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if seconds, _ := strconv.Atoi(todayResp.Header.Get("Retry-After")); todayResp.StatusCode != http.StatusServiceUnavailable || seconds <= 60 {
		t.Fatalf("the unheld answer is %d with Retry-After %q; this test needs the spent-pool 503 with a Retry-After over 60",
			todayResp.StatusCode, todayResp.Header.Get("Retry-After"))
	}

	lifecycle := NewLifecycle()
	held := newHoldRig(t, holdRigOptions{hold: holdTestPolicy(time.Minute), upstream: noLocalUpstream(t), exhausted: true, peer: peerAnswer, lifecycle: lifecycle})
	cutPeer(t, held.pool, func(n int64) error {
		if n == 2 {
			lifecycle.BeginShutdown()
		}
		return realDialTimeout(t)
	})
	resp, body, err := held.turn(t, context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	held.settle(t, 1)
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Retry-After") != "" {
		t.Fatalf("a hold ended by shutdown answered %d with Retry-After %q (body %q); want a 502 with none, which Claude Code retries",
			resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	if logs := held.logs.String(); held.countLog("proxy hold ended") != 1 || !strings.Contains(logs, "outcome=shutdown_502") || !strings.Contains(logs, "status=502") {
		t.Fatalf("the hold end should name the shutdown's 502; logs:\n%s", held.logs.String())
	}
}

const heldUpstream503Body = `{"type":"error","error":{"type":"api_error","message":"unavailable"}}`

func newHeld503Rig(t *testing.T, atSecondAttempt func(*Lifecycle)) *holdRig {
	lifecycle := NewLifecycle()
	hits := 0
	return newHoldRig(t, holdRigOptions{
		hold: holdTestPolicy(time.Minute),
		upstream: func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			if hits++; hits == 2 {
				atSecondAttempt(lifecycle)
			}
			w.Header().Set("Retry-After", "120")
			w.Header().Set("Request-Id", "req_upstream_123")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(heldUpstream503Body))
		},
		lifecycle: lifecycle,
	})
}

func TestAHoldEndedByShutdownAnswersARetryable502NotTheUpstream503(t *testing.T) {
	rig := newHeld503Rig(t, (*Lifecycle).BeginShutdown)
	resp, body, err := rig.post(t, context.Background(), "/v1/messages/count_tokens", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Retry-After") != "" {
		t.Fatalf("a hold ended by shutdown answered %d with Retry-After %q (body %q); want a 502 with none",
			resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	if rig.countLog("proxy hold ended") != 1 || !strings.Contains(rig.logs.String(), "outcome=shutdown_502") {
		t.Fatalf("the hold end should name the shutdown's 502; logs:\n%s", rig.logs.String())
	}
}

func TestAHoldEndedByAManualDrainAnswersTheFailureItKept(t *testing.T) {
	rig := newHeld503Rig(t, (*Lifecycle).Drain)
	resp, body, err := rig.post(t, context.Background(), "/v1/messages/count_tokens", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "120" ||
		resp.Header.Get("Request-Id") != "req_upstream_123" || strings.TrimSpace(body) != heldUpstream503Body {
		t.Fatalf("a hold ended by a manual drain answered %d, Retry-After %q, Request-Id %q, body %q; want the upstream's 503 as it was",
			resp.StatusCode, resp.Header.Get("Retry-After"), resp.Header.Get("Request-Id"), body)
	}
	if logs := rig.logs.String(); rig.countLog("proxy hold ended") != 1 || !strings.Contains(logs, "outcome=draining") || !strings.Contains(logs, "status=503") {
		t.Fatalf("the hold end should say draining with the 503 sent; logs:\n%s", logs)
	}
}

func TestARequestThatNeverHeldAnswersItsOwnFailureDuringADrainOrShutdown(t *testing.T) {
	for _, state := range []struct {
		name  string
		enter func(*Lifecycle)
	}{{"drain", (*Lifecycle).Drain}, {"shutdown", (*Lifecycle).BeginShutdown}} {
		for _, status := range []int{500, 503, 504, 429, 529, 401} {
			lifecycle := NewLifecycle()
			warm := false
			rig := newHoldRig(t, holdRigOptions{
				hold: holdTestPolicy(time.Minute),
				upstream: func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					if !warm {
						warm = true
						answerOK(w, r)
						return
					}
					w.Header().Set("Retry-After", "120")
					w.Header().Set("Request-Id", "req_upstream_123")
					w.WriteHeader(status)
					_, _ = w.Write([]byte(heldUpstream503Body))
				},
				lifecycle: lifecycle,
			})
			if resp, _, err := rig.turn(t, context.Background(), nil); err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("warming the session: %v, %v", resp, err)
			}
			state.enter(lifecycle)
			resp, body, err := rig.post(t, context.Background(), "/v1/messages/count_tokens", nil)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != status || resp.Header.Get("Retry-After") != "120" ||
				resp.Header.Get("Request-Id") != "req_upstream_123" || strings.TrimSpace(body) != heldUpstream503Body {
				t.Errorf("during a %s, an unheld %d answered %d, Retry-After %q, Request-Id %q, body %q; want the upstream's answer as it was",
					state.name, status, resp.StatusCode, resp.Header.Get("Retry-After"), resp.Header.Get("Request-Id"), body)
			}
			if got := rig.countLog("proxy hold ended"); got != 0 {
				t.Errorf("during a %s, an unheld %d logged %d hold ends; logs:\n%s", state.name, status, got, rig.logs.String())
			}
		}
	}
}

func TestAStrandedHoldAnswers502WhenItsDeadlineOrAShutdownEndsIt(t *testing.T) {
	for _, end := range []struct {
		name    string
		end     func(*Lifecycle, *movableClock)
		outcome string
	}{
		{"deadline", func(_ *Lifecycle, clock *movableClock) { clock.set(warmWaitStart.Add(time.Minute + time.Second)) }, "outcome=deadline status=502"},
		{"shutdown", func(lifecycle *Lifecycle, _ *movableClock) { lifecycle.BeginShutdown() }, "outcome=shutdown_502 status=502"},
	} {
		t.Run(end.name, func(t *testing.T) {
			clock := newMovableClock(warmWaitStart)
			policy := strandedPolicy(clock)
			policy.Deadline = time.Minute
			lifecycle := NewLifecycle()
			boxA := newContractBox(t, eligibleBoxAccount("acct-a"))
			cutPeer(t, boxA.pool, func(int64) error { return refusedFailure() })
			rig := newHoldRig(t, holdRigOptions{hold: policy, upstream: noLocalUpstream(t), exhausted: true,
				peers: []*RemotePool{boxA.pool}, lifecycle: lifecycle})
			holdOn(t, boxA.pool, "hold-session", "acct-a")
			answer := rig.sendAsync(t, holdTurnBody)
			awaitHeld(t, rig, policy, answer, "the stranded wait")
			end.end(lifecycle, clock)
			got := <-answer
			rig.settle(t, 1)
			retryable(t, got, http.StatusBadGateway)
			if logs := rig.logs.String(); !strings.Contains(logs, end.outcome) {
				t.Fatalf("want the stranded hold ended with %q; logs:\n%s", end.outcome, logs)
			}
		})
	}
}
