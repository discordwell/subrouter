#!/usr/bin/env bash
# Rehearse the laptop supervisor tooling end to end against a scratch
# LaunchAgent, with real builds and a real launchd:
#
#   bash ops/deploy/tests/rehearse-laptop-supervisor.sh <build-a> <build-b> <scratch-dir>
#
# build-a and build-b are two builds of this tree that differ (a different -X
# version is enough), each ad-hoc signed with the identifier subrouter
# (codesign -s - -f -i subrouter), as the scripts sign what they install.
#
# Results go to <scratch-dir>/results; the scratch job is booted out and its
# home removed at the end (KEEP=1 keeps it).
#
# Nothing outside <scratch-dir> and /private/tmp/srsv-<pid> is written. The
# label is test.subrouter.rehearsal.<pid>, the ports are 127.0.0.1:31499 (the
# daemon) and 31498 (the peer), and the job's HOME, SUBROUTER_STATE_DIR,
# CLAUDE_CONFIG_DIR, SUBROUTER_CLOUD_CONFIG and CODEX_HOME are all inside
# <scratch-dir>. Every script runs under env -i with HOME there too.
set -uo pipefail

A="${1:?usage: $0 <build-a> <build-b> <scratch-dir>}"
B="${2:?usage: $0 <build-a> <build-b> <scratch-dir>}"
ROOT="${3:?usage: $0 <build-a> <build-b> <scratch-dir>}"
HERE=$(cd "$(dirname "$0")" && pwd -P)
MACOS=$(cd "$HERE/../macos" && pwd -P)
ROUTE=$(cd "$HERE/../../bin" && pwd -P)/claude-route
PY=/usr/bin/python3

mkdir -p "$ROOT" && ROOT=$(cd "$ROOT" && pwd -P) || exit 1
case "$ROOT" in "$HOME"|"$HOME/.subrouter"*|"$HOME/.claude"*|"$HOME/.codex"*|"$HOME/.config"*|"$HOME/bin"*|"$HOME/Library"*|/)
    echo "refusing scratch dir $ROOT" >&2; exit 2 ;;
esac
JOB="test.subrouter.rehearsal.$$"
LABEL="gui/$(id -u)/$JOB"
PORT=31499; PEER_PORT=31498; ADDR="127.0.0.1:$PORT"
SOCKS="/private/tmp/srsv-$$"
H="$ROOT/home"
PLIST="$ROOT/$JOB.plist"
RES="$ROOT/results"
LOGS="$ROOT/logs"
CLAUDE_SOCK="$SOCKS/claude.sock"
CONTROL="$SOCKS/sv.sock"

for port in $PORT $PEER_PORT; do
    if curl -s -m 1 -o /dev/null "http://127.0.0.1:$port/"; then echo "127.0.0.1:$port is in use" >&2; exit 2; fi
done
rm -rf "$RES"; mkdir -p "$RES" "$LOGS" "$H/bin" "$H/.subrouter/codex" "$H/.claude" "$H/.codex" "$H/.config/subrouter" || exit 1
mkdir -m 700 "$SOCKS" || exit 1

