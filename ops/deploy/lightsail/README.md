# Run a box on AWS Lightsail

A box is a second subrouter daemon on a small Linux machine that stays on while the laptop comes and goes. The laptop reaches it over Tailscale. It costs about $5 a month, and needs no load balancer and no NAT gateway.

| File | Runs on | What it does |
|---|---|---|
| `push-box.sh` | the laptop | Uploads a Linux build and one of the two scripts below to the box over ssh, and runs it there. |
| `bootstrap.sh` | the box | Installs Tailscale and the daemon, keeps or mints its three tokens, and prints the laptop's configuration. |
| `deploy-box.sh` | the box | Swaps in a new build, verifies both listeners, and rolls back on a failed check. |

Upstream's remote installer, `sr server install`, installs upstream's release, which this fork refuses (PATCHES §37.3). `bootstrap.sh` adapts the same recipe, and its header names the two differences.

## Read this before you move an account

Refresh tokens rotate, and the daemon saves each new one. Both providers send `grant_type=refresh_token` and store what comes back (`internal/agents/claude/store.go` for Claude, `internal/accounts/codex_auth.go` for Codex).

So two daemons that hold the same account invalidate each other. The first to refresh gets the new token, and the other keeps a superseded one. A refresh is serialized only within one process.

A dead credential still parses and still looks unexpired, and it gets 401 forever. Only a new interactive login recovers it, and a Claude login cannot finish by itself: it prints a URL and waits for a pasted code. Ten accounts cost ten of those.

Move one account at a time, and never let both daemons hold the same one. A move is a cutover, not a copy.

## 1. Create the instance

In the Lightsail console, or with the AWS CLI:

```bash
aws lightsail create-instances \
  --region <region> \
  --instance-names <instance-name> \
  --availability-zone <availability-zone> \
  --blueprint-id ubuntu_24_04 \
  --bundle-id nano_3_0
```

`nano_3_0` is the $5 bundle, with 512 MB of memory and a public IPv4 address. `micro_3_0`, at $7, has 1 GB. Attach a static IP to the instance in the console, under Networking, so that the address stays the same. Pick a region near you, because every request the box serves pays this hop. Bundle and blueprint IDs change, so confirm them with `aws lightsail get-bundles` and `aws lightsail get-blueprints`.

Give the instance a `Host` entry in `~/.ssh/config`, so that every command below names it by alias:

```text
Host box-public
  HostName 203.0.113.10
  User ubuntu
  IdentityFile ~/.ssh/<lightsail-key>.pem
```

## 2. Build the binary

Build from this repo. Do not use upstream's install script or release: they lack the guard that makes a non-loopback bind refuse to serve without a proxy token (PATCHES §12).

Check the box's architecture first. `ssh box-public uname -m` prints `x86_64` for `GOARCH=amd64`, or `aarch64` for `GOARCH=arm64`. From the repo root:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/subrouter-linux ./cmd/subrouter
grep -a -c 'refusing to bind' /tmp/subrouter-linux    # 1 or more
```

`bootstrap.sh` runs the build and checks for the same guard before it replaces the installed binary. It refuses a build that fails either check.

## 3. Bootstrap the box

```bash
SUBROUTER_PUBLIC_PORT=31416 SUBROUTER_BOX_NAME=box \
  ops/deploy/lightsail/push-box.sh bootstrap /tmp/subrouter-linux --host box-public
```

On the first run, `tailscale up` prints a URL. Open it to add the box to your tailnet. The rest runs unattended.

- `SUBROUTER_BOX_NAME` names the box. It becomes the box's Tailscale host name on the first run, and the name in the laptop configuration that bootstrap prints. The default is `box`.
- `SUBROUTER_PUBLIC_PORT=31416` adds the TLS listener, on every address. The laptop's daemon peers with the box there, pinned to the box's certificate. Until you open the port in the Lightsail firewall, only the tailnet reaches it. Left unset, a re-run keeps what is configured, and `off` removes the listener.
- The private listener, on `:31415`, carries the admin surface. It binds only the box's tailnet address, and bootstrap checks that.

Bootstrap prints the box's tailnet address, the admin, import and proxy tokens, and the certificate pin. It also prints the `sr server add` command and the peers-file entry that step 4 uses. A re-run keeps the tokens, so the laptop's configuration stays valid. If the connection drops during bootstrap, run it again: a re-run converges. Bootstrap refuses to cross the Codex credential-isolation boundary (PATCHES §23). Use `push-box.sh deploy` with `ATTEST_FIRST=1` for that.

After this step, reach the box over the tailnet as `ubuntu@box`, or as `ubuntu@100.64.0.10` if MagicDNS is off.

## 4. Point the laptop at the box

The laptop uses the box in two ways, and each has its own configuration. Both work only on a laptop that holds `remote.key` (PATCHES §17; `ops/bin/subrouter-remote-key` sets it up). Without the key, `sr` rejects `sr server` and `sr claude move` the way it rejects a typo, with `unknown account command "server"` and `unknown command: sr claude move`, and the daemon ignores its peers file.

`sr` runs admin commands against the box, such as account moves and status. Paste the command that bootstrap printed:

```bash
sr server add box --url http://100.64.0.10:31415 \
  --admin-token '<admin token>' --account-import-token '<import token>' --default
