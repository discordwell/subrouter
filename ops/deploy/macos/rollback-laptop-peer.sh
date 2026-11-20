#!/usr/bin/env bash
# ONE-COMMAND ROLLBACK of the laptop peer cutover (2026-08-16).
#
# Stops the daemon, restores the pre-cutover binary and plist, and starts it
# again the only way that works: waiting on the launchd JOB, never the port.
#
#   ops/deploy/macos/rollback-laptop-peer.sh [--restart-supervisor]
#
# Paths (env): SUBROUTER_LABEL (ai.manaflow.subrouter), SUBROUTER_PLIST,
# SUBROUTER_ADDR (127.0.0.1:31415). It restores the binary the job runs: a
# plain serve's program, or a supervised job's --worker-bin.
#
# The port frees ~1s after SIGTERM while the process keeps serving established
# connections for the whole drain (ceiling 10m). `lsof -sTCP:LISTEN` going quiet
# is true and means nothing; bootstrapping into a domain that still holds the
# label returns launchd's unhelpful `5: Input/output error`. And only rc 113 from
# `launchctl print` says the job is gone; see launchd_blocker in
# laptop-deploy-lib.sh.
set -uo pipefail

SELF=$(python3 -c 'import os, sys; print(os.path.realpath(sys.argv[1]))' "$0")
# shellcheck source=laptop-deploy-lib.sh
. "$(dirname "$SELF")/laptop-deploy-lib.sh" || exit 1
DETACHED="${SUBROUTER_LAPTOP_DETACHED:-}"
unset SUBROUTER_LAPTOP_DETACHED

BK="$(cat "${LAPTOP_BACKUP_PTR:-/tmp/laptop-backup-dir}" 2>/dev/null || true)"
if [ -z "$BK" ] || [ ! -d "$BK" ]; then
  BK="$(ls -1d "$HOME"/.subrouter/laptop-cutover-backup-* 2>/dev/null | tail -1)"
fi
[ -d "$BK" ] || { echo "no backup dir found" >&2; exit 1; }
echo "restoring from: $BK"

JOB="${SUBROUTER_LABEL:-ai.manaflow.subrouter}"
PLIST="${SUBROUTER_PLIST:-$HOME/Library/LaunchAgents/$JOB.plist}"
ADDR="${SUBROUTER_ADDR:-127.0.0.1:31415}"
LABEL="gui/$(id -u)/$JOB"

take_mutation_lock || exit 1
job_state
if CONFLICT=$(shape_conflict); then
  echo "refusing: $CONFLICT. Nothing was restored." >&2
  exit 1
fi
CUR="$JOB_BINARY"
if [ -z "$CUR" ]; then
  echo "refusing: cannot tell which binary $LABEL runs: $PLIST runs neither '<binary> serve' nor supervise with a --worker-bin. Nothing was restored." >&2
  exit 1
fi
SHAPE="$LOADED_SHAPE"
case "$SHAPE" in none|unknown) SHAPE="$PLIST_SHAPE" ;; esac
if [ "$SHAPE" = supervised ] && [ "${1:-}" != --restart-supervisor ]; then
  echo "refusing: $LABEL runs subrouter supervise. Restoring $BK/plist.bak over it" >&2
  echo "restarts the supervisor, which reads its plist and SUBROUTER_CLAUDE_SOCKET only when" >&2
  echo "it starts: the port closes until the restored daemon is up, and streams in flight get" >&2
  echo "at most ExitTimeOut (${EXIT_TIMEOUT:-unset}s). Nothing was restored. To undo a supervisor" >&2
  echo "activation: activate-laptop-supervisor.sh --rollback. To restore this backup anyway:" >&2
  echo "  $0 --restart-supervisor" >&2
  exit 1
fi

# The Codex credential-isolation rollback boundary (upstream docs/upgrades.md).
# That backup predates the 2026-09 upstream merge. Once a post-isolation build
# has MIGRATED a Codex record (stamped it isolated-server-login or
# server-attested-transfer), restoring the backup re-exports that pool chain
# into ~/.codex/auth.json, OpenCode and pi, and every refresh it makes strips the
# oauthCredentialOrigin the new build requires. Until then it serves the store
# exactly as before, so the restore is refused only on that evidence -- or when
# the store exists and cannot be read. Marker: every post-isolation build
# carries attest-legacy.
if LC_ALL=C grep -aq attest-legacy "$CUR" 2>/dev/null &&
  ! LC_ALL=C grep -aq attest-legacy "$BK/subrouter.bak" 2>/dev/null &&
  [ "${ALLOW_PRE_ISOLATION_ROLLBACK:-0}" != 1 ]; then
  # The store the daemon serves: <state>/codex/accounts, <state> being the
  # plist's SUBROUTER_STATE_DIR, else its HOME/.subrouter.
  STORE="$(python3 - "$PLIST" "$HOME" <<'PY' 2>/dev/null
import os, plistlib, sys
try:
    with open(sys.argv[1], "rb") as f:
        env = plistlib.load(f).get("EnvironmentVariables") or {}
except Exception:
    env = {}
