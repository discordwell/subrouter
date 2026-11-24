package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	agentclaude "github.com/discordwell/subrouter/internal/agents/claude"
)

// The usage history (PATCHES §92.1): a durable series of this daemon's own
// meter readings, and a record of every reset claim it makes, beside
// token-usage.jsonl in the state directory. Nothing ranks or routes on it.

// UsageHistoryFile is the history's file name in the state directory.
const UsageHistoryFile = "usage-history.jsonl"

const (
	usageHistoryRetention     = 35 * 24 * time.Hour
	usageHistoryHeartbeat     = 10 * time.Minute
	usageHistoryFlushInterval = 2 * time.Minute
	usageHistoryCompactEvery  = time.Hour
	usageHistoryMaxBytes      = 32 << 20
	// usageHistoryClaimWait is how long a landed claim waits for the first
	// reading after it before it is written without one.
	usageHistoryClaimWait = 30 * time.Minute
	// usageHistoryMaxUnflushed bounds what a state directory that cannot be
	// written makes the daemon hold for it.
	usageHistoryMaxUnflushed = 50000
	// A reading whose used fraction moves less than usageHistorySameUsed and
	// whose roll moves less than usageHistorySameRoll is the last point again.
	usageHistorySameUsed = 0.0001
	usageHistorySameRoll = time.Minute
	// usageSegmentRollMove and usageSegmentFall start a new segment: the roll
	// moved further than the one, or the used fraction fell by more than the
	// other.
	usageSegmentRollMove = 5 * time.Minute
	usageSegmentFall     = 0.001
	codexWeekSeconds     = 7 * 24 * 3600
)

// The meters the series records.
const (
	// usageMeterWeek is Claude's 7d window and Codex's account-wide 7-day one.
	usageMeterWeek = "week"
	// usageMeterFable is Claude's Fable meter.
	usageMeterFable = "fable"
	// usageMeterPoolPrefix, followed by the pool's feature, is a Codex model
	// pool's own week.
	usageMeterPoolPrefix = "week:"
)

// How a segment began.
const (
	usageSegmentRoll  = "roll"
	usageSegmentClaim = "claim"
	usageSegmentDrop  = "drop"
)

// usageMeterReading is one meter as one reading gave it, on the wire: inside
// a point line, and as a claim's before and after.
type usageMeterReading struct {
	Meter string `json:"m,omitempty"`
	// At is when the reading was taken (when the provider was asked), in
	// Unix microseconds.
	At   int64   `json:"t,omitempty"`
	Used float64 `json:"u,omitempty"`
	// Roll is when the meter's window rolls, in Unix seconds, as the reading
	// gave it; 0 when it gave none.
	Roll int64 `json:"r,omitempty"`
	// Unstarted is a week whose clock has not started: its seconds to reset
	// are the whole window, so Roll slides with every reading (PATCHES §86).
	Unstarted bool `json:"n,omitempty"`
}

// usageHistoryLine is one line of the file: a point or a claim.
type usageHistoryLine struct {
	Provider string `json:"p,omitempty"`
	Account  string `json:"a,omitempty"`
	usageMeterReading
	Claim *usageClaim `json:"claim,omitempty"`
}

// usageClaim is one reset claim this daemon made.
type usageClaim struct {
	// At is when the claim was sent, in Unix microseconds.
	At       int64  `json:"t"`
	Provider string `json:"p"`
	Account  string `json:"a"`
	// Kind is what was claimed: a Claude grant's kind (week, fable, other;
	// PATCHES §64), a Codex credit's reset_type.
	Kind string `json:"kind,omitempty"`
	// Result is the provider's answer, or the error that stood for one.
	Result string `json:"result,omitempty"`
	// Failed is a claim that did not reset the account: it has no After.
	Failed bool                `json:"failed,omitempty"`
	Before []usageMeterReading `json:"before,omitempty"`
	// After is the first reading taken after the claim, empty when none
	// came within usageHistoryClaimWait or before the daemon stopped.
	After []usageMeterReading `json:"after,omitempty"`
}

