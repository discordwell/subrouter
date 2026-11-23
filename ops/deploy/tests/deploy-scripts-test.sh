#!/usr/bin/env bash
# Tests for lightsail/deploy-box.sh, lightsail/bootstrap.sh, macos/deploy-laptop.sh,
# and the isolation guard in macos/rollback-laptop-peer.sh -- the Codex
# credential-isolation branches added for the 2026-09 upstream merge, and the
# paths they must not break -- plus lightsail/push-box.sh, deploy-laptop.sh's
# step 7 (old backups, the ops tools checkout, PATCHES §40) and bin/sr-triage
# (PATCHES §48). The step-7 cases build throwaway git repos under the test dir
# with the real git, never reading this machine's git config.
#
#   ops/bin/sr-local-ci --docker deploy                                      every case, on Linux in Docker
#   DEPLOY_TESTS_SANDBOX=1 bash ops/deploy/tests/deploy-scripts-test.sh              all cases, in parallel
#   DEPLOY_TESTS_SANDBOX=1 bash ops/deploy/tests/deploy-scripts-test.sh <substring>  matching cases, in order
#   DEPLOY_TESTS_SANDBOX=1 bash ops/deploy/tests/deploy-scripts-test.sh '=<case>'    exactly that case (quote it in zsh)
#
# Every external effect is a stub on PATH: sudo, systemctl, launchctl, curl,
# codesign, ss, lsof, journalctl, sleep, ssh, the subrouter binary itself (a
# script whose build id and isolation era the stubs read back). Before each script
# runs, the harness checks that every one of those names resolves to its stub
# and aborts the whole run otherwise. Each script runs under `env -i` with HOME in the
# test dir, so this shell's CODEX_HOME / SUBROUTER_STATE_DIR cannot leak in
# except where a case plants them on purpose, and every fixed path the box
# scripts touch is relocated under the test dir (DEPLOY_BOX_TEST_ROOT,
# BOOTSTRAP_TEST_ROOT). Each script also gets a label and an address that
# only this test uses, and a case fails if a stub records the live job's label
# or port. When this test ends, by itself or on HUP, INT or TERM, it kills
# every detached run its case started before it removes the test dir
# (PATCHES §76).
#
# DEPLOY_TESTS_SANDBOX=1 (macOS) additionally runs each script inside
# sandbox-exec: writes outside the test dir, network, exec of the real
# sudo/launchctl/codesign/security/tailscale, and reads of the real
# ~/.subrouter, ~/.codex and ~/.claude are denied. It makes every exec several
# times slower. On macOS the test refuses to run without it or sr-local-ci's
# sandbox (SR_LOCAL_CI_SANDBOX=1), and checks that exec of /bin/launchctl is
# refused before any case runs (PATCHES §76.6). On Linux it changes nothing.
set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd -P)
DEPLOY=$(cd "$HERE/.." && pwd -P)
BOX_SH="$DEPLOY/lightsail/deploy-box.sh"
BOOT_SH="$DEPLOY/lightsail/bootstrap.sh"
PUSH_SH="$DEPLOY/lightsail/push-box.sh"
LAPTOP_SH="$DEPLOY/macos/deploy-laptop.sh"
LEASE_LIB="$(cd "$DEPLOY/../.." && pwd -P)/deploy/macos/mutation-lease-lib.sh"
FILTER="${1:-}"

PY3=$(command -v python3) || { echo "python3 is required" >&2; exit 2; }
BASH5=$(command -v bash)
# deploy-laptop.sh must also run under macOS's stock bash 3.2.
LAPTOP_BASH=/bin/bash; [ -x "$LAPTOP_BASH" ] || LAPTOP_BASH="$BASH5"
USER_HOME=$(eval echo "~$(id -un)")
# In a login session that has ended (PATCHES §47) `id -un` prints the uid, so
# this stays a literal `~502`, and the sandbox would fence a path that does not
# exist while every case still passed.
if [ "${DEPLOY_TESTS_SANDBOX:-0}" = 1 ]; then
  case "$USER_HOME" in
    /*) ;;
    *) echo "cannot resolve this user's home (got '$USER_HOME'; directory services are unreachable from this shell), so DEPLOY_TESTS_SANDBOX=1 would fence nothing" >&2; exit 2 ;;
  esac
fi

# DEPLOY_TESTS_SANDBOX=1's profile, for a test dir. /private/tmp: macOS
# /bin/bash 3.2 writes here-document temp files there whatever TMPDIR says, and
# deploy-laptop.sh must run under it.
sandbox_profile() { # test dir
  printf '%s' "(version 1)(allow default)
(deny file-write* (require-not (require-any (subpath \"$1\") (subpath \"/private/tmp\") (literal \"/dev/null\") (literal \"/dev/tty\") (literal \"/dev/dtracehelper\") (subpath \"/dev/fd\"))))
(deny process-exec (literal \"/usr/bin/sudo\") (literal \"/bin/launchctl\") (literal \"/usr/bin/codesign\") (literal \"/usr/bin/security\") (literal \"/usr/bin/open\") (literal \"/opt/homebrew/bin/tailscale\") (literal \"/usr/local/bin/tailscale\"))
(deny file-read* (subpath \"$USER_HOME/.subrouter\") (subpath \"$USER_HOME/.codex\") (subpath \"$USER_HOME/.claude\") (subpath \"$HOME/.subrouter\") (subpath \"$HOME/.codex\") (subpath \"$HOME/.claude\"))
(deny network-outbound (remote ip))"
}

# On macOS only stubs stand between a script and the real launchctl, and a
# stopped test run once unloaded the live service (PATCHES §76). So a run there
# needs a sandbox that refuses exec of launchctl. A variable that names one is
# not proof: before any case runs, exec of /bin/launchctl must fail with EPERM,
# as a sandbox fails it (PATCHES §76.6). If the sandbox lets it run, it runs
# `launchctl version`, which changes nothing.
refuse_unfenced() { # why
  {
    echo "refusing to run the deploy script cases on macOS outside a sandbox: $1. They drive scripts that call launchctl, and a stopped test run once unloaded the live subrouter service (ops/PATCHES.md §76). Run them with one of:"
    echo "	ops/bin/sr-local-ci --docker deploy                                   (Linux, in Docker)"
    echo "	ops/bin/sr-local-ci deploy                                            (this Mac, in sr-local-ci's sandbox)"
    echo "	DEPLOY_TESTS_SANDBOX=1 bash ops/deploy/tests/deploy-scripts-test.sh   (this Mac, each script sandboxed; add a case filter)"
  } >&2
  exit 2
}
SANDBOX_EXEC=""
if [ "$(uname -s)" = Darwin ]; then
  if [ "${DEPLOY_TESTS_SANDBOX:-0}" = 1 ]; then
    SANDBOX_EXEC=$(command -v sandbox-exec) || refuse_unfenced "DEPLOY_TESTS_SANDBOX=1, but there is no sandbox-exec on PATH"
    fenced="DEPLOY_TESTS_SANDBOX=1"
    probe=$("$SANDBOX_EXEC" -p "$(sandbox_profile /var/empty)" /bin/launchctl version 2>&1); probe_rc=$?
  elif [ "${SR_LOCAL_CI_SANDBOX:-0}" = 1 ]; then
    fenced="SR_LOCAL_CI_SANDBOX=1"
    probe=$(/bin/launchctl version 2>&1); probe_rc=$?
  else
    refuse_unfenced "neither DEPLOY_TESTS_SANDBOX=1 nor SR_LOCAL_CI_SANDBOX=1 is set"
  fi
  case "$probe_rc:$probe" in
    0:*) refuse_unfenced "$fenced is set, but exec of /bin/launchctl was not refused: it ran (${probe:-no output})" ;;
    *[Oo]"peration not permitted"*) ;;
    *) refuse_unfenced "$fenced is set, but exec of /bin/launchctl was not refused by a sandbox (rc $probe_rc: ${probe:-no output})" ;;
  esac
elif [ "${DEPLOY_TESTS_SANDBOX:-0}" = 1 ]; then
  SANDBOX_EXEC=$(command -v sandbox-exec)
fi

# With no case filter, fan the cases out: each runs as its own copy of this
# script ("=name" is an exact match), DEPLOY_TESTS_JOBS at a time, and the
# reports are printed in file order. Exec latency dominates, not CPU.
if [ -z "$FILTER" ] && [ "${DEPLOY_TESTS_JOBS:-6}" -gt 1 ]; then
  SELF="$HERE/$(basename "$0")"
  # Never `cd "$(mktemp -d)"` in one step: a failed mktemp makes that `cd ""`,
  # which stays put, and the rm -rf below would then remove the working dir.
  RUN=$(mktemp -d "${TMPDIR:-/tmp}/deploy-tests-run.XXXXXX") && [ -d "$RUN" ] || { echo "mktemp failed" >&2; exit 2; }
  sed -n 's/^if begin \([A-Za-z0-9_-]*\); then$/\1/p' "$SELF" > "$RUN/cases"
  [ -s "$RUN/cases" ] || { echo "no cases found in $SELF" >&2; exit 1; }
  xargs -P "${DEPLOY_TESTS_JOBS:-6}" -I{} sh -c 'bash "$1" "=$2" > "$3/$2.out" 2>&1; echo $? > "$3/$2.rc"' \
    sh "$SELF" {} "$RUN" < "$RUN/cases"
  total=0; failed=0; failing=""
  while IFS= read -r c; do
    total=$((total + 1))
    grep -E '^  (ok|FAIL|---|\|)|^ABORT' "$RUN/$c.out"
    if [ "$(cat "$RUN/$c.rc" 2>/dev/null)" != 0 ]; then
      failed=$((failed + 1)); failing="$failing $c"
      grep -qE '^  (ok|FAIL)|^ABORT' "$RUN/$c.out" || { echo "  FAIL $c: no verdict"; tail -5 "$RUN/$c.out" | sed 's/^/  | /'; }
    fi
  done < "$RUN/cases"
  rm -rf "$RUN"
  echo
  echo "deploy script tests: $total cases, $((total - failed)) passed, $failed failed"
  [ "$failed" -eq 0 ] || { echo "failing cases:$failing"; exit 1; }
  exit 0
fi

T0=$(mktemp -d "${TMPDIR:-/tmp}/deploy-tests.XXXXXX") && [ -d "$T0" ] || { echo "mktemp failed" >&2; exit 2; }
T=$(cd "$T0" && pwd -P) && [ -d "$T" ] && [ "$T" != / ] || { echo "cannot resolve $T0" >&2; exit 2; }
# A detached run that outlived this test's stubs once reached the real
# launchctl with the live job's label, and unloaded the developer's service
# (PATCHES §76). So every script gets this label and address (run_sandboxed),
# and finish fails a case whose stubs were handed the live ones.
TEST_JOB="test.subrouter.deploytests.$$"
TEST_ADDR="127.0.0.1:31497"
LIVE_NAMES='ai\.manaflow\.subrouter|:31415([^0-9]|$)'
# The box scripts run against a box whose own daemon listens on :31415 at the
# box's address, so for them only a loopback or wildcard :31415 is this
# machine's live daemon. Their cases give the box's loopback listener TEST_ADDR.
BOX_LIVE_NAMES='ai\.manaflow\.subrouter|(127\.0\.0\.1|localhost|\[::1\]|0\.0\.0\.0):31415([^0-9]|$)'
export TEST_JOB TEST_ADDR
# And no detached run outlives the dir: each one still running is killed
# first. A detached run ignores HUP, INT and TERM, so it gets SIGKILL, and its
# whole process group is stopped before that: a script waiting on its lease
# helper would carry on if the helper died first. A run that started just
# before a signal can write its pid a moment late.
STOPPED=0
# The process group of the detached run a pid file names, only when that pid
# is still a run of this test, with this test's dir in its arguments, and the
# group is neither this test's own nor 0 or 1. A run that died can leave its
# pid to another process, a pid of 0 names this test's own group, and kill
# -1 reaches every process this user owns (PATCHES §76.6).
run_group() { # pid file
  local pid
  pid=$(cat "$1" 2>/dev/null) || return 1
  case "$pid" in ''|*[!0-9]*) return 1 ;; esac
  [ "$pid" -gt 1 ] || return 1
  case "$(ps -ww -o args= -p "$pid" 2>/dev/null)" in *"$T/"*) ;; *) return 1 ;; esac
  "$PY3" -c 'import os, sys
group = os.getpgid(int(sys.argv[1]))
sys.exit(1) if group <= 1 or group == os.getpgid(0) else print(group)' "$pid" 2>/dev/null
}
stop_detached_runs() {
  local f pgid
  while IFS= read -r f; do
    { [ -n "$f" ] && [ ! -e "${f%.pid}.rc" ]; } || continue
    pgid=$(run_group "$f") || continue
    kill -STOP -- "-$pgid" 2>/dev/null
    kill -KILL -- "-$pgid" 2>/dev/null
  done <<EOF
$(find "$T" -path '*/.subrouter/transitions/*.pid' 2>/dev/null)
EOF
}
# chmod first: the unreadable-store cases leave mode-000 directories behind if
# they stop early, and neither find nor rm -rf can see into those.
cleanup() {
  [ -n "${KEEP:-}" ] || chmod -R u+rwx "$T" 2>/dev/null
  stop_detached_runs
  [ "$STOPPED" = 0 ] || { /bin/sleep 0.5; stop_detached_runs; }
  [ -n "${KEEP:-}" ] || rm -rf "$T"
}
[ -n "${KEEP:-}" ] && echo "keeping $T"
trap cleanup EXIT
trap 'STOPPED=1; exit 129' HUP
trap 'STOPPED=1; exit 130' INT
trap 'STOPPED=1; exit 143' TERM
PASS=0; FAILED=0; FAIL_LIST=""; CASE=""; C=""; RC=0

SANDBOX=""
[ -z "$SANDBOX_EXEC" ] || SANDBOX=$(sandbox_profile "$T")

# --- stubs --------------------------------------------------------------------
# #!/bin/bash, not env: half the execs, and the stubs stay bash-3.2 clean.
stub() { # name body
  printf '#!/bin/bash\n%s\n' "$2" > "$C/bin/$1"
  chmod 755 "$C/bin/$1"
}

# Abort the whole run unless every command that could touch this machine
# resolves to its stub under the PATH the scripts get.
SHADOWED="sudo systemctl launchctl codesign curl ss lsof journalctl kill timeout sleep chown tailscale ssh"
assert_shadowed() { # PATH
  local name got
  for name in $SHADOWED; do
    got=$(PATH="$1" type -P "$name" 2>/dev/null)
    if [ "$got" != "$C/bin/$name" ]; then
      echo "ABORT $CASE: '$name' resolves to '${got:-nothing}', not its stub; refusing to run" >&2
      exit 99
    fi
  done
}

make_common_stubs() {
  ln -s "$PY3" "$C/bin/python3"
  stub sleep 'exit 0'
  stub codesign 'echo "codesign $*" >> "$FAKE_LOG"
case " $* " in *" -s "*)
  [ -z "${FAKE_SIGN_FAILS:-}" ] || exit 1
  # FAKE_SIGN_FAILS_AFTER=<n>: the first n signatures work, and every later one fails.
  if [ -n "${FAKE_SIGN_FAILS_AFTER:-}" ]; then
    n=$(( $(cat "$FAKE_LOG.signs" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$FAKE_LOG.signs"
    [ "$n" -gt "$FAKE_SIGN_FAILS_AFTER" ] && exit 1
  fi
  # Once a build is installed, FAKE_SIGN_MOVES_JOB=<plist>: launchd answers for a
  # job loaded from <plist>, as the real one did once the stubs were gone;
  # FAKE_SIGN_REMOVES_PLIST=1: the plist the job was loaded from is gone.
  [ -z "${FAKE_SIGN_MOVES_JOB:-}" ] || echo "$FAKE_SIGN_MOVES_JOB" > "$FAKE_LAUNCHD/loaded-from"
  [ -z "${FAKE_SIGN_REMOVES_PLIST:-}" ] || rm -f "$FAKE_PLIST" ;;
esac
case " $* " in *" -d"*) for f; do :; done; echo "CDHash=$(sed -n "s/^# fake subrouter build \([^ ]*\).*/\1/p" "$f")" >&2 ;; esac
exit 0'
  stub chown 'echo "chown $*" >> "$FAKE_LOG"; exit 0'
  # The journal of one PID holds serve's startup line.
  # A daemon started with public admin on (FAKE_PUBLIC_ADMIN, the NEW build)
  # logs PUBLIC ADMIN IS ON, unless FAKE_BAD names public-admin-silent.
  stub journalctl '
case " $* " in
  *" _PID="*)
    if [ -n "${FAKE_PUBLIC_ADMIN:-}" ] && [ "$(cat "$FAKE_RUNNING" 2>/dev/null)" = "${FAKE_NEW_ID:-new}" ]; then
      case " ${FAKE_BAD:-} " in
        *" public-admin-silent "*) ;;
        *) echo "time=2026-09-29T00:00:00Z level=WARN msg=\"PUBLIC ADMIN IS ON (--public-admin / SUBROUTER_PUBLIC_ADMIN): named admin-role access tokens are ADMIN on the public listener\"" ;;
      esac
    fi
    # A daemon that has logged on since, as a real one has by step 8: more
    # than a pipe holds after the line step 8 looks for.
    if [ -n "${FAKE_JOURNAL_TAIL:-}" ]; then printf "%0300000d\n" 0; fi ;;
  *) echo "(journal)" ;;
esac'
  # FAKE_SS_TAIL: many more listeners after the public one, more than a pipe
  # holds, so a reader that stops at the match leaves ss/awk still writing.
  # FAKE_NO_PUBLIC: nothing listens on :31416. FAKE_BAD=public-unbound: the
  # NEW build does not bind it.
  stub ss 'pub="LISTEN 0 4096 *:31416 *:*"
[ -n "${FAKE_NO_PUBLIC:-}" ] && pub=""
case " ${FAKE_BAD:-} " in *" public-unbound "*) [ "$(cat "$FAKE_RUNNING" 2>/dev/null)" = "${FAKE_NEW_ID:-new}" ] && pub="" ;; esac
printf "State Recv-Q Send-Q Local Address:Port Peer Address:Port\n"
[ -n "$pub" ] && echo "$pub"
[ -n "${FAKE_SS_TAIL:-}" ] && awk "BEGIN { for (i = 0; i < 50000; i++) printf \"LISTEN 0 4096 127.0.0.1:%d *:*\\n\", 100000 + i }"
exit 0'
  # A build named by FAKE_RESTART_DRAINS drains on SIGTERM (PATCHES §38): its
  # main PID stays alive until kill -9 reaches it, which leaves $FAKE_SYSTEMD/killed,
  # or until its drain ends by itself (FAKE_DRAIN_SECS). Every build answers as
  # pid 4242, so the pid a script read is alive only while no process has
  # started since the read (box_setup's systemctl counts starts in gen): a
  # restart that already started the next build must not read it as draining.
  stub kill '
echo "kill $*" >> "$FAKE_LOG"
case "$1" in
  -0)
    [ -n "${FAKE_RESTART_DRAINS:-}" ] && [ "$(cat "$FAKE_RUNNING" 2>/dev/null)" = "$FAKE_RESTART_DRAINS" ] &&
      [ "$(cat "$FAKE_SYSTEMD/gen" 2>/dev/null)" = "$(cat "$FAKE_SYSTEMD/mainpid-gen" 2>/dev/null)" ] &&
      [ ! -e "$FAKE_SYSTEMD/killed" ] && exit 0
    exit 1 ;;
  -9) echo "killed at=$(date +%s)" >> "$FAKE_LOG"
      [ -n "${FAKE_RESTART_DRAINS:-}" ] && : > "$FAKE_SYSTEMD/killed" ;;
esac
exit 0'
  stub timeout 'shift; exec "$@"'
  # flock -n <fd>, as util-linux runs it: an exclusive lock on the open file
  # the caller holds as <fd>, which outlives this process while the caller
  # keeps the descriptor.
  stub flock 'nb=0; [ "$1" = -n ] && { nb=1; shift; }
exec python3 -c "import fcntl, sys
try:
    fcntl.flock(int(sys.argv[1]), fcntl.LOCK_EX | (fcntl.LOCK_NB if sys.argv[2] == \"1\" else 0))
except OSError:
    sys.exit(1)" "$1" "$nb"'
  stub tailscale 'echo "tailscale $*" >> "$FAKE_LOG"; exit 1'
  # push_setup installs the working fake; no case reaches a real host.
  stub ssh 'echo "UNEXPECTED ssh $*" >> "$FAKE_LOG"; exit 97'
  # The service managers default to refusing; box_setup / laptop_setup install
  # the working fakes. Either way the real one is never on PATH.
  stub systemctl 'echo "UNEXPECTED systemctl $*" >> "$FAKE_LOG"; exit 97'
  stub launchctl 'echo "UNEXPECTED launchctl $*" >> "$FAKE_LOG"; exit 97'
  stub lsof 'echo "UNEXPECTED lsof $*" >> "$FAKE_LOG"; exit 97'
  stub sudo '
echo "sudo $*" >> "$FAKE_LOG"
while [ $# -gt 0 ]; do
  case "$1" in
    -u) export FAKE_SUDO_USER="$2"; shift 2 ;;
    -n|-E|-H) shift ;;
    --) shift; break ;;
    *) break ;;
  esac
done
exec "$@"'
  # One curl for both machines. The daemon that answers is whichever build id
  # is in $FAKE_RUNNING (empty = down); FAKE_* scenario knobs apply only while
  # the NEW build ($FAKE_NEW_ID) is the one running, so a rollback heals.
  # Who owns the Claude socket (PATCHES §84) holds for the whole run:
  #   FAKE_CLAUDE_WORD         the claude_socket word of a health answer at the
  #                            path (standby is a stand-in)
  #   FAKE_SV_SOCKET           supervisor-status claude_socket (default serving)
  #   FAKE_SV_YIELD=<reason> <until>  supervisor-status claude_socket_yield
  #   FAKE_DAEMON_SOCKET_WORD  claude_socket in a plain serve's TCP health
  stub curl '
url=""; out=""; fmt=""; auth=""; import=""; failflag=0; sock=""; method=GET; maxtime=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -w) fmt="$2"; shift 2 ;;
    --unix-socket) sock="$2"; shift 2 ;;
    -X) method="$2"; shift 2 ;;
    -m) maxtime="$2"; shift 2 ;;
    -d) shift 2 ;;
    -H) case "$2" in
          "Authorization: Bearer "*) auth="${2#Authorization: Bearer }" ;;
          "X-Subrouter-Account-Import-Token: "*) import="${2#X-Subrouter-Account-Import-Token: }" ;;
        esac; shift 2 ;;
    -*) case "$1" in *f*) failflag=1 ;; esac; shift ;;
    *) url="$1"; shift ;;
  esac
done
echo "curl $url" >> "$FAKE_LOG"
pat="%{http_code}"
running=$(cat "$FAKE_RUNNING" 2>/dev/null)
# The drain socket (PATCHES §38.1), served by the builds FAKE_DRAIN_SOCKET
# names. The process answers it until it exits, through its SIGTERM drain, so
# this is keyed on the process still running, not on its listeners.
if [ -n "$sock" ]; then
  sup=$(cat "${FAKE_LAUNCHD:-/nonexistent}/supervisor" 2>/dev/null)
  lpath="/${url#*://*/}"
  door=""
  if [ "$sock" = "${FAKE_CONTROL_SOCKET:-}" ]; then door=control
  elif [ "$sock" = "${FAKE_CLAUDE_SOCKET:-}" ]; then door=claude
  elif [ -n "${FAKE_DRAIN:-}" ]; then door=drain; fi
  if [ -n "$door" ]; then
    echo "curl unix $sock $method $url" >> "$FAKE_LOG"
    code=200; body="{}"; up=1
    case "$door" in
      control)
        [ -n "$sup" ] || up=0
        case "$method $lpath" in
          "GET /_subrouter/supervisor-status")
            [ -z "${FAKE_SV_STATUS_DOWN:-}" ] || up=0
            owner=", \"claude_socket\": \"${FAKE_SV_SOCKET:-serving}\""
            [ -z "${FAKE_SV_YIELD:-}" ] || owner="$owner, \"claude_socket_yield\": {\"reason\": \"${FAKE_SV_YIELD%% *}\", \"until\": \"${FAKE_SV_YIELD#* }\"}"
            [ -e "$FAKE_LAUNCHD/supervisor-lacks-socket" ] && owner=""
            body=$(printf "{\"accepting\": true, \"active_worker\": {\"executable_identity\": \"%s\"}%s, \"version\": \"%s\"}" "$running" "$owner" "$sup") ;;
          "POST /_subrouter/upgrade")
            id=$(sed -n "s/^# fake subrouter build \([^ ]*\).*/\1/p" "$FAKE_DEST" 2>/dev/null)
            case " ${FAKE_UPGRADE_FAILS:-} " in
              *" $id "*) code=502; body="worker readiness timed out after 2m0s" ;;
              *) [ "${FAKE_UPGRADE_NOOP:-}" = "$id" ] || echo "$id" > "$FAKE_RUNNING"
                 echo "upgrade -> $id" >> "$FAKE_LOG"; body="{\"active\": {}}" ;;
            esac
            # FAKE_KILL_FG kills the foreground of the client once the
            # supervisor has decided, and FAKE_UPGRADE_TIMES_OUT answers as a
            # curl that gave up.
            if [ -n "${FAKE_KILL_FG:-}" ] && [ ! -e "$FAKE_LAUNCHD/killed" ]; then
              : > "$FAKE_LAUNCHD/killed"
              for _ in $(seq 1 100); do [ -s "$FAKE_KILL_FG" ] && break; /bin/sleep 0.05; done
              echo "SIGKILL the foreground process group" >> "$FAKE_LOG"
              kill -9 -- "-$(cat "$FAKE_KILL_FG")"
            fi
            if [ -n "${FAKE_UPGRADE_TIMES_OUT:-}" ]; then [ -n "$fmt" ] && printf "%b" "${fmt//"$pat"/000}"; exit 28; fi ;;
          *) code=404 ;;
        esac ;;
      claude)
        [ -n "$running" ] || up=0
        late=$(cat "$FAKE_LAUNCHD/socket-late" 2>/dev/null || echo 0)
        if [ "$up" = 1 ] && [ "$late" -gt 0 ]; then echo $((late - 1)) > "$FAKE_LAUNCHD/socket-late"; up=0; fi
        word=serving; [ -n "$sup" ] && word=owned_by_supervisor; word=${FAKE_CLAUDE_WORD:-$word}
        case "$lpath" in /_subrouter/health) body="{\"ok\": true, \"claude_socket\": \"$word\"}" ;; *) code=404 ;; esac ;;
      drain)
        { [ -z "$sup" ] && [ -n "$running" ] && [ "$lpath" = /_subrouter/drain-status ]; } || up=0
        turns=${FAKE_TURNS:-0}
        # FAKE_TURNS_QUEUE: one count per line, one line per answer, the last kept.
        if [ "$up" = 1 ] && [ -n "${FAKE_TURNS_QUEUE:-}" ]; then
          turns=$(head -1 "$FAKE_TURNS_QUEUE")
          if [ "$(wc -l < "$FAKE_TURNS_QUEUE")" -gt 1 ]; then tail -n +2 "$FAKE_TURNS_QUEUE" > "$FAKE_TURNS_QUEUE.next"; mv "$FAKE_TURNS_QUEUE.next" "$FAKE_TURNS_QUEUE"; fi
        fi
        if [ "$up" = 1 ] && [ -n "${FAKE_TURNS_FILE:-}" ]; then
          turns=$(cat "$FAKE_TURNS_FILE"); [ "$turns" -gt 0 ] && echo $((turns - 1)) > "$FAKE_TURNS_FILE"
        fi
        body=$(printf "{\"active_proxy_requests\": 4, \"active_websocket_turns\": %s, \"draining\": false}" "$turns") ;;
    esac
    if [ "$up" = 0 ]; then [ -n "$fmt" ] && printf "%b" "${fmt//"$pat"/000}"; exit 7; fi
    if [ -n "$out" ]; then printf "%s" "$body" > "$out"; else printf "%s" "$body"; fi
    [ -n "$fmt" ] && printf "%b" "${fmt//"$pat"/$code}"
    exit 0
  fi
  echo "curl unix $sock $method ${url#http://drain-socket} auth=${auth:-none}" >> "$FAKE_LOG"
  case " ${FAKE_DRAIN_SOCKET:-} " in
    *" $running "*) ;;
    *) [ -n "$fmt" ] && printf "%b" "${fmt//"$pat"/000}"; exit 7 ;;
  esac
  S="$FAKE_SYSTEMD"; code=200
  # FAKE_STATUS_HANGS: a process in its SIGTERM drain accepts on its drain
  # socket and answers drain-status only after that many seconds, so curl
  # gives up at its -m.
  if [ -n "${FAKE_STATUS_HANGS:-}" ] && [ -e "$S/sigterm-$running" ] && [ "$method ${url#http://drain-socket}" = "GET /_subrouter/drain-status" ]; then
    wait_s=$FAKE_STATUS_HANGS; [ -n "$maxtime" ] && [ "$maxtime" -lt "$wait_s" ] && wait_s=$maxtime
    echo "drain-status hung for ${wait_s}s (curl -m ${maxtime:-none})" >> "$FAKE_LOG"
    /bin/sleep "$wait_s"; exit 28
  fi
  case "$method ${url#http://drain-socket}" in
    "POST /_subrouter/drain") : > "$S/drained-$running" ;;
    "POST /_subrouter/resume")
      if [ -e "$S/sigterm-$running" ]; then code=409; else rm -f "$S/drained-$running"; fi ;;
    "GET /_subrouter/drain-status") ;;
    *) code=404 ;;
  esac
  draining=false; shutting=false
  [ -e "$S/drained-$running" ] && draining=true
  [ -e "$S/sigterm-$running" ] && { draining=true; shutting=true; }
  printf "{\n  \"active_proxy_requests\": %s,\n  \"active_websocket_turns\": %s,\n  \"draining\": %s,\n  \"shutting_down\": %s\n}\n" \
    "${FAKE_OPEN:-3}" "${FAKE_TURNS:-0}" "$draining" "$shutting"
  [ -n "$fmt" ] && printf "%b" "${fmt//"$pat"/$code}"
  exit 0
fi
# A draining build (FAKE_RESTART_DRAINS) that is no longer the one installed has
# had, or is about to get, its SIGTERM: its listener is closed and its port
# queues, which a client sees as nothing answering. Keyed on the install rather
# than on the restart itself, which the script backgrounds, so the probes see
# the drain whichever runs first.
if [ -n "${FAKE_RESTART_DRAINS:-}" ] && [ "$running" = "$FAKE_RESTART_DRAINS" ] &&
  [ "$(sed -n "s/^# fake subrouter build \([^ ]*\).*/\1/p" "$FAKE_DEST" 2>/dev/null)" != "$running" ]; then
  running=""
fi
scheme=${url%%://*}; rest=${url#*://}; path="/${rest#*/}"
code=200; body="{}"
new=0; [ "$running" = "$FAKE_NEW_ID" ] && new=1
bad() { [ "$new" = 1 ] && case " ${FAKE_BAD:-} " in *" $1 "*) return 0 ;; esac; return 1; }
ready() { # $1 = mode
  case "$1" in
    startup) code=503; body=$(printf "{\n  \"draining\": false,\n  \"ok\": false,\n  \"startup_ready\": false\n}") ;;
    draining) code=503; body=$(printf "{\n  \"draining\": true,\n  \"ok\": false\n}") ;;
    draining-startup) code=503; body=$(printf "{\n  \"draining\": true,\n  \"ok\": false,\n  \"startup_ready\": false\n}") ;;
    missing) code=404; body="404 page not found" ;;
    *) code=200; body=$(printf "{\n  \"draining\": false,\n  \"ok\": true\n}") ;;
  esac
}
if [ -z "$running" ]; then
  [ -n "$fmt" ] && printf "%b" "${fmt//"$pat"/000}"
  exit 7
fi
if [ "$scheme" = https ]; then
  case "$path" in
    /_subrouter/usage-status)
      if [ "$auth" = "$FAKE_PROXY" ]; then code=200; else code=401; fi ;;
    # A daemon started with --public-admin (PATCHES §43.1) mounts these for
    # named admin tokens and refuses the static admin and import tokens with
    # 401. public-admin-static lets the static admin token in; public-import-
    # static lets the import token past the import gate (a GET then 405s);
    # public-admin-unmounted mounts nothing. Transcripts are never mounted
    # there (§43.2); public-transcripts is a build that still mounts them.
    /_subrouter/transcripts)
      if [ -n "${FAKE_PUBLIC_ADMIN:-}" ] && bad public-transcripts; then code=401; else code=404; body="404 page not found"; fi ;;
    /_subrouter/sessions|/_subrouter/drain-status|/_subrouter/account-status|/_subrouter/account-import)
      if [ -n "${FAKE_PUBLIC_ADMIN:-}" ] && ! bad public-admin-unmounted; then
        if [ -n "$import" ]; then
          if bad public-import-static && [ "$import" = "$FAKE_IMPORT" ]; then code=405; else code=401; fi
        elif bad public-admin-static; then code=200; else code=401; fi
      else code=404; body="404 page not found"; fi ;;
    # The Claude reset claim is marked since §46.9: mounted with the rest
    # (a request with no token is 401 at the gate); public-reset-unmounted is
    # a build from before §46.9 that keeps it private.
    /_subrouter/claude-reset)
      if [ -n "${FAKE_PUBLIC_ADMIN:-}" ] && ! bad public-admin-unmounted && ! bad public-reset-unmounted; then
        if [ -n "$auth" ] && bad public-admin-static; then code=405; else code=401; fi
      else code=404; body="404 page not found"; fi ;;
    /internal/v1/session-leases)
      if bad public-leases; then code=401; else code=404; body="404 page not found"; fi ;;
    /_subrouter/health) body=$(printf "{\n  \"ok\": true\n}") ;;
    /_subrouter/ready) m=ok; [ "$new" = 1 ] && m=${FAKE_READY_PUBLIC:-ok}; ready "$m" ;;
    /v1/messages) code=401 ;;
    *) code=404; body="404 page not found" ;;
  esac
else
  case "$path" in
    /_subrouter/health) body=$(printf "{\n  \"ok\": true%s\n}" "${FAKE_DAEMON_SOCKET_WORD:+, \"claude_socket\": \"$FAKE_DAEMON_SOCKET_WORD\"}") ;;
    /_subrouter/ready) m=ok; [ "$new" = 1 ] && m=${FAKE_READY_PRIVATE:-ok}; ready "$m" ;;
    /v1/messages) code=401 ;;
    /_subrouter/accounts)
      if bad accounts-404; then code=404; body="404 page not found"
      elif [ "$auth" != "$FAKE_ADMIN" ]; then code=401
      else
        n=${FAKE_POOL_SIZE:-12}; bad few-accounts && n=3
        body="["; i=0
        while [ $i -lt $n ]; do body="$body{\"provider\":\"claude\"},"; i=$((i+1)); done
        body="${body%,}]"
      fi ;;
    /api/oauth/profile)
      if bad identity-unavailable || case " ${FAKE_ALWAYS_BAD:-} " in *" identity-unavailable "*) true ;; *) false ;; esac; then
        code=429
      else
        body="{\"account\":{\"email\":\"pool@example.com\"}}"
      fi ;;
    *) code=404 ;;
  esac
fi
if [ "$failflag" = 1 ] && [ "$code" -ge 400 ]; then exit 22; fi
if [ -n "$out" ]; then printf "%s" "$body" > "$out"; else printf "%s" "$body"; fi
[ -n "$fmt" ] && printf "%b" "${fmt//"$pat"/$code}"
exit 0'
}

make_build() { # path pre|main|post id
  local supervise='"supervise ") echo "claude_socket=owned_by_supervisor"; exit 0 ;;'
  [ "$2" = main ] && supervise=""
  if [ "$2" != pre ]; then
    cat > "$1" <<EOF
#!/bin/bash
# fake subrouter build $3 (post-isolation)
echo "exec build=$3 bin=\$0 args=\$* sudo_user=\${FAKE_SUDO_USER:-none} HOME=\$HOME STATE=\${SUBROUTER_STATE_DIR:-<unset>} CODEX_HOME=\${CODEX_HOME:-<unset>} daemon=\$(cat "\$FAKE_DAEMON" 2>/dev/null)" >> "\$FAKE_LOG"
store="\${SUBROUTER_STATE_DIR:-\$HOME/.subrouter}/fake-codex-needing-migration"
case "\${1:-} \${2:-}" in
"codex isolation-check")
  if [ -n "\${FAKE_ISO_BROKEN:-}" ]; then echo "subrouter: open codex store: permission denied" >&2; exit 1; fi
  n=\$(cat "\$store" 2>/dev/null || echo 0)
  if [ "\$n" -gt 0 ]; then
    printf '{"schema_version":1,"ok":false,"accounts_requiring_migration":%s,"remediation":"sr codex migrate-isolation"}\n' "\$n"
    echo "subrouter: codex credential isolation preflight failed" >&2
    exit 1
  fi
  printf '{"schema_version":1,"ok":true,"accounts_requiring_migration":0}\n'
  exit 0 ;;
"codex attest-legacy")
  n=\$(cat "\$store" 2>/dev/null || echo 0)
  case " \$* " in *" --dry-run "*) echo "\$n stored Codex OAuth account(s) would be attested:"; exit 0 ;; esac
  # Every record attests, except one whose chain is still in the passive copy.
  shared=0
  [ "\$n" -gt 0 ] && [ -e "\${CODEX_HOME:-\$HOME/.codex}/auth.json" ] && shared=1
  echo "\$shared" > "\$store"
  echo "Attested \$((n - shared)), skipped \$shared, failed 0."
  exit 0 ;;
$supervise
esac
exit 0
EOF
  else
    cat > "$1" <<EOF
#!/bin/bash
# fake subrouter build $3 (pre-isolation)
echo "exec build=$3 bin=\$0 args=\$* (pre-isolation build: would launch the codex CLI)" >> "\$FAKE_LOG"
exit 0
EOF
  fi
  chmod 755 "$1"
}

any_exists() { local f; for f in "$@"; do [ -e "$f" ] && return 0; done; return 1; }

# A Codex store record where a post-isolation build keeps it: <state>/codex/
# accounts/<name>.json, indented the way json.MarshalIndent writes it. The
# origin decides whether a pre-isolation rollback may cross the boundary:
# isolated-server-login and server-attested-transfer are MIGRATED records;
# interactive-import and "" (no origin at all) are not. A 4th arg "compact"
# writes it without spaces around the colon.
seed_record() { # state-dir name origin [compact]
  mkdir -p "$1/codex/accounts"
  {
    printf '{\n  "email": "%s@example.com",\n' "$2"
    if [ -n "$3" ] && [ "${4:-}" = compact ]; then printf '  "oauthCredentialOrigin":"%s",\n' "$3"
    elif [ -n "$3" ]; then printf '  "oauthCredentialOrigin": "%s",\n' "$3"; fi
    printf '  "addedAt": "2026-09-20T00:00:00Z",\n  "auth": {}\n}\n'
  } > "$1/codex/accounts/$2.json"
}

