#!/bin/bash
# background.sh (inside jail.sh): a Bash run_in_background call, default and with
# CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1. Writes $SCRATCH/mock/background/{default,disabled}.
source "$(dirname "$0")/lib.sh"
for c in default disabled; do
  O=$SCRATCH/mock/background/$c
  fresh "$O"
  mkdir -p "$O/home" "$O/cfg" "$O/work"
  extra=()
  [ "$c" = disabled ] && extra=(CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1)
  (
    cd "$O/work"
    start_mock "$O" bash '{"command":"sleep 1; echo bg","run_in_background":true}'
    run_claude "$O/run" "$O/home" CLAUDE_CONFIG_DIR="$O/cfg" "$BASE_URL" "$DUMMY_KEY" "${extra[@]}" -- \
      --gate --send "0:$(MSG 'run the command')" --close 15 --close-on-result 1 -- \
      "${BASE_ARGS[@]}" --tools Bash --allowedTools Bash --model claude-haiku-5-5
    stop_mock
  )
done