type usageSeriesKey struct{ provider, account, meter string }

type usageSeries struct {
	// points are the written readings, oldest first.
	points []usageMeterReading
	// latest is the newest reading seen, written or not.
	latest usageMeterReading
}

// UsageHistory is the series and the claim records. A nil history records
// nothing: SUBROUTER_USAGE_HISTORY=0.
type UsageHistory struct {
	path     string
	logger   *slog.Logger
	now      func() time.Time
	maxBytes int64

	mu     sync.Mutex
	series map[usageSeriesKey]*usageSeries
	claims []usageClaim
	// pending are landed claims waiting for their first reading after.
	pending     []usageClaim
	unflushed   []usageHistoryLine
	writeFailed bool
	lastCompact time.Time

	fileMu    sync.Mutex
	flushLoop sync.Once
}

type usageHistoryOption func(*UsageHistory)

func withUsageHistoryClock(now func() time.Time) usageHistoryOption {
	return func(h *UsageHistory) { h.now = now }
}

func withUsageHistoryMaxBytes(limit int64) usageHistoryOption {
	return func(h *UsageHistory) { h.maxBytes = limit }
}

// NewUsageHistory loads the history at path, dropping what is past its 35
// days or its size bound. A file that cannot be read or written logs a Warn
// and the history keeps going in memory.
func NewUsageHistory(path string, logger *slog.Logger) *UsageHistory {
	return newUsageHistory(path, logger)
}

func newUsageHistory(path string, logger *slog.Logger, options ...usageHistoryOption) *UsageHistory {
	h := &UsageHistory{
		path: strings.TrimSpace(path), logger: logger, now: time.Now, maxBytes: usageHistoryMaxBytes,
		series: map[usageSeriesKey]*usageSeries{},
	}
	if h.logger == nil {
		h.logger = slog.Default()
	}
	for _, option := range options {
		option(h)
	}
	if h.path == "" {
		return h
	}
	lines, err := h.compact()
	h.mu.Lock()
	for _, line := range lines {
		if line.Claim != nil {
			h.claims = append(h.claims, *line.Claim)
			continue
		}
		series := h.seriesLocked(usageSeriesKey{line.Provider, line.Account, line.Meter})
		series.points = append(series.points, line.usageMeterReading)
		series.latest = line.usageMeterReading
	}
	h.mu.Unlock()
	h.noteWrite(err)
	return h
}

// SetUsageHistory attaches the history the account reads and claims feed.
// Called once, before the daemon serves.
func (r *AccountRef) SetUsageHistory(h *UsageHistory) {
	if r != nil {
		r.usageHistory = h
	}
}

func (r *AccountRef) history() *UsageHistory {
	if r == nil {
		return nil
	}
	return r.usageHistory
}

func (h *UsageHistory) clock() time.Time { return h.now() }

func (h *UsageHistory) seriesLocked(key usageSeriesKey) *usageSeries {
	series := h.series[key]
	if series == nil {
		series = &usageSeries{}
		h.series[key] = series
	}
	return series
}

// usageHistoryProvider is the history's name for account's provider, "" for
// a provider whose meters it does not record.
func usageHistoryProvider(account accounts.Account) string {
	switch provider := accountProviderOrCodex(account); provider {
	case accounts.ProviderClaude, accounts.ProviderCodex:
		return string(provider)
	}
	return ""
}

