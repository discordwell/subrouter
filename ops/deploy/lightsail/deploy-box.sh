#!/usr/bin/env bash
# Deploy a subrouter build to the box, verifying BOTH surfaces and rolling back
# automatically if either fails.
#
#   bash deploy-box.sh <sha256> - [<binary>]                  swap, restart, verify
#   ATTEST_FIRST=1 bash deploy-box.sh <sha256> - [<binary>]   stop, attest, then start
#
# From the laptop, push-box.sh uploads the build and this script, and runs it:
#   push-box.sh deploy <linux-binary> --host <ssh-target>
#
# "-" (or nothing) reads the admin token from the box's env file
# (SUBROUTER_ADMIN_TOKEN), as the proxy and import tokens always were. Passing
# the token itself still works, but puts it in argv, where `ps` shows it to
# every user on the box for the whole deploy -- up to a 600 s drain.
#
# The private listener it verifies is SUBROUTER_BOX_ADDR (host:port), else the
# env file's SUBROUTER_ADDR, which install-systemd always writes. With neither,
# or a value that is not host:port, it stops before anything changes.
#
# <binary> is the build to install, /tmp/subrouter.new when it is left out;
# push-box.sh passes the copy it uploaded to a directory of its own. Exit codes:
#   0  DEPLOY OK
#   1  DEPLOY FAILED -- rolled back, OR a rollback was refused (read the message)
#   2  the detached run ended without an exit code (read its log)
#   3  DEPLOY INCOMPLETE -- the new build is serving and is KEPT, but Codex
#      accounts fail its credential-isolation gate; the message says how to fix
#
# The run goes on detached from the shell that started it (PATCHES §69.5's
# reason, on the box): in its own session, with HUP, INT and TERM ignored, and
# its output in ~/.subrouter/transitions/deploy-box-<stamp>-<pid>.log, with the
# exit code in the .rc beside it. That shell streams the log and exits with the
# run's code. A lost ssh connection stops only the streaming: the deploy still
# verifies, and still rolls back, and the log says how it ended.
#
# One deploy or bootstrap at a time. The detached run holds a flock(1) lock on
# /run/lock/subrouter-deploy.lock until it exits, as bootstrap.sh does, and a
# run that finds it held is refused before step 1 with the holder's log.
#
# Runs ON the box. Two rules it exists to enforce:
#
#  * Never write over the running binary. `cp` onto a live inode poisons the
#    path (macOS SIGKILLs every later exec; Linux gives ETXTBSY), and a rollback
#    that also uses cp cannot work. Stage beside it, then rename.
#  * Never declare success from the restart alone. This build changes how EVERY
#    route is mounted, so the failure mode is "a route stopped existing" -- which
#    a health check on one endpoint would sail straight past. Probe the provider
#    path AND the admin path before keeping it.
#
# CODEX CREDENTIAL ISOLATION (the 2026-09 upstream merge, PATCHES §23).
# A post-isolation build refuses, on first use, every stored Codex record that
# has no oauthCredentialOrigin -- which is every record a pre-isolation build
# wrote. Three consequences shape this script:
#
#  * The gate can leave the Codex pool dark while every route check passes. So
#    after the swap the NEW binary's read-only `codex isolation-check --json`
#    runs as the service user, and a failure prints DEPLOY INCOMPLETE with the
#    remediation instead of DEPLOY OK. It never rolls back: the old binary
#    cannot fix this, and restoring it would make it worse (next point).
#  * The rollback boundary (upstream docs/upgrades.md). A pre-isolation binary
#    re-exports a pool chain into $HOME/.codex/auth.json, and every refresh it
#    performs strips the origin the new build requires. That only damages a
#    record a post-isolation build has already MIGRATED. So rollback() refuses
#    to restore a pre-isolation backup over a post-isolation build, and step 1
#    refuses to deploy one, only when this run attested (ATTEST_FIRST) or the
#    service's Codex store holds a record stamped isolated-server-login or
#    server-attested-transfer (or exists and cannot be read) -- and never when
#    ALLOW_PRE_ISOLATION_ROLLBACK=1. Before any record is migrated the old
#    binary serves the store exactly as it did, so a broken first
#    post-isolation build still rolls back on its own.
#  * /_subrouter/ready answers 503 {"startup_ready": false} until the first
#    Codex usage sweep lands (5 s budget, retried every 5 s), and forever while
#    no Codex record can be scored. Step 8 accepts exactly that 503 publicly
#    (it still rolls back on draining or not-served); step 9 waits for the
#    private /ready to flip and only WARNS if it does not.
#
# ATTEST_FIRST=1 -- the first post-isolation deploy on a box whose Codex
# records predate it. Attestation must finish before the new `serve` starts,
# and the old daemon must NOT run during it: its auto-switch would re-export a
# freshly attested chain into auth.json, and a refresh it forces (on a 401)
# erases the origin. So the old daemon is stopped first and nothing serves
# until the new binary starts -- a full box outage of roughly a minute. The
# exact sequence, where AS_SVC runs a command AS THE SERVICE with its own
# HOME, state dir and CODEX_HOME (see as_svc below; as plain root,
# $HOME/.codex would resolve to /root/.codex and the check would read the
# wrong file):
#
#   AS_SVC="sudo -u subrouter env -u CODEX_HOME HOME=/var/lib/subrouter SUBROUTER_STATE_DIR=/var/lib/subrouter"
#   1. verify the sha, back up /usr/local/bin/subrouter      (as the default path)
#   2. stage the candidate as /usr/local/bin/subrouter.stage (root:root 0755)
#   3. $AS_SVC /usr/local/bin/subrouter.stage codex isolation-check --json
#        read-only, the old daemon still serving. ok:true -> nothing to attest,
#        and the deploy continues on the default restart path (no outage).
#        No JSON -> the candidate cannot read the store: abort, nothing changed.
#   4. $AS_SVC /usr/local/bin/subrouter.stage codex attest-legacy --dry-run
#   5. sudo timeout 30 systemctl stop subrouter.service subrouter.socket
#        (sudo kill -9 <old MainPID> and stop again if the drain outlives 30 s).
#        BOTH units: the socket would re-activate the OLD binary on the next
#        connection. If they will not stop, both are started again: abort.
#   6. move the passive interactive copies the old auto-switch exported aside,
#        to <file>.pre-isolation-<stamp>: ${CODEX_HOME:-$HOME/.codex}/auth.json,
#        $HOME/.local/share/opencode/auth.json, $HOME/.pi/agent/auth.json.
#        No human runs Codex as the service user. A record whose refresh token
#        is still in the Codex copy is skipped by attest-legacy and refused by
#        the gate; the OpenCode and pi copies merely die at the rotation.
#   7. $AS_SVC /usr/local/bin/subrouter.stage codex attest-legacy
#        one refresh per record; the provider rotates it. A record reported
#        `failed` was already dead and needs a fresh isolated login -- it does
#        not stop the deploy, and step 10 reports it.
#   8. $AS_SVC /usr/local/bin/subrouter.stage codex isolation-check --json
#   9. mv subrouter.stage subrouter; sudo systemctl start subrouter.socket subrouter.service
#  then steps 5-10 below, exactly as on the default path.
#
# Without ATTEST_FIRST the default path swaps, restarts (bounded) and verifies
# as it always did, then runs steps 9-10. Deploying a post-isolation build that way over unattested records
# ends in DEPLOY INCOMPLETE (exit 3), and the records can then be attested
# against the LIVE new daemon -- it never writes auth.json, its gate never
# refreshes an unattested record, and attest publishes the disk generation,
# so /ready flips on its own. That keeps Claude up; Codex is refused until then.
#
# THE RESTART (PATCHES §38.1). Every restart this script makes -- step 4's and
# rollback()'s -- first drains the running build over its drain socket
# (<state dir>/drain.sock, as root, no token), when that build serves one and
# is not draining already; a build from before the socket is restarted as
# before. It then gives the old process a WALL-CLOCK grace to finish its
# drain, and kill -9s it after that:
#
#   DRAIN_GRACE_SECONDS=<n>   step 4's grace. Default: the drain ceiling, the
#                             shorter of the daemon's --shutdown-timeout (in
#                             SUBROUTER_EXTRA_ARGS, else 10m) and the unit's
#                             TimeoutStopSec, so 600 s on the box: the old
#                             process is gone by then anyway. Clamped to that
#                             ceiling, and never more than 900 s whatever the
#                             config says. 0 forces it as the restart begins.
#   ROLLBACK_DRAIN_GRACE_SECONDS=<n>
#                             rollback()'s grace, for the build that failed its
#                             checks. Default: 60 s, or DRAIN_GRACE_SECONDS's
#                             grace when that is shorter. Clamped the same way.
#   DRAIN_REPORT_SECONDS=<n>  how often the wait reports (default 30, at least 1).
#
# Anything but a whole number in these, or in READY_WAIT_SECS (step 9), stops
# the deploy before anything changes. ATTEST_FIRST's stop keeps its fixed 30 s
# grace (step 3b).
#
# :31415 queues new connections for the new process meanwhile; :31416 refuses
# them. The drain socket answers until the old process exits, so the wait
# reports its turns in flight every DRAIN_REPORT_SECONDS.
set -uo pipefail
# Under pipefail, `producer | grep -q` reports a match as a failure whenever
# grep exits on the match while the producer is still writing: the producer
# dies of SIGPIPE (141) and the pipeline's status is its. So every match in
# this script reads to EOF (`grep PATTERN >/dev/null`), never `grep -q`.

