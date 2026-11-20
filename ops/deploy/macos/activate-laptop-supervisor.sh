#!/usr/bin/env bash
# Move the laptop LaunchAgent from `subrouter serve` to `subrouter supervise`,
# once, and back.
#
#   activate-laptop-supervisor.sh <build> [--peer-tailnet <name>=<ip>[,...]] [--wait-quiet [seconds]] [--dry-run]
#   activate-laptop-supervisor.sh --rollback [--backup <dir>]
#
# Every run reports the open websocket turns from the daemon's drain socket;
# --wait-quiet waits (default 600 s) until there are none.
#
# --dry-run prints the supervised plist, the worker config and the diff
# against the live plist, and changes nothing. A job that already runs
# supervise is left alone. --rollback writes a plain-serve plist derived from
# the current supervised plist and its worker config, so later edits and
# SUBROUTER_PEER_TAILNET survive, and keeps the current worker binary. It
# prints how that plist differs from the one in the newest activation backup
# (or --backup <dir>), puts the backup's worker config back, and removes the
# supervisor binary when the activation created it. That is a supervisor
# restart: the port closes until the plain daemon is up, and the supervisor
# waits up to its --drain-timeout (90 s) for open connections to close.
#
# Paths (env): SUBROUTER_LABEL (ai.manaflow.subrouter), SUBROUTER_PLIST
# (~/Library/LaunchAgents/<label>.plist), SUBROUTER_SUPERVISOR_BIN
# (subrouter-supervisor beside the program the live job runs),
# SUBROUTER_ADDR (when the live serve args name no --addr: 127.0.0.1:31415),
# SUBROUTER_CONTROL_SOCKET (~/.subrouter/supervisor.sock),
# SUBROUTER_WORKER_CONFIG (~/.subrouter/worker-config.json),
# SUBROUTER_ACTIVATION_BACKUPS (~/.subrouter), SUBROUTER_READY_TIMEOUT (2m).
#
# Exit codes: 0 activated, rolled back on request, or nothing to do; 1 refused
# with nothing changed, or failed and rolled back; 2 rolled back and the daemon
# is still not healthy, a job loaded from the plist on disk does not answer,
# or a file could not be put back.
set -uo pipefail

SELF=$(python3 -c 'import os, sys; print(os.path.realpath(sys.argv[1]))' "$0")
# shellcheck source=laptop-deploy-lib.sh
. "$(dirname "$SELF")/laptop-deploy-lib.sh" || exit 1
ARGS=("$@")
DETACHED="${SUBROUTER_LAPTOP_DETACHED:-}"
unset SUBROUTER_LAPTOP_DETACHED

JOB="${SUBROUTER_LABEL:-ai.manaflow.subrouter}"
PLIST="${SUBROUTER_PLIST:-$HOME/Library/LaunchAgents/$JOB.plist}"
LABEL="gui/$(id -u)/$JOB"
CONTROL_SOCKET="${SUBROUTER_CONTROL_SOCKET:-$HOME/.subrouter/supervisor.sock}"
WORKER_CONFIG="${SUBROUTER_WORKER_CONFIG:-$HOME/.subrouter/worker-config.json}"
BACKUPS="${SUBROUTER_ACTIVATION_BACKUPS:-$HOME/.subrouter}"
READY="${SUBROUTER_READY_TIMEOUT:-2m}"
STOP_GRACE=2m
DRAIN_TIMEOUT=90s
FILES_LEFT=3
PLIST_LEFT=4
STAMP=$(date +%Y%m%d-%H%M%S)

usage() {
    echo "usage: $(basename "$0") <build> [--peer-tailnet <name>=<ip>[,...]] [--wait-quiet [seconds]] [--dry-run]" >&2
    echo "       $(basename "$0") --rollback [--backup <dir>]" >&2
    exit 1
}

