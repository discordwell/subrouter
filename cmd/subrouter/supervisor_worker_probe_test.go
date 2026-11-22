//go:build !windows

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// PATCHES section 77.2. These tests drive a real supervisor with real workers
// and silence a worker the way a hang does: it stays alive, keeps accepting
// connections and stops answering. All but the first hand the probe's ticks
// to the test, so each round is one call and nothing waits on a real 15 s.
// A silent worker costs each probe its whole timeout, and a loaded machine is
// slow, so the waits below allow far more than they need.
const (
	probeTestPatience = 60 * time.Second
	probeTestTimeout  = time.Second
)

// workerProbeView is supervisor-status's worker_probe as another program
// reads it. The field names are the contract.
type workerProbeView struct {
	Enabled         bool    `json:"enabled"`
	IntervalSeconds float64 `json:"interval_seconds"`
	TimeoutSeconds  float64 `json:"timeout_seconds"`
	MissesToReplace int     `json:"misses_to_replace"`
	Misses          int     `json:"misses"`
	ReplaceError    string  `json:"replace_error"`
	LastReplacement *struct {
		At     time.Time `json:"at"`
		Reason string    `json:"reason"`
		From   string    `json:"from"`
		To     string    `json:"to"`
	} `json:"last_replacement"`
}

func (v workerProbeView) String() string {
	text, _ := json.Marshal(v)
	return string(text)
}

type probeTestWait struct {
	length time.Duration
	end    chan time.Time
}

// probeTestClock is the supervisor's clock with its waits in the test's
// hands. The probe loop arms one wait at a time, and arms the next only once
// the round the last one started is over.
type probeTestClock struct {
	mu    sync.Mutex
	now   time.Time
	armed chan probeTestWait
}

func newProbeTestClock() *probeTestClock {
	return &probeTestClock{
		now:   time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC),
		armed: make(chan probeTestWait, 4),
	}
}

func (c *probeTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *probeTestClock) After(length time.Duration) <-chan time.Time {
	wait := probeTestWait{length: length, end: make(chan time.Time, 1)}
	c.armed <- wait
	return wait.end
}

func (c *probeTestClock) advance(by time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(by)
	return c.now
}

func (c *probeTestClock) nextWait(t *testing.T) probeTestWait {
	t.Helper()
	select {
	case wait := <-c.armed:
		return wait
	case <-time.After(probeTestPatience):
		t.Fatalf("the supervisor armed no probe tick in %s", probeTestPatience)
		return probeTestWait{}
	}
}

type probedSupervisor struct {
	*testSupervisor
	config   supervisorConfig
	control  *http.Client
	hangFile string
	clock    *probeTestClock
	wait     probeTestWait
}

func probeTestConfig(t *testing.T) (supervisorConfig, string) {
	t.Helper()
	dir := claudeSocketTestDir(t)
	hangFile := filepath.Join(dir, "silent-worker")
	t.Setenv("SUBROUTER_TEST_FAKE_WORKER_HANG_FILE", hangFile)
	config := testSupervisorConfig(t, dir)
	config.WorkerProbeInterval = 15 * time.Second
	config.WorkerProbeTimeout = probeTestTimeout
	config.WorkerProbeMisses = 3
	return config, hangFile
}

// startProbedSupervisor runs a supervisor whose probe ticks only when the
// test calls round.
func startProbedSupervisor(t *testing.T, adjust func(*supervisorConfig)) *probedSupervisor {
	t.Helper()
	config, hangFile := probeTestConfig(t)
	if adjust != nil {
		adjust(&config)
	}
	clock := newProbeTestClock()
	p := &probedSupervisor{
		testSupervisor: startTestSupervisorWith(t, config, func(s *supervisor) { s.clock = clock }),
		config:         config,
		control:        unixHTTPClient(config.ControlSocket),
		hangFile:       hangFile,
		clock:          clock,
	}
	p.wait = clock.nextWait(t)
	if p.wait.length != config.WorkerProbeInterval {
		t.Fatalf("the probe waits %s between rounds, want --worker-probe-interval (%s)", p.wait.length, config.WorkerProbeInterval)
	}
	return p
}

// round lets the armed tick arrive, late by the given amount, and returns
// supervisor-status as it stands once the round that tick started is over.
func (p *probedSupervisor) round(t *testing.T, late time.Duration) supervisorControlStatus {
	t.Helper()
	p.wait.end <- p.clock.advance(p.wait.length + late)
	p.wait = p.clock.nextWait(t)
	return getSupervisorStatus(t, p.control)
}