PEER_PID=""; PROBE_PID=""
cleanup() {
    [ -n "$PROBE_PID" ] && kill "$PROBE_PID" 2>/dev/null
    local pid
    for pid in "$H"/.subrouter/transitions/*.pid; do
        { [ -e "$pid" ] && [ ! -e "${pid%.pid}.rc" ]; } || continue
        echo "cleanup: stopping the detached run ${pid%.pid}" >&2
        kill -9 -- "-$(ps -o pgid= -p "$(cat "$pid")" | tr -d ' ')" 2>/dev/null
    done
    launchctl bootout "$LABEL" 2>/dev/null
    for _ in $(seq 1 150); do launchctl print "$LABEL" >/dev/null 2>&1 || break; sleep 5; done
    [ -n "$PEER_PID" ] && kill "$PEER_PID" 2>/dev/null
    rm -rf "$SOCKS"
    [ -n "${KEEP:-}" ] || rm -rf "$H" "$PLIST" "$PLIST.supervisor-mutation.lock"
}
trap cleanup EXIT

PASS=0; FAILS=0
pass() { echo "  PASS  $1"; PASS=$((PASS + 1)); }
failed() { echo "  FAIL  $1"; FAILS=$((FAILS + 1)); }
expect() {
    local what="$1"; shift
    if "$@"; then pass "$what"; else failed "$what"; fi
}

run() {
    local name="$1"; shift
    env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME="$H" TMPDIR="$ROOT/tmp" \
        SUBROUTER_LABEL="$JOB" SUBROUTER_PLIST="$PLIST" SUBROUTER_ADDR="$ADDR" \
        SUBROUTER_CONTROL_SOCKET="$CONTROL" "$@" > "$RES/$name.txt" 2>&1
    local rc=$?
    echo "$rc" > "$RES/$name.rc"
    return $rc
}
mkdir -p "$ROOT/tmp"

health_ok() { curl -s -m 3 "http://$ADDR/_subrouter/health" | grep -q '"ok": *true'; }
sv_status() { curl -s -m 5 --unix-socket "$CONTROL" http://supervisor/_subrouter/supervisor-status; }
job_pid() { launchctl print "$LABEL" 2>/dev/null | sed -n 's/^[[:space:]]*pid = \([0-9]*\).*/\1/p' | head -1; }
cdhash() { codesign -dvvv "$1" 2>&1 | sed -n 's/^CDHash=//p'; }

stream() {
    local name="$1" door="$2" secs="$3" args=()
    if [ "$door" = socket ]; then args=(--unix-socket "$CLAUDE_SOCK" http://api.anthropic.com/v1/messages)
    else args=("http://$ADDR/v1/messages"); fi
    ( start=$(date +%s)
      curl -sS -N -m $((secs + 120)) -X POST -H 'content-type: application/json' -H 'anthropic-version: 2023-06-01' \
          -H "X-Stream-Seconds: $secs" -H "X-Rehearsal-Tag: $name" \
          -d '{"model":"claude-haiku-4-5","max_tokens":1,"stream":true,"messages":[{"role":"user","content":"x"}]}' \
          "${args[@]}" > "$RES/stream-$name.body" 2> "$RES/stream-$name.err"
      echo "curl_exit=$? seconds=$(( $(date +%s) - start ))" > "$RES/stream-$name.verdict" ) &
}
stream_done() {
    local v; v=$(cat "$RES/stream-$1.verdict" 2>/dev/null)
    echo "    stream $1: $v, pings=$(grep -c '^event: ping' "$RES/stream-$1.body" 2>/dev/null), message_stop=$(grep -c '^event: message_stop' "$RES/stream-$1.body" 2>/dev/null) $(head -c 200 "$RES/stream-$1.err" 2>/dev/null)"
    case "$v" in "curl_exit=0 "*) ;; *) return 1 ;; esac
    [ "$(grep -c '^event: message_stop' "$RES/stream-$1.body")" = 1 ] && [ "$(grep -c '^event: ping' "$RES/stream-$1.body")" -ge $(($2 * 2 - 1)) ]
}
await_streams() {
    local n
    for n in "$@"; do
        for _ in $(seq 1 600); do [ -s "$RES/stream-$n.verdict" ] && break; sleep 0.5; done
    done
}

probe_start() {
    "$PY" -I "$HERE/rehearsal.py" probe "$RES/probe-$1.jsonl" "$ADDR" "$CLAUDE_SOCK" &
    PROBE_PID=$!
    sleep 1
}
probe_stop() {
    sleep 2
    kill "$PROBE_PID"; wait "$PROBE_PID" 2>/dev/null; PROBE_PID=""
    echo "    clients during $1:"; sed 's/^/      /' "$RES/probe-$1.jsonl.summary"
}
no_long_retry_after() { grep -q '^503 with Retry-After > 60: 0$' "$RES/probe-$1.jsonl.summary"; }
loaded_runs() { launchctl print "$LABEL" 2>/dev/null | sed -n '/^	arguments = {/,/^	}/p' | grep -q "^		$1\$"; }

run_killed() { # name <the detached log line to kill at> command...
    local name="$1" at="$2" fg; shift 2
    set -m
    run "$name" "$@" &
    fg=$!
    set +m
    DETACHED_LOG=""
    for _ in $(seq 1 600); do
        DETACHED_LOG=$(sed -n 's/^   log: \(.*\.log\)   exit code: .*/\1/p' "$RES/$name.txt" 2>/dev/null)
        [ -n "$DETACHED_LOG" ] && grep -q "$at" "$DETACHED_LOG" 2>/dev/null && break
        sleep 0.2
    done
    kill -9 -- "-$fg" 2>/dev/null
    wait "$fg" 2>/dev/null; FG_RC=$?
    DETACHED_RC=""
    if [ -z "$DETACHED_LOG" ] || ! grep -q "$at" "$DETACHED_LOG"; then
        echo "    no detached run reached /$at/; $name printed:"; sed 's/^/      /' "$RES/$name.txt"
        return
    fi
    echo "    SIGKILLed the shell's process group (pid $fg) after: $(grep "$at" "$DETACHED_LOG" | head -1)"
    for _ in $(seq 1 2400); do [ -e "${DETACHED_LOG%.log}.rc" ] && break; sleep 0.25; done
    DETACHED_RC=$(cat "${DETACHED_LOG%.log}.rc" 2>/dev/null)
}

