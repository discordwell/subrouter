package docscheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// What a reader of the operator doc and the agent skill must find since
// PATCHES §96: that need holds a yes until under 0.9 and how a held answer
// reads, that a choice keeps its place through a short gap, that an account
// whose claim is not settled is not chosen, and what the worth switch undoes.
var steadyChoiceNames = []string{"still yes", "under 0.9", "30 minutes", "keeps its place", "is not settled", "the decision to §93's rule",
	"live read"}

// What no doc but ops/PATCHES.md may still say since §96: that
// SUBROUTER_AUTO_RESET_WORTH=0 undoes all of §94, when it undoes only the
// decision.
var staleWorthUndo = regexp.MustCompile(`goes back to §93's rule alone|\x600\x60 goes back to §93's rule`)

func TestDocsSayHowNeedHoldsAndWhatTheWorthSwitchUndoes(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{"ops/OPERATING.md", "ops/skills/subrouter/SKILL.md"} {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range steadyChoiceNames {
			if !strings.Contains(string(data), name) {
				t.Errorf("%s does not name %q, which a reader needs since §96", rel, name)
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
			if match := staleWorthUndo.FindString(line); match != "" {
				t.Errorf("%s:%d still says %q; SUBROUTER_AUTO_RESET_WORTH=0 returns only the decision to §93's rule (§96)", rel, n+1, match)
			}
		}
	}
}
