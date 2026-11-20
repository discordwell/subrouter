package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/manaflow-ai/subrouter/session"
)

// PATCHES §54: a breaker that opens while this machine itself is offline
// keeps every hold on the peer, in memory and on disk. Nothing here names an
// API §54 added, so each test runs unchanged against the code before it (and
// fails there).

// heldOnDisk is the set of session keys peer-holds.json lists.
func heldOnDisk(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(body, &stored); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(stored))
	for key := range stored {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// T13: two held sessions, a peer that refuses every connection, and nothing
// local answering: the 2026-10-09 reboot without the box's part. The breaker
// opens, both holds stay live, and the next flush still writes both.
func TestBreakerOpeningWhileOfflineKeepsEveryHoldOnDisk(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	server, first := heldSessionServer(t, deadURL)
	second := session.ScopedSessionKey("claude", "s2")
	server.RemotePool.delegations.set(second, time.Now(), remotePoolDelegationTTLFor("claude"), "peer-acct", "box")
	path := filepath.Join(t.TempDir(), "peer-holds.json")
	server.RemotePool.delegations.flush(path, time.Now())

	var out peerForwardOutcome
	for i := 0; i < remotePoolFailuresToOpen; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
		out = server.forwardToPeer(server.RemotePool, req, "claude", "s1", "/v1/messages", "", []byte(`{}`), remotePoolHoldSet, "peer-acct")
	}
	if server.RemotePool.breaker.allow(time.Now()) {
		t.Fatal("calibration: the breaker did not open; an unreachable peer must still cost a bounded wait")
	}
	for _, key := range []string{first, second} {
		if !server.RemotePool.delegations.active(key, time.Now()) {
			t.Fatalf("%s lost its hold when the breaker opened with nothing local answering", key)
		}
	}
	// The tripping turn is a blip too: served locally, its hold kept (P8).
	if out.Switch.Kind != switchTransient || out.Switch.Reason != "peer_unattributed" {
		t.Fatalf("the opening forward = %s/%q, want transient/peer_unattributed", out.Switch.Kind, out.Switch.Reason)
	}
	server.RemotePool.delegations.flush(path, time.Now())
	if got, want := heldOnDisk(t, path), []string{first, second}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("peer-holds.json after the opening = %v, want %v: a restart mid-outage would find no holds", got, want)
	}
}

// T14: the websocket dial is the same breaker, and keeps the holds the same way.
func TestWebSocketDialFailuresWhileOfflineKeepEveryHold(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	server := heldCodexWebSocketServer(t, deadURL)
	logs := &syncBuffer{}
	server.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	other := session.ScopedSessionKey("codex", "another-warm-session")
	server.RemotePool.delegations.set(other, time.Now(), remotePoolDelegationTTLFor("codex"), "peer-acct", server.RemotePool.name())
	path := filepath.Join(t.TempDir(), "peer-holds.json")
	server.RemotePool.delegations.flush(path, time.Now())

	var out peerForwardOutcome
	for i := 0; i < remotePoolFailuresToOpen; i++ {
		out = server.serveWebSocketToPeer(httptest.NewRecorder(), webSocketUpgradeRequest(context.Background()), server.RemotePool, "codex", wsHoldSession, remotePoolHoldSet, "peer-acct")
	}
	if server.RemotePool.breaker.allow(time.Now()) {
		t.Fatalf("calibration: the breaker did not open after %d failed dials", remotePoolFailuresToOpen)
	}
	for _, key := range []string{wsHoldKey(), other} {
		if !server.RemotePool.delegations.active(key, time.Now()) {
			t.Fatalf("%s lost its hold when the breaker opened on dials with nothing local answering", key)
		}
	}
	if out.Switch.Kind != switchTransient || out.Switch.Reason != "peer_unattributed" {
		t.Fatalf("the opening dial = %s/%q, want transient/peer_unattributed", out.Switch.Kind, out.Switch.Reason)
	}
	// The lines the ops/PATCHES.md §54 check reads: the opening dial's line
	// says the breaker opened and the peer is not down, and one holds-kept line
	// follows.
	lines := logLines(logs)
	opening := -1
	for i, line := range lines {
		if line["msg"] == "remote pool websocket dial failed" {
			opening = i
		}
	}
	if opening < 0 || lines[opening]["breaker_opened"] != true || lines[opening]["peer_down"] != false {
		t.Fatalf("log %v; want the opening dial's line with breaker_opened=true peer_down=false", lines)
	}
	if opening+1 >= len(lines) || lines[opening+1]["msg"] != holdsKeptMessage || lines[opening+1]["held"] != float64(2) || lines[opening+1]["why"] != "no_answer" {
		t.Fatalf("log %v; want a holds-kept line naming both holds, why=no_answer, right after the opening dial's line", lines)
	}
	if kept := e2eLogLines(logs, holdsKeptMessage); len(kept) != 1 {
		t.Fatalf("holds-kept lines %v, want exactly one, for the opening", kept)
	}
	server.RemotePool.delegations.flush(path, time.Now())
	if got := heldOnDisk(t, path); len(got) != 2 {
		t.Fatalf("peer-holds.json after the opening = %v, want both holds", got)
	}
}

