#!/usr/bin/env bash
# Tests for macos/subrouter-watchdog.sh and macos/install-watchdog.sh
# (PATCHES §81).
#
#   bash ops/deploy/tests/watchdog-test.sh              every case
#   bash ops/deploy/tests/watchdog-test.sh <substring>  matching cases
#   bash ops/deploy/tests/watchdog-test.sh '=<case>'    exactly that case (quote it in zsh)
#
# Both scripts reach launchctl and osascript only through
# SUBROUTER_WATCHDOG_LAUNCHCTL and SUBROUTER_WATCHDOG_OSASCRIPT. Here those are
# stand-ins in the test dir: a launchd that keeps its jobs in files, and a
# notifier that writes each notification down. The `date` and `sleep` on PATH
# are a clock the test moves: each sleep adds its seconds, so a run of checks
# takes no real time. Every label is a scratch one, and a case fails if a
# stand-in is ever handed the live job's label.
#
# Runs under macOS /bin/bash 3.2 as well as bash 5 (WATCHDOG_TEST_BASH picks
# the bash the scripts run under). On macOS it runs only inside a sandbox that
# refuses exec of launchctl, as the deploy cases do: use
# `ops/bin/sr-local-ci --docker deploy`.
set -u

HERE=$(cd "$(dirname "$0")" && pwd -P)
WATCHDOG="$HERE/../macos/subrouter-watchdog.sh"
INSTALLER="$HERE/../macos/install-watchdog.sh"
FILTER="${1:-}"
WD_BASH="${WATCHDOG_TEST_BASH:-}"
[ -n "$WD_BASH" ] || { WD_BASH=/bin/bash; [ -x "$WD_BASH" ] || WD_BASH=$(command -v bash); }

if [ "$(uname)" = Darwin ] && [ "${SR_LOCAL_CI_SANDBOX:-}" != 1 ] && [ -z "${WATCHDOG_TESTS_SANDBOXED:-}" ]; then
  if [ "${DEPLOY_TESTS_SANDBOX:-}" = 1 ]; then
    WATCHDOG_TESTS_SANDBOXED=1 exec sandbox-exec -p \
      '(version 1)(allow default)(deny process-exec (literal "/bin/launchctl") (literal "/usr/bin/osascript"))' \
      "$BASH" "$0" "$@"
  fi
  echo "refusing to run the watchdog cases on macOS outside a sandbox (PATCHES §76): run ops/bin/sr-local-ci --docker deploy" >&2
  exit 2
fi

T0=$(mktemp -d "${TMPDIR:-/tmp}/watchdog-tests.XXXXXX") && [ -d "$T0" ] || { echo "mktemp failed" >&2; exit 2; }
T=$(cd "$T0" && pwd -P) && [ -d "$T" ] && [ "$T" != / ] || { echo "cannot resolve $T0" >&2; exit 2; }
trap 'rm -rf "$T"' EXIT
UIDN=$(id -u)
START=1800000000
PFX="test.subrouter.wdtests.$$"

PASS=0; FAILED=0; FAIL_LIST=""; CASE=""; C=""; RC=0
ok() { PASS=$((PASS + 1)); }
bad() {
  FAILED=$((FAILED + 1))
  case " $FAIL_LIST " in *" $CASE "*) ;; *) FAIL_LIST="$FAIL_LIST $CASE" ;; esac
  echo "  FAIL $CASE: $1"
}
check() { # what command...
  local what="$1"; shift
  if "$@"; then ok; else bad "$what"; fi
}

