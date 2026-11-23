// Package skills checks that each agent skill in this directory names only
// what exists in the tree: repository paths, PATCHES sections, routes,
// environment variables, commands, flags, table columns, and quoted strings.
package skills

import (
	"fmt"
	"go/scanner"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const maxSkillLines = 250

// Docs the skill links that other changes add. A missing one passes, and once
// it exists the ordinary path check applies.
var pendingDocs = map[string]bool{
	"ops/GETTING-STARTED.md": true,
	"ops/OPERATING.md":       true,
}

// Routes the skill names as wrong guesses. Each must stay unregistered.
var guessedRoutes = map[string]bool{
	"/_subrouter/status": true,
	"/_subrouter/usage":  true,
}

// Upstream's label in the `sr status` Use column, which this fork replaced.
var upstreamOnly = map[string]bool{
	"protected < 40%": true,
}

// Commands that belong to other programs, whose names this tree does not define.
var externalCommands = map[string]bool{
	"codex": true, "curl": true, "git": true, "journalctl": true, "launchctl": true,
}

var (
	spanRE        = regexp.MustCompile("`([^`\n]+)`")
	sectionRE     = regexp.MustCompile(`§(\d+(?:\.\d+)*)`)
	headingRE     = regexp.MustCompile(`(?m)^## (\d+)\. `)
	envNameRE     = regexp.MustCompile(`\b[A-Z][A-Z0-9]*_[A-Z0-9_]+\b`)
	envSpanRE     = regexp.MustCompile(`^([A-Z][A-Z0-9]*_[A-Z0-9_]+)(=\S*)?$`)
	flagRE        = regexp.MustCompile("(?:^|[\\s\\[(`])--([a-z][a-z-]*\\*?)")
	columnRE      = regexp.MustCompile("(?:`([^`\n]+)`|\\b([A-Z][A-Za-z0-9]*)) column")
	placeholderRE = regexp.MustCompile(`\.\.\.|<[^>]*>`)
	plainWordRE   = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	pathPrefixes  = []string{"ops/", "deploy/", "cmd/", "internal/", "selectacct/", "./"}
)

type tree struct {
	root     string
	source   string // non-test Go, Python and shell sources, ops/bin, and the Makefile
	cli      string // non-test Go under cmd/subrouter
	usage    string // the string literals in cli, where its help and usage text lives
	patches  string
	srq      string
	makefile string
	sections map[string]bool
	guessed  map[string]bool
}

func loadTree(t *testing.T) *tree {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	read := func(rel string) string {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(data)
	}
	var source, cli strings.Builder
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || strings.HasPrefix(rel, "ops/skills/") || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		ext := filepath.Ext(rel)
		if ext != ".go" && ext != ".py" && ext != ".sh" && !strings.HasPrefix(rel, "ops/bin/") && rel != "Makefile" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, rel))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		source.Write(data)
		source.WriteByte('\n')
		if ext == ".go" && strings.HasPrefix(rel, "cmd/subrouter/") {
			cli.Write(data)
			cli.WriteByte('\n')
		}
	}
	tr := &tree{
		root:     root,
		source:   source.String(),
		cli:      cli.String(),
		usage:    stringLiterals(cli.String()),
		patches:  read("ops/PATCHES.md"),
		srq:      read("ops/bin/srq"),
		makefile: read("Makefile"),
		sections: map[string]bool{},
		guessed:  guessedRoutes,
	}
	for _, m := range headingRE.FindAllStringSubmatch(tr.patches, -1) {
		tr.sections[m[1]] = true
	}
	return tr
}

