//go:build !windows

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// PATCHES section 77.3. These tests drive a real supervisor with real workers
// through its control socket, and stand by for the socket's lock the way a
// stand-in process does. The supervisor's clock is in the test's hands, so 5 s,
// 90 s and 10 minutes pass when the test says so. The keeper still looks on
// real time, every 250 ms while it holds no socket, so each wait below needs
// one or two looks and allows far more.

// claudeSocketYieldView and exitGuardView are supervisor-status's
// claude_socket_yield and claude_socket_exit_guard as another program reads
// them. The field names are the contract.
type claudeSocketYieldView struct {
	Reason string    `json:"reason"`
	Until  time.Time `json:"until"`
}

type exitGuardView struct {
	Enabled            bool    `json:"enabled"`
	ExitsToYield       int     `json:"exits_to_yield"`
	WindowSeconds      float64 `json:"window_seconds"`
	RecentExits        int     `json:"recent_exits"`
	StandingBy         string  `json:"standing_by"`
	ReturnAfterSeconds float64 `json:"return_after_seconds"`
}

func (v exitGuardView) String() string {
	text, _ := json.Marshal(v)
	return string(text)
}

// startYieldTestSupervisor runs a supervisor that serves a Claude socket, lets
// go of it on the third worker exit, and reads the test's clock.
func startYieldTestSupervisor(t *testing.T, adjust func(*supervisorConfig)) (*probedSupervisor, string) {
	t.Helper()
	var socket string
	p := startProbedSupervisor(t, func(config *supervisorConfig) {
		socket = filepath.Join(filepath.Dir(config.ControlSocket), "claude.sock")
		t.Setenv(claudeSocketEnv, socket)
		config.ClaudeSocketYieldExits = 3
		if adjust != nil {
			adjust(config)
		}
	})
	waitForWorkerThroughClaudeSocket(t, socket, p.control)
	return p, socket
}

// postClaudeSocketRoute posts one of the two routes and returns the status and
// the answer, which carries supervisor-status's claude_socket fields.
func postClaudeSocketRoute(t *testing.T, control *http.Client, route string) (int, supervisorControlStatus, string) {
	t.Helper()
	response, err := control.Post("http://supervisor/_subrouter/claude-socket/"+route, "application/json", nil)
	if err != nil {
		t.Fatalf("POST %s: %v", route, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var answer supervisorControlStatus
	if response.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &answer); err != nil {
			t.Fatalf("POST %s answered %q: %v", route, body, err)
		}
	}
	return response.StatusCode, answer, string(body)
}

func mustPostClaudeSocketRoute(t *testing.T, control *http.Client, route string) supervisorControlStatus {
	t.Helper()
	code, answer, body := postClaudeSocketRoute(t, control, route)
	if code != http.StatusOK {
		t.Fatalf("POST %s = %d %s, want 200", route, code, body)
	}
	return answer
}

func requireYielded(t *testing.T, status supervisorControlStatus, reason string, until time.Time, when string) {
	t.Helper()
	if status.ClaudeSocket != "yielded" {
		t.Fatalf("%s claude_socket = %q, want %q (claude_socket_exit_guard %s)", when, status.ClaudeSocket, "yielded", status.ClaudeSocketExitGuard)
	}
	yield := status.ClaudeSocketYield
	if yield == nil {
		t.Fatalf("%s supervisor-status says yielded and has no claude_socket_yield", when)
	}
	if yield.Reason != reason || !yield.Until.Equal(until) {
		t.Fatalf("%s claude_socket_yield = {%s, until %s}, want {%s, until %s}",
			when, yield.Reason, yield.Until.Format(time.RFC3339), reason, until.Format(time.RFC3339))
	}
}

// requireStillYielded gives the keeper three looks to end the yield before it
// reads the status.
func requireStillYielded(t *testing.T, control *http.Client, reason string, until time.Time, when string) {
	t.Helper()
	time.Sleep(3 * claudeSocketLockPoll)
	requireYielded(t, getSupervisorStatus(t, control), reason, until, when)
}