# The stand-in launchd. A loaded job is a file jobs/<label> that holds the
# plist it was loaded from. Knobs, as files in $FAKE_LD:
#   print-rc       print answers this rc (and print-msg) for every job
#   boot-fails     every bootstrap fails with exit 5 and this message, and
#                  boot-fails-left, when present, counts the failures left
make_stubs() {
  mkdir -p "$C/bin" "$C/ld/jobs" "$C/home"
  echo "$START" > "$C/clock"
  cat > "$C/bin/fake-launchctl" <<'SH'
#!/bin/bash
L="$FAKE_LD"
echo "launchctl $* @$(cat "$FAKE_CLOCK" 2>/dev/null)" >> "$L/calls"
for a in "$@"; do
  b=${a##*/}; b=${b%.plist}
  if [ "$b" = ai.manaflow.subrouter ]; then echo "launchctl $*" >> "$L/live"; exit 99; fi
done
label_of() { tr -d '\n\t ' < "$1" | sed -n 's:.*<key>Label</key><string>\([^<]*\)</string>.*:\1:p'; }
case "$1" in
  print)
    if [ -e "$L/print-rc" ]; then [ ! -e "$L/print-msg" ] || cat "$L/print-msg" >&2; exit "$(cat "$L/print-rc")"; fi
    j="$L/jobs/${2##*/}"
    [ -e "$j" ] || { echo "Could not find service \"${2##*/}\" in domain for user gui: ${2#gui/}" >&2; exit 113; }
    printf '%s = {\n\tactive count = 1\n\tpath = %s\n\tstate = running\n\tpid = 4242\n}\n' "$2" "$(cat "$j")" ;;
  bootstrap)
    if [ -e "$L/boot-fails" ]; then
      left=$(cat "$L/boot-fails-left" 2>/dev/null || echo forever)
      if [ "$left" != 0 ]; then
        [ "$left" = forever ] || echo $((left - 1)) > "$L/boot-fails-left"
        cat "$L/boot-fails" >&2; exit 5
      fi
    fi
    [ -f "$3" ] || { echo "Bootstrap failed: 2: No such file or directory" >&2; exit 2; }
    label=$(label_of "$3")
    [ -e "$L/jobs/$label" ] && { echo "Bootstrap failed: 5: Input/output error" >&2; exit 5; }
    echo "$3" > "$L/jobs/$label" ;;
  bootout)
    [ -e "$L/jobs/${2##*/}" ] || { echo "Boot-out failed: 113: Could not find service" >&2; exit 113; }
    rm -f "$L/jobs/${2##*/}" ;;
esac
exit 0
SH
  cat > "$C/bin/fake-osascript" <<'SH'
#!/bin/bash
for a in "$@"; do
  case "$a" in *ai.manaflow.subrouter[!-]*|*ai.manaflow.subrouter) echo "osascript $*" >> "$FAKE_LD/live"; exit 99 ;; esac
done
prev=""; last=""
for a in "$@"; do prev=$last; last=$a; done
printf '%s\n' "$prev" >> "$FAKE_LD/notes"
SH
  cat > "$C/bin/date" <<'SH'
#!/bin/bash
[ "${1:-}" = +%s ] && { cat "$FAKE_CLOCK"; exit 0; }
exec /bin/date "$@"
SH
  cat > "$C/bin/sleep" <<'SH'
#!/bin/bash
n=$(cat "$FAKE_CLOCK"); s=${1%%.*}; echo $((n + ${s:-0})) > "$FAKE_CLOCK"
SH
  # A launchctl on PATH that only says it was found there.
  cat > "$C/bin/launchctl" <<'SH'
#!/bin/bash
echo "launchctl from PATH: $*" >> "$FAKE_LD/path-launchctl"; exit 0
SH
  chmod 755 "$C/bin/"*
  : > "$C/ld/calls"; : > "$C/ld/notes"
}

begin() {
  CASE="$1"
  case "$FILTER" in
    "") ;;
    =*) [ "$CASE" = "${FILTER#=}" ] || return 1 ;;
    *) [ "${CASE#*"$FILTER"}" != "$CASE" ] || return 1 ;;
  esac
  C="$T/$CASE"
  make_stubs
  JOB="$PFX.job"
  PLIST="$C/agents/$JOB.plist"
  STOPS="$C/home/.subrouter"
  mkdir -p "$C/agents" "$STOPS"
  write_job_plist "$PLIST" "$JOB"
  return 0
}

finish() {
  if [ -s "$C/ld/live" ]; then bad "a stand-in was handed the live job's label: $(head -1 "$C/ld/live")"; fi
  if [ -s "$C/ld/path-launchctl" ]; then bad "a script found launchctl on PATH: $(head -1 "$C/ld/path-launchctl")"; fi
  case " $FAIL_LIST " in
    *" $CASE "*)
      echo "  --- $CASE out ---"; tail -15 "$C/out" 2>/dev/null | sed 's/^/  | /'
      echo "  --- $CASE launchctl calls ---"; tail -15 "$C/ld/calls" | sed 's/^/  | /'
      echo "  --- $CASE watchdog log ---"; tail -15 "$C/wd.log" 2>/dev/null | sed 's/^/  | /' ;;
    *) echo "  ok   $CASE" ;;
  esac
}

