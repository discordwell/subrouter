//go:build !windows

// Package deploytests runs deploy-scripts-test.sh (the shell harness for
// lightsail/deploy-box.sh, lightsail/bootstrap.sh, lightsail/push-box.sh,
// macos/deploy-laptop.sh, macos/rollback-laptop-peer.sh and bin/sr-triage) as
// one Go subtest per case, so the harness runs through the same sandboxed
// `go test` wrapper as the rest of subrouter (ops/bin/sr-local-ci deploy). On
// Linux (CI, Docker) it also runs on its own; on macOS only sandboxed (see
// requireFence):
//
//	ops/bin/sr-local-ci --docker deploy
//	cd ops && DEPLOY_TESTS_SANDBOX=1 go test -count=1 -run 'TestDeployScripts/laptop-ok$' ./deploy/tests
//
// ops/ is its own Go module (ops/go.mod), outside the subrouter module on
// purpose: `go test ./...` at the repo root never picks it up, and nothing
// here conflicts with an upstream merge. The harness stubs every command that
// could touch a real machine (sudo, systemctl, launchctl, curl, codesign, ...)
// and aborts a case before the script runs if any of them resolves outside its
// stub directory, and moves every fixed path the box scripts touch under the
// case's directory. It also checks that PATCHES.md heads each numbered section
// once.
package deploytests

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Every case that pins a guard. Removing one from the harness fails this test,
// so a guard cannot lose its test silently.
var requiredCases = []string{
	// deploy-box.sh: the admin token from the env file, the private listener
	// from SUBROUTER_BOX_ADDR or the env file and never guessed, and step 7's
	// floor from the pool before the swap.
	"box-default-ok",
	"box-admin-token-from-env-file",
	"box-admin-token-dash-without-one-refused",
	"box-missing-address-refused",
	"box-malformed-address-refused",
	"box-address-host-with-a-path-refused",
	"box-unbracketed-ipv6-address-refused",
	"box-bracketed-ipv6-address-ok",
	"box-address-from-the-environment",
	"box-small-pool-ok",
	"box-empty-pool-ok",
	"box-pool-shrank-rolls-back",
	// deploy-box.sh runs detached, so a lost ssh connection stops only the
	// streaming of its log.
	"box-lost-connection-deploy-finishes",
	"box-lost-connection-still-rolls-back",
	"box-closed-output-deploy-finishes",
	"box-lost-connection-attest-first-starts-the-daemon",
	"box-second-deploy-refused-while-one-runs",
	"box-refused-while-a-bootstrap-runs",
	"box-binary-path-argument",
	// deploy-box.sh step 8: the public listener refuses the admin token and
	// 404s every private route; its port is the env file's, and one the env
	// file names that is down rolls back; with public admin on (PATCHES
	// §43.1), what the new daemon logged decides, and the flag form is
	// refused; no pipe into grep -q (§43.3).
	"box-public-admin-off-still-demands-404",
	"box-public-port-variable-ignored",
	"box-configured-public-listener-down-rolls-back",
	"box-private-only-skips-the-public-checks",
	"box-public-admin-env-ok",
	"box-scripts-never-pipe-into-grep-q",
	"box-public-admin-long-journal",
	"box-public-listener-among-many",
	"box-public-admin-dropin-ok",
	"box-public-admin-reset-unmounted-warns",
	"box-public-admin-asked-not-confirmed-warns",
	"box-public-admin-on-but-unmounted-warns",
	"box-public-admin-flag-refused",
	"box-public-admin-flag-false-refused",
	"box-public-admin-static-token-rolls-back",
	"box-public-admin-import-token-rolls-back",
	"box-public-admin-transcripts-mounted-rolls-back",
	"box-public-admin-unmarked-route-rolls-back",
	// deploy-box.sh step 8: public /ready 503 startup_ready:false is accepted;
	// draining and not-served still roll back; step 9 only warns.
	"box-ready-startup-503-accepted",
	"box-ready-startup-then-flips",
	"box-ready-draining-rolls-back",
	"box-ready-missing-rolls-back",
	"box-ready-draining-with-startup-key-rolls-back",
	// deploy-box.sh step 10: the isolation gate, as the service user.
	"box-isolation-incomplete",
	"box-isolation-check-cannot-run",
	"box-unit-env-honored",
	// deploy-box.sh rollback() and the step-1 downgrade guard: refused only
	// once a Codex record has been migrated.
	"box-rollback-refused-pre-isolation",
	"box-rollback-allowed-by-override",
	"box-rollback-refused-service-down",
	"box-rollback-no-migrated-records",
	"box-rollback-interactive-only-records",
	"box-rollback-refused-store-unreadable",
	"box-rollback-refused-unit-state-dir",
	"box-rollback-post-to-post-unaffected",
	"box-downgrade-refused",
	"box-downgrade-allowed-by-override",
	"box-downgrade-no-migrated-records",
	// deploy-box.sh's restarts, step 4's and rollback()'s, drain over the drain
	// socket first and force a build still draining after a wall-clock grace
	// (PATCHES §38.1).
	"box-restart-bounded",
	"box-rollback-restart-bounded",
	"box-drain-grace-is-wall-clock",
	"box-drain-grace-forces-at-the-deadline",
	"box-drain-grace-default-is-the-ceiling",
	"box-drain-grace-ceiling-is-the-unit-timeout",
	"box-drain-grace-ceiling-is-the-daemon-timeout",
	"box-drain-grace-clamped-to-the-ceiling",
	"box-drain-grace-overflow-clamped",
	"box-drain-grace-not-a-number-refused",
	"box-drains-over-the-socket-first",
	"box-drain-socket-reports-the-drain",
	"box-no-drain-socket-falls-back",
	"box-drain-socket-off",
	"box-drain-socket-off-any-spelling",
	"box-drain-socket-relative-path-ignored",
	"box-failed-restart-resumes-the-drain",
	"box-operator-drain-is-not-lifted",
	"box-drain-report-seconds-leading-zero",
	"box-drain-report-seconds-not-a-number-refused",
	"box-ready-wait-not-a-number-refused",
	"box-drain-report-hung-socket-does-not-delay-the-force",
	"box-drain-grace-fractional-unit-timeout",
	"box-drain-grace-fractional-daemon-timeout",
	"box-drain-grace-sub-second-timeout-is-one-second",
	"box-drain-grace-hard-cap",
	"box-rollback-grace-defaults-short",
	"box-rollback-grace-forces-the-abandoned-build",
	"box-rollback-grace-not-a-number-refused",
	"box-rollback-drains-the-new-build-over-its-socket",
	// deploy-box.sh ATTEST_FIRST=1.
	"box-attest-first",
	"box-attest-first-says-the-grace-does-not-apply",
	"box-attest-first-rollback-refused",
	"box-attest-first-preflight-ok-rolls-back",
	"box-attest-first-nothing-to-attest",
	"box-attest-first-preflight-cannot-run",
	"box-attest-first-stop-stuck",
	"box-attest-first-pre-isolation-candidate",
	// bootstrap.sh: the isolation-crossing guard, the box's name, the laptop
	// configuration it prints, the tokens a re-run keeps, and the checks a
	// build passes before it replaces the installed one.
	"boot-refuses-isolation-crossing",
	"boot-crossing-allowed-by-override",
	"boot-same-era-unaffected",
	"boot-names-the-tailnet-host",
	"boot-default-name-is-box",
	"boot-bad-box-name-refused",
	"boot-prints-the-laptop-configuration",
	"boot-node-id-unknown-prints-where-to-find-it",
	"boot-public-ip-lookup-failure-still-finishes",
	"boot-rerun-keeps-the-tokens",
	"boot-refused-while-a-deploy-runs",
	"boot-unrunnable-build-refused",
	"boot-unguarded-build-refused",
	// push-box.sh: nothing is sent without a box, a build and values the box's
	// shell cannot misread; the box is told the sha and "-", and its exit code
	// comes back.
	"push-deploy-ok",
	"push-each-push-gets-its-own-directory",
	"push-deploy-returns-the-box-exit-code",
	"push-missing-host-refused",
	"push-host-from-the-environment",
	"push-host-flag-beats-the-environment",
	"push-host-that-is-an-option-refused",
	"push-missing-build-refused",
	"push-unknown-verb-refused",
	"push-deploy-passes-only-its-knobs",
	"push-knob-the-box-shell-would-read-refused",
	"push-bootstrap-ok",
	"push-failed-upload-runs-nothing",
	"push-lost-connection-names-the-box-log",
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
	// The lease helper never leaves a run waiting on it for ever (PATCHES §83).
	"laptop-detached-run-waits-for-a-slow-lease-helper",
	"laptop-lease-helper-that-never-answers-is-stopped",
	"laptop-killed-run-frees-the-lease",
	"lease-release-stops-a-helper-that-ignores-term",
	"laptop-restart-survives-a-killed-foreground",
	"laptop-failed-install-leaves-no-stage-file",
	"laptop-failed-bootstrap-rolls-back-at-once",
	// The laptop scripts change launchd only for a job they can load again
	// (PATCHES §76).
	"laptop-refuses-a-job-loaded-from-another-plist",
	"laptop-stops-before-kickstarting-a-job-from-another-plist",
	"laptop-reload-refused-without-its-plist",
	"laptop-plist-gone-after-the-bootout-is-not-bootstrapped",
	"peer-rollback-refuses-a-job-loaded-from-another-plist",
	"act-refuses-a-job-loaded-from-another-plist",
	// A refusal at the restart puts back what a deploy or an activation
	// installed, or says how to; the harness signals only its own runs; and
	// one case reads the scripts' default label and address (PATCHES §76.6).
	"laptop-refusal-that-cannot-put-the-build-back-says-how",
	"act-refusal-at-the-restart-puts-the-files-back",
	"harness-cleanup-signals-only-its-own-runs",
	"scripts-default-to-the-live-job",
	// A deploy or a check never says OK while the daemon does not own its
	// Claude socket, and triage, claude-route and an activation's rollback
	// name a stand-in there (PATCHES §84).
	"sup-stand-in-at-the-claude-socket-is-not-the-daemon",
	"sup-check-stand-in-the-supervisor-does-not-ask-back",
	"sup-claude-socket-waiting-is-not-deploy-ok",
	"sup-claude-socket-displaced-is-not-deploy-ok",
	"sup-claude-socket-yielded-says-until-when",
	"laptop-stand-in-at-the-claude-socket-is-not-the-daemon",
	"triage-tells-a-stand-in-from-the-daemon",
	"route-status-and-socket-on-tell-a-stand-in-from-the-daemon",
	"act-rollback-tells-a-stand-in-from-the-daemon",
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
	// A credential command manages the store its own environment names (PATCHES §73).
	"route-login-keeps-the-profile-store",
	"route-login-in-a-pool-session-drops-the-pool",
	"route-login-off-keeps-the-profile-store",
	"route-every-credential-command-routes-credentials",
	"route-logout-of-a-pool-profile-is-refused",
	"route-plain-pool-launch-keeps-the-pool-store",
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

// On a Mac the real launchctl is one PATH entry behind the harness's stubs. A
// stopped plain `go test ./...` here once left a detached deploy-laptop.sh
// running after its stubs were deleted, and it unloaded the developer's live
// service (PATCHES §76). The test label now keeps such a run off the live job,
// and on macOS the cases still run only under a sandbox that refuses exec of
// launchctl: the harness's own (DEPLOY_TESTS_SANDBOX=1), or sr-local-ci's,
// which says so with SR_LOCAL_CI_SANDBOX=1 (a sandbox-exec inside it would be
// refused). Linux has no launchctl, so CI and Docker runs are unchanged. The
// variables only say which sandbox; the harness itself checks that it refuses
// launchctl before any case runs.
func requireFence(t *testing.T) {
	t.Helper()
	if why := fenceRefusal(runtime.GOOS, os.Getenv); why != "" {
		t.Fatal(why)
	}
}

// The ways to run the cases that the refusal names, and the docs too.
var safeDeployTestCommands = []struct{ command, where string }{
	{"ops/bin/sr-local-ci --docker deploy", "Linux, in Docker"},
	{"ops/bin/sr-local-ci deploy", "this Mac, sandboxed"},
	{"cd ops && DEPLOY_TESTS_SANDBOX=1 go test -count=1 ./deploy/tests", "this Mac, each script sandboxed"},
}

func fenceRefusal(goos string, getenv func(string) string) string {
	if goos != "darwin" || getenv("DEPLOY_TESTS_SANDBOX") == "1" || getenv("SR_LOCAL_CI_SANDBOX") == "1" {
		return ""
	}
	why := `refusing to run the deploy script cases on macOS outside a sandbox: they drive scripts that call launchctl, and a stopped plain "go test" once unloaded the live subrouter service (ops/PATCHES.md §76). Run them with one of:`
	for _, c := range safeDeployTestCommands {
		why += fmt.Sprintf("\n\t%-66s (%s)", c.command, c.where)
	}
	return why
}

func TestDeployCasesRefuseAMacOutsideASandbox(t *testing.T) {
	only := func(name string) func(string) string {
		return func(key string) string {
			if key == name {
				return "1"
			}
			return ""
		}
	}
	why := fenceRefusal("darwin", only(""))
	if why == "" {
		t.Fatal("a plain go test on macOS would run the deploy cases")
	}
	for _, c := range safeDeployTestCommands {
		if !strings.Contains(why, c.command) {
			t.Errorf("the refusal does not name %q:\n%s", c.command, why)
		}
	}
	for _, name := range []string{"DEPLOY_TESTS_SANDBOX", "SR_LOCAL_CI_SANDBOX"} {
		if why := fenceRefusal("darwin", only(name)); why != "" {
			t.Errorf("%s=1 is a sandboxed run, and it was refused: %s", name, why)
		}
	}
	if why := fenceRefusal("linux", only("")); why != "" {
		t.Errorf("a Linux run was refused: %s", why)
	}
}

// Run by itself on macOS, the harness refuses too, and it does not take a
// variable's word for a sandbox: it execs launchctl as a case would and runs
// only when that is refused (PATCHES §76.6). Here Linux stands in for the Mac:
// uname says Darwin, and a stand-in sandbox-exec refuses /bin/launchctl, or
// runs it.
func TestDeployHarnessRefusesAMacOutsideASandbox(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("stands in for macOS on Linux; on a Mac the harness checks its real sandbox")
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	harness := filepath.Join(wd, "deploy-scripts-test.sh")
	realID, err := exec.LookPath("id")
	if err != nil {
		t.Fatal(err)
	}
	const one = "laptop-refuses-a-job-loaded-from-another-plist"
	refusing := `if [ "$3" = /bin/launchctl ]; then echo "sandbox-exec: execvp() of '/bin/launchctl' failed: Operation not permitted" >&2; exit 71; fi
shift 2; exec "$@"`
	for _, c := range []struct {
		name    string
		env     []string
		sandbox string // the stand-in sandbox-exec's body; "" for none on PATH
		filter  []string
		refusal string // "" when the case runs
	}{
		{"nothing says sandboxed", nil, "", []string{"=" + one}, "neither DEPLOY_TESTS_SANDBOX=1 nor SR_LOCAL_CI_SANDBOX=1 is set"},
		{"nothing says sandboxed, every case", nil, "", nil, "neither DEPLOY_TESTS_SANDBOX=1 nor SR_LOCAL_CI_SANDBOX=1 is set"},
		{"DEPLOY_TESTS_SANDBOX=1 with no sandbox-exec", []string{"DEPLOY_TESTS_SANDBOX=1"}, "", []string{"=" + one}, "there is no sandbox-exec on PATH"},
		{"DEPLOY_TESTS_SANDBOX=1 in a sandbox that runs launchctl", []string{"DEPLOY_TESTS_SANDBOX=1"}, `shift 2; exec "$@"`, []string{"=" + one}, "exec of /bin/launchctl was not refused"},
		{"SR_LOCAL_CI_SANDBOX=1 outside sr-local-ci's sandbox", []string{"SR_LOCAL_CI_SANDBOX=1"}, "", []string{"=" + one}, "exec of /bin/launchctl was not refused"},
		{"DEPLOY_TESTS_SANDBOX=1 in a sandbox that refuses launchctl", []string{"DEPLOY_TESTS_SANDBOX=1"}, refusing, []string{"=" + one}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			bin, tmp := t.TempDir(), t.TempDir()
			calls := filepath.Join(bin, "sandbox-calls")
			write := func(name, body string) {
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			write("uname", "echo Darwin")
			// With DEPLOY_TESTS_SANDBOX=1 the harness needs the home of `id -un`.
			// The uid of the Docker run in ops/README.md has no passwd entry, so
			// no name; root has one on every Linux.
			write("id", fmt.Sprintf(`if [ "$*" = -un ]; then echo root; else exec %q "$@"; fi`, realID))
			if c.sandbox != "" {
				write("sandbox-exec", fmt.Sprintf("echo \"$*\" >> %q\n%s", calls, c.sandbox))
			}
			cmd := exec.Command("bash", append([]string{harness}, c.filter...)...)
			cmd.Dir = wd
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "TMPDIR="+tmp, "KEEP=",
				"DEPLOY_TESTS_JOBS=1", "DEPLOY_TESTS_SANDBOX=", "SR_LOCAL_CI_SANDBOX=")
			cmd.Env = append(cmd.Env, c.env...)
			out, err := cmd.CombinedOutput()
			text := string(out)
			if c.refusal == "" {
				if err != nil || !strings.Contains(text, "\n  ok   "+one+"\n") {
					t.Fatalf("a sandbox that refuses launchctl was refused (%v):\n%s", err, text)
				}
				ran, _ := os.ReadFile(calls)
				if !strings.Contains(string(ran), "/bin/launchctl version") || !strings.Contains(string(ran), "deploy-laptop.sh") {
					t.Fatalf("the probe and the case's script did not both go through sandbox-exec:\n%s", ran)
				}
				return
			}
			if code := cmd.ProcessState.ExitCode(); code != 2 {
				t.Errorf("exit %d, want 2:\n%s", code, text)
			}
			for _, want := range []string{"refusing to run the deploy script cases on macOS", c.refusal,
				"ops/bin/sr-local-ci --docker deploy", "DEPLOY_TESTS_SANDBOX=1 bash ops/deploy/tests/deploy-scripts-test.sh"} {
				if !strings.Contains(text, want) {
					t.Errorf("the refusal lacks %q:\n%s", want, text)
				}
			}
			if left, _ := filepath.Glob(filepath.Join(tmp, "deploy-tests*")); len(left) > 0 || strings.Contains(text, "  ok   ") {
				t.Errorf("a case started before the refusal (%v):\n%s", left, text)
			}
		})
	}
}

