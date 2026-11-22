//go:build !windows

package deploytests

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// The setup tools' cases (setup-tools-test.sh): subrouter-remote-key,
// subrouter-unbrick and subrouter-quickstart. Removing one from the harness
// fails this test, so a behaviour cannot lose its test silently.
var requiredSetupToolCases = []string{
	// The harness fails a case whose stubs were handed the live job's label or
	// port (PATCHES §76).
	"harness-fails-a-stub-handed-the-live-job",
	// subrouter-remote-key: the key goes where the gate reads it, and --check
	// applies the gate's rules (PATCHES §17).
	"remote-key-mints-where-the-gate-reads",
	"remote-key-creates-a-private-store",
	"remote-key-honours-the-state-dir",
	"remote-key-mints-into-the-legacy-store",
	"remote-key-legacy-store-after-the-import",
	"remote-key-check-applies-the-gate-rules",
	"remote-key-keeps-a-key-without-force",
	"remote-key-ignored-key-is-not-success",
	"remote-key-unreachable-store-is-not-a-key",
	"remote-key-refuses-a-directory",
	"remote-key-remove",
	"remote-key-no-launchagent-no-restart-hint",
	"remote-key-restart-hint-with-launchagent",
	"remote-key-supervised-hint-deploys-the-worker",
	"remote-key-hint-names-the-daemon-store",
	// subrouter-unbrick: takes Claude Code and Codex off the daemon, where the
	// Go writers put them, and can be undone.
	"unbrick-removes-only-the-anthropic-keys",
	"unbrick-disables-every-codex-routing-key",
	"unbrick-follows-claude-config-dir-and-codex-home",
	"unbrick-reports-shell-exports-and-edits-none",
	"unbrick-leaves-a-settings-json-it-cannot-read",
	"unbrick-check-touches-nothing",
	"unbrick-restore-puts-back-the-newest-backup",
	// subrouter-quickstart: it runs on macOS only; --peer stops before
	// anything is built unless the daemon's store holds remote.key; setup is
	// confirmed; a running daemon is upgraded only by deploy-laptop.sh; no
	// client goes to an empty pool or to a daemon that has not logged a turn;
	// the cutover is proved, and put back exactly when its proof fails.
	"quickstart-refuses-off-macos",
	"quickstart-peer-refuses-without-remote-key",
	"quickstart-peer-stops-before-build",
	"quickstart-peer-needs-a-token-file",
	"quickstart-peer-https-needs-a-fingerprint",
	"quickstart-peer-key-mode-refused",
	"quickstart-peer-key-too-short",
	"quickstart-peer-with-key-proceeds",
	"quickstart-peer-key-where-the-daemon-reads-it",
	"quickstart-peer-key-from-plist-state-dir",
	"quickstart-peer-plist-state-dir-survives-setup",
	"quickstart-peer-plist-home-replaced-by-setup",
	"quickstart-peer-key-from-the-worker-config",
	"quickstart-peer-key-in-the-legacy-store",
	"quickstart-peer-defers-to-a-peers-file",
	"quickstart-peer-key-before-the-peers-file",
	"quickstart-peers-file-is-a-pool",
	"quickstart-peer-step-rechecks-the-plist-it-writes",
	"quickstart-peer-step-writes-with-key",
	"quickstart-peer-step-writes-the-plist-it-is-given",
	"quickstart-peer-restart-waits-for-launchd",
	"quickstart-peer-rerun-leaves-a-peered-daemon-alone",
	"quickstart-peer-restart-needs-the-gui-session",
	"quickstart-peer-failed-bootstrap-says-the-daemon-is-down",
	"quickstart-peer-step-takes-the-mutation-lock",
	"quickstart-peer-restart-waits-for-a-slow-worker",
	"quickstart-peer-token-never-world-readable",
	"quickstart-peer-key-not-a-file",
	"quickstart-peer-key-unreadable",
	"quickstart-preflight-refuses-shell-exports",
	"quickstart-preflight-refuses-a-broken-settings-json",
	"quickstart-no-peer-needs-no-key",
	"quickstart-go-version-read-in-the-module",
	"quickstart-go-local-toolchain-below-go-mod",
	"quickstart-go-failed-fetch-stops",
	"quickstart-go-stalled-fetch-is-bounded",
	"quickstart-install-confirms-setup",
	"quickstart-upgrade-names-the-stale-daemon",
	"quickstart-upgrade-reads-the-supervised-worker",
	"quickstart-upgrade-same-build-is-quiet",
	"quickstart-upgrade-same-commit-is-quiet",
	"quickstart-accounts-empty-pool-needs-a-human",
	"quickstart-prove-needs-the-local-proxy-token",
	"quickstart-prove-needs-a-fresh-log-line",
	"quickstart-prove-names-a-quiet-log-level",
	"quickstart-prove-reads-the-rotated-log",
	"quickstart-prove-accepts-a-peer-turn",
	"quickstart-prove-needs-the-marker",
	"quickstart-prove-goes-through-the-daemon-with-its-token",
	"quickstart-reproof-reads-only-settings-json",
	"quickstart-cutover-writes-the-daemon-and-its-token",
	"quickstart-local-base-url-in-the-go-form",
	"quickstart-cutover-keeps-the-token-private",
	"quickstart-cutover-respects-a-chosen-cache-ttl",
	"quickstart-cutover-follows-claude-config-dir",
	"quickstart-cutover-revert-removes-a-file-it-created",
	"quickstart-cutover-revert-ignores-older-backups",
	"quickstart-cutover-revert-restores-the-file-byte-for-byte",
	"quickstart-cutover-linked-settings-json",
	"quickstart-cutover-revert-on-a-timed-out-reproof",
	"quickstart-rollback-after-fresh-mac-cutover",
	"quickstart-rollback-leaves-codex-alone",
	"quickstart-rollback-skips-backups-on-the-daemon",
	"quickstart-rollback-puts-a-link-back",
	"quickstart-rerun-over-a-routed-link",
	"quickstart-rollback-without-a-backup-off-the-daemon",
	"quickstart-rerun-keeps-the-pre-install-backup",
	"quickstart-rerun-names-a-cut-over-backup",
	"quickstart-done-says-where-sr-is",
	"quickstart-done-sr-on-path",
}