write_job_plist() { # path label
  printf '<?xml version="1.0" encoding="UTF-8"?>\n<plist version="1.0"><dict><key>Label</key><string>%s</string><key>ProgramArguments</key><array><string>/bin/sleep</string><string>100000</string></array></dict></plist>\n' "$2" > "$1"
}
load_job() { echo "${2:-$PLIST}" > "$C/ld/jobs/${1:-$JOB}"; }
loaded() { [ -e "$C/ld/jobs/${1:-$JOB}" ]; }
clock() { cat "$C/clock"; }
advance() { echo $(( $(clock) + $1 )) > "$C/clock"; }

envs() {
  echo PATH="$C/bin:/usr/bin:/bin:/usr/sbin:/sbin"
  echo HOME="$C/home"
  echo TMPDIR="$C"
  echo FAKE_LD="$C/ld"
  echo FAKE_CLOCK="$C/clock"
  echo SUBROUTER_WATCHDOG_LAUNCHCTL="$C/bin/fake-launchctl"
  echo SUBROUTER_WATCHDOG_OSASCRIPT="$C/bin/fake-osascript"
}
in_env() { # [--bg] [VAR=value | -u VAR]... -- command...  (--bg: BG is its pid)
  local e=() line drop="" bg=0
  [ "$1" != --bg ] || { bg=1; shift; }
  while [ $# -gt 0 ] && [ "$1" != -- ]; do
    case "$1" in -u) drop="$drop $2"; shift 2 ;; *) e+=("$1"); shift ;; esac
  done
  shift
  while IFS= read -r line; do
    case " $drop " in *" ${line%%=*} "*) continue ;; esac
    e=("$line" ${e[@]+"${e[@]}"})
  done <<EOF