# Mode 000 on a path, and a check that it took: a case that claims to test an
# unreadable store must fail loudly where it cannot build one (e.g. as root).
make_unreadable() { # path
  chmod 000 "$1"
  if [ -r "$1" ]; then bad "cannot make $1 unreadable (running as root?); the case proves nothing"; return 1; fi
  return 0
}

build_id() { sed -n 's/^# fake subrouter build \([^ ]*\).*/\1/p' "$1" 2>/dev/null; }

# --- assertions -------------------------------------------------------------
ok() { PASS=$((PASS + 1)); }
bad() {
  FAILED=$((FAILED + 1)); FAIL_LIST="$FAIL_LIST $CASE"
  echo "  FAIL $CASE: $1"
}
expect_rc() { [ "$RC" = "$1" ] && ok || bad "exit $RC, want $1"; }
expect_out() { grep -Fq -- "$1" "$C/out" && ok || bad "output lacks: $1"; }
expect_no_out() { grep -Fq -- "$1" "$C/out" && bad "output has: $1" || ok; }
# A whole line of output, for a command printed for a person to copy: a
# substring check also matches the same words inside a longer message.
expect_line() { grep -Fxq -- "$1" "$C/out" && ok || bad "output lacks the line: $1"; }
expect_log() { grep -Eq -- "$1" "$C/log" && ok || bad "log lacks /$1/"; }
expect_no_log() { grep -Eq -- "$1" "$C/log" && bad "log has /$1/" || ok; }
expect_no_restart() { expect_no_log "launchctl (kickstart|bootout|bootstrap)"; }
hold_lock() { # plist
  "$PY3" -c 'import fcntl, os, sys, time
fd = os.open(sys.argv[1] + ".supervisor-mutation.lock", os.O_WRONLY | os.O_CREAT, 0o600)
fcntl.flock(fd, fcntl.LOCK_EX)
open(sys.argv[2], "w").close()
time.sleep(300)' "$1" "$C/lock-held" &
  LOCK_HOLDER=$!
  while [ ! -e "$C/lock-held" ]; do /bin/sleep 0.05; done
}
release_lock() { kill "$LOCK_HOLDER" 2>/dev/null; wait "$LOCK_HOLDER" 2>/dev/null; }
# The box's deploy lock, held as a running deploy holds it, with its
# description in the file.
hold_deploy_lock() { # description
  "$PY3" -c 'import fcntl, os, sys, time
fd = os.open(sys.argv[1], os.O_RDWR | os.O_CREAT, 0o644)
fcntl.flock(fd, fcntl.LOCK_EX)
os.ftruncate(fd, 0); os.write(fd, sys.argv[2].encode() + b"\n")
open(sys.argv[3], "w").close()
time.sleep(300)' "$R/run/lock/subrouter-deploy.lock" "$1" "$C/lock-held" &
  LOCK_HOLDER=$!
  while [ ! -e "$C/lock-held" ]; do /bin/sleep 0.05; done
}
expect_lock_refusal() { expect_rc 1; expect_out "another deploy, rollback or update holds $1.supervisor-mutation.lock"; }
run_killable() { # runner args...
  local runner="$1" fg; shift
  rm -f "$C/fg.pid" "$C/launchd/killed"
  set -m
  "$runner" FAKE_KILL_FG="$C/fg.pid" "$@" &
  fg=$!
  set +m
  echo "$fg" > "$C/fg.pid"
  wait "$fg" 2>/dev/null; FG_RC=$?
  await_transitions
}
await_transitions() {
  local f n=0 pending
  while [ "$n" -lt 300 ]; do
    pending=0
    for f in "$H/.subrouter/transitions/"*.log; do [ -e "$f" ] && [ ! -e "${f%.log}.rc" ] && pending=1; done
    [ "$pending" = 0 ] && break
    /bin/sleep 0.1; n=$((n + 1))
  done
  [ "$pending" = 0 ] || bad "a detached run never wrote its exit code"
  TRANSITION_LOG=$(sed -n 's/^   log: \(.*\.log\)   exit code: .*/\1/p' "$C/out" | tail -1)
  TRANSITION_RC=$(cat "${TRANSITION_LOG%.log}.rc" 2>/dev/null)
}
expect_killed_and_finished() { # what the detached run's log must end with
  [ "$FG_RC" = 137 ] && ok || bad "the foreground exited $FG_RC, want 137 (SIGKILL)"
  expect_log "SIGKILL the foreground process group"; expect_no_out "$1"
  [ "$TRANSITION_RC" = 0 ] && ok || bad "the detached run exited '$TRANSITION_RC', want 0"
  grep -Fq -- "$1" "$TRANSITION_LOG" && ok || bad "the detached run's log lacks: $1"
}
expect_loaded() { # serve | supervised
  local got
  if [ ! -e "$C/launchd/loaded" ]; then bad "launchd has no job loaded"; return; fi
  if grep -q "<string>supervise</string>" "$C/launchd/loaded"; then got=supervised; else got=serve; fi
  [ "$got" = "$1" ] && ok || bad "launchd runs $got, want $1"
}
after_first_bootstrap() {
  awk '/^launchctl bootstrap/ && !seen { seen = 1; next } seen && /^launchctl bootout/ { exit } seen' "$C/log"
}
expect_build() { # path id
  local got; got=$(build_id "$1")
  [ "$got" = "$2" ] && ok || bad "$(basename "$1") holds build '$got', want '$2'"
}
# The line number in the log of the first match, or 999999.
log_line() { local n; n=$(grep -En -- "$1" "$C/log" | head -1 | cut -d: -f1); echo "${n:-999999}"; }
expect_order() { # regex-before regex-after
  [ "$(log_line "$1")" -lt "$(log_line "$2")" ] && ok || bad "log order: /$1/ must precede /$2/"
}
expect_none_between() { # regex-from regex-to regex: no log line matches the third between the first two
  local a b; a=$(log_line "$1"); b=$(log_line "$2")
  if [ "$a" -ge "$b" ]; then bad "log order: /$1/ must precede /$2/"
  elif sed -n "$((a + 1)),$((b - 1))p" "$C/log" | grep -Eq -- "$3"; then bad "log has /$3/ between /$1/ and /$2/"
  else ok; fi
}

begin() { # case name; returns 1 when filtered out
  CASE="$1"
  case "$FILTER" in
    "") ;;
    =*) [ "$CASE" = "${FILTER#=}" ] || return 1 ;;
    *) [ "${CASE#*"$FILTER"}" != "$CASE" ] || return 1 ;;
  esac
  C="$T/$CASE"
  mkdir -p "$C/bin" "$C/tmp" "$C/home"
  : > "$C/log"
  make_common_stubs
  return 0
}

finish() {
  local live=$LIVE_NAMES
  case "$CASE" in box-*|boot-*|push-*) live=$BOX_LIVE_NAMES ;; esac
  if grep -Eq -- "$live" "$C/log" 2>/dev/null; then
    bad "a stub was handed the live job's label or port: $(grep -Em1 -- "$live" "$C/log")"
  fi
  if [ -n "$FAIL_LIST" ] && [ "${FAIL_LIST##* }" = "$CASE" ]; then
    echo "  --- $CASE output (tail) ---"; tail -25 "$C/out" | sed 's/^/  | /'
    echo "  --- $CASE log (tail) ---"; tail -15 "$C/log" | sed 's/^/  | /'
  else
    echo "  ok   $CASE"
  fi
}

run_sandboxed() { # env-assignments... -- command...
  local envs=(SUBROUTER_LABEL="$TEST_JOB" SUBROUTER_ADDR="$TEST_ADDR" CLAUDE_ROUTE_TCP="http://$TEST_ADDR") e
  # DEPLOY_TESTS_HOLD=1: a kickstart writes its process group to $T/hold.held
  # and waits, so deploy_scripts_test.go can stop this test while a detached
  # run is in flight.
  [ -z "${DEPLOY_TESTS_HOLD:-}" ] || envs+=(FAKE_HOLD="$T/hold")
  while [ $# -gt 0 ] && [ "$1" != -- ]; do envs+=("$1"); shift; done
  shift
  for e in "${envs[@]}"; do case "$e" in PATH=*) assert_shadowed "${e#PATH=}" ;; esac; done
  if [ -n "$SANDBOX" ]; then
    env -i "${envs[@]}" "$SANDBOX_EXEC" -p "$SANDBOX" "$@" > "${OUT:-$C/out}" 2>&1
  else
    env -i "${envs[@]}" "$@" > "${OUT:-$C/out}" 2>&1
  fi
  RC=$?
}

# --- the box ------------------------------------------------------------------
box_setup() { # old-era new-era
  R="$C/root"; H="$C/home"
  mkdir -p "$R/etc/default" "$R/tmp" "$R/run/lock" "$R/usr/local/bin" "$R/var/lib/subrouter/.codex" "$C/systemd"
  DEST="$R/usr/local/bin/subrouter"
  NEWBIN="$R/tmp/subrouter.new"
  make_build "$DEST" "$1" old
  make_build "$NEWBIN" "$2" new
  cat > "$R/etc/default/subrouter" <<EOF
SUBROUTER_ADDR=$TEST_ADDR
SUBROUTER_PROXY_TOKEN=proxy-tok
SUBROUTER_ACCOUNT_IMPORT_TOKEN=import-tok
SUBROUTER_EXTRA_ARGS="--public-addr 0.0.0.0:31416"
SUBROUTER_STATE_DIR=$R/var/lib/subrouter
EOF
  echo active > "$C/systemd/subrouter.service"
  echo active > "$C/systemd/subrouter.socket"
  echo old > "$C/systemd/running"
  echo 0 > "$C/systemd/gen"
  echo 0 > "$R/var/lib/subrouter/fake-codex-needing-migration"
  # FAKE_KILL_FG: the ssh connection drops at the first restart (or, with
  # FAKE_KILL_FG_AT=stop, the first stop), which kills the foreground's
  # process group, as run_killable sets it up.
  stub systemctl '
echo "systemctl $*" >> "$FAKE_LOG"
S="$FAKE_SYSTEMD"
kill_fg() {
  [ -n "${FAKE_KILL_FG:-}" ] && [ "$1" = "${FAKE_KILL_FG_AT:-restart}" ] && [ ! -e "$S/fg-killed" ] || return 0
  : > "$S/fg-killed"
  for _ in $(seq 1 100); do [ -s "$FAKE_KILL_FG" ] && break; /bin/sleep 0.05; done
  echo "SIGKILL the foreground process group" >> "$FAKE_LOG"
  kill -9 -- "-$(cat "$FAKE_KILL_FG")"
}
start_service() {
  id=$(sed -n "s/^# fake subrouter build \([^ ]*\).*/\1/p" "$FAKE_DEST" 2>/dev/null)
  # A new process starts undrained, and is the next generation.
  rm -f "$S/drained-$id" "$S/sigterm-$id"
  echo $(( $(cat "$S/gen") + 1 )) > "$S/gen"
  if [ -n "${FAKE_CRASH_BUILD:-}" ] && [ "$id" = "$FAKE_CRASH_BUILD" ]; then
    echo failed > "$S/subrouter.service"; : > "$S/running"
  else
    echo active > "$S/subrouter.service"; echo "$id" > "$S/running"
  fi
  echo active > "$S/subrouter.socket"
}
case "$1" in
  show)
    case "$*" in
      *MainPID*) cp "$S/gen" "$S/mainpid-gen"; echo 4242 ;;
      *TimeoutStopUSec*) echo "${FAKE_UNIT_STOP-10min}" ;;
      *"-p User"*) echo "$FAKE_SVC_USER" ;;
      *Environment*) echo "HOME=$FAKE_SVC_HOME${FAKE_UNIT_ENV:+ $FAKE_UNIT_ENV}" ;;
    esac ;;
  is-active)
    shift; rc=0
    for u in "$@"; do s=$(cat "$S/$u" 2>/dev/null || echo inactive); echo "$s"; [ "$s" = active ] || rc=3; done
    exit $rc ;;
  restart)
    kill_fg restart
    echo "restart began at=$(date +%s)" >> "$FAKE_LOG"
    # FAKE_RESTART_FAILS: the restart fails and the running process stays.
    [ -n "${FAKE_RESTART_FAILS:-}" ] && exit 1
    running=$(cat "$S/running" 2>/dev/null)
    : > "$S/sigterm-$running"
    # A build named by FAKE_RESTART_DRAINS drains its turns on SIGTERM, and the
    # stop -- so the restart -- waits until kill -9 reaches it, or until its
    # drain ends by itself after FAKE_DRAIN_SECS. The 120 s cap only keeps a
    # script that never forces it from hanging the harness; every case that
    # drains sets a grace far below it.
    if [ -n "${FAKE_RESTART_DRAINS:-}" ] && [ "$running" = "$FAKE_RESTART_DRAINS" ]; then
      until [ -e "$S/killed" ]; do
        if [ -n "${FAKE_DRAIN_SECS:-}" ] && [ "$SECONDS" -ge "$FAKE_DRAIN_SECS" ]; then
          echo "the drain finished by itself after ${SECONDS}s" >> "$FAKE_LOG"; break
        fi
        if [ "$SECONDS" -ge 120 ]; then echo "systemctl restart waited out the whole drain" >> "$FAKE_LOG"; break; fi
        /bin/sleep 0.05
      done
      rm -f "$S/killed"
    fi
    rm -f "$S/sigterm-$running"
    start_service ;;
  start)
    shift
    for u in "$@"; do
      case "$u" in subrouter.service) start_service ;; subrouter.socket) echo active > "$S/subrouter.socket" ;; esac
    done ;;
  stop)
    kill_fg stop
    [ -n "${FAKE_STOP_STUCK:-}" ] && exit 1
    shift
    for u in "$@"; do echo inactive > "$S/$u"; done
    : > "$S/running" ;;
esac
exit 0'
}

# BOX_WRAP: a command the script runs under, given the script's argv.
# BOX_BIN_ARG: the script's third argument, the build to install.
run_box() { # extra env assignments...
  local sha; sha=$(shasum -a 256 "$NEWBIN" | awk '{print $1}')
  run_sandboxed PATH="$C/bin:/usr/bin:/bin:/usr/sbin:/sbin" HOME="$C/home" TMPDIR="$C/tmp" \
    CODEX_HOME=/leaked/codex-home SUBROUTER_CODEX_SERVER=leaked \
    FAKE_LOG="$C/log" FAKE_SYSTEMD="$C/systemd" FAKE_RUNNING="$C/systemd/running" \
    FAKE_DAEMON="$C/systemd/subrouter.service" FAKE_DEST="$DEST" FAKE_NEW_ID=new \
    FAKE_SVC_USER=subrouter FAKE_SVC_HOME="$R/var/lib/subrouter" \
    FAKE_ADMIN=admin-tok FAKE_PROXY=proxy-tok FAKE_IMPORT=import-tok \
    DEPLOY_BOX_TEST_ROOT="$R" READY_WAIT_SECS=3 "$@" \
    -- ${BOX_WRAP:+"$BOX_WRAP"} "$BASH5" "$BOX_SH" "$sha" "${BOX_ADMIN_ARG-admin-tok}" ${BOX_BIN_ARG:+"$BOX_BIN_ARG"}
}

SVC_ENV_RE="sudo_user=subrouter HOME=[^ ]*/root/var/lib/subrouter STATE=[^ ]*/root/var/lib/subrouter CODEX_HOME=<unset>"

echo "== deploy-box.sh"

if begin box-default-ok; then
  box_setup pre post; run_box
  expect_rc 0; expect_out "DEPLOY OK"; expect_build "$DEST" new
  # The gate ran with the NEW binary, as the service user, with the service's
  # HOME and state dir, and without the operator's CODEX_HOME.
  expect_log "exec build=new bin=[^ ]*/usr/local/bin/subrouter args=codex isolation-check --json $SVC_ENV_RE"
  # The default path never stops the socket and never attests.
  expect_no_log "systemctl stop"; expect_no_log "args=codex attest-legacy"
  expect_log "systemctl restart subrouter.service"
  finish
fi

# "-" reads the admin token from the env file, like the proxy and import
# tokens, so it is never in argv (visible in `ps`) for the whole deploy
# (PATCHES §43.2). The admin checks then pass with the file's token.
if begin box-admin-token-from-env-file; then
  box_setup pre post; echo "SUBROUTER_ADMIN_TOKEN=admin-tok" >> "$R/etc/default/subrouter"
  BOX_ADMIN_ARG=- run_box
  expect_rc 0; expect_out "DEPLOY OK"; expect_build "$DEST" new
  expect_out "admin token: from "; expect_out "/etc/default/subrouter (SUBROUTER_ADMIN_TOKEN)"
  finish
fi

# "-" with no SUBROUTER_ADMIN_TOKEN in the file stops before anything changes.
if begin box-admin-token-dash-without-one-refused; then
  box_setup pre post
  BOX_ADMIN_ARG=- run_box
  expect_rc 1; expect_out "no admin token"; expect_out "Nothing was changed"
  expect_build "$DEST" old; expect_no_log "systemctl restart"
  finish
fi

# --public-admin (PATCHES §43.1). Whether it is on is what the NEW daemon
# logged (PUBLIC ADMIN IS ON), wherever it was set. Off, the private-only
# routes must be 404 on the public listener exactly as before: a daemon that
# mounts them without saying so rolls back. On, the four it mounts must refuse
# the static admin and import tokens, and the rest must still be 404. The flag
# form in SUBROUTER_EXTRA_ARGS is refused before anything changes.
if begin box-public-admin-off-still-demands-404; then
  box_setup pre post; run_box FAKE_PUBLIC_ADMIN=1 FAKE_BAD=public-admin-silent
  expect_rc 1; expect_out "ROLLING BACK"
  expect_out "/_subrouter/sessions is reachable on the public listener (got 401)"
  expect_no_out "public admin: ON"; expect_build "$DEST" old
  finish
fi

if begin box-public-admin-env-ok; then
  box_setup pre post; echo "SUBROUTER_PUBLIC_ADMIN=1" >> "$R/etc/default/subrouter"
  run_box FAKE_PUBLIC_ADMIN=1
  expect_rc 0; expect_out "DEPLOY OK"; expect_out "public admin: ON (pid 4242 logged PUBLIC ADMIN IS ON)"
  expect_out "/_subrouter/sessions -> 401"; expect_out "/internal/v1/session-leases -> 404"
  expect_out "/_subrouter/transcripts -> 404"
  expect_out "/_subrouter/account-import -> 401"; expect_out "/_subrouter/account-import + IMPORT token -> 401"
  expect_out "/_subrouter/claude-reset -> 401"; expect_out "/_subrouter/claude-reset (no token) -> 401"
  expect_no_out "WARNING"; expect_log "sudo journalctl _PID=4242 --since @[0-9]+ "
  expect_build "$DEST" new
  finish
fi

# The box scripts run under pipefail, where `producer | grep -q` (any flag
# spelling with q) reports a match as a failure if the producer is still
# writing (SIGPIPE). None may come back (PATCHES §43.3).
if begin box-scripts-never-pipe-into-grep-q; then
  for f in "$BOX_SH" "$BOOT_SH" "$PUSH_SH"; do
    if grep -nE '\|[[:space:]]*grep[[:space:]]+-[A-Za-z]*q' "$f" | grep -vE '^[0-9]+:[[:space:]]*#' > "$C/grepq"; then
      bad "$(basename "$f") pipes into grep -q: $(head -1 "$C/grepq")"
    else
      ok
    fi
  done
  finish
fi

# By step 8 the new daemon has logged far more than the line step 8 looks for.
# Read through `grep -q` under pipefail, journalctl died of SIGPIPE and step 8
# called public admin off (2026-10-01, a live box deploy).
if begin box-public-admin-long-journal; then
  box_setup pre post; echo "SUBROUTER_PUBLIC_ADMIN=1" >> "$R/etc/default/subrouter"
  run_box FAKE_PUBLIC_ADMIN=1 FAKE_JOURNAL_TAIL=1
  expect_rc 0; expect_out "DEPLOY OK"; expect_out "public admin: ON (pid 4242 logged PUBLIC ADMIN IS ON)"
  expect_no_out "WARNING"
  finish
fi

# Many listeners after the public one: the public surface is still checked,
# never skipped as "no public listener configured".
if begin box-public-listener-among-many; then
  box_setup pre post
  run_box FAKE_SS_TAIL=1
  expect_rc 0; expect_out "DEPLOY OK"; expect_out "usage-status + PROXY token -> 200"
  expect_no_out "no public listener configured"
  finish
fi

# The public port is the one the env file tells the daemon to bind. A variable
# from the deploying shell, such as bootstrap.sh's SUBROUTER_PUBLIC_PORT=off,
# never moves or skips step 8.
if begin box-public-port-variable-ignored; then
  for v in off 31417; do
    box_setup pre post; run_box SUBROUTER_PUBLIC_PORT=$v FAKE_BAD=public-leases
    expect_rc 1; expect_out "/internal/v1/session-leases is reachable on the public listener (got 401)"
    expect_no_out "skipping"; expect_build "$DEST" old
  done
  finish
fi

if begin box-configured-public-listener-down-rolls-back; then
  box_setup pre post; run_box FAKE_BAD=public-unbound
  expect_rc 1; expect_out "!! ROLLING BACK: nothing listens on :31416, where SUBROUTER_EXTRA_ARGS in"
  expect_no_out "skipping"; expect_build "$DEST" old
  finish
fi

# With no --public-addr in the env file and nothing on :31416, the box has no
# public listener, and step 8 has nothing to check.
if begin box-private-only-skips-the-public-checks; then
  box_setup pre post; sed -i.orig '/^SUBROUTER_EXTRA_ARGS=/d' "$R/etc/default/subrouter"
  run_box FAKE_NO_PUBLIC=1
  expect_rc 0; expect_out "no public listener configured; skipping (private-only deployment)"
  expect_out "DEPLOY OK"; expect_no_log "curl https://"; expect_build "$DEST" new
  finish
fi

# Set in a systemd drop-in, not the env file: the daemon says it is on, and
# that is what step 8 believes.
if begin box-public-admin-dropin-ok; then
  box_setup pre post
  run_box FAKE_PUBLIC_ADMIN=1 FAKE_UNIT_ENV=SUBROUTER_PUBLIC_ADMIN=1
  expect_rc 0; expect_out "DEPLOY OK"; expect_out "public admin: ON"; expect_no_out "ROLLING BACK"
  expect_build "$DEST" new
  finish
fi

# On, from a build before PATCHES §46.9: the Claude reset claim is still
# private there. A warning, not a rollback (PATCHES §46.9).
if begin box-public-admin-reset-unmounted-warns; then
  box_setup pre post; echo "SUBROUTER_PUBLIC_ADMIN=1" >> "$R/etc/default/subrouter"
  run_box FAKE_PUBLIC_ADMIN=1 FAKE_BAD=public-reset-unmounted
  expect_rc 0; expect_out "DEPLOY OK"; expect_no_out "ROLLING BACK"
  expect_out "WARNING: /_subrouter/claude-reset is not mounted on the public listener"
  expect_out "/_subrouter/sessions -> 401"
  finish
fi

# Asked for, but the daemon never said so (a build without it): every marked
# route 404s, which is right for that build, and the run says it was not on.
if begin box-public-admin-asked-not-confirmed-warns; then
  box_setup pre post; echo "SUBROUTER_PUBLIC_ADMIN=1" >> "$R/etc/default/subrouter"
  run_box
  expect_rc 0; expect_out "DEPLOY OK"; expect_no_out "public admin: ON"
  expect_out "did not log PUBLIC ADMIN IS ON"; expect_out "/_subrouter/sessions -> 404"
  finish
fi

# On by the daemon's own word, yet nothing mounted: a warning, not a rollback.
if begin box-public-admin-on-but-unmounted-warns; then
  box_setup pre post; echo "SUBROUTER_PUBLIC_ADMIN=1" >> "$R/etc/default/subrouter"
  run_box FAKE_PUBLIC_ADMIN=1 FAKE_BAD=public-admin-unmounted
  expect_rc 0; expect_out "DEPLOY OK"; expect_out "public admin: ON"
  expect_out "WARNING: /_subrouter/sessions is not mounted on the public listener"
  finish
fi

if begin box-public-admin-flag-refused; then
  box_setup pre post
  sed -i.orig 's|^SUBROUTER_EXTRA_ARGS=.*|SUBROUTER_EXTRA_ARGS="--public-addr 0.0.0.0:31416 --public-admin"|' "$R/etc/default/subrouter"
  run_box FAKE_PUBLIC_ADMIN=1
  expect_rc 1; expect_out "DEPLOY FAILED"; expect_out "carries --public-admin"
  expect_out "set SUBROUTER_PUBLIC_ADMIN=1"; expect_out "Nothing was changed"
  expect_no_log "systemctl (restart|stop|start)"; expect_build "$DEST" old
  finish
fi

if begin box-public-admin-flag-false-refused; then
  box_setup pre post
  sed -i.orig 's|^SUBROUTER_EXTRA_ARGS=.*|SUBROUTER_EXTRA_ARGS="--public-addr 0.0.0.0:31416 --public-admin=false"|' "$R/etc/default/subrouter"
  run_box
  expect_rc 1; expect_out "carries --public-admin"
  expect_no_log "systemctl (restart|stop|start)"; expect_build "$DEST" old
  finish
fi

if begin box-public-admin-static-token-rolls-back; then
  box_setup pre post; echo "SUBROUTER_PUBLIC_ADMIN=1" >> "$R/etc/default/subrouter"
  run_box FAKE_PUBLIC_ADMIN=1 FAKE_BAD=public-admin-static
  expect_rc 1; expect_out "ROLLING BACK"
  expect_out "answered 200 to the STATIC admin token on the public listener"
  expect_build "$DEST" old
  finish
fi

if begin box-public-admin-import-token-rolls-back; then
  box_setup pre post; echo "SUBROUTER_PUBLIC_ADMIN=1" >> "$R/etc/default/subrouter"
  run_box FAKE_PUBLIC_ADMIN=1 FAKE_BAD=public-import-static
  expect_rc 1; expect_out "ROLLING BACK"
  expect_out "/_subrouter/account-import answered 405 to the static IMPORT token"
  expect_build "$DEST" old
  finish
fi

# Transcripts stay private with public admin on (PATCHES §43.2): a build
# that still mounts them on the public listener rolls back.
if begin box-public-admin-transcripts-mounted-rolls-back; then
  box_setup pre post; echo "SUBROUTER_PUBLIC_ADMIN=1" >> "$R/etc/default/subrouter"
  run_box FAKE_PUBLIC_ADMIN=1 FAKE_BAD=public-transcripts
  expect_rc 1; expect_out "ROLLING BACK"
  expect_out "/_subrouter/transcripts is reachable on the public listener (got 401)"
  expect_build "$DEST" old
  finish
fi

if begin box-public-admin-unmarked-route-rolls-back; then
  box_setup pre post; echo "SUBROUTER_PUBLIC_ADMIN=1" >> "$R/etc/default/subrouter"
  run_box FAKE_PUBLIC_ADMIN=1 FAKE_BAD=public-leases
  expect_rc 1; expect_out "ROLLING BACK"
  expect_out "/internal/v1/session-leases is reachable on the public listener (got 401)"
  expect_build "$DEST" old
  finish
fi

if begin box-ready-startup-503-accepted; then
  box_setup pre post; run_box FAKE_READY_PUBLIC=startup FAKE_READY_PRIVATE=startup
  expect_rc 0; expect_out "DEPLOY OK"; expect_no_out "ROLLING BACK"
  expect_out "503 startup_ready:false"
  expect_out "WARNING: /_subrouter/ready is still not ok"
  expect_build "$DEST" new
  finish
fi

if begin box-ready-startup-then-flips; then
  box_setup pre post; run_box FAKE_READY_PUBLIC=startup FAKE_READY_PRIVATE=ok
  expect_rc 0; expect_out "/_subrouter/ready: ok"; expect_no_out "WARNING"
  finish
fi

if begin box-ready-draining-rolls-back; then
  box_setup post post; run_box FAKE_READY_PUBLIC=draining
  expect_rc 1; expect_out "ROLLING BACK"; expect_out "draining"
  expect_build "$DEST" old
  finish
fi

if begin box-ready-missing-rolls-back; then
  box_setup post post; run_box FAKE_READY_PUBLIC=missing
  expect_rc 1; expect_out "ROLLING BACK"; expect_out "/_subrouter/ready is not served publicly (got 404)"
  expect_build "$DEST" old
  finish
fi

if begin box-ready-draining-with-startup-key-rolls-back; then
  # Not a body the handler emits today; pins that a DRAINING 503 rolls back even
  # if a future body also carries startup_ready:false.
  box_setup post post; run_box FAKE_READY_PUBLIC=draining-startup
  expect_rc 1; expect_out "ROLLING BACK"; expect_build "$DEST" old
  finish
fi

if begin box-isolation-incomplete; then
  box_setup pre post; echo 8 > "$R/var/lib/subrouter/fake-codex-needing-migration"
  run_box
  expect_rc 3; expect_out "DEPLOY INCOMPLETE"; expect_no_out "DEPLOY OK"; expect_no_out "ROLLING BACK"
  expect_out "8 Codex account(s) fail the credential-isolation gate"
  expect_out "codex attest-legacy"; expect_out "migrate-isolation"
  expect_build "$DEST" new
  finish
fi

if begin box-isolation-check-cannot-run; then
  box_setup pre post; run_box FAKE_ISO_BROKEN=1
  expect_rc 3; expect_out "DEPLOY INCOMPLETE"; expect_out "the isolation check could not run"
  expect_out "permission denied"; expect_build "$DEST" new
  finish
fi

if begin box-unit-env-honored; then
  # systemd resolves the service's environment from the unit's Environment= and
  # its EnvironmentFile=, the file winning. The gate must inspect THAT store and
  # THAT CODEX_HOME -- here the state dir comes from the env file and CODEX_HOME
  # from the unit, while this shell exports different ones.
  box_setup pre post
  mkdir -p "$R/alt-state"; echo 2 > "$R/alt-state/fake-codex-needing-migration"
  sed -i.orig "s|^SUBROUTER_STATE_DIR=.*|SUBROUTER_STATE_DIR=$R/alt-state|" "$R/etc/default/subrouter"
  run_box FAKE_UNIT_ENV="CODEX_HOME=$R/unit-codex SUBROUTER_STATE_DIR=$R/var/lib/subrouter"
  expect_rc 3; expect_out "2 Codex account(s) fail the credential-isolation gate"
  expect_log "args=codex isolation-check --json sudo_user=subrouter HOME=[^ ]*/root/var/lib/subrouter STATE=[^ ]*/root/alt-state CODEX_HOME=[^ ]*/root/unit-codex"
  expect_no_log "CODEX_HOME=/leaked"
  expect_out "$R/unit-codex/auth.json"
  finish
fi

if begin box-rollback-refused-pre-isolation; then
  # A record the new build logged in isolated: the old binary would damage it.
  box_setup pre post
  seed_record "$R/var/lib/subrouter" a@x isolated-server-login
  seed_record "$R/var/lib/subrouter" b@x ""
  run_box FAKE_BAD=few-accounts
  expect_rc 1; expect_out "NOT ROLLING BACK"; expect_out "predates Codex credential isolation"
  expect_out "1 Codex record(s) in $R/var/lib/subrouter/codex/accounts carry oauthCredentialOrigin"
  expect_out "ALLOW_PRE_ISOLATION_ROLLBACK=1"; expect_no_out "DEPLOY OK"
  expect_build "$DEST" new
  finish
fi

if begin box-rollback-allowed-by-override; then
  # Migrated records present, so only the override lets it through.
  box_setup pre post; seed_record "$R/var/lib/subrouter" a@x isolated-server-login
  run_box FAKE_BAD=few-accounts ALLOW_PRE_ISOLATION_ROLLBACK=1
  expect_rc 1; expect_out "!! ROLLING BACK"; expect_no_out "NOT ROLLING BACK"
  expect_build "$DEST" old
  finish
fi

if begin box-rollback-refused-service-down; then
  # Compact JSON: the origin match tolerates no whitespace around the colon.
  box_setup pre post; seed_record "$R/var/lib/subrouter" a@x server-attested-transfer compact
  run_box FAKE_CRASH_BUILD=new
  expect_rc 1; expect_out "NOT ROLLING BACK: service is not active"; expect_build "$DEST" new
  expect_out "1 Codex record(s) in"
  finish
fi

if begin box-rollback-no-migrated-records; then
  # The defect this pins: a genuinely broken FIRST post-isolation build, no
  # record migrated, no override. Nothing crossed the boundary, so the backup
  # is restored and the service comes back on it.
  box_setup pre post; run_box FAKE_CRASH_BUILD=new
  expect_rc 1; expect_out "!! ROLLING BACK: service is not active"; expect_no_out "NOT ROLLING BACK"
  expect_out "no Codex record in $R/var/lib/subrouter/codex/accounts carries a migrated origin; restoring it is safe"
  expect_build "$DEST" old; expect_out "service=active"
  finish
fi

if begin box-rollback-interactive-only-records; then
  # A store that exists and holds records, none of them migrated.
  box_setup pre post
  seed_record "$R/var/lib/subrouter" a@x interactive-import
  seed_record "$R/var/lib/subrouter" b@x ""
  seed_record "$R/var/lib/subrouter" c@x interactive-import compact
  run_box FAKE_BAD=few-accounts
  expect_rc 1; expect_out "!! ROLLING BACK: only 3 accounts visible"; expect_no_out "NOT ROLLING BACK"
  expect_out "no Codex record in"; expect_build "$DEST" old
  finish
fi

if begin box-rollback-refused-store-unreadable; then
  # The store exists but cannot be listed: nothing proves it holds no migrated
  # record, so the rollback is refused and the message says why.
  box_setup pre post; seed_record "$R/var/lib/subrouter" a@x interactive-import
  if make_unreadable "$R/var/lib/subrouter/codex/accounts"; then
    run_box FAKE_BAD=few-accounts
    chmod 755 "$R/var/lib/subrouter/codex/accounts"
    expect_rc 1; expect_out "NOT ROLLING BACK: only 3 accounts visible"; expect_no_out "!! ROLLING BACK"
    expect_out "the Codex store could not be read ($R/var/lib/subrouter/codex/accounts exists but cannot be read)"
    expect_build "$DEST" new
  fi
  finish
fi

if begin box-rollback-refused-unit-state-dir; then
  # The store is the service's state dir from its env file, not HOME: the
  # migrated record lives only there.
  box_setup pre post
  mkdir -p "$R/alt-state"; echo 0 > "$R/alt-state/fake-codex-needing-migration"
  sed -i.orig "s|^SUBROUTER_STATE_DIR=.*|SUBROUTER_STATE_DIR=$R/alt-state|" "$R/etc/default/subrouter"
  seed_record "$R/alt-state" a@x isolated-server-login
  run_box FAKE_BAD=few-accounts
  expect_rc 1; expect_out "NOT ROLLING BACK"; expect_out "1 Codex record(s) in $R/alt-state/codex/accounts"
  expect_build "$DEST" new
  finish
fi

if begin box-rollback-post-to-post-unaffected; then
  box_setup post post; run_box FAKE_BAD=few-accounts
  expect_rc 1; expect_out "!! ROLLING BACK"; expect_build "$DEST" old
  finish
fi

if begin box-restart-bounded; then
  # Step 4 forces the old build when it is still draining at the end of its
  # wall-clock grace.
  box_setup post post; run_box FAKE_RESTART_DRAINS=old DRAIN_GRACE_SECONDS=1
  expect_rc 0; expect_out "DEPLOY OK"; expect_build "$DEST" new
  expect_out "old pid 4242 still draining after 1s (DRAIN_GRACE_SECONDS) -- forcing it"
  expect_log "kill -9 4242"; expect_no_log "waited out the whole drain"
  finish
fi

if begin box-rollback-restart-bounded; then
  # PATCHES §38: the build being rolled back drains its streams and turns on
  # SIGTERM, possibly the very ones it is rolled back for. rollback()'s restart
  # is bounded like step 4's; a bare one waited out the whole drain, up to
  # TimeoutStopSec=10min, with :31416 closed.
  box_setup post post; run_box FAKE_BAD=few-accounts FAKE_RESTART_DRAINS=new DRAIN_GRACE_SECONDS=1
  expect_rc 1; expect_out "!! ROLLING BACK: only 3 accounts visible"; expect_build "$DEST" old
  expect_out "old pid 4242 still draining after 1s (ROLLBACK_DRAIN_GRACE_SECONDS) -- forcing it"
  expect_log "kill -9 4242"; expect_no_log "waited out the whole drain"
  expect_out "service=active"
  finish
fi

# The seconds between the restart and the kill -9, from the stubs' timestamps.
restart_to_kill() {
  local began killed
  began=$(sed -n 's/^restart began at=//p' "$C/log" | head -1)
  killed=$(sed -n 's/^killed at=//p' "$C/log" | head -1)
  [ -n "$began" ] && [ -n "$killed" ] && echo $((killed - began))
}

if begin box-drain-grace-is-wall-clock; then
  # PATCHES §38.1. The grace is seconds on the clock, not a count of health
  # probes: a drain that ends inside it is never forced. The 25 probes the
  # grace used to be took 2-3 s against these stubs, which answer at once (on
  # the box each hung up to 2 s on the queued socket): far short of this 10 s
  # drain, which they cut.
  box_setup post post; run_box FAKE_RESTART_DRAINS=old FAKE_DRAIN_SECS=10 DRAIN_GRACE_SECONDS=20
  expect_rc 0; expect_out "DEPLOY OK"; expect_build "$DEST" new
  expect_log "the drain finished by itself"; expect_no_log "kill -9"; expect_no_out "forcing it"
  expect_out "the old process may drain for 20s before it is forced (DRAIN_GRACE_SECONDS=20)"
  finish
fi

if begin box-drain-grace-forces-at-the-deadline; then
  # A drain that outlasts the grace is forced -- and not before the grace is up.
  box_setup post post; run_box FAKE_RESTART_DRAINS=old DRAIN_GRACE_SECONDS=3
  expect_rc 0; expect_out "DEPLOY OK"; expect_log "kill -9 4242"
  expect_out "still draining after 3s (DRAIN_GRACE_SECONDS) -- forcing it"
  waited=$(restart_to_kill)
  [ -n "$waited" ] && [ "$waited" -ge 2 ] && [ "$waited" -le 6 ] && ok ||
    bad "forced ${waited:-?}s after the restart, want the 3 s grace (2-6 at date's resolution and a 1 s poll)"
  finish
fi

