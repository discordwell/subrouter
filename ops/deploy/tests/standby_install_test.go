//go:build !windows

package deploytests

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestStandbyInstall runs standby-install-test.sh, the cases for
// macos/install-standby.sh (PATCHES §79.5), one Go subtest per case. On macOS
// it runs only in a sandbox, as the deploy cases do (requireFence).
func TestStandbyInstall(t *testing.T) {
	requireFence(t)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	harness := filepath.Join(wd, "standby-install-test.sh")
	src, err := os.ReadFile(harness)
	if err != nil {
		t.Fatalf("read the stand-in installer cases (run this test from the module, with the file path; see the package doc): %v", err)
	}
	var cases []string
	seen := map[string]bool{}
	for _, m := range caseLine.FindAllStringSubmatch(string(src), -1) {
		if seen[m[1]] {
			t.Fatalf("case %q is defined twice in %s", m[1], harness)
		}
		seen[m[1]] = true
		cases = append(cases, m[1])
	}
	if len(cases) == 0 {
		t.Fatalf("found no case in %s; the case pattern no longer matches", harness)
	}
	verdict := regexp.MustCompile(`standby install tests: [1-9][0-9]* assertions passed, 0 failed`)
	slots := make(chan struct{}, 6)
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			slots <- struct{}{}
			defer func() { <-slots }()
			cmd := exec.Command("bash", harness, "="+name)
			cmd.Dir = wd
			out, err := cmd.CombinedOutput()
			text := string(out)
			if err != nil {
				t.Fatalf("case %s failed: %v\n%s", name, err, text)
			}
			if !strings.Contains(text, "\n  ok   "+name+"\n") || !verdict.MatchString(text) {
				t.Fatalf("case %s exited 0 without its ok verdict and a non-zero assertion count:\n%s", name, text)
			}
		})
	}
}
