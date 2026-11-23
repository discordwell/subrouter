# Codex CLI Integration

Current Codex does not use an `OPENAI_BASE_URL` environment variable for the built-in OpenAI provider. Use the Subrouter wrapper, or let `sr server use <name>` write the Codex config file.

## Recommended

Use `subrouter codex` anywhere you would use `codex`:

```bash
subrouter codex
subrouter codex exec "your prompt"
subrouter codex resume --last
subrouter codex --version
```

The wrapper launches the child with an authenticated custom provider pointed at
Subrouter:

```toml
model_provider = "subrouter"
[model_providers.subrouter]
base_url = "http://127.0.0.1:31415/v1"
experimental_bearer_token = "subrouter"
wire_api = "responses"
supports_websockets = true
http_headers = { "X-Subrouter-Agent" = "codex" }
```

Subrouter supports Codex WebSocket requests, so the custom provider keeps the
normal transport behavior.
This includes Responses WebSockets at `/v1/responses` and realtime WebSockets at `/v1/realtime`.

Do not set a dummy `OPENAI_API_KEY`. The wrapper authenticates the local hop
itself and never sends ChatGPT credentials, which decouples the client from
`~/.codex/auth.json`: a local ChatGPT logout or refresh failure cannot stop the
request before proxy-side failover runs. Subrouter replaces the outbound
Authorization and `ChatGPT-Account-ID` headers with the selected `sr` account.
Resume with `sr codex resume ...`; a bare `codex resume ...` does not recreate
wrapper-only overrides.

The two launch modes are intentionally independent: plain `codex` uses Codex's
normal direct OpenAI configuration, while `sr codex` opts that process into the
Subrouter pool. The launcher strips older Subrouter-owned `-c` routing values
and local-provider `--oss` settings from copied or saved commands before adding
the current provider settings, so another provider or a retired URL cannot
override the selected server by argument precedence. It
also exports `SUBROUTER_CODEX_LAUNCHER="<launcher> codex"` and
`SUBROUTER_CODEX_RESUME_COMMAND="<launcher> codex resume"` for session managers
that persist a resume command. The launcher name follows the invoked `sr`,
`subrouter`, or `cx` alias; unrelated clients can ignore these variables.

### Pool credentials and your own Codex login

`~/.codex/auth.json` is your own interactive login (Codex.app, plain `codex`),
and only your own Codex client refreshes it. The daemon never writes it: for
Codex, the periodic `--sr-switch-interval` sweep only refreshes routing scores.

Each pooled account is a separate login of its own. `sr add codex` runs
`codex login` under a temporary `CODEX_HOME` and stamps the stored record
`isolated-server-login`. ChatGPT refresh tokens rotate on use, so two holders
of one refresh-token chain sign each other out; `serve` therefore refuses to use
or refresh a stored record that is not proven isolated, or whose refresh token
is the one in `~/.codex/auth.json`. The refusal is terminal for routing, and it
applies to a fresh access token too, so a pre-provenance record stops serving
as soon as a gated daemon starts, not at its next expiry.

- `sr codex isolation-check` counts the records that still need repair.
- `sr codex attest-legacy` repairs pre-provenance records without a browser:
  it refreshes each one once, requires the provider to rotate the refresh
  token, and stamps it `server-attested-transfer`. That rotation kills every
  other copy of the chain, including the same account held by another machine.
  A record whose refresh token is also `~/.codex/auth.json`'s is skipped; it
  needs an isolated re-login (`sr add codex` or `sr codex migrate-isolation`).
- An explicit `sr switch` / `sr g` still copies a pool account into
  `~/.codex/auth.json`, and doing so demotes that record to
  `interactive-import`, which takes it out of serving. Using an account
  interactively is an explicit act of taking it out of the pool.

