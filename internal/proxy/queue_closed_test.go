package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

var closedQueue = newAdmissionControl().limits.ClosedQueue

type filledQueue struct {
	gate    *admissionControl
	key     admissionKey
	leases  []*admissionLease
	cancels []context.CancelFunc
	waiting sync.WaitGroup
}

func fillQueue(t *testing.T, gate *admissionControl, key admissionKey, waiters int) *filledQueue {
	t.Helper()
	q := &filledQueue{gate: gate, key: key}
	for range gate.limits.Limit {
		lease, err := gate.acquire(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		q.leases = append(q.leases, lease)
	}
	t.Cleanup(q.drain)
	q.add(t, waiters)
	return q
}

func (q *filledQueue) add(t *testing.T, n int) {
	t.Helper()
	want := q.queued() + n
	for range n {
		ctx, cancel := context.WithCancel(context.Background())
		q.cancels = append(q.cancels, cancel)
		q.waiting.Add(1)
		go func() {
			defer q.waiting.Done()
			if lease, err := q.gate.acquire(ctx, q.key); err == nil {
				lease.release()
			}
		}()
	}
	q.await(t, want)
}

func (q *filledQueue) withdraw(t *testing.T) {
	t.Helper()
	want := q.queued() - 1
	last := len(q.cancels) - 1
	q.cancels[last]()
	q.cancels = q.cancels[:last]
	q.await(t, want)
}

func (q *filledQueue) queued() int {
	q.gate.mu.Lock()
	defer q.gate.mu.Unlock()
	if bucket := q.gate.buckets[q.key]; bucket != nil {
		return len(bucket.waiters)
	}
	return 0
}

func (q *filledQueue) await(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for q.queued() != want {
		if time.Now().After(deadline) {
			t.Fatalf("queue holds %d waiters, want %d", q.queued(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func (q *filledQueue) drain() {
	for _, lease := range q.leases {
		lease.release()
	}
	q.leases = nil
	q.waiting.Wait()
	for _, cancel := range q.cancels {
		cancel()
	}
}

func opusKey(account string) admissionKey {
	return admissionKeyFor(schedulerAccountProvider(accounts.ProviderClaude), account, claudePoolModel("claude-opus-4-8"))
}

func gatedTargetBox(t *testing.T, ids ...string) (*targetBox, *admissionControl) {
	t.Helper()
	box := newTargetBox(t)
	gate := newAdmissionControl()
	box.server.admission = gate
	if len(ids) > 0 {
		var kept []accounts.Account
		for _, account := range box.server.Accounts {
			for _, id := range ids {
				if account.ID == id {
					kept = append(kept, account)
				}
			}
		}
		box.server.Accounts = kept
	}
	return box, gate
}

func TestAClosedQueueRefusesANewTargetedSession(t *testing.T) {
	box, gate := gatedTargetBox(t)
	if v := box.roomyVerdict(); !v.Eligible {
		t.Fatalf("calibration: open roomy %+v, want eligible", v)
	}
	fillQueue(t, gate, opusKey("roomy"), closedQueue)
	reply := box.answerWithin(t, box.turnAsync("new-session", "roomy"))
	if reply.status != remotePoolTargetRefusedStatus || reply.refused != selectacct.VerdictQueueClosed {
		t.Fatalf("answered %d refused %q, want %d %q; body %s", reply.status, reply.refused, remotePoolTargetRefusedStatus, selectacct.VerdictQueueClosed, reply.body)
	}
	var refusal remotePoolTargetRefusal
	if err := json.Unmarshal([]byte(reply.body), &refusal); err != nil {
		t.Fatalf("refusal body does not decode: %v: %s", err, reply.body)
	}
	if v := refusal.Target.Verdict; v == nil || v.Eligible || v.Evicted || v.RefugeeCap.Or(-1) != 0 || refusal.Target.Continuing {
		t.Fatalf("refusal %+v verdict %+v, want a new placement refused, not evicted, refugee cap 0", refusal.Target, refusal.Target.Verdict)
	}
	if got := box.upstream.accounts(); len(got) != 0 {
		t.Fatalf("upstream contacted %v; a refusal must precede upstream", got)
	}
	if got := box.pinnedTo(t, "new-session"); got != "" {
		t.Fatalf("a refused turn wrote a pin to %q", got)
	}
}

func TestUsageStatusPublishesAClosedQueue(t *testing.T) {
	now := time.Now()
	fable := `"seven_day_oauth_apps":{"utilization":20,"resets_at":"` + now.Add(100*time.Hour).UTC().Format(time.RFC3339) + `"}`
	server, _, _ := claudeUsageBox(t, map[string]string{
		"acct": claudeUsageJSON(now, 10, 4*time.Hour, 20, 100*time.Hour, fable),
	}, nil)
	gate := newAdmissionControl()
	server.admission = gate
	if v := readUsageStatus(t, server)["acct"].Normalized.NewSession; v == nil || !v.Eligible {
		t.Fatalf("calibration: open acct publishes %+v, want eligible", v)
	}
	fillQueue(t, gate, opusKey("acct"), closedQueue)
	row := readUsageStatus(t, server)["acct"]
	for name, verdict := range map[string]*selectacct.NewSessionVerdict{
		"account": row.Normalized.NewSession, "claudefable": row.Normalized.Pools["claudefable"].NewSession,
	} {
		if verdict == nil || verdict.Eligible || verdict.Reason != selectacct.VerdictQueueClosed || verdict.Evicted || verdict.RefugeeCap.Or(-1) != 0 {
			t.Fatalf("%s publishes %+v, want not eligible, %q, not evicted, refugee cap 0", name, verdict, selectacct.VerdictQueueClosed)
		}
	}
}

func TestANewSessionLandsBesideAClosedQueue(t *testing.T) {
	box, gate := gatedTargetBox(t, "roomy", "steady")
	if picked, err := box.server.selectionScheduler("", "").Pick(box.server.Accounts); err != nil || picked.ID != "roomy" {
		t.Fatalf("calibration: open, a new session goes to %q (%v), want roomy", picked.ID, err)
	}
	fillQueue(t, gate, opusKey("roomy"), closedQueue)
	if got := box.answerWithin(t, box.turnAsync("new-session", "")); got.status != http.StatusOK || got.account != "steady" {
		t.Fatalf("new session answered %+v, want 200 on steady; logs:\n%s", got, box.logs.String())
	}
	if got := box.pinnedTo(t, "new-session"); got != "steady" {
		t.Fatalf("new session pinned to %q, want steady", got)
	}
}

type queuedAnswer struct {
	status                   int
	account, reason, refused string
	body                     string
	err                      error
}

func (b *targetBox) answerWithin(t *testing.T, answer <-chan queuedAnswer) queuedAnswer {
	t.Helper()
	select {
	case got := <-answer:
		return got
	case <-time.After(5 * time.Second):
		t.Fatalf("no answer in 5s: the turn waits in a queue; logs:\n%s", b.logs.String())
		return queuedAnswer{}
	}
}

func (b *targetBox) turnAsync(sessionID, target string) <-chan queuedAnswer {
	out := make(chan queuedAnswer, 1)
	server := httptest.NewServer(b.server.Handler())
	go func() {
		defer server.Close()
		req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8","messages":[]}`))
		if err != nil {
			out <- queuedAnswer{err: err}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Subrouter-Agent", "claude")
		req.Header.Set("X-Subrouter-Session", sessionID)
		if target != "" {
			req.Header.Set(remotePoolTargetAccountHeader, target)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			out <- queuedAnswer{err: err}
			return
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		out <- queuedAnswer{status: response.StatusCode, account: response.Header.Get(subrouterAccountHeader),
			reason: response.Header.Get(subrouterReasonHeader), refused: response.Header.Get(remotePoolTargetRefusedHeader), body: strings.TrimSpace(string(body))}
	}()
	return out
}

func TestAWarmSessionOnAClosedQueueQueuesThere(t *testing.T) {
	box, gate := gatedTargetBox(t, "roomy")
	box.pin(t, "warm-1", "roomy")
	box.pin(t, "warm-2", "roomy")
	queue := fillQueue(t, gate, opusKey("roomy"), closedQueue)
	if v := box.roomyVerdict(); v.Reason != selectacct.VerdictQueueClosed {
		t.Fatalf("calibration: roomy %+v, want closed", v)
	}
	untargeted := box.turnAsync("warm-1", "")
	targeted := box.turnAsync("warm-2", "roomy")
	queue.await(t, closedQueue+2)
	queue.drain()
	for id, answer := range map[string]<-chan queuedAnswer{"warm-1": untargeted, "warm-2": targeted} {
		select {
		case got := <-answer:
			if got.err != nil || got.status != http.StatusOK || got.account != "roomy" {
				t.Fatalf("%s answered %+v, want 200 on roomy; logs:\n%s", id, got, box.logs.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s was not served after the queue drained", id)
		}
		if got := box.pinnedTo(t, id); got != "roomy" {
			t.Fatalf("%s pinned to %q, want roomy", id, got)
		}
	}
	if logs := box.logs.String(); strings.Contains(logs, "session moved") || strings.Contains(logs, "targeted placement refused") {
		t.Fatalf("a warm session was moved or refused on its closed account; logs:\n%s", logs)
	}
}

func TestANewSessionWithOnlyAClosedQueueIsRefusedLikeAHoldout(t *testing.T) {
	answer := func(box *targetBox) (int, string, string) {
		reply := box.answerWithin(t, box.turnAsync("new-session", ""))
		if reply.err != nil {
			t.Fatal(reply.err)
		}
		if got := box.upstream.accounts(); len(got) != 0 {
			t.Fatalf("upstream contacted %v for a refused new session", got)
		}
		if got := box.pinnedTo(t, "new-session"); got != "" {
			t.Fatalf("a refused new session was pinned to %q", got)
		}
		return reply.status, reply.reason, reply.body
	}
	held, _ := gatedTargetBox(t, "held")
	wantStatus, wantReason, wantBody := answer(held)
	if wantStatus == http.StatusOK {
		t.Fatal("calibration: a box whose only account is held out served a new session")
	}
	closed, gate := gatedTargetBox(t, "roomy")
	fillQueue(t, gate, opusKey("roomy"), closedQueue)
	status, reason, body := answer(closed)
	if status != wantStatus || reason != wantReason || body != wantBody {
		t.Fatalf("closed answered %d %q %q, a holdout answers %d %q %q", status, reason, body, wantStatus, wantReason, wantBody)
	}
	t.Logf("a new session with every account closed: %d, reason %q, body %q", status, reason, body)
}

func TestAQueueClosesAtSixteenAndReopensBelow(t *testing.T) {
	if closedQueue != 16 {
		t.Fatalf("closedQueue is %d, want the owner's 16", closedQueue)
	}
	box, gate := gatedTargetBox(t)
	roomyKey := selectacct.ScoreKey(accounts.ProviderClaude, "roomy")
	queue := fillQueue(t, gate, opusKey("roomy"), closedQueue-1)
	if v := box.roomyVerdict(); !v.Eligible || gate.closedAccounts()[roomyKey] {
		t.Fatalf("15 waiters: roomy %+v, closed %v; want open", v, gate.closedAccounts()[roomyKey])
	}
	queue.add(t, 1)
	if v := box.roomyVerdict(); v.Reason != selectacct.VerdictQueueClosed || !gate.closedAccounts()[roomyKey] {
		t.Fatalf("16 waiters: roomy %+v, want closed", v)
	}
	if v := box.server.selectionScheduler("", "").NewSessionVerdict(accounts.ProviderClaude, "steady", time.Now()); !v.Eligible {
		t.Fatalf("roomy's queue closed steady too: %+v", v)
	}
	queue.withdraw(t)
	if v := box.roomyVerdict(); !v.Eligible || gate.closedAccounts()[roomyKey] {
		t.Fatalf("drained to 15: roomy %+v, want open again", v)
	}
}

func TestALocalRefugeeIsNotPlacedOnAClosedQueue(t *testing.T) {
	for _, closed := range []bool{false, true} {
		name := "open"
		if closed {
			name = "closed"
		}
		t.Run(name, func(t *testing.T) {
			policy := strandedPolicy(nil)
			gate := newAdmissionControl()
			rig := newWarmPeerRigGated(t, policy, false, func(int64) error { return refusedFailure() }, gate)
			refugeeHoldsOn(rig.boxB.pool, "acct-b", refugeeSlots, time.Minute)
			if closed {
				fillQueue(t, gate, admissionKeyFor(schedulerAccountProvider(accounts.ProviderClaude), "one@example.com", claudePoolModel("claude-opus-5-5")), closedQueue)
			}
			findDown(t, rig.boxA.pool)
			answer := rig.sendAsync(t, holdTurnBody)
			if !closed {
				got := notHeld(t, rig.holdRig, policy, answer, "a refugee")
				rig.settle(t, 1)
				if assignment, ok := rig.store.Get("claude", "hold-session"); got.status != http.StatusOK || !ok || assignment.AccountID != "one@example.com" {
					t.Fatalf("calibration: status %d, assignment %+v (%v), want a refugee on one@example.com; logs:\n%s", got.status, assignment, ok, rig.logs.String())
				}
				return
			}
			awaitHeld(t, rig.holdRig, policy, answer, "a refugee with the local queue closed")
			tries := attempts{count: func() int64 { return int64(rig.countLog("refugee not placed")) }, kick: func() {}}
			heldFor(t, rig.holdRig, answer, tries, 1, "with one@example.com's queue closed")
			if _, ok := rig.store.Get("claude", "hold-session"); ok {
				t.Fatalf("hold-session was pinned on a closed account; logs:\n%s", rig.logs.String())
			}
		})
	}
}

func sonnetKey(account string) admissionKey {
	return admissionKeyFor(schedulerAccountProvider(accounts.ProviderClaude), account, claudePoolModel("claude-sonnet-4-6"))
}

func TestAFailoverDoesNotLandOnAClosedQueue(t *testing.T) {
	for _, closed := range []bool{false, true} {
		name := "open"
		if closed {
			name = "closed"
		}
		t.Run(name, func(t *testing.T) {
			box, gate := gatedTargetBox(t, "roomy", "steady")
			box.pin(t, "warm", "steady")
			box.upstream.respond = func(account string, w http.ResponseWriter) {
				if account == "steady" {
					w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				w.Header().Set("anthropic-ratelimit-unified-status", "allowed")
				_, _ = io.WriteString(w, `{"id":"msg"}`)
			}
			if closed {
				fillQueue(t, gate, sonnetKey("roomy"), closedQueue)
			}
			got := box.answerWithin(t, box.turnAsync("warm", ""))
			hits := box.upstream.accounts()
			if !closed {
				if got.status != http.StatusOK || got.account != "roomy" {
					t.Fatalf("calibration: open, the failover answered %+v (upstream %v), want 200 on roomy; logs:\n%s", got, hits, box.logs.String())
				}
				return
			}
			for _, hit := range hits {
				if hit == "roomy" {
					t.Fatalf("the failover sent the session to roomy's closed queue: answered %+v, upstream %v; logs:\n%s", got, hits, box.logs.String())
				}
			}
			if got.account == "roomy" || box.pinnedTo(t, "warm") == "roomy" {
				t.Fatalf("the session landed on roomy's closed queue: answered %+v, pinned to %q", got, box.pinnedTo(t, "warm"))
			}
		})
	}
}

func TestARefreshFailoverDoesNotPickAClosedQueue(t *testing.T) {
	box, gate := gatedTargetBox(t, "roomy", "steady")
	tried := map[string]struct{}{"steady": {}}
	if next, err := box.server.retryAccount(context.Background(), accounts.ProviderClaude, "claude", "warm", "", tried, false); err != nil || next.ID != "roomy" {
		t.Fatalf("calibration: open, the refresh failover picked %q (%v), want roomy", next.ID, err)
	}
	fillQueue(t, gate, sonnetKey("roomy"), closedQueue)
	if next, err := box.server.retryAccount(context.Background(), accounts.ProviderClaude, "claude", "warm", "", tried, false); err == nil {
		t.Fatalf("the refresh failover picked %q with roomy's queue closed, want no replacement", next.ID)
	}
}

func (q *filledQueue) awaitBefore(t *testing.T, want int, answer <-chan queuedAnswer) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for q.queued() != want {
		select {
		case got := <-answer:
			t.Fatalf("answered %+v before its queue held %d waiters", got, want)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue holds %d waiters, want %d", q.queued(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAStaleSessionStaysOnItsClosedQueueWhenTheBestIsClosedToo(t *testing.T) {
	box, gate := gatedTargetBox(t, "roomy", "steady")
	box.pin(t, "stale", "steady")
	markIdle(box.active, "stale", warmSessionIdleTTL+time.Minute)
	fillQueue(t, gate, opusKey("roomy"), closedQueue)
	steady := fillQueue(t, gate, opusKey("steady"), closedQueue)
	answer := box.turnAsync("stale", "")
	steady.awaitBefore(t, closedQueue+1, answer)
	steady.drain()
	if got := box.answerWithin(t, answer); got.err != nil || got.status != http.StatusOK || got.account != "steady" {
		t.Fatalf("stale answered %+v, want 200 on steady; logs:\n%s", got, box.logs.String())
	}
	if got := box.pinnedTo(t, "stale"); got != "steady" {
		t.Fatalf("stale pinned to %q, want steady", got)
	}
}

func TestAStaleSessionOnAClosedQueueMovesToAnOpenAccount(t *testing.T) {
	box, gate := gatedTargetBox(t, "roomy", "steady")
	box.pin(t, "stale", "steady")
	markIdle(box.active, "stale", warmSessionIdleTTL+time.Minute)
	fillQueue(t, gate, opusKey("steady"), closedQueue)
	if got := box.answerWithin(t, box.turnAsync("stale", "")); got.err != nil || got.status != http.StatusOK || got.account != "roomy" {
		t.Fatalf("stale answered %+v, want 200 on roomy; logs:\n%s", got, box.logs.String())
	}
	if got := box.pinnedTo(t, "stale"); got != "roomy" {
		t.Fatalf("stale pinned to %q, want roomy", got)
	}
}

func TestAQueueFullOverflowMayLandOnAClosedAccount(t *testing.T) {
	box, gate := gatedTargetBox(t, "roomy", "steady")
	gate.limits = admissionLimits{Limit: 1, Queue: 2, ClosedQueue: 2}
	box.pin(t, "warm", "steady")
	fillQueue(t, gate, opusKey("steady"), 2)
	fillQueue(t, gate, sonnetKey("roomy"), 2)
	if !gate.closedAccounts()[selectacct.ScoreKey(accounts.ProviderClaude, "roomy")] {
		t.Fatal("calibration: roomy's queue is open")
	}
	got := box.answerWithin(t, box.turnAsync("warm", ""))
	if got.err != nil || got.status != http.StatusOK || got.account != "roomy" {
		t.Fatalf("the overflow off steady's full queue answered %+v, want 200 on roomy; logs:\n%s", got, box.logs.String())
	}
	if pinned := box.pinnedTo(t, "warm"); pinned != "steady" {
		t.Fatalf("the overflow moved warm's pin to %q, want it kept on steady", pinned)
	}
}

func TestAWarmSessionOnAClosedQueueQueuesThereBesideAnOpenAccount(t *testing.T) {
	box, gate := gatedTargetBox(t, "roomy", "steady")
	gate.limits.SpillQueue = 0
	box.pin(t, "warm", "roomy")
	queue := fillQueue(t, gate, opusKey("roomy"), closedQueue)
	if v := box.server.selectionScheduler("", "").NewSessionVerdict(accounts.ProviderClaude, "steady", time.Now()); !v.Eligible {
		t.Fatalf("calibration: steady %+v, want open and eligible", v)
	}
	answer := box.turnAsync("warm", "")
	queue.awaitBefore(t, closedQueue+1, answer)
	queue.drain()
	if got := box.answerWithin(t, answer); got.err != nil || got.status != http.StatusOK || got.account != "roomy" {
		t.Fatalf("warm answered %+v, want 200 on roomy; logs:\n%s", got, box.logs.String())
	}
	if got := box.pinnedTo(t, "warm"); got != "roomy" {
		t.Fatalf("warm pinned to %q, want roomy", got)
	}
	if logs := box.logs.String(); strings.Contains(logs, "session moved") || strings.Contains(logs, "re-ranking idle session") {
		t.Fatalf("the warm session was moved off its closed account; logs:\n%s", logs)
	}
}

func TestAWarmSessionOnAClosedQueueSpillsAndKeepsItsPin(t *testing.T) {
	box, gate := gatedTargetBox(t, "roomy", "steady")
	box.pin(t, "warm", "roomy")
	fillQueue(t, gate, opusKey("roomy"), closedQueue)
	if got := box.answerWithin(t, box.turnAsync("warm", "")); got.err != nil || got.status != http.StatusOK || got.account != "steady" {
		t.Fatalf("warm answered %+v, want its turn spilled to steady; logs:\n%s", got, box.logs.String())
	}
	if got := box.pinnedTo(t, "warm"); got != "roomy" {
		t.Fatalf("warm pinned to %q after a spill, want roomy", got)
	}
	if logs := box.logs.String(); !strings.Contains(logs, "admission spill") || strings.Contains(logs, "session moved") {
		t.Fatalf("want an admission spill and no move; logs:\n%s", logs)
	}
}

func TestASpillThatCarriesAMoveDoesNotLandOnAClosedQueue(t *testing.T) {
	box, gate := gatedTargetBox(t, "roomy", "steady")
	box.pin(t, "stale", "steady")
	markIdle(box.active, "stale", warmSessionIdleTTL+time.Minute)
	fillQueue(t, gate, sonnetKey("steady"), closedQueue)
	roomy := fillQueue(t, gate, opusKey("roomy"), gate.limits.SpillQueue)
	if free, _ := gate.freeLoad(opusKey("steady")); !free || !gate.closedAccounts()[selectacct.ScoreKey(accounts.ProviderClaude, "steady")] {
		t.Fatal("calibration: steady is not closed with a free Opus slot")
	}
	answer := box.turnAsync("stale", "")
	roomy.awaitBefore(t, gate.limits.SpillQueue+1, answer)
	roomy.drain()
	if got := box.answerWithin(t, answer); got.err != nil || got.status != http.StatusOK || got.account != "roomy" {
		t.Fatalf("stale answered %+v, want 200 on roomy, the account its move chose; logs:\n%s", got, box.logs.String())
	}
	if got := box.pinnedTo(t, "stale"); got != "roomy" {
		t.Fatalf("stale pinned to %q, want roomy, not steady's closed queue", got)
	}
	if strings.Contains(box.logs.String(), "admission spill") {
		t.Fatalf("a spill carrying the move went to steady's closed queue; logs:\n%s", box.logs.String())
	}
}

func TestAPlainSpillMayLandOnAClosedQueue(t *testing.T) {
	box, gate := gatedTargetBox(t, "roomy", "steady")
	box.pin(t, "warm", "roomy")
	fillQueue(t, gate, sonnetKey("steady"), closedQueue)
	fillQueue(t, gate, opusKey("roomy"), gate.limits.SpillQueue)
	if got := box.answerWithin(t, box.turnAsync("warm", "")); got.err != nil || got.status != http.StatusOK || got.account != "steady" {
		t.Fatalf("warm answered %+v, want its turn spilled to steady; logs:\n%s", got, box.logs.String())
	}
	if got := box.pinnedTo(t, "warm"); got != "roomy" {
		t.Fatalf("warm pinned to %q after a spill, want roomy", got)
	}
}

func TestASpillAfterATransientFailoverThatKeepsThePinMayLandOnAClosedQueue(t *testing.T) {
	box, gate := gatedTargetBox(t, "roomy", "steady", "chip-5")
	box.pin(t, "warm", "steady")
	box.upstream.respond = func(account string, w http.ResponseWriter) {
		if account == "steady" {
			w.Header().Set("anthropic-ratelimit-unified-status", "allowed_warning")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
			return
		}
		w.Header().Set("anthropic-ratelimit-unified-status", "allowed")
		_, _ = io.WriteString(w, `{"id":"msg"}`)
	}
	fillQueue(t, gate, opusKey("roomy"), gate.limits.SpillQueue)
	fillQueue(t, gate, sonnetKey("chip-5"), closedQueue)
	if free, _ := gate.freeLoad(opusKey("chip-5")); !free || !gate.closedAccounts()[selectacct.ScoreKey(accounts.ProviderClaude, "chip-5")] {
		t.Fatal("calibration: chip-5 is not closed with a free Opus slot")
	}
	got := box.answerWithin(t, box.turnAsync("warm", ""))
	if got.err != nil || got.status != http.StatusOK || got.account != "chip-5" {
		t.Fatalf("warm answered %+v, want the failover's turn spilled off roomy's queue to chip-5; upstream %v; logs:\n%s", got, box.upstream.accounts(), box.logs.String())
	}
	if pinned := box.pinnedTo(t, "warm"); pinned != "steady" {
		t.Fatalf("warm pinned to %q, want the transient failover to keep it on steady", pinned)
	}
	if logs := box.logs.String(); !strings.Contains(logs, "transient failover served this turn; the session keeps its account") {
		t.Fatalf("calibration: no kept pin logged; logs:\n%s", logs)
	}
}