BUILD=""; TAILNET=""; DRY=0; WAIT_QUIET=""; ROLLBACK=0; FROM=""
while [ $# -gt 0 ]; do
    case "$1" in
        --peer-tailnet) [ $# -ge 2 ] || usage; TAILNET="$2"; shift 2 ;;
        --peer-tailnet=*) TAILNET="${1#*=}"; shift ;;
        --dry-run) DRY=1; shift ;;
        --wait-quiet)
            WAIT_QUIET=600; shift
            case "${1:-}" in ''|*[!0-9]*) ;; *) WAIT_QUIET="$1"; shift ;; esac ;;
        --rollback) ROLLBACK=1; shift ;;
        --backup) [ $# -ge 2 ] || usage; FROM="$2"; shift 2 ;;
        -*) echo "unknown option: $1" >&2; usage ;;
        *) [ -z "$BUILD" ] || usage; BUILD="$1"; shift ;;
    esac
done
if [ "$ROLLBACK" = 1 ]; then [ -z "$BUILD" ] || usage; else [ -n "$BUILD" ] || usage; fi

if [ -n "$DETACHED" ]; then take_mutation_lock || exit 1; fi

fail() { echo "ACTIVATION FAILED: $*" >&2; }

render() {
    python3 - "$1" "$2" "$3" "$SUPERVISOR_BIN" "$WORKER_BIN" "$CONTROL_SOCKET" "$WORKER_CONFIG" \
        "$READY" "$STOP_GRACE" "$DRAIN_TIMEOUT" "$TAILNET" "${SUBROUTER_ADDR:-127.0.0.1:31415}" <<'PY'
import json, plistlib, sys
live, out_plist, out_config, sup, worker, control, config_path, ready, grace, drain, tailnet, addr = sys.argv[1:13]
with open(live, "rb") as f:
    p = plistlib.load(f)
serve = [str(a) for a in p["ProgramArguments"][2:]]
worker_args, i = [], 0
while i < len(serve):
    a = serve[i]
    if a == "--addr" and i + 1 < len(serve):
        addr, i = serve[i + 1], i + 2
        continue
    if a.startswith("--addr="):
        addr, i = a[len("--addr="):], i + 1
        continue
    for owned in ("--unix-socket", "--local-data-socket"):
        if a == owned or a.startswith(owned + "="):
            sys.stderr.write("the live serve args set %s, which the supervisor owns and refuses in a worker; remove it from %s first\n" % (owned, live))
            sys.exit(4)
    worker_args.append(a)
    i += 1
p.pop("Program", None)
p["ProgramArguments"] = [sup, "supervise", "--addr", addr, "--control-socket", control,
                         "--worker-bin", worker, "--worker-stop-grace", grace, "--ready-timeout", ready,
                         "--drain-timeout", drain, "--worker-config", config_path, "--"] + worker_args
p["ExitTimeOut"] = 120
p["ThrottleInterval"] = 10
p["KeepAlive"] = True
p["RunAtLoad"] = True
with open(out_plist, "wb") as f:
    plistlib.dump(p, f)
with open(out_config, "w") as f:
    json.dump({"args": worker_args, "env": {"SUBROUTER_PEER_TAILNET": tailnet} if tailnet else {}}, f, indent=2)
    f.write("\n")
PY
}

as_xml() { python3 -c 'import plistlib, sys; sys.stdout.write(plistlib.dumps(plistlib.load(open(sys.argv[1], "rb"))).decode())' "$1"; }

put_file() {
    if cp "$1" "$2.activate.$$" && mv -f "$2.activate.$$" "$2"; then return 0; fi
    rm -f "$2.activate.$$"
    return 1
}