echo "== setup: scratch job $JOB on $ADDR, sockets in $SOCKS =="
head -c 32 /dev/urandom | base64 > "$H/.subrouter/codex/remote.key"; chmod 600 "$H/.subrouter/codex/remote.key"
"$PY" -I "$HERE/rehearsal.py" peer "$PEER_PORT" "$RES/peer.log" > "$LOGS/peer.out" 2>&1 &
PEER_PID=$!
cp "$A" "$H/bin/subrouter" && codesign -s - -f -i subrouter "$H/bin/subrouter" 2>/dev/null
"$PY" - "$PLIST" "$JOB" "$H" "$ADDR" "$CLAUDE_SOCK" "$SOCKS/drain.sock" "$PEER_PORT" "$LOGS" <<'PY'
import plistlib, sys
path, job, home, addr, claude, drain, peer, logs = sys.argv[1:9]
with open(path, "wb") as f:
    plistlib.dump({
        "Label": job,
        "ProgramArguments": [home + "/bin/subrouter", "serve", "--addr", addr, "--transcripts", home + "/.subrouter/transcripts"],
        "EnvironmentVariables": {
            "HOME": home, "PATH": "/usr/bin:/bin",
            "SUBROUTER_STATE_DIR": home + "/.subrouter", "CLAUDE_CONFIG_DIR": home + "/.claude",
            "SUBROUTER_CLOUD_CONFIG": home + "/.config/subrouter/cloud.json", "CODEX_HOME": home + "/.codex",
            "SUBROUTER_CLAUDE_SOCKET": claude, "SUBROUTER_DRAIN_SOCKET": drain,
            "SUBROUTER_REMOTE_POOL": "http://127.0.0.1:" + peer, "SUBROUTER_REMOTE_POOL_TOKEN": "rehearsal-peer-token",
        },
        "KeepAlive": True, "RunAtLoad": True, "WorkingDirectory": home,
        "StandardOutPath": logs + "/daemon.out.log", "StandardErrorPath": logs + "/daemon.err.log",
    }, f)
PY
cp "$PLIST" "$RES/plist.direct"
launchctl bootstrap "gui/$(id -u)" "$PLIST" || exit 1
for _ in $(seq 1 60); do health_ok && break; sleep 0.5; done
expect "the scratch job answers on TCP" health_ok
stream warmup-tcp tcp 1; stream warmup-socket socket 1; await_streams warmup-tcp warmup-socket
expect "a turn streams through the scratch job over TCP" stream_done warmup-tcp 1
expect "a turn streams through the scratch job over the Claude socket" stream_done warmup-socket 1
echo "    launchd: $(launchctl print "$LABEL" | grep -E 'exit timeout' | xargs)"
probe_start steady; sleep 5; probe_stop steady

echo "== activate =="
run activate-dry-run "$MACOS/activate-laptop-supervisor.sh" "$A" --peer-tailnet rehearsal=192.0.2.10 --dry-run
expect "dry run exits 0" test "$(cat "$RES/activate-dry-run.rc")" = 0
expect "dry run changed nothing" cmp -s "$RES/plist.direct" "$PLIST"
PID_BEFORE=$(job_pid)
stream activation-tcp tcp 30
sleep 2
probe_start activation
run activate "$MACOS/activate-laptop-supervisor.sh" "$A" --peer-tailnet rehearsal=192.0.2.10 --wait-quiet 30
probe_stop activation
sed 's/^/    | /' "$RES/activate.txt"
expect "activation exits 0" test "$(cat "$RES/activate.rc")" = 0
expect "the plist now runs supervise" grep -q '<string>supervise</string>' "$PLIST"
expect "the job restarted (pid $PID_BEFORE -> $(job_pid))" test "$PID_BEFORE" != "$(job_pid)"
expect "supervisor-status runs build-a" test "$(sv_status | "$PY" -c 'import json,sys; print(json.load(sys.stdin)["active_worker"]["executable_identity"])')" = "$(cdhash "$A")"
expect "clients never saw a 503 with Retry-After > 60 during activation" no_long_retry_after activation
await_streams activation-tcp
stream_done activation-tcp 30 && echo "    (the stream open across the activation finished)" || echo "    (the stream open across the activation was cut, as expected for a plain serve)"
cp "$PLIST" "$RES/plist.supervised"; cp "$H/.subrouter/worker-config.json" "$RES/worker-config.json"

