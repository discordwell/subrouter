#!/usr/bin/env bash
# Bootstrap a subrouter server on an AWS Lightsail instance.
#
# By default the box is bound to its tailnet address and has NO public listener.
# Set SUBROUTER_PUBLIC_PORT to add the API-only TLS listener on every address.
# A laptop's daemon peers with the box there, pinning this box's certificate,
# over the tailnet and, once the firewall opens the port, over the internet:
#
#   sudo env SUBROUTER_PUBLIC_PORT=31416 bash /tmp/bootstrap.sh /tmp/subrouter
#   SUBROUTER_PUBLIC_PORT=off   ...   explicitly remove an existing one
#   (unset)                           leave whatever is configured alone
#
# SUBROUTER_BOX_NAME (default box) names the box: its Tailscale host name on the
# first `tailscale up`, and the name the printed laptop configuration uses
# (`sr server add <name>`, the peers-file entry, SUBROUTER_PEER_TAILNET). It is
# a DNS label: lower-case letters, digits and hyphens, 63 at most.
#
# Run ON the instance. From the laptop, push-box.sh uploads the binary and this
# script, then runs it:
#   push-box.sh bootstrap <linux-binary> --host <ssh-target>
#
# Idempotent: re-running upgrades the binary and re-renders the unit. Existing
# admin / account-import / proxy tokens are preserved (install-systemd keeps them
# when the flags are omitted), so a re-run does NOT invalidate configured
# clients. That preservation is the whole reason a re-run is safe -- see step 3.
# One exception: it refuses to upgrade an installed pre-isolation build to a
# post-isolation one (see the guard below); use deploy-box.sh ATTEST_FIRST=1.
#
# Adapted from the recipe `sr server install` generates (cmd/subrouter/sr_server.go),
# with two deliberate changes:
#   1. The binary is uploaded, NOT fetched from the public install script. That
#      script installs UPSTREAM, which lacks the startup guard that makes a
#      non-loopback bind refuse rather than serve unauthenticated (PATCHES §12;
#      §37.3 refuses the script).
#   2. Health is polled on the tailnet address, not 127.0.0.1. With a tailnet
#      bind there is no loopback listener, so the upstream poll would hang.

set -euo pipefail

BINARY="${1:-/tmp/subrouter}"
PORT="${SUBROUTER_PORT:-31415}"
PUBLIC_PORT="${SUBROUTER_PUBLIC_PORT:-}"
BOX_NAME="${SUBROUTER_BOX_NAME:-box}"
# Test hook: prefixes the fixed paths this script touches. Empty in real use.
# install-systemd writes $DEFAULTS itself, and the harness's fake binary honours
# the same prefix.
R="${BOOTSTRAP_TEST_ROOT:-}"
INSTALLED="$R/usr/local/bin/subrouter"
SR="$R/usr/local/bin/sr"
STATE_TOKENS="$R/etc/subrouter-bootstrap-tokens"
DEFAULTS="$R/etc/default/subrouter"
SESSIONS="$R/var/lib/subrouter/sessions.json"
LOCK="$R/run/lock/subrouter-deploy.lock"

die() { printf 'bootstrap: %s\n' "$*" >&2; exit 1; }
say() { printf '\n== %s\n' "$*"; }

[ "$(id -u)" -eq 0 ] || die "must run as root (install-systemd requires it); use sudo"
[ -f "$BINARY" ]     || die "binary not found at $BINARY"
# A Tailscale host name, a peers-file name and a SUBROUTER_PEER_TAILNET key at
# once, so it is held to the narrowest of the three.
case "$BOX_NAME" in
  ""|-*|*-|*[!a-z0-9-]*) die "SUBROUTER_BOX_NAME must be lower-case letters, digits and hyphens, and must not start or end with a hyphen; got '$BOX_NAME'. Nothing was changed." ;;
esac
[ "${#BOX_NAME}" -le 63 ] || die "SUBROUTER_BOX_NAME is longer than 63 characters. Nothing was changed."

