package docscheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// What a reader of the operator doc and the agent skill must find since
// PATCHES §103: the owner's 24-hour gate and its one exception, the Fable
// week room, the rule's steps and field for them, how `srq pool` shows a
// claim either holds, and what the claim's last check says when it holds one.
var rollGateNames = []string{"§103", "under 24 hours", "last chance", "25%", "roll_gate", "week_room", "week_left",
	"(rolls in 6h, under 24h)", "(week 3% left, under 25%)", "(week not read)", "§103 holds it on the reading before the claim"}

// What no doc but ops/PATCHES.md may still say since §103: that a claim with
// its roll under 24 hours away is held back for what it would add, which the
// gate now holds before the value is weighed.
var staleNearRollValue = regexp.MustCompile(`adds 0\.\d\d wk, rolls in (?:[0-9]|1[0-9]|2[0-3])h\)|held back: the roll is (?:[0-9]|1[0-9]|2[0-3])h(?:[0-9]+m)? away`)

func TestDocsSayNoClaimIsMadeInsideADayOfTheRoll(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{"ops/OPERATING.md", "ops/skills/subrouter/SKILL.md"} {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range rollGateNames {
			if !strings.Contains(string(data), name) {
				t.Errorf("%s does not name %q, which a reader needs to know why an automatic claim was held since §103", rel, name)
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
			if match := staleNearRollValue.FindString(line); match != "" {
				t.Errorf("%s:%d still says %q; since §103 a roll under 24 hours away holds the claim before its value is weighed", rel, n+1, match)
			}
		}
	}
}
