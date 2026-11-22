#!/bin/bash
# install-standby.sh: runs `subrouter standby` (ops/PATCHES.md §79.3) as the
# launchd job ai.manaflow.subrouter-standby (§79.5), so Claude Code sessions
# are sent to a box while the laptop's daemon does not hold its Claude socket.
#
#   install-standby.sh install <subrouter binary> [--peers-file PATH] [--no-watchdog] [--force] [--dry-run]
#   install-standby.sh uninstall [--purge-tokens] [--no-watchdog] [--dry-run]
#   install-standby.sh status
#   install-standby.sh token <box name> [--expires YYYY-MM-DD] < file-holding-the-token
#
# install copies the binary to ~/.subrouter/standby/subrouter-standby, a copy
# no daemon deploy replaces, writes
# ~/Library/LaunchAgents/ai.manaflow.subrouter-standby.plist (RunAtLoad,
# KeepAlive, its log in ~/Library/Logs/subrouter-standby.log) and loads it.
# Before it changes anything it asks the binary, with the job's settings,
# whether the stand-in would arm (`subrouter standby --check`), and refuses
# when it would not. Run again, it upgrades: it announces the stop to the
# watchdog, loads the new copy and withdraws the stop. It ends by having the
# watchdog watch the job (install-watchdog.sh, §81) unless --no-watchdog is
# given. It prints the copy's sha256. --dry-run prints the plist and every
# command, and changes nothing.
#
# uninstall announces the stop, boots the job out, removes its plist, its copy
# and its state files, and has the watchdog watch the daemon alone again. The
# token files stay unless --purge-tokens is given; the log stays.
#
# token reads one token from standard input, never from an argument or a
# terminal, and writes <state dir>/standby-tokens/<box name> at mode 0600 in a
# 0700 folder. --expires records the day the token ends, for status. Nothing of
# the token is printed.
#
# The job's settings are the daemon's: SUBROUTER_STATE_DIR,
# SUBROUTER_CLAUDE_SOCKET, SUBROUTER_REMOTE_POOLS_FILE and
# SUBROUTER_PEER_TAILNET from the daemon plist's environment, and its
# --control-socket, --addr and --worker-config. The daemon plist
# (~/Library/LaunchAgents/ai.manaflow.subrouter.plist) is only read.
# --state-dir, --socket, --control-socket, --daemon-addr and --peers-file
# name a setting outright.
#
# A rehearsal uses a label that starts with test.subrouter. and names
#   --label LABEL --dir DIR [--launch-agents DIR] [--log FILE] [--daemon-plist PATH]
# Its copy, plist and log go in DIR unless named, no daemon plist is read
# unless named, and the watchdog is never touched.
#
# This script calls launchctl (SUBROUTER_STANDBY_LAUNCHCTL, /bin/launchctl,
# never from PATH) only for its own label. The watchdog's job is changed only
# by install-watchdog.sh, which runs SUBROUTER_WATCHDOG_LAUNCHCTL. Runs under
# macOS /bin/bash 3.2 with only what macOS ships.
set -u

HERE=$(cd "$(dirname "$0")" && pwd -P)
WATCHDOG="$HERE/subrouter-watchdog.sh"
WATCHDOG_INSTALLER="$HERE/install-watchdog.sh"
LAUNCHCTL="${SUBROUTER_STANDBY_LAUNCHCTL:-/bin/launchctl}"
REAL_LABEL=ai.manaflow.subrouter-standby
DAEMON_LABEL=ai.manaflow.subrouter
WATCHDOG_LABEL=ai.manaflow.subrouter-watchdog
SCRATCH=test.subrouter.
STOP_HOLD=300
TAB=$(printf '\t')