# Refuse to carry an existing box across the Codex credential-isolation
# boundary. A post-isolation build refuses every stored Codex record that a
# pre-isolation build wrote, and this script checks only /health: it would ship
# a box with the whole Codex pool refused, and nothing would say so. That
# upgrade needs the old daemon stopped while the records are attested, which is
# deploy-box.sh's ATTEST_FIRST=1 mode. The marker: every post-isolation build
# carries the attest-legacy verb, and none before it does.
if [ -f "$INSTALLED" ] && ! LC_ALL=C grep -aq attest-legacy "$INSTALLED" &&
  LC_ALL=C grep -aq attest-legacy "$BINARY" && [ "${ALLOW_UNATTESTED_UPGRADE:-0}" != 1 ]; then
  die "the installed binary predates Codex credential isolation and $BINARY does not.
       Upgrade with deploy-box.sh instead: with ATTEST_FIRST=1 it attests the Codex
       store with the old daemon stopped. From the laptop:
         ATTEST_FIRST=1 push-box.sh deploy <binary> --host <ssh-target>
       Set ALLOW_UNATTESTED_UPGRADE=1 to install it anyway, then attest by hand."
fi

# One deploy or bootstrap at a time: both replace the binary and restart the
# service. The same lock as deploy-box.sh's, whose comment says why only root
# writes it.
command -v flock >/dev/null 2>&1 ||
  die "flock (util-linux) is needed to keep a bootstrap and a deploy apart. Nothing was changed."
{ : >>"$LOCK" && chmod 0644 "$LOCK" && exec 9<"$LOCK"; } 2>/dev/null ||
  die "cannot open the deploy lock $LOCK. Nothing was changed."
flock -n 9 ||
  die "a deploy or another bootstrap is running ($(cat "$LOCK" 2>/dev/null)). Let it end, then run this again. Nothing was changed."
printf 'bootstrap.sh, pid %s\n' "$$" >"$LOCK"

say "1/7 tailscale"
if ! command -v tailscale >/dev/null 2>&1; then
  curl -fsSL https://tailscale.com/install.sh | sh
fi
# --ssh lets us reach the box over the tailnet, which is what makes it safe to
# close port 22 afterwards. Interactive on first run: it prints an auth URL.
# --hostname only on that first run: a re-run never renames a box peers know.
if ! tailscale status >/dev/null 2>&1; then
  say "    tailscale needs authentication -- follow the URL it prints"
  tailscale up --ssh --hostname "$BOX_NAME"
fi
TSIP="$(tailscale ip -4 | head -1)"
[ -n "$TSIP" ] || die "no tailnet IPv4 address; is 'tailscale up' complete?"
printf '    tailnet address: %s\n' "$TSIP"

say "2/7 install binary as /usr/local/bin/subrouter (+ sr symlink)"
# Staged and checked beside the installed binary, then renamed over it: a build
# for the wrong architecture, or one without the guard below, must be refused
# before it replaces a working one.
STAGE="$INSTALLED.stage"
install -m 0755 "$BINARY" "$STAGE"
if ! "$STAGE" --help >/dev/null 2>&1; then
  rm -f "$STAGE"
  die "$BINARY does not run here. Build it for this box's architecture, $(uname -m)
       (GOARCH=arm64 for aarch64, amd64 for x86_64). Nothing was installed."
fi

# Refuse an unpatched build rather than discover it at bind time.
#
# Uses `grep -a`, NOT `strings`: binutils is absent from a stock Lightsail
# Ubuntu image, so the strings version of this check died with "binary lacks the
# guard" on a perfectly good binary -- a false security failure that reads as
# "you built from the wrong branch". LC_ALL=C keeps grep from treating the
# binary as invalid text in a UTF-8 locale.
if ! LC_ALL=C grep -aq "refusing to bind" "$STAGE"; then
  rm -f "$STAGE"
  die "this binary lacks the non-loopback admin-token guard -- it was built from
       upstream. Build it from this fork (GOOS=linux go build ./cmd/subrouter)
       and retry. Nothing was installed."
