#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
project_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
go_version=1.26.5

if command -v go >/dev/null 2>&1; then
  go_bin=$(command -v go)
else
  case "$(uname -m)" in
    x86_64|amd64) go_arch=amd64 ;;
    aarch64|arm64) go_arch=arm64 ;;
    *) echo "unsupported development architecture: $(uname -m)" >&2; exit 1 ;;
  esac
  toolchain_dir="$project_dir/.tmp-toolchains"
  go_bin="$toolchain_dir/go/bin/go"
  if [ ! -x "$go_bin" ]; then
    mkdir -p "$toolchain_dir"
    archive="${TMPDIR:-/tmp}/spynel-go-${go_version}-${go_arch}.tar.gz"
    curl -fsSL "https://go.dev/dl/go${go_version}.linux-${go_arch}.tar.gz" -o "$archive"
    tar -xzf "$archive" -C "$toolchain_dir"
  fi
fi

action=${1:-build}
if [ "$#" -gt 0 ]; then shift; fi

case "$action" in
  build)
    mkdir -p "$project_dir/.tmp-bin"
    (cd "$project_dir" && CGO_ENABLED=1 "$go_bin" build -o .tmp-bin/spynel ./cmd/spynel)
    echo "$project_dir/.tmp-bin/spynel"
    ;;
  test)
    (cd "$project_dir" && CGO_ENABLED=1 "$go_bin" test ./... && CGO_ENABLED=1 "$go_bin" vet ./...)
    ;;
  dox)
    (cd "$project_dir" && "$go_bin" run ./scripts/doxcheck)
    ;;
  run)
    "$script_dir/dev.sh" build >/dev/null
    exec "$project_dir/.tmp-bin/spynel" "$@"
    ;;
  *)
    echo "usage: $0 [build|test|dox|run [spynel arguments...]]" >&2
    exit 2
    ;;
esac
