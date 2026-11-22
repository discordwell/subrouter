//go:build !windows

// Package hooks tests the git hooks in this directory (privacy-check, and the
// pre-commit, commit-msg and pre-push links to it), and the matcher they share
// with ops/bin/public-scrub, through real git commands in a sandbox: a fake
// HOME and git config, a fake daemon (httptest), a fake tailscale at
// $SUBROUTER_TAILSCALE_BIN, and a bare remote. No test reads this machine's
// daemon, tailscale, private-values file or map.
//
//	cd ops && go test -count=1 ./hooks
package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fixture is what each source tells the hooks. An empty field leaves that
// source out.
type fixture struct {
	accounts, usage, peers string // the daemon's JSON bodies
	tailscale              string // what `tailscale status --json` prints
	email, login           string
	home                   string // $HOME under the sandbox's root; home/<login> when empty
	privateValues          string // ~/.config/subrouter/private-values
	legacyMap              string // the public-scrub map
	base                   string // the first commit's notes.md
}

var full = fixture{
	accounts: `[{"id":"quill.harbor@plantedmail.net","provider":"claude","auth_mode":"oauth",
		"label":"Quillfeather","email":"quill.harbor@plantedmail.net","source":"store"}]`,
	usage: `[{"id":"quill.harbor@plantedmail.net","provider":"claude","auth_mode":"oauth",
		"label":"Quillfeather","email":"quill.harbor@plantedmail.net","source":"store",
		"account_identity":"verified.quill@plantedmail.net"}]`,
	peers: `{"peers":[{"name":"fernbox-west","url":"https://100.80.0.5:31416","reachable":true,
		"accounts":[{"id":"peer.quill@plantedmail.net","provider":"claude","auth_mode":"oauth",
		"email":"peer.quill@plantedmail.net","source":"store"}]}]}`,
	tailscale: `{"BackendState":"Running","TailscaleIPs":["100.99.8.37","fd7a:115c:a1e0::9"],
		"MagicDNSSuffix":"tail0fixture.ts.net",
		"CurrentTailnet":{"Name":"tailnet.quill@plantedmail.net","MagicDNSSuffix":"tail0fixture.ts.net"},
		"Self":{"HostName":"quills-workstation","DNSName":"quills-workstation.tail0fixture.ts.net.",
			"TailscaleIPs":["100.99.8.37","fd7a:115c:a1e0::9"]},
		"Peer":{"nodekey:1":{"HostName":"fern-nas","DNSName":"fern-nas.tail0fixture.ts.net.","TailscaleIPs":["100.99.8.38"]},
			"nodekey:2":{"HostName":"localhost","DNSName":"bramble-phone.tail0fixture.ts.net."}},
		"User":{"1":{"LoginName":"tailquill@plantedmail.net"}}}`,
	email:         "gitquill@plantedmail.net",
	login:         "quillsmith",
	privateValues: "# a former employer\nFernwood Analytics\n",
	legacyMap:     "label\tHollowmere\tplaceholder-org\n",
}

// planted is every value that a test plants, which no hook output may contain.
var planted = []string{
	"quill.harbor@plantedmail.net", "quill.harbor", "Quillfeather", "verified.quill@plantedmail.net",
	"fernbox-west", "100.80.0.5", "peer.quill@plantedmail.net", "100.99.8.37", "fd7a:115c:a1e0::9",
	"tail0fixture.ts.net", "quills-workstation", "fern-nas", "100.99.8.38", "tailquill@plantedmail.net",
	"gitquill@plantedmail.net", "gitquill", "quillsmith", "Fernwood Analytics", "Hollowmere",
	"100.88.41.7", "100.64.213.9", "fd7a:115c:a1e0::10:3", "Émilie Zorbrecht", "Łukasz", "İlkay",
	"100-99-8-37", "100%2e99%2e8%2e37", "fd7a%3a115c%3aa1e0%3a%3a9", "tailnet.quill@plantedmail.net",
	"bramble-phone", "Wavecrest", "user@plantedmail.net", "Quill Harborside",
	"author.quill@plantedmail.net", "committer.quill@plantedmail.net",
}

type sandbox struct {
	t                      *testing.T
	root, home, repo, addr string // addr: the fake daemon's URL
	env                    []string
	accountReads           atomic.Int32
}

func hooksDir(t *testing.T) string {
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func newSandbox(t *testing.T, fx fixture) *sandbox {
	t.Helper()
	root := t.TempDir()
	login := fx.login
	if login == "" {
		login = "nobody-in-particular"
	}
	home := filepath.Join(root, "home", login)
	if fx.home != "" {
		home = filepath.Join(root, fx.home)
	}
	s := &sandbox{t: t, root: root, home: home, repo: filepath.Join(root, "repo"), addr: "http://127.0.0.1:1"}
	tools, fake := filepath.Join(root, "tools"), filepath.Join(root, "fakebin")
	for _, d := range []string{s.home, s.repo, tools, fake, filepath.Join(s.home, ".config", "subrouter")} {
		must(t, os.MkdirAll(d, 0o755))
	}
	// Only git and python3 come from this machine. The hooks look for tailscale
	// in fixed places too, so $SUBROUTER_TAILSCALE_BIN always names the fake or a
	// missing file, and this machine's own tailscale never runs.
	for _, tool := range []string{"git", "python3"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("no %s: %v", tool, err)
		}
		must(t, os.Symlink(path, filepath.Join(tools, tool)))
	}
	if fx.accounts != "" {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body := map[string]string{
				"/_subrouter/accounts":           fx.accounts,
				"/_subrouter/usage-status":       fx.usage,
				"/_subrouter/peers/usage-status": fx.peers,
			}[r.URL.Path]
			if r.URL.Path == "/_subrouter/accounts" {
				s.accountReads.Add(1)
			}
			if body == "" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, body)
		}))
		t.Cleanup(srv.Close)
		s.addr = srv.URL
	}
	tailscale := filepath.Join(root, "no-tailscale")
	if fx.tailscale != "" {
		tailscale = filepath.Join(fake, "tailscale")
		write(t, filepath.Join(root, "tailscale.json"), fx.tailscale)
		write(t, filepath.Join(fake, "tailscale"), fmt.Sprintf(
			"#!/bin/sh\necho \"$*\" >> '%s/tailscale.runs'\nexec /bin/cat '%s/tailscale.json'\n", root, root))
		must(t, os.Chmod(filepath.Join(fake, "tailscale"), 0o755))
	}
	email := fx.email
	if email == "" {
		email = "someone@example.com" // git needs one; a placeholder adds no value
	}
	write(t, filepath.Join(s.home, ".gitconfig"), "[user]\n\tname = Q\n\temail = "+email+"\n[init]\n\tdefaultBranch = main\n")
	if fx.privateValues != "" {
		write(t, filepath.Join(s.home, ".config", "subrouter", "private-values"), fx.privateValues)
	}
	if fx.legacyMap != "" {
		write(t, filepath.Join(root, "map.tsv"), fx.legacyMap)
	}
	s.env = []string{
		"HOME=" + s.home, "PATH=" + tools, "LOGNAME=" + login, "USER=" + login,
		"GIT_CONFIG_NOSYSTEM=1", "SUBROUTER_ADDR=" + s.addr, "SUBROUTER_TAILSCALE_BIN=" + tailscale,
		"SUBROUTER_PUBLIC_SCRUB_MAP=" + filepath.Join(root, "map.tsv"),
	}
	base := fx.base
	if base == "" {
		base = "notes\n"
	}
	s.mustGit("init", "-q")
	write(t, filepath.Join(s.repo, "notes.md"), base)
	s.mustGit("add", "notes.md")
	s.mustGit("commit", "-q", "-m", "first")
	s.mustGit("init", "-q", "--bare", filepath.Join(root, "remote.git"))
	s.mustGit("remote", "add", "origin", filepath.Join(root, "remote.git"))
	s.mustGit("push", "-q", "origin", "main")
	s.mustGit("config", "core.hooksPath", hooksDir(t))
	return s
}