health() {
    if [ $# -gt 0 ]; then
        curl -s -m 5 --unix-socket "$1" "http://api.anthropic.com/_subrouter/health" 2>/dev/null
    else
        curl -s -m 5 "http://$ADDR/_subrouter/health" 2>/dev/null
    fi
}
json_field() { python3 -c 'import json, sys
try:
    v = json.load(sys.stdin)
    for k in sys.argv[1].split("."):
        v = v.get(k) if isinstance(v, dict) else None
    print("" if v is None else str(v).lower() if isinstance(v, bool) else v)
except Exception:
    pass' "$1"; }
supervisor_status() { curl -s -m 5 --unix-socket "$CONTROL_SOCKET" "http://supervisor/_subrouter/supervisor-status" 2>/dev/null; }
drain_status() { curl -s -m 5 --unix-socket "$DRAIN_SOCKET" "http://drain-socket/_subrouter/drain-status" 2>/dev/null; }

# shellcheck disable=SC2317 # run through await
tcp_ok() { [ "$(health | json_field ok)" = true ]; }
# shellcheck disable=SC2317 # run through await
socket_ok() { [ "$(health "$CLAUDE_SOCKET" | json_field ok)" = true ]; }
# shellcheck disable=SC2317 # run through await
supervisor_up() { [ "$(supervisor_status | json_field accepting)" = true ]; }

go_seconds() {
    python3 -c 'import re, sys
d = sys.argv[1]
parts = re.findall(r"([0-9]+(?:\.[0-9]+)?)(ms|h|m|s)", d)
ok = parts and "".join(n + u for n, u in parts) == d
print(int(sum(float(n) * {"ms": 0.001, "s": 1, "m": 60, "h": 3600}[u] for n, u in parts)) if ok else 120)' "$1"
}

read_job() {
    job_state
    ADDR="$JOB_ADDR"
}

verify_supervised() {
    local bad=0 sock word
    if ! await $(($(go_seconds "$READY") + 60)) "supervisor-status on $CONTROL_SOCKET" supervisor_up; then
        echo "   supervisor-status: no answer on $CONTROL_SOCKET"
        return 1
    fi
    if [ -n "$CLAUDE_SOCKET" ]; then
        sock=$(supervisor_status | json_field claude_socket)
        word=$(health "$CLAUDE_SOCKET" | json_field claude_socket)
        echo "   claude socket owner        -> supervisor: ${sock:-unknown}; through $CLAUDE_SOCKET the worker says: ${word:-no answer}   (want serving; owned_by_supervisor)"
        { [ "$sock" = serving ] && [ "$word" = owned_by_supervisor ]; } || bad=1
    fi
    SUPERVISED=1; CUR="$WORKER_BIN"
    if [ "${1:-}" = --routes ]; then
        verify_routes "$BASE_PROVIDER" "$BASE_IDENTITY" "$BASE_ACCOUNTS" "$BASE_SOCKET" || bad=1
    else
        serving "$CUR" || bad=1
        if healthy; then echo "   health: ok"; else echo "   health: DOWN"; bad=1; fi
    fi
    return $bad
}

verify_direct() {
    if ! await 120 "the daemon to answer on $ADDR" tcp_ok; then echo "   tcp health      -> DOWN ($ADDR)"; return 1; fi
    echo "   tcp health      -> ok ($ADDR)"
    [ -n "$CLAUDE_SOCKET" ] || return 0
    if await 60 "the Claude socket to answer on $CLAUDE_SOCKET" socket_ok; then
        echo "   claude socket   -> serving"
        return 0
    fi
    echo "   claude socket   -> no answer on $CLAUDE_SOCKET"
    return 1
}

load_unloaded_job() {
    echo "== launchd has not loaded $LABEL: bootstrapping $PLIST =="
    bootstrap_job || return 1
    read_job
    if [ "$LOADED_SHAPE" = supervised ]; then
        WORKER_BIN="$JOB_WORKER_BIN"
        CONTROL_SOCKET="${JOB_CONTROL_SOCKET:-$CONTROL_SOCKET}"
        verify_supervised
    else
        verify_direct
    fi
}

read_manifest() {
    local key value
    M_PLIST=""; M_WORKER=""; M_SUPERVISOR=""; M_CONFIG=""
    while IFS='=' read -r key value; do
        case "$key" in
            plist) M_PLIST="$value" ;;
            worker_bin) M_WORKER="$value" ;;
            supervisor_bin) M_SUPERVISOR="$value" ;;
            worker_config) M_CONFIG="$value" ;;
        esac
    done < "$1/manifest"
    [ "$M_PLIST" = "$PLIST" ] && [ -f "$1/plist" ] && [ -n "$M_WORKER" ] && [ -n "$M_SUPERVISOR" ] && [ -n "$M_CONFIG" ]
}