// usageMeterReadings picks the recorded meters out of one reading's windows:
// Claude's week and Fable meter, Codex's week and each model pool's own week.
func usageMeterReadings(provider string, windows []accounts.UsageWindow, at time.Time) []usageMeterReading {
	var out []usageMeterReading
	seen := map[string]bool{}
	for _, window := range windows {
		if window.ExtraUsage != nil || accounts.IsSpendWindow(window) {
			continue
		}
		meter := ""
		switch {
		case provider == string(accounts.ProviderClaude) && window.Name == "7d":
			meter = usageMeterWeek
		case provider == string(accounts.ProviderClaude) && window.Name == agentclaude.FableWindowName:
			meter = usageMeterFable
		case provider == string(accounts.ProviderCodex) && window.LimitWindowSeconds == codexWeekSeconds && window.Feature == "":
			meter = usageMeterWeek
		case provider == string(accounts.ProviderCodex) && window.LimitWindowSeconds == codexWeekSeconds:
			meter = usageMeterPoolPrefix + window.Feature
		}
		if meter == "" || seen[meter] || math.IsNaN(window.UsedPercent) || math.IsInf(window.UsedPercent, 0) {
			continue
		}
		seen[meter] = true
		reading := usageMeterReading{Meter: meter, At: at.UnixMicro(), Used: window.UsedPercent / 100,
			Unstarted: window.LimitWindowSeconds > 0 && window.ResetAfterSeconds >= window.LimitWindowSeconds}
		if roll := window.ResetTime(at); !roll.IsZero() {
			reading.Roll = roll.Unix()
		}
		out = append(out, reading)
	}
	return out
}

// differs reports whether reading is worth a point after last.
func (reading usageMeterReading) differs(last usageMeterReading) bool {
	switch {
	case math.Abs(reading.Used-last.Used) > usageHistorySameUsed, reading.Unstarted != last.Unstarted,
		(reading.Roll == 0) != (last.Roll == 0):
		return true
	case reading.Unstarted:
		return false
	}
	return time.Duration(absInt64(reading.Roll-last.Roll))*time.Second > usageHistorySameRoll
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// observe records one usage reading of this daemon's own account, taken at
// at. A reading served again from a cache, or one older than the account's
// newest, adds nothing. A point is written when the reading differs from the
// meter's last point, or the last point is usageHistoryHeartbeat old.
func (h *UsageHistory) observe(account accounts.Account, windows []accounts.UsageWindow, at time.Time) {
	if h == nil || at.IsZero() {
		return
	}
	provider := usageHistoryProvider(account)
	if provider == "" || account.ID == "" {
		return
	}
	readings := usageMeterReadings(provider, windows, at)
	if len(readings) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var fresh []usageMeterReading
	for _, reading := range readings {
		series := h.seriesLocked(usageSeriesKey{provider, account.ID, reading.Meter})
		if reading.At <= series.latest.At {
			continue
		}
		series.latest = reading
		fresh = append(fresh, reading)
		if n := len(series.points); n > 0 && !reading.differs(series.points[n-1]) &&
			time.Duration(reading.At-series.points[n-1].At)*time.Microsecond < usageHistoryHeartbeat {
			continue
		}
		series.points = append(series.points, reading)
		h.queueLocked(usageHistoryLine{Provider: provider, Account: account.ID, usageMeterReading: reading})
	}
	if len(fresh) == 0 {
		return
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].Meter < fresh[j].Meter })
	kept := h.pending[:0]
	for _, claim := range h.pending {
		if claim.Provider == provider && claim.Account == account.ID && claim.At <= fresh[0].At {
			claim.After = fresh
			h.recordClaimLocked(claim)
			continue
		}
		kept = append(kept, claim)
	}
	h.pending = kept
}

func (h *UsageHistory) queueLocked(line usageHistoryLine) {
	if h.path == "" {
		return
	}
	h.unflushed = append(h.unflushed, line)
	if over := len(h.unflushed) - usageHistoryMaxUnflushed; over > 0 {
		h.unflushed = append(h.unflushed[:0:0], h.unflushed[over:]...)
	}
}

// usageClaimDraft is a claim about to be sent, with what each of the
// account's meters read last before it.
type usageClaimDraft struct{ claim usageClaim }

// startClaim is called just before a claim on account is sent.
func (h *UsageHistory) startClaim(account accounts.Account, kind string, at time.Time) *usageClaimDraft {
	if h == nil {
		return nil
	}
	provider := usageHistoryProvider(account)
	if provider == "" {
		return nil
	}
	claim := usageClaim{At: at.UnixMicro(), Provider: provider, Account: account.ID, Kind: kind}
	h.mu.Lock()
	for key, series := range h.series {
		if key.provider == provider && key.account == account.ID && series.latest.At > 0 && series.latest.At <= claim.At {
			claim.Before = append(claim.Before, series.latest)
		}
	}
	h.mu.Unlock()
	sort.Slice(claim.Before, func(i, j int) bool { return claim.Before[i].Meter < claim.Before[j].Meter })
	return &usageClaimDraft{claim: claim}
}

