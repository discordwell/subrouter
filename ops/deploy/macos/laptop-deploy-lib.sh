# shellcheck shell=bash
# shellcheck disable=SC2034 # LAUNCHD_FIX, SOCKET_INCOMPLETE and the job facts are read by the scripts that source this

LAPTOP_LIB_DIR=$(dirname "${BASH_SOURCE[0]}")

code() { curl -s -m 10 -o /dev/null -w '%{http_code}' "$@"; }
healthy() { curl -fsS -m 3 "http://$ADDR/_subrouter/health" >/dev/null 2>&1; }

# --- launchd reachability (PATCHES §47) ----------------------------------------
# Every restart below goes through launchctl, and a shell can be unable to use
# it. A terminal host that outlived a logout (Superset's pty-daemon, which a
# relaunch of Superset itself does not replace, or a tmux server) leaves every
# shell under it in the ENDED login session's bootstrap namespace. There every
# launchctl verb is refused: print and list rc 141 "Reentrancy avoided", for a
# label that does not exist as much as for one that does; managername rc 153.
# This script read the refused print as "unloaded", ignored the refused
# kickstart/bootout/bootstrap, found the OLD process still answering health,
# verified it against its own baseline, and printed DEPLOY OK for a binary that
# never started. A shell outside the GUI session (ssh: managername is not Aqua)
# is worse: it can boot the gui/ job OUT and then fail to bootstrap it back,
# leaving every local Claude/Codex session without its proxy and KeepAlive
# nothing to restart. Prints why and returns 0 when this shell must not restart
# the job (with "restart"), or cannot even tell what launchd runs (without it:
# a supervised deploy needs only that).
launchd_blocker() { # [restart]
    local mgr rc
    mgr=$(launchctl managername 2>&1); rc=$?
    if [ "$rc" -ne 0 ]; then
        echo "this shell cannot reach launchd (launchctl managername: rc $rc, ${mgr:-no output}): it belongs to a login session that has ended"
        return 0
    fi
    if [ "${1:-}" = restart ] && [ "$mgr" != Aqua ]; then
        echo "this shell is in launchd's '${mgr:-unnamed}' session, not the logged-in GUI (Aqua) session that owns gui/$(id -u), so a bootout could succeed and the bootstrap fail"
        return 0
    fi
    launchctl print "$LABEL" >/dev/null 2>&1; rc=$?
    case "$rc" in
        0|113) return 1 ;;
        *) echo "launchctl print $LABEL answered rc $rc (only 0, loaded, and 113, not loaded, are answers), so a restart could not tell a stopped job from a refused query"; return 0 ;;
    esac
}
LAUNCHD_FROM="a new Terminal.app window (or another terminal app that is not Superset; \`launchctl managername\` must print Aqua)"
LAUNCHD_SUPERSET="A new Superset tab is no fix: every Superset tab stays in the ended session, even after Superset is relaunched, until a person uses Settings > Terminal > Terminal daemon > Restart daemon (which closes every Superset terminal)."
LAUNCHD_FIX="Run it from $LAUNCHD_FROM. $LAUNCHD_SUPERSET"

# Every build past the credential-isolation boundary carries the attest-legacy
# verb; none before it does (no build from before the merge has that string).
is_post_isolation() { LC_ALL=C grep -aq attest-legacy "$1" 2>/dev/null; }

knows_supervised_socket() { LC_ALL=C grep -aq owned_by_supervisor "$1" 2>/dev/null; }

# The code-directory hash the kernel reports for a process running <binary>;
# supervisor-status gives it as active_worker.executable_identity.
cdhash() { codesign -dvvv "$1" 2>&1 | sed -n 's/^CDHash=//p' | head -1; }

local_token() {
    python3 - <<'PY' 2>/dev/null
import json, os
try:
    print((json.load(open(os.path.expanduser("~/.claude/settings.json"))).get("env") or {}).get("ANTHROPIC_AUTH_TOKEN", ""))
except Exception:
    print("")
PY
}

