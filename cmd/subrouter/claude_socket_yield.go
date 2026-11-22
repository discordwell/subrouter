package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// PATCHES section 77.3. The keeper (claude_socket_keeper.go) can let go of the
// socket and its lock on purpose, so that a process standing by on the lock
// serves the path meanwhile. Two things ask for that. An operator asks for a
// stated time, which is how the stand-in is tried without breaking the daemon.
// The supervisor asks by itself when its active worker keeps failing: such a
// worker is replaced each time, so the supervisor never exits, its lock is
// never free, and every stream is cut at each exit.

// Fixed since 2026-11-21: the deploy scripts and a process that stands in on
// the socket path match the two reasons by name.
const (
	yieldReasonOperator    = "operator"
	yieldReasonWorkerExits = "worker_exits"
)

const (
	// maxOperatorYield is the longest time an operator can ask for.
	maxOperatorYield = 60 * time.Minute
	// yieldExitWindow is how far back worker failures count, and how soon
	// after a return another yield makes that return a failed one.
	yieldExitWindow = 10 * time.Minute
	// yieldStandbyGrace is how long a process standing by has to take the lock
	// after the supervisor lets go of it for worker exits.
	yieldStandbyGrace = 5 * time.Second
	// The active worker has to stay up and answer every probe for
	// yieldSteadyWindow before the supervisor asks for the socket back. The
	// time doubles after each failed return, up to yieldSteadyWindowMax.
	yieldSteadyWindow    = 90 * time.Second
	yieldSteadyWindowMax = 15 * time.Minute
)

// socketYield is a time the keeper stays away from the socket on purpose. It
// lives in memory only, so a supervisor that restarts has not yielded.
type socketYield struct {
	reason string
	// until is when the keeper asks for the socket again.
	until time.Time
	// standbyDue is when a yield for worker exits finds out whether a process
	// took the lock. It is zero in an operator's yield, which never asks, and
	// once the answer is in.
	standbyDue time.Time
}

// standbyKnowledge is whether another process holds the socket's lock when
// this one does not. Without such a process, letting go of the socket leaves
// its clients with nothing.
type standbyKnowledge string

const (
	standbyUnknown standbyKnowledge = "unknown"
	standbySeen    standbyKnowledge = "seen"
	standbyNobody  standbyKnowledge = "nobody"
)

// exitGuard counts the failures of active workers and holds the time a worker
// must stay steady before the socket comes back.
type exitGuard struct {
	// exitsToYield is --claude-socket-yield-exits. 0 turns the guard off.
	exitsToYield int
	failures     []time.Time
	steady       time.Duration
	// returned is when the last yield for worker exits ended with the keeper
	// asking for the socket again.
	returned time.Time
}

func (g *exitGuard) failed(now time.Time) int {
	g.failures = append(g.recent(now), now)
	return len(g.failures)
}

func (g *exitGuard) recent(now time.Time) []time.Time {
	kept := g.failures[:0]
	for _, at := range g.failures {
		if now.Sub(at) <= yieldExitWindow {
			kept = append(kept, at)
		}
	}
	g.failures = kept
	return kept
}

// nextSteady is the steady time of a yield that starts now.
func (g *exitGuard) nextSteady(now time.Time) time.Duration {
	if g.returned.IsZero() || now.Sub(g.returned) > yieldExitWindow {
		return yieldSteadyWindow
	}
	return min(2*g.steady, yieldSteadyWindowMax)
}

// claudeSocketStatus is what the keeper found at its last look, as
// supervisor-status reports it. yield is set exactly while word is yielded.
type claudeSocketStatus struct {
	word  string
	yield *claudeSocketYieldStatus
	guard exitGuardStatus
}

// socketWord is the status of a socket that no keeper reports on.
func socketWord(word string) claudeSocketStatus {
	return claudeSocketStatus{word: word, guard: exitGuardStatus{StandingBy: standbyUnknown}}
}

type claudeSocketYieldStatus struct {
	Reason string `json:"reason"`
	// Until is RFC 3339 in UTC with whole seconds, which date(1) parses on
	// macOS and on Linux.
	Until string `json:"until"`
}