if begin box-drain-grace-default-is-the-ceiling; then
  # Unset, the grace is the drain ceiling: the daemon's default
  # --shutdown-timeout (10m), the unit's TimeoutStopSec being no shorter.
  box_setup post post; run_box FAKE_UNIT_STOP=10min
  expect_rc 0; expect_out "may drain for 600s before it is forced (DRAIN_GRACE_SECONDS unset: the drain ceiling, the daemon's default --shutdown-timeout, 10m)"
  finish
fi

if begin box-drain-grace-ceiling-is-the-unit-timeout; then
  # systemd kills the old process at TimeoutStopSec whatever the daemon's own
  # timeout, so a shorter unit timeout is the ceiling.
  box_setup post post
  sed -i.orig 's|^SUBROUTER_EXTRA_ARGS=.*|SUBROUTER_EXTRA_ARGS="--public-addr 0.0.0.0:31416 --shutdown-timeout=20m"|' "$R/etc/default/subrouter"
  run_box "FAKE_UNIT_STOP=1min 30s"
  expect_rc 0; expect_out "may drain for 90s before it is forced (DRAIN_GRACE_SECONDS unset: the drain ceiling, the unit's TimeoutStopSec, 1min 30s)"
  finish
fi

if begin box-drain-grace-ceiling-is-the-daemon-timeout; then
  # The daemon exits at its own --shutdown-timeout, so a shorter one is the
  # ceiling; a unit timeout systemd prints as infinity sets none.
  box_setup post post
  sed -i.orig 's|^SUBROUTER_EXTRA_ARGS=.*|SUBROUTER_EXTRA_ARGS="--shutdown-timeout 2m0s --public-addr 0.0.0.0:31416"|' "$R/etc/default/subrouter"
  run_box FAKE_UNIT_STOP=infinity
  expect_rc 0; expect_out "may drain for 120s before it is forced (DRAIN_GRACE_SECONDS unset: the drain ceiling, the daemon's --shutdown-timeout 2m0s)"
  finish
fi

if begin box-drain-grace-clamped-to-the-ceiling; then
  # Past the ceiling the old process is gone anyway: the grace is the ceiling,
  # and the output says so.
  box_setup post post; run_box DRAIN_GRACE_SECONDS=601
  expect_rc 0; expect_out "may drain for 600s before it is forced (DRAIN_GRACE_SECONDS=601 is past the drain ceiling"
  finish
fi

if begin box-drain-grace-overflow-clamped; then
  # A number too long for shell arithmetic must not wrap into a short grace:
  # 2^64 + 5 is 5 to bash.
  box_setup post post; run_box DRAIN_GRACE_SECONDS=18446744073709551621
  expect_rc 0; expect_out "may drain for 600s before it is forced (DRAIN_GRACE_SECONDS=18446744073709551621 is past the drain ceiling"
  finish
fi

if begin box-drain-grace-not-a-number-refused; then
  # Refused before anything changes: no backup, no stage, no restart.
  box_setup post post; run_box DRAIN_GRACE_SECONDS=10m
  expect_rc 1; expect_out "DRAIN_GRACE_SECONDS must be a whole number of seconds, got '10m'. Nothing was changed."
  expect_build "$DEST" old; expect_no_log "systemctl (restart|stop|start)"
  if any_exists "$R/usr/local/bin/"*bak-*; then bad "a backup was taken before the refusal"; else ok; fi
  finish
fi

if begin box-drains-over-the-socket-first; then
  # PATCHES §38.1. The running build serves a drain socket: the restart drains
  # it there first, as root and with no token, and the new process starts
  # undrained, so nothing is resumed.
  box_setup post post; run_box FAKE_DRAIN_SOCKET="old new"
  expect_rc 0; expect_out "DEPLOY OK"; expect_build "$DEST" new
  expect_log "sudo curl -s -m 5 --unix-socket [^ ]*/root/var/lib/subrouter/drain.sock -X POST"
  expect_log "curl unix [^ ]*/root/var/lib/subrouter/drain.sock POST /_subrouter/drain auth=none"
  expect_order "curl unix .* POST /_subrouter/drain auth" "systemctl restart subrouter.service"
  expect_out "drained the running build over $R/var/lib/subrouter/drain.sock (no token): new sessions get 503; 0 websocket turn(s) in flight"
  expect_no_log "POST /_subrouter/resume"; expect_no_out "WARNING: the restart did not replace"
  finish
fi

if begin box-drain-socket-reports-the-drain; then
  # The old process answers its drain socket through its SIGTERM drain, so the
  # wait reports what it is waiting for, and the force says what it cut. A 6 s
  # grace leaves a report 5 s to land: with 3 s, a loaded machine skipped it.
  box_setup post post
  run_box FAKE_DRAIN_SOCKET="old new" FAKE_RESTART_DRAINS=old FAKE_TURNS=2 DRAIN_GRACE_SECONDS=6 DRAIN_REPORT_SECONDS=1
  expect_rc 0; expect_out "DEPLOY OK"
  expect_out "old pid 4242 draining, "; expect_out "of 6s: 2 websocket turn(s) in flight, 3 proxy request(s) open"
  expect_out "old pid 4242 still draining after 6s (DRAIN_GRACE_SECONDS) with 2 websocket turn(s) in flight"
  expect_log "curl unix .* GET /_subrouter/drain-status"
  finish
fi

if begin box-no-drain-socket-falls-back; then
  # A running build from before the drain socket: restarted as before.
  box_setup post post; run_box FAKE_DRAIN_SOCKET="new"
  expect_rc 0; expect_out "DEPLOY OK"; expect_build "$DEST" new
  expect_out "no drain socket answered at $R/var/lib/subrouter/drain.sock: the running build predates it (or serves none); restarting without a pre-drain"
  expect_log "systemctl restart subrouter.service"
  finish
fi

if begin box-drain-socket-off; then
  # SUBROUTER_DRAIN_SOCKET=off in the service's env file: nothing is asked, and
  # the output says the socket is off rather than that the build is old.
  box_setup post post; echo "SUBROUTER_DRAIN_SOCKET=off" >> "$R/etc/default/subrouter"
  run_box FAKE_DRAIN_SOCKET="old new"
  expect_rc 0; expect_out "DEPLOY OK"; expect_no_log "curl unix"
  expect_out "no pre-drain: SUBROUTER_DRAIN_SOCKET=off in the service's environment turns the drain socket off"
  expect_no_out "predates it"
  finish
fi

if begin box-drain-socket-off-any-spelling; then
  # The daemon lowercases and trims the value (drainSocketDisabled), so None
  # turns its socket off. The script used to curl a relative path named None,
  # from whatever directory it ran in.
  box_setup post post; echo "SUBROUTER_DRAIN_SOCKET=None" >> "$R/etc/default/subrouter"
  run_box FAKE_DRAIN_SOCKET="old new"
  expect_rc 0; expect_out "DEPLOY OK"; expect_no_log "curl unix"
  expect_out "no pre-drain: SUBROUTER_DRAIN_SOCKET=None in the service's environment turns the drain socket off"
  finish
fi

if begin box-drain-socket-relative-path-ignored; then
  # The daemon refuses a relative path and serves no socket. A relative path
  # handed to `sudo curl --unix-socket` resolves in the operator's directory,
  # where another user could have bound it.
  box_setup post post; echo "SUBROUTER_DRAIN_SOCKET=drain.sock" >> "$R/etc/default/subrouter"
  run_box FAKE_DRAIN_SOCKET="old new"
  expect_rc 0; expect_out "DEPLOY OK"; expect_no_log "curl unix"
  expect_out "no pre-drain: SUBROUTER_DRAIN_SOCKET=drain.sock in the service's environment is not an absolute path, so the daemon serves no drain socket"
  finish
fi

if begin box-failed-restart-resumes-the-drain; then
  # The restart failed and the drained process is still the one running. It
  # must not be left refusing every new session until the next restart.
  box_setup post post; run_box FAKE_DRAIN_SOCKET="old new" FAKE_RESTART_FAILS=1
  expect_out "WARNING: the restart did not replace the drained process; resumed it over"
  expect_order "curl unix .* POST /_subrouter/drain auth" "curl unix .* POST /_subrouter/resume"
  [ -e "$C/systemd/drained-old" ] && bad "the old process was left drained" || ok
  finish
fi

if begin box-operator-drain-is-not-lifted; then
  # An operator drained the box (sudo subrouter drain) before deploying. The
  # restart fails and the old process survives: the script must leave that
  # drain alone. It used to resume whatever it found draining, putting the box
  # back in rotation against the operator's intent.
  box_setup post post; : > "$C/systemd/drained-old"
  run_box FAKE_DRAIN_SOCKET="old new" FAKE_RESTART_FAILS=1
  expect_out "the running build was already draining, and not by this script (an operator's drain?): left as it is"
  expect_no_log "POST /_subrouter/resume"; expect_no_log "POST /_subrouter/drain auth"
  [ -e "$C/systemd/drained-old" ] && ok || bad "the operator's drain was lifted"
  expect_no_out "resumed it over"
  finish
fi

if begin box-drain-report-seconds-leading-zero; then
  # 08 is 8 seconds, not an octal error. The script used to validate it with a
  # pattern and then do $((started + every)): "08: value too great for base"
  # aborted restart_bounded after the pre-drain and before the restart, and the
  # deploy went on to verify the old, drained process.
  box_setup post post
  run_box FAKE_DRAIN_SOCKET="old new" FAKE_RESTART_DRAINS=old DRAIN_GRACE_SECONDS=2 DRAIN_REPORT_SECONDS=08
  expect_rc 0; expect_out "DEPLOY OK"; expect_no_out "value too great"
  expect_log "systemctl restart subrouter.service"; expect_build "$DEST" new
  [ "$(cat "$C/systemd/running")" = new ] && ok || bad "running: $(cat "$C/systemd/running"), want new: the drained old process was left serving"
  expect_log "kill -9 4242"
  finish
fi

if begin box-drain-report-seconds-not-a-number-refused; then
  # Refused before anything changes, as DRAIN_GRACE_SECONDS is.
  box_setup post post; run_box DRAIN_REPORT_SECONDS=30s
  expect_rc 1; expect_out "DRAIN_REPORT_SECONDS must be a whole number of seconds, got '30s'. Nothing was changed."
  expect_build "$DEST" old; expect_no_log "systemctl (restart|stop|start)"; expect_no_log "curl unix"
  if any_exists "$R/usr/local/bin/"*bak-*; then bad "a backup was taken before the refusal"; else ok; fi
  finish
fi

if begin box-ready-wait-not-a-number-refused; then
  box_setup post post; run_box READY_WAIT_SECS=abc
  expect_rc 1; expect_out "READY_WAIT_SECS must be a whole number of seconds, got 'abc'. Nothing was changed."
  expect_build "$DEST" old; expect_no_log "systemctl (restart|stop|start)"
  if any_exists "$R/usr/local/bin/"*bak-*; then bad "a backup was taken before the refusal"; else ok; fi
  finish
fi

if begin box-drain-report-hung-socket-does-not-delay-the-force; then
  # The old process's drain socket accepts but stalls. A report's curl waits at
  # most the time left before the deadline, and the force line's at most 1 s,
  # so the kill -9 lands on time. With -m 5 on each it came about 8 s late.
  box_setup post post
  run_box FAKE_DRAIN_SOCKET="old new" FAKE_RESTART_DRAINS=old FAKE_STATUS_HANGS=30 DRAIN_GRACE_SECONDS=3 DRAIN_REPORT_SECONDS=1
  expect_rc 0; expect_out "DEPLOY OK"; expect_log "kill -9 4242"
  expect_log "drain-status hung for 1s \\(curl -m 1\\)"
  waited=$(restart_to_kill)
  [ -n "$waited" ] && [ "$waited" -ge 2 ] && [ "$waited" -le 6 ] && ok ||
    bad "forced ${waited:-?}s after the restart, want the 3 s grace (2-6), not held up by the stalled socket"
  finish
fi

if begin box-drain-grace-fractional-unit-timeout; then
  # systemd prints a fractional TimeoutStopUSec as "1min 30.500000s". It used
  # to fail to parse, so the ceiling silently stayed 600 s while systemd killed
  # at 90.5 s.
  box_setup post post; run_box "FAKE_UNIT_STOP=1min 30.500000s"
  expect_rc 0; expect_out "may drain for 90s before it is forced (DRAIN_GRACE_SECONDS unset: the drain ceiling, the unit's TimeoutStopSec, 1min 30.500000s)"
  finish
fi

if begin box-drain-grace-fractional-daemon-timeout; then
  # Go prints the same as 1m30.5s.
  box_setup post post
  sed -i.orig 's|^SUBROUTER_EXTRA_ARGS=.*|SUBROUTER_EXTRA_ARGS="--public-addr 0.0.0.0:31416 --shutdown-timeout=1m30.5s"|' "$R/etc/default/subrouter"
  run_box FAKE_UNIT_STOP=infinity
  expect_rc 0; expect_out "may drain for 90s before it is forced (DRAIN_GRACE_SECONDS unset: the drain ceiling, the daemon's --shutdown-timeout 1m30.5s)"
  finish
fi

if begin box-drain-grace-sub-second-timeout-is-one-second; then
  # A timeout under a second is 1 s, not 0: a 0 grace would kill -9 the old
  # process as the restart begins.
  box_setup post post
  sed -i.orig 's|^SUBROUTER_EXTRA_ARGS=.*|SUBROUTER_EXTRA_ARGS="--public-addr 0.0.0.0:31416 --shutdown-timeout=500ms"|' "$R/etc/default/subrouter"
  run_box FAKE_UNIT_STOP=infinity
  expect_rc 0; expect_out "may drain for 1s before it is forced (DRAIN_GRACE_SECONDS unset: the drain ceiling, the daemon's --shutdown-timeout 500ms)"
  finish
fi

if begin box-drain-grace-hard-cap; then
  # The ceiling comes from config, and config can make it hours: a 2h
  # --shutdown-timeout under TimeoutStopSec=infinity. The script's own cap,
  # 900 s, holds whatever the config says.
  box_setup post post
  sed -i.orig 's|^SUBROUTER_EXTRA_ARGS=.*|SUBROUTER_EXTRA_ARGS="--public-addr 0.0.0.0:31416 --shutdown-timeout=2h"|' "$R/etc/default/subrouter"
  run_box FAKE_UNIT_STOP=infinity DRAIN_GRACE_SECONDS=5000
  expect_rc 0; expect_out "may drain for 900s before it is forced (DRAIN_GRACE_SECONDS=5000 is past the drain ceiling, this script's hard cap of 900s (the daemon's --shutdown-timeout 2h is longer)"
  finish
fi

if begin box-rollback-grace-defaults-short; then
  # A rollback abandons a build that failed its checks, and every second it
  # drains is a second before the old build serves again: by default it gets
  # 60 s, not the restart's 600.
  box_setup post post; run_box FAKE_BAD=few-accounts
  expect_rc 1; expect_out "!! ROLLING BACK: only 3 accounts visible"
  expect_out "the old process may drain for 600s before it is forced (DRAIN_GRACE_SECONDS unset"
  expect_out "the old process may drain for 60s before it is forced (ROLLBACK_DRAIN_GRACE_SECONDS unset: 60s, or DRAIN_GRACE_SECONDS's grace when that is shorter)"
  finish
fi

if begin box-rollback-grace-forces-the-abandoned-build; then
  # The build being rolled back drains for 12 s. Under a 30 s restart grace
  # and a 2 s rollback grace it is forced at 2 s.
  box_setup post post
  run_box FAKE_BAD=few-accounts FAKE_RESTART_DRAINS=new FAKE_DRAIN_SECS=12 DRAIN_GRACE_SECONDS=30 ROLLBACK_DRAIN_GRACE_SECONDS=2
  expect_rc 1; expect_build "$DEST" old; expect_out "service=active"
  expect_out "old pid 4242 still draining after 2s (ROLLBACK_DRAIN_GRACE_SECONDS) -- forcing it"
  expect_log "kill -9 4242"; expect_no_log "the drain finished by itself"
  finish
fi

if begin box-rollback-grace-not-a-number-refused; then
  box_setup post post; run_box ROLLBACK_DRAIN_GRACE_SECONDS=1m
  expect_rc 1; expect_out "ROLLBACK_DRAIN_GRACE_SECONDS must be a whole number of seconds, got '1m'. Nothing was changed."
  expect_build "$DEST" old; expect_no_log "systemctl (restart|stop|start)"
  finish
fi

if begin box-rollback-drains-the-new-build-over-its-socket; then
  # rollback() restarts through the same function: it drains the build it
  # abandons over that build's own socket.
  box_setup post post; run_box FAKE_BAD=few-accounts FAKE_DRAIN_SOCKET="old new"
  expect_rc 1; expect_out "!! ROLLING BACK: only 3 accounts visible"; expect_build "$DEST" old
  n=$(grep -c "curl unix .* POST /_subrouter/drain auth=none" "$C/log")
  [ "$n" = 2 ] && ok || bad "drained $n time(s) over the socket, want 2: the old build before step 4, the new one before the rollback"
  finish
fi

if begin box-downgrade-refused; then
  box_setup post pre; seed_record "$R/var/lib/subrouter" a@x server-attested-transfer
  run_box
  expect_rc 1; expect_out "refusing to replace a post-isolation build with a pre-isolation one: 1 Codex record(s)"
  expect_build "$DEST" old; expect_no_log "systemctl (restart|stop|start)"
  if any_exists "$R/usr/local/bin/"*bak-*; then bad "a backup was taken before the refusal"; else ok; fi
  finish
fi

if begin box-downgrade-allowed-by-override; then
  box_setup post pre; seed_record "$R/var/lib/subrouter" a@x server-attested-transfer
  run_box ALLOW_PRE_ISOLATION_ROLLBACK=1
  expect_rc 0; expect_out "DEPLOY OK"; expect_out "n/a: this build predates the isolation gate"
  expect_build "$DEST" new
  finish
fi

if begin box-downgrade-no-migrated-records; then
  # Nothing was ever migrated: going back to a pre-isolation build is safe.
  box_setup post pre; seed_record "$R/var/lib/subrouter" a@x interactive-import
  run_box
  expect_rc 0; expect_out "DEPLOY OK"; expect_no_out "refusing to replace"
  expect_out "replacing a post-isolation build with a pre-isolation one: allowed, no Codex record in"
  expect_build "$DEST" new
  finish
fi

if begin box-attest-first; then
  box_setup pre post; echo 8 > "$R/var/lib/subrouter/fake-codex-needing-migration"
  echo '{"passive":"copy"}' > "$R/var/lib/subrouter/.codex/auth.json"
  run_box ATTEST_FIRST=1
  expect_rc 0; expect_out "DEPLOY OK"; expect_build "$DEST" new
  # preflight while the old daemon serves -> stop BOTH units -> attest with the
  # staged NEW binary while nothing runs -> start both.
  expect_log "exec build=new bin=[^ ]*subrouter.stage args=codex isolation-check --json $SVC_ENV_RE daemon=active"
  expect_log "systemctl stop subrouter.service subrouter.socket"
  expect_log "exec build=new bin=[^ ]*subrouter.stage args=codex attest-legacy $SVC_ENV_RE daemon=inactive"
  expect_no_log "args=codex attest-legacy sudo_user.*daemon=active"
  expect_log "systemctl start subrouter.socket subrouter.service"
  expect_order "codex isolation-check" "systemctl stop"
  expect_order "systemctl stop" "args=codex attest-legacy sudo_user"
  expect_order "args=codex attest-legacy sudo_user" "systemctl start"
  # The passive copy was moved aside, not deleted, and the attest covered all 8.
  [ -e "$R/var/lib/subrouter/.codex/auth.json" ] && bad "passive auth.json still in place" || ok
  if any_exists "$R/var/lib/subrouter/.codex/auth.json.pre-isolation-"*; then ok; else bad "no auth.json.pre-isolation-* copy"; fi
  expect_out "Attested 8, skipped 0, failed 0."
  finish
fi

if begin box-attest-first-says-the-grace-does-not-apply; then
  # ATTEST_FIRST stops the old daemon with a fixed 30 s grace. An operator who
  # sets DRAIN_GRACE_SECONDS is told it does not apply here.
  box_setup pre post; echo 8 > "$R/var/lib/subrouter/fake-codex-needing-migration"
  run_box ATTEST_FIRST=1 DRAIN_GRACE_SECONDS=300
  expect_rc 0; expect_out "DEPLOY OK"
  expect_out "DRAIN_GRACE_SECONDS=300 does not apply to this stop: ATTEST_FIRST gives the old daemon a fixed 30 s"
  finish
fi

if begin box-attest-first-rollback-refused; then
  # Once ATTEST_FIRST has attested the store, restoring the pre-isolation backup
  # is exactly the crossing the guard exists for: a failed route check must
  # leave the new build installed, and the old binary must never start again.
  # The fake attest-legacy stamps no record in codex/accounts, so only the
  # "attest ran in this deploy" flag can refuse here.
  box_setup pre post; echo 8 > "$R/var/lib/subrouter/fake-codex-needing-migration"
  run_box ATTEST_FIRST=1 FAKE_BAD=few-accounts
  expect_rc 1; expect_out "Attested 8, skipped 0, failed 0."
  expect_out "NOT ROLLING BACK: only 3 accounts visible"; expect_no_out "!! ROLLING BACK"
  expect_out "codex attest-legacy ran in this deploy (ATTEST_FIRST)"
  expect_build "$DEST" new
  expect_no_log "systemctl restart"
  finish
fi

if begin box-attest-first-preflight-ok-rolls-back; then
  # ATTEST_FIRST whose preflight said ok attests nothing, so a broken build
  # still rolls back like the default path.
  box_setup pre post; run_box ATTEST_FIRST=1 FAKE_BAD=few-accounts
  expect_rc 1; expect_out "nothing to attest"; expect_no_log "args=codex attest-legacy sudo_user"
  expect_out "!! ROLLING BACK: only 3 accounts visible"; expect_no_out "NOT ROLLING BACK"
  expect_build "$DEST" old
  finish
fi

if begin box-attest-first-nothing-to-attest; then
  box_setup pre post; run_box ATTEST_FIRST=1
  expect_rc 0; expect_out "nothing to attest"; expect_out "DEPLOY OK"
  expect_no_log "systemctl stop"; expect_no_log "args=codex attest-legacy"
  expect_log "systemctl restart subrouter.service"
  finish
fi

if begin box-attest-first-preflight-cannot-run; then
  box_setup pre post; echo 8 > "$R/var/lib/subrouter/fake-codex-needing-migration"
  run_box ATTEST_FIRST=1 FAKE_ISO_BROKEN=1
  expect_rc 1; expect_out "did not run"; expect_out "Nothing was stopped or changed"
  expect_no_log "systemctl (stop|restart|start)"; expect_no_log "args=codex attest-legacy"
  expect_build "$DEST" old
  [ -e "$DEST.stage" ] && bad "stage file left behind" || ok
  finish
fi

if begin box-attest-first-stop-stuck; then
  box_setup pre post; echo 8 > "$R/var/lib/subrouter/fake-codex-needing-migration"
  run_box ATTEST_FIRST=1 FAKE_STOP_STUCK=1
  expect_rc 1; expect_out "would not stop"; expect_no_log "args=codex attest-legacy sudo_user"
  expect_build "$DEST" old
  finish
fi

if begin box-attest-first-pre-isolation-candidate; then
  box_setup pre pre; run_box ATTEST_FIRST=1
  expect_rc 1; expect_out "ATTEST_FIRST needs a post-isolation build"
  expect_no_log "systemctl (stop|restart|start)"; expect_build "$DEST" old
  finish
fi

# The private listener the checks probe: SUBROUTER_BOX_ADDR, else the env
# file's SUBROUTER_ADDR, and nothing guessed. Without one, or with one that is
# not host:port, the run stops before the backup.
if begin box-missing-address-refused; then
  box_setup post post; sed -i.orig '/^SUBROUTER_ADDR=/d' "$R/etc/default/subrouter"
  run_box
  expect_rc 1; expect_out "no box address: SUBROUTER_BOX_ADDR is unset"; expect_out "Nothing was changed"
  expect_build "$DEST" old; expect_no_log "systemctl (restart|stop|start)"; expect_no_log "curl http"
  if any_exists "$R/usr/local/bin/"*bak-*; then bad "a backup was taken before the refusal"; else ok; fi
  finish
fi

if begin box-malformed-address-refused; then
  box_setup post post; run_box "SUBROUTER_BOX_ADDR=127.0.0.1:31415/_subrouter"
  expect_rc 1; expect_out "the box address '127.0.0.1:31415/_subrouter'"; expect_out "is not host:port"
  expect_out "Nothing was changed"; expect_build "$DEST" old; expect_no_log "systemctl (restart|stop|start)"
  if any_exists "$R/usr/local/bin/"*bak-*; then bad "a backup was taken before the refusal"; else ok; fi
  finish
fi

# A host is letters, digits, dots, dashes, colons and brackets. A slash would
# move the port into the URL's path, and the checks would probe port 80.
if begin box-address-host-with-a-path-refused; then
  box_setup post post; run_box "SUBROUTER_BOX_ADDR=127.0.0.1/x:31415"
  expect_rc 1; expect_out "the box address '127.0.0.1/x:31415'"; expect_out "is not host:port"
  expect_out "Nothing was changed"; expect_build "$DEST" old; expect_no_log "systemctl (restart|stop|start)"
  if any_exists "$R/usr/local/bin/"*bak-*; then bad "a backup was taken before the refusal"; else ok; fi
  finish
fi

# An IPv6 host is accepted only in brackets.
if begin box-unbracketed-ipv6-address-refused; then
  box_setup post post; run_box "SUBROUTER_BOX_ADDR=2001:db8::1:31415"
  expect_rc 1; expect_out "the box address '2001:db8::1:31415'"; expect_out "is not host:port"
  expect_out "Nothing was changed"; expect_build "$DEST" old; expect_no_log "systemctl (restart|stop|start)"
  finish
fi

if begin box-bracketed-ipv6-address-ok; then
  box_setup post post; run_box "SUBROUTER_BOX_ADDR=[2001:db8::1]:31415"
  expect_rc 0; expect_out "DEPLOY OK"; expect_log "^curl http://\\[2001:db8::1\\]:31415/_subrouter/accounts$"
  finish
fi

if begin box-address-from-the-environment; then
  box_setup post post; run_box SUBROUTER_BOX_ADDR=100.64.0.10:31415
  expect_rc 0; expect_out "DEPLOY OK"; expect_out "private listener: 100.64.0.10:31415"
  expect_log "curl http://100.64.0.10:31415/_subrouter/accounts"; expect_no_log "curl http://127.0.0.1"
  finish
fi

# Step 7's floor is the pool the running build listed before the swap, so a
# pool of any size deploys, an empty one included, and one that shrank, by a
# single account too, rolls back.
if begin box-small-pool-ok; then
  box_setup post post; run_box FAKE_POOL_SIZE=2
  expect_rc 0; expect_out "DEPLOY OK"; expect_out "accounts visible before the swap: 2"
  expect_out "accounts visible: 2   (before the swap: 2)"; expect_no_out "ROLLING BACK"
  finish
fi

if begin box-empty-pool-ok; then
  box_setup post post; run_box FAKE_POOL_SIZE=0
  expect_rc 0; expect_out "DEPLOY OK"; expect_out "accounts visible: 0   (before the swap: 0)"
  finish
fi

# A lost ssh connection kills the foreground's process group. The deploy goes
# on detached, still verifies and still rolls back, and says how it ended in
# its log.
if begin box-lost-connection-deploy-finishes; then
  box_setup post post; run_killable run_box
  expect_killed_and_finished "DEPLOY OK"
  expect_build "$DEST" new
  finish
fi

if begin box-lost-connection-still-rolls-back; then
  box_setup post post; run_killable run_box FAKE_BAD=few-accounts
  [ "$FG_RC" = 137 ] && ok || bad "the foreground exited $FG_RC, want 137 (SIGKILL)"
  [ "$TRANSITION_RC" = 1 ] && ok || bad "the detached run exited '$TRANSITION_RC', want 1"
  grep -Fq "!! ROLLING BACK: only 3 accounts visible" "$TRANSITION_LOG" && ok || bad "the detached run's log lacks the rollback"
  expect_build "$DEST" old
  finish
fi

# push-box.sh's ssh has no tty, so a drop closes the output: the reader is
# gone, and the next write to it raises SIGPIPE.
if begin box-closed-output-deploy-finishes; then
  box_setup post post
  printf '#!/bin/bash\n"$@" | head -2\n' > "$C/reader-gone"; chmod 755 "$C/reader-gone"
  BOX_WRAP="$C/reader-gone" run_box
  await_transitions
  [ "$TRANSITION_RC" = 0 ] && ok || bad "the detached run exited '$TRANSITION_RC', want 0"
  grep -Fq "DEPLOY OK" "$TRANSITION_LOG" && ok || bad "the detached run's log lacks: DEPLOY OK"
  expect_no_out "DEPLOY OK"; expect_build "$DEST" new
  finish
fi

# One deploy at a time. A second one, such as a retry after a dropped
# connection, is refused before step 1 with the running one's log, and the
# first still ends DEPLOY OK. The next deploy after it runs.
if begin box-second-deploy-refused-while-one-runs; then
  box_setup post post
  ( OUT="$C/first.out" run_box FAKE_RESTART_DRAINS=old DRAIN_GRACE_SECONDS=4; echo "$RC" > "$C/first.rc" ) &
  first=$!
  n=0; until grep -q "^restart began" "$C/log" || [ "$n" -ge 400 ]; do /bin/sleep 0.05; n=$((n + 1)); done
  run_box
  expect_rc 1; expect_out "DEPLOY FAILED: another deploy is running (deploy-box.sh, pid "
  expect_out "/.subrouter/transitions/deploy-box-"; expect_out "Nothing was changed."
  expect_no_out "== 1. verify the payload"
  wait "$first"
  [ "$(cat "$C/first.rc")" = 0 ] && grep -Fq "DEPLOY OK" "$C/first.out" && ok || bad "the first deploy did not end DEPLOY OK"
  [ "$(grep -c '^restart began' "$C/log")" = 1 ] && ok || bad "a second restart ran"
  [ "$(grep -Ec '^sudo cp [^ ]*/usr/local/bin/subrouter [^ ]*\.bak-' "$C/log")" = 1 ] && ok || bad "a second backup was taken"
  expect_build "$DEST" new
  make_build "$NEWBIN" post newer; run_box
  expect_rc 0; expect_out "DEPLOY OK"; expect_build "$DEST" newer
  finish
fi

if begin box-refused-while-a-bootstrap-runs; then
  box_setup post post; hold_deploy_lock "bootstrap.sh, pid 4321"
  run_box
  release_lock
  expect_rc 1; expect_out "another deploy is running (bootstrap.sh, pid 4321)"
  expect_build "$DEST" old; expect_no_log "systemctl (restart|stop|start)"
  if any_exists "$R/usr/local/bin/"*bak-*; then bad "a backup was taken before the refusal"; else ok; fi
  finish
fi

# push-box.sh names the build it uploaded to a directory of its own.
if begin box-binary-path-argument; then
  box_setup post post
  mkdir -p "$C/push"; mv "$NEWBIN" "$C/push/subrouter.new"; NEWBIN="$C/push/subrouter.new"
  BOX_BIN_ARG="$NEWBIN" run_box
  expect_rc 0; expect_out "DEPLOY OK"; expect_build "$DEST" new
  [ -e "$NEWBIN" ] && bad "the uploaded build was left behind" || ok
  finish
fi

# ATTEST_FIRST's outage: dropped between its stop and its start, the deploy
# still starts both units.
if begin box-lost-connection-attest-first-starts-the-daemon; then
  box_setup pre post; echo 8 > "$R/var/lib/subrouter/fake-codex-needing-migration"
  run_killable run_box ATTEST_FIRST=1 FAKE_KILL_FG_AT=stop
  expect_killed_and_finished "DEPLOY OK"
  [ "$(cat "$C/systemd/subrouter.service") $(cat "$C/systemd/subrouter.socket")" = "active active" ] && ok ||
    bad "the units were left stopped"
  finish
fi

if begin box-pool-shrank-rolls-back; then
  box_setup post post; run_box FAKE_POOL_SIZE=4 FAKE_BAD=few-accounts
  expect_rc 1; expect_out "!! ROLLING BACK: only 3 accounts visible, 4 before the swap"
  expect_build "$DEST" old
  finish
fi

# --- bootstrap.sh -------------------------------------------------------------
boot_setup() { # installed-era new-era
  R="$C/root"; mkdir -p "$R/usr/local/bin" "$R/etc/default" "$R/run/lock"
  make_build "$R/usr/local/bin/subrouter" "$1" old
  make_build "$C/subrouter-upload" "$2" new
  stub id '[ "${1:-}" = -u ] && { echo 0; exit 0; }; exec /usr/bin/id "$@"'
}
run_boot() {
  run_sandboxed PATH="$C/bin:/usr/bin:/bin:/usr/sbin:/sbin" HOME="$C/home" TMPDIR="$C/tmp" \
    FAKE_LOG="$C/log" BOOTSTRAP_TEST_ROOT="$R" FAKE_TS_UP="$C/ts-up" FAKE_RUNNING="$C/running" \
    FAKE_PIN="$BOOT_PIN" "$@" \
    -- "$BASH5" "$BOOT_SH" "$C/subrouter-upload"
}

# A box bootstrap.sh runs to the end on. Tailscale joins on `up` and then has
# 100.64.0.10, and the daemon answers health once restarted. The upload carries
# the guard (unless "unguarded"); its install-systemd writes the env file from
# the three tokens on stdin, keeping SUBROUTER_EXTRA_ARGS when not passed, and
# its fingerprint prints BOOT_PIN for the service's own store and fails for any
# other, as the real ones do.
BOOT_PIN=9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
make_boot_build() { # path id guard|unguarded
  local guard="; refusing to bind"; [ "$3" = unguarded ] && guard=""
  { printf '#!/bin/bash\n# fake subrouter build %s (post-isolation: attest-legacy%s)\n' "$2" "$guard"
    cat <<'EOF'
echo "exec $(basename "$0") $*" >> "$FAKE_LOG"
case "${1:-}" in
  install-systemd)
    shift; addr=""; extra=""; keep=1
    while [ $# -gt 0 ]; do
      case "$1" in
        --addr) addr=$2; shift 2 ;;
        --extra-args) extra=$2; keep=0; shift 2 ;;
        *) shift ;;
      esac
    done
    f="$BOOTSTRAP_TEST_ROOT/etc/default/subrouter"
    [ "$keep" = 0 ] || extra=$(sed -n 's/^SUBROUTER_EXTRA_ARGS="\(.*\)"$/\1/p' "$f" 2>/dev/null)
    read -r admin; read -r import; read -r proxy
    printf 'SUBROUTER_ADDR=%s\nSUBROUTER_ADMIN_TOKEN=%s\nSUBROUTER_ACCOUNT_IMPORT_TOKEN=%s\nSUBROUTER_PROXY_TOKEN=%s\nSUBROUTER_EXTRA_ARGS="%s"\n' \
      "$addr" "$admin" "$import" "$proxy" "$extra" > "$f" ;;
  fingerprint)
    if [ "$*" = "fingerprint --sessions $BOOTSTRAP_TEST_ROOT/var/lib/subrouter/sessions.json" ]; then echo "$FAKE_PIN"
    else echo "subrouter: no TLS identity in $(dirname "${3:-.}")" >&2; exit 1; fi ;;
esac
exit 0
EOF
  } > "$1"
  chmod 755 "$1"
}
boot_full_setup() { # [unguarded]
  boot_setup post post
  make_boot_build "$C/subrouter-upload" new "${1:-guard}"
  stub tailscale 'echo "tailscale $*" >> "$FAKE_LOG"
case "$1" in
  status) [ -e "$FAKE_TS_UP" ] || exit 1
    [ "${2:-}" = --json ] && [ -z "${FAKE_TS_NO_NODE_ID:-}" ] && echo "{\"Self\": {\"ID\": \"nTESTNODE1CNTRL\"}}"; exit 0 ;;
  up) : > "$FAKE_TS_UP" ;;
  ip) [ -e "$FAKE_TS_UP" ] && echo 100.64.0.10 ;;
esac'
  stub systemctl 'echo "systemctl $*" >> "$FAKE_LOG"; [ "$1" = restart ] && echo up > "$FAKE_RUNNING"; exit 0'
  stub curl 'for url; do :; done
echo "curl $url" >> "$FAKE_LOG"
case "$url" in
  http://100.64.0.10:31415/_subrouter/health) [ -s "$FAKE_RUNNING" ] && echo "{\"ok\": true}" ;;
  https://checkip.amazonaws.com) [ -z "${FAKE_NO_CHECKIP:-}" ] && echo 203.0.113.10 ;;
  *) false ;;
esac || exit 7'
  stub ss 'printf "State Recv-Q Send-Q Local Address:Port Peer Address:Port\nLISTEN 0 4096 100.64.0.10:31415 0.0.0.0:*\nLISTEN 0 4096 *:31416 *:*\n"'
}
# The one peers-file entry bootstrap printed, read as the daemon reads the
# file: exactly these keys, with these values.
expect_peers_entry() { # name url
  local proxy why
  proxy=$(sed -n 's/^SUBROUTER_PROXY_TOKEN=//p' "$R/etc/default/subrouter")
  why=$("$PY3" - "$C/out" "$1" "$2" "$proxy" "$BOOT_PIN" 2>&1 <<'PY'
import json, sys
out, name, url, token, pin = sys.argv[1:]
lines = [l.strip() for l in open(out) if l.lstrip().startswith('{"peers"')]
if len(lines) != 1:
    sys.exit("%d peers-file lines printed, want 1" % len(lines))
try:
    doc = json.loads(lines[0])
except ValueError as e:
    sys.exit("the peers-file line is not JSON (%s): %s" % (e, lines[0]))
want = {"peers": [{"name": name, "url": url, "token": token, "fingerprint": pin}]}
if doc != want:
    sys.exit("the peers-file line is %s, want %s" % (json.dumps(doc), json.dumps(want)))
PY
  ) && ok || bad "$why"
}
expect_no_stage() { [ -e "$R/usr/local/bin/subrouter.stage" ] && bad "the staged build was left behind" || ok; }

echo "== bootstrap.sh"

if begin boot-refuses-isolation-crossing; then
  boot_setup pre post; run_boot
  expect_rc 1; expect_out "predates Codex credential isolation"; expect_out "ATTEST_FIRST=1"
  expect_no_log "tailscale"
  finish
fi

if begin boot-crossing-allowed-by-override; then
  # Past the guard the script reaches the (stubbed, failing) tailscale step.
  boot_setup pre post; run_boot ALLOW_UNATTESTED_UPGRADE=1
  expect_no_out "predates Codex credential isolation"; expect_log "tailscale status"
  finish
fi

