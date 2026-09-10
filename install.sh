#!/bin/sh
# Release bootstrap: curl -LsSf https://spynel.agent-zero.ai/install.sh | sh
# The verified native binary owns full bundle validation and atomic installation.
set -eu

main() {
  case "${1:-}" in
    --help|-h) echo 'Usage: install.sh'; return ;;
    --uninstall|'') ;;
    *) echo 'Usage: install.sh' >&2; exit 2 ;;
  esac
  [ "$#" -le 1 ] || { echo 'Usage: install.sh' >&2; exit 2; }
  : "${HOME:?HOME must identify your user directory}"
  install_root=${SPYNEL_INSTALL_DIR:-"$HOME/.local/share/spynel"}
  case "$install_root" in /*) ;; *) echo 'SPYNEL_INSTALL_DIR must be absolute.' >&2; exit 1 ;; esac
  while [ "${install_root%/}" != "$install_root" ] && [ "$install_root" != / ]; do install_root=${install_root%/}; done
  case "$install_root" in *'
'*) echo 'Installation paths must not contain newlines.' >&2; exit 1 ;; esac
  if [ "${1:-}" = --uninstall ]; then
    uninstalling=true
  else
    uninstalling=false
  fi
  for tool in curl tar awk mktemp id sed grep; do
    command -v "$tool" >/dev/null 2>&1 || { echo "Required command not found: $tool" >&2; exit 1; }
  done
  case "$(uname -s)" in
    Linux) target_os=linux ;;
    Darwin) target_os=darwin ;;
    *) echo 'Spynel supports Linux and macOS.' >&2; exit 1 ;;
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
  bin_dir=${SPYNEL_BIN_DIR:-$(default_bin_dir)}
  case "$install_root:$bin_dir" in *'
'*) echo 'Installation paths must not contain newlines.' >&2; exit 1 ;; esac
  case "$install_root" in /*) ;; *) echo 'SPYNEL_INSTALL_DIR must be absolute.' >&2; exit 1 ;; esac
  case "$bin_dir" in /*) ;; *) echo 'SPYNEL_BIN_DIR must be absolute.' >&2; exit 1 ;; esac
  case "$bin_dir" in *:*) echo 'SPYNEL_BIN_DIR cannot contain a PATH separator (:).' >&2; exit 1 ;; esac
  bin_sudo=
  if [ "$uninstalling" = false ]; then
    if ! mkdir -p "$bin_dir" 2>/dev/null || [ ! -w "$bin_dir" ]; then
      command -v sudo >/dev/null 2>&1 || { echo 'Administrator access is required to install Spynel on PATH; sudo is unavailable.' >&2; exit 1; }
      echo 'Administrator access is required to install the Spynel command.' >&2
      sudo -v
      sudo mkdir -p "$bin_dir"
      bin_sudo=sudo
    fi
  fi
  stage=$(mktemp -d "${TMPDIR:-/tmp}/spynel-install.XXXXXXXX")
  trap 'rm -rf "$stage"' 0
  trap 'exit 1' HUP INT TERM
  version=${SPYNEL_VERSION:-}
  if [ -z "$version" ]; then
    echo 'Finding the latest Spynel release...' >&2
    latest=$(curl -LsSf --proto '=https' --proto-redir '=https' --connect-timeout 10 --max-time 10 --max-redirs 5 -o /dev/null -w '%{url_effective}' https://github.com/agent0ai/spynel/releases/latest)
    version=${latest##*/v}
  fi
  version=${version#v}
  # Stable releases only; the native installer performs strict semantic validation.
  case "$version" in ''|*[!0-9.]*|.*|*.) echo 'Unable to select a stable Spynel release.' >&2; exit 1 ;; esac
  archive="spynel_${version}_${target_os}_${target_arch}.tar.gz"
  base=${SPYNEL_DOWNLOAD_BASE:-"https://github.com/agent0ai/spynel/releases/download/v$version"}
  base=${base%/}
  echo "Downloading Spynel $version for $target_os/$target_arch..." >&2
  download "$base/$archive" "$stage/$archive" 536870912
  echo 'Downloading release checksums...' >&2
  download "$base/checksums.txt" "$stage/checksums.txt" 1048576
  echo 'Verifying and extracting the release...' >&2
  expected=$(awk -v name="$archive" '$2 == name { if (NF != 2 || ++n > 1 || length($1) != 64 || $1 ~ /[^0-9a-fA-F]/) exit 1; hash=tolower($1) } END { if (n != 1) exit 1; print hash }' "$stage/checksums.txt")
  actual=$($hash_command "$stage/$archive" | awk '{print $1}')
  [ "$expected" = "$actual" ] || { echo 'Release checksum mismatch; installation unchanged.' >&2; exit 1; }
  # Inspect all paths and types before executing the verified bootstrap. Extract
  # only exact regular runtime members to fixed output paths, never archive paths.
  (ulimit -f 8192; ulimit -t 120; tar -tzf "$stage/$archive" > "$stage/entries")
  awk '
    NR > 4096 || length($0) > 1024 || $0 !~ /^\.\/[A-Za-z0-9_.\/+-]*$/ { exit 1 }
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
  if [ "$uninstalling" = true ]; then
    echo 'Uninstalling Spynel...' >&2
    "$stage/runtime/spynel" uninstall-bundles --root "$install_root"
    return
  fi
  echo "Installing Spynel $version..." >&2
  if ! "$stage/runtime/spynel" install-bundle --root "$install_root" --archive "$stage/$archive" --checksums "$stage/checksums.txt" --version "$version"; then
    echo 'Installation failed. Use a release with standalone installer support; the prior bundle is retained.' >&2
    exit 1
  fi
  if $bin_sudo ln -s "$install_root/spynel" "$bin_dir/spynel" 2>/dev/null; then
    :
  elif [ ! -e "$bin_dir/spynel" ] && [ ! -L "$bin_dir/spynel" ]; then
    echo "Cannot create the launcher in $bin_dir; check directory permissions." >&2
    exit 1
  elif [ "$(readlink "$bin_dir/spynel" 2>/dev/null || true)" != "$install_root/spynel" ]; then
    echo "Preserved the existing $bin_dir/spynel. Run: \"$install_root/spynel\""
    return 1
  fi
  printf '%s\n' "$bin_dir" > "$install_root/.bin-dir"
  quoted_bin=$(shell_quote "$bin_dir")
  path_line="case \":\$PATH:\" in *:$quoted_bin:*) ;; *) export PATH=$quoted_bin:\$PATH ;; esac # Spynel installer"
  printf '%s\n' "$path_line" > "$install_root/env"
  if on_path "$bin_dir"; then
    [ "$(spynel --version)" = "spynel $version" ] || { echo 'The installed Spynel command could not be verified on PATH.' >&2; exit 1; }
    echo "Installed Spynel $version. Run: spynel"
  else
    configure_path
    echo "Installed Spynel $version. PATH configured for your shell."
    # A piped child cannot change its parent's environment. Show a command
    # usable immediately when no writable directory was already on PATH.
    printf 'Run now: %s\n' "$(shell_quote "$bin_dir/spynel")"
  fi
  resolved=$(command -v spynel || true)
  if [ -n "$resolved" ] && [ "$resolved" != "$bin_dir/spynel" ]; then
    echo "Your PATH currently selects $resolved. Use \"$bin_dir/spynel\" for this installation."
  fi
}