func (s *sandbox) git(args ...string) (string, error) {
	cmd := exec.Command(filepath.Join(s.root, "tools", "git"), args...)
	cmd.Dir, cmd.Env = s.repo, s.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (s *sandbox) mustGit(args ...string) string {
	s.t.Helper()
	out, err := s.git(args...)
	if err != nil {
		s.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

// commit writes a file and commits it; extra goes to git commit.
func (s *sandbox) commit(file, content, message string, extra ...string) (string, error) {
	write(s.t, filepath.Join(s.repo, file), content)
	s.mustGit("add", file)
	return s.git(append([]string{"commit", "-q", "-m", message}, extra...)...)
}

func (s *sandbox) head() string { return strings.TrimSpace(s.mustGit("rev-parse", "HEAD"))[:12] }

func write(t *testing.T, path, content string) {
	t.Helper()
	must(t, os.WriteFile(path, []byte(content), 0o644))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func assertNoValues(t *testing.T, out string) {
	t.Helper()
	for _, v := range planted {
		if strings.Contains(strings.ToLower(out), strings.ToLower(v)) {
			t.Errorf("hook output holds a planted value:\n%s", out)
		}
	}
}

func refused(t *testing.T, out string, err error, where ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("not refused:\n%s", out)
	}
	for _, w := range where {
		if !strings.Contains(out, "\n  "+w+"\n") {
			t.Errorf("output does not name %q:\n%s", w, out)
		}
	}
	assertNoValues(t, out)
}

// refusedExactly is refused, and the hook names no other place.
func refusedExactly(t *testing.T, out string, err error, where ...string) {
	t.Helper()
	refused(t, out, err, where...)
	var named []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "  ") {
			named = append(named, strings.TrimSpace(line))
		}
	}
	if !slices.Equal(named, where) {
		t.Errorf("the hook names %q, want %q:\n%s", named, where, out)
	}
}

func passed(t *testing.T, out string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("refused: %v\n%s", err, out)
	}
	assertNoValues(t, out)
}

// Each source's value is refused by each hook, and the change passes once a
// placeholder replaces it.
func TestEachSourceEachHook(t *testing.T) {
	cases := []struct{ source, value, placeholder string }{
		{"daemon account email", "quill.harbor@plantedmail.net", "someone@example.com"},
		{"daemon account label", "Quillfeather", "box"},
		{"daemon account label, camelCase", "myQuillfeatherConfig", "myBoxConfig"},
		{"daemon verified identity", "verified.quill@plantedmail.net", "someone@example.com"},
		{"daemon peer account", "peer.quill@plantedmail.net", "someone@example.com"},
		{"daemon peer name", "fernbox-west", "box"},
		{"daemon peer host", "100.80.0.5", "100.64.0.5"},
		{"tailscale address", "100.99.8.37", "100.64.0.1"},
		{"tailscale address, hyphens", "ssh 100-99-8-37", "ssh 100-64-0-1"},
		{"tailscale address, URL-encoded", "http%3a%2f%2f100%2e99%2e8%2e37%3a443", "http%3a%2f%2f100%2e64%2e0%2e1%3a443"},
		{"tailscale IPv6 address", "[fd7a:115c:a1e0::9]:443", "[fd7a:115c:a1e0::10:1]:443"},
		{"tailscale IPv6 address, URL-encoded", "host=fd7a%3a115c%3aa1e0%3a%3a9&x", "host=fd7a%3a115c%3aa1e0%3a%3a10%3a1&x"},
		{"tailscale host name", "quills-workstation", "laptop"},
		{"tailscale MagicDNS suffix", "box.tail0fixture.ts.net", "box.tailnet.example"},
		{"tailscale peer", "fern-nas", "box"},
		{"tailscale peer's DNS name, not its host name", "bramble-phone", "box"},
		{"tailscale tailnet name", "tailnet.quill@plantedmail.net", "someone@example.com"},
		{"tailscale user", "tailquill@plantedmail.net", "someone@example.com"},
		{"git email", "gitquill@plantedmail.net", "someone@example.com"},
		{"git email local part", "gitquill wrote this", "someone wrote this"},
		{"login name", "/Users/quillsmith/bin", "~/bin"},
		{"private-values", "Fernwood Analytics", "a former employer"},
		{"legacy map", "Hollowmere", "placeholder-org"},
	}
	for _, c := range cases {
		t.Run(c.source, func(t *testing.T) {
			t.Parallel()
			s := newSandbox(t, full)
			t.Run("pre-commit", func(t *testing.T) {
				out, err := s.commit("notes.md", "notes\nsee "+c.value+"\n", "notes")
				refused(t, out, err, "notes.md:2")
				out, err = s.commit("notes.md", "notes\nsee "+c.placeholder+"\n", "notes")
				passed(t, out, err)
			})
			t.Run("commit-msg", func(t *testing.T) {
				out, err := s.commit("notes.md", "notes\nmore\n", "fix for "+c.value)
				refused(t, out, err, "commit message")
				out, err = s.git("commit", "-q", "-m", "fix for "+c.placeholder)
				passed(t, out, err)
			})
			t.Run("pre-push", func(t *testing.T) {
				s.mustGit("reset", "-q", "--hard", "origin/main")
				s.commit("notes.md", "notes\nsee "+c.value+"\n", "notes", "--no-verify")
				out, err := s.git("push", "-q", "origin", "main")
				refused(t, out, err, s.head()+" notes.md:2")
				s.mustGit("reset", "-q", "--hard", "origin/main")
				s.commit("notes.md", "notes\nmore\n", "fix for "+c.value, "--no-verify")
				out, err = s.git("push", "-q", "origin", "main")
				refused(t, out, err, s.head()+" commit message")
				s.mustGit("reset", "-q", "--hard", "origin/main")
				s.commit("notes.md", "notes\nsee "+c.placeholder+"\n", "fix for "+c.placeholder)
				out, err = s.git("push", "-q", "origin", "main")
				passed(t, out, err)
			})
		})
	}
}

