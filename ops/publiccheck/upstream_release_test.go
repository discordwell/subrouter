package publiccheck

import (
	"bytes"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// This fork publishes its own releases (ops/PATCHES.md §107), so nothing may
// fetch, install or verify manaflow-ai/subrouter's. In any file, a release,
// API or raw-file URL for it is a finding. In code, so is the bare slug, the
// way a script or Go names its default release repository. A comment line
// may name it, to credit a port, and so may a quoted Go import path, which is
// how install-daemon recognizes a binary built before the module was renamed
// (§106).

const upstreamSlug = "manaflow-ai/subrouter"

var upstreamURLRE = regexp.MustCompile(`(?i)(github\.com/manaflow-ai/subrouter/releases|api\.github\.com/repos/manaflow-ai/subrouter|raw\.githubusercontent\.com/manaflow-ai/subrouter)`)

const upstreamFix = "fetch discordwell/subrouter's releases instead (ops/PATCHES.md §107)"

// upstreamHistory is where the fork's record of upstream lives.
var upstreamHistory = map[string]bool{"ops/PATCHES.md": true}

var codeExtensions = map[string]bool{
	".go": true, ".sh": true, ".bash": true, ".py": true, ".js": true, ".mjs": true, ".cjs": true,
	".ts": true, ".json": true, ".toml": true, ".yml": true, ".yaml": true, ".service": true,
}

// isCode reports whether rel is a program or a config a program reads.
func isCode(rel string, data []byte) bool {
	base := path.Base(rel)
	return codeExtensions[path.Ext(rel)] || base == "Makefile" || base == "Dockerfile" || bytes.HasPrefix(data, []byte("#!"))
}

func isCommentLine(line string) bool {
	line = strings.TrimSpace(line)
	for _, prefix := range []string{"//", "#", "/*", "*", "<!--"} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// upstreamFindings returns what in line fetches or names upstream's releases.
func upstreamFindings(rel, line string, code bool) []string {
	if urls := upstreamURLRE.FindAllString(line, -1); urls != nil || !code || isCommentLine(line) {
		return urls
	}
	lower := strings.ToLower(line)
	for i := 0; ; i++ {
		j := strings.Index(lower[i:], upstreamSlug)
		if j < 0 {
			return nil
		}
		i += j
		importPath := path.Ext(rel) == ".go" && strings.HasSuffix(lower[:i], `"github.com/`) && strings.HasPrefix(lower[i+len(upstreamSlug):], "/")
		if !importPath {
			return []string{line[i : i+len(upstreamSlug)]}
		}
	}
}

func TestUpstreamFindings(t *testing.T) {
	for _, tc := range []struct {
		rel, line string
		want      []string
	}{
		{"install.sh", `repo="manaflow-ai/subrouter"`, []string{"manaflow-ai/subrouter"}},
		{"deploy/gcp/fetch-release.sh", `REPO="${SUBROUTER_REPO:-manaflow-ai/subrouter}"`, []string{"manaflow-ai/subrouter"}},
		{"x.yml", `gh release download v1 --repo manaflow-ai/subrouter`, []string{"manaflow-ai/subrouter"}},
		{"cmd/subrouter/sr_update.go", `	updateReleaseRepo = "manaflow-ai/subrouter"`, []string{"manaflow-ai/subrouter"}},
		{"cmd/subrouter/sr_server.go", `const u = "https://github.com/manaflow-ai/subrouter/releases/latest/download/install.sh"`, []string{"github.com/manaflow-ai/subrouter/releases"}},
		{"README.md", "curl -fsSL https://github.com/Manaflow-AI/subrouter/releases/latest/download/install.sh | sh", []string{"github.com/Manaflow-AI/subrouter/releases"}},
		{"x.go", `	u := "https://api.github.com/repos/manaflow-ai/subrouter/commits/v1"`, []string{"api.github.com/repos/manaflow-ai/subrouter"}},
		{"x.sh", `# curl -fsSL https://raw.githubusercontent.com/manaflow-ai/subrouter/main/x.sh | bash`, []string{"raw.githubusercontent.com/manaflow-ai/subrouter"}},
		{"x.sh", `gh api "repos/manaflow-ai/subrouter/compare/a...main"`, []string{"manaflow-ai/subrouter"}},

		{"x.go", `// Ported from upstream manaflow-ai/subrouter #370 (6bcbcbd).`, nil},
		{"x.sh", `# upstream manaflow-ai/subrouter's script`, nil},
		{"x.go", `	preRenameMainPackage = "github.com/manaflow-ai/subrouter/cmd/subrouter"`, nil},
		{"AGENTS.md", "This repository is a maintained fork of manaflow-ai/subrouter.", nil},
		{"install.sh", `repo="discordwell/subrouter"`, nil},
	} {
		code := isCode(tc.rel, nil)
		if got := upstreamFindings(tc.rel, tc.line, code); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("upstreamFindings(%q, %q) = %q, want %q", tc.rel, tc.line, got, tc.want)
		}
	}
}

func TestNoUpstreamReleaseFetch(t *testing.T) {
	root := repoRoot(t)
	self := filepath.ToSlash(filepath.Join("ops", "publiccheck")) + "/"
	for _, rel := range trackedFiles(t, root) {
		if upstreamHistory[rel] || strings.HasPrefix(rel, self) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil || bytes.IndexByte(data[:min(len(data), 8192)], 0) >= 0 {
			continue
		}
		code := isCode(rel, data)
		for n, line := range strings.Split(string(data), "\n") {
			for _, found := range upstreamFindings(rel, line, code) {
				t.Errorf("%s:%d: %s; %s", rel, n+1, found, upstreamFix)
			}
		}
	}
}
