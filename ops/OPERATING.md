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
| `NO (pin_only)` | Takes pinned sessions only ([Keep an account for pinned sessions only](#keep-an-account-for-pinned-sessions-only)). It takes no new session from the pool. |
| `?` | The daemon published no verdict. |

The reasons are `eligible`, `burndown`, `exhausted`, `holdout`, `at_cap`, `short_floor`, `below_reserve`, `burn_projection`, `queue_closed`, and `pin_only`, plus `auth_invalid` for a login this daemon checked and found dead. A peer that refuses a forwarded turn names one of these, or `unknown_account`, `machine_policy`, or `not_oauth`. The order the router itself uses is `rank` and `reason` in `GET /_subrouter/pool.json`.

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

`srq burn` also says, under each meter, whether the pool needs its saved resets (§94.1):

```text
Claude week
  resets needed  yes: the 24-hour pressure is 0.80, under 1.0, but the 6-hour pressure 0.95 plus its trend +0.20 is 1.15, at or above 1.1
```

- Pressure is the burn of the accounts that are not at their limit: the burn divided by the share of account-time not at the limit. A burn of 0.84 with 30% at the limit is a pressure of 1.20. When every counted account was at its limit the whole window, the pressure is `all at the limit`.
- The answer is `yes` when the 24-hour pressure is 1.0 or more, or when the 6-hour pressure plus its trend (the change from the 6 hours before) is 1.1 or more. It is `no` otherwise. While the 24 hours have too little history, the 6-hour pressure alone decides at 1.0, then the 1-hour one. With no figure the answer is `not known yet`.
- Once `yes`, the answer stays `yes` until the pressure that decides it falls under 0.9 (§96). So a pressure that sits at 1.0 does not turn the answer, and the accounts chosen for a reset, on and off with each reading. The held answer reads `resets needed  still yes: the 24-hour pressure is 0.96, and it turns no under 0.9`. With no figure at all, a `yes` holds for up to 30 minutes and then reads `not known yet`. Each daemon keeps this memory for itself, in memory only, so a daemon that has just started answers from the figures alone. burn.json, the automatic decision and the choice of accounts on one daemon read the same answer.
- burn.json carries the same per meter as `need`: `answer`, `window`, `pressure` per window (a number, `"all_at_limit"` or null), `trend`, `why`, and `held: true` on a held `yes`. A daemon from before §94 has no `need`, and `srq burn` prints no line for it.
- Automatic resets read it. Outside a reset's expiry window and its last chance, an automatic claim needs `yes` on the meter the reset gives back (§94.2, [Automatic resets](#automatic-resets)), and so does the choice of an account to run to its limit for its reset (§95.1, [Accounts chosen for a reset](#accounts-chosen-for-a-reset)).

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
| `srq pool [--local\|--remote] [--json]` | Each provider's accounts, meters, and `new=` verdicts, on this daemon and each peer, with the accounts chosen for a reset (§95). |
| `srq dir <label>` | The profile directory of one account, from the daemon. |
| `srq selftest` | The regression fixtures. It needs no daemon. |
| `srq resets [--json]` | Every saved reset in the pool, soonest expiry first, with when each one's expiry window opens, then the totals per kind and expiry date (§91, §95). |
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
| `claude-route advisor on`, `claude-route advisor off` | Claude Code's experimental advisor tool in new pool launches. It is off until you turn it on. `claude local` never gets it from the shim (§97, §100). |
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

A pool launch's token names no plan, so the shim names the one every pooled account has: pool and pool-tcp launches set `CLAUDE_CODE_SUBSCRIPTION_TYPE=max`, unless you set it yourself. With it, Claude Code runs its built-in web-fetch agent on Haiku, as it does for your own Max login. Without it, the agent ran on the session's own model (§100).

Your own login sends Claude Code's advisor tool with each request. The plan brings a pool session the same tool as a standby: it is in each request, but asked to call it, the model found no such tool. While the advisor is off, pool and pool-tcp launches therefore also set `CLAUDE_CODE_DISABLE_ADVISOR_TOOL=1`, and their requests carry no advisor tool. To add it, run `claude-route advisor on`. New pool and pool-tcp launches then leave out `CLAUDE_CODE_DISABLE_ADVISOR_TOOL`, set `CLAUDE_CODE_ENABLE_EXPERIMENTAL_ADVISOR_TOOL=1` and pass `--settings '{"advisorModel":"opus"}'`. The `--settings` is needed: without it, a pool session gets at most the standby, which its model cannot call. Each request is then about 1,360 tokens larger, and each `advisor()` call has a second model read the conversation on the pool account. In one measured call, Opus read about 38,000 uncached tokens. A launch with its own `--settings` keeps that file, so it gets no advisor model unless its settings name one. `claude-route advisor off` turns it off. `claude-route status` shows the state on its `advisor:` line (§97, §100).

### Keep an account for pinned sessions only

`SUBROUTER_PIN_ONLY_ACCOUNTS` lists account IDs, separated by commas, that the daemon holding them serves only for a session pinned to them by name (`claude <account>`, which sends `X-Subrouter-Account-ID`). Use it for an account that the pool must leave alone, such as one you measure. The owner set it up on 2026-11-23 for a meter test (§98).

For a listed account, the daemon does these things:

- It places no new session there, by its ranking or by any fallback. It moves no session there when another account is spent, evicted or refused, and it spills, overflows and places no refugee there.
- It moves a session that is already there without its pin, at that session's next turn.
- It publishes the verdict `new=NO (pin_only)` (`eligible: false`, `reason: pin_only`), so no peer places a session there either.
- It claims none of the account's resets automatically (§93), never chooses the account for a reset (§95), and leaves the account out of the pool figures that need and the expiry windows read (§94, §96). The account's own `burn` stays on its row, and the usage history still records it. `sr claude reset` and `sr codex reset` still work.
- It sends the account no paid Messages probe and no tiny turn to start a Codex week (§90).
- Its sr auto-switch never makes the account the active Claude profile, which a bare `sr claude` launch would run on with no pin. It still reads the account's usage. `sr claude pick`, which you run by hand, does not read the verdict, so do not run it while the account is listed.
- It serves a pinned session as before. When the account is spent or refused, the pin gets the provider's answer, or §89's, and never moves to another account.

`srq pool` prints the account as `new=NO (pin_only)` with a legend line. Its `pool.json` row carries `"pin_only": true`, with a state note that says the account takes pinned sessions only.

To set it, see [Change the daemon's environment](#change-the-daemons-environment). On the supervised laptop job, add the variable to `env` in `~/.subrouter/worker-config.json`, such as `"SUBROUTER_PIN_ONLY_ACCOUNTS": "test-account"`, then deploy. Set it before you add the account, so that no new session reaches the account in between. An ID that the daemon does not hold yet is harmless. To undo it, remove the variable and deploy.

`claude-route` still applies its own checks to a pin. It reads `pin_only` as a refused verdict. So it refuses the launch when the account can bill past its plan, or reports no extra-usage state, unless `CLAUDE_ROUTE_ALLOW_PAID=1` is set. Otherwise it launches with a note. On this build it also refuses a pin to a Claude account on this Mac other than `SUBROUTER_IDENTITY_ACCOUNT` (see [Use claude-route](#use-claude-route)).

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

A saved reset clears an account's meters once. Anthropic has granted Claude accounts saved resets of two kinds, and Codex accounts hold rate-limit reset credits. A week reset clears the general week and the Fable meter, and discards what is left of the week. It keeps the week's roll time: on 2026-11-23 a week reset claimed at 100% used left the roll the same instant, and took the week from 100% to 3% or less and the Fable meter from 79% to 3% or less (§94.1). A Fable reset clears only the Fable limit and gives back the part of the week that Fable spent, so it loses nothing left in the week (§64). Each claim spends one for good. Never claim one unless the account owner asked for that claim, and never switch automatic resets to real mode for them (below).

- `sr status` shows a `1x reset` column on Claude and Codex tables (§46). A Claude cell names Fable resets: `2 Fable 3/9`, or `1 +1F 12/10` for a week reset with a Fable reset behind it (§64). A Codex cell shows the soonest expiry the daemon read beside the count, `3 left 12/14` (§91). On a row that `sr` reads itself, the date comes from this machine's daemon, and only when the daemon counted the same credits.
- `sr claude reset <name|email>` claims one Claude account's next reset, of either kind, through the daemon that holds it, and says which kind it claimed and what it cleared. It asks first only when more than 10% of the week is left or the week rolls within a day.
- `sr codex reset <name|email>` redeems one Codex credit at any time.
- `sr reset [email]` redeems a Codex credit only for an account that is out of quota.
- `SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET=1` lets failover spend a Codex credit when the whole Codex pool is out. It is off by default, and automatic resets in real mode turn it off (below).

An account that holds an unclaimed Claude week reset ranks at twice a non-holder until the reset is claimed or expires, and it enters burndown below 25% of its week (§56). A Fable reset does neither (§64). `SUBROUTER_RESET_HOLD_PRIORITY=0` turns that off. `srq pool` marks week holders `2x` and their burndown as `burndown·reset`. It marks each kind of saved reset an account holds with its count and its soonest expiry: `W1 12/10` for a week reset, `F2 3/9` for Fable resets, `R3 12/14` for Codex credits, and `U1` for a kind the daemon does not know (§91). The board shows the same marks, and its legend explains them.

Each `usage-status` row lists every reset the account holds in `resets`, one entry per kind and expiry date (§91). An entry gives the `kind` (`week`, `fable`, `codex_rate_limits`, or `unknown`), the `count`, `expires_at`, what a claim gives back, whether a claim restarts the seven days (`measured_no` for a week reset, measured on 2026-11-23; `assumed_no` for a Fable reset and `assumed_yes` for a Codex credit), whether it needs the limit reached, and its `order`, the order the provider uses them in. `expiry` says where the date came from: `read`, `last_good`, `not_read`, or `none_given`. A Codex reading carries only a count, so the daemon that holds the account reads the dates from the credit list Codex.app reads. It reads at most once an hour per account, and at once when the count changes. It never reads for a count of 0 or for a peer's account, and it never redeems. A failed read keeps the last good dates, shown as `last_good` with `expiry_age_seconds`. A kind the daemon does not know is listed as `unknown` with its `raw_type` and `manual_only: true`, and logs `saved reset of a kind this daemon does not know; shown, never claimed automatically` at Warn once an hour. `complimentary_reset` is unchanged. Nothing ranks on the list. Automatic resets read it for the expiry window of each kind and date (§94.2), and for each account's next reset when they choose the accounts to run to their limit (§95.1). `SUBROUTER_READ_CODEX_RESET_EXPIRY=0` stops the credit list reads, and every Codex count then reads `not_read`.

To see every saved reset in the pool, run `srq resets`. It prints one line per account, kind and expiry date, soonest first: when the reset expires and in how long, the kind, the count, the account's label, where the account lives, how much of its week and of its Fable meter is used, and when its week rolls. The last line totals each kind and date, such as `totals: 14 week resets expire 12/10; 3 Codex credits expire 12/14`. It reads this daemon and every peer that this daemon reads, through `/_subrouter/peers/usage-status`, so it needs no peer token. An account that two daemons hold is listed once, with both places.

- `expiry not read`, or `?` in a mark, means the daemon has not read that account's credit list yet, or `SUBROUTER_READ_CODEX_RESET_EXPIRY=0` stops the reads. It never means that the credit does not expire.
- `from the last good read, 3h 0m old`, or `*` after a date, is a date kept after a failed read of the credit list.
- `its daemon gives no full list`, or `~` after the marks, is a daemon from before §91. The entries are what its `complimentary_reset` says, and a date that it does not give reads `no date given`.
- `unknown <type>`, or `U`, is a kind that the daemon does not know. Nothing claims it automatically.
- `measured_no` in `restarts_clock` was measured: one week reset, claimed by hand on 2026-11-23, kept the account's roll time. `assumed_no` (a Fable reset) and `assumed_yes` (a Codex credit) are assumptions that no claim has settled yet.

Each entry of a kind the daemon knows, with a date still ahead, also carries `expiry_window` on the rows of the daemon that holds the account (§94.1): `opens_at`, `lead_seconds` and `why`. The window opens before the expiry by 1.5 times the time the pool needs, at its present demand on that meter, to spend what the holders of that kind and date still have left. It is never under 24 hours and never longer than the time left. When the demand is not known, the lead is 24 hours. The holders and the demand are this daemon's accounts and each peer's as its last usage body gave them. The daemon's background pass reads a peer again when that body is more than 5 minutes old (§95.1). A peer's rows, through `/_subrouter/peers/usage-status`, carry the peer's own windows. Inside the window an automatic claim needs only demand at the limit, and every holder of that kind and date is chosen and loaded: the expiry burndown (below). `srq resets` shows each window in its `window` column, `opens 12/8 10:00 in 15d 2h` or `OPEN since 12/8 10:00` (§95.3).

pool.json carries each row's list in `resets` (`kind`, `count`, `expires_at`, `expiry`), with `resets_partial: true` for a daemon from before §91.

Each daemon records every claim it makes, by hand or by the Codex failover and background spender, in `usage-history.jsonl` beside its `sessions.json`, with each meter's used fraction and roll time from the last reading before the claim and the first reading after it (§92.1). A usage read the endpoint refuses with 429 but that carries Anthropic's own figures for a meter counts as that reading (§99). It logs the same as `reset claim recorded` at Info: `before` and `after` hold one group per meter. A claim that did not reset the account is marked `failed` and has no `after`. A redeem `sr reset` makes from the CLI itself (`--local`, `SUBROUTER_CODEX_SERVER=local`, or a machine with no server configured) reaches no daemon and is not recorded. The same file holds the daemon's own meter readings for 35 days, at most 32 MiB. `SUBROUTER_USAGE_HISTORY=0` turns it off.

### Automatic resets

The daemon that holds an account can claim the account's next saved reset by itself at the moment the account runs out, without moving the work that runs on it (§93), when the claim is worth making (§94.2). Only that daemon decides for the account, never a peer. It considers a claim at two moments only: a real request meets the provider's usage-limit refusal on a meter a saved reset gives back (the week, or for Claude the Fable meter), or a reading says such a meter is at least 99.5% used while the next reset can be claimed now. It always takes the provider's next reset, and never one of a kind it does not know. There is no cap on claims and no list of excluded accounts.

`SUBROUTER_AUTO_RESET` sets the mode:

- `shadow`, the default when it is unset: the daemon decides and logs what it would claim, and claims nothing. Nothing waits, and every turn gets exactly what it gets with the mode off.
- `1` (real): the daemon claims the reset itself, through the same code `sr claude reset` and `sr codex reset` use, and the account's turns wait for it on the same account.
- `0`: off. Nothing is decided, logged or claimed.

The owner's condition stays on until the owner turns it off. While it is on, an automatic claim happens only when the account has under 1% left on the meter the next reset gives back: the week for a week reset or a Codex credit, the Fable meter for a Fable reset. That is known from the refusal on that meter, or from a reading at most 5 minutes old; an unread or older meter fails it. So a spent Fable meter with a week reset next is claimed only when the week is under 1% too. `SUBROUTER_AUTO_RESET_NEAR_EMPTY=0` turns off this condition alone, and only the owner sets it.

The owner's condition comes first, and nothing below changes it: a claim the condition refuses is never made. After it, the daemon claims only when the claim is worth making (§94.2). In the owner's words, "only reset if we are over burn rate, or the second derivative is taking it well over 1.0 to 1.1". The daemon reads the pool's need and pressure on the meter the reset is claimed on, as `srq burn` shows them (§94.1), with each peer's figures from its last usage body, which the background pass keeps at most about 5 minutes old (§95.1), and then goes in this order:

1. Inside the reset's expiry window, or in its last chance, it claims at any value. The window opens before the reset expires by 1.5 times the time the pool needs, at its present demand, to spend what the holders of that kind and date have left, and never less than 24 hours before. Last chance is a reset that expires before the account could reach its limit again after its roll: the roll plus 168 hours divided by the account's rate, the rate taken as at least 1.
2. When the pool's burn on that meter is not known yet, it claims nothing.
3. When the pool does not need its saved resets on that meter, it holds the claim back: the rest of the pool can carry the work. The pool needs them when its 24-hour pressure is 1.0 or more, or when its 6-hour pressure plus the change from the 6 hours before is 1.1 or more. While the 24 hours have too little history, the 6-hour pressure decides at 1.0, then the 1-hour one. Once the pool needs them, it keeps needing them until that pressure falls under 0.9 (§96, [Read the pool's burn](#read-the-pools-burn)).
4. When a claim would add less than a quarter of a week (0.25) before the roll, it holds the claim back. A week or Fable reset keeps the roll, so a claim adds what the account can spend again before it: its rate times the hours to the roll, over 168, at most 1. The rate is the account's own pressure, and never less than the pool's. A Codex credit restarts the seven days, so a claim adds the hours to the roll, over 168. So a Claude week reset is not claimed with the roll a few hours away, unless it expires first.
5. Otherwise it claims.

Pressure, not burn. An account at its limit burns nothing, however much work is sent to it, so the pool's mean burn reads low exactly when the pool is short. Pressure is the burn of the accounts that can still spend: the burn divided by the share of account-time not at the limit. A burn of 0.84 with 30% of the time at the limit is a pressure of 1.20, and the pool needs its resets. The same pool read by its burn would not.

A held-back decision changes nothing: no line is logged, nothing is claimed, nothing waits, and the turn gets what it gets without automatic resets. The account is decided again at its next reading or its next wall, so a rise in the pool's pressure or the opening of the expiry window is picked up without a restart. A reset of a kind whose `restarts_clock` is `unknown` is never claimed automatically. With `SUBROUTER_USAGE_HISTORY=0` the pool's burn is never known, so nothing is claimed outside the expiry window and last chance. `SUBROUTER_AUTO_RESET_WORTH=0` returns the decision to §93's rule: claim at the limit when there is demand. It changes nothing else that came with §94: in real mode a Claude claim at a reading moment still reads the account live first, and the two older Codex settings still spend nothing (below). Unset or `1` is the rule above. Any other value stops the daemon from starting, and a change needs a restart.

Read the decisions in the log:

```bash
grep -h 'auto reset:' ~/Library/Logs/subrouter.err.log* | tail
```

- `auto reset: would claim the next saved reset` (Info, shadow mode) is what real mode would claim now, and its `reason` ends with why the claim is worth making: the expiry window, last chance, or the pool's pressure and what a claim adds. It names the `account`, the `kind` (`week`, `fable` or `codex_credit`), the `reset` it would spend and its `reset_expires_at`, `left` (what is left on the meter that reset gives back, or why that is not known), the `moment` (`wall` or `reading`), the `reason`, and for a wall the `turn`: what the turn got instead, such as `moved to another account (<label>)`. There is at most one line per account and kind in 10 minutes, however many requests meet the limit. A decision the condition stops, or that is held back, logs nothing; `usage-status` keeps it.
- In real mode, `auto reset: claiming the next saved reset` (Warn) starts a claim, a Claude claim logs `claude reset claim` as a manual one does, and `auto reset: claim finished` (Warn) ends it with `verdict` (`claimed`, `failed` or `may_have_landed`), `landed`, `back` and `sent`, and for a claim the provider used, `reading_after`: `back` (the usage read after it shows the meter back), `still_spent` (it shows the meter still spent) or `not_taken` (no reading could be taken: the usage endpoint refused it with 429, served only figures from before the claim, or carried none for the meter; the reason says which). A reason that starts `never twice:` means the account's last automatic claim is not settled: no second one follows within 10 minutes, nor, after one that may have landed, before a reading shows the meter back.

See it without the log:

- `srq pool` prints the mode once for each daemon, as `--- automatic resets @ local (§93): shadow, it logs what it would claim and claims nothing; the owner's under-1% condition is on; §94's rule decides whether a claim is worth it`. With `SUBROUTER_AUTO_RESET_WORTH=0` the end reads `§94's rule is OFF, §93's alone decides`. Beside an account it prints its last decision in a few words, in this machine's time: `auto=would claim week 14:05`, `auto=claiming week since 14:05`, `auto=claimed week 14:05`, `auto=CLAIM FAILED Fable 14:05`, `auto=MAY HAVE LANDED credit 14:05`, `auto=refused <1% week (2.0% left) 14:05`, `auto=no claim (never twice) 14:05`, or `auto=no claim (burn not known) 14:05`. A claim that the expiry window or last chance decided ends `(expiry window)` or `(last chance)`, as `auto=would claim week (expiry window) 14:05`. In a peer's section they are the peer's own, and nothing here ranks on them. `srq pool --json` carries `auto_reset_mode`, `auto_reset_near_empty`, `auto_reset_worth`, `auto_reset_claiming`, `auto_reset_verdict`, `auto_reset_kind`, `auto_reset_at`, `auto_reset_left`, `auto_reset_reason`, and what the rule used: `auto_reset_decided_by`, `auto_reset_need`, `auto_reset_pressure`, `auto_reset_value` and `auto_reset_hours_to_roll`.
- A held-back line reads one of two ways. `auto=held back week (not needed, pressure 0.70) 14:05` means the pool does not need its saved resets on that meter: its pressure is 0.70, under 1.0, so the rest of the pool can carry the work. `auto=held back week (adds 0.07 wk, rolls in 10h) 14:05` means the pool needs them, but the account's week rolls in 10 hours and a claim now would add 0.07 of a week, under the floor of 0.25. Neither claims, logs or holds a turn. The account is decided again at its next reading or wall.
- Each `usage-status` row of an account the daemon holds has `"auto_reset": {"mode": ..., "near_empty": ..., "worth": ..., "claiming": ..., "last": {...}}`. `mode` is `off`, `shadow` or `real`. `worth` is whether §94's rule decides. `claiming` is there while a claim runs. `last` is the last decision, with `at`, `verdict` (`would_claim`, `refused_by_condition`, `held_back`, `no_claim`, `claimed`, `failed` or `may_have_landed`), `moment`, `kind`, `reset`, `left`, `reason`, `turn` and `rule`. It is left out before the first decision, and the daemon forgets it at a restart. A `held_back` reason says why: `held back: the pool does not need its saved resets on the Claude week (the 24-hour pressure is 0.70, under 1.0, ...)`, or `held back: the roll is 10h away, and a claim would add 0.07 of a week, under the floor of 0.25`. A `no_claim` whose reason says `the pool's burn on the Claude week is not known yet` is a pool with too little history for any figure.
- `rule` is what §94's rule used, on a decision the rule made: `decided_by`, the step it stopped at (`expiry_window`, `last_chance`, `need`, `value`, or for no claim `unknown_clock` and `roll_unread`); `need` (`yes`, `no` or `unknown`); `pressure` and `window`, the pool's pressure over the window that decided need (null when need is unknown); `trend`, the 6-hour pressure trend; `value`, the share of a week a claim adds now; and `hours_to_roll`. `value` and `hours_to_roll` are null when the account's roll was not read. A decision with the rule off, or one the owner's condition or another check of §93 stopped first, has no `rule`.
- `/_subrouter/health` has `"auto_reset": {"mode": "shadow", "near_empty": true, "worth": true}`.

What a client sees in real mode. A turn that waits gets nothing at all while the claim runs: no headers and no bytes. To Claude Code it is a slow answer, since it waits up to 600 s for an answer's headers (§58). A turn waits at most 3 minutes from the claim's start for Claude and 1 minute for Codex. When the provider says it used the reset, the turn is answered from the same account as an ordinary answer, and the session stays where it was, unless the usage read right after the claim still shows the meter spent. A usage read that cannot be taken, such as one the usage endpoint refuses with 429, does not stop it (§99); if the turn sent again meets the limit again, it is moved as below, and no second claim is made. When the claim fails, may have landed, or takes longer, or the read after it still shows the meter spent, the turn gets the answer it would get without §93: it is moved to another account, or gets the provider's refusal or the pool's 503. A turn a peer forwarded, a Codex websocket turn, a pinned turn and a turn that has sent output never wait; the claim still runs, and they get today's answer.

To switch to real mode, which is the owner's call: set `SUBROUTER_AUTO_RESET=1` in the daemon's environment and restart it. In real mode only automatic resets spend Codex credits: `SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET=1` no longer redeems at failover, and `--reset-credit-autospend=spend` only advises, as `warn` does. The daemon starts and logs one Warn for each that is set. In shadow mode and with automatic resets off, both behave as before. To go back, unset it (shadow) or set it to `0`, and restart. A claim already sent stays spent. If a claim says `may_have_landed`, check the `1x reset` column in `sr status` before any claim by hand.

Before a claim at a reading moment, the daemon reads the account live: a Claude week or Codex week that rolled since the reading, so that it no longer reads at least 99.5% used, is not claimed on. At the wall the provider's refusal is the evidence, and nothing more is read.

When a claim's record has a reading before and after it, the daemon compares it with the kinds table. When the roll moved and the table says the kind keeps it, or the roll stayed and the table says the kind restarts it, or a meter the kind gives back did not fall, it logs `reset claim contradicts the kinds table; the table is unchanged` at Warn, with the `kind`, what the `table` says and what the `claim` showed. The table changes only in a new build.

### Accounts chosen for a reset

A saved reset is worth something only on an account that reaches its limit. So the daemon that holds an account chooses which of its accounts the pool should run to their limit, so that the next saved reset is claimed there (§95). In the owner's words, "with resets in hand the aim is to load, not to balance". Only the daemon that holds an account chooses it, never a peer. A choice names the meter the account is run down on: `week` (a Claude week), `fable` (a Claude Fable meter) or `codex_week` (a Codex week), and an order number among this daemon's accounts chosen for that meter, 1 first.

An account is chosen when all of these hold:

1. It is this daemon's own, its credential is alive, and it has no owner's reserve (`SUBROUTER_RESERVED_ACCOUNTS`). Its last automatic claim, if any, is settled: an account whose claim was sent, or still runs, and that no reading since shows back is not chosen, since no other claim can follow on it until one does (§96). For Codex the next account is chosen in its place.
2. Its next reset, the one a claim at its limit spends, is of a kind the daemon knows, and its clock is known. For a Claude account that is the grant Anthropic names next. For a Codex account it is the credit that expires soonest. The meter is the one that reset gives back: the week for a week reset or a Codex credit, the Fable meter for a Fable reset.
3. The pool needs its saved resets on that meter (`resets needed  yes` in `srq burn`), or the reset is inside its expiry window.
4. §94's rule would claim at the wall the account is heading for. That wall is when the account would reach its limit if the pool's whole present demand on the meter went to it: what it has left, divided by the pool's pressure times the accounts counted, in weeks. A claim there must add at least a quarter of a week before the roll, or the reset must be in its last chance or its expiry window.

How many, and in what order:

- Claude week: every week-reset holder that qualifies, the one whose claim at its wall adds the most first. That is the holder with the most time left to its roll.
- Claude Fable: every account whose next reset is a Fable reset and that qualifies on the Fable meter, in the same order. An account whose next reset is a week reset is never chosen for Fable, even with its Fable meter spent: that reset is claimed on the week.
- Codex: one account at a time, the one whose claim at its wall adds the most, as the owner put it: "pick one account, burn it to the ground, use its reset, then pick a new account". It stays chosen for as long as it qualifies, even when another account's claim comes to add more, and through a short gap in the pool's need (below). When its credit is spent, or it stops qualifying for any other reason, the next one is chosen.

A short gap in the pool's need (§96). A chosen account that stops qualifying only because the pool's need on its meter turned `no` or `not known yet` keeps its place for 30 minutes. While it waits it is not loaded: no choice is published and nothing ranks on it. If the need returns within the 30 minutes, the same account is chosen again in its old order, ahead of an account chosen for the first time, and keeps that order until the 30 minutes are up. For Codex, the account being burnt down stays the one loaded, and a fresh account is not loaded in its place. After 30 minutes it is weighed again as any other account. An account that stops for any other reason is dropped at once: its reset is spent, its credential died, a reserve appeared, or its automatic claim is not settled.

The expiry burndown. In the owner's words, "when resets are running out of time, be aggressive". It starts when a reset kind's expiry window opens (see Saved resets above). The window opens before the expiry by 1.5 times the time the pool needs, at its present demand on that meter, to spend what the holders of that kind and date still have left, and never less than 24 hours before. So a pool that spends slowly, or holds many such resets, starts earlier. From then every holder of that kind and date is chosen, whatever the pool's need, the soonest expiry first, then by what a claim adds. For Codex that lifts "one at a time" to as many accounts as the window holds. A holder that would reach its limit only after its reset expires, even with the pool's whole demand on it, is still chosen, last, and its reason says that its reset is likely to expire unused.

The modes follow `SUBROUTER_AUTO_RESET`:

- `0` (off): nothing is chosen.
- `shadow`: the daemon chooses and logs, and publishes `would_choose` on the account's `usage-status` row, beside `normalized`. `normalized` is what it would be without the choice, and nothing ranks on it.
- `1` (real): the daemon publishes the choice inside `normalized`, as `reset_choice`, where every daemon that ranks the account reads it. A daemon from before §95 ignores it.

Both carry `{"meter": "week", "order": 1, "why": "..."}`.

What a choice does to routing (§95.2). Only a choice published in real mode moves anything, and it moves only new sessions:

- A chosen account that can take a new session ranks above every steady account. The drain stays above it: an account in its last 24 hours before its roll, and a reset holder in early burndown that is not chosen. An account whose week has not started still takes one session first (§86). Where machine policy is on, an account this machine is already bound to also comes first, and a chosen account that is shedding the model's requests (a capacity mark, which lasts minutes) goes after the accounts that are not.
- Among chosen accounts the holder's order decides, 1 first. The next one takes new sessions only when the ones before it carry their allowance. On the Fable pool a `fable` choice goes before a `week` choice. The local-over-peer preference, the live-session spread and the per-request debit do not reorder chosen accounts.
- A `week` or `codex_week` choice loads every model pool of the account. A `fable` choice loads the Fable pool only.
- The allowance of a chosen account is `SUBROUTER_PACED_SESSION_CEILING` (20 by default), not the taper. A 5h window that cannot sustain more sessions still holds it at 1, and a 429 that is not a quota rejection holds it at 4 for 15 minutes. At its allowance it ranks below every other account that can take a session, and its verdict says `at_cap`.
- The 5% short floor does not stop a chosen account, so a Codex account takes new sessions down to its limit. Its verdict reason is `chosen`. Exhaustion, the owner's holdout, a dead credential and a closed admission queue still refuse it.
- A chosen account is not in the reset holder's early burndown: it is loaded in its holder's order instead. In its last 24 hours it drains like any account.
- An Opus or Sonnet turn does not rank an account chosen for its Fable meter up for its spent Fable (the Fable-spent preference): that work would eat the week under its Fable reset. Every other account keeps the preference.
- A session that already has an account never moves for a choice. A warm session stays where it is when another account becomes chosen, and it stays on a chosen account when the choice ends. A session idle for 70 minutes re-ranks as a new one, as it always did.

Every daemon honours a choice published in real mode, the holder's own and its peers', whatever its own mode: the holder decides for its accounts. In shadow mode this daemon's own choices rank nowhere. `pool.json` and the board show the ranking with each `reason`, such as `chosen to run to its limit for its next saved reset`.

`SUBROUTER_RESET_LOADING=0` stops this daemon from choosing in any mode, and it then ignores a peer's choice too. Unset or `1` is on, the default. Any other value stops the daemon from starting, and it logs a Warn at startup when it is `0`. A change needs a restart.

See the choices without the log (§95.3):

- `srq pool` prints the choice at the end of the account's line, in this daemon's section and in a peer's: `chosen week #1` is an account chosen in real mode for its week, first in its holder's order; `chosen Fable #1` is one chosen for its Fable meter; a Codex account reads `chosen week #1` too. In shadow mode the same choice reads `would choose week #1`, and nothing ranks on it. The verdict of a chosen account reads `new=yes (chosen 3/20)`: 3 sessions in flight of the 20 it will take. At its allowance it reads `at_cap 20/20`. A section with no choice prints no mark and no legend line. `srq pool --json` carries `reset_choice_mode` (`real` or `shadow`), `reset_choice_meter`, `reset_choice_order` and `reset_choice_why`.
- `srq resets` has a `window` column wherever a daemon publishes expiry windows: `opens 12/8 10:00 in 15d 2h` is when the expiry burndown of that kind and date starts, `OPEN since 12/8 10:00` means it has started, and `-` means none is published (a kind the daemon does not know, no date, or a daemon from before §94). `srq resets --json` carries each `expiry_window`.
- The board marks a chosen account after its reset marks: `chosen week #1`, or `would choose week #1` in shadow mode. The mark's title is the reason, and the legend explains both.
- `pool.json` carries `reset_choice` (real mode) or `would_choose` (shadow mode) on each row, `{"meter": ..., "order": ..., "why": ...}`, as the account's holder published it, this daemon's rows and a peer's alike.

Read the choices in the log:

```bash
grep -h 'reset loading:' ~/Library/Logs/subrouter.err.log* | tail
```

- `reset loading: chose an account to run to its limit for its next saved reset` (Info, real mode), or `reset loading: would choose an account ...` in shadow mode, is logged once when an account becomes chosen, with the `account` label, `provider`, `mode`, `meter`, `order` and `reason`.
- `reset loading: an account is no longer chosen` (Info), or `reset loading: would no longer choose an account` in shadow mode, is logged once when it stops, with the meter and order it had and the `reason` it stopped, such as `it holds no saved reset` after its reset was claimed. A reason that ends `it keeps its place until <time>, should the need return` is a gap in the pool's need, and one that starts `its automatic claim started at <time> is not settled` is a claim that was sent and that no reading has shown back yet.
- Nothing is logged while a choice stays, even when its order changes.

The pool these choices and §94's decisions read. Each daemon reads its peers' usage when a routing decision, `burn.json` or the board needs it. The background pass, every 10 seconds, also reads a peer again when its last usage body is more than 5 minutes old, through the same read. So a daemon that routes nothing still counts every peer that answers, where before it judged need, value and the expiry windows on its own accounts alone once no peer had been read for 15 minutes. A turn at the wall and a read of `usage-status` never read a peer, so two daemons cannot set off each other's reads. With automatic resets off, no pass reads anything.

To go back, restart the daemon with one of these:

- `SUBROUTER_AUTO_RESET=shadow`, or unset (the default): this daemon's own choices rank nowhere, and it still honours a peer's real-mode choice.
- `SUBROUTER_RESET_LOADING=0`: this daemon chooses nothing and ranks on no choice, its own or a peer's.
- `SUBROUTER_AUTO_RESET=0`: automatic resets are off, so this daemon claims and chooses nothing and no pass reads a peer. It still honours a peer's real-mode choice.

A session that went to a chosen account stays there, as every warm session does.

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
| `GET /_subrouter/burn.json` | The pool's burn per meter over this daemon's and every peer's accounts, with the trend, the share at the limit, each account's figures (§92.2), and whether the pool needs its saved resets (`need`, §94.1). |

### Settings

The daemon reads these from its environment when it starts.

| Variable | Default | Effect |
|---|---|---|
| `SUBROUTER_CLAUDE_SOCKET` | unset | Serves Claude Code on this Unix socket (§30). |
| `SUBROUTER_IDENTITY_ACCOUNT` | unset | The Claude account that answers Claude Code's account calls. Required in socket mode. |
| `SUBROUTER_ALLOW_LOOPBACK_PROXY` | unset | `1` lets loopback callers in without the proxy token. |
| `SUBROUTER_LOG_LEVEL` | info | `debug` adds the lines for turns kept on their account. |
| `SUBROUTER_RESERVED_ACCOUNTS` | unset | Comma-separated account IDs, each optionally `id=fraction`. The router holds back that share of each one's week (default 0.05), so you keep a way in, and releases it in the last 24 hours before the roll (§60). |
| `SUBROUTER_PIN_ONLY_ACCOUNTS` | unset | Comma-separated IDs of this daemon's accounts that take pinned sessions only: no new session, failover or refugee from the pool, here or from a peer (verdict `pin_only`), no automatic reset, and no part of the pool's figures ([Keep an account for pinned sessions only](#keep-an-account-for-pinned-sessions-only), §98). On the supervised laptop job it goes in `env` in `~/.subrouter/worker-config.json`. Unset or empty lists none. |
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
| `SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET` | off | `1` lets failover spend a Codex reset credit, except in real mode of `SUBROUTER_AUTO_RESET`, which turns it off with a Warn at startup (§94.2). |
| `SUBROUTER_AUTO_RESET` | `shadow` | Automatic resets (§93, [Automatic resets](#automatic-resets)): `shadow` logs what the daemon would claim and claims nothing, `0` is off, `1` claims in place (§93.2), and turns off the failover redeem of `SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET` and the spending of `--reset-credit-autospend=spend` (§94.2). Only `shadow`, `0` or `1`; any other value stops the daemon. |
| `SUBROUTER_AUTO_RESET_WORTH` | on | An automatic claim is made only when it is worth making: the pool needs the reset and a claim adds at least a quarter of a week before the roll, or the reset is in its expiry window or last chance (§94.2). `0` returns the decision to §93's rule: claim at the limit when there is demand. The live read before a Claude claim at a reading moment, and the fold of the older Codex settings in real mode, stay. Only `1` or `0`; any other value stops the daemon. |
| `SUBROUTER_AUTO_RESET_NEAR_EMPTY` | on | The owner's condition on automatic claims: under 1% left on the meter the reset gives back. It stays on until the owner turns it off with `0`. Only `1` or `0`; any other value stops the daemon. |
| `SUBROUTER_RESET_LOADING` | on | The daemon chooses which of its accounts to run to their limit for their saved resets, in shadow or real mode of `SUBROUTER_AUTO_RESET` ([Accounts chosen for a reset](#accounts-chosen-for-a-reset), §95.1), and routes new sessions to the accounts chosen in real mode, its own and its peers' (§95.2). `0` chooses none and ranks on no choice, a peer's included. Only `1` or `0`; any other value stops the daemon. |
| `SUBROUTER_PUBLIC_ADMIN` | off | On a box, `1` serves admin routes on the public listener to named admin tokens. |
| `SUBROUTER_USAGE_HISTORY` | on | `0` stops recording this daemon's own meter readings and reset claims in `usage-history.jsonl` (§92.1), and with them the burn figures (§92.2). A value it does not recognize stops the daemon. |

Most of these log an unreadable value and start with the default. Ten do not. `SUBROUTER_RESET_HOLD_PRIORITY`, `SUBROUTER_FABLE_RESERVATION`, `SUBROUTER_START_UNSTARTED_WEEK`, `SUBROUTER_SELF_START_WEEK`, `SUBROUTER_READ_CODEX_RESET_EXPIRY` and `SUBROUTER_USAGE_HISTORY` accept only `1` or `0`, or `true` or `false`, `yes` or `no`, `on` or `off`. `SUBROUTER_AUTO_RESET` accepts only `shadow`, `0` or `1`, and `SUBROUTER_AUTO_RESET_NEAR_EMPTY`, `SUBROUTER_AUTO_RESET_WORTH` and `SUBROUTER_RESET_LOADING` only `1` or `0`. Any other value stops `serve` from starting, and on a plain job launchd then restarts it forever. The other on/off settings read any value but `1`, `true`, `yes`, or `on` as off, and log nothing.

The deploy scripts read `SUBROUTER_LABEL`, `SUBROUTER_PLIST`, `SUBROUTER_ADDR` (as `host:port`), and `SUBROUTER_CONTROL_SOCKET` to find the job, and default to the LaunchAgent above.
