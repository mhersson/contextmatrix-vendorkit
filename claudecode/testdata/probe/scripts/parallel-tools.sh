#!/bin/bash
# parallel-tools.sh (inside jail.sh): one assistant message with five Bash tool_use blocks,
# streamed slowly enough that tools run before the message ends. Writes $SCRATCH/mock/parallel.
source "$(dirname "$0")/lib.sh"
O=$SCRATCH/mock/parallel
fresh "$O"
mkdir -p "$O/home" "$O/cfg" "$O/work"
cd "$O/work"
start_mock "$O" parallel
run_claude "$O/run" "$O/home" CLAUDE_CONFIG_DIR="$O/cfg" "$BASE_URL" "$DUMMY_KEY" -- \
  --gate --send "0:$(MSG 'run the commands')" --close 60 --close-on-result 1 -- \
  "${BASE_ARGS[@]}" --tools Bash --allowedTools Bash --model claude-haiku-5-5
stop_mock