take_mutation_lock() {
    local lock="$PLIST.supervisor-mutation.lock" rc=0
    # shellcheck source=../../../deploy/macos/mutation-lease-lib.sh
    . "$LAPTOP_LIB_DIR/../../../deploy/macos/mutation-lease-lib.sh" \
        || { echo "refusing: cannot load deploy/macos/mutation-lease-lib.sh from this checkout. Nothing was changed." >&2; return 1; }
    acquire_subrouter_mutation_lease "$lock" || rc=$?
    if [ "$rc" = 0 ]; then
        trap release_mutation_lock EXIT
        return 0
    fi
    if [ "$rc" = 75 ]; then
        echo "refusing: another deploy, rollback or update holds $lock. Nothing was changed; run it again when that one finishes." >&2
    else
        echo "refusing: cannot take $lock (exit $rc). Nothing was changed." >&2
    fi
    return 1
}

release_mutation_lock() {
    [ -n "${SUBROUTER_MUTATION_LEASE_PID:-}" ] || return 0
    # -9: a detached run ignores TERM, and so does the lease helper it starts.
    kill -9 "$SUBROUTER_MUTATION_LEASE_PID" 2>/dev/null
    release_subrouter_mutation_lease
}

run_detached() { # script args...
    local dir="$HOME/.subrouter/transitions" base
    release_mutation_lock
    mkdir -p "$dir" || { echo "cannot create $dir" >&2; return 2; }
    base="$dir/$(basename "$1" .sh)-$(date +%Y%m%d-%H%M%S)-$$"
    echo "== detached: from here on, stopping this shell does not stop the run =="
    echo "   log: $base.log   exit code: $base.rc"
    SUBROUTER_LAPTOP_DETACHED=1 python3 - "$base" "$BASH" "$@" <<'PY'
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
    with open(base + ".pid", "w") as f:
        f.write("%d\n" % os.getpid())
    os.write(write_end, b"%d" % os.getpid())
    os.close(write_end)
    with open(os.devnull, "rb") as null:
        rc = subprocess.call(argv, stdin=null, stdout=log, stderr=log)
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
}

job_state() {
    local printed print_rc key value
    PLIST_SHAPE=missing LOADED_SHAPE=none JOB_PROGRAM="" JOB_BINARY="" JOB_ADDR="" JOB_WORKER_BIN="" JOB_CONTROL_SOCKET=""
    JOB_READY_SECONDS="" JOB_WORKER_CONFIG="" EXIT_TIMEOUT="" CLAUDE_SOCKET="" DRAIN_SOCKET="" LAUNCHD_EXIT=""
    printed=$(launchctl print "$LABEL" 2>/dev/null); print_rc=$?
    while IFS='=' read -r key value; do
        case "$key" in
            PLIST_SHAPE|LOADED_SHAPE|JOB_PROGRAM|JOB_BINARY|JOB_ADDR|JOB_WORKER_BIN|JOB_CONTROL_SOCKET|JOB_READY_SECONDS|JOB_WORKER_CONFIG|EXIT_TIMEOUT|CLAUDE_SOCKET|DRAIN_SOCKET|LAUNCHD_EXIT)
                printf -v "$key" '%s' "$value" ;;
        esac
    done <<EOF
$(python3 - "$PLIST" "$print_rc" "${SUBROUTER_ADDR:-127.0.0.1:31415}" "$printed" <<'PY' 2>/dev/null
import os, plistlib, re, sys
path, print_rc, default_addr, printed = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4]
try:
    with open(path, "rb") as f:
        plist = plistlib.load(f)
except Exception:
    plist = None

def shape(args):
    own = args[: args.index("--")] if "--" in args else args
    if "supervise" in own:
        return "supervised"
    return "direct" if len(args) > 1 and args[1] == "serve" else "other"

