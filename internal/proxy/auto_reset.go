package proxy

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// The daemon claims an account's next saved reset by itself when the account
// runs out (PATCHES §93). This file is §93.1: the mode, the two moments a
// claim is considered, the owner's under-1-percent condition, which reset,
// the one decision, and shadow mode. Nothing here claims: shadow mode logs
// what real mode would claim, and real mode's claim is not in this build.
//
// Only the daemon that holds an account's credential decides for it. The wall
// is seen on this daemon's own upstream answers, and a reading is one of this
// daemon's own usage rows; a peer's body is never one.

// AutoResetMode is SUBROUTER_AUTO_RESET.
type AutoResetMode int

const (
	// AutoResetOff decides nothing. It is the zero value, so a Server that
	// was never configured (every test that does not ask) behaves as before
	// §93; serve sets the mode from the environment, shadow when unset.
	AutoResetOff AutoResetMode = iota
	// AutoResetShadow decides and logs what real mode would claim, and
	// claims nothing.
	AutoResetShadow
	// AutoResetReal claims. In this build it decides and logs as shadow does.
	AutoResetReal
)

func (m AutoResetMode) String() string {
	switch m {
	case AutoResetShadow:
		return "shadow"
	case AutoResetReal:
		return "real"
	}
	return "off"
}

// Ruling 2026-11-23: an automatic claim happens only while the account has
// under 1% left on the meter the reset gives back -- the week for a full
// Claude reset or a Codex credit, the Fable meter for a Fable reset. The
// owner keeps this condition on until they turn it off themselves
// (SUBROUTER_AUTO_RESET_NEAR_EMPTY=0), "once we're more confident".
const autoResetNearEmptyLeft = 0.01

const (
	// autoResetReadingMaxAge is the oldest reading the condition, and the
	// reading moment, go by.
	autoResetReadingMaxAge = 5 * time.Minute
	// autoResetReadingUsed is how used a meter must read for a reading to be
	// a moment: 99.5 percent.
	autoResetReadingUsed = 99.5
	// autoResetLineEvery is the shortest gap between two would-claim lines for
	// one account and kind.
	autoResetLineEvery = 10 * time.Minute
	// autoResetCreditListEvery is how long a Codex credit list read for a
	// decision is reused.
	autoResetCreditListEvery = 10 * time.Minute
	// autoResetConsiderTimeout bounds one decision at the wall: a Codex
	// decision may read the credit list once.
	autoResetConsiderTimeout = 20 * time.Second
	// codexFiveHourWindow: a Codex usage limit whose reset is further away
	// than this cannot be the five-hour window, so it is the week's.
	codexFiveHourWindow = 5 * time.Hour
)

const autoResetWouldClaimMessage = "auto reset: would claim the next saved reset"

type autoResetSetting struct {
	mode      AutoResetMode
	nearEmpty bool
}

var autoResetConfig atomic.Pointer[autoResetSetting]

// SetAutoReset sets the mode and the owner's under-1-percent condition for
// the process (SUBROUTER_AUTO_RESET, SUBROUTER_AUTO_RESET_NEAR_EMPTY).
func SetAutoReset(mode AutoResetMode, nearEmpty bool) {
	autoResetConfig.Store(&autoResetSetting{mode: mode, nearEmpty: nearEmpty})
}

// AutoReset reports the mode and whether the under-1-percent condition is on.
// Never set, it is off with the condition on.
func AutoReset() (AutoResetMode, bool) {
	setting := autoResetConfig.Load()
	if setting == nil {
		return AutoResetOff, true
	}
	return setting.mode, setting.nearEmpty
}

// autoResetOn reports a mode other than off: only then is a wall looked for.
func autoResetOn() bool {
	mode, _ := AutoReset()
	return mode != AutoResetOff
}

// autoResetMoment is when a claim is considered: at the wall, or on a reading.
type autoResetMoment string

