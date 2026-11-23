# Get started with subrouter on a Mac

In this tutorial we set up subrouter on a Mac. At the end, a daemon on `127.0.0.1:31415` holds your Claude Max and ChatGPT Pro logins. Claude Code and Codex send their requests through it, and each conversation stays on one account, so its prompt cache stays warm. The last part adds an optional Linux box as a second daemon.

Plan on about 30 minutes, most of it browser logins.

One rule holds the whole way through. Add accounts before you point a client at the daemon. A client pointed at an empty pool fails every request, and it has no login of its own to fall back to.

[OPERATING.md](OPERATING.md) is the day-to-day guide once this works.

## Before you start

You need these:

- A Mac where you are signed in at the screen. Run every command in Terminal.app. The deploy scripts refuse to restart the daemon from ssh, or from a terminal that outlived a logout (PATCHES §47).
- The Xcode command-line tools (`xcode-select --install`), for `git`, `codesign`, `/usr/bin/python3`, and the C compiler that `make build` uses.
- Go 1.26 or newer. `go.mod` names `go 1.26.0`. Go 1.21 or newer downloads that toolchain on the first build, unless `GOTOOLCHAIN=local` is set.
- Claude Code from its native installer, at `~/.local/bin/claude`, signed in with your own login. `claude-route` looks for it there.
- The Codex CLI on your `PATH`, if you use Codex.
- One or more Claude Max or ChatGPT Pro subscriptions.

## Build subrouter

This fork publishes no release, so we build it from source:

```bash
git clone https://github.com/discordwell/subrouter.git
cd subrouter
make build
```

`make build` writes `bin/subrouter` and signs it ad hoc. Stay in this directory. Every later command runs from the root of this clone.

Do not install upstream's release with `install.sh`, npm, pipx, or the CLI autoupdater. Each one puts the `manaflow-ai/subrouter` binary in place of this fork's, which drops every fork patch.

## Start the daemon

Install the daemon as a LaunchAgent:

```bash
./bin/subrouter install-daemon
```

This copies the build to `~/bin/subrouter`, links `~/bin/sr` and `~/bin/cx` to it, writes `~/Library/LaunchAgents/ai.manaflow.subrouter.plist`, and starts the job. The job runs `~/bin/subrouter serve --addr 127.0.0.1:31415`, so only this Mac can reach it. Claude Code and Codex settings are not touched.

Check that the daemon answers:

```bash
curl -fsS http://127.0.0.1:31415/_subrouter/health
```

The JSON answer includes `"ok": true`.

We use `install-daemon` and not `sr setup`. `sr setup` also mints a proxy token, which the `claude-route` launches we set up below do not carry, so their requests get `401 unauthorized`. It can also write the daemon's address into the clients' own config files, which override `claude-route`. [OPERATING.md](OPERATING.md) has the fix if you already ran it.

## Put the tools on your PATH

`sr` is in `~/bin`, and the operator tools are in this clone's `ops/bin`. macOS puts neither on `PATH`. Add both to `~/.zshrc`:

```bash
echo "export PATH=\"\$HOME/bin:$PWD/ops/bin:\$PATH\"" >> ~/.zshrc
```

Open a new Terminal window, `cd` back into the clone, and check:

```bash
command -v sr srq claude-route
```

The first path is `~/bin/sr`. The other two are in this clone's `ops/bin`.

Run `sr` only with a subcommand. Bare `sr` ends at an interactive `Switch to (#):` prompt, and a stray digit and Enter switch your active account.

## Add your accounts

Add each ChatGPT Pro account:

```bash
sr add codex
```

Codex runs its login in a throwaway `CODEX_HOME`. Approve it in the browser. Your own `~/.codex` login does not change.

Add each Claude Max account:

```bash
sr claude addr <name>
```

`<name>` is a label you choose, such as `work`. Without it, the profile is named after the account's email. Sign in as that account in the browser, and finish the steps the terminal prints. The command prints `Added Claude profile "<name>".`, followed by the account's email and plan. The login stays on this Mac.

Type `addr`, not `add`. `sr claude add <name>` stores a one-year setup token with no refresh token.

Now ask the daemon what it holds:

```bash
srq pool
```

You see one section per provider, a legend under each header, and one line per account, soonest roll first. The names here are placeholders:

