#!/usr/bin/env bash
# Tests for macos/install-standby.sh (PATCHES §79.5).
#
#   bash ops/deploy/tests/standby-install-test.sh              every case
#   bash ops/deploy/tests/standby-install-test.sh <substring>  matching cases
#   bash ops/deploy/tests/standby-install-test.sh '=<case>'    exactly that case (quote it in zsh)
#
# The installer reaches launchctl only through SUBROUTER_STANDBY_LAUNCHCTL, and
# install-watchdog.sh, which it runs for the watchdog, through
# SUBROUTER_WATCHDOG_LAUNCHCTL. Here those are two stand-ins for one launchd
# that keeps its jobs in files. The installer's fails a case when it is handed
# the daemon's label or the watchdog's; the watchdog installer's fails one when
# it is handed any label but the watchdog's. The subrouter binary is a stand-in
# that answers `standby --check` from files the case writes. HOME is the case's
# own, with a daemon plist and the live job's stop marker in it, and a case
# fails when either changes. The `date` and `sleep` on PATH are a clock the
# case sets.
#
# Runs under macOS /bin/bash 3.2 as well as bash 5 (STANDBY_TEST_BASH picks
# the bash the installer runs under). On macOS it runs only inside a sandbox
# that refuses exec of launchctl, as the deploy cases do: use
# `ops/bin/sr-local-ci --docker deploy`.
set -u

HERE=$(cd "$(dirname "$0")" && pwd -P)
INSTALLER="$HERE/../macos/install-standby.sh"
FILTER="${1:-}"
SB_BASH="${STANDBY_TEST_BASH:-}"
[ -n "$SB_BASH" ] || { SB_BASH=/bin/bash; [ -x "$SB_BASH" ] || SB_BASH=$(command -v bash); }

if [ "$(uname)" = Darwin ] && [ "${SR_LOCAL_CI_SANDBOX:-}" != 1 ] && [ -z "${STANDBY_TESTS_SANDBOXED:-}" ]; then
  if [ "${DEPLOY_TESTS_SANDBOX:-}" = 1 ]; then
    STANDBY_TESTS_SANDBOXED=1 exec sandbox-exec -p \
      '(version 1)(allow default)(deny process-exec (literal "/bin/launchctl") (literal "/usr/bin/osascript"))' \
      "$BASH" "$0" "$@"
  fi
  echo "refusing to run the stand-in installer cases on macOS outside a sandbox (PATCHES §76): run ops/bin/sr-local-ci --docker deploy" >&2
  exit 2
fi

T0=$(mktemp -d "${TMPDIR:-/tmp}/standby-install-tests.XXXXXX") && [ -d "$T0" ] || { echo "mktemp failed" >&2; exit 2; }
T=$(cd "$T0" && pwd -P) && [ -d "$T" ] && [ "$T" != / ] || { echo "cannot resolve $T0" >&2; exit 2; }
trap 'rm -rf "$T"' EXIT
UIDN=$(id -u)
LABEL=ai.manaflow.subrouter-standby
# 2026-11-22T12:00:00Z
START=1795348800
TOKEN="srk_standin_TESTONLY_4f1c9a77e2b04d55a1c3e8f09b6d2a71"

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

# The stand-in launchd, one for each caller. A loaded job is a file
# jobs/<label> holding the plist it was loaded from; print gives the pid in
# ld/pid (4242). A bootout notes whether the job's stop marker was there.
make_stubs() {
  mkdir -p "$C/bin" "$C/ld/jobs" "$C/home/.subrouter" "$C/home/Library/LaunchAgents"
  echo "$START" > "$C/clock"
  cat > "$C/bin/sb-launchctl" <<'SH'
#!/bin/bash
L="$FAKE_LD"; me=${0##*/}
echo "$me $* @$(cat "$FAKE_CLOCK" 2>/dev/null)" >> "$L/calls"
for a in "$@"; do
  b=${a##*/}; b=${b%.plist}
  case "$me:$b" in
    *:ai.manaflow.subrouter) echo "$me $*" >> "$L/live"; exit 99 ;;
    sb-launchctl:ai.manaflow.subrouter-watchdog) echo "$me $*" >> "$L/live"; exit 99 ;;
    wd-launchctl:ai.manaflow.*|wd-launchctl:test.*) [ "$b" = ai.manaflow.subrouter-watchdog ] || { echo "$me $*" >> "$L/live"; exit 99; } ;;
  esac
