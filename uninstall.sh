#!/bin/sh
set -eu

main() {
  [ "$#" -eq 0 ] || { echo 'Usage: uninstall.sh' >&2; exit 2; }
  bootstrap=$(mktemp "${TMPDIR:-/tmp}/spynel-uninstall.XXXXXXXX")
  trap 'rm -f "$bootstrap"' 0
  trap 'exit 1' HUP INT TERM
  curl -LfS --proto '=https' --proto-redir '=https' --connect-timeout 10 --max-time 30 \
    -o "$bootstrap" https://spynel.agent-zero.ai/install.sh
  sh "$bootstrap" --uninstall
}

main "$@"