```
=== claude @ local (2 accounts): 2/2 preferred for a new session (the daemon's verdict); Fable 2/2
      (5h = short window, refills within hours; 7d = the WEEK, what actually runs out)
      (fable = the Fable/oauth-apps week, its own meter; new = the verdict this daemon publishes for a peer: yes is preferred;
       fallback = still takes a new session here when nothing preferred is left; NO = evicted;
       n/m = sessions in flight / sessions it will take while draining)
  personal                           5h=   -%  7d=  40%  fable=  10%  rolls    20h  new=yes (burndown 1/3)
  work                               5h=   -%  7d=  12%  fable=   3%  rolls    96h  new=yes

=== codex @ local (1 accounts): 1/1 preferred for a new session (the daemon's verdict)
      (5h = short window, refills within hours; 7d = the WEEK, what actually runs out)
      (new = the verdict this daemon publishes for a peer: yes is preferred;
       fallback = still takes a new session here when nothing preferred is left; NO = evicted)
  you@example.com                    5h=   -%  7d=   8%  rolls   110h  new=yes
```

`7d` is the share of the week used, and `new=` is the daemon's own verdict for a new session. `personal` rolls within a day, so it drains first. If an account is missing a minute after you add it, reload the pool:

```bash
curl -fsS -X POST http://127.0.0.1:31415/_subrouter/reload-accounts
```

## Turn on the Claude socket

`claude-route` runs Claude Code over the daemon's Unix socket. Over the socket, Claude Code keeps its first-party features, such as deferred tool search (PATCHES §30).

With the socket on, Claude Code's account calls (profile, organization, policy limits) need one fixed Claude account to answer them. Without one, they fail and Claude Code withholds WebFetch. Name one of your Claude profiles, then turn the socket on:

```bash
PLIST_SET_ENV=SUBROUTER_IDENTITY_ACCOUNT=<name> ops/deploy/macos/deploy-laptop.sh bin/subrouter
claude-route socket-on
```

Both commands run `ops/deploy/macos/deploy-laptop.sh`. It edits the LaunchAgent's environment under a lock, restarts the job, checks the daemon's routes, and puts the old plist and binary back if a check fails. The first command ends with `DEPLOY OK`, and the second with `socket: serving` and the socket path.

Check both settings:

```bash
curl -fsS http://127.0.0.1:31415/_subrouter/health | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["claude_socket"], d["control_plane_identity"])'
```

The answer is `serving {'id': '<name>', 'state': 'pinned'}`.

## Point Claude Code and Codex at the pool

Install the launch shims:

```bash
claude-route install
```

This puts `claude` and `codex` shims in `~/.claude-route/bin` and adds a PATH line to `~/.zshrc` and `~/.zprofile`. Each shim picks a route once, when a client starts. It never edits `~/.claude/settings.json`, the Keychain, or `~/.codex`. The mode starts as `off`, which means your own login.

Open a new Terminal window, then run the wet test:

```bash
claude-route test
```

It makes real launches with Haiku, a few tokens each, on the pool and on your own login, and checks each one against the daemon's session table. The last line counts the results, as in `== claude: <n> passed, 0 failed;  codex: <n> passed, 0 failed ==`.

Its first lines say how it launches. The launches that a new session would make go through Paradev's wrapper in `~/.paradev/bin` when the Paradev app is installed. Without Paradev, they go through the shim, as a new Terminal window does, and the checks of Paradev's wrapper print `SKIP`. A skipped check counts as neither a pass nor a failure.

Switch new launches to the pool:

```bash
claude-route pool
claude-route check codex
claude-route codex pool
```

`claude-route pool` refuses until `claude-route test` has passed today. `claude-route check codex` makes one real Codex launch through the pool with your own `~/.codex` config. `claude-route codex pool` refuses until it and the Codex checks of `claude-route test` have passed today.

A switch changes only new launches. A Claude Code session that is already running keeps its route, so quit Claude Code and start it again. To go back, run `claude-route off` and `claude-route codex off`. For one launch on your own login, type `claude local`.

## Check that it works

Start a one-line Claude session with a known session ID, and ask the daemon which account served it:

```bash
sid=$(uuidgen | tr '[:upper:]' '[:lower:]')
claude -p "Reply with one word: ready" --model haiku --session-id "$sid"
srq serving "$sid"
```