if begin boot-same-era-unaffected; then
  boot_setup post post; run_boot
  expect_no_out "predates Codex credential isolation"; expect_log "tailscale status"
  finish
fi

# SUBROUTER_BOX_NAME names the box on the tailnet and in the laptop's
# configuration. One that cannot be all three is refused before anything runs.
if begin boot-names-the-tailnet-host; then
  boot_setup post post; run_boot SUBROUTER_BOX_NAME=box-2
  expect_log "tailscale up --ssh --hostname box-2"
  finish
fi

if begin boot-default-name-is-box; then
  boot_setup post post; run_boot
  expect_log "tailscale up --ssh --hostname box$"
  finish
fi

if begin boot-bad-box-name-refused; then
  label63=$(printf 'b%.0s' $(seq 1 63))
  for name in My_Box -box box- "${label63}b"; do
    boot_setup post post; : > "$C/log"; run_boot SUBROUTER_BOX_NAME="$name"
    expect_rc 1; expect_out "Nothing was changed"; expect_no_log "tailscale"
    expect_build "$R/usr/local/bin/subrouter" old
    if [ "$name" = "${label63}b" ]; then expect_out "SUBROUTER_BOX_NAME is longer than 63 characters"
    else expect_out "SUBROUTER_BOX_NAME must be lower-case letters, digits and hyphens, and must not start or end with a hyphen; got '$name'"; fi
  done
  boot_setup post post; run_boot SUBROUTER_BOX_NAME="$label63"
  expect_log "tailscale up --ssh --hostname $label63$"
  finish
fi

# The laptop configuration bootstrap prints is what the operator pastes, and
# the daemon reads its peers file strictly: one wrong key arms no peer at all.
if begin boot-prints-the-laptop-configuration; then
  boot_full_setup; run_boot SUBROUTER_BOX_NAME=box-2 SUBROUTER_PUBLIC_PORT=31416
  expect_rc 0; expect_out "== done"; expect_build "$R/usr/local/bin/subrouter" new
  expect_out "sr reaches this box only on a laptop that holds remote.key"
  [ "$(grep -Fxc '  sr server add box-2 --url http://100.64.0.10:31415 \' "$C/out")" = 2 ] && ok ||
    bad "output lacks two 'sr server add box-2 --url http://100.64.0.10:31415' commands"
  expect_line "    --public-url 'https://203.0.113.10:31416' \\"
  expect_line "    --fingerprint '$BOOT_PIN'"
  expect_out "SUBROUTER_PEER_TAILNET=box-2=100.64.0.10 "
  expect_peers_entry box-2 https://100.64.0.10:31416
  # The entry carries the box's node ID, or `sr claude move` sends nothing, and
  # never --default, which rewrites the laptop's client configs.
  expect_line "    --tailscale-node-id 'nTESTNODE1CNTRL' \\"
  if grep -Eq -- "--default( |$)" "$C/out"; then bad "the printed sr server add carries --default"; else ok; fi
  finish
fi

if begin boot-node-id-unknown-prints-where-to-find-it; then
  boot_full_setup; run_boot SUBROUTER_BOX_NAME=box-2 FAKE_TS_NO_NODE_ID=1
  expect_rc 0; expect_out "== done"
  expect_line "    --tailscale-node-id '<node-id>' \\"
  expect_out "<node-id> is this box's ID under Peer in 'tailscale status --json' on the laptop."
  finish
fi

if begin boot-public-ip-lookup-failure-still-finishes; then
  boot_full_setup; run_boot SUBROUTER_PUBLIC_PORT=31416 FAKE_NO_CHECKIP=1
  expect_rc 0; expect_out "Public URL:  https://<public-ip>:31416"
  expect_peers_entry box https://100.64.0.10:31416
  finish
fi

# A re-run keeps the tokens every client holds, and never renames the box.
if begin boot-rerun-keeps-the-tokens; then
  boot_full_setup; run_boot SUBROUTER_PUBLIC_PORT=31416
  grep TOKEN "$R/etc/default/subrouter" > "$C/first-tokens"
  run_boot
  expect_rc 0
  grep TOKEN "$R/etc/default/subrouter" | cmp -s - "$C/first-tokens" && ok || bad "the re-run changed a token"
  [ "$(grep -c '^tailscale up' "$C/log")" = 1 ] && ok || bad "the re-run ran tailscale up again"
  expect_peers_entry box https://100.64.0.10:31416
  finish
fi

# bootstrap.sh shares deploy-box.sh's lock: it changes nothing while a deploy
# runs, and runs once that deploy has ended.
if begin boot-refused-while-a-deploy-runs; then
  boot_full_setup
  hold_deploy_lock "deploy-box.sh, pid 4321, log /var/tmp/deploy-box-1.log"
  run_boot SUBROUTER_PUBLIC_PORT=31416
  release_lock
  expect_rc 1
  expect_out "a deploy or another bootstrap is running (deploy-box.sh, pid 4321, log /var/tmp/deploy-box-1.log). Let it end, then run this again. Nothing was changed."
  expect_no_log "tailscale"; expect_no_log "install-systemd"; expect_build "$R/usr/local/bin/subrouter" old
  run_boot SUBROUTER_PUBLIC_PORT=31416
  expect_rc 0; expect_out "== done"; expect_build "$R/usr/local/bin/subrouter" new
  finish
fi

# A build is checked before it replaces the installed one.
if begin boot-unrunnable-build-refused; then
  boot_full_setup
  printf '\177ELF\002\001\001\000 a build for another machine; refusing to bind\n' > "$C/subrouter-upload"
  run_boot
  expect_rc 1; expect_out "does not run here"; expect_out "Nothing was installed"
  expect_build "$R/usr/local/bin/subrouter" old; expect_no_stage; expect_no_log "install-systemd"
  finish
fi

if begin boot-unguarded-build-refused; then
  boot_full_setup unguarded; run_boot
  expect_rc 1; expect_out "lacks the non-loopback admin-token guard"; expect_out "Nothing was installed"
  expect_build "$R/usr/local/bin/subrouter" old; expect_no_stage; expect_no_log "install-systemd"
  finish
fi

# --- push-box.sh --------------------------------------------------------------
# The laptop side. The fake ssh plays the box: an upload (`cat > <path>`, the
# first behind `mkdir -m 0700 <dir> &&`, which refuses a directory that exists)
# lands under $C/remote, and `rm -rf` removes there. Any other command is the
# box script: it is logged, $C/remote.at-run keeps what the box held as it
# started, and it answers FAKE_REMOTE_RC. FAKE_SSH_FAILS lists the calls (1,
# 2, ...) that fail as an unreachable host does.
push_setup() {
  mkdir -p "$C/remote/tmp"
  BUILD="$C/subrouter-linux"
  printf '\177ELF fake linux build %s\n' "$CASE" > "$BUILD"
  stub ssh '
n=$(( $(cat "$FAKE_SSH_N" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$FAKE_SSH_N"
echo "ssh $*" >> "$FAKE_LOG"
for cmd; do :; done
case " ${FAKE_SSH_FAILS:-} " in *" $n "*) echo "ssh: connect to host: Operation timed out" >&2; exit 255 ;; esac
case "$cmd" in
  "mkdir -m 0700 /"*" && cat > /"*)
    d=${cmd#mkdir -m 0700 }; d=${d%% && *}
    mkdir -m 0700 "$FAKE_REMOTE$d" || exit 1
    cat > "$FAKE_REMOTE${cmd##* && cat > }"; exit 0 ;;
  "cat > /"*) cat > "$FAKE_REMOTE${cmd#cat > }"; exit 0 ;;
  "rm -rf /tmp/"*) rm -rf "$FAKE_REMOTE${cmd#rm -rf }"; exit 0 ;;
  *) rm -rf "$FAKE_REMOTE.at-run"; cp -R "$FAKE_REMOTE" "$FAKE_REMOTE.at-run"
     echo "box ran: $cmd"; exit "${FAKE_REMOTE_RC:-0}" ;;
esac'
}
run_push() { # [env assignments...] -- push-box.sh args...
  local envs=()
  while [ $# -gt 0 ] && [ "$1" != -- ]; do envs+=("$1"); shift; done
  shift
  run_sandboxed PATH="$C/bin:/usr/bin:/bin:/usr/sbin:/sbin" HOME="$C/home" TMPDIR="$C/tmp" \
    FAKE_LOG="$C/log" FAKE_SSH_N="$C/ssh-calls" FAKE_REMOTE="$C/remote" ${envs[@]+"${envs[@]}"} \
    -- "$BASH5" "$PUSH_SH" "$@" < /dev/null
}
build_sha() { shasum -a 256 "$BUILD" | awk '{print $1}'; }
# Every path an upload wrote on the box, in order.
push_uploads() { sed -n 's|^ssh -- [^ ]* .*cat > \(/[^ ]*\)$|\1|p' "$C/log"; }
# The directory the nth push (default 1) made on the box.
push_dir() { sed -n 's|^ssh -- [^ ]* mkdir -m 0700 \(/tmp/[^ ]*\) && cat > .*|\1|p' "$C/log" | sed -n "${1:-1}p"; }
expect_uploaded() { # local-file remote-path: on the box as its script started
  cmp -s "$1" "$C/remote.at-run$2" && ok || bad "$2 on the box is not $(basename "$1")"
}
expect_nothing_sent() { expect_rc 64; expect_out "Nothing was sent"; expect_no_log "ssh"; }

echo "== push-box.sh"

if begin push-deploy-ok; then
  push_setup; run_push -- deploy "$BUILD" --host ubuntu@box
  expect_rc 0; D=$(push_dir)
  case "$D" in /tmp/subrouter-push-*) ok ;; *) bad "the push made no directory of its own on the box (got '$D')" ;; esac
  expect_uploaded "$BUILD" "$D/subrouter.new"; expect_uploaded "$BOX_SH" "$D/deploy-box.sh"
  # The box is told the sha of the bytes sent, and "-": the admin token stays
  # in its env file, out of every argv.
  expect_log "^ssh -- ubuntu@box env bash $D/deploy-box.sh $(build_sha) - $D/subrouter.new$"
  expect_order "ssh -- ubuntu@box cat > $D/deploy-box.sh" "bash $D/deploy-box.sh"
  # A box script that exits 0 leaves nothing behind.
  [ -e "$C/remote$D" ] && bad "$D is still on the box" || ok
  finish
fi

# A second push, such as a retry while the first deploy still runs, never
# writes over the first one's files.
if begin push-each-push-gets-its-own-directory; then
  push_setup; run_push FAKE_SSH_FAILS=3 -- deploy "$BUILD" --host box
  cp "$BUILD" "$C/first-build"; printf 'a second build\n' > "$BUILD"
  run_push -- deploy "$BUILD" --host box
  expect_rc 0
  B1=$(push_uploads | sed -n 1p); S1=$(push_uploads | sed -n 2p); B2=$(push_uploads | sed -n 3p)
  [ -n "$B1" ] && [ -n "$B2" ] && [ "$B1" != "$B2" ] && ok || bad "the second push wrote its build to the first one's path ('$B1')"
  cmp -s "$C/first-build" "$C/remote$B1" && ok || bad "the second push changed the first one's build at $B1"
  cmp -s "$BOX_SH" "$C/remote$S1" && ok || bad "the first push's script is gone from $S1"
  expect_uploaded "$BUILD" "$B2"
  finish
fi

# Kept for the commands a failed or incomplete deploy prints.
if begin push-deploy-returns-the-box-exit-code; then
  push_setup; run_push FAKE_REMOTE_RC=3 -- deploy "$BUILD" --host ubuntu@box
  expect_rc 3
  [ -e "$C/remote$(push_dir)/deploy-box.sh" ] && ok || bad "the push's directory was removed after exit 3"
  finish
fi

if begin push-missing-host-refused; then
  push_setup; run_push -- deploy "$BUILD"
  expect_nothing_sent; expect_out "no box to push to: pass --host <ssh-target> or set SUBROUTER_BOX_SSH"
  finish
fi

if begin push-host-from-the-environment; then
  push_setup; run_push SUBROUTER_BOX_SSH=box -- deploy "$BUILD"
  expect_rc 0; expect_log "^ssh -- box env bash /tmp/subrouter-push-[^ ]*/deploy-box.sh "
  finish
fi

if begin push-host-flag-beats-the-environment; then
  push_setup; run_push SUBROUTER_BOX_SSH=old-box -- deploy "$BUILD" --host=box
  expect_rc 0; expect_log "^ssh -- box "; expect_no_log "old-box"
  finish
fi

# ssh reads a destination that starts with '-' as an option.
if begin push-host-that-is-an-option-refused; then
  push_setup; run_push -- deploy "$BUILD" --host=-oProxyCommand=true
  expect_nothing_sent; expect_out "'-oProxyCommand=true' is not an ssh destination"
  finish
fi

if begin push-missing-build-refused; then
  push_setup; run_push -- deploy "$C/no-such-build" --host box
  expect_nothing_sent; expect_out "no build at $C/no-such-build"
  finish
fi

if begin push-unknown-verb-refused; then
  push_setup; run_push -- install "$BUILD" --host box
  expect_rc 64; expect_out "usage: push-box.sh bootstrap|deploy"; expect_no_log "ssh"
  finish
fi

# Only the box script's own knobs cross, quoted; this shell's CODEX_HOME does not.
if begin push-deploy-passes-only-its-knobs; then
  push_setup
  run_push DRAIN_GRACE_SECONDS=60 ATTEST_FIRST=1 CODEX_HOME=/laptop/codex SUBROUTER_BOX_NAME=box SUBROUTER_PUBLIC_PORT=off -- deploy "$BUILD" --host box
  expect_rc 0
  expect_log "^ssh -- box env ATTEST_FIRST='1' DRAIN_GRACE_SECONDS='60' bash $(push_dir)/deploy-box.sh $(build_sha) - $(push_dir)/subrouter.new$"
  expect_no_log "CODEX_HOME"; expect_no_log "SUBROUTER_BOX_NAME"; expect_no_log "SUBROUTER_PUBLIC_PORT"
  finish
fi

if begin push-knob-the-box-shell-would-read-refused; then
  push_setup; run_push "DRAIN_GRACE_SECONDS=60;reboot" -- deploy "$BUILD" --host box
  expect_nothing_sent; expect_out "DRAIN_GRACE_SECONDS='60;reboot' holds a character the box's shell would read"
  finish
fi

if begin push-bootstrap-ok; then
  push_setup
  run_push SUBROUTER_PUBLIC_PORT=31416 SUBROUTER_BOX_NAME=box DRAIN_GRACE_SECONDS=60 -- bootstrap "$BUILD" --host ubuntu@203.0.113.10
  expect_rc 0; D=$(push_dir)
  expect_uploaded "$BUILD" "$D/subrouter"; expect_uploaded "$BOOT_SH" "$D/bootstrap.sh"
  expect_log "^ssh -- ubuntu@203.0.113.10 sudo env SUBROUTER_PUBLIC_PORT='31416' SUBROUTER_BOX_NAME='box' bash $D/bootstrap.sh $D/subrouter$"
  finish
fi

if begin push-lost-connection-names-the-box-log; then
  push_setup; run_push FAKE_SSH_FAILS=3 -- deploy "$BUILD" --host box
  expect_rc 255; expect_out "A deploy that started goes on by itself on the box. Do not push again until it ends"
  expect_line "  ssh box 'tail -f \"\$(ls -t ~/.subrouter/transitions/deploy-box-*.log | head -1)\"'"
  [ -e "$C/remote$(push_dir)/deploy-box.sh" ] && ok || bad "the push's directory was removed while its deploy may run"
  finish
fi

if begin push-failed-upload-runs-nothing; then
  push_setup; run_push FAKE_SSH_FAILS=1 -- deploy "$BUILD" --host box
  expect_rc 64; expect_out "could not write $(push_dir)/subrouter.new on box (ssh exited 255)"
  expect_no_log "bash /tmp/"
  finish
fi


# --- the laptop ---------------------------------------------------------------
write_serve_plist() { # path home
  "$PY3" - "$1" "$2" <<'PY'
import os, plistlib, sys
with open(sys.argv[1], "wb") as f:
    plistlib.dump({"Label": os.environ["TEST_JOB"],
                   "ProgramArguments": [sys.argv[2] + "/bin/subrouter", "serve"],
                   "EnvironmentVariables": {"HOME": sys.argv[2]}}, f)
PY
}

laptop_setup() { # old-era new-era
  H="$C/home"
  LAPTOP_UNDER_TEST="$LAPTOP_SH"
  mkdir -p "$H/bin" "$H/Library/LaunchAgents" "$H/.claude" "$H/.subrouter" "$C/launchd"
  CUR="$H/bin/subrouter"
  NEWBIN="$C/subrouter-new"
  make_build "$CUR" "$1" old
  make_build "$NEWBIN" "$2" new
  write_serve_plist "$H/Library/LaunchAgents/$TEST_JOB.plist" "$H"
  echo '{"env":{"ANTHROPIC_AUTH_TOKEN":"admin-tok"}}' > "$H/.claude/settings.json"
  load_plist "$H/Library/LaunchAgents/$TEST_JOB.plist"
  echo old > "$C/launchd/running"
  echo 100 > "$C/launchd/pid"
  echo 0 > "$H/.subrouter/fake-codex-needing-migration"
  # The listener on the daemon's port: the pid of the process launchd last
  # started, while a build is running.
  stub lsof '
case " $* " in *" -iTCP:'"${TEST_ADDR##*:}"' "*) ;; *) echo "UNEXPECTED lsof $*" >> "$FAKE_LOG"; exit 97 ;; esac
[ -s "$FAKE_RUNNING" ] || exit 1
cat "$FAKE_LAUNCHD/pid"'
  # A launchd in the logged-in GUI session (managername Aqua). The loaded job
  # is a copy of the plist it was bootstrapped from, and print shows its
  # arguments as launchd does. Every process it starts gets the next pid. Like
  # launchd, it answers 113 to a print or a kickstart of a job that is not
  # loaded, and refuses to bootstrap a label it still holds. Knobs:
  #   FAKE_KICKSTART_NOOP   kickstart exits 0 and restarts nothing
  #   FAKE_PRINT_REFUSED    always | after-bootout: print rc 141, as from a shell
  #                         launchd refuses
  #   FAKE_BOOTSTRAP_FAILS  every bootstrap exits 5 with launchd's message
  #   FAKE_BOOTOUT_LOCKS    a directory every bootout makes read-only, so the
  #                         files a run puts back after it cannot be written
  #   FAKE_BOOTOUT_REMOVES_PLIST  every bootout deletes the plist on disk
  #   FAKE_SUPERVISE_AT_PRINT=<n>:<plist>  before answering the nth print, an
  #                         activation finishes: <plist> is written and loaded
  #   FAKE_STRAY            the daemon on the port was started by hand, not by
  #                         the job: launchd never stops it, and the job's own
  #                         process never takes the port
  stub launchctl '
echo "launchctl $*" >> "$FAKE_LOG"
S="$FAKE_LAUNCHD"
build() { sed -n "s/^# fake subrouter build \([^ ]*\).*/\1/p" "$1" 2>/dev/null; }
start() {
  [ -n "${FAKE_STRAY:-}" ] && return 0
  id=$(build "$FAKE_DEST")
  echo $(( $(cat "$S/pid") + 1 )) > "$S/pid"
  echo "${FAKE_SOCKET_LATE:-0}" > "$S/socket-late"
  if [ -n "${FAKE_CRASH_BUILD:-}" ] && [ "$id" = "$FAKE_CRASH_BUILD" ]; then : > "$S/running"; : > "$S/supervisor"; return 0; fi
  echo "$id" > "$S/running"
  rm -f "$S/supervisor-lacks-socket"
  if grep -q "<string>supervise</string>" "$S/loaded"; then
    sv=$(python3 -c "import plistlib, sys; print(plistlib.load(open(sys.argv[1], \"rb\"))[\"ProgramArguments\"][0])" "$S/loaded")
    build "$sv" > "$S/supervisor"
    grep -q owned_by_supervisor "$sv" || : > "$S/supervisor-lacks-socket"
  else
    : > "$S/supervisor"
  fi
}
refused() { echo "Could not print domain: 141: Reentrancy avoided" >&2; exit 141; }
eio() { echo "Bootstrap failed: 5: Input/output error" >&2; exit 5; }
case "$1" in
  managername) echo "${FAKE_MANAGER:-Aqua}" ;;
  kickstart)
    if [ -n "${FAKE_HOLD:-}" ]; then
      python3 -c "import os; print(os.getpgid(0))" > "$FAKE_HOLD.next" && mv "$FAKE_HOLD.next" "$FAKE_HOLD.held"
      until [ -e "$FAKE_HOLD" ]; do /bin/sleep 0.1; done
    fi
    [ -e "$S/loaded" ] || { echo "Could not find service \"${3##*/}\" in domain for user gui: $(id -u)" >&2; exit 113; }
    [ -n "${FAKE_KICKSTART_NOOP:-}" ] || start ;;
  bootout)
    if grep -q "<string>supervise</string>" "$FAKE_PLIST"; then echo "bootout: the plist on disk runs supervise" >> "$FAKE_LOG"
    else echo "bootout: the plist on disk runs serve" >> "$FAKE_LOG"; fi
    echo "bootout: the worker on disk is $(build "$FAKE_DEST")" >> "$FAKE_LOG"
    echo "bootout: the plist on disk has checksum $(cksum < "$FAKE_PLIST" | cut -d" " -f1)" >> "$FAKE_LOG"
    [ -z "${FAKE_BOOTOUT_LOCKS:-}" ] || chmod 555 "$FAKE_BOOTOUT_LOCKS"
    [ -z "${FAKE_BOOTOUT_REMOVES_PLIST:-}" ] || rm -f "$FAKE_PLIST"
    [ -e "$S/loaded" ] && echo "${FAKE_BOOTOUT_POLLS:-0}" > "$S/draining"
    rm -f "$S/loaded"; : > "$S/booted-out"
    [ -n "${FAKE_STRAY:-}" ] || { : > "$S/running"; : > "$S/supervisor"; }
    if [ -n "${FAKE_KILL_FG:-}" ] && [ ! -e "$S/killed" ]; then
      : > "$S/killed"
      for _ in $(seq 1 100); do [ -s "$FAKE_KILL_FG" ] && break; /bin/sleep 0.05; done
      echo "SIGKILL the foreground process group" >> "$FAKE_LOG"
      kill -9 -- "-$(cat "$FAKE_KILL_FG")"
    fi ;;
  bootstrap)
    [ -n "${FAKE_BOOTSTRAP_FAILS:-}" ] && eio
    if [ -n "${FAKE_BOOTSTRAP_FAILS_ONCE:-}" ] && [ ! -e "$S/bootstrap-failed" ]; then : > "$S/bootstrap-failed"; eio; fi
    [ -e "$S/loaded" ] && eio
    [ -f "$3" ] || { echo "Bootstrap failed: 2: No such file or directory" >&2; exit 2; }
    cp "$3" "$S/loaded"; echo "$3" > "$S/loaded-from"; start ;;
  print)
    if [ -n "${FAKE_SUPERVISE_AT_PRINT:-}" ]; then
      n=$(( $(cat "$S/prints" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$S/prints"
      if [ "$n" = "${FAKE_SUPERVISE_AT_PRINT%%:*}" ]; then
        cp "${FAKE_SUPERVISE_AT_PRINT#*:}" "$FAKE_PLIST"; cp "$FAKE_PLIST" "$S/loaded"; echo activated > "$S/supervisor"
        echo "an activation finished: launchd runs supervise" >> "$FAKE_LOG"
      fi
    fi
    case "${FAKE_PRINT_REFUSED:-}" in
      always) refused ;;
      after-bootout) [ -e "$S/booted-out" ] && refused ;;
    esac
    n=$(cat "$S/draining" 2>/dev/null || echo 0)
    if [ "$n" -gt 0 ]; then echo $((n - 1)) > "$S/draining"; echo "$2 = { state = exiting }"; exit 0; fi
    [ -e "$S/loaded" ] || exit 113
    python3 "$S/print.py" "$S/loaded" "$2" ;;
esac
exit 0'
  cat > "$C/launchd/print.py" <<'PY'
import os, plistlib, sys
args = [str(a) for a in plistlib.load(open(sys.argv[1], "rb"))["ProgramArguments"]]
loaded_from = open(os.path.join(os.path.dirname(sys.argv[1]), "loaded-from")).read().strip()
print("%s = {\n\tactive count = 1\n\tpath = %s\n\tstate = running\n\n\tprogram = %s\n\targuments = {" % (sys.argv[2], loaded_from, args[0]))
print("".join("\t\t%s\n" % a for a in args) + "\t}\n")
print("\tlast exit reason = (none)\n\texit timeout = 5\n}")
PY
}
load_plist() { cp "$1" "$C/launchd/loaded"; echo "$1" > "$C/launchd/loaded-from"; }
unload_job() { rm -f "$C/launchd/loaded"; : > "$C/launchd/running"; : > "$C/launchd/supervisor"; }

# A shell in a login session that has ended (PATCHES §47), as measured from a
# Superset terminal whose pty-daemon outlived a logout: managername rc 153,
# every other verb rc 141, and nothing changes.
dead_launchd() {
  stub launchctl '
echo "launchctl $*" >> "$FAKE_LOG"
case "$1" in managername) echo "Could not get manager name." >&2; exit 153 ;; esac
echo "Could not print domain: 141: Reentrancy avoided" >&2; exit 141'
}
# A shell outside the GUI session (ssh): launchd answers, the bootout works, and
# kickstarting or bootstrapping into gui/<uid> is refused. Modelled; the
# asymmetry was not observed on a real Mac.
background_launchd() {
  stub launchctl '
echo "launchctl $*" >> "$FAKE_LOG"
S="$FAKE_LAUNCHD"
case "$1" in
  managername) echo Background ;;
  kickstart) echo "Could not kickstart service: 125: Domain does not support specified action" >&2; exit 125 ;;
  bootout) rm -f "$S/loaded"; : > "$S/running" ;;
  bootstrap) echo "Bootstrap failed: 125: Domain does not support specified action" >&2; exit 125 ;;
  print) [ -e "$S/loaded" ] || exit 113; python3 "$S/print.py" "$S/loaded" "$2" ;;
esac
exit 0'
}
expect_running() { # build id the daemon is serving ("" = down)
  local got; got=$(cat "$C/launchd/running" 2>/dev/null)
  [ "$got" = "$1" ] && ok || bad "the daemon serves build '$got', want '$1'"
}

