package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/session"
	"github.com/gorilla/websocket"
)

// What starts a stranded chat's hour over, and where its clock is kept
// (PATCHES §67). Only a model turn an account's upstream answered with a 2xx
// the client got, over HTTP or a websocket, starts it over. A box-held chat
// keeps its clock with its box hold, so the next worker that loads the hold
// gives up on the same hour.

// localAnsweredThenStranded is hold-session pinned to the local account and
// answered at warmWaitStart. between runs at 50 minutes; then the upstream
// stops answering dials at 59 minutes, and the turn held then must give up at
// the hour.
func localAnsweredThenStranded(t *testing.T, upstreamStatus *atomic.Int64, between func(rig *holdRig)) {
	t.Helper()
	clock := newMovableClock(warmWaitStart)
	policy := strandedPolicy(clock)
	var refusing atomic.Bool
	rig := newHoldRig(t, holdRigOptions{
		hold: policy,
		upstream: func(w http.ResponseWriter, r *http.Request) {
			if status := upstreamStatus.Load(); status != 0 {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(int(status))
				_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long"}}`))
				return
			}
			answerOK(w, r)
		},
		pin:    "one@example.com",
		active: seenSessions(time.Minute),
		failDials: func(int64) error {
			if refusing.Load() {
				return refusedFailure()
			}
			return nil
		},
		peer:  peerAnswer,
		spill: spillTestPolicy(DefaultOutageSpillRate, DefaultOutageSpillBurst, standingClock()),
	})
	if got := <-rig.sendAsync(t, holdTurnBody); got.status != http.StatusOK {
		t.Fatalf("calibration: status = %d body = %q, want the local answer", got.status, got.body)
	}
	rig.settle(t, 1)
	clock.set(warmWaitStart.Add(50 * time.Minute))
	between(rig)
	refusing.Store(true)
	clock.set(warmWaitStart.Add(59 * time.Minute))
	stranded := rig.sendAsync(t, holdTurnBody)
	awaitHeld(t, rig, policy, stranded, "a turn whose upstream cannot be dialed")
	clock.set(warmWaitStart.Add(warmSessionIdleTTL))
	gaveUp(t, rig, answeredWithin(t, rig, stranded, localAttempts(rig), 20, "an hour after the last answered model turn"), "an hour after the last answered model turn")
}

func TestASideRequestOrARefusalDoesNotStartTheHourOver(t *testing.T) {
	t.Run("a side request the local account answers", func(t *testing.T) {
		var status atomic.Int64
		localAnsweredThenStranded(t, &status, func(rig *holdRig) {
			resp, body, err := rig.post(t, promptly(t), "/v1/messages/count_tokens", nil)
			if err != nil {
				t.Fatal(err)
			}
			rig.settle(t, 1)
			if resp.StatusCode != http.StatusOK || !strings.Contains(body, "msg_held") {
				t.Fatalf("calibration: count_tokens status = %d body = %q, want the local account's 2xx", resp.StatusCode, body)
			}
		})
	})
	t.Run("a model turn the local account refuses", func(t *testing.T) {
		var status atomic.Int64
		localAnsweredThenStranded(t, &status, func(rig *holdRig) {
			status.Store(http.StatusBadRequest)
			got := <-rig.sendAsync(t, holdTurnBody)
			rig.settle(t, 1)
			status.Store(0)
			if got.status != http.StatusBadRequest {
				t.Fatalf("calibration: status = %d body = %q, want the upstream's 400", got.status, got.body)
			}
		})
	})
	t.Run("a side request box A answers", func(t *testing.T) {
		clock := newMovableClock(warmWaitStart)
		policy := strandedPolicy(clock)
		rig, up := newSwitchedPeerRig(t, policy)
		answeredOnAcctA(t, rig)
		clock.set(warmWaitStart.Add(50 * time.Minute))
		before := len(rig.boxA.seen())
		resp, body, err := rig.post(t, promptly(t), "/v1/messages/count_tokens", nil)
		if err != nil {
			t.Fatal(err)
		}
		rig.settle(t, 1)
		if resp.StatusCode != http.StatusOK || len(rig.boxA.seen()) != before+1 {
			t.Fatalf("calibration: count_tokens status = %d body = %q, box A turns %d -> %d, want box A's 2xx", resp.StatusCode, body, before, len(rig.boxA.seen()))
		}
		up.Store(false)
		stranded := strandedFrom(t, rig, policy, clock, warmWaitStart.Add(59*time.Minute), "a turn box A cannot be dialed for")
		clock.set(warmWaitStart.Add(warmSessionIdleTTL))
		gaveUp(t, rig.holdRig, answeredWithin(t, rig.holdRig, stranded, boxAttempts(rig), 20, "an hour after the last answered model turn"), "an hour after the last answered model turn")
	})
}