const (
	autoResetAtTheWall  autoResetMoment = "wall"
	autoResetOnAReading autoResetMoment = "reading"
)

// autoResetMeter is a meter a saved reset gives back.
type autoResetMeter string

const (
	autoResetWeek  autoResetMeter = "week"
	autoResetFable autoResetMeter = "fable"
)

func (m autoResetMeter) words() string {
	if m == autoResetFable {
		return "the Fable meter"
	}
	return "the week"
}

// autoResetKind is what the next reset gives back.
type autoResetKind string

const (
	autoResetKindWeek        autoResetKind = "week"
	autoResetKindFable       autoResetKind = "fable"
	autoResetKindCodexCredit autoResetKind = "codex_credit"
	// autoResetKindUnknown is never claimed automatically.
	autoResetKindUnknown autoResetKind = "unknown"
)

// meter is the meter the owner's condition judges for a reset of this kind.
func (k autoResetKind) meter() autoResetMeter {
	if k == autoResetKindFable {
		return autoResetFable
	}
	return autoResetWeek
}

func (k autoResetKind) words() string {
	switch k {
	case autoResetKindWeek:
		return "a week reset"
	case autoResetKindFable:
		return "a Fable reset"
	case autoResetKindCodexCredit:
		return "a Codex reset credit"
	}
	return "a reset of a kind this daemon does not know"
}

// AutoResetVerdict is what a decision came to.
type AutoResetVerdict string

const (
	AutoResetWouldClaim AutoResetVerdict = "would_claim"
	// AutoResetRefusedByCondition is every other check passed and the owner's
	// under-1-percent condition failed.
	AutoResetRefusedByCondition AutoResetVerdict = "refused_by_condition"
	AutoResetNoClaim            AutoResetVerdict = "no_claim"
)

// AutoResetDecision is one decision about claiming an account's next saved
// reset. The daemon keeps the last one per account (LastAutoResetDecision).
type AutoResetDecision struct {
	At       time.Time         `json:"at"`
	Account  string            `json:"account"`
	Label    string            `json:"label,omitempty"`
	Provider accounts.Provider `json:"provider"`
	Mode     string            `json:"mode"`
	Moment   string            `json:"moment"`
	Verdict  AutoResetVerdict  `json:"verdict"`
	Kind     string            `json:"kind,omitempty"`
	// Reset is the grant or credit a claim would spend, and ResetExpiresAt
	// its use-by time.
	Reset          string `json:"reset,omitempty"`
	ResetExpiresAt string `json:"reset_expires_at,omitempty"`
	// Left is how much is left on the meter the reset gives back, in words:
	// "0.4%", or why it is not known.
	Left   string `json:"left,omitempty"`
	Reason string `json:"reason"`
	// Turn is what the request at the wall got from the daemon, as today.
	Turn string `json:"turn,omitempty"`
}

// autoResetNext is the provider's next reset: the one a claim would spend.
type autoResetNext struct {
	kind      autoResetKind
	id        string
	expiresAt string
	// what names an unknown kind: what it clears, or its type.
	what string
	// none says why no reset can be claimed now; "" when one can.
	none string
}

// autoResetReading is one meter as a usage reading measured it.
type autoResetReading struct {
	left float64 // fraction left, 0 to 1
	at   time.Time
}

// autoResetFacts is everything one decision reads.
type autoResetFacts struct {
	moment autoResetMoment
	// wallMeter is the meter the refusal at the wall named, "" when it named
	// none a reset gives back (its readings decide then).
	wallMeter autoResetMeter
	// readingMeter is the meter a reading moment found at its limit.
	readingMeter   autoResetMeter
	deadCredential string
	demand         bool
	next           autoResetNext
	readings       map[autoResetMeter]autoResetReading
	nearEmpty      bool
	now            time.Time
}

