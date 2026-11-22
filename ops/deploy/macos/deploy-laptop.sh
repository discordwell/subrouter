#!/usr/bin/env bash
# Deploy a subrouter build to the LAPTOP peer, verifying the routes and rolling
# back to this run's backup if anything fails.
#
#   deploy-laptop.sh <binary>            deploy it
#   deploy-laptop.sh <binary> --check    verify the RUNNING daemon; touch nothing
#   RELOAD=1 deploy-laptop.sh <binary>   you also edited the plist
#   PLIST_SET_ENV=<name>=<value> | PLIST_DELETE_ENV=<name> deploy-laptop.sh <binary>
#                                        edit the plist's environment under this run's
#                                        lock, and put it back on a rollback (claude-route)
#   deploy-laptop.sh <binary> --restart-supervisor
#                                        a supervised job: also replace the supervisor, and restart it
#   KEEP_BACKUPS=<n> ...                 backups to keep once the routes verify (default 5)
#
# Once the routes verify it also prunes old backups and refreshes the checkout
# srq runs from (step 7, PATCHES §40).
#
# Paths (env): SUBROUTER_LABEL (ai.manaflow.subrouter), SUBROUTER_PLIST
# (~/Library/LaunchAgents/<label>.plist), SUBROUTER_ADDR (127.0.0.1:31415),
# SUBROUTER_CONTROL_SOCKET (when the job names none; supervise's own default is
# /var/run/subrouter-supervisor.sock). The binary it replaces is the one the
# job runs: a plain serve's program, or a supervised job's --worker-bin.
#
# Exit codes: 0 OK; 1 FAILED (rolled back, or a rollback was refused, or this
# shell cannot restart the daemon -- read the message); 2 the daemon is DOWN;
# 3 INCOMPLETE -- the new build is serving and is kept, but Codex accounts fail
# its credential-isolation gate (see below), or the plist names a Claude socket
# that neither build serves (see probe_claude_socket).
#
# It exists because the laptop had only a ROLLBACK script
# (rollback-laptop-peer.sh). Deploys were therefore ad-hoc, and on 2026-09-01 an
# ad-hoc one took the daemon down for ~14 minutes and 53 respawns. Everything
# below is a rule that outage taught.
#
# Five rules:
#
#  * Never write over the running binary. `cp` onto a live Mach-O that a running
#    process has mapped leaves a stale signature, and launchd then SIGKILLs every
#    exec with OS_REASON_CODESIGNING while `keepalive` respawns it forever. Stage
#    beside it, re-sign, rename. A rollback that uses plain cp re-poisons the SAME
#    inode, so even known-good bytes stay dead -- which is what turned a failed
#    deploy into an outage.
#  * Re-sign every staged binary. `make build` codesigns; a bare `go build` does
#    not, and signing is what the Keychain access Claude profiles depend on.
#  * Wait on the JOB, never the port. The port frees ~1s after SIGTERM while the
#    process keeps serving established connections for the whole drain (ceiling
#    10m). Bootstrapping into a domain that still holds the label returns
#    launchd's unhelpful `5: Input/output error`.
#  * Never declare success from health alone. /_subrouter/health answers 200 from
#    a daemon whose route table is broken.
#    Nor from a restart launchctl may not have done: the process answering
#    health must be a NEW one (a different pid on the port).
#  * Restart only from a shell that can reach launchd (launchd_blocker, PATCHES
#    §47). A terminal that outlived a logout cannot, and from one this script
#    printed DEPLOY OK while the old binary kept serving.
#
# CODEX CREDENTIAL ISOLATION (the 2026-09 upstream merge and later). A
# post-isolation build refuses every stored Codex record without an
# oauthCredentialOrigin -- every record a pre-isolation build wrote -- while
# every route check below still passes. So:
#
#  * After the swap (and under --check) the NEW binary's read-only
#    `codex isolation-check --json` runs with the daemon's own HOME, state dir
#    and CODEX_HOME, read from its plist and, for a supervised worker, its
#    worker config. Accounts needing migration print
#    INCOMPLETE with the remediation (`codex attest-legacy`, then
#    `sr codex migrate-isolation` for a record that shares ~/.codex/auth.json
#    or whose chain is dead), and never roll back.
#  * Swap FIRST, migrate AFTER: attesting or re-logging and
#    then letting a pre-isolation binary run writes the new chain into
#    ~/.codex/auth.json, OpenCode and pi, where Codex Desktop's earlier refresh
#    kills the pool copy. For the same reason -- and because each refresh it
#    makes strips the origin (upstream docs/upgrades.md rollback boundary) -- a
#    rollback that would restore a pre-isolation backup over a post-isolation
#    build is REFUSED, and so is deploying a pre-isolation binary over one,
#    once the daemon's Codex store holds a MIGRATED record (origin
#    isolated-server-login or server-attested-transfer), or exists and cannot
#    be read -- unless ALLOW_PRE_ISOLATION_ROLLBACK=1. Before the first
#    migration the old binary serves the store exactly as it did, so a broken
#    first post-isolation build (swapped first, as above) still rolls
#    back on its own.
set -uo pipefail

SELF=$(python3 -c 'import os, sys; print(os.path.realpath(sys.argv[1]))' "$0")
# shellcheck source=laptop-deploy-lib.sh
. "$(dirname "$SELF")/laptop-deploy-lib.sh" || exit 2
DETACHED="${SUBROUTER_LAPTOP_DETACHED:-}"
unset SUBROUTER_LAPTOP_DETACHED

