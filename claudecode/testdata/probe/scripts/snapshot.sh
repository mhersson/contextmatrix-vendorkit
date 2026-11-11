#!/bin/bash
# snapshot.sh (inside jail.sh): which system prompt does a --resume use? A records one,
# B resumes with another, C resumes with another plus --system-prompt-snapshot off.
# Writes $SCRATCH/mock/snapshot.
source "$(dirname "$0")/lib.sh"
P=$SCRATCH/mock/snapshot
fresh "$P"
mkdir -p "$P/home" "$P/work" "$P/cfgA" "$P/cfgB" "$P/cfgC"
cd "$P/work"
UUID=0b7d3c2e-9a41-4f6b-8c5d-3e2f1a0b9c87
printf 'MARKER-APPEND-7f3a first launch\n' >"$P/first.txt"
printf 'MARKER-REPLACE-7f3a second launch\n' >"$P/second.txt"
run() { # NAME CONFIG_DIR MESSAGE [claude args...]
  local name=$1 cfg=$2 msg=$3
  shift 3
  start_mock "$P/mock-$name" text
  run_claude "$P/run-$name" "$P/home" CLAUDE_CONFIG_DIR="$cfg" "$BASE_URL" "$DUMMY_TOKEN" -- \
    --gate --send "0:$(MSG "$msg")" --close 30 --close-on-result 1 -- \
    "${BASE_ARGS[@]}" --tools "" --model claude-haiku-5-5 "$@"
  stop_mock
}
run a "$P/cfgA" first --session-id "$UUID" --append-system-prompt-file "$P/first.txt"
T=$(cd "$P/cfgA" && find projects -name "$UUID.jsonl")
for c in cfgB cfgC; do
  mkdir -p "$P/$c/$(dirname "$T")"
  cp "$P/cfgA/$T" "$P/$c/$T"
done
run b "$P/cfgB" second --resume "$UUID" --append-system-prompt-file "$P/second.txt"
run c "$P/cfgC" second --resume "$UUID" --append-system-prompt-file "$P/second.txt" --system-prompt-snapshot off
