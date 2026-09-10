#!/bin/sh
# Release bootstrap: curl -LsSf https://spynel.agent-zero.ai/install.sh | sh
# The verified native binary owns full bundle validation and atomic installation.
set -eu

main() {
  for tool in curl tar awk mktemp; do
    command -v "$tool" >/dev/null 2>&1 || { echo "Required command not found: $tool" >&2; exit 1; }
  done
  case "$(uname -s)" in
    Linux) target_os=linux ;;
    Darwin) target_os=darwin ;;
    *) echo 'Spynel supports Linux and macOS; Windows is temporarily unsupported.' >&2; exit 1 ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64) target_arch=amd64 ;;
    aarch64|arm64) target_arch=arm64 ;;
    *) echo 'Spynel supports amd64 and arm64 processors.' >&2; exit 1 ;;
  esac
  if command -v sha256sum >/dev/null 2>&1; then
    hash_command=sha256sum
  elif command -v shasum >/dev/null 2>&1; then
    hash_command='shasum -a 256'
  else
    echo 'Install sha256sum or shasum to verify the release.' >&2; exit 1
  fi
  : "${HOME:?HOME must identify your user directory}"
  install_root=${SPYNEL_INSTALL_DIR:-"$HOME/.local/share/spynel"}
  bin_dir=${SPYNEL_BIN_DIR:-"$HOME/.local/bin"}
  case "$install_root:$bin_dir" in *'
'*) echo 'Installation paths must not contain newlines.' >&2; exit 1 ;; esac
  case "$install_root" in /*) ;; *) echo 'SPYNEL_INSTALL_DIR must be absolute.' >&2; exit 1 ;; esac
  case "$bin_dir" in /*) ;; *) echo 'SPYNEL_BIN_DIR must be absolute.' >&2; exit 1 ;; esac
  stage=$(mktemp -d "${TMPDIR:-/tmp}/spynel-install.XXXXXXXX")
  trap 'rm -rf "$stage"' 0
  trap 'exit 1' HUP INT TERM
  version=${SPYNEL_VERSION:-}
  if [ -z "$version" ]; then
    latest=$(curl -LsSf --proto '=https' --proto-redir '=https' --connect-timeout 10 --max-time 10 --max-redirs 5 -o /dev/null -w '%{url_effective}' https://github.com/agent0ai/spynel/releases/latest)
    version=${latest##*/v}
  fi
  version=${version#v}
  # Stable releases only; the native installer performs strict semantic validation.
  case "$version" in ''|*[!0-9.]*|.*|*.) echo 'Unable to select a stable Spynel release.' >&2; exit 1 ;; esac
  archive="spynel_${version}_${target_os}_${target_arch}.tar.gz"
  base=${SPYNEL_DOWNLOAD_BASE:-"https://github.com/agent0ai/spynel/releases/download/v$version"}
  base=${base%/}
  download "$base/$archive" "$stage/$archive" 536870912
  download "$base/checksums.txt" "$stage/checksums.txt" 1048576
  expected=$(awk -v name="$archive" '$2 == name { if (NF != 2 || ++n > 1 || length($1) != 64 || $1 ~ /[^0-9a-fA-F]/) exit 1; hash=tolower($1) } END { if (n != 1) exit 1; print hash }' "$stage/checksums.txt")
  actual=$($hash_command "$stage/$archive" | awk '{print $1}')
  [ "$expected" = "$actual" ] || { echo 'Release checksum mismatch; installation unchanged.' >&2; exit 1; }
  # Inspect all paths and types before executing the verified bootstrap. Extract
  # only exact regular runtime members to fixed output paths, never archive paths.
  (ulimit -f 8192; ulimit -t 120; tar -tzf "$stage/$archive" > "$stage/entries")
  awk '
    NR > 4096 || length($0) > 1024 || $0 !~ /^\.\/[A-Za-z0-9_./+-]*$/ { exit 1 }
    { name=$0; sub(/^\.\//,"",name); sub(/\/$/,"",name) }
    name ~ /(^|\/)\.\.(\/|$)/ || seen[name]++ { exit 1 }
    END { if (NR == 0) exit 1 }
  ' "$stage/entries" || { echo 'Release archive contains unsafe paths.' >&2; exit 1; }
  (ulimit -f 8192; ulimit -t 120; tar -tvzf "$stage/$archive" > "$stage/types")
  awk 'substr($0,1,1) != "-" && substr($0,1,1) != "d" { exit 1 }' "$stage/types" || { echo 'Release archive contains links or special files.' >&2; exit 1; }
  mkdir "$stage/runtime" "$stage/runtime/lib"
  extract_runtime spynel
  case "$target_os" in
    linux) extract_runtime lib/libsherpa-onnx-c-api.so; extract_runtime lib/libonnxruntime.so ;;
    darwin) extract_runtime lib/libsherpa-onnx-c-api.dylib; extract_runtime lib/libonnxruntime.1.27.0.dylib ;;
  esac
  chmod 700 "$stage/runtime/spynel"
  if ! "$stage/runtime/spynel" install-bundle --root "$install_root" --archive "$stage/$archive" --checksums "$stage/checksums.txt" --version "$version"; then
    echo 'Installation failed. Use a release with standalone installer support; the prior bundle is retained.' >&2
    exit 1
  fi
  mkdir -p "$bin_dir"
  if ln -s "$install_root/spynel" "$bin_dir/spynel" 2>/dev/null; then
    :
  elif [ "$(readlink "$bin_dir/spynel" 2>/dev/null || true)" != "$install_root/spynel" ]; then
    echo "Preserved the existing $bin_dir/spynel. Run: \"$install_root/spynel\""
    return
  fi
  echo "Installed Spynel $version. Run: \"$bin_dir/spynel\""
  case ":$PATH:" in
    *":$bin_dir:"*) ;;
    *) echo "Add \"$bin_dir\" to PATH in your shell profile, then open a new terminal." ;;
  esac
  resolved=$(command -v spynel || true)
  if [ -n "$resolved" ] && [ "$resolved" != "$bin_dir/spynel" ]; then
    echo "Your PATH currently selects $resolved. Use \"$bin_dir/spynel\" for this installation."
  fi
}

download() (
  # ulimit also bounds streamed responses on curl versions that only check the
  # declared Content-Length. Only explicitly configured mirrors may use HTTP.
  ulimit -f 1048576
  protocols='=https'
  case "$1" in http://*) protocols='=http,https' ;; esac
  curl -LsSf --proto "$protocols" --proto-redir "$protocols" --connect-timeout 10 --max-time 120 --max-redirs 5 --max-filesize "$3" -o "$2" "$1"
  [ "$(wc -c < "$2")" -le "$3" ]
)

extract_runtime() (
  ulimit -f 1048576
  ulimit -t 120
  tar -xOzf "$stage/$archive" "./$1" > "$stage/runtime/$1"
  [ -s "$stage/runtime/$1" ]
)

# Keep execution behind a fully parsed function so piped stdin is never consumed
# as application input, and a truncated bootstrap cannot start partial work.
main "$@"
