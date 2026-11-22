#!/bin/bash
# Rehearse the stand-in (PATCHES §79.5) on this Mac's real launchd, with
# scratch labels, a scratch home and a scratch socket path only:
#
#   bash ops/deploy/tests/rehearse-standby.sh <scratch-dir> <subrouter binary> <token-dir> <peers-file>
#
# <subrouter binary> is a build of this tree. <token-dir> holds one stand-in
# token file per box, named as the box is in <peers-file>, the daemon's peers
# file (read in place, never copied or changed). Keep <scratch-dir> short, such
# as /tmp/sbr: a socket path must stay under 104 bytes.
#
# It runs a scratch supervisor job (label test.subrouter.sbrehearsal.<pid>.sup,
# its worker on 127.0.0.1:31496, its Claude socket <scratch-dir>/run.<pid>/claude.sock)
# with a home, a state dir and a remote.key of its own, and installs a scratch
# stand-in beside it with install-standby.sh. A probe asks the socket path for
# its health every 50 ms and notes who answered. Then:
#   1. it boots the supervisor's job out: the path's owner goes from the daemon
#      to the stand-in with no gap over 1 s, and one real turn sent at once is
#      answered by a box after the stand-in's 30 s hold;
#   2. it bootstraps the job again: the owner is back at the daemon within 10 s;
#   3. it has the supervisor yield the socket for 60 s: the same two hand-overs.
# It prints PASS or FAIL per check and ends with one REHEARSAL line. The turn
# costs one short haiku answer on a box.
#
# Every launchctl call, its own and install-standby.sh's, goes through a guard
# in <scratch-dir> that refuses any label not starting with
# test.subrouter.sbrehearsal., any plist outside <scratch-dir>, and every verb
# but print, bootstrap, bootout, list and managername. No account, setting or
# file of the Mac's daemon is read or written, the peers file aside. The token
# copies are removed at the end; KEEP=1 keeps the rest of the scratch folder.
set -u

