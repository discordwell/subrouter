package proxy

import (
	"context"
	"log/slog"
	"sort"
	"time"
)

// admissionMinute is one bucket's activity since its last minute line.
type admissionMinute struct {
	peakActive int
	peakQueued int
	// peakDemand is the most requests the bucket held at once, running and
	// waiting: the limit that would have started them all on arrival.
	peakDemand int
	served     int
	// waited counts the grants that queued first, and waitTotal and waitMax
	// how long they queued.
	waited    int
	waitTotal time.Duration
	waitMax   time.Duration
	spillsOut int
	spillsIn  int
	queueFull int
	// pauses counts the pauses that began, not the 429s behind them.
	pauses int
	// abandoned counts the requests whose clients gave up while queued.
	abandoned int
}

// admissionMinuteSessions is how many sessions each minute lists.
const admissionMinuteSessions = 5

type admissionMinuteBucket struct {
	key                   admissionKey
	limit, effectiveLimit int
	active, queued        int
	admissionMinute
}

type admissionMinuteSession struct {
	who                  admissionSessionKey
	peak, active, queued int
	account              string
}

// admissionMinuteReport is one minute of the gate: every bucket busy in it,
// and the sessions that fanned out furthest.
type admissionMinuteReport struct {
	buckets  []admissionMinuteBucket
	sessions []admissionMinuteSession
}

// rollMinute reports the minute ending at now and starts the next, whose
// peaks begin at what each bucket and session holds now.
func (a *admissionControl) rollMinute(now time.Time) admissionMinuteReport {
	if a == nil {
		return admissionMinuteReport{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var report admissionMinuteReport
	for key, bucket := range a.buckets {
		minute := bucket.minute
		bucket.minute = admissionMinute{}
		bucket.noteLoad()
		if minute == (admissionMinute{}) {
			continue
		}
		effective, _ := a.admissionLimitLocked(bucket, now)
		report.buckets = append(report.buckets, admissionMinuteBucket{
			key: key, limit: bucket.limit, effectiveLimit: effective,
			active: bucket.active, queued: len(bucket.waiters), admissionMinute: minute,
		})
	}
	sort.Slice(report.buckets, func(i, j int) bool {
		left, right := report.buckets[i].key, report.buckets[j].key
		if left.account != right.account {
			return left.account < right.account
		}
		if left.model != right.model {
			return left.model < right.model
		}
		return left.provider < right.provider
	})
	for who, entry := range a.sessions {
		peak := entry.minutePeak
		entry.minutePeak = 0
		entry.noteLoad()
		if peak == 0 {
			continue
		}
		report.sessions = append(report.sessions, admissionMinuteSession{
			who: who, peak: peak, active: entry.active, queued: entry.queued, account: entry.account,
		})
	}
	sort.Slice(report.sessions, func(i, j int) bool {
		left, right := report.sessions[i], report.sessions[j]
		if left.peak != right.peak {
			return left.peak > right.peak
		}
		return left.who.session < right.who.session
	})
	if len(report.sessions) > admissionMinuteSessions {
		report.sessions = report.sessions[:admissionMinuteSessions]
	}
	return report
}

// logMinute writes the minute ending at now: one line per busy bucket, then
// one per top session. The daemon log keeps them across restarts, so they
// answer what a higher limit would have changed.
func (a *admissionControl) logMinute(logger *slog.Logger, now time.Time) {
	report := a.rollMinute(now)
	for _, bucket := range report.buckets {
		var waitAvg time.Duration
		if bucket.waited > 0 {
			waitAvg = bucket.waitTotal / time.Duration(bucket.waited)
		}
		logger.Info("admission minute",
			"provider", bucket.key.provider,
			"account", bucket.key.account,
			"model", bucket.key.model,
			"limit", bucket.limit,
			"effective_limit", bucket.effectiveLimit,
			"active", bucket.active,
			"queued", bucket.queued,
			"peak_active", bucket.peakActive,
			"peak_queued", bucket.peakQueued,
			"peak_demand", bucket.peakDemand,
			"served", bucket.served,
			"waited", bucket.waited,
			"wait_avg_ms", waitAvg.Milliseconds(),
			"wait_max_ms", bucket.waitMax.Milliseconds(),
			"spills_out", bucket.spillsOut,
			"spills_in", bucket.spillsIn,
			"queue_full", bucket.queueFull,
			"pauses", bucket.pauses,
			"abandoned", bucket.abandoned)
	}
	for _, session := range report.sessions {
		logger.Info("admission minute session",
			"agent", session.who.agent,
			"session_prefix", admissionSessionPrefix(session.who.session),
			"peak", session.peak,
			"active", session.active,
			"queued", session.queued,
			"account", session.account)
	}
}

// runAdmission runs the gate's clocks until ctx ends: a tuning step every
// admissionTuneStep and a minute line every minute.
func runAdmission(ctx context.Context, control *admissionControl, logger *slog.Logger) {
	tune := time.NewTicker(admissionTuneStep)
	defer tune.Stop()
	minute := time.NewTicker(time.Minute)
	defer minute.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		// time.Now rather than the tick's time: a request that queued
		// after the tick would otherwise record a negative wait.
		case <-tune.C:
			control.tune(time.Now())
		case <-minute.C:
			control.logMinute(logger, time.Now())
		}
	}
}