// SUBROUTER_ADDR is host:port in the systemd environment file and in
// ops/deploy/macos, and a URL in srq. The hooks read the daemon at either.
func TestDaemonAddrWithoutScheme(t *testing.T) {
	s := newSandbox(t, fixture{accounts: full.accounts})
	s.env = append(s.env, "SUBROUTER_ADDR="+strings.TrimPrefix(s.addr, "http://"))
	out, err := s.commit("notes.md", "notes\nQuillfeather\n", "notes")
	refused(t, out, err, "notes.md:2")
}

// The hooks find the tailscale CLI at $SUBROUTER_TAILSCALE_BIN, as a git client
// with a short PATH needs, else on PATH. A configured CLI is the only one.
func TestTailscaleCLI(t *testing.T) {
	fx := fixture{tailscale: full.tailscale}
	t.Run("SUBROUTER_TAILSCALE_BIN, none on PATH", func(t *testing.T) {
		t.Parallel()
		s := newSandbox(t, fx)
		out, err := s.commit("notes.md", "notes\nssh 100.99.8.37\n", "notes")
		refused(t, out, err, "notes.md:2")
	})
	t.Run("PATH", func(t *testing.T) {
		t.Parallel()
		s := newSandbox(t, fx)
		s.env = append(s.env, "SUBROUTER_TAILSCALE_BIN=", "PATH="+filepath.Join(s.root, "fakebin")+":"+filepath.Join(s.root, "tools"))
		out, err := s.commit("notes.md", "notes\nssh 100.99.8.37\n", "notes")
		refused(t, out, err, "notes.md:2")
	})
	t.Run("configured CLI missing", func(t *testing.T) {
		t.Parallel()
		s := newSandbox(t, fx)
		s.env = append(s.env, "SUBROUTER_TAILSCALE_BIN="+filepath.Join(s.root, "no-tailscale"),
			"PATH="+filepath.Join(s.root, "fakebin")+":"+filepath.Join(s.root, "tools"))
		out, err := s.commit("notes.md", "notes\nssh 100.99.8.37\n", "notes")
		passed(t, out, err)
	})
}

// The home's last part is not the login, so only $HOME names it.
func TestHomePath(t *testing.T) {
	s := newSandbox(t, fixture{home: "srv/quillhouse"})
	out, err := s.commit("notes.md", "notes\nfirst\nsecond\nrun "+s.home+"/bin/x\n", "notes")
	refused(t, out, err, "notes.md:4")
	if strings.Contains(out, s.home) {
		t.Errorf("output holds the home path:\n%s", out)
	}
	out, err = s.commit("notes.md", "notes\nrun ~/bin/x\n", "notes")
	passed(t, out, err)
}

// A home path names someone only through its last part, so a home such as
// /home/ubuntu, /root or a short name passes; a distinctive home is refused
// (TestHomePath).
func TestGenericHomePathPasses(t *testing.T) {
	for _, c := range []struct{ login, home string }{{"ubuntu", ""}, {"root", "root"}, {"qa", ""}} {
		t.Run(c.login, func(t *testing.T) {
			t.Parallel()
			s := newSandbox(t, fixture{login: c.login, home: c.home, privateValues: "Fernwood Analytics\n"})
			out, err := s.commit("notes.md", "notes\nrun "+s.home+"/bin/subrouter\n", "notes")
			passed(t, out, err)
			out, err = s.commit("notes.md", "notes\nFernwood Analytics\n", "notes")
			refused(t, out, err, "notes.md:2")
		})
	}
}

func TestNewFilePath(t *testing.T) {
	s := newSandbox(t, full)
	out, err := s.commit("Quillfeather-notes.md", "nothing private\n", "notes")
	refused(t, out, err, "<private>-notes.md: in the path")
}

// A hunk that deletes lines ends, so the hunks and files after it are read
// with their own paths and line numbers.
func TestStagedDiffAfterADeletion(t *testing.T) {
	s := newSandbox(t, fixture{privateValues: "Quillfeather\nFernwood Analytics\n", base: "notes\nold\nkeep\nlast\n"})
	write(t, filepath.Join(s.repo, "notes.md"), "notes\nkeep\nlast\nFernwood Analytics\n")
	write(t, filepath.Join(s.repo, "quillfeather-new.md"), "plain\nFernwood Analytics\n")
	s.mustGit("add", "notes.md", "quillfeather-new.md")
	out, err := s.git("commit", "-q", "-m", "notes")
	refused(t, out, err, "notes.md:4", "<private>-new.md: in the path", "<private>-new.md:2")
}

// The lines after git's "\ No newline at end of file" keep their numbers.
func TestNoNewlineAtEndOfFile(t *testing.T) {
	s := newSandbox(t, fixture{privateValues: "Fernwood Analytics\n", base: "1\n2"})
	out, err := s.commit("notes.md", "1\n2\nFernwood Analytics\n", "notes")
	refusedExactly(t, out, err, "notes.md:3")
}