# The detach the header describes. The output goes to the log rather than the
# connection: once the connection is gone, the next write to it kills the
# writer with SIGPIPE, and a restart can wait 600 s for its drain while it
# reports every 30 s.
if [ -z "${DEPLOY_BOX_DETACHED:-}" ]; then
  RUNS="$HOME/.subrouter/transitions"
  command -v python3 >/dev/null 2>&1 ||
    { echo "DEPLOY FAILED: python3 is needed to run the deploy detached. Nothing was changed." >&2; exit 1; }
  mkdir -p "$RUNS" || { echo "DEPLOY FAILED: cannot create $RUNS. Nothing was changed." >&2; exit 1; }
  RUN="$RUNS/deploy-box-$(date +%Y%m%d-%H%M%S)-$$"
  echo "== detached: from here on, losing this connection does not stop the deploy =="
  echo "   log: $RUN.log   exit code: $RUN.rc"
  DEPLOY_BOX_DETACHED=1 DEPLOY_BOX_LOG="$RUN.log" exec python3 - "$RUN" "$BASH" "$0" "$@" <<'PY'
import os, signal, subprocess, sys, time

base, argv = sys.argv[1], sys.argv[2:]
log_path, rc_path = base + ".log", base + ".rc"
log = os.open(log_path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
read_end, write_end = os.pipe()
if os.fork() == 0:
    os.close(read_end)
    os.setsid()
    if os.fork():
        os._exit(0)
    for sig in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, signal.SIG_IGN)
    # Off the connection's descriptors too, or sshd keeps the session open
    # until the run ends.
    null = os.open(os.devnull, os.O_RDONLY)
    os.dup2(null, 0)
    os.dup2(log, 1)
    os.dup2(log, 2)
    with open(base + ".pid", "w") as f:
        f.write("%d\n" % os.getpid())
    os.write(write_end, b"%d" % os.getpid())
    os.close(write_end)
    rc = subprocess.call(argv)
    with open(rc_path + ".next", "w") as f:
        f.write("%d\n" % (rc if rc >= 0 else 128 - rc))
    os.replace(rc_path + ".next", rc_path)
    os._exit(0)
os.close(write_end)
os.close(log)
pid = int(os.read(read_end, 32) or 0)
os.close(read_end)
os.wait()


def alive():
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        pass
    return pid > 0


out = sys.stdout.buffer
with open(log_path, "rb") as log_file:
    while True:
        finished = os.path.exists(rc_path)
        gone = not finished and not alive()
        chunk = log_file.read()
        if chunk:
            out.write(chunk)
            out.flush()
        if finished:
            with open(rc_path) as f:
                sys.exit(int(f.read().strip() or 2))
        if gone and not os.path.exists(rc_path):
            out.write(b"!! the detached run ended without an exit code; read %s\n" % log_path.encode())
            sys.exit(2)
        time.sleep(0.25)
PY
fi

WANT_SHA="$1"
ADMIN_TOKEN="${2:-}"
# Test hook: prefixes the fixed paths this script touches. Empty in real use.
R="${DEPLOY_BOX_TEST_ROOT:-}"
ENV_FILE="$R/etc/default/subrouter"
NEW="${3:-$R/tmp/subrouter.new}"
LOCK="$R/run/lock/subrouter-deploy.lock"
DEST="$R/usr/local/bin/subrouter"
STAGE="$DEST.stage"

# One value from the box's env file, and ONLY from it: under `env -i`, so a key
# the file does not set reads as empty instead of inheriting this shell's value
# (a CODEX_HOME exported here must not become the service's). Read as root: the
# file holds the tokens.
env_val() { sudo env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin sh -c '. "$1" 2>/dev/null; eval "printf %s \"\${$2:-}\""' sh "$ENV_FILE" "$1"; }

# Read from the box's own env file rather than the command line: a token in argv
# is visible in `ps` to every user on the machine for the life of the deploy.
PROXY_TOKEN="$(env_val SUBROUTER_PROXY_TOKEN)"
IMPORT_TOKEN="$(env_val SUBROUTER_ACCOUNT_IMPORT_TOKEN)"
ADMIN_FROM_ENV=0
if [ -z "$ADMIN_TOKEN" ] || [ "$ADMIN_TOKEN" = "-" ]; then
  ADMIN_TOKEN="$(env_val SUBROUTER_ADMIN_TOKEN)"
  ADMIN_FROM_ENV=1
fi
# Overridable, because there can be more than one box. Read from the box's OWN
# config rather than hardcoded, so this script does not have to be edited per
# host -- and cannot silently deploy to, then verify against, two different
# machines. No default: a guessed loopback address on a box bound to its
# tailnet address answers nothing, and the deploy would roll a good build back.
BOX="${SUBROUTER_BOX_ADDR:-$(env_val SUBROUTER_ADDR)}"
# The public listener is probed over LOOPBACK from the box itself. Deliberately
# not its wildcard address: this step is asking "is the route mounted and is the
# credential model right", not "is the firewall open", and routing the check out
# and back would make a firewall change look like a route regression.
#
# Its port is the one the daemon is told to bind: --public-addr in the env
# file's SUBROUTER_EXTRA_ARGS, and never a variable from this shell. An
# override that named the wrong port, or bootstrap.sh's `off`, would skip step
# 8 while a public listener serves. Without --public-addr there, step 8 still
# checks :31416 when something listens on it.
CONFIGURED_PUBLIC_PORT="$(env_val SUBROUTER_EXTRA_ARGS | sed -n 's/.*--public-addr[ =][^ ]*:\([0-9]\{1,\}\).*/\1/p')"
PUBLIC_PORT="${CONFIGURED_PUBLIC_PORT:-31416}"
PUBLIC="127.0.0.1:$PUBLIC_PORT"
STAMP=$(date +%Y%m%d-%H%M%S)
BACKUP="$DEST.bak-$STAMP"

# Who the service runs as, and with what HOME / state dir / CODEX_HOME -- from
# the unit and its env file, so the isolation check inspects the store the
# daemon serves. The unit sets HOME with Environment= and the rest through
# EnvironmentFile=, and systemd lets the file override Environment=, so a key
# is read from the file first. The fallbacks are what install-systemd writes.
unit_env() { systemctl show subrouter.service -p Environment --value 2>/dev/null | tr ' ' '\n' | sed -n "s/^$1=//p" | head -1; }
svc_env() { local v; v="$(env_val "$1")"; [ -n "$v" ] || v="$(unit_env "$1")"; printf %s "$v"; }
SVC_USER="$(systemctl show subrouter.service -p User --value 2>/dev/null)"
SVC_USER="${SVC_USER:-subrouter}"
SVC_HOME="$(svc_env HOME)"
SVC_HOME="${SVC_HOME:-$R/var/lib/subrouter}"
SVC_STATE="$(svc_env SUBROUTER_STATE_DIR)"
SVC_STATE="${SVC_STATE:-$SVC_HOME}"
SVC_CODEX_HOME="$(svc_env CODEX_HOME)"
AS_SVC_TEXT="sudo -u $SVC_USER env -u CODEX_HOME HOME=$SVC_HOME SUBROUTER_STATE_DIR=$SVC_STATE${SVC_CODEX_HOME:+ CODEX_HOME=$SVC_CODEX_HOME}"
# What the old auto-switch exported. The new build never writes any of them.
PASSIVE_COPIES="${SVC_CODEX_HOME:-$SVC_HOME/.codex}/auth.json $SVC_HOME/.local/share/opencode/auth.json $SVC_HOME/.pi/agent/auth.json"