// AGENTS.md told agents to run go test before handing off, and the Test
// section of ops/README.md listed plain go test for these cases (PATCHES §76).
// The harness's own usage lines and the box README's Test section gave the
// harness and go test without a sandbox too (PATCHES §76.6).
var (
	plainOpsGoTest = regexp.MustCompile(`cd ops && go test|go test[^\n]*\./deploy/tests`)
	plainHarness   = regexp.MustCompile(`\bbash\s+\S*deploy-scripts-test\.sh`)
)

func TestDocsNameTheSafeDeployTestCommands(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(wd, "..", "..", "..")
	read := func(rel string) string {
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(src)
	}
	readme, agents := read("ops/README.md"), read("AGENTS.md")
	for _, c := range safeDeployTestCommands {
		if !strings.Contains(readme, c.command) {
			t.Errorf("ops/README.md does not name %q", c.command)
		}
	}
	if docker := safeDeployTestCommands[0].command; !strings.Contains(agents, docker) {
		t.Errorf("AGENTS.md does not name %q", docker)
	}
	// Of the harness, only its comments: the usage lines are there.
	comments := strings.Split(read("ops/deploy/tests/deploy-scripts-test.sh"), "\n")
	for i, line := range comments {
		if !strings.HasPrefix(line, "#") {
			comments[i] = ""
		}
	}
	for name, doc := range map[string]string{"ops/README.md": readme, "AGENTS.md": agents,
		"ops/deploy/lightsail/README.md":          read("ops/deploy/lightsail/README.md"),
		"ops/deploy/tests/deploy-scripts-test.sh": strings.Join(comments, "\n")} {
		for i, line := range strings.Split(doc, "\n") {
			if (plainOpsGoTest.MatchString(line) || plainHarness.MatchString(line)) && !strings.Contains(line, "DEPLOY_TESTS_SANDBOX=1") {
				t.Errorf("%s:%d runs the deploy cases outside a sandbox: %s", name, i+1, line)
			}
		}
	}
}