Claude answers `ready`, and `srq serving` prints one of your Claude profile names. Inside a Claude Code session, `srq serving` with no argument reads the session's own `CLAUDE_CODE_SESSION_ID`.

Do the same for Codex:

```bash
codex exec --skip-git-repo-check "Reply with one word: ready"
```

Codex prints `session id: <id>` near the top. `srq serving <id>` prints one of your ChatGPT accounts.

Watch requests arrive in the daemon's log:

```bash
tail -f ~/Library/Logs/subrouter.err.log | grep --line-buffered 'proxy request'
```

Each line names `agent=claude` or `agent=codex`, the `session=`, and the `account=` that paid. The account differs between conversations and stays the same within one.

The daemon's JSON routes answer this Mac with no token. `/_subrouter/usage-status` has each account's meters, `/_subrouter/sessions` maps sessions to accounts, and `/_subrouter/accounts` lists the pool. [OPERATING.md](OPERATING.md) explains how to read them.

## Move the daemon under the supervisor

So far, each deploy restarts the daemon and cuts any reply that is streaming. Under `subrouter supervise`, a deploy starts a new worker behind a port and a socket that stay open, and restarts nothing (PATCHES §69). The move itself restarts the daemon once, so pick a quiet moment.

Preview the move, then make it:

```bash
ops/deploy/macos/activate-laptop-supervisor.sh bin/subrouter --dry-run
ops/deploy/macos/activate-laptop-supervisor.sh bin/subrouter --wait-quiet 300
```

The dry run prints the new plist and a diff, and changes nothing. The real run ends with `ACTIVATED`. Health now reports `"claude_socket": "owned_by_supervisor"`.

From now on, deploy each new build the same way:

```bash
git pull
make build
ops/deploy/macos/deploy-laptop.sh bin/subrouter
```

A good run ends with `DEPLOY OK`. [LAPTOP.md](LAPTOP.md) covers rollback and recovery.

## Unlock the remote commands

Skip this part if you have no box.

A build is local-only until this Mac holds `~/.subrouter/codex/remote.key` (PATCHES §17). Without it, nothing that sends a credential off this Mac exists. `sr server`, `sr remote`, and `sr claude move` are missing from `sr --help`, and the `SUBROUTER_REMOTE_POOL*` settings arm no peer. On a Mac with no box that is the intended state, not a broken install.

The key is a file of mode 0600 that holds 16 or more characters. It is not access control. The box's own tokens are. `ops/bin/subrouter-remote-key` creates it, or you can create it by hand:

```bash
(umask 077; head -c 32 /dev/urandom | base64 > ~/.subrouter/codex/remote.key)
sr server help
```

`sr server help` now lists the server commands. The daemon reads the key when it starts, so deploy once more before you add a peer:

```bash
ops/deploy/macos/deploy-laptop.sh bin/subrouter
```

## Add a box

A box is a cheap Linux machine on your tailnet that runs a second daemon. The laptop ranks the box's accounts beside its own and sends a turn there when one of them is the better pick, or when the laptop cannot reach the provider (PATCHES §31, §59).

1. Create the box and deploy subrouter to it with [ops/deploy/lightsail/README.md](deploy/lightsail/README.md), which uses `ops/deploy/lightsail/deploy-box.sh` and `ops/deploy/lightsail/bootstrap.sh`.
2. Register the box on the laptop with `sr server add`, as "Move accounts to the box" in [OPERATING.md](OPERATING.md) shows. Leave out `--default`, even when a command you copy has it. That flag points Codex at the daemon in `~/.codex/config.toml`, rewrites `~/.config/subrouter/cloud.json`, and restarts the daemon outside `deploy-laptop.sh`.
3. Move Claude accounts to the box one at a time with `sr claude move <name> box`. Keep the profile that `SUBROUTER_IDENTITY_ACCOUNT` names on the laptop. If it moves, socket sessions' account calls get 503, and Claude Code withholds WebFetch. Add Codex accounts to the box with `sr server login box`.
4. Peer the laptop with the box. [OPERATING.md](OPERATING.md) has the steps under "Peer the laptop with a box".
5. Run `srq pool` again. It now prints a section for the box's accounts as well.

Never keep one account on both daemons. Each login refresh rotates the refresh token, and the daemon that did not refresh holds a dead copy for good. `sr claude move` uploads the login, checks that the box can use it, and only then removes the local copy.