fi
mv -f "$STAGE" "$INSTALLED"
# argv[0] selects the CLI surface (main.go: filepath.Base(os.Args[0])), so the
# `sr` name is not cosmetic -- install-systemd is only reachable through it.
ln -sf "$INSTALLED" "$SR"

say "3/7 tokens"
# THREE tokens, and the proxy token is deliberately NOT equal to the admin one.
#
# It used to be, with the reasoning that a client sends one bearer for both
# surfaces so the two had to match. That stopped being true when the admin token
# became a strict superset (localProxyAuthorized accepts it), and it is now
# actively wrong: the proxy token is the credential presented on the PUBLIC
# listener, so making them equal publishes the admin secret to anyone entitled
# to read stats. Admin is private-only and never valid on the public surface.
#
# A single-bearer client configured with the ADMIN token still works for both,
# which is what makes splitting them safe to do on an existing box.
if [ -f "$STATE_TOKENS" ]; then
  printf '    reusing existing tokens from %s\n' "$STATE_TOKENS"
  # shellcheck disable=SC1090
  . "$STATE_TOKENS"
fi

# The env file is the SECOND source, ahead of minting a fresh secret.
#
# Whatever is live there is what configured clients are actually holding, and
# this state file is not the only way a box gets configured -- one set up by
# hand, or whose state file was lost, has no record here at all. Minting in that
# case does not fail loudly: it rotates a working credential, install-systemd
# writes the new value, and every laptop and peer is locked out at once. The
# symptom is "the box stopped accepting me", which points at the client.
adopt_live() { # variable name, env-file key
  local current="${!1:-}"
  [ -n "$current" ] && return 0
  [ -f "$DEFAULTS" ] || return 0
  local live
  live="$(sed -n "s/^$2=\"\{0,1\}\([^\"]*\)\"\{0,1\}$/\1/p" "$DEFAULTS" | head -1)"
  [ -n "$live" ] || return 0
  printf -v "$1" '%s' "$live"
  printf '    adopted the live %s from %s\n' "$2" "$DEFAULTS"
}
adopt_live ADMIN_TOKEN  SUBROUTER_ADMIN_TOKEN
adopt_live IMPORT_TOKEN SUBROUTER_ACCOUNT_IMPORT_TOKEN
ADMIN_TOKEN="${ADMIN_TOKEN:-$(openssl rand -hex 32)}"
IMPORT_TOKEN="${IMPORT_TOKEN:-$(openssl rand -hex 32)}"

# The proxy token adopts the same way, with one extra condition: a live value
# EQUAL to the admin token is the legacy state this script used to create, and
# adopting it would carry that mistake forward. Mint a separate one instead.
if [ -z "${PROXY_TOKEN:-}" ] && [ -f "$DEFAULTS" ]; then
  LIVE_PROXY="$(sed -n 's/^SUBROUTER_PROXY_TOKEN="\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' "$DEFAULTS" | head -1)"
  if [ -n "$LIVE_PROXY" ] && [ "$LIVE_PROXY" != "$ADMIN_TOKEN" ]; then
    PROXY_TOKEN="$LIVE_PROXY"
    printf '    adopted the live SUBROUTER_PROXY_TOKEN from %s\n' "$DEFAULTS"
  fi
fi
PROXY_TOKEN="${PROXY_TOKEN:-$(openssl rand -hex 32)}"

if [ "$PROXY_TOKEN" = "$ADMIN_TOKEN" ]; then
  die "the proxy and admin tokens are identical. The proxy token is presented on
       the PUBLIC listener; making it equal to admin hands full control of the box
       to anyone who can read stats. Delete SUBROUTER_PROXY_TOKEN from $DEFAULTS
       and re-run to mint a separate one."
fi

