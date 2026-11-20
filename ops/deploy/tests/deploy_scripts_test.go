//go:build !windows

// Package deploytests runs deploy-scripts-test.sh (the shell harness for
// macos/deploy-laptop.sh, macos/rollback-laptop-peer.sh and bin/sr-triage) as
// one Go subtest per case, so the harness runs through the same sandboxed
// `go test` wrapper as the rest of subrouter (ops/bin/sr-local-ci deploy), or
// on its own:
//
//	cd ops && go test -count=1 ./deploy/tests
//	cd ops && go test -count=1 -run 'TestDeployScripts/laptop-ok$' ./deploy/tests
//
// ops/ is its own Go module (ops/go.mod), outside the subrouter module on
// purpose: `go test ./...` at the repo root never picks it up, and nothing
// here conflicts with an upstream merge. The harness stubs every command that
// could touch a real machine (sudo, systemctl, launchctl, curl, codesign, ...)
// and aborts a case before the script runs if any of them resolves outside its
// stub directory. It also checks that PATCHES.md heads each numbered section
// once.
package deploytests

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every case that pins a guard. Removing one from the harness fails this test,
// so a guard cannot lose its test silently.
var requiredCases = []string{
	// deploy-laptop.sh.
	"laptop-ok",
	"laptop-isolation-incomplete",
	"laptop-isolation-check-cannot-run",
	"laptop-plist-state-dir-honored",
	"laptop-rollback-refused-verify",
	"laptop-rollback-allowed-verify",
	"laptop-rollback-refused-down",
	"laptop-rollback-allowed-down",
	"laptop-rollback-post-to-post-unaffected",
	"laptop-rollback-down-no-migrated-records",
	"laptop-rollback-interactive-only-records",
	"laptop-rollback-refused-store-unreadable",
	"laptop-rollback-refused-store-hidden",
	"laptop-rollback-refused-plist-state-dir",
	"laptop-downgrade-refused",
	"laptop-downgrade-no-migrated-records",
	"laptop-check-incomplete",
	"laptop-check-ok",
	"laptop-installs-where-the-job-runs-not-subrouter-bin",
	// deploy-laptop.sh from a shell that cannot restart the daemon (a terminal
	// from a login session that has ended, or one outside the GUI session) is
	// refused before anything changes; restart() waits for print's rc 113 only,
	// shows launchctl's failures, and passes only when a NEW pid serves
	// (PATCHES §47).
	"laptop-dead-launchd-refused",
	"laptop-dead-launchd-reload-refused",
	"laptop-background-session-refused",
	"laptop-print-refused-is-not-unloaded",
	"laptop-check-notes-dead-launchd",
	"laptop-kickstart-noop-falls-back",
	"laptop-kickstart-error-falls-back",
	"laptop-reload-same-pid-fails",
	"laptop-reload-same-build-ok",
	"laptop-unload-wait-needs-rc-113",
	"laptop-bootstrap-error-is-shown",
	// deploy-laptop.sh under the mutation lock, its restart in a detached run,
	// and its failed installs and refused bootstraps (PATCHES §69).
	"laptop-refused-while-the-mutation-lock-is-held",
	"laptop-back-to-back-runs-take-the-lock",
	"laptop-restart-survives-a-killed-foreground",
	"laptop-failed-install-leaves-no-stage-file",
	"laptop-failed-bootstrap-rolls-back-at-once",
	// deploy-laptop.sh step 7: old backups go only after the routes verify,
	// this run's never; pre-isolation leftovers are named once a record is
	// migrated; the checkout srq runs from is compared with the
	// deployed binary's tree and moved only forward, to a merged commit, when
	// clean, detached and holding it (PATCHES §40).
	"laptop-prunes-old-backups",
	"laptop-prune-keeps-this-runs-backup",
	"laptop-keep-backups-zero-prunes-nothing",
	"laptop-keep-backups-near-misses-prune-nothing",
	"laptop-rollback-prunes-nothing",
	"laptop-check-prunes-nothing",
	"laptop-names-pre-isolation-binaries",
	"laptop-pre-isolation-backup-quiet-before-migration",
	"laptop-refreshes-stale-ops-checkout",
	"laptop-ops-checkout-on-branch-not-moved",
	"laptop-ops-checkout-dirty-not-moved",
	"laptop-ops-checkout-missing-commit-not-moved",
	"laptop-ops-local-commit-not-orphaned",
	"laptop-ops-never-moves-backwards",
	"laptop-ops-unmerged-commit-not-moved",
	"laptop-ops-compares-with-the-binarys-tree",
	"laptop-ops-binary-outside-a-checkout",
	"laptop-ops-check-reports-only",
	"laptop-ops-current-and-broken",
	"laptop-ops-copied-srq",
	"laptop-ops-srq-on-path",
	"laptop-ops-link-into-another-repo",
	"laptop-ops-binary-in-another-repo",
	// deploy-laptop.sh on a job launchd runs under supervise (PATCHES §69).
	"sup-upgrade-ok",
	"sup-upgrade-refused-restores-binary",
	"sup-verify-regressed-rolls-back-by-upgrade",
	"sup-wrong-generation-rolls-back",
	"sup-upgrade-timeout-after-the-switch-is-kept",
	"sup-upgrade-timeout-without-the-switch-restores",
	"sup-upgrade-unknown-worker-restores",
	"sup-same-build-refused-upgrade-fails",
	"sup-upgrade-survives-a-killed-foreground",
	"sup-never-ready-upgrade-survives-a-killed-foreground",
	"sup-reload-refused",
	"sup-restart-supervisor",
	"sup-restart-supervisor-rolls-back-both",
	"sup-restart-supervisor-refused-unsupervised",
	"sup-refuses-build-without-supervised-socket",
	"sup-refuses-a-build-from-main",
	"sup-restart-supervisor-refuses-a-build-from-main",
	"sup-check-ok",
	"sup-check-wrong-generation",
	"sup-claude-socket-through-the-supervisor",
	"sup-claude-socket-down-names-the-supervisor-state",
	"sup-restart-supervisor-refused-outside-the-gui-session",
	"sup-upgrade-runs-outside-the-gui-session",
	"sup-upgrade-refused-from-an-ended-session",
	"sup-not-loaded-bootstraps-before-upgrading",
	"sup-isolation-gate-reads-the-worker-config-env",
	"sup-reads-the-loaded-job-not-the-plist",
	"sup-upgrade-refused-while-the-mutation-lock-is-held",
	"sup-check-runs-while-the-mutation-lock-is-held",
	"sup-restart-supervisor-survives-a-killed-foreground",
	"sup-loaded-supervisor-plist-says-serve-refused",
	"sup-loaded-serve-plist-says-supervise-refused",
	// rollback-laptop-peer.sh.
	"peer-rollback-refused-pre-isolation",
	"peer-rollback-refused-attested-transfer",
	"peer-rollback-allowed-by-override",
	"peer-rollback-pre-to-pre-unaffected",
	"peer-rollback-restores-where-the-job-runs-not-subrouter-bin",
	"peer-rollback-refuses-a-backup-plist-for-another-binary",
	"peer-rollback-no-migrated-records",
	"peer-rollback-refused-store-unreadable",
	// rollback-laptop-peer.sh: the same refusal, before the first file is
	// restored, and the same restart rules (PATCHES §47).
	"peer-rollback-dead-launchd-refused",
	"peer-rollback-background-session-refused",
	"peer-rollback-unload-wait-needs-rc-113",
	"peer-rollback-bootstrap-error-is-shown",
	"peer-rollback-same-pid-is-not-healthy",
	"peer-rollback-print-refused-is-not-unloaded",
	"peer-rollback-daemon-down-ok",
	"peer-rollback-job-unloaded-ok",
	// rollback-laptop-peer.sh on a supervised job, under the lock, detached (PATCHES §69).
	"peer-rollback-refused-supervised",
	"peer-rollback-supervised-forced",
	"peer-rollback-refused-loaded-supervisor",
	"peer-rollback-refused-while-the-mutation-lock-is-held",
	"peer-rollback-stops-the-job-before-restoring-files",
	"peer-rollback-survives-a-killed-foreground",
	"peer-rollback-writes-the-plist-after-the-bootout",
	// activate-laptop-supervisor.sh (PATCHES §69).
	"act-dry-run-touches-nothing",
	"act-ok",
	"act-without-peer-tailnet",
	"act-worker-is-where-the-job-runs-not-subrouter-bin",
	"act-already-supervised-is-a-no-op",
	"act-verify-fails-rolls-back",
	"act-socket-not-served-rolls-back",
	"act-keeps-an-existing-worker-config",
	"act-refuses-build-without-supervised-socket",
	"act-refuses-a-build-from-main",
	"act-refuses-pre-isolation-live-binary",
	"act-refuses-a-worker-owned-socket-flag",
	"act-wait-quiet-waits-for-the-turns",
	"act-wait-quiet-gives-up",
	"act-wait-quiet-checks-again-before-the-bootout",
	"act-wait-quiet-gives-up-before-the-bootout",
	"act-rollback-on-request",
	"act-survives-a-killed-foreground",
	"act-rollback-survives-a-killed-foreground",
	"act-rerun-loads-a-supervised-plist-nothing-loaded",
	"act-activates-a-serve-plist-nothing-loaded",
	"act-rollback-rerun-loads-a-serve-plist-nothing-loaded",
	"act-rollback-finishes-a-half-done-rollback",
	"act-refuses-a-supervised-plist-under-a-serve-job",
	"act-rollback-stops-the-job-before-restoring-files",
	"act-waits-say-how-long",
	"act-route-regression-rolls-back",
	"act-route-silent-before-and-after-is-kept",
	"act-route-identity-regression-rolls-back",
	"act-baselines-before-the-bootout",
	"act-rollback-keeps-later-plist-edits",
	"act-rollback-keeps-the-current-worker",
	"act-rollback-keeps-the-worker-config-args",
	"act-rollback-puts-no-binary-back",
	"act-undo-restores-every-file-and-says-which-failed",
	"act-refuses-when-the-manifest-cannot-be-written",
	"act-rollback-skips-a-backup-without-a-manifest",
	"act-rollback-waits-for-the-claude-socket",
	"act-rollback-says-a-reload-is-not-healthy",
	"act-rollback-says-which-files-it-left",
	"act-failed-activation-stops-the-job-before-restoring-files",
	"act-refuses-outside-the-gui-session",
	"act-refused-from-an-ended-session",
	"act-rollback-refused-outside-the-gui-session",
	"act-failed-bootstrap-rolls-back-at-once",
	"act-refused-while-the-mutation-lock-is-held",
	"act-undo-removes-the-supervisor-it-installed",
	"act-rollback-after-a-failed-re-sign-loads-the-job",
	"act-rollback-that-cannot-remove-the-supervisor-loads-the-job",
	"act-rollback-that-cannot-restore-the-plist-loads-the-supervised-job",
	"act-rollback-on-request-that-cannot-write-the-plist-loads-the-supervised-job",
	"act-rollback-with-a-refused-bootstrap-names-the-command",
	"act-rollback-on-request-with-a-refused-bootstrap-names-the-command",
	// bin/claude-route (PATCHES §69).
	"route-socket-on-refused-supervised",
	"route-socket-off-refused-supervised",
	"route-socket-off-supervised-forced",
	"route-socket-on-supervised-forced-restores",
	"route-socket-on-leaves-the-plist-to-the-lock-holder",
	"route-socket-on-consent-holds-when-the-job-turns-supervised",
	"route-socket-on-refused-loaded-supervisor",
	"route-link-finds-the-scripts-in-the-checkout",
	"route-plain-copy-says-where-the-scripts-are",
	"route-shims-keep-max-retries",
	// sr-triage: rc-file exports by name only, probes bounded without
	// timeout(1), the install found off PATH (§48).
	"triage-rc-exports-names-only",
	"triage-runs-without-timeout",
	"triage-bounds-a-hung-sr",
	"triage-stopped-takes-its-probe-with-it",
	"triage-finds-sr-in-home-bin",
	"triage-finds-the-launchagent-binary",
	"triage-never-installed-is-missing",
}

