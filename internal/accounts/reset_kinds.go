package accounts

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// The kinds of saved usage-limit reset (PATCHES §91.1). Every kind differs in
// what a claim gives back, whether it starts a new seven days, whether it can
// be claimed before the limit is reached, and where the provider uses it in
// its own order; new kinds will appear. Each kind is one entry in one table,
// and a reading no entry recognizes is published as unknown, never dropped
// and never guessed.

// ResetKind names a kind of saved reset as usage-status publishes it.
type ResetKind string

const (
	// ResetKindClaudeWeek is a Claude grant that refills the general week.
	ResetKindClaudeWeek ResetKind = "week"
	// ResetKindClaudeFable is a Claude grant that clears the Fable limit and
	// does not refill the week (PATCHES §64); it gives back the part of the
	// week the Fable meter had counted (§103).
	ResetKindClaudeFable ResetKind = "fable"
	// ResetKindCodexRateLimits is a Codex rate-limit reset credit.
	ResetKindCodexRateLimits ResetKind = "codex_rate_limits"
	// ResetKindUnknown is a reading no table entry recognizes.
	ResetKindUnknown ResetKind = "unknown"
)

// CodexResetCountSource is the key of the Codex usage reading's credit count
// (ComplimentaryResetInfo.Source). The count carries no reset_type, so before
// the credit list is read the count is the kind this key names.
const CodexResetCountSource = "rate_limit_reset_credits"

// ClockRestart says whether a claim starts a new seven days, and whether that
// was measured or is assumed.
type ClockRestart string

const (
	ClockRestartAssumedYes ClockRestart = "assumed_yes"
	ClockRestartAssumedNo  ClockRestart = "assumed_no"
	// ClockRestartMeasuredNo is a kind a claim was seen not to restart: the
	// account's roll was the same instant before and after (PATCHES §94.1,
	// §103).
	ClockRestartMeasuredNo ClockRestart = "measured_no"
	ClockRestartUnknown    ClockRestart = "unknown"
)

// Restarts reports whether a claim of the kind starts a new seven days, and
// false for known when nothing says.
func (c ClockRestart) Restarts() (restarts, known bool) {
	switch c {
	case ClockRestartAssumedYes:
		return true, true
	case ClockRestartAssumedNo, ClockRestartMeasuredNo:
		return false, true
	}
	return false, false
}

// ResetMeter is the meter a kind is claimed on: the one its holder spends to
// reach the limit the claim lifts, named as the usage history names it.
type ResetMeter string

const (
	ResetMeterWeek  ResetMeter = "week"
	ResetMeterFable ResetMeter = "fable"
)

// ResetReading is one saved reset as a provider's reading states it, before
// it is classified.
type ResetReading struct {
	Provider Provider
	// Source is where the reading came from: ResetProgram's name for a Claude
	// grant, CodexResetCountSource for a Codex count read before its list.
	Source string
	// Type is the provider's own type: a Codex credit's reset_type, a Claude
	// grant's type.
	Type string
	// Clears is the limits a Claude grant says a claim clears.
	Clears []string
}

// ResetKindSpec is the description of one kind.
type ResetKindSpec struct {
	Kind     ResetKind
	Provider Provider
	// Matches reports whether a reading of Provider is this kind. The first
	// entry that matches decides.
	Matches func(ResetReading) bool
	// GivesBack is the meters a claim gives back, in the provider's names,
	// when the reading names none itself.
	GivesBack []string
	// Meter is the meter a holder spends to reach this kind's limit.
	Meter         ResetMeter
	RestartsClock ClockRestart
	// NeedsLimit is whether a claim is accepted only at the limit, when the
	// reading does not say (a Claude grant's use_requires_limit does).
	NeedsLimit bool
	// UseOrder is the kind's place in the provider's own order of use, 1
	// first.
	UseOrder int
}