def printed_args():
    block = re.search(r"^\targuments = \{\n((?:\t\t[^\n]*\n)*)\t\}", printed, re.M)
    if block:
        return [line[2:] for line in block.group(1).splitlines()]
    program = re.search(r"^\tprogram = (.*)$", printed, re.M)
    return [program.group(1)] if program else []

loaded = print_rc == 0
plist_args = [str(a) for a in (plist or {}).get("ProgramArguments") or []]
loaded_args = printed_args() if loaded else []
args = loaded_args if loaded else plist_args
own = args[: args.index("--")] if "--" in args else args

def flag(name, default=""):
    for i, a in enumerate(own):
        if a == name and i + 1 < len(own):
            return own[i + 1]
        if a.startswith(name + "="):
            return a[len(name) + 1:]
    return default

def seconds(d):
    parts = re.findall(r"([0-9]+(?:\.[0-9]+)?)(ms|h|m|s)", d)
    if not parts or "".join(n + u for n, u in parts) != d:
        return ""
    return str(max(1, int(sum(float(n) * {"ms": 0.001, "s": 1, "m": 60, "h": 3600}[u] for n, u in parts))))

env = (plist or {}).get("EnvironmentVariables") or {}
drain = str(env.get("SUBROUTER_DRAIN_SOCKET") or "").strip()
if not drain:
    state = env.get("SUBROUTER_STATE_DIR") or os.path.join(env.get("HOME") or os.path.expanduser("~"), ".subrouter")
    drain = os.path.join(state, "drain.sock")
exit_timeout = re.search(r"^\texit timeout = ([0-9]+)", printed, re.M)
facts = {
    "PLIST_SHAPE": "missing" if plist is None else shape(plist_args),
    "LOADED_SHAPE": shape(loaded_args) if loaded else "none" if print_rc == 113 else "unknown",
    "JOB_PROGRAM": args[0] if args else "",
    "JOB_BINARY": {"supervised": flag("--worker-bin"), "direct": args[0] if args else ""}.get(shape(args), ""),
    "JOB_ADDR": flag("--addr", default_addr),
    "JOB_WORKER_BIN": flag("--worker-bin"),
    "JOB_CONTROL_SOCKET": flag("--control-socket"),
    "JOB_READY_SECONDS": seconds(flag("--ready-timeout")),
    "JOB_WORKER_CONFIG": flag("--worker-config"),
    "EXIT_TIMEOUT": str((plist or {}).get("ExitTimeOut", "")),
    "CLAUDE_SOCKET": str(env.get("SUBROUTER_CLAUDE_SOCKET") or "").strip(),
    "DRAIN_SOCKET": drain,
    "LAUNCHD_EXIT": exit_timeout.group(1) if exit_timeout and loaded else "",
}
for key, value in facts.items():
    print("%s=%s" % (key, value.replace("\n", " ")))
PY
)
EOF
}

shape_conflict() {
    case "$LOADED_SHAPE/$PLIST_SHAPE" in
        supervised/supervised|none/*|unknown/*|*/missing) return 1 ;;
        supervised/*)
            echo "launchd runs $LABEL under supervise, but $PLIST no longer does: a move back to serve wrote the plist and never restarted the job. Finish that move with: activate-laptop-supervisor.sh --rollback" ;;
        */supervised)
            echo "launchd runs $LABEL as a plain serve, but $PLIST runs supervise: an activation wrote the plist and never restarted the job. Put the plist back with: activate-laptop-supervisor.sh --rollback, then activate again" ;;
        *) return 1 ;;
    esac
}

await() { # seconds what command...: true once the command is, within the limit
    local limit="$1" what="$2" waited=0
    shift 2
    until "$@"; do
        [ "$waited" -ge "$limit" ] && return 1
        [ "$waited" -gt 0 ] && [ $((waited % 10)) = 0 ] && echo "   waiting for $what (${waited}s of ${limit}s)"
        sleep 1; waited=$((waited + 1))
    done
}

LAUNCHD_ERR=""

