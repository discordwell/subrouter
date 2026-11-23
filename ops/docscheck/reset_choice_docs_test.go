package docscheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// What a reader of the operator doc and the agent skill must find about the
// accounts chosen for a reset (PATCHES §95.1): the switch, where each mode
// publishes the choice, the log lines, Codex one at a time, the expiry
// burndown's last holder, and the background pass that reads a stale peer.
var resetChoiceDocNames = []string{
	"SUBROUTER_RESET_LOADING",
	"would_choose",
	"reset_choice",
	"reset loading: would choose",
	"no longer chosen",
	"one at a time",
	"likely to expire unused",
	"5 minutes old",
}

// What no doc but ops/PATCHES.md may still say since §95.1: that automatic
// resets read the resets list only for the expiry window. The choice reads
// each account's next reset there too.
var staleListOnlyForWindow = regexp.MustCompile(`(?i)\bread(?:s)? it only for\b`)

func TestDocsSayWhichAccountsAreChosenForAReset(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{"ops/OPERATING.md", "ops/skills/subrouter/SKILL.md"} {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range resetChoiceDocNames {
			if !strings.Contains(string(data), name) {
				t.Errorf("%s does not name %q, which a reader needs to know which accounts are chosen for a reset", rel, name)
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
			if match := staleListOnlyForWindow.FindString(line); match != "" {
				t.Errorf("%s:%d still says %q; since §95.1 the choice of accounts reads each account's next reset from the list too", rel, n+1, match)
			}
		}
	}
}

// What a reader must find about what a choice does to routing (PATCHES
// §95.2): the ceiling a chosen account is loaded to, that a session with an
// account never moves for one, and the Fable-spent preference it skips.
var resetLoadingDocNames = []string{
	"§95.2",
	"SUBROUTER_PACED_SESSION_CEILING",
	"never moves for a choice",
	"Fable-spent preference",
}

// What no doc but ops/PATCHES.md may still say since §95.2: that nothing
// ranks on a choice yet.
var staleNothingRanksOnAChoice = regexp.MustCompile(`(?i)\bnothing ranks on a choice\b`)

func TestDocsSayWhatAChoiceDoesToRouting(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{"ops/OPERATING.md", "ops/skills/subrouter/SKILL.md"} {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range resetLoadingDocNames {
			if !strings.Contains(string(data), name) {
				t.Errorf("%s does not name %q, which a reader needs to know what a choice does to routing", rel, name)
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
			if match := staleNothingRanksOnAChoice.FindString(line); match != "" {
				t.Errorf("%s:%d still says %q; since §95.2 real mode routes new sessions to the chosen accounts", rel, n+1, match)
			}
		}
	}
}
