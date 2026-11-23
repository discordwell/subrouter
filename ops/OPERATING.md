# Operate subrouter

This guide is for a Mac that runs the daemon as [GETTING-STARTED.md](GETTING-STARTED.md) sets it up, with an optional Linux box as a second daemon. The how-to sections come first, and the reference tables are at the end. A § number points to the section of [PATCHES.md](PATCHES.md) that records why the behavior exists.

## Read the pool correctly

### Ask the daemon, not the CLI

The daemon decides where a session goes, and it publishes that decision. Read it there:

- `srq pool` shows each account's meters and the daemon's new-session verdict.
- `srq serving` shows which account serves a session.
- `GET /_subrouter/usage-status` is the JSON both commands read.

`sr status` is a view for the human at the terminal. Its columns explain your own direct login and the CLI's switch recommendation. They do not show the router's ranking.

A Codex account enrolled with owner claims has the ID `codex-owner-<hash>`, and its `usage-status` row carries the hash in `email` as well. Its `label` normally holds its login email. `sr status` shows such an account by its login email. `sr plot`, `srq pool`, `srdash` and the pool board name every account by its `label`, or by its ID when it has none, and `srq pool --json` gives both, as `id` and `name`. A command that takes an account, such as `srq dir`, takes the ID.

### The CLI's "protected" is not the router's gate

When an account fails `UsableForNewSession`, the Use column of `sr status` reads `<n>% left, not routable`. `sr status --json` gives the account `"state": "protected"`, unless another state, such as `active` or `cooked`, takes precedence. The table's State column never prints `protected`. The router also routes an account in its burndown window, and there it ranks the account above every healthy one until the account reaches its session allowance (`at_cap`). Burndown covers the last 24 hours before the weekly roll. It also covers an account that holds an unclaimed reset that gives back the week once less than 25% of its week is left (§56). A Fable reset, which clears only the Fable limit, holds nothing (§64). An account at 8% left can be the router's first pick for a new session.

The router's verdict is `normalized.new_session` on each `usage-status` row. `srq pool` prints it as `new=`:

| `new=` | Meaning |
|---|---|
| `yes` | Preferred for a new session. |
| `yes (burndown n/m)` | Draining before its roll. `n/m` is the sessions in flight against its allowance. |
| `fallback (<reason>)` | Takes a new session on this daemon only when nothing preferred is left. |
| `NO (<reason>)` | Evicted. It takes no new session. |
| `?` | The daemon published no verdict. |

The reasons are `eligible`, `burndown`, `exhausted`, `holdout`, `at_cap`, `short_floor`, `below_reserve`, `burn_projection`, and `queue_closed`, plus `auth_invalid` for a login this daemon checked and found dead. A peer that refuses a forwarded turn names one of these, or `unknown_account`, `machine_policy`, or `not_oauth`. The order the router itself uses is `rank` and `reason` in `GET /_subrouter/pool.json`.

### Window names differ by provider

`usage-status` rows carry the provider's own window names. A lookup that uses one provider's names on the other provider's rows finds nothing, and a script that reads nothing as zero headroom reports healthy accounts as spent.

| Provider | Window | Meaning |
|---|---|---|
| Claude | `5h` | The short window. Since about 2026-10-31 Anthropic sends none for Claude Max accounts. A Claude account with no `5h` reading is not floored on it (§62). |
| Claude | `7d` | The week. This is the budget that runs out. |
| Claude | `oauth-apps-weekly` (header `7d_oi`) | The Fable meter, read from the usage answer's `limits[]` Fable row, else from `seven_day_oauth_apps` (§64). It is independent of `7d`, and it can read over 100%. |
| Claude | `opus-weekly`, `sonnet-weekly` | Present only when Anthropic reports them. Opus and Sonnet spend `5h` and `7d` (§24). |
| Codex | `primary`, `<model>/primary` | The week. Codex has no short window. |
| Codex | `reached` | The limit is reached. |
| Codex | `spend`, `spend/reached` | A usage-based seat's monthly spend cap (§42). |

Prefer each row's `normalized` block, which is the scheduler's own reading: `headroom`, `short_headroom`, `weekly_headroom`, `seconds_to_weekly_reset`, `fresh` (§13), and `weekly_unstarted` (§86). `srq` already does this.

### A missing window means unknown

A window that is absent means the daemon has no reading. It never means unspent, and it never means spent. `srq pool` prints `??` for a value it does not have, and `-` for a window the provider does not send. A row with `"fresh": false` carries the last good reading past a failed fetch, and a row with `sweep_cut: true` was cut by the sweep's deadline (§18). Treat both as stale.

### Meters that mislead

- `ResetAfterSeconds: 0` on `5h` alone is normal. Nothing has been spent in that window. `5h` and `7d` both at zero means the account was never activated. Sign in on the provider's site and accept the terms.
- `normalized.weekly_headroom` is not the week left. It is the measured week less this daemon's live debit since its last reading. Read the week from `windows`.
- A new subscription inherits the account's running week. It can arrive with a day of week left and go straight into burndown.
- A Claude account carries an email in `/_subrouter/accounts` only when its profile is named after one. The profile name is the identity. Codex accounts carry their email.

### An unstarted week is started at once

A Codex weekly window starts on the account's first request, then counts down whether or not anyone spends. This happens at first and again after every weekly roll, so an idle account is unstarted once a week. Until its first request the account reads its whole week as left: the reset equals the window's length. Two things start such a week.

The daemon that holds the account starts it itself (§90). Within seconds of a reading that says one of its own Codex accounts has not started its week, it sends one tiny Codex turn on that account: "Reply with one word: ok", asked of `gpt-6.2-sol` at low effort, with no tools. The turn goes to that account's own upstream, the way a turn pinned to the account goes, and never to another account, a peer or a fallback. It leaves no session and no transcript, and its answer is read and thrown away, never logged. The daemon marks the account as a new session would, so the next new session is not also pulled there, and a new session that got there first leaves it nothing to send. It never sends for a peer's account: the peer sends for its own. It never sends for an account whose credential is dead or that the router would not serve, or for one under a capacity or rate-limit mark. If a newer reading still says unstarted once the mark has expired, it sends again: 10 minutes after the first send, then 20, 40 and so on, at most 6 hours apart. A failed send counts too.

The router also gives such an account the next new session ahead of every other account that can take one, on this daemon or on a peer (§86). Only machine policy, a worse selection tier, a capacity or rate-limit mark, the new-session test, and the machine-binding clauses rank above it. After that session, or the daemon's own turn, the daemon marks the account for 10 minutes, and the account ranks on its pace like any other. If a reading still says unstarted when the mark expires, the account takes the next new session first again. So a send that failed, or that did not start the clock, still leaves the next new session to start it. A warm session never moves for this. Only a new session, or a cold session that ranks again anyway, goes there.

To see it:

