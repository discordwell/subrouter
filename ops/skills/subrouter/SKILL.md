---
name: subrouter
description: This skill should be used when an agent inspects, debugs, or operates a subrouter install, for example "which account is serving this session", "why did the router pick that account", pool quota or exhaustion, a "503 no non-exhausted accounts" error, a turn that goes silent during a network outage, a saved usage-limit reset, adding a Claude or Codex account, or any use of `sr`, `srq`, or the `/_subrouter/*` routes. Read it before running `sr`, because bare `sr` is interactive and switches accounts on a stray digit, and the `Use` column in `sr status` is not the router's gate.
---

# Subrouter

Subrouter is a local proxy that pools Claude (Pro and Max) and Codex (ChatGPT) subscription accounts behind one address, `127.0.0.1:31415` by default. It pins each conversation to one account, so the provider's prompt cache stays warm. This repository is a maintained fork of the MIT-licensed `manaflow-ai/subrouter`, and `ops/PATCHES.md` records every change against upstream by section number (§).

- On a Mac the daemon runs as the LaunchAgent `ai.manaflow.subrouter` and logs to `~/Library/Logs/subrouter.err.log`.
- An optional second daemon on a small Linux box is a peer. Each daemon can serve a turn on the other's accounts over Tailscale (§16, §31). On the box, read the log with `journalctl -u subrouter`, the system unit that `sr install-systemd` writes by default. On a box that `sr setup` installed, the unit is per-user, so use `journalctl --user -u subrouter`.
- The person who runs the install is the operator. Paths below are relative to the checkout root. To find the checkout from the installed link, run `git -C "$(readlink -f ~/.claude/skills/subrouter)" rev-parse --show-toplevel`.

## Get three things right

### 1. The `Use` column in `sr status` is not the router's gate

On this fork the `Use` column marks an account that fails the CLI's check as `not routable`. Upstream builds and older fork builds say `protected < 40%` instead. Either label answers `UsableForNewSession`, which is the CLI's switch recommendation. In `sr status --json`, the `protected` state is the same check. Each provider's `summary` counts every account that is not cooked, temp, or error as `usable`. Neither is the router's gate.

The router admits an account when `UsableForNewSession` holds, when the account is drain-eligible (`endgameDrainEligible`), or when a choice published in real mode loads it for a saved reset below its allowance (§95.2, below). An account whose admission queue is closed takes no new session either way, and its verdict reason is `queue_closed` (§72). A drain-eligible account meets all of these conditions:

- It is in its burndown window. That is the last 24 hours before its weekly roll, or, for an account that holds an unclaimed reset that gives back the week, the point where under 25% of its week is left (§56). A Fable reset holds nothing (§64).
- It is at or above the 5% short-headroom floor (`MinShortHeadroomFloor`). A Claude account with no 5h reading is not floored and drains to its weekly wall (§62).
- It carries fewer sessions with a request in flight than its allowance, `allowed_sessions` in its verdict. The taper allows 1 to 4 by the budget left (§41). Since §72, a Claude account that the taper would leave with budget unspent at the roll gets more, up to `SUBROUTER_PACED_SESSION_CEILING` (default 20), and each change logs `paced allowance` with its inputs. `SUBROUTER_PACED_ALLOWANCE=0` keeps only the taper. A 5h window that cannot sustain more sessions holds the account at 1, and a Claude 429 that is not a quota rejection holds it at the taper for 15 minutes.
- It is above its weekly reserve, if `SUBROUTER_RESERVED_ACCOUNTS` gives it one. The daemon lifts the reserve in the last 24 hours before the roll (§60).

So an account at 8% inside its burndown window outranks every healthy account except one whose weekly window has not started, which takes the next new session first (§86). Never report the table as the routing decision. The router's own order is `rank` and `reason` on `/_subrouter/pool.json`. Each account's verdict is `normalized.new_session` on `/_subrouter/usage-status`, with `eligible`, `reason`, `burndown`, `at_cap`, `live_sessions`, and `allowed_sessions`. `srq pool` prints that verdict.

### 2. Bare `sr` and bare `sr claude` are interactive

