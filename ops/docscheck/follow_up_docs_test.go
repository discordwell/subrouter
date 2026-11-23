package docscheck

import (
	"os"
	"path/filepath"
	"regexp"
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

// PATCHES §102.2 cites code by file and line, each line followed by the
// function or constant it is in: `internal/proxy/proxy.go:1294`
// (`usageWindowsThrottleUntil`), or a range as `...go:1686-1704`. Lines move
// with every edit and the test does not pin them; the name says where the
// code went, so it must still be declared in the file.
var (
	codeCitationRE = regexp.MustCompile("`([A-Za-z0-9_./-]+\\.go):\\d+(?:-\\d+)?` \\(`([A-Za-z_][A-Za-z0-9_]*)`\\)")
	lineCitationRE = regexp.MustCompile(`\.go:\d`)
)

func TestPatches102CitesOnlyCodeThatIsDeclared(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "ops", "PATCHES.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	start := strings.Index(text, "\n## 102. ")
	if start < 0 {
		t.Fatal("ops/PATCHES.md has no §102")
	}
	section := text[start+1:]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	citations := codeCitationRE.FindAllStringSubmatch(section, -1)
	if len(citations) == 0 {
		t.Fatal("§102 cites no line of code by file, line and name")
	}
	if lines := len(lineCitationRE.FindAllString(section, -1)); lines != len(citations) {
		t.Errorf("§102 cites %d lines of code, and %d of them as `file.go:line` (`name`); name the function or constant each line is in", lines, len(citations))
	}
	for _, citation := range citations {
		file, name := citation[1], citation[2]
		source, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Errorf("§102 cites %s, which this tree does not have: %v", citation[0], err)
			continue
		}
		declared := regexp.MustCompile(`(?m)^(func (\([^)]*\) )?` + name + `\(|(const|var|type) ` + name + `\b|\t` + name + `\b[^\n]*=)`)
		if !declared.Match(source) {
			t.Errorf("§102 cites %s, but %s declares no %s; point the citation at the code where it is now", citation[0], file, name)
		}
	}
}