$(envs)
EOF
  if [ "$bg" = 1 ]; then env -i "${e[@]}" "$@" & BG=$!; else env -i "${e[@]}" "$@"; fi
}
# Runs the watchdog for N checks against the one scratch job.
watch() { # checks [env assignments...] [-- extra args]
  local n=$1 e=() extra=()
  shift
  while [ $# -gt 0 ] && [ "$1" != -- ]; do e+=("$1"); shift; done
  [ $# -gt 0 ] && { shift; extra=("$@"); }
  in_env ${e[@]+"${e[@]}"} -- "$WD_BASH" "$WATCHDOG" run --no-daemon --watch "$JOB=$PLIST" \
    --log "$C/wd.log" --stops "$STOPS" --checks "$n" ${extra[@]+"${extra[@]}"} > "$C/out" 2>&1
  RC=$?
}
wd() { in_env -- "$WD_BASH" "$WATCHDOG" "$@" > "$C/out" 2>&1; RC=$?; }
installer() { in_env -- "$WD_BASH" "$INSTALLER" "$@" > "$C/out" 2>&1; RC=$?; }

count() { # regex file
  local n; n=$(grep -cE -- "$1" "$2" 2>/dev/null); echo "${n:-0}"; }
calls() { count "$1" "$C/ld/calls"; }
notes() { count . "$C/ld/notes"; }
events() { count "$1" "$C/wd.log"; }
expect_rc() { [ "$RC" = "$1" ] && ok || bad "exit $RC, want $1"; }
expect_out() { grep -Fq -- "$1" "$C/out" && ok || bad "output lacks: $1"; }
expect_line() { grep -Fxq -- "$1" "$C/out" && ok || bad "output lacks the line: $1"; }
# The program arguments of the plist a dry run prints, one line.
planned_args() { sed -n '/<key>ProgramArguments<\/key>/,/<\/array>/s:^[[:space:]]*<string>\(.*\)</string>$:\1:p' "$C/out" | tr '\n' ' '; }
expect_calls() { local n; n=$(calls "$1"); [ "$n" = "$2" ] && ok || bad "$n launchctl calls match /$1/, want $2"; }
expect_notes() { local n; n=$(notes); [ "$n" = "$1" ] && ok || bad "$n notifications, want $1"; }
expect_events() { local n; n=$(events "$1"); [ "$n" = "$2" ] && ok || bad "$n log lines match /$1/, want $2"; }
expect_no_change() { expect_calls " (bootstrap|bootout|kickstart|enable|disable|remove|unload|load) " 0; }
bootstrap_times() { sed -n 's/^launchctl bootstrap .* @\([0-9]*\)$/\1/p' "$C/ld/calls"; }

echo "== the watchdog (under $WD_BASH)"

if begin unloaded-job-is-loaded-once; then
  m0=$(/bin/date '+%Y-%m-%dT%H:%M'); watch 6; m1=$(/bin/date '+%Y-%m-%dT%H:%M')
  expect_rc 0
  # Every event line starts with the time it was logged.
  n=$(grep -cvE "^($m0|$m1):[0-9]{2}[-+][0-9]{4} " "$C/wd.log")
  [ "$n" = 0 ] && ok || bad "$n log lines do not start with the time they were logged, as: $(grep -vE "^($m0|$m1):" "$C/wd.log" | head -1)"
  expect_calls "^launchctl bootstrap gui/$UIDN $PLIST @" 1
  expect_calls " (bootout|kickstart) " 0
  check "the job is not loaded afterwards" loaded
  expect_notes 1
  grep -Fq "$JOB" "$C/ld/notes" && ok || bad "the notification does not name the job"
  expect_events "$JOB is not loaded, and no stop is announced" 1
  finish
fi

if begin loaded-job-is-left-alone; then
  # However long it runs, a loaded job gets nothing but a print per check,
  # and the log gets nothing but the start line.
  load_job
  watch 30
  expect_rc 0
  expect_calls "^launchctl print gui/$UIDN/$JOB @" 30
  expect_no_change
  expect_notes 0
  n=$(grep -c . "$C/wd.log"); [ "$n" = 1 ] && ok || bad "the log has $n lines for a quiet run, want the start line alone"
  finish
fi

if begin unexpired-marker-is-respected; then
  wd hold "$JOB" 600 --stops "$STOPS" --by "a test"
  expect_rc 0
  watch 12
  expect_no_change
  expect_notes 0
  expect_events "a test announced the stop until .*leaving it alone" 1
  finish
fi

if begin expired-marker-is-ignored; then
  wd hold "$JOB" 60 --stops "$STOPS" --by "a test"
  advance 61
  watch 4
  expect_calls "^launchctl bootstrap gui/$UIDN $PLIST @" 1
  expect_events "the stop announced by a test ended at .*ignoring its marker" 1
  expect_notes 1
  finish
fi

if begin marker-expiring-mid-run-ends-the-hold; then
  # Held for 12 s, checked every 5: left alone at 0, 5 and 10, loaded at 15.
  wd hold "$JOB" 12 --stops "$STOPS"
  watch 8
  expect_calls "^launchctl bootstrap " 1
  t=$(bootstrap_times | head -1)
  [ -n "$t" ] && [ "$t" -ge $((START + 12)) ] && [ "$t" -le $((START + 17)) ] && ok \
    || bad "bootstrapped at clock ${t:-never}, want between $((START + 12)) and $((START + 17))"
  finish
fi

if begin ignored-markers-are-logged-once; then
  # An expired marker, one for another job, one with no expiry, one more than
  # a day ahead and one that cannot be read are all ignored: none can switch
  # the watchdog off. The bootstrap keeps failing, so the job stays down and
  # every check reads the marker again; each is still logged once.
  echo "Bootstrap failed: 5: Input/output error" > "$C/ld/boot-fails"
  for kind in expired other-label no-until too-far unreadable; do
    rm -f "$C/wd.log"; : > "$C/ld/calls"
    m="$STOPS/watchdog-stop.$JOB"; rm -rf "$m"
    t0=$(clock)
    case "$kind" in
      expired) printf 'label=%s\nuntil=%s\nby=a test\n' "$JOB" $((t0 - 1)) > "$m"; why="the stop announced by a test ended at" ;;
      other-label) printf 'label=%s.other\nuntil=%s\n' "$JOB" $((t0 + 600)) > "$m"; why="names the job '$JOB.other'" ;;
      no-until) printf 'label=%s\nuntil=soon\n' "$JOB" > "$m"; why="holds no expiry time" ;;
      too-far) printf 'label=%s\nuntil=%s\n' "$JOB" $((t0 + 10 * 86400)) > "$m"; why="names an expiry more than 86400 s ahead" ;;
      unreadable) mkdir "$m"; why="cannot be read" ;;
    esac
    watch 8
    n=$(calls "^launchctl print gui/$UIDN/$JOB @"); [ "$n" = 8 ] && ok || bad "$kind: $n checks, want 8"
    t=$(bootstrap_times | head -1)
    [ "$t" = "$t0" ] && ok || bad "$kind: first bootstrap at clock ${t:-never}, want the first check, $t0"
    n=$(events "$why"); [ "$n" = 1 ] && ok || bad "$kind: $n log lines say '$why' over 8 checks, want 1"
  done
  finish
fi

