package docscheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What a reader of the operator doc and the agent skill must find since
// PATCHES §102: what an account's row shows right after a claim the provider
// says it used, what the mark of a block from a claim means, that no automatic
// claim goes by such a block, that the hold of a used reset ends at once, and
// how the daemon reads the account again.
var followUpNames = []string{
	"resets_from_claim", "not_read_since_claim", "`^`", "no automatic claim goes by",
	"hold ends", "saved resets: the first reading after a claim was taken", "read_by",
	"hold-off", "two hours", "§102.2",
}

func TestDocsSayWhatARowShowsRightAfterAClaim(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{"ops/OPERATING.md", "ops/skills/subrouter/SKILL.md"} {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ToLower(string(data))
		for _, name := range followUpNames {
			if !strings.Contains(text, strings.ToLower(name)) {
				t.Errorf("%s does not name %q, which a reader needs since §102", rel, name)
			}
		}
	}
}