// A user's git diff settings change nothing: each hook finds the same lines,
// with their numbers, and the same new path. The change has a hunk with
// context lines, a file after it, a move into a path that holds a value, and a
// submodule's new commit, whose lines are the submodule's and not this repo's.
func TestUserDiffSettings(t *testing.T) {
	cases := []struct {
		name       string
		config     []string // key, value, ...
		env        string
		attributes string // .git/info/attributes
	}{
		{name: "diff.interHunkContext", config: []string{"diff.interHunkContext", "3"}},
		{name: "diff.suppressBlankEmpty", config: []string{"diff.interHunkContext", "3", "diff.suppressBlankEmpty", "true"}},
		{name: "GIT_DIFF_OPTS", env: "GIT_DIFF_OPTS=--unified=3"},
		{name: "binary attribute", attributes: "b.md binary\n"},
		// On by default: a move shows as a rename, which has no "new file mode" line.
		{name: "diff.renames", config: []string{"diff.renames", "true"}},
		{name: "color.diff", config: []string{"color.diff", "always"}},
		// An external diff or a textconv driver that prints nothing hides each line.
		{name: "diff.external", config: []string{"diff.external", "/usr/bin/true"}},
		{name: "GIT_EXTERNAL_DIFF", env: "GIT_EXTERNAL_DIFF=/usr/bin/true"},
		{name: "textconv", config: []string{"diff.hide.textconv", "/usr/bin/true"}, attributes: "*.md diff=hide\n"},
		{name: "diff.noprefix", config: []string{"diff.noprefix", "true"}},
		{name: "diff.mnemonicPrefix", config: []string{"diff.mnemonicPrefix", "true"}},
		{name: "diff.submodule", config: []string{"diff.submodule", "diff"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newSandbox(t, fixture{privateValues: "Fernwood Analytics\n"})
			s.commit("a.md", "1\n\n3\n\n5\n\n7\n", "a")
			s.commit("b.md", "x\n", "b")
			s.commit("c.md", "c\n", "c")
			s.mustGit("init", "-q", "sub")
			write(t, filepath.Join(s.repo, "sub", "s.md"), "plain\n")
			s.mustGit("-C", "sub", "add", "s.md")
			s.mustGit("-C", "sub", "commit", "-q", "-m", "s")
			s.mustGit("add", "sub")
			s.mustGit("commit", "-q", "-m", "sub")
			s.mustGit("push", "-q", "origin", "main")
			for i := 0; i < len(c.config); i += 2 {
				s.mustGit("config", c.config[i], c.config[i+1])
			}
			if c.env != "" {
				s.env = append(s.env, c.env)
			}
			if c.attributes != "" {
				write(t, filepath.Join(s.repo, ".git", "info", "attributes"), c.attributes)
			}
			// Two changes with three lines between them, two of them blank, make one hunk.
			write(t, filepath.Join(s.repo, "a.md"), "ONE\n\n3\n\nFernwood Analytics\n\n7\n")
			write(t, filepath.Join(s.repo, "b.md"), "y\nFernwood Analytics\n")
			s.mustGit("mv", "c.md", "Fernwood Analytics.md")
			write(t, filepath.Join(s.repo, "sub", "s.md"), "plain\nFernwood Analytics\n")
			s.mustGit("-C", "sub", "commit", "-q", "-a", "-m", "s")
			s.mustGit("add", "a.md", "b.md", "sub")
			out, err := s.git("commit", "-q", "-m", "notes")
			refusedExactly(t, out, err, "<private>.md: in the path", "a.md:5", "b.md:2")
			s.mustGit("commit", "-q", "--no-verify", "-m", "notes")
			out, err = s.git("push", "-q", "origin", "main")
			refusedExactly(t, out, err, s.head()+" <private>.md: in the path", s.head()+" a.md:5", s.head()+" b.md:2")
		})
	}
}

// A binary file's lines are not checked, as public-scrub --check skips a file
// that holds a NUL byte. Its path is.
func TestBinaryFile(t *testing.T) {
	s := newSandbox(t, fixture{privateValues: "Fernwood Analytics\n"})
	out, err := s.commit("image.bin", "\x00\x01Fernwood Analytics\x00\n", "image")
	passed(t, out, err)
	out, err = s.git("push", "-q", "origin", "main")
	passed(t, out, err)
	// pre-push reads each commit whole, although a binary file's bytes can hold
	// the marker that it puts between commits.
	write(t, filepath.Join(s.repo, "notes.md"), "notes\nFernwood Analytics\n")
	s.mustGit("add", "notes.md")
	s.commit("marker.bin", "\x00\x1e\n", "marker", "--no-verify")
	out, err = s.git("push", "-q", "origin", "main")
	refusedExactly(t, out, err, s.head()+" notes.md:2")
	s.mustGit("reset", "-q", "--hard", "origin/main")
	s.commit("marker.bin", "\x00\x1e\n", "marker")
	out, err = s.git("push", "-q", "origin", "main")
	passed(t, out, err)
	out, err = s.commit("Fernwood Analytics.bin", "\x00\x01\n", "image")
	refused(t, out, err, "<private>.bin: in the path")
}

// The repo's placeholders pass, and so do values that are never real, such as
// a documentation address, even when a source reports them.
func TestPlaceholdersPass(t *testing.T) {
	fx := fixture{
		accounts: `[{"id":"someone@example.com","label":"someone@example.com","email":"someone@example.com"}]`,
		peers: `{"peers":[{"name":"box","url":"https://127.0.0.1:31416","accounts":[{"email":"box@example.org"},
			{"email":"ops@example.net"}]},{"name":"laptop","url":"http://0.0.0.0:31416"}]}`,
		tailscale: `{"TailscaleIPs":["192.0.2.7","2001:db8::3"],"MagicDNSSuffix":"tailnet.example",
			"Self":{"HostName":"laptop","DNSName":"laptop.tailnet.example.","TailscaleIPs":["192.0.2.7","169.254.10.20","fe80::7"]},
			"Peer":{"nodekey:1":{"HostName":"box","TailscaleIPs":["198.51.100.9","203.0.113.10"]}}}`,
		privateValues: "203.0.113.9\nFernwood Analytics\nmirror.corp.invalid\ncache.lan.test\nweb.dev.localhost\n",
	}
	text := "someone@example.com box@example.org https://127.0.0.1:31416 192.0.2.7 [2001:db8::3] " +
		"laptop.tailnet.example 198.51.100.9 203.0.113.10 203.0.113.9 laptop box 100.64.0.1 [fd7a:115c:a1e0::10:1] " +
		"http://0.0.0.0:31416 169.254.10.20 [fe80::7] ops@example.net mirror.corp.invalid cache.lan.test web.dev.localhost\n"
	s := newSandbox(t, fx)
	out, err := s.commit("notes.md", "notes\n"+text, text)
	passed(t, out, err)
	out, err = s.git("push", "-q", "origin", "main")
	passed(t, out, err)
	out, err = s.commit("notes.md", "notes\nFernwood Analytics\n", "notes")
	refused(t, out, err, "notes.md:2")
}

