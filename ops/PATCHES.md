# Local patch against upstream (condensed)

This fork carries its own changes on top of `manaflow-ai/subrouter`. Each numbered section records one of them: what it does now, why it exists, and the settings that control it. This copy is condensed to about a tenth of the original.

The fork point is upstream `93bef02` (`v0.1.54`, 2026-08-03). Two upstream merges followed. §23 records the first (`1f9bf38`, 2026-09). The second brought the fork up to upstream `ec86e8cb` (2026-10-25) and has no section here.

Sections are in numeric order, but past §17 the numbers are not chronological. Three sections came from branches outside the main line:

- §63 and §64 were never merged, and their code is not in this repo.
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
health is also served on `--public-addr` and an error string carries the path). A socket peer reports `127.0.0.1`, so loopback gates treat it as
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

§37.3 `sr server install` piped upstream's `install.sh`, which replaced this fork's binary with upstream's release while the host reported healthy. The fork publishes no release of its own. The command now refuses without `--allow-upstream-release`, before it looks up the server or mints a token. The message says this fork deploys a Linux box from a source build and has no remote macOS deploy. Usage, `sr help` and the README omit the flag, so a copied command does not carry it. `sr server sync` and `diff` now tell the user to upgrade an old server from source. Upstream's `deploy/gcp/publish-subrouter.sh` installs upstream's release by design, so it passes the flag.

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
- Related: §56 builds on this (until claimed or expired, the router ranks the account at 2x and drains it early below 25 % of its week). §46.9 reverses §46.4's private-only rule and builds on §43.2's public admin listener. §46.11 supersedes parts of §46.10.

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

Measured on `eight-alpha` (2026-11-09, 19:22Z): a claim took the week from 100 % to 42 %
used and the Fable meter from 116 % to 0 %. So the Fable limit is half the week, and a reset
only refunds the Fable usage off it. A claim loses nothing left in the week, so §56's reason
to hold an account does not apply.