umask 077
cat >"$STATE_TOKENS" <<EOF
ADMIN_TOKEN=$ADMIN_TOKEN
IMPORT_TOKEN=$IMPORT_TOKEN
PROXY_TOKEN=$PROXY_TOKEN
EOF
chmod 0600 "$STATE_TOKENS"
printf '    admin=%s… import=%s… proxy=%s…  (%s, mode 0600)\n' \
  "${ADMIN_TOKEN:0:8}" "${IMPORT_TOKEN:0:8}" "${PROXY_TOKEN:0:8}" "$STATE_TOKENS"

say "4/7 install systemd unit bound to $TSIP:$PORT"
# The public listener, if one is wanted. Bound to 0.0.0.0 because that is the
# entire point -- it is reached from off-tailnet. The PRIVATE address stays
# $TSIP and step 7 asserts it.
#
# Passed through --extra-args, which install-systemd otherwise PRESERVES when
# omitted; so leaving SUBROUTER_PUBLIC_PORT unset keeps whatever is configured
# rather than silently tearing it down on an unrelated binary upgrade.
EXTRA_ARGS_FLAG=()
case "$PUBLIC_PORT" in
  "")        printf '    public listener: leaving existing configuration untouched\n' ;;
  off|none|0) EXTRA_ARGS_FLAG=(--extra-args "")
             printf '    public listener: REMOVING (SUBROUTER_PUBLIC_PORT=%s)\n' "$PUBLIC_PORT" ;;
  *[!0-9]*)  die "SUBROUTER_PUBLIC_PORT must be a port number, or 'off'; got '$PUBLIC_PORT'" ;;
  *)         EXTRA_ARGS_FLAG=(--extra-args "--public-addr 0.0.0.0:$PUBLIC_PORT")
             printf '    public listener: 0.0.0.0:%s (API only; admin stays on %s unless SUBROUTER_PUBLIC_ADMIN=1)\n' "$PUBLIC_PORT" "$TSIP" ;;
esac

# Tokens go over stdin so they never appear in ps output. Order is fixed by
# install-systemd: admin, import, proxy.
#
# The proxy token matters more than it looks. Without it the daemon REFUSES TO
# START on a non-loopback bind -- the guard added 2026-08-13, after a tokenless
# tailnet bind served /v1/messages, and every pooled subscription behind it, to
# any host the ACLs admit. That failure surfaces at step 6 as "health never came
# up", which reads like a broken build rather than a missing setting; this script
# hit exactly that.
printf '%s\n%s\n%s\n' "$ADMIN_TOKEN" "$IMPORT_TOKEN" "$PROXY_TOKEN" \
  | "$SR" install-systemd \
      --addr "$TSIP:$PORT" \
      --admin-token-stdin \
      --account-import-token-stdin \
      --proxy-token-stdin \
      "${EXTRA_ARGS_FLAG[@]}"

# Older binaries have no --proxy-token-stdin and drop the key on every render,
# so verify rather than assume. Checked with grep -c on a file this script owns:
# a silent 0 here is the difference between a running daemon and one that
# refuses to bind.
PROXY_LINES="$(grep -c '^SUBROUTER_PROXY_TOKEN=' "$DEFAULTS" || true)"
if [ "$PROXY_LINES" != "1" ]; then
  die "expected exactly 1 SUBROUTER_PROXY_TOKEN line in $DEFAULTS, found $PROXY_LINES.
       An older subrouter binary without --proxy-token-stdin will drop it; rebuild
       from a commit that carries the flag."
fi

# install-systemd already tried to start it and the guard may have stopped it, so
# the restart counter can be exhausted; clear it or the restart is a no-op.
systemctl reset-failed subrouter.service subrouter.socket 2>/dev/null || true
systemctl restart subrouter.service

say "5/7 wait for health on the tailnet address"
i=0
until curl -fsS "http://$TSIP:$PORT/_subrouter/health" >/dev/null 2>&1; do
  i=$((i+1))
  [ "$i" -ge 30 ] && die "health never came up; check: journalctl -u subrouter -n 50"
  sleep 1
