#!/bin/bash
# max-budget.sh (inside jail.sh): the error_max_budget_usd result shape (--max-budget-usd 0.000001):
# one call that ends its turn (end-turn) and one that asks for a Bash tool (tool-use).
# Writes $SCRATCH/mock/maxbudget/{end-turn,tool-use}.
source "$(dirname "$0")/lib.sh"
for c in end-turn:text tool-use:bash; do
  name=${c%%:*}
  sc=${c##*:}
  O=$SCRATCH/mock/maxbudget/$name
  fresh "$O"
  mkdir -p "$O/home" "$O/cfg" "$O/work"
  cd "$O/work"
  start_mock "$O" "$sc" '{"command":"echo one"}'
  run_claude "$O/run" "$O/home" CLAUDE_CONFIG_DIR="$O/cfg" "$BASE_URL" "$DUMMY_KEY" -- \
    --gate --send "0:$(MSG 'say ok')" --close 30 --close-on-result 1 -- \
    "${BASE_ARGS[@]}" --tools Bash --allowedTools Bash --model claude-haiku-5-5 --max-budget-usd 0.000001
  stop_mock
done