The week hold (`WeekHeldUntil`) is now the soonest `ends_at` among live grants whose
`clears` contains `seven_day` or is absent (October's shape). Live is the account eligible
and the grant with a reset left, not expired and not paused. An `ends_at` that does not
parse holds nothing. A Fable-only account is no holder. It is published as
`normalized.seconds_to_held_week_reset_expiry` and, for week holds only, as the legacy
`seconds_to_held_reset_expiry`. Readers take only the new key, so a pre-§64 body ranks 1x.
The Fable meter (`FableLimit`) is the usage answer's `limits[]` `weekly_scoped` row whose
display name starts with "fable" (case-insensitive; of several, the most used), else
`seven_day_oauth_apps`. Rows decode one by one, so drift never fails the usage read. A gap
of more than one point between the two logs a WARN at most once an hour per account.
`ResetGrant` also decodes `type`, `gives_back_weekly` and `unavailable_reason`, as Claude
Code 2.1.321 does. Nothing routes on them. Every Fable model now falls to `claude-opus-5-5`
with the client's bracketed suffix kept, because Opus and Fable versions do not release in
lockstep. The latent Bedrock path maps `claude-fable-5-2` to `us.anthropic.claude-fable-5-2`
and prices Fable 5.2 per MTok at input 8, output 40, cache write 10 (5 m) and 16 (1 h),
cache read 0.2, and Fable 5.1's cache read at 0.25. The `fable` alias or no model keeps
`us.anthropic.claude-fable-5`. Its quota bump still keys every Fable id to Fable 5's code
(`bedrockTPMQuotaByModel`, `L-9B258944`).

Owner decision, 2026-11-09: "The resets last until March; we will absolutely chill on them,
we don't need to build around resets". Claiming stays manual (`sr claude reset <name>`). Its
y/N asks about the general week (when it has more than 10 % left), so read `Fable wk` in
`sr status` first. The §57 Fable reservation stays off. Deploy the box, then the laptop: a
pre-§64 box still drains its Fable holders under 25 % of their week through its `burndown`
verdict, which a laptop honours (§31). A pre-§64 laptop keeps its own Fable holders at 4x
and burns them down early until it is deployed. Nothing is misrouted in either. Whether the
`limits[]` row and `seven_day_oauth_apps` are one bucket is unmeasured.

- Knobs: `SUBROUTER_RESET_HOLD_PRIORITY` (still switches the week hold; `0` on a pre-§64 box stops its early drain)
- Code: `internal/agents/claude/reset.go` (`ResetInfo`), `internal/proxy/claude_fable_meter.go`, `internal/proxy/claude_opus_substitute.go` (`claudeOpusModel`)
- Related: amends §11 (the Opus target and the Fable meter), §46 (which grant is current) and §56 (which grants hold an account).

## 65. An Opus or Sonnet turn ranks an account whose Fable meter is spent at up to 2x

Owner decision, 2026-11-12: "when we do opus turns we should preferentially be using those"
(accounts whose Fable meter is spent), in "the clean, proper version": a ranking term that
leaves slot and reserve numbers alone, unlike §57. Each account has a general week every model
spends and a separate Fable meter (`seven_day_oauth_apps`, published as `oauth-apps-weekly`).
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
- Related: stacks with §57 when `SUBROUTER_FABLE_RESERVATION=1`; §57 carries a "See §65" note.

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
- Related: pairs with turning off §65's Fable-spent preference on the laptop, which steered Opus sessions onto Fable-spent accounts and so made §71's case the usual one.

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
a pull request, a merge group and a push to main run "Public check" and the "Build, vet, test" gate
alone, and the gate passes on the skipped jobs as it does for a docs-only change. Start the full suite
with `gh workflow run ci.yml --ref <branch>`; there the classifier runs everything.

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

`TestLaptopScriptsChangeLaunchdOnlyThroughTheGuard` counts as a call `launchctl`, by name or by path, where a command starts: at the start of a line, after `;`, `&`, `|`, `(`, `)`, `{`, `}`, `!`, a backquote or `$(`, or after `if`, `elif`, `while`, `until`, `then`, `do` or `else`. It may stand behind `command`, `exec`, `env`, `nohup`, `nice`, `time`, `sudo` or `timeout` with their options, and behind variable assignments, and a line that ends in a backslash goes on into the next. The verbs are `kickstart`, `bootout`, `bootstrap`, `load`, `unload`, `remove`, `kill`, `stop`, `start`, `enable`, `disable` and `submit`. `TestLaunchdChangeScanCatchesEverySpelling` holds 32 spellings, and lines that must not count, such as a hint the scripts print. The first pattern missed 27 of the 32.

The harness's cleanup signals a process group only when it can show the group is a detached run of this test. The pid in the run's `.pid` file must be over 1 and still have the test dir in its arguments, and its group must not be 0, 1 or the harness's own. Before, a pid file with 0 named the harness's own group, and one with 1 sent `kill -STOP -1`, which reaches every process the user owns. A run that died could also leave its pid to another process.

`scripts-default-to-the-live-job` covers the default label and address again. Every other case hands the scripts a label and address of the test's own, so none had run a script on its defaults. This case runs no script. It evaluates each `${SUBROUTER_LABEL:-...}`, `${SUBROUTER_ADDR:-...}` and `${CLAUDE_ROUTE_TCP:-...}` in the laptop scripts, `sr-triage` and `claude-route` in a shell with the variable unset, and checks that each script has the ones it uses.

`TestStoppedHarnessKillsItsDetachedRuns` waited at most 5 minutes for `laptop-ok`'s detached run to park in its kickstart. At a load near 60 two of its subtests ran out of time in a full run and passed alone. It now waits while any file under its temporary dir changes, and gives up after 3 minutes with no change. It proves what it proved before.

Not closed: `own_job_or_exit` reads `launchctl print` and then acts, so a job that changes between the two is not caught. That is a limit of §76.1 on its own; §76.2 and §76.3 keep a test run off the live job.

There is nothing to turn off. On Linux the harness runs as before.

Tested in Docker. `TestDeployHarnessRefusesAMacOutsideASandbox` stands in for macOS on Linux: a `uname` that prints Darwin, and a `sandbox-exec` that refuses `/bin/launchctl` or runs it. It expects exit 2, the reason and the safe commands, and no case started, for each refusal, and a case that passes through the stand-in when it refuses. `laptop-stops-before-kickstarting-a-job-from-another-plist` and `laptop-reload-refused-without-its-plist` now check that the build on disk is the old one. `laptop-refusal-that-cannot-put-the-build-back-says-how` fails the restore's signature as well, expects the new build on disk, the line that names it and the exact command, and runs that command. `act-refusal-at-the-restart-puts-the-files-back` moves the job after the activation's step 2 and expects the plist and the build from before, and no supervisor build left. `harness-cleanup-signals-only-its-own-runs` makes `kill` a function that records its arguments, plants pid files for a run, for another process, for a process in the harness's group, and for 0, 1 and a word, and expects only the run's group. `TestHeldWaitFollowsTheRunsProgress` checks the wait on a slow run that keeps going, on one that stops, and on a harness that ends first. Before these changes, every one of them failed except `scripts-default-to-the-live-job`, which fails when a default has a typo, and the two Go tests of test code, which fail with the first pattern and with a fixed deadline. The old cleanup would have signalled the groups of the run, the other process and the harness, and `-1`. Not run: anything on macOS, so neither the real `sandbox-exec` nor sr-local-ci's sandbox answered the check.

- Code: `ops/deploy/macos/laptop-deploy-lib.sh` (`own_job_or_exit`, `kickstart_job`, `bootout_and_wait`, `bootstrap_job`, `take_mutation_lock`), `ops/deploy/macos/deploy-laptop.sh` (`restart`, `put_back_on_refusal`), `ops/deploy/macos/activate-laptop-supervisor.sh` (`undo_on_refusal`), `ops/bin/sr-triage`, `ops/bin/sr-local-ci`
- Tests: `ops/deploy/tests/deploy-scripts-test.sh` (`sandbox_profile`, `refuse_unfenced`, `run_sandboxed`, `finish`, `run_group`, `cleanup`, and the cases listed under §76 and §76.6 in `requiredCases`), `ops/deploy/tests/deploy_scripts_test.go`
- Docs: `AGENTS.md`, `ops/README.md`, `ops/deploy/lightsail/README.md`

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
is built in parts: §79.1 sends one request to a box, and §79.2 chooses the box. Until a later part adds
`subrouter standby`, nothing runs this code, and no behaviour of the daemon changes.

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
which goes to a box. A live session served by a box on 2026-11-21 showed why. With a 503 there, a session
that starts during the outage withholds WebFetch, DesignSync and RemoteTrigger, ignores the Retry-After, and
does not ask again for an hour, so the tools stay gone after the daemon returns. With the box's 200 it keeps
them, and nothing of the box's identity reached a file under its config folder. The box's body was the same
as the daemon's, because both identities had the default policy. A box whose identity belonged to another
organization would apply that organization's policy. The path is cleaned before it is read, so
`/v1/../api/...` counts as `/api`. A request that names an account (`X-Subrouter-Account-ID` or
`X-Subrouter-Account`, set by `claude <account>`) gets a 503 and is never sent, `policy_limits` included,
because a pinned launch is promised that account and no other. With no box usable, the client gets a 503
with Retry-After 30 at once, and nothing is parked. Every 503 the handler makes has Anthropic's `api_error`
shape, which Claude Code retries like any server error, and names its cause in `X-Subrouter-Reason`:
`standby_no_box`, `standby_box_unreachable`, `standby_box_failed`, `standby_pinned_account`,
`standby_control_plane` or `standby_daemon_surface`.

A `Box` exists only through `NewBox`. It takes an `https://host:port` URL, the stand-in token and the 64-hex
pin of the box's certificate, and builds its transport on `proxy.PinnedTLSConfig`. A certificate that is not
the pin ends the handshake before a byte of the request is written, so the token never reaches it. An
`http://` URL, a missing pin and a malformed one are refused. The token is never logged.

Not done: replaying the daemon's own `policy_limits` answer, saved while it is healthy, ahead of the box's.
The same session accepted such a replay, even with its signature header removed. It needs the process
that can reach the daemon, so it belongs to a later part. Turning it off: nothing runs the handler yet.
Once `subrouter standby` runs it, stopping that job turns it off.

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

Not done: the stand-in command must take `SUBROUTER_PEER_TAILNET` from the daemon's worker config, and must
apply the daemon's `remote.key` gate before it builds the set. Both belong to the part that adds
`subrouter standby`. Turning it off: nothing runs the set yet. Once `subrouter standby` runs it, removing a
box's token file takes that box out within 2 s.

Tests: `internal/standby/handler_test.go` (§79.1) and `internal/standby/boxset_test.go` (§79.2) hold one
test per behaviour above, against `httptest` TLS boxes that each have their own self-signed certificate and
pin, with no network, and temporary state folders. Run them with `./deploy/docker/test.sh ./internal/standby`.
Each test was run once with its behaviour's code taken out of the working tree, and failed.

- Code: `internal/standby/box.go` (`Box`, `NewBox`, `Boxes`), `internal/standby/handler.go` (`NewHandler`,
  `routeFor`, `rewriteForBox`, `send`, `retryAfterWriter`), `internal/standby/boxset.go` (`Set`, `NewSet`,
  `Config`, `Run`, `Candidates`, `Status`)
- Related: §30 (the Claude socket), §43 (named tokens; a stand-in token is a `membership` token), §68
  (tailnet first), §31 (the peers file and the peer-holds files)

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

§84.5 There is nothing to turn off. The change is in what the scripts read and print, and nothing in it restarts the daemon. `claude-route` still reads only `"ok": true` at the path, so a launch while a stand-in holds the path takes the pool route, as the stand-in intends.

§84.6 Tested in Docker with the deploy-script cases. The fake `curl` gains `FAKE_CLAUDE_WORD` (the word of a health answer at the path), `FAKE_SV_YIELD` (supervisor-status `claude_socket_yield`) and `FAKE_DAEMON_SOCKET_WORD` (a plain serve's word on TCP). `sup-stand-in-at-the-claude-socket-is-not-the-daemon` puts a stand-in at the path while the supervisor says `waiting`. It expects exit 3, the `standby` line, the new build kept, no restart and one upgrade. `sup-check-stand-in-the-supervisor-does-not-ask-back` runs `--check` with a stand-in while the supervisor says `unavailable`, and expects `CHECK INCOMPLETE`. `sup-claude-socket-waiting-is-not-deploy-ok` and `sup-claude-socket-displaced-is-not-deploy-ok` put another process at the path. `sup-claude-socket-yielded-says-until-when` checks an operator's yield in a deploy, and a yield for worker exits with nothing at the path under `--check`. `laptop-stand-in-at-the-claude-socket-is-not-the-daemon` deploys to a plain serve, whose restart is when a stand-in can take the path. `triage-tells-a-stand-in-from-the-daemon` checks the three `socket:` lines. With the scripts from before this change, every one of these cases failed. Each stand-in, `waiting` and `displaced` case and the operator's yield exited 0 with `DEPLOY OK` or `CHECK OK`. The yield for worker exits ended `CHECK FAILED`. Triage printed no `socket:` line. `sup-claude-socket-through-the-supervisor` and `sup-claude-socket-down-names-the-supervisor-state` pass unchanged.

- Code: `ops/deploy/macos/laptop-deploy-lib.sh` (`probe_claude_socket`, `at_the_path`, `status_field`, `verify_routes`), `ops/deploy/macos/deploy-laptop.sh` (`SOCKET_WHY`, the `--check` ending), `ops/bin/sr-triage`
- Tests: `ops/deploy/tests/deploy-scripts-test.sh` (the fake `curl`'s three knobs, `no_owner_kept`, and the seven cases above, listed under §84 in `requiredCases`), `ops/deploy/tests/deploy_scripts_test.go`
- Docs: `ops/LAPTOP.md`, `ops/OPERATING.md`

## 85. The Docker test runners keep their Go build caches near a limit

`deploy/docker/test.sh` keeps Go's build cache in the Docker volume `subrouter-test-gobuild`, so later runs reuse it. Go deletes only cache entries that no build used for five days. Each tree a run tests adds entries, and many worktrees were tested each day. On 2026-11-22 the volume held 17 GB after three days of heavy use, and a sibling volume held 8.8 GB. The disk that Docker writes to reached 98 percent, minutes before tests across the machine would have failed for lack of space. `ops/bin/sr-local-ci --docker` keeps its build cache in a volume of its own, `sr-local-ci-gocache`, which grows the same way.

§85.1 `deploy/docker/trim-go-cache.sh DIR LIMIT_KIB AGE_MINUTES` measures DIR with `du -sk`. When DIR holds more than the limit, it deletes the entries not used for AGE_MINUTES and prints one line that gives the size, the limit, the number of entries deleted and the space they freed. When every entry may have been used within that time, the line says that nothing was freed. At or under the limit, with a limit of 0, or with no DIR yet, it deletes nothing and prints nothing. An entry is what Go's own trim deletes: a file named `*-a` or `*-d` in one of the two-hex-digit folders. The cache's `README` and `trim.txt`, deeper files, other folders and folder links stay.

§85.2 Before each run, and before `--self-check`, `test.sh` runs the trim in a container of its own. The container mounts the build cache volume and the checkout read-only, with no network and no module volume. The limit is `SUBROUTER_TEST_CACHE_LIMIT_GB`, a number of GB, default 6. `0` turns the trim off, and a value that is not a number stops the run before any container starts. The age is a day. `sr-local-ci --docker` runs the same script, from its own checkout, on `sr-local-ci-gocache` before the suite, with the same variable. A trim that fails prints a line, and the run goes on with the cache as it was.

§85.3 Runs at the same time are safe. Go updates the time of an entry it uses only once that time is an hour old, so an entry used within the day has a time within the day and an hour. The trim deletes an entry only when its time is older than a day and an hour, and Go's own trim adds the same hour. So the trim never deletes an entry used within the day, or one that a running build uses. A build can still miss an entry that was unused for a day and is deleted just as the build looks it up, and the build then compiles it again. Go's own trim has the same window. Two trims at once each skip what the other deleted first (`find -ignore_readdir_race`), and both exit 0.

§85.4 The limit is not a ceiling. While every entry may have been used within the day, the cache stays over the limit, and each run prints that nothing was freed. When this was built, the volume held 8 GB, and no entry in it had a time over a day old. Over the limit, the cache keeps about one day of use where it kept five. `sr-local-ci` without `--docker` uses the developer's own Go cache, which every Go build on the machine shares, so it is left to Go.

§85.5 To turn it off, set `SUBROUTER_TEST_CACHE_LIMIT_GB=0`. To clear a cache by hand, remove its volume while no run uses it, as `deploy/docker/README.md` says.

§85.6 Tested in Docker. `TestGoCacheTrimDeletesOnlyEntriesUnusedForADayOverItsLimit` runs the script on a folder shaped like a Go build cache, with entries two days old, an entry whose time is 24.5 hours old (Go may have used it 23.5 hours ago), entries used three hours ago and now, the marker files, files that are not entries, a folder link and a module folder beside it. Over the limit, only the two old entries go, and the line says that 1.0 MB was freed. Under the limit and with a limit of 0, nothing goes and nothing is printed. `TestGoCacheTrimsAtOnceAllSucceed` runs four trims at once on 1,792 entries, and each exits 0 with one line and no error. `TestDockerTestScriptTrimsItsBuildCacheBeforeTheRun` and `TestDockerRunTrimsItsBuildCacheBeforeTheSuite` (in `ops/localci`) run `test.sh` and `sr-local-ci --docker` with `deploy/docker/testdata/fake-docker.sh` first on `PATH` as `docker`. The stand-in records each call, and runs the trim on folders that stand in for the volumes. Over the limit, the old entry in the build cache goes, the module volume keeps its old file, and the trim comes before the tests. With a limit of 0, nothing goes. A trim that fails leaves the run to go on, and a limit that is not a number is refused. With `test.sh` and `sr-local-ci` from before this change, the old entry stayed, a failed trim printed nothing about it, and a limit that is not a number ran. The cases for the default limit and for 0 check only that nothing is deleted, so they pass either way. Without the script, every case of the first two tests failed. Without its age test, the script deleted the entries in use. Without the extra hour, it deleted the entry whose time is 24.5 hours old. Without its size test, it trimmed a cache under its limit. Without `-ignore_readdir_race`, trims at once printed `No such file or directory`. The first real runs found `subrouter-test-gobuild` at 8.0 and 8.3 GB and freed nothing, because no entry's time was over a day and an hour old.

- Code: `deploy/docker/trim-go-cache.sh`, `deploy/docker/test.sh`, `ops/bin/sr-local-ci` (the `--docker` branch)
- Tests: `cmd/subrouter/docker_cache_trim_test.go`, `ops/localci/localci_test.go`, `deploy/docker/testdata/fake-docker.sh`
- Docs: `deploy/docker/README.md`