// finishClaim records the claim's answer. A claim that reset the account
// waits for the first reading after it; one that did not is written now.
func (h *UsageHistory) finishClaim(draft *usageClaimDraft, result string, landed bool) {
	if h == nil || draft == nil {
		return
	}
	claim := draft.claim
	claim.Result, claim.Failed = result, !landed
	h.mu.Lock()
	defer h.mu.Unlock()
	if landed {
		h.pending = append(h.pending, claim)
		return
	}
	h.recordClaimLocked(claim)
}

func (h *UsageHistory) recordClaimLocked(claim usageClaim) {
	h.claims = append(h.claims, claim)
	h.queueLocked(usageHistoryLine{Claim: &claim})
	h.logger.Info("reset claim recorded", usageClaimLogArgs(claim)...)
	if table, showed, contradicts := claimAgainstTable(claim); contradicts {
		h.logger.Warn(claimContradictsTableMessage, "account", claim.Account, "provider", claim.Provider, "kind", claim.Kind,
			"table", table, "claim", showed, "claimed_at", time.UnixMicro(claim.At).UTC().Format(time.RFC3339Nano))
	}
}

const claimContradictsTableMessage = "reset claim contradicts the kinds table; the table is unchanged"

// claimTableMeters are the history's meters for the names a kind's GivesBack
// uses.
var claimTableMeters = map[string]string{"seven_day": usageMeterWeek, "seven_day_overage_included": usageMeterFable, "weekly": usageMeterWeek}

// claimAgainstTable compares a landed claim's record with its kind's entry in
// the kinds table (§94.2): whether the roll of the kind's meter moved by more
// than usageSegmentRollMove, against restarts_clock, and whether each meter
// the kind gives back fell. It returns what the table says and what the
// claim showed when they disagree. A failed claim, one with no reading after
// it, a kind the table does not know, and a meter not read on both sides
// settle nothing. The table is never changed here.
func claimAgainstTable(claim usageClaim) (table, showed string, contradicts bool) {
	spec, known := accounts.ResetKindNamed(accounts.Provider(claim.Provider), accounts.ResetKind(claim.Kind))
	if claim.Failed || len(claim.After) == 0 || !known {
		return "", "", false
	}
	reading := func(readings []usageMeterReading, meter string) (usageMeterReading, bool) {
		for _, r := range readings {
			if r.Meter == meter {
				return r, true
			}
		}
		return usageMeterReading{}, false
	}
	var says, saw []string
	meter := string(spec.Meter)
	before, hadBefore := reading(claim.Before, meter)
	after, hadAfter := reading(claim.After, meter)
	// A roll already past at the claim is the week's own roll, not the claim's.
	if restarts, ok := spec.RestartsClock.Restarts(); ok && hadBefore && hadAfter && before.Roll != 0 && after.Roll != 0 &&
		!before.Unstarted && time.Unix(before.Roll, 0).After(time.UnixMicro(claim.At)) {
		moved := time.Duration(absInt64(after.Roll-before.Roll))*time.Second > usageSegmentRollMove
		if moved != restarts {
			says = append(says, fmt.Sprintf("restarts_clock %s", spec.RestartsClock))
			roll := fmt.Sprintf("the %s roll stayed at %s", meter, time.Unix(before.Roll, 0).UTC().Format(time.RFC3339))
			if moved {
				roll = fmt.Sprintf("the %s roll moved from %s to %s", meter, time.Unix(before.Roll, 0).UTC().Format(time.RFC3339),
					time.Unix(after.Roll, 0).UTC().Format(time.RFC3339))
			}
			saw = append(saw, roll)
		}
	}
	for _, name := range spec.GivesBack {
		given, ok := claimTableMeters[name]
		if !ok {
			continue
		}
		before, hadBefore := reading(claim.Before, given)
		after, hadAfter := reading(claim.After, given)
		if !hadBefore || !hadAfter || before.Used <= usageSegmentFall || after.Used < before.Used-usageSegmentFall {
			continue
		}
		says = append(says, fmt.Sprintf("gives back %s (%s)", given, name))
		saw = append(saw, fmt.Sprintf("the %s did not fall: %.3f used before, %.3f after", given, before.Used, after.Used))
	}
	if len(says) == 0 {
		return "", "", false
	}
	return strings.Join(says, "; "), strings.Join(saw, "; "), true
}