func TestDeployScripts(t *testing.T) {
	requireFence(t)
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

// A detached run ignores HUP, INT and TERM and outlives the harness that
// started it. One outlived its test dir, lost its stubs and drove the real
// launchctl (PATCHES §76). So the harness, stopped as a background task is
// stopped (a signal to its process group), kills its detached runs before it
// removes the dir. DEPLOY_TESTS_HOLD=1 parks laptop-ok's detached run in its
// kickstart, which writes its process group to <test dir>/hold.held.
func TestStoppedHarnessKillsItsDetachedRuns(t *testing.T) {
	requireFence(t)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	harness := filepath.Join(wd, "deploy-scripts-test.sh")
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			var out syncBuffer
			cmd := exec.Command("bash", harness, "=laptop-ok")
			cmd.Dir = wd
			cmd.Env = append(os.Environ(), "DEPLOY_TESTS_JOBS=1", "KEEP=", "TMPDIR="+dir, "DEPLOY_TESTS_HOLD=1")
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Stdout, cmd.Stderr = &out, &out
			cmd.WaitDelay = 10 * time.Second
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()

			pgid, err := awaitHeld(dir, heldStall, done)
			if err != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				t.Fatalf("%v:\n%s", err, out.String())
			}
			if pgid <= 1 {
				t.Fatalf("hold.held names process group %d", pgid)
			}
			t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })

			if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(2 * time.Minute):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				<-done
				t.Fatalf("the harness had not ended 2 minutes after %v:\n%s", sig, out.String())
			}
			// A killed process is gone once init reaps it.
			for deadline := time.Now().Add(10 * time.Second); syscall.Kill(-pgid, 0) == nil; time.Sleep(50 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("the detached run (process group %d) outlived the harness stopped with %v:\n%s", pgid, sig, out.String())
				}
			}
			if left, _ := filepath.Glob(filepath.Join(dir, "deploy-tests.*")); len(left) > 0 {
				t.Fatalf("the harness stopped with %v left %v", sig, left)
			}
		})
	}
}

