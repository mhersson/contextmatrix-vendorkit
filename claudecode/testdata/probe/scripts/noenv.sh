#!/bin/bash
# noenv.sh: help, version, startup and handshake captures with no credentials at all
# (env -i, HOME under SCRATCH). Writes $SCRATCH/probe.
source "$(dirname "$0")/lib.sh"
O=$SCRATCH/probe
fresh "$O"
mkdir -p "$SCRATCH/home" "$SCRATCH/work"
cd "$SCRATCH/work"
noenv() { env -i PATH="$CLAUDE_PATH" HOME="$SCRATCH/home" "$@"; }
FLAGS=(-p --input-format stream-json --output-format stream-json --verbose --include-partial-messages)

noenv "$CLAUDE" --version >"$O/version.txt"
noenv "$CLAUDE" --help >"$O/help.txt"
# Stdin open, nothing written for 10 s: does init (or anything) arrive before input?
noenv python3 -I "$HERE/driver.py" --out "$O/startup.jsonl" --timing "$O/startup.timing" --err "$O/startup.err" \
  --close 10 -- "$CLAUDE" "${FLAGS[@]}"
# The Agent SDK's initialize handshake, written at start.
noenv python3 -I "$HERE/driver.py" --out "$O/handshake.jsonl" --timing "$O/handshake.timing" --err "$O/handshake.err" \
  --send '0:{"type":"control_request","request_id":"r1","request":{"subtype":"initialize"}}' \
  --close 10 -- "$CLAUDE" "${FLAGS[@]}"