var caseLine = regexp.MustCompile(`(?m)^if begin ([A-Za-z0-9_-]+); then$`)

func TestDeployScripts(t *testing.T) {
	for _, tool := range []string{"bash", "python3", "shasum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required to run the deploy script harness: %v", tool, err)
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	harness := filepath.Join(wd, "deploy-scripts-test.sh")
	src, err := os.ReadFile(harness)
	if err != nil {
		t.Fatalf("read harness (run this test from the module, with the file path; see the package doc): %v", err)
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
	for _, name := range requiredCases {
		if !seen[name] {
			t.Errorf("harness lost the case %q, which pins a deploy guard", name)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	// Exec latency, not CPU, bounds these; more than a handful at once only
	// makes each case slower.
	slots := make(chan struct{}, 6)
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			slots <- struct{}{}
			defer func() { <-slots }()
			cmd := exec.Command("bash", harness, "="+name)
			cmd.Dir = wd
			// The harness itself runs every script under `env -i`; this only
			// keeps a caller's KEEP/DEPLOY_TESTS_* knobs from changing a case.
			cmd.Env = append(os.Environ(), "DEPLOY_TESTS_JOBS=1", "KEEP=")
			out, err := cmd.CombinedOutput()
			text := string(out)
			if err != nil {
				t.Fatalf("case %s failed: %v\n%s", name, err, text)
			}
			// A zero exit is not enough on its own: require the case's own
			// verdict line and a non-zero assertion count.
			if !strings.Contains(text, "\n  ok   "+name+"\n") {
				t.Fatalf("case %s exited 0 without its ok verdict:\n%s", name, text)
			}
			if !regexp.MustCompile(`deploy script tests: [1-9][0-9]* assertions passed, 0 failed`).MatchString(text) {
				t.Fatalf("case %s ran no assertions:\n%s", name, text)
			}
		})
	}
}