fail() { echo "DEPLOY FAILED: $*" >&2; }

# --- the restart's drain grace and the drain socket (PATCHES §38.1) ------------
# <systemd timespan or Go duration> -> whole seconds, rounded down, and 1 for a
# nonzero span under a second (0 would force the old process as the restart
# begins); fails on anything else, "infinity" included. "10min", "1min 30s",
# "1min 30.500000s", "90s", "10m0s", "1m30.5s", "500ms", "1h" and a bare number
# of seconds all parse.
secs_of() {
  local s="${1// /}" total=0 part=0 n f u
  [ -n "$s" ] || return 1
  while [ -n "$s" ]; do
    n="${s%%[!0-9]*}"
    [ -n "$n" ] && [ "${#n}" -le 9 ] || return 1
    s="${s#"$n"}"
    f=""
    case "$s" in .[0-9]*) f="${s#.}"; f="${f%%[!0-9]*}"; s="${s#."$f"}" ;; esac
    u="${s%%[0-9]*}"
    s="${s#"$u"}"
    case "$f" in *[1-9]*) part=1 ;; esac
    case "$u" in
      h|hr|hour|hours) total=$((total + 10#$n * 3600)) ;;
      m|min|minute|minutes) total=$((total + 10#$n * 60)) ;;
      s|sec|second|seconds|"") total=$((total + 10#$n)) ;;
      ms|msec|us|usec|µs|ns|nsec) [ "$((10#$n))" -eq 0 ] || part=1 ;;
      *) return 1 ;;
    esac
  done
  [ "$total" -eq 0 ] && [ "$part" = 1 ] && total=1
  echo "$total"
}
# <variable name> -> its value as whole seconds in SECS, "" when it is unset.
# Anything but a whole number stops the deploy here, before anything has
# changed. Read as base 10 (08 is 8), and a number too long for shell
# arithmetic reads as 999999999 rather than wrapping (2^64+5 is 5 to bash).
read_secs() {
  local v="${!1:-}"
  SECS=""
  case "$v" in
    "") return 0 ;;
    *[!0-9]*) fail "$1 must be a whole number of seconds, got '$v'. Nothing was changed."; exit 1 ;;
  esac
  if [ "${#v}" -gt 9 ]; then SECS=999999999; else SECS=$((10#$v)); fi
}
# The drain ceiling: the old process is gone by then whatever this script does.
# The daemon exits at its own --shutdown-timeout (10m unless the env file's
# SUBROUTER_EXTRA_ARGS says otherwise), and systemd SIGKILLs it at the unit's
# TimeoutStopSec (install-systemd renders 10min). Config can make that hours,
# or infinity, so the script's own cap, DRAIN_GRACE_CAP, bounds it whatever
# the config says.
DRAIN_GRACE_CAP=900
DRAIN_CEILING=600
DRAIN_CEILING_WHY="the daemon's default --shutdown-timeout, 10m"
SHUTDOWN_ARG="$(env_val SUBROUTER_EXTRA_ARGS | sed -n 's/.*--shutdown-timeout[ =]\([^ ]*\).*/\1/p')"
if [ -n "$SHUTDOWN_ARG" ] && CEILING_S=$(secs_of "$SHUTDOWN_ARG"); then
  DRAIN_CEILING=$CEILING_S
  DRAIN_CEILING_WHY="the daemon's --shutdown-timeout $SHUTDOWN_ARG"
fi
UNIT_STOP="$(systemctl show subrouter.service -p TimeoutStopUSec --value 2>/dev/null)"
if CEILING_S=$(secs_of "$UNIT_STOP") && [ "$CEILING_S" -lt "$DRAIN_CEILING" ]; then
  DRAIN_CEILING=$CEILING_S
  DRAIN_CEILING_WHY="the unit's TimeoutStopSec, $UNIT_STOP"
fi
if [ "$DRAIN_CEILING" -gt "$DRAIN_GRACE_CAP" ]; then
  DRAIN_CEILING=$DRAIN_GRACE_CAP
  DRAIN_CEILING_WHY="this script's hard cap of ${DRAIN_GRACE_CAP}s ($DRAIN_CEILING_WHY is longer)"
fi
read_secs DRAIN_GRACE_SECONDS
if [ -z "$SECS" ]; then
  GRACE=$DRAIN_CEILING
  GRACE_WHY="DRAIN_GRACE_SECONDS unset: the drain ceiling, $DRAIN_CEILING_WHY"
elif [ "$SECS" -gt "$DRAIN_CEILING" ]; then
  GRACE=$DRAIN_CEILING
  GRACE_WHY="DRAIN_GRACE_SECONDS=$DRAIN_GRACE_SECONDS is past the drain ceiling, $DRAIN_CEILING_WHY: the old process is gone by then anyway, so the grace is that ceiling"
else
  GRACE=$SECS
  GRACE_WHY="DRAIN_GRACE_SECONDS=$DRAIN_GRACE_SECONDS"
fi
# rollback()'s grace. It abandons a build that failed its checks, possibly one
# whose upstream calls hang, and every second it drains is a second before the
# old build serves again.
read_secs ROLLBACK_DRAIN_GRACE_SECONDS
if [ -z "$SECS" ]; then
  ROLLBACK_GRACE=60; [ "$GRACE" -lt 60 ] && ROLLBACK_GRACE=$GRACE
  ROLLBACK_GRACE_WHY="ROLLBACK_DRAIN_GRACE_SECONDS unset: 60s, or DRAIN_GRACE_SECONDS's grace when that is shorter"
elif [ "$SECS" -gt "$DRAIN_CEILING" ]; then
  ROLLBACK_GRACE=$DRAIN_CEILING
  ROLLBACK_GRACE_WHY="ROLLBACK_DRAIN_GRACE_SECONDS=$ROLLBACK_DRAIN_GRACE_SECONDS is past the drain ceiling, $DRAIN_CEILING_WHY, so the grace is that ceiling"
else
  ROLLBACK_GRACE=$SECS
  ROLLBACK_GRACE_WHY="ROLLBACK_DRAIN_GRACE_SECONDS=$ROLLBACK_DRAIN_GRACE_SECONDS"
fi
read_secs READY_WAIT_SECS
READY_WAIT=${SECS:-60}
read_secs DRAIN_REPORT_SECONDS
REPORT_EVERY=${SECS:-30}
if [ "$REPORT_EVERY" -lt 1 ]; then
  fail "DRAIN_REPORT_SECONDS must be a whole number of seconds, 1 or more, got '$DRAIN_REPORT_SECONDS'. Nothing was changed."
  exit 1
fi
# The running build's drain socket: SUBROUTER_DRAIN_SOCKET from the service's
# environment, else drain.sock in its state directory. Read as the daemon reads
# it (drainSocketDisabled, drainSocketPath): trimmed, off/0/false/no/none in any
# case mean none, and a relative path means none too, because the daemon
# refuses one -- and handed to curl it would resolve in this shell's directory,
# where another user could have bound it.
DRAIN_SOCK_SET="$(svc_env SUBROUTER_DRAIN_SOCKET)"
DRAIN_SOCK="$(printf %s "$DRAIN_SOCK_SET" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')"
DRAIN_SOCK_NONE=""
case "$(printf %s "$DRAIN_SOCK" | tr '[:upper:]' '[:lower:]')" in
  "") DRAIN_SOCK="$SVC_STATE/drain.sock" ;;
  off|0|false|no|none)
    DRAIN_SOCK_NONE="SUBROUTER_DRAIN_SOCKET=$DRAIN_SOCK_SET in the service's environment turns the drain socket off"
    DRAIN_SOCK="" ;;
  /*) ;;
  *)
    DRAIN_SOCK_NONE="SUBROUTER_DRAIN_SOCKET=$DRAIN_SOCK_SET in the service's environment is not an absolute path, so the daemon serves no drain socket"
    DRAIN_SOCK="" ;;
esac

# MAX-SECONDS METHOD PATH -> the body, over the running build's drain socket,
# as root and with no token, giving up after MAX-SECONDS. Returns 0 only on an
# HTTP 200: nothing listening there (a build from before the socket, or none
# running), a stall and a refusal are all 1.
drain_socket() {
  local out
  [ -n "$DRAIN_SOCK" ] || return 1
  out=$(sudo curl -s -m "$1" --unix-socket "$DRAIN_SOCK" -X "$2" -w '\n%{http_code}' "http://drain-socket$3" 2>/dev/null) || return 1
  printf '%s' "${out%$'\n'*}"
  [ "${out##*$'\n'}" = 200 ]
}
# <key> <drain-status body> -> that key's value (a number or a boolean).
status_field() { printf '%s\n' "$2" | sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\([a-z0-9]*\).*/\1/p" | head -1; }
# <drain-status body> -> what a restart is waiting for.
in_flight() {
  echo "$(status_field active_websocket_turns "$1") websocket turn(s) in flight, $(status_field active_proxy_requests "$1") proxy request(s) open (idle websockets included)"
}

