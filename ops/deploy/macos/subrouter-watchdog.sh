#!/bin/bash
# subrouter-watchdog.sh: loads a watched launchd job again when something
# unloads it (ops/PATCHES.md §81). launchd forgets a job that is booted out,
# so KeepAlive has nothing left to restart: on 2026-11-21 a stray script booted
# the daemon's job out, and it stayed out for over two hours.
#
#   subrouter-watchdog.sh run [options]     check every few seconds until stopped
#   subrouter-watchdog.sh config [options]  print what run would watch
#   subrouter-watchdog.sh hold LABEL SECONDS [--stops DIR] [--by TEXT]
#   subrouter-watchdog.sh release LABEL [--stops DIR]
#
# run and config take:
#   --watch LABEL=PLIST  watch this job as well (repeatable)
#   --no-daemon          leave out the daemon's own job, ai.manaflow.subrouter
#                        from ~/Library/LaunchAgents/ai.manaflow.subrouter.plist
#   --interval SECONDS   between checks (5)
#   --log FILE           events, never checks (~/Library/Logs/subrouter-watchdog.log)
#   --stops DIR          where the stop markers are (~/.subrouter)
#   --checks N           stop after N checks (0, the default: run until stopped)
#
# Each check, for each watched job:
#   loaded from its plist        nothing, however quiet the job is
#   loaded from another plist    logged and notified once; the job is not changed
#   not loaded, stop announced   nothing until the stop marker expires
#   not loaded, plist missing    logged and notified once; nothing else
#   not loaded                   bootstrapped from its plist into gui/<uid> and
#                                notified once; a bootstrap that fails is tried
#                                again after 10, 20, 40 ... up to 300 seconds
#   any other answer from print  logged and notified once; nothing else
#
# A stop marker is <stops>/watchdog-stop.<label>, with the lines label=<label>,
# until=<epoch seconds> and by=<who>. `hold` writes one and `release` removes
# it. A marker that has expired, cannot be read, names another label or names a
# time more than a day ahead is ignored and logged once, so a forgotten marker
# cannot switch the watchdog off.
#
# launchctl and osascript are run by absolute path from
# SUBROUTER_WATCHDOG_LAUNCHCTL (/bin/launchctl) and SUBROUTER_WATCHDOG_OSASCRIPT
# (/usr/bin/osascript), never looked up on PATH. Runs under macOS /bin/bash 3.2.
set -u

LAUNCHCTL="${SUBROUTER_WATCHDOG_LAUNCHCTL:-/bin/launchctl}"
OSASCRIPT="${SUBROUTER_WATCHDOG_OSASCRIPT:-/usr/bin/osascript}"
DAEMON_LABEL=ai.manaflow.subrouter
MAX_HOLD=86400
MAX_PAUSE=300
LOG_CAP=262144

die() { echo "subrouter-watchdog: $*" >&2; exit 2; }
[ -n "${HOME:-}" ] || die "HOME is not set"
DEFAULT_STOPS="$HOME/.subrouter"

valid_label() { case "$1" in ""|*[!A-Za-z0-9._-]*) return 1 ;; esac; }
now() { date +%s; }
when() { date -r "$1" '+%Y-%m-%d %H:%M:%S' 2>/dev/null || date -d "@$1" '+%Y-%m-%d %H:%M:%S' 2>/dev/null || echo "$1"; }

# --- stop markers --------------------------------------------------------------