// A hook matches only what a source reports, so a reported tailnet address is
// refused even where it equals a placeholder that the repo writes.
func TestReportedAddressesAreRefused(t *testing.T) {
	s := newSandbox(t, fixture{tailscale: `{"Self":{"HostName":"quills-workstation",
		"TailscaleIPs":["100.88.41.7","100.64.213.9","fd7a:115c:a1e0::10:3"]}}`})
	for _, ip := range []string{"100.88.41.7", "100.64.213.9", "[fd7a:115c:a1e0::10:3]:443"} {
		out, err := s.commit("notes.md", "notes\nssh "+ip+"\n", "notes")
		refused(t, out, err, "notes.md:2")
	}
	out, err := s.commit("notes.md", "notes\nssh 100.64.0.1 or [fd7a:115c:a1e0::10:1]:443\n", "notes")
	passed(t, out, err)
}

// A value with a capital letter outside ASCII matches in each case. İ, whose
// lower case is longer in UTF-8, matches in its own case.
func TestNonASCIICase(t *testing.T) {
	s := newSandbox(t, fixture{privateValues: "Émilie Zorbrecht\nŁukasz\nİlkay\n"})
	for _, form := range []string{"Émilie Zorbrecht", "ÉMILIE ZORBRECHT", "émilie zorbrecht",
		"Łukasz", "ŁUKASZ", "łukasz", "İlkay", "İLKAY"} {
		out, err := s.commit("notes.md", "notes\nby "+form+"\n", "notes")
		refused(t, out, err, "notes.md:2")
	}
	out, err := s.commit("notes.md", "notes\nby Emilie Zorbrecht and Lukasz\n", "notes")
	passed(t, out, err)
}

// public-scrub --check, which the hooks share their matcher with, finds the
// same forms in the files.
func TestPublicScrubCheckNonASCIICase(t *testing.T) {
	s := newSandbox(t, fixture{legacyMap: "label\tÉmilie Zorbrecht\tsomeone\nlabel\tŁukasz\tsomeone-else\nlabel\tİlkay\ta-third\n"})
	write(t, filepath.Join(s.repo, "n.md"), "by Émilie Zorbrecht\nby ÉMILIE ZORBRECHT\nby émilie zorbrecht\n"+
		"by Łukasz\nby ŁUKASZ\nby łukasz\nby Emilie Zorbrecht and Lukasz\nby İlkay\n")
	s.mustGit("add", "n.md")
	cmd := exec.Command(filepath.Join(s.root, "tools", "python3"), filepath.Join(hooksDir(t), "..", "bin", "public-scrub"), "--check")
	cmd.Dir, cmd.Env = s.repo, s.env
	out, err := cmd.CombinedOutput()
	if want := "n.md:1\nn.md:2\nn.md:3\nn.md:4\nn.md:5\nn.md:6\nn.md:8\n"; err == nil || string(out) != want {
		t.Errorf("public-scrub --check: %v, printed %q, want exit 1 and %q", err, out, want)
	}
}

// Short values, generic words and words the base already holds name no one,
// so they pass; a distinctive value from the same source is still refused.
func TestGenericValuesPass(t *testing.T) {
	t.Run("short", func(t *testing.T) {
		s := newSandbox(t, fixture{privateValues: "qx7\nFernwood Analytics\n"})
		out, err := s.commit("notes.md", "notes\nqx7 is a short value\n", "qx7")
		passed(t, out, err)
		out, err = s.commit("notes.md", "notes\nFernwood Analytics\n", "notes")
		refused(t, out, err, "notes.md:2")
	})
	t.Run("generic login and email local part", func(t *testing.T) {
		s := newSandbox(t, fixture{login: "user", email: "user@plantedmail.net"})
		out, err := s.commit("notes.md", "notes\nthe user runs it\n", "for the user")
		passed(t, out, err)
		out, err = s.commit("notes.md", "notes\nmail user@plantedmail.net\n", "notes")
		refused(t, out, err, "notes.md:2")
	})
	t.Run("numbered device name", func(t *testing.T) {
		s := newSandbox(t, fixture{tailscale: `{"Self":{"HostName":"macbook-pro-2",
			"DNSName":"macbook-pro-2.tail0fixture.ts.net."}}`})
		out, err := s.commit("notes.md", "notes\nssh macbook-pro-2\n", "on macbook-pro-2")
		passed(t, out, err)
		out, err = s.commit("notes.md", "notes\nssh macbook-pro-2.tail0fixture.ts.net\n", "notes")
		refused(t, out, err, "notes.md:2")
	})
	t.Run("already in the base", func(t *testing.T) {
		s := newSandbox(t, fixture{privateValues: "Wavecrest\nFernwood Analytics\n", base: "the Wavecrest pattern\n"})
		out, err := s.commit("notes.md", "the Wavecrest pattern\nWavecrest again\n", "Wavecrest")
		passed(t, out, err)
		out, err = s.git("push", "-q", "origin", "main")
		passed(t, out, err)
		out, err = s.commit("notes.md", "notes\nFernwood Analytics\n", "notes")
		refused(t, out, err, "notes.md:2")
	})
	// A push asks the remote, not the local branch, which holds the commits it checks.
	t.Run("only local commits hold it", func(t *testing.T) {
		s := newSandbox(t, fixture{privateValues: "Wavecrest\n"})
		out, err := s.commit("notes.md", "notes\nWavecrest\n", "notes")
		refused(t, out, err, "notes.md:2")
		s.mustGit("commit", "-q", "--no-verify", "-m", "notes")
		out, err = s.commit("notes.md", "notes\nWavecrest\nWavecrest again\n", "notes")
		passed(t, out, err)
		out, err = s.git("push", "-q", "origin", "main")
		refused(t, out, err)
	})
}

// Every source is missing here but the login, which the hooks also read from
// this machine's password database, and the text holds none of its values.
func TestMissingSourcesPassSilently(t *testing.T) {
	s := newSandbox(t, fixture{})
	out, err := s.commit("notes.md", "notes\nplain text\n", "plain")
	passed(t, out, err)
	if out != "" {
		t.Errorf("a commit with no sources printed:\n%s", out)
	}
	out, err = s.git("push", "-q", "origin", "main")
	passed(t, out, err)
	if out != "" {
		t.Errorf("a push with no sources printed:\n%s", out)
	}
}

