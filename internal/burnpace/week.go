package burnpace

import "strings"

// weekRule says which of one provider's windows is the week: the window of
// that Name, and, when limitSeconds is not 0, of exactly that length.
type weekRule struct {
	provider     string
	window       string
	limitSeconds int64
}

// weekRules is the whole definition of the week. It also decides which
// providers can be in the numbers, which providers' left-out rows are named,
// and the order of the provider rows, so none of those can disagree with it.
//
// Left out on purpose: a codex seat whose weekly limit arrives as "secondary"
// while "primary" is the 5 h window, a usage-based codex seat's 30-day "spend"
// window, and claude's "5h", "extra" and model-scoped windows.
var weekRules = []weekRule{
	{provider: "claude", window: "7d"},
	{provider: "codex", window: "primary", limitSeconds: 604800},
}

func weekRuleFor(provider string) (weekRule, bool) {
	for _, rule := range weekRules {
		if rule.provider == provider {
			return rule, true
		}
	}
	return weekRule{}, false
}

// weekOf returns the first of the row's windows, in its own order, that its
// provider's rule matches.
func weekOf(row Row) (Window, bool) {
	rule, ok := weekRuleFor(row.Provider)
	if !ok {
		return Window{}, false
	}
	for _, w := range row.Windows {
		if w.Name == rule.window && (rule.limitSeconds == 0 || w.LimitSeconds == rule.limitSeconds) {
			return w, true
		}
	}
	return Window{}, false
}

// rowKind is what one row adds to the page.
type rowKind uint8

const (
	// rowDropped is never counted and never named.
	rowDropped rowKind = iota
	// rowLeftOut is named on the page with a reason, and is not in the numbers.
	rowLeftOut
	// rowCounted enters the merge.
	rowCounted
)

type rowVerdict struct {
	kind    rowKind
	why     LeftOutReason
	account Account
	// stale is recorded even when the row then turns out to have rolled.
	stale bool
}

// judgeRow sorts one row of a scope whose reading is shift seconds old: 0 for
// a direct read, the body's age for a peer. A direct read's ResetAfterSeconds
// is taken as relative to now, so 0 reads as rolled.
func judgeRow(row Row, shift float64) rowVerdict {
	if row.ID == "" {
		return rowVerdict{kind: rowDropped}
	}
	week, ok := weekOf(row)
	if !ok || !row.AuthValid {
		switch {
		case !named(row):
			return rowVerdict{kind: rowDropped}
		case unmeasured(row):
			return rowVerdict{kind: rowLeftOut, why: LeftUnmeasured}
		case !row.AuthValid:
			return rowVerdict{kind: rowLeftOut, why: LeftAuthInvalid}
		default:
			return rowVerdict{kind: rowLeftOut, why: LeftNoWeeklyWindow}
		}
	}
	rs := float64(week.ResetAfterSeconds) - shift
	if rs <= 0 {
		return rowVerdict{kind: rowLeftOut, why: LeftRolled, stale: row.SweepCut}
	}
	return rowVerdict{
		kind:  rowCounted,
		stale: row.SweepCut,
		account: Account{
			ID:           row.ID,
			Label:        row.Label,
			Used:         week.UsedPercent / 100,
			ElapsedHours: (weekSeconds - rs) / 3600,
			ResetHours:   rs / 3600,
		},
	}
}

// named reports whether a left-out row is named on the page. AuthMode gates
// only the naming: an API-key row with a valid week is counted.
func named(row Row) bool {
	_, ok := weekRuleFor(row.Provider)
	return ok && oauthOrUnset(row.AuthMode)
}

// unmeasured is the daemon's own "not measured this pass" rule.
func unmeasured(row Row) bool {
	if !oauthOrUnset(row.AuthMode) {
		return false
	}
	return row.SweepCut || !row.AuthChecked ||
		strings.Contains(row.Error, "context canceled") ||
		strings.Contains(row.Error, "context deadline exceeded")
}

func oauthOrUnset(mode string) bool { return mode == "" || mode == "oauth" }
