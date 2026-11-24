package docscheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What a reader of the operator doc and the agent skill must find about the
// Fable-spent preference (PATCHES §65, §104): its name, the clause the router
// logs when it decides, that it holds when the daemon ranks the peers'
// accounts alone, and the switch.
var fableSpareDocNames = []string{
	"Fable-spent preference",
	"higher weekly pace after the Fable-spent preference and the live-session discount",
	"§104",
	"ranks the peers' accounts alone",
	"SUBROUTER_FABLE_SPENT_PREFERENCE",
}

func TestDocsSayWhereTheFableSpentPreferenceRanks(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{"ops/OPERATING.md", "ops/skills/subrouter/SKILL.md"} {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range fableSpareDocNames {
			if !strings.Contains(string(data), name) {
				t.Errorf("%s does not name %q, which a reader needs to know where an Opus or Sonnet turn goes", rel, name)
			}
		}
	}
}
