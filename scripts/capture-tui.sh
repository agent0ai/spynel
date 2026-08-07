#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
project_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
capture_dir=${1:-"$project_dir/.spynel-dev/tui-captures"}
case "$capture_dir" in
  /*) ;;
  *) capture_dir="$(pwd)/$capture_dir" ;;
esac
ansi_dir="$capture_dir/ansi"

if command -v go >/dev/null 2>&1; then
  go_bin=$(command -v go)
else
  go_bin="$project_dir/.spynel-dev/toolchains/go/bin/go"
fi
if [ ! -x "$go_bin" ]; then
  echo "Go toolchain not found; run scripts/dev.sh build first" >&2
  exit 1
fi
if ! command -v node >/dev/null 2>&1; then
  echo "Node.js is required to convert ANSI captures" >&2
  exit 1
fi
if ! command -v chromium >/dev/null 2>&1; then
  echo "Chromium is required to capture PNG screenshots" >&2
  exit 1
fi

mkdir -p "$ansi_dir"
(cd "$project_dir" && SPYNEL_CAPTURE_DIR="$ansi_dir" "$go_bin" test ./internal/channel/tui -run '^TestVisualCapture$' -count=1)

for source in "$ansi_dir"/*.ansi; do
  name=$(basename "$source" .ansi)
  html="$capture_dir/$name.html"
  png="$capture_dir/$name.png"
  node "$script_dir/ansi-to-html.mjs" "$source" "$html"
  chromium --headless --disable-gpu --no-sandbox --hide-scrollbars \
    --run-all-compositor-stages-before-draw --virtual-time-budget=1000 \
    --window-size=1120,620 --screenshot="$png" "file://$html" >/dev/null 2>&1
  echo "$png"
done

contact_html="$capture_dir/theme-contact-sheet.html"
contact_png="$capture_dir/theme-contact-sheet.png"
{
  printf '%s\n' '<!doctype html><meta charset="utf-8"><title>Spynel stock themes</title>'
  printf '%s\n' '<style>body{margin:0;padding:28px;background:#20242b;color:#f4f4f4;font:20px system-ui,sans-serif}h1{margin:0 0 20px}.grid{display:grid;grid-template-columns:repeat(3,1fr);gap:18px}.card{background:#303640;border-radius:10px;overflow:hidden}.label{padding:10px 14px;font-weight:700}.meta{float:right;color:#c4cad3;font-size:16px;font-weight:500}.card img{display:block;width:100%;height:auto}</style>'
  printf '%s\n' '<h1>Spynel stock themes — identical 120 × 34 TUI fixture</h1><div class="grid">'
  for name in spynel hack-the-box github-colorblind-dark gruvbox-dark nord okabe-ito-dark gruvbox-light rose-pine-dawn tol-muted-light catppuccin-latte okabe-ito-light solarized-light; do
    case "$name" in
      gruvbox-light|rose-pine-dawn|tol-muted-light|catppuccin-latte|okabe-ito-light|solarized-light) appearance=light ;;
      *) appearance=dark ;;
    esac
    case "$name" in
      github-colorblind-dark|tol-muted-light|okabe-ito-dark|okabe-ito-light) access=' · color-blind friendly' ;;
      *) access='' ;;
    esac
    printf '<div class="card"><div class="label">%s<span class="meta">%s%s</span></div><img src="theme-%s.png"></div>\n' "$name" "$appearance" "$access" "$name"
  done
  printf '%s\n' '</div>'
} >"$contact_html"
chromium --headless --disable-gpu --no-sandbox --hide-scrollbars \
  --run-all-compositor-stages-before-draw --virtual-time-budget=1000 \
  --window-size=2240,2200 --screenshot="$contact_png" "file://$contact_html" >/dev/null 2>&1
echo "$contact_png"