if begin job-from-another-plist-is-reported-only; then
  mkdir -p "$C/elsewhere"; write_job_plist "$C/elsewhere/$JOB.plist" "$JOB"
  load_job "$JOB" "$C/elsewhere/$JOB.plist"
  watch 10
  expect_no_change
  expect_notes 1
  expect_events "$JOB is loaded from $C/elsewhere/$JOB.plist, not from $PLIST" 1
  # Loaded from its own plist again, it gets nothing.
  load_job
  watch 3
  expect_no_change
  finish
fi

if begin missing-plist-is-reported-only; then
  rm -f "$PLIST"
  watch 10
  expect_no_change
  expect_notes 1
  expect_events "its plist $PLIST is missing" 1
  finish
fi

if begin failed-bootstrap-backs-off; then
  # The first three bootstraps fail. They are tried 10, 20 and 40 s apart,
  # not at every 5 s check, and the fourth works.
  echo "Bootstrap failed: 5: Input/output error" > "$C/ld/boot-fails"; echo 3 > "$C/ld/boot-fails-left"
  watch 30
  expect_calls "^launchctl bootstrap " 4
  check "the job is not loaded afterwards" loaded
  gaps=""; prev=""
  for t in $(bootstrap_times); do [ -n "$prev" ] && gaps="$gaps $((t - prev))"; prev=$t; done
  [ "$gaps" = " 10 20 40" ] && ok || bad "the tries came${gaps:- never} s apart, want 10 20 40"
  expect_events "failed \(rc 5: Bootstrap failed: 5: Input/output error\); trying again in 10s" 1
  # One notification for the failure, one for the recovery: not one per try.
  expect_notes 2
  expect_events "loaded again from $PLIST, after 3 failed tries" 1
  # A bootstrap that never works is tried less and less often, up to every
  # 300 s: 10 tries in half an hour (at 0, 10, 30, 70, 150, 310, 610, 910,
  # 1210 and 1510 s), where a try at every check would be 360.
  rm -f "$C/ld/boot-fails-left" "$C/wd.log"; : > "$C/ld/calls"; : > "$C/ld/notes"; rm -f "$C/ld/jobs/"*
  watch 360
  expect_calls "^launchctl bootstrap " 10
  expect_notes 1
  finish
fi

if begin unclear-launchd-answer-changes-nothing; then
  # rc 141 is launchd refusing this shell, not a job that is gone.
  echo 141 > "$C/ld/print-rc"; echo "Could not print domain: 141: Reentrancy avoided" > "$C/ld/print-msg"
  watch 6
  expect_no_change
  expect_notes 1
  expect_events "answered rc 141 \(Could not print domain: 141: Reentrancy avoided\)" 1
  finish
fi

if begin launchctl-is-never-looked-up-on-path; then
  # Named without a path, it is refused before any check.
  rm -f "$PLIST"
  watch 2 SUBROUTER_WATCHDOG_LAUNCHCTL=launchctl
  expect_rc 2; expect_out "never looked up on PATH"
  expect_calls . 0
  watch 2 SUBROUTER_WATCHDOG_OSASCRIPT=osascript
  expect_rc 2; expect_out "never looked up on PATH"
  # Unset, it is /bin/launchctl, even with a launchctl first on PATH (that one
  # fails the case in finish). The plist is missing, so even a real launchctl
  # would be asked only to print a scratch label.
  watch 2 -u SUBROUTER_WATCHDOG_LAUNCHCTL
  expect_rc 0
  expect_events "started .*launchctl /bin/launchctl" 1
  finish
fi

if begin default-watch-is-the-daemon-and-watch-adds; then
  # The defaults are read without a launchctl call (the stand-in would fail
  # the case on the live label).
  wd config
  expect_rc 0
  expect_line "watch ai.manaflow.subrouter $C/home/Library/LaunchAgents/ai.manaflow.subrouter.plist"
  expect_line "log $C/home/Library/Logs/subrouter-watchdog.log"
  expect_line "stops $C/home/.subrouter"
  expect_line "interval 5"
  wd config --watch "$JOB=$PLIST"
  expect_out "watch ai.manaflow.subrouter "; expect_line "watch $JOB $PLIST"
  wd config --watch "$JOB=$PLIST" --no-daemon
  n=$(grep -c '^watch ' "$C/out"); [ "$n" = 1 ] && ok || bad "--no-daemon left $n watched jobs, want 1"
  wd config --no-daemon --watch "$JOB=$PLIST" --watch "$JOB=$PLIST"
  expect_rc 2; expect_out "watched twice"
  wd config --no-daemon --watch "$JOB=agents/x.plist"
  expect_rc 2; expect_out "absolute"
  wd config --no-daemon
  expect_rc 2; expect_out "nothing to watch"
  expect_calls . 0
  finish
