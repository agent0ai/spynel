#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
project_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
binary="$project_dir/bin/spynel"

"$script_dir/dev.sh" build >/dev/null
smoke_dir=$(mktemp -d "${TMPDIR:-/tmp}/spynel-smoke.XXXXXX")
trap 'rm -rf "$smoke_dir"' EXIT HUP INT TERM

dev_bin_dir="$smoke_dir/user bin"
SPYNEL_DEV_BIN_DIR="$dev_bin_dir" "$script_dir/install-dev.sh" >/dev/null
test -x "$dev_bin_dir/spynel"
"$dev_bin_dir/spynel" version >/dev/null

docs_index=$(cd "$smoke_dir" && "$binary" docs)
printf '%s\n' "$docs_index" | grep -q '`goals`'
docs_json=$(cd "$smoke_dir" && "$binary" docs search review --format json)
printf '%s\n' "$docs_json" | grep -q '"schema_version": "spynel.docs/v1"'

"$binary" init --no-start --dir "$smoke_dir"
(cd "$smoke_dir" && "$binary" config)
instructions_output=$("$binary" instructions --config "$smoke_dir/.spynel/config.yaml")
printf '%s\n' "$instructions_output" | grep -q 'chat: .spynel/instructions/agent-chat.md — valid'
printf '%s\n' "$instructions_output" | grep -q 'heartbeat: .spynel/instructions/agent-heartbeat.md — valid'
reviewed_task=$(cd "$smoke_dir" && "$binary" task "smoke test task creation")
direct_task=$(cd "$smoke_dir" && "$binary" task --no-review "collect smoke status")
"$binary" task inspect "$reviewed_task" | grep -q 'Review required: true'
"$binary" task inspect "$direct_task" | grep -q 'Review required: false'
(cd "$smoke_dir" && "$binary" goal "smoke test goal creation")

tasks_output=$(cd "$smoke_dir" && "$binary" tasks --detail --limit 2)
printf '%s\n' "$tasks_output" | grep -q '# Tasks · open'
printf '%s\n' "$tasks_output" | grep -q 'review required'
printf '%s\n' "$tasks_output" | grep -q 'direct low-risk completion allowed'
tasks_recent=$(cd "$smoke_dir" && "$binary" tasks recent --limit 1)
printf '%s\n' "$tasks_recent" | grep -q '# Tasks · recent 3d'
goals_output=$(cd "$smoke_dir" && "$binary" goals --limit 1)
printf '%s\n' "$goals_output" | grep -q '# Goals · open'
printf '%s\n' "$goals_output" | grep -q 'smoke test goal creation'
goals_recent=$(cd "$smoke_dir" && "$binary" goals recent --limit 1)
printf '%s\n' "$goals_recent" | grep -q '# Goals · recent 7d'
for workflow_view in open active review waiting done failed all; do
  tasks_view=$(cd "$smoke_dir" && "$binary" tasks "$workflow_view" --limit 1)
  printf '%s\n' "$tasks_view" | grep -q "# Tasks · $workflow_view"
  goals_view=$(cd "$smoke_dir" && "$binary" goals "$workflow_view" --limit 1)
  printf '%s\n' "$goals_view" | grep -q "# Goals · $workflow_view"
done
tasks_json=$(cd "$smoke_dir" && "$binary" tasks --json failed --limit 1)
printf '%s\n' "$tasks_json" | grep -q '"kind":"final"'
printf '%s\n' "$tasks_json" | grep -q '# Tasks · failed'
goals_json=$(cd "$smoke_dir" && "$binary" goals --json review --detail --limit 1)
printf '%s\n' "$goals_json" | grep -q '"kind":"final"'
printf '%s\n' "$goals_json" | grep -q '# Goals · review'

status_json=$("$binary" status --config "$smoke_dir/.spynel/config.yaml" --conversation smoke --json)
printf '%s\n' "$status_json" | grep -q '"harness_state"'
printf '%s\n' "$status_json" | grep -q '"connections"'

command_output=$("$binary" command --config "$smoke_dir/.spynel/config.yaml" --conversation smoke help commands)
printf '%s\n' "$command_output" | grep -q '/status'
printf '%s\n' "$command_output" | grep -q '/tasks'
printf '%s\n' "$command_output" | grep -q '/goals'
conversation_json=$("$binary" conversations list --config "$smoke_dir/.spynel/config.yaml" --json)
printf '%s\n' "$conversation_json" | grep -q '"conversation":"smoke"'
"$binary" conversations show --config "$smoke_dir/.spynel/config.yaml" --tail 5 cli smoke >/dev/null
branch=$("$binary" conversations resume --config "$smoke_dir/.spynel/config.yaml" cli smoke)
case "$branch" in
  resume-????????) ;;
  *) echo "unexpected resumed CLI conversation: $branch" >&2; exit 1 ;;
esac
"$binary" conversations show --config "$smoke_dir/.spynel/config.yaml" --tail 5 cli "$branch" >/dev/null

if "$binary" followup --config "$smoke_dir/.spynel/config.yaml" --conversation smoke "too late" >/dev/null 2>&1; then
  echo "inactive plain CLI follow-up unexpectedly succeeded" >&2
  exit 1
fi

test -f "$smoke_dir/.spynel/config.yaml"
test ! -e "$smoke_dir/spynel.yaml"
if grep -q 'state_dir:' "$smoke_dir/.spynel/config.yaml"; then
  echo "canonical config unexpectedly contains workspace.state_dir" >&2
  exit 1
fi
test -f "$smoke_dir/.spynel/AGENTS.md"
test -f "$smoke_dir/.spynel/prompts/create-task.md"
test -f "$smoke_dir/.spynel/prompts/create-goal.md"
test -f "$smoke_dir/.spynel/prompts/goal-review.md"
test "$(find "$smoke_dir/.spynel/instructions" -type f -name '*.md' | wc -l)" -eq 5
test "$(find "$smoke_dir/.spynel/tasks/todo" -type f -name '*.md' | wc -l)" -eq 2
test "$(find "$smoke_dir/.spynel/goals/proposed" -type f -name '*.md' | wc -l)" -eq 1

for status in todo working review reviewing waiting done failed cancelled; do
  test -d "$smoke_dir/.spynel/tasks/$status"
done
for status in proposed planning active review reviewing waiting done abandoned; do
  test -d "$smoke_dir/.spynel/goals/$status"
done

echo "Spynel smoke test passed: $smoke_dir"
