package docscheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// What a reader of the operator doc and the agent skill must find about
// automatic resets (PATCHES §93): both switches, the owner's condition and
// that it stays on until the owner turns it off, the log line to read, the
// views, and how to switch to real mode and back.
var autoResetDocNames = []string{
	"SUBROUTER_AUTO_RESET",
	"SUBROUTER_AUTO_RESET_NEAR_EMPTY",
	"until the owner turns it off",
	"auto reset: would claim the next saved reset",
	"auto reset: claim finished",
	"may_have_landed",
	"auto_reset",
	"auto=",
	"/_subrouter/health",
	"SUBROUTER_AUTO_RESET=1",
	"no headers",
	"restart",
}

// What no doc may say since §93: that nothing claims a reset by itself, or
// that a holder sits at its wall until someone claims by hand. ops/PATCHES.md
// is the history and keeps what earlier sections said.
var staleAutoResetClaims = regexp.MustCompile(`\bNothing claims (?:a Claude|the) reset\b|\bsits at its wall until\b`)

func TestDocsSayWhatAutomaticResetsDo(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{"ops/OPERATING.md", "ops/skills/subrouter/SKILL.md"} {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range autoResetDocNames {
			if !strings.Contains(string(data), name) {
				t.Errorf("%s does not name %q, which a reader needs to run automatic resets", rel, name)
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
			if match := staleAutoResetClaims.FindString(line); match != "" {
				t.Errorf("%s:%d still says %q; since §93 a daemon in real mode claims saved resets itself", rel, n+1, match)
			}
		}
	}
}
