# Run the laptop daemon behind the supervisor

The laptop runs subrouter as the LaunchAgent `ai.manaflow.subrouter`. Once that LaunchAgent runs `subrouter supervise`, a deploy swaps the worker process behind a port and a Claude socket that stay open. A turn that is streaming when the deploy starts finishes on the old worker, and new turns go to the new one. The move to the supervisor restarts the daemon once. Every deploy after it restarts nothing.

These scripts do the work, and `PATCHES.md` §69 records why:

- `deploy/macos/activate-laptop-supervisor.sh` makes the one-time move, and `--rollback` moves back.
- `deploy/macos/deploy-laptop.sh` deploys a build, and `--check` verifies the running daemon.
- `deploy/macos/rollback-laptop-peer.sh` restores the 2026-08-16 cutover backup.
- `bin/claude-route` switches new Claude Code launches to the pool and turns the Claude socket on and off.

Run every command below from the root of the checkout whose `ops/bin` is on your `PATH`. Each script reads the same paths from the environment: `SUBROUTER_LABEL` (default `ai.manaflow.subrouter`), `SUBROUTER_PLIST`, `SUBROUTER_ADDR`, and `SUBROUTER_CONTROL_SOCKET`. The defaults match a LaunchAgent in `~/Library/LaunchAgents`. The binary a script replaces is always the one the job runs: the program of a plain serve, or the `--worker-bin` of a supervised job. `SUBROUTER_BIN`, the npm and Python wrappers' setting, does not move it.

## Use claude-route from the checkout

`claude-route` finds `ops/deploy/macos/deploy-laptop.sh` and `ops/bin/srq` from its own real path. A copy outside the checkout finds neither, and `socket-on` and `socket-off` then refuse. If `~/bin/claude-route` is a copy, move it aside so that `PATH` finds `ops/bin/claude-route`:

```bash
mv ~/bin/claude-route ~/bin/claude-route.bak-$(date +%Y%m%d)
hash -r
command -v claude-route
```

The last command prints the path under `ops/bin`. The shims that `claude-route install` writes are the same bytes as before, so the installed shims need no change.

`socket-on` and `socket-off` do not write the plist themselves. They run `deploy-laptop.sh` on the binary the job runs, with `PLIST_SET_ENV=SUBROUTER_CLAUDE_SOCKET=<socket>` or `PLIST_DELETE_ENV=SUBROUTER_CLAUDE_SOCKET`. The deploy makes the edit under its lock, restarts the job, and puts the plist back if the routes do not verify. While another run holds the lock, the deploy refuses and nothing changes.

## Check what launchd runs

```bash
launchctl print gui/$(id -u)/ai.manaflow.subrouter | sed -n '/arguments = {/,/}/p'
plutil -extract ProgramArguments json -o - ~/Library/LaunchAgents/ai.manaflow.subrouter.plist
curl -s --unix-socket ~/.subrouter/supervisor.sock http://supervisor/_subrouter/supervisor-status
ops/deploy/macos/deploy-laptop.sh ~/bin/subrouter --check
```

`launchctl print` shows what launchd runs now, and the plist shows what launchd loads next. The scripts decide from `launchctl print` and refuse when the two disagree, which happens only when an earlier run stopped part way. Under the supervisor, the arguments start with `~/bin/subrouter-supervisor supervise`. In `supervisor-status`, `active_worker.executable_identity` is the `CDHash` that `codesign -dvvv ~/bin/subrouter` prints, and `claude_socket` says whether the supervisor serves the Claude socket. `serving` is the only word that means it does. The supervisor re-creates a deleted socket path within about 2 seconds, and `waiting` or `displaced` means that another process has the socket's lock or its path (PATCHES §77). `yielded` means that the supervisor let go of the socket on purpose, for the reason and until the time that `claude_socket_yield` gives (PATCHES §77.3). The activation requires `serving`, so it fails during a yield, and a deploy reports the socket as not served. End a yield first with `POST /_subrouter/claude-socket/reclaim` on the control socket. `worker_probe.misses` counts the probes in a row that the active worker has not answered. After 3, which takes 30 to 75 seconds, the supervisor starts a new worker and retires the silent one, and `worker_probe.last_replacement` says when and why (PATCHES §77.2). Before the move, the job runs `~/bin/subrouter serve` and the control socket does not exist.

## Know what every run that changes the job does

