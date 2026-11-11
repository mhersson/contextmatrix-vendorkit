#!/bin/bash
# p6.sh (inside jail.sh): probe P6, CLAUDE_CONFIG_DIR isolation and --resume from a fresh
# config dir holding only the copied transcript. Dummy OAuth token, no API key.
# Writes $SCRATCH/p6; summary.py checks the pass conditions.
source "$(dirname "$0")/lib.sh"
P=$SCRATCH/p6
fresh "$P"
mkdir -p "$P/home" "$P/store" "$P/work" "$P/cfgA" "$P/cfgB" "$P/cfgC" "$P/state"
cd "$P/work"
UUID=6f1c2a7e-4b3d-4e8a-9c1f-2d5e7a9b0c11
hook() { printf '{"hooks":{"PostToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"touch %s/fired-%s"}]}]}}\n' "$P" "$1"; }
fired() { (cd "$P" && ls -1 | grep '^fired-' || true) >"$P/state/fired-after-$1.txt"; }
ARGS=("${BASE_ARGS[@]}" --tools Bash --allowedTools Bash --model claude-haiku-5-5)

# Run A: the mock holds its first answer while hooks are planted in A's config dir,
# HOME/.claude and the store dir.
start_mock "$P/mockA" bash '{"command":"echo run-a"}' "$P/release-a"
run_claude "$P/runA" "$P/home" CLAUDE_CONFIG_DIR="$P/cfgA" "$BASE_URL" "$DUMMY_TOKEN" -- \
  --gate --send "0:$(MSG 'Run A: run the command')" --close 60 --close-on-result 1 -- \
  "${ARGS[@]}" --session-id "$UUID" &
RUNA=$!
python3 -I -c '
import json, sys, time
for _ in range(600):
    try:
        for l in open(sys.argv[1]):
            if json.loads(l).get("params", {}).get("stream"):
                sys.exit(0)
    except FileNotFoundError:
        pass
    time.sleep(0.05)
sys.exit(1)' "$P/mockA/requests.jsonl" || echo "run A never reached the mock" >&2
mkdir -p "$P/home/.claude"
hook cfgA >"$P/cfgA/settings.json"
hook home >"$P/home/.claude/settings.json"
hook store >"$P/store/settings.json"
touch "$P/release-a"
wait "$RUNA"
stop_mock
fired a
(cd "$P" && find cfgA -type f | sort) >"$P/state/cfgA-files.txt"

# Only the transcript crosses into a fresh config dir.
T=$(cd "$P/cfgA" && find projects -name "$UUID.jsonl")
echo "$T" >"$P/state/transcript-path.txt"
mkdir -p "$P/cfgB/$(dirname "$T")"
cp "$P/cfgA/$T" "$P/cfgB/$T"

# Run B: resume from the transcript alone.
start_mock "$P/mockB" bash '{"command":"echo run-b"}'
run_claude "$P/runB" "$P/home" CLAUDE_CONFIG_DIR="$P/cfgB" "$BASE_URL" "$DUMMY_TOKEN" -- \
  --gate --send "0:$(MSG 'Run B: run the command again')" --close 60 --close-on-result 1 -- \
  "${ARGS[@]}" --resume "$UUID"
stop_mock
fired b

# Control C: as B, with the hook in the config dir before start.
mkdir -p "$P/cfgC/$(dirname "$T")"
cp "$P/cfgA/$T" "$P/cfgC/$T"
hook cfgC >"$P/cfgC/settings.json"
start_mock "$P/mockC" bash '{"command":"echo run-c"}'
run_claude "$P/runC" "$P/home" CLAUDE_CONFIG_DIR="$P/cfgC" "$BASE_URL" "$DUMMY_TOKEN" -- \
  --gate --send "0:$(MSG 'Run C: run the command again')" --close 60 --close-on-result 1 -- \
  "${ARGS[@]}" --resume "$UUID"
stop_mock
fired c
(cd "$P" && find home -mindepth 1 | sort) >"$P/state/home-files.txt"