fi

if begin hold-and-release-write-and-remove-the-marker; then
  wd hold "$JOB" 090 --stops "$STOPS" --by "deploy-laptop.sh (pid 7)"
  expect_rc 0
  m="$STOPS/watchdog-stop.$JOB"
  grep -qx "label=$JOB" "$m" && ok || bad "the marker does not hold label=$JOB"
  grep -qx "until=$((START + 90))" "$m" && ok || bad "the marker does not hold until=$((START + 90)): $(cat "$m" 2>/dev/null)"
  grep -qx "by=deploy-laptop.sh (pid 7)" "$m" && ok || bad "the marker does not say who announced it"
  wd release "$JOB" --stops "$STOPS"
  expect_rc 0
  check "release removed the marker" test ! -e "$m"
  wd release "$JOB" --stops "$STOPS"
  expect_rc 0
  wd hold "$JOB" 86401 --stops "$STOPS"
  expect_rc 2; check "a hold over a day wrote no marker" test ! -e "$m"
  wd hold "$JOB/x" 60 --stops "$STOPS"
  expect_rc 2
  wd hold "$JOB" soon --stops "$STOPS"
  expect_rc 2
  finish
fi

if begin a-signal-stops-it-at-once; then
  load_job
  # env execs the watchdog, so BG is the watchdog's own pid.
  in_env --bg PATH="/usr/bin:/bin" -- "$WD_BASH" "$WATCHDOG" run --no-daemon --watch "$JOB=$PLIST" \
    --log "$C/wd.log" --stops "$STOPS" --interval 3600 > "$C/out" 2>&1
  pid=$BG
  for _ in $(seq 1 100); do [ "$(calls "^launchctl print ")" -ge 1 ] && break; /bin/sleep 0.1; done
  if [ "$(calls "^launchctl print ")" -lt 1 ]; then
    bad "the watchdog made no check in 10 s"
  else
    /bin/sleep 0.5
    kill -TERM "$pid"
    for _ in $(seq 1 30); do kill -0 "$pid" 2>/dev/null || break; /bin/sleep 0.1; done
    if kill -0 "$pid" 2>/dev/null; then bad "still running 3 s after SIGTERM, mid-sleep"; else ok; fi
  fi
  kill -KILL "$pid" 2>/dev/null
  wait "$pid" 2>/dev/null
  expect_events "stopped by a signal" 1
  finish
fi

if begin log-stays-small; then
  awk 'BEGIN { for (i = 0; i < 3000; i++) printf "%0100d\n", i }' > "$C/wd.log"
  watch 2
  check "the full log was moved aside" test -s "$C/wd.log.1"
  n=$(wc -c < "$C/wd.log"); [ $((n)) -lt 4096 ] && ok || bad "the log is $((n)) bytes after it was moved aside"
  finish
fi

if begin stand-ins-catch-the-live-label; then
  # The instrument itself: the live label reaches neither stand-in unseen.
  in_env -- "$C/bin/fake-launchctl" print "gui/$UIDN/ai.manaflow.subrouter" > /dev/null 2>&1
  check "the launchctl stand-in saw the live label" test -s "$C/ld/live"
  : > "$C/ld/live"
  in_env -- "$C/bin/fake-launchctl" bootstrap "gui/$UIDN" "$C/home/Library/LaunchAgents/ai.manaflow.subrouter.plist" > /dev/null 2>&1
  check "the launchctl stand-in saw the live plist" test -s "$C/ld/live"
  : > "$C/ld/live"
  in_env -- "$C/bin/fake-osascript" -e x "ai.manaflow.subrouter was not loaded." t > /dev/null 2>&1
  check "the notifier stand-in saw the live label" test -s "$C/ld/live"
  : > "$C/ld/live"
  in_env -- "$C/bin/fake-launchctl" print "gui/$UIDN/ai.manaflow.subrouter-watchdog" > /dev/null 2>&1
  check "the watchdog's own label is not the live job's" test ! -s "$C/ld/live"
  : > "$C/ld/calls"
  finish
fi

echo "== the installer (under $WD_BASH)"

WDJOB_SUFFIX=wd
scratch_install() { # extra args...
  installer install --label "$PFX.$WDJOB_SUFFIX" --dir "$C/wdhome" --no-daemon --watch "$JOB=$PLIST" "$@"
}
WDPLIST() { echo "$C/wdhome/$PFX.$WDJOB_SUFFIX.plist"; }