type exitGuardStatus struct {
	Enabled       bool    `json:"enabled"`
	ExitsToYield  int     `json:"exits_to_yield,omitempty"`
	WindowSeconds float64 `json:"window_seconds,omitempty"`
	// RecentExits is the number of worker failures in the last WindowSeconds.
	RecentExits int              `json:"recent_exits"`
	StandingBy  standbyKnowledge `json:"standing_by"`
	// ReturnAfterSeconds is how long the worker had to stay steady in the
	// last yield for worker exits, or has to in the one under way.
	ReturnAfterSeconds float64 `json:"return_after_seconds,omitempty"`
}

func (k *socketKeeper) now() time.Time {
	// Round(0) leaves the wall clock, which is what until shows. Go's
	// monotonic clock stops while a Mac sleeps.
	return k.clock().Round(0)
}

// tell reports the word together with the yield and the guard as they stand.
func (k *socketKeeper) tell(word string) {
	status := socketWord(word)
	status.guard.StandingBy = k.standby
	if k.yield != nil {
		status.yield = &claudeSocketYieldStatus{Reason: k.yield.reason, Until: k.yield.until.UTC().Format(time.RFC3339)}
	}
	if g := &k.guard; g.exitsToYield > 0 {
		status.guard.Enabled = true
		status.guard.ExitsToYield = g.exitsToYield
		status.guard.WindowSeconds = yieldExitWindow.Seconds()
		status.guard.RecentExits = len(g.recent(k.now()))
		status.guard.ReturnAfterSeconds = g.steady.Seconds()
	}
	k.report(status)
}

type keeperRequest struct {
	change func(now time.Time)
	done   chan struct{}
}

// ask runs change on the keeper's goroutine and returns after the look that
// follows it, so the caller finds the socket as its request left it. It
// reports false when the keeper has stopped.
func (k *socketKeeper) ask(change func(now time.Time)) bool {
	request := keeperRequest{change: change, done: make(chan struct{})}
	select {
	case k.requests <- request:
		<-request.done
		return true
	case <-k.stop:
		return false
	}
}

func (k *socketKeeper) startYield(y socketYield, why ...any) {
	k.yield = &y
	k.lastLogged = ""
	attrs := append([]any{"path", k.path, "reason", y.reason, "until", y.until.UTC().Format(time.RFC3339)}, why...)
	k.logger.Warn("claude socket yielded: the supervisor lets go of the socket and its lock; TCP is unaffected", attrs...)
}

func (k *socketKeeper) endYield(now time.Time, why string) {
	ended := k.yield
	k.yield = nil
	k.lastLogged = ""
	if ended.reason == yieldReasonWorkerExits {
		k.guard.returned = now
	}
	k.logger.Info("claude socket yield is over: asking for the socket again", "path", k.path, "reason", ended.reason, "why", why)
	k.tell(claudeSocketWaiting)
}

// lookWhileYielded is the look of a keeper that has let go on purpose. It
// reports false when the yield has just ended and the socket is to be taken
// as in any other look.
func (k *socketKeeper) lookWhileYielded() (done bool) {
	y, now := k.yield, k.now()
	if !now.Before(y.until) {
		k.endYield(now, "its time is up")
		return false
	}
	if !y.standbyDue.IsZero() && !now.Before(y.standbyDue) {
		y.standbyDue = time.Time{}
		// One try at the path answers it. With the lock held elsewhere the
		// try changes nothing. With the lock free nobody stands by, and the
		// try has taken the path back.
		if err := k.take(); !socketLockHeldElsewhere(err) {
			k.standby = standbyNobody
			k.yield = nil
			k.lastLogged = ""
			k.logger.Warn("nobody took the claude socket's lock: taking the socket back, and keeping it through worker exits until another process is seen holding the lock",
				"path", k.path, "waited", yieldStandbyGrace)
			k.took(err)
			return true
		}
	}
	k.tell(claudeSocketYielded)
	return true
}

// yieldFor is the operator's yield. It replaces a yield already under way.
func (k *socketKeeper) yieldFor(length time.Duration) bool {
	return k.ask(func(now time.Time) {
		k.startYield(socketYield{reason: yieldReasonOperator, until: now.Add(length)})
	})
}