echo "== supervised deploy of build-b, with streams open =="
SUP_PID=$(job_pid)
stream deploy-tcp tcp 30; stream deploy-socket socket 30
sleep 3
probe_start deploy
run deploy "$MACOS/deploy-laptop.sh" "$B" &
DEPLOY_JOB=$!
sleep 8
sv_status > "$RES/supervisor-status.mid-deploy.json"
wait "$DEPLOY_JOB"
await_streams deploy-tcp deploy-socket
probe_stop deploy
sed 's/^/    | /' "$RES/deploy.txt"
expect "deploy-laptop.sh exits 0 (DEPLOY OK)" grep -q '^DEPLOY OK' "$RES/deploy.txt"
expect "the deploy upgraded, never restarted (supervisor pid $SUP_PID unchanged)" test "$SUP_PID" = "$(job_pid)"
expect "supervisor-status runs build-b" test "$(sv_status | "$PY" -c 'import json,sys; print(json.load(sys.stdin)["active_worker"]["executable_identity"])')" = "$(cdhash "$H/bin/subrouter")"
expect "the installed worker is build-b" test "$(cdhash "$H/bin/subrouter")" = "$(cdhash "$B")"
expect "the TCP stream open across the deploy finished" stream_done deploy-tcp 30
expect "the Claude socket stream open across the deploy finished" stream_done deploy-socket 30
expect "mid-deploy, the retired generation still held both streams" "$PY" -c 'import json,sys
s = json.load(open(sys.argv[1]))
sys.exit(0 if any(not b["active"] and b["connections"] >= 2 for b in s["backends"]) else 1)' "$RES/supervisor-status.mid-deploy.json"
expect "clients never saw a 503 with Retry-After > 60 during the deploy" no_long_retry_after deploy

echo "== refusals on the supervised job =="
SUP_PID=$(job_pid)
cp "$PLIST" "$RES/plist.before-refusals"
run refuse-reload env RELOAD=1 "$MACOS/deploy-laptop.sh" "$B"
expect "RELOAD=1 deploy-laptop.sh is refused (exit 1)" test "$(cat "$RES/refuse-reload.rc")" = 1
expect "  ...and says why" grep -q 'reads its flags and environment (SUBROUTER_CLAUDE_SOCKET among them) only when it starts' "$RES/refuse-reload.txt"
mkdir -p "$H/.claude-route"
run refuse-socket-off env CLAUDE_ROUTE_HOME="$H/.claude-route" CLAUDE_ROUTE_PLIST="$PLIST" CLAUDE_ROUTE_SOCKET="$CLAUDE_SOCK" \
    CLAUDE_ROUTE_TCP="http://$ADDR" CLAUDE_ROUTE_DEPLOY="$MACOS/deploy-laptop.sh" "$ROUTE" socket-off
expect "claude-route socket-off is refused (exit 1)" test "$(cat "$RES/refuse-socket-off.rc")" = 1
expect "  ...and says why" grep -q 'reads SUBROUTER_CLAUDE_SOCKET only when it starts' "$RES/refuse-socket-off.txt"
run socket-on-noop env CLAUDE_ROUTE_HOME="$H/.claude-route" CLAUDE_ROUTE_PLIST="$PLIST" CLAUDE_ROUTE_SOCKET="$CLAUDE_SOCK" \
    CLAUDE_ROUTE_TCP="http://$ADDR" CLAUDE_ROUTE_DEPLOY="$MACOS/deploy-laptop.sh" "$ROUTE" socket-on