cmd_hold() {
    local label="${1:-}" secs="${2:-}" stops="$DEFAULT_STOPS" by="" f until
    [ $# -ge 2 ] || die "usage: hold LABEL SECONDS [--stops DIR] [--by TEXT]"
    shift 2
    while [ $# -gt 0 ]; do
        case "$1" in
            --stops) stops="${2:?--stops takes a directory}"; shift 2 ;;
            --by) by="${2:?--by takes a text}"; shift 2 ;;
            *) die "hold: unknown option $1" ;;
        esac
    done
    valid_label "$label" || die "hold: '$label' is not a launchd label"
    case "$secs" in ""|*[!0-9]*) die "hold: SECONDS must be a whole number, not '$secs'" ;; esac
    secs=$((10#$secs))
    { [ "$secs" -ge 1 ] && [ "$secs" -le $MAX_HOLD ]; } ||
        die "hold: SECONDS must be 1 to $MAX_HOLD; a longer stop cannot be announced, so a forgotten marker cannot switch the watchdog off"
    by=$(printf '%s' "${by:-by hand}" | tr '\n' ' ')
    mkdir -p "$stops" || die "hold: cannot create $stops"
    until=$(( $(now) + secs ))
    f="$stops/watchdog-stop.$label"
    { printf 'label=%s\nuntil=%s\nby=%s\n' "$label" "$until" "$by" > "$f.$$" && mv -f "$f.$$" "$f"; } ||
        { rm -f "$f.$$"; die "hold: cannot write $f"; }
    echo "the watchdog leaves $label alone until $(when "$until") ($f)"
}

cmd_release() {
    local label="${1:-}" stops="$DEFAULT_STOPS"
    [ $# -ge 1 ] || die "usage: release LABEL [--stops DIR]"
    shift
    while [ $# -gt 0 ]; do
        case "$1" in
            --stops) stops="${2:?--stops takes a directory}"; shift 2 ;;
            *) die "release: unknown option $1" ;;
        esac
    done
    valid_label "$label" || die "release: '$label' is not a launchd label"
    rm -f "$stops/watchdog-stop.$label" || die "release: cannot remove $stops/watchdog-stop.$label"
}

# Sets MARK (none | held | expired | invalid), MARK_UNTIL and MARK_BY for job
# $1, and logs an expired or unusable marker the first time it sees it.
read_marker() {
    local i=$1 label=${W_LABEL[$1]} f content="" k v m_label="" m_until="" why="" t
    f="$STOPS/watchdog-stop.$label"
    MARK=none MARK_UNTIL="" MARK_BY=""
    if [ -e "$f" ] || [ -L "$f" ]; then
        if ! content=$(cat "$f" 2>/dev/null); then
            MARK=invalid why="cannot be read"
        else
            while IFS='=' read -r k v; do
                case "$k" in label) m_label=$v ;; until) m_until=$v ;; by) MARK_BY=$v ;; esac
            done <<EOF