func usageClaimLogArgs(claim usageClaim) []any {
	meters := func(readings []usageMeterReading) []any {
		out := make([]any, 0, len(readings))
		for _, reading := range readings {
			fields := []any{"used", reading.Used, "read_at", time.UnixMicro(reading.At).UTC().Format(time.RFC3339Nano)}
			if reading.Roll != 0 {
				fields = append(fields, "roll", time.Unix(reading.Roll, 0).UTC().Format(time.RFC3339))
			}
			if reading.Unstarted {
				fields = append(fields, "unstarted", true)
			}
			out = append(out, slog.Group(reading.Meter, fields...))
		}
		return out
	}
	return []any{
		"account", claim.Account, "provider", claim.Provider, "kind", claim.Kind, "result", claim.Result,
		"failed", claim.Failed, "claimed_at", time.UnixMicro(claim.At).UTC().Format(time.RFC3339Nano),
		slog.Group("before", meters(claim.Before)...), slog.Group("after", meters(claim.After)...),
	}
}

// RunFlushLoop writes the history every usageHistoryFlushInterval until ctx
// ends. It writes nothing when ctx ends: the daemon's stop calls Close once,
// after its drain, so the last write happens before the process exits.
func (h *UsageHistory) RunFlushLoop(ctx context.Context) {
	if h == nil || h.path == "" {
		return
	}
	h.flushLoop.Do(func() {
		go func() {
			ticker := time.NewTicker(usageHistoryFlushInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					_ = h.Flush()
				}
			}
		}()
	})
}

// Flush appends what is not yet written, writes the claims that waited
// usageHistoryClaimWait without a reading after them, and compacts the file
// once an hour or when it passes its size bound.
func (h *UsageHistory) Flush() error { return h.flush(false) }

// Close is the last Flush: every claim still waiting is written without a
// reading after it.
func (h *UsageHistory) Close() error { return h.flush(true) }

func (h *UsageHistory) flush(final bool) error {
	if h == nil || h.path == "" {
		return nil
	}
	now := h.clock()
	h.mu.Lock()
	kept := h.pending[:0]
	for _, claim := range h.pending {
		if final || now.Sub(time.UnixMicro(claim.At)) >= usageHistoryClaimWait {
			h.recordClaimLocked(claim)
			continue
		}
		kept = append(kept, claim)
	}
	h.pending = kept
	lines := h.unflushed
	h.unflushed = nil
	compactDue := now.Sub(h.lastCompact) >= usageHistoryCompactEvery
	h.mu.Unlock()
	size, err := h.appendLines(lines)
	if err != nil {
		h.mu.Lock()
		h.unflushed = append(lines, h.unflushed...)
		if over := len(h.unflushed) - usageHistoryMaxUnflushed; over > 0 {
			h.unflushed = h.unflushed[over:]
		}
		h.mu.Unlock()
		h.noteWrite(err)
		return err
	}
	if compactDue || size > h.maxBytes {
		_, err = h.compact()
	}
	h.noteWrite(err)
	return err
}

// noteWrite logs the first failure of a run of failed writes, once.
func (h *UsageHistory) noteWrite(err error) {
	h.mu.Lock()
	first := err != nil && !h.writeFailed
	h.writeFailed = err != nil
	h.mu.Unlock()
	if first {
		h.logger.Warn("usage history could not be written; the daemon keeps it in memory and serves as before",
			"path", h.path, "error", err)
	}
}