- `srq pool` prints `unstarted` beside the account's roll, and `start sent HH:MM` beside that once the daemon sent its turn, in this machine's time. `x3` after it means the third send in a row. `srq pool --json` carries `weekly_unstarted`, `self_start_sent_at` and `self_start_sends`.
- The account's `usage-status` row has `"weekly_unstarted": true` in `normalized`. A quota pool's entry has it for that pool's own week, such as the Spark pool's. It is left out when false. Once the daemon sent its turn, `normalized` also has `"self_start": {"sent_at": ..., "sends": N}`: the last send, to the second in UTC, and how many it has sent in a row. It is left out before the first send, and again once a reading shows the clock running.
- `GET /_subrouter/pool.json` gives the row `"weekly_unstarted": true`, the state `pristine` and a PRISTINE fuse, and `self_start` as `usage-status` has it. Its `state_note` says, in words, whether the account's daemon sent its request and when, whether the next new session goes there, and until when the priority is off. A peer's row says what the peer's daemon sent. Its `reason` is `its weekly window has not started; one new session starts its clock` where that clause set its rank. The board at `/_subrouter/pool` shows `start sent HH:MM` beside the fuse.
- Each send logs `weekly window not started; the daemon sent one request to start its clock` at Info, or `weekly window not started; the daemon's request to start its clock failed` at Warn. Both carry `account`, `model`, `status` (0 when no answer came), `sends` (the count in a row), `duration_ms`, `priority_off_until` and `next_send_after`, and a failure carries `error`.
- After three sends without the week starting, `weekly window still not started after the daemon's requests` is logged once at Warn. When a reading shows the clock running after sends, `weekly window started after the daemon's request` is logged at Info with `sends`.
- Each session sent to start a week logs `weekly window not started; this new session starts its clock`, with `account`, `peer` for a peer's account, and `priority_off_until`. The `remote pool decision` line names the same clause in `clause`.

A peer's send changes no ranking here. Until the peer's reading turns, this daemon can still send that account one new session of its own.

`SUBROUTER_SELF_START_WEEK=0` turns the daemon's own turn off. `SUBROUTER_SELF_START_MODEL` names another model for it. `SUBROUTER_START_UNSTARTED_WEEK=0` turns the priority and the session marks off. The two are independent: with the priority off, the daemon still sends its turn and marks the account. Either way the daemon still reads and publishes the fact, and a peer ranks on it by its own switch.

### Read the pool's burn

Burn says how fast the pool spends each weekly meter (§92.2). Each daemon keeps a history of its own accounts' meter readings, and from it each account's burn over the last hour, 6 hours and 24 hours: the rise in the meter's used fraction in that time, divided by that time's share of the week. 1.00 spends the budget exactly as fast as the week lasts, 2.00 twice as fast, and 0 not at all. A roll or a claim that gives budget back adds nothing. The meters are Claude's week, Claude's Fable meter and Codex's week. This is not `sr plot`'s burn pace, which divides what each week has spent so far by how much of it has passed: burn reads only the last hours, so a change in demand shows the same day.

The pool figure is the mean over the accounts on this daemon and on every peer, each account weighing the same. Read it with `srq burn`, or one line at the end of `srq pool`:

```text
pool burn over 24 hours (1.00 = on pace): Claude week 0.80 (40% at the limit), Claude Fable 1.10, Codex week not enough history yet; srq burn says more
```

- The trend compares the last 6 or 24 hours with the 6 or 24 hours before them. `up 0.20 on the 6 hours before` means the burn over the last 6 hours is 0.20 higher than over the 6 hours before.
- The share at the limit is how much of the accounts' time was spent at 99.5% used or more. A spent account cannot take more work, so it burns 0 however much is sent to it. A low burn with a high share at the limit means the pool is short, not idle.
- A figure needs readings over at least half its window: 30 minutes for the last hour, 3 hours for 6 hours and 12 hours for 24 hours. A trend needs 9 hours of history for 6 hours and 36 hours for 24 hours. Until then `srq burn` says `not enough history yet`, never 0.
- A peer that publishes no burn, a build from before §92 or one with the history off, is named as not counted. Its accounts are left out, not read as 0. So is a peer whose last answer is more than 15 minutes old.

What the figures cannot say. Each account weighs the same, because plan sizes are not known. A pool figure is only as good as each daemon's history: a daemon that was down has a gap, and the rise across it is spread evenly. A meter that reads in whole percents makes the 1-hour figure move in steps of about 1.7.

The history is `usage-history.jsonl`, beside `token-usage.jsonl` and `sessions.json` in the folder of the daemon's `--sessions` file (`~/.subrouter` by default; `SUBROUTER_STATE_DIR` does not move it), kept for 35 days. It loads at start, so a restart or a deploy keeps the figures. Each `usage-status` row carries its account's figures as `burn`, outside `normalized`, and `GET /_subrouter/burn.json` gives the pool figure with each account's figures. Nothing ranks or routes on any of it. `SUBROUTER_USAGE_HISTORY=0` turns the history and the figures off.

## Avoid the dangerous commands

These commands change state that every running client depends on, or spend something that does not come back. Run them only when you mean to.