// The detached run parks in its kickstart and writes its process group to
// hold.held under dir. A loaded machine can take many minutes to get there: at
// a load near 60, two subtests ran out a fixed 5 minutes in a full run and
// passed alone (PATCHES §76.6). So the wait lasts while the run makes
// progress, a file under dir changing, and gives up once nothing has changed
// for stall.
const heldStall = 3 * time.Minute

func awaitHeld(dir string, stall time.Duration, ended <-chan error) (int, error) {
	last, progress := "", time.Now()
	for {
		if held, _ := filepath.Glob(filepath.Join(dir, "deploy-tests.*", "hold.held")); len(held) == 1 {
			if b, err := os.ReadFile(held[0]); err == nil {
				if pgid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
					return pgid, nil
				}
			}
		}
		select {
		case err := <-ended:
			return 0, fmt.Errorf("the harness ended (%v) before its detached run reached the kickstart", err)
		default:
		}
		if now := treeState(dir); now != last {
			last, progress = now, time.Now()
		} else if time.Since(progress) > stall {
			return 0, fmt.Errorf("the detached run never reached the kickstart: nothing under %s changed for %v", dir, stall)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// How many files are under dir, their total size and their latest change.
func treeState(dir string) string {
	n, size, latest := 0, int64(0), int64(0)
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			n, size = n+1, size+info.Size()
			latest = max(latest, info.ModTime().UnixNano())
		}
		return nil
	})
	return fmt.Sprint(n, size, latest)
}