done
printf '    healthy after %ss\n' "$i"

say "6/7 the ADMIN surface is not exposed"
# This assertion does NOT become conditional on the public listener. The private
# port carries admin -- accounts, drain, transcripts, credential minting, and a
# loopback bypass that skips the token check entirely -- and must be tailnet-only
# whether or not an API-only public listener exists beside it. Relaxing it
# because "we expose a port now" is precisely the conflation the two-listener
# split exists to prevent.
# A POSITIVE assertion -- every listener on $PORT must be exactly $TSIP:$PORT --
# rather than a denylist of wildcard spellings.
#
# The denylist this replaces grepped for `0.0.0.0:$PORT` and `[::]:$PORT`, and a
# wildcard bind does not necessarily render as either: ss prints a dual-stack
# wildcard as `*:31415`. So the check passed while the admin surface was bound to
# the world, and printed a line saying it was confirmed tailnet-only. Enumerating
# the ways an address can be spelled fails open; naming the one address allowed
# fails closed.
BAD_BINDS="$(ss -ltn 2>/dev/null | awk -v suffix=":$PORT" '$4 ~ suffix"$" {print $4}' \
  | grep -vx "$TSIP:$PORT" || true)"
if [ -n "$BAD_BINDS" ]; then
  die "port $PORT carries the admin surface and is bound to: $BAD_BINDS
       -- expected $TSIP:$PORT only"
fi
printf '    confirmed: admin port %s is bound to %s only\n' "$PORT" "$TSIP"

# What IS allowed to be wildcard-bound is the public API port, and only that one.
#
# The effective port is read back from the rendered env file rather than from
# $PUBLIC_PORT, because the three input cases (set / off / unset-and-preserved)
# do not all name it: a re-run that preserves an existing listener still has to
# print its pin, and inferring "no listener" from an unset variable would skip
# exactly that case.
PUBLIC_URL=""
FINGERPRINT=""
# [^ ]* rather than [^:]*, so the port is the digits after the LAST colon: an
# IPv6 bind such as `--public-addr [::]:31416` has colons inside the address, and
# a pattern that stops at the first one yields NOTHING -- which this script would
# then read as "no public listener" while one is running, skipping both the bind
# check and the pin.
EFFECTIVE_PUBLIC_PORT="$(sed -n 's/.*--public-addr[ =][^ ]*:\([0-9]\{1,\}\).*/\1/p' "$DEFAULTS" | head -1)"
if [ -n "$EFFECTIVE_PUBLIC_PORT" ]; then
  # Same field-suffix match, for the same reason: any address is acceptable here,
  # so this only asks whether SOMETHING is listening on the port.
  # grep reads to EOF: under pipefail, `| grep -q` can report a match as a
  # failure when awk is still writing (SIGPIPE).
  if ! ss -ltn 2>/dev/null | awk '{print $4}' | grep ":$EFFECTIVE_PUBLIC_PORT\$" >/dev/null; then
    die "public listener is configured on $EFFECTIVE_PUBLIC_PORT but nothing is bound there;
         check: journalctl -u subrouter -n 50"
  fi
  printf '    confirmed: public API port %s is listening\n' "$EFFECTIVE_PUBLIC_PORT"

  say "7/7 certificate pin"
  # Read AFTER the daemon has started, deliberately. The daemon generates the
  # identity as the unprivileged user it runs as; if this script generated it
  # first, as root, the files would be 0600 root-owned and the daemon could not
  # read them -- and LoadOrCreateBoxIdentity refuses an unreadable identity
  # rather than replacing one every peer has pinned, so it would fail to start.
  # `fingerprint` is read-only for that reason and cannot cause it either way.
  FINGERPRINT="$("$INSTALLED" fingerprint --sessions "$SESSIONS")" \
    || die "could not read the certificate pin; peers cannot be configured without it"
  # Only for the printed URL. Under set -e a failed lookup would end the run
  # here, silently, before the laptop configuration is printed.
  PUBLIC_IP="$(curl -fsS --max-time 10 https://checkip.amazonaws.com 2>/dev/null | tr -d '[:space:]')" || PUBLIC_IP=""
  PUBLIC_URL="https://${PUBLIC_IP:-<public-ip>}:$EFFECTIVE_PUBLIC_PORT"
  printf '    pin: %s\n' "$FINGERPRINT"