func (h *UsageHistory) appendLines(lines []usageHistoryLine) (int64, error) {
	if len(lines) == 0 {
		return 0, nil
	}
	h.fileMu.Lock()
	defer h.fileMu.Unlock()
	var buffer bytes.Buffer
	for _, line := range lines {
		raw, err := json.Marshal(line)
		if err != nil {
			continue
		}
		buffer.Write(raw)
		buffer.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o700); err != nil {
		return 0, err
	}
	file, err := os.OpenFile(h.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	// One write: a worker that is retiring and the one replacing it append
	// to the same file, and O_APPEND keeps whole writes from interleaving.
	if _, err := file.Write(buffer.Bytes()); err != nil {
		_ = file.Close()
		return 0, err
	}
	info, statErr := file.Stat()
	if err := file.Close(); err != nil {
		return 0, err
	}
	if statErr != nil {
		return 0, statErr
	}
	return info.Size(), nil
}

// compact rewrites the file as its lines once each, oldest first, without
// those past usageHistoryRetention and, above the size bound, without the
// oldest until it fits in three quarters of it. It returns the lines kept,
// also when the rewrite failed, and drops the same from memory.
func (h *UsageHistory) compact() ([]usageHistoryLine, error) {
	now := h.clock()
	h.fileMu.Lock()
	defer h.fileMu.Unlock()
	lines, readErr := readUsageHistoryFile(h.path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return nil, readErr
	}
	lines, cutoff := trimUsageHistory(lines, now.Add(-usageHistoryRetention).UnixMicro(), h.maxBytes)
	h.mu.Lock()
	h.dropBeforeLocked(cutoff)
	h.lastCompact = now
	h.mu.Unlock()
	if errors.Is(readErr, os.ErrNotExist) {
		return lines, nil
	}
	var buffer bytes.Buffer
	for _, line := range lines {
		raw, err := json.Marshal(line)
		if err != nil {
			continue
		}
		buffer.Write(raw)
		buffer.WriteByte('\n')
	}
	temp, err := os.CreateTemp(filepath.Dir(h.path), ".usage-history-*.jsonl")
	if err != nil {
		return lines, err
	}
	name := temp.Name()
	_, err = temp.Write(buffer.Bytes())
	if err == nil {
		err = temp.Chmod(0o600)
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, h.path)
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return lines, err
}

// usageHistoryMaxLine is far longer than any line the history writes, so a
// longer one is damage.
const usageHistoryMaxLine = 64 << 10

// readUsageHistoryFile reads every line it can decode; a line it cannot, or
// one longer than usageHistoryMaxLine, is skipped. Ending the read there
// instead would load nothing and stop every compaction, and with it the 35
// days and the size bound, until the file was removed.
func readUsageHistoryFile(path string) ([]usageHistoryLine, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var lines []usageHistoryLine
	reader := bufio.NewReaderSize(file, usageHistoryMaxLine)
	for {
		raw, err := reader.ReadSlice('\n')
		overlong := errors.Is(err, bufio.ErrBufferFull)
		for errors.Is(err, bufio.ErrBufferFull) {
			_, err = reader.ReadSlice('\n')
		}
		var line usageHistoryLine
		if !overlong && json.Unmarshal(raw, &line) == nil && line.valid() {
			lines = append(lines, line)
		}
		if errors.Is(err, io.EOF) {
			return lines, nil
		}
		if err != nil {
			return lines, err
		}
	}
}

func (line usageHistoryLine) valid() bool {
	if line.Claim != nil {
		return line.Claim.At > 0 && line.Claim.Provider != "" && line.Claim.Account != ""
	}
	return line.At > 0 && line.Provider != "" && line.Account != "" && line.Meter != ""
}

func (line usageHistoryLine) at() int64 {
	if line.Claim != nil {
		return line.Claim.At
	}
	return line.At
}

// trimUsageHistory orders lines oldest first, keeps one of each point and
// claim, drops those before cutoff, and above maxBytes the oldest until three
// quarters of it is left. It returns what is kept and the time of the oldest
// line it may hold.
func trimUsageHistory(lines []usageHistoryLine, cutoff, maxBytes int64) ([]usageHistoryLine, int64) {
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].at() < lines[j].at() })
	type lineKey struct {
		claim                    bool
		provider, account, meter string
		at                       int64
	}
	seen := map[lineKey]bool{}
	kept := lines[:0]
	var sizes []int64
	var total int64
	for _, line := range lines {
		key := lineKey{provider: line.Provider, account: line.Account, meter: line.Meter, at: line.At}
		if line.Claim != nil {
			key = lineKey{claim: true, provider: line.Claim.Provider, account: line.Claim.Account, at: line.Claim.At}
		}
		if line.at() < cutoff || seen[key] {
			continue
		}
		seen[key] = true
		raw, _ := json.Marshal(line)
		kept = append(kept, line)
		sizes = append(sizes, int64(len(raw)+1))
		total += int64(len(raw) + 1)
	}
	if total > maxBytes {
		drop := 0
		for drop < len(kept) && total > maxBytes*3/4 {
			total -= sizes[drop]
			drop++
		}
		kept = kept[drop:]
		if len(kept) > 0 {
			cutoff = kept[0].at()
		}
	}
	return kept, cutoff
}