# Run a subrouter binary AS THE SERVICE. From /, because the service user
# cannot necessarily read the directory this script was started in.
as_svc() { # <binary> <args...>
  local extra=()
  [ -n "$SVC_CODEX_HOME" ] && extra=("CODEX_HOME=$SVC_CODEX_HOME")
  (cd / && sudo -u "$SVC_USER" env -u CODEX_HOME -u SUBROUTER_CODEX_SERVER \
    HOME="$SVC_HOME" SUBROUTER_STATE_DIR="$SVC_STATE" ${extra[@]+"${extra[@]}"} "$@")
}

# Every build past the credential-isolation boundary carries the attest-legacy
# verb; none before it does.
is_post_isolation() { sudo env LC_ALL=C grep -aq attest-legacy "$1" 2>/dev/null; }

# <binary> -> ok | migrate:<N> | error:<why>
# The binary's own read-only `codex isolation-check --json`. It exits 1 both
# when records need migration and when it cannot run at all, so the verdict is
# read from the JSON: no JSON means the check did not run, which is never "ok".
isolation_state() {
  local out rc n
  out=$(as_svc "$1" codex isolation-check --json 2>&1); rc=$?
  n=$(printf '%s\n' "$out" | sed -n 's/.*"accounts_requiring_migration"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' | head -1)
  if [ -z "$n" ]; then
    echo "error:$(printf '%s\n' "$out" | tail -1)"
  elif [ "$n" -gt 0 ]; then
    echo "migrate:$n"
  elif [ "$rc" -ne 0 ]; then
    echo "error:exit $rc with 0 accounts requiring migration: $(printf '%s\n' "$out" | tail -1)"
  else
    echo "ok"
  fi
}

# --- the rollback boundary: has any Codex record been migrated? -----------------
# The store the service serves (storepath.CodexDir): <state>/codex/accounts, one
# <account>.json per record; dot-files and directories are not records. A record
# stamped with either origin below was written or attested by a post-isolation
# build, and a pre-isolation binary would damage it: it re-exports the chain and
# strips the origin on its next refresh. A record with no origin (or
# interactive-import) is one the old binary serves exactly as it did before.
CODEX_STORE="$SVC_STATE/codex/accounts"
MIGRATED_ORIGIN_RE='"oauthCredentialOrigin"[[:space:]]*:[[:space:]]*"(isolated-server-login|server-attested-transfer)"'
# POSIX sh, run as root (the store belongs to the service user). $1 = the store,
# $2 = the origin pattern. Prints the migrated-record count, or -- exit 2 -- why
# the store could not be read. Missing counts as 0 only when it provably does not
# exist: a parent that cannot be searched hides the store rather than disproving it.
# shellcheck disable=SC2016 # expanded by the sh that runs it, not here
MIGRATED_SCAN='d=$1; p=$d
if [ ! -e "$d" ]; then
  while [ ! -e "$p" ]; do q=$(dirname "$p"); [ "$q" = "$p" ] && break; p=$q; done
  if [ -d "$p" ] && [ -x "$p" ]; then echo 0; exit 0; fi
  echo "cannot tell whether $d exists: $p cannot be searched"; exit 2
