#!/usr/bin/env bash
# Tests for the setup tools in ops/bin: subrouter-remote-key, subrouter-unbrick
# and subrouter-quickstart.
#
#   bash ops/deploy/tests/setup-tools-test.sh              all cases, in parallel
#   bash ops/deploy/tests/setup-tools-test.sh <substring>  matching cases, in order
#   bash ops/deploy/tests/setup-tools-test.sh '=<case>'    exactly that case (quote it in zsh)
#
# Each tool runs under `env -i` with HOME in the case's own directory, and every
# command that could act on this machine (launchctl, make, go, claude, security,
# sudo, open) is a stub on PATH. Before a tool runs, the harness checks that each
# of those names resolves to its stub, and aborts the run otherwise.
#
# On macOS every tool also runs inside sandbox-exec, which denies writes outside
# the case directories, all IP traffic (the live daemon on 127.0.0.1:31415
# included), exec of the real launchctl, security, sudo and open, and reads of
# the real ~/.subrouter, ~/.claude, ~/.codex and keychains. SETUP_TOOLS_SANDBOX=0
# turns it off. Without sandbox-exec, as on Linux, only the stubs and the fake
# HOME fence a case.
set -uo pipefail
# The macOS default. A stricter umask would hide a mode a tool forgets to set.
umask 022

HERE=$(cd "$(dirname "$0")" && pwd -P)
OPS=$(cd "$HERE/../.." && pwd -P)
ROOT=$(cd "$OPS/.." && pwd -P)
REMOTE_KEY="$OPS/bin/subrouter-remote-key"
FILTER="${1:-}"

PY3=$(command -v python3) || { echo "python3 is required" >&2; exit 2; }
USER_HOME=$(eval echo "~$(id -un)")

# With no case filter, fan the cases out: each runs as its own copy of this
# script ("=name" is an exact match), SETUP_TOOLS_JOBS at a time, and the
# reports are printed in file order.
if [ -z "$FILTER" ] && [ "${SETUP_TOOLS_JOBS:-6}" -gt 1 ]; then
  SELF="$HERE/$(basename "$0")"
  # Never `cd "$(mktemp -d)"` in one step: a failed mktemp makes that `cd ""`,
  # which stays put, and the rm -rf below would then remove the working dir.
  RUN=$(mktemp -d "${TMPDIR:-/tmp}/setup-tools-run.XXXXXX") && [ -d "$RUN" ] || { echo "mktemp failed" >&2; exit 2; }
  sed -n 's/^if begin \([A-Za-z0-9_-]*\); then$/\1/p' "$SELF" > "$RUN/cases"
  [ -s "$RUN/cases" ] || { echo "no cases found in $SELF" >&2; exit 1; }
  xargs -P "${SETUP_TOOLS_JOBS:-6}" -I{} sh -c 'bash "$1" "=$2" > "$3/$2.out" 2>&1; echo $? > "$3/$2.rc"' \
    sh "$SELF" {} "$RUN" < "$RUN/cases"
  total=0; failed=0; failing=""
  while IFS= read -r c; do
    total=$((total + 1))
    grep -E '^  (ok|FAIL|---|\|)|^ABORT' "$RUN/$c.out"
    if [ "$(cat "$RUN/$c.rc" 2>/dev/null)" != 0 ]; then
      failed=$((failed + 1)); failing="$failing $c"
      grep -qE '^  (ok|FAIL)|^ABORT' "$RUN/$c.out" || { echo "  FAIL $c: no verdict"; tail -5 "$RUN/$c.out" | sed 's/^/  | /'; }
    fi
  done < "$RUN/cases"
  rm -rf "$RUN"
  echo
  echo "setup tool tests: $total cases, $((total - failed)) passed, $failed failed"
  [ "$failed" -eq 0 ] || { echo "failing cases:$failing"; exit 1; }
  exit 0
fi

T0=$(mktemp -d "${TMPDIR:-/tmp}/setup-tools.XXXXXX") && [ -d "$T0" ] || { echo "mktemp failed" >&2; exit 2; }
T=$(cd "$T0" && pwd -P) && [ -d "$T" ] && [ "$T" != / ] || { echo "cannot resolve $T0" >&2; exit 2; }
# chmod first: the unreadable-key cases leave mode-000 files behind if they stop
# early, and rm -rf cannot always remove those.
[ -n "${KEEP:-}" ] && echo "keeping $T" || trap 'chmod -R u+rwx "$T" 2>/dev/null; rm -rf "$T"' EXIT
PASS=0; FAILED=0; FAIL_LIST=""; CASE=""; C=""; RC=0