func TestCache(t *testing.T) {
	s := newSandbox(t, full)
	for i := range 2 {
		out, err := s.commit("notes.md", fmt.Sprintf("notes\n%d\n", i), "notes")
		passed(t, out, err)
	}
	runs, _ := os.ReadFile(filepath.Join(s.root, "tailscale.runs"))
	if got := s.accountReads.Load(); got != 1 || strings.Count(string(runs), "\n") != 1 {
		t.Fatalf("two commits read the daemon %d times and tailscale %d times, want once each", got, strings.Count(string(runs), "\n"))
	}
	dir := filepath.Join(s.home, ".cache", "subrouter-hooks")
	for _, name := range []string{"daemon.json", "tailscale.json"} {
		info, err := os.Stat(filepath.Join(dir, name))
		must(t, err)
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v, want 0600", name, info.Mode().Perm())
		}
	}
	info, err := os.Stat(dir)
	must(t, err)
	if info.Mode().Perm() != 0o700 {
		t.Errorf("%s has mode %v, want 0700", dir, info.Mode().Perm())
	}
	// Eleven minutes on, the daemon is read again.
	path := filepath.Join(dir, "daemon.json")
	var entry map[string]any
	data, err := os.ReadFile(path)
	must(t, err)
	must(t, json.Unmarshal(data, &entry))
	entry["at"] = float64(time.Now().Add(-11 * time.Minute).Unix())
	data, _ = json.Marshal(entry)
	must(t, os.WriteFile(path, data, 0o600))
	out, err := s.commit("notes.md", "notes\n3\n", "notes")
	passed(t, out, err)
	if got := s.accountReads.Load(); got != 2 {
		t.Errorf("a stale cache read the daemon %d times in all, want 2", got)
	}
}

// core.hooksPath skips .git/hooks, so each hook runs the clone's own first,
// with the same arguments and input, and stops when it fails.
func TestCloneHookRunsFirst(t *testing.T) {
	for _, hook := range []string{"pre-commit", "commit-msg", "pre-push"} {
		t.Run(hook, func(t *testing.T) {
			t.Parallel()
			s := newSandbox(t, full)
			record := filepath.Join(s.root, "record")
			own := filepath.Join(s.repo, ".git", "hooks", hook)
			write(t, own, fmt.Sprintf("#!/bin/sh\necho \"args $*\" >> '%s'\n/bin/cat >> '%s'\n[ ! -e '%s.fail' ]\n",
				record, record, record))
			must(t, os.Chmod(own, 0o755))
			run := func() (string, error) {
				if hook == "pre-push" {
					s.commit("notes.md", "notes\n"+time.Now().String()+"\n", "notes", "--no-verify")
					return s.git("push", "-q", "origin", "main")
				}
				return s.commit("notes.md", "notes\n"+time.Now().String()+"\n", "notes")
			}
			out, err := run()
			passed(t, out, err)
			got, _ := os.ReadFile(record)
			want := map[string]string{"pre-commit": "args \n", "commit-msg": "args .git/COMMIT_EDITMSG\n"}[hook]
			if hook == "pre-push" {
				want = "args origin " + filepath.Join(s.root, "remote.git") + "\nrefs/heads/main "
			}
			if !strings.HasPrefix(string(got), want) {
				t.Errorf("the clone's %s got %q, want a prefix %q", hook, got, want)
			}
			write(t, record+".fail", "")
			out, err = run()
			if err == nil {
				t.Errorf("the clone's failing %s did not stop git:\n%s", hook, out)
			}
			// The privacy check runs once the clone's hook passes.
			must(t, os.Remove(record+".fail"))
			switch hook {
			case "pre-push":
				s.mustGit("reset", "-q", "--hard", "origin/main")
				s.commit("notes.md", "notes\nQuillfeather\n", "notes", "--no-verify")
				out, err = s.git("push", "-q", "origin", "main")
				refused(t, out, err, s.head()+" notes.md:2")
			case "commit-msg":
				out, err = s.commit("notes.md", "notes\nplain\n", "Quillfeather")
				refused(t, out, err, "commit message")
			default:
				out, err = s.commit("notes.md", "notes\nQuillfeather\n", "notes")
				refused(t, out, err, "notes.md:2")
			}
		})
	}
}

// commitOnce commits a staged value and wants a single refusal. A hook that
// runs itself would start a new process each time, so it stops all of them
// after 20 s.
func (s *sandbox) commitOnce() {
	s.t.Helper()
	write(s.t, filepath.Join(s.repo, "notes.md"), "notes\nQuillfeather\n")
	s.mustGit("add", "notes.md")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(s.root, "tools", "git"), "commit", "-q", "-m", "notes")
	cmd.Dir, cmd.Env = s.repo, s.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		s.t.Fatalf("the hook did not finish in 20 s:\n%s", out)
	}
	refused(s.t, string(out), err, "notes.md:2")
	if n := strings.Count(string(out), "refused"); n != 1 {
		s.t.Errorf("the hook refused %d times, want once:\n%s", n, out)
	}
}

// Linked from .git/hooks without core.hooksPath, a hook runs once.
func TestSymlinkInstall(t *testing.T) {
	s := newSandbox(t, full)
	s.mustGit("config", "--unset", "core.hooksPath")
	must(t, os.Symlink(filepath.Join(hooksDir(t), "pre-commit"), filepath.Join(s.repo, ".git", "hooks", "pre-commit")))
	s.commitOnce()
}

// A script in .git/hooks that runs a hook here runs once, with core.hooksPath
// and without it.
func TestWrapperInstall(t *testing.T) {
	for _, hooksPath := range []bool{false, true} {
		t.Run(fmt.Sprintf("core.hooksPath %v", hooksPath), func(t *testing.T) {
			t.Parallel()
			s := newSandbox(t, full)
			if !hooksPath {
				s.mustGit("config", "--unset", "core.hooksPath")
			}
			record := filepath.Join(s.root, "record")
			// The script stops after five runs, so that a loop ends.
			wrapper := filepath.Join(s.repo, ".git", "hooks", "pre-commit")
			write(t, wrapper, fmt.Sprintf("#!/bin/sh\necho run >> '%s'\n[ \"$(/usr/bin/wc -l < '%s')\" -le 5 ] || exit 0\nexec '%s' \"$@\"\n",
				record, record, filepath.Join(hooksDir(t), "pre-commit")))
			must(t, os.Chmod(wrapper, 0o755))
			s.commitOnce()
			runs, _ := os.ReadFile(record)
			if n := strings.Count(string(runs), "\n"); n != 1 {
				t.Errorf("one commit ran the script %d times, want once", n)
			}
		})
	}
}

// pre-push takes its base only from the pushed remote, so a word that only
// another remote holds is refused. The upstream counts when it is on the
// pushed remote.
func TestPushBaseIsThePushedRemote(t *testing.T) {
	s := newSandbox(t, fixture{privateValues: "Wavecrest\n", base: "the Wavecrest pattern\n"})
	first := s.head()
	s.mustGit("checkout", "-q", "-b", "topic", "origin/main")
	out, err := s.commit("topic.md", "Wavecrest again\n", "topic")
	passed(t, out, err)
	out, err = s.git("push", "-q", "origin", "topic")
	passed(t, out, err)
	s.mustGit("init", "-q", "--bare", filepath.Join(s.root, "mirror.git"))
	s.mustGit("remote", "add", "mirror", filepath.Join(s.root, "mirror.git"))
	out, err = s.git("push", "-q", "mirror", "topic")
	refused(t, out, err, first+" notes.md:1", s.head()+" topic.md:1")
}