func silenceWorker(t *testing.T, hangFile string, pid int) {
	t.Helper()
	if err := os.WriteFile(hangFile, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// silenceActiveWorker makes the active worker stop answering and returns
// supervisor-status from just before.
func (p *probedSupervisor) silenceActiveWorker(t *testing.T) supervisorControlStatus {
	t.Helper()
	status := getSupervisorStatus(t, p.control)
	silenceWorker(t, p.hangFile, status.Worker.PID)
	return status
}

func requireProbeMisses(t *testing.T, status supervisorControlStatus, want int, active, when string) {
	t.Helper()
	if status.Active.ID != active {
		t.Fatalf("%s the active worker is %s, want it still to be %s (worker_probe %s)", when, status.Active.ID, active, status.WorkerProbe)
	}
	if status.WorkerProbe.Misses != want {
		t.Fatalf("%s worker_probe.misses = %d, want %d", when, status.WorkerProbe.Misses, want)
	}
}

func requireReplaced(t *testing.T, status supervisorControlStatus, silent supervisorControlStatus, when string) {
	t.Helper()
	if status.Active.ID == silent.Active.ID || status.Worker.PID == silent.Worker.PID {
		t.Fatalf("%s the silent worker %s (pid %d) is still the active one (worker_probe %s)",
			when, silent.Active.ID, silent.Worker.PID, status.WorkerProbe)
	}
	last := status.WorkerProbe.LastReplacement
	if last == nil {
		t.Fatalf("%s supervisor-status has no worker_probe.last_replacement", when)
	}
	if last.From != silent.Active.ID || last.To != status.Active.ID || last.Reason == "" || last.At.IsZero() {
		t.Fatalf("%s last_replacement = %+v, want from %s to %s with a reason and a time", when, *last, silent.Active.ID, status.Active.ID)
	}
	if status.WorkerProbe.Misses != 0 {
		t.Fatalf("%s worker_probe.misses = %d, want the new worker to start at 0", when, status.WorkerProbe.Misses)
	}
}

func answeringWorkerPID(address string) (int, error) {
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Get("http://" + address + "/")
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return strconv.Atoi(response.Header.Get("X-Fake-Worker-Pid"))
}

// With the long client wait, a session whose worker stopped answering no
// longer fails after 13 minutes: it sends the turn again every 10 minutes
// with no end. Nothing replaced such a worker, because it never exits.
func TestSupervisorReplacesUnresponsiveWorker(t *testing.T) {
	config, hangFile := probeTestConfig(t)
	config.WorkerProbeInterval = 100 * time.Millisecond
	startTestSupervisor(t, config)
	control := unixHTTPClient(config.ControlSocket)

	// A stream the worker is already sending when it goes silent.
	gate := filepath.Join(filepath.Dir(hangFile), "gate")
	streams := &http.Client{}
	t.Cleanup(streams.CloseIdleConnections)
	stream, err := streams.Get("http://" + config.Addr + "/stream?gate=" + url.QueryEscape(gate))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	streamReader := bufio.NewReader(stream.Body)
	if line, err := streamReader.ReadString('\n'); err != nil || line != "first\n" {
		t.Fatalf("stream opened with %q, %v", line, err)
	}

	silent := getSupervisorStatus(t, control)
	if silent.WorkerProbe.LastReplacement != nil {
		t.Fatalf("a worker that answers was replaced: %+v", *silent.WorkerProbe.LastReplacement)
	}
	silenceWorker(t, hangFile, silent.Worker.PID)

	deadline := time.Now().Add(probeTestPatience)
	for {
		pid, err := answeringWorkerPID(config.Addr)
		if err == nil && pid != silent.Worker.PID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no other worker answered on the port %s after pid %d went silent: last answer pid %d, %v",
				probeTestPatience, silent.Worker.PID, pid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	after := getSupervisorStatus(t, control)
	requireReplaced(t, after, silent, "after the worker went silent")
	if !after.WorkerProbe.Enabled || after.WorkerProbe.MissesToReplace != 3 ||
		after.WorkerProbe.IntervalSeconds != 0.1 || after.WorkerProbe.TimeoutSeconds != probeTestTimeout.Seconds() {
		t.Fatalf("worker_probe = %s, want it enabled with the configured interval, timeout and 3 misses", after.WorkerProbe)
	}

	// The silent worker is retired the way a deploy retires one: what it
	// still sends is left to finish, and it is stopped once nothing is left.
	if !supervisorBackendPresent(after.Backends, silent.Active.ID) {
		t.Fatal("the silent worker was dropped while a stream it was sending was still open")
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(streamReader)
	if err != nil || string(rest) != "second\n" {
		t.Fatalf("the stream the silent worker was sending ended with %q, %v; want it to finish", rest, err)
	}
	_ = stream.Body.Close()
	streams.CloseIdleConnections()
	deadline = time.Now().Add(probeTestPatience)
	for {
		gone := !supervisorBackendPresent(getSupervisorStatus(t, control).Backends, silent.Active.ID)
		if gone && errors.Is(syscall.Kill(silent.Worker.PID, 0), syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the silent worker (pid %d) was not stopped %s after its last stream closed", silent.Worker.PID, probeTestPatience)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Readiness is a routing preference: an initial worker may answer 503 for as
// long as it runs and still serve every request.
func TestSupervisorKeepsAWorkerThatAnswersUnready(t *testing.T) {
	t.Setenv("SUBROUTER_TEST_FAKE_WORKER_NEVER_READY", "1")
	p := startProbedSupervisor(t, func(config *supervisorConfig) { config.ReadyTimeout = 300 * time.Millisecond })
	// A replacement would report ready, so only the probe's own rule keeps
	// the unready worker.
	t.Setenv("SUBROUTER_TEST_FAKE_WORKER_NEVER_READY", "")
	unready := getSupervisorStatus(t, p.control)

	for round := 1; round <= 6; round++ {
		status := p.round(t, 0)
		requireProbeMisses(t, status, 0, unready.Active.ID, fmt.Sprintf("after probe %d of a worker that answers 503", round))
		if status.WorkerProbe.LastReplacement != nil {
			t.Fatalf("a worker that answers 503 was replaced: %+v", *status.WorkerProbe.LastReplacement)
		}
	}
	if pid, err := answeringWorkerPID(p.config.Addr); err != nil || pid != unready.Worker.PID {
		t.Fatalf("the port was answered by pid %d, %v; want the unready worker %d", pid, err, unready.Worker.PID)
	}
}

// A Mac that sleeps stops every process on it. The tick that arrives after it
// wakes says nothing about the worker, and neither do the misses from before.
func TestSupervisorDoesNotReadALateProbeTickAsAHang(t *testing.T) {
	p := startProbedSupervisor(t, nil)
	silent := p.silenceActiveWorker(t)

	requireProbeMisses(t, p.round(t, 0), 1, silent.Active.ID, "after the first silent probe")
	requireProbeMisses(t, p.round(t, 0), 2, silent.Active.ID, "after the second silent probe")
	requireProbeMisses(t, p.round(t, time.Hour), 1, silent.Active.ID, "after a tick that came an hour late")
	// More than 10 s late is the rule, so 10 s late is still on time.
	requireProbeMisses(t, p.round(t, 10*time.Second), 2, silent.Active.ID, "after a tick that came 10 s late")

	replaced := p.round(t, 0)
	requireReplaced(t, replaced, silent, "after three silent probes in a row")
	if at := replaced.WorkerProbe.LastReplacement.At; !at.Equal(p.clock.Now()) {
		t.Fatalf("last_replacement.at = %s, want the time of the replacement (%s)", at, p.clock.Now())
	}
}

// A worker nobody can reach is as silent as one that hangs. Its private socket
// sits in the temporary folder, where other programs delete files.
func TestSupervisorReplacesAWorkerWhoseSocketIsGone(t *testing.T) {
	p := startProbedSupervisor(t, nil)
	unreachable := getSupervisorStatus(t, p.control)
	if err := os.Remove(unreachable.Active.Address); err != nil {
		t.Fatal(err)
	}
	p.round(t, 0)
	requireProbeMisses(t, p.round(t, 0), 2, unreachable.Active.ID, "after two probes of a worker whose socket is gone")
	requireReplaced(t, p.round(t, 0), unreachable, "after three probes of a worker whose socket is gone")
	if pid, err := answeringWorkerPID(p.config.Addr); err != nil || pid == unreachable.Worker.PID {
		t.Fatalf("the port was answered by pid %d, %v; want the new worker", pid, err)
	}
}

func TestSupervisorForgetsMissesOnceTheWorkerAnswers(t *testing.T) {
	p := startProbedSupervisor(t, nil)
	silent := p.silenceActiveWorker(t)
	p.round(t, 0)
	requireProbeMisses(t, p.round(t, 0), 2, silent.Active.ID, "after two silent probes")

	if err := os.Remove(p.hangFile); err != nil {
		t.Fatal(err)
	}
	requireProbeMisses(t, p.round(t, 0), 0, silent.Active.ID, "after the worker answered again")

	p.silenceActiveWorker(t)
	requireProbeMisses(t, p.round(t, 0), 1, silent.Active.ID, "after the first silent probe since the answer")
}

// Misses are evidence about one worker. A deploy that switched workers in
// between must not leave the new one a miss away from being replaced.
func TestSupervisorCountsMissesPerWorker(t *testing.T) {
	p := startProbedSupervisor(t, nil)
	first := p.silenceActiveWorker(t)
	p.round(t, 0)
	requireProbeMisses(t, p.round(t, 0), 2, first.Active.ID, "after two silent probes")

	deployed := upgradeSupervisor(t, p.control)
	if deployed == first.Active.ID {
		t.Fatal("the upgrade did not switch workers")
	}
	requireProbeMisses(t, getSupervisorStatus(t, p.control), 0, deployed, "after a deploy, before the new worker's first probe,")
	p.silenceActiveWorker(t)
	requireProbeMisses(t, p.round(t, 0), 1, deployed, "after the new worker's first silent probe")
}

// A probe takes up to its timeout and runs without the upgrade lock, so a
// deploy can switch workers while the old one is being asked. The verdict on
// the old worker must not replace the new one.
func TestSupervisorDoesNotReplaceAWorkerItDidNotProbe(t *testing.T) {
	p := startProbedSupervisor(t, func(config *supervisorConfig) {
		config.WorkerProbeMisses = 1
		config.WorkerProbeTimeout = 4 * time.Second
	})
	silent := p.silenceActiveWorker(t)

	p.wait.end <- p.clock.advance(p.wait.length)
	// The probe of the silent worker is now waiting out its timeout.
	time.Sleep(300 * time.Millisecond)
	deployed := upgradeSupervisor(t, p.control)
	if deployed == silent.Active.ID {
		t.Fatal("the upgrade did not switch workers")
	}
	p.wait = p.clock.nextWait(t)

	status := getSupervisorStatus(t, p.control)
	requireProbeMisses(t, status, 0, deployed, "after a probe of the worker the deploy replaced timed out")
	if status.WorkerProbe.LastReplacement != nil {
		t.Fatalf("the probe replaced a worker: %+v", *status.WorkerProbe.LastReplacement)
	}
}

// Each of these already changes workers, or forbids it. The probe's round is
// skipped, so its misses do not pile up behind the lock and nothing is
// replaced twice.
func TestSupervisorSkipsAProbeRound(t *testing.T) {
	const rounds = 4
	skipped := func(t *testing.T, p *probedSupervisor, silent supervisorControlStatus, when string) {
		t.Helper()
		for round := 1; round <= rounds; round++ {
			requireProbeMisses(t, p.round(t, 0), 0, silent.Active.ID, fmt.Sprintf("after round %d %s", round, when))
		}
	}
	replacedAfterwards := func(t *testing.T, p *probedSupervisor, silent supervisorControlStatus) {
		t.Helper()
		p.round(t, 0)
		requireProbeMisses(t, p.round(t, 0), 2, silent.Active.ID, "two rounds later")
		requireReplaced(t, p.round(t, 0), silent, "three rounds later")
	}

	t.Run("while an upgrade holds the upgrade lock", func(t *testing.T) {
		p := startProbedSupervisor(t, nil)
		silent := p.silenceActiveWorker(t)
		p.upgradeMu.Lock()
		locked := true
		defer func() {
			if locked {
				p.upgradeMu.Unlock()
			}
		}()
		skipped(t, p, silent, "with the upgrade lock held")
		p.upgradeMu.Unlock()
		locked = false
		replacedAfterwards(t, p, silent)
	})

	t.Run("while a canary runs", func(t *testing.T) {
		p := startProbedSupervisor(t, nil)
		canary := func(path string) {
			t.Helper()
			response, err := p.control.Post("http://supervisor"+path, "application/json", nil)
			if err != nil {
				t.Fatalf("POST %s: %v", path, err)
			}
			body, _ := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("POST %s = %d: %s", path, response.StatusCode, body)
			}
		}
		p.control.Timeout = 20 * time.Second
		canary("/_subrouter/canary/start?weight=5")
		silent := p.silenceActiveWorker(t)
		skipped(t, p, silent, "of a canary")
		canary("/_subrouter/canary/abort")
		replacedAfterwards(t, p, silent)
	})

	t.Run("while the supervisor shuts down", func(t *testing.T) {
		p := startProbedSupervisor(t, nil)
		silent := p.silenceActiveWorker(t)
		p.beginShutdown()
		skipped(t, p, silent, "of a shutdown")
	})
}

// A replacement that cannot start leaves the silent worker where it is: what
// it still serves is worth more than a restart that drops every connection.
// The marker is how a deployment transaction forbids a new worker.
func TestSupervisorKeepsRunningWhenTheSilentWorkerCannotBeReplaced(t *testing.T) {
	var marker string
	p := startProbedSupervisor(t, func(config *supervisorConfig) {
		marker = filepath.Join(filepath.Dir(config.ControlSocket), "upgrade-inhibited")
		config.UpgradeInhibitFile = marker
	})
	if err := os.WriteFile(marker, []byte("transaction\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	silent := p.silenceActiveWorker(t)
	p.round(t, 0)
	p.round(t, 0)
	refused := p.round(t, 0)
	requireProbeMisses(t, refused, 3, silent.Active.ID, "after three silent probes with worker changes forbidden")
	if !strings.Contains(refused.WorkerProbe.ReplaceError, "inhibited") {
		t.Fatalf("worker_probe.replace_error = %q, want the reason the worker could not be replaced", refused.WorkerProbe.ReplaceError)
	}
	select {
	case <-p.exited:
		t.Fatalf("the supervisor exited because it could not replace a silent worker: %v", p.err)
	default:
	}

	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	replaced := p.round(t, 0)
	requireReplaced(t, replaced, silent, "one round after worker changes were allowed again")
	if replaced.WorkerProbe.ReplaceError != "" {
		t.Fatalf("worker_probe.replace_error = %q after the replacement, want none", replaced.WorkerProbe.ReplaceError)
	}
}

func TestSupervisorWorkerProbeIsOffAtIntervalZero(t *testing.T) {
	config, hangFile := probeTestConfig(t)
	config.WorkerProbeInterval = 0
	clock := newProbeTestClock()
	startTestSupervisorWith(t, config, func(s *supervisor) { s.clock = clock })
	control := unixHTTPClient(config.ControlSocket)

	silent := getSupervisorStatus(t, control)
	silenceWorker(t, hangFile, silent.Worker.PID)
	select {
	case wait := <-clock.armed:
		t.Fatalf("a supervisor with --worker-probe-interval 0 armed a probe tick of %s", wait.length)
	case <-time.After(500 * time.Millisecond):
	}
	status := getSupervisorStatus(t, control)
	if status.WorkerProbe.Enabled || status.WorkerProbe.Misses != 0 || status.Active.ID != silent.Active.ID {
		t.Fatalf("with the probe off: active %s, worker_probe %s; want %s kept and the probe reported off",
			status.Active.ID, status.WorkerProbe, silent.Active.ID)
	}
}

// The defaults live in the code, so a supervisor started from a plist written
// before the probe existed gets the probe.
func TestParseSupervisorConfigWorkerProbeDefaultsAndLimits(t *testing.T) {
	base := []string{"--control-socket", "/var/run/subrouter-test.sock", "--worker-bin", "/usr/local/bin/subrouter"}
	parse := func(extra ...string) (supervisorConfig, error) {
		t.Helper()
		config, err := parseSupervisorConfig(append(append([]string{}, base...), extra...))
		if err != nil {
			t.Fatal(err)
		}
		return config, validateSupervisorConfig(config)
	}

	config, err := parse()
	if err != nil {
		t.Fatal(err)
	}
	if config.WorkerProbeInterval != 15*time.Second || config.WorkerProbeTimeout != 10*time.Second || config.WorkerProbeMisses != 3 {
		t.Fatalf("probe defaults = every %s, timeout %s, %d misses; want 15s, 10s and 3",
			config.WorkerProbeInterval, config.WorkerProbeTimeout, config.WorkerProbeMisses)
	}

	config, err = parse("--worker-probe-interval", "40s", "--worker-probe-timeout", "4s", "--worker-probe-misses", "5")
	if err != nil {
		t.Fatal(err)
	}
	if config.WorkerProbeInterval != 40*time.Second || config.WorkerProbeTimeout != 4*time.Second || config.WorkerProbeMisses != 5 {
		t.Fatalf("probe flags were not read: %+v", config)
	}

	if config, err = parse("--worker-probe-interval", "0", "--worker-probe-timeout", "0", "--worker-probe-misses", "0"); err != nil || config.WorkerProbeInterval != 0 {
		t.Fatalf("interval 0 must turn the probe off whatever the other two say: interval %s, %v", config.WorkerProbeInterval, err)
	}
	for _, refused := range [][]string{
		{"--worker-probe-interval", "-1s"},
		{"--worker-probe-timeout", "0"},
		{"--worker-probe-misses", "0"},
	} {
		if _, err := parse(refused...); err == nil {
			t.Fatalf("%v was accepted", refused)
		}
	}
}