// codexTurnAsync is one HTTP Codex turn of sessionID on its own goroutine.
func codexTurnAsync(t *testing.T, base, path, sessionID string) <-chan clientAnswer {
	t.Helper()
	ctx := promptly(t)
	out := make(chan clientAnswer, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, strings.NewReader(`{"model":"gpt-5.6-sol","input":"hi"}`))
		if err != nil {
			out <- clientAnswer{err: err}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Session-Id", sessionID)
		resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
		if err != nil {
			out <- clientAnswer{err: err}
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		out <- clientAnswer{status: resp.StatusCode, body: string(body), retryAfter: resp.Header.Get("Retry-After"), reason: resp.Header.Get(subrouterReasonHeader)}
	}()
	return out
}

// webSocketTurn is one Codex websocket turn of sessionID, answered
// response.completed.
func webSocketTurn(t *testing.T, base, sessionID string, logs *syncBuffer) string {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/backend-api/codex/responses",
		http.Header{"Session-Id": []string{sessionID}})
	if err != nil {
		t.Fatalf("calibration: websocket dial: %v; logs:\n%s", err, logs.String())
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.6-sol","input":"hi"}`)); err != nil {
		t.Fatal(err)
	}
	_, msg, err := conn.ReadMessage()
	if err != nil || !strings.Contains(string(msg), "response.completed") {
		t.Fatalf("calibration: websocket turn %q %v; logs:\n%s", msg, err, logs.String())
	}
	return string(msg)
}

// strandedPastTheCompactsHour sends the chat's HTTP turns from 13:47, past
// the hour from its 12:46 compact: the first is held to its deadline and
// answered 502, and the client's resend at 14:44 is held until 14:45, an hour
// after the 13:45 websocket turn, and gives up there.
func strandedPastTheCompactsHour(t *testing.T, rig *holdRig, policy *HoldPolicy, clock *movableClock, tries attempts, send func() <-chan clientAnswer) {
	t.Helper()
	wsTurn := warmWaitStart.Add(59 * time.Minute)
	clock.set(warmWaitStart.Add(61 * time.Minute))
	first := send()
	awaitHeld(t, rig, policy, first, "the HTTP turn past the compact's hour")
	clock.set(warmWaitStart.Add(61*time.Minute + DefaultHoldDeadline + time.Second))
	retryable(t, answeredWithin(t, rig, first, tries, 20, "at its deadline"), http.StatusBadGateway)
	clock.set(wsTurn.Add(warmSessionIdleTTL - time.Minute))
	resent := send()
	awaitHeld(t, rig, policy, resent, "the client's resend")
	clock.set(wsTurn.Add(warmSessionIdleTTL - time.Second))
	heldFor(t, rig, resent, tries, 3, "a second before the hour from the websocket turn")
	clock.set(wsTurn.Add(warmSessionIdleTTL))
	gaveUp(t, rig, answeredWithin(t, rig, resent, tries, 20, "an hour after the websocket turn"), "an hour after the websocket turn")
}