done
label_of() { tr -d '\n\t ' < "$1" | sed -n 's:.*<key>Label</key><string>\([^<]*\)</string>.*:\1:p'; }
case "$1" in
  print)
    j="$L/jobs/${2##*/}"
    [ -e "$j" ] || { echo "Could not find service \"${2##*/}\" in domain for user gui: ${2#gui/}" >&2; exit 113; }
    printf '%s = {\n\tactive count = 1\n\tpath = %s\n\tstate = running\n\tpid = %s\n}\n' "$2" "$(cat "$j")" "$(cat "$L/pid" 2>/dev/null || echo 4242)" ;;
  bootstrap)
    [ -f "$3" ] || { echo "Bootstrap failed: 2: No such file or directory" >&2; exit 2; }
    label=$(label_of "$3")
    [ -e "$L/jobs/$label" ] && { echo "Bootstrap failed: 5: Input/output error" >&2; exit 5; }
    echo "$3" > "$L/jobs/$label" ;;
  bootout)
    label=${2##*/}
    [ -e "$L/jobs/$label" ] || { echo "Boot-out failed: 113: Could not find service" >&2; exit 113; }
    if [ -e "$HOME/.subrouter/watchdog-stop.$label" ]; then echo "marker at bootout of $label" >> "$L/markers"; fi
    rm -f "$L/jobs/$label" ;;
esac
exit 0
SH
  cp "$C/bin/sb-launchctl" "$C/bin/wd-launchctl"
  # The subrouter binary, as far as the installer runs it: `standby --check`.
  # It records what it was run with, since the installer clears the
  # environment for it.
  cat > "$C/bin/subrouter" <<SH
#!/bin/bash
L="$C/ld"
SH
  cat >> "$C/bin/subrouter" <<'SH'
{ printf 'args:'; printf ' %s' "$@"; printf '\n'; env | grep -E '^(HOME|PATH|SUBROUTER_[A-Z_]*)=' | sort; } > "$L/check-seen"
echo "subrouter $*" >> "$L/binary-calls"
[ ! -e "$L/no-check" ] || { echo "flag provided but not defined: -check" >&2; exit 2; }
for last; do :; done
[ "${1:-}" = standby ] && [ "${last:-}" = --check ] || { echo "the stand-in binary answers only standby ... --check" >&2; exit 3; }
[ ! -e "$L/check-stderr" ] || cat "$L/check-stderr" >&2
printf 'socket %s\ncontrol_socket %s\nstate_dir %s\n' "$HOME/.subrouter/claude.sock" "$HOME/.subrouter/supervisor.sock" "$HOME/.subrouter"
if [ -e "$L/check-out" ]; then cat "$L/check-out"; else
  printf 'peers_file ok %s\nbox box-a untested\nsupervisor serving waits_for_lock=true\nverdict armed\n' "$HOME/.subrouter/peers.json"
fi
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
  cat > "$C/bin/launchctl" <<'SH'
#!/bin/bash
echo "launchctl from PATH: $*" >> "$FAKE_LD/path-launchctl"; exit 0
SH
  chmod 755 "$C/bin/"*
  : > "$C/ld/calls"
}

begin() {
  CASE="$1"
  case "$FILTER" in
    "") ;;
    =*) [ "$CASE" = "${FILTER#=}" ] || return 1 ;;
    *) [ "${CASE#*"$FILTER"}" != "$CASE" ] || return 1 ;;
  esac
  C="$T/$CASE"
  H="$C/home"
  make_stubs
  PLIST="$H/Library/LaunchAgents/$LABEL.plist"
  COPY="$H/.subrouter/standby/subrouter-standby"
  WDPLIST="$H/Library/LaunchAgents/ai.manaflow.subrouter-watchdog.plist"
  DAEMON_PLIST="$H/Library/LaunchAgents/ai.manaflow.subrouter.plist"
  write_daemon_plist
  cp "$DAEMON_PLIST" "$C/daemon.plist.orig"
  printf '%s\n' "$LIVE_CANARY" > "$H/.subrouter/$LIVE_MARKER"
  return 0
}

# The live job's stop marker. No case may write, change or remove it.
LIVE_MARKER=watchdog-stop.ai.manaflow.subrouter
LIVE_CANARY="label=ai.manaflow.subrouter
until=0
by=the stand-in installer tests, as a canary"

# The daemon's plist as a supervised job's looks: the installer reads the
# stand-in's settings from it and must never change it.
write_daemon_plist() {
  cat > "$DAEMON_PLIST" <<P
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>$H</string>
		<key>SUBROUTER_CLAUDE_SOCKET</key>
		<string>$H/.subrouter/claude.sock</string>
		<key>SUBROUTER_PEER_TAILNET</key>
		<string>box-a=100.64.0.9</string>
		<key>SUBROUTER_REMOTE_POOLS_FILE</key>
		<string>$H/.subrouter/peers.json</string>
		<key>SUBROUTER_REMOTE_POOL_TOKEN</key>
		<string>srk_peer_NOT_FOR_THE_STANDIN</string>
	</dict>
	<key>ExitTimeOut</key>
	<integer>120</integer>
	<key>KeepAlive</key>
	<true/>
	<key>Label</key>
	<string>ai.manaflow.subrouter</string>
	<key>ProgramArguments</key>
	<array>
		<string>$H/bin/subrouter-supervisor</string>
		<string>supervise</string>
		<string>--addr</string>
		<string>127.0.0.1:31415</string>
		<string>--control-socket</string>
		<string>$H/.subrouter/supervisor.sock</string>
		<string>--worker-bin</string>
		<string>$H/bin/subrouter</string>
		<string>--worker-config=$H/.subrouter/worker-config.json</string>
		<string>--</string>
		<string>--control-socket</string>
		<string>/not/this/one.sock</string>
	</array>
</dict>
</plist>
P
}

# What fails a case whatever it tests, one line each.
complaints() {
  local f
  [ ! -s "$C/ld/live" ] || echo "a launchctl stand-in was handed a label it must never see: $(head -1 "$C/ld/live")"
  [ ! -s "$C/ld/path-launchctl" ] || echo "a script found launchctl on PATH: $(head -1 "$C/ld/path-launchctl")"
  cmp -s "$DAEMON_PLIST" "$C/daemon.plist.orig" || echo "the daemon's plist was changed"
  [ ! -e "$C/ld/jobs/ai.manaflow.subrouter" ] || echo "the daemon's job was loaded"
  f=$(find "$C" \( -name "$LIVE_MARKER" -o -name "$LIVE_MARKER.*" \) ! -path "$H/.subrouter/$LIVE_MARKER" 2>/dev/null | head -1)
  [ -z "$f" ] || echo "a stop marker for the live job was written: $f"
  [ "$(cat "$H/.subrouter/$LIVE_MARKER" 2>/dev/null)" = "$LIVE_CANARY" ] || echo "the live job's stop marker was changed or removed"
}