BIN="${1:-}"
MODE="${2:-}"
JOB="${SUBROUTER_LABEL:-ai.manaflow.subrouter}"
PLIST="${SUBROUTER_PLIST:-$HOME/Library/LaunchAgents/$JOB.plist}"
LABEL="gui/$(id -u)/$JOB"
ADDR="${SUBROUTER_ADDR:-127.0.0.1:31415}"
STAMP=$(date +%Y%m%d-%H%M%S)

[ -n "$BIN" ] || { echo "usage: $(basename "$0") <binary> [--check | --restart-supervisor]" >&2; exit 2; }
[ "$MODE" = "--check" ] || [ -f "$BIN" ] || { echo "no such binary: $BIN" >&2; exit 2; }

# The token every local client authenticates with. Read from settings.json rather
# than the environment so --check works from any shell, and never echoed.
TOKEN="$(local_token)"

fail() { echo "DEPLOY FAILED: $*" >&2; }

PLIST_SET_ENV="${PLIST_SET_ENV:-}"; PLIST_DELETE_ENV="${PLIST_DELETE_ENV:-}"; PLIST_BACKUP=""
[ -n "$PLIST_SET_ENV$PLIST_DELETE_ENV" ] && RELOAD=1
edit_plist_env() {
    python3 - "$PLIST" "$PLIST_SET_ENV" "$PLIST_DELETE_ENV" <<'PY'
import os, plistlib, stat, sys
path, set_env, delete_env = sys.argv[1:4]
with open(path, "rb") as f:
    p = plistlib.load(f)
env = p.setdefault("EnvironmentVariables", {})
if set_env:
    name, value = set_env.split("=", 1)
    env[name] = value
if delete_env:
    env.pop(delete_env, None)
tmp = "%s.deploy.%d" % (path, os.getpid())
with open(tmp, "wb") as f:
    plistlib.dump(p, f)
os.chmod(tmp, stat.S_IMODE(os.stat(path).st_mode))
os.replace(tmp, path)
PY
}

if [ "$MODE" != --check ]; then take_mutation_lock || exit 1; fi
job_state
if CONFLICT=$(shape_conflict); then
    fail "$CONFLICT. Nothing was changed."
    exit 1
fi
SHAPE="$LOADED_SHAPE"
case "$SHAPE" in none|unknown) SHAPE="$PLIST_SHAPE" ;; esac
CUR="$JOB_BINARY"
if [ -z "$CUR" ]; then
    fail "cannot tell which binary $LABEL runs: $PLIST runs neither '<binary> serve' nor supervise with a --worker-bin. Nothing was changed."
    exit 1
fi
SUPERVISED=0; SUPERVISOR_BIN=""; CONTROL_SOCKET=""; WORKER_ENV_CONFIG=""
READY_TIMEOUT="${JOB_READY_SECONDS:-30}"
if [ "$SHAPE" = supervised ]; then
    SUPERVISED=1
    SUPERVISOR_BIN="$JOB_PROGRAM"
    CONTROL_SOCKET="${JOB_CONTROL_SOCKET:-${SUBROUTER_CONTROL_SOCKET:-/var/run/subrouter-supervisor.sock}}"
    WORKER_ENV_CONFIG="$JOB_WORKER_CONFIG"
fi
BACKUP="$CUR.bak-$STAMP"
SUP_BACKUP=""
SWAP=restart
[ "$SUPERVISED" = 1 ] && [ "$MODE" != --restart-supervisor ] && SWAP=upgrade
LAUNCHD_NEEDS=""
{ [ "$SWAP" = restart ] || [ "$LOADED_SHAPE" = none ]; } && LAUNCHD_NEEDS=restart

# --- Codex credential isolation -----------------------------------------------
# The daemon's own HOME / SUBROUTER_STATE_DIR / CODEX_HOME, read from its plist
# and, for a supervised worker, its worker config, so the check inspects the
# store the daemon serves rather than whatever this shell happens to export.
# One KEY=value per line; a key neither sets is left out, and as_svc then
# UNSETS it -- as launchd does.
svc_env() {
    python3 - "$PLIST" "$WORKER_ENV_CONFIG" <<'PY' 2>/dev/null
import json, plistlib, sys
try:
    with open(sys.argv[1], "rb") as f:
        env = plistlib.load(f).get("EnvironmentVariables") or {}
except Exception:
    env = {}
if sys.argv[2]:
    try:
        with open(sys.argv[2]) as f:
            env.update(json.load(f).get("env") or {})
    except Exception:
        pass
for key in ("HOME", "SUBROUTER_STATE_DIR", "CODEX_HOME"):
    if env.get(key):
        print(f"{key}={env[key]}")
PY
}

as_svc() { # <binary> <args...>, with the daemon's environment
    local assign extra=()
    while IFS= read -r assign; do
        [ -n "$assign" ] && extra+=("$assign")
    done <<EOF
$(svc_env)
EOF
    env -u CODEX_HOME -u SUBROUTER_STATE_DIR -u SUBROUTER_CODEX_SERVER \
        ${extra[@]+"${extra[@]}"} "$@"
}

# The same environment as text to paste in front of a remediation command, so
# the operator's own shell fixes the store the daemon serves. Empty when the
# plist sets nothing beyond this user's HOME and this shell exports neither key.
svc_prefix() {
    local assign text=""
    while IFS= read -r assign; do
        case "$assign" in ""|"HOME=$HOME") ;; *) text="$text$assign " ;; esac
    done <<EOF