die() { echo "install-standby: $*" >&2; exit 2; }
[ -n "${HOME:-}" ] || die "HOME is not set"
case "$LAUNCHCTL" in /*) ;; *) die "refusing '$LAUNCHCTL': launchctl is run by absolute path, never looked up on PATH" ;; esac
valid_label() { case "$1" in ""|*[!A-Za-z0-9._-]*) return 1 ;; esac; }
# Every value goes into the plist as written, so none may need escaping.
plain() { case "$1" in *[\&\<\>\"\']*) die "refusing '$1': the plist writer does not escape & < > \" or '" ;; esac; }

VERB="${1:-}"
[ $# -eq 0 ] || shift
LABEL=$REAL_LABEL DIR="" AGENTS="" LOG="" DAEMON_PLIST="" DRY=0 FORCE=0 NO_WATCHDOG=0 PURGE=0 EXPIRES="" HAVE_EXPIRES=0
OPT_STATE="" OPT_SOCKET="" OPT_CONTROL="" OPT_ADDR="" OPT_PEERS="" POSITIONAL=()
case "$VERB" in
    install|uninstall|status) ;;
    token)
        # Every argument of token could be a token pasted in the wrong place,
        # so none is ever printed.
        while [ $# -gt 0 ]; do
            case "$1" in
                --expires) HAVE_EXPIRES=1; EXPIRES="${2:-}"; shift 2 || die "--expires takes a date, YYYY-MM-DD" ;;
                --state-dir) OPT_STATE="${2:-}"; shift 2 || die "--state-dir takes a directory" ;;
                --label) LABEL="${2:-}"; shift 2 || die "--label takes a label" ;;
                --dir) DIR="${2:-}"; shift 2 || die "--dir takes a directory" ;;
                --daemon-plist) DAEMON_PLIST="${2:-}"; shift 2 || die "--daemon-plist takes a plist" ;;
                -*) die "token refuses an option it does not take, and does not print it, in case it is a token" ;;
                *) POSITIONAL+=("$1"); shift ;;
            esac
        done ;;
    *) die "usage: install-standby.sh install <subrouter binary>|uninstall|status|token <box name> [options]" ;;
esac
if [ "$VERB" != token ]; then
    while [ $# -gt 0 ]; do
        case "$1" in
            --label) LABEL="${2:-}"; shift 2 || die "--label takes a label" ;;
            --dir) DIR="${2:-}"; shift 2 || die "--dir takes a directory" ;;
            --launch-agents) AGENTS="${2:-}"; shift 2 || die "--launch-agents takes a directory" ;;
            --log) LOG="${2:-}"; shift 2 || die "--log takes a file" ;;
            --daemon-plist) DAEMON_PLIST="${2:-}"; shift 2 || die "--daemon-plist takes a plist" ;;
            --state-dir) OPT_STATE="${2:-}"; shift 2 || die "--state-dir takes a directory" ;;
            --socket) OPT_SOCKET="${2:-}"; shift 2 || die "--socket takes a path" ;;
            --control-socket) OPT_CONTROL="${2:-}"; shift 2 || die "--control-socket takes a path" ;;
            --daemon-addr) OPT_ADDR="${2:-}"; shift 2 || die "--daemon-addr takes HOST:PORT" ;;
            --peers-file) OPT_PEERS="${2:-}"; shift 2 || die "--peers-file takes a file" ;;
            --no-watchdog) NO_WATCHDOG=1; shift ;;
            --purge-tokens) PURGE=1; shift ;;
            --force) FORCE=1; shift ;;
            --dry-run) DRY=1; shift ;;
            -*) die "unknown option $1" ;;
            *) POSITIONAL+=("$1"); shift ;;
        esac
    done
fi

case "$LABEL" in
    "$DAEMON_LABEL") die "refusing the label $LABEL: that is the daemon's own job, and this script never changes it" ;;
    "$WATCHDOG_LABEL") die "refusing the label $LABEL: that is the watchdog's job, which only install-watchdog.sh changes" ;;
    "$REAL_LABEL") SCRATCH_RUN=0 ;;
    "$SCRATCH"*) valid_label "$LABEL" || die "refusing '$LABEL': not a launchd label"; SCRATCH_RUN=1 ;;
    *) die "refusing the label $LABEL: this installs only $REAL_LABEL, or a label that starts with $SCRATCH for a rehearsal" ;;
esac
if [ "$SCRATCH_RUN" = 1 ]; then
    [ -n "$DIR" ] || die "a scratch stand-in needs --dir, where its copy, plist and log go"
    AGENTS="${AGENTS:-$DIR}" LOG="${LOG:-$DIR/standby.log}" STOPS="$DIR"
else
    [ -z "$AGENTS" ] || die "--launch-agents is for a scratch label: launchd loads $REAL_LABEL at login only from ~/Library/LaunchAgents"
    [ -z "$LOG" ] || die "--log is for a scratch label: $REAL_LABEL logs to ~/Library/Logs/subrouter-standby.log"
    DIR="${DIR:-$HOME/.subrouter/standby}" AGENTS="$HOME/Library/LaunchAgents"
    LOG="$HOME/Library/Logs/subrouter-standby.log" STOPS="$HOME/.subrouter"
    DAEMON_PLIST="${DAEMON_PLIST:-$HOME/Library/LaunchAgents/$DAEMON_LABEL.plist}"
fi
for p in "$DIR" "$AGENTS" "$LOG" ${DAEMON_PLIST:+"$DAEMON_PLIST"}; do
    case "$p" in /*) plain "$p" ;; *) die "'$p' must be an absolute path" ;; esac
done
PLIST="$AGENTS/$LABEL.plist"
COPY="$DIR/subrouter-standby"
WATCHDOG_PLIST="$HOME/Library/LaunchAgents/$WATCHDOG_LABEL.plist"
DOMAIN="gui/$(id -u)"

# --- plists ----------------------------------------------------------------------

# Prints each string of a plist as "<container key><TAB><dict key><TAB><value>",
# the dict key empty for an array item. A binary plist is read through plutil.
plist_strings() {
    [ -f "$1" ] || return 0
    { if [ -x /usr/bin/plutil ]; then /usr/bin/plutil -convert xml1 -o - "$1"; else cat "$1"; fi; } 2>/dev/null |
    awk 'BEGIN { RS = "<"; n = 0; key = "" }
    NR > 1 {
        i = index($0, ">"); if (i == 0) next
        tag = substr($0, 1, i - 1); text = substr($0, i + 1)
        if (tag == "key") { key = text; next }
        if (tag == "dict" || tag == "array") { n++; kind[n] = tag; name[n] = key; key = ""; next }
        if (tag == "/dict" || tag == "/array") { if (n > 0) n--; next }
        if (tag == "string" || tag == "string/") {
            if (tag == "string/") text = ""
            gsub(/&lt;/, "<", text); gsub(/&gt;/, ">", text); gsub(/&quot;/, "\"", text)
            gsub(/&apos;/, "'"'"'", text); gsub(/&amp;/, "\\&", text)
            if (kind[n] == "array") print name[n] "\t\t" text; else print name[n] "\t" key "\t" text
            key = ""; next
        }
        if (substr(tag, 1, 1) != "/") key = ""
    }'
}
plist_env() { plist_strings "$1" | awk -F "$TAB" -v k="$2" '$1 == "EnvironmentVariables" && $2 == k { print $3; exit }'; }
plist_args() { plist_strings "$1" | awk -F "$TAB" '$1 == "ProgramArguments" { print $3 }'; }
# Reads arguments, one per line, and prints the value of --flag X or --flag=X
# before any --.
flag_value() { awk -v f="$1" 'want { print; exit } $0 == "--" { exit } $0 == f { want = 1; next } index($0, f "=") == 1 { print substr($0, length(f) + 2); exit }'; }

# --- the job's settings ----------------------------------------------------------
# A setting named on the command line goes into the job as a flag. One read
# from the daemon plist's environment goes into the job's environment under
# its own name, so the stand-in reads it as the daemon's worker does (the
# worker config before the environment). One read from the daemon's arguments
# goes in as a flag.

ENVV=() FLAGS=()
D_STATE="" D_SOCKET=""
if [ -n "$DAEMON_PLIST" ] && [ -f "$DAEMON_PLIST" ]; then
    for name in SUBROUTER_STATE_DIR SUBROUTER_CLAUDE_SOCKET SUBROUTER_REMOTE_POOLS_FILE SUBROUTER_PEER_TAILNET; do
        value=$(plist_env "$DAEMON_PLIST" "$name")
        [ -n "$value" ] || continue
        plain "$value"
        ENVV+=("$name=$value")
        case "$name" in SUBROUTER_STATE_DIR) D_STATE=$value ;; SUBROUTER_CLAUDE_SOCKET) D_SOCKET=$value ;; esac
    done
    D_ARGS=$(plist_args "$DAEMON_PLIST")
    for pair in --control-socket=--control-socket --addr=--daemon-addr --worker-config=--worker-config; do
        value=$(printf '%s\n' "$D_ARGS" | flag_value "${pair%%=*}")
        [ -n "$value" ] || continue
        plain "$value"
        FLAGS+=("${pair#*=}" "$value")
    done
elif [ -n "$DAEMON_PLIST" ] && [ "$VERB" = install ]; then
    echo "note: the daemon's plist $DAEMON_PLIST is not there; the stand-in gets the default settings"
fi
STATE_DIR="${OPT_STATE:-${D_STATE:-$HOME/.subrouter}}"
for pair in "--state-dir=$OPT_STATE" "--socket=$OPT_SOCKET" "--control-socket=$OPT_CONTROL" "--daemon-addr=$OPT_ADDR" "--peers-file=$OPT_PEERS"; do
    value=${pair#*=}
    [ -n "$value" ] || continue
    plain "$value"
    FLAGS+=("${pair%%=*}" "$value")
done
case "$STATE_DIR" in /*) ;; *) die "the state dir '$STATE_DIR' must be an absolute path" ;; esac
TOKENS="$STATE_DIR/standby-tokens"
EXPIRY="$STATE_DIR/standby-token-expiry"
STATE_FILE="$STATE_DIR/standby-state.json"
plain "$HOME"
ENVV+=("HOME=$HOME" "PATH=/usr/bin:/bin:/usr/sbin:/sbin" "SUBROUTER_LOG_MAX_MB=4" "SUBROUTER_LOG_KEEP=2")

plist_xml() {
    local a e
    printf '<?xml version="1.0" encoding="UTF-8"?>\n'
    printf '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">\n'
    printf '<plist version="1.0">\n<dict>\n'
    printf '\t<key>Label</key>\n\t<string>%s</string>\n' "$LABEL"
    printf '\t<key>ProgramArguments</key>\n\t<array>\n'
    for a in "$COPY" standby ${FLAGS[@]+"${FLAGS[@]}"}; do printf '\t\t<string>%s</string>\n' "$a"; done
    printf '\t</array>\n'
    printf '\t<key>EnvironmentVariables</key>\n\t<dict>\n'
    for e in "${ENVV[@]}"; do printf '\t\t<key>%s</key>\n\t\t<string>%s</string>\n' "${e%%=*}" "${e#*=}"; done
    printf '\t</dict>\n'
    printf '\t<key>RunAtLoad</key>\n\t<true/>\n'
    # It ends itself with exit 1 when it cannot give the socket back, and must
    # then start again.
    printf '\t<key>KeepAlive</key>\n\t<true/>\n'
    printf '\t<key>ThrottleInterval</key>\n\t<integer>10</integer>\n'
    printf '\t<key>StandardOutPath</key>\n\t<string>%s</string>\n' "$LOG"
    printf '\t<key>StandardErrorPath</key>\n\t<string>%s</string>\n' "$LOG"
    printf '</dict>\n</plist>\n'
}

# --- launchd ---------------------------------------------------------------------

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
job_pid() { "$LAUNCHCTL" print "$DOMAIN/$LABEL" 2>/dev/null | sed -n 's/^	pid = //p' | head -1; }

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

# The watchdog loads a watched job it finds unloaded, so a bootout is announced
# first (§81.2). A run that dies after it leaves a stop that ends by itself.
announce_stop() {
    if [ "$DRY" = 1 ]; then echo "would run: $BASH $WATCHDOG hold $LABEL $STOP_HOLD --stops $STOPS --by install-standby.sh $VERB"; return 0; fi
    "$BASH" "$WATCHDOG" hold "$LABEL" "$STOP_HOLD" --stops "$STOPS" --by "install-standby.sh $VERB" > /dev/null || die "could not announce the stop to the watchdog"
}
withdraw_stop() { act "$BASH" "$WATCHDOG" release "$LABEL" --stops "$STOPS"; }

# The watchdog's --watch pairs other than this job's, one per line.
other_watches() {
    plist_args "$WATCHDOG_PLIST" | awk -v own="$LABEL=" 'take { if (index($0, own) != 1) print; take = 0; next } $0 == "--watch" { take = 1 }'
}
watchdog_watches_us() { plist_args "$WATCHDOG_PLIST" | grep -Fxq -- "$LABEL=$PLIST"; }
# Installs the watchdog again with the daemon (its default) and the other
# pairs it watches, and this job when $1 is "with".
set_watchdog() {
    local args=() pair
    [ "$1" != with ] || args+=(--watch "$LABEL=$PLIST")
    while IFS= read -r pair; do [ -z "$pair" ] || args+=(--watch "$pair"); done <<EOF
$(other_watches)
EOF
    act "$BASH" "$WATCHDOG_INSTALLER" install ${args[@]+"${args[@]}"} || die "install-watchdog.sh install failed; the stand-in itself is as this run left it"
}

# --- the stand-in's own words ------------------------------------------------------

# Prints "key<TAB>value" for each field of a JSON file the stand-in wrote, in
# order. Its keys are fixed words and its values hold no comma or brace.
json_fields() {
    tr '{}[],' '\n\n\n\n\n' < "$1" 2>/dev/null |
        sed -n "s/^[[:space:]]*\"\([A-Za-z_]*\)\":[[:space:]]*\"\{0,1\}\([^\"]*\)\"\{0,1\}[[:space:]]*\$/\1$TAB\2/p"
}
field() { awk -F "$TAB" -v k="$1" '$1 == k { print $2; exit }'; }

# One refusal line per reason word of `subrouter standby --check`.
refusal() {
    case "$1" in
        remote_locked) echo "refusing: this machine's remote.key gate is locked for the state dir $5 (its codex/remote.key is missing, or refused as said above), so the stand-in would reach no box" ;;
        peers_file_missing) echo "refusing: the stand-in has no peers file ($2); set SUBROUTER_REMOTE_POOLS_FILE in the daemon's plist or name one with --peers-file" ;;
        no_supervisor_reading) echo "refusing: no supervisor answered on $3; the stand-in works only beside a daemon run by \`subrouter supervise\`, so start or activate that first" ;;
        supervisor_without_marker) echo "refusing: the supervisor's status lacks claude_socket_waits_for_lock, so it would never take the socket back from a stand-in; restart the supervisor on a build that has it first (PATCHES §77.1)" ;;
        supervisor_keeps_no_socket) echo "refusing: the supervisor keeps no Claude socket now (claude_socket: $4), so the stand-in would never arm; serve the socket first" ;;
        *) echo "refusing: the stand-in says it would stay disarmed ($1)" ;;
    esac
}

# Asks the binary whether a stand-in with the job's settings would arm, as the
# job would run it, and refuses when it would not.
check_would_arm() {
    local out err rc verdict peers control word state name token
    err=$(mktemp "${TMPDIR:-/tmp}/install-standby.XXXXXX") || die "mktemp failed"
    out=$(env -i "${ENVV[@]}" "$1" standby ${FLAGS[@]+"${FLAGS[@]}"} --check 2> "$err"); rc=$?
    verdict=$(printf '%s\n' "$out" | sed -n 's/^verdict //p' | head -1)
    if [ "$rc" != 0 ] || [ -z "$verdict" ]; then
        echo "install-standby: $1 did not answer \`standby --check\` (exit $rc): $(head -3 "$err" | tr '\n' ' ')" >&2
        rm -f "$err"
        die "refusing: the binary is older than this script, or not a subrouter build"
    fi
    peers=$(printf '%s\n' "$out" | sed -n 's/^peers_file //p' | head -1)
    control=$(printf '%s\n' "$out" | sed -n 's/^control_socket //p' | head -1)
    word=$(printf '%s\n' "$out" | sed -n 's/^supervisor \([^ ]*\).*/\1/p' | head -1)
    state=$(printf '%s\n' "$out" | sed -n 's/^state_dir //p' | head -1)
    case "$verdict" in
        armed) ;;
        "disarmed "*)
            [ ! -s "$err" ] || sed 's/^/    /' "$err" >&2
            rm -f "$err"
            die "$(refusal "${verdict#disarmed }" "${peers:-none is named}" "$control" "$word" "$state")" ;;
        *) rm -f "$err"; die "refusing: \`standby --check\` answered '$verdict'" ;;
    esac
    rm -f "$err"
    case "$peers" in "ok "*) ;; *) echo "warning: the peers file is ${peers%% *} (${peers#* }); the stand-in sends nothing to a box until it loads" ;; esac
    printf '%s\n' "$out" | sed -n 's/^box //p' | while read -r name token; do
        case "$token" in
            untested|ok) ;;
            file_missing) echo "warning: box $name has no token file; give it one with: bash $0 token $name < file-holding-the-token" ;;
            *) echo "warning: the token file of box $name is $token; write it again with: bash $0 token $name < file-holding-the-token" ;;
        esac
    done
}