fi
if [ ! -d "$d" ] || [ ! -r "$d" ] || [ ! -x "$d" ]; then echo "$d exists but cannot be read"; exit 2; fi
n=0
for f in "$d"/*.json; do
  [ -e "$f" ] || [ -L "$f" ] || continue
  [ -d "$f" ] && continue
  grep -Eq -- "$2" "$f" 2>/dev/null; rc=$?
  case $rc in 0) n=$((n + 1)) ;; 1) ;; *) echo "$f cannot be read"; exit 2 ;; esac
done
echo "$n"'
# Set the moment ATTEST_FIRST has executed attest-legacy, whatever it reported:
# from then on records may carry an origin this script did not see written.
ATTEST_RAN=0

# Why restoring a pre-isolation binary would damage the Codex store. Prints the
# reason and returns 0 when it would; prints why not and returns 1 when it
# would not. An unreadable store is a reason: nothing proves it holds no
# migrated record.
migration_hazard() {
  local out rc
  if [ "$ATTEST_RAN" = 1 ]; then
    echo "codex attest-legacy ran in this deploy (ATTEST_FIRST), so the store holds attested records"
    return 0
  fi
  # stderr dropped: sudo's own warnings (an unresolvable hostname) must not turn
  # a clean count into "unreadable". The scan prints its failures on stdout.
  out=$(sudo env LC_ALL=C sh -c "$MIGRATED_SCAN" sh "$CODEX_STORE" "$MIGRATED_ORIGIN_RE" 2>/dev/null); rc=$?
  case "$rc:$out" in
    0:0) echo "no Codex record in $CODEX_STORE carries a migrated origin"; return 1 ;;
    0:|0:*[!0-9]*) echo "the scan of $CODEX_STORE printed no count (${out:-nothing}), so nothing proves no record was migrated" ;;
    0:*) echo "$out Codex record(s) in $CODEX_STORE carry oauthCredentialOrigin isolated-server-login or server-attested-transfer" ;;
    *)   echo "the Codex store could not be read (${out:-the scan exited $rc}), so nothing proves no record was migrated" ;;
  esac
  return 0
}

# <addr> -> 0 when it is host:port, the host an address or name (IPv6 in
# brackets) and the port digits. Anything else would be pasted into a URL.
host_port_ok() {
  local host="${1%:*}" port="${1##*:}"
  [ "$host" != "$1" ] && [ -n "$host" ] && [ -n "$port" ] || return 1
  case "$port" in *[!0-9]*) return 1 ;; esac
  case "$host" in *[!]A-Za-z0-9.:[-]*) return 1 ;; esac
  # An unbracketed IPv6 host, as in 2001:db8::1:31415, makes a URL curl cannot
  # parse (exit 3), and step 6 would roll a good build back.
  case "$host" in
    \[*:*\]) ;;
    *[][:]*) return 1 ;;
  esac
}

# GET /_subrouter/accounts -> how many accounts the running build lists.
account_count() {
  curl -s -m 20 -H "Authorization: Bearer $ADMIN_TOKEN" "http://$BOX/_subrouter/accounts" \
    | grep -o '"provider"' | wc -l | tr -d ' '
}

# Both a deploy and bootstrap.sh replace $DEST and restart the service. A
# second deploy beside a first would back up the first's build under test as
# the one to restore, and share $STAGE with it. A dropped connection does not
# stop a deploy, so a retry is the likely second run. Only root writes the
# file: bootstrap.sh runs as root, this script as the ssh user, and a file one
# of them owns in /run/lock is one the other may not open to write
# (fs.protected_regular). Reading is enough for flock. The kernel drops the
# lock when this run exits, however it ends.
if ! command -v flock >/dev/null 2>&1; then
  fail "flock (util-linux) is needed to keep two deploys apart. Nothing was changed."
  exit 1
fi
if ! sudo sh -c ': >>"$1" && chmod 0644 "$1"' sh "$LOCK" || ! { exec 9<"$LOCK"; } 2>/dev/null; then
  fail "cannot open the deploy lock $LOCK. Nothing was changed."
  exit 1
fi
if ! flock -n 9; then
  fail "another deploy is running ($(cat "$LOCK" 2>/dev/null)). Follow its log, and do not start another until it ends. Nothing was changed."
  exit 1
fi
printf 'deploy-box.sh, pid %s, log %s\n' "$$" "${DEPLOY_BOX_LOG:-none, in the foreground}" | sudo tee "$LOCK" >/dev/null

echo "== 1. verify the payload =="
if [ -z "$ADMIN_TOKEN" ]; then
  fail "no admin token: $ENV_FILE sets no SUBROUTER_ADMIN_TOKEN for \"-\", and step 6 cannot check the admin path without one. Nothing was changed."
  exit 1
fi
[ "$ADMIN_FROM_ENV" = 1 ] && echo "   admin token: from $ENV_FILE (SUBROUTER_ADMIN_TOKEN)"
if [ -z "$BOX" ]; then
  fail "no box address: SUBROUTER_BOX_ADDR is unset and $ENV_FILE sets no SUBROUTER_ADDR, so the checks after the restart have no private listener to probe. Set SUBROUTER_BOX_ADDR=<host:port>. Nothing was changed."
  exit 1
fi
if ! host_port_ok "$BOX"; then
  fail "the box address '$BOX' (SUBROUTER_BOX_ADDR, else SUBROUTER_ADDR in $ENV_FILE) is not host:port. Nothing was changed."
  exit 1
fi
echo "   private listener: $BOX"
GOT_SHA=$(sha256sum "$NEW" | awk '{print $1}')
if [ "$GOT_SHA" != "$WANT_SHA" ]; then
  fail "checksum mismatch across the wire (got ${GOT_SHA:0:16}, want ${WANT_SHA:0:16})"
  exit 1
fi
echo "   sha ok: ${GOT_SHA:0:16}"
# --public-admin in the unit's arguments (PATCHES §43.1) is refused, in any
# form. rollback() below restores the previous binary under the SAME
# arguments, and a build that predates the flag exits on it ("flag provided
# but not defined") and systemd restarts it forever: the first deploy of a
# §43.1 build that then fails a check would take the box down. The variable
# does the same thing, and an older build ignores a variable it does not know.
if printf ' %s ' "$(env_val SUBROUTER_EXTRA_ARGS)" | grep -E '[[:space:]]--?public-admin(=[^[:space:]]*)?[[:space:]]' >/dev/null; then
  fail "SUBROUTER_EXTRA_ARGS in $ENV_FILE carries --public-admin. Remove it and set SUBROUTER_PUBLIC_ADMIN=1 (or 0) in $ENV_FILE instead: a rollback to a build that predates the flag would exit on it and restart forever. Nothing was changed."
  exit 1
fi
# Deploying a pre-isolation build over a post-isolation one IS a rollback past
# the boundary, whatever it is called. Refuse it here, before anything changes --
# once a record has been migrated.
if is_post_isolation "$DEST" && ! is_post_isolation "$NEW" && [ "${ALLOW_PRE_ISOLATION_ROLLBACK:-0}" != 1 ]; then
  if HAZARD=$(migration_hazard); then
    fail "refusing to replace a post-isolation build with a pre-isolation one: $HAZARD. It would re-export a pool chain into ${SVC_CODEX_HOME:-$SVC_HOME/.codex}/auth.json and strip oauthCredentialOrigin from every Codex record it refreshes (upstream docs/upgrades.md rollback boundary). Nothing was changed. Set ALLOW_PRE_ISOLATION_ROLLBACK=1 to do it anyway."
    exit 1
  fi
  echo "   replacing a post-isolation build with a pre-isolation one: allowed, $HAZARD"
fi

echo "== 2. count the pool, then back up the running binary =="
# Step 7's floor: the accounts the running build lists now. A pool of any size
# passes as long as the new build lists no fewer, an empty one included. 0
# when the running build does not answer, which sets no floor.
BASE_COUNT=$(account_count)
echo "   accounts visible before the swap: ${BASE_COUNT:-0}"
sudo cp "$DEST" "$BACKUP" || { fail "backup failed"; exit 1; }
echo "   backup: $BACKUP"

# ATTEST_FIRST steps 3-8 of the header. Returns with the old daemon STOPPED
# (SERVICE_STOPPED=1) and every record attested that can be, or -- when the
# preflight finds nothing to attest -- with the old daemon untouched.
SERVICE_STOPPED=0
attest_first() {
  local state out rc f
  echo "== 3a. ATTEST_FIRST: preflight with the candidate (read-only; the old daemon still serves) =="
  if ! is_post_isolation "$STAGE"; then
    sudo rm -f "$STAGE"
    fail "ATTEST_FIRST needs a post-isolation build, and this one has no attest-legacy verb. Nothing was stopped or changed."
    exit 1
  fi
  state=$(isolation_state "$STAGE")
  case "$state" in
    ok)
      echo "   isolation-check: ok -- nothing to attest; continuing on the default restart path"
      return 0 ;;
    error:*)
      sudo rm -f "$STAGE"
      fail "the candidate's isolation-check did not run (${state#error:}). Nothing was stopped or changed."
      exit 1 ;;
  esac
  echo "   isolation-check: ${state#migrate:} account(s) require migration"
  as_svc "$STAGE" codex attest-legacy --dry-run 2>&1 | sed 's/^/   /'

  echo "== 3b. stop the old daemon AND its socket (nothing serves until step 4) =="
  # A fixed 30 s grace, then kill -9: nothing serves until step 4 either way, so
  # a longer wait only lengthens the outage. The old binary here predates §38
  # and exits at once.
  [ -n "${DRAIN_GRACE_SECONDS:-}" ] &&
    echo "   DRAIN_GRACE_SECONDS=$DRAIN_GRACE_SECONDS does not apply to this stop: ATTEST_FIRST gives the old daemon a fixed 30 s, then forces it"
  local old_pid; old_pid=$(systemctl show subrouter.service -p MainPID --value)
  if ! sudo timeout 30 systemctl stop subrouter.service subrouter.socket; then
    if [ -n "$old_pid" ] && [ "$old_pid" != "0" ]; then
      echo "   old pid $old_pid still draining after 30s -- forcing it"
      sudo kill -9 "$old_pid" 2>/dev/null
    fi
    sudo systemctl stop subrouter.service subrouter.socket
  fi
  for f in subrouter.service subrouter.socket; do
    case "$(systemctl is-active "$f")" in
      inactive|failed) ;;
      *)
        sudo systemctl start subrouter.socket subrouter.service
        sudo rm -f "$STAGE"
        fail "$f would not stop, so the old daemon could race the attestation. It was started again on the OLD binary; nothing was attested."
        exit 1 ;;
    esac
  done
  SERVICE_STOPPED=1
  echo "   service and socket: stopped"

  echo "== 3c. move the passive auth copies aside =="
  for f in $PASSIVE_COPIES; do
    if sudo test -e "$f"; then
      sudo mv "$f" "$f.pre-isolation-$STAMP" && echo "   moved $f -> $f.pre-isolation-$STAMP"
    fi
  done

  echo "== 3d. attest with the NEW binary (the old one is not running) =="
  out=$(as_svc "$STAGE" codex attest-legacy 2>&1); rc=$?
  ATTEST_RAN=1
  printf '%s\n' "$out" | sed 's/^/   /'
  [ "$rc" -eq 0 ] || echo "   attest-legacy exited $rc: those records need a fresh isolated login; step 10 reports what is left"
  echo "   isolation-check after attest: $(isolation_state "$STAGE")"
}

echo "== 3. install (stage beside, then rename) =="
sudo cp "$NEW" "$STAGE" || { fail "stage failed"; exit 1; }
sudo chown root:root "$STAGE"
sudo chmod 755 "$STAGE"
[ "${ATTEST_FIRST:-0}" = 1 ] && attest_first
if ! sudo mv "$STAGE" "$DEST"; then
  fail "rename failed"
  [ "$SERVICE_STOPPED" = 1 ] &&
    echo "   subrouter.service and .socket are STOPPED and the store is attested. Do NOT start the old binary: move $STAGE to $DEST by hand, then sudo systemctl start subrouter.socket subrouter.service" >&2
  exit 1
fi
echo "   installed: $(sudo sha256sum "$DEST" | cut -c1-16)"

# `systemctl restart` BLOCKS until the stop completes, and the unit sets
# TimeoutStopSec=10min against a daemon that drains in-flight turns gracefully
# (PATCHES §38: the streams on BOTH listeners and the Codex websocket turns). So
# a naive restart can sit for minutes with nothing serving. :31415 is socket
# activated (systemd holds the listener, so clients queue rather than get
# refused) but :31416 is bound by the process itself and is simply gone for the
# whole drain. The wait is bounded by the WALL CLOCK: the old process gets its
# grace, then its main PID is forced (PATCHES §38.1). That trades the turns the
# force cuts against the time :31416 stays closed. By default step 4 takes the
# side of the turns (PATCHES §38.1): the drain ceiling, 600 s on the box, where
# turns are never cut before the daemon or systemd would end them anyway; a
# shorter DRAIN_GRACE_SECONDS takes the other side. This used to be 25 health probes,
# 25-75 s depending on how long each curl hung on the queued socket.
#
# First, the running build is drained over its drain socket, when it serves one
# and is not draining already. In the moment before SIGTERM, a new session then
# gets a 503 instead of being admitted by a process about to wait on it; a
# drain keeps serving the sessions the daemon already knows, so this does not
# empty the process and the restart follows at once. The socket also reports
# what the restart then waits for. A build from before the socket is restarted
# without it, as before. A drain found in place (an operator's) is left alone,
# and never lifted by this script.
#
# Every restart this script makes goes through here, step 4's and rollback()'s
# alike: a rollback abandons a build that may be draining the very turns it is
# being rolled back for, and a bare restart there waited out the whole 10
# minutes. So rollback() passes its own, shorter grace.
restart_bounded() { # grace-seconds why variable-name
  local grace="$1" why="$2" var="$3" old_pid job started deadline body pre_drained=0 next_report left m
  old_pid=$(systemctl show subrouter.service -p MainPID --value)
  if [ -z "$DRAIN_SOCK" ]; then
    echo "   no pre-drain: ${DRAIN_SOCK_NONE:-no drain socket}"
  elif ! body=$(drain_socket 5 GET /_subrouter/drain-status); then
    echo "   no drain socket answered at $DRAIN_SOCK: the running build predates it (or serves none); restarting without a pre-drain"
  elif [ "$(status_field draining "$body")" = true ]; then
    echo "   the running build was already draining, and not by this script (an operator's drain?): left as it is, and not lifted if the restart fails; $(in_flight "$body")"
  elif body=$(drain_socket 5 POST /_subrouter/drain); then
    pre_drained=1
    echo "   drained the running build over $DRAIN_SOCK (no token): new sessions get 503; $(in_flight "$body")"
  else
    echo "   the drain socket at $DRAIN_SOCK answered but did not drain; restarting without a pre-drain"
  fi
  echo "   the old process may drain for ${grace}s before it is forced ($why)"
  started=$SECONDS
  deadline=$((started + grace))
  next_report=$((started + REPORT_EVERY))
  sudo systemctl restart subrouter.service &
  job=$!
  while kill -0 "$job" 2>/dev/null; do
    if [ "$SECONDS" -ge "$deadline" ]; then
      if [ -n "$old_pid" ] && [ "$old_pid" != "0" ] && sudo kill -0 "$old_pid" 2>/dev/null; then
        # At most a second on a stalled socket: the deadline has passed.
        if body=$(drain_socket 1 GET /_subrouter/drain-status); then body=" with $(in_flight "$body")"; else body=""; fi
        echo "   old pid $old_pid still draining after ${grace}s ($var)$body -- forcing it"
        sudo kill -9 "$old_pid" 2>/dev/null
      fi
      break
    fi
    if [ "$SECONDS" -ge "$next_report" ]; then
      next_report=$((SECONDS + REPORT_EVERY))
      # A report never holds the force past the deadline: its curl waits at
      # most the time left (and at most 5 s).
      left=$((deadline - SECONDS)); m=5; [ "$left" -lt 5 ] && m=$left; [ "$m" -ge 1 ] || m=1
      if body=$(drain_socket "$m" GET /_subrouter/drain-status); then
        echo "   old pid $old_pid draining, $((SECONDS - started))s of ${grace}s: $(in_flight "$body")"
      else
        echo "   old pid $old_pid draining, $((SECONDS - started))s of ${grace}s"
      fi
    fi
    sleep 1
  done
  wait "$job" 2>/dev/null
  # A process starts undrained. So a drain socket that still answers draining,
  # from a process not shutting down, after this script drained it, is that
  # process, still running: the restart did not replace it. Left alone it would
  # refuse every new session until the next restart. A drain this script did
  # not make is never lifted.
  if [ "$pre_drained" = 1 ] && body=$(drain_socket 5 GET /_subrouter/drain-status) &&
    [ "$(status_field draining "$body")" = true ] && [ "$(status_field shutting_down "$body")" != true ]; then
    if drain_socket 5 POST /_subrouter/resume >/dev/null; then
      echo "   WARNING: the restart did not replace the drained process; resumed it over $DRAIN_SOCK"
    else
      echo "   WARNING: the restart did not replace the drained process, and resuming it failed: run sudo subrouter resume"
    fi
  fi
}

echo "== 4. restart =="
# Step 8 reads what the new daemon logged from here on.
RESTARTED_AT=$(( $(date +%s) - 1 ))
if [ "$SERVICE_STOPPED" = 1 ]; then
  # ATTEST_FIRST stopped both units. Start the socket too: it holds :31415.
  sudo systemctl start subrouter.socket subrouter.service
  for _ in $(seq 1 25); do
    sleep 1
    curl -s -m 2 "http://$BOX/_subrouter/health" 2>/dev/null | grep '"ok"' >/dev/null && break
  done
else
  restart_bounded "$GRACE" "$GRACE_WHY" DRAIN_GRACE_SECONDS
fi
sleep 5

rollback() {
  # The credential-origin rollback boundary. Checked before anything moves, so
  # a refusal leaves the new build exactly as installed. Crossing it is refused
  # only once a record has been migrated (migration_hazard): until then the old
  # binary serves the store as it did an hour ago.
  if is_post_isolation "$DEST" && ! is_post_isolation "$BACKUP" && [ "${ALLOW_PRE_ISOLATION_ROLLBACK:-0}" != 1 ]; then
    local why
    if why=$(migration_hazard); then
      echo "!! NOT ROLLING BACK: $1" >&2
      echo "   $BACKUP predates Codex credential isolation, and $why." >&2
      echo "   Restoring it would cross the rollback boundary (upstream docs/upgrades.md): its" >&2
      echo "   auto-switch re-exports a pool chain into ${SVC_CODEX_HOME:-$SVC_HOME/.codex}/auth.json, and every" >&2
      echo "   refresh it makes strips the oauthCredentialOrigin this build requires. The new build stays installed;" >&2
      echo "   service=$(systemctl is-active subrouter.service). Fix forward:" >&2
      echo "     sudo journalctl -u subrouter -n 50 --no-pager" >&2
      echo "     $AS_SVC_TEXT $DEST codex isolation-check --json" >&2
      echo "   To restore it anyway (only if the outage is worse than re-migrating those records after):" >&2
      echo "     sudo cp $BACKUP $NEW && sudo chown \$(id -un) $NEW && ALLOW_PRE_ISOLATION_ROLLBACK=1 bash $0 \"\$(sha256sum $NEW | cut -d' ' -f1)\" - $NEW" >&2
      fail "$1 (not rolled back)"
      exit 1
    fi
    echo "   $BACKUP predates Codex credential isolation, but $why; restoring it is safe." >&2
  fi
  echo "!! ROLLING BACK: $1" >&2
  sudo cp "$BACKUP" "$STAGE"
  sudo chown root:root "$STAGE"
  sudo chmod 755 "$STAGE"
  sudo mv "$STAGE" "$DEST"
  restart_bounded "$ROLLBACK_GRACE" "$ROLLBACK_GRACE_WHY" ROLLBACK_DRAIN_GRACE_SECONDS >&2
  sleep 5
  echo "   rolled back to $(sudo sha256sum "$DEST" | cut -c1-16), service=$(systemctl is-active subrouter.service)" >&2
  exit 1
}

echo "== 5. is it even running =="
if [ "$(systemctl is-active subrouter.service)" != "active" ]; then
  echo "--- last 15 journal lines ---" >&2
  sudo journalctl -u subrouter -n 15 --no-pager >&2
  rollback "service is not active after restart"
fi
echo "   service: active"

echo "== 6. BOTH surfaces on the private listener =="
# The provider path. 401 is a pass: it proves the route is mounted and reached
# the auth gate. A 404 means the route stopped existing, which is exactly what a
# route-table refactor breaks.
#
# KNOWN GAP, deliberately not fixed here: 000 is folded in with 404 below, and
# those are different facts. 000 is also what a TIMEOUT produces, and this probe
# is not guaranteed to stop at the auth gate -- where the caller is exempt from
# the token, it becomes a real upstream request that walks the retry chain and
# can outlive -m 10 whenever the pool is rate-limited. On the laptop that rolled
# back a binary later proved route-identical to the one it replaced (2026-09-16);
# deploy-laptop.sh now classifies via curl's exit code (28 = timed out, so the
# route IS mounted) and compares against a pre-swap baseline.
#
# The same treatment belongs here and is not yet ported. If this line rolls
# back a build, check whether the pool is spent before believing the route
# table broke.
PROVIDER=$(curl -s -m 10 -o /dev/null -w '%{http_code}' -X POST "http://$BOX/v1/messages" -d '{}')
echo "   POST /v1/messages       -> $PROVIDER"
case "$PROVIDER" in
  000|404) rollback "provider path is not served (got $PROVIDER); the route table is broken" ;;
esac

ADMIN=$(curl -s -m 10 -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $ADMIN_TOKEN" "http://$BOX/_subrouter/accounts")
echo "   GET  /_subrouter/accounts -> $ADMIN"
if [ "$ADMIN" != "200" ]; then
  rollback "admin surface is not served on the PRIVATE listener (got $ADMIN)"
fi

echo "== 7. the pool still answers with every account it had =="
COUNT=$(account_count)
echo "   accounts visible: ${COUNT:-0}   (before the swap: ${BASE_COUNT:-0})"
if [ "${COUNT:-0}" -lt "${BASE_COUNT:-0}" ]; then
  rollback "only ${COUNT:-0} accounts visible, ${BASE_COUNT:-0} before the swap; the pool did not load"
fi

echo "== 8. the PUBLIC surface serves stats and nothing else =="
# -k, not --cacert: the pin is verified by a separate probe from the peer side.
# What this step asserts is the ROUTE and CREDENTIAL contract, which is the part
# a route-table refactor breaks silently. Every check rolls back, because each
# failure here is a security regression rather than a degraded feature.
# Match the local-address FIELD ending in the port, rather than enumerating how
# the address might be spelled. A dual-stack wildcard renders as `*:31416`, not
# `0.0.0.0:31416`, so an address-spelling allowlist skipped every assertion below
# and still printed DEPLOY OK -- a skip branch that reports success is worse than
# no check, because it reads as one that ran.
if ! sudo ss -ltn | awk '{print $4}' | grep ":$PUBLIC_PORT\$" >/dev/null; then
  [ -n "$CONFIGURED_PUBLIC_PORT" ] &&
    rollback "nothing listens on :$PUBLIC_PORT, where SUBROUTER_EXTRA_ARGS in $ENV_FILE puts the public listener (--public-addr); peers cannot reach the box. For a box with no public listener, re-run bootstrap.sh with SUBROUTER_PUBLIC_PORT=off"
  echo "   no public listener configured; skipping (private-only deployment)"
else
  pub() { curl -sk -m 10 -o /dev/null -w '%{http_code}' "$@"; }

  SPEND=$(pub -H "Authorization: Bearer $PROXY_TOKEN" "https://$PUBLIC/_subrouter/usage-status")
  echo "   usage-status + PROXY token -> $SPEND   (want 200: the scheduler reads peer capacity here)"
  [ "$SPEND" = "200" ] || rollback "public stats unreadable with the proxy token (got $SPEND); a publicly-reached peer is invisible to the scheduler"

  ADMIN_PUB=$(pub -H "Authorization: Bearer $ADMIN_TOKEN" "https://$PUBLIC/_subrouter/usage-status")
  echo "   usage-status + ADMIN token -> $ADMIN_PUB   (want 401: admin is not a credential out here)"
  [ "$ADMIN_PUB" = "401" ] || rollback "the ADMIN token was accepted on the public listener (got $ADMIN_PUB); the more powerful secret must never be usable across the public wire"

  NO_TOKEN=$(pub "https://$PUBLIC/_subrouter/usage-status")
  echo "   usage-status, no token     -> $NO_TOKEN   (want 401)"
  [ "$NO_TOKEN" = "401" ] || rollback "public stats served without any token (got $NO_TOKEN)"

  # Liveness IS public, and unauthenticated on purpose: a peer has to be able to
  # tell "unreachable" from "up but refusing me" without holding a credential.
  # Asserted positively -- the private-path loop below would otherwise be the
  # only statement about /_subrouter/*, and a mux that served NOTHING would pass
  # it.
  CODE=$(pub "https://$PUBLIC/_subrouter/health")
  echo "   /_subrouter/health (no token) -> $CODE   (want 200: peers probe liveness unauthenticated)"
  [ "$CODE" = "200" ] || rollback "/_subrouter/health is not served publicly (got $CODE); peers cannot distinguish a down box from a refusing one"

  # /ready is liveness too, but a post-isolation build answers 503 with
  # {"startup_ready": false} until its first Codex usage sweep lands -- and
  # forever while no Codex record can be scored. That 503 proves the route is
  # mounted and the process is not draining, which is all this step asks; step
  # 9 waits for the flip. Any other 503 (a DRAINING body carries no
  # startup_ready key) and any other code still roll back. The body is
  # indented JSON, so the match tolerates whitespace around the colon.
  READY=$(curl -sk -m 10 -w '\n%{http_code}' "https://$PUBLIC/_subrouter/ready")
  CODE=${READY##*$'\n'}
  READY_BODY=${READY%$'\n'*}
  case "$CODE" in
    200)
      echo "   /_subrouter/ready (no token) -> 200   (want 200, or 503 startup_ready:false)" ;;
    503)
      if printf '%s' "$READY_BODY" | grep -E '"startup_ready"[[:space:]]*:[[:space:]]*false' >/dev/null &&
        ! printf '%s' "$READY_BODY" | grep -E '"draining"[[:space:]]*:[[:space:]]*true' >/dev/null; then
        echo "   /_subrouter/ready (no token) -> 503 startup_ready:false   (mounted; startup scores pending, see step 9)"
      else
        rollback "/_subrouter/ready answered 503 without startup_ready:false (draining?): $(printf '%s' "$READY_BODY" | tr -s ' \n' ' ')"
      fi ;;
    *)
      rollback "/_subrouter/ready is not served publicly (got $CODE); peers cannot distinguish a down box from a refusing one" ;;
  esac

  # The private-only routes. Probed WITH the admin token, so a pass cannot be
  # explained away as "it 404d because the caller was unauthorized".
  #
  # --public-admin (PATCHES §43.1) mounts some admin routes here for NAMED
  # admin-role access tokens, which this script does not hold. Whether it is
  # on is asked of the daemon itself: serve logs "PUBLIC ADMIN IS ON" at the
  # top of every start when it is, so the journal of the MainPID that started
  # in step 4 says what this build did with the setting, wherever it was set
  # (the env file, a systemd drop-in). The env file and the unit's
  # Environment= only say what was ASKED for.
  #   on (the daemon said so): the marked routes probed below must refuse the
  #     static admin and import tokens with 401; a 404 is a warning (not
  #     mounted), anything else rolls back. transcripts is not marked (PATCHES
  #     §43.2): it must be 404 here whatever the setting says. claude-reset
  #     IS marked since PATCHES §46.9: 401 here with the flag
  #     on, 404 with it off. Probed by GET, which no gate lets spend anything
  #     (the handler takes POST only).
  #   asked for, not confirmed (a build that predates the setting, or an
  #     unreadable journal): 401 or 404 there, with a warning.
  #   off: every probed route must be 404, as before.
  MAIN_PID=$(systemctl show subrouter.service -p MainPID --value 2>/dev/null)
  PUBLIC_ADMIN=off
  if [ -n "$MAIN_PID" ] && [ "$MAIN_PID" != 0 ] &&
    sudo journalctl "_PID=$MAIN_PID" --since "@$RESTARTED_AT" -o cat --no-pager 2>/dev/null | grep 'PUBLIC ADMIN IS ON' >/dev/null; then
    PUBLIC_ADMIN=on
  else
    case "$(svc_env SUBROUTER_PUBLIC_ADMIN | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')" in
      1|true|yes|on) PUBLIC_ADMIN=asked ;;
    esac
  fi
  case "$PUBLIC_ADMIN" in
    on) echo "   public admin: ON (pid $MAIN_PID logged PUBLIC ADMIN IS ON): named admin tokens are admin on this listener; the static admin and import tokens must still be refused" ;;
    asked) echo "   WARNING: SUBROUTER_PUBLIC_ADMIN asks for public admin, but pid ${MAIN_PID:-?} did not log PUBLIC ADMIN IS ON since the restart:"
      echo "   a build that predates it ignores it, or the journal could not be read. The marked routes may answer 401 or 404." ;;
  esac
  # Session leases are served under /internal/v1/, not /_subrouter/.
  for path in /_subrouter/sessions /_subrouter/pool.json /_subrouter/drain-status /_subrouter/transcripts \
    /internal/v1/session-leases /_subrouter/dashboard /_subrouter/stream-stats /_subrouter/account-status \
    /_subrouter/reload-accounts /_subrouter/account-import /_subrouter/claude-reset; do
    CODE=$(pub -H "Authorization: Bearer $ADMIN_TOKEN" "https://$PUBLIC$path")
    echo "   $path -> $CODE"
    case "$PUBLIC_ADMIN:${path##*/}" in
      on:sessions|on:drain-status|on:account-status|on:account-import|on:claude-reset|asked:sessions|asked:drain-status|asked:account-status|asked:account-import|asked:claude-reset)
        case "$PUBLIC_ADMIN:$CODE" in
          *:401|asked:404) ;;
          on:404) echo "   WARNING: $path is not mounted on the public listener, although the daemon says public admin is on" ;;
          *) rollback "$path answered $CODE to the STATIC admin token on the public listener; with --public-admin only a named admin token may open it" ;;
        esac ;;
      *)
        if [ "$CODE" != 404 ]; then
          HINT=""
          [ "$CODE" = 401 ] && HINT=" (if SUBROUTER_PUBLIC_ADMIN is set where this script does not look, the daemon did not say so either: check the journal for PUBLIC ADMIN IS ON)"
          rollback "$path is reachable on the public listener (got $CODE); it must not be mounted there$HINT"
        fi ;;
    esac
  done
  # The Claude reset claim with NO credential at all: with public admin on it
  # is mounted and the gate must answer 401 (PATCHES §46.9); off, 404.
  CODE=$(pub "https://$PUBLIC/_subrouter/claude-reset")
  echo "   /_subrouter/claude-reset (no token) -> $CODE"
  case "$PUBLIC_ADMIN:$CODE" in
    on:401|asked:401|asked:404|off:404) ;;
    on:404) echo "   WARNING: /_subrouter/claude-reset is not mounted on the public listener, although the daemon says public admin is on (a build before PATCHES §46.9?)" ;;
    *) rollback "/_subrouter/claude-reset answered $CODE with no token on the public listener (public admin: $PUBLIC_ADMIN); want 401 with it on, 404 with it off" ;;
  esac
  # The import gate is a second gate (requireAccountImportAuth), and the static
  # import token must open nothing out here either. A GET that gets past it
  # answers 405, with no side effect.
  if [ "$PUBLIC_ADMIN" != off ] && [ -n "$IMPORT_TOKEN" ]; then
    CODE=$(pub -H "X-Subrouter-Account-Import-Token: $IMPORT_TOKEN" "https://$PUBLIC/_subrouter/account-import")
    echo "   /_subrouter/account-import + IMPORT token -> $CODE   (want 401)"
    case "$PUBLIC_ADMIN:$CODE" in
      *:401|asked:404) ;;
      on:404) echo "   WARNING: /_subrouter/account-import is not mounted on the public listener, although the daemon says public admin is on" ;;
      *) rollback "/_subrouter/account-import answered $CODE to the static IMPORT token on the public listener; it must never be a credential there" ;;
    esac
  fi

  # And the whole point of the listener: provider traffic still flows.
  PUB_PROVIDER=$(pub -X POST "https://$PUBLIC/v1/messages" -d '{}')
  echo "   POST /v1/messages (public) -> $PUB_PROVIDER   (want NOT 404)"
  case "$PUB_PROVIDER" in
    000|404) rollback "the public listener does not serve provider traffic (got $PUB_PROVIDER); it has no purpose" ;;
  esac