finish() {
  local w
  while IFS= read -r w; do [ -z "$w" ] || bad "$w"; done <<EOF
$(complaints)
EOF
  case " $FAIL_LIST " in
    *" $CASE "*)
      echo "  --- $CASE out ---"; tail -20 "$C/out" 2>/dev/null | sed 's/^/  | /'
      echo "  --- $CASE launchctl calls ---"; tail -15 "$C/ld/calls" | sed 's/^/  | /' ;;
    *) echo "  ok   $CASE" ;;
  esac
}

envs() {
  echo PATH="$C/bin:/usr/bin:/bin:/usr/sbin:/sbin"
  echo HOME="$H"
  echo TMPDIR="$C"
  echo FAKE_LD="$C/ld"
  echo FAKE_CLOCK="$C/clock"
  echo SUBROUTER_STANDBY_LAUNCHCTL="$C/bin/sb-launchctl"
  echo SUBROUTER_WATCHDOG_LAUNCHCTL="$C/bin/wd-launchctl"
}
in_env() { # [VAR=value | -u VAR]... -- command...
  local e=() line drop=""
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
  env -i "${e[@]}" "$@"
}
installer() { in_env -- "$SB_BASH" "$INSTALLER" "$@" > "$C/out" 2>&1 < /dev/null; RC=$?; }
# token <args>: with the case's token file on standard input.
token_cmd() { in_env -- "$SB_BASH" "$INSTALLER" token "$@" > "$C/out" 2>&1 < "$C/token-in"; RC=$?; }

count() { local n; n=$(grep -cE -- "$1" "$2" 2>/dev/null); echo "${n:-0}"; }
calls() { count "$1" "$C/ld/calls"; }
expect_rc() { [ "$RC" = "$1" ] && ok || bad "exit $RC, want $1"; }
expect_out() { grep -Fq -- "$1" "$C/out" && ok || bad "output lacks: $1"; }
expect_no_out() { grep -Fq -- "$1" "$C/out" && bad "output has: $1" || ok; }
expect_calls() { local n; n=$(calls "$1"); [ "$n" = "$2" ] && ok || bad "$n launchctl calls match /$1/, want $2"; }
expect_no_change() { expect_calls " (bootstrap|bootout|kickstart|enable|disable|remove|unload|load) " 0; }
# The one refusal line: stderr holds exactly one line from the installer.
expect_one_refusal() {
  local n; n=$(grep -c '^install-standby: ' "$C/out")
  [ "$n" = 1 ] && ok || bad "$n lines start with install-standby:, want one refusal line"
  expect_out "install-standby: refusing"
}
# The plist's ProgramArguments, one line.
plist_args_of() { sed -n '/<key>ProgramArguments<\/key>/,/<\/array>/s:^[[:space:]]*<string>\(.*\)</string>$:\1:p' "$1" | tr '\n' ' '; }
plist_env_of() { tr -d '\n\t' < "$1" | sed -n "s:.*<key>$2</key><string>\([^<]*\)</string>.*:\1:p"; }
plist_has() { tr -d '\n\t' < "$1" | grep -Fq -- "$2"; }
loaded() { [ -e "$C/ld/jobs/${1:-$LABEL}" ]; }
load_job() { echo "${2:-$PLIST}" > "$C/ld/jobs/${1:-$LABEL}"; }
nothing_written() {
  check "no plist was written" test ! -e "$PLIST"
  check "no copy was written" test ! -e "$H/.subrouter/standby"
  check "the watchdog was not installed" test ! -e "$WDPLIST"
}
set_clock() { echo "$1" > "$C/clock"; }
mode_of() { ls -ld "$1" | cut -c1-10; }
# A state file as the stand-in writes it (json.MarshalIndent of standbyView).
write_state() { # state pid
  cat > "$H/.subrouter/standby-state.json" <<J
{
  "state": "$1",
  "since": "2026-11-22T11:58:00.123456789Z",
  "pid": $2,
  "supervisor": {
    "claude_socket": "serving",
    "claude_socket_waits_for_lock": true,
    "read_at": "2026-11-22T11:59:59.5-08:00"
  },
  "supervisor_answering": true,
  "takes": 0,
  "ready_checks": 0,
  "parked": 0,
  "boxes": {
    "peers_file": "ok",
    "boxes": [
      {
        "name": "box-a",
        "token": "ok",
        "at": "2026-11-22T11:55:00Z"
      },
      {
        "name": "box-b",
        "token": "refused",
        "at": "2026-11-22T11:56:00Z"
      }
    ]
  },
  "policy_copy": {
    "held": true,
    "saved_at": "2026-11-22T03:50:00-08:00",
    "age_seconds": 3
  }
}
J
}

echo "== the stand-in installer (under $SB_BASH)"