// On macOS the harness runs each tool inside sandbox-exec, which refuses exec
// of launchctl. Without it only the stubs keep a tool off this Mac's launchd,
// and a stopped deploy test once outlived its stubs and unloaded the live
// service (PATCHES §76). So, as for the deploy cases, a Mac runs these cases
// only sandboxed.
func setupToolsFenceRefusal(goos string, getenv func(string) string, lookPath func(string) (string, error)) string {
	if goos != "darwin" {
		return ""
	}
	var why string
	if v := getenv("SETUP_TOOLS_SANDBOX"); v != "" && v != "1" {
		why = "SETUP_TOOLS_SANDBOX=" + v + " turns off the harness's sandbox"
	} else if _, err := lookPath("sandbox-exec"); err != nil {
		why = "there is no sandbox-exec on PATH for the harness's sandbox"
	} else {
		return ""
	}
	return "refusing to run the setup tool cases on macOS outside a sandbox: " + why +
		", which refuses exec of launchctl (ops/PATCHES.md §76). Leave SETUP_TOOLS_SANDBOX unset, or run them on Linux."
}

func TestSetupToolsRefuseAMacOutsideTheirSandbox(t *testing.T) {
	sandbox := func(value string) func(string) string {
		return func(key string) string {
			if key == "SETUP_TOOLS_SANDBOX" {
				return value
			}
			return ""
		}
	}
	found := func(string) (string, error) { return "/usr/bin/sandbox-exec", nil }
	missing := func(string) (string, error) { return "", exec.ErrNotFound }
	for _, c := range []struct {
		run     string
		goos    string
		value   string
		look    func(string) (string, error)
		refused bool
	}{
		{"a Mac, by default", "darwin", "", found, false},
		{"a Mac, SETUP_TOOLS_SANDBOX=1", "darwin", "1", found, false},
		{"a Mac, SETUP_TOOLS_SANDBOX=0", "darwin", "0", found, true},
		{"a Mac without sandbox-exec", "darwin", "", missing, true},
		{"Linux, SETUP_TOOLS_SANDBOX=0", "linux", "0", missing, false},
	} {
		why := setupToolsFenceRefusal(c.goos, sandbox(c.value), c.look)
		if c.refused && why == "" {
			t.Errorf("%s would run the cases", c.run)
		}
		if !c.refused && why != "" {
			t.Errorf("%s was refused: %s", c.run, why)
		}
	}
}

// TestSetupTools runs each case of setup-tools-test.sh as its own subtest, on
// macOS only inside the harness's sandbox (setupToolsFenceRefusal):
//
//	cd ops && go test -count=1 -run TestSetupTools ./deploy/tests
//	cd ops && go test -count=1 -run 'TestSetupTools/remote-key-remove$' ./deploy/tests
func TestSetupTools(t *testing.T) {
	if why := setupToolsFenceRefusal(runtime.GOOS, os.Getenv, exec.LookPath); why != "" {
		t.Fatal(why)
	}
	for _, tool := range []string{"bash", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required to run the setup tool harness: %v", tool, err)
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	harness := filepath.Join(wd, "setup-tools-test.sh")
	src, err := os.ReadFile(harness)
	if err != nil {
		t.Fatalf("read harness: %v", err)
	}
	seen := map[string]bool{}
	var cases []string
	for _, m := range caseLine.FindAllStringSubmatch(string(src), -1) {
		if seen[m[1]] {
			t.Fatalf("case %q is defined twice in %s", m[1], harness)
		}
		seen[m[1]] = true
		cases = append(cases, m[1])
	}
	for _, name := range requiredSetupToolCases {
		if !seen[name] {
			t.Errorf("harness lost the case %q", name)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	assertions := regexp.MustCompile(`setup tool tests: [1-9][0-9]* assertions passed, 0 failed`)
	slots := make(chan struct{}, 6)
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			slots <- struct{}{}
			defer func() { <-slots }()
			cmd := exec.Command("bash", harness, "="+name)
			cmd.Dir = wd
			cmd.Env = append(os.Environ(), "SETUP_TOOLS_JOBS=1", "KEEP=")
			out, err := cmd.CombinedOutput()
			text := string(out)
			if err != nil {
				t.Fatalf("case %s failed: %v\n%s", name, err, text)
			}
			// A zero exit is not enough: require the case's own verdict line
			// and a non-zero assertion count.
			if !strings.Contains(text, "\n  ok   "+name+"\n") {
				t.Fatalf("case %s exited 0 without its ok verdict:\n%s", name, text)
			}
			if !assertions.MatchString(text) {
				t.Fatalf("case %s ran no assertions:\n%s", name, text)
			}
		})
	}
}
