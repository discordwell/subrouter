package docscheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// What a reader must find about the Claude week reset's clock since PATCHES
// §94.1: it was measured, on 2026-11-23, and a claim keeps the roll.
var measuredWeekResetNames = []string{"measured_no", "2026-11-23"}

// What no doc but ops/PATCHES.md may still say: that no restarts_clock value
// is measured.
var staleClockClaims = regexp.MustCompile(`\bneither value is measured\b|restarts_clock\x60? values are assumed`)

func TestDocsSayTheWeekResetKeepsItsRoll(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{"ops/OPERATING.md", "ops/skills/subrouter/SKILL.md"} {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range measuredWeekResetNames {
			if !strings.Contains(string(data), name) {
				t.Errorf("%s does not name %q: the week reset's clock was measured, and when", rel, name)
			}
		}
	}
	for _, rel := range trackedFiles(t, root) {
		if !strings.HasSuffix(rel, ".md") || rel == "ops/PATCHES.md" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		for n, line := range strings.Split(string(data), "\n") {
			if match := staleClockClaims.FindString(line); match != "" {
				t.Errorf("%s:%d still says %q; since §94.1 the week reset's clock is measured", rel, n+1, match)
			}
		}
	}
}

// What a reader of the operator doc and the agent skill must find about the
// rule of PATCHES §94.2: its switch, the held-back verdict, and that real
// mode turns the two older Codex spenders off.
var worthRuleNames = []string{"SUBROUTER_AUTO_RESET_WORTH", "held_back", "SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET", "reset claim contradicts the kinds table"}

// What no doc but ops/PATCHES.md may still say since §94.2: that real mode
// refuses to start beside the older Codex spenders.
var staleSpenderRefusal = regexp.MustCompile(`(?i)refuses to start (?:with \x601\x60 )?beside`)

func TestDocsSayHowAutomaticResetsDecideAndWhoSpends(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{"ops/OPERATING.md", "ops/skills/subrouter/SKILL.md"} {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range worthRuleNames {
			if !strings.Contains(string(data), name) {
				t.Errorf("%s does not name %q, which a reader needs to know when an automatic claim is made", rel, name)
			}
		}
	}
	for _, rel := range trackedFiles(t, root) {
		if !strings.HasSuffix(rel, ".md") || rel == "ops/PATCHES.md" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		for n, line := range strings.Split(string(data), "\n") {
			if match := staleSpenderRefusal.FindString(line); match != "" {
				t.Errorf("%s:%d still says %q; since §94.2 real mode starts and turns the older Codex spenders off", rel, n+1, match)
			}
		}
	}
}

// What a reader of the operator doc and the agent skill must find about the
// views of PATCHES §94.3: that the owner's condition still comes first, why
// the rule reads pressure and not burn, how `srq pool` shows a held-back
// decision and a claim the expiry decided, and the rule's fields on a
// decision and on the health route.
var worthViewNames = []string{"comes first", "Pressure, not burn", "auto=held back", "(not needed, pressure", "(adds 0.07 wk",
	"(expiry window)", "(last chance)", "decided_by", "hours_to_roll", "`worth`"}

// What no doc but ops/PATCHES.md may still say since §94.2: that nothing
// claims on the pool's need, the expiry window or the resets list it is
// built from.
var staleNothingClaimsOnIt = regexp.MustCompile(`\bNothing claims on it yet\b|§93's rule still decides|(?i:nothing claims or ranks on)`)

func TestDocsSayHowToReadWhatTheRuleUsed(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{"ops/OPERATING.md", "ops/skills/subrouter/SKILL.md"} {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range worthViewNames {
			if !strings.Contains(string(data), name) {
				t.Errorf("%s does not name %q, which a reader needs to read what the rule decided", rel, name)
			}
		}
	}
	for _, rel := range trackedFiles(t, root) {
		if !strings.HasSuffix(rel, ".md") || rel == "ops/PATCHES.md" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		for n, line := range strings.Split(string(data), "\n") {
			if match := staleNothingClaimsOnIt.FindString(line); match != "" {
				t.Errorf("%s:%d still says %q; since §94.2 automatic resets claim on the pool's need and the expiry window", rel, n+1, match)
			}
		}
	}
}
