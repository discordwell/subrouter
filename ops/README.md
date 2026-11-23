# ops

Tools and docs for running this fork on your own Mac and, optionally, your own Linux box. Start with [`GETTING-STARTED.md`](GETTING-STARTED.md).

| Path | What it does |
|---|---|
| `GETTING-STARTED.md` | The tutorial: from a fresh Mac to a working pool, and adding a box. |
| `OPERATING.md` | Day-to-day operation: status and meters, dangerous commands, deploys, peers, held turns, saved resets, troubleshooting. |
| `bin/srq` | Answers questions about the local daemon without its traps: `srq serving`, `srq pool`, `srq resets`, `srq burn`, `srq selftest`. |
| `bin/sr-plot` | Shows each account's 7-day burn pace in the terminal. `sr plot` runs it. |
| `bin/srdash` | Shows every pool this machine can reach on one page. |
| `bin/srprobe` | Sends a request shaped like Claude Code's through a daemon. |
| `bin/sr-triage` | Explains, read-only, why `sr` does nothing. |
| `bin/sr-local-ci` | Runs the Go suite locally, isolated from the live pool, logins, and keychain. |
| `bin/codexmode`, `bin/claudemode` | Start Codex or Claude Code against the pool that has capacity. |
| `bin/claude-route` | Switches new Claude Code and Codex launches between the pool and your own login, and turns the daemon's Claude socket on and off. |
| `bin/subrouter-quickstart` | Takes a Mac from nothing to a laptop daemon every client uses, and points no client at it until a separate process has proved a real turn through it. It can peer the laptop with your box. |
| `bin/subrouter-remote-key` | Mints, checks and removes `<store>/remote.key`, which makes the remote commands exist (PATCHES §17). |
| `bin/subrouter-unbrick` | Finds and undoes client settings that point Claude Code or Codex at a daemon that does not answer. |
| `bin/public-scrub` | Replaces private values with placeholders from a map kept outside the repo. `--check` lists any that remain. |
| `skills/subrouter/` | A Claude Code skill for agents that inspect or operate a subrouter install. `skills/README.md` says how to install it. |
| `skills/revive-workflow-agents/` | A Claude Code skill and `wf_revive.py`: continues workflow agents that died in an outage from their saved transcripts, and starts their runs again without redoing finished stages. The skill also holds the standard prefix for workflow scripts. |
| `hooks/` | Privacy hooks. `pre-commit`, `commit-msg` and `pre-push` refuse a value they find on your machine (account names, tailnet addresses, your email). Install with `git config core.hooksPath ops/hooks`; `hooks/README.md` has the details. |
| `docscheck/` | Fails when `GETTING-STARTED.md` or `OPERATING.md` names a path, link, setting or route that the tree does not have. |
| `publiccheck/` | Fails CI on a tailnet address outside the placeholder blocks. It holds no private values. |
| `deploy/macos/activate-laptop-supervisor.sh` | Moves the laptop job under `subrouter supervise` once, and `--rollback` moves it back. |
| `deploy/macos/deploy-laptop.sh` | Deploys a build to this Mac, checks its routes, and rolls back on failure. Under the supervisor it upgrades the worker without a restart. |
| `deploy/macos/rollback-laptop-peer.sh` | Puts the previous laptop build back. |
| `deploy/macos/laptop-deploy-lib.sh` | The route checks, launchd checks, lock and detached run that the laptop scripts share. |
| `deploy/macos/subrouter-watchdog.sh` | Loads the daemon's job again when something unloads it, unless a stop is announced; `hold` and `release` announce one. |
| `deploy/macos/install-watchdog.sh` | Installs, removes and shows the watchdog's own LaunchAgent, `ai.manaflow.subrouter-watchdog`. |
| `deploy/macos/install-standby.sh` | Installs, upgrades, removes and shows the stand-in's own LaunchAgent, `ai.manaflow.subrouter-standby`, which answers the Claude socket from a box while the daemon is gone, and writes its box tokens. |
| `deploy/lightsail/` | Bootstraps your box (Tailscale, the binary, systemd, tokens, the TLS listener) and deploys builds to it with a drain, checks and rollback. `deploy/lightsail/README.md` walks through it. |
| `deploy/tests/` | Tests for the laptop and box scripts, the watchdog, the stand-in's installer, the setup tools, `claude-route` and `sr-triage`, and rehearsals against scratch LaunchAgents. |
| `LAPTOP.md` | How to move the laptop under the supervisor, deploy, roll back, and recover. |
| `PATCHES.md` | Every change against upstream and why, by § number, condensed. |
| `KNOWN-ISSUES.md` | Problems and misleading meters, with their status, condensed. |

## Run the tools

Put `ops/bin` from a detached checkout of `origin/main` on `PATH`. After a deploy, `deploy-laptop.sh` moves that checkout forward to the deployed commit, but only when the commit is on `origin/main`.

## Test

The deploy cases drive scripts that call `launchctl`, kept from the real one by stubs on `PATH`. A stopped plain `go test ./...` in `ops/` on a Mac once left a detached `deploy-laptop.sh` running after its stubs were deleted, and it unloaded the developer's live service (PATCHES §76). The cases now use a label of their own, and a stopped test kills its detached runs. Still, run them in Docker, or on a Mac only in a sandbox. On macOS plain `go test` refuses them, and so does the harness run by itself; with a sandbox named, the harness first checks that the sandbox refuses exec of `launchctl` (PATCHES §76.6).

```bash
ops/bin/sr-local-ci --docker deploy                # every deploy case, on Linux in Docker
ops/bin/sr-local-ci deploy                         # the same on this Mac, sandboxed
(cd ops && DEPLOY_TESTS_SANDBOX=1 go test -count=1 ./deploy/tests)    # through go test, each script sandboxed
DEPLOY_TESTS_SANDBOX=1 bash ops/deploy/tests/deploy-scripts-test.sh   # the cases alone, each script sandboxed
bash ops/deploy/tests/rehearse-laptop-supervisor.sh <build-a> <build-b> <scratch-dir>   # real launchd, scratch job
bash ops/deploy/tests/rehearse-watchdog.sh <scratch-dir>        # the watchdog on real launchd, scratch jobs
bash ops/deploy/tests/rehearse-standby.sh <scratch-dir> <subrouter binary> <token-dir> <peers-file>   # the stand-in on real launchd, scratch jobs
docker run --rm --init --user "$(id -u):$(id -g)" -e HOME=/tmp -e GOCACHE=/tmp/go-cache -e GOPATH=/tmp/go \
  -v "$PWD:/src" -w /src/ops golang:1.26-bookworm go test -count=1 ./...   # the whole ops module on Linux, as CI runs it
python3 ops/bin/srq selftest
python3 ops/bin/sr-plot --selftest
python3 ops/skills/revive-workflow-agents/test_wf_revive.py
node --test ops/skills/revive-workflow-agents/test_wf_prefix.mjs
node --test internal/proxy/pool_ui_page_test.mjs   # the pool board page; go test ./internal/proxy runs it too
ops/bin/sr-local-ci --selftest
```

`ops/` is its own Go module, so `go test ./...` at the repo root does not run the deploy tests. CI runs them in a separate step.

To run the whole Go suite on this Mac, isolated from the live pool, logins, and keychain, run `ops/bin/sr-local-ci` from the checkout. It also runs the deploy tests. Name packages to run only those, as in `ops/bin/sr-local-ci ./internal/proxy`. Use `--docker` for the same run on Linux, and `--help` for every option.

## Not here

The §63 code never reached `main` and is not in this repo. The §64 code reached `main` only with the change that ported it on 2026-11-22, after its section was written.
