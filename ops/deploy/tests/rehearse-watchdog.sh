#!/bin/bash
# Rehearse the watchdog (PATCHES §81) on this Mac's real launchd, with scratch
# labels only:
#
#   bash ops/deploy/tests/rehearse-watchdog.sh <scratch-dir>
#
# It loads a scratch job, installs a scratch watchdog that watches it, boots the
# job out three times and times how long until launchd has it again. Then it
# shows that an announced stop is left alone until it is withdrawn, that a stop
# that expires is not, and that a job loaded from another plist is left as it
# is. It prints PASS or FAIL per check and ends with one REHEARSAL line.
#
# Every launchctl call, its own and the scratch watchdog's, goes through a
# guard in <scratch-dir> that refuses any label not starting with
# test.subrouter.wdrehearsal., any plist outside <scratch-dir>, and every verb
# but print, bootstrap, bootout, list and managername. The live job's label is
# never named. Expect about six notifications from "subrouter watchdog".
set -u

[ "$(uname)" = Darwin ] || { echo "this rehearsal needs macOS launchd" >&2; exit 2; }
[ $# = 1 ] || { echo "usage: $0 <scratch-dir>" >&2; exit 2; }
HERE=$(cd "$(dirname "$0")" && pwd -P)
MACOS=$(cd "$HERE/../macos" && pwd -P)
mkdir -p "$1" && ROOT=$(cd "$1" && pwd -P) || exit 2
case "$ROOT/" in "$HOME/"|"$HOME/.subrouter/"*|"$HOME/.claude/"*|"$HOME/Library/"*|"$HOME/bin/"*|/)
    echo "refusing scratch dir $ROOT" >&2; exit 2 ;;
esac
PFX=test.subrouter.wdrehearsal.
SVC="${PFX}svc.$$"
WD="${PFX}wd.$$"
UIDN=$(id -u)
DOM="gui/$UIDN"
S="$ROOT/run.$$"
GUARD="$S/launchctl-guard"
SVC_PLIST="$S/$SVC.plist"
OTHER_PLIST="$S/other/$SVC.plist"
WDLOG="$S/wd/watchdog.log"
mkdir -p "$S/wd" "$S/other" || exit 2

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
loaded_from() { lc print "$DOM/$1" 2>/dev/null | sed -n 's/^	path = //p' | head -1; }
now() { /usr/bin/perl -MTime::HiRes=time -e 'printf "%.1f\n", time'; }
# `--`: a negative number after -e would be read as a perl switch.
since() { /usr/bin/perl -e 'printf "%.1f", $ARGV[1] - $ARGV[0]' -- "$1" "$(now)"; }
under() { /usr/bin/perl -e 'exit($ARGV[0] < $ARGV[1] ? 0 : 1)' -- "$1" "$2"; }
watchdog() { /bin/bash "$MACOS/subrouter-watchdog.sh" "$@"; }
installer() { SUBROUTER_WATCHDOG_LAUNCHCTL="$GUARD" /bin/bash "$MACOS/install-watchdog.sh" "$@"; }
await_loaded() { # label limit-seconds: true once loaded within the limit
    local t0; t0=$(now)
    until loaded "$1"; do under "$(since "$t0")" "$2" || return 1; sleep 0.1; done
}
boot_out() { # label: true once launchd has let it go
    local t0; t0=$(now)
    lc bootout "$DOM/$1" || return 1
    while loaded "$1"; do under "$(since "$t0")" 10 || return 1; sleep 0.05; done
}

PASS=0; FAILS=0
pass() { echo "  PASS  $1"; PASS=$((PASS + 1)); }
failed() { echo "  FAIL  $1"; FAILS=$((FAILS + 1)); }

cleanup() {
    installer uninstall --label "$WD" --dir "$S/wd" > /dev/null 2>&1
    lc bootout "$DOM/$SVC" > /dev/null 2>&1
    sleep 1
    local left; left=$(lc list 2>/dev/null | grep -c "$PFX")
    if [ "$left" = 0 ]; then pass "no scratch job is left in launchd"; else failed "$left scratch jobs are still in launchd"; fi
    [ ! -s "$S/refused" ] || failed "the guard refused $(wc -l < "$S/refused" | tr -d ' ') launchctl calls: $(head -1 "$S/refused")"
    echo "  the watchdog's log is kept at $WDLOG"
    if [ "$FAILS" = 0 ]; then echo "REHEARSAL PASS ($PASS checks)"; else echo "REHEARSAL FAIL ($FAILS of $((PASS + FAILS)) checks failed)"; fi
    [ "$FAILS" = 0 ]
}
trap 'cleanup; exit $?' EXIT
trap 'exit 130' INT TERM HUP