// A user's log settings change nothing for pre-push: it checks a root commit's
// lines despite log.showRoot=false, and each message in UTF-8 despite
// i18n.logOutputEncoding.
func TestUserLogSettings(t *testing.T) {
	t.Run("log.showRoot", func(t *testing.T) {
		t.Parallel()
		s := newSandbox(t, fixture{privateValues: "Fernwood Analytics\n", base: "Fernwood Analytics\n"})
		s.mustGit("config", "log.showRoot", "false")
		root := s.head()
		s.mustGit("init", "-q", "--bare", filepath.Join(s.root, "mirror.git"))
		s.mustGit("remote", "add", "mirror", filepath.Join(s.root, "mirror.git"))
		out, err := s.git("push", "-q", "mirror", "main")
		refusedExactly(t, out, err, root+" notes.md:1")
	})
	t.Run("i18n.logOutputEncoding", func(t *testing.T) {
		t.Parallel()
		s := newSandbox(t, fixture{privateValues: "Émilie Zorbrecht\n"})
		s.mustGit("config", "i18n.logOutputEncoding", "ISO-8859-1")
		s.commit("notes.md", "notes\nmore\n", "by Émilie Zorbrecht", "--no-verify")
		out, err := s.git("push", "-q", "origin", "main")
		refusedExactly(t, out, err, s.head()+" commit message")
	})
}

// A trailer that names the commit's own author repeats the commit header, so
// it passes, in any case; the same email in the body is refused.
func TestOwnIdentityTrailer(t *testing.T) {
	s := newSandbox(t, full)
	out, err := s.commit("notes.md", "notes\nmore\n", "notes\n\nSigned-off-by: Q <gitquill@plantedmail.net>")
	passed(t, out, err)
	out, err = s.commit("notes.md", "notes\nmore\nstill\n", "notes\n\nSigned-off-by: Q <GitQuill@PlantedMail.NET>")
	passed(t, out, err)
	out, err = s.git("push", "-q", "origin", "main")
	passed(t, out, err)
	out, err = s.commit("notes.md", "notes\nagain\n", "notes\n\nask gitquill@plantedmail.net")
	refused(t, out, err, "commit message")
}

// The commit's own author or committer may differ from user.email, as with
// --author or $GIT_COMMITTER_EMAIL, and a trailer that names either passes.
func TestOtherIdentityTrailer(t *testing.T) {
	fx := full
	fx.privateValues += "author.quill@plantedmail.net\ncommitter.quill@plantedmail.net\n"
	s := newSandbox(t, fx)
	out, err := s.commit("notes.md", "notes\nmore\n", "notes\n\nSigned-off-by: A <author.quill@plantedmail.net>",
		"--author=A <author.quill@plantedmail.net>")
	passed(t, out, err)
	s.env = append(s.env, "GIT_COMMITTER_EMAIL=committer.quill@plantedmail.net")
	out, err = s.commit("notes.md", "notes\nmore\nstill\n", "notes\n\nSigned-off-by: C <committer.quill@plantedmail.net>")
	passed(t, out, err)
	out, err = s.git("push", "-q", "origin", "main")
	passed(t, out, err)
	out, err = s.commit("notes.md", "notes\nagain\n", "notes\n\nask author.quill@plantedmail.net")
	refused(t, out, err, "commit message")
}

// A rebased branch's force-push checks its own commits, not the commits of the
// remote's main that it now sits on.
func TestRebasedPushChecksItsOwnCommits(t *testing.T) {
	s := newSandbox(t, full)
	s.mustGit("checkout", "-q", "-b", "topic")
	out, err := s.commit("topic.md", "plain\n", "topic")
	passed(t, out, err)
	s.mustGit("push", "-q", "origin", "topic")
	s.mustGit("checkout", "-q", "main")
	s.commit("leak.md", "Fernwood Analytics\n", "leak", "--no-verify")
	s.mustGit("push", "-q", "--no-verify", "origin", "main")
	s.mustGit("checkout", "-q", "topic")
	s.mustGit("rebase", "-q", "main")
	out, err = s.git("push", "-q", "--force", "origin", "topic")
	passed(t, out, err)
	s.commit("topic.md", "plain\nFernwood Analytics\n", "topic", "--no-verify")
	out, err = s.git("push", "-q", "origin", "topic")
	refused(t, out, err, s.head()+" topic.md:2")
}

// pre-push checks the commits the remote lacks, each one: a value the remote
// already holds is not this push's, and a value a later commit removes is
// still in the history.
func TestPushChecksTheCommitsTheRemoteLacks(t *testing.T) {
	s := newSandbox(t, full)
	s.commit("notes.md", "notes\nQuillfeather\n", "notes", "--no-verify")
	old := s.head()
	s.mustGit("rm", "-q", "notes.md")
	s.mustGit("commit", "-q", "--no-verify", "-m", "remove")
	s.mustGit("push", "-q", "--no-verify", "origin", "main")
	out, err := s.commit("other.md", "plain\n", "plain")
	passed(t, out, err)
	out, err = s.git("push", "-q", "origin", "main")
	passed(t, out, err)

	s.commit("leak.md", "Fernwood Analytics\n", "leak", "--no-verify")
	leak := s.head()
	s.mustGit("rm", "-q", "leak.md")
	s.mustGit("commit", "-q", "-m", "remove")
	out, err = s.git("push", "-q", "origin", "main")
	refused(t, out, err, leak+" leak.md:1")

	s.mustGit("checkout", "-q", "-b", "topic", "origin/main")
	s.commit("topic.md", "Fernwood Analytics\n", "topic", "--no-verify")
	out, err = s.git("push", "-q", "origin", "topic")
	refused(t, out, err, s.head()+" topic.md:1")
	if strings.Contains(out, old) {
		t.Errorf("a new branch's push checked a commit the remote already holds:\n%s", out)
	}
}

