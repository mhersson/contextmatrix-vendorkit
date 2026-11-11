#!/bin/bash
# idle-timeout.sh (inside jail.sh): does claude abort a silent http MCP tool call, and what
# stops it? A bridged-style server (sleepmcp.py, server cm) runs one sleep tool that sends
# nothing until it answers; mock.py's idle scenario calls it once. Cases vary
# CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT and the cm entry's per-server "timeout" in --mcp-config.
# IDLE_CASES picks the set: short (default; a 3 s idle timeout, about 2 min), sixty (90 s
# tools, about 5 min) or default (400 s tools past the 300 s default idle, about 14 min).
# Writes $SCRATCH/mock/idle-<set>.
source "$(dirname "$0")/lib.sh"
SET=${IDLE_CASES:-short}
MCP_PORT=${MCP_PORT:-28112}
I=$SCRATCH/mock/idle-$SET
fresh "$I"
mkdir -p "$I/work"
cd "$I/work"
start_mock "$I" idle

# one NAME SECONDS TIMEOUT ENV...: TIMEOUT "-" leaves the per-server timeout out.
one() {
  local name=$1 secs=$2 timeout=$3
  shift 3
  local O=$I/$name
  mkdir -p "$O/home" "$O/cfg"
  python3 -I -c '
import json, sys
cm = {"type": "http", "url": "http://127.0.0.1:%s/mcp" % sys.argv[2]}
if sys.argv[3] != "-":
    cm["timeout"] = int(sys.argv[3])
json.dump({"mcpServers": {"cm": cm}}, open(sys.argv[1], "w"))' "$O/mcp.json" "$MCP_PORT" "$timeout"
  python3 -I "$HERE/sleepmcp.py" "$MCP_PORT" "$secs" "$O/mcp-calls.jsonl" 2>"$O/mcp.err" &
  local mcppid=$!
  sleep 0.5
  run_claude "$O" "$O/home" CLAUDE_CONFIG_DIR="$O/cfg" "$BASE_URL" "$DUMMY_TOKEN" "$@" -- \
    --gate --send "0:$(MSG 'Run the sleep tool.')" --close $((secs * 2 + 60)) --close-on-result 1 -- \
    "${BASE_ARGS[@]}" --tools "" --mcp-config "$O/mcp.json" --allowedTools mcp__cm__sleep --model claude-haiku-5-5
  kill "$mcppid" 2>/dev/null || true
  wait "$mcppid" 2>/dev/null || true
}

case $SET in
short)
  one idle3s-sleep7 7 - CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT=3000
  one idle3s-sleep40 40 - CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT=3000
  one idle0-sleep40 40 - CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT=0
  one idle3s-timeout-sleep40 40 100000000 CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT=3000
  ;;
sixty)
  one default-sleep90 90 -
  one idle0-sleep90 90 - CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT=0
  one timeout-sleep90 90 100000000
  ;;
default)
  one default-sleep400 400 -
  one default-timeout-sleep400 400 100000000
  ;;
esac
stop_mock