# --- days, in bash alone (date parses differently on macOS and Linux) ------------

day_number() { # y m d: days since 1970-01-01
    local y=$1 m=$2 d=$3 era yoe doy
    [ "$m" -gt 2 ] || y=$((y - 1))
    era=$(( (y >= 0 ? y : y - 399) / 400 ))
    yoe=$((y - era * 400))
    doy=$(( (153 * (m > 2 ? m - 3 : m + 9) + 2) / 5 + d - 1 ))
    echo $((era * 146097 + yoe * 365 + yoe / 4 - yoe / 100 + doy - 719468))
}
date_day() { # YYYY-MM-DD: its day number, or fails
    case "$1" in [0-9][0-9][0-9][0-9]-[0-1][0-9]-[0-3][0-9]) ;; *) return 1 ;; esac
    local y=$((10#${1:0:4})) m=$((10#${1:5:2})) d=$((10#${1:8:2}))
    [ "$m" -ge 1 ] && [ "$m" -le 12 ] && [ "$d" -ge 1 ] && [ "$d" -le 31 ] || return 1
    day_number "$y" "$m" "$d"
}
epoch_of() { # an RFC 3339 time: seconds since 1970, or fails
    local t=$1 z off=0 days
    days=$(date_day "${t:0:10}") || return 1
    case "${t:10:9}" in T[0-2][0-9]:[0-5][0-9]:[0-6][0-9]) ;; *) return 1 ;; esac
    z=${t:19}
    case "$z" in .*) z=${z#.}; while case "$z" in [0-9]*) true ;; *) false ;; esac; do z=${z#?}; done ;; esac
    case "$z" in
        Z) ;;
        [+-][0-2][0-9]:[0-5][0-9]) off=$(( 10#${z:1:2} * 3600 + 10#${z:4:2} * 60 )); [ "${z:0:1}" = + ] || off=$((0 - off)) ;;
        *) return 1 ;;
    esac
    echo $(( days * 86400 + 10#${t:11:2} * 3600 + 10#${t:14:2} * 60 + 10#${t:17:2} - off ))
}
ago() { # seconds: as a short duration
    local s=$1
    if [ "$s" -lt 120 ]; then echo "${s} s"; elif [ "$s" -lt 7200 ]; then echo "$((s / 60)) min"
    elif [ "$s" -lt 172800 ]; then echo "$((s / 3600)) h"; else echo "$((s / 86400)) days"; fi
}
# The word for a token whose last day is $1: ok, renew_soon in its last 7
# days, lapsed after it.
expiry_word() {
    local last today
    last=$(date_day "$1") || { echo unreadable; return; }
    today=$(( $(date +%s) / 86400 ))
    if [ "$today" -gt "$last" ]; then echo lapsed; elif [ $((last - today)) -lt 7 ]; then echo renew_soon; else echo ok; fi
}

# --- verbs -----------------------------------------------------------------------

cmd_token() {
    local box token f
    [ ${#POSITIONAL[@]} -eq 1 ] || die "token takes one box name; the token itself goes on standard input"
    box=${POSITIONAL[0]}
    # A box name is a peers-file name, [A-Za-z0-9._-]{1,64}. A stand-in token
    # starts with srk_, and a static one is a long run of hex digits.
    case "$box" in
        ""|.|..|*[!A-Za-z0-9._-]*|[Ss][Rr][Kk]_*|*[0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F]*)
            die "refusing the box name: it is not a box name of the peers file, or it looks like a token; it is not printed. The token goes on standard input" ;;
    esac
    [ ${#box} -le 64 ] || die "refusing the box name: longer than 64 characters, so not a box name of the peers file; it is not printed"
    if [ "$HAVE_EXPIRES" = 1 ]; then
        date_day "$EXPIRES" > /dev/null || die "--expires takes the token's last day as YYYY-MM-DD; the value given is not printed"
    fi
    [ ! -t 0 ] || die "token reads the token from standard input, not from a terminal, so it never shows on screen or in history: pbpaste | bash $0 token $box"
    token=$(cat)
    # Surrounding spaces and line ends go, as the stand-in reads the file.
    token=$(printf '%s' "$token" | tr -d '\r' | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' | sed '/^$/d')
    [ -n "$token" ] || die "standard input held no token"
    case "$token" in *[[:space:]]*) die "standard input held more than one word; a token is one" ;; esac
    [ ${#token} -lt 4096 ] || die "standard input held more than 4 KiB; that is not a token"
    umask 077
    mkdir -p "$TOKENS" && chmod 700 "$TOKENS" || die "cannot make $TOKENS private"
    f="$TOKENS/.$box.new.$$"
    { printf '%s\n' "$token" > "$f" && chmod 600 "$f" && mv -f "$f" "$TOKENS/$box"; } || { rm -f "$f"; die "cannot write $TOKENS/$box"; }
    echo "wrote the token of box $box to $TOKENS/$box (mode 0600); a running stand-in reads it within 2 s"
    if [ "$HAVE_EXPIRES" = 1 ]; then
        mkdir -p "$EXPIRY" && chmod 700 "$EXPIRY" || die "cannot make $EXPIRY private"
        { printf '%s\n' "$EXPIRES" > "$EXPIRY/.$box.new.$$" && mv -f "$EXPIRY/.$box.new.$$" "$EXPIRY/$box"; } || die "cannot write $EXPIRY/$box"
        echo "its last day is $EXPIRES ($(expiry_word "$EXPIRES")); status says renew_soon from 7 days before"
    elif [ -e "$EXPIRY/$box" ]; then
        # The day recorded was the old token's.
        rm -f "$EXPIRY/$box"
    fi
}


sha256_of() {
    if [ -x /usr/bin/shasum ]; then /usr/bin/shasum -a 256 "$1"; else sha256sum "$1"; fi | awk '{ print $1 }'
}

cmd_install() {
    local bin word pid n said
    [ ${#POSITIONAL[@]} -eq 1 ] || die "usage: install-standby.sh install <subrouter binary> [options]"
    bin=${POSITIONAL[0]}
    case "$bin" in /*) ;; *) bin="$PWD/$bin" ;; esac
    [ -f "$bin" ] && [ -x "$bin" ] || die "$bin is not an executable file"
    [ "$bin" != "$COPY" ] || die "install takes a new build, not the installed copy $COPY"
    check_would_arm "$bin"
    case "$STATE" in
        loaded:*)
            # A stand-in that holds the socket carries every session on the
            # Mac, and a restart now would cut them.
            word=$(json_fields "$STATE_FILE" | field state)
            pid=$(json_fields "$STATE_FILE" | field pid)
            case "$word" in
                holding|serving|yielding)
                    if [ -n "$pid" ] && [ "$pid" = "$(job_pid)" ] && [ "$FORCE" = 0 ]; then
                        die "refusing: the stand-in holds the Claude socket now (state $word), and an upgrade would cut the sessions it carries; run it again once the daemon has the socket back, or add --force"
                    fi ;;
            esac ;;
    esac

    if [ "$DRY" = 1 ]; then
        echo "would copy $bin (sha256 $(sha256_of "$bin")) to $COPY"
        echo "would write $PLIST:"
        plist_xml | sed 's/^/    /'
    else
        mkdir -p "$DIR" "$AGENTS" "$(dirname "$LOG")" || die "cannot create $DIR, $AGENTS or $(dirname "$LOG")"
        # New files, renamed into place once the old job is out: until then
        # the running stand-in keeps the file it started from.
        { cp "$bin" "$COPY.new.$$" && chmod 755 "$COPY.new.$$"; } || { rm -f "$COPY.new.$$"; die "cannot copy $bin to $DIR"; }
        plist_xml > "$PLIST.new.$$" || { rm -f "$COPY.new.$$" "$PLIST.new.$$"; die "cannot write $PLIST"; }
        if [ -x /usr/bin/plutil ] && ! /usr/bin/plutil -lint "$PLIST.new.$$" > /dev/null; then
            rm -f "$COPY.new.$$" "$PLIST.new.$$"; die "the plist written for $LABEL does not lint; nothing was loaded"
        fi
    fi
    case "$STATE" in
        loaded:*)
            announce_stop
            ( bootout_own ) || { rm -f "$COPY.new.$$" "$PLIST.new.$$"; die "the old stand-in was not unloaded, so nothing was changed; the stop announced to the watchdog ends within $STOP_HOLD s"; } ;;
    esac
    if [ "$DRY" = 0 ]; then
        { mv -f "$COPY.new.$$" "$COPY" && mv -f "$PLIST.new.$$" "$PLIST"; } || die "cannot move the new copy and plist into place in $DIR and $AGENTS"
    fi
    act "$LAUNCHCTL" bootstrap "$DOMAIN" "$PLIST" ||
        die "launchctl bootstrap $DOMAIN $PLIST failed; the files are in place, so it can be run again by hand"
    case "$STATE" in loaded:*) withdraw_stop ;; esac
    if [ "$DRY" = 0 ]; then
        STATE=$(job_state)
        [ "$STATE" = "loaded:$PLIST" ] || die "launchd answers '$STATE' for $LABEL after the bootstrap, not loaded:$PLIST"
        echo "installed: $DOMAIN/$LABEL runs $COPY (sha256 $(sha256_of "$COPY")); its log is $LOG"
    fi
    if [ "$SCRATCH_RUN" = 0 ] && [ "$NO_WATCHDOG" = 0 ]; then
        set_watchdog with
    fi
    [ "$DRY" = 0 ] || return 0
    # What the new stand-in says, once it has written its state.
    n=0 said=""
    while [ $n -lt 20 ]; do
        pid=$(json_fields "$STATE_FILE" | field pid)
        if [ -n "$pid" ] && [ "$pid" = "$(job_pid)" ]; then
            said=$(json_fields "$STATE_FILE" | awk -F "$TAB" '$1 == "state" { s = $2 } $1 == "reason" { r = " (" $2 ")" } END { print s r }')
            [ "$said" = "disarmed (no_supervisor_reading)" ] || break
        fi
        n=$((n + 1)); sleep 0.5
    done
    echo "the stand-in says: ${said:-nothing yet; see bash $0 status}"
}

cmd_uninstall() {
    local loaded=0
    case "$STATE" in loaded:*) loaded=1 ;; esac
    [ "$loaded" = 1 ] || [ -e "$PLIST" ] || [ -e "$COPY" ] || [ "$PURGE" = 1 ] || { echo "nothing to uninstall: $LABEL is not loaded and $PLIST is not there"; return 0; }
    announce_stop
    [ "$loaded" = 0 ] || bootout_own
    if [ "$SCRATCH_RUN" = 0 ] && [ "$NO_WATCHDOG" = 0 ] && watchdog_watches_us; then
        set_watchdog without
    fi
    act rm -f "$PLIST" "$COPY" "$STATE_DIR/standby-state.json" "$STATE_DIR/standby-supervisor.json" "$STATE_DIR/standby-policy.json"
    [ "$PURGE" = 0 ] || act rm -rf "$TOKENS" "$EXPIRY"
    withdraw_stop
    [ "$DRY" = 0 ] || return 0
    rmdir "$DIR" 2>/dev/null
    if [ "$PURGE" = 1 ]; then echo "uninstalled: $DOMAIN/$LABEL, and its token files; its log stays at $LOG"
    else echo "uninstalled: $DOMAIN/$LABEL; the token files stay in $TOKENS, and its log at $LOG"; fi
}

cmd_status() {
    local fields boxes names name word at last now saved age line
    case "$STATE" in
        loaded:*) echo "job $DOMAIN/$LABEL: loaded from ${STATE#loaded:}, pid $(job_pid)" ;;
        none) echo "job $DOMAIN/$LABEL: not loaded" ;;
        *) echo "job $DOMAIN/$LABEL: launchctl print answered ${STATE#rc:}" ;;
    esac
    if [ -f "$PLIST" ]; then echo "plist $PLIST: runs $(plist_args "$PLIST" | tr '\n' ' ' | sed 's/ $//')"; else echo "plist $PLIST: missing"; fi
    if [ -f "$COPY" ]; then echo "copy $COPY: sha256 $(sha256_of "$COPY")"; else echo "copy $COPY: missing"; fi
    now=$(date +%s)
    if [ -f "$STATE_FILE" ]; then
        fields=$(json_fields "$STATE_FILE")
        line="state: $(printf '%s\n' "$fields" | field state)"
        word=$(printf '%s\n' "$fields" | field reason); [ -z "$word" ] || line="$line ($word)"
        word=$(printf '%s\n' "$fields" | field warning); [ -z "$word" ] || line="$line, warning $word"
        line="$line since $(printf '%s\n' "$fields" | field since), written by pid $(printf '%s\n' "$fields" | field pid)"
        case "$STATE" in
            loaded:*) [ "$(printf '%s\n' "$fields" | field pid)" = "$(job_pid)" ] || line="$line, not the job's pid: this file is stale" ;;
            *) line="$line; the job is not loaded, so this file is stale" ;;
        esac
        echo "$line"
        word=$(printf '%s\n' "$fields" | field claude_socket)
        if [ -n "$word" ]; then
            echo "supervisor: $word, waits for the lock: $(printf '%s\n' "$fields" | field claude_socket_waits_for_lock), answering: $(printf '%s\n' "$fields" | field supervisor_answering)"
        else
            echo "supervisor: no reading yet"
        fi
        echo "peers file: $(printf '%s\n' "$fields" | field peers_file)"
        boxes=$(printf '%s\n' "$fields" | awk -F "$TAB" '$1 == "name" { if (n) print n "\t" t "\t" a; n = $2; t = ""; a = "" } $1 == "token" { t = $2 } $1 == "at" { a = $2 } END { if (n) print n "\t" t "\t" a }')
        saved=$(printf '%s\n' "$fields" | field saved_at)
        if [ "$(printf '%s\n' "$fields" | field held)" = true ] && [ -n "$saved" ]; then
            if age=$(epoch_of "$saved"); then echo "policy copy: held, saved $(ago $((now - age))) ago, at $saved"; else echo "policy copy: held, saved at $saved"; fi
        else
            echo "policy copy: not held"
        fi
    else
        echo "state: none yet ($STATE_FILE is not there)"
        boxes=""
    fi
    names=$(printf '%s\n' "$boxes" | cut -f1)
    for f in "$EXPIRY"/*; do
        [ -f "$f" ] || continue
        printf '%s\n' "$names" | grep -Fxq -- "${f##*/}" || boxes="$boxes${boxes:+
}${f##*/}		"
    done
    printf '%s\n' "$boxes" | while IFS="$TAB" read -r name word at; do
        [ -n "$name" ] || continue
        line="box $name: token ${word:-not reported yet}${at:+ at $at}"
        if [ -f "$EXPIRY/$name" ]; then
            last=$(head -1 "$EXPIRY/$name")
            line="$line, last day $last: $(expiry_word "$last")"
        fi
        echo "$line"
    done
    if [ "$SCRATCH_RUN" = 1 ]; then echo "watchdog: not used for a scratch label"
    elif [ ! -f "$WATCHDOG_PLIST" ]; then echo "watchdog: not installed ($WATCHDOG_PLIST is not there)"
    elif watchdog_watches_us; then echo "watchdog: watches $LABEL ($WATCHDOG_PLIST)"
    else echo "watchdog: does not watch $LABEL; install again, or run install-watchdog.sh install --watch $LABEL=$PLIST"; fi
    echo "log: $LOG"
}

if [ "$VERB" != token ]; then
    STATE=$(job_state)
    case "$VERB:$STATE" in
        status:*|*:none) ;;
        *:loaded:*)
            FROM=${STATE#loaded:}
            if [ "$FROM" != "$PLIST" ] && ! [ "$FROM" -ef "$PLIST" ]; then
                die "refusing: launchd loaded $LABEL from ${FROM:-a plist it does not name}, not from $PLIST. launchd was not changed."
            fi ;;
        *) die "launchctl print $DOMAIN/$LABEL answered ${STATE#rc:}; only 0 and 113 are answers. Nothing was changed." ;;
    esac
fi
case "$VERB" in
    install) cmd_install ;;
    uninstall) [ ${#POSITIONAL[@]} -eq 0 ] || die "uninstall takes no argument"; cmd_uninstall ;;
    status) [ ${#POSITIONAL[@]} -eq 0 ] || die "status takes no argument"; cmd_status ;;
    token) cmd_token ;;
esac
