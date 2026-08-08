#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
project_dir=$(CDPATH= cd -- "$script_dir/.." && pwd -P)
temp_base=${TMPDIR:-/tmp}
temp_base=$(CDPATH= cd -- "$temp_base" && pwd -P)

case "$temp_base/" in
  "$project_dir"/*)
    echo "cold-cache temporary base must be outside the project workspace: $temp_base" >&2
    exit 2
    ;;
esac

if [ "$#" -eq 0 ]; then
  echo "usage: $0 COMMAND [ARG...]" >&2
  exit 2
fi

cache_dir=$(mktemp -d "$temp_base/spynel-gocache.XXXXXX")
tree_alive() {
  [ -n "${child_pid:-}" ] && kill -0 -"$child_pid" 2>/dev/null
}
tree_running() {
  if [ -n "${child_pid:-}" ] && ps -e -o pgid= -o stat= 2>/dev/null |
    awk -v pgid="$child_pid" '$1 == pgid && $2 !~ /^Z/ { found = 1 } END { exit !found }'; then
    return 0
  fi
  grep -l -z -F -x -- "GOCACHE=$cache_dir" /proc/[0-9]*/environ \
    >/dev/null 2>&1
}
wait_tree() {
  while tree_running; do
    sleep 0.02
  done
}
terminate_tree() {
  signal=${1:-TERM}
  if tree_alive; then
    kill -"$signal" -"$child_pid" 2>/dev/null || true
  fi
  # A diagnostic descendant can establish a new session and leave the process
  # group. Every owned process still inherits the unique cache identity, so use
  # that exact signature to find and terminate escapees before cache removal.
  for process in $(grep -l -z -F -x -- "GOCACHE=$cache_dir" \
    /proc/[0-9]*/environ 2>/dev/null || true); do
    pid=${process#/proc/}
    pid=${pid%/environ}
    kill -"$signal" "$pid" 2>/dev/null || true
  done
}
cleanup() {
  terminate_tree TERM
  terminate_tree KILL
  wait_tree
  case "$cache_dir" in
    "$temp_base"/spynel-gocache.*)
      if [ -d "$cache_dir" ]; then
        rm -rf -- "$cache_dir"
      fi
      ;;
    *)
      echo "refusing to clean unexpected cold-cache path: $cache_dir" >&2
      return 1
      ;;
  esac
}
on_signal() {
  signal=$1
  status=$1
  case "$signal" in
    HUP) status=129 ;;
    INT) status=130 ;;
    TERM) status=143 ;;
  esac
  trap - HUP INT TERM
  if [ -n "${child_pid:-}" ]; then
    terminate_tree "$signal"
    terminate_tree KILL
    wait "$child_pid" 2>/dev/null || true
    wait_tree
  fi
  exit "$status"
}
trap cleanup 0
trap 'on_signal HUP' HUP
trap 'on_signal INT' INT
trap 'on_signal TERM' TERM

if ! command -v setsid >/dev/null 2>&1; then
  echo "cold-cache requires setsid to bound the diagnostic process tree" >&2
  exit 2
fi
if [ ! -d /proc/self ] || [ ! -r /proc/self/environ ]; then
  echo "cold-cache requires readable procfs to validate the complete diagnostic process tree" >&2
  exit 2
fi

GOCACHE="$cache_dir" setsid "$@" &
child_pid=$!
set +e
wait "$child_pid"
status=$?
set -e
terminate_tree TERM
terminate_tree KILL
wait "$child_pid" 2>/dev/null || true
wait_tree
exit "$status"