| Command | What it does | Instead |
|---|---|---|
| bare `sr` | Ends at a `Switch to (#):` prompt. A digit and Enter switch your active account. | `sr status </dev/null`, or `srq`. |
| `sr switch`, `sr g`, `sr gui`, `sr pick`, or the prompt's digit, on a Codex pool account | Exports that pool login into `~/.codex/auth.json`, OpenCode and pi, which takes the record out of serving (§23). | Leave pool accounts in the pool. Re-enroll a demoted one with `sr codex migrate-isolation`. |
| `sr setup` | Installs the daemon, mints a proxy token, and can point Claude Code and Codex at the daemon in their config files. Those files override `claude-route`, and the `claude-route` launches they do not override get 401. | `install-daemon` and `claude-route`. See [Fix 401s after `sr setup`](#fix-401s-after-sr-setup). |
| `sr storage`, `sr login`, `sr logout`, `sr server add`, `sr server use` | Rewrite `cloud.json`, `servers.json`, or the Codex config that clients read. | Run them in an isolated home first, with its own `remote.key` (below). |
| `sr server add <name> ... --default` | Makes the box the default server for `sr codex`, and writes the four Codex base-URL keys into `~/.codex/config.toml`, so `codex local` still goes to the daemon. It also switches `cloud.json` to legacy storage and restarts the daemon outside `deploy-laptop.sh`. | Leave out `--default`, as in [Move accounts to the box](#move-accounts-to-the-box). |
| `subrouter serve` by hand | Starts a second daemon against the same store. | `deploy-laptop.sh`. |
| `sr daemon restart`, `sr daemon stop`, and, with `remote.key`, `sr server restart` and `sr server down` | Restart or boot out the LaunchAgent outside `deploy-laptop.sh`'s lock, route checks, and rollback. On a supervised job, a restart also closes the port and the Claude socket. | `deploy-laptop.sh`, with `--restart-supervisor` when the supervisor itself must restart. |
| `sr cleanup --purge --yes` | Removes the daemon and deletes every local credential. Each account then needs a new browser login. | `sr cleanup --yes` keeps the credentials. |
| `sr claude add <name>` | Stores a one-year setup token with no refresh token. | `sr claude addr <name>` for a browser login. |
| `sr claude reset`, `sr codex reset`, `sr reset` | Spend a saved reset. | See [Saved resets](#saved-resets). |
| `codex login` or `codex logout` with `CODEX_HOME` set to a pool store | Replaces or ends that account's login chain. | `sr codex login` and `sr codex logout` run on your own `~/.codex`. |
| `deploy/macos/install-cli-autoupdate.sh`, `deploy/macos/migrate-launchagent-to-supervisor.sh`, `install.sh`, npm, pipx | Install upstream's release in place of this fork's build, or rewrite the plist the deploy scripts expect. | `make build` and `deploy-laptop.sh`. |
| `cp` over `~/bin/subrouter` | Leaves a stale code signature. launchd then kills every start, and KeepAlive restarts it forever. | `deploy-laptop.sh`, which stages, signs, and renames. |
| `go test` on `./internal/proxy`, `./internal/agents/claude`, or `./cmd/subrouter` | Reaches the real Keychain. | `ops/bin/sr-local-ci <packages>`, or `make docker-test`. |
| one Claude or Codex login on two daemons | Each refresh rotates the token. The other daemon's copy dies for good. | `sr claude move <name> <server>`. |

To try a command without touching the live pool, give it an isolated home. Unset every variable that points back at the real one:

```bash
go build -o /tmp/sr-try ./cmd/subrouter
t=$(mktemp -d)
env -u CODEX_HOME -u SUBROUTER_CLOUD_CONFIG SUBROUTER_STATE_DIR=$t/state HOME=$t CLAUDE_CONFIG_DIR=$t/claude /tmp/sr-try setup --plan --storage local
```

`sr storage`, `sr login`, `sr logout`, and `sr server` are remote commands (§17). The build hides them until it finds a `remote.key` in `codex/` under `SUBROUTER_STATE_DIR`, so a key under the isolated `HOME` does not count. Give the isolated store a key of its own:

```bash
mkdir -p $t/state/codex && (umask 077; head -c 32 /dev/urandom | base64 > $t/state/codex/remote.key)
env -u CODEX_HOME -u SUBROUTER_CLOUD_CONFIG SUBROUTER_STATE_DIR=$t/state HOME=$t CLAUDE_CONFIG_DIR=$t/claude /tmp/sr-try server list
```

`server list` then prints `No servers configured`. Without the key, it fails with `unknown account command "server"`.

## Query the daemon

### Use srq

`ops/bin/srq` answers the common questions and reads the daemon's own verdicts. A hand-written query is how five untouched Codex accounts were once reported as spent.

| Command | Answer |
|---|---|
| `srq serving [<session-id>] [--json]` | The account that serves the session. With no ID, it reads `CLAUDE_CODE_SESSION_ID`. |
| `srq pool [--local\|--remote] [--json]` | Each provider's accounts, meters, and `new=` verdicts, on this daemon and each peer. |
| `srq dir <label>` | The profile directory of one account, from the daemon. |
| `srq selftest` | The regression fixtures. It needs no daemon. |
| `srq resets [--json]` | Every saved reset in the pool, soonest expiry first, then the totals per kind and expiry date (§91). |
| `srq burn [--json]` | How fast the pool spends each weekly meter, with the trend and the share at the limit (§92.2). |

`srq serving` prints `<account>` for a local session, and `<account> on <peer>  [remote]` for one a peer serves, with exit code 0. It prints `unassigned (session ...)` and exits 1 when the daemon has no row for the session. That happens before the session's first model turn, for a client that does not route through subrouter, and after a peer hold lapses.

Do not derive the session ID from transcript file names. The newest file under `~/.claude/projects` belongs to whichever session wrote last.

`srq` reads `SUBROUTER_ADDR` as a URL (default `http://127.0.0.1:31415`). The deploy scripts read the same variable as `host:port`. Set it for one command, and never export it. For a daemon that is not on loopback, set `SUBROUTER_ADMIN_TOKEN` or `SUBROUTER_ADMIN_TOKEN_FILE`.

Other tools in `ops/bin`:

- `srdash` serves a page on `127.0.0.1:31416` with every pool this Mac can reach. `srdash --once` prints the same data as JSON.
- `sr-triage` explains, read-only, why `sr` does nothing.
- `srprobe` sends a request shaped like Claude Code's through a daemon.
- `sr-plot` shows each account's 7-day burn pace in the terminal. `sr plot` charts the burn pace from `sr` itself.

### Read the JSON routes

From this Mac, the routes answer with no token. Route names are hyphenated, and an unlisted `/_subrouter/` path answers 404. [Routes](#routes) lists the useful ones.

```bash
curl -fsS http://127.0.0.1:31415/_subrouter/usage-status | python3 -m json.tool | less
curl -fsS http://127.0.0.1:31415/_subrouter/sessions | python3 -m json.tool | less
```

A 403 `admin request host must be a loopback address` means the `Host` header named something other than this Mac. A 403 `cross-site admin request rejected` means a web page on another origin made the call.

### Read the log

The daemon logs to `~/Library/Logs/subrouter.err.log`. The daemon caps the file itself and keeps three rotated copies, `.1` to `.3` (§39). Search all of them with `subrouter.err.log*`.

```bash
grep -h 'proxy request' ~/Library/Logs/subrouter.err.log* | tail -20
```

Each `proxy request` line names the `agent`, `session`, `account`, `path`, and `upstream`. A `remote pool decision` line says why a turn went to a peer, with `target_peer`, `target_account`, and `session_state`. A `weekly window not started; this new session starts its clock` line names each account a new session was sent to so that its week starts (§86), and a `weekly window not started; the daemon sent one request to start its clock` line each account the daemon sent its own turn to (§90). A turn kept on its account logs that line at Debug. To see those, set `SUBROUTER_LOG_LEVEL=debug` in the daemon's environment.

## Point clients at the pool

### Use claude-route

`ops/bin/claude-route` decides each client's route once, at launch, through the `claude` and `codex` shims in `~/.claude-route/bin`. It never edits a file that a running session reads, so a switch cannot strand one.

| Command | Effect |
|---|---|
| `claude-route status` | The mode, the shims, the socket (`STAND-IN` when a stand-in holds it, PATCHES §84), the identity account, and the last test. |
| `claude-route pool`, `claude-route off` | New Claude Code launches use the pool over the socket, or your own login. |
| `claude-route codex pool`, `claude-route codex off` | The same switch for new Codex launches. |
| `claude-route test` | The wet test. `pool` refuses without a pass today. Without the Paradev app, the checks of its wrapper print `SKIP` and count as neither a pass nor a failure. |
| `claude-route check`, `claude-route check codex` | One real launch, made as a new session makes it. It goes through Paradev's wrapper when this Mac has one, and through the shim otherwise. `codex pool` refuses without a passing `check codex` today. |
| `claude-route socket-on`, `claude-route socket-off` | Start or stop the daemon's Claude socket through `deploy-laptop.sh`. A supervised job also needs `--restart-supervisor`. |
| `claude-route accounts` | The account names that route words accept. |
| `claude-route install`, `claude-route uninstall` | Add or remove the shims and their PATH lines. |

Route words are one-launch exceptions, typed first:

- `claude local ...` launches on your own login, with subrouter dropped.
- `claude <account> ...` launches through subrouter, pinned to that account. A pin never falls back to your login. On this build, a pin to a Claude account on this Mac must name `SUBROUTER_IDENTITY_ACCOUNT`. A word the shim cannot honor launches nothing and exits 2, or 75 when the daemon is down.
- `codex local` and `codex <account>` do the same for Codex.
- `CLAUDE_ROUTE=off claude` and `CLAUDE_ROUTE=pool claude` override the mode for one launch.

A pool launch whose socket does not answer falls back to the daemon's TCP port, then to your own login. A Claude Code session that started in socket mode does not fall back if the socket dies later. It retries for minutes. Watch `claude_socket` in `/_subrouter/health`. A running daemon binds a deleted or dead socket path again within about 2 seconds (§77.1), so the session hangs only while the daemon itself is down.

A credential command (`claude auth`, `claude login`, `claude logout`, `claude setup-token`) bypasses the route, so a login lands in the store its environment names (§73). `claude-route status` warns when the shim's login-free store holds a login (`pool store:`).

### Fix 401s after `sr setup`

`sr setup` mints a proxy token in `~/.config/subrouter/cloud.json`, and the daemon then refuses any request without it. The `claude-route` launches send a placeholder, so each one gets `401 unauthorized`. Setup can also write the daemon's address into each client's config file, which takes the route away from `claude-route`. To give it back, quit every Claude Code and Codex session, then:

1. Run `claude-route status`. If its `settings:` line reads `route keys in` and names `~/.claude/settings.json`, setup wrote the daemon's TCP address and the token into that file's `env`. Claude Code applies that `env` over the environment `claude-route` sets. Every launch then follows the file, and a route word such as `claude local` refuses with `can't be honoured` and exits 2. Remove `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, and `ANTHROPIC_CUSTOM_HEADERS` from that `env`. Remove all three, because `claude-route` counts each one as a route.
2. If `~/.codex/config.toml` sets `openai_base_url` or `chatgpt_base_url`, setup pointed Codex at the daemon, and `codex local` still goes there. Remove those two keys and the two `experimental_realtime_*_base_url` keys.
3. Set `SUBROUTER_ALLOW_LOOPBACK_PROXY=1` in the daemon's environment, which lets callers on this Mac in without the token. See [Change the daemon's environment](#change-the-daemons-environment). Remote callers still need the token.

### Point clients with sr setup instead

If you do not want the shims, `sr setup --storage local` writes the daemon's TCP address and proxy token into `~/.claude/settings.json`, and writes the four Codex base-URL keys into `~/.codex/config.toml`. Claude Code over TCP does not get the first-party features of the socket. A running client can read the changed files mid-session, so run it with no client open. `sr setup --plan` previews the change, and `sr doctor` checks the result.

## Deploy and roll back the laptop

[LAPTOP.md](LAPTOP.md) is the full runbook for the laptop scripts. This section covers the common tasks.

### Deploy a build

```bash
git pull
make build
ops/deploy/macos/deploy-laptop.sh bin/subrouter
```

The script installs the build where the job runs it, restarts the job or upgrades the supervised worker, and checks the routes. It ends with `DEPLOY OK`, or it restores this run's backup and exits 1. Exit 3 (`INCOMPLETE`) keeps the new build. It names Codex accounts that fail its credential-isolation check, with the fix. It also exits 3, with `claude socket not served`, when the plist names a Claude socket that neither build serves. It exits 3, with `claude socket not owned by the daemon`, when something other than the daemon holds that socket: a stand-in, or another process, or the supervisor let go of it on purpose (PATCHES §84). Its message says which, and what to do. `ops/deploy/macos/deploy-laptop.sh bin/subrouter --check` verifies the running daemon and changes nothing, and ends with `CHECK OK`, `CHECK FAILED` or `CHECK INCOMPLETE`.

Each run that changes the job continues in a detached process and logs to `~/.subrouter/transitions/<script>-<stamp>-<pid>.log`, with the exit code in `.rc`. Closing the terminal does not stop it.

### Change the daemon's environment

The daemon reads its settings from its environment. How you change it depends on whether the job runs under the supervisor. To tell which, run `launchctl print gui/$(id -u)/ai.manaflow.subrouter`. A supervised job's arguments start with `subrouter-supervisor supervise`.

- On a plain job, set or remove one variable per deploy:

  ```bash
  PLIST_SET_ENV=SUBROUTER_LOG_LEVEL=debug ops/deploy/macos/deploy-laptop.sh bin/subrouter
  PLIST_DELETE_ENV=SUBROUTER_LOG_LEVEL ops/deploy/macos/deploy-laptop.sh bin/subrouter
  ```

- On a supervised job, add the variable to `env` in `~/.subrouter/worker-config.json`, then deploy. The new worker starts with it, and nothing restarts:

  ```json
  {"args": ["serve", "..."], "env": {"SUBROUTER_LOG_LEVEL": "debug"}}
  ```

  Keep `args` as the file has it. A change to the plist itself needs `--restart-supervisor`, which closes the port until the new supervisor's first worker is ready.

Put settings in the environment, never as flags in the plist. A rolled-back binary that predates a flag exits on it, and launchd restarts it forever.

### Roll back

A deploy whose checks fail puts this run's backup back by itself. Each run also keeps the replaced binary as `~/bin/subrouter.bak-<stamp>`, the newest five by default (`KEEP_BACKUPS`). To return to one, deploy it:

```bash
ops/deploy/macos/deploy-laptop.sh ~/bin/subrouter.bak-<stamp>
```

Prefer a fix forward. An older binary can undo a one-way migration, and the script refuses a build from before Codex credential isolation once a migrated Codex record exists ([KNOWN-ISSUES.md](KNOWN-ISSUES.md)). `ops/deploy/macos/rollback-laptop-peer.sh` restores a `~/.subrouter/laptop-cutover-backup-*` directory, which only an earlier cutover made. On a new Mac there is none.

### Use supervised mode

Under `subrouter supervise`, a deploy starts a new worker while the supervisor keeps the port and the Claude socket open. A streaming reply finishes on the old worker, and new turns go to the new one (§69). The supervisor reads its plist, its flags, and `SUBROUTER_CLAUDE_SOCKET` only when it starts.

```bash
ops/deploy/macos/activate-laptop-supervisor.sh bin/subrouter --dry-run       # preview
ops/deploy/macos/activate-laptop-supervisor.sh bin/subrouter --wait-quiet 300
ops/deploy/macos/activate-laptop-supervisor.sh --rollback                    # back to a plain serve
curl -s --unix-socket ~/.subrouter/supervisor.sock http://supervisor/_subrouter/supervisor-status
```

`supervisor-status` gives the active worker's CDHash as `active_worker.executable_identity`, and the Claude socket's state as `claude_socket`. `serving` is the only word that means the supervisor serves the socket. `waiting` and `displaced` mean that another process has the socket's lock or its path, and the supervisor takes the path back as soon as it can (§77.1). `yielded` means that the supervisor let go of the socket on purpose, and `claude_socket_yield` gives the reason and the time until which it stays away. The reason is `operator` after `POST /_subrouter/claude-socket/yield?for=2m` on the control socket, which `POST /_subrouter/claude-socket/reclaim` ends early. The reason is `worker_exits` after the third failure of the active worker within 10 minutes. With nothing standing by on the socket's lock, the supervisor takes the socket back 5 seconds later and keeps it (§77.3). `worker_probe` shows the supervisor's check of its worker. `misses` counts the probes in a row that the active worker has not answered. After 3 the supervisor starts a new worker and retires the silent one, and `last_replacement` says when and why (§77.2).

### Recover from an interrupted run

Read the newest log in `~/.subrouter/transitions/`. A `.log` with no `.rc` beside it belongs to a run that is still going.

- If a script says launchd refused to answer (launchctl rc 141, 153, or 125), run it again from a new Terminal.app window (§47).
- If a script refuses because the plist and the running job disagree, run the command the refusal names.
- If the daemon is down and nothing is running, the watchdog, once installed, loads it within about 5 s unless a stop is announced (§81, [LAPTOP.md](LAPTOP.md#keep-the-job-loaded-with-the-watchdog)). Without the watchdog, load it from Terminal.app:

  ```bash
  launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/ai.manaflow.subrouter.plist
  ```

## Run a box and peer with it

A box is a second daemon on an always-on Linux machine. The laptop ranks the box's accounts beside its own. The laptop decides, so it names the one box account it chose, and the box serves exactly that account or refuses with a reason before it calls the provider (§31).

### Deploy the box

[ops/deploy/lightsail/README.md](deploy/lightsail/README.md) creates the box and deploys this fork to it with `ops/deploy/lightsail/deploy-box.sh` and `ops/deploy/lightsail/bootstrap.sh`. The box serves its private listener on its tailnet address, port 31415, and a TLS public listener on port 31416. The public listener mounts no admin route unless the box sets `SUBROUTER_PUBLIC_ADMIN=1`.

The laptop needs a `remote.key` before it arms the peer link or runs `sr server` and `sr claude move` ([GETTING-STARTED.md](GETTING-STARTED.md), "Unlock the remote commands"). The box needs none. The key gates only the daemon that sends turns away and the commands that reach a server, and the box only serves.

### Mint tokens on the box

Named access tokens give each caller its own secret, role, and expiry, and each request logs `caller=<name>` (§43). Mint them on the box. Pass the `--sessions` path the box's daemon runs with, which is `SUBROUTER_SESSIONS` in `/etc/default/subrouter`:

```bash
sudo subrouter token mint laptop --role peer --expires 90d --sessions <sessions-path>
sudo subrouter token list --sessions <sessions-path>
sudo subrouter token revoke laptop --sessions <sessions-path>
```

The token is shown once.

| Role | Can |
|---|---|
| `peer` | Spend the pool and read `usage-status` on the public listener. It is for a daemon's peer link. |
| `membership` | Spend the pool, and nothing else. |
| `admin` | Admin routes. On the public listener only with `SUBROUTER_PUBLIC_ADMIN=1`. |

`subrouter token list` marks a token `RENEW` within 14 days of its expiry.

### Peer the laptop with a box

The laptop finds its peers in its environment. A peer link reads the box's `usage-status` to rank its accounts, so use the box's public listener with a `peer` token.

1. On the box, print the public listener's certificate pin. Pass the same `--sessions` path as the token commands, because `fingerprint` reads no setting from the environment. Bootstrap's output also shows the pin.

   ```bash
   sudo subrouter fingerprint --sessions <sessions-path>
   ```

2. On the laptop, write the peers file. Keep it mode 0600:

   ```json
   {"peers": [
     {"name": "box", "url": "https://203.0.113.10:31416", "token": "srk_...", "fingerprint": "<pin>"}
   ]}
   ```

3. Name the file in the daemon's environment as an absolute path, such as `SUBROUTER_REMOTE_POOLS_FILE=/Users/<you>/.subrouter/peers.json`. Add `SUBROUTER_PEER_TAILNET=box=100.64.x.x` to dial the box over Tailscale first (§68). Then deploy. See [Change the daemon's environment](#change-the-daemons-environment).
4. Check the link. `srq pool --remote` prints the box's accounts, and `GET /_subrouter/peers/usage-status` shows `reachable` and `as_of` for each peer.

The settings:

- `SUBROUTER_REMOTE_POOLS_FILE` lists every peer. Each entry has `name`, `url`, and `token`, plus `fingerprint` for an `https` URL and an optional `allow` string. `allow` is a comma-separated list of the CIDRs or IPs of the local callers that may spend the peer's accounts. With no `allow`, or with `*`, every caller can spend them. A file that does not parse, is readable by others, or holds an invalid entry arms no peer at all.
- `SUBROUTER_REMOTE_POOL`, `SUBROUTER_REMOTE_POOL_TOKEN`, `SUBROUTER_REMOTE_POOL_FINGERPRINT`, and `SUBROUTER_REMOTE_POOL_ALLOW` configure a single peer. They are ignored while `SUBROUTER_REMOTE_POOLS_FILE` is set.
- `SUBROUTER_PEER_TAILNET` takes `name=IP` pairs, with each IP in `100.64.0.0/10` or `fd7a:115c:a1e0::/48`. While this Mac is on the tailnet, a new connection dials that IP on the URL's port first and falls back to the URL. A malformed value logs one warning and leaves every peer on its URL. The activation script's `--peer-tailnet` writes it into the worker config.

None of these arm without `remote.key`. The daemon reads them when it starts.

### Move accounts to the box

Register the box for the `sr` commands. `sr status` then shows the box's rows under its own heading:

```bash
sr server add box --url http://100.64.x.x:31415 --tailscale-node-id <node-id> --admin-token <box-admin-token>
sr server list
```

`--admin-token` puts the box's admin token in your shell history. Clear that line afterward. Leave out `--default`, even when a command you copy has it ([Avoid the dangerous commands](#avoid-the-dangerous-commands)).

`<node-id>` is the box's stable Tailscale node ID. It is the `ID` of the box's entry under `Peer` in `tailscale status --json` on the laptop. Without it, and with no public admin path, `sr claude move` to a plain `http` URL sends nothing.

Move Claude accounts one at a time. `sr claude move` uploads the login, waits for the box to confirm that it can use it, and only then removes the local copy. Any failure keeps the local profile:

```bash
sr claude move <name> box
```

Keep the profile that `SUBROUTER_IDENTITY_ACCOUNT` names on the laptop. `sr claude move` does not check for it. With the socket on, moving it leaves `control_plane_identity` `missing` in `/_subrouter/health`. Every socket session's `/api/*` call then gets 503, and Claude Code withholds WebFetch. `claude-route status` shows which profile that is.

`sr server login box` signs a Codex account in directly on the box. `sr claude remove <name> --server box` deletes an account on the box only.

When the box's tailnet address is out of reach, `sr` can fall back to the box's public admin path. That needs `SUBROUTER_PUBLIC_ADMIN=1` on the box and a named admin token on the laptop. `sr` reads the token from a file and never prints it:

```bash
sr server set-public box --url https://203.0.113.10:31416 --fingerprint <pin> --token-file <token-file>
```

Take the pin only from bootstrap's output or from `sudo subrouter fingerprint --sessions <sessions-path>` on the box, never from the endpoint. A pin mismatch can be an impersonator, and `sr` then sends nothing. A leaked named admin token opens every public admin route until you revoke it.

## Turns held through an outage

### A held turn

A long silence from the daemon can be a hold, not a hang (§58). When the upstream cannot be reached and nothing has reached the client yet, the daemon waits and runs the turn again. The wait starts at 1 second, doubles to 30 seconds, and has jitter. It continues for up to 8 minutes from the request's arrival. Then the client gets the failure it would have got at once.

The daemon holds failed lookups, refused or timed-out dials, connection resets, a token refresh with no answer, and an upstream 500, 502, 503, or 504. It answers at once on a 429, a 529, an exhausted pool, a refusal, a drain, a 401, and anything after the first byte. It never holds a websocket turn (most Codex turns), a request a peer forwarded, or anything on the public listener. Nothing runs while the Mac sleeps.

```bash
L=~/Library/Logs/subrouter.err.log
grep -h 'proxy hold' $L* | tail -20
grep -h 'proxy hold ended' $L* | grep -o 'outcome=[a-z_]*' | sort | uniq -c
grep -h 'proxy hold started' $L* | grep -o 'reason=[^ ]*' | sort | uniq -c
```

Each hold logs `proxy hold started` and `proxy hold ended`. `outcome=answered` means some answer went out, so read its `status=`. `proxy hold refused; failing this request now` means a cap was reached. At start, the daemon logs `proxy hold is on` with its settings.

The settings are `SUBROUTER_HOLD_DEADLINE` (default `8m`, and `0` turns holding off), `SUBROUTER_HOLD_MAX_REQUESTS` (256), and `SUBROUTER_HOLD_MAX_BYTES` (512 MiB). Keep the deadline under 9 minutes, because Claude Code gives up on a request with no response headers after 600 seconds.

The daemon writes two 503s itself, one for a spent pool and one for a held chat that gives up (see [A box that goes quiet](#a-box-that-goes-quiet)). Each tells an end client to wait at most `SUBROUTER_CLIENT_RETRY_AFTER_MAX` in `Retry-After`. The default is `60s`, and `0` turns the cap off (§78). A session that sleeps out a `Retry-After` then tries again each minute instead of sleeping for an hour. The spent pool's body keeps the real wait, and a request a peer daemon forwarded gets the whole wait. At start, the daemon logs `Retry-After cap is on` or `Retry-After cap is off`.

### A failed lookup, and the outage spill

Two mechanisms act before a hold when this Mac's own path to the provider breaks (§59):

- Sticky DNS. Each upstream dial remembers the public address it connected to. When a later lookup fails, the dial goes to that address if it is under a week old. TLS still checks the certificate against the real host name. The log line is `dialing its last good address`.
- The outage spill. A turn whose request never left this Mac goes to a peer for that turn only. The session keeps its local account, and its next turn tries local first. Each spill logs `outage spill` with `outcome=served`, `capped`, or `not_served`.

The settings are `SUBROUTER_STICKY_DNS_MAX_AGE` (default `168h`, and `0` turns it off), `SUBROUTER_OUTAGE_SPILL_RATE` (turns a second, default 5, and `0` turns it off), and `SUBROUTER_OUTAGE_SPILL_BURST` (default 10). The spill needs a peer.

### A box that goes quiet

A network that refuses new connections while old ones work can make a healthy box look dead. The laptop finds a box down only after the box answers nothing for `SUBROUTER_PEER_DOWN_GRACE`, 15 minutes by default (§67). Any answer restarts the count. Until then, a chat held for that box waits for its account across retries, and an opening inside the grace logs `holds kept` with `why=within_grace`.

When a box is found down, each of its chats becomes a refugee. It goes to another box first, and to the laptop only when no other box can take it. A refugee gives up after an hour with `stranded chat gave up; answered 503`, and resumes when room appears. A full admission queue also holds the turn, with `reason=admission_queue_full`, instead of answering 503.

### The stand-in, when the daemon itself is gone

None of the above helps when the supervisor is not running: a Claude Code session talks only to the daemon's socket. `subrouter standby` (§79.3) is a second process for that case. It waits on the lock of the Claude socket. When the supervisor lets go of the lock, the stand-in binds the same path, holds new requests for 30 seconds in case the daemon comes straight back, and after that sends turns to a box. It gives the path back when the supervisor asks for it and the daemon has answered ready 3 times in a row. `ops/deploy/macos/install-standby.sh` runs it as its own LaunchAgent, `ai.manaflow.subrouter-standby` (§79.5).

It needs the peers file, one `membership` token for each box in `~/.subrouter/standby-tokens/<box name>` (mode 0600, §79.2), the remote gate's key (§17), and a supervisor that waits for the socket's lock: `claude_socket_waits_for_lock` in `supervisor-status`. Beside an older supervisor it stays disarmed and touches nothing.

Install it from the checkout. Mint one `membership` token on each box (see [Mint tokens on the box](#mint-tokens-on-the-box)) and give it to the stand-in through standard input, so it never shows on screen or in your shell's history:

```bash
pbpaste | bash ops/deploy/macos/install-standby.sh token <box name> --expires <its last day, YYYY-MM-DD>
bash ops/deploy/macos/install-standby.sh install bin/subrouter --dry-run   # what it would write and run
bash ops/deploy/macos/install-standby.sh install bin/subrouter
bash ops/deploy/macos/install-standby.sh status
bash ops/deploy/macos/install-standby.sh uninstall                       # turns it off; the tokens stay
```

`install` copies the build to `~/.subrouter/standby/subrouter-standby`, which no deploy of the daemon replaces, and loads the job with the daemon's own settings, read from the daemon's plist. First it asks the build, with `subrouter standby --check`, whether the stand-in would arm, and refuses with one line when it would not: the gate is locked, the peers file is missing, no supervisor answers, or the supervisor lacks `claude_socket_waits_for_lock` and must be restarted on a newer build first. A box without a token is a warning. Run `install` again with a new build to upgrade; it refuses while the stand-in holds the socket, because the restart would cut every session it carries. It adds the job to the watchdog's list (§81), and `uninstall` takes it off. `status` shows the job, the copy's sha256, the state, each box's token word and last day (`renew_soon` in the last 7 days, `lapsed` after), the age of the saved `policy_limits` answer, and whether the watchdog watches the job. Its log is `~/Library/Logs/subrouter-standby.log`.

```bash
cat ~/.subrouter/standby-state.json                                          # its state, written at each change
curl -s --unix-socket ~/.subrouter/claude.sock http://x/_subrouter/health    # "claude_socket": "standby" while it holds the path
```

`state` is `disarmed` (with a `reason`), `armed` (it waits on the lock), `holding` (it has the path, and requests wait), `serving` (requests go to a box), `yielding` or `stopped`. `boxes` gives the word for each box's token, tested every 10 minutes while the stand-in is idle, so a token that lapsed shows as `refused` before the day it is needed. `takes` counts the times it took the path in the last 10 minutes. From the third, it keeps the path until the daemon has stayed ready for a minute or more, shown as `hand_back_after_seconds`.

To try it without breaking the daemon, let the supervisor yield its socket for a stated time, and end the yield early with `reclaim` (§77.3):

```bash
curl -s --unix-socket ~/.subrouter/supervisor.sock -X POST 'http://supervisor/_subrouter/claude-socket/yield?for=2m'
curl -s --unix-socket ~/.subrouter/supervisor.sock -X POST http://supervisor/_subrouter/claude-socket/reclaim
```

A stand-in ends itself, which frees the lock, when it cannot free the lock within 5 seconds of setting out to give the path back, and when it stops answering its own health through the path. A launch pinned to one account gets a 503 for as long as a stand-in serves, except on `policy_limits` when the stand-in holds the saved answer below.

While the daemon serves its socket, the stand-in asks it for `policy_limits` as a starting session does, once when it is armed and then every 30 minutes (`--policy-every`), and keeps the last 200 in `~/.subrouter/standby-policy.json` (§79.4). A session that starts while the stand-in holds the path gets that answer at once, so it keeps WebFetch, DesignSync and RemoteTrigger. A copy 24 hours old or more is not used, and the request goes to a box instead. `policy_copy` in the state file and in health says whether a copy is held, when it was saved, and its age in seconds.

## Saved resets

A saved reset clears an account's meters once. Anthropic has granted Claude accounts saved resets of two kinds, and Codex accounts hold rate-limit reset credits. A week reset clears the general week and discards what is left of it. A Fable reset clears only the Fable limit and gives back the part of the week that Fable spent, so it loses nothing left in the week (§64). Each claim spends one for good. Never claim one unless the account owner asked for that claim.

- `sr status` shows a `1x reset` column on Claude and Codex tables (§46). A Claude cell names Fable resets: `2 Fable 3/9`, or `1 +1F 12/10` for a week reset with a Fable reset behind it (§64). A Codex cell shows the soonest expiry the daemon read beside the count, `3 left 12/14` (§91). On a row that `sr` reads itself, the date comes from this machine's daemon, and only when the daemon counted the same credits.
- `sr claude reset <name|email>` claims one Claude account's next reset, of either kind, through the daemon that holds it, and says which kind it claimed and what it cleared. It asks first only when more than 10% of the week is left or the week rolls within a day.
- `sr codex reset <name|email>` redeems one Codex credit at any time.
- `sr reset [email]` redeems a Codex credit only for an account that is out of quota.
- `SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET=1` lets failover spend a Codex credit when the whole Codex pool is out. It is off by default.

An account that holds an unclaimed Claude week reset ranks at twice a non-holder until the reset is claimed or expires, and it enters burndown below 25% of its week (§56). A Fable reset does neither (§64). `SUBROUTER_RESET_HOLD_PRIORITY=0` turns that off. `srq pool` marks week holders `2x` and their burndown as `burndown·reset`. It marks each kind of saved reset an account holds with its count and its soonest expiry: `W1 12/10` for a week reset, `F2 3/9` for Fable resets, `R3 12/14` for Codex credits, and `U1` for a kind the daemon does not know (§91). The board shows the same marks, and its legend explains them.

Each `usage-status` row lists every reset the account holds in `resets`, one entry per kind and expiry date (§91). An entry gives the `kind` (`week`, `fable`, `codex_rate_limits`, or `unknown`), the `count`, `expires_at`, what a claim gives back, whether a claim restarts the seven days (`assumed_yes` or `assumed_no`: neither is measured), whether it needs the limit reached, and its `order`, the order the provider uses them in. `expiry` says where the date came from: `read`, `last_good`, `not_read`, or `none_given`. A Codex reading carries only a count, so the daemon that holds the account reads the dates from the credit list Codex.app reads. It reads at most once an hour per account, and at once when the count changes. It never reads for a count of 0 or for a peer's account, and it never redeems. A failed read keeps the last good dates, shown as `last_good` with `expiry_age_seconds`. A kind the daemon does not know is listed as `unknown` with its `raw_type` and `manual_only: true`, and logs `saved reset of a kind this daemon does not know; shown, never claimed automatically` at Warn once an hour. `complimentary_reset` is unchanged. Nothing claims or ranks on the list. `SUBROUTER_READ_CODEX_RESET_EXPIRY=0` stops the credit list reads, and every Codex count then reads `not_read`.

To see every saved reset in the pool, run `srq resets`. It prints one line per account, kind and expiry date, soonest first: when the reset expires and in how long, the kind, the count, the account's label, where the account lives, how much of its week and of its Fable meter is used, and when its week rolls. The last line totals each kind and date, such as `totals: 14 week resets expire 12/10; 3 Codex credits expire 12/14`. It reads this daemon and every peer that this daemon reads, through `/_subrouter/peers/usage-status`, so it needs no peer token. An account that two daemons hold is listed once, with both places.

- `expiry not read`, or `?` in a mark, means the daemon has not read that account's credit list yet, or `SUBROUTER_READ_CODEX_RESET_EXPIRY=0` stops the reads. It never means that the credit does not expire.
- `from the last good read, 3h 0m old`, or `*` after a date, is a date kept after a failed read of the credit list.
- `its daemon gives no full list`, or `~` after the marks, is a daemon from before §91. The entries are what its `complimentary_reset` says, and a date that it does not give reads `no date given`.
- `unknown <type>`, or `U`, is a kind that the daemon does not know. Nothing claims it automatically.
- `assumed_yes` and `assumed_no` in `restarts_clock` are assumptions. No claim has recorded the roll time before and after yet, so neither value is measured.

pool.json carries each row's list in `resets` (`kind`, `count`, `expires_at`, `expiry`), with `resets_partial: true` for a daemon from before §91.

Each daemon records every claim it makes, by hand or by the Codex failover and background spender, in `usage-history.jsonl` beside its `sessions.json`, with each meter's used fraction and roll time from the last reading before the claim and the first reading after it (§92.1). It logs the same as `reset claim recorded` at Info: `before` and `after` hold one group per meter. A claim that did not reset the account is marked `failed` and has no `after`. A redeem `sr reset` makes from the CLI itself (`--local`, `SUBROUTER_CODEX_SERVER=local`, or a machine with no server configured) reaches no daemon and is not recorded. The same file holds the daemon's own meter readings for 35 days, at most 32 MiB. `SUBROUTER_USAGE_HISTORY=0` turns it off.

## Troubleshoot

**A client gets `503 no accounts available`.** The daemon holds no account for the provider it picked. This is not a quota problem. An exhausted pool answers `no non-exhausted <provider> accounts available` instead. Run `srq pool`. If it prints no section for the client's provider, add an account, or run `curl -fsS -X POST http://127.0.0.1:31415/_subrouter/reload-accounts` if you added one since the daemon last read the store. If that section is there, the daemon picked the other provider for the request. The daemon picks the provider from the agent type, and the default is Codex. A request is Claude when `X-Subrouter-Agent` says `claude`, or when the header is absent and the request has one of these:

- a `User-Agent` that contains `claude-cli` or `claude-code`;
- `X-Claude-Session-ID`, `X-Claude-Code-Session-Id`, or `Anthropic-Conversation-ID`;
- any `Anthropic-Version` or `Anthropic-Beta` header;
- the path `/v1/messages` or one under it, `/v1/complete`, or `/api/hello`.

**A hand-written request to `/v1/messages` gets `429 rate_limit_error` with `"message":"Error"`.** Anthropic refuses raw requests on subscription logins because they do not look like Claude Code. Test with a real client (`claude -p`) and check the pool with `srq pool`.

**Claude Code reports `no non-exhausted claude accounts available`.** Run `srq pool`. If an account shows headroom, it can still be refused after its 429 mark lapses, until a usage fetch for it succeeds. See "Left open by §44" in [KNOWN-ISSUES.md](KNOWN-ISSUES.md).

**After `sr setup`, launches get `401 unauthorized`, or `claude local` refuses with `can't be honoured`.** See [Fix 401s after `sr setup`](#fix-401s-after-sr-setup).

**A socket session lacks WebFetch, or `/api/*` calls get 503.** In socket mode, Claude Code's account calls need `SUBROUTER_IDENTITY_ACCOUNT`. Check `control_plane_identity` in `/_subrouter/health`. Its `state` must be `pinned`.

**A pinned launch, `claude <account>`, is refused before it starts.** On this build, a pin to a Claude account on this Mac must name the identity account. `claude-route status` shows which account that is.

**`sr server` or `sr claude move` is missing.** This Mac holds no `remote.key` (§17). That is the intended state without a box.

**A script says launchd refused to answer, or `sr` fails on the Keychain or TLS in an old terminal.** The terminal outlived a logout (§47). Open a new Terminal.app window.

**`/_subrouter/ready` stays 503 while `/_subrouter/health` is 200.** No Codex record can be scored yet. Run `sr codex isolation-check --json`, and finish the migration it names or remove the dead record ([KNOWN-ISSUES.md](KNOWN-ISSUES.md)).

**`sr claude addr` fails with `login finished but was saved outside this profile's credential store`.** A `claude` on your `PATH`, or `CLAUDE_SECURESTORAGE_CONFIG_DIR` in the environment, saved the login to another store (§73). The command removes a profile it created. Run `claude-route install` again to update the shims, then sign in again.

**`srq pool` marks every Claude row from a peer `UNMEASURED (sweep cut before this row; not a verdict)`.** The rows carry `sweep_cut: true`. The peer's usage sweep hit its deadline before it checked those accounts, and they are fine (§18). Wait for the next sweep.

**An agent cannot reach anything after a switch.** Run `claude-route off` and `claude-route codex off`, then quit and reopen the client. Running sessions keep their route, so a new launch is the fix.

## Reference

### Files on the Mac

| Path | Holds |
|---|---|
| `~/Library/LaunchAgents/ai.manaflow.subrouter.plist` | The LaunchAgent. Its `EnvironmentVariables` are the daemon's settings. |
| `~/bin/subrouter`, `~/bin/sr`, `~/bin/cx` | The installed build and its two names. |
| `~/bin/subrouter-supervisor` | The supervisor, under supervised mode. |
| `~/bin/subrouter.bak-<stamp>` | Builds that deploys replaced. |
| `~/.subrouter/` | The store, with the accounts, `sessions.json`, `peer-holds.json`, and the sockets. |
| `~/.subrouter/codex/remote.key` | The remote gate (§17). |
| `~/.subrouter/claude.sock` | The daemon's Claude socket. |
| `~/.subrouter/supervisor.sock` | The supervisor's control socket. |
| `~/.subrouter/worker-config.json` | The supervised worker's `serve` arguments and extra environment. |
| `~/Library/LaunchAgents/ai.manaflow.subrouter-standby.plist` | The stand-in's LaunchAgent, written by `install-standby.sh` (§79.5). |
| `~/.subrouter/standby/subrouter-standby` | The build the stand-in runs, a copy of its own. |
| `~/Library/Logs/subrouter-standby.log` | The stand-in's log. |
| `~/.subrouter/standby-tokens/<box name>` | The stand-in's `membership` token for one box (§79.2). |
| `~/.subrouter/standby-token-expiry/<box name>` | The last day of that token, when `install-standby.sh token --expires` recorded one. |
| `~/.subrouter/standby-state.json`, `~/.subrouter/standby-supervisor.json` | The stand-in's state, and the supervisor's last answer to it (§79.3). |
| `~/.subrouter/standby-policy.json` | The daemon's last 200 to `policy_limits`, which the stand-in answers sessions with while it holds the socket (§79.4). |
| `~/.subrouter/transitions/` | Logs of deploy, activation, and rollback runs. |
| `~/.claude-route/` | The shims, the mode files, and `claude-route`'s launch log. |
| `~/.config/subrouter/cloud.json` | Written by `sr setup`. It holds the proxy token. |
| `~/Library/Logs/subrouter.err.log` | The daemon log, with rotated copies `.1` to `.3`. |
| `~/.subrouter/usage-history.jsonl` | The daemon's own meter readings and reset claims, 35 days, at most 32 MiB (§92). |

### Routes

All are on `127.0.0.1:31415` and answer loopback with no token.

| Route | Answer |
|---|---|
| `GET /_subrouter/health` | Liveness, `claude_socket`, `control_plane_identity`, and the build. |
| `GET /_subrouter/ready` | 503 while draining. With Codex accounts in the pool, also 503 until their first scores load. |
| `GET /_subrouter/usage-status` | Each account's `windows` and `normalized` block, with `new_session`. |
| `GET /_subrouter/sessions` | Each session's account, with `served_by` and, for a peer, `served_peer` and `served_account`. |
| `GET /_subrouter/accounts` | The pool: ID, provider, and auth mode. |
| `GET /_subrouter/pool.json` | The router's order, with `rank` and `reason`. `/_subrouter/pool` is the same as a page. |
| `GET /_subrouter/peers/usage-status` | Each peer's `usage-status` as the peer link last read it. |
| `POST /_subrouter/reload-accounts` | Re-reads the store. |
| `GET /_subrouter/drain-status` | Whether the daemon is draining. `sr drain`, `sr resume`, and `sr drain-status` use the drain socket. |
| `POST /_subrouter/claude-reset` | Claims a saved Claude reset. Use `sr claude reset` only, and only when asked. |
| `GET /_subrouter/burn.json` | The pool's burn per meter over this daemon's and every peer's accounts, with the trend, the share at the limit, and each account's figures (§92.2). |

### Settings

The daemon reads these from its environment when it starts.

| Variable | Default | Effect |
|---|---|---|
| `SUBROUTER_CLAUDE_SOCKET` | unset | Serves Claude Code on this Unix socket (§30). |
| `SUBROUTER_IDENTITY_ACCOUNT` | unset | The Claude account that answers Claude Code's account calls. Required in socket mode. |
| `SUBROUTER_ALLOW_LOOPBACK_PROXY` | unset | `1` lets loopback callers in without the proxy token. |
| `SUBROUTER_LOG_LEVEL` | info | `debug` adds the lines for turns kept on their account. |
| `SUBROUTER_RESERVED_ACCOUNTS` | unset | Comma-separated account IDs, each optionally `id=fraction`. The router holds back that share of each one's week (default 0.05), so you keep a way in, and releases it in the last 24 hours before the roll (§60). |
| `SUBROUTER_REMOTE_POOLS_FILE` | unset | The peers file. |
| `SUBROUTER_REMOTE_POOL` and its `_TOKEN`, `_FINGERPRINT`, `_ALLOW` | unset | A single peer. |
| `SUBROUTER_PEER_TAILNET` | unset | `name=IP` pairs to dial over Tailscale first (§68). |
| `SUBROUTER_PEER_DOWN_GRACE` | `15m` | Silence before a box is found down (§67). |
| `SUBROUTER_HOLD_DEADLINE` | `8m` | How long a turn is held. `0` is off (§58). |
| `SUBROUTER_HOLD_MAX_REQUESTS` | 256 | Held requests at once. |
| `SUBROUTER_HOLD_MAX_BYTES` | 512 MiB | Held request bodies at once. |
| `SUBROUTER_CLIENT_RETRY_AFTER_MAX` | `60s` | The longest `Retry-After` an end client gets in a spent pool's 503 or a held chat's give-up. The body keeps the real wait, and a peer daemon gets the whole wait. `0` is off (§78). |
| `SUBROUTER_STICKY_DNS_MAX_AGE` | `168h` | How long a last good address is used. `0` is off (§59). |
| `SUBROUTER_OUTAGE_SPILL_RATE` | 5 | Outage spills a second. `0` is off. |
| `SUBROUTER_OUTAGE_SPILL_BURST` | 10 | The spill's burst. |
| `SUBROUTER_RESET_HOLD_PRIORITY` | on | `0` stops ranking reset holders at 2x (§56). A value it does not recognize stops the daemon. |
| `SUBROUTER_FABLE_RESERVATION` | off | `1` holds back weekly budget for Fable (§57). A value it does not recognize stops the daemon. |
| `SUBROUTER_START_UNSTARTED_WEEK` | on | `0` stops giving the next new session first to an account whose weekly window has not started (§86). A value it does not recognize stops the daemon. |
| `SUBROUTER_SELF_START_WEEK` | on | `0` stops the daemon from sending one tiny turn itself on its own account whose weekly window has not started (§90). A value it does not recognize stops the daemon. |
| `SUBROUTER_SELF_START_MODEL` | `gpt-6.2-sol` | The model of that turn (§90). |
| `SUBROUTER_READ_CODEX_RESET_EXPIRY` | on | `0` stops the daemon from reading its own Codex accounts' reset credit list for the credits' expiry dates (§91). A value it does not recognize stops the daemon. |
| `SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET` | off | `1` lets failover spend a Codex reset credit. |
| `SUBROUTER_PUBLIC_ADMIN` | off | On a box, `1` serves admin routes on the public listener to named admin tokens. |
| `SUBROUTER_USAGE_HISTORY` | on | `0` stops recording this daemon's own meter readings and reset claims in `usage-history.jsonl` (§92.1), and with them the burn figures (§92.2). A value it does not recognize stops the daemon. |

Most of these log an unreadable value and start with the default. Six do not. `SUBROUTER_RESET_HOLD_PRIORITY`, `SUBROUTER_FABLE_RESERVATION`, `SUBROUTER_START_UNSTARTED_WEEK`, `SUBROUTER_SELF_START_WEEK`, `SUBROUTER_READ_CODEX_RESET_EXPIRY` and `SUBROUTER_USAGE_HISTORY` accept only `1` or `0`, or `true` or `false`, `yes` or `no`, `on` or `off`. Any other value stops `serve` from starting, and on a plain job launchd then restarts it forever. The other on/off settings read any value but `1`, `true`, `yes`, or `on` as off, and log nothing.

The deploy scripts read `SUBROUTER_LABEL`, `SUBROUTER_PLIST`, `SUBROUTER_ADDR` (as `host:port`), and `SUBROUTER_CONTROL_SOCKET` to find the job, and default to the LaunchAgent above.
