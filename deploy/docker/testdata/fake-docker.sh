#!/usr/bin/env sh
# A stand-in for docker in the tests of deploy/docker/test.sh and
# ops/bin/sr-local-ci, so that no test starts Docker. It appends each call to
# $FAKE_DOCKER_DIR/calls. A `docker run` of trim-go-cache.sh runs the trim on
# this machine, reading each -v mount as a folder here: a path as itself, and
# a named volume as $FAKE_DOCKER_DIR/volumes/<name>. With FAKE_DOCKER_TRIM_RC
# set, that run exits with it instead. Every other call exits 0.
set -u
printf '%s\n' "$*" >>"${FAKE_DOCKER_DIR}/calls"
[ "${1:-}" = run ] || exit 0
shift
mounts=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -v)
      mounts="${mounts}$2
"
      shift 2
      ;;
    -e | -w | --name | --network | --security-opt) shift 2 ;;
    -*) shift ;;
    *) break ;;
  esac
done
shift # the image
case "$*" in *trim-go-cache.sh*) ;; *) exit 0 ;; esac
[ -z "${FAKE_DOCKER_TRIM_RC:-}" ] || exit "${FAKE_DOCKER_TRIM_RC}"
n=$#
for arg do
  mapped=${arg}
  while IFS= read -r m; do
    [ -n "${m}" ] || continue
    src=${m%%:*}
    dst=${m#*:}
    dst=${dst%%:*}
    case "${src}" in
      /*) host=${src} ;;
      *) host=${FAKE_DOCKER_DIR}/volumes/${src} ;;
    esac
    case "${arg}" in "${dst}" | "${dst}"/*)
      mapped=${host}${arg#"${dst}"}
      break
      ;;
    esac
  done <<EOF
${mounts}
EOF
  set -- "$@" "${mapped}"
done
shift "${n}"
exec "$@"