// decideAutoReset is the one decision (PATCHES §93.1), for an account this
// daemon holds (only such an account ever has a moment): claim when a moment
// has come, its credential is alive, there is demand on it, its next reset is
// one this daemon knows and can claim now, and -- while the owner's condition
// is on -- the meter that reset gives back has under 1% left. It returns the
// verdict with its reason; the caller fills in who, when and the turn. A
// later rule (burn rate and expiry) replaces this function and leaves the
// claim alone.
func decideAutoReset(f autoResetFacts) AutoResetDecision {
	d := AutoResetDecision{Moment: string(f.moment), Verdict: AutoResetNoClaim}
	switch {
	case f.deadCredential != "":
		d.Reason = "its credential is dead: " + f.deadCredential
		return d
	case !f.demand:
		d.Reason = "no demand: no request met the wall and no warm session is on the account"
		return d
	case f.next.none != "":
		d.Reason = f.next.none
		return d
	}
	d.Kind, d.Reset, d.ResetExpiresAt = string(f.next.kind), f.next.id, f.next.expiresAt
	if f.next.kind == autoResetKindUnknown {
		d.Reason = "the next reset is of a kind this daemon does not know (" + f.next.what + "); such a reset is never claimed automatically"
		return d
	}
	meter := f.next.kind.meter()
	left, known, why := autoResetMeterLeft(f, meter)
	d.Left = why
	if known {
		d.Left = fmt.Sprintf("%.1f%%", left*100)
	}
	moment := autoResetMomentWords(f)
	next := "the next reset is " + f.next.kind.words() + ", which gives back " + meter.words()
	if f.nearEmpty && (!known || left >= autoResetNearEmptyLeft) {
		d.Verdict = AutoResetRefusedByCondition
		if known {
			why = fmt.Sprintf("%s has %.1f%% left", meter.words(), left*100)
		}
		d.Reason = moment + "; " + next + "; refused by the under-1-percent condition: " + why
		return d
	}
	d.Verdict = AutoResetWouldClaim
	d.Reason = moment + "; " + next
	return d
}

// autoResetMeterLeft is how much meter has left: nothing, when the wall's
// own refusal was on it; else what a reading no older than five minutes
// measured. known is false otherwise, and why says so.
func autoResetMeterLeft(f autoResetFacts, meter autoResetMeter) (left float64, known bool, why string) {
	if f.moment == autoResetAtTheWall && f.wallMeter == meter {
		return 0, true, ""
	}
	reading, ok := f.readings[meter]
	if !ok {
		return 0, false, meter.words() + " is unread"
	}
	if age := f.now.Sub(reading.at); age > autoResetReadingMaxAge {
		return reading.left, false, fmt.Sprintf("%s was last read %s ago", meter.words(), age.Round(time.Minute))
	}
	return reading.left, true, ""
}

func autoResetMomentWords(f autoResetFacts) string {
	if f.moment == autoResetOnAReading {
		reading := f.readings[f.readingMeter]
		return fmt.Sprintf("a reading says %s is %.1f%% used", f.readingMeter.words(), (1-reading.left)*100)
	}
	if f.wallMeter == "" {
		return "a request met a usage limit that names no meter"
	}
	return "a request met the limit of " + f.wallMeter.words()
}

// autoResets is the daemon's memory of its decisions, shared by every Server
// copy over one AccountRef (sharedAutoResets).
type autoResets struct {
	mu sync.Mutex
	// now is the clock; a test sets it.
	now func() time.Time
	// last is each account's last decision, by ScoreKey.
	last map[string]AutoResetDecision
	// lines is when the last would-claim line was logged, by ScoreKey and kind.
	lines map[string]time.Time
	// readings is the newest reading already considered, by ScoreKey.
	readings map[string]time.Time
	// credits are the Codex credit lists a decision read, by ScoreKey.
	credits map[string]autoResetCreditList
}

type autoResetCreditList struct {
	credit accounts.RateLimitResetCredit
	found  bool
	at     time.Time
}