curl -fsS http://100.64.0.10:31415/_subrouter/health
```

The health route answers without a token, so it works as a liveness check from any machine on the tailnet.

The laptop's daemon sends turns to the box when its own accounts run low. It reads its peers from the file that `SUBROUTER_REMOTE_POOLS_FILE` names, which must be mode 0600. Bootstrap prints the box's entry:

```json
{"peers": [{"name": "box", "url": "https://100.64.0.10:31416", "token": "<proxy token>", "fingerprint": "<pin>"}]}
```

Put `SUBROUTER_REMOTE_POOLS_FILE` in the daemon's environment. Under the supervisor, that is the `env` of `~/.subrouter/worker-config.json`, followed by a deploy of the installed binary (`ops/LAPTOP.md`).

To keep the box reachable when the tailnet is down, open the port in the Lightsail firewall last, once the pin, the token split, and the 404 on every admin path are confirmed. Then set the entry's `url` to `https://203.0.113.10:31416`, and add `SUBROUTER_PEER_TAILNET=box=100.64.0.10` beside `SUBROUTER_REMOTE_POOLS_FILE`. The daemon still dials the tailnet first, with the same pin, and falls back to the public address (PATCHES §68). Add the same fallback for `sr`:

```bash
sr server add box --url http://100.64.0.10:31415 \
  --public-url 'https://203.0.113.10:31416' --fingerprint '<pin>'
```

The pin is the trust decision, because no CA signs this certificate. Take it from bootstrap's output, or from `sudo subrouter fingerprint --sessions /var/lib/subrouter/sessions.json` on the box over the tailnet. Never take it from the public address: whoever answers there gives you their own.

The proxy token spends the pool and reads its stats, which is what a peer needs. The admin token is never accepted on `:31416`. To give the laptop a credential you can revoke alone, mint a peer-role token on the box and put it in the entry's `token` (PATCHES §43.1):

```bash
sudo -u subrouter /usr/local/bin/subrouter token mint laptop --role peer --expires 90d \
  --sessions /var/lib/subrouter/sessions.json
```

## 5. Move accounts, one at a time

```bash
sr claude move <name> box
```

`move` uploads the credential, confirms that the box can use it, and then removes the local profile, so only the box refreshes the token. A local profile left behind is still refreshed by the laptop's daemon, and one of the two copies dies. Any failure keeps the local profile.

Check with the box, not with the client:

```bash
curl -fsS -H "Authorization: Bearer <admin token>" http://100.64.0.10:31415/_subrouter/accounts
```

Start with the account that has the least left this week, which `srq pool` shows. If the move goes wrong, you lose little. `sr server login box` signs a Codex account in on the box directly.

## 6. Close public SSH, last

Once the tailnet path works, delete the SSH (22) rule in the Lightsail console, under Networking. You keep access as `ssh ubuntu@box` over Tailscale SSH. Do this before Tailscale works, and you are locked out of a box that holds your credentials.

## Deploy an upgrade

```bash
ops/deploy/lightsail/push-box.sh deploy /tmp/subrouter-linux --host ubuntu@box
```

`--host` can come from `SUBROUTER_BOX_SSH` instead. Without either, nothing is sent. `push-box.sh` uploads the build and `deploy-box.sh` to a new directory on the box, `/tmp/subrouter-push-<stamp>-<pid>`, and runs `deploy-box.sh <sha256> - <build>` there. The box checks that it got the bytes that were sent, and reads the admin token from its own env file, so the token is never in `ps`. `push-box.sh` removes the directory when the deploy exits 0, and keeps it otherwise. The exit code is the box's:

| Exit | Line | Meaning |
|---|---|---|
| 0 | `DEPLOY OK` | The new build serves, and every check passed. |
| 1 | `DEPLOY FAILED` | A check failed. The previous build was restored, unless the message says why it was not. |
| 2 | | The detached run ended without an exit code. Read its log. |
| 3 | `DEPLOY INCOMPLETE` | The new build serves and is kept, but Codex records fail its credential-isolation gate. The message prints the commands that fix it. |
| 64 | | `push-box.sh` stopped before the box script ran, for example without a host or on a failed upload. |
| 255 | | ssh failed, or the connection dropped. A deploy that started goes on, and `push-box.sh` prints how to follow it. |

`deploy-box.sh` stages the build beside the running binary and renames it, never writing over a running file. Then it restarts the daemon and checks:

- that the provider path and the admin path answer on the private listener;
- that the box lists at least as many accounts as before the swap;
- that the public listener, when there is one, reads stats with the proxy token, refuses the admin token, and answers 404 on every private route;
- that `/_subrouter/ready` turns ok, which only warns, and that the Codex records pass the isolation check.

