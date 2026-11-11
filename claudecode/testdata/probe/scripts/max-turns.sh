#!/bin/bash
# max-turns.sh (inside jail.sh): the error_max_turns result shape (--max-turns 1, one Bash
# turn). Writes $SCRATCH/mock/maxturns.
source "$(dirname "$0")/lib.sh"
O=$SCRATCH/mock/maxturns
fresh "$O"
mkdir -p "$O/home" "$O/cfg" "$O/work"
cd "$O/work"
start_mock "$O" bash '{"command":"echo one"}'
run_claude "$O/run" "$O/home" CLAUDE_CONFIG_DIR="$O/cfg" "$BASE_URL" "$DUMMY_KEY" -- \
  --gate --send "0:$(MSG 'run the command')" --close 30 --close-on-result 1 -- \
  "${BASE_ARGS[@]}" --tools Bash --allowedTools Bash --model claude-haiku-5-5 --max-turns 1
stop_mock
