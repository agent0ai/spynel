#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
project_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)

if [ "$#" -ne 4 ]; then
  echo "usage: $0 <version> <goos> <goarch> <output-directory>" >&2
  exit 2
fi

version=${1#v}
target_os=$2
target_arch=$3
output_dir=$4
go_bin=${SPYNEL_GO_BINARY:-go}

case "$version" in
  [0-9]*.*.*) ;;
  *) echo "invalid release version: $version" >&2; exit 2 ;;
esac
case "$version" in
  *[!0-9A-Za-z.+-]*) echo "invalid release version: $version" >&2; exit 2 ;;
esac

host_os=$($go_bin env GOOS)
host_arch=$($go_bin env GOARCH)
if [ "$host_os/$host_arch" != "$target_os/$target_arch" ]; then
  echo "native package requires a $target_os/$target_arch runner, found $host_os/$host_arch" >&2
  exit 1
fi

case "$target_os/$target_arch" in
  linux/amd64|linux/arm64|darwin/amd64|darwin/arm64|windows/amd64) ;;
  *) echo "unsupported release target: $target_os/$target_arch" >&2; exit 1 ;;
esac

work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT HUP INT TERM
stage_dir="$work_dir/spynel_${version}_${target_os}_${target_arch}"
mkdir -p "$stage_dir" "$output_dir"

binary=spynel
if [ "$target_os" = windows ]; then
  binary=spynel.exe
fi

(cd "$project_dir" && CGO_ENABLED=1 GOOS="$target_os" GOARCH="$target_arch" "$go_bin" build -trimpath -ldflags="-s -w -X main.version=$version" -o "$stage_dir/$binary" ./cmd/spynel)

case "$target_os" in
  linux)
    module_dir=$(cd "$project_dir" && "$go_bin" list -m -f '{{.Dir}}' github.com/k2-fsa/sherpa-onnx-go-linux)
    case "$target_arch" in
      amd64) library_arch=x86_64-unknown-linux-gnu ;;
      arm64) library_arch=aarch64-unknown-linux-gnu ;;
    esac
    mkdir -p "$stage_dir/lib" "$stage_dir/licenses/sherpa-onnx"
    cp "$module_dir/lib/$library_arch/libsherpa-onnx-c-api.so" "$stage_dir/lib/"
    cp "$module_dir/lib/$library_arch/libonnxruntime.so" "$stage_dir/lib/"
    cp "$module_dir/LICENSE" "$stage_dir/licenses/sherpa-onnx/LICENSE"
    ;;
  darwin)
    module_dir=$(cd "$project_dir" && "$go_bin" list -m -f '{{.Dir}}' github.com/k2-fsa/sherpa-onnx-go-macos)
    case "$target_arch" in
      amd64) library_arch=x86_64-apple-darwin ;;
      arm64) library_arch=aarch64-apple-darwin ;;
    esac
    mkdir -p "$stage_dir/lib" "$stage_dir/licenses/sherpa-onnx"
    cp "$module_dir/lib/$library_arch/libsherpa-onnx-c-api.dylib" "$stage_dir/lib/"
    cp "$module_dir/lib/$library_arch/libonnxruntime.1.27.0.dylib" "$stage_dir/lib/"
    cp "$module_dir/LICENSE" "$stage_dir/licenses/sherpa-onnx/LICENSE"
    ;;
  windows)
    module_dir=$(cd "$project_dir" && "$go_bin" list -m -f '{{.Dir}}' github.com/k2-fsa/sherpa-onnx-go-windows)
    if command -v cygpath >/dev/null 2>&1; then
      module_dir=$(cygpath -u "$module_dir")
    fi
    mkdir -p "$stage_dir/licenses/sherpa-onnx"
    cp "$module_dir/lib/x86_64-pc-windows-gnu/sherpa-onnx-c-api.dll" "$stage_dir/"
    cp "$module_dir/lib/x86_64-pc-windows-gnu/onnxruntime.dll" "$stage_dir/"
    cp "$module_dir/LICENSE" "$stage_dir/licenses/sherpa-onnx/LICENSE"
    ;;
esac

mkdir -p "$stage_dir/licenses/miniaudio"
cp "$project_dir/LICENSE" "$project_dir/README.md" "$project_dir/THIRD_PARTY_NOTICES.md" "$stage_dir/"
cp "$project_dir/internal/media/miniaudio/LICENSE" "$stage_dir/licenses/miniaudio/LICENSE"
mkdir -p "$stage_dir/licenses/onnxruntime"
cp "$project_dir/third_party/onnxruntime/LICENSE" "$stage_dir/licenses/onnxruntime/LICENSE"
mkdir -p "$stage_dir/licenses/pion-opus"
cp "$project_dir/third_party/pion-opus/LICENSE" "$stage_dir/licenses/pion-opus/LICENSE"

"$stage_dir/$binary" --version >/dev/null

archive_base="spynel_${version}_${target_os}_${target_arch}"
if [ "$target_os" = windows ]; then
  archive_path=$(CDPATH= cd -- "$output_dir" && pwd)/$archive_base.zip
  (cd "$stage_dir" && 7z a -bd -tzip "$archive_path" ./* >/dev/null)
else
  archive_path=$(CDPATH= cd -- "$output_dir" && pwd)/$archive_base.tar.gz
  tar -C "$stage_dir" -czf "$archive_path" .
fi

echo "$archive_path"
