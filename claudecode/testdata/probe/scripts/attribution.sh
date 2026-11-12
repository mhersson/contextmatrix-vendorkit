#!/bin/bash
# attribution.sh (inside jail.sh): which --settings content keeps Claude Code's commit and PR
# attribution out of the request? One text turn per case with the Bash tool on; mock.py
# records the attribution texts per request part. resume-b resumes resume-a's transcript
# (recorded without the setting) with it. Writes $SCRATCH/mock/attribution.
source "$(dirname "$0")/lib.sh"
P=$SCRATCH/mock/attribution
fresh "$P"
mkdir -p "$P/home" "$P/work"
cd "$P/work"
printf '{}' >"$P/none.json"
printf '{"attribution":{"commit":"","pr":""}}' >"$P/attribution.json"
printf '{"includeCoAuthoredBy":false}' >"$P/coauthored.json"
printf 'MARKER-REPLACE-7f3a replaced prompt\n' >"$P/prompt.txt"
UUID=1c2d3e4f-5a6b-4c7d-8e9f-0a1b2c3d4e5f
run() { # NAME SETTINGS [ENV=VALUE...] -- [claude args...]
  local name=$1 settings=$2
  shift 2
  local envs=()
  while [ "$1" != "--" ]; do envs+=("$1"); shift; done
  shift
  mkdir -p "$P/cfg-$name"
  start_mock "$P/mock-$name" text
  run_claude "$P/run-$name" "$P/home" CLAUDE_CONFIG_DIR="$P/cfg-$name" "$BASE_URL" "$DUMMY_TOKEN" ${envs[@]+"${envs[@]}"} -- \
    --gate --send "0:$(MSG "commit the change")" --close 30 --close-on-result 1 -- \
    "${BASE_ARGS[@]}" --system-prompt-snapshot off --tools Bash --model claude-haiku-5-5 --settings "$settings" "$@"
  stop_mock
}
run none "$P/none.json" --
run attribution "$P/attribution.json" --
run coauthored "$P/coauthored.json" --
run replace-none "$P/none.json" -- --system-prompt-file "$P/prompt.txt"
run replace-attribution "$P/attribution.json" -- --system-prompt-file "$P/prompt.txt"
run announce-env "$P/attribution.json" CLAUDE_CODE_ATTRIBUTION_ANNOUNCEMENT=1 --
run resume-a "$P/none.json" -- --session-id "$UUID"
T=$(cd "$P/cfg-resume-a" && find projects -name "$UUID.jsonl")
mkdir -p "$P/cfg-resume-b/$(dirname "$T")"
cp "$P/cfg-resume-a/$T" "$P/cfg-resume-b/$T"
run resume-b "$P/attribution.json" -- --resume "$UUID"
