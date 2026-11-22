// Package docscheck fails when an operator doc names a repo path, a
// SUBROUTER_* setting or a /_subrouter/ route that this tree does not have.
// The docs are read by people setting up their own machines, so a renamed
// script or variable must break the build, not their setup.
package docscheck

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The docs this package checks, relative to ops/.
var docs = []string{"GETTING-STARTED.md", "OPERATING.md"}

var (
	repoPathRE = regexp.MustCompile(`(?:^|[\s` + "`" + `\[])((?:ops|cmd|internal|selectacct|docs|deploy)/[A-Za-z0-9._/-]*[A-Za-z0-9_/-])`)
	settingRE  = regexp.MustCompile(`\b(?:SUBROUTER|CLAUDE_ROUTE|SR)_[A-Z0-9_]*[A-Z0-9]\b`)
	routeRE    = regexp.MustCompile(`/_subrouter/[a-z0-9._/-]*[a-z0-9]`)
	linkRE     = regexp.MustCompile(`\]\(([^)#\s]+)`)
)

type ref struct {
	doc  string
	line int
	name string
}

// refKind is one class of name a doc can use, with how the tree proves it.
type refKind struct {
	name     string
	find     func(line string) []string
	resolves func(name string) bool
}

func TestDocReferencesExist(t *testing.T) {
	root := repoRoot(t)
	files := trackedFiles(t, root)
	clone := clonePaths(root, files)
	// The deploy scripts alone read some settings, such as SUBROUTER_PLIST.
	settings := tokenSet(corpus(t, root, files, isCode), settingRE)
	// The Cloudflare worker's TypeScript names routes too, and the docs
	// describe the daemon's.
	routes := tokenSet(corpus(t, root, files, isGo), routeRE)

	kinds := []refKind{
		{"path", submatches(repoPathRE), func(p string) bool { return pathExists(clone, p) }},
		{"link", submatches(linkRE), func(p string) bool {
			return strings.Contains(p, "://") || pathExists(clone, path.Join("ops", p))
		}},
		{"setting", matches(settingRE), func(s string) bool { return settings[s] }},
		{"route", matches(routeRE), func(r string) bool { return routes[r] }},
	}

	for _, kind := range kinds {
		var refs []ref
		for _, doc := range docs {
			refs = append(refs, findRefs(t, filepath.Join(root, "ops", doc), doc, kind.find)...)
		}
		if len(refs) == 0 {
			t.Errorf("no %s references found in %v; the pattern matches nothing", kind.name, docs)
		}
		t.Logf("%d %s references", len(refs), kind.name)
		for _, r := range refs {
			if !kind.resolves(r.name) {
				t.Errorf("%s:%d names %s %q, which this tree does not have", r.doc, r.line, kind.name, r.name)
			}
		}
	}
}

func trackedFiles(t *testing.T, root string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	return strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
}

// isCode leaves out docs and tests, so a name counts only where code or a
// script reads it.
func isCode(rel string) bool {
	return !strings.HasSuffix(rel, ".md") && !strings.HasSuffix(rel, "_test.go")
}

func isGo(rel string) bool {
	return strings.HasSuffix(rel, ".go") && isCode(rel)
}

func corpus(t *testing.T, root string, files []string, keep func(string) bool) []byte {
	t.Helper()
	var corpus bytes.Buffer
	for _, rel := range files {
		if !keep(rel) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		corpus.Write(data)
		corpus.WriteByte('\n')
	}
	return corpus.Bytes()
}

func findRefs(t *testing.T, path, doc string, find func(string) []string) []ref {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var refs []ref
	scanner := bufio.NewScanner(f)
	for n := 1; scanner.Scan(); n++ {
		for _, name := range find(scanner.Text()) {
			refs = append(refs, ref{doc: doc, line: n, name: name})
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return refs
}

// clonePaths is what a clean clone of this commit has: each tracked file still
// in the checkout, and each directory above one. An untracked or ignored local
// file is not in it, so it cannot stand in for one the docs name.
func clonePaths(root string, files []string) map[string]bool {
	clone := map[string]bool{}
	for _, file := range files {
		if _, err := os.Stat(filepath.Join(root, file)); err != nil {
			continue
		}
		for p := file; p != "." && !clone[p]; p = path.Dir(p) {
			clone[p] = true
		}
	}
	return clone
}

func pathExists(clone map[string]bool, p string) bool {
	return clone[path.Clean(p)]
}

func tokenSet(corpus []byte, re *regexp.Regexp) map[string]bool {
	set := map[string]bool{}
	for _, m := range re.FindAll(corpus, -1) {
		set[string(m)] = true
	}
	return set
}

func matches(re *regexp.Regexp) func(string) []string {
	return func(line string) []string { return re.FindAllString(line, -1) }
}

func submatches(re *regexp.Regexp) func(string) []string {
	return func(line string) []string {
		var out []string
		for _, m := range re.FindAllStringSubmatch(line, -1) {
			out = append(out, m[1])
		}
		return out
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}