SANDBOX=""
if [ "${SETUP_TOOLS_SANDBOX:-1}" = 1 ] && command -v sandbox-exec >/dev/null 2>&1; then
  case "$USER_HOME" in
    /*) ;;
    *) echo "cannot resolve this user's home (got '$USER_HOME'), so the sandbox would fence nothing" >&2; exit 2 ;;
  esac
  SANDBOX="(version 1)(allow default)
(deny file-write* (require-not (require-any (subpath \"$T\") (subpath \"/private/tmp\") (subpath \"/private/var/folders\") (literal \"/dev/null\") (literal \"/dev/tty\") (literal \"/dev/dtracehelper\") (subpath \"/dev/fd\"))))
(deny process-exec (literal \"/usr/bin/sudo\") (literal \"/bin/launchctl\") (literal \"/usr/bin/security\") (literal \"/usr/bin/open\"))
(deny file-read* (subpath \"$USER_HOME/.subrouter\") (subpath \"$USER_HOME/.codex\") (subpath \"$USER_HOME/.claude\") (subpath \"$USER_HOME/Library/Keychains\") (subpath \"$USER_HOME/.config/subrouter\"))
(deny network-outbound (remote ip))"
fi

# --- stubs --------------------------------------------------------------------
stub() { # name body
  printf '#!/bin/bash\n%s\n' "$2" > "$C/bin/$1"
  chmod 755 "$C/bin/$1"
}

# Abort the whole run unless every command that could act on this machine
# resolves to its stub under the PATH the tools get.
SHADOWED="launchctl make go claude security sudo open"
assert_shadowed() { # PATH
  local name got
  for name in $SHADOWED; do
    got=$(PATH="$1" type -P "$name" 2>/dev/null)
    if [ "$got" != "$C/bin/$name" ]; then
      echo "ABORT $CASE: '$name' resolves to '${got:-nothing}', not its stub; refusing to run" >&2
      exit 99
    fi
  done
}

make_common_stubs() {
  local name
  # Every stub records that it ran, and acts on nothing.
  for name in $SHADOWED; do stub "$name" "echo \"$name \$*\" >> \"\$FAKE_LOG\"; exit 1"; done
  # launchctl answers as launchd does in a GUI terminal with no such job loaded.
  stub launchctl 'echo "launchctl $*" >> "$FAKE_LOG"; case "$1" in managername) echo Aqua; exit 0 ;; print) exit 113 ;; esac; exit 1'
}

any_exists() { local f; for f in "$@"; do [ -e "$f" ] && return 0; done; return 1; }

make_unreadable() { # path
  chmod 000 "$1"
  if [ -r "$1" ]; then bad "cannot make $1 unreadable (running as root?); the case proves nothing"; return 1; fi
  return 0
}

mode_of() { "$PY3" -c 'import os, sys; print(oct(os.stat(sys.argv[1]).st_mode & 0o777))' "$1"; }

# A LaunchAgent at the path the tools read, from a JSON dict of its keys.
write_plist() { # json
  mkdir -p "$H/Library/LaunchAgents"
  "$PY3" - "$H/Library/LaunchAgents/ai.manaflow.subrouter.plist" "$1" <<'PY'
import json, plistlib, sys
body = {"Label": "ai.manaflow.subrouter"}
body.update(json.loads(sys.argv[2]))
with open(sys.argv[1], "wb") as f:
    plistlib.dump(body, f)
PY
}

# --- assertions ---------------------------------------------------------------
ok() { PASS=$((PASS + 1)); }
bad() {
  FAILED=$((FAILED + 1)); FAIL_LIST="$FAIL_LIST $CASE"
  echo "  FAIL $CASE: $1"
}
expect_rc() { [ "$RC" = "$1" ] && ok || bad "exit $RC, want $1"; }
expect_out() { grep -Fq -- "$1" "$C/out" && ok || bad "output lacks: $1"; }
expect_no_out() { grep -Fq -- "$1" "$C/out" && bad "output has: $1" || ok; }
# A whole line of output, for a command printed for a person to copy.
expect_line() { grep -Fxq -- "$1" "$C/out" && ok || bad "output lacks the line: $1"; }
expect_log() { grep -Eq -- "$1" "$C/log" && ok || bad "log lacks /$1/"; }
expect_no_log() { grep -Eq -- "$1" "$C/log" && bad "log has /$1/" || ok; }
expect_file() { [ -f "$1" ] && ok || bad "no file at $1"; }
expect_no_file() { [ -e "$1" ] && bad "$1 exists" || ok; }
expect_mode() { local got; got=$(mode_of "$1" 2>/dev/null); [ "$got" = "$2" ] && ok || bad "$1 has mode '$got', want $2"; }
# remote.key opens the gate: a regular file, mode 600, at least 16 characters.
expect_key() { # path
  local body
  expect_file "$1"; expect_mode "$1" 0o600
  body=$(tr -d '[:space:]' < "$1" 2>/dev/null)
  [ "${#body}" -ge 16 ] && ok || bad "$1 holds ${#body} characters, want at least 16"
}

begin() { # case name; returns 1 when filtered out
  CASE="$1"
  case "$FILTER" in
    "") ;;
    =*) [ "$CASE" = "${FILTER#=}" ] || return 1 ;;
    *) [ "${CASE#*"$FILTER"}" != "$CASE" ] || return 1 ;;
  esac
  C="$T/$CASE"
  H="$C/home"
  mkdir -p "$C/bin" "$C/tmp" "$H"
  : > "$C/log"
  make_common_stubs
  return 0
}

finish() {
  if [ -n "$FAIL_LIST" ] && [ "${FAIL_LIST##* }" = "$CASE" ]; then
    echo "  --- $CASE output (tail) ---"; tail -25 "$C/out" | sed 's/^/  | /'
    echo "  --- $CASE log (tail) ---"; tail -15 "$C/log" | sed 's/^/  | /'
  else
    echo "  ok   $CASE"
  fi
}

run_sandboxed() { # env-assignments... -- command...
  local envs=() e
  while [ $# -gt 0 ] && [ "$1" != -- ]; do envs+=("$1"); shift; done
  shift
  for e in "${envs[@]}"; do case "$e" in PATH=*) assert_shadowed "${e#PATH=}" ;; esac; done
  if [ -n "$SANDBOX" ]; then
    env -i "${envs[@]}" sandbox-exec -p "$SANDBOX" "$@" > "$C/out" 2>&1
  else
    env -i "${envs[@]}" "$@" > "$C/out" 2>&1
  fi
  RC=$?
}

# The PATH, HOME and logs every tool runs with, then any extra assignments.
run_tool() { # tool [env-assignments...] [-- args...]
  local tool="$1" envs=() args=()
  shift
  while [ $# -gt 0 ] && [ "$1" != -- ]; do envs+=("$1"); shift; done
  [ $# -gt 0 ] && { shift; args=("$@"); }
  run_sandboxed PATH="$C/bin:/usr/bin:/bin:/usr/sbin:/sbin" HOME="$H" TMPDIR="$C/tmp" FAKE_LOG="$C/log" \
    ${envs[@]+"${envs[@]}"} -- "$PY3" "$tool" ${args[@]+"${args[@]}"}
}

# --- subrouter-remote-key -----------------------------------------------------
# The key goes where the daemon's gate reads it (cmd/subrouter/remote_gate.go,
# PATCHES §17), and --check applies the gate's four rules.
run_remote_key() { run_tool "$REMOTE_KEY" "$@"; }

echo "== subrouter-remote-key"

if begin remote-key-mints-where-the-gate-reads; then
  run_remote_key
  expect_rc 0; expect_out "wrote $H/.subrouter/codex/remote.key"
  expect_key "$H/.subrouter/codex/remote.key"
  run_remote_key -- --check
  expect_rc 0; expect_out "$H/.subrouter/codex/remote.key: present -- this machine can reach the remote"
  finish
fi

if begin remote-key-creates-a-private-store; then
  # The store's file names are account emails, and the daemon creates it 0700.
  # A mint on a fresh Mac comes before the daemon, so it creates the store.
  run_remote_key
  expect_rc 0; expect_mode "$H/.subrouter" 0o700; expect_mode "$H/.subrouter/codex" 0o700
  run_remote_key SUBROUTER_STATE_DIR="$C/deep/state"
  expect_rc 0; expect_mode "$C/deep" 0o700; expect_mode "$C/deep/state/codex" 0o700
  # A directory that is already there keeps its mode, as with the Go side's MkdirAll.
  mkdir -m 755 "$C/kept"
  run_remote_key SUBROUTER_STATE_DIR="$C/kept"
  expect_rc 0; expect_mode "$C/kept" 0o755; expect_mode "$C/kept/codex" 0o700
  finish
fi

if begin remote-key-honours-the-state-dir; then
  run_remote_key SUBROUTER_STATE_DIR="$C/state"
  expect_rc 0; expect_key "$C/state/codex/remote.key"
  expect_no_file "$H/.subrouter/codex/remote.key"
  finish
fi

if begin remote-key-mints-into-the-legacy-store; then
  # Before the daemon imports a pre-fork ~/.codex-accounts, that directory is
  # the store the gate reads, and a file in the empty <state>/codex would make
  # the import skip it. A lock file does not count as content.
  mkdir -p "$H/.codex-accounts" "$H/.subrouter/codex/accounts"
  echo '{}' > "$H/.codex-accounts/someone.json"
  : > "$H/.subrouter/codex/accounts/.batch.json.lock"
  run_remote_key
  expect_rc 0; expect_key "$H/.codex-accounts/remote.key"
  expect_no_file "$H/.subrouter/codex/remote.key"
  finish
fi

if begin remote-key-legacy-store-after-the-import; then
  # Calibration: once <state>/codex holds a file, or the import marker, it is
  # the store, legacy directory or not.
  mkdir -p "$H/.codex-accounts" "$H/.subrouter/codex/accounts"
  echo '{}' > "$H/.subrouter/codex/accounts/someone.json"
  run_remote_key
  expect_rc 0; expect_key "$H/.subrouter/codex/remote.key"
  rm -rf "$H/.subrouter"; mkdir -p "$H/.subrouter/codex"; : > "$H/.subrouter/codex/.legacy-codex-migrated"
  run_remote_key
  expect_rc 0; expect_key "$H/.subrouter/codex/remote.key"
  expect_no_file "$H/.codex-accounts/remote.key"
  finish
fi

if begin remote-key-check-applies-the-gate-rules; then
  K="$H/.subrouter/codex/remote.key"
  run_remote_key -- --check
  expect_rc 0; expect_out "$K: absent -- this machine is local-only"
  mkdir -p "$(dirname "$K")"
  printf '0123456789abcdef0123\n' > "$K"; chmod 644 "$K"
  run_remote_key -- --check
  expect_out "$K: present but mode 0644; the gate ignores it (chmod 600 it)"
  printf 'short\n' > "$K"; chmod 600 "$K"
  run_remote_key -- --check
  expect_out "$K: present but too short to be a key; the gate ignores it"
  # Eight two-byte characters: 16 bytes, which is how the gate counts.
  printf '\303\251%.0s' 1 2 3 4 5 6 7 8 > "$K"
  run_remote_key -- --check
  expect_out "$K: present -- this machine can reach the remote"
  rm -f "$K"; mkdir "$K"
  run_remote_key -- --check
  expect_out "$K: present but not a regular file; the gate ignores it"
  finish
fi

if begin remote-key-keeps-a-key-without-force; then
  mkdir -p "$H/.subrouter/codex"
  printf '0123456789abcdef0123456789abcdef\n' > "$H/.subrouter/codex/remote.key"; chmod 600 "$H/.subrouter/codex/remote.key"
  run_remote_key
  expect_rc 0; expect_out "Nothing to do. Pass --force to replace it."
  [ "$(cat "$H/.subrouter/codex/remote.key")" = 0123456789abcdef0123456789abcdef ] && ok || bad "the key changed without --force"
  run_remote_key -- --force
  expect_rc 0; expect_key "$H/.subrouter/codex/remote.key"
  [ "$(cat "$H/.subrouter/codex/remote.key")" != 0123456789abcdef0123456789abcdef ] && ok || bad "--force kept the old key"
  finish
fi

if begin remote-key-ignored-key-is-not-success; then
  # A key the gate ignores leaves the remote off, so keeping it is a failure a
  # calling script must see; --force replaces it with one that opens the gate.
  K="$H/.subrouter/codex/remote.key"
  mkdir -p "$(dirname "$K")"
  printf 'short\n' > "$K"; chmod 600 "$K"
  run_remote_key
  expect_rc 1; expect_out "$K: present but too short to be a key; the gate ignores it"
  expect_out "To fix it: SUBROUTER_STATE_DIR=$H/.subrouter $REMOTE_KEY --force"
  [ "$(cat "$K")" = short ] && ok || bad "the key changed without --force"
  printf '0123456789abcdef0123456789abcdef\n' > "$K"; chmod 644 "$K"
  run_remote_key
  expect_rc 1; expect_out "mode 0644"; expect_out "To fix it: chmod 600 $K"
  run_remote_key -- --force
  expect_rc 0; expect_key "$K"
  finish
fi

if begin remote-key-unreachable-store-is-not-a-key; then
  # A store this user cannot search hides whether a key is there, from the
  # gate too. The tool does not call the key present, and names what to look
  # at: --force writes through the same directory, so it is no fix.
  mkdir -p "$H/.subrouter/codex"
  if make_unreadable "$H/.subrouter/codex"; then
    run_remote_key
    expect_rc 1; expect_out "$H/.subrouter/codex/remote.key: not reachable"
    expect_out "To fix it: ls -ld $H/.subrouter/codex $H/.subrouter"
    expect_no_out "present"; expect_no_out "--force"
    run_remote_key -- --check
    expect_rc 0; expect_out "not reachable"; expect_no_out "present"
  fi
  finish
fi

if begin remote-key-refuses-a-directory; then
  # Opening a directory for writing would end in a traceback.
  mkdir -p "$H/.subrouter/codex/remote.key"
  run_remote_key -- --force
  expect_rc 1; expect_out "is not a regular file, so nothing was written"
  [ -d "$H/.subrouter/codex/remote.key" ] && ok || bad "the directory was replaced"
  finish
fi

if begin remote-key-remove; then
  run_remote_key
  run_remote_key -- --remove
  expect_rc 0; expect_out "removed $H/.subrouter/codex/remote.key"
  expect_no_file "$H/.subrouter/codex/remote.key"
  run_remote_key -- --remove
  expect_rc 0; expect_out "already absent"
  # The gate ignores a directory, so the remote is already off: say so, and
  # leave the directory, which may hold someone's files.
  mkdir -p "$H/.subrouter/codex/remote.key"
  run_remote_key -- --remove
  expect_rc 0; expect_out "is not a regular file, so nothing was removed"; expect_no_out Traceback
  [ -d "$H/.subrouter/codex/remote.key" ] && ok || bad "the directory was removed"
  finish
fi

if begin remote-key-no-launchagent-no-restart-hint; then
  # A fresh Mac has no daemon yet: launchctl lines there only produce errors.
  run_remote_key
  expect_rc 0; expect_out "wrote $H/.subrouter/codex/remote.key"
  expect_no_out "launchctl"; expect_out "no LaunchAgent"
  # The plist is all it checks, so it must not claim no daemon runs here at all.
  expect_out "restart it"
  expect_no_log .
  finish
fi

if begin remote-key-restart-hint-with-launchagent; then
  write_plist "{\"ProgramArguments\": [\"$H/bin/subrouter\", \"serve\", \"--addr\", \"127.0.0.1:31415\"]}"
  run_remote_key
  expect_rc 0
  # Only the key is new, so a restart that does not re-read the plist is enough,
  # and it has no bootout to wait out before a bootstrap.
  expect_line "  launchctl kickstart -k gui/\$(id -u)/ai.manaflow.subrouter"
  expect_no_out "bootstrap"
  expect_no_log .
  finish
fi

if begin remote-key-supervised-hint-deploys-the-worker; then
  # A supervised job starts a new worker on a deploy, and the worker reads the
  # key; restarting the supervisor would close the port for nothing.
  write_plist "{\"ProgramArguments\": [\"$H/bin/subrouter-supervisor\", \"supervise\", \"--worker-bin\", \"$H/bin/subrouter\", \"--\", \"serve\"]}"
  run_remote_key
  expect_rc 0
  expect_line "  $OPS/deploy/macos/deploy-laptop.sh $H/bin/subrouter"
  expect_no_out "launchctl bootout"
  finish
fi

if begin remote-key-hint-names-the-daemon-store; then
  # A plist that names its own state dir is where the daemon reads the key, so
  # a mint into this shell's store arms nothing there: the tool says so and
  # names the command for the daemon's store, not a restart.
  write_plist "{\"ProgramArguments\": [\"$H/bin/subrouter\", \"serve\"], \"EnvironmentVariables\": {\"HOME\": \"$H\", \"SUBROUTER_STATE_DIR\": \"$H/alt-state\"}}"
  run_remote_key
  expect_rc 0; expect_out "wrote $H/.subrouter/codex/remote.key"
  expect_out "The LaunchAgent's daemon reads $H/alt-state/codex/remote.key, not this file, and that one is absent"
  expect_line "  SUBROUTER_STATE_DIR=$H/alt-state $REMOTE_KEY"
  expect_no_out "kickstart"
  run_remote_key -- --check
  expect_rc 0; expect_out "$H/alt-state/codex/remote.key: absent -- this machine is local-only (the LaunchAgent's daemon reads this one)"
  # Minted where the daemon reads it, the restart is what is left.
  run_remote_key SUBROUTER_STATE_DIR="$H/alt-state"
  expect_rc 0; expect_line "  launchctl kickstart -k gui/\$(id -u)/ai.manaflow.subrouter"
  expect_no_out "not this file"
  finish
fi

# --- subrouter-unbrick --------------------------------------------------------
# A client pointed at a daemon that no longer answers has no login to fall
# back on; unbrick takes it off, from the paths the Go writers use.
UNBRICK="$OPS/bin/subrouter-unbrick"
run_unbrick() { run_tool "$UNBRICK" "$@"; }
claude_env() { # settings.json -> its env keys, sorted, one line
  "$PY3" -c 'import json, sys; print(" ".join(sorted((json.load(open(sys.argv[1])).get("env") or {}))))' "$1"
}
routed_settings() { # path
  mkdir -p "$(dirname "$1")"
  printf '{"env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:31415", "ANTHROPIC_AUTH_TOKEN": "local-proxy-token-value", "AWS_PROFILE": "me"}, "model": "opus"}\n' > "$1"
}
routed_codex() { # path
  mkdir -p "$(dirname "$1")"
  cat > "$1" <<'TOML'
model = "gpt-5"
openai_base_url = "http://127.0.0.1:31415/v1"
chatgpt_base_url = "http://127.0.0.1:31415/backend-api"
experimental_realtime_ws_base_url = "ws://127.0.0.1:31415/v1"
experimental_realtime_webrtc_call_base_url = "http://127.0.0.1:31415/v1"
TOML
}
active_codex_keys() { grep -cE '^[a-z_]*base_url *=' "$1"; }

echo "== subrouter-unbrick"

if begin unbrick-removes-only-the-anthropic-keys; then
  routed_settings "$H/.claude/settings.json"
  run_unbrick
  expect_rc 0; expect_out "removed ['ANTHROPIC_BASE_URL', 'ANTHROPIC_AUTH_TOKEN']"
  [ "$(claude_env "$H/.claude/settings.json")" = AWS_PROFILE ] && ok || bad "env is '$(claude_env "$H/.claude/settings.json")', want only AWS_PROFILE"
  "$PY3" -c 'import json, sys; sys.exit(json.load(open(sys.argv[1])).get("model") != "opus")' "$H/.claude/settings.json" \
    && ok || bad "a setting outside env was lost"
  any_exists "$H/.claude/settings.json.subrouter-bak-"* && ok || bad "no backup was taken"
  finish
fi

if begin unbrick-disables-every-codex-routing-key; then
  # The four keys cmd/subrouter/codex_config.go writes, and nothing else.
  routed_codex "$H/.codex/config.toml"
  run_unbrick
  expect_rc 0; expect_out "disabled 4 line(s)"
  [ "$(active_codex_keys "$H/.codex/config.toml")" = 0 ] && ok || bad "a base URL is still active"
  grep -qx 'model = "gpt-5"' "$H/.codex/config.toml" && ok || bad "an unrelated key was touched"
  grep -qx '# disabled-by-unbrick experimental_realtime_webrtc_call_base_url = "http://127.0.0.1:31415/v1"' "$H/.codex/config.toml" \
    && ok || bad "the webrtc key was not commented out in place"
  finish
fi

if begin unbrick-follows-claude-config-dir-and-codex-home; then
  # The directories the clients and `sr setup` read, not ~/.claude and ~/.codex.
  routed_settings "$C/claude/settings.json"; routed_codex "$C/codex/config.toml"
  routed_settings "$H/.claude/settings.json"; cp "$H/.claude/settings.json" "$C/home-before.json"
  run_unbrick CLAUDE_CONFIG_DIR="$C/claude" CODEX_HOME="$C/codex"
  expect_rc 0
  [ "$(claude_env "$C/claude/settings.json")" = AWS_PROFILE ] && ok || bad "CLAUDE_CONFIG_DIR's settings.json still routes"
  [ "$(active_codex_keys "$C/codex/config.toml")" = 0 ] && ok || bad "CODEX_HOME's config.toml still routes"
  cmp -s "$H/.claude/settings.json" "$C/home-before.json" && ok || bad "~/.claude/settings.json was changed"
  finish
fi

if begin unbrick-reports-shell-exports-and-edits-none; then
  # With settings.json clean, an exported base URL routes Claude Code. It is
  # named, never edited.
  printf 'alias ll="ls -l"\nexport ANTHROPIC_BASE_URL=http://127.0.0.1:31415\n' > "$H/.zshrc"
  cp "$H/.zshrc" "$C/zshrc-before"
  run_unbrick
  expect_rc 0
  expect_out "~/.zshrc:2: export ANTHROPIC_BASE_URL=http://127.0.0.1:31415"
  expect_out "those shell exports ARE the problem"
  cmp -s "$H/.zshrc" "$C/zshrc-before" && ok || bad "~/.zshrc was edited"
  finish
fi

if begin unbrick-leaves-a-settings-json-it-cannot-read; then
  # A settings.json it cannot rewrite is left for a person, and the rest of the
  # repair still runs: Codex and the shell are checked too.
  mkdir -p "$H/.claude"
  for body in '[]' '{"env": "x"}' '{"env": ["ANTHROPIC_BASE_URL"]}'; do
    printf '%s\n' "$body" > "$H/.claude/settings.json"; cp "$H/.claude/settings.json" "$C/before.json"
    routed_codex "$H/.codex/config.toml"
    run_unbrick
    expect_rc 0; expect_out "is not a JSON object with an object \"env\"; fix it by hand"; expect_no_out "Traceback"
    [ "$(active_codex_keys "$H/.codex/config.toml")" = 0 ] && ok || bad "config.toml was not repaired after '$body'"
    expect_out "shell:   clean"
    cmp -s "$H/.claude/settings.json" "$C/before.json" && ok || bad "settings.json '$body' was changed"
  done
  finish
fi

if begin unbrick-check-touches-nothing; then
  routed_settings "$H/.claude/settings.json"; routed_codex "$H/.codex/config.toml"
  cp "$H/.claude/settings.json" "$C/settings-before"; cp "$H/.codex/config.toml" "$C/codex-before"
  run_unbrick -- --check
  expect_rc 0; expect_out "would remove"; expect_out "would disable 4 line(s)"; expect_out "--check: nothing was modified."
  cmp -s "$H/.claude/settings.json" "$C/settings-before" && ok || bad "settings.json changed under --check"
  cmp -s "$H/.codex/config.toml" "$C/codex-before" && ok || bad "config.toml changed under --check"
  any_exists "$H/.claude/settings.json.subrouter-bak-"* "$H/.codex/config.toml.subrouter-bak-"* && bad "--check took a backup" || ok
  finish
fi

if begin unbrick-restore-puts-back-the-newest-backup; then
  routed_settings "$H/.claude/settings.json"; routed_codex "$H/.codex/config.toml"
  cp "$H/.claude/settings.json" "$C/settings-before"; cp "$H/.codex/config.toml" "$C/codex-before"
  printf '{"env": {"OLDER": "1"}}\n' > "$H/.claude/settings.json.subrouter-bak-20200101-000000"
  run_unbrick
  run_unbrick -- --restore
  expect_rc 0
  cmp -s "$H/.claude/settings.json" "$C/settings-before" && ok || bad "settings.json is not the newest backup"
  cmp -s "$H/.codex/config.toml" "$C/codex-before" && ok || bad "config.toml is not the newest backup"
  finish
fi

# --- subrouter-quickstart -----------------------------------------------------
# The laptop bootstrap. The driver loads the script as a module and replaces
# its probes of this machine: daemon_alive (FAKE_DAEMON_UP=1 is a daemon
# that answers, FAKE_DAEMON_FILE one that answers once that file exists,
# FAKE_DAEMON_AFTER=N one that answers from the (N+1)th probe), the accounts
# route (FAKE_ACCOUNTS is its JSON body), and claude_turn
# (FAKE_TURN=ok|fail|timeout; unset, the real probe runs the fake `claude`
# that stub_claude describes). FAKE_QS_CALL runs one phase alone:
# configure_peer (phase 3's peer step, as it runs after setup), cutover (phase
# 6) or install (phase 3). FAKE_DIE_AT=chmod ends the process at its first
# chmod, as a kill there would. FAKE_QS_BIN stands in for phase 2's build,
# FAKE_QS_STUB="<phase> ..." makes those phases print their name and return,
# FAKE_CWD is the directory the script runs from, and FAKE_TIMEOUT_CAP=N cuts
# every timeout the script gives a command to N seconds. Waits do not sleep.
# SUBROUTER_LOCAL_BASE_URL points every other request at a closed port, so the
# daemon on this machine never answers for a case.
QUICKSTART="$OPS/bin/subrouter-quickstart"
SETTINGS_REL=".claude/settings.json"
MODGO=$(sed -n 's/^go //p' "$ROOT/go.mod" | head -1)
PEER_URL="https://box.example.com:31416"
# `claude -p` against a fake daemon on 127.0.0.1:1. A turn takes the process
# env, then settings.json's env, then a --settings file's, each over the one
# before, as Claude Code 2.1.330 does. A turn whose base URL is not the daemon
# is answered elsewhere: the marker, and no line in the daemon's log. One that
# reaches it needs FAKE_DAEMON_TOKEN (the local proxy token by default) and is
# served as FAKE_SERVE says: local (a proxy request line), rotated (that line
# in LOG.1 only), peer (a remote-pool line, no account), error (the line, then
# an upstream error and no marker) or elsewhere (it never reaches the daemon).
# Each call appends what it was given to FAKE_LOG.
stub_claude() {
  { printf '#!%s\n' "$PY3"; cat <<'PY'; } > "$C/bin/claude"
import json, os, stat, sys, time
argv = sys.argv[1:]
flag = argv[argv.index("--settings") + 1] if "--settings" in argv else None
env = dict(os.environ)
user = os.path.join(os.environ.get("CLAUDE_CONFIG_DIR") or os.path.expanduser("~/.claude"), "settings.json")
for path in (user, flag):
    if path and os.path.isfile(path):
        env.update(json.load(open(path)).get("env") or {})
base = str(env.get("ANTHROPIC_BASE_URL") or "").rstrip("/")
token = env.get("ANTHROPIC_AUTH_TOKEN") or ""
want = os.environ.get("FAKE_DAEMON_TOKEN", "local-proxy-token-value")
with open(os.environ["FAKE_LOG"], "a") as log:
    log.write("claude %s | settings-file=%s env=%s base=%s token=%s\n" % (
        " ".join(argv),
        oct(stat.S_IMODE(os.stat(flag).st_mode)) if flag and os.path.isfile(flag) else "none",
        ",".join(sorted(k for k in os.environ if k.startswith("ANTHROPIC_"))) or "none",
        base or "unset",
        "daemon" if token == want else "other" if token else "unset"))
serve = os.environ.get("FAKE_SERVE", "local")
if base != "http://127.0.0.1:1" or serve == "elsewhere":
    print("subrouter-ok")
    sys.exit(0)
if token != want:
    print('API Error: 401 {"error": "invalid local proxy token"}', file=sys.stderr)
    sys.exit(1)
now = time.strftime("%Y/%m/%d %H:%M:%S")
if serve == "peer":
    line = f"{now} INFO serving from remote pool remote=box agent=claude status=200\n"
else:
    line = f"{now} INFO proxy request agent=claude session=s1 account=acct-1 method=POST path=/v1/messages\n"
daemon_log = os.path.expanduser("~/Library/Logs/subrouter.err.log") + (".1" if serve == "rotated" else "")
os.makedirs(os.path.dirname(daemon_log), exist_ok=True)
with open(daemon_log, "a") as out:
    out.write(line)
if serve == "error":
    print("API Error: 529 Overloaded", file=sys.stderr)
    sys.exit(1)
print("subrouter-ok")
PY
  chmod 755 "$C/bin/claude"
}
qs_setup() {
  mkdir -p "$H/Library/LaunchAgents"
  # The real go is not on the harness PATH. This one answers as Go 1.21+ does:
  # inside a module, and unless GOTOOLCHAIN=local, `go version` reports the
  # toolchain go.mod names (it would fetch it); anywhere else, the local one
  # (FAKE_LOCAL_GO). `go version -m` reads `# vcs.*=` lines from a fake binary.
  # FAKE_GO_HANG=1 is a fetch that stalls.
  stub go '
case "$1" in
  version)
    if [ "${2:-}" = -m ]; then sed -n "s/^# \(vcs\.[a-z]*=.*\)/	build	\1/p" "$3"; exit 0; fi
    if [ -f go.mod ] && [ -n "${FAKE_GO_HANG:-}" ]; then exec /bin/sleep 30; fi
    if [ -f go.mod ] && [ "${GOTOOLCHAIN:-auto}" != local ]; then v="go$(sed -n "s/^go //p" go.mod)"; else v="${FAKE_LOCAL_GO:-go1.26.0}"; fi
    if [ -f go.mod ] && [ -n "${FAKE_GO_FETCH_FAILS:-}" ]; then echo "go: download $v for darwin/arm64: toolchain not available" >&2; exit 1; fi
    echo "go version $v darwin/arm64" ;;
  env) echo "${GOTOOLCHAIN:-auto}" ;;
  *) exit 2 ;;
esac'
  echo "peer-proxy-token" > "$C/peer-token"
  stub_claude
  cat > "$C/qs-driver.py" <<'PY'
import importlib.machinery, importlib.util, json, os, sys
sys.dont_write_bytecode = True  # no __pycache__ beside the script in the checkout
loader = importlib.machinery.SourceFileLoader("quickstart", sys.argv[1])
qs = importlib.util.module_from_spec(importlib.util.spec_from_loader("quickstart", loader))
loader.exec_module(qs)
# Phase 1 refuses anything but macOS. Every command a case can reach is a stub,
# so each case runs as on macOS, on any OS. FAKE_PLATFORM names another.
sys.platform = os.environ.get("FAKE_PLATFORM", "darwin")
qs.daemon_alive = lambda: os.environ.get("FAKE_DAEMON_UP") == "1" or os.path.exists(os.environ.get("FAKE_DAEMON_FILE", "/nonexistent"))
if os.environ.get("FAKE_DAEMON_AFTER"):
    polls = iter(range(int(os.environ["FAKE_DAEMON_AFTER"])))
    qs.daemon_alive = lambda: next(polls, None) is None
if os.environ.get("FAKE_ACCOUNTS"):
    real_get_json = qs.get_json
    def get_json(path, token=None, timeout=5):
        if path == "/_subrouter/accounts":
            return json.loads(os.environ["FAKE_ACCOUNTS"])
        return real_get_json(path, token, timeout)
    qs.get_json = get_json
qs.time.sleep = lambda seconds: None
if os.environ.get("FAKE_TURN") == "timeout":
    def timed_out(settings_env=None):
        raise qs.subprocess.TimeoutExpired(["claude", "-p", "fake"], 180)
    qs.claude_turn = timed_out
elif os.environ.get("FAKE_TURN"):
    served = os.environ["FAKE_TURN"] == "ok"
    qs.claude_turn = lambda settings_env=None: (served, "" if served else "fake: no turn served", "local:fake" if served else None)
if os.environ.get("FAKE_QS_BIN"):
    qs.BIN = os.environ["FAKE_QS_BIN"]
for name in os.environ.get("FAKE_QS_STUB", "").split():
    setattr(qs, name, lambda *a, _name=name, **k: print(f"  (stubbed {_name})"))
if os.environ.get("FAKE_CWD"):
    os.chdir(os.environ["FAKE_CWD"])
if os.environ.get("FAKE_DIE_AT") == "chmod":
    def killed(*args, **kwargs):
        raise SystemExit("killed at chmod")
    qs.os.chmod = killed
if os.environ.get("FAKE_TIMEOUT_CAP"):
    cap, real_run = float(os.environ["FAKE_TIMEOUT_CAP"]), qs.run
    qs.run = lambda argv, **kw: real_run(argv, **(dict(kw, timeout=min(kw["timeout"], cap)) if "timeout" in kw else kw))
sys.argv = sys.argv[1:]
call = os.environ.get("FAKE_QS_CALL")
if call:
    try:
        if call == "configure_peer":
            qs.configure_peer("https://box.example.com:31416", "peer-proxy-token", "ab12", False)
        elif call == "cutover":
            qs.cutover(False)
        elif call == "install":
            qs.install(False, None)
    except qs.Stop as stop:
        print(f"STOPPED: {stop}")
        sys.exit(stop.code)
    sys.exit(0)
sys.exit(qs.main(sys.argv[1:]))
PY
}
qs_plist_has() { # key
  "$PY3" -c 'import plistlib, sys; sys.exit(0 if sys.argv[2] in (plistlib.load(open(sys.argv[1], "rb")).get("EnvironmentVariables") or {}) else 1)' \
    "$H/Library/LaunchAgents/ai.manaflow.subrouter.plist" "$1"
}
seed_remote_key() { # state-dir [mode] [contents]
  mkdir -p "$1/codex"
  printf '%s\n' "${3:-0123456789abcdef0123456789abcdef}" > "$1/codex/remote.key"
  chmod "${2:-600}" "$1/codex/remote.key"
}
run_qs() { # extra env assignments... [-- script args]; --check --peer ... unless `--` is given
  local envs=() args=(--check --peer "$PEER_URL" --peer-fingerprint ab12 --peer-token-file "$C/peer-token")
  while [ $# -gt 0 ] && [ "$1" != -- ]; do envs+=("$1"); shift; done
  if [ $# -gt 0 ]; then shift; args=("$@"); fi
  run_tool "$C/qs-driver.py" SUBROUTER_LOCAL_BASE_URL=http://127.0.0.1:1 ${envs[@]+"${envs[@]}"} \
    -- "$QUICKSTART" ${args[@]+"${args[@]}"}
}
# A whole run from phase 4, with one local account and the real probes.
run_qs_turns() { run_qs FAKE_QS_STUB="preflight build install" FAKE_ACCOUNTS='[{"id": "acct-1"}]' "$@"; }
# What the Nth `claude` call was given, as the fake records it.
claude_call() { grep '^claude ' "$C/log" | sed -n "${1}p"; }
qs_cloud() {
  mkdir -p "$H/.config/subrouter"
  printf '{"localProxyToken": "local-proxy-token-value"}\n' > "$H/.config/subrouter/cloud.json"
}
# settings.json's env.ANTHROPIC_BASE_URL, "unset", or "absent" for no file.
qs_base_url() { # [settings.json]
  "$PY3" -c 'import json, os, sys
p = sys.argv[1]
print(((json.load(open(p)).get("env") or {}).get("ANTHROPIC_BASE_URL") or "unset") if os.path.exists(p) else "absent")' "${1:-$H/$SETTINGS_REL}"
}
expect_base_url() { # want [settings.json]
  local got; got=$(qs_base_url "${2:-}")
  [ "$got" = "$1" ] && ok || bad "settings.json ANTHROPIC_BASE_URL is '$got', want '$1'"
}
# settings.json's env.CLAUDE_CODE_PROMPT_CACHE_TTL, or "unset".
expect_ttl() { # want why
  local got
  got=$("$PY3" -c 'import json, sys; print((json.load(open(sys.argv[1])).get("env") or {}).get("CLAUDE_CODE_PROMPT_CACHE_TTL", "unset"))' "$H/$SETTINGS_REL")
  [ "$got" = "$1" ] && ok || bad "$2: CLAUDE_CODE_PROMPT_CACHE_TTL is '$got', want '$1'"
}
fake_binary() { # path [extra lines]
  mkdir -p "$(dirname "$1")"
  printf '#!/bin/bash\n# fake subrouter\n%s\n' "${2:-}" > "$1"; chmod 755 "$1"
}

echo "== subrouter-quickstart"

if begin quickstart-refuses-off-macos; then
  # launchd is the daemon's supervisor here, so phase 1 stops anywhere else,
  # before the build, and names the Linux box's deploy.
  qs_setup; run_qs FAKE_PLATFORM=linux --
  expect_rc 1; expect_out "this script is macOS-only (launchd); for a Linux box see ops/deploy/lightsail/"
  expect_no_out "== 2/6"; expect_no_log "^make"; expect_no_log "launchctl"
  finish
fi

if begin quickstart-peer-refuses-without-remote-key; then
  qs_setup; run_qs
  expect_rc 1
  expect_out "--peer needs this machine to hold remote.key, and $H/.subrouter/codex/remote.key is absent"
  expect_out "SUBROUTER_STATE_DIR=$H/.subrouter $OPS/bin/subrouter-remote-key"
  expect_no_out "== 2/6"
  finish
fi

if begin quickstart-peer-stops-before-build; then
  # Not --check: the refusal must come before phase 2 builds or phase 3 installs.
  qs_setup
  run_qs -- --peer "$PEER_URL" --peer-fingerprint ab12 --peer-token-file "$C/peer-token"
  expect_rc 1; expect_out "needs this machine to hold remote.key"
  expect_no_log "^make"; expect_no_log "launchctl"
  if any_exists "$H/Library/LaunchAgents/"*; then bad "a LaunchAgent was written"; else ok; fi
  finish
fi

if begin quickstart-peer-needs-a-token-file; then
  # The token comes from a file only: argv is readable through ps.
  qs_setup; run_qs -- --check --peer "$PEER_URL" --peer-fingerprint ab12
  expect_rc 2; expect_out "--peer needs --peer-token-file"; expect_no_out "== 1/6"
  : > "$C/empty-token"
  run_qs -- --check --peer "$PEER_URL" --peer-fingerprint ab12 --peer-token-file "$C/empty-token"
  expect_rc 2; expect_out "$C/empty-token is empty"; expect_no_out "== 1/6"
  finish
fi

if begin quickstart-peer-https-needs-a-fingerprint; then
  # An unpinned client fails CA validation against the box's self-signed
  # listener. Refused at the command line, before phase 3's setup installs a
  # daemon that its peer step would then refuse to peer.
  qs_setup; seed_remote_key "$H/.subrouter"
  run_qs -- --check --peer "$PEER_URL" --peer-token-file "$C/peer-token"
  expect_rc 2; expect_out "an https peer is self-signed"; expect_no_out "== 1/6"
  # A plain-http tailnet peer needs no pin.
  run_qs -- --check --peer http://100.64.0.1:31415 --peer-token-file "$C/peer-token"
  expect_rc 0; expect_out "would add to the LaunchAgent: SUBROUTER_REMOTE_POOL, SUBROUTER_REMOTE_POOL_TOKEN"
  finish
fi

if begin quickstart-peer-key-mode-refused; then
  qs_setup; seed_remote_key "$H/.subrouter" 644; run_qs
  expect_rc 1; expect_out "remote.key is mode 0644"; expect_out "chmod 600 $H/.subrouter/codex/remote.key"
  finish
fi

if begin quickstart-peer-key-too-short; then
  qs_setup; seed_remote_key "$H/.subrouter" 600 short; run_qs
  expect_rc 1; expect_out "remote.key is too short to be a key"; expect_out "subrouter-remote-key --force"
  finish
fi

if begin quickstart-peer-with-key-proceeds; then
  qs_setup; seed_remote_key "$H/.subrouter"; run_qs
  expect_rc 0; expect_out "remote:  $H/.subrouter/codex/remote.key present"
  expect_out "would add to the LaunchAgent: SUBROUTER_REMOTE_POOL, SUBROUTER_REMOTE_POOL_FINGERPRINT, SUBROUTER_REMOTE_POOL_TOKEN"
  expect_out "--check: nothing was modified"
  finish
fi

if begin quickstart-peer-key-where-the-daemon-reads-it; then
  # This shell's SUBROUTER_STATE_DIR never reaches launchd: a key there arms
  # nothing, so it is not accepted.
  qs_setup; seed_remote_key "$C/shell-state"
  run_qs SUBROUTER_STATE_DIR="$C/shell-state"
  expect_rc 1; expect_out "and $H/.subrouter/codex/remote.key is absent"
  finish
fi

if begin quickstart-peer-key-from-plist-state-dir; then
  # A plist that names a state dir is where the daemon looks.
  qs_setup; seed_remote_key "$H/alt-state"
  write_plist "{\"EnvironmentVariables\": {\"HOME\": \"$H\", \"SUBROUTER_STATE_DIR\": \"$H/alt-state\"}}"
  run_qs FAKE_DAEMON_UP=1
  expect_rc 0; expect_out "remote:  $H/alt-state/codex/remote.key present"
  finish
fi

if begin quickstart-peer-plist-state-dir-survives-setup; then
  # The daemon does not answer, so phase 3's setup writes the plist again. It
  # keeps every key but HOME and PATH (PATCHES §37), so the daemon still reads
  # the state dir the old plist names, and a key only in ~/.subrouter arms nothing.
  qs_setup; seed_remote_key "$H/.subrouter"
  write_plist "{\"EnvironmentVariables\": {\"HOME\": \"$H\", \"SUBROUTER_STATE_DIR\": \"$H/alt-state\"}}"
  run_qs
  expect_rc 1; expect_out "and $H/alt-state/codex/remote.key is absent"
  expect_out "SUBROUTER_STATE_DIR=$H/alt-state $OPS/bin/subrouter-remote-key"
  expect_no_out "the peer can arm"
  finish
fi

if begin quickstart-peer-plist-home-replaced-by-setup; then
  # setup writes HOME and PATH into the plist again and keeps the rest (PATCHES
  # §37). A plist that names another HOME and no state dir is read from that
  # HOME while it is kept, and from this one once setup has written it.
  qs_setup; seed_remote_key "$C/other-home/.subrouter"
  write_plist "{\"EnvironmentVariables\": {\"HOME\": \"$C/other-home\"}}"
  run_qs FAKE_DAEMON_UP=1
  expect_rc 0; expect_out "remote:  $C/other-home/.subrouter/codex/remote.key present"
  run_qs
  expect_rc 1; expect_out "and $H/.subrouter/codex/remote.key is absent"
  finish
fi

if begin quickstart-peer-key-from-the-worker-config; then
  # A supervised worker gets its --worker-config env over the plist's, so a
  # state dir set there is where the daemon reads the key.
  qs_setup; seed_remote_key "$H/worker-state"; mkdir -p "$H/.subrouter"
  printf '{"args": ["serve"], "env": {"SUBROUTER_STATE_DIR": "%s"}}\n' "$H/worker-state" > "$H/.subrouter/worker-config.json"
  write_plist "{\"ProgramArguments\": [\"$H/bin/subrouter-supervisor\", \"supervise\", \"--worker-bin\", \"$H/bin/subrouter\", \"--worker-config\", \"$H/.subrouter/worker-config.json\", \"--\", \"serve\"], \"EnvironmentVariables\": {\"HOME\": \"$H\"}}"
  run_qs FAKE_DAEMON_UP=1
  expect_rc 0; expect_out "remote:  $H/worker-state/codex/remote.key present"
  # Calibration: with no daemon answering, setup writes a plain serve job,
  # and the worker config no longer applies.
  run_qs
  expect_rc 1; expect_out "and $H/.subrouter/codex/remote.key is absent"
  finish
fi

if begin quickstart-peer-key-in-the-legacy-store; then
  # Before its first start imports a pre-fork ~/.codex-accounts, the daemon
  # reads the key there; the rule is subrouter-remote-key's.
  qs_setup; mkdir -p "$H/.codex-accounts"; echo '{}' > "$H/.codex-accounts/someone.json"
  run_qs
  expect_rc 1; expect_out "and $H/.codex-accounts/remote.key is absent"
  printf '0123456789abcdef0123456789abcdef\n' > "$H/.codex-accounts/remote.key"; chmod 600 "$H/.codex-accounts/remote.key"
  run_qs
  expect_rc 0; expect_out "remote:  $H/.codex-accounts/remote.key present"
  finish
fi

if begin quickstart-peer-defers-to-a-peers-file; then
  # With SUBROUTER_REMOTE_POOLS_FILE in its env, serve takes its peers from that
  # file and ignores the single-peer variables, which are all --peer writes
  # (PATCHES §31). So --peer stops before the build, and the peer step before
  # it writes, and both name the file the box goes in.
  qs_setup; seed_remote_key "$H/.subrouter"
  P="$H/Library/LaunchAgents/ai.manaflow.subrouter.plist"
  write_plist "{\"EnvironmentVariables\": {\"HOME\": \"$H\", \"SUBROUTER_REMOTE_POOLS_FILE\": \"$H/.subrouter/peers.json\"}}"
  cp "$P" "$C/plist-before"
  run_qs FAKE_DAEMON_UP=1
  expect_rc 1; expect_out "the daemon takes its peers from $H/.subrouter/peers.json (SUBROUTER_REMOTE_POOLS_FILE)"
  expect_no_out "== 2/6"
  # setup keeps the variable when it writes the plist again (PATCHES §37).
  run_qs
  expect_rc 1; expect_out "takes its peers from $H/.subrouter/peers.json"
  run_qs FAKE_QS_CALL=configure_peer
  expect_rc 1; expect_out "takes its peers from $H/.subrouter/peers.json"; expect_no_log "^launchctl boot"
  cmp -s "$P" "$C/plist-before" && ok || bad "the plist was written"
  # A supervised worker gets its --worker-config env over the plist's.
  printf '{"args": ["serve"], "env": {"SUBROUTER_REMOTE_POOLS_FILE": "%s"}}\n' "$H/peers.json" > "$H/.subrouter/worker-config.json"
  write_plist "{\"ProgramArguments\": [\"$H/bin/subrouter-supervisor\", \"supervise\", \"--worker-bin\", \"$H/bin/subrouter\", \"--worker-config\", \"$H/.subrouter/worker-config.json\", \"--\", \"serve\"], \"EnvironmentVariables\": {\"HOME\": \"$H\"}}"
  run_qs FAKE_DAEMON_UP=1
  expect_rc 1; expect_out "takes its peers from $H/peers.json"
  # Calibration: serve reads a blank value as unset.
  write_plist "{\"EnvironmentVariables\": {\"HOME\": \"$H\", \"SUBROUTER_REMOTE_POOLS_FILE\": \" \"}}"
  run_qs FAKE_DAEMON_UP=1
  expect_rc 0; expect_no_out "takes its peers from"
  finish
fi

if begin quickstart-peer-key-before-the-peers-file; then
  # The peers-file stop sends the run back without --peer, and a daemon with
  # no key ignores the file too. So a keyless machine hears about the key
  # first, in phase 1 and in the peer step. The case above is the calibration.
  qs_setup
  P="$H/Library/LaunchAgents/ai.manaflow.subrouter.plist"
  write_plist "{\"EnvironmentVariables\": {\"HOME\": \"$H\", \"SUBROUTER_REMOTE_POOLS_FILE\": \"$H/.subrouter/peers.json\"}}"
  cp "$P" "$C/plist-before"
  run_qs FAKE_DAEMON_UP=1
  expect_rc 1; expect_out "--peer needs this machine to hold remote.key, and $H/.subrouter/codex/remote.key is absent"
  expect_no_out "takes its peers from"; expect_no_out "== 2/6"
  run_qs FAKE_QS_CALL=configure_peer
  expect_rc 1; expect_out "--peer needs this machine to hold remote.key"; expect_no_out "takes its peers from"
  cmp -s "$P" "$C/plist-before" && ok || bad "the plist was written"
  finish
fi

if begin quickstart-peers-file-is-a-pool; then
  # The stop above sends the box to the peers file and the run back without
  # --peer. Phase 4 then counts the peers file as the pool, as it counts
  # --peer, once remote.key opens the gate the daemon reads the file through.
  qs_setup; qs_cloud; seed_remote_key "$H/.subrouter"
  write_plist "{\"EnvironmentVariables\": {\"HOME\": \"$H\", \"SUBROUTER_REMOTE_POOLS_FILE\": \"$H/.subrouter/peers.json\"}}"
  run_qs_turns FAKE_ACCOUNTS='[]' FAKE_SERVE=peer --
  expect_rc 0; expect_out "pool:    0 local accounts, which is fine: the peer is the pool"; expect_out "served:  a full turn -- peer"
  # Without the key the daemon ignores the file, so the pool is empty at both
  # ends: phase 4 names the key, and no probe runs.
  rm "$H/.subrouter/codex/remote.key"; : > "$C/log"
  run_qs_turns FAKE_ACCOUNTS='[]' FAKE_SERVE=peer --
  expect_rc 1
  expect_out "The peers file $H/.subrouter/peers.json needs this machine to hold remote.key, and $H/.subrouter/codex/remote.key is absent"
  expect_out "SUBROUTER_STATE_DIR=$H/.subrouter $OPS/bin/subrouter-remote-key"
  expect_no_out "== 5/6"; expect_no_log "^claude "
  # Calibration: local accounts serve without the key, peers file or not.
  run_qs_turns --
  expect_rc 0; expect_out "pool:    1 local account(s)"; expect_no_out "remote.key"
  write_plist "{\"EnvironmentVariables\": {\"HOME\": \"$H\"}}"
  run_qs_turns FAKE_ACCOUNTS='[]' FAKE_SERVE=peer --
  expect_rc 3; expect_out "the pool is empty"
  finish
fi

if begin quickstart-peer-step-rechecks-the-plist-it-writes; then
  # Phase 3's peer step asks again, against the plist launchd is about to
  # read, before the peer goes into it: here one whose store holds no key.
  qs_setup; seed_remote_key "$H/.subrouter"
  write_plist "{\"EnvironmentVariables\": {\"HOME\": \"$H\", \"SUBROUTER_STATE_DIR\": \"$H/keyless\"}}"
  run_qs FAKE_QS_CALL=configure_peer
  expect_rc 1; expect_out "and $H/keyless/codex/remote.key is absent"
  if qs_plist_has SUBROUTER_REMOTE_POOL; then bad "the peer was written into the LaunchAgent"; else ok; fi
  expect_no_log "launchctl"
  finish
fi

if begin quickstart-peer-step-writes-with-key; then
  # Calibration for the case above: with the key it writes the peer, and only
  # the peer, then restarts (the launchctl stub refuses, which stops it there).
  qs_setup; write_plist "{\"EnvironmentVariables\": {\"HOME\": \"$H\"}}"; seed_remote_key "$H/.subrouter"
  run_qs FAKE_QS_CALL=configure_peer
  expect_out "remote:  $H/.subrouter/codex/remote.key present"
  for key in SUBROUTER_REMOTE_POOL SUBROUTER_REMOTE_POOL_TOKEN SUBROUTER_REMOTE_POOL_FINGERPRINT; do
    qs_plist_has "$key" && ok || bad "$key was not written"
  done
  # Claude Code presents the proxy token, so the loopback exemption stays off.
  qs_plist_has SUBROUTER_ALLOW_LOOPBACK_PROXY && bad "the loopback exemption was turned on" || ok
  expect_mode "$H/Library/LaunchAgents/ai.manaflow.subrouter.plist" 0o600
  expect_log "launchctl bootstrap"
  finish
fi

if begin quickstart-peer-restart-waits-for-launchd; then
  # A supervised job drains for up to its ExitTimeOut after the bootout, and
  # a bootstrap while launchd still holds the label fails with an I/O error.
  # The peer step waits as deploy-laptop.sh does: up to ExitTimeOut + 60 s,
  # and only rc 113 from `launchctl print` means the job is gone.
  qs_setup; seed_remote_key "$H/.subrouter"
  write_plist "{\"EnvironmentVariables\": {\"HOME\": \"$H\"}, \"ExitTimeOut\": 600}"
  stub launchctl 'echo "launchctl $*" >> "$FAKE_LOG"
prints=$(grep -c "^launchctl print" "$FAKE_LOG")
case "$1" in
  managername) echo Aqua ;;
  print) [ "$prints" -gt "$FAKE_HELD" ] && exit "$FAKE_GONE_RC"; exit 0 ;;
  bootstrap)
    [ "$prints" -gt "$FAKE_HELD" ] || { echo "Bootstrap failed: 5: Input/output error" >&2; exit 5; }
    : > "$FAKE_DAEMON_FILE"; exit 0 ;;
esac
exit 0'
  run_qs FAKE_QS_CALL=configure_peer FAKE_HELD=640 FAKE_GONE_RC=113 FAKE_DAEMON_FILE="$C/daemon-up"
  expect_rc 0; expect_out "restarted and healthy"
  # Any other answer is not "gone": it stops, and names the bootstrap to run by hand.
  rm -f "$C/daemon-up"; : > "$C/log"
  run_qs FAKE_QS_CALL=configure_peer FAKE_HELD=3 FAKE_GONE_RC=5 FAKE_DAEMON_FILE="$C/daemon-up"
  expect_rc 1; expect_no_log "^launchctl bootstrap"
  expect_out "launchctl bootstrap gui/$(id -u) $H/Library/LaunchAgents/ai.manaflow.subrouter.plist"
  finish
fi

# launchctl as a session sees it: managername prints FAKE_MANAGER (Background,
# as from ssh) and exits FAKE_MANAGER_RC, print answers FAKE_PRINT_RC until a
# bootout and 113 after it, and launchd refuses the bootstrap.
stub_launchd_session() {
  stub launchctl 'echo "launchctl $*" >> "$FAKE_LOG"
case "$1" in
  managername) [ -n "${FAKE_MANAGER-Background}" ] && echo "${FAKE_MANAGER-Background}"; exit "${FAKE_MANAGER_RC:-0}" ;;
  print) grep -q "^launchctl bootout" "$FAKE_LOG" && exit 113; exit "${FAKE_PRINT_RC:-0}" ;;
  bootstrap) echo "Bootstrap failed: 125: Domain does not support specified action" >&2; exit 125 ;;
esac
exit 0'
}

# launchd with state: bootstrap loads the plist's env into FAKE_LAUNCHD (a JSON
# file), bootout unloads it, and print shows a loaded job as launchctl does,
# its inherited environment block first, then its environment block.
stub_launchd() {
  { printf '#!%s\n' "$PY3"; cat <<'PY'; } > "$C/bin/launchctl"
import json, os, plistlib, sys
args = sys.argv[1:]
state = os.environ["FAKE_LAUNCHD"]
with open(os.environ["FAKE_LOG"], "a") as log:
    log.write("launchctl " + " ".join(args) + "\n")
verb = args[0] if args else ""
if verb == "managername":
    print("Aqua")
elif verb == "print":
    if not os.path.exists(state):
        sys.exit(113)
    print(args[1] + " = {\n\tinherited environment = {\n\t\tSSH_AUTH_SOCK => /private/tmp/agent.sock\n\t}\n\tenvironment = {")
    for key, value in json.load(open(state)).items():
        print(f"\t\t{key} => {value}")
    print("\t}\n}")
elif verb == "bootout":
    if os.path.exists(state):
        os.remove(state)
elif verb == "bootstrap":
    with open(args[2], "rb") as handle:
        env = plistlib.load(handle).get("EnvironmentVariables") or {}
    with open(state, "w") as out:
        json.dump(dict(env, XPC_SERVICE_NAME="ai.manaflow.subrouter"), out)
else:
    sys.exit(1)
PY
  chmod 755 "$C/bin/launchctl"
}

if begin quickstart-peer-rerun-leaves-a-peered-daemon-alone; then
  # A re-run must not bounce the daemon every routed client uses when the peer
  # is already set. launchd reads the plist only at a bootstrap, so the step
  # writes the plist only when it lacks the peer, and restarts the daemon only
  # when the loaded job lacks it.
  qs_setup; seed_remote_key "$H/.subrouter"; stub_launchd
  write_plist "{\"EnvironmentVariables\": {\"HOME\": \"$H\"}}"
  P="$H/Library/LaunchAgents/ai.manaflow.subrouter.plist"
  printf '{"HOME": "%s"}\n' "$H" > "$C/launchd.json"
  run_qs FAKE_QS_CALL=configure_peer FAKE_LAUNCHD="$C/launchd.json" FAKE_DAEMON_UP=1
  expect_rc 0; expect_out "restarted and healthy"; expect_log "^launchctl bootstrap"
  cp "$P" "$C/plist-peered"; : > "$C/log"
  run_qs FAKE_QS_CALL=configure_peer FAKE_LAUNCHD="$C/launchd.json" FAKE_DAEMON_UP=1
  expect_rc 0; expect_out "already in the LaunchAgent and the running daemon"; expect_no_log "^launchctl boot"
  cmp -s "$P" "$C/plist-peered" && ok || bad "the plist was written again"
  # The plist holds the peer and the loaded job does not, as after a run killed
  # between the write and the restart: restart, and leave the plist as it is.
  printf '{"HOME": "%s"}\n' "$H" > "$C/launchd.json"; : > "$C/log"
  run_qs FAKE_QS_CALL=configure_peer FAKE_LAUNCHD="$C/launchd.json" FAKE_DAEMON_UP=1
  expect_rc 0; expect_out "restarted and healthy"; expect_log "^launchctl bootstrap"
  cmp -s "$P" "$C/plist-peered" && ok || bad "the plist was written again"
  # A job that is not loaded is started.
  rm -f "$C/launchd.json"; : > "$C/log"
  run_qs FAKE_QS_CALL=configure_peer FAKE_LAUNCHD="$C/launchd.json" FAKE_DAEMON_UP=1
  expect_rc 0; expect_log "^launchctl bootstrap"
  finish
fi

if begin quickstart-peer-restart-needs-the-gui-session; then
  # Outside the GUI login session a bootout of gui/<uid> works and launchd
  # refuses the bootstrap, which leaves the daemon down (PATCHES §47). setup
  # checks the session, but with a daemon already answering setup never runs
  # and the peer step still restarts it. So --peer checks it in phase 1,
  # before the build, and the peer step again before it writes the plist.
  qs_setup; seed_remote_key "$H/.subrouter"; write_plist "{\"EnvironmentVariables\": {\"HOME\": \"$H\"}}"
  P="$H/Library/LaunchAgents/ai.manaflow.subrouter.plist"; cp "$P" "$C/plist-before"
  stub_launchd_session
  run_qs FAKE_DAEMON_UP=1 FAKE_QS_STUB=build -- --peer "$PEER_URL" --peer-fingerprint ab12 --peer-token-file "$C/peer-token"
  expect_rc 1; expect_out "this shell is in launchd's 'Background' session"; expect_out "new Terminal.app window"
  expect_no_out "(stubbed build)"
  expect_no_log "bootout"
  run_qs FAKE_QS_CALL=configure_peer
  expect_rc 1; expect_out "'Background' session"; expect_no_log "bootout"
  cmp -s "$P" "$C/plist-before" && ok || bad "the plist was written"
  # A shell left in an ended login session cannot ask at all.
  run_qs FAKE_QS_CALL=configure_peer FAKE_MANAGER= FAKE_MANAGER_RC=153
  expect_rc 1; expect_out "this shell cannot reach launchd (\`launchctl managername\`: rc 153"; expect_no_log "bootout"
  # Only 0 and 113 from print are answers.
  run_qs FAKE_QS_CALL=configure_peer FAKE_MANAGER=Aqua FAKE_PRINT_RC=141
  expect_rc 1; expect_out "answered rc 141"; expect_no_log "bootout"
  cmp -s "$P" "$C/plist-before" && ok || bad "the plist was written"
  finish
fi

if begin quickstart-peer-failed-bootstrap-says-the-daemon-is-down; then
  # A bootstrap launchd refuses after the bootout leaves no job loaded. The run
  # says the daemon is down, names the command that starts it, and does not
  # claim the clients are fine: this settings.json is routed at the daemon.
  qs_setup; seed_remote_key "$H/.subrouter"; write_plist "{\"EnvironmentVariables\": {\"HOME\": \"$H\"}}"
  mkdir -p "$H/.claude"
  printf '{"env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:1", "ANTHROPIC_AUTH_TOKEN": "t"}}\n' > "$H/$SETTINGS_REL"
  stub_launchd_session
  run_qs FAKE_DAEMON_UP=1 FAKE_QS_STUB=build FAKE_MANAGER=Aqua -- --peer "$PEER_URL" --peer-fingerprint ab12 --peer-token-file "$C/peer-token"
  expect_rc 1; expect_out "launchctl bootstrap failed: Bootstrap failed: 125"
  expect_out "The daemon is DOWN"
  expect_out "launchctl bootstrap gui/$(id -u) $H/Library/LaunchAgents/ai.manaflow.subrouter.plist"
  expect_out "Claude Code is down with it; $OPS/bin/subrouter-unbrick takes it off"
  expect_no_out "Your clients were not left half-configured"
  finish
fi

if begin quickstart-peer-step-takes-the-mutation-lock; then
  # deploy-laptop.sh, the supervisor scripts and `sr update` edit the plist and
  # restart the job under <plist>.supervisor-mutation.lock (PATCHES §69.5).
  # The peer step takes it too, so none of them writes over its plist edit or
  # restarts the job under it.
  qs_setup; seed_remote_key "$H/.subrouter"; write_plist "{\"EnvironmentVariables\": {\"HOME\": \"$H\"}}"
  P="$H/Library/LaunchAgents/ai.manaflow.subrouter.plist"; cp "$P" "$C/plist-before"
  "$PY3" -c 'import fcntl, os, sys, time
fd = os.open(sys.argv[1], os.O_WRONLY | os.O_CREAT, 0o600)
fcntl.flock(fd, fcntl.LOCK_EX)
open(sys.argv[2], "w").close()
time.sleep(60)' "$P.supervisor-mutation.lock" "$C/held" &
  holder=$!
  for _ in $(seq 1 100); do [ -e "$C/held" ] && break; sleep 0.1; done
  run_qs FAKE_QS_CALL=configure_peer
  kill "$holder" 2>/dev/null; wait "$holder" 2>/dev/null
  expect_rc 1; expect_out "another deploy, rollback or update holds $P.supervisor-mutation.lock"
  expect_no_log "launchctl"
  cmp -s "$P" "$C/plist-before" && ok || bad "the plist was written while another run held the lock"
  # Calibration: once the lock is free, the step goes on to the restart.
  run_qs FAKE_QS_CALL=configure_peer
  expect_log "^launchctl bootstrap"
  finish
fi

if begin quickstart-peer-restart-waits-for-a-slow-worker; then
  # A supervised job's first worker has its --ready-timeout to come up (2m
  # from activate-laptop-supervisor.sh, PATCHES §69.1). After the bootstrap the
  # peer step waits as deploy-laptop.sh does: 120 s, or that timeout plus 60 s.
  qs_setup; seed_remote_key "$H/.subrouter"
  stub launchctl 'echo "launchctl $*" >> "$FAKE_LOG"; case "$1" in managername) echo Aqua ;; print) exit 113 ;; esac; exit 0'
  write_plist "{\"ProgramArguments\": [\"$H/bin/subrouter\", \"serve\"], \"EnvironmentVariables\": {\"HOME\": \"$H\"}}"
  run_qs FAKE_QS_CALL=configure_peer FAKE_DAEMON_AFTER=100
  expect_rc 0; expect_out "restarted and healthy"
  write_plist "{\"ProgramArguments\": [\"$H/bin/subrouter-supervisor\", \"supervise\", \"--worker-bin\", \"$H/bin/subrouter\", \"--ready-timeout\", \"5m\", \"--\", \"serve\"], \"EnvironmentVariables\": {\"HOME\": \"$H\"}}"
  run_qs FAKE_QS_CALL=configure_peer FAKE_DAEMON_AFTER=300
  expect_rc 0; expect_out "restarted and healthy"
  # Past that it stops, and says how long it waited.
  run_qs FAKE_QS_CALL=configure_peer FAKE_DAEMON_AFTER=400
  expect_rc 1; expect_out "has not answered for 360 s"
  finish
fi

if begin quickstart-peer-token-never-world-readable; then
  # setup leaves a plist with no token at 0644. The peer token must not sit in
  # it at that mode, even when the run is killed partway through the write.
  qs_setup; write_plist "{\"EnvironmentVariables\": {\"HOME\": \"$H\"}}"; seed_remote_key "$H/.subrouter"
  chmod 644 "$H/Library/LaunchAgents/ai.manaflow.subrouter.plist"
  run_qs FAKE_QS_CALL=configure_peer FAKE_DIE_AT=chmod
  # The run must get as far as the kill: a script that does not load also exits 1.
  expect_rc 1; expect_out "killed at chmod"
  if qs_plist_has SUBROUTER_REMOTE_POOL_TOKEN; then
    expect_mode "$H/Library/LaunchAgents/ai.manaflow.subrouter.plist" 0o600
  else
    ok
  fi
  left=$(ls -A "$H/Library/LaunchAgents" | grep -vx 'ai.manaflow.subrouter.plist.supervisor-mutation.lock')
  [ "$left" = ai.manaflow.subrouter.plist ] && ok || bad "files left beside the plist: $(echo "$left" | tr '\n' ' ')"
  finish
fi

if begin quickstart-peer-key-not-a-file; then
  qs_setup; mkdir -p "$H/.subrouter/codex/remote.key"; run_qs
  expect_rc 1; expect_out "remote.key is not a regular file"
  expect_out "mv $H/.subrouter/codex/remote.key $H/.subrouter/codex/remote.key.aside && SUBROUTER_STATE_DIR=$H/.subrouter"
  expect_no_out "--force"
  finish
fi

if begin quickstart-peer-key-unreadable; then
  qs_setup; seed_remote_key "$H/.subrouter"
  if make_unreadable "$H/.subrouter/codex/remote.key"; then
    run_qs
    expect_rc 1; expect_out "remote.key is unreadable"
    expect_out "ls -l $H/.subrouter/codex/remote.key"; expect_no_out "--force"
  fi
  finish
fi

if begin quickstart-preflight-refuses-shell-exports; then
  # An exported base URL or token routes everything that shell starts and
  # outlives a rollback, so phase 1 names the line and stops.
  qs_setup
  printf 'alias ll="ls -l"\nexport ANTHROPIC_AUTH_TOKEN=whatever\n' > "$H/.zprofile"
  run_qs -- --check
  expect_rc 1; expect_out "~/.zprofile:2: export ANTHROPIC_AUTH_TOKEN=whatever"
  expect_out "delete those export lines first"; expect_no_out "== 2/6"
  finish
fi

if begin quickstart-preflight-refuses-a-broken-settings-json; then
  # Phase 6 rewrites settings.json, so one that is not a JSON object with an
  # object env stops the run in phase 1, before anything is built or installed.
  qs_setup; mkdir -p "$H/.claude"
  for body in '{"env": {"A": 1},}' '[]' '{"env": "x"}'; do
    printf '%s\n' "$body" > "$H/$SETTINGS_REL"
    run_qs -- --check
    expect_rc 1; expect_out "$H/$SETTINGS_REL is not"; expect_no_out "== 2/6"; expect_no_out "Traceback"
  done
  # Phase 6 reads it again and stops the same way, having written nothing.
  qs_cloud; cp "$H/$SETTINGS_REL" "$C/before.json"
  run_qs FAKE_QS_CALL=cutover FAKE_TURN=ok
  expect_rc 1; expect_out "$H/$SETTINGS_REL is not"; expect_no_out "Traceback"
  cmp -s "$H/$SETTINGS_REL" "$C/before.json" && ok || bad "settings.json changed"
  any_exists "$H/$SETTINGS_REL.subrouter-bak-"* && bad "a backup was taken" || ok
  finish
fi

if begin quickstart-no-peer-needs-no-key; then
  qs_setup; run_qs -- --check
  expect_rc 0; expect_no_out "remote.key"; expect_out "--check: nothing was modified"
  finish
fi

if begin quickstart-go-version-read-in-the-module; then
  # Run from a directory without a go.mod, a local Go 1.22 is what `go version`
  # reports, though the build, run in the checkout, fetches what go.mod names.
  qs_setup; run_qs FAKE_CWD="$C" FAKE_LOCAL_GO=go1.22.5 -- --check
  expect_rc 0; expect_out "go:      go version go$MODGO darwin/arm64"
  finish
fi

if begin quickstart-go-local-toolchain-below-go-mod; then
  # GOTOOLCHAIN=local never fetches, so a Go below go.mod cannot build it.
  qs_setup; run_qs FAKE_CWD="$C" FAKE_LOCAL_GO=go1.25.3 GOTOOLCHAIN=local -- --check
  expect_rc 1; expect_out "needs Go $MODGO"; expect_out "go1.25.3"; expect_out "GOTOOLCHAIN=local"
  expect_no_out "== 2/6"
  finish
fi

if begin quickstart-go-failed-fetch-stops; then
  # A fetch that fails prints only its error, and that names the wanted version:
  # it is not a toolchain that is new enough.
  qs_setup; run_qs FAKE_CWD="$C" FAKE_LOCAL_GO=go1.22.5 FAKE_GO_FETCH_FAILS=1 -- --check
  expect_rc 1; expect_out "needs Go $MODGO"; expect_out "toolchain not available"; expect_no_out "== 2/6"
  finish
fi

if begin quickstart-go-stalled-fetch-is-bounded; then
  # --check runs phase 1 too, and `go version` there can be fetching the
  # toolchain go.mod names: a stalled fetch stops the run, it does not hang it.
  qs_setup
  started=$SECONDS
  run_qs FAKE_CWD="$C" FAKE_LOCAL_GO=go1.22.5 FAKE_GO_HANG=1 FAKE_TIMEOUT_CAP=1 -- --check
  took=$((SECONDS - started))
  expect_rc 1; expect_out "timed out"; expect_no_out "== 2/6"
  [ "$took" -lt 20 ] && ok || bad "phase 1 took ${took}s: \`go version\` ran with no time limit"
  finish
fi

if begin quickstart-install-confirms-setup; then
  # setup applies nothing without a person at a terminal unless --yes approves
  # it, and exits 0 either way (cmd/subrouter/sr_setup.go). This fake answers
  # the same way, and its daemon answers only after an applied setup.
  qs_setup
  fake_binary "$C/build/subrouter" 'echo "subrouter $*" >> "$FAKE_LOG"
case " $* " in *" --plan "*) echo "  + Run Subrouter in the background"; exit 0 ;; esac
case " $* " in *" --yes "*) : > "$FAKE_DAEMON_FILE"; echo "daemon healthy"; exit 0 ;; esac
echo "noninteractive terminal: re-run with --yes to apply"; exit 0'
  run_qs FAKE_QS_CALL=install FAKE_QS_BIN="$C/build/subrouter" FAKE_DAEMON_FILE="$C/daemon-up"
  expect_rc 0; expect_out "daemon:  healthy on http://127.0.0.1:1, no client config written"
  expect_log "^subrouter setup --storage local --no-config --yes$"
  finish
fi

if begin quickstart-upgrade-names-the-stale-daemon; then
  # A daemon already answers, so phase 3 installs nothing: it must say the build
  # phase 2 made is not the one serving, and name the command that installs it.
  qs_setup
  fake_binary "$H/bin/subrouter" "# old build"; fake_binary "$C/build/subrouter" "# new build"
  write_plist "{\"ProgramArguments\": [\"$H/bin/subrouter\", \"serve\"]}"
  run_qs FAKE_QS_CALL=install FAKE_DAEMON_UP=1 FAKE_QS_BIN="$C/build/subrouter"
  expect_rc 0
  expect_out "the daemon runs $H/bin/subrouter, which is not the build phase 2 made"
  expect_line "       $OPS/deploy/macos/deploy-laptop.sh $C/build/subrouter"
  expect_no_log "^subrouter setup"
  finish
fi

if begin quickstart-upgrade-reads-the-supervised-worker; then
  # Under the supervisor the job's program is the supervisor; the build that
  # serves, and that deploy-laptop.sh replaces, is --worker-bin.
  qs_setup
  fake_binary "$H/bin/subrouter-supervisor" "# old build"; fake_binary "$C/build/subrouter" "# new build"
  cp "$C/build/subrouter" "$H/bin/subrouter"
  write_plist "{\"ProgramArguments\": [\"$H/bin/subrouter-supervisor\", \"supervise\", \"--worker-bin\", \"$H/bin/subrouter\", \"--\", \"serve\"]}"
  run_qs FAKE_QS_CALL=install FAKE_DAEMON_UP=1 FAKE_QS_BIN="$C/build/subrouter"
  expect_rc 0; expect_out "already answering"; expect_no_out "deploy-laptop.sh"
  fake_binary "$H/bin/subrouter" "# older worker"
  run_qs FAKE_QS_CALL=install FAKE_DAEMON_UP=1 FAKE_QS_BIN="$C/build/subrouter"
  expect_rc 0; expect_out "the daemon runs $H/bin/subrouter, which is not the build phase 2 made"
  finish
fi

if begin quickstart-upgrade-same-build-is-quiet; then
  # Calibration: the build that is running is not reported as stale.
  qs_setup
  fake_binary "$C/build/subrouter" "# one build"; mkdir -p "$H/bin"; cp "$C/build/subrouter" "$H/bin/subrouter"
  write_plist "{\"ProgramArguments\": [\"$H/bin/subrouter\", \"serve\"]}"
  run_qs FAKE_QS_CALL=install FAKE_DAEMON_UP=1 FAKE_QS_BIN="$C/build/subrouter"
  expect_rc 0; expect_out "already answering"; expect_no_out "deploy-laptop.sh"
  finish
fi

if begin quickstart-upgrade-same-commit-is-quiet; then
  # deploy-laptop.sh re-signs what it installs, so the bytes can differ for one
  # clean commit; that is the same build. A modified tree is not.
  qs_setup
  fake_binary "$H/bin/subrouter" "# vcs.revision=abc123
# vcs.modified=false
# signed by deploy-laptop.sh"
  fake_binary "$C/build/subrouter" "# vcs.revision=abc123
# vcs.modified=false"
  write_plist "{\"ProgramArguments\": [\"$H/bin/subrouter\", \"serve\"]}"
  run_qs FAKE_QS_CALL=install FAKE_DAEMON_UP=1 FAKE_QS_BIN="$C/build/subrouter"
  expect_rc 0; expect_no_out "deploy-laptop.sh"
  fake_binary "$C/build/subrouter" "# vcs.revision=abc123
# vcs.modified=true"
  run_qs FAKE_QS_CALL=install FAKE_DAEMON_UP=1 FAKE_QS_BIN="$C/build/subrouter"
  expect_rc 0; expect_out "deploy/macos/deploy-laptop.sh $C/build/subrouter"
  finish
fi

# settings.json as this run found it, and no backup: phase 6 wrote nothing.
qs_settings_untouched() {
  cmp -s "$H/$SETTINGS_REL" "$C/before.json" && ok || bad "settings.json changed"
  any_exists "$H/$SETTINGS_REL.subrouter-bak-"* && bad "a backup was taken" || ok
}
qs_settings_of_my_own() {
  mkdir -p "$H/.claude"
  printf '{"env": {"AWS_PROFILE": "me"}}\n' > "$H/$SETTINGS_REL"; cp "$H/$SETTINGS_REL" "$C/before.json"
}

if begin quickstart-accounts-empty-pool-needs-a-human; then
  # Never point a client at an empty pool (PATCHES §3). With no account and no
  # peer, phase 4 stops at the step only a person can do, before any probe.
  qs_setup; qs_cloud; qs_settings_of_my_own
  run_qs_turns FAKE_ACCOUNTS='[]' --
  expect_rc 3; expect_out "the pool is empty, and this is the step that needs a human"
  expect_out "claude addr <name>"; expect_no_out "== 5/6"; expect_no_log "^claude "
  qs_settings_untouched
  # A daemon that does not answer the route has no accounts to offer either.
  run_qs_turns FAKE_ACCOUNTS= --
  expect_rc 3; expect_out "the pool is empty"; expect_no_log "^claude "
  # With --peer the pool is at the other end, so the run goes on.
  run_qs_turns FAKE_ACCOUNTS='[]' FAKE_SERVE=peer -- --peer "$PEER_URL" --peer-fingerprint ab12 --peer-token-file "$C/peer-token"
  expect_rc 0; expect_out "pool:    0 local accounts, which is fine: the peer is the pool"; expect_out "served:  a full turn -- peer"
  run_qs_turns FAKE_ACCOUNTS='[{"id": "a"}, {"id": "b"}]' --
  expect_rc 0; expect_out "pool:    2 local account(s)"
  finish
fi

if begin quickstart-prove-needs-the-local-proxy-token; then
  # The daemon enforces its local proxy token (PATCHES §4), which setup writes
  # into cloud.json. With none there, phase 5 stops before any probe.
  qs_setup
  run_qs_turns --
  expect_rc 1; expect_out "no $H/.config/subrouter/cloud.json; run the install phase first"; expect_no_log "^claude "
  mkdir -p "$H/.config/subrouter"
  for body in '{}' '{"localProxyToken": ""}' '{"localProxyToken": "  "}'; do
    printf '%s\n' "$body" > "$H/.config/subrouter/cloud.json"
    run_qs_turns --
    expect_rc 1; expect_out "cloud.json carries no localProxyToken"; expect_no_log "^claude "
  done
  qs_cloud
  run_qs_turns --
  expect_rc 0; expect_out "served:  a full turn -- local:acct-1"
  finish
fi

if begin quickstart-prove-needs-a-fresh-log-line; then
  # A reply alone proves nothing: Claude Code answers on its own login too. The
  # proof is a line the daemon logged for this turn, and a machine that served
  # before has old lines of both shapes. Without a fresh one the run stops
  # before phase 6 writes anything.
  qs_setup; qs_cloud; qs_settings_of_my_own
  run_qs_turns FAKE_SERVE=elsewhere --
  expect_rc 1; expect_out "nothing fresh in the log -- it did NOT go through the daemon"; expect_no_out "== 6/6"
  mkdir -p "$H/Library/Logs"
  for old in "proxy request agent=claude session=s0 account=acct-old" "serving from remote pool remote=box agent=claude"; do
    printf '2020/01/01 00:00:00 INFO %s\n' "$old" > "$H/Library/Logs/subrouter.err.log"
    run_qs_turns FAKE_SERVE=elsewhere --
    expect_rc 1; expect_out "nothing fresh in the log"; expect_no_out "== 6/6"
  done
  qs_settings_untouched
  run_qs_turns --
  expect_rc 0; expect_out "served:  a full turn -- local:acct-1"
  expect_out "proved:  a fresh process read settings.json and was served -- local:acct-1"
  finish
fi

if begin quickstart-prove-names-a-quiet-log-level; then
  # Above Info the daemon logs no turn (PATCHES §39), so no proof can be had:
  # the stop names the variable, not a turn that went elsewhere. The fake
  # answers and logs nothing, as such a daemon does.
  qs_setup; qs_cloud; qs_settings_of_my_own
  for level in warn ERROR info+1; do
    write_plist "{\"EnvironmentVariables\": {\"HOME\": \"$H\", \"SUBROUTER_LOG_LEVEL\": \"$level\"}}"
    run_qs_turns FAKE_SERVE=elsewhere --
    expect_rc 1; expect_out "the daemon runs with SUBROUTER_LOG_LEVEL=$level"; expect_no_out "== 6/6"
  done
  qs_settings_untouched
  # Calibration: at Info or below, or malformed, which the daemon runs at
  # Info, a turn with no line went elsewhere.
  for level in info debug warn-4 loud; do
    write_plist "{\"EnvironmentVariables\": {\"HOME\": \"$H\", \"SUBROUTER_LOG_LEVEL\": \"$level\"}}"
    run_qs_turns FAKE_SERVE=elsewhere --
    expect_rc 1; expect_out "nothing fresh in the log -- it did NOT go through the daemon"; expect_no_out "SUBROUTER_LOG_LEVEL"
  done
  finish
fi

if begin quickstart-prove-reads-the-rotated-log; then
  # The daemon rotates its log past a cap (PATCHES §39), so the line for a turn
  # can be in LOG.1 alone, at the end of a long file.
  qs_setup; qs_cloud; mkdir -p "$H/Library/Logs"
  "$PY3" -c 'import sys; open(sys.argv[1], "w").write("2020/01/01 00:00:00 INFO proxy request agent=claude session=s0 account=acct-old\n" * 25000)' \
    "$H/Library/Logs/subrouter.err.log.1"
  run_qs_turns FAKE_SERVE=rotated --
  expect_rc 0; expect_out "served:  a full turn -- local:acct-1"; expect_out "was served -- local:acct-1"
  finish
fi

if begin quickstart-prove-accepts-a-peer-turn; then
  # A turn the daemon hands to its peer logs "serving from remote pool" and no
  # account. That proves the stack too, which is the --peer setup.
  qs_setup; qs_cloud
  run_qs_turns FAKE_SERVE=peer --
  expect_rc 0; expect_out "served:  a full turn -- peer"; expect_out "was served -- peer"
  finish
fi

if begin quickstart-prove-needs-the-marker; then
  # The daemon took the turn, and the reply never came (an upstream error):
  # that is no proof. The run stops, names the error, and writes nothing.
  qs_setup; qs_cloud; qs_settings_of_my_own
  run_qs_turns FAKE_SERVE=error --
  expect_rc 1; expect_out "the daemon did not serve a turn, so the cutover is NOT safe: API Error: 529 Overloaded"
  expect_no_out "== 6/6"
  qs_settings_untouched
  finish
fi

if begin quickstart-prove-goes-through-the-daemon-with-its-token; then
  # A settings.json env beats the environment (PATCHES §29.9.1), so phase 5
  # cannot hand its probe the daemon through the environment: here settings.json
  # routes straight at the box, from a shell that exports a third base URL. The
  # probe still reaches the daemon with its token, the token is never in argv,
  # and nothing is left in TMPDIR.
  qs_setup; qs_cloud; mkdir -p "$H/.claude"
  printf '{"env": {"ANTHROPIC_BASE_URL": "http://100.64.0.1:31415", "ANTHROPIC_AUTH_TOKEN": "box-token"}}\n' > "$H/$SETTINGS_REL"
  run_qs_turns ANTHROPIC_BASE_URL=http://203.0.113.7:31415 ANTHROPIC_AUTH_TOKEN=shell-token --
  expect_rc 0; expect_out "served:  a full turn -- local:acct-1"
  case "$(claude_call 1)" in
    *"| settings-file=0o600 env=none base=http://127.0.0.1:1 token=daemon") ok ;;
    *) bad "phase 5's probe was given: $(claude_call 1)" ;;
  esac
  grep '^claude ' "$C/log" | grep -q local-proxy-token-value && bad "the token was in claude's argv" || ok
  [ -z "$(ls -A "$C/tmp")" ] && ok || bad "left in TMPDIR: $(ls -A "$C/tmp" | tr '\n' ' ')"
  # Calibration: a daemon that wants another token serves the probe no turn.
  run_qs_turns FAKE_DAEMON_TOKEN=another-token --
  expect_rc 1; expect_out "API Error: 401"
  finish
fi

if begin quickstart-reproof-reads-only-settings-json; then
  # Phase 6 proves the file a fresh `claude` reads. A session already on the
  # daemon exports both routing keys, and the re-proof must not inherit them.
  qs_setup; qs_cloud
  run_qs_turns ANTHROPIC_BASE_URL=http://127.0.0.1:1 ANTHROPIC_AUTH_TOKEN=local-proxy-token-value --
  expect_rc 0; expect_out "proved:"
  case "$(claude_call 2)" in
    *"| settings-file=none env=none base=http://127.0.0.1:1 token=daemon") ok ;;
    *) bad "phase 6's probe was given: $(claude_call 2)" ;;
  esac
  finish
fi

if begin quickstart-cutover-writes-the-daemon-and-its-token; then
  # Phase 6 points Claude Code at the daemon with the token the daemon enforces
  # (PATCHES §4), keeps every other setting, and asks for the 1-hour cache.
  qs_setup; qs_cloud; mkdir -p "$H/.claude"
  printf '{"env": {"AWS_PROFILE": "me"}, "model": "opus"}\n' > "$H/$SETTINGS_REL"
  run_qs FAKE_QS_CALL=cutover FAKE_TURN=ok
  expect_rc 0; expect_base_url "http://127.0.0.1:1"
  "$PY3" -c 'import json, sys
d = json.load(open(sys.argv[1]))
e = d["env"]
sys.exit(not (e.get("ANTHROPIC_AUTH_TOKEN") == "local-proxy-token-value" and e.get("AWS_PROFILE") == "me"
              and e.get("CLAUDE_CODE_PROMPT_CACHE_TTL") == "1h" and d.get("model") == "opus"))' "$H/$SETTINGS_REL" \
    && ok || bad "settings.json lacks the token, the 1h cache, or a setting it had"
  any_exists "$H/$SETTINGS_REL.subrouter-bak-"* && ok || bad "no backup of the old settings.json"
  finish
fi

if begin quickstart-local-base-url-in-the-go-form; then
  # The Go CLI's SUBROUTER_LOCAL_BASE_URL carries /v1 (localFallbackBaseURL),
  # and setup writes Claude's base URL without it (codexProxyRootURL), since
  # Claude Code adds /v1 itself. The quickstart takes the same root.
  qs_setup; qs_cloud
  for url in http://127.0.0.1:1/v1 http://127.0.0.1:1/v1/ http://127.0.0.1:1/backend-api; do
    rm -f "$H/$SETTINGS_REL"
    run_qs FAKE_QS_CALL=cutover FAKE_TURN=ok SUBROUTER_LOCAL_BASE_URL="$url"
    expect_rc 0; expect_base_url "http://127.0.0.1:1"
  done
  finish
fi

if begin quickstart-cutover-keeps-the-token-private; then
  # settings.json now holds the local proxy token, which keeps other local users
  # from spending the pool through 127.0.0.1. It is written 0600 however it was
  # found, as the Go writer does.
  qs_setup; qs_cloud
  run_qs FAKE_QS_CALL=cutover FAKE_TURN=ok
  expect_rc 0; expect_mode "$H/$SETTINGS_REL" 0o600
  for mode in 600 644; do
    printf '{"env": {"AWS_PROFILE": "me"}}\n' > "$H/$SETTINGS_REL"; chmod "$mode" "$H/$SETTINGS_REL"
    run_qs FAKE_QS_CALL=cutover FAKE_TURN=ok
    expect_rc 0; expect_mode "$H/$SETTINGS_REL" 0o600
  done
  finish
fi

if begin quickstart-cutover-respects-a-chosen-cache-ttl; then
  # The 1-hour cache is asked for only when no TTL is chosen yet. Env beats the
  # promptCacheTtl setting, and the user's own export is a choice too, except
  # inside Claude Code, which exports one itself (PATCHES §21).
  qs_setup; qs_cloud; mkdir -p "$H/.claude"
  printf '{"promptCacheTtl": "5m"}\n' > "$H/$SETTINGS_REL"
  run_qs FAKE_QS_CALL=cutover FAKE_TURN=ok
  expect_rc 0; expect_ttl unset "promptCacheTtl in the file"
  printf '{"env": {"CLAUDE_CODE_PROMPT_CACHE_TTL": ""}}\n' > "$H/$SETTINGS_REL"
  run_qs FAKE_QS_CALL=cutover FAKE_TURN=ok
  expect_rc 0; expect_ttl "" "an empty TTL key in the file"
  printf '{}\n' > "$H/$SETTINGS_REL"
  run_qs FAKE_QS_CALL=cutover FAKE_TURN=ok CLAUDE_CODE_PROMPT_CACHE_TTL=5m
  expect_rc 0; expect_ttl unset "an exported 5m"
  printf '{}\n' > "$H/$SETTINGS_REL"
  run_qs FAKE_QS_CALL=cutover FAKE_TURN=ok FORCE_PROMPT_CACHING_5M=
  expect_rc 0; expect_ttl unset "an exported FORCE_PROMPT_CACHING_5M"
  printf '{}\n' > "$H/$SETTINGS_REL"
  run_qs FAKE_QS_CALL=cutover FAKE_TURN=ok CLAUDE_CODE_PROMPT_CACHE_TTL=1h
  expect_rc 0; expect_ttl 1h "an exported 1h"
  printf '{}\n' > "$H/$SETTINGS_REL"
  run_qs FAKE_QS_CALL=cutover FAKE_TURN=ok CLAUDECODE=1 CLAUDE_CODE_PROMPT_CACHE_TTL=5m
  expect_rc 0; expect_ttl 1h "an export inside Claude Code"
  finish
fi

if begin quickstart-cutover-follows-claude-config-dir; then
  # The file a fresh `claude` reads, which the re-proof's process reads too.
  qs_setup; qs_cloud
  run_qs FAKE_QS_CALL=cutover FAKE_TURN=ok CLAUDE_CONFIG_DIR="$C/claude"
  expect_rc 0; expect_base_url "http://127.0.0.1:1" "$C/claude/settings.json"
  expect_base_url absent
  finish
fi

if begin quickstart-cutover-revert-removes-a-file-it-created; then
  # A Mac with no settings.json: a failed re-proof must leave none, and say so.
  qs_setup; qs_cloud
  run_qs FAKE_QS_CALL=cutover FAKE_TURN=fail
  expect_rc 1; expect_base_url absent
  expect_out "there was no settings.json before this run"
  finish
fi

if begin quickstart-cutover-revert-ignores-older-backups; then
  # Not the newest backup on disk: this run's starting state, which was no file.
  qs_setup; qs_cloud; mkdir -p "$H/.claude"
  printf '{"env": {"ANTHROPIC_BASE_URL": "http://100.64.0.1:31415"}}\n' > "$H/$SETTINGS_REL.subrouter-bak-20200101-000000"
  run_qs FAKE_QS_CALL=cutover FAKE_TURN=fail
  expect_rc 1; expect_base_url absent
  finish
fi

if begin quickstart-cutover-revert-restores-the-file-byte-for-byte; then
  # Its mode too: 0600 is what the cutover writes, so a 0644 file shows it is put back.
  qs_setup; qs_cloud; mkdir -p "$H/.claude"
  for mode in 600 644; do
    printf '{\n  "env": {"AWS_PROFILE": "me"},\n  "model": "opus"\n}\n' > "$H/$SETTINGS_REL"
    chmod "$mode" "$H/$SETTINGS_REL"; cp -p "$H/$SETTINGS_REL" "$C/before.json"
    run_qs FAKE_QS_CALL=cutover FAKE_TURN=fail
    expect_rc 1
    cmp -s "$H/$SETTINGS_REL" "$C/before.json" && ok || bad "settings.json differs from what the run found"
    expect_mode "$H/$SETTINGS_REL" "0o$mode"
    expect_out "put back exactly as this run found it"
  done
  finish
fi

if begin quickstart-cutover-linked-settings-json; then
  # A dotfile manager can make settings.json a link into a repo. The cutover
  # writes a file of its own in the link's place, so the token stays out of
  # the repo, and a failed re-proof puts the link back.
  qs_setup; qs_cloud; mkdir -p "$H/.claude" "$C/dotfiles"
  printf '{"env": {"AWS_PROFILE": "me"}}\n' > "$C/dotfiles/settings.json"; cp "$C/dotfiles/settings.json" "$C/before.json"
  ln -s "$C/dotfiles/settings.json" "$H/$SETTINGS_REL"
  run_qs FAKE_QS_CALL=cutover FAKE_TURN=fail
  expect_rc 1; expect_out "put back exactly as this run found it"
  [ "$(readlink "$H/$SETTINGS_REL")" = "$C/dotfiles/settings.json" ] && ok || bad "settings.json is not the link this run found"
  cmp -s "$C/dotfiles/settings.json" "$C/before.json" && ok || bad "the link's target changed"
  run_qs FAKE_QS_CALL=cutover FAKE_TURN=ok
  expect_rc 0; expect_base_url "http://127.0.0.1:1"; expect_out "a file of its own"
  cmp -s "$C/dotfiles/settings.json" "$C/before.json" && ok || bad "the token was written into the link's target"
  # A link whose target is not there yet is put back too.
  rm "$H/$SETTINGS_REL"; ln -s "$C/dotfiles/missing.json" "$H/$SETTINGS_REL"
  run_qs FAKE_QS_CALL=cutover FAKE_TURN=fail
  expect_rc 1; expect_out "put back exactly as this run found it"; expect_no_out "Traceback"
  [ "$(readlink "$H/$SETTINGS_REL")" = "$C/dotfiles/missing.json" ] && ok || bad "the dangling link was not put back"
  finish
fi

if begin quickstart-cutover-revert-on-a-timed-out-reproof; then
  # A re-proof that times out is a failed one: settings.json goes back too. The
  # whole run, so main()'s own handling of the timeout is what is tested.
  qs_setup; qs_cloud
  run_qs FAKE_QS_STUB="preflight build install accounts prove" FAKE_TURN=timeout --
  expect_rc 1; expect_base_url absent
  expect_out "timed out"; expect_out "there was no settings.json before this run"
  mkdir -p "$H/.claude"; printf '{"env": {"AWS_PROFILE": "me"}}\n' > "$H/$SETTINGS_REL"
  cp -p "$H/$SETTINGS_REL" "$C/before.json"
  run_qs FAKE_QS_STUB="preflight build install accounts prove" FAKE_TURN=timeout --
  expect_rc 1
  cmp -s "$H/$SETTINGS_REL" "$C/before.json" && ok || bad "settings.json differs from what the run found"
  expect_out "put back exactly as this run found it"
  finish
fi

if begin quickstart-rollback-after-fresh-mac-cutover; then
  # A cutover on a Mac with no settings.json, then --rollback: Claude Code goes
  # back to its own login, not "Nothing restored".
  qs_setup; qs_cloud
  run_qs FAKE_QS_CALL=cutover FAKE_TURN=ok
  expect_rc 0; expect_base_url "http://127.0.0.1:1"
  run_qs -- --rollback
  expect_rc 0; expect_base_url unset; expect_no_out "Nothing restored"
  finish
fi

if begin quickstart-rollback-leaves-codex-alone; then
  # The quickstart never writes config.toml, and its newest backup can be the
  # routed copy unbrick took: --rollback puts back settings.json only.
  qs_setup; qs_cloud; mkdir -p "$H/.claude" "$H/.codex"
  printf '{"env": {"AWS_PROFILE": "me"}}\n' > "$H/$SETTINGS_REL"
  printf 'model = "gpt-5"\n' > "$H/.codex/config.toml"; cp "$H/.codex/config.toml" "$C/codex-before"
  printf 'openai_base_url = "http://127.0.0.1:31415/v1"\n' > "$H/.codex/config.toml.subrouter-bak-20200101-000000"
  run_qs FAKE_QS_CALL=cutover FAKE_TURN=ok
  expect_rc 0
  run_qs -- --rollback
  expect_rc 0; expect_base_url unset
  cmp -s "$H/.codex/config.toml" "$C/codex-before" && ok || bad "--rollback changed config.toml"
  finish
fi

if begin quickstart-rollback-skips-backups-on-the-daemon; then
  # unbrick backs up the routed settings.json before it takes the keys out, so
  # the newest backup can point at a daemon that is down. --rollback leaves a
  # settings.json that is off the daemon alone, and otherwise restores the
  # newest backup that is off it.
  qs_setup; qs_cloud; mkdir -p "$H/.claude"
  printf '{"env": {"AWS_PROFILE": "me"}}\n' > "$H/$SETTINGS_REL"; cp "$H/$SETTINGS_REL" "$C/pre-install.json"
  run_qs FAKE_QS_CALL=cutover FAKE_TURN=ok
  expect_rc 0; expect_base_url "http://127.0.0.1:1"
  # The cutover was a day earlier, so unbrick's backup is the newer one.
  mv "$H/$SETTINGS_REL.subrouter-bak-"* "$H/$SETTINGS_REL.subrouter-bak-20200101-000000"
  run_unbrick
  expect_rc 0; expect_base_url unset
  printf '{"env": {"AWS_PROFILE": "me"}, "model": "sonnet"}\n' > "$H/$SETTINGS_REL"; cp "$H/$SETTINGS_REL" "$C/edited.json"
  run_qs -- --rollback
  expect_rc 0; expect_base_url unset; expect_out "does not point at"
  cmp -s "$H/$SETTINGS_REL" "$C/edited.json" && ok || bad "--rollback overwrote a settings.json that was off the daemon"
  # The daemon came back and unbrick --restore put the routed file back.
  run_unbrick -- --restore
  expect_rc 0; expect_base_url "http://127.0.0.1:1"
  run_qs -- --rollback
  expect_rc 0; expect_base_url unset
  cmp -s "$H/$SETTINGS_REL" "$C/pre-install.json" && ok || bad "--rollback did not restore the pre-install backup"
  finish
fi

if begin quickstart-rollback-puts-a-link-back; then
  # A cutover over a linked settings.json backs up the link itself, so
  # --rollback and unbrick --restore put the dotfile manager's link back, not
  # a copy of what it pointed at.
  qs_setup; qs_cloud; mkdir -p "$H/.claude" "$C/dotfiles"
  printf '{"env": {"AWS_PROFILE": "me"}}\n' > "$C/dotfiles/settings.json"; cp "$C/dotfiles/settings.json" "$C/before.json"
  ln -s "$C/dotfiles/settings.json" "$H/$SETTINGS_REL"
  run_qs FAKE_QS_CALL=cutover FAKE_TURN=ok
  expect_rc 0; expect_base_url "http://127.0.0.1:1"; expect_out "--rollback puts the link back"
  run_qs -- --rollback
  expect_rc 0; expect_out "the link to $C/dotfiles/settings.json is back"
  [ "$(readlink "$H/$SETTINGS_REL")" = "$C/dotfiles/settings.json" ] && ok || bad "--rollback did not put the link back"
  cmp -s "$C/dotfiles/settings.json" "$C/before.json" && ok || bad "the link's target changed"
  run_qs FAKE_QS_CALL=cutover FAKE_TURN=ok
  expect_rc 0
  run_unbrick -- --restore
  expect_rc 0
  [ "$(readlink "$H/$SETTINGS_REL")" = "$C/dotfiles/settings.json" ] && ok || bad "unbrick --restore did not put the link back"
  # A link whose target is gone goes back as that link, with no traceback.
  rm "$H/$SETTINGS_REL"; ln -s "$C/dotfiles/missing.json" "$H/$SETTINGS_REL"
  run_qs FAKE_QS_CALL=cutover FAKE_TURN=ok
  expect_rc 0
  run_qs -- --rollback
  expect_rc 0; expect_no_out "Traceback"
  [ "$(readlink "$H/$SETTINGS_REL")" = "$C/dotfiles/missing.json" ] && ok || bad "--rollback did not put the dangling link back"
  finish
fi

if begin quickstart-rerun-over-a-routed-link; then
  # A link whose target already points at the daemon gets no backup, since
  # --rollback skips one that does. The cutover still makes settings.json a
  # file, so the run names the command that puts the link back, and that
  # command does.
  qs_setup; qs_cloud; mkdir -p "$H/.claude" "$C/dotfiles"
  printf '{"env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:1", "ANTHROPIC_AUTH_TOKEN": "t"}}\n' > "$C/dotfiles/settings.json"
  ln -s "$C/dotfiles/settings.json" "$H/$SETTINGS_REL"
  run_qs FAKE_QS_CALL=cutover FAKE_TURN=ok
  expect_rc 0; expect_out "already pointed here"; expect_no_out "--rollback puts the link back"
  back=$(sed -n 's/.*to put the link back: //p' "$C/out")
  [ "$back" = "ln -sf $C/dotfiles/settings.json $H/$SETTINGS_REL" ] && ok || bad "the command to put the link back is '$back'"
  (cd "$C" && PATH=/usr/bin:/bin eval "$back") && [ "$(readlink "$H/$SETTINGS_REL")" = "$C/dotfiles/settings.json" ] \
    && ok || bad "the printed command did not put the link back"
  finish
fi

if begin quickstart-rollback-without-a-backup-off-the-daemon; then
  # No backup would take it off the daemon: change nothing, and name the tool that does.
  qs_setup; mkdir -p "$H/.claude"
  printf '{"env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:1", "ANTHROPIC_AUTH_TOKEN": "t"}}\n' > "$H/$SETTINGS_REL"
  cp "$H/$SETTINGS_REL" "$C/before.json"
  run_qs -- --rollback
  expect_rc 1; expect_out "subrouter-unbrick"
  cp "$C/before.json" "$H/$SETTINGS_REL.subrouter-bak-20200101-000000"
  run_qs -- --rollback
  expect_rc 1; expect_out "subrouter-unbrick"
  cmp -s "$H/$SETTINGS_REL" "$C/before.json" && ok || bad "settings.json changed"
  finish
fi

if begin quickstart-rerun-keeps-the-pre-install-backup; then
  # A second full run finds settings.json already cut over. Backing that up
  # would make it the newest backup, and --rollback would restore the cutover.
  qs_setup; qs_cloud; mkdir -p "$H/.claude"
  printf '{"env": {"AWS_PROFILE": "me"}}\n' > "$H/$SETTINGS_REL"
  run_qs FAKE_QS_CALL=cutover FAKE_TURN=ok
  expect_rc 0; expect_base_url "http://127.0.0.1:1"
  run_qs FAKE_QS_CALL=cutover FAKE_TURN=ok
  expect_rc 0; expect_out "already pointed here"
  run_qs -- --rollback
  expect_rc 0; expect_base_url unset
  "$PY3" -c 'import json, sys; sys.exit(0 if json.load(open(sys.argv[1]))["env"].get("AWS_PROFILE") == "me" else 1)' \
    "$H/$SETTINGS_REL" && ok || bad "--rollback did not bring back the pre-install settings.json"
  finish
fi

if begin quickstart-rerun-names-a-cut-over-backup; then
  # An older quickstart could leave a cut-over copy as the newest backup, which
  # --rollback skips: a re-run names the backup --rollback restores, or says
  # that only subrouter-unbrick takes it off.
  qs_setup; qs_cloud; mkdir -p "$H/.claude"
  printf '{"env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:1"}}\n' > "$H/$SETTINGS_REL"
  cp "$H/$SETTINGS_REL" "$H/$SETTINGS_REL.subrouter-bak-20260901-000000"
  run_qs FAKE_QS_CALL=cutover FAKE_TURN=ok
  expect_rc 0; expect_out "already pointed here"
  expect_out "no backup is off the daemon"; expect_out "subrouter-unbrick"
  printf '{}\n' > "$H/$SETTINGS_REL.subrouter-bak-20200101-000000"
  run_qs FAKE_QS_CALL=cutover FAKE_TURN=ok
  expect_rc 0; expect_out "--rollback restores settings.json.subrouter-bak-20200101-000000"
  finish
fi

if begin quickstart-done-says-where-sr-is; then
  # setup puts sr in ~/bin, which macOS does not put on PATH.
  qs_setup; fake_binary "$H/bin/subrouter"; ln -s subrouter "$H/bin/sr"
  run_qs FAKE_QS_STUB="preflight build install accounts prove cutover" --
  expect_rc 0; expect_out "Done."
  expect_out "\`sr\` is $H/bin/sr, and ~/bin is not on your PATH"
  finish
fi

if begin quickstart-done-sr-on-path; then
  # Calibration: an sr on PATH that is the installed one gets no PATH advice.
  qs_setup; fake_binary "$H/bin/subrouter"; ln -s subrouter "$H/bin/sr"; ln -s "$H/bin/sr" "$C/bin/sr"
  run_qs FAKE_QS_STUB="preflight build install accounts prove cutover" --
  expect_rc 0; expect_out "\`sr\` is $H/bin/sr, on your PATH"; expect_no_out "not on your PATH"
  finish
fi

echo
echo "setup tool tests: $PASS assertions passed, $FAILED failed"
[ "$FAILED" -eq 0 ] || { echo "failing:$FAIL_LIST"; exit 1; }
