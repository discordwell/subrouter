#!/usr/bin/env bash
# Push a Linux build from the laptop to the box over ssh, and run the box's own
# script on it there.
#
#   push-box.sh bootstrap <linux-binary> [--host <ssh-target>]   first install, or a re-render
#   push-box.sh deploy    <linux-binary> [--host <ssh-target>]   upgrade: swap, restart, verify
#
# The ssh target is --host, else SUBROUTER_BOX_SSH: anything ssh takes as a
# destination, such as ubuntu@203.0.113.10 before Tailscale is up, ubuntu@box
# over the tailnet after it, or a Host alias in ~/.ssh/config, which is where a
# key file or a user name belongs. Nothing is sent without one.
#
# Each push uploads the build and the box script to a new directory on the box,
# /tmp/subrouter-push-<stamp>-<pid>. bootstrap runs `sudo bootstrap.sh
# <build>`. deploy runs `deploy-box.sh <sha256> - <build>`, so the box checks
# that the bytes it got are the bytes sent, and reads the admin token from its
# own env file. The directory is removed when the box script exits 0, and kept
# otherwise, for the commands a failed run prints. The exit code is the box
# script's: deploy's 0 (DEPLOY OK), 1 (FAILED), 2 (no exit code, read the log)
# or 3 (INCOMPLETE); 255 is ssh's own failure, and 64 is push-box stopping
# before the box script ran. A deploy runs detached on the box, so one the
# connection loses still finishes there, and the box refuses a second deploy
# until it does.
#
# These variables, when set here, are passed to the box script, and nothing else
# from this shell is:
#   bootstrap  SUBROUTER_PORT SUBROUTER_PUBLIC_PORT SUBROUTER_BOX_NAME
#              ALLOW_UNATTESTED_UPGRADE
#   deploy     ATTEST_FIRST DRAIN_GRACE_SECONDS ROLLBACK_DRAIN_GRACE_SECONDS
#              DRAIN_REPORT_SECONDS READY_WAIT_SECS ALLOW_PRE_ISOLATION_ROLLBACK
#              SUBROUTER_BOX_ADDR
# A value may hold only letters, digits and . _ : - [ ], so it needs no quoting
# in the box's shell.
set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd -P)

fail() { echo "push-box: $*" >&2; exit 64; }
usage() { fail "usage: push-box.sh bootstrap|deploy <linux-binary> [--host <ssh-target>]"; }

VERB="${1:-}"; [ $# -gt 0 ] && shift
BIN=""; HOST="${SUBROUTER_BOX_SSH:-}"
while [ $# -gt 0 ]; do
  case "$1" in
    --host) [ $# -ge 2 ] || usage; HOST="$2"; shift 2 ;;
    --host=*) HOST="${1#--host=}"; shift ;;
    -*) usage ;;
    *) [ -z "$BIN" ] || usage; BIN="$1"; shift ;;
  esac
done

# A directory per push, made by mkdir, which refuses one that exists. A second
# push, such as a retry after a dropped connection, then never writes over the
# files a running deploy still reads: bash reads its script as it runs.
DIR="/tmp/subrouter-push-$(date +%Y%m%d-%H%M%S)-$$"
case "$VERB" in
  bootstrap)
    SCRIPT="$HERE/bootstrap.sh"; REMOTE_BIN=$DIR/subrouter
    PASS="SUBROUTER_PORT SUBROUTER_PUBLIC_PORT SUBROUTER_BOX_NAME ALLOW_UNATTESTED_UPGRADE" ;;
  deploy)
    SCRIPT="$HERE/deploy-box.sh"; REMOTE_BIN=$DIR/subrouter.new
    PASS="ATTEST_FIRST DRAIN_GRACE_SECONDS ROLLBACK_DRAIN_GRACE_SECONDS DRAIN_REPORT_SECONDS READY_WAIT_SECS ALLOW_PRE_ISOLATION_ROLLBACK SUBROUTER_BOX_ADDR" ;;
  *) usage ;;
esac

[ -n "$BIN" ] || usage
[ -f "$BIN" ] || fail "no build at $BIN. Nothing was sent."
[ -n "$HOST" ] || fail "no box to push to: pass --host <ssh-target> or set SUBROUTER_BOX_SSH (an ssh destination, or a Host alias in ~/.ssh/config). Nothing was sent."
# ssh reads a destination that starts with '-' as an option, -oProxyCommand
# included, and a space would split it into a command.
case "$HOST" in
  -*|*[[:space:]]*) fail "'$HOST' is not an ssh destination. Nothing was sent." ;;
esac

ENVS=""
for name in $PASS; do
  value="${!name-}"
  [ -n "${!name+set}" ] || continue
  case "$value" in
    *[!]A-Za-z0-9._:[-]*) fail "$name='$value' holds a character the box's shell would read. Nothing was sent." ;;
  esac
  ENVS="$ENVS $name='$value'"
done

if command -v shasum >/dev/null 2>&1; then
  SHA=$(shasum -a 256 "$BIN" | awk '{print $1}')
else
  SHA=$(sha256sum "$BIN" | awk '{print $1}')
fi
[ "${#SHA}" = 64 ] || fail "could not hash $BIN. Nothing was sent."

# <local file> <remote path> [a command to run first]. Over ssh's own stdin,
# so the box needs nothing but a shell: no scp, and no sftp server.
upload() {
  echo "   $1 -> $HOST:$2"
  ssh -- "$HOST" "${3:+$3 && }cat > $2" < "$1" || fail "could not write $2 on $HOST (ssh exited $?)."
}

REMOTE_SCRIPT="$DIR/$(basename "$SCRIPT")"
echo "== push $VERB to $HOST: $(basename "$BIN") sha=${SHA:0:16}"
upload "$BIN" "$REMOTE_BIN" "mkdir -m 0700 $DIR"
upload "$SCRIPT" "$REMOTE_SCRIPT"
if [ "$VERB" = bootstrap ]; then
  RUN="sudo env$ENVS bash $REMOTE_SCRIPT $REMOTE_BIN"
else
  RUN="env$ENVS bash $REMOTE_SCRIPT $SHA - $REMOTE_BIN"
fi
echo "== $HOST: $RUN"
ssh -- "$HOST" "$RUN"
RC=$?
case "$RC:$VERB" in
  0:*)
    ssh -- "$HOST" "rm -rf $DIR" || echo "push-box: could not remove $DIR on $HOST; the box does not need it." >&2 ;;
  255:deploy)
    echo "push-box: ssh exited 255: the connection failed or was lost. A deploy that started goes on by itself on the box. Do not push again until it ends: the box refuses a second deploy while one runs. Follow it with" >&2
    # shellcheck disable=SC2016 # expanded by the box's shell
    printf '  ssh %s '\''tail -f "$(ls -t ~/.subrouter/transitions/deploy-box-*.log | head -1)"'\''\n' "$HOST" >&2
    echo "and read its exit code in the .rc file beside that log." >&2 ;;
  255:*)
    echo "push-box: ssh exited 255: the connection failed or was lost. Run bootstrap again: a re-run converges." >&2 ;;
esac
exit "$RC"
