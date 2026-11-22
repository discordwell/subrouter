#!/bin/bash
# install-watchdog.sh: runs subrouter-watchdog.sh as a launchd job under its
# own label, ai.manaflow.subrouter-watchdog (ops/PATCHES.md §81).
#
#   install-watchdog.sh install [--watch LABEL=PLIST]... [--interval S] [--dir DIR] [--dry-run]
#   install-watchdog.sh uninstall [--dir DIR] [--dry-run]
#   install-watchdog.sh status [--dir DIR]
#
# install copies the watchdog to DIR (~/.subrouter/watchdog), writes
# ~/Library/LaunchAgents/ai.manaflow.subrouter-watchdog.plist (KeepAlive,
# RunAtLoad) and loads it; run again, it loads the new copy. The watchdog
# watches the daemon's job, and each --watch job as well. uninstall boots the
# watchdog out and removes its plist and its copy; the log stays. --dry-run
# prints the plist and every command, and changes nothing.
#
# A rehearsal uses a label that starts with test.subrouter. and names:
#   --label LABEL --dir DIR --no-daemon [--watch LABEL=PLIST]...
#   [--launch-agents DIR] [--launchctl PATH]
# Its plist, copy, log and stop markers all go in DIR (the plist in
# --launch-agents when given), it watches only scratch labels, and --launchctl
# names the launchctl the watchdog job runs.
#
# This script calls launchctl (SUBROUTER_WATCHDOG_LAUNCHCTL, /bin/launchctl,
# never from PATH) only for the watchdog's own label, and never changes the
# daemon's job or its plist.
set -u

HERE=$(cd "$(dirname "$0")" && pwd -P)
SOURCE="$HERE/subrouter-watchdog.sh"
LAUNCHCTL="${SUBROUTER_WATCHDOG_LAUNCHCTL:-/bin/launchctl}"
REAL_LABEL=ai.manaflow.subrouter-watchdog
DAEMON_LABEL=ai.manaflow.subrouter
SCRATCH=test.subrouter.

