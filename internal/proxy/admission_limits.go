package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"time"
)

// admissionLimits sizes the admission gate. One value is swapped whole under
// admissionControl.mu, so every path sees one consistent set.
type admissionLimits struct {
	// Limit is how many requests one (provider, account, model) bucket runs
	// at once.
	Limit int `json:"limit"`
	// Queue is how many more may wait in a bucket before new ones are
	// refused (admission_queue_full).
	Queue int `json:"queue"`
	// SpillQueue is how many may wait before a new request spills to an
	// account with a free slot; 0 never spills. A short queue keeps a
	// session's prompt cache; a long one only adds latency.
	SpillQueue int `json:"spill_queue"`
	// ClosedQueue is how many waiting requests close a bucket (closed), for
	// placement to refuse its account new sessions; 0 never closes one.
	ClosedQueue int `json:"closed_queue"`
	// PerModel overrides Limit for a model's buckets, keyed by pool model.
	PerModel map[string]int `json:"per_model,omitempty"`
	// PerAccount overrides Limit and PerModel for every bucket of an
	// account, keyed by account id.
	PerAccount map[string]int `json:"per_account,omitempty"`
	// Tune moves each Claude bucket's limit from its configured value
	// (admission_tune.go).
	Tune admissionTune `json:"tune"`
}

// The limits with no admission.json.
const (
	defaultAdmissionLimit      = 6
	defaultAdmissionQueue      = 64
	defaultAdmissionSpillQueue = 8
)

func defaultAdmissionLimits() admissionLimits {
	return admissionLimits{
		Limit:       defaultAdmissionLimit,
		Queue:       defaultAdmissionQueue,
		SpillQueue:  defaultAdmissionSpillQueue,
		ClosedQueue: defaultAdmissionQueue / 4,
		Tune:        admissionTune{On: true, Min: 4, Max: 24},
	}
}

// limitFor is the configured limit of key's bucket.
func (l admissionLimits) limitFor(key admissionKey) int {
	if limit, ok := l.PerAccount[key.account]; ok {
		return limit
	}
	if limit, ok := l.PerModel[key.model]; ok {
		return limit
	}
	return l.Limit
}

func (l admissionLimits) String() string {
	raw, _ := json.Marshal(l)
	return string(raw)
}

// admissionLimitsJSON is admission.json as written: an absent field keeps
// its default.
type admissionLimitsJSON struct {
	Limit       *int           `json:"limit"`
	Queue       *int           `json:"queue"`
	SpillQueue  *int           `json:"spill_queue"`
	ClosedQueue *int           `json:"closed_queue"`
	PerModel    map[string]int `json:"per_model"`
	PerAccount  map[string]int `json:"per_account"`
	Tune        *struct {
		On  *bool `json:"on"`
		Min *int  `json:"min"`
		Max *int  `json:"max"`
	} `json:"tune"`
}

// parseAdmissionLimits reads admission.json. An absent field keeps its
// default, and an absent closed_queue is a quarter of the queue.
func parseAdmissionLimits(data []byte) (admissionLimits, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var raw admissionLimitsJSON
	if err := decoder.Decode(&raw); err != nil {
		return admissionLimits{}, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return admissionLimits{}, errors.New("text after the JSON object")
	}
	limits := defaultAdmissionLimits()
	for _, field := range []struct {
		name  string
		value *int
		min   int
		into  *int
	}{
		{"limit", raw.Limit, 1, &limits.Limit},
		{"queue", raw.Queue, 0, &limits.Queue},
		{"spill_queue", raw.SpillQueue, 0, &limits.SpillQueue},
		{"closed_queue", raw.ClosedQueue, 0, &limits.ClosedQueue},
	} {
		if field.value == nil {
			continue
		}
		if *field.value < field.min {
			return admissionLimits{}, fmt.Errorf("%s is %d, want at least %d", field.name, *field.value, field.min)
		}
		*field.into = *field.value
	}
	if raw.ClosedQueue == nil {
		limits.ClosedQueue = limits.Queue / 4
	}
	if tune := raw.Tune; tune != nil {
		if tune.On != nil {
			limits.Tune.On = *tune.On
		}
		if tune.Min != nil {
			limits.Tune.Min = *tune.Min
		}
		if tune.Max != nil {
			limits.Tune.Max = *tune.Max
		}
	}
	if limits.Tune.Min < 1 || limits.Tune.Max < limits.Tune.Min {
		return admissionLimits{}, fmt.Errorf("tune min %d and max %d, want 1 <= min <= max", limits.Tune.Min, limits.Tune.Max)
	}
	var err error
	if limits.PerModel, err = parseAdmissionOverrides("per_model", raw.PerModel, claudePoolModel); err != nil {
		return admissionLimits{}, err
	}
	if limits.PerAccount, err = parseAdmissionOverrides("per_account", raw.PerAccount, func(id string) string { return id }); err != nil {
		return admissionLimits{}, err
	}
	return limits, nil
}