[ "$(uname)" = Darwin ] || { echo "this rehearsal needs macOS launchd" >&2; exit 2; }
[ $# = 4 ] || { echo "usage: $0 <scratch-dir> <subrouter binary> <token-dir> <peers-file>" >&2; exit 2; }
HERE=$(cd "$(dirname "$0")" && pwd -P)
MACOS=$(cd "$HERE/../macos" && pwd -P)
mkdir -p "$1" && ROOT=$(cd "$1" && pwd -P) || exit 2
# pwd spells the root / or //, so "$ROOT/" is then // or ///.
case "$ROOT/" in "$HOME/"|"$HOME/.subrouter/"*|"$HOME/.claude/"*|"$HOME/Library/"*|"$HOME/bin/"*|//|///)
    echo "refusing scratch dir $ROOT" >&2; exit 2 ;;
esac
BIN_IN=$2 TOKENS_IN=$3 PEERS=$4
[ -f "$BIN_IN" ] && [ -x "$BIN_IN" ] || { echo "$BIN_IN is not an executable file" >&2; exit 2; }
[ -d "$TOKENS_IN" ] || { echo "$TOKENS_IN is not a folder" >&2; exit 2; }
case "$PEERS" in /*) ;; *) PEERS="$PWD/$PEERS" ;; esac
[ -f "$PEERS" ] || { echo "$PEERS is not a file" >&2; exit 2; }

PFX=test.subrouter.sbrehearsal.
SUP="${PFX}$$.sup"
SB="${PFX}$$.standby"
UIDN=$(id -u)
DOM="gui/$UIDN"
PORT=31496
ADDR="127.0.0.1:$PORT"
S="$ROOT/run.$$"
GUARD="$S/launchctl-guard"
H="$S/home"
STATE="$H/.subrouter"
SOCK="$S/claude.sock"
CONTROL="$S/sv.sock"
SUP_PLIST="$S/$SUP.plist"
SB_DIR="$S/standby"
LOGS="$S/logs"
PROBE="$S/probe.log"
[ ${#SOCK} -lt 100 ] && [ ${#CONTROL} -lt 100 ] || { echo "the socket path $SOCK is ${#SOCK} bytes; a Unix socket path must stay under 104. Use a shorter scratch dir, such as /tmp/sbr" >&2; exit 2; }
case "$SOCK" in "$ROOT/"*) ;; *) echo "refusing the socket path $SOCK: it is outside $ROOT" >&2; exit 2 ;; esac
if curl -s -m 1 -o /dev/null "http://$ADDR/"; then echo "$ADDR is in use" >&2; exit 2; fi
mkdir -p "$S/bin" "$LOGS" "$STATE/codex" "$H/.claude" "$H/.codex" "$H/.config/subrouter" || exit 2
chmod 700 "$S" "$STATE" "$STATE/codex"

cat > "$GUARD" <<G
#!/bin/bash
# A launchctl that takes only this rehearsal's scratch labels and plists.
refuse() { echo "REFUSED by the rehearsal's guard: launchctl \$*" >&2; echo "launchctl \$*" >> "$S/refused"; exit 99; }
case "\${1:-}" in print|bootstrap|bootout|list|managername) ;; *) refuse "\$@" ;; esac
first=1
for a in "\$@"; do
  [ \$first = 1 ] && { first=0; continue; }
  case "\$a" in */../*|*/./*) refuse "\$@" ;; esac
  case "\$a" in
    "$DOM") ;;
    "$DOM/$PFX"*) case "\${a#$DOM/}" in *[!A-Za-z0-9._-]*) refuse "\$@" ;; esac ;;
    "$S/"*.plist) ;;
    *) refuse "\$@" ;;
  esac
done
exec /bin/launchctl "\$@"
G
chmod 755 "$GUARD"
lc() { "$GUARD" "$@"; }
loaded() { lc print "$DOM/$1" > /dev/null 2>&1; }
now() { /usr/bin/perl -MTime::HiRes=time -e 'printf "%.3f\n", time'; }
since() { /usr/bin/perl -e 'printf "%.1f", $ARGV[1] - $ARGV[0]' -- "$1" "$(now)"; }
under() { /usr/bin/perl -e 'exit($ARGV[0] < $ARGV[1] ? 0 : 1)' -- "$1" "$2"; }
standby_installer() {
    env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME="$H" TMPDIR="$S" SUBROUTER_STANDBY_LAUNCHCTL="$GUARD" \
        /bin/bash "$MACOS/install-standby.sh" "$@" --label "$SB" --dir "$SB_DIR" --state-dir "$STATE"
}
state_word() { sed -n 's/^  "state": "\([a-z]*\)".*/\1/p' "$STATE/standby-state.json" 2>/dev/null; }
sv_status() { curl -s -m 5 --unix-socket "$CONTROL" http://supervisor/_subrouter/supervisor-status; }

PASS=0; FAILS=0; PROBE_PID=""
pass() { echo "  PASS  $1"; PASS=$((PASS + 1)); }
failed() { echo "  FAIL  $1"; FAILS=$((FAILS + 1)); }

cleanup() {
    [ -z "$PROBE_PID" ] || { kill "$PROBE_PID" 2>/dev/null; wait "$PROBE_PID" 2>/dev/null; }
    standby_installer uninstall --purge-tokens > "$LOGS/uninstall.out" 2>&1
    lc bootout "$DOM/$SUP" > /dev/null 2>&1
    sleep 1
    local left; left=$(lc list 2>/dev/null | grep -c "$PFX$$")
    if [ "$left" = 0 ]; then pass "no scratch job is left in launchd"; else failed "$left scratch jobs are still in launchd"; fi
    [ ! -s "$S/refused" ] || failed "the guard refused $(wc -l < "$S/refused" | tr -d ' ') launchctl calls: $(head -1 "$S/refused")"
    rm -rf "$STATE/standby-tokens"
    if [ -n "${KEEP:-}" ]; then echo "  kept $S (the token copies are removed)"; else rm -rf "$S"; fi
    if [ "$FAILS" = 0 ]; then echo "REHEARSAL PASS ($PASS checks)"; else echo "REHEARSAL FAIL ($FAILS of $((PASS + FAILS)) checks failed)"; fi
    [ "$FAILS" = 0 ]
}
trap 'cleanup; exit $?' EXIT
trap 'exit 130' INT TERM HUP

# The probe: who answers the path's health, every 50 ms, one line per answer:
# "<time> daemon|standin|other|none".
start_probe() {
    /usr/bin/perl -MIO::Socket::UNIX -MIO::Select -MTime::HiRes=time,sleep -e '
        $| = 1; my ($path, $out) = @ARGV;
        open(my $log, ">>", $out) or die "probe: $out: $!";
        $log->autoflush(1);
        while (1) {
            my $word = "none";
            my $s = IO::Socket::UNIX->new(Peer => $path, Timeout => 1);
            if ($s) {
                print $s "GET /_subrouter/health HTTP/1.0\r\nHost: api.anthropic.com\r\n\r\n";
                my $sel = IO::Select->new($s); my $body = ""; my $until = time + 2;
                while (time < $until && $sel->can_read($until - time)) { my $n = sysread($s, my $buf, 65536); last unless $n; $body .= $buf; }
                close $s;
                if ($body =~ /"claude_socket":\s*"standby"/) { $word = "standin" }
                elsif ($body =~ /"claude_socket":\s*"(owned_by_supervisor|serving)"/) { $word = "daemon" }
                elsif ($body ne "") { $word = "other" }
            }
            printf $log "%.3f %s\n", time, $word;
            sleep 0.05;
        }' "$SOCK" "$PROBE" &
    PROBE_PID=$!
}
# owner_at T: who answered last at or before time T.
owner_at() { awk -v t="$1" '$1 <= t { w = $2 } END { print w }' "$PROBE"; }
# hand_over FROM TO T0 LIMIT: once the path is TO's within LIMIT s of T0,
# prints "<seconds after T0> <longest gap between answers, in s>" over the time
# from T0 to then, with the last FROM answer before T0 as the start.
hand_over() {
    awk -v from="$1" -v to="$2" -v t0="$3" -v limit="$4" '
        $1 < t0 { if ($2 == from) last = $1; next }
        $1 > t0 + limit { exit }
        $2 == "none" { next }
        { if (last != "" && $1 - last > gap) gap = $1 - last; last = $1 }
        $2 == to { printf "%.1f %.2f\n", $1 - t0, gap; done = 1; exit }
        END { if (!done) print "never" }' "$PROBE"
}
await_owner() { # word limit-seconds
    local t0; t0=$(now)
    until [ "$(tail -1 "$PROBE" 2>/dev/null | cut -d' ' -f2)" = "$1" ]; do under "$(since "$t0")" "$2" || return 1; sleep 0.1; done
}
check_hand_over() { # what from to t0 limit max-seconds
    local got secs gap
    got=$(hand_over "$2" "$3" "$4" "$5")
    if [ "$got" = never ]; then failed "$1: the path was not the $3's within $5 s"; return; fi
    secs=${got% *} gap=${got#* }
    if under "$secs" "$6" && under "$gap" 1.001; then pass "$1: the $3 answered at the path $secs s later, longest gap between answers $gap s"
    else failed "$1: the $3 answered at the path $secs s later (want under $6), longest gap between answers $gap s (want 1 or less)"; fi
}

write_supervisor_plist() {
    cat > "$SUP_PLIST" <<P
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>$SUP</string>
<key>ProgramArguments</key><array>
<string>$S/bin/subrouter</string><string>supervise</string><string>--addr</string><string>$ADDR</string>
<string>--control-socket</string><string>$CONTROL</string><string>--worker-bin</string><string>$S/bin/subrouter</string>
<string>--drain-timeout</string><string>10s</string><string>--worker-stop-grace</string><string>10s</string>
<string>--ready-timeout</string><string>60s</string><string>--</string>
<string>--transcripts</string><string>$STATE/transcripts</string>
</array>
<key>EnvironmentVariables</key><dict>
<key>HOME</key><string>$H</string><key>PATH</key><string>/usr/bin:/bin:/usr/sbin:/sbin</string>
<key>SUBROUTER_STATE_DIR</key><string>$STATE</string><key>CLAUDE_CONFIG_DIR</key><string>$H/.claude</string>
<key>CODEX_HOME</key><string>$H/.codex</string><key>SUBROUTER_CLOUD_CONFIG</key><string>$H/.config/subrouter/cloud.json</string>
<key>SUBROUTER_CLAUDE_SOCKET</key><string>$SOCK</string><key>SUBROUTER_DRAIN_SOCKET</key><string>$S/drain.sock</string>
</dict>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>ExitTimeOut</key><integer>30</integer>
<key>WorkingDirectory</key><string>$H</string>
<key>StandardOutPath</key><string>$LOGS/supervisor.log</string><key>StandardErrorPath</key><string>$LOGS/supervisor.log</string>
</dict></plist>
P
}

# One turn as Claude Code sends it, through the path, in the background.
send_turn() {
    local sid
    sid=$(od -An -tx1 -N16 /dev/urandom | tr -d ' \n' | sed 's/^\(........\)\(....\).\(...\).\(...\)\(............\).*/\1-\2-4\3-8\4-\5/')
    TURN_SID=$sid TURN_MARK="REHEARSAL-$$-$1"
    ( start=$(now)
      curl -sS -m 200 --unix-socket "$SOCK" http://api.anthropic.com/v1/messages \
          -H 'content-type: application/json' -H 'anthropic-version: 2023-06-01' \
          -H 'anthropic-beta: claude-code-20250219,oauth-2025-04-20' -H 'x-app: cli' \
          -H 'User-Agent: claude-cli/2.1.199 (external, cli)' -H 'Authorization: Bearer ssh-placeholder' \
          -H "X-Claude-Code-Session-Id: $sid" -o "$S/turn-$1.body" -w '%{http_code}' \
          -d "{\"model\":\"claude-haiku-4-5\",\"max_tokens\":64,\"system\":[{\"type\":\"text\",\"text\":\"You are Claude Code, Anthropic's official CLI for Claude.\"}],\"messages\":[{\"role\":\"user\",\"content\":\"Reply with exactly $TURN_MARK\"}]}" \
          > "$S/turn-$1.code" 2> "$S/turn-$1.err"
      echo "$(since "$start")" > "$S/turn-$1.took" ) &
    TURN_PID=$!
}

echo "== stand-in rehearsal: $SUP and $SB, in $S"
echo "   load average: $(sysctl -n vm.loadavg)"
[ "$(lc managername 2>&1)" = Aqua ] || { failed "this shell is not in the logged-in GUI session (launchctl managername is not Aqua); run it from a Terminal.app window"; exit 1; }

head -c 32 /dev/urandom | base64 > "$STATE/codex/remote.key" && chmod 600 "$STATE/codex/remote.key"
mkdir -p "$STATE/standby-tokens" && chmod 700 "$STATE/standby-tokens"
n=0
for f in "$TOKENS_IN"/*; do
    [ -f "$f" ] || continue
    case "${f##*/}" in ""|.*|*[!A-Za-z0-9._-]*) continue ;; esac
    ( umask 077; cp "$f" "$STATE/standby-tokens/${f##*/}" ) && chmod 600 "$STATE/standby-tokens/${f##*/}" && n=$((n + 1))
done
[ "$n" -gt 0 ] || { failed "no token file in $TOKENS_IN"; exit 1; }
cp "$BIN_IN" "$S/bin/subrouter" && chmod 755 "$S/bin/subrouter" || { failed "could not copy $BIN_IN"; exit 1; }

write_supervisor_plist
lc bootstrap "$DOM" "$SUP_PLIST" || { failed "could not load the scratch supervisor"; exit 1; }
t0=$(now); until sv_status | grep -q '"claude_socket": *"serving"'; do under "$(since "$t0")" 60 || break; sleep 0.5; done
if sv_status | grep -q '"claude_socket_waits_for_lock": *true'; then pass "the scratch supervisor serves its socket and waits for the lock ($(since "$t0") s)"
else failed "the scratch supervisor does not serve its socket with the marker: $(sv_status | head -c 300)"; exit 1; fi

if standby_installer install "$S/bin/subrouter" --socket "$SOCK" --control-socket "$CONTROL" --daemon-addr "$ADDR" --peers-file "$PEERS" > "$LOGS/install.out" 2>&1; then
    pass "install-standby.sh installed the scratch stand-in ($(grep '^installed:' "$LOGS/install.out" | sed 's/.*(sha256 \([0-9a-f]\{12\}\).*/sha256 \1.../'))"
    grep '^warning' "$LOGS/install.out" | sed 's/^/        /'
else
    failed "install-standby.sh failed:"; sed 's/^/        /' "$LOGS/install.out"; exit 1
fi
t0=$(now); until [ "$(state_word)" = armed ]; do under "$(since "$t0")" 30 || break; sleep 0.5; done
[ "$(state_word)" = armed ] && pass "the stand-in is armed" || { failed "the stand-in is $(state_word), not armed"; exit 1; }

start_probe
await_owner daemon 10 || { failed "the probe never saw the daemon at the path"; exit 1; }
sleep 2

echo "== 1. the supervisor's job is booted out"
# launchctl bootout returns once the job has exited, which can take the
# supervisor's drain; the turn goes out as soon as the stand-in holds the path.
t0=$(now)
( lc bootout "$DOM/$SUP"; echo "$? $(since "$t0")" > "$S/bootout" ) &
BOOTOUT_PID=$!
await_owner standin 10 && send_turn 1
check_hand_over "boot out" daemon standin "$t0" 10 5
wait "$BOOTOUT_PID"
read -r rc took < "$S/bootout"
[ "$rc" = 0 ] && echo "    the bootout returned after $took s" || failed "the bootout of the scratch supervisor failed (exit $rc)"
if [ -n "${TURN_PID:-}" ]; then
    wait "$TURN_PID"
    code=$(cat "$S/turn-1.code" 2>/dev/null) took=$(cat "$S/turn-1.took" 2>/dev/null)
    if [ "$code" = 200 ] && grep -q "$TURN_MARK" "$S/turn-1.body" && grep -q "a box answered.*$TURN_SID" "$SB_DIR/standby.log" && under 25 "$took"; then
        pass "one real turn was answered by box $(sed -n "s/.*a box answered.*box=\([^ ]*\).*$TURN_SID.*/\1/p" "$SB_DIR/standby.log" | head -1) after the hold, $took s after it was sent"
    else
        failed "the turn: HTTP ${code:-none} after ${took:-?} s (want 200 after the 30 s hold), marker $(grep -c "$TURN_MARK" "$S/turn-1.body" 2>/dev/null) times, $(head -c 300 "$S/turn-1.err" "$S/turn-1.body" 2>/dev/null | tr '\n' ' ')"
    fi
else
    failed "no turn was sent: the stand-in never held the path"
fi
echo "    the stand-in's state: $(state_word)"

echo "== 2. the supervisor's job is bootstrapped again"
t0=$(now)
lc bootstrap "$DOM" "$SUP_PLIST" || failed "could not load the scratch supervisor again"
await_owner daemon 60
check_hand_over "bootstrap" standin daemon "$t0" 60 10
sleep 3

echo "== 3. the supervisor yields the socket for 60 s"
t0=$(now)
answer=$(curl -s -m 10 -X POST --unix-socket "$CONTROL" 'http://supervisor/_subrouter/claude-socket/yield?for=60s')
case "$answer" in *'"yielded"'*) ;; *) failed "the yield answered: $(printf '%s' "$answer" | head -c 200)" ;; esac
await_owner standin 10
check_hand_over "yield" daemon standin "$t0" 10 5
t1=$(/usr/bin/perl -e 'printf "%.3f", $ARGV[0] + 60' -- "$t0")
await_owner daemon 100
check_hand_over "the yield's end" standin daemon "$t1" 40 10
echo "    the stand-in's state: $(state_word)"

standby_installer status > "$LOGS/status.out" 2>&1
if grep -q '^state: armed since' "$LOGS/status.out" && grep -q '^box [^:]*: token ok at ' "$LOGS/status.out"; then
    pass "install-standby.sh status reads the stand-in's own state file: armed, and a box's token ok"
else
    failed "install-standby.sh status did not show an armed stand-in with a box's token ok"
fi
sed 's/^/    /' "$LOGS/status.out"
echo "  probe: $(wc -l < "$PROBE" | tr -d ' ') answers; $(grep -c ' none$' "$PROBE") failed dials; $(grep -c ' standin$' "$PROBE") from the stand-in"
echo "  the stand-in's last events:"
grep -E 'standby: (took|holding|serving|gave|letting|a box answered)' "$SB_DIR/standby.log" | tail -12 | sed 's/^/    /'