// dropBeforeLocked forgets what is older than cutoff, as the file does.
func (h *UsageHistory) dropBeforeLocked(cutoff int64) {
	for key, series := range h.series {
		drop := 0
		for drop < len(series.points) && series.points[drop].At < cutoff {
			drop++
		}
		series.points = series.points[drop:]
		if len(series.points) == 0 && series.latest.At < cutoff {
			delete(h.series, key)
		}
	}
	drop := 0
	for drop < len(h.claims) && h.claims[drop].At < cutoff {
		drop++
	}
	h.claims = h.claims[drop:]
}

// usageSegment is a run of one meter's points inside one budget: burn is
// never read across its start. Cause is how it began: roll, claim or drop,
// and "" for the first one the history holds.
type usageSegment struct {
	Cause  string
	Points []usageMeterReading
}

// segments splits one meter's points into segments.
func (h *UsageHistory) segments(provider, account, meter string) []usageSegment {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var points []usageMeterReading
	if series := h.series[usageSeriesKey{provider, account, meter}]; series != nil {
		points = append(points, series.points...)
	}
	var claims []int64
	for _, list := range [][]usageClaim{h.claims, h.pending} {
		for _, claim := range list {
			if claim.Provider == provider && claim.Account == account && !claim.Failed {
				claims = append(claims, claim.At)
			}
		}
	}
	return usageSegments(points, claims)
}

// usageSegments starts a segment where the roll of a started week moved by
// more than usageSegmentRollMove, or where the used fraction fell. The cause
// is claim when a claim that landed lies between the two points, roll when
// the old roll time has passed, and drop otherwise.
func usageSegments(points []usageMeterReading, claims []int64) []usageSegment {
	var out []usageSegment
	for i, point := range points {
		if i == 0 {
			out = append(out, usageSegment{Points: []usageMeterReading{point}})
			continue
		}
		prev := points[i-1]
		moved := !prev.Unstarted && !point.Unstarted && prev.Roll != 0 && point.Roll != 0 &&
			time.Duration(absInt64(point.Roll-prev.Roll))*time.Second > usageSegmentRollMove
		if !moved && point.Used >= prev.Used-usageSegmentFall {
			last := &out[len(out)-1]
			last.Points = append(last.Points, point)
			continue
		}
		cause := usageSegmentDrop
		switch {
		case claimBetween(claims, prev.At, point.At):
			cause = usageSegmentClaim
		case prev.Roll != 0 && prev.Roll*1_000_000 <= point.At:
			cause = usageSegmentRoll
		}
		out = append(out, usageSegment{Cause: cause, Points: []usageMeterReading{point}})
	}
	return out
}

func claimBetween(claims []int64, after, through int64) bool {
	for _, at := range claims {
		if at > after && at <= through {
			return true
		}
	}
	return false
}