if begin install-writes-the-plist-and-the-copy-and-loads-once; then
  installer install "$C/bin/subrouter"
  expect_rc 0
  check "the plist was written" test -f "$PLIST"
  check "the copy was written" test -x "$COPY"
  cmp -s "$C/bin/subrouter" "$COPY" && ok || bad "the copy differs from the binary"
  expect_calls "^sb-launchctl bootstrap gui/$UIDN $PLIST @" 1
  expect_calls "^sb-launchctl bootout " 0
  check "launchd has the stand-in job" loaded
  sum=$(sha256sum "$COPY" | cut -d' ' -f1)
  expect_out "installed: gui/$UIDN/$LABEL runs $COPY (sha256 $sum)"
  got=$(plist_args_of "$PLIST")
  want="$COPY standby --control-socket $H/.subrouter/supervisor.sock --daemon-addr 127.0.0.1:31415 --worker-config $H/.subrouter/worker-config.json "
  [ "$got" = "$want" ] && ok || bad "the plist runs
  $got
want
  $want"
  [ "$(plist_env_of "$PLIST" SUBROUTER_REMOTE_POOLS_FILE)" = "$H/.subrouter/peers.json" ] && ok || bad "the plist lacks the daemon's peers file"
  [ "$(plist_env_of "$PLIST" SUBROUTER_PEER_TAILNET)" = "box-a=100.64.0.9" ] && ok || bad "the plist lacks the daemon's tailnet setting"
  [ "$(plist_env_of "$PLIST" SUBROUTER_CLAUDE_SOCKET)" = "$H/.subrouter/claude.sock" ] && ok || bad "the plist lacks the daemon's socket"
  [ "$(plist_env_of "$PLIST" HOME)" = "$H" ] && ok || bad "the plist's HOME is not the installer's"
  plist_has "$PLIST" "srk_peer_NOT_FOR_THE_STANDIN" && bad "the plist carries the daemon's peer token" || ok
  for want in "<key>RunAtLoad</key><true/>" "<key>KeepAlive</key><true/>" "<key>ThrottleInterval</key><integer>10</integer>" \
      "<key>StandardErrorPath</key><string>$H/Library/Logs/subrouter-standby.log</string>" "<key>Label</key><string>$LABEL</string>"; do
    plist_has "$PLIST" "$want" && ok || bad "the plist lacks $want"
  done
  if command -v python3 > /dev/null 2>&1; then
    python3 -c 'import plistlib, sys; p = plistlib.load(open(sys.argv[1], "rb")); sys.exit(0 if p["KeepAlive"] is True and p["RunAtLoad"] is True else 1)' "$PLIST" &&
      ok || bad "the plist does not parse as a plist with KeepAlive and RunAtLoad"
  fi
  # The check ran the binary with the job's own arguments and environment.
  grep -Fxq "args: standby --control-socket $H/.subrouter/supervisor.sock --daemon-addr 127.0.0.1:31415 --worker-config $H/.subrouter/worker-config.json --check" "$C/ld/check-seen" &&
    ok || bad "the check did not run with the job's arguments: $(head -1 "$C/ld/check-seen")"
  grep -Fxq "SUBROUTER_REMOTE_POOLS_FILE=$H/.subrouter/peers.json" "$C/ld/check-seen" && ok || bad "the check did not see the job's peers file"
  grep -q '^SUBROUTER_REMOTE_POOL_TOKEN=' "$C/ld/check-seen" && bad "the check was given the daemon's peer token" || ok
  # The watchdog now watches the stand-in, and the daemon by its default.
  check "the watchdog's plist was written" test -f "$WDPLIST"
  got=$(plist_args_of "$WDPLIST")
  want="/bin/bash $H/.subrouter/watchdog/subrouter-watchdog.sh run --watch $LABEL=$PLIST --log $H/Library/Logs/subrouter-watchdog.log --stops $H/.subrouter "
  [ "$got" = "$want" ] && ok || bad "the watchdog runs
  $got
want
  $want"
  expect_calls "^wd-launchctl bootstrap gui/$UIDN $WDPLIST @" 1
  expect_out "the stand-in says: "
  finish
fi

if begin second-install-announces-a-stop-and-reloads; then
  installer install "$C/bin/subrouter"
  expect_rc 0
  cp "$C/bin/subrouter" "$C/subrouter-v2"; echo "# build 2" >> "$C/subrouter-v2"
  installer install "$C/subrouter-v2"
  expect_rc 0
  expect_calls "^sb-launchctl bootout gui/$UIDN/$LABEL @" 1
  expect_calls "^sb-launchctl bootstrap gui/$UIDN $PLIST @" 2
  [ "$(cat "$C/ld/markers" 2>/dev/null)" = "marker at bootout of $LABEL" ] && ok || bad "the bootout came without the stop announced: $(cat "$C/ld/markers" 2>/dev/null)"
  check "the stop was withdrawn after the bootstrap" test ! -e "$H/.subrouter/watchdog-stop.$LABEL"
  cmp -s "$C/subrouter-v2" "$COPY" && ok || bad "the copy is not the new build"
  expect_out "(sha256 $(sha256sum "$C/subrouter-v2" | cut -d' ' -f1))"
  check "launchd has the stand-in job" loaded
  n=$(grep -c "<string>--watch</string>" "$WDPLIST")
  [ "$n" = 1 ] && ok || bad "the watchdog watches the stand-in $n times"
  finish
fi