run_laptop() { # extra env assignments... [-- script args]
  local envs=() args=()
  while [ $# -gt 0 ] && [ "$1" != -- ]; do envs+=("$1"); shift; done
  [ $# -gt 0 ] && shift
  args=("$@")
  [ ${#args[@]} -gt 0 ] || args=("$NEWBIN")
  run_sandboxed PATH="$C/bin:/usr/bin:/bin:/usr/sbin:/sbin" HOME="$H" TMPDIR="$C/tmp" \
    CODEX_HOME=/leaked/codex-home SUBROUTER_STATE_DIR=/leaked/state SUBROUTER_CODEX_SERVER=leaked \
    FAKE_LOG="$C/log" FAKE_LAUNCHD="$C/launchd" FAKE_RUNNING="$C/launchd/running" \
    FAKE_DAEMON="$C/launchd/running" FAKE_DEST="$CUR" FAKE_NEW_ID=new \
    FAKE_PLIST="$H/Library/LaunchAgents/$TEST_JOB.plist" \
    FAKE_CONTROL_SOCKET="$H/.subrouter/supervisor.sock" FAKE_CLAUDE_SOCKET="$H/.subrouter/claude.sock" \
    FAKE_ADMIN=admin-tok ${envs[@]+"${envs[@]}"} \
    -- "$LAPTOP_BASH" "$LAPTOP_UNDER_TEST" "${args[@]}"
}

echo "== deploy-laptop.sh (under $LAPTOP_BASH)"

if begin laptop-ok; then
  laptop_setup pre post; run_laptop
  expect_rc 0; expect_out "DEPLOY OK"; expect_build "$CUR" new
  # The daemon's HOME from its plist; the shell's CODEX_HOME and state dir unset.
  expect_log "exec build=new bin=[^ ]*/home/bin/subrouter args=codex isolation-check --json sudo_user=none HOME=[^ ]*/home STATE=<unset> CODEX_HOME=<unset>"
  finish
fi

if begin laptop-isolation-incomplete; then
  laptop_setup pre post; echo 1 > "$H/.subrouter/fake-codex-needing-migration"
  run_laptop
  expect_rc 3; expect_out "DEPLOY INCOMPLETE"; expect_no_out "DEPLOY OK"; expect_no_out "ROLLING BACK"
  expect_out "1 Codex account(s) fail the credential-isolation gate"
  expect_out "codex attest-legacy"; expect_out "sr codex migrate-isolation"
  # This shell exports CODEX_HOME (run_laptop plants it), so the pasteable
  # remediation must strip it, as the check itself did.
  expect_out "1. env -u CODEX_HOME -u SUBROUTER_STATE_DIR $H/bin/subrouter codex attest-legacy"
  expect_build "$CUR" new
  finish
fi

if begin laptop-isolation-check-cannot-run; then
  laptop_setup pre post; run_laptop FAKE_ISO_BROKEN=1
  expect_rc 3; expect_out "DEPLOY INCOMPLETE"; expect_out "The check could not run"
  finish
fi

if begin laptop-plist-state-dir-honored; then
  laptop_setup pre post
  mkdir -p "$H/alt-state"; echo 2 > "$H/alt-state/fake-codex-needing-migration"
  "$PY3" - "$H/Library/LaunchAgents/$TEST_JOB.plist" "$H" <<'PY'
import plistlib, sys
with open(sys.argv[1], "rb") as f:
    p = plistlib.load(f)
p["EnvironmentVariables"]["SUBROUTER_STATE_DIR"] = sys.argv[2] + "/alt-state"
with open(sys.argv[1], "wb") as f:
    plistlib.dump(p, f)
PY
  run_laptop
  expect_rc 3; expect_out "2 Codex account(s) fail"
  expect_log "args=codex isolation-check --json sudo_user=none HOME=[^ ]*/home STATE=[^ ]*/home/alt-state CODEX_HOME=<unset>"
  # The printed remediation targets the same store.
  expect_out "1. env -u CODEX_HOME -u SUBROUTER_STATE_DIR SUBROUTER_STATE_DIR=$H/alt-state $H/bin/subrouter codex attest-legacy"
  finish
fi

if begin laptop-rollback-refused-verify; then
  laptop_setup pre post
  seed_record "$H/.subrouter" a@x isolated-server-login
  seed_record "$H/.subrouter" b@x server-attested-transfer
  run_laptop FAKE_BAD=accounts-404
  expect_rc 1; expect_out "NOT ROLLING BACK (verification regressed)"
  expect_out "2 Codex record(s) in $H/.subrouter/codex/accounts carry oauthCredentialOrigin"
  expect_out "ALLOW_PRE_ISOLATION_ROLLBACK=1"; expect_build "$CUR" new
  finish
fi

if begin laptop-rollback-allowed-verify; then
  laptop_setup pre post; seed_record "$H/.subrouter" a@x isolated-server-login
  run_laptop FAKE_BAD=accounts-404 ALLOW_PRE_ISOLATION_ROLLBACK=1
  expect_rc 1; expect_out "!! ROLLING BACK"; expect_no_out "NOT ROLLING BACK"
  expect_build "$CUR" old
  finish
fi

if begin laptop-rollback-refused-down; then
  laptop_setup pre post; seed_record "$H/.subrouter" a@x server-attested-transfer compact
  run_laptop FAKE_CRASH_BUILD=new
  expect_rc 2; expect_out "NOT ROLLING BACK (daemon did not come back)"
  expect_out "1 Codex record(s) in"
  expect_out "The daemon is DOWN on the new build"; expect_build "$CUR" new
  finish
fi

if begin laptop-rollback-allowed-down; then
  laptop_setup pre post; seed_record "$H/.subrouter" a@x isolated-server-login
  run_laptop FAKE_CRASH_BUILD=new ALLOW_PRE_ISOLATION_ROLLBACK=1
  expect_rc 1; expect_out "rolled back, healthy"; expect_build "$CUR" old
  finish
fi

if begin laptop-rollback-down-no-migrated-records; then
  # The defect this pins: the deploy swaps FIRST, so a genuinely broken first
  # post-isolation build meets a store nothing has migrated yet. It must roll
  # back on its own instead of leaving the daemon DOWN (exit 2).
  laptop_setup pre post; run_laptop FAKE_CRASH_BUILD=new
  expect_rc 1; expect_out "!! ROLLING BACK"; expect_out "rolled back, healthy"
  expect_no_out "NOT ROLLING BACK"; expect_no_out "The daemon is DOWN"
  expect_out "no Codex record in $H/.subrouter/codex/accounts carries a migrated origin; restoring it is safe"
  expect_build "$CUR" old
  finish
fi

if begin laptop-rollback-interactive-only-records; then
  laptop_setup pre post
  seed_record "$H/.subrouter" a@x interactive-import
  seed_record "$H/.subrouter" b@x ""
  # Not records: a dot-file and a directory, whatever they contain.
  printf '{"oauthCredentialOrigin": "isolated-server-login"}\n' > "$H/.subrouter/codex/accounts/.pending@x.json"
  mkdir -p "$H/.subrouter/codex/accounts/migrated.json"
  run_laptop FAKE_BAD=accounts-404
  expect_rc 1; expect_out "!! ROLLING BACK"; expect_no_out "NOT ROLLING BACK"
  expect_out "no Codex record in"; expect_build "$CUR" old
  finish
fi

if begin laptop-rollback-refused-store-unreadable; then
  # One record file cannot be read. It might be migrated, so: refused.
  laptop_setup pre post
  seed_record "$H/.subrouter" a@x interactive-import
  seed_record "$H/.subrouter" b@x interactive-import
  if make_unreadable "$H/.subrouter/codex/accounts/b@x.json"; then
    run_laptop FAKE_CRASH_BUILD=new
    chmod 644 "$H/.subrouter/codex/accounts/b@x.json"
    expect_rc 2; expect_out "NOT ROLLING BACK (daemon did not come back)"
    expect_out "the Codex store could not be read ($H/.subrouter/codex/accounts/b@x.json cannot be read)"
    expect_build "$CUR" new
  fi
  finish
fi

if begin laptop-rollback-refused-store-hidden; then
  # codex/ cannot be searched, so whether accounts/ exists is unknowable -- which
  # is not the same as "it does not exist": refused.
  laptop_setup pre post; seed_record "$H/.subrouter" a@x interactive-import
  if make_unreadable "$H/.subrouter/codex"; then
    run_laptop FAKE_BAD=accounts-404
    chmod 755 "$H/.subrouter/codex"
    expect_rc 1; expect_out "NOT ROLLING BACK (verification regressed)"
    expect_out "cannot tell whether $H/.subrouter/codex/accounts exists: $H/.subrouter/codex cannot be searched"
    expect_build "$CUR" new
  fi
  finish
fi

if begin laptop-rollback-refused-plist-state-dir; then
  # The store is the PLIST's state dir. This shell exports another one
  # (run_laptop plants SUBROUTER_STATE_DIR=/leaked/state) and HOME/.subrouter
  # holds nothing migrated; the record that must stop the rollback is only in
  # the plist's.
  laptop_setup pre post
  seed_record "$H/.subrouter" z@x interactive-import
  mkdir -p "$H/alt-state"; seed_record "$H/alt-state" a@x isolated-server-login
  "$PY3" - "$H/Library/LaunchAgents/$TEST_JOB.plist" "$H" <<'PY'
import plistlib, sys
with open(sys.argv[1], "rb") as f:
    p = plistlib.load(f)
p["EnvironmentVariables"]["SUBROUTER_STATE_DIR"] = sys.argv[2] + "/alt-state"
with open(sys.argv[1], "wb") as f:
    plistlib.dump(p, f)
PY
  run_laptop FAKE_BAD=accounts-404
  expect_rc 1; expect_out "NOT ROLLING BACK (verification regressed)"
  expect_out "1 Codex record(s) in $H/alt-state/codex/accounts"
  expect_build "$CUR" new
  finish
fi

if begin laptop-rollback-post-to-post-unaffected; then
  laptop_setup post post; run_laptop FAKE_BAD=accounts-404
  expect_rc 1; expect_out "!! ROLLING BACK"; expect_build "$CUR" old
  finish
fi

if begin laptop-downgrade-refused; then
  laptop_setup post pre; seed_record "$H/.subrouter" a@x isolated-server-login
  run_laptop
  expect_rc 1; expect_out "refusing to replace a post-isolation build with a pre-isolation one: 1 Codex record(s)"
  expect_build "$CUR" old; expect_no_log "launchctl (kickstart|bootout|bootstrap)"
  if any_exists "$H/bin/"*bak-*; then bad "a backup was taken before the refusal"; else ok; fi
  finish
fi

if begin laptop-downgrade-no-migrated-records; then
  laptop_setup post pre; seed_record "$H/.subrouter" a@x ""
  run_laptop
  expect_rc 0; expect_out "DEPLOY OK"; expect_no_out "refusing to replace"
  expect_out "replacing a post-isolation build with a pre-isolation one: allowed, no Codex record in"
  expect_build "$CUR" new
  finish
fi

if begin laptop-check-incomplete; then
  laptop_setup post post; echo 1 > "$H/.subrouter/fake-codex-needing-migration"
  run_laptop -- "$NEWBIN" --check
  expect_rc 3; expect_out "CHECK INCOMPLETE"; expect_no_out "CHECK OK"
  expect_no_log "launchctl (kickstart|bootout|bootstrap)"
  finish
fi

if begin laptop-check-ok; then
  laptop_setup post post; run_laptop -- "$NEWBIN" --check
  expect_rc 0; expect_out "CHECK OK"
  finish
fi

if begin laptop-installs-where-the-job-runs-not-subrouter-bin; then
  # SUBROUTER_BIN is the npm and Python wrappers' knob. The deploy installed
  # there, restarted a job that runs ~/bin/subrouter, and printed DEPLOY OK
  # on the old build.
  laptop_setup post post; mkdir -p "$H/other"; make_build "$H/other/subrouter" post elsewhere
  run_laptop SUBROUTER_BIN="$H/other/subrouter"
  expect_rc 0; expect_out "DEPLOY OK"; expect_build "$CUR" new; expect_running new
  expect_build "$H/other/subrouter" elsewhere
  finish
fi

# --- deploy-laptop.sh: a shell that cannot restart the daemon (PATCHES §47) ---
LAPTOP_PLIST_REL="Library/LaunchAgents/$TEST_JOB.plist"
expect_no_backup() { if any_exists "$H/bin/"*bak-*; then bad "a backup was taken before the refusal"; else ok; fi; }

if begin laptop-dead-launchd-refused; then
  # From a Superset terminal whose pty-daemon outlived a logout this printed
  # DEPLOY OK: the kickstart was refused without a word and the OLD process
  # answered health, so the new build never ran.
  laptop_setup pre post; dead_launchd; run_laptop
  expect_rc 1; expect_out "this shell cannot reach launchd (launchctl managername: rc 153, Could not get manager name.)"
  expect_out "Nothing was changed"; expect_out "a new Terminal.app window"; expect_out "A new Superset tab is no fix"
  expect_no_out "DEPLOY OK"; expect_no_out "started in your current login"
  expect_build "$CUR" old; expect_running old; expect_no_backup
  expect_no_log "launchctl (kickstart|bootout|bootstrap)"
  finish
fi

if begin laptop-dead-launchd-reload-refused; then
  # RELOAD=1 read the refused print (rc 141) as "unloaded" on the first pass and
  # bootstrapped blind; the plist edit was never applied.
  laptop_setup pre post; dead_launchd; run_laptop RELOAD=1
  expect_rc 1; expect_out "cannot reach launchd"; expect_out "Nothing was changed"; expect_no_out "DEPLOY OK"
  expect_build "$CUR" old; expect_running old; expect_no_backup
  expect_no_log "launchctl (kickstart|bootout|bootstrap)"
  finish
fi

if begin laptop-background-session-refused; then
  # Outside the GUI session (ssh) the bootout works and the bootstrap does not:
  # this left the daemon unloaded and every local session without its proxy.
  laptop_setup pre post; background_launchd; run_laptop RELOAD=1
  expect_rc 1; expect_out "launchd's 'Background' session, not the logged-in GUI (Aqua) session"
  expect_out "Nothing was changed"
  expect_build "$CUR" old; expect_running old; expect_no_backup
  expect_no_log "launchctl (kickstart|bootout|bootstrap)"
  finish
fi

if begin laptop-print-refused-is-not-unloaded; then
  # Aqua, but print is refused: rc 141 answers for a loaded job and a missing
  # one alike, so the restart's wait on the job could not work.
  laptop_setup pre post; run_laptop RELOAD=1 FAKE_PRINT_REFUSED=always
  expect_rc 1; expect_out "answered rc 141 (only 0, loaded, and 113, not loaded, are answers)"
  expect_build "$CUR" old; expect_running old; expect_no_backup
  expect_no_log "launchctl (kickstart|bootout|bootstrap)"
  finish
fi

if begin laptop-check-notes-dead-launchd; then
  # --check restarts nothing, so it still runs, and keeps its exit code; it says
  # that a deploy from here would refuse.
  laptop_setup post post; dead_launchd; run_laptop -- "$NEWBIN" --check
  expect_rc 0; expect_out "CHECK OK"; expect_out "NOTE: this shell cannot reach launchd"
  expect_out "a deploy from this shell would refuse"
  expect_no_log "launchctl (kickstart|bootout|bootstrap)"
  finish
fi

if begin laptop-kickstart-noop-falls-back; then
  # A kickstart that exits 0 and restarts nothing leaves the old process (pid
  # 100) answering health. That is not a restart: the job is reloaded, and the
  # new build has to be the one serving.
  laptop_setup pre post; run_laptop FAKE_KICKSTART_NOOP=1
  expect_rc 0; expect_out "kickstart returned, but pid 100 is still the one serving; reloading the job"
  expect_out "DEPLOY OK"; expect_build "$CUR" new; expect_running new
  expect_order "launchctl kickstart" "launchctl bootout"; expect_order "launchctl bootout" "launchctl bootstrap"
  finish
fi

if begin laptop-kickstart-error-falls-back; then
  # The job is not loaded and nothing serves (a deploy that ended STILL DOWN, or
  # a bootout by hand), so launchd refuses the kickstart with 113. The deploy
  # used to drop that, wait out thirty health checks, and report "kickstart did
  # not come up". Now it shows launchd's answer and reloads at once.
  laptop_setup pre post; rm -f "$C/launchd/loaded"; : > "$C/launchd/running"; run_laptop
  expect_rc 0
  expect_out "kickstart failed (rc 113: Could not find service \"$TEST_JOB\" in domain for user gui: $(id -u)); reloading the job"
  expect_no_out "kickstart did not come up"
  expect_none_between "launchctl kickstart" "launchctl bootout" "curl .*/_subrouter/health"
  expect_order "launchctl bootout" "launchctl bootstrap"
  expect_out "DEPLOY OK"; expect_build "$CUR" new; expect_running new
  finish
fi

if begin laptop-reload-same-pid-fails; then
  # A daemon started by hand holds the port, so the job's own process never
  # binds and health keeps answering from the old pid: not a restart.
  laptop_setup pre post; run_laptop RELOAD=1 FAKE_STRAY=1
  expect_rc 2; expect_out "pid 100 is still the one serving: the job's new process never took the port"
  expect_no_out "DEPLOY OK"
  finish
fi

if begin laptop-reload-same-build-ok; then
  # The plist-only reload (RELOAD=1 with a copy of the running binary)
  # restarts the SAME build. A new pid is what counts, not a new build.
  laptop_setup post post; make_build "$NEWBIN" post old; run_laptop RELOAD=1
  expect_rc 0; expect_out "DEPLOY OK"; expect_running old; expect_log "launchctl bootstrap"
  finish
fi

if begin laptop-unload-wait-needs-rc-113; then
  # launchd stops answering after the bootout. rc 141 is not "the job is gone",
  # so nothing is bootstrapped blind, and STILL DOWN names the command that
  # loads the job from a shell that can.
  laptop_setup pre post; run_laptop RELOAD=1 FAKE_PRINT_REFUSED=after-bootout
  expect_rc 2; expect_out "answered rc 141 after the bootout"; expect_out "not bootstrapping blind"
  expect_out "STILL DOWN"; expect_line "     launchctl bootstrap gui/$(id -u) $H/$LAPTOP_PLIST_REL"
  expect_no_log "launchctl bootstrap"; expect_no_out "DEPLOY OK"
  finish
fi

if begin laptop-bootstrap-error-is-shown; then
  # A failed bootstrap was silent (2>/dev/null) and then waited out two minutes
  # of health checks. Now: launchd's own words, and the command to run, on a
  # line of its own (restart()'s error names the same command mid-sentence).
  laptop_setup pre post; run_laptop RELOAD=1 FAKE_BOOTSTRAP_FAILS=1
  expect_rc 2; expect_out "rc 5: Bootstrap failed: 5: Input/output error"
  expect_out "STILL DOWN"; expect_line "     launchctl bootstrap gui/$(id -u) $H/$LAPTOP_PLIST_REL"
  finish
fi

if begin laptop-refused-while-the-mutation-lock-is-held; then
  laptop_setup post post; hold_lock "$H/$LAPTOP_PLIST_REL"; run_laptop; release_lock
  expect_lock_refusal "$H/$LAPTOP_PLIST_REL"; expect_build "$CUR" old; expect_no_restart; expect_no_backup
  finish
fi

if begin laptop-back-to-back-runs-take-the-lock; then
  laptop_setup post post; run_laptop; expect_rc 0
  run_laptop; expect_rc 0; expect_out "DEPLOY OK"
  run_laptop -- "$CUR"; expect_rc 0; expect_out "DEPLOY OK"
  finish
fi

# The lease helper never leaves a run waiting on it for ever (PATCHES §83).
# FAKE_LEASE_HELPER_DELAY=<s>: a lease helper started with TERM ignored, as a
# detached run starts it, sleeps that long before it starts, keeping its pid.
slow_lease_helper() {
  cat > "$C/lease-delay.py" <<'PY'
import os, signal, sys, time
if signal.getsignal(signal.SIGTERM) == signal.SIG_IGN:
    delay = os.environ["FAKE_LEASE_HELPER_DELAY"]
    with open(os.environ["FAKE_LOG"], "a") as log:
        log.write("lease helper delayed %ss\n" % delay)
    time.sleep(float(delay))
os.execv(sys.argv[1], sys.argv[1:])
PY
  rm -f "$C/bin/python3"
  stub python3 '
case "${2:-}" in *.supervisor-mutation.lock)
  [ -z "${FAKE_LEASE_HELPER_DELAY:-}" ] || exec "'"$PY3"'" "'"$C/lease-delay.py"'" "'"$PY3"'" "$@" ;;
esac
exec "'"$PY3"'" "$@"'
}
# "pid ppid" of each live lease helper for <lock file>, which its arguments name.
lease_helpers() { ps -A -o pid=,ppid=,args= 2>/dev/null | LOCK_ARG="$1" awk 'index($0, ENVIRON["LOCK_ARG"]) { print $1, $2 }'; }
lease_free() { # lock file
  "$PY3" -c 'import fcntl, os, sys
fd = os.open(sys.argv[1], os.O_WRONLY | os.O_CREAT, 0o600)
try:
    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
except BlockingIOError:
    sys.exit(1)' "$1"
}
# Runs <runner> in its own process group and gives it <seconds> on the clock,
# so a run that hangs fails its case instead of stalling the test.
run_within() { # seconds runner args...
  local limit="$1" runner="$2" pid start="$SECONDS"; shift 2
  set -m
  ( "$runner" "$@"; exit "$RC" ) &
  pid=$!
  set +m
  while kill -0 "$pid" 2>/dev/null && [ $((SECONDS - start)) -lt "$limit" ]; do /bin/sleep 0.1; done
  if kill -0 "$pid" 2>/dev/null; then
    kill -KILL -- "-$pid" 2>/dev/null; wait "$pid" 2>/dev/null
    RC=hung; bad "the run had not ended after ${limit}s"
    return 1
  fi
  wait "$pid"; RC=$?
}

if begin laptop-detached-run-waits-for-a-slow-lease-helper; then
  # The limit is seconds on the clock. It was 1000 polls, which this stubbed
  # `sleep` spends in a few seconds; at a load of 80 they once ran out before
  # the helper's Python had started, and the run then waited for ever on it.
  laptop_setup post post; slow_lease_helper
  run_within 120 run_laptop FAKE_LEASE_HELPER_DELAY=6
  expect_rc 0; expect_out "DEPLOY OK"; expect_build "$CUR" new
  expect_log "lease helper delayed 6s"
  finish
fi

if begin laptop-lease-helper-that-never-answers-is-stopped; then
  # A detached run ignores TERM, and so does its helper: the run that gave up
  # sent TERM and waited on the helper for ever, with the lease taken once the
  # helper got to it. Now it kills the helper, ends, and changes nothing, and
  # the next run takes the lease.
  laptop_setup post post; slow_lease_helper; LOCK="$H/$LAPTOP_PLIST_REL.supervisor-mutation.lock"
  run_within 120 run_laptop FAKE_LEASE_HELPER_DELAY=600 SUBROUTER_MUTATION_LEASE_WAIT_SECONDS=3
  expect_rc 1; expect_log "lease helper delayed 600s"
  expect_out "the mutation lease helper did not answer within 3s"
  expect_out "refusing: cannot take $LOCK (exit 73). Nothing was changed."
  left=$(lease_helpers "$LOCK")
  [ -z "$left" ] && ok || bad "a lease helper outlived its run: $left"
  lease_free "$LOCK" && ok || bad "the lease is held after the run ended"
  expect_build "$CUR" old; expect_no_restart; expect_no_backup
  run_within 120 run_laptop
  expect_rc 0; expect_out "DEPLOY OK"; expect_build "$CUR" new
  finish
fi

if begin laptop-killed-run-frees-the-lease; then
  # The helper exits once the shell that started it is no longer its parent,
  # unless it adopted a rollback child, so a killed run leaves the lease free.
  # The detached run is parked in its kickstart, past the lock, and only its
  # shell is killed.
  laptop_setup post post; LOCK="$H/$LAPTOP_PLIST_REL.supervisor-mutation.lock"
  run_laptop FAKE_HOLD="$C/hold" &
  fg=$!
  n=0; until [ -e "$C/hold.held" ] || [ "$n" -ge 600 ]; do /bin/sleep 0.1; n=$((n + 1)); done
  holder=$(lease_helpers "$LOCK" | head -1); shell=${holder#* }
  if [ -z "$holder" ] || [ "$shell" -le 1 ] || lease_free "$LOCK"; then
    bad "the parked run holds no lease (helper: '${holder:-none}')"
  else
    ok; kill -KILL "$shell"
    n=0; while { [ -n "$(lease_helpers "$LOCK")" ] || ! lease_free "$LOCK"; } && [ "$n" -lt 100 ]; do /bin/sleep 0.1; n=$((n + 1)); done
    [ "$n" -lt 100 ] && ok || bad "the lease is held 10 s after the shell that took it was killed"
  fi
  : > "$C/hold"; wait "$fg"
  run_within 120 run_laptop
  expect_rc 0; expect_out "DEPLOY OK"
  finish
fi

if begin lease-release-stops-a-helper-that-ignores-term; then
  # A shell that ignores TERM, as a detached run does, waited for ever on the
  # helper it had just told to let go.
  laptop_setup post post
  run_within 60 run_sandboxed PATH="$C/bin:/usr/bin:/bin" TMPDIR="$C/tmp" LIB="$LEASE_LIB" LOCK="$C/lease.lock" \
    -- "$LAPTOP_BASH" -c 'trap "" TERM; . "$LIB"; acquire_subrouter_mutation_lease "$LOCK" || exit 9
release_subrouter_mutation_lease; echo released'
  expect_rc 0; expect_out released
  lease_free "$C/lease.lock" && ok || bad "the lease is held after its release"
  finish
fi

if begin laptop-restart-survives-a-killed-foreground; then
  laptop_setup post post; run_killable run_laptop RELOAD=1
  expect_killed_and_finished "DEPLOY OK"
  expect_loaded serve; expect_running new; expect_build "$CUR" new
  finish
fi

if begin laptop-failed-install-leaves-no-stage-file; then
  laptop_setup post post; run_laptop FAKE_SIGN_FAILS=1
  expect_rc 1; expect_out "install failed; the running binary is untouched"; expect_build "$CUR" old
  if any_exists "$H/bin/subrouter.stage."*; then bad "a staged copy was left in $H/bin"; else ok; fi
  finish
fi

if begin laptop-failed-bootstrap-rolls-back-at-once; then
  laptop_setup post post; run_laptop RELOAD=1 FAKE_BOOTSTRAP_FAILS_ONCE=1
  expect_rc 1; expect_out "failed: rc 5: Bootstrap failed: 5: Input/output error"
  expect_out "!! ROLLING BACK"; expect_out "rolled back, healthy"; expect_build "$CUR" old
  after_first_bootstrap > "$C/between"
  grep -q "_subrouter/health" "$C/between" && bad "health was polled after launchd refused the bootstrap" || ok
  finish
fi

# A script changes launchd only for a job it can load again (PATCHES §76).
if begin laptop-refuses-a-job-loaded-from-another-plist; then
  # As for a run with a home that is not the job's: launchd loaded the job from
  # another plist. Nothing changes, and both plists are named.
  laptop_setup pre post; echo "$C/other/$TEST_JOB.plist" > "$C/launchd/loaded-from"
  run_laptop
  expect_rc 1
  expect_out "refusing to change gui/$(id -u)/$TEST_JOB: launchd loaded it from $C/other/$TEST_JOB.plist, and this script's plist is $H/$LAPTOP_PLIST_REL"
  expect_build "$CUR" old; expect_no_restart; expect_no_backup
  finish
fi

if begin laptop-stops-before-kickstarting-a-job-from-another-plist; then
  # The outage: the job was the test's when the run started, and by the restart
  # launchd answered for one loaded from another plist.
  laptop_setup pre post; run_laptop FAKE_SIGN_MOVES_JOB="$C/other/$TEST_JOB.plist"
  expect_rc 1
  expect_out "refusing to kickstart gui/$(id -u)/$TEST_JOB: launchd loaded it from $C/other/$TEST_JOB.plist, and this script's plist is $H/$LAPTOP_PLIST_REL"
  expect_no_restart; expect_running old
  # The next start of the job runs the build from before this run, not the
  # new one, which nothing verified (PATCHES §76.6).
  expect_build "$CUR" old; expect_out "the files this run changed are as they were before it"
  finish
fi

if begin laptop-refusal-that-cannot-put-the-build-back-says-how; then
  # The restore's signature fails too: the run names the file that still holds
  # the new build and the command that puts it back, and that command does.
  laptop_setup pre post; run_laptop FAKE_SIGN_MOVES_JOB="$C/other/$TEST_JOB.plist" FAKE_SIGN_FAILS_AFTER=1
  expect_rc 1; expect_out "refusing to kickstart gui/$(id -u)/$TEST_JOB"
  expect_no_restart; expect_running old; expect_build "$CUR" new
  expect_out "!! $CUR still holds what this run installed, and the next start of gui/$(id -u)/$TEST_JOB would use it"
  expect_no_out "the files this run changed are as they were before it"
  bak=$(printf '%s\n' "$H/bin/subrouter.bak-"*)
  expect_line "     cp -p $bak $CUR.restore && mv -f $CUR.restore $CUR"
  undo=$(grep -E '^     cp -p ' "$C/out")
  /bin/bash -c "$undo" && ok || bad "the printed command failed: $undo"
  expect_build "$CUR" old
  finish
fi

if begin laptop-refusal-puts-the-edited-plist-back; then
  # A plist edit is a file the next start of the job would read too.
  laptop_setup post post; cp -p "$H/$LAPTOP_PLIST_REL" "$C/plist.orig"
  run_laptop PLIST_SET_ENV="SUBROUTER_CLAUDE_SOCKET=$H/.subrouter/claude.sock" FAKE_SIGN_MOVES_JOB="$C/other/$TEST_JOB.plist"
  expect_rc 1; expect_out "$H/$LAPTOP_PLIST_REL: SUBROUTER_CLAUDE_SOCKET=$H/.subrouter/claude.sock"
  expect_out "refusing to boot out gui/$(id -u)/$TEST_JOB"; expect_no_restart; expect_running old
  expect_out "the files this run changed are as they were before it"; expect_build "$CUR" old
  cmp -s "$C/plist.orig" "$H/$LAPTOP_PLIST_REL" && ok || bad "the plist on disk still holds this run's edit"
  finish
fi

if begin laptop-reload-refused-without-its-plist; then
  # Nothing could load the job again after this bootout.
  laptop_setup pre post; run_laptop RELOAD=1 FAKE_SIGN_REMOVES_PLIST=1
  expect_rc 1
  expect_out "refusing to boot out gui/$(id -u)/$TEST_JOB: $H/$LAPTOP_PLIST_REL does not exist, so launchd could not load the job again"
  expect_no_restart; expect_running old; expect_build "$CUR" old
  finish
fi

if begin laptop-plist-gone-after-the-bootout-is-not-bootstrapped; then
  # The plist goes during the drain: no bootstrap of a missing file, and the run
  # says the daemon is down.
  laptop_setup pre post; run_laptop RELOAD=1 FAKE_BOOTOUT_REMOVES_PLIST=1
  expect_rc 2
  expect_out "$H/$LAPTOP_PLIST_REL does not exist, so it was not bootstrapped"
  expect_out "STILL DOWN"; expect_no_log "launchctl bootstrap"
  finish
fi

# --- deploy-laptop.sh step 7: old backups and the ops tools (PATCHES §40) ---
seed_backups() { # era stamp...
  local era="$1" stamp; shift
  for stamp in "$@"; do make_build "$H/bin/subrouter.bak-$stamp" "$era" "bak-$stamp"; done
}
count_backups() { local n=0 f; for f in "$H/bin/subrouter.bak-"*; do [ -e "$f" ] && n=$((n + 1)); done; echo "$n"; }
expect_file() { [ -e "$1" ] && ok || bad "$(basename "$1") is gone"; }
expect_no_file() { [ -e "$1" ] && bad "$(basename "$1") is still there" || ok; }
OLD_STAMPS="20200101-000000 20200102-000000 20200103-000000 20200104-000000 20200105-000000 20200106-000000 20200107-000000 20200108-000000"

# git for the fixtures only, never reading this machine's git config.
tgit() {
  HOME="$C/home" GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null git -c user.name=test \
    -c user.email=test@example.com -c commit.gpgsign=false -c init.defaultBranch=main "$@"
}
# A throwaway repo shaped like this one -- srq at v1, then v2 on origin/main --
# with this deploy-laptop.sh inside it and the new build at bin/subrouter, run
# from the repo (v2). The links point into a second checkout, "live", at v1: a
# worktree (shares the objects and the refs, so it holds v2) or, with "clone",
# a clone taken before v2 existed.
ops_setup() { # [clone]
  G="$C/repo"
  mkdir -p "$G/ops/deploy/macos" "$G/ops/bin" "$G/deploy/macos"
  cp "$LAPTOP_SH" "$DEPLOY/macos/laptop-deploy-lib.sh" "$G/ops/deploy/macos/"
  cp "$LEASE_LIB" "$G/deploy/macos/"
  echo "srq v1" > "$G/ops/bin/srq"
  tgit -C "$G" init -q && tgit -C "$G" add -A && tgit -C "$G" commit -qm v1
  V1=$(tgit -C "$G" rev-parse HEAD)
  [ "${1:-}" = clone ] && tgit clone -q "$G" "$C/live" && tgit -C "$C/live" checkout -q --detach "$V1"
  echo "srq v2" > "$G/ops/bin/srq"
  tgit -C "$G" commit -qam v2
  V2=$(tgit -C "$G" rev-parse HEAD)
  # Merged: origin/main as a fetch would have left it.
  tgit -C "$G" update-ref refs/remotes/origin/main "$V2"
  [ "${1:-}" = clone ] || tgit -C "$G" worktree add -q --detach "$C/live" "$V1"
  ln -s "$C/live/ops/bin/srq" "$H/bin/srq"
  # The build is part of the tree it came from, as `make build` leaves it.
  mkdir -p "$G/bin"; mv "$NEWBIN" "$G/bin/subrouter"; NEWBIN="$G/bin/subrouter"
  LAPTOP_UNDER_TEST="$G/ops/deploy/macos/deploy-laptop.sh"
}
live_head() { tgit -C "$C/live" rev-parse HEAD; }

echo "== deploy-laptop.sh step 7 (under $LAPTOP_BASH)"

if begin laptop-prunes-old-backups; then
  laptop_setup post post; seed_backups post $OLD_STAMPS
  make_build "$H/bin/subrouter.bak-handmade" post handmade
  run_laptop
  expect_rc 0; expect_out "DEPLOY OK"; expect_out "backups: kept the newest 5, removed 4 (KEEP_BACKUPS=5)"
  # This run's backup and the four newest old ones; the four oldest are gone.
  [ "$(count_backups)" = 6 ] && ok || bad "$(count_backups) backups left, want 5 stamped + the hand-named one"
  expect_no_file "$H/bin/subrouter.bak-20200104-000000"; expect_file "$H/bin/subrouter.bak-20200105-000000"
  # A name this script never writes is not its to prune.
  expect_file "$H/bin/subrouter.bak-handmade"
  if any_exists "$H/bin/subrouter.bak-$(date +%Y)"*; then ok; else bad "this run's backup was pruned"; fi
  finish
fi

if begin laptop-prune-keeps-this-runs-backup; then
  # A future-dated name sorts first. KEEP_BACKUPS=1 keeps it AND this run's.
  laptop_setup post post; seed_backups post $OLD_STAMPS 29991231-000000
  run_laptop KEEP_BACKUPS=1
  expect_rc 0; expect_out "kept the newest 2, removed 8"
  expect_file "$H/bin/subrouter.bak-29991231-000000"
  if any_exists "$H/bin/subrouter.bak-$(date +%Y)"*; then ok; else bad "this run's backup was pruned"; fi
  finish
fi

if begin laptop-keep-backups-zero-prunes-nothing; then
  laptop_setup post post; seed_backups post $OLD_STAMPS
  run_laptop KEEP_BACKUPS=0
  expect_rc 0; expect_out "nothing pruned (KEEP_BACKUPS=0"
  [ "$(count_backups)" = 9 ] && ok || bad "$(count_backups) backups left, want all 9"
  finish
fi

if begin laptop-keep-backups-near-misses-prune-nothing; then
  # 00 is 0 to the operator but not to a `0)` pattern; a count too big for
  # `[` makes the comparison error, and an error must not fall through to rm.
  # Each run backs up to bak-<this second>, so two runs in one second share a
  # name and the second overwrites the first (on the CI runner the counts came
  # up one short). Each run's backup moves to its own old stamp before the
  # next run, so the counts do not depend on the clock.
  restamp_run_backup() { # stamp
    local f
    for f in "$H/bin/subrouter.bak-"*; do
      case "$f" in *.bak-2020*|*.bak-2021*) ;; *) mv "$f" "$H/bin/subrouter.bak-$1" ;; esac
    done
  }
  laptop_setup post post; seed_backups post $OLD_STAMPS
  run_laptop KEEP_BACKUPS=00
  expect_rc 0; expect_out "nothing pruned (KEEP_BACKUPS=00"
  [ "$(count_backups)" = 9 ] && ok || bad "KEEP_BACKUPS=00 left $(count_backups) backups, want all 9"
  restamp_run_backup 20210101-000000
  run_laptop KEEP_BACKUPS=99999999999999999999
  expect_rc 0; expect_out "nothing pruned (KEEP_BACKUPS=99999999999999999999"
  [ "$(count_backups)" = 10 ] && ok || bad "a huge KEEP_BACKUPS left $(count_backups) backups, want all 10"
  restamp_run_backup 20210102-000000
  run_laptop KEEP_BACKUPS=007
  expect_rc 0; expect_out "kept the newest 7, removed 4 (KEEP_BACKUPS=007)"
  finish
fi

if begin laptop-rollback-prunes-nothing; then
  # A deploy that did not verify keeps every backup it found.
  laptop_setup post post; seed_backups post $OLD_STAMPS
  run_laptop FAKE_BAD=accounts-404
  expect_rc 1; expect_out "!! ROLLING BACK"; expect_no_out "== 7."
  [ "$(count_backups)" = 9 ] && ok || bad "$(count_backups) backups left, want all 9"
  finish
fi

if begin laptop-check-prunes-nothing; then
  laptop_setup post post; seed_backups post $OLD_STAMPS
  run_laptop -- "$NEWBIN" --check
  expect_rc 0; expect_out "CHECK OK"; expect_no_out "backups:"
  [ "$(count_backups)" = 8 ] && ok || bad "$(count_backups) backups left, want all 8"
  finish
fi

if begin laptop-names-pre-isolation-binaries; then
  # Past the first migration, a pre-isolation binary in ~/bin is only a hazard.
  laptop_setup post post; seed_record "$H/.subrouter" a@x isolated-server-login
  make_build "$H/bin/subrouter.fable51" pre fable51
  run_laptop
  expect_rc 0; expect_out "!! pre-isolation binaries beside $H/bin/subrouter: subrouter.fable51"
  expect_out "1 Codex record(s) in $H/.subrouter/codex/accounts carry oauthCredentialOrigin"
  expect_out "rollback boundary (upstream docs/upgrades.md)"
  expect_file "$H/bin/subrouter.fable51"
  finish
fi

if begin laptop-pre-isolation-backup-quiet-before-migration; then
  # The first crossing: this run's backup is pre-isolation and IS the rollback
  # target until a record is migrated. Calling it a hazard would be wrong.
  laptop_setup pre post; run_laptop
  expect_rc 0; expect_out "DEPLOY OK"; expect_no_out "!! pre-isolation binaries"
  finish
fi

if begin laptop-refreshes-stale-ops-checkout; then
  laptop_setup post post; ops_setup; run_laptop
  expect_rc 0; expect_out "DEPLOY OK"
  expect_out "$H/bin/srq: STALE -- runs from $C/live at ${V1:0:9}; this deploy is ${V2:0:9}"
  expect_out "refreshed: $C/live ${V1:0:9} -> ${V2:0:9}"
  [ "$(live_head)" = "$V2" ] && ok || bad "live is at $(live_head), want $V2"
  grep -qx "srq v2" "$H/bin/srq" && ok || bad "srq did not refresh"
  finish
fi

if begin laptop-ops-checkout-on-branch-not-moved; then
  laptop_setup post post; ops_setup; tgit -C "$C/live" checkout -q -b tools
  run_laptop
  expect_rc 0; expect_out "not refreshed ($C/live is on branch tools, and a branch is never moved)"
  # Not advice to detach the branch it just refused to move.
  expect_out "git -C $C/live pull --ff-only"; expect_no_out "checkout --detach"
  [ "$(live_head)" = "$V1" ] && ok || bad "a branch checkout was moved"
  finish
fi

if begin laptop-ops-checkout-dirty-not-moved; then
  laptop_setup post post; ops_setup; echo "local edit" >> "$C/live/ops/bin/srq"
  run_laptop
  expect_rc 0; expect_out "not refreshed ($C/live has local changes)"
  [ "$(live_head)" = "$V1" ] && ok || bad "a dirty checkout was moved"
  grep -q "local edit" "$C/live/ops/bin/srq" && ok || bad "the local edit was lost"
  finish
fi

if begin laptop-ops-checkout-missing-commit-not-moved; then
  laptop_setup post post; ops_setup clone; run_laptop
  expect_rc 0; expect_out "not refreshed ($C/live does not have ${V2:0:9})"
  expect_out "git -C $C/live fetch origin && git -C $C/live checkout --detach $V2"
  [ "$(live_head)" = "$V1" ] && ok || bad "live moved without the commit"
  finish
fi

if begin laptop-ops-local-commit-not-orphaned; then
  # Committed local work on the detached HEAD: clean to `status`, and a
  # checkout would leave the commit on no branch.
  laptop_setup post post; ops_setup
  echo "tools fix" >> "$C/live/ops/bin/srq"; tgit -C "$C/live" commit -qam local
  L=$(live_head)
  run_laptop
  expect_rc 0; expect_out "not refreshed (its HEAD is not an ancestor of ${V2:0:9}"
  [ "$(live_head)" = "$L" ] && ok || bad "live moved off its own commit"
  finish
fi

if begin laptop-ops-never-moves-backwards; then
  # The tools are ahead of the tree being deployed (an older main).
  laptop_setup post post; ops_setup
  tgit -C "$C/live" checkout -q --detach "$V2"; tgit -C "$G" checkout -q --detach "$V1"
  run_laptop
  expect_rc 0; expect_out "$H/bin/srq: STALE -- runs from $C/live at ${V2:0:9}; this deploy is ${V1:0:9}"
  expect_out "that would move it backwards"
  [ "$(live_head)" = "$V2" ] && ok || bad "live moved backwards to $(live_head)"
  finish
fi

if begin laptop-ops-unmerged-commit-not-moved; then
  # A lane's own commit: the shared worktree holds it, origin/main does not.
  laptop_setup post post; ops_setup
  echo "srq v3" > "$G/ops/bin/srq"; tgit -C "$G" commit -qam v3
  V3=$(tgit -C "$G" rev-parse HEAD)
  run_laptop
  expect_rc 0; expect_out "not refreshed (${V3:0:9} is not on origin/main"
  [ "$(live_head)" = "$V1" ] && ok || bad "live moved to an unmerged commit"
  finish
fi

if begin laptop-ops-compares-with-the-binarys-tree; then
  # Run from the tools checkout itself, the script's own tree is the stale
  # one: compared with itself, every link would read current.
  laptop_setup post post; ops_setup
  LAPTOP_UNDER_TEST="$C/live/ops/deploy/macos/deploy-laptop.sh"
  run_laptop
  expect_rc 0; expect_no_out "$H/bin/srq: current"
  expect_out "$H/bin/srq: STALE -- runs from $C/live at ${V1:0:9}; this deploy is ${V2:0:9}"
  expect_out "not refreshed (this script is running from $C/live)"
  [ "$(live_head)" = "$V1" ] && ok || bad "moved the checkout the script was running from"
  finish
fi

if begin laptop-ops-binary-outside-a-checkout; then
  # No tree to compare with but this script's own, and it says so.
  laptop_setup post post; ops_setup
  mkdir -p "$C/elsewhere"; mv "$NEWBIN" "$C/elsewhere/subrouter"; NEWBIN="$C/elsewhere/subrouter"
  run_laptop
  expect_rc 0; expect_out "$NEWBIN is not in a checkout of this repo, so the tools are compared with this script's tree"
  expect_out "refreshed: $C/live ${V1:0:9} -> ${V2:0:9}"
  finish
fi

if begin laptop-ops-check-reports-only; then
  laptop_setup post post; ops_setup; run_laptop -- "$NEWBIN" --check
  expect_rc 0; expect_out "CHECK OK"; expect_out "$H/bin/srq: STALE"
  expect_out "not refreshed (--check touches nothing)"
  [ "$(live_head)" = "$V1" ] && ok || bad "--check moved a checkout"
  finish
fi

if begin laptop-ops-current-and-broken; then
  laptop_setup post post; ops_setup; tgit -C "$C/live" checkout -q --detach "$V2"
  run_laptop
  expect_rc 0; expect_out "$H/bin/srq: current"; expect_no_out "STALE"
  rm "$H/bin/srq"; ln -s "$C/nowhere/bin/srq" "$H/bin/srq"
  run_laptop
  expect_rc 0; expect_out "$H/bin/srq: BROKEN link"; expect_no_out "STALE"
  finish
fi

if begin laptop-ops-copied-srq; then
  # A copy is in no checkout, so no deploy can move it.
  laptop_setup post post; ops_setup
  rm "$H/bin/srq"; cp "$C/live/ops/bin/srq" "$H/bin/srq"
  run_laptop
  expect_rc 0; expect_out "$H/bin/srq: STALE, and not in a git checkout"
  expect_line "      ln -sfn <a checkout you keep current>/ops/bin/srq $H/bin/srq"
  finish
fi

if begin laptop-ops-srq-on-path; then
  # The srq that runs is the one on PATH, wherever that is.
  laptop_setup post post; ops_setup; ln -s "$C/live/ops/bin/srq" "$C/bin/srq"
  run_laptop
  expect_rc 0; expect_out "refreshed: $C/live ${V1:0:9} -> ${V2:0:9}"
  expect_out "$C/bin/srq: current"
  finish
fi

if begin laptop-ops-link-into-another-repo; then
  # srq from a checkout of another repository. That checkout never holds
  # this repo's commits.
  other_repo() { # dir path-of-srq-in-it
    mkdir -p "$(dirname "$1/$2")"; echo "srq from $1" > "$1/$2"
    tgit -C "$1" init -q && tgit -C "$1" add -A && tgit -C "$1" commit -qm other
    tgit -C "$1" checkout -q --detach
  }
  expect_relinked() { # dir
    expect_out "$C/bin/srq: STALE -- runs from another repository, $1"
    expect_line "      ln -sfn $G/ops/bin/srq $C/bin/srq"
    expect_no_out "fetch origin"; expect_no_out "checkout --detach"
    [ "$(tgit -C "$1" rev-parse HEAD)" = "$A" ] && ok || bad "moved $1"
  }
  laptop_setup post post; ops_setup; rm "$H/bin/srq"
  other_repo "$C/archive" tools/bin/srq; A=$(tgit -C "$C/archive" rev-parse HEAD)
  ln -s "$C/archive/tools/bin/srq" "$C/bin/srq"
  run_laptop
  expect_rc 0; expect_out "DEPLOY OK"; expect_relinked "$C/archive"
  # The same path as here is not enough: this repo has more under ops/.
  other_repo "$C/lookalike" ops/bin/srq; A=$(tgit -C "$C/lookalike" rev-parse HEAD)
  ln -sfn "$C/lookalike/ops/bin/srq" "$C/bin/srq"
  run_laptop
  expect_rc 0; expect_relinked "$C/lookalike"
  finish
fi

if begin laptop-ops-binary-in-another-repo; then
  # A repo with an ops/ of its own is not this repo, so the tools are
  # compared with this script's tree, as for a binary outside any checkout.
  laptop_setup post post; ops_setup
  mkdir -p "$C/other/ops/bin" "$C/other/bin"; echo "srq from other" > "$C/other/ops/bin/srq"
  tgit -C "$C/other" init -q && tgit -C "$C/other" add -A && tgit -C "$C/other" commit -qm other
  mv "$NEWBIN" "$C/other/bin/subrouter"; NEWBIN="$C/other/bin/subrouter"
  run_laptop
  expect_rc 0; expect_out "$NEWBIN is not in a checkout of this repo, so the tools are compared with this script's tree"
  expect_out "refreshed: $C/live ${V1:0:9} -> ${V2:0:9}"
  finish
fi

write_supervised_plist() { # path home [env assignments...]
  "$PY3" - "$@" <<'PY'
import os, plistlib, sys
path, home = sys.argv[1], sys.argv[2]
env = {"HOME": home}
env.update(kv.split("=", 1) for kv in sys.argv[3:])
with open(path, "wb") as f:
    plistlib.dump({"Label": os.environ["TEST_JOB"],
                   "ProgramArguments": [home + "/bin/subrouter-supervisor", "supervise",
                                        "--addr", os.environ["TEST_ADDR"],
                                        "--control-socket", home + "/.subrouter/supervisor.sock",
                                        "--worker-bin", home + "/bin/subrouter",
                                        "--worker-stop-grace", "2m", "--ready-timeout", "2m", "--drain-timeout", "90s",
                                        "--worker-config", home + "/.subrouter/worker-config.json",
                                        "--", "--transcripts", home + "/.subrouter/transcripts"],
                   "EnvironmentVariables": env, "ExitTimeOut": 120, "ThrottleInterval": 10,
                   "KeepAlive": True, "RunAtLoad": True}, f)
PY
}
sup_setup() { # old-era new-era [plist env assignments...]
  local old="$1" new="$2"; shift 2
  laptop_setup "$old" "$new"
  make_build "$H/bin/subrouter-supervisor" "$old" sup-old
  write_supervised_plist "$H/$LAPTOP_PLIST_REL" "$H" "$@"
  load_plist "$H/$LAPTOP_PLIST_REL"
  echo sup-old > "$C/launchd/supervisor"
}
count_upgrades() { grep -c "unix .*supervisor.sock POST .*/_subrouter/upgrade" "$C/log"; }

echo "== deploy-laptop.sh, supervised (under $LAPTOP_BASH)"

if begin sup-upgrade-ok; then
  sup_setup post post; run_laptop
  expect_rc 0; expect_out "DEPLOY OK"; expect_build "$CUR" new
  expect_out "== 4. upgrade the worker (POST /_subrouter/upgrade) =="
  expect_out "active worker              -> new"
  expect_log "upgrade -> new"; expect_no_restart
  expect_build "$H/bin/subrouter-supervisor" sup-old
  finish
fi

if begin sup-upgrade-refused-restores-binary; then
  sup_setup post post; run_laptop FAKE_UPGRADE_FAILS=new
  expect_rc 1; expect_out "upgrade answered HTTP 502: worker readiness timed out"
  expect_out "the supervisor kept the old worker"; expect_no_out "ROLLING BACK"
  expect_build "$CUR" old; expect_no_restart; expect_running old
  finish
fi

if begin sup-verify-regressed-rolls-back-by-upgrade; then
  sup_setup post post; run_laptop FAKE_BAD=accounts-404
  expect_rc 1; expect_out "!! ROLLING BACK"; expect_out "rolled back, healthy"
  expect_build "$CUR" old; expect_no_restart; expect_running old
  [ "$(count_upgrades)" = 2 ] && ok || bad "$(count_upgrades) upgrades, want 2 (the deploy, then the rollback)"
  finish
fi

if begin sup-upgrade-timeout-after-the-switch-is-kept; then
  # The supervisor switched, and the client gave up first (curl 000): the
  # deploy restored the backup over the build that serves.
  sup_setup post post; run_laptop FAKE_UPGRADE_TIMES_OUT=1
  expect_rc 0; expect_out "upgrade answered HTTP 000"; expect_out "supervisor-status: active worker new"
  expect_out "DEPLOY OK"; expect_build "$CUR" new; expect_running new; expect_no_out "ROLLING BACK"
  finish
fi

if begin sup-upgrade-timeout-without-the-switch-restores; then
  sup_setup post post; run_laptop FAKE_UPGRADE_TIMES_OUT=1 FAKE_UPGRADE_FAILS=new
  expect_rc 1; expect_out "upgrade answered HTTP 000"; expect_out "the supervisor kept the old worker"
  expect_build "$CUR" old; expect_running old; expect_no_restart
  finish
fi

if begin sup-upgrade-unknown-worker-restores; then
  sup_setup post post; run_laptop FAKE_UPGRADE_TIMES_OUT=1 FAKE_SV_STATUS_DOWN=1
  expect_rc 1; expect_out "supervisor-status: active worker no answer"
  expect_out "names neither build as the active worker"; expect_build "$CUR" old; expect_no_restart
  finish
fi

if begin sup-same-build-refused-upgrade-fails; then
  # A redeploy of the installed build, as for a worker-config change: both
  # CDHashes are the same, so the active identity cannot show an upgrade.
  sup_setup post post; run_laptop FAKE_UPGRADE_FAILS=old -- "$CUR"
  expect_rc 1; expect_out "upgrade answered HTTP 502"; expect_out "the supervisor kept the old worker"
  expect_no_out "DEPLOY OK"; expect_build "$CUR" old; expect_no_restart
  finish
fi

if begin sup-upgrade-survives-a-killed-foreground; then
  sup_setup post post; run_killable run_laptop
  expect_killed_and_finished "DEPLOY OK"
  expect_running new; expect_build "$CUR" new; expect_no_restart
  finish
fi

if begin sup-never-ready-upgrade-survives-a-killed-foreground; then
  # Killed in the calling shell, this left the never-ready build at
  # --worker-bin while the old worker served; the supervisor rebuilds from
  # that path when its worker exits, and launchd then crash-loops it.
  sup_setup post post; run_killable run_laptop FAKE_UPGRADE_FAILS=new
  [ "$FG_RC" = 137 ] && ok || bad "the foreground exited $FG_RC, want 137 (SIGKILL)"
  expect_log "SIGKILL the foreground process group"
  [ "$TRANSITION_RC" = 1 ] && ok || bad "the detached run exited '$TRANSITION_RC', want 1"
  grep -Fq "the supervisor kept the old worker" "$TRANSITION_LOG" && ok || bad "the detached run's log lacks the kept-old verdict"
  expect_build "$CUR" old; expect_running old; expect_no_restart
  finish
fi

if begin sup-wrong-generation-rolls-back; then
  sup_setup post post; run_laptop FAKE_UPGRADE_NOOP=new
  expect_rc 1; expect_out "the supervisor's active worker is not $CUR"
  expect_out "!! ROLLING BACK"; expect_build "$CUR" old; expect_no_restart
  finish
fi

if begin sup-reload-refused; then
  sup_setup post post; run_laptop RELOAD=1
  expect_rc 1; expect_out "RELOAD=1, and every plist edit, restarts the job"; expect_out "SUBROUTER_CLAUDE_SOCKET"
  expect_out "ExitTimeOut (120s)"; expect_out "--restart-supervisor"; expect_out "Nothing was changed"
  expect_build "$CUR" old; expect_no_restart; expect_no_log "upgrade"; expect_no_backup
  finish
fi

if begin sup-restart-supervisor; then
  sup_setup post post; run_laptop -- "$NEWBIN" --restart-supervisor
  expect_rc 0; expect_out "DEPLOY OK"; expect_out "== 4. restart the supervisor =="
  expect_out "the port closes now"; expect_out "ExitTimeOut (120s)"
  expect_build "$CUR" new; expect_build "$H/bin/subrouter-supervisor" new
  expect_order "launchctl bootout" "launchctl bootstrap"; expect_no_log "launchctl kickstart"
  expect_no_log "upgrade"
  if any_exists "$H/bin/subrouter-supervisor.bak-"*; then ok; else bad "no supervisor backup"; fi
  finish
fi

if begin sup-restart-supervisor-rolls-back-both; then
  sup_setup post post; run_laptop FAKE_BAD=accounts-404 -- "$NEWBIN" --restart-supervisor
  expect_rc 1; expect_out "!! ROLLING BACK"; expect_out "rolled back, healthy"
  expect_build "$CUR" old; expect_build "$H/bin/subrouter-supervisor" sup-old
  finish
fi

if begin sup-restart-supervisor-refusal-puts-both-builds-back; then
  sup_setup post post; run_laptop FAKE_SIGN_MOVES_JOB="$C/other/$TEST_JOB.plist" -- "$NEWBIN" --restart-supervisor
  expect_rc 1; expect_out "refusing to boot out gui/$(id -u)/$TEST_JOB"; expect_no_restart
  expect_out "the files this run changed are as they were before it"
  expect_build "$CUR" old; expect_build "$H/bin/subrouter-supervisor" sup-old
  finish
fi

if begin sup-restart-supervisor-refused-unsupervised; then
  laptop_setup post post; run_laptop -- "$NEWBIN" --restart-supervisor
  expect_rc 1; expect_out "does not run supervise"; expect_build "$CUR" old
  expect_no_restart
  finish
fi

if begin sup-refuses-build-without-supervised-socket; then
  sup_setup post pre; run_laptop
  expect_rc 1; expect_out "has no supervisor-owned Claude socket"; expect_out "Nothing was changed"
  expect_build "$CUR" old; expect_no_restart; expect_no_log "upgrade"
  finish
fi

if begin sup-refuses-a-build-from-main; then
  sup_setup post main; run_laptop
  expect_rc 1; expect_out "$NEWBIN has no supervisor-owned Claude socket (it lacks owned_by_supervisor)"
  expect_out "Build it from this repo; a build of upstream subrouter cannot run this job"; expect_out "Nothing was changed"
  expect_build "$CUR" old; expect_no_restart; expect_no_log "upgrade"; expect_no_backup
  finish
fi

if begin sup-restart-supervisor-refuses-a-build-from-main; then
  sup_setup post main; run_laptop -- "$NEWBIN" --restart-supervisor
  expect_rc 1; expect_out "$NEWBIN has no supervisor-owned Claude socket (it lacks owned_by_supervisor)"
  expect_out "Nothing was changed"
  expect_build "$CUR" old; expect_build "$H/bin/subrouter-supervisor" sup-old; expect_no_restart; expect_no_backup
  finish
fi

if begin sup-check-ok; then
  sup_setup post post; run_laptop -- "$CUR" --check
  expect_rc 0; expect_out "CHECK OK"; expect_out "active worker              -> old"
  expect_no_log "upgrade"; expect_no_restart
  finish
fi

if begin sup-check-wrong-generation; then
  sup_setup post post; echo stale > "$C/launchd/running"; run_laptop -- "$CUR" --check
  expect_rc 1; expect_out "CHECK FAILED"; expect_out "the supervisor's active worker is not $CUR"
  finish
fi

if begin sup-claude-socket-through-the-supervisor; then
  sup_setup post post SUBROUTER_CLAUDE_SOCKET="$C/home/.subrouter/claude.sock"; run_laptop
  expect_rc 0; expect_out "claude socket              -> serving"
  expect_log "curl unix $C/home/.subrouter/claude.sock GET http://api.anthropic.com/_subrouter/health"
  finish
fi

if begin sup-claude-socket-down-names-the-supervisor-state; then
  sup_setup post post SUBROUTER_CLAUDE_SOCKET="$C/home/.subrouter/other.sock"
  run_laptop FAKE_SV_SOCKET=unavailable
  expect_rc 3; expect_out "supervisor says: unavailable"; expect_out "DEPLOY INCOMPLETE (claude socket not served)"
  finish
fi

# The words from before PATCHES §84 keep their meaning. A supervisor that says
# `stopped` is read as before: an answer at the path is the daemon serving it,
# and silence there is a socket not served. A supervisor from before
# supervisor-status carried `claude_socket` names no word, and an answer at
# the path is serving.
if begin sup-claude-socket-stopped-and-answering-is-serving; then
  sup_setup post post SUBROUTER_CLAUDE_SOCKET="$C/home/.subrouter/claude.sock"
  run_laptop FAKE_SV_SOCKET=stopped
  expect_rc 0; expect_out "claude socket              -> serving"; expect_out "DEPLOY OK"
  expect_no_out "not owned by the daemon"; expect_build "$CUR" new
  finish
fi

if begin sup-claude-socket-stopped-and-silent-is-not-served; then
  sup_setup post post SUBROUTER_CLAUDE_SOCKET="$C/home/.subrouter/other.sock"
  run_laptop FAKE_SV_SOCKET=stopped
  expect_rc 3; expect_out "claude socket              -> down:no health answer on $H/.subrouter/other.sock (supervisor says: stopped)"
  expect_out "DEPLOY INCOMPLETE (claude socket not served)"; expect_no_out "not owned by the daemon"
  finish
fi

if begin sup-claude-socket-from-a-supervisor-without-the-word; then
  sup_setup post post SUBROUTER_CLAUDE_SOCKET="$C/home/.subrouter/claude.sock"
  : > "$C/launchd/supervisor-lacks-socket"
  run_laptop
  [ -e "$C/launchd/supervisor-lacks-socket" ] && ok || bad "the supervisor started again and now names the word"
  expect_rc 0; expect_out "claude socket              -> serving"; expect_out "DEPLOY OK"
  expect_build "$CUR" new
  finish
fi

# Answering at the path is not owning it (PATCHES §84). A stand-in answers
# health there with "ok": true while the daemon is away, and a supervisor can
# serve TCP while it waits for the socket's lock, finds a listener it does not
# own at the path, or has let go of the path. None of these is DEPLOY OK, and
# none is a rollback: the build is kept and nothing more is restarted.
no_owner_kept() { # the state word
  expect_rc 3; expect_out "the daemon is not serving its Claude socket"
  expect_out "DEPLOY INCOMPLETE (claude socket not owned by the daemon: $1)"
  expect_no_out "DEPLOY OK"; expect_no_out "ROLLING BACK"; expect_build "$CUR" new
}

if begin sup-stand-in-at-the-claude-socket-is-not-the-daemon; then
  sup_setup post post SUBROUTER_CLAUDE_SOCKET="$C/home/.subrouter/claude.sock"
  run_laptop FAKE_CLAUDE_WORD=standby FAKE_SV_SOCKET=waiting
  no_owner_kept standby; expect_out "claude-socket=standby"
  expect_out "claude socket              -> standby:a stand-in answers on $H/.subrouter/claude.sock, not the daemon. The supervisor is asking for the path back"
  expect_no_restart
  [ "$(count_upgrades)" = 1 ] && ok || bad "$(count_upgrades) upgrades, want 1"
  finish
fi

if begin sup-check-stand-in-the-supervisor-does-not-ask-back; then
  sup_setup post post SUBROUTER_CLAUDE_SOCKET="$C/home/.subrouter/claude.sock"
  run_laptop FAKE_CLAUDE_WORD=standby FAKE_SV_SOCKET=unavailable -- "$CUR" --check
  expect_rc 3; expect_out "CHECK INCOMPLETE (claude socket not owned by the daemon: standby)"; expect_no_out "CHECK OK"
  expect_out "the supervisor is not asking for the path back (supervisor says: unavailable)"
  finish
fi

if begin sup-claude-socket-waiting-is-not-deploy-ok; then
  sup_setup post post SUBROUTER_CLAUDE_SOCKET="$C/home/.subrouter/claude.sock"
  run_laptop FAKE_CLAUDE_WORD=serving FAKE_SV_SOCKET=waiting
  no_owner_kept waiting
  expect_out "waiting:another process holds $H/.subrouter/claude.sock.lock, and the supervisor is asking for it; another process answers at the path"
  expect_out "lsof $H/.subrouter/claude.sock.lock"
  finish
fi

if begin sup-claude-socket-displaced-is-not-deploy-ok; then
  sup_setup post post SUBROUTER_CLAUDE_SOCKET="$C/home/.subrouter/claude.sock"
  run_laptop FAKE_CLAUDE_WORD=serving FAKE_SV_SOCKET=displaced
  no_owner_kept displaced
  expect_out "displaced:a listener the supervisor does not own answers on $H/.subrouter/claude.sock"
  expect_out "lsof $H/.subrouter/claude.sock"
  finish
fi

if begin sup-claude-socket-yielded-says-until-when; then
  sup_setup post post SUBROUTER_CLAUDE_SOCKET="$C/home/.subrouter/claude.sock"
  run_laptop FAKE_CLAUDE_WORD=standby FAKE_SV_SOCKET=yielded FAKE_SV_YIELD="operator 2026-11-21T23:30:00Z"
  no_owner_kept yielded
  expect_out "yielded:the supervisor let go of $H/.subrouter/claude.sock on purpose (an operator asked) until 2026-11-21T23:30:00Z; a stand-in answers at the path. Wait for the yield to end, or end it"
  # Nothing answers at the path, and the workers' exits were the reason.
  run_laptop FAKE_CLAUDE_SOCKET=/nonexistent/claude.sock FAKE_SV_SOCKET=yielded FAKE_SV_YIELD="worker_exits 2026-11-21T23:40:00Z" -- "$CUR" --check
  expect_rc 3; expect_out "CHECK INCOMPLETE (claude socket not owned by the daemon: yielded)"
  expect_out "on purpose (its workers kept exiting) until 2026-11-21T23:40:00Z; nothing answers at the path"
  finish
fi

if begin laptop-stand-in-at-the-claude-socket-is-not-the-daemon; then
  # A plain serve: its restart is the window a stand-in takes the path in.
  laptop_setup pre post
  "$PY3" -c 'import plistlib, sys; p = plistlib.load(open(sys.argv[1], "rb")); p["EnvironmentVariables"]["SUBROUTER_CLAUDE_SOCKET"] = sys.argv[2]; plistlib.dump(p, open(sys.argv[1], "wb"))' \
    "$H/Library/LaunchAgents/$TEST_JOB.plist" "$H/.subrouter/claude.sock"
  load_plist "$H/Library/LaunchAgents/$TEST_JOB.plist"
  run_laptop FAKE_CLAUDE_WORD=standby FAKE_DAEMON_SOCKET_WORD=waiting
  no_owner_kept standby
  expect_out "standby:a stand-in answers on $H/.subrouter/claude.sock, not the daemon. The daemon is asking for the path back"
  finish
fi

if begin sup-restart-supervisor-refused-outside-the-gui-session; then
  sup_setup post post; run_laptop FAKE_MANAGER=Background -- "$NEWBIN" --restart-supervisor
  expect_rc 1; expect_out "launchd's 'Background' session, not the logged-in GUI (Aqua) session"
  expect_out "Nothing was changed"
  expect_build "$CUR" old; expect_build "$H/bin/subrouter-supervisor" sup-old; expect_no_restart; expect_no_backup
  finish
fi

if begin sup-upgrade-runs-outside-the-gui-session; then
  sup_setup post post; run_laptop FAKE_MANAGER=Background
  expect_rc 0; expect_out "DEPLOY OK"; expect_build "$CUR" new; expect_no_restart
  finish
fi

if begin sup-upgrade-refused-from-an-ended-session; then
  sup_setup post post; dead_launchd; run_laptop
  expect_rc 1; expect_out "this shell cannot reach launchd (launchctl managername: rc 153"
  expect_out "Nothing was changed"; expect_build "$CUR" old; expect_no_log "upgrade"; expect_no_backup
  finish
fi

if begin sup-not-loaded-bootstraps-before-upgrading; then
  sup_setup post post; unload_job
  run_laptop
  expect_rc 0; expect_out "launchd has not loaded gui/"; expect_out "loaded, and answering"
  expect_out "DEPLOY OK"; expect_log "upgrade -> new"; expect_no_log "launchctl (kickstart|bootout)"
  grep -q "<string>supervise</string>" "$C/launchd/loaded" && ok || bad "launchd does not run the supervised plist"
  finish
fi

if begin sup-isolation-gate-reads-the-worker-config-env; then
  sup_setup post post
  mkdir -p "$H/alt-state"; echo 2 > "$H/alt-state/fake-codex-needing-migration"
  printf '{"args": [], "env": {"SUBROUTER_STATE_DIR": "%s"}}\n' "$H/alt-state" > "$H/.subrouter/worker-config.json"
  run_laptop
  expect_rc 3; expect_out "2 Codex account(s) fail"
  expect_log "args=codex isolation-check --json sudo_user=none HOME=[^ ]*/home STATE=[^ ]*/home/alt-state CODEX_HOME=<unset>"
  expect_out "SUBROUTER_STATE_DIR=$H/alt-state $H/bin/subrouter codex attest-legacy"
  finish
fi

if begin sup-reads-the-loaded-job-not-the-plist; then
  sup_setup post post
  "$PY3" - "$H/$LAPTOP_PLIST_REL" <<'PY'
import plistlib, sys
with open(sys.argv[1], "rb") as f:
    p = plistlib.load(f)
a = p["ProgramArguments"]
a[a.index("--control-socket") + 1] = "/nonexistent/next.sock"
a[a.index("--worker-bin") + 1] = "/nonexistent/next-worker"
with open(sys.argv[1], "wb") as f:
    plistlib.dump(p, f)
PY
  run_laptop
  expect_rc 0; expect_out "DEPLOY OK"; expect_build "$CUR" new; expect_log "upgrade -> new"
  expect_no_log "nonexistent"
  finish
fi

if begin sup-upgrade-refused-while-the-mutation-lock-is-held; then
  sup_setup post post; hold_lock "$H/$LAPTOP_PLIST_REL"; run_laptop; release_lock
  expect_lock_refusal "$H/$LAPTOP_PLIST_REL"; expect_build "$CUR" old; expect_no_log "upgrade"
  finish
fi

if begin sup-check-runs-while-the-mutation-lock-is-held; then
  sup_setup post post
  hold_lock "$H/$LAPTOP_PLIST_REL"; run_laptop -- "$CUR" --check; release_lock
  expect_rc 0; expect_out "CHECK OK"
  finish
fi

if begin sup-restart-supervisor-survives-a-killed-foreground; then
  sup_setup post post; run_killable run_laptop -- "$NEWBIN" --restart-supervisor
  expect_killed_and_finished "DEPLOY OK"
  expect_loaded supervised; expect_running new; expect_build "$H/bin/subrouter-supervisor" new
  finish
fi

if begin sup-loaded-supervisor-plist-says-serve-refused; then
  sup_setup post post
  write_serve_plist "$H/$LAPTOP_PLIST_REL" "$H"
  run_laptop
  expect_rc 1; expect_out "launchd runs gui/$(id -u)/$TEST_JOB under supervise, but $H/$LAPTOP_PLIST_REL no longer does"
  expect_out "activate-laptop-supervisor.sh --rollback"; expect_out "Nothing was changed"
  expect_build "$CUR" old; expect_no_restart; expect_no_log "upgrade"
  finish
fi

if begin sup-loaded-serve-plist-says-supervise-refused; then
  laptop_setup post post
  make_build "$H/bin/subrouter-supervisor" post sup-old
  write_supervised_plist "$H/$LAPTOP_PLIST_REL" "$H"
  run_laptop
  expect_rc 1; expect_out "as a plain serve, but $H/$LAPTOP_PLIST_REL runs supervise"
  expect_out "activate-laptop-supervisor.sh --rollback, then activate again"
  expect_build "$CUR" old; expect_no_restart; expect_no_log "upgrade"
  finish
fi

# --- rollback-laptop-peer.sh --------------------------------------------------
peer_setup() { # current-era backup-era
  laptop_setup "$1" "$1"
  PEER_BK="$H/.subrouter/laptop-cutover-backup-20260816"
  mkdir -p "$PEER_BK"
  make_build "$PEER_BK/subrouter.bak" "$2" cutover
  cp "$H/Library/LaunchAgents/$TEST_JOB.plist" "$PEER_BK/plist.bak"
}
run_peer() { # extra env assignments... [-- script args]
  local envs=()
  while [ $# -gt 0 ] && [ "$1" != -- ]; do envs+=("$1"); shift; done
  [ $# -gt 0 ] && shift
  run_sandboxed PATH="$C/bin:/usr/bin:/bin:/usr/sbin:/sbin" HOME="$H" TMPDIR="$C/tmp" \
    LAPTOP_BACKUP_PTR="$C/no-such-pointer" \
    FAKE_LOG="$C/log" FAKE_LAUNCHD="$C/launchd" FAKE_RUNNING="$C/launchd/running" \
    FAKE_DAEMON="$C/launchd/running" FAKE_DEST="$CUR" FAKE_NEW_ID=new \
    FAKE_PLIST="$H/Library/LaunchAgents/$TEST_JOB.plist" ${envs[@]+"${envs[@]}"} \
    -- "$LAPTOP_BASH" "$DEPLOY/macos/rollback-laptop-peer.sh" "$@"
}

echo "== rollback-laptop-peer.sh"

if begin peer-rollback-refused-pre-isolation; then
  peer_setup post pre; seed_record "$H/.subrouter" a@x isolated-server-login
  run_peer
  expect_rc 1; expect_out "predates Codex credential isolation"
  expect_out "1 Codex record(s) in $H/.subrouter/codex/accounts carry oauthCredentialOrigin"
  expect_build "$CUR" old; expect_no_restart
  finish
fi

if begin peer-rollback-refused-attested-transfer; then
  # The other migrated origin, compact JSON: the peer's match must take both
  # origins and tolerate no whitespace around the colon.
  peer_setup post pre; seed_record "$H/.subrouter" a@x server-attested-transfer compact
  run_peer
  expect_rc 1; expect_out "predates Codex credential isolation"
  expect_out "1 Codex record(s) in $H/.subrouter/codex/accounts carry oauthCredentialOrigin"
  expect_build "$CUR" old; expect_no_restart
  finish
fi

if begin peer-rollback-allowed-by-override; then
  peer_setup post pre; seed_record "$H/.subrouter" a@x isolated-server-login
  run_peer ALLOW_PRE_ISOLATION_ROLLBACK=1
  expect_rc 0; expect_out "ROLLED BACK, daemon healthy"; expect_build "$CUR" cutover
  finish
fi

if begin peer-rollback-no-migrated-records; then
  peer_setup post pre; seed_record "$H/.subrouter" a@x interactive-import
  run_peer
  expect_rc 0; expect_out "no Codex record in $H/.subrouter/codex/accounts was migrated"
  expect_out "ROLLED BACK, daemon healthy"; expect_build "$CUR" cutover
  finish
fi

if begin peer-rollback-refused-store-unreadable; then
  peer_setup post pre; seed_record "$H/.subrouter" a@x interactive-import
  if make_unreadable "$H/.subrouter/codex/accounts"; then
    run_peer
    chmod 755 "$H/.subrouter/codex/accounts"
    expect_rc 1; expect_out "the Codex store $H/.subrouter/codex/accounts exists but cannot be read"
    expect_build "$CUR" old; expect_no_restart
  fi
  finish
fi

if begin peer-rollback-restores-where-the-job-runs-not-subrouter-bin; then
  peer_setup post post; mkdir -p "$H/other"; make_build "$H/other/subrouter" post elsewhere
  run_peer SUBROUTER_BIN="$H/other/subrouter"
  expect_rc 0; expect_out "ROLLED BACK, daemon healthy"
  expect_build "$CUR" cutover; expect_running cutover; expect_build "$H/other/subrouter" elsewhere
  finish
fi

if begin peer-rollback-refuses-a-backup-plist-for-another-binary; then
  peer_setup post post; cp "$H/$LAPTOP_PLIST_REL" "$C/live.plist"
  "$PY3" - "$PEER_BK/plist.bak" "$H" <<'PY2'
import plistlib, sys
with open(sys.argv[1], "rb") as f:
    p = plistlib.load(f)
p["ProgramArguments"][0] = sys.argv[2] + "/other/subrouter"
with open(sys.argv[1], "wb") as f:
    plistlib.dump(p, f)
PY2
  run_peer
  expect_rc 1; expect_out "refusing: $PEER_BK/plist.bak runs $H/other/subrouter, but gui/$(id -u)/$TEST_JOB runs $CUR"
  expect_out "Nothing was restored"; expect_build "$CUR" old; expect_running old; expect_no_restart
  cmp -s "$C/live.plist" "$H/$LAPTOP_PLIST_REL" && ok || bad "the plist changed"
  finish
fi

if begin peer-rollback-pre-to-pre-unaffected; then
  peer_setup pre pre; run_peer
  expect_rc 0; expect_out "ROLLED BACK, daemon healthy"; expect_build "$CUR" cutover
  finish
fi

# rollback-laptop-peer.sh from a shell that cannot restart the daemon (PATCHES
# §47). The backup plist is marked, so a restore of it shows.
expect_plist_kept() {
  if grep -q "cutover plist" "$H/$LAPTOP_PLIST_REL"; then bad "the backup plist was restored"; else ok; fi
}

if begin peer-rollback-dead-launchd-refused; then
  # It restored the backup on disk, had its bootout and bootstrap refused, and
  # printed ROLLED BACK while the build it was fleeing kept serving.
  peer_setup post post; printf 'cutover plist\n' > "$PEER_BK/plist.bak"; dead_launchd; run_peer
  expect_rc 1; expect_out "refusing: this shell cannot reach launchd (launchctl managername: rc 153"
  expect_out "Nothing was restored"; expect_no_out "ROLLED BACK"
  expect_build "$CUR" old; expect_running old; expect_plist_kept
  expect_no_log "launchctl (bootout|bootstrap)"
  finish
fi

if begin peer-rollback-background-session-refused; then
  # ssh: the bootout worked and the bootstrap did not, so a HEALTHY daemon
  # ended up unloaded.
  peer_setup post post; printf 'cutover plist\n' > "$PEER_BK/plist.bak"; background_launchd; run_peer
  expect_rc 1; expect_out "refusing: this shell is in launchd's 'Background' session, not the logged-in GUI (Aqua) session"
  expect_out "Nothing was restored"; expect_no_out "ROLLED BACK"
  expect_build "$CUR" old; expect_running old; expect_plist_kept
  expect_no_log "launchctl (bootout|bootstrap)"
  finish
fi

if begin peer-rollback-print-refused-is-not-unloaded; then
  # Aqua, but launchd refuses print (rc 141, for a loaded job and a missing one
  # alike): the wait after the bootout could not tell a stopped job from a
  # refused query, so nothing is restored and nothing is booted out.
  peer_setup post post; printf 'cutover plist\n' > "$PEER_BK/plist.bak"; run_peer FAKE_PRINT_REFUSED=always
  expect_rc 1
  expect_out "refusing: launchctl print gui/$(id -u)/$TEST_JOB answered rc 141 (only 0, loaded, and 113, not loaded, are answers)"
  expect_out "Nothing was restored"; expect_no_out "ROLLED BACK"
  expect_build "$CUR" old; expect_running old; expect_plist_kept
  expect_no_log "launchctl (bootout|bootstrap)"
  finish
fi

# A daemon already down is the usual reason to roll back. Nothing serves the
# port beforehand, so there is no old pid to differ from, and a new process
# answering health is the rollback working.
if begin peer-rollback-daemon-down-ok; then
  # The job is loaded and its build crash-loops.
  peer_setup post post; : > "$C/launchd/running"; run_peer
  expect_rc 0; expect_out "ROLLED BACK, daemon healthy"
  expect_build "$CUR" cutover; expect_running cutover
  finish
fi

if begin peer-rollback-job-unloaded-ok; then
  # The job is not loaded either (a deploy whose bootstrap failed): print's
  # 113 is an answer, so the rollback proceeds and loads it.
  peer_setup post post; rm -f "$C/launchd/loaded"; : > "$C/launchd/running"; run_peer
  expect_rc 0; expect_out "ROLLED BACK, daemon healthy"
  expect_build "$CUR" cutover; expect_running cutover; expect_log "launchctl bootstrap"
  finish
fi

if begin peer-rollback-unload-wait-needs-rc-113; then
  # launchd stops answering after the bootout; rc 141 is not "the job is gone".
  # The bootout did run, so the job may be unloaded: the text says so (it used
  # to say the daemon was not restarted) and gives the check and the reload.
  peer_setup post post; run_peer FAKE_PRINT_REFUSED=after-bootout
  expect_rc 1; expect_out "answered rc 141 after the bootout"; expect_out "not bootstrapping blind"
  expect_out "the bootout may already have unloaded the job, so the daemon may be down"
  expect_line "  launchctl print gui/$(id -u)/$TEST_JOB"
  expect_line "  launchctl bootstrap gui/$(id -u) $H/$LAPTOP_PLIST_REL"
  expect_no_out "the daemon was not restarted"
  expect_no_out "ROLLED BACK"; expect_no_log "launchctl bootstrap"
  finish
fi

if begin peer-rollback-bootstrap-error-is-shown; then
  # The failed bootstrap's rc was ignored and two minutes of health checks
  # followed; now the rc, and the command to run from a shell that can.
  peer_setup post post; run_peer FAKE_BOOTSTRAP_FAILS=1
  expect_rc 1; expect_out "failed: rc 5: Bootstrap failed: 5: Input/output error"
  expect_line "  launchctl bootstrap gui/$(id -u) $H/$LAPTOP_PLIST_REL"
  expect_no_out "ROLLED BACK"; expect_no_out "daemon did not come back"
  finish
fi

if begin peer-rollback-same-pid-is-not-healthy; then
  # A daemon started by hand holds the port: health answers, from the pid that
  # served before the rollback. Not ROLLED BACK.
  peer_setup post post; run_peer FAKE_STRAY=1
  expect_rc 1; expect_out "pid 100, the daemon from before the rollback, is still the one serving"
  expect_no_out "ROLLED BACK"
  finish
fi

if begin peer-rollback-refused-supervised; then
  peer_setup post post
  write_supervised_plist "$H/$LAPTOP_PLIST_REL" "$H"; load_plist "$H/$LAPTOP_PLIST_REL"
  cp "$H/$LAPTOP_PLIST_REL" "$C/supervised.plist"
  run_peer
  expect_rc 1; expect_out "runs subrouter supervise"; expect_out "SUBROUTER_CLAUDE_SOCKET"
  expect_out "ExitTimeOut (120s)"; expect_out "activate-laptop-supervisor.sh --rollback"
  expect_out "Nothing was restored"; expect_build "$CUR" old; expect_no_restart
  cmp -s "$C/supervised.plist" "$H/$LAPTOP_PLIST_REL" && ok || bad "the plist changed"
  finish
fi

if begin peer-rollback-supervised-forced; then
  peer_setup post post
  write_supervised_plist "$H/$LAPTOP_PLIST_REL" "$H"; load_plist "$H/$LAPTOP_PLIST_REL"
  run_peer -- --restart-supervisor
  expect_rc 0; expect_out "ROLLED BACK, daemon healthy"; expect_build "$CUR" cutover
  cmp -s "$PEER_BK/plist.bak" "$H/$LAPTOP_PLIST_REL" && ok || bad "the cutover plist was not restored"
  expect_loaded serve
  finish
fi

if begin peer-rollback-refused-loaded-supervisor; then
  peer_setup post post
  write_supervised_plist "$H/$LAPTOP_PLIST_REL" "$H"; load_plist "$H/$LAPTOP_PLIST_REL"
  write_serve_plist "$H/$LAPTOP_PLIST_REL" "$H"
  run_peer
  expect_rc 1; expect_out "under supervise, but"; expect_out "activate-laptop-supervisor.sh --rollback"
  expect_build "$CUR" old; expect_no_restart
  finish
fi

if begin peer-rollback-refused-while-the-mutation-lock-is-held; then
  peer_setup post post; hold_lock "$H/$LAPTOP_PLIST_REL"; run_peer; release_lock
  expect_lock_refusal "$H/$LAPTOP_PLIST_REL"; expect_build "$CUR" old; expect_no_restart
  finish
fi

if begin peer-rollback-refuses-a-job-loaded-from-another-plist; then
  peer_setup post post; echo "$C/other/$TEST_JOB.plist" > "$C/launchd/loaded-from"
  run_peer
  expect_rc 1
  expect_out "refusing to change gui/$(id -u)/$TEST_JOB: launchd loaded it from $C/other/$TEST_JOB.plist, and this script's plist is $H/$LAPTOP_PLIST_REL"
  expect_build "$CUR" old; expect_no_restart
  finish
fi

if begin peer-rollback-stops-the-job-before-restoring-files; then
  peer_setup post post; run_peer
  expect_rc 0; expect_log "bootout: the worker on disk is old"; expect_no_log "bootout: the worker on disk is cutover"
  expect_build "$CUR" cutover
  finish
fi

if begin peer-rollback-writes-the-plist-after-the-bootout; then
  peer_setup post post
  "$PY3" - "$PEER_BK/plist.bak" <<'PY2'
import plistlib, sys
with open(sys.argv[1], "rb") as f:
    p = plistlib.load(f)
p["EnvironmentVariables"]["SUBROUTER_CUTOVER"] = "1"
with open(sys.argv[1], "wb") as f:
    plistlib.dump(p, f)
PY2
  live=$(cksum < "$H/$LAPTOP_PLIST_REL" | cut -d" " -f1)
  run_peer
  expect_rc 0; expect_out "ROLLED BACK, daemon healthy"
  expect_log "bootout: the plist on disk has checksum $live\$"
  cmp -s "$PEER_BK/plist.bak" "$H/$LAPTOP_PLIST_REL" && ok || bad "the cutover plist was not restored"
  finish
fi

if begin peer-rollback-survives-a-killed-foreground; then
  peer_setup post post; run_killable run_peer
  expect_killed_and_finished "ROLLED BACK, daemon healthy"
  expect_loaded serve; expect_running cutover; expect_build "$CUR" cutover
  finish
fi

ACT_SH="$DEPLOY/macos/activate-laptop-supervisor.sh"
act_setup() {
  local old="$1" new="$2"; shift 2
  laptop_setup "$old" "$new"
  APLIST="$H/Library/LaunchAgents/$TEST_JOB.plist"
  "$PY3" - "$APLIST" "$H" "$@" <<'PY'
import os, plistlib, sys
path, home = sys.argv[1], sys.argv[2]
with open(path, "wb") as f:
    plistlib.dump({"Label": os.environ["TEST_JOB"],
                   "ProgramArguments": [home + "/bin/subrouter", "serve", "--addr", os.environ["TEST_ADDR"],
                                        "--cloud-config", home + "/.config/subrouter/cloud.json",
                                        "--transcripts", home + "/.subrouter/transcripts"] + sys.argv[3:],
                   "EnvironmentVariables": {"HOME": home, "SUBROUTER_CLAUDE_SOCKET": home + "/.subrouter/claude.sock",
                                            "SUBROUTER_REMOTE_POOLS_FILE": home + "/.subrouter/remote-pools.json"},
                   "KeepAlive": True, "RunAtLoad": True, "WorkingDirectory": home,
                   "StandardOutPath": home + "/Library/Logs/subrouter.log",
                   "StandardErrorPath": home + "/Library/Logs/subrouter.err.log"}, f)
PY
  load_plist "$APLIST"
  cp "$APLIST" "$C/plist.orig"
}
run_act() {
  local envs=() args=()
  while [ $# -gt 0 ] && [ "$1" != -- ]; do envs+=("$1"); shift; done
  [ $# -gt 0 ] && shift
  args=("$@")
  [ ${#args[@]} -gt 0 ] || args=("$NEWBIN")
  run_sandboxed PATH="$C/bin:/usr/bin:/bin:/usr/sbin:/sbin" HOME="$H" TMPDIR="$C/tmp" \
    FAKE_LOG="$C/log" FAKE_LAUNCHD="$C/launchd" FAKE_RUNNING="$C/launchd/running" \
    FAKE_DAEMON="$C/launchd/running" FAKE_DEST="$CUR" FAKE_NEW_ID=new FAKE_PLIST="$APLIST" \
    FAKE_CONTROL_SOCKET="$H/.subrouter/supervisor.sock" FAKE_CLAUDE_SOCKET="$H/.subrouter/claude.sock" \
    FAKE_DRAIN=1 FAKE_ADMIN=admin-tok ${envs[@]+"${envs[@]}"} \
    -- "$LAPTOP_BASH" "$ACT_SH" "${args[@]}"
}
expect_plist_orig() { cmp -s "$C/plist.orig" "$APLIST" && ok || bad "the plist is not the original"; }
expect_plist_orig_plus() {
  local got
  got=$("$PY3" - "$APLIST" "$C/plist.orig" "$@" <<'PY'
import plistlib, sys
new, orig = plistlib.load(open(sys.argv[1], "rb")), plistlib.load(open(sys.argv[2], "rb"))
rest = sys.argv[3:]
env = rest[: rest.index("--")] if "--" in rest else rest
args = rest[rest.index("--") + 1:] if "--" in rest else []
orig["EnvironmentVariables"].update(kv.split("=", 1) for kv in env)
orig["ProgramArguments"] += args
print("" if new == orig else "plist %r, want %r" % (new, orig))
PY
)
  [ -z "$got" ] && ok || bad "$got"
}
expect_untouched() {
  expect_plist_orig; expect_build "$CUR" old
  expect_no_log "launchctl (bootout|bootstrap|kickstart)"
  [ -e "$H/bin/subrouter-supervisor" ] && bad "subrouter-supervisor exists" || ok
  [ -e "$H/.subrouter/worker-config.json" ] && bad "worker-config.json exists" || ok
  any_exists "$H/.subrouter/supervisor-activation-"* && bad "a backup was taken" || ok
}
check_supervised() {
  "$PY3" - "$APLIST" "$C/plist.orig" "$H/.subrouter/worker-config.json" "$H" "$1" <<'PY'
import json, os, plistlib, sys
new_p, orig_p, config_p, home, tailnet = sys.argv[1:6]
new, orig = plistlib.load(open(new_p, "rb")), plistlib.load(open(orig_p, "rb"))
serve = ["--cloud-config", home + "/.config/subrouter/cloud.json", "--transcripts", home + "/.subrouter/transcripts"]
want_args = [home + "/bin/subrouter-supervisor", "supervise", "--addr", os.environ["TEST_ADDR"],
             "--control-socket", home + "/.subrouter/supervisor.sock", "--worker-bin", home + "/bin/subrouter",
             "--worker-stop-grace", "2m", "--ready-timeout", "2m", "--drain-timeout", "90s",
             "--worker-config", home + "/.subrouter/worker-config.json", "--"] + serve
problems = []
if new["ProgramArguments"] != want_args:
    problems.append("ProgramArguments %r" % new["ProgramArguments"])
for key in ("Label", "EnvironmentVariables", "StandardOutPath", "StandardErrorPath", "WorkingDirectory"):
    if new.get(key) != orig.get(key):
        problems.append("%s changed" % key)
for key, want in (("ExitTimeOut", 120), ("ThrottleInterval", 10), ("KeepAlive", True), ("RunAtLoad", True)):
    if new.get(key) != want:
        problems.append("%s=%r" % (key, new.get(key)))
if any("local-data-socket" in a for a in new["ProgramArguments"]):
    problems.append("--local-data-socket")
config = json.load(open(config_p))
want_config = {"args": serve, "env": {"SUBROUTER_PEER_TAILNET": tailnet} if tailnet else {}}
if config != want_config:
    problems.append("worker config %r" % config)
print("; ".join(problems))
PY
}
expect_supervised() { local got; got=$(check_supervised "$1"); [ -z "$got" ] && ok || bad "supervised plist: $got"; }

echo "== activate-laptop-supervisor.sh (under $LAPTOP_BASH)"

if begin act-dry-run-touches-nothing; then
  act_setup post post; run_act -- "$NEWBIN" --peer-tailnet box=192.0.2.10 --dry-run
  expect_rc 0; expect_out "DRY RUN: nothing was changed"
  expect_out "<string>supervise</string>"; expect_out "\"SUBROUTER_PEER_TAILNET\": \"box=192.0.2.10\""
  expect_out "+		<string>--worker-config</string>"; expect_out "-		<string>serve</string>"
  expect_out "drain-status: active_websocket_turns=0 active_proxy_requests=4"
  expect_out "launchd kills it 5 s after the bootout"
  expect_untouched
  finish
fi

if begin act-ok; then
  act_setup post post; run_act -- "$NEWBIN" --peer-tailnet box=192.0.2.10
  expect_rc 0; expect_out "ACTIVATED"
  expect_build "$CUR" new; expect_build "$H/bin/subrouter-supervisor" new
  expect_supervised box=192.0.2.10
  expect_out "active worker              -> new"; expect_out "claude socket owner        -> supervisor: serving"
  expect_out "owned_by_supervisor"; expect_out "health: ok"; expect_out "accounts loaded            -> 12"
  expect_out "control-plane identity     -> stable (pool@example.com)"
  b=$(ls -d "$H/.subrouter/supervisor-activation-"* 2>/dev/null | head -1)
  cmp -s "$C/plist.orig" "$b/plist" && ok || bad "the backup plist is not the original"
  expect_build "$b/worker" old
  [ -e "$b/supervisor" ] && bad "a supervisor backup from nowhere" || ok
  grep -n "launchctl \(bootout\|bootstrap\)" "$C/log" | head -2 | grep -q "bootout" && ok || bad "bootstrap before bootout"
  finish
fi

if begin act-worker-is-where-the-job-runs-not-subrouter-bin; then
  act_setup post post; mkdir -p "$H/other"; make_build "$H/other/subrouter" post elsewhere
  run_act SUBROUTER_BIN="$H/other/subrouter"
  expect_rc 0; expect_out "ACTIVATED"; expect_supervised ""
  expect_build "$CUR" new; expect_build "$H/other/subrouter" elsewhere
  [ -e "$H/other/subrouter-supervisor" ] && bad "a supervisor beside SUBROUTER_BIN" || ok
  finish
fi

if begin act-without-peer-tailnet; then
  act_setup post post; run_act
  expect_rc 0; expect_out "ACTIVATED"; expect_supervised ""
  finish
fi

if begin act-already-supervised-is-a-no-op; then
  act_setup post post; run_act
  cp "$APLIST" "$C/plist.orig"; : > "$C/log"
  run_act
  expect_rc 0; expect_out "already runs supervise; nothing to do"; expect_plist_orig
  expect_no_log "launchctl (bootout|bootstrap)"
  [ "$(ls -d "$H/.subrouter/supervisor-activation-"* | wc -l | tr -d ' ')" = 1 ] && ok || bad "a second backup was taken"
  finish
fi

if begin act-verify-fails-rolls-back; then
  act_setup post post; run_act FAKE_CRASH_BUILD=new
  expect_rc 1; expect_out "supervisor-status: no answer"; expect_out "!! ROLLING BACK"
  expect_out "ROLLED BACK, healthy on the previous job"
  expect_plist_orig; expect_build "$CUR" old
  [ -e "$H/bin/subrouter-supervisor" ] && bad "subrouter-supervisor was left behind" || ok
  [ -e "$H/.subrouter/worker-config.json" ] && bad "worker-config.json was left behind" || ok
  [ "$(grep -c 'launchctl bootstrap' "$C/log")" = 2 ] && ok || bad "want two bootstraps (supervised, then restored)"
  [ "$(cat "$C/launchd/running")" = old ] && ok || bad "the old build is not running"
  finish
fi

if begin act-socket-not-served-rolls-back; then
  act_setup post post; run_act FAKE_SV_SOCKET=unavailable
  expect_rc 1; expect_out "claude socket owner        -> supervisor: unavailable"; expect_out "ROLLED BACK, healthy"
  expect_plist_orig; expect_build "$CUR" old
  finish
fi

if begin act-keeps-an-existing-worker-config; then
  act_setup post post; mkdir -p "$H/.subrouter"; echo '{"args": [], "env": {"X": "1"}}' > "$H/.subrouter/worker-config.json"
  cp "$H/.subrouter/worker-config.json" "$C/config.orig"
  run_act FAKE_CRASH_BUILD=new
  expect_rc 1; cmp -s "$C/config.orig" "$H/.subrouter/worker-config.json" && ok || bad "the earlier worker config was not restored"
  finish
fi

if begin act-refuses-build-without-supervised-socket; then
  act_setup post pre; run_act
  expect_rc 1; expect_out "has no supervisor-owned Claude socket"; expect_untouched
  finish
fi

if begin act-refuses-a-build-from-main; then
  act_setup post main; run_act
  expect_rc 1; expect_out "$NEWBIN has no supervisor-owned Claude socket (it lacks owned_by_supervisor)"
  expect_out "Build it from this repo; a build of upstream subrouter cannot run this job"; expect_out "Nothing was changed"
  expect_untouched; expect_loaded serve
  finish
fi

if begin act-refuses-pre-isolation-live-binary; then
  act_setup pre post; run_act
  expect_rc 1; expect_out "predates Codex credential isolation"; expect_untouched
  finish
fi

if begin act-refuses-a-worker-owned-socket-flag; then
  act_setup post post --unix-socket /tmp/x.sock; run_act
  expect_rc 1; expect_out "the live serve args set --unix-socket"; expect_untouched
  finish
fi

if begin act-wait-quiet-waits-for-the-turns; then
  act_setup post post; echo 3 > "$C/turns"
  run_act FAKE_TURNS_FILE="$C/turns" -- "$NEWBIN" --wait-quiet 60
  expect_rc 0; expect_out "3 websocket turn(s) still open; waited 0s of 60s"; expect_out "no websocket turn in flight"; expect_out "ACTIVATED"
  finish
fi

if begin act-wait-quiet-checks-again-before-the-bootout; then
  # The parent saw no turn, then the detached run spent the baseline (up to
  # ~95 s) before its bootout, and a turn opened in between.
  act_setup post post; printf '0\n0\n2\n0\n' > "$C/turns"
  run_act FAKE_TURNS_QUEUE="$C/turns" -- "$NEWBIN" --wait-quiet 60
  expect_rc 0; expect_out "ACTIVATED"; expect_out "--wait-quiet checks the turns again"
  step=$(grep -n "== 4. restart under the supervisor" "$C/out" | head -1 | cut -d: -f1)
  wait_line=$(grep -n "2 websocket turn(s) still open" "$C/out" | head -1 | cut -d: -f1)
  bootout=$(grep -n "booting out" "$C/out" | head -1 | cut -d: -f1)
  [ -n "$wait_line" ] && [ "${step:-999999}" -lt "$wait_line" ] && [ "$wait_line" -lt "${bootout:-0}" ] && ok \
    || bad "no wait for the turn between step 4 and the bootout"
  finish
fi

if begin act-wait-quiet-gives-up-before-the-bootout; then
  act_setup post post; printf '0\n0\n2\n' > "$C/turns"
  run_act FAKE_TURNS_QUEUE="$C/turns" -- "$NEWBIN" --wait-quiet 10
  expect_rc 1; expect_out "websocket turns still open after 10s"; expect_out "restored the files from"
  expect_plist_orig; expect_build "$CUR" old; expect_no_restart; expect_loaded serve
  [ -e "$H/bin/subrouter-supervisor" ] && bad "subrouter-supervisor was left behind" || ok
  finish
fi

if begin act-wait-quiet-gives-up; then
  act_setup post post; run_act FAKE_TURNS=2 -- "$NEWBIN" --wait-quiet 10
  expect_rc 1; expect_out "websocket turns still open after 10s; nothing was changed"; expect_untouched
  finish
fi

if begin act-rollback-on-request; then
  act_setup post post; run_act -- "$NEWBIN" --peer-tailnet box=192.0.2.10
  expect_rc 0
  run_act -- --rollback
  expect_rc 0; expect_out "ROLLED BACK"; expect_out "tcp health      -> ok"; expect_out "claude socket   -> serving"
  expect_out "+ EnvironmentVariables.SUBROUTER_PEER_TAILNET, set in the worker config since the activation"
  expect_plist_orig_plus SUBROUTER_PEER_TAILNET=box=192.0.2.10
  expect_build "$CUR" new; expect_out "it runs the current worker, $CUR (new)"
  [ -e "$H/bin/subrouter-supervisor" ] && bad "subrouter-supervisor was left behind" || ok
  [ -e "$H/.subrouter/worker-config.json" ] && bad "worker-config.json was left behind" || ok
  [ "$(cat "$C/launchd/running")" = new ] && ok || bad "the current worker is not running"
  : > "$C/log"; run_act -- --rollback
  expect_rc 0; expect_out "runs serve and $APLIST agrees; nothing to roll back"; expect_no_restart
  finish
fi

if begin act-survives-a-killed-foreground; then
  act_setup post post; run_killable run_act
  expect_killed_and_finished "ACTIVATED"
  grep -Fq "health: ok" "$TRANSITION_LOG" && ok || bad "the detached run verified no health"
  expect_loaded supervised; expect_supervised ""
  [ "$(cat "$C/launchd/running")" = new ] && ok || bad "the new build is not the active worker"
  finish
fi

if begin act-rollback-survives-a-killed-foreground; then
  act_setup post post; run_act; expect_rc 0
  run_killable run_act -- --rollback
  expect_killed_and_finished "ROLLED BACK"
  grep -Fq "tcp health      -> ok" "$TRANSITION_LOG" && ok || bad "the detached rollback verified no health"
  expect_loaded serve; expect_plist_orig
  finish
fi

if begin act-rerun-loads-a-supervised-plist-nothing-loaded; then
  act_setup post post; run_act; expect_rc 0
  unload_job; : > "$C/log"
  run_act
  expect_rc 0; expect_out "nothing to do but load it"; expect_out "LOADED  gui/"
  expect_out "active worker              -> new"; expect_loaded supervised; expect_no_log "launchctl bootout"
  finish
fi

if begin act-activates-a-serve-plist-nothing-loaded; then
  act_setup post post; unload_job
  run_act
  expect_rc 0; expect_out "launchd has not loaded gui/"; expect_out "ACTIVATED"
  expect_loaded supervised; expect_supervised ""
  finish
fi

if begin act-rollback-rerun-loads-a-serve-plist-nothing-loaded; then
  act_setup post post; unload_job
  run_act -- --rollback
  expect_rc 0; expect_out "nothing to roll back but load it"; expect_out "LOADED  gui/"
  expect_out "tcp health      -> ok"; expect_loaded serve; expect_plist_orig
  finish
fi

if begin act-rollback-finishes-a-half-done-rollback; then
  act_setup post post; run_act; expect_rc 0
  cp "$C/plist.orig" "$APLIST"
  run_act -- --rollback
  expect_rc 0; expect_out "finishing that rollback"; expect_out "ROLLED BACK"
  expect_loaded serve; expect_plist_orig
  finish
fi

if begin act-refuses-a-supervised-plist-under-a-serve-job; then
  act_setup post post
  write_supervised_plist "$APLIST" "$H"; cp "$APLIST" "$C/plist.orig"
  run_act
  expect_rc 1; expect_out "as a plain serve, but $APLIST runs supervise"
  expect_out "activate-laptop-supervisor.sh --rollback, then activate again"
  expect_untouched; expect_loaded serve
  finish
fi

if begin act-rollback-stops-the-job-before-restoring-files; then
  act_setup post post; run_act; expect_rc 0; : > "$C/log"
  run_act -- --rollback
  expect_rc 0; expect_log "bootout: the plist on disk runs supervise"
  expect_no_log "bootout: the plist on disk runs serve"
  finish
fi

if begin act-waits-say-how-long; then
  act_setup post post; run_act FAKE_BOOTOUT_POLLS=12
  expect_rc 0; expect_out "waiting for launchd to let the old daemon go (10s of 600s)"; expect_out "ACTIVATED"
  finish
fi

if begin act-route-regression-rolls-back; then
  act_setup post post; run_act FAKE_BAD=accounts-404
  expect_rc 1; expect_out "GET  /_subrouter/accounts  -> 404"; expect_out "health: ok"
  expect_out "!! ROLLING BACK"; expect_out "ROLLED BACK, healthy on the previous job"
  expect_plist_orig; expect_build "$CUR" old; expect_loaded serve
  finish
fi

if begin act-route-silent-before-and-after-is-kept; then
  act_setup post post; run_act FAKE_ALWAYS_BAD=identity-unavailable
  expect_rc 0; expect_out "COULD NOT PROBE (upstream: 429)"; expect_out "equally silent before the swap"
  expect_out "ACTIVATED"; expect_loaded supervised
  finish
fi

if begin act-route-identity-regression-rolls-back; then
  act_setup post post; run_act FAKE_BAD=identity-unavailable
  expect_rc 1; expect_out "it answered before the swap, so this IS a regression"; expect_out "!! ROLLING BACK"
  expect_plist_orig; expect_build "$CUR" old
  finish
fi

if begin act-baselines-before-the-bootout; then
  act_setup post post; run_act
  expect_rc 0; expect_out "baseline: provider=mounted  identity=stable  accounts=12  claude-socket=serving"
  first_probe=$(grep -n "api/oauth/profile" "$C/log" | head -1 | cut -d: -f1)
  first_bootout=$(grep -n "^launchctl bootout" "$C/log" | head -1 | cut -d: -f1)
  [ -n "$first_probe" ] && [ "$first_probe" -lt "${first_bootout:-0}" ] && ok || bad "no route probe before the bootout"
  finish
fi

if begin act-rollback-keeps-later-plist-edits; then
  act_setup post post; run_act; expect_rc 0
  "$PY3" - "$APLIST" <<'PY'
import plistlib, sys
with open(sys.argv[1], "rb") as f:
    p = plistlib.load(f)
p["EnvironmentVariables"]["SUBROUTER_EXTRA"] = "later"
with open(sys.argv[1], "wb") as f:
    plistlib.dump(p, f)
PY
  load_plist "$APLIST"
  run_act -- --rollback
  expect_rc 0; expect_out "+ EnvironmentVariables.SUBROUTER_EXTRA, set in the plist since the activation"
  expect_plist_orig_plus SUBROUTER_EXTRA=later; expect_loaded serve
  finish
fi

if begin act-rollback-keeps-the-current-worker; then
  act_setup post post; run_act; expect_rc 0
  make_build "$CUR" post deployed-later
  run_act -- --rollback
  expect_rc 0; expect_build "$CUR" deployed-later
  [ "$(cat "$C/launchd/running")" = deployed-later ] && ok || bad "the plain job does not run the current worker"
  expect_no_out "goes back to the build that ran before the activation"
  finish
fi

if begin act-rollback-keeps-the-worker-config-args; then
  act_setup post post; run_act; expect_rc 0
  "$PY3" -c 'import json, sys
p = sys.argv[1]; c = json.load(open(p)); c["args"] += ["--sr-switch-interval", "5m"]; json.dump(c, open(p, "w"))' "$H/.subrouter/worker-config.json"
  run_act -- --rollback
  expect_rc 0; expect_out "~ ProgramArguments"
  expect_plist_orig_plus -- --sr-switch-interval 5m
  finish
fi

if begin act-rollback-puts-no-binary-back; then
  act_setup post post; make_build "$H/bin/subrouter-supervisor" pre sup-pre
  run_act; expect_rc 0; expect_build "$H/bin/subrouter-supervisor" new
  run_act -- --rollback
  expect_rc 0; expect_build "$H/bin/subrouter-supervisor" new; expect_build "$CUR" new
  expect_out "stays as it is: nothing runs it"
  finish
fi

if begin act-undo-restores-every-file-and-says-which-failed; then
  act_setup post post; : > "$APLIST.supervisor-mutation.lock"; chmod 555 "$H/Library/LaunchAgents"
  run_act
  chmod 755 "$H/Library/LaunchAgents"
  expect_rc 2; expect_out "could not write $APLIST"; expect_out "could not restore $APLIST"
  expect_no_out "restored the files from"; expect_out "put them back from"
  [ -e "$H/.subrouter/worker-config.json" ] && bad "the worker config the activation wrote was left behind" || ok
  expect_build "$CUR" old; expect_no_restart; expect_loaded serve
  finish
fi

if begin act-refuses-when-the-manifest-cannot-be-written; then
  act_setup post post
  stub mkdir 'for a; do :; done; /bin/mkdir "$@" || exit
case "$a" in */supervisor-activation-*) /bin/mkdir "$a/manifest" ;; esac'
  run_act
  rm -f "$C/bin/mkdir"
  expect_rc 1; expect_out "cannot write the backup manifest"
  expect_plist_orig; expect_build "$CUR" old; expect_no_restart
  [ -e "$H/bin/subrouter-supervisor" ] && bad "subrouter-supervisor exists" || ok
  finish
fi

if begin act-rollback-skips-a-backup-without-a-manifest; then
  act_setup post post; run_act; expect_rc 0
  mkdir -p "$H/.subrouter/supervisor-activation-29991231-000000"
  run_act -- --rollback
  expect_rc 0; expect_out "ROLLED BACK"; expect_no_out "supervisor-activation-29991231-000000"
  expect_loaded serve
  finish
fi

if begin act-rollback-waits-for-the-claude-socket; then
  act_setup post post; run_act; expect_rc 0
  run_act FAKE_SOCKET_LATE=3 -- --rollback
  expect_rc 0; expect_out "claude socket   -> serving"; expect_out "ROLLED BACK"
  finish
fi

# A stand-in answers health at the Claude socket with "ok": true, and is not
# the daemon serving it (PATCHES §84).
if begin act-rollback-tells-a-stand-in-from-the-daemon; then
  act_setup post post; run_act; expect_rc 0
  run_act FAKE_CLAUDE_WORD=standby -- --rollback
  expect_rc 2; expect_out "claude socket   -> a stand-in answers on $H/.subrouter/claude.sock, not the daemon, so the daemon does not own its socket"
  expect_no_out "claude socket   -> serving"; expect_out "ROLLED BACK, NOT HEALTHY"
  expect_loaded serve
  finish
fi

if begin act-rollback-says-a-reload-is-not-healthy; then
  act_setup post post; unload_job
  run_act FAKE_CRASH_BUILD=old -- --rollback
  expect_rc 2; expect_out "LOADED, NOT HEALTHY"; expect_no_out "ROLLED BACK"
  finish
fi

if begin act-rollback-says-which-files-it-left; then
  act_setup post post; run_act; expect_rc 0
  chmod 555 "$H/.subrouter"
  run_act -- --rollback
  chmod 755 "$H/.subrouter"
  expect_rc 2; expect_out "could not remove $H/.subrouter/worker-config.json"
  expect_out "ROLLED BACK, healthy, but the files named above were not put back"; expect_no_out "NOT HEALTHY"
  expect_loaded serve
  finish
fi

if begin act-failed-activation-stops-the-job-before-restoring-files; then
  act_setup post post; run_act FAKE_CRASH_BUILD=new
  expect_rc 1; expect_out "ROLLED BACK, healthy on the previous job"
  [ "$(grep -c "bootout: the plist on disk runs supervise" "$C/log")" = 2 ] && ok || bad "a bootout did not find the supervised plist still on disk"
  expect_no_log "bootout: the plist on disk runs serve"; expect_no_log "bootout: the worker on disk is old"
  finish
fi

if begin act-refuses-outside-the-gui-session; then
  act_setup post post; run_act FAKE_MANAGER=Background
  expect_rc 1; expect_out "launchd's 'Background' session, not the logged-in GUI (Aqua) session"; expect_untouched
  finish
fi

if begin act-refused-from-an-ended-session; then
  act_setup post post; dead_launchd; run_act
  expect_rc 1; expect_out "this shell cannot reach launchd (launchctl managername: rc 153"
  expect_out "Nothing was changed"; expect_untouched
  finish
fi

if begin act-rollback-refused-outside-the-gui-session; then
  act_setup post post; run_act
  expect_rc 0; cp "$APLIST" "$C/plist.supervised"; : > "$C/log"
  run_act FAKE_MANAGER=Background -- --rollback
  expect_rc 1; expect_out "not the logged-in GUI (Aqua) session"; expect_no_restart
  cmp -s "$C/plist.supervised" "$APLIST" && ok || bad "the plist changed"
  finish
fi

if begin act-failed-bootstrap-rolls-back-at-once; then
  act_setup post post; run_act FAKE_BOOTSTRAP_FAILS_ONCE=1
  expect_rc 1; expect_out "failed: rc 5: Bootstrap failed: 5"; expect_out "!! ROLLING BACK"
  expect_out "ROLLED BACK, healthy on the previous job"; expect_plist_orig; expect_build "$CUR" old
  after_first_bootstrap > "$C/between"
  grep -q "supervisor-status\|_subrouter/health" "$C/between" && bad "it verified after launchd refused the bootstrap" || ok
  finish
fi

if begin act-undo-removes-the-supervisor-it-installed; then
  act_setup post post; : > "$APLIST.supervisor-mutation.lock"; chmod 555 "$H/Library/LaunchAgents"
  run_act
  chmod 755 "$H/Library/LaunchAgents"
  expect_rc 2; expect_out "could not write $APLIST"; expect_build "$CUR" old; expect_no_restart
  [ -e "$H/bin/subrouter-supervisor" ] && bad "the supervisor the activation installed was left behind" || ok
  finish
fi

if begin act-rollback-after-a-failed-re-sign-loads-the-job; then
  act_setup post post; run_act FAKE_BAD=accounts-404 FAKE_SIGN_FAILS_AFTER=2
  expect_rc 2; expect_out "!! ROLLING BACK"; expect_out "could not restore $CUR from"
  expect_out "ROLLED BACK, healthy, but the files named above were not put back"; expect_no_out "NOT HEALTHY"
  expect_loaded serve; expect_plist_orig
  [ -e "$H/bin/subrouter-supervisor" ] && bad "subrouter-supervisor was left behind" || ok
  finish
fi

if begin act-rollback-that-cannot-remove-the-supervisor-loads-the-job; then
  act_setup post post; run_act FAKE_BAD=accounts-404 FAKE_BOOTOUT_LOCKS="$H/bin"
  chmod 755 "$H/bin"
  expect_rc 2; expect_out "could not restore $H/bin/subrouter-supervisor"
  expect_out "ROLLED BACK, healthy, but the files named above were not put back"; expect_no_out "NOT HEALTHY"
  expect_loaded serve; expect_plist_orig
  finish
fi

if begin act-rollback-that-cannot-restore-the-plist-loads-the-supervised-job; then
  act_setup post post; run_act FAKE_BAD=accounts-404 FAKE_BOOTOUT_LOCKS="$H/Library/LaunchAgents"
  chmod 755 "$H/Library/LaunchAgents"
  expect_rc 2; expect_out "could not restore $APLIST from"
  expect_out "NOT ROLLED BACK: launchd runs the supervised plist again, healthy"; expect_no_out "NOT HEALTHY"
  expect_loaded supervised; expect_supervised ""; expect_build "$H/bin/subrouter-supervisor" new; expect_build "$CUR" new
  finish
fi

if begin act-rollback-on-request-that-cannot-write-the-plist-loads-the-supervised-job; then
  act_setup post post; run_act; expect_rc 0
  run_act FAKE_BOOTOUT_LOCKS="$H/Library/LaunchAgents" -- --rollback
  chmod 755 "$H/Library/LaunchAgents"
  expect_rc 2; expect_out "could not write $APLIST"
  expect_out "NOT ROLLED BACK: launchd runs the supervised plist again, healthy"; expect_no_out "NOT HEALTHY"
  expect_loaded supervised; expect_supervised ""; expect_build "$H/bin/subrouter-supervisor" new
  finish
fi

if begin act-rollback-with-a-refused-bootstrap-names-the-command; then
  act_setup post post; run_act FAKE_BOOTSTRAP_FAILS=1
  expect_rc 2; expect_out "ROLLED BACK, NOT HEALTHY"
  expect_out "launchd has not loaded gui/$(id -u)/$TEST_JOB"
  expect_line "     launchctl bootstrap gui/$(id -u) $APLIST"; expect_plist_orig
  finish
fi

if begin act-rollback-on-request-with-a-refused-bootstrap-names-the-command; then
  act_setup post post; run_act; expect_rc 0
  run_act FAKE_BOOTSTRAP_FAILS=1 -- --rollback
  expect_rc 2; expect_out "ROLLED BACK, NOT HEALTHY"
  expect_line "     launchctl bootstrap gui/$(id -u) $APLIST"
  finish
fi

if begin act-refused-while-the-mutation-lock-is-held; then
  act_setup post post; hold_lock "$APLIST"; run_act; release_lock
  expect_lock_refusal "$APLIST"; expect_untouched
  finish
fi

if begin act-refuses-a-job-loaded-from-another-plist; then
  act_setup post post; echo "$C/other/$TEST_JOB.plist" > "$C/launchd/loaded-from"
  run_act
  expect_rc 1
  expect_out "refusing to change gui/$(id -u)/$TEST_JOB: launchd loaded it from $C/other/$TEST_JOB.plist, and this script's plist is $APLIST"
  expect_untouched
  finish
fi

if begin act-refusal-at-the-restart-puts-the-files-back; then
  # launchd's job moved after step 2 installed the builds: the bootout is
  # refused, and the plist and builds go back, as for any failure before the
  # restart (PATCHES §76.6).
  act_setup post post; run_act FAKE_SIGN_MOVES_JOB="$C/other/$TEST_JOB.plist"
  expect_rc 1
  expect_out "refusing to boot out gui/$(id -u)/$TEST_JOB: launchd loaded it from $C/other/$TEST_JOB.plist, and this script's plist is $APLIST"
  expect_out "restored the files from"
  expect_plist_orig; expect_build "$CUR" old; expect_no_restart
  [ -e "$H/bin/subrouter-supervisor" ] && bad "subrouter-supervisor was left behind" || ok
  finish
fi


ROUTE_SH="$(cd "$DEPLOY/.." && pwd -P)/bin/claude-route"
route_setup() { # [plist env assignments...]
  laptop_setup post post
  mkdir -p "$H/.claude-route"
  RPLIST="$H/$LAPTOP_PLIST_REL"
  make_build "$H/bin/subrouter-supervisor" post sup-old
  write_supervised_plist "$RPLIST" "$H" "$@"
  cp "$RPLIST" "$C/plist.orig"
  load_plist "$RPLIST"; echo sup-old > "$C/launchd/supervisor"
  printf '#!/bin/bash\necho "deploy RELOAD=${RELOAD:-} $*" >> "$FAKE_LOG"\nexit 0\n' > "$C/deploy-stub"
  chmod 755 "$C/deploy-stub"
}
run_route_as() { # claude-route envs... -- args...
  local script="$1" envs=(); shift
  while [ $# -gt 0 ] && [ "$1" != -- ]; do envs+=("$1"); shift; done
  [ $# -gt 0 ] && shift
  run_sandboxed PATH="$C/bin:/usr/bin:/bin:/usr/sbin:/sbin" HOME="$H" TMPDIR="$C/tmp" \
    CLAUDE_ROUTE_HOME="$H/.claude-route" CLAUDE_ROUTE_PLIST="$RPLIST" CLAUDE_ROUTE_SOCKET="$H/.subrouter/claude.sock" \
    FAKE_LOG="$C/log" FAKE_LAUNCHD="$C/launchd" FAKE_RUNNING="$C/launchd/running" FAKE_DAEMON="$C/launchd/running" FAKE_NEW_ID=new \
    FAKE_PLIST="$RPLIST" FAKE_DEST="$CUR" FAKE_ADMIN=admin-tok \
    FAKE_CONTROL_SOCKET="$H/.subrouter/supervisor.sock" FAKE_CLAUDE_SOCKET="$H/.subrouter/claude.sock" ${envs[@]+"${envs[@]}"} \
    -- "$LAPTOP_BASH" "$script" "$@"
}
run_route_stub_deploy() { run_route_as "$ROUTE_SH" CLAUDE_ROUTE_DEPLOY="$C/deploy-stub" -- "$@"; }
route_serve_job() { write_serve_plist "$RPLIST" "$H"; cp "$RPLIST" "$C/plist.orig"; load_plist "$RPLIST"; : > "$C/launchd/supervisor"; }
expect_plist_unchanged() { cmp -s "$C/plist.orig" "$RPLIST" && ok || bad "the plist changed"; }
plist_env() { "$PY3" -c 'import plistlib, sys; print((plistlib.load(open(sys.argv[1], "rb")).get("EnvironmentVariables") or {}).get(sys.argv[2], ""))' "$RPLIST" "$1"; }
# The claude shim as claude-route's own write_shim writes it, in front of a real
# claude that prints its environment.
route_shim_setup() {
  route_setup
  mkdir -p "$C/real"; printf '#!/bin/bash\nenv\n' > "$C/real/claude"; chmod 755 "$C/real/claude"
  sed '/^case "${1:-status}" in/,$d' "$ROUTE_SH" > "$C/route-lib.sh"
  SHIM="$H/.claude-route/bin/claude"
  env -i PATH="$C/bin:/usr/bin:/bin" HOME="$H" CLAUDE_ROUTE_HOME="$H/.claude-route" \
    "$LAPTOP_BASH" -c 'source "$1"; write_shim "$2"' _ "$C/route-lib.sh" "$SHIM"
  PROFILE="$H/.subrouter/codex/claude/work"
}
run_shim() { # env-assignments... -- shim args...
  local envs=()
  while [ $# -gt 0 ] && [ "$1" != -- ]; do envs+=("$1"); shift; done
  [ $# -gt 0 ] && shift
  run_sandboxed PATH="$H/.claude-route/bin:$C/real:$C/bin:/usr/bin:/bin" HOME="$H" TMPDIR="$C/tmp" \
    CLAUDE_ROUTE_HOME="$H/.claude-route" CLAUDE_ROUTE_SOCKET="$H/.subrouter/claude.sock" \
    FAKE_LOG="$C/log" FAKE_RUNNING="$C/launchd/running" FAKE_LAUNCHD="$C/launchd" \
    FAKE_CLAUDE_SOCKET="$H/.subrouter/claude.sock" ${envs[@]+"${envs[@]}"} -- "$SHIM" "$@"
}
# Pool mode with the daemon's Claude socket answering. `[ -S ]` needs only the
# file; it is bound from its own directory, since the full path can pass the
# 104 bytes a socket address holds.
route_shim_pool() {
  echo pool > "$H/.claude-route/mode"
  "$PY3" -c 'import os, socket, sys; os.chdir(sys.argv[1]); socket.socket(socket.AF_UNIX).bind("claude.sock")' "$H/.subrouter"
}

echo "== claude-route"

if begin route-socket-on-refused-supervised; then
  route_setup; run_route_stub_deploy socket-on
  expect_rc 1; expect_out "socket-on refused"; expect_out "reads SUBROUTER_CLAUDE_SOCKET only when it starts"
  expect_out "Nothing was changed"; expect_out "claude-route socket-on --restart-supervisor"
  expect_plist_unchanged; expect_no_log "^deploy"
  finish
fi

if begin route-socket-off-refused-supervised; then
  route_setup SUBROUTER_CLAUDE_SOCKET="$C/home/.subrouter/claude.sock"; run_route_stub_deploy socket-off --force
  expect_rc 1; expect_out "socket-off refused"; expect_out "claude-route socket-off --restart-supervisor"
  expect_plist_unchanged; expect_no_log "^deploy"
  finish
fi

if begin route-socket-off-supervised-forced; then
  route_setup SUBROUTER_CLAUDE_SOCKET="$C/home/.subrouter/claude.sock"; run_route_as "$ROUTE_SH" -- socket-off --restart-supervisor
  expect_rc 0; expect_out "restarting the supervisor"; expect_out "DEPLOY OK"; expect_out "socket: off"
  [ -z "$(plist_env SUBROUTER_CLAUDE_SOCKET)" ] && ok || bad "SUBROUTER_CLAUDE_SOCKET is still in the plist"
  [ "$(plist_env HOME)" = "$H" ] && ok || bad "the rest of the environment did not survive the edit"
  expect_loaded supervised; grep -q SUBROUTER_CLAUDE_SOCKET "$C/launchd/loaded" && bad "launchd still loads the socket" || ok
  expect_order "launchctl bootout" "launchctl bootstrap"
  finish
fi

if begin route-socket-on-supervised-forced-restores; then
  route_setup; run_route_as "$ROUTE_SH" FAKE_CLAUDE_SOCKET=/nonexistent/claude.sock -- socket-on --restart-supervisor
  expect_rc 1; expect_out "!! ROLLING BACK"; expect_out "rolled back, healthy"; expect_out "!! socket-on failed"
  expect_no_out "on the restored plist"; expect_plist_unchanged
  expect_loaded supervised; cmp -s "$C/plist.orig" "$C/launchd/loaded" && ok || bad "launchd does not run the original plist again"
  finish
fi

if begin route-socket-on-leaves-the-plist-to-the-lock-holder; then
  # claude-route backed the plist up and edited it without the lock. When the
  # deploy then refused, it put its stale backup over what the holder wrote
  # and said the daemon was healthy on the restored plist.
  route_setup; route_serve_job
  "$PY3" -c 'import fcntl, os, plistlib, sys, time
plist, held, saw = sys.argv[1:4]
fd = os.open(plist + ".supervisor-mutation.lock", os.O_WRONLY | os.O_CREAT, 0o600)
fcntl.flock(fd, fcntl.LOCK_EX)
open(held, "w").close()
for _ in range(3000):
    with open(plist, "rb") as f:
        if "SUBROUTER_CLAUDE_SOCKET" in (plistlib.load(f).get("EnvironmentVariables") or {}):
            open(saw, "w").close()
            break
    time.sleep(0.01)
time.sleep(300)' "$RPLIST" "$C/lock-held" "$C/saw-edit" &
  LOCK_HOLDER=$!
  while [ ! -e "$C/lock-held" ]; do /bin/sleep 0.05; done
  run_route_as "$ROUTE_SH" -- socket-on
  release_lock
  expect_rc 1; expect_out "another deploy, rollback or update holds $RPLIST.supervisor-mutation.lock"
  expect_out "!! socket-on failed"; expect_no_out "on the restored plist"
  [ -e "$C/saw-edit" ] && bad "claude-route edited the plist while another run held the lock" || ok
  expect_plist_unchanged; expect_no_restart
  finish
fi

if begin route-socket-on-consent-holds-when-the-job-turns-supervised; then
  # The consent check read a plain serve; an activation finished; the reload
  # read the job again and added --restart-supervisor that nobody gave.
  route_setup; route_serve_job
  write_supervised_plist "$C/supervised.plist" "$H"
  run_route_as "$ROUTE_SH" FAKE_SUPERVISE_AT_PRINT="2:$C/supervised.plist" -- socket-on
  expect_log "an activation finished"
  expect_rc 1; expect_out "RELOAD=1, and every plist edit, restarts the job"; expect_out "Nothing was changed"
  expect_no_restart; expect_loaded supervised
  cmp -s "$C/supervised.plist" "$RPLIST" && ok || bad "the activated plist changed"
  finish
fi

# A stand-in at the socket answers health with "ok": true, and is not the
# daemon serving it (PATCHES §84). status names it, and socket-on neither says
# "already serving" nor restarts anything.
if begin route-status-and-socket-on-tell-a-stand-in-from-the-daemon; then
  route_setup SUBROUTER_CLAUDE_SOCKET="$C/home/.subrouter/claude.sock"
  "$PY3" -c 'import os, socket, sys; os.chdir(sys.argv[1]); socket.socket(socket.AF_UNIX).bind("claude.sock")' "$H/.subrouter"
  run_route_stub_deploy status
  expect_out "socket:      serving $H/.subrouter/claude.sock"
  run_route_stub_deploy socket-on
  expect_rc 0; expect_out "socket: already serving $H/.subrouter/claude.sock"
  run_route_as "$ROUTE_SH" CLAUDE_ROUTE_DEPLOY="$C/deploy-stub" FAKE_CLAUDE_WORD=standby -- status
  expect_out "socket:      STAND-IN at $H/.subrouter/claude.sock: the daemon does not own its socket"
  expect_no_out "socket:      serving"
  run_route_as "$ROUTE_SH" CLAUDE_ROUTE_DEPLOY="$C/deploy-stub" FAKE_CLAUDE_WORD=standby -- socket-on
  expect_rc 1; expect_out "a stand-in answers there, not the daemon"; expect_out "Nothing was changed"
  expect_no_out "already serving"
  expect_plist_unchanged; expect_no_log "^deploy"; expect_no_restart
  finish
fi

if begin route-socket-on-refused-loaded-supervisor; then
  route_setup
  write_serve_plist "$RPLIST" "$H"; cp "$RPLIST" "$C/plist.orig"
  run_route_stub_deploy socket-on
  expect_rc 1; expect_out "socket-on refused: launchd runs gui/"; expect_out "under supervise, but"
  expect_plist_unchanged; expect_no_log "^deploy"
  finish
fi

if begin route-link-finds-the-scripts-in-the-checkout; then
  route_setup; ln -s "$ROUTE_SH" "$H/bin/claude-route"
  run_route_as "$H/bin/claude-route" -- socket-on
  expect_rc 1; expect_out "socket-on refused: gui/$(id -u)/$TEST_JOB runs subrouter supervise"
  expect_plist_unchanged
  finish
fi

if begin route-plain-copy-says-where-the-scripts-are; then
  route_setup; cp "$ROUTE_SH" "$H/bin/claude-route"
  run_route_as "$H/bin/claude-route" -- socket-on --restart-supervisor
  expect_rc 1; expect_out "no deploy-laptop.sh and laptop-deploy-lib.sh at $H/bin/../deploy/macos/deploy-laptop.sh"
  expect_out "Remove the copy so PATH finds ops/bin/claude-route"; expect_out "CLAUDE_ROUTE_DEPLOY="
  expect_plist_unchanged; expect_no_log "^deploy"
  finish
fi

if begin route-shims-keep-max-retries; then
  route_shim_setup
  grep -q MAX_RETRIES "$SHIM" && bad "the shim names CLAUDE_CODE_MAX_RETRIES" || ok
  for route in off pool; do
    run_sandboxed PATH="$H/.claude-route/bin:$C/real:$C/bin:/usr/bin:/bin" HOME="$H" TMPDIR="$C/tmp" \
      CLAUDE_ROUTE_HOME="$H/.claude-route" CLAUDE_ROUTE="$route" CLAUDE_ROUTE_SOCKET=/nonexistent/claude.sock \
      CLAUDE_CODE_MAX_RETRIES=15 FAKE_LOG="$C/log" FAKE_RUNNING="$C/launchd/running" \
      -- "$SHIM" -p hi
    expect_rc 0; expect_out "CLAUDE_CODE_MAX_RETRIES=15"
  done
  expect_out "ANTHROPIC_BASE_URL=http://$TEST_ADDR"; expect_out "CLAUDE_CODE_PROMPT_CACHE_TTL=1h"
  env -i PATH=/usr/bin:/bin HOME="$H" CLAUDE_CODE_MAX_RETRIES=15 CLAUDE_CODE_EFFORT_LEVEL=high \
    "$LAPTOP_BASH" -c 'source "$1"; build_launch_env; printf "%s\n" "${CLEAN[@]}"' _ "$C/route-lib.sh" > "$C/out"
  expect_out "CLAUDE_CODE_EFFORT_LEVEL"; expect_no_out "CLAUDE_CODE_MAX_RETRIES"
  finish
fi

if begin route-login-keeps-the-profile-store; then
  # sr claude addr runs `claude auth login` with CLAUDE_CONFIG_DIR at the
  # profile. In pool mode the shim pointed Claude Code's credential store at
  # its login-less one, so the login landed there and the profile had none.
  route_shim_setup; route_shim_pool
  run_shim CLAUDE_ROUTE_DRYRUN=1 CLAUDE_CONFIG_DIR="$PROFILE" -- auth login
  expect_rc 0; expect_line "route=credentials"
  run_shim CLAUDE_CONFIG_DIR="$PROFILE" -- auth login --claudeai
  expect_rc 0; expect_line "CLAUDE_CONFIG_DIR=$PROFILE"; expect_line "CLAUDE_ROUTE_ACTIVE=credentials"
  expect_no_out "CLAUDE_SECURESTORAGE_CONFIG_DIR="; expect_no_out "CLAUDE_CODE_OAUTH_TOKEN="; expect_no_out "ANTHROPIC_UNIX_SOCKET="
  grep -q " route=credentials" "$H/.claude-route/launch.log" && ok || bad "launch.log lacks route=credentials"
  expect_no_log "^curl"
  finish
fi

if begin route-login-in-a-pool-session-drops-the-pool; then
  # The same login from a pooled session's shell, whose environment holds the
  # parent shim's route: listed in CLAUDE_ROUTE_SET, and not.
  route_shim_setup; route_shim_pool
  for set in "ANTHROPIC_BASE_URL ANTHROPIC_UNIX_SOCKET CLAUDE_CODE_OAUTH_TOKEN CLAUDE_SECURESTORAGE_CONFIG_DIR ENABLE_CLAUDEAI_MCP_SERVERS ANTHROPIC_CUSTOM_HEADERS CLAUDE_CODE_PROMPT_CACHE_TTL" ""; do
    run_shim CLAUDE_ROUTE_SET="$set" CLAUDE_ROUTE_ACTIVE=pool ANTHROPIC_BASE_URL=http://api.anthropic.com \
      ANTHROPIC_UNIX_SOCKET="$H/.subrouter/claude.sock" CLAUDE_CODE_OAUTH_TOKEN=ssh-placeholder \
      CLAUDE_SECURESTORAGE_CONFIG_DIR="$H/.claude-route/securestorage" ENABLE_CLAUDEAI_MCP_SERVERS=0 \
      "ANTHROPIC_CUSTOM_HEADERS=X-Subrouter-Agent: claude" CLAUDE_CODE_PROMPT_CACHE_TTL=1h CLAUDE_CONFIG_DIR="$PROFILE" -- auth login
    expect_rc 0; expect_line "CLAUDE_CONFIG_DIR=$PROFILE"; expect_line "CLAUDE_ROUTE_ACTIVE=credentials"
    for k in ANTHROPIC_BASE_URL ANTHROPIC_UNIX_SOCKET CLAUDE_CODE_OAUTH_TOKEN CLAUDE_SECURESTORAGE_CONFIG_DIR ENABLE_CLAUDEAI_MCP_SERVERS ANTHROPIC_CUSTOM_HEADERS CLAUDE_ROUTE_SET; do
      expect_no_out "$k="
    done
  done
  finish
fi

if begin route-login-off-keeps-the-profile-store; then
  # off drops a CLAUDE_CONFIG_DIR inside ~/.subrouter. A login that lost it
  # would land in your own login's store.
  route_shim_setup
  for route in off ""; do
    run_shim CLAUDE_ROUTE="$route" CLAUDE_CONFIG_DIR="$PROFILE" -- auth login
    expect_rc 0; expect_line "CLAUDE_CONFIG_DIR=$PROFILE"; expect_line "CLAUDE_ROUTE_ACTIVE=credentials"
  done
  finish
fi

if begin route-every-credential-command-routes-credentials; then
  route_shim_setup; route_shim_pool
  for cmd in setup-token /login login "auth status"; do
    run_shim CLAUDE_ROUTE_DRYRUN=1 CLAUDE_CONFIG_DIR="$PROFILE" -- $cmd
    expect_rc 0; expect_line "route=credentials"
  done
  for cmd in logout "auth logout"; do
    run_shim CLAUDE_ROUTE_DRYRUN=1 -- $cmd
    expect_rc 0; expect_line "route=credentials"
  done
  finish
fi

if begin route-logout-of-a-pool-profile-is-refused; then
  # The credentials route keeps a pool profile's config dir, so a logout from a
  # shell that points at one would sign the pool's account out.
  route_shim_setup; route_shim_pool
  for route in "" off pool; do
    for cmd in "auth logout" logout; do
      run_shim CLAUDE_ROUTE="$route" CLAUDE_CONFIG_DIR="$PROFILE" -- $cmd
      expect_rc 2
      grep -q "sign out the pool profile" "$C/out" && ok || bad "refusal does not say it would sign a pool profile out"
      expect_no_out "CLAUDE_ROUTE_ACTIVE="
    done
  done
  run_shim CLAUDE_CODE_CONFIG_DIR="$PROFILE" -- auth logout
  expect_rc 2
  finish
fi

if begin route-plain-pool-launch-keeps-the-pool-store; then
  # Only a credential command as $1 leaves the pool: a plain launch, and a
  # prompt that says login, keep the login-less store.
  route_shim_setup; route_shim_pool
  for args in "-p hi" "-p login"; do
    run_shim -- $args
    expect_rc 0; expect_line "CLAUDE_ROUTE_ACTIVE=pool"
    expect_line "CLAUDE_SECURESTORAGE_CONFIG_DIR=$H/.claude-route/securestorage"
    expect_line "ANTHROPIC_UNIX_SOCKET=$H/.subrouter/claude.sock"; expect_line "CLAUDE_CODE_OAUTH_TOKEN=ssh-placeholder"
  done
  finish
fi

# check, check codex and test, on a Mac with no Paradev app unless
# route_paradev adds its wrappers. Both shims as install writes them, the
# socket serving, pool mode. The Claude Code and Codex answer the word their
# prompt asks for, and a launch the shim routed to the daemon is a session in
# $H/served, which srq serving reads (FAKE_NO_SESSIONS=1: the daemon has none).
route_launch_setup() {
  route_setup SUBROUTER_CLAUDE_SOCKET="$C/home/.subrouter/claude.sock"
  rm -f "$H/.claude/settings.json"   # it names a route, and a route word refuses then
  sed '/^case "${1:-status}" in/,$d' "$ROUTE_SH" > "$C/route-lib.sh"
  env -i PATH="$C/bin:/usr/bin:/bin" HOME="$H" CLAUDE_ROUTE_HOME="$H/.claude-route" \
    "$LAPTOP_BASH" -c 'source "$1"; write_shim "$HOME/.claude-route/bin/claude" && write_codex_shim "$HOME/.claude-route/bin/codex"' _ "$C/route-lib.sh"
  route_shim_pool
  printf '\n# >>> claude-route >>>\nexport PATH="$HOME/.claude-route/bin:$PATH"\n# <<< claude-route <<<\n' > "$H/.zshrc"
  # A box Codex account, so codex pool routes, and a laptop one, so test pins it.
  printf 'codex\tcx-box\tcx-box\t\tbox\tyes\tno\ncodex\tcx-lap\tcx-lap\t\tlocal\tyes\tno\n' > "$H/.claude-route/accounts.tsv"
  mkdir -p "$H/.local/bin"
  cat > "$H/.local/bin/claude" <<'SH'
#!/bin/bash
sid=""; word=""; prev=""
for a; do
  case "$prev" in --session-id|--resume) sid="$a" ;; esac
  case "$a" in --version) echo "9.9.9 (Claude Code)"; exit 0 ;; "Reply with exactly one word, in lowercase: "*) word="${a##*: }" ;; esac
  prev="$a"
done
[ -n "$sid" ] && [ -n "${ANTHROPIC_UNIX_SOCKET:-}${ANTHROPIC_BASE_URL:-}" ] && echo "$sid acct-pool" >> "$HOME/served"
echo "$word"
SH
  chmod 755 "$H/.local/bin/claude"
  stub codex 'out=""; word=""; pool=""; pin=""; prev=""
for a; do
  case "$prev" in -o) out="$a" ;; esac
  case "$a" in --version) echo "codex-cli 9.9.9"; exit 0 ;; "Reply with exactly one word, in lowercase: "*) word="${a##*: }" ;; model_provider=\"subrouter\") pool=1 ;;
    *X-Subrouter-Account-ID*) pin="${a##*X-Subrouter-Account-ID\"=\"}"; pin="${pin%%\"*}" ;; esac
  prev="$a"
done
[ -n "$word" ] || exit 0
sid="cx-$$-$RANDOM"; echo "session id: $sid"
[ -n "$pool" ] && echo "$sid ${pin:-cx-pool}" >> "$HOME/served"
[ -n "$out" ] && echo "$word" > "$out"
exit 0'
  stub uuidgen 'printf "%08x-%04x-4%03x-8%03x-%012x\n" "$$" "$RANDOM" "$((RANDOM % 4096))" "$((RANDOM % 4096))" "$RANDOM$RANDOM"'
  cat > "$C/srq" <<'SH'
#!/bin/bash
[ "$1" = serving ] && [ -z "${FAKE_NO_SESSIONS:-}" ] || exit 1
a=$(awk -v s="$2" '$1 == s { a = $2 } END { print a }' "$HOME/served" 2>/dev/null)
[ -n "$a" ] && echo "$a"
SH
  chmod 755 "$C/srq"
}
# Paradev's wrappers in ~/${1:-.paradev}/bin: each runs the first other one on
# PATH (the shim), Codex's with Paradev's hooks prefix, and logs the launch.
route_paradev() {
  local tool d="$H/${1:-.paradev}/bin"
  mkdir -p "$d"
  for tool in claude codex; do
    cat > "$d/$tool" <<'SH'
#!/bin/bash
tool="${0##*/}"; self="$(cd "$(dirname "$0")" && pwd -P)"
echo "paradev wrapper $tool $*" >> "$FAKE_LOG"
pre=(); [ "$tool" = codex ] && pre=(--enable hooks --dangerously-bypass-hook-trust)
IFS=: read -r -a dirs <<< "$PATH"
for d in "${dirs[@]}"; do
  [ "$(cd "$d" 2>/dev/null && pwd -P)" = "$self" ] && continue
  [ -x "$d/$tool" ] && exec "$d/$tool" "${pre[@]}" "$@"
done
exit 127
SH
    chmod 755 "$d/$tool"
  done
  stub paradev 'echo "[{\"presetId\": \"claude\", \"args\": [\"--dangerously-skip-permissions\"]}, {\"presetId\": \"codex\", \"promptArgs\": [\"--\"]}]"'
}
run_route_launch() { run_route_as "$ROUTE_SH" CLAUDE_ROUTE_SRQ="$C/srq" "$@"; }
pass_today() { echo "PASS $(date +%Y-%m-%d) 00:00:00 (1 checks)" > "$1"; }
# test's cases about Paradev's wrapper itself: skipped on a Mac without one.
ROUTE_PARADEV_CASES=("HOME with a trailing slash never picks Paradev's wrapper" "Paradev's wrapper execs the shim"
  "Paradev's launch shape is never a word" "a word through Paradev's wrapper"
  "Paradev's presets still put a flag first (claude) and the prompt after -- (codex)"
  "codex local through Paradev's wrapper: prefix kept, word dropped, subrouter URL dropped"
  "Paradev's Codex preset shape (-- prompt) is never a word" "pool via Paradev's wrapper answers" "  ...and the pool served it")
# The summary's counts are the PASS and FAIL lines: a SKIP is neither.
expect_test_counts() {
  local p f cp cf
  read -r p f cp cf <<< "$(sed -n 's/^== claude: \([0-9]*\) passed, \([0-9]*\) failed;  codex: \([0-9]*\) passed, \([0-9]*\) failed ==$/\1 \2 \3 \4/p' "$C/out")"
  [ -n "$cf" ] || { bad "test printed no summary line"; return; }
  [ "$((p + cp))" = "$(grep -c '^  PASS  ' "$C/out")" ] && ok || bad "the summary counts $((p + cp)) passed, the output $(grep -c '^  PASS  ' "$C/out") PASS lines"
  [ "$((f + cf))" = "$(grep -c '^  FAIL  ' "$C/out")" ] && ok || bad "the summary counts $((f + cf)) failed, the output $(grep -c '^  FAIL  ' "$C/out") FAIL lines"
}

if begin route-check-without-paradev-launches-through-the-shim; then
  # check and check codex launched through ~/.paradev/bin unconditionally, so
  # without Paradev they failed and codex pool could not pass its gate. The
  # daemon's socket is moved: route=pool, not pool-tcp, says the shim found it.
  route_launch_setup
  mkdir -p "$H/moved"; mv "$H/.subrouter/claude.sock" "$H/moved/claude.sock"
  moved=(CLAUDE_ROUTE_SOCKET="$H/moved/claude.sock" FAKE_CLAUDE_SOCKET="$H/moved/claude.sock")
  run_route_launch "${moved[@]}" -- check
  expect_rc 0; expect_out "launch: shim $H/.claude-route/bin/claude -> claude, as a new terminal launches it (no Paradev wrapper)"
  expect_out "mode=pool  route=pool  answer=ready  pool account=acct-pool"
  run_route_launch "${moved[@]}" -- check codex
  expect_rc 0; expect_out "launch: shim $H/.claude-route/bin/codex -> codex, as a new terminal launches it (no Paradev wrapper)"
  expect_out "answer=ready  pool account=cx-pool"
  grep -q "^PASS $(date +%Y-%m-%d) .*; CODEX_HOME=$H/.codex)$" "$H/.claude-route/last-codex-check" && ok || bad "last-codex-check: $(cat "$H/.claude-route/last-codex-check")"
  expect_no_log "paradev wrapper"
  pass_today "$H/.claude-route/last-test"; pass_today "$H/.claude-route/last-codex-test"
  run_route_launch "${moved[@]}" -- pool
  expect_rc 0; expect_out "mode: pool. NEW Claude Code launches"
  run_route_launch "${moved[@]}" -- codex pool
  expect_rc 0; expect_out "codex mode: pool. NEW Codex launches"
  finish
fi

if begin route-test-without-paradev-skips-only-the-paradev-cases; then
  route_launch_setup
  run_route_launch -- test
  expect_out "claude launches: shim $H/.claude-route/bin/claude -> claude, as a new terminal launches it (no Paradev wrapper)"
  for name in "${ROUTE_PARADEV_CASES[@]}"; do
    expect_line "  SKIP  $name   (no Paradev wrapper)"
  done
  [ "$(grep -c '^  SKIP  ' "$C/out")" = "${#ROUTE_PARADEV_CASES[@]}" ] && ok || bad "$(grep -c '^  SKIP  ' "$C/out") SKIP lines, want ${#ROUTE_PARADEV_CASES[@]}"
  # Launches that are not about the wrapper went through it; now through the shim.
  expect_line "  PASS  'claude local' answers on your own login"
  expect_line "  PASS    ...logged route=off word=local, and the daemon never saw it"
  expect_line "  PASS  codex pinned through the shim answers"
  expect_line "  PASS    ...on exactly that account"
  expect_test_counts
  finish
fi

if begin route-check-and-test-with-paradev-go-through-its-wrapper; then
  route_launch_setup; route_paradev
  run_route_launch -- check
  expect_rc 0; expect_out "launch: Paradev's wrapper $H/.paradev/bin/claude -> shim -> claude, as a new Paradev session launches it"
  expect_out "pool account=acct-pool"; expect_log "^paradev wrapper claude -p Reply with exactly one word"
  run_route_launch -- check codex
  expect_rc 0; expect_out "launch: Paradev's wrapper $H/.paradev/bin/codex -> shim -> codex, as a new Paradev session launches it"
  expect_out "pool account=cx-pool"; expect_log "^paradev wrapper codex exec --skip-git-repo-check"
  run_route_launch -- test
  expect_out "claude launches: Paradev's wrapper $H/.paradev/bin/claude -> shim -> claude"
  for name in "${ROUTE_PARADEV_CASES[@]}"; do expect_line "  PASS  $name"; done
  expect_no_out "  SKIP  "
  expect_line "  PASS  'claude local' answers on your own login"
  expect_log "^paradev wrapper claude local -p Reply with exactly one word, in lowercase: juliet"
  expect_line "  PASS  codex pinned through Paradev's wrapper answers"
  expect_log "^paradev wrapper codex cx-lap exec --skip-git-repo-check Reply with exactly one word, in lowercase: foxtrot"
  expect_test_counts
  finish
fi

if begin route-check-finds-paradev-build-and-superset-wrappers; then
  # A Paradev build's own home, and Superset's (Paradev's old name).
  route_launch_setup
  for d in .paradev-canary .superset .superset-canary; do
    rm -rf "$H"/.paradev* "$H"/.superset*; route_paradev "$d"
    run_route_launch -- check
    expect_rc 0; expect_out "launch: Paradev's wrapper $H/$d/bin/claude -> shim -> claude, as a new Paradev session launches it"
  done
  finish
fi

if begin route-failed-check-without-paradev-blocks-the-gate; then
  # The daemon serves no session: every check that applies fails, and both
  # gates refuse, though the cases about Paradev's wrapper were skipped.
  route_launch_setup
  run_route_launch FAKE_NO_SESSIONS=1 -- check
  expect_rc 1; expect_out "pool account=none (not through the pool)"
  run_route_launch FAKE_NO_SESSIONS=1 -- check codex
  expect_rc 1; expect_out "pool account=none (not through the pool)"
  grep -q "^FAIL " "$H/.claude-route/last-codex-check" && ok || bad "last-codex-check: $(cat "$H/.claude-route/last-codex-check")"
  pass_today "$H/.claude-route/last-codex-test"
  run_route_launch -- codex pool
  expect_rc 1; expect_out "no passing 'claude-route check codex' today"
  [ "$(cat "$H/.claude-route/codex-mode" 2>/dev/null)" = pool ] && bad "codex mode is pool" || ok
  echo off > "$H/.claude-route/mode"
  run_route_launch FAKE_NO_SESSIONS=1 -- test
  expect_line "  FAIL    ...through the pool   (details: $H/.claude-route/test.err)"
  expect_line "  FAIL    ...and the pool served it   (details: $H/.claude-route/test.err)"
  expect_line "  SKIP  Paradev's wrapper execs the shim   (no Paradev wrapper)"
  expect_test_counts
  grep -q "^FAIL " "$H/.claude-route/last-test" && ok || bad "last-test: $(cat "$H/.claude-route/last-test")"
  run_route_launch -- pool
  expect_rc 1; expect_out "no passing 'claude-route test' today"
  [ "$(cat "$H/.claude-route/mode")" = off ] && ok || bad "mode is $(cat "$H/.claude-route/mode")"
  finish
fi

# --- sr-triage ------------------------------------------------------------------
# Read-only triage. Run under macOS's stock bash, with no
# timeout(1) (stock macOS has none: the stub answers as a missing command does).
TRIAGE="$(cd "$DEPLOY/.." && pwd -P)/bin/sr-triage"
triage_setup() {
  H="$C/home"; mkdir -p "$H/bin" "$H/Library/LaunchAgents"
  stub timeout 'echo "timeout: command not found" >&2; exit 127'
  stub launchctl 'echo "launchctl $*" >> "$FAKE_LOG"; exit 113'
  stub go 'exit 1'
}
# An sr that answers at once; FAKE_SR_HANG=1 one that never does (its pid is
# recorded, so a case can check it was stopped, not abandoned). The hung one
# ignores SIGALRM, as a Go binary does, so an alarm(2) bound cannot stop it,
# and SIGTERM, so only the KILL fallback can; ignored signals stay ignored
# across exec. It outlasts the case's time limit, so a bound that only waits
# for it to end is caught there.
fake_sr() { # path
  mkdir -p "$(dirname "$1")"
  cat > "$1" <<'SH'
#!/bin/bash
if [ -n "${FAKE_SR_HANG:-}" ]; then echo $$ >> "$FAKE_SR_PIDS"; trap '' ALRM TERM; exec /bin/sleep 120; fi
case "$1" in
  --help) echo "usage: sr"; echo "  sr status" ;;
  status) echo "Local (this machine)" ;;
esac
SH
  chmod 755 "$1"
}
run_triage() { # extra env assignments...
  run_sandboxed PATH="$C/bin:/usr/bin:/bin:/usr/sbin:/sbin" HOME="$H" TMPDIR="$C/tmp" FAKE_LOG="$C/log" \
    FAKE_SR_PIDS="$C/sr-pids" "$@" -- "$LAPTOP_BASH" "$TRIAGE"
}

echo "== sr-triage"

if begin triage-rc-exports-names-only; then
  # Its header promises no secret values; the rc-file section printed these.
  triage_setup
  cat > "$H/.zshrc" <<'RC'
export ANTHROPIC_API_KEY=sk-ant-api03-SECRETONE
export ANTHROPIC_AUTH_TOKEN="SECRETTWO"
export SUBROUTER_REMOTE_POOL_TOKEN=SECRETTHREE
export ANTHROPIC_BASE_URL=http://127.0.0.1:31415
RC
  printf 'export CODEX_HOME=/tmp/codex SUBROUTER_REMOTE_POOL_TOKEN=SECRETFOUR\n' > "$H/.zshenv"
  run_triage
  expect_rc 0
  expect_no_out SECRETONE; expect_no_out SECRETTWO; expect_no_out SECRETTHREE; expect_no_out SECRETFOUR
  expect_out "$H/.zshrc:1: export ANTHROPIC_API_KEY=<not shown>"
  expect_out "$H/.zshrc:3: export SUBROUTER_REMOTE_POOL_TOKEN=<not shown>"
  expect_out "$H/.zshenv:1: export CODEX_HOME=<not shown> SUBROUTER_REMOTE_POOL_TOKEN=<not shown>"
  finish
fi

if begin triage-runs-without-timeout; then
  triage_setup; fake_sr "$C/bin/sr"
  run_triage
  expect_rc 0; expect_out "sr --help: exit=0"; expect_out "sr status:             exit=0"
  expect_out "Local (this machine)"
  finish
fi

if begin triage-bounds-a-hung-sr; then
  # The probes are bounded without timeout(1), and the hung sr is stopped. With
  # `sleep` stubbed the bound's polls take no time, so a passing run takes a few
  # seconds; one that waits for the hung sr to end takes 120 s a probe.
  triage_setup; fake_sr "$C/bin/sr"
  started=$SECONDS
  run_triage FAKE_SR_HANG=1
  took=$((SECONDS - started))
  [ "$took" -lt 60 ] && ok || bad "triage took ${took}s: the bound waited for the hung sr instead of stopping it"
  expect_rc 0; expect_out "sr --help: exit=124"; expect_out "sr status:             exit=124"
  if [ -s "$C/sr-pids" ]; then
    left=""; for p in $(cat "$C/sr-pids"); do kill -0 "$p" 2>/dev/null && { left="$left $p"; kill -KILL "$p"; }; done
    [ -z "$left" ] && ok || bad "a hung sr is still running:$left"
  else
    bad "the hung sr never ran"
  fi
  finish
fi

if begin triage-stopped-takes-its-probe-with-it; then
  # Stopping triage mid-probe must stop the probed sr too: a background job of
  # a non-interactive shell ignores SIGINT, so Ctrl-C reaches triage but not
  # the hung sr. SIGTERM to triage alone takes the same exit path, and unlike
  # SIGINT to it alone (bash waits out a child that did not die of it), it
  # stops triage every time.
  triage_setup; fake_sr "$C/bin/sr"
  cat > "$C/interrupt.py" <<'PY'
import os, signal, subprocess, sys, time
pids = os.environ["FAKE_SR_PIDS"]
triage = subprocess.Popen(sys.argv[1:])
deadline = time.time() + 20
while time.time() < deadline and not (os.path.exists(pids) and os.path.getsize(pids)):
    time.sleep(0.05)
os.kill(triage.pid, signal.SIGTERM)
print("triage stopped:", triage.wait(20))
PY
  run_sandboxed PATH="$C/bin:/usr/bin:/bin:/usr/sbin:/sbin" HOME="$H" TMPDIR="$C/tmp" FAKE_LOG="$C/log" \
    FAKE_SR_PIDS="$C/sr-pids" FAKE_SR_HANG=1 -- "$PY3" "$C/interrupt.py" "$LAPTOP_BASH" "$TRIAGE"
  expect_out "triage stopped:"
  if [ -s "$C/sr-pids" ]; then
    # The parent's exit can lag the kill; a process that is gone stays gone.
    left=""; for p in $(cat "$C/sr-pids"); do
      for _ in 1 2 3 4 5 6 7 8 9 10; do kill -0 "$p" 2>/dev/null || break; sleep 0.5; done
      kill -0 "$p" 2>/dev/null && { left="$left $p"; kill -KILL "$p"; }
    done
    [ -z "$left" ] && ok || bad "the probed sr outlived triage:$left"
  else
    bad "the hung sr never ran"
  fi
  finish
fi

if begin triage-tells-a-stand-in-from-the-daemon; then
  # A stand-in at the job's Claude socket is its own state: not OK, not down (PATCHES §84).
  triage_setup
  "$PY3" -c 'import os, plistlib, sys; plistlib.dump({"Label": os.environ["TEST_JOB"], "ProgramArguments": ["/nonexistent/subrouter", "serve"], "EnvironmentVariables": {"SUBROUTER_CLAUDE_SOCKET": sys.argv[2]}}, open(sys.argv[1], "wb"))' \
    "$H/Library/LaunchAgents/$TEST_JOB.plist" "$H/.subrouter/claude.sock"
  echo new > "$C/running"
  run_triage FAKE_RUNNING="$C/running" FAKE_CLAUDE_SOCKET="$H/.subrouter/claude.sock" FAKE_CLAUDE_WORD=standby
  expect_rc 0; expect_out "socket:   STAND-IN at $H/.subrouter/claude.sock"; expect_no_out "socket:   OK"
  run_triage FAKE_RUNNING="$C/running" FAKE_CLAUDE_SOCKET="$H/.subrouter/claude.sock"
  expect_rc 0; expect_out "socket:   OK ($H/.subrouter/claude.sock)"; expect_no_out "STAND-IN"
  : > "$C/running"
  run_triage FAKE_RUNNING="$C/running" FAKE_CLAUDE_SOCKET="$H/.subrouter/claude.sock"
  expect_rc 0; expect_out "socket:   NOT answering at $H/.subrouter/claude.sock"
  finish
fi

if begin triage-finds-sr-in-home-bin; then
  # setup installs ~/bin/subrouter and ~/bin/sr; ~/bin is not on PATH.
  triage_setup; fake_sr "$H/bin/subrouter"; ln -s subrouter "$H/bin/sr"
  run_triage
  expect_rc 0; expect_out "which:     NOT ON PATH"; expect_out "installed at $H/bin/sr"
  expect_out "binary:    $H/bin/subrouter"; expect_no_out "binary:    MISSING"
  expect_out "sr --help: exit=0"
  finish
fi

if begin triage-finds-the-launchagent-binary; then
  # No sr anywhere, but the LaunchAgent starts a binary that exists.
  triage_setup; rmdir "$H/bin"; fake_sr "$C/opt/subrouter"
  H_PLIST="$H/Library/LaunchAgents/$TEST_JOB.plist"
  "$PY3" -c 'import os, plistlib, sys; plistlib.dump({"Label": os.environ["TEST_JOB"], "ProgramArguments": [sys.argv[2], "serve"]}, open(sys.argv[1], "wb"))' \
    "$H_PLIST" "$C/opt/subrouter"
  run_triage
  expect_rc 0; expect_out "binary:    $C/opt/subrouter"; expect_no_out "binary:    MISSING"
  finish
fi

if begin triage-never-installed-is-missing; then
  # Calibration: nothing installed still reads as MISSING.
  triage_setup; run_triage
  expect_rc 0; expect_out "which:     NOT ON PATH"; expect_out "binary:    MISSING"
  # No sr, so no help was read: that is not a locked build.
  expect_out "help shows remote verbs? not checked (no sr found)"; expect_no_out "LOCKED"
  finish
fi

# --- the harness itself (PATCHES §76.6) ---------------------------------------
if begin harness-cleanup-signals-only-its-own-runs; then
  # A pid file can name a pid that is another process's by then, or 0 or 1,
  # whose groups are this test's own and every process this user owns. kill is
  # a function here, so the case records what the cleanup would signal and
  # signals nothing; the pid files are gone before this test's own cleanup.
  D="$C/home/.subrouter/transitions"; mkdir -p "$D"
  in_own_session() { # pid: its process group is its own pid
    local n=0
    until [ "$(ps -o pgid= -p "$1" 2>/dev/null | tr -d ' ')" = "$1" ] || [ "$n" -ge 600 ]; do /bin/sleep 0.05; n=$((n + 1)); done
  }
  # A detached run of this test: a session of its own, this test's dir in its arguments.
  "$PY3" -c 'import os, time; os.setsid(); time.sleep(120)' "$D/run" & RUN=$!
  # Another process: a session of its own, nothing of this test's in its arguments.
  "$PY3" -c 'import os, time; os.setsid(); time.sleep(120)' & OTHER=$!
  # A process in this test's own group, this test's dir in its arguments.
  "$PY3" -c 'import time; time.sleep(120)' "$D/own" & OWN=$!
  in_own_session "$RUN"; in_own_session "$OTHER"
  echo "$RUN" > "$D/run.pid"; echo "$OTHER" > "$D/other.pid"; echo "$OWN" > "$D/own.pid"
  echo 0 > "$D/zero.pid"; echo 1 > "$D/one.pid"; echo none > "$D/none.pid"
  kill() { echo "kill $*" >> "$C/signalled"; }
  stop_detached_runs
  unset -f kill
  rm -f "$D/"*.pid
  want="kill -STOP -- -$RUN
kill -KILL -- -$RUN"
  [ "$(cat "$C/signalled" 2>/dev/null)" = "$want" ] && ok ||
    bad "the cleanup would signal [$(tr '\n' ';' < "$C/signalled" 2>/dev/null)], want only the run's group $RUN"
  kill -9 "$RUN" "$OTHER" "$OWN" 2>/dev/null; wait "$RUN" "$OTHER" "$OWN" 2>/dev/null
  finish
fi

if begin scripts-default-to-the-live-job; then
  # Every other case gives the scripts a label and address of this test's own,
  # so none runs one on its defaults. This case reads each default a script
  # would use with the variable unset, and runs no script.
  want_default() { case "$1" in
    SUBROUTER_LABEL) echo ai.manaflow.subrouter ;;
    SUBROUTER_ADDR) echo 127.0.0.1:31415 ;;
    CLAUDE_ROUTE_TCP) echo http://127.0.0.1:31415 ;;
  esac; }
  while IFS='|' read -r script names; do
    found=""
    while IFS= read -r expr; do
      [ -n "$expr" ] || continue
      name=${expr#??}; name=${name%%:-*}
      got=$(env -i /bin/bash -c "printf %s \"$expr\"")
      [ "$got" = "$(want_default "$name")" ] && ok || bad "$(basename "$script"): $expr gives '$got', want '$(want_default "$name")'"
      found="$found $name"
    done <<EOF
$(grep -oE '\$\{(SUBROUTER_LABEL|SUBROUTER_ADDR|CLAUDE_ROUTE_TCP):-[^}]*\}' "$script" | sort -u)
EOF
    for name in $names; do
      case " $found " in *" $name "*) ok ;; *) bad "$(basename "$script") has no default for $name" ;; esac
    done
  done <<EOF