// A merge adds the lines that none of its parents has, such as those of a
// conflict's resolution. So a merge of the remote's main passes, although that
// main holds a value, and a value that the merge itself writes is refused.
func TestMerge(t *testing.T) {
	s := newSandbox(t, fixture{privateValues: "Fernwood Analytics\nQuill Harborside\n", base: "1\n2\n3\n"})
	s.mustGit("checkout", "-q", "-b", "topic")
	out, err := s.commit("notes.md", "1\ntopic\n3\n", "topic")
	passed(t, out, err)
	s.mustGit("push", "-q", "origin", "topic")
	s.mustGit("checkout", "-q", "main")
	s.commit("notes.md", "1\nFernwood Analytics\n3\n", "main", "--no-verify")
	s.commit("leak.md", "Fernwood Analytics\n", "leak", "--no-verify")
	s.mustGit("push", "-q", "--no-verify", "origin", "main")
	s.mustGit("checkout", "-q", "topic")
	merge := func(resolution string) (string, error) {
		s.mustGit("reset", "-q", "--hard", "origin/topic")
		s.git("merge", "-q", "main") // notes.md conflicts
		write(t, filepath.Join(s.repo, "notes.md"), resolution)
		s.mustGit("add", "notes.md")
		return s.git("commit", "-q", "--no-edit")
	}
	out, err = merge("1\nFernwood Analytics\ntopic\nQuill Harborside\n3\n")
	refusedExactly(t, out, err, "notes.md:4")
	s.mustGit("commit", "-q", "--no-edit", "--no-verify")
	out, err = s.git("push", "-q", "origin", "topic")
	refusedExactly(t, out, err, s.head()+" notes.md:4")
	out, err = merge("1\nFernwood Analytics\ntopic\n3\n")
	passed(t, out, err)
	out, err = s.git("push", "-q", "origin", "topic")
	passed(t, out, err)
}

// A merge's added lines keep their numbers where the merge drops lines on both
// sides of a line that both parents hold, even under a user's diff.context of
// 0, which gives the -U0 diff that numbers them wrong.
func TestMergeLineNumbers(t *testing.T) {
	s := newSandbox(t, fixture{privateValues: "Fernwood Analytics\n", base: "x\nkeep\ny\n"})
	s.mustGit("config", "diff.context", "0")
	s.mustGit("checkout", "-q", "-b", "topic")
	s.commit("notes.md", "t1\nkeep\nt2\n", "topic")
	s.mustGit("checkout", "-q", "main")
	s.commit("notes.md", "m1\nkeep\nm2\n", "main")
	s.mustGit("checkout", "-q", "topic")
	s.git("merge", "-q", "main") // notes.md conflicts
	write(t, filepath.Join(s.repo, "notes.md"), "keep\nFernwood Analytics\n")
	s.mustGit("add", "notes.md")
	out, err := s.git("commit", "-q", "--no-edit")
	refusedExactly(t, out, err, "notes.md:2")
	s.mustGit("commit", "-q", "--no-edit", "--no-verify")
	out, err = s.git("push", "-q", "origin", "topic")
	refusedExactly(t, out, err, s.head()+" notes.md:2")
}

// pre-push checks the message of each pushed annotated tag, and of the tags it
// points to.
func TestPushedTagMessage(t *testing.T) {
	s := newSandbox(t, full)
	s.mustGit("tag", "-a", "v1", "-m", "release for Fernwood Analytics")
	v1 := strings.TrimSpace(s.mustGit("rev-parse", "v1"))[:12]
	out, err := s.git("push", "-q", "origin", "v1")
	refused(t, out, err, v1+" tag message")
	s.mustGit("tag", "-a", "v2", "-m", "plain release", "v1")
	out, err = s.git("push", "-q", "origin", "v2")
	refused(t, out, err, v1+" tag message")
	s.mustGit("tag", "-a", "v3", "-m", "plain release")
	out, err = s.git("push", "-q", "origin", "v3")
	passed(t, out, err)
}

// pre-push checks the name of each branch or tag that the push adds to the
// remote. A branch the remote already has is not this push's.
func TestNewRefName(t *testing.T) {
	s := newSandbox(t, full)
	out, err := s.git("push", "-q", "origin", "main:quillfeather-fix")
	refused(t, out, err, "refs/heads/<private>-fix: in the ref name")
	out, err = s.git("push", "-q", "origin", "main:topic")
	passed(t, out, err)
	s.mustGit("push", "-q", "--no-verify", "origin", "main:quillfeather-fix")
	s.commit("notes.md", "notes\nmore\n", "notes")
	out, err = s.git("push", "-q", "origin", "main:quillfeather-fix")
	passed(t, out, err)
	// A push that deletes a branch adds nothing to the remote.
	out, err = s.git("push", "-q", "origin", "--delete", "quillfeather-fix")
	passed(t, out, err)
}

// After the editor, git strips comment lines from the message, so commit-msg
// skips them too and pre-push checks the message as git stored it. With -m,
// git keeps them, so commit-msg checks them. core.commentChar sets the mark.
func TestCommitMsgCommentLines(t *testing.T) {
	for _, mark := range []string{"#", ";"} {
		t.Run(mark, func(t *testing.T) {
			t.Parallel()
			s := newSandbox(t, full)
			if mark != "#" {
				s.mustGit("config", "core.commentChar", mark)
			}
			editor := filepath.Join(s.root, "editor")
			write(t, editor, "#!/bin/sh\nprintf 'subject\\n"+mark+" Quillfeather\\n' > \"$1\"\n")
			must(t, os.Chmod(editor, 0o755))
			s.env = append(s.env, "GIT_EDITOR="+editor)
			write(t, filepath.Join(s.repo, "notes.md"), "notes\nmore\n")
			s.mustGit("add", "notes.md")
			out, err := s.git("commit", "-q")
			passed(t, out, err)
			out, err = s.git("push", "-q", "origin", "main")
			passed(t, out, err)
			out, err = s.commit("notes.md", "notes\nagain\n", "subject\n\n"+mark+"Quillfeather")
			refused(t, out, err, "commit message")
		})
	}
}

// git commit -v puts the staged diff below the scissors line, and git drops
// it from the message, so a diff that removes a value passes.
func TestVerboseCommitSkipsTheDiff(t *testing.T) {
	s := newSandbox(t, full)
	s.commit("notes.md", "notes\nFernwood Analytics\n", "leak", "--no-verify")
	s.mustGit("push", "-q", "--no-verify", "origin", "main")
	editor := filepath.Join(s.root, "editor")
	write(t, editor, "#!/bin/sh\n{ echo 'remove a line'; /bin/cat \"$1\"; } > \"$1.new\" && /bin/mv \"$1.new\" \"$1\"\n")
	must(t, os.Chmod(editor, 0o755))
	s.env = append(s.env, "GIT_EDITOR="+editor)
	write(t, filepath.Join(s.repo, "notes.md"), "notes\n")
	s.mustGit("add", "notes.md")
	out, err := s.git("commit", "-q", "-v")
	passed(t, out, err)
	out, err = s.git("push", "-q", "origin", "main")
	passed(t, out, err)
}