func newAutoResets() *autoResets {
	return &autoResets{now: time.Now, last: map[string]AutoResetDecision{}, lines: map[string]time.Time{},
		readings: map[string]time.Time{}, credits: map[string]autoResetCreditList{}}
}

func (r *AccountRef) sharedAutoResets() *autoResets {
	if r == nil {
		return newAutoResets()
	}
	if memory := r.autoResetMemory.Load(); memory != nil {
		return memory
	}
	r.autoResetMemory.CompareAndSwap(nil, newAutoResets())
	return r.autoResetMemory.Load()
}

func (m *autoResets) clock() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now()
}

// record keeps d as the account's last decision and reports whether it gets
// a would-claim line: at most one per account and kind in autoResetLineEvery.
func (m *autoResets) record(key string, d AutoResetDecision) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last[key] = d
	if d.Verdict != AutoResetWouldClaim {
		return false
	}
	lineKey := key + "\x00" + d.Kind
	if last, ok := m.lines[lineKey]; ok && d.At.Sub(last) < autoResetLineEvery {
		return false
	}
	m.lines[lineKey] = d.At
	return true
}

// firstLook reports whether the reading measured at is newer than the last
// one considered for key, and remembers it.
func (m *autoResets) firstLook(key string, at time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !at.After(m.readings[key]) {
		return false
	}
	m.readings[key] = at
	return true
}

// LastAutoResetDecision is the last decision this daemon made for one of its
// own accounts.
func (s Server) LastAutoResetDecision(provider accounts.Provider, id string) (AutoResetDecision, bool) {
	memory := s.AccountRef.sharedAutoResets()
	memory.mu.Lock()
	defer memory.mu.Unlock()
	d, ok := memory.last[selectacct.ScoreKey(schedulerAccountProvider(provider), id)]
	return d, ok
}

// autoResetWall is one refusal at a wall: the account it was on, and the
// meter it named ("" for none).
type autoResetWall struct {
	account accounts.Account
	meter   autoResetMeter
}

// claudeAutoResetWallMeter reads a Claude answer: a 429 that rejects the week
// (7d) is the week's wall, one that rejects the Fable bucket (7d_oi) the
// Fable meter's. A 429 that rejects only the five-hour window is no wall: no
// saved reset is claimed for it. A rejection that names no window is a wall
// whose meter the readings decide.
func claudeAutoResetWallMeter(status int, header http.Header) (autoResetMeter, bool) {
	if status != http.StatusTooManyRequests {
		return "", false
	}
	rejected := func(prefix string) bool {
		return strings.EqualFold(strings.TrimSpace(claudeHeaderGet(header, "anthropic-ratelimit-unified-"+prefix+"-status")), "rejected")
	}
	switch {
	case claudeResponseCooksWeeklyWindow(header):
		return autoResetWeek, true
	case rejected("7d_oi"):
		return autoResetFable, true
	case rejected("5h"):
		return "", false
	case claudeUnifiedStatus(header) == "rejected":
		return "", true
	}
	return "", false
}

// codexAutoResetWallMeter reads a Codex usage-limit refusal. A workspace
// refusal (a spend cap, credits depleted) is no wall: no reset credit lifts
// it. A refusal whose own reset is more than five hours away is the week's;
// any other names no meter, and the readings decide.
func codexAutoResetWallMeter(body []byte, now time.Time) (autoResetMeter, bool) {
	until, workspace, ok := codexUsageLimitHold(body, now)
	switch {
	case !ok:
		return "", usageLimitJSON(body)
	case workspace:
		return "", false
	case until.Sub(now) > codexFiveHourWindow:
		return autoResetWeek, true
	}
	return "", true
}