// A Codex chat whose turns go over a websocket: one answered there starts the
// hour over, as an HTTP turn's 2xx does. The chat compacts over HTTP at
// 12:46, takes a websocket turn at 13:45, and is stranded from 13:47: it
// waits until 14:45, an hour after the websocket turn.
func TestAWebSocketTurnStartsTheHourOver(t *testing.T) {
	const sid = "codex-ws-chat"
	wsTurn := warmWaitStart.Add(59 * time.Minute)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	t.Run("on a box", func(t *testing.T) {
		clock := newMovableClock(warmWaitStart)
		boxA := newVerdictPeer(t, accounts.ProviderCodex, verdictPeerAccount{Score: codexVerdictScore("pa", 0.9, 50*time.Hour)})
		poolA := boxA.pool()
		var local atomic.Int64
		server, _, logs := codexCrossPoolLaptop(t, nil, func(string, http.ResponseWriter, *http.Request) bool {
			local.Add(1)
			return false
		})
		server.RemotePeers = []*RemotePool{poolA}
		policy := strandedPolicy(clock)
		server.Hold = policy
		poolA.delegations.setServed(session.ScopedSessionKey("codex", sid), time.Now(), remotePoolDelegationTTLFor("codex"), "pa", poolA.name(), true)
		front := newProxyTestServer(t, server.Handler())
		rig := &holdRig{logs: logs}
		if got := <-codexTurnAsync(t, front.URL, "/backend-api/codex/responses/compact", sid); got.status != http.StatusOK || !strings.Contains(got.body, "pa") {
			t.Fatalf("calibration: compact %d %q; logs:\n%s", got.status, got.body, logs.String())
		}
		clock.set(wsTurn)
		if msg := webSocketTurn(t, front.URL, sid, logs); !strings.Contains(msg, `"served_by":"pa"`) {
			t.Fatalf("calibration: websocket turn %q, want pa's", msg)
		}
		dials := cutPeer(t, poolA, func(int64) error { return refusedFailure() })
		tries := attempts{count: dials.Load, kick: func() { e2eRewindCooldown(poolA) }}

		strandedPastTheCompactsHour(t, rig, policy, clock, tries, func() <-chan clientAnswer {
			return codexTurnAsync(t, front.URL, "/backend-api/codex/responses", sid)
		})
		if local.Load() != 0 {
			t.Fatalf("the local upstream served %d turns, want none: the chat's cache is on pa", local.Load())
		}
	})
	t.Run("on the local account", func(t *testing.T) {
		clock := newMovableClock(warmWaitStart)
		boxB := newVerdictPeer(t, accounts.ProviderCodex, verdictPeerAccount{Score: codexVerdictScore("px", 0.1, 100*time.Hour)})
		server, store, logs := codexCrossPoolLaptop(t, nil, func(_ string, w http.ResponseWriter, r *http.Request) bool {
			if !websocket.IsWebSocketUpgrade(r) {
				return false
			}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return true
			}
			defer conn.Close()
			if _, _, err := conn.ReadMessage(); err == nil {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_ws"}}`))
			}
			_, _, _ = conn.ReadMessage()
			return true
		})
		server.RemotePeers = []*RemotePool{boxB.pool()}
		policy := strandedPolicy(clock)
		server.Hold = policy
		front := newProxyTestServer(t, server.Handler())
		if got := <-codexTurnAsync(t, front.URL, "/backend-api/codex/responses/compact", sid); got.status != http.StatusOK || !strings.Contains(got.body, `"served_by":"c1"`) {
			t.Fatalf("calibration: compact %d %q, want c1's answer; logs:\n%s", got.status, got.body, logs.String())
		}
		clock.set(wsTurn)
		webSocketTurn(t, front.URL, sid, logs)
		if assignment, ok := store.Get("codex", sid); !ok || assignment.AccountID != "c1" {
			t.Fatalf("calibration: assignment %+v (%v), want the chat on c1", assignment, ok)
		}

		var dials atomic.Int64
		undialable := server
		undialable.Transport = &http.Transport{DisableKeepAlives: true, DialContext: func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			return nil, refusedFailure()
		}}
		stranding := newProxyTestServer(t, undialable.Handler())
		rig := &holdRig{logs: logs}
		tries := attempts{count: dials.Load, kick: func() {}}
		strandedPastTheCompactsHour(t, rig, policy, clock, tries, func() <-chan clientAnswer {
			return codexTurnAsync(t, stranding.URL, "/backend-api/codex/responses", sid)
		})
		if turns := boxB.seen(); len(turns) != 0 {
			t.Fatalf("box B saw %+v, want nothing: the chat waits for its own upstream", turns)
		}
	})
}