// PATCHES.md is the ledger that commits, code comments and the other docs cite
// by section number, so a number must lead to one section. A docs commit once
// appended §47 twice, the first copy running into a second copy of the end of
// §46 (PATCHES §47.6), so a grep for either heading found two answers. This
// package is the one outside the subrouter module that CI runs (the "Test the
// ops tools" step in .github/workflows/ci.yml), so the ledger's check lives
// here. The level is part of the key, so a sub-section may reuse its
// section's number one level down.
var patchesHeading = regexp.MustCompile(`^(#{2,4}) ([0-9]+(?:\.[0-9]+)*[a-z]?)\.? `)

func TestPatchesLedgerHeadsEachSectionOnce(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(wd, "..", "..", "PATCHES.md")
	src, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatalf("read the ledger (run this test from the module, with the file path; see the package doc): %v", err)
	}
	first := map[string]int{}
	levels := map[string]bool{}
	for i, line := range strings.Split(string(src), "\n") {
		m := patchesHeading.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		levels[m[1]] = true
		key := m[1] + " " + m[2]
		if at, ok := first[key]; ok {
			t.Errorf("PATCHES.md heads %q twice, at lines %d and %d", key, at, i+1)
			continue
		}
		first[key] = i + 1
	}
	// Without a numbered ## heading the pattern has stopped matching the
	// ledger, and a check that matches nothing finds no duplicate. The
	// condensed ledger folds sub-sections into their section's body, so
	// ### headings are optional.
	if !levels["##"] {
		t.Fatalf("found no numbered ## heading in %s; the pattern no longer matches the ledger", ledger)
	}
}

// Every file the ops tools cite in their messages and comments must be at
// the repo root or under ops/.
var citedFile = regexp.MustCompile(`[\w./-]*\w\.(?:md|ya?ml)\b`)

func TestOpsToolsCiteOnlyFilesThatExist(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ops := filepath.Join(wd, "..", "..")
	root := filepath.Dir(ops)
	exists := func(path string) bool {
		_, err := os.Stat(path)
		return err == nil
	}
	cited := 0
	for _, dir := range []string{"bin", "deploy"} {
		err := filepath.WalkDir(filepath.Join(ops, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(src), "\n") {
				for _, name := range citedFile.FindAllString(line, -1) {
					cited++
					if !exists(filepath.Join(root, name)) && !exists(filepath.Join(ops, name)) {
						rel, _ := filepath.Rel(root, path)
						t.Errorf("%s:%d cites %s, which is neither at the repo root nor under ops/", rel, i+1, name)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// A pattern that matches nothing would pass every tree.
	if cited == 0 {
		t.Fatal("found no cited file in ops/bin or ops/deploy; the pattern no longer matches")
	}
}
