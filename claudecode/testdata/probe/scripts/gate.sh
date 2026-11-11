#!/bin/bash
# gate.sh (inside jail.sh): messages written at process start with and without the
# initialize gate; does any message get dropped? Writes $SCRATCH/mock/gate.
source "$(dirname "$0")/lib.sh"
G=$SCRATCH/mock/gate
fresh "$G"
mkdir -p "$G/work"
cd "$G/work"
start_mock "$G" text
one() { # NAME RESULTS [driver args...]
  local name=$1 nres=$2
  shift 2
  local O=$G/$name
  mkdir -p "$O/home" "$O/cfg"
  run_claude "$O" "$O/home" CLAUDE_CONFIG_DIR="$O/cfg" "$BASE_URL" "$DUMMY_KEY" -- \
    "$@" --close 20 --close-on-result "$nres" -- \
    "${BASE_ARGS[@]}" --tools "" --model claude-haiku-5-5
}
for i in $(seq -w 1 30); do one "ungated-$i" 1 --send "0:$(MSG "ungated $i")"; done
for i in $(seq -w 1 10); do one "gated-$i" 1 --gate --send "0:$(MSG "gated $i")"; done
for i in $(seq -w 1 5); do one "two-at-start-$i" 2 --send "0:$(MSG "first $i")" --send "0:$(MSG "second $i")"; done
for i in $(seq -w 1 3); do one "two-gated-$i" 2 --gate --send "0:$(MSG "first gated $i")" --send "0:$(MSG "second gated $i")"; done
stop_mock
