#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
project_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
binary="$project_dir/bin/spynel"

"$script_dir/dev.sh" build >/dev/null
smoke_dir=$(mktemp -d "${TMPDIR:-/tmp}/spynel-smoke.XXXXXX")
trap 'rm -rf "$smoke_dir"' EXIT HUP INT TERM

docs_index=$(cd "$smoke_dir" && "$binary" docs)
printf '%s\n' "$docs_index" | grep -q '`goals`'
docs_json=$(cd "$smoke_dir" && "$binary" docs search review --format json)
printf '%s\n' "$docs_json" | grep -q '"schema_version": "spynel.docs/v1"'

"$binary" init --no-start --dir "$smoke_dir"
(cd "$smoke_dir" && "$binary" config)
reviewed_task=$(cd "$smoke_dir" && "$binary" task "smoke test task creation")
direct_task=$(cd "$smoke_dir" && "$binary" task --no-review "collect smoke status")
"$binary" task inspect "$reviewed_task" | grep -q 'Review required: true'
"$binary" task inspect "$direct_task" | grep -q 'Review required: false'
(cd "$smoke_dir" && "$binary" goal "smoke test goal creation")

status_json=$("$binary" status --config "$smoke_dir/spynel.yaml" --conversation smoke --json)
printf '%s\n' "$status_json" | grep -q '"harness_state"'
printf '%s\n' "$status_json" | grep -q '"connections"'

command_output=$("$binary" command --config "$smoke_dir/spynel.yaml" --conversation smoke help commands)
printf '%s\n' "$command_output" | grep -q '/status'
conversation_json=$("$binary" conversations list --config "$smoke_dir/spynel.yaml" --json)
printf '%s\n' "$conversation_json" | grep -q '"conversation":"smoke"'
"$binary" conversations show --config "$smoke_dir/spynel.yaml" --tail 5 cli smoke >/dev/null
branch=$("$binary" conversations resume --config "$smoke_dir/spynel.yaml" cli smoke)
case "$branch" in
  resume-????????) ;;
  *) echo "unexpected resumed CLI conversation: $branch" >&2; exit 1 ;;
esac
"$binary" conversations show --config "$smoke_dir/spynel.yaml" --tail 5 cli "$branch" >/dev/null

if "$binary" followup --config "$smoke_dir/spynel.yaml" --conversation smoke "too late" >/dev/null 2>&1; then
  echo "inactive plain CLI follow-up unexpectedly succeeded" >&2
  exit 1
fi

test -f "$smoke_dir/spynel.yaml"
test -f "$smoke_dir/.spynel/AGENTS.md"
test -f "$smoke_dir/.spynel/prompts/create-task.md"
test -f "$smoke_dir/.spynel/prompts/create-goal.md"
test -f "$smoke_dir/.spynel/prompts/goal-review.md"
test "$(find "$smoke_dir/.spynel/tasks/todo" -type f -name '*.md' | wc -l)" -eq 2
test "$(find "$smoke_dir/.spynel/goals/proposed" -type f -name '*.md' | wc -l)" -eq 1

for status in todo working review reviewing waiting done failed cancelled; do
  test -d "$smoke_dir/.spynel/tasks/$status"
done
for status in proposed planning active review reviewing waiting done abandoned; do
  test -d "$smoke_dir/.spynel/goals/$status"
done

echo "Spynel smoke test passed: $smoke_dir"