Bare `sr` prints the usage table and ends at `Switch to (#):`. A stray digit switches the active account. Escape, Ctrl-C, or Ctrl-D on an empty line dismisses the prompt. Use `sr status` or `sr status --json`, which never prompt, or read the JSON routes below.

Bare `sr claude` is a launcher. On an install that `sr setup` configured, it starts an interactive Claude Code session through the pool. With no storage setting, it prints the Claude profiles and ends at its own `Switch to (#):` prompt, which has no Escape handling. Either way it can start the daemon first. Never run it. Read the profiles with `sr claude list`.

A Codex switch does more harm than a Claude one. The digit, `sr switch`, `sr use`, `sr g`, and `sr pick` each copy the pool account's credential chain into `~/.codex/auth.json`, OpenCode, and pi. The record then becomes `interactive-import` and stops serving until `sr codex migrate-isolation` enrolls it again. Never switch to a pool account.

### 3. Never run setup, login, and the like against the host

These commands rewrite the files that every local agent authenticates through (`~/.claude/settings.json`, `~/.codex/config.toml`, and `~/.config/subrouter/cloud.json`). If the daemon rejects the new token, every running Claude Code session fails with 401 at once, the agent's own session included.

- `sr setup`, `sr storage`, `sr login`, `sr logout`, `sr server add`, `sr server use`, and `subrouter serve`.
- `sr switch`, `sr use`, `sr g`, and `sr pick` on a Codex pool account (above).
- `codex login` or `codex logout` with `CODEX_HOME` pointing at a pool store. Either one kills that account's chain.
- `sr server install`. Upstream's version installs upstream's release binary on the box, and this fork's refuses unless forced (§37.3).
- Upstream's `deploy/macos/*autoupdate*.sh` and `deploy/macos/migrate-*-to-supervisor.sh`. They install upstream's release or rewrite the plist that this fork's supervisor runs from (§69).
- Any script under `ops/deploy/` against the live machine, unless the operator asks for that deploy. `ops/LAPTOP.md` describes the laptop scripts.

To check a command's usage or plan, build the tree and run it with every store path moved. An exported store variable overrides `HOME`, so unset each one:

```bash
go build -o /tmp/sr-check/sr ./cmd/subrouter
env -u SUBROUTER_STATE_DIR -u SUBROUTER_CLOUD_CONFIG -u CODEX_HOME \
  HOME="$(mktemp -d)" CLAUDE_CONFIG_DIR="$(mktemp -d)" /tmp/sr-check/sr setup --plan
```

The binary must be named `sr` to take the `sr` grammar. The recipe moves the stores, not the LaunchAgent label `ai.manaflow.subrouter` or the live daemon's `127.0.0.1:31415`. A lifecycle verb from it, such as `sr daemon restart`, can boot out the live daemon. Use the recipe only for `--help`, `--plan`, and usage errors.

Prefer a Go test or `make docker-test` to running the CLI. Run tests of `./internal/proxy`, `./internal/agents/claude`, and `./cmd/subrouter` through `ops/bin/sr-local-ci <packages>`, never bare `go test`, because those packages reach the keychain.

On a machine without a `remote.key` file in its store, the remote verbs (`sr server`, `sr remote`, `sr login`, `sr logout`, `sr storage`) are absent from `sr --help` by design (§17). A missing command there is not a broken install.

### A shell from an ended login session

If `launchctl managername` does not print `Aqua`, the shell belongs to a macOS login session that has ended. A shell from a tmux server, or from a terminal app's daemon, that started before the last logout is in this state. It has no Keychain, no TLS verification, and no launchd. In such a shell, read the daemon with `srq pool` or `sr status`, and run no writing verb and no deploy script. Ask the operator to open a new Terminal.app window (§47).

## Prefer the JSON routes, through `srq`

Loopback callers need no token. Routes are hyphenated, and a guessed route such as `/_subrouter/status` or `/_subrouter/usage` returns 404.

