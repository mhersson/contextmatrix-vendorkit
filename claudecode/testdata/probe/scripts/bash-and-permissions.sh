#!/bin/bash
# bash-and-permissions.sh (inside jail.sh): dontAsk with and without an allow rule, and the
# Bash timeout variables against a sleep 5 tool call. Writes $SCRATCH/mock/bash.
source "$(dirname "$0")/lib.sh"
B=$SCRATCH/mock/bash
fresh "$B"
mkdir -p "$B/work"
cd "$B/work"
one() { # NAME TOOL_INPUT ALLOWED_TOOLS [ENV=VALUE...]
  local name=$1 input=$2 allowed=$3
  shift 3
  local O=$B/$name extra=()
  mkdir -p "$O/home" "$O/cfg"
  [ -n "$allowed" ] && extra=(--allowedTools "$allowed")
  start_mock "$O" bash "$input"
  run_claude "$O/run" "$O/home" CLAUDE_CONFIG_DIR="$O/cfg" "$BASE_URL" "$DUMMY_KEY" "$@" -- \
    --gate --send "0:$(MSG 'run the command')" --close 60 --close-on-result 1 -- \
    "${BASE_ARGS[@]}" --tools Bash "${extra[@]}" --model claude-haiku-5-5
  stop_mock
}
one perm-allowed '{"command":"echo allowed-ran"}' Bash
one perm-readonly '{"command":"echo readonly-ran"}' ""
one perm-denied "{\"command\":\"touch $B/perm-denied/should-not-exist\"}" ""
one t-control '{"command":"sleep 5","timeout":10000}' Bash
one t-max-only '{"command":"sleep 5","timeout":10000}' Bash BASH_MAX_TIMEOUT_MS=2000
one t-default-and-max '{"command":"sleep 5","timeout":10000}' Bash BASH_DEFAULT_TIMEOUT_MS=1000 BASH_MAX_TIMEOUT_MS=2000
one t-default-only '{"command":"sleep 5"}' Bash BASH_DEFAULT_TIMEOUT_MS=2000