var defaultResetKinds = []ResetKindSpec{
	{
		Kind:     ResetKindClaudeWeek,
		Provider: ProviderClaude,
		// A grant with no clears list is one too: that is how a block from
		// before Anthropic listed them reads, and October's grant cleared the
		// week.
		Matches: func(r ResetReading) bool {
			return len(r.Clears) == 0 || slices.Contains(r.Clears, "seven_day")
		},
		GivesBack: []string{"seven_day"},
		Meter:     ResetMeterWeek,
		// Measured 2026-11-23 (PATCHES §94.1): a week reset claimed by hand
		// at 100% used left the week's roll the same instant, and gave back
		// the week and the Fable meter. Seen five times by 2026-11-24, two by
		// hand and three automatic (§103): the roll never moved, and the week
		// and the Fable meter both went to 0.
		RestartsClock: ClockRestartMeasuredNo,
		NeedsLimit:    true,
		// The owner, 2026-11-22: the provider uses the week reset first, then
		// the Fable resets.
		UseOrder: 1,
	},
	{
		Kind:     ResetKindClaudeFable,
		Provider: ProviderClaude,
		Matches: func(r ResetReading) bool {
			return slices.Contains(r.Clears, "seven_day_overage_included") && !slices.Contains(r.Clears, "seven_day")
		},
		GivesBack: []string{"seven_day_overage_included"},
		Meter:     ResetMeterFable,
		// Measured 2026-11-24 (PATCHES §103): one automatic claim of a Fable
		// reset left the week's roll the same instant, took the Fable meter
		// from 100% to 0, and the week from 97% to 47%.
		RestartsClock: ClockRestartMeasuredNo,
		NeedsLimit:    true,
		UseOrder:      2,
	},
	{
		Kind:     ResetKindCodexRateLimits,
		Provider: ProviderCodex,
		Matches: func(r ResetReading) bool {
			return r.Type == "codex_rate_limits" || (r.Type == "" && r.Source == CodexResetCountSource)
		},
		// What the redeem code and its fakes assume (reset_autospend.go): the
		// weekly window refills.
		GivesBack:     []string{"weekly"},
		Meter:         ResetMeterWeek,
		RestartsClock: ClockRestartAssumedYes,
		NeedsLimit:    false,
		UseOrder:      1,
	},
}

// resetKinds is the table in use: nil is defaultResetKinds.
var resetKinds atomic.Pointer[[]ResetKindSpec]

// ResetKinds is the table in use, in the order its entries are tried.
func ResetKinds() []ResetKindSpec {
	if table := resetKinds.Load(); table != nil {
		return slices.Clone(*table)
	}
	return slices.Clone(defaultResetKinds)
}

// UseResetKinds replaces the table and returns the call that puts the
// previous one back. Tests use it to add a kind; the daemon never calls it.
func UseResetKinds(table []ResetKindSpec) (restore func()) {
	previous := resetKinds.Swap(&table)
	return func() { resetKinds.Store(previous) }
}

// ClassifyReset is the table entry a reading is, or false for a reading of a
// kind the table does not know.
func ClassifyReset(reading ResetReading) (ResetKindSpec, bool) {
	for _, spec := range ResetKinds() {
		if spec.Provider == reading.Provider && spec.Matches(reading) {
			return spec, true
		}
	}
	return ResetKindSpec{}, false
}

// ResetKindNamed is the table entry for provider's kind, as a list entry
// names it; false for unknown and for a kind the table does not hold.
func ResetKindNamed(provider Provider, kind ResetKind) (ResetKindSpec, bool) {
	for _, spec := range ResetKinds() {
		if spec.Provider == provider && spec.Kind == kind {
			return spec, true
		}
	}
	return ResetKindSpec{}, false
}

// ResetExpiry says where a HeldReset's expiry came from.
type ResetExpiry string

const (
	// ResetExpiryRead is the provider's date from its latest reading.
	ResetExpiryRead ResetExpiry = "read"
	// ResetExpiryLastGood is a Codex date kept from an earlier good read of
	// the credit list after the latest read failed.
	ResetExpiryLastGood ResetExpiry = "last_good"
	// ResetExpiryNotRead is a Codex count whose credit list has not been read
	// well yet, or is not read at all here: never "no expiry".
	ResetExpiryNotRead ResetExpiry = "not_read"
	// ResetExpiryNoneGiven is a reading that carries no date for the reset.
	ResetExpiryNoneGiven ResetExpiry = "none_given"
)

// HeldResetReading is resets an account holds as one reading states them.
type HeldResetReading struct {
	ResetReading
	Count int
	// ExpiresAt is the provider's expiry, verbatim.
	ExpiresAt string
	Expiry    ResetExpiry
	// ExpiryReadAt is when the read that gave ExpiresAt was sent, for a date
	// that can be older than the row (the Codex credit list).
	ExpiryReadAt time.Time
	// NeedsLimit is the reading's own word on whether a claim needs the
	// limit reached; nil when it says nothing.
	NeedsLimit *bool
}

// HeldReset is one entry of a usage-status row's resets: what the account
// holds of one kind ending at one time.
type HeldReset struct {
	Kind ResetKind `json:"kind"`
	// RawType is an unknown reset's own type, as the provider wrote it.
	RawType   string      `json:"raw_type,omitempty"`
	Count     int         `json:"count"`
	ExpiresAt string      `json:"expires_at,omitempty"`
	Expiry    ResetExpiry `json:"expiry"`
	// ExpiryReadAt and ExpiryAgeSeconds date a Codex expiry: when the credit
	// list that gave it was read, and how long ago that was.
	ExpiryReadAt     time.Time `json:"expiry_read_at,omitzero"`
	ExpiryAgeSeconds *int64    `json:"expiry_age_seconds,omitempty"`
	// GivesBack is what a claim gives back: the meters the reading names, else
	// the kind's own.
	GivesBack     []string     `json:"gives_back,omitempty"`
	RestartsClock ClockRestart `json:"restarts_clock"`
	// NeedsLimit is absent when nothing says.
	NeedsLimit *bool `json:"needs_limit,omitempty"`
	// Order is the entry's place in the provider's order of use, 1 first. An
	// unknown kind has no place in it.
	Order int `json:"order,omitempty"`
	// ManualOnly marks a kind the table does not know: shown, never claimed
	// automatically.
	ManualOnly bool `json:"manual_only,omitempty"`
	// ExpiryWindow is when the daemon that holds the account starts spending
	// this reset before it expires (PATCHES §94.1). Set on that daemon's own
	// rows, for a known kind with a date.
	ExpiryWindow *ResetExpiryWindow `json:"expiry_window,omitempty"`
}