if begin installer-refuses-every-other-label; then
  for label in ai.manaflow.subrouter ai.manaflow.subrouter-watchdog com.example.standby ai.manaflow.subrouter-standby2 test.subrouterx.sb "test.subrouter.sb/x"; do
    for verb in "install $C/bin/subrouter" uninstall status "token box-a"; do
      # shellcheck disable=SC2086
      installer $verb --label "$label" --dir "$C/sbhome"
      [ "$RC" = 2 ] && ok || bad "$verb --label $label: exit $RC, want 2"
    done
  done
  installer install "$C/bin/subrouter" --label ai.manaflow.subrouter --dir "$C/sbhome"
  expect_out "the daemon's own job"
  installer install "$C/bin/subrouter" --label ai.manaflow.subrouter-watchdog --dir "$C/sbhome"
  expect_out "the watchdog's job"
  installer install "$C/bin/subrouter" --label test.subrouter.sb.x
  expect_rc 2; expect_out "--dir"
  expect_calls . 0
  check "nothing was written" test ! -e "$C/sbhome"
  nothing_written
  check "the binary was never run" test ! -e "$C/ld/binary-calls"
  finish
fi

if begin installer-refuses-what-the-stand-in-would-not-arm-for; then
  for pair in "supervisor_without_marker|lacks claude_socket_waits_for_lock" "remote_locked|remote.key gate is locked" \
      "peers_file_missing|has no peers file" "no_supervisor_reading|no supervisor answered on $H/.subrouter/supervisor.sock" \
      "supervisor_keeps_no_socket|keeps no Claude socket now (claude_socket: unavailable)"; do
    printf 'supervisor unavailable waits_for_lock=true\nverdict disarmed %s\n' "${pair%%|*}" > "$C/ld/check-out"
    installer install "$C/bin/subrouter"
    expect_rc 2
    expect_one_refusal
    expect_out "${pair#*|}"
    nothing_written
  done
  # An already installed stand-in is not changed by a refused upgrade.
  rm -f "$C/ld/check-out"
  installer install "$C/bin/subrouter"
  expect_rc 0
  : > "$C/ld/calls"
  printf 'verdict disarmed supervisor_without_marker\n' > "$C/ld/check-out"
  installer install "$C/bin/subrouter"
  expect_rc 2
  expect_no_change
  check "the stand-in is still loaded" loaded
  finish
fi

if begin installer-warns-of-a-missing-token-and-goes-on; then
  printf 'peers_file ok %s\nbox box-a file_missing\nbox box-b file_mode\nbox box-c untested\nsupervisor serving waits_for_lock=true\nverdict armed\n' "$H/.subrouter/peers.json" > "$C/ld/check-out"
  installer install "$C/bin/subrouter"
  expect_rc 0
  expect_out "warning: box box-a has no token file; give it one with: bash $INSTALLER token box-a < file-holding-the-token"
  expect_out "warning: the token file of box box-b is file_mode"
  expect_no_out "box-c"
  check "the stand-in is loaded" loaded
  finish
fi

if begin installer-refuses-a-binary-that-cannot-check; then
  : > "$C/ld/no-check"
  installer install "$C/bin/subrouter"
  expect_rc 2
  expect_out "did not answer \`standby --check\`"
  expect_out "flag provided but not defined: -check"
  nothing_written
  expect_calls " bootstrap " 0
  installer install "$C/missing-binary"
  expect_rc 2; expect_out "is not an executable file"
  finish
fi

if begin installer-refuses-an-upgrade-while-it-holds-the-socket; then
  installer install "$C/bin/subrouter"
  expect_rc 0
  write_state serving 4242
  : > "$C/ld/calls"
  installer install "$C/bin/subrouter"
  expect_rc 2; expect_one_refusal; expect_out "holds the Claude socket now (state serving)"
  expect_no_change
  # A state file another process wrote is not the job's word.
  write_state serving 999
  installer install "$C/bin/subrouter" --dry-run
  expect_rc 0
  write_state holding 4242
  installer install "$C/bin/subrouter" --force
  expect_rc 0
  expect_calls "^sb-launchctl bootout gui/$UIDN/$LABEL @" 1
  finish
fi

if begin installer-refuses-its-label-loaded-from-another-plist; then
  load_job "$LABEL" "$C/elsewhere.plist"
  installer install "$C/bin/subrouter"
  expect_rc 2; expect_out "$C/elsewhere.plist"
  installer uninstall
  expect_rc 2
  expect_no_change
  finish
fi

if begin installer-dry-runs-change-nothing; then
  installer install "$C/bin/subrouter" --dry-run
  expect_rc 0
  expect_out "would copy $C/bin/subrouter (sha256 $(sha256sum "$C/bin/subrouter" | cut -d' ' -f1)) to $COPY"
  expect_out "would write $PLIST:"
  expect_out "<key>KeepAlive</key>"
  expect_out "would run: $C/bin/sb-launchctl bootstrap gui/$UIDN $PLIST"
  expect_out "install-watchdog.sh install --watch $LABEL=$PLIST"
  nothing_written
  expect_no_change
  # Installed, then dry runs of the upgrade and the uninstall.
  installer install "$C/bin/subrouter"
  expect_rc 0
  cp -p "$PLIST" "$C/plist.before"; cp -p "$WDPLIST" "$C/wdplist.before"
  : > "$C/ld/calls"
  installer install "$C/bin/subrouter" --dry-run
  expect_rc 0
  expect_out "subrouter-watchdog.sh hold $LABEL 300"
  expect_out "would run: $C/bin/sb-launchctl bootout gui/$UIDN/$LABEL"
  installer uninstall --purge-tokens --dry-run
  expect_rc 0
  expect_out "would run: $C/bin/sb-launchctl bootout gui/$UIDN/$LABEL"
  expect_out "would run: rm -rf $H/.subrouter/standby-tokens"
  expect_no_change
  check "the stop was not announced" test ! -e "$H/.subrouter/watchdog-stop.$LABEL"
  cmp -s "$PLIST" "$C/plist.before" && ok || bad "a dry run changed the plist"
  cmp -s "$WDPLIST" "$C/wdplist.before" && ok || bad "a dry run changed the watchdog's plist"
  check "the copy stays" test -x "$COPY"
  check "the stand-in stays loaded" loaded
  finish