$content
EOF
            t=$(now)
            case "$m_until" in ""|*[!0-9]*) m_until="" ;; esac
            if [ "$m_label" != "$label" ]; then
                MARK=invalid why="names the job '$m_label', not $label"
            elif [ -z "$m_until" ]; then
                MARK=invalid why="holds no expiry time"
            elif [ ${#m_until} -gt 12 ] || [ $((10#$m_until - t)) -gt $MAX_HOLD ]; then
                MARK=invalid why="names an expiry more than $MAX_HOLD s ahead"
            elif [ $((10#$m_until)) -le "$t" ]; then
                MARK=expired
            else
                MARK=held MARK_UNTIL=$((10#$m_until))
            fi
        fi
    fi
    if [ "${W_MARK[$i]}" != "$MARK $content" ]; then
        case "$MARK" in
            expired) log "$label: the stop announced by ${MARK_BY:-someone} ended at $(when "$m_until"); ignoring its marker $f" ;;
            invalid) log "$label: the stop marker $f $why; ignoring it" ;;
        esac
    fi
    W_MARK[$i]="$MARK $content"
}

# --- the watched jobs ----------------------------------------------------------
# One row per job: W_LABEL, W_PLIST, and W_STATE, the last thing observed:
#   ok | held:<until> | down | noplist | foreign:<path> | unknown:<rc>
# W_FAILS and W_NEXT pace the bootstraps of a job that is down, and W_MARK is
# the last marker logged.

parse_options() {
    local daemon=1 pair label plist extra="" i
    INTERVAL=5 LOG="$HOME/Library/Logs/subrouter-watchdog.log" STOPS="$DEFAULT_STOPS" CHECKS=0
    W_LABEL=() W_PLIST=() N=0
    while [ $# -gt 0 ]; do
        case "$1" in
            --watch) [ $# -ge 2 ] || die "--watch takes LABEL=PLIST"; extra="$extra
$2"; shift 2 ;;
            --no-daemon) daemon=0; shift ;;
            --interval) INTERVAL="${2:-}"; shift 2 || die "--interval takes seconds" ;;
            --log) LOG="${2:-}"; shift 2 || die "--log takes a file" ;;
            --stops) STOPS="${2:-}"; shift 2 || die "--stops takes a directory" ;;
            --checks) CHECKS="${2:-}"; shift 2 || die "--checks takes a count" ;;
            *) die "unknown option $1" ;;
        esac
    done
    case "$INTERVAL" in ""|0|*[!0-9]*) die "--interval must be a whole number of seconds above 0" ;; esac
    case "$CHECKS" in ""|*[!0-9]*) die "--checks must be a whole number" ;; esac
    case "$LOG" in /*) ;; *) die "--log must be an absolute path" ;; esac
    case "$STOPS" in /*) ;; *) die "--stops must be an absolute path" ;; esac
    [ "$daemon" = 0 ] || extra="$DAEMON_LABEL=$HOME/Library/LaunchAgents/$DAEMON_LABEL.plist$extra"
    while IFS= read -r pair; do
        [ -n "$pair" ] || continue
        label=${pair%%=*} plist=${pair#*=}
        [ "$label" != "$pair" ] || die "--watch takes LABEL=PLIST, not '$pair'"
        valid_label "$label" || die "'$label' is not a launchd label"
        case "$plist" in /*) ;; *) die "the plist of $label must be an absolute path, not '$plist'" ;; esac
        i=0
        while [ $i -lt $N ]; do
            [ "${W_LABEL[$i]}" != "$label" ] || die "$label is watched twice"
            i=$((i + 1))
        done
        W_LABEL[$N]=$label W_PLIST[$N]=$plist W_STATE[$N]="" W_FAILS[$N]=0 W_NEXT[$N]=0 W_MARK[$N]=""
        N=$((N + 1))
    done <<EOF
$extra
EOF
    [ $N -gt 0 ] || die "nothing to watch: --no-daemon and no --watch"
    for tool in "$LAUNCHCTL" "$OSASCRIPT"; do
        case "$tool" in /*) ;; *) die "refusing '$tool': launchctl and osascript are run by absolute path, never looked up on PATH" ;; esac
    done
}

watched() {
    local i=0 s=""
    while [ $i -lt $N ]; do s="$s${s:+, }${W_LABEL[$i]} (${W_PLIST[$i]})"; i=$((i + 1)); done
    printf '%s' "$s"
}

log() {
    local n
    if [ -f "$LOG" ]; then
        n=$(wc -c < "$LOG" 2>/dev/null)
        [ $((n)) -le $LOG_CAP ] || mv -f "$LOG" "$LOG.1"
    fi
    printf '%s %s\n' "$(date '+%Y-%m-%dT%H:%M:%S%z')" "$1" >> "$LOG"
}

# In the background: a notifier that hangs must not stop the checks.
alert() {
    log "$1 (notified)"
    "$OSASCRIPT" -e 'on run argv' -e 'display notification (item 1 of argv) with title (item 2 of argv)' -e 'end run' \
        "$1" "subrouter watchdog" < /dev/null > /dev/null 2>&1 &
}

# Sets OBS to what launchd says of job $1 now. Only rc 113 means not loaded;
# rc 141, for one, is launchd refusing this shell.
observe() {
    local i=$1 label=${W_LABEL[$1]} plist=${W_PLIST[$1]} out rc from
    out=$("$LAUNCHCTL" print "$DOMAIN/$label" 2>&1); rc=$?
    OBS_TEXT=""
    case "$rc" in
        0)
            from=$(printf '%s\n' "$out" | sed -n 's/^	path = //p' | head -1)
            if [ "$from" = "$plist" ] || { [ -n "$from" ] && [ "$from" -ef "$plist" ]; }; then
                OBS=ok
            else
                OBS="foreign:${from:-a plist launchd does not name}"
            fi ;;
        113)
            read_marker "$i"
            if [ "$MARK" = held ]; then OBS="held:$MARK_UNTIL"
            elif [ -f "$plist" ]; then OBS=down
            else OBS=noplist; fi ;;
        *)
            OBS="unknown:$rc" OBS_TEXT=$(printf '%s\n' "$out" | head -1) ;;
    esac
}

enter() { # job state: what is said the first time a job is seen in a state
    local i=$1 new=$2 old=${W_STATE[$1]} label=${W_LABEL[$1]} plist=${W_PLIST[$1]}
    [ "$new" != "$old" ] || return 0
    W_STATE[$i]=$new
    case "$new" in
        ok) case "$old" in ""|ok) ;; *) log "$label is loaded from $plist again" ;; esac ;;
        held:*) log "$label is not loaded, and ${MARK_BY:-someone} announced the stop until $(when "${new#held:}"); leaving it alone" ;;
        down)
            W_FAILS[$i]=0 W_NEXT[$i]=0
            log "$label is not loaded, and no stop is announced; loading it again from $plist" ;;
        noplist) alert "$label is not loaded, and its plist $plist is missing, so the watchdog cannot load it again." ;;
        foreign:*) alert "$label is loaded from ${new#foreign:}, not from $plist. The watchdog leaves it as it is." ;;
        unknown:*) alert "launchctl print $DOMAIN/$label answered rc ${new#unknown:}${OBS_TEXT:+ ($OBS_TEXT)}, which says neither loaded nor not loaded. The watchdog does nothing until launchd answers." ;;
    esac
}

try_load() {
    local i=$1 label=${W_LABEL[$1]} plist=${W_PLIST[$1]} t out rc pause k
    t=$(now)
    [ "$t" -ge "${W_NEXT[$i]}" ] || return 0
    out=$("$LAUNCHCTL" bootstrap "$DOMAIN" "$plist" 2>&1); rc=$?
    if [ "$rc" = 0 ]; then
        if [ "${W_FAILS[$i]}" = 0 ]; then
            alert "$label was not loaded. The watchdog loaded it again from $plist."
        else
            alert "$label is loaded again from $plist, after ${W_FAILS[$i]} failed tries."
        fi
        W_STATE[$i]=ok W_FAILS[$i]=0
        return 0
    fi
    W_FAILS[$i]=$((W_FAILS[$i] + 1))
    pause=$INTERVAL k=0
    while [ $k -lt "${W_FAILS[$i]}" ] && [ "$pause" -lt $MAX_PAUSE ]; do pause=$((pause * 2)) k=$((k + 1)); done
    [ "$pause" -le $MAX_PAUSE ] || pause=$MAX_PAUSE
    W_NEXT[$i]=$((t + pause))
    out=$(printf '%s' "$out" | tr '\n' ' ')
    log "$label: launchctl bootstrap $DOMAIN $plist failed (rc $rc${out:+: $out}); trying again in ${pause}s"
    [ "${W_FAILS[$i]}" != 1 ] || alert "$label is not loaded, and the watchdog could not load it (rc $rc${out:+: $out}). It keeps trying, less often each time; see $LOG."
}

cmd_run() {
    local i n=0
    parse_options "$@"
    DOMAIN="gui/$(id -u)"
    mkdir -p "$(dirname "$LOG")" 2>/dev/null
    log "started (pid $$, launchctl $LAUNCHCTL): watching $(watched) every ${INTERVAL}s; stop markers in $STOPS"
    trap 'log "stopped by a signal"; exit 0' TERM INT HUP
    while :; do
        i=0
        while [ $i -lt $N ]; do
            observe "$i"
            enter "$i" "$OBS"
            [ "$OBS" != down ] || try_load "$i"
            i=$((i + 1))
        done
        n=$((n + 1))
        [ "$CHECKS" = 0 ] || [ "$n" -lt "$CHECKS" ] || break
        # In the background, so a stop signal is handled at once.
        sleep "$INTERVAL" &
        wait $!
    done
    wait
}

cmd_config() {
    local i=0
    parse_options "$@"
    while [ $i -lt $N ]; do echo "watch ${W_LABEL[$i]} ${W_PLIST[$i]}"; i=$((i + 1)); done
    echo "interval $INTERVAL"
    echo "log $LOG"
    echo "stops $STOPS"
    echo "launchctl $LAUNCHCTL"
}

usage() { sed -n '2,/^set -u$/{/^set -u$/d;s/^# \{0,1\}//;p;}' "$0"; }

verb="${1:-}"
[ $# -eq 0 ] || shift
case "$verb" in
    run) cmd_run "$@" ;;
    config) cmd_config "$@" ;;
    hold) cmd_hold "$@" ;;
    release) cmd_release "$@" ;;
    -h|--help|help) usage ;;
    *) usage >&2; exit 2 ;;
esac
