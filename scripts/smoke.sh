#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
project_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
binary="$project_dir/bin/spynel"

"$script_dir/dev.sh" build >/dev/null
smoke_dir=$(mktemp -d "${TMPDIR:-/tmp}/spynel-smoke.XXXXXX")
trap 'rm -rf "$smoke_dir"' EXIT HUP INT TERM

"$binary" init --no-start --dir "$smoke_dir"
(cd "$smoke_dir" && "$binary" config)
(cd "$smoke_dir" && "$binary" task "smoke test task creation")
(cd "$smoke_dir" && "$binary" goal "smoke test goal creation")

test -f "$smoke_dir/spynel.yaml"
test -f "$smoke_dir/.spynel/AGENTS.md"
test "$(find "$smoke_dir/.spynel/tasks/todo" -type f -name '*.md' | wc -l)" -eq 1
test "$(find "$smoke_dir/.spynel/goals/active" -type f -name '*.md' | wc -l)" -eq 1

echo "Spynel smoke test passed: $smoke_dir"