fi

if begin uninstall-keeps-the-tokens-and-purges-them-when-told; then
  printf '%s\n' "$TOKEN" > "$C/token-in"
  installer install "$C/bin/subrouter"
  token_cmd box-a
  expect_rc 0
  write_state armed 4242
  : > "$H/.subrouter/standby-policy.json"
  installer uninstall
  expect_rc 0
  check "launchd no longer has the stand-in" test ! -e "$C/ld/jobs/$LABEL"
  check "the plist was removed" test ! -e "$PLIST"
  check "the copy was removed" test ! -e "$COPY"
  check "the state file was removed" test ! -e "$H/.subrouter/standby-state.json"
  check "the policy copy was removed" test ! -e "$H/.subrouter/standby-policy.json"
  check "the token file stays" test -f "$H/.subrouter/standby-tokens/box-a"
  [ "$(cat "$C/ld/markers" 2>/dev/null)" = "marker at bootout of $LABEL" ] && ok || bad "the uninstall's bootout came without the stop announced"
  check "the stop was withdrawn" test ! -e "$H/.subrouter/watchdog-stop.$LABEL"
  got=$(plist_args_of "$WDPLIST")
  want="/bin/bash $H/.subrouter/watchdog/subrouter-watchdog.sh run --log $H/Library/Logs/subrouter-watchdog.log --stops $H/.subrouter "
  [ "$got" = "$want" ] && ok || bad "after the uninstall the watchdog runs
  $got
want its default list
  $want"
  installer install "$C/bin/subrouter"
  installer uninstall --purge-tokens
  expect_rc 0
  check "the token folder was removed" test ! -e "$H/.subrouter/standby-tokens"
  # Its own label only, from its own plist.
  n=$(grep '^sb-launchctl' "$C/ld/calls" | grep -vE " gui/$UIDN/$LABEL @| gui/$UIDN $PLIST @" | grep -c .)
  [ "$n" = 0 ] && ok || bad "the installer called launchctl for another job: $(grep '^sb-launchctl' "$C/ld/calls" | grep -v "$LABEL" | head -1)"
  finish
fi

if begin the-watchdog-keeps-its-other-jobs; then
  other="test.subrouter.other=$C/other.plist"
  in_env -- "$SB_BASH" "$HERE/../macos/install-watchdog.sh" install --watch "$other" > "$C/out" 2>&1
  installer install "$C/bin/subrouter"
  expect_rc 0
  got=$(plist_args_of "$WDPLIST")
  case "$got" in *"--watch $LABEL=$PLIST --watch $other --log"*) ok ;; *) bad "the watchdog runs $got" ;; esac
  installer uninstall
  got=$(plist_args_of "$WDPLIST")
  case "$got" in *" run --watch $other --log"*) ok ;; *) bad "after the uninstall the watchdog runs $got" ;; esac
  installer install "$C/bin/subrouter" --no-watchdog
  got=$(plist_args_of "$WDPLIST")
  case "$got" in *"$LABEL"*) bad "--no-watchdog changed the watchdog: $got" ;; *) ok ;; esac
  finish
fi