expect "claude-route socket-on finds the socket already served through the supervisor" grep -q 'already serving' "$RES/socket-on-noop.txt"
mkdir -p "$ROOT/cutover-backup"; cp "$RES/plist.direct" "$ROOT/cutover-backup/plist.bak"; cp "$A" "$ROOT/cutover-backup/subrouter.bak"
echo "$ROOT/cutover-backup" > "$ROOT/cutover-ptr"
run refuse-peer-rollback env LAPTOP_BACKUP_PTR="$ROOT/cutover-ptr" "$MACOS/rollback-laptop-peer.sh"
expect "rollback-laptop-peer.sh is refused (exit 1)" test "$(cat "$RES/refuse-peer-rollback.rc")" = 1
"$PY" -c 'import fcntl, os, sys, time
fd = os.open(sys.argv[1], os.O_WRONLY | os.O_CREAT, 0o600)
fcntl.flock(fd, fcntl.LOCK_EX)
open(sys.argv[2], "w").close()
time.sleep(120)' "$PLIST.supervisor-mutation.lock" "$ROOT/lock-held" &
HOLDER=$!
for _ in $(seq 1 100); do [ -e "$ROOT/lock-held" ] && break; sleep 0.1; done
run refuse-locked "$MACOS/deploy-laptop.sh" "$B"
kill "$HOLDER"; wait "$HOLDER" 2>/dev/null
expect "deploy-laptop.sh is refused while another run holds the mutation lock (exit 1)" test "$(cat "$RES/refuse-locked.rc")" = 1
expect "  ...and names the lock" grep -q 'another deploy, rollback or update holds .*supervisor-mutation.lock' "$RES/refuse-locked.txt"
run activate-again "$MACOS/activate-laptop-supervisor.sh" "$B"
expect "a second activation is a no-op" grep -q 'already runs supervise; nothing to do' "$RES/activate-again.txt"
expect "no refusal touched the plist" cmp -s "$RES/plist.before-refusals" "$PLIST"
expect "no refusal restarted the supervisor (pid $SUP_PID)" test "$SUP_PID" = "$(job_pid)"
run check "$MACOS/deploy-laptop.sh" "$H/bin/subrouter" --check
echo "    deploy-laptop.sh --check: $(tail -1 "$RES/check.txt")"

echo "== claude-route socket-off and socket-on, through deploy-laptop.sh =="
route() { run "$1" env CLAUDE_ROUTE_HOME="$H/.claude-route" CLAUDE_ROUTE_PLIST="$PLIST" CLAUDE_ROUTE_SOCKET="$CLAUDE_SOCK" \
    CLAUDE_ROUTE_TCP="http://$ADDR" "$ROUTE" "${@:2}"; }
plist_has_socket() { "$PY" -c 'import plistlib, sys; sys.exit(0 if "SUBROUTER_CLAUDE_SOCKET" in plistlib.load(open(sys.argv[1], "rb"))["EnvironmentVariables"] else 1)' "$PLIST"; }
plist_lacks_socket() { ! plist_has_socket; }
launchd_lacks_socket() { ! launchctl print "$LABEL" | grep -q SUBROUTER_CLAUDE_SOCKET; }
route socket-off socket-off --restart-supervisor
sed 's/^/    | /' "$RES/socket-off.txt"
expect "claude-route socket-off --restart-supervisor exits 0" test "$(cat "$RES/socket-off.rc")" = 0
expect "  ...the plist names no Claude socket" plist_lacks_socket
expect "  ...and launchd runs that plist" launchd_lacks_socket
route socket-on socket-on --restart-supervisor
sed 's/^/    | /' "$RES/socket-on.txt"
expect "claude-route socket-on --restart-supervisor exits 0" test "$(cat "$RES/socket-on.rc")" = 0
expect "  ...the plist names the Claude socket again" plist_has_socket
expect "  ...and the socket answers through the supervisor" curl -sf -m 3 --unix-socket "$CLAUDE_SOCK" http://api.anthropic.com/_subrouter/health -o /dev/null
expect "  ...and supervisor-status says it serves it" test "$(sv_status | "$PY" -c 'import json,sys; print(json.load(sys.stdin).get("claude_socket"))')" = serving