restore_plist() {
    put_file "$1/plist" "$PLIST" && return 0
    echo "   could not restore $PLIST from $1/plist" >&2
    return 1
}
restore_files() {
    local bad=0
    if [ -f "$1/worker" ]; then
        install_binary "$1/worker" "$M_WORKER" || { echo "   could not restore $M_WORKER from $1/worker" >&2; bad=1; }
    fi
    if [ -f "$1/worker-config.json" ]; then
        put_file "$1/worker-config.json" "$M_CONFIG" || { echo "   could not restore $M_CONFIG" >&2; bad=1; }
    else
        rm -f "$M_CONFIG" || { echo "   could not remove $M_CONFIG" >&2; bad=1; }
    fi
    return $bad
}
restore_idle_supervisor_bin() {
    if [ -f "$1/supervisor" ]; then
        install_binary "$1/supervisor" "$M_SUPERVISOR" && return 0
    else
        rm -f "$M_SUPERVISOR" && return 0
    fi
    echo "   could not restore $M_SUPERVISOR" >&2
    return 1
}

restore() {
    read_manifest "$1" || { echo "   $1 is not an activation backup of $PLIST; nothing was changed" >&2; return 1; }
    echo "   restoring $M_PLIST, $M_WORKER and $M_CONFIG from $1"
    if [ -f "$1/worker" ] && [ "$(cdhash "$1/worker")" != "$(cdhash "$M_WORKER")" ]; then
        echo "   $M_WORKER goes back to the build that ran before the activation; deploy a newer one with deploy-laptop.sh afterwards if you need it"
    fi
    echo "   restarting: the port closes until the restored daemon is up; the supervisor drains open connections for up to $DRAIN_TIMEOUT first"
    bootout_and_wait "$EXIT_TIMEOUT" || return 2
    local left=0
    if restore_plist "$1"; then
        restore_files "$1" || left=$FILES_LEFT
        restore_idle_supervisor_bin "$1" || left=$FILES_LEFT
    else
        echo "   the supervised plist stays, and so do the files it names" >&2
        left=$PLIST_LEFT
    fi
    bootstrap_job || return 2
    read_job
    echo "== verify the job =="
    verify_direct || return 2
    return $left
}

derive_serve() {
    python3 - "$PLIST" "$1" "$2" <<'PY'
import json, plistlib, sys
cur_path, backup_path, out_path = sys.argv[1:4]
with open(cur_path, "rb") as f:
    cur = plistlib.load(f)
with open(backup_path, "rb") as f:
    backup_bytes = f.read()
backup = plistlib.loads(backup_bytes)
args = [str(a) for a in cur.get("ProgramArguments") or []]
if "--" not in args:
    sys.exit("%s names no worker args after --" % cur_path)
own, worker_args = args[: args.index("--")], args[args.index("--") + 1:]

def flag(name):
    for i, a in enumerate(own):
        if a == name and i + 1 < len(own):
            return own[i + 1]
        if a.startswith(name + "="):
            return a[len(name) + 1:]
    return ""

worker, addr, config_path = flag("--worker-bin"), flag("--addr"), flag("--worker-config")
if not worker or not addr:
    sys.exit("%s names no --worker-bin or --addr" % cur_path)
env = dict(cur.get("EnvironmentVariables") or {})
from_config = set()
if config_path:
    try:
        with open(config_path) as f:
            config = json.load(f)
        worker_args = [str(a) for a in config["args"]]
        extra = {str(k): str(v) for k, v in (config.get("env") or {}).items()}
        env.update(extra)
        from_config = set(extra)
    except FileNotFoundError:
        pass
    except Exception as e:
        print("   %s is unusable (%s); the supervisor ran its workers on the plist's args, and so does this" % (config_path, e))
if worker_args[:1] == ["serve"]:
    worker_args = worker_args[1:]
out = dict(cur)
out["ProgramArguments"] = [worker, "serve", "--addr", addr] + worker_args
out["EnvironmentVariables"] = env
for key in ("ExitTimeOut", "ThrottleInterval", "KeepAlive", "RunAtLoad"):
    if key in backup:
        out[key] = backup[key]
    else:
        out.pop(key, None)
if out == backup:
    print("   it matches the plist the activation backed up")
    with open(out_path, "wb") as f:
        f.write(backup_bytes)
    sys.exit(0)
print("   it differs from the plist the activation backed up (%s), and keeps:" % backup_path)
for key in sorted(set(out) | set(backup)):
    before, after = backup.get(key), out.get(key)
    if before == after:
        continue
    if key == "EnvironmentVariables":
        before, after = before or {}, after or {}
        for name in sorted(set(before) | set(after)):
            where = "the worker config" if name in from_config else "the plist"
            if name not in before:
                print("     + EnvironmentVariables.%s, set in %s since the activation" % (name, where))
            elif name not in after:
                print("     - EnvironmentVariables.%s, removed from the plist since the activation" % name)
            elif before[name] != after[name]:
                print("     ~ EnvironmentVariables.%s, changed in %s since the activation" % (name, where))
    else:
        print("     ~ %s: %r -> %r" % (key, before, after))
with open(out_path, "wb") as f:
    plistlib.dump(out, f)
PY
}