// reclaim ends a yield early, whatever its reason.
func (k *socketKeeper) reclaim() bool {
	return k.ask(func(now time.Time) {
		if k.yield != nil {
			k.endYield(now, "an operator ended it")
		}
	})
}

// workerFailed counts one failure of an active worker. The third within
// yieldExitWindow makes the keeper let go of the socket, unless an operator's
// yield is under way or nobody stands by for the lock.
func (k *socketKeeper) workerFailed() {
	k.ask(func(now time.Time) {
		g := &k.guard
		if g.exitsToYield <= 0 {
			return
		}
		recent := g.failed(now)
		switch {
		case k.yield != nil:
			k.steadyAgain(now)
		case recent < g.exitsToYield || k.standby == standbyNobody:
		default:
			g.steady = g.nextSteady(now)
			k.startYield(socketYield{
				reason:     yieldReasonWorkerExits,
				until:      now.Add(g.steady),
				standbyDue: now.Add(yieldStandbyGrace),
			}, "worker_exits", recent, "within", yieldExitWindow)
		}
	})
}

// workerFaltered is a probe the active worker did not answer.
func (k *socketKeeper) workerFaltered() {
	k.ask(func(now time.Time) { k.steadyAgain(now) })
}

// steadyAgain starts the steady time of a yield for worker exits again. An
// operator's yield has its own end.
func (k *socketKeeper) steadyAgain(now time.Time) {
	if k.yield != nil && k.yield.reason == yieldReasonWorkerExits {
		k.yield.until = now.Add(k.guard.steady)
	}
}

func (c *claudeSocket) kept() *socketKeeper {
	if c == nil {
		return nil
	}
	return c.keeper.Load()
}

func (c *claudeSocket) workerFailed() {
	if keeper := c.kept(); keeper != nil {
		keeper.workerFailed()
	}
}

func (c *claudeSocket) workerFaltered() {
	if keeper := c.kept(); keeper != nil {
		keeper.workerFaltered()
	}
}

// describe adds the socket's fields of supervisor-status. The two routes
// below answer with the same fields.
func (c *claudeSocket) describe(status map[string]any) {
	found := c.state.Load().(claudeSocketStatus)
	status["claude_socket"] = found.word
	// A process that stands in on the socket path checks this before it
	// takes the lock: a supervisor without it tries the socket once at
	// start and would never take the path back. PATCHES section 77.1.
	status["claude_socket_waits_for_lock"] = true
	status["claude_socket_rebinds"] = c.rebinds()
	if found.yield != nil {
		status["claude_socket_yield"] = found.yield
	}
	status["claude_socket_exit_guard"] = found.guard
}

// registerClaudeSocketHandlers adds the operator's two calls. Both answer
// after the keeper has acted, so when the yield call has answered the lock is
// free, and when the reclaim call has answered the supervisor either serves
// the path again or waits for it.
func (s *supervisor) registerClaudeSocketHandlers(mux *http.ServeMux) {
	answer := func(w http.ResponseWriter, kept bool) {
		if !kept {
			http.Error(w, "this supervisor keeps no Claude socket", http.StatusConflict)
			return
		}
		status := map[string]any{}
		s.claudeSocket.describe(status)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(status)
	}
	mux.HandleFunc("POST /_subrouter/claude-socket/yield", func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.Query().Get("for")
		length, err := time.ParseDuration(raw)
		if err != nil || length <= 0 || length > maxOperatorYield {
			http.Error(w, fmt.Sprintf("for=%q must be a time of at most %s, such as 10m", raw, maxOperatorYield), http.StatusBadRequest)
			return
		}
		keeper := s.claudeSocket.kept()
		answer(w, keeper != nil && keeper.yieldFor(length))
	})
	mux.HandleFunc("POST /_subrouter/claude-socket/reclaim", func(w http.ResponseWriter, _ *http.Request) {
		keeper := s.claudeSocket.kept()
		answer(w, keeper != nil && keeper.reclaim())
	})
}