func requireServing(t *testing.T, status supervisorControlStatus, when string) {
	t.Helper()
	if status.ClaudeSocket != claudeSocketServing || status.ClaudeSocketYield != nil {
		t.Fatalf("%s claude_socket = %q, claude_socket_yield = %+v; want serving and no yield (claude_socket_exit_guard %s)",
			when, status.ClaudeSocket, status.ClaudeSocketYield, status.ClaudeSocketExitGuard)
	}
}

func requireClaudeSocketLockHeld(t *testing.T, socket, when string) {
	t.Helper()
	lease, err := acquireLocalDataSocketLease(socket)
	if err == nil {
		_ = lease.Close()
		t.Fatalf("the Claude socket's lock is free %s; want the supervisor to hold it", when)
	}
}

// killActiveWorker ends the active worker and returns supervisor-status once
// another worker is active. The supervisor decides about the socket before it
// starts that worker, so the status already shows what the exit led to.
func killActiveWorker(t *testing.T, control *http.Client) supervisorControlStatus {
	t.Helper()
	before := getSupervisorStatus(t, control)
	if err := syscall.Kill(before.Worker.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill the active worker (pid %d): %v", before.Worker.PID, err)
	}
	deadline := time.Now().Add(probeTestPatience)
	for {
		status, err := fetchSupervisorStatus(control)
		if err == nil && status.Active.ID != before.Active.ID && status.Worker.PID != 0 {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("no other worker became active %s after pid %d was killed: %v", probeTestPatience, before.Worker.PID, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func killActiveWorkers(t *testing.T, control *http.Client, count int) supervisorControlStatus {
	t.Helper()
	var status supervisorControlStatus
	for range count {
		status = killActiveWorker(t, control)
	}
	return status
}

// lockStandIn is a process standing by for the socket: blocked on its lock,
// and holding the lock from the moment the supervisor lets go until release.
type lockStandIn struct {
	taken chan struct{}

	mu       sync.Mutex
	fd       int
	released bool
}

func standByForClaudeSocketLock(t *testing.T, socket string) *lockStandIn {
	t.Helper()
	fd, err := syscall.Open(socket+".lock", syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	standIn := &lockStandIn{taken: make(chan struct{}), fd: fd}
	go func() {
		for {
			err := syscall.Flock(fd, syscall.LOCK_EX)
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			standIn.mu.Lock()
			if standIn.released || err != nil {
				_ = syscall.Close(fd)
				standIn.fd = -1
			} else {
				close(standIn.taken)
			}
			standIn.mu.Unlock()
			return
		}
	}()
	t.Cleanup(standIn.release)
	return standIn
}

func (s *lockStandIn) holdsLock() bool {
	select {
	case <-s.taken:
		return true
	default:
		return false
	}
}

func (s *lockStandIn) waitForLock(t *testing.T, when string) {
	t.Helper()
	select {
	case <-s.taken:
	case <-time.After(keeperTestPatience):
		t.Fatalf("the process standing by did not get the Claude socket's lock in %s %s", keeperTestPatience, when)
	}
}

// release gives the lock up, or stops waiting for it.
func (s *lockStandIn) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released {
		return
	}
	s.released = true
	if s.holdsLock() && s.fd >= 0 {
		_ = syscall.Close(s.fd)
		s.fd = -1
	}
}

// yieldForWorkerExits kills active workers until the supervisor lets go of the
// socket, with a process standing by that takes the lock, and lets the 5 s
// pass after which the supervisor knows somebody did.
func yieldForWorkerExits(t *testing.T, p *probedSupervisor, socket string, kills int) (*lockStandIn, supervisorControlStatus) {
	t.Helper()
	standIn := standByForClaudeSocketLock(t, socket)
	status := killActiveWorkers(t, p.control, kills)
	standIn.waitForLock(t, fmt.Sprintf("after %d worker exits", kills))
	if status.ClaudeSocket != "yielded" || status.ClaudeSocketYield == nil || status.ClaudeSocketYield.Reason != "worker_exits" {
		t.Fatalf("after %d worker exits claude_socket = %q, claude_socket_yield = %+v; want yielded for worker_exits (claude_socket_exit_guard %s)",
			kills, status.ClaudeSocket, status.ClaudeSocketYield, status.ClaudeSocketExitGuard)
	}
	p.clock.advance(5 * time.Second)
	waitForStandingBy(t, p.control, "seen")
	return standIn, status
}

func waitForStandingBy(t *testing.T, control *http.Client, want string) supervisorControlStatus {
	t.Helper()
	deadline := time.Now().Add(keeperTestPatience)
	for {
		status := getSupervisorStatus(t, control)
		if status.ClaudeSocketExitGuard.StandingBy == want {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("claude_socket_exit_guard.standing_by = %q after %s, want %q", status.ClaudeSocketExitGuard.StandingBy, keeperTestPatience, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Before this the fallback could be tried on the live job only by breaking
// the live job. The operator call is the drill: the supervisor lets go of the
// socket and its lock for a stated time, and takes them back afterwards.
func TestSupervisorYieldsTheClaudeSocketToAnOperator(t *testing.T) {
	p, socket := startYieldTestSupervisor(t, nil)

	gate := filepath.Join(filepath.Dir(socket), "gate")
	streams := streamingUnixClient(socket)
	defer streams.CloseIdleConnections()
	stream, err := streams.Get("http://api.anthropic.com/stream?gate=" + url.QueryEscape(gate))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	streamReader := bufio.NewReader(stream.Body)
	if line, err := streamReader.ReadString('\n'); err != nil || line != "first\n" {
		t.Fatalf("stream opened with %q, %v", line, err)
	}

	until := p.clock.Now().Add(10 * time.Minute)
	answer := mustPostClaudeSocketRoute(t, p.control, "yield?for=10m")
	// The call answers only once the lock is free, so a process standing by
	// has it by then. One second is the promise; no wait is needed.
	lease, err := acquireLocalDataSocketLease(socket)
	if err != nil {
		t.Fatalf("the Claude socket's lock is still held when the yield call has answered: %v", err)
	}
	requireYielded(t, answer, "operator", until, "the yield call's answer:")
	requireYielded(t, getSupervisorStatus(t, p.control), "operator", until, "after the yield call")
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the supervisor left its socket at the path it let go of: %v", err)
	}
	if pid, err := answeringWorkerPID(p.config.Addr); err != nil || pid == 0 {
		t.Fatalf("TCP stopped serving while the socket is yielded: pid %d, %v", pid, err)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if rest, err := io.ReadAll(streamReader); err != nil || string(rest) != "second\n" {
		t.Fatalf("the stream accepted before the yield ended with %q, %v; want it to finish", rest, err)
	}

	// Nobody stands by, and an operator's yield keeps its full time anyway.
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	p.clock.advance(9*time.Minute + 59*time.Second)
	requireStillYielded(t, p.control, "operator", until, "a second before the yield's time is up, with the lock free,")

	// With the time up the supervisor asks for the path again, and gets it
	// when the process that has the lock lets go.
	lease, err = acquireLocalDataSocketLease(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	p.clock.advance(time.Second)
	if status := waitForClaudeSocketWord(t, p.control, "waiting"); status.ClaudeSocketYield != nil {
		t.Fatalf("supervisor-status still has claude_socket_yield %+v after the yield's time was up", *status.ClaudeSocketYield)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	waitForWorkerThroughClaudeSocket(t, socket, p.control)
	requireServing(t, getSupervisorStatus(t, p.control), "after the yield")
}

func TestSupervisorTakesTheClaudeSocketBackWhenAnOperatorEndsTheYield(t *testing.T) {
	p, socket := startYieldTestSupervisor(t, nil)

	mustPostClaudeSocketRoute(t, p.control, "yield?for=1m")
	// A second yield call states the time again.
	until := p.clock.Now().Add(time.Hour)
	requireYielded(t, mustPostClaudeSocketRoute(t, p.control, "yield?for=60m"), "operator", until, "the second yield call's answer:")

	requireServing(t, mustPostClaudeSocketRoute(t, p.control, "reclaim"), "the reclaim call's answer:")
	requireClaudeSocketLockHeld(t, socket, "when the reclaim call has answered")
	waitForWorkerThroughClaudeSocket(t, socket, p.control)
	requireServing(t, mustPostClaudeSocketRoute(t, p.control, "reclaim"), "a reclaim call with nothing yielded:")

	// With the lock held elsewhere, the reclaim ends the yield and the
	// supervisor waits for the lock like any other time.
	mustPostClaudeSocketRoute(t, p.control, "yield?for=60m")
	lease, err := acquireLocalDataSocketLease(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	if answer := mustPostClaudeSocketRoute(t, p.control, "reclaim"); answer.ClaudeSocket != "waiting" || answer.ClaudeSocketYield != nil {
		t.Fatalf("reclaim with the lock held elsewhere answered claude_socket %q, claude_socket_yield %+v; want waiting and no yield",
			answer.ClaudeSocket, answer.ClaudeSocketYield)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	waitForWorkerThroughClaudeSocket(t, socket, p.control)
}

func TestSupervisorRefusesAYieldWithoutAStatedTimeOfAtMostAnHour(t *testing.T) {
	p, socket := startYieldTestSupervisor(t, nil)
	for _, route := range []string{"yield", "yield?for=", "yield?for=soon", "yield?for=0s", "yield?for=-1m", "yield?for=60m1s", "yield?for=24h"} {
		if code, _, body := postClaudeSocketRoute(t, p.control, route); code != http.StatusBadRequest {
			t.Fatalf("POST %s = %d %s, want 400", route, code, body)
		}
		requireServing(t, getSupervisorStatus(t, p.control), "after the refused POST "+route)
		requireClaudeSocketLockHeld(t, socket, "after the refused POST "+route)
	}
	until := p.clock.Now().Add(time.Hour)
	requireYielded(t, mustPostClaudeSocketRoute(t, p.control, "yield?for=60m"), "operator", until, "the answer to a yield for the longest time:")
}

func TestSupervisorWithoutAClaudeSocketHasNothingToYield(t *testing.T) {
	refused := func(t *testing.T, control *http.Client) {
		t.Helper()
		for _, route := range []string{"yield?for=1m", "reclaim"} {
			if code, _, body := postClaudeSocketRoute(t, control, route); code != http.StatusConflict {
				t.Fatalf("POST %s = %d %s, want 409", route, code, body)
			}
		}
	}
	t.Run("no Claude socket", func(t *testing.T) {
		dir := claudeSocketTestDir(t)
		t.Setenv(claudeSocketEnv, "")
		config := testSupervisorConfig(t, dir)
		config.ClaudeSocketYieldExits = 3
		startTestSupervisor(t, config)
		control := unixHTTPClient(config.ControlSocket)

		refused(t, control)
		response, err := control.Get("http://supervisor/_subrouter/supervisor-status")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var status map[string]any
		if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"claude_socket_yield", "claude_socket_exit_guard"} {
			if got, ok := status[key]; ok {
				t.Fatalf("a supervisor with no Claude socket reports %s = %v", key, got)
			}
		}
	})
	t.Run("a Claude socket path it refused at start", func(t *testing.T) {
		dir := claudeSocketTestDir(t)
		t.Setenv(claudeSocketEnv, "relative/claude.sock")
		config := testSupervisorConfig(t, dir)
		config.ClaudeSocketYieldExits = 3
		startTestSupervisor(t, config)
		control := unixHTTPClient(config.ControlSocket)

		refused(t, control)
		status := getSupervisorStatus(t, control)
		if status.ClaudeSocket != claudeSocketUnavailable {
			t.Fatalf("claude_socket = %q for a relative path, want %q", status.ClaudeSocket, claudeSocketUnavailable)
		}
		if guard := status.ClaudeSocketExitGuard; guard.Enabled || guard.StandingBy != "unknown" {
			t.Fatalf("claude_socket_exit_guard = %s with no socket kept, want enabled false and standing_by unknown", guard)
		}
	})
}

// A worker that reports ready and dies again and again is replaced each time,
// so the supervisor never exits, its lock is never free, and a process
// standing by never serves. Every stream is cut at each exit.
func TestSupervisorYieldsTheClaudeSocketOnTheThirdWorkerExit(t *testing.T) {
	p, socket := startYieldTestSupervisor(t, nil)
	standIn := standByForClaudeSocketLock(t, socket)

	if guard := getSupervisorStatus(t, p.control).ClaudeSocketExitGuard; !guard.Enabled || guard.ExitsToYield != 3 ||
		guard.WindowSeconds != 600 || guard.RecentExits != 0 || guard.StandingBy != "unknown" || guard.ReturnAfterSeconds != 90 {
		t.Fatalf("claude_socket_exit_guard = %s before any exit; want it enabled for 3 exits in 600 s, a 90 s return, nobody seen yet", guard)
	}
	for exit := 1; exit <= 2; exit++ {
		status := killActiveWorker(t, p.control)
		requireServing(t, status, fmt.Sprintf("after worker exit %d", exit))
		if status.ClaudeSocketExitGuard.RecentExits != exit {
			t.Fatalf("after worker exit %d claude_socket_exit_guard.recent_exits = %d", exit, status.ClaudeSocketExitGuard.RecentExits)
		}
	}
	if standIn.holdsLock() {
		t.Fatal("the process standing by got the lock after two worker exits")
	}

	status := killActiveWorker(t, p.control)
	standIn.waitForLock(t, "after the third worker exit")
	until := p.clock.Now().Add(90 * time.Second)
	requireYielded(t, status, "worker_exits", until, "after the third worker exit")
	if status.ClaudeSocketExitGuard.RecentExits != 3 {
		t.Fatalf("claude_socket_exit_guard.recent_exits = %d after three exits", status.ClaudeSocketExitGuard.RecentExits)
	}
	if pid, err := answeringWorkerPID(p.config.Addr); err != nil || pid != status.Worker.PID {
		t.Fatalf("TCP was answered by pid %d, %v while the socket is yielded; want the new worker %d", pid, err, status.Worker.PID)
	}

	// Somebody took the lock, so the supervisor stays away until the worker
	// has answered every probe for 90 s.
	p.clock.advance(5 * time.Second)
	waitForStandingBy(t, p.control, "seen")
	for round := 1; round <= 5; round++ {
		requireYielded(t, p.round(t, 0), "worker_exits", until, fmt.Sprintf("after answered probe %d", round))
	}
	requireStillYielded(t, p.control, "worker_exits", until, "80 s after the third exit")

	p.round(t, 0)
	if status := waitForClaudeSocketWord(t, p.control, "waiting"); status.ClaudeSocketYield != nil {
		t.Fatalf("supervisor-status still has claude_socket_yield %+v once the supervisor asks for the path", *status.ClaudeSocketYield)
	}
	standIn.release()
	waitForWorkerThroughClaudeSocket(t, socket, p.control)
	requireServing(t, getSupervisorStatus(t, p.control), "after the process standing by let go")
}

// The 90 s are 90 s in which the worker stayed up and answered. A missed probe
// or another exit starts them again.
func TestSupervisorAsksForTheClaudeSocketOnlyAfterTheWorkerStayedSteady(t *testing.T) {
	p, socket := startYieldTestSupervisor(t, nil)
	_, yielded := yieldForWorkerExits(t, p, socket, 3)
	until := yielded.ClaudeSocketYield.Until

	requireYielded(t, p.round(t, 0), "worker_exits", until, "after an answered probe")
	p.silenceActiveWorker(t)
	missed := p.round(t, 0)
	if missed.WorkerProbe.Misses != 1 {
		t.Fatalf("worker_probe.misses = %d after a probe of a silent worker, want 1", missed.WorkerProbe.Misses)
	}
	until = p.clock.Now().Add(90 * time.Second)
	requireYielded(t, missed, "worker_exits", until, "after a missed probe")
	if err := os.Remove(p.hangFile); err != nil {
		t.Fatal(err)
	}
	requireYielded(t, p.round(t, 0), "worker_exits", until, "after the worker answered again")

	exited := killActiveWorker(t, p.control)
	until = p.clock.Now().Add(90 * time.Second)
	requireYielded(t, exited, "worker_exits", until, "after a worker exit during the yield")

	for round := 1; round <= 5; round++ {
		requireYielded(t, p.round(t, 0), "worker_exits", until, fmt.Sprintf("after answered probe %d since the exit", round))
	}
	requireStillYielded(t, p.control, "worker_exits", until, "75 s after the exit")
	p.round(t, 0)
	waitForClaudeSocketWord(t, p.control, "waiting")
}

// A worker that comes back and fails again within 10 minutes of the return
// has to stay up twice as long the next time, up to 15 minutes.
func TestSupervisorDoublesTheWaitAfterAReturnThatFailed(t *testing.T) {
	p, socket := startYieldTestSupervisor(t, nil)

	// One cycle: the exits make the supervisor let go, the window passes, and
	// the supervisor has the socket back.
	cycle := func(kills int, wantWindow time.Duration) {
		t.Helper()
		standIn, yielded := yieldForWorkerExits(t, p, socket, kills)
		if got := yielded.ClaudeSocketExitGuard.ReturnAfterSeconds; got != wantWindow.Seconds() {
			t.Fatalf("claude_socket_exit_guard.return_after_seconds = %v, want %v", got, wantWindow.Seconds())
		}
		// yieldForWorkerExits let 5 s pass since the yield began.
		until := p.clock.Now().Add(wantWindow - 5*time.Second)
		requireYielded(t, getSupervisorStatus(t, p.control), "worker_exits", until, fmt.Sprintf("in a yield with a %s window", wantWindow))
		p.clock.advance(wantWindow - 6*time.Second)
		requireStillYielded(t, p.control, "worker_exits", until, fmt.Sprintf("a second before the %s window ends", wantWindow))
		p.clock.advance(time.Second)
		waitForClaudeSocketWord(t, p.control, "waiting")
		standIn.release()
		waitForWorkerThroughClaudeSocket(t, socket, p.control)
	}

	cycle(3, 90*time.Second)
	// Three exits in the last 10 minutes still stand, so one more is enough.
	cycle(1, 3*time.Minute)
	cycle(1, 6*time.Minute)
	cycle(1, 12*time.Minute)
	// The 12 minutes outlasted the earlier exits.
	cycle(3, 15*time.Minute)
	cycle(3, 15*time.Minute)

	// A yield more than 10 minutes after the last return starts at 90 s again.
	p.clock.advance(10*time.Minute + time.Second)
	cycle(3, 90*time.Second)
}

// On a Mac with no stand-in installed, letting go of the socket leaves its
// sessions with nothing. The supervisor finds out in 5 s and stops doing it.
func TestSupervisorTakesTheClaudeSocketBackWhenNobodyStandsBy(t *testing.T) {
	p, socket := startYieldTestSupervisor(t, nil)

	status := killActiveWorkers(t, p.control, 3)
	requireYielded(t, status, "worker_exits", p.clock.Now().Add(90*time.Second), "after the third worker exit")
	requireClaudeSocketLockFree(t, socket, "after the third worker exit")
	p.clock.advance(4 * time.Second)
	requireStillYielded(t, p.control, "worker_exits", p.clock.Now().Add(86*time.Second), "4 s after the supervisor let go")

	p.clock.advance(time.Second)
	waitForClaudeSocketWord(t, p.control, claudeSocketServing)
	waitForWorkerThroughClaudeSocket(t, socket, p.control)
	if got := getSupervisorStatus(t, p.control).ClaudeSocketExitGuard.StandingBy; got != "nobody" {
		t.Fatalf("claude_socket_exit_guard.standing_by = %q after 5 s with the lock free, want %q", got, "nobody")
	}

	// It does not let go for worker exits again.
	for exit := 4; exit <= 6; exit++ {
		status := killActiveWorker(t, p.control)
		if status.ClaudeSocketExitGuard.RecentExits != exit {
			t.Fatalf("claude_socket_exit_guard.recent_exits = %d after worker exit %d", status.ClaudeSocketExitGuard.RecentExits, exit)
		}
		requireServing(t, status, fmt.Sprintf("after worker exit %d, with nobody standing by,", exit))
		requireClaudeSocketLockHeld(t, socket, fmt.Sprintf("after worker exit %d, with nobody standing by", exit))
	}

	// Until it has seen another process hold the lock: here, when its
	// operator's yield ends and the lock is taken.
	mustPostClaudeSocketRoute(t, p.control, "yield?for=1m")
	lease, err := acquireLocalDataSocketLease(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	p.clock.advance(time.Minute)
	waitForClaudeSocketWord(t, p.control, "waiting")
	waitForStandingBy(t, p.control, "seen")
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	waitForWorkerThroughClaudeSocket(t, socket, p.control)

	standIn := standByForClaudeSocketLock(t, socket)
	status = killActiveWorker(t, p.control)
	standIn.waitForLock(t, "after an exit once a process had been seen holding the lock")
	requireYielded(t, status, "worker_exits", p.clock.Now().Add(90*time.Second), "after an exit once a process had been seen holding the lock")
}

func TestSupervisorCountsOnlyWorkerExitsOfTheLastTenMinutes(t *testing.T) {
	p, socket := startYieldTestSupervisor(t, nil)

	killActiveWorker(t, p.control)
	p.clock.advance(5 * time.Minute)
	killActiveWorker(t, p.control)
	p.clock.advance(5*time.Minute + time.Second)
	status := killActiveWorker(t, p.control)
	requireServing(t, status, "after three exits, the first of them more than 10 minutes ago,")
	if status.ClaudeSocketExitGuard.RecentExits != 2 {
		t.Fatalf("claude_socket_exit_guard.recent_exits = %d, want 2", status.ClaudeSocketExitGuard.RecentExits)
	}
	requireClaudeSocketLockHeld(t, socket, "after three exits, the first of them more than 10 minutes ago")

	status = killActiveWorker(t, p.control)
	requireYielded(t, status, "worker_exits", p.clock.Now().Add(90*time.Second), "after a third exit within 10 minutes")
}

func TestSupervisorKeepsTheClaudeSocketThroughWorkerExitsWithTheGuardOff(t *testing.T) {
	p, socket := startYieldTestSupervisor(t, func(config *supervisorConfig) { config.ClaudeSocketYieldExits = 0 })

	status := killActiveWorkers(t, p.control, 4)
	requireServing(t, status, "after four worker exits with --claude-socket-yield-exits 0")
	requireClaudeSocketLockHeld(t, socket, "after four worker exits with --claude-socket-yield-exits 0")
	if guard := status.ClaudeSocketExitGuard; guard.Enabled || guard.RecentExits != 0 {
		t.Fatalf("claude_socket_exit_guard = %s with the guard off, want enabled false and no count", guard)
	}
	// The operator's call does not depend on the guard.
	requireYielded(t, mustPostClaudeSocketRoute(t, p.control, "yield?for=1m"), "operator", p.clock.Now().Add(time.Minute), "an operator's yield with the guard off:")
}

func TestParseSupervisorConfigClaudeSocketYieldExits(t *testing.T) {
	base := []string{"--control-socket", "/var/run/subrouter-test.sock", "--worker-bin", "/usr/local/bin/subrouter"}
	config, err := parseSupervisorConfig(base)
	if err != nil {
		t.Fatal(err)
	}
	if config.ClaudeSocketYieldExits != 3 {
		t.Fatalf("--claude-socket-yield-exits defaults to %d, want 3", config.ClaudeSocketYieldExits)
	}
	if err := validateSupervisorConfig(config); err != nil {
		t.Fatalf("the defaults do not validate: %v", err)
	}
	off, err := parseSupervisorConfig(append([]string{"--claude-socket-yield-exits", "0"}, base...))
	if err != nil || off.ClaudeSocketYieldExits != 0 {
		t.Fatalf("--claude-socket-yield-exits 0 parsed as %d, %v", off.ClaudeSocketYieldExits, err)
	}
	if err := validateSupervisorConfig(off); err != nil {
		t.Fatalf("--claude-socket-yield-exits 0 does not validate: %v", err)
	}
	negative, err := parseSupervisorConfig(append([]string{"--claude-socket-yield-exits", "-1"}, base...))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSupervisorConfig(negative); err == nil {
		t.Fatal("--claude-socket-yield-exits -1 validates")
	}
}

// The operator's word is the last word: a yield for worker exits becomes the
// operator's when the operator yields, ends when the operator reclaims, and
// worker exits do not change an operator's yield.
func TestSupervisorOperatorCallsOverrideAYieldForWorkerExits(t *testing.T) {
	p, socket := startYieldTestSupervisor(t, nil)
	standIn, _ := yieldForWorkerExits(t, p, socket, 3)

	until := p.clock.Now().Add(30 * time.Minute)
	requireYielded(t, mustPostClaudeSocketRoute(t, p.control, "yield?for=30m"), "operator", until, "an operator's yield over a yield for worker exits:")
	requireYielded(t, killActiveWorker(t, p.control), "operator", until, "after a worker exit during an operator's yield")
	standIn.release()
	p.clock.advance(2 * time.Minute)
	requireStillYielded(t, p.control, "operator", until, "2 minutes into the operator's yield, with the lock free,")
	requireServing(t, mustPostClaudeSocketRoute(t, p.control, "reclaim"), "the reclaim call's answer:")

	standIn, _ = yieldForWorkerExits(t, p, socket, 1)
	if answer := mustPostClaudeSocketRoute(t, p.control, "reclaim"); answer.ClaudeSocket != "waiting" || answer.ClaudeSocketYield != nil {
		t.Fatalf("reclaim during a yield for worker exits answered claude_socket %q, claude_socket_yield %+v; want waiting and no yield",
			answer.ClaudeSocket, answer.ClaudeSocketYield)
	}
	standIn.release()
	waitForWorkerThroughClaudeSocket(t, socket, p.control)
}

type lockedLog struct {
	mu   sync.Mutex
	text bytes.Buffer
}

func (l *lockedLog) Write(line []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.Write(line)
}

func (l *lockedLog) has(part string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Contains(l.text.String(), part)
}

// Both ways of letting go are written to the log with their reason, so the
// log of a day on which sessions moved to a box says why they did.
func TestSupervisorLogsWhyItLetGoOfTheClaudeSocket(t *testing.T) {
	log := &lockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(log, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	p, socket := startYieldTestSupervisor(t, nil)

	mustPostClaudeSocketRoute(t, p.control, "yield?for=1m")
	if want := "reason=operator"; !log.has(want) {
		t.Fatalf("the log has no line with %s after an operator's yield", want)
	}
	mustPostClaudeSocketRoute(t, p.control, "reclaim")
	if log.has("reason=worker_exits") {
		t.Fatal("the log names worker exits as a reason before any worker exited")
	}
	yieldForWorkerExits(t, p, socket, 3)
	if want := "reason=worker_exits"; !log.has(want) {
		t.Fatalf("the log has no line with %s after the third worker exit", want)
	}
}

// A yield is kept in memory only. A supervisor that stops while it has let go
// of the socket leaves the lock free, and the next one serves the path.
func TestSupervisorForgetsAYieldWhenItRestarts(t *testing.T) {
	p, socket := startYieldTestSupervisor(t, nil)
	mustPostClaudeSocketRoute(t, p.control, "yield?for=60m")

	p.signals <- syscall.SIGTERM
	if err := p.waitExit(t, keeperTestPatience, "after SIGTERM while it had let go of the socket"); err != nil {
		t.Fatalf("run = %v", err)
	}
	requireClaudeSocketLockFree(t, socket, "after the supervisor exited")

	config := testSupervisorConfig(t, filepath.Dir(socket))
	config.ClaudeSocketYieldExits = 3
	startTestSupervisor(t, config)
	control := unixHTTPClient(config.ControlSocket)
	waitForWorkerThroughClaudeSocket(t, socket, control)
	requireServing(t, getSupervisorStatus(t, control), "in the supervisor started after one that had yielded")
}