- It takes `<plist>.supervisor-mutation.lock`, the lock that `sr update` and upstream's supervisor scripts take, and refuses while another run holds it. `--check` and `--dry-run` take no lock. A small helper process holds the lock for the run. If that helper has not answered within 30 seconds (`SUBROUTER_MUTATION_LEASE_WAIT_SECONDS`), the run kills it and stops, having changed nothing, and the lock is free again (PATCHES §83).
- A run that restarts the job refuses unless `launchctl managername` prints `Aqua` and `launchctl print` answers 0 or 113 (PATCHES §47). From ssh, launchd boots the job out and then refuses to bootstrap it. From a terminal that outlived a logout, launchd refuses every verb. A supervised deploy restarts nothing, so it needs only `launchctl print` to answer.
- Every run that changes the job, a supervised upgrade included, goes on in a detached process and writes `~/.subrouter/transitions/<script>-<stamp>-<pid>.log`, with the exit code in `.rc` and the pid in `.pid`. Your shell shows the log as it grows. If the shell is closed or killed, or a tool times out, the run still finishes.
- Before a bootout, a run tells the watchdog the stop is on purpose (PATCHES §81), and once its bootstrap works it says so. A run that dies in between leaves a notice that ends within three minutes.
- The activation and both rollbacks boot the job out and wait for launchd to let it go before they write a binary or a plist. `deploy-laptop.sh` installs first, staged, signed and renamed so that the running process keeps the file it was started from, and makes its plist edit if it has one. Then it restarts the job, by `kickstart -k` or by a bootout, a wait for launchd to let the job go, and a bootstrap.

## Move the job under the supervisor

The move cuts any turn that is still streaming when launchd stops the old daemon, a few seconds after the bootout. Pick a quiet moment.

1. Build and sign the binary, for example with `make build`. Build it from this repo, which gives the supervisor the Claude socket. A build of upstream subrouter lacks `owned_by_supervisor`, and the activation and a supervised deploy refuse it and change nothing.
2. Preview the change. The dry run prints the new plist, the worker config, and a diff against the live plist, and changes nothing:

   ```bash
   ops/deploy/macos/activate-laptop-supervisor.sh bin/subrouter --dry-run
   ```

3. Activate:

   ```bash
   ops/deploy/macos/activate-laptop-supervisor.sh bin/subrouter --wait-quiet 300
   ```

`--wait-quiet` waits until no Codex websocket turn is in flight. The drain socket cannot tell a Claude Code HTTP stream from an idle websocket, so the script reports those and does not wait on them. To dial peers over Tailscale first, add `--peer-tailnet <peer>=<tailscale-ip>[,...]`, with each peer named as in `SUBROUTER_REMOTE_POOLS_FILE`. The value goes into the worker config as `SUBROUTER_PEER_TAILNET`, so a later change to it is a deploy, not a restart.

Before the bootout, the script records how the live daemon answers the route checks that `deploy-laptop.sh` runs. It prints `ACTIVATED` when the supervisor runs the build, owns the Claude socket, and answers every route check at least as well as the live daemon did. If a check fails or launchd refuses the new plist, the script stops the job, restores the old plist and binary, starts the daemon again, and exits with 1. Once it has stopped the job, it always starts it again from the plist on disk. A file it cannot put back is named, and it exits with 2. If it cannot put the old plist back, it leaves the supervised plist and the files that plist names, and the job comes back under the supervisor. If launchd will not load the job, the script prints the `launchctl bootstrap` command that loads it. On a job that already runs supervise, it does nothing.

The activation writes these files:

- `~/bin/subrouter` and `~/bin/subrouter-supervisor`, both the build you gave.
- `~/.subrouter/worker-config.json`, the worker's `serve` arguments and extra environment. The supervisor reads it for every new worker.
- The plist. It runs `supervise --addr <the live --addr> --control-socket ~/.subrouter/supervisor.sock --worker-bin ~/bin/subrouter --worker-stop-grace 2m --drain-timeout 90s --ready-timeout 2m --worker-config ~/.subrouter/worker-config.json -- <the live serve arguments>`, with `ExitTimeOut` 120 and `ThrottleInterval` 10. The environment stays as it was, `SUBROUTER_CLAUDE_SOCKET` included.
- `~/.subrouter/supervisor-activation-<stamp>/`, the backup that a failed activation restores and that `--rollback` compares against.

## Deploy a build

```bash
ops/deploy/macos/deploy-laptop.sh bin/subrouter
```

On a supervised job, the script installs the build at the job's `--worker-bin` and posts `/_subrouter/upgrade` to the job's `--control-socket`. The supervisor switches new connections to the new worker once that worker is ready. The script then checks that `supervisor-status` reports the new build's `CDHash` and that the routes answer as they did before. If the new worker never becomes ready, the old one keeps serving and the script puts the old binary back. The supervisor finishes an upgrade even after the script stops waiting, so an answer other than 200, a timeout included, is settled by the build supervisor-status reports active. If a check fails, the script installs the old binary and upgrades again. A successful run ends with `DEPLOY OK`. A run never ends with `DEPLOY OK` while the daemon does not own its Claude socket. That is the case when a stand-in answers at the path, or when `supervisor-status` says `waiting`, `displaced` or `yielded`. The run keeps the new build, rolls back nothing, prints what holds the path and what to do, and exits 3 with `claude socket not owned by the daemon` (PATCHES §84).