if begin installer-refuses-the-daemon-label; then
  installer install --label ai.manaflow.subrouter --dir "$C/wdhome" --no-daemon --watch "$JOB=$PLIST"
  expect_rc 2; expect_out "the daemon's own job"
  installer uninstall --label ai.manaflow.subrouter --dir "$C/wdhome"
  expect_rc 2
  expect_calls . 0
  check "nothing was written" test ! -e "$C/wdhome"
  finish
fi

if begin installer-refuses-a-foreign-label; then
  # Each attempt would be a valid install under the real label, so the label
  # is the only thing left to refuse.
  for label in com.example.watchdog ai.manaflow.subrouter-watchdog2 test.subrouterx.wd; do
    installer install --label "$label" --dir "$C/wdhome" --watch "$JOB=$PLIST"
    [ "$RC" = 2 ] && ok || bad "label $label: exit $RC, want 2"
    expect_out "refusing the label $label:"
  done
  # A scratch prefix does not make any text a label.
  installer install --label "test.subrouter.wd/x" --dir "$C/wdhome" --no-daemon --watch "$JOB=$PLIST"
  expect_rc 2; expect_out "refusing 'test.subrouter.wd/x': not a launchd label"
  expect_calls . 0
  check "nothing was written" test ! -e "$C/wdhome"
  check "nothing was written under LaunchAgents" test ! -e "$C/home/Library/LaunchAgents"
  finish
fi

if begin installer-scratch-watchdog-watches-only-scratch-jobs; then
  installer install --label "$PFX.wd" --dir "$C/wdhome" --watch "$JOB=$PLIST"
  expect_rc 2; expect_out "--no-daemon"
  installer install --label "$PFX.wd" --dir "$C/wdhome" --no-daemon --watch "com.example.job=$PLIST"
  expect_rc 2
  installer install --label "$PFX.wd" --no-daemon --watch "$JOB=$PLIST"
  expect_rc 2; expect_out "--dir"
  expect_calls . 0
  check "nothing was written" test ! -e "$C/wdhome"
  finish
fi

if begin installer-installs-reports-reinstalls-and-uninstalls; then
  scratch_install
  expect_rc 0
  P=$(WDPLIST)
  check "the plist was written" test -f "$P"
  check "the watchdog was copied" test -f "$C/wdhome/subrouter-watchdog.sh"
  cmp -s "$WATCHDOG" "$C/wdhome/subrouter-watchdog.sh" && ok || bad "the copy differs from the source"
  expect_calls "^launchctl bootstrap gui/$UIDN $P @" 1
  check "launchd has the watchdog job" loaded "$PFX.wd"
  if command -v python3 > /dev/null 2>&1; then
    got=$(python3 - "$P" <<'PY'
import plistlib, sys
p = plistlib.load(open(sys.argv[1], "rb"))
print(p["Label"], p["KeepAlive"], p["RunAtLoad"])
print(" ".join(p["ProgramArguments"]))
print(p["EnvironmentVariables"]["PATH"])
PY
)
    want="$PFX.wd True True
/bin/bash $C/wdhome/subrouter-watchdog.sh run --no-daemon --watch $JOB=$PLIST --log $C/wdhome/watchdog.log --stops $C/wdhome
/usr/bin:/bin:/usr/sbin:/sbin"
    [ "$got" = "$want" ] && ok || bad "the plist reads as
$got
want
$want"
  fi
  installer status --label "$PFX.wd" --dir "$C/wdhome"
  expect_rc 0; expect_out "loaded from $P"
  # Again: the running watchdog is booted out and loaded from the new copy.
  scratch_install
  expect_rc 0
  expect_calls "^launchctl bootout gui/$UIDN/$PFX.wd @" 1
  expect_calls "^launchctl bootstrap gui/$UIDN $P @" 2
  installer uninstall --label "$PFX.wd" --dir "$C/wdhome"
  expect_rc 0
  expect_calls "^launchctl bootout gui/$UIDN/$PFX.wd @" 2
  check "launchd no longer has the watchdog job" test ! -e "$C/ld/jobs/$PFX.wd"
  check "the plist was removed" test ! -e "$P"
  check "the copy was removed" test ! -e "$C/wdhome/subrouter-watchdog.sh"
  # It addressed only its own label.
  n=$(grep -vE " gui/$UIDN/$PFX\.wd @| gui/$UIDN $P @" "$C/ld/calls" | grep -c .)
  [ "$n" = 0 ] && ok || bad "launchctl was called for another job: $(grep -v "$PFX.wd" "$C/ld/calls" | head -1)"
  check "the watched job was not touched" test ! -e "$C/ld/jobs/$JOB"
  finish