// check returns one problem per name in the skill that the tree does not have.
// name is the skill's directory name, and text is its SKILL.md.
func (tr *tree) check(name, text string) []string {
	var problems []string
	add := func(line int, format string, args ...any) {
		problems = append(problems, "SKILL.md:"+strconv.Itoa(line)+": "+fmt.Sprintf(format, args...))
	}

	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) > maxSkillLines {
		add(len(lines), "%d lines, over the %d-line budget", len(lines), maxSkillLines)
	}
	for _, p := range frontmatterProblems(name, lines) {
		add(1, "%s", p)
	}

	inCode := false
	for i, line := range lines {
		n := i + 1
		if strings.HasPrefix(line, "```") {
			inCode = !inCode
			continue
		}
		for _, m := range sectionRE.FindAllStringSubmatch(line, -1) {
			if !tr.hasSection(m[1]) {
				add(n, "§%s is not in ops/PATCHES.md", m[1])
			}
		}
		for _, env := range envNameRE.FindAllString(line, -1) {
			if !tr.hasEnv(env) {
				add(n, "environment variable %s is not read anywhere in the tree", env)
			}
		}
		if inCode {
			command, _, _ := strings.Cut(line, " #")
			for _, flag := range tr.missingFlags(command) {
				add(n, "flag --%s is not defined in %s", flag, flagOwner(command))
			}
			if fields := strings.Fields(command); len(fields) > 0 && isTreeCommand(fields[0]) {
				if p := tr.spanProblem(command); p != "" {
					add(n, "%s", p)
				}
			}
			continue
		}
		for _, m := range columnRE.FindAllStringSubmatch(line, -1) {
			if title := m[1] + m[2]; !strings.Contains(tr.cli, `Title: "`+title+`"`) {
				add(n, "no table in cmd/subrouter has a column titled %q", title)
			}
		}
		for _, m := range spanRE.FindAllStringSubmatch(line, -1) {
			for _, flag := range tr.missingFlags(m[1]) {
				add(n, "`%s`: flag --%s is not defined in %s", m[1], flag, flagOwner(m[1]))
			}
			if p := tr.spanProblem(m[1]); p != "" {
				add(n, "`%s`: %s", m[1], p)
			}
		}
	}
	return problems
}

func frontmatterProblems(name string, lines []string) []string {
	if len(lines) == 0 || lines[0] != "---" {
		return []string{"no YAML frontmatter"}
	}
	fields := map[string]string{}
	closed := false
	for _, line := range lines[1:] {
		if line == "---" {
			closed = true
			break
		}
		if key, value, ok := strings.Cut(line, ":"); ok {
			fields[key] = strings.TrimSpace(value)
		}
	}
	var problems []string
	if !closed {
		problems = append(problems, "frontmatter is not closed")
	}
	if fields["name"] != name {
		problems = append(problems, fmt.Sprintf("frontmatter name %q does not match the directory %q", fields["name"], name))
	}
	if fields["description"] == "" {
		problems = append(problems, "frontmatter has no description")
	}
	return problems
}

func (tr *tree) spanProblem(span string) string {
	if upstreamOnly[span] {
		return ""
	}
	if strings.HasPrefix(span, "/_subrouter/") {
		literal := `"` + span + `"`
		if prefix, ok := strings.CutSuffix(span, "*"); ok {
			literal = `"` + prefix
		}
		registered := strings.Contains(tr.source, literal)
		switch {
		case tr.guessed[span] && registered:
			return "the skill calls this route a wrong guess, but the tree registers it"
		case !tr.guessed[span] && !registered:
			return "no such route in the tree"
		}
		return ""
	}
	fields := strings.Fields(span)
	head := fields[0]
	switch {
	case isRepoPath(head):
		return tr.pathProblem(strings.TrimPrefix(head, "./"))
	case envSpanRE.MatchString(span):
		return "" // the line scan checks the variable
	case head == "sr" || head == "subrouter":
		var path []string
		for _, word := range fields[1:] {
			if !plainWordRE.MatchString(word) {
				break
			}
			if !strings.Contains(tr.cli, `"`+word+`"`) {
				return fmt.Sprintf("%s has no %q verb", head, word)
			}
			path = append(path, word)
		}
		// The dispatch is nested switches, so a verb path is checked against
		// the help and usage text that names it.
		if len(path) > 1 && !tr.cliNamesVerbPath(path) {
			return fmt.Sprintf("no help or usage text in cmd/subrouter names `%s %s`", head, strings.Join(path, " "))
		}
		return ""
	case head == "srq":
		if len(fields) > 1 && !tr.hasSrqCommand(fields[1]) {
			return fmt.Sprintf("srq has no %q subcommand", fields[1])
		}
		return ""
	case head == "make":
		if len(fields) < 2 || !strings.Contains("\n"+tr.makefile, "\n"+fields[1]+":") {
			return "no such Makefile target"
		}
		return ""
	case externalCommands[head], strings.HasPrefix(head, "--"):
		return "" // another program's command, or a flag that missingFlags checks
	}
	for _, fragment := range placeholderRE.Split(span, -1) {
		if fragment = strings.TrimSpace(fragment); fragment != "" && !strings.Contains(tr.source, fragment) {
			return fmt.Sprintf("%q is not in the tree's source", fragment)
		}
	}
	return ""
}