$LAPTOP_SH|SUBROUTER_LABEL SUBROUTER_ADDR
$DEPLOY/macos/laptop-deploy-lib.sh|SUBROUTER_ADDR
$DEPLOY/macos/rollback-laptop-peer.sh|SUBROUTER_LABEL SUBROUTER_ADDR
$ACT_SH|SUBROUTER_LABEL SUBROUTER_ADDR
$TRIAGE|SUBROUTER_LABEL SUBROUTER_ADDR
$ROUTE_SH|CLAUDE_ROUTE_TCP
EOF
  finish
fi

# --- the watchdog's stop marker (PATCHES §81) ----------------------------------
# A bootout by these scripts is a stop on purpose, so they announce it to the
# watchdog first and withdraw it once their own bootstrap worked. The stand-in
# launchd's bootout and bootstrap are wrapped to log the marker they find.
STOP_MARKER_REL=".subrouter/watchdog-stop.$TEST_JOB"
watch_the_marker() {
  mv "$C/bin/launchctl" "$C/bin/launchctl.rig"
  stub launchctl '
case "$1" in bootout|bootstrap)
  m="$HOME/'"$STOP_MARKER_REL"'"
  if [ -f "$m" ]; then echo "marker at $1: $(tr "\n" " " < "$m")now=$(date +%s)" >> "$FAKE_LOG"
  else echo "marker at $1: none" >> "$FAKE_LOG"; fi ;;