// ResetExpiryWindow is the stretch before a reset's expiry in which any demand
// at the account's limit is reason enough to claim it.
type ResetExpiryWindow struct {
	OpensAt time.Time `json:"opens_at"`
	// LeadSeconds is how long before the expiry the window opens.
	LeadSeconds int64 `json:"lead_seconds"`
	// Why says how the lead was reached, with its numbers.
	Why string `json:"why"`
}

// Expires is the entry's expiry as a time; false when it carries none, or one
// that does not parse.
func (h HeldReset) Expires() (time.Time, bool) {
	return parseResetExpiry(h.ExpiresAt)
}

// HeldResets groups readings into a row's list: one entry per kind and
// expiry, counts added, in the provider's order of use (the kind's UseOrder,
// then the soonest expiry), with unknown kinds after every known one. Never
// nil.
func HeldResets(readings []HeldResetReading, now time.Time) []HeldReset {
	type held struct {
		entry   HeldReset
		order   int
		expires time.Time
	}
	var list []*held
	byKey := map[string]*held{}
	for _, reading := range readings {
		if reading.Count <= 0 {
			continue
		}
		entry := HeldReset{
			Count:        reading.Count,
			ExpiresAt:    reading.ExpiresAt,
			Expiry:       reading.Expiry,
			ExpiryReadAt: reading.ExpiryReadAt.UTC().Truncate(time.Second),
		}
		order := 0
		if spec, ok := ClassifyReset(reading.ResetReading); ok {
			entry.Kind, entry.RestartsClock, order = spec.Kind, spec.RestartsClock, spec.UseOrder
			entry.GivesBack = slices.Clone(spec.GivesBack)
			needs := spec.NeedsLimit
			entry.NeedsLimit = &needs
		} else {
			entry.Kind, entry.RawType, entry.RestartsClock, entry.ManualOnly = ResetKindUnknown, reading.Type, ClockRestartUnknown, true
		}
		if len(reading.Clears) > 0 {
			entry.GivesBack = slices.Clone(reading.Clears)
		}
		if reading.NeedsLimit != nil {
			needs := *reading.NeedsLimit
			entry.NeedsLimit = &needs
		}
		expires, _ := parseResetExpiry(reading.ExpiresAt)
		expiryKey := reading.ExpiresAt
		if !expires.IsZero() {
			expiryKey = expires.UTC().Format(time.RFC3339Nano)
		}
		needs := "unknown"
		if entry.NeedsLimit != nil {
			needs = fmt.Sprint(*entry.NeedsLimit)
		}
		key := strings.Join([]string{string(entry.Kind), entry.RawType, strings.Join(entry.GivesBack, ","), needs,
			expiryKey, string(entry.Expiry), entry.ExpiryReadAt.Format(time.RFC3339)}, "\x00")
		if existing, ok := byKey[key]; ok {
			existing.entry.Count += entry.Count
			continue
		}
		item := &held{entry: entry, order: order, expires: expires}
		byKey[key] = item
		list = append(list, item)
	}
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if (a.order == 0) != (b.order == 0) {
			return a.order != 0
		}
		if a.order != b.order {
			return a.order < b.order
		}
		if a.expires.IsZero() != b.expires.IsZero() {
			return !a.expires.IsZero()
		}
		if !a.expires.Equal(b.expires) {
			return a.expires.Before(b.expires)
		}
		return a.entry.RawType < b.entry.RawType
	})
	out := make([]HeldReset, 0, len(list))
	place := 0
	for _, item := range list {
		entry := item.entry
		if item.order != 0 {
			place++
			entry.Order = place
		}
		if !entry.ExpiryReadAt.IsZero() {
			age := int64(now.Sub(entry.ExpiryReadAt) / time.Second)
			entry.ExpiryAgeSeconds = &age
		}
		out = append(out, entry)
	}
	return out
}

// CreditHeldAt reports whether an account still holds credit at now: not
// redeemed, and not past an expiry it states.
func CreditHeldAt(credit RateLimitResetCredit, now time.Time) bool {
	if credit.Status != "" && credit.Status != "available" {
		return false
	}
	expires, ok := parseResetExpiry(credit.ExpiresAt)
	return !ok || expires.After(now)
}

// parseResetExpiry reads an expiry in the formats both providers send.
func parseResetExpiry(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999999Z0700"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}
