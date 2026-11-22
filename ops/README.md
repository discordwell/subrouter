# ops

Tools and notes for running this fork on a Mac, kept for the redesign.

| Path | What it does |
|---|---|
| `bin/srq` | Answers questions about the local daemon without its traps: `srq serving`, `srq pool`, `srq selftest`. |
| `bin/sr-plot` | Shows each account's 7-day burn pace in the terminal. `sr plot` runs it. |
| `bin/srdash` | Shows every pool this machine can reach on one page. |
| `bin/srprobe` | Sends a request shaped like Claude Code's through a daemon. |
| `bin/sr-triage` | Explains, read-only, why `sr` does nothing. |
| `bin/sr-local-ci` | Runs the Go suite locally, isolated from the live pool, logins, and keychain. |
| `bin/codexmode`, `bin/claudemode` | Start Codex or Claude Code against the pool that has capacity. |
| `bin/claude-route` | Switches new Claude Code and Codex launches between the pool and your own login, and turns the daemon's Claude socket on and off. |
| `bin/public-scrub` | Replaces private values with placeholders from a map kept outside the repo. `--check` lists any that remain. |
| `skills/revive-workflow-agents/` | A Claude Code skill and `wf_revive.py`: continues workflow agents that died in an outage from their saved transcripts, and starts their runs again without redoing finished stages. |
| `publiccheck/` | Fails CI on a tailnet address outside the placeholder blocks. It holds no private values. |
| `deploy/macos/activate-laptop-supervisor.sh` | Moves the laptop job under `subrouter supervise` once, and `--rollback` moves it back. |
| `deploy/macos/deploy-laptop.sh` | Deploys a build to this Mac, checks its routes, and rolls back on failure. Under the supervisor it upgrades the worker without a restart. |
| `deploy/macos/rollback-laptop-peer.sh` | Puts the previous laptop build back. |
| `deploy/macos/laptop-deploy-lib.sh` | The route checks, launchd checks, lock and detached run that the laptop scripts share. |
| `deploy/tests/` | Tests for the laptop scripts, `claude-route` and `sr-triage`, and a rehearsal against a scratch LaunchAgent. |
| `LAPTOP.md` | How to move the laptop under the supervisor, deploy, roll back, and recover. |
| `PATCHES.md` | Every change against upstream and why, §1 to §76, condensed. |
| `KNOWN-ISSUES.md` | Problems and misleading meters, with their status, condensed. |

## Run the tools

Put `ops/bin` from a detached checkout of `origin/main` on `PATH`. After a deploy, `deploy-laptop.sh` moves that checkout forward to the deployed commit, but only when the commit is on `origin/main`.

## Test

The deploy cases drive scripts that call `launchctl`, kept from the real one by stubs on `PATH`. A stopped plain `go test ./...` in `ops/` on a Mac once left a detached `deploy-laptop.sh` running after its stubs were deleted, and it unloaded the developer's live service (PATCHES §76). The cases now use a label of their own, and a stopped test kills its detached runs. Still, run them in Docker, or on a Mac only in a sandbox. Plain `go test` refuses them on macOS.

```bash
ops/bin/sr-local-ci --docker deploy                # every deploy case, on Linux in Docker
ops/bin/sr-local-ci deploy                         # the same on this Mac, sandboxed
(cd ops && DEPLOY_TESTS_SANDBOX=1 go test -count=1 ./deploy/tests)    # through go test, each script sandboxed
DEPLOY_TESTS_SANDBOX=1 bash ops/deploy/tests/deploy-scripts-test.sh   # the cases alone, each script sandboxed
bash ops/deploy/tests/rehearse-laptop-supervisor.sh <build-a> <build-b> <scratch-dir>   # real launchd, scratch job
python3 ops/bin/srq selftest
python3 ops/bin/sr-plot --selftest
python3 ops/skills/revive-workflow-agents/test_wf_revive.py
ops/bin/sr-local-ci --selftest
```

`ops/` is its own Go module, so `go test ./...` at the repo root does not run the deploy tests. CI runs them in a separate step.

To run the whole Go suite on this Mac, isolated from the live pool, logins, and keychain, run `ops/bin/sr-local-ci` from the checkout. It also runs the deploy tests. Name packages to run only those, as in `ops/bin/sr-local-ci ./internal/proxy`. Use `--docker` for the same run on Linux, and `--help` for every option.

## Not here

The Linux box's deploy and bootstrap scripts are not in this repo, and neither are the quickstart and remote-key scripts. `PATCHES.md` and `KNOWN-ISSUES.md` describe them where a change depends on them. The §63 and §64 code never reached `main` and is not in this repo.