// autoResetWallOf reads response as a wall on account, or reports none.
// Only a Claude or Codex OAuth account can be at one; the body of a Codex
// answer is peeked, never consumed.
func autoResetWallOf(account accounts.Account, response *http.Response, now time.Time) (autoResetWall, bool) {
	if response == nil || account.ID == "" {
		return autoResetWall{}, false
	}
	var meter autoResetMeter
	var ok bool
	switch accountProviderOrCodex(account) {
	case accounts.ProviderClaude:
		meter, ok = claudeAutoResetWallMeter(response.StatusCode, response.Header)
	case accounts.ProviderCodex:
		meter, ok = codexAutoResetWallMeter(peekResponseBodyPrefix(response), now)
	}
	return autoResetWall{account: account, meter: meter}, ok
}

// autoResetWallsSeenKey marks a response whose walls the retry transport
// already noted, so the passive capture of the same answer does not note
// them again.
type autoResetWallsSeenKey struct{}

func markAutoResetWallsSeen(response *http.Response) *http.Response {
	if response == nil {
		return nil
	}
	request := response.Request
	if request == nil {
		request = &http.Request{}
	}
	response.Request = request.WithContext(context.WithValue(request.Context(), autoResetWallsSeenKey{}, true))
	return response
}

func autoResetWallsSeen(response *http.Response) bool {
	if response == nil || response.Request == nil {
		return false
	}
	seen, _ := response.Request.Context().Value(autoResetWallsSeenKey{}).(bool)
	return seen
}

// autoResetTurnAfterFailover says what the retry transport did with a turn
// that met a wall on walled: the answer it returned, and from which account.
func autoResetTurnAfterFailover(walled string, response *http.Response, err error) string {
	switch {
	case err != nil || response == nil:
		return "failed before an answer"
	case response.StatusCode >= 200 && response.StatusCode < 300:
		routed, ok := routedResponseAccount(response)
		switch {
		case !ok || routed.ID == "":
			return "served by a fallback stage (an API key, Bedrock or the peer)"
		case routed.ID == walled:
			return "served on the same account by another model"
		}
		return "moved to another account (" + routed.ID + ")"
	}
	return fmt.Sprintf("answered %d", response.StatusCode)
}

// Turns at a Codex websocket wall, as the relay answers them.
const (
	autoResetTurnReconnect        = "the socket was closed so the client reconnects onto another account"
	autoResetTurnRefusalDelivered = "answered with the provider's refusal"
)

// autoResetAtTheWall considers a claim for the account a real request just
// met a wall on; its callers look for a wall only when autoResetOn. The
// decision runs in the background, so the turn never waits for it; what the
// turn got (turn) is today's whatever the mode.
func (s Server) autoResetAtTheWall(wall autoResetWall, turn string) {
	if s.AccountRef == nil || s.CredentialBroker != nil {
		return
	}
	s.Lifecycle.StartBackground(func() {
		ctx, cancel := context.WithTimeout(context.Background(), autoResetConsiderTimeout)
		defer cancel()
		account, own := s.autoResetOwnAccount(accountProviderOrCodex(wall.account), wall.account.ID)
		if !own {
			return
		}
		s.considerAutoReset(ctx, account, s.autoResetRow(account), autoResetFacts{
			moment: autoResetAtTheWall, wallMeter: wall.meter, demand: true,
		}, turn)
	})
}