See [upgrades.md](upgrades.md#credential-origin-rollback-boundary) before
rolling back to a binary that predates the provenance field.

### Automatic quota failover

When an account reaches its usage limit or workspace spend cap, Subrouter marks
that account exhausted and fails the request over. The stages run in this
order, and each runs only when the one before it found nothing:

1. **Another pool account.** For HTTP requests Subrouter retries the rejected
   request on another OAuth account with available usage, within the
   request's retry budget. For WebSockets it intercepts the quota error before
   forwarding it, prepares the alternate account, and sends a retryable 1013
   close; Codex reconnects and resumes through its native recovery path.
   Subrouter does not replay model requests or tool calls itself.
2. **A rate-limit-reset credit** (opt-in, off by default; see below). Runs only
   when a quota failure finds no alternate account.
3. **The peer pool** (opt-in, `SUBROUTER_REMOTE_POOL`). The request is
   forwarded to the peer daemon's pool.

   When this machine and a peer are on one Tailscale tailnet,
   `SUBROUTER_PEER_TAILNET` makes Subrouter dial the peer's Tailscale address
   first. A network that refuses new connections while it is busy still
   passes traffic in a Tailscale tunnel that is already up.
   The value is a comma-separated list of `name=IP` pairs, for example
   `box=100.64.0.7,box2=fd7a:115c:a1e0::9`. The name is the peer's name in
   the `SUBROUTER_REMOTE_POOLS_FILE` file, or the URL's `host:port` for
   `SUBROUTER_REMOTE_POOL`. Subrouter dials the IP on the URL's port. The
   URL, the `Host` header, and the certificate pin do not change.
   A dial to the tailnet address fails when it cannot connect, or when the
   listener there fails the TLS handshake or the pin. Subrouter then dials
   the URL. If the URL answers, Subrouter dials the URL first for one
   minute. During that minute, a dial to the URL that fails, or that does
   not connect within 2 seconds, is followed by one dial to the tailnet
   address. While this machine has no address in
   `fd7a:115c:a1e0::/48`, Subrouter dials only the URL. Each change of route,
   or of how the tailnet dial failed, logs one
   `remote pool peer route changed` line. Its `failure` field is `connect`,
   `handshake_timeout`, `pin_mismatch`, or `handshake`. If any entry is invalid,
   Subrouter logs a warning and every peer dials its URL. An entry is
   invalid when it is not `name=IP`, names no configured peer, repeats a
   peer, gives an address outside `100.64.0.0/10` and
   `fd7a:115c:a1e0::/48`, or gives an IP that another entry gives to a peer
   on the same port. Peers on different ports can share one IP. Unset the
   variable to turn this off.
4. **The Azure fallback** (opt-in; see [Azure fallback](#azure-fallback)).
   Azure has no WebSocket Responses surface, so a WebSocket session reaches it
   only after Codex moves to HTTPS.

If no stage is armed or every armed stage declines, the original quota error
remains visible.

The credit stage needs a quota error to act on. When the request spent its
retry budget, or when no account could be selected before anything was sent,
there is nothing to redeem against: stage 2 is skipped and the peer pool or the
Azure fallback, when configured, answers instead.

#### When the whole pool is cooked

Failover can only rotate to an account that has quota. When every account is
cooked there is nothing to rotate to, and the turn dies on the upstream's own
quota error -- the exact interruption failover exists to prevent. Subrouter now
says so in the log (`codex quota failover found no alternate account`) instead
of giving up silently.

A cooked ChatGPT Pro account may still hold complimentary rate-limit-reset
credits, the ones `sr reset` and `/_subrouter/rate-limit-reset` redeem by hand.
Set `SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET=1` to let failover spend one
automatically as stage 2 above, before any peer or Azure stage: it redeems a
credit on a cooked account, clears that account's exhaustion marks, invalidates
the usage scores, pins the session, and reconnects onto the revived account.

It is **off by default and should stay off unless you want it**, because the
credits are finite and do not regenerate. Three bounds apply once it is on:

- the explicit opt-in above,
- one redemption at a time daemon-wide, and
- a 30-minute floor between redemptions, so a burst of failing turns (Codex
  retries, plus every concurrent session seeing the same cooked pool) cannot
  drain the reserve.

This fork also claims saved resets by itself at an account's own usage limit
(`ops/OPERATING.md`, Automatic resets; `ops/PATCHES.md` §93, §94). By default
it runs in shadow mode, which logs what it would redeem and spends nothing. In
real mode (`SUBROUTER_AUTO_RESET=1`) the daemon that holds the account redeems
its credit that expires soonest, by default only while the week has under 1%
left and only when the claim is worth making: the pool needs the credit and
the wait it skips is at least a quarter of a week, or the credit is about to
expire. It keeps the account's turns waiting on it. In real mode that one
decision is the only thing that spends credits: the failover redeem of
`SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET=1` is off, and
`--reset-credit-autospend=spend` only advises, as `warn` does. The daemon logs
one Warn for each at startup. In shadow mode and with automatic resets off,
both behave as described above.

Eligibility is decided against live upstream usage, never against Subrouter's
cached scores -- those scores are what just reported the pool empty. An account
that is not actually cooked, or holds no credit, keeps what it has. A workspace
**spend cap** is a billing cap, not a rate-limit window: no credit resets it, so
a spend-capped account is skipped and the cap has to be raised by the workspace
owner.

Regression coverage includes the workspace spend-cap message, HTTP account
rotation, WebSocket reconnect and account rotation, unrelated error/output
passthrough, and the auto-redeem stage (off by default, revives a cooked
account, honours the interval floor, skips the already-tried account, skips a
healthy account, and never fires for Claude). The optional native-client test verifies automatic recovery using a
fake upstream, dummy credentials, and an isolated Codex home:

```sh
SUBROUTER_TEST_CODEX_BINARY="$(command -v codex)" go test ./internal/proxy -run '^TestCodexClientRecoversRetryableQuotaClose$' -count=1 -v
```

### Redeeming a reset credit by hand (`sr reset`)

A reset credit is finite and never renews, so every spelling that spends one
either names its target or previews first.

- **Cooked** means one rule everywhere (`accounts.WeeklyCookedWindow`, shared by
  `sr status`, `sr switch`, `sr reset` and the server): a fully used
  account-wide window of at least six days, whether Codex reports it as
  `primary_window` or `secondary_window`, or a length-less legacy secondary.
  `limit_reached` counts only when no weekly window is reported and no full
  shorter window explains it -- a 5h stall recovers on its own within hours,
  and a credit spent on it is lost. A usage-based seat's `spend` window (its
  workspace spend cap, which `rate_limit: null` seats publish instead) reads
  cooked in `sr status` and `sr switch` when full, as does its `spend/reached`
  hard stop on its own, but never makes a reset candidate: the server and local
  reset paths judge the rate limit alone, and `--gto` skips a seat cooked on its
  spend cap and says so. No credit lifts a spend cap.
- `sr reset` with no target asks the server to pick (`best=true`): an expiring
  credit on an account stuck at least 6h first, then the longest natural weekly
  wait. An older server that does not know `best=true` gets the client-side
  pick instead.
- `sr reset <email>` resets that account. Flags work on either side of the
  email: `sr reset <email> --dry-run` previews and redeems nothing.
- `sr reset --all` lists what it would redeem, with each account's weekly wait
  and credit expiry, and asks before spending. Without a terminal it refuses;
  pass `--yes` to confirm from a script. It then redeems exactly the listed
  accounts, one request per account.
- `--min-wait D` (e.g. `1d`, `12h`) with no target, `--all` or `--candidates`
  skips accounts whose weekly window resets on its own within `D`. A server
  that does not report weekly waits is refused rather than trusted.
- `--candidates` lists what `--all` would redeem, without redeeming.
- `--local` uses the tokens on this machine. When `sr` has already routed `reset`
  to a selected server it is refused; `SUBROUTER_CODEX_SERVER=local sr reset`
  is the spelling that reaches the local store there.
- The command exits non-zero when any redemption fails.

## Server Switching

Register and select a remote server:

```bash
sr server add team --url http://100.64.0.1:31415 --default
sr server use team
```

Both commands write the selected routing defaults to `CODEX_HOME/config.toml`, or `~/.codex/config.toml` when `CODEX_HOME` is unset:

```toml
openai_base_url = "http://100.64.0.1:31415/v1"
chatgpt_base_url = "http://100.64.0.1:31415/backend-api"
experimental_realtime_ws_base_url = "http://100.64.0.1:31415/v1"
```

Use `--no-codex-config` to change only Subrouter's selected server. Use `sr server use local` or `sr server clear-default` to restore local routing. When a remote server is selected, bare `sr` and `sr status` render that server's usage table.

## Codex Desktop

Codex Desktop has two outbound paths:

- The Rust `app-server` sends model traffic, including Responses WebSockets. It reads `CODEX_HOME/config.toml`, so `openai_base_url = "http://127.0.0.1:31415/v1"` routes that traffic through Subrouter.
- The same `app-server` reads account usage and sends add-credit nudges through `chatgpt_base_url`. Set `chatgpt_base_url = "http://127.0.0.1:31415/backend-api"` if Desktop UI state should reflect the selected Subrouter OAuth account.
- The Electron shell sends ChatGPT backend requests with `electron.net.fetch`. It reads `CODEX_API_BASE_URL` at process start. Use `CODEX_API_BASE_URL=http://127.0.0.1:31415/backend-api` so Electron requests for `/codex/...` reach Subrouter as `/backend-api/codex/...`.

`subrouter codex app` intentionally does not inject routing flags. Current Codex accepts `-c` on `codex app`, but the app launcher opens the installed desktop app through the OS and does not carry those overrides into the running app-server. To test desktop routing, start Codex Desktop in an isolated launch environment with the env var above and an isolated `CODEX_HOME` containing the config overrides. Subrouter maps `/backend-api/...` back to the normal ChatGPT backend for OAuth accounts and will not route those backend paths through API-key accounts.

Optional realtime voice routing can be pinned explicitly in the same config:

```toml
experimental_realtime_ws_base_url = "http://127.0.0.1:31415/v1"
```

## User Attribution

Use `SUBROUTER_CODEX_USER_EMAIL` when a teammate should be visible in Subrouter logs and session data:

```bash
SUBROUTER_CODEX_USER_EMAIL=alice@example.com subrouter codex exec "your prompt"
```

Use `SUBROUTER_CODEX_ACCOUNT_ID` when a run should use one explicit Subrouter account, including an API-key account:

```bash
SUBROUTER_CODEX_ACCOUNT_ID=team-codex-1 subrouter codex exec "your prompt"
SUBROUTER_CODEX_ACCOUNT_ID=apikey:team-codex-1 subrouter codex exec "your prompt"
```

`subrouter codex` always uses the custom `subrouter` provider with WebSockets
enabled and sends `X-Subrouter-Agent: codex`. These variables add
`X-Subrouter-User-Email` and/or `X-Subrouter-Account-ID`. Subrouter still
replaces outbound credentials before forwarding upstream.

## Models

There are two separate Codex concepts:

- `model`: the model slug selected by `/model`.
- `model_provider`: the backend/provider config.

Subrouter does not rewrite the `model` field. The wrapper sets
`model_provider = "subrouter"` so local ChatGPT authentication cannot fail
before the proxy sees the request.

Subrouter accepts `/v1/responses` and `/responses`. For OAuth subscription accounts it forwards to `https://chatgpt.com/backend-api/codex` and strips the `/v1` prefix when present. For API-key accounts it forwards to `https://api.openai.com` and adds `/v1` when needed.

## Manual Provider Configuration

If WebSocket support needs to be disabled for debugging, use a custom provider:

```bash
codex exec \
  -c 'model_provider="subrouter"' \
  -c 'model_providers.subrouter.name="Subrouter"' \
  -c 'model_providers.subrouter.base_url="http://127.0.0.1:31415/v1"' \
  -c 'model_providers.subrouter.experimental_bearer_token="subrouter"' \
  -c 'model_providers.subrouter.wire_api="responses"' \
  -c 'model_providers.subrouter.supports_websockets=false' \
  "your prompt"
```

## Env Vars

- `SUBROUTER_CODEX_BASE_URL`: base URL injected by `subrouter codex`; defaults to `http://127.0.0.1:31415/v1`.
- `SUBROUTER_SERVER`: named server from `sr server add` (or `local`) for this one command; ignored when `SUBROUTER_CODEX_BASE_URL` is set.
- `SUBROUTER_CODEX_SERVER`: older alias for `SUBROUTER_SERVER`, used only when `SUBROUTER_SERVER` is unset.
- `SUBROUTER_TAILSCALE_BIN`: optional path to the Tailscale CLI used to repair a named server carrying `--tailscale-node-id`; normal `PATH` and macOS app-bundle locations are detected automatically.
- `SUBROUTER_CODEX_BIN`: Codex binary used by the wrapper; defaults to `codex`.
- `SUBROUTER_CODEX_USER_EMAIL`: optional self-reported user email. When set, the wrapper sends `X-Subrouter-Agent: codex` and `X-Subrouter-User-Email` through a custom Subrouter provider.
- `SUBROUTER_CODEX_ACCOUNT_ID`: optional Subrouter account id or API-key label. When set, the wrapper sends `X-Subrouter-Account-ID` and Subrouter forces that account for the session.
- `OPENAI_API_KEY`: only for real API-key mode or custom env-key providers. Avoid setting it when you want ChatGPT subscription model behavior.
- `CODEX_HOME`: optional. Use it to test an isolated Codex config.
- `OPENAI_ORGANIZATION` and `OPENAI_PROJECT`: Codex forwards these as OpenAI headers for the built-in OpenAI provider.
- `CODEX_OSS_BASE_URL` and `CODEX_OSS_PORT`: only affect OSS providers such as Ollama or LM Studio, not the OpenAI provider.

## "Selected model is at capacity"

That message is OpenAI shedding load for one model and service tier; pressing retry usually gets through. Subrouter retries it for you, but only before any output reached Codex, so nothing is ever duplicated (`ops/PATCHES.md` §27):

- Capacity failover is enabled by default: Subrouter retries the same account once after 250-750ms, then tries other accounts 100-400ms apart (`SUBROUTER_CODEX_OVERLOAD_MAX_ACCOUNTS`, default 3), within a budget of about 10 seconds. A WebSocket turn that hits capacity before any output is closed with 1012 so Codex reconnects on another account. Set `SUBROUTER_CODEX_OVERLOAD_FAILOVER=0` to restore same-account-only behavior.
- A conversation estimated above `SUBROUTER_CODEX_OVERLOAD_FAILOVER_MAX_INPUT` input tokens (default 32000; `0` means no limit) never switches accounts, so its prompt cache is kept. Like any request with the failover off, it waits on its own account: gaps of about 0.5, 1, 2, 4 and 8 seconds, then about every 9 seconds, for up to 4 minutes from the first attempt (`SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT`, `0` = until the client disconnects; `SUBROUTER_CODEX_CAPACITY_RETRY_INTERVAL`, at least 500ms), capped at about 10 seconds when an egress or Azure fallback is configured. Then the failure goes to that fallback, or to Codex. A long wait is logged on its first retry and then about once a minute, and `/_subrouter/health` counts requests currently waiting under `overload_retry_held`.
- Not carried from upstream: the autonomous `sr codex` retry policy (`X-Subrouter-Retry-Policy`, upstream #505), persist mode (`sr codex --persist-capacity`), the `X-Subrouter-Retry` header with `sr codex --retry-interval/--retry-max-wait`, and the per-session account bans and escalating marks of upstream #599/#622.

To retry harder on the same account, set it on the daemon:

```bash
# every 2s after the ramp, for up to 4 minutes (the default cap)
SUBROUTER_CODEX_CAPACITY_RETRY_INTERVAL=2s subrouter serve
```

With the failover on, an account that shed a request ranks below the others for that model and tier for a few minutes, but its sessions stay on it (their prompt cache is there) unless it fails twice in a row. Its first success clears the mark. Without the failover no account is marked. Capacity is never counted as quota.

If you really want to move one conversation, the recommended way is to start or fork a new Codex session (it is placed fresh); you can also pin a launch with `SUBROUTER_CODEX_ACCOUNT_ID`, or have an admin drop the session's assignment with `DELETE /_subrouter/sessions?agent_type=codex&session_id=ID`.

## Azure fallback

`SUBROUTER_AZURE_CODEX_ENDPOINT` plus `SUBROUTER_AZURE_CODEX_API_KEY` (or `SUBROUTER_AZURE_CODEX_CONFIG_FILE` for several Azure resources) lets Subrouter finish a Codex `/responses` request on Azure OpenAI after the pool has spent five retries or has no usable account. It is the last stage of [quota failover](#automatic-quota-failover): it sees a request only after pool rotation, the optional rate-limit-reset credit and the optional peer pool have all failed to serve it. It also absorbs the ChatGPT backend's in-stream `server_is_overloaded` failure ("Selected model is at capacity") on both the SSE and WebSocket transports, and `SUBROUTER_AZURE_CODEX_MODELS` limits which requested models it serves. The session then stays on that Azure endpoint for 30 minutes of activity so its prompt cache keeps hitting.

Force the route to test it:

```bash
sr az status          # which Azure endpoints the daemon armed
sr az test            # one forced request through the daemon
sr az codex exec "…"  # Codex with every request forced onto Azure
```

`sr az codex` pins Codex to a provider that sends `X-Subrouter-Azure: force` and disables WebSockets, because Azure has no WebSocket Responses surface. The daemon rejects a forced request it cannot serve rather than answering from the ChatGPT pool. See the README for the full behavior and configuration.