esac
exec "$(dirname "$0")/launchctl.rig" "$@"'
}
marker_seconds_left() { # log line regex: until minus now, as that line logged them
  grep -E -- "$1" "$C/log" | head -1 | sed -n 's/.*until=\([0-9]*\) .*now=\([0-9]*\).*/\1 \2/p' | { read -r u n; echo $((u - n)); }
}

if begin laptop-announces-its-bootout-to-the-watchdog; then
  laptop_setup pre post; watch_the_marker; run_laptop RELOAD=1
  expect_rc 0; expect_out "DEPLOY OK"
  expect_log "^marker at bootout: label=$TEST_JOB until=[0-9]+ by=deploy-laptop.sh "
  expect_log "^marker at bootstrap: label=$TEST_JOB until=[0-9]+ "
  # Before the bootout it covers the whole wait for launchd (600 s here) and
  # the bootstrap after it; once the job is gone, three minutes.
  left=$(marker_seconds_left "^marker at bootout: ")
  [ "$left" -gt 600 ] && [ "$left" -le 790 ] && ok || bad "the marker at the bootout had ${left}s left, want 601 to 790"
  left=$(marker_seconds_left "^marker at bootstrap: ")
  [ "$left" -gt 0 ] && [ "$left" -le 180 ] && ok || bad "the marker at the bootstrap had ${left}s left, want 1 to 180"
  expect_no_file "$H/$STOP_MARKER_REL"
  finish
