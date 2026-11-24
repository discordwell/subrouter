package docscheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// What a reader of the operator doc and the agent skill must find since
// PATCHES §103: the owner's 24-hour gate and its one exception, the rule's
// step for it, how `srq pool` shows a claim it holds, what the claim's last
// check says when it holds one, and since §105 that the week does not hold a
// Fable reset.
var rollGateNames = []string{"§103", "under 24 hours", "last chance", "roll_gate", "(rolls in 6h, under 24h)",
	"§103 holds it on the reading before the claim", "§105", "it should not matter how much Opus is left"}

// What neither may still say since §105: that a Fable reset waits for a
// quarter of the week, or that a decision carries the week it read for that.
var staleWeekRoom = regexp.MustCompile(`week_left|\(week \d+% left, under 25%\)|week has at least 25%|at least 25% of its week`)

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
		if match := staleWeekRoom.FindString(string(data)); match != "" {
			t.Errorf("%s still says %q; since §105 the account's week does not hold a Fable reset", rel, match)
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