fi

if begin installer-refuses-its-label-loaded-from-another-plist; then
  load_job "$PFX.wd" "$C/elsewhere.plist"
  scratch_install
  expect_rc 2; expect_out "$C/elsewhere.plist"
  installer uninstall --label "$PFX.wd" --dir "$C/wdhome"
  expect_rc 2
  expect_no_change
  finish
fi

if begin installer-dry-run-changes-nothing; then
  scratch_install --dry-run
  expect_rc 0
  expect_out "would write $(WDPLIST):"
  expect_out "<key>KeepAlive</key>"
  expect_out "would run: $C/bin/fake-launchctl bootstrap gui/$UIDN $(WDPLIST)"
  check "nothing was written" test ! -e "$C/wdhome"
  expect_no_change
  # The real label's plan: its own plist under LaunchAgents, the daemon watched
  # by the watchdog's default, not named in the plist, its log under
  # ~/Library/Logs, and its stop markers in ~/.subrouter, where the deploy
  # scripts write theirs.
  installer install --dry-run
  expect_rc 0
  expect_out "would write $C/home/Library/LaunchAgents/ai.manaflow.subrouter-watchdog.plist:"
  expect_out "<string>ai.manaflow.subrouter-watchdog</string>"
  got=$(planned_args)
  want="/bin/bash $C/home/.subrouter/watchdog/subrouter-watchdog.sh run --log $C/home/Library/Logs/subrouter-watchdog.log --stops $C/home/.subrouter "
  [ "$got" = "$want" ] && ok || bad "the real plan runs
  $got
want
  $want"
  check "nothing was written under LaunchAgents" test ! -e "$C/home/Library/LaunchAgents"
  expect_no_change
  installer uninstall --dry-run
  expect_rc 0
  expect_no_change
  # The real label takes no scratch-only option.
  installer install --launch-agents "$C/agents" --dry-run
  expect_rc 2
  installer install --launchctl "$C/bin/fake-launchctl" --dry-run
  expect_rc 2
  finish
fi

if begin installed-watchdog-reads-the-markers-hold-writes; then
  # The deploy scripts announce a stop with `hold` and no --stops. A watchdog
  # run as the real install plans it must find that marker, or it loads the job
  # in the middle of a deploy.
  installer install --dry-run
  stops=$(planned_args | sed -n 's/.* --stops \([^ ]*\) .*/\1/p')
  [ -n "$stops" ] && ok || bad "the real plan names no --stops"
  wd hold "$JOB" 600 --by "a deploy"
  expect_rc 0
  watch 4 -- --stops "${stops:-/nowhere}"
  expect_no_change
  expect_events "a deploy announced the stop until .*leaving it alone" 1
  finish
fi

if begin installer-runs-launchctl-by-absolute-path; then
  # Named without a path, it is refused before any call.
  in_env SUBROUTER_WATCHDOG_LAUNCHCTL=launchctl -- "$WD_BASH" "$INSTALLER" status --label "$PFX.wd" --dir "$C/wdhome" > "$C/out" 2>&1; RC=$?
  expect_rc 2; expect_out "never looked up on PATH"
  # Unset, it is /bin/launchctl, even with a launchctl first on PATH (that one
  # fails the case in finish). Here /bin/launchctl is missing or fenced off,
  # and status shows what running it answered.
  in_env -u SUBROUTER_WATCHDOG_LAUNCHCTL -- "$WD_BASH" "$INSTALLER" status --label "$PFX.wd" --dir "$C/wdhome" > "$C/out" 2>&1; RC=$?
  expect_out "/bin/launchctl"
  expect_calls . 0
  finish
fi

if begin installer-refuses-what-the-watchdog-would-refuse; then
  # A job that exits at start is started again by launchd every 10 s for ever,
  # so the installer asks the watchdog first. A job watched twice is one such.
  scratch_install --watch "$JOB=$PLIST"
  expect_rc 2; expect_out "watched twice"
  expect_calls . 0
  check "nothing was written" test ! -e "$C/wdhome"
  finish
fi

echo
echo "watchdog tests: $PASS assertions passed, $FAILED failed"
if [ "$FAILED" -ne 0 ]; then
  echo "failing cases:$FAIL_LIST"
  exit 1
fi
[ "$PASS" -gt 0 ] || { echo "no case matched '$FILTER'" >&2; exit 1; }
exit 0