// holdsKeptMessage is the line an opening that keeps the holds logs
// (ops/PATCHES.md §54 quotes it).
const holdsKeptMessage = "remote pool breaker opened; holds kept: outage not attributed to the peer"

// logLines is every JSON log line in logs, in order.
func logLines(logs *syncBuffer) []map[string]any {
	var out []map[string]any
	for _, raw := range strings.Split(logs.String(), "\n") {
		var line map[string]any
		if json.Unmarshal([]byte(raw), &line) == nil {
			out = append(out, line)
		}
	}
	return out
}

// sleptThrough is at as a Mac process reads the clock after the machine slept
// for d: the wall clock moved on, the monotonic clock did not (darwin's stops
// during sleep). time.Time is {wall uint64; ext int64; loc *Location}, and with
// a monotonic reading ext is that reading; the calibration below fails loudly
// if the layout ever changes.
func sleptThrough(t *testing.T, at time.Time, d time.Duration) time.Time {
	t.Helper()
	woke := at.Add(d)
	(*struct {
		wall uint64
		ext  int64
		loc  *time.Location
	})(unsafe.Pointer(&woke)).ext -= int64(d)
	if woke.Sub(at) != 0 || woke.Round(0).Sub(at.Round(0)) != d {
		t.Fatalf("calibration: could not build a time %v later on the wall clock only (monotonic %v, wall %v)",
			d, woke.Sub(at), woke.Round(0).Sub(at.Round(0)))
	}
	return woke
}

// Risk 4 of §54: holds compared their deadline on the monotonic clock, which
// stops while a Mac sleeps. A hold set before a two-hour lid-close was still
// live on waking, so the session's next turn went targeted to a peer account
// whose one-hour cache had gone cold. The idle bound must include sleep, as it
// does across a restart (the file stores the wall clock).
func TestAHoldLapsesAcrossALongSleep(t *testing.T) {
	pool := NewRemotePool("http://box.example:31416", "", "")
	key := session.ScopedSessionKey("claude", "s1")
	ttl := remotePoolDelegationTTLFor("claude")
	set := time.Now()
	pool.delegations.set(key, set, ttl, "peer-acct", pool.name())

	if !pool.delegations.active(key, sleptThrough(t, set, ttl/2)) {
		t.Fatal("calibration: a hold lapsed inside its idle bound")
	}
	if pool.delegations.active(key, sleptThrough(t, set, ttl+time.Hour)) {
		t.Fatalf("a hold set %v before the machine slept for %v was still live on waking", ttl, ttl+time.Hour)
	}

	// The touch that restarts the idle clock at the end of a turn, and the
	// pre-claim, are the same deadline.
	pool.delegations.set(key, set, ttl, "peer-acct", pool.name())
	pool.delegations.touch(key, set, ttl)
	if pool.delegations.active(key, sleptThrough(t, set, ttl+time.Hour)) {
		t.Fatal("a touched hold was still live after a sleep longer than its idle bound")
	}
	claimed := session.ScopedSessionKey("claude", "s2")
	pool.delegations.claim(claimed, set, ttl, pool.name())
	if pool.delegations.active(claimed, sleptThrough(t, set, ttl+time.Hour)) {
		t.Fatal("a pre-claim was still live after a sleep longer than its idle bound")
	}
}