else
  say "7/7 certificate pin -- skipped, no public listener"
fi

cat <<EOF

== done

Server:        http://$TSIP:$PORT
Admin token:   $ADMIN_TOKEN
Import token:  $IMPORT_TOKEN
Proxy token:   $PROXY_TOKEN

The two tokens are NOT interchangeable, and which one you hand out matters:

  admin  enumerates accounts, drains, mints credentials. Tailnet/loopback only,
         never accepted on the public listener. Treat it as the box itself.
  proxy  spends the pool and reads stats. This is the one a peer or a public
         client gets. Admin also opens the proxy surface, so a client holding
         admin needs nothing else.

== the laptop's sr (Tailscale must be on the same tailnet)

sr reaches this box only on a laptop that holds remote.key (PATCHES §17).
Without the key, sr answers 'unknown account command "server"', as to a typo.

  sr server add $BOX_NAME --url http://$TSIP:$PORT \\
    --admin-token '$ADMIN_TOKEN' \\
    --account-import-token '$IMPORT_TOKEN' --default
EOF

if [ -n "$FINGERPRINT" ]; then
cat <<EOF

== the laptop's daemon (a peer of this box)

It reads its peers from the file SUBROUTER_REMOTE_POOLS_FILE names (mode 0600),
and only on a machine that holds remote.key (PATCHES §17). This box's entry, over
the tailnet:

  {"peers": [{"name": "$BOX_NAME", "url": "https://$TSIP:$EFFECTIVE_PUBLIC_PORT", "token": "$PROXY_TOKEN", "fingerprint": "$FINGERPRINT"}]}

== public access (no Tailscale)

  Public URL:  $PUBLIC_URL
  Pin:         $FINGERPRINT

Add the fallback path to sr's entry:

  sr server add $BOX_NAME --url http://$TSIP:$PORT \\
    --public-url '$PUBLIC_URL' \\
    --fingerprint '$FINGERPRINT'

and to the daemon's: set the entry's "url" to $PUBLIC_URL, and
SUBROUTER_PEER_TAILNET=$BOX_NAME=$TSIP in the daemon's environment, so it still
dials the tailnet first, with the same pin (PATCHES §68).

The tailnet URL stays PREFERRED; the public one is a fallback for when the
tailnet is unavailable -- a VPN kill switch, for instance, blocks it while
leaving everything else routed.

The pin is not decoration. There is no CA behind this certificate, so the
fingerprint IS the trust decision: a peer configured without it refuses to
connect rather than trusting whatever it is handed.

LAST, and only once the above is verified end to end, open the port:
  Lightsail console > Networking > IPv4 Firewall > add TCP $EFFECTIVE_PUBLIC_PORT.

That step is the irreversible one. Scanners find an open port within hours and
it cannot be un-found, so leave it until the pin, the token split, and the
404s on every admin path have all been confirmed from off-tailnet.
EOF
else
cat <<EOF

== the laptop's daemon

It peers with a box over the box's TLS listener, which this box does not have.
To add it, from the laptop:
  SUBROUTER_PUBLIC_PORT=31416 push-box.sh bootstrap <linux-binary> --host <ssh-target>
EOF
fi

cat <<EOF

A new box's pool is empty. Read ops/deploy/lightsail/README.md before moving
an account to it: a move is a cutover, not a copy, and doing it wrong costs an
interactive re-login per account.

Only after the tailnet path is confirmed working, close public SSH:
  Lightsail console > Networking > delete the SSH (22) rule.
  You will still reach the box via 'ssh ubuntu@$BOX_NAME' over the tailnet.
EOF