write_plist() { # path
    cat > "$1" <<P
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>$SVC</string>
<key>ProgramArguments</key><array><string>/bin/sleep</string><string>100000</string></array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
</dict></plist>
P
}

echo "== watchdog rehearsal: $SVC watched by $WD, in $S"
echo "   load average: $(sysctl -n vm.loadavg)"
[ "$(lc managername 2>&1)" = Aqua ] || { failed "this shell is not in the logged-in GUI session (launchctl managername is not Aqua); run it from a Terminal.app window"; exit 1; }
write_plist "$SVC_PLIST"; write_plist "$OTHER_PLIST"
lc bootstrap "$DOM" "$SVC_PLIST" || { failed "could not load the scratch job"; exit 1; }

if installer install --label "$WD" --dir "$S/wd" --no-daemon --watch "$SVC=$SVC_PLIST" --launchctl "$GUARD" > "$S/install.out" 2>&1; then
    pass "the scratch watchdog is installed ($(tail -1 "$S/install.out"))"
else
    failed "the installer failed:"; sed 's/^/        /' "$S/install.out"; exit 1
fi
n=0; until grep -q ' started ' "$WDLOG" 2>/dev/null; do n=$((n + 1)); [ $n -le 150 ] || break; sleep 0.1; done
grep -q ' started ' "$WDLOG" 2>/dev/null && pass "the watchdog runs" || { failed "the watchdog wrote no start line in 15 s"; exit 1; }
sleep 1

for trial in 1 2 3; do
    t0=$(now)
    boot_out "$SVC" || failed "trial $trial: the bootout did not unload the job"
    if await_loaded "$SVC" 60; then
        took=$(since "$t0")
        if under "$took" 10; then pass "trial $trial: booted out, and loaded again ${took} s later"
        else failed "trial $trial: loaded again only ${took} s later (want under 10)"; fi
    else
        failed "trial $trial: not loaded again within 60 s"
    fi
    sleep 2
done

watchdog hold "$SVC" 30 --stops "$S/wd" --by "the rehearsal" > /dev/null
boot_out "$SVC" || failed "held: the bootout did not unload the job"
t0=$(now); back=""
while under "$(since "$t0")" 15; do loaded "$SVC" && { back=$(since "$t0"); break; }; sleep 0.2; done
if [ -z "$back" ]; then pass "an announced stop is left alone for 15 s"; else failed "an announced stop was undone after ${back} s"; fi
watchdog release "$SVC" --stops "$S/wd"
t0=$(now)
if await_loaded "$SVC" 30; then
    took=$(since "$t0")
    if under "$took" 10; then pass "once the stop is withdrawn, loaded again ${took} s later"; else failed "withdrawn, loaded again only ${took} s later"; fi
else
    failed "withdrawn, and not loaded again within 30 s"
fi
sleep 2

# The marker ends at a whole second, up to 1 s before 12 s have passed, so the
# return is timed from the end the marker names.
watchdog hold "$SVC" 12 --stops "$S/wd" --by "the rehearsal" > /dev/null
ends=$(sed -n 's/^until=//p' "$S/wd/watchdog-stop.$SVC")
boot_out "$SVC" || failed "12 s stop: the bootout did not unload the job"
if await_loaded "$SVC" 60; then
    took=$(since "$ends")
    if under -0.5 "$took" && under "$took" 10; then pass "a 12 s stop is respected and then not: loaded again ${took} s after the marker ended"
    else failed "a 12 s stop ended with the job loaded again ${took} s after the marker ended (want 0 to 10)"; fi
else
    failed "a 12 s stop: not loaded again within 60 s"
fi
sleep 2

# Swapped under an announced stop, so the watchdog cannot load it first.
watchdog hold "$SVC" 60 --stops "$S/wd" --by "the rehearsal" > /dev/null
boot_out "$SVC" || failed "other plist: the bootout did not unload the job"
lc bootstrap "$DOM" "$OTHER_PLIST" || failed "could not load the scratch job from the other plist"
watchdog release "$SVC" --stops "$S/wd"
sleep 12
from=$(loaded_from "$SVC")
if [ "$from" = "$OTHER_PLIST" ]; then pass "a job loaded from another plist is left as it is for 12 s"; else failed "the job loaded from the other plist is now loaded from '${from:-nothing}'"; fi
grep -q "is loaded from $OTHER_PLIST, not from $SVC_PLIST" "$WDLOG" && pass "and the watchdog reported it" || failed "the watchdog did not report the other plist"

echo "  the watchdog's events:"
sed 's/^[^ ]* /    /' "$WDLOG"
echo "  notifications sent: $(grep -c '(notified)$' "$WDLOG")"
