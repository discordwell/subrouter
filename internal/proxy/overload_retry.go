package proxy

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
)

// Same-account overload retry: the Codex half of upstream manaflow-ai/subrouter's
// overload_retry.go (#389, fb575d1; as at c9303d3), ported for the capacity
// stay ladder (PATCHES section 27, the 389/373 port).
//
// Overload should be rare and brief. The session's prompt cache lives on its
// account, so a Codex capacity failure the failover does not move -- every
// one with the failover off, and with it on a conversation above the size cap
// -- waits it out there: a short ramp of growing gaps, then a steady gap,
// until a wall-clock cap measured from the first attempt. The cap keeps the
// client answered with a clean capacity error before its own stream idle
// timeout, rather than a timeout.
//
// Not ported, deliberately: upstream's Claude ladder (8m, the request-wide
// claudeOverloadHold, the opt-in Claude reroute) is outside the capacity
// port; and the X-Subrouter-Retry header, which lets a client stretch the
// ladder to 60 minutes (OverloadRetryMaxWaitCap) wherever the failover is on
// -- with the failover shipped on here, that would put every size-kept
// conversation's hold in the client's hands. The operator's
// SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT / _INTERVAL are the only knobs.
const (
	// Codex: 0.5s doubling to 8s, then every ~9s (8-10s with jitter) for up
	// to 4m, inside Codex CLI's 5-minute stream idle timeout.
	codexCapacityDefaultStayInterval = 9 * time.Second
	codexCapacityDefaultStayMaxWait  = 4 * time.Minute

	// overloadRetryLogEvery is how often a long same-account wait is logged
	// after its first retry.
	overloadRetryLogEvery = time.Minute
)

// OverloadRetryMinInterval is the floor for a configured steady gap
// (SUBROUTER_CODEX_CAPACITY_RETRY_INTERVAL).
const OverloadRetryMinInterval = 500 * time.Millisecond

// overloadRetryPolicy is one request's same-account overload ladder: the
// steady gap after the ramp (the ramp is capped at it) and the wall-clock cap.
// Zero values mean the provider default; unbounded means no cap, so only the
// client's disconnect ends the wait.
type overloadRetryPolicy struct {
	interval  time.Duration
	maxWait   time.Duration
	unbounded bool
}

func (p overloadRetryPolicy) intervalOr(fallback time.Duration) time.Duration {
	if p.interval > 0 {
		return p.interval
	}
	return fallback
}

func (p overloadRetryPolicy) maxWaitOr(fallback time.Duration) time.Duration {
	if p.maxWait > 0 {
		return p.maxWait
	}
	return fallback
}

// overloadHeldGauge counts requests currently waiting out an overload on
// their own account, for /_subrouter/health. Upstream counts Claude and Codex;
// the fork ports only the Codex ladder, so only Codex is counted (a Claude
// counter would always read 0 and suggest a wait that does not exist here).
type overloadHeldGauge struct {
	codex atomic.Int64
}

func newOverloadHeldGauge() *overloadHeldGauge { return &overloadHeldGauge{} }

func (g *overloadHeldGauge) counter(provider accounts.Provider) *atomic.Int64 {
	if g == nil {
		return nil
	}
	if provider == accounts.ProviderCodex {
		return &g.codex
	}
	return nil
}

// enter counts one held request and returns its idempotent release.
func (g *overloadHeldGauge) enter(provider accounts.Provider) func() {
	counter := g.counter(provider)
	if counter == nil {
		return func() {}
	}
	counter.Add(1)
	var once sync.Once
	return func() { once.Do(func() { counter.Add(-1) }) }
}

func (g *overloadHeldGauge) snapshot() map[string]int64 {
	if g == nil {
		return nil
	}
	return map[string]int64{
		string(accounts.ProviderCodex): g.codex.Load(),
	}
}

// enterOverloadHold counts a request as held in same-account overload retry.
// The rolling recovery counter is upstream #490's (e6ade663); only the Codex
// stay ladder calls this here.
func (s *Server) enterOverloadHold(provider accounts.Provider) func() {
	if s == nil {
		return func() {}
	}
	if s.recoveryCounters != nil {
		s.recoveryCounters.add(provider, "held", time.Now())
	}
	return s.overloadHeld.enter(provider)
}

// overloadRetryLog rate-limits the retry log of one request: its first
// retry, then about once a minute.
type overloadRetryLog struct {
	last time.Time
}

func (l *overloadRetryLog) due(now time.Time) bool {
	if l.last.IsZero() || now.Sub(l.last) >= overloadRetryLogEvery {
		l.last = now
		return true
	}
	return false
}