// cliNamesVerbPath reports whether a string literal in cmd/subrouter starts a
// line with the verb path, as a help listing does, or gives it after "usage:".
// A comment, an error message, or a generated header that mentions it does not
// count.
func (tr *tree) cliNamesVerbPath(path []string) bool {
	phrase := regexp.QuoteMeta(strings.Join(path, " "))
	return regexp.MustCompile(`(?m)(?:^\s*|[Uu]sage:\s*)(?:sr|subrouter) ` + phrase + `(?:[^\w-]|$)`).MatchString(tr.usage)
}

// stringLiterals returns the values of the string literals in Go source, one
// per line.
func stringLiterals(src string) string {
	var s scanner.Scanner
	fset := token.NewFileSet()
	s.Init(fset.AddFile("", fset.Base(), len(src)), []byte(src), nil, 0)
	var b strings.Builder
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			return b.String()
		}
		if value, err := strconv.Unquote(lit); tok == token.STRING && err == nil {
			b.WriteString(value)
			b.WriteByte('\n')
		}
	}
}

func isTreeCommand(word string) bool {
	return word == "sr" || word == "subrouter" || word == "srq"
}

func flagOwner(text string) string {
	if fields := strings.Fields(text); len(fields) > 0 && fields[0] == "srq" {
		return "ops/bin/srq"
	}
	return "cmd/subrouter"
}

func isRepoPath(word string) bool {
	if word == "AGENTS.md" || word == "README.md" {
		return true
	}
	for _, prefix := range pathPrefixes {
		if strings.HasPrefix(word, prefix) {
			return true
		}
	}
	return false
}

func (tr *tree) pathProblem(rel string) string {
	matches, err := filepath.Glob(filepath.Join(tr.root, rel))
	if err != nil {
		return err.Error()
	}
	if len(matches) == 0 && !pendingDocs[rel] {
		return "no such path in the repository"
	}
	return ""
}

func (tr *tree) hasSection(id string) bool {
	top, _, nested := strings.Cut(id, ".")
	if !tr.sections[top] {
		return false
	}
	return !nested || regexp.MustCompile(`(^|[^0-9.])`+regexp.QuoteMeta(id)+`([^0-9]|$)`).MatchString(tr.patches)
}

func (tr *tree) hasEnv(name string) bool {
	return strings.Contains(tr.source, `"`+name+`"`)
}

// missingFlags skips another program's command, whose flags are its own, and
// checks an srq command's flags against srq.
func (tr *tree) missingFlags(text string) []string {
	fields := strings.Fields(text)
	if len(fields) == 0 || externalCommands[fields[0]] {
		return nil
	}
	defined := tr.hasFlag
	if fields[0] == "srq" {
		defined = func(name string) bool { return strings.Contains(tr.srq, `"--`+name+`"`) }
	}
	var missing []string
	for _, m := range flagRE.FindAllStringSubmatch(text, -1) {
		if !defined(m[1]) {
			missing = append(missing, m[1])
		}
	}
	return missing
}

func (tr *tree) hasFlag(name string) bool {
	if prefix, ok := strings.CutSuffix(name, "*"); ok {
		return strings.Contains(tr.cli, `"`+prefix)
	}
	return strings.Contains(tr.cli, `"`+name+`"`) || strings.Contains(tr.cli, `"--`+name+`"`)
}

func (tr *tree) hasSrqCommand(name string) bool {
	return strings.Contains(tr.srq, `"`+name+`": cmd_`)
}