// A chat held on a box with no local pin keeps its clock with its box hold
// (peer-holds.json), so a supervisor's next worker that loads the hold gives
// up an hour after the first worker's answer, not an hour after its own first
// held turn.
func TestABoxHeldChatKeepsItsGiveUpClockWithItsHold(t *testing.T) {
	sessionsPath := filepath.Join(t.TempDir(), "sessions.json")
	holdsPath := filepath.Join(t.TempDir(), "peer-holds.json")
	boxA := newContractBox(t, eligibleBoxAccount("acct-a"))
	cutPeer(t, boxA.pool, func(int64) error { return nil })
	first := newHoldRig(t, holdRigOptions{hold: strandedPolicy(newMovableClock(warmWaitStart)), upstream: answerOK,
		peers: []*RemotePool{boxA.pool}, sessionsPath: sessionsPath, active: seenSessions(time.Minute)})
	holdOn(t, boxA.pool, "hold-session", "acct-a")
	if got := <-first.sendAsync(t, holdTurnBody); got.status != http.StatusOK || !strings.Contains(got.body, `"served_by":"acct-a"`) {
		t.Fatalf("calibration: status = %d body = %q, want acct-a's answer; logs:\n%s", got.status, got.body, first.logs.String())
	}
	first.settle(t, 1)
	if assignment, ok := first.store.Get("claude", "hold-session"); ok {
		t.Fatalf("calibration: a local assignment %+v; want the chat on box A alone", assignment)
	}
	boxA.pool.delegations.flush(holdsPath, time.Now())

	nextA := NewRemotePool(boxA.srv.URL, "", "")
	nextA.delegations.loadFor(holdsPath, time.Now(), nextA.name())
	dials := cutPeer(t, nextA, func(int64) error { return refusedFailure() })
	clock := newMovableClock(warmWaitStart.Add(55 * time.Minute))
	policy := strandedPolicy(clock)
	next := newHoldRig(t, holdRigOptions{hold: policy, upstream: answerOK,
		peers: []*RemotePool{nextA}, sessionsPath: sessionsPath, active: seenSessions(time.Minute)})
	tries := attempts{count: dials.Load, kick: func() { e2eRewindCooldown(nextA) }}
	stranded := next.sendAsync(t, holdTurnBody)
	awaitHeld(t, next, policy, stranded, "the next worker's stranded turn")
	clock.set(warmWaitStart.Add(warmSessionIdleTTL - time.Second))
	heldFor(t, next, stranded, tries, 3, "a second before the hour from the first worker's answer")
	clock.set(warmWaitStart.Add(warmSessionIdleTTL))
	gaveUp(t, next, answeredWithin(t, next, stranded, tries, 20, "an hour after the first worker's answer"), "an hour after the first worker's answer")
}

// A laptop sleep stops Go's monotonic clock on darwin while the wall clock
// runs on. A box hold lapses on the wall clock (holdUntil strips the
// monotonic reading, PATCHES §54), so the give-up that must come before the
// lapse is on the wall clock too. On the monotonic clock, after a sleep the
// hold lapsed first and the chat was placed cold with no give-up. A test
// cannot move one clock without the other, so this reads the give-up time,
// which must carry no monotonic reading.
func TestTheGiveUpIsOnTheWallClockAsTheBoxHoldIs(t *testing.T) {
	key := session.ScopedSessionKey("claude", "hold-session")
	t.Run("after a turn answered on a box", func(t *testing.T) {
		policy := strandedPolicy(nil)
		rig, _ := newSwitchedPeerRig(t, policy)
		answeredOnAcctA(t, rig)
		hold, held := rig.boxA.pool.delegations.lookup(key, time.Now())
		if !held || !onTheWallClock(hold.Until) {
			t.Fatalf("calibration: box A's hold %+v (held %v), want one that lapses on the wall clock", hold, held)
		}
		if at := policy.lastAnswered(key, time.Time{}); !onTheWallClock(at) {
			t.Fatalf("the give-up's clock %s is on the monotonic clock, and box A's hold lapses on the wall clock at %s", at, hold.Until)
		}
	})
	t.Run("from a first held turn", func(t *testing.T) {
		policy := strandedPolicy(nil)
		if chain := policy.heldAttempt(key, time.Now()); !onTheWallClock(chain.Since) || !onTheWallClock(chain.Last) {
			t.Fatalf("the chain of held turns %+v is on the monotonic clock", chain)
		}
	})
}

// onTheWallClock reports a time with no monotonic reading, which every
// comparison with it then reads on the wall clock.
func onTheWallClock(at time.Time) bool {
	return at == at.Round(0)
}