print(os.path.join(env.get("SUBROUTER_STATE_DIR") or os.path.join(env.get("HOME") or sys.argv[2], ".subrouter"), "codex", "accounts"))
PY
)"
  STORE="${STORE:-$HOME/.subrouter/codex/accounts}"
  # Count the migrated records; WHY stays empty only when there are none and the
  # store could be read (missing counts as none only if provably absent).
  WHY=""
  if [ ! -e "$STORE" ]; then
    p="$STORE"
    while [ ! -e "$p" ]; do q=$(dirname "$p"); [ "$q" = "$p" ] && break; p=$q; done
    { [ -d "$p" ] && [ -x "$p" ]; } || WHY="cannot tell whether $STORE exists ($p cannot be searched)"
  elif [ ! -d "$STORE" ] || [ ! -r "$STORE" ] || [ ! -x "$STORE" ]; then
    WHY="the Codex store $STORE exists but cannot be read"
  else
    n=0
    for f in "$STORE"/*.json; do
      [ -e "$f" ] || [ -L "$f" ] || continue
      [ -d "$f" ] && continue
      LC_ALL=C grep -Eq '"oauthCredentialOrigin"[[:space:]]*:[[:space:]]*"(isolated-server-login|server-attested-transfer)"' "$f" 2>/dev/null
      case $? in 0) n=$((n + 1)) ;; 1) ;; *) WHY="$f cannot be read"; break ;; esac
    done
    [ -z "$WHY" ] && [ "$n" -gt 0 ] &&
      WHY="$n Codex record(s) in $STORE carry oauthCredentialOrigin isolated-server-login or server-attested-transfer"
  fi
  if [ -n "$WHY" ]; then
    echo "refusing: $BK/subrouter.bak predates Codex credential isolation and $CUR" >&2
    echo "does not, and $WHY." >&2
    echo "Fix forward (ops/deploy/macos/deploy-laptop.sh), or set" >&2
    echo "ALLOW_PRE_ISOLATION_ROLLBACK=1 if the outage is worse than re-migrating those records." >&2
    exit 1
  fi
  echo "$BK/subrouter.bak predates Codex credential isolation, but no Codex record in $STORE was migrated; restoring it is safe"
fi

if LAUNCHD_WHY=$(launchd_blocker restart); then
  echo "refusing: $LAUNCHD_WHY. Nothing was restored. $LAUNCHD_FIX" >&2
  exit 1
fi
RESTORES=$(python3 -c 'import plistlib, sys; print(plistlib.load(open(sys.argv[1], "rb"))["ProgramArguments"][0])' "$BK/plist.bak" 2>/dev/null)
if [ "$RESTORES" != "$CUR" ]; then
  echo "refusing: $BK/plist.bak runs ${RESTORES:-no program it names}, but $LABEL runs $CUR, so the restored binary would not be the one launchd starts. Nothing was restored." >&2
  exit 1
fi
if [ -z "$DETACHED" ]; then
  run_detached "$SELF" "$@"
  exit $?
fi

# The process serving the port now. The rollback succeeds only when a different
# one answers health: the old process still answering is not a restart.
BEFORE=$(listener_pid)

if ! bootout_and_wait "$EXIT_TIMEOUT"; then
  echo "Nothing was restored, but the bootout may already have unloaded the job, so the daemon may be down. From $LAUNCHD_FROM, check the job:" >&2
  echo "  launchctl print $LABEL" >&2
  echo "If that cannot find the service (rc 113, not loaded), load it:" >&2
  echo "  launchctl bootstrap gui/$(id -u) $PLIST" >&2
  echo "If it is still loaded, rerun this script from there. $LAUNCHD_SUPERSET" >&2
  exit 1
fi

# Stage-and-rename, never cp onto the live inode: macOS SIGKILLs later execs off
# a clobbered path, which would also break this script's own second attempt.
if cp "$BK/subrouter.bak" "$CUR.restore" && chmod 755 "$CUR.restore" && mv "$CUR.restore" "$CUR" \
  && cp "$BK/plist.bak" "$PLIST"; then
  echo "binary + plist restored"
else
  rm -f "$CUR.restore"
  echo "could not restore $CUR and $PLIST from $BK; starting the job again on what is there" >&2
  bootstrap_job
  exit 1
fi

if ! bootstrap_job; then
  echo "If launchctl refused (rc 141, 153 or 125), this shell is the problem, not the backup. From a new Terminal.app window:" >&2
  echo "  launchctl bootstrap gui/$(id -u) $PLIST" >&2
  exit 1
fi

NOW=""
for _ in $(seq 1 60); do
  if healthy; then
    NOW=$(listener_pid)
    if [ -n "$NOW" ] && [ "$NOW" != "$BEFORE" ]; then
      echo "ROLLED BACK, daemon healthy"
      exit 0
    fi
  fi
  sleep 2
done
if [ -n "$BEFORE" ] && [ "$NOW" = "$BEFORE" ]; then
  echo "pid $BEFORE, the daemon from before the rollback, is still the one serving: the job's new process never took the port" >&2
fi
echo "daemon did not come back; check /tmp or the launchd log" >&2
exit 1
