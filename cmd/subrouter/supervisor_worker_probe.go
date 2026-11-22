package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// PATCHES section 77.2. A worker that exits is replaced by monitorWorker. A
// worker that stays alive and stops answering was replaced by nothing, and a
// Claude Code session with the long client wait sends its turn to such a
// worker again every 10 minutes with no end. The probe asks the active
// worker's readiness route on its private socket and, after enough misses in a
// row, replaces the worker the way an upgrade does.

// workerProbeLateTick is how late a probe tick may arrive before the misses
// counted so far say nothing. A Mac that sleeps stops every process on it, and
// the tick after it wakes arrives late by the length of the sleep.
const workerProbeLateTick = 10 * time.Second

// workerProbe is what the supervisor has seen of its active worker. Misses are
// evidence about one generation, so a new active generation starts at none.
// runWorkerProbe's goroutine is its only writer.
type workerProbe struct {
	mu           sync.Mutex
	generation   string
	misses       int
	replaceError string
	replacement  *workerReplacement
}

type workerReplacement struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
	From   string    `json:"from"`
	To     string    `json:"to"`
}

type workerProbeStatus struct {
	Enabled         bool    `json:"enabled"`
	IntervalSeconds float64 `json:"interval_seconds,omitempty"`
	TimeoutSeconds  float64 `json:"timeout_seconds,omitempty"`
	MissesToReplace int     `json:"misses_to_replace,omitempty"`
	// Misses is the number of probes in a row the active worker has not
	// answered. It reaches MissesToReplace only while a replacement cannot be
	// made, and ReplaceError then says why.
	Misses          int                `json:"misses"`
	ReplaceError    string             `json:"replace_error,omitempty"`
	LastReplacement *workerReplacement `json:"last_replacement,omitempty"`
}

func (p *workerProbe) answered(generation string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.generation, p.misses, p.replaceError = generation, 0, ""
}

func (p *workerProbe) missed(generation string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.generation != generation {
		p.generation, p.misses, p.replaceError = generation, 0, ""
	}
	p.misses++
	return p.misses
}

func (p *workerProbe) forgetMisses() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.misses, p.replaceError = 0, ""
}

func (p *workerProbe) replaceFailed(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.replaceError = err.Error()
}

func (p *workerProbe) replaced(replacement workerReplacement) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.generation, p.misses, p.replaceError = replacement.To, 0, ""
	p.replacement = &replacement
}

func (s *supervisor) workerProbeStatus() workerProbeStatus {
	if s.config.WorkerProbeInterval <= 0 {
		return workerProbeStatus{}
	}
	active := s.router.Active().ID
	s.probe.mu.Lock()
	defer s.probe.mu.Unlock()
	status := workerProbeStatus{
		Enabled:         true,
		IntervalSeconds: s.config.WorkerProbeInterval.Seconds(),
		TimeoutSeconds:  s.config.WorkerProbeTimeout.Seconds(),
		MissesToReplace: s.config.WorkerProbeMisses,
	}
	// Between a switch of workers and the next round, the count is still the
	// old worker's.
	if s.probe.generation == active {
		status.Misses, status.ReplaceError = s.probe.misses, s.probe.replaceError
	}
	if s.probe.replacement != nil {
		last := *s.probe.replacement
		status.LastReplacement = &last
	}
	return status
}

// runWorkerProbe probes the active worker every --worker-probe-interval until
// ctx ends. An interval of 0 turns the probe off.
func (s *supervisor) runWorkerProbe(ctx context.Context) {
	interval := s.config.WorkerProbeInterval
	if interval <= 0 {
		return
	}
	clock := s.clock
	if clock == nil {
		clock = realRolloutClock{}
	}
	for {
		armed := clock.Now()
		select {
		case <-ctx.Done():
			return
		case <-clock.After(interval):
		}
		// Round(0) compares wall clocks. On a Mac, Go's monotonic clock stops
		// during sleep, so two monotonic readings would not show the sleep.
		if late := clock.Now().Round(0).Sub(armed.Round(0)) - interval; late > workerProbeLateTick {
			slog.Info("subrouter worker probe tick arrived late; the misses counted before it are dropped", "late", late.Round(time.Second))
			s.probe.forgetMisses()
		}
		s.probeActiveWorker()
	}
}

func (s *supervisor) probeActiveWorker() {
	worker := s.workerToProbe()
	if worker == nil {
		return
	}
	// Any HTTP answer is an answer. An initial worker may report 503 for as
	// long as it runs and still serve every request (see generationRole).
	if _, err := readyStatus(worker, s.config.WorkerProbeTimeout); err != nil {
		misses := s.probe.missed(worker.id)
		slog.Warn("active subrouter worker did not answer its probe",
			"generation", worker.id, "misses", misses, "replace_at", s.config.WorkerProbeMisses, "error", err)
		if misses >= s.config.WorkerProbeMisses {
			s.replaceSilentWorker(worker, fmt.Sprintf("no answer to %d probes in a row: %v", misses, err))
		}
		return
	}
	s.probe.answered(worker.id)
}

// workerToProbe returns the active generation, or nil when the round is
// skipped: an upgrade, a rollout or a recovery holds upgradeMu, a canary
// runs, or the supervisor is shutting down.
func (s *supervisor) workerToProbe() *workerGeneration {
	if !s.upgradeMu.TryLock() {
		return nil
	}
	defer s.upgradeMu.Unlock()
	return s.probeTargetLocked()
}

// probeTargetLocked is the one rule for when the probe may judge the active
// generation. Call with upgradeMu.
func (s *supervisor) probeTargetLocked() *workerGeneration {
	if accepting, _ := s.lifecycleStatus(); !accepting || s.canary != nil {
		return nil
	}
	s.workersMu.Lock()
	defer s.workersMu.Unlock()
	return s.workers[s.router.Active().ID]
}

// replaceSilentWorker starts a new generation and retires the silent one as an
// upgrade would: what it still sends is left to finish, and it is stopped once
// it holds no connection. A replacement that cannot be made leaves the silent
// worker in place and is tried again on the next miss.
func (s *supervisor) replaceSilentWorker(silent *workerGeneration, reason string) {
	if !s.upgradeMu.TryLock() {
		return
	}
	defer s.upgradeMu.Unlock()
	// The probe ran without the lock, so a deploy or a recovery may have
	// switched workers meanwhile. The verdict was on the old one.
	if s.probeTargetLocked() != silent {
		return
	}
	slog.Error("active subrouter worker stopped answering; replacing it",
		"generation", silent.id, "pid", silent.command.Process.Pid, "reason", reason)
	if err := s.upgradeLocked(); err != nil {
		s.probe.replaceFailed(err)
		slog.Error("silent subrouter worker could not be replaced", "generation", silent.id, "error", err)
		return
	}
	s.probe.replaced(workerReplacement{At: s.now().UTC(), Reason: reason, From: silent.id, To: s.router.Active().ID})
}