$(svc_env)
EOF
    if [ -n "$text" ] || [ -n "${CODEX_HOME+x}" ] || [ -n "${SUBROUTER_STATE_DIR+x}" ]; then
        printf 'env -u CODEX_HOME -u SUBROUTER_STATE_DIR %s' "$text"
    fi
}

# <binary> -> ok | migrate:<N> | error:<why>
# It exits 1 both when records need migration and when it cannot run at all, so
# the verdict is read from the JSON: no JSON means the check did not run, which
# is never "ok".
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

# Runs the gate against $CUR and prints the remediation when it fails.
# Returns 0 when the Codex pool is served (or the build predates the gate).
isolation_gate() {
    if ! is_post_isolation "$CUR"; then
        echo "   n/a: this build predates the isolation gate"
        return 0
    fi
    local state; state=$(isolation_state "$CUR")
    echo "   isolation-check: $state"
    [ "$state" = ok ] && return 0
    case "$state" in
        migrate:*) echo "   ${state#migrate:} Codex account(s) fail the credential-isolation gate and are REFUSED." ;;
        *)         echo "   The check could not run (${state#error:}), so nothing proves the Codex pool is served." ;;
    esac
    local pfx; pfx=$(svc_prefix)
    echo "   Not rolled back: the old binary cannot fix this. Fix forward (swap first, migrate after):"
    echo "     1. $pfx$CUR codex attest-legacy"
    echo "     2. ${pfx}sr codex migrate-isolation   for each record it skips as sharing ~/.codex/auth.json,"
    echo "        or reports as failed (a dead chain). Quit Codex Desktop first: it aborts if"
    echo "        ~/.codex/auth.json changes mid-run."
    echo "     3. ${pfx}sr codex isolation-check --json   (want \"ok\":true)"
    return 1
}

# The daemon's Codex store (storepath.CodexDir): <state>/codex/accounts, where
# <state> is the plist's SUBROUTER_STATE_DIR, else its HOME/.subrouter -- never
# this shell's, which launchd does not pass on.
codex_store() {
    local assign home="$HOME" state=""
    while IFS= read -r assign; do
        case "$assign" in
            HOME=*) home="${assign#HOME=}" ;;
            SUBROUTER_STATE_DIR=*) state="${assign#SUBROUTER_STATE_DIR=}" ;;
        esac
    done <<EOF
$(svc_env)
EOF
    printf '%s/codex/accounts' "${state:-$home/.subrouter}"
}

