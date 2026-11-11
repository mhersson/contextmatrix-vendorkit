#!/bin/bash
# flags.sh (inside jail.sh): --effort values, the system-prompt file flags and
# --no-session-persistence. Writes $SCRATCH/mock/flags.
source "$(dirname "$0")/lib.sh"
F=$SCRATCH/mock/flags
fresh "$F"
mkdir -p "$F/work"
cd "$F/work"
printf 'MARKER-REPLACE-7f3a You are a probe. Answer briefly.\n' >"$F/replace.txt"
printf 'MARKER-APPEND-7f3a Extra instructions for the probe.\n' >"$F/append.txt"
one() { # NAME MODEL [claude args...]
  local name=$1 model=$2
  shift 2
  local O=$F/$name
  mkdir -p "$O/home" "$O/cfg"
  start_mock "$O" text
  run_claude "$O/run" "$O/home" CLAUDE_CONFIG_DIR="$O/cfg" "$BASE_URL" "$DUMMY_KEY" -- \
    --gate --send "0:$(MSG 'say ok')" --close 30 --close-on-result 1 -- \
    "${BASE_ARGS[@]}" --tools "" --model "$model" "$@"
  stop_mock
}
for e in low medium high xhigh max bogus minimal; do one "effort-$e" claude-sonnet-5-5 --effort "$e"; done
one effort-unset claude-sonnet-5-5
one sys-replace claude-haiku-5-5 --system-prompt-file "$F/replace.txt"
one sys-append claude-haiku-5-5 --append-system-prompt-file "$F/append.txt"
one sys-default claude-haiku-5-5
one nopersist claude-haiku-5-5 --no-session-persistence
one persist claude-haiku-5-5