echo "== roll back, with streams open =="
stream rollback-tcp tcp 20; stream rollback-socket socket 20
sleep 3
probe_start rollback
run rollback "$MACOS/activate-laptop-supervisor.sh" --rollback
probe_stop rollback
await_streams rollback-tcp rollback-socket
sed 's/^/    | /' "$RES/rollback.txt"
expect "rollback exits 0" test "$(cat "$RES/rollback.rc")" = 0
expect "the plist is the original, plus the tailnet route the worker config held" "$PY" -c 'import plistlib, sys
new, orig = plistlib.load(open(sys.argv[1], "rb")), plistlib.load(open(sys.argv[2], "rb"))
orig["EnvironmentVariables"]["SUBROUTER_PEER_TAILNET"] = "rehearsal=192.0.2.10"
sys.exit(0 if new == orig else 1)' "$PLIST" "$RES/plist.direct"
expect "the worker binary is still build-b, the one deployed since" test "$(cdhash "$H/bin/subrouter")" = "$(cdhash "$B")"
expect "subrouter-supervisor and the worker config are gone" test ! -e "$H/bin/subrouter-supervisor" -a ! -e "$H/.subrouter/worker-config.json"
expect "the restored job answers on TCP" health_ok
stream_done rollback-tcp 20 && echo "    (the TCP stream open across the rollback finished)" || echo "    (the TCP stream open across the rollback did not finish)"
stream_done rollback-socket 20 && echo "    (the socket stream open across the rollback finished)" || echo "    (the socket stream open across the rollback did not finish)"
expect "clients never saw a 503 with Retry-After > 60 during the rollback" no_long_retry_after rollback

echo "== activate again, and SIGKILL the calling shell once the bootout has started =="
probe_start activation-killed
run_killed activate-killed '^   booting out ' "$MACOS/activate-laptop-supervisor.sh" "$A"
probe_stop activation-killed
[ -n "$DETACHED_LOG" ] && sed 's/^/    | /' "$DETACHED_LOG"
expect "the calling shell died of SIGKILL (exit $FG_RC)" test "$FG_RC" = 137
expect "the detached activation finished and exited 0" test "$DETACHED_RC" = 0
expect "  ...and printed ACTIVATED" grep -q '^ACTIVATED' "$DETACHED_LOG"
expect "launchd has the job loaded, under supervise" loaded_runs supervise
expect "supervisor-status runs build-a" test "$(sv_status | "$PY" -c 'import json,sys; print(json.load(sys.stdin)["active_worker"]["executable_identity"])')" = "$(cdhash "$A")"
expect "the job answers on TCP" health_ok
expect "clients never saw a 503 with Retry-After > 60 during the killed activation" no_long_retry_after activation-killed

echo "== deploy build-b, and SIGKILL the calling shell once the upgrade has started =="
SUP_PID=$(job_pid)
run_killed deploy-killed '^== 4. upgrade the worker' "$MACOS/deploy-laptop.sh" "$B"
[ -n "$DETACHED_LOG" ] && sed 's/^/    | /' "$DETACHED_LOG"
expect "the calling shell died of SIGKILL (exit $FG_RC)" test "$FG_RC" = 137
expect "the detached deploy finished and exited 0" test "$DETACHED_RC" = 0
expect "  ...and printed DEPLOY OK" grep -q '^DEPLOY OK' "$DETACHED_LOG"
expect "the deploy upgraded, never restarted (supervisor pid $SUP_PID unchanged)" test "$SUP_PID" = "$(job_pid)"
expect "supervisor-status runs build-b" test "$(sv_status | "$PY" -c 'import json,sys; print(json.load(sys.stdin)["active_worker"]["executable_identity"])')" = "$(cdhash "$B")"

echo "== roll back again, and SIGKILL the calling shell once the bootout has started =="
probe_start rollback-killed
run_killed rollback-killed '^   booting out ' "$MACOS/activate-laptop-supervisor.sh" --rollback
probe_stop rollback-killed
[ -n "$DETACHED_LOG" ] && sed 's/^/    | /' "$DETACHED_LOG"
expect "the calling shell died of SIGKILL (exit $FG_RC)" test "$FG_RC" = 137
expect "the detached rollback finished and exited 0" test "$DETACHED_RC" = 0
expect "  ...and printed ROLLED BACK" grep -q '^ROLLED BACK' "$DETACHED_LOG"
expect "launchd has the job loaded, as a plain serve" loaded_runs serve
expect "the plain job runs build-b, the worker it was rolled back from" test "$(cdhash "$H/bin/subrouter")" = "$(cdhash "$B")"
expect "the job answers on TCP" health_ok
expect "the job answers on the Claude socket" curl -sf -m 3 --unix-socket "$CLAUDE_SOCK" http://api.anthropic.com/_subrouter/health -o /dev/null
expect "clients never saw a 503 with Retry-After > 60 during the killed rollback" no_long_retry_after rollback-killed

echo
echo "rehearsal: $PASS passed, $FAILS failed (results in $RES)"
[ "$FAILS" = 0 ]