# Boots the job out and waits on the JOB, never the port: launchd holds the
# label for the whole drain, and a bootstrap before then fails with `5:
# Input/output error`. Only rc 113 says the job is gone; a shell launchd
# refuses gets 141 for a job that is still loaded.
bootout_and_wait() { # [exit timeout seconds]
    local exit_timeout="${1:-}" limit=600 waited=0 out rc
    case "$exit_timeout" in ''|*[!0-9]*) ;; *) [ $((exit_timeout + 60)) -gt $limit ] && limit=$((exit_timeout + 60)) ;; esac
    LAUNCHD_ERR=""
    echo "   booting out $LABEL"
    out=$(launchctl bootout "$LABEL" 2>&1)
    while :; do
        launchctl print "$LABEL" >/dev/null 2>&1; rc=$?
        [ "$rc" = 0 ] || break
        if [ "$waited" -ge "$limit" ]; then
            LAUNCHD_ERR="launchd still holds $LABEL ${limit}s after the bootout${out:+ ($out)}"
            echo "   $LAUNCHD_ERR"
            return 1
        fi
        [ "$waited" -gt 0 ] && [ $((waited % 10)) = 0 ] && echo "   waiting for launchd to let the old daemon go (${waited}s of ${limit}s)"
        sleep 1; waited=$((waited + 1))
    done
    if [ "$rc" != 113 ]; then
        LAUNCHD_ERR="launchctl print $LABEL answered rc $rc after the bootout${out:+ ($out)}, and only 113 means the job is gone; not bootstrapping blind"
        echo "   $LAUNCHD_ERR"
        return 1
    fi
    echo "   job gone"
}

bootstrap_job() {
    local out rc
    LAUNCHD_ERR=""
    out=$(launchctl bootstrap "gui/$(id -u)" "$PLIST" 2>&1); rc=$?
    [ "$rc" = 0 ] && return 0
    LAUNCHD_ERR="launchctl bootstrap gui/$(id -u) $PLIST failed: rc $rc${out:+: $out}"
    echo "   $LAUNCHD_ERR"
    return 1
}

control() { # method path max-seconds -> the body, then the HTTP code on a line of its own
    curl -s -m "$3" -w '\n%{http_code}' --unix-socket "$CONTROL_SOCKET" -X "$1" "http://supervisor$2" 2>/dev/null
}

status_field() { # name | active_identity
    control GET /_subrouter/supervisor-status 5 | sed '$d' | python3 -c '
import json, sys
try:
    s = json.load(sys.stdin)
except Exception:
    sys.exit(0)
if sys.argv[1] == "active_identity":
    print((s.get("active_worker") or {}).get("executable_identity") or "")
else:
    v = s.get(sys.argv[1])
    print(str(v).lower() if isinstance(v, bool) else v or "")
' "$1" 2>/dev/null
}

serving() { # <binary>: the supervisor's active worker runs it
    local want got
    want=$(cdhash "$1"); got=$(status_field active_identity)
    echo "   active worker              -> ${got:-no answer}   ($1: ${want:-not signed})"
    [ -n "$want" ] && [ "$want" = "$got" ]
}

# --- probes -------------------------------------------------------------------
# A probe returns a CLASSIFICATION, never a bare HTTP code, because the code alone
# cannot separate "this build broke the route" from "every account is spent" --
# and only the first is a reason to roll a deploy back.
#
# On 2026-09-16 that conflation rolled back a binary later proved route-identical
# to the one it replaced. The pool had exhausted its weekly overage allowance, the
# provider probe timed out, and a timeout surfaces as HTTP 000 -- the same code a
# missing route produces. What settled it: the ALREADY-DEPLOYED binary then failed
# the identical check three runs out of three. A check that fails for the binary
# it just passed for is measuring the pool, not the build.