fi

# Nothing from here on rolls back: every route check has passed, and what is
# left is the Codex credential state, which the old binary cannot fix. Step 9
# waits READY_WAIT_SECS (default 60) for the private /ready to flip.
echo "== 9. wait for readiness (warning only) =="
READY_OK=0
for _ in $(seq 1 "$READY_WAIT"); do
  if curl -s -m 2 "http://$BOX/_subrouter/ready" 2>/dev/null | grep -E '"ok"[[:space:]]*:[[:space:]]*true' >/dev/null; then
    READY_OK=1
    break
  fi
  sleep 1
done
if [ "$READY_OK" = 1 ]; then
  echo "   /_subrouter/ready: ok"
else
  echo "   WARNING: /_subrouter/ready is still not ok after ${READY_WAIT}s. No Codex usage sweep has"
  echo "   scored yet; while every Codex record fails the isolation gate it never will. Step 10 says"
  echo "   whether that is why. Check: $AS_SVC_TEXT $DEST codex isolation-check"
fi

echo "== 10. Codex credential isolation (the new binary, as $SVC_USER) =="
ISOLATION=ok
if is_post_isolation "$DEST"; then
  ISOLATION=$(isolation_state "$DEST")
  echo "   isolation-check: $ISOLATION"
else
  echo "   n/a: this build predates the isolation gate"
