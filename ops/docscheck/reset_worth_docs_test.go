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
