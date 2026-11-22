#!/usr/bin/env sh
# trim-go-cache.sh DIR LIMIT_KIB AGE_MINUTES
#
# Keeps a Go build cache that many test runs share near a size limit. When DIR
# holds more than LIMIT_KIB (as du -k counts), it deletes the cache entries
# not used for AGE_MINUTES and prints one line that says how much it freed. At
# or under the limit, with a limit of 0, or with no DIR yet, it deletes
# nothing and prints nothing. deploy/docker/test.sh and ops/bin/sr-local-ci
# run it before each run in Docker (ops/PATCHES.md §85).
#
# An entry is what Go's own trim deletes: a file named *-a or *-d in one of the
# two-hex-digit folders. README, trim.txt and every other file stay. Go
# updates the time of an entry it uses only once that time is an hour old, so
# an entry used within AGE_MINUTES has a time within AGE_MINUTES plus an hour,
# and the trim waits that hour longer, as Go's own trim does. Two trims at once
# each skip what the other deleted first.
set -u

usage() {
  echo "usage: trim-go-cache.sh DIR LIMIT_KIB AGE_MINUTES (whole numbers)" >&2
  exit 2
}

[ "$#" -eq 3 ] || usage
dir=$1 limit=$2 age=$3
for n in "${limit}" "${age}"; do
  case "${n}" in '' | *[!0-9]*) usage ;; esac
done
[ "${limit}" -gt 0 ] || exit 0
[ -d "${dir}" ] || exit 0

human() { # bytes
  awk -v b="$1" 'BEGIN {
    if (b >= 1073741824) printf "%.1f GB", b / 1073741824
    else if (b >= 1048576) printf "%.1f MB", b / 1048576
    else if (b >= 1024) printf "%.0f KB", b / 1024
    else printf "%d B", b
  }'
}

# A file another trim deletes while du reads the folder is not an error.
size=$(du -sk "${dir}" 2>/dev/null | cut -f1)
case "${size:-x}" in *[!0-9]*)
  echo "trim-go-cache.sh: cannot measure ${dir}" >&2
  exit 1
  ;;
esac
[ "${size}" -gt "${limit}" ] || exit 0

# -ls prints each entry's size in bytes in its seventh field before -delete.
# shellcheck disable=SC2046
set -- $(cd "${dir}" && find . -ignore_readdir_race -mindepth 2 -maxdepth 2 \
  -path './[0-9a-f][0-9a-f]/*' -name '*-[ad]' -type f -mmin +"$((age + 60))" -ls -delete |
  awk '{ n++; b += $7 } END { printf "%d %.0f\n", n, b }')
deleted=${1:-0} freed=${2:-0}

case $((age % 60)) in
  0) unused="$((age / 60))h" ;;
  *) unused="${age} min" ;;
esac
entries=entries
[ "${deleted}" != 1 ] || entries=entry
over="${dir}: $(human $((size * 1024))) is over the $(human $((limit * 1024))) limit"
if [ "${deleted}" -gt 0 ]; then
  echo "${over}; deleted ${deleted} ${entries} not used for ${unused}, which freed $(human "${freed}")"
else
  echo "${over}; every entry may have been used in the last ${unused}, so nothing was freed"
fi