fi

if begin laptop-leaves-a-short-marker-when-its-bootstrap-fails; then
  # A run that cannot load the job again leaves the marker to expire within
  # three minutes, and the watchdog then loads the job.
  laptop_setup pre post; watch_the_marker; run_laptop RELOAD=1 FAKE_BOOTSTRAP_FAILS=1
  expect_rc 2; expect_out "STILL DOWN"
  expect_file "$H/$STOP_MARKER_REL"
  left=$(( $(sed -n 's/^until=//p' "$H/$STOP_MARKER_REL" 2>/dev/null) - $(date +%s) ))
  [ "$left" -gt 0 ] && [ "$left" -le 180 ] && ok || bad "the marker left behind has ${left}s left, want 1 to 180"
  finish
fi

# No case may hand the marker code the live job's label: a marker for it is
# the one the live watchdog reads. finish cannot see one, so each run looks
# over its whole test dir after its cases.
live_stop_markers() { # dir
  find "$1" \( -name 'watchdog-stop.ai.manaflow.subrouter' -o -name 'watchdog-stop.ai.manaflow.subrouter.*' \) 2>/dev/null
}

if begin live-stop-marker-is-seen-anywhere-in-a-case; then
  mkdir -p "$C/home/.subrouter"; : > "$C/home/.subrouter/watchdog-stop.ai.manaflow.subrouter.42"
  [ -n "$(live_stop_markers "$T")" ] && ok || bad "a stop marker for the live job went unseen"
  rm -f "$C/home/.subrouter/watchdog-stop.ai.manaflow.subrouter.42"
  [ -z "$(live_stop_markers "$T")" ] && ok || bad "a removed marker is still reported"
  finish
fi

CASE=no-case-wrote-a-stop-marker-for-the-live-job
LIVE_STOP_MARKER=$(live_stop_markers "$T" | head -1)
[ -z "$LIVE_STOP_MARKER" ] || bad "a case wrote a stop marker for the live job: ${LIVE_STOP_MARKER#"$T"/}"

echo
echo "deploy script tests: $PASS assertions passed, $FAILED failed"
if [ "$FAILED" -ne 0 ]; then
  echo "failing cases:$(printf '%s\n' $FAIL_LIST | sort -u | tr '\n' ' ')"
  exit 1
fi
[ "$PASS" -gt 0 ] || { echo "no case matched '$FILTER'" >&2; exit 1; }
exit 0