# POST /v1/messages -> mounted | gone | unreachable
#
# SUBROUTER_ALLOW_LOOPBACK_PROXY exempts a loopback caller from the token gate, so
# on such a host this is NOT the cheap "did it reach the credential gate" touch it
# reads as: it is a real upstream request that walks the whole retry chain, and it
# can outlive any timeout we pick.
#
# A missing route answers 404 at once -- it does not hang. So a timeout (curl 28)
# is evidence FOR the route: the listener accepted the connection and a handler is
# working on it. Only a connection that never got that far (curl 7 and friends)
# means nothing is serving the path.
probe_provider() {
    local code rc
    code=$(curl -s -m 10 -o /dev/null -w '%{http_code}' -X POST "http://$ADDR/v1/messages" -d '{}' 2>/dev/null)
    rc=$?
    case "$code" in
        404) echo "gone" ;;
        000) if [ "$rc" -eq 28 ]; then echo "mounted"; else echo "unreachable"; fi ;;
        *)   echo "mounted" ;;
    esac
}

# GET /api/oauth/profile, three times -> stable:<email> | rotating | unavailable:<status>
#
# The control-plane identity pin. Rotating this is not a degraded answer but a
# WRONG one: the client caches whichever org replies as its own identity, and a
# pooled daemon previously answered from a different account each call -- which
# cached another account's organization settings and hid a model from the
# picker. Asserted because it is invisible to every other check.
#
# `unavailable` is kept distinct from `rotating`: this probe needs one usable
# account to answer at all, so a rate-limited pool silences it, and silence is not
# evidence about the pin in either direction. verify_routes decides what that means.
probe_identity() {
    local first="" same=1 who body status last_status=""
    for _ in 1 2 3; do
        # The Claude Code user-agent is REQUIRED: without it Anthropic answers 403
        # and this probe silently reports nothing, which previously made the whole
        # assertion skip while the deploy still printed success.
        body=$(curl -s -m 20 -w '\n%{http_code}' -H "Authorization: Bearer $TOKEN" \
              -H "anthropic-beta: oauth-2025-04-20" \
              -H "x-app: cli" -H "user-agent: claude-cli/2.1.257 (external, cli)" \
              "http://$ADDR/api/oauth/profile" 2>/dev/null)
        status=$(printf '%s' "$body" | tail -1)
        who=$(printf '%s' "$body" | sed '$d' \
              | python3 -c 'import json,sys; print((json.load(sys.stdin).get("account") or {}).get("email",""))' 2>/dev/null)
        [ -n "$status" ] && last_status="$status"
        [ -z "$first" ] && first="$who"
        [ "$who" = "$first" ] || same=0
    done
    if [ -z "$first" ]; then echo "unavailable:${last_status:-no-answer}"
    elif [ "$same" = 1 ]; then echo "stable:$first"
    else echo "rotating"; fi
}

# <socket> -> serving | down:<why>
#
# Probed THROUGH the socket, because nothing on TCP proves it: the daemon fails
# the socket soft and keeps serving TCP, and a client in socket mode that finds
# the path dead or missing does not fall back to TCP -- it retries silently for
# minutes (measured against Claude Code 2.1.284: no output for 60-90 s, zero TCP
# requests). So "healthy" above can be true while every socket-mode session
# hangs. Host is api.anthropic.com, as Claude Code sends it. The daemon's own
# word for the socket (health `claude_socket` on TCP) rides along to say why.
probe_claude_socket() {
    local ok word
    ok=$(curl -s -m 5 --unix-socket "$1" "http://api.anthropic.com/_subrouter/health" 2>/dev/null \
        | python3 -c 'import json,sys; print(json.load(sys.stdin).get("ok") is True)' 2>/dev/null)
    if [ "$ok" = True ]; then echo "serving"; return; fi
    if [ "${SUPERVISED:-0}" = 1 ]; then
        echo "down:no health answer on $1 (supervisor says: $(status_field claude_socket || echo no answer))"
        return
    fi
    word=$(curl -s -m 3 "http://$ADDR/_subrouter/health" 2>/dev/null \
        | python3 -c 'import json,sys; print(json.load(sys.stdin).get("claude_socket") or "not reported")' 2>/dev/null)
    echo "down:no health answer on $1 (daemon says: ${word:-no answer})"
}