To change the worker's flags or environment, edit `~/.subrouter/worker-config.json` and deploy the binary that is already installed.

## Restart the supervisor

The supervisor reads its plist, its flags, and `SUBROUTER_CLAUDE_SOCKET` only when it starts, so a change to any of them needs a restart. A restart closes the port and the Claude socket until the new supervisor's first worker is ready. The old supervisor waits up to 90 seconds (`--drain-timeout`) for its open connections to close, and launchd ends it after 120 seconds (`ExitTimeOut`) at the latest.

These commands refuse a supervised job unless you add `--restart-supervisor`:

- `RELOAD=1 ops/deploy/macos/deploy-laptop.sh <binary>`. Use `ops/deploy/macos/deploy-laptop.sh <binary> --restart-supervisor`, which also installs the build as `~/bin/subrouter-supervisor`.
- `claude-route socket-on` and `claude-route socket-off`.
- `ops/deploy/macos/rollback-laptop-peer.sh`.

## Move back to a plain serve

```bash
ops/deploy/macos/activate-laptop-supervisor.sh --rollback
```

The rollback writes a plain `serve` plist from the current supervised plist and the worker config. It keeps the worker that deploys since the activation installed, the plist's environment as it is now, and the worker config's arguments and environment, `SUBROUTER_PEER_TAILNET` included. It prints each way that plist differs from the one in the newest activation backup, or in `--backup <dir>`. It puts the backup's worker config back, removes `~/bin/subrouter-supervisor` if the activation created it, and restores no binary.

The rollback restarts the supervisor. The script prints `ROLLED BACK` when the plain daemon answers on TCP and on the Claude socket.

## Recover from an interrupted run

Read the newest log in `~/.subrouter/transitions/`. A `.log` with no `.rc` beside it is a run that is still going; its pid is in the `.pid` file.

- If a script refuses because the plist and the job launchd runs disagree, run the command the refusal names. A plist that says serve under a running supervisor needs `activate-laptop-supervisor.sh --rollback`. A plist that says supervise under a running serve needs `--rollback`, and then the activation again.
- If launchd has not loaded the job at all, run the same command again. A deploy of a supervised job and an activation or rollback that has nothing left to do bootstrap the plist on disk and verify it. An unsupervised deploy loads the job as part of its restart.
- If a script says launchd refused to answer (rc 141, 153, or 125), run it from a new Terminal.app window. A new Superset tab stays in the ended login session.
- If the daemon is down and nothing is running, the watchdog loads the plist on disk within about 5 s, unless a stop is announced. Without the watchdog, load it from a Terminal.app window with `launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/ai.manaflow.subrouter.plist`.

## Keep the job loaded with the watchdog

launchd forgets a job that is booted out, and `KeepAlive` then has nothing to restart. The watchdog is a second LaunchAgent, `ai.manaflow.subrouter-watchdog`, that checks every 5 s and loads the daemon's job again from its plist when launchd has no job for it (PATCHES §81). It never restarts a job that is loaded, and it leaves alone a job loaded from another plist. Each time it acts it sends one notification and logs what it found and did in `~/Library/Logs/subrouter-watchdog.log`.

```bash
bash ops/deploy/macos/install-watchdog.sh install --dry-run   # what it would write and run
bash ops/deploy/macos/install-watchdog.sh install
bash ops/deploy/macos/install-watchdog.sh status
bash ops/deploy/macos/install-watchdog.sh uninstall           # turns it off
```

The deploy scripts and `sr daemon restart` announce their own stops. `sr daemon down` does not, on purpose: a stray one must not keep the watchdog away for long. To stop the daemon by hand, with `sr daemon down` or a `launchctl bootout`, or before `deploy/macos/migrate-launchagent-to-supervisor.sh`, `rollback-launchagent-supervisor.sh` or the peer step of `ops/bin/subrouter-quickstart`, announce it first, for up to a day, or the watchdog loads it again:

```bash
bash ops/deploy/macos/subrouter-watchdog.sh hold ai.manaflow.subrouter 3600
bash ops/deploy/macos/subrouter-watchdog.sh release ai.manaflow.subrouter   # before the hour ends
```

Rehearse it on scratch jobs, from a Terminal.app window: `bash ops/deploy/tests/rehearse-watchdog.sh <scratch-dir>`. It ends with `REHEARSAL PASS` when a booted-out scratch job came back in under 10 s each time, an announced stop was kept until it was withdrawn or its marker ended, and a job loaded from another plist was left as it was.

## Stand in for the daemon while it is gone