fi

rm -f "$NEW"
echo
if [ "$ISOLATION" != ok ]; then
  case "$ISOLATION" in
    migrate:*) WHY="${ISOLATION#migrate:} Codex account(s) fail the credential-isolation gate and are REFUSED" ;;
    *)         WHY="the isolation check could not run (${ISOLATION#error:}), so nothing proves the Codex pool is served" ;;
  esac
  echo "DEPLOY INCOMPLETE  sha=${GOT_SHA:0:16}  backup=$BACKUP"
  echo "  The new build is serving and is kept, but $WHY."
  echo "  Not rolled back: the old binary cannot fix this, and $BACKUP may be pre-isolation."
  echo "  Fix forward against the live daemon (it never writes auth.json):"
  echo "    1. move aside any passive copy the old auto-switch exported:"
  echo "         for f in $PASSIVE_COPIES; do"
  echo "           sudo test -e \"\$f\" && sudo mv \"\$f\" \"\$f.pre-isolation-$STAMP\"; done"
  echo "    2. $AS_SVC_TEXT $DEST codex attest-legacy"
  echo "    3. a record it skips as 'shares the interactive Codex login' needs an isolated re-login:"
  echo "         $AS_SVC_TEXT $DEST codex migrate-isolation --device-auth"
  echo "       That needs a codex CLI on this box. Without one: nobody runs Codex as $SVC_USER, so the"
  echo "       shared file is a passive copy -- move it aside (step 1) and rerun step 2 with --only <email>."
  echo "       A record it reports 'failed' is a dead chain: remove it and re-add it with an isolated"
  echo "       login targeted at this box (sr codex add)."
  echo "    4. $AS_SVC_TEXT $DEST codex isolation-check --json   (want \"ok\":true)"
  exit 3
fi
echo "DEPLOY OK  sha=${GOT_SHA:0:16}  backup=$BACKUP"
