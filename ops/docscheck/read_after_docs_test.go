package docscheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// What a reader of the operator doc and the agent skill must find since
// PATCHES §99: the claim line's reading_after and its outcomes, and that a
// usage read after the claim that cannot be taken does not move the turns.
var readAfterNames = []string{"reading_after", "still_spent", "not_taken", "§99"}

// What no doc but ops/PATCHES.md may still say since §99: that a waiting turn
// is served on the same account only when the account reads back after the
// claim.
var staleReadAfterRule = regexp.MustCompile(`When the account reads back, the turn is answered`)

func TestDocsSayATurnWaitsOnAClaimWhoseReadingAfterCannotBeTaken(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{"ops/OPERATING.md", "ops/skills/subrouter/SKILL.md"} {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range readAfterNames {
			if !strings.Contains(string(data), name) {
				t.Errorf("%s does not name %q, which a reader needs since §99", rel, name)
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
			if match := staleReadAfterRule.FindString(line); match != "" {
				t.Errorf("%s:%d still says %q; since §99 a reading that cannot be taken does not move the turns", rel, n+1, match)
			}
		}
	}
}