A Claude Code session talks only to the daemon's Claude socket, so while the daemon is down every session on the Mac waits, even when the boxes are healthy. The stand-in, `subrouter standby`, is a second LaunchAgent, `ai.manaflow.subrouter-standby`, that waits on the socket's lock. When the supervisor lets go of the socket, the stand-in takes the path, holds new requests for 30 s in case the daemon comes straight back, and then sends turns to a box. When the supervisor asks for the socket again and the daemon has answered ready three times, it gives the path back (PATCHES §79). It needs a supervisor that waits for the socket's lock, which `supervisor-status` shows as `claude_socket_waits_for_lock: true` (§77.1). An older supervisor must be restarted on a newer build first.

Give it one `membership` token for each box in the peers file, minted on that box (`subrouter token mint <name> --role membership --expires 90d`, see `OPERATING.md`). The token goes in through standard input only, and nothing of it is printed:

```bash
pbpaste | bash ops/deploy/macos/install-standby.sh token <box name> --expires <its last day, YYYY-MM-DD>
```

Then install it with a build of this checkout. `--dry-run` prints the plist and every command and changes nothing:

```bash
bash ops/deploy/macos/install-standby.sh install ~/bin/subrouter --dry-run
bash ops/deploy/macos/install-standby.sh install ~/bin/subrouter
```

`install` copies the build to `~/.subrouter/standby/subrouter-standby`, a copy that no deploy replaces, so deploys of the daemon never touch the stand-in. It reads the daemon's settings from `~/Library/LaunchAgents/ai.manaflow.subrouter.plist` and never changes that file or the daemon's job. Before it changes anything it asks the build, as `subrouter standby --check`, whether the stand-in would arm, and refuses with one line when it would not. It prints the copy's sha256, and it adds the job to the watchdog's list (`--no-watchdog` leaves the watchdog alone). To upgrade, run `install` again with the new build. It refuses while the stand-in holds the socket, because the restart would cut every session it carries, unless you add `--force`.

Read it with `status`. `state` is `armed` while it waits, `holding` and then `serving` while it has the socket, and `disarmed` with a reason when it will not take the socket. Each box shows its token word (`ok`, `refused`, `file_missing`, ...), and with a recorded last day `renew_soon` in the last 7 days and `lapsed` after it:

```bash
bash ops/deploy/macos/install-standby.sh status
```

Drill it on the live job without stopping the daemon: let the supervisor yield its socket for two minutes, watch `status` go to `holding` and then `serving`, and see the daemon take the socket back afterwards. `reclaim` ends a drill early.

```bash
curl -s --unix-socket ~/.subrouter/supervisor.sock -X POST 'http://supervisor/_subrouter/claude-socket/yield?for=2m'
curl -s --unix-socket ~/.subrouter/claude.sock http://x/_subrouter/health        # "claude_socket": "standby" during the drill
curl -s --unix-socket ~/.subrouter/supervisor.sock -X POST http://supervisor/_subrouter/claude-socket/reclaim
```

Rehearse it first on scratch jobs, from a Terminal.app window. It needs a build of this checkout, a folder holding one stand-in token file per box (named as the box), and the peers file, which it reads where it is. Keep the scratch folder short, because a socket path must stay under 104 bytes:

```bash
bash ops/deploy/tests/rehearse-standby.sh /tmp/sbr <subrouter binary> <token-dir> <peers-file>
```

It ends with `REHEARSAL PASS` when the socket went from the scratch daemon to the stand-in with no gap over 1 s, one real turn was answered by a box after the hold, the daemon had the socket back within 10 s of its job being loaded again, and a 60 s yield gave both hand-overs again. The turn costs one short haiku answer on a box.

To turn it off, `bash ops/deploy/macos/install-standby.sh uninstall`. It takes the job off the watchdog's list and keeps the token files unless you add `--purge-tokens`.

## Rehearse on a scratch job

`ops/deploy/tests/rehearse-laptop-supervisor.sh` runs the activation, a deploy with streams open over TCP and the Claude socket, the refusals, and the rollback against a scratch LaunchAgent with two builds. It also turns the Claude socket off and on with `claude-route` through `deploy-laptop.sh`. Then it activates, deploys and rolls back again, and SIGKILLs the calling shell each time: once the bootout has started, or once the upgrade has. It uses the label `test.subrouter.rehearsal.<pid>`, ports 31499 and 31498, sockets in `/private/tmp/srsv-<pid>`, and a scratch home for all five state paths. It never touches the real LaunchAgent:

```bash
bash ops/deploy/tests/rehearse-laptop-supervisor.sh <build-a> <build-b> <scratch-dir>
```

The two builds must differ, for example by `-X github.com/discordwell/subrouter/internal/buildversion.version=<name>`. The script prints one `PASS` or `FAIL` line per check, and what clients saw during each transition.
