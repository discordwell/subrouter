#!/usr/bin/env sh
# Hermetic Docker test harness. Runs the Go suite inside the Dockerfile's
# `test` stage with the repository mounted read-only, so nothing a test does
# can reach the host's live agent configs (~/.claude, ~/.codex, ~/.subrouter,
# ~/.config/subrouter) or mutate the working tree.
#
#   ./deploy/docker/test.sh                       # go test ./...
#   ./deploy/docker/test.sh ./cmd/subrouter -run TestFoo
#   ./deploy/docker/test.sh --self-check          # isolation proof run
#
# SUBROUTER_TEST_NETWORK=1 opts out of --network none for a future test that
# genuinely needs DNS; the default network namespace has loopback only, which
# is all httptest needs.
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH='' cd -- "${script_dir}/../.." && pwd)

die() {
  echo "test: $*" >&2
  exit 1
}

image=subrouter-test
# The module volume is seeded from the image layer only while EMPTY. After
# go.mod gains a dependency, the stale volume masks the rebuilt layer and
# `go test` fails under --network none with a confusing dial error; the fix is
# `docker volume rm subrouter-test-gomod` (or SUBROUTER_TEST_NETWORK=1 once).
gomod_volume=subrouter-test-gomod
gobuild_volume=subrouter-test-gobuild

self_check=0
if [ "${1:-}" = "--self-check" ]; then
  self_check=1
  shift
fi
[ "$#" -gt 0 ] || set -- ./...

network_args="--network none"
if [ "${SUBROUTER_TEST_NETWORK:-0}" = "1" ]; then
  network_args=""
fi

# The three cleared vars are HOME-derived fallbacks (see storepath.StateDir
# and broker.DefaultConfigPath), so with HOME pinned to /home/agent they
# resolve to the very same container paths either way -- the canary test
# asserts exactly that. They are cleared for the go test invocation because
# many existing tests isolate themselves with t.Setenv("HOME", ...) alone and
# an explicit override would defeat that isolation (it turns ~25
# cmd/subrouter tests red). Nothing host-side can leak through here: docker
# run passes no host environment, and no host path is mounted.
# --init matters for correctness: without a PID 1 reaper the process-group
# lifecycle tests see their killed descendants as zombies that still answer
# signal probes, and fail.
run_in_harness() {
  # shellcheck disable=SC2086
  docker run --rm --init ${network_args} \
    -v "${repo_root}:/src:ro" \
    -v "${gomod_volume}:/go/pkg/mod" \
    -v "${gobuild_volume}:/root/.cache/go-build" \
    "${image}" \
    env -u SUBROUTER_STATE_DIR -u SUBROUTER_CLOUD_CONFIG -u CODEX_HOME "$@"
}

command -v docker >/dev/null 2>&1 || die "docker is required"
docker build --target test -t "${image}" "${repo_root}" || die "docker build --target test failed"

if [ "${self_check}" -eq 0 ]; then
  exit_code=0
  run_in_harness go test "$@" || exit_code=$?
  exit "${exit_code}"
fi

# --self-check: prove the isolation properties, not suite health.
# 1. Hash the host's live agent configs before and after a full in-container
#    run and require them byte-identical. shasum only -- these files are never
#    read as content (the first one holds a live token).
hash_file() {
  path=$1
  if [ ! -f "${path}" ]; then
    echo "absent"
    return 0
  fi
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "${path}" | awk '{print $1}'
  elif command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${path}" | awk '{print $1}'
  else
    echo "unhashable"
  fi
}

claude_settings="${HOME}/.claude/settings.json"
codex_config="${HOME}/.codex/config.toml"
claude_before=$(hash_file "${claude_settings}")
codex_before=$(hash_file "${codex_config}")
[ "${claude_before}" != "absent" ] || echo "test: ${claude_settings} absent; hash check will only assert it stays absent"
[ "${codex_before}" != "absent" ] || echo "test: ${codex_config} absent; hash check will only assert it stays absent"

suite_exit=0
# -count=1 defeats Go's result cache: the point of this run is that test code
# actually EXECUTES between the two hash reads, not that cached verdicts are
# replayed.
run_in_harness go test -count=1 ./... || suite_exit=$?
echo "test: full in-container suite exited ${suite_exit} (informational; compare against the documented baseline)"

# 2. The canary must PASS inside the harness -- a skip would mean the harness
#    env is not what the Dockerfile test stage promises.
canary=TestDockerHarnessCanaryResolvesEveryConfigSurfaceUnderTheAgentHome
canary_output=$(run_in_harness go test -count=1 -v -run "${canary}" ./cmd/subrouter) ||
  die "canary test failed:
${canary_output}"
case "${canary_output}" in
  *"--- PASS: ${canary}"*) ;;
  *) die "canary did not report PASS (skipped?):
${canary_output}" ;;
esac
echo "test: canary PASS -- os.UserHomeDir and every derived config path resolve under /home/agent"

# 3. The repository mount must be read-only: a test mutating the working tree
#    has to fail loudly, so a write probe must be refused. The sentinel words
#    distinguish "touch was refused" from "docker itself failed" -- a bare
#    exit-code check would read a dead daemon as a passing probe.
probe_output=$(run_in_harness sh -c 'touch /src/.subrouter-harness-write-probe 2>/dev/null && echo PROBE_WROTE || echo PROBE_REFUSED') ||
  die "write probe could not run (docker failure, not a read-only verdict): ${probe_output}"
case "${probe_output}" in
  *PROBE_REFUSED*) ;;
  *PROBE_WROTE*)
    rm -f "${repo_root}/.subrouter-harness-write-probe"
    die "/src is writable inside the harness; the repository must be mounted read-only"
    ;;
  *) die "write probe produced no verdict (docker failure?): ${probe_output}" ;;
esac
echo "test: /src write probe refused -- repository is mounted read-only"

claude_after=$(hash_file "${claude_settings}")
codex_after=$(hash_file "${codex_config}")
[ "${claude_before}" = "${claude_after}" ] || die "HOST CONFIG DRIFT: ${claude_settings} changed during the run (${claude_before} -> ${claude_after})"
[ "${codex_before}" = "${codex_after}" ] || die "HOST CONFIG DRIFT: ${codex_config} changed during the run (${codex_before} -> ${codex_after})"
echo "test: host agent configs unchanged (claude=${claude_before} codex=${codex_before})"
echo "test: self-check passed"