func TestHeldWaitFollowsTheRunsProgress(t *testing.T) {
	const stall = 1500 * time.Millisecond
	t.Run("a slow run that keeps going", func(t *testing.T) {
		run := filepath.Join(t.TempDir(), "deploy-tests.x")
		if err := os.Mkdir(run, 0o755); err != nil {
			t.Fatal(err)
		}
		go func() {
			// Twice and more the stall in all, each step well inside it.
			for i := 1; i <= 40; i++ {
				_ = os.WriteFile(filepath.Join(run, "log"), []byte(strings.Repeat("x", i)), 0o644)
				time.Sleep(100 * time.Millisecond)
			}
			_ = os.WriteFile(filepath.Join(run, "hold.next"), []byte("4242\n"), 0o644)
			_ = os.Rename(filepath.Join(run, "hold.next"), filepath.Join(run, "hold.held"))
		}()
		if pgid, err := awaitHeld(filepath.Dir(run), stall, nil); err != nil || pgid != 4242 {
			t.Fatalf("awaitHeld = %d, %v; want 4242 from a run that kept going", pgid, err)
		}
	})
	t.Run("a run that stops", func(t *testing.T) {
		run := filepath.Join(t.TempDir(), "deploy-tests.x")
		if err := os.MkdirAll(run, 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(run, "log"), []byte("x"), 0o644)
		if _, err := awaitHeld(filepath.Dir(run), stall, nil); err == nil || !strings.Contains(err.Error(), "changed for") {
			t.Fatalf("awaitHeld on a run that stopped: %v, want the stall", err)
		}
	})
	t.Run("a harness that ends first", func(t *testing.T) {
		ended := make(chan error, 1)
		ended <- fmt.Errorf("exit status 1")
		if _, err := awaitHeld(t.TempDir(), stall, ended); err == nil || !strings.Contains(err.Error(), "harness ended") {
			t.Fatalf("awaitHeld after the harness ended: %v", err)
		}
	})
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Every launchctl call that changes a job is in laptop-deploy-lib.sh's
// kickstart_job, bootout_and_wait or bootstrap_job, which first check that the
// job is the script's own (PATCHES §76). A call anywhere else skips that check.
// A call is launchctl, by name or by path, where a command starts: at the
// start of a line, after ; & | ( ) { } ! a backquote or $(, or after a
// keyword; behind any of command, exec, env, nohup, nice, time, sudo and
// timeout with their options, an option's value included, and behind variable
// assignments. Its verb is
// one that loads, unloads, starts, stops or signals a job (PATCHES §76.6). A
// line that ends in a backslash goes on into the next.
var (
	launchdChange = regexp.MustCompile(`(?:^|[;&|(){}!` + "`" + `]|\$\(|\b(?:then|do|else|if|elif|while|until)\b)\s*` +
		`(?:(?:[A-Za-z_][A-Za-z0-9_]*=\S*|(?:\S*/)?(?:command|exec|env|nohup|nice|time|sudo|timeout)|-\S+(?:\s+(?:"[^"]*"|'[^']*'|[^-\s]\S*))?|[0-9][0-9.]*[smhd]?)\s+)*` +
		`["']?(?:\S*/)?launchctl["']?\s+["']?(?:kickstart|bootout|bootstrap|load|unload|remove|kill|stop|start|enable|disable|submit)\b`)
	shellFunc    = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\(\) *\{`)
	guardedCalls = map[string]bool{"kickstart_job": true, "bootout_and_wait": true, "bootstrap_job": true}
)

type launchdCall struct {
	line     int // the first line of the call
	function string
	text     string
}

// Each call in a shell script that changes a launchd job, with the function it
// is in.
func launchdChanges(src string) []launchdCall {
	var calls []launchdCall
	fn := ""
	lines := strings.Split(src, "\n")
	for i := 0; i < len(lines); i++ {
		first, line := i, lines[i]
		for strings.HasSuffix(line, `\`) && i+1 < len(lines) {
			i++
			line = strings.TrimSuffix(line, `\`) + " " + lines[i]
		}
		if m := shellFunc.FindStringSubmatch(line); m != nil {
			fn = m[1]
		} else if strings.HasPrefix(line, "}") {
			fn = ""
		}
		if strings.HasPrefix(strings.TrimSpace(line), "#") || !launchdChange.MatchString(line) {
			continue
		}
		calls = append(calls, launchdCall{first + 1, fn, strings.TrimSpace(line)})
	}
	return calls
}

func TestLaptopScriptsChangeLaunchdOnlyThroughTheGuard(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	scripts, err := filepath.Glob(filepath.Join(wd, "..", "macos", "*.sh"))
	if err != nil || len(scripts) == 0 {
		t.Fatalf("found no laptop scripts beside the harness: %v", err)
	}
	guarded := map[string]bool{}
	for _, path := range scripts {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range launchdChanges(string(src)) {
			if filepath.Base(path) == "laptop-deploy-lib.sh" && guardedCalls[c.function] {
				guarded[c.function] = true
				continue
			}
			t.Errorf("%s:%d changes a launchd job outside kickstart_job, bootout_and_wait and bootstrap_job, so it skips their check that the job is this script's own: %s",
				filepath.Base(path), c.line, c.text)
		}
	}
	// A pattern that matches nothing would pass every script.
	for fn := range guardedCalls {
		if !guarded[fn] {
			t.Errorf("found no launchctl call in %s; the pattern no longer matches laptop-deploy-lib.sh", fn)
		}
	}
}

// The independent check of §76 found that its first pattern missed most of
// these.
func TestLaunchdChangeScanCatchesEverySpelling(t *testing.T) {
	for _, line := range []string{
		`launchctl bootout "$LABEL"`,
		`    out=$(launchctl bootout "$LABEL" 2>&1)`,
		`if launchctl bootout "$LABEL"; then`,
		`elif launchctl kickstart -k "$LABEL"; then`,
		`! launchctl bootstrap "gui/$(id -u)" "$PLIST"`,
		`if ! launchctl bootout "$LABEL"; then`,
		`while launchctl bootout "$LABEL"; do`,
		`until launchctl bootstrap gui/501 "$PLIST"; do`,
		`/bin/launchctl kickstart -k "$LABEL"`,
		`command launchctl bootout "$LABEL"`,
		`exec launchctl bootout "$LABEL"`,
		`env -i PATH=/bin launchctl bootout "$LABEL"`,
		`/usr/bin/env launchctl bootout "$LABEL"`,
		`timeout 10 launchctl bootout "$LABEL"`,
		`timeout -k 5 10s launchctl bootout "$LABEL"`,
		`LC_ALL=C launchctl bootout "$LABEL"`,
		`sudo -n launchctl bootout "$LABEL"`,
		`timeout -s KILL 10 launchctl bootout "$LABEL"`,
		`timeout --signal KILL 10 launchctl bootout "$LABEL"`,
		`env -u HOME launchctl bootout "$LABEL"`,
		`sudo -u "$USER" launchctl bootout "$LABEL"`,
		`sudo -p 'a prompt: ' launchctl bootout "$LABEL"`,
		`nice -n 5 launchctl kickstart -k "$LABEL"`,
		`nohup launchctl kickstart -k "$LABEL" &`,
		`check && launchctl kickstart -k "$LABEL"`,
		`check || launchctl bootstrap gui/501 "$PLIST"`,
		`{ launchctl bootout "$LABEL"; }`,
		"x=`launchctl bootout \"$LABEL\"`",
		`case $a in x) launchctl bootout "$LABEL" ;; esac`,
		`launchctl unload "$PLIST"`,
		`launchctl load -w "$PLIST"`,
		`launchctl remove "$JOB"`,
		`launchctl kill TERM "$LABEL"`,
		`launchctl stop "$JOB"`,
		`launchctl start "$JOB"`,
		`launchctl disable "$LABEL"`,
		`launchctl enable "$LABEL"`,
		`launchctl submit -l x -- /bin/true`,
	} {
		if got := launchdChanges(line); len(got) != 1 {
			t.Errorf("not taken for a launchd change: %s", line)
		}
	}
	src := "x\nkickstart_job() {\n    launchctl \\\n        bootout \"$LABEL\"\n}\n"
	if got := launchdChanges(src); len(got) != 1 || got[0].line != 3 || got[0].function != "kickstart_job" {
		t.Errorf("a call continued on the next line: %+v, want one call at line 3 in kickstart_job", got)
	}
	for _, line := range []string{
		`echo "     launchctl bootstrap gui/$(id -u) $PLIST" >&2`,
		`echo "If launchctl refused (rc 141, 153 or 125), this shell is the problem" >&2`,
		`LAUNCHD_ERR="launchctl bootstrap gui/$(id -u) $PLIST failed: rc $rc"`,
		`launchctl print "$LABEL" >/dev/null 2>&1; rc=$?`,
		`mgr=$(launchctl managername 2>&1); rc=$?`,
		`    launchctl_hint`,
		`notify -u "$USER" launchctl bootout "$LABEL"`,
		`# launchctl bootout "$LABEL"`,
	} {
		if got := launchdChanges(line); len(got) != 0 {
			t.Errorf("taken for a launchd change: %s", line)
		}
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

// A section the ops tools cite in a message or comment must be in the ledger.
var (
	citedSection  = regexp.MustCompile(`PATCHES(?:\.md)? §([0-9]+)`)
	ledgerSection = regexp.MustCompile(`(?m)^## ([0-9]+)\. `)
)

func TestOpsToolsCiteOnlySectionsThatExist(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ops := filepath.Join(wd, "..", "..")
	ledger, err := os.ReadFile(filepath.Join(ops, "PATCHES.md"))
	if err != nil {
		t.Fatal(err)
	}
	heads := map[string]bool{}
	for _, m := range ledgerSection.FindAllStringSubmatch(string(ledger), -1) {
		heads[m[1]] = true
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
				for _, m := range citedSection.FindAllStringSubmatch(line, -1) {
					cited++
					if !heads[m[1]] {
						rel, _ := filepath.Rel(ops, path)
						t.Errorf("%s:%d cites PATCHES §%s, which ops/PATCHES.md has no section for", rel, i+1, m[1])
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if cited == 0 {
		t.Fatal("found no cited section in ops/bin or ops/deploy; the pattern no longer matches")
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