roll_back_to_serve() {
    read_manifest "$1" || { echo "   $1 is not an activation backup of $PLIST; nothing was changed" >&2; return 1; }
    WORK=$(mktemp -d "${TMPDIR:-/tmp}/activate-supervisor.XXXXXX") || return 1
    trap 'rm -rf "$WORK"; release_mutation_lock' EXIT
    echo "== the plain-serve plist, from $PLIST and its worker config =="
    derive_serve "$1/plist" "$WORK/plist" || { echo "   cannot derive it; nothing was changed" >&2; return 1; }
    local worker
    worker=$(python3 -c 'import plistlib, sys; print(plistlib.load(open(sys.argv[1], "rb"))["ProgramArguments"][0])' "$WORK/plist")
    echo "   it runs the current worker, $worker ($(cdhash "$worker"))"
    echo "== restart on it =="
    echo "   the port closes until the plain daemon is up; the supervisor drains open connections for up to $DRAIN_TIMEOUT first"
    bootout_and_wait "$EXIT_TIMEOUT" || return 2
    local left=0
    if put_file "$WORK/plist" "$PLIST"; then
        if [ -f "$1/worker-config.json" ]; then
            put_file "$1/worker-config.json" "$M_CONFIG" || { echo "   could not restore $M_CONFIG" >&2; left=$FILES_LEFT; }
        else
            rm -f "$M_CONFIG" || { echo "   could not remove $M_CONFIG" >&2; left=$FILES_LEFT; }
        fi
        if [ -f "$1/supervisor" ]; then
            echo "   $M_SUPERVISOR stays as it is: nothing runs it, and the one from before the activation is not put back"
        else
            rm -f "$M_SUPERVISOR" || { echo "   could not remove $M_SUPERVISOR" >&2; left=$FILES_LEFT; }
        fi
    else
        echo "   could not write $PLIST, so the supervised plist stays, and so do the files it names" >&2
        left=$PLIST_LEFT
    fi
    bootstrap_job || return 2
    read_job
    echo "== verify the job =="
    verify_direct || return 2
    return $left
}

newest_backup() {
    local dir newest=""
    for dir in "$BACKUPS"/supervisor-activation-*; do
        [ -d "$dir" ] && [ -f "$dir/manifest" ] && newest="$dir"
    done
    echo "$newest"
}

detach() {
    if LAUNCHD_WHY=$(launchd_blocker restart); then
        echo "refusing: $LAUNCHD_WHY. Nothing was changed. $LAUNCHD_FIX" >&2
        exit 1
    fi
    run_detached "$SELF" ${ARGS[@]+"${ARGS[@]}"}
    exit $?
}

load_hint() {
    launchctl print "$LABEL" >/dev/null 2>&1
    case $? in
        0) return 0 ;;
        113) echo "   launchd has not loaded $LABEL. Load it from $LAUNCHD_FROM:" >&2 ;;
        *) echo "   launchd did not say whether $LABEL is loaded. From $LAUNCHD_FROM, run launchctl print $LABEL, and if it is not loaded:" >&2 ;;
    esac
    echo "     launchctl bootstrap gui/$(id -u) $PLIST" >&2
}