| Route | Answers |
|---|---|
| `/_subrouter/usage-status` | Each account's windows, its `normalized` capacity, and its new-session verdict (§13). This is the authoritative source. |
| `/_subrouter/sessions` | Each `session_id` with its `account_id` and `served_by`, plus `served_peer` and `served_account` for a session that a peer serves (§49). |
| `/_subrouter/pool.json` | The router's own ranking, as `rank` and `reason`. |
| `/_subrouter/peers/usage-status` | Each peer's usage-status as this daemon's peer link last read it, with `reachable` and `as_of`. Loopback only (§32). |
| `/_subrouter/burn.json` | The pool's burn per meter, over this daemon's and every peer's accounts (§92.2). |
| `/_subrouter/accounts` | The account registry. |
| `/_subrouter/health`, `/_subrouter/ready` | Liveness, and readiness to serve. |

Use `ops/bin/srq` instead of hand-written queries. Earlier hand-written snippets for both of its main questions returned plausible wrong answers instead of failing.

```bash
srq serving     # which account serves this session (reads CLAUDE_CODE_SESSION_ID)
srq pool        # per-provider status and each account's new-session verdict
srq resets      # every saved reset in the pool, soonest expiry first, with when its expiry window opens
srq selftest    # regression fixtures; needs no daemon
srq burn        # how fast the pool spends each weekly meter, with its trend
```

To query a daemon over the tailnet, set `SUBROUTER_ADDR`, and `SUBROUTER_ADMIN_TOKEN` or `SUBROUTER_ADMIN_TOKEN_FILE`. Never derive the session id from transcript filenames. `CLAUDE_CODE_SESSION_ID` is in the environment, and a glob of `~/.claude/projects/*/*.jsonl` returns whichever session wrote last.

`srq serving` prints `<account> on <peer>  [remote]` and exits 0 for a session that a peer serves (§49). It prints `unassigned (session ...)` and exits 1 when the daemon has no row for the session. Common causes are no model turn through this daemon yet, a client that does not route through subrouter, or a peer hold that lapsed after 70 idle minutes.

Other read-only tools in `ops/bin`: `sr-triage` explains why `sr` does nothing, `sr-plot` shows each account's 7-day burn pace, and `srdash` shows every pool this machine can reach. `ops/README.md` lists them all.

### Explain why the router picked an account

- `srq pool` sorts by time to roll, so accounts in their last 24 hours drain first and the earliest roll leads. Each session with a request in flight discounts a draining account, so under load a sibling that rolls soon after takes the next session (§41). A draining account prints `burndown` with its live and allowed session counts (§53).
- A reset holder in early burndown prints `burndown·reset`, and `2x` marks a holder ranked at twice a non-holder's pace (§56).
- In the last day before an account's weekly roll, the daemon lifts the account's reserve, and the account drains to its wall (§60).
- In real mode an account its holder chose to run to its limit for its next saved reset takes new sessions ahead of every steady account, with the reason `chosen to run to its limit for its next saved reset`, and `srq pool` ends its line with `chosen week #1`. `would choose week #1` is shadow mode, and ranks nothing (§95, under Saved resets).
- An account whose weekly window has not started reads its whole week as left. It takes the next new session ahead of every other account that can take one, with the clause `its weekly window has not started; one new session starts its clock`. Only machine policy, a worse selection tier, a capacity or rate-limit mark, the new-session test, and the machine-binding clauses rank above it. After that session the account is marked for 10 minutes and ranks on its pace like any other account. If a reading still says unstarted when the mark expires, it takes the next session first again (§86). A peer's unstarted account counts too: the session is forwarded with that account as its target, and both daemons mark it. A warm session never moves for this. `srq pool` prints `unstarted` beside the roll, `normalized.weekly_unstarted` is `true` on the account's `usage-status` row, and a pool.json row carries `weekly_unstarted` and the state `pristine`. Each session sent to start a week logs `weekly window not started; this new session starts its clock`. `SUBROUTER_START_UNSTARTED_WEEK=0` turns this off, and the fact is still published. The daemon also starts such a week itself (§90): within seconds of the reading it sends one tiny Codex turn ("Reply with one word: ok", `gpt-6.2-sol` at low effort, no tools) on each of its own unstarted accounts, only to that account's own upstream, and marks the account the same way. It sends again 10, 20, 40 minutes later and so on (at most 6 hours apart) while a newer reading still says unstarted, counts a failed send, never sends for a peer's account, and leaves no session and no transcript. A send that failed, or did not start the clock, leaves §86's priority to start it once the mark expires. `srq pool` prints `start sent HH:MM` beside `unstarted` (`x3` for the third in a row), `normalized.self_start` on the `usage-status` row carries `sent_at` and `sends`, and the pool.json row carries `self_start` with a `state_note` that says it in words; a peer's send is shown on its row and ranks nothing. Each send logs `weekly window not started; the daemon sent one request to start its clock` (Info) or `weekly window not started; the daemon's request to start its clock failed` (Warn) with `account`, `model`, `status`, `sends`, `duration_ms` and `next_send_after`, and a reading that shows the clock running logs `weekly window started after the daemon's request` with `sends`. `SUBROUTER_SELF_START_WEEK=0` turns it off, independently of `SUBROUTER_START_UNSTARTED_WEEK`, and `SUBROUTER_SELF_START_MODEL` names another model.
- A warm session moves only on a forced switch, such as a spent window, a dead credential, or a peer that refuses or is down (§20). A session idle past 70 minutes is cold and ranks again across the local accounts and every peer (§31).
- The `remote pool decision` log line names `target_peer`, `target_account`, `session_state`, `clause`, and `runner_up`. A warm session that stays on its local account logs it at Debug, so set `SUBROUTER_LOG_LEVEL=debug` to see those lines (§39).

