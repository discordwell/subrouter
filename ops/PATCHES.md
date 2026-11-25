# Local patch against upstream (condensed)

This fork carries its own changes on top of `manaflow-ai/subrouter`. Each numbered section records one of them: what it does now, why it exists, and the settings that control it. This copy is condensed to about a tenth of the original.

The fork point is upstream `93bef02` (`v0.1.54`, 2026-08-03). Two upstream merges followed. §23 records the first (`1f9bf38`, 2026-09). The second brought the fork up to upstream `ec86e8cb` (2026-10-25) and has no section here.

Sections are in numeric order, but past §17 the numbers are not chronological. Three sections came from branches outside the main line:

- §63 was never merged, and its code is not in this repo. §64's code reached `main` only with the change that ported it on 2026-11-22, after its text.
- §68 was once numbered §62, where it collided with the other §62.
- §14 is left out on purpose.

## 1. A fresh machine keeps its own credentials

`sr setup` on a never-configured machine used to pick the hosted vault and log in to cmux.com. Uploading OAuth refresh tokens is a default nobody can withdraw once the tokens have left. A first run now resolves to `CredentialSourceLocal`. Explicit `--storage hosted`, `--storage team` and `sr login` still reach the vault, and a configured machine keeps its recorded source. Porting note: an upstream merge can drop this one branch without a conflict, so a test guards it. Check it first after any rebase.

- Code: `resolveSetupCredentialSource`

## 2. Setup tells the truth about what it will do

The plan and the run disagreed three ways. `--plan` now honors `--storage` (config loads before the early return, and `loadCloudConfigForLogin` never writes). The review screen's first line names the credential destination. It is listed but not togglable, since `--storage` owns that choice and a toggle would imply the screen can undo an upload. `runSetup` now applies its own client-config items (`writeCodexConfigForLocal()`, `writeClaudeProxyEnvForLocal()`) after the health check; before, they stayed `+` forever. Claude Code's "already done" probe checks the proxy root, not a `/v1` URL that client never stores.

- Code: `runSetup`, `loadCloudConfigForLogin`

## 3. Never point a client at an empty pool

A client pointed at a daemon with no credential fails every request, and Claude Code's proxy env also overrides its claude.ai login, so nothing falls back. Client config is blocked, with the reason shown, until that provider's pool is non-empty. Counts are per provider: a pooled count let one Codex login unblock Claude Code. §3.1: a running daemon with an armed peer (`remote.key` present (§17) and a peer configured) also counts as a Claude route. A remote-only box keeps refresh tokens on the peer (§16); the old workaround put a one-year token on the box, the token this mode exists to keep off it. Setup reads the daemon's own peer list from `/_subrouter/peers/usage-status` (5 s cap), because the peer is set in the daemon's environment, which setup never sees. A listed peer counts even if its last read failed. Setup asks the daemon only on a keyed machine whose local Claude pool is empty, so a keyless machine never dials it and keeps §3's wording, which names no remote (§17). Codex keeps the §3 gate, since a remote-only machine exists to serve Claude Code. A daemon down at plan time reports no peers, so rerun setup once it is up.

- Knobs: `SUBROUTER_REMOTE_POOL`, `SUBROUTER_REMOTE_POOLS_FILE` (read from the daemon's environment, not setup's)
- Code: `cmd/subrouter/setup_plan.go` (`clientConfigBlockedReason`), `daemonArmedRemotePeers`

## 4. Claude Code gets the token the daemon actually enforces

`writeClaudeProxyEnvForLocal` wrote the literal `"subrouter"` placeholder, but in local mode the daemon checks the bearer against `cloud.json`'s `localProxyToken` (`localProxyAuthorized`). Every Claude request 401'd, with no login left to fall back on. `sr codex` already sent the right token, so only Claude Code broke. The writer now calls `cloudClientProxyToken(...)`; the placeholder applies only when the target enforces nothing.

- Code: `writeClaudeProxyEnvForLocal`, `cloudClientProxyToken`

## 5. Nothing except first-time setup can rotate that token

`LoadConfig` returned `Config{}, err` for both a parse failure and a `Validate()` rejection. The login path built a fallback from that zero value, and `SaveConfig` minted a fresh `localProxyToken` whenever it was empty. So any future tightening of `Validate()` would rotate the token and 401 every running client. `LoadConfigRaw` now parses without validating, and the token and credential source carry forward from it. An unparseable file is a refusal naming the path, not a silent remint. `SaveConfig` preserves the existing token; only `EnsureLocalProxyToken`, called from one place, generates one. `serve` fails loud rather than run unauthenticated in local mode.

- Code: `LoadConfigRaw`, `SaveConfig`, `EnsureLocalProxyToken`
- Related: builds on §4.

## 6. Client configs are written the way the Codex writer already wrote them

The Claude config writers now match the Codex writer: no-op if unchanged, `.bak`, then `WritePrivateFileAtomic` (tmp + chmod + fsync + rename + dir fsync). The no-op check matters most: an identical rerun cannot race a running Claude Code or leave a half-written `settings.json`. Storage switches (`sr storage`, `server add --default`) now rewrite the Claude config before restarting the daemon (only Codex was re-pointed before) and verify it with an authorized `HEAD /`. A mismatch prints the repair command. `CLAUDE_CONFIG_DIR` is honored at the two setup write sites, mirroring `CODEX_HOME`. `DefaultStore().SharedStateDir` deliberately ignores it, because that would relocate the account pool.

- Knobs: `CLAUDE_CONFIG_DIR` (setup write sites only)
- Code: `WritePrivateFileAtomic`, `writeClaudeProxyEnv`, `prepareClaudeLoginFastPath`

## 7. Pool credentials survive a keychain round-trip

`security -w` hex-encodes a multi-line payload, and `writeCredential` stored indented JSON. Every pooled Claude account became unreadable after its first token refresh, so the pool emptied itself. Payloads are now compacted before the write. The keychain sits behind a read/write/delete seam so Linux CI covers the logic with a fake; before, CI ran none of the path where this bug lived. One `//go:build darwin` test drives the real binary.

- Code: `writeCredential`, `readCredential`

## 8. Agents cannot reach host agent configs

An agent editing this repo bricked every running Claude Code session three times, always through a host write. Tests now run in a `test` stage of the existing `Dockerfile` (before the final `FROM`, so the production image is unaffected) that sets all five isolation vars. The last two are not derived from `HOME`, so a shell exporting only `HOME` leaks. `make docker-test` and `deploy/docker/test.sh` mount source read-only, default to `--network none`, never mount the host home, and pass test args through so agents never go back to the host for one test. A `go/parser` lint fails when the set of `os.UserHomeDir()` call sites differs from a reviewed allowlist; a per-package `TestMain` guard silently stops guarding once someone forgets it.

- Knobs: `HOME`, `CLAUDE_CONFIG_DIR`, `CODEX_HOME`, `SUBROUTER_STATE_DIR`, `SUBROUTER_CLOUD_CONFIG` (all set by the test stage)
- Code: `Dockerfile` (`test` stage), `deploy/docker/test.sh`

## 9. The usage sweep was codex-shaped, and it ran the pool blind

Symptom: `503 no non-exhausted claude accounts available` while `sr claude list` showed full headroom. The auto-switch `FetchScores` and the startup fetch used the Codex store and endpoint for Claude accounts too, so every Claude fetch 401'd and scored `headroom=0`. The scheduler ran blind, piling sessions onto one account until it 429'd. `Server.scoreAccounts`, which dispatches per provider, is now exposed as `ScoreOAuthAccounts` and wired into both; the codex-shaped sweep is deleted, and `srAutoSwitchOnce` refuses to run without `FetchScores`. Each provider rotates its own active slot, so one provider's failure cannot stop the other's. Codex rotation is read-only since §23 decision 1 (the pick is reported; the daemon never writes `auth.json`). Anthropic rotates refresh tokens on use, so concurrent refreshes of one profile race. The credential flock is released during the network call (an import must never wait behind a 15-second OAuth round trip), so a separate per-profile refresh lock serializes refreshers; since the merge it is upstream's cross-process `.refresh.lock`, held across the call. Do not "fix" the flock and the refresh lock at each other's expense: they are two halves of one contract.

- Code: `Server.scoreAccounts` (`ScoreOAuthAccounts`), `srAutoSwitchOnce`
- Related: §18 and §23 replace the in-process limiter with `.refresh.lock`.

## 10. Claude routing follows weekly pace

Upstream picks the most headroom right now, which strands weekly quota on accounts about to roll over unused. Selection now ranks by spendable weekly budget ÷ time-to-roll; the 5h window only gates. Accounts within 24h of rolling drain first, earliest roll first, under a concurrency taper (originally `ceil(weeklyHeadroom / 0.25)`, capped at 4). The Fable reservation was `spendable = weeklyRemaining − 0.5 × fableRemaining × decay(timeToRoll)`, with `decay` 1 while a day or more is left and falling linearly to 0 at the roll; Anthropic caps Fable at 50% of the weekly budget. An account with no Fable window reserves nothing. Do not conflate the two session counts: `Sessions.CountByAccount` (parked, never pruned) spreads new sessions, and the taper uses `activeSessionCountsByAccount()` (in flight). `Score.WeeklyResetAt` is an absolute deadline, so a carried seed decays instead of looking urgent forever.

Amendments. §41: the taper step is 0.0625 on the budget above the owner's reserve (at 0.25 the cap of 4 never bound); an account whose 5h window is projected dry gets 1 session; the earliest roll's urgency is discounted by its live sessions. §55: for a local, unreserved, not weekly-only Claude account, the taper, the 5h clamp and the drain's 5% floor read the last usage reading without the 0.02-per-request live debit. §56: a holder of an unclaimed saved reset drains first once under 25 % of its measured week is left. §57: the Fable reservation is off by default (owner, 2026-10-15: "for now it's all about the opus"). §58: the live debit is refunded for an attempt the upstream never served (`SchedulerRef.RefundRouted`). §60: no owner reserve in the last day before the weekly roll. §62: a Claude account with no 5h reading has `ShortHeadroom` 1 and drains to its weekly wall; a Codex weekly-only account's floor still reads its week.

- Knobs: `SUBROUTER_FABLE_RESERVATION` (off by default since §57; `1` restores the formula)
- Code: `Sessions.CountByAccount`, `activeSessionCountsByAccount`, `Score.WeeklyResetAt`
- Related: amended by §41, §55, §57, §58, §60, §62; see §56.

## 11. Opus serves when the Fable pool is spent

Fable has its own weekly bucket (`seven_day_oauth_apps`), and a spent Fable quota often leaves 5h and 7d headroom that only other models can use. The fallback chain (pool, Bedrock, API key) had nowhere to go without the last two, so requests died with quota unspent. Opus is now the last stage: the literal `fable` run is spliced (`claude-fable-5[1m]` → `claude-opus-5[1m]`), which keeps 1M context and survives a generation bump, and selection reruns on the Opus pool. Capacity is not predicted, because a guessing gate would bring back stale-score false 503s. `oauthRetryAccount` decides, and its decline means no capacity. Only a clean 2xx replaces the original response; anything else surfaces the original error unchanged. The pick is debited (`NoteRouted`) because debit keys are account-wide.

- Code: `oauthRetryAccount`, `NoteRouted`
- Related: §64 replaces the splice: every Fable model falls to `claude-opus-5-5`, the bracketed suffix kept. Since §64 the Fable meter is the usage answer's `limits[]` Fable row, else `seven_day_oauth_apps`.

## 12. Remote admin fails closed, and macOS can bind a tailnet

Mostly reversed by §23 decision 8. With no `AdminToken` and no `AccountImportToken`, a caller of the private handler gets admin again, and `serve` no longer refuses a tokenless non-loopback bind. Pre-merge it refused, because a tailnet bind gave admin to every host the ACLs admit. Still holds: the public `:31416` listener mounts no admin route and refuses every admin request; a wide bind needs a proxy token; `TailnetAuth`, when on, admits only its allowlist or the admin token; `install-daemon` refuses a tokenless non-loopback daemon. The rest describes the pre-merge rule. Loopback admin is judged by the socket peer, so a spoofed `X-Forwarded-For` cannot claim it. macOS `install-daemon` reuses `serve`'s loopback predicate; it used to reject every non-localhost address. The admin token never goes in launchd `ProgramArguments`, which `ps` shows to every local user. The token file is validated at install via `readSecretFile` (an unreadable path caused an endless KeepAlive restart loop) and a relative path is made absolute.

- Knobs: `--admin-token-file` (plist 0644), `--admin-token` (inline, plist 0600), `SUBROUTER_ADMIN_TOKEN[_FILE]`, `--allow-unauthenticated-admin` (pre-merge opt-in)
- Code: `authorizeAdmin`, `readSecretFile`
- Related: §23 decision 8 mostly reverses this. Topology: `docs/tailscale.md`.

## 13. `usage-status` publishes normalized capacity, not just raw windows

Raw `windows` names differ by provider (Claude `5h`/`7d`/`oauth-apps-weekly`, plus `opus-weekly`/`sonnet-weekly` only when Anthropic reports them (§24); Codex `primary`, `<model>/primary`, `reached`), and a wrong guess fails silently: reading Codex rows with a `7d` lookup showed five untouched Pro accounts as spent. Each row now carries the scheduler's own `normalized` capacity (`headroom`, `short_headroom`, `seconds_to_short_reset`, `weekly_headroom`, `seconds_to_weekly_reset`, `fresh`). It comes from `Scheduler.LookupScore`, not `ScoreFor`, which would claim full health for an account nobody measured. Absent means unknown. `fresh` separates a current fetch from a carried seed. Consumers prefer `normalized` and fall back to `windows`, as `bin/srq` does.

- Code: `Scheduler.LookupScore`, `handleUsageStatus`, `bin/srq`

## 15. `sr claude` spills to the server once local accounts run low

`sr claude` never reached a configured server: every non-hosted launch used `localBaseURL()`, and the one remote branch required `server.Name == "cmux"`. With local storage it now prefers this machine's accounts and spills to the server only when no local account clears `MinNewSessionHeadroom`. Team storage stays pinned to the local daemon, because leased credentials must be sent from this machine. Capacity comes from `normalized` (§13), never raw windows: an early `scoreFromWindows` version scored an account with 80% of its week left at `0.00`. An unreadable local pool or a server failing its health probe keeps the launch local. `cloudClientProxyToken` never sends this machine's proxy token to a non-loopback target.

- Knobs: `SUBROUTER_DISABLE_FALLBACK` (pins the server, skipping the local-first preference)
- Code: `remoteServerUsageStatus`, `cloudClientProxyToken`

## 16. The daemon falls through to a peer pool

With `SUBROUTER_REMOTE_POOL` set, the daemon forwards to a peer pool, so a client pointed at `127.0.0.1` can spill mid-session. Local stays preferred: a forward happens only at a give-up point (Hook A at the handler's 503, Hook B after §11's Opus stage). The forward carries `X-Subrouter-Session` so the peer keeps its own sticky assignment, and this side remembers the delegation until the peer declines a model turn. It sends the client-facing path, not `pathForUpstream`'s rewrite, which turns Codex `/backend-api/wham/tasks` into `/wham/tasks`. `X-Subrouter-Agent` is load-bearing: without it the peer defaults to Codex. A 2 s dial bounds unreachability, and the breaker opens on the second consecutive failure because an idle tailnet link drops its first packet; a peer 429/503 clears it but still unpins the session. Loops stop on `X-Pool-Hop` (an `X-Subrouter-*` name would be stripped). Lease and broker modes never forward. §54/§67: an opening ends no hold. A peer is found down only when the opening is attributable to it and it has answered nothing for the down grace; a held chat waits for its peer until then, and is then placed as a refugee.

- Knobs: `SUBROUTER_REMOTE_POOL` (unset: off), `SUBROUTER_PEER_DOWN_GRACE` (15 minutes); `SUBROUTER_PEER_HOLD_WAIT` is no longer read (§67)
- Code: `internal/proxy/remote_pool.go`, `internal/proxy/proxy.go`, `cmd/subrouter/main.go`
- Related: amended by §54 and §67.

## 17. The remote exists only on a machine holding the key

Without `<store>/remote.key` the build is local-only: nothing that sends a credential off the machine is reachable or named in help. Before, `sr --help` listed the remote commands on every machine. One predicate gates six chokepoints: `selectedRemoteServer` (the funnel for all twelve remote call sites), `configuredRemoteServers`, `srRunner.run` dispatch, help (`srHelp`, `usageText`), `remotePoolFromEnv`, and `cloudModeConfig`. That last one forces `Local` and reads via `LoadConfigRaw`, so a leftover `hosted` config neither fails every command nor remints the token. `resolveSetupCredentialSource` refuses `--storage hosted|team|legacy`. The verdict is computed once per process (`sync.Once`), and help is filtered by the dispatch's own `remoteCommands` list, not forked. This is not access control: anyone can read the source and create the file, and the box's own tokens are the real boundary. No migration, deliberately: minting a key when `servers.json` held a server would make that file a second key. Give each machine its `remote.key` before installing the gated binary, or they lose the remote at the next `sr` call and the peer pool at the next daemon restart.

- Code: `cmd/subrouter/remote_gate.go`, `selectedRemoteServer`, `cloudModeConfig`

## 18. A caller's cancellation never poisons shared state or aborts a rotation

Incident 2026-09-08: the box reported every Claude profile as `context canceled` / `AUTH-INVALID`, and the laptop logged `peer could not be measured` and routed local-only. The accounts were fine. The usage sweep (8 to 9 s, Codex then Claude) ran on the first caller's request context. The laptop's 3 s probe hung up mid-sweep, the Claude rows failed, and that result was cached 30 s as `auth_valid:false`.

1. The sweep runs on its own clock (`context.WithoutCancel`, `usageSweepTimeout` 60 s), one at a time. A caller arriving mid-sweep gets the last snapshot; a caller with none waits on the running sweep, and if its own context ends first it gets 503, never an empty list (readers take a non-200 as unmeasured). Rows cut by the deadline publish `sweep_cut:true` (plus `auth_checked:false` if the credential was never checked), never `auth_valid:false`. `InvalidateUsageStatusCache` bumps a generation so a sweep started before an import cannot publish over it.
2. Peer readers treat an unmeasured OAuth row as unknown, not empty, and carry the last measured reading for up to `remotePoolCarryTTL` (15 min), aged from when its body was measured and logged as `peer_carried=true`. A real verdict (`invalid_grant`, exhausted, absent) is never carried.
3. The token exchange ignores the caller's cancel (`context.WithoutCancel`, 30 s; the Claude persist gets its own 10 s). Anthropic and OpenAI rotate refresh tokens on use, so an abort mid-response stranded the profile on a dead token. Waiting for the per-profile slot still honours the caller. `http.Client.Timeout` (15 s default) still bounds the exchange, so a provider slower than that loses the rotation as before.

Porting note: the three `context.WithoutCancel` sites are one line each and each merges cleanly over an upstream change to its function. Their guard tests fail on an unpatched tree, so run `make docker-test` after any rebase touching these files. A pre-§18 peer still poisons its own cache.

- Code: `internal/proxy/proxy.go`, `internal/proxy/remote_pool.go`, `internal/agents/claude/store.go` (`refreshProfileCredential`)
- Related: §23's merge replaces `keyedRefreshLimiter` with upstream's `.refresh.lock`.

## 19. One grammar for accounts, and the verbs it revealed were dead

Superseded in part by §22: the house verb-first grammar is retired, and its spellings print a one-line pointer. These fixes survive:

- `directSRCommands` lists every dispatched top-level verb (`sr export` never dispatched); tests drive `runForProgram`.
- `pushClaudeProfileTo(ctx, name, server, mode)` is the one upload and takes the caller's resolved server (`--server box2` used to upload to box1).
- Email profile names are allowed (`ValidateProfileNameAllowEmail`), and `profileDirForName` adds a digest suffix so names cannot share a directory.
- New profiles take the first unowned directory (`freeProfileDir`); names differing only in case are refused.
- §17 leaks closed: help lines naming remote vocabulary are dropped, and `resolveServer` answers `unknown flag: --server` on a keyless machine.
- `sr status` shows local, then each server. Since §23 decision 1, `sr status`, bare `sr` and `sr pick` never import `~/.codex` into an empty store (`sr import` stays the explicit way), and bare `sr` never switches away from an exhausted Codex login itself; it prints a recommendation.
- `sr claude rename` re-keys locally and refuses if a server already holds the new name; an unreachable server is a warning that does not block (servers have no rename route).
- `sr login` (the cmux.com sign-in) refuses positional arguments.
- Related: superseded in part by §22.

## 20. A warm session moves only when it is evicted

Owner rule, 2026-09-24: a warm session (a turn in flight, or its last turn within 70m, the prompt-cache lifetime) moves only on a forced eviction. Forced means a 5h or weekly limit (any bucket, incl. `7d_oi`), the owner's holdout, or a side that cannot serve (dead credential, peer declines or is down). Local and remote are equally sticky. The holdout is hard: a session is evicted there and none may start inside it. Ranking is unchanged (`LocalPreferenceRatio` 2x for local). Fixes, from measurements on the laptop, 2026-09-10..24:

- Warm sessions spilled to the peer on 5h burn projections (437 warm moves). Warm Claude sessions now get no score on the sticky path.
- One Esc marked the pool exhausted (2,725 marks for 10m, each cascading over the pool). A refresh with no verdict (`refreshFailureIsTransient`) now marks nothing; the turn is served once by the peer or fails as a 502 with `X-Subrouter-Transient`.
- Holdout leaks closed. `Scheduler.Evicted` / `InHoldout` are the one verdict, judged on the measured week (`Score.WeeklyRemaining`), failing open on a reading from before the roll.
- Peer holds survive cancels and blips, last 70m from turn end, and persist to `peer-holds.json`. A decline clears the hold; a breaker opening cleared all holds (since §54 only an opening attributable to the peer; since §67 none: a chat held on a peer found down is placed as a refugee on its next turn).
- After a restart, an unseen session counts as warm for one idle bound.
- Failover marks the account that actually answered.

Gap: `sr auto-switch` and tenant/broker leases rank without reserves; a bare (headerless) 429 still fails the turn over (the rebind is settled for Claude by §31 and for Codex by 35.1; a Codex burst ranks its account last without evicting, 35.3). §67: a peer is down only after an opening attributable to it (§54) plus 15 minutes of silence, and every session it held is then placed as a refugee on its next turn. §60: no holdout in the last day before the weekly roll.

- Code: `refreshSelectedAccount`, `Scheduler.Evicted`, `serveRemotePoolOneTurn`
- Related: amended by §54 and §67; §21 extends it to Codex; see §60.

## 21. Warm rule covers Codex; side requests never move sessions; routed Claude asks for 1h cache

Owner rule, 2026-09-24, for both providers: a warm session (in flight, last turn within `warmSessionIdleTTL` = 70m, or unseen since a restart under 70m ago) keeps its account and side until §20 forces it off. Fixes, measured 2026-09-17..24:

- Codex was never warm-sticky: one Codex Desktop user moved 3,700 times in 39,404 model calls. The warm check now runs first for every provider; cold Codex keeps the `UsableForNewSession` gate. Peer holds last `warmSessionIdleTTL` for every agent (Codex held 2h before). The shared `codex:fallback` bucket is never warm.
- Side requests re-pinned conversations: analytics POSTs made 2,029 of 4,893 moves on the box. Side requests (`sideChannelRequest`: Codex analytics, `/backend-api/ps/mcp`, Claude `count_tokens`; `fallback:` keys and `/backend-api/files*` excluded) now read placement and write none, and never set or clear a peer hold (`remotePoolHoldReadOnly`). A model turn is judged by provider and path, never by agent name.
- Websocket turns now set a peer hold after a successful upgrade.
- Forwards `claim` the hold before sending, so concurrent turns cannot split between pools.
- One blip no longer counts twice and clears every hold, and the breaker cooldown starts when the failure is seen.
- Routed Claude Code asked for the 5m cache, because Claude Code treats `ANTHROPIC_AUTH_TOKEN` as an API-key client (0 one-hour tokens through Subrouter); a probe with `CLAUDE_CODE_PROMPT_CACHE_TTL=1h` wrote `ephemeral_1h=74,253`. Writers and launchers now set `1h` unless the user already chose. A choice is `CLAUDE_CODE_PROMPT_CACHE_TTL` or `FORCE_PROMPT_CACHING_5M` in the settings file's env (any value) or a top-level `promptCacheTtl`, or, only with `CLAUDECODE` unset, an exported `FORCE_PROMPT_CACHING_5M` or an exported TTL other than `1h`. Subagents keep 5m on purpose.

Both daemons must be redeployed for the side-request fix to hold on the peer.

- Knobs: `CLAUDE_CODE_PROMPT_CACHE_TTL` (written as `1h`), `FORCE_PROMPT_CACHING_5M` (a user's own setting is respected), `CLAUDECODE` (when set, exported cache vars are not read as a choice)
- Code: `sideChannelRequest`, `poolModelTurnRequest`, `remotePoolBreaker.failure`
- Related: extends §20; §54 and §67 narrow when a breaker opening clears holds.

## 22. Upstream's CLI grammar, adopted, and a refresh token is never in two places

Owner decision, 2026-09-24, final: the merge adopts upstream's provider-first grammar (`sr <provider> <verb>`; `sr add <provider>` also works) instead of §19's. The pool comes from ambient state (the selected server, `SUBROUTER_CODEX_SERVER`), never from a positional word.

- `sr claude add <name>`: one-year setup token (upstream #305), stored refresh-less, also copied to a selected server.
- `sr claude addr [<name>]` (= `sr claude login`, `sr add claude <name> --oauth`): browser OAuth with a refresh token, this machine only. Also re-auths a dead refresh token in place.
- `sr claude move <name> [<server>]`: upload, confirm `auth_valid`, then remove locally; every failure keeps the local profile.
- `sr claude push <name> [--copy]`: copies a setup token, moves a refresh token (`--copy` refused).
- `sr claude remove <name> [--server <server>]`: local, or DELETE on that server only.
- `sr codex add [--device-auth] [--email <e>]`: upstream's isolated pool login; `--email` is our identity pin.
- `sr codex login|logout`: the real Codex CLI against the human's own `~/.codex`.

Invariant on top of upstream: a refresh-token credential is never in two places, because every refresh rotates the token and the second holder dies permanently. `pushClaudeProfileTo` is the one upload; a copy refuses a refresh token, and an OAuth add never uploads. Hosted-cmux onboarding (`hostedClaudeAdd`) keeps upstream's upload, since it runs in a temporary store and leaves no local profile. Retired spellings print a one-line pointer and do nothing else. `claudeManagementSubcommands` must list `addr`, `move` and `rename`, or `sr claude move x` launches a profile named "move". Behaviour change: `sr claude add <name>` used to be the OAuth login.

- Knobs: `SUBROUTER_CODEX_SERVER` (picks the Codex pool)
- Code: `sr_claude_upload.go` (`pushClaudeProfileTo`), `sr_retired.go`
- Related: supersedes §19's grammar; keeps §17's gates.

## 23. The 2026-09 upstream merge (manaflow-ai/subrouter `1f9bf38`)

Upstream `1f9bf38` was merged into the fork (fork point `93bef02`), conflicts resolved hunk by hunk, with both sides' additions unioned. Owner decisions, 2026-09-24:

1. Codex uses upstream's isolated-login model. Our 10-minute `~/.codex/auth.json` writer is deleted, the Codex pick is only reported, and `~/.codex` is the human's own login. An explicit `sr g` / `sr switch` still exports a pool account there, as upstream does, and that takes the record out of serving. Each machine migrates once; a pre-merge binary is a rollback past the credential-origin boundary.
2. Named access tokens: a later PR, built in §43.
3. Tailscale stays for now; `TailnetAuth` is off by default and refuses an empty allowlist.
4. Claude setup tokens are upstream's (§22).
5. Grammar is upstream's (§22).
6. Upstream's full `handleHealth` on both listeners.
7. Upstream's `workerIdleTimeout` of 620 s over our 90 s.
8. Upstream's tokenless admin fallback (reverses most of §12); the public `:31416` listener stays API-only.
9. Tailscale is allowed but never the default.

Kept ours: routing (§10, §20, §21), with no flat floor and our transient check before upstream's terminal-only failover in `refreshSelectedAccount`; `Score.WeeklyHeadroom` with our meaning (spendable weekly after the Fable reservation, 0 = unmeasured; upstream's raw weekly is `WeeklyRemaining`); fresh-only mark clearing; the forced-path holdout refusal; `workspace_*` holds a fresh usage read cannot end early; our Codex WS auto-redeem; no `X-Subrouter-Account(-ID)` on a forwarded peer hop; the public/private route split, with every new upstream admin route `public:false`; the §15/§16 launch spill; the §17 gate on every new remote path; Claude active-profile rotation; §7/§18/§19 re-applied on upstream's Claude store.
Taken: the crash-safe Claude store removal, setup tokens, the cross-process `.refresh.lock`, the `SchedulerRef` overlay fixes, session store `CompareAndPut`, strict forced vs preferred request selectors, bounded parallel usage sweeps, Codex limit holds to the named reset, the Codex WS 1012 reroute, `ReadyCheck` on `/_subrouter/ready`, `go 1.26.0`.
Skipped: spread placement and `MinNewSessionHeadroom`; paid Claude extra usage (`2269757`: reporting only, nothing spends money); autoupdate installers (merged for tests, run by no default path); CLA and CI workflows, `cloudflare/`, `planning/`.

- Related: reverses most of §12; replaces §9's limiter; §22 is its grammar pass.

## 24. A model with no limit of its own shows no meter and routes on the account's budget

Owner rule, 2026-09-27: when a model has no separate limit, do not show it. On Claude only Fable has its own limit (`seven_day_oauth_apps`); Opus and Sonnet spend the general `5h`/`7d` budget. Fake 0%-used Opus and Sonnet windows showed a meter that does not exist. Setup-token accounts cannot read the usage endpoint, got no such pool, and scored 0: on 2026-09-27 three of them, one with its whole week unspent, took 0 of 120 Opus requests. Now a null bucket publishes nothing, and a Claude account with no pool for a model routes on its account-wide score (`absentPoolInheritsAccountScore`). Codex pools stay strict. An Opus 429 marks only that account's `claudeopus` pool; on expiry the mark is judged on the account-wide score and re-armed if a fresh score confirms the budget is spent, and fresh headroom never ends a live mark early. A bucket Anthropic reports survives a live mark. Claude-family membership now comes from the Fable window.

- Code: `claudeUsageWindows`, `ForModel`, `applyExhaustionMarks`

## 25. A session moved off a spent account sticks to the account that served it

Owner rule, 2026-09-28: a session moved off a spent, evicted or broken account sticks to the replacement once it answers successfully. There is no per-request re-pick, because the prompt cache is per account and every re-pick re-bills the prefix. Since §23 a move commits only after the stream's terminal event (`message_stop`, `response.completed`, `[DONE]`). The proxy forwarded the client's `Accept-Encoding`, so replies arrived compressed, the parser never saw the terminal event, and no move committed: 0 commits in 74 h over 27,060 reroutes on the box, about 8 accounts per rerouted session. `stripClientAcceptEncoding` (ported from upstream `55ead07`, #439) drops the header on every outbound request so Go decodes, and it must run on the binary that owns the pin. A missed commit logs `provisional session move not committed` with its reason. A non-streaming 2xx commits on a clean EOF without parsing. Fail-closed is unchanged: stream EOF is not success, an `error` event or a non-2xx leaves the pin, and a CAS against a moved pin loses. The box had no drain (`systemctl restart` cut every stream); §38 fixes that.

- Code: `stripClientAcceptEncoding`, `newSessionCommitSSEReadCloser`
- Related: §38 adds the drain.

## 26. A usage-status read no longer postpones or cancels the refresh that clears the live debits

Each routed request debits its account by `LiveDebitPerRequest` (0.02 of headroom) so concurrent traffic spreads. Only the scheduled refresh (`refreshUsageScoresIfStale`, started by a pick once `updatedAt` is older than 30 s) clears the debits. Every `/_subrouter/usage-status` and `/_subrouter/pool.json` read republished the scheduler, stamped `updatedAt = now` and cancelled any refresh in flight. So any reader faster than 30 s starved the refresh (the laptop reads the box on every eligible request through a 10 s cache, and the laptop's pool board polls the laptop every 5 s while visible).

Evidence (box, 2026-09-28, before the fix): debits cleared every 2.5 to 5 min, and in 7 of 16 samples the box had no Claude account it would start a session on while measurement said two had room. The ~23k/day `routing optimistically` warnings were mostly §25's reroute churn, not debits. Refuted: the box did not look emptier to the laptop. usage-status `normalized` and pool.json headroom/short-left/pace are measured values with no debits; only the box's own picks and pool.json `state`/rank/reason carried them.

Reads now publish through `SchedulerRef.PublishWithoutRefreshForAccountGenerationAtScoreRevision`: same guard, clock untouched, no cancel. Reads still never clear debits (that would erase real unmeasured traffic each time a peer looked) and make no upstream usage calls. The refresh reads the shared 2-minute usage-window cache, so an account is fetched upstream at most once per 2 minutes. `Set` keeps the old behavior only for the full re-score after an account reload. Exhaustion, credential and unavailable marks still stamp the clock (request-time events; §27 covers the capacity marks). Known limitation: `FetchUsageWindowsCached` has no single-flight, so a read and the refresh can both fetch one expired entry (7 of 90 simulated 10-minute runs). Both binaries need the fix; no config change. The box has no drain, and `systemctl restart` cuts every in-flight stream (§25). Debit counts are not exported; after deploy, pool.json `state` (debited) and its headroom/short-left (measured) should disagree for at most about 30 s at a time.

- Knobs: `--usage-score-ttl` (30 s).
- Code: `SchedulerRef.PublishWithoutRefreshForAccountGenerationAtScoreRevision`, `updateSchedulerFromUsageStatusesAtScoreRevision`.

## 27. Codex "model at capacity": upstream's capacity marks, as written, inside the warm rule

Owner decision, 2026-09-28: "You can take their model at capacity rule as written", but keep the fork's 70-minute warm rule (§20, §21) over upstream's sticky-retention floor "because it captures cache". So upstream's capacity marks and eviction are ported (`862a5a5`, #382), not its retention floor (`MinStickyRetentionHeadroom`) or +10pp move gate. Owner decision, 2026-09-28 (second round): port #373 detection and the #389 stay ladder "as written", keep the 32k size cap and the websocket reconnect that "remembers the model", and noted there is no Azure here. Owner decision, 2026-09-28: "ship it on" (upstream's failover is opt-in).

A capacity failure now writes a rank-only mark (`MarkCapacityUntil`, was `MarkExhaustedUntil`) per (account, model, service tier). It is not quota, and usage refreshes never clear it. Before, one failure read as `Evicted` and moved every warm session on the account. Within a selection tier, fewer consecutive capacity failures ranks first; a marked account is demoted, never excluded. After 2 consecutive failures in one (model, tier) pool while the mark lives (`CapacityEvicting`), the account's sticky sessions are released, the warm rule's one new exit. A released session moves only to an account that is not shedding and passes `UsableForStickySession`, else it stays. A failure is first replayed once on the same account (250 to 750 ms gap), then the failover switches (100 to 400 ms gaps, ~10 s budget). A mark lasts the upstream hint clamped to 30 s to 5 m, else 2 m, jittered ±20%, and the first success on the model clears it on every tier. The overall effect is fewer cache-losing moves than before, not more.

A conversation above the size cap is treated as failover-off: never marked, switched or evicted. Its size is the decoded body length / 4 over HTTP; on a websocket, the larger of the longest `response.create` / 4 and a finished turn's reported input tokens. A size-kept websocket turn's failure reaches the client (upstream has no websocket ladder). #373 counts as failure 408/5xx, a 2xx SSE stream failing before its first visible output, and a capacity body on any status. The stream peek holds a 2xx stream until the first delta, finished item or end of response (128 KiB, 3 s). A websocket failure after visible output passes through instead of closing, because a reconnect would replay `response.create` and duplicate the partial answer (a server-class one still marks the account). #389 stay ladder: a request the failover does not move retries on the same account (gaps ~0.5, 1, 2, 4, 8 s, then ~9 s) for up to 4 min, or ~10 s if an egress or Azure fallback is configured, then returns the failure unchanged. It writes no marks and draws no attempt budget; only time and cancellation end it. `/_subrouter/health` shows `overload_retry_held: {codex: n}`. #371: a replacement's quota or auth failure is charged to the replacement. A move commits at the terminal success event or a clean EOF, not at the 2xx headers.

Fix-ups 389-1 and 389-2 (2026-09-28): chatgpt.com streams Codex turns with no Content-Type, so a pre-output `server_is_overloaded` in a 200 stream counted as success and cleared the marks. 389-1 labelled the stream at the capacity layer, so the usage-limit layer below still read an opaque body: its A -> B move committed at EOF and pinned the session to B when B's stream failed after output. In 389-2, upstream #450's content sniff wraps the base transport under every layer, Codex only (other providers are the owner's call). A remote-pool answer (`SUBROUTER_REMOTE_POOL`) passes through untouched; before, a peer's capacity failure marked the local account and returned a local `429 usage_limit_reached`.

Differences from upstream: the keep check's guard is `!Evicted`, not `!Exhausted`, because §20 forbids keeping a session inside its owner's holdout. "Can hold the session" is `UsableForStickySession`, stricter than upstream's 5% floor. The capacity clause outranks machine binding and endgame drain. A websocket reconnect reads the marks of its session's last rerouted model (10-minute window), because Codex's upgrade names no model and the session never reached the two-failure exit. After visible output, the websocket quota filter returns the quota error instead of closing 1013. Not ported (flagged, not decided): persist mode, the shedding tracker, `X-Subrouter-Retry` (would let a client stretch a size-kept hold to 60 min), the Claude 8-minute overload ladder and its opt-in reroute, the Fable header strip, upstream's spread-band change (the fork's `Pick` is deterministic, §10). Claude marks nothing.

- Knobs: `SUBROUTER_CODEX_OVERLOAD_FAILOVER` (on, health reports `codex_overload_failover: true`; `0`/`false`/`no`/`off` marks nothing and sends every HTTP capacity failure to the stay ladder; any other value fails the start), `SUBROUTER_CODEX_OVERLOAD_FAILOVER_MAX_INPUT` (32000 estimated input tokens; `0` = no limit), `SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT` (4 min; `0` = until the client disconnects), `SUBROUTER_CODEX_CAPACITY_RETRY_INTERVAL` (at least 500 ms).
- Code: `selectacct/capacity.go` (`MarkCapacityUntil`, `CapacityEvicting`, `CapacityStickyEvictFailures = 2`), `selectacct/explain.go` (`compare`), `codex_overload.go` (`RoundTrip`).
- Related: works inside the §20/§21 warm rule.

## 28. A credential read that got no answer is not a dead login

Owner rule (P8): only a forced switch moves a session (window spent, evicted, credential genuinely dead, account removed). Transient trouble (deadlines, network blips, a refresh with no verdict) keeps the account.

On the laptop, a keychain read cut by a 5 s clock (the pool refresh's single budget or `security`'s own) became `Claude profile "X" has no access token`, the dead-login text. `authLikeUsageError` matched it, the zeroed account was carried as the seed and read as evicted, and §25 committed the warm session's move for good. Evidence (laptop, 2026-09-28, 01:22 to 03:36 CDT): 14 of 15 committed moves followed such a line 0 to 38 s earlier, while all 15 accounts read `auth_valid=true` at 03:17. A false reading moved a session that sent another turn within 90 s in 13 of 14 cases; a deadline line did so in 0 of 17. Daily false readings were 2 to 33 through 09-23 and 63 to 151 from 09-24.

`agentclaude.ErrCredentialUnchecked` marks a read that got no answer: any vault failure, a clock-cut `security` read, and any credentials-file error other than `ENOENT` (absent) or a refusal. The keychain is not consulted when the file errors, because rotations go to the file and the keychain copy can hold a spent refresh token. An unchecked read moves nothing: the turn goes to the peer once or fails 502 with `X-Subrouter-Transient`, the seed is kept, and the status row is `sweep_cut` with `auth_checked:false`. A rotation is still written when the re-read gets no answer; dropping it left a spent token that became a real invalid_grant. Reloads and the startup listing keep an unchecked profile, because a restart under load had dropped it and moved its pins for good. Real answers stay verdicts: `security` exit 44 or an empty item, no file on a vault-less host, a token-less credential, upstream 400 invalid_grant or 401. Outside the daemon, `sr` shows the vault error and profile removal stops at its pre-removal backup. The 5 s pool budget is unchanged; a cut read is now labelled a cut and keeps its seed.

The bound: unchecked forever let a store that never answers keep its pins and draw new sessions (20 of 20 warm turns and 5 of 5 new sessions got 502). Real cases: a root-owned 0600 file the box's `User=subrouter` daemon reads as `EACCES`, and a locked laptop keychain answering exit 36. `agentclaude.ErrCredentialUnavailable` is the verdict: the account is zeroed and its sessions move once, with no one-hour terminal hold, until a read answers. It comes from a refusal (`EACCES`, `EPERM`, `EISDIR`, `ENOTDIR`, `ELOOP`), from 15 minutes of unchecked reads (at least 3, no answer between, no two counted failures more than `credentialSilenceMaxGap` = 12 m apart), or from own-clock `security` hangs alone for `credentialHangVerdictAfter` = 1 h, since a loaded Mac also hangs (one refusal restores the 15-minute bound). Reads cut by the caller's deadline never count, so the 5 s pool budget never reaches a verdict. The verdict text starts "no access token can be read" so text-based classifiers zero the account and peers see `auth_valid:false`. The hang's text carries no context error, because `sweepCut` and the peers would publish it as a cut. A rotation is also written over a refused re-read (the rename replaces the root-owned file with one the daemon owns).

Round 3: the 12 m gap rule exists because one failure, 20 quiet minutes and one turn had been a verdict. The auto-switch sweep reads every profile at least every 10 m, so a longer gap means every read between was cut or none was taken. New sessions skip an account in a live streak (`agentclaude.CredentialStoreNotAnswering`, via `placeableAccounts`), because its kept score drew new sessions that each got 502. This is eligibility only (nothing marked, zeroed or published; a warm session keeps its account), waived when every candidate is silent or no answering one passes the caller's hard gate, and a hang-only streak does not steer. A lock that cannot be taken is classified like the file (ENFILE on a lock file, 2026-09-06, dropped the account on reload and 503'd every turn).

Crosspool's D4b must treat an account skipped as silent as `refresh_no_verdict`, not `Evicted` or `AccountGone`; only `ErrCredentialUnavailable` is forced. Land this before crosspool runs anywhere, or an unanswered read becomes a forced switch for peers (`409 auth_invalid` on a targeted turn; `cachedHoldVerdict` = forced/auth_invalid on reading this daemon's row). `proxy.go` conflicts with crosspool at the two pick sites in `accountForSessionProviderWithOptions`; resolve both as `s.placeForSession(scheduler, placeableAccounts(availableAccounts, refused), options.crossPool, state)`, where `refused` is this patch's `selectionRefusesAccount` closure. The proxy tests' "unchecked" stand-in is a symlink to a Unix socket, because a directory now reads as a refusal. Store tests pin `PATH` empty and assert `security` does not resolve; with a seam swap alone, a test with the swap edited out stayed green while it ran two real `find-generic-password` execs. Left as documented, not changed: a keychain write killed by its 5 s clock still loses a rotated token, and the stale credentials file beside a silent keychain (KNOWN-ISSUES). The laptop is the fix (the only keychain reader). The box needs it only for the file door (EMFILE/EIO), which it has not shown. Sessions moved before the deploy stay where they are.

- Code: `store.go` (`securityKeychain.read`, `keychainCredential`), `credentialFileRefused`, `observeCredentialRead`, `credentialLockFailure`, `proxy.go` (`accountForSessionProviderWithOptions`, `placeableAccounts`).
- Related: §25's commit made the false moves permanent. §34.6 amends this section's proxy tests (on a Mac, a missing file swaps in `agentclaude.KeychainWithNoItemsForTest`).

## 29. The first upstream harvest: eighteen upstream fixes our tree still needed

Upstream `manaflow-ai/subrouter` (read at `c9303d3`) is a source only. Nothing goes back upstream, and no upstream release binary is installed over the fork. Each fix was assessed against our code, checked by a skeptic, ported on its own branch from the pre-harvest tree, and merged with the others. None of it changes an owner decision: no flat new-session floor, the 70-minute warm rule, routing spec P1-P10, peer fallthrough, the public TLS listener, holdout/reserve. It is numbered §29 so it collides with no number that work then in flight had claimed. The deploy notes for §29.1 to §29.4 and §29.6 to §29.8 call for no config change. One line per fix:

- §29.1 (#371, singleFlight half). Identical concurrent Codex plugin-catalog GETs share one fetch through `singleFlight`. A panicking fetch left its key in the map with its `WaitGroup` never done, so every later identical GET hung forever and only a restart cleared it. The panic is normal: the flight's `context.WithoutCancel` clone keeps `http.ServerContextKey`, so `httputil.ReverseProxy` panics with `http.ErrAbortHandler` when the upstream body breaks mid-copy (connection reset, gzip error, laptop sleep). Now `do` releases the key in a `defer`, waiters start from `flightFailedResult()` (502 `upstream fetch failed`, because the zero result was an empty 200), and the flight closure recovers any panic, logs `coalesced upstream fetch aborted`, and answers 502. Nothing is cached across flights. Coalesced GETs never count as in flight, so the warm rule never sees them. The overload-charge half is ported in §27.
- §29.2 (#429). A caller's own pin (`X-Subrouter-Account-ID` from `sr claude proxy --account`, `SUBROUTER_CODEX_ACCOUNT_ID`, `sr kimi --account`) disarms every failover stage, so a pin on a dead credential (`invalid_grant`, `account_on_hold`) got a retryable 503 forever. A Docker replay on the pre-harvest tree got 503 ten times with no `X-Should-Retry` and hit the token endpoint 10 times. Now it gets 403 with `X-Should-Retry: false` in the provider's error shape (`permission_error` for Claude, `invalid_request_error` / `account_unusable` otherwise), naming the account, the reason and the next step. Adapted three ways: `refreshFailureIsTransient` is asked first, so a refresh with no verdict (token endpoint 429/5xx, network failure, client cancel, `ErrCredentialUnchecked`) stays 503; the gate is `forcedAccountID`, so session leases (they recover by re-leasing) and peer targeted forwards (the decider reads a target's 503 as a decline) never get the 403; hints name only real commands (`sr add claude`, `sr claude proxy --account`, `sr add codex`, `SUBROUTER_CODEX_ACCOUNT_ID`), never upstream's `sr codex proxy --account`. The pinned account is still not marked. The unpinned on-hold gap noted here (a 503 with no mark and no failover) was fixed in §35.2: it now fails over locally, marked. Land after §31.
- §29.3a (#399). `responseUsageLimited` failed over on every Codex 429 without opening the body, so `usage_limit_reached` never marked the account and it stayed routable after a successful failover. That cost every new HTTP Codex session on it a 429 and a failover until reset: days for a weekly window, indefinitely for a workspace spend cap or depleted credits, since wham/usage reports the member's windows as healthy. Now the body is read with the bounded reader and the account is held for the refusal's own time (48 h, or 1 h for a workspace refusal). A fresh healthy reading ends the member mark but not the workspace hold. An unreadable body still fails over but does not mark (upstream hands the 429 back). Burst 429s (no usage-limit code or message) still failed over unmarked here, the §20 gap. §35.3 settled it: a burst keeps the session's account and leaves a rate-limit mark that only ranks. §35.1 also exhausts `usage_not_included`, `insufficient_quota` and `quota_exceeded` 429s like `usage_limit_reached`. Websocket turns already marked; the exposure was the box pool and the 426-to-HTTP fallback. The laptop log since 08-03 showed 0 Codex usage-limit failovers against 2,530 Claude ones.
- §29.3b (#398, adapted). `retainExhaustedExpiriesForScoreKeysLocked` cleared a request-time exhaustion mark on any Fresh score with headroom, never asking whether it was measured before the 429. With the 2-minute usage-window cache and §26 republishing the 30 s sweep cache as Fresh (peer probe every 10 s, pool board every 5 s), a 5 h mark was gone within seconds. Now marks are numbered (upstream), and fork-only `Score.MeasuredAt` records when the usage fetch behind a score started (a cache hit uses the entry's time; `AccountUsageStatus.UsageMeasuredAt` is `json:"-"`). A score clears only marks set strictly before it, equal instants keep the mark, a model pool's score inherits its account's time, and a score without `MeasuredAt` falls back to the numbering. The tokenless `FinishRefresh` orders after every mark already set, and `ScoreRevision()` takes the write lock. A reading taken after the 429 still ends the mark early, so the cost is bounded by when that reading happens: at most one window-cache lifetime (2 min) plus the next refresh or sweep. Only Fresh headroom supersedes a mark, a Fresh zero re-anchors it, and marks still lapse on their own expiry. The weekly half is not ported: T3 was skipped, there is no `weeklyExhaustedUntil`, and porting it would bring back the paid fallback.
- §29.4 (#374, adapted). The laptop daemon on `127.0.0.1:31415` trusted every loopback peer, so any web page in the owner's browser could spend reset credits (`POST /_subrouter/rate-limit-reset?all=true`), drain or quiesce, unpin and prune sessions, mint tenant keys, force tenant reloads, and read sessions, accounts, usage-status and `pool.json` by DNS rebinding. A probe before this fix got 200 for a cross-site POST and for `Host: evil.example:31415`. Now every admin request needs `Sec-Fetch-Site` absent, `same-origin` or `none` (`same-site` covers other 127.0.0.1 ports and is refused) and an `Origin`, if present, equal to its `Host` (`null` refused). A loopback peer must name `localhost`, `127.0.0.0/8`, `::1` or the `--public-url` host, waived only for the admin token, a store-handshake connection, or a tenant-scoped server (`tenantControlAuthorized`). Failures get 403 `cross-site admin request rejected` or `admin request host must be a loopback address`; a hosts-file alias, `http://0.0.0.0:31415` or `curl -H 'Host: …'` now needs the token. `publicSurface` still runs first (flat 401), the session-lease gate and the `MultiTenant` reload hook share `trustedLoopbackAdminRequest`, and top-level navigation to `/_subrouter/pool` and `/_subrouter/dashboard` is exempt from the metadata check (the Host check still applies). A `Use`-only tenant key can GET `account-status` but POST needs `ManageAccounts`. Upstream names are kept for later subtree merges. The provider proxy and owner decision 8's tokenless fallback are unchanged. Follow-up: `requireAccountImportAuth` trusts a tailnet identity a browser also carries (latent, `--tailscale-auth` is off). After deploy, check `sr` and the pool board once.
- §29.5 (#363). A ChatGPT Pro reset credit is finite and never renews. A weekly limit reported as a full `primary_window` (604800 s, no secondary, `limit_reached: false`) now counts as cooked in both `rateLimitCooked` and `localRateLimitCooked`; before, the week-spent account was refused. The auto-redeem path `redeemAccountIfEligible` inherits this, but `SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET` is set in no plist or deploy script.
- §29.5 (#364). `sr reset <email> --dry-run` and `<email> --list` redeemed a real credit because Go's `flag` stops at the first positional. `parseFlagsAnywhere` (`cmd/subrouter/flags.go`) fixes it and reset refuses a second positional.
- §29.5 (#380, adapted). `parseFlagsNoPositionals` / `parseFlagsOneName` cover setup, cleanup, account import/repair, server add/use/install/login/sync, tenant and qwen login (`sr setup junk --plan` had dropped `--plan`, written `cloud.json` and minted the local proxy token). A failed redemption, or no account's usage readable, exits non-zero, out-of-range picker numbers are errors instead of substring selectors, and a broken auto-switch lease lock is surfaced and the sweep runs anyway. Installers never delete a foreign `~/bin/sr` / `cx` (often a status wrapper script): `install-daemon` / `install-systemd` refuse without `--force-shims`, and macOS `sr setup` (`installDaemonForSetup`) warns and installs the rest. Part C (`explicitServerTarget`) is not taken: the disagreement it fixes needs a Team/Hosted credential source no machine runs, would change what `cx` targets under `SUBROUTER_SERVER`, and §22 keeps `SUBROUTER_CODEX_SERVER`.
- §29.5 (#365). `accounts.WeeklyCookedWindow` is the one cooked rule for `sr status`, `sr switch`, `sr reset` and the handler: a full account-wide window of at least six days in either slot, a length-less legacy secondary, or `limit_reached` only when no weekly window is reported and no full shorter window explains it. The last clause saves credits: a 5h stall with weekly room raised `limit_reached`, and `--all`, a named email and local mode spent a credit on an account back within hours. `sr reset` with no target sends `best=true` so the server picks an account it will accept; an older server gets the client pick. Fork change: a failed server pick exits non-zero.
- §29.5 (#367). `--all` previews each account (weekly wait, credit expiry), asks, refuses from a non-terminal without `--yes`, then redeems exactly the previewed accounts one email at a time, so an older server that ignores `min_wait_seconds` cannot spend more. Adds `--min-wait D`, `--candidates`, and `--local` (refused once `sr` routes reset to a selected server; the refusal names `SUBROUTER_CODEX_SERVER=local`). The server orders candidates by an expiring credit (<= 48 h) on an account stuck >= 6 h, then the longest weekly wait (before: goroutine-completion order). It is a new query param on the existing admin-only route, nothing on the public listener. #366 is not taken (premise refuted: the daemon already refreshes tokens), so an unreadable account is still skipped. Deferred: routing the auto-redeem failover through `sortResetCandidates`.
- §29.6 (#369, adapted). `sessions.json` and `tenants.json` went to a fixed `<file>.tmp` with no fsync, and APFS can commit the rename before the data after a power loss, leaving a zero-length or torn file. A torn `sessions.json` failed `session.NewStore`, which is fatal to `serve` and tenant loads, so KeepAlive restarted into the same error forever; a stale directory at the fixed temp name failed every write; deleted legacy Codex accounts were re-imported from `~/.codex-accounts`. It had not bitten yet: 58 laptop daemon starts, including after the 09-26 panic, all loaded. Now `internal/fsutil` writes atomically (`ReplaceFile`, `SyncDir`, `WriteFileAtomic`), a torn file is kept as `sessions.json.corrupt-<unixnano>` (safe to delete, logged as `session store is corrupt`), and `~/.subrouter/codex/.legacy-codex-migrated` stops re-import. Adapted: with nothing in memory the torn file is renamed aside and the store starts empty (upstream); with assignments in memory the torn bytes are copied aside and the file is rewritten from memory, and if that fails the torn file stays live, memory stays intact, and the next access retries. Starting empty on a running daemon would re-pick warm sessions cold (a move that is not an eviction, forbidden by §20 and §21) and zero `CountByAccount`. Accepted residual: a process with nothing in memory (`sr` reading `SUBROUTER_SESSIONS`) can empty the file first. Cost: two syncs per write under `s.mu` (F_FULLFSYNC on macOS), measured 15 ms median and 208 ms worst per flush; Touch writes at most once per 10 minutes per session, and reads never sync.
- §29.7 (#370, adapted). `launchdController.start()` ran `launchctl kickstart -k` on any bootstrap failure, and bootstrap fails (`5: Input/output error`) whenever the agent is loaded, so `sr up`, `sr setup` and `ensureLocalHealthy` (a missed 1.5 s probe at `sr claude` launch) killed a serving daemon and its sessions. Now a failed bootstrap kickstarts without `-k` only if `launchctl print` shows the agent loaded, else returns the bootstrap error; `sr restart` is still the verb that replaces a wedged daemon. A systemd reinstall now backfills `--addr`, `--sr-switch-interval` and `--sessions` from the EnvironmentFile unless passed (a rerun had rewritten `SUBROUTER_ADDR=0.0.0.0:31415`, opening a tailnet-only box on every interface, and brought a disabled `0` sweep back at 10m), using a `flag.Visit` map merged with `ExtraArgsExplicit` so `--extra-args ""` still clears; passing a flag at its default value is still explicit. This box half is latent for the fleet box (its bootstrap script always passes `--addr`) and live for any hand-run `install-systemd`. A changed socket unit stops the service, restarts `subrouter.socket`, then the service (dropping queued connections once); system stop and remove act on the socket first. GCP `sr server install` forwards only flags the operator passed, since its old forwarded `--extra-args ''` tore down the public listener on this fork; the SSH path refuses them. Windows passes `--sr-switch-interval 0` through and `installWindowsTask` now installs the exe to `%LOCALAPPDATA%\Subrouter\bin\subrouter.exe`. `sr server install` still runs the upstream release (`publicInstallScriptURL`), against the owner decision; since §37 it needs `--allow-upstream-release` (37.3). Known limitation: the GCP health loop probes `127.0.0.1:31415`, so a host whose preserved bind is elsewhere reports a failed install.
- §29.8 (#423). Since upstream `684e1ea` (2026-08-27; here since the §23 merge) transcripts store the requester only as `user_hash`, but `transcript.Analyze` and the index read `user`, so every dashboard row grouped as `(unknown)`. `eventUser` now reads `user`, then falls back to `user:<hash>`; files are byte-identical to `e3b3a91`. Dormant: it needs `--transcripts`, which neither the laptop plist nor the box's bootstrap script sets (the live box was not checked). The Cloudflare worker has the same bug and is not ours.
- §29.8 (#417 plus the #420 pacer hunk). `goldenWebSocketPacer.runWriter` re-checked `frameNeedsDelayLocked` after waiting, so after the first data frame it looped and delivered nothing more until the gate released. The re-check now runs only when no interval was applied, and the tests' 1 s / 250 ms deadlines became one 30 s hang guard. Only `cmd/subrouter-transport-observer` has this code; no user sees a change.
- §29.9.1 (#463, adapted). Claude Code turns tool search off behind a non-first-party `ANTHROPIC_BASE_URL` (2.1.283 logs `[ToolSearch:optimistic] disabled`), so routed sessions loaded every MCP schema up front, lost deferred tools, busted the prompt cache on late servers, and hid failed servers. `true` is safe because the OAuth path forwards body and `Anthropic-Beta` intact; only the Fable Bedrock fallback drops betas, and it fails over to the Fable API key. Routed `sr claude` launches get `ENABLE_TOOL_SEARCH=true` in their environment (`withClaudeToolSearch`), and Subrouter profile directories get it in settings.json (`profileOwned`), unless the user already set it to any value. The launcher puts it in the environment, not the private `--settings` file, so a value in the user's or the profile's settings.json still wins. The user's own `$CLAUDE_CONFIG_DIR` or `~/.claude` is never written, because plain `claude` reads it and client-config writers there are a known brick class; the first port did write it and was narrowed before merge. `sr setup` never reports the key missing. Direct launches are unchanged. Widening to `~/.claude/settings.json` waits on one routed session checked by hand (an MCP tool loads through ToolSearch) and the owner's sign-off.
- §29.9.2 (#435, adapted). Launches that set `CLAUDE_CONFIG_DIR` to a profile or proxy directory never loaded `~/.claude/settings.json` (12 hook events, permissions, a status line), and each new directory re-downloaded the plugin marketplace (2026-09-28: several profile directories with 6.2 MB copies, most linked by hand). `withClaudeUserSettings` now layers sr's launch settings over the user's file inside the private `--settings` file. Adapted: every `claude.RoutingEnvKeys` key is stripped in any case, the user's raw env is never restored, nothing is copied when Claude already reads the file (it would double every hook), `--setting-sources` wins, and direct `sr claude run` gets it only when the caller passed no `--settings` or `--managed-settings`. Credential keys are dropped (`apiKeyHelper`, `awsAuthRefresh`, `awsCredentialExport`, `gcpAuthRefresh`, `forceLoginMethod`, `forceLoginOrgUUID`), and the directory's own single-value keys win. §21's 1h TTL injection reads the overlay's `promptCacheTtl`. `plugins/` links to `~/.claude/plugins` only where absent. The bounded picker and status-line chaining are not ported.
- §29.9.3 (#441). `sr claude proxy` homes (`claude-proxy/<hash>`) shared only history directories, so they lacked the user's skills, agents, commands, memory and key bindings. `Store.ShareUserConfigDir` links `CLAUDE.md`, `skills`, `agents`, `commands` and `keybindings.json` only when the user has the entry and the proxy home has nothing or an empty first-run directory (kept as `<name>.subrouter-local`). `settings.json` (filtered overlay) and `.claude.json` (the home's login state) stay out, failures are logged, and profile directories are deliberately not linked (they are logins a user may launch and customize directly).
- §29.9.4 (#430, adapted). Proxy homes asked again to trust every folder the user had trusted. Before a proxy launch, `true` values of `hasTrustDialogAccepted`, `hasCompletedProjectOnboarding`, `hasClaudeMdExternalIncludesApproved` and `hasClaudeMdExternalIncludesWarningShown` are copied from `$CLAUDE_CONFIG_DIR/.claude.json` (else `~/.claude.json`) into undecided entries only; an explicit `false` survives, a held `<file>.lock` skips the launch's sync, and failures are logged. Upstream's write-back into the user's file is dropped: every plain `claude` session rewrites it (24 on the laptop), the owner's rule is that nothing `sr claude` writes may alter plain `claude` launches, and 2.1.283's lock behavior was not proven. Cost: a folder trusted only in a proxy session is asked again elsewhere. Trusting it once in plain `claude` covers every proxy home from its next launch.

Not taken: #366, the overload half of #371, #380 part C, the weekly half of #398, #463's Bedrock beta mapping, #435's bounded picker and status line. Only §29.9 changes what `sr claude` writes, and plain `claude` reads nothing new. On integration `TestHomeDirResolutionStaysInsideTheAllowlist` went red because `claudeUserConfigPath` is a new `os.UserHomeDir` caller; it is read-only and honours `CLAUDE_CONFIG_DIR` first, so it was allowlisted. Every behavioural test was watched failing on the pre-harvest tree and passing after (tests marked guard pass on both by design), in Docker (`golang:1.26`, `--cpus=2`), never against a real home, keychain or daemon. `TestDeployTestCommandCancelsDescendantProcessGroup` fails 11/20 without `docker run --init` and passes 20/20 with it. The wall-clock tests `TestAccountFetchSweepsUseOneDeadlineAcrossAllBatches` (75 ms) and `TestUsageStatusesLiveRotatesStarvedTailAcrossSweeps` (150 ms) flake under load and need their own fix.

- Knobs: `SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET` (set nowhere; auto-redeem off), `ENABLE_TOOL_SEARCH` (`true` on routed launches and profile directories unless the user chose), `SUBROUTER_CLAUDE_USER_SETTINGS` (`0` disables the settings overlay), `SUBROUTER_CODEX_SERVER=local` (reaches the local store), `--force-shims` (lets installers replace a foreign `sr`/`cx`), `sr reset --yes` / `--min-wait D` / `--candidates` / `--local`, `--public-url` (its host passes the loopback Host check), `--transcripts` (off on the laptop and the box), `--allow-upstream-release` (§37.3)
- Code: `internal/proxy/single_flight.go` (`singleFlight.do`), `internal/proxy/pinned_account_unusable.go` (`pinnedAccountCannotRecover`), `internal/fsutil/atomic.go` (`WriteFileAtomic`)
- Related: §35 amends §29.2 (35.2) and §29.3a (35.1, 35.3). §37.3 amends §29.7. Builds on §20 and §21 (warm rule, TTL), §22, §26.

## 30. An opt-in Unix socket serves Claude Code in first-party mode

Claude Code enables first-party behavior (tool search with deferred tools) only when the base URL's host is
`api.anthropic.com`. The scheme is not checked. `http://127.0.0.1:31415` never qualifies. Its
`ANTHROPIC_UNIX_SOCKET` transport, with `ANTHROPIC_BASE_URL=http://api.anthropic.com` and
`CLAUDE_CODE_OAUTH_TOKEN=ssh-placeholder`, writes every request as plain HTTP into a socket. Measured on
2.1.284 that is first-party: 12 tools deferred, against 21 or 24 and none deferred over TCP. `serve` now
serves the same handler on that socket. Unset changes nothing.

Plists use the env var, never the flag: an old binary exits on an unknown flag, and a rolled-back backup would
crash-loop under KeepAlive. Socket failures are soft (logged; TCP keeps serving), because reusing the fatal
`--local-data-socket` opener would have made a socket problem a respawn loop with every TCP client down. Health
reports `claude_socket` as `serving`, `unavailable`, `stopped` or `ignored_under_supervise` (fixed words, since
health is also served on `--public-addr` and an error string carries the path). §77.1 adds `waiting` and
`displaced`, and the socket is no longer opened only once. §77.3 adds `yielded`, in supervisor-status only. A socket peer reports `127.0.0.1`, so loopback gates treat it as
loopback TCP and it gains nothing more. Without this, a token-enforcing daemon would 401 the placeholder and
machine policy would count the socket as a second machine. The socket is 0600. The
proxy replaces the placeholder `Authorization` with the pooled credential. The socket is bound as
`<path>.bind`, chmodded 0600, then renamed, because a crash between bind (0755 under umask 022) and chmod left
a socket that `removeStaleSocket` refused on every later start. A path over 98 bytes on darwin (102 on Linux)
is refused up front; checked later, a too-long path read as "nobody listens" and a live socket was unlinked.
A stale socket is removed only under the `<path>.lock` lease and when a dial gets no answer. Supervised
workers ignore it and `supervise` refuses `--unix-socket` in worker arguments, since a retiring worker's Close
would unlink the path with nobody left to re-bind. The supervisor does not serve the socket. `sr`
scrubs `ANTHROPIC_UNIX_SOCKET` (`claude.RoutingEnvKeys`) because a process-env socket beats the base URL.
`deploy-laptop.sh` rolls back when the old build served the socket and the new one does not, and exits 3
(INCOMPLETE) when neither did.

Client keys go in the process environment only. Claude Code 2.1.284 honours `ANTHROPIC_BASE_URL` from
settings.json but not `ANTHROPIC_UNIX_SOCKET`, so all three there sent the conversation as cleartext HTTP to
api.anthropic.com:80. A dead socket hangs socket-mode sessions with no TCP fallback, and
`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` stalled them in 3 of 3 runs. Adopt by adding the variable to the
plist and deploying with `RELOAD=1` (`launchctl kickstart -k` does not re-read the plist).
`sr setup` and every settings writer still write the TCP base URL; no default changes.
`sr claude --unix-socket[=PATH]` (first argument; default `<state dir>/claude.sock`) launches socket mode
after a health check over the socket. It strips `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_API_KEY` and ambient
`HTTP(S)_PROXY`. It falls back to TCP with a warning when the socket does not answer or
`sr` holds a real local proxy token. `_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL=1` gave the same first-party
request over TCP with no socket. It is internal, and checks that read the host directly were not tested.

- Knobs: `SUBROUTER_CLAUDE_SOCKET` (unset; no socket, nothing changes), `--unix-socket <path>` (wins over the variable; hand runs and tests only), `SUBROUTER_ALLOW_LOOPBACK_PROXY=1` (still needed by a socket client on a token-enforcing daemon)
- Code: `claude_socket.go`, `sr_claude_socket.go`, `deploy-laptop.sh`
- Related: §36 adds the `sr doctor` warning for socket keys in settings.json. Since §37, `sr setup` and `install-daemon` keep the hand-added plist variable.

## 31. One ranking across every pool; a peer serves the targeted account or refuses it

Owner rules, 2026-09-28. P1: burndown (within ~24 h of the weekly roll) routes anywhere, under the same
eligibility and taper. P2: one ranking, eligible burndown accounts first, then `remaining% x 7 / days to the
weekly reset` with local counted 2x (100% at 5 days = 140, or 280 local); only eligible accounts compete. P3:
the account's owner decides eligibility (exhausted, 5 h floor, 5 h burn projection, burndown cap, weekly
reserve) through one implementation. P4: a peer session is tied to its account; when that runs out, P2 picks
across every pool. P5: a move onto an account that answered sticks (§25). P6: any "no" to a target
recomputes, bounded. P7: any number of peers; a request from a peer is served on its target or refused, never
forwarded again. P8: only a forced switch (window spent, evicted or holdout, dead credential, account gone,
target refusal) recomputes. Transient trouble (overload, a 429 that is not a spent window, a blip, a refresh
with no verdict) keeps the account and serves at most that turn elsewhere. P9: a cold session re-ranks
across everything on the 70-minute timer. D7: one ranking function at every re-rank site.

Why: 2026-09-27 19:00 to 09-28 04:40 UTC, the laptop compared its pick with the box's best score, re-derived
from published numbers. The box's `chip-5` (about 4% of its week left, 16 h to the roll) re-derived as
drain-eligible because usage-status lacked live session counts, though it was at its taper cap. About 23
untargeted sessions landed on spent 5 h windows (23,195 `selected OAuth account below new-session headroom;
routing optimistically` in 24 h) while the laptop's `fourteen` (12% used) and `fifteen` (2%) sat idle.

Answering half (every daemon). Each measured OAuth row and `normalized.pools[k]` in usage-status carries a
`new_session` verdict from `Scheduler.NewSessionVerdict` on the daemon's own selection scheduler, so it cannot
disagree with the local pick. Reasons, in precedence: `exhausted`, `holdout`, `at_cap`, `burndown`,
`eligible`, `short_floor`, `below_reserve`, `burn_projection`, plus `auth_invalid`. A missing verdict (no
score, API-key rows) is never read as eligible or ineligible. `X-Subrouter-Target-Account: <id>` is a hard
placement (`forcedAccountSelection`) that nothing may substitute or forward. A warm session pinned there is
refused only when evicted (§20); anything else must pass the verdict now, at-cap included (X3: a peer piling
past the cap is the incident). A refusal is `409` with `X-Subrouter-Target-Refused: <reason>`, before the
provider or any session-store write (one exception: a dead credential found by the request-time refresh). An
answer that reached the provider passes through, stamped refused when the target is now evicted, a 401, or
Claude said `rejected`. A limited 429 is stamped `X-Subrouter-Transient: 1`, and a 529/5xx that
outlasts the same-account retry is `X-Subrouter-Transient: overload`, which keeps the hold. A refresh blip
keeps its `502` + `X-Subrouter-Transient`, a filtered-out API-key target reads `not_oauth`, and a malformed
target header is treated as absent. The header is stripped before anything leaves the daemon, and a turn with
no target header takes today's path byte for byte.

Deciding half (any daemon with peers; today the laptop). `placeAcrossPools` adds eligible peer accounts as
`Remote` scores beside local candidates (live debits and weekly reserve included, owner follow-up F2) and runs
the shared comparator once; `pickRoutingAccount` is the same function with no peers. A body with no verdicts
is a legacy peer: never targeted (X1: an old daemon passes unknown headers upstream). It competes only where
origin/main consulted the peer. `crossPoolRanking.targetedOnly` drops it from a forced switch inside local
failover and from a cold session still on its best local account, because an untargeted forward is placed by
the old daemon's own rules, past a burndown cap (the 9/27 path). A verdict peer that serves the target on
another account keeps the turn but not the session, and ranks as legacy until its next body. Burndown-first
applies among eligible accounts only. Since §41, two draining accounts compare on time to the roll discounted
by live sessions (the earlier roll leads but shares), ties break on raw pace, and there is no local 2x inside
the tier. Pace is `WeeklyHeadroom / secondsToRoll` (local 2x) after the Fable reservation and live debits,
discounted by live sessions, in 1% tie buckets. Machine policy applies to the merged ranking: an account this
machine is bound to outranks a peer account (inert on a loopback daemon). A decision fetches every usable
peer in parallel and waits at most 250 ms (`remotePoolDecisionFetchBudget`, X6); a slow peer contributes its
last good body. Each peer keeps its own client, breaker, admin cache and hold table; a session is held on at
most one, and holds persist in `peer-holds.json` (first peer) and `peer-holds-<name>.json`. A refusal marks
that peer account until a newer body and the ranking recomputes, with at most `remotePoolMaxTargetRefusals`
(2) failed peer attempts per turn, an unreachable peer included, then local. A cached verdict (60 s or newer)
of evicted, dead or gone, a refusal, or a legacy decline ends a hold; a warm held session below the floor or
at the cap stays. A breaker opening ends a hold only when this machine can attribute it to the peer
(`peer_down`, §54; any other opening is `transient/peer_unattributed`). Since §67 no opening ends a hold: a
held model turn whose peer cannot be reached waits for it under the hold until it answers, or until it is
found down (attributable, and silent for the grace), and is then placed as a refugee (§67.3, §67.4). It is
never ranked onto another peer. `classifyAccountSwitch` is the one forced vs transient table (17 rows; 21 since §54), and
`commitSuccessfulFailover` commits only a forced move: after a limited-not-spent 429 a
Claude session keeps its account (`transientFailoverKeepsPin`), and a move already under way
(`commitFirstSuccess`) commits as §25 says. Inside local failover a forced switch runs the one ranking over
the local failover candidate and every verdict peer (`forcedSwitchAcrossPools`); a target refusal recomputes
with the local candidate still ranked, so it lands local, never on a lower peer account and never untargeted.
With nothing local and no eligible peer account, the turn goes untargeted to the first usable peer, not a 503
(X2).

Merged with main (§27, §28): a Claude account whose credential store is silent is not a destination while an
answering one passes the refusal gate, and a recompute after a refusal does not re-offer it. §27's capacity
release moves a session only when a LOCAL account is not shedding (peers publish no capacity marks), and a
forced switch skips the capacity keep. Deploy the box first (its restart cuts in-flight streams; pick a quiet
moment), check that `curl -s …/_subrouter/usage-status | jq '.[0].normalized.new_session'` answers, then the
laptop. Mixed versions degrade to today's behavior in both directions.

Round 3: a sweep refresh with no verdict is `sweep_cut`, not a dead credential (it ended every warm hold for
15 minutes after each box restart); `capacity_evicted` is forced, `credential_unchecked` transient,
`pool_exhausted` forced for that model; a verdict body older than 15 minutes (`remotePoolDecisionBodyMaxAge`)
ranks nothing, and a row whose weekly roll has since passed is dropped. Every forced re-rank site (a
request-time dead credential, `model_unsupported`, the Codex websocket quota failover, both Fable->Opus
stages) ranks across every pool through `crossPoolRanking.forcedRerank`. Each peer account is debited for the
targeted turns this daemon sent it since the body (`RemotePool.routed`). A targeted Codex capacity failure
runs the same-account ladder only, bounded at `codexCapacityTargetedRetryBudget` (10s) to answer inside the
decider's 30s header timeout, and is stamped `X-Subrouter-Transient: overload`. A stale Codex session (idle
>= `warmSessionIdleTTL`) re-ranks across every pool (policy row `{coldRerank, retentionFloorGated}`); the
retention floor now gates only the keyless `codex:fallback` bucket. Left open: the mid-socket Codex quota
failover still commits a local alternate, a targeted Codex turn sets no capacity mark, and a peer served
through the transport-level stage is answered with local stamps (`X-Subrouter-Pool: local`).

- Knobs: `SUBROUTER_REMOTE_POOLS_FILE` (unset; the four `SUBROUTER_REMOTE_POOL*` variables configure one peer named by its URL host). The file is gated by `remote.key`, mode 0600, at most 64 KiB and 16 peers, and fails closed: one bad entry arms no peers, and the single-peer variables are never a fallback.
- Code: `internal/proxy/target_placement.go`, `selectacct/new_session_verdict.go` (`Scheduler.NewSessionVerdict`), `internal/proxy/remote_pool_target.go`
- Related: §41 amends the draining-tier order. §54 and §67 amend when a breaker opening ends a hold. §35 settles Codex transient failover (keeps the pin; the capacity switch, two failures in a row, commits).

## 32. `sr status` shows the box via the daemon's peer link when its own address is out of reach

The owner's only `servers.json` entry is the box's tailnet address, and `secureTenantServerURL` refuses
plaintext to a non-loopback host unless `tailscale status --json` says online. With Tailscale off the box
collapsed to `Tailscale is offline; plaintext tenant routing is disabled`, while the local daemon still
reached it over its pinned-TLS peer link. Owner goal: see the box whether or not we are on Tailscale.

The daemon serves `GET /_subrouter/peers/usage-status`, one entry per configured peer, each read through the
routing path's `usageForDecision` (10 s cache, one read in flight, a failed peer not redialled until
`remotePoolAdminTTL`). A box costs at most one read per 10 s; the first cut used `AdminGet`, and a
black-holed peer cost every `sr` call 3 s. A non-array body is `reachable:false`, `url` is scheme and host
only, and `/t/<key>` is scrubbed from errors. The route waits up to `remotePoolAdminTimeout` + 250 ms. The
route is `requireAdmin(requireLoopback(...))` (tailnet
admin token 403, public listener 404), because it relays a roster read with a peer credential the caller
never presented.

`sr` reads it concurrently inside the same 5 s cap, only when `localBaseURL()` is loopback, gated on
`remote.key` (§17) and `SR_STATUS_SERVERS`. A
peer is hidden only when every `(provider, account id)` it reports is in ONE rendered `servers.json` section;
the first cut hid on any shared account and dropped a different box. A hidden peer whose latest read failed
gets one dim line. Other peers render as `Server: <peer>
(via this machine's peer link, <url>)`. A last-good body says `last good as of` with a red `unreachable from
this machine's daemon: <reason>`, and a body over 2 minutes old says `as of 4m ago`. A 404/405 (older daemon)
adds nothing, and the local table and exit code never depend on the route. A machine with no `servers.json`
entry waits up to 1 s (`localHeadingWait`) before printing the local table; the first cut held it for the
whole read (up to 5 s). Not done: an account both local and on the box shows twice, two peers that are the
same box both show, and the CLI cannot tell that a failed entry and a peer are one box.

§32.1 `sr plot` (`ops/bin/sr-plot`) reads what `sr status` reads. Diagnosed live 2026-09-29: local
got the box's 8 s cap though a cold read took over 8 s at load ~100; the headline named scopes it never read;
sweep-cut rows dropped silently; the box was read only at its tailnet address. Local now gets 65 s (the
daemon bounds its sweep at 60 s, `usageSweepTimeout`), a box 8 s, the peer view 15 s, in parallel. Peers
merge by §32's roster rule only; a shared name proves nothing. A peer body's resets move on by its age, and a
body over 15 minutes old (`usageWindowsLastGoodTTL`) is not counted (the first cut counted a 6 h body). Each
missing scope gets an amber line (`LOCAL MISSING`, `HOME NOT READ`), left-out rows are named by reason, and
nothing read or measured exits 1. Gates match `sr status`: no `remote.key` (or one that is not a regular 0600
file of 16+ bytes) means local only. The admin token is an unredirected header, and a redirected answer is a
failed read. Ctrl-C exits 130 at once. `~/bin/sr-plot` is a standalone copy, so nothing changes until the
owner puts `ops/bin` on `PATH`.

- Knobs: `SR_STATUS_SERVERS` (`0` means local only: no box and no peer view)
- Code: `internal/proxy/peer_usage_view.go`, `cmd/subrouter/sr_status_servers.go`, `ops/bin/sr-plot`
- Related: §43.2 lets a `servers.json` entry fall back to its public admin path, so the section renders and a same-box peer is hidden.

## 33. A peer-forwarded turn streams until it ends; a quiet peer is cut, and every cut is logged

The peer client (`RemotePool.client`) carries every HTTP model turn the laptop forwards to the box over the pinned `:31416` link. Its `http.Client.Timeout` was 5 minutes, which covers reading the body. So every forwarded HTTP turn was cut 5 minutes after it was sent, however busily it was streaming. On the handler relay, which serves nearly every peer turn, net/http then ended the body cleanly: the client got 5 minutes of SSE with a well-formed end, and neither daemon logged a cut. Reproduced before the fix: a 7-minute stream, one event every 20 s, stopped at 5m0s after 15 of 22 events. On 2026-09-28 the laptop logged 78,916 `serving from remote pool ... transport=http` lines against 136,663 local `INFO proxy request` lines, so more than a third of its traffic rode this client.

The peer client now has no total timeout, and each phase has its own bound. Dial and TLS (2 s), time-to-headers (`remotePoolResponseHeaderTimeout`, 30 s) and admin reads (`remotePoolAdminTimeout`, 3 s) are unchanged. A write-stall bound (`peerWriteStallConn`) sets a 30 s write deadline before each piece of at most 32 KiB, so a send that stops fails and anything faster than about 1 KiB/s passes. Without it, a large turn sent on a keep-alive socket that a VPN flip blackholed ended only when the client gave up, read as `client_canceled`: no breaker strike, the hold kept, the next turn sent into the same hole. Now it fails at 30 s, counts a strike, and falls back to local accounts. A stream watchdog (`peerStreamWatchdog`) closes the peer's body once a single Read has waited 5 minutes without a byte. Only time inside Read counts, so time a relay spends blocked writing to a slow client never counts toward a cut. 5 minutes is the old number counted while waiting on the peer, so it cannot lose anything the old client delivered, and Codex (300 s) and Claude Code (about 270 to 300 s) usually give up first. While the answer streams, the hold is touched at most once per 30 s, because an answer can now outlive the hold's 70-minute TTL.

Every cut seen while reading the peer's answer is logged once as `remote pool stream cut`, with `cause` set to `idle_timeout` (Warn), `peer_error` (Warn) or `client_gone` (Info), plus `silent_ms`. When the peer's body breaks and the client is still there, the handler relay panics with `http.ErrAbortHandler`, as ReverseProxy does, so the client sees a torn body instead of a truncated answer passed off as whole. The websocket path never used this client and is unchanged.

Residual risks. Nothing bounds a stream's length. A legitimate silence of 5 minutes or more is cut, as before. A client that hangs up while the relay is writing is not logged, so `client_gone` undercounts. A bound below the clients' detectors (the Bedrock watchdog uses 120 s) is an owner call not made here. The other `flushingCopy` relays (Bedrock, the Azure Codex fallback, both Fable fallback serves) still end a broken stream cleanly and log nothing, as recorded in KNOWN-ISSUES.

- Code: `internal/proxy/remote_pool.go`, `internal/proxy/remote_pool_stream.go` (`peerStreamWatchdog`, `peerWriteStallConn`), `internal/proxy/bedrock.go` (`flushingCopy`)

## 34. Keychain slowness no longer costs a rotation, starves the sweep, or hides why a turn left

Three laptop findings after §28 in the Claude credential path, plus two review-4 follow-ups
to §28. The box was never affected.

§34.1 Anthropic rotates the refresh token on use, so after the exchange the persist holds the only
live copy. That keychain write was one `security add-generic-password -U` on a 5 s clock inside the
10 s persist clock, with no retry. On 2026-09-27 17:41-17:44 four writes logged `write Claude
credential to keychain: signal: killed`, and two of those accounts (fifteen-beta,
nine-alpha) went `invalid_grant` minutes later. The owner approved this fix.
`writeKeychainCredential` now retries every failure until the caller's clock runs out, pausing
250 ms doubling to 2 s (`-U` keeps retries idempotent). A rotation the keychain still refuses goes
to `.credentials.json`, marked as a stand-in by `<instance>/.credentials.keychain-pending`. The next
credential the keychain takes removes the file and the mark. An import clears the mark. (The first
cut made the profile file-backed for good, so the keychain item a direct Claude Code launch reads
stayed spent.) A persist that cannot take the credential lock tries a second fresh 10 s clock, then
writes `<instance>/.credentials.rescued.json` (0600) with `replaces`, a hash of the replaced
credential. The same file is the last resort when `.credentials.json` itself cannot be written.
`ReadCredential` adopts the rescue (`adoptRescuedRotation`) while the store holds the
`replaces` credential or none, and drops it otherwise. A later-expiry rule was rejected because an
older rotation could clobber an import. No rescue is written for a profile removed or re-added
meanwhile (registry `CreatedAt`). A re-read error or an empty store now writes the rotation instead
of dropping it with no ERROR line.

§34.2 Every turn and every sweep account ran `refreshProfileCredential` under the refresh and
credential locks, nearly always to learn the token was still good. The laptop, five
hours from 16:37 on 2026-09-28: 313 `account reload refresh failed` (240 a sweep out of budget
waiting for a lock, 70 a killed `security` read) and 16 sweeps with no fresh score. Three warm
turns cut to `errRefreshTransient` were served on the box cache-cold, on accounts at 0.51 and 0.56
headroom. `ClaudeConfigDir` also held the shared-state lock with no deadline, so one slow profile
held up all. Now a keychain-only profile's answer (`keychainAnswers`) is served without either lock
or `security` for `keychainAnswerReuse` (60 s) while its access token needs no refresh. Credential
paths use `claudeInstancePath`, which skips the shared-state preparation. Only a real answer or
this process's own write is remembered. Any non-answer, failed write, delete or Claude 401
(`agentclaude.ForgetKeychainAnswer`) forgets it. An answer is stamped with the profile's
`CreatedAt`. A token inside the 60 s refresh window or with no recorded expiry, a forced refresh,
and a rotation in flight (`keychainRotationStarted`) never reuse, and reuse never feeds an exchange. A profile with a
`.credentials.json` or a rescue is read every time (every profile on the box). The 5 s budget
(`usageStatusFetchTimeout`) is unchanged.

§34.3 A warm turn sent to the peer after a no-verdict refresh logged `account=""`,
`local_known=false`. The decision line now names the account with a new `local_cause` key, plus
`local_failed_over_from`. A Warn `selected account refresh reached no verdict; this turn is not
served on it` is logged too.

§34.4 R4-1: `placeableAccounts` read silent-store state in two passes, so a failure between them
gave 503 `no claude accounts` 17,823 times in 5.77 M calls. One snapshot per candidate now drives
both. R4-3: 50 minutes of hangs plus one non-hang failure was an instant verdict. The quarter hour
and its three reads now run from the first non-hang failure (`refusedSince`). The hour still runs
from the first failure of any kind. R4-2 is latent and stays in KNOWN-ISSUES.

§34.5 Residual risks. Until a stand-in's next rotation lands in the keychain, the keychain item
holds the spent chain, so a direct Claude Code launch on the profile may get a dead token. Never
delete a stand-in file by hand: it can hold the only live refresh token. A rotation is still lost
if the daemon dies between exchange and persist, or the profile is removed during it (deliberate,
ERROR). With nowhere at all to write (keychain, file and rescue all refused) the refresh answers a
verdict. A rescue that cannot be read waits as a verdict naming the file. Another process's
rotation is seen up to a minute late. Keychain silence still sends warm turns to the peer.
Stale-if-error would change §28, and the owner has not ruled on it. An answer older than a minute,
a file-backed profile, and a profile beside a rescue are still read under the 5 s sweep budget.
Reloads and `account-status` still queue on the shared-state lock.

§34.6 Since §28 landed, two of its tests failed on every darwin run, because emptying PATH does not make a Mac
vault-less. `defaultKeychain` decides by `runtime.GOOS`, never by probing for the tool, and the
Linux box was measured unaffected.
A missing `security` is never absence, since that gives every keychain-only profile a dead-login
verdict (§28's harm). Review showed a daemon whose PATH lacked `/usr/bin` still reached that harm
after 15 minutes and three reads. A Mac now runs `/usr/bin/security` by path (`securityTool`). A
test binary (`testing.Testing()`) runs it by name, which adds 18 KB to `sr`. A `security` that never
ran to an answer (EAGAIN, ENOMEM, jetsam, missing tool) is `errSecurityReadNotRun`, bounded like a
hang: the hour, never `CredentialStoreNotAnswering`. Only an exit status (36, 51, ...) is a refusal
and starts the quarter hour. A start failure (ENOENT) never passes as a missing file
(`securityStartFailure` keeps only its text). Open: bare darwin `go test` still reaches the real
`security`, and the `antigravity` store still runs `security` by name.

- Code: `internal/agents/claude/store.go` (`writeKeychainCredential`, `writeRotation`), `internal/agents/claude/credential_reuse.go` (`keychainAnswers`), `keychain_tool.go` (`securityTool`)
- Related: builds on §28 and closes its "Still open after §28" cut write. §44.1 must merge with this section's `writeRotation`.

## 35. Codex keeps its pin on transient failures; bursts rank last; on-hold fails over marked

Owner decision, 2026-09-28 (P8): only a forced switch moves a session's pin and re-ranks it. Forced means a spent window, an eviction (capacity eviction included, §27), a dead credential, the account gone, or a refusal. A transient failure (a 529 or overload, a refresh blip, a network error, a headerless burst 429) keeps the account and serves at most that one turn elsewhere. P5: a session moved off a broken account onto one that answered sticks there. §31 wired P8 for Claude only. The owner has since extended the transient rule to Codex, and asked that a Codex burst 429 and an unpinned `account_on_hold` stop failing over unmarked, so the next pick does not land on the same account. Unchanged, as decided on 9/28: the 70-minute warm rule for Codex, no retention floor, no +10pp gain gate, and upstream's capacity marks as §27 ported them.

§35.1 A Codex burst 429 used to move the pin. Codex's policy row lacked `transientFailoverKeepsPin`, and the capacity layer (`codexOverloadFailoverTransport`, §27) committed every move the layer below made without asking why. Now the usage-limit layer tags each answer with why it first left, and the capacity layer keeps that first leave across its attempts and keeps the pin when it was transient. The capacity layer's own switch still commits: it leaves only after two failures in a row on the request's own count, which is §27's `capacity_evicted`, forced. When the capacity layer already moved the request, the usage-limit layer leaves the pin to it, except a move already under way (`commitFirstSuccess`), which commits as §25 says. A Codex 429 coded `usage_not_included`, `insufficient_quota` or `quota_exceeded` (in `code` or `error.type`) is now marked exhausted (its own reset when it names one, else the short default mark) and forced. It used to read as a burst, and with a kept pin the session would have paid a 429 and a failover on every turn. `rate_limit_exceeded` stays a burst over HTTP, because there it is a request rate. The laptop log since 08-03 holds no Codex usage-limit failover, so the exposure is the box's Codex HTTP pool and the 426-to-HTTP fallback. On the websocket, `rate_limit_exceeded` still marks the account exhausted and moves every warm session on it. Changing that needs the owner. Nothing in §35.1 changes a Claude turn.

§35.2 Anthropic's `account_on_hold` (a 403 from the token endpoint) was neither transient nor on `refreshFailureCannotServe`, so an unpinned request got the account back unmarked. The box answered the no-account 503, which a forwarding laptop reads as a decline. The laptop, which holds the Claude accounts, handed warm sessions to the peer with a 70-minute hold. The next pick could land on the same account until a usage sweep zeroed it. Now `refreshFailureCannotServe` accepts `accountOnHold(err)`. `refreshFailureIsTransient` is still asked first, so a 5xx whose body echoes the text stays a blip. The account takes the short mark (`DefaultExhaustedTTL`, 10 min), the request fails over locally, and the switch is forced and sticks (P5). A target on hold is now marked as it is refused `auth_invalid`. A session lease whose sticky account is on hold is issued on the replacement instead of answering 503. `accountOnHold` stays off `isTerminalCredentialError`, so the credential failure cache and the long credential hold do not change, and a caller's own pin keeps §29.2's final 403.

§35.3 A Codex 429 that names no usage limit marked nothing, so new placements could land on the account mid-burst. Neither existing mark fits P8. An exhaustion mark evicts every warm session. §27's capacity mark releases warm sessions at the second failure in a row, which a burst reaches at once (tried: the pinned session was capacity-evicted at its third turn). So a fork-only rate-limit mark (`MarkRateLimitedUntil`) is set per account, only on an untargeted HTTP turn. A 429 whose body could not be read takes it too. It lasts the upstream's retry hint clamped to [30 s, 5 min], else 30 s. A success does not end it, because a burst's refusals and successes interleave. It counts as one capacity failure for ranking only, so new placements rank the account after unmarked ones. It never evicts and is never persisted. Open question for the owner: whether some number of bursts in a row (the capacity rule's two, say) should count as forced.

Residual risks. A Codex account that keeps bursting costs each pinned session one extra round trip per turn (P8's trade). The rate-limit mark rides the comparator's capacity clause, so for its 30 s to 5 min it outranks the machine binding and the endgame drain, and the capacity board does not list it. The on-hold mark is the short one (10 min). After it, the account stays out only while the usage sweep keeps reading its 403, so an unreachable sweep costs one wasted refresh per 10 minutes. A restart clears the rate-limit marks. No config change, and the daemons need not move together.

- Knobs: `--usage-score-ttl` (30 s; selection re-sweeps scores older than this)
- Code: `internal/proxy/selection_policy.go` (`transientFailoverKeepsPin`), `internal/proxy/codex_overload.go`, `selectacct/rate_limit_marks.go` (`MarkRateLimitedUntil`)
- Related: extends §31's P8 pin rule to Codex. Closes the on-hold gap recorded in §29.2. Keeps §27's capacity marks as ported.

## 36. `sr claude env` honours names; re-add hints fixed; `sr doctor` fails on the socket recipe

Three CLI defects from the 2026-09-28 fix programme. Each sent an operator to a command that did the wrong thing. Routing is unchanged, and in the daemon only the pinned-account 403 text changes.

§36.1 `sr claude env [name]` now exports the named profile's directory. It used to ignore the name. On 2026-08-28 it answered main's directory for seven-alpha and ten-alpha, so `CLAUDE_CONFIG_DIR=$(…) claude auth login` re-authed main, the account serving the session. A name must match exactly or differ only in case (`existingClaudeProfile`). The fragment match that `run`, `switch` and `remove` accept is rejected, because `env` decides where a sign-in writes: `sr claude env seven` would overwrite seven-alpha. Unknown, fragment, empty or blank names fail and print nothing, so `eval` exports nothing. An empty name never falls back to the active profile. No name still means the active profile. Naming a profile does not make it the active one. A dead login is still repaired with `sr claude addr <name>` (§22).

§36.2 The re-add hint said `sr add claude` for every Claude auth error. Since the 2026-09 merge (§23) that verb mints a one-year setup token and refuses without a name. On 2026-09-27, following the hint minted setup tokens for accounts whose OAuth login had died. Hints now name the profile: `sr claude addr <name>` for OAuth, `sr claude add <name>` for a setup token. A server's rows say `re-auth on that server` and use the status `id`, never the row's email, because `sr claude addr user@corp.example` for profile `chip-5` would sign in a new profile and leave `chip-5` dead. Hosted cmux storage keeps `sr add claude`, its own add verb. The pinned-account 403 names the same verbs with the account id. For a non-loopback socket peer it adds `on the server that holds it (host <hostname>)`, because `sr claude proxy --account` with a server selected launches straight at the box.

§36.3 §30 measured that Claude Code 2.1.284 honours `ANTHROPIC_BASE_URL` from settings.json but ignores `ANTHROPIC_UNIX_SOCKET` there. The socket recipe therefore sent conversations to api.anthropic.com:80 in cleartext, outside the pool (measured 2026-09-28). `sr doctor` (`claudeSettingsDoctorCheck`) now reads the settings.json that setup writes. It prints `FAIL  Claude settings` and exits non-zero for an unsafe plain-http base URL or a non-empty `ANTHROPIC_UNIX_SOCKET`. A clean or missing file prints `ok` with its path. An unreadable one warns and never passes silently. URLs are redacted (`redactedServerURL`), because doctor output gets pasted into chats. The FAIL names the hand fix first (set `ANTHROPIC_BASE_URL` to the daemon's root, delete `ANTHROPIC_UNIX_SOCKET`), because setup blocks its Claude Code item under local storage on a machine with no Claude profile (`clientConfigBlockedReason`), as on a laptop whose OAuth profiles all moved to the box. `sr setup` lists a non-empty socket key as a pending change, and its one settings writer deletes the key in the same write, so one `sr setup` leaves a file the doctor passes. Doctor, detector and writer share one predicate, `claudeSettingsUnixSocket`.

§36.4 Owner decision, 2026-09-29: failing every non-loopback plain-http host was too strict. The doctor now applies sr's per-launch transport rule (`judgeClaudeBaseURLTransport`). HTTPS, loopback and the local daemon's own URL pass. Plain HTTP otherwise passes only to a Tailscale node verified online, where WireGuard encrypts the hop. sr's settings writer still never persists a plaintext tailnet URL, since plain `claude` cannot check it again, so the `ok` line says it holds only as of this run. The URL is read the WHATWG way Claude Code reads it (`claudeCodeBaseURL`), because loose spellings such as `http:api.anthropic.com` reach api.anthropic.com and §36.3 passed them. Plain http to `api.anthropic.com` FAILs before DNS or Tailscale is asked. A name that only resolves to loopback FAILs and is never dialed, because Claude Code resolves it again per request. A short or foreign name for a node's address (`http://box:31415` is the common one) FAILs and names the MagicDNS name to use. These also FAIL: a user name, password or `#fragment` in the URL; a MagicDNS name that does not resolve while Tailscale is up (an earlier draft warned, but the ruling said "any other host"); a non-MagicDNS name resolving into Tailscale's range while Tailscale cannot be checked; and a non-empty `ANTHROPIC_UNIX_SOCKET` beside any base URL, a verified one included. Each FAIL names its fix. Three cases warn, and a warn never fails the doctor. (1) A tailnet address or a resolving MagicDNS name while Tailscale cannot be checked. This can leak, because 100.64/10 routes in cleartext with Tailscale stopped, and the line says so. (2) A MagicDNS name that does not resolve while Tailscale cannot be checked. (3) A node Tailscale shows offline, as during a box reboot. Owner decision needed: case 3 goes beyond the ruling, which named only Tailscale stopped or its CLI missing. It warns because nothing leaks and a FAIL during a reboot would be false.

Residual risks. The doctor reads only that one file, so an rc file exporting `ANTHROPIC_BASE_URL=http://api.anthropic.com` leaks unflagged. MagicDNS is matched by suffix, so a split-DNS or Headscale domain FAILs instead of warning while Tailscale cannot be checked. The daemon-URL exemption is keyed on `SUBROUTER_LOCAL_BASE_URL` in the doctor's own shell, so a non-loopback value there (no install path sets one) also exempts that URL. OAuth grants orphaned on 2026-09-27 must be revoked by hand.

- Knobs: `CLAUDE_CONFIG_DIR` (picks the settings.json that doctor and setup use), `SUBROUTER_LOCAL_BASE_URL` (the local daemon's URL, exempt from the check), `SUBROUTER_TAILSCALE_BIN` (the Tailscale CLI the doctor runs)
- Code: `sr_claude.go` (`existingClaudeProfile`), `sr.go` (`providerReaddCommand`), `setup_plan.go` (`claudeSettingsDoctorCheck`, `judgeClaudeBaseURLTransport`)
- Related: builds on §30 (the socket hazard), §22 (`sr claude addr` re-auths in place) and §29.2 (the pinned 403). §36.4 supersedes §36.3's tailnet rule.

## 37. Reinstalls keep hand-added keys; box socket survives reboot; server install refuses upstream

Three installer defects from the 2026-09-28 audit, each confirmed against the code before it was fixed.

§37.1 `install-daemon` rendered the LaunchAgent plist from `launchAgentTemplate` (`HOME`, `PATH`, the admin-token pair) and overwrote the old one, deleting every hand-added key. The laptop's plist held seven (measured 2026-09-28): `SUBROUTER_ALLOW_LOOPBACK_PROXY`, `SUBROUTER_CLAUDE_SOCKET`, `SUBROUTER_IDENTITY_ACCOUNT`, `SUBROUTER_REMOTE_POOL`, `SUBROUTER_REMOTE_POOL_FINGERPRINT`, `SUBROUTER_REMOTE_POOL_TOKEN`, `SUBROUTER_RESERVED_ACCOUNTS`. A macOS `sr setup` re-run, the quickstart script and upstream's upgrade recipe all run it. The daemon came back healthy but without the box fallthrough (§16), the Claude socket (§30), the reservations or the identity account, and a daemon that enforces a proxy token 401ed its own loopback clients. `install-systemd` rewrote `/etc/default/subrouter` the same way, losing a hand-set `CODEX_HOME` or `SUBROUTER_PROXY_TOKEN_FILE`.

Now both installers carry every entry they do not write, verbatim, and a second run writes the same bytes. `HOME` and `PATH` are always the template's. An unparsable plist (realistically a binary one) stops the install before the binary is copied, naming `plutil -convert xml1`. The plist is written atomically, because `os.WriteFile` never changes an existing file's mode. It is 0600 with an inline admin token or a kept `*_TOKEN` key, and never wider than before. When the cloud config holds a proxy secret, the plist's proxy-token pair is dropped, because `serve` refuses a different token and would crash-loop under KeepAlive. A kept key that would stop `serve` refuses the install (`launchAgentKeptEnvironmentRefusal`): an unreadable `*_TOKEN_FILE`, `SUBROUTER_TRANSCRIPT_AZURE_URL` without `--transcripts`, or a truthy `SUBROUTER_TAILSCALE_AUTH` without an allowlist flag. In `install-systemd`, a token the render writes non-empty replaces its `_FILE` twin, because `serve` refuses both. An environment file it cannot read fails the install before anything is written, and that file's mode follows the same two rules (0600 for a kept `*_TOKEN`, never wider).

§37.2 The box's bootstrap script binds `subrouter.socket` to the box's tailnet address (`ListenStream=100.x.y.z:31415`), which does not exist yet when socket units start. After a reboot the bind failed with `EADDRNOTAVAIL`, systemd never retried a failed socket, and the box stayed down. `systemdSocketTemplate` now sets `FreeBind=true`. `After=tailscaled.service` was rejected: it is an ordering cycle with `sockets.target`, and `tailscaled` running does not mean the address is up. The box's deploy script never re-renders units, so the live box needs a bootstrap re-run or a drop-in. No test boots a box. Confirm it on the next planned reboot.

§37.3 `sr server install` piped upstream's `install.sh`, which replaced this fork's binary with upstream's release while the host reported healthy. The fork publishes no release of its own. The command now refuses without `--allow-upstream-release`, before it looks up the server or mints a token. The message says this fork deploys a Linux box from a source build and has no remote macOS deploy. Usage, `sr help` and the README omit the flag, so a copied command does not carry it. `sr server sync` and `diff` now tell the user to upgrade an old server from source. Upstream's `deploy/gcp/publish-subrouter.sh` installs upstream's release by design, so it passes the flag. Removed by §107, when the fork began to publish its own releases.

Residual risks. `install-daemon` still re-renders `ProgramArguments` and the working directory; upgrades go through `deploy-laptop.sh`, which touches neither. A pre-§37 binary (the upstream rollback recipe runs one) still drops the keys. The box's bootstrap script still replaces the box's `*_TOKEN_FILE` keys with inline tokens on every run, so the box's tokens stay inline. `install-systemd` does not check a kept `*_TOKEN_FILE`, because it runs as root, and a check that passes as root would not show that the service user cannot read the file.

- Knobs: `--allow-upstream-release` (off; `sr server install` refuses), `--admin-token` / `--admin-token-file` (either given: the flags own the admin pair; neither: the plist's pair is kept), `--proxy-token` (replaces a hand-set `SUBROUTER_PROXY_TOKEN_FILE`), `--dry-run` (names kept keys, never their values)
- Code: `cmd/subrouter/install_daemon.go`, `cmd/subrouter/install_systemd.go`, `cmd/subrouter/sr_server.go`
- Related: builds on §29.7 (`install-systemd` backfills its flags from the file).

## 38. A restart drains the public listener and in-flight websocket turns, and the box can be drained

On SIGTERM `serve` drains everything under one deadline, `--shutdown-timeout` (10 min, the same as
the unit's `TimeoutStopSec`). The private and public servers shut down together. Then serve refuses
new websocket turns and waits for those in flight. Before, `shutdownOnSignal` shut only the private
server, and `defer publicServer.Close()` cut every public stream on :31416. On 09-28 the old process
exited 9 ms after SIGTERM (§25 deploy note). `http.Server.Shutdown` does not wait for hijacked
connections, and Codex runs its turns over a websocket.

Serve waits on turns, not sockets: an idle Codex socket stays open between turns, so waiting for
`ActiveProxyRequests()` to reach 0 would hold every restart to the full 10 minutes.
`Lifecycle.websocketTurns` counts a `response.create` until its finishing event is delivered, and
`releaseTurns` frees a dead socket's turns. With the 256 MiB `webSocketInspectBudget` spent,
`webSocketEventType` reads `type` from the first 4 KiB. `Lifecycle.Stop()` (`Drain` plus a terminal
`stopping` flag) runs only once both listeners drain. Until then established sockets keep starting
turns, because a refused client would reconnect to a closed listener. After `Stop` a new turn gets
close code 1012 (`closeWebSocketWithServiceRestart`), and Codex reconnects and replays it. Status
answers add `active_websocket_turns`. Retirement (SIGUSR1) also turns keep-alive off publicly.

The box could never be drained: `handleDrain` refused non-loopback callers, and the box binds only
its tailnet address. `drainControlAuthorized` now admits loopback, the admin token, or a tailnet
identity `--tailscale-auth` admits, for drain and resume only. The admin token already deletes and
imports accounts over the same tailnet. A loopback listener was rejected: it hands drain to every
local process and needs a second bind. The gate is narrower than `authorizeAdmin`: no tokenless
fallback, no import or proxy token, quiesce stays loopback-only, and the public listener does not
mount it. A remote resume calls `ResumeDrain` (403 while quiesced). Every drain and resume logs
`subrouter lifecycle control` with the caller, since a forgotten drain looks like a spent pool. This
departs from upstream's `docs/tailscale.md`. During a drain the box's :31415 queues new connections
in the systemd socket, :31416 refuses them, and the laptop refuses all until exit, bounded by
launchd's `ExitTimeOut`. Do not drain before a restart: it drains by itself. A manual drain lasts
until resume or the next restart. The box deploy script's rollback ran a bare `systemctl restart`, which
against a §38 build waits out the abandoned build's whole drain (up to `TimeoutStopSec=10min` with
:31416 closed). Step 4 and the rollback now share `restart_bounded`. The bootstrap script's restart is
still bare. The restart that installs this build is the old binary's shutdown and still cuts.
Open: under `supervise`, `--worker-stop-grace` (30 s) bounds the public drain. Blue/green
(`supervise` plus SO_REUSEPORT) removes the gap. The laptop's side of a forwarded socket
(`serveWebSocketToPeer`) and non-Codex websockets track no turns and close at exit. An uninspected
message whose `type` is not in its first 4 KiB is not recognised, which holds a shutdown to its
deadline at most. A pipelined second `response.create` would make the 1012 cut the first turn
(Codex does not pipeline).

§38.1 Owner decision 7 (2026-09-29): tailnet drain keeps the admin token or an admitted identity; an
SSH operator on the box must drain, resume and read drain-status with no token; the box's deploy script
forces a draining old process after a wall-clock grace, not 25 health probes, and drains first.
`serve` serves `<state dir>/drain.sock` (0600; box `/var/lib/subrouter/drain.sock`) with only
drain, resume and drain-status, and 404 for the rest. On the box run `sudo subrouter drain`. It is
least privilege: only root and the service user can open it, and both can already stop or signal
the daemon and read the admin token. `DrainSocketHandler` is its own mux and never on TCP. Peer
credentials (`SO_PEERCRED`, `LOCAL_PEERCRED`) are checked, and a uid other than 0 or the daemon's
own is closed and logged. It opens through `openPrivateServedSocket` (§30's rules).
`SUBROUTER_DRAIN_SOCKET` moves or disables it. It is an env var, not a flag, because an old binary
exits on an unknown flag. An open failure is logged and serve goes on. Supervised workers and
Windows serve none. `subrouter drain|resume|drain-status [--socket PATH]` (also under `sr`) looks
for the socket at `--socket`, then `SUBROUTER_DRAIN_SOCKET`, then `$SUBROUTER_STATE_DIR/drain.sock`,
then the first that exists of `~/.subrouter/drain.sock`, the service's from `/etc/default/subrouter`,
and `/var/lib/subrouter/drain.sock`. The CLI never prints the socket's answer raw (unprintable runes
escaped), and refuses a path that is not a socket, symlink included. SIGTERM calls
`Lifecycle.BeginShutdown`, which sets the drain and `shutting_down` in one step under the gate. Every resume through any door then answers 409. Review
found a two-step version let a resume undo SIGTERM's drain (red 5 of 5). The deploy grace defaults
to the drain ceiling: the shorter of `--shutdown-timeout` and `TimeoutStopSec`, capped at 900 s, so
600 s on the box. A larger `DRAIN_GRACE_SECONDS` is clamped to the ceiling. A rollback gets 60 s
because it abandons a build that failed its checks. ATTEST_FIRST keeps its 30 s stop and says the
grace does not apply. The pre-drain is off when the service's `SUBROUTER_DRAIN_SOCKET` is off, 0,
false, no or none (any case) or a relative path. A value that is not a whole number stops the
deploy before the backup. Review found `DRAIN_REPORT_SECONDS=08`
left the box drained and printed DEPLOY OK, so values are read as base 10. The pre-drain does not
shorten the restart. An operator's drain is left alone, and a script drain that survives a failed
restart is resumed. A reviewer asked for a 90-120 s default. Owner decision 7 keeps the ceiling. For
the first deploy of this build, run with `DRAIN_GRACE_SECONDS=60` if a 10-minute gap on :31416 is
not acceptable. Open: a restart that fails without stopping the old process is not detected. Steps
5-9 verify the old process and print DEPLOY OK.

- Knobs: `--shutdown-timeout` (10 min), `--worker-stop-grace` (30 s), `SUBROUTER_DRAIN_SOCKET` (`<state dir>/drain.sock`; `off` serves none), `DRAIN_GRACE_SECONDS` (drain ceiling, capped at 900 s), `ROLLBACK_DRAIN_GRACE_SECONDS` (60 s, or step 4's grace if shorter), `DRAIN_REPORT_SECONDS` (30)
- Code: `cmd/subrouter/main.go` (`shutdownOnSignal`, `serveUntilSignal`), `proxy.go` (`handleDrain`), the box's deploy script (`restart_bounded`)
- Related: §38.1 settles §38's open grace question. Builds on §30's socket rules.

## 39. The daemon log is capped by the daemon, and a kept turn no longer writes an Info line

On 2026-09-28 the laptop's `~/Library/Logs/subrouter.err.log` was 930 MB, unrotated since 08-03, on a volume
98% full (20 GiB free). Nothing rotated it (a fixed plist stderr path, no `/etc/newsyslog.d` entry, no rotation
in `deploy-laptop.sh`, and newsyslog needs root for a user agent). It averaged about 16 MB a day since 08-03,
but 17 to 108 MB a day after the §31 build on 09-24, and about 95 MB a day at the end. In its last 40 MB, 55% of
bytes were `remote pool decision` lines, and 99% of those (51,828 of 52,335) were turns kept on their account
with nothing ranked, a line §31 widened to about 800 bytes. 87 of the 96 Error lines were `proxy request failed`
or `replayable upstream request exhausted` with `error="context canceled"`: a client that hung up first, mostly
Codex abandoning its `/api/hello`, `/wham/accounts/check` and `/models` probes. Nothing on our side failed. The
level was fixed at Info.

A kept turn's decision line (warm, or cold and retained by its provider's rule) is now Debug, and is not built
when its level is off. Every line a ranking produced stays Info with its keys. `selection did not rank this
turn` stays Info too: it is the only trace of a failed ranking (`proxy.go`'s three `invalidate()` calls drop the
error). A client that hung up first is Warn, not Error, on both failure lines, by `streamCancelAttribution`'s
rule (a cancellation AND the client's context is done). `canceled_by=proxy` and every other failure stay Error.
`SUBROUTER_LOG_LEVEL` is read by `serve`, `supervise` and `front` and sets the default handler's level with
`slog.SetLogLoggerLevel`, so the format is unchanged and every `slog` call obeys it. A malformed value runs at
Info and logs `SUBROUTER_LOG_LEVEL is malformed; logging at info`. It never refuses to start, because under
launchd KeepAlive a refusal is a restart loop.

The daemon caps its own stderr file at startup and every minute. Over `SUBROUTER_LOG_MAX_MB` it copies the file
to `<file>.1`, shifts older copies up, drops the oldest, truncates in place and logs `daemon log rotated`. At
rest that is at most (keep + 1) x cap plus a minute's overshoot (256 MiB at the defaults), about six days at
43 MB a day. During a rotation the staging copy `<file>.rotating` adds one more cap for a few seconds. It
copy-truncates, not renames, because supervised workers inherit the supervisor's stderr (`supervisor.go`) and
after a rename would write into `.1`, then an unlinked file that fills the disk unseen. Only an O_APPEND regular
file is capped (launchd opens `StandardErrorPath` O_APPEND, `R,W,AP` in `lsof +fg`, and so does a shell's
`2>>`). A non-O_APPEND file would get a hole, so it is left uncapped and startup logs `daemon log is not capped`
at Error. A socket, pipe or terminal is skipped at Debug. On the box stderr is the journal's stream socket and
journald rotates it, so there nothing but the two level changes is different. The path comes from the descriptor
(F_GETPATH, `/proc/self/fd`) with device and inode checked, so an `mv` is followed and a new file at the old
name is untouched. A log deleted while open (nlink 0) is emptied in place once over the cap and logs `daemon log
was deleted while open; emptied it`. The copy must leave `daemonLogFreeFloor` (1 GiB) free per `Fstatfs`, else
only the newest cap's worth is kept from a line start (Warn, `dropped_bytes`), else nothing is copied and the
file is left whole. A stale staging copy is removed before the space is measured. Without that, a volume with
less free space than the file was filled to ENOSPC by every minute's retry, failing the daemon's own state
writes. A failure skips 1, 2, 4 .. 64 intervals (tries 2, 3, 5 .. 65 minutes apart; a success resets it) and
logs `daemon log rotation failed` at Error with `retry_in`, once per distinct error, so it survives
`SUBROUTER_LOG_LEVEL=error`. A non-waiting `flock`, with the size re-read under it, keeps it to one rotation
across processes. A failed copy removes its partial staging file and leaves the live file whole. A failed
catch-up copy also removes `.1`, a duplicate prefix that would push older history out.

Operator notes: the first start of a §39 build rotates at once (the 930 MB lands in `.1` whole if the disk has
room for it plus 1 GiB, else its newest 64 MiB), and it ages out after three more rotations (about five days).
Capture `subrouter.err.log*` for any post-deploy check first, then delete `.1` by hand to reclaim the space.
Never `rm` the live file: the daemon keeps writing to the deleted inode until it restarts. Knobs go in the
plist's `EnvironmentVariables` plus a `RELOAD=1` deploy (a pre-§37 `sr setup` dropped them, §30). On the box
only `SUBROUTER_LOG_LEVEL` matters, in a systemd drop-in (`systemctl edit subrouter`), because a pre-§37
`install-systemd` rewrites `/etc/default/subrouter` from `systemdDefaults` only. A daemon killed mid-rotation
leaves `subrouter.err.log.rotating`. The next §39 rotation removes it; after a rollback, delete it by hand.
`tail -F` and `sr daemon logs` follow a truncate. For history, grep `subrouter.err.log*`.

Left open: `account state generation reload failed error="context canceled"` stays Error (its path needs its
own look and test). `proxy request` (37% of bytes) and `serving from remote pool` (7%) are the access log and
unchanged. Copies are not compressed. A line written between the catch-up copy and the truncate is lost. With
less than the file plus 1 GiB free, a rotation drops all but the newest cap's worth for good, because keeping
it whole would leave the file growing on a disk already short of space. The `invalidate()` calls still drop
the ranking error, so the decision line says a ranking failed, not why.

- Knobs: `SUBROUTER_LOG_LEVEL` (`debug`/`info`/`warn`/`error` or slog `+N/-N`; default `info`), `SUBROUTER_LOG_MAX_MB` (default 64 MiB; 0 turns the cap off), `SUBROUTER_LOG_KEEP` (default 3, at most 20).
- Code: `remote_pool_decide.go` (`crossPoolDecisionLevel`), `proxy.go` (`requestFailureLevel`), `daemon_log.go` (`configureDaemonLog`), `daemon_log_cap_unix.go` (`stderrLogCap`).

## 40. Four ops views and scripts that answered confidently and wrongly

Each gave an operator a plausible false answer without failing loudly. Routing is unchanged: two read what the
daemon publishes, two are the laptop's install and deploy scripts.

§40.1 `srq pool` counted a row "with headroom" when `used_percent < 100` (the binding 5h/7d meter). On 2026-09-16
it said nearly every local Claude account had headroom while every one 429'd. The binding meter was
`anthropic-ratelimit-unified-7d_oi` (the Fable week): `srq` had no column for it, and `account_windows()` would
have filtered it out by its `Feature: claude-fable` stamp. Headroom also ignores a holdout, the endgame cap, a
reserve and the burn projection, which the router applies. Claude rows now show `fable=` (matched like
`isClaudeOAuthAppsWeeklyWindow`), the header counts Fable from `normalized.pools.claudefable` and prints `!! Fable
0/N: no account takes a new Fable session` at zero, and a row refused only for Fable reads `FABLE-REFUSED
(<reason>)`. The base count cannot catch that shape, since the base score drops per-model windows. Counts come
from the daemon's verdict (`normalized.new_session`, else `normalized.usable`, else none, never a guess). A peer
section reads `N/M take a new session from a peer (its verdict)`. Locally a refused row that is not evicted sits
in tier 2 and still serves when tier 0 is empty, so it reads `new=fallback (<reason>)` and the header `N/M
preferred for a new session (the daemon's verdict), K more only as fallback`. Only an evicted row (spent, holdout,
dead credential) reads `NO`. A verdict without `evicted` is never called a fallback, and `, K with no verdict`
counts rows with none. With no verdict on any row the header counts headroom and says `headroom is NOT
routability`.

§40.2 The board's box in-flight count came from `/_subrouter/drain-status`, which the public listener 404s for
every token, so a laptop peered over https published `box.active_proxy_requests: 0` however busy the box was,
and a failed read reused the last good body. `poolPanel.ActiveProxyRequests` is now `*int64` (`null`, `–` on the
page), set for the box only when the read is current (`AdminSnapshot.Reachable`) and decoded, and for local from
the lifecycle as before. `box.draining` and `box.started_at` keep their old defaults. Box rows still carry no
`rank`.

§40.3 Without `<store>/codex/remote.key` (the §17 gate) `serve` silently ignores every `SUBROUTER_REMOTE_POOL*`
variable, by design (naming the file would defeat the gate).

§40.4 Measured on 2026-09-28: 25 `~/bin/subrouter.bak-<stamp>` backups (599 MB, 19 pre-isolation), though only
the same run's backup is a rollback target. A hand-rolled swap script
defaulted to the pre-isolation `subrouter.fable51` with none of §23's checks. `srq` pointed at a
checkout detached seven merges behind main. `deploy-laptop.sh` gains step 7 after the routes
verify (`INCOMPLETE` included, never on a rollback, never under `--check`). The step-6 verdict is held in
`ISOLATION_INCOMPLETE` so step 7 runs before either `INCOMPLETE` exit. `prune_backups` keeps the newest
`KEEP_BACKUPS` of `bak-YYYYMMDD-…` by the name's stamp (not mtime, which `cp -p` keeps from the old binary),
keeps this run's, and never touches other names. `name_pre_isolation_binaries` names leftovers once
`migration_hazard` holds (a migrated record or an unreadable store; before that, this run's backup is the
rollback). `refresh_ops_tools` diffs each `srq` link against the tree the deployed binary was built
in, not the script's tree, which run from the tools checkout would call a stale checkout current. A binary
outside any checkout falls back to the script's tree, and the report says so. It detaches a stale checkout only
when the deployed tree is in git and the checkout is not the running script's, is detached and clean, holds the
commit, the commit is on `origin/main`, and its HEAD is an ancestor. Each refusal prints its fix. A copy not in
git gets `ln -sfn` for a file and `mv <dir> <dir>.old && ln -s` for a directory. `--check` runs
`refresh_ops_tools` report-only.
Open gaps (KNOWN-ISSUES.md, *Residual gaps §40 leaves open*): box rows without a rank on the board, the box's
other drain-status fields, the refresh leaving the tools stale after a deploy of unmerged work, leftovers other
than `bak-*` only named, and no Opus/Sonnet column in `srq`.

- Knobs: `KEEP_BACKUPS` (default 5; only a base-10 count from 1 to 999999 prunes, `0` or a word prunes nothing).
- Code: `bin/srq` (`summarize`), `deploy-laptop.sh` (`prune_backups`, `refresh_ops_tools`, `ops_move_blocker`).

## 41. Draining accounts share the roll window, and the taper's ceiling binds

A change from 2026-09-02 is carried onto §31, plus follow-ups. It fixes two defects in the 24 h endgame drain (§10). First, `compare` ordered two drain-eligible accounts on raw `WeeklyResetAt`, so the earlier roll took every pick until it hit its cap. On the 2026-09-02 pool, `main` (rolls in 14.1 h, forfeiting 0.336 of a week) took every new session up to its cap of 2, and `eight-alpha` (16.2 h, forfeiting 0.774) took none. Second, the cap of 4 could never bind. The allowance is `ceil(WeeklyHeadroom / claudeEndgameSessionStep)`, `WeeklyHeadroom` is at most 1, and the step was 0.25, so 4 came only above 75 % left. The excess spilled onto healthy accounts: 12 concurrent sessions put 7 on healthy accounts while the two draining ones forfeited 1.11 weeks. Measured accounts spent 1 to 15 % of their week in total, not the quarter the step implied.

Item 1: urgency (`1 / seconds to the roll`) goes through the session spread. An idle pool still hands the window to the earlier roll. Live sessions (a request in flight at pick time) discount the leader: one divides urgency by 1.15, two by 1.3, four by 1.6. So the next pick can go to a sibling rolling up to about 15 % later (14 h against 16 h), but never to one rolling most of a day later (1 h against 20 h). Urgency has no headroom term, because between two accounts about to forfeit, the earlier deadline cannot wait. Item 2: the step goes from 0.25 to 0.0625, giving 3, 2 and 1 sessions at or below 18.75 %, 12.5 % and 6.25 % spendable, and 4 above. It is read only inside the endgame window, so steady-state routing is unchanged. Item 3: a tie inside the burndown tier breaks on raw weekly pace and skips `rankingPace`, whose local 2x let a local account with two thirds of a peer's budget win every such tie, which §31 D2 rules out. A pair that ties on that too falls to short expiry, then local preference. Item 4: the allowance is sized on `WeeklyHeadroom - Reserve`, because at 0.0625 an account at 0.20 over the default 0.05 reserve got 4, all spending past the holdout until `inHoldout` evicted all four at once. The allowance is also 1 whenever the 5 h burn projection fails (`shortBurnSustainable`), because each extra session only brings the 5 h wall sooner: a 50 % account whose 5 h window projected 2.8 against 0.3 left got 4. Drain eligibility checks the 5 h floor (5 %), not `usableForNewSession`'s burn projection. Weekly-only accounts and accounts without 5 h timing pass the projection.

For a verdict peer, the spread reads the owner's live count, so one rule orders local and peer draining accounts. No wire change, and either deploy order is safe. A pre-§31 peer is ranked with a live count of 0, so it takes every new session while a local draining account is busy. Upgrade such a peer first. Falling back to raw urgency for a pair with a legacy side was rejected: it would make `compare` non-transitive when a local account, a verdict peer and a legacy peer all drain at once.

Residual risks. A draining account with 18.75 to 75 % spendable above its reserve now takes 4 sessions where it took 1 to 3. The upstream 429 and the forced switch (§31 P8) remain the backstop. The drain applies to Codex accounts too. A verdict peer's live count is only as fresh as its body (10 s), so in a burst its draining account takes every pick after the local leader's first until it refuses `at_cap`. `SessionSpreadWeight` (0.15) also sizes the endgame handoff, so retuning it for steady-state ranking moves where draining accounts alternate. `bin/srq pool` sorts by time-to-roll, which is the idle order only; under load, read `rank`/`reason` on the pool board.

- Code: `selectacct/explain.go` (`Score.endgameUrgency`), `selectacct/limits.go` (`claudeEndgameSessionStep`), `selectacct/scheduler.go` (`endgameAllowedSessions`)
- Related: amends §10 (taper formula, "earliest roll first") and §31 D2. Amended by §55 (a local, unreserved, non-weekly-only Claude account is sized on its last usage reading), §56 (a reset holder's early burndown joins this tier), §60 (no reserve in the last day before the weekly roll), §62 (no 5 h reading means `ShortHeadroom` 1).

## 42. Usage-based Codex plans are scored from `spend_control`

From a change of 2026-09-05, plus follow-ups. A seat on plan
`self_serve_business_usage_based` answers `GET /backend-api/wham/usage` with `rate_limit: null` and
`additional_rate_limits: null`. Its only quota is `spend_control.individual_limit` (in the test fixture,
limit `"25000"`, used `"24745.3"`, `used_percent` 99, `reset_after_seconds` 2191576, `reset_at` 1790812800).
The parser read only `rate_limit` and `additional_rate_limits`, so the seat had zero windows, which score
Headroom 1.0 with `Fresh=true`. It sat at 99% of its cap while `sr status` and `usage-status` said "100% left",
and on older builds won every new-session pick (§13's failure with the sign flipped, worse because it attracts
load). On the build before this fix a paced usable seat outranks it, but usage-status still published it `usable`, Headroom 1.0
and `new_session.eligible`. Once every rate-limited seat was under the 5% floor every new session went to it,
and at the cap a workspace-typed 429 held it out for only an hour (`codexWorkspaceExhaustionTTL`), then it was
picked again.

`spend_control` now becomes an account-wide `spend` window (`used_percent`, else `used/limit`; a nominal 30-day
`LimitWindowSeconds`; reset from `reset_after_seconds`, else `reset_at`), and `reached: true` adds a
`spend/reached` hard stop. It is parsed apart (`parseCodexSpendControl`) with lenient scalars (number or
numeric string; `reached` as bool or `"true"`/`"false"`; `source` unread). Strict types made an ISO `reset_at`,
a fractional `reset_after_seconds` or `reached: "false"` fail the whole `json.Unmarshal`, costing subscription
seats their `rate_limit` windows. `NaN`/`Inf` read as not reported, since one would truncate usage-status for
every account. On a seat with no included window, a `spend_control` that yields no window fails the fetch
(`unreadable spend_control on a seat with no rate_limit window …`), so the seat keeps its seed, not `Fresh`.
`individual_limit: null` is not unreadable: zero windows is then the truth. A seat reporting `primary` or
`secondary` ignores `spend_control`, whose cap governs only overage there (folding it in would score a seat at
20% included usage at Headroom 0.10 on a 90% cap). Both windows carry `Feature == ""`, because a model-tagged
`gpt-6-astra/spend` would make `Scheduler.ForModel` zero-score every Codex seat lacking that model pool,
starving every rate-limited seat for that model.

The cap gates but never paces. As first ported, a seat half through its cap 12 h before reset beat a Pro seat
with 60% of its week left and 5 days to go, and one with 80% of its cap left and 20 days to go beat a Pro seat
with 10% left and 3 days to go. That spends billed money while prepaid weekly budget expires. Now
`scoreFromUsageWindows` (proxy) and `scoreFromWindows` (`sr`) mark it `selectacct.LimitWindow.SpendCap`, and
`bindingWeekly` skips it. It keeps real Headroom (5% floor, exhaustion) and reads `WeeklyOnly`, with no pace,
drain or `burndown`. It ranks below every paced usable seat and takes new sessions when no paced seat can.
Preferring billed usage is a routing choice the owner can make later, not this build's default. It would be a
deliberate flip of the `SpendCap` skip in `bindingWeekly`, plus a decision on whether the 24 h drain should ever
spend money (`TestSpendCapGatesButNeverPacesOrDrains` pins today's order). `spend/reached` is cooked on its own
(`accounts.WeeklyCookedWindow`, shown `spend cap reached`). Before, with a null `individual_limit`, it read
usable in `sr status` and `--gto` while the scheduler scored Headroom 0. With the window and no guard,
`sr reset --gto` redeemed a credit on a spend-capped seat (`7d 100%/25d 8h -> 100%/25d 8h (credit redeemed)`).
It now skips it (`accounts.IsSpendWindow`), prints `Skipped <email>: cooked on its spend cap, which a rate-limit
reset credit does not lift.`, and its refusal counts the skipped seats. The board's `readFuse` ignores spend
caps (its 30 days are nominal, so a 31-day month's 1st read PRISTINE at 40% spent, and mid-month unspent billed
cap showed as Forfeit). `bin/srq` reads `spend/reached` as limit-reached, and `spend` as a Codex weekly-like
window when a daemon predates `normalized`. Not done: unmeasured seats route on their seed,
`individual_limit: null` with `reached: false` reads full (only one real response seen), a subscription seat's
cap stays invisible, and model pools are uncapped. Nothing changes until the box and the laptop run the new
binary.

- Knobs: `SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET` (asks `accounts.WeeklyLimitCooked`, which never sees the spend window), `SUBROUTER_RESERVED_ACCOUNTS` (holds back a weekly budget only, so does nothing on a spend-capped seat).
- Code: `internal/accounts` (`parseCodexSpendControl`), `selectacct` (`bindingWeekly`), `internal/proxy` (`scoreFromUsageWindows`, `readFuse`).

## 43. Named access tokens: one secret per caller and a role each

Owner decision, 2026-09-24 (§23 decision 2, with same-day follow-ups): named access tokens, each with a role chosen at mint (`membership` spends the pool and nothing else, or `admin`), an expiry, a revoked flag, and the caller's identity in the logs. Tailscale stays allowed but is never the default control path. Before this, there was one static proxy token, so revoking one client meant rotating it on every client, the laptop's peer link included. That spend token also read the public roster on `:31416` (every pooled account's email and spend). No log said which credential a request used, and `bin/claudemode` wrote the box's admin token into `~/.claude/settings.json`, so rotating the admin token would 401 every machine set up that way.

Named tokens are strictly additive: with no token file nothing changes but the logs. The store is `<state dir>/access-tokens.json` (the directory of `--sessions`), version 1, holding only each secret's SHA-256. Tokens are `srk_<64 hex>`, not `srt_`, because the MultiTenant router claims every `srt_` bearer and fails closed on an unknown one. `subrouter token mint <name> --role membership|admin [--expires 90d]`, `list` and `revoke` refuse a state dir serve never ran with (no `sessions.json` and no `.lock`): a forgotten `--sessions` on the box once landed in `~/.subrouter`, where mint gave a token that 401s and revoke reported success on a live token. `--role` has no default, `--expires` takes `90d`, `36h`, an RFC3339 time or a `YYYY-MM-DD` date, `list` marks `RENEW` within 14 days of expiry, and revoke keeps the entry. A name is unique among active tokens and made of `[a-z0-9._-]`; `loopback`, `open` and `static` are reserved because the log uses them. Writes take a flock on `access-tokens.json.lock`, then temp file, fsync, rename and dir sync, mode 0600. Run as root, the file (before the rename) and its lock are chowned to the state dir's owner, or the `subrouter` daemon user cannot read a root-minted file and `sudo -u subrouter … token revoke` fails. No HTTP route mints a named token, because an admin that could mint could plant tokens that outlive its revocation. A named admin can still create tenant keys (`POST /_subrouter/tenants`), which survive its revocation.

Only an `srk_` bearer touches the file (one stat, a re-parse on inode, size or mtime change), so revocation and expiry land on the next request with no restart. A file that does not parse, or has a version other than 1, refuses every named token; static tokens are unaffected, and the failure is not cached, so a fixed file works on the next request. A bad row (unknown role, bad name, bad hash) refuses only that token and can still be revoked by name. Failing the whole file would let a row from a newer build (a third role) 401 every named caller after a rollback. So the version is a major version: bump it only for a change an older build must not half-read, such as a field that narrows what a token may do. Startup warns on any active token within 14 days of expiry, and such a token logs `access token expires soon` at most once a day while used.

Gates: the static proxy token keeps the private proxy, the public proxy and the public roster. The static admin token keeps the private proxy and private admin. A membership token gets both proxies and a 403 at the public roster. A named admin token gets the private proxy and private admin, and nothing on `:31416`, so a secret that is admin privately is never usable across the public wire (the box deploy script's invariant). §43.1's opt-in changes this for admin routes only. `proxyCaller` checks static proxy, static admin, then named, and a named token never closes a gate that was open (no proxy token configured, or loopback). Every admin gate reaches tokens through `matchesConfiguredAdminToken`, which reads the bearer and `X-Subrouter-Admin-Token` through one parser (`presentedCredential`) and refuses named tokens on `publicSurface`. Those gates are `authorizeAdmin`, `requireAccountImportAuth`, the `DELETE /_subrouter/accounts` gate, the §29 Host waiver and tenant CRUD. Upstream's tokenless fallback (decision 8) also admits a named admin when only an import token is set.

Logs name the caller, never the secret: `caller=` on every `proxy request` line (token name, `static:proxy`, `static:admin`, `loopback` or `open`), `admin request by access token caller=<name>` on each named admin grant (`listener=public` on a public one, §43.1), hourly lines for static-token roster reads and admin use, `access token refused the account roster caller=` for a membership token at the roster, and `access token refused ... reason=revoked|expired|admin_token_on_public_listener` (once a minute per name, only when the request was refused; §43.1 adds `reason=admin_token_spends_privately_only` with the flag on). `servers.json` gains `proxyToken`, set with `sr server add <name> --proxy-token <token>`, kept on a re-add that does not name it, and never printed by `sr server list`. `bin/claudemode` writes it (falling back to `adminToken` with a warning) and first probes `HEAD <base>/`, refusing on 401, because an expired token otherwise 401'd every Claude Code launch while srdash still showed the server online. A transport error is not a refusal, so the write goes ahead.

§43.1 Owner decision, 2026-09-29: (a) a `peer` role granting exactly what a deciding daemon needs from a peer, and (b) admin on the public listener behind an opt-in, off by default. A peer token spends the pool, keeps its account selectors and sees `X-Subrouter-Account` and `X-Subrouter-Peer`, without which it could not serve or refuse the account the laptop ranked (§31). It reads `usage-status` only on `:31416` (`peerMayRead`), as the owner scoped it; privately that route stays `requireAdmin` and answers it 401. Mint it with `--role peer`. It is never admin, and `/_subrouter/accounts` answers it 403 publicly and 401 privately. Its reads log `account roster read caller=<name>` hourly. There is no file version bump: a pre-§43.1 build refuses only the `peer` row, so move the laptop back to the static proxy token before rolling back past §43.1. Once retired, that token is held only by the box (`SUBROUTER_PROXY_TOKEN` in `/etc/default/subrouter`). Unlike the static token it expires (90 days by default). Renewal is a new name, a swap, then a revoke, because one name cannot have two active tokens.

§43.1 `SUBROUTER_PUBLIC_ADMIN=1` / `--public-admin` (`Server.PublicAdmin`) is off by default and changes nothing when off. On, only routes marked `publicAdmin` in `routeTable` mount on `:31416`. That is an allowlist, so a new private route stays off until marked. Routes whose gate grants on network position, proves same-machine possession, answers unauthenticated, or relays a peer's roster never mount (session leases, the store handshake, `stream-stats`, `quiesce`, `reload-accounts`, `cutover-challenge`, the pool board, the dashboard, `peers/usage-status`), and neither does tenant CRUD. `publicAdminGranted` checks the flag, then the §29 browser checks, then an active admin-role named token. Nothing else counts: not the static admin or import token, not the tailnet identity, not loopback position (`loopbackPeer` is false there, so a relay on the box can resume a drain but never lift a quiesce). A named admin token never spends on `:31416`, flag or not. The public limiter runs before any gate and charges every presented credential (`ratePrincipals`). Startup logs the WARN `PUBLIC ADMIN IS ON (--public-admin / SUBROUTER_PUBLIC_ADMIN): ...`, which says the setting has no effect without `--public-addr`.

§43.1 Set it in `/etc/default/subrouter`, not as a flag: a rolled-back binary that predates the flag exits on it and restart-loops, but ignores an unknown variable. The box's deploy script refuses `SUBROUTER_EXTRA_ARGS` carrying the flag, because its own `rollback()` would hit that loop. An explicit flag beats the variable (`flags.Visit`), so `--public-admin=false` turns it off. Step 8 reads the new MainPID's journal for `PUBLIC ADMIN IS ON`. When on, `sessions`, `drain-status`, `account-status` and `account-import` must answer the static admin token 401, and `account-import` the static import token 401 (a 404 warns, anything else rolls back). Asked for in the env file or unit but not logged, step 8 warns and accepts 401 or 404. When off, every probed route must be 404. Since §43.2, `transcripts` must be 404 publicly in every mode, and a build that mounts it there rolls back. Rollout: mint the peer token with an explicit `--expires` and a calendar renewal, and swap the laptop, ignoring the deploy script's own `static:proxy` read from `127.0.0.1`. Then retire the static proxy token by rotating it to a value no client holds.

§43.1 residuals: a named admin token on the public wire is full admin, protected only by secrecy, expiry, revocation and the pin, and `curl -k` sends it to whoever answers. A lapsed peer token shows on the laptop only as `peer could not be measured`. Tiering the allowlist behind a second opt-in is the owner's call (KNOWN-ISSUES.md). The static admin token at `:31416` gets a flat 401 and no warning, because step 8 presents it there on every deploy. §43.1.1 The same-day review found: with the flag on, a named admin bearer beside a junk `X-Subrouter-Admin-Token` got 200 on `/_subrouter/accounts` through the public handler, skipping the browser checks and the log (hence the never-spends rule); import-gated routes let cross-site requests through; the peer read `usage-status` privately too; a fresh junk bearer reset the limiter (400 requests never limited); `flag || env` let the variable win; the box's deploy script accepted the flag form; and step 8 reading only the env file rolled back a good build set by a drop-in. No `rollback()` guard against peer rows was added, because the automatic rollback crosses §43.1 only on the first §43.1 deploy, before any peer token exists.

§43.1.2 The merge with §39, §40, §42 and §44 added no route or gate. The opt-in opens `drain-status` publicly to a named admin token only, which the peer link never holds, so the pool board's box in-flight count stays `–` over https. §43.1.3 After the merge with §38.1, a named admin resuming over `:31416` gets `ResumeDrain`: it never lifts a quiesce (403) and gets 409 once SIGTERM has begun. The drain socket is a Unix socket only, so the opt-in never reaches it.

§43.2 Owner, 2026-09-29: *"go ahead and turn it on"* (the box's `--public-admin`). Whether the box runs with it is what its journal says, not this record. Transcripts (every session's request and response bodies) are no longer marked, and answer 404 on `:31416` to every credential. The allowlist is 14 routes: `drain`, `drain-status`, `resume`, `accounts` (list and DELETE), `account-status`, `usage-status`, `claude-web-balance`, `qwen-console`, `account-import`, `rate-limit-reset`, `reset-credits`, `sessions`, `bedrock-cost`, `azure-codex-cost` (§46.9, 2026-09-30, added `claude-reset` as the 15th). Before this, `sr` never dialled `publicUrl` and its admin client did not pin, so with Tailscale off no admin command reached the box.

§43.2 `servers.json` holds `publicUrl` (`https://host:31416`), `fingerprint` (SHA-256 of the DER leaf, as `subrouter fingerprint` prints it; there is no second pin field) and the new `publicAdminToken`, a named admin `srk_` token. The static `adminToken` never crosses the internet, and `sr` refuses a `publicAdminToken` that is not `srk_` and 64 hex. `sr server set-public <name> [--url] [--fingerprint] [--token-file <path|->] [--clear]` takes the token from a file or pipe only, never prints it, refuses a terminal on `-`, and writes only a complete, valid trio (mode 0600). An omitted flag keeps what is there, and `--clear` removes the token only. A stored token is bound to its endpoint and pin: changing host, port or pin needs a new token or `--keep-token`, or a token minted on one box would go to a rebuilt box, a reassigned address, or an impersonator. `sr server add` drops the token when `publicUrl` or `fingerprint` changes. `sr server list` never prints it.

§43.2 Every `sr` admin request goes through `doServerAdmin`: the private URL first, and the public path once more only when the private URL could not be used (the transport policy refused it, as with Tailscale off, or no TCP connection was made, per `httptrace`). A TLS failure after connect is reported, never routed around. An answer (401, 403, 404, 5xx) never falls back, and neither does a connection made and then lost, since a reset sent twice spends two credits. It needs the whole trio and an allowlisted route (`proxy.PublicAdminPath`), and never applies to a tenant-scoped entry (`tenantKey` or `/t/<key>`), which would act on the box's root pool. An unreachable private URL is skipped for 30 s within one command (`serverAdminReach`), because `sr server status` runs several requests under one 3 s clock. The public request carries only `Authorization: Bearer <publicAdminToken>` and is pinned by `proxy.PinnedTLSConfig`, so a wrong certificate fails before any byte is sent. A pin mismatch reads as possible impersonation: take the pin on the box over the tailnet, then `set-public --fingerprint <pin> --keep-token`.

§43.2 A public 401 and the listener's own `404 page not found` are reported as refusals with a fix, and `sr codex reset` says "not redeemed" when nothing ran. Output names the path (`via public admin`). Routes never served publicly (tenant CRUD; `claude-reset` until §46.9) fail with `this needs the tailnet`. Two blockers were removed. A failed Tailscale heal (`node not found`) only warns for admin commands with a usable public path (`healAdminServer`). With a public path, the private dial is bounded at 2 s (`privateAdminDialBound`), because a VPN's kill switch black-holed the SYN and ate `sr status`'s whole 5 s. Without a public path nothing is bounded. `redactServerRequestError` also scrubs `publicAdminToken` and `proxyToken`.

§43.2 The box needs this build before the flag goes on, or transcripts are public. Turn it on in this order: deploy with the flag off; audit admin-role tokens, since every active one becomes public and nothing enforces the audit; mint the laptop's token and take the pin on the box; set the variable and restart through the box's deploy script with `<sha> -` (it reads the admin token from the env file, keeping it out of `ps`), turning it off at once on anything but `DEPLOY OK` because a rollback keeps the same env file; then `sr server set-public`, deleting the box copy only once that succeeded; verify with `SUBROUTER_TAILSCALE_BIN=/usr/bin/false sr server status home`. Record the enable, with its date, as a deploy note. Residuals: leaked, the laptop's token opens the allowlisted routes from anywhere. It expires (90 days by default); renew with a new name, `set-public`, then revoke the old one. It is full admin on the private `:31415` too (transcripts included) to anyone who reaches it, such as a node-share guest, and turning the flag off does not revoke it. It can redeem every Codex credit early through `anytime=true` (§46.8), logged as `WARN codex reset credit redeem anytime caller=<name> listener=...`; refusing that publicly is the owner's call. With the tailnet down, account import now sends credentials over the public listener, and `SR_STATUS_SERVERS` does not gate it; `sr server set-public home --clear` keeps it on the tailnet. `sr-plot`, `srdash` and `claudemode` do not use the public path.

§43.2.1 Review fixes, folded in above: the token bound to its endpoint; `--token-file` never echoed; the missed `/t/<key>` scope, through which `sr claude remove --server` could have deleted a same-named root account; the heal swallow narrowed to admin commands; a TCP connect counted instead of `GotConn`, so a private TLS failure no longer falls back; and the per-command reach memory.

§43.3 Every pipe into `grep -q` in the box's deploy and bootstrap scripts now reads to EOF (`grep PATTERN >/dev/null`). The live box deploy of 2026-10-01 18:11 printed `WARNING: SUBROUTER_PUBLIC_ADMIN asks for public admin, but pid 218402 did not log PUBLIC ADMIN IS ON`, then `DEPLOY OK`, though the daemon had logged it at 18:12:24. Under `set -uo pipefail`, `grep -q` exits on the match, `journalctl` dies of SIGPIPE (141), and the pipeline fails: `set -o pipefail; (echo M; seq 1 200000) | grep -q M` exits 141 every time. The same race in `ss -ltn | awk | grep -q` could skip the whole public-surface check under DEPLOY OK. A case in those scripts' test harness blocks any new one.

- Knobs: `SUBROUTER_PUBLIC_ADMIN=1` / `--public-admin` (default off; off mounts no admin route on `:31416`), `--role membership|admin|peer` (no default), `--expires` (default 90 days), `--keep-token` (keep `publicAdminToken` across a new pin or endpoint), `--sessions` (default `SUBROUTER_SESSIONS`, then serve's default)
- Code: `internal/accesstoken` (`Checker`), `internal/proxy/access_tokens.go` (`proxyCaller`), `cmd/subrouter/sr_server_public_admin.go` (`doServerAdmin`)
- Related: builds on §23 (owner decision 2). §46.9 (2026-09-30) supersedes §43.2 on `claude-reset`, now the 15th allowlisted route. §43.1.2 and §43.1.3 record the merges with §38.1, §39, §40, §42 and §44.

## 44. The second upstream harvest (upstream #368, a3745cc, #378, ddb8744)

Upstream `manaflow-ai/subrouter` is a source only, read at `c9303d3`. This is the batch §29 held
back for three sections since merged (§28, §27, §31), and re-verified after them. No owner
decision moves: the daemon never writes `~/.codex/auth.json` (§23, decision 1). The 70-minute warm
rule, holdout/reserve (§20), the Opus "no capacity" decline (§11), and P1-P10 with the one ranking
(§31) all stand. There is no retention floor and no gain gate.

- §44.1 #368 parts 2-4, ported (part 1 was §18). `writeRefreshedCredentialIfUnchanged` compares only the access and refresh token, not metadata, and writes the rotation under the current metadata. A changed pair still wins, as before. A same-login metadata rewrite during the exchange used to drop the rotated pair, leaving a spent refresh token and a permanent `invalid_grant`. No incident on our pool is attributed to it.
- §44.1 Codex: `writeCodexActiveAuth` renamed `auth.json` away before writing its replacement, so a Codex could find no login. It now copies to `.bak` and lands through `writeFileAtomic`. A backup that cannot be written fails the write with the old file in place.
- §44.1 lock: `SwitchActiveStored`, `WriteActiveCodexAuth` and `syncActiveCodexAuthIfAccountActive` take `lockActiveCodexAuthWrite` (mutex plus `flock` on `~/.codex/auth.json.write.lock`; Windows the mutex only), so a refresh of A cannot overwrite a concurrent switch to B. It is a leaf lock, and the daemon never takes it (`DisableActiveAuthSync`). Only the active account's sync takes it, because a root-owned lock file made every refresh of any account fail `EACCES`.
- §44.2 a3745cc (T7), narrowed: the Opus stages pinned the session before the Opus pick answered. Of 777 transport-stage substitutions up to 2026-09-19, 27 declined `status=429 rejected=true`, each leaving the session on the refusing account. The transport stage now picks with `oauthRetryCandidate` and commits by compare-and-swap (`commitSuccessfulHTTPResponse`) after a clean 2xx. The pool-selection stage passes a `pendingSessionCommit`. `oauthRetryAccount` keeps the Codex websocket quota reconnect, which belongs to the Codex transient-failover branch. Dormant while clients send `claude-opus-5-5`.
- §44.3 #378, narrowed: the request that found scores older than `--usage-score-ttl` (30 s) ran the sweep inline, up to `usageScoreRefreshTimeout` (60 s). `refreshUsageScoresForRequest` now takes the SchedulerRef claim and hands the sweep to a goroutine with `recover`. The request routes on the scores it found. Exhaustion, credential, unavailable and capacity marks are request-time overlays, so every refusal still applies. Only `SchedulerRef.Scored()` false sweeps inline. `Scored` is its own flag because every account reload zeroes the clock, and it is cleared only by `SetUpdatedAt(time.Time{})` (a redeemed reset credit, §29.5). The sweep runs through `Lifecycle.StartBackground`, and `shutdownOnSignal` calls `WaitBackground` on the same deadline, so SIGTERM no longer exits between a rotation and its save before that deadline. A sweep still in a token exchange when `--shutdown-timeout` runs out is cut. A draining process starts no sweep. Not taken: the idle refresher loop (the 10-minute auto-switch sweep and the 30 s sweep cache of §26 already keep an idle pool's scores at most minutes old, and a 30 s loop would refresh 30+ box accounts around the clock), `FetchUsageWindowsCached` single-flight (it lives in the sweep path the credential-timeout branch changes), and the log-level change (the log-volume branch owns log levels).
- §44.4 ddb8744, narrowed: an account whose carried zero (`Fresh=false`) outlived its lapsed mark stayed out of failover, about 2.5 minutes after the reset and unbounded while the usage endpoint throttles (calibration on the pre-§29 tree: `calls=1 freshCalls=0`). Upstream deletes our Evicted filter, which walks into the holdout and breaks the Opus decline. Ours probes last, in the HTTP usage failover only, after the gated reset redemption revived nothing and every give-up stage declined (Bedrock, API key, Opus, peer pool). `oauthRecoveryProbeCandidate` offers one untried Exhausted account that is not `InHoldout` or `BlockedUntilFor`, one attempt per account per lapse. A refused probe re-marks the account and goes to the client (`reason=recovery_probe_refused`). The first cut probed first, and Fable turns local Opus would serve went to the peer. The price: a Codex pool with the reset redemption armed (armed nowhere today, §29.5) spends a credit before a free probe.
- #401 not taken (live debits could drop a subscription account below an API-key account): latent (no API-key account in either pool).
- #377 not taken (each request body was decoded once per extractor): a CPU/memory optimisation (1,195 lines), not a defect.

Residual: a served probe does not heal the score, so the session's next turn gets `no non-exhausted
claude accounts available`. Healing needs the allowance, or a 2xx overlay, wired into selection (follow-up). A reload
during a sweep starts a second sweep. `runSRAutoSwitch` and the startup fetch are not waited for at
shutdown. §44.1's Claude hunk conflicts with §34: build
`merged`, then `return s.writeRotation(ctx, profile, instancePath, before, merged)`. No config
change. Deploy the box for §44.3/§44.4 and the laptop for everything. §44.1's Codex half is
CLI-only (`~/bin/sr`).

- Knobs: `--usage-score-ttl` (30 s), `--shutdown-timeout` (10 min)
- Code: `internal/agents/claude/store.go` (`writeRefreshedCredentialIfUnchanged`), `internal/proxy/proxy.go` (`refreshUsageScoresForRequest`, `oauthRecoveryProbeCandidate`), `internal/proxy/claude_opus_substitute.go`
- Related: builds on §29's harvest. §44.1 must merge with §34.1's `writeRotation`.

## 45. CI runs the Go suite; an exported store override or the wall clock no longer decides a test

Test and CI changes only; nothing ships in a binary. No CI ran subrouter's Go code. The code then lived in a
subdirectory, and its copy of upstream's `ci.yml` never ran, because GitHub
runs workflows only from the repo root. An exported `SUBROUTER_STATE_DIR`, `SUBROUTER_CLOUD_CONFIG`,
`CODEX_HOME` or `CLAUDE_CONFIG_DIR` beat the tests' `HOME` isolation (a Superset shell exports `CODEX_HOME`;
every `sr claude` session exports `CLAUDE_CONFIG_DIR`). With the first three at a read-only decoy, 159 tests
failed (147 `cmd/subrouter`, 11 `internal/accounts`, 1 `internal/proxy`). Two tests rewrote the decoy
`CLAUDE_CONFIG_DIR`'s `ANTHROPIC_AUTH_TOKEN`, which in a live `sr claude` session means 401s. Earlier flaky
Codex isolation tests were this bug, and one run opened a real browser `codex login`. The upstream #451 test
read the real Claude login and keychain. Three proxy tests (75 ms, 75 ms, 150 ms deadlines) read `0/0` calls
on a loaded host.

The workflow runs on PRs and `main` pushes touching the Go code, the deploy scripts or itself, and
on dispatch, on `ubuntu-latest` with Go 1.26.0, `contents: read` and no secrets: gofmt, build, vet, a darwin/arm64 vet (else
`*_darwin.go` never compiles), `go test -count=1 ./...` with `CGO_ENABLED=0` and the four overrides unset,
then the deploy scripts' harness. Each `main` push gets a concurrency group keyed on its commit, because
GitHub cancels a pending run when a newer one queues. TestMain in the three affected packages clears the four
overrides. The no-op `migrate-isolation` tests and the #451 test run with an empty `PATH`, so they cannot
open a browser or reach `security`. The three proxy tests run in `synctest.Test`. Residual:
CI is Linux only, and on 2026-09-28 14 tests failed on every macOS run (12 Unix-socket tests from §30, 2
dead-credential tests from §28). A new store-relocating variable must join `testenv.HomeOverrides`.

§45.1 (owner decision, 2026-09-29): no macOS GitHub job and no required check. The macOS suite, whose §30 and
§28 failures merged green on Linux, runs locally on request with `ops/bin/sr-local-ci` (bash 3.2).
It must be safe on the machine serving the pool. `go test` runs under `env -i` with an allowlist (64 variables
dropped on this laptop), a throwaway `HOME` with the real Go caches, `GOROOT/bin` first on `PATH`, and
`CGO_ENABLED=0` as CI. `TMPDIR` on macOS is `/private/tmp/srci.XXXXXX/t` (26 bytes): `/tmp` is a symlink, and
35 tests failed on `profile parent "/tmp" is a symbolic link`. On Linux it is `/tmp`, because a longer dir
pushed a socket past the 108-byte `sun_path`. A file-backed fake `security` replaces the about 1,240 real
`security` calls a bare run made. A `sandbox-exec` profile is required on macOS (exit 2 without it). It
refuses exec of `/usr/bin/security`, `/bin/launchctl`, `/usr/bin/sudo`, `/usr/bin/open` and `tailscale`,
connections to `localhost:31415` and to Unix sockets under the real home (`$HOME` and the `getpwuid` home),
writes there except Go caches, and the credential stores.
Each socket path gets its own rule, because `(remote unix-socket …)` honours only its first filter.

The suite is `go test -json -count=1 -timeout 40m ./...` plus the deploy harness. Each failed or unfinished
test re-runs alone, once, with its own `-timeout` (10 minutes by default). Real (exit 1): fails again alone, a
race report, an unlisted sandbox refusal, a build failure, a package cut short, a non-zero `go test` exit with
no failing package, a failure outside any test, a preflight failure. A test that passes alone is load/timing. A `known_refused` test (the two `sr claude add`
tests) failing on its refusal is "not verified on macOS". Exit 2 is a harness problem. A Linux host run stops
with exit 2 unless `--no-sandbox`. `--docker` is the fenced Linux run: a non-root user via `setpriv` (`su`'s
umask 002 breaks 19 tests), no seccomp filter (it blocks `pidfd_getfd`), and the suite under `--network none`
and `GOPROXY=off`. On a 2026-09-29 build the laptop run (load 106 to 160) had
3,624 tests: 0 real, 2 not verified, 1 load red. Residual: nothing makes anyone run it (a reviewer should ask
for the summary on a PR that touches darwin-only code), `sandbox-exec` is deprecated, in-process keychain
access is not fenced, and the two `sr claude add` tests need a fake `security` in their own `PATH` dir.

- Knobs: `--no-sandbox` (off; runs unfenced and skips the `known_refused` tests), `--strict` (off; load reds and known refusals also exit 1), `--docker` (fenced Linux run), `--selftest` (switches off each guard and requires its probe to go red)
- Code: `.github/workflows/ci.yml`, `internal/testenv` (`ClearHomeOverrides`), `ops/bin/sr-local-ci`
- Related: §61 renames `ClearHomeOverrides` to `IsolateHome`, which also points `HOME` at an empty directory.

## 46. Saved usage-limit reset: `1x reset` column, `sr claude reset`, `sr codex reset`

For the Claude Opus 5.5 launch, Anthropic gave every Pro and Max account one saved usage-limit reset (program `cedar_ember`). It must be used by 2026-10-22 16:00 UTC and clears `five_hour`, `seven_day` and `seven_day_overage_included`. A read-only survey on 2026-09-29 found local accounts eligible with one reset left. Subrouter could not see it (plain `/api/oauth/usage` answers `cedar_ember: null`) and no verb could spend one. Codex already had a `1x reset` column (from `rate_limit_reset_credits`) and the verb-first `sr reset`. Owner decision, 2026-09-29: put the column between `Fable wk` and `Extra usage` on every Claude table, and add `sr claude reset <account>` and `sr codex reset <account>`. Nothing claims a Claude reset on its own (unlike Codex's `SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET`).

§46.1: The sweep's `FetchUsage` asks `/api/oauth/usage?cedar_ember=1`, without `skip_spend=1` because the sweep reads `spend` from that answer. It sends Claude Code's `User-Agent` (`claude-cli/2.1.284 (external, cli)`, constant `ResetClientUserAgent`) instead of Go's default, because Anthropic grades eligibility by surface and CLI version (`ineligible_reason` `surface`, `cli_version`). This is a deliberate deviation for the owner to accept or reverse. Claude Code never sends that query combination, so when the answer lacks the block the daemon reads Claude Code's own `?cedar_ember=1&skip_spend=1` in the background, at most once an hour per account (`EnableClaudeResetFallback`). `ParseResetStatus` decodes as Claude Code 2.1.284 does: no boolean `eligible` makes the block malformed, a grant whose id fails `^[a-z0-9_-]{1,40}$` or that lacks a whole `resets_left` is dropped, and `use_requires_limit` defaults true. A drifted block never fails the usage read that routing depends on. Absent or `null` means "not read", never "none".

§46.2: The block rides the usage-windows cache onto the usage-status row as `complimentary_reset` (`source: "cedar_ember"`, plus Claude-only `expires_at`, `clears`, `ineligible_reason`). Codex rows stay byte-identical. Expired grants do not count. A credential whose scope list lacks `user:profile` (a setup token) publishes `status: "unreadable"` (`ineligible_reason: "no_profile_scope"`), but one with no scope list does not, since many stored OAuth logins never recorded scopes. Only a fresh read publishes a reset, so last-good windows show `?`, not an old `1 left`. Anthropic's `ineligible_reason: "unavailable"` is its own failed read and also shows `?`. Local, `servers.json` and peer-link (§32) sections all carry it. A local row whose own read carried no block takes the reset this machine's daemon publishes for the same profile: one loopback read of its usage-status (3 s budget), only when a row needs it, never a request to Anthropic.

§46.3: The usual table reads `Fable wk | 1x reset | Extra usage` (`usageGridResetCell`), with `Opus wk`/`Sonnet wk` kept with the weekly meters before it. Cells: `1 left 10/22` (use-by date), `used`, `expired`, `none`, `not elig`, `n/a` (token cannot read it), `?` (not read), and `1 left !elig`. It is fitted last (`insertClaudeResetColumn`), so the paid-usage columns keep their room: dated, else undated `1 left` (8 wide), else dropped. It is not hidden when empty because the owner listed `?` as a value to show. After 2026-10-22 a follow-up can drop it.

§46.4: The daemon claims through `POST /_subrouter/claude-reset` behind `requireAdmin`, on the private listener only (not `publicAdmin`, as the owner required; §46.9 reverses this). The daemon claims because it is the account's only refresher and a second process refreshing a Claude login rotates the refresh token out from under it. A body that does not say `"dry_run": false` is a preview, so nothing is spent by omission. A claim must repeat the account in `confirm_account` (400 otherwise). The token comes only from `RefreshCredentialDetailsIfExpired`, and a 401 or 403 is `auth_error`, never refreshed or retried. A setup token is refused before any request. The profile (`GET /api/oauth/profile`: organization UUID, email) is read on every call, never cached. It claims only Anthropic's `next_grant_id`, as Claude Code does, when that grant is `usable_now`, not paused, not expired and has `resets_left > 0`. The POST is copied from the Claude Code 2.1.284 binary: `POST https://api.anthropic.com/api/organizations/{org}/reset_rate_limits` with body `{"program":"cedar_ember","grant_id":…,"request_id":…}`, header `anthropic-beta: oauth-2025-04-20` and a 25 s timeout, sent once. The request_id is the caller's, else one an unconfirmed claim on the same grant minted within 10 minutes, else a new UUID. It is logged at WARN (`claude reset claim sending`) before the POST and lives in daemon memory only. Claude Code treats `unavailable` like `error`: both keep the request_id and "may have landed"; settled answers (`reset`, `already_used`, `not_limited`, `ineligible`) forget it. On `reset`, or `unavailable`/`error` with a non-empty `cleared`, the daemon clears exhaustion marks (`SchedulerRef.ClearExhausted`), drops the cached reads and re-reads. The first version also required `expected_organization` from a preview, refused early use, and serialized claims daemon-wide on a 90 s clock. §46.7 changed all three.

§46.5 and §46.6: `sr claude reset` takes one account and refuses `--all`, `--gto` and `--best`. It routes to `--server` when given, else to this machine's daemon if it lists the account and no other reachable daemon lists the same name, else to the one `servers.json` server that does. Names are unique only within one store, so a collision asks for `--server`, and a silent local daemon stops the command instead of falling through. A peer-link-only account (§32) is refused, because the peer link never carries an admin path. A `surface` refusal says the reset is still saved (claude.ai can claim it); `cli_version` names `SUBROUTER_CLAUDE_CODE_VERSION`. Only `reset` exits 0. `sr codex reset` redeems one named account through `/_subrouter/rate-limit-reset?email=…` and never sends an empty `email` (the route's smart pick). It used to launch Codex with "reset" as the prompt. `sr reset` is unchanged. `reset` is a reserved Claude profile name (`claudeManagementSubcommands`).

§46.7: Owner decision, 2026-09-29: *"it should just be one command, no confirmation. sr claude reset <acountname> or <accountemail> just does it, NQA"*, and the same for `sr codex reset <name|email>`. The §46.5 preview-then-claim flow read the usage endpoint twice back to back, and Anthropic rate-limits it per login, so the owner's claim failed with `429 ... rate_limit_error`. One direct `dry_run:false` call later landed: `result: "reset"`, `resets_left` 1 -> 0, 7d 100% -> 0%, `cooldown_until` about two minutes out. That first claim proved Anthropic accepts Claude Code's request from subrouter, though the endpoint is undocumented. The CLI now sends one request with no preview and no question (§46.11 adds a question when the reset would be partly wasted). The old `--confirm` and `--early` spellings are accepted and ignored, but a `--confirm` naming another account sends nothing. Only the profile name or account email names an account, whole and ignoring case; a near miss (edit distance 1-2) gets `did you mean "…"? nothing was sent`. The daemon claims from a cached block under 30 minutes old (`claudeResetCacheFresh`) when it offers a claimable `next_grant_id`, was read with the account's current token (a token hash is kept with it), and came from a read sent after the account's last claim POST (`noteClaudeResetClaimSent` stamps the account before every POST). A cached refusal is re-read live. A 429 on the status read, the profile read or the claim POST waits `Retry-After`, else 30 s, 60 s, 90 s, 90 s, within 4 minutes total (`claudeResetRateLimitBound`), leaving 45 s for the claim (`claudeResetClaimReserve`). A claim with less than 45 s of budget left is not sent. A dry run never waits. A rate-limited POST is resent with the same request_id. If the live read stays unreadable, a cached block under 24 hours old stands in. Early use is now claimed and reported as `early_use` (the owner's call), and `expected_organization` is optional (enforced only when given). The call runs on one 6-minute `ClaudeResetBudget`, claims serialize per account, and no claim is sent once the caller hangs up. The CLI waits 7 minutes (`claudeResetCLITimeout`) on its own client, because sr's 120 s runner client had cut claims off as "may have landed".

§46.8: Owner decision, 2026-09-29, asked whether Codex resets may be claimed anytime: *"sure"*. `POST /_subrouter/rate-limit-reset` takes `anytime=true`, honoured only beside `email=<id>`; with `all=true`, `best=true` or no email it is 400, so no sweep or smart pick spends a credit early. It skips the cooked rule and nothing else. Every other caller (`all`, `best`, `sr reset <email>`, `--gto`, the auto-reset) keeps `redeemAccountIfEligible`. Only `sr codex reset` sends it, by CLI convention rather than a check, so any admin credential can ask, including a public named admin token (§43.2). Each non-dry-run anytime redeem logs WARN `codex reset credit redeem anytime caller=<name> listener=public|private` and reports `early_use: true` when early. What ChatGPT does with a credit spent below the weekly limit is not observed live. Credits are finite and do not regenerate. A leaked named admin token can now spend every Codex credit from the internet, where before it could redeem only accounts at their limit. Refusing `anytime` on the public listener is left to the owner (it would make `sr codex reset` off the tailnet cooked-only again).

§46.9: Owner decision, 2026-09-30: *"make them work over public admin"*, because with Tailscale down a saved Claude reset on the box could not be claimed from the laptop. This reverses §46.4's private-only rule. `claude-reset` is now a `publicAdmin` route (`publicAdminPatterns`, 15 routes). With public admin off it is 404 on `:31416`. With it on, only a named admin-role token passes; static admin and import tokens, peer, membership, loopback, tailnet and no credential get 401. The CLI falls back to the pinned public admin path only when the private URL is unreachable (never after a connection was made), and a lost answer there is not resent. Step 8 of the box's deploy script probes the route on the public listener with the static admin token and with none. With public admin on both must be 401 (a 404 is a warning). Off, both must be 404. Anything else rolls back. A leaked named admin token can now spend every Claude reset the box holds. Mitigation is `subrouter token revoke <name>`.

§46.10: On 2026-10-01 the owner ran `sr reset claude seventeen-beta` and got `reset takes at most one account, got 2: claude seventeen-beta` (nothing spent). `normalizeProviderResetArgs` now folds `reset claude|anthropic …` into `claude reset …` before the credential-source routing, so `SUBROUTER_CODEX_SERVER` never sends it to a Codex route. `sr help` had listed neither reset verb since §46 and now does. Owner decision, 2026-10-01: the invented parts (refusing a Codex word, refusing a Claude word after a flag, a hint for names without `@`, `--dry-run` in help and errors) were rejected and removed in §46.11.

§46.11: Owner decision, 2026-10-01: `--dry-run` is for testing and never shown to users, follow the subrouter canon instead of inventing UI, and *"When you trigger a manual reset it should send a y/n dialogue if it has >10% headroom or <1d left. Otherwise just do it"*. The 10% line exists because a Claude account seldom sits at exactly 100% (*"it's a lot easier to run fully out of codex than claude on their system"*). The fold is now the §22 canon: `reset claude|anthropic` becomes `claude reset`, `reset codex|openai|chatgpt` becomes `codex reset`, args[1] only. `sr reset <email>` and bare `sr reset` stay the cooked-only Codex verb. Both `sr claude reset` and `sr codex reset` read the holder's row (as `usage-status` last served it, at most 30 seconds old) before sending anything. The claim itself still goes by the daemon's own read. `resetWasteReasons` asks when the week has more than 10% left (`providerResetAskHeadroom`), rolls on its own within 1 day (`providerResetWeeklyWaste`), or has no weekly meter read. Exactly 10% left, or a roll exactly a day out, does not ask. Claude's week is the `7d` window (the Fable week never stands in). Codex's is the long unscoped window, never a usage-based seat's 30-day `spend` cap (§42), which reads as unread and asks. Only y or yes proceeds, and without a terminal it refuses unless `--yes`/`-y`. `--dry-run` still works, but no help, usage or error names it; the `may have landed` lines point to `sr status` instead.

- Knobs: `SUBROUTER_CLAUDE_CODE_VERSION` (default `2.1.284`; a plain `x.y.z` replaces the `User-Agent` version on daemon restart), `SUBROUTER_PUBLIC_ADMIN=1` (off keeps `claude-reset` 404 on `:31416`), `--server <name>` (picks the holding daemon; `local` is this machine), `--yes`/`-y` (answers the waste question; required without a terminal), `--dry-run` (hidden; one preview request that never asks or waits).
- Code: `internal/agents/claude/reset.go` (`ParseResetStatus`), `internal/proxy/claude_reset.go` (`ClaudeResetPath`), `cmd/subrouter/sr_provider_reset.go`.
- Related: §56 builds on this (until claimed or expired, the router ranks the account at 2x and drains it early below 25 % of its week). §46.9 reverses §46.4's private-only rule and builds on §43.2's public admin listener. §46.11 supersedes parts of §46.10. §64 adds Fable resets to the cell (`2 Fable 3/9`, `1 +1F 12/10`) and the kind claimed to the claim's line.

## 47. Ended macOS login session: `sr status` reads the daemon, verbs that need the session and deploy scripts refuse

Superset's terminal daemon (`pty-daemon.js`), and any tmux or screen server, outlives a macOS logout. Every shell under it stays in the ended login's launchd bootstrap namespace. Measured 2026-09-30 (Superset 1.32.0, pty-daemon 0.3.5): the user lookup fails with `user: unknown userid 502`; `security` fails with `SecKeychainSearchCreateFromAttributes ... (-50)` and exits 44, like a missing item; every Go TLS handshake fails with `x509: OSStatus -26276` (12 of 12); `launchctl managername` exits 153 and `print`/`list` exit 141. The subrouter daemon runs in the current login and is unaffected. Before, `sr status` showed every row as an error, and verbs could stop half-way (traced from code): `sr claude move` could leave one refresh-token chain in two places, which kills the account at its next rotation. `deploy-laptop.sh` printed `DEPLOY OK` and `rollback-laptop-peer.sh` printed `ROLLED BACK, daemon healthy` while the build each replaced kept serving. From ssh (`managername` answers `Background`), bootout works and bootstrap is refused, so a healthy daemon becomes none (modelled, not observed).

§47.1 `probeLoginSession()` marks the session unusable only on darwin, only when `user.LookupId(<uid>)` returns `user.UnknownUserIdError`. That is the store's precondition before every Keychain call, so gate and failure cannot disagree (`LookupId` because `user.Current` caches). Any other error, and every other GOOS, is usable. `launchctl managername` (2 s timeout) runs only to word the message and never changes the verdict. If launchd answers, the session has not ended but something else (such as a sandbox denying mach lookups) stops the lookup. It is refused the same way, and the remedy is a normal terminal outside any sandbox. The host is named from `TMUX`, then `STY`, then `SUPERSET_TERMINAL_ID`. The daemon's pid is never printed, because `kill <pid>` ends every Superset terminal. The remedy is a new terminal app window, because logout quits every app. New Superset tabs stay in the ended session until Superset's Restart daemon. New tmux or screen windows join the old server, so under tmux or screen start a new one in that window (`tmux -L <name>`, or `screen` not `screen -r`). It must never grow a `$USER` fallback or file-based TLS roots: each hides a symptom without restoring the Keychain and removes the §47.5 net.

§47.2 An ALLOW list (`login_session_gate.go`) refuses every other verb before any credential, network or launchd step, so a verb added later is refused by default. It is checked in `runForProgram` and in `srRunner.run` after the §17 remote gate, because `cx` skips `runForProgram`. Allowed: help forms, `sr status`, bare `sr`, `sr <selector>`, `sr list`, `sr trace`, `sr remote` and `sr server` list/current, `sr daemon [status|logs]`, unnamed `sr server status`, `sr doctor` (first line `FAIL  login session`), `sr codex isolation-check`, `sr claude reset`/`sr codex reset` (`--dry-run` included; the holding daemon claims the reset), `subrouter help|probe|fingerprint`, and `subrouter accounts` under any name but `sr`. Refused: every launch, `sr claude list|status|env|run|rename|switch|use`, `sr usage`, `sr server status <name>`, and every write to credentials, the store, a plist or launchd. No https health probe is sent (`not checked (no TLS from this shell)`). The refusal exits 2 (a verb that ran and failed exits 1), prints without the `subrouter:` prefix, and names the host's remedy. On team or hosted credential storage it says `sr status` asks that storage instead of the daemon and points to `srq pool` (`statusAsksElsewhere`). There is no override variable.

§47.3 In such a session `sr status`, bare `sr` and `sr <selector>` read local rows from the daemon (`GET /_subrouter/usage-status` over loopback, no credential, 15 s) and never touch the Keychain. A local daemon address that is not loopback is an error. A usable session never asks the daemon. Claude rows are named by profile, and `active` is this machine's pointer, not the daemon's. Rows marked `sweep_cut` or `normalized.fresh=false` get a footnote (the daemon serves the last good reading for up to 15 minutes). `remoteServerUsageStatus` gains `SweepCut`, whose absence broke strict decode. Bare `sr` never prompts. A daemon that is down or too old gives one error, never a table of unknowns.

§47.4 `requireAquaLaunchd()` runs before every `gui/<uid>` launchctl call and requires `managername` to exit 0 and print `Aqua`. It covers a usable shell outside the GUI login (ssh). `install-daemon` checks it before writing anything when it will start the agent; `--dry-run` is not checked. §47.5 `security` output containing `SecKeychainSearchCreateFromAttributes` is no answer: a read returns `errSecurityReadNotRun` and a delete `ErrCredentialUnchecked`. Before, they counted as absent and as deleted.

§47.6 `launchd_blocker` in both laptop scripts refuses with exit 1 ("Nothing was changed." or "Nothing was restored.") unless `managername` prints exactly `Aqua` and `launchctl print $LABEL` exits 0 or 113. In `--check` it prints only a `NOTE:` and the exit code does not change. The fix it names is a new Terminal.app window, never a new Superset tab. A failed kickstart goes straight to the reload, and a failed bootstrap returns at once with no two-minute health wait. Restarts end the unload wait only on 113 and pass only on health from a NEW listener pid (`lsof`); `RELOAD=1` still passes, because its pid changes. `bin/sr-triage` prints a `session:` line and no longer reads 141 as `job NOT loaded`. `TestPatchesLedgerHeadsEachSectionOnce` checks each numbered section heads PATCHES.md once (the first §47 docs commit appended it twice).

§47.7 The suite from an ended shell fails the same three packages as main before it: 334 failing tests against the base's 336, none failing here that passes there. CI (Linux) has not run this branch. Not verified live: that `managername` prints exactly `Aqua` in a GUI terminal. If it prints anything else, every laptop deploy and Go launchd action refuses. Check it once from Terminal.app before the first deploy. Not done, by decision: pooled launches stay refused; team (cmux.com) `sr status`/`sr list` still fail with x509; `install_launchd.go` and the quickstart script's `--peer` have no preflight; the remote-key script still prints a raw bootout and bootstrap. Go 1.27's `SSL_CERT_FILE` would restore TLS, but not the Keychain or launchd. Out of scope: the Antigravity native-login journal (`HadOriginal`), whose failed Keychain read reads as "no original" and can later delete the real native login; and `ensureSwitchableForFreshUsage` failing open on a fetch error in a usable session.

- Code: `cmd/subrouter/login_session.go` (`probeLoginSession`), `login_session_gate.go`, `internal/agents/claude/store.go` (`errSecurityReadNotRun`)

## 48. `sr-triage` stops printing secrets and works on a stock Mac

§48.1 `sr-triage` printed `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` and `SUBROUTER_REMOTE_POOL_TOKEN` values in clear, because its sed matched `TOKEN|KEY` only after the `=`. Rc-file lines now print variable names only. Stock macOS has no `timeout` (probes showed `exit=127`), so a `bounded` helper polls every 0.1 s, sends TERM at the limit, KILL 2 s later, and returns 124. `alarm(2)` would not work: the Go runtime ignores SIGALRM. Triage finds `sr` at `~/bin/sr`, and the binary it describes at `~/bin/subrouter` or the LaunchAgent's, because macOS does not put `~/bin` on `PATH` and an installed daemon that was down read as never installed. Stopping triage KILLs a running probe (exit trap). Scratch files live in a `mktemp -d` directory, not fixed `/tmp` names a parallel run overwrites or a symlink can pre-plant.

- Code: `bin/sr-triage` (`bounded`)

## 49. A session a peer serves with no local assignment is listed, and `srq serving` names the peer

A machine with zero local accounts drawing on a peer (`SUBROUTER_REMOTE_POOL` armed by `remote.key`) had every turn served by the peer. Yet `srq serving` printed `unassigned (session <id>)` and exited 1. With an empty pool, `placeForSession` returns `ErrNoAccounts` before any `Sessions.Put`, and `sessionViews` listed only assignments, never the session's peer hold. §49.1 `sessionViews` now appends one row per live peer hold that has no assignment (`served_by: "remote"`, `served_peer`, `served_account`, empty `account_id`), naming the first peer in configuration order. An assigned session is never listed twice. §49.2 A session with no hold (served on a substituted account, hold lapsed after the 70-minute idle window, cleared by the breaker, or side requests only) still gets no row. Recording these would be a hold the router chose not to keep, or new state routing never reads. `srq` then says `not listed by the daemon` without claiming there is no hold, because `srq` can be ahead of the deployed daemon. §49.3 A tenant's listing gets no held-only rows (`tenantControlAuthorized`), because holds are shared and would leak the operator's session IDs and peer accounts. The cutover canary skips rows with no `account_id`, because DELETE drops only the local assignment and a peer-served canary turn would otherwise fail with `routed Claude session remained after cleanup`.

Residual: a held-only row cannot be DELETEd (404). During a drain, its next turn gets 503 `subrouter is draining`. Pending claims are listed, so during a first forward a session reads as held before the peer answers. A tenant's own assigned rows still read the shared holds, as before §49.

- Code: `internal/proxy/proxy.go` (`sessionViews`), `remote_pool_decide.go` (`placeForSession`), `ops/bin/srq` (`serving`)
- Related: builds on §31 (`served_by: remote`). §54 limits the breaker's clearing of holds to outages attributable to the peer.

## 50. A Fable turn that no local Opus account can take reaches the peer as the client sent it

When local Fable selection failed, §11's stage rewrote the request in place to Opus. When Opus selection failed too, the rewrite stayed. The peer chain (§16 Hook A) then ranked peers on the Fable pool but forwarded the Opus body, while held turns went as Fable. So the model changed under the user one turn in, and turn 2 re-read the prefix uncached. Reproduced before the fix: turn 1 reached the peer as `"model":"claude-opus-5"` (Content-Length 54), turn 2 as `claude-fable-5` (55). It hit every Fable session on a machine with no local Claude account, on its first turn and after each hold ended, and any machine whose local Fable is spent with no local Opus account able to take the turn. It is dormant while clients send Opus directly. `rewriteRequestToClaudeOpus` now returns an `undo` (body bytes, `ContentLength`, `GetBody`, `X-Subrouter-Model`), called when the Opus selection (or the Opus pick's refresh) fails, before the peer chain. A targeted peer account serves the Fable request it was ranked for. The untargeted last resort hands the peer the Fable request, and it runs its own chain, Bedrock and API-key stages included. Neither `deploy/macos/deploy-laptop.sh` nor the box's deploy scripts configure Bedrock, `SUBROUTER_FABLE_BEDROCK_PRIMARY` or `SUBROUTER_CLAUDE_FABLE_API_KEY`. A peer gets an Opus body only where a ranking on the Opus pool chose it (§31). A successful substitution is unchanged. Deploy: daemon change; the laptop and any remote-only machine need this build.

- Code: `internal/proxy/claude_opus_substitute.go` (`rewriteRequestToClaudeOpus`), `internal/proxy/proxy.go` (pool-selection Opus stage)
- Related: fixes the failure path of §11's Opus stage.

## 51. A Claude upload to a plaintext entry with no Tailscale node ID goes over the public admin path

`requireManagedClaudeServerIdentity` (upstream `f97faf23`, arrived with the §23 merge) refused every Claude move, push and add-copy to an http, non-loopback entry with no `tailscaleNodeID`. The owner's `home` is such an entry, with a complete §43.2 public admin trio (`publicUrl`, `fingerprint`, `publicAdminToken`). It is also the laptop's default, so `sr claude add` warned and suggested a push that failed the same way. Dropping the check alone would have sent the credential over plaintext, because `doServerAdmin` tries the private URL first.

§51.1 For such an entry with a usable trio, a Claude upload (preflight, credential POST, a move's usage-status check) goes only to the public admin path (`credentialUploadPublicOnly`). It never goes to the private URL, first or as a fallback, and prints a `note: sent <name> to home via public admin` line. §51.2 With no usable public path, the upload is refused before anything is sent and names both fixes (`--tailscale-node-id` or `sr server set-public`). A tenant-scoped entry is offered the pin only. §51.3 The copy after an add sends nothing to such an entry, keeps the profile, exits 0, and names the `sr claude push` to run (or, with no usable public path, the two fixes first). An add is not a request to send a credential over the internet; a push is. §51.4 A setup-token copy over the public path skips `writeClaudeProxyEnvForServer`, which refuses this entry because `sr claude run` has no node to verify. Pinned, https and loopback entries route as before. `requireManagedClaudeServerIdentity` is unchanged. A move still deletes the local copy only after usage-status reads the account `auth_valid`.

§51.5 Owner decision: Codex matches Claude where the entry has a public path, and nothing else changes. Codex logins (`sr server login`, `sr server sync`, `sr codex add`) go through `serverLoginOne`, which sent the new refresh-token chain over plain http to whichever tailnet node owned the address. With a usable trio they now go public only. A public failure before the login stops before a browser opens. A failure after it loses the new login (it lived only in a throwaway `CODEX_HOME`), and the message says so. Nothing falls back to the plaintext URL, and `sr server sync` stops at that target. Its account-diff read carries the admin token, not a credential, and stays private first. With no public path, a Codex login routes as before and is not refused: upstream never refused one, and a refusal would break working logins. `sr doctor`'s account-import check follows the same route (before, it said `ok` while the add hit the public path's 404). `claudeUploadPublicOnly` is renamed `credentialUploadPublicOnly`. Residual: an entry with the trio whose box has public admin off now fails these logins; enable it, pin the node, or run `sr server set-public home --clear`. `sr add-key`, Kimi, Antigravity and the Qwen sync stay private first, by the owner's choice, so an API key typed into `sr add-key` crosses plain http to such an entry. Without a usable public path a Codex login also still goes over plain http (upstream's behaviour, kept on purpose). Not verified live. Deploy: `sr` only. Nothing changes on the box.

- Knobs: `SUBROUTER_PUBLIC_ADMIN=1` on the box (§43.2; without it the preflight gets 404 and nothing is sent)
- Code: `cmd/subrouter/sr_claude_upload.go` (`pushClaudeProfileTo`), `cmd/subrouter/sr_server_public_admin.go` (`serverAdminCall.publicOnly`), `cmd/subrouter/sr_server.go` (`serverLoginOne`)
- Related: builds on §22 (the one Claude upload site) and §43.2 (public admin path).

## 52. A box account's Fable pool is published as fresh as its account, and the laptop ranks it

No box account was ever a candidate for a Fable session placed from the laptop. Live on 2026-10-10, the box's acct-p row published `normalized.fresh=true` but `normalized.pools.claudefable.fresh=false`. The box stamps `Fresh` only on the account score, and `withNormalizedUsage` built pool entries from unstamped sub-scores. The laptop ranks a Fable turn on the pool entry and drops stale ones. So new Fable sessions stayed local even against a box account in burndown, and when local could not serve, the turn went untargeted to the box's pre-§31 rules. Test fakes that hand-wrote `"fresh": true` into every pool hid it. Now the publisher stamps each pool with its account's freshness, and the consumer takes pool freshness from the account. Held turns (account-level verdict) and Codex turns (top level) were not affected. Afterwards a box account with Fable left by its own claudefable verdict competes in every laptop Fable ranking, on the shared comparator with local's 2x. The same applies to claudeopus and claudesonnet when the box publishes them. When local Fable is spent and a local Opus account can take the turn, §11's Opus stage still runs first. Either side alone fixes the ranking. Deploy: daemon change; both sides are needed for the published row to read right on every reader. Left open: `SchedulerRef` mark reconciliation reads a raw sub-score's `Fresh`, so a `claudefable` 429 mark lapses on its TTL instead of being dropped early. That costs some Fable placements and misroutes nothing.

- Code: `internal/proxy/proxy.go` (`withNormalizedUsage`), `internal/proxy/remote_pool_decide.go` (`inheritPeerAccountFacts`)

## 53. `srq pool` shows how many sessions a draining account is carrying and will take

A draining account printed `new=yes (burndown)` and a full one `new=NO (at_cap)`, with no sign of how close to its allowance it was. Every §31 verdict already carries `live_sessions` (sessions with a request in flight on the publishing daemon) and `allowed_sessions` (the endgame taper allowance, 0 outside the 24h window). A `burndown` or `at_cap` verdict now reads `<reason> live/allowed`, for example `new=yes (burndown 1/2)`, `new=fallback (at_cap 2/2)` locally, or `FABLE-REFUSED (at_cap 2/2)`, with an `n/m` legend. Other rows, a verdict without the two fields, and an allowance of 0 render as before. `srq pool --json` rows gain `new_session_live_sessions`, `new_session_allowed_sessions` and their `fable_` pair (`null` when absent). Nothing changes on the daemon or the box.

- Code: `ops/bin/srq` (`_session_counts`, `_slots`, `_render_sections`)

## 54. An outage this machine cannot attribute to the peer keeps every hold; a dead box ends them

The breaker's opening (2 strikes) used to `clearAll` every hold on the peer, though nothing could tell
"the box is down" from "this machine has no network". The owner, 2026-10-09: "whenever I reset my
computer, which session goes to which box gets reset". That night a rebooted laptop with no network yet
failed two held turns, the opening emptied `peer-holds.json` while the box was up, and each session rode
its local placeholder pin while the warm rule read it warm (70 minutes). Of 14 openings in the eight
days to 10-09 only one was a reboot: 7 were `connect: network is unreachable`, 6 `i/o timeout`, 1 a
header timeout. The owner's rule: a failure not attributable to the peer is a P8 network blip, so the
hold stays and at most that one turn is served elsewhere (P4, P5; P6 unchanged: a refusal is an answer).

§54.1: the breaker still opens on the second strike (30 s cooldown, §21 dedupe unchanged). It ends the
holds (`peer_down`, forced) only if: the opening strike's error is not a local no-route error
(`ENETUNREACH`, `ENETDOWN`, `EADDRNOTAVAIL`, however wrapped); `upSince` (the first local answer after
the last local failure) is at or before its send; a local answer came at or after `seriesStarted` (the
egress is read when the opening strike's failure is seen); and, once the process has seen a local
failure or after a kept opening, the strike before was also sent at or after `upSince`. The series
restarts at a strike more than a cooldown after the last on the wall clock (`Round(0)`), since darwin's
monotonic clock stops in sleep. Any peer answer is attributable. The evidence is the local round trip,
no probe: `localEgressObserver` wraps `Server.transport()` on a machine with a peer (headers of any
status = answer, transport error = failure, client cancel = nothing), and `proxyWebSocket` notes the
Codex websocket handshake. The Fable chain, Bedrock, the Azure Codex fallback, the Codex regional
egress, usage sweeps and token refreshes are not observed. An unattributed opening evicts nothing. It
leaves the breaker one strike from re-opening, series kept, `unattributed` flag set (a peer success or
an opening that ends the holds clears it). An opening kept for `no_answer`, and every counted no-route
strike (opening or not), notes a local failure, which resets `upSince`. Review round 3 decision, in line with the owner's goal:
the next failure re-opens the breaker at once, but ends the holds only at the second strike sent since
recovery (`sentSinceBack`). One failure after recovery is what a cold tailnet link or a dead keep-alive
socket looks like. A dead box costs one more cooldown and failed forward. Since §67 an attributed strike
finds a dead box only once the peer has answered nothing for the grace (15 minutes).
`classifyAccountSwitch` row 21 is `transient/peer_unattributed`: the turn is served locally
(`turn.stayLocal`) and the hold kept. Failure lines log `breaker_opened` and `peer_down`. A kept opening
logs `remote pool breaker opened; holds kept: outage not attributed to the peer` with `held`,
`no_route`, `local_answer_age_ms` and `why` (`no_route`, `no_answer`, `network_back_mid_strike`,
`one_strike_since_back`).

§54.2: with no `clearAll` the 15 s flush leaves `peer-holds.json` intact. Expiry stays the 70-minute
idle bound (P9). §54.3: a restart loses the peer's usage body, so held turns went untargeted (two on
10-04 at 17:23:40). A hold now persists `Targeted` (omitempty), set when the peer served the exact
targeted account (HTTP forward, or websocket after the 101). An untargeted re-serve keeps it on the same
account and drops it on another. `honouredHoldTarget` keeps a `Targeted` hold's target with no body
unless the peer was seen ignoring targets; a legacy body without verdicts still blanks it. §54.4:
`holdUntil` stores `now.Add(ttl).Round(0)` (wall clock), since a hold set before a two-hour lid-close
was still live on waking. The 70 minutes include sleep.

Residual risks. A machine with no observed local traffic, or a provider-wide outage, reads a dead peer
as itself offline: holds live until it answers and two strikes since fail, or the 70 minutes lapse, and
each held turn pays its bound (2 s dial, 30 s header wait, 30 s websocket handshake) every cooldown. A
second silent drop after recovery still ends every hold at two failed peer requests unless an observed
local failure lands between. A strike sent offline that nothing marks offline (a timeout whose fallback
goes an unobserved way or is cancelled, in a process with no local failure seen) still lets the first
cold link after recovery end every hold. A strike since recovery never ages. An eviction is final even
if egress drops between the local answer that attributed it and the replacement's own answer. A box
downgraded within a hold's life strips `X-Subrouter-*`, serves its own pick, and the substitution rule
clears the hold. A loopback local upstream counts as egress. The plist stays: launchd has no working
network-online gate. Owner decision, 2026-11-19 (§67): a peer-path-only blip (6 of the 14 openings)
keeps the holds for the grace. Verify: `grep 'breaker_opened=true' ~/Library/Logs/subrouter.err.log`,
then read `peer_down=` on that line. Deploy the laptop; the box has no peer.

- Knobs: `SUBROUTER_PEER_DOWN_GRACE` (§67; 15 minutes; holds kept as `why=within_grace` until the peer
  answered nothing that long; any peer answer restarts it; `0` restores §54's timing),
  `SUBROUTER_PEER_HOLD_WAIT` (no longer read: since §67 a held turn waits for its peer until it is
  found down)
- Code: `internal/proxy/remote_pool_egress.go` (`localEgress`, `whyNotThePeers`, `noRoute`),
  `internal/proxy/remote_pool.go` (`remotePoolBreaker.failure`, `holdUntil`),
  `internal/proxy/account_switch.go` (`PeerOutageUnattributed`)
- Related: replaces §20's clear-all on breaker-open for an opening this machine cannot attribute. §67
  amends it with the grace (on 2026-11-19 a gateway refusing new connections made a live box read down 2
  to 4 s after it answered). §67 (67.2) supersedes the bystander rule: a peer found
  down ends no hold.

## 55. The drain's session cap reads the measured meters, not the live debit

For a local Claude account that is not weekly-only and has no owner reserve (`drainReadsMeasured`), the
drain's session cap (`endgameAllowedSessions`), 5 h clamp and 5 % floor (`endgameDrainEligible`) read
the last usage reading with the live debit added back (`drainInputs`). §41's formula is unchanged:
`ceil(spendable / 0.0625)`, 1 to 4, held to 1 while the 5 h window is projected to run dry. Ranking,
pace, `usableForNewSession`, `belowWeeklyReserve`, and the whole drain for reserved, weekly-only, Codex
and peer accounts keep the debit. `inheritAccountFacts` copies `Provider` into pool scores so the Fable
pool's cap reads measured too. No wire field changes.

Why: on 10/10 the box drained `acct-p` mostly one session at a time. §41's ladder on its readings said
3, then 2, but published `allowed_sessions` was 1 in 7 of 14 snapshots (23:58 to 01:03) and in 31 of 40
polls (01:08 to 01:27). `ApplyLiveDebit` takes `LiveDebitPerRequest` (0.02) off `WeeklyHeadroom` per
request since the last usage refresh. That is a 5 h-window unit (`TestVelocityPolicyExperiment`), about
200 times a request's real cost (0.45 of the window over ~5,000 requests). Four requests in one 30 s
cycle took 0.128 to 0.048, an allowance of 1. acct-p still spent its week (7 d meter 100 % at 03:08), so
the cap cost new sessions sent elsewhere, not budget.

Simulated (`selectacct/endgame_sim_test.go`, six seeds): `at_cap` refusals fall in every cell, the
account takes 2 % to 20 % more new sessions, forced moves rise +0.5 to +3.3 per drain, and forfeit
barely moves. Owner decision, 10/10: "just lift the cap for new sessions". The sim fails past +4.0
forced moves. Either deploy order is safe; the `peerVerdict` re-derive stays debited. Residual: a usage
outage leaves the cap on a stale, overstated reading, bounded by the ceiling of 4, the upstream 429
(`MarkExhaustedUntil`) and the roll. Whether the box reserves `acct-p` (`SUBROUTER_RESERVED_ACCOUNTS`)
was not checked. Re-sizing the 0.02 debit is a separate decision.

- Knobs: `SUBROUTER_ENDGAME_MEASURED_INPUTS` (unset = on; `0`, `false`, `no`, `off` = the pre-§55
  drain exactly; any other value stops the daemon starting; a change needs a restart)
- Code: `selectacct/scheduler.go` (`drainReadsMeasured`, `drainInputs`, `ApplyLiveDebit`),
  `selectacct/limits.go` (`SetEndgameMeasuredInputs`), `cmd/subrouter/main.go`
  (`endgameMeasuredInputsEnabled`)
- Related: amends §10 and §41 where they size the taper. §62 amends it (no 5 h reading: `ShortHeadroom`
  is 1, the floor does not bind). §60 amends it (a reserved account has no reserve in its last day).

## 56. A holder of an unclaimed saved reset ranks at 2x and drains early below 25% of its week

Owner decision, 2026-10-15: "I have a bunch of subrouter claude accounts with a one-time reset expiring
in a week. I'd like to give all of those a higher priority until reset--2x and once they hit sub-25%
weekly enter early burndown mode". The reset is §46's `cedar_ember` grant (one per Pro and Max account),
lapsing unclaimed at 2026-10-22 16:00 UTC. A claim throws away what is left of the week, and the router
ranked holders like anyone, so holders kept their budget. On 10/15 accounts on both machines held one.

`Score.ResetHeldUntil` is the absolute expiry of an unclaimed `cedar_ember` reset (zero = none); the
`Source` check keeps it Claude-only. `resetHeldUntil` sets it at both local score build sites. A `Known`
reading decides (held with a future expiry sets it, anything else clears it). A reading that knows
nothing (no block, or `Known` false) carries the previous hold: clearing flapped holders in and out of
the drain. Only the sweep starts the fallback read; the refresh reads its stored answer.
`inheritAccountFacts` copies the hold into every model pool, or the Fable pool would rank holders at 1x.

`rankingPace` ranks a holder at `ResetHoldPreferenceRatio` (2.0) times a local non-holder wherever it
lives, instead of stacking with `LocalPreferenceRatio`. Owner decision, 2026-10-15: "This 2x should take
precedence so whether they're remote or on-laptop they're 2x the on-laptop". Against a peer non-holder:
any holder 4x, a local non-holder 2x. Stacking left box holders level with laptop non-holders. It is a
pace factor, not a tier, so a non-holder over twice as fast still wins. Not in the both-draining branch.

`inResetBurndown(now)` is a local, non-exhausted holder whose measured `WeeklyRemaining` is above 0 and
below `ResetBurndownWeeklyRemaining` (0.25), on an unrolled reading. Not `WeeklyHeadroom`, which can carry
the Fable reservation: `fifteen-beta` read 0.39 measured, 0.195 headroom on 10/15. 0 is
unmeasured and stays out. `inBurndownWindow(now)` (24 h roll or early burndown) replaces
`inEndgameWindow` in drain eligibility, the taper cap, `allowed_sessions` and the `PickSteady` demotion,
so it runs the 24 h drain's taper, 5 % floor and reserve. `endgameUrgency` stays on the roll, so a
24 h-roll account leads. An earlier-of-roll-and-expiry deadline was dropped: it misorders on expiry day.
`BurndownCause` (`roll` beats `reset`) is published as `new_session.burndown_cause` (omitempty); routing
never reads it.

Peers learn early burndown only from the owner's verdict. For the 2x the owner publishes
`normalized.seconds_to_held_reset_expiry`, anchored at the body's time behind the 15-minute age gate.
Nothing auto-claims: the owner runs `sr claude reset <name>` (§46.7); an agent never claims unasked
(one-time, irreversible). Costs: non-holders keep more until their own 24 h drain; an early holder walls
days before its roll until claim or roll. `srq pool` shows `burndown·reset n/m`, `at_cap·reset n/m` and
a `2x` column. In a peer section that `2x` is the peer's word: this daemon ranks it 2x only on §56 with
its switch on. `sr` is unchanged, and `sr status` does not show early burndown (its Pick column can read
`not routable` for a holder being drained). Two additive wire fields; deploy box, then laptop (the
convention; either order is safe; an older box publishes no hold, so its holders rank 1x). Residual: a
reserved holder whose reserve exceeds its spendable week never burns down early; the legacy peer path
anchors the hold at now (2x up to an hour past expiry).

- Knobs: `SUBROUTER_RESET_HOLD_PRIORITY` (unset, `1`, `true`, `yes`, `on` = on; `0`, `false`, `no`,
  `off` = pre-§56 ranking on the same inputs for this daemon, but a peer's verdict still drains, so turn
  it off on the box first; any other value stops the daemon starting; a change needs a restart)
- Code: `selectacct/scheduler.go` (`rankingPace`, `inResetBurndown`, `inBurndownWindow`),
  `internal/proxy/claude_reset.go` (`resetHeldUntil`), `cmd/subrouter/main.go` (`resetHoldPriorityEnabled`)
- Related: amends §10 and §41 (which accounts drain first) and §46 (an unclaimed reset before its claim).
  §57 amends it: by default early burndown takes the full taper (4 / 3 / 2 / 1 below 25 % and at or
  below 18.75 / 12.5 / 6.25 % measured); only with `SUBROUTER_FABLE_RESERVATION=1` is it 2 at 25 % and 1
  below about 12.5 %. Lifting the reservation was cut: 4 Fable sessions on a Fable meter with 5 % left.
  §64 amends it: only a reset that gives back the week holds an account, and the hold is published as
  `seconds_to_held_week_reset_expiry`, with the legacy key beside it.

## 57. The Fable weekly reservation is off unless `SUBROUTER_FABLE_RESERVATION=1`

Owner decision, 2026-10-15: "also, Fable allocation isn't important anymore! Eventually
anthropic will release a new fable and the fable weekly limit will matter again but for now
it's all about the opus". §10's reservation subtracts `0.5 × fableRemaining ×
decay(timeToRoll)` from every non-Fable Claude pool's `WeeklyHeadroom`, which halves the week
while the Fable meter is unspent (10/15: `fifteen-beta` read 0.39 measured, 0.195
`WeeklyHeadroom`). That cut the drain taper (a §56 early-burndown holder at 24 % got 2 slots,
not 4), tripped `belowWeeklyReserve` at about twice the owner's reserve, and ranked such
accounts at about half their pace for Opus and Sonnet. Now `scoreFromLimitWindows` takes the
reservation only when the switch is on. Off (the default), a non-Fable pool's `WeeklyHeadroom`
is `WeeklyRemaining` less the live debit; on, it is the pre-§57 formula exactly. Off, the
taper is the full 4 / 3 / 2 / 1 below 25 % and at or below 18.75 / 12.5 / 6.25 % of the
week, and the reserve gate trips at the owner's reserve on the debited week. The Fable
pool's own score, `WeeklyRemaining` (so `inHoldout` and §56's trigger) and Codex scores do not
change. `sr` never reads the variable, and the pool board rescores box rows under this daemon's
switch, not the box's.

No wire change: daemons on different builds or switches skew only the cross-pool balance, never
eligibility, so set the switch the same on both and deploy the box, then the laptop. Residual
risk: non-Fable traffic can exhaust a general week while the Fable meter is unspent, stranding
Fable capacity (accepted while no Fable limit binds). §55's sim s1b now adds 3.84 forced moves
per drain at the central rate (3.34 with the reservation) against its +4.0 guard. Turn the
switch on, on both daemons, when Anthropic ships a new Fable whose weekly limit binds again.

- Knobs: `SUBROUTER_FABLE_RESERVATION`, set in the daemon's environment (systemd
  `EnvironmentFile`, LaunchAgent plist). Unset, `0`, `false`, `no`, `off` is off, the default;
  `1`, `true`, `yes`, `on` restores the pre-§57 reservation; any other value stops the daemon
  from starting; restart, not rebuild.
- Code: `selectacct/limits.go` (`fableReservationOn`, `SetFableReservation`,
  `scoreFromLimitWindows`), `cmd/subrouter/main.go` (`fableReservationEnabled`).
- Related: amends §10 (the reservation) and §56 (its taper residual). §65 stacks with it when
  both are on.

## 58. A request whose upstream cannot be reached is held and retried, not failed

On 2026-10-20 the laptop lost its network twice: from 16:00 to 19:59 CDT 2,410 `/v1/messages`
got a fast `502 upstream request failed` (2,089 of them `lookup api.anthropic.com: no such
host`), and 204 more turns failed in the token refresh. Claude Code gave up within a minute,
and a workflow agent that gives up is dead. Owner decision, 2026-10-21: "Subrouter holds and
retries" (with `CLAUDE_CODE_MAX_RETRIES=15` as well), toward his goal: "in a perfect world what
would happen is sometimes I close my computer and subrouter or something like it just handles
that gracefully".

Now when an attempt fails transiently and nothing has reached the client, the daemon waits (1 s
doubling, equal jitter, capped at 30 s) and re-runs the whole handler (selection, refresh, peer
ranking, failover) until an answer or the deadline, counted from arrival. A hold pins nothing:
a failover's provisional move still commits only on a 2xx (§20, §31). Transient marks
(`noteHoldable`) are `upstream_unreachable:<cause>` (`dns`, `connection_refused`,
`dial_timeout` and similar), `token_refresh:<cause>` (no verdict and no peer took the turn),
and `upstream_status:<code>` or `peer_status:<code>` for 500/502/503/504 (`upstream_status`
only from a pool account; on `/v1/messages` after the same-account overload retries of 1 s and
2 s, which each held attempt runs again). Never held: a cancel, the client's deadline, a
certificate that does not verify (a captive portal is a network that lies, not one that is
down), 429, 529, an exhausted pool, a refusal, a drain, a 401, or anything after the status
line.

Only the private listener holds. The public :31416 never does (its callers that matter are
peers), nor does a websocket upgrade, an `X-Subrouter-No-Retry` canary, a peer forward (nonzero
`X-Pool-Hop`; the forwarder holds it) or a chunked or oversized body. `proxyCaller` runs before
the body is read, so a refused caller gets its 401 with the body unread and cannot force a
128 MiB buffer. Each attempt gets its own `r.Clone`, because the handler swaps in the account's
`Authorization`. Past either cap (256 held requests, 512 MiB of bodies) a failure is answered
at once with a `proxy hold refused` line; the first attempt never waits for a slot. The client
leaving ends the hold; a drain or restart ends it at the next check (at most one 30 s wait). On
give-up the client gets exactly the failure it would have got. An upstream error body over
256 KiB streams to the client and ends the hold. An attempt the upstream never served (a DNS,
dial or TLS-handshake failure, or a pool account's 5xx) refunds its 0.02 live debit
(`RefundRouted`): twenty held attempts would read a healthy account as 40 % spent. A reset or a
timeout after the request was sent keeps its debit. The 8 minute default stays under Claude
Code's `API_TIMEOUT_MS` (600 s to headers) with room for a 30 s dial and a 10 s TLS handshake.
A deadline past about 9 minutes would make the client give up first. Not covered: a sleeping
laptop (only a turn that fails just after wake is held), Codex websocket turns, fallback-stage
5xx, and peer failover (§59). A held turn that tries the peer counts a breaker strike per
attempt (§54). Deploy: the laptop alone is enough; the box would hold only turns on its tailnet
listener. To disable on the laptop, set `SUBROUTER_HOLD_DEADLINE=0` and run
`RELOAD=1 deploy/macos/deploy-laptop.sh <that binary>` on the installed build, which re-reads
the plist (`kickstart -k` does not).

§58.1: `transientNetworkCause` called any `context.DeadlineExceeded` not transient before
checking DNS and dial errors, and standard-library dial and lookup timeouts match it
(`mapErr`). So a dial `i/o timeout` failed at once: 426 times in the laptop log of 10/20 to
10/21, against 321 `connection refused`. Now only `context.Canceled` returns early, DNS and
dial timeouts are held, and other deadlines (net/http's 10 minute header timeout, a context
deadline on a request already sent) are not.

- Knobs: `SUBROUTER_HOLD_DEADLINE` (Go duration, default 8 minutes; `0` turns holding off),
  `SUBROUTER_HOLD_MAX_REQUESTS` (default 256), `SUBROUTER_HOLD_MAX_BYTES` (default 512 MiB),
  `--hold-*` flags (win when given; keep them out of the plist and unit: a rolled-back binary
  exits on an unknown flag and is respawned forever). An unparseable variable is logged and
  ignored, so a plist typo cannot stop the daemon.
- Code: `internal/proxy/hold.go` (`holdOnTransientFailure`, `transientNetworkCause`),
  `selectacct/scheduler_ref.go` (`RefundRouted`), `cmd/subrouter/hold_config.go`.
- Related: §59 adds the peer spill this section left to the owner.

## 59. A failed lookup dials the last good address; an unreachable turn goes to the peer

§58's held attempts retry the same local placement. From 16:00 on 10/20 to 01:00 on 10/21 (CDT)
the laptop logged 3,148 `lookup api.anthropic.com: no such host` failures while the peer,
dialed by IP (`https://203.0.113.201:31416`), served 313 to 4,771 turns an hour: DNS was broken
while IP traffic moved. From 19:20 to 19:50, when dials to 160.79.104.10 were refused too, only
the peer could have served. Owner goal, 2026-10-21: the laptop survives a repeat of 10/20.
The owner approved spilling to the box.

Sticky DNS: every upstream dial by host name (turns, token refresh, usage reads, Codex egress
and websocket) goes through the process-wide `upstreamLastGood`. A dial that connects records
its address (the latest per host) only if publicly routable: never loopback, link-local,
private (the 10/20 resolver at times handed out 172.16.0.1), 100.64/10 or 198.18/15. A dial
that fails with a `*net.DNSError` dials a remembered address within the maximum age once, on
the same port; a client-canceled lookup never does. The URL is kept, so SNI and the certificate
check use the real hostname and a moved address fails TLS. Only a lookup refreshes an address's
age, a lookup that answers is always used, and nothing is kept on disk. The peer needs no cover:
`SUBROUTER_REMOTE_POOL` names it by IP.

Outage spill: in `rp.ErrorHandler` a failure that never left the machine (`neverSent`: a
lookup, a refused or timed-out dial, a TLS handshake timeout) goes to the peer before it is
answered, if the client is still there, `peerAllowed` and `peerForwardingAllowed` pass, the
body is replayable, and the bucket has a token. Never for a held session's turn whose peer
attempt just failed transiently (`turn.stayLocal`): it may be running there. It is served like
a no-verdict refresh (`serveAcrossPeers` with `remotePoolHoldNone`): the session keeps its
local account, no peer hold is set, the next turn tries local first, and the local debit is
refunded. With no token or no peer, §58 holds the turn and each held attempt asks again. A sent
failure, a 429, a 529 or an exhausted pool never spills. The bucket is daemon-wide (the public
listener's `rateLimiter`): 5 a second, burst 10. The box allows 20 a second per token (burst
200), and the laptop averaged 1.8 turns a second over the 24 hours to 10/21. Turns placed on
the peer by ranking or a hold are not spills and do not count.

Settings come from the environment only, never flags (§58's rollback reason). A value that does
not parse is logged and the default used. Not covered: a hung resolver or a blackholed upstream
(30 s per new connection or spilled turn), Codex websocket spills, and the older uncapped
refresh peer serve (§20, §31). Deploy the laptop alone (the box has no peer). To disable a
part, set its variable to `0` and redeploy with `RELOAD=1` (§58).

- Knobs: `SUBROUTER_STICKY_DNS_MAX_AGE` (Go duration, default `168h` wall-clock; `0` turns the
  fallback off, though addresses are still remembered), `SUBROUTER_OUTAGE_SPILL_RATE` (default
  `5` a second; `0` turns the spill off; `NaN` and `Inf` are unreadable),
  `SUBROUTER_OUTAGE_SPILL_BURST` (default `10`; below 1 is 1).
- Code: `internal/proxy/upstream_last_good.go` (`upstreamLastGood`, `publiclyRoutable`),
  `internal/proxy/outage_spill.go` (`spillUnreachableTurn`), `cmd/subrouter/outage_config.go`.
- Related: builds on §58 and closes the gap it left to the owner.

## 60. The owner's holdout is lifted in the last day before the weekly roll

Owner decision, 2026-10-29: "the 5% main account holdout--can you turn that holdout feature off
for the final 24 hours?" `SUBROUTER_RESERVED_ACCOUNTS` holds back part of a week (`main` at the
default 5%, `owner@example.com` at 20% on the laptop) so the owner keeps a way in, but the
roll wipes whatever is held: on 2026-10-29 `main` sat at 95% used, 5h from its roll, its 5%
about to be lost. Now `measuredScore` drops the reserve (`Score.reserveReleased`) when the
account's week rolls within `claudeEndgameWindowSeconds` (24h). The reserve gate and
`inHoldout` stop refusing and evicting, the taper sizes on the whole week, a non-weekly-only
local Claude account enters §55's scope, and the published verdict follows. A reading carried
past the roll is released; an unknown roll keeps the reserve; §56's earlier burndown still
stops at the holdout. A model pool follows the account's roll (`Score.accountWeeklyResetAt`,
via `inheritAccountFacts`), not Fable's own. Each daemon decides this for its own accounts. The
trade: no way in on that account in its last day when the rest of the pool is spent. The holdout comes back with the new week.

In the sim (s4, 6 seeds, at 2% of the week per session-hour) the reserved account's week left at
its roll goes from 3.90% to 0% and forced moves per drain from 5.75 to 8.17 (at 4% of the week
per session-hour: 3.87% to 0%, 6.17 to 6.08). `TestEndgameSimulation` fails if the release
stops spending the reserve or adds more than 4.0 forced moves per drain. Never loosen that
bound: take the trade to the owner. Deploy to the daemon that holds the reserved accounts (on
the laptop, `main` and `owner@example.com`).

- Knobs: `SUBROUTER_RELEASE_RESERVE_FINAL_DAY` (on by default; `0`, false, no or off keeps the
  holdout through the roll; any other value stops `serve` from starting).
- Code: `selectacct/scheduler.go` (`measuredScore`, `Score.reserveReleased`),
  `selectacct/limits.go` (`SetReleaseReserveFinalDay`), `cmd/subrouter/main.go`
  (`releaseReserveFinalDayEnabled`).
- Related: amends §20 (the holdout is a hard limit) and the drain's reserve gate (§10, §41).

## 61. Tests never see the developer's HOME, and five reds that were the tests' own

Five tests went red with no change to the code they test. (1)
`TestStaleUsageScoresAfterAnAccountReloadRefreshOffTheRequestPath` set no HOME, loaded the
developer's real Claude store, and printed live refresh tokens (2026-10-29), because §45's
TestMain cleared only the four override variables. `testenv.IsolateHome` now also points HOME
at a fresh empty directory per test process, and the test prints only a count. The probe
`TestNoDevelopersHomeReachesATest` re-runs its binary with HOME exported at a decoy and fails if
that HOME reaches a test. (2) `TestLocalClaudeUsageReadCarriesTheReset` was red from
2026-10-22, when its fixed grant expired, so every PR's CI was red since; its grant now ends a
week after the run. (3)
`TestConcurrentCatalogRequestsShareOneCompleteWalk` (`upstream saw 8 list requests for 6
concurrent clients, want 4`) was a fixed 50 ms overlap racing the session store, whose four
locked re-reads of `sessions.json` cost about 16 ms a request. It now waits on
`singleFlight.waiting` (up to 10 s): 0 of 1,800 runs fail under load, against 164 before. (4) A
websocket test's `TempDir` cleanup failed (`directory not empty`) because
`httptest.Server.Close` does not wait for hijacked handlers; `newProxyTestServer` waits up to
10 s more and backs 68 servers. (5) The process-group test's helper exited 143 under `set -e`
when SIGTERM killed its foreground `sleep` (175 of 600 runs); it now loops on `sleep 1 & wait
"$!" || :`. The session store's ~16 ms per routed request is a candidate follow-up. Nothing to
deploy.

- Code: `internal/testenv/testenv.go` (`IsolateHome`),
  `internal/proxy/proxy_test_server_test.go` (`newProxyTestServer`),
  `internal/proxy/single_flight.go` (`singleFlight.waiting`).
- Related: fixes the catalog-test timing flake that §57, §58 and §59 record.

## 62. A Claude account with no 5h reading is not floored on its week

Anthropic stopped returning `five_hour` for Claude Max accounts around 2026-10-31 (board of
2026-11-02 15:15 CST: every local and all but one box Claude row lacked it).
`scoreFromLimitWindows` filled the missing short window with the week, so the 5% floor
(`MinShortHeadroomFloor`) refused, and would not drain, an account under 5% of its week, even
in its last day. On 2026-11-03 the box's `pixel-gamma` (96% used, roll in 1h38m) read
`eligible: false`, `reason: short_floor`, and lost its last 4% at the roll. The old
`WeeklyOnly` "conservative flat floor" dated from when a null `five_hour` was a one-fetch gap,
and Anthropic applies no 5h brake now: one warm session has spent a whole week in 6 to 10
hours. Owner decision, 2026-11-03: "I want that last 5%".

Now a Claude score with no short window keeps `ShortHeadroom` at 1 (`WeeklyOnly` stays false).
A `five_hour` that arrives is read as before, and a Codex weekly-only account keeps its week
under the 2026-09-14 floor. Such a Claude account takes sessions to its weekly wall on weekly
pace and drains in its burndown window (one session under 6.25% of the week). A spent week, a
429 (`MarkExhaustedUntil`) or a reserve outside the last day (§60) still stops it.
`ApplyLiveDebit` no longer debits an unread 5h figure: debiting the week there refused a busy
account after about 15 routed requests at 35% left. Its Fable pool's `ShortHeadroom` is 1 too,
so a Fable meter under 5% runs to its Fable wall. `normalized.short_headroom` publishes 1. `sr
claude` (§15) reads the daemon's `new_session.eligible` (a rebuilt score only for a daemon too
old to publish one), and `sr status` adds the unused Fable window
(`agentclaude.WithUnusedFableWindow`) as the daemon does. The board, `srdash` and `srq` show a
5h figure only where one was read, and the burn note names the reserve or the live debit.

The trade: 0.8 to 4.0 forced moves per drain at the weekly wall in the sim (none before). After
a weekly 429, a fresh 99% reading drops the mark and costs one rejected round trip per fresh
read (about 30 an hour at the 2 min `usageWindowsTTL`, an estimate); every Claude account had
that exposure before 2026-10-31. These accounts stay in §55's scope: leaving it would save at
most 2.3 forced moves per drain where nothing is left unspent, and bring back the one-session
drain §55 removed on the owner's 2026-10-10 request. Sim s5 (`pixel` at 4%, no 5h reading) went
from 0 new sessions and 4.00% carried to the roll to 10.3 / 8.3 / 4.3 sessions and 0.92% / 0% /
0% left across its three rates. It fails if s5 carries over half its 4% or exceeds 5.0 forced
moves per drain. Never loosen that bound: take the trade to the owner. No wire change and no
switch; undo by deploying the previous build. Deploy both daemons, in either order (the box
holds the accounts that lost their 5h bucket and roll soonest).

- Code: `selectacct/limits.go` (`scoreFromLimitWindows`), `selectacct/scheduler.go`
  (`ApplyLiveDebit`), `internal/proxy/pool_ui.go` (`classifyPoolState`, `burnNote`).
- Related: amends the exhaustion floor of 2026-09-14 and §55 where the drain's
  5% floor reads the 5h meter.

## 63. A named caller's Claude system prompt can be replaced before routing

For callers named in `SUBROUTER_CLAUDE_SYSTEM_PROMPT_CALLERS`, the daemon replaces the
host's Claude system prompt with a file's text before routing. Why: some hosts drive Claude
Code through the Agent SDK with their own system prompt, and no Claude Code setting (the
`agent` key in `settings.json` or the SDK's `agent` option, on 2.1.293) replaces it. The
proxy is the one point on the path the operator controls.

Callers must be named access tokens (§43). `accesstoken.ValidateName` refuses `open`,
`loopback`, `static` and `static:*`. Only Claude POSTs to `/v1/messages` and
`/v1/messages/count_tokens` (and the bare `/messages` forms) are rewritten, in
`proxyHandler` before the session ID or model is read, so the peer link forwards the new
body. In `system`, the text from the marker to the end of the first text holding it becomes
the file's text. Earlier text, `cache_control` and every other block, tool and turn are
kept. `SUBROUTER_CLAUDE_STRIP_TAGS` (for example, a host's injected-context tag) cuts tagged
spans from user text. It drops an emptied block and moves its `cache_control` to the nearest
kept block in the same message that has none, so breakpoints never grow. A message that
would be left empty is kept as sent. The rewrite is a pure function of the body, so the
prompt cache keeps hitting. JSON is re-encoded without HTML escaping. It fails open: a
non-object, compressed, over-128-MiB or unmarked body (Claude Code's own side calls
included) passes byte for byte. Half a configuration, an unreadable or empty file or a
refused caller stops `serve` with an error naming the variable.
The file holds the stock prompt (5,625 bytes from 2.1.293), captured outside this repo,
with its memory section pointed at the callers' folder (the `autoMemoryDirectory` in their
`settings.json`). Request content is never logged.

- Knobs: `SUBROUTER_CLAUDE_SYSTEM_PROMPT_FILE` (read once at startup) and `SUBROUTER_CLAUDE_SYSTEM_PROMPT_CALLERS` (comma or space separated; neither set is off), `SUBROUTER_CLAUDE_SYSTEM_PROMPT_MARKER` (default `<identity>`), `SUBROUTER_CLAUDE_STRIP_TAGS` (off unless set). Environment only, in `/etc/default/subrouter` on the box. No flag, so a rolled-back binary that predates it still starts.
- Code: `internal/proxy/claude_system_prompt.go` (`Server.rewriteClaudeSystemPrompt`), `cmd/subrouter/claude_system_prompt.go` (`claudeSystemPromptFromEnv`)

## 64. Fable 5.2: a Fable reset holds no account, the meter is the Fable row, fallback is Opus 5.5

Anthropic launched Fable 5.2 on 2026-11-09 and gave every pooled Max account 1-3
`cedar_ember` Fable resets that clear only `seven_day_overage_included` (Claude Code
2.1.321's "Fable limit", subrouter's `oauth-apps-weekly`), ending 2027-03-09T07:59Z. Three
things broke. `resetHeldUntil` held every account to its soonest grant whatever it cleared,
so every laptop row ranked at 4x (erasing the local 2x), and `thirteen-beta` (7d
94 %, Fable 0 %) burned down its week on a grant that does not refill it. The Fable meter
came from `seven_day_oauth_apps`, which Claude Code never reads. Spent Fable fell to
`claude-opus-5`, the May 2026 Opus.

This text was first written on 2026-11-09, for code on a branch that was never merged. The
code reached `main` only with the change that ported it on 2026-11-22: until that change, a
daemon built from `main` held every account on any grant (§56's rule), read the Fable meter
from `seven_day_oauth_apps` alone and fell to `claude-opus-5`. The text below was corrected to
the ported code where the two differ.

Measured on `eight-alpha` (2026-11-09, 19:22Z): a claim took the week from 100 % to 42 %
used and the Fable meter from 116 % to 0 %. So the Fable limit is half the week, and a reset
only refunds the Fable usage off it. A claim loses nothing left in the week, so §56's reason
to hold an account does not apply.

A grant's kind (`ResetGrant.Kind`) is read from its `clears`: `week` when it lists
`seven_day` or nothing (October's shape), `fable` when it lists `seven_day_overage_included`
and not `seven_day`, else `other`. The week hold (`WeekHeldUntil`, published on the row's
`complimentary_reset` as `week_held_until`) is the soonest `ends_at` among live `week`
grants. Live is the account eligible and the grant with a reset left, not expired and not
paused. An `ends_at` that does not parse holds nothing. A Fable-only account is no holder:
it ranks exactly as an account with no reset, on every pool, and its row still shows the
resets. On 2026-11-22 the pool read two shapes: one week reset per account, clearing
`seven_day` and `seven_day_overage_included`, to be used by 2026-12-10 22:00Z, with Fable
resets to 2027-03-09 behind it. Such an account is a week holder until 12-10, never until a
Fable reset's end, and with two live week resets until the sooner end. The hold is
published as `normalized.seconds_to_held_week_reset_expiry` and, with the same value, as the
legacy `seconds_to_held_reset_expiry`, for week holds only. Every reader (the peer score,
the board's box rows, `srq`) takes the week key, else the legacy key
(`heldWeekResetSeconds`): a body with the legacy key alone is from a pre-§64 daemon, which
held an account on any grant, and it ranks as it did before, at 2x. Changed in the
2026-11-22 port: the first version read only the new key, so a pre-§64 body ranked 1x.

The Fable meter (`UsageResponse.FableLimit`) is the usage answer's `limits[]` row of kind
`weekly_scoped` whose model display name starts with "fable" in any case, and of several
such rows the most used one. A `weekly_all` or `session` row scoped to Fable is not it. With
no such row the meter is `seven_day_oauth_apps`, as before. Both usage reads build the
`oauth-apps-weekly` window from it, the daemon's and `sr`'s own (`claudeUsageWindows` in
`internal/proxy/proxy.go` and `cmd/subrouter/sr.go`), so the `claudefable` verdict, the
scheduler's Fable pool and §65's `fable_week_spent` follow the row. The window takes the
row's `percent` and `resets_at`. With a row in hand no Messages probe is sent; the probe
still runs when neither is there. Rows decode one by one: a row that does not decode is
dropped, and a `limits` value that is not a list reads as none, so drift never fails the
usage read. When the row and `seven_day_oauth_apps` differ by more than one point, the
daemon logs `claude fable meter: the limits[] Fable row and seven_day_oauth_apps disagree;
the Fable window uses the row` at WARN, with both percents and both `resets_at`, at most
once an hour per account (`fableMeterWarner`). An answer without a usable
`seven_day_oauth_apps` logs nothing, and `sr` never logs it.

`ResetGrant` also decodes `type` (`session`, `weekly`, `full`), `gives_back_weekly` (false
when absent or not a boolean) and `unavailable_reason` (`cooldown`, `blocked_five_hour`,
`blocked_seven_day`, `blocked_opus`, `blocked_sonnet`, `blocked_other`, `cannot_clear`,
`needs_limit`, `unavailable`), as Claude Code 2.1.321 does: a value it does not know reads as
absent, and the grant is kept. Nothing routes on them; a claim's answer carries them in its
`grant`.

Every Fable model falls to `claude-opus-5-5` (`claudeSubstituteOpusModel`) where it fell to
`claude-opus-5`, whichever Fable version and case the client sent, with its bracketed suffix
kept: `claude-fable-5-2[1m]` becomes `claude-opus-5-5[1m]`, and the `fable` alias becomes
`claude-opus-5-5`. Opus and Fable versions do not release in lockstep, so matching the
version number lands on an older Opus or on an id Anthropic rejects. The one function serves
§11's stage at selection and in flight, §50's undo and §71's rewrite of a subagent's turn
(`claudeOpusBody`); nothing else in them changed.

The latent Bedrock Fable fallback (`claudeFableBedrockResponse`, behind
`SUBROUTER_FABLE_BEDROCK_PRIMARY` and the fallback chain) serves the Fable version the client
asked for (`bedrockFableModelIDFor`). `claude-fable-5-2[1m]` goes to
`us.anthropic.claude-fable-5-2`, the `[1m]` marker dropped. An id that already holds
`anthropic.` passes through. The `fable` alias, no model, or any other value keeps
`us.anthropic.claude-fable-5`. The same id names the throttle log line, the quota-bump hook
and the cost record, which prices it at its own rates (`bedrockPriceFor`, per MTok): Fable
5.2 input 8, output 40, cache write 10 (5 m) and 16 (1 h), cache read 0.2; Fable 5.1 as
Fable 5 (10, 50, 12.5, 20) but cache read 0.25; Fable 5 unchanged. The `/bedrock/*`
gateway's cost log prices from the same table. The quota bump still keys every Fable id to
Fable 5's code (`bedrockTPMQuotaByModel`, `L-9B258944`), and the team route's auto-mode
classifier rewrite still sends Fable 5. `sr claude-aws --model` maps any `claude-*` id its
table does not name to `us.anthropic.<id>` (`bedrockModelID`), so `claude-fable-5-2` no
longer reaches Bedrock bare. Where Bedrock is not configured nothing changes, and no deploy
script configures it (§50). Not tested against Bedrock: if Bedrock does not offer the
requested version, its error goes to the API-key stage, or to the client when there is no
key, where before the turn was served as Fable 5.

The views name the kind. `complimentary_reset.remaining_by_kind` splits `remaining` by kind
(`{"week":1,"fable":1}`; absent from a pre-§64 daemon). `sr status`'s `1x reset` cell reads
`2 Fable 3/9` when every reset left is a Fable reset and `1 +1F 12/10` when a week reset has
Fable resets behind it (the week hold's end, even when a Fable reset ends sooner); undated,
`2 Fable` and `1 +1F`. The first number counts every reset that is not a Fable reset; no grant
of kind `other` has been seen. pool.json rows carry `reset_hold_s` (seconds to the week hold's
end) and `fable_resets`, and the board shows `F2` beside the pace, apart from the `2x`.
`srq pool` adds an `Fn` column. The board's notes, its legend, `srq`'s legend and the decision
reason say "week reset" where they said "saved reset". `sr claude reset` still claims
Anthropic's `next_grant_id`, of either kind, and its line names the kind and what Anthropic
cleared, with the Fable meter beside the week:
`Fable reset claimed — cleared seven_day_overage_included; 7d 100% -> 42% used, 5h 3%,
Fable 116% -> 0%; …` (an older daemon's answer, which names no grant, reads `reset claimed`).

Owner decision, 2026-11-09: "The resets last until March; we will absolutely chill on them,
we don't need to build around resets". Claiming stays manual (`sr claude reset <name>`). Its
y/N asks about the general week (when it has more than 10 % left), so read `Fable wk` in
`sr status` first. The §57 Fable reservation stays off. Deploy the box, then the laptop: a
pre-§64 box still drains its Fable holders under 25 % of their week through its `burndown`
verdict, which a laptop honours (§31), and a §64 laptop ranks them at 2x off its legacy key
until the box is deployed. A pre-§64 laptop keeps its own Fable holders at 4x and burns them
down early until it is deployed. Nothing is misrouted in either. The meter, the Opus target
and the Bedrock ids need no order: each daemon reads its own accounts' usage, and a peer
takes the windows a daemon publishes. Whether the `limits[]` row and
`seven_day_oauth_apps` are one bucket is unmeasured; the WARN line is how a deployed daemon
will show it.

- Knobs: `SUBROUTER_RESET_HOLD_PRIORITY` (still switches the week hold; `0` on a pre-§64 box stops its early drain). The meter, the Opus target and the Bedrock ids have no switch; going back is a rebuild.
- Code: `internal/agents/claude/reset.go` (`ResetGrant.Kind`, `ResetInfo`, `weekHeldUntil`), `internal/proxy/claude_reset.go` (`resetHeldUntil`, `heldWeekResetSeconds`), `internal/agents/claude/store.go` (`UsageResponse.FableLimit`, `UsageLimits`), `claudeUsageWindows` in `internal/proxy/proxy.go` and `cmd/subrouter/sr.go`, `internal/proxy/claude_fable_meter.go` (`fableMeterWarner`), `internal/proxy/claude_opus_substitute.go` (`claudeSubstituteOpusModel`, `claudeOpusModel`), `internal/proxy/bedrock.go` (`bedrockFableModelIDFor`), `internal/proxy/bedrock_cost.go` (`bedrockPriceFor`), `cmd/subrouter/sr_claude.go` (`bedrockModelID`)
- Tests (each run in Docker, red with its change taken out and green with it). The reset half: `internal/agents/claude/reset_test.go` (`TestResetGrantKindIsWhatItsClaimGivesBack`, `TestResetInfoCountsWhatIsLeftByKind`, `TestResetInfoHoldsTheWeekOnlyOnGrantsThatClearIt`, `TestParseResetGrantKeepsTypeGivesBackWeeklyAndUnavailableReason`); `internal/proxy/reset_hold_test.go` (`TestAFableOnlyResetRanksAsNoReset`, `TestAWeekResetHoldsTheAccountUntilItsEnd`, `TestTheWeekHoldEndsWithTheSoonerWeekReset`, `TestTheWeekHoldTravelsApartFromFableResets`, and §56's tests with Fable-only, mixed, legacy-key and week-key-only rows, the board's box rows among them); `internal/proxy/claude_reset_test.go` (`TestClaudeResetClaimsTheNextGrantOfEitherKind`); `cmd/subrouter/sr_provider_reset_test.go` (`TestClaudeResetCellNamesAFableResetAsOne`, with a Fable reset that ends before the week reset; `TestClaudeResetSaysWhichKindItClaimedAndWhatItGaveBack`); `srq selftest` (the §64 block); `internal/proxy/pool_ui_page_test.mjs`, a Node test that runs the board page's own script on a pool.json answer and checks the `F2` and `2x` marks and the legend (`node --test`, in Docker in the Node image; the Go test image has no Node). The meter: `internal/proxy/claude_fable_meter_test.go` (`TestTheFableMeterIsTheLimitsFableRowWhenTheAnswerHasOne`, `TestClaudeFableMeterDisagreementWarnsOncePerAccountPerHour`, `TestClaudeFableMeterIsQuietWithoutSevenDayOAuthApps`); `internal/proxy/claude_ratelimit_routing_test.go` (`TestClaudeFableWindowComesFromTheLimitsRow`, `TestClaudeFableWindowIsAWeeklyScopedRowNamedFableInAnyCase`); `internal/agents/claude/store_test.go` (`TestUsageReadSurvivesDriftedLimitsRows`, `TestFableLimitIsTheMostUsedFableRow`); `cmd/subrouter/sr_test.go` (`TestLocalClaudeUsageReadTakesTheFableWindowFromTheLimitsRow`, `TestLocalClaudeUsageReadTakesAWeeklyScopedRowNamedFableInAnyCase`). The Opus target: `internal/proxy/claude_opus_substitute_test.go` (`TestClaudeOpusModelIsTheNewestOpusWithTheClientSuffix`, `TestClaudeFableHandlerSubstitutesOpusWhenFablePoolExhausted` and the rest of the file), `internal/proxy/claude_opus_substitute_commit_test.go`, and §71's `internal/proxy/subagent_fable_pin_test.go`. Bedrock: `internal/proxy/claude_fable_test.go` (`TestServeClaudeFableFallbackServesTheRequestedFableModel`, `TestAStreamedBedrockFableFallbackIsServedAndPricedAsTheRequestedModel`, `TestABedrockThrottleOnFable52NamesItAndBumpsFable5sQuota`); `internal/proxy/bedrock_cost_test.go` (`TestBedrockUsageCost`); `cmd/subrouter/sr_claude_aws_test.go` (`TestBedrockModelID`)
- Related: amends §11 (the Opus target and the Fable meter), §46 (which grant is current), §56 (which grants hold an account), §65 (the meter its `fable_week_spent` reads) and §71 (the Opus its rewrite names).

## 65. An Opus or Sonnet turn ranks an account whose Fable meter is spent at up to 2x

Owner decision, 2026-11-12: "when we do opus turns we should preferentially be using those"
(accounts whose Fable meter is spent), in "the clean, proper version": a ranking term that
leaves slot and reserve numbers alone, unlike §57. Each account has a general week every model
spends and a separate Fable meter (`seven_day_oauth_apps`, since §64 the usage answer's
`limits[]` Fable row when it has one, published as `oauth-apps-weekly`).
Opus ranked on the general week alone and burned general budget that Fable also needs on
accounts with Fable left.

`Score.FableSpent` is the used fraction (0..1, clamped) of the account's own Fable meter, not
the `7d` copy (`copiesAccountWindow`) and not §57's lower of the two. It is computed in
`ScoreFromLimitWindowsAt` and carried to model views by `inheritAccountFacts`. No Fable window,
the unused placeholder, or a peer without the field reads 0. `ForModel` sets
`nonFableClaudeTurn` on every return path (the early one is the usual Opus path, since Opus and
Sonnet buckets come back null), true only for `claudeopus` and `claudesonnet`. Haiku is
excluded because Claude Code often opens a session with a Haiku call, which must not pin a
Fable session to a Fable-spent account. For that class `rankingPace` multiplies weekly pace by
`FableSpentFactor` = 1 + (`FableSpentPreferenceRatio` − 1) × `FableSpent`, ratio 2.0, stacked
on the local 2x and §56's hold factor. Only `compare`'s weekly-pace clause reads it.
Eligibility, `new_session`, `WeeklyHeadroom`, the taper, the reserve gate and the both-draining
clauses are unchanged. The decision reason names the preference only when it separated the pair
(`fableSpentDecides`).

Each daemon publishes `normalized.fable_week_spent` (top level, omitempty) whatever its switch
says. Peers read it clamped, `inheritPeerAccountFacts` copies it to each pool entry, and absent
is 0 (1x), so either deploy order is safe (convention: the box, then the laptop). Board rows
show `fable_spent_factor`; `srq pool` shows `fspent`. `sr status` and `sr`'s auto-switch rank
unscoped and are unchanged. The trade: Fable-spent accounts reach their general wall (and drain
or reserve) sooner; accounts with Fable left keep more general week for their own 24 h drain. Not
simulated: no eligibility, taper or reserve input moved. Residual risks: a Haiku-first session
stays where it was placed until re-placed. `FableSpent` has no expiry, so if a Fable meter
resets early (§64 moves its source to `usage.FableLimit()`), a carried seed ranks it spent
until the next good read. A setup token's `FableSpent` comes from the probe's `7d_oi` header (0
without one), though the design brief said always 0. The legacy `bestPeerScoreFrom` peer path
ranks unscoped, so the term is inert there. Mixed switches skew cross-pool ranking.

- Knobs: `SUBROUTER_FABLE_SPENT_PREFERENCE` (unset, `1`, `true`, `yes`, `on` is on, the
  default; `0`, `false`, `no`, `off` ranks exactly as before §65 but still publishes
  `fable_week_spent`; any other value stops the daemon from starting; restart, not rebuild).
- Code: `selectacct/scheduler.go` (`FableSpentFactor`, `rankingPace`, `ForModel`),
  `selectacct/limits.go` (`claudeFableMeterSpent`, `copiesAccountWindow`),
  `internal/proxy/remote_pool_decide.go` (`inheritPeerAccountFacts`).
- Related: stacks with §57 when `SUBROUTER_FABLE_RESERVATION=1`; §57 carries a "See §65" note. Until §104 the ranking of the peers' accounts alone (a daemon whose own pool cannot serve the turn) was not scoped to the turn's model, so the factor applied to none of those turns; §104 scopes it for every turn but Fable.

## 66. A Codex token OpenAI rejects as expired is refreshed at once, not at its own expiry

A Codex usage read that OpenAI answers `401` with code `token_expired` now refreshes that
one account's chain at once (reason `usage.token-expired`) and reads usage once more. Why:
on the box on 2026-11-16, OpenAI's usage endpoint answered two accounts that way every ~30 s
for hours while their JWT `exp` was 8 days ahead. After a ChatGPT plan change an account
could sit out of the pool for up to 10 days, because the sweeps refreshed only on JWT expiry
(`RefreshStoredIfExpired`), discarded the 401 body, and `scoreAccounts` zeroed the account.
One refresh brought it straight back. Any non-2xx now returns `*CodexUsageStatusError` (same
`Error()` text), which keeps `error.code`, else `detail.code`, from up to 16 KiB of a 401
body. Every other code and status is reported as before. Under the account lock the refresh
runs only if the stored token is still the rejected one or has expired by its JWT. Otherwise
it logs `skip_reason=access_token_replaced_after_lock` (debug) and reads usage with the
newer token. OpenAI rotates the refresh token on use, so two sweeps must exchange the chain
once. A renewed chain replaces the in-memory account, the usage-status row sets `refreshed`,
and the handler syncs the scheduler's credentials after the sweep, so the request path's
exhaustion mark on the rejected credential no longer applies. A failed refresh reports its
own error, not the 401. A transient one keeps the account's last score. A dead chain
(`refresh_token_reused`) is recorded by `noteCredResult`, so the next sweep reads it as an
invalid login without calling OpenAI. Unchanged: the proxied-request 401 path, and `POST
/_subrouter/account-status` still refreshes every account. There is no switch. Deploy the
previous build to stop it. Deploy to every daemon that pools a Codex OAuth account: the box
and the laptop.

- Code: `internal/accounts/codex_usage.go` (`CodexUsageStatusError`, `CodexAccessTokenRejectedAsExpired`), `internal/accounts/codex_auth.go` (`RefreshStoredIfRejected`), `internal/proxy/proxy.go` (`AccountRef.fetchCodexUsageDetails`)

## 67. A box is found down only after it answers nothing for 15 minutes; until then a chat waits for its account across retries; then it is a refugee, spread across the other boxes and on the laptop only when no other box can take it; it gives up after an hour and resumes when room appears

**Why.** On 2026-11-19 the building's gateway refused new connections for seconds to
minutes while connections already open kept working. The laptop found a box down six
times, each 2 to 4 s after that box had last answered. Each time it ended every hold on
the box and left its sessions on the laptop's one local Claude account, whose admission
gate (6 in flight, 64 queued) answered 1,731 turns `503 upstream admission queue is full`,
506 of them after the network had recovered. Cause: §54 counted an answer on a pooled
keep-alive connection as this machine being online, so a box whose new dials failed read
as down at the second strike.

**The owner's decisions (2026-11-19).**

- Before a box is marked dead: "it needs to wait for a while before marking the boxes
  dead. at least 5 min b/c cache. probably longer". The grace G is 15 minutes.
- D1, the cap: "any given home chat can only take 4 refugees". "Home" is the account
  that receives them. A refugee is a chat stranded because its box could not be reached.
- D2, overflow: "then they're just left, and they keep trying subrouter."
- D3, the order, as amended the same evening (R9): "Okay, we have two boxes. Can we have
  refugees try to go to the other box first? Can we have refugees try to go to any other
  accounts first? Have them be a fucking diaspora, because the only reason I'm able to
  have only one account is because they don't all fucking come here. I'm like
  Switzerland, so see if we can do it that way." Refugees spread across the other boxes,
  the laptop last, and only when no other box can take them.
- D4, no return: "if they return, then that's like bussing cash twice ... No returning.
  Connections try to drain normally."
- The limit and the wait: "there is a limit to total number of refugees; hold for
  longer". A chat moves only when its box is found down.
- The give-up: "claude code should give up just after an hour, because cache". The
  unlink: "I'd rather they go unlinked at 60" (R8).
- Resuming (R9): "it's not so bad if a refugee stops and is then restarted if we have to
  hard reset a box ... If this is a scenario where, say, I have one agent on computer, 12
  agents on box, and I'm at my hard cap, we should still be able to resume the refugees
  when the box has space." And: "Or when the new box comes back up again is what I
  mean."

**What.**

- **67.1, a box is found down only when two rules agree.** Gateway attribution blames
  the box: this machine has completed a new connection since the outage began. And the
  box has answered nothing, on any connection, for G. Any answer restarts G: a response
  of any status to a forward or a websocket dial, or a usage body. A strike more than 5
  minutes after the last restarts the silence too, so a sleep or an idle spell is not
  silence. An opening that fails either rule keeps the holds and says why
  (`no_new_connection`, `within_grace`, or §54's reasons). The box stays found down until
  it answers anything.
- **67.2, a box found down ends no hold.** It logs `remote pool peer found down; its
  chats are placed as refugees on their next turns`. A hold ends only when its chat is
  placed (67.4), gives up (67.5) or goes cold (an hour with no turn). New and cold
  sessions skip a box whose breaker is open, as before; they are not refugees.
- **67.3, a stranded chat waits for its account.** A model turn of a chat held on a box
  that cannot reach it (the dial failed, the breaker is open, or an earlier attempt may
  be running there) is held (§58) and run again on the same account
  (`reason=warm_wait:...`). At the hold's 8-minute deadline it answers 502, which Claude
  Code retries, and the retry is held again, until the box answers or is found down. A
  chat warm on the local account whose upstream cannot be dialed waits the same way and
  is never spilled to a box. A turn a box may be running is never sent there again. A
  stranded Codex websocket upgrade is answered 502, and its HTTPS fallback is held.
- **67.4, refugees.** On the first held attempt after its box is found down, a chat is a
  refugee (`placeRefugee`, `spreadRefugee`):
  - Every placeable account on every box but its own competes: targetable, eligible by
    its box's verdict, and not refused, declined or failed this turn. The account with
    the most free refugee slots takes it; the shared comparator breaks ties. Free slots
    are the account's cap less the warm refugees this daemon holds there. A claim counts
    again under the hold table's lock, so racing refugees cannot overfill an account. It
    claims only while the account holds no more refugees than when it was chosen;
    otherwise every account left is counted again and the one with the most free slots
    is taken, so racing refugees spread too.
  - A local account takes it only when every other box is found down, was asked and has
    every account full, refused or declined, or is not an option (closed to this caller,
    ignoring targets, or no forward possible). Local accounts are ordered the same way.
  - Any other box that cannot be asked right now (its breaker open, a failed dial, a turn
    it may be running, a transient answer, its usage unknown) keeps the chat on its home
    hold as `refugee_overflow`, retried on every held attempt. It is placed on that box
    once it answers, or on the laptop once that box is found down too.
  - The cap is `refugeeSlotsFor(peer, account, poolModel)`: the refugee cap the account's
    owner publishes (§72), or 4 with none. It may be any value from 0 up; an account with
    no free slot is full. Every read of it goes through that method: the box claim, the
    local placement, the sort and the log lines.
  - The mark (origin `box/account`, placed at) is persisted with the hold in
    `peer-holds.json` or with the assignment in `sessions.json`, so a restart keeps every
    count. A slot frees when its refugee goes cold, an hour after its last turn. The move
    is permanent (D4).
  - With no room anywhere the chat keeps its hold and waits (D2). Each held attempt tries
    again, so it is placed when a slot frees, or served at home when its box answers.
  - One turn of a chat places it at a time. While it does, the chat's home hold is
    cleared during each forward, and the claim on the account it tries is pending until
    that account answers. Another turn of the chat that comes meanwhile would read its
    local pin and be served on the laptop, unmarked and uncapped. It is held instead
    (`refugee_placing`), and its next attempt follows the chat where the placement put
    it, or places it.
  - Log lines: `refugee placed` with `to_peer`, `to_account`, `refugees` (with this one),
    `refugee_slots` and `free_slots` (before this one); `refugee not placed; the chat
    keeps its hold and waits` with `why` (`boxes_full` or `box_unreachable:<box>`) and
    `laptop_free_slots`.
- **67.5, the give-up, and resuming.** A stranded chat's chain of holds gives up an hour
  after its last answered turn (`SUBROUTER_STRANDED_GIVE_UP`, the warm window by
  default; `0` is no limit), or, for a spent pool's chat waiting for a box, an hour after
  its first held turn. "Answered" is a model turn whose 2xx, or whose websocket
  `response.completed`, the client got from an account's upstream. The give-up answer is
  `503`, `Retry-After: 3600`, `X-Subrouter-Reason: stranded_gave_up`, which Claude Code
  shows at once. Since §78 an end client is told `Retry-After: 60` instead, and a session
  with `CLAUDE_CODE_RETRY_WATCHDOG` sends the turn again a minute later. The hold reads
  the give-up again after each backoff, so a turn of the same chat answered meanwhile is
  not undone. The chain is kept with the session's
  assignment, so the next process carries it on; one the store cannot save is kept by
  the running process.
  - A chat whose cache was on a box then keeps its assignment marked waiting
    (`Assignment.Waiting`: the box, its account, and when). Whenever it is restarted, its
    next turn goes home first. Once that box's breaker lets a request through, found down
    or not, as a held turn's does, a forward targeted at its account serves the turn
    there, which takes no slot. A box back from an outage reads as found down until
    something reaches it, and this forward reaches it. Otherwise the turn is placed as in
    67.4, and a home box that is not found down but cannot be asked right now keeps the
    laptop out, as any other box does. When nothing places it (every account full, or a
    box that cannot be asked yet) it gets the same 503 at once, with no hold, and stops
    again. While the record lasts the chat is not placed as a cold session. The record
    ends when a box serves the turn, home included, whatever the box answers; when a
    local account takes it; or when a turn is answered. Otherwise it lasts until its
    assignment is deleted or pruned (the store keeps 512 assignments, dropping the least
    recently used one older than 70 minutes first). A record the store cannot save is
    kept by the running process, and a new process places the chat's next turn as a new
    session.
  - A chat on the local account is unlinked, and its next turn is placed as a new
    session.
  - A refugee placed on a local account is stamped seen there, so selection serves it on
    that pin even when it has been idle past the warm window.
- **67.6, a full admission queue holds the turn.** A non-targeted turn on an unpaused
  account refused for a full queue is held until a slot frees, within the hold's deadline
  and caps.
- **Every stranded hold answers 502 however it ends** (its deadline, a cap, a drain or a
  shutdown); only the give-up answers 503. No hold falls through to the spent pool's 503
  with a long `Retry-After`, which Claude Code shows at once, and none rewrites a 429, a
  529 or a 401.

**What a user sees.**

- *New connections refused, open ones working.* Turns on open connections are answered
  as before. A turn that needs a new dial waits and is answered by the same account.
  Nothing moves.
- *A box silent, short of 15 minutes.* Its chats' turns wait, a 502 every 8 minutes that
  Claude Code retries, and are served by their own accounts when it answers.
- *A box dead past 15 minutes.* Its chats spread across the other boxes' accounts, the
  emptiest first, each up to its cap, and reach the laptop only once no other box can take
  them. They stay where they were placed.
- *Another box unreachable too, but not found down.* The chats wait on their home holds
  and the laptop takes none until that box answers or is found down.
- *An hour without an answer.* The chat gets one 503, which its client shows, or, with the
  watchdog (§78), sends again a minute later. Restarted, it goes home if its box answers,
  or wherever there is room, or gets the 503 again at once. Its own box up but not
  answering yet keeps it off the laptop.

**Residual risks.** A chat on a dead box pays up to 15 minutes before it moves. A slot
stays taken up to an hour after a blip. A box that is never struck (no forward ever
reaches it, and its usage was never read) is never found down, so its refugees wait for
it until the give-up. A box account's count lives in `peer-holds.json` and a local one's
in `sessions.json`. A waiting record created for a chat with no local assignment names no
account, so the pool board's orphan prune removes it. With the hold off, a turn that comes
while another turn of its chat is being placed is not held, and is served as before. Two
turns of a waiting chat restarted at the same moment may each be placed.

**Open owner options.** An uncapped overflow move before the hold deadline (declined under
D2); a refugee activity window shorter than the warm window; `subagentPromptCacheTtl=1h`;
whether brand-new sessions (a workflow relaunching a dead agent under a new session id)
should prefer another box over the laptop during an outage: they keep the new-session
ranking, where a box account must be twice as good on weekly pace
(`selectacct.LocalPreferenceRatio`) to beat a local one, so they usually land on the
laptop; and how a waiting record ends besides an answered turn.

**Switch.** `SUBROUTER_PEER_DOWN_GRACE` (Go duration, default `15m`; `0` turns off only
the silence rule), `SUBROUTER_STRANDED_GIVE_UP` (default the warm window; `0` is no
limit; set above the warm window it logs a Warn at start), `SUBROUTER_HOLD_DEADLINE`
(`0` turns the hold off: a stranded chat moves as before this entry, and a refugee with
no room gets a 502 at once). Refugee placement, the spread and the waiting record have no
switch; deploy the previous build. A rollback is safe: the refugee mark, the answered
time, the held chain and the waiting record are optional fields that an older build
ignores.

- Code: `internal/proxy/refugee.go`, `hold.go`, `remote_pool.go` (`remotePoolBreaker`,
  `claimRefugee`, `refugeesOn`), `remote_pool_decide.go` (`serveHeldTurn`,
  `strandedOnPeer`, `rankedPeers`), `session/store.go` (`Refugee`, `PutRefugee`,
  `RefugeesByAccount`, `HeldChain`, `Waiting`, `Wait`, `EndWait`),
  `cmd/subrouter/hold_config.go` and `outage_config.go`.

## 68. A peer named in `SUBROUTER_PEER_TAILNET` is dialed over Tailscale first

`SUBROUTER_PEER_TAILNET` lists `name=IP` pairs, keyed by the peer's name: its
`SUBROUTER_REMOTE_POOLS_FILE` name, or the URL's `host:port` for `SUBROUTER_REMOTE_POOL`. Each
IP must be in `100.64.0.0/10` or `fd7a:115c:a1e0::/48`. The setting fails open: a malformed
pair, an unknown name, a peer named twice, or one IP and port on two peers logs one warning and
leaves every peer dialing its URL. It is an environment variable, not a peers-file field,
because a build already deployed parses the peers file strictly and would arm no peer at all on
an unknown field.

While this machine holds an address in `fd7a:115c:a1e0::/48` (only Tailscale assigns it; read
at most once a second, and a failed read keeps the last answer), a new connection to a
configured peer dials its Tailscale IP on the URL's port first. It gets 2 s to connect and 2 s
for TLS, with the same certificate pin. A connect, handshake or pin failure falls back to the
URL. A failure starts a one-minute cooldown only when the URL then answers. During the cooldown
a failed URL dial tries the tailnet once more, and when it ends only one dial probes the
tailnet. A connection dialed over the tailnet refuses its next write once this machine leaves
the tailnet, so the transport resends the request on another connection, and the breaker
counts that as this machine's failure, not the peer's. Both peer dialers (HTTP and the Codex
websocket) are covered, a dial to an `HTTPS_PROXY` is not rerouted, and the URL, Host header,
holds, breaker and the peer's idle-connection pool are unchanged. One Info line `remote pool
peer route changed` logs each change of route, reason or failure class, never one per dial.

Why: the laptop's building gateway (`172.16.0.2`) silently drops new connections when busy,
while connections already open keep working. On 2026-11-19 the laptop logged 2,033 failed peer
forwards (`dial tcp 203.0.113.221:31416: i/o timeout`, and the same for
`203.0.113.218:31416`), and the peer breaker opened 32 times. A Tailscale connection rides the
tunnel's one UDP flow, which the gateway already passes, and each box's `*:31416` listener
answers there with the same certificate. Owner decision, 2026-11-19: "if I have Tailscale on, it
should prefer Tailscale", and nothing else may change route. The laptop sets one pair per box,
for example `box=100.64.10.1`.

- Knobs: `SUBROUTER_PEER_TAILNET` (unset: every peer dials its URL exactly as before)
- Code: `internal/proxy/remote_pool_tailnet.go` (`DialPeersOverTailnet`, `dialPeer`,
  `hostTailnet`, `holdsTailscaleIPv6`, `remotePoolTailnetCooldown`), `cmd/subrouter/remote_gate.go`
  (reads the variable), `internal/proxy/remote_pool_egress.go` (`errLeftTailnet` in `noRoute`)

## 69. The laptop runs under `subrouter supervise`, and a deploy upgrades the worker without a restart

Every laptop deploy restarted the daemon, so every stream in flight was cut, and a terminal closed between the bootout and the bootstrap left the job unloaded with KeepAlive nothing to restart. The laptop job now moves once from `subrouter serve` to `subrouter supervise`, the one restart the owner accepted, and every deploy after it switches worker generations through the supervisor's control socket. Upstream's autoupdate, release installers and `migrate-*-to-supervisor.sh` stay unused: they install upstream's release or rewrite the plist. The runbook is `ops/LAPTOP.md`.

§69.1 `activate-laptop-supervisor.sh <build>` backs up the plist, the worker, the supervisor and the worker config into `~/.subrouter/supervisor-activation-<stamp>/`, baselines the route checks on the live job, installs the build as `~/bin/subrouter` and `~/bin/subrouter-supervisor` (staged, signed `-i subrouter`, renamed), writes `~/.subrouter/worker-config.json` and a plist that runs `supervise --addr <live> --control-socket ~/.subrouter/supervisor.sock --worker-bin <worker> --worker-stop-grace 2m --drain-timeout 90s --ready-timeout 2m --worker-config ~/.subrouter/worker-config.json -- <serve args>`, and restarts the job. The environment is unchanged, `SUBROUTER_CLAUDE_SOCKET` stays in the supervisor's, ExitTimeOut is 120 and ThrottleInterval 10. No `--local-data-socket`: `sr` attests the store over TCP. `ACTIVATED` needs supervisor-status to answer, the supervisor to own the Claude socket, and every route check to answer no worse than the baseline; anything else stops the job, restores the files and starts it again. Once the job is booted out, the activation's restore and `--rollback` always bootstrap the plist on disk: a failed re-sign or a supervisor binary that cannot be removed is named (exit 2), and a plist that cannot be put back keeps the supervised plist and the files it names, so the job comes back supervised. They returned before the bootstrap, and a route regression with a failed re-sign ended with no job loaded. When launchd will not load the job, they print `launchctl bootstrap gui/<uid> <plist>`. `--peer-tailnet <name>=<ip>[,...]` goes into the worker config as `SUBROUTER_PEER_TAILNET`. `--rollback` derives a plain-serve plist from the current supervised plist and the worker config, keeps the current worker and restores no binary. Both are idempotent: an activated job is a no-op, and a job not loaded is bootstrapped from the plist on disk and verified.

§69.2 On a job launchd runs under supervise, `deploy-laptop.sh` installs the build at the job's `--worker-bin`, POSTs `/_subrouter/upgrade` on its `--control-socket`, and checks that supervisor-status reports the build's CDHash as `active_worker.executable_identity` before the route checks. The supervisor finishes an upgrade after its client goes away, so an answer other than 200, a curl timeout included, is settled by `active_identity`: the new build's CDHash goes on to the route checks, the backup's installs the backup, and neither also installs the backup and fails. A failed check installs the backup and upgrades again. The upgrade runs detached, as a restart does: in the calling shell, a SIGKILL during the POST with a never-ready build left that build at `--worker-bin` while the old worker served, and the supervisor rebuilds from that path when its worker exits. The job restarts only with `--restart-supervisor`, which also installs the build as the supervisor; `RELOAD=1`, `claude-route socket-on|socket-off` and `rollback-laptop-peer.sh` refuse a supervised job without it. A build without `owned_by_supervisor` is refused as a supervised worker, because it would unlink the socket under its successor, and the activation refuses it too.

§69.3 Supervision, the worker path, the control socket and the ready timeout come from the job `launchctl print` shows, never the plist alone. So does the binary every script replaces: a plain serve's program, a supervised job's `--worker-bin`. `SUBROUTER_BIN` once moved it, and it is the npm and Python wrappers' setting: a deploy installed to that path, restarted a job that ran `~/bin/subrouter`, and printed DEPLOY OK on the old build. `rollback-laptop-peer.sh` also refuses a backup plist whose program is not that binary. A plist that disagrees with the loaded job (a run that wrote it and stopped before the restart) is refused, with the command that finishes the move. `deploy-laptop.sh`, `rollback-laptop-peer.sh`, `activate-laptop-supervisor.sh` and `claude-route` share this through `laptop-deploy-lib.sh` (`job_state`, `shape_conflict`).

§69.4 One `launchd_blocker` covers §47 and the GUI-session rule. A run that restarts the job needs `managername` to print `Aqua` and `print` to answer 0 or 113. A supervised upgrade restarts nothing, so it needs only `print` to answer and runs from ssh. A bootstrap launchd refuses rolls back at once, with no health wait.

§69.5 Every run but `--check` and `--dry-run` takes `<plist>.supervisor-mutation.lock` through the repo's `deploy/macos/mutation-lease-lib.sh`, the lock upstream's scripts and `sr update` take. A run that changes the job passes its refusals in the calling shell, then goes on in a detached process (double fork, setsid, HUP, INT and TERM ignored, the lock taken again inside) that logs to `~/.subrouter/transitions/<script>-<stamp>-<pid>.{log,rc,pid}`; the shell streams the log and exits with the run's code. The activation and both rollbacks boot the job out and wait for 113 before they write a binary or a plist. `deploy-laptop.sh` installs first (stage, sign, rename, so the running process keeps its own file) and makes its plist edit, then kickstarts, or boots out, waits for 113 and bootstraps; this section used to say every restart boots out first, and the docs now match that code. An activation with `--wait-quiet` checks the turns again just before its bootout, because the baseline in between takes up to about 95 s.

§69.6 `claude-route` moved from a copy in `~/bin` to `ops/bin/claude-route`. It finds `deploy-laptop.sh` and `srq` from its real path (`CLAUDE_ROUTE_DEPLOY`, `CLAUDE_ROUTE_SRQ`) and writes the same shims as before. `socket-on` and `socket-off` hand the plist edit to `deploy-laptop.sh` (`PLIST_SET_ENV`, `PLIST_DELETE_ENV`), which makes it under its lock, backs the plist up beside it as `<plist>.bak-<stamp>`, and puts it back on a rollback. claude-route once backed up and edited the plist without the lock: when the deploy then refused because another run held it, claude-route put its stale backup over what that run wrote. The `--restart-supervisor` it passes comes from the same job reading as the consent, and the deploy refuses a supervised job without it, so an activation that finishes in between cannot turn the reload into a supervisor restart nobody agreed to. It never sets or clears `CLAUDE_CODE_MAX_RETRIES`, which lives in `~/.claude/settings.json`.

§69.7 Verified with `ops/deploy/tests/deploy-scripts-test.sh` (the `sup-`, `act-`, `route-` and §69 `laptop-` and `peer-rollback-` cases, all in `requiredCases`) and `ops/deploy/tests/rehearse-laptop-supervisor.sh` against a scratch LaunchAgent with real builds: activation, a supervised deploy with streams open over TCP and the Claude socket, the refusals, `claude-route socket-off` and `socket-on` through `deploy-laptop.sh`, the rollback, and a SIGKILLed calling shell during an activation, a supervised upgrade and a rollback. Not done: the activation still cuts what streams when launchd stops the plain daemon; `--peer-tailnet` is not validated here (the daemon ignores a malformed value with a Warn).

- Knobs: `SUBROUTER_LABEL`, `SUBROUTER_PLIST`, `SUBROUTER_ADDR`, `SUBROUTER_CONTROL_SOCKET` (paths, for a rehearsal), `--restart-supervisor`, `--peer-tailnet`, `--wait-quiet`, `--dry-run`, `--rollback`, `PLIST_SET_ENV`, `PLIST_DELETE_ENV`, `CLAUDE_ROUTE_DEPLOY`, `CLAUDE_ROUTE_SRQ`
- Code: `ops/deploy/macos/activate-laptop-supervisor.sh`, `ops/deploy/macos/deploy-laptop.sh`, `ops/deploy/macos/rollback-laptop-peer.sh`, `ops/deploy/macos/laptop-deploy-lib.sh` (`job_state`, `launchd_blocker`, `take_mutation_lock`, `run_detached`, `bootout_and_wait`), `ops/bin/claude-route`
- Related: builds on §47 (launchd reachability) and §38 (the drain socket). Needs a build of this repo, which gives the supervisor the Claude socket (`owned_by_supervisor` in the build, `claude_socket` in supervisor-status). A build of upstream subrouter is refused with nothing changed.

## 70. The admission limit is read from admission.json, tunes itself, and logs each minute

The admission gate (upstream #589) ran at most 6 requests per (provider, account, model) and
queued 64 more, as compile-time constants. Owner, 2026-11-19: "6 feels kind of tight,
honestly. Maybe it's not, but I want to start tracking this, and I want to make that number
something that we can change dynamically." That day Opus on `owner` ran 6 with 28 to 54
waiting for most of the day: 1,778 queue-full 503s, and 40 to 120 s mean time to first byte.
Other model families on the same account stayed fast. One 19-agent workflow peaked at 9
requests in flight.

**The file.** `<state dir>/admission.json` is the directory of the session store:
`~/.subrouter/admission.json` on the laptop, `/var/lib/subrouter/admission.json` on a box.
The daemon reads it at start and every 2 s after, and compares content, not mtime. No file
means the defaults below. An unreadable or invalid file keeps the limits in force and logs one
error per distinct content. Each change logs `admission limits changed` with `old` and `new`.
Every field is optional:

```json
{"limit": 6, "queue": 64, "spill_queue": 8, "closed_queue": 16,
 "per_model": {"opus": 12}, "per_account": {"claw-1": 30},
 "tune": {"on": true, "min": 4, "max": 24}}
```

`per_account` beats `per_model`, which beats `limit`. A `per_model` key goes through
`claudePoolModel`, so `opus` names `claude-opus`. An absent `closed_queue` is a quarter of
`queue`. `spill_queue` 0 never spills and `closed_queue` 0 never closes. Unknown fields and
text after the object are errors. A daemon that starts on an invalid file has no last good
values, so it runs the defaults: read the log after a restart. A `per_model` key other than a
Claude family is matched exactly, case included, for every provider. A tenant pool's gate
ignores the file. A raise grants queued requests at once, in FIFO order. A lower limit only stops new
grants until `active` falls under it. A lower queue turns away only new arrivals. Nothing
already queued is dropped or reordered. `/_subrouter/traffic` shows the values in force under
`admission`, with `per_account` keyed by account hash.

**Self-tuning.** Owner, 2026-11-19: "I want self-tuning", and "I am genuinely convinced that
we should be raising this to 12". Each Claude bucket
keeps its own limit. Every 30 s, a bucket where a request waited at the full limit, or was
refused for a full queue there, rises by one, unless a transient 429 came in the last 2 min.
Transient is any Claude 429 not marked `rejected` for the account or the model's window: the
headerless "Server is temporarily limiting requests", and also a per-minute rate limit on a
Claude API-key account, which tunes too. A queue behind a pause or its 1, 2, 4 recovery ramp does not count. A
transient 429 lowers the limit by a quarter, at least 1, at most once every 10 s. The limit
stays between `tune.min` and `tune.max`, widened to take in the configured limit, so
`per_account: 30` is its bucket's ceiling. Each move logs `admission limit raised` or
`admission limit lowered` with `from` and `to`. A file change restarts tuning only for
buckets whose configured limit changed, and clamps the others into the new bounds. Codex and
the keyed providers keep their configured limit. A restart starts every bucket again from
its configured limit. Only the gate `StartAdmission` runs tunes, since only its clock raises a
limit; a tenant pool's gate keeps its configured limit.

**Closed buckets.** Owner, 2026-11-19: if the queue is "over like a quarter of the way to the
cap, we just say no more ... This one will not take more". A bucket with `closed_queue` or
more waiting is closed: `admissionControl.closed(key)`, and `closed: true` in traffic. Routing
does not read it yet, so new and warm sessions keep queueing and spilling as before.

**Minute lines.** Once a minute, each bucket busy in that minute logs `admission minute`:
`limit`, `effective_limit`, `active`, `queued`, `peak_active`, `peak_queued`, `peak_demand`
(running plus waiting at its busiest), `served`, `waited`, `wait_avg_ms`, `wait_max_ms`,
`spills_out`, `spills_in`, `queue_full`, `pauses` (pauses begun) and `abandoned` (clients that
gave up while queued). The five sessions that
fanned out furthest log `admission minute session` with `session_prefix`, `peak`, `active`,
`queued` and `account`. The daemon log keeps them across restarts. A minute whose
`peak_demand` is at most N, with `queue_full=0` and `spills_out=0`, is one a limit of N would
have run with no wait. With tuning on, read `limit` too: it is the limit that minute ran.
`grep 'admission minute ' | grep -E 'peak_demand=(7|8|9|10|11|12) ' | grep 'queue_full=0' |
grep 'spills_out=0'` lists the minutes a limit of 12 would have absorbed whole.

On 2026-11-19 the box ran a 2026-11-17 binary, from before the upstream merge, with no gate at
all. Its first build from main brings the gate, and this file, with it.

- Knobs: `<state dir>/admission.json` (absent: 6/64/8/16, tuning on from 4 to 24)
- Code: `internal/proxy/admission_limits.go` (`parseAdmissionLimits`, `setLimits`, `closed`, `admissionLimitsFile`, `AccountRef.StartAdmission`), `internal/proxy/admission_tune.go` (`tune`, `backoff`), `internal/proxy/admission_minute.go` (`rollMinute`, `logMinute`, `runAdmission`), `internal/proxy/admission.go`, `cmd/subrouter/main.go`

## Verification

The full suite matched a pristine `93bef02` worktree's pass/fail set (the two baseline failures in KNOWN-ISSUES.md are upstream and unrelated), and `make docker-test` passed. Regression guards were mutation-tested; two mutants first slipped past test shortcuts, so the tests now assert through production paths.

## Upstreaming

§1 to §4 are worth sending upstream: §2 to §4 are plain bugs, and §1 is a product default that may not be accepted, isolated in `resolveSetupCredentialSource` so it can be dropped alone. §9 is the most valuable: any multi-provider pool hits it, and it looks like a quota problem. §10 is our own spending preference and should probably stay local.

## 71. A subagent's Fable turn on a Fable-spent pinned account is served there as Opus, and the session stays

A session is held on one account, and its subagents share its session id, so one hold covers all of them. When a subagent asked for Fable on a held account whose Fable meter was spent but whose Opus pool still served, the box refused the targeted turn (409, `exhausted`), the hold ended, and the turn was re-ranked onto an account with Fable left. The main thread's next turn then followed the session there and rebuilt its whole cache, and its Opus turns spent the Fable account's week. Measured on 2026-11-20: a Sonnet main thread on a Fable-spent box account, one Fable subagent turn, and the session moved to the other box. The owner's rule: "Ideally, it doesn't switch. It just pins to Opus. The only thing that forces you to move to a Fable box is if you're switching the main model from Opus [to] Fable."

§71.1 A subagent turn is a Claude `/v1/messages` POST with a non-empty `X-Claude-Code-Agent-Id`; Claude Code sends it on every subagent request and never on the main thread's. A main-thread Fable turn, a side request, a forced or targeted account, a lease and a `X-Subrouter-No-Retry` canary are unchanged.

§71.2 Held on a peer: before sending, when the peer's cached `claudefable` verdict for the held account says exhausted and its Opus pool is not evicted, the body's model is rewritten to Opus (`claudeOpusBody`, the `[1m]` suffix kept) and sent targeted at the same account. After a refusal with reason `exhausted`, the turn is resent once as Opus to the same target, and only the hold its own refusal ended is put back. A second refusal, or a refusal that ended nothing because another turn already moved the session, takes the forced switch as before.

§71.3 Pinned to a local account (a warm session, no peer hold) whose Fable pool is exhausted and whose Opus pool is not evicted: the request is rewritten to Opus before selection, so selection keeps the session on its account on the Opus pool. A cold session is re-ranked as before, on Fable.

§71.4 A turn in flight no longer undoes another turn's move. A refusal ends the session's hold only if the hold still names the refused account, and an answered turn sets the hold only if it still names the account that served it (or names none, from an older peer). Before this, any turn answered on the old account after the main thread moved put the hold back there, so the next main turn bounced off it with a 409. That was already true on main for any targeted turn; §71.2 would have made it common.

§71.5 Opus does not read a Fable thinking block: per Anthropic's model-migration docs the API drops it from the prompt unbilled, so the Opus turn runs without the earlier Fable reasoning. Not tested against the API.

§71.6 Not done: when the local pin breaks after the rewrite (a credential found dead at refresh, an Opus 429 and usage failover), the turn moves as Opus though the new account may have Fable, and the `local_pin` line names the old account. The pre-send check trusts a verdict up to `remotePoolHoldVerdictMaxAge` (60 s), so a subagent can get Opus for up to a minute after the box's Fable refills. The local-path log line is written when the rewrite is decided, before the upstream answers.

- Log: `claude subagent fable served as opus on pinned account` (`path` = `pre_send`, `after_refusal` or `local_pin`), `claude subagent fable as opus not served on pinned account`, and an INFO line when a served turn leaves a hold another turn moved.
- Code: `internal/proxy/claude_subagent_fable.go`, `serveHeldTurn` and `serveHeldAttempt` (`remote_pool_decide.go`), the guarded hold clear and set in `forwardToPeer` (`remote_pool.go`), the handler's local-pin call (`proxy.go`). Tests: `internal/proxy/subagent_fable_pin_test.go`.
- Related: §65's Fable-spent preference, on by default, steers Opus sessions onto Fable-spent accounts and so makes §71's case the usual one. Since §64 the Opus a subagent's turn is served as is `claude-opus-5-5`.

## 72. A burndown account behind pace takes more sessions, each account caps its refugees by its week, and a full queue takes no new session

Owner, 2026-11-19: "No token should be wasted ... we want to be as token efficient as possible,
and we want to be using everything." An account that rolls its week with headroom left has
wasted that headroom. An account that runs dry before its roll strands every session on it, and
each one pays a full cache rewrite to move. That day the laptop's own account went from 0% to
96% of its week in about 9 hours. A box account had 31% left 2.5 hours before its roll, with 4
sessions allowed and 1 live.

**The paced allowance.** Inside the burndown window (the 24 hours before the roll, or a reset
holder's early burndown, §56) the session allowance is no longer a fixed taper of at most 4.
The owning daemon computes it from the account's measured burn:

```
needed  = spendable / hours to the roll / burn per live session
allowed = max(taper, min(ceil(needed), ceiling))   when ceil(needed) > taper
allowed = taper                                    otherwise
```

`spendable` and the taper are what they were (§41, §55): weekly headroom above the owner's
reserve, and ceil(spendable / 0.0625) between 1 and 4. The 5h brake still holds an account
whose 5h window cannot sustain more sessions at 1. `ceiling` is 20 by default
(`SUBROUTER_PACED_SESSION_CEILING`), counted over one account's live sessions on all its
models, since the live count is keyed by provider and account. Outside the burndown window
nothing new caps an ordinary session.

Owner, 2026-11-20, after a box account rolled with 17% of its week unspent while the taper held
it at 3 sessions: "this is fucking stupid it needs to count time left as well." So the raise
applies whenever the time left needs more sessions than the taper allows, not only above 4. An
account with no measured burn yet uses 0.015 of a week per live session-hour, the middle of the
2026-11-19 figures, instead of falling back to the taper. At 17% with 1.5 hours left that is
0.17 / 1.5 / 0.015 = 7.56, so 8, not 3. The default needs a `Fresh` reading, since a restored
seed or a reading whose fetch keeps failing may be hours old. It also needs a 5h window: with
no 5h timing the 5h brake cannot act, and in the endgame sim's s5 (no `five_hour` bucket) the
default took 6.50 forced moves per drain over 24 seeds, past §62's accepted 5.0. Such an account
keeps the taper until its burn is measured. A measured burn still raises it.

The allowance is a target, not a switch. The brief proposed raising it only while the projected
waste at the current burn was above a threshold. A throwaway simulation of both (one account,
Poisson session arrivals of 8 or 30 an hour lasting 45 minutes or 2 hours, 1.5 points per
session-hour, a 1-hour burn window) showed the switch snapping back to 4 as soon as the account
caught up, so it rolled with about the threshold unspent: 4 to 5 points in every high-demand
case. The target left 0 to 2.9 points.

Owner: lowering the allowance "just does not allow new sessions on." It feeds the drain tier,
the at-cap demotion, `new_session.eligible` and the targeted 409, and nothing else. `Evicted`
and `CapacityEvicting` stay the only ways a warm session leaves an account.
`TestLoweringThePacedAllowanceMovesNoWarmSession` holds eleven live sessions at a raised cap of
11, lowers it to 4, and checks that every warm turn is served where it was.

**Backpressure.** A Claude 429 that is not a quota rejection means Anthropic is pushing back,
and more sessions would only wait: it holds the account at its taper for 15 minutes. A wait in
our own admission queue is not backpressure: the gate tunes itself up (§70), and a real backlog
closes the account to new sessions (below).

**Measurement.** `SchedulerRef` keeps a burn tracker per local Claude account, fed by every
fresh usage reading it publishes and by a 30-second sampler of the sessions with a request the
admission gate has admitted. A request waiting in the queue spends nothing, so it is not live
here; counting it would read the burn per session low and raise an account whose gate is
already full. Over a 2-hour window it reads the burn per hour and the burn per live
session-hour. It needs at least 30 minutes of readings and half a session-hour, and otherwise
borrows the burn per session-hour of this daemon's other accounts, and otherwise uses the
default above. The tracker lives in memory, so after a restart every account runs on the
default until 30 minutes of readings exist. Each change to an account's
allowance logs `paced allowance` with its inputs: `needed`, `burn_per_live`, `burn_source`,
`burn_now`, `spendable`, `hours_to_roll`, `projected_waste`, `danger` (projected waste above
`SUBROUTER_PACE_WASTE_THRESHOLD`) and `live_sessions`. In this patch `danger` gates nothing.

**The refugee cap.** Owner: "If we have one account and it takes 20 refugees, they immediately
drop it to zero, and then they switch twice ... We basically forced a double move when a single
move could have done." A refugee should go dark (close, or let its cache lapse) before it
drains the account it moved to. Each account's cap on warm refugees is now the owner's formula,
in points of its week left above its reserve:

```
normal:   floor(points / 10)
burndown: max(normal, floor(points * 24 / (5 * hours to the roll)))
```

At 24 hours that is points / 5; at 12 hours with 20 points left it is 8. The formula assumes
a refugee spends about 5% of a week over about 20 hours, with a 2x margin outside burndown. It
has no floor, so an account with 9 points left takes none. It has no ceiling: near the roll the
owner wants the account spent, and the admission gate still limits requests in flight. The cap
reads the meter as last measured, without this daemon's per-request debit, which on a reserved
account can read a busy half-minute 0.36 of a week low.

The owner computes the cap and publishes it as `new_session.refugee_cap` for the account and
for each quota pool. The Fable pool's cap reads its binding meter, the lower of the Fable meter
and the week. A peer places refugees up to the published cap; a daemon from before this patch
publishes none, which reads as 4. The count stays one per account across models. Every read of
the cap goes through `refugeeSlotsFor(peer, account, poolModel)`. To calibrate the 5% and 20 hours,
each refugee that goes dark logs `refugee went dark` once, with its lifetime, its account's
points at first sight and now, the account's average live sessions over the stay, and
`est_session_burn_pct`.

**A full queue takes no new session.** Owner (§70): once an account's admission queue on any
model holds `closed_queue` waiters, "this one will not take more." The account takes no new
session and no refugee until the queue drains below it:
- its verdict is `queue_closed`, not eligible, never evicted, with a refugee cap of 0;
- a peer's targeted new session gets 409 `queue_closed`;
- local selection refuses it outright, with no fallback onto it, and a session with nowhere
  else to go gets the pool-exhausted 503;
- a failed-over session, or an admission spill that carries a move, never lands on it. A spill
  after a Claude transient 429 carries no move, since that failover keeps the pin.

Its warm sessions stay, queue, and still spill one request at a time to another account as
before. A session idle past the warm window moves to an open account that outranks it, since
its cache is cold anyway; when no open account outranks it, it stays and queues. The closure
is account-wide, because one Claude Code session runs several models. The scheduler reads it
live on every build, so it opens and closes with the queue. `closed_queue: 0` in
`admission.json` turns it off without a restart; `SUBROUTER_PACED_ALLOWANCE` does not.

**One in-flight tracker for both listeners.** The private and public listeners each normalized
their own `Server` copy and so kept separate `ActiveSessions`. A box's public listener, the one
peers read and target, counted only peer-forwarded turns in its live sessions, its taper and
its targeted at-cap refusal. Its own clients, who come in through the private listener and
multi-tenant routes, were invisible to it. `AccountRef` now shares one tracker the way it shares
the admission gate.

**The trade.** `TestPacedAllowanceTradeTable` (`-paced-trade-table`, skipped otherwise) runs
every endgame sim scenario under the taper and five forms of the raise: the default burn all
drain with ceil, nearest or floor; and the true burn measured after 30 minutes with ceil or
nearest. It prices a forced move at 0.05 points. Over 24 seeds the shipped raise spends a net
7.9 ± 0.65 points more than the taper across the 13 rows (1.7 on the sim's own ten). The five
forms differ from each other by at most 0.35 points. It is not free in moves: on s1b at the
central rates every form adds 4.5 to 7.6 forced moves per drain, all at the weekly or 5h wall,
which is past the 4.0 the owner accepted for §55. No new bound guards that: it is the owner's
call. `TestEndgameSimulation` keeps §55, §60 and §62 priced with the raise off.
`TestThePacedAllowanceSpendsWhatTheTaperForfeits` checks that the raise cuts the forfeit at the
roll. On an account with a 5h meter, where a 5h window is a sixth of the week as at the sim's
central rates, no allowance can spend 17% in the last 3 hours: the 5h window walls first. The
account that rolled with 17% unspent on 2026-11-20 had no 5h meter, only its weekly windows. Demand
and the taper held it back, and that is the case a measured burn raises.

Owner, 2026-11-20, asked whether an account with no 5h meter may be raised on its measured
burn, at 5.6 forced moves per drain against the 5.0 accepted: "yes". The account that rolled
with 17% unspent was that case. The s1b moves past §55's 4.0 are the same trade on an account
that has a 5h meter, and are taken as accepted with it. The owner gave no answer on raising
before the last day, so the raise stays inside the burndown window.

Owner, 2026-11-20, on the paced ceiling: "should be 20" / "to start". The default is 20 in code,
so every daemon takes it with no env edit, and `SUBROUTER_PACED_SESSION_CEILING` still sets 1
to 64. The 6 it replaces came from unverified community reports of a throttle at about 6
concurrent sessions; that day one box account ran 18 to 23 Opus requests at once with no 429s.
A non-quota 429 still holds the account at 4 for 15 minutes.

At ceiling 20 the trade table moves only on the table-only s6 rows. The shipped raise nets 8.9
± 0.80 points over the taper across the 13 rows, against the 7.9 ± 0.65 accepted at 6, and 1.00
± 0.30 more paired by seed. At the central rates s6b forfeits 3.71 points instead of 4.39 for
7.58 forced moves per drain instead of 6.21, and s6a 16.19 instead of 16.71 for 7.71 instead of
5.29, the added moves all at the 5h wall. The sim's ten rows, s1b and s5 included, and the §55,
§60 and §62 guards are unchanged to the digit: no new session reaches the draining account
while it holds 6 or more live sessions in burndown, so demand bounds them, not the ceiling. No
row reaches 20; the most live sessions on the draining account is 14.

Still open for the owner, each chosen by the implementer and flagged:
- the refugee cap rounds down in both modes;
- it reads points above the owner's reserve;
- in burndown it takes the larger of the two forms, so a reset holder days out never drops below normal;
- the queue closure is account-wide rather than per model;
- the default burn of 0.015 per live session-hour;
- the raise stays inside the burndown window.

- Knobs: `SUBROUTER_PACED_ALLOWANCE` (unset is on; 0 keeps the taper of at most 4 and 4 refugees an account, and leaves the closed queue on), `SUBROUTER_PACED_SESSION_CEILING` (1 to 64, default 20, live sessions per account across all models), `SUBROUTER_PACE_WASTE_THRESHOLD` (points of the week, default 5; the `danger` log field only), `closed_queue` in `admission.json` (§70; 0 turns the closed queue off, live)
- Code: `selectacct/pace.go` (`allowance`, `refugeeCap`, `paceTracker`), `selectacct/new_session_verdict.go` (`RefugeeCap`, `VerdictQueueClosed`), `selectacct/scheduler.go`, `selectacct/scheduler_ref.go`, `internal/proxy/pace_sampler.go`, `internal/proxy/refugee_pace.go`, `internal/proxy/refugee_dark.go`, `internal/proxy/admission.go` (`noteBackpressure`, `closedAccounts`), `internal/proxy/refugee.go` (`refugeeSlotsFor`), `internal/proxy/proxy.go` (`sharedActiveSessions`, `selectionSchedulerFrom`), `cmd/subrouter/main.go`

## 73. A Claude login through `sr` lands in the profile's own credential store, and `sr` checks it there

`sr claude addr <name>` (also `login` and `add --oauth`) could save the login outside the
profile and still print `Added Claude profile`. The profile then had no credential (`has no
access token`), and `sr claude move` had nothing to move. Seen on the laptop in pool mode.

Claude Code (2.1.329, measured and read from its source) names its credential store after
`CLAUDE_SECURESTORAGE_CONFIG_DIR` when it is set, ahead of `CLAUDE_CONFIG_DIR`: the Keychain
item `Claude Code-credentials-<first 8 hex of sha256(dir)>` on macOS, `<dir>/.credentials.json`
elsewhere. An empty value names the bare `Claude Code-credentials` item, the user's own login.
In pool mode the claude-route shim exports it for every launch, at its login-less store
(`~/.claude-route/securestorage`). `sr` runs the `claude` on PATH, which is the shim, so the
login went to the shim's store. `sr` then asked `claude auth status`, which through the shim
answers `loggedIn: true` for any profile, from the shim's placeholder token. `sr` also passed
an inherited `CLAUDE_SECURESTORAGE_CONFIG_DIR` to every Claude it started, so `sr` run inside a
pooled session did the same without the shim.

The rules:
- `CLAUDE_SECURESTORAGE_CONFIG_DIR` is a routing key (`claude.RoutingEnvKeys`). It is stripped
  from every Claude child's environment and dropped from the user's settings and `--settings`.
  A managed profile's launch settings set it to the profile's directory, which names the item
  `CLAUDE_CONFIG_DIR` alone names. A direct launch sets it empty, which with no
  `CLAUDE_CONFIG_DIR` is the same default store. It is never empty beside a profile.
- An OAuth add's verdict is the profile's own credential, read through the store. It must hold
  an access token, and on a re-auth not the one the profile held before. `claude auth status`
  only names the account and its plan. When the store holds no new credential, the add says the
  login was saved outside the profile's store and names `CLAUDE_SECURESTORAGE_CONFIG_DIR` or a
  `claude` wrapper on PATH as the likely cause. It rolls back a profile it created, and a
  profile that already existed keeps what it held. When the store cannot be read
  (`ErrCredentialUnchecked` or any other read error), nothing is rolled back, since the login
  may be in the store that did not answer, and the kept profile is published so the daemon
  reloads it. A re-auth reads the profile's old credential before the browser step. When that
  read fails, the re-auth stops there, since a login saved elsewhere would leave the old token
  looking new. The
  older TUI login path (Claude builds without `claude auth login`) waits for the same new
  token before it closes Claude.
- The shim gives a credential command (`$1` is `auth`, `login`, `logout`, `setup-token` or
  `/login`) the route `credentials`. It undoes what a parent shim set (`CLAUDE_ROUTE_SET`),
  drops the rest of the route as `off` does, and keeps `CLAUDE_CONFIG_DIR` even inside
  `~/.subrouter`. It beats the mode file, `CLAUDE_ROUTE` and a route already in the environment,
  asks the daemon nothing, and is logged and printed by `CLAUDE_ROUTE_DRYRUN=1` like any other
  route. A route word still comes first. Every other launch is unchanged. Installed shims take
  it at the next `claude-route install`.
- Because that route keeps a pool profile's directory, `claude auth logout` (or `claude logout`)
  with `CLAUDE_CONFIG_DIR` or `CLAUDE_CODE_CONFIG_DIR` inside `~/.subrouter` is refused: it would
  sign the pool's account out. Your own login's logout runs with neither set.
- `claude-route status` says whether the shim's login-less store holds a login (`pool store:`).
  A login there is one an older shim routed to it. It must be moved to its profile before
  the item is deleted, since it may be the only copy.

Not done: `sr claude run`'s preflight still asks `claude auth status`. With both fixes it reads
the profile's own login through the shim, and a wrong answer there ends in a launch that shows
it is not logged in, not in a lost login.

- Code: `internal/agents/claude/store.go` (`claudeRoutingEnvKeys`), `cmd/subrouter/sr_claude.go`
  (`addOAuth`, `claudeLaunchSettingsJSON`), `ops/bin/claude-route` (the shim's `credentials`
  route, `drop_subrouter`)
- Tests: `TestClaudeAddLandsInTheProfileStoreDespiteAnInheritedSecureStorageDir`,
  `TestClaudeAddFailsWhenTheLoginLandsInAnotherStore`,
  `TestProxyClaudeLaunchSettingsNeutralizeHostilePersistedRouting`,
  `TestClaudeReauthStopsWhenTheProfilesCredentialCannotBeRead`,
  `TestClaudeReauthThroughTheTUIWaitsForTheNewLogin`,
  `TestClaudeAddKeptAfterAnUnreadableCredentialIsPublished`
  (`cmd/subrouter/sr_claude_test.go`), and the `route-login-`, `route-every-credential-command-`,
  `route-logout-of-a-pool-profile-` and `route-plain-pool-launch-` cases in
  `ops/deploy/tests/deploy-scripts-test.sh`

## 74. CI runs the full suite only when someone starts it

Every push to a pull request and every merge ran the unit tests, five cross-compiles and the Docker
profiles: nine jobs a push, for a project whose only deployments are its owner's laptop and boxes. A
change is already tested where it is built (`deploy/docker/test.sh`), and a deploy is checked by
`deploy-laptop.sh` against the live routes. A bad merge costs a fix, not an outage, and one flaky test
on main no longer stops every other change from landing. The owner: "we were probably doing way too
much CI".

§74.1 `ci.yml` takes `workflow_dispatch`. The classifier reports `code=false` for every other event, so
a pull request, a merge group and a push to main run "Public check" and the "Build, vet, test" gate,
and the gate passes on the skipped jobs as it does for a docs-only change. A pull request and a
push to main also run the board page's node tests and `srq selftest`, in a job the gate does not wait
on. Start the full suite with `gh workflow run ci.yml --ref <branch>`; there the classifier runs
everything.

Tests made steady, so that a red run on main means something. Each failed by chance with no change to the code it tests. The fix removes the cause; no wait is lengthened and no assertion is retried.
- `TestACodexFollowUpReadsTheUsageAndTheCreditListAtOnce` (§102): it compared a credit's date with `heldCodexCredits(...)` called again at the assertion, a date computed from the clock to the second, so it failed when a second turned between the fixture and the assertion (7 of 200 runs under `-race`). It now takes the date from the credits its fixture holds: 0 of 200 since.
- `TestAClaimByHandAndACodexRedeemLeaveTheirBlockOnTheRow` (codex, §102): the same cause, seen once under `-race` in §102's check, and the same fix: 0 of 200 since.
- `TestANonRetiredGenerationKeepsItsIdleWebSocket`: "no 1012 close on the retired generation's idle socket within 10s". The dial returns at the 101, and the handler arms its Retire watch only after the session commit. A Retire in that gap made the socket count as opened after Retire, which owes its first turn and is never closed idle (10 of 500 runs under `-race` on one processor held busy; 20 of 20 with the handler stalled 50 ms after the upgrade). The test now waits until the relay reads the socket (`readSocketOnceRelayed`, as the fresh socket of `TestRetiringClosesAnIdleCodexWebSocketWith1012` already does): 0 of 500 since on the same busy processor, and 20 of 20 with the same stall.
- `TestUsageStatusCarriesEachResetsExpiryWindow` (§94): the box's window is open, so it opens at the box's clock, truncated to the second, and lasts as long as is left to the credit's date. The box computes it each time it serves a body, so its own read and the body it served the laptop a moment later differed by a second (`opens_at`) or by one in `lead_seconds` when a second or a half second turned between them (16 of 500 runs). The box now serves on a fixed clock, as the laptop already did: 0 of 500 since.

§74.2 To gate merges again once the project has users, delete the classifier's `workflow_dispatch`
early exit.

- Code: `.github/workflows/ci.yml` (`on.workflow_dispatch`, the `changes` job's first lines)

## 75. A dual-stack MPTCP listener is read as dual-stack, so a legacy handoff on Linux keeps it

Go 1.24 and later listen with MPTCP wherever the kernel offers it. On kernels such as 6.8, an MPTCP
socket answers `getsockopt(IPPROTO_IPV6, IPV6_V6ONLY)` with `EOPNOTSUPP`. The coverage check
(`listenerIPv6WildcardAcceptsIPv4`) took any error for "IPv6-only". So on such a Linux host it judged
every dual-stack `[::]` listener unable to serve an IPv4 `0.0.0.0` configuration. Takeover, listener
transfer and front handoff then refused the legacy dual-stack listener that older builds made by
listening on network `tcp`. An exact-address handoff and macOS never reached this path.

§75.1 On `EOPNOTSUPP`, and only then, the flag is read from the kernel's TCP listener table over
`NETLINK_SOCK_DIAG` (attribute `INET_DIAG_SKV6ONLY`, which `ss -e` also reads). The entry is matched by
the socket's inode from `fstat`. The MPTCP subflow listener reports the parent socket's inode.

§75.2 It fails safe. A missing entry, a missing attribute or any netlink error leaves the check at "not
covered", which is the same refusal as before. Other Unix systems get a stub that returns `EOPNOTSUPP`,
so their behaviour is unchanged.

§75.3 Tests. `TestListenerCoverageReadsDualStackCapabilityOfMultipathListeners` builds dual-stack and
IPv6-only listeners explicitly as MPTCP. It skips only when the kernel hands back plain TCP. The
takeover tests skip where `pidfd_getfd` is refused for the calling process itself, as under Docker's
default seccomp profile, because a refusal there can only mean the system call is blocked.

- Code: `cmd/subrouter/listener_address_linux.go`, `cmd/subrouter/listener_address_unix.go`

## 76. The deploy-script tests never address the live job, and the laptop scripts change only a job loaded from their own plist

On 2026-11-21 a stopped test run unloaded the developer's live service, and nothing loaded it again for over two hours. An agent ran plain `go test ./...` in `ops/` on the Mac, which runs `deploy-scripts-test.sh`, and stopped the run while a case was in flight. The test fenced the scripts only with stubs on `PATH` (`launchctl`, `curl`, `sleep` and the rest) in a temporary dir with a fake home. It gave them no label, so they addressed `gui/<uid>/ai.manaflow.subrouter`, the live job. The test's exit trap deleted the dir. One case had started `deploy-laptop.sh` as a detached run, which by design outlives its caller and ignores SIGTERM (§69). With its stubs gone, its `PATH` reached `/bin/launchctl`: `kickstart -k`, then `bootout`, then `bootstrap` of the fake plist, which no longer existed. A booted-out job has no definition left in launchd, so `KeepAlive` had nothing to restart.

§76.1 The laptop scripts change launchd only for a job they can load again. `own_job_or_exit` in `laptop-deploy-lib.sh` reads `launchctl print` and exits 1, naming both paths, when launchd loaded the label from a plist other than the script's `$PLIST`. A bootout of a loaded job also needs `$PLIST` to exist. `kickstart_job`, `bootout_and_wait` and `bootstrap_job` call it before their `launchctl` call. They are the only places the scripts kickstart, boot out or bootstrap. `TestLaptopScriptsChangeLaunchdOnlyThroughTheGuard` reads every script in `ops/deploy/macos` and fails on a call to `launchctl` outside them that changes a job, in the spellings §76.6 lists. It does not see a call through a variable, such as `"$LAUNCHCTL" bootout`, or a call in a script outside that directory. `take_mutation_lock` calls `own_job_or_exit` first, so a mismatch found at the start, as for a run with a home that is not the job's, stops the script before it changes a file. `bootstrap_job` does not bootstrap a missing `$PLIST`. It returns a failure instead of exiting, because launchd already holds no job there, so the caller's recovery and its DOWN message still run. A print that launchd refuses proves nothing either way, and `launchd_blocker` already refuses on it.

§76.2 Every case runs its scripts with the label `test.subrouter.deploytests.<pid>` and the address `127.0.0.1:31497`, as the rehearsal already did for its own label. `run_sandboxed` sets `SUBROUTER_LABEL`, `SUBROUTER_ADDR` and `CLAUDE_ROUTE_TCP`, and the fake plists carry the label in their file name and `Label`. `finish` fails a case whose stubs recorded `ai.manaflow.subrouter` or `:31415`. `sr-triage` reads the same two variables; it had the live label and port written in. The fake launchd now prints `path = <plist>` for a loaded job, as launchd does.

§76.3 No detached run outlives the test. On its exit, and on HUP, INT and TERM, the test finds each detached run of its case that has no exit code yet. It stops that run's process group, kills the group with SIGKILL, and only then removes the dir. The stop comes first because a script waiting on its lease helper would carry on if the helper died first. After a signal it looks again half a second later, for a run that wrote its pid late. `TestStoppedHarnessKillsItsDetachedRuns` parks `laptop-ok`'s detached run in its kickstart (`DEPLOY_TESTS_HOLD=1`), stops the test's process group with each of the three signals, and checks that the run's process group and the test dir are gone.

§76.4 On macOS, `TestDeployScripts` and `TestStoppedHarnessKillsItsDetachedRuns` refuse to run outside a sandbox that refuses exec of `launchctl`, and the message names the safe commands. Two sandboxes count: the test's own (`DEPLOY_TESTS_SANDBOX=1`), and `sr-local-ci`'s, which now passes `SR_LOCAL_CI_SANDBOX=1` to the deploy run because a `sandbox-exec` inside its sandbox would be refused. On Linux, CI and Docker run the cases as before. `AGENTS.md` and the Test section of `ops/README.md` no longer send anyone to plain `go test` here. Since §76.6 the harness refuses too, and checks that the sandbox a variable names is in force.

§76.5 Checkouts made before this change keep the old test. A job that something else boots out stays out unless the watchdog of §81 is installed.

§76.6 An independent check of this section passed it and named small gaps. These are now closed.

The harness refuses on macOS by itself, not only through `go test`. `uname -s` decides, before any case runs, in a single case run and in the run of every case. With neither `DEPLOY_TESTS_SANDBOX=1` nor `SR_LOCAL_CI_SANDBOX=1` it exits 2 and names the safe commands. A variable is not taken as proof. With `DEPLOY_TESTS_SANDBOX=1` the harness needs `sandbox-exec` on `PATH`, and runs `/bin/launchctl version` inside it with the profile the cases get. With `SR_LOCAL_CI_SANDBOX=1` it runs `/bin/launchctl version` as it is. Either way it goes on only when that exec fails with "Operation not permitted", the EPERM a sandbox gives. A `launchctl` that runs, or fails in any other way, is a refusal. If the sandbox lets it run, `launchctl version` changes nothing. The harness also calls `sandbox-exec` by the path it found, because `env -i` looks it up in a case's `PATH`. The usage lines at the top of the harness, the Test section of `ops/deploy/lightsail/README.md`, and the help for `sr-local-ci --no-sandbox` now give only safe commands, or say that the cases refuse.

A laptop deploy that `own_job_or_exit` refuses at its restart no longer leaves the new build on disk. Step 3 has installed it by then, while launchd still runs the build from before, so the next start of the job would have run a build nothing verified. `own_job_or_exit` now calls the function a script names in `UNDO_ON_REFUSAL` before it exits, and `deploy-laptop.sh` names `put_back_on_refusal` from step 4 on. It puts each file this run changed back from its backup: the binary, the supervisor's for `--restart-supervisor`, and the plist for a plist edit. A file that stays as the run left it is named, with the `cp -p` and `mv -f` that put it back. `activate-laptop-supervisor.sh` had the same gap: a refusal at its step 4 left the supervised plist, the worker config and both builds in place. From its step 2 until its bootout returns, it names `undo_on_refusal`, which puts them back as `undo_files` does for any failure before the restart. After that the job has been booted out, and a refusal exits as before. `rollback-laptop-peer.sh` changes no file before its bootout and names no function.

`TestLaptopScriptsChangeLaunchdOnlyThroughTheGuard` counts as a call `launchctl`, by name or by path, where a command starts: at the start of a line, after `;`, `&`, `|`, `(`, `)`, `{`, `}`, `!`, a backquote or `$(`, or after `if`, `elif`, `while`, `until`, `then`, `do` or `else`. It may stand behind `command`, `exec`, `env`, `nohup`, `nice`, `time`, `sudo` or `timeout` with their options, each option with or without a separate value, plain or quoted, as in `timeout -s KILL 10`, `env -u HOME` or `sudo -u "$USER"`. It may also stand behind variable assignments, with the value plain or quoted, or with the whole assignment quoted as in `env "PATH=/bin"`, and behind `launchctl asuser <uid>` or `launchctl bsexec <pid>`. A line that ends in a backslash goes on into the next. The verbs are `kickstart`, `bootout`, `bootstrap`, `load`, `unload`, `remove`, `kill`, `stop`, `start`, `enable`, `disable`, `submit` and `reboot`. `TestLaunchdChangeScanCatchesEverySpelling` holds 42 spellings, and lines that must not count, such as a hint the scripts print. The first pattern missed 27 of the first 32. The second missed 5 of the 6 added after it, each a wrapper option with a separate value.

The harness's cleanup signals a process group only when it can show the group is a detached run of this test. The pid in the run's `.pid` file must be over 1 and still have the test dir in its arguments, and its group must not be 0, 1 or the harness's own. Before, a pid file with 0 named the harness's own group, and one with 1 sent `kill -STOP -1`, which reaches every process the user owns. A run that died could also leave its pid to another process.

`scripts-default-to-the-live-job` covers the default label and address again. Every other case hands the scripts a label and address of the test's own, so none had run a script on its defaults. This case runs no script. It evaluates each `${SUBROUTER_LABEL:-...}`, `${SUBROUTER_ADDR:-...}` and `${CLAUDE_ROUTE_TCP:-...}` in the laptop scripts, `sr-triage` and `claude-route` in a shell with the variable unset, and checks that each script has the ones it uses.

`TestStoppedHarnessKillsItsDetachedRuns` waited at most 5 minutes for `laptop-ok`'s detached run to park in its kickstart. At a load near 60 two of its subtests ran out of time in a full run and passed alone. It now waits while any file under its temporary dir changes, and gives up after 3 minutes with no change. It proves what it proved before.

A second independent check found three more gaps, now closed. The first is the scan's gap with option values, above. The second: `TestDeployHarnessRefusesAMacOutsideASandbox` failed three subtests under the Docker command in `ops/README.md`, because that command runs as the host's uid, which has no passwd entry in the image. With `DEPLOY_TESTS_SANDBOX=1` the harness refuses a home it cannot resolve before it reaches the check that these subtests test. They now stand in for `id -un` with `root`, which every Linux resolves. The third: nothing tested the help for `sr-local-ci --no-sandbox`. `TestNoSandboxHelpSaysTheDeployCasesRefuseOnMacOS` in `ops/localci` runs `sr-local-ci --help` and expects that option's sentences that say the deploy cases refuse on macOS and name `--docker`. Each failed before its change: the second pattern missed the five spellings, the three subtests failed with `cannot resolve this user's home (got '~501' ...)`, and the help test fails when the sentences are taken out.

A third check named three spellings the scan still missed, though no script uses them: an assignment quoted whole as an argument of `env`, `launchctl asuser 501 launchctl bootout`, and `launchctl reboot`. The scan now takes them, and the pattern before failed the four spellings added for them. The scan does not read quotes, so it takes `echo "(launchctl bootout x)"` for a call. That mistake fails the test; it never lets a call pass. It also found that of the files `put_back_on_refusal` puts back, only the binary was tested. `laptop-refusal-puts-the-edited-plist-back` refuses a plain serve's restart after a plist edit and expects the plist as it was. `sup-restart-supervisor-refusal-puts-both-builds-back` refuses a `--restart-supervisor` at its bootout and expects both builds as they were. Each fails when its part of `put_back_on_refusal` is taken out.

Not closed: `own_job_or_exit` reads `launchctl print` and then acts, so a job that changes between the two is not caught. That is a limit of §76.1 on its own; §76.2 and §76.3 keep a test run off the live job.

There is nothing to turn off. On Linux the harness runs as before.

Tested in Docker. `TestDeployHarnessRefusesAMacOutsideASandbox` stands in for macOS on Linux: a `uname` that prints Darwin, and a `sandbox-exec` that refuses `/bin/launchctl` or runs it. It expects exit 2, the reason and the safe commands, and no case started, for each refusal, and a case that passes through the stand-in when it refuses. `laptop-stops-before-kickstarting-a-job-from-another-plist` and `laptop-reload-refused-without-its-plist` now check that the build on disk is the old one. `laptop-refusal-that-cannot-put-the-build-back-says-how` fails the restore's signature as well, expects the new build on disk, the line that names it and the exact command, and runs that command. `act-refusal-at-the-restart-puts-the-files-back` moves the job after the activation's step 2 and expects the plist and the build from before, and no supervisor build left. `harness-cleanup-signals-only-its-own-runs` makes `kill` a function that records its arguments, plants pid files for a run, for another process, for a process in the harness's group, and for 0, 1 and a word, and expects only the run's group. `TestHeldWaitFollowsTheRunsProgress` checks the wait on a slow run that keeps going, on one that stops, and on a harness that ends first. Before these changes, every one of them failed except `scripts-default-to-the-live-job`, which fails when a default has a typo, and the two Go tests of test code, which fail with the first pattern and with a fixed deadline. The old cleanup would have signalled the groups of the run, the other process and the harness, and `-1`. Not run: anything on macOS, so neither the real `sandbox-exec` nor sr-local-ci's sandbox answered the check.

- Code: `ops/deploy/macos/laptop-deploy-lib.sh` (`own_job_or_exit`, `kickstart_job`, `bootout_and_wait`, `bootstrap_job`, `take_mutation_lock`), `ops/deploy/macos/deploy-laptop.sh` (`restart`, `put_back_on_refusal`), `ops/deploy/macos/activate-laptop-supervisor.sh` (`undo_on_refusal`), `ops/bin/sr-triage`, `ops/bin/sr-local-ci`
- Tests: `ops/deploy/tests/deploy-scripts-test.sh` (`sandbox_profile`, `refuse_unfenced`, `run_sandboxed`, `finish`, `run_group`, `cleanup`, and the cases listed under §76 and §76.6 in `requiredCases`), `ops/deploy/tests/deploy_scripts_test.go`, `ops/localci/localci_test.go`
- Docs: `AGENTS.md`, `ops/README.md`, `ops/deploy/lightsail/README.md`

## 77. The supervisor keeps its Claude socket, replaces a worker that stops answering, and lets go of the socket on purpose

On 2026-11-21 the laptop's daemon was gone for more than two hours, and every Claude Code session on
the Mac failed with it while the boxes were healthy. A session in socket mode has one endpoint, the
socket path, and no TCP fallback (§30). The plan is that another process stands in on that path while
the daemon is away and sends turns to a box. That works only if the supervisor takes the path back
afterwards, and it did not. It opened the socket once, at start. A lock held at that moment left it at
`unavailable` until the next restart, and a restart drops every connection. For the same reason a
deleted socket path stayed deleted while supervisor-status said `serving`.

§77.1 One goroutine, the keeper (`socketKeeper`), now owns the socket for as long as the supervisor or a
plain `serve` runs. Without the socket's lock (the flock on `<path>.lock`) it asks for the lock every
250 ms. With the lock it checks the path every 2 s. Each look ends in one word, reported as
`claude_socket` in supervisor-status and, under a plain `serve`, in `/_subrouter/health`.

- `waiting` means that it does not hold the lock and its last try to bind the path failed. The lock
  is held elsewhere, or a listener answers at the path, or the bind failed for another reason. It
  keeps trying until it stops, and it binds the path as soon as a try succeeds.
- `serving` means that it holds the lock and its own socket is at the path. If the path is missing,
  or holds a dead socket, it binds the path again under the lock it still holds and adds one to
  `claude_socket_rebinds`. It never releases the lock to do so, so a process that waits on the lock
  does not get it. Connections accepted before the path went keep working.
- `displaced` means that it holds the lock and a socket it does not own answers at the path. It
  never removes a socket that answers. It checks again every 2 s and binds the path once nothing
  answers there.
- `unavailable` under a held lock means that it could not bind the path again, for example because
  a regular file is there. It removes only a mode-0600 socket of this user, as at start (§30), and
  it tries again every 2 s.
- `unavailable` from start means that the path is relative, too long, or in a folder that is not
  canonical and private. Nothing is tried again.
- `stopped` still means that the accept loop ended.

When the socket's folder is replaced by another at the same path, the lock in the old folder guards a
socket no client can dial. The keeper closes that socket, releases that lock and starts over in the
new folder.

Supervisor-status gains `claude_socket_waits_for_lock: true` and `claude_socket_rebinds`, the number of
times the path was bound after the first. A process that stands in on the path must see the first
field before it takes the lock, because a supervisor without it never takes the path back. A
supervisor with no Claude socket reports neither field. The words and the two field names are fixed.

An accept loop that ended used to keep the path and the lock. Clients queued on a socket nobody
accepted, and nothing else could take the path. The socket is now closed when its accept loop ends,
so the path is unlinked and the lock is free before the word becomes `stopped`.

The supervisor's accept loops (`front.Router.Serve`) also ended on any accept error. `EMFILE`,
`ENFILE` and `ECONNABORTED` say nothing about the listener, so the loop now retries them on the same
listener after a pause that doubles from 5 ms to 1 s, and logs one line for each run of failures.
On the TCP listener such an error used to end the supervisor, and with it every connection it
carried. `subrouter front` uses the same loop.

A socket that opens at start and whose path nobody touches behaves as before. The keeper adds one
`fstatat` and one `stat` every 2 s.

- Knobs: none. The keeper runs whenever `SUBROUTER_CLAUDE_SOCKET` (or `--unix-socket`) names a
  socket. To be without it, run a build from before this section as the supervisor.
- Code: `cmd/subrouter/claude_socket_keeper.go` (`socketKeeper`, `leasedListener`),
  `cmd/subrouter/claude_socket.go` (`start`, `servedSocketParent`),
  `cmd/subrouter/local_data_listener_unix.go` (`pathState`, `rebind`, `retire`),
  `cmd/subrouter/supervisor.go` (supervisor-status), `internal/front/router.go` (`Serve`)
- Tests, in Docker (`./deploy/docker/test.sh ./cmd/subrouter ./internal/front`), each seen failing with
  its part of the change taken out: `TestSupervisorRecreatesDeletedClaudeSocket` (a stream opened
  before the delete finishes, a waiter blocked on the lock never gets it, and the lock is held
  through the same open file after the delete as before it),
  `TestSupervisorReplacesADeadSocketAtTheClaudeSocketPath`,
  `TestSupervisorTakesClaudeSocketWhenLeaseFrees`,
  `TestSupervisorWaitsWhileAnotherProcessListensAtTheClaudeSocket`,
  `TestSupervisorNeverTriesAClaudeSocketInAnUnsafeFolder` (a folder its group or anyone can write
  to, and a folder that is a symlink: `unavailable`, and still unserved once the folder is put
  right), `TestSupervisorDoesNotStealALiveForeignSocket`,
  `TestSupervisorLeavesAnUnsafeEntryAtTheClaudeSocketPathAlone`,
  `TestSupervisorTakesTheClaudeSocketAgainWhenItsFolderIsReplaced`,
  `TestSupervisorStatusSaysItWaitsForTheClaudeSocketLock`,
  `TestSupervisorShutsDownCleanlyWhileWaitingForTheClaudeSocket`,
  `TestSupervisorShutsDownCleanlyWhileDisplacedFromTheClaudeSocket`,
  `TestClaudeSocketAsksForItsLockAboutEvery250Milliseconds`,
  `TestClaudeSocketPathIsBackAboutTwoSecondsAfterItIsDeleted`,
  `TestClaudeSocketKeepsAcceptingAfterTemporaryError`,
  `TestClaudeSocketGivesUpThePathWhenItStopsServing`, `TestClaudeSocketSecondOwnerIsSoft`,
  `TestRouterKeepsAcceptingAfterTemporaryAcceptErrors` and
  `TestRouterPausesBetweenTriesWhileAcceptKeepsFailing` (at most 20 tries in 400 ms of `EMFILE`).
  The two tests of the 250 ms and the 2 s judge the shortest of several waits, and fail below
  half the time or above twice the time. A loaded machine makes a wait longer and never shorter,
  so load does not fail them. A waiter blocked in `flock` is woken when the holder lets go, and
  is not handed the lock, so a holder that takes the lock again at once can beat the waiter to
  it. The check of the open file cannot be beaten that way. With the path bound again by letting
  go and opening again, the waiter caught 4 runs of 10 and the check of the open file caught the
  other 6. Not tested on macOS. The tests run on Linux only, and the laptop's supervisor picks the
  change up at its next restart.
- Related: builds on §30 (the socket and its lock) and §69 (the supervisor owns the socket). The
  activation still requires `serving`. The deploy's probe asks through the socket and prints the
  word only when nothing answers there.

§77.2 The supervisor replaced a worker that exited (`monitorWorker`) and did nothing about a worker
that stayed alive and stopped answering. A Claude Code session on such a worker used to fail after
about 13 minutes. With the client's longer wait it sends the turn again every 10 minutes with no end
(measured on 2026-11-21 with Claude Code 2.1.330), so a silent worker has to be replaced.

The supervisor now probes its active worker. Every 15 s it sends `GET /_subrouter/ready` to the
worker's private socket and waits up to 10 s. Any HTTP answer counts, a 503 included, because an
initial worker may report unready for as long as it runs and still serve every request. No answer
means a timeout or a failed connection, so a worker whose private socket was deleted is replaced
too. After 3 probes in a row with no answer, the supervisor starts a new generation from
`--worker-bin` the way `POST /_subrouter/upgrade` does, and retires the silent worker the way a
deploy does. New connections go to the new worker. What the silent worker still sends is left to
finish, and the supervisor stops that worker once it holds no connection. By these numbers a worker
is replaced 30 to 75 s after it stops answering, plus the time the new worker takes to report
ready.

A request that already waits on the silent worker is not moved. Its client gives up by its own
timer and sends the request again on a new connection, which reaches the new worker. Claude Code
2.1.330 does that 183 s after it sent a request that got no response headers.

A worker that is stopped or stuck never closes a connection. The router (`front.Router`) kept a
connection for as long as the worker's side stayed open, even after the client had gone. Such a
worker never went idle, so the supervisor never stopped it. The router now drops a connection 15
minutes after its client ended its side if the worker has not ended its own (`workerCloseGrace`).
A worker that runs ends its side at once, because net/http drops the request when it reads the
client's end. The limit is long so that a client that has only half-closed can still read a
stream. A stopped worker is therefore killed at the latest 15 minutes and `--worker-stop-grace`
after its last client left. A deploy over a stuck worker gets the same limit.

The misses belong to one worker. An answer sets the count to 0, and so does a switch to another
worker. The supervisor skips a round in three cases:

- An upgrade, a rollout step or a recovery holds the upgrade lock.
- A canary runs.
- The supervisor is shutting down.

A probe tick that arrives more than 10 s late also sets the count to 0. A Mac that sleeps stops
every process, so the tick after it wakes says nothing about the worker, and neither do the misses
from before. The supervisor reads the lateness from the wall clock, because Go's monotonic clock
stops while a Mac sleeps (the runtime reads `mach_absolute_time`).

When the replacement cannot be made, the silent worker stays. That happens under an inhibit marker,
and when the new worker never reports ready. The supervisor does not exit, because the silent
worker may still be sending streams and an exit drops every connection. It tries again after the
next missed probe and reports the reason.

Supervisor-status gains `worker_probe`.

- `enabled`, `interval_seconds`, `timeout_seconds` and `misses_to_replace` give the settings.
- `misses` is the number of probes in a row that the active worker has not answered.
- `replace_error` is present while a replacement cannot be made.
- `last_replacement` has `at`, `reason`, `from` and `to`, where `from` and `to` are generation ids.

With the probe off, `worker_probe` is `{"enabled": false, "misses": 0}`.

- Knobs: `--worker-probe-interval` (15 s, and `0` turns the probe off), `--worker-probe-timeout`
  (10 s) and `--worker-probe-misses` (3). They are supervisor flags whose defaults are in the code,
  so the plist does not change, and the probe starts at the supervisor's next restart.
- Code: `cmd/subrouter/supervisor_worker_probe.go` (`runWorkerProbe`, `probeActiveWorker`,
  `probeTargetLocked`, `replaceSilentWorker`), `cmd/subrouter/supervisor.go` (the flags,
  `readyStatus`, supervisor-status), `internal/front/router.go` (`workerCloseGrace`)
- Tests, in Docker (`./deploy/docker/test.sh ./cmd/subrouter -run 'TestSupervisor.*(Probe|Unresponsive|Unready|Misses|SilentWorker|StoppedWorker|WhoseSocketIsGone)|WorkerProbeDefaults'`),
  each seen failing with its part of the change taken out. They run a real supervisor over real
  worker processes, and a worker is silenced while it stays alive and keeps accepting connections.
  `TestSupervisorReplacesUnresponsiveWorker` runs on real time. It checks that another worker
  answers on the port, that a stream the silent worker was sending finishes, and that the silent
  worker's process is gone afterwards. The others move an injected clock one tick at a time:
  `TestSupervisorKeepsAWorkerThatAnswersUnready`, `TestSupervisorDoesNotReadALateProbeTickAsAHang`,
  `TestSupervisorReplacesAWorkerWhoseSocketIsGone`, `TestSupervisorReplacesAndEndsAStoppedWorker`
  (a worker under `SIGSTOP` is replaced, and killed once `--worker-stop-grace` has passed),
  `TestSupervisorForgetsMissesOnceTheWorkerAnswers`, `TestSupervisorCountsMissesPerWorker`,
  `TestSupervisorDoesNotReplaceAWorkerItDidNotProbe`, `TestSupervisorSkipsAProbeRound` (the lock,
  a canary, a shutdown), `TestSupervisorKeepsRunningWhenTheSilentWorkerCannotBeReplaced`,
  `TestSupervisorWorkerProbeIsOffAtIntervalZero` and
  `TestParseSupervisorConfigWorkerProbeDefaultsAndLimits`. In `./internal/front`,
  `TestRouterReleasesAClientThatLeftAWorkerThatNeverCloses` sets the router's limit to 500 ms. No
  test runs the 15 minutes with a real supervisor, because the stopped worker in the supervisor
  test holds no connection. A real sleep is not tested. The late-tick test moves the injected clock, and no
  test shows that the wall clock of a Mac jumps over its sleep while Go's monotonic clock does
  not. Not tested on macOS.
- Related: builds on §69 (the supervisor and its upgrade path). The probe asks the route that
  gates every new generation, and it ignores whether the answer says ready.

§77.3 A process that stands in on the socket path serves only while the supervisor does not hold the
socket's lock, and the supervisor held the lock for as long as it ran. Two cases need the supervisor
to let go while it keeps running. The first is the drill. Without one, the stand-in can be tried on
the live job only by breaking the live job. The second is a worker that reports ready and exits
again and again. The supervisor replaces such a worker each time (`monitorWorker`), so the
supervisor never exits and its lock is never free, and every stream on the worker is cut at each
exit.

The keeper can now yield. It closes its listener, unlinks its socket and releases the lock, and it
does not ask for them until the yield's time is up. `claude_socket` then reads `yielded`, and
`claude_socket_yield` gives the `reason` and `until`. Connections that the supervisor already
accepted keep working, and TCP is unaffected. When the time is up, the word becomes `waiting` and
the supervisor takes the path as §77.1 describes: at once when the lock is free, and otherwise when
the holder lets go. Each bind after a yield adds one to `claude_socket_rebinds`. A yield is kept in
memory only, so a supervisor that restarts serves its socket. Both ways of yielding write one WARN
line with the `reason` and `until`.

An operator yields with two calls on the supervisor's control socket.

- `POST /_subrouter/claude-socket/yield?for=<time>` yields for a stated time such as `2m`, with
  the reason `operator`. The time is a Go duration above 0 and at most `60m`. Anything else gets
  400 and changes nothing. A call during a yield states the time again.
- `POST /_subrouter/claude-socket/reclaim` ends a yield early, whatever its reason. With nothing
  yielded it changes nothing.

Both calls answer 200 with the socket's fields of supervisor-status as they stand after the call:
`claude_socket`, `claude_socket_yield` while yielded, `claude_socket_waits_for_lock`,
`claude_socket_rebinds` and `claude_socket_exit_guard`. The yield call answers after the lock is
free, so a process that waits on the lock has it by then. The reclaim call answers `serving` when
the supervisor took the path at once, and `waiting` when another process holds the lock. A
supervisor that keeps no Claude socket answers 409 to both.

```sh
curl -s --unix-socket ~/.subrouter/supervisor.sock -X POST 'http://supervisor/_subrouter/claude-socket/yield?for=2m'
# {"claude_socket":"yielded", ..., "claude_socket_yield":{"reason":"operator","until":"2026-11-21T12:02:00Z"}}
curl -s --unix-socket ~/.subrouter/supervisor.sock -X POST http://supervisor/_subrouter/claude-socket/reclaim
```

The supervisor yields by itself on the third failure of an active worker within 10 minutes, with
the reason `worker_exits`. A failure is an exit of the active worker, or an active worker that the
probe found silent and set out to replace (§77.2). The supervisor lets go before it starts the
replacement, because a process standing by serves the path sooner than a new worker reports ready.
It asks for the path only after 90 seconds in which the active worker stayed up and answered every
probe. A missed probe or another failure starts the 90 seconds again, and `until` moves with them.
A return is a yield for worker exits that ended because its time was up or an operator reclaimed
the socket. A yield that begins within 10 minutes of the last return doubles the time, up to 15
minutes. The times are 90 seconds, then 3, 6, 12 and 15 minutes. A yield that begins later starts
at 90 seconds again. With the probe off, only an exit starts the time again.

On a Mac with no stand-in, a yield leaves socket sessions with nothing to connect to. So 5 seconds
into a yield for worker exits, the supervisor tries the path once. If another process holds the
lock, the try changes nothing and the yield goes on. If the lock is free, nobody stands by. The
try has then taken the path back, and the supervisor keeps the socket through later worker
failures. It yields for worker exits again only after it has seen another process hold the lock,
and it sees that whenever its own try at the lock fails for that reason. An operator's yield never
asks and keeps its full time. So after a stand-in is installed beside a running supervisor that
found nobody, one operator's yield shows the supervisor the stand-in.

The operator has the last word. An operator's yield replaces a yield for worker exits, and worker
failures do not change an operator's yield. The times are wall-clock times, so the time a Mac
sleeps counts toward each of them.

A process that stands in must do three things for this to work. It takes the lock within 5 seconds
of the lock becoming free. It holds the lock for as long as it listens at the path. It lets go
after `claude_socket` reads `waiting`.

Supervisor-status gains `claude_socket_yield` while yielded, and `claude_socket_exit_guard`.

- `claude_socket_yield.reason` is `operator` or `worker_exits`. `until` is the time at which the
  supervisor asks for the path again, in UTC with whole seconds. In a yield for worker exits it is
  the earliest such time.
- `claude_socket_exit_guard.enabled`, `exits_to_yield` and `window_seconds` give the settings.
- `recent_exits` is the number of worker failures in the last 10 minutes.
- `standing_by` is `unknown` at start, `seen` once another process held the lock, and `nobody`
  after a yield for worker exits in which nobody took the lock.
- `return_after_seconds` is the time the worker had to stay steady in the last yield for worker
  exits, and 90 before the first.

With the guard off, `claude_socket_exit_guard` has only `enabled`, `recent_exits` and
`standing_by`. A supervisor with no Claude socket reports neither field. The word `yielded`, the two
reasons and the field name `claude_socket_yield` are fixed.

- Knobs: `--claude-socket-yield-exits` (3, and `0` turns the yield for worker exits off). The
  operator's two calls work either way. It is a supervisor flag whose default is in the code, so
  the plist does not change, and the behaviour starts at the supervisor's next restart.
- Code: `cmd/subrouter/claude_socket_yield.go` (`socketYield`, `exitGuard`, `lookWhileYielded`,
  `yieldFor`, `reclaim`, `workerFailed`, `workerFaltered`, the two routes),
  `cmd/subrouter/claude_socket_keeper.go` (`keep`, `look`, `take`), `cmd/subrouter/claude_socket.go`
  (the status), `cmd/subrouter/supervisor.go` (the flag, `monitorWorker`),
  `cmd/subrouter/supervisor_worker_probe.go` (`probeActiveWorker`, `replaceSilentWorker`),
  `cmd/subrouter/local_data_listener_unix.go` (`socketLockHeldElsewhere`)
- Tests, in Docker (`./deploy/docker/test.sh ./cmd/subrouter -run 'TestSupervisor.*(Yield|ClaudeSocketBack|StandsBy|StayedSteady|ReturnThatFailed|LastTenMinutes|GuardOff|LetGoOfTheClaudeSocket|OperatorCalls|FoundSilent)|ClaudeSocketYieldExits'`),
  each seen failing with its part of the change taken out. They run a real supervisor over real
  worker processes, kill the active worker, and stand by for the lock with a goroutine that
  blocks in `flock` and keeps the lock once it has it. The supervisor reads an injected clock, so
  the tests move 5 seconds, 90 seconds and 10 minutes at will.
  `TestSupervisorYieldsTheClaudeSocketToAnOperator` takes the lock as soon as the yield call has
  answered, sees a stream opened before the call finish, and sees the path come back after the
  stated time. `TestSupervisorTakesTheClaudeSocketBackWhenAnOperatorEndsTheYield`,
  `TestSupervisorRefusesAYieldWithoutAStatedTimeOfAtMostAnHour` and
  `TestSupervisorWithoutAClaudeSocketHasNothingToYield` cover the two calls.
  `TestSupervisorYieldsTheClaudeSocketOnTheThirdWorkerExit` sees the process standing by get the
  lock at the third exit, and `waiting` and then `serving` after 90 seconds of answered probes.
  `TestSupervisorAsksForTheClaudeSocketOnlyAfterTheWorkerStayedSteady` (a missed probe, an exit),
  `TestSupervisorDoublesTheWaitAfterAReturnThatFailed` (90 seconds up to 15 minutes, and back),
  `TestSupervisorTakesTheClaudeSocketBackWhenNobodyStandsBy` (the 5 seconds, no second yield, and
  a yield again once the lock was seen held),
  `TestSupervisorCountsOnlyWorkerExitsOfTheLastTenMinutes`,
  `TestSupervisorCountsAWorkerItFoundSilentWithTheExits`,
  `TestSupervisorKeepsTheClaudeSocketThroughWorkerExitsWithTheGuardOff`,
  `TestSupervisorOperatorCallsOverrideAYieldForWorkerExits`,
  `TestSupervisorLogsWhyItLetGoOfTheClaudeSocket`, `TestSupervisorForgetsAYieldWhenItRestarts` and
  `TestParseSupervisorConfigClaudeSocketYieldExits`. No stand-in process exists yet, so no test
  runs one. A real sleep is not tested. Not tested on macOS.
- Related: builds on §77.1 (the keeper and the `waiting` state) and §77.2 (the probe). The deploy
  scripts do not know `yielded` yet. The activation requires `serving`, so it fails during a
  yield. A deploy during a yield reports the socket down and prints the word, unless something
  answers at the path.

## 78. An end client is told to wait at most a minute in the daemon's own 503s

Two 503s that the daemon writes itself carry a long `Retry-After`. The spent pool's answer
(`no non-exhausted claude accounts available; next account frees up in <human> (retry
after <N>s)`) sends N, the seconds until an account frees, which can be hours. The stranded
give-up (§67.5) sends 3600. Both were set for a Claude Code that fails a turn on a
`Retry-After` over 60 s, so that the user saw the answer at once. Since 2026-11-21 the
laptop's sessions run with `CLAUDE_CODE_RETRY_WATCHDOG=1`, toward the owner's goal that
sessions and workflows survive the laptop daemon going away. Such a session sleeps the
whole `Retry-After` of a 503 before it tries again (measured with 120 s). These two answers
became an hour or more of silence in a session that could have carried on a minute after
the pool or its upstream came back.

§78.1 An end client is now told `Retry-After: min(N, 60)` in both answers. A wait under a
minute is sent as it is. The body keeps the real N, because cmux parses "(retry after
<N>s)" (`pool_exhausted.go`). A session with the watchdog tries again each minute, and the
turn goes where there is room. A session without it retries a `Retry-After` of 60 s or
less on its counted retries, where it used to show the answer at once. The give-up still
ends the chat's chain of holds and unlinks it or leaves it waiting (§67.5), so the turn a
minute later is placed afresh.

§78.2 Who is an end client. Any caller whose request has no `X-Pool-Hop`: a Claude Code
session on the socket or on TCP, and a caller with a membership token. A peer daemon's
forward carries `X-Pool-Hop` of 1 or more (both forward paths set it) and gets the whole N,
as before. The daemon that forwarded the turn reads only the status of the peer's 503
(`classifyAccountSwitch`: a 429 or a 503 is `peer_declined`), then answers its own client
with its own 503, capped there. The hold never runs on a peer's forward, so the give-up
never reaches a peer.

§78.3 Turning it off. `SUBROUTER_CLIENT_RETRY_AFTER_MAX` is a Go duration, default `60s`.
`0` turns the cap off, and every client gets the whole wait, as before this entry. It has
no flag, for the reason in §58. An unparseable value is logged and the default kept. At
start serve logs `Retry-After cap is on` with `retry_after_max`, or `Retry-After cap is
off`. A value over 60 s makes a client without the watchdog show the answer at once again.
The change is in the worker only, so a laptop deploy swaps it in without dropping a
connection. To turn it off on the laptop, set the variable in the plist and redeploy with
`RELOAD=1`, as in §58.

§78.4 What it costs. Under the watchdog each retry of a 503 counts against Claude Code's
300. A pool spent for longer than about 5 hours (300 times 60 s) now fails the turn after
that, where a long `Retry-After` slept up to 6 hours per retry. Not covered: a hosted
broker's `Retry-After` in team mode, which is relayed as before, and the Codex capacity
503, which already sends 1 to 60 s.

§78.5 Tests (`internal/proxy/client_retry_after_test.go`). The handler's spent pool 503: a
wait over the cap, a wait under it, a cap of 90 s, the cap off, and a peer's forward. The
credit balance's 503, which the transport makes. Two daemons: the laptop forwards a turn
to a box whose pool is spent, the box answers the laptop with the whole N, and the
laptop's client gets 60. The hold's give-up and a waiting chat's give-up at once: 60 with
the cap and 3600 with it off. With `clientRetryAfter` reverted to send the whole wait,
every capped case fails (`Retry-After = "2820", want "60"`) and the cap-off, short-wait and
peer cases pass. With the peer check removed, the peer cases fail. In `cmd/subrouter`,
`TestResolveClientRetryAfterMax` covers the variable. `TestServeRunsWithTheRetryAfterCap`
runs serve itself, which logs its start line from the field of the Server it built. With
the cap's line taken out of serve's Server literal, the default and 90 s cases fail
(`lacks "Retry-After cap is on"`) because the daemon would run with the cap off.

- Code: `internal/proxy/client_retry_after.go` (`clientRetryAfter`),
  `internal/proxy/pool_exhausted.go`, `internal/proxy/hold.go`, `internal/proxy/refugee.go`,
  `cmd/subrouter/hold_config.go` (`resolveClientRetryAfterMax`, `logClientRetryAfterMax`).
- Related: §58 (the hold), §67.5 (the give-up).

## 79. The stand-in: while the laptop's daemon is gone, Claude Code's socket is answered from a box

On 2026-11-21 the laptop daemon's launchd job was unloaded for 2 h 22 min. Every socket session on the Mac
failed once its retries ran out, and 242 workflow agents died, while both boxes were healthy. A Claude Code
session reaches one endpoint for its whole life, the socket path it was launched with (§30), so only a
process on the Mac that answers that path can carry a session to a box. The stand-in is that process. It
is built in parts: §79.1 sends one request to a box, §79.2 chooses the box, §79.3 is the process,
`subrouter standby`, that takes the socket path and gives it back, §79.4 keeps the daemon's own
`policy_limits` answer for the sessions that start while the stand-in serves, and §79.5 installs the process as
a launchd job of its own. Until someone runs that install, nothing starts it, and no behaviour of the daemon
changes.

§79.1 The box handler (`internal/standby`) is a plain `http.Handler` for a request as it arrives on the
Claude socket. It removes the client's `Authorization` (Claude Code's `ssh-placeholder`), `X-Api-Key`,
`X-Pool-Hop` and every `X-Subrouter-*` header (`session.StripSubrouterHeaders`, then any other header with
that prefix). It sets `Authorization: Bearer <the box's stand-in token>`, `X-Subrouter-Agent: claude`
(without it a box assumes Codex) and `X-Subrouter-Session` to `session.ExtractRoutingID` of the client's
request. `X-Claude-Code-Session-Id`, the `anthropic-*` headers and the rest pass as the client sent them.
These are the peer forward's rules (`forwardToPeer`) without a hop count or a target account, so a box
treats the request as one from a session of its own, and a session the box already holds keeps its account
and its prompt cache.

Bodies stream both ways through `httputil.ReverseProxy`. Events reach the client as the box writes them, and
a request body goes to the box as the client sends it, so a 20 MB body costs one copy buffer and nothing is
kept for a resend. A request moves to the next box only while no request head has been written to any box
(`httptrace` `WroteHeaders`): a refused dial, a timeout or a certificate that fails its pin moves on. A
failure after that point, in the body or after the first byte of the answer, is the client's to retry, and
the client sees an answer cut mid-stream as cut, never as a clean end. The transport speaks HTTP/2 (both
boxes offer `h2`), so when many sessions move at once they share one connection per box. A connection that
has received nothing for 15 s is pinged, and closed when the ping goes unanswered for 5 s. So a connection
that falls silent (the Mac slept, or a gateway dropped its state) costs the sessions on it about 20 s, not
the header bound, and each gets `standby_box_failed` to retry. Dial and TLS take at most 2 s each, and an
answer's headers at most 150 s. The peer link's 30 s header bound is short because
the peer link can fall back to local accounts. The stand-in cannot, and 150 s stays inside Claude Code's
180 s first-byte limit. A stream's length is never bounded.

The handler runs inside the daemon's own guard (`proxy.NewClaudeSocketGuard`,
`internal/proxy/claude_socket_guard.go`). A box's 401, or its 403 that says the token was revoked, reaches
the client as a 503, because either would make Claude Code drop its socket credential for the login stored
on the Mac. Every Retry-After the client sees is at most 60 s. A longer one, or one that is not a whole
number of seconds, becomes 30, and a 503 without one, the guard's included, gets 30. Without the client's
retry watchdog, Claude Code fails a request at once on a 503 that asks for more than 60 s. With it, a
session sleeps the whole value, so a box's `Retry-After: 3600` would park a session for an hour.

One function, `routeFor`, decides what never goes to a box. `/_subrouter/health` gets the stand-in's own
body, `{"ok":true,"claude_socket":"standby"}`. `claude-route` reads only `"ok": true`, so a new launch still
takes the pool route, and `claude_socket` tells a deploy check that the daemon does not own the socket. The
daemon's other `/_subrouter/` routes get a 503. `/api/*`, the client's account calls, gets a 503 with
Retry-After 30: a box answers them with its own identity account, and the client keeps whichever identity
answered (`internal/proxy/control_plane.go`). The one exception is `GET /api/claude_code/policy_limits`,
which goes to a box when the stand-in holds no copy of the daemon's own answer under a day old (§79.4). A
live session served by a box on 2026-11-21 showed why. With a 503 there, a session
that starts during the outage withholds WebFetch, DesignSync and RemoteTrigger, ignores the Retry-After, and
does not ask again for an hour, so the tools stay gone after the daemon returns. With the box's 200 it keeps
them, and nothing of the box's identity reached a file under its config folder. The box's body was the same
as the daemon's, because both identities had the default policy. A box whose identity belonged to another
organization would apply that organization's policy. The path is cleaned before it is read, so
`/v1/../api/...` counts as `/api`. A request that names an account (`X-Subrouter-Account-ID` or
`X-Subrouter-Account`, set by `claude <account>`) gets a 503 and is never sent, `policy_limits` included,
because a pinned launch is promised that account and no other. Only §79.4's copy, when the stand-in holds one
under a day old, answers a pinned launch's `policy_limits`. With no box usable, the client gets a 503
with Retry-After 30 at once, and nothing is parked. Every 503 the handler makes has Anthropic's `api_error`
shape, which Claude Code retries like any server error, and names its cause in `X-Subrouter-Reason`:
`standby_no_box`, `standby_box_unreachable`, `standby_box_failed`, `standby_pinned_account`,
`standby_control_plane` or `standby_daemon_surface`.

A `Box` exists only through `NewBox`. It takes an `https://host:port` URL, the stand-in token and the 64-hex
pin of the box's certificate, and builds its transport on `proxy.PinnedTLSConfig`. A certificate that is not
the pin ends the handshake before a byte of the request is written, so the token never reaches it. An
`http://` URL, a missing pin and a malformed one are refused. The token is never logged.

The same session accepted a replay of the daemon's own `policy_limits` answer, even with its signature
header removed. §79.4 keeps that answer and replays it ahead of a box's. Turning it off: only
`subrouter standby` (§79.3) runs the handler, and stopping that process turns it off.

§79.2 The box set (`Set`, `internal/standby/boxset.go`) decides which boxes a request may go to, and in
which order. It reads only files the daemon already keeps, and it writes nothing to the state folder.

Each box's name, URL and certificate pin come from the daemon's peers file (`SUBROUTER_REMOTE_POOLS_FILE`),
read by the daemon's own loader (`proxy.LoadRemotePeersFile`), so a file the daemon refuses is refused here
too. The tokens in that file are the daemon's peer link credentials. The set drops them as the file is read,
and no request carries one. A box's tailnet address comes from the daemon's `SUBROUTER_PEER_TAILNET`
(`name=IP,...`), read by the daemon's rules (§68). The peers file has no field for it: a new field would
make builds already deployed refuse the whole file. The tailnet address is tried first and the URL second,
both against the box's pin. A value the daemon would ignore (an IP outside Tailscale's ranges, a name that is
not in the peers file, a box named twice) is ignored here too, and every box dials its URL.

Each box needs a token minted for the stand-in: one `membership` token (§43) in
`<state folder>/standby-tokens/<box name>`, a regular file that only its owner can read. A file that group
or others can read is refused, and so is a missing, unreadable or malformed one. The box then shows a fixed
word in the status (`file_mode`, `file_missing`, `file_unreadable` or `file_malformed`) and is sent nothing.
The log names the box and the word, never a token. A peers-file entry that cannot make a pinned `https` box
shows `box_invalid`. The files are read again every 2 s, so a token file written, changed or removed is used
from then on, with no restart, and a rewritten peers file is picked up the same way. A box whose entry and
token did not change keeps its connections and its result. A peers file that stops loading keeps the boxes
of the last file that loaded. The status then shows a fixed word for the peers file, the token file's words
(`file_missing`, `file_mode`, `file_unreadable` or `file_malformed`), and the log has the loader's message.

A session the Mac holds on a box goes to that box, which has its account and its prompt cache. The holds
are the daemon's `peer-holds.json` for the first peer and `peer-holds-<name>.json` for each other, keyed
`claude:<session id>`, read as the daemon reads them: an entry that names another peer is dropped, and the
first file wins. A hold's deadline is not read. While the daemon is down nothing renews a hold, and a
session the stand-in has kept on its holder must not move when the outage outlasts the hold. Every other
session goes to the box that a rendezvous hash of its session id ranks first (SHA-256 of the box name and
the id). So a session lands on the same box every time, after a restart of the stand-in too, and a box that
goes down moves only its own sessions. A box that refused its token gets no request. A box that could not
be reached is tried only after every other box, because it may be back.

The set tests a box's token with `HEAD /`, the cheapest request a box authenticates. The box's proxy answers
204 to a token it takes and 401 to one it refuses, and sends nothing upstream. The result per box (`ok`,
`refused` or `unreachable`) and its time are in `Set.Status()`, for a status body. A box not tested yet
shows `untested` and is used. The box handler reports each box's answer to the set (`Boxes.Answered`,
`Boxes.Unreachable`), and an answer other than a 401 counts as a test. So a box gets a token test only when
nothing has reached it for 10 minutes: every 10 minutes while the stand-in is idle, and not while it
serves. A token that lapses is found within 10 minutes, not on the day the stand-in needs it.

A box's 401 to a request decides nothing by itself, because a box also relays an upstream 401. A box's
identity whose token Anthropic revoked answers `policy_limits` with Anthropic's own 401 (the 2026-11-19 case
in `internal/proxy/claude_socket_guard.go`), while the box takes the stand-in token and serves every model
turn. This part first marked a box refused on that 401, and two session starts took both boxes out: every
model turn got `standby_no_box` for 10 minutes while both boxes served. So a request's 401 asks for a token
test on the next tick, within 2 s, and only the test's 401 marks the box refused: the box answers `HEAD /`
itself, so that 401 is its own. The client's retry, 30 s later, goes to another box when the token was
refused, and to the same box when it was not. An address that fails before a request is sent goes after its
box's other addresses for a minute, as the peer link's tailnet cooldown does, and a box is unreachable only
once all its addresses have failed. So a box whose tailnet address fails while its URL works is never shown
unreachable.

The set does not read `SUBROUTER_PEER_TAILNET` from the daemon's worker config, and does not ask the
daemon's `remote.key` gate: the process that builds it does both (§79.3). Turning it off: only
`subrouter standby` runs the set, and removing a box's token file takes that box out within 2 s.

Tests: `internal/standby/handler_test.go` (§79.1) and `internal/standby/boxset_test.go` (§79.2) hold one
test per behaviour above, against `httptest` TLS boxes that each have their own self-signed certificate and
pin, with no network, and temporary state folders. Run them with `./deploy/docker/test.sh ./internal/standby`.
Each test was run once with its behaviour's code taken out of the working tree, and failed.

§79.3 The process. `subrouter standby` (`cmd/subrouter/standby.go`) is a second small process beside the
supervisor. It waits on the lock of the daemon's Claude socket (`<socket>.lock`, §30). When the supervisor
lets go of that lock, because it ended, its job was unloaded, or it let go on purpose (§77.3), the stand-in
binds the same socket path and sends turns to a box through §79.1 and §79.2, until the supervisor asks for
the path back. While the daemon serves its socket, the stand-in is not in the path of any request. It runs on
Unix. On Windows the command says that it is not supported. This part adds the command only; §79.5
installs it.

Arming. A stand-in is armed or disarmed, and a disarmed one never touches the lock or the path, not even to
create the lock file. It asks the supervisor's control socket for `supervisor-status` every 3 s. It is armed
when the supervisor's last answer has `claude_socket_waits_for_lock: true` and a `claude_socket` of
`serving`, `waiting`, `displaced` or `yielded`. A supervisor built before §77.1 tries its socket once at
start and gives up for good. A stand-in that took the lock while such a supervisor restarted would leave the
daemon without its socket until someone noticed, so beside one the stand-in stays disarmed. The supervisor's
last answer, good or bad, is kept in `<state folder>/standby-supervisor.json` (mode 0600), written when it
changes and every 5 minutes. A stand-in that starts while the supervisor is away, which is the day it is
needed, is armed by a saved answer under 24 hours old. It waits for its first look at the control socket
before it decides. A saved answer without the marker keeps it disarmed, so a supervisor that was rolled back
to an older build is not armed against from an earlier answer. A disarmed stand-in gives one fixed word for
the reason:

- `remote_locked`: the `remote.key` gate (§17) is locked for the state folder. The box set is then never
  built, so no token is read and no box is dialled.
- `peers_file_missing`: no peers file is named, or the named one is not there. The stand-in arms within
  seconds of the file arriving.
- `no_supervisor_reading`: the supervisor has not answered, and there is no saved answer under 24 hours old.
- `supervisor_without_marker`: the supervisor's last answer has no `claude_socket_waits_for_lock`.
- `supervisor_keeps_no_socket`: the marker is there, and the word is `unavailable` or `stopped`.

Taking the path. An armed stand-in tries the lock every 50 ms with the daemon's own opener
(`openLeasedClaudeSocket`). So it holds the lock about 50 ms after the lock is free, well inside the 5 s the
supervisor gives a process standing by (§77.3), and the socket it binds is one the supervisor accepts: bound
under a temporary name, made private, and renamed into place. It polls for the keeper's reason (§77.1): a
goroutine blocked in `flock(2)` cannot be told to stop. It holds the lock for as long as it listens. The
opener refuses a path at which a live listener answers and frees the lock again, and the stand-in then tries
every 2 s. While the supervisor's last answer is `waiting`, the stand-in does not try at all. That supervisor
takes the lock within 250 ms of it being free, and the stand-in must not win back the lock it has just let
go of.

The hold. For the first 30 s after it takes the path (`--hold`, at most 90 s), the stand-in answers
`/_subrouter/health` and parks every other request. A watchdog loads an unloaded job again within seconds
(§81) and launchd restarts a supervisor that crashed in about 15 s. A session that moved to a box for that
long would pay one write of its prompt cache for nothing. If the path goes back inside the hold, each parked
connection is closed with no answer. The client's own retry then reaches the daemon, and nothing was sent to
a box. When the hold ends, the parked requests go on in the order they came. Each goes once the one before it
has had its head written to a box, or 2 s after it, and a request that arrives after the hold goes at once.
The requests the stand-in answers itself with a 503 (the daemon's other routes, `/api/*`, a pinned launch)
are parked too. A 503 would make such a client sleep 30 s through a restart of 5 s, and a closed connection
is tried again at once.

Serving. After the hold, a request goes through §79.1 to a box that §79.2 chose. The stand-in hands the box
set the daemon's peers file, its `SUBROUTER_PEER_TAILNET` and the state folder. It reads the two settings
where a worker reads them: in the `env` of the worker config (`<state folder>/worker-config.json`) first, and
in its own environment second. `--peers-file` and `--tailnet` name them outright. It asks the `remote.key`
gate once, for the state folder it was given, before it builds the box set. It reads no account of the
daemon, writes only its own files (two here, and §79.4's copy), and dials only boxes. Its `/_subrouter/health` is its own answer,
below, and never §79.1's short one.

Giving the path back. While it holds the path, the stand-in reads `supervisor-status` every second. When the
word is `waiting`, it also asks the daemon `GET /_subrouter/ready` on TCP (`--daemon-addr`). Once the
supervisor has asked and the daemon has answered 200 on 3 checks in a row, the stand-in shuts its server
down (`http.Server.Shutdown`). That stops accepting, closes the listener, which removes the stand-in's socket
file, and frees the lock, in that order. The supervisor binds the path within 250 ms (§77.1). From that
moment every answer says `Connection: close` and idle kept-alive connections are closed, so no session stays
on the stand-in. A stream that is already running to a box runs to its end. When the last one has ended, the
connections to the boxes are closed. A supervisor that says `yielded` is not asking, and the stand-in keeps
the path for the whole yield. Then it is armed again.

No bouncing. The rule counts the times the stand-in took the path. From the third take within 10 minutes,
3 checks are not enough: the daemon has to answer ready on every check for 60 s, and for 2, 4, 8 and then 10
minutes at each further take within those 10 minutes. The hold is the same in every take. So a daemon that is
away twice for less than the hold moves no session. A daemon that keeps coming and going moves sessions to a
box once, when the third hold ends, and they stay there until the daemon has stayed up for a minute. The
other design's guard was built for a relay that sat in front of the daemon all the time. That relay had to
judge the daemon by probes, so it kept a window of probes, a returning state and a table of the sessions it
had moved. The stand-in is told when the supervisor wants the path, and the supervisor already waits for a
steady worker before it asks (§77.3). All that is left to decide is how long the daemon must prove itself,
and one time that doubles decides it. The health body shows `takes` and `hand_back_after_seconds`. An
operator's yield counts as a take, so a third drill within 10 minutes goes back a minute after its reclaim.

Ending itself. A stand-in that holds the lock and does not serve makes a healthy daemon unreachable by
socket, and nothing else would notice. Two rules end such a process, and the kernel frees a dead process's
lock. If the lock is not free 5 s after the stand-in set out to give the path back, it exits with 1. While it
holds the path, it asks its own `/_subrouter/health` through the path every 15 s. After 3 misses in a row,
each no answer in 5 s or an answer that is not a stand-in's, it exits with 1. A supervisor that answers
without the marker while the stand-in holds the path shows as `warning: supervisor_without_marker` and one
ERROR line. The stand-in keeps serving and never gives that supervisor the path, whatever word it says: it
has already given up on its socket, and a box is better than nothing.

Health and state. `/_subrouter/health` at the path answers at once, the hold included, with
`{"ok": true, "claude_socket": "standby", "standby": {...}}`. The object has `state` (`disarmed`, `armed`,
`holding`, `serving` or `yielding`), `reason` when disarmed, `warning`, `since` (when the state began),
`pid`, `supervisor` (the last answer's `claude_socket` and `claude_socket_waits_for_lock`, and `read_at`),
`supervisor_answering`, `takes`, `hand_back_after_seconds`, `ready_checks`, `parked` (requests in the hold)
and `boxes` (§79.2's status: each box's name and the word for its token). The same object is written to
`<state folder>/standby-state.json` (mode 0600, replaced by rename) whenever anything in it changes but a
count or a time of day. So the file's `parked`, `ready_checks` and `read_at` are those of the last change.
After a clean stop the file says `stopped`, and `pid` shows a file that a killed process left. Neither holds
a token or an account.

SIGTERM and SIGINT. The stand-in gives the path back as above, lets streams run for 8 s, cuts the rest, and
exits with 0.

Flags. `--state-dir` (default `~/.subrouter`, or `SUBROUTER_STATE_DIR`), `--socket` (default
`SUBROUTER_CLAUDE_SOCKET`, else `<state folder>/claude.sock`), `--control-socket` (default
`<state folder>/supervisor.sock`), `--daemon-addr` (default `127.0.0.1:31415`), `--worker-config`,
`--peers-file`, `--tailnet`, `--hold` and `--policy-every` (§79.4). The defaults are the laptop daemon's own, so a job that runs the
stand-in needs the peers file's variable and little else. Every other interval above is a named constant in
`standby.go`.

Turning it off: stop the process. It frees the path and the lock on SIGTERM, and a supervisor that waits has
the path 250 ms later. `--hold 0` sends requests to a box as soon as the stand-in has the path.

Not done. A socket file that is deleted while the stand-in holds the path is not bound again: the
stand-in's own check ends the process about a minute later, and whatever restarts it takes the path again. A
changed `SUBROUTER_PEER_TAILNET` is read at the stand-in's next start. The ready check wants a 200, so a
worker that answers unready for good keeps sessions on a box, although the supervisor would serve with it.
The number of parked requests has no limit. When a supervisor is restarted on purpose and the old one drains
for longer than the hold, sessions move to a box until the new one asks for the path. Nothing ran on macOS.

Tests (§79.3): `cmd/subrouter/standby_test.go`, run with
`./deploy/docker/test.sh ./cmd/subrouter -run TestStandby`. They run a stand-in beside a real supervisor
with a real worker on a temporary socket path, or beside a control socket that answers what the test tells
it to, with TLS boxes that each have a certificate, a pin and a token of their own. The stand-in's rules
read the test's clock, so 30 s, a minute and a day pass when the test says so. The three tests of how it
ends run it as a process of its own.

- `TestStandbyAnswersFromABoxAfterTheHoldWhenTheSupervisorEnds`: the supervisor ends, the stand-in holds the
  lock, a request is parked and counted in health, no answer comes a second before the hold ends, and after
  it the box answers and saw its stand-in token.
- `TestStandbyClosesHeldRequestsWhenTheSupervisorIsBackInsideTheHold`: a new supervisor starts inside the
  hold, the parked connection is closed with no answer, the retry is answered by the daemon's worker, and
  the box saw nothing.
- `TestStandbyReleasesHeldRequestsInTheOrderTheyCame`: 8 parked requests are written to a box in the order
  they were parked, and all 8 before the box answers one. The box in this test reads the HTTP/2 frames
  itself. A `net/http` server starts the handlers of requests that arrive together in whatever order its
  scheduler picks, so it cannot say which came first.
- `TestStandbyNeverTakesTheLockBesideASupervisorWithoutTheMarker`: no lock file is ever made beside a
  supervisor without the marker, or beside one that says `stopped`, and the test takes the lock itself. The
  same stand-in takes the path once the supervisor has the marker.
- `TestStandbyLeavesTheLockToASupervisorThatAsksForIt`: nothing is touched while the word is `waiting`, and
  the path is taken when that supervisor is gone.
- `TestStandbyArmsFromASavedReadingUnderADayOld`: 23 hours arms and 25 does not; a saved answer without the
  marker disarms; the file is private and a later answer without the marker replaces it.
- `TestStandbyIsDisarmedWithoutTheRemoteKeyOrThePeersFile`: with the gate locked the box gets no request at
  all; the gate reads the key in the state folder it is given; a peers file that arrives arms the stand-in.
- `TestStandbyDoesNotStealALiveListenerAtThePath`: another process's listener keeps answering, and the lock
  is free between tries.
- `TestStandbyGivesThePathBackWhenTheSupervisorAsksForIt`: an operator's yield moves requests to the box
  after the hold; the stand-in stays through `yielded`; after the reclaim the worker answers, the path is
  the supervisor's private socket, the idle connection is closed, the request in flight is answered with
  `Connection: close`, the open stream runs to its end, and the box's connections then close.
- `TestStandbyGivesThePathBackOnlyAfterThreeReadyChecksInARow`: a daemon that is not ready, or ready twice
  and then not, three times over, does not get the path.
- `TestStandbyKeepsSessionsOnABoxWhileTheDaemonComesAndGoes` and
  `TestStandbyDoublesTheSteadyTimeForEachFurtherTake`: two absences shorter than the hold send nothing to
  the box; on the third take the stand-in still serves after many ready checks and a second under a minute,
  and gives the path back at the minute.
- `TestStandbyKeepsServingBesideASupervisorWithoutTheMarker`: the warning word, and no hand-back to a
  supervisor that says `waiting` without the marker while the daemon answers ready.
- `TestStandbyHealthAndStateFileHoldFixedWordsAndNoToken`: the words and fields of both, the file's mode,
  and neither the stand-in token, the peer link token nor any `srk_` text in either.
- `TestStandbyLeavesNoDeadTimeAtThePath`: a client dials the path every 10 ms while the supervisor ends, a
  new one starts, and that one yields and takes the path back. The longest run of failed dials must be at
  most 500 ms. Nine runs measured 150 to 188 ms. A run in which the dialling loop itself was held up for
  200 ms is measured again, up to three times, because it measured the machine.
- `TestStandbyEndsItselfWhenItCannotFreeTheLock`, `TestStandbyEndsItselfWhenItStopsAnswering`,
  `TestStandbyKeepsRunningWhileItAnswers` and `TestStandbyExitsOnSIGTERMWithAStreamOpen`: exit code 1 and a
  free lock for the first two; a stand-in that answers is still there after 20 checks; SIGTERM with a stream
  open ends the process with 0 well inside 10 s, with the path and the lock gone and `stopped` in the file.
- `TestStandbyDefaultsAreTheDaemons`: the defaults above, the worker config before the environment, flags
  before both, and the refused values.

Each behaviour's code was taken out of the working tree in turn, 43 changes in all, and each one failed its
test. One behaviour has no such change: that a live listener at the path is left alone is the opener's own
rule (§30), and the stand-in's part in it is to use that opener.

§79.4 The daemon's own `policy_limits` answer. A session that starts while the stand-in serves asks
`GET /api/claude_code/policy_limits` once, with no session id. Without a 200 it withholds WebFetch,
DesignSync and RemoteTrigger, ignores the Retry-After, and asks again only an hour later, so a 503 costs it
those tools for an hour after the daemon is back (§79.1). A box's 200 keeps them, but it is the box identity's
answer, and a box whose identity belongs to another organization would apply that organization's policy.
The live session of §79.1 also accepted a replay of the daemon's own 200, with or without its signature
header. So the stand-in keeps the daemon's answer and replays it.

Asking. While the stand-in is armed and the supervisor's latest answer says `serving`, the stand-in asks the
daemon for `policy_limits` through the Claude socket, as a starting session does: Claude Code's User-Agent,
`Cache-Control: no-cache`, no credential and no session id. So the daemon counts no session, and answers
from its identity account (`internal/proxy/control_plane.go`). The stand-in asks when it is first armed, and
then every 30 minutes (`--policy-every`, at least 1 minute and under 24 hours). It writes a 200 to
`<state folder>/standby-policy.json` (mode 0600, replaced by rename): the status, the body, the time, and
every header but the connection's own, `Date`, `Set-Cookie` and the daemon's `X-Subrouter-*`. Any other
answer, a failed dial included, keeps the copy it has. An answer that comes back after the stand-in took the
path is dropped, because it may be the stand-in's own copy or a box's. A disarmed stand-in asks nothing. Nor
does one beside a supervisor that says `waiting` or `displaced`, because nobody, or another process, answers
at the path then.

Answering. While the stand-in holds the path, a `GET policy_limits` is answered with the copy when the copy
is under 24 hours old, whether or not the request names an account. It is answered at once, the hold
included, and a client's `If-None-Match` still gets the 200. A copy 24 hours old or more is not used. The
answer's signature expires a day after it was issued; Claude Code 2.1.330 does not check it, and a later
client may. With no copy, or an older one, an unpinned request goes to a box after the hold, as in §79.1, and
the stand-in never answers it with a 503 of its own choosing. A pinned one then gets §79.1's 503, because a
box is not the account it was promised. The file is read at start, so a stand-in that launchd restarts during
an outage still has its copy.

A pinned launch gets the copy as well. A session that starts without a 200 here loses WebFetch, DesignSync
and RemoteTrigger for an hour, and the daemon's return does not give them back, so the 503 costs a pinned
launch more than the copy can. The daemon answers a pinned launch's `policy_limits` from the pinned account
(`internal/proxy/proxy.go`: a forced account is chosen before the control-plane identity). But the copy is
this Mac's daemon's own last answer, and on this build `claude-route` launches a pin to an account on this
Mac only when it names the identity account, the account the copy came from. Where the accounts differ, the
default policy gives the same body: §79.1's box identity, in another organization, answered with the
daemon's body byte for byte. The first build of this part answered a pinned launch's `policy_limits` with
the 503 even when it held a fresh copy.

The health body and the state file have `policy_copy`: `held`, and when held, `saved_at` and
`age_seconds`. As with `parked`, the state file's `age_seconds` is as of its last write. Neither holds the
body or a header of the copy.

The loose ends of §79.2 and §79.3. The connections to the boxes are closed once the stand-in has given the
path back and the last stream has ended, and when the stand-in stops, not 90 s later when a box's idle limit
runs out. A request's 401 asks for one token test, and its flag is cleared once that test has run, so one
401 costs one test, not one per sweep. `file_unreadable`, `file_malformed` and `box_invalid` each appear for
the case they name. These needed tests and no change in code.

Turning it off: stop the stand-in. The copy has no switch of its own. Removing `standby-policy.json` drops the
copy until the next ask.

Not done. Nothing ran on macOS, against a real Claude Code client, or against the real daemon. The tests ask
a daemon of their own. That the real daemon answers the ask from its identity account is read in
`control_plane.go`, not measured. A replay older than the signature's expiry was never tried with a client.

Tests (§79.4): `cmd/subrouter/standby_policy_test.go`, with a daemon of the test's own at the socket path that
holds the socket's lock and answers `policy_limits` with the header names of a real answer.

- `TestStandbyAnswersPolicyLimitsWithTheDaemonsSavedAnswer`: one ask at start, with no session id, no
  `X-Subrouter-Session` and no credential, and a file of mode 0600. Once the daemon is gone and the stand-in
  holds the path, `policy_limits` gets the daemon's status, body and headers inside the hold and after it,
  without `Set-Cookie` and `X-Subrouter-*`, and the box sees nothing. A pinned launch gets the same copy
  inside the hold and after it. Health shows the copy 90 s old after the clock moves 90 s, and neither health
  nor the state file holds the body or a header.
- `TestStandbyAnswersPolicyLimitsFromACopyUnderADayOld`: a stand-in started after the first one stopped
  answers with the saved file inside its hold. A second under 24 hours the copy still answers, pinned or not.
  At 24 hours the box answers, and a pinned launch gets the 503 and sends the box nothing.
- `TestStandbyKeepsItsPolicyCopyWhenTheDaemonAnswers500`: after a 500 the state file still shows the first
  copy, and the path answers with it.
- `TestStandbyAsksForPolicyLimitsEveryHalfHour`: one ask 29 minutes after the first, two at 30 minutes, and
  the second 200 replaces the copy.
- `TestStandbyKeepsOnlyTheDaemonsPolicyAnswer`: a disarmed stand-in asks nothing; beside a supervisor that
  says `displaced`, the process at the path is not asked; an answer held back until the stand-in has taken
  the path is dropped, and with no copy a pinned launch gets the 503 after the hold.
- `TestStandbyClosesItsBoxConnectionsWhenItStops` (`cmd/subrouter/standby_test.go`): the box has no
  connection left 10 s after the stop. `TestStandbyGivesThePathBackWhenTheSupervisorAsksForIt` checks the same
  after a hand-back.
- `TestOneRequest401AsksForOneTokenTest` and `TestEachUnusableTokenFileOrPeersEntryShowsItsWord`
  (`internal/standby/boxset_test.go`): two token tests a second after one 401. A folder at a token file's
  path or at the peers file's path shows `file_unreadable`. A token file with two words, an empty one and one
  over 4 KiB show `file_malformed`. An `http://` URL or a missing pin shows `box_invalid`. The box is sent
  nothing in each case.
- `TestStandbyDefaultsAreTheDaemons`: `--policy-every` is 30 minutes by default, and 59 s and 24 h are
  refused.

Each behaviour's code was taken out of the working tree in turn, 29 changes in all, and each one
failed its test.

§79.5 Packaging: the stand-in's own launchd job. `ops/deploy/macos/install-standby.sh` installs, upgrades,
reads and removes the stand-in, and writes its tokens. Nothing is installed by hand, and nothing it runs can
change the daemon's job.

Installing. `install-standby.sh install <subrouter binary>` copies the binary to
`~/.subrouter/standby/subrouter-standby`. A deploy replaces `~/bin/subrouter`, so a copy of its own keeps every
deploy of the daemon from changing the file the stand-in runs. It writes
`~/Library/LaunchAgents/ai.manaflow.subrouter-standby.plist` and loads it, and it prints the copy's sha256. The
job has `RunAtLoad`, `KeepAlive`, because a stand-in that ends itself exits 1 and must start again (§79.3), and
`ThrottleInterval` 10. Its log is `~/Library/Logs/subrouter-standby.log`, kept small by the daemon's own stderr
cap (§39) at 4 MiB with two older copies (`SUBROUTER_LOG_MAX_MB=4`, `SUBROUTER_LOG_KEEP=2`). Before this part,
`subrouter standby` wrote its log to the CLI's file, `<state folder>/logs/subrouter-cli.log`, because `main`
gives that logger to every command but the daemon's own. It now keeps the process logger, as `serve` and
`supervise` do, and takes `SUBROUTER_LOG_LEVEL` and the cap as they do.

The job's settings are the daemon's, read from the daemon's plist and never written there. The daemon plist's
`SUBROUTER_STATE_DIR`, `SUBROUTER_CLAUDE_SOCKET`, `SUBROUTER_REMOTE_POOLS_FILE` and `SUBROUTER_PEER_TAILNET` go
into the job's environment under the same names, so the stand-in reads them as the daemon's worker does: the
worker config first, then the environment. The daemon's `--control-socket`, `--addr` and `--worker-config` go
in as the stand-in's `--control-socket`, `--daemon-addr` and `--worker-config`. Nothing else is copied; the
daemon's peer link token stays in its own plist. `--state-dir`, `--socket`, `--control-socket`,
`--daemon-addr` and `--peers-file` name a setting outright.

The check. Before it changes anything, `install` runs the binary as the job would, with the job's flags and
environment, as `subrouter standby --check`. That flag, new here, takes one live look and prints one fact per
line: `socket`, `control_socket`, `state_dir`, `peers_file <word> <path>` and `box <name> <token word>` (only
with the gate open, as in a run), `supervisor <claude_socket> waits_for_lock=<bool>` or `supervisor no_answer`,
and last `verdict armed` or `verdict disarmed <reason>`. The verdict is the run's own rule (`armingVerdict`)
applied to the supervisor's answer now. A saved reading is not read, no lock is taken, no path is bound, no
file is written and no box is dialled. The script refuses every `disarmed` verdict with one line of its own:
`remote_locked`, `peers_file_missing`, `no_supervisor_reading`, `supervisor_without_marker` (the supervisor
must first be restarted on a build with §77.1), and `supervisor_keeps_no_socket`. A box without a token file,
or with one the box set cannot use, is a warning that names the `token` verb. A binary that does not know
`--check` is refused. The binary answers, not the script, so that the gate's key path, the order in which the
peers file is looked for, and the arming rule each keep one spelling (the lesson of §12). The watchdog's
installer asks the watchdog the same way (§81.3).

Upgrading. Run again, `install` replaces the copy and the plist and loads the job again. It refuses while the
stand-in holds the socket, that is while a state file written by the job's own pid says `holding`, `serving`
or `yielding`: a restart then would cut every session the stand-in carries. `--force` goes on. Otherwise it
announces the stop to the watchdog for 300 s (`subrouter-watchdog.sh hold`, §81.2), boots the job out, renames
the new copy and plist into place, bootstraps the job and withdraws the stop. Until the rename, the running
stand-in keeps the file it started from.

Labels and launchd. The script refuses every label but `ai.manaflow.subrouter-standby`, and labels that start
with `test.subrouter.` for a rehearsal. The refusals of the daemon's label and the watchdog's name them. A
scratch label needs `--dir`, keeps its copy, plist and log there, reads no daemon plist unless one is named,
and never touches the watchdog. The script calls launchctl only for its own label, from
`SUBROUTER_STANDBY_LAUNCHCTL`, which defaults to `/bin/launchctl` and is never looked up on PATH. It refuses
its label loaded from another plist. The watchdog's job is changed only by `install-watchdog.sh`, which runs
`SUBROUTER_WATCHDOG_LAUNCHCTL`. Everything runs under macOS `/bin/bash` 3.2 with only what macOS ships. Days
and times are counted in bash alone, because `date` parses differently on macOS and Linux.

The watchdog. `install` ends with `install-watchdog.sh install --watch ai.manaflow.subrouter-standby=<plist>`.
The daemon stays watched, as by default, and so does every other `--watch` pair the watchdog's plist already
has. `--no-watchdog` leaves the watchdog as it is.

Removing. `install-standby.sh uninstall` announces the stop, boots the job out, has the watchdog watch the
daemon and its other jobs alone again, and removes the plist, the copy and the stand-in's three files in the
state folder (§79.3, §79.4). It keeps the token files unless `--purge-tokens` is given, and it keeps the log.
`--dry-run` prints the plist and every command of `install` or `uninstall` and changes nothing.

Tokens. `install-standby.sh token <box name>` reads one token from standard input and refuses a terminal, so
the token is never on screen, in an argument or in the shell's history. It refuses an argument that looks like
a token: one that starts with `srk_`, holds 24 hex digits in a row, or is not a peers-file name
(`[A-Za-z0-9._-]{1,64}`). It never prints an argument it refuses. It drops the spaces and line ends around the
token, refuses more than one word or 4 KiB, and writes `<state folder>/standby-tokens/<box name>` at mode 0600
in a folder it sets to 0700, by rename. A running stand-in reads it within 2 s (§79.2). `--expires
YYYY-MM-DD` records the token's last day in `<state folder>/standby-token-expiry/<box name>`. A new token
without it drops the old token's day.

Status. `install-standby.sh status` prints whether the job is loaded and from which plist, what the plist runs,
the copy's sha256, the state word with its reason, warning, time and pid from `standby-state.json` (a file
whose pid is not the job's is called stale), the supervisor's last answer, the peers file's word, each box's
token word with its time and, with a recorded last day, `ok`, `renew_soon` in the token's last 7 days, or
`lapsed` after it, the age of the saved `policy_limits` copy, whether the watchdog's plist names the job, and
the log's path. It prints no token and no account.

The rehearsal. `bash ops/deploy/tests/rehearse-standby.sh <scratch-dir> <subrouter binary> <token-dir>
<peers-file>` runs on the Mac's real launchd, from a Terminal.app window, with scratch labels only. It runs a
scratch supervisor job (`test.subrouter.sbrehearsal.<pid>.sup`, its worker on `127.0.0.1:31496`) with a home,
a state folder and a `remote.key` of its own and a Claude socket inside the scratch folder, and it installs a
scratch stand-in beside it with `install-standby.sh`. The token files are copied in and removed at the end;
the peers file is read where it is. A probe asks the socket path for its health every 50 ms. It boots the
supervisor's job out and checks that the path's owner goes from the daemon to the stand-in with no gap over
1 s, and that one real turn, sent at once, is answered by a box after the 30 s hold. It bootstraps the job
again and checks that the daemon has the path back within 10 s. It has the supervisor yield for 60 s and checks
both hand-overs again. It also checks that `status` reads the stand-in's own state file. Every launchctl call
goes through a guard that refuses any label not starting with `test.subrouter.sbrehearsal.` and any plist
outside the scratch folder. It prints `PASS` or `FAIL` per check and one `REHEARSAL` line. In a Linux
container, with a stand-in launchd that runs the jobs and two TLS boxes of the test's own, all 10 checks
passed in each of two runs. The stand-in answered at the path 0.1 s after the bootout or the yield, with
0.05 to 0.11 s as the longest wait between two answers. The turn came back 29.9 s after it was sent. The
daemon answered again 3.4 s after the bootstrap and 3.1 to 3.2 s after the yield ended, with 0.21 to 0.27 s as
the longest wait. It has not run on the Mac.

Turning it off: `bash ops/deploy/macos/install-standby.sh uninstall`.

Not done. Nothing here ran on macOS or under the real launchd; the rehearsal is for that, and then the drill on
the live job (one `yield?for=2m`). The scratch daemon has no accounts, so the rehearsal cannot show the saved
`policy_limits` answer. The upgrade's refusal reads the state file, which the stand-in writes at each change of
state, so a stand-in that took the path in the second before the check is not seen.

Tests (§79.5): `ops/deploy/tests/standby-install-test.sh`, run by `TestStandbyInstall`
(`ops/deploy/tests/standby_install_test.go`). Two stand-ins for launchd share one set of jobs. The installer's
fails a case when it is handed the daemon's label or the watchdog's, and the watchdog installer's fails one
when it is handed any label but the watchdog's. The binary is a stand-in that answers `standby --check` from
the case's files and records the arguments and environment it was run with. Each case has a daemon plist and
the live job's stop marker in its home, and fails when either changes. The cases: one install writes the
plist, the copy and the watchdog's plist and bootstraps each once, with the daemon's settings and not its
token; a second install announces a stop that is in place at the bootout and gone after it, and loads the new
copy; every other label is refused for every verb, and nothing runs; each of the five verdicts is refused with
one line, before anything is written, and a refused upgrade changes nothing; a missing token is a warning; a
binary without `--check` is refused; an upgrade while the job's own state file says `serving` is refused, and
`--force` goes on; the label loaded from another plist is refused; the dry runs of install, upgrade and
uninstall change nothing; uninstall keeps the tokens and purges them when told, and puts the watchdog back to
its default list; the watchdog keeps its other jobs; `token` writes the token alone at 0600 in a 0700 folder,
prints and writes it nowhere else, and refuses each argument that looks like a token, a terminal, two words and
nothing; `renew_soon` and `lapsed` at five times the case sets; `status` with a state file written as the
stand-in writes it, and a stale one; a scratch label keeps to its folder and reads no daemon plist; launchctl is
never looked up on PATH; neither script uses bash 4; and the stand-ins catch what they must. The cases pass
under bash 5 and, in the `bash:3.2` image, under bash 3.2.

`cmd/subrouter/standby_check_test.go`: `TestStandbyCheckSaysWhetherItWouldArm` (each verdict, the facts, no
lock, no path, no file, no request to the box and no token printed; with no supervisor a saved reading is not
used; with the gate locked no box is read), `TestStandbyCommandWithCheckPrintsAndReturns` (the command with
the gate read from the state folder's key returns its lines and leaves the path free) and
`TestStandbyCheckIsAFlag`. `TestConfigureDefaultLoggerLeavesStandbyLoggerAlone` and `TestIsDaemonLogCommand`
cover the log.

Each behaviour's code was taken out of the working tree in turn, 40 changes in all, and each one failed its
test.

- Code: `internal/standby/box.go` (`Box`, `NewBox`, `Boxes`), `internal/standby/handler.go` (`NewHandler`,
  `routeFor`, `IsHealth`, `PolicyCopyAnswers`, `rewriteForBox`, `send`, `retryAfterWriter`), `internal/standby/boxset.go` (`Set`,
  `NewSet`, `Config`, `Run`, `Candidates`, `Status`, `CloseIdleConnections`), `cmd/subrouter/standby.go`
  (`standbyCommand`, `parseStandbyConfig`, `runStandby`, `standIn`, `standbyTenure`, `holdGate`,
  `armingVerdict`, `flapSteady`), `cmd/subrouter/standby_policy.go` (`savedPolicy`, `standbyPolicy`,
  `considerPolicy`, `askPolicy`, `gotPolicy`, `answerPolicy`), `cmd/subrouter/remote_gate.go`
  (`resolveRemoteGateAt`), `cmd/subrouter/standby_check.go` (`checkStandby`), `cmd/subrouter/main.go`
  (`shouldKeepProcessLogger`), `cmd/subrouter/daemon_log.go` (`isDaemonLogCommand`),
  `ops/deploy/macos/install-standby.sh`
- Tests (§79.5): `ops/deploy/tests/standby-install-test.sh`, `ops/deploy/tests/standby_install_test.go`,
  `ops/deploy/tests/rehearse-standby.sh`, `cmd/subrouter/standby_check_test.go`
- Related: §30 (the Claude socket), §43 (named tokens; a stand-in token is a `membership` token), §68
  (tailnet first), §31 (the peers file and the peer-holds files), §17 (the `remote.key` gate), §77 (the
  supervisor keeps its socket, and lets go of it on purpose), §81 (the watchdog), §84 (the deploy checks
  tell a stand-in from the daemon)

## 81. A watchdog job loads the daemon's job again when something unloads it, unless a stop was announced

On 2026-11-21 a stray test run booted the daemon's job out of launchd (§76). launchd forgets a job that is booted out, so `KeepAlive` had nothing to restart, and the job stayed out for 2 h 22 min until a person loaded it by hand. Every Claude Code session and workflow on the Mac lost its proxy, and 242 workflow agents died while both boxes were healthy. A second job under its own label, one long-lived process that checks every 5 s, loaded a booted-out scratch job again in 2.4 to 3.4 s in a test on this Mac's launchd.

§81.1 `ops/deploy/macos/subrouter-watchdog.sh run` runs as the launchd job `ai.manaflow.subrouter-watchdog` (`KeepAlive`, `RunAtLoad`). Every 5 s it asks `launchctl print gui/<uid>/<label>` about each watched job: the daemon's label and plist by default, and every `--watch LABEL=PLIST` as well (the stand-in's label goes there). Only rc 113 means a job is not loaded.

| launchd says | The watchdog |
|---|---|
| loaded from the watched plist | does nothing, however quiet the job is |
| loaded from another plist (its `path =` line) | logs it and notifies once; never changes the job |
| not loaded, and a stop is announced | does nothing until the stop marker expires |
| not loaded, and the plist is missing | logs it and notifies once; does nothing else |
| not loaded | runs `launchctl bootstrap gui/<uid> <plist>`, logs it and notifies once; a bootstrap that fails is tried again after 10, 20, 40 and up to 300 s, and each failure's text is logged |
| any other rc (141: launchd refuses this shell) | logs it and notifies once; does nothing |

It never restarts a loaded job. Two watchers that restart each other is the pattern the outage research rejected, and a hung daemon is the supervisor's to notice. The notification is `osascript`'s `display notification`, sent in the background so a notifier that hangs does not stop the checks. The log, `~/Library/Logs/subrouter-watchdog.log`, has events only, never checks, and is moved to `.1` at 256 KB.

§81.2 An announced stop is left alone. A stop marker is `~/.subrouter/watchdog-stop.<label>`, with the lines `label=`, `until=` (epoch seconds) and `by=`. `subrouter-watchdog.sh hold LABEL SECONDS` writes one, for at most a day, and `release LABEL` removes it. A marker that has expired, cannot be read, names another label or names a time more than a day ahead is ignored and logged once, so a forgotten marker cannot switch the watchdog off. In `laptop-deploy-lib.sh`, `bootout_and_wait` announces the stop before its bootout, for its whole wait for launchd and 180 s more, and shortens it to 180 s once the job is gone. `bootstrap_job` withdraws it when its bootstrap worked. So a run that dies after its bootout leaves a marker that ends within three minutes, and then the watchdog loads the job. The marker is always under `$HOME/.subrouter`, the default state dir, never under the shell's `SUBROUTER_STATE_DIR`, which these scripts ignore on purpose.

`restartLaunchAgent` in `cmd/subrouter/install_daemon.go`, behind `sr daemon restart`, `sr server restart` and an install that starts the job, boots the job out and bootstraps it at once. It writes the same marker, for 5 minutes, before its bootout, and removes it however its bootstrap ends: the watchdog cannot win the gap and fail the restart's bootstrap, and a restart whose bootstrap failed leaves the job to the watchdog at once. Writing the marker is best effort, as before this change.

`sr daemon down`, `sr server down` and `sr cleanup` boot the job out without announcing it, on purpose. A stray `sr daemon down` from a script or an agent is the same accident as 2026-11-21, so it must not hold the watchdog off for long; the stop that is meant is the one a person announces with `hold`. With the watchdog installed, `sr daemon down` is undone within about 5 s. To stop the daemon on purpose, announce it first, as in `subrouter-watchdog.sh hold ai.manaflow.subrouter 3600`, or uninstall the watchdog. `sr cleanup` deletes the plist, so the watchdog reports the missing plist once and does nothing. `deploy/macos/migrate-launchagent-to-supervisor.sh` and `rollback-launchagent-supervisor.sh` (upstream's), the peer step of `ops/bin/subrouter-quickstart` (§82) and a deploy from a checkout older than this change do not announce their bootout either: the watchdog can load the plist between that script's bootout and its bootstrap, and the script's bootstrap then fails, so it rolls back or reports the daemon down. Announce a hold before running one, and update the checkout first.

§81.3 `ops/deploy/macos/install-watchdog.sh install` copies the watchdog to `~/.subrouter/watchdog/`, writes `~/Library/LaunchAgents/ai.manaflow.subrouter-watchdog.plist` and loads it. Run again, it boots the old watchdog out and loads the new copy. `uninstall` boots it out and removes the plist and the copy; the log stays. `status` shows the job, what it runs, the stop markers and the last events. `--dry-run` prints the plist and every command and changes nothing. Before it writes anything, `install` runs the watchdog's `config` with the same options and refuses what the watchdog would refuse at start, such as a job watched twice: launchd would start such a job again every 10 s for ever. The installer refuses every label but `ai.manaflow.subrouter-watchdog` and labels that start with `test.subrouter.`. A scratch label needs `--dir` and `--no-daemon`, keeps everything in that dir, watches only scratch labels, and alone takes `--launch-agents` and `--launchctl`. The installer calls launchctl only for its own label, refuses its label loaded from another plist, and never touches the daemon's job or plist. To turn the watchdog off: `bash ops/deploy/macos/install-watchdog.sh uninstall`.

§81.4 Both scripts run launchctl and osascript only from `SUBROUTER_WATCHDOG_LAUNCHCTL` and `SUBROUTER_WATCHDOG_OSASCRIPT`, which default to `/bin/launchctl` and `/usr/bin/osascript` and are never looked up on PATH; a value without a path is refused. They run under macOS `/bin/bash` 3.2 with only what macOS ships. The installer writes the plist itself and lints it with `/usr/bin/plutil` when that exists.

§81.5 Tests. `ops/deploy/tests/watchdog-test.sh`, run by `TestWatchdog`, drives both scripts with a stand-in launchd, a stand-in notifier and a clock that each `sleep` moves, so an hour of checks takes seconds. Its cases: an unloaded job gives one bootstrap of the watched plist and one notification; a loaded job gets one print per check and nothing else; every event line starts with the time it was logged; an unexpired marker gives nothing; an expired one gives a bootstrap; a 12 s hold ends at the first check after it; a marker that has expired, names another label, holds no expiry, names one more than a day ahead or cannot be read is ignored and logged once over 8 checks of a job whose bootstrap keeps failing; a job from another plist and a missing plist are reported once and changed in no way; failed bootstraps come 10, 20 and 40 s apart with one notification, and 10 in half an hour when they never work; rc 141 changes nothing; launchctl is never taken from PATH; the defaults, matched as whole lines; hold and release; SIGTERM ends a run mid-sleep; the log is moved aside; the installer refuses the daemon's label, and a foreign label in an install that would otherwise be valid, keeps a scratch watchdog to scratch jobs, installs, reports, reinstalls and uninstalls, and its dry run changes nothing; the real install's plan runs exactly `run --log ~/Library/Logs/subrouter-watchdog.log --stops ~/.subrouter`, and a watchdog run with that plan's `--stops` leaves alone a job whose stop `hold` announced with its default folder, as the deploy scripts do; the installer's launchctl is `/bin/launchctl` when unset, the very path in the shell's error for it, and is refused without a path; it refuses a configuration the watchdog would refuse. With two watched jobs, as the real watchdog has once the stand-in's label is added, each is checked at every check and repaired on its own: the first one loaded from another plist is reported while the second, down, is loaded; both down are each loaded once from their own plist; and with both failing their bootstraps each keeps its own pace, so the one that loads at its third try is tried at once and is not held back by the other's waits. Every case fails when a notification is not a `display notification` banner: a dialog or an alert waits for a click and would leave an `osascript` hanging per event. The stand-ins fail a case that hands them `ai.manaflow.subrouter`, and so does the marker code: each case starts with a canary marker for the live label in its stop dir, which is also the default one, and fails when that canary is changed or removed (`hold` or `release` of the live label) or when a marker for the live label turns up anywhere else in the case's dir. A case checks that the stand-ins and the canary see what they must. The cases pass under bash 5 and, in the `bash:3.2` image, under bash 3.2; since CI and Docker have only bash 5, a case also fails on the commonest bash 4 constructs in the three watchdog scripts (`[[ -v`, `declare -A`, `${x,,}`, `mapfile`, `|&` and others), and checks that its pattern finds each and none of the bash 3.2 forms the scripts use. Two cases at the end of `deploy-scripts-test.sh` check that a deploy announces its bootout with a marker and withdraws it after its bootstrap, and that a failed bootstrap leaves a marker with at most 180 s left. After its cases, each run of `deploy-scripts-test.sh` fails when a marker for the live label is anywhere in its test dir, and a case checks that it finds one. In `cmd/subrouter`, the stop-marker code panics in every test when handed the live label, before it makes the marker's path (`watchdogStopMarkerHook`, set by an `init` in `installer_lifecycle_test.go`), and `TestStopMarkerCodeIsNeverHandedTheLiveLabelInTests` checks that it does and that a scratch label goes through. `TestLaunchdIsNeverAskedOutsideTheGUILogin` now uses a scratch label: its calibration, in the GUI login, reached the marker code with the live one. `TestRestartLaunchAgentAnnouncesItsBootoutToTheWatchdog` checks that `restartLaunchAgent` has the marker in place at its bootout and its bootstrap, and none afterwards, whether the bootstrap worked or failed. `sr-local-ci`'s deploy run now compiles every test file in `ops/deploy/tests`, not only `deploy_scripts_test.go`. Each behaviour, taken out on its own, fails its case.

§81.6 `bash ops/deploy/tests/rehearse-watchdog.sh <scratch-dir>` rehearses it on the Mac's real launchd with labels that start with `test.subrouter.wdrehearsal.` and plists in the scratch dir. Every launchctl call goes through a guard that refuses anything else. It boots a scratch job out three times and times its return (under 10 s passes), and checks that an announced stop is left alone until it is withdrawn, that a 12 s stop ends on time (timed from the end its marker names, which is a whole second and can fall up to 1 s before 12 s), and that a job loaded from another plist is left as it is. Against a fake launchd in a container it passed all 11 checks; it has not yet run on the Mac.

§81.7 Not covered: a job that is loaded but hung, a logout (the `gui/<uid>` domain goes, and the watchdog with it), and a load of 100 or more, which no test has reached.

- Code: `ops/deploy/macos/subrouter-watchdog.sh`, `ops/deploy/macos/install-watchdog.sh`, `ops/deploy/macos/laptop-deploy-lib.sh` (`announce_stop`, `withdraw_stop`, `bootout_and_wait`, `bootstrap_job`), `ops/bin/sr-local-ci` (`deploy_tests`), `cmd/subrouter/install_daemon.go` (`restartLaunchAgent`, `announceWatchdogStop`, `withdrawWatchdogStop`, `watchdogStopMarkerHook`), `cmd/subrouter/lifecycle.go` (`restart`)
- Tests: `ops/deploy/tests/watchdog-test.sh`, `ops/deploy/tests/watchdog_test.go`, `ops/deploy/tests/deploy-scripts-test.sh` (the three cases after `triage-never-installed-is-missing`, and the sweep after them), `ops/deploy/tests/rehearse-watchdog.sh`, `cmd/subrouter/installer_lifecycle_test.go` (`TestRestartLaunchAgentAnnouncesItsBootoutToTheWatchdog`, `TestStopMarkerCodeIsNeverHandedTheLiveLabelInTests`, the `init` guard), `cmd/subrouter/login_session_test.go` (`TestLaunchdIsNeverAskedOutsideTheGUILogin`)
- Docs: `ops/LAPTOP.md`, `ops/README.md`

## 82. Main holds what one person needs to run their own subrouter

`ops/` held the laptop's tools and this log. The box deploy, the setup tools, the docs a newcomer needs and the agent skill lived outside the repository, written for a team that shared one pool. Main is now canonical for one person running their own subrouter: a Mac laptop daemon and, optionally, a Linux box of their own on Tailscale.

§82.1 Docs. `ops/GETTING-STARTED.md` is the tutorial, from a fresh Mac to a working pool and a box. `ops/OPERATING.md` is the reference for day-to-day work. `ops/docscheck` fails when either names a repository path, link, setting or route that the tree does not have.

§82.2 The box. `ops/deploy/lightsail/bootstrap.sh` sets a box up, `deploy-box.sh` deploys a build to it with a drain, checks and rollback, and `push-box.sh` runs the deploy over ssh. Nothing real is hardcoded. Bootstrap prints the laptop's `sr server add` with the box's `--tailscale-node-id`, without which `sr claude move` to a plain `http` URL sends nothing. It prints no `--default`, which would rewrite the laptop's Codex config and `cloud.json` and restart its daemon outside `deploy-laptop.sh`. Under §76.2 the box cases give the box's loopback listener the test address. A box's own daemon listens on `:31415` at the box's address, so for those cases only a loopback or wildcard `:31415` counts as this machine's live daemon.

§82.3 Setup tools.
- `ops/bin/subrouter-quickstart` takes a Mac from nothing to a laptop daemon every client uses. It points no client at the daemon until a separate process has proved a real turn through it, and it refuses an empty pool unless a peer serves it (§3). Its peer step restarts the job under the laptop scripts' mutation lock.
- `ops/bin/subrouter-remote-key` mints, checks and removes `remote.key` (§17).
- `ops/bin/subrouter-unbrick` undoes client settings that point at a daemon that does not answer.

The quickstart and remote-key take `SUBROUTER_LABEL` and `SUBROUTER_PLIST`. Like the laptop scripts under §76.1, the quickstart restarts only a job that launchd loaded from its own plist, and otherwise refuses before it writes anything. It announces that restart's bootout to the watchdog (§81) and withdraws it once its bootstrap worked, as `laptop-deploy-lib.sh` does. Their tests follow §76: every case uses a test-only label, a case fails when its stubs see the live job, and on macOS the cases run only inside the harness's sandbox.

§82.4 The skill. `ops/skills/subrouter/SKILL.md` is for agents that inspect or operate an install. `skill_test.go` fails when the skill names a path, section, route, setting, command, flag, column or quoted string that the tree does not have.

§82.5 Privacy hooks. `pre-commit`, `commit-msg` and `pre-push` in `ops/hooks/` refuse a value read live from the contributor's own machine, and print only where it is. The sources are the daemon's accounts, tailscale, git's email, the login name and home path, and an optional values file. The diff they read ignores the user's git diff settings. Install them with `git config core.hooksPath ops/hooks`.

§82.6 `AGENTS.md` says main is canonical and maps where things live. `README.md` points at the tutorial.

- Code: `ops/deploy/lightsail/`, `ops/bin/subrouter-quickstart`, `ops/bin/subrouter-remote-key`, `ops/bin/subrouter-unbrick`, `ops/hooks/`, `ops/skills/subrouter/`
- Tests: `ops/docscheck/`, `ops/skills/skill_test.go`, `ops/hooks/hooks_test.go`, `ops/deploy/tests/deploy-scripts-test.sh` (the box cases), `ops/deploy/tests/setup-tools-test.sh`, `ops/deploy/tests/setup_tools_test.go`
- Docs: `ops/GETTING-STARTED.md`, `ops/OPERATING.md`, `ops/deploy/lightsail/README.md`, `ops/skills/README.md`, `ops/hooks/README.md`, `AGENTS.md`, `README.md`, `ops/README.md`

## 83. A run never waits for ever on its mutation lease helper

`deploy/macos/mutation-lease-lib.sh` takes the supervisor mutation lease (§69.5) through a Python helper that holds the lock's descriptor. `acquire_subrouter_mutation_lease` started the helper and looked for its answer 1000 times, with `sleep 0.01` between looks. When the looks ran out, it sent the helper a plain `kill` (TERM) and waited for it. A detached run ignores HUP, INT and TERM, and the helper it starts inherits that, so the TERM did nothing. The helper runs until its parent shell is gone, and the parent was waiting on it, so the wait never returned. A helper that got the lock meanwhile kept it for as long as the two lived. On 2026-11-21 six stray detached deploy runs from stopped test runs sat parked like this for hours, and a full test run hung in the same place (§76). A hung lease holder blocks every later deploy, rollback and update, and would mislead a watchdog that respects the lease.

The 1000 looks were not a time limit either. Under the test's instant `sleep` stub they ran out within a few seconds, and at a load of 80 that came before a healthy helper had started Python.

§83.1 The helper gets a limit on the clock: `SUBROUTER_MUTATION_LEASE_WAIT_SECONDS`, default 30, counted with bash's `SECONDS`. A helper that answers within it is waited for, however slow it is. A value that is not a whole number of seconds is read as 30.

§83.2 When the limit passes, `stop_subrouter_mutation_lease_helper` sends TERM, gives the helper up to 2 s, and then sends KILL, which no process can ignore. It waits on the helper only once `kill -0` finds it gone, and gives up after 10 s if KILL has not ended it, so the caller never waits on a live helper. A killed helper's lock goes with its descriptor. The acquire prints one line, `the mutation lease helper did not answer within <N>s (SUBROUTER_MUTATION_LEASE_WAIT_SECONDS) and was stopped; the lease was not taken`, removes its control dir and returns 73 as before. The laptop scripts' `take_mutation_lock` then prints `refusing: cannot take <lock> (exit 73). Nothing was changed.` `release_subrouter_mutation_lease` stops its helper the same way, so a caller that ignores TERM no longer hangs when it lets go. The laptop scripts' `release_mutation_lock` still sends KILL first, which spares a detached run the 2 s that TERM gets.

§83.3 A run that is killed while it holds the lease leaves the lease free. This was already so, and is now tested. The helper checks every 10 ms that the shell that started it is still its parent, and exits when it is not, unless it has adopted a rollback child that is still alive. A helper whose shell was killed before it answered takes the lock, finds its parent gone at its first check, and exits.

§83.4 There is nothing to turn off. A larger `SUBROUTER_MUTATION_LEASE_WAIT_SECONDS` waits longer for a slow helper.

§83.5 Tested in Docker with the deploy-script cases. `laptop-detached-run-waits-for-a-slow-lease-helper` delays the detached run's helper by 6 s and expects `DEPLOY OK`. `laptop-lease-helper-that-never-answers-is-stopped` gives the detached run a helper that never answers and a 3 s limit, and expects exit 1 with both lines, no helper left alive, the lease free, nothing changed, and a next run that deploys. `laptop-killed-run-frees-the-lease` parks the detached run in its kickstart, kills only its shell, and expects the lease free within 10 s. `lease-release-stops-a-helper-that-ignores-term` releases the lease from a shell that ignores TERM. Each case gives its runs a limit on the clock, so a hang fails the case. With the library from before this change, the first, second and fourth cases hung until that limit; the third passed. With only the 1000 looks put back, the first case failed with exit 1: the looks were spent, and the helper stopped, before its 6 s were up. With the helper's parent check removed, the third case failed.

- Code: `deploy/macos/mutation-lease-lib.sh` (`stop_subrouter_mutation_lease_helper`, `acquire_subrouter_mutation_lease`, `release_subrouter_mutation_lease`)
- Tests: `ops/deploy/tests/deploy-scripts-test.sh` (`slow_lease_helper`, `lease_helpers`, `lease_free`, `run_within`, and the four cases above, listed under §83 in `requiredCases`), `ops/deploy/tests/deploy_scripts_test.go`
- Docs: `ops/LAPTOP.md`

## 84. A deploy check never takes a stand-in at the Claude socket for the daemon

On 2026-11-21 the laptop's daemon was gone for more than two hours, and every Claude Code session on the Mac failed with it while the boxes were healthy. The fix under way puts a stand-in on the Claude socket path while the daemon is away. The stand-in answers `/_subrouter/health` there with `"ok": true`, so that new launches still take the pool route, and with `"claude_socket": "standby"`. `probe_claude_socket` in the laptop deploy library said `serving` when anything at the path answered health with `"ok": true`. A deploy would then print `DEPLOY OK` while a stand-in held the path. It did the same when the supervisor served TCP but did not hold the path: while it waited for the socket's lock (`waiting`), when a listener it does not own answered there (`displaced`), and when it let go of the path on purpose (`yielded`). This change has to be in before a stand-in can answer at the path.

§84.1 The probe now reads who answered at the path and what the owner says. The owner's word is `claude_socket` in supervisor-status for a supervised job, and `claude_socket` in a plain serve's TCP health. The first word of the probe's answer is one of these:

- `yielded` when the owner says `yielded`. The line gives the reason from `claude_socket_yield` (an operator asked, or its workers kept exiting), the time in its `until`, and what answers at the path. It says to wait for the yield to end or to end it.
- `standby` when a health answer at the path says `"claude_socket": "standby"`. If the owner says `waiting`, the line says that the owner is asking for the path back and that the stand-in hands it over once it sees the daemon ready. Otherwise it says that the owner is not asking for the path back, gives the owner's word, and points to the owner's log.
- `waiting` when the owner says `waiting` and no stand-in answers. The line names `<path>.lock` and `lsof <path>.lock`.
- `displaced` when the owner says `displaced`. The line names `lsof <path>`.
- `serving` when any other process answers health at the path with `"ok": true`, and `down` when none does, as before.

§84.2 A deploy never prints `DEPLOY OK` while the probe says `standby`, `waiting`, `displaced` or `yielded`. It prints `the daemon is not serving its Claude socket`, keeps the new build, and rolls back or restarts nothing for it. It ends with `DEPLOY INCOMPLETE (claude socket not owned by the daemon: <word>)` and exit 3. `--check` ends with `CHECK INCOMPLETE (claude socket not owned by the daemon: <word>)` and exit 3. Both hold whatever the socket's baseline was. A rollback does not give the daemon the path back, because the stand-in, the lock holder or the yield is still there after it, and for a plain serve a rollback is one more restart. A yield because the workers kept exiting can start with a new build. The line names that reason, and the operator decides whether to deploy the backup.

§84.3 `serving` and `down` behave as before, and so do the supervisor's words `unavailable` and `stopped`. A socket served before the swap and down after it rolls back. A socket down before the swap too ends `DEPLOY INCOMPLETE (claude socket not served)`. A daemon from before these words reports none of them, so its deploys run as before. The `(before: ...)` part of the line now gives only the first word of the baseline. The probe of a plain serve now reads the daemon's TCP health every time, not only when the path does not answer.

§84.4 `sr-triage` prints a `socket:` line for the Claude socket the job's plist names. It is `STAND-IN at <path>` when a stand-in answers there, `OK (<path>)` when another process answers health with `"ok": true`, `NOT answering at <path>`, or `none` when the plist names no socket.

§84.5 There is nothing to turn off. The change is in what the scripts read and print, and nothing in it restarts the daemon. What decides a launch's route still reads only `"ok": true` at the path: the shim's own probe, and the check `claude-route pool` makes before it switches. A launch while a stand-in holds the path takes the pool route, as the stand-in intends.

§84.6 Tested in Docker with the deploy-script cases. The fake `curl` gains `FAKE_CLAUDE_WORD` (the word of a health answer at the path), `FAKE_SV_YIELD` (supervisor-status `claude_socket_yield`) and `FAKE_DAEMON_SOCKET_WORD` (a plain serve's word on TCP). `sup-stand-in-at-the-claude-socket-is-not-the-daemon` puts a stand-in at the path while the supervisor says `waiting`. It expects exit 3, the `standby` line, the new build kept, no restart and one upgrade. `sup-check-stand-in-the-supervisor-does-not-ask-back` runs `--check` with a stand-in while the supervisor says `unavailable`, and expects `CHECK INCOMPLETE`. `sup-claude-socket-waiting-is-not-deploy-ok` and `sup-claude-socket-displaced-is-not-deploy-ok` put another process at the path. `sup-claude-socket-yielded-says-until-when` checks an operator's yield in a deploy, and a yield for worker exits with nothing at the path under `--check`. `laptop-stand-in-at-the-claude-socket-is-not-the-daemon` deploys to a plain serve, whose restart is when a stand-in can take the path. `triage-tells-a-stand-in-from-the-daemon` checks the three `socket:` lines. With the scripts from before this change, every one of these cases failed. Each stand-in, `waiting` and `displaced` case and the operator's yield exited 0 with `DEPLOY OK` or `CHECK OK`. The yield for worker exits ended `CHECK FAILED`. Triage printed no `socket:` line. `sup-claude-socket-through-the-supervisor` and `sup-claude-socket-down-names-the-supervisor-state` pass unchanged. Three cases pin §84.3. `sup-claude-socket-stopped-and-answering-is-serving` and `sup-claude-socket-stopped-and-silent-is-not-served` deploy while the supervisor says `stopped`: with an answer at the path the run ends `DEPLOY OK`, and with none it ends `DEPLOY INCOMPLETE (claude socket not served)`. `sup-claude-socket-from-a-supervisor-without-the-word` deploys under a supervisor whose supervisor-status has no `claude_socket`, with an answer at the path, and expects `DEPLOY OK`. An independent check found that a probe which read `stopped` as `displaced`, or a missing word as `down`, passed every other case. Each of those probes fails one of these three.

§84.7 Two more tools read any answer at the path as the daemon serving it. An independent check of this section found them, and they now tell a stand-in from the daemon too. `claude-route`'s `socket_owner` says `serving`, `standby` or `down`. `claude-route status` prints `socket: STAND-IN at <path>: the daemon does not own its socket` for a stand-in. The preflight check `daemon answers THROUGH the socket` of `claude-route test` fails while a stand-in answers. When the plist already names the socket and a stand-in answers there, `claude-route socket-on` no longer says `already serving`. It says that restarting the daemon would not hand it the path, changes nothing, names `deploy-laptop.sh <binary> --check`, and exits 1. `activate-laptop-supervisor.sh` checks a plain serve after `--rollback`, after it loads a job that was not loaded, and after a failed activation. It said `claude socket -> serving` for any answer at the path. It now waits up to 60 s for the daemon itself, then names a stand-in that still holds the path, and the run ends `NOT HEALTHY` with exit 2, as for a socket that does not answer. It restarts nothing more for it. `route-status-and-socket-on-tell-a-stand-in-from-the-daemon` runs `status` and `socket-on` with the daemon at the path and then with a stand-in. `act-rollback-tells-a-stand-in-from-the-daemon` rolls an activation back while a stand-in answers. With the two scripts from before, the first failed 6 of its 12 checks (`socket: serving`, `already serving`, exit 0) and the second failed 4 (`claude socket -> serving`, `ROLLED BACK`, exit 0). Not run: the preflight check, because `claude-route test` launches real sessions. It reads the same `socket_owner` that `status` does.

- Code: `ops/deploy/macos/laptop-deploy-lib.sh` (`probe_claude_socket`, `at_the_path`, `status_field`, `verify_routes`), `ops/deploy/macos/deploy-laptop.sh` (`SOCKET_WHY`, the `--check` ending), `ops/bin/sr-triage`, `ops/bin/claude-route` (`socket_owner`, `cmd_socket_on`, `cmd_status`, the `test` preflight), `ops/deploy/macos/activate-laptop-supervisor.sh` (`socket_owner`, `verify_direct`)
- Tests: `ops/deploy/tests/deploy-scripts-test.sh` (the fake `curl`'s three knobs, `no_owner_kept`, and the twelve cases above, listed under §84 in `requiredCases`), `ops/deploy/tests/deploy_scripts_test.go`
- Docs: `ops/LAPTOP.md`, `ops/OPERATING.md`

## 85. The Docker test runners keep their Go build caches near a limit

`deploy/docker/test.sh` keeps Go's build cache in the Docker volume `subrouter-test-gobuild`, so later runs reuse it. Go deletes only cache entries that no build used for five days. Each tree a run tests adds entries, and many worktrees were tested each day. On 2026-11-22 the volume held 17 GB after three days of heavy use, and a sibling volume held 8.8 GB. The disk that Docker writes to reached 98 percent, minutes before tests across the machine would have failed for lack of space. `ops/bin/sr-local-ci --docker` keeps its build cache in a volume of its own, `sr-local-ci-gocache`, which grows the same way.

§85.1 `deploy/docker/trim-go-cache.sh DIR LIMIT_KIB AGE_MINUTES` measures DIR with `du -sk`. When DIR holds more than the limit, it deletes the entries not used for AGE_MINUTES and prints one line that gives the size, the limit, the number of entries deleted and the space they freed. When every entry may have been used within that time, the line says that nothing was freed. At or under the limit, with a limit of 0, or with no DIR yet, it deletes nothing and prints nothing. An entry is what Go's own trim deletes: a file named `*-a` or `*-d` in one of the two-hex-digit folders. The cache's `README` and `trim.txt`, deeper files, other folders and folder links stay.

§85.2 Before each run, and before `--self-check`, `test.sh` runs the trim in a container of its own. The container mounts the build cache volume and the checkout read-only, with no network and no module volume. The limit is `SUBROUTER_TEST_CACHE_LIMIT_GB`, a number of GB, default 6. `0` turns the trim off, and a value that is not a number stops the run before any container starts. The age is a day. `sr-local-ci --docker` runs the same script, from its own checkout, on `sr-local-ci-gocache` before the suite, with the same variable. A trim that fails prints a line, and the run goes on with the cache as it was.

§85.3 Runs at the same time are safe. Go updates the time of an entry it uses only once that time is an hour old, so an entry used within the day has a time within the day and an hour. The trim deletes an entry only when its time is older than a day and an hour, and Go's own trim adds the same hour. So the trim never deletes an entry used within the day, or one that a running build uses. A build can still miss an entry that was unused for a day and is deleted just as the build looks it up, and the build then compiles it again. Go's own trim has the same window. Two trims at once each skip what the other deleted first (`find -ignore_readdir_race`), and both exit 0.

§85.4 The limit is not a ceiling. While every entry may have been used within the day, the cache stays over the limit, and each run prints that nothing was freed. When this was built, the volume held 8 GB, and no entry in it had a time over a day old. Over the limit, the cache keeps about one day of use where it kept five. `sr-local-ci` without `--docker` uses the developer's own Go cache, which every Go build on the machine shares, so it is left to Go.

§85.5 To turn it off, set `SUBROUTER_TEST_CACHE_LIMIT_GB=0`. To clear a cache by hand, remove its volume while no run uses it, as `deploy/docker/README.md` says. Its section Test runner caches gives the limit, its default, `0` to turn it off, and a `docker volume rm` line for each volume: the two that `test.sh` mounts and `sr-local-ci-gocache`. `TestDockerReadmeSaysHowToBoundAndClearTheCaches` runs `test.sh` through the stand-in `docker` below, reads the volumes it mounts and the default limit it hands the trim, reads `sr-local-ci`'s build cache volume from the script, and expects the section to name each. It fails on the README from before §85, and on one without the `sr-local-ci-gocache` line.

§85.6 Tested in Docker. `TestGoCacheTrimDeletesOnlyEntriesUnusedForADayOverItsLimit` runs the script on a folder shaped like a Go build cache, with entries two days old, an entry whose time is 24.5 hours old (Go may have used it 23.5 hours ago), entries used three hours ago and now, the marker files, files that are not entries, a folder link and a module folder beside it. Over the limit, only the two old entries go, and the line says that 1.0 MB was freed. Under the limit and with a limit of 0, nothing goes and nothing is printed. `TestGoCacheTrimsAtOnceAllSucceed` runs four trims at once on 1,792 entries, and each exits 0 with one line and no error. `TestDockerTestScriptTrimsItsBuildCacheBeforeTheRun` and `TestDockerRunTrimsItsBuildCacheBeforeTheSuite` (in `ops/localci`) run `test.sh` and `sr-local-ci --docker` with `deploy/docker/testdata/fake-docker.sh` first on `PATH` as `docker`. The stand-in records each call, and runs the trim on folders that stand in for the volumes. Over the limit, the old entry in the build cache goes, the module volume keeps its old file, and the trim comes before the tests. With a limit of 0, nothing goes. A trim that fails leaves the run to go on, and a limit that is not a number is refused. With `test.sh` and `sr-local-ci` from before this change, the old entry stayed, a failed trim printed nothing about it, and a limit that is not a number ran. The cases for the default limit and for 0 check only that nothing is deleted, so they pass either way. Without the script, every case of the first two tests failed. Without its age test, the script deleted the entries in use. Without the extra hour, it deleted the entry whose time is 24.5 hours old. Without its size test, it trimmed a cache under its limit. Without `-ignore_readdir_race`, trims at once printed `No such file or directory`. The first real runs found `subrouter-test-gobuild` at 8.0 and 8.3 GB and freed nothing, because no entry's time was over a day and an hour old.

- Code: `deploy/docker/trim-go-cache.sh`, `deploy/docker/test.sh`, `ops/bin/sr-local-ci` (the `--docker` branch)
- Tests: `cmd/subrouter/docker_cache_trim_test.go`, `ops/localci/localci_test.go`, `deploy/docker/testdata/fake-docker.sh`
- Docs: `deploy/docker/README.md`

## 86. An account whose week has not started takes the next new session first, once, on this daemon or a peer

The owner, 2026-11-22: "Codex accounts don't start running until you try to use them for the first time. My default is to try to spend the accounts down as they're running out of time. The thing is that accounts that are at exactly 7 days need some traffic as soon as possible to start it." A Codex weekly window starts on the account's first request and then counts down whether or not anyone spends. Weekly pace (§10, §31 P2) ranked an account whose week had not started like one spending at exactly even pace: behind every account with budget above even pace, and behind every account in its last day. In a lightly used pool it could wait near the bottom for days, and each day it waited was a day of its week the pool could not spend. `TestCodexPrefersTheSoonestRefresh` pinned it last. The pool board already shows such an account as PRISTINE (`readFuse`), but ranking never read that. Upstream has no rule for it.

§86.1 The fact, the clause and the mark.

`Score.WeeklyUnstarted` says that the binding weekly window has not started. `bindingWeekly` reads it from the window it binds on: the reset as read is at or above the window's length. It uses `>=`, as the board's fuse does, so a second of upstream clock skew does not turn an untouched week into a running one. The fact is set only where the windows are read (`scoreFromLimitWindows`) and is never rebuilt from `WeeklyResetAt`. That time is anchored when the score is built, so a cached reading two minutes old of an account started a minute before the fetch would read as untouched. No weekly window, a week that reports no reset, a reading from before its own reset, and a usage-based seat's spend window (§42) are not unstarted. Each model pool reads its own window. The Spark pool's week is separate from the account's, and a pool whose own window is unread is not unstarted, even while it ranks on the account's week. The rule reads the window, not the provider's name. Claude reports an unset reset as 0, so in practice only a Codex week reads this way.

In `compare`, when two accounts can both take a new session, one whose week has not started and that carries no start mark sorts first. The clause is `its weekly window has not started; one new session starts its clock`. It comes after machine policy, the selection tier, the capacity and rate-limit marks (§27, §35.3), the new-session test and the machine-binding clauses. It comes before the endgame drain and every clause below it. It applies in `Pick` and in `PickSteady`. A peer's unstarted account beats a local account whose week runs, because the local 2x is a pace factor below the clause. Two unstarted accounts fall through to the clauses below.

`SchedulerRef.NoteNewSession(scheduler, account)` is called for each new session the router places. When the ranking that placed the session read the account's week as unstarted, it marks the account for `StartSentMarkDuration`, 10 minutes. The mark is kept in memory per provider and account ID. A peer's account has the ID that the merged ranking gave it, so its mark is per peer. `Get` attaches the live marks to every scheduler it returns. While the mark stands, the account has no priority and no penalty: it ranks exactly as it would without §86. The mark is never cleared early, and a later new session on the account extends it. Without the mark, every new session of the next minutes would go to the same account, because usage is read again at most every 2 minutes (§26) and a just-used account still reads untouched for 2.5 minutes or more. When the mark has expired and a reading still says unstarted, the priority comes back, so a first request that never reached the provider is tried again. The rate-limit marks of §35.3 are not reused, because they demote the account. §86.2 sets the marks where the proxy places a session.

Residuals. The mark is set after the pick, not as part of it, so two new sessions placed at the same instant can both land on one unstarted account. Both spend a full week, so nothing is lost. A score carried forward after failed usage reads keeps its last reading, so an account that last read unstarted gets one session with priority every 10 minutes until a reading succeeds. The marks are not persisted, so after a restart the first new session can go to an account that a session started minutes before.

Tested in Docker, one test per behaviour in `selectacct/unstarted_week_test.go`. `TestAnUnstartedWeekIsReadFromTheWindow` reads the fact from Codex and Claude windows, a week one second in, the cases above that are not unstarted, two weeks of equal budget of which one has started, and the Spark pool. `TestAnUnstartedWeekTakesTheNextNewSessionFirst` ranks an unstarted account first against an account mid-week with budget above even pace, an account in its last day, a reset holder in early burndown, and, as a peer's account, against a local account mid-week. Each case first checks that with the switch off the rival leads. `TestAnUnstartedWeekDoesNotJumpTheClausesAboveIt` has it lose to machine policy, a worse selection tier, a closed admission queue, a capacity mark, the new-session test, a machine binding and an account already shared. `TestPickSteadyGivesAnUnstartedWeekTheSamePriority` covers `PickSteady`. `TestANewSessionOnAnUnstartedWeekMarksItForTenMinutes` places a session, then ranks the same pool with the reading unchanged at once and 1 second before 10 minutes, and expects the order with the switch off and no mark. At 10 minutes the priority is back, and a reading whose week runs ends it with the mark gone. A session on a running week sets no mark. `TestTwoUnstartedWeeksTakeOneNewSessionEach` places three sessions on two unstarted accounts and one mid-week: one each, in the order of the clauses below, then the mid-week account. `TestTheStartSwitchOffRanksAsBefore` compares rankings and clauses over 1,500 random pools with and without the fact and the marks, with the switch off, and checks that the same pools differ with it on. `TestTheStartClauseKeepsTheRankingAStrictWeakOrdering` sorts every permutation of 300 random pools of 3 to 5 accounts in both modes, with unstarted, marked, draining, capped, capacity-marked and peer accounts, and checks that no pair sorts each before the other. `TestCodexPrefersTheSoonestRefresh` now expects the pristine account first, and with the switch off, last, as before. `cmd/subrouter/start_unstarted_week_test.go` reads the variable and checks that `serve` sets the switch. Each part was taken out in turn and its test went red for the predicted reason: the fact never read; `>` for `>=`; a tie of equal budget that keeps the untouched reading; the Claude branch without the fact; the clause taken out; the clause below the drain (the last-day and reset-holder cases failed, the mid-week case passed); the clause above the tier (six cases failed, machine policy passed); no mark set; marks ignored by the ranking; marks not attached by `Get`; a mark on every session; a mark that never expires in the ranking; a mark of 5 and of 15 minutes; a marked account demoted whatever the switch says; the clause without the switch; and `serve` that never sets the switch.

§86.2 The wire, the peer path and the status surfaces.

On 2026-11-22, 7 of the owner's 8 Codex accounts sat on one peer, and one of them had not started its week, so the daemon that places a session must see a peer's fact. It could not. The `normalized` block published `seconds_to_weekly_reset` counted from the score's age, so an untouched week read below its length a second after it was scored, and it published no window length. The pool board drew such an account as PRISTINE by a rule of its own (`readFuse` on the raw windows), and nothing else read it.

The wire. Each `usage-status` row's `normalized` block carries `weekly_unstarted`, which is `Score.WeeklyUnstarted`, left out when false. A quota pool's entry carries its own pool's week, such as the Spark pool's. The value is the reading. It is published whatever the switch says and whatever start marks the daemon holds. A peer reads it into the account's `Remote` score (`remotePoolNormalized.score`), and the clause of §86.1 ranks it in the one ranking of §31. A pool entry never takes its account's value (`inheritPeerAccountFacts` copies the weekly pair, not this). A body without the field comes from a daemon before §86, and its accounts rank exactly as before. The reading is published, not the reading less the owner's marks, because `srq`, the board and the peer all show it, and with the switch off it must still be reported. The cost: each other daemon that places sessions may send the account one session of its own before the reading turns. Only then does its own mark stop it. With a laptop and one box that is at most two sessions in the window, each on a full week.

The peer path. Each peer candidate carries its scheduler provider (`crossPoolCandidate.Provider`), so its ID in the merged ranking, and with it its start mark, is the same with and without local candidates. A ranking of peers alone (`serveAcrossPeers`, the transport stage after local failover gave up, refugee placement) starts from this daemon's start marks (`SchedulerRef.WithStartMarks`), not from a bare scheduler. When a new session's forward is targeted at a peer's account and the peer serves it on that account, the deciding daemon marks the account under its merged-ranking ID, so per peer (`notePeerTargetServed`). It does so when the peer's answer headers arrive, or for a websocket when the client's upgrade completes. It does not wait for the answer to end, because one websocket carries a whole conversation. A refusal or a failed attempt marks nothing, and the recompute of §31 P6 picks again without the refused account. An answer the peer served on another account marks nothing either. On the peer, `selectTargetedAccount` marks its own account when it accepts a targeted new placement, so its own next pick does not follow. A continuing session and a cold pin are not new placements there and mark nothing.

The local pick. Selection marks the account it places a new session on, including a cold session that moves (`noteNewLocalSession`, called where the session's account changes). When the cross-pool ranking sends the turn to a peer's account, the local pick only holds the session's pin, so its mark waits. It is set only if every peer attempt fails and the pick serves the turn (`serveCrossPoolDecision`). A warm session is never ranked again (§20), so it never moves for this. A cold session that ranks again can be placed by the clause, and that marks the account. `NoteNewSession` now reports whether it set a mark, and `SchedulerRef.StartSentUntil` reads a mark's expiry.

The status surfaces. The board reads the fact the ranker reads. A row's `state` is `pristine` when its score says `WeeklyUnstarted`, its fuse gauge reads PRISTINE or RUNNING by the same fact (`readFuse` takes it from the score, and the Spark gauge from the Spark pool's score), and the row carries `weekly_unstarted`. Its `state_note` says one of four things: the next new session goes there; a session was sent and the priority is off until a given time; the switch is off here; or, for a box from before §86, this daemon ranks it on its pace. `rank` and `reason` already name the clause where it decided. Each mark logs `weekly window not started; this new session starts its clock` with `account`, `peer` and `priority_off_until`, and the `remote pool decision` line names the clause in `clause`. `srq pool` prints `unstarted` beside the roll, with a legend line, and `srq pool --json` carries `weekly_unstarted`.

`SUBROUTER_START_UNSTARTED_WEEK=0` turns off the clause and the marks, local and peer alike. The fact is still published and shown. Either deploy order is safe: a box from before §86 publishes no field, and a laptop from before §86 ignores it. The convention stays the box first, then the laptop.

Residuals. A peer's mark is set when its answer headers arrive, so new sessions decided while the first one waits for them can target the same account, as with the peer's live debit (§31, round 3). Usage-limit failover's next account (`oauthCandidate`), a refugee placed on a local account, and a tenant credential lease set no mark. A session placed there starts the week, and new sessions of the next few minutes can follow it until a reading turns.

Tested in Docker, in `internal/proxy/unstarted_week_test.go`. `TestUsageStatusPublishesWhetherTheWeekHasStarted` checks the JSON of `withNormalizedUsage`: `true` for an untouched week, left out for a running week and for no weekly window, and the Spark pool's own entry. The same body decoded as a peer reads it gives only the untouched account `WeeklyUnstarted`, and the body without the field gives none. It is still published with a start mark set and with the switch off. `TestAnUnstartedCodexWeekTakesOneNewSession` runs a daemon over stored Codex accounts measured through its real usage read, with a fake usage endpoint that reports reset equal to the length for one account and 20% spent at 300,000 s for the other. pool.json shows the first account `weekly_unstarted`, pristine, rank 1 with the clause, and usage-status publishes the field. The first new session goes to it and the second to the other account, one mark log line is written, and the board's note says the session was sent. Once the endpoint reports the clock running, the field and the pristine state are gone and the next session goes on pace. `TestTheStartSwitchOffPlacesANewCodexSessionAsBefore` runs the same pool with the switch off: the first session goes on pace, no mark is set, and the fact is still published and shown. `TestANewSessionIsTargetedAtAPeersUnstartedWeekOnce` puts a laptop with one Codex account 20% spent against a box that publishes an untouched account. The first new session is forwarded targeted at it, and the decision line names the clause. The second session stays local, and the laptop marks the box's account per peer and logs it. A box that withholds the field, the switch off here, and a box that serves the turn on another account change nothing from before and mark nothing. `TestANewWebSocketSessionIsTargetedAtAPeersUnstartedWeekOnce` does the same over websocket upgrades, the way Codex opens its model turns: the first new session's upgrade is forwarded targeted at the box's untouched account, and a second new session opened while the first socket is still open stays local. `TestAPeersUnstartedWeekTakesOneSessionWhenLocalCannotServe` ranks the box's accounts alone, for Codex and for Claude: the untouched account first, then the other one. `TestARefusedTargetOnAPeersUnstartedWeekMarksNothing` has the box refuse the target. The turn is served locally, and once the box's next body clears the refusal, the account still takes the next session first. `TestOnlyATurnBackFromAPeerMarksTheLocalUnstartedWeek` leaves an untouched local account unmarked while the box serves the session, and marks it when the box refuses and the turn comes back to it. `TestAPeerMarksItsUnstartedWeekWhenItAcceptsATarget` has a Codex daemon accept a targeted turn on its untouched account: its own next new session goes to its other account, and it still publishes the reading. `TestOnlyAColdSessionMovesToAnUnstartedWeek` keeps a warm session on its account, and moves and marks a cold one. `TestTheBoardSaysWhatThisDaemonDoesWithABoxsUnstartedWeek` covers a box row's note. The fuse tests of `pool_ui_test.go` now read the fuse through the score. `TestTheFuseFollowsTheRouterWhereItsOwnWindowSaysOtherwise` gives the board three readings where the window the fuse draws says the opposite of the router's fact: two weekly windows of which the binding one has started, a reading from before its own reset, and two of which the binding one has not started. PRISTINE follows the router in each. `ops/bin/srq selftest` covers `unstarted` beside the roll, the column alignment, both legends, `--json`, and a daemon without the field. Each part was taken out in turn and its test went red for the predicted reason: the field never published; published only with the switch on; hidden while this daemon's mark stands; never read from a peer; the local pick unmarked; the local pick marked though a peer's account won; the turn back from a peer unmarked; a served peer target unmarked, over HTTP and over a websocket; a websocket's target marked only as its socket closes; a refused target marked; a substituted answer marked; a ranking of peers alone without the marks; a placeholder that ignores its provider (the Claude case failed, the Codex case passed); the peer's acceptance unmarked; the board's state, the fuse and the Spark fuse not read from the score's fact (the Spark case failed alone when the account's fact was read for it); the fuse back to its own window rule (all three disagreeing readings failed); the board's note blind to the mark and to a box from before §86; and no log line. The srq column and its legend were each taken out of a copy, and two fixtures failed each time.

- Knobs: `SUBROUTER_START_UNSTARTED_WEEK` (unset, `1`, `true`, `yes`, `on` is on, the default; `0`, `false`, `no`, `off` ranks exactly as before §86 and sets no mark, local or peer, while `WeeklyUnstarted` is still read and published; any other value stops the daemon from starting; a change needs a restart). The mark's length is `StartSentMarkDuration`.
- Code: `selectacct/limits.go` (`bindingWeekly`, `scoreFromLimitWindows`), `selectacct/unstarted_week.go` (the switch, `StartSentMarkDuration`, `NoteNewSession`, `startsWeek`), `selectacct/explain.go` (`compare`), `selectacct/scheduler_ref.go` (`Get`), `cmd/subrouter/main.go` (`startUnstartedWeekEnabled`); §86.2: `internal/proxy/unstarted_week.go` (`noteNewLocalSession`, `notePeerTargetServed`, the log line), `internal/proxy/proxy.go` (`NormalizedUsage.WeeklyUnstarted`, the local pick), `internal/proxy/remote_pool.go` (`remotePoolNormalized`, the marks on a served forward), `internal/proxy/remote_pool_decide.go` (`crossPoolCandidate.Provider`, `peersOnlyScheduler`, `serveCrossPoolDecision`), `internal/proxy/target_placement.go` (`selectTargetedAccount`), `internal/proxy/pool_ui.go` (`readFuse`, `classifyPoolState`, `noteUnstartedRow`), `ops/bin/srq`
- Tests: `selectacct/unstarted_week_test.go`, `selectacct/codex_weekly_test.go` (`TestCodexPrefersTheSoonestRefresh`), `cmd/subrouter/start_unstarted_week_test.go`, `internal/proxy/unstarted_week_test.go`, `internal/proxy/pool_ui_test.go` (the fuse tests), `ops/bin/srq selftest`
- Docs: `ops/OPERATING.md`, `ops/skills/subrouter/SKILL.md`
- Related: amends §10 and §31 P2 (an unstarted week no longer ranks as an even pace), §41 and §56 (it ranks above the drain), and §13 and §31 (one more `normalized` field, read from a peer).

## 87. Every view names an account by its label, not its ID

A Codex OAuth login is keyed by `accounts.CodexOAuthIdentifier`: `codex-owner-` plus a SHA-256 of the user and workspace, so that one person's two workspaces stay apart. Every path that adds a Codex record goes through it. The usage-status rows publish that key in `id` and `email`, and the name a person knows, normally the login email, in `label`. Older Codex records and every Claude account use their name as their ID, so the readers named each row by its ID, and an owner-keyed account showed as a hash. `sr plot`'s "unspent · expiring soonest" printed `codex-owner-` and 18 hex digits where the account's email belonged.

§87.1 `sr plot` names each account by its label and falls back to its ID. Both renderers do this, `internal/burnpace` and `ops/bin/sr-plot`, kept in parity, and so do `srq pool`, `srdash` and the pool board: the account rows, the move dialog, the flash message and the Sessions tab. The Sessions tab gets the label through a new `account_label` on each session. Peer rows carry the label the box published. Commands and files that take an ID keep the ID: pins, `srq dir`, `srq serving` and the prune preview.

§87.2 The CLI views show the label at once. The pool board's Sessions-tab label is filled in by the daemon, so it shows after that daemon's next deploy.

- Code: `internal/burnpace` (`model.go`, `render.go`, `snapshot.go`, `week.go`), `ops/bin/sr-plot`, `ops/bin/srq`, `ops/bin/srdash`, `internal/proxy/pool_ui.go`, `internal/proxy/pool_ui.html`, `internal/proxy/proxy.go` (`AccountStatus.Label`)
- Tests: `TestPlotNamesAnAccountByItsLabel`, the `codex-owner-labels` parity scenario, `TestBoardCarriesEachAccountsLabel`, `sr-plot --selftest`, `srq selftest`
- Docs: `ops/OPERATING.md`

## 88. claude-route proves the pool on a Mac without the Paradev app

`claude-route check` and `claude-route check codex` launched through `~/.paradev/bin/claude` and `~/.paradev/bin/codex`, and `claude-route test` had cases on that path. On a Mac without Paradev they failed. `claude-route pool` and `claude-route codex pool` refuse until those pass, so such a Mac could switch only with `--force`, which also skips the checks that do apply.

§88.1 `launch_front` looks for an executable wrapper in `~/.paradev/bin`, `~/.paradev-*/bin`, `~/.superset/bin` and `~/.superset-*/bin`. With one, the checks launch the way a new Paradev session does: wrapper, then shim, then the real binary, as before. Without one, they launch the way a new terminal does: shim, then the real binary. `check` prints which path it proved. In `test`, the nine checks about the wrapper itself print `SKIP ... (no Paradev wrapper)` and count as neither a pass nor a failure. Neither gate changed: a check that applies and fails still blocks it.

§88.2 The launches that `test` and `check` make now keep `CLAUDE_ROUTE_SOCKET`, `CLAUDE_ROUTE_TCP` and `CLAUDE_ROUTE_PLIST`, as they already kept `CLAUDE_ROUTE_HOME`. Before, they dropped them. With the daemon moved, the shims they launched went to the default socket and port, not to the daemon `claude-route` was checking.

§88.3 `check` and `check codex` take `srq serving`'s answer as the pool account only when it names one. They used to print and accept srq's "unassigned (session ...)" line as an account, so a launch that the pool never served could pass.

§88.4 `test`'s check on a pin to a spent or held-out laptop account passes in exactly two states: the session is served by exactly the pinned account, or it has no row and the launch's answer is the daemon's refusal of the pin (§89 refuses such a pin before any assignment). A row on any other account, a launch that never reached the daemon, or an `srq` that cannot answer each fail it. `served` tells srq's "unassigned" answer apart from its other failures, and `unserved` builds on it.

- Code: `ops/bin/claude-route` (`launch_front`, `build_launch_env`, `cmd_check`, `cmd_check_codex`, `cmd_test`)
- Tests: `ops/deploy/tests/deploy-scripts-test.sh` (`route-check-without-paradev-launches-through-the-shim`, `route-test-without-paradev-skips-only-the-paradev-cases`, `route-check-and-test-with-paradev-go-through-its-wrapper`, `route-check-finds-paradev-build-and-superset-wrappers`, `route-failed-check-without-paradev-blocks-the-gate`), `ops/deploy/tests/deploy_scripts_test.go`
- Docs: `ops/GETTING-STARTED.md`, `ops/OPERATING.md`

## 89. A pin on an account inside its owner's holdout is answered once, finally

A request with the caller's own pin (`X-Subrouter-Account-ID`) on an account inside its owner's holdout is refused by selection (§20), and a pin is never moved to another account. The refusal left as the ordinary no-account 503, which clients retry. Claude Code stopped after ten tries, inside a minute, and that was all that made a pinned launch on such an account fail fast. A client that waits hours for a daemon that is gone (`CLAUDE_CODE_RETRY_WATCHDOG=1` with no retry limit, set on the owner's laptop on 2026-11-22) asks again for as long: `claude-route test` measured 13 identical refusals in 4 minutes on Claude Code 2.1.330, and its check "pinned (spent) fails fast" failed at 241 s against 45 s.

The holdout ends when the account's week rolls, or in its last day (§60), not between two tries. The daemon now answers such a pin once, with `X-Should-Retry: false`, as §28 answers a pin whose credential is dead. The message names the account, says it is inside its owner's holdout, gives the time its week rolls when that is known, and says how to pick another account or launch pooled.

- Claude: 429 `rate_limit_error` in Anthropic's shape. Measured on Claude Code 2.1.330 with the long wait on, against a test server: with the header the client stops after one request and prints the message; the same 429 without the header, and the old 503, are retried until the launch is killed.
- Every other provider: §28's 403, with code `account_held_out`.
- The gate is §28's: only the caller's own selector. A session lease, or a placement another daemon made, keeps the retryable 503 and recovers by asking again.

Not changed: a pin on an account that is not found, that cannot serve ChatGPT backend paths, or that is identity-only still gets the 503. A pin on an account above its holdout that the provider then limits still gets the provider's own answer.

- Code: `internal/proxy/pinned_account_held_out.go`, `internal/proxy/proxy.go` (`pinnedAccountHeldOutError`, the handler's no-account branch)
- Tests: `internal/proxy/pinned_account_held_out_test.go`. `TestPinnedClaudeAccountInsideItsHoldoutIsAnsweredOnceAndFinally` and `TestPinnedCodexAccountInsideItsHoldoutIsAnsweredOnceAndFinally` fail with the final answer taken out (status 503). `TestLeasedRequestOnAHeldOutAccountStaysRetryable` fails with the gate on the caller's own selector taken out (status 429).

## 90. The daemon starts an unstarted Codex week itself, with one tiny request

The owner, 2026-11-22, on the offer to build this: "Daemon sending a tiny request by itself to start a Codex clock seems based." It follows their earlier request: "accounts that are at exactly 7 days need some traffic as soon as possible to start it." A Codex weekly window starts on the account's first request, at first and again after every weekly roll, so an idle account is unstarted once a week. §86 gives such an account the next new session, but in a quiet pool that session can be hours away, and each hour it waits is an hour of the week the pool cannot spend. Now the daemon that holds the account sends one tiny Codex turn on it as soon as a reading says its week has not started, so the 7-day clock starts at once.

§90.1 The sender and its rules.

Who sends. Only the daemon that holds the account's credential, for its own accounts. `RunSelfStartWeek` reads this daemon's own Codex OAuth accounts and never a peer's. A peer runs the same build and sends for its own.

When. `serve` runs a pass every 10 seconds (`selfStartScanInterval`). A pass reads the measured scores (`SchedulerRef.RefreshSeed`: the snapshot the usage reads publish, without the routing overlays), so it adds no usage fetch. An account is due when its reading says the binding weekly window has not started (`Score.WeeklyUnstarted`, §86.1), the reading was taken after the account's last send (`Score.MeasuredAt`), the wait after that send has passed, and no start mark stands. On a daemon that has not sent yet, the first such reading is due, at startup too.

What. One minimal Codex turn on exactly that account, as measured on 2026-11-22: the body `{"model":…,"stream":true,"store":false,"input":[one user message "Reply with one word: ok"],"reasoning":{"effort":"low"},"text":{"verbosity":"low"}}` with no instructions, no tools and no `include` (`selfStartBody`), and the headers `originator: codex_exec`, `session-id` and `thread-id` set to one new UUID, `accept: text/event-stream` and `content-type: application/json`. The model is `SUBROUTER_SELF_START_MODEL`, or `gpt-6.2-sol` (`SelfStartDefaultModel`) when that is unset. The turn leaves the daemon the way a turn with the caller's own pin on that account does: the same credential refresh (`refreshAccount`, which is all a pinned forward's refresh does, and which keeps the store's credential isolation), the auth headers a forward adds (`setAccountAuthHeaders`: the account's bearer token and `ChatGPT-Account-ID`), the account's upstream and path (`upstreamForRequest`, `pathForUpstream`), and the outbound transport. It is not sent through the handler. Even with a pin and `X-Subrouter-No-Retry`, the handler writes a session assignment, begins an active session, admits the turn, records the request and its answer in the client-exchange transcript, records token usage and wraps the turn in the hold (§58). The turn is one round trip with no retry, and a 60-second limit covers the refresh, the round trip and the answer. It never goes to another account, a peer, Azure or any other fallback. The answer is read to its end and thrown away, never parsed or logged.

The mark. A send claims §86's start mark at send time (`SchedulerRef.ClaimStartMark`), so new sessions are not also pulled to the account for the next 10 minutes, and the next pass does not send again. The claim fails while a mark stands, so a new session that took §86's priority first leaves the daemon nothing to send: one start per mark, whoever comes first. Two senders with separate memories, as two worker processes of one daemon would have, still send once. The claim does not read §86's switch: with the priority off, the daemon still sends and still marks.

Again, with a growing wait. When the mark has expired and a newer reading still says unstarted, the daemon sends again. The sends in a row on one account wait 10 minutes, then 20, 40 and so on, at most 6 hours (`selfStartWait`). A failed send (a network error, a 4xx, a 5xx, or a credential refresh that failed) counts as a send. A newer reading that says the clock runs ends the run, and the next unstarted week starts again at a count of zero.

Never for: a peer's account; an account whose credential is known dead (`terminalCredFailure`) or that the router would not route to (`BlockedUntilFor`: a credential the upstream refused, a persistent denial, an exhaustion mark); an account under a capacity mark or a rate-limit mark; an API-key account; an account with no weekly window, or a usage-based seat, neither of which ever reads as unstarted (§86.1). Identity-only (`SUBROUTER_IDENTITY_ONLY`) names a Claude account, and the turn is a Codex turn, so that account never gets one. Nothing is sent with a credential broker (team mode, where the daemon does not refresh local credentials), while the daemon drains or is quiesced, or with the switch off.

No trace as a session. The send writes no session assignment, so `/_subrouter/sessions` lists no row for it, and it begins no active session and takes no admission slot. A failed send moves no session and marks no account exhausted, rate-limited or dead.

Tested in Docker, one test per behaviour in `internal/proxy/self_start_week_test.go`, over §86's daemon measured through its real usage read (`unstartedCodexDaemon`, whose fake upstream now records each request). `TestTheDaemonStartsAnUnstartedCodexWeekByItself` runs the daemon's own loop with no client request: the upstream receives one request, on the unstarted account's credential at `/responses`, with the measured body byte for byte, the measured headers and no `X-Subrouter-*` header, and nothing more over about twenty more passes. With `SUBROUTER_SELF_START_MODEL` set, the body names that model. `TestTheSelfStartSetsTheStartMark` finds the mark standing for 10 minutes from the send, sends nothing more on newer unstarted readings while it stands, and places a new client session as §86 with a mark does: on pace to an account above even pace (no priority), and still on the unstarted account against one behind pace (no penalty). `TestANewSessionThatStartsTheWeekLeavesTheDaemonNothingToSend` places the new session first and sees no send, against the same pool without the session, which sends. `TestTheDaemonSendsAgainWithAGrowingWait` runs the sender on the test's clock: nothing a second before each wait, a send at it, for waits of 10, 20, 40, 80, 160 and 320 minutes and then 6 hours twice; one Warn line after the third send naming the account, 3 sends and status 200; a reading with the clock running ends the run with one line; the next unstarted week sends at once and again 10 minutes later, counted 1 and 2; and a reading from before the send never sends again, however long the wait. `TestAFailedSelfStartCountsAndMovesNothing` places a session on each account, the failed one included, then answers 500, answers 400 and refuses the connection: each is one Warn line with its status, one upstream request at most, no session moved, nothing marked exhausted, dead or rate-limited, and the next attempts come at 10 and then 30 minutes. `TestTheDaemonSendsNothingForAnExcludedAccount` covers a credential the upstream refused, one whose refresh was refused, a capacity mark, a rate-limit mark and no weekly window, each first without the condition, which sends; a box that publishes `weekly_unstarted` for its account, which gets nothing from this daemon while the same reading on this daemon's own account sends here; and an identity-only Claude account, which gets nothing while the Codex account beside it gets its send. `TestASelfStartLeavesNoSession` compares `/_subrouter/sessions`, the session store, the live session counts and the admitted sessions before and after a send, and then shows a client turn changes them. `TestASelfStartNeverLeavesItsAccount` configures a peer and an upstream that refuses or never answers: the peer receives nothing, and the send ends within its limit. `TestTheSelfStartSwitch` sends nothing with the switch off, pass after pass and from the loop, and sends and marks with §86's switch off. `TestTwoUnstartedWeeksTakeOneSelfStartEach` sends once on each of two unstarted accounts, and once for four readings and four passes with separate memories at the same moment. `TestTheSelfStartAnswerIsNeverLogged` answers 200, 400 and 500 with a marker in a long body: no log line carries it or the access token. `cmd/subrouter/start_unstarted_week_test.go` reads the variable (`TestSelfStartWeekFromEnvironment`) and checks that `serve` sets the switch and the model and refuses an unknown value (`TestServeSetsTheSelfStartWeek`). `cmd/subrouter/self_start_serve_test.go` runs `serve` itself in a child of the test binary (`TestServeStartsAnUnstartedCodexWeekByItself`), over one stored Codex account whose reading reaches it through the startup score snapshot and says the week has not started, with every address but loopback sent to a closed port. With no client request, the fake upstream receives the measured turn on the account's credential, with the model `SUBROUTER_SELF_START_MODEL` names, and the child logs the send.

Each part was taken out of the working tree in turn and its test went red for the predicted reason: a `serve` that never starts the loop (no request came in 90 seconds); the loop without its pass; the model setting ignored; a send that sets no mark; a claim that ignores a live mark (a session's first, and another sender's); a claim that reads §86's switch; a flat wait; a wait without its cap; a run that never ends; no Warn line; a reading from before the send counted as newer; the fact not read; each of the four exclusions in turn; every provider's accounts read (the identity-only Claude account was tried); a failed send not counted; one retry inside the send; a failure that marks the account; a failure that drops the failed account's sessions; the turn sent through the handler with the account's pin and `X-Subrouter-No-Retry` (a row appeared in `/_subrouter/sessions`); the time limit taken out; the switch ignored; one send per pass; the answer put in the error; and a `serve` that never sets the switch, or drops the model. The same run with nothing taken out passed.

§90.2 What the operator sees, the settings and the documents.

The log lines. A send logs `weekly window not started; the daemon sent one request to start its clock` at Info, or `weekly window not started; the daemon's request to start its clock failed` at Warn. Both carry `account`, `provider`, `model`, `status` (0 when no answer came), `sends` (the count in the run), `duration_ms`, `priority_off_until` (the mark's end), `next_send_after`, and, for a failure, `error`. After the third send in a row, a newer reading that still says unstarted logs `weekly window still not started after the daemon's requests` once, at Warn, with `account`, `sends`, `last_status` and `next_send_after`. After a send, a newer reading with the clock running logs `weekly window started after the daemon's request`, with `sends`. None carries the answer, the request body or the credential, and none names a session: each is the daemon's own request.

`SUBROUTER_SELF_START_WEEK=0` turns it off: no pass sends, and the start marks are only §86's. Unset, `1`, `true`, `yes` or `on` is on, the default. `0`, `false`, `no` or `off` is off. Any other value stops the daemon from starting, as §86's switch does, and startup logs one line when it is off. It is independent of `SUBROUTER_START_UNSTARTED_WEEK`. `SUBROUTER_SELF_START_MODEL` names the turn's model. A change to either needs a restart.

The wire. Each `usage-status` row whose `normalized` block says `weekly_unstarted` carries `self_start` once this daemon has sent its turn on the account: `{"sent_at": ..., "sends": N}`, the last send to the second in UTC and the count in a row (`NormalizedUsage.SelfStart`, `SelfStartSent`). One object holds both, so neither is published without the other. It is left out before the first send, for a week that runs, by a daemon from before §90, and at once when a reading shows the clock running: it reads the reading, so it does not wait for the sender's next pass to end the run. A quota pool's entry never carries it, because the turn starts the account's binding week. The sender's memory moved from its loop onto the account ref (`sharedSelfStarts`), so the loop that sends and the handlers that publish read one memory. A peer ranks on nothing in it: `remotePoolNormalized` has no such field, so the deciding daemon ranks the reading and its own marks, as in §86.2.

The board. A pool.json row carries `self_start` as its daemon's `usage-status` publishes it, for a local row and a peer's row alike (`poolRow.SelfStart`). The `state_note` of a pristine row now first says what its daemon did: `this daemon sent one request to start its clock at 14:05 UTC`, or `its daemon` on a peer's row, with `(3 sent in a row)` from the second send. Then it says what this daemon's ranking does (`unstartedRanking`): it ranks on its pace until the mark ends, where this daemon's own turn set the mark or a session extended it; a new session was sent here; the switch is off here; the peer is from before §86; or `until a reading shows its clock running, the next new session goes here first`. The last one is §86's priority back after the mark, the fallback for a send that did not start the clock. A row with no send reads as before. The note no longer says that a new session was sent where the daemon's own turn set the mark, which §90.1 left it saying. The state, the fuse and the rank still follow the router's fact and marks (§86.2), and a send changes none of them. The page shows `start sent HH:MM` in the browser's time beside the row's PRISTINE fuse, with `×3` for the third in a row, and its legend explains it.

srq. `srq pool` prints `start sent HH:MM` beside `unstarted`, in the time of the machine it runs on, with `x3` from the second send in a row. The column appears only in a section with a send and is padded so the columns stay aligned. Its legend line says whose request it was: this daemon's, or, in a peer's section, the peer's, which still takes a new session from here until its reading turns. `srq pool --json` carries `self_start_sent_at` as published and `self_start_sends`, both null without a send. A malformed `self_start` reads as none.

Deploy order. Either order is safe. A laptop from before §90 ignores the field, and a box from before §90 publishes none, so its rows read as before. The convention stays the box first, then the laptop.

Tested in Docker. In `internal/proxy/self_start_week_test.go`, over §86's daemon measured through its real usage read: `TestUsageStatusAndTheBoardShowTheDaemonsOwnStart` reads `usage-status` and pool.json before any send, where neither carries `self_start`. After a send two hours ago, whose mark has expired, both carry that time and 1 send, the note says this daemon sent at that time and that the next new session goes there first until a reading shows the clock running, and the row is pristine with a PRISTINE fuse, ranked first by §86's clause. After a second send now, on a newer reading that still says unstarted, both carry 2 sends at the new time, and the note names the count and the mark's end, with the priority off. Once the usage endpoint reports the clock running, neither carries it, before and after the sender's next pass, and the fuse runs. `TestAPeersOwnStartIsShownAndRanksNothing` runs two such daemons, a box with an unstarted account and a laptop with an account above even pace, and the laptop reads the box through the box's own `usage-status`. After the box's send, the laptop's row for the box's account carries the box's `self_start` and a note that its daemon sent at that time, and its fuse stays PRISTINE. The board's cross-pool ranking still targets the box's account first by §86's clause, exactly as before the send. `TestEachSelfStartLogsWhatTheOperatorReads` reads the Info line of two sends in a row (account, model, status 200, `sends`, `duration_ms`, and `next_send_after` 10 and then 30 minutes after the first send) and the one Info line with `sends` 2 once the clock runs. `TestTheDaemonStartsAnUnstartedCodexWeekByItself` now also finds the loop's send in `usage-status`. The §86 tests of the board's note pass unchanged. `ops/bin/srq selftest` covers `start sent HH:MM` beside `unstarted`; the alignment beside an unstarted row with no send and a running row; `x3`; both legends; `--json`; six malformed shapes; and a daemon from before §90. `TestTheBoardPageShowsTheDaemonsOwnStart` runs the board page the daemon serves, its whole script, in node on the pool.json the daemon serves (`internal/proxy/testdata/pool_ui_render.mjs`, with the browser's clock on UTC). The row shows no tag before a send, `start sent HH:MM` beside the PRISTINE fuse after the first, `×2` after the second, and nothing once the clock runs, and the running week's row never shows it. The test is skipped where there is no `node` on the PATH. The Docker test image has none, so its proofs ran in that image with a node binary mounted; GitHub's runners have one.

Each part was taken out of the working tree in turn and its test went red for the predicted reason: the field never published (both tests); published for a week that runs; the loop with a memory of its own (`usage-status` never saw its send); the board's row without the field (both tests); the note blind to the send (both tests); the note without the count; the note calling the daemon's own mark a session's; a peer's row named as this daemon's; the send dropped from the note once the mark expired; a peer's `self_start` read into its ranking as a mark (the ranking moved off the box's account); `next_send_after` in the wrong format; the model left out of the line; and the started line at Debug. In `srq`: the column taken out, its padding, its legend, the field never read, and the count. In the page: the tag taken out, and the count taken out of it. The same runs with nothing taken out passed.

Proven live, 2026-11-23. A turn this small starts the clock of an unstarted week, and the backend accepts it without a client's `User-Agent`. On the first account whose week rolled after the deploy, the daemon that holds it sent its one request 15 seconds after the roll: `weekly window not started; the daemon sent one request to start its clock`, with `model` `gpt-6.2-sol`, `status` 200, `sends` 1 and `duration_ms` 2214. No session was on the account. The next reading, 37 seconds later, showed the week running, and the daemon logged `weekly window started after the daemon's request` with `sends` 1. A reading three minutes after the roll gave the weekly window 604,609 seconds to its reset, 191 seconds into a new seven days, at 0% used. The daemon's turn sets no `User-Agent`, so Go's default went out, and the status was 200. One account, one roll. Before this the turn had only been shown to be accepted: on 2026-11-22, status 200 in 2.1 s with 12 input and 5 output tokens, on a day when no account was unstarted. Not seen live: the second and third sends and the Warn after the third, which only the tests cover.

Residuals. Only the account's binding weekly window is read and started. A model pool's own week, such as the Spark pool's, is not read, and a turn on the default model is not expected to start it. A deciding daemon may still target one new session at a peer's unstarted account in the minutes before the peer's reading turns: the peer's own send sets the peer's mark, which the deciding daemon does not see, and the peer's `self_start` is shown, not ranked on. With a laptop and one box that is about one more session (§86.2's own residual names the others decided while it waits), and it lands on a full week. The marks and the run counts are not persisted, so a daemon that restarts minutes after a send can send once more, and so can the new worker of a swap, whose `self_start` then counts from 1 again. A reading with no `MeasuredAt` never counts as newer than a send, so after a send such readings send nothing until a measured one arrives.

- Knobs: `SUBROUTER_SELF_START_WEEK` (on by default; `0` off; any value other than unset, `1`, `true`, `yes`, `on`, `0`, `false`, `no` or `off` stops the daemon from starting), `SUBROUTER_SELF_START_MODEL` (default `gpt-6.2-sol`). The waits are `selfStartFirstWait` (the start mark's 10 minutes) and `selfStartMaxWait` (6 hours), the limit `selfStartTimeout` (60 seconds), the pass interval `selfStartScanInterval` (10 seconds).
- Code: `internal/proxy/self_start_week.go` (`RunSelfStartWeek`, `selfStartWeeks`, `selfStartAllowed`, `sendSelfStart`, `selfStartBody`, `selfStartWait`, `SetSelfStartWeek`, `sharedSelfStarts`, `SelfStartSent`), `selectacct/unstarted_week.go` (`ClaimStartMark`), `cmd/subrouter/main.go` (`selfStartWeekEnabled`, `selfStartScanInterval`, the loop), `internal/accounts/codex_reset.go` (`NewUUIDv4`, exported for the turn's IDs); §90.2: `internal/proxy/proxy.go` (`NormalizedUsage.SelfStart`, `withNormalizedUsage`, `AccountRef.selfStartMemory`), `internal/proxy/pool_ui.go` (`poolRow.SelfStart`, `noteUnstartedRow`, `unstartedRanking`), `internal/proxy/pool_ui.html`, `ops/bin/srq` (`_self_start`, `_start_sent`)
- Tests: `internal/proxy/self_start_week_test.go`, `internal/proxy/unstarted_week_test.go` (the fixture records the upstream's requests and reads the whole board), `internal/proxy/testdata/pool_ui_render.mjs` (the board page in node), `cmd/subrouter/start_unstarted_week_test.go` (`TestSelfStartWeekFromEnvironment`, `TestServeSetsTheSelfStartWeek`), `cmd/subrouter/self_start_serve_test.go` (`TestServeStartsAnUnstartedCodexWeekByItself`), `ops/bin/srq selftest`
- Docs: `ops/OPERATING.md`, `ops/skills/subrouter/SKILL.md`
- Related: builds on §86 (the fact, the start mark and its 10 minutes), and amends §86.2 (the board's note says whose request set the mark) and §13 and §31 (one more `normalized` field, shown by a peer and never ranked on). Never held (§58). The turn is the pinned forward's (§20: a pin never moves).

## 91. Every saved reset an account holds is shown, with its kind and expiry

The owner, 2026-11-22: "the idea is that subrouter handles all your account resetting for you". Their brief says resets come in kinds, that every kind differs, and that new kinds will appear. It says a Claude account holds at most one week reset, and that a count above one is Fable resets queued behind it, which the provider uses after the week reset. It says Codex resets have expiry dates that must be read from Codex. Every later step of that plan (claim at the wall, decide when, the expiry burndown) needs every reset an account holds, its kind and its expiry. Before this section usage-status showed less. A Claude row's `complimentary_reset` carries a count, a split by kind (§64) and only the soonest expiry. A Codex row carries a count with no date, and nothing read a credit's `reset_type`. This section reads, records and shows. It claims nothing, moves no session and ranks nothing. The owner has not approved automatic claiming.

§91.1 The kinds table, the full list and the Codex expiry reader.

The table. Each kind is one entry in `internal/accounts/reset_kinds.go` (`ResetKindSpec`): the provider, how a reading is recognized (`Matches`), the meters a claim gives back when the reading names none (`GivesBack`), whether a claim starts a new seven days (`RestartsClock`), whether a claim is accepted only at the limit when the reading does not say (`NeedsLimit`), and the kind's place in the provider's order of use (`UseOrder`). The first entry that matches decides. There are three entries.

| Kind | Recognized by | Gives back | Restarts the 7 days | Only at the limit | Order |
|---|---|---|---|---|---|
| `week` (Claude) | a grant whose `clears` names `seven_day`, or names nothing | the grant's `clears` | assumed no | the grant's `use_requires_limit` (default true) | 1 |
| `fable` (Claude) | a grant whose `clears` names `seven_day_overage_included` and not `seven_day` | the grant's `clears` | assumed no | the grant's `use_requires_limit` | 2 |
| `codex_rate_limits` | a credit whose `reset_type` is `codex_rate_limits`; before the list is read, the usage reading's count under `rate_limit_reset_credits` | `weekly` | assumed yes | no | 1 |

Both clock values are assumed: no claim has recorded the roll time before and after, and §92's claim records are to settle them. The Codex `weekly` is what the redeem code and its fakes assume (`reset_autospend.go`), not a measurement. `ResetGrant.Kind` (§64) now reads the same table, with its rule unchanged, so `remaining_by_kind` and the list cannot disagree. A new kind is one new entry. `accounts.UseResetKinds` swaps the table for a test, and the daemon never calls it.

An unknown kind. A Claude grant the table does not know (§64's `other`), and a Codex credit whose `reset_type` it does not know or that has none, is listed as kind `unknown`. The entry carries `raw_type` (the provider's own type, as the provider wrote it), `gives_back` (what the grant says it clears), `restarts_clock` `unknown`, no place in the order, and `manual_only: true`. A Claude grant keeps its `type` verbatim for this (`ResetGrant.RawType`, never encoded), while the grant's `type` still reads as Claude Code reads it: `session`, `weekly` or `full`, and anything else as absent. Nothing drops or guesses one. Each logs `saved reset of a kind this daemon does not know; shown, never claimed automatically` at Warn with `account`, `provider`, `raw_type`, `clears`, `count` and `expires_at`. It logs at most once an hour per account and kind (provider, account, raw type and what it clears), however often usage-status is read.

The list. Each usage-status row carries `resets`, one entry per kind and expiry with its count. An entry has `kind`, `count`, `expires_at` (verbatim), `expiry`, `gives_back`, `restarts_clock`, `needs_limit` (absent when nothing says) and `order`, plus `expiry_read_at` and `expiry_age_seconds` on a Codex date. `order` starts at 1: Claude's week reset, then its Fable resets, soonest first, and Codex's credits soonest first, the order subrouter's redeem takes them in. `expiry` is `read`, `last_good` (the latest list read failed and the date is from the earlier good one), `not_read` (no good read yet, or the reader is off), or `none_given` (the reading carries no date). An account with a week reset to 2026-12-10 and two Fable resets to 2027-03-09 lists two entries, the week one first, whatever order Anthropic lists the grants in. A spent grant and one past its date are not listed. A paused grant, and a grant on an account Anthropic calls ineligible, are listed, as `remaining_by_kind` counts them; eligibility stays on `complimentary_reset`. `[]` is a reading that holds none. The field is absent when there is no reading, as `complimentary_reset` is absent then: no block, Anthropic's `unavailable`, a setup token, a Codex reading with no count, and a Claude row served from the persisted usage snapshot before the first sweep, which keeps the block but not the list. `complimentary_reset` is byte for byte what it was. The list is read with the same block and rides beside it (`ComplimentaryResetInfo.Held`, never encoded), and `withHeldResets`, the last step of usage-status, puts it on the row.

The Codex expiry reader. A Codex usage reading carries a count (`rate_limit_reset_credits.available_count`) and no date. The dates are in `GET wham/rate-limit-reset-credits`, the list that the redeem path and `/_subrouter/reset-credits` already read. `RunCodexResetExpiryReader` runs a pass every 10 seconds (`codexResetExpiryScanInterval`). A pass reads the counts the last usage sweep published, so it adds no usage read. It reads the list of each of this daemon's own Codex OAuth accounts that is due. An account is due when its count is above 0 and the list was never read, or the count differs from the one the last read was for (at once), or an hour has passed since the last read, whatever that read answered (`codexResetExpiryEvery`). It never reads for a count of 0 or no count, or for an account this daemon holds no credential for: a peer's account is never in its sweep. It reads nothing with a credential broker (team mode), while the daemon drains, or with the switch off. Each read runs in the background, one at a time per account, within 20 seconds (`codexResetExpiryTimeout`), on the account's credential as the sweep last left it. It refreshes no token, so it rotates none. It only reads: nothing here calls the consume route.

A good read replaces the account's list. A failed read (any non-2xx, a timeout, a network error) keeps the last good list, whose dates then read `last_good` with their age. It logs `codex reset credit list read failed; the last good expiry dates stand` at Warn with `account`, `error` and `dates_read_at`, and it marks nothing: no exhaustion mark, no dead credential, no session moved. A credit that is redeemed, or past its `expires_at`, is not listed. Credits the count has beyond the list's are one entry with expiry `not_read`. A count of 0 lists none, whatever the last list said.

Selection. Nothing ranks, routes or claims on the list. The §56 and §64 hold still reads `week_held_until`, and the scheduler is built from the rows before the list is put on them.

`SUBROUTER_READ_CODEX_RESET_EXPIRY=0` turns the reader off. No pass reads, and every Codex count is listed with expiry `not_read`, dates read before it went off included. Unset, `1`, `true`, `yes` or `on` is on, the default. `0`, `false`, `no` or `off` is off. Any other value stops the daemon from starting, as §90's switch does, and startup logs one line when it is off. A change needs a restart. The Claude list has no switch: it comes from the block the sweep already reads.

Not here. §91.2 carries the list to the peer path, pool.json, the board, `srq` and `sr status`. Usage history and burn rate are §92.

Residuals. When a count changes and the read that follows fails, the entries are the last good list, marked `last_good`, and can still name a credit spent since; the count on `complimentary_reset` is the reading's. The reader's memory is not persisted, so after a restart every Codex count reads `not_read` until its first good read, within a pass of the first sweep. A Codex count read before its list is taken to be the kind its key names. A list credit with no `reset_type` is listed as unknown.

Tested in Docker, one test per behaviour, every provider a fake. In `internal/proxy/held_resets_test.go`, over a daemon of stored Codex accounts measured through its real usage read, whose fake ChatGPT backend fails the test on any request to redeem (`creditDaemon`), and over §64's Claude box (`claudeUsageBox`):
- `TestUsageStatusListsEveryClaudeResetByKindAndExpiry`: a week reset and two Fable resets, listed Fable first, give two entries, the week one first. `complimentary_reset` equals the golden text, and the same golden passed against `main` before this change. A block with no grants lists `[]`. An unread block, no block, and every block served from the persisted snapshot list nothing.
- `TestUsageStatusListsEachCodexCreditByItsExpiry`: a count of 3 and a list of three live credits, one redeemed and one past its date, give the three dates soonest first, with their read time and age. Before the read it is one entry of 3, `not_read`. `complimentary_reset` is unchanged.
- `TestTheDaemonReadsTheCreditListByItself`: the reader's own loop reads the list once, with no client request.
- `TestTheCreditListIsReadAtMostOnceAnHour`, `TestTheCreditListIsReadAgainAtOnceWhenTheCountChanges`, `TestTheCreditListIsNeverReadForACountOfZero` (a count of 0, no count, and a count that falls to 0, which also lists none), `TestTheCreditListIsNeverReadForAPeersAccount` (a peer publishing credits is asked for nothing but its usage), `TestTheCreditListIsNeverReadWithoutTheCredential` (a credential broker).
- `TestAFailedCreditListReadKeepsTheLastGoodDates`: a 500, a 401 and a timeout each keep the dates as `last_good` with their age, log one Warn, leave the account unexhausted with a live credential, no rate-limit or capacity mark and its session on it, are not retried inside the hour, and are replaced by the next good read. A list never read well says `not_read`.
- `TestTheCreditListReaderNeverRedeems`: across reads, a changed count and failures, every request about a credit is a GET of the list.
- `TestAnUnknownResetKindIsShownAndWarnedOnceAnHour`: a Claude grant of a type Claude Code does not know that clears `five_hour` alone, and a Codex credit of another `reset_type`, are listed unknown with their own type and flagged, and warned once in 59 minutes and again after 61.
- `TestANewResetKindIsPublishedFromOneTableEntry`: with a made-up Codex kind and a made-up Claude kind added to the table, both are listed as those kinds, in order, with no warning, and `remaining_by_kind` names the Claude one. Without the entries they are unknown.
- `TestTheResetsListChangesNoSelection`: `Explain`, `ExplainSteady`, `Pick` and `PickSteady` give the same order, clauses and picks for statuses with the list and without it: a week holder in early burndown, Fable-only and unknown resets, accounts with none, account-wide and on the Fable pool, and Codex credits read and not read.
- `TestTheCreditListReaderSwitchFallsBackToExpiryNotRead`.
Also `internal/accounts/reset_kinds_test.go` (`TestTheResetKindsTableNamesEveryKnownKind`, `TestHeldResetsListsEachKindAndExpiryInTheProvidersOrder`, `TestANewResetKindIsOneTableEntry`, `TestCreditHeldAt`), `internal/agents/claude/reset_held_test.go` (`TestResetInfoListsEveryResetByKindAndExpiry`, whose golden text also holds an unknown entry, so `raw_type` and `manual_only` are pinned as the wire spells them), §64's `TestResetGrantKindIsWhatItsClaimGivesBack` and `TestResetInfoCountsWhatIsLeftByKind` unchanged, and `cmd/subrouter/read_codex_reset_expiry_test.go` (`TestReadCodexResetExpiryFromEnvironment`, `TestServeSetsTheCodexResetExpiryReader`, and `TestServeReadsACodexCreditListByItself`, which runs `serve` in a child of the test binary, as §90's test does, over a Codex account whose persisted usage reading holds a credit, and waits for the reader's failure line: the child reaches nothing but loopback).

Each part was taken out of a copy of the working tree in turn, one replacement each, and its test went red for the predicted reason: the Claude list never built; the list encoded inside `complimentary_reset` (the golden); the publish blind to the credit list; redeemed and lapsed credits listed; no hour between reads; a changed count left to the hour; a count of 0 read; a count fallen to 0 shown with its old list; the dates of a peer's accounts asked of the peer; reads with a credential broker; a failed read that drops the dates; a reader that redeems (the backend's guard and the never-redeems test); the unknown-kind line on every read; an unknown kind dropped; the grant's kind from conditionals of its own (`remaining_by_kind` disagreed); a table that takes no entry; the hold reading the list (the selection test); the pass and the publish each blind to the switch; a loop that never reads; a block without its list read as holding none; `serve` never setting the switch; `serve` never starting the loop; the grant's type kept only when Claude Code knows it, in the parser and in the table's reading; and a failed read that marks the account rate-limited. The same runs with nothing taken out pass.

§91.2 The peer path, the views and the documents.

The peer path. A peer's usage-status carries the `resets` list of §91.1. The board reads the first peer's rows as `AccountUsageStatus`, list included. `GET /_subrouter/peers/usage-status` (§32) hands every peer's body on verbatim, so `sr status` and `srq resets` read each peer's list through this daemon. Nothing on the decision path reads it. `remotePoolAccountUsage` and `remotePoolNormalized` have no such field, and `inheritPeerAccountFacts` copies none of it, so a peer's list ranks nothing here.

pool.json. Each row carries `resets` in a compact form, for this daemon's rows and a peer's alike (`poolRow.Resets`, `poolResetsOf`). An entry has `kind`, `count`, `expires_at` (when it has one) and `expiry`, one entry per kind and expiry as usage-status lists them. This daemon's rows now go through `withHeldResets`, as usage-status's do, so a Codex row's dates are the reader's. A row whose daemon publishes no list carries what its `complimentary_reset` says, with `resets_partial: true` (`resetsFromComplimentary`). Such a row is from a peer before §91, or is a Claude row served from the persisted usage snapshot before the first sweep. The rule guesses nothing. A Codex count is the kind its key names, with expiry `not_read`. A Claude kind is one that `remaining_by_kind` names (§64), and `other` reads as `unknown`. A date is given only where the block ties it to that kind: `week_held_until` to the week reset, and `expires_at` to the only kind left. Every other entry reads `none_given`, so a week reset with Fable resets behind it lists the week reset dated and the Fable resets undated. A Claude block from before §64 names no kinds, so its count is one `unknown` entry with the soonest date. A block that holds none is `[]` and not partial, and a row with no reading has no `resets`. `fable_resets`, `reset_hold` and `reset_hold_s` are as §64 left them.

The board. A Claude row's pace cell shows, after its `2x`, one mark per kind the account holds: a letter, the count, and the soonest expiry in the browser's time. `W1 12/10` is a week reset ending December 10, and `F2 3/9` is two Fable resets, the soonest ending March 9. A Codex row shows `R3 12/14` beside its fuse: three credits, the soonest ending December 14. `R3 ?` is a count whose dates are not read yet, and `12/14*` is a date from the last good read. A kind the daemon does not know is `U1`, and the marks of a row whose daemon publishes no list end in `~`. Each mark's title lists every entry with its date and where the date came from. The legend explains each mark. §64's `F2` badge is drawn only for a row that carries no list.

srq. `srq pool` shows the same marks in one column, in this machine's time, in a section where a row holds a reset. The column is padded so the columns stay aligned, and it takes the place of §64's `Fn`, whose count it holds. Its legend names the letters and says that a Fable claim brings no 2x and no early burndown. Where a cell shows `?`, `*` or `~`, one more legend line says what they mean. `srq pool --json` carries `resets` and `resets_partial` on each row. For a row with no list, srq takes the same entries from `complimentary_reset` as the board does (`_resets_from_complimentary`).

`srq resets` prints every saved reset in the pool, one line per account, kind and expiry, soonest expiry first and undated last. A line gives when the reset expires and how long until then, the kind, the count, the account's label (§87), where the account lives, how much of its week and of its Fable meter is used, and when its week rolls. A kind the daemon does not know reads `unknown <raw type>` with `never claimed automatically`. A count whose dates were not read reads `expiry not read`, never "no expiry". A date from the last good read says how old it is, and a line from a daemon that publishes no list says `its daemon gives no full list`. An account that two daemons hold is one budget, so it is listed once with both places. The last line totals each kind and date, such as `totals: 14 week resets expire 12/10; 3 Codex credits expire 12/14; 1 Codex credit, expiry not read`. `srq resets` reads this daemon's usage-status and its `peers/usage-status`, so it needs no peer credential and shows every peer this daemon reads, not only the board's first. A peer served from its last good body says so, with the body's time and the error. An unreadable peer prints `!!! remote pool UNREADABLE` and says that its resets are missing. A daemon from before §32 lists its own accounts, with a line that says its peers are not shown. `srq resets --json` carries every entry, the notes and the totals.

sr status. The Codex `1x reset` cell shows the soonest expiry that a daemon read beside the count, `3 left 12/14`, on a row read from a daemon: a server, a peer through this daemon, or this machine's daemon when the login session cannot read the accounts itself (§47). A date from the last good read counts. A Codex row that `sr` reads itself, as it does in a working login session, takes its dates from this machine's daemon: the one loopback read of its usage-status that also fills in a Claude row's missing block (`fillResetsFromLocalDaemon`, within 3 seconds), only when a row needs it. The read asks for the daemon's last sweep as it stands (`?snapshot=1`), so it starts no sweep and no provider request; the Claude fill-in of §46 now reads the same way. The list is taken only when the daemon counted as many credits as `sr`'s own read, so a credit spent since the daemon's sweep is never dated. A count not read, a daemon from before §91, and a daemon that counted another number or did not answer show `3 left` as before, because `sr` reads no credit list itself. The Claude cells are as §64 left them.

Deploy order. Either order is safe. A board, `sr` or `srq` from before §91.2 ignores the list. A daemon from before §91 publishes none, so its rows show what `complimentary_reset` says, marked `~` or `its daemon gives no full list`.

Tested in Docker, every provider a fake. In `internal/proxy/pool_resets_test.go`:
- `TestThePoolBoardCarriesEveryResetOfThisDaemonAndItsPeer` runs two daemons over §91.1's `creditDaemon`, whose backends fail the test on any request to redeem. The laptop's pool.json carries its own account's two read dates, and the box's three dates as the box publishes them, on a row of the box alone. The laptop's `peers/usage-status` carries the box's list as the box publishes it. A peer from before §91 then serves §64's blocks. A week reset with two Fable resets behind it lists the week reset with its date and the Fable resets as `none_given`. Fable resets alone list their date, a block from before §64 lists one `unknown` entry, and a Codex count lists one `not_read` entry, each marked partial. A block that holds none lists `[]`, not partial, and a row with no reading lists nothing.
- `TestAPeersResetsListChangesNoRanking`: the cross-pool candidates read from a box's body, with read dates on two accounts, are the same with the list and without it.
- `TestTheBoardPageMarksEachResetKindWithItsSoonestExpiry` runs the page the daemon serves, in node, on the pool.json it serves (§90's page runner, now `renderPoolPage`). A Codex row reads `R3 ?` before the list is read, `R3 <date>` after, and `R3 <date>*` after a failed read. On a Claude box a row reads `W1 <date>` right after its `2x`, then `F2 <date>`, and the rows of a peer from before §91 read `W1 <date> F2 ~` and `R3 ? ~`.
In `internal/proxy/pool_ui_page_test.mjs`, in node: each mark, the soonest date of several, `?`, `*`, `~`, no mark for a row with none, every entry in the title, and the legend. In `cmd/subrouter/sr_reset_expiry_test.go`, `TestTheCodexResetCellShowsTheSoonestReadExpiry` decodes a daemon's rows into `sr`'s wire type and reads the cell: the soonest read date, a last good date, `3 left` for a count not read and for a daemon before §91, and the Claude cell as §64 left it. `TestALocalCodexRowTakesItsCreditDatesFromThisMachinesDaemon` reads three stored Codex accounts through `sr`'s own usage read, with a stand-in for this machine's daemon: the account the daemon counted alike reads `3 left <date>`, the one it counted otherwise `2 left`, a spent one `used`, and the daemon is read once. Through the real loopback read, against a stand-in daemon, the cells are the same and the one request is `/_subrouter/usage-status?snapshot=1`. `TestLocalRowsTakeTheDaemonsResetWhenTheirReadHasNone` (§46) now goes through the same read. `ops/skills/skill_test.go`'s `TestSubrouterSkillNamesEverySavedResetSetting` requires the skill's saved-resets section to name each setting the operating guide's names, and naming the switch makes `TestSubrouterSkillNamesOnlyWhatExists` require the literal `serve` reads. `srq selftest` covers the pool column: each kind, `U`, the columns aligned, `?`, `*` and `~`, both legend lines, `--json`, a malformed entry, and a daemon before §91 and one before §64. It runs `srq resets` against a daemon with three peers (one served from its last good body, one unreadable), against one with none, and against one from before §32. It checks the order, labels rather than IDs, where each account lives, the meters and the roll, an account on two daemons listed once, `expiry not read`, the last good age, an unknown kind, a daemon with no list, the totals, `--json`, a pool with no reset, and how long until a date. §64's srq checks now read the column.

Each part was taken out of a copy of the working tree in turn, one change each, and its test went red for the predicted reason. In pool.json: no row carrying the list; this daemon's rows without `withHeldResets`, so its own Codex row read as partial; a row with no list carrying nothing; the derived list never marked partial; a Fable reset behind a week reset given the block's soonest date; a block from before §64 taken for a week reset; and the peer's ranking reading the list, which moved the candidates. On the page: no mark on a Codex row; no mark on a Claude row; no `*`; no `?`; no `~`; the first date of a kind in place of the soonest (the node test); and the legend's new entries taken out. In `sr`: the cell without its date; a last good date not shown; and the row dropping the daemon's list. In `srq`: the column taken out; the daemon's list ignored; a row with no list showing nothing; the legend taken out; no `?`, no `*` and no `~`; an account on two daemons listed twice; the lines left unsorted; no totals line; the account named by its ID; an unknown kind not flagged; the peers not read; an unreadable peer left silent; a last good body left unmarked; and a Fable reset behind a week reset given the block's date. With the peers not read, the selftest reports each failing check and runs to its end, where it stopped at the first missing line before. In `sr status`'s fill: the fill on the Claude rows only; Codex rows never asking; and the list taken whatever the daemon counted. In the skill: the switch not named, and the switch's literal renamed in `serve`. The same runs with nothing taken out pass.

Residuals. The board has a panel for the first peer only (§31's D0), so its rows show every reset only in a pool with one peer; `srq resets` reads every peer. An account that two daemons hold shows the first daemon's list, this daemon's own when it holds the account. A daemon from before §91 gives no date for Fable resets behind a week reset. A kind that a peer's table knows and this build's does not is marked `U` on the board and in `srq pool`, and `srq resets` names it by the peer's kind.

§91 as a whole. §91 reads, records and shows every saved reset an account holds: its kind from one table, its count, and its expiry, with the Codex dates read from Codex by the daemon that holds the account. usage-status, pool.json, the board, `srq pool`, `srq resets` and `sr status` show them, for this daemon's accounts and its peers'. Every later step the owner asked for needs this: claiming at the wall, deciding when to claim, and the burndown before an expiry. Before it, a Codex credit had no date and nothing showed the pool's resets in one place. It claims nothing, sends no request that spends a reset or a credit, moves no session, and ranks nothing. The tests prove that the reader only reads, and that selection and the peer's ranking are the same with the list and without it. `SUBROUTER_READ_CODEX_RESET_EXPIRY=0` turns the credit list reads off, and every Codex count then reads `not_read`. The rest needs no switch, because it only shows what the readings already carry. Assumed and not measured: whether a claim starts a new seven days (`restarts_clock`, `assumed_yes` for Codex and `assumed_no` for Claude), that a Codex credit gives back only the weekly window (`gives_back` `weekly`), and, where a grant does not say, that a Claude claim needs the limit reached (`needs_limit`). §92's claim records are to settle the clock.

- Knobs: `SUBROUTER_READ_CODEX_RESET_EXPIRY` (on by default; `0` off; any value other than unset, `1`, `true`, `yes`, `on`, `0`, `false`, `no` or `off` stops the daemon from starting). The cadence is `codexResetExpiryEvery` (1 hour), the read's limit `codexResetExpiryTimeout` (20 seconds), the pass interval `codexResetExpiryScanInterval` (10 seconds), the warning's `unknownResetWarnEvery` (1 hour).
- Code: `internal/accounts/reset_kinds.go` (`ResetKindSpec`, `defaultResetKinds`, `ClassifyReset`, `UseResetKinds`, `HeldResets`, `HeldReset`, `CreditHeldAt`), `internal/accounts/codex_usage.go` (`ComplimentaryResetInfo.Held`), `internal/agents/claude/reset.go` (`ResetGrant.Kind`, `ResetGrant.held`, `ResetInfo`), `internal/proxy/held_resets.go` (`RunCodexResetExpiryReader`, `readCodexResetExpiries`, `codexResetExpiryDue`, `withHeldResets`, `codexHeld`, `warnUnknown`, `SetReadCodexResetExpiry`), `internal/proxy/proxy.go` (`AccountUsageStatus.Resets`, `AccountRef.heldResetMemory`, the usage-status chain), `cmd/subrouter/main.go` (`readCodexResetExpiryEnabled`, `codexResetExpiryScanInterval`, the loop), `cmd/subrouter/sr_server.go` (`remoteServerUsageStatus.Resets`); §91.2: `internal/proxy/pool_resets.go` (`poolReset`, `poolResetsOf`, `resetsFromComplimentary`), `internal/proxy/pool_ui.go` (`poolRow.Resets`, `poolRow.ResetsPartial`, this daemon's rows through `withHeldResets`), `internal/proxy/pool_ui.html` (`resetMarks`, the legend), `ops/bin/srq` (`_held_resets`, `_resets_from_complimentary`, `_reset_marks`, `pool_resets`, `reset_totals`, `cmd_resets`), `cmd/subrouter/sr.go` (`srUsageRow.resets`, `soonestReadCreditExpiry`), `cmd/subrouter/sr_provider_reset.go` (`fillResetsFromLocalDaemon`), `cmd/subrouter/sr_server.go` (`usageRowsFromServerUsageStatuses`), `internal/accounts/reset_kinds.go` (`HeldReset.Expires`)
- Tests: `internal/proxy/held_resets_test.go`, `internal/accounts/reset_kinds_test.go`, `internal/agents/claude/reset_held_test.go`, `cmd/subrouter/read_codex_reset_expiry_test.go`; §91.2: `internal/proxy/pool_resets_test.go`, `internal/proxy/pool_ui_page_test.mjs`, `cmd/subrouter/sr_reset_expiry_test.go`, `ops/skills/skill_test.go`, `ops/bin/srq selftest`
- Docs: `ops/OPERATING.md`, `ops/skills/subrouter/SKILL.md`, `ops/README.md`
- Related: builds on §46 (the block and the Codex count) and §64 (grant kinds, `remaining_by_kind`, `week_held_until`), and reads the list §29.5's redeem path reads. Spends nothing (§46, §56). One more top-level usage-status field, which a peer ignores. §91.2 reads the peers through §32's view, names accounts by label (§87), and takes the place of §64's `Fn` in `srq pool`.

## 92. Each daemon keeps a durable history of its own meter readings and of every reset claim it makes

The owner, 2026-11-22: subrouter is to handle every account reset itself and get the most out of each one, and to claim a reset only when the pool spends faster than its budget lasts, or when the trend takes it well over that. Nothing could tell. Each daemon held 2 hours of readings in memory for its own Claude accounts (§72), lost them at every restart and deploy, kept nothing for Codex, and computed no pool burn rate. Two facts every reset plan rests on were never measured either: whether a Claude claim keeps the week's roll time, and whether a Codex redeem starts a new 7 days. This section adds what every version of the plan needs and what spends nothing: it reads, records and shows. Nothing claims, ranks or routes on it.

§92.1 The series, its segments and the claim records.

What is recorded. Each daemon keeps a series of readings for its own accounts in `usage-history.jsonl`, beside `token-usage.jsonl` and `sessions.json` in the folder of serve's `--sessions` file (`~/.subrouter` by default; `SUBROUTER_STATE_DIR` does not move it). A point is one meter as one reading gave it: the time of the reading, the provider, the account, the meter, the fraction used, and when the meter's window rolls. The meters are Claude's week (`7d`), Claude's Fable meter (`oauth-apps-weekly`, §64), Codex's week (the account-wide window of 7 days) and a Codex model pool's own week when the reading has one (`week:<feature>`). The time is the reading's own: when its read was sent, as everywhere in the daemon (§29), never when a cached copy was served. The roll is the provider's own reset time when it gives one, else the answer's time plus its seconds to reset. A week whose clock has not started (§86) is marked unstarted, since its roll slides with every reading. A point is written when a reading differs from the meter's last point (by more than 0.01 point used, or a minute of roll), and at least every 10 minutes while readings arrive. A reading older than the meter's newest adds nothing.

Only this daemon's own accounts. A point is taken where the daemon itself reads an account: every live usage read of a Claude account (`fetchAccountUsageLive`), every live read of a Codex account (`fetchCodexUsageDetails`), the claim path's status read, and the redeem's own reads before and after. A peer's rows reach the daemon over the peer link and never pass those, so they never enter the series. Each daemon records its own, and §92.2 carries each one's figures to the other.

Kept across restarts, 35 days, bounded. New lines are appended every 2 minutes, and once more when the daemon stops, after its drain, each flush as one write, so a retiring worker and its replacement can append to one file. The flush loop writes nothing when it stops: `serve` makes the one last write itself, so it lands before the process exits instead of racing the exit. At start the daemon loads the file, and the file is rewritten at start, once an hour, and whenever it passes its bound: oldest first, each point and claim once, nothing older than 35 days, and above 32 MiB the oldest dropped until three quarters of it is left. So a restart or a deploy loses at most the unflushed 2 minutes, and a point two workers both wrote is stored once. A line that does not decode is skipped, and so is one longer than 64 KiB, far past any line the history writes: ending the read there would load nothing and stop every later compaction. A state directory that cannot be written logs `usage history could not be written; the daemon keeps it in memory and serves as before` once, at Warn, until a write succeeds again; the readings stay in memory, at most 50000 lines wait for the file, and nothing else changes.

Segments. A meter's points form segments, and burn is never read across a segment's start. A segment starts where a started week's roll moves by more than 5 minutes, or where the used fraction falls by more than 0.1 point. An unstarted week's roll slides with every reading, so it moves nothing. A week that reads unstarted again after it ran starts a segment by its fall in used; one that spent nothing loses no burn without one. Its cause is `claim` when a claim this daemon recorded as landed lies between the two points, `roll` when the old roll time has passed, and `drop` otherwise. A failed claim explains nothing. The first segment the history holds has no cause.

Claim records. Every claim this daemon makes writes one record to the same file: when it was sent, the account, the kind claimed (a Claude grant's `week`, `fable` or `other`, §64; a Codex credit's `reset_type`), the provider's result, and each meter's used fraction and roll as the last reading before the claim gave them and as the first reading after it. That covers the Claude claim handler (§46) and every Codex redeem a daemon makes, since all of them pass `redeemAccountChecked`: `sr codex reset`, `sr reset` with an account, `--all` and the smart pick when sr sends them to a daemon, the failover auto-reset (§29.5) and the background spender. A redeem `sr reset` makes from the CLI itself is not recorded: with `--local`, with `SUBROUTER_CODEX_SERVER=local`, on a machine with no server configured, and the local `--gto`, the CLI calls the provider straight and no daemon sees the claim. Its fall then starts a `drop` segment in the daemon's series. To settle the two questions below, claim through a daemon. The values are real readings, each with its own time. Before a Claude claim the daemon reads the account's status live unless a fresh cached block served, and before a Codex redeem it always reads live. After a Claude claim the handler reads the account again (`afterClaudeReset`), and after a Codex redeem it reads it again for `windows_after`. A claim waits for that reading and is written with it; one with no reading after it within 30 minutes, or when the daemon stops, is written without one. Since §99, a throttled usage answer that carries Anthropic's own figures for a meter is such a reading. A claim that did not reset the account is written at once, marked `failed`, with the before values only: a Codex consume that failed, or a Claude answer other than `reset`, unless it may have landed and names what it cleared. A Codex redeem that the approval declined, or that found no credit, sent nothing and records nothing. Each record logs `reset claim recorded` at Info with `account`, `provider`, `kind`, `result`, `failed`, `claimed_at`, and the groups `before` and `after`, one per meter with `used`, `read_at`, `roll` and, for a week not started, `unstarted`. These records answer the two open questions at the first real claims: whether a Claude claim keeps `roll` from before to after, and whether a Codex redeem leaves the week unstarted or 7 days from the claim.

A defect in §72's tracker, found and fixed. The tracker starts an account's series again when its weekly roll moves by more than a minute. A score was built from the window cache, which lives 2 minutes, but read a cached window's seconds to reset against the clock of the score, not of the reading, so the roll moved later by the cache's age. With the sweep re-scoring every 30 seconds, a cached reading more than a minute old was scored as a roll that much later, the tracker threw away the account's points, and the next fresh reading, rolling at the true time again, restarted the series once more. Measured in the test below before the fix: a reading 90 seconds old rolled the week 1m29s late, and the account's measured burn of 0.12 a week per hour read 0. `scoreFromUsageWindows` now re-anchors every window that has a reset time to the same clock it scores with (`accounts.ResetsAsOf`, as usage-status already did for its readers), so a cached reading rolls at its own reset time. It serves every score built from windows: the sweep's, usage-status's, the board's, a restored seed and passive reply headers.

Settings. `SUBROUTER_USAGE_HISTORY=0` turns it off: no file is written, no point or claim is recorded, and every other behaviour is what it was. Unset, `1`, `true`, `yes` or `on` is on, the default. `0`, `false`, `no` or `off` is off. Any other value stops the daemon from starting, and startup logs one line when it is off. A change needs a restart. The switch does not cover the §72 fix above: with it off, a score built from a cached window still rolls at the window's own reset time.

Tested in Docker, one test per behaviour, in `internal/proxy/usage_history_test.go` unless named: `TestTheSeriesHoldsOnePointPerChangedReadingAtTheReadingsOwnTime` (the daemon's own Codex read against a fake whose readings change and answer 300 ms late: one point per changed reading at the read's start, no point for an unchanged new reading, the roll the reading's; a cached reading never reaches the series, which only live reads feed), `TestAnUnchangedMeterIsWrittenAtLeastEveryTenMinutes`, `TestACodexModelPoolsOwnWeekIsAMeterOfItsOwn` (beside the account-wide week; the pool's 5 hours not recorded), `TestTheSeriesSurvivesARestartAndAPointWrittenTwiceIsStoredOnce` (two workers on one file), `TestStoppingTheDaemonWritesWhatWaits` (the last write at stop, a waiting claim in it), `TestTheSeriesKeepsThirtyFiveDaysUnderItsSizeBound` (600 readings against a 4 KiB bound), `TestOnlyThisDaemonsOwnAccountsEnterTheSeries` (a peer whose rows carry windows, read for a new session and the board), `TestSegmentsStartAtARollAClaimAndADrop` (Claude and Codex, a failed claim among them), `TestAClaudeClaimRecordsEachMeterBeforeAndAfter` (through the handler; the fake answers §64's measured claim, the week from 100 % to 42 % and the Fable meter from 116 % to 0, at the same roll; the Info line and the file carry the same values), `TestACodexRedeemRecordsTheWeekBeforeAndTheRestartedWeekAfter` (the fake restarts the 7 days), `TestAFailedClaimRecordsTheBeforeValuesOnly`, `TestAnUnwritableStateDirectoryWarnsOnceAndACorruptLineIsSkipped`, `TestAnOverlongLineIsSkippedAndTheFileStillCompacts` (a damaged line of 2 MiB between two good ones), `TestWithTheHistoryOffARedeemRecordsNothingAndAnswersAsBefore`, `TestSelectionIsTheSameWithTheHistoryOnAndOff` (Pick, PickSteady and every clause of both explanations), the defect's `TestACachedReadingKeepsTheRollTimeAndTheMeasuredBurn` in `internal/proxy/cached_reading_roll_test.go`, and in `cmd/subrouter/usage_history_test.go` `TestUsageHistoryFromEnvironment`, `TestServeUsageHistoryIsOffWithTheSwitch`, `TestServeReadsTheUsageHistorySwitch` and `TestServeAttachesTheUsageHistoryOnlyWithTheSwitchOn` (`serve` in a child on loopback: the burn route answers 404 naming the switch with it off, the three meters with it on), and in `cmd/subrouter/history_serve_test.go` `TestServeKeepsItsUsageHistoryBesideItsSessionsFile` (`serve` in a child with one stored Codex account, its usage reads sent to a fake chatgpt.com that answers nothing else: a history seeded beside `--sessions` gives the account its 24-hour burn of 1.0 before `serve` reads anything, a reading `serve` takes is in the file while it still runs once the flush loop has run, a reading taken just before SIGTERM is in the file after the stop, and the fake is asked for usage reads only. The test goes on in parallel after its first reading, and its file sorts before the `sr_` and `serve_` files, so its wait of up to 2 minutes for the flush loop passes while their sequential tests run: in Docker the whole package took 202 s with it there, and 330 s with the same test in a file that sorts after them).

Each part was taken out of the working tree in turn and its test went red for the predicted reason: the re-anchoring (the burn read 0 and the roll 1m29s late), peer rows fed into the series, the Claude handler's record, the Codex redeem's record, a failed claim held for a reading after it, the Warn on every failed write, the claim cause, the moved roll, the 35 days, the size bound, the once-only rule, the load at start, a line that does not decode ending the read, a line over the reader's limit ending it (nothing loaded past a line of 2 MiB), the heartbeat, the point stamped when the answer came, a history that changes what selection reads, the last write at stop, the waiting claim in it, the Codex model pool's own week (the series held only `week`), `serve` keeping the history anywhere but beside `--sessions` (the seeded account had no figure), `serve` not starting the flush loop (no point in the file while it ran), `serve` not making the last write at stop (the reading before SIGTERM missing after it), the switch ignored, `serve` attaching the history with the switch off (the burn route answered 200) or not attaching it with the switch on (404), the off line not logged, and an unreadable value read as on.

Residuals. A point's time is when its read was sent, up to that read's latency before the provider answered. Readings the daemon does not take itself are not recorded: passive reply headers, the Fable Messages probe and the hourly fallback reset read. A Claude answer that may have landed but names nothing cleared is recorded as failed; a fall in the next readings then starts a `drop` segment. A claim waiting for its reading is lost if the process dies without a clean stop. A claim's after values are the claim path's own reading right after it: a provider that applies a claim later shows the state before it there, and the fall then starts a `drop` segment, so read the points after a claim as well as its record. Two processes' appends to one file do not interleave, but a compaction does not hold the other's appends off: it reads the file, writes a new one and renames it over the old, so lines the other process appends in between are lost from the file, as with `token-usage.jsonl`. That is at most one flush of the other's points, at its start or once an hour, and the other process keeps them in memory until it restarts. `serve`'s test reaches the provider's usage address through a transport the test binary's child installs in place of the default one, so the dial to it is the test's, not the daemon's. Two workers that run at once each record their own readings, so a swap can add points, never duplicates. The size bound is an estimate, not measured on a pool: a point is about 100 bytes and each meter writes at least 144 a day, so 20 accounts with two meters each fill about 1 MB a day and keep the whole 35 days, and a pool several times larger keeps less.

§92.2 Burn, the pool figure, the route, `srq` and the documents.

What burn is. For one account and one meter, over a window of length w that ends now, burn is the rise in the meter's used fraction inside the window divided by w's share of the meter's 7 days. 1.0 spends the budget exactly as fast as the week lasts, 2.0 twice as fast, and 0 not at all: an account whose series rises by 1/7 of its week in 24 hours burns 1.0 over those 24 hours. It is not `sr plot`'s burn pace (`internal/burnpace`), which divides what each week has spent so far by the share of it that has passed: burn reads only the window, so a change in demand shows the same day. The rise is read inside segments only (§92.1): the rise between consecutive points of one segment, the part of each stretch that lies inside the window. So the fall at a roll or a claim adds nothing, and neither does a jump where a new week reads more used than the old one. The windows are the last hour, 6 hours and 24 hours, and the 6 and the 24 hours before the last 6 and 24, which the trend reads. A window has a figure only when the series covers at least half of it. Covered is the time between two consecutive points at most 20 minutes apart (twice the heartbeat, so a longer stretch is a time with no readings), and from the last point to the newest reading. A window covered less than half reads "not enough history", never 0. The share at the limit is the part of the covered time in which the meter read 99.5 % used or more, each stretch counted by the point that starts it.

What usage-status publishes. Each row of this daemon's own accounts carries `burn`, outside `normalized`: one entry per meter the account's series holds (`week`, `fable`, `week:<feature>`), and in each the five windows `1h`, `6h`, `24h`, `6h_prev` and `24h_prev`, each as `{"burn": …, "covered": …, "at_limit": …}` to three decimals. A window covered less than half has `covered` and no `burn` or `at_limit`, so a reader tells a series too short from a daemon that publishes none. A row whose account has no series (no reading yet, an API key, a provider the history does not record) has no `burn`, and with the history off no row has one. Only this daemon's own accounts: a peer's row is the peer's to publish, and the peer link carries it here.

The pool figure. `GET /_subrouter/burn.json` answers on the private listener, admin-gated as pool.json is, so loopback reads it with no token. Per meter, Claude's week, the Fable meter and Codex's week always and a Codex model pool's own week once one is read, it gives for the last hour, 6 hours and 24 hours: the pool burn (`burn`, the mean over the accounts with a figure, each account weighing the same), how many accounts it counts (`accounts`), how many hold the meter with the window covered less than half (`not_enough_history`), and `at_limit`, the share of the counted accounts' covered time at 99.5 % used or more. That share is demand a spent account could not serve, which burn hides: an account at its limit the whole window burns 0 and spends all of it at the limit, so a pool whose accounts are all spent reads burn 0 with `at_limit` 1, not "under pace". `trend` gives for 6 and 24 hours the mean, over the accounts with a figure in both, of the window's burn less the burn of the window before it. Then each account's own figures, this daemon's first, with its label (§87). The accounts are this daemon's loaded accounts and every configured peer's, read from the peer's usage-status as a routing decision reads it (`usageForDecision`: the same cache and the same single fetch, so the route reads a peer at most once every 10 seconds, as `sr status` does). An account two pools hold is counted once, from this daemon. `peers` names each peer and whether it is counted. A peer is not counted, and its accounts are neither counted nor read as 0, when none of its rows carries `burn` (a build from before §92, or its history off), when its body does not decode, when no body has been read from it yet, or when its last body is more than 15 minutes old: a peer that stopped answering, at the age at which its carried reading ends too (`remotePoolCarryTTL`). With the history off here the route answers 404 `this daemon keeps no usage history (SUBROUTER_USAGE_HISTORY=0)`.

Nothing ranks or routes on it. usage-status adds the figures after the scheduler has taken its scores from the same rows, they sit outside `normalized`, and a peer's reading of the row (`remotePoolAccountUsage`) has no field for them.

The two differences from the brief. The trend is taken over the accounts with a figure in both windows, not as the difference of two pool figures over different accounts, so an account whose series is too short for the window before adds no trend of its own. The route is admin-gated on the private listener, as pool.json, rather than loopback only: it relays the peers' figures, as pool.json relays their rows.

srq. `srq burn` prints each meter's pool burn over the last hour, 6 hours and 24 hours, how many accounts each counts and how many more have not enough history yet, the trend in words (`up 0.20 on the 6 hours before`, `no trend yet: it needs 36 hours of history`), the share of account-time at the limit per window, and a line per account by its label, a peer's with `on <peer>`, and `-` where it has not enough history. A meter with no figure says `not enough history yet` window by window, a meter no account reads says so, a peer that is not counted is named with why, and a legend says how much history each figure needs. `srq burn --json` prints the route's body, and both exit 1 from a daemon that publishes none. `srq pool` ends with one line, the three meters over 24 hours: `pool burn over 24 hours (1.00 = on pace): Claude week 0.80 (40% at the limit), Claude Fable 1.10, Codex week not enough history yet; srq burn says more`. A daemon that answers 404 reads `pool burn: this daemon publishes none (a build from before §92, or SUBROUTER_USAGE_HISTORY=0)`, and any other failure is said on that line while the rest of `srq pool` prints as before. `srq pool --json` carries the route's body as `burn`, null without one, and `--remote` prints no burn line.

How long before the figures mean anything. A window needs readings over half its length: 30 minutes for the last hour, 3 hours for 6 hours and 12 hours for 24 hours. A trend also needs half the window before it: 9 hours of history for the 6-hour trend and 36 hours for the 24-hour one. The series loads at start (§92.1), so a restart or a deploy keeps them; a new state directory starts from nothing.

Deploy order. Either order is safe. A daemon from before §92 ignores `burn` in a peer's rows, and a peer from before §92 publishes none and is named as not counted. The convention stays the box first, then the laptop.

Settings. `SUBROUTER_USAGE_HISTORY=0` (§92.1) turns the figures off with the series: no row carries `burn`, the route answers 404, and every other behaviour is what it was.

Tested in Docker, one test per behaviour in `internal/proxy/usage_burn_test.go`, each over a daemon of this build with the history's clock fixed and its series seeded a reading every 10 minutes. `TestBurnIsTheRiseOverTheWindowsShareOfTheWeek`: 1/7 of the week in 24 hours burns 1.0 over the last hour, 6 hours and 24 hours, twice that 2.0, flat 0; a roll whose used falls, a roll whose new week reads more used, and a claim each leave 1.0 (less the one 10-minute stretch of 144 that spans the start); a series 11 hours long has no 24-hour figure, 0.458 covered, and burns 1.0 over 6 hours. `TestUsageStatusPublishesEachAccountsBurn`: usage-status carries all five windows at 1.0 for a series 49 hours long, coverage and no burn for one 20 minutes long, and with the history off no row has `burn` and the route answers 404 naming the switch. `TestTheBurnRouteCountsLocalAndPeerAccounts`: a laptop, a box of this build, a peer whose Claude row carries figures from a real history, and a box from before §92. The Codex week counts the laptop's 1.0, flat and 3.0 and the box's 2.0, the shared account once from the laptop, and the old box's account not at all; its 24-hour trend is +0.5 over the 2 accounts with both days, Claude's week 0.5 and the Fable meter 1.5 come from the peer, and the old box is named as publishing no burn. Then the Claude peer stops answering, and 20 minutes after its last body it is named with that age and its accounts drop out. `TestASpentPoolReadsAtTheLimitNotUnderPace`: an account spent the whole window burns 0 and is at the limit all of it, alone and in the pool; one that reached its limit 12 hours ago burns 0.5 with 12h50m of the 24 hours at 99.5 % or more, and the pool reads 0.25 with 0.767 at the limit. `TestSelectionIsTheSameWithBurnPublished`: a laptop and a box, each with figures for its own accounts, give the same board ranks, the same cross-pool verdict (the box's account in its roll window first) and the same three placements as the same pair with the history off. `ops/bin/srq selftest` covers `srq burn`'s words, counts, trend, share at the limit, labels and legend, an uncounted peer, a spent pool, figures that are not numbers, the `srq pool` line and its `--json` and `--remote`, a daemon that answers 404 and one that fails otherwise. The CLI's usage-status type decodes the new field (`remoteServerUsageStatus.Burn`): §47's `TestUsageStatusWireTypeHasEveryDaemonField` failed on it with `json: unknown field "burn"` until it did. The CLI does not decode usage-status strictly, so a CLI from before §92 reads a §92 daemon as before.

Each part was taken out of the working tree in turn and its test went red for the predicted reason: the rise read across a segment's start (the rolled account read 0 over 24 hours, the jumped one 4.1), a figure for any coverage, the rise not divided by the window's share of the week (the steady account read 0.143 over 24 hours), usage-status without the figures (a peer of this build then counts as none), the route answering with the history off, a peer's rows without `burn` read as 0, an account two pools hold counted twice, the peers not read, the trend as the difference of the two pool figures, a peer's body of any age counted, only a meter past 100 % at the limit, the pool's share at the limit not published, and a figure that leaks into the verdict a peer ranks on (the laptop then kept all three sessions). In `srq`: the pool line, the trend words, the label, the 404 read as none, the uncounted peer, a string read as a figure, and the share at the limit on the pool line. The same runs with nothing taken out passed.

What the figures cannot say. Each account weighs the same, since plan sizes are not known: a small plan spending fast moves the pool figure as much as a large one. A pool figure is only as good as each daemon's history. A daemon that was down has a gap: a window covered less than half has no figure, and inside a window with enough coverage the rise across a gap is spread evenly over it. Up to 10 minutes of rise across each segment start counts nothing. A meter that reads in whole percents moves the 1-hour figure in steps of about 1.7, one percent of a week in an hour, and the 24-hour one in steps of 0.07. A peer's figures are computed on the peer's clock when it answered, normally under 10 seconds before, at most 15 minutes. The share at the limit counts a stretch that ends in a roll as at the limit to its end. The trend over the accounts with both windows can differ from the difference of the two pool figures when accounts join or leave. Nothing here was measured on the live pool yet: the figures need a deployed build and the history above to mean anything.

- Knobs: `SUBROUTER_USAGE_HISTORY` (on by default; `0` off; any value other than unset, `1`, `true`, `yes`, `on`, `0`, `false`, `no` or `off` stops the daemon from starting). The §92.1 constants are `usageHistoryRetention` (35 days), `usageHistoryHeartbeat` (10 minutes), `usageHistoryFlushInterval` (2 minutes), `usageHistoryMaxBytes` (32 MiB), `usageHistoryClaimWait` (30 minutes), `usageSegmentRollMove` (5 minutes) and `usageSegmentFall` (0.001); the §92.2 ones are `usageBurnMaxGap` (20 minutes), `usageBurnMinCover` (half a window), `usageBurnAtLimit` (0.995), `usageBurnPeerMaxAge` (15 minutes) and the window table `usageBurnWindows`.
- Code: `internal/proxy/usage_history.go` (`UsageHistory`, `observe`, `usageMeterReadings`, `startClaim`, `finishClaim`, `compact`, `trimUsageHistory`, `segments`, `usageSegments`), `internal/proxy/proxy.go` (`fetchAccountUsageLive`, `fetchCodexUsageDetails`, `redeemAccountChecked`, `scoreFromUsageWindows`, `AccountRef.usageHistory`, `AccountUsageStatus.Burn`, `handleUsageStatus`, the route), `internal/proxy/claude_reset.go` (`readClaudeResetStatus`, `claudeReset`), `cmd/subrouter/main.go` (`usageHistoryEnabled`, `serveUsageHistory`); §92.2: `internal/proxy/usage_burn.go` (`accountBurn`, `meterBurn`, `withUsageBurn`, `handleUsageBurn`, `readPeerBurn`, `usagePoolMeters`, `UsageBurnPath`), `ops/bin/srq` (`cmd_burn`, `render_burn`, `burn_pool_line`, `_get_burn`), `cmd/subrouter/sr_server.go` (`remoteServerUsageStatus.Burn`)
- Tests: `internal/proxy/usage_history_test.go`, `internal/proxy/cached_reading_roll_test.go`, `cmd/subrouter/usage_history_test.go`, `internal/proxy/usage_burn_test.go`, `ops/bin/srq selftest`
- Docs: `ops/OPERATING.md`, `ops/skills/subrouter/SKILL.md`
- Related: corrects §72 (a cached reading no longer restarts the tracker's series). Builds on §46 and §64 (the Claude claim and its kinds), §29.5 and §46.8 (the Codex redeem paths), §86 (an unstarted week), §87 (views name accounts by label) and §31 (the peer link carries the figures, and ranks on nothing in them). Adds one row field to §13's usage-status, outside `normalized`.

## 93. The daemon claims an account's next saved reset when the account runs out, behind the owner's under-1-percent condition

The owner, 2026-11-22: "the idea is that subrouter handles all your account resetting for you". On 2026-11-23 they set the terms: automatic claims, with a forced condition that a full Claude reset or a Codex credit is claimed only while the account has under 1% left on its week and a Fable reset only while it has under 1% left on its Fable meter, a condition they keep on "for my comfort" until they are more confident; shadow mode first; no cap on claims; no excluded accounts. Until now nothing claimed a Claude reset by itself (§46, §56, §64), and the two Codex paths that can redeem a credit by themselves are off by default (§29.5, upstream #392's background spender). At the wall, today, a running session is moved to another account, which re-bills its whole prompt, or gets the pool's answer when nothing else can take it. §93 is built in parts: §93.1 decides, and in shadow mode logs what it would claim; §93.2 makes the claim, in place; §93.3 shows it in the views and the documents, and sums up §93 as a whole.

§93.1 The mode, the two moments, the owner's condition, the decision and shadow mode.

The mode. `SUBROUTER_AUTO_RESET` is `0` (off), `shadow`, or `1` (real). Unset is `shadow`. Any other value stops `serve` from starting, as §86's switch does. It is per daemon, and a daemon decides only for the accounts whose credentials it holds; nothing is decided with a credential broker (team mode). `1` claims (§93.2), and startup says so at Warn. The package default, for a `Server` that `serve` did not set up, is off, so no other test sees the feature.

The two moments, and no other.
- The wall: the provider refuses a real request on the account with its usage-limit refusal for a meter a saved reset gives back. Claude: a 429 that rejects the week (`7d`) is the week's wall, one that rejects the Fable bucket (`7d_oi`) the Fable meter's, a 429 that rejects only the five-hour window is no wall, and a rejection that names no window is a wall whose meter the readings decide. Codex: a `usage_limit_reached` refusal that is not a workspace refusal (a spend cap or depleted credits, which no credit lifts); its own reset more than five hours away makes it the week's wall, else the readings decide. A wall is seen where the daemon reads the answer: in the usage-limit retry transport, which notes each wall once the turn is over, with what the turn got; in the passive capture of the answer the client gets, for a pin, a turn a peer targeted here, or a request that cannot be replayed (not again for an answer the retry transport already noted); and at the five places the Codex websocket relay handles a quota event.
- The reading: every 10 seconds (`autoResetScanInterval`) a pass reads this daemon's last usage rows (`UsageStatusSnapshot`, so no usage fetch is added). A row is a moment when its reading is newer than the last one considered for the account, at most five minutes old, says a meter a saved reset gives back is at least 99.5% used (`autoResetReadingUsed`: Claude's week or Fable meter, Codex's week), and the account's next reset can be claimed now (Claude: the grant is `usable_now` and nothing else stops it, as the claim judges; Codex: the reading counts an available credit).

The owner's condition. Ruling 2026-11-23: `SUBROUTER_AUTO_RESET_NEAR_EMPTY`, on when unset or `1`, off with `0`; any other value stops `serve`. While it is on, a claim happens only if the account has under 1% left (`autoResetNearEmptyLeft`) on the meter the next reset gives back: the week for a Claude week reset or a Codex credit, the Fable meter for a Claude Fable reset. That is known from the wall's own refusal on that meter, or from a reading at most five minutes old; an unread or older meter fails it. Turning it off removes only this condition. The constant and the switch carry the ruling in the code.

Which reset. Always the provider's next one. Claude: the grant Anthropic names next (`next_grant_id`), from the block this daemon last read with the account's current login and after its last claim (`cachedClaudeResetState`, the claim's own cache), judged by `chooseClaudeResetGrant` as the claim judges it. One exception, at the wall only: a next grant whose block says it cannot be used only because the account was not at a limit when the block was read (`use_requires_limit`, not `usable_now`, `at_limit` false, nothing else in the way) counts as usable, because the wall is that limit, and the claim reads the block live again before it sends anything. Its kind is §64's `Kind`: `week` or `fable`; a grant of kind `other` is of a kind this daemon does not know and is never claimed automatically. Codex: a credit, when the reading counts one; a decision that would claim then lists the account's credits once (reused for 10 minutes) and names the credit the claim would spend, the available one that expires soonest (`FirstAvailableRateLimitResetCredit`, the claim's own pick). A `reset_type` other than `codex_rate_limits` (or none) is a kind this daemon does not know. The condition is judged for the kind of that reset, so a spent Fable meter with a week reset next is claimed only if the week is under 1% too.

The decision. `decideAutoReset` is one function with one answer and its reason in words, so a later rule (burn rate and expiry) can replace it without touching the claim. It checks, in order: the credential is alive (`terminalCredFailure`, or Anthropic's revocation); there is demand (the wall was a real request's refusal, or a warm session (§21) is assigned to the account); the next reset exists and can be claimed now; its kind is known; and, last, the owner's condition. Its verdicts are `would_claim`, `refused_by_condition` (everything else held) and `no_claim`. There is no cap on claims and no list of excluded accounts (the owner). Never for: a peer's account (no peer body or peer answer reaches either moment: the wall reads this daemon's own upstream answers, a fallback stage's answer names no account, and the reading reads this daemon's own rows; and a wall that named a peer's account would still decide nothing, because both moments decide only for an account in this daemon's own list, `autoResetOwnAccount`); a dead credential; an account with no reset left; the daemon's own §90 request, which is sent outside the handler and so meets no moment.

Shadow mode. Nothing waits and nothing is claimed. A wall's decision runs in the background (`Lifecycle.StartBackground`) after the turn, so the turn is byte for byte what it is with the mode off. Each would-claim logs one Info line, `auto reset: would claim the next saved reset`, with `account` (the label, §87), `provider`, `mode`, `moment` (`wall` or `reading`), `kind`, `reset` (the grant or the credit), `reset_expires_at`, `left` (what is left on the meter the reset gives back, or why it is not known), `reason`, and for a wall `turn`: what the turn got today (`moved to another account (<label>)`, `served on the same account by another model`, `served by a fallback stage ...`, `answered 429`, `failed before an answer`, and on the websocket relay `the socket was closed so the client reconnects onto another account` or `answered with the provider's refusal`). At most one line per account and kind in 10 minutes (`autoResetLineEvery`), however many requests meet the wall. Every decision, whatever its verdict, is kept as the account's last decision (`LastAutoResetDecision`), which §93's views read. Startup logs a line when the mode is off, a Warn when it is `1`, and a Warn when the condition is off.

How to turn it off: `SUBROUTER_AUTO_RESET=0` and restart. Nothing else changes: no wall is looked for, no pass reads a row, nothing is logged or kept.

Tested in Docker, over a daemon measured through its real usage read in front of fakes that fail the test on a claim, a redeem, a profile read or any other request: `internal/proxy/auto_reset_test.go`. `TestShadowLogsAWouldClaimAtTheClaudeWeekWallAndTheTurnIsUnchanged` sends a turn on a Claude account holding a week reset (read before the limit) whose answer is the weekly refusal: in shadow mode one would-claim line (kind `week`, `left` 0.0%, the turn `moved to another account (spare)`) and the same last decision, with the mode off neither, and the client's answer (status, headers but the date, body) identical in both. Its real-mode leg went to §93.2's tests once real mode claimed. `TestShadowSeesTheWallOfAPinnedTurn` covers the passive capture (`answered 429`), and `TestShadowSeesTheWallOfACodexWebSocketTurn` the relay: with another account the socket is closed for a reconnect, without one the refusal is delivered, and each decision says so. `TestShadowLogsAWouldClaimOnAReadingAtTheLimit`: 99.6% used with a grant usable now is one line over three passes a minute apart, decided at the first; 99.4%, or a grant not usable now, nothing; no warm session, a `no demand` decision and no line. `TestTheUnderOnePercentCondition`: 2% left, unread, and read six minutes ago are each refused by the condition and a would-claim with it off; 0.9% left read four minutes ago, and the wall's own meter, pass; through the daemon, a Fable wall with a week reset next and the week at 98% is refused, and a would-claim with the condition off. `TestTheNextResetsKindDecides`: a Fable reset next on a Fable wall claims kind `fable`; a week reset next with the week at 40% left is refused; with the week at 0.4% left it claims kind `week`; a grant that clears only `five_hour` is never claimed, and the reason says the kind is unknown. `TestShadowNamesTheCodexCreditThatExpiresSoonest`: a Codex wall names the credit expiring in 5 days, not the one in 20, and a credit of an unknown `reset_type` is never claimed. `TestNeverForAnAccountWithADeadCredential`, `TestNeverForAnAccountWithNoResetLeft`, `TestNeverForTheDaemonsOwnSelfStartRequest` and `TestNeverForAPeersAccount` each run the same fixture without the exclusion first, which would claim. `TestOneWouldClaimLinePerAccountAndKindInTenMinutes`: five walls, one line; a wall nine minutes later, a new last decision and no line; a Fable reset next, its own line; eleven minutes after the first, a third. `TestModeOffDecidesNothingOnAReading`. In `cmd/subrouter/auto_reset_test.go`, `TestAutoResetFromEnvironment` and `TestServeSetsTheAutoReset` cover both switches, their startup lines and the refusal to start.

Each part of §93.1 was taken out of the working tree in turn, one perl substitution each, and its test went red for the predicted reason: the wall in the retry transport; the wall in the passive capture; the relay's wall on a reconnect and on a delivered refusal; the 99.5% threshold; each reading once; a reading needing a reset claimable now; demand; the reading's age; the condition; the Fable meter for a Fable reset; the unknown kind; the Codex credit named; the dead credential; no reset left; the wall standing for the limit a grant waits for; the line once in 10 minutes; the line per kind; mode off on the reading and at the wall; a header added to the turn in shadow mode; a wall seen on the §90 request; the two defaults; and `serve` setting the switches. The same runs with nothing taken out passed.

§93.2 The claim in place, and never twice.

The claim. In real mode (`SUBROUTER_AUTO_RESET=1`) a decision that would claim starts the claim, through the same functions the manual routes call, so nothing sends a second copy of the provider requests. Claude: `claudeReset`, the function behind `sr claude reset`. It takes the account's claim lock, reads the block live unless the cached one is claimable, claims only Anthropic's next grant, reuses a `request_id` that may have landed for 10 minutes, and reads the account again after a reset (`afterClaudeReset`). The automatic claim names the grant the decision judged, so if Anthropic now offers another one next, nothing is sent. Codex: `redeemAccountChecked`, the function behind every redeem path. It reads usage live under the daemon-wide redeem lock and consumes the available credit that expires soonest. At a reading moment the account reads 99.5% used and is not at its limit yet, so the claim lifts the cooked rule there (§46.8's `anytime`). Its last check sees the exact credit and the live reading: a `reset_type` this daemon does not know is never redeemed; at a reading moment the live week must still read at least 99.5% used, with the owner's condition on or off, because the lifted cooked rule would otherwise let a week that rolled since the reading be redeemed (§93.4); and while the condition is on the live week must have under 1% left. After a redeem the daemon clears the account's exhaustion marks and forces a re-score, as the failover redeem does. One claim runs per account at a time.

Where a turn waits. Only in the usage-limit retry transport, which answers the client once it is done, so nothing of a turn it holds has reached the client. A turn that meets the wall waits there, before the marks and the failover. A turn for an account whose claim is running waits before it is sent, ahead of the pre-check and of the admission gate that the refusal paused. That covers another session's turn and a new session's turn placed on the account. A turn already on its way when the claim starts meets the wall and waits on the same claim. When the claim returns, its own re-read decides. If every meter the reset gives back, and the meter the moment found spent, reads back (at most half used, `autoResetBackUsed`; a reset gives a meter back whole), or, since §99, no reading could be taken after a claim the provider says it used, the refused turn is sent again on the same account, spending no failover attempt and no retry budget, and the waiting turns are sent there too. At a second wall the latch below forbids another claim, so a turn is sent again at most once.

The bounds, counted from the claim's start. Claude, 3 minutes (`autoResetClaudeWait`): the claim may spend 2 minutes 15 seconds before it sends (`autoResetClaudePatience`: the claim lock, the reads and Anthropic's own 429s on the reset routes, which a manual claim waits out for up to 4 minutes), and 45 seconds remain for the claim (25 seconds a try) and the re-read. That is well under Claude Code's 600 seconds to an answer's headers (§58). Codex, 1 minute (`autoResetCodexWait`), for a redeem that takes seconds; the redeem itself has 2 minutes (`autoResetCodexClaimBudget`).

Otherwise today's answer. The claim fails (Claude: `auth_error`, `rate_limited`, `cooldown`, `already_used`, `not_limited`, `ineligible`; Codex: a consume answered 4xx), answers that it may have landed (Claude: `unavailable`, `error`, an unconfirmed `request_id`; Codex: a consume answered 5xx, or one that came back with no status or without a redeem), sends nothing (a refusal before the claim), the re-read was taken and shows a meter still spent (§99), the wait passes its bound, or the daemon begins to drain. In each case every waiting turn goes on from where it waited, exactly as without §93. The turn at the wall goes to the marks and the failover: it is moved, or gets the fallback stages, the peer, the recovery probe, or the provider's own refusal when nothing can take it. A turn that waited before its send goes to the pre-check, which moves it if the account is marked by then, or meets the wall itself and is moved there. Nothing marks a credential dead. A claim that is still running when its turns stop waiting goes on, and the account is back for later turns.

Turns that do not wait. The claim still runs for each of them, and each gets today's answer.
- A turn a peer daemon forwarded (`X-Pool-Hop` above 0). The forwarder gives up on headers after 30 seconds (`remotePoolResponseHeaderTimeout`), sooner than a claim may take, and then serves the turn locally, so a wait would serve the turn twice. A targeted forward (§31) is a forced selection and has no usage-limit retry transport anyway.
- A Codex websocket turn. The relay meets the quota event in the middle of a socket and cannot send the turn again itself; today it closes the socket 1013 so the client reconnects onto another account, or delivers the refusal. Waiting in place would mean holding the reconnect's handshake, which this part does not build.
- A pin, and a request whose body cannot be replayed. Neither has the retry transport; the refusal reaches the client.
- A turn that has sent output. In the retry transport there is none; on the relay, a quota event after visible output is delivered as today, and the turn is never sent again.

Never twice. Each account keeps a latch of its last automatic claim: when it started, whether it finished, whether a claim request may have reached the provider, and when a reading showed its meters back. No automatic claim follows another on the account within 10 minutes of its start (`autoResetClaimFloor`). After one that may have reached the provider, or did not finish, none follows until a reading measured after it shows the meters back. A claim that sent nothing holds only the 10 minutes. So a "may have landed" answer is settled by a reading, never by a blind second claim, and Claude's `request_id` reuse stays as it is. The readings that settle a latch are the claim's own re-read and every row whose reading is fresh, of any age: each such row the 10-second pass sees, and the row a decision or §96's choice reads. A row served from last-good figures or from a throttled answer is not fresh and settles nothing (corrected in §102.1). A wall the latch stops is recorded as `no_claim`, with a reason that starts `never twice:`.

The next worker of a swap. The latch is written to the state directory, beside the session store (`auto-reset/<hash>.json`), before anything is sent, and read again at every decision that would claim. So the new worker of a swap reads a claim the old one started as sent until a reading shows the meters back. A draining or stopping daemon starts no claim, and writes no record, and a turn waiting on a claim stops waiting at the drain (checked every 50 ms). A claim already started finishes, and shutdown waits for it as for any background work.

One spender. `SUBROUTER_AUTO_RESET=1` together with `SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET=1`, or with `--reset-credit-autospend=spend`, stops `serve` from starting with a message that names both settings. In shadow mode or off, both older paths behave as before.

What is logged and kept. At the start, Warn `auto reset: claiming the next saved reset` with the would-claim line's fields; at the end, Warn `auto reset: claim finished` with `verdict`, `landed`, `back` and `sent`, and since §99 `reading_after` for a claim that landed. The claim functions' own Warn lines (`claude reset claim sending`, `claude reset claim`) come in between, as for a manual claim. The account's last decision is `claimed`, `failed`, or since §93.3 `may_have_landed`, its reason ending with what came of the claim. Startup in real mode says at Warn that the daemon claims by itself.

How to go back. Unset `SUBROUTER_AUTO_RESET` (shadow) or set it to `0`, and restart. A claim already sent cannot be undone.

Tested in Docker, in `internal/proxy/auto_reset_claim_test.go`, over daemons measured through their real usage read in front of fakes that spend as the providers do: a turn meets the usage limit while its meter is spent, and a claim of the next reset gives the meter back. `TestRealClaimsAtTheClaudeWeekWallAndServesTheTurnInPlace`: with the week spent at Anthropic and read at 95%, real mode claims once, and the turn is refused once and then served on the same account; the session stays and the spare sees nothing. Off and shadow make no claim and move the turn. `TestTurnsThatArriveWhileTheClaimRunsWaitOnTheSameAccount`: three more turns (two sessions' and a new session's) arrive while the claim is held at the fake; nothing is sent while it runs, then all four are served on the account after one claim. `TestATurnInFlightWhenTheClaimStartsWaitsOnIt`: a turn held at the fake until the claim starts meets the wall and joins the claim. `TestATurnSentAgainThatMeetsTheWallAgainGetsTodaysAnswer`: a refusal after the resend is moved to the spare, with no second claim. `TestRealRedeemsACodexCreditAtTheWallAndServesTheTurnInPlace` does the same for Codex over HTTP. `TestTurnsThatCannotWaitGetTodaysAnswerAndTheClaimStillRuns`: a Codex socket turn is closed 1013 and a forwarded turn is moved, each claim runs once, and the redeemed account serves the next turn. `TestARedeemClearsTheMarksAnotherTurnLeftWhileItRan`: while a turn waits on a redeem, a socket turn on the account meets the wall and the relay marks it; the waiting turn is still served in place. `TestAFailedClaimLeavesTheTurnWithTodaysAnswer`: a 403, a 5xx that may have landed, `not_limited`, a wait past its bound, a credit whose type changed by the consume, a consume answered 400 (`failed`) and one answered 502 (`may_have_landed`) each leave the turn moved to the spare, as off does; for each Claude case the next wall makes no second claim and no credential is marked dead. `TestNoSecondClaimInsideTenMinutesOrBeforeAReadingShowsTheMeterBack`: after a claim that landed, a wall 5 minutes later makes none and one 11 minutes later claims; after one that may have landed, a wall 11 minutes later makes none until a reading shows the week back, and a 99% reading does not count. `TestATurnThatHasSentOutputIsNeverHeldOrSentAgain`: a socket turn that forwarded output gets its refusal, the upstream sees it once, and the claim runs. `TestTheOwnersConditionHoldsInRealMode`: a Fable wall with a week reset next and the week at 98% claims nothing, and the turn gets what it gets with the mode off. `TestTheClaimSpendsOnlyTheResetTheDecisionJudged`: when Anthropic offers the Fable reset next at the claim, nothing is sent. `TestRealClaimsOnACodexReadingAtTheLimit`: a reading at 99.6% claims with the cooked rule lifted, and a live week with 5% left refuses it before the consume; with the condition off a live week at 99.6% claims, and one that rolled to 0% is not redeemed (§93.4). `TestADrainingDaemonStartsNoClaimAndLetsWaitingTurnsGo` and `TestANewWorkerDoesNotRepeatAClaimInFlightWithoutAReading` (two daemons over one state directory) cover the drain and the swap. In `cmd/subrouter/auto_reset_test.go`, `TestAutoResetRefusesToStartBesideTheOlderCodexSpenders`. The tests that hold several turns also ran with `-race`.

Each part of §93.2 was taken out of the working tree in turn, one perl substitution each, and its test went red for the predicted reason: the wait at the wall (the turn moved); the wait before a send (a turn answered 503 by the paused admission gate); joining the claim that runs (a turn in flight answered 429); the 10-minute floor; settling only by a reading (here and across the swap); reading the record on disk; writing it before the claim; a drain letting waiting turns go; a draining daemon writing no record; the kind and the condition on the live reading before a consume; lifting the cooked rule at a reading; the redeem clearing marks; the forwarded turn not waiting; naming the judged grant; "back" meaning half used rather than under 99.5%; the walls already decided; the wait's bound; the resend spending no attempt (the refused resend reached the client); the startup refusal; the re-read deciding the resend; and the pass settling latches. The same runs with nothing taken out passed, and a failing first run (a reading at 99% settled a claim that may have landed) is why "back" is half used.

§93.3 The views, the documents, and §93 as a whole.

The row. Each `usage-status` row of an account this daemon holds and could decide for (a Claude or Codex OAuth account, with no credential broker) carries `auto_reset`. It has `mode` (`off`, `shadow` or `real`), `near_empty` (whether the owner's condition is on), `claiming` while an automatic claim runs on the account, and `last`, the account's last decision as `LastAutoResetDecision` keeps it: `at`, `verdict`, `moment`, `kind`, `reset`, `reset_expires_at`, `left`, `reason`, `turn`, and the account, label, provider and mode it was made under. A row with no decision yet carries the mode alone. An API-key row, and every row of a daemon with a credential broker, carries nothing. The field sits at the top of the row, beside `reset_advice`, not in `normalized`: that block is the scheduler's capacity, and it is absent for an account the scheduler has no score for, which would hide the decision. A peer's `usage-status` carries the peer's own values. This daemon ranks nothing on them, because `remotePoolAccountUsage` has no such field, and it decides nothing for a peer's account (§93.1). The decisions live in memory, so a restart forgets them; the latch of §93.2 is kept on disk and is not shown.

`may_have_landed`. §93.3 splits §93.2's `failed`. A claim whose answer leaves it unknown whether the reset was used is now `may_have_landed`: on Claude an `unavailable` or `error` answer or a `request_id` left unconfirmed, on Codex a consume that was sent and came back answered 5xx, with no status, or without a redeem. A Codex consume answered 4xx was refused and is `failed` (§93.4). The decision and the `verdict` of `auto reset: claim finished` both say so. As before, only a reading settles it. `failed` is now a claim that sent nothing or got a definite refusal.

The health route. `/_subrouter/health` carries `"auto_reset": {"mode": ..., "near_empty": ...}` on every listener (`autoResetHealth`).

srq. `srq pool` prints the mode once for each daemon, above that daemon's sections: `--- automatic resets @ local (§93): shadow, it logs what it would claim and claims nothing; the owner's under-1% condition is on`. Real mode reads `REAL, it claims saved resets by itself`, off reads `off, nothing is decided or claimed`, and the condition turned off reads `OFF`. Beside an account with a decision it prints `auto=` and the decision in a few words, in the time of the machine it runs on, with the kind as `week`, `Fable` or `credit`: `auto=would claim week 14:05`, `auto=claiming week since 14:05`, `auto=claimed week 14:05`, `auto=CLAIM FAILED Fable 14:05`, `auto=MAY HAVE LANDED credit 14:05`, `auto=refused <1% week (2.0% left) 14:05`, `auto=no claim (never twice) 14:05`, or `auto=no claim 14:05` for any other stop. A section with a decision gets a legend line. In a peer's section the legend says the decision is the peer's own for its own account and ranks nothing here. `srq pool --json` carries `auto_reset_mode`, `auto_reset_near_empty`, `auto_reset_claiming`, `auto_reset_verdict`, `auto_reset_kind`, `auto_reset_at`, `auto_reset_left` and `auto_reset_reason`. A daemon from before §93 prints no line and no label, and a malformed field reads as none. The pool board does not show automatic resets yet.

The manual commands. `sr claude reset` and `sr codex reset` behave as before in every mode. Their routes call `claudeReset` and `redeemAccountChecked` with no decision, no latch and no owner's condition, so the "never twice" of an automatic claim stops only the next automatic claim. Their y/N rules are the CLI's own (§46.7) and did not change.

The documents. `ops/OPERATING.md` has an "Automatic resets" section under "Saved resets": what each mode does, the owner's condition and that it stays on until the owner turns it off, how to read the would-claim and claim lines, `srq pool`, the row and the health route, what a waiting turn looks like to a client, and how to switch to real mode and back. `ops/skills/subrouter/SKILL.md` says the same in short, and no longer says that nothing claims a Claude reset on its own. `ops/KNOWN-ISSUES.md`'s §56 residual now says that a holder stays at its wall until a manual claim only in shadow mode. `docs/codex.md` says that real mode is the one spender of Codex credits.

§93 as a whole.

What it does. The daemon that holds an account considers claiming the account's next saved reset at two moments only: a real request meets the provider's usage-limit refusal on a meter a saved reset gives back, or a reading says such a meter is at least 99.5% used while the next reset can be claimed now. One function (`decideAutoReset`) decides, with its reason in words. In shadow mode, the default, the daemon logs what it would claim and changes nothing else. In real mode it claims the provider's next reset through the manual routes' own functions while the account's turns wait for it on the account, and it never claims twice without a reading. There is no cap on claims and no list of excluded accounts, by the owner's word.

Why. A saved reset is worth most when it is spent at the wall, on the account the work already runs on: moving a running session to another account re-bills its whole prompt, and a reset left unclaimed at its expiry is lost. The owner asked for subrouter to do all the resetting.

The two switches and how to go back. `SUBROUTER_AUTO_RESET` is `shadow` (unset), `1` (real) or `0` (off). `SUBROUTER_AUTO_RESET_NEAR_EMPTY` is the owner's condition: on (unset or `1`) or off (`0`). It stays on until the owner turns it off. Any other value of either stops `serve`, and so does `1` beside `SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET=1` or `--reset-credit-autospend=spend`. To go back from real mode, unset `SUBROUTER_AUTO_RESET` (shadow) or set it to `0`, and restart. A claim already sent cannot be undone.

What waits in place, and what cannot. Only turns in the usage-limit retry transport wait, because nothing of them has reached the client: the turn at the wall, every turn for the account that arrives while the claim runs, and a turn in flight that meets the wall during it. A client sees no headers and no bytes until the claim answers, at most 3 minutes from the claim's start for Claude and 1 minute for Codex. Then the turn is answered from the same account, or, when the claim fails, may have landed or runs past its bound, gets today's answer. A turn a peer forwarded (its forwarder gives up on headers after 30 seconds), a Codex websocket turn (the relay cannot send the turn again; it closes the socket for a reconnect as today), a pinned turn, a request whose body cannot be replayed, and a turn that has sent output never wait. The claim still runs for each of them.

How it was tested. Every test runs in Docker against fakes; none sends a request to a real provider or claims a real reset. §93.1's tests prove the moments, the condition, the kinds, the decision and shadow mode over fakes that fail on any claim; §93.2's prove the claim in place, the bounds, the drain and the swap over fakes that spend as the providers do. §93.3's tests are in `internal/proxy/auto_reset_view_test.go`, `ops/bin/srq`'s selftest and `ops/docscheck/auto_reset_docs_test.go`. `TestUsageStatusCarriesTheAutoResetModeAndTheLastDecision` reads `usage-status` after a shadow would-claim at the week's wall (the held row carries `shadow`, the condition, and the decision with its time, kind, moment, `left` and reason; the spare row carries the mode alone), with the mode and the condition off (every row says so, with no decision), and in real mode after a claim refused by the condition, a claim that landed, one Anthropic refused with 403 (`failed`), one answered 500 (`may_have_landed`), while a claim is held at the fake (`claiming`, then `claimed`), and after a Codex redeem (`codex_credit`, the credit named). `TestHealthNamesTheAutoResetModeAndTheCondition` reads the health route in three settings. `TestManualResetsAreTheSameInEveryMode` claims by hand through both routes in each mode, in real mode right after an automatic claim landed and a second automatic one was stopped by "never twice", and each manual claim sends one request and lands. The selftest renders local and peer sections with every verdict, a claim in flight, the condition off, mode off, malformed fields and a daemon from before §93. `TestDocsSayWhatAutomaticResetsDo` checks that the operator doc and the skill name both switches, the condition's term, the log lines, the views and the way back, and that no doc but this file still says nothing claims a reset by itself. The existing tests of `sr claude reset` and `sr codex reset` were not changed, and pass. Two of §93.2's expectations moved from `failed` to `may_have_landed`. The CLI's own type for a row (`remoteServerUsageStatus`) learned the field too: the first full run failed `TestUsageStatusWireTypeHasEveryDaemonField` on `unknown field "auto_reset"`.

Each part of §93.3 was taken out of the working tree in turn, one perl substitution each, and its test went red for the predicted reason: the field never added to `usage-status` (every row read nil); `may_have_landed` never set (the row and §93.2's failed-claim test read `failed`); `claiming` never set; the last decision never copied; the health field left out; the health route's condition always on; a manual Claude claim refused in real mode, and a manual Codex redeem refused in real mode (each mode's other legs stayed green). In `srq`, on a copy: the mode line, the row's label, the legend, the peer's legend, the fields left out of `summarize` (the selftest crashed), the claim in flight, the `left` shown on a refusal, `(never twice)`, the check of a malformed decision, and the condition shown as off. In the docs test, `ops/OPERATING.md`, the skill and `ops/KNOWN-ISSUES.md` each put back as they were before §93.3, mounted over the tree in the container. The same runs with nothing taken out passed.

Assumed, not measured. No claim has been made for real by §93, so these stay open until the owner switches a daemon to real mode, and the record §92 writes at every claim (the roll time before and after) is meant to settle the first two:
- whether a Claude claim keeps the account's weekly roll time (the owner thinks it does);
- whether a Codex redeem restarts the account's 7 days (the owner thinks it does);
- whether Anthropic accepts a claim before the limit is reached. Every grant read so far carries `use_requires_limit: true`. At the wall §93.1 counts a grant that waits only for a limit as usable, and the claim reads the block live before it sends; at a reading moment a grant that is not `usable_now` is no moment at all;
- whether Anthropic's block says `usable_now` at the wall, which the exception above assumes it will once read live;
- which `reset_type` values Codex credits carry besides `codex_rate_limits`, which is all the fixtures show; any other is never redeemed automatically;
- whether the Codex CLI waits longer than 1 minute for an HTTP answer's headers, which the Codex bound assumes;
- what happens to a request already at the provider at the moment a reset lands.

What comes later. A later section (planned as §94) replaces `decideAutoReset`'s rule with the plan's: claim a meter's reset when the pool's burn rate plus its trend is at or above 1.0, or when the reset is inside its expiry window, and not when the roll is a few hours away unless the reset expires first. The moments, the claim in place, the latch and these views stay as they are. After that comes loading work onto chosen accounts: pick the accounts that will be reset and send new sessions to them first (one Codex account at a time; the Claude holders with the most time left to their roll; Fable work to the chosen Fable accounts), and a reset burndown that loads holders hard before a reset kind expires. The board, and folding the two older Codex automatic paths into this one decision, come with those.

§93.4 An independent check of §93 proved every part but one test, and found two defects. These are now closed.

A peer's account. `TestNeverForAPeersAccount` drove only the 10-second reading pass, which reads this daemon's own rows, so no peer account reached `autoResetOwnAccount`, the one guard that names it, and the test stayed green with the guard taken out. It now also hands `autoResetAtTheWall`, the function every wall hook calls, a wall naming the box's account as the box's body names it, and a wall naming the laptop's own account. The first decides and logs nothing. The second is a would-claim at the wall, which shows the wall is live.

A Codex reading moment with the condition off. A reading moment lifts the cooked rule (`anytime`), and only the owner's condition looked at the live reading before the consume. With `SUBROUTER_AUTO_RESET_NEAR_EMPTY=0`, a week that rolled between the reading and the claim was redeemed, which spends a credit on a full week. At a reading moment the last check now asks that the live week still read at least 99.5% used, the moment itself, with the condition on or off. The condition then adds its under-1% check. At the wall the cooked rule already asks the live reading for the limit.

A refused Codex consume. `ConsumeRateLimitResetCredit` returned a bare error for any status other than 2xx, so a consume answered 4xx read `may_have_landed`, and `srq pool` showed `MAY HAVE LANDED`. A 4xx is the server refusing the request, and no credit is used. The function now returns `accounts.RateLimitResetConsumeError` with the status, and its message is the same. `redeemAccountChecked` keeps the status beside the result, outside its JSON, so the manual route's answer does not change. A consume answered 4xx is `failed`, and its reason says no credit was used. A 5xx, a consume that got no status, and a 2xx without a redeem stay `may_have_landed`. In both cases the latch holds the account until a reading shows the week back, as before.

A turn's account by its label. The `turn` of a would-claim line and of a decision named the account a turn moved to by its ID, which for an owner-keyed Codex account is a hash. It now names that account by its usage row's label (§87), or by its ID when the row has none.

What the check also noted, and what stays. In shadow mode a Codex would-claim reads the account's credit list, a read-only request, at most once per account in 10 minutes (`autoResetCreditListEvery`), to name the credit a claim would spend (D). A claim that was sent and refused holds the account until a reading shows the meter back, which in practice is its weekly roll, as G says. Without that, a refusal that repeats would be sent again every 10 minutes. `sr claude reset` and `sr codex reset` can still claim by hand.

Tested in Docker. `TestNeverForAPeersAccount`; `TestRealClaimsOnACodexReadingAtTheLimit`, where with the condition off a live week at 99.6% claims and one that rolled to 0% is not redeemed; `TestAFailedClaimLeavesTheTurnWithTodaysAnswer`, where a consume answered 400 is `failed` and says no credit was used, and one answered 502 is `may_have_landed`; and `TestShadowNamesTheCodexCreditThatExpiresSoonest`, over labelled accounts, where the line's `account` and `turn` are labels. Each fix was taken out of a copy of the working tree in turn, and its test went red for the predicted reason. With `autoResetOwnAccount` taking any ID, the box's wall decided `no_claim`. Without the live moment check, the rolled week was redeemed. Without the 4xx branch, and without the typed error, the refused consume read `may_have_landed`. Without the label, the turn named the ID. The same runs with nothing taken out passed.

- Knobs: `SUBROUTER_AUTO_RESET` (`shadow` by default; `0` off; `1` real, which claims), `SUBROUTER_AUTO_RESET_NEAR_EMPTY` (on by default, and on until the owner turns it off; `0` off). Any other value of either stops the daemon from starting, and so does `1` beside `SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET=1` or `--reset-credit-autospend=spend`. A change needs a restart. The constants are `autoResetNearEmptyLeft` (1%), `autoResetReadingMaxAge` (5 minutes), `autoResetReadingUsed` (99.5%), `autoResetLineEvery` (10 minutes), `autoResetCreditListEvery` (10 minutes), `codexFiveHourWindow` (5 hours) and `autoResetScanInterval` (10 seconds); for §93.2 `autoResetClaimFloor` (10 minutes), `autoResetBackUsed` (50%), `autoResetClaudeWait` (3 minutes), `autoResetClaudePatience` (2 minutes 15 seconds), `autoResetCodexWait` (1 minute) and `autoResetCodexClaimBudget` (2 minutes).
- Code: `internal/proxy/auto_reset.go` (`AutoResetMode`, `SetAutoReset`, `decideAutoReset`, `autoResetWallOf`, `autoResetAtTheWall`, `RunAutoReset`, `autoResetReadings`, `considerAutoReset`, `autoResetNextFor`, `claudeGrantWaitsOnlyForALimit`, `autoResetCodexCredit`, `LastAutoResetDecision`, `actOnAutoReset`), `internal/proxy/auto_reset_claim.go` (§93.2: `startAutoResetClaim`, `runAutoResetClaim`, `autoResetClaimClaude`, `autoResetClaimCodex`, `autoResetClaim.waitFor`, `autoResetLatch`, `autoResetWaitsInPlace`), `internal/proxy/proxy.go` (`usageLimitRetryTransport.RoundTrip` and `roundTrip`: the wait at the wall and before a send, and `peerForwarded`; the passive capture; the websocket relay; `AccountRef.autoResetMemory`; `Server.AutoResetStateDir`; the credit a `resetApproval` sees), `cmd/subrouter/main.go` (`autoResetMode`, `autoResetNearEmptyEnabled`, `autoResetSpendsAlone`, the loop, the state directory); §93.3: `internal/proxy/auto_reset_view.go` (`AutoResetStatus`, `withAutoResets`, `autoResetHealth`), `internal/proxy/proxy.go` (`AccountUsageStatus.AutoReset`, `handleUsageStatus`, `handleHealth`), `internal/proxy/auto_reset_claim.go` (`autoResetClaimResult.unsure`, `AutoResetMayHaveLanded`), `ops/bin/srq` (`_auto_reset`, `_auto_reset_mode_line`, `_auto_reset_label`), `cmd/subrouter/sr_server.go` (`remoteServerUsageStatus.AutoReset`, so the CLI's strict decode of a row still takes every field); §93.4: `internal/accounts/codex_reset.go` (`RateLimitResetConsumeError`), `internal/proxy/proxy.go` (`RateLimitResetResult.consumeStatus`), `internal/proxy/auto_reset_claim.go` (the live moment check and the refused consume in `autoResetClaimCodex`), `internal/proxy/auto_reset.go` (`autoResetLabel`, `autoResetTurnAfterFailover`)
- Tests: `internal/proxy/auto_reset_test.go`, `internal/proxy/auto_reset_claim_test.go`, `internal/proxy/auto_reset_view_test.go`, `cmd/subrouter/auto_reset_test.go`, `ops/docscheck/auto_reset_docs_test.go`, `srq selftest`
- Docs: `ops/OPERATING.md` (Automatic resets), `ops/skills/subrouter/SKILL.md`, `ops/KNOWN-ISSUES.md` (§56's residual), `docs/codex.md`
- Related: reads §46's claim rules (`chooseClaudeResetGrant`, the cached block) and claims through §46's functions, §64's kinds, §21's warm sessions and §87's labels; never for §90's turn. §29.5's failover redeem and the background spender are unchanged, and cannot run beside real mode. §58's hold and §89's final answer are what a turn gets when it stops waiting.

## 94. An automatic claim is made only when it is worth it: the pool's pressure, what a claim adds, last chance and the expiry window

The owner, 2026-11-22: subrouter is to "maximize the value got from each reset", and to "only reset if we are over burn rate, or the second derivative is taking it well over 1.0 to 1.1". The plan they approved adds: do not claim a Claude reset when the roll is a few hours away, since nothing given back could be spent again before it, unless the reset expires first. §93's decision is deliberately simple: at the limit, with demand, claim. §94 replaces that rule with the plan's step 4, gives the expiry window of step 6 as one function, and folds the two older Codex automatic paths under the one decision (step 7). The owner's under-1-percent condition (§93.1) is not touched: it stays the first gate, on by default, and the default mode stays shadow. §94 is built in parts: §94.1 computes and publishes the figures the rule reads, and decides nothing; §94.2 decides on them; §94.3 shows what the rule used in the views and the documents, and sums up §94 as a whole.

§94.1 Pressure, need, what a claim adds, last chance and the expiry window.

Pressure. §92.2 gives each meter's pool burn per window and the share of the counted accounts' time at the limit. An account at its limit burns nothing, so the mean reads low exactly when the pool is short. Pressure is the burn among the accounts that could still spend: the window's burn divided by 1 minus its share at the limit. A pool whose 24-hour burn is 0.84 with 30% of its account-time at the limit has pressure 1.2; with none at the limit pressure is the burn. A window whose every counted account spent it at the limit has pressure "all at the limit", above any bar. A window with too little history has none. Pressure is computed from the same rows burn.json is built from, this daemon's accounts and its counted peers' alike (`pressureOf`, `poolWindowPressure`).

Need. Per meter (Claude week, Claude Fable, Codex week, and a Codex model pool's own week once read), the pool needs its saved resets when the 24-hour pressure is at or above 1.0 (`resetNeedAt`), or when the 6-hour pressure plus the 6-hour trend is at or above 1.1 (`resetNeedRisingAt`): the owner's second-derivative case. While the 24 hours have too little history, the 6-hour pressure alone decides at 1.0, and after it the 1-hour one. With no figure at all need is unknown. Three readings are this section's, not the plan's words, each with its reason:
- The trend is the change in pressure from the 6 hours before, over the accounts with a figure in both windows, as §92.2 takes the burn trend. The burn's own trend reads low when accounts reach their limit, the case pressure is for. In the route's test one account rises from 0.6 to 0.9 while another sits spent: the burn trend reads +0.15 and misses the 1.1, the pressure trend reads +0.30 and reaches it.
- A 6-hour window all at the limit counts as above 1.1 whatever the trend, and needs none.
- The trend is read only beside a 24-hour figure. With no 24-hour figure the 6-hour pressure decides alone, as the brief says.

burn.json carries `need` on each meter: `answer` (`yes`, `no` or `unknown`), `window` (the window that decided: `24h`, `6h` or `1h`), `pressure` (per window a number, `"all_at_limit"`, or null), `trend` (the 6-hour pressure trend, null when it is not known), and `why`, one sentence with the numbers, such as `the 24-hour pressure is 0.80, under 1.0, but the 6-hour pressure 0.95 plus its trend +0.20 is 1.15, at or above 1.1`. `srq burn` prints it as one line under each meter's label, `  resets needed  yes: <why>` (`not known yet` for unknown), with a legend line. A daemon from before §94 publishes no `need`, and `srq burn` then says nothing about it.

What a claim adds (`claimValueOf`), as a share of one week's budget, from the kinds table's `restarts_clock`:
- A reset that keeps the roll (a Claude week or Fable reset) adds what the account can spend again before its roll: the rate times the hours to the roll over 168, at most 1. The rate (`claimRateOf`) is the account's own pressure on the meter over the longest window that has one, never under the pool's pressure when that is known, and 1.0 when neither is. An account burning 3.0 with 28 hours to its roll adds 0.5, and with 70 hours 1. One with no figure in a pool at pressure 1.5 reads at 1.5. In a pool all at its limit the claim adds 1 while any time is left. The account's own pressure, not its burn, is this section's reading: an account deciding a claim is at its limit, and its time there burned nothing. With none of its time at the limit the two are the same.
- A reset that restarts the seven days (a Codex credit) adds the wait it skips: the hours to the roll over 168. A Codex credit with 42 hours to the roll adds 0.25, whatever the rate.
- A kind whose `restarts_clock` is unknown has no value, and §93.1 already never claims it automatically.

Last chance (`lastChance`). A reset is in its last chance when it expires before the account could meet its next wall: its roll plus 168 hours over the rate, the rate floored at 1, so at most a week after the roll. A reset that expires before the roll is in it; one that expires two days after the roll is in it at rate 1 and not at rate 7.

The expiry window (`expiryWindow`), one pure function a later step will also call. It opens `lead` before a kind's expiry date. The lead is the time the pool needs, at its present demand on the kind's meter, to spend what the holders of those resets still have left, times 1.5 (`resetWindowMargin`), never under 24 hours (`resetWindowFloor`) and never longer than the time left. In weeks of one account: the holders' headroom H over the demand D a week, where D is the pool's pressure over the longest window that has one times the accounts that window counts. Eleven holders with 6.0 weeks of headroom between them, in a pool of 25 counted accounts at pressure 1.0, open about 60 hours before (1.5 x 6 / 25 weeks); the same pool at pressure 4 opens at the 24-hour floor. When the demand is not known, or the pool is all at the limit, the lead is 24 hours. A pool that spends nothing on the meter opens the window at once: at its present demand the holders' headroom would never be spent. Each holder of a kind and date adds what it has left on the kind's meter (`resetHoldings`): a whole week when that meter is unread, and a week more for each reset of the kind and date beyond its first, since using the second needs the account spent again. That week more is this section's reading; the brief's sum counts each holder once. An account two pools hold counts once, from this daemon.

Each `resets` entry of this daemon's own usage-status rows, for a kind the table knows and a date still ahead, carries `expiry_window`: `opens_at`, `lead_seconds`, and `why`, such as `3 holders have 1.4 weeks of the Codex week left between them; at the pool's 24-hour pressure 1.00 over 4 accounts they need 58h48m to spend it, and 1.5 times that is 88h12m`. The holders and the demand are this daemon's rows and each counted peer's rows as its last usage body gave them (`withResetWindows`). usage-status reads no peer: a peer that reads this daemon's usage-status would be read back, and each read could start the other's usage sweep, for ever. The body is the one a routing decision, burn.json or the board last read, and one more than 15 minutes old is not counted (§92.2). A peer's rows keep the peer's own values: `GET /_subrouter/peers/usage-status` hands a peer's body on verbatim (§32), and pool.json's compact list carries no window. `srq` does not show the window yet. With the usage history off there is no pressure, so every window opens 24 hours before its expiry.

Measured since §91, 2026-11-23. A Claude week reset was claimed by hand on an account at 100% used. Before and after, the account's weekly roll time was the same instant. The week meter went from 100% used to 3% or less, and the Fable meter from 79% to 3% or less. The account's block went from one week reset and one Fable reset to the Fable reset alone. So a Claude week reset does not restart the seven days, and it gives back the Fable meter too. One claim, on one account. The kinds table's `week` entry now reads `restarts_clock` `measured_no`, a new value (`ClockRestartMeasuredNo`), and usage-status publishes it. Its `GivesBack`, used only when a grant names nothing, stays `seven_day`: the December grants name both meters in their own `clears`, which the list publishes. Fable stays `assumed_no` and Codex `assumed_yes`. Still not measured: whether a Codex redeem restarts the seven days, whether a Fable reset keeps the roll, and whether a claim is accepted before the limit. Each kind now also names its `Meter` (`week` or `fable`): the meter a holder spends to reach the limit its claim lifts, which the expiry window reads. `accounts.ResetKindNamed` finds an entry by provider and kind.

Nothing ranks, routes or decides on any of it. Need and the windows are added to usage-status and burn.json after the scheduler has taken its scores from the same rows, and a peer's reading of a row for ranking (`remotePoolAccountUsage`) has no field for them. `decideAutoReset` is §93's, unchanged: what a claim adds and last chance are functions only their tests call until §94.2. There is no switch, because §94.1 only publishes. `SUBROUTER_USAGE_HISTORY=0` takes burn.json away, and with it need.

Tested in Docker, every provider a fake, one test per behaviour, in `internal/proxy/reset_worth_test.go`: `TestPressureIsTheBurnOfTheAccountsThatCouldStillSpend` (0.84 at 30% is 1.2, none at the limit is the burn, all at the limit, too little history, and the wire form both ways); `TestNeedFollowsThePressureAndItsTrend` (1.0 yes, 0.99 no, all at the limit yes, 0.8 with 0.9 rising by +0.25 yes and by +0.1 no, no trend yet, the last 6 hours all at the limit, the 6-hour and then the 1-hour pressure deciding at 1.0 without a 24-hour figure, unknown with none, each with the numbers in its sentence); `TestTheBurnRoutePublishesNeedPerMeter` (a laptop with a rising and a spent Codex account and a Claude peer of this build: the Codex week needs resets on its 6-hour pressure 0.90 and trend +0.30 while its burn trend reads +0.15, the Claude week from the peer alone does not at 0.50, the Fable meter does at 1.50, and a daemon with 20 minutes of history publishes unknown on every meter, with the wire form); `TestTheValueOfAClaim`; `TestLastChance`; `TestTheExpiryWindowsLead` (about 60 hours, the floor at pressure 4, unknown demand, all at the limit, the cap at 40 hours left, 10 hours left, a pool that spends nothing); `TestTheHoldersHeadroom` (a read meter, an unread one, a second reset of one kind and date, an account two pools hold, an unknown kind and a date gone by); `TestUsageStatusCarriesEachResetsExpiryWindow` (a laptop of three Codex accounts and a box of one, each with a usage history: the laptop's credits ending in 5 days are held on both daemons and open 1.5 x 1.4 / 4 weeks before, its two credits ending in 12 days open 1.5 x 1.6 / 4 weeks before, an account with none lists none, and the laptop relays the box's row with the box's own window, which counts only the box and is open); `TestTheClaudeWeekResetKeepsItsRollAsMeasured` (the table and usage-status). The goldens of §91 (`TestHeldResetsListsEachKindAndExpiryInTheProvidersOrder`, `TestResetInfoListsEveryResetByKindAndExpiry`, `TestUsageStatusListsEveryClaudeResetByKindAndExpiry`) read `measured_no` for the week kind, and §91's list checks in `held_resets_test.go` compare each entry without its window, which this section's tests read. `ops/docscheck/reset_worth_docs_test.go`'s `TestDocsSayTheWeekResetKeepsItsRoll` requires the operator doc and the skill to say `measured_no` and the date it was measured, and no doc but this file to say that no `restarts_clock` is measured. `srq selftest` covers the need line for each answer, the legend, a daemon from before §94 and a need that does not read.

The selection tests of §91 and §92 (`TestTheResetsListChangesNoSelection`, `TestAPeersResetsListChangesNoRanking`, `TestSelectionIsTheSameWithBurnPublished`) and §93's tests pass unchanged.

Each part was taken out of a copy of the working tree in turn, one replacement each, and its test went red for the predicted reason: pressure without the division (0.84 at 30% read 0.84, not 1.20); need without the trend rule (0.8 with 0.9 rising by +0.25 read no); need without the 6-hour and 1-hour fallback (read unknown); burn.json without `need`; the burn's trend in place of the pressure's (the Codex week read no, at 0.90 plus +0.15); the account's own burn in place of its pressure (0.167 for 0.333); no pool floor on the rate (0.083 for 0.25); a Codex credit valued by the rate (0.75 for 0.25); an unknown `restarts_clock` given a value; last chance without the floor at 1 (ten days after the roll at rate 0.5 read as last chance); the window without its 1.5, without its 24-hour floor, and without its cap at the time left; usage-status without the windows; the peer's holders left out (2 holders with 0.9 weeks); no week for a second reset; an unread meter read as spent; an account two pools hold counted twice; the week kind back to `assumed_no` (the table and usage-status); and the peer's figures left out of the pool (the Claude week read unknown). In `srq`, the need line and its legend, each taken out. In the docs check, the operator doc and the skill, each put back as they were, mounted over the tree in the container. The same runs with nothing taken out passed. Two first attempts did not go red. The last-chance case at rate 0.5 expired two days after the roll, where the floor changes nothing; it now expires ten days after. One replacement did not compile, and was made again.

Residuals. A daemon that has read no peer in 15 minutes (no new session, no burn.json and no board read) publishes windows from its own accounts alone. A Fable reset queued behind a week reset needs the week reset used first, which the window does not count; the Fable resets expire in March, long after the week resets. A holder's credits of an earlier date, which the redeem takes first, are not counted in a later date's window. Each account weighs the same, as in §92.2. An unread meter counts as a whole week left, which opens a window earlier, never later.

§94.2 The decision, its switch, the live read, the claim record check and the fold of the older Codex paths.

The decision. `decideAutoReset` stays one pure function of its facts, and the facts grow by what the rule reads (`autoResetWorth`): the pool's need on the meter the next reset is claimed on (§94.1), the account's own figures there, the kind's `restarts_clock`, the account's roll on that meter from its reading, the reset's expiry, and the expiry window of its kind and date. Every check of §93.1 comes first and is unchanged: a live credential, demand, a known next reset that can be claimed now, and the owner's under-1-percent condition. Then, in this order (`decideWorth`):
1. A kind whose `restarts_clock` is `unknown` is never claimed: a claim has no value it can be judged by.
2. Inside the expiry window, or in its last chance, the reset is claimed at any value. The reason says which: `inside its expiry window, open since <time>, <lead> before the reset expires: any demand at the limit is enough`, or `its last chance: it expires at <time>, before the account could meet its next wall after the roll at <time> at the rate <rate>`.
3. Need unknown: no claim, `the pool's burn on the <meter> is not known yet (<why>), so nothing is claimed`.
4. Need no: held back, `held back: the pool does not need its saved resets on the <meter> (<why, with the pressures>), so the rest of the pool can carry the work`.
5. The account's roll on the meter not read: no claim, since what a claim adds is not known.
6. A claim that adds under 0.25 of a week (`resetValueFloor`): held back, `held back: the roll is 10h away, and a claim would add 0.07 of a week, under the floor of 0.25 (...)`.
7. Otherwise claim, with the pressure and the value: `the pool needs its saved resets on the Claude week (the 24-hour pressure is 1.20, at or above 1.0), and a claim adds 0.43 of a week (...)`.

`held_back` is a new verdict beside §93's. The rule's facts are gathered only for a decision §93.1's rule would claim on (`autoResetWorthFor`), as a Codex decision reads its credit list only then: this daemon's last sweep, each row with its resets and figures, and each counted peer's last usage body. The pool is read by the same function the windows of usage-status are built from (`resetPoolOf`, extracted from §94.1's `withResetWindows`), at the decision's own clock. No peer is fetched for a decision: a turn at the wall waits on nothing new.

What a held-back turn gets. Exactly what it gets today: in shadow mode the wall's decision already runs after the turn, and in real mode a held-back decision starts no claim, so the turn at the wall does not wait and goes to the marks and the failover, or gets §58's hold and §89's final answer. No would-claim line is logged; the decision is kept as the account's last (`LastAutoResetDecision`). A held-back account is decided again at its next reading (each new reading is a new moment, §93.1) and at its next wall, so a rise in the pool's pressure or the opening of the expiry window is picked up without a restart.

The switch. `SUBROUTER_AUTO_RESET_WORTH`: unset or `1` is on, `0` goes back to §93.1's rule exactly, and any other value stops `serve`, read as §93's two switches are. Startup logs a Warn when it is `0`. The package default, for a `Server` that `serve` did not set up, is off, so §93's own tests decide as they did. With `SUBROUTER_USAGE_HISTORY=0` there is no pool figure, so need is always unknown and nothing is claimed outside the expiry window and last chance.

The live read before a Claude claim at a reading moment. §93.4 made a Codex reading moment read the week live before the consume; a Claude claim went by the cached block when it was claimable, so a week that rolled between the reading and the claim could be claimed on. An automatic Claude claim at a reading moment now sets `ClaudeResetRequest.liveCheck` (unexported, never on the route's JSON): `claudeReset` then reads the block live, never from the cache, does not fall back to the cached block when that read fails, and judges the live meters before the profile read and the claim. The meter the moment found must still read at least 99.5% used, and while the owner's condition is on the reset's meter must have under 1% left. Otherwise nothing is sent, and the decision is `failed` with `nothing was sent: the live reading before the claim no longer says the week is at least 99.5% used`. At the wall the refusal is the evidence, and the claim reads nothing more than §93.2's did.

The claim records against the table. When a landed claim's record (§92.1) has its reading after it, `claimAgainstTable` compares it with its kind's entry: the roll of the kind's meter moved by more than five minutes (`usageSegmentRollMove`) or it did not, against `restarts_clock`; and each meter the kind gives back (`GivesBack`: `seven_day` and `weekly` are the week, `seven_day_overage_included` the Fable meter) fell or did not. When they disagree it logs `reset claim contradicts the kinds table; the table is unchanged` at Warn with `account`, `provider`, `kind`, `table` (what the table says, such as `restarts_clock assumed_yes`) and `claim` (what it showed, such as `the week roll stayed at <time>`). A failed claim, a record with no reading after it, a kind the table does not know, a meter not read on both sides, a roll that had already passed at the claim, and a meter at 0 before it settle nothing. The table changes only in a new build. Claims by hand through a daemon are checked too.

The expiry window opens at once when capped. §94.1's window rounds its lead to the second. A lead capped at the time left means the window is open, but rounding could put `opens_at` up to half a second after the decision that reads it, which then read the window as shut. A capped window now opens at the second its clock reads, and the decision reads the pool at its own clock. Seen while this section was built, not in the field: `TestAWindowCappedAtTheTimeLeftIsOpenNow` fails without the fix.

The fold. In real mode (`SUBROUTER_AUTO_RESET=1`) the one decision is the only thing that spends Codex credits. The failover redeem of `SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET` redeems nothing (`failoverRedeems`), and the background spender in its spending mode only advises, as in warn mode: it keeps publishing `reset_advice` and spends nothing. `serve` logs one Warn for each that is set, naming the setting and saying that automatic resets now decide, and starts. §93.2's refusal to start beside either is gone. In shadow mode and with automatic resets off, both older paths behave exactly as before, and their tests are unchanged.

Not here. The views of part L (what the rule used, as fields of the last decision on a usage-status row, and `srq pool` naming a held-back decision) come with §94's views. `srq pool` reads a `held_back` decision as none, since its list of verdicts does not know it yet, and so prints nothing beside the account.

How to go back. `SUBROUTER_AUTO_RESET_WORTH=0` and a restart decide by §93's rule again. `SUBROUTER_AUTO_RESET` unset or `0` leaves the older Codex paths as they were.

Tested in Docker, every provider a fake, in `internal/proxy/auto_reset_worth_test.go`:
- `TestTheRuleClaimsOnlyWhenThePoolNeedsItAndItAddsEnough`: a week reset at 0.4% left with 60 hours to its roll, in a pool at 24-hour pressure 0.7, is held back with the pressure; at 1.2 it claims and adds 0.43 of a week. With the account's own burn 1.2 and 10 hours to the roll a claim adds 0.07 and is held back with the hours and the share; with 40 hours, 0.29, it claims. With no figure it claims nothing, and says the burn is not known yet.
- `TestLastChanceAndTheExpiryWindowOverrideTheRule`: the held-back fixtures at 0.7 and at 10 hours, each expiring before the roll or inside its window, claim, and the reason says which. A kind whose clock is not known is not claimed even then. Expiring six days after the roll is last chance at rate 0.7 (floored at 1), and eight days after is not.
- `TestACodexCreditIsWorthTheWaitItSkips`: at pressure 1.3, 50 hours to the roll claims (0.30), 30 hours is held back (0.18), and 30 hours with the credit expiring first claims.
- `TestTheOwnersConditionStillComesFirst`: at 2% left every fixture, the window and last chance among them, is refused by the condition.
- `TestWithTheRuleOffEveryFixtureDecidesAsSection93`: without the rule's facts each fixture's decision equals §93's, field by field.
- `TestTheDaemonDecidesByThePoolsPressureAtTheWall`: a shadow daemon whose usage history holds the spare's Claude week, at the week's wall of an account read at 95% with 60 hours to its roll. At pressure 0.7 the decision is held back, with no would-claim line; at 1.2 a would-claim adding 0.43; with the account's own burn 3.0 beside the spare's 1.2 (pool 2.1) and 15 hours to the roll, a would-claim adding 0.27 at its own rate; with no history, no claim; expiring in 50 hours, last chance; with two holders of a reset ending in 300 hours at pressure 0.5, inside the window; and with the rule off at 0.7, §93's would-claim and reason. Every turn's answer equals the answer with automatic resets off.
- `TestRealModeHoldsNothingForAHeldBackDecisionAndDecidesAgainAtTheNextReading`: in real mode at pressure 0.70 the wall claims nothing and the turn is moved to the spare; at 1.40 the claim is made and the turn served in place. On readings, an account at 99.6% with a grant usable now is held back at pressure 0.70; the same reading is not decided again; the pool's clock moves 13 hours to pressure 1.40, the next reading claims.
- `TestARealClaudeClaimAtAReadingReadsTheMeterLiveFirst`: a week that rolled after the daemon's reading is not claimed on, and the decision says the live reading no longer shows it spent; a week still at 99.6% is claimed after one live read and the re-read after it. At the wall, with a cached block that is claimable, the claim reads usage once, the re-read after it.
- `TestAClaimRecordThatContradictsTheTableWarns`: a Codex redeem whose roll stayed, a Claude week claim whose roll moved four days, and a Codex redeem whose week did not fall each log one Warn naming the kind, the table and the claim; a Codex redeem that restarted the week and a Claude week claim that kept its roll log nothing; the table reads `assumed_yes` and `measured_no` after.
- `TestRealModeTurnsTheOlderCodexSpendersOff`: over §29.5's cooked pool and the spender's fixtures, in shadow mode the failover redeem revives the account and the spend mode redeems two credits, as before; in real mode neither redeems anything, and the spender's results are advice, published as `reset_advice`.
- `TestAWindowCappedAtTheTimeLeftIsOpenNow`: a window capped at the time left opens no later than a clock a tenth, four tenths or six tenths of a second past the second.
In `cmd/subrouter/auto_reset_test.go`, `TestServeSetsTheAutoResetWorthRule` (the switch's values, `serve` setting it, the Warn when off and the refusal naming it) and `TestRealModeFoldsTheOlderCodexSpenders` (each combination of real mode with the two older paths starts, with one Warn per path; shadow and off log none), which takes the place of §93.2's `TestAutoResetRefusesToStartBesideTheOlderCodexSpenders`. `ops/docscheck/reset_worth_docs_test.go`'s `TestDocsSayHowAutomaticResetsDecideAndWhoSpends` requires the operator doc and the skill to name the switch, `held_back`, the failover setting and the contradiction line, and no doc but this file to say that real mode refuses to start beside the older spenders. The fakes of §93 learned three things for these tests: a Claude profile's roll, grants usable before the limit, and a count of usage reads. The selection tests of §91 and §92 (`TestTheResetsListChangesNoSelection`, `TestAPeersResetsListChangesNoRanking`, `TestSelectionIsTheSameWithBurnPublished`), the `selectacct` package, and every test of §93 and of the two older Codex paths pass unchanged. The tests that hold several turns also ran with `-race`.

Each part was taken out of a copy of the working tree in turn, one perl replacement each, and its test went red for the predicted reason: a held-back verdict made a claim (pressure 0.7 read would_claim); no value floor (10 hours to the roll claimed); no window override, and no last chance (each held-back fixture stayed held back); an unknown need taken for yes (no figure claimed); the rule ahead of the owner's condition (2% left read held_back); the rule on with its switch off, and never on (the daemon's 0.7 cases); no live read at a reading (the rolled week was claimed); the live read at the wall too (the wall's claim refused); the live check taking the cached block (the rolled week was claimed); the contradiction Warn, the roll check and the fall check each taken out; the failover redeem and the spender each left spending in real mode (one consume, two consumes); no startup Warn for the fold; `serve` not setting the switch, logging no Warn when it is off, and reading any value as on; the capped window opening at the rounded lead (a clock a tenth of a second before the second read it as shut); the reading's roll, the account's own figures, the window, the expiry and the pool's need each left out of the facts (the daemon test's 1.2, own-rate, window, last-chance and 0.7 cases); and a kind whose clock is not known claimed inside its window. None failed to build. The same runs with nothing taken out passed. In the docs check, the operator doc, the skill and `docs/codex.md` each put back as they were, mounted over the tree in the container, went red.

Residuals. A decision reads each peer from its last usage body, as usage-status does: a daemon that has read no peer in 15 minutes decides on its own accounts' figures. The value of a claim reads the account's roll from its last reading, at most five minutes old at a reading moment and as old as the last sweep at the wall. A held-back decision is shown only in `usage-status` until §94's views. Need, value and last chance are judged per meter on this daemon's figures and its peers', each account weighing the same (§92.2). Still not measured, as §94.1 says: whether a Codex redeem restarts the seven days, whether a Fable reset keeps the roll, and whether a claim is accepted before the limit; the record check now says so in the log the first time a claim shows otherwise.

§94.3 The views, the documents, and §94 as a whole.

What the rule used, on the decision. Each decision that §94.2's rule makes carries `rule` (`AutoResetRule`), which `decideWorth` fills from the same figures its reason is written from:
- `decided_by`, the step the rule stopped at: `unknown_clock`, `expiry_window`, `last_chance`, `need`, `roll_unread` or `value`. `need` is a held-back pool that does not need the reset, or a no-claim pool whose need is unknown; `value` is a held-back claim under the floor, or a claim made.
- `need` (`yes`, `no` or `unknown`), and `pressure` with its `window`: the pool's pressure over the window that decided need, as a number, `"all_at_limit"`, or null when need is unknown. `trend` is the 6-hour pressure trend need read, or null.
- `value`, the share of a week a claim adds now, and `hours_to_roll`, to a tenth of an hour. Both are null when the account's roll was not read, and `value` is null too for a kind whose clock is unknown.

A decision the rule did not make carries no `rule`: with `SUBROUTER_AUTO_RESET_WORTH=0`, or when a check of §93.1, the owner's condition among them, stopped the claim first. A real claim copies the decision it starts from, so `claimed`, `failed` and `may_have_landed` keep what the rule used, and so does a `no_claim` that "never twice" stopped. The row's `last` is that decision, so usage-status carries it on this daemon's own rows. A peer's rows through `/_subrouter/peers/usage-status` are the peer's body verbatim (§32), so they keep the peer's own `rule` and `worth`. Nothing ranks on them: `remotePoolAccountUsage` has no such field.

Whether the rule decides. Each row's `auto_reset` and the health route's `auto_reset` carry `worth`, the value of `SUBROUTER_AUTO_RESET_WORTH` as the daemon runs it.

srq. The mode line of `srq pool` ends `; §94's rule decides whether a claim is worth it`, or `; §94's rule is OFF, §93's alone decides` with the switch at `0`. A daemon before §94 publishes no `worth`, and the line then says nothing of the rule. Beside an account:
- `auto=held back week (not needed, pressure 0.70) 14:05` is a pool that does not need the reset.
- `auto=held back week (adds 0.07 wk, rolls in 10h) 14:05` is a claim that would add too little before the roll.
- A claim that the expiry window or last chance decided ends `(expiry window)` or `(last chance)`: `auto=would claim week (expiry window) 14:05`, `auto=claiming week (last chance) since 14:05`, `auto=claimed week (last chance) 14:05`.
- `auto=no claim (burn not known) 14:05` is a pool with too little history for any figure.

The legend explains held back and the two tags. `srq pool --json` adds `auto_reset_worth`, `auto_reset_decided_by`, `auto_reset_need`, `auto_reset_pressure`, `auto_reset_value` and `auto_reset_hours_to_roll`. A malformed `rule` reads as none, and a `worth` that is not a boolean reads as not published. This closes §94.2's "Not here": `srq pool` no longer reads a `held_back` decision as none, and a held-back decision is no longer shown only in usage-status.

The documents. The Automatic resets section of `ops/OPERATING.md` says, in plain words with the numbers, when the daemon claims and when it holds back. It says that the owner's condition comes first, what pressure is and why the rule reads it and not the burn, the switch and how to go back, that in real mode the two older Codex settings no longer spend, and how to read a held-back line in `srq pool` and in usage-status. Its "Read the pool's burn" no longer says that nothing claims on need. `ops/skills/subrouter/SKILL.md` says the same in short, and no longer says that nothing claims on need or on the expiry window. Both now say that automatic resets read each account's `resets` list for the expiry window, where they said that nothing claims on it. §94.2 corrected §93's refusal to start beside the older Codex settings in the operator doc, the skill and `docs/codex.md`. The older PATCHES sections keep it as history.

The manual commands. `sr claude reset` and `sr codex reset` behave as before in every mode, with the rule on or off. Their routes call `claudeReset` and `redeemAccountChecked` with no decision, and §94.2's fold gates only the failover redeem and the spender. Their tests did not change.

Tested in Docker, every provider a fake, in `internal/proxy/auto_reset_worth_view_test.go`:
- `TestEachDecisionCarriesWhatTheRuleUsed`: on §94.2's fixtures, each step names itself with its figures. Pressure 0.7 is held back by need (pressure 0.70, value 0.25, 60 hours). 10 hours to the roll is held back by value (0.071). 1.2 with 60 hours is claimed by value (0.429). No figure is need unknown, with pressure null. Inside the window and in last chance each names its step. A Codex credit with 30 hours to the roll is held back by value (0.179). A kind whose clock is unknown has value null, and a roll not read has value and hours null. The rule off, and a decision the owner's condition refused, carry no rule.
- `TestUsageStatusCarriesWhatTheRuleUsed`: a shadow daemon's usage-status at the week's wall carries the same figures for need, value, no history, last chance and the expiry window, with `worth` true on every row. With the rule off the rows say `worth` false and the decision carries no rule. In real mode a claim at pressure 1.40 is `claimed` and keeps need `yes`. A box that held back a claim is read by a laptop whose rule is off: the box's row as the laptop relays it keeps the box's `worth` and rule, and the laptop's own row says the rule is off.
- `TestHealthSaysWhetherTheRuleDecides`: on and off. §93.3's `TestHealthNamesTheAutoResetModeAndTheCondition` compares the whole field, and now reads `worth` false for a `Server` that `serve` did not set up.
- `TestManualResetsAreTheSameWithTheRuleOn`: in real mode with the rule on, a manual Claude claim and a manual Codex redeem each send one request and land.

`srq selftest` covers each held-back reading, the two tags, a claim in flight, the mode line with the rule on, off and from a daemon before §94, a peer's section, the JSON fields and malformed rules. `ops/docscheck/reset_worth_docs_test.go`'s `TestDocsSayHowToReadWhatTheRuleUsed` requires the operator doc and the skill to say that the condition comes first, "Pressure, not burn", the held-back labels, the two tags, `decided_by`, `hours_to_roll` and `worth`. No doc but this file may still say that nothing claims on need or on the resets list. The tests of `sr claude reset` and `sr codex reset`, §93.3's `TestManualResetsAreTheSameInEveryMode`, the selection tests of §91 and §92 (`TestTheResetsListChangesNoSelection`, `TestAPeersResetsListChangesNoRanking`, `TestSelectionIsTheSameWithBurnPublished`), the `selectacct` package, and every test of §93 and §94.2 pass unchanged.

Each part was taken out of a copy of the working tree in turn, one replacement each, and its test went red for the predicted reason: the decision without its rule (every case read no rule, on facts and through usage-status); last chance named as the window, the window named as `value`, and a held-back pool named as `value` (each step misnamed); no hours to the roll, and no value (each read null); figures kept without a roll (the roll not read read 0 hours, not null); the pressure of another window (0.7 read null); a rule on a decision the rule did not make (the rule off read an empty rule); `worth` left off the rows, and off the health route; a real claim that drops the rule (the claim read no rule); and the fold reaching the manual Codex route, and the rule reaching the manual Claude route (each manual claim answered 409). In `srq`, on a copy: `held_back` unknown to it, the held-back reason dropped, the value reason dropped, the two tags dropped, the mode line without the rule, the rule not read, a boolean read as a number, `(burn not known)` dropped, and the legend without held back. In the docs check, the operator doc and the skill, each put back as they were before §94.3 and mounted over the tree in the container. One replacement, `worth` taken off the rows, first left a variable unused and did not build; it was made again. Two `srq` runs and one docs run first failed on Docker's file sharing, which served a file still being written; each went red for the predicted reason when run again. The same runs with nothing taken out passed.

§94 as a whole.

What it does. At §93's two moments, after every check of §93.1 and the owner's under-1-percent condition, the daemon claims an account's next saved reset only when the claim is worth making. Inside the reset's expiry window, or in its last chance, any demand at the limit is enough. Otherwise the pool must need its saved resets on the meter the reset is claimed on, and a claim must add at least a quarter of a week before the account's roll. Else the claim is held back, and the account is decided again at its next reading or wall. In shadow mode, the default, nothing is claimed either way.

Why. The owner, 2026-11-22: "maximize the value got from each reset", and "only reset if we are over burn rate, or the second derivative is taking it well over 1.0 to 1.1". A reset is extra budget. It adds work only when demand is above what the pool's weeks give. A Claude reset claimed a few hours before a roll it does not move gives back budget that nothing can spend. A reset left unclaimed at its expiry is lost, so near the expiry the bar drops to any demand.

Each number and where it is named. In `internal/proxy/reset_worth.go`: `resetNeedAt` (1.0) and `resetNeedRisingAt` (1.1), the owner's two numbers, under a dated ruling comment; `resetWindowMargin` (1.5); and `resetWindowFloor` (24 hours), the owner's "about 24 hours before a reset expires". In `internal/proxy/auto_reset.go`: `resetValueFloor` (0.25). From §92.2: the 1-, 6- and 24-hour windows and the 168-hour week (`usageMeterLength`). From §93: `autoResetNearEmptyLeft` (1%) and `autoResetReadingUsed` (99.5%).

The switch and how to go back. `SUBROUTER_AUTO_RESET_WORTH`: unset or `1` is the rule, `0` is §93's rule exactly, and any other value stops `serve`. A change needs a restart. `SUBROUTER_AUTO_RESET` (`shadow` by default) and the owner's `SUBROUTER_AUTO_RESET_NEAR_EMPTY` (on) are §93's, unchanged. In real mode the two older Codex automatic paths spend nothing. With `SUBROUTER_AUTO_RESET` unset or `0` they behave as they did before §94.

Measured, and still assumed. Measured on 2026-11-23 with one claim by hand on one account (§94.1): a Claude week reset keeps the account's weekly roll, and gives back the Fable meter as well as the week. The kinds table reads `measured_no` for it. Still assumed: that a Codex redeem restarts the seven days (`assumed_yes`), that a Fable reset keeps the roll (`assumed_no`), and whether a claim is accepted before the limit. What a claim adds rests on the first two. §94.2's record check logs a Warn the first time a claim shows the table wrong. The table changes only in a new build.

How it was tested. Every test runs in Docker against fakes. None sends a request to a real provider or claims a real reset. §94.1's tests prove the figures on fixtures and through burn.json and usage-status. §94.2's prove the decision on facts and through shadow and real daemons, the switch, the live read, the record check and the fold. §94.3's prove the views and the documents. Each part was taken out in turn, and its test went red.

What comes later. Loading work onto chosen accounts, the plan's step 5: when the pool needs its resets, choose the accounts that will be reset and send new sessions to them first. That is one Codex account at a time, the Claude holders with the most time left to their roll, and Fable work to the chosen Fable accounts. Then the expiry burndown, the plan's step 6: before a reset kind expires, every holder becomes a chosen account, in order of expiry, from the moment §94.1's `expiryWindow` opens. The pool board does not show automatic resets yet. Switching a daemon to real mode, and turning the owner's condition off, stay the owner's calls.

- Knobs: `SUBROUTER_AUTO_RESET_WORTH` (on by default; `0` is §93's rule; any other value stops the daemon from starting). The constants are `resetNeedAt` (1.0), `resetNeedRisingAt` (1.1), `resetWindowMargin` (1.5), `resetWindowFloor` (24 hours) and `resetValueFloor` (0.25).
- Code: `internal/proxy/reset_worth.go` (`Pressure`, `pressureOf`, `poolWindowPressure`, `needOf`, `MeterNeed`, `meterNeed`, `pressureTrend`, `claimRateOf`, `claimValueOf`, `lastChance`, `meterDemand`, `expiryWindow`, `resetHoldings`, `withResetWindows`; §94.2: `resetPool`, `resetPoolOf`), `internal/proxy/usage_burn.go` (`UsagePoolMeter.Need`, `poolWindowOf`, the peer's holders in `readPeerBurn`), `internal/proxy/proxy.go` (usage-status through `withResetWindows`), `internal/accounts/reset_kinds.go` (`ClockRestartMeasuredNo`, `ClockRestart.Restarts`, `ResetMeter`, `ResetKindSpec.Meter`, `ResetKindNamed`, `HeldReset.ExpiryWindow`, `ResetExpiryWindow`), `ops/bin/srq` (`_need_line`); §94.2: `internal/proxy/auto_reset.go` (`SetAutoResetWorth`, `AutoResetWorth`, `AutoResetHeldBack`, `autoResetWorth`, `resetValueFloor`, `decideWorth`, `autoResetWorthFor`, `autoResetKind.tableKind`, `autoResetReading.roll`, `autoResetFoldsOlderSpenders`), `internal/proxy/auto_reset_claim.go` (the live check in `autoResetClaimClaude`), `internal/proxy/claude_reset.go` (`ClaudeResetRequest.liveCheck`), `internal/proxy/usage_history.go` (`claimAgainstTable`), `internal/proxy/codex_quota_reset_failover.go` (`failoverRedeems`), `internal/proxy/reset_autospend.go` (spend mode advising in real mode), `cmd/subrouter/main.go` (`autoResetWorthEnabled`, `autoResetFoldWarnings`, in place of `autoResetSpendsAlone`); §94.3: `internal/proxy/auto_reset.go` (`AutoResetDecision.Rule`, `AutoResetRule`, `AutoResetRuleStep`, `decideWorth`'s third result), `internal/proxy/auto_reset_view.go` (`AutoResetStatus.Worth`, `autoResetHealth`), `ops/bin/srq` (`_auto_reset_rule`, `_held_back_why`, `_auto_reset_label`, `_auto_reset_mode_line`)
- Tests: `internal/proxy/reset_worth_test.go`, `ops/docscheck/reset_worth_docs_test.go`, `srq selftest`, and the §91 goldens named above; §94.2: `internal/proxy/auto_reset_worth_test.go`, `cmd/subrouter/auto_reset_test.go`; §94.3: `internal/proxy/auto_reset_worth_view_test.go`, `internal/proxy/auto_reset_view_test.go` (the health field), `ops/docscheck/reset_worth_docs_test.go` (`TestDocsSayHowToReadWhatTheRuleUsed`), `srq selftest`
- Docs: `ops/OPERATING.md` (Read the pool's burn, Saved resets, Automatic resets, the settings table), `ops/skills/subrouter/SKILL.md`, `ops/KNOWN-ISSUES.md` (§56's residual), `docs/codex.md`
- Related: reads §92.2's figures and §91's lists, and settles §91's `restarts_clock` for the Claude week reset. §94.2 replaces §93.1's rule behind its own switch and leaves §93's moments, condition, claim in place and latch as they are; it extends §93.4's live check to Claude and lifts §93.2's refusal to start beside §29.5's failover redeem and the background spender. §94.3 extends §93.3's views: the row, the health route and `srq pool`.

## 95. The accounts chosen for a reset are loaded until they reach their limit, and the expiry burndown

The owner, 2026-11-22: "with resets in hand the aim is to load, not to balance. Pick which accounts will be reset and load work onto them. A little balancing is fine"; for Codex "pick one account, burn it to the ground, use its reset, then pick a new account"; Fable "works the same way: spend the Fable out from under the accounts that will get it back, by moving Fable work onto designated accounts"; and "when resets are running out of time, be aggressive: a reset burndown that starts before the weekly burndown would, about 24 hours before a reset expires, loading the holders hard so the resets get used". Running work is not moved off. §93 claims an account's next saved reset at its limit and §94 decides when a claim is worth it, but a reset is worth something only on an account that reaches its limit, and nothing sent work to the accounts that hold the resets: §56's 2x is a pace factor that a faster spender still beats, and every other term of the ranking balances. §95 is the plan's steps 5 and 6. §95.1 decides which accounts are chosen and in what order, logs each choice and publishes it, and changes no ranking. §95.2 makes real mode load the chosen accounts. Then the views, the documents and §95 as a whole.

§95.1 Which accounts are chosen, the expiry burndown's holders, the modes and what is published.

A chosen account is one this pool means to run to its limit so that its next saved reset is claimed there. Only the daemon that holds an account chooses it. A choice names its meter, `week` (a Claude week), `fable` (a Claude Fable meter) or `codex_week` (a Codex week); its order among this daemon's accounts chosen for that meter, 1 first; and one sentence that says why (`ResetChoice`).

Who is chosen. The background pass of §93.1 (`RunAutoReset`, every 10 seconds, now `autoResetPass`) weighs each of this daemon's own Claude and Codex OAuth accounts against the pool (`weigh`). An account is chosen when all of these hold:
1. It is this daemon's own (`autoResetOwnAccount`), its credential is alive (`autoResetDeadCredential`), and it has no owner's reserve (`SUBROUTER_RESERVED_ACCOUNTS`, read as the scheduler reads it, so in the last 24 hours before the roll, when §60 lifts the reserve, it counts as none).
2. Its next reset is of a kind the kinds table knows (§91.1), whose `restarts_clock` is known. The next reset is the one a claim at the limit spends (`nextHeldReset`): for a Claude account the grant Anthropic names next (`next_grant_id`) in the block the claim itself reads, so a week reset listed behind a grant of an unknown kind does not count; for a Codex account the credit that expires soonest, which the redeem takes first whatever its type. The meter is the one that reset is claimed on (`ResetKindSpec.Meter`): the week for a week reset or a Codex credit, the Fable meter for a Fable reset.
3. §94.1's need on that meter is `yes`, or the reset is inside its expiry window.
4. §94's rule would claim at the wall the account is heading for (`projectedWall`). That wall is now plus the account's headroom on the meter divided by the pool's demand on it, as if the whole demand went to it: §94.1's pressure over the longest window that has one, times the accounts that window counts, per week. A claim there must add at least `resetValueFloor` (0.25 of a week, `claimValueOf` at the wall, the rate as §94.2 reads it), or the reset must be in its last chance or its expiry window. A meter that is not read, a demand that is not known, or a roll that is not read leaves the value unknown, and only the expiry window can choose the account then.

How many, and in what order (`chooseResets`). Per meter:
- Claude week: every week-reset holder that qualifies, ordered by the value of a claim at its projected wall, highest first, so the holders with the most time left to their roll are loaded first. A tie goes to the longer time from the wall to the roll, then the label.
- Claude Fable: every account whose next reset is a Fable reset and that qualifies on the Fable meter, in the same order. An account whose next reset is a week reset is never chosen for Fable, whatever its Fable meter reads: that reset is claimed on the week, and the account is weighed on the week.
- Codex: one account at a time (`codexSingle`), the one whose claim at its projected wall adds the most. A choice stands for as long as the account qualifies, even when another account's claim comes to add more: in the owner's words, "pick one account, burn it to the ground, use its reset, then pick a new account". When its credit is spent (a reading shows none) or it stops qualifying, the one worth the most is chosen at once.

The expiry burndown (the plan's step 6). Inside a reset kind's expiry window (§94.1's `expiryWindow`: what the pool needs to spend the holders, times 1.5, never under 24 hours), every holder of that kind and date qualifies whatever the need. Within a meter the holders inside their window come first, ordered by expiry, then by value; then the accounts chosen on need, by value. For Codex this lifts one at a time: every Codex holder inside its window is chosen, beside the one account loaded one at a time, which is picked from every Codex account that qualifies and so may be one of them. A holder whose projected wall falls after its reset's expiry is still chosen, last in its meter's order, and its sentence ends `its <kind> is likely to expire unused: even with the pool's whole demand it reaches its limit at <time>, after the reset expires at <time>`.

What is published, by mode. The choice follows `SUBROUTER_AUTO_RESET`. Off decides nothing. Shadow decides, logs, and publishes the choice as `would_choose` on the account's `usage-status` row, beside `normalized`, which is byte for byte what it is without the choice. Real publishes it inside `normalized` as `reset_choice` (`withResetChoices`), where a peer already reads its holder's verdicts (§56's burndown arrives in `new_session` the same way). Both are `{"meter": ..., "order": ..., "why": ...}`, and only on the daemon's own rows. The choices live in memory (`autoResets.choices`) and are replaced whole at each pass; a restart chooses again at its first pass. Nothing ranks on a choice in §95.1, in any mode: §95.2 does that in real mode. A daemon from before §95 publishes none, and ignores `reset_choice` and `would_choose`, which its decoders do not know.

The switch. `SUBROUTER_RESET_LOADING`: unset or `1` is on, `0` means this daemon never chooses (and, from §95.2, ignores a peer's published choice too), and any other value stops `serve` from starting with a message that names it. `serve` logs a Warn when it is `0`. The package default, for a `Server` that `serve` did not set up, is off, so the tests of §91 to §94 see nothing new.

What is logged. One Info line when an account becomes chosen, `reset loading: chose an account to run to its limit for its next saved reset`, and one when it stops, `reset loading: an account is no longer chosen`, each with `account` (the label, §87), `provider`, `mode`, `meter`, `order` and `reason`. The stop line's meter and order are the ones it had, and its reason is why it no longer qualifies: `it holds no saved reset` once its reset is claimed, `its credential is dead: ...`, `the pool does not need its saved resets on the Claude week (...)`, or `Codex accounts are loaded one at a time, and another one is loaded now`. A choice that moves to another meter logs both. Nothing is logged while a choice stands, even when its order changes. In shadow mode the lines read `reset loading: would choose an account ...` and `reset loading: would no longer choose an account`.

The pool the choice and §94's decision read (part I). §94 reads each peer from its last cached usage body (`resetPoolOf`), and a body more than 15 minutes old is not counted (§92.2). So a daemon that routed no new session and served no `burn.json` or board for 15 minutes judged need, value and the expiry windows on its own accounts alone, and its own accounts are the ones at the wall: §94.1's and §94.2's residual. The background pass now reads a peer again when its last body is more than 5 minutes old (`resetPeerRefreshAge`), or when it has none, through the read every routing decision, `burn.json` and the peers view share (`usageForDecision`: the one cache the board's read writes too, one fetch in flight per peer, a failed fetch not retried inside 10 seconds), and only then reads the readings and chooses (`refreshStaleResetPeers`). Only the pass reads a peer: a turn at the wall decides on the cached bodies, and the `usage-status` handler reads no peer, so two daemons that read each other cannot set off each other's reads. With `SUBROUTER_AUTO_RESET=0` no pass runs and nothing is read. The refresh runs whatever `SUBROUTER_RESET_LOADING` says, because §94's decision reads the same pool. A peer that answers it restarts §67's grace, as every usage body does.

How to turn it off. `SUBROUTER_RESET_LOADING=0` and a restart chooses nothing; the refresh of part I stays while automatic resets are on. `SUBROUTER_AUTO_RESET=0` turns off all of §93, §94 and §95.

Tested in Docker, every provider a fake that fails the test on a claim or a redeem. In `internal/proxy/reset_choice_test.go`, on usage-status rows weighed against the pool built from them as the pass builds it:
- `TestAWeekHolderIsChosenWhenThePoolNeedsItsResets` (behaviour 1): a week holder with 30% of its week left, 100 hours to its roll, at pressure 1.2 over 4 accounts, reaches its limit in 10h30m and a claim there adds 0.64 of a week: chosen for the week, order 1. At 0.7 (need no, its window 27 hours before an expiry 17 days away) it is not chosen and the reason says so, and with no figure (need unknown) it is not either.
- `TestWeekHoldersAreOrderedByTheValueAtTheirWall` (2): holders with 150, 90 and 40 hours to their rolls and 10% left each, at 1.5 over 6 accounts, are ordered 1, 2, 3; one with 20 hours, whose claim would add 0.16, is not chosen, its reason naming the floor.
- `TestAFableHolderIsChosenForTheFableMeterAndAWeekHolderNeverIs` (3): a Fable holder in a pool whose Fable need is yes is chosen for `fable`; a week holder with its Fable meter spent is not chosen while the week's need is no, and is chosen for `week` when it is yes.
- `TestOneCodexAccountIsChosenAtATime` (4): of three Codex holders worth 0.77, 0.47 and 0.23 at their wall, only the first is chosen; with its credit spent the second is; when the second's value passes it, the first still stands at 29 minutes, 31 minutes and 6 hours, with the same hold, and with none worth more it stands after 31.
- `TestInsideTheExpiryWindowEveryHolderIsChosen` (5): four Codex holders at pressure 0.5 (need no) with credits ending in 10 and 20 hours are all chosen, the 10-hour one first, then the 20-hour ones by value, and the one with its whole week left, whose wall is 84 hours away, last, saying its reset is likely to expire unused. Two Claude week holders ending in 20 hours are both chosen by value. The same Codex pool with its credits 9 days away chooses none.
- `TestANextResetOfAnUnknownKindIsNeverChosen` (6, on facts), with the same account holding a week reset chosen.
In `internal/proxy/reset_choice_daemon_test.go`, over §94.2's Claude daemon (`worthClaude`, its usage history giving the pool's pressure) and §93.1's Codex daemon with a history seeded:
- `TestTheChoiceIsPublishedAsTheModeSays` (7): off publishes and logs nothing; shadow publishes `would_choose` (week, order 1) and logs one `would choose` line; real publishes `reset_choice` inside `normalized` and logs one `chose` line; with the switch at `0` each mode publishes and logs nothing; the account that holds no reset publishes nothing in any mode.
- `TestShadowLeavesNormalizedAsItWas` (7): in shadow mode each row's `normalized` marshals byte for byte as it was given, and in real mode it differs only by the choice, which equals shadow's `would_choose`.
- `TestAChoiceLogsOnceWhenItBeginsAndOnceWhenItStops` (8): three passes log one line, with the label, `claude`, the mode, `week`, order 1 and the reason; once the account's reset is used, two more passes log one stop line, `it holds no saved reset`. Shadow and real alike, each with its own words.
- `TestAPeersAccountIsNeverChosen`, `TestAnAccountWithADeadCredentialIsNeverChosen`, `TestAnAccountWithAnOwnersReserveIsNeverChosen` and `TestAnAccountWhoseNextResetIsOfAnUnknownKindIsNeverChosen` (6): each runs the same fixture without the exclusion first, which is chosen. A box's held account is chosen by the box; a laptop that reads the box's body chooses nothing for it, publishes nothing and logs nothing, and refuses each of the box's rows as a candidate while it takes its own. A Codex holder is chosen, and when its credential dies it stops with a line naming the dead credential. A reserve of 0.05 stops the choice. A Claude block whose next grant clears only `five_hour` is not chosen even with a week reset listed behind it, and the same grants with the week reset next are.
- `TestTheBackgroundPassReadsAStalePeerAndNothingElseDoes` (9): a peer whose cached body is 10 minutes old and says 0.6, with this daemon's holder at 0.6: reading `usage-status` and a pinned turn at the week's wall make no peer request (the turn goes through a handler built after the peer is set, since `Handler` copies the `Server`, and a new session's turn at the end does read the peer, which shows the turns can see it), a pass with the mode off makes none, and a choice made on the stale body chooses nothing. The next pass reads the peer once, the pool's pressure is 1.50, and the holder is chosen with that pressure in its reason. A pass on a body 4 minutes old reads nothing more.
- `TestAChoiceChangesNoRanking` (10) held that a pass that chooses changed no ranking in any mode, and that a peer ranked the rows published with `reset_choice` as it ranked them without it. §95.2 makes real mode rank on the choice and replaced it with `TestAChoiceRanksOnlyInRealMode`. §72's endgame simulation (`TestEndgameSimulation`) runs in the `selectacct` package, which §95.1 does not touch.
In `cmd/subrouter/auto_reset_test.go`, `TestServeSetsTheResetLoading`: the switch's values, `serve` setting it, the Warn when it is `0`, and the refusal naming it. §47's `TestUsageStatusWireTypeHasEveryDaemonField` takes the new row field (`remoteServerUsageStatus.WouldChoose`). The selection tests of §91 to §94 (`TestTheResetsListChangesNoSelection`, `TestAPeersResetsListChangesNoRanking`, `TestSelectionIsTheSameWithBurnPublished`), the `selectacct` package, and every test of §93 and §94 pass unchanged.

Each part was taken out of the working tree in turn, one replacement each, its test run in Docker, and the part put back, by a script outside the repository. Each went red for the predicted reason: need no and need unknown let through (the held account chosen at 0.7; the reason no longer says the burn is not known); the value order reversed (h150 ordered 3); no value floor (h20 chosen); the projected wall at now (the reason reads `in 0m`); the Fable choice named as the week; every Codex qualifier chosen; the hold dropped (c100 chosen at 29 minutes); the hold giving way after 30 minutes to an account worth more (c100 chosen at 31); the window not choosing (the Codex holders at need no not chosen); the window's expiry order dropped (x20c ahead of x10); the likely-unused holder not put last, and not named; an unknown kind taken for a week reset; the own-account check dropped (the laptop took the box's row); the dead credential and the reserve ignored; the next grant read from the list's order (the unknown grant behind the week reset chosen); shadow publishing inside `normalized`, and real mode publishing beside it; the switch and the mode ignored by the pass (lines and choices with loading off and in mode off); a line on every pass; no stop line; the pass not reading the stale peer (no request, nothing chosen); a pass reading a body 4 minutes old; a pass reading a peer with the mode off; `usage-status` reading a peer; a choice that sets `new_session.burndown` on its account (the peer's candidates changed); and `serve` never setting the switch. The same runs with nothing taken out passed. Three first attempts did not go red. The holder likely to expire unused was last by its value anyway, so the fixture now gives it a higher value than another holder of its date. A choice leaking into `new_session` was invisible to a test that compared a body with the choice against the same body with the field deleted, so the test now publishes the same rows with the choice and without it. And a mode check inside the choosing function, under the pass's own, could not be seen at all: it was dead, and is gone. An independent check then found a fourth: a peer read added at the wall stayed green, because the test's handler was built before the test set the peer, so the turn at the wall had no peer to read. The test now builds its handler after the peer, and the same mutation fails it (`a turn at the wall made 1 peer requests`). Its new calibration, a new session's turn that reads the peer, fails on the old handler with nothing else changed.

Readings of the design, each with its reason. A Codex choice never gives way to an account whose claim adds more: the design keeps a choice "until the account's credit is spent or the choice stops holding", and the owner's rule is to burn one account to the ground before picking the next. The first build let a choice give way after 30 minutes to an account worth strictly more. An independent check found that this leaves a half-spent account behind: an account that has just rolled into a fresh week has more time from its wall to its roll, so a claim there is usually worth more. The 30-minute least time went with it, since a choice that stands until it stops qualifying cannot flap between accounts. Inside a meter the window's holders come before the accounts chosen on need, since theirs is the budget lost at a date. The order is per meter and per daemon: each daemon numbers its own accounts from 1, and §95.2 reads a peer's numbers beside this daemon's.

Residuals. A choice is made on the last sweep's rows and the last pass's pool, at most a pass old. The projected wall assumes the whole demand goes to the account; while several accounts are chosen, each reaches its limit later than projected, which only lowers what is measured as the claim's value, never chooses an account that the rule would not claim on. A Claude account's next grant is read from the cached block; when the account has no block read yet, the list's first entry stands in for it. The Fable resets that queue behind a week reset are not weighed until the week reset is used, as §94.1's window does not count them. A Codex choice that stops qualifying is forgotten, so when the pool's need on the Codex week turns `no` and later `yes` again, the account worth the most is chosen afresh, which may not be the one half spent before. Nothing here was run against the live pool; the daemons stay in shadow mode, where the choices show in the log and on `usage-status` only.

- Knobs: `SUBROUTER_RESET_LOADING` (on by default; `0` chooses none; any value other than unset, `1` or `0` stops the daemon from starting). The constant is `resetPeerRefreshAge` (5 minutes); §94's `resetValueFloor` (0.25) and `expiryWindow`, and §93.1's `autoResetScanInterval` (10 seconds), are read as they are.
- Code: `internal/proxy/reset_choice.go` (`SetResetLoading`, `ResetLoading`, `resetCandidate`, `resetWeighing`, `projectedWall`, `resetPool.weigh`, `chooseResets`, `codexSingle`, `compareChosen`, `compareValue`, `chooseResetAccounts`, `resetCandidateOf`, `nextHeldReset`, `logResetChoices`, `withResetChoices`, `refreshStaleResetPeers`, `autoResetPass`), `internal/proxy/auto_reset.go` (`autoResets.choices`, `autoResets.codexHold`, `RunAutoReset` running `autoResetPass`), `internal/proxy/proxy.go` (`AccountUsageStatus.WouldChoose`, `NormalizedUsage.ResetChoice`, the usage-status chain through `withResetChoices`), `cmd/subrouter/main.go` (`resetLoadingEnabled`, the Warn), `cmd/subrouter/sr_server.go` (`remoteServerUsageStatus.WouldChoose`); `ResetChoice` and `ResetChoiceMeter` were here and moved to `selectacct/reset_choice.go` in §95.2
- Tests: `internal/proxy/reset_choice_test.go`, `internal/proxy/reset_choice_daemon_test.go`, `cmd/subrouter/auto_reset_test.go` (`TestServeSetsTheResetLoading`), `ops/docscheck/reset_choice_docs_test.go`, `ops/skills/skill_test.go`
- Docs: `ops/OPERATING.md` (Accounts chosen for a reset, the settings table, Saved resets, Read the pool's burn), `ops/skills/subrouter/SKILL.md`
- Related: reads §94.1's need, pressure, value, last chance and expiry window, §91.1's kinds table and lists, §93.1's pass, own-account and dead-credential checks, §60's reserve release and §87's labels; corrects §94.1's and §94.2's residual on the peers a decision reads; adds one row field to §13's usage-status outside `normalized`, and one to `normalized` that §95.2 ranks on.

§95.2 What a choice does to ranking.

In real mode a published choice moves new sessions, and nothing else. The daemon that holds an account ranks on its own choices (`rankedResetChoices`, which `selectionSchedulerFrom` attaches with `Scheduler.WithResetChoices`, so routing, the verdicts and the board read the same ones), and every daemon ranks on a peer's `normalized.reset_choice` (`remotePoolNormalized.ResetChoice`, carried onto the peer's score; a pool entry takes its account's, `inheritPeerAccountFacts`). A choice in shadow mode is `would_choose`, and nothing ranks on it. A daemon in shadow or off mode still honours a choice a peer published in real mode, because the holder decides for its accounts. With `SUBROUTER_RESET_LOADING=0` a daemon ranks on no choice, its own or a peer's. A choice with a meter this build does not know, or an order under 1, is ignored (`ResetChoice.Known`). The choice's types moved to `selectacct/reset_choice.go` (`ResetChoice`, `ResetChoiceMeter`), which ranks on them; the wire is unchanged.

Which pools. A `week` or `codex_week` choice loads every model pool of the account. A `fable` choice loads the Fable pool only (`Scheduler.loads`; `ForModel` marks the Fable pool): Opus, Sonnet and Haiku work is ranked on that account as on any steady account.

Where a chosen account ranks (`compare`, in the routing order). A chosen account that can take a new session (`loadEligible`) ranks above every steady account. The drain stays above it: an account in its own last 24 hours before its roll, whose budget is lost at the roll, and a holder in §56's early burndown that is not chosen. An unstarted week (§86) stays above both, and so does, where machine policy is on, an account this machine is already bound to. A chosen account that is shedding the model's requests (a capacity mark, which lasts minutes) ranks after the accounts that are not, as every account does. Among chosen accounts (`compareChosen`): on the Fable pool a Fable choice goes before a week choice, since only Fable work spends a Fable meter; then the holder's order, 1 first, so the next account takes new sessions only when the ones before it carry their allowance; then the longer time to the roll; then the account ID. The local-over-peer factor, the live-session spread and the live debit do not enter, so a peer's order 1 goes before this daemon's order 2. A chosen account at its allowance (`loadAtCap`) ranks below every other routable account, as a draining account at its cap does. `Explain` and `pool.json` name each clause: `chosen to run to its limit for its next saved reset`, `both chosen for a saved reset; this one is first in its holder's order`, `both chosen for a saved reset; this one for its Fable meter, which only Fable work spends`, and `the other is chosen for a saved reset but already carries its allowance`.

The allowance. A chosen account's allowance is the paced ceiling (`SUBROUTER_PACED_SESSION_CEILING`, 20 by default) in place of the taper (`Score.allowance`). The brakes stay: a 5h window projected not to sustain more sessions holds it at 1, and a 429 that is not a quota answer holds it at 4 for 15 minutes. Its verdict publishes that allowance in `allowed_sessions` and says `at_cap` once it carries it, so a peer stops placing sessions there when the holder does.

The floors. The 5% short floor (for a Codex weekly-only account, 5% of its week) does not stop a chosen account: it takes new sessions down to its limit. Its verdict is `eligible` with the reason `chosen` (`VerdictChosen`) where it would say `short_floor` or `burn_projection`. Exhaustion, the owner's holdout, a dead credential (which the scoring sweep scores zero, so it is exhausted) and a closed admission queue still refuse it. A placement on a chosen account below the floor no longer logs `selected OAuth account below new-session headroom; routing optimistically` (`Scheduler.LoadsForReset`): it is on purpose.

§56's early burndown. A chosen account is not in it (`inResetBurndown`); it is loaded in its holder's order instead. In the drain the account whose roll comes first leads, which would turn that order round: the holders loaded first are the ones with the most time to their roll. In its own last 24 hours a chosen account drains as any account does, with its allowance.

§65. An Opus or Sonnet turn ranks an account chosen for its Fable meter without the Fable-spent factor (`rankingPace`). The factor sends Opus work to accounts whose Fable is spent; on this one that work would eat the week under its Fable reset. Every account not chosen for Fable keeps the factor, `SUBROUTER_FABLE_SPENT_PREFERENCE` keeps its default, and §71 is unchanged.

Sessions that have an account. Nothing here moves one. A warm session moves only when its account is evicted (§20), and a choice evicts nothing. A stale session (idle 70 minutes, its prompt cache gone, §20's P9) re-ranks as a new session, as before, and so does a session a forced switch moves. `PickSteady`, sr's interactive switch, never parks a session on a chosen account: it ranks one last, with `the other is chosen to run to its limit for a saved reset; not a place to park a session`, as it ranks an account in its burndown window.

What did not change. With no choice ranked (any mode but real, the loading switch at `0`, or no account chosen), every ranking, pick and verdict is what it was: each clause added reads a choice, and none is set. In real mode an account that is not chosen, in a pool with no chosen account, ranks as before.

How to turn it off. `SUBROUTER_AUTO_RESET=shadow` (the default) keeps this daemon's own choices out of ranking, while it honours a peer's real-mode choice. `SUBROUTER_RESET_LOADING=0` and a restart makes the daemon choose nothing and rank on no choice.

What it buys, measured. `TestALoadedCodexAccountReachesItsLimitEarlierInItsWeek` runs a Codex pool through the real comparator: four weekly-only accounts part-way through their weeks; a new session every 40 minutes, two hours long, spending 1% of a week an hour while it runs, about 1.26 weeks of demand for each week the pool holds; a session keeps its account until the account is spent. The account §95.1 would choose, c1, starts 8 hours into its week with 90% left. With loading off it reaches its limit 109.3 hours into its week. Chosen, it reaches it 38.7 hours in, 70.6 hours earlier: about 3 live sessions spend 0.03 of a week an hour, and 90% lasts 30 hours. `./deploy/docker/test.sh ./selectacct -run TestALoadedCodexAccountReachesItsLimitEarlierInItsWeek -v` prints both. §72's endgame simulation (`TestEndgameSimulation`) builds no choice, and its numbers are unchanged.

Tested in Docker. In `selectacct/reset_choice_test.go`, on scores built as the proxy builds them:
- `TestAChosenAccountTakesTheNewSessionAheadOfBetterPace` (behaviour 1): a chosen account with half its week left and 150 hours to its roll takes the new session from two steady accounts with a better pace, for `chosen to run to its limit ...`; an account 12 hours from its roll and a holder in early burndown rank above it; in steady mode a chosen account that would win on pace ranks below a steady one.
- `TestChosenAccountsAreLoadedInTheirOrder` (2): order 1 goes before order 2, which has the better pace, idle and with 19 live sessions and its week debited by 40 requests; at 20 live the second goes first and the first ranks below the steady account it would beat on pace; a peer's order 1 goes before a local order 2, a peer's order 1 with 40 hours to its roll before a local order 1 with 30, and at its allowance the peer's gives way.
- `TestAChosenAccountsAllowanceIsThePacedCeiling` (3): with 3% of its week left, unchosen its taper is 1; chosen it takes a twentieth live session (verdict `chosen`, allowed 20, out of early burndown) and refuses a twenty-first (`at_cap`); a 5h window projected to run dry holds it at 1, and backpressure at 4.
- `TestTheFloorsDoNotStopAChosenAccount` (4): a Codex weekly-only account with 2% left is refused at the short floor unchosen and takes the session chosen; exhausted or with its queue closed it takes none.
- `TestTheFableSpentPreferenceSkipsAnAccountChosenForFable` (5): an Opus turn ranks a Fable-spent account at 2x over an untouched one with 57% left; chosen for Fable, it ranks at 1x and the untouched one wins.
- `TestAFableChoiceLoadsOnlyTheFablePool` (6): a Fable choice puts its account first on the Fable pool and nowhere else; a week choice on every pool; on the Fable pool a Fable choice goes before a week choice.
- `TestTheRankingWithChoicesIsAStrictWeakOrdering`: 200 random pools of chosen, capped, draining, peer and steady accounts on two pools; the comparator is irreflexive, asymmetric and transitive, and every order of the candidates ranks the same.
- `selectacct/reset_load_sim_test.go`, `TestALoadedCodexAccountReachesItsLimitEarlierInItsWeek` (10), above.
In `internal/proxy/reset_loading_test.go`, through the daemon with fake providers that fail the test on a claim or a redeem:
- `TestAWarmSessionNeverMovesForAChoice` (7): a session on a steady account stays there when another account becomes chosen, while a new session goes to the chosen one; that session stays when the choice ends, and the next new session goes back to the steady account.
- `TestAPeersChoiceIsHonouredAsALocalOne` (8): the box ranks its chosen account first for `chosen to run to its limit ...`; a laptop whose own account would win on pace and the local 2x places the new session on the box's chosen account for the same clause in off, shadow and real mode; a body without `reset_choice` and the switch at `0` leave the laptop's own account first.
- `TestAChosenCodexAccountBelowTheFloorTakesANewSession` (4): a Codex account with 2% left takes a new session once chosen, with no `routing optimistically` line, and none once its credential is dead and scored zero.
- `TestAChoiceRanksOnlyInRealMode` (9): off, shadow, and real with the switch at `0` rank and publish verdicts the same before and after a pass that chooses; real mode with the switch on puts the chosen account first. It replaces §95.1's `TestAChoiceChangesNoRanking`.
- `TestRankingReadsTheChoicesWhileAPassReplacesThem`: four goroutines build the selection scheduler, rank and compute the verdicts while 20 passes choose; under `-race` it finds nothing, and the chosen account ranks first after.
In `ops/docscheck/reset_choice_docs_test.go`, `TestDocsSayWhatAChoiceDoesToRouting`: the operating guide and the skill name §95.2, the ceiling, that a session never moves for a choice and the Fable-spent preference, and no doc but this one says nothing ranks on a choice. The selection tests of §91 to §94, §65's and §71's tests, `TestEndgameSimulation` and the whole `selectacct`, `internal/proxy` and `cmd/subrouter` packages pass unchanged, and the selection tests pass under `-race`.

Each change was taken out of the working tree in turn, one replacement each, its test run in Docker, and the change put back, by a script outside the repository. Each went red for the predicted reason: the chosen tier removed (the faster steady account picked, the second chosen account picked on pace, the 2% Codex account refused); the chosen clause put above the drain (the chosen account ahead of the last-day account); the holder's order reversed, a spread term or a local preference consulted between chosen accounts, and the roll tie-break reversed (each picked the wrong one of the two); the Fable choice not put first on the Fable pool; a chosen account at its allowance not demoted (it beat the steady account on pace); the taper kept for a chosen account (refused at 19 live); the 5h brake and the backpressure cap skipped (allowed 20); the short floor applied to a chosen account, and a chosen account not routable (the 2% Codex account refused); an exhausted chosen account taken; §56's early burndown kept for a chosen account (the reason named early burndown); steady mode not demoting a chosen account; §65's factor kept on an account chosen for Fable; a Fable choice loading every pool, and the Fable pool not marked; the verdict without `at_cap`, without `chosen`, and without the allowance; a peer's score not loaded, and this daemon's table not read; the choices not attached where selection is built (the new session went to spare, the box ranked held on pace, the Codex account was refused); shadow mode ranking on its own choices; a peer's choice dropped, honoured with the switch at `0`, taken at order 0, and not carried to the Fable pool entry; the `routing optimistically` line logged for a chosen account; and the choices read without the lock (`-race` found three data races). The same runs with nothing taken out passed. One first attempt did not compile: dropping the Fable clause left a variable unused, and a condition that never holds replaced it. Two guards were found dead and removed before the run: a closed queue and the reserve gate inside `loadEligible` (a closed queue refuses the account before ranking reaches it, and §95.1 never chooses a reserved account), and the loading switch inside `rankedResetChoices` (with it at `0` the pass chooses nothing).

Readings of the design, each with its reason. A chosen account leaves §56's early burndown, for the order above. sr's steady pick ranks a chosen account last, as it ranks the early burndown that a chosen account leaves. On the Fable pool a Fable choice goes before a week choice, since the week choice is also loaded by every other pool. Between two chosen accounts at the same place in two holders' orders, the longer time to the roll goes first, the rule each holder orders its own by. A chosen account at its allowance is demoted like a draining one at its cap, so "refuses a twenty-first" holds wherever another account can take the session. The owner's holdout still refuses a chosen account, though §95.1 never chooses an account with a reserve; the softer reserve gate on the debited week is not read for one.

Residuals. Views (`srq pool`, `srq resets`, the board) do not yet mark a chosen account beyond the ranking's `reason`; the `paced allowance` log line still reports only accounts in their burndown window. A peer's refugee cap (§72) is unchanged for a chosen account. Nothing here was run against the live pool, whose daemons stay in shadow mode.

- Knobs: none new. `SUBROUTER_RESET_LOADING=0` ranks on no choice; `SUBROUTER_AUTO_RESET` other than real keeps this daemon's own out; `SUBROUTER_PACED_SESSION_CEILING` is a chosen account's allowance.
- Code: `selectacct/reset_choice.go` (`ResetChoice`, `ResetChoiceMeter`, `ResetChoice.Known`, `Scheduler.WithResetChoices`, `Scheduler.LoadsForReset`, `Scheduler.loads`, `Score.loadEligible`, `Score.loadAtCap`, `compareChosen`), `selectacct/scheduler.go` (`Score.ResetChoice`, `Score.loading`, `Scheduler.fablePool`, `measuredScore`, `rankingPace`, `routableForNewSession`, `inResetBurndown`), `selectacct/explain.go` (`compare`), `selectacct/pace.go` (`allowance`), `selectacct/new_session_verdict.go` (`VerdictChosen`, `at_cap`, `allowed_sessions`), `internal/proxy/reset_choice.go` (`rankedResetChoices`), `internal/proxy/proxy.go` (`selectionSchedulerFrom`, the optimistic-routing line), `internal/proxy/remote_pool.go` (`remotePoolNormalized.ResetChoice`, `score`), `internal/proxy/remote_pool_decide.go` (`inheritPeerAccountFacts`)
- Tests: `selectacct/reset_choice_test.go`, `selectacct/reset_load_sim_test.go`, `internal/proxy/reset_loading_test.go`, `ops/docscheck/reset_choice_docs_test.go`
- Docs: `ops/OPERATING.md` (Accounts chosen for a reset, the settings table), `ops/skills/subrouter/SKILL.md`
- Related: ranks on §95.1's choices; sits below §86's unstarted week and the drain of §41, §56 and §60, takes §72's ceiling and brakes, lifts the short floor (`MinShortHeadroomFloor`) for a chosen account and keeps §20's warm rule, §55's measured inputs and §65's factor for every account not chosen for Fable; §71 unchanged.

§95.3 The views, the documents, and §95 as a whole.

pool.json. Each row carries the account's choice as its holder published it (`poolRow.ResetChoice`, `poolRow.WouldChoose`): `reset_choice`, from inside `normalized`, where real mode publishes it, or `would_choose`, from beside it, where shadow mode does. Both are `{"meter": ..., "order": ..., "why": ...}`. This daemon's own rows now go through `withResetChoices`, as its usage-status rows do, so they carry this daemon's own choice by its mode, and none with the mode off or `SUBROUTER_RESET_LOADING=0`. A peer's rows carry what the peer published. A choice this build cannot rank on (`ResetChoice.Known`: a meter it does not know, or an order under 1) is left out, so the board never marks a choice that ranks nowhere here. Nothing ranks on these fields: the board's `rank` and `reason` come from the scheduler that selection builds, as §95.2 left them.

The board. A chosen account's row shows, after its reset marks (in a Claude row's pace cell and a Codex row's fuse cell), `chosen week #1`: the meter, as `week`, `Fable`, or `week` for a Codex week, and the account's place in its holder's order for that meter. Shadow mode reads `would choose week #1`, in grey. The mark's title is the choice's `why`, and in shadow mode it starts `shadow mode, nothing ranks on it:`. The legend explains both marks (`choiceMark`).

srq pool. An account's line ends with the same words, `chosen week #1` or `would choose week #1`, in this daemon's section and in a peer's, on a line that names the account by its label (§87). The verdict `chosen` (§95.2) now shows the account's sessions in flight of its allowance, `new=yes (chosen 3/20)`, as `burndown` and `at_cap` do (§53), and the `n/m` legend line then ends `or chosen for a saved reset`. One legend line explains the marks. In this daemon's section it says that new sessions go there first, #1 first per meter, up to the paced ceiling and below the 5% floor, and that a would-be choice ranks nothing. In a peer's section it says that a new session from here goes there first unless `SUBROUTER_RESET_LOADING=0` here, which srq cannot read. A section with no choice prints no mark and no legend line, so a daemon from before §95 reads as before. `srq pool --json` adds `reset_choice_mode` (`real` or `shadow`), `reset_choice_meter`, `reset_choice_order` and `reset_choice_why`. A malformed choice, a meter srq does not know, or an order under 1 reads as none (`_reset_choice`).

srq resets. Where a daemon publishes §94.1's `expiry_window` on an entry, a `window` column after the expiry says when the expiry burndown of that kind and date opens, in this machine's time: `opens 12/8 10:00 in 15d 2h`, or `OPEN since 12/8 10:00` once it has opened (`_window_words`). An entry with none reads `-`: a kind the daemon does not know, no date, or a daemon from before §94. A line above the table says what the column is. Each window is the one the holder's daemon computed from the pool it reads (§94.1, with §95.1's part I). A pool whose daemons publish no window prints no column, as before. `srq resets --json` carries `expiry_window` on each entry. This closes §94.1's "`srq` does not show the window yet" and §95.2's residual on the views.

The documents. `ops/OPERATING.md`'s "Accounts chosen for a reset" now says in one place when the expiry burndown starts and how its lead is found, with the owner's words. It shows how to read the choices in `srq pool`, `srq resets`, the board and pool.json. "To go back" now names the three settings once, each with what it keeps: shadow mode keeps this daemon's own choices out of routing and still honours a peer's; `SUBROUTER_RESET_LOADING=0` makes and ranks on no choice; automatic resets off claim and choose nothing. Its `srq` table names the choice and the window, and Saved resets points `expiry_window` at `srq resets`. `ops/skills/subrouter/SKILL.md` says the same in short, and its "Explain why the router picked an account" names the chosen clause. `ops/KNOWN-ISSUES.md` has "Residual gaps §95 leaves open".

What did not change. These are views. No ranking, pick, verdict, claim or log line changed, in any mode. The manual commands (`sr claude reset`, `sr codex reset`), §93's modes and §94's rule behave as before, and their tests did not change.

Tested in Docker, every provider a fake that fails the test on a claim or a redeem. In `internal/proxy/reset_choice_view_test.go`, over §95.1's daemons:
- `TestPoolJSONCarriesEachRowsResetChoice` (behaviour 4): a box in real mode chooses its held, and a laptop in shadow mode that reads the box would choose its own mine. The laptop's pool.json carries mine's `would_choose` and the box's held's `reset_choice`, each equal to what its daemon's usage-status publishes, and nothing on the two accounts that no daemon chose. In real mode mine carries `reset_choice`. With the mode off, and with `SUBROUTER_RESET_LOADING=0`, mine carries none, and held keeps the box's. A box choice with a meter this build does not know is not carried.
- `TestTheBoardPageMarksAChosenAccount` (3): the page the daemon serves, run in node on the pool.json it serves (§90's runner), marks mine `would choose week #1` and held `chosen week #1`, each title holding its reason. It marks neither unchosen account, and in real mode it marks mine `chosen week #1`.
In `internal/proxy/pool_ui_page_test.mjs`, in node: the mark after a Claude row's reset marks and after a Codex row's credits, a choice for the Fable meter, shadow mode's words with an escaped title, no mark for an account no daemon chose, and both legend entries.
`srq selftest` (1 and 2): a chosen account by its label with `new=yes (chosen 3/20)  chosen week #1`; the next in the order at its allowance, `at_cap 20/20  chosen week #2`; `chosen Fable #1`; no mark on an unchosen account; `would choose week #1` on a Claude and a Codex row in shadow mode; each legend, a peer's included; no mark and no legend in a pool with no choice; the JSON fields; malformed choices. Then `srq resets` with one window open and one ahead: `OPEN since <time>`, `opens <time> in 14d 3h`, `-` for an entry with none, the column's head and its line, `--json`, no column for a pool with no window, and malformed windows.
In `ops/docscheck/reset_choice_docs_test.go`, `TestDocsSayHowToSeeTheChoicesAndGoBack` (5): the operator doc and the skill name the `srq pool` marks in both modes, `chosen 3/20`, `reset_choice_mode`, `OPEN since`, the expiry burndown, its 1.5 times and `SUBROUTER_AUTO_RESET=shadow`. `ops/skills/skill_test.go`'s `TestSubrouterSkillNamesEverySavedResetSetting` and `TestSubrouterSkillNamesOnlyWhatExists` pass on the new skill text.
Behaviour 6. The whole `internal/proxy`, `cmd/subrouter` and `selectacct` packages pass after the last code edit. That includes §91.2's `TestThePoolBoardCarriesEveryResetOfThisDaemonAndItsPeer` and `TestTheBoardPageMarksEachResetKindWithItsSoonestExpiry`, §93.3's `TestManualResetsAreTheSameInEveryMode`, §94.3's `TestManualResetsAreTheSameWithTheRuleOn`, every test of §93, §94, §95.1 and §95.2, and `TestEndgameSimulation`. Every earlier `srq selftest` check passes unchanged.

Each part was taken out of the working tree in turn, one replacement each, its test run in Docker, and the part put back, by a script outside the repository; `srq` ran from a mutated copy. Each went red for the predicted reason: this daemon's rows not put through `withResetChoices` (mine carried nothing); `reset_choice` not carried (held carried nothing); `would_choose` not carried; a meter this build does not know carried (held carried `month`); no mark on a Claude row, and none on a Codex row (the node test); shadow mode reading `chosen`, and drawn with the `chosen` class; a title without the reason; and the legend entry dropped. In `srq`: the choice not read (the selftest stopped on the missing field); shadow mode reading `chosen`; no legend line; the peer's legend in this daemon's words; `chosen` without its n/m, and the n/m legend without it; any meter, and any order, read as a choice; the window cell always `-`; open and ahead swapped; the column printed for a pool with no window; a malformed window kept; and no line that says what the column is. In the docs check, the operator doc and the skill as they were before §95.3, mounted over the tree in the container, lacked 12 names between them. The same runs with nothing taken out passed.

Residuals. The board has a panel for the first peer only (§31's D0), so it marks only that peer's choices; `srq pool` reads every configured peer. `sr status` shows no choice. The `paced allowance` log line still reports only accounts in their burndown window, and a peer's refugee cap (§72) is unchanged for a chosen account (§95.2).

- Knobs: none new.
- Code: `internal/proxy/pool_ui.go` (`poolRow.ResetChoice`, `poolRow.WouldChoose`, `applyPoolReadings`, this daemon's rows through `withResetChoices` in `poolBoard`), `internal/proxy/pool_ui.html` (`choiceMark`, `CHOICE_METERS`, the legend), `ops/bin/srq` (`RESET_CHOICE_METERS`, `_reset_choice`, `_reset_choice_label`, `SLOT_REASONS`, `_render_sections`, `_expiry_window`, `_window_words`, `_render_resets`)
- Tests: `internal/proxy/reset_choice_view_test.go`, `internal/proxy/pool_ui_page_test.mjs`, `ops/bin/srq selftest`, `ops/docscheck/reset_choice_docs_test.go` (`TestDocsSayHowToSeeTheChoicesAndGoBack`), `ops/skills/skill_test.go`
- Docs: `ops/OPERATING.md` (Use srq, Saved resets, Accounts chosen for a reset), `ops/skills/subrouter/SKILL.md`, `ops/KNOWN-ISSUES.md`
- Related: shows §95.1's choices and §94.1's expiry windows; extends §91.2's pool.json, board and `srq resets`, §53's `n/m`, and §87's labels.

§95 as a whole.

What it does. A saved reset adds work only when an account reaches its limit, so the daemon that holds an account now picks which of its accounts the pool will run to their limit and claim there. In real mode new sessions go to those accounts first, one after another in the holder's order, with their session caps lifted, until each reaches its limit; §93 then claims its next reset in place, behind the owner's under-1% condition and §94's rule, and the account is loaded again or the next one is. Claude week-reset holders are loaded with the most time left to their roll first, an account whose next reset is a Fable reset takes Fable work first, and Codex accounts are loaded one at a time. Before a kind of reset expires, from the moment its expiry window opens, every holder of it is loaded, the soonest expiry first: the expiry burndown. Running work never moves.

Why. The owner, 2026-11-22: "with resets in hand the aim is to load, not to balance. Pick which accounts will be reset and load work onto them. A little balancing is fine"; for Codex "pick one account, burn it to the ground, use its reset, then pick a new account. Be more aggressive about this than for Claude"; "Fable works the same way: spend the Fable out from under the accounts that will get it back, by moving Fable work onto designated accounts"; "when resets are running out of time, be aggressive: a reset burndown that starts before the weekly burndown would, about 24 hours before a reset expires, loading the holders hard so the resets get used"; and "running work is not moved off". Before §95 every term of the ranking balanced, and §56's 2x was a pace factor a faster spender still beat, so a holder reached its limit late in its week or not at all. On the Codex fixture of §95.2, the chosen account reaches its limit 38.7 hours into its week instead of 109.3.

Each number and where it is named. The 5-minute age at which the background pass reads a peer again (`resetPeerRefreshAge`) is in `internal/proxy/reset_choice.go`; the pass runs every 10 seconds (`autoResetScanInterval`, `cmd/subrouter/main.go`). A choice needs a claim at the projected wall to add a quarter of a week (`resetValueFloor`, `internal/proxy/auto_reset.go`). The expiry window opens 1.5 times the time the pool needs before the expiry (`resetWindowMargin`) and never less than 24 hours before (`resetWindowFloor`), both in `internal/proxy/reset_worth.go`; the 24 hours are the owner's. A chosen account's allowance is `SUBROUTER_PACED_SESSION_CEILING` (`DefaultPacedSessionCeiling`, 20, `selectacct/pace.go`), held at 4 for 15 minutes after a 429 that is not a quota answer (`claudeEndgameMaxSessions`, `paceBackpressureWindow`) and at 1 by the 5h brake. The short floor it passes is `MinShortHeadroomFloor` (5%, `selectacct/scheduler.go`). The drain above it is the last 24 hours before a roll (§41) and §56's early burndown below 25% of the week.

The switch and how to go back. `SUBROUTER_RESET_LOADING`: unset or `1` is on, `0` makes the daemon choose nothing and rank on no choice, its own or a peer's, and any other value stops `serve`. Loading follows `SUBROUTER_AUTO_RESET`: only real mode publishes a choice that ranks. Going back is shadow mode (the default), which keeps this daemon's own choices out of routing, `SUBROUTER_RESET_LOADING=0`, or automatic resets off, each with a restart. A daemon in shadow or off mode still honours a choice a peer publishes in real mode, because the holder decides for its accounts. A session already on a chosen account stays there.

What is shown in shadow mode. The daemon chooses, logs `reset loading: would choose ...` once when a choice begins and `would no longer choose` once when it ends, and publishes `would_choose` beside `normalized`. `srq pool` and the board read it as `would choose week #1`, and `srq resets` shows when each expiry burndown would open. Nothing ranks on it. The daemons stay in shadow mode until the owner switches one to real mode.

How it was tested. Every test runs in Docker against fakes, and none sends a request to a real provider or claims a real reset. §95.1's tests prove who is chosen and in what order, the expiry burndown, the modes, the log lines and the peer read. §95.2's prove the ranking, the allowance, the floors, the Fable rule, the warm rule and that nothing changes outside real mode, with a strict-weak-ordering property test, a race test and the Codex simulation. §95.3's prove the views and the documents. Each part was taken out in turn, and its test went red.

Assumed, and not measured. The projected wall assumes the pool's whole demand goes to one account; while several are chosen each reaches its limit later than projected. That a Codex redeem restarts the seven days (`assumed_yes`) and that a Fable reset keeps the roll (`assumed_no`) are still assumptions, and what a claim adds, so who is chosen and in what order, rests on them. Whether a claim is accepted before the limit is not measured. How fast a chosen account reaches its limit under the live pool's demand is not measured: nothing here ran against the live pool.

What is left of the plan. Switching a daemon to real mode is the owner's call, and so is turning the owner's under-1% condition off. The plan's step 7 asked that the board show each account's last automatic decision. It shows the resets and the choice, but not the decision, which `srq pool` and usage-status carry. The cap on claims and the list of excluded accounts are not built: the owner asked for neither.

## 96. Steadier choices before real mode: need holds near its bar, a choice keeps its place through a short gap, an account whose claim has not settled is not chosen

§93 to §95 run in shadow mode on every daemon, and the owner switches a daemon to real mode when the choices it would make look steady. The first hours of shadow mode on a live pool (2026-11-23) and the two independent checks of §94 and §95 showed three places where real mode would act unsteadily. Within one minute three daemons read the Claude week's pressure as 0.98, 1.02 and 1.04, so one said the pool does not need its resets and two said it does: a need that sits at its bar turns the choices on and off with every reading. A Codex choice that stopped qualifying for one pass was forgotten (§95.1's residual), so a flicker in need could swap the account being burnt down for a fresh one and leave the first half spent. And an account whose automatic claim was sent and never settled stayed chosen while §93.2's latch forbade another claim on it, so no other Codex account was loaded in its place (the second check's note on §95.1). §96 settles the three and corrects what the documents say `SUBROUTER_AUTO_RESET_WORTH=0` undoes. The daemons stay in shadow mode: nothing here makes the default claim anything, and with no choice published every ranking, pick and verdict is what it was.

A. Need holds its answer near the bar. Per meter, on each daemon, need turns `yes` as §94.1 rules it: the 24-hour pressure at or above 1.0 (`resetNeedAt`), or the 6-hour pressure plus its trend at or above 1.1 (`resetNeedRisingAt`), or, without a 24-hour figure, the 6-hour and then the 1-hour pressure at 1.0. Once `yes`, it stays `yes` until the pressure that decides it falls under 0.9 (`resetNeedHoldAt`): the 24-hour pressure, or the 6-hour pressure plus its trend, at or above 0.9 keeps it; without a 24-hour figure, the window that decides need. "All at the limit" is `yes` as before. A meter with no figure at all keeps an earlier `yes` for up to 30 minutes from the last reading that answered `yes` on a figure (`resetNeedUnknownHold`), then reads `unknown`, and the `yes` is gone. §94.1's pure function `needOf` keeps its meaning. The holding is one small memory per meter around it (`resetNeeds`, on the `AccountRef`, so in memory only: a daemon that has just started answers from the figures alone). burn.json (`handleUsageBurn`) and the pool that §94's decision and §95's choice read (`resetPoolOf`) both put the held need in place of the need from the figures (`resetNeeds.hold`, `holdNeed`), so all three read the same answer on one daemon. A held answer says so: `why` reads `still yes: the 24-hour pressure is 0.96, and it turns no under 0.9`, or for the rising case `still yes: the 24-hour pressure is 0.80, and the 6-hour pressure 0.90 plus its trend +0.05 is 0.95, and it turns no under 0.9`, or with no figure `still yes: no window has a pressure now, and the last one said yes 12m ago; without a figure a yes holds for 30m`. burn.json adds `held: true` on a held `yes` (`MeterNeed.Held`). `srq burn` prints it as `resets needed  still yes: the 24-hour pressure is 0.96, and it turns no under 0.9`, with one more legend line. A daemon from before §96 publishes no `held`, and holds nothing.

B. A choice keeps its place through a short gap. A chosen account that stops qualifying only because of the pool (§94.1's need on its meter is `no` or `unknown`, or the pool's demand on it is not known, so where it reaches its limit cannot be projected; `resetWeighing.onPool`) keeps its place for 30 minutes (`resetChoiceGap`) from the pass where it stopped (`chooseAcrossGaps`, around §95.1's `chooseResets`, which is unchanged for every caller that has no gap). While it waits it is not chosen: no choice is published and nothing ranks on it, so it is not loaded. The line that ends its choice is logged as before, and its reason ends `; it keeps its place until <time>, should the need return`. If it qualifies again for the same meter before its 30 minutes are up, it takes its old order back, ahead of every account chosen afresh on its meter, and keeps that order until the 30 minutes are up (`resetWeighing.place`); its reason then ends `; it keeps the place it had before the pool's need paused, until <time>`. For Codex the one account loaded one at a time keeps its hold, with its `since`, through the gap (`codexSingle` with `holdWaits`), so when need returns the same account is loaded again, and an account whose claim would now add more is not chosen in its place. A hold whose account waited out its 30 minutes is released, and the usual rule chooses. An account that stops qualifying for any other reason is dropped at once, as before: its reset is spent, its credential died, an owner's reserve appeared, or its automatic claim has not settled (C). The gaps live in memory beside the choices (`autoResets.resetGaps`), so a restart forgets them.

C. An account whose automatic claim did not settle is not chosen. While §93.2's latch says the account's last automatic claim is not settled (it was sent, or it is still running, and no reading measured after it shows its meters back: `autoResetLatch.unsettled`, the case of `blocks` that is not the 10-minute floor), the account does not qualify for a choice (`resetCandidateOf`, through `autoResetUnsettledClaim`). The latch is read as a decision reads it (`autoResets.currentLatch`, which `startAutoResetClaim` now shares): this worker's, or the record another worker of a swap left in the state directory if that claim started later, settled by the account's readings. For Codex the next account is chosen in its place. The line that ends the choice gives the reason, `its automatic claim started at <time> is not settled: no reading since shows the week back, and until one does no other claim follows`. Once a reading shows the meter back, the account qualifies again under the usual rules, as a fresh candidate. In shadow mode no automatic claim is made, so no latch exists and nothing changes there.

D. What `SUBROUTER_AUTO_RESET_WORTH=0` undoes. It returns the decision to §93's rule: claim at the limit when there is demand. It does not undo the rest of §94.2: in real mode a Claude claim at a reading moment still reads the account live first, and the two older Codex settings (`SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET`, `--reset-credit-autospend=spend`) still spend nothing. The "how to go back" sentences of `ops/OPERATING.md` (the Automatic resets section and the settings table) and `ops/skills/subrouter/SKILL.md` said it went back to §93's rule alone. They now say what it does and what stays.

Why. The owner, 2026-11-22: subrouter is to "maximize the value got from each reset", and for Codex to "pick one account, burn it to the ground, use its reset, then pick a new account". A choice that turns on and off with each reading near the bar loads an account for a few minutes and then balances again, and a Codex hold lost to a one-pass flicker leaves a half-spent account behind. An account that cannot be claimed on until a reading settles its last claim gains nothing from being loaded. These are what real mode would do on the pool as it stands, so they are settled before the owner's switch.

How to turn it off. There is no switch of its own. Need's holding, the gap and the unsettled check act wherever §94 and §95 do: `SUBROUTER_RESET_LOADING=0` chooses nothing, and `SUBROUTER_AUTO_RESET=0` turns off all of §93 to §96. `SUBROUTER_AUTO_RESET_WORTH=0` takes need out of the claim decision, as D says; burn.json and the choice still read the held need. `SUBROUTER_USAGE_HISTORY=0` takes this daemon's figures away, and need with them.

What did not change. In shadow mode and with automatic resets off no ranking changes: a choice published in shadow mode is `would_choose`, and nothing ranks on it (§95.2). With no choice published every ranking, pick and verdict is exactly what it was. The manual commands (`sr claude reset`, `sr codex reset`) do not read need, the choice or the latch's new reader. The owner's under-1-percent condition stays the first gate, on by default, and the default mode stays shadow.

Tested in Docker, every provider a fake that fails the test on any request a test does not expect; no test claims or redeems a real reset or credit. In `internal/proxy/reset_steady_test.go`:
- `TestNeedHoldsItsYesUntilThePressureFallsUnderTheHoldBar` (behaviour 1): a meter read at 1.02, then 0.98, then 0.95 answers yes three times; the second and third are held and say `still yes: the 24-hour pressure is 0.98, and it turns no under 0.9`, while `needOf` alone answers no at both. 0.89 answers no; from no, 0.98 answers no and 1.0 yes. The rising case at 1.15 turns it yes, a 6-hour pressure of 0.90 with a trend of +0.05 holds it on the `6h` window, and 0.85 ends it.
- `TestAYesHoldsThirtyMinutesWithoutAFigure` (2): after a yes, readings with no figure answer yes 10 and 30 minutes on, and unknown 31 minutes on; 0.95 then answers no. A new memory answers 0.95 no and no figure unknown.
- `TestBurnJSONTheDecisionAndTheChoiceReadOneNeed` (3): over §94.2's shadow daemon, held read at 95% of a week rolling in 60 hours with a week reset, at its week's wall. With spare's week rising at 1.02, burn.json says yes, the decision would claim with need `yes`, and the choice is made; with the history replaced by one at 0.95, all three say `still yes: the 24-hour pressure is 0.95, and it turns no under 0.9`, the decision first. A new daemon at 0.95 says no on burn.json, holds the claim back, and chooses nothing. A daemon whose burn.json alone read the yes at 1.02 decides and chooses on that yes at 0.95.
- `TestNeedIsReadWhilePassesHoldIt` (3, under `-race`): burn.json is read from four goroutines while four others run passes and a turn meets the wall, all through the one memory; the race detector finds nothing, and burn.json still says yes after.
- `TestAChosenWeekHolderKeepsItsPlaceThroughAGapInNeed` (4, on facts): holders a and b, chosen 1 and 2 at 1.5; need no at 0.7 chooses neither, and each reason says it keeps its place until 30 minutes after the gap began; during the gap b spends its week, so the usual order is b first. Back 20 minutes into the gap they are a and b, with a's reason naming its old place, still a and b 29 minutes into it, and b and a at 31; back after 31 minutes they are b and a.
- `TestAChoiceIsNeitherPublishedNorRankedDuringAGap` (4, through a real-mode daemon): held is chosen and takes a new session; with the pool's need at 0.5 it publishes no choice, a new session goes to spare, and the stop line says it keeps its place; with need back 20 minutes later it publishes order 1 and takes the next new session.
- `TestTheCodexAccountBeingLoadedKeepsItsPlaceThroughAGap` (5): c150 is loaded; during a gap at 0.7 nothing is chosen and the hold stands, with its time; back 20 minutes into the gap, with c100's claim now worth more, c150 is loaded with the same hold and c100 reads `another one is loaded now`; back after 31 minutes, c100 is chosen.
- `TestASpentResetDropsAChoiceAtOnce`, `TestADeadCredentialDropsAChoiceAtOnce` and `TestAnOwnersReserveDropsAChoiceAtOnce` (6): each first runs the gap alone, after which c150 is loaded again; then the gap with the other reason on c150, whose reason names it and not its place, whose hold is released, and after which c100 is chosen.
- `TestAnAccountWhoseClaimHasNotSettledIsNotChosen` (7): a real-mode Codex daemon, with three accounts rising at 1.3 in its history, chooses held; held meets its wall and the redeem answers 502 (`may_have_landed`). The next pass chooses second, and the one line that ends held's choice says `its automatic claim started at <time> is not settled: no reading since shows the week back`. With second's credential dead, held is still not chosen; once a reading shows held's week back, it is. One consume in all.
`ops/docscheck/steady_choice_docs_test.go`'s `TestDocsSayHowNeedHoldsAndWhatTheWorthSwitchUndoes` (8) requires the operator doc and the skill to name `still yes`, `under 0.9`, `30 minutes`, `keeps its place`, `is not settled`, `the decision to §93's rule` and `live read`, and no doc but this file to say that `SUBROUTER_AUTO_RESET_WORTH=0` goes back to §93's rule alone. `srq selftest` reads a held yes as `resets needed  still yes: ...` and the new legend line. The selection tests of §91 to §95, the whole `selectacct`, `internal/proxy` and `cmd/subrouter` packages, the ops `docscheck` and `skills` tests and every earlier `srq selftest` check pass with no line of them changed (9).

Each part was taken out of the working tree in turn, one replacement each, its test run in Docker, and the file put back, by a script outside the repository. Each went red for the predicted reason: need's `no` holding nothing (0.98 after a yes read no); the rising case not held (0.95 on the 6-hour figure read no); the hold bar at 1.0 (0.98 read no); no 30-minute limit without a figure (31 minutes on read yes); no hold without a figure (10 minutes on read unknown); one memory for every daemon (a new daemon's burn.json read still yes at 0.95); the pool that the decision and the choice read left unheld (the decision at 0.95 held back); burn.json left unheld (it read no at 0.95); no old place (a came back second); the place forgotten once the choice came back (a second 29 minutes into the gap); no gap at all (no reason said the place is kept); a waiting choice published (held published order 1 during the gap); the Codex hold not kept through the gap (the hold read empty); a hold not released once its gap ran out (c150 still loaded after 31 minutes, c100 not chosen); a place kept for any reason (the spent reset, the dead credential and the reserve each kept it); an account whose claim has not settled still chosen (held chosen after the claim). In the docs check, the skill and the operator doc each with the old sentence put back (the check found it, and the skill also lacked two names). In `srq`, the held line and the legend line each taken out. The same runs with nothing taken out passed. One first run of the legend's mutation failed on Docker's file sharing, which served `srq` while it was still being written; run again with a pause after the write, it went red for the predicted reason. The tests that run the background pass, §95's and §93's among them, also ran with `-race`.

Readings of the design, each with its reason.
- The hold reads both of need's figures at 0.9. A yes the 24 hours gave is kept by the 6-hour pressure plus its trend at 0.9 or more, and the reverse: the design names one constant beside 1.0 and 1.1, and "the pressure that decides it" is the figure need reads at that reading.
- A Claude choice back from a gap keeps its old order until 30 minutes after its gap began, not for one pass. A Claude order is worked out again at every pass, so an old order given back for one pass would turn round at the next wherever the values crossed during the gap.
- A pool whose demand on the meter is not known counts as a gap. With no figure need holds `yes` for 30 minutes (A), but where an account reaches its limit cannot be projected without the demand, so the choice would otherwise end at once for the very reason the gap is meant to cover.
- An unsettled claim is the latch's case of a claim sent, or running, with no reading since showing it back. The 10-minute floor after a claim that was read back is not one: that claim has settled, and the floor only spaces claims.
- The choice reads the latch with its record on disk, as a decision does, so the new worker of a swap does not choose an account whose claim the old worker left unsettled. A reading that settles a record read from disk is written back, as at a decision.
- A Codex hold whose account waited out its 30 minutes is released before the choosing, so the usual rule chooses. Without it the old account came back as the one loaded at any later pass where it qualified, which the test of behaviour 5 showed while it was written.

What it closes. §95.1's residual that a Codex choice that stops qualifying is forgotten, so that a need that turns `no` and back to `yes` may load a fresh account; the second independent check of §95's note that an account whose claim never settled stays chosen while no other Codex account is loaded; and the first independent check of §94's note that the documents said `SUBROUTER_AUTO_RESET_WORTH=0` goes back to §93's rule alone. `ops/KNOWN-ISSUES.md` has "Residual gaps §96 leaves open".

Residuals. Need's memory is per daemon: two daemons can still read need differently near the bar, a daemon that has read 1.02 holding `yes` at 0.98 while one that has read nothing above 0.98 since it started says `no`. The hold steadies each daemon's answer over time, and each daemon decides only for its own accounts. The memory is lost at a restart and at a worker swap, by design. A Claude choice keeps its old order only until 30 minutes after its gap began. Nothing here was run against the live pool, whose daemons stay in shadow mode.

- Knobs: none new. The constants are `resetNeedHoldAt` (0.9) and `resetNeedUnknownHold` (30 minutes) in `internal/proxy/reset_worth.go`, beside §94.1's `resetNeedAt` (1.0) and `resetNeedRisingAt` (1.1), and `resetChoiceGap` (30 minutes) in `internal/proxy/reset_choice.go`.
- Code: `internal/proxy/reset_worth.go` (`MeterNeed.Held`, `holdNeed`, `needHeldBy`, `resetNeeds`, `AccountRef.sharedResetNeeds`, `resetNeeds.hold`, `resetPoolOf` holding need), `internal/proxy/usage_burn.go` (`handleUsageBurn` holding need), `internal/proxy/proxy.go` (`AccountRef.resetNeedMemory`), `internal/proxy/reset_choice.go` (`resetWeighing.onPool`, `waiting` and `place`, `chooseAcrossGaps`, `resetGap`, `resetChoiceGap`, `resetChoicePlace`, `codexSingle`'s `holdWaits`, `compareChosen`, `resetCandidateOf`'s unsettled claim, `chooseResetAccounts`), `internal/proxy/auto_reset.go` (`autoResets.resetGaps`), `internal/proxy/auto_reset_claim.go` (`autoResetLatch.unsettled`, `autoResets.currentLatch`, `autoResetUnsettledClaim`), `ops/bin/srq` (`_need_line`, the legend)
- Tests: `internal/proxy/reset_steady_test.go`, `ops/docscheck/steady_choice_docs_test.go`, `srq selftest`
- Docs: `ops/OPERATING.md` (Read the pool's burn, Automatic resets, Accounts chosen for a reset, the settings table), `ops/skills/subrouter/SKILL.md`, `ops/KNOWN-ISSUES.md`
- Related: holds §94.1's need for §94.2's decision and §95.1's choice; keeps §95.1's choice and Codex hold through a gap in need; reads §93.2's latch as a decision does; corrects what §94.2's switch undoes in the documents. §95.2's ranking is unchanged.

## 97. claude-route can give pool launches Claude Code's advisor tool, off by default

A plain login (`claude local`, route `off`) sends Claude Code's experimental advisor server tool, `advisor`, with each main request, with the advisor model `claude-opus-5-5`. A socket-mode pool session never sent it. Claude Code 2.1.330 decides this on the client, in two steps. A gate opens through an Anthropic-side flag for a signed-in login, or through `CLAUDE_CODE_ENABLE_EXPERIMENTAL_ADVISOR_TOOL`. Then a model comes from `--advisor`, from the `advisorModel` setting, or, with neither, from a standby choice that needs Claude Code to know that the org serves the model. A socket session's token carries no plan, so that choice finds nothing.

Measured on a laptop pool session (route `pool`, Sonnet, one turn, "Reply with one word: ok"), each read from the session's `api-request-shape` row:
- The variable alone: 12 tools sent, no `advisor`, no `advisorModel` on the reply. Prompt 35,017 tokens.
- The variable and `--settings '{"advisorModel":"opus"}'`: 13 tools, `advisor` on `claude-opus-5-5`, the model a plain login gets. Prompt 36,377 tokens, 1,360 more.

The owner, 2026-11-23: "I'm fine giving the option to turn it on but leaving it off by default if we can."

§97.1 `claude-route advisor on` and `claude-route advisor off` write `on` or `off` to `~/.claude-route/advisor`, as `codex pool` and `codex off` write `codex-mode`. A missing file, or anything but `on`, is off. `install` writes nothing there, and `uninstall` removes the file. `claude-route status` prints an `advisor:` line, and the usage text names the command.

§97.2 The shim reads that file at each launch. When it says `on`, a launch on route `pool` or `pool-tcp`, pinned or not, gets `CLAUDE_CODE_ENABLE_EXPERIMENTAL_ADVISOR_TOOL=1`, unless the caller already set the variable, and `--settings '{"advisorModel":"opus"}'` ahead of its own arguments. The variable joins `CLAUDE_ROUTE_SET`, so a `claude local`, a `CLAUDE_ROUTE=off` launch or a credential command started inside such a session drops it. A Claude Code command (`$1` in the shim's reserved list, such as `mcp` or `doctor`) gets the variable and no `--settings`. Routes `off`, `credentials` and `inherited` set nothing. The dry run prints the variable, and its `args=` line shows the `--settings`.

§97.3 `claude-route test` follows the advisor file. Its dry runs use the live route home, so with the advisor on, a pool launch's `args=` line starts with the `--settings`. Eight checks expected nothing there: "'-- local' is a prompt", "'local please' is a prompt", "a word after a flag is a prompt", "an @-mention is a prompt, never a word", "Paradev's launch shape is never a word", "CLAUDE_ROUTE_WORDS=0 turns words off", the pin to a laptop account by its name in capitals, and "an unknown plain word is a prompt". Turning the advisor on failed them, `last-test` read FAIL, and `claude-route pool` then refused with "no passing 'claude-route test' today". Each now expects that start when `advisor_now` says on, and nothing when it says off. "CLAUDE_ROUTE_WORDS=0 turns words off" sets `CLAUDE_ROUTE=pool`, so its route no longer follows the mode. Two new checks say that a pool launch follows the advisor's state and that `claude local` never has it. test's real launches run Haiku. One such launch with the variable and the `--settings` (route `pool`, test's one-word prompt) sent `advisor` on `claude-opus-5-5` and answered in one request.

Readings of the design, each with its reason.
- The model goes in as `--settings`, not as the hidden `--advisor` flag. `--settings` and `advisorModel` are public. Claude Code skips a model it will not accept as an advisor request by request, while `--advisor` fails the launch, and a Claude Code that dropped the hidden flag would fail every pool launch.
- `opus` is an alias, so the advisor follows the newest Opus, as a plain login's does.
- A launch with its own `--settings` keeps it. Claude Code takes the last `--settings`, so the caller's wins, and that launch gets no advisor model unless its settings name one.

Residuals.
- With the variable set, Claude Code skips its own check that the advisor is at least as capable as the main model, so a Fable session gets an Opus advisor. Whether the API accepts that is not tested. Claude Code retries a 400 that says a model "cannot be used as an advisor" without the tool.
- `pool-tcp` gets both, as the owner asked. A real `pool-tcp` launch with them is not tested.
- Each request is about 1,360 tokens larger, and each `advisor()` call has a second model read the conversation, all on the pool account.
- Running sessions keep what they started with.

Not changed: `claude local`, launches on the `off` route, and the daemon.

- Code: `ops/bin/claude-route` (`ADVISOR_FILE`, `advisor_now`, `cmd_advisor`, `cmd_status`, `cmd_uninstall`, `cmd_test`, the shim's `add_advisor`)
- Tests: `ops/deploy/tests/deploy-scripts-test.sh` (`route-advisor-is-off-on-a-fresh-install`, `route-advisor-on-sets-it-for-pool-and-pool-tcp`, `route-advisor-off-removes-it`, `route-advisor-leaves-claude-local-alone`), `ops/deploy/tests/deploy_scripts_test.go`. With `origin/main`'s `claude-route`, all four fail. Each of five one-change mutants, run in a scratch copy, fails the case named: the file ignored, so always on (fresh install, off); no advisor on `pool-tcp` (on); the variable exported, not kept, so it is missing from `CLAUDE_ROUTE_SET` and overrides the caller's own value (on); `advisor off` writing `on` (off); the advisor added on route `off` (local). `route-test-with-the-advisor-on-passes-what-it-passes-off` runs `claude-route test` in mode `pool` and in mode `off`, with the advisor off and then on, and wants the same PASS and FAIL lines both times. It fails with the first build's `claude-route` (§97.1 and §97.2 alone: with the advisor on, the checks above flip to FAIL in both modes), with `origin/main`'s, with the checks expecting no `--settings` when the advisor is on, and with `CLAUDE_ROUTE=pool` taken off the `CLAUDE_ROUTE_WORDS=0` check (mode `off`).
- Docs: `ops/OPERATING.md` (Use claude-route), `ops/GETTING-STARTED.md`

## 98. An account listed in `SUBROUTER_PIN_ONLY_ACCOUNTS` takes pinned sessions only

The owner, 2026-11-23, approved holding a test account on the laptop's daemon "with a switch so it takes pinned sessions only". The owner wants to know whether plain Claude Code, on its own login, gets anything that Anthropic does not charge for, or charges less for, that a pooled socket-mode session pays for. The test logs one clean account in twice: into plain Claude Code in its own config folder, and into the laptop's daemon. The same workload then runs through both paths on that one account, and the account's meter is compared per token. That comparison is clean only if the pool leaves the account alone. Before §98 nothing could do that: a fresh account at 0% ranks first for every new session (and first of all while its week is unstarted, §86), takes failovers and refugees, is chosen for its resets, and counts in the pool's figures. The owner's holdout (§20) refuses pins too, and identity-only (`SUBROUTER_IDENTITY_ONLY`) serves the control plane only.

`SUBROUTER_PIN_ONLY_ACCOUNTS` is a comma-separated list of account IDs, read by the daemon that holds the accounts. Unset or empty lists none, and nothing changes. The switch's purpose is marked in the code with the owner's dated words.

What a listed account gets:
- A pin by name. A request with the caller's own selector (`X-Subrouter-Account-ID`, which claude-route's pinned launch sends on every request) takes the forced path of `accountForSessionProviderWithOptions`. That path is unchanged and never asks whether the account is pin-only. So the account serves the pinned session as any pin is served. When the account is spent or refused, the pin gets the provider's own answer, or §89's final answer inside a holdout, and is never moved to another account.
- Manual resets. `sr claude reset` and `sr codex reset` call `claudeReset` and `redeemAccountChecked` with no decision, as before.
- Its own reading. The usage sweep still reads the account. Its row keeps its meters, its resets and its own `burn`, and §92's history keeps recording it.

What it does not get, and where:
- No new session from the pool, by ranking or by any fallback. Selection's scheduler carries the list (`Server.scheduler`, `Scheduler.WithPinOnlyAccounts`), so the account is not routable for a new session (`routableForNewSession`) and ranks with the evicted and closed accounts, below every account that can take the session (`selectionTier`). Every path that places work refuses it: a new placement and a session leaving its account (`selectionRefusesAccount`, whose refusal ends in the pool's answer when nothing else is left), a caller's preference, and an idle re-rank (`migrateIdleSessionAcross`).
- No forced move onto it. A failover after a spent, evicted or refused account (`oauthCandidate`, behind `oauthRetryCandidate` and the websocket reconnect, and `retryAccount`), a single overflowed request (`oauthOverflowCandidate`), an admission spill, the usage failover's recovery probe (`oauthRecoveryProbeCandidate`) and the cooked Codex pool's redeem (§29.5) each skip it (`withoutPinOnly`, or a check beside the existing ones).
- No session stays there without its pin. A session already on it, placed before the list was set, leaves at its next unpinned model turn, as a cold session that must leave does. Its side requests (no model turn, §21) read the placement as before.
- No peer places sessions there. The verdict on its `usage-status` row, `normalized.new_session`, is `eligible: false` with reason `pin_only` (`VerdictPinOnly`), and its refugee cap is 0. Peers rank this daemon's accounts by that verdict (§31), and a peer's targeted placement of a new session is refused for that reason. A peer's score never reads the list: the list is the holder's own.
- No reset loading. §95 never chooses it (`resetCandidateOf`: "it takes pinned sessions only"), and a choice could not load it anyway: `routableForNewSession` refuses it before `loadEligible` is read, so `loadEligible` needs no check of its own.
- No automatic reset. §93 decides nothing for it at either moment: the wall (`autoResetDecideAtTheWall`) and the 10-second reading pass (`autoResetReadings`). Nothing is claimed, no decision is kept, no latch is written, and no turn waits.
- No part of the pool's figures. A fresh account at 0% would count as one more account in the equal-weight pressure, and as a holder with a whole week of headroom, which lowers the pool's pressure and lengthens the expiry lead. `resetPoolOf` (§94's decision, §95's choice and the expiry windows on usage-status) and `burn.json` (`handleUsageBurn`) leave it out, so §96's held need reads one pool. A peer that reads this daemon's body leaves out every row whose verdict is `pin_only` (`readPeerBurn`).
- Nothing the daemon sends of its own. No paid Claude Messages probe (`claudeFableProbe`, `probeClaudeLiveness`), as for the identity-only account, and no §90 turn to start a Codex week (`selfStartAllowed`). Both would spend the account's meter.
- Not the interactive default. The daemon's sr auto-switch still scores the account, every 10 minutes, but never picks it for either provider (`srAutoSwitchConfig.PinOnly`, which serve sets to `AccountRef.PinOnlyAccount`), as it never picks the identity-only account. Its scheduler comes from `SchedulerRef`, which does not carry the list, so the skip is in the rotation's candidates. As the store's active Claude profile, the account would be what a bare `sr claude run` launches on directly: its meter spent outside the pinned workload, and a second refresher on the daemon's login.

The views. `srq pool` prints the account as `new=NO (pin_only)`, never as `fallback`, with a legend line in the section that has one. `pool.json` carries `"pin_only": true` on this daemon's own row, and its state is `burn` with the note `takes pinned sessions only (SUBROUTER_PIN_ONLY_ACCOUNTS): ...` unless the account is spent. `serve` logs one Warn at startup that names the listed accounts.

Where it is set. On the supervised laptop job the setting goes in `env` in `~/.subrouter/worker-config.json`, not in the plist ([Change the daemon's environment](OPERATING.md#change-the-daemons-environment)). Set it before the account is added, so that no new session reaches the account in between. To undo it, remove it and deploy.

Not changed. claude-route reads `pin_only` as any refused verdict. It refuses a pinned launch on an account that can bill past its plan, or that reports no extra-usage state, unless `CLAUDE_ROUTE_ALLOW_PAID=1`, and otherwise launches with a note. On this build it also refuses a pin to a Claude account on this Mac other than `SUBROUTER_IDENTITY_ACCOUNT`, because a pinned session's account calls go to the pinned account (§88). The daemon's other views (`sr status`, the board's page) show the verdict only as the reason. `sr claude pick`, run by hand, picks by headroom from the daemon's rows and does not read the verdict, as for the identity-only account, so it can still make a listed account the active profile.

Tested in Docker, every provider a fake that fails the test on a request it does not expect. In `selectacct/pin_only_test.go`, `TestAPinOnlyAccountIsNeverPlacedEvenWhenItRanksFirst`: an account that ranks first unlisted is picked after the other account once listed, by the selection tier, and also after an account that is itself only a fallback (under the 5h floor), so the fallback serves. Its verdict is `pin_only` with no refugee slot, spent or not, chosen for a reset or not. A nil or empty list changes no pick and no verdict. A peer's score with the same ID keeps its holder's verdict. In `internal/proxy/pin_only_test.go`:
- `TestAPinOnlyAccountTakesNoNewSessionAndServesItsPin`: unlisted, a new session goes to the test account, which ranks first. Listed, three new sessions go to the other account, and a pinned turn is served on the test account. The session placed there before the list, and the pinned session's turn without its pin, each leave. In a pool of the listed account alone, a new session is refused and nothing is sent, and its pin is served.
- `TestAForcedMoveNeverLandsOnAPinOnlyAccount`: a session whose account meets the weekly wall moves to the test account unlisted, to the third account listed, and with no third account it is not moved there.
- `TestAPreferenceForAPinOnlyAccountIsNotHonoured` (`X-Subrouter-Preferred-Account-ID`), `TestASpillNeverLandsOnAPinOnlyAccount` (a saturated account's request goes to the least loaded free account: the test account unlisted, the next one listed) and `TestTheCookedCodexRedeemSkipsAPinOnlyAccount` (§29.5: a credit spent and the session placed unlisted, neither listed).
- `TestPeersSeeAPinOnlyAccountIneligible`: usage-status publishes `eligible: false`, `pin_only`, not evicted, refugee cap 0; a laptop that ranks the box's body places a new session on the box's test account unlisted and on its other account listed; pool.json marks the row.
- `TestAutomaticResetsDecideNothingForAPinOnlyAccount`: in shadow mode, a pinned turn at the week's wall and at the Fable wall, and a reading at 99.6% with a grant usable now and a warm session, are each a would-claim unlisted and no decision listed.
- `TestARealModePinnedTurnAtAPinOnlyWallClaimsAndHoldsNothing`: in real mode, a pinned turn on a listed account holding a week reset at the week's wall, and one holding a Fable reset at the Fable wall, get the provider's 429, the same answer as unlisted. Unlisted the claim runs. Listed there is no claim, no decision, no latch on disk and no running claim, and the next pin is answered at once without a claim.
- `TestAPinOnlyAccountIsNeverChosenForAReset`: chosen unlisted in shadow and real mode, never listed.
- `TestThePoolFiguresLeaveAPinOnlyAccountOut`: the 24-hour pressure behind need, the expiry window's holders, `burn.json` and a peer's reading of the body each count both accounts unlisted and only the other account listed, while the test account's row keeps its `burn`.
- `TestThePoolSendsAPinOnlyAccountNothingOfItsOwn` (no Messages probe, no §90 start) and `TestAnEmptyPinOnlyListMarksNoAccount`.
In `cmd/subrouter`:
- `TestPinOnlyAccountsFromTheEnvironment`: the parse.
- `TestAutoSwitchNeverRotatesOntoAPinOnlyAccount`: the sweep scores every account and, unlisted, picks the test account, which has the most headroom, as the Claude profile and the Codex pick. Listed, it picks the other account for each.
- `TestServeKeepsTheAccountItsEnvironmentListsForPinnedSessions`: `serve` in a child of the test binary, over two stored Codex accounts whose usage reads a fake chatgpt.com answers, with `SUBROUTER_PIN_ONLY_ACCOUNTS` naming the one with the most headroom. Its usage-status says `eligible: false`, `pin_only` for that account and eligible for the other, its pool.json marks that account alone, and its sr auto-switch, every 2 seconds, picks the other account.

`srq selftest` reads a `pin_only` row as `NO (pin_only)` here and on a peer, counts no fallback for it, and prints its legend line only where a row has it. The whole `selectacct`, `internal/proxy` and `cmd/subrouter` packages and the ops module pass.

Each part was taken out of a copy of the working tree in turn, one replacement each, by a script outside the repository, and its test run in Docker went red for the predicted reason. The parts are the routable check, the selection tier, the verdict's reason, the refugee cap, the score's mark, the list on selection's scheduler, selection's refusal, the session leaving the account, the preference, the spill, the failover, the cooked redeem, both §93 moments, §95's candidate, `resetPoolOf`, `burn.json`, the peer's read, both probes, §90, both pool.json marks, an empty list read as none, the parse, the sr auto-switch's skip, serve's store of the list, and serve's setting of the sr auto-switch's `PinOnly`. In `srq`, on a copy, `local_fallback` without its `pin_only` check made the selftest read the row as a fallback, and the legend line left out failed the two legend checks. The same runs with nothing taken out passed.

Not tested on their own, and kept because each is a place where the pool's work could otherwise land on the account. A failover after the selected account's credential refresh fails (`retryAccount`). The usage failover's recovery probe, which sends the turn to an exhausted account whose block has lapsed. An idle re-rank whose winner is the listed account, which needs every other account to be evicted or closed. Each is one check beside the existing evicted or closed check at the same place.

- Knobs: `SUBROUTER_PIN_ONLY_ACCOUNTS` (unset by default; account IDs, separated by commas). A change needs a restart, or a deploy on the supervised job.
- Code: `selectacct/pin_only.go` (`WithPinOnlyAccounts`, `PinOnly`), `selectacct/scheduler.go` (`Score.pinOnly`, `measuredScore`, `selectionTier`, `routableForNewSession`), `selectacct/new_session_verdict.go` (`VerdictPinOnly`), `selectacct/pace.go` (`refugeeCap`); `internal/proxy/pin_only.go` (`AccountRef.SetPinOnlyAccounts`, `AccountRef.PinOnlyAccount`, `withoutPinOnly`), `internal/proxy/proxy.go` (`Server.scheduler`, `selectionRefusesAccount`, the sticky and preferred branches, `migrateIdleSessionAcross`, `retryAccount`, the spill, `oauthCandidate`, `oauthRecoveryProbeCandidate`), `internal/proxy/codex_quota_reset_failover.go`, `internal/proxy/auto_reset.go`, `internal/proxy/reset_choice.go` (`resetCandidateOf`), `internal/proxy/reset_worth.go` (`resetPoolOf`), `internal/proxy/usage_burn.go` (`handleUsageBurn`, `readPeerBurn`), `internal/proxy/claude_credential_health.go`, `internal/proxy/self_start_week.go`, `internal/proxy/pool_ui.go` (`poolRow.PinOnly`, `classifyPoolState`), `cmd/subrouter/main.go` (`pinOnlyAccounts`, the store and the Warn, the sr auto-switch's `PinOnly`), `cmd/subrouter/sr_auto_switch.go` (`srAutoSwitchConfig.PinOnly`), `ops/bin/srq` (`local_fallback`, the legend)
- Tests: `selectacct/pin_only_test.go`, `internal/proxy/pin_only_test.go`, `cmd/subrouter/pin_only_config_test.go`, `cmd/subrouter/pin_only_serve_test.go`, `srq selftest`
- Docs: `ops/OPERATING.md` (The CLI's "protected" is not the router's gate, Keep an account for pinned sessions only, Settings)
- Related: keeps §89's answer to a pin; mirrors §72's closed queue in ranking and the verdict, and identity-only's probe and sr auto-switch exclusions; leaves §93 to §96 out for the account and out of their pool figures; §90 never starts its week.

## 99. A claim the provider says it used serves its waiting turns on the same account, even when the reading after it cannot be taken

The owner, 2026-11-22: subrouter claims every saved reset itself and gets the most out of each one, and the work running on an account is not moved off when its reset is claimed (AR-1, AR-8). Automatic resets run in real mode on the live daemons since 2026-11-23 (§93 to §96). The first automatic claim on a live pool worked: one Claude account, a week reset, claimed at a reading moment, and Anthropic used it one second after the decision. It also showed a defect. The usage read the daemon takes right after a claim (`afterClaudeReset`) was refused by the usage endpoint with `429 Too Many Requests`, so the claim finished as `landed=true back=false`, its reason ending "the reading after it does not show the week back". A turn waiting at the wall is sent again on the same account only when that reading shows the meter back (§93.2). So at a wall, every waiting turn would have been moved to another account, which is what the owner asked not to happen. No turn waited on that claim, since it was made at a reading moment. Ten seconds later the account's score read the week back, and the latch settled 16 seconds after the claim started: the meter was back, and only the immediate read could not see it. The same claim showed a smaller gap. For more than 40 minutes after it, the usage endpoint held the account off, and the daemon served Anthropic's own figures from the throttled answers, the week alone. §92's claim record waited for a reading the history records, and was written 30 minutes later with no `after`.

What it does.
- Three outcomes of the reading after a landed claim (`autoResetAfter`, the claim's `after`, in place of §93.2's `back`): `back`, a reading was taken and every meter the claim gives back reads back (at most half used, `autoResetBackUsed`, as before); `still_spent`, a reading was taken and one of those meters still reads spent; `not_taken`, no reading could be taken. The last covers a read that failed, a read that served only figures measured before the claim, and a read that carried no figure for a meter the claim gives back. The claim functions say how their read went (`resetReadAfter`: when the read was asked for, when its figures were measured, and its error), and one function (`autoResetAfterOf`) judges it for both providers.
- Served in place when no reading could be taken. When the provider says it used the reset (Claude `result: reset`; Codex a consume that came back as a redeem) and the outcome is `back` or `not_taken`, the waiting turns go on the same account (`autoResetClaimResult.servesInPlace`): the turn that met the wall is sent again once, spending no failover attempt and no retry budget, and the turns that waited before their send go to the same account, as for `back`. The provider's answer is the evidence. A turn sent again that meets the wall again gets today's answer, by §93.2's rule that a turn is sent again only once: the latch forbids a second claim, so a wrong guess costs one request and the turn is moved as before. `still_spent` keeps §93.2's behaviour: each waiting turn gets today's answer.
- Figures served from before the claim are no reading after it. When the usage endpoint is already holding the account off at the claim, the read after it serves the figures of an earlier throttled answer without asking the provider (`readUsageWindowsCached`). Before this section they were judged as if read after the claim, so a week read at 99% before the claim moved every waiting turn. A read whose figures were measured before it was asked for is now `not_taken`, and its reason says so.
- The latch is unchanged. Only a reading measured after the claim that shows the meters back settles it, and no second claim follows until one does, so a reset is never claimed twice. `not_taken` does not settle it. Until a reading settles it, §96's choice passes over the account, as for any claim that is not settled.
- The reason and the log line name the outcome. A `not_taken` claim's reason ends `the reading after it could not be taken (<why>); the provider used the reset, so the waiting turns go on the same account`, where `<why>` is the read's error (`usage fetch failed: 429 Too Many Requests`), `it carried no figure for the week`, or `the usage endpoint served only figures read before the claim, after <the throttle's error>`. A `still_spent` reason ends `the reading after it does not show the week back, so the waiting turns get today's answer`. `auto reset: claim finished` carries `reading_after` (`back`, `still_spent` or `not_taken`) for a claim that landed, beside `back`, which is true only for `back`.
- Codex the same. `redeemAccountChecked` keeps the error of its read after the consume (`RateLimitResetResult.readAfter`, outside its JSON, so the manual route's answer does not change), and a redeem whose read fails is `not_taken`.
- The daemon's own marks. A turn sent again in place is not refused by the daemon itself before it leaves. Claude: `afterClaudeReset` drops the exhaustion marks (`ClearExhausted`) and the admission pause the wall's 429 set (`clearAccount`) before its read, whatever the read gets. Codex: `redeemAccountChecked` ends the admission pause right after the consume, before its read, and `autoResetClaimCodex` drops the exhaustion marks after it, whatever the read got. A Codex rate-limit mark (§35.3) only ranks an account for new placements; the check before every send (`ExplicitBlockedUntilFor`) does not read it, so it refuses no turn and is left as it is. The test below takes each clearing out in turn.
- A throttled answer's figures are a reading for the history. A 429 from Anthropic's usage endpoint can carry the unified rate-limit headers: for each meter, how much is used and when it rolls (`UsageThrottleError.Windows`, read by `usageWindowsFromFableHeaders`, which keeps a meter only when its utilization parses). These are Anthropic's own figures for the account, the same headers every Messages answer carries and the daemon already ranks on, and the same source as the Fable meter that §92.1 records whenever the usage endpoint leaves it out. `fetchAccountUsageLive` now records them in the history as the reading they are, at the time the read was sent, so a waiting claim record gets its `after` from the first such answer and burn has no hole. A throttled answer with no figure for a meter records nothing for it, and a claim waits as before. Throttled figures served again from the daemon's cache are not new readings and are not recorded again.
- No usage read is added. The resend reads nothing, and the claim path reads exactly what it read before: the block before the claim when the cached one is not claimable, and the one read after it.

What does not change. Shadow mode and mode off claim nothing and hold no turn. The owner's under-1-percent condition comes first. A claim that failed or may have landed gives today's answer and no second claim. `sr claude reset` and `sr codex reset` answer as before: their routes call the same functions, and the new fields are not on their answers. Selection and ranking are unchanged. A Codex websocket turn and a turn a peer forwarded do not wait in place (§93.2: the relay cannot send a turn again, and a forwarder gives up on headers after 30 seconds), so they get today's answer in every outcome, as before; the claim still runs for them, and a `not_taken` claim leaves the account's marks cleared for the next turn.

Why. The provider's answer that it used the reset is what the turns need. The reading after it is a check, and the usage endpoint limits reads per login: right after the claim's own block read, it can refuse the next one. A refused check said nothing about the meter, yet it moved every waiting turn off an account that could serve them, re-billing each session's prompt elsewhere. A wrong guess costs at most one request per waiting turn, and the latch still forbids a second claim.

How to turn it off. It has no switch of its own: it changes only what a turn waiting on a claim does, and only in real mode, where turns wait. `SUBROUTER_AUTO_RESET` unset (shadow) or `0`, and a restart, turn automatic claims off with it. The history's new points go with `SUBROUTER_USAGE_HISTORY=0`, which turns the whole history off (§92.1).

Tested in Docker, every provider a fake that answers the reads after a claim as the usage endpoint answered on the live pool, in `internal/proxy/auto_reset_read_after_test.go`:
- `TestALandedClaimServesTheTurnInPlaceWhenTheReadingAfterIsRefused`: Anthropic uses the reset and refuses the read after it with 429 and `Retry-After`. In real mode the turn is refused once and then served on the same account, the session stays, the spare sees nothing, and the decision says the reading could not be taken and why. Off and shadow claim nothing and move the turn.
- `TestEveryReadingThatCouldNotBeTakenServesInPlace`: a read refused with no figures, one refused with the five hours' figures and none for the week, and a read that serves only figures from before the claim (the endpoint was holding the account off at the claim, with the week at 99%) each serve the turn in place, and each reason names its cause.
- `TestTurnsThatArriveWhileTheClaimRunsAreServedInPlaceWhenTheReadingAfterIsRefused`: three more turns (two sessions' and a new session's) wait while the claim is held at the fake; after one claim all four are served on the account.
- `TestAReadingAfterTheClaimThatStillShowsTheWeekSpentGivesTodaysAnswer`: Anthropic gives the week back to turns while its usage read still says 100%. The turn is moved to the spare, as before this section. The same fixture with the read refused serves the turn in place.
- `TestAWrongGuessAfterARefusedReadingCostsOneResend`: Anthropic says it used the reset, the read is refused, and the turn sent again meets the wall again. It is sent again once, then moved to the spare, with one claim.
- `TestTheLatchSettlesOnlyOnAReadingThatShowsTheWeekBack`: after a `not_taken` claim the latch is not settled, a wall 5 minutes later and one 11 minutes later make no claim (the floor, then the latch), a reading of the week back settles it, and the next wall claims. The finish line carries `reading_after` `not_taken` and the read's error in its reason.
- `TestARedeemWhoseReadingAfterIsRefusedServesTheTurnInPlace`: Codex over HTTP, the read after the consume refused with 429.
- `TestTheTurnSentAgainIsNotRefusedByTheDaemonsOwnMarks`: Claude, with an exhaustion mark set while the claim runs and the admission pause of the wall's own 429; Codex, with the relay's mark from a socket turn at the wall during the redeem, the admission pause (held for an hour, as a Codex 429 whose `Retry-After` runs to its reset holds it; the fake's refusal names none, so its own pause lasts a second) and a rate-limit mark. In both the turn sent again is served on the account. The test first checks that each mark is in place.
- `TestAThrottledAnswerWithTheWeeksFiguresFillsTheClaimRecord`: the claim waits for its `after`; one throttled answer with no figures leaves it waiting with no new point; the next, carrying the week's unified figures, fills its `after` with the week at 0 and the roll the headers gave, and the series gains that point.
- `TestServingInPlaceAddsNoUsageRead`: from the wall to the answer the fake counts 2 usage reads, the block before the claim and the read after it.
All but the last failed on the code before this section, each where the turn moved to the spare (or, with one account, got a 429); the last passed there with the same 2 reads. Then each part was taken out of a copy of the working tree in turn, one literal replacement each, and its test went red for the predicted reason: the wall's rule back to `back` alone (every in-place test moved its turn); `still_spent` served in place (the spent leg was served on the account); a failed read judged as spent; figures from before the claim judged as a reading; a missing meter judged as spent (each its leg moved); the latch settled by `not_taken` (the latch read settled right after the claim); no `reading_after` on the line; Claude's exhaustion marks and admission pause, and Codex's, each left in place (the resent turn moved to the spare); throttled figures not recorded (the claim had no `after`); and the Codex read's error dropped (the reason named a missing figure instead). The same runs with nothing taken out passed. The tests of §91 to §96, the manual reset tests, `selectacct`, `cmd/subrouter`, `srq selftest` and the ops skills and docs checks pass with no line of them changed, and the tests that hold several turns also ran with `-race`.

Residuals. A wrong guess costs one request for each waiting turn, and each such turn is then moved as before. The read after the claim keeps its error only for the automatic claim; a manual claim logs `claude reset re-read failed` as before. The 10-second pass settles a latch only from fresh rows, and a row served from a throttled answer is not fresh, so while the usage endpoint holds an account off, a `not_taken` claim stays unsettled until a fresh reading arrives; on the live pool that took 16 seconds, and until then §96's choice passes over the account. A throttled answer's figures are recorded at the precision the headers give. If they round differently from the usage endpoint's, a fall of more than 0.1 point between one of each starts a `drop` segment (§92.1), which costs burn at most one stretch at that start. A claim record whose first reading after it is a throttled answer has `after` only for the meters that answer named. After a Claude claim whose reading could not be taken, the account's saved resets read as not read until its next usage read succeeds, since `afterClaudeReset` forgets the cached block before its read; a wall in that time decides `no_claim` with `the account's saved resets have not been read`, before the latch is asked (the wrong-guess test sees this). Not measured: how often the usage endpoint refuses the read right after a claim, and whether Anthropic ever answers `result: reset` for a reset it did not apply, which a wrong guess would show as one refused resend.

- Knobs: none new. `SUBROUTER_AUTO_RESET` (§93.1) and `SUBROUTER_USAGE_HISTORY` (§92.1) as before.
- Code: `internal/proxy/auto_reset_claim.go` (`autoResetAfter`, `autoResetClaimResult.after` and `servesInPlace`, `resetReadAfter`, `autoResetAfterOf`, `runAutoResetClaim`, `finishAutoResetClaim`, `autoResetClaimClaude`, `autoResetClaimCodex`), `internal/proxy/claude_reset.go` (`afterClaudeReset`, `ClaudeResetResponse.readAfter`), `internal/proxy/proxy.go` (the wall's wait in `usageLimitRetryTransport.roundTrip`, `redeemAccountChecked` and `RateLimitResetResult.readAfter`, `fetchAccountUsageLive`)
- Tests: `internal/proxy/auto_reset_read_after_test.go`, `ops/docscheck/read_after_docs_test.go`
- Docs: `ops/OPERATING.md` (Automatic resets, the claim record), `ops/skills/subrouter/SKILL.md`
- Related: corrects §93.2's resend rule and §92.1's claim record; keeps §93.2's latch and §96's choice as they are; the marks are §93.2's and §46's.

## 100. Pool launches name the Max plan, and the advisor stays off by disabling it

A socket-mode pool session authenticates with the shim's placeholder `CLAUDE_CODE_OAUTH_TOKEN`. Claude Code 2.1.330 reads the plan of such a token from `CLAUDE_CODE_SUBSCRIPTION_TYPE`, and the shim set none. Socket mode also skips Claude Code's bootstrap, so the list of models the org serves stays empty. With neither, Claude Code's check that the org serves a model fails for every model. Two things follow (code, and measured where marked):
- The built-in web-fetch agent (`agent:builtin:web-fetch`) runs on the session's own model, where a Max login runs it on Haiku (code). The pool sent 58 such requests between 11-20 and 11-23: 41 on Fable 5.2, 17 on Opus 5.5 and none on Haiku (measured). At Claude Code's list prices they cost about $172, against about $2.50 on Haiku.
- No standby advisor is sent, where a plain login sends one.

The owner, 2026-11-23: pooled sessions present the plain login's subscription type, and the advisor stays off unless it is switched on (§97: "I'm fine giving the option to turn it on but leaving it off by default if we can.").

Measured on laptop pool sessions (route `pool`, Sonnet, `-p`, one launch each), read from each session's `api-request-shape` and `api-request` rows and its usage. The prompt asked Claude Code to fetch one small public page.

| Launch | Tools sent | Advisor | Betas | Web-fetch agent's model | First request's prompt tokens |
|---|---|---|---|---|---|
| The pool route before this section | 12 | none | 20 | `claude-sonnet-5-5` | 35,043 |
| + `CLAUDE_CODE_SUBSCRIPTION_TYPE=max` + `CLAUDE_CODE_DISABLE_ADVISOR_TOOL=1` | 12 | none | 19 | `claude-haiku-5-5` | 35,045 |
| + `CLAUDE_CODE_SUBSCRIPTION_TYPE=max` only | 13 | standby, `claude-opus-5-5`, deferred | 20 | `claude-haiku-5-5` | 35,045 |
| + that and §97's `on` (its variable and `--settings`) | 13 | `claude-opus-5-5`, deferred | 20 | `claude-haiku-5-5` | 36,401 |

- The plan alone sends the standby advisor at no prompt cost, but the model cannot use it. Asked to call its advisor, that session searched its deferred tools for `advisor`, found none and answered without one.
- With §97's `on` and the plan, the same prompt called the advisor once. In that call, `claude-opus-5-5` read 37,995 input tokens uncached and wrote 380 output tokens. Claude Code put it at $0.16 at list prices.
- Without the advisor, requests drop the `advisor-tool-2026-03-01` beta, so they send 19 betas, where a plain login and the pool route before this section send 20.

§100.1 Routes `pool` and `pool-tcp`, pinned or not, set `CLAUDE_CODE_SUBSCRIPTION_TYPE=max` unless the caller set it. Every pooled account is a Max plan. While `~/.claude-route/advisor` is not `on`, both routes also set `CLAUDE_CODE_DISABLE_ADVISOR_TOOL=1` unless the caller set it. That keeps the plan's standby advisor out, so a pool launch stays advisor-off by default, as §97 left it. With the advisor `on`, they leave `CLAUDE_CODE_DISABLE_ADVISOR_TOOL` unset and add §97's variable and `--settings` as before, because the standby alone cannot be called. Both variables join `CLAUDE_ROUTE_SET`, so a `claude local`, a `CLAUDE_ROUTE=off` launch or a credential command started inside a pool session drops them. Routes `off`, `credentials` and `inherited` set neither. The dry run prints both. The shim marks the default with the owner's dated wording.

§100.2 `claude-route test` checks that a pool launch disables the tool while the advisor is off and leaves it enabled while it is on, that the launch names the plan either way (the new check "...and names the Max plan"), and that `claude local` gets neither variable.

Launches through this branch's shim, from a scratch route home against the live daemon (Sonnet, `-p`):
- Advisor off, fetch prompt: 12 tools, no advisor, and the web-fetch agent's two requests on `claude-haiku-5-5`. The first request's prompt was 35,045 tokens.
- Advisor off, a Bash command that prints the environment: the session's Bash saw `CLAUDE_ROUTE_ACTIVE=pool`, the socket and `CLAUDE_CODE_DISABLE_ADVISOR_TOOL=1`, but not `CLAUDE_CODE_SUBSCRIPTION_TYPE`.
- Advisor on, advisor prompt: 13 tools, and the advisor was called once on `claude-opus-5-5` (37,995 input tokens, 267 output).

Readings of the design, each with its reason.
- The plan is `max` for every launch, and the shim does not ask the daemon. Every pooled account is a Max plan, and the measured gates read only the plan. The shim leaves `CLAUDE_CODE_RATE_LIMIT_TIER` unset, because the tier belongs to each account.
- The tool is disabled while the advisor is off, instead of leaving the standby in each request. The standby costs no prompt tokens, but the owner's default is no advisor.

Side effects, measured on these launches.
- With a plan set, Claude Code fetches its Grove settings and Grove notice config at start, and gets a 401 for each. It skips the remote-settings fetch, which also gets a 401 without a plan. None of these calls reached the daemon: in that window its log shows only `/api/hello`, `/api/claude_code/policy_limits` and `/v1/messages`. So each launch now sends Anthropic two refused calls, where it sent one before. They carry the placeholder token (inferred from the 401s).

Residuals.
- A `claude` started from a pool session's own Bash tool takes route `inherited` without the plan. Claude Code withholds `CLAUDE_CODE_SUBSCRIPTION_TYPE` from the processes it starts (measured above; code: the child environment's withheld list). Such a session runs as pool sessions did before this section, with the advisor still disabled. Subagents run inside the session and keep the plan.
- On `pool-tcp`, Claude Code sends no OAuth token, and it reads the plan only for an OAuth token (code), so the plan does nothing there. The route sets it for parity with `pool`.
- Interactive sessions, Fable and Opus main threads, and the bootstrap's missing `client_data` are not measured here. The plan does not bring the bootstrap back: socket mode still skips it.
- Running sessions keep what they started with.

Not changed: `claude local`, launches on the `off` route, and the daemon.

- Code: `ops/bin/claude-route` (the shim's `pool` and `pool-tcp` routes and `add_advisor`, the dry run, `cmd_test`, the usage text)
- Tests: `ops/deploy/tests/deploy-scripts-test.sh` (`route-pool-names-the-plan-and-disables-the-advisor-when-off`, `route-pool-names-the-plan-without-disable-when-the-advisor-is-on`, `route-a-launch-inside-a-pool-session-drops-the-plan`), `ops/deploy/tests/deploy_scripts_test.go`. With `origin/main`'s `claude-route`, all three fail and §97's four still pass. Each of five one-change mutants, run in a scratch copy, fails the cases named: the plan also set on route `off` (all three); the plan exported, not kept, so it is missing from `CLAUDE_ROUTE_SET` and overrides the caller's own value (off, inside a pool session); the tool disabled with the advisor on too (on); the tool not disabled while off (all three); no plan on `pool-tcp` (off, on). `route-test-with-the-advisor-on-passes-what-it-passes-off` also fails for the first, third and fourth, through test's new checks.
- Docs: `ops/OPERATING.md` (Use claude-route), `ops/GETTING-STARTED.md`
- Related: changes §97's off state from setting nothing to disabling the tool; §97's `on` is unchanged.

## 101. No subrouter-internal header reaches a provider

The owner's goal, 2026-11-23: a pooled session should behave and cost like plain Claude Code on a normal login, with nothing extra that Anthropic charges or sees. That day's comparison of socket-mode sessions with plain ones found one request header that only a pooled turn sent. The laptop sets `X-Pool-Hop: 1` on each turn it forwards to a box (`forwardToPeer`, and `serveRemotePoolWebSocket` for a Codex websocket). The box reads it, and then sent it on: only the stand-in (`internal/standby/handler.go`, §79) deleted it, and neither `session.StripSubrouterHeaders` nor `stripOutboundForwardingHeaders` names it. So every box-served turn reached Anthropic, or chatgpt.com, with `X-Pool-Hop: 1`. The same gap let through, on every path, any `X-Subrouter-*` name that the list in `StripSubrouterHeaders` leaves out. Upstream's `X-Subrouter-Retry-Policy` (upstream #505, not ported, still sent by `deploy/fly-egress/live-regions.py`) reached the provider from the laptop's own turns too. And the Bedrock gateway copied a client's headers to AWS with no strip at all.

Measured before the fix, in Docker, with the tests below run on `origin/main` and the fix's code kept out of the tree. A real laptop forwarding a Claude turn to a real box, each in front of its own fake Anthropic: the box's Anthropic received `X-Pool-Hop: 1` and `X-Subrouter-Retry-Policy`. A Codex turn the laptop forwarded, over HTTP and over a websocket: chatgpt.com received both. The laptop's own Claude and Codex turns: `X-Subrouter-Retry-Policy`. The Fable API-key stage: `X-Pool-Hop`. The Bedrock gateway and a Codex regional egress: all six internal names the tests send.

§101.1 What counts as internal. `internalHeader` (`internal/proxy/provider_headers.go`): every `X-Subrouter-*` name, `X-Pool-Hop`, and the two older routing names `X-Model` and `X-User-Email`. No provider reads any of them. A new internal name with the prefix needs no new list entry.

§101.2 Where they come off: where the request leaves for the provider, and nowhere sooner. A daemon reads the hop while it decides. A forwarded turn is never forwarded again (`peerForwardingAllowed`) and never held (`holdEligible`, §58). It never waits in place for a claim (`peerForwarded`, §93.2). Its sender gets the whole Retry-After of this daemon's own 503 (`clientRetryAfter`, §78), the one the usage-limit layer builds in the transport (`poolExhaustedResponse`) included. So:
- `providerTransport` is the bottom of `Server.transport()`, under every retry and fallback layer. Each attempt goes out with the internal headers taken off a copy, and the request each layer holds keeps them. Everything else that sends through `Server.transport()` gets the same: the Opus substitution (§11), §90's self-start turn, the Antigravity project lookup.
- Each Codex regional egress transport (`codexEgressTransports`) wraps one too: a regional attempt does not go through `Server.transport()`.
- The exits that build their own headers strip them there: the websocket dial (`proxyWebSocket`), the Fable API-key stage (its own `X-Subrouter-*` loop gives way to `stripInternalHeaders`), and the Bedrock gateway (`copyBedrockRequestHeaders`, before the request is signed).
- The Azure fallback builds a fresh request with only its own four headers, and is unchanged.

The laptop-to-box hop is unchanged. A peer forward goes out through the peer's own client (`RemotePool.client`), never through `Server.transport()`, and still carries `X-Pool-Hop`, `X-Subrouter-Agent`, `X-Subrouter-Session` and the target account.

§101.3 A subrouter chain keeps its headers. With `--upstream` naming another subrouter and `SUBROUTER_FORWARD_SESSION_HEADERS` on, the daemon sends the session headers on purpose (`setDelegatedSessionHeaders`), and the next daemon strips at its own provider exit. In that mode (`upstreamIsSubrouter`) neither `Server.transport()` nor the websocket dial strips anything, as before. `--upstream` alone names a provider endpoint and is stripped like any provider.

§101.4 The daemon's own requests set only the provider's own headers, so none carries an internal one: §90's self-start turn (`sendSelfStart`: content type, accept, `originator`, `session-id`, `thread-id` and the account's auth, and now through `providerTransport` too); the Claude usage reads and token refreshes (`agents/claude/store.go`); the Claude reset claims and their reads (`agents/claude/reset.go`); the Codex usage reads and redeems (`accounts/codex_usage.go`, `accounts/codex_reset.go`); the Fable probe (`agents/claude/setup_token.go`); the provider health probe (`internal/proxy/provider_health.go`). Each builds a fresh request and never copies a client's headers.

The audit: every header a subrouter hop or client sets on a request that is proxied, where it is set, and where it came off before the provider. "Box" is a peer-served turn, "own" a daemon's turn on its own account.

| Header | Set by | Stripped before §101 | Reached a provider before §101 |
|---|---|---|---|
| `X-Pool-Hop` | the peer forward (`forwardToPeer`, `serveRemotePoolWebSocket`) | only by the stand-in | yes: every box turn, Claude and Codex, HTTP and websocket; the Opus substitution and the Fable API-key stage on a box |
| `X-Subrouter-Agent`, `X-Subrouter-Session` | the client, the peer forward, the stand-in, a session lease, `sr` | `StripSubrouterHeaders` on every path; the Bedrock gateway not | only through the Bedrock gateway |
| `X-Subrouter-Target-Account` | the peer forward (§31) | `StripSubrouterHeaders` | no |
| `X-Subrouter-Account-ID`, `X-Subrouter-Account`, `X-Subrouter-Preferred-Account-ID` | the client, a lease, the Codex catalog | `StripSubrouterHeaders` | only through the Bedrock gateway |
| `X-Subrouter-Model`, `X-Model` | the client, a lease, the Opus substitution | `StripSubrouterHeaders` | only through the Bedrock gateway |
| `X-Subrouter-User-Email`, `X-Subrouter-User`, `X-User-Email`, `X-Subrouter-Lease` | the client, `sr`'s relay | `StripSubrouterHeaders` | only through the Bedrock gateway |
| `X-Subrouter-Client`, `X-Subrouter-Client-Version` | the client, `sr`'s relay | `StripSubrouterHeaders` | only through the Bedrock gateway |
| `X-Subrouter-Azure`, `X-Subrouter-No-Retry`, `X-Subrouter-Retry`, `X-Subrouter-Capacity-Retry*`, `X-Subrouter-Test-Fault` | the client, `sr az` | `StripSubrouterHeaders` | only through the Bedrock gateway |
| `X-Subrouter-OpenAI` | `sr openai`'s Codex | no list | not on a forced turn (Azure builds its own request); yes on any other turn carrying it |
| `X-Subrouter-Retry-Policy`, any other `X-Subrouter-*` | upstream's clients, `deploy/fly-egress/live-regions.py` | no list (the Fable API-key stage by prefix) | yes, own and box, Claude and Codex |
| `Forwarded`, `X-Forwarded-*`, `X-Real-IP` | a proxy in front | `stripOutboundForwardingHeaders`; the Bedrock gateway not | only through the Bedrock gateway. Not internal; §101 leaves them as they are |

The `X-Subrouter-*` names that a daemon sets on its answers (`Account`, `Pool`, `Peer`, `Region`, `Reason`, `Transient`, `Target-Refused`, `Error`) go to the client and never to a provider. The admin and store names (`Admin-Token`, `Store-*`, `Shadow-Challenge`, the tenant and import tokens) go to `/_subrouter/` routes, which are never proxied.

Tested in Docker, in `internal/proxy/provider_headers_test.go`. Each fake provider records the headers of every request it is sent. The client's turns carry `X-Subrouter-Client`, `X-Subrouter-Retry-Policy` and Claude Code's own `X-Claude-Code-Session-Id`, which must arrive.
- `TestNoInternalHeaderReachesAnthropicOnABoxServedTurn`: §31's end-to-end laptop and box. The box serves the turn, sees it at hop 1, and its Anthropic receives no internal header. The laptop's own Anthropic receives nothing.
- `TestNoInternalHeaderReachesAnthropicOnTheLaptopsOwnTurn`: the laptop's local account serves the turn.
- `TestNoInternalHeaderReachesChatGPTOnCodexTurns`: Codex over HTTP and over a websocket, on the daemon's own account and on a box's after a real laptop forwarded it at hop 1.
- `TestTheProviderTransportStripsOnlyWhatGoesOnTheWire`: through `Server.transport()`, the wire gets no internal header for a provider and for an `--upstream` provider, and all of them for a subrouter chain. The caller's request keeps every one, so the layers above still read the hop.
- `TestTheOtherProviderExitsStripInternalHeaders`: the Fable API-key stage, the Bedrock gateway and a Codex regional egress.
- `TestAForwardedTurnIsReadAsForwardedUnderTheReverseProxy`: where the hop comes off. A Claude pool whose last account is an API key out of credit gets the daemon's own 503 from inside the transport. An end client is told 60 seconds and a peer's forward the whole two hours, and Anthropic receives no internal header either way.

With `origin/main`'s code and these tests, every test fails. The last one's two Retry-After cases pass there, since nothing is stripped, and only its check of the wire fails. §93.2's `TestTurnsThatCannotWaitGetTodaysAnswerAndTheClaimStillRuns` ("a turn a peer forwarded" is moved to a spare, never waits), `TestE2EMutualPeersNeverLoop`, §78's peer cases, the websocket hop tests and the two delegated-session tests pass unchanged, and so does the whole `internal/proxy` package.

Each part was taken out of a scratch worktree in turn, one change each, by a script outside the repository, and its tests went red for the predicted reason. Without the strip in `Server.transport()`, the box's Anthropic received `X-Pool-Hop: 1`, the laptop's own turns and the Codex HTTP turns `X-Subrouter-Retry-Policy`, and the wire everything. Without the websocket strip, the Codex websocket turns failed, own and box-served. Without the strip on each of the other three exits, that exit's case failed. The Fable API-key stage also failed the older `TestClaudeFableAPIKeyStageStripsSubrouterHeaders`, whose prefix loop this replaces. With a subrouter chain stripped too, the chain case and both delegated-session tests failed. Two placements that strip too soon both failed the last test, telling the peer `60` where `7200` was due: the strip moved into the reverse proxy's outbound request, and a strip on the request the handler clones for the provider. The §93.2 test stays green under both. It reads the hop on the inbound request, which neither placement touches, so it cannot tell when the strip comes too soon below that point. The last test can. The same runs with nothing taken out passed.

Deploy. Needs a deploy to the laptop and both boxes: the boxes send the box-served turns, and every daemon sends its own. Either order is safe: the hop still crosses the peer link, and no daemon reads anything new. The coordinating session deploys it together with §99.

- Knobs: none.
- Code: `internal/proxy/provider_headers.go` (`internalHeader`, `stripInternalHeaders`, `providerTransport`, `upstreamIsSubrouter`), `internal/proxy/proxy.go` (`Server.transport`, `proxyWebSocket`, `setDelegatedSessionHeaders`), `internal/proxy/codex_egress.go` (`codexEgressTransports`), `internal/proxy/claude_fable.go` (`claudeFableAPIKeyResponse`), `internal/proxy/bedrock.go` (`copyBedrockRequestHeaders`)
- Tests: `internal/proxy/provider_headers_test.go`
- Related: §16 and §31 (the peer forward and its hop, unchanged), §58, §78 and §93.2 (readers of the hop, unchanged), §79 (the stand-in already deleted it), §90 (the self-start turn, now through the same transport).

## 102. After a claim the provider says it used, the account's row and ranking tell the truth at once

The owner, 2026-11-22: subrouter claims every saved reset itself and gets the most out of each one (AR-1, AR-2). Automatic resets run in real mode on the live daemons since 2026-11-23 (§93 to §96, §99). After the first automatic claim the owner asked (AR-25): "once a rate limit claims it should trigger an immediate usage check update". The daemon already reads the account's usage in the same second as the claim (`afterClaudeReset`), but on that first claim the usage endpoint refused the read with `429 Too Many Requests`, and for about an hour afterwards the account's row was partly wrong or empty. It carried no `complimentary_reset` block and so no `resets` list, for most of that time no Fable meter, and while the endpoint held the account off the daemon could serve the account's figures from before the claim. And by §56's carry rule (a reading that knows nothing keeps the previous hold) the account went on counting as the holder of the week reset it had just used: ranked at 2x and drained early, here and, through its row, on its peers. §102 is built in parts: §102.1 makes the row and the ranking say, from the claim itself, what the daemon knows; §102.2 gets a real reading as soon as the provider allows one, and carries the marks to the views.

§102.1 The block, the hold and the meters from the claim itself.

The record of a claim. When the provider says a claim used a reset (Claude `result: reset`; Codex a consume that came back as a redeem), the daemon keeps one record for the account (`claimedReset`, in memory on the `AccountRef`): when the answer came, the kind used, the block the claim left, and the meters the claim gave back. Both claim functions make it, so the automatic claim and a claim by hand through a daemon (`sr claude reset`, `sr codex reset`, `sr reset` with an account, the failover redeem and the spender) all do (`noteResetClaimed`, from `claudeReset` and `redeemAccountChecked`). A claim that failed, was refused, or may have landed makes none: only the provider's word that it used the reset does. A new landed claim on the account replaces the record.

A. The block from the claim. The block the claim left is the last block read before it with the claimed reset counted as used. Claude: the block the claim went by (the cached one or the live read), with one reset fewer on the grant claimed, read through `agentclaude.ResetInfo` as every block is (`claudeBlockAfterClaim`), so `remaining` goes down by one, `remaining_by_kind` loses one of that kind, and `week_held_until` is gone when the week reset was used, by the same rules as a reading. Codex: the reading before the redeem with the count one lower (`codexBlockAfterRedeem`), and the redeemed credit leaves the credit list the daemon last read (`heldResets.forgetCredit`), since the credit is spent at the provider. Until a reading of the block replaces it, each `usage-status` row of the account carries that block as `complimentary_reset`, builds `resets` (§91) from it, and says `resets_from_claim: true`; the field is absent otherwise (`withResetsFromClaims`, the first step of `withHeldResets`, so pool.json, §94's worth and §95's choice read the same block).

A reading replaces it. A reading of the block is a known block from a read sent after the claim's answer: the sweep's or the refresh's Claude usage read (stored with its block, before the readers that wait on it wake), Claude Code's own read (the hourly fallback, and the claim route's status read), the Codex sweep's read, and a Codex redeem's own read after the consume (`noteResetBlockRead`; the last since part I). It stands, and the mark goes. A Codex row's count comes only from the sweep's own read, so a Codex row that no read since the claim measured (the snapshot a failover's redeem leaves in place, a sweep whose read is refused) carries the first reading after the claim in place of the block, unmarked, and B's decision goes by it too (`blockOn`, since part I). A reading that still lists the reset the claim used (more of that kind than the claim left) wins too, and logs `saved resets: the first reading after a claim still lists the reset the claim used; the reading stands` at Warn once, with `account` (its label, §87), `provider`, `kind`, `claimed_at`, `read_at`, `left_after_claim` and `left_read`. A block Anthropic could not read itself (`unavailable`), and a read that carried no block, replace nothing.

B. Nothing claims on a block that is not a reading. While the record holds a block from the claim, an automatic decision that needs the account's next reset answers as before that its saved resets have not been read, and says why: `the account's saved resets have not been read; its saved resets were last known from the claim at <time>, not from a reading` (`autoResetNextFor`, both providers). The wall records `no_claim` with that reason and sends nothing; the 10-second pass decides nothing. Once a reading of the block arrives, the decision goes by it as §93 to §96 rule.

C. The hold ends with the reset. The claim publishes the account's score at once (D) with the hold the block from the claim gives (§56's `resetHeldUntil` on that block): none when the week reset was used, the soonest other week reset's end when one is left. A later reading that knows nothing carries that hold, now none, by §56's rule. A sweep whose reading was measured before the claim, such as one in flight at the claim, scores nothing for the account (`claimedBefore` in `updateSchedulerFromUsageStatusesAtScoreRevision`), so it cannot bring the hold back. So the account stops counting as a holder the moment the provider says the reset was used, with the block unread: no 2x rank and no early burndown from that reset, and its row's `normalized` publishes no `seconds_to_held_week_reset_expiry`, so a peer sees no hold either. An account whose claim did not land keeps its hold. §95's choice reads the same block, so the account is not chosen for the reset it used.

D. The meters a claim gives back. The meters are what Anthropic says it cleared, else what the grant clears (`five_hour` is the `5h` window, `seven_day` the `7d` window, `seven_day_overage_included` the Fable window), else the week; for Codex the account's week windows of the reading before the redeem. From the claim's answer on, no figure for such a meter measured before the claim stands as that meter's present value:
- The claim drops the figures the daemon kept from before it: the cached usage read (already §46's `forgetUsageWindows`, now for Codex too) and the given-back meters in the Fable cache (`forgetFableBeforeClaim`). The figures a throttled answer carried before the claim stay with the provider's hold-off, which a read while it lasts can only extend: a read after the claim that is served them judges them as figures from before it (§99), and no row or score shows or ranks on them (below).
- The claim publishes the account's score from the reading before it without the given-back meters (`scoreFromUsageWindows`, measured when that reading was, merged as a reply's headers are merged). A score with no week figure ranks on what is left (the five hours), as a Claude account with no week reading always has, so a new session can be placed on the account in the gap and the figures its replies carry fill the meter in. Nothing is made up: no figure and no roll time.
- A reply to a request sent before the claim says nothing after it: its headers are not observed (`claimedBefore` in `observeClaudeQuotaHeaders`).
- A row whose reading was measured before the claim (the last-good row the sweep restores over a refused read, or the figures of a throttled answer from before the claim) shows no figure for a given-back meter. No score is built from such a row: the sweep scores only fresh rows, and one measured before the claim is skipped (C).
- The row names each given-back meter no figure measured after the claim shows yet in `not_read_since_claim` (its window's name, such as `7d` or `oauth-apps-weekly`). A figure measured after the claim, from a reading, a reply's headers or a throttled answer's headers, is shown, and the name goes. Once a reading has replaced the block and every meter has a figure again, the record is forgotten.

G. §99's independent check, two findings.
- Its test of turns that arrive while a claim runs (`TestTurnsThatArriveWhileTheClaimRunsAreServedInPlaceWhenTheReadingAfterIsRefused`) used a pool of one account, so it could not catch a turn being moved. It now runs with a spare account that has room: the spare receives nothing and each session stays on the claimed account. With a spare in the pool, selection would place a session with no assignment on the spare, so the fourth turn is a new session placed on the claimed account as its first turn arrives. Test only.
- A Fable claim could be judged `still_spent` on a Fable figure from before the claim: `afterClaudeReset` forgot the usage cache but not the Fable cache, and a usage answer that left the Fable bucket out was merged with it. The claim now drops the given-back meters from the Fable cache (D), and the claim path's own read after the claim (`withReadAfterClaim`), when its answer carries no figure for a meter the claim gave back, adds none: no figure from before the claim, no Fable probe (a request the claim path never made), and no unused-Fable placeholder. The meter reads `not_taken`, the turn is served in place by §99's rule, and the row names the meter as not read since the claim. Every figure the read after the claim judges is measured after it, meter by meter.

H. §93.2 said that every row the 10-second pass sees, of any age, can settle a latch. The pass and a decision read a row's meters only when its reading is fresh (`autoResetReadingsOf`), so a row served from last-good figures or from a throttled answer settles nothing; §99's residual says so too. The sentence in §93.2 now says what the code does.

§102.2 The reading that follows, the views and the documents.

What the live pool showed. The first automatic claim (2026-11-23, one Claude account, a week reset, real mode) landed in one second: `result=reset`, `resets_left_after=0`. The usage read in the same second got `429 Too Many Requests`. Ten seconds later the account's score read headroom 1, from the figures every reply carries in its headers. For the next 40 minutes and more, the account's `usage-status` row carried the week alone: no Fable meter and no `complimentary_reset` block. After about an hour it had the Fable meter back, and still no block. Every other account on that daemon had all three.

E. Why the block stayed unread, read from the code before this change. Each citation is a file and line as they stand since §102.2, followed by the function or constant the line is in, so it still finds its code once the lines move. `ops/docscheck/follow_up_docs_test.go` fails when a cited name is no longer declared in its file.
- The deadline after a refused usage read is the one the error carries, else one minute: `internal/proxy/proxy.go:1294` (`usageWindowsThrottleUntil`) and `internal/proxy/proxy.go:1290` (`usageWindowsThrottleTTL`). A Claude 429 always carries one. `agentclaude.FetchUsage` reads `Retry-After` as seconds or a date, and when it is absent, 0 or malformed it sets five minutes from the refusal: `internal/agents/claude/store.go:5653` (`FetchUsage`), `internal/agents/claude/store.go:5612` (`usageThrottleRetryAt`) and `internal/agents/claude/store.go:5592` (`usageThrottleFallbackWait`). So for Claude the deadline is Anthropic's `Retry-After` when it sends one, and the daemon's own five minutes when it sends none. The one minute applies only to a Codex 429 with no `Retry-After`, which carries no deadline: `internal/accounts/codex_usage.go:525` (`FetchCodexUsageDetails`) and `internal/accounts/codex_usage.go:562-578` (`codexUsageRetryAfter`).
- The deadline is set when the refused read's shared flight finishes: `internal/proxy/proxy.go:1686-1704` (`fetchUsageWindowsShared`). While it lasts, a read of the account through the cache is answered without a request, `internal/proxy/proxy.go:1536-1546` (`readUsageWindowsCached`), and the same check runs under the lock, `internal/proxy/proxy.go:1626` (`fetchUsageWindowsShared`): with the last good figures, with the figures the throttled answer carried (on the live pool, the week alone), or with the error. A sweep inside the hold-off does not renew it. The first read after it that is refused again sets a new one from that moment. So while the endpoint keeps refusing, the account is read at most once per deadline, and only when a sweep comes after the deadline.
- A sweep row carries a block only from a fresh read: `internal/proxy/proxy.go:3880` (`usageStatusesLive`). Figures served during the hold-off are not fresh, so the row had no block for as long as the endpoint refused.
- When a row is fresh but its usage answer carried no block, the block comes from the daemon's own read with Claude Code's query (§46.1): `internal/proxy/claude_reset.go:1000` (`claudeResetFromFallback`). The claim forgets that read, `internal/proxy/claude_reset.go:635` (`afterClaudeReset`), so the next fresh row starts one at once; since §102.2 it starts none while a follow-up reads the account, `internal/proxy/claude_reset.go:1004` (`claudeResetFromFallback`). It reads the same endpoint for the same login, but it does not look at the hold-off: `internal/proxy/claude_reset.go:1016-1024` (`claudeResetFromFallback`). A refused one is stamped, so the next one waits the full hour: `internal/proxy/claude_reset.go:969-973` (`finishClaudeResetFallback`), with the hour set at `cmd/subrouter/main.go:1547` (`serve`).
- An account in use is not read live at all. Each reply's headers renew its cached usage entry, `internal/proxy/claude_response_evidence.go:119-149` (`observeClaudeQuotaHeaders`), and a read finds that entry younger than two minutes and serves it, `internal/proxy/proxy.go:1522` (`readUsageWindowsCached`). Its block then comes only from that hourly read.
So the hour came from the daemon's own rules, not the provider's: once the reset read after the claim had been tried, no read of the block was due for an hour, and while replies kept the cached entry young, no usage read was made. This is read from the code. The live logs of that claim were not read for this section, so the `Retry-After` values Anthropic sent, and which of the last two paths ran, are not known.

The follow-up. When the claim path's own read after a landed claim did not read the block, the daemon arms one follow-up for the account (`armClaimFollowUp`, called from `afterClaudeReset` after its read and from `redeemAccountChecked` when its read fails). The read was refused, failed, served only figures from before the claim, or carried no block. A pass every 10 seconds (`RunClaimFollowUps`, `claimFollowUpScanInterval` in `cmd/subrouter/main.go`) reads the account again when it is due:
- Never while the provider holds the account's usage reads off, since a read then can only extend the hold. The follow-up is due at the deadline the refusal recorded, and the pass looks at the hold again before each read.
- After a refusal, at that deadline: Anthropic's `Retry-After`, or the daemon's own five minutes when it sent none. A 429 that names no deadline still asks to hold off, so the five minutes stay. After a failure that names no deadline (a network error), after 20 seconds, so within 30 seconds at the 10-second pass, then 40, 80 and so on, up to 10 minutes (`claimFollowUpRetry`, `claimFollowUpMaxWait`). A read that is answered and still leaves the block unread (Anthropic's `unavailable`) backs off the same way, so it is not sent again at every pass.
- Through the read the sweep makes: the shared flight (`fetchUsageWindowsShared`), not the two-minute cache that replies keep young. A sweep that reads the account at the same moment shares the one request. The flight records a refusal's hold-off for every reader, and a sweep after the follow-up is served from what the flight stored. A flight notes its block before it leaves the map of flights in progress, so a read that finds no flight finds the block read.
- When the usage answer carries no block, Claude Code's own reset read follows at once, as the sweep's fallback would. While the follow-up runs, the sweep starts no reset read of its own for the account. A refused reset read records the account's usage hold-off, as a refused usage read does, since both read the same endpoint for the same login (`holdUsageReads`). With the daemon's reset read off, such an answer counts as a failed read.
- Codex the same, through the same flight. A Codex read's count is now a reading of the block wherever the daemon reads the account (`fetchCodexUsageDetails`), and after the follow-up's reading the account's credit list is read at once with §91.1's reader (`readCodexCreditsNow`), not at its hourly cadence.
- Only a daemon that runs the follow-up pass arms one (`claimedResets.runs`, set while `RunClaimFollowUps` runs; it runs only with usage fetching on). On any other daemon the sweep keeps its own reset read of the account, as before.
- One follow-up per account, and one read of it at a time. It stops at the first reading of the block by any path, when the daemon drains, and two hours after the claim (`claimFollowUpGiveUp`). At two hours it logs `saved resets: no reading of the account's saved resets in the two hours since the claim; the daemon stops reading it for the claim` at Warn, with `account` (§87's label), `provider`, `kind`, `claimed_at`, `tries` and `last_error`.
- When its reading arrives, the usage-status snapshot is dropped, so the next row is built from the reading: whole, with no mark. The 10-second pass then settles §93.2's latch from that row, as §99 rules.

F. The first reading after a claim, whichever read takes it, logs `saved resets: the first reading after a claim was taken` at Info, with `account`, `provider`, `kind`, `claimed_at`, `read_at`, `after_seconds` and `read_by`. `read_by` is `claim` (the claim path's own read), `follow_up`, `sweep` (a usage read of the sweeps or the score refresh, a follow-up that joined one included), `fallback` (the sweep's hourly reset read) or `claim_route` (the claim route's status read, for a later claim or a dry run).

The views. While a row's block is from the claim (`resets_from_claim: true`):
- `srq pool` ends the account's reset marks with `^`, such as `F1 3/9^`, or `0^` when the claim used the last one. Its legend gains one line where a mark is drawn: `^ = known from the claim just made, not read since: the last block read with the reset it used counted as used; no automatic claim goes by it until a reading`. `srq resets` writes the count as `1^`, with the same line once above its table.
- The board draws `^` after the marks, and its legend says the same.
- `sr status`'s `1x reset` cell ends in `^`: `1 Fable 3/9^`, `1 Fable^` when narrow, `used^`. A Claude row the CLI read itself and completed from this machine's daemon takes the mark with the daemon's block.
- pool.json carries `resets_from_claim` on the row. `srq pool --json` carries `resets_from_claim` and `not_read_since_claim`, and `srq resets --json` carries `from_claim` on each entry.
A daemon from before §102 publishes no mark, and every view reads its rows as before.

How to read the mark. `^` means that the account's daemon knows its saved resets from the provider's answer to the claim, not from a reading: the last block it read, with the reset the claim used counted as used. No automatic claim goes by it (B), the hold of a used week reset is already gone (C), and the mark goes with the first reading.

The documents. `ops/OPERATING.md` (Saved resets: the `1x reset` bullet, the paragraphs after a claim, the `^` bullet, pool.json) and `ops/skills/subrouter/SKILL.md` say what an account's row shows right after a claim, what `^` means, that nothing is claimed on a block from a claim, that the hold of a used reset ends at once, and how and when the daemon reads the account again. `ops/docscheck/follow_up_docs_test.go` holds both to it.

Why. The owner asked (AR-25): "once a rate limit claims it should trigger an immediate usage check update". The provider's answer that it used the reset is a fact about the account, and the daemon held it in hand while its views said "not read" and its ranking said "holder" and "spent". A refused reading should say "not read since the claim", never stand in older figures. §102.1 makes the row and the ranking right without a reading. The immediate check is the claim path's own read, which §99 already makes; when the provider refuses it, §102.2 takes the reading as soon as the provider allows, never sooner, since a read inside the hold-off can only extend it.

How to turn it off. There is no switch of its own: it acts only after a claim the provider says it used, and a claim is made only when the owner's settings allow it (`SUBROUTER_AUTO_RESET`, §93) or by hand. The follow-up runs only with usage fetching on (`--fetch-usage=false` stops it). Going back is a rebuild.

What does not change. No usage read is added on the claim path: the follow-up is armed only after the claim path's own read has returned, and §99's `TestServingInPlaceAddsNoUsageRead` stays green and untouched. Shadow and off claim nothing, the owner's under-1-percent condition comes first, and never twice stays. `sr claude reset` and `sr codex reset` answer as before. Selection is unchanged for every account that made no claim: the record, the follow-up and the change to the sweep's reset read exist only for an account whose claim landed.

Tested in Docker, every provider a fake; no test claims or redeems a real reset or credit. In `internal/proxy/reset_from_claim_test.go`:
- `TestAClaimedWeekResetLeavesItsBlockOnTheRow` (1): real mode, a week reset claimed at the wall, the read after it refused with 429: the row carries one reset left, `remaining_by_kind` `{"fable":1}`, no `week_held_until`, `resets_from_claim`, and `resets` lists the Fable reset alone. The spare's row says nothing of a claim.
- `TestAClaimByHandAndACodexRedeemLeaveTheirBlockOnTheRow` (2): the same after a claim by hand through the claim route, in shadow mode; and after a Codex redeem by hand whose read after is refused, the count is 1 and the list holds the credit left, with its date.
- `TestAReadingOfTheBlockReplacesTheBlockAClaimLeft` (3): once a read succeeds the row is the reading's, unmarked. When Anthropic answers `reset` and still lists the grant, the reading (`{"week":1,"fable":1}`) stands and one Warn says so, not repeated on the next reading.
- `TestNoClaimGoesByTheBlockAClaimLeft` (4): after the week claim, a Fable wall decides `no_claim` with the reason of part B and sends nothing; after a reading, and past the 10-minute floor, the same wall claims the Fable reset.
- `TestTheHoldEndsWithTheWeekResetItWasFor` (5): an account at 80% of its week with a week reset ranks in early burndown and publishes its hold; after a claim by hand with the read refused it holds nothing, is in no burndown from the reset, `Explain` names neither, and its row publishes no hold. A claim answered `not_limited` keeps the hold.
- `TestTheChoiceIsNotMadeForTheResetAClaimUsed` (6): §95's pass chooses the account for its week reset; after a claim by hand whose read carries no block, the pass after it does not.
- `TestAFigureFromBeforeTheClaimIsNotShownOrRankedOn` (7): the week read at 100% before a claim at the reading moment, the read after it refused. The row shows no week and names `7d` in `not_read_since_claim`; the score has no week figure and is not exhausted; a new session is placed on the account, with the spare spent. A reply whose headers read the week at 5% puts it on the row.
- `TestARepliesFiguresFromBeforeTheClaimAreNotRankedOn` (7): a reply to a request admitted before a claim by hand, whose headers read the week at 95%, arrives after it; the account ranks on no week figure.
- `TestFiguresAThrottledAnswerCarriedBeforeTheClaimAreNotShown` (7): §99's case of the usage endpoint holding the account off at the claim, with the week read before it: after the claim the row shows no week figure and names the week as not read since the claim.
- `TestAFableFigureFromBeforeTheClaimIsNoReadingAfterIt` (9): a Fable reset claimed at a Fable wall, the Fable cache at 100% from a reply before the claim, the usage answer after it leaving the Fable bucket out: the claim is `not_taken` (`it carried no figure for the Fable meter`), the turn is served in place, and the row does not show the old figure.
In `internal/proxy/auto_reset_read_after_test.go`, §99's fixture of behaviour 8, now with a spare. In `ops/docscheck/latch_docs_test.go`, `TestPatchesSaysWhichRowsSettleALatchAsTheCodeDoes` (10). The CLI's wire type for a row learned both new fields (§47's `TestUsageStatusWireTypeHasEveryDaemonField`).

§102.2 is tested in Docker the same way. Its fake fails the test on any read of the account inside the provider's hold-off, and tests move the follow-up's clock and the recorded hold-off instead of waiting. In `internal/proxy/claim_follow_up_test.go`:
- `TestTheBlockIsReadAtTheProvidersDeadlineNotAnHourLater` (E, the daemon's own rule corrected): the usage answer after a claim by hand carries no block. The sweep starts no reset read of its own; the follow-up's reset read goes at its next pass and is refused with a retry deadline of 90 seconds; a sweep inside that hold-off reads nothing; one pass after the deadline the reset read goes again and the row is whole.
- `TestAFollowUpReadsTheAccountAtTheProvidersDeadline` (2): real mode, a week claim at the wall, the read after it refused with a retry deadline of 90 seconds, and a reply after the claim that keeps the cached usage young. Passes at 10, 45 and 85 seconds read nothing, and the sweep after the deadline is served the young cache. The pass at 100 seconds reads once, the row is whole with no mark, the sweep after it reads nothing, the latch settles, and one Info line names `follow_up`.
- `TestAFollowUpWaitsOutAHoldOffAnotherReadMet` (E): a sweep's read is refused after the follow-up came due. Passes at 10 and 60 seconds send neither read; one pass after the hold-off the reset read goes.
- `TestADaemonThatRunsNoFollowUpKeepsTheSweepsResetRead` (E): on a daemon that runs no follow-up pass, the sweep still sends its own reset read of a claimed account whose usage answer carried no block.
- `TestAnAnswerWithNoBlockToTakeBacksOff` (E): reset reads answered with Anthropic's `unavailable` go at 10, 40 and 90 seconds, not at every pass, and the row keeps the block from the claim.
- `TestAFollowUpAfterAFailedReadComesWithinThirtySecondsAndBacksOff` (3): the read after the claim fails on the network. The follow-up reads at 30 seconds, not at 10; then waits 40, 80 and 160 seconds; and the fifth read makes the row whole.
- `TestAFollowUpSharesTheSweepsRead` (3): a sweep that reads the account while the follow-up's read is in flight sends no second read, so one read serves both; and after a sweep's reading the follow-up reads nothing.
- `TestAFollowUpStops` (4): after a reading by another path, nothing more is read and no Warn comes at two hours; a draining daemon reads nothing, and nothing after the drain is resumed; at two hours nothing is read and one Warn names the account, and a later pass logs no second one.
- `TestTheFirstReadingAfterAClaimIsLoggedOnce` (5): the claim's own read takes the reading, and one Info line names `claim` with `after_seconds` 0; a later reading logs nothing.
- `TestACodexFollowUpReadsTheUsageAndTheCreditListAtOnce` (6): a Codex redeem by hand whose read is refused with a retry deadline of 90 seconds. Nothing is read at 60 seconds; at 100 seconds one usage read and, at once, one credit list read; the row holds one credit, the list read, unmarked.
- `TestPoolJSONMarksABlockFromAClaim` (7): pool.json marks the claimed account's row and not the spare's, and the mark goes with a reading.
In `internal/proxy/pool_ui_page_test.mjs` (run by `TestTheBoardPagePassesItsNodeTests`), the board draws `^` and `0^` and its legend explains them (7). In `cmd/subrouter/sr_reset_from_claim_test.go`, `TestTheResetCellMarksResetsKnownFromAClaim` (7): the cell, dated and narrow, Claude and Codex, from a daemon's rows and on a local row completed from this machine's daemon, and no mark on a row without one. `srq selftest` checks the `^` marks, `0^`, the legend line only where a mark is drawn, `--json`, `srq resets`' count and line, and that a daemon before §102 reads as before (7). `ops/docscheck/follow_up_docs_test.go`'s `TestDocsSayWhatARowShowsRightAfterAClaim` requires both documents to name `resets_from_claim`, `not_read_since_claim`, `` `^` ``, `no automatic claim goes by`, `hold ends`, the Info line's message, `read_by`, `hold-off`, `two hours` and §102.2 (8). §99's `TestServingInPlaceAddsNoUsageRead` passes untouched, counting 2 usage reads from the wall to the answer (9).

The new tests use names that do not exist before §102.2, so they cannot run on the code before it. Each part was taken out of the working tree in turn instead, one replacement each (two where a part has two guards), by a script outside the repository, and its test went red for the predicted reason. No follow-up armed after a Claude claim, the follow-up reading through the cache that replies keep young, and the daemon's own five minutes added after the provider's deadline: no read one pass after the deadline. Both hold-off guards taken out: the reset read went inside a hold-off a sweep met. The sweep's own reset read allowed while the claim waits: it went at once. No reset read after an answer without the block: none at the first pass. A refused reset read recording no hold-off: the sweep's usage read went inside it. No backoff: a read 60 seconds after the claim. A first wait of 40 seconds: no read at 30. A read beside the sweep's rather than through its flight: a second read while the follow-up's was in flight. The follow-up going on after a reading: a third read. Going on through a drain: a read after the drain was resumed. No end at two hours: a read at two hours. A follow-up armed on a daemon that runs no pass: the sweep's own reset read never went. An answer with no block to take read again at once: a reset read at 20 seconds. Codex: no follow-up after the redeem (no read), a Codex read through the flight taken as no reading, and the credit list left to its hourly cadence (a usage read, no list read). No Info line, no Warn at two hours, and every read named a sweep's: the log lines missing or naming the wrong read. The status snapshot kept after the follow-up's reading: the row showed no block. pool.json without the mark, the board without `^` (its Node test), the `1x reset` cell unmarked, the mark not read from a daemon's row, and not taken on a local row: each view's check. In `srq`, a copy each: the marks ignoring the claim, the legend line missing in `srq pool`, the count unmarked and the line missing in `srq resets`. The skill without when the reads stop, and the operator guide without `^`: the docs check. One first run of the Codex list's mutation did not compile and was run again with the value kept. The same tests with nothing taken out passed, and the follow-up's tests also ran with `-race`.

Run on the code before this change (`origin/main` as it stood, with the new tests copied in), every new test failed: the row carried no block (1, 2, 3, and 6 at its first check); a Fable wall's reason said only that the saved resets had not been read (4); the holder stayed in early burndown (5); the row read the week at 100% (7); the account ranked on the week a reply measured before the claim, and the row showed the week from before the claim during the hold-off (7); the Fable claim was judged still spent and the turn was moved to the spare (9); and the docs check found §93.2's old sentence (10). Behaviour 8's fixture passed there, since §99 is in it, and with §99's rule taken out (a turn served in place only on `back`) the first waiting turn was served on the spare. Then each part was taken out of the working tree in turn, one literal replacement each, by a script outside the repository, and its test went red for the predicted reason: the row overlay (the row carried no block); the claimed reset not counted (the row still listed the week reset, and §95's pass weighed the account for the week); the redeemed credit kept (the list held both credits); the Codex count not lowered; a usage read's block not taken as a reading (the row stayed the claim's after a reading); no Warn on a reading that disagrees; a decision going by the block from the claim (the Fable wall's reason did not name the claim); no score from the claim (the holder stayed in early burndown); the score from the claim keeping the hold; a sweep read before the claim scoring after it (the hold and the week came back); the score ranking on the week from before the claim (exhausted until the roll); a row read before the claim keeping its figures (the week at 100%, and during the hold-off from before the claim, on the row); replies sent before the claim observed (ranked on 95%); the Fable cache kept across the claim (the row showed the Fable meter at 100%); the read after the claim free to probe (the fake got a Messages probe the claim path never sent); and §93.2's old sentence put back (the docs check). The same runs with nothing taken out passed. The tests of §91 to §99 pass with no line changed but behaviour 8's fixture, and the tests that hold several turns also ran with `-race`.

I. §102's independent check, three findings and two notes. Part E cited lines of a tree from before a rebase; each citation now gives the line on this branch and the function or constant it is in, and `TestPatches102CitesOnlyCodeThatIsDeclared` (`ops/docscheck/follow_up_docs_test.go`) fails when a cited name is no longer declared in its file or a cited line names none. A Codex redeem whose own read after the consume was answered did not take the reading: `redeemAccountChecked` read the usage outside the path that calls `noteResetBlockRead`, so the row stayed marked, automatic Codex decisions on the account stayed refused by part B, and the Info line named whichever sweep read the account next. That read is now a reading, as the claim path's read is for Claude: no follow-up is armed, the Info line names `claim`, and at once its count replaces the block from the claim, on the row and in the decision. The record keeps the reading, and `blockOn` sets it, unmarked, on a Codex row that no read since the redeem measured, in `withResetsFromClaims` for every view and in `autoResetNextFor` for the decision: the snapshot the failover's redeem leaves in place for up to 30 seconds, and a sweep whose read is refused. A row that a read after the redeem measured carries that read's own count. It adds no request. Its second check found that the first version only took the mark away and the reading reached no row: after the failover's redeem the row showed the two credits from before it, unmarked; over a refused sweep it showed no count where it had shown `1^`; and a wall decided by the snapshot from before the redeem. A redeem whose read after fails is as above: the mark, then the follow-up. A read after that is answered with no credit count in it takes nothing and arms no follow-up, as before; the next sweep's read takes the reading. Two Codex tests compared a credit's date with one computed again from the clock at the assertion, to the second, and failed when a second turned between the two; each now takes the date from its own fixture (§74.1). The notes: part B had no Codex test, and for Codex only that rule stops a claim on a row that still lists a credit; and behaviour 6's fixture unchose the account by the worth floor whatever the block said. The Code list below no longer names `internal/proxy/usage_reset_awareness.go`, which §102 did not change.

Tested in Docker, every provider a fake. `TestACodexRedeemsOwnReadAfterIsTheReading` (`internal/proxy/claim_follow_up_test.go`): a redeem by hand whose read after is answered, then the provider refuses the account's reads. At once the row, from a sweep whose read is refused, carries the reading: one credit, credit-late with its date, unmarked. Follow-up passes at 30 and 100 seconds read nothing, one Info line names `claim` with `after_seconds` at most 5, and the next answered sweep shows the same. The provider got three usage reads after the consume, no credit list read and one consume, the same as on the code before this change. There, with the test copied in, the row stayed marked and the Info line named `sweep`. `TestAFailoverRedeemsReadingIsOnTheRowAtOnce` (`internal/proxy/claim_follow_up_test.go`): the failover's redeem leaves the snapshot from before it, with two credits, and its read after is answered; at once the row carries the reading. `TestASweepsReadingAfterARedeemIsTheRows`: a credit granted after the redeem, the next sweep reads two, and the row shows two. `TestACodexDecisionAfterARedeemGoesByItsReading` (`internal/proxy/reset_from_claim_test.go`): an account with one credit, redeemed by hand with its read after answered, then the snapshot from before the redeem put back as in the next test; a wall decides `no_claim` with `the account holds no reset credit`, by the reading, and lists no credits. On the first version of this change (the mark taken away only) the first row had no count, the failover's row showed two credits, and the wall listed the credits and decided by the snapshot's one credit; on the code before part I both rows carried the block from the claim, marked, and the wall refused by part B. With the reading set on no row the first two failed, with the decision reading the row alone the third, and with the reading set over a row measured after the redeem the fourth (one left where the sweep read two). The checker's three probes, run on this change: after the failover's redeem the row shows one credit, unmarked; over a refused sweep, one credit, unmarked; and on the snapshot from before the redeem a wall in real mode claims credit-late, by the reading's one credit and the credit list it then reads, as it would after any reading that leaves a credit. `TestNoCodexClaimGoesByTheBlockARedeemLeft` (`internal/proxy/reset_from_claim_test.go`): after a redeem by hand whose read is refused, the snapshot holds the row read before the redeem, with two credits, as the failover's redeem leaves it; the failover does not redeem in real mode, so the test puts that snapshot back by hand. A wall on the account in real mode decides `no_claim` with part B's reason and sends no consume; with part B's rule taken out it redeemed credit-late, a second consume. `TestTheChoiceIsNotMadeForTheResetAClaimUsed` now runs on a pool that burns its week at 3.0 a week, so a week reset is worth claiming at the account's next wall even on the fresh week a claim gives back; with the claimed reset not counted as used, the pass after the claim chose the account for its week, a claim adding 0.79 of a week. The docs check failed with one cited name changed to one the file does not declare, with the cited function renamed in the code, and with part E as it stood before.

Residuals. The score refresh republishes a Codex row's windows from its read but not its count (`refreshUsageStatusRowsFromWindows`, as before §102), so after the failover's redeem the re-score that follows shows the count from before the redeem, unmarked, until the next sweep, at most 30 seconds later; on the code before part I it did the same. The record and its follow-up live in memory, so a restart forgets them; the next reading then speaks, and until it does the restored snapshot shows what it shows. A score from the claim keeps the reading's other figures (the five hours) at the time they were measured. A Claude claim that may have landed and names what it cleared drops the marks and reads again as before, but makes no record and no follow-up: only `result: reset` does. A meter whose usage answer never carries it and whose account takes no turn stays named in `not_read_since_claim` until a probe or a reply measures it; the follow-up waits for the block, not for every meter. A 429 that names no deadline still holds the account off for the daemon's own five minutes. The Codex usage-status sweep reads every Codex account live on each sweep, whatever a hold-off (as before this section): after a redeem whose read was refused, a status read inside the hold-off reads the account again, and the follow-up's own reads keep to the deadline. A follow-up read that starts in the instant after a sweep's flight left the map, and before its own check, can still send a second read; the block is noted before the flight leaves the map, so that window is the time between one lock and the next. Not measured: how long Anthropic's usage endpoint holds a login off after a claim, what `Retry-After` it sends, and whether a read at that deadline is answered. Which of E's paths kept the live block unread is read from the code, not from that daemon's logs.

- Knobs: none new. The constants are `claimFollowUpRetry` (20 seconds), `claimFollowUpMaxWait` (10 minutes), `claimFollowUpGiveUp` (2 hours), `claimFollowUpTimeout` (30 seconds) and `claudeResetReadHold` (5 minutes) in `internal/proxy/claim_follow_up.go`, and `claimFollowUpScanInterval` (10 seconds) in `cmd/subrouter/main.go`.
- Code: `internal/proxy/reset_from_claim.go` (`claimedReset`, `noteResetClaimed`, `forgetFableBeforeClaim`, `noteResetBlockRead`, `blockOn`, `withResetsFromClaims`, `withReadAfterClaim`, `readAfterClaimMissing`, `claimedBefore`, `claudeBlockAfterClaim`, `codexBlockAfterRedeem`, `claudeGivenBack`, `codexGivenBack`), `internal/proxy/claim_follow_up.go` (`claimFollowUp`, `armClaimFollowUp`, `RunClaimFollowUps`, `claimFollowUpPass`, `followUpClaim`, `readClaudeResetNow`, `holdUsageReads`, `usageHoldUntil`, `readCodexCreditsNow`, `blockReadPath`), `internal/proxy/claude_reset.go` (`claudeReset`, `afterClaudeReset`, `readClaudeResetStatus`, `fetchClaudeResetFallback`, `claudeResetFromFallback`), `internal/proxy/proxy.go` (`AccountUsageStatus.ResetsFromClaim`, `NotReadSinceClaim`, `fetchUsageWindowsShared`, `fetchClaudeOAuthUsageLive`, `fetchCodexUsageDetails`, the Codex sweep row, the score builds, `redeemAccountChecked`), `internal/proxy/held_resets.go` (`withHeldResets`, `forgetCredit`), `internal/proxy/auto_reset.go` (`autoResetNextFor`), `internal/proxy/claude_response_evidence.go` (`observeClaudeQuotaHeaders`), `internal/proxy/pool_ui.go` (`poolRow.ResetsFromClaim`), `internal/proxy/pool_ui.html` (`resetMarks`, the legend), `cmd/subrouter/main.go` (`RunClaimFollowUps`), `cmd/subrouter/sr_server.go` (`remoteServerUsageStatus`, `usageRowsFromServerUsageStatuses`), `cmd/subrouter/sr.go` (`srUsageRow.resetsFromClaim`, `resetFromClaimCell`), `cmd/subrouter/sr_provider_reset.go` (`fillResetsFromLocalDaemon`), `ops/bin/srq` (`_held_resets`, `_reset_marks`, `RESETS_FROM_CLAIM_LEGEND`, `pool_resets`, `_render_resets`)
- Tests: `internal/proxy/reset_from_claim_test.go`, `internal/proxy/claim_follow_up_test.go`, `internal/proxy/pool_ui_page_test.mjs`, `internal/proxy/auto_reset_read_after_test.go` (behaviour 8's fixture), `cmd/subrouter/sr_reset_from_claim_test.go`, `ops/docscheck/latch_docs_test.go`, `ops/docscheck/follow_up_docs_test.go`, `srq selftest`
- Docs: `ops/OPERATING.md` (the hold, the resets list, the `1x reset` bullet, the reads after a claim, `^`, pool.json), `ops/skills/subrouter/SKILL.md`, `ops/KNOWN-ISSUES.md` (Residual gaps §102 leaves open)
- Related: amends §56 and §64 (the hold of an account whose reset was used), §91 (the block and list a claim leaves; the Codex list read at once after a redeem's follow-up), §46.1 (the daemon's own reset read waits while a follow-up reads the account), §93.2 (the sentence on settling a latch), §99 (the read after a claim; the two findings of its independent check). Builds on §46 and §92.1 (the claim functions every path takes).

## 103. No automatic claim inside a day of the roll, but in a reset's last chance, and a Fable reset only with room on the week

Superseded in part by §105: the Fable week room (decision 2a, §103.1 item 3, `week_room` and `week_left`) is gone, from the decision, §95's choice and the claim's last check. The 24-hour gate, the unread roll and the last check on the roll stand as written here.

The owner, 2026-11-24, after a day of real mode (§93 to §96, §99): "the auto-trigger is way too eager to fire when a session is almost burned out", "we had code that was being reasonably smart about how much of the week was left", and "it shouldn't be firing automatically for anything with under 24 hours left". And for Fable: "because we have more time to use those, we should be prioritizing chats where we're out of Fable but still have some Opus". The owner's decisions: (1) no automatic claim when the meter the next reset gives back rolls in under 24 hours, except the existing last chance; the expiry window does not pass this. (2a) a Fable reset is claimed automatically only on an account whose Fable meter is out and whose week still has real room, never on one nearly out of its week too. Real mode stays on. (2b), routing other models onto accounts out of Fable, is §104.

What happened, in invented names. At 08:37Z one box claimed account A's Fable reset, which keeps until March. The reading said the Fable meter was 100% used and the week 97% used, and the roll was 6h23m away. §94.2 let it through on value: the account's own 24-hour rate was 7.07, in a pool whose Fable pressure was 1.05, so a claim "adds 0.27 of a week" (7.07 x 6.4 / 168), just over `resetValueFloor` 0.25. The claim gave back Fable 100% to 0% and the week 97% to 47%, and the roll stayed where it was. The day before, two week resets were claimed 18 and 20 hours before their rolls. An account's own rate after hours at its limit can be large, so the value rule alone lets almost any claim through near the roll.

§103.1 The gate: one predicate, three places.

`resetHoldOf` (`internal/proxy/reset_worth.go`) is the one predicate. It takes the meter the reset gives back, the time from the account's limit to that meter's roll, whether the roll is read, the reset's last chance (`lastChance`, §94.1), and the account's week (`resetWeek`: what is left, or why that is not known). In order:
1. A roll not read: no claim (`roll_unread`), since nothing says the roll is a day away. Before §103 the expiry window claimed with the roll unread; now it does not.
2. A roll under `ResetRollWaste` (24 hours) away, or none past the limit: held back (`roll_gate`), unless it is the reset's last chance.
3. A Fable reset whose account's week has under `resetFableWeekRoom` (0.25) left: held back (`week_room`). Its week not read: no claim (`week_room`). Last chance does not pass this: the owner said "never".

`ResetRollWaste` is §46.11's constant, moved from `cmd/subrouter` (`providerResetWeeklyWaste`) to the proxy package and exported, so `sr claude reset` asking first inside a day of the roll and the daemon never claiming there by itself read one value. `resetFableWeekRoom` is one value for the decision, the choice and the last check. Each carries a dated comment with the owner's words.

The three places:
- The decision (`decideWorth`). After the unknown-clock step and before everything else: the expiry window, last chance, need and value. The week is the decision's own reading (`autoResetWeekOf`, through `autoResetMeterLeft`): nothing left when the wall's refusal was on the week, else a reading at most 5 minutes old (`autoResetReadingMaxAge`). The toRoll is from the decision's clock, which is the account's limit: the wall, or a reading at 99.5% or more.
- §95's choice (`resetPool.weigh`). The gate is judged at the wall the account is heading for (`projectedWall`), or from now when that wall is not known. A holder whose claim there the gate would hold is not chosen, inside the expiry window too, so nothing is loaded toward a claim that cannot happen. Outside the window the pool's need is read first, so a choice that stops on a pause in need alone still keeps its place (§96). The week room reads the account's week as its row has it now. Last chance is now judged whenever the roll is read, not only when the wall is known.
- The claim's last check (`autoResetFacts.holdBeforeSend`). Just before a real claim sends, the gate is judged again, at the daemon's clock, on the decision's roll and week, each replaced by the claim's own read when that read is newer than the decision's reading. A Claude claim calls it from the request's `check`, which now runs at both moments: `ClaudeResetRequest.liveCheck` became `check` (judges the read the claim goes by, live or the cached block) and `readLive` (set at a reading moment, as before, so a claim at the wall reads nothing more). A Codex redeem calls it from its `approve`, on the live usage read under the redeem lock. A hold there sends nothing; the reason reads `nothing was sent: §103 holds it on the reading before the claim: <why>`, and the verdict is `failed`, as for the other last checks.

With `SUBROUTER_AUTO_RESET_WORTH=0` the decision is §93's and carries no worth, so the gate is off in the decision and in the claim's last check (`holdBeforeSend` holds nothing without a worth). §95's choice keeps it: the choice never reads that switch, and it keeps §94's value rule with it off too, so it chooses and loads no account the gate would hold even while the decision would claim on it. `SUBROUTER_RESET_LOADING=0` turns the choice off.

§103.2 What an operator sees.

- The rule's steps (`AutoResetRuleStep`) gain `roll_gate` and `week_room`. The rule (`AutoResetRule`) gains `week_left`, the share of its week the account had left, on a Fable reset's decision whose week was read; it is absent otherwise.
- Reasons: `held back: the Fable meter rolls in 6h23m, under 24h, and gives itself back then; that near the roll only a reset's last chance is claimed`; `held back: the week has 3.0% left, under 25%: a Fable reset is claimed only on an account out of Fable that still has week left`; `the week is unread, and a Fable reset is claimed only on an account whose week is read and has at least 25% left, so nothing is claimed`; `the roll of the week is not read: neither what a claim adds nor whether the roll is a day away is known, so nothing is claimed`.
- A choice's reason: `..., but at the pool's whole demand it reaches its limit in 5h2m; a claim of its week reset at its limit would be held: the week rolls in 19h58m, under 24h, ...`, and the stop line of a chosen account carries it.
- `srq pool`: `auto=held back Fable (rolls in 6h, under 24h)`, `auto=held back Fable (week 3% left, under 25%)` and `auto=no claim (week not read)`; the legend names §103; `--json` adds `auto_reset_week_left` (`_auto_reset_rule`, `_held_back_why`, `_auto_reset_label`). The board prints no rule step; a choice's title is its reason, which says the gate held it. A held-back example under a day from the roll, `(adds 0.07 wk, rolls in 10h)`, can no longer happen; the docs now show `(adds 0.21 wk, rolls in 30h)`.

§103.3 Tests, all in Docker, every provider a fake.

`internal/proxy/reset_gate_test.go`:
- `TestTheFableClaimSixHoursBeforeTheRollIsHeld`: the 08:37 claim on facts (Fable reset, 6h23m, own rate 7.07, pool 1.05, week 3% left) is held back by `roll_gate`; its rule still shows 6.4 hours, the value 0.269 and `week_left` 0.03.
- `TestNoResetIsClaimedInsideADayOfItsRoll`: week resets 18 hours and 23h59m before the roll, at an own rate of 3.0, are held; at 24 hours one claims (0.43); a Codex credit 20 hours before the roll is held by the gate.
- `TestOnlyLastChanceClaimsInsideADayOfTheRoll`: last chance 6 hours before the roll claims; the expiry window open 6 hours before the roll, not in its last chance, is held; window and last chance together claim; the window with the roll unread claims nothing.
- `TestAFableResetIsClaimedOnlyWithRoomOnTheWeek`: a Fable reset 48 hours before the roll is held with the week 97% used, claims with it 50% used (0.34) and at exactly 25% left, claims nothing with the week unread or read 7 minutes before; a week reset has no such bar.
- `TestTheChoiceLoadsNothingTowardAClaimTheGateWouldHold`: a week holder whose roll is 19h58m past its projected wall is not chosen (and was, on value 0.30, before); 24h58m past it, it is; in its last chance it is; inside an open expiry window a holder 14h48m from its roll past its wall is not chosen while the other three holders are; a Fable holder with 10% of its week left is not chosen, one with 60% is.
- `TestTheClaimsLastCheckJudgesTheGateAgain`: a decision 24h1m before the roll passes at its own time and is held two minutes later; a newer reading of a roll 23 hours away holds it, an older one is not read; a Fable account whose week fell to 10% on a newer reading is held; with the rule off nothing is held.
- `TestARealClaimIsHeldWhenTheLiveReadingPutsTheRollUnderADay`: real mode at a reading, through the fakes of §93.2. A Claude week reset decided 48 hours before the roll claims; when the live read before the claim says 23 hours, no claim reaches the fake. The same for a Codex credit decided six days before the roll.
- `TestARealClaimAtTheWallIsHeldWhenItsReadPutsTheRollUnderADay`: real mode at the wall. The cached block does not offer the grant, so the Claude claim reads the account; a week reset decided 48 hours before the roll claims in place, and when that read says 23 hours, no claim reaches the fake and the turn is served on the other account.

Changed for the new behaviour: `TestTheRuleClaimsOnlyWhenThePoolNeedsItAndItAddsEnough` and `TestEachDecisionCarriesWhatTheRuleUsed` (10 hours to the roll is now `roll_gate`), `TestLastChanceAndTheExpiryWindowOverrideTheRule` (the window no longer overrides 10 hours to the roll), `TestTheDaemonDecidesByThePoolsPressureAtTheWall` (the own-rate case moved to 25 hours with rates 0.6 and 2.4, so it still claims on the account's own rate alone; 15 hours is now held), `TestUsageStatusCarriesWhatTheRuleUsed`, and `TestWeekHoldersAreOrderedByTheValueAtTheirWall` (the holder 18 hours from its roll past its wall is held by the gate, no longer by value; the Codex holder `c60` still covers the value floor). `srq selftest` covers the four new labels, the legend and `auto_reset_week_left`. `ops/docscheck/roll_gate_docs_test.go` requires the gate's names in the operator guide and the skill, and fails on a held-back-by-value example under 24 hours from the roll anywhere but here.

Each new test was run with the gate taken out of a copy of the tree (`resetHoldOf` returning no hold), and again with only the last check taken out (`holdBeforeSend` returning nil), and with the Claude claim's check judging the gate only at a reading moment: each went red for the predicted reason, as recorded in the branch's notes.

§103.4 How to go back.

The constants are `ResetRollWaste` (24 hours, shared with §46.11) and `resetFableWeekRoom` (0.25). `SUBROUTER_AUTO_RESET_WORTH=0` and a restart turn the gate off in the decision and in the claim's last check, together with §94's rule; that is the owner's call. §95's choice keeps the gate whatever that switch says, as it keeps §94's value rule; `SUBROUTER_RESET_LOADING=0` and a restart turn the choice off. To remove §103 alone, revert its commits: the decision returns to §94.2's order, §95 chooses as before, and the claims' last check is the reading-moment check of §94.2 again.

- Code: `internal/proxy/reset_worth.go` (`ResetRollWaste`, `resetFableWeekRoom`, `resetWeek`, `resetHold`, `resetHoldOf`), `internal/proxy/auto_reset.go` (`AutoResetByRollGate`, `AutoResetByWeekRoom`, `AutoResetRule.WeekLeft`, `autoResetWeekOf`, `autoResetWorth.rate`, `autoResetWorth.hold`, `decideWorth`, `autoResetFacts.holdBeforeSend`), `internal/proxy/reset_choice.go` (`resetPool.weigh`), `internal/proxy/auto_reset_claim.go` (`autoResetClaimClaude`, `autoResetClaimCodex`), `internal/proxy/claude_reset.go` (`ClaudeResetRequest.check`, `readLive`), `cmd/subrouter/sr_provider_reset.go` (`resetWasteReasons`, `printClaudeResetPreview`), `ops/bin/srq`
- Tests: `internal/proxy/reset_gate_test.go`, `internal/proxy/auto_reset_worth_test.go`, `internal/proxy/auto_reset_worth_view_test.go`, `internal/proxy/reset_choice_test.go`, `internal/proxy/auto_reset_claim_test.go` (the fakes' `roll` and `resetAfter`), `ops/docscheck/roll_gate_docs_test.go`, `ops/docscheck/reset_worth_docs_test.go`, `srq selftest`
- Docs: `ops/OPERATING.md` (the decision's order, the held-back lines, `rule`, the last check, accounts chosen, the settings table), `ops/skills/subrouter/SKILL.md`, `ops/KNOWN-ISSUES.md` (Residual gaps §103 leaves open)
- Related: amends §94.2 (the decision's order; the expiry window with an unread roll), §95.1 (which accounts are chosen), §46.11 (its constant now lives in the proxy package). §104 routes other models' sessions onto accounts out of Fable with week left. Superseded in part by §105 (the Fable week room).

## 104. An Opus or Sonnet turn ranked across the peers alone reads their Fable meters too

The owner, 2026-11-24: route Opus and other non-Fable sessions onto accounts that are out of Fable but still have week left, so Fable headroom elsewhere is kept for Fable sessions. §65 already does this as a factor of 1 to 2 on an Opus or Sonnet turn's weekly pace (`FableSpentFactor`), and a peer publishes each account's Fable meter (`fable_week_spent`) for it. The question for §104 was how much stronger §65 must be. Measured, its strength was not what failed: on the laptop it never ran.

The defect. A daemon whose own pool cannot serve a turn ranks the peers' accounts alone (`serveAcrossPeers`, a recompute with no local ranking, the transport stage, a refugee's peers). That ranking starts from `peersOnlyScheduler`, which was `selectacct.NewScheduler(nil)` with the start marks and was never scoped to the turn's model (`ForModel`). So `nonFableClaudeTurn` was false there, and §65's factor applied to no turn ranked that way. The laptop's own account sits in its owner's holdout most of the time, so this is the laptop's usual path. §65's peer test built its own scoped scheduler (`TestALaptopRanksAPeerAccountOnItsPublishedFableMeter`), not the one the handler uses, so nothing caught it.

What was measured, over the 72 hours to 2026-11-24 10:45Z, from the three daemons' logs, their `usage-history.jsonl` and `token-usage.jsonl`, and the laptop's transcripts (each turn's model and time only):
- The laptop logged 378 placements. 287 of them (76%) ranked the peers alone. Of the 100 Opus or Sonnet placements whose model the transcripts give, 84 ranked the peers alone, with §65 off. The other daemon that places sessions, the box with accounts of its own, ranked all 422 of its placements with a local candidate, where §65 applies.
- Those 100 were decided by the drain in 56 (both draining 28, the roll window 23, an early burndown 5), by a choice (§95.2) in 18, by the weekly pace clause, where §65 lives, in 23, and by other clauses in 3.
- The meter history starts at 2026-11-23 10:46Z, so meters are known for the last 24 hours only. In that day no account had its Fable meter at 95% or more with at most 80% of its week used. One account had Fable at 72% with 40% of its week used all day. It took no new work, because chosen and draining accounts rank above every steady account. Many accounts sat at 100% of their week with their Fable meter between 8% and 84%: the week binds first.
- Fable headroom never ran out. At every 10-minute sample of that day, 16 to 20 accounts had their Fable meter under 95% and their week under 98%. No daemon logged `substituting claude opus for exhausted fable pool` in the 72 hours. The laptop's 38 `claude opus substitution unavailable` lines in the window are its own pool with no account to serve. Those turns then went to a peer. §71 served a subagent's Fable turn as Opus 66 times on 2026-11-21 and 11-22, and none since.
- Demand, from `token-usage.jsonl`: 389,876 Opus requests, 21,920 Fable, 25,128 Haiku and 53 Sonnet. Fable is about 5% of the requests and 9% of the output tokens.
- Every one of the 70 laptop placements made on a Haiku turn was a session whose main thread ran Haiku throughout. None was an Opus or Fable session that opened with a Haiku call.

The decision, from that evidence. §65 stays a factor at 2x, and is not made a tier. In the last day no account was Fable-spent with week left for it to prefer, Fable never ran short, and Fable is a small share of the demand. A tier among steady accounts would send every Opus session past an account that rolls in 24 to 48 hours with week left, which the pace already prices. What changes is its reach: the ranking of the peers alone now knows the turn's model.

The change. `peersOnlyScheduler` returns the same scheduler scoped with `ForModel(poolModel)`. It holds no score of this daemon's, so the scoping sets only the turn's class. An Opus or Sonnet turn ranks each peer account at `FableSpentFactor`, from the meter its owner published, as this daemon's own selection already does. A Haiku turn and a Codex turn are scoped too, which changes nothing for them. A Fable turn is left unscoped: scoping it would also load a peer's Fable choice for it (§95.2, `Scheduler.loads` reads the Fable pool flag), which this path has never done. That would move Fable sessions at once, while Fable choices are published, so it is left for the owner (KNOWN-ISSUES, *Residual gaps §104 leaves open*). The owner's words are marked in the code with their date, at `peersOnlyScheduler` and at §65's factor in `rankingPace`.

Where it ranks. The factor stays in the comparator's weekly pace clause. Above it, in order: machine policy, the selection tier, capacity marks, the new-session test, machine bindings, an unstarted week (§86), the drain and its cap (§41, §56, §72), and the chosen accounts and their cap (§95.2). So it cannot pull a new session away from an account in its last day with week left, or from a chosen one. Ranking never moves a warm session: only a new, cold or forced placement ranks. Holdouts, reserves, `pin_only` (§98), refugee caps and admission read the verdicts, which §104 does not touch.

What did not change. A turn with a local candidate was already scoped. Verdicts, eligibility, allowances, the board and every selectacct ranking are unchanged, so §72's endgame simulation, the paced trade table and §95.2's Codex simulation run the same code. `SUBROUTER_FABLE_SPENT_PREFERENCE=0` (§65) still ranks every Opus or Sonnet turn at 1x, this path included. No new switch: §65's covers it.

Tested in Docker, in `internal/proxy/opus_onto_fable_spent_test.go`, through the handler of a laptop with no account of its own and a box that publishes its verdicts. The box now publishes `fable_week_spent` and `reset_choice` as an owner does (`contractBox.usageBody`).
- `TestAnOpusTurnRankedAcrossPeersAloneGoesToTheFableSpentAccount`: two box accounts with the same published numbers, the one with its Fable meter spent sorting second. An Opus turn and a Sonnet turn each go to the spent account, and the decision line names `higher weekly pace after the Fable-spent preference and the live-session discount`. With the change taken out, both went to the other account by the ID tie-break.
- `TestTurnsTheFableSpentPreferenceDoesNotCoverRankAcrossPeersAsBefore`: on the same box, a Fable turn and a Haiku turn with the preference on, and an Opus turn with it off, go to the account the ID tie-break gives.
- `TestTheDrainAndAChoiceStillTakeAnOpusTurnBeforeAFableSpentAccount`: a box account in its roll window with 10% left, and one chosen for its week with 20% left and 140 hours to its roll, each take the Opus session ahead of a Fable-spent account whose doubled pace is more than twice theirs, for the drain clause and for the choice.
The last two pass with the change taken out as well: they hold what §104 must not change. §65's tests in `selectacct/fable_spent_test.go` and `internal/proxy/fable_spent_test.go`, §71's `internal/proxy/subagent_fable_pin_test.go`, §95.2's `selectacct/reset_choice_test.go` and `internal/proxy/reset_loading_test.go`, and the whole `selectacct`, `internal/proxy` and `cmd/subrouter` packages pass.

- Knobs: none new. `SUBROUTER_FABLE_SPENT_PREFERENCE` (§65) turns the preference off everywhere.
- Code: `internal/proxy/remote_pool_decide.go` (`peersOnlyScheduler`), `selectacct/scheduler.go` (`rankingPace`, the dated comment only)
- Tests: `internal/proxy/opus_onto_fable_spent_test.go`, `internal/proxy/crosspool_decider_test.go` (`contractBox.usageBody`), `ops/docscheck/fable_spare_docs_test.go`
- Docs: `ops/OPERATING.md` (The CLI's "protected" is not the router's gate, Settings), `ops/skills/subrouter/SKILL.md` (Explain why the router picked an account), `ops/KNOWN-ISSUES.md` (Residual gaps §104 leaves open)
- Related: completes §65's reach on the peer path; leaves §65's legacy `bestPeerScoreFrom` path inert, as §65 says; leaves §95.2's Fable choice unloaded for a Fable turn ranked across the peers alone, as before; §71 is unchanged.

## 105. A Fable reset does not wait on the account's week

The owner, 2026-11-24, of the automatic Fable reset: "it should not matter how much Opus is left. It's just: do we have a Fable reset, is Fable out, and is the week in a reasonable time?" The burn-down toward that claim, §95's loading, should not depend on the week either. So §103's Fable week room is removed. Its 24-hour gate stays: that is "the week in a reasonable time".

§105.1 What changed.

`resetHoldOf` (`internal/proxy/reset_worth.go`) keeps §103's roll cases exactly: a roll not read is no claim (`roll_unread`), and a roll under `ResetRollWaste` (24 hours) away, or none past the limit, is held back (`roll_gate`) unless it is the reset's last chance. The two week-room cases are gone. A Fable reset on an account with under 25% of its week left is no longer held back, and one whose week is not read, or was read more than 5 minutes ago, is no longer refused. The predicate takes no week at all. The owner's words are on it with their date, and `resetFableWeekRoom` went with its own dated comment, since the owner removed that rule.

In the three places §103 put the gate:
- The decision (`decideWorth`) reads no week: `autoResetWeekOf` is gone. A Fable reset at its Fable wall, or on a reading at 99.5% of its Fable meter, goes on to the expiry window, last chance, need and value whatever the week reads.
- §95's choice (`resetPool.weigh`) judges the gate on the roll alone, so an account low on its week is chosen for its Fable reset like any other.
- The claim's last check (`autoResetFacts.holdBeforeSend`) still takes the claim's own read of the roll when it is newer than the decision's, and holds a roll that came under 24 hours. A newer reading of the week no longer holds a Fable claim.

Removed with them: the type `resetWeek`, the `week` parameters of `resetHoldOf`, `autoResetWorth.hold` and `decideWorth`, the rule step `AutoResetByWeekRoom` (`week_room`), and the rule's field `AutoResetRule.WeekLeft` (`week_left`), which nothing else read.

§105.2 What did not change.

- The owner's under-1% condition: a reset is claimed only when the meter it gives back has under 1% left. For a Fable reset that is the Fable meter: Fable is out.
- The pool-need step and `resetValueFloor`, the expiry window and last chance (§94).
- The next-grant rule: only the grant Anthropic names next (`next_grant_id`) is ever claimed.
- §103's 24-hour gate, its unread-roll step, its place in the decision before everything else, and the last check of the roll before a claim sends.
- §104's ranking of Opus and Sonnet turns onto accounts out of Fable.

§105.3 What follows from it.

- An account low on its week may now be loaded with Fable work (§95) and have its Fable reset claimed once its Fable meter runs out. If its week runs out before its Fable meter does, it simply reaches its week wall with the Fable reset still held. No reset is spent there: the claim needs Fable out, and a chosen account at its limit takes no new session.
- A Fable reset behind a week reset waits for the week reset to be used (the next-grant rule). The account's next claim spends the week reset, and the condition holds that until the week has under 1% left; §95 never chooses such an account for Fable.

§105.4 What an operator sees.

- No rebuilt daemon gives `week_room` or `week_left`. A Fable reset near its roll still reads `auto=held back Fable (rolls in 6h, under 24h)`.
- `srq pool` still reads a `week_room` decision from a daemon built before §105, for the rollout: `auto=held back Fable (week under 25%)` and `auto=no claim (week not read)`. It no longer reads `week_left`, `--json` drops `auto_reset_week_left`, and the legend's §103 clause names the roll alone.

§105.5 Tests, all in Docker, every provider a fake.

`internal/proxy/reset_gate_test.go`, on `fableOutLowWeek`: an account whose next reset is a Fable reset, its Fable meter read a minute ago with nothing left and rolling in 3 days, its week 98% used, the pool's Fable need yes at 1.2, and a claim adding 0.51 of a week.
- `TestAFableResetIsClaimedWhateverIsLeftOfTheWeek` replaces `TestAFableResetIsClaimedOnlyWithRoomOnTheWeek`: the decision would claim, by `value`, with the week 98% used, not read, or read 7 minutes before, as it does with the week 50% used.
- `TestTheChoiceLoadsAFableHolderLowOnItsWeek` replaces the Fable half of `TestTheChoiceLoadsNothingTowardAClaimTheGateWouldHold`: a Fable holder with 2% of its week left is chosen for Fable, as one with 60% is.
- `TestTheClaimsLastCheckDoesNotHoldAFableResetOnALowWeek` replaces the week case of `TestTheClaimsLastCheckJudgesTheGateAgain`: a Fable claim decided with half the week left is not held by a newer reading of 2% left.

`TestTheFableClaimSixHoursBeforeTheRollIsHeld` no longer checks `week_left`; the other roll-gate tests are unchanged. With §103's code put back under these tests (`reset_worth.go`, `auto_reset.go` and `reset_choice.go` from before §105), each of the three new tests failed on the week room (held back at 2%, no claim with the week unread or stale; §95's `would be held: the week has 2.0% left, under 25%`; the last check's `§103 holds it on the reading before the claim: the week has 2.0% left`), and every roll-gate test passed. `srq selftest` covers the older daemon's labels and `--json` without `auto_reset_week_left`. `ops/docscheck/roll_gate_docs_test.go` requires the operator guide and the skill to name §105 and the owner's words, and fails on either one still saying that a Fable reset waits for a quarter of the week.

§105.6 How to go back.

There is no switch. Reverting this section's commits brings the week room back in all three places. `SUBROUTER_AUTO_RESET_WORTH=0` and a restart still turn §103's whole gate off in the decision and the claim's last check, as §103.4 says.

- Code: `internal/proxy/reset_worth.go` (`resetHoldOf`), `internal/proxy/auto_reset.go` (`AutoResetRule`, `AutoResetRuleStep`, `autoResetWorth.hold`, `decideWorth`, `autoResetFacts.holdBeforeSend`), `internal/proxy/reset_choice.go` (`resetPool.weigh`), `ops/bin/srq`
- Tests: `internal/proxy/reset_gate_test.go`, `ops/docscheck/roll_gate_docs_test.go`, `srq selftest`
- Docs: `ops/OPERATING.md` (the decision's order, the held-back lines, `rule`, the last check, accounts chosen, the settings table), `ops/skills/subrouter/SKILL.md`, `ops/KNOWN-ISSUES.md` (the gaps of §103's week room resolved; Residual gaps §105 leaves open)
- Related: supersedes §103's Fable week room (decision 2a, §103.1 item 3); amends §95.1 (which accounts are chosen). §104 is unchanged.

## 106. The Go module is `github.com/discordwell/subrouter`

The owner, 2026-11-24: agents and Go tooling that look for subrouter should land on this fork, not on `manaflow-ai/subrouter`, which has merged nothing since 2026-10-25. The owner chose to rename the Go module and to point the docs at the fork. Keeping upstream merges easy is not needed.

§106.1 What changed.

- `go.mod` names `module github.com/discordwell/subrouter`. A script rewrote all 1,341 imports of the old path, in 642 `.go` files, and gofmt sorted them. The line in §106.3 gives the same tree. The two constants that name the main package, `subrouterMainPackage` and `hostModulePath`, were renamed with them. `ops/go.mod` was already `github.com/discordwell/subrouter/ops` and requires nothing from the main module.
- The other places that name the module as a build path: the `-X .../internal/buildversion` flags in `Dockerfile` and `scripts/build-release.sh`, the examples in `internal/buildversion`'s package comment and `ops/LAPTOP.md`, and the prefix that `ops/bin/sr-local-ci` strips from package names in its report. `sr host`'s source install, which is not dispatched, builds the new path.
- `install-daemon` replaces an existing `sr` or `cx` only when it is Subrouter's. A binary built before the rename, upstream's releases among them, names `github.com/manaflow-ai/subrouter/cmd/subrouter`, and it still counts (`isSubrouterMainPackage`).
- `AGENTS.md`: the `gh` and `glaeda-gh` commands name `discordwell/subrouter`. This repository has no merge queue, so `gh pr merge` takes `--rebase` and lands at once. The person an agent hands off to is the owner.
- `README.md`: the install section builds from source and points to `ops/GETTING-STARTED.md`. No command in it fetches upstream's release, CLI autoupdater, npm or PyPI package. The Linux service points to `ops/deploy/lightsail/`.

§106.2 What did not change.

- What names upstream itself: the README's link to the project this forks, this file's history, and the comments on ported upstream code. The code and scripts that fetched or verified upstream's releases kept upstream's name until §107 pointed them at this fork's releases and deleted the npm and PyPI wrappers.
- The launchd labels, `ai.manaflow.subrouter` and the rest.

§106.3 Merging upstream after this.

Upstream's code imports `github.com/manaflow-ai/subrouter/...`. Rename its imports before it builds here, from the repository root. The line changes import lines only, and on this tree it changes nothing:

```bash
git grep -lz '"github.com/manaflow-ai/subrouter/' -- '*.go' | xargs -0 perl -pi -e 's#^(\s*(?:import\s+)?(?:[\w.]+\s+)?)"github\.com/manaflow-ai/subrouter/([^"]*)"(\s*(?://.*)?)$#$1"github.com/discordwell/subrouter/$2"$3#' && gofmt -w .
```

Then check `git grep 'github.com/manaflow-ai/subrouter/' -- '*.go'`. What may stay names upstream's releases or commits, or is the pre-rename package `isSubrouterMainPackage` accepts and its test. Any other use of the old path, such as a new `hostModulePath`, is renamed by hand.

- Code: `go.mod`, `cmd/subrouter/install_daemon.go` (`subrouterMainPackage`, `preRenameMainPackage`, `isSubrouterMainPackage`), `cmd/subrouter/sr_host.go` (`hostModulePath`), `Dockerfile`, `scripts/build-release.sh`, `ops/bin/sr-local-ci`
- Tests: `cmd/subrouter/install_alias_test.go` (`TestSubrouterMainPackageMatchesThisPackage`, `TestPreRenameBuildIsSubrouters`), `cmd/subrouter/sr_host_test.go`
- Docs: `AGENTS.md`, `README.md`, `ops/LAPTOP.md`

## 107. The fork publishes its own releases, and every install or update path fetches them

The owner, 2026-11-25: the fork publishes its own GitHub releases, so anyone can install it without Go, and every install or update path in the fork fetches the fork's releases, never upstream's. Upstream's releases carry none of the fork's patches. This change makes a release possible and creates none; the owner tags the first one, `v0.2.0`, after it merges.

§107.1 What changed.

- `install.sh` fetches from `https://github.com/discordwell/subrouter/releases`. `SUBROUTER_DOWNLOAD_BASE`, `SUBROUTER_VERSION` and `SUBROUTER_INSTALL_DIR` still override it. The README's install section leads with `curl -fsSL https://github.com/discordwell/subrouter/releases/latest/download/install.sh | sh`, then the build from source.
- `sr server install` pipes the fork's `install.sh` (`publicInstallScriptURL`, from `releasesURL`). §37.3's refusal, `errUpstreamReleaseInstall` and `--allow-upstream-release` are gone, because their premise, that the fork publishes no release, is gone. `deploy/gcp/publish-subrouter.sh` no longer passes the flag. Agents still do not run the command on the owner's hosts: it rewrites the unit or plist that the scripts in `ops/deploy/` write (`ops/skills/subrouter/SKILL.md`).
- `sr host attach`'s install script resolves `latest` from the fork's releases, and `sr update`'s release repository (`updateReleaseRepo`) is the fork's. Neither verb is dispatched.
- The transport observer's latest-release, tag and compare URLs name the fork. Its pinned predecessor (`v0.1.60`) and bootstrap (`v0.1.63`), and the same pins in `deploy/gcp/golden-local-mac-production-continuity.sh`, are upstream tags that the fork has not released, so a pinned golden run fails its tag check until they are re-pinned to fork releases.
- `deploy/macos/*autoupdate*.sh`, `deploy/macos/install-cli-autoupdate.sh`, the `deploy/gcp/` scripts (`fetch-release.sh`, `subrouter-autoupdate.sh`, `create-subrouter-vm.sh`, `startup.sh`'s trusted repository, the golden harness, the install hints in `publish-subrouter.sh` and `upload-codex-accounts.sh`) and `.github/workflows/gcp-deploy.yml` default to `discordwell/subrouter`, and verify attestations signed by its `release.yml`. `release.yml` still copies `install.sh`, `install-front-slots.sh` and `deployment-contract.py` into each release, which the GCP scripts read.
- Deleted: `npm/`, `subrouter_cli/`, `package.json`, `pyproject.toml`, `release.yml`'s `npm-publish` and `pypi-publish` jobs, its `workflow_dispatch` inputs and its `pyproject.toml` version fallback, and the `.gitignore` lines for them. The wrappers downloaded upstream's release, and the package name `subrouter` on npm and PyPI is upstream's (both at 0.1.16), so the fork cannot publish them. `release.yml` now runs only on a pushed `v*` tag.
- A release's title and notes are both `Subrouter v<version>` (`scripts/publish-github-release.sh`). The script requires the repository's release immutability setting.
- `ops/publiccheck/upstream_release_test.go` fails on a release, API or raw-file URL for `manaflow-ai/subrouter` in any tracked file, and on the bare slug in a line of code that is not a comment, which is how a script or Go names a default release repository. This file and `ops/publiccheck/` are exempt. So are comment lines, which credit ported code, and quoted Go import paths, such as `preRenameMainPackage` (§106). On the tree before this change it reports 51 lines.

§107.2 What did not change.

- `sr update`, `sr rollback` and the background `update --auto` stay unwired (§23, §29).
- The laptop and the box still deploy from a source build through `ops/deploy/`.
- The Cloudflare workflow and `cloudflare/`, the launchd labels, the README's link to upstream and the comments on ported upstream code.

- Code: `install.sh`, `cmd/subrouter/sr_server.go` (`releasesURL`, `publicInstallScriptURL`, `serverInstall`), `cmd/subrouter/sr_host.go` (`hostInstallScript`), `cmd/subrouter/sr_update.go` (`updateReleaseRepo`), `cmd/subrouter-transport-observer/golden.go`, `deploy/gcp/`, `deploy/macos/`, `.github/workflows/release.yml`, `.github/workflows/gcp-deploy.yml`
- Tests: `ops/publiccheck/upstream_release_test.go` (`TestUpstreamFindings`, `TestNoUpstreamReleaseFetch`), `cmd/subrouter/installer_lifecycle_test.go` (`TestServerInstallPipesThisForksRelease`), `cmd/subrouter/sr_host_test.go` (`TestHostInstallScriptIsIdempotent`), `cmd/subrouter/deploy_scripts_test.go`, `cmd/subrouter/release_publish_test.go`, `cmd/subrouter-transport-observer/golden_options_test.go`
- Docs: `README.md`, `ops/GETTING-STARTED.md`, `ops/OPERATING.md`, `ops/skills/subrouter/SKILL.md`, `docs/production.md`, `docs/upgrades.md`, `deploy/gcp/README.md`, `ops/deploy/lightsail/README.md`
- Related: removes §37.3's refusal; amends §106.2.