on_path() {
  case ":$PATH:" in *":$1:"*) return 0 ;; *) return 1 ;; esac
}

default_bin_dir() {
  if [ "$(id -u)" = 0 ] && on_path /usr/local/bin &&
     { { [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; } || { [ ! -e /usr/local/bin ] && [ -w /usr/local ]; }; }; then
    echo /usr/local/bin
    return
  fi
  if on_path "$HOME/.local/bin"; then
    printf '%s\n' "$HOME/.local/bin"
    return
  fi
  remaining_path=$PATH:
  while [ -n "$remaining_path" ]; do
    directory=${remaining_path%%:*}
    remaining_path=${remaining_path#*:}
    case "$directory" in /*) ;; *) continue ;; esac
    if [ -d "$directory" ] && [ -w "$directory" ] && [ -x "$directory" ]; then
      printf '%s\n' "$directory"
      return
    fi
  done
  # A piped process cannot change its caller's PATH. The default must use
  # an existing PATH directory, requesting write permission when necessary.
  if on_path /usr/local/bin; then
    echo /usr/local/bin
    return
  fi
  remaining_path=$PATH:
  while [ -n "$remaining_path" ]; do
    directory=${remaining_path%%:*}
    remaining_path=${remaining_path#*:}
    case "$directory" in /*) printf '%s\n' "$directory"; return ;; esac
  done
  echo 'PATH has no absolute directory for the Spynel command.' >&2
  return 1
}

shell_quote() {
  printf "'"
  printf '%s' "$1" | sed "s/'/'\\\\''/g"
  printf "'"
}

configure_path() {
  case "${SHELL:-}" in
    */fish)
      "$SHELL" -c 'fish_add_path --universal --prepend -- $argv[1]' -- "$bin_dir"
      return
      ;;
    */zsh)
      append_path "${ZDOTDIR:-$HOME}/.zprofile"
      append_path "${ZDOTDIR:-$HOME}/.zshrc"
      ;;
    *)
      profile=$HOME/.profile
      if [ -f "$HOME/.bash_profile" ]; then profile=$HOME/.bash_profile
      elif [ -f "$HOME/.bash_login" ]; then profile=$HOME/.bash_login; fi
      append_path "$profile"
      append_path "$HOME/.bashrc"
      ;;
  esac
}

append_path() {
  if ! grep -Fqx "$path_line" "$1" 2>/dev/null; then
    (umask 077; printf '\n%s\n' "$path_line" >> "$1")
  fi
}

download() (
  # ulimit also bounds streamed responses on curl versions that only check the
  # declared Content-Length. Only explicitly configured mirrors may use HTTP.
  ulimit -f 1048576
  protocols='=https'
  case "$1" in http://*) protocols='=http,https' ;; esac
  curl -LfS --progress-bar --proto "$protocols" --proto-redir "$protocols" --connect-timeout 10 --max-time 120 --max-redirs 5 --max-filesize "$3" -o "$2" "$1"
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