if begin token-never-prints-the-token-and-sets-the-modes; then
  printf '  %s  \n\n' "$TOKEN" > "$C/token-in"
  mkdir -p "$H/.subrouter/standby-tokens"; chmod 755 "$H/.subrouter/standby-tokens"
  token_cmd box-a
  expect_rc 0
  f="$H/.subrouter/standby-tokens/box-a"
  [ "$(cat "$f")" = "$TOKEN" ] && ok || bad "the token file does not hold the token alone"
  [ "$(mode_of "$f")" = "-rw-------" ] && ok || bad "the token file's mode is $(mode_of "$f")"
  [ "$(mode_of "$H/.subrouter/standby-tokens")" = "drwx------" ] && ok || bad "the token folder's mode is $(mode_of "$H/.subrouter/standby-tokens")"
  cp "$C/out" "$C/all-output"
  # Each refusal: the argument that looks like a token is not printed.
  for args in "$TOKEN" "box-a $TOKEN" "--token=$TOKEN" "4f1c9a77e2b04d55a1c3e8f09b6d2a71" "SRK_x" "box/a" "."; do
    # shellcheck disable=SC2086
    token_cmd $args
    [ "$RC" = 2 ] && ok || bad "token $args: exit $RC, want 2"
    cat "$C/out" >> "$C/all-output"
  done
  token_cmd box-a --expires "$TOKEN"
  expect_rc 2; cat "$C/out" >> "$C/all-output"
  printf 'two words\n' > "$C/token-in"; token_cmd box-a; expect_rc 2; cat "$C/out" >> "$C/all-output"
  : > "$C/token-in"; token_cmd box-a; expect_rc 2; expect_out "held no token"
  if command -v script > /dev/null 2>&1 && script -qec true /dev/null > /dev/null 2>&1; then
    in_env -- script -qec "$SB_BASH $INSTALLER token box-a" /dev/null > "$C/out" 2>&1 < /dev/null; RC=$?
    expect_out "not from a terminal"
  fi
  grep -F -- "${TOKEN#srk_}" "$C/all-output" > /dev/null && bad "the output holds the token: $(grep -F -- "${TOKEN#srk_}" "$C/all-output" | head -1)" || ok
  [ "$(cat "$f")" = "$TOKEN" ] && ok || bad "a refused run changed the token file"
  # Nothing but the token file holds it.
  n=$(grep -rlF -- "${TOKEN#srk_}" "$C" 2>/dev/null | grep -vxF -e "$f" -e "$C/token-in" | grep -c .)
  [ "$n" = 0 ] && ok || bad "the token is written elsewhere: $(grep -rlF -- "${TOKEN#srk_}" "$C" | grep -vxF -e "$f" -e "$C/token-in" | head -1)"
  expect_calls . 0
  finish
fi

if begin token-expiry-shows-renew-soon-and-lapsed; then
  printf '%s\n' "$TOKEN" > "$C/token-in"
  token_cmd box-a --expires 2026-12-31
  expect_rc 0
  expect_out "its last day is 2026-12-31 (ok)"
  [ "$(cat "$H/.subrouter/standby-token-expiry/box-a")" = 2026-12-31 ] && ok || bad "the last day was not recorded"
  [ "$(mode_of "$H/.subrouter/standby-token-expiry")" = "drwx------" ] && ok || bad "the expiry folder's mode is $(mode_of "$H/.subrouter/standby-token-expiry")"
  # The last 7 days are 25 to 31 December.
  for at in "1796040000|2026-11-30T12:00:00Z|ok" "1798156799|2026-12-24T23:59:59Z|ok" "1798156800|2026-12-25T00:00:00Z|renew_soon" \
      "1798761599|2026-12-31T23:59:59Z|renew_soon" "1798761600|2027-01-01T00:00:00Z|lapsed"; do
    set_clock "${at%%|*}"
    installer status
    expect_out "box box-a: token not reported yet, last day 2026-12-31: ${at##*|}"
  done
  # A new token without a last day drops the old one's.
  token_cmd box-a
  check "the old last day was dropped" test ! -e "$H/.subrouter/standby-token-expiry/box-a"
  for bad_day in 2026-13-01 2026-1-01 tomorrow; do
    token_cmd box-a --expires "$bad_day"
    [ "$RC" = 2 ] && ok || bad "--expires $bad_day: exit $RC, want 2"
  done
  finish
fi

if begin status-shows-the-job-state-boxes-and-watchdog-and-no-token; then
  printf '%s\n' "$TOKEN" > "$C/token-in"
  installer install "$C/bin/subrouter"
  token_cmd box-a --expires 2027-06-30
  write_state serving 4242
  # 12:00:00Z, ten minutes after the policy copy was saved.
  set_clock 1795348800
  installer status
  expect_rc 0
  expect_out "job gui/$UIDN/$LABEL: loaded from $PLIST, pid 4242"
  expect_out "plist $PLIST: runs $COPY standby --control-socket"
  expect_out "copy $COPY: sha256 $(sha256sum "$COPY" | cut -d' ' -f1)"
  expect_out "state: serving since 2026-11-22T11:58:00.123456789Z, written by pid 4242"
  expect_no_out "stale"
  expect_out "supervisor: serving, waits for the lock: true, answering: true"
  expect_out "peers file: ok"
  expect_out "box box-a: token ok at 2026-11-22T11:55:00Z, last day 2027-06-30: ok"
  expect_out "box box-b: token refused at 2026-11-22T11:56:00Z"
  expect_out "policy copy: held, saved 10 min ago, at 2026-11-22T03:50:00-08:00"
  expect_out "watchdog: watches $LABEL"
  expect_out "log: $H/Library/Logs/subrouter-standby.log"
  grep -F -- "${TOKEN#srk_}" "$C/out" > /dev/null && bad "status printed the token" || ok
  write_state disarmed 999
  installer status
  expect_out "not the job's pid: this file is stale"
  installer uninstall
  installer status
  expect_rc 0
  expect_out "job gui/$UIDN/$LABEL: not loaded"
  expect_out "plist $PLIST: missing"
  expect_out "state: none yet"
  expect_out "watchdog: does not watch $LABEL"
  before=$(calls " (bootstrap|bootout) ")
  installer status
  [ "$(calls " (bootstrap|bootout) ")" = "$before" ] && ok || bad "status changed launchd"
  finish
fi

if begin a-scratch-label-keeps-to-its-folder; then
  S="$C/scratch"
  installer install "$C/bin/subrouter" --label test.subrouter.sbtests.x --dir "$S" --state-dir "$S/state" \
    --socket "$S/claude.sock" --control-socket "$S/sv.sock" --daemon-addr 127.0.0.1:31497 --peers-file "$S/peers.json"
  expect_rc 0
  check "its plist is in its folder" test -f "$S/test.subrouter.sbtests.x.plist"
  check "its copy is in its folder" test -x "$S/subrouter-standby"
  plist_has "$S/test.subrouter.sbtests.x.plist" "<key>StandardErrorPath</key><string>$S/standby.log</string>" && ok || bad "its log is not in its folder"
  got=$(plist_args_of "$S/test.subrouter.sbtests.x.plist")
  want="$S/subrouter-standby standby --state-dir $S/state --socket $S/claude.sock --control-socket $S/sv.sock --daemon-addr 127.0.0.1:31497 --peers-file $S/peers.json "
  [ "$got" = "$want" ] && ok || bad "the scratch plist runs
  $got
want
  $want"
  grep -q 'SUBROUTER_REMOTE_POOLS_FILE\|SUBROUTER_PEER_TAILNET\|SUBROUTER_CLAUDE_SOCKET' "$C/ld/check-seen" && bad "a scratch install read the daemon's plist" || ok
  expect_calls "^wd-launchctl" 0
  nothing_written
  installer uninstall --label test.subrouter.sbtests.x --dir "$S"
  expect_rc 0
  check "its plist was removed" test ! -e "$S/test.subrouter.sbtests.x.plist"
  finish
fi

if begin launchctl-is-never-looked-up-on-path; then
  in_env SUBROUTER_STANDBY_LAUNCHCTL=launchctl -- "$SB_BASH" "$INSTALLER" status > "$C/out" 2>&1; RC=$?
  expect_rc 2; expect_out "never looked up on PATH"
  # Unset, it is /bin/launchctl, even with a launchctl first on PATH (that
  # one fails the case). Here /bin/launchctl is missing or fenced off, and
  # status shows what running it answered.
  in_env -u SUBROUTER_STANDBY_LAUNCHCTL -- "$SB_BASH" "$INSTALLER" status > "$C/out" 2>&1; RC=$?
  expect_out "/bin/launchctl"
  expect_calls . 0
  finish
fi

if begin scripts-use-nothing-from-bash-4; then
  # The Mac runs these under /bin/bash 3.2, Linux CI under bash 5. The same
  # pattern as the watchdog's case, outside comments.
  bash4='(\[\[?|test) +-[vR] |(declare|typeset|local) +-[A-Za-z]*[Agn]( |$)|\$\{[#!]?[A-Za-z_][A-Za-z0-9_]*(\[[^]]*\])?(\^|,|@[A-Za-z]\})|(^|[^A-Za-z0-9_-])(mapfile|readarray|coproc)( |$)|\|&|&>>|;;?&( |$)|\[-[0-9]+\]\}|wait +-[nfp]|EPOCH(SECONDS|REALTIME)|BASHPID|SRANDOM|shopt +-s +(globstar|lastpipe)|(^|[^$])\{[A-Za-z_][A-Za-z0-9_]*\}[<>]|%\([^)]*\)T'
  for f in "$INSTALLER" "$HERE/rehearse-standby.sh"; do
    check "${f##*/} exists" test -f "$f"
    hits=$(grep -nE -- "$bash4" "$f" | grep -vE '^[0-9]+:[[:space:]]*#')
    [ -z "$hits" ] && ok || bad "${f##*/} uses bash 4 or later: $(printf '%s\n' "$hits" | head -1)"
  done
  printf '%s\n' 'declare -A seen' | grep -qE -- "$bash4" && ok || bad "the bash 4 pattern misses declare -A"
  finish
fi

if begin stand-ins-catch-the-labels-they-must-not-see; then
  in_env -- "$C/bin/sb-launchctl" print "gui/$UIDN/ai.manaflow.subrouter" > /dev/null 2>&1
  check "the installer's launchctl saw the daemon's label" test -s "$C/ld/live"; : > "$C/ld/live"
  in_env -- "$C/bin/sb-launchctl" bootstrap "gui/$UIDN" "$H/Library/LaunchAgents/ai.manaflow.subrouter.plist" > /dev/null 2>&1
  check "the installer's launchctl saw the daemon's plist" test -s "$C/ld/live"; : > "$C/ld/live"
  in_env -- "$C/bin/sb-launchctl" print "gui/$UIDN/ai.manaflow.subrouter-watchdog" > /dev/null 2>&1
  check "the installer's launchctl saw the watchdog's label" test -s "$C/ld/live"; : > "$C/ld/live"
  in_env -- "$C/bin/wd-launchctl" print "gui/$UIDN/$LABEL" > /dev/null 2>&1
  check "the watchdog installer's launchctl saw the stand-in's label" test -s "$C/ld/live"; : > "$C/ld/live"
  in_env -- "$C/bin/wd-launchctl" print "gui/$UIDN/ai.manaflow.subrouter-watchdog" > /dev/null 2>&1
  check "the watchdog installer's launchctl takes the watchdog's label" test ! -s "$C/ld/live"
  in_env -- "$C/bin/sb-launchctl" print "gui/$UIDN/$LABEL" > /dev/null 2>&1
  check "the installer's launchctl takes the stand-in's label" test ! -s "$C/ld/live"
  echo x >> "$DAEMON_PLIST"
  [ -n "$(complaints)" ] && ok || bad "a changed daemon plist went unseen"
  cp "$C/daemon.plist.orig" "$DAEMON_PLIST"
  echo "until=1" >> "$H/.subrouter/$LIVE_MARKER"
  [ -n "$(complaints)" ] && ok || bad "a changed live marker went unseen"
  printf '%s\n' "$LIVE_CANARY" > "$H/.subrouter/$LIVE_MARKER"
  [ -z "$(complaints)" ] && ok || bad "an untouched case has complaints: $(complaints)"
  finish
fi

echo
echo "standby install tests: $PASS assertions passed, $FAILED failed"
if [ "$FAILED" -ne 0 ]; then
  echo "failing cases:$FAIL_LIST"
  exit 1
fi
[ "$PASS" -gt 0 ] || { echo "no case matched '$FILTER'" >&2; exit 1; }
exit 0
