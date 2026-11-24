package docscheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// What a reader of the operator doc and the agent skill must find since
// PATCHES §103: the switch, the step a held-back claim names, what the row
// carries, the 0.8 bar and the one under it that keeps a passed claim
// passing, how srq pool reads such a claim, the worked example, and what a
// Fable reset was measured to give back, and when.
var waitForBetterNames = []string{"SUBROUTER_AUTO_RESET_WAIT_FOR_BETTER", "later_wall", "later_value", "later_wall_at", "next wall",
	"0.8", "0.64", "(0.27 wk now, 1.00 at its next wall", "7.07", "2026-11-24", "97%", "47%"}

// What no doc but ops/PATCHES.md may still say since §103: that a Fable
// reset's restarts_clock is assumed.
var staleFableClock = regexp.MustCompile(`\x60assumed_no\x60 \((?:a )?Fable(?: reset)?\)|\x60assumed_no\x60 for a Fable reset`)

func TestDocsSayHowAClaimWaitsForABetterWall(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{"ops/OPERATING.md", "ops/skills/subrouter/SKILL.md"} {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range waitForBetterNames {
			if !strings.Contains(string(data), name) {
				t.Errorf("%s does not name %q, which a reader needs since §103", rel, name)
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
			if match := staleFableClock.FindString(line); match != "" {
				t.Errorf("%s:%d still says %q; since §103 a Fable reset's restarts_clock is measured_no", rel, n+1, match)
			}
		}
	}
}