rolled_back() {
    case $1 in
        0) echo; echo "ROLLED BACK  backup=$2"; exit 0 ;;
        1) exit 1 ;;
        "$FILES_LEFT") echo; echo "ROLLED BACK, healthy, but the files named above were not put back  backup=$2" >&2; exit 2 ;;
        "$PLIST_LEFT") echo; echo "NOT ROLLED BACK: launchd runs the supervised plist again, healthy  backup=$2" >&2; exit 2 ;;
        *) echo; echo "ROLLED BACK, NOT HEALTHY: tail -50 the plist's StandardErrorPath; launchctl print $LABEL" >&2; load_hint; exit 2 ;;
    esac
}

read_job
if [ "$LOADED_SHAPE" = unknown ]; then
    LAUNCHD_WHY=$(launchd_blocker restart)
    [ "$DRY" = 1 ] || { echo "refusing: $LAUNCHD_WHY. Nothing was changed. $LAUNCHD_FIX" >&2; exit 1; }
    echo "   NOTE: $LAUNCHD_WHY; this dry run reads only the plist"
    LOADED_SHAPE=none
fi
if [ "$ROLLBACK" = 1 ]; then
    case "$PLIST_SHAPE/$LOADED_SHAPE" in
        direct/direct)
            echo "$LABEL runs serve and $PLIST agrees; nothing to roll back"
            exit 0 ;;
        direct/none)
            [ -n "$DETACHED" ] || detach
            echo "$PLIST runs serve; nothing to roll back but load it"
            if load_unloaded_job; then echo; echo "LOADED  $LABEL runs serve"; exit 0; fi
            echo; echo "LOADED, NOT HEALTHY: tail -50 the plist's StandardErrorPath; launchctl print $LABEL" >&2
            load_hint; exit 2 ;;
        direct/supervised)
            [ -n "$DETACHED" ] || detach
            echo "== $PLIST runs serve but launchd still runs supervise: finishing that rollback =="
            { bootout_and_wait "$EXIT_TIMEOUT" && bootstrap_job; } || rolled_back 2
            read_job
            verify_direct || rolled_back 2
            echo; echo "ROLLED BACK  $LABEL runs serve"; exit 0 ;;
        supervised/*) ;;
        missing/*) echo "no plist at $PLIST" >&2; exit 1 ;;
        *) echo "refusing: $PLIST does not run '<binary> serve ...' or supervise; nothing was changed" >&2; exit 1 ;;
    esac
    DIR="${FROM:-$(newest_backup)}"
    { [ -n "$DIR" ] && [ -f "$DIR/manifest" ]; } || { echo "no activation backup found in $BACKUPS (or --backup <dir>)" >&2; exit 1; }
    [ -n "$DETACHED" ] || detach
    echo "== roll back the supervisor activation =="
    roll_back_to_serve "$DIR"
    rolled_back $? "$DIR"
fi

if CONFLICT=$(shape_conflict); then
    echo "refusing: $CONFLICT. Nothing was changed." >&2
    exit 1
fi
case "$PLIST_SHAPE/$LOADED_SHAPE" in
    supervised/supervised) echo "$LABEL already runs supervise; nothing to do"; exit 0 ;;
    supervised/none)
        [ -n "$DETACHED" ] || detach
        echo "$PLIST already runs supervise; nothing to do but load it"
        if load_unloaded_job; then echo; echo "LOADED  $LABEL runs supervise"; exit 0; fi
        echo; echo "LOADED, NOT HEALTHY: tail -50 the plist's StandardErrorPath; launchctl print $LABEL. To go back to serve: $(basename "$0") --rollback" >&2
        load_hint; exit 2 ;;
    direct/direct|direct/none) ;;
    missing/*) echo "no plist at $PLIST" >&2; exit 1 ;;
    *) echo "refusing: $PLIST does not run '<binary> serve ...'; nothing was changed" >&2; exit 1 ;;
esac
WORKER_BIN="$JOB_PROGRAM"
SUPERVISOR_BIN="${SUBROUTER_SUPERVISOR_BIN:-$(dirname "$WORKER_BIN")/subrouter-supervisor}"

{ [ -f "$BUILD" ] && [ -x "$BUILD" ]; } || { echo "no executable build at $BUILD" >&2; exit 1; }
if ! knows_supervised_socket "$BUILD"; then
    echo "refusing: $BUILD has no supervisor-owned Claude socket (it lacks owned_by_supervisor), which the supervised job needs. Build it from this repo; a build of upstream subrouter cannot run this job. Nothing was changed" >&2
    exit 1
fi
if [ -f "$WORKER_BIN" ] && ! is_post_isolation "$WORKER_BIN"; then
    echo "refusing: $WORKER_BIN predates Codex credential isolation, and a failed activation would restore it. Deploy a current build with deploy-laptop.sh first; nothing was changed" >&2
    exit 1
fi

WORK=$(mktemp -d "${TMPDIR:-/tmp}/activate-supervisor.XXXXXX") || exit 1
trap 'rm -rf "$WORK"; release_mutation_lock' EXIT
render "$PLIST" "$WORK/plist" "$WORK/worker-config.json" || exit 1

live_job() {
    echo "== the live job =="
    echo "   $PLIST runs $WORKER_BIN serve; label $LABEL"
    if [ "$LOADED_SHAPE" = none ]; then
        echo "   launchd has not loaded it"
        return
    fi
    echo "   launchd kills it ${LAUNCHD_EXIT:-an unknown number of} s after the bootout; what is still streaming then is cut"
    DRAIN=$(drain_status)
    if [ -n "$DRAIN" ]; then
        echo "   drain-status: active_websocket_turns=$(printf '%s' "$DRAIN" | json_field active_websocket_turns)" \
            "active_proxy_requests=$(printf '%s' "$DRAIN" | json_field active_proxy_requests) (HTTP streams and every open websocket)"
    else
        echo "   drain-status: no answer on $DRAIN_SOCKET"
    fi
}

if [ "$DRY" = 1 ]; then
    live_job
    echo "== supervised plist (for $PLIST) =="
    as_xml "$WORK/plist"
    echo "== worker config (for $WORKER_CONFIG) =="
    cat "$WORK/worker-config.json"
    echo "== diff: live -> supervised =="
    as_xml "$PLIST" > "$WORK/live.xml"
    as_xml "$WORK/plist" > "$WORK/new.xml"
    diff -u "$WORK/live.xml" "$WORK/new.xml" | sed '1,2d'
    echo
    echo "DRY RUN: nothing was changed"
    exit 0
fi

turns_blocker() { # prints why and returns 0 when the turns do not close within --wait-quiet
    local turns waited=0
    turns=$(drain_status | json_field active_websocket_turns)
    if [ -z "$turns" ]; then echo "--wait-quiet cannot see the turns ($DRAIN_SOCKET does not answer)"; return 0; fi
    until [ "$turns" = 0 ]; do
        if [ "$waited" -ge "$WAIT_QUIET" ]; then echo "websocket turns still open after ${WAIT_QUIET}s"; return 0; fi
        echo "   ${turns:-unknown} websocket turn(s) still open; waited ${waited}s of ${WAIT_QUIET}s" >&2
        sleep 5; waited=$((waited + 5))
        turns=$(drain_status | json_field active_websocket_turns)
    done
    echo "   no websocket turn in flight (waited ${waited}s)" >&2
    return 1
}

if [ -z "$DETACHED" ]; then
    if [ -n "$WAIT_QUIET" ] && [ "$LOADED_SHAPE" != none ]; then
        if TURNS_WHY=$(turns_blocker); then echo "refusing: $TURNS_WHY; nothing was changed" >&2; exit 1; fi
    fi
    detach
fi

if [ "$LOADED_SHAPE" = none ]; then
    load_unloaded_job || { echo; echo "NOT ACTIVATED: the job on $PLIST does not answer; nothing else was changed" >&2; load_hint; exit 2; }
fi
live_job
echo "== 0. baseline the routes on the live job =="
TOKEN="$(local_token)"
take_baseline

echo "== 1. back up =="
DIR="$BACKUPS/supervisor-activation-$STAMP"
{ mkdir -p "$BACKUPS" && mkdir -m 700 "$DIR"; } || { fail "cannot create $DIR"; exit 1; }
cp -p "$PLIST" "$DIR/plist" || { fail "cannot back up $PLIST"; exit 1; }
if [ -f "$WORKER_BIN" ]; then cp -p "$WORKER_BIN" "$DIR/worker" || { fail "cannot back up $WORKER_BIN"; exit 1; }; fi
if [ -f "$SUPERVISOR_BIN" ]; then cp -p "$SUPERVISOR_BIN" "$DIR/supervisor" || { fail "cannot back up $SUPERVISOR_BIN"; exit 1; }; fi
if [ -f "$WORKER_CONFIG" ]; then cp -p "$WORKER_CONFIG" "$DIR/worker-config.json" || { fail "cannot back up $WORKER_CONFIG"; exit 1; }; fi
{ printf 'plist=%s\nworker_bin=%s\nsupervisor_bin=%s\nworker_config=%s\n' "$PLIST" "$WORKER_BIN" "$SUPERVISOR_BIN" "$WORKER_CONFIG" > "$DIR/manifest" \
    && read_manifest "$DIR"; } || { fail "cannot write the backup manifest $DIR/manifest; nothing else was changed"; exit 1; }
echo "   backup: $DIR"

undo_files() {
    local bad=0
    fail "$1"
    restore_plist "$DIR" || bad=1
    restore_files "$DIR" || bad=1
    restore_idle_supervisor_bin "$DIR" || bad=1
    if [ "$bad" = 0 ]; then
        echo "   restored the files from $DIR; the live job was not restarted"
        exit 1
    fi
    echo "!! the live job was not restarted and still serves, but the files above are not what it was started from: put them back from $DIR before anything restarts it" >&2
    exit 2
}
roll_back() {
    fail "$1"
    echo "!! ROLLING BACK from $DIR"
    restore "$DIR"
    case $? in
        0) echo; echo "ROLLED BACK, healthy on the previous job  backup=$DIR"; exit 1 ;;
        "$FILES_LEFT") echo; echo "ROLLED BACK, healthy, but the files named above were not put back  backup=$DIR" >&2; exit 2 ;;
        "$PLIST_LEFT") echo; echo "NOT ROLLED BACK: launchd runs the supervised plist again, healthy  backup=$DIR" >&2; exit 2 ;;
        *) echo; echo "ROLLED BACK, NOT HEALTHY: tail -50 the plist's StandardErrorPath; launchctl print $LABEL" >&2; load_hint; exit 2 ;;
    esac
}

echo "== 2. install the build as the worker and the supervisor =="
install_binary "$BUILD" "$WORKER_BIN" || undo_files "could not install $WORKER_BIN"
install_binary "$BUILD" "$SUPERVISOR_BIN" || undo_files "could not install $SUPERVISOR_BIN"
echo "   $WORKER_BIN, $SUPERVISOR_BIN: $(cdhash "$WORKER_BIN")"

echo "== 3. write the worker config and the supervised plist =="
{ mkdir -p "$(dirname "$WORKER_CONFIG")" && put_file "$WORK/worker-config.json" "$WORKER_CONFIG" && chmod 600 "$WORKER_CONFIG"; } \
    || undo_files "could not write $WORKER_CONFIG"
put_file "$WORK/plist" "$PLIST" || undo_files "could not write $PLIST"
echo "   $WORKER_CONFIG"
echo "   $PLIST"

echo "== 4. restart under the supervisor =="
if [ -n "$WAIT_QUIET" ]; then
    echo "   the baseline took a while: --wait-quiet checks the turns again"
    if TURNS_WHY=$(turns_blocker); then undo_files "$TURNS_WHY"; fi
fi
bootout_and_wait "$LAUNCHD_EXIT" || roll_back "the live job did not exit"
bootstrap_job || roll_back "launchd did not load the supervised plist"

echo "== 5. verify =="
read_job
verify_supervised --routes || roll_back "the supervised job did not verify"

echo
echo "ACTIVATED  backup=$DIR"
echo "   deploy:   deploy-laptop.sh <build>   (hot-swaps the worker)"
echo "   undo:     $(basename "$0") --rollback"
exit 0