# GET /_subrouter/accounts -> how many accounts the daemon loaded
#
# Separate from the 200-check because the count answers a different question, and
# on some hosts the honest answer is zero: a machine that pools nothing locally
# and draws entirely on a peer holds no accounts of its own and never will.
probe_account_count() {
    curl -s -m 20 -H "Authorization: Bearer $TOKEN" \
        "http://$ADDR/_subrouter/accounts" | grep -o '"provider"' | wc -l | tr -d ' '
}

# --- verification -------------------------------------------------------------
# Each check is a route that a bad build removes SILENTLY.
#
# The checks run TWICE: once against the old binary before the swap, and once
# after. A check fails the deploy only when it REGRESSED across that boundary,
# because a check already failing beforehand cannot have been caused by the build
# being installed. On a host whose pool health moves on its own, absolute
# pass/fail is the wrong question; "did this swap change it" is the right one.
#
# --check has no baseline, so there every not-good answer stands on its own.
verify_routes() { # $1 = provider baseline, $2 = identity baseline, $3 = accounts baseline, $4 = socket baseline (empty under --check)
    local base_provider="${1:-}" base_identity="${2:-}" base_accounts="${3:-}" base_socket="${4:-}"
    local bad=0
    if ! healthy; then echo "   health: DOWN"; return 1; fi
    echo "   health: ok"
    if [ "${SUPERVISED:-0}" = 1 ] && ! serving "$CUR"; then
        echo "      ^ the supervisor's active worker is not $CUR"
        bad=1
    fi

    # Regression-compared like the rest. A socket the old build served and this
    # one does not is this build's fault: roll back. A socket neither served is
    # not, and the old binary cannot fix it (a pre-section-29 build ignores the
    # variable), so it is INCOMPLETE rather than a rollback -- the usual cause is
    # a plist edit that a plain kickstart never read (rerun with RELOAD=1).
    if [ -n "$CLAUDE_SOCKET" ]; then
        local sock; sock=$(probe_claude_socket "$CLAUDE_SOCKET")
        echo "   claude socket              -> $sock${base_socket:+   (before: $base_socket)}"
        if [ "$sock" != "serving" ]; then
            if [ -n "$base_socket" ] && [ "$base_socket" != "serving" ]; then
                echo "      ^ not served before the swap either; this build did not cause it. Every"
                echo "        socket-mode Claude session hangs until it is. If the plist was just edited: RELOAD=1"
                SOCKET_INCOMPLETE=1
            else
                bad=1
            fi
        fi
    fi

    local provider; provider=$(probe_provider)
    echo "   POST /v1/messages          -> $provider${base_provider:+   (before: $base_provider)}"
    if [ "$provider" != "mounted" ]; then
        if [ -n "$base_provider" ] && [ "$provider" = "$base_provider" ]; then
            echo "      ^ answered the same before the swap; this build did not cause it"
        else
            bad=1
        fi
    fi

    local accounts; accounts=$(code -H "Authorization: Bearer $TOKEN" "http://$ADDR/_subrouter/accounts")
    echo "   GET  /_subrouter/accounts  -> $accounts   (want 200)"
    [ "$accounts" = "200" ] || bad=1

    # Regression-compared, like provider and identity above. It was ABSOLUTE, and
    # that contradicted this function's own rule: a host that pools nothing
    # locally and draws entirely on a peer loads zero accounts on EVERY build, so
    # the check could never pass there and the deploy could never complete. A
    # check that fails identically for the binary it just passed for is measuring
    # the machine, not the build -- the same conflation that rolled back a
    # route-identical binary on 2026-09-16.
    local count; count=$(probe_account_count)
    echo "   accounts loaded            -> ${count:-0}   (want >=1)${base_accounts:+   (before: $base_accounts)}"
    if [ "${count:-0}" -lt 1 ]; then
        if [ -n "$base_accounts" ] && [ "${base_accounts:-0}" -lt 1 ]; then
            echo "      ^ the old build loaded none either, so this machine draws on a peer pool; not caused by this build"
        else
            bad=1
        fi
    fi

    local identity; identity=$(probe_identity)
    case "$identity" in
        stable:*)
            echo "   control-plane identity     -> stable (${identity#stable:})" ;;
        rotating)
            echo "   control-plane identity     -> ROTATING -- clients will cache a stranger's org"
            bad=1 ;;
        unavailable:*)
            # Still not a silent skip: a check that cannot run and lets the deploy
            # print OK reads as one that ran and passed. But it is only the
            # BUILD's fault if it used to answer. This probe needs a usable
            # account, and a rate-limited pool has none to lend it.
            echo "   control-plane identity     -> COULD NOT PROBE (upstream: ${identity#unavailable:}); not proof of rotation"
            case "$base_identity" in
                stable:*|rotating)
                    echo "      ^ it answered before the swap, so this IS a regression"
                    bad=1 ;;
                unavailable:*)
                    echo "      ^ equally silent before the swap; this build did not cause it" ;;
                *)  bad=1 ;;
            esac ;;
    esac
    return $bad
}