## Read the meters per provider

Window names differ by provider. No single window name means the week for every provider.

| Provider | Windows |
|---|---|
| Claude | `5h`, `7d`, and `oauth-apps-weekly` (the Fable meter, from the usage answer's Fable row, else `seven_day_oauth_apps`; §64). `opus-weekly` and `sonnet-weekly` appear only when Anthropic reports those buckets (§24). |
| Codex | `primary`, `<model>/primary`, and `reached`. No 5h window. |
| Codex, usage-based seat (`rate_limit: null`) | `spend` and `spend/reached`, the monthly workspace spend cap (§42). |

- A missing window is unknown, never empty. A lookup of `7d` on a Codex row returns nothing, which reads as "no headroom" and blanks the whole Codex pool. Read each row's `normalized` block (`headroom`, `short_headroom`, `weekly_headroom`, `seconds_to_weekly_reset`, `fresh`) first, and fall back to `windows`, as `srq` does (§13).
- Most Claude Max accounts have returned no `five_hour` bucket since about 2026-10-31. No `5h` on a measured Claude row means not applicable, not unmeasured (§62).
- Opus and Sonnet have no meter of their own. Both spend the account's `7d` budget (§24).
- A spent Fable meter refuses Fable turns while `5h` and `7d` look healthy, and it can read above 100%. `srq pool` shows `fable=` on each Claude row and flags `FABLE-REFUSED` (§40).
- A row with `auth_checked` false or `sweep_cut` true is unmeasured, not a dead login. When every Claude row from a peer reads `context canceled` or `AUTH-INVALID` at once, the sweep was cut, and the accounts are not dead (§18). Confirm with `/_subrouter/pool.json` or `curl -m 90` on `/_subrouter/usage-status`. Never `POST` to `/_subrouter/account-status`, because a POST forces a credential refresh.

## A silent turn may be a held turn (§58)

The daemon holds a turn whose upstream it cannot reach, instead of failing it. Causes are a DNS failure, a refused or timed-out dial, a token refresh with no answer, and an upstream 500, 502, 503, or 504. The client sees nothing while the daemon runs the whole turn again with backoff (1 s doubling to 30 s, jittered) for up to 8 minutes. Then the client gets the error it would have got at once.

The daemon never holds a 429, a 529, an exhausted pool's 503, a 401, or anything after the first byte. It also never holds websocket turns, turns that a peer forwarded, `X-Subrouter-No-Retry` canaries, or turns on the box's public `:31416` listener. A laptop that is asleep holds nothing.

So a long silence during an outage is a hold, not a hang. Do not kill the client or restart the daemon. Each hold logs one start line and one end line:

```bash
grep -h 'proxy hold' ~/Library/Logs/subrouter.err.log* | tail
```

The `proxy hold started` line carries the `reason`, and the `proxy hold ended` line carries the `outcome` and how long the turn was `held`.

- Before a hold, a failed lookup dials the host's last good public address and logs `upstream lookup failed; dialing its last good address`. A turn that never left the machine can go to the peer for that turn only, logged as `outage spill` (§59).
- A chat held on a box that cannot be reached waits for its account across the client's retries, with a `reason` that starts with `warm_wait:`. The box is found down only after 15 minutes of silence (§67).
- A 503 the daemon writes itself, a spent pool's or a stranded chat's give-up, tells a session to wait at most 60 s in `Retry-After`, so a session that sleeps out a Retry-After tries again each minute. The spent pool's body keeps the real wait in `(retry after <N>s)`, and a peer daemon gets the whole wait in the header. `SUBROUTER_CLIENT_RETRY_AFTER_MAX` sets the cap, and `0` turns it off (§78).
- Settings are `SUBROUTER_HOLD_DEADLINE` (`0` turns holding off), `SUBROUTER_HOLD_MAX_REQUESTS`, `SUBROUTER_HOLD_MAX_BYTES`, `SUBROUTER_STICKY_DNS_MAX_AGE`, `SUBROUTER_OUTAGE_SPILL_RATE`, and `SUBROUTER_OUTAGE_SPILL_BURST`. Set them in the daemon's environment. Never put the `--hold-*` flags in the plist or the unit, because a rolled-back binary exits on an unknown flag and the service manager starts it again forever (§58).

## Saved resets: never claim one

Both providers' tables in `sr status` have a `1x reset` column. A Codex row counts the account's reset credits, with the soonest expiry its daemon read, `3 left 12/14` (§91), and `unknown` means not read. A Claude row shows the account's saved usage-limit resets. `1 left` is a reset that gives back the week, `2 Fable 3/9` is resets that clear only the Fable limit, and `1 +1F 12/10` is a week reset with a Fable reset behind it (§64). `?` means not read, never "none". `1 left !elig` means the account holds a reset that Anthropic calls ineligible. Until a Claude week reset is claimed, the router ranks its holder at 2x and drains it first once under 25% of its week is left (§56). A Fable reset does neither (§64). `SUBROUTER_RESET_HOLD_PRIORITY=0` in a daemon's environment turns both off for that daemon. The daemon that holds an account can claim its next saved reset by itself when the account runs out (§93), only for its own accounts. `SUBROUTER_AUTO_RESET` is unset or `shadow` by default: at an account's usage limit, or on a reading at 99.5% used with a reset claimable now, the daemon logs `auto reset: would claim the next saved reset` (Info, at most once per account and kind in 10 minutes) and claims nothing; the turn is unchanged. `SUBROUTER_AUTO_RESET=1` is real mode: the daemon claims through the same code `sr claude reset` and `sr codex reset` use, and the account's turns wait on the same account for it (no headers until it answers; at most 3 minutes for Claude, 1 for Codex), then are served there, or get today's answer if the claim fails, may have landed or runs long. `0` is off. The owner's condition stays on until the owner turns it off: a claim needs under 1% left on the meter the next reset gives back (the week, or the Fable meter for a Fable reset), read at most 5 minutes ago. `SUBROUTER_AUTO_RESET_NEAR_EMPTY=0` drops only the condition; neither switch is yours to change. The condition always comes first. After it a claim must be worth making (§94.2): inside the reset's expiry window (it opens 1.5 times the time the pool needs to spend what that kind's holders have left before the expiry, at least 24 hours before) or in its last chance (it expires before the account could reach its limit again after the roll) it is claimed at any value; otherwise the pool must need its saved resets on that meter (`srq burn`'s `resets needed`: 24-hour pressure 1.0 or more, or 6-hour pressure plus its trend 1.1 or more) and a claim must add at least 0.25 of a week before the roll (a week or Fable reset: the rate times the hours to the roll over 168; a Codex credit: the hours to the roll over 168). Pressure, not burn, because an account at its limit burns 0: pressure is the burn of the accounts that can still spend. Else the verdict is `held_back` (no line, nothing claimed, the turn unchanged, decided again at the next reading or wall), or `no_claim` when the pool's burn is not known yet. `SUBROUTER_AUTO_RESET_WORTH=0` goes back to §93's rule alone, and a restart. In real mode a Claude claim at a reading reads the account live first and skips a week that rolled since. Look for `auto reset: claiming the next saved reset` and `auto reset: claim finished` (Warn, `verdict` is `claimed`, `failed` or `may_have_landed`); `never twice:` in a reason means the last automatic claim on that account is not settled. `srq pool` prints each daemon's mode once, with whether §94's rule decides, and, beside an account, `auto=` with its last decision: `would claim`, `claiming`, `claimed`, `CLAIM FAILED`, `MAY HAVE LANDED`, `refused <1%`, `held back` or `no claim`. `auto=held back week (not needed, pressure 0.70)` is a pool that does not need the reset; `auto=held back week (adds 0.07 wk, rolls in 10h)` is a claim that would add too little before the roll; `(expiry window)` or `(last chance)` after a claim says it was made at any value; `no claim (burn not known)` is a pool with too little history. A peer's section shows the peer's own. The `usage-status` row carries `auto_reset` (mode, condition, `worth`, last decision); a decision §94's rule made carries `rule` with `decided_by`, `need`, `pressure`, `value` and `hours_to_roll`. `/_subrouter/health` names the mode, the condition and `worth`. Apart from real mode, a Codex credit is spent by the daemon only when the operator opts in with `SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET` or `--reset-credit-autospend spend`; real mode turns both off (one startup Warn each), so only automatic resets spend; the default `warn` spends nothing. A claim record that contradicts the kinds table logs `reset claim contradicts the kinds table; the table is unchanged` at Warn. Turning automatic resets to `1` is the owner's call, never yours. Going back is unsetting it or `0`, and a restart; a claim already sent stays spent.

Accounts chosen for a reset (§95). The daemon that holds an account chooses which of its accounts to run to their limit, so that their next saved reset is claimed there ("load, not balance"); a peer never chooses for it. An account is chosen when its credential is alive, it has no owner's reserve (`SUBROUTER_RESERVED_ACCOUNTS`), its next reset (for Claude the grant Anthropic names next, for Codex the credit that expires soonest) is of a known kind, the pool needs its resets on that reset's meter or the reset is inside its expiry window, and a claim at the wall it is heading for (its headroom over the pool's pressure times the accounts counted) adds at least 0.25 of a week, or the reset is in its last chance or expiry window. Claude week holders are all chosen, the one whose claim adds the most first; an account whose next reset is a Fable reset is chosen for the Fable meter, and a week-reset holder never is; Codex accounts are loaded one at a time, the most valuable, kept at least 30 minutes while it qualifies and then until another adds more, its credit is spent or it stops qualifying. Inside an expiry window every holder of that kind and date is chosen whatever the need, soonest expiry first, for Codex as many as it takes; one that would reach its limit only after the expiry is last, its reason saying its reset is likely to expire unused. In shadow mode the row carries `would_choose` (`meter` `week`, `fable` or `codex_week`, `order`, `why`) beside `normalized`, which is unchanged; in real mode the same is `reset_choice` inside `normalized`. Off chooses nothing, and `SUBROUTER_RESET_LOADING=0` chooses nothing in any mode and ignores a peer's choice (any value but unset, `1` or `0` stops the daemon). A choice published in real mode moves new sessions only (§95.2), on every daemon whatever its own mode: a chosen account that can take one ranks above every steady account, below the drain (the last 24 hours before a roll, and early burndown of a holder that is not chosen) and below an unstarted week; among chosen accounts the holder's order decides, the next taking sessions only when the ones before it carry their allowance, a `fable` choice first on the Fable pool, with no local preference, session spread or debit between them; a `week` or `codex_week` choice loads every pool of the account, a `fable` choice the Fable pool only. Its allowance is `SUBROUTER_PACED_SESSION_CEILING` (20), still held at 1 by a 5h window that cannot sustain more and at 4 for 15 minutes after a non-quota 429, and at it the account ranks below every routable one with `at_cap`; the 5% short floor does not stop it (verdict reason `chosen`), while exhaustion, the holdout, a dead credential and a closed queue do. A chosen account is not in a holder's early burndown, and an Opus or Sonnet turn ranks an account chosen for Fable without the Fable-spent preference. A session that has an account never moves for a choice. Look for `reset loading: chose an account ...` or `reset loading: would choose an account ...` (Info, once when it begins) and `reset loading: an account is no longer chosen` or `would no longer choose an account` (once when it stops, with why). The background pass (every 10 seconds) reads a peer again when its last usage body is more than 5 minutes old, so need, value and expiry windows count every peer that answers; a turn at the wall and `usage-status` never read a peer, and with automatic resets off no pass reads anything. To see the choices (§95.3): `srq pool` ends a chosen account's line with `chosen week #1`, its meter and its place in its holder's order (`chosen Fable #1` for the Fable meter; a Codex account reads `week`), or `would choose week #1` in shadow mode, where nothing ranks on it; a chosen account's verdict reads `new=yes (chosen 3/20)`, sessions in flight of its allowance, and `--json` carries `reset_choice_mode`, `reset_choice_meter`, `reset_choice_order` and `reset_choice_why`. `srq resets` has a column headed `window` where a daemon publishes expiry windows: `opens <date> in <how long>`, `OPEN since <date>` once that kind and date's expiry burndown has started, `-` for none. The board marks the account `chosen week #1` or `would choose week #1` with the reason in the title, and each pool.json row carries `reset_choice` or `would_choose` as its holder published it, this daemon's and a peer's. To go back, restart with `SUBROUTER_AUTO_RESET=shadow` (the default; this daemon's own choices then rank nowhere, a peer's real-mode choice still does) or `SUBROUTER_RESET_LOADING=0` (no choice made or ranked on); a session already on a chosen account stays there.

To see every reset an account holds, read the `resets` list on its `usage-status` row: one entry per kind (`week`, `fable`, `codex_rate_limits`, `unknown`) and expiry, with its count, what a claim gives back, and `order`, the order the provider uses them in (§91). A Codex date is read by the daemon that holds the account from the credit list, at most once an hour. `expiry` is `not_read` until that read succeeds, and `last_good` with `expiry_age_seconds` after a failed one. An `unknown` entry, with `manual_only` set, is a kind the daemon does not know. `SUBROUTER_READ_CODEX_RESET_EXPIRY=0` in the daemon's environment stops the credit list reads, and every Codex count then reads `not_read`. Nothing ranks on the list; automatic resets read it for each kind and date's expiry window (§94.2) and for each account's next reset when choosing accounts to load (§95.1); and `complimentary_reset` is unchanged. `srq resets` prints every reset in the pool, this daemon's and each peer's, soonest first, with the kind, count, account label, where it lives, the week and Fable meters and the roll, and a last line of totals per kind and date. `srq pool` and the board mark each kind with its count and soonest expiry: `W1 12/10`, `F2 3/9`, `R3 12/14`, `U1` for an unknown kind, `?` for an expiry not read (never "no expiry"), `*` for a date from the last good read, and `~` for a daemon before §91 that gives no full list. `restarts_clock` is `measured_no` for a week reset: one claimed on 2026-11-23 kept the account's roll time and gave back the Fable meter as well as the week (§94.1). `assumed_no` (Fable) and `assumed_yes` (Codex) are not measured. Each entry of a known kind with a date also carries `expiry_window` (`opens_at`, `lead_seconds`, `why`) on the rows of the daemon that holds the account: 1.5 times the time the pool needs at its present demand to spend what that kind's holders have left, never under 24 hours. Inside it an automatic claim needs only demand at the limit (§94.2).

- Never claim a reset unless the operator asks for that account. `sr claude reset <name|email>`, `sr codex reset <name|email>`, and `sr reset` each spend a credit that does not come back. `sr codex reset` spends one even on an account below its limit (§46.8).
- Never send a request to `/_subrouter/claude-reset` or `/_subrouter/rate-limit-reset` directly.
- Never show or suggest `--dry-run` to the operator. It is a hidden testing flag (§46.11).
- Name the account by its exact profile name or email. A near miss gets `did you mean ...? nothing was sent`.
- The command asks y/N only when the week has more than 10% left, rolls within a day, or is unread. Without a terminal it refuses, and `--yes` answers. Pass `--yes` only after the operator answers yes to that question.
- The command prints one line. If it says `may have landed`, check the `1x reset` column in `sr status` before any retry.
- The daemon that makes a claim records it in `usage-history.jsonl`, with each meter's used fraction and roll time before and after, and logs `reset claim recorded` (§92.1). `SUBROUTER_USAGE_HISTORY=0` turns the record off. A redeem that `sr reset` makes from the CLI itself (`SUBROUTER_CODEX_SERVER=local`, or a machine with no server configured) reaches no daemon and is not recorded.

## Read the pool's burn (§92.2)

`srq burn` gives, per meter (Claude week, Claude Fable, Codex week), the pool's burn over the last hour, 6 hours and 24 hours, the trend against the 6 and 24 hours before, and the share of account-time at the limit. `srq pool` ends with the 24-hour figures on one line.

- 1.00 spends the week's budget exactly as fast as it lasts, 2.00 twice as fast, 0 not at all. Each account weighs the same.
- A low burn with a high share at the limit means spent accounts, not an idle pool: an account at its limit burns 0.
- `not enough history yet` is not 0. A figure needs readings over half its window (12 hours for 24 hours), and the 24-hour trend needs 36 hours.
- A peer named `not counted` publishes no burn (a build from before §92, or its history off) or has not answered for 15 minutes. Its accounts are left out, not read as 0.
- Each daemon reads only its own accounts' history, `usage-history.jsonl` beside its `sessions.json` (the folder of `--sessions`, `~/.subrouter` by default). A daemon that was down leaves a gap. Nothing ranks or routes on the figures. `SUBROUTER_USAGE_HISTORY=0` turns them off.
- `resets needed` under a meter says whether the pool needs its saved resets (§94.1). Pressure is the burn of the accounts not at their limit. `yes` at a 24-hour pressure of 1.0 or more, or a 6-hour pressure plus its trend of 1.1 or more; with too little history the 6-hour, then the 1-hour, pressure decides at 1.0; `not known yet` with no figure. burn.json carries it as `need`, with the sentence in `why`. An automatic claim outside a reset's expiry window and last chance needs `yes` (§94.2).

## Add an account

Give these commands to the operator to run. Each sign-in needs a person at a browser.

| Goal | Command |
|---|---|
| A Codex account into the pool | `sr codex add [--device-auth] [--email <email>]`. It signs in through a throwaway `CODEX_HOME`. |
| A Claude account with browser OAuth (a refresh token) | `sr claude addr [<name>]`. It prints a URL and waits for a pasted code. |
| A Claude account from a 1-year setup token | `sr claude add <name>`. |
| A Claude account onto the box | `sr claude move <name> [<server>]` on a machine with `remote.key`. |
| The operator's own Codex CLI login | `sr codex login`. It runs the real Codex CLI against `~/.codex`, never a pool account. |

A refresh-token Claude profile is never in two places. Anthropic rotates refresh tokens on use, so a copy dies at the next refresh. `sr claude move` uploads the profile, confirms that the server can use it, and only then removes the local copy (§22). `sr claude add` takes a setup token, and `sr claude addr` is the OAuth login.

## Where the docs live

- `ops/GETTING-STARTED.md` sets up a laptop daemon and an optional Linux box.
- `ops/OPERATING.md` covers daily operation in long form.
- `ops/LAPTOP.md` covers the laptop supervisor, deploys, and rollback.
- `ops/PATCHES.md` records every change against upstream and why. Cite it by § number.
- `ops/KNOWN-ISSUES.md` lists meters that mislead and the gaps that each change leaves open.
- `ops/README.md` lists the tools in `ops/bin` and how to test them.
- `AGENTS.md` has the development rules for this repository.