// parseAdmissionOverrides trims and names each key of a per_model or
// per_account table; each limit must be at least 1.
func parseAdmissionOverrides(field string, raw map[string]int, name func(string) string) (map[string]int, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]int, len(raw))
	for key, limit := range raw {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" {
			return nil, fmt.Errorf("%s has an empty name", field)
		}
		if limit < 1 {
			return nil, fmt.Errorf("%s %q is %d, want at least 1", field, trimmed, limit)
		}
		named := name(trimmed)
		if _, dup := out[named]; dup {
			return nil, fmt.Errorf("%s names %q twice", field, named)
		}
		out[named] = limit
	}
	return out, nil
}

// setLimits puts next in force and returns the limits it replaced. A bucket
// whose configured limit changed starts again from it; one still tuning keeps
// its tuned limit inside the new bounds. A raise
// grants queued requests in FIFO order at once; a lower limit only stops new
// grants until the bucket's active count falls under it. Nothing already
// queued is dropped or reordered, even past a lowered queue.
func (a *admissionControl) setLimits(next admissionLimits) admissionLimits {
	a.mu.Lock()
	defer a.mu.Unlock()
	previous := a.limits
	a.limits = next
	now := time.Now()
	for key, bucket := range a.buckets {
		if limit := next.retuned(previous, key, bucket.limit); limit != bucket.limit {
			bucket.limit = limit
			// Waiters behind a recovery ramp that had reached the old limit
			// armed no timer; wake them to arm one for the new limit.
			close(bucket.changed)
			bucket.changed = make(chan struct{})
		}
		a.wakeLocked(key, bucket, now)
	}
	return previous
}

// closed reports whether key's bucket has so many requests waiting (the
// closed queue) that its account should take no new session.
func (a *admissionControl) closed(key admissionKey) bool {
	if a == nil || key.account == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closedLocked(a.buckets[key])
}

func (a *admissionControl) closedLocked(bucket *admissionBucket) bool {
	return bucket != nil && a.limits.ClosedQueue > 0 && len(bucket.waiters) >= a.limits.ClosedQueue
}

// admissionLimitsPoll is how often admission.json is re-read.
const admissionLimitsPoll = 2 * time.Second

// admissionLimitsFile keeps a gate's limits in step with a JSON file. It
// compares the file's content, not its mtime, so a rewrite within the
// mtime's resolution still lands.
type admissionLimitsFile struct {
	path    string
	control *admissionControl
	logger  *slog.Logger
	// seen is the last state read: absent, a read error, or the content.
	// The file is acted on, and logged, only when it changes.
	seen string
}

func newAdmissionLimitsFile(path string, control *admissionControl, logger *slog.Logger) *admissionLimitsFile {
	if logger == nil {
		logger = slog.Default()
	}
	return &admissionLimitsFile{path: path, control: control, logger: logger}
}

// poll reads the file once. An absent file is today's defaults; an
// unreadable or invalid one keeps the limits in force and logs an error.
func (f *admissionLimitsFile) poll() {
	data, err := os.ReadFile(f.path)
	seen := "file " + string(data)
	if errors.Is(err, fs.ErrNotExist) {
		seen = "absent"
	} else if err != nil {
		seen = "error " + err.Error()
	}
	if seen == f.seen {
		return
	}
	f.seen = seen
	next := defaultAdmissionLimits()
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		f.logger.Error("admission limits file unreadable; keeping the limits in force", "path", f.path, "error", err)
		return
	default:
		if next, err = parseAdmissionLimits(data); err != nil {
			f.logger.Error("admission limits file invalid; keeping the limits in force", "path", f.path, "error", err)
			return
		}
	}
	if previous := f.control.setLimits(next); previous.String() != next.String() {
		f.logger.Info("admission limits changed", "path", f.path, "old", previous.String(), "new", next.String())
	}
}

func watchAdmissionLimits(ctx context.Context, file *admissionLimitsFile, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			file.poll()
		}
	}
}

// StartAdmission puts the admission limits at path in force, then, while
// ctx lives, re-reads them, so a change takes effect with no restart, tunes
// the Claude buckets and logs the gate's minute lines.
func (r *AccountRef) StartAdmission(ctx context.Context, path string, logger *slog.Logger) {
	if r == nil {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	control := r.sharedAdmission()
	control.setLogger(logger)
	control.setLimits(defaultAdmissionLimits())
	file := newAdmissionLimitsFile(path, control, logger)
	file.poll()
	go watchAdmissionLimits(ctx, file, admissionLimitsPoll)
	go runAdmission(ctx, control, logger)
}