// Only the subrouter skill: the check knows subrouter's names, and the other
// skills here name Claude Code's.
func TestSubrouterSkillNamesOnlyWhatExists(t *testing.T) {
	tr := loadTree(t)
	data, err := os.ReadFile(filepath.Join("subrouter", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range tr.check("subrouter", string(data)) {
		t.Errorf("subrouter/%s", p)
	}
}

func TestCheckCatchesEachKindOfMissingName(t *testing.T) {
	tr := loadTree(t)
	header := "---\nname: demo\ndescription: A demo skill.\n---\n"
	for _, tc := range []struct {
		kind string
		body string
	}{
		{"path", "Read `ops/NO-SUCH-DOC.md`."},
		{"glob path", "Never run `deploy/macos/*no-such-script*.sh`."},
		{"section", "See §999."},
		{"subsection", "See §46.99."},
		{"route", "Read `/_subrouter/no-such-route`."},
		{"guessed route that exists", "A guess such as `/_subrouter/health` returns 404."},
		{"environment variable", "Set SUBROUTER_NO_SUCH_KNOB in the plist."},
		{"environment assignment", "Set `SUBROUTER_NO_SUCH_KNOB=1`."},
		{"sr verb", "Run `sr no-such-verb`."},
		{"sr sub-verb", "Run `sr claude no-such-verb <name>`."},
		{"sr verb path of real words", "Run `sr claude kill <name>`."},
		{"sr verb path in a comment and an error message", "Run `sr codex launch`."},
		{"sr verb path in a generated header", "Run `sr codex from <dir>`."},
		{"sr line in a code block", "```bash\nsr no-such-verb --json\n```"},
		{"sr verb path in a code block", "```bash\nsr daemon reset   # demo\n```"},
		{"srq subcommand", "Run `srq no-such-sub`."},
		{"srq line in a code block", "```bash\nsrq no-such-sub   # demo\n```"},
		{"srq flag that only sr has", "Run `srq pool --plan`."},
		{"make target", "Run `make no-such-target`."},
		{"flag", "Pass `--no-such-flag`."},
		{"flag in a code block", "```bash\nsr status --no-such-flag\n```"},
		{"column known only by its code key", "The `sr status` Pick column is not the gate."},
		{"quoted column", "Read the `Pick` column."},
		{"quoted string", "The log says `no such log line anywhere in the tree`."},
		{"quoted string with a placeholder", "It prints `<account> is nowhere in the tree`."},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			tr := *tr
			tr.guessed = map[string]bool{"/_subrouter/health": true}
			if got := tr.check("demo", header+tc.body+"\n"); len(got) != 1 {
				t.Errorf("check found %d problems, want 1: %q", len(got), got)
			}
		})
	}

	for _, tc := range []struct {
		kind string
		text string
	}{
		{"no frontmatter", "# Demo\n"},
		{"unclosed frontmatter", "---\nname: demo\ndescription: A demo skill.\n"},
		{"name differs from the directory", "---\nname: other\ndescription: A demo skill.\n---\n"},
		{"no description", "---\nname: demo\n---\n"},
		{"over the line budget", header + strings.Repeat("Text.\n", maxSkillLines)},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			if got := tr.check("demo", tc.text); len(got) != 1 {
				t.Errorf("check found %d problems, want 1: %q", len(got), got)
			}
		})
	}

	valid := header + "Read `ops/PATCHES.md` §46.8, set `SUBROUTER_HOLD_DEADLINE=0`, run `sr status --json`, `sr claude reset <name>`, and `srq pool --json`, read the `Use` column, and grep for `proxy hold started`.\n" +
		"```bash\nsr daemon restart   # demo\nsrq serving --json\n```\n"
	if got := tr.check("demo", valid); len(got) != 0 {
		t.Errorf("check found problems in names that exist: %q", got)
	}
}

// The skill's saved-resets section mirrors the operating guide's: each
// setting the guide's section names, the switches that turn a reset
// behaviour off, the skill names too (PATCHES §91.2), so an agent reading
// only the skill can say how to turn each one off.
func TestSubrouterSkillNamesEverySavedResetSetting(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	guide, err := os.ReadFile(filepath.Join(root, "ops", "OPERATING.md"))
	if err != nil {
		t.Fatal(err)
	}
	skill, err := os.ReadFile(filepath.Join("subrouter", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	guideSettings := envNameRE.FindAllString(markdownSection(t, string(guide), "Saved resets"), -1)
	if len(guideSettings) == 0 {
		t.Fatal("calibration: ops/OPERATING.md's Saved resets section names no setting")
	}
	mirror := markdownSection(t, string(skill), "Saved resets")
	for _, name := range guideSettings {
		if !strings.Contains(mirror, name) {
			t.Errorf("ops/OPERATING.md's Saved resets section names %s; say what it does in the Saved resets section of subrouter/SKILL.md too", name)
		}
	}
}

// markdownSection is the body of the first "## " section whose heading starts
// with title, up to the next "## " heading.
func markdownSection(t *testing.T, text, title string) string {
	t.Helper()
	parts := regexp.MustCompile(`(?m)^## `).Split(text, -1)
	for _, part := range parts[1:] {
		if strings.HasPrefix(part, title) {
			return part
		}
	}
	t.Fatalf("no section headed %q", title)
	return ""
}