# <store> -> the number of records a post-isolation build has MIGRATED (origin
# isolated-server-login or server-attested-transfer): the ones a pre-isolation
# binary damages, by re-exporting the chain and stripping the origin on its
# next refresh. A record with no origin, or interactive-import, it serves as
# before. Returns 2 -- the reason on stdout -- when the store cannot be read.
# Missing counts as 0 only when it provably does not exist: a parent that
# cannot be searched hides the store rather than disproving it.
migrated_records() {
    local d="$1" p="$1" q f n=0 rc
    if [ ! -e "$d" ]; then
        while [ ! -e "$p" ]; do q=$(dirname "$p"); [ "$q" = "$p" ] && break; p=$q; done
        if [ -d "$p" ] && [ -x "$p" ]; then echo 0; return 0; fi
        echo "cannot tell whether $d exists: $p cannot be searched"; return 2
    fi
    if [ ! -d "$d" ] || [ ! -r "$d" ] || [ ! -x "$d" ]; then echo "$d exists but cannot be read"; return 2; fi
    for f in "$d"/*.json; do
        [ -e "$f" ] || [ -L "$f" ] || continue
        [ -d "$f" ] && continue
        LC_ALL=C grep -Eq '"oauthCredentialOrigin"[[:space:]]*:[[:space:]]*"(isolated-server-login|server-attested-transfer)"' "$f" 2>/dev/null
        rc=$?
        case $rc in 0) n=$((n + 1)) ;; 1) ;; *) echo "$f cannot be read"; return 2 ;; esac
    done
    echo "$n"
}

# Why restoring a pre-isolation binary would damage the daemon's Codex store.
# Prints the reason and returns 0 when it would; prints why not and returns 1
# when it would not. An unreadable store is a reason: nothing proves it holds
# no migrated record.
migration_hazard() {
    local store out
    store=$(codex_store)
    if ! out=$(migrated_records "$store"); then
        echo "the Codex store could not be read ($out), so nothing proves no record was migrated"
        return 0
    fi
    if [ "$out" = 0 ]; then
        echo "no Codex record in $store carries a migrated origin"
        return 1
    fi
    echo "$out Codex record(s) in $store carry oauthCredentialOrigin isolated-server-login or server-attested-transfer"
}

# The credential-origin rollback boundary. Returns 0 -- and says why -- when
# restoring $BACKUP over $CUR must NOT happen.
rollback_refused() { # $1 = what failed
    is_post_isolation "$CUR" || return 1
    is_post_isolation "$BACKUP" && return 1
    [ "${ALLOW_PRE_ISOLATION_ROLLBACK:-0}" = 1 ] && return 1
    local why
    if ! why=$(migration_hazard); then
        echo "   $BACKUP predates Codex credential isolation, but $why; restoring it is safe." >&2
        return 1
    fi
    echo "!! NOT ROLLING BACK ($1): $BACKUP predates Codex credential isolation, and $why." >&2
    echo "   Restoring it would cross the rollback boundary (upstream docs/upgrades.md): it" >&2
    echo "   re-exports a pool chain into ~/.codex/auth.json, OpenCode and pi, and every refresh" >&2
    echo "   it makes strips the oauthCredentialOrigin this build requires. The new build stays" >&2
    echo "   installed. Fix forward:" >&2
    echo "     tail -50 ~/Library/Logs/subrouter.err.log" >&2
    echo "     $CUR codex isolation-check --json" >&2
    echo "   To restore it anyway (only if the outage is worse than re-migrating those records after):" >&2
    echo "     ALLOW_PRE_ISOLATION_ROLLBACK=1 $0 $BACKUP" >&2
    return 0
}


# --- after a verified deploy: old backups and the ops tools (PATCHES §40) ------
# Two things a laptop deploy left behind that nothing ever cleaned up.

# BACKUPS. Every run leaves $CUR.bak-<stamp> and nothing removed one: a laptop
# held 25 (~600 MB) on 2026-09-28, 19 of them from before Codex credential
# isolation. Only the backup THIS run took is a rollback target (KNOWN-ISSUES.md:
# restoring an older release re-arms a pool-wide brick), so the rest were never
# a safety net -- they were pre-boundary binaries waiting for a hand-rolled swap
# to pick one up, and the ad-hoc swap script on that laptop defaulted to one.
# After the routes verify, the newest KEEP_BACKUPS (default 5, this run's
# always among them) stay and the rest go. Only names this script writes
# (bak-YYYYMMDD-...) are pruned; anything else is left alone. Anything but a
# count from 1 to 999999 prunes nothing: 0, 00, a word, or a number too big
# for `[` (which then errors instead of comparing, and must not fall through
# to the rm).
KEEP_BACKUPS="${KEEP_BACKUPS:-5}"

prune_backups() {
    local n="" f list kept=0 removed=0
    case "$KEEP_BACKUPS" in
        ''|*[!0-9]*) ;;
        *) [ "${#KEEP_BACKUPS}" -gt 6 ] || n=$((10#$KEEP_BACKUPS)) ;;
    esac
    if [ -z "$n" ] || [ "$n" -eq 0 ]; then
        echo "   backups: nothing pruned (KEEP_BACKUPS=$KEEP_BACKUPS is not a count from 1 to 999999)"
        return 0
    fi
    list=$(for f in "$CUR".bak-[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]-*; do
               if [ -f "$f" ] && [ ! -L "$f" ]; then printf '%s\n' "$f"; fi
           done | LC_ALL=C sort -r)
    while IFS= read -r f; do
        [ -n "$f" ] || continue
        if [ "$f" = "$BACKUP" ] || [ "$kept" -lt "$n" ]; then
            kept=$((kept + 1))
        elif [ "$kept" -ge "$n" ] && rm -f "$f"; then
            removed=$((removed + 1))
        fi
    done <<EOF
$list
EOF
    echo "   backups: kept the newest $kept, removed $removed (KEEP_BACKUPS=$KEEP_BACKUPS)"
}

# Pre-isolation binaries still beside the daemon's once a Codex record is
# migrated: kept backups, and anything else named $CUR.*. From then on none of
# them is a rollback target -- this script refuses to install one -- but a
# hand-rolled swap does not ask. Before the first migration they are still
# the documented rollback (rollback_refused), so nothing is said. Named, never
# deleted: this script did not write most of them.
name_pre_isolation_binaries() {
    is_post_isolation "$CUR" || return 0
    local f found="" why
    for f in "$CUR".*; do
        { [ -f "$f" ] && [ ! -L "$f" ]; } || continue
        case "$f" in "$CUR".stage.*) continue ;; esac
        is_post_isolation "$f" || found="$found $(basename "$f")"
    done
    [ -n "$found" ] || return 0
    why=$(migration_hazard) || return 0
    echo "   !! pre-isolation binaries beside $CUR:$found"
    echo "      $why, so restoring any of them crosses the Codex credential-isolation"
    echo "      rollback boundary (upstream docs/upgrades.md). Nothing should; move them out of $(dirname "$CUR")."
}

# OPS TOOLS. srq runs from whatever checkout its links point into -- not from
# the tree this deploy came from -- and nothing moved that checkout. On
# 2026-09-28 srq ran from a worktree pinned seven merges behind main, and it
# rendered a daemon that old. So each link is compared with the copy in the
# tree the deployed binary was built in (ops_reference).
#
# That checkout is SHARED -- every session's srq -- and a worktree of
# the same clone holds every lane's commits. So a stale one is moved only
# forward, only to a merged commit, and only when nothing can be lost: clean,
# on a detached HEAD (a pinned tools checkout, never someone's branch), not the
# checkout this script is running from, already holding the commit, the commit
# on origin/main, and its HEAD an ancestor of it (ops_move_blocker). A deploy
# from a feature worktree or an older main therefore leaves it alone. Anything
# else is printed with the command for that case.
OPS_DIR=$(cd "$(dirname "$0")/../.." 2>/dev/null && pwd -P)
OPS_MAIN="refs/remotes/origin/main"

ops_tool_links() { # "<path under ops> <link>" per line; the link may hold spaces
    local on_path
    echo "bin/srq $HOME/bin/srq"
    on_path=$(command -v srq 2>/dev/null)
    case "$on_path" in /*) [ "$on_path" = "$HOME/bin/srq" ] || echo "bin/srq $on_path" ;; esac
}

real_path() { python3 -c 'import os, sys; print(os.path.realpath(sys.argv[1]))' "$1" 2>/dev/null; }

# A file or a whole directory, byte for byte; Finder's .DS_Store is not the tool.
same_tool() { diff -rq -x .DS_Store "$1" "$2" >/dev/null 2>&1; }

# A checkout of this repo, not any repo that happens to have an ops/.
is_this_repo() { [ -f "$1/ops/deploy/macos/deploy-laptop.sh" ]; }

# The ops/ the tools should match: the deployed <binary>'s tree
# (`make build` leaves it at <tree>/bin/subrouter), not this
# script's. Run from the tools checkout itself, the script's own tree would
# compare every link with itself and call a stale checkout current. A binary
# outside any checkout of this repo falls back to this script's tree, and the
# report says so. Sets REF_OPS, REF_REV (empty outside git) and REF_NOTE.
ops_reference() {
    local real top=""
    REF_OPS="$OPS_DIR"; REF_NOTE=""
    real=$(real_path "$BIN")
    [ -z "$real" ] || top=$(git -C "$(dirname "$real")" rev-parse --show-toplevel 2>/dev/null)
    if [ -n "$top" ] && is_this_repo "$top"; then
        REF_OPS="$top/ops"
    else
        REF_NOTE="$BIN is not in a checkout of this repo, so the tools are compared with this script's tree, $OPS_DIR"
    fi
    REF_REV=$(git -C "$REF_OPS" rev-parse --verify -q HEAD 2>/dev/null)
}

# Why the stale tools checkout <top> may not be moved to REF_REV, and the
# command for that case; both empty when it may. Sets the caller's `why` and
# `fix`. In this order, so the first reason named is the one to act on.
ops_move_blocker() { # top script-top [report]
    local top="$1" move branch
    move="git -C $top checkout --detach $REF_REV"
    why=""; fix=""
    if [ -z "$REF_REV" ]; then
        why="$REF_OPS is not in a git checkout"
        fix="re-link it into a checkout you keep current"
    elif [ "$top" = "$2" ]; then
        # git would swap files under a script bash is still reading.
        why="this script is running from $top"
        fix="$move   # once this deploy has finished"
    elif branch=$(git -C "$top" symbolic-ref -q --short HEAD 2>/dev/null); then
        why="$top is on branch $branch, and a branch is never moved"
        fix="git -C $top pull --ff-only   # or keep the tools in a detached worktree"
    elif [ -n "$(git -C "$top" --no-optional-locks status --porcelain --untracked-files=no 2>/dev/null)" ]; then
        why="$top has local changes"
        fix="commit or discard them, then: $move"
    elif ! git -C "$top" cat-file -e "$REF_REV^{commit}" 2>/dev/null; then
        why="$top does not have ${REF_REV:0:9}"
        if [ -n "$(git -C "$REF_OPS" branch -r --contains "$REF_REV" 2>/dev/null)" ]; then
            fix="git -C $top fetch origin && $move"
        else
            fix="no origin branch has ${REF_REV:0:9}; push and merge it first, then: git -C $top fetch origin && $move"
        fi
    elif ! git -C "$top" merge-base --is-ancestor "$REF_REV" "$OPS_MAIN" 2>/dev/null; then
        why="${REF_REV:0:9} is not on origin/main (as $top knows it), and the shared tools only move to merged commits"
        fix="git -C $top fetch origin; to run these tools unmerged anyway: $move"
    elif ! git -C "$top" merge-base --is-ancestor HEAD "$REF_REV" 2>/dev/null; then
        why="its HEAD is not an ancestor of ${REF_REV:0:9}: that would move it backwards, or leave its own commits on no branch"
        fix="deploy from a tree at or past its HEAD; to move it anyway: $move"
    elif [ "${3:-}" = report ]; then
        why="--check touches nothing"
        fix="$move"
    fi
}

refresh_ops_tools() { # [report] -- report only, never move a checkout (--check)
    local link rel real mine top head short why fix noted="" script_top
    [ -n "$OPS_DIR" ] || { echo "   cannot locate this tree's ops/; not checked"; return 0; }
    ops_reference
    script_top=$(git -C "$OPS_DIR" rev-parse --show-toplevel 2>/dev/null)
    while read -r rel link; do
        if [ -L "$link" ] && [ ! -e "$link" ]; then
            echo "   $link: BROKEN link (-> $(readlink "$link")); re-link it into a checkout you keep current"
            continue
        fi
        [ -e "$link" ] || continue
        if [ -n "$REF_NOTE" ] && [ -z "$noted" ]; then echo "   ($REF_NOTE)"; noted=1; fi
        mine="$REF_OPS/$rel"
        real=$(real_path "$link")
        # Never fall back to "." -- that is whatever checkout this shell is in.
        [ -n "$real" ] || { echo "   $link: cannot resolve it; not checked"; continue; }
        if same_tool "$real" "$mine"; then
            echo "   $link: current"
            continue
        fi
        top=$(git -C "$(dirname "$real")" rev-parse --show-toplevel 2>/dev/null)
        if [ -z "$top" ]; then
            echo "   $link: STALE, and not in a git checkout (a copy drifts; a link tracks). Replace it with a link:"
            echo "      ln -sfn <a checkout you keep current>/ops/$rel $link"
            continue
        fi
        # A checkout of another repository never holds this repo's commits,
        # so moving it cannot help.
        if ! is_this_repo "$top"; then
            echo "   $link: STALE -- runs from another repository, $top; no deploy of this one can refresh it. Re-link it:"
            echo "      ln -sfn $REF_OPS/$rel $link"
            continue
        fi
        head=$(git -C "$top" rev-parse --verify -q HEAD 2>/dev/null)
        short="${head:0:9}"
        echo "   $link: STALE -- runs from $top at ${short:-?}; this deploy is ${REF_REV:0:9}"
        ops_move_blocker "$top" "$script_top" "${1:-}"
        if [ -z "$why" ] && git -C "$top" checkout -q --detach "$REF_REV" 2>/dev/null; then
            if same_tool "$real" "$mine"; then
                echo "      refreshed: $top ${short:-?} -> ${REF_REV:0:9}"
            else
                echo "      moved $top to ${REF_REV:0:9}, and it still differs from $mine"
                echo "      (uncommitted changes in the deployed tree, or untracked files in that one)"
            fi
            continue
        fi
        echo "      not refreshed (${why:-git checkout failed}):"
        echo "        ${fix:-git -C $top checkout --detach $REF_REV}"
    done <<EOF
$(ops_tool_links)
EOF
}

upgrade() { # <the build it replaces>
    local out code want was active
    out=$(control POST /_subrouter/upgrade $((READY_TIMEOUT + 60)))
    code=$(printf '%s\n' "$out" | tail -1)
    [ "$code" = 200 ] && return 0
    echo "   upgrade answered HTTP ${code:-000}: $(printf '%s\n' "$out" | sed '$d' | tail -1)"
    want=$(cdhash "$CUR"); was=$(cdhash "$1")
    [ "$want" = "$was" ] && return 1
    active=$(status_field active_identity)
    echo "   supervisor-status: active worker ${active:-no answer}   (new: $want, before: $was)"
    [ "$active" = "$want" ] && return 0
    [ "$active" = "$was" ] && return 1
    return 2
}

SOCKET_INCOMPLETE=0
if [ "$MODE" = "--check" ]; then
    echo "== verify =="
    [ "$SUPERVISED" = 1 ] && echo "   supervised: worker $CUR, control socket $CONTROL_SOCKET"
    verify_routes; rc=$?
    echo "== Codex credential isolation =="
    iso=0; isolation_gate || iso=1
    echo "== ops tools (srq) =="
    refresh_ops_tools report
    if LAUNCHD_WHY=$(launchd_blocker "$LAUNCHD_NEEDS"); then
        echo "== launchd =="
        echo "   NOTE: $LAUNCHD_WHY; a deploy from this shell would refuse. $LAUNCHD_FIX"
    fi
    echo
    if [ $rc -ne 0 ]; then echo "CHECK FAILED"; exit $rc; fi
    if [ $iso -ne 0 ]; then echo "CHECK INCOMPLETE"; exit 3; fi
    echo "CHECK OK"
    exit 0
fi

if [ "$MODE" = --restart-supervisor ] && [ "$SUPERVISED" != 1 ]; then
    fail "--restart-supervisor: $LABEL does not run supervise. Nothing was changed; deploy without it."
    exit 1
fi
if [ "$SWAP" = upgrade ] && [ "${RELOAD:-0}" = 1 ]; then
    fail "RELOAD=1, and every plist edit, restarts the job, and $LABEL runs subrouter supervise. launchd reads a plist edit only when it loads the job, and the supervisor reads its flags and environment (SUBROUTER_CLAUDE_SOCKET among them) only when it starts. A restart closes the port until the new supervisor's first worker is ready and gives open streams at most ExitTimeOut (${EXIT_TIMEOUT:-unset}s) to finish, so it is never done implicitly. Nothing was changed. To restart it anyway: $0 $BIN --restart-supervisor"
    exit 1
fi
[ "$MODE" = --restart-supervisor ] && RELOAD=1
if [ "$SUPERVISED" = 1 ] && ! knows_supervised_socket "$BIN"; then
    fail "$BIN has no supervisor-owned Claude socket (it lacks owned_by_supervisor). As a supervised worker it would bind SUBROUTER_CLAUDE_SOCKET itself, and the retiring worker would unlink the socket under its successor. Build it from this repo; a build of upstream subrouter cannot run this job. Nothing was changed."
    exit 1
fi

# Deploying a pre-isolation build over a post-isolation one IS a rollback past
# the boundary, whatever it is called. Refuse it before anything changes -- once
# a record has been migrated.
if is_post_isolation "$CUR" && ! is_post_isolation "$BIN" && [ "${ALLOW_PRE_ISOLATION_ROLLBACK:-0}" != 1 ]; then
    if HAZARD=$(migration_hazard); then
        fail "refusing to replace a post-isolation build with a pre-isolation one: $HAZARD. It would re-export a pool chain into ~/.codex/auth.json, OpenCode and pi, and strip oauthCredentialOrigin from every Codex record it refreshes (upstream docs/upgrades.md rollback boundary). Nothing was changed. Set ALLOW_PRE_ISOLATION_ROLLBACK=1 to do it anyway."
        exit 1
    fi
    echo "   replacing a post-isolation build with a pre-isolation one: allowed, $HAZARD"
fi

# Before anything changes, and after the downgrade refusal (which needs no
# launchd): this shell must be able to restart the job it is about to replace,
# or, for a supervised upgrade, at least see what launchd runs.
if LAUNCHD_WHY=$(launchd_blocker "$LAUNCHD_NEEDS"); then
    fail "$LAUNCHD_WHY. Nothing was changed. $LAUNCHD_FIX"
    exit 1
fi
if [ -z "$DETACHED" ]; then
    run_detached "$SELF" "$@"
    exit $?
fi

restart() {
    # kickstart is the cheap path. It cannot clear launchd's cached code
    # requirement for the service ("managed LWCR"), so a binary rejected with
    # OS_REASON_CODESIGNING needs a full bootout/bootstrap -- as does any plist
    # edit, which kickstart does not re-read.
    local before out rc up=120
    LAUNCHD_ERR=""
    before=$(listener_pid)
    if [ "${RELOAD:-0}" != 1 ]; then
        kickstart_job; rc=$?; out=$KICKSTART_OUT
        if [ "$rc" -ne 0 ]; then
            echo "   kickstart failed (rc $rc${out:+: $out}); reloading the job"
        else
            for _ in $(seq 1 30); do came_back "$before" && return 0; sleep 1; done
            if [ -n "$before" ] && healthy && [ "$(listener_pid)" = "$before" ]; then
                echo "   kickstart returned, but pid $before is still the one serving; reloading the job"
            else
                local reason; reason=$(launchctl print "$LABEL" 2>/dev/null | grep -o 'last exit reason = .*' | head -1)
                echo "   kickstart did not come up${reason:+ ($reason)}; reloading the job"
            fi
        fi
    fi
    bootout_and_wait "$EXIT_TIMEOUT" || return 1
    bootstrap_job || return 1
    [ "$SUPERVISED" = 1 ] && [ $((READY_TIMEOUT + 60)) -gt $up ] && up=$((READY_TIMEOUT + 60))
    for _ in $(seq 1 $((up / 2))); do came_back "$before" && return 0; sleep 2; done
    if [ -n "$before" ] && healthy && [ "$(listener_pid)" = "$before" ]; then
        echo "   pid $before is still the one serving: the job's new process never took the port"
    fi
    return 1
}

# After a restart launchctl itself failed: what it said, and the one command
# that loads the job again from a shell that can.
launchctl_hint() {
    [ -n "$LAUNCHD_ERR" ] || return 0
    echo "   launchctl failed: $LAUNCHD_ERR" >&2
    echo "   If it refused (rc 141, 153 or 125), this shell is the problem, not the build. From a new Terminal.app window:" >&2
    echo "     launchctl bootstrap gui/$(id -u) $PLIST" >&2
}

restore_backups() {
    install_binary "$BACKUP" "$CUR" || return 1
    [ -z "$SUP_BACKUP" ] || install_binary "$SUP_BACKUP" "$SUPERVISOR_BIN" || return 1
    [ -z "$PLIST_BACKUP" ] || { cp -p "$PLIST_BACKUP" "$PLIST.restore.$$" && mv -f "$PLIST.restore.$$" "$PLIST"; }
}

# UNDO_ON_REFUSAL from step 4 on (own_job_or_exit, PATCHES §76.6): launchd's
# job changed during the run, the restart was refused, and the files go back
# to what they were before this run. Each file is tried on its own, and one
# that stays as this run left it is named with the command that puts it back.
put_back_on_refusal() {
    local left=0
    install_binary "$BACKUP" "$CUR" || { still_new "$BACKUP" "$CUR"; left=1; }
    if [ -n "$SUP_BACKUP" ]; then
        install_binary "$SUP_BACKUP" "$SUPERVISOR_BIN" || { still_new "$SUP_BACKUP" "$SUPERVISOR_BIN"; left=1; }
    fi
    if [ -n "$PLIST_BACKUP" ]; then
        { cp -p "$PLIST_BACKUP" "$PLIST.restore.$$" && mv -f "$PLIST.restore.$$" "$PLIST"; } || { still_new "$PLIST_BACKUP" "$PLIST"; left=1; }
    fi
    [ "$left" = 1 ] || echo "   the files this run changed are as they were before it: $CUR is $BACKUP again" >&2
}
still_new() { # backup file
    echo "   !! $2 still holds what this run installed, and the next start of $LABEL would use it. To put it back:" >&2
    printf '     cp -p %q %q && mv -f %q %q\n' "$1" "$2.restore" "$2.restore" "$2" >&2
}

if [ "$SUPERVISED" = 1 ] && [ "$LOADED_SHAPE" = none ]; then
    echo "== 0. launchd has not loaded $LABEL: bootstrapping $PLIST =="
    bootstrap_job || { fail "launchd did not load $PLIST; the daemon is DOWN"; exit 2; }
    await $((READY_TIMEOUT + 60)) "the daemon to answer on $ADDR" healthy \
        || { fail "the daemon loaded from $PLIST does not answer on $ADDR; it is DOWN"; exit 2; }
    echo "   loaded, and answering on $ADDR"
fi

echo "== 1. before =="
echo "   healthy=$(healthy && echo yes || echo no)  sha=$(shasum -a256 "$CUR" 2>/dev/null | cut -c1-12)  new=$(shasum -a256 "$BIN" | cut -c1-12)"
[ "$SUPERVISED" = 1 ] && echo "   supervised: worker $CUR, control socket $CONTROL_SOCKET"
take_baseline

echo "== 2. back up the running binary =="
cp -p "$CUR" "$BACKUP" || { fail "backup failed"; exit 1; }
echo "   backup: $BACKUP"
if [ "$MODE" = --restart-supervisor ]; then
    SUP_BACKUP="$SUPERVISOR_BIN.bak-$STAMP"
    cp -p "$SUPERVISOR_BIN" "$SUP_BACKUP" || { fail "supervisor backup failed"; exit 1; }
    echo "   backup: $SUP_BACKUP"
fi
if [ -n "$PLIST_SET_ENV$PLIST_DELETE_ENV" ]; then
    PLIST_BACKUP="$PLIST.bak-$STAMP"
    cp -p "$PLIST" "$PLIST_BACKUP" || { fail "plist backup failed"; exit 1; }
    echo "   backup: $PLIST_BACKUP"
fi

echo "== 3. install (stage, sign, rename) =="
install_binary "$BIN" "$CUR" || { fail "install failed; the running binary is untouched"; exit 1; }
echo "   installed: $(shasum -a256 "$CUR" | cut -c1-12)"
if [ -n "$SUP_BACKUP" ]; then
    install_binary "$BIN" "$SUPERVISOR_BIN" || {
        fail "supervisor install failed; nothing was restarted"
        install_binary "$BACKUP" "$CUR"
        exit 1
    }
    echo "   installed: $SUPERVISOR_BIN"
fi
if [ -n "$PLIST_BACKUP" ]; then
    edit_plist_env || { fail "could not edit $PLIST; nothing was restarted"; restore_backups; exit 1; }
    echo "   $PLIST: ${PLIST_SET_ENV:-without $PLIST_DELETE_ENV}"
    job_state
fi
# shellcheck disable=SC2034 # read by own_job_or_exit
UNDO_ON_REFUSAL=put_back_on_refusal

if [ "$SWAP" = upgrade ]; then
    echo "== 4. upgrade the worker (POST /_subrouter/upgrade) =="
    echo "   the supervisor answers once the new worker is ready, within ${READY_TIMEOUT}s"
    upgrade "$BACKUP"
    case $? in
        0) echo "   switched; connections already open stay on the old worker until they finish" ;;
        1) fail "the supervisor kept the old worker; nothing a client sees changed"
           install_binary "$BACKUP" "$CUR" && echo "   $CUR is $BACKUP again"
           exit 1 ;;
        *) fail "supervisor-status names neither build as the active worker, so which one serves is unknown. $CUR goes back to $BACKUP, a build the supervisor can start; read supervisor-status"
           install_binary "$BACKUP" "$CUR" && echo "   $CUR is $BACKUP again"
           exit 1 ;;
    esac
elif [ "$MODE" = --restart-supervisor ]; then
    echo "== 4. restart the supervisor =="
    echo "   !! the port closes now and stays closed until the new supervisor's first worker is ready;"
    echo "      launchd gives open streams up to ExitTimeOut (${EXIT_TIMEOUT:-unset}s) to finish"
else
    echo "== 4. restart =="
fi
if [ "$SWAP" = restart ] && ! restart; then
    fail "daemon did not come back"
    # Roll back ONLY to the backup this run took -- i.e. the binary that was
    # serving sixty seconds ago. KNOWN-ISSUES.md ("Rolling back the binary is
    # more dangerous than rolling forward") warns against restoring OLDER
    # releases: a build without the keychain payload compaction (PATCHES.md §7)
    # re-breaks every pooled Claude credential on its next refresh, and a build
    # without the SaveConfig no-mint fix (PATCHES.md §5) can re-mint
    # localProxyToken out from under every
    # running client. Undoing the swap you just made is not that -- unless the
    # swap crossed the credential-isolation boundary (rollback_refused).
    if rollback_refused "daemon did not come back"; then
        echo "   The daemon is DOWN on the new build." >&2
        launchctl_hint
        exit 2
    fi
    echo "!! ROLLING BACK to $BACKUP"
    restore_backups && RELOAD=1 restart \
        && { echo "   rolled back, healthy"; exit 1; }
    echo "   STILL DOWN. Do NOT reach for an older binary; capture the log and fix forward:" >&2
    echo "     tail -50 ~/Library/Logs/subrouter.err.log" >&2
    echo "     launchctl print $LABEL | grep -E 'last exit|runs ='" >&2
    launchctl_hint
    exit 2
fi
[ "$SWAP" = restart ] && echo "   healthy"

echo "== 5. verify the routes, not just liveness =="
if ! verify_routes "$BASE_PROVIDER" "$BASE_IDENTITY" "$BASE_ACCOUNTS" "$BASE_SOCKET"; then
    fail "verification regressed on the new build"
    rollback_refused "verification regressed" && exit 1
    echo "!! ROLLING BACK to $BACKUP"
    if [ "$SWAP" = upgrade ]; then
        if install_binary "$BACKUP" "$CUR" && upgrade "$BIN" && serving "$CUR" && healthy; then
            echo "   rolled back, healthy"
        else
            echo "   !! the rollback did not verify: $CUR is $BACKUP again, but read supervisor-status for what serves" >&2
        fi
    else
        restore_backups && RELOAD=1 restart && echo "   rolled back, healthy"
    fi
    exit 1
fi

# Nothing from here on rolls back: the routes are good, and what is left is the
# Codex credential state, which the old binary cannot fix.
echo "== 6. Codex credential isolation =="
ISOLATION_INCOMPLETE=0
isolation_gate || ISOLATION_INCOMPLETE=1

# The new build is kept from here on, INCOMPLETE or not.
echo "== 7. old backups and the ops tools =="
prune_backups
name_pre_isolation_binaries
refresh_ops_tools

if [ "$ISOLATION_INCOMPLETE" = 1 ]; then
    echo
    echo "DEPLOY INCOMPLETE  sha=$(shasum -a256 "$CUR" | cut -c1-12)  backup=$BACKUP"
    exit 3
fi
if [ "$SOCKET_INCOMPLETE" = 1 ]; then
    echo
    echo "DEPLOY INCOMPLETE (claude socket not served)  sha=$(shasum -a256 "$CUR" | cut -c1-12)  backup=$BACKUP"
    exit 3
fi

echo
echo "DEPLOY OK  sha=$(shasum -a256 "$CUR" | cut -c1-12)  backup=$BACKUP"