The private listener is `SUBROUTER_BOX_ADDR`, else `SUBROUTER_ADDR` in `/etc/default/subrouter`. It refuses to run with neither, and it refuses an IPv6 address without brackets, such as `2001:db8::1:31415` instead of `[2001:db8::1]:31415`. The public listener's port is the one `--public-addr` names in the env file's `SUBROUTER_EXTRA_ARGS`, and no variable overrides it. When the env file names one and nothing listens there after the restart, the deploy rolls back.

The deploy runs detached on the box, as the laptop's does (PATCHES §69.5). If the connection drops, the deploy goes on, verifies, and rolls back on a failed check. It writes its output to `~/.subrouter/transitions/deploy-box-<stamp>-<pid>.log` on the box, and its exit code to the `.rc` file beside the log. After a drop, follow the deploy, and do not push again until it ends:

```bash
ssh ubuntu@box 'tail -f "$(ls -t ~/.subrouter/transitions/deploy-box-*.log | head -1)"'
```

The box runs one deploy or bootstrap at a time. Each holds `/run/lock/subrouter-deploy.lock` for the whole run. A second one is refused before it changes anything, and the message names the run that holds the lock, with a deploy's log. The deploy needs `python3` on the box, and both scripts need `flock`. Ubuntu images include both.

`push-box.sh` passes these to the box when they are set, and nothing else:

| Variable | Effect |
|---|---|
| `DRAIN_GRACE_SECONDS` | How long the old process may drain before it is forced. The default is the drain ceiling: the shorter of the daemon's `--shutdown-timeout` and the unit's `TimeoutStopSec`, 600 s on a bootstrapped box, and never more than 900 s. `:31416` refuses connections for that long. |
| `ROLLBACK_DRAIN_GRACE_SECONDS` | The same for the build a rollback abandons. The default is 60 s, or `DRAIN_GRACE_SECONDS` when that is shorter. |
| `DRAIN_REPORT_SECONDS` | How often the wait reports the turns in flight. The default is 30. |
| `READY_WAIT_SECS` | How long to wait for `/_subrouter/ready`. The default is 60. |
| `ATTEST_FIRST=1` | For the first deploy past the Codex credential-isolation boundary. It stops the old daemon, attests the Codex store with the new binary, and then starts it: an outage of about a minute. `deploy-box.sh`'s header lists each step. |
| `ALLOW_PRE_ISOLATION_ROLLBACK=1` | Lets a deploy or a rollback restore a build from before the isolation boundary after a Codex record was migrated. Without it, both refuse. |
| `SUBROUTER_BOX_ADDR` | The private listener's `host:port`. It overrides `SUBROUTER_ADDR` in the env file. |

A value that is not a whole number stops the deploy before it changes anything. Before the restart, the script drains the running build over its drain socket, unless the build is draining already. It never lifts a drain that it did not make. Do not drain by hand before a deploy: a restart drains by itself (PATCHES §38).

Re-running `bootstrap.sh` with a newer build also upgrades the box, and keeps the tokens. It checks only `/_subrouter/health`, though, and its restart waits out the whole drain.

## Drain the box by hand

A manual drain takes the box out of rotation until a resume or the next restart.

- On the box, with no token: `sudo subrouter drain`, `sudo subrouter drain-status`, and `sudo subrouter resume`. They use `/var/lib/subrouter/drain.sock`, which serves only those three (PATCHES §38.1).
- From any machine on the tailnet that holds the admin token: `curl -X POST -H "Authorization: Bearer <admin token>" http://100.64.0.10:31415/_subrouter/drain`. `/_subrouter/resume` undoes it.
- With `SUBROUTER_PUBLIC_ADMIN=1` in `/etc/default/subrouter`, a named admin token can also drain over `:31416`, and the static admin token never can (PATCHES §43.1). Set the variable, never `--public-admin` in `SUBROUTER_EXTRA_ARGS`: a rollback would restart an older build with a flag it does not know, so `deploy-box.sh` refuses the flag.

A deploy that stops before its restart, on a sha mismatch or a failed backup, leaves a drained box drained.

## Notes

- The daemon logs to stderr, which systemd keeps: `sudo journalctl -u subrouter`. An empty log file does not mean no traffic.
- The box's socket unit binds the tailnet address, which does not exist yet at boot. `install-systemd` writes `FreeBind=true` for that (PATCHES §37.2), but `deploy-box.sh` never renders the units. On a box bootstrapped before §37, check with `systemctl cat subrouter.socket | grep FreeBind`, and re-run bootstrap to add it.
- The endgame concurrency taper counts per account across every client of the box. Two laptops draw against the same cap.

## Test

The harness stubs ssh, sudo, systemctl, curl and the binary, and moves every fixed path under its own directory. It never reaches a real box.

```bash
(cd ops && go test -count=1 -run 'TestDeployScripts/(box|boot|push)-' ./deploy/tests)
bash ops/deploy/tests/deploy-scripts-test.sh '=box-default-ok'
```