die() { echo "install-watchdog: $*" >&2; exit 2; }
[ -n "${HOME:-}" ] || die "HOME is not set"
case "$LAUNCHCTL" in /*) ;; *) die "refusing '$LAUNCHCTL': launchctl is run by absolute path, never looked up on PATH" ;; esac
valid_label() { case "$1" in ""|*[!A-Za-z0-9._-]*) return 1 ;; esac; }
# Every value goes into the plist as written, so none may need escaping.
plain() { case "$1" in *[\&\<\>\"\']*) die "refusing '$1': the plist writer does not escape & < > \" or '" ;; esac; }

VERB="${1:-}"
[ $# -eq 0 ] || shift
LABEL=$REAL_LABEL DIR="" AGENTS="" JOB_LAUNCHCTL="" INTERVAL="" DRY=0 NO_DAEMON=0 WATCH=()
while [ $# -gt 0 ]; do
    case "$1" in
        --label) LABEL="${2:-}"; shift 2 || die "--label takes a label" ;;
        --dir) DIR="${2:-}"; shift 2 || die "--dir takes a directory" ;;
        --launch-agents) AGENTS="${2:-}"; shift 2 || die "--launch-agents takes a directory" ;;
        --launchctl) JOB_LAUNCHCTL="${2:-}"; shift 2 || die "--launchctl takes a path" ;;
        --interval) INTERVAL="${2:-}"; shift 2 || die "--interval takes seconds" ;;
        --watch) [ $# -ge 2 ] || die "--watch takes LABEL=PLIST"; WATCH+=("$2"); shift 2 ;;
        --no-daemon) NO_DAEMON=1; shift ;;
        --dry-run) DRY=1; shift ;;
        *) die "unknown option $1" ;;
    esac
done
case "$VERB" in install|uninstall|status) ;; *) die "usage: install-watchdog.sh install|uninstall|status [options]" ;; esac

case "$LABEL" in
    "$DAEMON_LABEL") die "refusing the label $LABEL: that is the daemon's own job, and this script never changes it" ;;
    "$REAL_LABEL") SCRATCH_RUN=0 ;;
    "$SCRATCH"*) valid_label "$LABEL" || die "refusing '$LABEL': not a launchd label"; SCRATCH_RUN=1 ;;
    *) die "refusing the label $LABEL: this installs only $REAL_LABEL, or a label that starts with $SCRATCH for a rehearsal" ;;
esac
if [ "$SCRATCH_RUN" = 1 ]; then
    [ -n "$DIR" ] || die "a scratch watchdog needs --dir, where its plist, copy, log and stop markers go"
    [ "$NO_DAEMON" = 1 ] || [ "$VERB" != install ] || die "a scratch watchdog must not watch the daemon's job: add --no-daemon"
    AGENTS="${AGENTS:-$DIR}"
    LOG="$DIR/watchdog.log" STOPS="$DIR"
else
    [ -z "$AGENTS" ] || die "--launch-agents is for a scratch label: launchd loads $REAL_LABEL at login only from ~/Library/LaunchAgents"
    [ -z "$JOB_LAUNCHCTL" ] || die "--launchctl is for a scratch label: $REAL_LABEL runs /bin/launchctl"
    [ "$NO_DAEMON" = 0 ] || die "--no-daemon is for a scratch label: $REAL_LABEL watches the daemon's job"
    DIR="${DIR:-$HOME/.subrouter/watchdog}" AGENTS="$HOME/Library/LaunchAgents"
    LOG="$HOME/Library/Logs/subrouter-watchdog.log" STOPS="$HOME/.subrouter"
fi
for p in "$DIR" "$AGENTS"; do case "$p" in /*) plain "$p" ;; *) die "'$p' must be an absolute path" ;; esac; done
case "$JOB_LAUNCHCTL" in ""|/*) plain "$JOB_LAUNCHCTL" ;; *) die "--launchctl must be an absolute path" ;; esac
case "$INTERVAL" in ""|[1-9]|[1-9][0-9]|[1-9][0-9][0-9]) ;; *) die "--interval must be 1 to 999 seconds" ;; esac
PLIST="$AGENTS/$LABEL.plist"
SCRIPT="$DIR/subrouter-watchdog.sh"
DOMAIN="gui/$(id -u)"
for pair in ${WATCH[@]+"${WATCH[@]}"}; do
    label=${pair%%=*} plist=${pair#*=}
    [ "$label" != "$pair" ] || die "--watch takes LABEL=PLIST, not '$pair'"
    valid_label "$label" || die "'$label' is not a launchd label"
    case "$plist" in /*) plain "$plist" ;; *) die "the plist of $label must be an absolute path" ;; esac
    [ "$label" != "$LABEL" ] || die "the watchdog cannot watch its own label"
    if [ "$SCRATCH_RUN" = 1 ]; then
        case "$label" in "$SCRATCH"*) ;; *) die "a scratch watchdog watches only labels that start with $SCRATCH, not $label" ;; esac
    fi
done

ARGS=(/bin/bash "$SCRIPT" run)
[ "$NO_DAEMON" = 0 ] || ARGS+=(--no-daemon)
for pair in ${WATCH[@]+"${WATCH[@]}"}; do ARGS+=(--watch "$pair"); done
[ -z "$INTERVAL" ] || ARGS+=(--interval "$INTERVAL")
ARGS+=(--log "$LOG" --stops "$STOPS")

plist_xml() {
    local a
    printf '<?xml version="1.0" encoding="UTF-8"?>\n'
    printf '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">\n'
    printf '<plist version="1.0">\n<dict>\n'
    printf '\t<key>Label</key>\n\t<string>%s</string>\n' "$LABEL"
    printf '\t<key>ProgramArguments</key>\n\t<array>\n'
    for a in "${ARGS[@]}"; do printf '\t\t<string>%s</string>\n' "$a"; done
    printf '\t</array>\n'
    printf '\t<key>EnvironmentVariables</key>\n\t<dict>\n'
    printf '\t\t<key>PATH</key>\n\t\t<string>/usr/bin:/bin:/usr/sbin:/sbin</string>\n'
    [ -z "$JOB_LAUNCHCTL" ] || printf '\t\t<key>SUBROUTER_WATCHDOG_LAUNCHCTL</key>\n\t\t<string>%s</string>\n' "$JOB_LAUNCHCTL"
    printf '\t</dict>\n'
    printf '\t<key>KeepAlive</key>\n\t<true/>\n'
    printf '\t<key>RunAtLoad</key>\n\t<true/>\n'
    printf '\t<key>StandardOutPath</key>\n\t<string>/dev/null</string>\n'
    printf '\t<key>StandardErrorPath</key>\n\t<string>%s</string>\n' "${LOG%.log}.err.log"
    printf '</dict>\n</plist>\n'
}

# loaded:<plist> | none | rc:<rc> <message>
job_state() {
    local out rc from
    out=$("$LAUNCHCTL" print "$DOMAIN/$LABEL" 2>&1); rc=$?
    case "$rc" in
        0) from=$(printf '%s\n' "$out" | sed -n 's/^	path = //p' | head -1); echo "loaded:$from" ;;
        113) echo none ;;
        *) echo "rc:$rc $(printf '%s\n' "$out" | head -1)" ;;
    esac
}

# Refuses, before anything changes, a watchdog job that launchd loaded from
# another plist, or a launchd that does not answer.
STATE=$(job_state)
case "$STATE" in
    loaded:*)
        FROM=${STATE#loaded:}
        if [ "$VERB" != status ] && [ "$FROM" != "$PLIST" ] && ! [ "$FROM" -ef "$PLIST" ]; then
            die "refusing: launchd loaded $LABEL from ${FROM:-a plist it does not name}, not from $PLIST. launchd was not changed."
        fi ;;
    none) ;;
    *) [ "$VERB" = status ] || die "launchctl print $DOMAIN/$LABEL answered ${STATE#rc:}; only 0 and 113 are answers. Nothing was changed." ;;
esac

act() { # command...: runs it, or under --dry-run prints it
    if [ "$DRY" = 1 ]; then echo "would run: $*"; return 0; fi
    "$@"
}

bootout_own() {
    local n=0
    act "$LAUNCHCTL" bootout "$DOMAIN/$LABEL" || die "launchctl bootout $DOMAIN/$LABEL failed"
    [ "$DRY" = 0 ] || return 0
    until [ "$(job_state)" = none ]; do
        n=$((n + 1))
        [ $n -le 30 ] || die "launchd still holds $LABEL 30 s after the bootout"
        sleep 1
    done
}

case "$VERB" in
install)
    [ -f "$SOURCE" ] || die "cannot find $SOURCE"
    if [ "$DRY" = 1 ]; then
        echo "would copy $SOURCE to $SCRIPT"
        echo "would write $PLIST:"
        plist_xml | sed 's/^/    /'
    else
        mkdir -p "$DIR" "$AGENTS" "$(dirname "$LOG")" || die "cannot create $DIR, $AGENTS or $(dirname "$LOG")"
        # A new file, renamed into place: the running watchdog keeps reading
        # the copy it started from.
        { cp "$SOURCE" "$SCRIPT.new.$$" && chmod 755 "$SCRIPT.new.$$" && mv -f "$SCRIPT.new.$$" "$SCRIPT"; } ||
            { rm -f "$SCRIPT.new.$$"; die "cannot copy the watchdog to $SCRIPT"; }
        plist_xml > "$PLIST.new.$$" || { rm -f "$PLIST.new.$$"; die "cannot write $PLIST"; }
        if [ -x /usr/bin/plutil ] && ! /usr/bin/plutil -lint "$PLIST.new.$$" > /dev/null; then
            rm -f "$PLIST.new.$$"; die "the plist written for $LABEL does not lint; nothing was loaded"
        fi
        mv -f "$PLIST.new.$$" "$PLIST" || die "cannot write $PLIST"
    fi
    case "$STATE" in loaded:*) bootout_own ;; esac
    act "$LAUNCHCTL" bootstrap "$DOMAIN" "$PLIST" || die "launchctl bootstrap $DOMAIN $PLIST failed; the files are in place, so it can be run again by hand"
    if [ "$DRY" = 0 ]; then
        STATE=$(job_state)
        [ "$STATE" = "loaded:$PLIST" ] || die "launchd answers '$STATE' for $LABEL after the bootstrap, not loaded:$PLIST"
        echo "installed: $DOMAIN/$LABEL runs $SCRIPT; its log is $LOG"
    fi ;;
uninstall)
    case "$STATE" in loaded:*) bootout_own ;; esac
    act rm -f "$PLIST" "$SCRIPT"
    [ "$DRY" = 1 ] || { rmdir "$DIR" 2>/dev/null; echo "uninstalled: $DOMAIN/$LABEL; its log stays at $LOG"; } ;;
status)
    case "$STATE" in
        loaded:*) echo "$DOMAIN/$LABEL: loaded from ${STATE#loaded:}, pid $("$LAUNCHCTL" print "$DOMAIN/$LABEL" 2>/dev/null | sed -n 's/^	pid = //p' | head -1)" ;;
        none) echo "$DOMAIN/$LABEL: not loaded" ;;
        *) echo "$DOMAIN/$LABEL: launchctl print answered ${STATE#rc:}" ;;
    esac
    if [ -f "$PLIST" ]; then
        echo "plist $PLIST runs:"
        sed -n '/<array>/,/<\/array>/s:^[[:space:]]*<string>\(.*\)</string>$:    \1:p' "$PLIST"
    else
        echo "plist $PLIST: missing"
    fi
    now=$(date +%s)
    for f in "$STOPS"/watchdog-stop.*; do
        [ -e "$f" ] || continue
        until=$(sed -n 's/^until=//p' "$f" | head -1)
        case "$until" in ""|*[!0-9]*) left="no expiry time" ;;
            *) if [ "$until" -gt "$now" ]; then left="$((until - now)) s left"; else left="expired"; fi ;;
        esac
        echo "stop marker $f: $left, $(sed -n 's/^by=//p' "$f" | head -1)"
    done
    if [ -f "$LOG" ]; then echo "last events in $LOG:"; tail -10 "$LOG" | sed 's/^/    /'; else echo "log $LOG: none yet"; fi ;;
esac