# --- install ------------------------------------------------------------------
install_binary() { # $1 = source, $2 = destination
    local stage="$2.stage.$$"
    # Ad-hoc sign the STAGED copy, before anything can exec it.
    #
    # -i subrouter is NOT cosmetic. Without an explicit identifier codesign
    # derives one from the FILENAME it is signing, so staging under a temp name
    # stamps the binary `subrouter.stage.<pid>` and every deploy produces a
    # different identity. Observed drift on one machine in a single day:
    # `a.out` (bare go build), `subrouter-5555…`, `subrouter.tmp.30799`,
    # `subrouter.stage.65298`. launchd caches a code requirement per service
    # ("managed LWCR" in `launchctl print`), and an identity that keeps moving is
    # what gets execs killed with OS_REASON_CODESIGNING while keepalive respawns
    # forever. `make build` gets this right only by accident, because it happens
    # to sign a file already named `subrouter`.
    if cp "$1" "$stage" && chmod 755 "$stage" && codesign -s - -f -i subrouter "$stage" >/dev/null 2>&1 \
        && mv -f "$stage" "$2"; then
        return 0
    fi
    rm -f "$stage"
    return 1
}

# The pid listening on the daemon's port, or nothing. lsof needs no launchd, so
# it answers even in a shell launchctl refuses.
listener_pid() { lsof -nP -iTCP:"${ADDR##*:}" -sTCP:LISTEN -t 2>/dev/null | head -1; }

# Healthy, and served by a different process than $1, the listener before the
# restart. Health alone passed a kickstart that did nothing, because the old
# process went on answering it.
came_back() { # $1 = listener pid before the restart
    local now
    healthy || return 1
    now=$(listener_pid)
    [ -n "$now" ] && [ "$now" != "$1" ]
}

take_baseline() {
    BASE_PROVIDER=""; BASE_IDENTITY=""; BASE_ACCOUNTS=""; BASE_SOCKET=""
    healthy || { echo "   baseline: none, the daemon does not answer on $ADDR"; return 0; }
    BASE_PROVIDER=$(probe_provider); BASE_IDENTITY=$(probe_identity)
    BASE_ACCOUNTS=$(probe_account_count)
    [ -n "$CLAUDE_SOCKET" ] && BASE_SOCKET=$(probe_claude_socket "$CLAUDE_SOCKET")
    echo "   baseline: provider=$BASE_PROVIDER  identity=${BASE_IDENTITY%%:*}  accounts=$BASE_ACCOUNTS${CLAUDE_SOCKET:+  claude-socket=${BASE_SOCKET%%:*}}"
}