// RunAutoReset looks at each new usage reading of this daemon's own accounts
// every interval, for the reading moment. It reads the rows the usage reads
// already published, so it adds no usage fetch.
func (s Server) RunAutoReset(ctx context.Context, every time.Duration) {
	if s.AccountRef == nil || every <= 0 {
		return
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		s.autoResetReadings(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// autoResetReadings is one pass over this daemon's own usage rows. A row is
// a moment when its reading is new, at most five minutes old, says a meter a
// saved reset gives back is at least 99.5% used, and the account's next
// reset can be claimed now.
func (s Server) autoResetReadings(ctx context.Context) {
	if mode, _ := AutoReset(); mode == AutoResetOff || s.AccountRef == nil || s.CredentialBroker != nil || ctx.Err() != nil {
		return
	}
	rows, ok := s.AccountRef.UsageStatusSnapshot()
	if !ok {
		return
	}
	memory := s.AccountRef.sharedAutoResets()
	now := memory.clock()
	for i := range rows {
		row := rows[i]
		provider := accountProviderFor(row.Provider)
		if row.AuthMode != accounts.AuthModeOAuth || (provider != accounts.ProviderClaude && provider != accounts.ProviderCodex) {
			continue
		}
		account, own := s.autoResetOwnAccount(provider, row.ID)
		if !own || !row.UsageFresh || row.UsageMeasuredAt.IsZero() || now.Sub(row.UsageMeasuredAt) > autoResetReadingMaxAge {
			continue
		}
		if !memory.firstLook(selectacct.ScoreKey(schedulerAccountProvider(provider), account.ID), row.UsageMeasuredAt) {
			continue
		}
		meter, spent := autoResetSpentMeter(provider, autoResetReadingsOf(&row))
		if !spent {
			continue
		}
		if next := s.autoResetNextFor(account, &row, autoResetOnAReading, now); next.none != "" {
			continue
		}
		s.considerAutoReset(ctx, account, &row, autoResetFacts{
			moment: autoResetOnAReading, readingMeter: meter, demand: s.autoResetWarmSessionOn(account),
		}, "")
	}
}

// autoResetSpentMeter is the meter a reading found at least 99.5% used: the
// week first, then (Claude) the Fable meter.
func autoResetSpentMeter(provider accounts.Provider, readings map[autoResetMeter]autoResetReading) (autoResetMeter, bool) {
	meters := []autoResetMeter{autoResetWeek}
	if provider == accounts.ProviderClaude {
		meters = append(meters, autoResetFable)
	}
	for _, meter := range meters {
		if reading, ok := readings[meter]; ok && (1-reading.left)*100 >= autoResetReadingUsed {
			return meter, true
		}
	}
	return "", false
}

// considerAutoReset gathers the rest of f, decides, and keeps and logs the
// decision. A Codex decision that would claim reads the account's credit
// list once to name the credit a claim would spend: the available one that
// expires soonest.
func (s Server) considerAutoReset(ctx context.Context, account accounts.Account, row *AccountUsageStatus, f autoResetFacts, turn string) {
	mode, nearEmpty := AutoReset()
	memory := s.AccountRef.sharedAutoResets()
	provider := accountProviderOrCodex(account)
	f.now = memory.clock()
	f.nearEmpty = nearEmpty
	f.deadCredential = s.autoResetDeadCredential(account)
	f.readings = autoResetReadingsOf(row)
	f.next = s.autoResetNextFor(account, row, f.moment, f.now)
	d := decideAutoReset(f)
	if d.Verdict == AutoResetWouldClaim && provider == accounts.ProviderCodex {
		f.next = s.autoResetCodexCredit(ctx, memory, account, f.now)
		d = decideAutoReset(f)
	}
	d.At, d.Account, d.Provider, d.Mode, d.Turn = f.now, account.ID, provider, mode.String(), turn
	d.Label = account.ID
	if row != nil && strings.TrimSpace(row.Label) != "" {
		d.Label = row.Label
	}
	if !memory.record(selectacct.ScoreKey(schedulerAccountProvider(provider), account.ID), d) || s.Logger == nil {
		return
	}
	fields := []any{"account", d.Label, "provider", string(provider), "mode", d.Mode, "moment", d.Moment,
		"kind", d.Kind, "reset", d.Reset, "reset_expires_at", d.ResetExpiresAt, "left", d.Left, "reason", d.Reason}
	if d.Turn != "" {
		fields = append(fields, "turn", d.Turn)
	}
	s.Logger.Info(autoResetWouldClaimMessage, fields...)
}

// autoResetOwnAccount is this daemon's own OAuth account id of provider.
func (s Server) autoResetOwnAccount(provider accounts.Provider, id string) (accounts.Account, bool) {
	if id == "" {
		return accounts.Account{}, false
	}
	for _, account := range oauthAccounts(s.accountList()) {
		if account.ID == id && accountProviderOrCodex(account) == provider {
			return account, true
		}
	}
	return accounts.Account{}, false
}

// autoResetRow is account's row in this daemon's last usage sweep, or nil.
func (s Server) autoResetRow(account accounts.Account) *AccountUsageStatus {
	rows, ok := s.AccountRef.UsageStatusSnapshot()
	if !ok {
		return nil
	}
	provider := accountProviderOrCodex(account)
	for i := range rows {
		if rows[i].ID == account.ID && accountProviderFor(rows[i].Provider) == provider {
			return &rows[i]
		}
	}
	return nil
}

// autoResetReadingsOf is the meters a saved reset gives back, as row's
// reading measured them: Claude's week (7d) and Fable meter, Codex's week.
func autoResetReadingsOf(row *AccountUsageStatus) map[autoResetMeter]autoResetReading {
	out := map[autoResetMeter]autoResetReading{}
	if row == nil || !row.UsageFresh || row.UsageMeasuredAt.IsZero() {
		return out
	}
	provider := accountProviderFor(row.Provider)
	note := func(meter autoResetMeter, used float64) {
		left := min(max((100-used)/100, 0), 1)
		if current, ok := out[meter]; !ok || left < current.left {
			out[meter] = autoResetReading{left: left, at: row.UsageMeasuredAt}
		}
	}
	for _, window := range row.Windows {
		if window.ExtraUsage != nil {
			continue
		}
		switch {
		case provider == accounts.ProviderClaude && window.Name == "7d" && window.Feature == "":
			note(autoResetWeek, window.UsedPercent)
		case provider == accounts.ProviderClaude && window.Name == agentclaude.FableWindowName:
			note(autoResetFable, window.UsedPercent)
		case provider == accounts.ProviderCodex && !accounts.IsModelScopedWindow(window) &&
			!accounts.IsSpendWindow(window) && accounts.IsLongQuotaWindow(window):
			note(autoResetWeek, window.UsedPercent)
		}
	}
	return out
}

// autoResetDeadCredential says why account's credential is dead, or "".
func (s Server) autoResetDeadCredential(account accounts.Account) string {
	if failure, dead := s.AccountRef.terminalCredFailure(account); dead {
		return failure
	}
	if _, revoked := claudeCredentialRevokedSince(s.claudeCostStore(), account); revoked {
		return "Anthropic revoked it"
	}
	return ""
}

// autoResetWarmSessionOn reports demand on account between requests: a warm
// session (a turn in flight, a turn in the last 70 minutes, or no evidence
// since a restart) assigned to it.
func (s Server) autoResetWarmSessionOn(account accounts.Account) bool {
	if s.Sessions == nil {
		return false
	}
	provider := string(schedulerAccountProvider(accountProviderOrCodex(account)))
	for _, assignment := range s.Sessions.All() {
		if assignment.AccountID != account.ID || string(providerForStoredSession(assignment.AgentType)) != provider {
			continue
		}
		if s.sessionWarm(assignment.AgentType, assignment.SessionID) {
			return true
		}
	}
	return false
}

// autoResetNextFor is the provider's next reset on account. Claude: the
// grant Anthropic names next (next_grant_id), from the block this daemon last
// read with the account's login, which is the week reset before any Fable
// reset. Codex: a credit, when the reading says one is available; which one
// is read only for a claim (autoResetCodexCredit).
func (s Server) autoResetNextFor(account accounts.Account, row *AccountUsageStatus, moment autoResetMoment, now time.Time) autoResetNext {
	if accountProviderOrCodex(account) == accounts.ProviderCodex {
		reset := (*accounts.ComplimentaryResetInfo)(nil)
		if row != nil {
			reset = row.ComplimentaryReset
		}
		switch {
		case reset == nil || !reset.Known:
			return autoResetNext{none: "the account's reset credits have not been read"}
		case !reset.Available:
			return autoResetNext{none: "the account holds no reset credit"}
		}
		return autoResetNext{kind: autoResetKindCodexCredit}
	}
	state := s.AccountRef.cachedClaudeResetState(account.ID, account.Token)
	if state.status == nil {
		return autoResetNext{none: "the account's saved resets have not been read"}
	}
	grant, refusal := chooseClaudeResetGrant(state.status, "", now)
	if refusal != "" && !(moment == autoResetAtTheWall && grant != nil && grant.ID == state.status.NextGrantID &&
		claudeGrantWaitsOnlyForALimit(state.status, *grant, now)) {
		return autoResetNext{none: refusal}
	}
	next := autoResetNext{kind: autoResetKindUnknown, id: grant.ID, expiresAt: grant.EndsAt, what: "clears " + strings.Join(grant.Clears, ", ")}
	switch grant.Kind() {
	case agentclaude.ResetKindWeek:
		next.kind = autoResetKindWeek
	case agentclaude.ResetKindFable:
		next.kind = autoResetKindFable
	}
	return next
}

// claudeGrantWaitsOnlyForALimit reports a next grant whose block says it
// cannot be used only because the account was not at a limit when the block
// was read. At the wall the account is at one, and a claim reads the block
// again before it sends anything.
func claudeGrantWaitsOnlyForALimit(status *agentclaude.ResetStatus, grant agentclaude.ResetGrant, now time.Time) bool {
	if !status.Eligible || agentclaude.ResetStatusUnread(status) || status.AtLimit || grant.UsableNow || !grant.UseRequiresLimit {
		return false
	}
	if until, ok := agentclaude.ParseResetTime(status.CooldownUntil); ok && now.Before(until) {
		return false
	}
	grant.UsableNow = true
	return claudeResetGrantUnusable(grant, status, now) == ""
}

// autoResetCodexCredit names the credit a claim on account would spend: the
// available one that expires soonest, as the claim itself picks it
// (accounts.FirstAvailableRateLimitResetCredit). The list is read at most
// once per autoResetCreditListEvery per account.
func (s Server) autoResetCodexCredit(ctx context.Context, memory *autoResets, account accounts.Account, now time.Time) autoResetNext {
	key := selectacct.ScoreKey(schedulerAccountProvider(accounts.ProviderCodex), account.ID)
	memory.mu.Lock()
	cached, ok := memory.credits[key]
	memory.mu.Unlock()
	if !ok || now.Sub(cached.at) >= autoResetCreditListEvery {
		credit, found, err := accounts.FirstAvailableRateLimitResetCredit(ctx, s.AccountRef.client, account)
		if err != nil {
			return autoResetNext{none: "the account's reset credits could not be listed: " + err.Error()}
		}
		cached = autoResetCreditList{credit: credit, found: found, at: now}
		memory.mu.Lock()
		memory.credits[key] = cached
		memory.mu.Unlock()
	}
	if !cached.found {
		return autoResetNext{none: "the account holds no reset credit that can be redeemed now"}
	}
	next := autoResetNext{kind: autoResetKindCodexCredit, id: cached.credit.ID, expiresAt: cached.credit.ExpiresAt}
	if resetType := strings.TrimSpace(cached.credit.ResetType); resetType != "" && resetType != codexRateLimitsResetType {
		next.kind, next.what = autoResetKindUnknown, "reset_type "+resetType
	}
	return next
}

// codexRateLimitsResetType is the reset_type of the Codex credits the code
// knows: they reset the account's rate limits.
const codexRateLimitsResetType = "codex_rate_limits"
